package store

// Tests for beginImmediate — the honest BEGIN IMMEDIATE (#493, follow-up #598).
//
// These exist because the property they check is one the code CANNOT get from
// the driver. modernc.org/sqlite ignores sql.TxOptions.Isolation, so every
// BeginTx in this package is DEFERRED and sql.LevelSerializable is a no-op. Nine
// call sites in store.go asserted the opposite in their comments for months and
// nothing failed, because the then-current SetMaxOpenConns(1) hid it in-process
// (the pool is maxOpenConns since #669, which is exactly why those sites had to
// take the write lock up front first — see #668). A test that
// drives a SECOND connection is the only thing that can tell the two apart.
//
// Every test here is therefore two-armed: the positive arm proves beginImmediate
// takes the lock, and the control arm proves the same call WITHOUT the promotion
// does not — so a regression that quietly stops promoting cannot pass by
// coincidence.
//
// ⏱️ EACH TEST PAYS THE FULL busy_timeout (5000ms in this store's DSN) once, and
// that is not avoidable by shortening the context. MEASURED: beginImmediate under
// a 300ms context deadline still returns after 5.098s — modernc.org/sqlite's busy
// handler does not check ctx while it spins, so the deadline only changes the
// error TEXT ("context deadline exceeded" instead of "database is locked"), which
// makes the test assert on a failure mode production never sees. So they wait,
// and t.Parallel() overlaps the three waits into roughly one instead of three.
//
// t.Parallel() is safe here specifically: each test builds its own database under
// its own t.TempDir(), and none of them touch the package-global fixture counters
// the repair tests use.
//
// ⚠️ THAT IS NO LONGER THE WHOLE ARGUMENT. TestRepriceCommitTakesTheUnboundedWriteLock
// also READS the package-global price table (ActivePriceTableInfo, and
// ComputeCostHost via Reprice), which no earlier test in this file touched. It is
// still safe, but for a SECOND reason that must be stated or the next test added
// here will assume the per-test TempDir covers everything: nothing that MUTATES
// those globals runs in parallel — installPriceTable's callers are all serial (the
// only t.Parallel() in prices_test.go is inside a comment), and Go runs every
// serial test to completion before the parallel ones resume. installPriceTable
// documents that invariant from the writer's side; this is the reader's side of it.
// A future parallel test that installs a price table breaks BOTH.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// holdWriteLock opens a second handle to path and takes BEGIN IMMEDIATE on it,
// exactly as a live `tierd serve` ingest would. It returns a release func that
// rolls the holder back and closes both the connection and the handle.
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	holder, err := Open(path)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	conn, err := holder.db.Conn(context.Background())
	if err != nil {
		_ = holder.Close()
		t.Fatalf("grab holder conn: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		_ = holder.Close()
		t.Fatalf("holder BEGIN IMMEDIATE: %v", err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		_ = conn.Close()
		_ = holder.Close()
	}
}

// holdWriteLockRaw is holdWriteLock without Open(): it opens the file through
// the driver directly and takes BEGIN IMMEDIATE.
//
// 🔴 IT EXISTS BECAUSE Open() IS A WRITER. The migration-site test below works by
// DELETING tier_migrations marker rows to force the marker-gated backfills to run
// again; a lock holder that went through Open() would re-run those same backfills
// on its own handle and re-INSERT the markers, silently restoring the early
// return the test is trying to defeat. That failure would look like a pass.
func holdWriteLockRaw(t *testing.T, path string) (release func()) {
	t.Helper()
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw lock holder: %v", err)
	}
	conn, err := holder.Conn(context.Background())
	if err != nil {
		_ = holder.Close()
		t.Fatalf("grab raw holder conn: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		_ = holder.Close()
		t.Fatalf("raw holder BEGIN IMMEDIATE: %v", err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		_ = conn.Close()
		_ = holder.Close()
	}
}

// TestConvertedMigrationSitesTakeTheWriteLock pins the SEVEN Open()-time sites
// #598 converted to beginImmediate. Without it, every one of those conversions is
// an unguarded edit: reverting any single call site back to
// `db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})` compiles,
// keeps every other test in this tree green, and silently restores the DEFERRED
// behaviour — which is exactly how the wrong shape survived for months.
//
// 🔴 WHAT MAKES THIS DISCRIMINATE, AND IT IS NOT OBVIOUS. Each case runs against
// an ALREADY-MIGRATED database while another connection holds the write lock. In
// that state the migration has no rows to change, so:
//
//   - DEFERRED (the old shape): opens, reads, finds nothing to do, COMMITS —
//     returns nil. It never writes, so it never discovers the lock.
//   - beginImmediate (the new shape): fails at the promote, before the read.
//
// So "returns nil" is precisely the mutant's signature and "wraps
// ErrWriteLockUnavailable" is precisely the converted one's. A test that only
// asserted "no panic" or "some error" would not tell them apart.
//
// The marker-gated backfills are forced to reach their transaction by deleting
// their tier_migrations rows first; the others open their tx unconditionally.
func TestConvertedMigrationSitesTakeTheWriteLock(t *testing.T) {
	t.Parallel()

	// clearMarkers forces the marker-gated backfills (backfillPeriodMembership,
	// backfillPriceVersion, recomputeKnownSourceCosts) past their early return so
	// they actually reach the transaction under test.
	clearMarkers := func(t *testing.T, db *DB) {
		t.Helper()
		if _, err := db.db.Exec(`DELETE FROM tier_migrations`); err != nil {
			t.Fatalf("clear tier_migrations markers: %v", err)
		}
	}
	// legacyActualSpend reshapes actual_spend back to its pre-#24 form so
	// dropActualSpendNonNegativeCheck's rebuild path is entered rather than
	// early-returning on the absent CHECK. Only the CHECK text matters: the call
	// never gets past the promote, so the column list is irrelevant.
	//
	// ⚠️ ONLY that case uses it. migrateActualSpendToMicro calls addColumnIfMissing
	// BEFORE its transaction, and against this reshaped table that ALTER is a real
	// write — so it would fail on the lock at the ALTER and never reach the
	// promote, which reads as a failure of a conversion that is in fact fine.
	legacyActualSpend := func(t *testing.T, db *DB) {
		t.Helper()
		if _, err := db.db.Exec(`
			DROP TABLE actual_spend;
			CREATE TABLE actual_spend (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				developer       TEXT    NOT NULL,
				period          TEXT    NOT NULL,
				actual_paid_usd REAL    NOT NULL CHECK (actual_paid_usd >= 0),
				ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`); err != nil {
			t.Fatalf("reshape actual_spend to legacy form: %v", err)
		}
	}

	cases := []struct {
		name  string
		setup func(*testing.T, *DB)
		call  func(*sql.DB) error
	}{
		{"dropActualSpendNonNegativeCheck", legacyActualSpend, func(d *sql.DB) error {
			return dropActualSpendNonNegativeCheck(d, "actual_spend")
		}},
		{"migrateCacheWriteSplit", nil, migrateCacheWriteSplit},
		{"migrateCostUSDToMicro", nil, migrateCostUSDToMicro},
		{"migrateActualSpendToMicro", nil, func(d *sql.DB) error {
			return migrateActualSpendToMicro(d, "actual_spend")
		}},
		{"backfillPeriodMembership", clearMarkers, backfillPeriodMembership},
		{"backfillPriceVersion", clearMarkers, backfillPriceVersion},
		{"recomputeKnownSourceCosts", clearMarkers, recomputeKnownSourceCosts},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Each case gets its own database and its own lock holder. That is not
			// tidiness: every case blocks for the full busy_timeout, and only
			// separate files let t.Parallel() overlap those waits into roughly one
			// instead of seven. (Pre-#669 a shared handle would have queued them on
			// the single connection and serialised the waits anyway; at
			// maxOpenConns separate files are still the clean way to keep each
			// case's lock holder independent.)
			t.Parallel()
			path := filepath.Join(t.TempDir(), "migsite.db")
			db, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			if c.setup != nil {
				c.setup(t, db)
			}

			// 🔴 CONTROL ARM, LOCK FREE. The case must be REACHABLE and must not
			// report contention when there is none. Without this, a fixture change
			// that made the function early-return before its transaction would leave
			// the locked arm below passing for the wrong reason.
			if err := c.call(db.db); errors.Is(err, ErrWriteLockUnavailable) {
				t.Fatalf("control: reported write-lock contention with the lock FREE: %v — the fixture is broken, so the locked arm below proves nothing", err)
			}
			// The control run may have re-inserted markers or rebuilt the table.
			if c.setup != nil {
				c.setup(t, db)
			}

			release := holdWriteLockRaw(t, path)
			defer release()

			err = c.call(db.db)
			if err == nil {
				t.Fatalf("returned nil while another connection held the write lock — it is running as a DEFERRED transaction (it found nothing to write, so it never noticed the lock). This call site has lost its beginImmediate.")
			}
			if !errors.Is(err, ErrWriteLockUnavailable) {
				t.Errorf("error = %v, want it to wrap ErrWriteLockUnavailable — the failure must come from the promote, not from some later statement", err)
			}
		})
	}
}

