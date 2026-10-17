package api

import (
	"context"
	"database/sql"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	"reflect"
	"testing"
	"time"
)

func TestCorrectionManifest_PinsPolicyAndRuleVersion(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			h, db, _ := newDigestTestHandler(t)
			seedOutcome(t, db, "alice", "issue-1", 3, 1)
			outcomes, _, err := db.ListOutcomes(context.Background(), time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range outcomes {
				if err := db.ReconcileCorrectionChain(context.Background(), o.ID); err != nil {
					t.Fatal(err)
				}
			}
			h.scoringPolicy = scoring.ScoringPolicy{Event3: enabled, Resolved: true, RuleVersion: "event3-v2"}
			m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
			wantPolicy, wantRule, wantRubric := "legacy", "legacy-v1", float64(1)
			if enabled {
				wantPolicy, wantRule, wantRubric = "event3", "event3-v2", 2
			}
			if m["scoring_policy"] != wantPolicy || m["correction_rule_version"] != wantRule || m["rubric_version"] != wantRubric {
				t.Fatalf("policy not pinned: %v", m)
			}
			for _, duplicate := range []string{"correction_policy", "correction_revision"} {
				if _, ok := m[duplicate]; ok {
					t.Fatalf("duplicate provenance %s: %v", duplicate, m)
				}
			}
			ledgers := m["watermarks"].(map[string]any)["ledgers"].(map[string]any)
			state, err := db.CorrectionScoringPolicy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if ledgers["correction_projection_revision"] != float64(state.Revision) {
				t.Fatalf("revision not pinned: %v, stored %+v", ledgers, state)
			}
			if _, ok := m["corrections_digest"]; !ok {
				t.Fatal("corrective evidence not pinned")
			}
			counts, ok := m["outcome_counts"].(map[string]any)
			if !ok || counts["outcome_n"] != float64(1) || counts["sample_n"] != float64(1) {
				t.Fatalf("counts missing: %v", m)
			}
		})
	}
}

func TestCorrectionManifest_AnonymizedWithholdsCorrectiveCounts(t *testing.T) {
	h, _, _ := newDigestTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 3)
	m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	for _, k := range []string{"corrections_digest", "outcome_counts"} {
		if _, ok := m[k]; ok {
			t.Fatalf("unfloored %s disclosed", k)
		}
	}
}

// A writer commits after the snapshot's first read. Counts and corrective pins
// must still match the scoring inputs in that snapshot, not the live pool.
type correctionManifestRacingStore struct {
	Store
	mutate func()
	since  time.Time
}

func (s *correctionManifestRacingStore) ReadSnapshot(ctx context.Context, fn func(*store.Snapshot) error) error {
	return s.Store.ReadSnapshot(ctx, func(snap *store.Snapshot) error {
		if _, err := snap.AllOutcomesWindow(ctx, s.since, time.Time{}, store.FleetWide); err != nil {
			return err
		}
		s.mutate()
		return fn(snap)
	})
}
func TestCorrectionManifest_OneSnapshotCoversScoresAndEvidence(t *testing.T) {
	h, db, path := newDigestTestHandler(t)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	before := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	fired := false
	h.store = &correctionManifestRacingStore{Store: h.store, since: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), mutate: func() {
		fired = true
		if _, err := raw.Exec(`UPDATE outcomes SET additions=0,quality=0.7; UPDATE correction_policy SET projection_revision=projection_revision+1 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
	}}
	during := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	if !fired {
		t.Fatal("writer did not interleave")
	}
	if !reflect.DeepEqual(before, during) {
		t.Fatalf("mixed snapshots: before %v during %v", before, during)
	}
	h.store = db
	after := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	if reflect.DeepEqual(before["corrections_digest"], after["corrections_digest"]) {
		t.Fatal("mutation not visible after snapshot")
	}
}

// A stored-policy handler may retain stale startup options. The stamp and the
// raw/eligible denominators must both follow the same per-request stored state.
func TestCorrectionManifest_StoredPolicyControlsStampAndCounts(t *testing.T) {
	h, db := correctionReaderFixture(t)
	h.storedScoringPolicy = true
	h.scoringPolicy = scoring.ScoringPolicy{Event3: true} // deliberately unresolved
	for _, enabled := range []bool{false, true, false} {
		if err := db.ConfigureCorrectionScoring(t.Context(), enabled); err != nil {
			t.Fatal(err)
		}
		m := decodeManifest(t, h, "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01")
		policy, rule, rubric, sampleN := "legacy", "legacy-v1", float64(1), float64(2)
		if enabled {
			policy, rule, rubric, sampleN = "event3", "event3-v2", 2, 1
		}
		if m["scoring_policy"] != policy || m["correction_rule_version"] != rule || m["rubric_version"] != rubric || m["rubric"].(map[string]any)["version"] != rubric {
			t.Fatalf("stored policy not stamped: %v", m)
		}
		counts := m["outcome_counts"].(map[string]any)
		if counts["outcome_n"] != float64(2) || counts["sample_n"] != sampleN {
			t.Fatalf("counts used a different policy: %v", m)
		}
	}
}

func TestCorrectionManifest_StoredPolicySharesEvidenceSnapshot(t *testing.T) {
	h, db := correctionReaderFixture(t)
	h.storedScoringPolicy = true
	if err := db.ConfigureCorrectionScoring(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01"
	before := decodeManifest(t, h, path)
	fired := false
	h.store = &correctionManifestRacingStore{Store: h.store, since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), mutate: func() {
		fired = true
		if err := db.ConfigureCorrectionScoring(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	}}
	during := decodeManifest(t, h, path)
	if !fired || !reflect.DeepEqual(before, during) {
		t.Fatalf("stored policy escaped snapshot: fired=%t before=%v during=%v", fired, before, during)
	}
	h.store = db
	after := decodeManifest(t, h, path)
	if after["scoring_policy"] != "legacy" || after["outcome_counts"].(map[string]any)["sample_n"] != float64(2) {
		t.Fatalf("committed policy not visible: %v", after)
	}
}
