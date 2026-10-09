package store

import (
	"context"
	"fmt"
	"time"
)

// Watermarks is the as-of position of every mutation-bearing ledger a report
// reads (#715). It is the second half of a report's identity: the window
// predicate alone is an UNSTABLE identity, because late JSONL ingestion adds
// rows carrying yesterday's `ts` that legitimately join "the same window". Two
// runs of the same query over the same [since, until) can therefore return
// different numbers and both be correct. A report binds to (predicate, as-of),
// never the predicate alone.
//
// 🔴 WHY THERE ARE FIVE LEDGERS AND NOT ONE. A watermark on `token_events.id`
// alone CANNOT bound a report, and that is measured, not reasoned:
// `outcomes.quality` is MUTATED IN PLACE by UpdateQuality and
// UpdateQualityForOutcome. An outcome's quality — a direct multiplier on every
// weighted point the report publishes — can change with NO NEW ROW anywhere in
// `token_events` or `outcomes`. `quality_history` is the append-only transition
// log that makes such a revision visible, and it is joined by four more
// in-place-mutation ledgers: `reprice_row_audit` (cost_micro/price_version/
// billing_mode rewritten by Reprice), `cost_correction_audit` (cost_micro
// rewritten by a sanctioned POST /costs override, #346),
// `repo_repair_row_audit` (repo rewritten by RepairRepo, #493) and
// `push_outcome_audit` (a push outcome's developer/ts re-derived, or the row
// superseded by a merged PR, #849).
// TestWatermarks_QualityRevisionIsInvisibleToTheObviousSequences drives exactly
// that case: the two obvious id sequences do not move and the report changes.
//
// 🔑 THE THREE SIGNALS AND WHAT EACH ONE CATCHES. This is the whole design, and
// it is why both a MAX and a COUNT are published for every ledger:
//
//	MAX(id)  catches an INSERT      — a new row raises the high-water mark.
//	COUNT(*) catches a DELETE       — EraseDeveloper hard-deletes; ids never
//	                                  come back, so only the count moves.
//	neither  catches an in-place UPDATE — which is precisely why the five
//	                                  ledgers above are watermarked at all: an
//	                                  UPDATE that matters appends to one of them.
//
// ⚠️ TWO INDEPENDENT ID SEQUENCES, NOT ONE. `token_events.id` and `outcomes.id`
// are separate AUTOINCREMENT counters. A singular "max event id" cannot bound
// outcomes, and nothing about one sequence's value constrains the other's.
//
// ⚠️ SCOPE OF THE CLAIM, stated precisely because an over-claim here is worse
// than no manifest at all. Equal watermarks (with an equal window, scope, price
// table and rubric) mean no ledger this block watches has moved. They do NOT
// prove byte-identical output. FIVE report inputs are NOT covered, and the count
// is stated because an earlier draft of this comment said "two" — an
// enumeration that undercounts is the same defect as a watermark that misses a
// ledger, one level up:
//
//   - `period_membership` is UPDATEd in place (period_end) with no ledger of its
//     own, and it selects the cohort in team/division aggregation. A membership
//     edit can therefore reshape an anonymized report while every watermark here
//     holds still. A MAX(id)/COUNT(*) pair CANNOT see an in-place UPDATE, so
//     emitting one for this table would be a guard that looks like a guard and
//     is not; closing it needs its own append-only ledger. Left uncovered and
//     documented rather than half-covered.
//   - `developer_alias` is upserted and deleted in place (UpsertDeveloperAlias /
//     DeleteDeveloperAlias) and RE-KEYS THE ENTIRE cost-to-outcome join — the
//     single highest-leverage uncovered input here. Read on the scores path to
//     canonicalize every identity.
//   - `hierarchy_membership` (#886) selects the team/division rows. It is dated
//     and append-only, so a write cannot move a window that ended before it, but
//     it is not watermarked, so a window still open at the write can move unseen.
//   - `actual_spend` feeds per-developer Spend Leverage on a fleet-wide read.
//   - Open()-time migrations (migrateCacheWriteSplit, the cost_usd->cost_micro
//     conversion, the price_version backfill and the cost recompute) rewrite
//     token_events with no ledger row at all. Those fire on a BINARY UPGRADE, so
//     the manifest's tool_version/commit stamp is what makes them detectable —
//     not this block.
//
// The first four share one root cause: they are IDENTITY-AND-GROUPING tables
// that this schema mutates in place and keeps no transition log for. Covering
// them is not a matter of adding four more MAX(id) reads — a MAX cannot see an
// in-place UPDATE at all — it needs ledgers those tables do not have. That is a
// separate piece of work, and until it exists this manifest's honest reading is
// "no ROW-LEVEL evidence I watch has moved", not "the report is reproducible".
// 🔑 THE SPLIT INTO Window AND Ledgers IS THE MOST IMPORTANT THING THIS TYPE
// SAYS, so it is structural rather than a comment on a flat field list. The two
// halves are read over DIFFERENT predicates and carry DIFFERENT disclosure risk,
// and a consumer that cannot see which is which will read one as the other.
type Watermarks struct {
	// Window is scoped to [since, until) and the repo scope. It is a statement
	// about THIS report's rows.
	//
	// 🔒 IT IS ALSO THE HALF THAT IS A COUNT OF WORK OVER A CALLER-CHOSEN
	// POPULATION, so it is the half a k-anonymity floor has to be able to
	// withhold. See api.reportManifestJSON — the served surface omits it whole
	// in an anonymized aggregation mode rather than publishing an unfloored
	// window aggregate.
	Window WindowWatermarks `json:"window"`
	// Ledgers is UNWINDOWED and UNSCOPED — install-wide positions of the five
	// append-only mutation ledgers.
	Ledgers LedgerWatermarks `json:"ledgers"`
}

