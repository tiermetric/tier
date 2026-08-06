package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDispatch_ServeReturnsExitCode pins the #146 restructure: runServe returns
// an exit code instead of calling os.Exit, so a post-store.Open failure unwinds
// through the deferred db.Close (and, when a watcher is live, the drain) rather
// than skipping every defer. A bad --watch-repo (no .git) fails resolveWatchRepos
// AFTER the DB is open — on main this path was os.Exit(1), which would KILL this
// test process; on the branch it returns 1. Reaching this assertion at all is the
// proof that serve now returns.
func TestDispatch_ServeReturnsExitCode(t *testing.T) {
	// Clear ambient TIER_* secrets so an operator's environment (e.g. a
	// TIER_API_TOKEN=@/missing that would hit an earlier os.Exit in
	// resolveSecretFlag) can't kill this test process before the path we mean
	// to exercise. t.Setenv also restores them after the test.
	for _, k := range []string{
		"TIER_API_TOKEN", "TIER_READ_TOKEN", "TIER_WEBHOOK_SECRET",
		"TIER_PRICES", "TIER_AGGREGATION", "TIER_K_ANONYMITY", "TIER_PUSH_CAPTURE",
	} {
		t.Setenv(k, "")
	}

	dir := t.TempDir()
	db := filepath.Join(dir, "p306.db")
	notARepo := filepath.Join(dir, "not-a-git-repo") // exists, but has no .git

	var out, errOut bytes.Buffer
	code := dispatch([]string{
		"serve",
		"--db", db,
		"--addr", "127.0.0.1:0",
		"--aggregation", "developer", // required (#185); pass explicitly
		"--watch-repo", notARepo, // fails resolveWatchRepos after store.Open
	}, &out, &errOut)

	if code != 1 {
		t.Fatalf("dispatch(serve, bad --watch-repo) = %d, want 1 (runServe must RETURN, not os.Exit)", code)
	}
}

