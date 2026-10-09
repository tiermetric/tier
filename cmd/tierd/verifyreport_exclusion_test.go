package main

// The #751 guards: a SCOPED manifest must attest the repo_scope_excluded
// disclosure figure it published, not merely the rows inside its own scope.
//
// 🔴 THE GAP THESE ARMS REPRODUCE. #747 scoped the #716 digests so a `?repo=`
// manifest attests exactly the rows its report read — strictly, so the reserved
// `unqualified` sentinel rows are excluded (a tolerant predicate would
// over-attribute every repo-blind row in the fleet to whichever repository the
// caller named, which is #590). But a scoped /scores STILL READS those rows, to
// publish data_quality.repo_scope_excluded. That figure therefore sat outside
// every identity a scoped manifest ships: an in-place reprice of the sentinel
// rows moved it while the scoped digest AND the scoped watermarks held still,
// and verify-report printed REPRODUCED over changed served bytes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// seedVerifyDBWithSentinel is seedVerifyDB plus THREE repo-blind rows: two token
// events and one outcome.
//
// 🔑 THE SENTINEL ROWS ARE THE WHOLE POINT AND THEY MUST BE REAL. store
// normalizes an empty Repo to repoid.Unqualified on both inserts, so these land
// carrying the reserved sentinel — the rows a strict scope drops and
// UnqualifiedExclusionWindow counts. A fixture without them makes every arm
// below pass over a disclosure that is structurally zero.
//
// 🔴 THE SHAPE IS DELIBERATELY ASYMMETRIC, AND THE FIRST DRAFT WAS NOT. It seeded
// exactly one token event and one outcome, both inside [since, until), which made
// token_events == outcomes == 1 and left the attribution look-back EMPTY. Two real
// mutants measured SURVIVING every arm in this file on that fixture:
//
//   - narrowing UnqualifiedExclusionWindow's token band from
//     `since - AttributableWindow` to `since` — invisible, because no sentinel row
//     sat in the look-back for it to drop;
//   - swapping the emitter's TokenEvents and Outcomes members — invisible, because
//     1 and 1 are indistinguishable when transposed.
//
// Both are exactly the defects this file exists to catch, and both were caught by
// a REVIEWER rather than by a guard. So: TWO token events at DIFFERENT costs, one
// of them in the look-back BEFORE `since`, and ONE outcome — three distinguishable
// numbers (2, $13.00, 1) that no transposition and no narrowed band can reproduce.
//
// ⚠️ The emitter and the verifier both call the same store method, so no
// end-to-end arm here can pin that method's own bounds by agreement — only by a
// fixture whose expected values differ under a different band. That is what the
// look-back row buys.
func seedVerifyDBWithSentinel(t *testing.T) verifyFixture {
	t.Helper()
	f := seedVerifyDB(t)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "carol", IssueID: "issue-200", Repo: "",
		Model: "claude-sonnet-4", InputTok: 100_000,
		CostMicro: store.DollarsToMicro(9.00),
		Source:    "proxy", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed repo-blind token event: %v", err)
	}
	// 🔴 THE LOOK-BACK ROW. 2026-07-25 is BEFORE the window's `since` (2026-08-01)
	// and inside `since - store.AttributableWindow` (2026-07-18), which is the band
	// UnqualifiedExclusionWindow deliberately widens on the token side — a scope can
	// suppress a repo-blind row inside an outcome's 14-day look-back. A different
	// cost from the row above, so the two are also distinguishable by sum.
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "carol", IssueID: "issue-201", Repo: "",
		Model: "claude-sonnet-4", InputTok: 50_000,
		CostMicro: store.DollarsToMicro(4.00),
		Source:    "proxy", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed repo-blind look-back token event: %v", err)
	}
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		Developer: "carol", IssueID: "issue-200", Repo: "",
		Weight: 3.0, Quality: 1.0, MergeCommitSHA: "sha-carol-issue-200",
		Source: "api-outcome", WorkType: store.WorkTypeFeature,
		Timestamp: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed repo-blind outcome: %v", err)
	}
	return f
}

