package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// schemaSourceWatermark is each pulled source's settled-through watermark
// (#913-D9 ruling C′): a month is sealed only once, for every registered,
// unretired source, one of its gap-free runs covers the whole month. serve
// writes one row per source it starts and retires the rest (RegisterSources);
// only a successful pass advances a row (AdvanceSourceWatermark).
//
// covered_from..settled_through is the source's current gap-free run: both are
// NULL until its first successful pass. gap_from is where the run before it
// ended, NULL when none did. Times are sealTime strings, which compare as text.
// Tenant retrofit: tenant_id LEADS -> (tenant_id, source).
const schemaSourceWatermark = `
CREATE TABLE IF NOT EXISTS source_watermark (
    source          TEXT PRIMARY KEY CHECK (source <> ''),
    registered_at   TEXT NOT NULL,
    retired_at      TEXT,
    covered_from    TEXT,
    settled_through TEXT,
    gap_from        TEXT,
    CHECK ((covered_from IS NULL) = (settled_through IS NULL)),
    CHECK (covered_from IS NULL OR covered_from <= settled_through)
);
-- A source's earlier gap-free runs, each closed when a pass started after its
-- end (#913-D9 ruling R-8): a month any one run covers stays certified.
-- Tenant retrofit: tenant_id LEADS -> (tenant_id, source, covered_from).
CREATE TABLE IF NOT EXISTS source_run (
    source          TEXT NOT NULL,
    covered_from    TEXT NOT NULL,
    settled_through TEXT NOT NULL,
    CHECK (covered_from <= settled_through),
    PRIMARY KEY (source, covered_from)
);
-- Spend a source moved past without recording it and can never re-read
-- (#913-D9 ruling R-8): no month a span touches is certified by that source,
-- whatever its runs cover, until "tierd seal --skip" records the month as a gap.
-- Tenant retrofit: tenant_id LEADS -> (tenant_id, source, lost_from).
CREATE TABLE IF NOT EXISTS source_loss (
    source       TEXT NOT NULL,
    lost_from    TEXT NOT NULL,
    lost_through TEXT NOT NULL,
    CHECK (lost_from <= lost_through),
    PRIMARY KEY (source, lost_from)
);
-- Its one row records that serve registered its sources at least once, even
-- none: until then no row is ever read as "not configured".
CREATE TABLE IF NOT EXISTS source_registration (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    registered_at TEXT NOT NULL
);
`

// SourceWatermark is one source_watermark row. CoveredFrom..SettledThrough is
// the source's current gap-free run, set once Settled; GapFrom is where the run
// before it ended, zero when none did; Earlier are its closed runs, oldest
// first; Lost are the spans it recorded as lost (RecordSourceLoss), oldest first.
type SourceWatermark struct {
	Source                               string
	Retired, Settled                     bool
	CoveredFrom, SettledThrough, GapFrom time.Time
	Earlier                              []SourceRun
	Lost                                 []SourceLoss
}

// SourceRun is one closed gap-free run of a source's successful passes.
type SourceRun struct{ CoveredFrom, SettledThrough time.Time }

// SourceLoss is one span of spend a source recorded as lost.
type SourceLoss struct{ From, Through time.Time }

// Covers reports whether one of w's runs covers all of start..end and no span
// it lost touches start..end (end exclusive).
func (w SourceWatermark) Covers(start, end time.Time) bool {
	if _, lost := w.LostIn(start, end); lost {
		return false
	}
	covers := func(r SourceRun) bool { return !r.CoveredFrom.After(start) && !r.SettledThrough.Before(end) }
	return (w.Settled && covers(SourceRun{w.CoveredFrom, w.SettledThrough})) || slices.ContainsFunc(w.Earlier, covers)
}

// LostIn returns the first span w lost that touches start..end (end exclusive).
func (w SourceWatermark) LostIn(start, end time.Time) (SourceLoss, bool) {
	i := slices.IndexFunc(w.Lost, func(g SourceLoss) bool { return g.From.Before(end) && !g.Through.Before(start) })
	if i < 0 {
		return SourceLoss{}, false
	}
	return w.Lost[i], true
}

