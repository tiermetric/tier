package store

import (
	"context"
	"testing"
	"time"
)

// insertCostEvent is a small helper: one priced token event at ts with the given
// key so repeated inserts don't collapse on the idempotency index.
func insertCostEvent(t *testing.T, db *DB, key string, cost int64, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-1",
		Model:          "claude-sonnet-4",
		InputTok:       100,
		OutputTok:      50,
		CostMicro:      cost,
		Source:         "proxy",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(%s): %v", key, err)
	}
}

// TestWindowActivity_TripCondition is the core zero-outcome tripwire query (#189):
// cost accrued inside the window but zero outcomes recorded there. It also pins the
// boundary (an event/outcome exactly at the window edge counts) and the just-outside
// case (older cost does not).
func TestWindowActivity_TripCondition(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-7 * 24 * time.Hour)

	// Empty DB: no cost, no outcomes -> not tripped.
	if a, err := db.WindowActivity(ctx, since); err != nil {
		t.Fatalf("WindowActivity(empty): %v", err)
	} else if a.CostMicro != 0 || a.Outcomes != 0 {
		t.Fatalf("empty DB: got %+v, want zero cost and zero outcomes", a)
	}

	// Cost inside the window, plus one event exactly at the lower bound (included
	// by ts >= since) and one one-second before it (excluded).
	insertCostEvent(t, db, "in-window", 500_000, now.Add(-time.Hour))
	insertCostEvent(t, db, "at-bound", 250_000, since)                       // boundary: included
	insertCostEvent(t, db, "just-outside", 999_000, since.Add(-time.Second)) // excluded

	a, err := db.WindowActivity(ctx, since)
	if err != nil {
		t.Fatalf("WindowActivity: %v", err)
	}
	if a.CostMicro != 750_000 {
		t.Errorf("CostMicro = %d, want 750_000 (in-window + at-bound, not just-outside)", a.CostMicro)
	}
	if a.Outcomes != 0 {
		t.Errorf("Outcomes = %d, want 0 (tripwire condition: cost but no outcomes)", a.Outcomes)
	}

	// Record an outcome inside the window -> condition clears.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice",
		IssueID:   "issue-1",
		Weight:    3,
		Quality:   1,
		Timestamp: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	a, err = db.WindowActivity(ctx, since)
	if err != nil {
		t.Fatalf("WindowActivity after outcome: %v", err)
	}
	if a.Outcomes != 1 {
		t.Errorf("Outcomes = %d, want 1 after recording an in-window outcome", a.Outcomes)
	}
}

// TestWindowActivity_OutcomeOutsideWindowStillTrips: an outcome that landed BEFORE
// the window does not clear the tripwire — the point is fresh cost with no fresh
// outcome (a webhook that stopped delivering days ago).
func TestWindowActivity_OutcomeOutsideWindowStillTrips(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-7 * 24 * time.Hour)

	insertCostEvent(t, db, "fresh-cost", 400_000, now.Add(-time.Hour))
	// Outcome older than the window (10 days ago).
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-9", Weight: 1, Quality: 1,
		Timestamp: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	a, err := db.WindowActivity(ctx, since)
	if err != nil {
		t.Fatalf("WindowActivity: %v", err)
	}
	if a.CostMicro <= 0 || a.Outcomes != 0 {
		t.Errorf("got %+v, want cost>0 and Outcomes=0 (stale outcome must not clear the tripwire)", a)
	}
}