// serveManifestNoResults writes the manifest the running server publishes,
// BYTE FOR BYTE, with no `results` block attached.
//
// 🔴 THAT ABSENCE IS LOAD-BEARING AND IT IS WHY THIS HELPER EXISTS BESIDE
// serveFixtureManifest. `results` is #718's own additive extension; the SERVED
// emitter does not write it, and an operator fetching /api/v1/report_manifest
// gets exactly these bytes. With a results block attached the output comparison
// catches the #751 reprice all by itself — so an arm built on serveFixtureManifest
// would report rc 1 today and prove nothing about the INPUT dimensions, which is
// the half #751 is about.
func serveManifestNoResults(t *testing.T, f verifyFixture, query string) verifyFixture {
	t.Helper()
	f.manifestPath = filepath.Join(filepath.Dir(f.dbPath), "manifest-no-results.json")
	body := serveJSON(t, f.dbPath, "/api/v1/report_manifest?"+query)
	if err := os.WriteFile(f.manifestPath, body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	m := readFixtureManifest(t, f.manifestPath)
	if m.Results != nil {
		t.Fatal("the served manifest carries a results block; this helper's whole purpose is that it does not")
	}
	if len(m.unknownFields) > 0 {
		t.Fatalf("the SERVED manifest carries %d field(s) this binary does not evaluate: %v",
			len(m.unknownFields), m.unknownFields)
	}
	return f
}

// rawVerifyDSN builds the second-connection DSN these arms use to construct a
// state no public writer can reach (an in-place edit that writes no ledger row).
//
// 🔑 `file:` + an explicit busy_timeout, not the bare path. The store's own handle
// may still hold a WAL write lock when one of these opens, and modernc's default
// is to fail immediately rather than wait — which would surface as a flaky
// "database is locked" in a suite whose whole subject is tamper evidence. Mirrors
// internal/api's rawTestDSN; several older cmd/tierd tests still pass the bare
// path, which is why this is a helper rather than another inline literal.
func rawVerifyDSN(path string) string { return "file:" + path + "?_pragma=busy_timeout(5000)" }

// repriceSentinelRows is the issue's probe verbatim: an IN-PLACE reprice of every
// repo-blind token event. It moves no id and no row count, and it touches no row
// inside any repository scope.
func repriceSentinelRows(t *testing.T, dbPath string, deltaMicro int64) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", rawVerifyDSN(dbPath))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(context.Background(),
		`UPDATE token_events SET cost_micro = cost_micro + ? WHERE repo = 'unqualified'`, deltaMicro)
	if err != nil {
		t.Fatalf("reprice sentinel rows: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	return n
}

// scoresExclusion reads data_quality.repo_scope_excluded.cost_usd from a served
// /scores body, and reports whether the key was present at all.
func scoresExclusion(t *testing.T, dbPath, query string) (costUSD float64, present bool) {
	t.Helper()
	var env struct {
		DataQuality struct {
			RepoScopeExcluded *struct {
				TokenEvents int64   `json:"token_events"`
				CostUSD     float64 `json:"cost_usd"`
				Outcomes    int64   `json:"outcomes"`
			} `json:"repo_scope_excluded"`
		} `json:"data_quality"`
	}
	if err := json.Unmarshal(serveJSON(t, dbPath, "/api/v1/scores?"+query), &env); err != nil {
		t.Fatalf("decode /scores: %v", err)
	}
	if env.DataQuality.RepoScopeExcluded == nil {
		return 0, false
	}
	return env.DataQuality.RepoScopeExcluded.CostUSD, true
}

// TestVerifyReport_ScopedExclusionIsAttested is #751's probe, run end to end
// through the SERVED manifest.
func TestVerifyReport_ScopedExclusionIsAttested(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t), query)

	// CONTROL: the disclosure this manifest must attest is genuinely non-zero.
	before, present := scoresExclusion(t, f.dbPath, query)
	if !present || before == 0 {
		t.Fatalf("scoped /scores published repo_scope_excluded present=%v cost_usd=%v; the fixture has no "+
			"repo-blind spend, so every arm below would pass over a figure that cannot move", present, before)
	}

	// A clean re-run reproduces, and the new dimension says so rather than
	// staying silent.
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("clean scoped re-run exit = %d, want %d; stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "UNCHANGED")

	// 🔴 THE ARM THAT IS THE ISSUE. An in-place reprice of the sentinel rows
	// alone: no id moves, no count moves, no row inside acme/tier is touched.
	if n := repriceSentinelRows(t, f.dbPath, 5_000_000); n != 2 {
		t.Fatalf("the reprice touched %d row(s), want 2 (one in-window, one in the look-back) — the probe "+
			"never landed, or the look-back row is not carrying the sentinel", n)
	}
	after, _ := scoresExclusion(t, f.dbPath, query)
	if after == before {
		t.Fatalf("repo_scope_excluded.cost_usd is still %v after the reprice; the SERVED BYTES did not change, "+
			"so there is nothing for the manifest to have failed to attest", before)
	}
	code, out, errb = runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("#751: an in-place reprice of the `unqualified` sentinel rows moved the published "+
			"repo_scope_excluded.cost_usd from %v to %v and verify-report exit = %d, want %d. The scoped manifest "+
			"attests no identity for the disclosure figure it published.\nstdout=%s stderr=%s",
			before, after, code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
	assertDimDetail(t, out, "repo_scope_excluded", "cost $13.00 -> $23.00")
	// 🔴 AND THE RIGHT ATTRIBUTION. A tool that blamed every input for every
	// mismatch would pass a naive rc==1 suite while being useless: nothing inside
	// acme/tier moved, and every line that reads through the scope must say so.
	assertDimStatus(t, out, "events_digest", "UNCHANGED")
	assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
	assertDimStatus(t, out, "token_events", "UNCHANGED")
	assertDimStatus(t, out, "outcomes", "UNCHANGED")
	// ⛔ NO UNATTRIBUTED ASSERTION HERE, AND ITS ABSENCE IS DELIBERATE. An earlier
	// draft checked for it on this very manifest, where the check CANNOT FAIL:
	// verifyResult.unattributed() returns false whenever no `results` block is
	// pinned, and serveManifestNoResults asserts there is none. A guard that is
	// true for every implementation, including a reverted one, is worse than no
	// guard — it reads as coverage. The real arm is
	// TestVerifyReport_ExclusionPinAttributesTheServedMove below, which pins
	// results and carries the control that proves the reading can go the other way.
}

// TestVerifyReport_ExclusionPinAttributesTheServedMove is the end-to-end arm:
// the served NUMBERS move, and #751 is what stops that being blamed on nothing.
//
// 🔴 IT IS THE ONLY ARM IN THIS FILE THAT COMPARES AN OUTPUT. Every other one
// verifies a manifest with no `results` block, which is what the endpoint
// actually publishes — but that shape can never produce an UNATTRIBUTED finding,
// because unattributed() short-circuits on !ResultsPinned. So the interesting
// question ("does the tool now NAME what moved, instead of reporting that the
// numbers moved and not one pinned input did?") is unaskable there.
//
// 🔴 AND THE CONTROL IS THE WHOLE TEST. Asserting "UNATTRIBUTED is absent" means
// nothing unless the same fixture, minus the pin, PRODUCES it. The second half
// strips repo_scope_excluded — reconstructing the exact pre-#751 document — and
// requires the bare finding to come back. Measured: it does.
func TestVerifyReport_ExclusionPinAttributesTheServedMove(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	f := serveFixtureManifest(t, seedVerifyDBWithSentinel(t), query)

	m := readFixtureManifest(t, f.manifestPath)
	if m.Results == nil || len(m.Results.Scores) == 0 {
		t.Fatal("fixture pins no results — the output comparison this arm turns on would not run")
	}
	if m.RepoScopeExcluded == nil {
		t.Fatal("the served scoped manifest carries no pin, so the strip below would change nothing")
	}

	if n := repriceSentinelRows(t, f.dbPath, 5_000_000); n != 2 {
		t.Fatalf("the reprice touched %d row(s), want 2", n)
	}

	// WITH the pin: rc 1, and the moved figure is NAMED.
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
	if strings.Contains(out, "UNATTRIBUTED") {
		t.Errorf("the served numbers moved and repo_scope_excluded named why, yet the run still printed the "+
			"UNATTRIBUTED finding:\n%s", out)
	}

	// 🔴 THE CONTROL: the same database, the same reprice, the PRE-#751 document.
	// Without it the assertion above would hold on a tool that never prints
	// UNATTRIBUTED at all.
	m.RepoScopeExcluded = nil
	legacy := filepath.Join(filepath.Dir(f.dbPath), "legacy-with-results.json")
	writeManifest(t, legacy, m)
	code, out, errb = runVerify(t, legacy, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("control: the pre-#751 manifest exit = %d, want %d; stdout=%s stderr=%s",
			code, rcDiverged, out, errb)
	}
	if !strings.Contains(out, "UNATTRIBUTED") {
		t.Fatalf("control: stripping repo_scope_excluded did NOT reproduce the bare UNATTRIBUTED finding, so "+
			"the assertion above is satisfied by something other than this pin:\n%s", out)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "NOT PINNED")
}

// TestVerifyReport_ExclusionPinDoesNotMoveOnEveryWrite is the ANTI-VACUITY arm.
//
// 🔴 A PIN THAT MOVES ON EVERYTHING IS WORTHLESS, and it is the cheap way to pass
// the arm above. This write touches NEITHER the scope's rows NOR the sentinel
// rows — it lands in a THIRD repository, inside the report's window — so every
// dimension, including the new one, must read UNCHANGED and the run must exit 0.
// ⚠️ IT IS NOT THE ONLY ARM THAT WOULD CATCH A RECOMPUTATION COUNTING EVERY ROW
// IN THE WINDOW — measured, that mutation fails four of the five arms in this
// file, and an earlier draft of this comment claimed "here and nowhere else".
// What IS unique here is the DIRECTION: this is the only arm whose correct answer
// is "nothing moved" AFTER a real write, so it is the only one a pin that moves on
// everything cannot satisfy.
func TestVerifyReport_ExclusionPinDoesNotMoveOnEveryWrite(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t), query)

	// CONTROL FIRST: the write must actually land, or "UNCHANGED" below is a
	// reading about a database nobody touched. seedVerifyDB leaves exactly one
	// other/repo row (bob's pre-window event), so this must take it to two.
	if n := insertVerifyEvent(t, f.dbPath, "other/repo", "bob", "issue-999",
		time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)); n != 2 {
		t.Fatalf("after the other/repo insert the repository holds %d token_events rows, want 2 — the write "+
			"never landed", n)
	}
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("FALSE ALARM: a write to a THIRD repository made an acme/tier-scoped report diverge "+
			"(exit %d, want %d); stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "UNCHANGED")
	assertDimStatus(t, out, "events_digest", "UNCHANGED")
}

