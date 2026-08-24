package store

// Tests for #668 — the conversion of the last DEFERRED read-then-write sites to
// BEGIN IMMEDIATE, which is the precondition for raising SetMaxOpenConns (#669).
//
// 🔴 WHAT THESE PIN, AND WHY A TIMING ASSERTION IS PART OF IT.
//
// Every site below is a read-then-write whose READ is the guard for the WRITE.
// Under the DEFERRED begin they all used to carry (modernc.org/sqlite ignores
// sql.TxOptions.Isolation outright, so a plain BeginTx is DEFERRED), the shape
// fails in two distinct ways, and MEASUREMENT corrected this list once — the
// original draft claimed the second case blocked for five seconds. It does not:
//
//   - against a concurrent COMMIT: SQLITE_BUSY_SNAPSHOT (517), which
//     busy_timeout does NOT retry — an opaque "database is locked" in ~19µs;
//   - against a HELD lock: plain SQLITE_BUSY (5) in ~65µs. Also immediate.
//     SQLite does not invoke the busy handler for a deadlock-prone upgrade, so
//     a transaction holding a read lock does not WAIT for the writer at all.
//
// ⭐ So the pre-#668 defect was never "it stalls". It was an INSTANT failure
// that no busy_timeout would retry and no handler could classify, arriving at
// the client as a permanent 500 in under a millisecond. Taking the write lock up
// front is what converts that into either a wait (unbounded sites) or a
// classifiable, retryable answer (bounded sites).
//
// Both are removed by taking the write lock UP FRONT. So the discriminating
// assertion is not merely "an error came back" — it is WHICH error, and HOW
// FAST. A bounded site must answer with the ErrWriteLockUnavailable sentinel in
// about requestPathBusyTimeout, and an unbounded one must be seen to WAIT.
//
// MEASURED against the pre-#668 implementation, and the two arms kill DIFFERENT
// mutants — which is why both exist:
//
//   - Reverting a bounded site to `d.db.BeginTx(ctx, nil)` is caught by the
//     SENTINEL arm alone. It returns a raw SQLITE_BUSY in ~134µs, so it is FAST
//     and the timing ceiling never fires.
//   - Downgrading a bounded site to the unbounded beginImmediate is caught by
//     the TIMING arm alone. It produces the correct sentinel after ~5.05s, so
//     the sentinel assertion passes.
//
// ⚠️ Neither arm alone covers both. An earlier draft of this header claimed the
// revert failed "on BOTH counts"; it does not, and saying so would have made a
// future reader think either assertion was redundant and safe to drop.
//
// ⚠️ AND NOTE WHICH FAILURE MODE IS REPRODUCED HERE. The regression test below
// builds its contention with a HELD write lock, which yields a plain
// SQLITE_BUSY (5). The 517 shape needs a competing COMMIT mid-transaction and is
// reproduced in beginimmediate_test.go's TestBusySnapshotClassifiesAsContention,
// not in this file. Both are DEFERRED failure modes; only one is exercised here.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertCount runs a COUNT query and requires an exact result. Used by every
// site's verify hook to prove the write LANDED, not merely that no error came
// back.
func assertCount(t *testing.T, db *DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("post-state query %q: %v", query, err)
	}
	if got != want {
		t.Errorf("post-state %q = %d, want %d — the call returned nil but the row is not in the state it claims; several of these methods have a legitimate no-op path that ALSO returns nil, so without this the control arm cannot tell 'wrote it' from 'decided not to'", query, got, want)
	}
}

