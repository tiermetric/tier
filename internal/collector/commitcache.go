package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// gitLogFunc matches gitLog's signature so tests can inject a fake commit
// source without shelling out to git.
type gitLogFunc func(ctx context.Context, repoPath string, since time.Time) ([]gitCommit, error)

// commitCacheTTL bounds how stale the live watcher's per-repo commit list may
// be. A commit on a watched branch becomes visible to attribution within this
// window. Kept short relative to how often commits land so a freshly merged
// PR attributes promptly, but long enough that streaming write fan-out
// (30-50 events/sec/file) never triggers more than one git log per repo per
// window.
const commitCacheTTL = 30 * time.Second

// commitLookback is how far back the watcher reads git log. Live sessions are
// recent by construction, and joinSessionsToCommits only matches commits
// within 30 minutes of a session's [start,end]; 30 days is generous headroom
// for a long-lived session while keeping `git log --since` cheap.
const commitLookback = 30 * 24 * time.Hour

// gitLogTimeout bounds a single `git log` exec in the commit cache (#146). A
// hung git — an fs stall, a pathologically large repo, or a misconfigured
// credential helper that blocks on a prompt — must not freeze live ingestion
// for that repo until the process is restarted. 10s is generous versus the
// observed <100 ms for `git log --since` on a large repo, and small versus
// "ingestion frozen forever". On expiry the exec's context is cancelled, which
// kills the git process (gitLog uses exec.CommandContext) and surfaces a
// non-nil error; get's degradation path (stale entry, else nil → branch-name
// attribution) then takes over. A killed git is therefore treated as a failed
// refresh, never mistaken for "no commits" — which would silently zero
// attribution.
const gitLogTimeout = 10 * time.Second

// commitCache gives the live watcher the same commit-time-window attribution
// the on-demand `tierd score` path gets, without running git log on every
// streaming write. Results are cached per repo and refreshed when older than
// ttl.
//
// Closes #99: the watcher used to pass a nil commit slice to
// joinSessionsToCommits, so every session whose branch carries no issue
// number (most importantly `main`) fell through to issueID=="" and was
// silently dropped ($0 capture). With real commits the watcher attributes
// those sessions via the commit decorate/subject exactly as `score` does.
//
// Residual limit (documented, not a bug): a commit can only attribute a
// session if it already exists within the session's time window at process
// time. Spend on `main` that is never committed near the session — or whose
// attributing commit lands after the messages were already tailed past — is
// still live-invisible; `tierd score` backfills it retroactively. That is
// inherent to live-tail commit-window attribution, and matches score's join
// semantics rather than introducing a new sentinel bucket (option 3).
// The entries map is never evicted. Keys are repo roots, and the only caller
// (the watcher) keys exclusively by its finite, configured Repos list — so
// growth is bounded by the number of watched repos, not by arbitrary CWDs. A
// future caller that keyed this by session CWD would need an eviction story.
type commitCache struct {
	ttl      time.Duration
	lookback time.Duration
	timeout  time.Duration
	gitLog   gitLogFunc
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]commitCacheEntry
	// inflight single-flights concurrent refreshes of the same repo. The git
	// log exec runs OUTSIDE mu (#146), so without this every same-repo
	// process() goroutine that raced the TTL expiry would spawn its own git
	// process. A repo has at most one entry here while its refresh is running.
	inflight map[string]*commitFetch
}

type commitCacheEntry struct {
	commits   []gitCommit
	fetchedAt time.Time
}

// commitFetch is one in-flight git log refresh for a single repo. The fetcher
// fills commits/err then closes done; every waiter that joined this fetch reads
// the same immutable result once done is closed, so a repo refreshes at most
// once per TTL window no matter how many process() goroutines race it.
type commitFetch struct {
	done    chan struct{}
	commits []gitCommit
	err     error
}

