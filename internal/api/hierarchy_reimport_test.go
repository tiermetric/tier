package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type reimportMember struct{ Developer, Org string }

func TestEndMembershipAliasKeyedSeat(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	if err := db.UpsertHierarchy(ctx, "alice-laptop", "eng", "", "acme", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDeveloperAlias(ctx, "alice-laptop", "alice", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/period_membership/alice/end", map[string]any{"org": "acme", "period_end": "2025-06"})
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	p, _ := testStorePaths.Load(db)
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var end, reason string
	if err := raw.QueryRow(`SELECT COALESCE(period_end, ''), COALESCE(ended_reason, '') FROM period_membership WHERE developer = 'alice-laptop' AND org = 'acme'`).Scan(&end, &reason); err != nil {
		t.Fatal(err)
	}
	if end != "2025-06" || reason != "explicit" {
		t.Errorf("alias-keyed end=%q reason=%q; want 2025-06/explicit", end, reason)
	}
	code, body = doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", []map[string]any{{"developer": "alice", "team": "eng", "org": "acme"}})
	if code != http.StatusCreated || !strings.Contains(string(body), `"kept_departed_count":1`) {
		t.Errorf("re-import status=%d body=%s; want 201/kept-departed", code, body)
	}
}

func TestHierarchyReimport(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		for _, scenario := range []string{"explicit", "rejoin", "blanked", "alias", "move", "unknown", "null", "cleared-legacy", "mixed"} {
			t.Run(fmt.Sprintf("bulk=%t/%s", bulk, scenario), func(t *testing.T) {
				h, db := newTestHandler(t)
				ctx := context.Background()
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				dev := "alice"
				if scenario == "alias" {
					dev = "alice-laptop"
				}
				must(db.UpsertHierarchy(ctx, dev, "eng", "", "acme", "test:fixture"))
				must(db.EndMembership(ctx, dev, "acme", "2025-03"))
				if scenario == "alias" {
					must(db.UpsertDeveloperAlias(ctx, dev, "alice", "test:fixture"))
				}
				if scenario == "blanked" || scenario == "cleared-legacy" {
					must(db.UpsertHierarchy(ctx, dev, "eng", "", "", "test:fixture"))
				}
				p, _ := testStorePaths.Load(db)
				raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
				must(err)
				defer func() { _ = raw.Close() }()
				if scenario == "unknown" || scenario == "null" || scenario == "cleared-legacy" {
					var reason any = "unknown"
					if scenario == "null" {
						reason = nil
					}
					_, err = raw.Exec(`UPDATE period_membership SET ended_reason = ? WHERE developer = ?`, reason, dev)
					must(err)
				}
				if scenario == "move" {
					_, err = raw.Exec(`UPDATE period_membership SET period_end = NULL, ended_reason = NULL WHERE developer = ?`, dev)
					must(err)
					must(db.UpsertHierarchy(ctx, dev, "eng", "", "globex", "test:fixture"))
				}
				item := map[string]any{"developer": "alice", "team": "eng", "org": "acme"}
				if scenario == "rejoin" {
					item["rejoin"] = true
				}
				batch := []map[string]any{item}
				wantKept, wantReseated := []reimportMember{}, []reimportMember{}
				closed := scenario == "explicit" || scenario == "blanked" || scenario == "alias" || scenario == "mixed"
				if closed {
					wantKept = append(wantKept, reimportMember{"alice", "acme"})
				}
				if scenario == "unknown" || scenario == "null" || scenario == "cleared-legacy" {
					wantReseated = append(wantReseated, reimportMember{"alice", "acme"})
				}
				if scenario == "mixed" && bulk {
					_, err = raw.Exec(`INSERT INTO period_membership (developer, org, period_start, period_end, ended_reason) VALUES ('legacy', 'acme', '0000-01', '2025-03', 'unknown')`)
					must(err)
					batch = append(batch, map[string]any{"developer": "legacy", "team": "eng", "org": "acme"}, map[string]any{"developer": "new", "team": "eng", "org": "acme"})
					wantReseated = append(wantReseated, reimportMember{"legacy", "acme"})
				}
				method, target, wantStatus := http.MethodPut, "/api/v1/org_hierarchy/alice", http.StatusOK
				var input any = item
				if bulk {
					method, target, wantStatus, input = http.MethodPost, "/api/v1/org_hierarchy", http.StatusCreated, batch
				} else {
					delete(item, "developer")
				}
				before := time.Now().UTC().Format("2006-01")
				code, body := doRequest(t, h, method, target, input)
				if code != wantStatus {
					t.Fatalf("status=%d body=%s", code, body)
				}
				var report struct {
					KeptDeparted    []reimportMember `json:"kept_departed"`
					ReseatedUnknown []reimportMember `json:"reseated_unknown"`
				}
				must(json.Unmarshal(body, &report))
				if !reflect.DeepEqual(report.KeptDeparted, wantKept) || !reflect.DeepEqual(report.ReseatedUnknown, wantReseated) {
					t.Errorf("report=%s; want kept=%v reseated=%v", body, wantKept, wantReseated)
				}
				var count int
				must(raw.QueryRow(`SELECT COUNT(*) FROM period_membership WHERE developer IN ('alice','alice-laptop') AND org = 'acme' AND period_end IS NULL`).Scan(&count))
				if closed {
					if count != 0 {
						t.Errorf("explicitly departed person has %d open seats", count)
					}
				} else {
					var start string
					must(raw.QueryRow(`SELECT period_start FROM period_membership WHERE developer = 'alice' AND org = 'acme' AND period_end IS NULL`).Scan(&start))
					if start != before && start != time.Now().UTC().Format("2006-01") {
						t.Errorf("reopened at %s, want current month", start)
					}
				}
			})
		}
	}
}

func TestHierarchyReimportReportBound(t *testing.T) {
	h, db := newTestHandler(t)
	p, _ := testStorePaths.Load(db)
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var batch []map[string]string
	for _, reason := range []string{"explicit", "unknown"} {
		for i := 0; i < 101; i++ {
			dev := fmt.Sprintf("%s-%03d", reason, i)
			if _, err := raw.Exec(`INSERT INTO period_membership (developer, org, period_start, period_end, ended_reason) VALUES (?, 'acme', '2025-01', '2025-03', ?)`, dev, reason); err != nil {
				t.Fatal(err)
			}
			batch = append(batch, map[string]string{"developer": dev, "team": "eng", "org": "acme"})
		}
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", batch)
	var report map[string]json.RawMessage
	if code != http.StatusCreated || json.Unmarshal(body, &report) != nil {
		t.Fatalf("status=%d body=%s", code, body)
	}
	for key, reason := range map[string]string{"kept_departed": "explicit", "reseated_unknown": "unknown"} {
		var members []reimportMember
		var count int
		if json.Unmarshal(report[key], &members) != nil || json.Unmarshal(report[key+"_count"], &count) != nil || len(members) != 100 || count != 101 {
			t.Errorf("%s: members=%d count=%d; want 100/101", key, len(members), count)
			continue
		}
		if members[0].Developer != reason+"-000" || members[99].Developer != reason+"-099" {
			t.Errorf("%s did not retain the first 100", key)
		}
	}
}

func TestHierarchyRejoinInvalid(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		h, _ := newTestHandler(t)
		method, target := http.MethodPut, "/api/v1/org_hierarchy/alice"
		item := map[string]any{"team": "eng", "rejoin": "yes"}
		var input any = item
		if bulk {
			method, target = http.MethodPost, "/api/v1/org_hierarchy"
			item["developer"] = "alice"
			input = []map[string]any{item}
		}
		code, body := doRequest(t, h, method, target, input)
		if code != http.StatusBadRequest || !strings.Contains(string(body), "bool") {
			t.Errorf("bulk=%t status=%d body=%s", bulk, code, body)
		}
	}
}
