package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// exportRules fetches /events once as JSON and once as CSV and returns each
// row's attribution_rule, in (ts, id) order, from both.
func exportRules(t *testing.T, h *Handler) (fromJSON, fromCSV []string, bodies string) {
	t.Helper()
	rec := doExport(t, h, "/api/v1/events?since=2026-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON status=%d body=%s", rec.Code, rec.Body.String())
	}
	bodies = rec.Body.String()
	var page struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	for _, e := range page.Events {
		v, ok := e["attribution_rule"].(string)
		if !ok {
			t.Fatalf("JSON event has no string attribution_rule: %v", e)
		}
		fromJSON = append(fromJSON, v)
	}
	rec = doExport(t, h, "/api/v1/events?since=2026-01-01", http.Header{"Accept": []string{"text/csv"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("CSV status=%d body=%s", rec.Code, rec.Body.String())
	}
	bodies += rec.Body.String()
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	last := len(records[0]) - 1
	for _, row := range records[1:] {
		fromCSV = append(fromCSV, row[last])
	}
	return fromJSON, fromCSV, bodies
}

func seedRuleAt(t *testing.T, db *store.DB, rule store.AttributionRule, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
		InputTok: 1000, CostMicro: 1, Source: "jsonl", Fidelity: "realtime",
		AttributionRule: rule, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

func rawStore(t *testing.T, db *store.DB) *sql.DB {
	t.Helper()
	p, ok := testStorePaths.Load(db)
	if !ok {
		t.Fatal("store not registered")
	}
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// TestEventsCSVHeader_AttributionRuleLast pins attribution_rule as the column
// appended after billed_to, last in both the declared and the served header.
func TestEventsCSVHeader_AttributionRuleLast(t *testing.T) {
	n := len(eventsCSVHeader)
	if eventsCSVHeader[n-1] != "attribution_rule" || eventsCSVHeader[n-2] != "billed_to" {
		t.Fatalf("eventsCSVHeader tail = %v, want [... billed_to attribution_rule]", eventsCSVHeader[n-2:])
	}
	h, _ := newTestHandler(t)
	rec := doExport(t, h, "/api/v1/events?since=2026-01-01", http.Header{"Accept": []string{"text/csv"}})
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(records) == 0 {
		t.Fatalf("parse CSV: %v (%d records)", err, len(records))
	}
	if got := records[0][len(records[0])-1]; got != "attribution_rule" {
		t.Errorf("served CSV header ends with %q, want attribution_rule", got)
	}
}

// TestExport_NullIsLegacy_JSONAndCSV pins the #823 Q4 ruling: a row with no
// rule stores NULL and exports as "legacy" in JSON and CSV, and every member of
// the closed set exports verbatim.
func TestExport_NullIsLegacy_JSONAndCSV(t *testing.T) {
	h, db := newTestHandler(t)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seedRuleAt(t, db, store.AttributionRuleNone, base)
	want := []string{"legacy"}
	for i, r := range store.AttributionRules() {
		seedRuleAt(t, db, r, base.Add(time.Duration(i+1)*time.Hour))
		want = append(want, string(r))
	}

	var nulls int
	if err := rawStore(t, db).QueryRow(`SELECT COUNT(*) FROM token_events WHERE attribution_rule IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 {
		t.Fatalf("rows storing NULL = %d, want 1 (the rule-less row)", nulls)
	}

	fromJSON, fromCSV, _ := exportRules(t, h)
	if strings.Join(fromJSON, ",") != strings.Join(want, ",") {
		t.Errorf("JSON attribution_rule = %v, want %v", fromJSON, want)
	}
	if strings.Join(fromCSV, ",") != strings.Join(want, ",") {
		t.Errorf("CSV attribution_rule = %v, want %v", fromCSV, want)
	}
}

// TestExport_OutOfSetRuleRenderedAsFixedToken pins that a stored value outside
// store.AttributionRules(), which no insert path admits, exports as "unknown"
// and its text never appears in either body.
func TestExport_OutOfSetRuleRenderedAsFixedToken(t *testing.T) {
	h, db := newTestHandler(t)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	stored := []string{"/Users/alice/secret-repo", "Branch", "legacy"}
	for i := range stored {
		seedRuleAt(t, db, store.AttributionRuleBranch, base.Add(time.Duration(i)*time.Hour))
	}
	raw := rawStore(t, db)
	for i, v := range stored {
		if _, err := raw.Exec(`UPDATE token_events SET attribution_rule = ? WHERE ts = ?`,
			v, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	fromJSON, fromCSV, bodies := exportRules(t, h)
	want := "unknown,unknown,unknown"
	if got := strings.Join(fromJSON, ","); got != want {
		t.Errorf("JSON attribution_rule = %s, want %s", got, want)
	}
	if got := strings.Join(fromCSV, ","); got != want {
		t.Errorf("CSV attribution_rule = %s, want %s", got, want)
	}
	if strings.Contains(bodies, "secret-repo") {
		t.Error("an out-of-set stored value was echoed in the export")
	}
}