// TestBeginImmediateTakesTheWriteLock is the whole point of the helper: the
// transaction must hold the write lock from its FIRST statement, so a
// read-then-write caller learns it lost the race up front rather than after a
// full scan (where SQLITE_BUSY_SNAPSHOT, which busy_timeout does not retry,
// throws the work away).
//
// 🔴 THE CONTROL ARM IS THE TEST. A plain BeginTx against the same locked
// database SUCCEEDS — that is the DEFERRED behaviour, and it is exactly what
// every `Isolation: sql.LevelSerializable` call site in store.go actually does.
// Without that arm this test would also pass against a helper that had stopped
// promoting entirely, because "BeginTx returned no error" proves nothing.
func TestBeginImmediateTakesTheWriteLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "immediate.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	release := holdWriteLock(t, path)
	defer release()

	ctx := context.Background()

	// CONTROL: DEFERRED opens happily while another connection holds the write
	// lock — including one passed LevelSerializable, which the driver ignores.
	for name, opts := range map[string]*sql.TxOptions{
		"BeginTx(nil)":               nil,
		"BeginTx(LevelSerializable)": {Isolation: sql.LevelSerializable},
	} {
		deferredTx, err := db.db.BeginTx(ctx, opts)
		if err != nil {
			t.Fatalf("control %s failed while the write lock was held: %v — the control must SUCCEED, otherwise the positive arm below proves nothing", name, err)
		}
		if err := deferredTx.Rollback(); err != nil {
			t.Fatalf("control %s rollback: %v", name, err)
		}
	}

	// POSITIVE: beginImmediate must NOT.
	tx, err := beginImmediate(ctx, db.db)
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("beginImmediate succeeded while another connection held the write lock — it is not promoting, so a read-then-write caller would discover the contention only at its first write")
	}
	// errors.Is, NOT strings.Contains. The sentinel is what lets a caller attach
	// the "is a tierd serve ingesting?" hint to CONTENTION only, and not to the
	// helper's other failure (BeginTx itself failing — a cancelled context, a dead
	// handle), where that hint names the wrong cause.
	if !errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("error = %v, want it to wrap ErrWriteLockUnavailable — callers gate an operator-facing contention hint on exactly this", err)
	}
	// 🔴 CONTROL: the sentinel must DISCRIMINATE. A cancelled context reaches
	// BeginTx, not the promotion, so it must NOT be reported as contention —
	// otherwise gating on it is the same as not gating at all.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := beginImmediate(cancelled, db.db); err == nil {
		t.Error("beginImmediate succeeded on a cancelled context")
	} else if errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("a cancelled context reported as write-lock contention (%v) — the sentinel does not discriminate, so the hint it gates would name the wrong cause", err)
	}

	// And once the lock is free the SAME call works, so the failure above was the
	// lock and not a broken fixture.
	release()
	tx, err = beginImmediate(ctx, db.db)
	if err != nil {
		t.Fatalf("control: beginImmediate after releasing the lock: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

// TestBeginImmediateIsNotBoundedByContext pins the limitation beginImmediate's
// doc comment states: the caller's context does NOT bound how long the promote
// blocks. modernc.org/sqlite's busy handler does not consult ctx while it spins,
// so a contended promote runs the FULL busy_timeout (5000ms in this store's DSN)
// no matter what deadline the caller passed; the deadline changes only the error
// TEXT ("context deadline exceeded" instead of "database is locked").
//
// 🔴 WHY THIS IS A TEST AND NOT JUST A COMMENT. The doc comment is what a #598
// author reads before converting a call site, and it is now the thing telling them
// NOT to convert the two request-path sites (UpsertDeveloperAlias, EraseDeveloper)
// without first lowering busy_timeout. If a driver upgrade ever made ctx actually
// bound the promote, that warning would become false and no other test in this
// tree would notice — the two arms below are what fail instead.
//
// The assertion is deliberately coarse (>= 2s under a 300ms deadline). It is not a
// latency measurement; it is the sign of the claim, and 2s cannot be reached by
// scheduler noise on a 300ms deadline.
func TestBeginImmediateIsNotBoundedByContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ctxbound.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// CONTROL FIRST, while the lock is FREE: the same 300ms deadline must let
	// beginImmediate return promptly. Without this arm, "it took 5s" could just as
	// well mean the fixture is slow, and the positive arm would prove nothing about
	// the busy handler.
	fastCtx, cancelFast := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelFast()
	freeStart := time.Now()
	freeTx, err := beginImmediate(fastCtx, db.db)
	freeElapsed := time.Since(freeStart)
	if err != nil {
		t.Fatalf("control: beginImmediate with the lock FREE and a 300ms deadline: %v", err)
	}
	if err := freeTx.Rollback(); err != nil {
		t.Fatalf("control rollback: %v", err)
	}
	if freeElapsed > time.Second {
		t.Fatalf("control: an UNCONTENDED promote took %v — the fixture itself is slow, so the contended timing below would not be evidence of anything", freeElapsed)
	}
	t.Logf("uncontended promote under a 300ms deadline: %v", freeElapsed)

	// POSITIVE: contended, with a deadline far shorter than busy_timeout.
	release := holdWriteLock(t, path)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	tx, err := beginImmediate(ctx, db.db)
	elapsed := time.Since(start)
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("beginImmediate succeeded while another connection held the write lock")
	}
	t.Logf("contended promote under a 300ms deadline: %v (err: %v)", elapsed, err)
	if elapsed < 2*time.Second {
		t.Errorf("contended beginImmediate returned after %v under a 300ms deadline — the context now DOES bound the promote, so beginImmediate's ⚠️ block and the request-path caveats on UpsertDeveloperAlias/EraseDeveloper are stale and must be rewritten (a #598 author is reading them to decide whether converting a request-path site is safe)", elapsed)
	}
	// The sentinel must still fire: a caller gating an operator hint on
	// ErrWriteLockUnavailable must get it for THIS shape too, where the driver
	// reports the deadline rather than "database is locked".
	if !errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("error = %v, want it to wrap ErrWriteLockUnavailable — under a deadline the driver renames the error, and a caller that gated on the text would lose the hint here", err)
	}
}

// TestBeginImmediatePromotesBeforeAnyReadIsPossible proves the promotion happens
// INSIDE the helper rather than being left to the caller's first statement. A
// DEFERRED BEGIN takes no snapshot until its first statement, so promoting here
// establishes the write lock and the read snapshot atomically — the window a
// caller-side promote would leave open is the SQLITE_BUSY_SNAPSHOT window.
//
// The observable form of "already promoted": a second connection cannot take
// BEGIN IMMEDIATE while our tx is merely OPEN, with no statement executed on it.
func TestBeginImmediatePromotesBeforeAnyReadIsPossible(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "promote.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Open the second handle FIRST. Open() applies migrations, which WRITE — so
	// opening it after the promotion would fail inside Open and the test would
	// pass for the wrong reason (and be indistinguishable from a fixture bug).
	other, err := Open(path)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer func() { _ = other.Close() }()
	conn, err := other.db.Conn(ctx)
	if err != nil {
		t.Fatalf("grab conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 🔴 CONTROL, AND WITHOUT IT THIS TEST IS WORTHLESS. The assertion below is
	// "the second connection CANNOT take the lock". That passes for the wrong
	// reason if the connection could not have taken it anyway — a migration tx
	// Open() failed to release, a stale WAL writer, a future Open() change. Prove
	// the lock is FREE first, so the failure below can only be the promotion.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("control: the second connection could not take the lock BEFORE any promotion (%v) — the assertion below would pass for the wrong reason", err)
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("control rollback: %v", err)
	}

	tx, err := beginImmediate(ctx, db.db)
	if err != nil {
		t.Fatalf("beginImmediate: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// No statement has run on tx. A second connection must still be locked out.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err == nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatal("a second connection took BEGIN IMMEDIATE while a beginImmediate tx was open but had run no statement — the promotion is deferred to the caller's first write, which is the hazard this helper removes")
	}
}

// TestBeginImmediateRollsBackWhenPromotionFails guards the failure path, which is
// where a leak would be invisible and lethal. It pins the pool to ONE connection
// (pinPoolToOne) so that a transaction abandoned on the error return holds the
// only connection: the next query does not error, it BLOCKS.
//
// 🔴 THE PIN IS THE INSTRUMENT AND #669 IS WHY IT IS EXPLICIT. Production runs
// maxOpenConns (4) now. At 4 this test would pass WITH THE LEAK PRESENT — the
// abandoned tx takes one connection and the assertion read simply uses one of
// the other three. Do not "align it with production"; that would delete the
// test's ability to fail while leaving it green.
//
// The assertion is therefore a bounded-context read after the failed call. With
// the rollback it returns immediately; without it, it waits out the deadline —
// which is the real production symptom (a hung command, not a failed one).
func TestBeginImmediateRollsBackWhenPromotionFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "leak.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	// DELIBERATELY 1, so a leaked connection still blocks — see pinPoolToOne.
	pinPoolToOne(t, db)

	release := holdWriteLock(t, path)
	defer release()

	tx, err := beginImmediate(context.Background(), db.db)
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("fixture: beginImmediate should have failed while the lock was held")
	}
	if tx != nil {
		t.Error("beginImmediate returned a non-nil tx alongside an error; the caller has no contract to close it")
	}
	release() // the lock is no longer the reason a query could stall

	// If the failed call leaked its transaction, this read waits for the only
	// connection to come back and never gets it.
	// 10s, not 3s: this is a LIVENESS check (does the connection come back at
	// all), never a latency one, and the three tests in this file now run in
	// parallel on one machine. A generous deadline costs nothing on the healthy
	// path — it returns in microseconds — and removes the only flake surface.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM token_events`).Scan(&n); err != nil {
		t.Fatalf("read after a failed beginImmediate: %v — on this test's PINNED pool of 1 this means the failed call did not roll its transaction back and is still holding the only connection (see pinPoolToOne — production runs maxOpenConns, where this leak would NOT block)", err)
	}
}

// pinPoolToOne forces a store's pool back to a SINGLE connection, DELIBERATELY,
// for the tests whose assertion only means something at that size.
//
// 🔴 THIS IS NOT "MIRRORING PRODUCTION" — PRODUCTION IS maxOpenConns (4) SINCE
// #669. It is the opposite: these tests detect a LEAKED CONNECTION, and the only
// way to observe a leak is to make the leaked connection the last one. At a pool
// of 4, leaking one leaves three free, every subsequent query succeeds, and the
// test passes WITH THE BUG PRESENT. Two shapes depend on it:
//
//   - "the next query BLOCKS" (TestBeginImmediateRollsBackWhenPromotionFails).
//     A leak at N=4 does not block anything.
//   - "the busy_timeout the pool hands out" (readBusyTimeout). At N>1 the pool
//     hands out an ARBITRARY connection, so an assertion about the timeout on
//     "the" connection is reading one of four and calling it the process state.
//
// ⚠️ So do NOT "fix" these to match production. If you raise maxOpenConns again,
// these stay at 1. The thing they guard is the release path, which is pool-size
// independent; the pool of 1 is the INSTRUMENT, not the subject.
//
// 🔴 AND THE PIN HAS A COST — KNOW IT BEFORE ADDING ONE TO A NEW TEST. A store
// method that concurrently uses TWO connections is legal since #669, and inside a
// pinned test it will DEADLOCK rather than fail. Several pinned sites acquire on
// context.Background(), where the symptom is the whole package timing out at 600s
// naming nothing — the worst diagnostic in this tree. Pin only a test whose
// assertion genuinely needs a single connection, and prefer a bounded ctx.
func pinPoolToOne(t *testing.T, db *DB) {
	t.Helper()
	db.db.SetMaxOpenConns(1)
	db.db.SetMaxIdleConns(1)
	// Assert the INSTRUMENT took. Every caller's assertion is only meaningful at a
	// pool of 1, so a pin that silently landed on the wrong *DB — or was added
	// before a later Open in some future edit — would return these tests to
	// "cannot fail" with nothing visible in the diff to say so.
	if n := db.db.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("pinPoolToOne did not take: MaxOpenConnections = %d, want 1 — every leak assertion in this test is inert until it does", n)
	}
}

