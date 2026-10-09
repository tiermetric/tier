package main

// #746 — the DEFAULT report shape (no ?since=, no ?until=) must be replayable.
//
// 🔴 WHY THIS FILE EXISTS SEPARATELY FROM verifyreport_test.go: every other
// fixture in the tree passes an EXPLICIT whole-day window, so every other arm is
// blind to the default bound by construction. The one shape an operator gets by
// typing nothing was the one shape `verify-report` refused (rc 2), and nothing
// asserted anything about it.
//
// 🔴 AND THE ANTI-VACUITY RULE THAT SHAPES THE WHOLE FILE: a test that does not
// plant a row in the SLIVER between UTC midnight and now−90d's time of day
// asserts NOTHING about this change. Snapped and unsnapped windows return
// byte-identical bodies over an empty sliver — which is the state of every other
// fixture in this repo. seedDefaultWindowDB exists to make the sliver non-empty.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// sliverDeveloper's entire spend and outcome history sits inside the sliver, so
// its presence in a report is a one-bit readout of whether the default bound was
// snapped. controlDeveloper sits deep inside the window and is present either
// way — without it, "the snap failed" and "the report is empty for some
// unrelated reason" would look the same.
const (
	sliverDeveloper  = "sliver"
	controlDeveloper = "inside"
	// horizonDeveloper owns the OLDEST event in the database. See
	// seedDefaultWindowDB for why that matters.
	horizonDeveloper = "ancient"
)

// The seed's money, named rather than inlined, because the resulting totals are
// PUBLISHED — CHANGELOG.md and docs/api-compatibility.md quote this fixture's
// 9 -> 21 as the measured size of the population change. A published figure that
// no test pins is a figure that drifts silently the next time somebody edits a
// seed constant, so the arms below assert the totals these produce.
const (
	issuesPerDeveloper = 3
	sliverCostUSD      = 4.00 // 3 x $4 = $12, the spend the old bound dropped
	controlCostUSD     = 3.00 // 3 x $3 = $9, the spend both bounds always saw
)

// dayEndMargin is how much of the UTC day must remain for this test to run on a
// single day. Everything here is anchored to "the UTC day containing now"; if
// the day rolled over mid-test the window would move 24h and the sliver rows
// would fall out of it — a RED for a reason that has nothing to do with the code
// under test. stableUTCNow waits rather than flake.
const dayEndMargin = 30 * time.Second

// stableUTCNow returns a UTC instant guaranteed to sit at least one second after
// the start of its UTC day and at least dayEndMargin before the next one.
//
// The first condition is what makes the sliver [midnight, now−90d) non-empty at
// all: at exactly 00:00:00Z the snapped and unsnapped bounds are the same
// instant and there is nothing to measure. The second keeps the whole test on
// one day. Both windows are ~1 second and ~30 seconds wide out of 86,400, so the
// wait is essentially never taken — and taking it is still better than a
// once-a-day red with a misleading transcript.
func stableUTCNow(t *testing.T) time.Time {
	t.Helper()
	for {
		now := time.Now().UTC()
		tod := now.Sub(now.Truncate(24 * time.Hour))
		switch {
		case tod < time.Second:
			time.Sleep(time.Second - tod)
		case tod > 24*time.Hour-dayEndMargin:
			time.Sleep(24*time.Hour - tod + time.Second)
		default:
			return now
		}
	}
}