// convertedSite describes one #668 conversion so every arm below runs against
// every site, rather than a representative one. A site missing from this table
// is a site with no coverage, so the table IS the checklist.
type convertedSite struct {
	name string
	// bounded reports whether the site uses beginImmediateBounded (a request
	// path, capped at requestPathBusyTimeout) rather than the unbounded
	// beginImmediate (a background caller that should wait).
	bounded bool
	// seed prepares whatever rows the call needs in order to reach its write on
	// the UNCONTENDED arm. Without it the control arm could pass by no-opping.
	seed func(t *testing.T, db *DB)
	// call runs the converted method.
	call func(ctx context.Context, db *DB) error
	// verify asserts the POST-STATE of a successful uncontended call.
	//
	// 🔴 WITHOUT THIS, "THE WRITE HAPPENED" IS NEVER PROVEN. Every arm below
	// otherwise asserts only the returned error, and several of these methods
	// have a legitimate no-op path that also returns nil — EndMembership when
	// there is no open membership, UpdateQualityForOutcome when the value is
	// unchanged, ReconcileSubscriptionFee when the delta is zero. A control arm
	// that cannot tell "wrote the row" from "decided not to" is not a control.
	verify func(t *testing.T, db *DB)
}

func convertedSites() []convertedSite {
	return []convertedSite{
		{
			name:    "UpsertHierarchy",
			bounded: true,
			seed:    func(*testing.T, *DB) {},
			call: func(ctx context.Context, db *DB) error {
				return db.UpsertHierarchy(ctx, "alice", "core", "platform", "acme")
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				assertCount(t, db, `SELECT COUNT(*) FROM org_hierarchy WHERE developer='alice' AND team='core'`, 1)
			},
		},
		{
			name:    "UpsertHierarchies",
			bounded: true,
			seed:    func(*testing.T, *DB) {},
			call: func(ctx context.Context, db *DB) error {
				return db.UpsertHierarchies(ctx, []HierarchyRow{
					{Developer: "bob", Team: "core", Division: "platform", Org: "acme"},
				})
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				assertCount(t, db, `SELECT COUNT(*) FROM org_hierarchy WHERE developer='bob' AND team='core'`, 1)
			},
		},
		{
			name:    "EndMembership",
			bounded: true,
			// Seed an OPEN membership, or EndMembership returns its idempotent
			// no-op and the uncontended arm proves nothing about the write.
			seed: func(t *testing.T, db *DB) {
				t.Helper()
				if err := db.UpsertHierarchy(context.Background(), "carol", "core", "platform", "acme"); err != nil {
					t.Fatalf("seed membership: %v", err)
				}
			},
			call: func(ctx context.Context, db *DB) error {
				return db.EndMembership(ctx, "carol", "acme", time.Now().UTC().Format("2006-01"))
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				// The seat must be CLOSED. A nil error also comes back from the
				// idempotent no-op path, which writes nothing.
				assertCount(t, db, `SELECT COUNT(*) FROM period_membership WHERE developer='carol' AND period_end IS NOT NULL`, 1)
			},
		},
		{
			name:    "UpdateQualityForOutcome",
			bounded: true,
			seed: func(t *testing.T, db *DB) {
				t.Helper()
				// 🔴 ASSERT `inserted`. InsertOutcome is ON CONFLICT DO NOTHING,
				// so a seed can silently write nothing and return a nil error.
				inserted, err := db.InsertOutcome(context.Background(), Outcome{
					Developer: "dave", IssueID: "1", PRNumber: 1,
					Weight: 1, Quality: 1, MergeCommitSHA: "deadbeef",
					Timestamp: time.Now().UTC(),
				})
				if err != nil {
					t.Fatalf("seed outcome: %v", err)
				}
				if !inserted {
					t.Fatal("seed outcome inserted NOTHING (ON CONFLICT DO NOTHING) — every arm for this site would then exercise a call that no-ops")
				}
			},
			call: func(ctx context.Context, db *DB) error {
				o, ok, err := db.OutcomeByMergeCommit(ctx, "deadbeef")
				if err != nil {
					return err
				}
				// 🔴 A MISSING FIXTURE MUST BE A LOUD FAILURE, NOT A SILENT
				// SUCCESS. This previously read `if err != nil || !ok { return
				// err }`, which returns NIL when the row is absent — so the
				// uncontended control arm passed WITHOUT EVER CALLING the
				// converted method, and the contended arm then failed while
				// blaming the production conversion for a fixture bug. That is
				// exactly the no-op the `seed` field exists to prevent.
				if !ok {
					return fmt.Errorf("fixture: no outcome for merge commit deadbeef — the seed did not land, so this site's call never reaches UpdateQualityForOutcome")
				}
				// 0.5 differs from the seeded 1.0, so this reaches the UPDATE
				// rather than taking the no-op early return.
				return db.UpdateQualityForOutcome(ctx, o.ID, 0.5, "ci_fail", "ref")
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				// 0.5, not the seeded 1.0 — proves the UPDATE ran rather than
				// the value-unchanged early return.
				assertCount(t, db, `SELECT COUNT(*) FROM outcomes WHERE merge_commit_sha='deadbeef' AND quality=0.5`, 1)
				assertCount(t, db, `SELECT COUNT(*) FROM quality_history WHERE new_quality=0.5`, 1)
			},
		},
		{
			name:    "UpdateQuality",
			bounded: false, // no production caller — see its doc comment
			seed: func(t *testing.T, db *DB) {
				t.Helper()
				if _, err := db.InsertOutcome(context.Background(), Outcome{
					Developer: "erin", IssueID: "2", PRNumber: 2,
					Weight: 1, Quality: 1, Timestamp: time.Now().UTC(),
				}); err != nil {
					t.Fatalf("seed outcome: %v", err)
				}
			},
			call: func(ctx context.Context, db *DB) error {
				return db.UpdateQuality(ctx, "erin", "2", 0.5)
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				assertCount(t, db, `SELECT COUNT(*) FROM outcomes WHERE developer='erin' AND issue_id='2' AND quality=0.5`, 1)
			},
		},
		{
			name:    "ReconcileSubscriptionFee",
			bounded: false, // hourly background ticker
			seed:    func(*testing.T, *DB) {},
			call: func(ctx context.Context, db *DB) error {
				_, err := db.ReconcileSubscriptionFee(ctx, "claude", "acme", "2026-08", 200_000_000)
				return err
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				assertCount(t, db, `SELECT COUNT(*) FROM org_actual_spend WHERE org='acme' AND period='2026-08' AND actual_paid_micro=200000000`, 1)
			},
		},
		{
			name:    "PostSubscriptionFeeIfUnposted",
			bounded: false, // hourly background ticker
			seed:    func(*testing.T, *DB) {},
			call: func(ctx context.Context, db *DB) error {
				_, err := db.PostSubscriptionFeeIfUnposted(ctx, "claude", "acme", "2026-07", 200_000_000)
				return err
			},
			verify: func(t *testing.T, db *DB) {
				t.Helper()
				assertCount(t, db, `SELECT COUNT(*) FROM org_actual_spend WHERE org='acme' AND period='2026-07' AND actual_paid_micro=200000000`, 1)
			},
		},
	}
}

