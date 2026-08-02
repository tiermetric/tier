package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// manualCostEvent builds a /costs-shaped TokenEvent with the given key and
// cost_micro. Source/fidelity mirror the manual-import surface's defaults.
func manualCostEvent(key string, costMicro int64) TokenEvent {
	return TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		OutputTok:      500,
		CostMicro:      costMicro,
		Source:         "api",
		Fidelity:       "estimated",
		IdempotencyKey: key,
		Timestamp:      time.Now().UTC().Truncate(time.Second),
	}
}

func onlyCostMicro(t *testing.T, db *DB) int64 {
	t.Helper()
	costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("expected exactly 1 developer cost row, got %d (%+v)", len(costs), costs)
	}
	return costs[0].TotalCostMicro
}

// TestInsertManualCostEvent_DivergentCostConflicts is the #295 store contract:
// a keyed re-post with a DIFFERENT cost_micro returns ErrCostConflict and leaves
// the stored row untouched (cost_micro is immutable per #233).
func TestInsertManualCostEvent_DivergentCostConflicts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 20_600))
	if !errors.Is(err, ErrCostConflict) {
		t.Fatalf("divergent re-post err = %v, want ErrCostConflict", err)
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 (a conflict must not mutate the row)", got)
	}
}

// TestInsertManualCostEvent_IdenticalCostIsIdempotent confirms a matching-cost
// re-post is a no-op (no error, no second row, no double-count).
func TestInsertManualCostEvent_IdenticalCostIsIdempotent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
			t.Fatalf("insert #%d: %v", i, err)
		}
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 (identical re-post must not double-count)", got)
	}
}

// TestInsertManualCostEvent_IdenticalCostMaxMergesCounts pins that the
// same-cost idempotent path still runs the ON CONFLICT DO UPDATE MAX-merge on
// token counts (not just cost immutability): a re-post with the SAME key and
// SAME cost but LARGER input_tok promotes the stored count. This proves the
// divergence guard falls through to the shared upsert, rather than short-
// circuiting to a bare no-op, on an identical-cost re-post.
func TestInsertManualCostEvent_IdenticalCostMaxMergesCounts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// One UTC instant drives both the stored ts and the read window: modernc
	// renders a bound time.Time with its zone offset and compares it lexically
	// against the UTC-stored ts string, so a local-zone bound would mis-window the
	// keyset range (the documented SQLite timestamp keyset hazard).
	now := time.Now().UTC().Truncate(time.Second)
	small := manualCostEvent("k1", 10_500)
	small.InputTok = 100
	small.Timestamp = now
	if err := db.InsertManualCostEvent(ctx, small); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	big := manualCostEvent("k1", 10_500) // same cost, larger counts
	big.InputTok = 900
	big.Timestamp = now
	if err := db.InsertManualCostEvent(ctx, big); err != nil {
		t.Fatalf("second (larger-count) insert: %v", err)
	}

	// cost stays immutable; the MAX-merge is verified at the SQL level here via a
	// direct read of the promoted input_tok through the events listing.
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 (cost immutable across the merge)", got)
	}
	events, _, err := db.ListTokenEvents(ctx, now.Add(-time.Hour), now.Add(time.Hour), PageCursor{}, 10)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 row after idempotent merge, got %d", len(events))
	}
	if events[0].InputTok != 900 {
		t.Errorf("input_tok = %d, want 900 (MAX-merge must promote the larger count)", events[0].InputTok)
	}
}

// TestInsertManualCostEvent_UnkeyedNeverConflicts confirms the empty-key path
// bypasses the guard entirely: two unkeyed posts with different costs both land
// (they cannot collide on the partial unique index), summing in the aggregate.
func TestInsertManualCostEvent_UnkeyedNeverConflicts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("", 10_500)); err != nil {
		t.Fatalf("first unkeyed insert: %v", err)
	}
	if err := db.InsertManualCostEvent(ctx, manualCostEvent("", 20_600)); err != nil {
		t.Fatalf("second unkeyed insert: %v", err)
	}
	if got := onlyCostMicro(t, db); got != 31_100 {
		t.Errorf("summed cost_micro = %d, want 31_100 (two unkeyed rows must both persist)", got)
	}
}

// TestInsertManualCostEvent_NewKeyInserts confirms a brand-new key is a plain
// first-writer insert with no error.
func TestInsertManualCostEvent_NewKeyInserts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("fresh", 10_500)); err != nil {
		t.Fatalf("new-key insert: %v", err)
	}
	if got := onlyCostMicro(t, db); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500", got)
	}
}