// seedDefaultWindowDB builds a database whose report over the DEFAULT window
// differs depending on whether the lower bound is snapped to UTC midnight.
//
// Three populations, and each one is load-bearing:
//
//   - horizonDeveloper, 200 days back. 🔴 IT MUST BE SEEDED AND IT MUST BE THE
//     OLDEST ROW. /scores publishes the installation's cost horizon
//     (data_quality.cost_coverage_start) from the earliest captured event in the
//     WHOLE database, with no window predicate. If the sliver row were the
//     earliest event, the snap would move the horizon too and the response body
//     would diverge for a SECOND, unrelated reason — leaving this test green or
//     red for the wrong cause. It is also outside any 90-day window, so it never
//     appears in the report itself.
//   - sliverDeveloper, at exactly the UTC midnight that opens the default
//     window. Included by a snapped bound (ts >= since, since == midnight),
//     excluded by an unsnapped one (since == midnight + today's time of day).
//   - controlDeveloper, 10 days back, i.e. deep inside the window under either
//     bound.
//
// The sizes mirror seedVerifyDB's reasoning: each (developer, issue) clears
// scoring.MinAttributableTokens so no outcome is zero-token-flagged (#136), and
// each developer's total clears scoring.MinRankedCostUSD so the row is ranked
// (#133).
func seedDefaultWindowDB(t *testing.T, now, windowStart time.Time) verifyFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verify-default-window.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// The horizon row FIRST, and deliberately with no outcome: it exists only to
	// pin cost_coverage_start well before anything either window can reach.
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: horizonDeveloper, IssueID: "issue-900", Repo: "other/repo",
		Model: "claude-sonnet-4", InputTok: 10_000,
		CostMicro: store.DollarsToMicro(0.50),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: now.AddDate(0, 0, -200),
	}); err != nil {
		t.Fatalf("seed cost-horizon event: %v", err)
	}

	ids := map[string]int64{}
	seed := func(developer string, ts time.Time, issues []string, costUSD float64) {
		for _, issue := range issues {
			if err := db.InsertTokenEvent(ctx, store.TokenEvent{
				Developer: developer, IssueID: issue, Repo: "acme/tier",
				Model: "claude-sonnet-4", InputTok: 200_000, OutputTok: 20_000,
				CostMicro: store.DollarsToMicro(costUSD),
				Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
				Timestamp: ts,
			}); err != nil {
				t.Fatalf("seed token event %s/%s: %v", developer, issue, err)
			}
			sha := "sha-" + developer + "-" + issue
			if _, err := db.InsertOutcome(ctx, store.Outcome{
				Developer: developer, IssueID: issue, Repo: "acme/tier",
				Weight: 3.0, Quality: 1.0, MergeCommitSHA: sha,
				Source: "api-outcome", WorkType: store.WorkTypeFeature,
				Timestamp: ts,
			}); err != nil {
				t.Fatalf("seed outcome %s/%s: %v", developer, issue, err)
			}
			o, ok, err := db.OutcomeByMergeCommit(ctx, sha)
			if err != nil || !ok {
				t.Fatalf("read back outcome %s: ok=%v err=%v", sha, ok, err)
			}
			ids[sha] = o.ID
		}
	}
	// 🔑 THE SLIVER IS SEEDED AT TWO INSTANTS, AND THE SPLIT IS DELIBERATE. One
	// issue sits at EXACTLY windowStart — the sharpest possible discriminator
	// between a snapped bound (`ts >= midnight`, included) and an unsnapped one
	// (`ts >= midnight+tod`, excluded). The other two sit one second later, so
	// they survive an EXCLUSIVE bound. Without that split, flipping store's
	// `ts >= ?` to `ts > ?` — a change with nothing to do with #746 — would drop
	// the whole developer and this test would accuse the snap of a defect in the
	// comparison operator. Measured: with the split, such a mutation reddens the
	// COST arm ("total_cost_usd = 8, want 12") and leaves the presence arm green,
	// so the transcript names the right thing.
	seed(sliverDeveloper, windowStart, []string{"issue-100"}, sliverCostUSD)
	seed(sliverDeveloper, windowStart.Add(time.Second), []string{"issue-101", "issue-102"}, sliverCostUSD)
	// Deep inside the window under either bound.
	seed(controlDeveloper, now.AddDate(0, 0, -10), []string{"issue-200", "issue-201", "issue-202"}, controlCostUSD)

	return verifyFixture{dbPath: path, outcomeIDs: ids}
}

