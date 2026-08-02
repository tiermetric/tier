package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// CheckpointStore is the subset of store.DB the watcher needs for its per-file
// tail-state persistence (#71). Defined here as an interface so tests can supply
// an in-memory recorder without spinning up SQLite.
//
// The checkpoints persist per-file tail state so a restart resumes from the last
// parsed offset instead of byte 0. They are a derived cache: a load/save failure
// is non-fatal (logged), because dedup on token_events backstops any re-parse the
// missing checkpoint would cause.
//
// Event insertion is NOT here: as of #46 the watcher forwards token events
// through the shared collector.Ingester seam (the same sink JSONL and the proxy
// use), so the store-write path is uniform across every collector. Checkpoints
// stay on their own interface because they are a watcher-specific resume cache,
// not a token-event sink.
type CheckpointStore interface {
	LoadWatcherCheckpoints(ctx context.Context) ([]store.WatcherCheckpoint, error)
	SaveWatcherCheckpoint(ctx context.Context, cp store.WatcherCheckpoint) error
	DeleteWatcherCheckpoint(ctx context.Context, path string) error
}

// Watcher tails Claude Code JSONL session files via fsnotify and inserts new
// token events as sessions evolve. It is the live counterpart of
// JSONLCollector (which scans on demand from `tierd score`).
//
// Run lifecycle:
//   - Run(ctx) blocks; cancel ctx to shut down. Any pending debounced work is
//     dropped, not flushed — re-parsing on next startup recovers the data.
//   - Out of scope (per issue #18): backfilling historical files. Sessions
//     that never receive a write event after Run starts are not ingested.
//     Use `tierd score` for that.
type Watcher struct {
	// ClaudeDir defaults to ~/.claude when empty. The watcher attaches to
	// ClaudeDir/projects/ recursively.
	ClaudeDir string

	// Repos lists absolute paths of git repositories whose sessions should be
	// ingested. A Claude Code session is in-scope when its recorded CWD is at
	// or inside one of these paths (see filterSessionsByRepo's matching).
	// Empty Repos disables ingestion entirely — Run still drains events but
	// inserts nothing, which is useful for "watcher is healthy" checks.
	Repos []string

	// RepoSlugs optionally overrides a watched repository's canonical
	// "owner/repo" identity, keyed by the same path string used in Repos (#231).
	// Absent entries fall back to remote.origin.url, then to repoid.Unqualified.
	//
	// The override exists for FORKS: a contributor whose origin is "alice/tier"
	// emits cost rows that would never join outcomes from the upstream
	// "tiermetric/tier" webhook. Nothing but an explicit override can fix that.
	RepoSlugs map[string]string

	// repoSlugCache memoizes the resolved slug per absolute target path so the
	// unresolvable-repo warning fires once per target, not once per session file.
	repoSlugCache sync.Map // string -> string

	// Ingester is the shared sink every token event is forwarded through
	// (#46) — the same collector.Ingester the JSONL and proxy collectors use,
	// so validation, idempotency, and the Source stamp are applied uniformly.
	// Required.
	Ingester Ingester

	// Checkpoints persists per-file tail state (#71) so a restart resumes from
	// the last parsed offset. Required. Kept separate from Ingester because a
	// resume cache is not a token-event sink.
	Checkpoints CheckpointStore

	// DeveloperID labels every emitted event. Defaults to the OS username.
	DeveloperID string

	// Logger receives diagnostic output. Defaults to slog.Default().
	Logger *slog.Logger

	// DebounceDelay coalesces rapid writes to the same JSONL file. A Claude
	// Code streaming response writes 30-50 times per second; we want one
	// re-parse after the stream settles, not 50. Default 1 second.
	DebounceDelay time.Duration

	// gitLogFn is the commit source for the per-repo commit cache that powers
	// main-branch (non-issue) attribution (#99). Nil defaults to the package
	// gitLog; tests inject a fake so the watcher suite doesn't shell out to
	// git or require committed fixtures.
	gitLogFn gitLogFunc

	// OnWatchAddFailure, when non-nil, is invoked for every fsnotify Add that
	// fails while (re)watching a directory tree — at startup and for every
	// new-dir CREATE event (#142). It exists so the process can surface silent
	// capture degradation (on macOS, kqueue burns one fd per watched dir, so an
	// aging ~/.claude/projects tree eventually hits EMFILE and the watcher goes
	// blind directory-by-directory with nothing in /healthz or /metrics) WITHOUT
	// the collector importing internal/health — the dependency stays one-way
	// (health knows nothing of collector; collector stays health-free). The
	// callback runs inline in the walk, so keep it cheap and non-blocking.
	OnWatchAddFailure func(path string, err error)
}