// readBusyTimeout reports the busy_timeout the POOL hands out — i.e. what the
// next arbitrary caller in this process will get.
//
// 🔴 ONLY MEANINGFUL ON A POOL PINNED TO 1 — call pinPoolToOne first. At
// maxOpenConns this reads whichever of N connections the pool happens to hand
// back, which is not "what the next caller gets" and not the process's state.
//
// 🔴 THE DEADLINE TURNS A HANG INTO A FAILURE. Every caller runs this right after
// a release path it is testing, and on the PINNED pool of 1 these tests use the
// failure mode of a release that leaked the connection is not an error — it is a
// BLOCK, forever, waiting for the only connection to come back. On context.Background() that
// surfaces as the whole package timing out at 600s with a goroutine dump, which
// reports zero test failures and names nothing; a mutant that hangs is not a
// mutant that fails. 10s is a LIVENESS bound, never a latency one: the healthy
// path returns in microseconds, so the deadline costs nothing and cannot flake.
func readBusyTimeout(t *testing.T, db *DB) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ms int
	if err := db.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&ms); err != nil {
		t.Fatalf("read busy_timeout: %v — on a deadline this almost certainly means the release path under test did NOT return the connection, and on this test's pinned pool that is its only one", err)
	}
	return ms
}

// TestBeginImmediateBoundedCapsTheContendedWait is the reason the two
// request-path sites could be converted at all. beginImmediate's promote ignores
// ctx, so at the DSN's busy_timeout a contended request-path promote blocks ~5s
// while holding a pool slot — and at maxOpenConns simultaneous blockers, stalling
// every other in-flight request (measured: 4.75s for an unrelated read at 4).
// beginImmediateBounded caps that.
//
// 🔴 THE CONTROL IS PLAIN beginImmediate UNDER THE SAME CONTENTION. "It returned
// in 300ms" proves nothing on its own: it is also what an UNCONTENDED call looks
// like, so a fixture whose lock holder had quietly died would pass. Measuring the
// uncapped helper against the same held lock is what makes the fast return
// evidence of the cap rather than evidence of no contention.
func TestBeginImmediateBoundedCapsTheContendedWait(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bounded.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	// DELIBERATELY 1, so a leaked connection still blocks — see pinPoolToOne.
	pinPoolToOne(t, db)

	release := holdWriteLockRaw(t, path)
	defer release()

	ctx := context.Background()

	// CONTROL: the UNCAPPED helper against this very lock. It must take the long
	// path, proving the lock is genuinely held for the duration of this test.
	uncappedStart := time.Now()
	if tx, err := beginImmediate(ctx, db.db); err == nil {
		_ = tx.Rollback()
		t.Fatal("control: plain beginImmediate succeeded while the lock was held — the fixture is not contended, so the timing below means nothing")
	}
	uncapped := time.Since(uncappedStart)
	if uncapped < 2*time.Second {
		t.Fatalf("control: uncapped beginImmediate returned in %v, expected it to wait out the ~%dms DSN busy_timeout — either the lock is not held or the driver now bounds the promote, and either way the fast return below would not be evidence of the cap", uncapped, dsnBusyTimeoutMS)
	}

	// POSITIVE: the capped helper, same lock, must come back far sooner.
	start := time.Now()
	_, releaseTx, err := beginImmediateBounded(ctx, db.db, requestPathBusyTimeout)
	capped := time.Since(start)
	if err == nil {
		releaseTx()
		t.Fatal("beginImmediateBounded succeeded while another connection held the write lock — it is not promoting")
	}
	t.Logf("contended: uncapped %v vs capped %v (cap = %v)", uncapped, capped, requestPathBusyTimeout)
	// 2s is the same coarse sign the uncapped test uses: far above the 250ms cap,
	// far below the 5s default, so neither scheduler noise nor a slow machine can
	// move a correct implementation across it.
	if capped >= 2*time.Second {
		t.Errorf("capped promote took %v — the lowered busy_timeout is not being applied, so a contended request-path write stalls every in-flight request in the process for the full %dms", capped, dsnBusyTimeoutMS)
	}
	if !errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("error = %v, want it to wrap ErrWriteLockUnavailable — callers gate the operator-facing contention hint on exactly this", err)
	}
	// A failed call must return the connection AND its timeout. This is the path
	// where a leak hides: on this test's pinned pool of 1 the next caller would
	// otherwise inherit the short timeout, or block forever on a connection never
	// returned. (Pinned deliberately — see pinPoolToOne. At production's
	// maxOpenConns the leak would be masked by the other connections.)
	if got := readBusyTimeout(t, db); got != dsnBusyTimeoutMS {
		t.Errorf("busy_timeout after a FAILED beginImmediateBounded = %d, want %d — the failure path skipped the restore, so every later caller on this pooled connection silently inherits the short request-path timeout", got, dsnBusyTimeoutMS)
	}
}

// TestBeginImmediateBoundedStillTakesTheWriteLock proves the bounded helper did
// not buy its speed by giving up the property it exists for. Lowering
// busy_timeout and then NOT promoting would pass the timing test above
// perfectly — and would silently restore the DEFERRED read-then-write hazard on
// every request-path site.
//
// Observable form of "already promoted": with our tx merely OPEN and no statement
// executed on it, a second connection cannot take BEGIN IMMEDIATE.
func TestBeginImmediateBoundedStillTakesTheWriteLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "boundedlock.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Second handle opened FIRST: Open() writes, so opening it after the promotion
	// would fail inside Open and the test would pass for the wrong reason.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer func() { _ = other.Close() }()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("grab conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 🔴 CONTROL: prove the lock is FREE first, so the failure below can only be
	// our promotion and not some pre-existing writer.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("control: the second connection could not take the lock BEFORE any promotion (%v) — the assertion below would pass for the wrong reason", err)
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("control rollback: %v", err)
	}

	tx, releaseTx, err := beginImmediateBounded(ctx, db.db, requestPathBusyTimeout)
	if err != nil {
		t.Fatalf("beginImmediateBounded: %v", err)
	}
	defer releaseTx()

	// 🔴 THE CAP, ASSERTED DETERMINISTICALLY AND WITH NO WALL CLOCK. Reading the
	// pragma back from INSIDE the live transaction is the direct observation of
	// "the lowered timeout is installed on the connection this tx is running on" —
	// the property the contended-timing tests can only infer. It is worth having
	// separately because timing cannot distinguish "the site uses the unbounded
	// helper" from "the lowered pragma did not stick", and a review of this branch
	// observed one contended request-path call return in 5.04s (a full DSN
	// busy_timeout) against code that was correct on both counts, cause unresolved.
	// If the pragma ever fails to stick, THIS is the assertion that says so.
	var installed int
	if err := tx.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&installed); err != nil {
		t.Fatalf("read busy_timeout inside the bounded tx: %v", err)
	}
	if want := int(requestPathBusyTimeout.Milliseconds()); installed != want {
		t.Errorf("busy_timeout INSIDE a bounded tx = %d, want %d — the cap is not on the connection the transaction is actually running on, so a contended write here waits the full DSN %dms and, once maxOpenConns promotes block at once, stalls every other in-flight request too", installed, want, dsnBusyTimeoutMS)
	}

	// No statement has run on tx... except the pragma read above, which takes no
	// lock and cannot itself promote. A second connection must still be locked out.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err == nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatal("a second connection took BEGIN IMMEDIATE while a beginImmediateBounded tx was open but had run no write — the helper lowered busy_timeout without promoting, which is the DEFERRED read-then-write hazard it exists to remove")
	}
}