// TestVerifyReport_DefaultWindowIsReplayable is the #746 guard: a manifest
// served with NO window parameters must carry a midnight-UTC `since`, must have
// scored the sliver rows that bound covers, and must verify clean.
//
// 🔴 WHAT EACH ARM CAN AND CANNOT SEE — measured by mutation, not assumed, because
// "which arm catches which defect" is the only thing that makes a multi-arm test
// worth more than its loudest arm:
//
//   - revert the snap in parseSince's empty branch → (i), (ii) and (iii) ALL red;
//     sliverDeveloper vanishes from the report entirely.
//   - truncate the MANIFEST bound while still serving the un-truncated window
//     (the fabricated verdict #718 exists to refuse) → (i) passes, and (iii)
//     CATCHES IT: rc 1, `"sliver" APPEARED in the report`, outcomes_digest 3 →
//     6 rows. ⚠️ An earlier version of this comment claimed (iii) would be fooled
//     too. Measurement says otherwise, and the reason is worth keeping: (iii)
//     re-runs the window and diffs the RESULT, so it sees a population change
//     that arm (i) — which only reads the published bound — cannot.
//   - delete parseManifestBound's refusal → only the surviving-refusal arm reds.
//   - flip store's `ts >= ?` to `ts > ?` → the COST arm reds, the presence arm
//     stays green (see the two-instant sliver seeding above).
//
// ⇒ arm (ii) is still the one that is load-bearing for the SNAP itself, and it
// is the only arm that would notice a snapped-and-then-ignored bound.
func TestVerifyReport_DefaultWindowIsReplayable(t *testing.T) {
	// ⚠️ .UTC() ALREADY APPLIED (stableUTCNow), THEN AddDate — mirroring
	// api.defaultSince. Doing AddDate on a LOCAL instant instead picks a different
	// UTC day for ~2% of the year on a DST host, and this test would then red with
	// a message blaming the snap for a defect in its own arithmetic. See
	// internal/api's TestDefaultSince_IsIndependentOfHostZone for the measurement.
	now := stableUTCNow(t)
	today := now.Truncate(24 * time.Hour)
	windowStart := now.AddDate(0, 0, -90).Truncate(24 * time.Hour)

	// The vacuity tripwire, stated as an assertion rather than assumed: if the
	// unsnapped bound already WERE midnight there would be no sliver, and every
	// arm below would pass on unfixed code.
	if unsnapped := now.AddDate(0, 0, -90); !unsnapped.After(windowStart) {
		t.Fatalf("the sliver [%s, %s) is empty — this test asserts nothing; "+
			"stableUTCNow was supposed to make that impossible",
			windowStart.Format(time.RFC3339Nano), unsnapped.Format(time.RFC3339Nano))
	}

	f := seedDefaultWindowDB(t, now, windowStart)
	// The DEFAULT shape: no since, no until. Everything else in this suite passes
	// an explicit window, which is exactly why nothing else covers this.
	f = serveFixtureManifest(t, f, "")

	m := readFixtureManifest(t, f.manifestPath)

	// --- arm (i): the published bound is EXACTLY midnight UTC ---
	since, err := time.Parse(time.RFC3339, m.Since)
	if err != nil {
		t.Fatalf("manifest since %q is not RFC3339: %v", m.Since, err)
	}
	if !since.UTC().Equal(windowStart) {
		t.Errorf("manifest since = %s, want %s (the start of the UTC day 90 days back). "+
			"A default bound carrying a time of day is what makes the default report unverifiable (#746)",
			since.UTC().Format(time.RFC3339), windowStart.Format(time.RFC3339))
	}
	if !since.UTC().Equal(since.UTC().Truncate(24 * time.Hour)) {
		t.Errorf("manifest since = %s is not midnight UTC — parseManifestBound will refuse it", m.Since)
	}

	// 🔴 THE TWO SURFACES MUST DESCRIBE THE SAME INSTANT, AND THIS IS LITERALLY
	// THE BUG #746 FIXED — yet nothing in the tree asserted it. /scores echoes the
	// bound as a bare calendar day; /report_manifest publishes it as an RFC3339
	// instant. Before the snap a default request answered "2026-05-31" on one
	// surface and "2026-05-31T05:00:58Z" on the other: the day was a LOSSY
	// rendering of the instant, and no test compared them. Comparing them is the
	// assertion that the echo is TRUE rather than merely well-formed.
	//
	// The fixture already fetched both bodies for the same query, so this costs
	// two lines and closes the gap at its own grain rather than by proxy.
	var echoed struct {
		Since string `json:"since"`
	}
	if err := json.Unmarshal(m.Results.Scores, &echoed); err != nil {
		t.Fatalf("results.scores is not decodable: %v", err)
	}
	if want := since.UTC().Format("2006-01-02"); echoed.Since != want {
		t.Errorf("/scores echoed since=%q but /report_manifest published %q for the SAME request. The echoed "+
			"calendar day must be the day the window actually opens on — a day that merely LOOKS like the "+
			"instant is the #746 defect, not the fix", echoed.Since, m.Since)
	} else if echoed.Since != windowStart.Format("2006-01-02") {
		t.Errorf("both surfaces agree on %q, but the window opens on %q — they are consistently wrong",
			echoed.Since, windowStart.Format("2006-01-02"))
	}

	// --- arm (ii): the sliver rows are IN the scored totals ---
	var env scoresEnvelope
	if err := json.Unmarshal(m.Results.Scores, &env); err != nil {
		t.Fatalf("results.scores is not decodable: %v", err)
	}
	seen := map[string]float64{}
	for _, r := range env.rows() {
		seen[r.label()] = r.TIER
	}
	if _, ok := seen[controlDeveloper]; !ok {
		t.Fatalf("the CONTROL developer %q is missing from the default report — the fixture produced nothing, "+
			"so the sliver assertion below would be measuring an empty report, not a window: %v",
			controlDeveloper, seen)
	}
	if _, ok := seen[sliverDeveloper]; !ok {
		t.Errorf("developer %q — whose entire history sits at the UTC midnight that OPENS the snapped window, "+
			"i.e. inside the sliver [%s, %s) the unsnapped bound skipped — is ABSENT from the default report. "+
			"The default lower bound was not snapped to the start of its UTC day, so a whole day's opening "+
			"spend fell out of a cost metric (#746). Rows present: %v",
			sliverDeveloper, windowStart.Format(time.RFC3339),
			now.AddDate(0, 0, -90).Format(time.RFC3339), seen)
	}

	// 🔴 THE PUBLISHED FIGURE, PINNED. CHANGELOG.md and the API changelog quote
	// this fixture's `total_cost_usd` 9 -> 21 as the measured size of the
	// population change. Neither number is derivable from the doc prose alone, so
	// without these assertions a later edit to a seed constant would leave a
	// published measurement quietly false — the failure mode that stops a
	// changelog being evidence. 9 is the control alone (what the UNSNAPPED bound
	// returned); 21 is control + sliver.
	var costs struct {
		Total *struct {
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"total"`
		Developers []struct {
			Developer    string  `json:"developer"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(m.Results.Scores, &costs); err != nil {
		t.Fatalf("results.scores costs are not decodable: %v", err)
	}
	// 🔑 `total.total_cost_usd` IS THE FIELD THE DOCS NAME — not the sum of the
	// developer rows. They agree on this fixture and can legitimately diverge on a
	// real one (unattributed spend lands in the top-level total and in no
	// developer row), so asserting the reconstruction would pin a number the
	// changelog does not actually quote.
	if costs.Total == nil {
		t.Fatalf("the default report carries no top-level `total` — the published 9 -> 21 figure names " +
			"total.total_cost_usd, so without it this arm has nothing to pin")
	}
	if want := float64(issuesPerDeveloper) * (sliverCostUSD + controlCostUSD); costs.Total.TotalCostUSD != want {
		t.Errorf("default-window total.total_cost_usd = %v, want %v. CHANGELOG.md and the API changelog "+
			"publish this fixture's 9 -> 21 move as the measured effect of #746; if this number changed, fix "+
			"the docs in the same commit rather than relaxing the assertion", costs.Total.TotalCostUSD, want)
	}
	// The per-developer split, so the total cannot be right by coincidence — and
	// so an exclusive-bound regression names the cost, not the snap.
	wantCost := map[string]float64{
		sliverDeveloper:  issuesPerDeveloper * sliverCostUSD,  // 12 — the half the old bound dropped
		controlDeveloper: issuesPerDeveloper * controlCostUSD, // 9  — what the old bound returned alone
	}
	for _, d := range costs.Developers {
		if want, ok := wantCost[d.Developer]; ok && d.TotalCostUSD != want {
			t.Errorf("%s total_cost_usd = %v, want %v", d.Developer, d.TotalCostUSD, want)
		}
	}

	// --- arm (iii): the default manifest verifies clean ---
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("verify-report on a DEFAULT-window manifest exited %d, want %d. This is the whole of #746: the "+
			"shape an operator gets by passing no window at all must be re-runnable.\nstdout=%s\nstderr=%s",
			code, rcReproduced, out, errb)
	}
	if !strings.Contains(out, "REPRODUCED") {
		t.Errorf("rc=%d but the report does not say REPRODUCED:\n%s", code, out)
	}
	// 🔴 rc 0 IS NOT THE SAME AS "EVERY DIMENSION WAS CHECKED", and this file's own
	// thesis is that those two must never render alike. A dimension that degrades
	// to NOT CHECKED can leave rc at 0 and still print REPRODUCED, so both
	// assertions above can pass while a pin goes unexamined. (Since #1033 a band
	// disagreement that withholds a pin exits 2; the absence check below still
	// pins that no band line was printed at all.)
	//
	// The attribution band is the dimension that matters on THIS shape, because
	// half of what #746 buys is that a default manifest's `token_since`
	// (= since − 14d, and AttributableWindow is a whole number of days) is also
	// midnight — which is what makes the band commensurable here for the first
	// time.
	//
	// ⚠️ THE HEALTHY SIGNAL IS THE LINE'S ABSENCE, WHICH IS THE ONE SHAPE OF
	// ASSERTION THIS REPO DISTRUSTS — verifyDimAttributionBand returns no
	// dimension at all when the pinned and derived bands AGREE, and prints a NOT
	// CHECKED line only when they disagree or the value will not parse. So
	// absence is load-bearing, and it is also what an EMPTY `token_since` would
	// produce, for an entirely different reason. Both halves are therefore
	// asserted: the pin exists and is the value we expect, and no NOT-CHECKED
	// line was emitted about it.
	if m.TokenSince == "" {
		t.Error("the default manifest pins no token_since — the band assertion below would then be satisfied " +
			"by the pin being ABSENT rather than by it MATCHING, which are opposite facts")
	} else {
		bandBound, err := time.Parse(time.RFC3339, m.TokenSince)
		if err != nil {
			t.Errorf("manifest token_since %q is not RFC3339: %v", m.TokenSince, err)
		} else if want := since.UTC().Add(-store.AttributableWindow); !bandBound.UTC().Equal(want) {
			t.Errorf("manifest token_since = %s, want %s (since − %s). #746 is what makes this land on "+
				"midnight and so become checkable on a default-window manifest",
				bandBound.UTC().Format(time.RFC3339), want.Format(time.RFC3339), store.AttributableWindow)
		}
	}
	if strings.Contains(out, "attribution band") {
		t.Errorf("verify-report emitted an `attribution band` line on a clean default-window run. That line is "+
			"printed ONLY when the band could not be checked — so rc 0 above was reported over an unexamined "+
			"pin:\n%s", out)
	}

	// 🔴 THE REFUSAL MUST SURVIVE. #746 makes the SERVER stop emitting non-midnight
	// bounds; it does not — and must not — teach the verifier to accept one. A
	// hand-written or legacy manifest carrying a mid-day instant is still a report
	// this build cannot re-run, and truncating it would compare a DIFFERENT
	// window's numbers against this manifest's. Deleting parseManifestBound's
	// refusal would leave every arm above green, which is exactly why this arm is
	// here and not only in TestVerifyReport_NonMidnightWindowIsCouldNotCheck.
	legacy := m
	legacy.Since = "2026-05-31T05:00:58Z"
	legacyPath := filepath.Join(filepath.Dir(f.manifestPath), "hand-written-mid-day.json")
	writeManifest(t, legacyPath, legacy)
	code, out, errb = runVerify(t, legacyPath, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("a hand-written manifest with since=%q exited %d, want %d (could not check). The #718 refusal "+
			"must stay reachable for manifests this server did not emit.\nstdout=%s\nstderr=%s",
			legacy.Since, code, rcCannotCheck, out, errb)
	}
	if !strings.Contains(errb, "not midnight UTC") {
		t.Errorf("the refusal does not name its reason: %q", errb)
	}
	if strings.Contains(out, "REPRODUCED") {
		t.Errorf("an rc=%d run printed REPRODUCED:\n%s", code, out)
	}

	// Belt and braces: everything above is anchored to `today`, so a UTC day
	// rollover mid-test would have moved the window under the fixture. Fail with
	// the reason rather than leaving a confusing red.
	if got := time.Now().UTC().Truncate(24 * time.Hour); !got.Equal(today) {
		t.Fatalf("the UTC day rolled from %s to %s during this test; dayEndMargin (%s) was too small",
			today.Format("2006-01-02"), got.Format("2006-01-02"), dayEndMargin)
	}
}