// TestConvertedSitesSucceedUncontended is the CONTROL ARM for the whole file.
//
// 🔴 WITHOUT THIS, THE CONTENTION ARM BELOW IS UNFALSIFIABLE. A call that is
// simply broken — a bad fixture, a validation error, a typo in a seed — returns
// an error under contention too, and the contention test would read that as
// "the site correctly reported contention". This arm proves each call SUCCEEDS
// when nothing holds the lock, so an error in the next test is attributable to
// the lock and nothing else.
func TestConvertedSitesSucceedUncontended(t *testing.T) {
	t.Parallel()
	for _, site := range convertedSites() {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			db, err := Open(filepath.Join(t.TempDir(), "uncontended.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()

			site.seed(t, db)
			if err := site.call(context.Background(), db); err != nil {
				t.Fatalf("control: %s failed with NO competing writer (%v) — every contention assertion in this file would pass for the wrong reason", site.name, err)
			}
			// A nil error is not proof of a write. Several of these methods
			// have a no-op path that also returns nil.
			site.verify(t, db)
			// 🔴 AND THE CONNECTION MUST COME BACK. beginImmediateBounded
			// reserves a *sql.Conn and lowers its busy_timeout; release()
			// restores the timeout and returns it. A site that leaks the conn,
			// or returns it without restoring, passes every assertion above.
			// Since #669 the check is db.Stats().InUse rather than "the next
			// acquisition hangs" — at maxOpenConns a leak strands one of N
			// connections and hangs nothing, so the old symptom-based check
			// would have gone quiet exactly when leaks got harder to see.
			assertConnectionReturned(t, db, site.name)
		})
	}
}