// WindowWatermarks is the position of the two windowed row sequences: the rows a
// report actually reads. A late-arriving row that lands INSIDE the window moves
// these, and an unrelated insert outside it does not — which is exactly the
// instability a manifest exists to expose.
//
// 🔴 THE TWO SIDES DO NOT SHARE A LOWER BOUND, AND THAT IS NOT A BUG — IT IS THE
// ONLY WAY THIS BOUNDS WHAT THE REPORT ACTUALLY READS.
//
//	outcomes     [since, until)
//	token_events [since - AttributableWindow, until)   <- 14 days wider
//
// The scoring path does NOT read token_events only inside [since, until).
// OutcomeTokenTotals — the #136 zero-token tripwire — builds a per-outcome
// window [merge - AttributableWindow, merge], so an outcome near the lower edge
// of the report window is funded by token_events up to 14 days BEFORE `since`.
// UnqualifiedExclusionWindow already carries this exact asymmetry for the same
// reason (see its `tsWindow(since.Add(-AttributableWindow), until)`).
//
// Get this wrong and the manifest fails at precisely the job it exists for: a
// late-ingested row 20 days before `since` can lift a (developer, issue) total
// past scoring.MinAttributableTokens, clear the tripwire, un-suppress a
// developer and change /scores — while a watermark bounded at `since` holds
// perfectly still. Two different reports, identical manifests, reading as
// "nothing has changed". An earlier draft of this file had exactly that defect
// and a comment claiming predicate parity with the scoring reads.
//
// ⚠️ SO `token_event_count` IS NOT "rows in the report's window". It is rows in
// the report's ATTRIBUTION BAND, and it is legitimately larger. The served
// manifest publishes the band's resolved lower bound (`token_since`) so a
// consumer never has to infer it.
type WindowWatermarks struct {
	MaxTokenEventID int64 `json:"max_token_event_id"`
	TokenEventCount int64 `json:"token_event_count"`
	MaxOutcomeID    int64 `json:"max_outcome_id"`
	OutcomeCount    int64 `json:"outcome_count"`
}

// LedgerWatermarks is the position of the five append-only mutation ledgers.
//
// 🔴 THEY ARE GLOBAL, NOT WINDOWED, AND THAT ASYMMETRY IS DELIBERATE. Every one
// of these tables stamps `ts DEFAULT CURRENT_TIMESTAMP` — the instant of the
// MUTATION, which has nothing to do with the `ts` of the row it mutated. A
// quality revision made today against a June outcome carries today's ts, so
// filtering these ledgers by the report's own window would hide exactly the
// revision the manifest exists to expose. Unwindowed is also the conservative
// direction: it can report a change that did not touch this window (a false
// "look again"), never miss one that did.
//
// ⚠️ It also keeps ReportWatermarks clear of the CURRENT_TIMESTAMP keyset trap:
// these columns store second-precision 'YYYY-MM-DD HH:MM:SS' text, not Go's
// time.Time rendering, so a bound time.Time would silently match no rows.
// Nothing in this file binds a time against these five.
//
// Field names track the PHYSICAL tables. `reprice_row_audit` and
// `repo_repair_row_audit` each have a sibling AGGREGATE ledger (`reprice_audit`,
// `repo_repair_audit`) that is written once per RUN; the row ledgers are the ones
// written once per MUTATED ROW, in the same transaction as the UPDATE, and they
// are what a replay needs. Naming these "reprice_audit" would point a reader at
// the wrong table.
type LedgerWatermarks struct {
	MaxQualityHistoryID      int64 `json:"max_quality_history_id"`
	QualityHistoryCount      int64 `json:"quality_history_count"`
	MaxRepriceRowAuditID     int64 `json:"max_reprice_row_audit_id"`
	RepriceRowAuditCount     int64 `json:"reprice_row_audit_count"`
	MaxCostCorrectionAuditID int64 `json:"max_cost_correction_audit_id"`
	CostCorrectionAuditCount int64 `json:"cost_correction_audit_count"`
	MaxRepoRepairRowAuditID  int64 `json:"max_repo_repair_row_audit_id"`
	RepoRepairRowAuditCount  int64 `json:"repo_repair_row_audit_count"`
	// push_outcome_audit (#849): a merged PR superseding a push outcome, or a
	// push outcome's owner being re-derived. The re-derivation rewrites
	// outcomes.developer/ts in place, so only this ledger shows it moved.
	MaxPushOutcomeAuditID int64 `json:"max_push_outcome_audit_id"`
	PushOutcomeAuditCount int64 `json:"push_outcome_audit_count"`
}