// TestBeginImmediateBoundedKeepsItsLoweredTimeoutUnderPoolChurn pins that the
// lowered busy_timeout reaches the transaction that actually promotes, even when
// the pool is discarding and recreating connections as fast as database/sql
// allows. It exists to catch a REFACTOR BACK to an Exec on the pooled *sql.DB.
//
// 🔴 WHAT IT GUARDS, AND WHAT IT DOES NOT. This is the #63 hypothesis, which
// #607 named as its leading suspect: database/sql can discard a pooled
// connection and dial a replacement from the DSN, shedding a pragma that was
// applied with a one-shot Exec. beginImmediateBounded is immune because it
// reserves a *sql.Conn and runs the pragma, the BeginTx and the promote on that
// one pinned connection — a pinned conn cannot be swapped underneath (a dead one
// surfaces as ErrConnDone, an error, not a silent replacement).
//
// ⚠️ SO THIS IS NOT A REGRESSION TEST FOR #607. That anomaly was MEASURED to be
// pool QUEUEING — a caller waiting for a free connection before its own
// perfectly-capped promote begins — and no pragma was ever shed. This test would
// not have caught it and cannot. Its value is keeping a REFUTED hypothesis from
// quietly becoming true again if the pinning is ever refactored away.
//
// WHY THE CHURN MATTERS, given TestBeginImmediateBoundedStillTakesTheWriteLock
// already reads the pragma back inside the live tx: without churn an unpinned
// implementation still PASSES. On a pool of 1 there is only one connection, so a
// `db.ExecContext` pragma and a `db.BeginTx` land on the same one anyway. Only forcing the pool to retire it BETWEEN those two calls
// separates pinned from unpinned. MEASURED: reverting the helper to the unpinned
// shape leaves that test green and fails this one.
//
// 🔴 THE CONTROL ARM IS THE WHOLE POINT. A pass proves "the pragma survived" —
// which is ALSO what a test whose churn levers did nothing looks like. Arm 2
// applies the identical levers to an UNPINNED pool and requires the pragma to be
// SHED, back to the DSN's value. Without it this test cannot tell "pinning
// protects the pragma" from "nothing was churned", which is the false-negative
// shape the #607 investigation existed to avoid. MEASURED: deleting the three
// churn lever calls from arm 2 fails arm 2.
func TestBeginImmediateBoundedKeepsItsLoweredTimeoutUnderPoolChurn(t *testing.T) {
	t.Parallel()

	// Every lever database/sql offers for retiring a pooled connection.
	churn := func(db *sql.DB) {
		db.SetConnMaxLifetime(time.Nanosecond)
		db.SetConnMaxIdleTime(time.Nanosecond)
		db.SetMaxIdleConns(0)
	}

	t.Run("pinned conn keeps the lowered timeout", func(t *testing.T) {
		t.Parallel()
		db, err := Open(filepath.Join(t.TempDir(), "churnpinned.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		ctx := context.Background()

		// Levers on BEFORE the call, so the pool is free to retire the connection
		// at every acquisition the helper makes.
		churn(db.db)

		tx, release, err := beginImmediateBounded(ctx, db.db, requestPathBusyTimeout)
		if err != nil {
			t.Fatalf("beginImmediateBounded under a churning pool: %v", err)
		}
		defer release()

		// Read the pragma from INSIDE the live transaction: the direct observation
		// that the cap is on the connection this tx is really running on.
		var installed int
		if err := tx.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&installed); err != nil {
			t.Fatalf("read busy_timeout inside the bounded tx: %v", err)
		}
		if want := int(requestPathBusyTimeout.Milliseconds()); installed != want {
			t.Errorf("busy_timeout inside a bounded tx under pool churn = %d, want %d — the pragma is no longer reaching the connection that promotes, so this helper has lost its pinned *sql.Conn and a contended request-path write now waits the DSN's %dms (#63, and the hypothesis #607 was filed on)", installed, want, dsnBusyTimeoutMS)
		}
	})

	t.Run("control: unpinned pool SHEDS the lowered timeout", func(t *testing.T) {
		t.Parallel()
		db, err := Open(filepath.Join(t.TempDir(), "churnunpinned.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		// 🔴 PINNED TO 1 BY #669, AND THIS ARM IS WHY IT MATTERS. The control
		// reproduces the pre-#63 shape: Exec a pragma against the POOL, then read
		// it back off the POOL. That round trip only means something if both land
		// on the SAME connection, which a pool of 1 guarantees and maxOpenConns
		// does not. Unpinned, the read-back could draw a different connection and
		// the precondition below would fail for a reason unrelated to churn.
		pinPoolToOne(t, db)
		ctx := context.Background()

		// The pre-#63 shape: a one-shot Exec against the POOL, no pinning.
		if _, err := db.db.ExecContext(ctx, busyTimeoutPragma(requestPathBusyTimeout)); err != nil {
			t.Fatalf("lower on the pool: %v", err)
		}
		var before int
		if err := db.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&before); err != nil {
			t.Fatalf("read before churn: %v", err)
		}
		if want := int(requestPathBusyTimeout.Milliseconds()); before != want {
			t.Fatalf("control precondition: busy_timeout before churn = %d, want %d — the Exec did not take, so the shed below would prove nothing", before, want)
		}

		churn(db.db)

		var after int
		if err := db.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&after); err != nil {
			t.Fatalf("read after churn: %v", err)
		}
		if after != dsnBusyTimeoutMS {
			t.Errorf("busy_timeout on an UNPINNED pool after churn = %d, want the DSN's %d — the churn levers did not actually retire the connection, so the pinned arm above passed for the wrong reason and this test is a false negative", after, dsnBusyTimeoutMS)
		}
	})
}

// TestBeginImmediateBoundedRestoresBusyTimeout pins the restore on the SUCCESS
// path, and proves the restore is load-bearing rather than decorative.
//
// 🔴 THE CONTROL ARM IS THE INTERESTING HALF. It MEASURES that a lowered
// busy_timeout SURVIVES conn.Close() back into the pool — so a missing restore
// poisons every later caller that draws that connection. On this test's pinned
// pool of 1 that is the WHOLE PROCESS, including the Open()-time migrations that
// must be able to wait out a competing process; at production's maxOpenConns it
// is one connection in N, which is intermittent rather than total and therefore
// harder to diagnose, not easier (see restoreAndReleaseConn). If a future driver or pool version started
// resetting session state on release, this control fails and tells us the restore
// has become redundant — rather than leaving an unfalsifiable guard in place.
func TestBeginImmediateBoundedRestoresBusyTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "restore.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	// DELIBERATELY 1, so a leaked connection still blocks — see pinPoolToOne.
	pinPoolToOne(t, db)
	ctx := context.Background()

	if got := readBusyTimeout(t, db); got != dsnBusyTimeoutMS {
		t.Fatalf("fixture: pool busy_timeout = %d, want the DSN's %d", got, dsnBusyTimeoutMS)
	}

	// CONTROL: does the pragma leak across Close without a restore?
	leakConn, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := leakConn.ExecContext(ctx, `PRAGMA busy_timeout = 111`); err != nil {
		t.Fatalf("set 111: %v", err)
	}
	if err := leakConn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := readBusyTimeout(t, db); got != 111 {
		t.Fatalf("control: busy_timeout after closing an unrestored connection = %d, want 111 — the pool now resets session state on release, so beginImmediateBounded's restore (and its poison fallback) are dead code and should be removed rather than left as an unfalsifiable guard", got)
	}
	// Put it back so the positive arm starts from the DSN value.
	fix, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := fix.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, dsnBusyTimeoutMS)); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := fix.Close(); err != nil {
		t.Fatalf("close fix: %v", err)
	}

	// POSITIVE: a full successful bounded transaction must leave no trace.
	tx, releaseTx, err := beginImmediateBounded(ctx, db.db, requestPathBusyTimeout)
	if err != nil {
		t.Fatalf("beginImmediateBounded: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE token_events SET id = id WHERE 0`); err != nil {
		t.Fatalf("write in bounded tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	releaseTx()

	if got := readBusyTimeout(t, db); got != dsnBusyTimeoutMS {
		t.Errorf("busy_timeout after a COMMITTED beginImmediateBounded = %d, want %d — the request-path timeout leaked into a pooled connection, so every later caller that DRAWS it (including Open()-time migrations) now gives up after %dms — on this test's pinned pool of 1 that is every caller; at production's maxOpenConns it is intermittent, which is harder to diagnose, not easier", got, dsnBusyTimeoutMS, got)
	}
}

// TestRestoreAndReleaseConnDiscardsAConnectionItCannotRestore pins the POISON
// FALLBACK — the branch that runs when the restore itself fails.
//
// 🔴 IT WAS UNGUARDED, AND ITS DOC COMMENT CLAIMED THE OPPOSITE. The comment calls
// a connection of unknown busy_timeout going back to a one-connection pool "the
// one outcome this must never produce"; MEASURED, deleting the
// `conn.Raw(... driver.ErrBadConn)` line left the ENTIRE tree green. Nothing
// reached it, because a `PRAGMA busy_timeout` on a healthy connection does not
// fail — which is exactly why restoreAndReleaseConn takes the statement as a
// parameter.
//
// The stakes are wider than one request: on this test's pinned pool of 1 the
// pool hands the same connection to everything that follows, so ONE unrestored
// release silently drops every later caller — including the Open()-time migrations
// that must be able to wait out a competing process — to the 250ms request-path
// timeout.
func TestRestoreAndReleaseConnDiscardsAConnectionItCannotRestore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "poison.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	// DELIBERATELY 1, so a leaked connection still blocks — see pinPoolToOne.
	pinPoolToOne(t, db)
	ctx := context.Background()

	const failingRestoreSQL = `PRAGMA busy_timeout = 'not a number at all' AND`

	// 🔴 CONTROL 1: the statement must actually FAIL. SQLite silently ignores
	// UNKNOWN pragmas, so a "clearly broken" pragma can be a no-op error-free
	// statement. If this one succeeded, the poison branch would never run and this
	// test would be measuring nothing.
	probe, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("probe conn: %v", err)
	}
	if _, err := probe.ExecContext(ctx, failingRestoreSQL); err == nil {
		_ = probe.Close()
		t.Fatalf("the fixture's restore statement SUCCEEDED — restoreAndReleaseConn would take its happy path and the poison branch below would never be exercised")
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}

	// 🔴 CONTROL 2: a lowered busy_timeout really does survive release, so "the
	// pool reports 5000 afterwards" below can only mean the connection was
	// discarded and rebuilt from the DSN — not that the pool resets session state
	// on its own. (TestBeginImmediateBoundedRestoresBusyTimeout measures the same
	// property; repeated here so THIS test's conclusion does not depend on another
	// test having run.)
	leakConn, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("leak conn: %v", err)
	}
	if _, err := leakConn.ExecContext(ctx, `PRAGMA busy_timeout = 111`); err != nil {
		t.Fatalf("set 111: %v", err)
	}
	if err := leakConn.Close(); err != nil {
		t.Fatalf("close leak conn: %v", err)
	}
	if got := readBusyTimeout(t, db); got != 111 {
		t.Fatalf("control: busy_timeout after closing an unrestored connection = %d, want 111 — the pool now resets session state on release, so the poison fallback is dead code and should be removed rather than left as an unfalsifiable guard", got)
	}

	// POSITIVE: lower the timeout, then release through a restore that fails. The
	// connection must be DISCARDED, so the pool's next connection is built fresh
	// from the DSN and carries dsnBusyTimeoutMS.
	conn, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 222`); err != nil {
		t.Fatalf("lower to 222: %v", err)
	}
	restoreAndReleaseConn(ctx, conn, failingRestoreSQL)

	if got := readBusyTimeout(t, db); got != dsnBusyTimeoutMS {
		t.Errorf("busy_timeout after a FAILED restore = %d, want the DSN's %d — the connection was returned to the pool with an unknown timeout instead of being poisoned, and on this pinned pool of 1 that value is now what EVERY later caller gets, Open()-time migrations included (at production's maxOpenConns it poisons one connection in N — intermittent rather than total)", got, dsnBusyTimeoutMS)
	}
}

// TestRestoreAndReleaseConnDiscardsAConnectionItDidNotActuallyRestore covers the
// failure mode an error check CANNOT see: a restore statement that SUCCEEDS while
// restoring nothing.
//
// 🔴 THIS IS THE HOLE THE restoreSQL PARAMETER OPENED. Making the statement an
// argument was the only way to reach the poison branch from a test, but it also
// made the statement free text — and dsnBusyTimeoutMS's own doc claims the
// one-constant design makes DSN/restore drift "unrepresentable". Two plausible
// statements defeat an error-only check:
//
//   - a wrong-but-valid `PRAGMA busy_timeout = 250` — succeeds, restores nothing;
//   - a typo'd `PRAGMA busy_timout = 5000` — SQLite silently IGNORES unknown
//     pragmas, so it also succeeds and also restores nothing.
//
// Both would return the process's ONLY connection at the short timeout, silently,
// through the happy path. restoreAndReleaseConn therefore reads the value back and
// compares it, and these two cases are what prove it.
func TestRestoreAndReleaseConnDiscardsAConnectionItDidNotActuallyRestore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		restoreSQL string
	}{
		{"succeeds but sets the WRONG value", fmt.Sprintf(`PRAGMA busy_timeout = %d`, requestPathBusyTimeout.Milliseconds())},
		{"succeeds but is silently IGNORED (unknown pragma)", `PRAGMA busy_timout = 5000`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "wrongrestore.db")
			db, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			// DELIBERATELY 1, so a leaked connection still blocks — see pinPoolToOne.
			pinPoolToOne(t, db)
			ctx := context.Background()

			// 🔴 CONTROL: the statement must SUCCEED. If it errored, the ordinary
			// poison branch would fire and this test would prove nothing about the
			// read-back — it would be a duplicate of the test above.
			probe, err := db.db.Conn(ctx)
			if err != nil {
				t.Fatalf("probe conn: %v", err)
			}
			if _, err := probe.ExecContext(ctx, c.restoreSQL); err != nil {
				_ = probe.Close()
				t.Fatalf("fixture: restore statement %q ERRORED (%v) — this case exists to cover statements that succeed, so it must not error", c.restoreSQL, err)
			}
			if err := probe.Close(); err != nil {
				t.Fatalf("close probe: %v", err)
			}
			// The probe just left the pool's connection at the wrong value; put it
			// back so the positive arm starts from a known state.
			fix, err := db.db.Conn(ctx)
			if err != nil {
				t.Fatalf("fix conn: %v", err)
			}
			if _, err := fix.ExecContext(ctx, busyTimeoutRestoreSQL); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if err := fix.Close(); err != nil {
				t.Fatalf("close fix: %v", err)
			}

			conn, err := db.db.Conn(ctx)
			if err != nil {
				t.Fatalf("conn: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 222`); err != nil {
				t.Fatalf("lower to 222: %v", err)
			}
			restoreAndReleaseConn(ctx, conn, c.restoreSQL)

			if got := readBusyTimeout(t, db); got != dsnBusyTimeoutMS {
				t.Errorf("busy_timeout after a restore that SUCCEEDED without restoring = %d, want the DSN's %d — restoreAndReleaseConn trusted the statement's exit status instead of reading the value back, so this pinned pool's only connection is now at %dms and every later caller inherits it", got, dsnBusyTimeoutMS, got)
			}
		})
	}
}

