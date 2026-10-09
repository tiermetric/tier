package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// TestReportDigests_EventsCoverTheAttributionBandAndOutcomesDoNot is the whole
// contract of ReportDigests in one arm (#740).
//
// 🔴 IT IS THE GUARD AGAINST THE OBVIOUS SIMPLIFICATION. `return
// d.WindowDigests(ctx, since, until, FleetWide)` looks like a strictly tidier body and is
// wrong in both directions at once:
//
//   - events bounded at `since` leaves a token event in [since-14d, since)
//     OUTSIDE the digest. The scoring path READS that row — OutcomeTokenTotals
//     funds an outcome from up to 14 days before `since`, which is why
//     WindowWatermarks widens the token side and why the manifest publishes
//     token_since — so a late ingest there can clear the #136 tripwire and change
//     /scores while the digest holds still. That is the silent miss a content
//     digest exists to close.
//   - outcomes widened to the band would cover an outcome that is not in the
//     report at all, making an unrelated edit a FALSE ALARM — the one failure
//     mode a tamper-evidence surface may not have.
//
// The fixture places exactly one row of each kind in the band-but-not-window
// slice, so BOTH mistakes are reachable and each has its own assertion.
func TestReportDigests_EventsCoverTheAttributionBandAndOutcomesDoNot(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()

	inWindow := digestSince.AddDate(0, 0, 3)
	inBand := digestSince.Add(-AttributableWindow / 2) // before `since`, inside the band

	seedDigestEvent(t, db, "alice", "issue-1", 1_000, inWindow, "k-window")
	seedDigestOutcome(t, db, "alice", "issue-1", 3.0, 1.0, inWindow, "sha-window")

	base, baseOutcomes, err := db.ReportDigests(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("ReportDigests: %v", err)
	}
	if base.Rows != 1 || baseOutcomes.Rows != 1 {
		t.Fatalf("baseline rows = (%d events, %d outcomes), want (1, 1) — the fixture is wrong and every "+
			"assertion below would be about the wrong set", base.Rows, baseOutcomes.Rows)
	}

	// A token event BEFORE `since` but inside the attribution band MUST move the
	// events digest: the report reads it.
	seedDigestEvent(t, db, "alice", "issue-1", 4_000, inBand, "k-band")
	withBandEvent, _, err := db.ReportDigests(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("ReportDigests: %v", err)
	}
	if withBandEvent.Rows != 2 {
		t.Errorf("events rows = %d after adding a row in [since-14d, since), want 2 — the events digest is "+
			"bounded at `since` and therefore does NOT cover the rows this report reads", withBandEvent.Rows)
	}
	if withBandEvent.Value == base.Value {
		t.Errorf("a token event inside the attribution band left the events digest at %s", base.Value)
	}

	// An OUTCOME at the same instant must NOT move the outcomes digest: it is not
	// in the report, and covering it would be a false alarm.
	seedDigestOutcome(t, db, "alice", "issue-9", 3.0, 1.0, inBand, "sha-band")
	_, withBandOutcome, err := db.ReportDigests(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("ReportDigests: %v", err)
	}
	if withBandOutcome.Rows != 1 {
		t.Errorf("outcomes rows = %d after adding an outcome BEFORE `since`, want 1 — the outcomes digest "+
			"has been widened to the token band, so an outcome outside the report now reads as part of it",
			withBandOutcome.Rows)
	}
	if withBandOutcome.Value != baseOutcomes.Value {
		t.Errorf("an outcome outside [since, until) moved the outcomes digest %s -> %s: an edit that cannot "+
			"touch this report would be reported as a divergence", baseOutcomes.Value, withBandOutcome.Value)
	}
}