// TestVerifyReport_ExclusionPinOnAnOpenEndedWindow closes the one window shape
// every other arm here misses.
//
// 🔴 AN ABSENT `until` IS A DIFFERENT CODE PATH ON BOTH SIDES, not a cosmetic
// variation: the emitter passes a ZERO time.Time and the verifier resolves
// `w.Until` to one, and store.tsWindow drops the upper bound entirely rather than
// binding it. Every arm above pins an explicit `until`, so a verifier that
// substituted `time.Now()`, or an emitter that filled in a synthetic upper bound,
// would be invisible to all of them — and the open-ended manifest is what a
// caller who omits the parameter actually receives.
func TestVerifyReport_ExclusionPinOnAnOpenEndedWindow(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&repo=acme/tier"
	f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t), query)

	m := readFixtureManifest(t, f.manifestPath)
	if m.Until != "" {
		t.Fatalf("fixture manifest pins until = %q; this arm is about the OPEN-ENDED shape", m.Until)
	}
	// 🔴 TWO, NOT ONE, AND THE SECOND IS THE POINT: the look-back row is only
	// counted if the emitter kept UnqualifiedExclusionWindow's widened token band.
	// A narrowed band reads 1 here and this arm names it.
	if m.RepoScopeExcluded == nil || m.RepoScopeExcluded.TokenEvents != 2 {
		t.Fatalf("open-ended scoped manifest pins %+v, want 2 repo-blind token events (one in-window, one in "+
			"the 14-day look-back) — a bound the emitter filled in, a narrowed attribution band, or a fixture "+
			"that landed outside it", m.RepoScopeExcluded)
	}
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("clean open-ended re-run exit = %d, want %d; stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "UNCHANGED")

	// And it still catches the probe with no upper bound to lean on.
	if n := repriceSentinelRows(t, f.dbPath, 5_000_000); n != 2 {
		t.Fatalf("the reprice touched %d row(s), want 2", n)
	}
	code, out, errb = runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d on an open-ended window; stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
}