// beginImmediatePromoteError runs the promote statement directly and returns the
// raw driver error, so a test can classify the REAL error object rather than a
// synthetic stand-in. modernc.org/sqlite's Error has unexported fields and no
// constructor, so a table test over hand-built codes is not possible — and would
// be testing arithmetic rather than the driver.
func beginImmediatePromoteError(t *testing.T, db *sql.DB) error {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(context.Background(), beginImmediatePromoteSQL)
	if err == nil {
		t.Fatal("promote SUCCEEDED; this helper is only meaningful when it fails")
	}
	return err
}

// TestBusySnapshotClassifiesAsContention pins the `& 0xff` extended-code mask
// with a REAL SQLITE_BUSY_SNAPSHOT (517) rather than with arithmetic.
//
// 🔴 517 IS THE FAILURE THIS ENTIRE FILE EXISTS TO PREVENT, and until now nothing
// produced one. Every other contention test drives a lock holder and gets plain
// SQLITE_BUSY (5), for which the mask is a no-op — so deleting `& 0xff` left the
// tree green while making the one code the comments cite repeatedly classify as
// non-contention, i.e. answer 500 instead of 503.
//
// 517 arises only from the DEFERRED read-then-write upgrade: a transaction takes
// its read snapshot, ANOTHER connection commits, and the first then tries to
// write. busy_timeout does NOT retry it. That is precisely the shape beginImmediate
// removes by promoting up front, which is why it has to be built by hand here.
func TestBusySnapshotClassifiesAsContention(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// A second handle, so the interleaving is genuinely cross-connection.
	other, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, dsnBusyTimeoutMS))
	if err != nil {
		t.Fatalf("open other: %v", err)
	}
	defer func() { _ = other.Close() }()

	ctx := context.Background()

	// DEFERRED, exactly as every pre-#598 call site was.
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin deferred: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Read: this is what takes the snapshot the write will later be judged against.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM token_events`).Scan(&n); err != nil {
		t.Fatalf("read in deferred tx: %v", err)
	}

	// Another connection commits underneath us, invalidating that snapshot.
	if _, err := other.ExecContext(ctx,
		`INSERT INTO developer_alias (alias, canonical, ts) VALUES ('snap-gh', 'snap', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("other write: %v", err)
	}

	// Now upgrade to a write. This is the 517.
	_, err = tx.ExecContext(ctx, beginImmediatePromoteSQL)
	if err == nil {
		t.Skip("this driver/build no longer produces SQLITE_BUSY_SNAPSHOT for the deferred read-then-write upgrade; the mask it pins may have become unreachable")
	}
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		t.Skipf("upgrade failed with a non-SQLite error (%v); cannot pin the extended-code mask from this shape", err)
	}
	t.Logf("deferred read-then-write upgrade: code=%d primary=%d err=%v", serr.Code(), serr.Code()&0xff, err)

	// 🔴 CONTROL: the code must actually be EXTENDED, or the mask is untested. A
	// plain 5 here would make the assertion below pass with `& 0xff` deleted.
	if serr.Code() == sqliteBusy {
		t.Fatalf("control: got a PRIMARY code %d, not an extended one — the mask this test exists to pin is a no-op for this error, so the assertion below would prove nothing", serr.Code())
	}
	if serr.Code()&0xff != sqliteBusy {
		t.Fatalf("control: code %d does not mask to SQLITE_BUSY; this is not the 517-family error this test means to pin", serr.Code())
	}

	if !isPromoteContention(context.Background(), err) {
		t.Errorf("isPromoteContention classified a real SQLITE_BUSY_SNAPSHOT (code %d) as NOT contention — the extended-code mask is gone, so the one failure mode beginImmediate exists to prevent answers 500 'store error' instead of a retryable 503", serr.Code())
	}
}

// TestBusyTimeoutPragmaFloorsAtOneMillisecond pins the floor, because the value
// it prevents is not "a shorter wait" but "no wait at all".
//
// PRAGMA busy_timeout = 0 DISABLES SQLite's busy handler: every concurrent write
// fails instantly with SQLITE_BUSY instead of retrying. Since Duration ->
// milliseconds TRUNCATES, a caller passing any sub-millisecond duration would
// silently get that, which is the opposite of what asking for a short wait means.
// Today's only caller passes a 250ms constant, so the floor is unreachable through
// beginImmediateBounded — which is exactly why the rendering is a pure function
// rather than an inline `if` no test could reach.
func TestBusyTimeoutPragmaFloorsAtOneMillisecond(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		busy time.Duration
		want string
	}{
		{"the production value", requestPathBusyTimeout, "PRAGMA busy_timeout = 250"},
		{"the DSN value", time.Duration(dsnBusyTimeoutMS) * time.Millisecond, "PRAGMA busy_timeout = 5000"},
		{"exactly one millisecond", time.Millisecond, "PRAGMA busy_timeout = 1"},
		{"sub-millisecond truncates to 0 without the floor", 500 * time.Microsecond, "PRAGMA busy_timeout = 1"},
		{"zero", 0, "PRAGMA busy_timeout = 1"},
		{"negative", -5 * time.Second, "PRAGMA busy_timeout = 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := busyTimeoutPragma(c.busy); got != c.want {
				t.Errorf("busyTimeoutPragma(%v) = %q, want %q — a rendered 0 DISABLES the busy handler rather than shortening it, so a caller asking for a brief wait would get no wait at all", c.busy, got, c.want)
			}
		})
	}
}

// TestPromoteFailureThatIsNotContentionIsNotRetryable pins the DISCRIMINATION in
// isPromoteContention: only a genuine lock outcome may wrap ErrWriteLockUnavailable.
//
// 🔴 WHAT WAS WRONG, AND WHY IT IS WORSE THAN THE 500 IT REPLACED. The promote's
// error was wrapped in the sentinel UNCONDITIONALLY, so every failure — read-only
// database, disk full, I/O error — reached the two request-path handlers as
// contention and was answered 503 + `Retry-After: 1` + "database is busy: another
// writer holds the write lock, retry shortly". None of those conditions clears on
// its own: automation honouring Retry-After retries forever, and the operator has
// been told explicitly NOT to suspect the database. On the GDPR erasure endpoint
// that is a compliance action reported as merely delayed.
//
// MEASURED (modernc.org/sqlite v1.48.0, this store's DSN, real helpers):
// contention is *sqlite.Error code 5, a read-only database is code 8, and a
// contended promote under a ctx deadline carries NO SQLite code at all — which is
// why the ctx arm exists and why both are asserted here.
func TestPromoteFailureThatIsNotContentionIsNotRetryable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.db")

	// Build a real, migrated database, then make the FILE read-only while leaving
	// the directory writable. Open() itself refuses this file (it probes for
	// write access up front), so the handle is opened through the driver — which
	// is exactly how the pool builds a REPLACEMENT connection after a poison, and
	// therefore a state a long-running tierd can reach without restarting.
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	ro, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, dsnBusyTimeoutMS))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = ro.Close() }()
	// DELIBERATELY 1, not "same as production" — production is maxOpenConns (4)
	// since #669. A pool of 1 is what makes a leaked connection observable here.
	ro.SetMaxOpenConns(1)

	ctx := context.Background()

	// 🔴 CONTROL: the promote must actually FAIL here. If this database turned out
	// to be writable, both assertions below would pass vacuously.
	tx, err := beginImmediate(ctx, ro)
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("control: beginImmediate SUCCEEDED against a read-only database — the fixture is not read-only, so the classification below is untested")
	}
	t.Logf("read-only promote error: %v", err)

	if errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("a read-only database reports as write-lock contention (%v) — the API layer turns that into 503 + Retry-After \"retry shortly\", so a client retries forever and the operator is told not to look at the database, for a condition that never clears", err)
	}

	// Same for the bounded helper, which is the one the request path uses.
	_, release, err := beginImmediateBounded(ctx, ro, requestPathBusyTimeout)
	if err == nil {
		release()
		t.Fatal("control: beginImmediateBounded SUCCEEDED against a read-only database")
	}
	if errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("bounded: a read-only database reports as write-lock contention (%v) — this is the helper every request-path writer uses, so this is the error that becomes a 503", err)
	}

	// 🔴 THE ORDERING ARM. isPromoteContention consults the SQLite result code
	// BEFORE ctx.Err(), and this is the case that tells the two orderings apart: a
	// permanently-unwritable database failing while the caller's context happens to
	// be expired. With the clock checked first, this classifies as contention and a
	// read-only store answers "retry shortly" forever — the same heuristic-beats-
	// exact-match inversion the alias handler was just fixed for, reintroduced one
	// layer down. Both arms above use a live context, so without this the ordering
	// is untested.
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	roErr := beginImmediatePromoteError(t, ro)
	if got := isPromoteContention(expired, roErr); got {
		t.Errorf("isPromoteContention(expired ctx, %v) = true — the caller's clock is pre-empting the engine's own result code, so a read-only or full database is reported as retryable whenever the request context has already expired", roErr)
	}
	// Counter-control: with no SQLite code at all, the clock IS the only signal and
	// must still classify as contention — otherwise the deadline-renamed contended
	// promote (measured: "context deadline exceeded", no code) loses its sentinel.
	if !isPromoteContention(expired, errors.New("no sqlite code here")) {
		t.Error("isPromoteContention(expired ctx, code-less error) = false — a contended promote that the driver renamed to a context error would stop wrapping the sentinel, and TestBeginImmediateIsNotBoundedByContext's shape would answer 500 instead of 503")
	}

	// 🔴 AND THE COUNTER-CONTROL, WITHOUT WHICH THE NARROWING COULD BE TOTAL. A
	// classifier that simply never returns true would pass everything above. Real
	// contention on a healthy database must STILL wrap the sentinel.
	live := filepath.Join(t.TempDir(), "live.db")
	db, err := Open(live)
	if err != nil {
		t.Fatalf("open live: %v", err)
	}
	defer func() { _ = db.Close() }()
	rel := holdWriteLockRaw(t, live)
	defer rel()
	if _, _, err := beginImmediateBounded(ctx, db.db, requestPathBusyTimeout); !errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("counter-control: genuine contention no longer wraps ErrWriteLockUnavailable (%v) — the narrowing went too far and the 503 path is now dead", err)
	}
}

