package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for exercising the commit cache TTL
// without sleeping. now() is concurrency-safe so the cache's internal locking
// is the only synchronization under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestCommitCache_CachesWithinTTL: a second get inside the TTL window reuses
// the cached commits and does not re-shell git log — the property that keeps
// streaming write fan-out from spawning a git process per event.
func TestCommitCache_CachesWithinTTL(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var calls int
	want := []gitCommit{{Hash: "abc", IssueID: "issue-1"}}
	c := newCommitCache(30*time.Second, time.Hour, time.Hour, func(_ context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		calls++
		return want, nil
	}, clk.now)

	got1 := c.get(context.Background(), "/repo", discardLogger())
	clk.advance(10 * time.Second) // still within TTL
	got2 := c.get(context.Background(), "/repo", discardLogger())

	if calls != 1 {
		t.Errorf("git log calls = %d, want 1 (second get must hit cache)", calls)
	}
	if len(got1) != 1 || len(got2) != 1 || got1[0].IssueID != "issue-1" || got2[0].IssueID != "issue-1" {
		t.Errorf("unexpected commits: got1=%+v got2=%+v", got1, got2)
	}
}

// TestCommitCache_RefreshesAfterTTL: once the entry ages past the TTL, the
// next get re-fetches so freshly landed commits become visible.
func TestCommitCache_RefreshesAfterTTL(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var calls int
	c := newCommitCache(30*time.Second, time.Hour, time.Hour, func(_ context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		calls++
		return []gitCommit{{Hash: "abc"}}, nil
	}, clk.now)

	c.get(context.Background(), "/repo", discardLogger())
	clk.advance(31 * time.Second) // past TTL
	c.get(context.Background(), "/repo", discardLogger())

	if calls != 2 {
		t.Errorf("git log calls = %d, want 2 (stale entry must refresh)", calls)
	}
}

// TestCommitCache_PerRepoKeying: distinct repos are cached independently and
// each gets its own git log read.
func TestCommitCache_PerRepoKeying(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var calls int
	c := newCommitCache(30*time.Second, time.Hour, time.Hour, func(_ context.Context, repoPath string, _ time.Time) ([]gitCommit, error) {
		calls++
		return []gitCommit{{Hash: repoPath}}, nil
	}, clk.now)

	c.get(context.Background(), "/repo-a", discardLogger())
	c.get(context.Background(), "/repo-b", discardLogger())
	c.get(context.Background(), "/repo-a", discardLogger()) // cached

	if calls != 2 {
		t.Errorf("git log calls = %d, want 2 (one per distinct repo)", calls)
	}
}

// TestCommitCache_ErrorReturnsStale: when a refresh fails but a prior entry
// exists, the cache serves the stale commits rather than dropping attribution
// to nil — a transiently flaky repo keeps attributing on last-known-good.
func TestCommitCache_ErrorReturnsStale(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var calls int
	c := newCommitCache(30*time.Second, time.Hour, time.Hour, func(_ context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		calls++
		if calls == 1 {
			return []gitCommit{{Hash: "good", IssueID: "issue-7"}}, nil
		}
		return nil, errors.New("git boom")
	}, clk.now)

	c.get(context.Background(), "/repo", discardLogger()) // populates cache
	clk.advance(31 * time.Second)                         // force refresh
	got := c.get(context.Background(), "/repo", discardLogger())

	if calls != 2 {
		t.Fatalf("git log calls = %d, want 2", calls)
	}
	if len(got) != 1 || got[0].IssueID != "issue-7" {
		t.Errorf("got %+v, want stale issue-7 commit on refresh error", got)
	}
}

// TestCommitCache_ErrorNoCacheReturnsNil: a first-ever fetch that errors
// returns nil, which makes joinSessionsToCommits fall back to branch-name
// attribution (the pre-#99 behaviour) — never a panic, never a dropped event
// for a feature branch.
func TestCommitCache_ErrorNoCacheReturnsNil(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCommitCache(30*time.Second, time.Hour, time.Hour, func(_ context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		return nil, errors.New("git boom")
	}, clk.now)

	got := c.get(context.Background(), "/repo", discardLogger())
	if got != nil {
		t.Errorf("got %+v, want nil on first-fetch error", got)
	}
}

// TestCommitCache_LookbackPassedToGitLog: the cache asks git log for commits
// since now-lookback, so a long-lived session's window is covered.
func TestCommitCache_LookbackPassedToGitLog(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var gotSince time.Time
	c := newCommitCache(30*time.Second, 48*time.Hour, time.Hour, func(_ context.Context, _ string, since time.Time) ([]gitCommit, error) {
		gotSince = since
		return nil, nil
	}, clk.now)

	c.get(context.Background(), "/repo", discardLogger())

	wantSince := clk.now().Add(-48 * time.Hour)
	if !gotSince.Equal(wantSince) {
		t.Errorf("since = %v, want %v (now - lookback)", gotSince, wantSince)
	}
}

// TestNewCommitCache_Defaults: nil fn / nil now / zero timeout must not panic —
// they default to the package gitLog, time.Now, and gitLogTimeout so production
// callers can pass them through unset.
func TestNewCommitCache_Defaults(t *testing.T) {
	c := newCommitCache(time.Second, time.Hour, 0, nil, nil)
	if c.gitLog == nil {
		t.Error("gitLog default not applied")
	}
	if c.now == nil {
		t.Error("now default not applied")
	}
	if c.timeout != gitLogTimeout {
		t.Errorf("timeout default = %v, want %v", c.timeout, gitLogTimeout)
	}
}

