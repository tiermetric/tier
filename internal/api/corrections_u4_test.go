package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"fmt"
	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/event3"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	"math/big"
)

func u4Diagnostics(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	w := correctionGet(t, h, "/api/v1/correction_diagnostics?since=2026-10-01&until=2026-11-01")
	if w.Code != 200 {
		t.Fatalf("diagnostics: %d %s", w.Code, w.Body)
	}
	var b map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}
func TestCorrectionDiagnostics_PendingBlocksCompleteCoverage(t *testing.T) {
	h, db := correctionReaderFixture(t)
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE corrective_relations SET state='pending', resolution_reason='missing original'; UPDATE correction_allocations SET status='pending',pending_reason='missing original' WHERE role!='original'`); err != nil {
		t.Fatal(err)
	}
	b := u4Diagnostics(t, h)
	if b["declaration_coverage"] != "incomplete" || b["history_coverage"] != "not_established" || b["captured_spend_fidelity"] != "reported_separately" {
		t.Fatal(b)
	}
	if !strings.Contains(string(mustU4JSON(t, b)), `"status":"pending"`) {
		t.Fatal(b)
	}
}
func TestCorrectionDeclarations_UnadjudicatedAfterThirtyDays(t *testing.T) {
	h, db := correctionReaderFixture(t)
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE corrective_relations SET recorded_at=?`, time.Now().UTC().Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	b := u4Diagnostics(t, h)
	if !strings.Contains(string(mustU4JSON(t, b)), `"escalation":"unadjudicated_30_days"`) {
		t.Fatal(b)
	}
}
func mustU4JSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRepairSpend_CountsEachTokenEventOnce(t *testing.T) {
	h, db := correctionReaderFixture(t)
	raw := rawSealStore(t, db)
	// One token event supports two declared repairs; it must be counted once.
	if _, err := raw.Exec(`INSERT INTO corrective_relations(child_outcome_id,child_developer,child_repo,child_pr_number,parent_repo,parent_pr_number,state,revision,recorded_at,updated_at,rule_version) SELECT child_outcome_id,child_developer,child_repo,child_pr_number,parent_repo,733,'pending',1,recorded_at,updated_at,rule_version FROM corrective_relations`); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: "acme/app", IssueID: "repair", CostMicro: 7000000, Timestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	w := correctionGet(t, h, "/api/v1/repair_spend?since=2026-10-01&until=2026-11-01")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var b map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if b["token_events"] != float64(1) || b["cost_micro"] != float64(7000000) {
		t.Fatal(b)
	}
}
func TestRepairSpend_DisclosesPooledAndPartialAttribution(t *testing.T) {
	h, db := correctionReaderFixture(t)
	for _, pr := range []*int{nil, func() *int { v := 2; return &v }()} {
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: "acme/app", IssueID: "repair", PRNumber: pr, CostMicro: 3000000, Timestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	w := correctionGet(t, h, "/api/v1/repair_spend?since=2026-10-01&until=2026-11-01")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, want := range []string{`"pooled_issue_cost_micro":3000000`, `"supported_pr_cost_micro":3000000`, `"attribution":"partial"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatal(w.Body.String())
		}
	}
}
func TestCorrectionEvidence_NotCountedAsCIOrRevertEvidence(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	b := u4Diagnostics(t, h)
	if b["ci_or_revert_evidence"] != false {
		t.Fatal(b)
	}
}
func TestCorrectionDiagnostics_TeamAndDivisionNotShown(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	for _, level := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h.SetAggregation(level, 5)
		for _, path := range []string{"/api/v1/correction_diagnostics", "/api/v1/repair_spend"} {
			w := correctionGet(t, h, path)
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"status":"not_shown"}` {
				t.Fatal(w.Code, w.Body.String())
			}
		}
	}
}
func u4SealProvenance(t *testing.T, db *store.DB) {
	t.Helper()
	raw := rawSealStore(t, db)
	var policy, rule *string
	var rubric *int
	if err := raw.QueryRow(`SELECT scoring_policy,correction_rule_version,rubric_version FROM sealed_report LIMIT 1`).Scan(&policy, &rule, &rubric); err != nil {
		t.Fatal(err)
	}
	if policy == nil || rule == nil || rubric == nil {
		t.Fatal("new seal lacks pinned policy provenance")
	}
	if *policy != "legacy" || *rubric != 1 || *rule != "legacy-v1" {
		t.Fatal(*policy, *rule, *rubric)
	}
}
func TestCorrectionSeal_QualityChangePreservesSealedBytes(t *testing.T) {
	u4RealChainSealPreservation(t, "quality")
	_, db, s := newSealFixture(t)
	ctx := context.Background()
	before, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := db.SealedReport(ctx, "month", sealMay.Start)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE outcomes SET quality=0.7 WHERE developer='b2'`); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := db.SealedReport(ctx, "month", sealMay.Start)
	if !bytes.Equal(before, after) || got.BodyDigest != rep.BodyDigest {
		t.Fatal("sealed bytes changed")
	}
	u4SealProvenance(t, db)
}
func TestCorrectionSeal_CreditChangePreservesSealedBytes(t *testing.T) {
	u4RealChainSealPreservation(t, "credit")
	_, db, s := newSealFixture(t)
	ctx := context.Background()
	before, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := db.SealedReport(ctx, "month", sealMay.Start)
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE correction_policy SET projection_revision=projection_revision+1`); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := db.SealedReport(ctx, "month", sealMay.Start)
	if !bytes.Equal(before, after) || got.BodyDigest != rep.BodyDigest {
		t.Fatal("sealed bytes changed")
	}
	u4SealProvenance(t, db)
}
func TestCorrectionSeal_ConcurrentProjectionChangeRetries(t *testing.T) {
	u4RealChainSealPreservation(t, "race")
	h, db, s := newSealFixture(t)
	h.storedScoringPolicy = true
	if err := db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	raw := rawSealStore(t, db)
	calls := 0
	s.beforeSeal = func() {
		calls++
		if calls == 1 {
			if _, err := raw.Exec(`UPDATE correction_policy SET projection_revision=projection_revision+1`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := s.sealOrLoad(context.Background(), sealMay); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("computed %d times, want retry after projection change", calls)
	}
}
func TestCorrectionLateEvidence_DisclosesAffectedOriginalAndChildPeriods(t *testing.T) {
	u4RealChainSealPreservation(t, "credit")
	h, db := correctionReaderFixture(t)
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE corrective_relations SET parent_merged_at=?,child_merged_at=?`, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	b := u4Diagnostics(t, h)
	s := string(mustU4JSON(t, b))
	if !strings.Contains(s, `"original_period":"2026-09"`) || !strings.Contains(s, `"child_period":"2026-10"`) {
		t.Fatal(s)
	}
}

func TestCorrectionStoredPolicy_AllReadersAndLegacyBytes(t *testing.T) {
	h, db := correctionReaderFixture(t)
	h.scoringPolicy = scoring.ScoringPolicy{}
	h.storedScoringPolicy = true
	ctx := context.Background()
	path := "/api/v1/scores?since=2026-10-01&until=2026-11-01"
	before := correctionGet(t, h, path).Body.String()
	exportPath := "/api/v1/outcomes?since=2026-10-01&until=2026-11-01"
	legacyExports := make(map[string]string)
	for _, path := range []string{exportPath, exportPath + "&csv=true"} {
		w := correctionGet(t, h, path)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		legacyExports[path] = w.Body.String()
	}
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{path, "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01", "/api/v1/scores/alice?since=2026-10-01&until=2026-11-01", "/api/v1/outcomes?since=2026-10-01&until=2026-11-01"} {
		w := correctionGet(t, h, path)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(path, "/outcomes") {
			if !strings.Contains(w.Body.String(), `"allocated_points":`) || !strings.Contains(w.Body.String(), `"disputed":`) {
				t.Fatal(w.Body.String())
			}
		} else if !strings.Contains(w.Body.String(), `"rubric_version":2`) {
			t.Fatal(w.Body.String())
		}
		if strings.Contains(path, "/report_manifest") && !strings.Contains(w.Body.String(), `"version":2`) {
			t.Fatal(w.Body.String())
		}
	}
	for path := range legacyExports {
		w := correctionGet(t, h, path)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(path, "csv=true") {
			rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 || len(rows[0]) != len(outcomesCSVHeader) || rows[0][18] != "raw_additions" || rows[1][18] != "10" || rows[1][21] != "true" {
				t.Fatal(rows)
			}
		} else if !strings.Contains(w.Body.String(), `"raw_additions":10`) || !strings.Contains(w.Body.String(), `"diff_stats_available":true`) {
			t.Fatal(w.Body.String())
		}
	}
	// A stale, unresolved handler policy must not override persisted state or
	// reject the manifest before its snapshot reads the resolved stored policy.
	h.scoringPolicy = scoring.ScoringPolicy{Event3: true}
	if w := correctionGet(t, h, "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01"); w.Code != 200 || !strings.Contains(w.Body.String(), `"rubric_version":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	got := correctionGet(t, h, path)
	var body struct {
		Rubric           rubricJSON                   `json:"rubric"`
		CorrectionPolicy *scoring.CorrectionRuleStamp `json:"correction_policy"`
		Developers       []struct {
			WeightedPoints float64 `json:"weighted_points"`
			SampleN        int     `json:"sample_n"`
			OutcomeN       int     `json:"outcome_n"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/correction_v2_scores.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(golden), mustU4JSON(t, body)) {
		t.Fatalf("v2 golden: %s", mustU4JSON(t, body))
	}
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE corrective_relations SET state='pending'`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{path, "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01"} {
		if w := correctionGet(t, h, path); w.Code != 500 {
			t.Fatal("unresolved served", w.Code, w.Body.String())
		}
	}
	if err := db.ConfigureCorrectionScoring(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := correctionGet(t, h, path).Body.String(); got != before {
		t.Fatal("flag off did not restore legacy bytes")
	}
	for path, want := range legacyExports {
		if w := correctionGet(t, h, path); w.Code != 200 || w.Body.String() != want {
			t.Fatal("flag off did not restore legacy export bytes", w.Code, w.Body.String())
		}
	}
}
func TestCorrectionSeal_EnabledPolicyAndLegacyProvenance(t *testing.T) {
	h, db, s := newSealFixture(t)
	h.storedScoringPolicy = true
	if err := db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.sealOrLoad(context.Background(), sealMay)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"version":2`) {
		t.Fatal(string(body))
	}
	rep, err := db.SealedReport(context.Background(), "month", sealMay.Start)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RubricVersion == nil || *rep.RubricVersion != 2 || rep.ScoringPolicy == nil || *rep.ScoringPolicy != "event3" {
		t.Fatal(rep)
	}
	rollups, persons, err := db.SealedFoldInputs(context.Background(), rep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefoldSealed(rep, rollups, persons); err != nil {
		t.Fatal(err)
	}
	if d := sealDisclosureOf(store.SealedReport{}); d.ScoringPolicy != "unknown" || d.RubricVersion != nil {
		t.Fatal(d)
	}
}

// #733/#736-style same-author repair with the original and child in separate
// periods: window filtering must not sever the chain.
func u4RealChainFixture(t *testing.T) (*Handler, *store.DB, *sealer, int64, int64, int64) {
	t.Helper()
	h, db, s := newSealFixture(t)
	ctx := context.Background()
	var ids []int64
	for i, pr := range []int{733, 736} {
		at := sealMay.Start.AddDate(0, i, 1)
		o := store.Outcome{Developer: "b2", Repo: repoAlpha, PRNumber: pr, IssueID: "u4-repair", Weight: 5, Quality: 1, Timestamp: at, MergedAt: &at, MergeCommitSHA: fmt.Sprintf("u4-pr-%d", pr)}
		body := "Corrects: none"
		if i == 1 {
			o.Weight = 1
			body = "Corrects: #733"
		}
		a, b, c := 10, 0, 1
		o.SetDiffStats(&a, &b, &c)
		if _, err := db.RecordPROutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "repair evidence", "fixture"); err != nil {
			t.Fatal(err)
		}
		stored, ok, err := db.OutcomeByMergeCommit(ctx, o.MergeCommitSHA)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		ids = append(ids, stored.ID)
	}
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	h.storedScoringPolicy = true
	var rel int64
	if err := rawSealStore(t, db).QueryRow(`SELECT id FROM corrective_relations WHERE child_pr_number=736`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	return h, db, s, ids[0], ids[1], rel
}
func u4RealChainSealPreservation(t *testing.T, change string) {
	t.Helper()
	h, db, s, original, _, rel := u4RealChainFixture(t)
	ctx := context.Background()
	before, _ := foldedMayBody(t, h)
	mutate := func() {
		if change == "quality" {
			if err := db.UpdateQualityForOutcome(ctx, original, 0.7, "ci_fail", "head:1"); err != nil {
				t.Fatal(err)
			}
			return
		}
		r := event3.Review{Revision: 1, Original: big.NewRat(5, 1), Own: big.NewRat(1, 1), Before: big.NewRat(1, 1), After: big.NewRat(3, 1), Scope: "reviewed original scope", Rationale: "repair and growth", Reviewer: "operator-login", ReviewedAt: sealNow, ComparisonBase: "base", ComparisonBefore: "before", ComparisonAfter: "after"}
		if err := db.ConfirmCorrection(ctx, store.CorrectionCommand{RelationID: rel, Revision: 1, Actor: r.Reviewer, Role: "operator", Review: &r}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	if change == "race" {
		s.beforeSeal = func() {
			calls++
			if calls == 1 {
				mutate()
			}
		}
	}
	sealed, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := db.SealedReport(ctx, "month", sealMay.Start)
	if err != nil {
		t.Fatal(err)
	}
	if change != "race" {
		mutate()
	}
	after, _ := foldedMayBody(t, h)
	if bytes.Equal(before, after) {
		t.Fatal("control: mutation did not change live original-period body")
	}
	if change == "race" {
		if calls != 2 || !bytes.Equal(sealed, after) {
			t.Fatal("mixed projection sealed instead of retried", calls)
		}
	} else {
		got, _, err := s.sealOrLoad(ctx, sealMay)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := db.SealedReport(ctx, "month", sealMay.Start)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, sealed) || stored.BodyDigest != rep.BodyDigest || !bytes.Equal(got, before) {
			t.Fatal("prior seal changed")
		}
	}
	h.SetAggregation(scoring.AggregationDeveloper, 0)
	w := correctionGet(t, h, "/api/v1/correction_diagnostics?since=2026-06-01&until=2026-07-01")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"original_period":"2026-05"`) || !strings.Contains(w.Body.String(), `"child_period":"2026-06"`) || !strings.Contains(w.Body.String(), `"original_report":"sealed_preserved"`) || !strings.Contains(w.Body.String(), `"child_report":"live"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCorrectionDiagnostics_ProvisionalSizingAndRecordedReceipt(t *testing.T) {
	h, db := correctionReaderFixture(t)
	b := u4Diagnostics(t, h)
	for _, want := range []string{`"status":"provisional"`, `"scoring_status":"recorded, not yet scored"`, `"original_raw_weight":"5/1"`, `"original_credited_weight":"5/1"`, `"original_allocated_points":"13/4"`} {
		if !strings.Contains(string(mustU4JSON(t, b)), want) {
			t.Fatal(want, b)
		}
	}
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE corrective_relations SET state='confirmed'`); err != nil {
		t.Fatal(err)
	}
	b = u4Diagnostics(t, h)
	if !strings.Contains(string(mustU4JSON(t, b)), `"status":"confirmed_with_provisional_sizing"`) {
		t.Fatal(b)
	}
	if _, err := raw.Exec(`UPDATE corrective_relations SET state='excluded'`); err != nil {
		t.Fatal(err)
	}
	b = u4Diagnostics(t, h)
	if !strings.Contains(string(mustU4JSON(t, b)), `"status":"excluded"`) || b["declaration_coverage"] != "incomplete" {
		t.Fatal(b)
	}
}