// requestPathWriterSite is ONE bounded request-path write site, exercised
// identically by the two guards that follow. They ask opposite questions of the
// same sites — "does contention still reach the caller as the retryable
// sentinel?" and "does a PERMANENT failure still reach it as something else?" —
// and the answers are only meaningful together, so the sites live in one place.
//
// 🔴 THE SHARING IS THE POINT, NOT A TIDY-UP. Kept as two literals, a newly
// converted site gets added to whichever guard its author was thinking about,
// and the other silently keeps passing over a stale list. That is precisely how
// #610 stayed open: CorrectManualCostEvent was added to the bounded-write-lock
// table when #346 converted it, while InsertManualCostEvent — the other half of
// the same route — was in neither guard at all.
type requestPathWriterSite struct {
	name string
	// call must perform a read-then-write through the site under test. All
	// return an error only; EraseDeveloper's counts are irrelevant to both
	// guards.
	//
	// ⚠️ It must be callable BOTH lock-free (where it has to succeed) and
	// against a database where the promote cannot succeed at all (where it has
	// to fail). Anything that only works on a pristine database — a divergent
	// keyed cost, an argument the site validates before it begins — breaks one
	// guard's control arm while leaving the other green.
	call func(context.Context, *DB) error
}

func requestPathWriterSites() []requestPathWriterSite {
	return []requestPathWriterSite{
		{"UpsertDeveloperAlias", func(ctx context.Context, d *DB) error {
			return d.UpsertDeveloperAlias(ctx, "alice-gh", "alice")
		}},
		{"EraseDeveloper", func(ctx context.Context, d *DB) error {
			_, err := d.EraseDeveloper(ctx, "alice")
			return err
		}},
		// The lock-free arm runs this with a fresh key, which takes the
		// no-existing-row branch: it still SELECTs first and then writes inside
		// the same transaction, so it exercises the identical begin. The locked
		// arm reuses the key, which is irrelevant — a converted site fails at the
		// promote, before the SELECT that would notice.
		{"CorrectManualCostEvent", func(ctx context.Context, d *DB) error {
			_, err := d.CorrectManualCostEvent(ctx, manualCostEvent("lockprobe", 10_500), "finance-alice", "contention probe")
			return err
		}},
		// #610, KEYED. The lock-free arm creates the key; the locked arm re-posts
		// it at the IDENTICAL cost, which is the idempotent no-op branch — so if
		// this site ever stopped promoting, the locked arm would reach the SELECT,
		// find a matching cost, and fail at the INSERT instead, which is exactly
		// the DEFERRED signature property 1 catches. A DIVERGENT cost would be
		// wrong here: ErrCostConflict returns BEFORE any write and would make the
		// locked arm fail for a reason that has nothing to do with the lock.
		{"InsertManualCostEvent (keyed)", func(ctx context.Context, d *DB) error {
			return d.InsertManualCostEvent(ctx, manualCostEvent("insertprobe", 10_500))
		}},
		// #610, UNKEYED. Same method, different branch: no idempotency_key means
		// no pre-check, so this arm pins the begin ALONE. Before #610 this branch
		// delegated to InsertTokenEvent — a bare ExecContext with no transaction
		// at all — and reverting it there is a one-line edit that property 1
		// catches (a bare INSERT under contention fails with "database is locked",
		// never the sentinel).
		{"InsertManualCostEvent (unkeyed)", func(ctx context.Context, d *DB) error {
			return d.InsertManualCostEvent(ctx, manualCostEvent("", 10_500))
		}},
	}
}

// TestRequestPathWritersTakeTheBoundedWriteLock pins the request-path sites that
// take the bounded write lock: UpsertDeveloperAlias and EraseDeveloper (converted
// by #598), CorrectManualCostEvent (#346's sanctioned /costs override, converted
// on its own branch) and BOTH branches of InsertManualCostEvent (#610).
//
// CorrectManualCostEvent earns its place here more than either of the other two:
// it is the ONLY path in this project that rewrites already-captured money rather
// than appending to it, and it decides what to rewrite by SELECTing the row and
// comparing its identity tuple — a read-then-write whose read is the guard. Under
// a DEFERRED begin that guard is evaluated against a snapshot the subsequent
// UPDATE may no longer be entitled to write.
//
// InsertManualCostEvent is here because leaving it out was itself the defect
// (#610). It is the OTHER half of the same route: same SELECT-then-upsert shape,
// same r.Context(), same store — and while it ran on a DEFERRED
// BeginTx, POST /api/v1/costs answered a lost race for the write lock with a
// bounded retryable 503 when `override=true` and an unbounded 5000ms block then a
// permanent-looking 500 when it was false. Both of its branches are listed, keyed
// and unkeyed, because they are two different begins to revert: the keyed one
// guards #295's divergence verdict, and the unkeyed one would otherwise be a bare
// ExecContext that reintroduces the same split one field further down.
//
// 🔴 THESE WERE THE UNGUARDED TWO. TestConvertedMigrationSitesTakeTheWriteLock
// covers the seven Open()-time conversions; these two were covered by NOTHING.
// MEASURED, independently for each: reverting the call to
// `d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})` — the
// precise broken form #598 exists to eliminate — compiled and left
// `go test ./internal/...` fully green. A weaker mutant (bounded -> unbounded
// beginImmediate, rollback intact) survived the whole tree too.
//
// Each subtest therefore asserts TWO independent properties, because the two
// mutants have different signatures:
//
//   - IT PROMOTES. The error must wrap ErrWriteLockUnavailable, which only the
//     promote produces. The DEFERRED revert instead sails through its reads and
//     fails at its first WRITE with a bare "database is locked" — a different
//     error, from a different place, after the whole read has been done.
//   - THE WAIT IS BOUNDED. It must return well inside the DSN's 5s busy_timeout.
//     Reverting to plain beginImmediate still wraps the sentinel, so only the
//     timing tells the two apart — and the bound is the entire reason these sites
//     could be converted at all: the promote ignores ctx, and with
//     a pool of 1 an uncapped 5s wait on an HTTP handler stalls every other
//     in-flight request in the process.
//
// ⚠️ The ceiling is `4 * requestPathBusyTimeout`, NOT the flat 2s this paragraph
// used to name — the flat form outlived the code that used it and was still being
// described here after the derived ceiling landed. It is written in terms of the
// constant on purpose (see the long note at the assertion itself): a flat number
// answers only "is this site bounded at all" and cannot see the cap being WIDENED,
// which is a mutant that was MEASURED to survive the whole suite.
func TestRequestPathWritersTakeTheBoundedWriteLock(t *testing.T) {
	t.Parallel()

	for _, c := range requestPathWriterSites() {
		t.Run(c.name, func(t *testing.T) {
			// Own database per case so the contended waits overlap rather than
			// serialise. (Each case also pins its pool to 1 below, so a shared
			// handle would queue them; separate files keep each case's lock
			// holder independent regardless.)
			t.Parallel()
			path := filepath.Join(t.TempDir(), "reqpath.db")
			db, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			// DELIBERATELY 1, so a leaked connection still blocks — see pinPoolToOne.
			pinPoolToOne(t, db)

			// 🔴 A LIVENESS BOUND, NOT A LATENCY ONE — and it does not weaken the
			// timing assertion below, because the cap under test is 250ms. It is
			// here so a release path that LEAKS the connection fails this test
			// instead of hanging it: on a pool of 1, beginImmediateBounded's
			// db.Conn() would otherwise block forever waiting for the only
			// connection, and the package would die at the 600s go test timeout
			// reporting zero failures and naming nothing. MEASURED: dropping
			// conn.Close() from restoreAndReleaseConn does exactly that.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// 🔴 CONTROL ARM, LOCK FREE. The call must SUCCEED when nothing holds
			// the lock. Without it, a fixture that made the method fail early
			// (bad argument validation, a missing table) would leave the locked
			// arm passing for a reason that has nothing to do with the promote.
			if err := c.call(ctx, db); err != nil {
				t.Fatalf("control: %s failed with the lock FREE: %v — the fixture is broken, so the locked arm below proves nothing", c.name, err)
			}

			release := holdWriteLockRaw(t, path)
			defer release()

			start := time.Now()
			err = c.call(ctx, db)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("%s returned nil while another connection held the write lock", c.name)
			}
			// PROPERTY 1: the failure came from the promote.
			if !errors.Is(err, ErrWriteLockUnavailable) {
				t.Errorf("error = %v, want it to wrap ErrWriteLockUnavailable — this site has lost its BEGIN IMMEDIATE and is running DEFERRED again: it read first and only discovered the contention at its first write, where cross-process the failure is an unretried SQLITE_BUSY_SNAPSHOT (517) rather than the sentinel the API layer turns into a 503", err)
			}
			// PROPERTY 2: and the wait was capped.
			//
			// ⚠️ THIS ASSERTION NAMES SEVERAL CAUSES, DELIBERATELY. A review of this
			// branch observed ONE failure here in ~20 full-suite runs at 5.040s —
			// bit-for-bit a full DSN busy_timeout — against code that was correct,
			// with scheduler noise, pragma flakiness and cold-dial cost each ruled
			// out by measurement and the mechanism left UNRESOLVED. Wall clock
			// cannot separate "this site uses the unbounded helper" from "the
			// lowered pragma was not in effect on that connection", so it must not
			// claim to: the deterministic half of this property is asserted by the
			// pragma read-back in TestBeginImmediateBoundedStillTakesTheWriteLock.
			// If you land here, read that test's result FIRST and do not rewrite
			// this call site until you know which cause you have.
			//
			// ⚠️ #607 NOW HAS FOUR CALL SITES, NOT TWO. This table gained
			// CorrectManualCostEvent and then InsertManualCostEvent (#610), each
			// using the same helper, the same pragma-lowering mechanism and the
			// same contention shape, so each inherits the anomaly identically.
			// FIVE table rows, FOUR sites: InsertManualCostEvent's keyed and
			// unkeyed branches share one beginImmediateBounded call. At the
			// observed ~1-in-20 rate the chance of at least one flake per suite
			// run is now roughly 5/20 of a probe rather than 2/20 — a
			// flake-budget change, not a new defect, but #607 should carry the
			// updated site count.
			// 🔴 THE CEILING IS DERIVED FROM THE CAP, NOT A FLAT 2s, AND THAT IS A
			// FIX FOR A MEASURED HOLE. A flat threshold only asks "is this site
			// bounded at all", so it cannot see the cap being WIDENED. MEASURED:
			// changing this site's call to
			// `beginImmediateBounded(ctx, d.db, 1500*time.Millisecond)` — a 6x
			// widening that stalls every in-flight request for 1.5s, exactly the
			// harm requestPathBusyTimeout exists to prevent — left
			// `go test ./internal/store/ ./internal/api/` FULLY GREEN under the old
			// 2s threshold. Nothing anywhere asserted WHICH duration a call site
			// passes: TestBeginImmediateBoundedStillTakesTheWriteLock reads the
			// pragma back deterministically but passes requestPathBusyTimeout in
			// ITSELF, so it pins the helper, never the caller.
			//
			// 4x leaves ~3.9x headroom over the ~259ms these sites actually take,
			// which is ample for scheduler noise, while failing any widening past
			// 1s. Because it is written in terms of the constant, lowering
			// requestPathBusyTimeout tightens this automatically.
			//
			// ⚠️ RAISING THE CONSTANT ITSELF IS CAUGHT SOMEWHERE ELSE, AND ONLY
			// THERE. This ceiling scales WITH requestPathBusyTimeout, so widening
			// the constant moves the guard with it and this assertion stays
			// green — as does the pragma read-back in
			// TestBeginImmediateBoundedStillTakesTheWriteLock, which also derives
			// its expectation from the constant. The one assertion that pins the
			// VALUE is the "the production value" row of
			// TestBusyTimeoutPragmaFloorsAtOneMillisecond, whose want is the
			// literal string "PRAGMA busy_timeout = 250" (MEASURED: widening the
			// constant to 1500ms fails that row and nothing else in the tree). Do
			// not "tidy" that literal into a computed string — it is the whole
			// guard, and computing it would delete the guard silently.
			maxContendedWait := 4 * requestPathBusyTimeout
			t.Logf("%s contended: %v (cap = %v, ceiling = %v, DSN busy_timeout = %dms)", c.name, elapsed, requestPathBusyTimeout, maxContendedWait, dsnBusyTimeoutMS)
			if elapsed >= maxContendedWait {
				t.Errorf("%s took %v under contention, over the %v ceiling (4x the %v cap; DSN busy_timeout is %dms). THREE causes, and they have different fixes: (1) this site moved to the UNBOUNDED beginImmediate, (2) this site still calls the bounded helper but passes a WIDER duration than requestPathBusyTimeout, or (3) the lowered busy_timeout was not in effect on the connection that promoted (see the 5.04s anomaly noted above). Wall clock cannot separate them; read TestBeginImmediateBoundedStillTakesTheWriteLock's result and the call site's literal argument before changing code", c.name, elapsed, maxContendedWait, requestPathBusyTimeout, dsnBusyTimeoutMS)
			}

			// The pooled connection must come back with the DSN timeout intact —
			// the failure path is where a skipped restore would hide.
			if got := readBusyTimeout(t, db); got != dsnBusyTimeoutMS {
				t.Errorf("busy_timeout after a contended %s = %d, want %d — the request-path timeout leaked into the pool", c.name, got, dsnBusyTimeoutMS)
			}
		})
	}
}