// assertConnectionReturned proves the pool got its connection back, with the
// DSN's busy_timeout restored on it.
//
// 🔴 #669 ADDED THE FIRST ARM, AND WITHOUT IT THIS HELPER WOULD HAVE STOPPED
// DETECTING LEAKS ENTIRELY. The original two arms inferred a leak from a
// SYMPTOM: at SetMaxOpenConns(1), a stranded connection made the next
// acquisition hang, so a short deadline turned the hang into a failure. That
// inference dies at maxOpenConns — leak one of four and the read below simply
// takes another connection and passes. The leak would still be real, still
// unbounded across repeated calls, and nothing here would say so.
//
// db.Stats().InUse is the DIRECT observation: after a call has returned, no
// connection should still be checked out. Nothing under internal/ read
// db.Stats() before #669 — leak detection was entirely a side effect of the pool
// being 1.
//
// ⚠️ IT HOLDS AT ANY POOL SIZE, **PROVIDED NOTHING ELSE IS USING THIS *sql.DB
// CONCURRENTLY** — and that qualifier is load-bearing, so do not drop it when
// reusing this helper. InUse is computed as numOpen - len(freeConn), and
// database/sql increments numOpen BEFORE the dial when its background opener
// services a queued connection request. A concurrent user of the same *sql.DB
// could therefore make this read non-zero with nothing leaked. It is safe here
// because each subtest owns its own *DB under its own TempDir, the store starts
// no goroutines of its own, and every arm is sequential. A future converted site
// that fans out internally would break that, and the failure would look like a
// flaky leak rather than a bad assertion.
//
// The other two arms stay, and are not redundant. The busy_timeout read catches
// the subtler case where the connection came back but with
// beginImmediateBounded's lowered request-path value still installed — a caller
// that later draws it silently inherits a 250ms timeout. The deadline stays
// because a hang is still the symptom if a future change re-pins the pool.
func assertConnectionReturned(t *testing.T, db *DB, site string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Arm 1 — the direct leak assertion. Read BEFORE issuing another query, so
	// nothing this helper does is itself counted as in-use.
	if inUse := db.db.Stats().InUse; inUse != 0 {
		t.Errorf("%s: %d connection(s) still checked out after the call returned — the site leaked its *sql.Conn. At maxOpenConns this does NOT hang (the pool has %d others), so it would repeat until the pool is exhausted and only then surface, as an unrelated timeout somewhere else", site, inUse, maxOpenConns-1)
	}

	// 🔴 ARM 2's PRECONDITION, AND IT IS NOT OPTIONAL AT maxOpenConns. The read
	// below goes through the POOL and is then interpreted as the state of the
	// connection the site borrowed. That inference is only valid while exactly one
	// connection exists — otherwise database/sql hands back an arbitrary one and a
	// restored-vs-unrestored verdict is decided by which slot happened to be free.
	// It holds today because these subtests are sequential so only one connection
	// is ever dialled, but nothing enforced it, and "true by luck" is precisely
	// what pinPoolToOne's doc warns about for readBusyTimeout. Assert it.
	if n := db.db.Stats().OpenConnections; n != 1 {
		t.Fatalf("%s: %d connections open, want exactly 1 — the busy_timeout read below would sample an ARBITRARY connection and its verdict would be luck, not measurement", site, n)
	}

	var ms int
	if err := db.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&ms); err != nil {
		t.Fatalf("%s: could not read busy_timeout back after the call (%v) — on a deadline this means the connection was never returned to the pool and every other pool slot was already taken", site, err)
	}
	if ms != dsnBusyTimeoutMS {
		t.Errorf("%s: busy_timeout after the call = %d, want %d — the connection was returned WITHOUT its timeout restored, so a later caller drawing it inherits the short request-path value", site, ms, dsnBusyTimeoutMS)
	}
}

