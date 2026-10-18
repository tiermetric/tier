package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type storedReadQuerier struct {
	readQuerier
	rewrites int
}

func (q *storedReadQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	for _, index := range []string{"", " INDEXED BY idx_token_events_ts_id"} {
		prefix := effectiveAttributionCTE(WithReadTimeRecovery(ctx, false), index)
		if strings.HasPrefix(query, prefix) {
			q.rewrites++
			query = strings.TrimPrefix(query, prefix)
			query = strings.ReplaceAll(query, "effective_events", "token_events"+index)
			query = strings.ReplaceAll(query, "effective_issue_id", "issue_id")
			query = strings.ReplaceAll(query, "stored_issue_id", "issue_id")
			break
		}
	}
	return q.readQuerier.QueryContext(ctx, query, args...)
}

func TestEffectiveAttributionDisabledDifferential(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := WithReadTimeRecovery(t.Context(), false)
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	issues := []string{"issue-1", "issue-2", UnattributedIssueID, UnattributedMainBucket, UnattributedNoIssueBucket, ""}
	for i := 0; i < 180; i++ {
		issue := issues[i%len(issues)]
		e := TokenEvent{Developer: fmt.Sprint("dev", i%3), Repo: []string{"acme/a", "acme/b", "unqualified"}[i%3], IssueID: issue, CostMicro: int64(i + 1), InputTok: 2000, Source: []string{"api", "jsonl"}[i%2], Fidelity: []string{"realtime", "daily", "estimated"}[i%3], Timestamp: at.Add(time.Duration(i-90) * time.Minute)}
		if i%7 == 0 && issue != "" && !IsUnattributed(issue) {
			e.AttributionRule = AttributionRuleBranch
		}
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE token_events SET attribution_rule='branch-at-merge' WHERE attribution_rule='branch'`); err != nil {
		t.Fatal(err)
	}
	stored := &storedReadQuerier{readQuerier: db.db}
	goldenBytes, err := os.ReadFile("testdata/effective_attribution_main.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatal(err)
	}
	pin := func(scope RepoScope, until time.Time, name string, value any) {
		t.Helper()
		key := fmt.Sprintf("%s/%v/%s", scope, until.IsZero(), name)
		want, ok := golden[key]
		if !ok {
			t.Fatal("missing main golden", key)
		}
		if !bytes.Equal(mainReadJSON(value), mainReadJSON(want)) {
			t.Fatalf("%s differs from 6b9851d9: got %s want %s", key, mainReadJSON(value), mainReadJSON(want))
		}
	}
	projected, legacy := reader{db.db}, reader{stored}
	for _, scope := range []RepoScope{FleetWide, "acme/a"} {
		for _, until := range []time.Time{{}, at.Add(30 * time.Minute)} {
			since := at.Add(-30 * time.Minute)
			compare := func(name string, a, b any, ea, eb error) {
				t.Helper()
				if stored.rewrites == 0 {
					t.Fatalf("%s: no projection rewrite undone", name)
				}
				stored.rewrites = 0
				if ea != nil || eb != nil {
					t.Fatalf("%s: %v / %v", name, ea, eb)
				}
				x, _ := json.Marshal(a)
				y, _ := json.Marshal(b)
				pin(scope, until, name, b)
				if !bytes.Equal(x, y) {
					t.Fatalf("%s: projected %s != stored %s", name, x, y)
				}
			}
			a, ea := projected.DeveloperIssueCostsWindow(ctx, since, until, scope)
			b, eb := legacy.DeveloperIssueCostsWindow(ctx, since, until, scope)
			compare("issue costs", a, b, ea, eb)
			c, ec := projected.CostCompositionWindow(ctx, since, until, scope)
			d, ed := legacy.CostCompositionWindow(ctx, since, until, scope)
			compare("composition", c, d, ec, ed)
			e, ee := projected.UnattributedBucketCostsWindow(ctx, since, until, scope)
			f, ef := legacy.UnattributedBucketCostsWindow(ctx, since, until, scope)
			compare("buckets", e, f, ee, ef)
			g, eg := projected.CapturedDollarRowsWindow(ctx, since, until, scope)
			h, eh := legacy.CapturedDollarRowsWindow(ctx, since, until, scope)
			compare("captured dollars", g, h, eg, eh)
			outs := []Outcome{{Developer: "dev0", Repo: "acme/a", IssueID: "issue-1", Timestamp: at}, {Developer: "dev1", Repo: "unqualified", IssueID: "issue-2", Timestamp: at}}
			j, ej := projected.OutcomeTokenTotals(ctx, outs, scope)
			k, ek := legacy.OutcomeTokenTotals(ctx, outs, scope)
			if stored.rewrites == 0 {
				t.Fatal("token totals: no projection rewrite undone")
			}
			stored.rewrites = 0
			if ej != nil || ek != nil || fmt.Sprint(j) != fmt.Sprint(k) {
				t.Fatalf("token totals %v/%v: %v/%v", j, k, ej, ek)
			}
			pin(scope, until, "token totals", fmt.Sprint(k))
			l, el := projected.RepairSpendWindow(ctx, since, until, scope)
			m, em := legacy.RepairSpendWindow(ctx, since, until, scope)
			compare("repair spend", l, m, el, em)
		}
	}
	events, err := db.EffectiveAttributionsWindow(ctx, at.Add(-24*time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[AttributionLayer]bool{}
	var total int64
	for _, e := range events {
		if e.IssueID != e.StoredIssueID {
			t.Fatal(e)
		}
		seen[e.Layer] = true
		total += e.CostMicro
	}
	if len(events) != 180 || total != 180*181/2 || !seen[AttributionStored] || !seen[AttributionFill] || !seen[AttributionUnresolved] {
		t.Fatalf("events=%d total=%d layers=%v", len(events), total, seen)
	}
}

func BenchmarkEffectiveAttributionWindow(b *testing.B) {
	db, err := Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := WithReadTimeRecovery(context.Background(), false)
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 40000; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO token_events(developer,issue_id,model,cost_micro,source,fidelity,repo,ts) VALUES(?,?,'test',1,'jsonl','realtime','acme/a',?)`, fmt.Sprint("dev", i%10), fmt.Sprint("issue-", i%500), at); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.DeveloperIssueCostsWindow(ctx, at.Add(-time.Hour), at.Add(time.Hour), FleetWide); err != nil {
			b.Fatal(err)
		}
	}
}

func TestEffectiveStatementRejectsMultipleEventReferences(t *testing.T) {
	ctx := WithReadTimeRecovery(t.Context(), false)
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, err := (reader{db.db}).queryEffective(ctx, `SELECT a.issue_id FROM token_events a JOIN token_events b ON a.id=b.id WHERE a.ts>=?`, time.Now())
	if err == nil {
		t.Fatal("accepted a multiply referenced projection whose window can be materialized")
	}
}

// The baseline is the stored reader output of 6b9851d9 for this fixture.
func mainReadJSON(value any) []byte {
	data, _ := json.Marshal(value)
	var v any
	_ = json.Unmarshal(data, &v)
	var normalize func(any)
	normalize = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "ReadTimeRecoveryMicro")
			delete(x, "ReadTimeRecoveryEvents")
			for _, child := range x {
				normalize(child)
			}
		case []any:
			for _, child := range x {
				normalize(child)
			}
		}
	}
	normalize(v)
	data, _ = json.Marshal(v)
	return data
}
