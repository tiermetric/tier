package shipper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tiermetric/tier/internal/issueref"
)

func TestClientIssueRuleClosedSetAndSkew(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		if len(batch) != 3 {
			t.Errorf("batch=%v", batch)
			return
		}
		if _, ok := batch[0]["issue_rule"]; ok {
			t.Error("absent rule was sent")
		}
		if _, ok := batch[1]["issue_rule"]; ok {
			t.Error("unknown rule was sent")
		}
		if batch[2]["issue_rule"] != "branch_first_numeric" {
			t.Error(batch[2])
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, Config{BatchSize: 3})
	for i, rule := range []string{"", "unknown", "branch_first_numeric"} {
		ev := testEvent(string(rune('a' + i)))
		ev.IssueRule = issueref.Rule(rule)
		if err := c.Ingest(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
