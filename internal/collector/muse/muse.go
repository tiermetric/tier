// Package muse captures Meta Muse Code spend from the session logs the Muse CLI
// writes locally to ~/.local/share/muse/sessions/YYYY/MM/DD/<session-id>/
// session.jsonl (#895), and the subagent sessions nested under
// <session-id>/subagent/<id>/session.jsonl (#901).
//
// It is the fourth LOCAL per-call collector, and structurally the twin of
// internal/collector/codexrollout: a date-partitioned tree of JSONL files,
// scanned on an interval, scoped by the directory the agent ran in. Read that
// package's doc first; what follows is only what differs.
//
// # What is billed
//
// Two record kinds are model calls Meta bills: an agent turn's model_completed
// event, and the automated_review_completed event of the model Muse asks
// whether a pending tool call is safe. Each yields ONE token event, keyed on the
// record's own `id` — unique per record, so re-reading a file re-derives the
// same keys and the store absorbs the repeat. The token mapping (cached input
// carved out of input; reasoning inside output) is stated and measured on
// mapAgentUsage.
//
// # Privacy
//
// A session file holds prompts, replies, tool arguments and tool output. This
// package decodes none of it: see the allowlist note in parse.go. What reaches
// a TokenEvent is the session id, the model name, token counts, the timestamp,
// the branch Muse recorded for the run (as an issue id), and the workspace root
// — which is used for repo scoping and then dropped, exactly as the other
// readers use their cwd; only the repo SLUG it resolves to is stored.
//
// # Why a run is held until it settles
//
// Muse writes a run's workspace_branch record AFTER the run's terminal event,
// i.e. after every model call it made. The store's ON CONFLICT clause updates
// only token counters, never issue_id — so an event emitted mid-run, before its
// branch is known, would be filed under the unattributed bucket FOREVER, and the
// later, correctly attributed re-read would collide with it and change nothing.
// A running session is the common case under a 5-minute scan, so this is not an
// edge. A call is therefore emitted only once its run has SETTLED:
//
//   - its workspace_branch record is present (branch known); or
//   - its terminal event is present and some record follows it (Muse chose not
//     to record a branch for that run); or
//   - the file has not been written for settleGrace (Muse exited mid-run, and
//     no branch will ever arrive).
//
// A file holding an unsettled call stays on a pending list and is re-read every
// pass until it settles, whatever its mtime — or a crashed run's spend would be
// stranded behind the scan cursor.
//
// # Subagent sessions
//
// A subagent session records no workspace_root, no branch and no pointer to its
// parent; its own run id is its session id. Two facts tie it to the parent, and
// only these are used (#901):
//
//   - the directory nesting <parent>/subagent/<child-id>/ names the parent
//     session log, whose workspace_root scopes the child's calls; and
//   - the parent's memory_reminder_child_session_linked record whose
//     child_session_id is <child-id> names the parent RUN that spawned it.
//
// The child's calls take that parent run's branch, and are held under the
// settle rules above applied to the PARENT run and the parent file — a child
// run ends before its parent's branch record is written. A child no link record
// names is held while the parent file is still being written (the parent's
// quiescence, or the child's own when there is no parent log), then EXCLUDED
// and counted at WARN; it is never filed under some other run's branch, and
// never written without a repo, because a stored row's issue is never
// rewritten. Scanning a parent always scans its children, whatever their mtime,
// so a child older than the cursor is still read when its parent changes.
package muse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

