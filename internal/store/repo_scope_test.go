package store

// Store-level tests for the #590 RepoScope predicate and the ruling-C exclusion
// disclosure. The API-level control arms live in internal/api/scores_repo_filter_test.go
// and assert on response fields; these assert on the SQL itself, so a scoping bug is
// localized to the read that carries it rather than only showing up as a wrong total
// three layers up.
//
// Every read on the scores path is covered here. That is deliberate and not
// completionism: the failure mode #590 exists to prevent is a PARTIAL scope — some
// reads narrowed, others not — which produces a response that looks scoped and is a
// blend. A per-read test is what makes "all of them" checkable.

import (
	"context"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

const (
	scopeRepoA = "acme/alpha"
	scopeRepoB = "acme/beta"
)

// seedScopedEvent inserts one cost-bearing token_event in a named repository.
func seedScopedEvent(t *testing.T, db *DB, repo, dev, issue string, costUSD float64, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer: dev,
		IssueID:   issue,
		Repo:      repo,
		Model:     "claude-sonnet-4",
		InputTok:  2000,
		CostMicro: DollarsToMicro(costUSD),
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(%s/%s/%s): %v", repo, dev, issue, err)
	}
}

func seedScopedOutcome(t *testing.T, db *DB, repo, dev, issue string, ts time.Time) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), Outcome{
		Developer:      dev,
		IssueID:        issue,
		Repo:           repo,
		Weight:         1.0,
		Quality:        1.0,
		MergeCommitSHA: "sha-" + repo + "-" + issue,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertOutcome(%s/%s/%s): %v", repo, dev, issue, err)
	}
}

// scopeFixture: alpha $1, beta $10, and one repo-blind $100 row whose size makes any
// accidental inclusion impossible to miss in a failure message.
func scopeFixture(t *testing.T, db *DB) time.Time {
	t.Helper()
	now := time.Now().UTC()
	seedScopedEvent(t, db, scopeRepoA, "alice", "A-1", 1.00, now)
	seedScopedEvent(t, db, scopeRepoB, "alice", "B-1", 10.00, now)
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "U-1", 100.00, now)
	seedScopedOutcome(t, db, scopeRepoA, "alice", "A-1", now)
	seedScopedOutcome(t, db, scopeRepoB, "alice", "B-1", now)
	seedScopedOutcome(t, db, repoid.Unqualified, "alice", "U-1", now)
	return now
}

func TestRepoScope_FleetWideIsUnscoped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := scopeFixture(t, db)
	since := now.Add(-time.Hour)
	ctx := context.Background()

	// The zero value must reproduce the pre-#590 read exactly: everything, sentinel
	// rows included. If FleetWide ever silently excluded the sentinel it would change
	// every existing unscoped figure in the product.
	costs, err := db.DeveloperCostsWindow(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("DeveloperCostsWindow: %v", err)
	}
	var total int64
	for _, c := range costs {
		total += c.TotalCostMicro
	}
	if got, want := total, DollarsToMicro(111.00); got != want {
		t.Errorf("fleet-wide total = %d micro, want %d (all three repos incl. unqualified)", got, want)
	}
}

func TestRepoScope_StrictlyExcludesUnqualified(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := scopeFixture(t, db)
	since := now.Add(-time.Hour)
	ctx := context.Background()

	costs, err := db.DeveloperCostsWindow(ctx, since, time.Time{}, RepoScope(scopeRepoA))
	if err != nil {
		t.Fatalf("DeveloperCostsWindow: %v", err)
	}
	var total int64
	for _, c := range costs {
		total += c.TotalCostMicro
	}
	// $101.00 here would mean the tolerant rule leaked in; $11.00 would mean the
	// scope matched the wrong repository.
	if got, want := total, DollarsToMicro(1.00); got != want {
		t.Errorf("scoped total = %d micro, want %d; $101 would mean the 'unqualified' sentinel "+
			"was folded in, which over-attributes unplaceable spend to a named repo", got, want)
	}
}