// Run starts the watcher and blocks until ctx is cancelled or a fatal error
// occurs. Recoverable fsnotify errors (transient kernel notifications,
// permission denied on individual subdirs) are logged and skipped.
//
// Shutdown semantics: ctx cancellation stops accepting new events and waits
// for any in-flight debounced callback to finish writing to the store before
// returning. This prevents a write-after-close race when the caller calls
// db.Close() immediately after Run() returns.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Ingester == nil {
		return errors.New("watcher: Ingester is required")
	}
	if w.Checkpoints == nil {
		return errors.New("watcher: Checkpoints is required")
	}
	logger := w.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if w.DebounceDelay <= 0 {
		w.DebounceDelay = time.Second
	}
	developer := w.DeveloperID
	if developer == "" {
		developer = osUsername()
	}

	claudeDir := w.ClaudeDir
	if claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("watcher: home dir: %w", err)
		}
		claudeDir = filepath.Join(home, ".claude")
	}
	projectsDir := filepath.Join(claudeDir, "projects")

	// Pre-resolve target repo paths for matching. We resolve symlinks once so
	// the per-event hot path is a pure string compare.
	targets := resolveTargets(w.Repos)

	// Per-repo commit cache (#99): lets the live join attribute main-branch
	// (non-issue) sessions via commit decorate/subject the same way `score`
	// does, refreshing git log at most once per repo per commitCacheTTL.
	commits := newCommitCache(commitCacheTTL, commitLookback, gitLogTimeout, w.gitLogFn, time.Now)

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher: fsnotify.NewWatcher: %w", err)
	}
	defer func() { _ = fw.Close() }()

	// Ensure projectsDir exists before subscribing — fsnotify.Add returns
	// "no such file or directory" otherwise, and we want a clear error.
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		return fmt.Errorf("watcher: mkdir %s: %w", projectsDir, err)
	}
	if err := w.addRecursive(fw, projectsDir, projectsDir, targets, logger); err != nil {
		return fmt.Errorf("watcher: add recursive: %w", err)
	}
	// Startup breadcrumb (#142): flag any configured repo with no project dir
	// yet so a silent-capture investigation has an immediate first lead.
	logReposWithoutProjectDir(projectsDir, targets, logger)
	// Log the resolved target paths, not the raw input, so a wrong-path
	// symptom is greppable from the start line ("repos" shows what the
	// matcher will actually compare against).
	resolvedRepoPaths := make([]string, 0, len(targets))
	for _, t := range targets {
		resolvedRepoPaths = append(resolvedRepoPaths, t.resolved)
	}
	logger.Info("watcher started",
		"dir", projectsDir,
		"repos_in", w.Repos,
		"repos_resolved", resolvedRepoPaths,
		"debounce_ms", w.DebounceDelay.Milliseconds(),
	)
	// A repo_slugs key that matches no watched target is a silent no-op that produces
	// exactly the bug the override exists to fix (#231): on a fork we would fall back
	// to remote.origin.url, resolve the FORK's slug, and — because that is a real slug
	// rather than the sentinel — never warn. Surface the typo instead.
	for k := range w.RepoSlugs {
		ck := filepath.Clean(k)
		matched := false
		for _, t := range targets {
			if ck == t.abs || ck == t.resolved {
				matched = true
				break
			}
		}
		if !matched {
			logger.Warn("watch.repo_slugs entry matches no watched repo; the override will not apply",
				"key", k,
				"cleaned", ck,
				"watched", resolvedRepoPaths,
				"hint", "use the same absolute path as watch.repos (no ~ expansion is performed)")
		}
	}

	// Per-file debounce timers. Each timer reschedules on every new event for
	// the same path. Guarded by mu — timer callbacks fire on a fresh goroutine
	// so we can't rely on the event-loop goroutine for serialization.
	//
	// Incremental tailing state (#30): for each JSONL we remember the byte
	// offset we last successfully parsed plus the inode, so the next
	// debounce reads only the appended bytes. Cached session metadata
	// lets the incremental parser build a sessionSummary without
	// re-reading the file head. Inode mismatch (rotation, recreate) or
	// size shrink (truncation) triggers a full re-parse, throwing away
	// the stale state.
	var mu sync.Mutex
	timers := map[string]*time.Timer{}
	offsets := map[string]parseState{}
	// Seed the tail-state map from persisted checkpoints (#71) so the first
	// post-restart write to a file resumes from its last parsed offset instead
	// of re-parsing from byte 0. Load failures and individual malformed rows are
	// non-fatal: we degrade to the pre-#71 behaviour (full re-parse, absorbed by
	// dedup). No lock needed — the event loop hasn't started yet.
	if cps, err := w.Checkpoints.LoadWatcherCheckpoints(ctx); err != nil {
		logger.Warn("watcher: load checkpoints failed; starting from byte 0", "err", err)
	} else {
		for _, cp := range cps {
			st, err := stateFromCheckpoint(cp)
			if err != nil {
				// Unusable row — prune it so it isn't re-logged every restart.
				logger.Warn("watcher: pruning malformed checkpoint", "path", cp.Path, "err", err)
				w.deleteCheckpoint(ctx, cp.Path, logger)
				continue
			}
			// Prune checkpoints whose file no longer exists — removed while the
			// watcher was DOWN, so the removal-time delete never fired. Without
			// this the table grows unbounded over time, quietly reintroducing
			// the cost #71 set out to bound (just on a different table). One
			// stat per row at startup keeps it bounded to live files; a file
			// that later reappears with a new inode is handled by the staleness
			// guard regardless. A non-ErrNotExist stat error (e.g. transient
			// permission) is NOT pruned — keep the row and let the guard decide.
			if _, statErr := os.Stat(cp.Path); errors.Is(statErr, fs.ErrNotExist) {
				w.deleteCheckpoint(ctx, cp.Path, logger)
				continue
			}
			offsets[cp.Path] = st
		}
		if len(offsets) > 0 {
			logger.Info("watcher: resumed from checkpoints", "files", len(offsets))
		}
	}
	// stopping flips once on shutdown (under mu) and gates both schedule()
	// and the timer callbacks. Without it, two paths could write to the
	// store after Run returned and the caller closed the DB:
	//   - a callback that fired before stopTimers but reached its body
	//     after inflight.Wait() (the WaitGroup Add used to happen outside
	//     the mutex, so Wait couldn't see it);
	//   - the per-path re-arm for in-flight coalescing, which used to be an
	//     anonymous AfterFunc invisible to stopTimers.
	stopping := false
	// inflightPath tracks which paths currently have a process() goroutine
	// running. Without this, two debounce callbacks for the same path can
	// race: each reads the same `prior` snapshot from offsets, both call
	// process concurrently, then both write back — the later writer
	// silently overwrites the earlier with a possibly-smaller offset,
	// causing the next debounce to re-read bytes (and ID-less keys to
	// collide because parseSeq advancement isn't visible across the
	// racers). Per-path serialization keeps the cached state monotonic.
	inflightPath := map[string]bool{}

	// inflight counts callbacks that have started executing but not finished.
	// On shutdown we Wait on it so an in-flight Ingest can complete before Run
	// returns (preventing a write-after-db.Close race in the caller).
	var inflight sync.WaitGroup

	// stopTimers cancels any pending debounced work and flips `stopping` —
	// called on shutdown so we don't kick off new writes after the caller
	// wanted us to stop. Any callback that already fired but hasn't entered
	// its critical section yet will observe `stopping` and bail; any that
	// got past the check has already registered with `inflight` (under this
	// same mutex), so the Wait below covers it. Returns the count of
	// cancelled (not-yet-fired) work units for the shutdown log line.
	stopTimers := func() int {
		mu.Lock()
		defer mu.Unlock()
		stopping = true
		var dropped int
		for path, t := range timers {
			if t.Stop() {
				dropped++
			}
			delete(timers, path)
		}
		return dropped
	}

	// schedule sets up (or refreshes) the debounce timer for path. The
	// callback closes over `thisTimer` so it can verify it's still the
	// active timer for path before deleting from the map — without this,
	// a fast-fire race could delete a later-scheduled timer.
	var schedule func(path string)
	schedule = func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if stopping {
			return
		}
		if existing, ok := timers[path]; ok {
			// Reset is the documented way to refresh an existing timer; it
			// avoids the per-event allocation a fresh AfterFunc would cause
			// under streaming-write fan-out (30-50 events/sec/file).
			existing.Reset(w.DebounceDelay)
			return
		}
		// First time we've seen this path — create the timer. The closure
		// holds a reference to the timer we're storing so the callback can
		// detect "I was replaced" later.
		var thisTimer *time.Timer
		thisTimer = time.AfterFunc(w.DebounceDelay, func() {
			mu.Lock()
			// Shutdown won the race: stopTimers flipped `stopping` before we
			// got the lock. Do nothing — especially don't touch the store.
			if stopping {
				mu.Unlock()
				return
			}
			// Race window: between Stop()'s return and now, the timer may
			// have been replaced in the map by a sibling event. Only delete
			// the entry if it's still ours.
			if timers[path] == thisTimer {
				delete(timers, path)
			}
			// Serialize per-path. If another goroutine is already in
			// process() for this path, defer to it: re-arm a fresh debounce
			// so the new content is picked up after the in-flight parse
			// completes. schedule() re-acquires the lock itself, registers
			// the timer in the map (so stopTimers can cancel it — an
			// anonymous AfterFunc here would outlive shutdown), and no-ops
			// once stopping is set.
			if inflightPath[path] {
				mu.Unlock()
				schedule(path)
				return
			}
			inflightPath[path] = true
			// Read the prior tail state under the mutex; the new state
			// is computed without the lock (process opens the file and
			// parses, which can take milliseconds — we don't want to
			// block other timers' map ops on that I/O).
			prior := offsets[path]
			// Register with the shutdown WaitGroup BEFORE releasing the
			// lock: stopTimers flips `stopping` under this same mutex, so
			// either we registered first (and Wait blocks on us) or
			// stopping was already set (and we returned above). Adding
			// after the unlock reopens the fired-but-not-yet-Added window
			// that lets Wait return while a store write is still coming.
			inflight.Add(1)
			mu.Unlock()

			defer inflight.Done()
			defer func() {
				mu.Lock()
				delete(inflightPath, path)
				mu.Unlock()
			}()

			newState, ok := w.process(ctx, path, prior, targets, developer, commits, logger)
			if !ok {
				// File gone or shutdown — drop cached state so a future
				// CREATE+WRITE pair re-establishes from byte 0. Mirror the
				// drop in the persisted checkpoint (#71) so a stale row can't
				// resume against a future file at this path.
				mu.Lock()
				delete(offsets, path)
				mu.Unlock()
				w.deleteCheckpoint(ctx, path, logger)
				return
			}
			mu.Lock()
			offsets[path] = newState
			mu.Unlock()
			// Persist the advanced tail state (#71) outside the mutex — a store
			// write can take milliseconds and per-path serialization (inflightPath)
			// already prevents a concurrent advance for this path.
			w.saveCheckpoint(ctx, path, newState, logger)
		})
		timers[path] = thisTimer
	}

	// cancelTimer drops any pending work for path — used when a JSONL file
	// is removed or renamed away, so we don't waste a debounce cycle parsing
	// a file that no longer exists. Also clears the incremental-tailing
	// offset cache so a same-named replacement file is full-re-parsed.
	cancelTimer := func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := timers[path]; ok {
			t.Stop()
			delete(timers, path)
		}
		delete(offsets, path)
	}

	for {
		select {
		case <-ctx.Done():
			dropped := stopTimers()
			// Wait for any callback that already started executing — its
			// Ingest call must finish before we let the caller close the DB.
			inflight.Wait()
			logger.Info("watcher stopped", "dropped_pending", dropped)
			return nil

		case ev, ok := <-fw.Events:
			if !ok {
				// Watcher channel closed unexpectedly. Treat as fatal — the
				// caller (tierd serve) will log and shut down.
				return errors.New("watcher: fsnotify Events channel closed")
			}
			// New directory anywhere under projects/ — Claude Code creates
			// one per repo on first use. Attach recursively so we cover the
			// case of a tree being moved/copied in (mv, mkdir -p), not just
			// a freshly-created leaf dir.
			if ev.Has(fsnotify.Create) {
				if info, statErr := os.Stat(ev.Name); statErr == nil && info.IsDir() {
					// Same prune applies here: a newly-created top-level project
					// dir for a non-target repo is skipped so it never consumes a
					// descriptor (#142).
					if err := w.addRecursive(fw, ev.Name, projectsDir, targets, logger); err != nil {
						logger.Warn("watcher: add new subdir failed", "path", ev.Name, "err", err)
					}
					continue
				}
			}
			// JSONL removed or renamed away — drop any pending parse so we
			// don't leak map entries for paths that will never re-fire and
			// don't waste a debounce on a file that's gone.
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				if filepath.Ext(ev.Name) == ".jsonl" {
					cancelTimer(ev.Name)
					// Drop the persisted checkpoint too (#71) — cancelTimer only
					// clears the in-memory offset. Outside cancelTimer's mutex so
					// the store write doesn't hold the event-loop lock.
					w.deleteCheckpoint(ctx, ev.Name, logger)
				}
				continue
			}
			if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
				continue
			}
			if filepath.Ext(ev.Name) != ".jsonl" {
				continue
			}
			schedule(ev.Name)

		case err, ok := <-fw.Errors:
			if !ok {
				return errors.New("watcher: fsnotify Errors channel closed")
			}
			// fsnotify errors are not necessarily fatal (e.g. kernel buffer
			// overflow on Linux). Log and continue.
			logger.Warn("watcher: fsnotify error", "err", err)
		}
	}
}

