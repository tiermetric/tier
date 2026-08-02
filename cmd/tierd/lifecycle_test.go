package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"path/filepath"
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

// captureErrorLogger returns a logger whose ERROR lines land in the returned
// buffer, for asserting shutdownServer's wedged-watcher warning.
func captureErrorLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	return slog.New(h), &buf
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
		shutdownServer(func() { cancelled = true }, supDone, &http.Server{},
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
		shutdownServer(func() {}, supDone, &http.Server{}, 5*time.Second, time.Second, logger)
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
		shutdownServer(func() {}, supDone, &http.Server{}, drainTimeout, time.Second, logger)
		elapsed := time.Since(start)

		if elapsed < drainTimeout {
			t.Errorf("shutdown returned in %v, before the %v guard timer — it did not wait", elapsed, drainTimeout)
		}
		if !bytes.Contains(logs.Bytes(), []byte("watcher failed to drain")) {
			t.Errorf("expected a wedged-watcher ERROR log, got %q", logs.String())
		}
	})
}
