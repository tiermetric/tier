package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestPostOutcomeCannotSetIssueRule(t *testing.T) {
	h, db := newTestHandler(t)
	code, body := postOutcome(t, h, validOutcome("forged", map[string]any{"issue_rule": "body_close_directive"}))
	if code != 400 || !strings.Contains(string(body), "unknown field") {
		t.Fatalf("%d %s", code, body)
	}
	var n int
	if err := rawStore(t, db).QueryRow(`SELECT COUNT(*) FROM outcomes`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if code, body = postOutcome(t, h, validOutcome("explicit", nil)); code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	var rule any
	if err := rawStore(t, db).QueryRow(`SELECT issue_rule FROM outcomes`).Scan(&rule); err != nil || rule != nil {
		t.Fatal(rule, err)
	}
}
func TestPostEventsIssueRuleClosedSetAndSkew(t *testing.T) {
	h, db := newTestHandler(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	batch := []map[string]any{validEvent("absent")}
	for _, bad := range []string{"unknown", "legacy", "/secret/path"} {
		e := validEvent(bad)
		e["issue_rule"] = bad
		batch = append(batch, e)
	}
	for _, r := range issueref.Rules() {
		e := validEvent(string(r))
		e["issue_rule"] = r
		batch = append(batch, e)
	}
	if code, body := postEvents(t, h, batch); code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	rows, _, err := db.ListTokenEvents(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(batch) {
		t.Fatal(len(rows))
	}
	for _, r := range rows {
		want := issueref.Rule(r.IdempotencyKey)
		if issueref.Validate(want) != nil {
			want = ""
		}
		if r.IssueRule != want {
			t.Fatal(r.IdempotencyKey, r.IssueRule, want)
		}
	}
	if strings.Count(logs.String(), "dropping an issue_rule") != 1 || strings.Contains(logs.String(), "/secret/path") {
		t.Fatal(logs.String())
	}
	var n int
	if err := rawStore(t, db).QueryRow(`SELECT COUNT(*) FROM token_events WHERE issue_rule IS NULL`).Scan(&n); err != nil || n != 4 {
		t.Fatal(n, err)
	}
	if err := rawStore(t, db).QueryRow(`SELECT COUNT(*) FROM outcomes`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
func TestIssueRuleExportLastColumn(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	ts := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, rule := range []issueref.Rule{"", issueref.RuleBodyBareHash} {
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", Source: "jsonl", Fidelity: "realtime", Timestamp: ts, IssueRule: rule}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: "alice", IssueID: "issue-1", Weight: 1, Quality: 1, Timestamp: ts, IssueRule: rule}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"events", "outcomes"} {
		rec := doExport(t, h, "/api/v1/"+path+"?since=2026-01-01", http.Header{"Accept": []string{"text/csv"}})
		rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
		if err != nil || len(rows) != 3 {
			t.Fatal(rec.Code, rec.Body.String(), err)
		}
		last := len(rows[0]) - 1
		if rows[0][last] != "issue_rule" || rows[1][last] != "legacy" || rows[2][last] != "body_bare_hash" {
			t.Fatal(rows)
		}
		rec = doExport(t, h, "/api/v1/"+path+"?since=2026-01-01", nil)
		var page map[string]json.RawMessage
		if err = json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		var items []map[string]any
		if err = json.Unmarshal(page[path], &items); err != nil {
			t.Fatal(err)
		}
		if items[0]["issue_rule"] != "legacy" || items[1]["issue_rule"] != "body_bare_hash" {
			t.Fatal(items)
		}
	}
}

func TestIssueRuleExportEvent3LastColumn(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.writeOutcomesCSV(rec, []store.Outcome{{IssueRule: issueref.RuleBodyCloseDirective}}, "", scoring.ScoringPolicy{Event3: true})
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatal(err, rec.Body.String())
	}
	last := len(rows[0]) - 1
	if last != len(outcomesCSVHeader)-1 || rows[0][last] != "issue_rule" || rows[1][last] != "body_close_directive" || rows[0][last-1] != "correction_frozen" {
		t.Fatal(rows)
	}
}

func TestIssueRuleExportOutOfSetRule_JSONAndCSV(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	ts := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", Source: "jsonl", Fidelity: "realtime", Timestamp: ts}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: "alice", IssueID: "issue-1", Weight: 1, Quality: 1, Timestamp: ts}); err != nil {
		t.Fatal(err)
	}
	raw := rawStore(t, db)
	for _, stored := range []string{"/Users/alice/secret-repo", "Branch_first_numeric", "legacy"} {
		for _, table := range []string{"token_events", "outcomes"} {
			if _, err := raw.Exec(`UPDATE `+table+` SET issue_rule = ?`, stored); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{"events", "outcomes"} {
			for _, format := range []string{"application/json", "text/csv"} {
				rec := doExport(t, h, "/api/v1/"+path+"?since=2026-01-01", http.Header{"Accept": []string{format}})
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s status=%d body=%s", path, format, rec.Code, rec.Body.String())
				}
				var got string
				if format == "text/csv" {
					rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
					if err != nil || len(rows) != 2 {
						t.Fatalf("parse CSV: %v body=%s", err, rec.Body.String())
					}
					got = rows[1][len(rows[0])-1]
				} else {
					var rows []struct {
						IssueRule string `json:"issue_rule"`
					}
					// Decode only the named collection; next_cursor is a string.
					var body map[string]json.RawMessage
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(body[path], &rows); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 {
						t.Fatalf("%s: want one row, body=%s", path, rec.Body.String())
					}
					got = rows[0].IssueRule
				}
				if got != "unknown" || strings.Contains(rec.Body.String(), "secret-repo") {
					t.Errorf("%s %s stored %q: issue_rule=%q, want unknown with no stored text", path, format, stored, got)
				}
			}
		}
	}
}
