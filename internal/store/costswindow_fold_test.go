package store

// #466: DeveloperCostsWindow (the pooled TIER denominator) and
// DeveloperIssueCostsWindow (the per-(developer, repo, issue) grain the work-type
// segments and the segment reconciliation denominate on) are two DIFFERENT queries
// over the same token_events rows with the same window predicate. They must fold to
// the same per-developer totals.
//
// This is the genuine CROSS-QUERY check. The API deliberately does not publish it as a
// wire invariant: handleGetScores issues the two reads non-transactionally, several
// statements apart, over a window whose upper bound is normally open, so a concurrent
// write makes them legitimately disagree and a consumer cannot tell that apart from
// corruption (see developerCostReconciliationJSON, and
// TestSegmentReconciliation_InvariantHoldsUnderConcurrentWrites). Here the database is
// QUIESCENT — no writer runs during the reads — so a discrepancy really does mean one
// of the two queries is wrong, which is exactly the bug worth catching: a GROUP BY
// grain change, a filter that lands on one query and not the other, or a scope clause
// threaded through only one of them.

import (
	"context"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// TestDeveloperCostsWindowFoldsToIssueCosts pins the two queries against each other
// over a fixed database, at fleet scope and at a narrowed repo scope, and for both the
// realtime and total cost columns.
func TestDeveloperCostsWindowFoldsToIssueCosts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	// A fixture that exercises every dimension the two queries could disagree on:
	// several developers, the same issue id in two DIFFERENT repos (the #231 collision
	// shape), repo-blind rows, the unattributed sentinel family, a mix of realtime and
	// non-realtime fidelity, and multiple rows folding into one group.
	rows := []struct {
		dev, issue, repo, fidelity string
		costMicro                  int64
	}{
		{"alice", "#1", "acme/web", "realtime", 100_000},
		{"alice", "#1", "acme/web", "realtime", 50_000},      // same group, must fold
		{"alice", "#1", "acme/api", "realtime", 70_000},      // SAME issue, other repo
		{"alice", "#2", repoid.Unqualified, "daily", 25_000}, // repo-blind, not realtime
		{"alice", UnattributedIssueID, repoid.Unqualified, "realtime", 33_000},
		{"alice", UnattributedMainBucket, "acme/web", "realtime", 11_000},
		{"bob", "#1", "acme/web", "daily", 9_000},
		{"bob", "#3", "acme/api", "realtime", 400_000},
		{"carol", UnattributedNoIssueBucket, repoid.Unqualified, "estimated", 1},
	}
	for i, r := range rows {
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: r.dev,
			IssueID:   r.issue,
			Repo:      r.repo,
			Model:     "claude-sonnet-4",
			InputTok:  2000,
			CostMicro: r.costMicro,
			Source:    "jsonl",
			Fidelity:  r.fidelity,
			Timestamp: now.Add(-time.Duration(i+1) * time.Minute),
		}); err != nil {
			t.Fatalf("InsertTokenEvent[%d]: %v", i, err)
		}
	}

	for _, scope := range []RepoScope{FleetWide, RepoScope("acme/web")} {
		t.Run(string(scope), func(t *testing.T) {
			costs, err := db.DeveloperCostsWindow(ctx, since, time.Time{}, scope)
			if err != nil {
				t.Fatalf("DeveloperCostsWindow: %v", err)
			}
			issueCosts, err := db.DeveloperIssueCostsWindow(ctx, since, time.Time{}, scope)
			if err != nil {
				t.Fatalf("DeveloperIssueCostsWindow: %v", err)
			}

			foldTotal := map[string]int64{}
			foldRealtime := map[string]int64{}
			for _, ic := range issueCosts {
				foldTotal[ic.Developer] += ic.TotalCostMicro
				foldRealtime[ic.Developer] += ic.RealtimeCostMicro
			}

			// The developer SETS must match too, not just the totals of the shared
			// keys: a developer present in one query and absent from the other is the
			// same class of bug and would otherwise pass a totals-only comparison.
			if len(costs) != len(foldTotal) {
				t.Errorf("developer count: DeveloperCostsWindow has %d, folded issue costs has %d",
					len(costs), len(foldTotal))
			}
			seen := map[string]bool{}
			for _, c := range costs {
				seen[c.Developer] = true
				if got, want := foldTotal[c.Developer], c.TotalCostMicro; got != want {
					t.Errorf("%s: folded issue cost = %d micro, DeveloperCostsWindow = %d micro "+
						"(the two scores queries disagree over a quiescent DB — #466)",
						c.Developer, got, want)
				}
				if got, want := foldRealtime[c.Developer], c.RealtimeCostMicro; got != want {
					t.Errorf("%s: folded realtime cost = %d micro, DeveloperCostsWindow = %d micro",
						c.Developer, got, want)
				}
			}
			for dev := range foldTotal {
				if !seen[dev] {
					t.Errorf("%s has issue-grain cost rows but no DeveloperCostsWindow row", dev)
				}
			}

			// Non-vacuity: a fixture that produced no rows would satisfy every check
			// above. Assert the fixture actually reached both queries.
			if len(costs) == 0 || len(issueCosts) == 0 {
				t.Fatalf("vacuous: costs=%d issueCosts=%d — the fixture reached neither query",
					len(costs), len(issueCosts))
			}
			// And that the finer grain really is finer here, so the fold is doing work
			// rather than comparing two one-row-per-developer results.
			if len(issueCosts) <= len(costs) {
				t.Errorf("fixture does not exercise the fold: %d issue-grain rows for %d developers",
					len(issueCosts), len(costs))
			}
		})
	}
}