const (
	// DefaultScanInterval is the re-scan cadence when config omits scan_interval,
	// matching the other local collectors.
	DefaultScanInterval = 5 * time.Minute

	// MinScanInterval is the config floor.
	MinScanInterval = 30 * time.Second

	// cursorLagFactor and attributionLookback are codexrollout's, for codexrollout's
	// reasons: overlap is free (idempotency keys) and a miss is permanent, and the
	// git-log snapshot must reach past the ±30-minute commit join window.
	cursorLagFactor     = 2
	attributionLookback = 24 * time.Hour

	// settleGrace is how long a file must go unwritten before a run with no
	// terminal event is treated as abandoned and emitted without a branch. Long
	// on purpose: a run legitimately idles while it waits on a human tool
	// approval, and emitting it early would freeze it as unattributed (see the
	// package doc). It costs latency on a crashed run's spend, and it is still a
	// heuristic: a LIVE run idle longer than this at an approval gate is emitted
	// as unattributed:detached-head, and its later branch record cannot correct
	// the stored issue_id. That spend stays in the denominator, but its
	// attribution is wrong permanently.
	settleGrace = 6 * time.Hour

	// maxClockSkew bounds how far ahead of this machine's clock a record may be
	// stamped before it is treated as corrupt: a far-future ts would sit inside
	// every window query until then, and the store never rewrites ts.
	maxClockSkew = 24 * time.Hour

	// sessionFileName selects session logs out of everything else Muse keeps in a
	// session directory (sqlite files, CLI logs, tool output).
	sessionFileName = "session.jsonl"

	// subagentDirName is the directory under a session directory that holds one
	// directory per subagent session, named by the child's session id.
	subagentDirName = "subagent"

	// providerMeta namespaces the idempotency key. Constant rather than read from
	// the metadata record so a key never depends on a line that may be absent.
	providerMeta = "meta"

	// refuseFutureStamp is the collector-side refusal reason (it needs a clock).
	refuseFutureStamp = "timestamp_beyond_clock_horizon"
)

// RepoTarget names one repository this collector attributes cost to. Identical
// in shape and meaning to codexrollout.RepoTarget.
type RepoTarget struct {
	// Path is the git checkout root. A session whose workspace_root is at,
	// inside, or a git worktree of this path is attributed here.
	Path string
	// Slug is the operator override for the canonical "owner/repo" (#231).
	Slug string
}

// Config configures a Collector. Repos is required; everything else defaults.
type Config struct {
	// Home overrides the Muse home (~/.local/share/muse). Sessions are read from
	// its sessions/ subdirectory.
	Home        string
	Repos       []RepoTarget
	DeveloperID string
	Interval    time.Duration
	Logger      *slog.Logger
	// Now overrides the clock used for settlement and the timestamp horizon.
	// Nil means time.Now.
	Now func() time.Time
	// Settled, when set, is called after each pass that advances the cursor, from
	// Run's since through the pass's start less settleGrace, the longest a
	// quiescent run is held before it is emitted.
	Settled collector.SettledFunc
	// Lost, when set, records the span of spend a pass excludes for good (an
	// unlinked subagent past settleGrace, or refused records) or finds gone
	// (a held file deleted), before the cursor moves.
	Lost collector.LostFunc
}

// Collector reads Muse session logs and emits one TokenEvent per billed model
// call. It implements collector.Collector. Every field is unexported and New is
// the only way in, as for codexrollout.Collector: New is what makes a nil
// logger, an empty repo list or a zero interval impossible.
type Collector struct {
	sessionsDir string
	repos       []RepoTarget
	developerID string
	interval    time.Duration
	logger      *slog.Logger
	now         func() time.Time
	settled     collector.SettledFunc
	lost        collector.LostFunc

	slugOnce sync.Once
	slugs    []string

	mu sync.Mutex
	// fileFloor is the mtime pre-filter for the next Run pass (zero = none).
	fileFloor time.Time
	// pending holds files with an unsettled call, each with its oldest held
	// call's time; they are re-read every pass regardless of fileFloor.
	pending map[string]time.Time
}

// New builds a Collector, or returns an error for a configuration that could
// never capture anything.
func New(cfg Config) (*Collector, error) {
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("muse: at least one repo target is required (no target means no session can ever be attributed)")
	}
	for i, r := range cfg.Repos {
		if strings.TrimSpace(r.Path) == "" {
			return nil, fmt.Errorf("muse: repos[%d].Path is empty", i)
		}
	}
	home := cfg.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("muse: resolve home dir for the default Muse home: %w", err)
		}
		home = filepath.Join(h, ".local", "share", "muse")
	}
	c := &Collector{
		sessionsDir: filepath.Join(home, "sessions"),
		repos:       cfg.Repos,
		developerID: cfg.DeveloperID,
		interval:    cfg.Interval,
		logger:      cfg.Logger,
		now:         cfg.Now,
		settled:     cfg.Settled,
		lost:        cfg.Lost,
		pending:     map[string]time.Time{},
	}
	if c.interval <= 0 {
		c.interval = DefaultScanInterval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return collector.SourceMuse }

