package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// eraseDuringReadStore commits token and outcome deletions after the first pooled read, or
// after fixing ReadSnapshot's state and before its callback. No timing race is
// needed: a separate WAL connection commits while the reader remains open.
type eraseDuringReadStore struct {
	Store
	t                        *testing.T
	raw                      *sql.DB
	fired, snapshots, pooled int
	released                 bool
}

func (s *eraseDuringReadStore) erase(ctx context.Context) {
	s.t.Helper()
	if s.fired != 0 {
		return
	}
	for _, table := range []string{"token_events", "outcomes"} {
		if _, err := s.raw.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			s.t.Fatalf("concurrent %s deletion: %v", table, err)
		}
	}
	s.fired++
}

func (s *eraseDuringReadStore) DeveloperCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperCost, error) {
	s.pooled++
	rows, err := s.Store.DeveloperCostsWindow(ctx, since, until, scope)
	if err == nil {
		s.erase(ctx)
	}
	return rows, err
}

func (s *eraseDuringReadStore) ReportWatermarks(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.Watermarks, error) {
	s.pooled++
	marks, err := s.Store.ReportWatermarks(ctx, since, until, scope)
	if err == nil {
		s.erase(ctx)
	}
	return marks, err
}

func (s *eraseDuringReadStore) CostCoverageStart(ctx context.Context, scope store.RepoScope) (time.Time, bool, error) {
	s.pooled++
	return s.Store.CostCoverageStart(ctx, scope)
}

func (s *eraseDuringReadStore) SourceCoverageStart(ctx context.Context, scope store.RepoScope) (map[string]time.Time, error) {
	s.pooled++
	return s.Store.SourceCoverageStart(ctx, scope)
}

func (s *eraseDuringReadStore) HierarchyMembership(ctx context.Context) ([]store.MembershipRow, error) {
	s.pooled++
	return s.Store.HierarchyMembership(ctx)
}

// Trap every windowReader method, including reads whose results happen to be
// unchanged by the deletion. Embedded Store forwarding would hide a snapshot bypass.
func (s *eraseDuringReadStore) DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DevIssueCost, error) {
	s.pooled++
	return s.Store.DeveloperIssueCostsWindow(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) DeveloperEvidenceWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperCostEvidence, error) {
	s.pooled++
	return s.Store.DeveloperEvidenceWindow(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) BotDevelopers(ctx context.Context, since, until time.Time) ([]string, error) {
	s.pooled++
	return s.Store.BotDevelopers(ctx, since, until)
}

func (s *eraseDuringReadStore) CostCompositionWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.CostComposition, error) {
	s.pooled++
	return s.Store.CostCompositionWindow(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) UnattributedBucketCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.UnattributedBucketCost, error) {
	s.pooled++
	return s.Store.UnattributedBucketCostsWindow(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]int, error) {
	s.pooled++
	return s.Store.DistinctPriceVersionsWindow(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) AllOutcomesWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.Outcome, error) {
	s.pooled++
	return s.Store.AllOutcomesWindow(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) OutcomeTokenTotals(ctx context.Context, outcomes []store.Outcome, scope store.RepoScope) (map[store.DevIssue]int64, error) {
	s.pooled++
	return s.Store.OutcomeTokenTotals(ctx, outcomes, scope)
}

func (s *eraseDuringReadStore) ActualSpendAllWindow(ctx context.Context, since, until time.Time) (map[string]float64, error) {
	s.pooled++
	return s.Store.ActualSpendAllWindow(ctx, since, until)
}

func (s *eraseDuringReadStore) ActualSpendByPeriodWindow(ctx context.Context, since, until time.Time) ([]store.PeriodSpend, error) {
	s.pooled++
	return s.Store.ActualSpendByPeriodWindow(ctx, since, until)
}

func (s *eraseDuringReadStore) UnqualifiedExclusionWindow(ctx context.Context, since, until time.Time) (store.UnqualifiedExclusion, error) {
	s.pooled++
	return s.Store.UnqualifiedExclusionWindow(ctx, since, until)
}

func (s *eraseDuringReadStore) DeveloperAliases(ctx context.Context) (map[string]string, error) {
	s.pooled++
	return s.Store.DeveloperAliases(ctx)
}

func (s *eraseDuringReadStore) ReportDigests(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.Digest, store.Digest, error) {
	s.pooled++
	return s.Store.ReportDigests(ctx, since, until, scope)
}

func (s *eraseDuringReadStore) ReadSnapshot(ctx context.Context, fn func(*store.Snapshot) error) error {
	s.snapshots++
	err := s.Store.ReadSnapshot(ctx, func(snap *store.Snapshot) error {
		if _, err := snap.DeveloperCostsWindow(ctx, time.Time{}, time.Time{}, store.FleetWide); err != nil {
			return err
		}
		s.erase(ctx)
		return fn(snap)
	})
	s.released = true // the underlying deferred rollback has run
	return err
}

func eraseOnRead(t *testing.T, h *Handler, path string) *eraseDuringReadStore {
	t.Helper()
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	s := &eraseDuringReadStore{Store: h.store, t: t, raw: raw}
	h.store = s
	return s
}

func (s *eraseDuringReadStore) check(t *testing.T) {
	t.Helper()
	var remainingTokens, remainingOutcomes int
	if err := s.raw.QueryRow(`SELECT (SELECT COUNT(*) FROM token_events), (SELECT COUNT(*) FROM outcomes)`).Scan(&remainingTokens, &remainingOutcomes); err != nil {
		t.Fatal(err)
	}
	if s.fired != 1 || remainingTokens != 0 || remainingOutcomes != 0 {
		t.Fatalf("deletions did not commit: fired=%d tokens=%d outcomes=%d", s.fired, remainingTokens, remainingOutcomes)
	}
	if s.snapshots != 1 || s.pooled != 0 {
		t.Errorf("read used %d snapshots and %d pooled reads, want one snapshot only", s.snapshots, s.pooled)
	}
}

func TestAuditS03_1_ScoresShareOneSnapshot(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision} {
		for _, scope := range []string{"", "&repo=acme/beta"} {
			if mode.Anonymized() && scope != "" {
				continue // scoped reads are refused in anonymized modes
			}
			t.Run(mode.String()+scope, func(t *testing.T) {
				h, db, path := newDigestTestHandler(t)
				h.SetAggregation(mode, scoring.MinKAnonymity)
				registerTestStore(db, path)
				for _, dev := range []string{"alice", "bob", "carol"} {
					if err := baselineHierarchy(db, context.Background(), dev, "eng", "div", "org"); err != nil {
						t.Fatal(err)
					}
					seedRepoCostAt(t, db, "acme/beta", dev, dev+"-a-1", 10, winAInstant)
					seedRepoOutcomeAt(t, db, "acme/beta", dev, dev+"-a-1", 3, winAInstant)
				}
				s := eraseOnRead(t, h, path)
				code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-01-01&until=2026-02-01"+scope, nil)
				if code != http.StatusOK {
					t.Fatalf("status=%d body=%s", code, body)
				}
				var resp scoresResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatal(err)
				}
				s.check(t)
				if resp.Total == nil || resp.Total.TotalCostUSD != 30 || resp.Total.WeightedPoints != 9 {
					t.Errorf("torn scores total: %+v", resp.Total)
				}
				if !mode.Anonymized() && (resp.CostComposition == nil || resp.CostComposition.TotalCostUSD != 30) {
					t.Errorf("composition did not retain snapshot cost: %+v", resp.CostComposition)
				}
				if mode.Anonymized() && (len(resp.Teams) != 1 || resp.Teams[0].TotalCostUSD != 30 || resp.Teams[0].WeightedPoints != 9) {
					t.Errorf("group did not retain snapshot inputs: %+v", resp.Teams)
				}
			})
		}
	}
}