// TestRepoScope_AllScoresPathReadsAreScoped walks every windowed read the scores path
// uses and asserts each one narrows. A read that is not scoped here is a read that
// silently blends fleet rows into a scoped response.
func TestRepoScope_AllScoresPathReadsAreScoped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := scopeFixture(t, db)
	since := now.Add(-time.Hour)
	ctx := context.Background()
	scope := RepoScope(scopeRepoA)

	t.Run("DeveloperIssueCostsWindow", func(t *testing.T) {
		rows, err := db.DeveloperIssueCostsWindow(ctx, since, time.Time{}, scope)
		if err != nil {
			t.Fatalf("DeveloperIssueCostsWindow: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1 (only %s)", len(rows), scopeRepoA)
		}
		if rows[0].Repo != scopeRepoA {
			t.Errorf("row repo = %q, want %q", rows[0].Repo, scopeRepoA)
		}
	})

	t.Run("AllOutcomesWindow", func(t *testing.T) {
		outs, err := db.AllOutcomesWindow(ctx, since, time.Time{}, scope)
		if err != nil {
			t.Fatalf("AllOutcomesWindow: %v", err)
		}
		if len(outs) != 1 {
			t.Fatalf("outcomes = %d, want 1", len(outs))
		}
		if outs[0].Repo != scopeRepoA {
			t.Errorf("outcome repo = %q, want %q", outs[0].Repo, scopeRepoA)
		}
	})

	t.Run("CostCompositionWindow", func(t *testing.T) {
		// Also guards the bind ORDER: this query prepends two unattributed-family
		// binds before the ts bounds, so the scope's bind must trail all of them. A
		// misordered arg here yields a wrong number, not an error.
		comp, err := db.CostCompositionWindow(ctx, since, time.Time{}, scope)
		if err != nil {
			t.Fatalf("CostCompositionWindow: %v", err)
		}
		if got, want := comp.TotalCostMicro, DollarsToMicro(1.00); got != want {
			t.Errorf("composition total = %d micro, want %d", got, want)
		}
	})

	t.Run("UnattributedBucketCostsWindow", func(t *testing.T) {
		// Same bind-order hazard. Seed unattributed spend in BOTH repos so a scoped
		// read has something to narrow rather than trivially returning empty.
		//
		// ⚠️ OWN DATABASE, deliberately. An earlier version seeded into the SHARED
		// fixture mid-run, which made the CostCompositionWindow subtest above pass only
		// because it happened to execute first — add t.Parallel() or reorder and it
		// breaks. A test whose correctness depends on sibling execution order is a
		// latent false green.
		db, cleanup := newTestDB(t)
		defer cleanup()
		now := scopeFixture(t, db)
		since := now.Add(-time.Hour)
		seedScopedEvent(t, db, scopeRepoA, "alice", UnattributedIssueID, 7.00, now)
		seedScopedEvent(t, db, scopeRepoB, "alice", UnattributedIssueID, 9.00, now)
		rows, err := db.UnattributedBucketCostsWindow(ctx, since, time.Time{}, scope)
		if err != nil {
			t.Fatalf("UnattributedBucketCostsWindow: %v", err)
		}
		var total int64
		for _, r := range rows {
			total += r.CostMicro
		}
		if got, want := total, DollarsToMicro(7.00); got != want {
			t.Errorf("scoped unattributed = %d micro, want %d (beta's $9 must not appear)", got, want)
		}
	})

	t.Run("OutcomeTokenTotals", func(t *testing.T) {
		outs, err := db.AllOutcomesWindow(ctx, since, time.Time{}, scope)
		if err != nil {
			t.Fatalf("AllOutcomesWindow: %v", err)
		}
		totals, err := db.OutcomeTokenTotals(ctx, outs, scope)
		if err != nil {
			t.Fatalf("OutcomeTokenTotals: %v", err)
		}
		// ⚠️ A `for range` over an empty map asserts NOTHING. Without this the subtest
		// passes on a query that returns nothing at all. Note also that this subtest
		// does NOT prove strictness — under the tolerant branch k.repo is already the
		// scoped repo, so both branches agree here. The strictness guard is
		// TestRepoScope_TripwireJoinGoesStrict; do not delete it believing this covers it.
		if len(totals) == 0 {
			t.Fatal("no token totals returned — the assertion below would be vacuous")
		}
		for k := range totals {
			if k.Repo != scopeRepoA {
				t.Errorf("scoped token totals include repo %q, want only %q", k.Repo, scopeRepoA)
			}
		}
	})
}

