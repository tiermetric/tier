package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Tests that pin the POOL ITSELF (#669).
//
// 🔴 WHY THIS FILE EXISTS. Before it, nothing in the tree failed if maxOpenConns
// was reverted to 1, raised to 64, or if SetMaxIdleConns was deleted outright —
// the constant's entire purpose (latency isolation, measured 1.450s -> 627µs) had
// zero regression protection, and reverting it would have made the suite MORE
// deterministic rather than less. A change whose whole value is a runtime
// property needs a test that observes that property, not just the number.

// TestOpenAppliesThePoolSettings pins what Open actually installs. It is the
// cheap arm: it cannot tell you the pool WORKS, only that the settings reached
// the *sql.DB — which is exactly the mutant "someone deleted SetMaxIdleConns"
// that the behavioural test below cannot see.
func TestOpenAppliesThePoolSettings(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	st := db.db.Stats()
	if st.MaxOpenConnections != maxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want maxOpenConns (%d)", st.MaxOpenConnections, maxOpenConns)
	}
	// ⚠️ A DESIGN-DECISION GUARD, AND HONESTLY LABELLED AS ONE: it is not evidence
	// that the value is optimal, and it cannot be. It exists because the
	// behavioural test below passes at ANY pool above 1 — measured, a mutant
	// raising this to 64 survived it — so nothing otherwise stops the constant
	// drifting upward. It should not: modernc.org/sqlite reads are pure-Go and
	// CPU-bound, so N above the core count buys contention rather than
	// concurrency (measured read throughput at 4 was only 1.09x that of 1), and
	// each connection is a live fd on the DB plus its -wal/-shm sidecars. Raising
	// this past the band is a deliberate act that should come with a fresh
	// measurement — so it must edit this line and read maxOpenConns' doc comment.
	if maxOpenConns <= 1 || maxOpenConns > 8 {
		t.Errorf("maxOpenConns = %d, outside the deliberated 2..8 band — 1 reinstates the connection-acquisition starvation #669 fixed, and a large pool adds CPU contention on a pure-Go driver without adding read parallelism. Re-measure before widening this band", maxOpenConns)
	}
	// 🔴 IDLE MUST EQUAL MAX, and nothing else observes this. database/sql
	// defaults MaxIdleConns to 2; at maxOpenConns=4 that silently destroys and
	// rebuilds two connections continuously, each rebuild re-running the DSN
	// pragmas with a cold page cache in front of a 138ms scan. There is no
	// Stats() field for MaxIdleConns, so this is measured by behaviour: open
	// maxOpenConns connections at once, release them all, and require that they
	// are all still idle afterwards rather than having been closed on release.
	conns := make([]*sql.Conn, 0, maxOpenConns)
	ctx := context.Background()
	for i := 0; i < maxOpenConns; i++ {
		c, err := db.db.Conn(ctx)
		if err != nil {
			t.Fatalf("reserve conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Fatalf("release conn: %v", err)
		}
	}
	if st := db.db.Stats(); st.Idle != maxOpenConns {
		t.Errorf("after releasing %d connections, Idle = %d (MaxIdleConns is capping it) — want %d. SetMaxIdleConns must equal maxOpenConns, or every request past the idle limit pays a reconnect plus a cold page cache", maxOpenConns, st.Idle, maxOpenConns)
	}
}

// TestPoolIsolatesAWriterFromAnInFlightRead is the BEHAVIOURAL pin, and it is the
// one that would fail on a revert to 1.
//
// 🔴 IT ASSERTS THE PROPERTY, NOT THE NUMBER. #669's justification is that a
// request-path writer used to block at CONNECTION ACQUISITION rather than at the
// write lock: a reader holding the process's only connection starved a concurrent
// bounded write for 1.450s against a 250ms bound. MEASURED 2026-08-13 against
// this implementation — pool of 1: 1.4505s; pool of maxOpenConns: 627µs.
//
// ⚠️ THE CONTROL IS THE HALF THAT MAKES IT MEAN ANYTHING. "The write returned
// fast" is ALSO what an uncontended write looks like, so a fixture whose reader
// had quietly failed to hold anything would pass. Arm 1 pins the pool to 1 and
// REQUIRES the starvation to reproduce; only then is arm 2's fast return
// evidence. If arm 1 ever stops starving, this test fails there rather than
// silently downgrading arm 2 into a tautology.
func TestPoolIsolatesAWriterFromAnInFlightRead(t *testing.T) {
	t.Parallel()

	// A read held long enough to dominate any scheduling noise, and far longer
	// than requestPathBusyTimeout so a starved write is unambiguous.
	const readHold = 1500 * time.Millisecond

	measure := func(t *testing.T, poolSize int) time.Duration {
		t.Helper()
		db, err := Open(filepath.Join(t.TempDir(), "isolation.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		db.db.SetMaxOpenConns(poolSize)
		db.db.SetMaxIdleConns(poolSize)

		ctx := context.Background()
		// Seed, so the reader's scan matches rows. A window that matches NOTHING
		// is the false green that broke the first #669 read probe: it timed an
		// empty scan and reported the opposite conclusion.
		for i := 0; i < 200; i++ {
			if _, err := db.db.ExecContext(ctx,
				`INSERT INTO token_events (developer, issue_id, model, input_tok, output_tok, cost_micro, source, fidelity, ts)
				 VALUES ('seed', 'ISSUE-1', 'claude-opus-4', 10, 10, 100, 'jsonl', 'exact', datetime('now'))`); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}

		readerHolding := make(chan struct{})
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			tx, err := db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				close(readerHolding)
				return
			}
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_events`).Scan(&n); err != nil || n == 0 {
				t.Errorf("FIXTURE: the reader scanned %d rows (err %v) — a read matching nothing holds no snapshot and starves nobody", n, err)
			}
			close(readerHolding)
			time.Sleep(readHold)
			_ = tx.Rollback()
		}()
		<-readerHolding
		// Let the reader settle into its hold before timing the write.
		time.Sleep(50 * time.Millisecond)

		start := time.Now()
		err = db.UpsertHierarchy(ctx, "alice", "core", "platform", "acme")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("bounded write at pool=%d: %v", poolSize, err)
		}
		<-readerDone
		return elapsed
	}

	// ARM 1 — CONTROL. At a pool of 1 the write MUST starve behind the reader.
	starved := measure(t, 1)
	if starved < readHold/2 {
		t.Fatalf("CONTROL DID NOT STARVE: the bounded write took %v at a pool of 1, but the reader holds its connection for %v. The fixture is not reproducing connection-acquisition starvation, so arm 2's fast return would prove nothing", starved, readHold)
	}

	// ARM 2 — the property under test.
	isolated := measure(t, maxOpenConns)
	if isolated > readHold/2 {
		t.Errorf("the bounded write took %v at maxOpenConns=%d while a %v read was in flight — it is still blocking at CONNECTION ACQUISITION, which is the whole defect #669 fixed (control at a pool of 1: %v). Did maxOpenConns get reverted to 1?", isolated, maxOpenConns, readHold, starved)
	}
	t.Logf("write under an in-flight %v read: pool=1 -> %v (starved, control), pool=%d -> %v", readHold, starved, maxOpenConns, isolated)
}
