package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

// Read every on-disk column, including ones absent from the export projection.
func eventRowBytes(t *testing.T, db *store.DB, key string) []byte {
	t.Helper()
	path, ok := testStorePaths.Load(db)
	if !ok {
		t.Fatal("store path missing")
	}
	raw, err := sql.Open("sqlite", path.(string))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.QueryContext(context.Background(), "SELECT * FROM token_events WHERE idempotency_key = ?", key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing keyed row")
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPostEvents_CrossDeveloperCollision(t *testing.T) {
	h, db := newTestHandler(t)
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	original := validEvent("cross-developer-1144")
	if code, body := postEvents(t, h, []map[string]any{original}); code != http.StatusCreated {
		t.Fatalf("first post %d: %s", code, body)
	}
	before := eventRowBytes(t, db, "cross-developer-1144")
	collision := validEvent("cross-developer-1144")
	collision["developer"] = "bob"
	collision["input_tokens"] = 9000
	collision["output_tokens"] = 9000
	collision["cache_read_tokens"] = 9000
	collision["cache_write_5m_tokens"] = 9000
	collision["cache_write_1h_tokens"] = 9000
	for _, tc := range []struct {
		name               string
		batch              []map[string]any
		rejected, accepted int
	}{
		{"cross-developer", []map[string]any{collision, validEvent("fresh-1144")}, 1, 1},
		{"same-developer replay", []map[string]any{original}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postEvents(t, h, tc.batch)
			if code != http.StatusCreated {
				t.Fatalf("post %d: %s", code, body)
			}
			var receipt map[string]int
			if err := json.Unmarshal(body, &receipt); err != nil {
				t.Fatal(err)
			}
			rejected, exists := receipt["rejected_cross_developer"]
			if !exists || rejected != tc.rejected || receipt["accepted"] != tc.accepted {
				t.Errorf("receipt = %s, want rejected=%d accepted=%d", body, tc.rejected, tc.accepted)
			}
			if after := eventRowBytes(t, db, "cross-developer-1144"); !bytes.Equal(before, after) {
				t.Errorf("row changed:\nbefore %s\nafter %s", before, after)
			}
		})
	}
	if !bytes.Contains(logs.Bytes(), []byte("level=WARN")) || !bytes.Contains(logs.Bytes(), []byte("rejected_cross_developer=1")) {
		t.Errorf("missing count WARN: %s", logs.String())
	}
}

func TestPostEvents_RejectedRowHasNoPricingDivergence(t *testing.T) {
	for _, source := range []string{"jsonl", "anthropic-admin"} {
		t.Run(source, func(t *testing.T) {
			h, db := newTestHandler(t)
			var logs bytes.Buffer
			h.logger = slog.New(slog.NewTextHandler(&logs, nil))
			reg := metrics.NewRegistry()
			h.SetPricingDivergenceCounter(reg.NewCounter("tier_pricing_divergence_total", "test"))
			original := store.TokenEvent{Developer: "alice", IssueID: "issue-1144", Model: "claude-sonnet-4", InputTok: 1000, CostMicro: 3000, Fidelity: "realtime", IdempotencyKey: "test-1144", Timestamp: time.Now().UTC()}
			original.Source = source
			if err := db.InsertTokenEvent(context.Background(), original); err != nil {
				t.Fatal(err)
			}
			collision := validEvent(original.IdempotencyKey)
			if source == "jsonl" {
				collision["developer"] = "bob"
			}
			collision["cost_usd"] = 999.99
			if code, body := postEvents(t, h, []map[string]any{collision}); code != http.StatusCreated {
				t.Fatalf("post %d: %s", code, body)
			}
			if got := divergenceCount(t, reg); got != 0 {
				t.Errorf("rejected row recorded pricing divergence=%v, want 0", got)
			}
			if strings.Contains(logs.String(), "events cost divergence") {
				t.Errorf("rejected row logged pricing divergence: %s", logs.String())
			}
		})
	}
}

func TestPostEvents_UnattributedCollisionIsNotCrossDeveloper(t *testing.T) {
	h, db := newTestHandler(t)
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	original := store.TokenEvent{Developer: "alice", IssueID: "issue-1144", Model: "claude-sonnet-4", InputTok: 1000, CostMicro: 3000, Fidelity: "realtime", IdempotencyKey: "proxy-unattributed-1144", Timestamp: time.Now().UTC()}
	original.Developer = store.UnattributedIssueID
	original.Source = "proxy"
	if err := db.InsertTokenEvent(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	before := eventRowBytes(t, db, original.IdempotencyKey)
	replay := validEvent(original.IdempotencyKey)
	replay["input_tokens"] = 9000
	code, body := postEvents(t, h, []map[string]any{replay})
	if code != http.StatusCreated {
		t.Fatalf("post %d: %s", code, body)
	}
	var receipt eventsResponse
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.RejectedCrossDeveloper != 0 || receipt.Accepted != 1 {
		t.Errorf("receipt=%s, want accepted=1 rejected_cross_developer=0", body)
	}
	if after := eventRowBytes(t, db, original.IdempotencyKey); !bytes.Equal(before, after) {
		t.Errorf("row changed: before %s after %s", before, after)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("placeholder collision warned: %s", logs.String())
	}
}