// process parses one JSONL file, matches its CWD against the configured
// targets, and inserts a TokenEvent through the store for each message in
// the session.
//
// Incremental tailing (#30): if prior.inode matches the file's current
// inode AND the file hasn't shrunk below prior.offset, only bytes after
// prior.offset are read — cached session metadata avoids the file-head
// re-read. Inode mismatch or shrinkage triggers a full re-parse from byte
// 0. Returns the new state (caller persists it under the offsets-map
// mutex) and an ok flag; ok=false means the file is gone or unreadable
// and the caller should drop any cached state for path.
//
// Cross-debounce dedup is handled by the store layer: per-message
// IdempotencyKey from #19 + MAX-on-conflict UPSERT from #18 mean any
// message re-emitted across debounce cycles (e.g. the streaming
// placeholder + final-entry pattern crossing a debounce boundary) lands
// as one row with the larger values.
func (w *Watcher) process(ctx context.Context, path string, prior parseState, targets []resolvedPath, developer string, cache *commitCache, logger *slog.Logger) (parseState, bool) {
	info, err := os.Stat(path)
	if err != nil {
		// File gone or permission denied — drop any cached state and let
		// the next CREATE event re-establish.
		logger.Warn("watcher: stat failed", "path", path, "err", err)
		return parseState{}, false
	}

	currentInode := inodeOf(info)
	// Verify the file head still matches the cached fingerprint BEFORE
	// deciding incremental vs full reparse: a truncate-and-rewrite-to-
	// similar-or-larger size can leave inode and size both passing the
	// resume check, yet the bytes underneath prior.offset are different
	// content. Without this guard, the rewritten file's new content
	// would parse under the OLD session's cached metadata — silent
	// cross-session mis-attribution.
	//
	// We hash exactly prior.headLen bytes (not min(file_size, 4 KB)) so
	// the fingerprint over the file's INITIAL bytes is stable as the
	// file grows by appending. A short-read (file is now smaller than
	// prior.headLen) trips the size-shrink path naturally and fails the
	// comparison via headLen mismatch.
	startOffset := int64(0)
	knownMeta := sessionMetadata{}
	if prior.inode != 0 &&
		prior.inode == currentInode &&
		info.Size() >= prior.offset &&
		prior.headLen > 0 {
		currentCRC, currentHeadLen, fpErr := fingerprintHead(path, prior.headLen)
		if fpErr != nil && !errors.Is(fpErr, io.EOF) {
			logger.Warn("watcher: fingerprint failed", "path", path, "err", fpErr)
			return parseState{}, false
		}
		if currentHeadLen == prior.headLen && currentCRC == prior.headCRC {
			// Same file, head unchanged — safe to resume.
			startOffset = prior.offset
			knownMeta = prior.metadata
		}
	}
	// Compute the fingerprint we'll cache for the NEXT debounce. Fixed
	// length headFingerprintBytes (or smaller for tiny files) so future
	// re-checks hash the same byte range.
	wantHeadLen := headFingerprintBytes
	if info.Size() < int64(wantHeadLen) {
		wantHeadLen = int(info.Size())
	}
	nextCRC, nextHeadLen, fpErr := fingerprintHead(path, wantHeadLen)
	if fpErr != nil && !errors.Is(fpErr, io.EOF) {
		logger.Warn("watcher: fingerprint failed", "path", path, "err", fpErr)
		return parseState{}, false
	}

	// tailMode=true: this is a live JSONL the agent is still writing to.
	// Partial trailing lines must not be consumed; we re-evaluate them on
	// the next debounce once the writer has flushed the \n.
	s, newOffset, err := parseSessionFileFromOffset(path, startOffset, knownMeta, true)
	if err != nil {
		logger.Warn("watcher: parse failed", "path", path, "err", err)
		return parseState{}, false
	}
	if s == nil {
		// No usable session yet (just-created file, only user messages
		// so far, or no new assistant entries in this chunk). Advance the
		// offset anyway so we don't re-read these same bytes on the next
		// debounce — but only if we'd already established session
		// metadata. Without metadata, retaining offset would be a bug
		// (incremental parses without metadata can't build a summary).
		if knownMeta.SessionID != "" {
			return parseState{
				offset:   newOffset,
				inode:    currentInode,
				headCRC:  nextCRC,
				headLen:  nextHeadLen,
				metadata: knownMeta,
			}, true
		}
		// Drop state — we still don't have enough to act on this file.
		return parseState{}, true
	}
	target, matched := matchTarget(s.CWD, targets)
	if !matched {
		// Foreign repo — but cache the session metadata + offset anyway
		// so the next debounce can skip cheaply without re-reading.
		return parseState{
			offset:  newOffset,
			inode:   currentInode,
			headCRC: nextCRC,
			headLen: nextHeadLen,
			metadata: sessionMetadata{
				SessionID:    s.SessionID,
				GitBranch:    s.GitBranch,
				CWD:          s.CWD,
				StartTime:    s.StartTime,
				Model:        s.Model,
				NextParseSeq: s.NextParseSeq,
			},
		}, true
	}

	// Reuse the existing join pipeline with the target repo's recent commits
	// (cached, TTL-refreshed) so a main-branch / non-issue session is
	// attributed via commit decorate/subject exactly as `score` does, instead
	// of falling through to issueID=="" and being dropped at $0 (#99). On a
	// git log failure the cache returns nil and the join falls back to
	// branch-name attribution — the pre-#99 behaviour.
	events := joinSessionsToCommits([]sessionSummary{*s}, cache.get(ctx, target.abs, logger), developer, w.repoSlug(target, logger))
	var insertFailed bool
	for _, ev := range events {
		// Forward through the shared collector.Ingester (#46) instead of building
		// a store.TokenEvent here. The old hand-rolled literal silently omitted
		// SessionID — a field #281 threaded through the ingester adapter for the
		// JSONL score path but never onto this live-watcher bypass — so the
		// primary capture path was dropping session_id it had already parsed. The
		// unified sink copies every field the collector produced, closing that
		// exact asymmetry (#27/#46).
		if err := w.Ingester.Ingest(ctx, ev); err != nil {
			// context.Canceled is the expected shutdown signal — not a real
			// error. Logging it at WARN would scare an operator who saw
			// "watcher: insert failed" on every tierd serve stop.
			if errors.Is(err, context.Canceled) {
				logger.Debug("watcher: insert cancelled on shutdown", "path", path)
				// Shutdown cancelled the insert before it landed. Returning
				// false routes to the !ok path, which drops this path from
				// offsets AND deletes its persisted checkpoint (#71): the
				// cancelled chunk never persisted, so the next start must
				// re-read this file from byte 0 to re-attempt it. Dedup
				// (#18/#19) absorbs the already-written events from that
				// re-parse, so re-reading from byte 0 is safe, just not free.
				return parseState{}, false
			}
			// Any other insert error: the event we just tried to write is
			// lost. Pre-#30 the next debounce did a full re-parse so dedup
			// absorbed the retry; post-#30 the offset would advance past
			// the failed event and we'd never see it again. Preserve the
			// prior offset so the next debounce retries this chunk. The
			// store layer's MAX-on-conflict UPSERT (closes #18) absorbs the
			// successful-then-retried events idempotently.
			logger.Warn("watcher: insert failed", "path", path, "err", err)
			insertFailed = true
		}
	}
	if insertFailed {
		// Roll back offset to prior so the next debounce retries this
		// chunk. Inode and metadata can advance to the current values
		// since they're independent of the offset cursor. Carry the
		// FRESH fingerprint (not prior's) so the retry doesn't reject
		// itself for a head-mismatch we just observed.
		return parseState{
			offset:   prior.offset,
			inode:    currentInode,
			headCRC:  nextCRC,
			headLen:  nextHeadLen,
			metadata: prior.metadata,
		}, true
	}

	// All events forwarded. Cache the metadata + new offset so the next
	// debounce can resume incrementally.
	return parseState{
		offset:  newOffset,
		inode:   currentInode,
		headCRC: nextCRC,
		headLen: nextHeadLen,
		metadata: sessionMetadata{
			SessionID:    s.SessionID,
			GitBranch:    s.GitBranch,
			CWD:          s.CWD,
			StartTime:    s.StartTime,
			Model:        s.Model,
			NextParseSeq: s.NextParseSeq,
		},
	}, true
}

