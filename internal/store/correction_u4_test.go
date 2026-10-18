package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCorrectionPolicy_EngineF2ScopedRefreshPreservesGlobalInvalidation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	original, _, _ := projectionFixture(t, db, true)
	unrelated := correctionOutcome(t, db, "unrelated", "other/repo", "unrelated-sha", 30)
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Simulate a v10 writer changing component A without refreshing it.
	if _, err := db.db.Exec(`UPDATE outcomes SET quality=0.4 WHERE id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CorrectionScoringPolicy(ctx); !errors.Is(err, ErrCorrectionPolicyUnresolved) {
		t.Fatal("control: older writer did not invalidate policy", err)
	}
	// A modern scoped write to B must not publish A's stale allocation.
	if err := db.UpdateQualityForOutcome(ctx, unrelated.ID, 0.7, "ci_fail", "unrelated-head"); err != nil {
		t.Fatal(err)
	}
	_, err := db.CorrectionScoringPolicy(ctx)
	if errors.Is(err, ErrCorrectionPolicyUnresolved) {
		return // Refusal also preserves the contract.
	}
	if err != nil {
		t.Fatal(err)
	}
	var points string
	if err := db.db.QueryRow(`SELECT allocated_points FROM correction_allocations WHERE outcome_id=?`, original.ID).Scan(&points); err != nil {
		t.Fatal(err)
	}
	if points != "2/1" {
		t.Fatalf("resolved policy served stale A after scoped B refresh: points=%s want=2/1", points)
	}
	t.Log("A refreshed to 2/1 before policy resolved")
}

func TestCorrectionPolicy_EngineF3CompletenessRefusal(t *testing.T) {
	for _, mutation := range []string{
		`DELETE FROM correction_allocations WHERE outcome_id=(SELECT MAX(id) FROM outcomes)`,
		`UPDATE correction_allocations SET rule_version='future-rule' WHERE outcome_id=(SELECT MAX(id) FROM outcomes)`,
	} {
		t.Run(mutation, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := t.Context()
			projectionFixture(t, db, true)
			if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := db.CorrectionScoringPolicy(ctx); !errors.Is(err, ErrCorrectionPolicyUnresolved) {
				t.Fatal("incomplete allocations accepted", err)
			}
			if err := db.ConfigureCorrectionScoring(ctx, false); err != nil {
				t.Fatal("legacy policy incorrectly requires allocations", err)
			}
		})
	}
}

func TestCorrectionConfirmation_DoesNotEnableScoring(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	before, err := db.EraseEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmCorrection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var enabled, rubric int
	var rule string
	if err := db.db.QueryRow(`SELECT scoring_enabled,rubric_version,rule_version FROM correction_policy`).Scan(&enabled, &rubric, &rule); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || rubric != 1 || rule != "legacy-v1" {
		t.Fatal(enabled, rubric, rule)
	}
	after, err := db.EraseEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("scoring-off confirmation invalidated a legacy seal")
	}
}

func TestCorrectionPolicy_AtomicFlipAndLegacyRestore(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	original, child, _ := projectionFixture(t, db, true)
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	p, err := db.CorrectionScoringPolicy(ctx)
	if err != nil || !p.Enabled || p.RubricVersion != 2 {
		t.Fatal(p, err)
	}
	rows, err := db.AllOutcomesWindow(ctx, original.Timestamp.Add(-24*time.Hour), child.Timestamp.Add(24*time.Hour), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Projection == nil || rows[0].Projection.AllocatedPoints != 3.25 {
		t.Fatal(rows)
	}
	if err := db.UpdateQualityForOutcome(ctx, child.ID, 0.7, "ci_fail", "head:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CorrectionScoringPolicy(ctx); err != nil {
		t.Fatal("writer left unresolved policy", err)
	}
	rows, err = db.AllOutcomesWindow(ctx, original.Timestamp.Add(-24*time.Hour), child.Timestamp.Add(24*time.Hour), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Projection.AllocatedPoints != 3.25 || rows[1].Projection.AllocatedPoints != 0.7 {
		t.Fatal("D1 quality cross-contamination", rows)
	}
	if err := db.ConfigureCorrectionScoring(ctx, false); err != nil {
		t.Fatal(err)
	}
	p, err = db.CorrectionScoringPolicy(ctx)
	if err != nil || p.Enabled || p.RubricVersion != 1 {
		t.Fatal(p, err)
	}
}
func TestCorrectionSeal_OlderWriterCannotBypassProtection(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	_, _, rel := projectionFixture(t, db, true)
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.EraseEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A v10 writer uses SQL without any knowledge of U4. Durable triggers make
	// its mutation invalidate construction and the resolved policy marker.
	if _, err := db.db.Exec(`UPDATE corrective_relations SET state='disputed',revision=revision+1 WHERE id=?`, rel); err != nil {
		t.Fatal(err)
	}
	after, err := db.EraseEpoch(ctx)
	if err != nil || after <= epoch {
		t.Fatal(after, epoch, err)
	}
	if _, err := db.CorrectionScoringPolicy(ctx); err == nil {
		t.Fatal("old writer silently left resolved scoring")
	}
	// The old seal INSERT omits all policy columns and cannot bypass protection.
	_, err = db.db.Exec(`INSERT INTO sealed_report(level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES('team','month','2026-01-01T00:00:00Z','2026-02-01T00:00:00Z',5,'c','2026-02-15T00:00:00Z','{}','d','old','old')`)
	if err == nil {
		t.Fatal("old writer sealed enabled policy without provenance")
	}
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatal(version, err)
	}
}
func TestCorrectionEvidence_ClosedCIOrRevertPredicate(t *testing.T) {
	push := "push"
	for _, kind := range []string{"ci_pass", "ci_fail", "ci_fail_flaky", "revert_quality", "revert_strategic"} {
		if !IsCIOrRevertEvidence(kind, &push) {
			t.Fatal(kind)
		}
	}
	for _, kind := range []string{"corrective_relations", "declared", "confirmed", "event3-v2", "revert_ambiguous", ""} {
		if IsCIOrRevertEvidence(kind, &push) {
			t.Fatal(kind)
		}
	}
}

func TestCorrectionPolicy_EraseFreezesWithoutRedistributing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	_, child, rel := projectionFixture(t, db, false)
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.db.QueryRow(`SELECT allocated_points FROM correction_allocations WHERE outcome_id=?`, child.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EraseDeveloper(ctx, "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CorrectionScoringPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	var after string
	var frozen bool
	if err := db.db.QueryRow(`SELECT allocated_points,frozen FROM correction_allocations WHERE outcome_id=?`, child.ID).Scan(&after, &frozen); err != nil {
		t.Fatal(err)
	}
	if !frozen || after != before {
		t.Fatal("erasure redistributed", before, after, frozen)
	}
}