// TestConvertedBoundedSitesAnswerContentionFast pins that a request-path site
// answers a held write lock with the retryable sentinel, at its CAP.
//
// 🔴 THE TIMING HALF IS NOT DECORATION. Sentinel-only would still pass against
// the unbounded beginImmediate, which produces the same sentinel after waiting
// the DSN's 5000ms — and a 5s stall on an HTTP handler is the exact defect
// requestPathBusyTimeout exists to remove. Asserting BOTH is what separates
// "bounded" from "merely promoted".
func TestConvertedBoundedSitesAnswerContentionFast(t *testing.T) {
	t.Parallel()
	for _, site := range convertedSites() {
		if !site.bounded {
			continue
		}
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "contended.db")
			db, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()

			// Seed BEFORE the lock is taken — seeding is itself a write.
			site.seed(t, db)

			release := holdWriteLock(t, path)
			defer release()

			start := time.Now()
			err = site.call(context.Background(), db)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("%s SUCCEEDED while another connection held the write lock — it is not taking the lock up front", site.name)
			}
			if !errors.Is(err, ErrWriteLockUnavailable) {
				t.Errorf("%s under contention returned %v, which does NOT wrap ErrWriteLockUnavailable — the API layer maps only the sentinel to a retryable 503, so this transient condition would be answered as a permanent 500", site.name, err)
			}
			// Generous ceiling, DERIVED from the cap rather than hard-coded:
			// the cap is requestPathBusyTimeout and SQLite's busy-handler
			// granularity adds ~13ms, so 4x proves the cap is in force while
			// staying clear of CI scheduler noise. The value this rejects is the
			// DSN's 5000ms. (A literal `time.Second` here would fail all four
			// bounded arms spuriously the day someone raises the cap.)
			if elapsed > 4*requestPathBusyTimeout {
				t.Errorf("%s under contention took %v, want well under 4x the cap (cap is %v) — it is waiting the DSN's %dms, so this site is on the UNBOUNDED path and a contended request stalls the caller for five seconds", site.name, elapsed, requestPathBusyTimeout, dsnBusyTimeoutMS)
			}
			t.Logf("%s: contended answer in %v, err=%v", site.name, elapsed, err)
			// 🔴 THE FAILURE PATH IS WHERE A LEAK HIDES — store.go says so on
			// restoreAndReleaseConn, and until #669's review this suite checked
			// the connection only on the SUCCESS path. A site that answers
			// contention correctly and keeps its *sql.Conn passes every assertion
			// above. Release the external lock first so this measures the site's
			// own release, not the fixture's.
			release()
			assertConnectionReturned(t, db, site.name+" (contended)")
		})
	}
}

// TestConvertedUnboundedSitesWaitForTheLock pins the OTHER half of the split:
// the background sites deliberately do NOT carry the request-path cap, so they
// must be seen to wait rather than fail fast.
//
// 🔴 THIS IS THE ARM THAT CATCHES A WELL-MEANING "make it consistent" EDIT.
// Putting requestPathBusyTimeout on a background ticker converts "wait out a
// competing writer" into "fail and retry in an hour", which for
// ReconcileSubscriptionFee means a fee delta that silently does not post until
// the next tick. The assertion is a FLOOR, not a ceiling.
func TestConvertedUnboundedSitesWaitForTheLock(t *testing.T) {
	t.Parallel()
	for _, site := range convertedSites() {
		if site.bounded {
			continue
		}
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "unbounded.db")
			db, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()

			site.seed(t, db)

			release := holdWriteLock(t, path)
			defer release()

			start := time.Now()
			err = site.call(context.Background(), db)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("%s SUCCEEDED while another connection held the write lock — it is not taking the lock up front", site.name)
			}
			if !errors.Is(err, ErrWriteLockUnavailable) {
				t.Errorf("%s under contention returned %v, which does NOT wrap ErrWriteLockUnavailable — a background caller cannot tell a retryable lock conflict from a real fault", site.name, err)
			}
			// The floor is the point: an unbounded site rides the DSN's
			// busy_timeout, so it must be seen to have WAITED. Anything near
			// requestPathBusyTimeout means the cap leaked onto a background path.
			if floor := 2 * requestPathBusyTimeout; elapsed < floor {
				t.Errorf("%s returned in %v, under the %v floor — this site is UNBOUNDED by design (background ticker) and returning at the request-path cap means the bound leaked onto it, turning 'wait out a competing writer' into 'skip this tick'", site.name, elapsed, floor)
			}
			t.Logf("%s: waited %v before reporting contention, err=%v", site.name, elapsed, err)
		})
	}
}

