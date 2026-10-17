package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestCorrectionScore_FlagOffUsesLegacyResponseBytes(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		when := at.Add(time.Duration(i) * time.Hour)
		o := store.Outcome{Developer: "alice", Repo: "acme/app", PRNumber: i, IssueID: "repair-issue", Weight: 5, Quality: 1, Timestamp: when, MergedAt: &when, MergeCommitSHA: []string{"", "original-sha", "repair-sha"}[i]}
		if i == 2 {
			o.Weight = 1
		}
		a, b, c := 10, 0, 1
		o.SetDiffStats(&a, &b, &c)
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
		body := "Corrects: none"
		if i == 2 {
			body = "Corrects: #1"
		}
		if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "bounded evidence", "test"); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/v1/scores?since=2026-10-01&until=2026-11-01"
	before := sealedGet(t, h, path)
	if before.Code != http.StatusOK {
		t.Fatal(before.Body.String())
	}
	o, ok, err := db.OutcomeByMergeCommit(ctx, "repair-sha")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := db.ReconcileCorrectionChain(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	after := sealedGet(t, h, path)
	if after.Code != before.Code || after.Body.String() != before.Body.String() {
		t.Fatalf("flag-off response changed\nbefore %s\nafter %s", before.Body, after.Body)
	}
	off := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", RateLimitConfig{}, WithScoringPolicy(scoring.ScoringPolicy{}))
	if !reflect.DeepEqual(h.scoringPolicy, off.scoringPolicy) {
		t.Fatal("default policy differs from explicit off")
	}
	explicit := sealedGet(t, off, path)
	if explicit.Body.String() != before.Body.String() {
		t.Fatal("explicit off response differs")
	}
}