// TestRepoScope_TripwireJoinGoesStrict is the assertion the subtest above CANNOT make.
//
// ⚠️ Recorded because it nearly shipped as a false green: an earlier version of this
// file checked only that the returned keys carried the scoped repo, on a fixture where
// no repo-blind token row shared an ISSUE ID with a scoped outcome. Under the tolerant
// rule the per-window predicate is `(repo = <scope> OR repo = 'unqualified')`, so with
// no colliding issue id there is nothing for tolerance to admit and strict and tolerant
// return the SAME thing. Mutating the strictness away left that subtest green. A guard
// is guilty until its deletion makes a named test fail.
//
// The collision is the whole scenario: repo-blind spend recorded against an issue id
// that a scoped outcome also uses. Tolerance would let that spend "fund" the scoped
// outcome and SUPPRESS a zero-token flag that should have fired — a false negative on a
// data-quality tripwire, which is worse than a missing one.
func TestRepoScope_TripwireJoinGoesStrict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := time.Now().UTC()
	ctx := context.Background()

	// A scoped outcome whose issue id is ALSO used by a repo-blind token row.
	seedScopedOutcome(t, db, scopeRepoA, "alice", "SHARED-1", now)
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "SHARED-1", 42.00, now)

	outs, err := db.AllOutcomesWindow(ctx, now.Add(-time.Hour), time.Time{}, RepoScope(scopeRepoA))
	if err != nil {
		t.Fatalf("AllOutcomesWindow: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("fixture: scoped outcomes = %d, want 1", len(outs))
	}

	// STRICT: the repo-blind tokens must not be admitted, so this outcome reads as
	// having NO attributable tokens and would correctly trip the #136 zero-token flag.
	strict, err := db.OutcomeTokenTotals(ctx, outs, RepoScope(scopeRepoA))
	if err != nil {
		t.Fatalf("OutcomeTokenTotals(scoped): %v", err)
	}
	for k, v := range strict {
		if k.Repo == repoid.Unqualified && v > 0 {
			t.Errorf("scoped tripwire admitted %d repo-blind tokens for issue %q; tolerance here "+
				"would let another repository's unplaceable spend suppress a zero-token flag",
				v, k.IssueID)
		}
	}

	// Positive control: fleet-wide, the SAME call must find those tokens. Without this
	// arm the assertion above would also pass if the query were simply broken and
	// returned nothing at all.
	fleet, err := db.OutcomeTokenTotals(ctx, outs, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals(fleet): %v", err)
	}
	var fleetTokens int64
	for _, v := range fleet {
		fleetTokens += v
	}
	if fleetTokens == 0 {
		t.Fatal("fleet-wide tripwire found no tokens either — the fixture or the query is broken, " +
			"so the strict assertion above proves nothing")
	}
}

// TestRepoScope_DistinctPriceVersionsScoped guards the one read whose scope predicate
// had to be placed after an existing non-window condition (`price_version > 0`), which
// is exactly where a hand-assembled WHERE clause goes wrong.
func TestRepoScope_DistinctPriceVersionsScoped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := scopeFixture(t, db)
	since := now.Add(-time.Hour)
	ctx := context.Background()

	all, err := db.DistinctPriceVersionsWindow(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("DistinctPriceVersionsWindow(fleet): %v", err)
	}
	if len(all) == 0 {
		t.Fatal("fixture produced no stamped price versions; the scoped assertion below would be vacuous")
	}
	scoped, err := db.DistinctPriceVersionsWindow(ctx, since, time.Time{}, RepoScope(scopeRepoA))
	if err != nil {
		t.Fatalf("DistinctPriceVersionsWindow(scoped): %v", err)
	}
	// Same active table prices every row in this fixture, so the sets match; what is
	// under test is that the scoped query still RUNS and returns a sane set — a
	// misplaced predicate would error or return nothing.
	if len(scoped) != len(all) {
		t.Errorf("scoped versions = %v, fleet = %v", scoped, all)
	}
}

// TestUnqualifiedExclusionWindow is the disclosure half of ruling C.
func TestUnqualifiedExclusionWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := scopeFixture(t, db)
	since := now.Add(-time.Hour)
	ctx := context.Background()

	ex, err := db.UnqualifiedExclusionWindow(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow: %v", err)
	}
	if got, want := ex.CostMicro, DollarsToMicro(100.00); got != want {
		t.Errorf("excluded cost = %d micro, want %d", got, want)
	}
	if ex.TokenEvents != 1 {
		t.Errorf("excluded token_events = %d, want 1", ex.TokenEvents)
	}
	if ex.OutcomeRecords != 1 {
		t.Errorf("excluded outcomes = %d, want 1", ex.OutcomeRecords)
	}
	if !ex.Any() {
		t.Error("Any() = false despite non-zero exclusions")
	}
}

