package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// S03-2: a later merge by another developer sets the shared issue's freshest
// attributable window on both the list and the developer detail.
func TestAuditS03_2_ZeroTokenListDetailAgree(t *testing.T) {
	for _, scope := range []string{"", "&repo=acme/widgets"} {
		t.Run(scope, func(t *testing.T) {
			h, db := newTestHandler(t)
			ctx := context.Background()
			now := time.Now().UTC()
			if err := db.UpsertDeveloperAlias(ctx, "alice-gh", "alice", "test"); err != nil {
				t.Fatal(err)
			}
			if err := db.InsertTokenEvent(ctx, store.TokenEvent{
				Developer: "alice", Repo: "acme/widgets", IssueID: "42",
				Model: "claude-sonnet-4", InputTok: 2000, CostMicro: 6000,
				Source: "jsonl", Fidelity: "realtime", Timestamp: now.Add(-21 * 24 * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			for i, dev := range []string{"alice-gh", "bob"} {
				ts := now.Add(-time.Hour)
				if i == 0 {
					ts = now.Add(-20 * 24 * time.Hour)
				}
				if _, err := db.InsertOutcome(ctx, store.Outcome{
					Developer: dev, Repo: "acme/widgets", IssueID: "42", PRNumber: i + 1,
					Weight: 3, Quality: 1, Timestamp: ts,
				}); err != nil {
					t.Fatal(err)
				}
			}
			query := "?since=" + now.Add(-30*24*time.Hour).Format("2006-01-02") + scope
			code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores"+query, nil)
			if code != http.StatusOK {
				t.Fatalf("list status=%d: %s", code, body)
			}
			var list scoresResponse
			if err := json.Unmarshal(body, &list); err != nil {
				t.Fatal(err)
			}
			var flagged int
			for _, dev := range list.Developers {
				if dev.Developer == "alice" {
					flagged = dev.FlaggedOutcomes
				}
			}
			if flagged != 1 {
				t.Fatalf("list alice flagged=%d, want 1", flagged)
			}
			code, body = doRequest(t, h, http.MethodGet, "/api/v1/scores/alice-gh"+query, nil)
			if code != http.StatusOK {
				t.Fatalf("detail status=%d: %s", code, body)
			}
			var detail developerDetailResponse
			if err := json.Unmarshal(body, &detail); err != nil {
				t.Fatal(err)
			}
			if detail.FlaggedOutcomes != flagged || len(detail.Issues) != 1 || !detail.Issues[0].ZeroToken {
				t.Errorf("detail disagrees with list flagged=%d: %+v", flagged, detail)
			}
		})
	}
}

// S03-3: omitted/null payments cannot create ledger entries; explicit zero and
// negative credit memos remain valid. A rejected developer invoice must leave
// the org fallback allocation intact.
func TestAuditS03_3_ActualPaidRequired(t *testing.T) {
	for _, org := range []bool{false, true} {
		for _, payment := range []string{"missing", "null", "zero", "credit"} {
			name := "developer/" + payment
			if org {
				name = "org/" + payment
			}
			t.Run(name, func(t *testing.T) {
				h, db := newTestHandler(t)
				ctx := context.Background()
				if err := baselineHierarchy(db, ctx, "alice", "platform", "", "acme"); err != nil {
					t.Fatal(err)
				}
				if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
					Org: "acme", Period: "2026-05", ActualPaidMicro: 100_000_000, Timestamp: time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
				path := "/api/v1/actual_spend"
				body := map[string]any{"developer": "alice", "period": "2026-05"}
				if org {
					path = "/api/v1/org_actual_spend"
					body = map[string]any{"org": "acme", "period": "2026-05"}
				}
				wantStatus, wantPaid := http.StatusBadRequest, 100.0
				switch payment {
				case "null":
					body["actual_paid_usd"] = nil
				case "zero":
					body["actual_paid_usd"] = 0
					wantStatus = http.StatusCreated
					if !org {
						wantPaid = 0
					}
				case "credit":
					body["actual_paid_usd"] = -10
					wantStatus, wantPaid = http.StatusCreated, -10
					if org {
						wantPaid = 90
					}
				}
				code, response := doRequest(t, h, http.MethodPost, path, body)
				if code != wantStatus {
					t.Errorf("status=%d, want %d: %s", code, wantStatus, response)
				}
				paid, err := db.ActualSpendForDeveloper(ctx, "alice", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatal(err)
				}
				if paid != wantPaid {
					t.Errorf("allocation=%v, want %v", paid, wantPaid)
				}
				// Allocation alone cannot detect an erroneous $0 org ledger row.
				totals, err := db.OrgActualSpendTotals(ctx, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), "acme")
				if err != nil {
					t.Fatal(err)
				}
				wantRows := 1
				if org && wantStatus == http.StatusCreated {
					wantRows = 2
				}
				if len(totals) != 1 || totals[0].Entries != wantRows {
					t.Errorf("org totals=%+v, want %d rows", totals, wantRows)
				}
			})
		}
	}
}