// TestReportDigests_MatchTheSingleTableReadsOverTheirOwnWindows pins that the
// pairing wrapper changes only the SNAPSHOT, never the values — so a reader can
// trust that ReportDigests and a hand recomputation from the published
// token_since agree byte for byte.
func TestReportDigests_MatchTheSingleTableReadsOverTheirOwnWindows(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedDigestWindow(t, db)
	// BOTH a band event and a band outcome. The event alone makes only the EVENTS
	// half of the comparison below discriminate — with no outcome before `since`,
	// `outcomes == wantOutcomes` holds whichever window ReportDigests used, and the
	// arm would claim to pin "the wrapper changes only the SNAPSHOT, never the
	// values" while being blind on one of the two values. Measured: widening
	// outcomesDigestFrom to the band left this arm green before this row existed.
	seedDigestEvent(t, db, "carol", "issue-3", 7_000, digestSince.Add(-AttributableWindow/2), "k-band")
	seedDigestOutcome(t, db, "carol", "issue-3", 3.0, 1.0, digestSince.Add(-AttributableWindow/2), "sha-band")

	events, outcomes, err := db.ReportDigests(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("ReportDigests: %v", err)
	}
	// The band bound spelled out the way a manifest publishes it (token_since),
	// not by reusing ReportDigests' own arithmetic — sharing the expression would
	// let the test and the code agree by making the same mistake.
	wantEvents, err := db.EventsDigest(ctx, digestSince.Add(-AttributableWindow), digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("EventsDigest: %v", err)
	}
	wantOutcomes, err := db.OutcomesDigest(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("OutcomesDigest: %v", err)
	}
	if events != wantEvents {
		t.Errorf("ReportDigests events = %+v, want %+v (EventsDigest over [token_since, until))", events, wantEvents)
	}
	if outcomes != wantOutcomes {
		t.Errorf("ReportDigests outcomes = %+v, want %+v (OutcomesDigest over [since, until))", outcomes, wantOutcomes)
	}
}

// ---------------------------------------------------------------------------
// The cost of putting the digests on a request path (#740)
// ---------------------------------------------------------------------------

