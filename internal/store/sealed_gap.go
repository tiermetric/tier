package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// schemaSealedGap is the permanent seal gap (#913-D6 ruling D): a period the
// operator recorded, with `tierd seal --skip`, as never to be sealed, so the
// periods after it seal. A gap holds no figures and no person key, but its
// reason is operator free text: served to readers, and never erasable.
//
// It is append-only like sealed_report, and also refuses DELETE: nothing prunes
// it yet (retention, #252, is not built). A gap and a sealed period never
// overlap: the BEFORE INSERT triggers refuse either one over the other, and
// SealReport and RecordSealedGap check both tables inside their write
// transaction. The no-replace trigger aborts, because INSERT OR REPLACE would
// delete the row without firing its DELETE trigger (recursive_triggers is off).
// Tenant retrofit: tenant_id LEADS -> (tenant_id, period_size, period_start).
const schemaSealedGap = `
CREATE TABLE IF NOT EXISTS sealed_gap (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    period_size  TEXT    NOT NULL,
    period_start TEXT    NOT NULL,
    period_end   TEXT    NOT NULL,
    category     TEXT    NOT NULL CHECK (category <> ''),
    reason       TEXT    NOT NULL CHECK (reason <> ''),
    created_at   TEXT    NOT NULL,
    tool_version TEXT    NOT NULL,
    tool_commit  TEXT    NOT NULL,
    CHECK (period_start < period_end)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sealed_gap_period
    ON sealed_gap(period_size, period_start);
CREATE TRIGGER IF NOT EXISTS trg_sealed_gap_no_update
BEFORE UPDATE ON sealed_gap
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_gap_no_delete
BEFORE DELETE ON sealed_gap
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap is append-only: DELETE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_gap_no_replace
BEFORE INSERT ON sealed_gap
WHEN EXISTS (SELECT 1 FROM sealed_gap WHERE rowid = NEW.rowid
             OR (period_size = NEW.period_size AND period_start = NEW.period_start))
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap is append-only: a row with that key exists (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_gap_not_over_seal
BEFORE INSERT ON sealed_gap
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE period_start < NEW.period_end AND period_end > NEW.period_start)
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap: that period is sealed (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_report_not_over_gap
BEFORE INSERT ON sealed_report
WHEN EXISTS (SELECT 1 FROM sealed_gap WHERE period_start < NEW.period_end AND period_end > NEW.period_start)
BEGIN
    SELECT RAISE(ABORT, 'sealed_report: that period is recorded as a gap and is never sealed (#913)');
END;
`

// SealedGap is a period recorded as never to be sealed (#913-D6).
type SealedGap struct {
	ID                     int64
	PeriodSize             string
	PeriodStart, PeriodEnd time.Time
	// Category is the seal failure the operator's re-attempt observed, one of
	// the SealGap* codes; no error text is stored.
	Category string
	// Reason is the operator's reason (CheckSealedGapReason).
	Reason                  string
	CreatedAt               time.Time
	ToolVersion, ToolCommit string
}

// SealGap* are the only categories a gap stores: the seal failure classes that
// are not temporary. A busy write lock, an erase race and a clock behind the
// newest seal are retried, never recorded as a gap (#913-D6 condition 2).
const (
	SealGapRefoldMismatch = "refold_mismatch"
	SealGapFloorMoved     = "floor_moved"
	SealGapNotNext        = "not_next"
	SealGapOverlap        = "overlap"
	SealGapNotSealable    = "not_sealable"
	SealGapInternal       = "internal"
	// SealGapSourceBehind is a month a configured source has not settled past
	// (#913-D9): a gap the operator records rather than wait for it.
	SealGapSourceBehind = "source_behind"
)

// ErrSealedGapCategory refuses a gap whose category is not a SealGap* code.
var ErrSealedGapCategory = errors.New("a gap's category must be a seal gap category code")

// validSealGapCategory reports whether c is a SealGap* code.
func validSealGapCategory(c string) bool {
	switch c {
	case SealGapRefoldMismatch, SealGapFloorMoved, SealGapNotNext, SealGapOverlap, SealGapNotSealable, SealGapInternal,
		SealGapSourceBehind:
		return true
	}
	return false
}

