package anthropicadmin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// TestPoll_RealStoreSubtractsOpencodeRows pins #875 through the real store: an
// Opencode per-call row on a polled day is subtracted from the Admin aggregate,
// so the remainder event carries admin − opencode, not the full aggregate. The
// fakeStore tests cannot catch this — the source list lives in the store's SQL.
func TestPoll_RealStoreSubtractsOpencodeRows(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("claude-sonnet-4", 1000, 200, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, usage))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "poller.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
		InputTok: 400, OutputTok: 50, CostMicro: 1, Host: "anthropic",
		Source: collector.SourceOpencode, Fidelity: collector.FidelityRealtime,
		Timestamp:      time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
		IdempotencyKey: "opencode-row-1",
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	poller := NewPoller(PollerConfig{
		Client:   newTestClient(t, srv, nil),
		Store:    db,
		Org:      "acme",
		Interval: time.Hour,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return fixedNow },
	})
	ing := &recordingIngester{}
	if err := poller.pollOnce(ctx, ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 1 {
		t.Fatalf("got %d events, want 1 remainder event", len(ing.events))
	}
	if ev := ing.events[0]; ev.InputTok != 600 || ev.OutputTok != 150 {
		t.Errorf("remainder = %d in / %d out, want 600 / 150 (admin 1000/200 minus the opencode row's 400/50); "+
			"the full aggregate means opencode is missing from the subtraction baseline and is counted twice (#875)",
			ev.InputTok, ev.OutputTok)
	}
}
