package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestTokenEvent_SessionIDRoundTrip proves the (#238) session_id column persists
// on insert and reads back through ListTokenEvents. Session identity is the only
// grouping key for the context-bloat / rework-loop token-waste signatures, so a
// dropped value silently destroys the diagnostic grain.
func TestTokenEvent_SessionIDRoundTrip(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice",
		IssueID:   "issue-238",
		Model:     "claude-sonnet-4",
		InputTok:  1000,
		CostMicro: 12_345,
		Source:    "jsonl",
		Fidelity:  "realtime",
		SessionID: "sess-abc",
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", events[0].SessionID, "sess-abc")
	}
}

// TestTokenEvent_EmptySessionIDStoresNULL proves a producer that cannot know the
// session (proxy / poller rows) leaves session_id NULL rather than empty-string —
// provenance-honest, mirroring the fidelity/idempotency_key discipline (#238).
// A NULL leading a future grouping index also stays DISTINCT under SQLite, so the
// distinction is load-bearing, not cosmetic.
func TestTokenEvent_EmptySessionIDStoresNULL(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "bob",
		IssueID:   "issue-238",
		Model:     "claude-sonnet-4",
		InputTok:  500,
		CostMicro: 6_000,
		Source:    "proxy",
		Fidelity:  "realtime",
		// SessionID deliberately unset.
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	var stored sql.NullString
	if err := db.db.QueryRowContext(ctx,
		`SELECT session_id FROM token_events WHERE developer = 'bob'`).Scan(&stored); err != nil {
		t.Fatalf("select session_id: %v", err)
	}
	if stored.Valid {
		t.Errorf("session_id = %q (valid), want SQL NULL for a session-blind producer", stored.String)
	}

	events, _, err := db.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty (NULL COALESCEd)", events[0].SessionID)
	}
}

// TestTokenEvent_SessionIDFirstWriterWins proves session_id is INSERT-only and
// deliberately absent from the ON CONFLICT DO UPDATE set (#238): a replay of the
// same idempotency key by a session-BLIND producer (empty SessionID) must NOT
// overwrite the session the first writer stamped. Session identity is the grouping
// key #238 exists to preserve, so a later NULL/"" downgrade would destroy exactly
// the diagnostic grain we captured. This guards against a future refactor that
// carelessly adds `session_id = excluded.session_id` to the DO UPDATE clause.
func TestTokenEvent_SessionIDFirstWriterWins(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	const key = "msg-first-writer-wins"

	// First writer: the JSONL collector, which knows the session.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "carol",
		IssueID:        "issue-238",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		CostMicro:      10_000,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		SessionID:      "sess-x",
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (first writer): %v", err)
	}

	// Session-blind replay: SAME key, empty SessionID (as a proxy re-post would
	// arrive). The MAX-on-conflict upsert must fire on the token fields but leave
	// session_id untouched — session_id is not in the DO UPDATE set.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "carol",
		IssueID:        "issue-238",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		CostMicro:      10_000,
		Source:         "proxy",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		// SessionID deliberately empty — the session-blind replay.
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (session-blind replay): %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (replay must upsert, not insert a second row)", len(events))
	}
	if events[0].SessionID != "sess-x" {
		t.Errorf("SessionID = %q after session-blind replay, want %q preserved (first-writer-wins)", events[0].SessionID, "sess-x")
	}
}

// TestTokenEvent_SessionIDColumnPresent asserts the migration adds session_id on a
// freshly-opened DB, mirroring the columnExists checks the other schema migrations
// carry in store_test.go (#238).
func TestTokenEvent_SessionIDColumnPresent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	exists, err := columnExists(db.db, "token_events", "session_id")
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if !exists {
		t.Error("token_events.session_id column missing after Open")
	}
}