// TestServe_ConfigErrorPathJoinsBackgroundWriters pins #113 review Y5: the
// post-store.Open `return 1` paths that never reach shutdownServer must STILL
// join the background writers before the deferred db.Close runs.
//
// Those returns (the Codex-rollout config errors) unwind past the whole shutdown
// sequence into `defer db.Close()`, which would close the pool underneath an open
// reconciler transaction — the precise race shutdownServer's own doc says the
// join exists to prevent "rather than relying on the race being small". A defer
// registered after both writers start closes it, and LIFO puts it before
// db.Close.
//
// HOW IT DISCRIMINATES, and why it is not a timing test. A goroutine-liveness
// probe was tried first and MEASURED USELESS: with the defer removed it still
// passed 20/20, because the deferred watcherCancel (registered earlier, so it
// runs after) stops the reconciler anyway and db.Close is slow enough that the
// goroutine has almost always exited by the time dispatch returns. The race is
// real but narrow — which is exactly why it needs a structural pin rather than
// an observational one.
//
// So the signal is the join's own report. joinBackgroundWriters returns whether
// either writer was still LIVE when it was called, and on this path that is a
// certainty rather than a race: nothing has cancelled the background context yet,
// so both writers are necessarily running, and the join is what stops them. It
// logs one line when that happens. Registered → the line appears. Not registered
// → nothing calls the join at all and the line cannot appear.
func TestServe_ConfigErrorPathJoinsBackgroundWriters(t *testing.T) {
	for _, k := range []string{
		"TIER_API_TOKEN", "TIER_READ_TOKEN", "TIER_WEBHOOK_SECRET",
		"TIER_PRICES", "TIER_AGGREGATION", "TIER_K_ANONYMITY", "TIER_PUSH_CAPTURE",
		"TIER_CODEX_ROLLOUT",
	} {
		t.Setenv(k, "")
	}

	// This test drives a REAL `serve`, which swaps the package-global price table
	// for the --prices override below and never puts it back — leaking a 4-model
	// table into every later test in this package (it took out
	// TestRunRepriceCmd_CommitApplies). Restore the embedded default on cleanup.
	restoreEmbeddedPriceTable(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	// A price table carrying the subscription route the config below pays for —
	// no subscription row ships in the embedded table, and the startup coverage
	// gate is FATAL without a match, so the reconciler would never start.
	prices := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(prices, []byte("version: 1\neffective_date: \"2026-08-01\"\nmodels:\n"+
		"  \"glm-5.2@ollama.com\": { input_per_m: 0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }\n"+
		"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n"+
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n"+
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"), 0o600); err != nil {
		t.Fatalf("write prices: %v", err)
	}
	// A long backfill, so the reconciler is still working when the Codex error
	// fires. Derived from the clock, and kept inside the 240-period bound.
	activeSince := fmt.Sprintf("%d-01", time.Now().UTC().Year()-15)
	cfgPath := filepath.Join(dir, "tier.yaml")
	if err := os.WriteFile(cfgPath, []byte("subscriptions:\n"+
		"  - route_prefix: \"glm-5.2@ollama.com\"\n    org: \"acme\"\n    monthly_fee_usd: 100\n"+
		"    active_since: \""+activeSince+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// An empty HOME makes os.UserHomeDir fail, which is what makes
	// codexrollout.New return an error — the reachable post-store.Open `return 1`.
	t.Setenv("HOME", "")

	// serve writes its startup log and this failure to os.Stderr DIRECTLY (not to
	// dispatch's writer), so capture the real file. Without it this test could not
	// tell "failed on the Codex path" from "failed earlier for some other reason"
	// — and a failure that never started the reconciler would pass vacuously.
	stderr := captureOSStderr(t)

	var out, errOut bytes.Buffer
	code := dispatch([]string{
		"serve",
		"--db", filepath.Join(dir, "tier.db"),
		"--addr", "127.0.0.1:0",
		"--aggregation", "developer",
		"--prices", prices,
		"--config", cfgPath,
		"--watch-repo", repo,
		"--codex-rollout",
	}, &out, &errOut)
	logged := stderr()

	if code != 1 {
		t.Fatalf("dispatch = %d, want 1 — this test needs the Codex config-error path, not a clean start:\n%s", code, logged)
	}
	if !strings.Contains(logged, "codex-rollout:") {
		t.Fatalf("serve failed somewhere other than the Codex config error:\n%s", logged)
	}
	// The discriminator only exists if the writers actually started: a run that
	// failed before the reconciler launched would have nothing to join, and the
	// absence of the line below would mean nothing.
	if !strings.Contains(logged, "subscription fee reconciler enabled") {
		t.Fatalf("the fee reconciler never started, so this test proves nothing:\n%s", logged)
	}
	if !strings.Contains(logged, "drained background writers before closing the database") {
		t.Errorf("serve returned on the Codex config-error path WITHOUT joining its background writers — "+
			"the deferred db.Close can close the pool underneath the reconciler's open transaction:\n%s", logged)
	}
}

// TestJoinBackgroundWriters_ReportsWhoItStopped is the unit control for the
// signal the test above relies on. If the "still live" report were wrong in
// either direction that test would be measuring nothing: always-true would pass
// even against a build that had already drained the writers, always-false would
// fail forever.
func TestJoinBackgroundWriters_ReportsWhoItStopped(t *testing.T) {
	cases := []struct {
		name             string
		supLive, feeLive bool
		want             bool
	}{
		{"both already drained (the orderly shutdown)", false, false, false},
		{"reconciler still live", false, true, true},
		{"watcher still live", true, false, true},
		{"both still live (the abrupt exit)", true, true, true},
	}
	// chanFor returns a closed channel for an already-drained writer, or one that
	// closes shortly after cancel for a live one (so the join really waits).
	chanFor := func(live bool) <-chan struct{} {
		ch := make(chan struct{})
		if !live {
			close(ch)
			return ch
		}
		go func() {
			t := time.NewTimer(5 * time.Millisecond)
			defer t.Stop()
			<-t.C
			close(ch)
		}()
		return ch
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cancelled bool
			logger, logs := captureErrorLogger()
			got := joinBackgroundWriters(func() { cancelled = true },
				chanFor(tc.supLive), chanFor(tc.feeLive), 5*time.Second, logger)
			if got != tc.want {
				t.Errorf("joinBackgroundWriters reported live=%v, want %v", got, tc.want)
			}
			if !cancelled {
				t.Error("the background context was not cancelled — the join would wait on goroutines nobody told to stop")
			}
			if logs.Len() != 0 {
				t.Errorf("guard timer fired spuriously: %q", logs.String())
			}
		})
	}
}

// captureOSStderr redirects os.Stderr to a temp file for the rest of the test
// and returns a func that reads back what was written. A FILE, not an os.Pipe:
// a pipe's 64KB buffer would deadlock the process under test if serve out-logged
// it, and nothing here reads concurrently.
func captureOSStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("capture stderr: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	// ⚠️ slog.Default() must be restored too, not just os.Stderr.
	// runServeWithOptions calls slog.SetDefault(newLogger(os.Stderr, ...)) while
	// the swap is in place, so without this the package default logger outlives
	// the test pointing at a CLOSED fd — and a later slog.Default().Warn neither
	// panics nor errors, it silently drops the line. Latent today (backfill.go is
	// the only slog.Default() consumer and no test asserts its output), but it is
	// exactly the global-state leak this file just fixed for the price table.
	origLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(origLogger); os.Stderr = orig; _ = f.Close() })
	return func() string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("read captured stderr: %v", err)
		}
		return string(b)
	}
}