// TestVerifyReport_ExclusionPinSeesAllThreeQuantities stops the arms above from
// resting on cost alone.
//
// 🔴 THE REPRICE ARM EXERCISES ONE OF THE THREE PINNED MEMBERS. A recomputation
// that compared only cost_micro — or an emitter that pinned only cost — passes it
// and passes the anti-vacuity arm too. Each sub-case below moves a DIFFERENT
// member and asserts the detail NAMES that member, because "the exclusion
// changed" is not an attribution.
func TestVerifyReport_ExclusionPinSeesAllThreeQuantities(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	inWindow := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, dbPath string)
		detail string
	}{
		{
			name: "a late repo-blind token event moves the count",
			mutate: func(t *testing.T, dbPath string) {
				insertSentinelTokenEvent(t, dbPath, inWindow)
			},
			detail: "token_events 2 -> 3",
		},
		{
			name: "a late repo-blind outcome moves the outcome count",
			mutate: func(t *testing.T, dbPath string) {
				insertSentinelOutcome(t, dbPath, inWindow)
			},
			detail: "outcomes 1 -> 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t), query)
			tc.mutate(t, f.dbPath)
			code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcDiverged {
				t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
			}
			assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
			assertDimDetail(t, out, "repo_scope_excluded", tc.detail)
			// The scoped digests are blind to it, which is why this dimension exists.
			assertDimStatus(t, out, "events_digest", "UNCHANGED")
			assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
			// ⛔ And a count move must NOT be described as an in-place reprice.
			assertDimDetailLacks(t, out, "repo_scope_excluded", "in-place reprice")
		})
	}
}