// TestUnqualifiedExclusionWindow_CleanWindow: a fully-qualified window must report
// nothing excluded. Without this, an implementation that always reported an exclusion
// would pass the test above — and the API's omit-when-clean contract, where ABSENCE
// means "this figure is a true total", would be silently broken.
func TestUnqualifiedExclusionWindow_CleanWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := time.Now().UTC()
	seedScopedEvent(t, db, scopeRepoA, "alice", "A-1", 1.00, now)
	seedScopedOutcome(t, db, scopeRepoA, "alice", "A-1", now)
	ctx := context.Background()

	ex, err := db.UnqualifiedExclusionWindow(ctx, now.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow: %v", err)
	}
	if ex.Any() {
		t.Errorf("clean window reports exclusions: %+v", ex)
	}
}

// TestUnqualifiedExclusionWindow_EmptyWindowNoNullScan guards the COALESCE on the SUM.
// An empty match set makes SQLite's SUM return NULL, which will not scan into an
// int64 — the read would fail at runtime on the most ordinary input there is: a window
// with no repo-blind rows in it.
func TestUnqualifiedExclusionWindow_EmptyWindowNoNullScan(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ex, err := db.UnqualifiedExclusionWindow(ctx, time.Now().UTC().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow on an empty store: %v", err)
	}
	if ex.Any() {
		t.Errorf("empty store reports exclusions: %+v", ex)
	}
}

// TestUnqualifiedExclusionWindow_RespectsWindow: the disclosure must describe THIS
// window, not the whole table. A disclosure that silently reported lifetime totals
// would overstate what a short window's scope actually dropped.
//
// ⚠️ The token-side lower bound is deliberately `since - AttributableWindow`, not
// `since` — see UnqualifiedExclusionWindow's doc for why (the scoped tripwire join
// reaches into that look-back, so exclusions there are real and must not read as
// "clean"). This test was written before that widening and initially used a 72-hour-old
// row as its out-of-window arm; that row is now legitimately INSIDE the look-back, so
// the arm was moved well outside it rather than the widening being reverted. Both
// bounds are asserted below so neither can drift unnoticed.
func TestUnqualifiedExclusionWindow_RespectsWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	ctx := context.Background()

	// In the reporting window.
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "NEW-1", 5.00, now)
	// Inside the 14-day attributable look-back, OUTSIDE the reporting window: counted,
	// because a scoped tripwire join can reach it.
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "LOOKBACK-1", 7.00, now.Add(-7*24*time.Hour))
	// Comfortably outside BOTH: must never be counted.
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "ANCIENT-1", 50.00, now.Add(-30*24*time.Hour))

	ex, err := db.UnqualifiedExclusionWindow(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow: %v", err)
	}
	if got, want := ex.CostMicro, DollarsToMicro(12.00); got != want {
		t.Errorf("excluded cost = %d micro, want %d ($5 in-window + $7 in the look-back; "+
			"the 30-day-old $50 row must be excluded from both)", got, want)
	}
	if ex.TokenEvents != 2 {
		t.Errorf("excluded token_events = %d, want 2", ex.TokenEvents)
	}
}

// TestUnqualifiedExclusionWindow_CoversAttributableLookback isolates the widened bound
// with its own control arm, so "counts the look-back" cannot be confused with "counts
// everything". The two rows sit one day either side of the `since - 14d` boundary.
func TestUnqualifiedExclusionWindow_CoversAttributableLookback(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	ctx := context.Background()

	// 1 day INSIDE the look-back boundary.
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "IN-1", 3.00, since.Add(-AttributableWindow).Add(24*time.Hour))
	// 1 day OUTSIDE it — the control. Without this arm, a query that ignored the lower
	// bound entirely would pass the assertion below.
	seedScopedEvent(t, db, repoid.Unqualified, "alice", "OUT-1", 90.00, since.Add(-AttributableWindow).Add(-24*time.Hour))

	ex, err := db.UnqualifiedExclusionWindow(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow: %v", err)
	}
	if got, want := ex.CostMicro, DollarsToMicro(3.00); got != want {
		t.Errorf("excluded cost = %d micro, want %d — the look-back must include the row one day "+
			"inside the boundary and exclude the one a day outside it", got, want)
	}
}