// watermarkLedgers maps every PHYSICAL table Watermarks covers to the prefix its
// JSON fields carry (`max_<prefix>_id` and `<prefix>_count`). The two differ for
// the first pair only, and only in number: the tables are plural, while the
// manifest field names are singular because each names ONE row's id.
//
// It exists so the coverage pin (TestWatermarks_CoverEveryLedgerInTheLiveSchema)
// can check the struct against the LIVE schema rather than against a second
// hand-list in the test — a hand-list on both sides is a tautology that a newly
// added ledger could never redden.
var watermarkLedgers = map[string]string{
	"token_events":          "token_event",
	"outcomes":              "outcome",
	"quality_history":       "quality_history",
	"reprice_row_audit":     "reprice_row_audit",
	"cost_correction_audit": "cost_correction_audit",
	"repo_repair_row_audit": "repo_repair_row_audit",
	"push_outcome_audit":    "push_outcome_audit",
}

// ReportWatermarks reads the as-of position of every ledger in Watermarks for
// the half-open window [since, until) under `scope` (#715). A zero `until` is
// open-ended, matching every other windowed read.
//
// 🔴 ALL SEVEN LEDGERS ARE READ IN ONE TRANSACTION, AND THAT IS A CORRECTNESS
// REQUIREMENT, NOT TIDINESS. The whole product of this function is a claim about
// ONE INSTANT. Read across separate connections, a quality revision committing
// between the outcomes read and the quality_history read yields a manifest whose
// two halves describe database states that never coexisted — an as-of stamp for
// a moment that never happened, which is worse than no stamp because it looks
// authoritative. A DEFERRED transaction in WAL mode takes its snapshot at the
// FIRST READ and holds it, so every SELECT below sees the same state.
//
// It takes no write lock and blocks no writer: WAL readers and the single writer
// do not exclude each other. Cost is a range scan bounded by idx_token_events_ts_id
// / idx_outcomes_ts_id — index-only when fleet-wide; a scoped read must also touch
// the table, because `repo` is in neither index. It is NOT a constant-time seek
// and this comment does not claim one.
//
// ⚠️ WAL CADENCE, not duration, is the thing to watch here. The measurement on
// beginRead shows WAL growth tracking how CONTINUOUSLY some read transaction is
// open, not how long any single one runs. beginRead's only previous caller was a
// human-initiated DSAR export, where the gap between calls is the bound; this is
// the first REQUEST-PATH holder, and a report generator can poll it in a loop.
// The transaction is deliberately kept to four statements and no application
// work for that reason. If a future caller adds work inside it, re-read that
// measurement first.
func (d *DB) ReportWatermarks(ctx context.Context, since, until time.Time, scope RepoScope) (Watermarks, error) {
	// beginRead, not a hand-rolled BeginTx. Beyond consistency, the package has a
	// census test (TestConvertedSiteTableIsAnExhaustiveCensus) that AST-walks for
	// calls to the three begin helpers and fails on an unclassified site — a raw
	// d.db.BeginTx is INVISIBLE to it, so hand-rolling here would have quietly
	// removed this site from a guard whose entire job is to notice new ones.
	// Its release func rolls back; a read transaction is never committed.
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return Watermarks{}, fmt.Errorf("ReportWatermarks: %w", err)
	}
	// Always released. A read transaction left open holds a WAL read mark and a
	// pooled connection; on a bounded pool that is a hang, not a leak you notice
	// later.
	defer release()

	return reader{tx}.ReportWatermarks(ctx, since, until, scope)
}