// captureErrorLogger returns a logger whose ERROR lines land in the returned
// buffer, for asserting shutdownServer's wedged-watcher warning.
func captureErrorLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	return slog.New(h), &buf
}

// closedChan returns an already-closed channel — the "nothing to join here"
// argument for a shutdown join the calling subtest is not exercising.
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestShutdownServer_FeeReconcilerJoin is the subscription-fee twin of the
// watcher join below (#113). The reconciler opens TRANSACTIONS, and its #155
// startup catch-up is the longest-running write this process issues, so
// shutdown must wait for it before the caller's deferred db.Close — and must
// still give up after drainTimeout rather than ignore SIGTERM forever.
func TestShutdownServer_FeeReconcilerJoin(t *testing.T) {
	t.Run("late-draining reconciler: waits for the join", func(t *testing.T) {
		feeDone := make(chan struct{})
		const drainDelay = 40 * time.Millisecond
		logger, logs := captureErrorLogger()

		start := time.Now()
		go func() {
			tm := time.NewTimer(drainDelay)
			defer tm.Stop()
			<-tm.C
			close(feeDone)
		}()
		shutdownServer(func() {}, closedChan(), feeDone, &http.Server{}, 5*time.Second, time.Second, logger)

		if elapsed := time.Since(start); elapsed < drainDelay {
			t.Errorf("shutdown returned in %v, before the reconciler drained (%v) — it did not join, so db.Close could race an open transaction", elapsed, drainDelay)
		}
		if logs.Len() != 0 {
			t.Errorf("guard timer fired spuriously: %q", logs.String())
		}
	})

	t.Run("wedged reconciler: proceeds after the guard timer with an ERROR", func(t *testing.T) {
		feeDone := make(chan struct{}) // never closed
		const drainTimeout = 30 * time.Millisecond
		logger, logs := captureErrorLogger()

		start := time.Now()
		shutdownServer(func() {}, closedChan(), feeDone, &http.Server{}, drainTimeout, time.Second, logger)

		if elapsed := time.Since(start); elapsed < drainTimeout {
			t.Errorf("shutdown returned in %v, before the %v guard timer — it did not wait", elapsed, drainTimeout)
		}
		if !bytes.Contains(logs.Bytes(), []byte("subscription fee reconciler failed to drain")) {
			t.Errorf("expected a wedged-reconciler ERROR log, got %q", logs.String())
		}
	})
}

// TestShutdownServer_WatcherJoin exercises the #146 shutdown sequence in
// isolation: it must cancel the watcher context, then wait on supDone (the
// supervisor join) before draining HTTP — but never wait past drainTimeout, so a
// wedged watcher cannot make shutdown ignore SIGTERM forever.
func TestShutdownServer_WatcherJoin(t *testing.T) {
	t.Run("already-drained watcher: returns promptly, no error", func(t *testing.T) {
		supDone := make(chan struct{})
		close(supDone) // watcher already joined
		var cancelled bool
		logger, logs := captureErrorLogger()

		start := time.Now()
		shutdownServer(func() { cancelled = true }, supDone, closedChan(), &http.Server{},
			5*time.Second, time.Second, logger)
		elapsed := time.Since(start)

		if !cancelled {
			t.Error("watcherCancel was not called")
		}
		if elapsed > time.Second {
			t.Errorf("shutdown took %v with supDone pre-closed; join should be instant", elapsed)
		}
		if logs.Len() != 0 {
			t.Errorf("unexpected error log: %q", logs.String())
		}
	})

	t.Run("late-draining watcher: waits for the join, no timeout", func(t *testing.T) {
		supDone := make(chan struct{})
		const drainDelay = 40 * time.Millisecond
		logger, logs := captureErrorLogger()

		// Capture start BEFORE launching the drainer so the drainer's timer
		// deadline (drainerStart + drainDelay) is provably >= start + drainDelay;
		// otherwise elapsed could dip just under drainDelay on a fast machine and
		// flake the lower-bound assertion.
		start := time.Now()
		go func() {
			t := time.NewTimer(drainDelay)
			defer t.Stop()
			<-t.C
			close(supDone) // watcher drains a little after cancel
		}()
		// drainTimeout (5s) >> drainDelay, so the guard must NOT fire.
		shutdownServer(func() {}, supDone, closedChan(), &http.Server{}, 5*time.Second, time.Second, logger)
		elapsed := time.Since(start)

		if elapsed < drainDelay {
			t.Errorf("shutdown returned in %v, before the watcher drained (%v) — it did not join", elapsed, drainDelay)
		}
		if logs.Len() != 0 {
			t.Errorf("guard timer fired spuriously: %q", logs.String())
		}
	})

	t.Run("wedged watcher: proceeds after the guard timer with an ERROR", func(t *testing.T) {
		supDone := make(chan struct{}) // never closed
		const drainTimeout = 30 * time.Millisecond
		logger, logs := captureErrorLogger()

		start := time.Now()
		shutdownServer(func() {}, supDone, closedChan(), &http.Server{}, drainTimeout, time.Second, logger)
		elapsed := time.Since(start)

		if elapsed < drainTimeout {
			t.Errorf("shutdown returned in %v, before the %v guard timer — it did not wait", elapsed, drainTimeout)
		}
		if !bytes.Contains(logs.Bytes(), []byte("watcher failed to drain")) {
			t.Errorf("expected a wedged-watcher ERROR log, got %q", logs.String())
		}
	})
}