// MaxSealedGapReason is the longest reason a gap stores, in characters.
const MaxSealedGapReason = 200

// ErrSealedGapReason refuses a gap reason that is empty or blank (nothing but
// spaces, combining marks and blank fillers), longer than MaxSealedGapReason
// characters, not UTF-8, or holds a character that is not graphic: a control or
// format character (a line break, an escape, a bidirectional override), a line
// or paragraph separator, a private-use, surrogate or unassigned code point. A
// reason is served verbatim in a 404.
var ErrSealedGapReason = fmt.Errorf("a gap's reason must be 1 to %d characters of printable text", MaxSealedGapReason)

// ErrSealedPeriodGapped refuses a seal or a gap overlapping a period recorded as
// a gap.
var ErrSealedPeriodGapped = errors.New("period is recorded as a permanent gap and is never sealed")

// ErrSealedPeriodSealed refuses a gap overlapping a sealed period.
var ErrSealedPeriodSealed = errors.New("period is sealed")

// ErrSealedGapNotFound is SealedGap's answer when no gap is recorded for the period.
var ErrSealedGapNotFound = errors.New("no gap recorded for that period")

// CheckSealedGapReason returns ErrSealedGapReason unless reason is a reason a
// gap may store.
func CheckSealedGapReason(reason string) error {
	if !strings.ContainsFunc(reason, visibleRune) || !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > MaxSealedGapReason ||
		strings.ContainsFunc(reason, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return ErrSealedGapReason
	}
	return nil
}

// blankFillers are letters and symbols that render as blank space.
var blankFillers = []rune{'\u115F', '\u1160', '\u3164', '\uFFA0', '\u2800'}

// visibleRune reports whether r shows as something other than blank space.
func visibleRune(r rune) bool {
	return !unicode.IsSpace(r) && !unicode.Is(unicode.Mn, r) && !slices.Contains(blankFillers, r)
}

// gapOverlaps reports whether a recorded gap intersects [start, end).
func gapOverlaps(ctx context.Context, q rowReader, start, end time.Time) (bool, error) {
	var gapped bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sealed_gap
		WHERE period_start < ? AND period_end > ?)`, sealTime(end), sealTime(start)).Scan(&gapped); err != nil {
		return false, fmt.Errorf("read sealed gaps: %w", err)
	}
	return gapped, nil
}

// newestOccupiedEnd is the end of the newest period sealed or recorded as a
// gap; ok is false when there is neither.
func newestOccupiedEnd(ctx context.Context, q rowReader) (end string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT period_end FROM (
		SELECT period_start, period_end FROM sealed_report
		UNION ALL SELECT period_start, period_end FROM sealed_gap)
		ORDER BY period_start DESC LIMIT 1`).Scan(&end)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read newest sealed period: %w", err)
	}
	return end, true, nil
}

