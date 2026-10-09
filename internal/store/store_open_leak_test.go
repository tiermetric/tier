package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOpen_DoesNotLeakHandleOnMigrationFailure is the regression guard for #316:
// every error return in Open AFTER sql.Open succeeds must release the underlying
// *sql.DB, whose pooled connections hold the db file plus the WAL -wal/-shm
// handles open. (This said "SetMaxOpenConns(1) pooled connection" until #669
// raised the pool to maxOpenConns. The guard is unaffected — the fd probe below
// counts handles, and a leak strands at least one connection's worth per
// iteration whatever the pool size — but the singular was about to become wrong.)
// Before the fix, only the early gates (ping, version
// read, refuse-if-newer) closed the handle; every migration-phase branch
// (`apply schema:`, the addColumnIfMissing ALTERs, `migrate ... drop check:`, the
// data migrations, the post-migration indexes, the webhook prune) returned a nil
// *DB while stranding the open connection.
//
// The probe is resource-level and portable across the darwin/linux hosts this
// project builds on: enumerate the process's open file descriptors (via /dev/fd,
// which lists them on both platforms) before and after a batch of failing Opens.
// Each Open is tripped through the `apply schema:` branch by the same view
// injection assertViewTripsApplySchema uses (indexing a VIEW named `outcomes` is
// rejected), one of the branches that leaked. A leak strands one connection's
// worth of handles per iteration, so the fd count climbs ~iterations; with the
// deferred success-guarded Close it stays flat.
//
// Negative proof: reverting Open's fix (dropping the `defer ... db.Close()`) makes
// the fd delta jump to roughly `iterations`, tripping the threshold below; the
// #153 failure-injection tests cannot catch this because they only assert the
// returned *DB is nil, which is true in BOTH the leaking and fixed code.
//
// Platform note: /dev/fd exists only on POSIX, so on Windows -- the platform where
// this leak is WORST (an open handle blocks deletion of the DB file for a caller
// that recovers-and-retries, per Open's own doc) -- this test Skips. That is
// acceptable because the leaking code is platform-identical: the defer either fires
// or it doesn't, so catching a regression on the darwin/linux hosts this POC builds
// on catches it everywhere. A future Windows port should not read this Skip as
// coverage of the Windows-critical case.
//
// Concurrency note: the probe reads PROCESS-GLOBAL fd state, so it is only sound
// while nothing else in the process churns fds between the two samples. The store
// package is deliberately serial (no t.Parallel anywhere -- see prices_test.go),
// and this test must NOT add t.Parallel or be raced against another fd-touching
// test, or the before/after delta becomes non-deterministic.
func TestOpen_DoesNotLeakHandleOnMigrationFailure(t *testing.T) {
	fdCount := func() int {
		// /dev/fd lists one entry per open descriptor on macOS (a real directory)
		// and on Linux (a symlink to /proc/self/fd). Readdirnames (not ReadDir,
		// whose per-entry stat is rejected for /dev/fd's live fd entries on macOS)
		// yields the raw fd names. The directory handle itself opens and closes one
		// transient fd, identically on both calls, so it cancels out of the delta.
		// If /dev/fd is not enumerable (e.g. a non-POSIX host), skip rather than
		// assert nothing.
		f, err := os.Open("/dev/fd")
		if err != nil {
			t.Skipf("cannot open /dev/fd to enumerate descriptors on this platform: %v", err)
		}
		defer func() { _ = f.Close() }()
		names, err := f.Readdirnames(-1)
		if err != nil {
			t.Skipf("cannot enumerate open file descriptors on this platform: %v", err)
		}
		return len(names)
	}

	// Pre-seed independent view-injected databases and close every seeding handle
	// up front, so the fd delta measured across the failing Opens reflects ONLY
	// Open's own leak, not the seed connections. A fresh path per iteration keeps
	// each Open independent (no shared pool, no WAL carried between them).
	const iterations = 40
	dir := t.TempDir()
	paths := make([]string, iterations)
	for i := range paths {
		p := filepath.Join(dir, fmt.Sprintf("leak-%d.db", i))
		seedOutcomesView(t, p)
		paths[i] = p
	}

	before := fdCount()
	for _, p := range paths {
		db, err := Open(p)
		if db != nil {
			_ = db.Close()
			t.Fatalf("Open returned a usable *DB for a view-injected schema (%s); want nil", p)
		}
		if err == nil {
			t.Fatalf("Open on a view-injected schema (%s) succeeded; want error", p)
		}
	}
	after := fdCount()

	// With the fix, Close runs on each failure and the delta is ~0; a per-Open
	// leak of even a single handle would push the delta to at least `iterations`.
	// Half of `iterations` sits comfortably above unrelated runtime fd churn
	// (a handful at most) and well below the leak signal.
	if grew := after - before; grew > iterations/2 {
		t.Errorf("open fd count grew by %d across %d failed Opens (before=%d, after=%d); a migration-failure branch is leaking the *sql.DB handle (#316)", grew, iterations, before, after)
	}
}

