package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestCorrectionReview_R1OpenWindow(t *testing.T) {
	h, db := correctionReaderFixture(t)
	pr := 2
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: "acme/app", PRNumber: &pr, CostMicro: 123, Timestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"correction_diagnostics", "repair_spend"} {
		t.Run(endpoint, func(t *testing.T) {
			open := correctionGet(t, h, "/api/v1/"+endpoint+"?since=2026-10-01")
			bounded := correctionGet(t, h, "/api/v1/"+endpoint+"?since=2026-10-01&until=2026-11-01")
			if open.Code != 200 || bounded.Code != 200 || !bytes.Equal(open.Body.Bytes(), bounded.Body.Bytes()) {
				t.Fatalf("open %d %s; bounded %d %s", open.Code, open.Body, bounded.Code, bounded.Body)
			}
		})
	}
	w := correctionGet(t, h, "/api/v1/scores?since=2026-10-01")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"relation_id":1`) {
		t.Fatal("scores open window omitted dispute context", w.Code, w.Body.String())
	}

}

func TestCorrectionReview_R3ScoresClockStable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, db := correctionReaderFixture(t)
		if _, err := rawSealStore(t, db).Exec(`UPDATE corrective_relations SET recorded_at=?`, time.Now().Add(-30*24*time.Hour+time.Second)); err != nil {
			t.Fatal(err)
		}
		path := "/api/v1/scores?since=2026-10-01&until=2026-11-01"
		before := correctionGet(t, h, path)
		d1 := u4Diagnostics(t, h)
		time.Sleep(2 * time.Second)
		after := correctionGet(t, h, path)
		d2 := u4Diagnostics(t, h)
		if bytes.Equal(mustU4JSON(t, d1), mustU4JSON(t, d2)) {
			t.Fatal("control: diagnostics did not cross escalation boundary")
		}
		if before.Code != 200 || after.Code != 200 || !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
			t.Fatalf("clock changed scores: before %s after %s", before.Body, after.Body)
		}
	})
}

func TestCorrectionReview_R8StoredExport(t *testing.T) {
	h, db := correctionReaderFixture(t)
	h.storedScoringPolicy = true
	h.scoringPolicy = scoring.ScoringPolicy{}
	path := "/api/v1/outcomes?since=2026-10-01&until=2026-11-01"
	before := correctionGet(t, h, path).Body.String()
	if err := db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "&csv=true"} {
		w := correctionGet(t, h, path+suffix)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "disputed") || !strings.Contains(w.Body.String(), "allocated_points") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if err := db.ConfigureCorrectionScoring(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := correctionGet(t, h, path).Body.String(); got != before {
		t.Fatal("legacy export changed")
	}
}

func TestCorrectionReview_R9AnonymousManifestRevision(t *testing.T) {
	h, db := correctionReaderFixture(t)
	h.storedScoringPolicy = true
	if err := db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h.SetAggregation(mode, 5)
		for i := 0; i < 2; i++ {
			w := correctionGet(t, h, "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01")
			if w.Code != 200 || strings.Contains(w.Body.String(), "correction_revision") || strings.Contains(w.Body.String(), "correction_projection_revision") {
				t.Fatal(mode, i, w.Code, w.Body.String())
			}
			if err := db.UpdateQuality(context.Background(), "alice", "repair", []float64{0.7, 1}[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCorrectionReview_R10CompareAndSegments(t *testing.T) {
	h, db := correctionReaderFixture(t)
	if _, err := rawSealStore(t, db).Exec(`UPDATE corrective_relations SET state='disputed-declared'`); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	h.storedScoringPolicy = true
	q := "?since_a=2026-10-01&until_a=2026-10-02&since_b=2026-10-02&until_b=2026-11-01"
	for _, mode := range []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision} {
		h.SetAggregation(mode, 5)
		w := compareGet(t, h, q)
		want := `"disputed":true`
		if mode.Anonymized() {
			want = `"status":"not_shown"`
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", mode, w.Code, w.Body)
		}
	}
	h.SetAggregation(scoring.AggregationDeveloper, 0)
	w := correctionGet(t, h, "/api/v1/scores?since=2026-10-01&until=2026-11-01")
	var body struct {
		WorkTypes []json.RawMessage `json:"work_types"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.WorkTypes) == 0 {
		t.Fatal("no segments")
	}
	for _, seg := range body.WorkTypes {
		if !bytes.Contains(seg, []byte(`"disputed":true`)) {
			t.Fatal("segment lacks window disclosure", string(seg))
		}
	}
}