// SessionsDir returns the resolved sessions root, for startup logging.
func (c *Collector) SessionsDir() string { return c.sessionsDir }

// Run implements collector.Collector: one pass immediately, then one every
// Interval until ctx is cancelled. A failing pass is logged and retried, never
// returned — the same deliberate asymmetry as codexrollout.Run.
func (c *Collector) Run(ctx context.Context, since time.Time, ing collector.Ingester) error {
	if ing == nil {
		return fmt.Errorf("muse: Ingester is required")
	}
	c.runPass(ctx, since, ing)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.runPass(ctx, since, ing)
		}
	}
}

// runPass scans and ingests. The cursor and the pending set advance ONLY after
// every event of the pass was accepted; an aborted ingest leaves both where
// they were, so the tail is re-read next pass.
//
// An INCOMPLETE scan (a file that failed to read, a repo whose resolver failed)
// does not advance the cursor either, and keeps every pending file: a file the
// pass could not read, or classified as foreign only because its repo was
// unavailable, would otherwise fall behind the floor and never be read again.
// A persistent failure therefore pins the floor, trading repeated re-reads
// (and a repeated ERROR) for spend that is never skipped.
//
// There is deliberately no per-event floor (codexrollout advances one): a run
// held across passes emits events OLDER than anything already ingested, and a
// floor would drop them. The cost is that a touched file re-emits its settled
// events each pass, which the idempotency keys make no-ops at the store.
func (c *Collector) runPass(ctx context.Context, since time.Time, ing collector.Ingester) {
	passStart := c.now()
	floor, pending := c.cursor()
	events, nextPending, heldFrom, lost, walked, scanErr := c.scan(ctx, since, floor, pending)
	if !walked {
		// No repo was attributable or the tree could not be walked: nothing was
		// looked at, so the cursor must not move past files nobody read.
		c.logger.Error("muse scan could not run; will retry next scan", "err", logsafe.Err(scanErr))
		return
	}
	for _, ev := range events {
		if ctx.Err() != nil {
			return
		}
		if err := ing.Ingest(ctx, ev); err != nil {
			c.logger.Error("muse ingest failed; will retry next scan", "err", logsafe.Err(err))
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	// Spend excluded for good or gone is stored as lost before the cursor moves
	// past it (#913-D9 ruling R-8).
	for i := 0; c.lost != nil && i < len(lost); i++ {
		if err := c.lost(ctx, lost[i][0], lost[i][1]); err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("record lost spend: %w", err))
			break
		}
	}
	c.mu.Lock()
	if scanErr == nil {
		c.fileFloor = passStart
	} else {
		for p, at := range pending {
			holdFile(nextPending, p, at)
		}
	}
	c.pending = nextPending
	c.mu.Unlock()
	if scanErr != nil {
		c.logger.Error("muse scan incomplete; cursor held, failed files and held runs are retried next scan", "err", logsafe.Err(scanErr))
		return
	}
	// A held call is emitted later with its own, older timestamp, so the pass
	// settles no later than the oldest one.
	through := passStart.Add(-settleGrace)
	if !heldFrom.IsZero() && heldFrom.Before(through) {
		through = heldFrom
	}
	if c.settled != nil {
		c.settled(ctx, since, through)
	}
}

// holdFile keeps path pending with at if that is its oldest held call.
func holdFile(pending map[string]time.Time, path string, at time.Time) {
	if old, ok := pending[path]; !ok || at.Before(old) {
		pending[path] = at
	}
}

// cursor returns the lagged mtime floor and a copy of the pending set.
func (c *Collector) cursor() (time.Time, map[string]time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	floor := c.fileFloor
	if !floor.IsZero() {
		floor = floor.Add(-time.Duration(cursorLagFactor) * c.interval)
	}
	return floor, maps.Clone(c.pending)
}

// Collect implements collector.Collector: one stateless pass over every
// session file, returning events from files that parsed and an error naming
// those that did not. Unsettled runs are simply not returned; a later call
// returns them once they settle.
func (c *Collector) Collect(ctx context.Context, since time.Time) ([]collector.TokenEvent, error) {
	events, _, _, _, _, err := c.scan(ctx, since, time.Time{}, nil)
	return events, err
}

type scanTarget struct {
	scope    *collector.RepoScope
	resolver *collector.IssueResolver
	slug     string
}