// TestRequestPathWritersDoNotSellAPermanentFailureAsRetryable is the OTHER half
// of the guard above, over the same requestPathWriterSites: contention must reach
// the caller as ErrWriteLockUnavailable, and everything else must NOT.
//
// 🔴 WHY IT EXISTS AT THE SITE AND NOT ONLY AT THE HELPER. The discrimination
// itself — SQLite result code 5 is contention, code 8 (read-only or full
// database) is not — is isPromoteContention's, and
// TestPromoteFailureThatIsNotContentionIsNotRetryable pins it on the two helpers
// directly. What NOTHING pinned was that a call site passes the helper's verdict
// through unchanged. MEASURED as a surviving mutant: replacing a converted site's
//
//	if err != nil {
//	    return err
//	}
//
// with `return fmt.Errorf("%w: %w", ErrWriteLockUnavailable, err)` compiles, reads
// as making the site's errors "consistent" with the sentinel its handler checks,
// and leaves the whole tree green — the guard above still passes because under
// contention the error carries the sentinel either way, and the helper-level test
// never calls the site. On the live path a read-only or full database then
// answers POST /costs (and the alias and erasure routes) with 503 +
// `Retry-After: 1` + "database is busy … retry shortly": a condition that never
// clears, advertised as transient, with the operator told explicitly not to
// suspect the database. That is strictly worse than the 500 it replaces, and it
// is the exact harm #598 narrowed isPromoteContention to prevent.
//
// The fixture is the same read-only-FILE trick that test uses (the file is 0444,
// the directory is not, and the handle is opened through the driver because Open
// probes for write access and would refuse) — a state a long-running tierd
// reaches without restarting, once the pool builds a replacement connection.
//
// COUNTER-CONTROL: a site that swallowed the sentinel in the other direction —
// never wrapping it — would pass every assertion here and kill the 503 path
// outright. That direction is asserted, over the identical site list, by property
// 1 of TestRequestPathWritersTakeTheBoundedWriteLock. Neither guard is meaningful
// alone; both iterate requestPathWriterSites so a new site cannot land in one and
// miss the other.
func TestRequestPathWritersDoNotSellAPermanentFailureAsRetryable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "readonly.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	ro, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, dsnBusyTimeoutMS))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = ro.Close() }()
	// 🔴 NOT "mirrors production" — it said that until #669 and the sentence is
	// now false (production is maxOpenConns, 4). It is DELIBERATELY 1 for the
	// same reason as pinPoolToOne: the single connection is what makes a leaked
	// one hang rather than merely slow the next caller. Keep it at 1 even if
	// production's pool changes again.
	ro.SetMaxOpenConns(1)
	db := &DB{db: ro}

	ctx := context.Background()
	for _, c := range requestPathWriterSites() {
		t.Run(c.name, func(t *testing.T) {
			// 🔴 CONTROL, AND IT IS NOT OPTIONAL. If this database turned out to be
			// writable the call would succeed, err would be nil, and errors.Is(nil,
			// ...) is false — so the assertion below would pass vacuously and this
			// test would certify nothing. (No t.Parallel here: the sites share one
			// connection by design, and running them concurrently would queue them
			// behind it for no benefit — each call fails in microseconds, at the
			// promote, with no lock to wait for.)
			err := c.call(ctx, db)
			if err == nil {
				t.Fatalf("control: %s SUCCEEDED against a read-only database — the fixture is not read-only, so the classification below is untested", c.name)
			}
			t.Logf("%s against a read-only database: %v", c.name, err)
			if errors.Is(err, ErrWriteLockUnavailable) {
				t.Errorf("%s reports a read-only database as write-lock contention (%v) — the API layer answers exactly this sentinel with 503 + Retry-After \"retry shortly\", so a client retries a condition that never clears and the operator is told not to look at the database. The site is wrapping the sentinel around failures it did not classify; only isPromoteContention may decide this, and it already said no", c.name, err)
			}
		})
	}
}

// TestRequestPathContentionErrorCarriesNoValidationPrefix pins, AT THE PRODUCER,
// the premise the alias handler's classification rests on: the store's write-lock
// contention error must not look like one of its caller-facing validation errors.
//
// UpsertDeveloperAlias stamps "developer_alias:" on every validation failure
// (self-map, chain) and internal/api/handler.go classifies 400-vs-503 partly on
// that prefix. MEASURED: adding the same prefix to the begin failure —
// `fmt.Errorf("developer_alias: %w", err)`, an edit that reads as making the
// function's errors consistent — left the entire tree green.
//
// The handler no longer depends on this (it checks the sentinel FIRST, pinned by
// api.TestContentionOutranksTheValidationPrefix), so this test is not the load
// bearing guard it would have been. It stays because the premise belongs where it
// is PRODUCED: any future consumer that classifies this error by message text
// deserves to have the store's half of the contract pinned rather than assumed.
func TestRequestPathContentionErrorCarriesNoValidationPrefix(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "prefix.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	release := holdWriteLockRaw(t, path)
	defer release()

	// Bounded for the same liveness reason as the test above: a leaked connection
	// must fail this test, not hang the package at the 600s go test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = db.UpsertDeveloperAlias(ctx, "alice-gh", "alice")
	if !errors.Is(err, ErrWriteLockUnavailable) {
		t.Fatalf("UpsertDeveloperAlias under a held lock returned %v, want a wrapped ErrWriteLockUnavailable — without contention there is no error here to check the prefix of", err)
	}
	if strings.HasPrefix(err.Error(), "developer_alias:") {
		t.Errorf("contention error %q carries the \"developer_alias:\" validation prefix this method stamps on its caller-facing errors — a consumer classifying by that prefix reads a transient lock race as a permanent client error and answers 400, which no client retries", err)
	}
}