// fingerprintHead reads exactly want bytes from the start of path and
// returns the IEEE CRC-32. want is bounded at headFingerprintBytes (so a
// caller asking for the initial fingerprint passes that constant). The
// length is fixed by the caller, not by the file size, so a small file
// that grows by appending has a stable fingerprint over its initial
// bytes (the bytes that were there when we last cached).
//
// Returns (0, 0, nil) for a zero-length file. Returns (0, 0, io.EOF) if
// the file is shorter than want — the caller treats that as "file
// shrank or never had that many bytes, force full reparse."
func fingerprintHead(path string, want int) (uint32, int, error) {
	if want <= 0 {
		return 0, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, want)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, 0, err
	}
	if n < want {
		// File shorter than want — short read. Return what we have but
		// surface EOF so the caller knows the fingerprint isn't the
		// full requested length.
		return crc32.ChecksumIEEE(buf[:n]), n, io.EOF
	}
	return crc32.ChecksumIEEE(buf[:n]), n, nil
}

// resolvedPath holds the absolute and EvalSymlinks-resolved form of one
// configured target repo, plus the RepoScope that decides whether a CWD belongs
// to it. abs/resolved are still carried directly because the fd-optimising
// prune (targetPrefixes) and the commit cache read them; the scope owns the
// scope DECISION so this path shares one implementation with the batch and Codex
// paths (#464 R4).
type resolvedPath struct {
	abs      string
	resolved string
	// scope is a LIVE (non-memoizing) scope: it outlives every scan and is used
	// from concurrent debounce goroutines. See matchTarget for why not memoized.
	scope *RepoScope
}