// passStats counts what one pass did with every file and every billed record.
type passStats struct {
	files, failed, idle, noRoot, foreign int
	emitted, held, noRootCalls           int
	skippedLines, zeroToken, cacheWrite  int
	// subagentEmitted counts the emitted calls that came from subagent files.
	subagentEmitted int
	// unlinked counts subagent files no parent link record names; their calls
	// are held (parent still being written) or excluded (after settleGrace).
	unlinked, unlinkedHeld, unlinkedExcluded int
	// unlinkedWhy counts unlinked subagent files by cause (unlinkedNoLink…).
	unlinkedWhy  map[string]int
	refused      map[string]int
	firstRefusal string
}

func (s *passStats) refuse(reason string, n int, example string) {
	if s.refused == nil {
		s.refused = map[string]int{}
	}
	s.refused[reason] += n
	if s.firstRefusal == "" && example != "" {
		s.firstRefusal = example
	}
}

// scan walks the sessions root and emits every settled call from every file the
// floor admits plus every pending file. It returns the files still holding an
// unsettled call, heldFrom the oldest held call's timestamp (zero when none is
// held), lost the spans of spend it excluded for good or found gone, and
// walked=false when no file could be looked at (no
// attributable repo, or an unwalkable root), on which Run must not advance its
// cursor. Per-FILE and per-repo failures still count as walked; they are
// returned as err, on which Run holds its cursor too (see runPass).
func (c *Collector) scan(ctx context.Context, since, fileFloor time.Time, pending map[string]time.Time) (events []collector.TokenEvent, nextPending map[string]time.Time, heldFrom time.Time, lost [][2]time.Time, walked bool, err error) {
	now := c.now()
	gitFloor := time.Time{}
	if !fileFloor.IsZero() {
		gitFloor = fileFloor.Add(-attributionLookback)
	}
	targets, errs := c.resolveTargets(ctx, gitFloor)
	if len(targets) == 0 {
		return nil, pending, time.Time{}, nil, false, errors.Join(errs...)
	}
	scopes := make([]*collector.RepoScope, len(targets))
	for i := range targets {
		scopes[i] = targets[i].scope
	}
	paths, err := c.findSessionFiles(fileFloor, pending)
	if err != nil && paths == nil {
		return nil, pending, time.Time{}, nil, false, errors.Join(append(errs, err)...)
	} else if err != nil {
		// A directory the walk could not list may hold files it did not reach.
		errs = append(errs, err)
	}
	// A pending file findSessionFiles left out is provably gone, with its held
	// calls: lost from the oldest of them.
	for p, at := range pending {
		if _, found := slices.BinarySearch(paths, p); !found {
			lost = append(lost, [2]time.Time{at, now})
		}
	}
	developer := c.developerID
	if developer == "" {
		developer = collector.OSUsername()
	}

	var st passStats
	nextPending = map[string]time.Time{}
	hold := func(ts time.Time) {
		if heldFrom.IsZero() || ts.Before(heldFrom) {
			heldFrom = ts
		}
	}
	horizon := now.Add(maxClockSkew)
	// A parent log is read for each of its subagents as well as for itself; the
	// cache makes that one read per pass. Only parents are cached, so every other
	// parsed session is dropped after use.
	parents := map[string]bool{}
	for _, p := range paths {
		if pp, _, ok := subagentParent(p); ok {
			parents[pp] = true
		}
	}
	cache := map[string]*loadedFile{}
	load := func(p string) *loadedFile {
		if l, ok := cache[p]; ok {
			return l
		}
		l := loadSession(p)
		if parents[p] {
			cache[p] = l
		}
		return l
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return events, pending, time.Time{}, nil, false, err
		}
		st.files++
		// A file that fails to read stays pending if it was: its held calls must
		// not be stranded behind the cursor by one transient read error.
		fail := func(err error) {
			st.failed++
			errs = append(errs, fmt.Errorf("%s: %w", logsafe.Str(path), err))
			if at, was := pending[path]; was {
				holdFile(nextPending, path, at)
			}
		}
		l := load(path)
		if l.err != nil {
			fail(l.err)
			continue
		}
		sess := l.sess
		// runs is the session whose records settle each call's run and name its
		// branch; quietAt is the latest mtime whose silence abandons it.
		runs, quietAt := sess, l.modTime
		root := sess.WorkspaceRoot
		parentPath, childID, isChild := subagentParent(path)
		var parent *loadedFile
		if isChild {
			// A subagent is scoped ONLY through its parent, never by a record of
			// its own: the parent's root is the one fact that places it. A parent
			// log that is absent, or not a regular file (the walk refuses one
			// too), leaves nothing to link the child to: unlinked below.
			parent = load(parentPath)
			if parent.err != nil && !errors.Is(parent.err, fs.ErrNotExist) && !errors.Is(parent.err, errNotRegular) {
				fail(fmt.Errorf("parent session %s: %w", logsafe.Str(parentPath), parent.err))
				continue
			}
			// A nested subagent's parent is itself a subagent, which has no root
			// by this rule: it is never scoped by that subagent's own record.
			_, _, nested := subagentParent(parentPath)
			root = ""
			if parent.err == nil && !nested {
				root = parent.sess.WorkspaceRoot
			}
		}
		idx := collector.MatchScopes(scopes, root)
		if root != "" && idx < 0 {
			// Machine-global tree: a session in another repo is the common case,
			// not an error, and its damage is not ours to report. A session whose
			// root is UNKNOWN counts as ours below — damage that consumed the
			// metadata record is exactly the case worth reporting.
			st.foreign++
			continue
		}
		st.skippedLines += sess.SkippedLines
		st.zeroToken += sess.ZeroTokenCalls
		st.cacheWrite += sess.CacheWriteCalls
		refusedSpend := false
		for reason, n := range sess.Refused {
			st.refuse(reason, n, sess.firstRefusal)
			// A duplicate is already captured by its first record, not lost.
			refusedSpend = refusedSpend || (n > 0 && reason != refuseDuplicateID)
		}
		if refusedSpend {
			// The parser keeps refusal counts, not their timestamps (which may
			// themselves be absent). Conservatively mark the scan window lost,
			// including files from which no priceable call survived.
			lost = append(lost, [2]time.Time{since, now})
		}
		if len(sess.Calls) == 0 {
			st.idle++
			continue
		}
		linkedRun := ""
		// A child of a root-less parent is excluded as no_workspace_root below.
		if isChild && (parent.err != nil || root != "") {
			run, linked, why := "", false, unlinkedNoParentLog
			// Either file still being written means the parent run may be alive:
			// a child's hold ends on the LATER of the two silences.
			childQuietAt := l.modTime
			if parent.err == nil {
				run, linked = parent.sess.runForChild(childID)
				why = unlinkedNoLink
				if _, named := parent.sess.childRuns[childID]; named {
					why = unlinkedAmbiguous
				}
				if parent.modTime.After(childQuietAt) {
					childQuietAt = parent.modTime
				}
			}
			if !linked {
				// Nothing names the run this child belongs to. Hold it while the
				// parent may still write the link; after that, exclude it rather
				// than guess a run.
				st.unlinked++
				if st.unlinkedWhy == nil {
					st.unlinkedWhy = map[string]int{}
				}
				st.unlinkedWhy[why]++
				if now.Sub(childQuietAt) < settleGrace {
					st.unlinkedHeld += len(sess.Calls)
					for _, cl := range sess.Calls {
						hold(cl.Timestamp)
						holdFile(nextPending, path, cl.Timestamp)
					}
				} else {
					st.unlinkedExcluded += len(sess.Calls)
					if span, ok := windowSpan(sess.Calls, since, horizon); ok {
						lost = append(lost, span)
					}
				}
				continue
			}
			runs, quietAt, linkedRun = parent.sess, childQuietAt, run
		}
		if idx < 0 {
			// Billed calls, but no workspace_root to scope them by (the session's
			// own, or for a subagent its parent's): they cannot be attributed to
			// any repo, so they are excluded and counted, and report() raises
			// them to WARN.
			st.noRoot++
			st.noRootCalls += len(sess.Calls)
			continue
		}
		quiescent := now.Sub(quietAt) >= settleGrace
		for _, cl := range sess.Calls {
			if cl.Timestamp.After(horizon) {
				st.refuse(refuseFutureStamp, 1, fmt.Sprintf("%s: record %s stamped %s", refuseFutureStamp, logsafe.Str(cl.RecordID), cl.Timestamp.Format(time.RFC3339)))
				// Its corrupt stamp cannot locate the lost spend reliably.
				lost = append(lost, [2]time.Time{since, now})
				continue
			}
			if !since.IsZero() && cl.Timestamp.Before(since) {
				continue
			}
			runID := cl.RunID
			if isChild {
				runID = linkedRun
			}
			branch, recorded := runs.branchFor(runID)
			if !recorded && !runs.runEnded(runID) && !quiescent {
				st.held++
				hold(cl.Timestamp)
				holdFile(nextPending, path, cl.Timestamp)
				continue
			}
			events = append(events, c.event(sess, cl, &targets[idx], developer, branch))
			st.emitted++
			if isChild {
				st.subagentEmitted++
			}
		}
	}
	c.report(st)
	return events, nextPending, heldFrom, lost, true, errors.Join(errs...)
}

