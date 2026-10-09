package store

import (
	"context"
	"testing"

	"github.com/tiermetric/tier/internal/repoid"
)

// TestSnapshot_ReportDigestsAndExclusionSurviveARepoRepair pins that a
// Snapshot's ReportDigests and UnqualifiedExclusionWindow read one database
// state across a repo repair committing between them (#1038). The repair moves
// the one repo-blind row into the scoped repository, so in one state the row is
// counted in exactly one of the two sets.
func TestSnapshot_ReportDigestsAndExclusionSurviveARepoRepair(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	at := digestSince.AddDate(0, 0, 3)
	seedDigestScopedEvent(t, db, string(scopeBeta), "bob", 1_000, at, "k-beta")
	seedDigestScopedEvent(t, db, "", "carol", 2_000, at, "k-blind")

	var id int64
	if err := db.db.QueryRowContext(ctx, `SELECT id FROM token_events WHERE repo = ?`, repoid.Unqualified).Scan(&id); err != nil {
		t.Fatalf("find the repo-blind row: %v", err)
	}
	repair := func(from, to string) {
		t.Helper()
		res, err := db.db.ExecContext(ctx, repairRepoUpdateSQL, to, id, from)
		if err != nil {
			t.Fatalf("repair %s -> %s: %v", from, to, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("repair %s -> %s moved %d row(s), want 1", from, to, n)
		}
	}

	// Control: the two reads on the pool, the repair between them. The row is
	// counted in neither set — the torn pairing a caller gets from two pool reads —
	// which proves the interleave lands.
	events, _, err := db.ReportDigests(ctx, digestSince, digestUntil, scopeBeta)
	if err != nil {
		t.Fatalf("ReportDigests: %v", err)
	}
	repair(repoid.Unqualified, string(scopeBeta))
	ex, err := db.UnqualifiedExclusionWindow(ctx, digestSince, digestUntil)
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow: %v", err)
	}
	if events.Rows != 1 || ex.TokenEvents != 0 {
		t.Fatalf("control: two independent reads around a repair read (scoped rows, excluded rows) = (%d, %d), "+
			"want (1, 0) — the repair did not land between them, so the snapshot arm below proves nothing",
			events.Rows, ex.TokenEvents)
	}
	repair(string(scopeBeta), repoid.Unqualified)

	// One snapshot, the same repair between the same two reads. The repair runs on
	// a second pooled connection while the snapshot holds one: needs maxOpenConns > 1.
	err = db.ReadSnapshot(ctx, func(snap *Snapshot) error {
		var err error
		if events, _, err = snap.ReportDigests(ctx, digestSince, digestUntil, scopeBeta); err != nil {
			return err
		}
		repair(repoid.Unqualified, string(scopeBeta))
		ex, err = snap.UnqualifiedExclusionWindow(ctx, digestSince, digestUntil)
		return err
	})
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if events.Rows != 1 || ex.TokenEvents != 1 {
		t.Errorf("snapshot read (scoped rows, excluded rows) = (%d, %d), want (1, 1), the state before the "+
			"repair committed", events.Rows, ex.TokenEvents)
	}
	// Control: the repair committed while the snapshot was open.
	after, err := db.UnqualifiedExclusionWindow(ctx, digestSince, digestUntil)
	if err != nil {
		t.Fatalf("UnqualifiedExclusionWindow after: %v", err)
	}
	if after.TokenEvents != 0 {
		t.Fatalf("control: %d repo-blind rows after the repair, want 0", after.TokenEvents)
	}
}
