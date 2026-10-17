package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestPostEventsPRNumberValidationAndStorage(t *testing.T) {
	h, db := newTestHandler(t)
	for _, value := range []any{0, -1, 1.5, "42", 1e30} {
		e := validEvent("invalid-pr")
		e["pr_number"] = value
		if status, body := postEvents(t, h, []map[string]any{e}); status != 400 {
			t.Fatalf("value=%v status=%d: %s", value, status, body)
		}
	}
	for _, value := range []any{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 65), 42} {
		e := validEvent("invalid-hash")
		e["branch_hash"] = value
		if status, body := postEvents(t, h, []map[string]any{e}); status != 400 {
			t.Fatalf("hash=%v status=%d: %s", value, status, body)
		}
	}
	forged := validEvent("forged")
	forged["attribution_rule"] = string(store.AttributionRulePRNumberAtInsert)
	if status, body := postEvents(t, h, []map[string]any{forged}); status != 400 {
		t.Fatalf("forged rule: %d %s", status, body)
	}
	e := validEvent("valid-pr")
	e["branch_hash"] = strings.Repeat("a", 64)
	e["pr_number"] = 42
	e["issue_id"] = "unattributed"
	e["repo"] = "acme/a"
	if status, body := postEvents(t, h, []map[string]any{e}); status != 201 {
		t.Fatalf("status=%d: %s", status, body)
	}
	rows, _, err := db.ListTokenEvents(context.Background(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().Add(time.Hour), store.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PRNumber == nil || *rows[0].PRNumber != 42 || rows[0].BranchHash == nil || *rows[0].BranchHash != strings.Repeat("a", 64) || rows[0].IssueID != "unattributed" {
		t.Fatalf("rows=%+v", rows)
	}
}