// windowSpan is the span of calls stamped from since through horizon.
func windowSpan(calls []call, since, horizon time.Time) (span [2]time.Time, ok bool) {
	for _, cl := range calls {
		if cl.Timestamp.Before(since) || cl.Timestamp.After(horizon) {
			continue
		}
		if !ok || cl.Timestamp.Before(span[0]) {
			span[0] = cl.Timestamp
		}
		if !ok || cl.Timestamp.After(span[1]) {
			span[1] = cl.Timestamp
		}
		ok = true
	}
	return span, ok
}

// loadedFile is one session log parsed in a pass, with the mtime stat'd AFTER
// the parse: a write landing between the two can only make the file look
// fresher, never quiescent over content not yet read.
type loadedFile struct {
	sess    *session
	modTime time.Time
	err     error
}

// errNotRegular refuses a path that is not a regular file. A parent log is
// opened by path, not reached by the walk, so it gets the walk's check here: a
// symlink would be followed out of the tree and a FIFO would block os.Open.
var errNotRegular = errors.New("not a regular file")

func loadSession(path string) *loadedFile {
	info, err := os.Lstat(path)
	if err != nil {
		return &loadedFile{err: err}
	}
	if !info.Mode().IsRegular() {
		return &loadedFile{err: fmt.Errorf("%w (mode %s)", errNotRegular, info.Mode().Type())}
	}
	sess, err := parseSession(path)
	if err != nil {
		return &loadedFile{err: err}
	}
	if info, err = os.Stat(path); err != nil {
		return &loadedFile{err: err}
	}
	return &loadedFile{sess: sess, modTime: info.ModTime()}
}

