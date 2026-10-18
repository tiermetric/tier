package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/event3"
)

func projectionRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}
func projectionFixture(t *testing.T, db *DB, same bool) (Outcome, Outcome, int64) {
	t.Helper()
	o := correctionOutcome(t, db, "original", "acme/app", "original-sha", 10)
	f := correctionOutcome(t, db, "fixer", "acme/app", "fixer-sha", 20)
	developer := "fixer"
	if same {
		developer = "original"
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.db.Exec(`UPDATE outcomes SET weight=CASE pr_number WHEN 10 THEN 5 ELSE 1 END,quality=1,merged_at=CASE pr_number WHEN 10 THEN ? ELSE ? END,additions=10,deletions=0,changed_files=1,developer=CASE pr_number WHEN 10 THEN 'original' ELSE ? END`, at, at.Add(time.Hour), developer); err != nil {
		t.Fatal(err)
	}
	o.Weight = 5
	f.Weight = 1
	f.Developer = developer
	o.MergedAt = &at
	later := at.Add(time.Hour)
	f.MergedAt = &later
	a, b, c := 10, 0, 1
	o.SetDiffStats(&a, &b, &c)
	f.SetDiffStats(&a, &b, &c)
	recordCorrection(t, db, o, "Corrects: none", "original scope")
	recordCorrection(t, db, f, "Corrects: #10", "repair scope")
	var relation int64
	if err := db.db.QueryRow(`SELECT id FROM corrective_relations WHERE child_outcome_id=?`, f.ID).Scan(&relation); err != nil {
		t.Fatal(err)
	}
	return o, f, relation
}
func projectionReview() event3.Review {
	return event3.Review{Original: projectionRat("5"), Own: projectionRat("1"), Before: projectionRat("5"), After: projectionRat("5"), PureRepair: true, Scope: "original reviewed scope", Rationale: "reviewer says repair only", Reviewer: "operator-login", ReviewedAt: time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC), Revision: 1, ComparisonBase: "base-private-sha", ComparisonBefore: "before-private-sha", ComparisonAfter: "after-private-sha"}
}