// repoSlug returns the canonical "owner/repo" for a watched target (#231),
// resolving at most once per target: operator override (RepoSlugs), else
// remote.origin.url, else the repoid.Unqualified sentinel.
//
// Degrading to the sentinel never fails the ingest. A repo we cannot name still
// yields true per-developer cost — the primary TIER denominator groups by developer
// alone. Only cross-repo issue disambiguation degrades, so refusing to ingest would
// trade a real number for no number. It must be observable though, so the fallback
// warns EXACTLY once per target: LoadOrStore decides the winner, and only the
// goroutine that actually stored the value emits the log.
func (w *Watcher) repoSlug(t resolvedPath, logger *slog.Logger) string {
	if v, ok := w.repoSlugCache.Load(t.abs); ok {
		return v.(string)
	}
	slug, unresolved := w.resolveRepoSlug(t)
	actual, loaded := w.repoSlugCache.LoadOrStore(t.abs, slug)
	if !loaded && unresolved {
		logger.Warn("cannot determine repository slug; cost rows will be repo-unqualified and multi-repo issues sharing a number will fuse",
			"repo", t.abs,
			"hint", "set watch.repo_slugs[<path>] (required for forks), or add a remote.origin.url")
	}
	return actual.(string)
}

// resolveRepoSlug applies the override-then-remote-then-sentinel precedence.
// unresolved reports whether it fell all the way through to the sentinel.
func (w *Watcher) resolveRepoSlug(t resolvedPath) (slug string, unresolved bool) {
	// Override keys are operator-typed paths; normalize both sides before comparing
	// so "~/src/tier" (already expanded by config) and a trailing slash still match.
	for k, v := range w.RepoSlugs {
		ck := filepath.Clean(k)
		if ck != t.abs && ck != t.resolved {
			continue
		}
		if s, ok := repoid.Canonical(v); ok {
			return s, false
		}
		// A configured-but-invalid slug is an operator error worth failing loudly on
		// rather than silently ignoring — but not worth killing ingest for. Fall
		// through to remote detection and let the sentinel warning carry it.
		break
	}
	if s := RepoSlugFromGitConfig(t.abs); s != "" {
		return s, false
	}
	return repoid.Unqualified, true
}