// TestConvertedSitesRejectADeferredRegression documents the pre-#668 shape by
// rebuilding it, and pins the two facts the arms above depend on.
//
// ⚠️ IT DOES NOT PROVE THE OTHER TESTS CAN FAIL, AND AN EARLIER VERSION OF THIS
// COMMENT CLAIMED IT DID. Measured: this test stays GREEN under a revert of a
// bounded site, under a bounded→unbounded downgrade, and under a dropped
// `release()`. It is a hand-built analogue wired to no converted site, so it
// cannot speak for them. What proves those arms can fail is mutation testing,
// recorded in the file header. The claim is narrowed rather than the test
// deleted, because the two things it DOES establish are load-bearing:
//
//  1. holdWriteLock genuinely holds — without which every "contended" arm in
//     this file would be measuring an uncontended call;
//  2. the DEFERRED shape fails FAST (~65µs) rather than waiting the DSN's
//     5000ms, which is why the bounded arms' timing ceiling cannot catch a
//     revert and the sentinel assertion is doing that work alone.
func TestConvertedSitesRejectADeferredRegression(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deferredregression.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	release := holdWriteLock(t, path)
	defer release()

	// The pre-#668 shape, verbatim: DEFERRED begin, read, then write.
	start := time.Now()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("deferred begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prevOrg int
	// The read succeeds — a DEFERRED begin takes no write lock, which is
	// precisely why the failure lands later, at the write, where it is harder
	// to classify.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM org_hierarchy`).Scan(&prevOrg); err != nil {
		t.Fatalf("deferred read: %v", err)
	}
	_, werr := tx.ExecContext(ctx, `
		INSERT INTO org_hierarchy (developer, team, division, org)
		VALUES ('regression', 'core', 'platform', 'acme')
		ON CONFLICT(developer) DO UPDATE SET org=excluded.org`)
	elapsed := time.Since(start)

	if werr == nil {
		t.Fatal("control: the DEFERRED write SUCCEEDED against a held write lock — the lock holder is not holding, so this test cannot demonstrate the regression it exists to demonstrate")
	}
	t.Logf("pre-#668 DEFERRED shape against a held lock: %v after %v", werr, elapsed)

	// 🔴 ASSERT THE TIMING, DO NOT MERELY LOG IT — and assert a CEILING, which
	// is the opposite of what this test's author first assumed.
	//
	// MEASURED: ~65µs. The DEFERRED write does NOT ride the DSN's 5000ms
	// busy_timeout. SQLite does not invoke the busy handler for a
	// deadlock-prone upgrade — a transaction that already holds a read lock
	// cannot safely block waiting for a writer, because the writer may be
	// waiting on it — so it returns SQLITE_BUSY on contact instead.
	//
	// ⭐ THAT MAKES THE PRE-#668 DEFECT WORSE THAN "IT STALLS", WHICH IS WHY THE
	// CEILING IS THE RIGHT ASSERTION. The old shape did not block and eventually
	// succeed; it failed IMMEDIATELY with an error busy_timeout would never
	// retry, and the handler above it had no sentinel to classify — so a
	// perfectly retryable lock conflict reached the client as a permanent 500,
	// in well under a millisecond. The conversion does not make these sites
	// faster to fail; it makes their failure CLASSIFIABLE and, on the unbounded
	// sites, survivable by waiting.
	if ceiling := requestPathBusyTimeout; elapsed > ceiling {
		t.Errorf("the DEFERRED shape took %v, over the %v ceiling — it now appears to be WAITING for the lock, which the deadlock-avoidance rule says it cannot do; this test has stopped reproducing the pre-#668 behaviour it exists to contrast against", elapsed, ceiling)
	}

	// The regression's signature: it does NOT carry the sentinel. That is the
	// whole reason the conversion was needed — an unclassifiable error on a
	// request path becomes a 500, not a retryable 503.
	//
	// ⚠️ THIS ARM IS WEAK AND IS LABELLED AS SUCH. Only this package's two
	// helpers ever attach the sentinel, so a raw driver error cannot carry it
	// and the assertion is near-unfalsifiable by construction. It is kept as a
	// tripwire for someone wrapping the sentinel somewhere new, NOT as evidence
	// that the arms above discriminate. The timing assertion below is the one
	// that pins a real, measurable property.
	if errors.Is(werr, ErrWriteLockUnavailable) {
		t.Error("the DEFERRED shape produced ErrWriteLockUnavailable — only beginImmediate/beginImmediateBounded wrap that sentinel, so if a raw driver error now carries it, the contention arms above can no longer tell a converted site from an unconverted one")
	}
}