func TestAuditS04_2_ManifestWatermarksShareDigestSnapshot(t *testing.T) {
	for _, scope := range []string{"", "&repo=acme/beta"} {
		t.Run(scope, func(t *testing.T) {
			h, db, path := newDigestTestHandler(t)
			seedRepoCostAt(t, db, "acme/beta", "alice", "a-1", 10, winAInstant)
			seedRepoOutcomeAt(t, db, "acme/beta", "alice", "a-1", 3, winAInstant)
			s := eraseOnRead(t, h, path)
			m := decodeManifest(t, h, "/api/v1/report_manifest?since=2026-01-01&until=2026-02-01"+scope)
			s.check(t)
			_, rows := digestBlock(t, m, "events_digest")
			_, outcomeRows := digestBlock(t, m, "outcomes_digest")
			window := m["watermarks"].(map[string]any)["window"].(map[string]any)
			if rows != 1 || outcomeRows != 1 || window["token_event_count"] != float64(1) || window["outcome_count"] != float64(1) {
				t.Errorf("torn manifest: event rows=%v outcome rows=%v window=%v", rows, outcomeRows, window)
			}
		})
	}
}

func TestAuditS04_3_CompareWindowsShareOneSnapshot(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			h, db, path := newDigestTestHandler(t)
			h.SetAggregation(mode, scoring.MinKAnonymity)
			registerTestStore(db, path)
			for _, dev := range []string{"alice", "bob", "carol"} {
				if err := baselineHierarchy(db, context.Background(), dev, "eng", "div", "org"); err != nil {
					t.Fatal(err)
				}
				seedCostAt(t, db, dev, "a-1", 10, winAInstant)
				seedCostAt(t, db, dev, "b-1", 20, winBInstant)
				seedOutcomeAt(t, db, dev, "a-1", 3, 1, winAInstant)
				seedOutcomeAt(t, db, dev, "b-1", 6, 1, winBInstant)
			}
			s := eraseOnRead(t, h, path)
			code, resp := getCompare(t, h, compareURL())
			if code != http.StatusOK {
				t.Fatalf("status=%d", code)
			}
			s.check(t)
			if resp.Total == nil || resp.Total.A.TotalCostUSD != 30 || resp.Total.B.TotalCostUSD != 60 || resp.Total.DeltaTotalCostUSD != 30 || resp.Total.A.WeightedPoints != 9 || resp.Total.B.WeightedPoints != 18 || resp.Total.DeltaWeightedPoints != 9 {
				t.Errorf("torn comparison: %+v", resp.Total)
			}
			if mode.Anonymized() && (len(resp.Teams) != 1 || resp.Teams[0].A.TotalCostUSD != 30 || resp.Teams[0].B.TotalCostUSD != 60 || resp.Teams[0].A.WeightedPoints != 9 || resp.Teams[0].B.WeightedPoints != 18 || resp.Teams[0].DeltaWeightedPoints != 9) {
				t.Errorf("comparison groups did not retain snapshot inputs: %+v", resp.Teams)
			}
		})
	}
}