// RecordSealedGap records g as a permanent gap, in one write transaction,
// bounded like every request-path writer; g.ID and g.CreatedAt are assigned
// here. It is refused unless g is one UTC calendar month that has ended
// (ErrSealedPeriodInvalid), its category is a SealGap* code
// (ErrSealedGapCategory), its reason passes CheckSealedGapReason, it overlaps no
// sealed period (ErrSealedPeriodSealed) and no gap (ErrSealedPeriodGapped), it
// starts where the newest sealed or gapped period ends (ErrSealNotNext, also
// while nothing is sealed), and the clock is not behind the newest seal or gap
// (ErrSealClockBehind).
func (d *DB) RecordSealedGap(ctx context.Context, g SealedGap) (SealedGap, error) {
	now := d.clock()
	start := g.PeriodStart.UTC()
	if !validSealSpan(g.PeriodSize, g.PeriodStart, g.PeriodEnd) || g.PeriodSize != "month" ||
		!start.Equal(time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)) ||
		!g.PeriodEnd.Equal(start.AddDate(0, 1, 0)) || g.PeriodEnd.After(now) {
		return SealedGap{}, fmt.Errorf("%w: a gap is one UTC calendar month that has ended", ErrSealedPeriodInvalid)
	}
	if !validSealGapCategory(g.Category) {
		return SealedGap{}, ErrSealedGapCategory
	}
	if err := CheckSealedGapReason(g.Reason); err != nil {
		return SealedGap{}, err
	}
	tx, release, err := beginImmediateBounded(ctx, d.db, requestPathBusyTimeout)
	if err != nil {
		return SealedGap{}, err
	}
	defer release()
	if sealed, err := sealedOverlapping(ctx, tx, g.PeriodStart, g.PeriodEnd); err != nil {
		return SealedGap{}, err
	} else if len(sealed) > 0 {
		return SealedGap{}, fmt.Errorf("%w: %s %s", ErrSealedPeriodSealed, sealed[0].PeriodSize, sealTime(sealed[0].PeriodStart))
	}
	if gapped, err := gapOverlaps(ctx, tx, g.PeriodStart, g.PeriodEnd); err != nil {
		return SealedGap{}, err
	} else if gapped {
		return SealedGap{}, ErrSealedPeriodGapped
	}
	want, ok, err := newestOccupiedEnd(ctx, tx)
	if err != nil {
		return SealedGap{}, err
	}
	if got := sealTime(g.PeriodStart); !ok || got != want {
		return SealedGap{}, fmt.Errorf("%w: a gap must start where the newest sealed or gapped period ends, and %s %s does not",
			ErrSealNotNext, g.PeriodSize, got)
	}
	if err := checkSealClock(ctx, tx, sealTime(now)); err != nil {
		return SealedGap{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sealed_gap
		(period_size, period_start, period_end, category, reason, created_at, tool_version, tool_commit)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, g.PeriodSize, sealTime(g.PeriodStart), sealTime(g.PeriodEnd),
		g.Category, g.Reason, sealTime(now), g.ToolVersion, g.ToolCommit); err != nil {
		return SealedGap{}, fmt.Errorf("insert sealed_gap: %w", err)
	}
	stored, err := sealedGap(ctx, tx, g.PeriodSize, g.PeriodStart)
	if err != nil {
		return SealedGap{}, err
	}
	if err := tx.Commit(); err != nil {
		return SealedGap{}, fmt.Errorf("commit sealed gap: %w", err)
	}
	return stored, nil
}

// SealedGap returns the gap recorded at (periodSize, periodStart), or
// ErrSealedGapNotFound.
func (d *DB) SealedGap(ctx context.Context, periodSize string, periodStart time.Time) (SealedGap, error) {
	return sealedGap(ctx, d.db, periodSize, periodStart)
}

func sealedGap(ctx context.Context, q rowReader, periodSize string, periodStart time.Time) (SealedGap, error) {
	var g SealedGap
	var start, end, created string
	err := q.QueryRowContext(ctx, `SELECT id, period_size, period_start, period_end, category, reason,
		created_at, tool_version, tool_commit FROM sealed_gap WHERE period_size = ? AND period_start = ?`,
		periodSize, sealTime(periodStart)).Scan(&g.ID, &g.PeriodSize, &start, &end, &g.Category, &g.Reason,
		&created, &g.ToolVersion, &g.ToolCommit)
	if errors.Is(err, sql.ErrNoRows) {
		return SealedGap{}, ErrSealedGapNotFound
	}
	if err != nil {
		return SealedGap{}, fmt.Errorf("read sealed gap: %w", err)
	}
	for _, p := range []struct {
		dst *time.Time
		src string
	}{{&g.PeriodStart, start}, {&g.PeriodEnd, end}, {&g.CreatedAt, created}} {
		if *p.dst, err = time.Parse(time.RFC3339, p.src); err != nil {
			return SealedGap{}, fmt.Errorf("parse sealed_gap %d time %q: %w", g.ID, p.src, err)
		}
	}
	return g, nil
}

// LatestSealedGap returns the start of the newest gap of periodSize; ok is false
// when none is recorded.
func (d *DB) LatestSealedGap(ctx context.Context, periodSize string) (start time.Time, ok bool, err error) {
	return latestPeriodStart(ctx, d.db, "sealed_gap", periodSize)
}