// TestOpen_DoesNotLeakHandleOnPriceTableCollision extends the #316 leak guard to
// the #714 refusal branch — a NEW early return in Open (Phase 1.5) that sits
// between `apply schema:` and every migration phase the test above exercises.
//
// 🔑 THIS IS THE ARM MOST EASILY FORGOTTEN, and its absence is invisible: a
// refusal that leaks is still a refusal, so every other #714 test passes while
// the handle strands. It matters most precisely because this refusal is
// RECOVERABLE — an operator meets the collision, runs `tierd prices
// forget-version --commit`, and re-opens the same file; on Windows a stranded
// handle blocks exactly that retry (see the platform note on the test above,
// whose /dev/fd probe, Skip behaviour and no-t.Parallel requirement this test
// shares).
//
// The refusal is driven the way TestOpen_RefusesCollidingPriceTable drives it:
// record one synthetic table under version 4245, then swap the active table for
// a different one at the same version.
func TestOpen_DoesNotLeakHandleOnPriceTableCollision(t *testing.T) {
	fdCount := func() int {
		f, err := os.Open("/dev/fd")
		if err != nil {
			t.Skipf("cannot open /dev/fd to enumerate descriptors on this platform: %v", err)
		}
		defer func() { _ = f.Close() }()
		names, err := f.Readdirnames(-1)
		if err != nil {
			t.Skipf("cannot enumerate open file descriptors on this platform: %v", err)
		}
		return len(names)
	}

	const iterations = 40
	const collidingVersion = 4245
	dir := t.TempDir()
	restoreDefaultPriceTable(t)

	// Seed every database with the FIRST table's identity, closing each seeding
	// handle immediately so the delta below measures only Open's own leak.
	first := writePriceTable(t, dir, "leak-first.yaml", collidingVersion, 3.00)
	if _, err := LoadPriceTable(first); err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	paths := make([]string, iterations)
	for i := range paths {
		p := filepath.Join(dir, fmt.Sprintf("pt-leak-%d.db", i))
		db, err := Open(p)
		if err != nil {
			t.Fatalf("seed Open %s: %v", p, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close seed handle %s: %v", p, err)
		}
		paths[i] = p
	}

	// Swap in a DIFFERENT table at the SAME version: every Open below now trips
	// the Phase 1.5 refusal.
	second := writePriceTable(t, dir, "leak-second.yaml", collidingVersion, 3.01)
	if _, err := LoadPriceTable(second); err != nil {
		t.Fatalf("LoadPriceTable(second): %v", err)
	}

	before := fdCount()
	for _, p := range paths {
		db, err := Open(p)
		if db != nil {
			_ = db.Close()
			t.Fatalf("Open returned a usable *DB for a colliding price table (%s); want nil", p)
		}
		if err == nil {
			t.Fatalf("Open on a colliding price table (%s) succeeded; want error", p)
		}
		// Assert WHICH refusal fired. Without this the loop still "passes" if Open
		// starts failing for an unrelated reason, and the probe would then be
		// measuring the leak behaviour of a completely different branch.
		if !strings.Contains(err.Error(), "table_hash") {
			t.Fatalf("Open(%s) failed for the wrong reason: %v", p, err)
		}
	}
	after := fdCount()

	if grew := after - before; grew > iterations/2 {
		t.Errorf("open fd count grew by %d across %d price-table-collision refusals (before=%d, after=%d); the #714 Phase 1.5 early return is leaking the *sql.DB handle (#316)", grew, iterations, before, after)
	}
}

// seedOutcomesView writes a SQLite database at path whose `outcomes` name is a
// VIEW, not a table. Open's Phase-1 schemaTables tries to build an index ON
// outcomes, which SQLite refuses for a view ("views may not be indexed"), so
// Open fails with the `apply schema:` wrap AFTER opening its connection -- the
// leak window #316 closes. Mirrors assertViewTripsApplySchema's injection but
// returns nothing, so it composes in a loop.
func seedOutcomesView(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open for seed %s: %v", path, err)
	}
	if _, err := raw.Exec("CREATE VIEW outcomes AS SELECT 1 AS developer"); err != nil {
		_ = raw.Close()
		t.Fatalf("seed outcomes view %s: %v", path, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed handle %s: %v", path, err)
	}
}
