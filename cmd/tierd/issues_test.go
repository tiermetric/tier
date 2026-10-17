package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestIssuesCommandServedHandler(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "alice", Repo: "acme/app", IssueID: "issue-1", CostMicro: 6000000, InputTok: 2000, Source: "jsonl", Timestamp: at}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := db.InsertOutcome(t.Context(), store.Outcome{Developer: "alice", Repo: "acme/app", IssueID: "issue-1", Weight: 1, Quality: 1, PRNumber: i, Timestamp: at}); err != nil {
			t.Fatal(err)
		}
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "admin", nil, "test", api.RateLimitConfig{}, api.WithStoredScoringPolicy())
	h.SetReadToken("viewer")
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("TIER_API_TOKEN", "viewer")
	args := []string{"issues", "--server", srv.URL, "--since", "2026-10-01", "--repo", "acme/app"}
	var out, stderr bytes.Buffer
	if code := dispatch(args, &out, &stderr); code != 0 {
		t.Fatalf("exit=%d %s", code, &stderr)
	}
	for _, want := range []string{"attributed (lower bound)", "unattributed", "issue-1", "500.00", "rubric 1", "Reconciliation", `Cost capture began "2026-10-01T00:00:00Z"`, "window predates cost capture: false", `fully covered --since "2026-10-01"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	out.Reset()
	stderr.Reset()
	if code := dispatch(append(args, "--json"), &out, &stderr); code != 0 {
		t.Fatal(code, &stderr)
	}
	var r struct {
		Total float64 `json:"total_cost_usd"`
		Rows  []struct {
			TIER *float64 `json:"tier"`
		} `json:"reconciliation"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Total != 6 || len(r.Rows) != 1 || r.Rows[0].TIER == nil || *r.Rows[0].TIER != 500 {
		t.Fatal(out.String())
	}
	// Server-side mode refusal reaches the command as a failing exit.
	h.SetAggregation(scoring.AggregationTeam, 3)
	out.Reset()
	stderr.Reset()
	if code := dispatch(args, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "403") {
		t.Fatal(code, &out, &stderr)
	}
}

func TestIssuesCommandDataQualityAndJSONBytes(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", "")
	// Include an additive field unknown to this CLI. --json must retain it,
	// along with the original whitespace, after validating the report.
	body := `{
  "since": "2026-01-01", "rubric_version": 1,
  "label": "attributed (lower bound)", "reconciliation": [],
  "future_disclosure": {"checked": true},
  "data_quality": {
    "cost_coverage_start": "2026-10-01T12:00:00Z",
    "cost_coverage_safe_since": "2026-10-02",
    "window_predates_cost_capture": true,
    "mixed_price_versions": [1, 2]
  }
}
`
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/api/v1/issues" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	args := []string{"--server", srv.URL, "--since", "2026"}
	var out, stderr bytes.Buffer
	if code := runIssues(args, &out, &stderr); code != 0 {
		t.Fatal(code, &stderr)
	}
	for _, want := range []string{`Cost capture began "2026-10-01T12:00:00Z"`, "window predates cost capture: true", `fully covered --since "2026-10-02"`, "Mixed price versions: [1 2]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	out.Reset()
	if code := runIssues(append(args, "--json"), &out, &stderr); code != 0 || out.String() != body {
		t.Fatal(code, &out, &stderr)
	}
	if requests != 2 {
		t.Fatalf("got %d requests; want one GET per invocation", requests)
	}
}