// The two benchmarks below are a PAIR and must be read as one: the first is what
// GET /api/v1/report_manifest already paid, the second is what #740 ADDED. A
// benchmark of the added work alone answers "is a digest fast", which is not the
// question — the question is what happened to the endpoint.
//
// MEASURED, Apple M5 Max, 20,000 in-window token_events + 2,000 outcomes,
// `-benchtime 20x -count 3` (2026-08-29):
//
//	ReportWatermarks    1.42 -  1.49 ms/op    3.7 - 5.6 KB/op     84 - 88 allocs/op
//	ReportDigests      28.5  - 34.6  ms/op    12.15 MB/op        ~725,000 allocs/op
//
// ⇒ the manifest endpoint goes from ~1.4 ms to ~30-36 ms on this fixture: a ~20x
// increase, ~1.5 µs per token_events row, scaling LINEARLY with the window (six
// index-bounded aggregates become two full scans).
//
// 🔴 THAT IS MATERIAL, AND IT IS WHY THE DIGESTS ARE ON THE MANIFEST ENDPOINT AND
// NOWHERE ELSE. A manifest is fetched once per published report, so ~30 ms is
// affordable there. /scores already pays four full-window scans and is polled by
// the dashboard; adding two more is a different decision needing its own
// measurement. ⛔ Do not lift ReportDigests onto the scoring path on the strength
// of these numbers — they say the opposite.
func benchDigestDB(b *testing.B, events int) *DB {
	b.Helper()
	db, err := Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	rows := make([]TokenEvent, 0, events)
	for i := 0; i < events; i++ {
		rows = append(rows, TokenEvent{
			Developer: fmt.Sprintf("dev-%d", i%50), IssueID: fmt.Sprintf("issue-%d", i%500),
			Model: "claude-sonnet-4", InputTok: 1000, OutputTok: 500,
			CostMicro: int64(i), Source: "jsonl", Fidelity: "realtime", Repo: "acme/tier",
			IdempotencyKey: fmt.Sprintf("bench-%d", i),
			Timestamp:      digestSince.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := db.InsertTokenEvents(ctx, rows); err != nil {
		b.Fatalf("seed events: %v", err)
	}
	for i := 0; i < events/10; i++ {
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer: fmt.Sprintf("dev-%d", i%50), IssueID: fmt.Sprintf("issue-%d", i%500),
			Weight: 3, Quality: 1, MergeCommitSHA: fmt.Sprintf("sha-%d", i),
			Source: "api", WorkType: "feature", Repo: "acme/tier",
			Timestamp: digestSince.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			b.Fatalf("seed outcome: %v", err)
		}
	}
	return db
}

// BenchmarkReportManifestWatermarks is the BASELINE half of the pair: what the
// manifest endpoint cost before #740.
func BenchmarkReportManifestWatermarks(b *testing.B) {
	db := benchDigestDB(b, 20000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w, err := db.ReportWatermarks(ctx, digestSince, digestUntil, FleetWide)
		if err != nil {
			b.Fatalf("ReportWatermarks: %v", err)
		}
		if w.Window.TokenEventCount == 0 {
			b.Fatal("the baseline watermark read counted ZERO rows — it is measuring an empty window")
		}
	}
}

// TestExclusionReadIsAWindowSeekNotARepoSeek pins the PLAN the benchmark's
// interpretation rests on, and it exists because the first draft of that
// interpretation was wrong.
//
// 🔴 THE COMMENT USED TO SAY "`repo` IS IN NO INDEX". That is false for
// `outcomes`, which carries idx_outcomes_push_daily_repo ON outcomes(repo,
// issue_id, push_day) — repo LEADING. The conclusion survives only because that
// index is PARTIAL (`WHERE source = 'push'`) and the exclusion query carries no
// `source` predicate, so SQLite cannot use it. That is a chain of reasoning about
// the planner, which is exactly the kind of claim that should be MEASURED rather
// than asserted in a comment — a later change adding `source` to the query would
// silently flip the plan while the prose kept insisting there was no index to hit.
//
// MEASURED, both arms: each statement SEEKS its ts index and evaluates `repo` as
// a filter, so the cost is proportional to the WINDOW and not to the table. That
// is what makes BenchmarkReportManifestExclusion's ~3 ms a window figure.
func TestExclusionReadIsAWindowSeekNotARepoSeek(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)

	tokenWhere, tokenArgs := tsWindow(digestSince.Add(-AttributableWindow), digestUntil)
	where, args := tsWindow(digestSince, digestUntil)

	checked := 0
	for _, c := range []struct {
		name, sql, index string
		args             []any
	}{
		{
			name:  "token_events exclusion",
			sql:   "SELECT COUNT(*), COALESCE(SUM(cost_micro), 0) FROM token_events WHERE " + tokenWhere + " AND repo = ?",
			index: "idx_token_events_ts_id",
			args:  append(append([]any{}, tokenArgs...), repoid.Unqualified),
		},
		{
			name:  "outcomes exclusion",
			sql:   "SELECT COUNT(*) FROM outcomes WHERE " + where + " AND repo = ?",
			index: "idx_outcomes_ts_id",
			args:  append(append([]any{}, args...), repoid.Unqualified),
		},
	} {
		plan := queryPlan(t, db, c.sql, c.args...)
		checked++
		if strings.Contains(plan, "SCAN ") {
			t.Errorf("%s plans as a FULL SCAN (%q) — the exclusion read is now proportional to the TABLE, "+
				"not the window, and the benchmark's figure no longer describes it", c.name, plan)
		}
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s does not seek %s (%q)", c.name, c.index, plan)
		}
	}
	if checked != 2 {
		t.Fatalf("control: checked %d plans, want 2 — this test asserted less than it claims", checked)
	}

	// 🔴 CONTROL, AND IT IS THE HALF THAT MAKES THE ABOVE MEAN ANYTHING. The
	// repo-leading index really does exist, so "no index was used" is a statement
	// about the PARTIAL predicate rather than about an empty schema. If this ever
	// stops finding it, the reasoning in the comment above is stale.
	var ddl string
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_outcomes_push_daily_repo'`,
	).Scan(&ddl); err != nil {
		t.Fatalf("control: idx_outcomes_push_daily_repo not found (%v) — the comment above argues that a "+
			"repo-LEADING index exists and is unusable here; if it no longer exists, that argument is stale", err)
	}
	if !strings.Contains(ddl, "source = 'push'") {
		t.Fatalf("control: idx_outcomes_push_daily_repo is no longer PARTIAL (%q) — it may now be usable by the "+
			"exclusion read, which changes the cost story the benchmark tells", ddl)
	}
}