// parseState is the per-watched-JSONL state that powers incremental tailing
// (#30). The watcher caches one of these per path under the same mutex as
// the debounce timers. Cleared on inode change, size shrink, or head-byte
// fingerprint mismatch — each of those signals that the file we cached
// state for is no longer the file on disk.
//
// headCRC fingerprints the first headFingerprintBytes (or fewer if the
// file is smaller) so that truncate-and-rewrite-to-similar-or-larger size
// is detectable: inode and size pass the prior checks but a different
// file head proves the bytes underneath prior.offset were rewritten and
// the cached metadata + offset are stale. Without this guard, the
// rewritten file's new content would be parsed under the OLD session's
// metadata — mis-attribution worse than re-parsing from byte 0.
type parseState struct {
	offset   int64
	inode    uint64
	headCRC  uint32
	headLen  int
	metadata sessionMetadata
}

// checkpointFromState serializes a parseState into a store.WatcherCheckpoint.
// The session metadata is JSON-encoded into the opaque Metadata blob and MUST
// round-trip in full — NextParseSeq especially, since losing it would let
// id-less message dedup keys drift on resume and produce duplicate rows.
func checkpointFromState(path string, s parseState) (store.WatcherCheckpoint, error) {
	meta, err := json.Marshal(s.metadata)
	if err != nil {
		return store.WatcherCheckpoint{}, fmt.Errorf("marshal session metadata: %w", err)
	}
	return store.WatcherCheckpoint{
		Path:     path,
		Inode:    s.inode,
		Offset:   s.offset,
		HeadCRC:  s.headCRC,
		HeadLen:  s.headLen,
		Metadata: string(meta),
	}, nil
}

// stateFromCheckpoint is the inverse, used at startup to seed the in-memory
// offsets map from persisted checkpoints.
func stateFromCheckpoint(cp store.WatcherCheckpoint) (parseState, error) {
	var meta sessionMetadata
	if err := json.Unmarshal([]byte(cp.Metadata), &meta); err != nil {
		return parseState{}, fmt.Errorf("unmarshal session metadata: %w", err)
	}
	return parseState{
		offset:   cp.Offset,
		inode:    cp.Inode,
		headCRC:  cp.HeadCRC,
		headLen:  cp.HeadLen,
		metadata: meta,
	}, nil
}

// saveCheckpoint persists the tail state for path after a successful advance.
// Empty/uninitialized state (no inode or no head fingerprint) carries nothing
// to resume from and is skipped — a path that later rotates is self-healed by
// the inode/head staleness guard in process(), so a missing row is safe. A save
// failure is non-fatal: the checkpoint is a cache that dedup backstops; on
// shutdown ctx is cancelled, which is expected and logged at Debug.
func (w *Watcher) saveCheckpoint(ctx context.Context, path string, s parseState, logger *slog.Logger) {
	if s.inode == 0 || s.headLen == 0 {
		return
	}
	cp, err := checkpointFromState(path, s)
	if err != nil {
		logger.Warn("watcher: build checkpoint failed", "path", path, "err", err)
		return
	}
	if err := w.Checkpoints.SaveWatcherCheckpoint(ctx, cp); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Debug("watcher: checkpoint save cancelled on shutdown", "path", path)
			return
		}
		logger.Warn("watcher: checkpoint save failed", "path", path, "err", err)
	}
}

// deleteCheckpoint removes the persisted tail state for a path the watcher has
// dropped (file removed/rotated), mirroring the in-memory offsets delete. A
// stale row left behind would still be caught by the staleness guard on the next
// touch, so a delete failure is non-fatal and logged.
func (w *Watcher) deleteCheckpoint(ctx context.Context, path string, logger *slog.Logger) {
	if err := w.Checkpoints.DeleteWatcherCheckpoint(ctx, path); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Debug("watcher: checkpoint delete cancelled on shutdown", "path", path)
			return
		}
		logger.Warn("watcher: checkpoint delete failed", "path", path, "err", err)
	}
}

// headFingerprintBytes is the prefix length we hash for the staleness
// guard. 4 KB is enough to capture multiple JSONL lines (each line's
// sessionId is at byte 0 of every entry but the first line's content
// shifts under rewrites, so longer is more sensitive); 4 KB ReadAt is
// well under one disk block on every modern filesystem so cost is
// negligible.
const headFingerprintBytes = 4096