func TestCorrectionReview_R11DeclarationSpend(t *testing.T) {
	h, db := correctionReaderFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	o := store.Outcome{Developer: "carol", Repo: "acme/app", PRNumber: 30, MergeCommitSHA: "declaration-only", Timestamp: at, MergedAt: &at}
	if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse("Corrects: #1"), "repair", "test"); err != nil {
		t.Fatal(err)
	}
	pr := 30
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{Developer: "carol", Repo: o.Repo, PRNumber: &pr, CostMicro: 12345, Timestamp: at}); err != nil {
		t.Fatal(err)
	}
	w := correctionGet(t, h, "/api/v1/repair_spend?since=2026-10-01&until=2026-11-01")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"supported_pr_cost_micro":12345`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCorrectionReview_R12SealedRubricAndHeaders(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	ctx := context.Background()
	may, _ := ParsePeriod("2026-05")
	june, _ := ParsePeriod("2026-06")
	if _, _, err := s.sealOrLoad(ctx, may); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	h.storedScoringPolicy = true
	if _, _, err := s.sealOrLoad(ctx, june); err != nil {
		t.Fatal(err)
	}
	w := correctionGet(t, h, "/api/v1/scores?period=2026-06")
	if w.Code != 200 || w.Header().Get("Tier-Sealed-Scoring-Policy") != "event3" || w.Header().Get("Tier-Sealed-Rubric-Version") != "2" || w.Header().Get("Tier-Sealed-Correction-Rule") != "event3-v2" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	w = compareGet(t, h, "?period_a=2026-05&period_b=2026-06")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

type correctionSnapshotFailure struct {
	Store
	err   error
	after bool
}

func (s correctionSnapshotFailure) ReadSnapshot(ctx context.Context, fn func(*store.Snapshot) error) error {
	if s.after {
		if err := s.Store.ReadSnapshot(ctx, fn); err != nil {
			return err
		}
	}
	return s.err
}
func TestCorrectionReview_DetailSnapshotErrors(t *testing.T) {
	for _, after := range []bool{false, true} {
		h, _ := correctionReaderFixture(t)
		h.storedScoringPolicy = true
		h.store = correctionSnapshotFailure{Store: h.store, err: context.Canceled, after: after}
		w := correctionGet(t, h, "/api/v1/scores/alice?since=2026-10-01&until=2026-11-01")
		if w.Code != 500 || strings.Contains(w.Body.String(), "Event 3") || strings.Contains(w.Body.String(), "weighted_points") {
			t.Fatal(after, w.Code, w.Body.String())
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Fatal("double response", w.Body.String())
		}
	}
	h, _ := correctionReaderFixture(t)
	h.storedScoringPolicy = true
	h.store = correctionSnapshotFailure{Store: h.store, err: store.ErrCorrectionPolicyUnresolved}
	w := correctionGet(t, h, "/api/v1/scores/alice?since=2026-10-01&until=2026-11-01")
	if w.Code != 500 || !strings.Contains(w.Body.String(), "Event 3 policy unresolved") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCorrectionReview_DiagnosticIdentityAndPooledSpend(t *testing.T) {
	h, db := correctionReaderFixture(t)
	ctx := context.Background()
	if err := db.UpsertDeveloperAlias(ctx, "alice", "alice-main", "test"); err != nil {
		t.Fatal(err)
	}
	b := u4Diagnostics(t, h)
	if !strings.Contains(string(mustU4JSON(t, b)), `"child_developer":"alice-main"`) || !strings.Contains(string(mustU4JSON(t, b)), `"alice-main":1`) {
		t.Fatal(b)
	}
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{Developer: "alice-main", Repo: "acme/app", IssueID: "repair", CostMicro: 123, Timestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	w := correctionGet(t, h, "/api/v1/repair_spend?since=2026-10-01")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"attribution":"pooled"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.EraseDeveloper(ctx, "alice-main"); err != nil {
		t.Fatal(err)
	}
	b = u4Diagnostics(t, h)
	if strings.Contains(string(mustU4JSON(t, b)), `"":1`) {
		t.Fatal("erased child counted under empty identity", b)
	}
}