// subagentParent reports whether path is a subagent session log,
// <parent-dir>/subagent/<child-id>/session.jsonl, and if so returns the parent
// session log's path and the child's id (its directory name — the id the
// parent's link record names).
func subagentParent(path string) (parentPath, childID string, ok bool) {
	childDir := filepath.Dir(path)
	subDir := filepath.Dir(childDir)
	if filepath.Base(path) != sessionFileName || filepath.Base(subDir) != subagentDirName {
		return "", "", false
	}
	return filepath.Join(filepath.Dir(subDir), sessionFileName), filepath.Base(childDir), true
}

// event builds one TokenEvent. branch is Muse's own record for the run, or ""
// when it recorded none — which resolves to the labelled unattributed bucket,
// the convention every reader uses for a call with no recorded branch. It never
// reads the checkout's CURRENT branch: that would pin old spend on whatever
// happens to be checked out at scan time.
func (c *Collector) event(s *session, cl call, t *scanTarget, developer, branch string) collector.TokenEvent {
	cost, billingMode := store.ComputeCostHost("", cl.Model, store.CostUsage{
		Input:     cl.Tokens.Input,
		Output:    cl.Tokens.Output,
		CacheRead: cl.Tokens.CacheRead,
	})
	return collector.TokenEvent{
		Developer: developer,
		IssueID:   t.resolver.Resolve(branch, cl.Timestamp),
		Model:     cl.Model,
		InputTok:  cl.Tokens.Input,
		OutputTok: cl.Tokens.Output,
		CacheRead: cl.Tokens.CacheRead,
		CostMicro: cost,
		Source:    collector.SourceMuse,
		Fidelity:  collector.FidelityRealtime,
		// Muse's record id is unique per record and stable across re-reads of an
		// append-only file, so it alone identifies the call.
		IdempotencyKey: collector.IdempotencyKey(collector.SourceMuse, providerMeta, cl.RecordID),
		Repo:           t.slug,
		// Host is left empty, as in codexrollout: a local log records the
		// client's view, not the serving host.
		Host:        "",
		BillingMode: billingMode,
		SessionID:   s.SessionID,
		Timestamp:   cl.Timestamp,
	}
}