// ErrSourceNotRegistered is AdvanceSourceWatermark's refusal of a source with no
// row: serve registers every source it starts before starting it.
var ErrSourceNotRegistered = errors.New("source is not registered in source_watermark")

// ErrSourcesNotRegistered is SourceWatermarks' answer on a database no serve
// has registered its sources on, where a missing row cannot mean "not
// configured".
var ErrSourcesNotRegistered = errors.New("tierd serve has not registered its sources on this database yet")

// RegisterSources records, in one transaction, that exactly sources are
// configured, and that a registration happened: each gets a row, or its
// retired row is unretired with its runs kept, and every other unretired row is
// retired. retired names the rows this call retired.
func (d *DB) RegisterSources(ctx context.Context, sources []string) (retired []string, err error) {
	now := sealTime(d.clock())
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_registration (id, registered_at) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET registered_at = excluded.registered_at`, now); err != nil {
		return nil, fmt.Errorf("record the source registration: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `UPDATE source_watermark SET retired_at = ? WHERE retired_at IS NULL RETURNING source`, now)
	if err != nil {
		return nil, fmt.Errorf("retire sources: %w", err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("retire sources: %w", err)
		}
		if !slices.Contains(sources, s) {
			retired = append(retired, s)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("retire sources: %w", err)
	}
	for _, s := range sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_watermark (source, registered_at) VALUES (?, ?)
			ON CONFLICT (source) DO UPDATE SET retired_at = NULL`, s, now); err != nil {
			return nil, fmt.Errorf("register source %q: %w", s, err)
		}
	}
	slices.Sort(retired)
	return retired, tx.Commit()
}

// AdvanceSourceWatermark records a successful pass of source that proved
// from..through complete. The run is extended only over gap-free coverage: when
// from is at or before the stored settled_through, settled_through becomes the
// later of the two, and covered_from the earlier when the pass also reaches the
// run's start. Otherwise (and on the first pass) a new run starts at from, the
// old one is kept in source_run, and gap_from keeps where it ended, so no month
// the gap touches is ever certified.
func (d *DB) AdvanceSourceWatermark(ctx context.Context, source string, from, through time.Time) error {
	if through.Before(from) {
		return fmt.Errorf("source %q: settled through %s is before its window start %s", source, sealTime(through), sealTime(from))
	}
	f, t := sealTime(from), sealTime(through)
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_run (source, covered_from, settled_through)
		SELECT source, covered_from, settled_through FROM source_watermark
		WHERE source = ? AND settled_through IS NOT NULL AND ? > settled_through
		ON CONFLICT (source, covered_from) DO UPDATE SET settled_through = MAX(settled_through, excluded.settled_through)`,
		source, f); err != nil {
		return fmt.Errorf("close source %q's run: %w", source, err)
	}
	// Every SET expression reads the row as it was before this UPDATE.
	res, err := tx.ExecContext(ctx, `UPDATE source_watermark SET
		gap_from = CASE WHEN settled_through IS NULL THEN gap_from
			WHEN ? > settled_through THEN settled_through
			WHEN ? >= covered_from AND ? <= gap_from THEN NULL ELSE gap_from END,
		covered_from = CASE WHEN settled_through IS NULL OR ? > settled_through THEN ?
			WHEN ? >= covered_from THEN MIN(covered_from, ?) ELSE covered_from END,
		settled_through = CASE WHEN settled_through IS NULL OR ? > settled_through THEN ? ELSE MAX(settled_through, ?) END
		WHERE source = ?`, f, t, f, f, f, t, f, f, t, t, source)
	if err != nil {
		return fmt.Errorf("advance source %q: %w", source, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: %q", ErrSourceNotRegistered, source)
	}
	return tx.Commit()
}

// RecordSourceLoss records that source moved past in-scope spend from from to
// through without recording it, and can never re-read it (#913-D9 ruling R-8).
// A span recorded again from the same start widens to the later end.
func (d *DB) RecordSourceLoss(ctx context.Context, source string, from, through time.Time) error {
	if through.Before(from) {
		return fmt.Errorf("source %q: lost span ends %s, before its start %s", source, sealTime(through), sealTime(from))
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO source_loss (source, lost_from, lost_through)
		SELECT source, ?, ? FROM source_watermark WHERE source = ?
		ON CONFLICT (source, lost_from) DO UPDATE SET lost_through = MAX(lost_through, excluded.lost_through)`,
		sealTime(from), sealTime(through), source)
	if err != nil {
		return fmt.Errorf("record source %q's lost span: %w", source, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: %q", ErrSourceNotRegistered, source)
	}
	return nil
}

