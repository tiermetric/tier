package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestTokenEvent_PriceVersionColumnPresent asserts the migration adds the
// price_version provenance column (#233) on a freshly-opened DB, mirroring the
// columnExists checks the other schema migrations carry (session_id, repo).
func TestTokenEvent_PriceVersionColumnPresent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	exists, err := columnExists(db.db, "token_events", "price_version")
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if !exists {
		t.Error("token_events.price_version column missing after Open")
	}
}

// TestTokenEvent_PriceVersionStampedFromActiveTable proves a normal insert
// stamps the row with the active price-table version (#233) even when the caller
// leaves PriceVersion unset — the store defaults it to the table that priced the
// event. Without a stamp, cross-version drift is unauditable.
func TestTokenEvent_PriceVersionStampedFromActiveTable(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	want := ActivePriceTableInfo().Version
	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice",
		IssueID:   "issue-233",
		Model:     "claude-sonnet-4",
		InputTok:  1000,
		CostMicro: 12_345,
		Source:    "jsonl",
		Fidelity:  "realtime",
		// PriceVersion deliberately unset — the store stamps the active version.
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
	if events[0].PriceVersion != want {
		t.Errorf("PriceVersion = %d, want active table version %d", events[0].PriceVersion, want)
	}
}

// TestTokenEvent_CostMicroImmutableOnConflict is the core #233 regression: once a
// row is priced under the server's table, a replay of the SAME idempotency key must
// NOT re-price it upward. On current main the ON CONFLICT clause runs
// cost_micro = MAX(cost_micro, excluded.cost_micro), so any later binary shipping a
// higher price silently ratchets stored history up. The fix drops cost_micro from
// the MAX set (immutable / DO NOTHING for cost). Token COUNTS still take MAX for the
// placeholder-promotion edge (store.go's per-message rationale).
func TestTokenEvent_CostMicroImmutableOnConflict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)
	const key = "msg-cost-immutable"

	// First writer prices the event under the active table.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "carol",
		IssueID:        "issue-233",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		CostMicro:      10_000,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (first writer): %v", err)
	}

	// A newer binary re-ships the SAME event priced HIGHER, with larger token
	// counts (the placeholder-promotion edge the token-count MAX legitimately serves).
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "carol",
		IssueID:        "issue-233",
		Model:          "claude-sonnet-4",
		InputTok:       2000,
		CostMicro:      99_000, // a higher-priced table
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (reprice replay): %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (replay must upsert, not insert a second row)", len(events))
	}
	if events[0].CostMicro != 10_000 {
		t.Errorf("CostMicro = %d after higher-priced replay, want 10000 immutable (no silent reprice)", events[0].CostMicro)
	}
	// The token-count MAX edge is preserved: the larger count promotes the placeholder.
	if events[0].InputTok != 2000 {
		t.Errorf("InputTok = %d, want 2000 (token counts still take MAX for placeholder promotion)", events[0].InputTok)
	}
}

// TestTokenEvent_PriceVersionFirstWriterWins proves price_version is INSERT-only and
// absent from the ON CONFLICT DO UPDATE set (#233): a replay must not restamp the
// provenance version. The version records which table priced the (immutable) cost,
// so it must move only when the cost does — i.e. never on replay.
func TestTokenEvent_PriceVersionFirstWriterWins(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := time.Date(2026, 7, 9, 16, 0, 0, 0, time.UTC)
	const key = "msg-priceversion-first-writer"

	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "dave",
		IssueID:        "issue-233",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		CostMicro:      10_000,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		PriceVersion:   3,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (first writer): %v", err)
	}
	// Replay stamping a DIFFERENT version — must not overwrite.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "dave",
		IssueID:        "issue-233",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		CostMicro:      10_000,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		PriceVersion:   99,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (replay): %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].PriceVersion != 3 {
		t.Errorf("PriceVersion = %d after replay, want 3 preserved (first-writer-wins)", events[0].PriceVersion)
	}
}