// TestServe_WedgedWriterIsJoinedOnce is the control arm for the `shutdownJoined`
// one-shot flag in runServeWithOptions.
//
// joinBackgroundWriters is idempotent when the done channels are CLOSED, so on a
// healthy shutdown a second call is a silent no-op and nothing detects a missing
// flag. It is NOT idempotent when a writer is WEDGED: it burns its full
// drainTimeout per still-open channel and gives up. Without the flag, an orderly
// shutdown that timed out joins twice — once in shutdownServer, once again in the
// deferred abrupt-exit join — which at the shipped 15s turns a wedged SIGTERM
// drain into 30s (60s if both writers are wedged), duplicates the timeout ERROR,
// and re-samples live=true so the "startup aborted before the shutdown sequence"
// Info fires on a plain SIGTERM, asserting an abort that did not happen.
//
// GUARD COVERAGE: delete either half of the flag (the `if shutdownJoined { return }`
// early return, or a `shutdownJoined = true` assignment) and this test fails on
// the elapsed-time assertion.
func TestServe_WedgedWriterIsJoinedOnce(t *testing.T) {
	// Lower the drain so one timeout is ~120ms rather than 15s. The assertion is
	// about ONE timeout vs TWO, so the absolute value only has to be big enough
	// to be unambiguous and small enough to run in `make check`.
	const drain = 120 * time.Millisecond
	orig := watcherDrainTimeout
	watcherDrainTimeout = drain
	t.Cleanup(func() { watcherDrainTimeout = orig })

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A wedged reconciler: feeDone never closes, so every join must wait out the
	// full drain. supDone is already closed, isolating the cost to one writer.
	supDone := make(chan struct{})
	close(supDone)
	feeDone := make(chan struct{})

	// Model runServeWithOptions' exact structure: the deferred abrupt-exit join
	// guarded by the flag, and the orderly path setting it.
	elapsed := func(orderly bool) time.Duration {
		start := time.Now()
		func() {
			shutdownJoined := false
			defer func() {
				if shutdownJoined {
					return
				}
				joinBackgroundWriters(func() {}, supDone, feeDone, watcherDrainTimeout, logger)
			}()
			if orderly {
				joinBackgroundWriters(func() {}, supDone, feeDone, watcherDrainTimeout, logger)
				shutdownJoined = true
			}
		}()
		return time.Since(start)
	}

	// Thresholds sit MIDWAY between one drain and two, not at the boundary. One
	// join measures ~1.0x, two measure ~2.0x; a 2x threshold rejected the mutant
	// by 242ms vs 240ms, which is a 1% margin and a flake waiting to happen.
	oneAndAHalf := drain * 3 / 2
	almostOne := drain * 9 / 10

	orderlyCost := elapsed(true)
	if orderlyCost >= oneAndAHalf {
		t.Errorf("orderly shutdown with a wedged writer took %v, want < %v (one drain is %v) — the join ran TWICE, doubling a wedged SIGTERM drain", orderlyCost, oneAndAHalf, drain)
	}

	// Control arm: the abrupt path must STILL join, or the flag could have been
	// "fixed" by never joining at all — which would reintroduce the very race the
	// join exists to close. One drain, not zero, and not two.
	abruptCost := elapsed(false)
	if abruptCost < almostOne {
		t.Errorf("abrupt exit with a wedged writer took %v, want >= %v — the deferred join did not run, so db.Close would land under a live writer", abruptCost, almostOne)
	}
	if abruptCost >= oneAndAHalf {
		t.Errorf("abrupt exit took %v, want < %v — it joined twice", abruptCost, oneAndAHalf)
	}
}
