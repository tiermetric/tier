package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestTokenEvent_CrossDeveloperGuard(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	e := TokenEvent{Developer: "alice", IssueID: "issue-1144", Model: "claude-sonnet-4", InputTok: 10, OutputTok: 20, CacheRead: 30, CacheWrite5m: 40, CacheWrite1h: 50, Source: "jsonl", Fidelity: "realtime", CostMicro: 100, IdempotencyKey: "collision-1144", Timestamp: time.Now().UTC()}
	if err := db.InsertTokenEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	snapshot := func() []byte {
		rows, err := db.db.QueryContext(ctx, "SELECT * FROM token_events WHERE idempotency_key = ?", e.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatal("missing row")
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(vals)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	e.Developer = "bob"
	e.InputTok = 100
	e.OutputTok = 100
	e.CacheRead = 100
	e.CacheWrite5m = 100
	e.CacheWrite1h = 100
	if err := db.InsertTokenEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); !bytes.Equal(before, after) {
		t.Fatalf("row changed: before %s after %s", before, after)
	}
}

func TestTokenEventsInsertResult_PollerGuardReasons(t *testing.T) {
	for _, source := range []string{"anthropic-admin", "openai-usage"} {
		t.Run(source, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			original := TokenEvent{Developer: "alice", IssueID: "issue-1144", Model: "claude-sonnet-4", InputTok: 10, CostMicro: 100, Source: source, Fidelity: "daily", IdempotencyKey: "poller-1144", Timestamp: time.Now().UTC()}
			if err := db.InsertTokenEvent(ctx, original); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				developer string
				rejected  int
			}{{"alice", 0}, {"bob", 1}} {
				replay := original
				replay.Source = "jsonl"
				replay.Developer = tc.developer
				replay.InputTok = 1000
				result, err := db.InsertTokenEventsWithResult(ctx, []TokenEvent{replay})
				if err != nil {
					t.Fatal(err)
				}
				if result.RejectedCrossDeveloper != tc.rejected || len(result.Applied) != 1 || result.Applied[0] {
					t.Errorf("developer=%s: result=%+v, want rejected=%d applied=[false]", tc.developer, result, tc.rejected)
				}
			}
		})
	}
}