// TestTokenEvent_PriceVersionBackfillsLegacyRows proves the one-shot migration
// stamps pre-#233 rows (price_version 0) with the currently-active table version —
// honest, since that is the table they were last priced under. Simulated by writing
// a row, zeroing its price_version and deleting the migration marker, then reopening.
func TestTokenEvent_PriceVersionBackfillsLegacyRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backfill.db")
	ctx := context.Background()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ts := time.Date(2026, 7, 9, 17, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "erin", IssueID: "issue-233", Model: "claude-sonnet-4",
		InputTok: 1000, CostMicro: 10_000, Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: "legacy-key", Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
	// Force the legacy shape: zero the stamp and clear the backfill marker so the
	// next Open re-runs it.
	if _, err := db.db.ExecContext(ctx, `UPDATE token_events SET price_version = 0`); err != nil {
		t.Fatalf("zero price_version: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `DELETE FROM tier_migrations WHERE name = ?`, migrationBackfillPriceVersion); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()

	events, _, err := db2.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if want := ActivePriceTableInfo().Version; events[0].PriceVersion != want {
		t.Errorf("PriceVersion = %d after backfill, want active version %d", events[0].PriceVersion, want)
	}
}

// insertPricedEvent writes a token_event at an explicit price_version and ts,
// unkeyed (no idempotency key) so successive inserts never collide — the helper
// behind the DistinctPriceVersionsWindow tests (#293).
func insertPricedEvent(t *testing.T, db *DB, issue string, version int, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer:    "alice",
		IssueID:      issue,
		Model:        "claude-sonnet-4",
		InputTok:     1000,
		CostMicro:    10_000,
		Source:       "jsonl",
		Fidelity:     "realtime",
		PriceVersion: version,
		Timestamp:    ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (v%d): %v", version, err)
	}
}

// TestDistinctPriceVersionsWindow is the #293 store read behind the mixed-version
// data_quality WARN. It pins: a window spanning multiple price_table versions
// returns them ascending + deduplicated; a single-version window returns exactly
// one; an empty window returns nil; the half-open [since, until) bound excludes
// rows at/after `until`; and the unstamped sentinel 0 is filtered out so a stray
// pre-#233 row cannot fabricate a "mixed" signal.
func TestDistinctPriceVersionsWindow(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	t.Run("multiple versions sorted and deduped", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		// Insert out of order and with a duplicate version to prove ORDER BY + DISTINCT.
		insertPricedEvent(t, db, "issue-a", 2, base)
		insertPricedEvent(t, db, "issue-b", 1, base.Add(time.Minute))
		insertPricedEvent(t, db, "issue-c", 2, base.Add(2*time.Minute)) // dup of 2

		got, err := db.DistinctPriceVersionsWindow(ctx, base.Add(-time.Hour), base.Add(time.Hour), FleetWide)
		if err != nil {
			t.Fatalf("DistinctPriceVersionsWindow: %v", err)
		}
		want := []int{1, 2}
		if !equalInts(got, want) {
			t.Errorf("versions = %v, want %v (ascending, deduped)", got, want)
		}
	})

	t.Run("single version", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		insertPricedEvent(t, db, "issue-a", 7, base)
		insertPricedEvent(t, db, "issue-b", 7, base.Add(time.Minute))

		got, err := db.DistinctPriceVersionsWindow(ctx, base.Add(-time.Hour), base.Add(time.Hour), FleetWide)
		if err != nil {
			t.Fatalf("DistinctPriceVersionsWindow: %v", err)
		}
		if want := []int{7}; !equalInts(got, want) {
			t.Errorf("versions = %v, want %v", got, want)
		}
	})

	t.Run("empty window returns nil", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		// One event well outside the queried window.
		insertPricedEvent(t, db, "issue-a", 3, base)

		got, err := db.DistinctPriceVersionsWindow(ctx, base.Add(24*time.Hour), base.Add(48*time.Hour), FleetWide)
		if err != nil {
			t.Fatalf("DistinctPriceVersionsWindow: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("versions = %v, want empty for a window with no rows", got)
		}
	})

	t.Run("half-open upper bound excludes until", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		insertPricedEvent(t, db, "issue-a", 5, base)                // in window
		insertPricedEvent(t, db, "issue-b", 6, base.Add(time.Hour)) // exactly at `until` → excluded

		got, err := db.DistinctPriceVersionsWindow(ctx, base, base.Add(time.Hour), FleetWide)
		if err != nil {
			t.Fatalf("DistinctPriceVersionsWindow: %v", err)
		}
		if want := []int{5}; !equalInts(got, want) {
			t.Errorf("versions = %v, want %v (row at `until` must be excluded by half-open bound)", got, want)
		}
	})

	t.Run("sentinel zero excluded", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		insertPricedEvent(t, db, "issue-a", 4, base)
		insertPricedEvent(t, db, "issue-z", 4, base.Add(time.Minute))
		// Force one row to the unstamped sentinel — a shape the backfill converts at
		// Open, but which must never masquerade as a distinct real pricing version.
		if _, err := db.db.ExecContext(ctx,
			`UPDATE token_events SET price_version = 0 WHERE issue_id = ?`, "issue-z"); err != nil {
			t.Fatalf("zero price_version: %v", err)
		}

		got, err := db.DistinctPriceVersionsWindow(ctx, base.Add(-time.Hour), base.Add(time.Hour), FleetWide)
		if err != nil {
			t.Fatalf("DistinctPriceVersionsWindow: %v", err)
		}
		if want := []int{4}; !equalInts(got, want) {
			t.Errorf("versions = %v, want %v (sentinel 0 must be filtered, not counted as a version)", got, want)
		}
	})
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