// report logs one pass. Refusals and skipped lines are spend that is MISSING,
// so any of them raises the line to WARN.
func (c *Collector) report(st passStats) {
	if st.files == 0 {
		return
	}
	level := slog.LevelInfo
	if len(st.refused) > 0 || st.skippedLines > 0 || st.failed > 0 {
		level = slog.LevelWarn
	}
	reasons := make([]string, 0, len(st.refused))
	for r, n := range st.refused {
		reasons = append(reasons, fmt.Sprintf("%s=%d", r, n))
	}
	sort.Strings(reasons)
	refused := "none"
	if len(reasons) > 0 {
		refused = strings.Join(reasons, " ")
	}
	c.logger.Log(context.Background(), level, "muse scan complete",
		"files", st.files,
		"events", st.emitted,
		"subagent_events", st.subagentEmitted,
		"held_unsettled_runs", st.held,
		"foreign_repo", st.foreign,
		"no_workspace_root", st.noRoot,
		"unlinked_subagents", st.unlinked,
		"no_billed_calls", st.idle,
		"failed_files", st.failed,
		"skipped_lines", st.skippedLines,
		"zero_token_calls", st.zeroToken,
		"cache_write_calls_at_input_rate", st.cacheWrite,
		"refused_calls", refused,
		"first_refusal", logsafe.Str(st.firstRefusal))
	if st.noRootCalls > 0 {
		c.logger.Warn(msgNoRootExcluded, "excluded_calls", st.noRootCalls, "sessions", st.noRoot)
	}
	if st.unlinked > 0 {
		c.logger.Warn(msgUnlinkedSubagent, "held_calls", st.unlinkedHeld, "excluded_calls", st.unlinkedExcluded,
			"sessions", st.unlinked, "reason", countsAttr(st.unlinkedWhy))
	}
}