// SourceWatermarks returns every source_watermark row, ordered by source.
func (d *DB) SourceWatermarks(ctx context.Context) ([]SourceWatermark, error) {
	return sourceWatermarks(ctx, d.db)
}

// SourceWatermarks is DB.SourceWatermarks inside the snapshot.
func (r reader) SourceWatermarks(ctx context.Context) ([]SourceWatermark, error) {
	return sourceWatermarks(ctx, r.q)
}

func sourceWatermarks(ctx context.Context, q readQuerier) ([]SourceWatermark, error) {
	var registered bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM source_registration)`).Scan(&registered); err != nil {
		return nil, fmt.Errorf("read the source registration: %w", err)
	} else if !registered {
		return nil, ErrSourcesNotRegistered
	}
	rows, err := q.QueryContext(ctx, `SELECT source, retired_at IS NOT NULL, covered_from, settled_through, gap_from
		FROM source_watermark ORDER BY source`)
	if err != nil {
		return nil, fmt.Errorf("read source watermarks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceWatermark
	for rows.Next() {
		var w SourceWatermark
		var from, through, gap sql.NullString
		if err := rows.Scan(&w.Source, &w.Retired, &from, &through, &gap); err != nil {
			return nil, fmt.Errorf("read source watermark: %w", err)
		}
		w.Settled = through.Valid
		for _, p := range []struct {
			dst *time.Time
			src sql.NullString
		}{{&w.CoveredFrom, from}, {&w.SettledThrough, through}, {&w.GapFrom, gap}} {
			if !p.src.Valid {
				continue
			}
			if *p.dst, err = time.Parse(time.RFC3339, p.src.String); err != nil {
				return nil, fmt.Errorf("parse source_watermark %q time %q: %w", w.Source, p.src.String, err)
			}
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, sourceRuns(ctx, q, out)
}

// sourceRuns fills each row's Earlier from source_run and Lost from source_loss.
func sourceRuns(ctx context.Context, q readQuerier, out []SourceWatermark) error {
	at := make(map[string]int, len(out))
	for i, w := range out {
		at[w.Source] = i
	}
	for _, t := range []struct {
		query string
		add   func(w *SourceWatermark, from, through time.Time)
	}{
		{`SELECT source, covered_from, settled_through FROM source_run ORDER BY source, covered_from`,
			func(w *SourceWatermark, from, through time.Time) {
				w.Earlier = append(w.Earlier, SourceRun{from, through})
			}},
		{`SELECT source, lost_from, lost_through FROM source_loss ORDER BY source, lost_from`,
			func(w *SourceWatermark, from, through time.Time) { w.Lost = append(w.Lost, SourceLoss{from, through}) }},
	} {
		if err := readSpans(ctx, q, t.query, func(source string, from, through time.Time) {
			if i, ok := at[source]; ok {
				t.add(&out[i], from, through)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// readSpans calls add with each (source, from, through) row query returns.
func readSpans(ctx context.Context, q readQuerier, query string, add func(source string, from, through time.Time)) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("read source spans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source, from, through string
		if err := rows.Scan(&source, &from, &through); err != nil {
			return fmt.Errorf("read source span: %w", err)
		}
		f, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return fmt.Errorf("parse source %q span: %w", source, err)
		}
		t, err := time.Parse(time.RFC3339, through)
		if err != nil {
			return fmt.Errorf("parse source %q span: %w", source, err)
		}
		add(source, f, t)
	}
	return rows.Err()
}