// The assembly hook runs before any live response rows or bootstrap CIs are
// built. A committed deletion leaves WAL frames that an open snapshot would
// pin, so a successful TRUNCATE checkpoint also proves the read mark is gone.
func TestScoresCompareSnapshotReleasedBeforeBootstrap(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision} {
		for _, endpoint := range []string{"scores", "scoped scores", "compare"} {
			if mode.Anonymized() && endpoint == "scoped scores" {
				continue // scoped reads are refused in anonymized modes
			}
			t.Run(mode.String()+"/"+endpoint, func(t *testing.T) {
				h, db, path := newDigestTestHandler(t)
				h.SetAggregation(mode, scoring.MinKAnonymity)
				registerTestStore(db, path)
				for _, dev := range []string{"alice", "bob", "carol"} {
					if err := baselineHierarchy(db, context.Background(), dev, "eng", "div", "org"); err != nil {
						t.Fatal(err)
					}
					for i := 0; i < scoring.MinRankedOutcomes; i++ {
						for j, ts := range []time.Time{winAInstant, winBInstant} {
							issue := fmt.Sprintf("%s-%d-%d", dev, j, i)
							seedRepoCostAt(t, db, "acme/beta", dev, issue, 10, ts)
							seedRepoOutcomeAt(t, db, "acme/beta", dev, issue, float64(i+1), ts)
						}
					}
				}
				s := eraseOnRead(t, h, path)
				if _, err := s.raw.Exec(`PRAGMA busy_timeout = 0`); err != nil {
					t.Fatal(err)
				}
				assemblies := 0
				h.beforeWindowAssembly = func() {
					assemblies++
					if s.snapshots != 1 || !s.released || s.fired != 1 {
						t.Fatalf("assembly started before snapshot release: snapshots=%d released=%v deletion=%d", s.snapshots, s.released, s.fired)
					}
					var busy, logFrames, checkpointed int
					if err := s.raw.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
						t.Fatal(err)
					}
					if busy != 0 || logFrames != 0 || checkpointed != 0 {
						t.Fatalf("snapshot still pins WAL at assembly: busy=%d frames=%d checkpointed=%d", busy, logFrames, checkpointed)
					}
				}
				url := "/api/v1/scores?since=2026-01-01&until=2026-02-01"
				if endpoint == "scoped scores" {
					url += "&repo=acme/beta&team=eng"
				}
				if endpoint == "compare" {
					code, resp := getCompare(t, h, compareURL())
					if code != http.StatusOK {
						t.Fatalf("status=%d", code)
					}
					if !mode.Anonymized() {
						if len(resp.Developers) != 3 {
							t.Fatalf("developers=%d, want 3", len(resp.Developers))
						}
						for _, row := range resp.Developers {
							if !row.A.Ranked || !row.B.Ranked || row.A.CILow <= 0 || row.B.CILow <= 0 {
								t.Fatalf("fixture did not run both bootstraps: %+v", row)
							}
						}
					}
				} else {
					code, body := doRequest(t, h, http.MethodGet, url, nil)
					if code != http.StatusOK {
						t.Fatalf("status=%d body=%s", code, body)
					}
					var resp scoresResponse
					if err := json.Unmarshal(body, &resp); err != nil {
						t.Fatal(err)
					}
					if !mode.Anonymized() {
						if len(resp.Developers) != 3 {
							t.Fatalf("developers=%d, want 3", len(resp.Developers))
						}
						for _, row := range resp.Developers {
							if !row.Ranked || row.CILow <= 0 {
								t.Fatalf("fixture did not run bootstrap: %+v", row)
							}
						}
					}
				}
				if assemblies != 1 {
					t.Fatalf("assembly hook ran %d times, want 1", assemblies)
				}
				s.check(t)
			})
		}
	}
}