// TestConvertedSiteTableIsAnExhaustiveCensus makes the table above FAIL when a
// new site is converted without being added to it.
//
// 🔴 WITHOUT THIS, "THE TABLE IS THE CHECKLIST" IS A COMMENT, NOT A GUARD. A
// missing entry does not fail anything — it silently reduces coverage, and the
// suite stays green while a newly-converted site has no contention test at all.
// That is the project's standing bug class in its purest form: a green that
// means "never ran", produced by an omission rather than a defect.
//
// It censuses the SOURCE (go/ast over the non-test files of this package) rather
// than trusting a hand-kept list, then asserts three things:
//  1. the exact set of functions calling each helper — so ANY new call site
//     fails here until a human classifies it;
//  2. every site in convertedSites() really does call the helper its `bounded`
//     flag claims — so the flag cannot silently disagree with the code;
//  3. every #668-converted site is present in the table.
//
// The pattern is borrowed from computecost_callers_test.go, which censuses its
// callers by AST for the same reason.
func TestConvertedSiteTableIsAnExhaustiveCensus(t *testing.T) {
	t.Parallel()

	// The full inventory, as of #668. A new entry here is a deliberate act: add
	// the site to convertedSites() too, or state in the diff why it needs no
	// contention coverage.
	wantBounded := map[string]bool{
		// #598 / #346 / #610 — request-path writers converted before #668.
		"UpsertDeveloperAlias": true, "EraseDeveloper": true,
		"CorrectManualCostEvent": true, "InsertManualCostEvent": true,
		// #668.
		"UpsertHierarchy": true, "UpsertHierarchies": true,
		"EndMembership": true, "UpdateQualityForOutcome": true,
	}
	wantUnbounded := map[string]bool{
		// Open()-time migrations — never a request.
		"dropActualSpendNonNegativeCheck": true, "migrateCacheWriteSplit": true,
		"migrateCostUSDToMicro": true, "migrateActualSpendToMicro": true,
		"backfillPeriodMembership": true, "backfillPriceVersion": true,
		"recomputeKnownSourceCosts": true,
		// Operator CLI commit paths.
		"Reprice": true, "RepairRepo": true,
		// #668 background sites.
		"ReconcileSubscriptionFee": true, "PostSubscriptionFeeIfUnposted": true,
		"UpdateQuality": true,
		// #673 — the two retention DELETEs in PruneWebhookPayloads share one
		// transaction because the row-cap pass reads what the age pass wrote.
		// UNBOUNDED, and deliberately so: its only caller is Open(), a background
		// path with no request to answer quickly. It is NOT in convertedSites()
		// because that table's arms assert contention BEHAVIOUR at request path
		// (sentinel + timing), and neither is meaningful for a method that runs
		// once at boot before anything else can contend with it — what needs
		// pinning here is that the enclosure EXISTS, which is this census.
		"PruneWebhookPayloads": true,
	}
	// #673: beginRead's callers, censused for the same reason as the two write
	// helpers — a read transaction is a deliberate act with a WAL cost (it pins
	// the snapshot a passive checkpoint needs), so a new one must be classified
	// rather than absorbed. ⚠️ NOTE THE ASYMMETRY: a MISSING beginRead is the
	// dangerous direction (a multi-read method silently reading several
	// snapshots), and no census can see a call that was never written. This
	// catches the other half — a new caller arriving without its bound argument.
	wantRead := map[string]bool{
		"ExportDeveloper": true,
	}

	gotBounded, gotUnbounded, gotRead := censusHelperCallers(t)

	// 🔴 CONTROL: the census must actually find things. An AST walk that matched
	// nothing would make every set comparison below vacuously satisfiable in the
	// "missing" direction and is exactly the empty-pattern false green this
	// project has hit before.
	if len(gotBounded) == 0 || len(gotUnbounded) == 0 || len(gotRead) == 0 {
		t.Fatalf("control: the AST census found %d bounded, %d unbounded and %d read call sites — at least one is ZERO, so the walk is not matching and every assertion below proves nothing", len(gotBounded), len(gotUnbounded), len(gotRead))
	}

	assertSameSet(t, "beginImmediateBounded", wantBounded, gotBounded)
	assertSameSet(t, "beginImmediate", wantUnbounded, gotUnbounded)
	assertSameSet(t, "beginRead", wantRead, gotRead)

	// The table's flags must agree with the source, not merely coexist with it.
	for _, site := range convertedSites() {
		inB, inU := gotBounded[site.name], gotUnbounded[site.name]
		if !inB && !inU {
			t.Errorf("convertedSites() lists %q but no call to either helper was found in it — the table names a site that is not converted", site.name)
			continue
		}
		if site.bounded != inB {
			t.Errorf("convertedSites()[%q].bounded = %v, but the SOURCE calls beginImmediateBounded = %v — the table's flag disagrees with the code, so the arm that runs against this site is the wrong one and its pass means nothing", site.name, site.bounded, inB)
		}
	}

	// And every #668 site must be IN the table.
	for _, name := range []string{
		"UpsertHierarchy", "UpsertHierarchies", "EndMembership", "UpdateQualityForOutcome",
		"ReconcileSubscriptionFee", "PostSubscriptionFeeIfUnposted", "UpdateQuality",
	} {
		found := false
		for _, site := range convertedSites() {
			if site.name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("#668 converted %q but convertedSites() does not list it — that site has NO contention coverage and nothing else would have told you", name)
		}
	}
}