// resolveTargets normalises the configured Repos for fast prefix-comparison
// in the event loop. EvalSymlinks failures (path doesn't exist yet) fall back
// to the lexical form — a session can match later if the dir is created.
func resolveTargets(repos []string) []resolvedPath {
	out := make([]resolvedPath, 0, len(repos))
	for _, r := range repos {
		abs, err := filepath.Abs(r)
		if err != nil {
			abs = r
		}
		abs = filepath.Clean(abs)
		resolved := abs
		if x, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = filepath.Clean(x)
		}
		out = append(out, resolvedPath{abs: abs, resolved: resolved, scope: NewLiveRepoScope(r)})
	}
	return out
}

// matchTarget returns the configured target repo that cwd is at or inside. The
// bool is false when cwd is empty or matches no target. The returned target's abs
// path is the repo root the commit cache reads git log from (#99).
//
// AS OF #464 THIS IS NO LONGER A SECOND COPY OF THE RULE. The four-way symlink
// match and the worktree fallback both live in RepoScope, and the
// direct-before-worktree precedence across multiple targets lives in
// MatchScopes; this function only maps the winning index back to the
// resolvedPath the commit cache needs. Previously the live path, the batch path
// (filterSessionsByRepo) and the Codex collector each had their own copy, which
// is precisely how the collector's first draft ended up ordering multi-target
// matches differently from this one.
//
// Worktree fallback (now inside RepoScope.Classify): when the prefix match
// misses, cwd is resolved to its main repository root via gitMainRepoRoot (git's
// .git-file + commondir layout, no git exec) and the four-way match is retried
// against that root. This captures sessions running in a `git worktree` of a
// watched repo — whose CWD sits outside the repo's path prefix — while keeping
// foreign repos foreign (resolution maps cwd→root; the target set is still the
// authorization gate). Returning the MATCHED target means the commit cache reads
// git log from the MAIN repo root (target.abs), where the worktree branch's
// commits live (gitLog runs --all), so join-to-commit works.
//
// MEMOIZATION, decided explicitly (#464 R4): the scopes here are LIVE scopes, so
// they resolve the filesystem on every call and cache nothing — the behaviour
// this path has always had. A memoizing scope would be faster, but its cached
// negatives never expire, so a `git worktree add` during a run would leave that
// worktree's sessions foreign until the process restarted: a silent capture loss
// traded for stats that process() already dwarfs by parsing a file per debounce.
// Read-only Classify also keeps this safe for the concurrent per-path debounce
// goroutines that call it.
func matchTarget(cwd string, targets []resolvedPath) (resolvedPath, bool) {
	if cwd == "" || len(targets) == 0 {
		return resolvedPath{}, false
	}
	scopes := make([]*RepoScope, len(targets))
	for i, t := range targets {
		scopes[i] = t.scope
	}
	if i := MatchScopes(scopes, cwd); i >= 0 {
		return targets[i], true
	}
	return resolvedPath{}, false
}

// cwdMatchesAnyTarget returns true when the session's CWD is at or inside one
// of the configured target repos, OR is a git worktree of one. Mirrors the
// four-way match plus the worktree fallback in filterSessionsByRepo so the live
// and batch paths agree on what's in-scope.
func cwdMatchesAnyTarget(cwd string, targets []resolvedPath) bool {
	_, ok := matchTarget(cwd, targets)
	return ok
}

// watchAdder is the subset of *fsnotify.Watcher that addRecursive needs. It is
// an interface purely so unit tests can inject an Add that returns
// syscall.EMFILE/ENOSPC without exhausting real file descriptors — the machine
// running the suite cannot be pushed to its fd limit safely. *fsnotify.Watcher
// satisfies it directly.
type watchAdder interface {
	Add(string) error
}

// encodeProjectDir mirrors Claude Code's project-dir naming: it replaces every
// path separator in a CWD with '-', preserving the leading separator (observed:
// cwd /Users/dev/gitrepos/tier -> dir -Users-dev-gitrepos-tier).
// Verified against the live ~/.claude/projects layout. A session whose CWD is
// INSIDE a repo gets a project dir whose encoded name has the repo's encoded
// name as a PREFIX, which is why the prune below is a prefix match, not equality.
func encodeProjectDir(path string) string {
	return strings.ReplaceAll(path, string(filepath.Separator), "-")
}

// worktreePaths returns the filesystem paths of the linked git worktrees of the
// repo at root, read directly from root/.git/worktrees/<id>/gitdir (git's own
// on-disk layout, no git exec). Empty on any error or when there are no linked
// worktrees. It exists so pruning keeps the project dir of an OUT-OF-TREE
// worktree of a target: such a worktree's CWD shares no path prefix with the
// main repo, yet matchTarget accepts its sessions via the gitMainRepoRoot
// fallback — so a name-prefix prune that did not know about it would silently
// drop that worktree's capture. Best-effort: an unreadable layout just yields no
// extra prefixes (the worktree's own sessions still match at ingestion time; we
// only lose the fd optimisation's completeness for that dir).
func worktreePaths(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, ".git", "worktrees"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// gitdir points at the worktree's .git file (…/<worktree>/.git); the
		// worktree directory is its parent.
		b, err := os.ReadFile(filepath.Join(root, ".git", "worktrees", e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		if p := strings.TrimSpace(string(b)); p != "" {
			out = append(out, filepath.Dir(p))
		}
	}
	return out
}

// targetPrefixes returns the encoded-name prefixes to keep: each target's abs +
// symlink-resolved path, plus the path of every linked git worktree of each
// target (so out-of-tree worktrees are not pruned — see worktreePaths). Deduped
// so the prune inner loop stays small. Empty targets returns nil, which callers
// treat as "prune nothing".
func targetPrefixes(targets []resolvedPath) []string {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(targets)*2)
	out := make([]string, 0, len(targets)*2)
	add := func(path string) {
		enc := encodeProjectDir(path)
		if enc == "" {
			return
		}
		if _, ok := seen[enc]; ok {
			return
		}
		seen[enc] = struct{}{}
		out = append(out, enc)
	}
	for _, t := range targets {
		add(t.abs)
		add(t.resolved)
		for _, wt := range worktreePaths(t.abs) {
			add(wt)
		}
	}
	return out
}