func TestCorrectionProjection_ReviewBindsSelectedParent(t *testing.T) {
	for _, selectedReview := range []bool{false, true} {
		t.Run(map[bool]string{false: "only-ancestor-review", true: "selected-review-before-later-ancestor-review"}[selectedReview], func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			a, b, ab := projectionFixture(t, db, false)
			confirmFixture(t, db, ab)
			c := correctionOutcome(t, db, "carol", "acme/app", "carol-sha", 30)
			if _, err := db.db.Exec(`UPDATE outcomes SET weight=1,quality=1,merged_at=? WHERE id=?`, b.MergedAt.Add(time.Hour), c.ID); err != nil {
				t.Fatal(err)
			}
			recordCorrection(t, db, c, "Corrects: #10, #20", "C names both A and B")
			var ca, cb int64
			for parent, target := range map[int64]*int64{a.ID: &ca, b.ID: &cb} {
				if err := db.db.QueryRow(`SELECT id FROM corrective_relations WHERE child_outcome_id=? AND parent_outcome_id=?`, c.ID, parent).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if selectedReview {
				review := projectionReview()
				review.Original, review.Own = projectionRat("1"), projectionRat("1/2")
				review.Before, review.After, review.PureRepair = projectionRat("1"), projectionRat("3"), false
				if err := db.RecordCorrectionAssessment(ctx, cb, 1, review); err != nil {
					t.Fatal(err)
				}
			}
			// A later, higher-revision C->A review must never size C->B.
			if _, err := db.db.Exec(`UPDATE corrective_relations SET state='confirmed'; UPDATE corrective_relations SET revision=2 WHERE id=?`, ca); err != nil {
				t.Fatal(err)
			}
			wrong := projectionReview()
			wrong.Own, wrong.Revision = projectionRat("3"), 2
			if err := db.RecordCorrectionAssessment(ctx, ca, 2, wrong); err != nil {
				t.Fatal(err)
			}
			if err := db.ReconcileCorrectionChain(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			for _, row := range projectionRows(t, db) {
				if rowInt(row, "outcome_id") != c.ID {
					continue
				}
				if rowInt(row, "allocation_parent_outcome_id") != b.ID || rowInt(row, "relation_id") != cb {
					t.Fatal("did not select deepest parent B", row)
				}
				if !selectedReview {
					if rowString(row, "status") != "pending" || rowString(row, "rule_version") != event3.ProvisionalRule || rowString(row, "pending_reason") != "missing-sizing-or-parent-evidence" || rowInt(row, "provisional") != 1 || rowString(row, "allocated_points") != "1/1" {
						t.Fatal("C->A review enabled allocation against B without sizing evidence", row)
					}
				} else if rowString(row, "status") != "resolved" || rowString(row, "rule_version") != event3.ReviewedRule || rowInt(row, "provisional") != 0 || rowString(row, "reviewed_own_weight") != "1/2" || rowString(row, "before_weight") != "1/1" || rowString(row, "after_weight") != "3/1" || rowString(row, "marginal_credit") != "1/2" || rowString(row, "allocated_points") != "1/2" {
					t.Fatal("later C->A review replaced selected C->B sizing", row)
				}
				return
			}
			t.Fatal("missing C allocation")
		})
	}
}
func projectionRows(t *testing.T, db *DB) []map[string]any {
	t.Helper()
	rows, err := correctionRows(context.Background(), db.db, `SELECT * FROM correction_allocations ORDER BY id`, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func confirmFixture(t *testing.T, db *DB, relation int64) {
	t.Helper()
	if err := db.RecordCorrectionAssessment(context.Background(), relation, 1, projectionReview()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE corrective_relations SET state='confirmed' WHERE id=?`, relation); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectionProjection_PersistsProvisionalAndReplay(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{true: "same", false: "cross"}[same], func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			o, f, _ := projectionFixture(t, db, same)
			if err := db.ReconcileCorrectionChain(context.Background(), f.ID); err != nil {
				t.Fatal(err)
			}
			first := projectionRows(t, db)
			if len(first) != 2 {
				t.Fatal(first)
			}
			want := "5/1"
			if same {
				want = "13/4"
			}
			if first[0]["allocated_points"] != want || first[1]["allocated_points"] != "1/1" || first[1]["ranking_eligibility"] != "ineligible" {
				t.Fatal(first)
			}
			if err := db.ReconcileCorrectionChain(context.Background(), o.ID); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, projectionRows(t, db)) {
				t.Fatal("replay changed allocation bytes")
			}
			exp, err := db.ExportDeveloper(context.Background(), "original")
			if err != nil {
				t.Fatal(err)
			}
			if len(exp.CorrectionAllocations) != 2 || len(exp.CorrectionAssessments) != 1 {
				t.Fatalf("projection export: %+v", exp)
			}
		})
	}
}
func TestCorrectionAssessment_ReviewedValidationAndReplay(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, f, relation := projectionFixture(t, db, false)
	ctx := context.Background()
	good := projectionReview()
	invalid := good
	invalid.Own = nil
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, invalid); err == nil {
		t.Fatal("missing reviewed own accepted")
	}
	if err := db.RecordCorrectionAssessment(ctx, relation, 2, good); err == nil {
		t.Fatal("stale review accepted")
	}
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, good); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, good); err != nil {
		t.Fatal("identical replay", err)
	}
	changed := good
	changed.Reviewer = "impostor"
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, changed); err == nil {
		t.Fatal("review provenance replaced")
	}
	if _, err := db.db.Exec(`UPDATE corrective_relations SET state='confirmed'; UPDATE outcomes SET weight=8 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	rows := projectionRows(t, db)
	if rows[0]["allocated_points"] != "13/4" || rows[1]["allocated_points"] != "1/1" || rows[1]["raw_weight"] != "8/1" || rows[1]["reviewed_own_weight"] != "1/1" || rows[1]["rule_version"] != "event3-v2" {
		t.Fatal(rows)
	}
}
func TestCorrectionProjection_DisputeRetentionAndQualityIsolation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o, f, r := projectionFixture(t, db, false)
	confirmFixture(t, db, r)
	ctx := context.Background()
	if err := db.ReconcileCorrectionChain(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	before := projectionRows(t, db)
	for _, state := range []string{"disputed", "withdrawn-confirmed"} {
		if _, err := db.db.Exec(`UPDATE corrective_relations SET state=?`, state); err != nil {
			t.Fatal(err)
		}
		if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
			t.Fatal(err)
		}
		after := projectionRows(t, db)
		if !reflect.DeepEqual(before[0], after[0]) || before[1]["allocated_points"] != after[1]["allocated_points"] || after[1]["disputed"] != int64(1) {
			t.Fatal(after)
		}
	}
	if _, err := db.db.Exec(`UPDATE outcomes SET quality=0.7 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	after := projectionRows(t, db)
	if !reflect.DeepEqual(before[0], after[0]) || after[1]["allocated_points"] != "7/10" {
		t.Fatal(after)
	}
}
func TestCorrectionProjectionErase_FrozenSurvivorAndEveryIdentifier(t *testing.T) {
	for _, subject := range []string{"original", "fixer", "operator-login"} {
		t.Run(subject, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			o, f, r := projectionFixture(t, db, false)
			confirmFixture(t, db, r)
			ctx := context.Background()
			if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
				t.Fatal(err)
			}
			before := projectionRows(t, db)
			exp, err := db.ExportDeveloper(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			wantExport := map[string]int{"original": 2, "fixer": 1, "operator-login": 0}[subject]
			if len(exp.CorrectionAllocations) != wantExport || len(exp.CorrectionAssessments) < 1 {
				t.Fatal(exp)
			}
			counts, err := db.EraseDeveloper(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			if subject == "operator-login" {
				if counts["correction_allocations"] != 0 || !reflect.DeepEqual(before, projectionRows(t, db)) {
					t.Fatal("reviewer froze allocations", counts)
				}
				assertCount(t, db, `SELECT COUNT(*) FROM correction_assessments WHERE reviewer IS NOT NULL`, 0)
				if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
					t.Fatal(err)
				}
				return
			}
			if counts["correction_allocations"] != 2 || counts["correction_assessments"] < 1 {
				t.Fatal(counts)
			}
			rows := projectionRows(t, db)
			wantSurvivors := 1
			if len(rows) != wantSurvivors {
				t.Fatal(rows)
			}
			for _, row := range rows {
				var prior map[string]any
				for _, candidate := range before {
					if candidate["outcome_id"] == row["outcome_id"] {
						prior = candidate
					}
				}
				if prior == nil || row["outcome_id"] == nil {
					t.Fatal("subject row retained", row)
				}
				for _, key := range []string{"share", "projected_quality", "allocated_points", "marginal_credit", "original_term"} {
					if prior[key] != row[key] {
						t.Fatalf("survivor numeric %s changed", key)
					}
				}
				if row["relation_id"] != nil || row["frozen"] != int64(1) {
					t.Fatal(row)
				}
			}
			assessments, err := correctionRows(ctx, db.db, `SELECT * FROM correction_assessments`, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range assessments {
				for _, key := range []string{"relation_id", "comparison_base", "comparison_before", "comparison_after", "scope", "rationale", "reviewer", "reviewed_at"} {
					if row[key] != nil {
						t.Fatalf("%s survived", key)
					}
				}
			}
			survivor := o.ID
			if subject == "original" {
				survivor = f.ID
			}
			if err := db.ReconcileCorrectionChain(ctx, survivor); err != nil {
				t.Fatalf("frozen reconcile %v", err)
			}
			if err := db.RecordCorrectionAssessment(ctx, r, 1, projectionReview()); err == nil {
				t.Fatal("erased assessment upgraded")
			}
			after, err := db.ExportDeveloper(ctx, subject)
			if err != nil || after.RowCount() != 0 {
				t.Fatalf("erased export: %d %v", after.RowCount(), err)
			}
		})
	}
}
func TestCorrectionProjection_AtomicConcurrentReplay(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o, f, _ := projectionFixture(t, db, true)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Go(func() { errs <- db.ReconcileCorrectionChain(context.Background(), f.ID) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(projectionRows(t, db)) != 2 {
		t.Fatal("duplicate projections")
	}
	var rev int
	if err := db.db.QueryRow(`SELECT projection_revision FROM correction_policy`).Scan(&rev); err != nil || rev != 1 {
		t.Fatal(rev, err)
	}
	// An encoding failure rolls back the whole chain and revision together.
	before := projectionRows(t, db)
	if _, err := db.db.Exec(`UPDATE outcomes SET quality=-1 WHERE id=?`, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(context.Background(), o.ID); err == nil {
		t.Fatal("negative input accepted")
	}
	if !reflect.DeepEqual(before, projectionRows(t, db)) {
		t.Fatal("partial projection committed")
	}
}
func TestCorrectionSchemaU1_MigrateReopenAndSealedBytes(t *testing.T) {
	for _, version := range []int{8, 9} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "previous.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			// A historical v8/v9 database has no U4 guards. Remove them before
			// reconstructing that schema, including triggers on other tables.
			guardRows, err := db.db.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'trg_u4_%'`)
			if err != nil {
				t.Fatal(err)
			}
			var guards []string
			for guardRows.Next() {
				var name string
				if err = guardRows.Scan(&name); err != nil {
					t.Fatal(err)
				}
				guards = append(guards, name)
			}
			if err = guardRows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = guardRows.Close()
			for _, name := range guards {
				if _, err = db.db.Exec(`DROP TRIGGER ` + name); err != nil {
					t.Fatal(err)
				}
			}
			// Exercise actual ALTER migration from the previous sealed table layout.
			body := []byte(`{"old":"sealed bytes remain exact"}`)
			if _, err = db.db.Exec(`INSERT INTO sealed_report(level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES('team','month','2026-01-01T00:00:00Z','2026-02-01T00:00:00Z',3,'old-config','2026-02-02T00:00:00Z',?,'old-digest','old-tool','old-commit')`, body); err != nil {
				t.Fatal(err)
			}
			for _, col := range []string{"scoring_policy", "correction_rule_version", "rubric_version", "correction_revision"} {
				if _, err = db.db.Exec(`ALTER TABLE sealed_report DROP COLUMN ` + col); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = db.db.Exec(`ALTER TABLE sealed_rollup DROP COLUMN outcome_n; DROP TABLE correction_assessments; DROP TABLE correction_allocations; DROP TABLE correction_policy`); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			setUserVersion(t, path, version)
			before := dumpSchema(t, path)
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			wantNotice := UpgradeNoticeCorrectionProjection + "\n" + UpgradeNoticeQualityTriggers + "\n" + UpgradeNoticeIssueRule
			if version < 9 {
				wantNotice = UpgradeNoticePricingObservations + "\n" + wantNotice
			}
			if db.UpgradeNotice() != wantNotice {
				t.Fatal(db.UpgradeNotice())
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readUserVersion(t, path); got != schemaVersion {
				t.Fatalf("user_version = %d, want %d", got, schemaVersion)
			}
			after := dumpSchema(t, path)
			if before == after {
				t.Fatal("migration did not run")
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if db.UpgradeNotice() != "" {
				t.Fatal("repeat notice")
			}
			if dumpSchema(t, path) != after {
				t.Fatal("reopen not idempotent")
			}
			var stored []byte
			var digest string
			var policy any
			if err = db.db.QueryRow(`SELECT body,body_digest,scoring_policy FROM sealed_report`).Scan(&stored, &digest, &policy); err != nil {
				t.Fatal(err)
			}
			if string(stored) != string(body) || digest != "old-digest" || policy != nil {
				t.Fatal("old seal mutated")
			}
			var enabled, rubric int
			var rule string
			if err = db.db.QueryRow(`SELECT scoring_enabled,rubric_version,rule_version FROM correction_policy`).Scan(&enabled, &rubric, &rule); err != nil || enabled != 0 || rubric != 1 || rule != "legacy-v1" {
				t.Fatal(enabled, rubric, rule, err)
			}
		})
	}
}
func TestCorrectionProjectionPrivacy_Classified(t *testing.T) {
	b, err := os.ReadFile("../../docs/privacy.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"correction_assessments", "correction_allocations", "comparison", "reviewer", "frozen"} {
		if !strings.Contains(string(b), word) {
			t.Errorf("privacy inventory missing %s", word)
		}
	}
	// Pin the additive DSAR contract independently of the older three tables.
	b, err = json.Marshal(DeveloperExport{})
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"correction_assessments", "correction_allocations"} {
		if !strings.Contains(string(b), word) {
			t.Fatal(word)
		}
	}
}

func TestCorrectionProjectionErase_ExcludedOutcomeAndAliasReviewer(t *testing.T) {
	for _, who := range []string{"original", "fixer", "reviewer-alias"} {
		t.Run(who, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			o, f, rel := projectionFixture(t, db, false)
			confirmFixture(t, db, rel)
			ctx := context.Background()
			if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
				t.Fatal(err)
			}
			if who == "reviewer-alias" {
				if err := db.UpsertDeveloperAlias(ctx, "operator-login", "reviewer-alias", "test"); err != nil {
					t.Fatal(err)
				}
			} else {
				excluded := o
				if who == "fixer" {
					excluded = f
				}
				if err := db.ExcludePROutcome(ctx, excluded, "trusted revert evidence", "test"); err != nil {
					t.Fatal(err)
				}
			}
			exp, err := db.ExportDeveloper(ctx, who)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int{"original": 2, "fixer": 1, "reviewer-alias": 0}[who]
			if len(exp.CorrectionAllocations) != want {
				t.Fatalf("excluded or alias direct export: %+v", exp.CorrectionAllocations)
			}
			if _, err := db.EraseDeveloper(ctx, who); err != nil {
				t.Fatal(err)
			}
			if who == "reviewer-alias" {
				assertCount(t, db, `SELECT COUNT(*) FROM correction_allocations WHERE frozen=1`, 0)
				assertCount(t, db, `SELECT COUNT(*) FROM correction_assessments WHERE reviewer IS NOT NULL`, 0)
				exp, err = db.ExportDeveloper(ctx, who)
				if err != nil || exp.RowCount() != 0 {
					t.Fatal(exp.RowCount(), err)
				}
				return
			}
			for _, row := range projectionRows(t, db) {
				if row["frozen"] != int64(1) || row["allocation_parent_outcome_id"] != nil {
					t.Fatal("unfrozen link", row)
				}
				if who != "reviewer-alias" && row["outcome_id"] == map[string]int64{"original": o.ID, "fixer": f.ID}[who] {
					t.Fatal("excluded subject link survived")
				}
			}
			exp, err = db.ExportDeveloper(ctx, who)
			if err != nil || exp.RowCount() != 0 {
				t.Fatal(exp.RowCount(), err)
			}
		})
	}
}

func TestCorrectionProjection_AliasTrustAndAssessmentInputs(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o, f, _ := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.UpsertDeveloperAlias(ctx, "fixer", "original", "test"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	first := projectionRows(t, db)
	if first[0]["allocated_points"] != "13/4" {
		t.Fatal("same person aliases treated as foreign", first)
	}
	rows, err := correctionRows(ctx, db.db, `SELECT * FROM correction_assessments WHERE basis='provisional'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["original_weight"] != "5/1" || rows[0]["original_quality"] != "17/20" {
		t.Fatal("assessment recorded child in original fields", rows)
	}
	if _, err := db.db.Exec(`UPDATE outcomes SET quality=0.7 WHERE id=?`, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = correctionRows(ctx, db.db, `SELECT * FROM correction_assessments WHERE basis='provisional' ORDER BY chain_version`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1]["original_quality"] != "11/20" {
		t.Fatal("updated assessment inputs not retained atomically", rows)
	}
}