// TestVerifyReport_ExclusionPinSeesTheCountsGoDOWN is the direction every other
// arm misses, and it is the case the production comment actually cites.
//
// 🔴 EVERY OTHER MUTATION HERE ADDS OR REPRICES. verifyDimRepoScopeExcluded's own
// comment motivates the dimension with "a repo repair moves counts DOWN by
// qualifying rows that were blind" — and nothing exercised it. The `want > 0,
// got == 0` boundary matters on its own: it is the same reading a silently broken
// recomputation produces (a filter that matched nothing — #718's shape), so a
// suite that only ever watches numbers grow cannot tell a real repair from a read
// that stopped working.
//
// It is also the only arm where THREE dimensions fire at once and each says
// something different: the exclusion FALLS while the scope GAINS the same rows.
//
// ⚠️ A raw UPDATE, not db.RepairRepo, and the difference is disclosed rather than
// hidden: RepairRepo requires a session_id on the row, and the audit ledger it
// would write is a separate signal. So `repo repairs` correctly reads UNCHANGED
// below — asserted, because counts falling with a SILENT repair ledger is an
// UNAUDITED requalification, which is a different and worse event than a repair.
func TestVerifyReport_ExclusionPinSeesTheCountsGoDOWN(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t), query)

	m := readFixtureManifest(t, f.manifestPath)
	if m.RepoScopeExcluded == nil || m.RepoScopeExcluded.TokenEvents == 0 {
		t.Fatalf("fixture pins %+v — with nothing excluded, a count cannot FALL", m.RepoScopeExcluded)
	}

	// Requalify every repo-blind token event into the scoped repository, which is
	// what a repo repair does to the rows it can place.
	db, err := sql.Open("sqlite", rawVerifyDSN(f.dbPath))
	if err != nil {
		t.Fatalf("open %s: %v", f.dbPath, err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(context.Background(),
		`UPDATE token_events SET repo = 'acme/tier' WHERE repo = 'unqualified'`)
	if err != nil {
		t.Fatalf("requalify: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("control: requalified %d row(s), want 2 — the mutation never landed", n)
	}

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
	assertDimDetail(t, out, "repo_scope_excluded", "token_events 2 -> 0")
	assertDimDetail(t, out, "repo_scope_excluded", "cost $13.00 -> $0.00")
	// 🔴 THE DISCRIMINATOR. A recomputation that had simply stopped matching rows
	// would ALSO read "2 -> 0" while leaving everything else alone. These two say
	// the rows genuinely moved INTO the scope rather than out of the query.
	assertDimStatus(t, out, "events_digest", "CHANGED")
	assertDimStatus(t, out, "token_events", "CHANGED")
	// The outcome leg did not move — one dimension, three members, reported apart.
	assertDimDetailLacks(t, out, "repo_scope_excluded", "outcomes 1 ->")
	// An UNAUDITED requalification: no repair ledger row was written.
	assertDimStatus(t, out, "repo repairs", "UNCHANGED")
}

// TestVerifyReport_FleetWideManifestIsUnchangedBy751 is the REGRESSION arm: every
// fleet-wide manifest already published must keep verifying, with the same digest
// values it has always carried.
//
// 🔴 THE DIGESTS ARE PINNED TO LITERAL CONSTANTS, not merely compared against a
// re-read. A test that recomputed both sides would agree with itself after any
// change to the digest domain, which is the one thing this arm exists to refuse:
// #747's own justification for not bumping digestScheme is that "every digest
// published before #747 still recomputes to the same value".
func TestVerifyReport_FleetWideManifestIsUnchangedBy751(t *testing.T) {
	// The FLEET-WIDE digests over seedVerifyDB's window.
	//
	// 🔑 MEASURED ON THE PRE-#751 TREE, NOT COPIED OUT OF A POST-CHANGE RUN. Both
	// values were read from GET /api/v1/report_manifest with this change stashed;
	// a constant harvested afterwards would agree with whatever the code now does,
	// which is precisely the regression this arm exists to refuse.
	//
	// ⭐ CORROBORATED INDEPENDENTLY, and a later reader can check it without
	// re-creating that stash: `git show main:cmd/tierd/verifyreport_contract_test.go
	// | grep d6519360` finds the SAME events value over the SAME three rows,
	// recorded before this branch existed. ⚠️ That is a COMMENT, not an assertion —
	// it corroborates where this constant came from; it would NOT fail if the value
	// drifted. The assertion below is the only guard.
	//
	// ⚠️ THIS ARM USES seedVerifyDB, WHICH HOLDS NO REPO-BLIND ROWS, and that is
	// deliberate: a sentinel row IS inside a fleet-wide digest, so seeding one would
	// move both constants for a reason unrelated to what they guard. The cost is
	// stated rather than hidden — on this fixture "carries no pin" would ALSO hold
	// for an emitter that merely omitted the pin when empty. The arm that forbids
	// omit-when-clean is api.TestReportManifest_ExclusionPinIsScopedOnlyAndUnconditional,
	// and the sibling assertion below re-checks the fleet-wide absence on a fixture
	// that DOES hold sentinel rows.
	const (
		fleetEventsDigest   = "tierdig1:d6519360bb6dc9d386fac01fdc134fd00336080fbc8df4ac4548249b41b7508e"
		fleetOutcomesDigest = "tierdig1:23124fdc79bc3b5ea38ca47eff6ceb787c73a719eed5fa0291552e236ea15e9b"
	)
	f := serveManifestNoResults(t, seedVerifyDB(t),
		"since="+verifyWindowSince+"&until="+verifyWindowUntil)
	m := readFixtureManifest(t, f.manifestPath)

	if m.Repo != "" {
		t.Fatalf("fixture is scoped to %q; this arm is about the fleet-wide manifest", m.Repo)
	}
	// ⛔ NO EXCLUSION PIN ON A FLEET-WIDE MANIFEST. A fleet-wide report excludes
	// nothing — the sentinel rows are inside its own digests — so pinning an
	// install-wide repo-blind cost figure on a request that named no scope would
	// be adding disclosure to close a reproducibility gap.
	if m.RepoScopeExcluded != nil {
		t.Errorf("the fleet-wide manifest carries repo_scope_excluded = %+v; a fleet-wide report publishes no "+
			"such figure", m.RepoScopeExcluded)
	}
	if m.EventsDigest == nil || m.EventsDigest.Value != fleetEventsDigest {
		t.Errorf("fleet-wide events_digest = %+v, want %s — #751 must not move a value any already-published "+
			"fleet-wide manifest carries", m.EventsDigest, fleetEventsDigest)
	}
	if m.OutcomesDigest == nil || m.OutcomesDigest.Value != fleetOutcomesDigest {
		t.Errorf("fleet-wide outcomes_digest = %+v, want %s", m.OutcomesDigest, fleetOutcomesDigest)
	}

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("fleet-wide re-run exit = %d, want %d; stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}
	// 🔑 AND NO LINE AT ALL, rather than a NOT PINNED one. A NOT PINNED line on a
	// fleet-wide manifest would invite an operator to hunt for a pin that should
	// not exist; verifyDimAttributionBand takes the same shape for the same reason.
	if strings.Contains(out, "repo_scope_excluded") {
		t.Errorf("a fleet-wide report printed a repo_scope_excluded line:\n%s", out)
	}
}

// TestVerifyReport_ExclusionPinLegacyAndMisplaced covers the two shapes that must
// NOT be errors and must NOT be agreement.
func TestVerifyReport_ExclusionPinLegacyAndMisplaced(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"

	// 🔴 LEGACY: a scoped manifest from a pre-#751 emitter. It must still VERIFY —
	// exit 0 — and report NOT PINNED, never an error and never UNCHANGED. Every
	// manifest published before this change is this shape.
	t.Run("a pre-751 scoped manifest still verifies as NOT PINNED", func(t *testing.T) {
		f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t), query)
		m := readFixtureManifest(t, f.manifestPath)
		if m.RepoScopeExcluded == nil {
			t.Fatal("the served scoped manifest carries no pin, so REMOVING it below proves nothing")
		}
		m.RepoScopeExcluded = nil
		legacy := filepath.Join(filepath.Dir(f.dbPath), "legacy.json")
		writeManifest(t, legacy, m)

		code, out, errb := runVerify(t, legacy, "--db", f.dbPath)
		if code != rcReproduced {
			t.Fatalf("a pre-#751 manifest exit = %d, want %d — an additive field must not invalidate an "+
				"already-published manifest; stdout=%s stderr=%s", code, rcReproduced, out, errb)
		}
		assertDimStatus(t, out, "repo_scope_excluded", "NOT PINNED")

		// And the absence must stay a NON-agreement even when the rows HAVE moved:
		// a legacy manifest cannot be made to say UNCHANGED about a figure it never
		// pinned.
		repriceSentinelRows(t, f.dbPath, 5_000_000)
		code, out, _ = runVerify(t, legacy, "--db", f.dbPath)
		if code != rcReproduced {
			t.Fatalf("exit = %d, want %d — an unpinned dimension is reported, never a divergence", code, rcReproduced)
		}
		assertDimStatus(t, out, "repo_scope_excluded", "NOT PINNED")
	})

	// 🔴 MISPLACED: a FLEET-WIDE manifest that carries the pin anyway (hand-written,
	// or a future emitter). The field is "known", so unknownManifestFields is blind
	// to it by construction — without its own branch it would be decoded and
	// silently ignored, which is pinnedButUnexamined's failure mode one dimension
	// over. NOT CHECKED: reported, never agreement, never a divergence.
	t.Run("a fleet-wide manifest that pins it is NOT CHECKED", func(t *testing.T) {
		f := serveManifestNoResults(t, seedVerifyDBWithSentinel(t),
			"since="+verifyWindowSince+"&until="+verifyWindowUntil)
		m := readFixtureManifest(t, f.manifestPath)
		// 🔑 DELIBERATELY EQUAL TO WHAT A RECOMPUTATION WOULD PRODUCE, so this arm
		// separates "not checked" from "checked and agreed". A deliberately wrong
		// pin would read NOT CHECKED for either reason.
		m.RepoScopeExcluded = &manifestRepoScopeExcluded{TokenEvents: 2, CostMicro: 13_000_000, Outcomes: 1}
		path := filepath.Join(filepath.Dir(f.dbPath), "misplaced.json")
		writeManifest(t, path, m)

		code, out, errb := runVerify(t, path, "--db", f.dbPath)
		if code != rcReproduced {
			t.Fatalf("exit = %d, want %d — an unexaminable pin is a report, not a divergence; stdout=%s stderr=%s",
				code, rcReproduced, out, errb)
		}
		assertDimStatus(t, out, "repo_scope_excluded", "NOT CHECKED")
		assertDimDetail(t, out, "repo_scope_excluded", "NO repo scope")
	})
}

// insertSentinelTokenEvent adds one repo-blind token event at ts.
func insertSentinelTokenEvent(t *testing.T, dbPath string, ts time.Time) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "dan", IssueID: "issue-300", Repo: "",
		Model: "claude-sonnet-4", InputTok: 10_000,
		CostMicro: store.DollarsToMicro(1.00),
		Source:    "proxy", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("insert repo-blind token event: %v", err)
	}
}

// insertSentinelOutcome adds one repo-blind outcome at ts.
func insertSentinelOutcome(t *testing.T, dbPath string, ts time.Time) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "dan", IssueID: "issue-300", Repo: "",
		Weight: 3.0, Quality: 1.0, MergeCommitSHA: "sha-dan-issue-300",
		Source: "api-outcome", WorkType: store.WorkTypeFeature,
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("insert repo-blind outcome: %v", err)
	}
}