// TestRepriceCommitTakesTheUnboundedWriteLock pins Reprice's begin, which is the
// OTHER side of the helper choice from the request-path table test above — and the
// asymmetry is the whole point of the test.
//
// Reprice is the widest read-then-write in the package: it scans every
// token_events row at or above the version floor, buffers the changed ones, and
// only then issues its first UPDATE. That window is the SQLITE_BUSY_SNAPSHOT (517)
// hazard at its largest, and 517 is not retried by busy_timeout.
//
// 🔴 THREE INDEPENDENT PROPERTIES, BECAUSE THREE DIFFERENT MUTANTS EXIST.
//
//   - IT PROMOTES ON COMMIT. Reverting to `d.db.BeginTx(ctx, nil)` compiles and
//     leaves the rest of the tree green: DEFERRED, the scan succeeds and the
//     failure surfaces later at the first UPDATE as a bare "database is locked"
//     that wraps nothing. Asserting the SENTINEL, not merely "an error", is what
//     separates them.
//
//   - THE WAIT IS NOT BOUNDED. Swapping beginImmediate for
//     beginImmediateBounded(ctx, d.db, requestPathBusyTimeout) ALSO wraps the
//     sentinel, so only the clock tells those two apart. It is a real defect and
//     not a stylistic one: Reprice's sole caller is `tierd reprice`
//     (cmd/tierd/reprice.go), never an HTTP handler, so a 250ms cap protects no
//     request — it just fails an operator's history rewrite whenever a live tierd
//     holds the lock for a quarter of a second. The threshold is the mirror of the
//     bounded test's: >= 2s is unreachable for a 250ms cap and comfortably below
//     the 5s the DSN's busy_timeout actually spends.
//
//     ⚠️ THE TWO THRESHOLDS READ AS SYMMETRIC AND ARE NOT. The bounded test's
//     ceiling can be crossed by a correct implementation (#607, ~1 in 20). This
//     floor cannot: crossing it needs a contended promote to return in under 2s,
//     which needs a busy_timeout below the DSN's 5000ms on that connection, and
//     the ONLY thing that lowers it is beginImmediateBounded — never called
//     against this test's database (its own t.TempDir(), its own pool). So this
//     arm's failure mode is mechanically unreachable rather than merely
//     improbable. Measured margin 5.050s / 5.041s / 5.059s against a 2s floor.
//
//   - THE DRY RUN DOES NOT PROMOTE. Commit is false by DEFAULT, so the dry run is
//     the common invocation and it writes nothing. Promoting it unconditionally —
//     the tempting "just always take the lock" simplification — would hold the
//     exclusive write lock across a full-table scan and stall every writer in a
//     live `tierd serve`. This arm requires the dry run to SUCCEED while another
//     connection holds the write lock, which is only possible because it stayed
//     DEFERRED (WAL readers do not block on a writer).
//
// ⏱️ This test pays the full 5s busy_timeout once, for the same unavoidable reason
// documented at the top of this file. t.Parallel() overlaps it with the others.
func TestRepriceCommitTakesTheUnboundedWriteLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "reprice-lock.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	// 🔴 DELIBERATELY 1 — see pinPoolToOne. ARM 3 below is a LEAK DETECTOR: it
	// works by the dry run starving for a connection that a leak never returned.
	// At production's maxOpenConns the mutant leaks exactly ONE connection and the
	// dry run simply takes one of the other three, so the arm would pass WITH THE
	// BUG and its "MEASURED: fails at 30.01s" note would be a claim about a kill
	// the test no longer has. Missed in the first pass of #669 and caught in
	// review.
	pinPoolToOne(t, db)

	// A liveness bound, not a latency one: 30s is far past the 5s this test
	// legitimately waits, and it exists so a begin that LEAKS the pinned single
	// connection fails here instead of hanging the package to the 600s go test
	// timeout with zero named failures.
	//
	// 🔴 IT WORKS EVEN THOUGH THE PROMOTE IGNORES ctx, and the distinction is the
	// reason it is load-bearing rather than decorative: beginImmediate reaches the
	// pool through db.BeginTx(ctx, …), and connection ACQUISITION *is* ctx-bounded
	// even though the promote that follows is not. The promote needs no ctx bound —
	// busy_timeout already makes it finite (5s). So this deadline catches exactly
	// the unbounded case: waiting forever for a connection that was never returned.
	// MEASURED: dropping tx.Rollback() from beginImmediate's promote-failure path
	// fails this test at 30.01s instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A row the reprice will genuinely CHANGE. Inserted through the normal path
	// (so every column is populated the way production populates it), then its
	// cost_micro is corrupted directly so re-deriving it from the token counts
	// produces a different number. Without a changed row a commit writes nothing,
	// and the DEFERRED mutant would sail through on an empty write set for a
	// reason unrelated to the property under test.
	if err := db.InsertTokenEvents(ctx, []TokenEvent{{
		Developer: "alice",
		IssueID:   "issue-42",
		Model:     "claude-sonnet-4",
		InputTok:  1000,
		OutputTok: 500,
		Source:    "jsonl",
		Fidelity:  "estimated",
		Timestamp: time.Now().UTC().Truncate(time.Second),
	}}); err != nil {
		t.Fatalf("seed token_events: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE token_events SET cost_micro = 1, price_version = 1`); err != nil {
		t.Fatalf("corrupt seeded cost: %v", err)
	}

	// AllowGuessed IS LOAD-BEARING, not defensive padding. The seeded row carries
	// no Host, so it is likely priced by the size-class GUESS fallback; without
	// this flag the GUESS gate would fail the commit BEFORE any write, and the
	// control arm below would report a broken fixture rather than a lock property.
	commitOpts := RepriceOptions{FromVersion: 1, Commit: true, AllowGuessed: true}

	// 🔴 CONTROL ARM, LOCK FREE. The commit must SUCCEED and must actually change
	// the row. Without this, a fixture that made Reprice fail early (an empty
	// table, a rejected floor) would leave the locked arm below passing for a
	// reason that has nothing to do with the promote.
	res, err := db.Reprice(ctx, commitOpts)
	if err != nil {
		t.Fatalf("control: committing reprice failed with the lock FREE: %v — the fixture is broken, so the locked arms below prove nothing", err)
	}
	if res.ChangedRowCount == 0 {
		t.Fatalf("control: committing reprice changed 0 rows — the seeded row is not repriceable, so this test would pass against a DEFERRED begin that simply had nothing to write")
	}

	release := holdWriteLockRaw(t, path)
	defer release()

	// ARM 1 + 2: a COMMIT under contention must fail at the promote, and must
	// have waited the unbounded DSN timeout to do it.
	start := time.Now()
	_, err = db.Reprice(ctx, commitOpts)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("committing reprice returned nil while another connection held the write lock")
	}
	if !errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("committing reprice err = %v, want it to wrap ErrWriteLockUnavailable — this site has lost its BEGIN IMMEDIATE and is running DEFERRED again: it scans the whole table first and only discovers the contention at its first UPDATE, where cross-process the failure is an unretried SQLITE_BUSY_SNAPSHOT (517) that discards the entire scan", err)
	}
	// 🔴 AND THE OPERATOR HINT MUST BE ON IT — this pins the ORDER of the two
	// cases in Reprice's begin switch, which errors.Is alone cannot see.
	// MEASURED: reversing them so `case err != nil` runs first makes the
	// contention case DEAD CODE and strips the "quiesced database" hint from
	// exactly the failure it was written for — and left `go test ./...` FULLY
	// GREEN, because both branches wrap the sentinel via %w and the assertion
	// above is satisfied either way. The hint is the only observable difference,
	// so it is what the test has to look at. (The absence half — a NON-contention
	// promote failure must NOT get the hint, or the diagnosis names a cause that
	// did not occur — is pinned separately by
	// TestRepriceNonContentionFailureCarriesNoQuiescedHint.)
	if !strings.Contains(err.Error(), "quiesced") {
		t.Errorf("committing reprice err = %v, want the contention case to attach the \"run the reprice against a quiesced database\" hint. Its absence means the begin switch's `case errors.Is(err, ErrWriteLockUnavailable)` no longer runs FIRST, so the generic `case err != nil` swallowed it: the operator is told a reprice failed but not that a live tierd is holding the lock, which is the one thing that tells them what to do next", err)
	}
	t.Logf("contended committing reprice: %v (DSN busy_timeout = %dms, request-path cap = %v)", elapsed, dsnBusyTimeoutMS, requestPathBusyTimeout)
	if elapsed < 2*time.Second {
		t.Errorf("committing reprice gave up after %v, expected it to wait out the DSN's %dms busy_timeout. This site has moved to the BOUNDED helper, which caps at %v — that cap exists to keep an HTTP handler from blocking five seconds past its client's patience, and from stalling every other in-flight request once maxOpenConns promotes block at once, and Reprice has no HTTP caller at all (cmd/tierd/reprice.go is the only one). Bounding it converts a working operator command into a spurious failure whenever a live tierd holds the lock briefly", elapsed, dsnBusyTimeoutMS, requestPathBusyTimeout)
	}

	// ARM 3: the DRY RUN must still work under the same held lock.
	dry, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, AllowGuessed: true})
	if err != nil {
		// ⚠️ THIS ASSERTION NAMES TWO CAUSES, like ARM 1, because it cannot
		// distinguish them and must not pretend to. MEASURED: dropping
		// tx.Rollback() from beginImmediate's promote-failure path fails this arm
		// at 30.01s with "reprice: begin tx: context deadline exceeded" — the dry
		// run had NOT started promoting; a leaked connection starved the pool
		// (pinned to 1 by this test since #669 — at maxOpenConns the leak is
		// invisible here) and the dry run never got a connection at all. The
		// error text is the discriminator: a genuine promotion failure surfaces as
		// the write-lock sentinel, a leak surfaces as a ctx deadline waiting for a
		// connection.
		t.Errorf("dry-run reprice failed while another connection held the write lock: %v — EITHER the dry run has started promoting (it writes nothing, so it must not contend with a live `tierd serve`: a diagnostic an operator cannot run without taking the database down is a diagnostic nobody runs), OR a connection was LEAKED somewhere in this package and the dry run starved waiting for this test's pinned single connection. If the error is `begin tx: context deadline exceeded` it is the leak, not the promotion — check that every beginImmediate/beginImmediateBounded failure path still rolls back and releases", err)
	} else if dry.RowCount == 0 {
		t.Errorf("dry-run reprice under contention examined 0 rows — it returned successfully without reading anything, so this arm is not actually proving the read went through")
	}
}

// TestRepriceNonContentionFailureCarriesNoQuiescedHint is the ABSENCE half of the
// hint assertion in TestRepriceCommitTakesTheUnboundedWriteLock, and it is the arm
// that makes that hint falsifiable in both directions.
//
// 🔴 WHY BOTH HALVES ARE NEEDED. Reprice's begin switch attaches "is a tierd serve
// ingesting into this database? run the reprice against a quiesced database" to
// the CONTENTION case only. The presence arm alone would be satisfied by the
// laziest possible implementation — stamping the hint onto every begin failure —
// which is precisely the defect the switch exists to avoid, and the same one
// repairrepo.go documents: a read-only or full database is not a busy one, and
// telling that operator to quiesce a `tierd serve` sends them to check something
// that is not the problem. RepairRepo makes the identical split, so this pins the
// contract both functions share.
//
// The fixture is the read-only database from
// TestPromoteFailureThatIsNotContentionIsNotRetryable: Open() refuses such a file
// up front, so the handle is built through the driver and wrapped in a DB
// directly — the same shape the pool produces when it replaces a poisoned
// connection, so this is a state a long-running tierd can reach.
func TestRepriceNonContentionFailureCarriesNoQuiescedHint(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reprice-readonly.db")

	seed, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	ro, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, dsnBusyTimeoutMS))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = ro.Close() }()
	// DELIBERATELY 1, not "same as production" — production is maxOpenConns (4)
	// since #669. A pool of 1 is what makes a leaked connection observable here.
	ro.SetMaxOpenConns(1)

	ctx := context.Background()
	_, err = (&DB{db: ro}).Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true, AllowGuessed: true})

	// CONTROL: the commit must actually FAIL here. If this database turned out to
	// be writable, the absence assertion below would pass vacuously — a hint
	// cannot appear in an error that does not exist.
	if err == nil {
		t.Fatal("control: committing reprice SUCCEEDED against a read-only database — the fixture is not read-only, so the assertions below are untested")
	}
	t.Logf("read-only committing reprice error: %v", err)

	// It must not be classified as contention in the first place...
	if errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("read-only database reports as write-lock contention (%v) — isPromoteContention has stopped discriminating on the SQLite result code, so a permanent condition is being sold as retryable", err)
	}
	// ...and therefore must not carry the hint written for contention.
	if strings.Contains(err.Error(), "quiesced") {
		t.Errorf("read-only committing reprice err = %v, want NO \"quiesced database\" hint. The hint belongs to the contention case ALONE; on a read-only or full database it names a cause that did not occur and sends the operator to stop a `tierd serve` that is not the problem — a diagnosis naming the wrong cause is worse than none", err)
	}
}