// TestCommitCache_ConcurrentGetsSingleFlight pins the property the #146
// exec-outside-the-mutex restructure had to preserve: N concurrent get() calls
// for the SAME repo that race the TTL expiry collapse into exactly ONE git log
// exec (via the inflight join), and every caller receives that one fetch's
// commits. Without single-flight, moving the exec out of the lock would let each
// racing goroutine spawn its own git process — the regression this guards.
func TestCommitCache_ConcurrentGetsSingleFlight(t *testing.T) {
	const n = 8
	var calls int32
	var enterOnce sync.Once
	entered := make(chan struct{}) // closed when the (single) fetcher is in flight
	release := make(chan struct{}) // holds the fetcher in flight until all callers arrive
	want := []gitCommit{{Hash: "abc", IssueID: "issue-1"}}

	fn := func(_ context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		atomic.AddInt32(&calls, 1)
		enterOnce.Do(func() { close(entered) })
		<-release
		return want, nil
	}
	// A generous timeout so the deliberately-blocked fetch is never killed.
	c := newCommitCache(30*time.Second, time.Hour, time.Hour, fn, nil)

	var wg sync.WaitGroup
	started := make(chan struct{}, n)
	results := make([][]gitCommit, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			results[i] = c.get(context.Background(), "/repo", discardLogger())
		}(i)
	}
	for i := 0; i < n; i++ {
		<-started // all n goroutines have entered get
	}
	<-entered      // the sole fetcher is blocked mid-exec; the rest join its inflight
	close(release) // let the fetch complete; every caller shares its result
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("git log calls = %d, want 1 (concurrent same-repo gets must single-flight)", got)
	}
	for i, r := range results {
		if len(r) != 1 || r[0].IssueID != "issue-1" {
			t.Errorf("goroutine %d got %+v, want the single shared issue-1 commit", i, r)
		}
	}
}

// TestCommitCache_GitLogTimeoutDegrades pins the #146 fix: a hung git log is
// bounded by the cache's timeout, its context is cancelled (killing the real
// process), and the resulting error flows through the SAME degradation path as
// any other git failure — a timeout is never mistaken for "no commits". The
// third subtest is the regression teeth: because the exec runs OUTSIDE the
// cache mutex, a hung fetch for one repo must not block callers for other repos
// beyond the timeout.
func TestCommitCache_GitLogTimeoutDegrades(t *testing.T) {
	// blockUntilCancel simulates a hung git: it returns only once its context
	// is cancelled (by the cache's per-fetch timeout), reporting the ctx error
	// exactly as gitLog does when exec.CommandContext kills the process.
	blockUntilCancel := func(ctx context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	t.Run("first-fetch timeout returns nil (branch-name fallback)", func(t *testing.T) {
		c := newCommitCache(30*time.Second, time.Hour, 50*time.Millisecond, blockUntilCancel, nil)
		start := time.Now()
		got := c.get(context.Background(), "/repo", discardLogger())
		elapsed := time.Since(start)
		if got != nil {
			t.Errorf("got %+v, want nil on first-fetch timeout", got)
		}
		if elapsed > time.Second {
			t.Errorf("get blocked %v, want it bounded near the 50ms timeout (git log not timed out?)", elapsed)
		}
	})

	t.Run("timeout with a stale entry serves the stale commits", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		var calls int32
		fn := func(ctx context.Context, _ string, _ time.Time) ([]gitCommit, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return []gitCommit{{Hash: "good", IssueID: "issue-7"}}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		c := newCommitCache(30*time.Second, time.Hour, 50*time.Millisecond, fn, clk.now)

		c.get(context.Background(), "/repo", discardLogger()) // populate
		clk.advance(31 * time.Second)                         // force a refresh
		got := c.get(context.Background(), "/repo", discardLogger())
		if len(got) != 1 || got[0].IssueID != "issue-7" {
			t.Errorf("got %+v, want the stale issue-7 commit on refresh timeout", got)
		}
	})

	t.Run("hung fetch does not block a different repo past the timeout", func(t *testing.T) {
		entered := make(chan struct{})
		fn := func(ctx context.Context, repo string, _ time.Time) ([]gitCommit, error) {
			if repo == "/slow" {
				close(entered) // signal we are inside the exec, mu already released
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []gitCommit{{Hash: repo}}, nil // /fast returns immediately
		}
		// 2s timeout for the slow repo: long enough that a lock-across-exec
		// regression would clearly stall /fast, short enough to bound the test.
		c := newCommitCache(30*time.Second, time.Hour, 2*time.Second, fn, nil)

		slowDone := make(chan struct{})
		go func() {
			defer close(slowDone)
			c.get(context.Background(), "/slow", discardLogger())
		}()

		<-entered // /slow is now inside git log with mu released
		start := time.Now()
		got := c.get(context.Background(), "/fast", discardLogger())
		elapsed := time.Since(start)
		if len(got) != 1 || got[0].Hash != "/fast" {
			t.Errorf("got %+v, want /fast commit", got)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("/fast blocked %v behind /slow's exec — git log is not running outside the cache mutex", elapsed)
		}
		<-slowDone // let the hung fetch drain (its 2s timeout fires) before returning
	})
}