// hasAnyPrefix reports whether base begins with any of prefixes. Pure prefix
// match is deliberately over-inclusive under encoding collisions (/a/b-c and
// /a/b/c both encode to -a-b-c): over-inclusion only wastes one watch, which is
// the safe direction — silently dropping a watch loses real cost data.
func hasAnyPrefix(base string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// addRecursive walks root and registers every subdirectory with the fsnotify
// watcher. fsnotify on Linux/macOS does not support a recursive flag, so we
// have to enumerate. Missing-dir or permission-denied on a single subdir is
// logged and skipped, not fatal.
//
// Pruning (#142): a TOP-LEVEL project dir (a direct child of projectsDir) whose
// encoded base name matches no configured target's encoded prefix is skipped
// with filepath.SkipDir — it can never produce an in-scope session, so watching
// it only burns a file descriptor (kqueue/macOS) and is the direct cause of
// EMFILE exhaustion. projectsDir itself is always watched so new-dir CREATE
// events still arrive, and everything BELOW a matched project dir is watched as
// before. The keep-set (targetPrefixes) includes each target's OUT-OF-TREE git
// worktrees, mirroring matchTarget's worktree fallback so a worktree's capture
// is never silently pruned. Fail-open toward capture: a dir whose name does not
// fit the expected encoded shape (does not start with '-') is watched, and when
// there are no targets (health-check mode) nothing is pruned.
//
// EMFILE/ENOSPC handling: on macOS every watched dir consumes an fd, so an aging
// projects/ tree makes fw.Add return EMFILE; on Linux, exhausting
// `fs.inotify.max_user_watches` makes it return ENOSPC. We log the first
// occurrence of EACH at ERROR with the platform-correct knob to raise, then
// continue at WARN so subsequent failures are findable but don't bury the
// recovery guidance in a flood. syscall.EMFILE/ENOSPC are defined on every
// target tier builds for (including Windows), so this file needs no build tags;
// the genuinely platform-specific inode lookup is split out into
// watcher_inode_unix.go / watcher_inode_other.go. Every Add failure, whichever
// branch, invokes OnWatchAddFailure so the health/metrics layer counts the
// silent degradation.
//
// Symlink note: filepath.WalkDir does not follow symlinks. If a developer
// has symlinked ~/.claude/projects/<repo> to another directory, the link
// target is not watched. Document in the user-facing readme when symlinked
// layouts become common.
func (w *Watcher) addRecursive(fw watchAdder, root, projectsDir string, targets []resolvedPath, logger *slog.Logger) error {
	prefixes := targetPrefixes(targets)
	var (
		enospcLogged bool
		emfileLogged bool
		skipped      int
	)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			logger.Warn("watcher: walk error", "path", path, "err", err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		// Skip dotfile dirs nested under projects/ (none exist today, but a
		// future Claude Code layout change shouldn't crash us). filepath.Base
		// strips the encoded-path prefix from project subdirs cleanly.
		if path != root && strings.HasPrefix(base, ".") {
			return filepath.SkipDir
		}
		// Prune non-target top-level project dirs. Only direct children of
		// projectsDir are candidates (subdirs of a matched project dir are
		// always watched); projectsDir itself is never a candidate. Fail open
		// when the name is not the expected encoded shape, or when there are no
		// prefixes to match against.
		if len(prefixes) > 0 && path != projectsDir && filepath.Dir(path) == projectsDir &&
			strings.HasPrefix(base, "-") && !hasAnyPrefix(base, prefixes) {
			skipped++
			return filepath.SkipDir
		}
		if addErr := fw.Add(path); addErr != nil {
			switch {
			case errors.Is(addErr, syscall.ENOSPC) && !enospcLogged:
				logger.Error("watcher: inotify watch limit reached — raise fs.inotify.max_user_watches",
					"path", path,
					"hint", "sudo sysctl fs.inotify.max_user_watches=524288",
				)
				enospcLogged = true
			case errors.Is(addErr, syscall.EMFILE) && !emfileLogged:
				logger.Error("watcher: file-descriptor limit reached — raise the open-file limit",
					"path", path,
					"hint", "ulimit -n 4096 (per shell) or launchctl limit maxfiles (persistent, macOS)",
				)
				emfileLogged = true
			default:
				logger.Warn("watcher: add failed", "path", path, "err", addErr)
			}
			if w.OnWatchAddFailure != nil {
				w.OnWatchAddFailure(path, addErr)
			}
		}
		return nil
	})
	if skipped > 0 {
		logger.Info("watcher: skipped non-target project dirs", "count", skipped)
	}
	return err
}

// logReposWithoutProjectDir emits one INFO per configured target that has no
// existing project dir under projectsDir yet (#142). That is NORMAL for a fresh
// repo — the dir appears on first Claude Code use and the CREATE handler watches
// it — but it is the first thing to check when live capture for one repo is
// silent, so make it greppable at startup rather than a mystery. Best-effort:
// an unreadable projectsDir is left to the caller's own error handling.
func logReposWithoutProjectDir(projectsDir string, targets []resolvedPath, logger *slog.Logger) {
	if len(targets) == 0 {
		return
	}
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}
	for _, t := range targets {
		prefixes := targetPrefixes([]resolvedPath{t})
		found := false
		for _, e := range entries {
			if e.IsDir() && hasAnyPrefix(e.Name(), prefixes) {
				found = true
				break
			}
		}
		if !found {
			logger.Info("watcher: no existing project dir for repo (will attach when created)", "repo", t.resolved)
		}
	}
}