// countsAttr renders counts as sorted "key=n" pairs, refused_calls' format.
func countsAttr(m map[string]int) string {
	out := make([]string, 0, len(m))
	for k, n := range m {
		out = append(out, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// Causes of an unlinked subagent, the WARN's reason attribute: no regular
// parent log; a parent naming no run for it; a parent naming two different runs.
const (
	unlinkedNoParentLog = "no_parent_log"
	unlinkedNoLink      = "no_link"
	unlinkedAmbiguous   = "ambiguous_link"
)

// msgNoRootExcluded is the WARN for billed calls excluded because their session
// (for a subagent, its parent session) recorded no workspace_root. That spend
// is real and missing from every figure.
const msgNoRootExcluded = "Muse calls excluded: session has no workspace_root"

// msgUnlinkedSubagent is the WARN for a subagent session linked to no single
// parent run; its reason attribute names why. Its calls are held while either
// log may still be written, then excluded: real spend, missing from every figure.
const msgUnlinkedSubagent = "Muse subagent calls held or excluded: no parent run is linked to this subagent"

// resolveTargets builds the per-repo scope and attribution snapshot for one
// pass. One unresolvable repo does not blind the others (codexrollout's rule).
func (c *Collector) resolveTargets(ctx context.Context, since time.Time) ([]scanTarget, []error) {
	c.resolveSlugs()
	targets := make([]scanTarget, 0, len(c.repos))
	var errs []error
	for i, r := range c.repos {
		resolver, err := collector.NewIssueResolver(ctx, r.Path, since)
		if err != nil {
			errs = append(errs, fmt.Errorf("muse: repo %s is not attributable this pass (skipped; other repos still scanned): %w", logsafe.Str(r.Path), err))
			continue
		}
		targets = append(targets, scanTarget{scope: collector.NewRepoScope(r.Path), resolver: resolver, slug: c.slugs[i]})
	}
	return targets, errs
}

// resolveSlugs resolves each target's canonical slug once: override, else
// remote.origin.url, else the 'unqualified' sentinel (codexrollout's rule).
func (c *Collector) resolveSlugs() {
	c.slugOnce.Do(func() {
		c.slugs = make([]string, len(c.repos))
		for i, r := range c.repos {
			if slug, ok := repoid.Canonical(r.Slug); ok {
				c.slugs[i] = slug
				continue
			}
			if r.Slug != "" {
				c.logger.Warn("muse: configured repo slug is not a canonical owner/repo; ignoring",
					"repo_path", logsafe.Str(r.Path), "configured", logsafe.Str(r.Slug))
			}
			if slug := collector.RepoSlugFromGitConfig(r.Path); slug != "" {
				c.slugs[i] = slug
				continue
			}
			c.slugs[i] = repoid.Unqualified
			c.logger.Warn("muse: cannot determine repository slug; Muse cost rows will be repo-unqualified",
				"repo_path", logsafe.Str(r.Path),
				"hint", "set the per-repo `repo:` override, or add a remote.origin.url")
		}
	})
}

// findSessionFiles returns every session.jsonl under the sessions root whose
// mtime is at or after floor, plus every pending file that still exists, in
// lexical order. A missing root is an empty scan, not an error (Muse may not be
// installed yet); it is Stat-checked up front because the walk callback below
// swallows per-entry errors (codexrollout #464 Y-G1).
func (c *Collector) findSessionFiles(floor time.Time, pending map[string]time.Time) ([]string, error) {
	if _, err := os.Stat(c.sessionsDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.logger.Info("muse sessions root does not exist; nothing to scan", "sessions_dir", logsafe.Str(c.sessionsDir))
			return nil, nil
		}
		return nil, fmt.Errorf("muse: sessions root %s: %w", logsafe.Str(c.sessionsDir), err)
	}
	set := map[string]struct{}{}
	// children collects every subagent log the walk reaches, by parent path,
	// whatever its mtime: scanning a parent scans all of its children.
	children := map[string][]string{}
	var entryErrs []error
	err := filepath.WalkDir(c.sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == c.sessionsDir {
				// An unlistable ROOT means nothing was looked at: fail the walk so
				// the cursor and pending set stay put.
				return err
			}
			c.logger.Warn("muse walk error", "path", logsafe.Str(path), "err", logsafe.Err(err))
			entryErrs = append(entryErrs, fmt.Errorf("muse: walk %s: %w", logsafe.Str(path), err))
			return nil
		}
		if d.IsDir() || d.Name() != sessionFileName {
			return nil
		}
		// Regular files only: a symlink would be followed out of the tree and a
		// FIFO would block os.Open forever (codexrollout's reasoning).
		if !d.Type().IsRegular() {
			c.logger.Warn("muse: refusing a non-regular file named like a session log",
				"path", logsafe.Str(path), "mode", d.Type().String())
			return nil
		}
		if parent, _, ok := subagentParent(path); ok {
			children[parent] = append(children[parent], path)
		}
		_, isPending := pending[path]
		if !floor.IsZero() && !isPending {
			if info, ierr := d.Info(); ierr == nil && info.ModTime().Before(floor) {
				return nil
			}
		}
		set[path] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("muse: walk %s: %w", logsafe.Str(c.sessionsDir), err)
	}
	// A pending file the walk did not reach (an unreadable directory) is still
	// scanned, so its read failure keeps it pending. Only a file that is provably
	// gone leaves the pending set, and a non-regular one is never opened.
	for p := range pending {
		if _, ok := set[p]; ok {
			continue
		}
		info, lerr := os.Lstat(p)
		if errors.Is(lerr, fs.ErrNotExist) || (lerr == nil && !info.Mode().IsRegular()) {
			continue
		}
		set[p] = struct{}{}
	}
	// A subagent file the floor passed is still read when its parent is: its
	// calls may never have been emitted (a pass before #901 excluded them, or
	// held them unlinked until grace), and its mtime alone never brings it back.
	// Repeated to a fixed point: a pulled-in child may itself be a parent, and one
	// pass over the map would depend on its iteration order.
	for grew := true; grew; {
		grew = false
		for parent, kids := range children {
			if _, ok := set[parent]; !ok {
				continue
			}
			for _, k := range kids {
				if _, ok := set[k]; !ok {
					set[k] = struct{}{}
					grew = true
				}
			}
		}
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, errors.Join(entryErrs...)
}
