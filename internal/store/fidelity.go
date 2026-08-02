package store

import (
	"context"
	"sort"
	"time"
)

// FidelityWindow7d and FidelityWindow30d are the look-back windows the
// capture-fidelity endpoint reports over (#236). 7d catches "who stopped
// shipping this week"; 30d is the steady-state denominator for the
// unknown-model cost share and the fidelity-level mix.
const (
	FidelityWindow7d  = 7 * 24 * time.Hour
	FidelityWindow30d = 30 * 24 * time.Hour
)

// DeveloperFidelitySignal is one RAW token_events developer's capture-fidelity
// summary (#236). Developer is the raw token_events key; the API layer
// canonicalizes it through developer_alias (#125) and merges raw keys that
// resolve to the same canonical identity, so an adopter mid-rename is not
// double-counted. Every window below is anchored at the `now` passed to
// DeveloperFidelity.
type DeveloperFidelitySignal struct {
	Developer     string
	EventCount7d  int64
	EventCount30d int64
	// LastEventBySource maps a capture source ("jsonl", "proxy", "api", ...) to
	// the most recent token_events ts for it over ALL history (not just the 30d
	// window): the "when did this source last deliver anything" rollout signal
	// that answers "is this developer's shipper still running".
	LastEventBySource map[string]time.Time
	// FidelityCounts maps a fidelity level ("realtime", "daily", "estimated") to
	// its event count over the 30d window — the capture-quality mix.
	FidelityCounts map[string]int64
	// TotalCostMicro30d is SUM(cost_micro) over the 30d window; UnknownCostMicro30d
	// is the subset billed at the unknown-model pricing GUESS (a model with no exact
	// price-table entry, #267). Their ratio is the per-developer unknown-model cost
	// share — the "how much of this spend is priced at a rate we cannot audit"
	// signal. Both are exact integer micro-dollars; the API converts to a ratio.
	TotalCostMicro30d   int64
	UnknownCostMicro30d int64
}

// DeveloperFidelity returns one capture-fidelity summary per RAW token_events
// developer (#236), anchored at `now`. It is the read behind GET /api/v1/fidelity:
// the rollout dashboard for "which developers are (not) capturing, and at what
// quality". `now` is normalized to UTC in-method — modernc.org/sqlite compares
// DATETIME lexically, so a non-UTC window bound would mis-count rows near the
// boundary (#180) — and drives three cheap GROUP BY queries against the existing
// ts index, never a per-row scan. The API layer canonicalizes and merges the raw
// developer keys (#125); returning raw keys keeps this read identity-policy-free.
func (d *DB) DeveloperFidelity(ctx context.Context, now time.Time) ([]DeveloperFidelitySignal, error) {
	now = now.UTC()
	since7 := now.Add(-FidelityWindow7d)
	since30 := now.Add(-FidelityWindow30d)

	acc := map[string]*DeveloperFidelitySignal{}
	get := func(dev string) *DeveloperFidelitySignal {
		s := acc[dev]
		if s == nil {
			s = &DeveloperFidelitySignal{
				Developer:         dev,
				LastEventBySource: map[string]time.Time{},
				FidelityCounts:    map[string]int64{},
			}
			acc[dev] = s
		}
		return s
	}

	// NOTE(multi-tenancy): the three reads below scan all of token_events with no
	// WHERE tenant_id (#65). When schema tenancy lands they must each gain
	// `tenant_id = ?` (leading the GROUP BY too) or the fidelity endpoint becomes a
	// cross-tenant leak — same standing constraint as DeveloperAliases et al.
	//
	// Query 1: most recent event ts per (developer, source), over ALL history.
	// The ts is read back through a self-join on MAX(ts) rather than SELECTing
	// MAX(ts) directly: modernc.org/sqlite scans a plain DATETIME COLUMN into
	// time.Time, but an aggregate expression loses that column affinity and comes
	// back as an unparseable string. Joining the grouped max to the base row yields
	// ts as a real column again. A (developer, source) with two events at the exact
	// same ts yields two identical rows here — harmless, the map write is idempotent.
	if err := d.forEachRow(ctx,
		`SELECT te.developer, te.source, te.ts
		 FROM token_events te
		 JOIN (
		     SELECT developer, source, MAX(ts) AS mx
		     FROM token_events
		     GROUP BY developer, source
		 ) m ON te.developer = m.developer AND te.source = m.source AND te.ts = m.mx`,
		func(scan func(...any) error) error {
			var dev, source string
			var ts time.Time
			if err := scan(&dev, &source, &ts); err != nil {
				return err
			}
			get(dev).LastEventBySource[source] = ts.UTC()
			return nil
		}); err != nil {
		return nil, err
	}

	// Query 2: per (developer, fidelity) event counts over the 30d window, with a
	// 7d sub-count folded in via CASE so a single scan yields both windows.
	if err := d.forEachRow(ctx,
		`SELECT developer, fidelity,
		        SUM(CASE WHEN ts >= ? THEN 1 ELSE 0 END) AS c7,
		        COUNT(*)                                 AS c30
		 FROM token_events
		 WHERE ts >= ?
		 GROUP BY developer, fidelity`,
		func(scan func(...any) error) error {
			var dev, fidelity string
			var c7, c30 int64
			if err := scan(&dev, &fidelity, &c7, &c30); err != nil {
				return err
			}
			s := get(dev)
			s.EventCount7d += c7
			s.EventCount30d += c30
			s.FidelityCounts[fidelity] += c30
			return nil
		}, since7, since30); err != nil {
		return nil, err
	}

	// Query 3: per (developer, host, model) cost over the 30d window. The
	// unknown-model classification is a price-table property (#267) with no
	// persisted per-row flag, so it is decided in Go via modelIsExactHost — the same
	// NOT-guessed test ComputeCostHost uses. The host MUST be part of the grain and
	// the classification: an open-weights model priced at an audited host-qualified
	// rate (#300) has no model-only entry, and grouping/classifying host-blind would
	// miscount that audited spend as unknown, inflating the headline share.
	if err := d.forEachRow(ctx,
		`SELECT developer, host, model, SUM(cost_micro)
		 FROM token_events
		 WHERE ts >= ?
		 GROUP BY developer, host, model`,
		func(scan func(...any) error) error {
			var dev, host, model string
			var cost int64
			if err := scan(&dev, &host, &model, &cost); err != nil {
				return err
			}
			s := get(dev)
			s.TotalCostMicro30d += cost
			if !modelIsExactHost(host, model) {
				s.UnknownCostMicro30d += cost
			}
			return nil
		}, since30); err != nil {
		return nil, err
	}

	out := make([]DeveloperFidelitySignal, 0, len(acc))
	for _, s := range acc {
		out = append(out, *s)
	}
	// Deterministic order so the JSON response and its tests are stable.
	sort.Slice(out, func(i, j int) bool { return out[i].Developer < out[j].Developer })
	return out, nil
}

// forEachRow runs query with args and invokes fn once per row, passing fn a bound
// scan closure. It centralizes the rows.Close/rows.Err boilerplate so the three
// grouped reads in DeveloperFidelity stay declarative and each row iterator can
// never leak a *sql.Rows on an early return (the classic reused-`rows`-variable
// defer hazard).
func (d *DB) forEachRow(ctx context.Context, query string, fn func(scan func(...any) error) error, args ...any) error {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}