// newCommitCache builds a cache. A nil fn defaults to the package gitLog; a
// nil now defaults to time.Now; a non-positive timeout defaults to
// gitLogTimeout. All three are injectable so commit-cache behaviour can be
// unit-tested without git, a real clock, or a 10-second wall wait.
func newCommitCache(ttl, lookback, timeout time.Duration, fn gitLogFunc, now func() time.Time) *commitCache {
	if fn == nil {
		fn = gitLog
	}
	if now == nil {
		now = time.Now
	}
	if timeout <= 0 {
		timeout = gitLogTimeout
	}
	return &commitCache{
		ttl:      ttl,
		lookback: lookback,
		timeout:  timeout,
		gitLog:   fn,
		now:      now,
		entries:  make(map[string]commitCacheEntry),
		inflight: make(map[string]*commitFetch),
	}
}

// get returns the cached commits for repoPath, refreshing via git log when
// the entry is missing or older than ttl.
//
// Degradation: a git log error never fails ingestion. If a stale entry exists
// it is returned (better than re-fetching a flaky repo on every event);
// otherwise nil is returned, which makes joinSessionsToCommits fall back to
// branch-name attribution — the pre-#99 behaviour, so a non-git or
// transiently-failing repo is no worse off than before. A git log that exceeds
// gitLogTimeout is cancelled and killed, and surfaces as exactly this error
// path (#146) — a timed-out refresh degrades, it never reports zero commits.
//
// Concurrency (#146): the git log exec runs OUTSIDE the cache mutex, so a slow
// or hung git for one repo cannot pin the lock and stall callers for OTHER
// repos — the failure that the earlier lock-across-exec design allowed. mu is
// held only for the O(1) map reads/writes around the fetch. Same-repo callers
// that race the TTL expiry still single-flight through the inflight map: the
// first becomes the fetcher, the rest wait on its commitFetch and share its
// result, so a repo refreshes at most once per window rather than spawning N
// redundant `git log` processes.
//
// Ownership: the returned slice is the cached backing array, shared across
// every caller until the next refresh — callers (joinSessionsToCommits today)
// MUST treat it as read-only. Mutating it in place would be a concurrent-write
// race two same-repo, different-file process() goroutines could hit. Copy
// first if a future caller needs to sort/filter/dedup.
func (c *commitCache) get(ctx context.Context, repoPath string, logger *slog.Logger) []gitCommit {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.entries[repoPath]; ok && now.Sub(e.fetchedAt) < c.ttl {
		c.mu.Unlock()
		return e.commits
	}
	// Refresh needed. Join an in-flight refresh for this repo if one exists,
	// otherwise become the fetcher.
	if f, ok := c.inflight[repoPath]; ok {
		c.mu.Unlock()
		<-f.done
		return c.resolve(repoPath, f, logger)
	}
	f := &commitFetch{done: make(chan struct{})}
	c.inflight[repoPath] = f
	c.mu.Unlock()

	// Exec git log with NO lock held, bounded by timeout so a hung git is
	// killed rather than freezing this repo's ingestion forever (#146).
	fetchCtx, cancel := context.WithTimeout(ctx, c.timeout)
	commits, err := c.gitLog(fetchCtx, repoPath, now.Add(-c.lookback))
	cancel()

	c.mu.Lock()
	f.commits, f.err = commits, err
	if err == nil {
		c.entries[repoPath] = commitCacheEntry{commits: commits, fetchedAt: now}
	}
	delete(c.inflight, repoPath)
	close(f.done)
	c.mu.Unlock()

	return c.resolve(repoPath, f, logger)
}

// resolve turns a completed fetch into the commit slice to return, applying the
// unchanged degradation policy: a successful fetch returns its commits; a failed
// one serves the current cached entry if any (stale-but-attributing), else nil
// (branch-name fallback). Shared by the fetcher and every waiter so all joiners
// of one fetch degrade identically.
func (c *commitCache) resolve(repoPath string, f *commitFetch, logger *slog.Logger) []gitCommit {
	if f.err == nil {
		return f.commits
	}
	c.mu.Lock()
	e, ok := c.entries[repoPath]
	c.mu.Unlock()
	if ok {
		logger.Warn("watcher: git log refresh failed, using stale commits",
			"repo", repoPath, "err", f.err)
		return e.commits
	}
	logger.Warn("watcher: git log failed, falling back to branch-name attribution",
		"repo", repoPath, "err", f.err)
	return nil
}
