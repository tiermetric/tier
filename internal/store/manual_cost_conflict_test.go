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

// rowsInWindow lists every token_events row stamped within an hour of now.
// Bounds are UTC: a local-zone bound mis-windows the UTC-stored ts string (the
// SQLite timestamp keyset hazard).
func rowsInWindow(t *testing.T, db *DB) []TokenEvent {
	t.Helper()
	now := time.Now().UTC()
	events, _, err := db.ListTokenEvents(context.Background(), now.Add(-time.Hour), now.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	return events
}

// TestInsertManualCostEvent_IdenticalCostLeavesCountsUnchanged pins #871's
// second half: a same-identity, same-cost re-post under an existing key is a
// no-op on EVERY column, token counts included, as docs/api-compatibility.md
// states. Before #871 it fell through to the shared upsert, whose MAX-merge
// promoted input_tok 100 -> 900.
func TestInsertManualCostEvent_IdenticalCostLeavesCountsUnchanged(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	small := manualCostEvent("k1", 10_500)
	small.InputTok = 100
	if err := db.InsertManualCostEvent(ctx, small); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	big := manualCostEvent("k1", 10_500) // same key, identity and cost; larger counts
	big.InputTok = 900
	big.OutputTok = 9_000
	if err := db.InsertManualCostEvent(ctx, big); err != nil {
		t.Fatalf("same-identity re-post: err = %v, want nil (idempotent)", err)
	}

	events := rowsInWindow(t, db)
	if len(events) != 1 {
		t.Fatalf("rows = %d, want 1", len(events))
	}
	if events[0].InputTok != 100 || events[0].OutputTok != small.OutputTok || events[0].CostMicro != 10_500 {
		t.Errorf("stored row = input %d output %d cost %d, want input 100 output %d cost 10_500 (a re-post must not change the row)",
			events[0].InputTok, events[0].OutputTok, events[0].CostMicro, small.OutputTok)
	}
}

// TestInsertManualCostEvent_KeyCollisionAcrossIdentitiesWritesNothing is #871's
// measured scenario at the store: bob's captured jsonl row owns msg-abc at
// $0.001 with input_tok 10; alice's manual post reuses msg-abc at the SAME cost
// with input_tok 9,000,000. Before #871 the cost-only pre-check fell through to
// the upsert: bob's input_tok became 9,000,000 and the call returned nil.
func TestInsertManualCostEvent_KeyCollisionAcrossIdentitiesWritesNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	bob := TokenEvent{
		Developer: "bob", IssueID: "issue-7", Model: "claude-sonnet-4",
		InputTok: 10, OutputTok: 5, CostMicro: 1_000,
		Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "msg-abc",
		Timestamp: time.Now().UTC().Truncate(time.Second),
	}
	if err := db.InsertTokenEvent(ctx, bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	alice := manualCostEvent("msg-abc", 1_000)
	alice.InputTok = 9_000_000
	if err := db.InsertManualCostEvent(ctx, alice); !errors.Is(err, ErrCostCorrectionIdentityMismatch) {
		t.Fatalf("colliding post: err = %v, want ErrCostCorrectionIdentityMismatch", err)
	}

	events := rowsInWindow(t, db)
	if len(events) != 1 {
		t.Fatalf("rows = %+v, want only bob's", events)
	}
	if got := events[0]; got.Developer != "bob" || got.InputTok != 10 || got.OutputTok != 5 || got.CostMicro != 1_000 {
		t.Errorf("bob's row = %+v, want it unchanged (input 10, output 5, cost 1000)", got)
	}
}

// TestInsertManualCostEvent_EveryIdentityColumnIsCompared pins that each member
// of costRowIdentity, alone, is enough to refuse a same-cost collision — so
// dropping any one from the comparison fails a named row here.
func TestInsertManualCostEvent_EveryIdentityColumnIsCompared(t *testing.T) {
	cases := map[string]func(*TokenEvent){
		"developer": func(e *TokenEvent) { e.Developer = "mallory" },
		"issue_id":  func(e *TokenEvent) { e.IssueID = "issue-99" },
		"model":     func(e *TokenEvent) { e.Model = "claude-opus-4" },
		"source":    func(e *TokenEvent) { e.Source = "jsonl" },
		"fidelity":  func(e *TokenEvent) { e.Fidelity = "daily" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()

			if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			other := manualCostEvent("k1", 10_500)
			other.InputTok = 7_777
			mutate(&other)
			if err := db.InsertManualCostEvent(ctx, other); !errors.Is(err, ErrCostCorrectionIdentityMismatch) {
				t.Fatalf("%s differs: err = %v, want ErrCostCorrectionIdentityMismatch", name, err)
			}
			if events := rowsInWindow(t, db); len(events) != 1 || events[0].InputTok != 1000 {
				t.Errorf("rows = %+v, want the seeded row alone with input_tok 1000", events)
			}
		})
	}
}

// TestInsertManualCostEvent_DivergentCostOutranksIdentity pins the order: a
// collision whose cost AND identity both differ is ErrCostConflict, whose 409
// body carries the #860 remedy for a non-owner (the identity 409 names none).
func TestInsertManualCostEvent_DivergentCostOutranksIdentity(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("k1", 10_500)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	other := manualCostEvent("k1", 20_600)
	other.Developer = "bob"
	if err := db.InsertManualCostEvent(ctx, other); !errors.Is(err, ErrCostConflict) {
		t.Fatalf("err = %v, want ErrCostConflict", err)
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