// BenchmarkReportManifestExclusion is the THIRD member of the set, added by #751.
//
// 🔴 IT EXISTS BECAUSE api/manifest.go SAYS "do not add a third scan here without
// re-running that pair", AND THAT INSTRUCTION IS BINDING WHETHER OR NOT THE
// ADDITION LOOKS CHEAP. This is UnqualifiedExclusionWindow: two index-bounded
// aggregates over the same window, each carrying a `repo = ?` filter that no
// index can serve here — measured, and NOT because there is no repo index (see
// TestExclusionReadIsAWindowSeekNotARepoSeek) — so both touch the table for every
// in-window row.
//
// MEASURED, Apple M5 Max, same 20,000-event / 2,000-outcome fixture,
// `-benchtime 20x`, 3 rounds INTERLEAVED (W, E, D per round — not a naive
// before/after, and not `-count 3`, which repeats each benchmark in a block and
// would let a load excursion land entirely on one of them). 2026-08-29, on a
// BUSY machine: CPU idle 68% at the start of the first run and 37-48% across the
// interleaved rounds, so read these as CEILINGS.
//
//	ReportWatermarks      1.44 -  1.78 ms/op    ~5.3 KB/op      ~87 allocs/op
//	UnqualifiedExclusion  2.98 -  4.56 ms/op    ~1.9 KB/op      ~47 allocs/op
//	ReportDigests        28.7  - 36.5  ms/op   ~12.15 MB/op  ~725,000 allocs/op
//
// ⚠️ IT IS NOT FREE AND THE FIRST DRAFT OF THIS COMMENT GUESSED IT WAS — it read
// "0.29 - 0.36 ms", a tenth of the truth, written before the benchmark was run.
// The exclusion read is TWO TO THREE TIMES the whole watermark read (1.7x - 3.2x
// across the ranges above), because it is two statements where the watermark's
// are index-only. ⚠️ Compute that ratio WITHIN this run, never against the #740
// figures quoted elsewhere: those are a different run, and mixing the operands is
// how "roughly twice" got written when the same-run bound is 3.2x.
//
// ⇒ In context: the endpoint goes from ~30-38 ms to ~33-42 ms, +9.9% to +11.9%
// on top of the ~20x #740 already spent — and it is the SAME query a scoped
// /scores runs for the same window, so no new class of work reaches the store. ⛔ Do not read
// any of these three alone: the point of the set is that the digests dominate, so
// a future addition must be judged against ~30 ms, not against ~3 ms.
func BenchmarkReportManifestExclusion(b *testing.B) {
	db := benchDigestDB(b, 20000)
	ctx := context.Background()
	// Repo-blind rows in BOTH tables, so neither aggregate is measured over an
	// empty match set. The cost is the WINDOW's either way (see
	// TestExclusionReadIsAWindowSeekNotARepoSeek), but a zero denominator would
	// leave that claim untested.
	//
	// ⚠️ THE OUTCOMES HALF WAS MISSING AND THE COMMENT CLAIMED OTHERWISE. benchDigestDB
	// seeds every one of its 2,000 outcomes with Repo "acme/tier", so the second
	// statement — SELECT COUNT(*) FROM outcomes … AND repo = 'unqualified' — matched
	// nothing while the guard below checked only the first. Exactly the "green while
	// measuring nothing" shape, inside the control that exists to prevent it.
	for i := 0; i < 10; i++ {
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "blind", IssueID: fmt.Sprintf("issue-blind-%d", i),
			Model: "claude-sonnet-4", InputTok: 1000, CostMicro: 1234,
			Source: "proxy", Fidelity: "realtime", Repo: "",
			Timestamp: digestSince.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			b.Fatalf("seed repo-blind event: %v", err)
		}
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer: "blind", IssueID: fmt.Sprintf("issue-blind-%d", i),
			Weight: 3, Quality: 1, MergeCommitSHA: fmt.Sprintf("sha-blind-%d", i),
			Source: "api", WorkType: "feature", Repo: "",
			Timestamp: digestSince.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			b.Fatalf("seed repo-blind outcome: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ex, err := db.UnqualifiedExclusionWindow(ctx, digestSince, digestUntil)
		if err != nil {
			b.Fatalf("UnqualifiedExclusionWindow: %v", err)
		}
		// BOTH legs, not just the first: the two statements are independent, and a
		// guard on one of them says nothing about the other.
		if ex.TokenEvents == 0 || ex.OutcomeRecords == 0 {
			b.Fatalf("the exclusion read matched %d token_events / %d outcomes — a zero on either leg means "+
				"that statement is being timed over an empty result set", ex.TokenEvents, ex.OutcomeRecords)
		}
	}
}

// BenchmarkReportManifestDigests is the ADDED half. Read it beside the baseline
// above, never alone.
func BenchmarkReportManifestDigests(b *testing.B) {
	db := benchDigestDB(b, 20000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, o, err := db.ReportDigests(ctx, digestSince, digestUntil, FleetWide)
		if err != nil {
			b.Fatalf("ReportDigests: %v", err)
		}
		// The denominator control: an empty window produces a valid-looking digest
		// in constant time, which would make this benchmark measure nothing.
		if e.Rows == 0 || o.Rows == 0 {
			b.Fatalf("digested %d events / %d outcomes — an empty window is not a scan", e.Rows, o.Rows)
		}
	}
}