// ReportWatermarks shares the caller's snapshot with its other report reads.
func (r reader) ReportWatermarks(ctx context.Context, since, until time.Time, scope RepoScope) (Watermarks, error) {
	var w Watermarks
	var err error
	tx := r.q

	scopeSQL, scopeArgs := scope.clause()

	// 🔴 THE TOKEN SIDE IS WIDENED BY AttributableWindow AND THE OUTCOME SIDE IS
	// NOT. This asymmetry mirrors UnqualifiedExclusionWindow exactly, and it is
	// what makes the watermark bound what the report READS rather than what the
	// report's window NAMES — see WindowWatermarks for the failure it prevents.
	tokenWhere, tokenArgs := tsWindow(since.Add(-AttributableWindow), until)
	tokenArgs = append(tokenArgs, scopeArgs...)
	w.Window.MaxTokenEventID, w.Window.TokenEventCount, err =
		windowedWatermark(ctx, tx, "token_events", tokenWhere+scopeSQL, tokenArgs)
	if err != nil {
		return Watermarks{}, err
	}

	// Outcomes are read over the report's own window, matching AllOutcomesWindow.
	outcomeWhere, outcomeArgs := tsWindow(since, until)
	outcomeArgs = append(outcomeArgs, scopeArgs...)
	w.Window.MaxOutcomeID, w.Window.OutcomeCount, err =
		windowedWatermark(ctx, tx, "outcomes", outcomeWhere+scopeSQL, outcomeArgs)
	if err != nil {
		return Watermarks{}, err
	}

	// The five mutation ledgers, unwindowed and unscoped — see the field comment.
	// One statement so all five share the snapshot without five more round trips.
	if err := tx.QueryRowContext(ctx, `
		SELECT
		    (SELECT COALESCE(MAX(id), 0) FROM quality_history),
		    (SELECT COUNT(*)             FROM quality_history),
		    (SELECT COALESCE(MAX(id), 0) FROM reprice_row_audit),
		    (SELECT COUNT(*)             FROM reprice_row_audit),
		    (SELECT COALESCE(MAX(id), 0) FROM cost_correction_audit),
		    (SELECT COUNT(*)             FROM cost_correction_audit),
		    (SELECT COALESCE(MAX(id), 0) FROM repo_repair_row_audit),
		    (SELECT COUNT(*)             FROM repo_repair_row_audit),
		    (SELECT COALESCE(MAX(id), 0) FROM push_outcome_audit),
		    (SELECT COUNT(*)             FROM push_outcome_audit)`,
	).Scan(
		&w.Ledgers.MaxQualityHistoryID, &w.Ledgers.QualityHistoryCount,
		&w.Ledgers.MaxRepriceRowAuditID, &w.Ledgers.RepriceRowAuditCount,
		&w.Ledgers.MaxCostCorrectionAuditID, &w.Ledgers.CostCorrectionAuditCount,
		&w.Ledgers.MaxRepoRepairRowAuditID, &w.Ledgers.RepoRepairRowAuditCount,
		&w.Ledgers.MaxPushOutcomeAuditID, &w.Ledgers.PushOutcomeAuditCount,
	); err != nil {
		return Watermarks{}, fmt.Errorf("ReportWatermarks: mutation ledgers: %w", err)
	}

	return w, nil
}

// windowedWatermark reads COALESCE(MAX(id),0) and COUNT(*) for one windowed
// table in a single statement.
//
// 🔒 `table` and `where` are BOTH built from package-internal literals by
// ReportWatermarks — the two table names are string constants at the call sites
// above, and `where` is assembled by tsWindow and RepoScope.clause, neither of
// which interpolates a value. Every caller-controlled datum (the two window
// bounds and the repo slug) arrives in `args` as a bound `?` parameter. Keep it
// that way: this function concatenates its SQL, so a future caller passing a
// request-derived name would turn it into an injection site.
//
// COALESCE is load-bearing: MAX over an empty window returns SQL NULL, which
// fails a scan into int64. An empty window's watermark is 0, and 0 is a real
// answer ("nothing here yet"), not an error.
func windowedWatermark(ctx context.Context, tx readQuerier, table, where string, args []any) (maxID, count int64, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0), COUNT(*) FROM `+table+` WHERE `+where, args...,
	).Scan(&maxID, &count)
	if err != nil {
		return 0, 0, fmt.Errorf("ReportWatermarks: %s: %w", table, err)
	}
	return maxID, count, nil
}