// censusHelperCallers returns the set of function names in this package's
// non-test sources that call each helper.
func censusHelperCallers(t *testing.T) (bounded, unbounded, read map[string]bool) {
	t.Helper()
	bounded, unbounded, read = map[string]bool{}, map[string]bool{}, map[string]bool{}

	// parser.ParseFile over an explicit file list rather than parser.ParseDir,
	// which is deprecated as of Go 1.25.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch id.Name {
				case "beginImmediateBounded":
					// Skip the helper's own definition, not its callers.
					if fn.Name.Name != "beginImmediateBounded" {
						bounded[fn.Name.Name] = true
					}
				case "beginImmediate":
					if fn.Name.Name != "beginImmediate" {
						unbounded[fn.Name.Name] = true
					}
				case "beginRead":
					// Skip the helper's own definition, not its callers.
					if fn.Name.Name != "beginRead" {
						read[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	// 🔴 CONTROL: a file-walk that parsed NOTHING would return two empty sets,
	// which reads downstream as "no new call sites" — a vacuous pass of exactly
	// the shape this test exists to prevent.
	if parsed == 0 {
		t.Fatal("census parsed ZERO non-test files — the walk is broken, so every set comparison built on it is vacuous")
	}
	return bounded, unbounded, read
}

func assertSameSet(t *testing.T, helper string, want, got map[string]bool) {
	t.Helper()
	for name := range got {
		if !want[name] {
			t.Errorf("NEW %s call site in %q, not in this test's inventory — classify it: if it is a converted read-then-write site it also needs an entry in convertedSites(), or it ships with no contention coverage", helper, name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%q no longer calls %s — a site was reverted or renamed; if that was deliberate, remove it from this test's inventory in the same commit", name, helper)
		}
	}
}
