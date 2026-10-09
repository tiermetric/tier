package shipper

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

func TestShipper_500MaximalEventsDelivered(t *testing.T) {
	for _, char := range []string{"x", "\"", "<"} {
		t.Run(char, func(t *testing.T) {
			srv, db := realTierdServer(t)
			c := newTestClient(t, srv, Config{})
			ctx := context.Background()
			long := strings.Repeat(char, 256)
			ev := testEvent("")
			ev.Developer, ev.IssueID, ev.Model = long, long, long
			ev.Host, ev.SessionID = long, long
			ev.Repo = strings.Repeat("a", 127) + "/" + strings.Repeat("b", 128)
			ev.Source = collector.SourceCodexRollout
			ev.AttributionRule = store.AttributionRuleWorktreeToolPath
			// The unknown-model fallback bills all five fields at $0.50/M.
			maxTokens := min(math.MaxInt, store.MaxTokenCount, 4_000_000_000)
			ev.InputTok, ev.OutputTok, ev.CacheRead = maxTokens, maxTokens, maxTokens
			ev.CacheWrite5m, ev.CacheWrite1h = maxTokens, maxTokens
			ev.CostMicro = store.MaxTokenEventCostMicro
			ev.Timestamp = time.Date(2026, 5, 19, 10, 0, 0, 123456789, time.UTC)
			want := make(map[string]bool)
			for i := 0; i < 500; i++ {
				ev.IdempotencyKey = strings.Repeat(char, 253) + fmt.Sprintf("%03d", i)
				want[ev.IdempotencyKey] = true
				if i == 499 {
					batch := make([]wireEvent, 500)
					copy(batch, c.buf)
					batch[499] = batch[498]
					batch[499].IdempotencyKey = ev.IdempotencyKey
					body, err := json.Marshal(batch)
					if err != nil {
						t.Fatal(err)
					}
					if len(body) <= 1<<20 {
						t.Fatalf("500-event fixture must exceed 1 MiB unsplit: %d bytes", len(body))
					}
					eventBytes := (len(body)-1)/500 - 1
					t.Logf("one maximal event: %d bytes; 500-event batch: %d bytes", eventBytes, len(body))
				}
				if err := c.Ingest(ctx, ev); err != nil {
					t.Fatalf("Ingest(%d): %v", i, err)
				}
			}
			if err := c.Flush(ctx); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := c.Shipped(); got != 500 {
				t.Fatalf("Shipped() = %d, want 500", got)
			}
			rows, more, err := db.ListTokenEvents(ctx, ev.Timestamp.Add(-time.Second), ev.Timestamp.Add(time.Second), store.PageCursor{}, 501)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 500 || more {
				t.Fatalf("stored %d events (more=%v), want 500", len(rows), more)
			}
			for _, row := range rows {
				if !want[row.IdempotencyKey] {
					t.Errorf("unexpected or duplicate key %q", row.IdempotencyKey)
				}
				delete(want, row.IdempotencyKey)
			}
			if len(want) != 0 {
				t.Errorf("%d events missing from store", len(want))
			}
			if err := c.Ingest(ctx, testEvent("after-maximal-batch")); err != nil {
				t.Fatalf("Ingest after split batch: %v", err)
			}
			if err := c.Flush(ctx); err != nil {
				t.Fatalf("Flush after split batch: %v", err)
			}
			if got := c.Shipped(); got != 501 {
				t.Errorf("Shipped() after continued capture = %d, want 501", got)
			}
		})
	}
}

func TestClient_SplitBatchRetainsUnsentEvents(t *testing.T) {
	rs := &recordingServer{statuses: []int{http.StatusCreated, http.StatusBadRequest, http.StatusCreated}}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv, Config{})
	for i := 0; i < 4; i++ {
		c.buf = append(c.buf, wireEvent{
			Developer:      strings.Repeat("x", maxBatchBody/3),
			IdempotencyKey: fmt.Sprintf("k-%d", i),
		})
	}
	ctx := context.Background()
	if err := c.Flush(ctx); err == nil {
		t.Fatal("Flush should fail on the second split batch")
	}
	if c.Shipped() != 2 || len(c.buf) != 2 || c.buf[0].IdempotencyKey != "k-2" {
		t.Fatalf("Shipped()=%d, buffered=%v; want only the last two events buffered", c.Shipped(), len(c.buf))
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush retained events: %v", err)
	}
	if c.Shipped() != 4 || len(c.buf) != 0 {
		t.Fatalf("Shipped()=%d, buffered=%d; want 4 shipped and none buffered", c.Shipped(), len(c.buf))
	}
	if got := rs.batchSizes(); fmt.Sprint(got) != "[2 2 2]" {
		t.Errorf("batch sizes = %v, want [2 2 2]", got)
	}
}

func TestClient_SingleOversizeEventStopsAndRetains(t *testing.T) {
	server, _ := realTierdServer(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, Config{})
	ctx := context.Background()
	ev := testEvent("single-oversize")
	ev.Developer = strings.Repeat("x", maxBatchBody+1)
	if err := c.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	want := c.buf[0]
	err := c.Flush(ctx)
	if err == nil || !strings.Contains(err.Error(), "server returned 400") || !strings.Contains(err.Error(), "body exceeds") {
		t.Errorf("Flush = %v, want server's 400 body exceeds error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	if got := c.Shipped(); got != 0 {
		t.Errorf("Shipped() = %d, want 0", got)
	}
	if len(c.buf) != 1 || c.buf[0] != want {
		t.Errorf("oversized event was not retained unchanged; buffered=%d", len(c.buf))
	}
}
