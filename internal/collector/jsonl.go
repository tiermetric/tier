package collector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// JSONLCollector reads Claude Code session files from ~/.claude/projects/ and
// joins them to the git log of a target repository to produce per-commit token
// attribution.
type JSONLCollector struct {
	// RepoPath is the root of the git repository to join against.
	RepoPath string
	// ClaudeDir overrides the default ~/.claude directory (for testing).
	ClaudeDir string
	// DeveloperID is used for all events from this machine.
	// Falls back to the OS username if empty.
	DeveloperID string
	// RepoSlug is the OPERATOR OVERRIDE for this repository's canonical
	// "owner/repo" identity (#231). It wins over remote.origin.url.
	//
	// It exists for forks: a contributor whose origin is "alice/tier" produces cost
	// rows that would never join outcomes from the upstream "tiermetric/tier"
	// webhook. Setting this to the upstream slug fixes that; nothing else can.
	//
	// Empty -> fall back to remote.origin.url, then to repoid.Unqualified with one
	// warning per collector.
	RepoSlug string

	// repoOnce/repoResolved memoize the slug so the warning fires once, not once per
	// scanned session file.
	repoOnce     sync.Once
	repoResolved string
}

// repo returns the canonical repository slug for this collector's target, resolving
// (and warning) at most once: operator override, else remote.origin.url, else the
// 'unqualified' sentinel.
//
// Degrading to the sentinel is deliberate rather than fatal. A repo we cannot name
// still produces true per-developer cost — the primary TIER denominator groups by
// developer alone and is unaffected. What degrades is only cross-repo issue
// disambiguation, so failing the scan would trade a real number for no number. But
// it must be OBSERVABLE, never silent: an operator whose capture is repo-blind needs
// to know why their multi-repo scores still fuse.
func (c *JSONLCollector) repo() string {
	c.repoOnce.Do(func() {
		if slug, ok := repoid.Canonical(c.RepoSlug); ok {
			c.repoResolved = slug
			return
		}
		if c.RepoSlug != "" {
			slog.Warn("configured repo slug is not a canonical owner/repo; ignoring",
				"repo_path", c.RepoPath, "configured", c.RepoSlug)
		}
		if slug := RepoSlugFromGitConfig(c.RepoPath); slug != "" {
			c.repoResolved = slug
			return
		}
		c.repoResolved = repoid.Unqualified
		slog.Warn("cannot determine repository slug; cost rows will be repo-unqualified and multi-repo issues sharing a number will fuse",
			"repo_path", c.RepoPath,
			"hint", "set the per-repo `repo:` override, or add a remote.origin.url")
	})
	return c.repoResolved
}

// Name implements Collector.
func (c *JSONLCollector) Name() string { return SourceJSONL }

// jsonlEntry is the minimal set of fields we need from each JSONL line.
//
// Note on Message.ID and dedup: Claude Code emits one assistant entry per
// streaming chunk during a response, then a final entry with the definitive
// usage block. The placeholder chunks share the same message.id but carry
// partial or zero token counts (gille.ai, Apr 2026 — input_tokens is 0/1 in
// ~75% of streaming entries; output_tokens is 10–17× undercounted in some).
// Summing all entries naively double-counts the cache fields and undercounts
// non-cache tokens. The fix: dedup by message.id and keep the entry with the
// largest total (the post-stream definitive one).
type jsonlEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	GitBranch string    `json:"gitBranch"`
	CWD       string    `json:"cwd"`
	SessionID string    `json:"sessionId"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens int `json:"input_tokens"`
			// CacheCreationInputTokens is the rolled-up cache-write count.
			// Newer Anthropic responses also include a nested CacheCreation
			// object that splits it by TTL bucket; when present, that's the
			// authoritative source. Kept here for legacy JSONL (pre-1h-feature)
			// and as a fallback if the nested object is missing.
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			// CacheCreation carries the 5m vs 1h TTL split for cache writes.
			// Pointer so we can distinguish "field absent" (legacy JSONL —
			// bucket all writes as 5m) from "field present with zero values".
			CacheCreation *struct {
				Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// messageUsage captures the per-message dedup state used inside parseSessionFile.
//
// parseOrder is a stable per-file ordinal captured the first time we see a
// given message.id (or once per ID-less entry). It anchors the ID-less
// fallback IdempotencyKey, so a re-scan of the same file always produces the
// same key for the same physical position — without depending on map
// iteration order or sort stability.
//
// Model is stored per-message: Claude Code emits a stable message.model
// across every entry sharing a message.id, but we keep it here so the
// downstream emission carries the model that produced each message.
type messageUsage struct {
	messageID string // upstream id ("msg_*"); empty for older JSONL formats
	timestamp time.Time
	// gitBranch is THIS message's own branch, captured from the assistant line
	// that produced it — not the session-latched branch. Claude Code stamps
	// gitBranch on every assistant line, and a session commonly OPENS on main
	// before a feature checkout, so the first-seen (session) branch mis-routes
	// later feature-branch work. Carrying the per-message branch lets
	// joinSessionsToCommits attribute each event by the branch it actually
	// happened on (#refocus, Option A). Empty when the line carried no branch;
	// the join then falls back to the session-latched branch.
	gitBranch    string
	model        string
	input        int
	output       int
	cacheRead    int
	cacheWrite5m int // 5-minute cache writes; legacy entries with no TTL split bucket here
	cacheWrite1h int // 1-hour cache writes (Anthropic only)
	// nestedTTL records whether this entry's cache-write counts came from
	// the nested `cache_creation` object (true) or from the legacy
	// fallback bucketing of the rolled-up `cache_creation_input_tokens`
	// into 5m (false). Used as a tie-breaker in the same-message dedup:
	// when two entries share a message.id and tie on total(), the
	// nested-shape entry wins so we don't silently downgrade real TTL
	// information into the all-5m fallback. See #55.
	nestedTTL  bool
	parseOrder int // first-seen position in this file; stable across re-scans
}

// total returns the sum of all token fields, used to decide which duplicate
// message.id wins (largest total = definitive post-stream entry).
func (m messageUsage) total() int {
	return m.input + m.output + m.cacheRead + m.cacheWrite5m + m.cacheWrite1h
}

// sessionSummary aggregates all assistant usage from a single session file.
//
// The aggregate fields (InputTok / OutputTok / CacheRead / CacheWrite / Model)
// are still computed for backwards compatibility — tests and any future
// session-level consumer can read them — but the authoritative per-event data
// is in Messages. joinSessionsToCommits emits one TokenEvent per Messages
// entry, keyed by the upstream message id for cross-source dedup (closes #19).
type sessionSummary struct {
	SessionID    string
	GitBranch    string
	CWD          string
	StartTime    time.Time
	EndTime      time.Time
	Model        string
	InputTok     int
	OutputTok    int
	CacheRead    int
	CacheWrite5m int
	CacheWrite1h int
	Messages     []messageUsage
	// NextParseSeq is the ID-less ordinal the next incremental call must
	// seed parseSeq from so its IdempotencyKeys don't collide with this
	// call's keys. Watcher copies this into the cached sessionMetadata;
	// CLI consumers can ignore it.
	NextParseSeq int
	// LastRealBranch is the most recent HUMAN-named branch seen in this file —
	// the branch a worktree-agent message inherits (see
	// parseSessionFileFromOffset). It must survive across incremental parses,
	// because a tail parse can begin AFTER the line that named the branch and
	// would otherwise have nothing to inherit; the watcher carries it in
	// sessionMetadata for exactly that reason.
	//
	// ⚠️ SCOPE IS THE FILE, NOT THE SESSION. A JSONL file that violates the
	// upstream one-sessionId-per-file invariant is warned about once and then
	// parsed straight through under the FIRST session id (see the mixedSessionWarned
	// branch below) — so the latch can carry a branch across the boundary and a
	// worktree-agent message can inherit a branch named by a different session.
	// That is pre-existing, documented behaviour, but #490 made this field a new
	// consumer of it: before, a mixed file merely mis-attributed the counts it
	// already had; now it can also hand out a branch. Both stay wrong in the same
	// direction and for the same reason, so the fix belongs at the parse boundary
	// (split on sessionId) rather than here — do not paper over it with a
	// latch-local reset, which would leave the counts mis-keyed anyway.
	LastRealBranch string
}

// gitCommit is a parsed row from git log.
type gitCommit struct {
	Hash      string
	Timestamp time.Time
	Subject   string
	Branches  []string // branch names from the decorate field
	IssueID   string   // extracted from branch or commit message
}

// Run implements Collector. It scans all JSONL session files in the Claude
// projects directory, joins them to the git log of c.RepoPath, and forwards
// each resulting TokenEvent into ing in order. Stops on the first Ingest
// error or ctx cancellation; partial progress (events already accepted) is
// NOT rolled back — the Ingester decides its own durability story (the
// store adapter relies on SQLite's per-statement atomicity).
//
// Memory note: this implementation materialises the full event slice via
// collectEvents before forwarding, so it's not truly streaming — at JSONL
// scale (hundreds of events per scan) that's fine. Streaming-capable
// collectors (a future Anthropic Admin paginated poller) should call
// Ingest as events are produced. New collectors should follow this
// Run-into-Ingester shape so every source funnels into the single
// `ingester.Store(*store.DB)` adapter wired by cmd/tierd. See #27.
func (c *JSONLCollector) Run(ctx context.Context, since time.Time, ing Ingester) error {
	if ing == nil {
		return fmt.Errorf("jsonl collector: Ingester is required")
	}
	events, err := c.collectEvents(ctx, since)
	if err != nil {
		return err
	}
	for _, ev := range events {
		// Honour ctx cancellation between events so a long backlog on a
		// cancelled context doesn't keep firing Ingest calls. Each call
		// also passes ctx into Ingest, which is the second layer of
		// defense if the sink itself does context-aware I/O.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ing.Ingest(ctx, ev); err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
	}
	return nil
}

// Collect is a convenience wrapper around Run for callers that want every
// event in a slice (the tierd score CLI is the only one today; it
// aggregates events into a printable cost report). New code should call
// Run directly with an appropriate Ingester.
func (c *JSONLCollector) Collect(ctx context.Context, since time.Time) ([]TokenEvent, error) {
	return c.collectEvents(ctx, since)
}

// collectEvents is the shared implementation. Split out so Run and Collect
// can share the parse/filter/join pipeline without one calling the other
// (which would duplicate the slice allocation).
func (c *JSONLCollector) collectEvents(ctx context.Context, since time.Time) ([]TokenEvent, error) {
	if err := validateGitRepo(c.RepoPath); err != nil {
		return nil, fmt.Errorf("repo validation: %w", err)
	}

	claudeDir := c.ClaudeDir
	if claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		claudeDir = filepath.Join(home, ".claude")
	}
	projectsDir := filepath.Join(claudeDir, "projects")

	sessions, err := parseAllSessions(projectsDir, since)
	if err != nil {
		return nil, fmt.Errorf("parse sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil
	}

	// Drop sessions recorded in other repositories. Without this filter,
	// joinSessionsToCommits' branch-name fallback would attribute every Claude
	// Code session on the machine to whatever repo the caller specified, since
	// the per-session branch (e.g. "feature/947-foo") parses to an issue id even
	// when the session was never on this repo (#15).
	sessions = filterSessionsByRepo(sessions, c.RepoPath)
	if len(sessions) == 0 {
		return nil, nil
	}

	commits, err := gitLog(ctx, c.RepoPath, since)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	developer := c.DeveloperID
	if developer == "" {
		developer = osUsername()
	}

	return joinSessionsToCommits(sessions, commits, developer, c.repo()), nil
}

// validateGitRepo checks that path contains a .git entry, preventing git log
// from being run on non-repo paths. The .git entry may be either a directory
// (a normal clone) or a regular file (a "gitdir: ..." pointer, as git worktree
// and submodules create): both are legitimate checkouts, so only a missing
// .git is rejected. This matches the serve/--watch-repo path (resolveWatchRepos
// in cmd/tierd/main.go); keeping the two consistent lets tierd score/backfill/
// seam-exercise run inside a worktree, not just serve (#342).
func validateGitRepo(path string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return fmt.Errorf("%s does not appear to be a git repository (.git not found); run this command from inside a git repository, or pass --repo <path> to point at one", path)
	}
	return nil
}

// parseAllSessions walks the Claude projects directory and parses every *.jsonl
// file, returning one sessionSummary per session that has assistant usage.
func parseAllSessions(projectsDir string, since time.Time) ([]sessionSummary, error) {
	var summaries []sessionSummary

	err := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Log but don't abort on per-entry errors (permission denied, etc.)
			slog.Warn("walk error", "path", logsafe.Str(path), "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		s, err := parseSessionFile(path)
		if err != nil {
			slog.Warn("failed to parse session file", "path", logsafe.Str(path), "err", err)
			return nil
		}
		if s == nil || s.EndTime.Before(since) {
			return nil
		}
		summaries = append(summaries, *s)
		return nil
	})
	return summaries, err
}

// filterSessionsByRepo keeps only sessions whose CWD resolves to repoPath.
// Sessions with empty CWD are dropped because we cannot safely attribute them.
// A summary count is logged once per scan — at WARN when NOTHING matched
// (#549 arm 1: a zero-kept target is nearly always a wrong path), at INFO
// when some but not all sessions were filtered.
//
// The scope decision itself — the four-way symlink match, the git-worktree
// fallback, and the per-scan memoization — lives in RepoScope (reposcope.go),
// shared with the LIVE watcher (matchTarget) and the Codex rollout-log collector
// so all three capture paths cannot drift about which sessions are in scope. This function is the batch wrapper:
// it adds the per-scan operator diagnostics (kept / foreign / worktree /
// empty_cwd counts) that a bare scope decision has no place to report.
func filterSessionsByRepo(sessions []sessionSummary, repoPath string) []sessionSummary {
	scope := NewRepoScope(repoPath)
	kept := make([]sessionSummary, 0, len(sessions))
	var foreign, empty, worktree int
	for _, s := range sessions {
		// Empty CWD is counted separately from foreign purely for diagnostics:
		// "we recorded no directory" and "it ran somewhere else" are different
		// problems for an operator, even though both are out of scope.
		if s.CWD == "" {
			empty++
			continue
		}
		switch scope.Classify(s.CWD) {
		case RepoScopeDirect:
			kept = append(kept, s)
		case RepoScopeWorktree:
			kept = append(kept, s)
			worktree++
		case RepoScopeForeign:
			foreign++
		}
	}
	switch {
	// worktree is deliberately absent from this guard and its log fields
	// (#549): every RepoScopeWorktree match is appended to kept in the loop
	// above, so len(kept)==0 already implies worktree==0 — the disjunct was
	// dead and the field below always logged a constant worktree=0.
	case len(kept) == 0 && (foreign > 0 || empty > 0):
		// WARN, not INFO (#549 arm 1). A --repo target that matched NOTHING at
		// all is nearly always an operator error — a stale checkout, a typo, a
		// path copied from another machine — not a repo that's genuinely idle.
		// During the 2026-07-30 dogfood backfill this exact case (kept=0,
		// foreign=4836) logged at INFO and `ship` exited 0 anyway; nothing
		// about the run signaled a problem short of an operator diffing
		// `select count(*) from token_events` by hand. INFO is what a healthy
		// scan emits too (some sessions filtered, most kept), so raising ONLY
		// the all-filtered case to WARN keeps the two severities meaningfully
		// different rather than training operators to ignore WARN.
		slog.Warn("no sessions matched this --repo target; nearly always a wrong --repo path, not an idle repo",
			"repo", scope.Target(),
			"foreign", foreign,
			"empty_cwd", empty,
		)
	case foreign > 0 || empty > 0 || worktree > 0:
		slog.Info("filtered sessions outside target repo",
			"repo", scope.Target(),
			"kept", len(kept),
			"foreign", foreign,
			"worktree", worktree,
			"empty_cwd", empty,
		)
	}
	return kept
}

// pathInside returns true when child equals parent or is a descendant directory.
// Both arguments must already be filepath.Clean'd. The trailing separator on
// parent prevents false positives like "/repo-other" matching "/repo".
func pathInside(child, parent string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// cwdWithinTarget reports whether either symlink form of a cwd is at or inside
// either symlink form of a target. The four combinations cover all symlink
// permutations (target symlinked, cwd symlinked, both, neither). All four
// arguments must already be filepath.Clean'd. Shared by the batch
// (filterSessionsByRepo) and live (matchTarget) paths — and by the worktree
// fallback in each — so they agree byte-for-byte on what is in-scope.
func cwdWithinTarget(cwdAbs, cwdResolved, targetAbs, targetResolved string) bool {
	return pathInside(cwdAbs, targetAbs) ||
		pathInside(cwdResolved, targetResolved) ||
		pathInside(cwdAbs, targetResolved) ||
		pathInside(cwdResolved, targetAbs)
}

// maxJSONLLine caps the bufio.Scanner line buffer at 10 MB to match the proxy's
// response buffer (the maxBody cap in proxy.handleJSON). A single Claude Code
// assistant entry — even a
// fully-buffered streaming response — fits well under this limit. Lines beyond
// the cap terminate the scan (logged in parseSessionFile via scanner.Err).
const maxJSONLLine = 10 << 20

// maxJSONLChunk caps the in-memory chunk parseSessionFileFromOffset loads at
// once (#30). At 64 MB this comfortably exceeds any realistic full session
// file while bounding RAM if some path produces a runaway file. Set
// independently of maxJSONLLine because a chunk holds many lines.
const maxJSONLChunk = 64 << 20

// sessionMetadata is the per-file session-level fields normally extracted
// from the first assistant entry: session id, branch, cwd, start time, and
// the session's first observed model. Cached by the watcher across
// debounce cycles (#30) so incremental parses don't have to re-read the
// file head just to recover this info.
//
// NextParseSeq carries the ID-less fallback ordinal across debounce
// boundaries. The IdempotencyKey for entries without a message.id embeds
// this ordinal (joinSessionsToCommits → IdempotencyKey(..., "noid",
// strconv.Itoa(parseOrder))); without persisting it across calls, two
// ID-less entries from different debounces would both receive
// parseOrder=0 and collide on the store's partial unique index, silently
// dropping the second. Persist + thread through.
type sessionMetadata struct {
	SessionID string
	GitBranch string
	CWD       string
	StartTime time.Time
	Model     string
	// LastRealBranch carries the worktree-agent inheritance latch across
	// debounces — see sessionSummary.LastRealBranch. Without it a tail parse
	// whose window opens on a worktree-agent line has no branch to inherit and
	// the spend falls back to unattributed, which is the #490 defect returning
	// through the incremental path only.
	LastRealBranch string
	NextParseSeq   int
}

// parseSessionFile is a back-compat wrapper for callers (the CLI's
// tierd score path) that want a one-shot full parse. tailMode=false
// means: consume even a final line that lacks a trailing newline. A
// static log file the user is inspecting may legitimately end without
// a \n, and the CLI shouldn't drop those bytes.
func parseSessionFile(path string) (*sessionSummary, error) {
	s, _, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, false)
	return s, err
}

// parseSessionFileFromOffset reads a single JSONL file starting at
// startOffset and returns an aggregated session summary plus the byte
// offset the next parse should resume from (#30). Used by the watcher's
// incremental tailing path: on each debounce, only the bytes appended
// since the last parse are read.
//
// startOffset semantics:
//   - 0 (or knownMeta is zero): full parse. Metadata is extracted from the
//     first assistant entry; offset is returned at end-of-last-complete-line
//     so the caller can resume incrementally next time.
//   - >0: resume from offset using knownMeta. New messages are emitted
//     without re-reading the prior bytes.
//
// tailMode semantics:
//   - true (watcher path): partial final lines (no trailing \n) are NOT
//     consumed — a writer mid-flush is safe and the partial line is picked
//     up on the next debounce once the \n arrives.
//   - false (CLI / one-shot path): every byte through end-of-file is
//     consumed, including a final line that lacks a trailing newline. A
//     static log a user inspects from the CLI may legitimately end without
//     a \n; dropping those bytes would lose real data.
//
// Dedup-by-message-id and stable parseOrder for ID-less entries still
// hold WITHIN a single call. Cross-call dedup is the store layer's job
// via the partial unique index on idempotency_key (#19) and the
// MAX-on-conflict UPSERT (#18): the same message.id surfacing in two
// consecutive debounce parses lands as one row with the larger totals.
//
// Per-message usage is deduplicated by message.id: the entry with the
// highest total token count wins. Ties resolve later-wins, matching the
// "post-stream definitive entry" intuition. Entries lacking a message.id
// (older JSONL formats, malformed lines) are keyed by a per-file
// sequence number so they're counted exactly once.
//
// Malformed lines are skipped. Unlike the previous json.Decoder
// implementation, bufio.Scanner parses each line independently, so a
// syntax error on one line cannot stall or infinite-loop the rest of the
// file (resolves issue #7).
func parseSessionFileFromOffset(path string, startOffset int64, knownMeta sessionMetadata, tailMode bool) (*sessionSummary, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	// Capture file size at parse start so newOffset is bounded by what we
	// actually committed to parse. Writes that arrive while we're scanning
	// extend the file past this size; they'll be picked up next debounce.
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	endOffset := info.Size()
	if startOffset > endOffset {
		// File shrunk since last parse — caller should have detected the
		// inode/size mismatch and re-driven with offset=0; defensive
		// fallback: return early with no events and the new size so the
		// caller realises and re-parses.
		return nil, endOffset, nil
	}
	chunkLen := endOffset - startOffset
	if chunkLen == 0 {
		return nil, endOffset, nil
	}
	if chunkLen > maxJSONLChunk {
		// Defensive bound on RAM. A 10MB session file is the legitimate
		// upper bound (the line cap from #7 implies that's also the
		// reasonable file ceiling). Refuse to load more in one shot.
		// Preserve the caller's startOffset (rather than returning 0)
		// so the watcher doesn't drop its cached state and re-fail on
		// the same oversize chunk every debounce. The slog.Warn fires
		// once per debounce; an operator should investigate.
		slog.Warn("JSONL chunk exceeds size cap, skipping this debounce",
			"path", logsafe.Str(path),
			"chunk_bytes", chunkLen,
			"cap_bytes", maxJSONLChunk,
		)
		return nil, startOffset, nil
	}
	chunk := make([]byte, chunkLen)
	// ReadAt can return n < len(chunk) with err == nil only at EOF — but
	// the file may also have been truncated between Stat and ReadAt
	// (concurrent writer doing truncate+rewrite). Trim chunk to the
	// actual bytes read so the scanner never sees zero-filled tail
	// bytes, which would otherwise decode as garbage and either trigger
	// "skipped malformed JSONL lines" warnings or — worse — advance the
	// offset past bytes that never existed.
	n, err := f.ReadAt(chunk, startOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	chunk = chunk[:n]
	// Recompute endOffset from what was actually read; a TOCTOU truncate
	// would make the original Stat-derived endOffset overshoot.
	endOffset = startOffset + int64(n)

	// In tailMode, trim the chunk to the position immediately after the last
	// '\n' so the scanner sees only complete lines and the partial final
	// line (if any) stays uneaten in the file. Without this, bufio.Scanner
	// would emit the partial bytes as a "line", fail to JSON-decode it, and
	// silently advance the offset past it — the partial line would be lost
	// forever once completed. The caller's next debounce, after the writer
	// flushes the terminating \n, picks up the now-complete line.
	//
	// In one-shot mode (CLI) the file is treated as static — a missing
	// trailing \n is the file's actual end, not a writer mid-flush, so we
	// scan the entire chunk including any partial last line.
	var newOffset int64
	if tailMode {
		lastNL := bytes.LastIndexByte(chunk, '\n')
		if lastNL < 0 {
			// No complete lines in the chunk — nothing to parse, don't advance.
			// Caller's offset stays at startOffset so the same bytes are
			// re-evaluated next debounce.
			return nil, startOffset, nil
		}
		consumedEnd := lastNL + 1
		newOffset = startOffset + int64(consumedEnd)
		chunk = chunk[:consumedEnd]
	} else {
		newOffset = endOffset
	}

	var s sessionSummary
	if startOffset > 0 {
		// Use the cached metadata so joinSessionsToCommits has everything
		// it needs (GitBranch, CWD, SessionID, StartTime) without us
		// re-reading the file head.
		s.SessionID = knownMeta.SessionID
		s.GitBranch = knownMeta.GitBranch
		s.CWD = knownMeta.CWD
		s.StartTime = knownMeta.StartTime
		s.Model = knownMeta.Model
		// Seed the #490 inheritance latch too. This window may open partway
		// through a worktree-agent run, after the line that named the branch has
		// already been consumed by an earlier debounce.
		s.LastRealBranch = knownMeta.LastRealBranch
	}

	// Dedup buffer: keep the largest-total usage per message.id seen in this file.
	messages := make(map[string]messageUsage)
	var fallbackSeq int // for assistant entries without a message.id
	// parseSeq seeds the ID-less ordinal from knownMeta.NextParseSeq so
	// incremental calls don't reset to 0 — that would let two ID-less
	// entries from different debounces share parseOrder=0 and collide on
	// the store's partial unique index.
	parseSeq := knownMeta.NextParseSeq
	mixedSessionWarned := false

	// bufio.Scanner's default 64 KB buffer truncates long Anthropic
	// responses; raise it to maxJSONLLine. Scanning over the in-memory
	// chunk (trimmed to the last complete line above) means every Scan
	// returns a guaranteed-complete line and the partial-line problem is
	// avoided structurally.
	scanner := bufio.NewScanner(bytes.NewReader(chunk))
	scanner.Buffer(nil, maxJSONLLine)

	// Per-file skip counter — emitted as one slog.Warn at function exit
	// rather than per-line debug, which floods at higher log levels and
	// reveals nothing at the default INFO.
	var skipped int

	for scanner.Scan() {
		// TrimSpace handles real-world artifacts: stray whitespace lines from
		// editors and CRLF line endings (Scanner strips '\n' but leaves a
		// trailing '\r' that json.Unmarshal then rejects).
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// One malformed line does not poison the file — keep going.
			skipped++
			continue
		}

		if s.SessionID == "" {
			s.SessionID = entry.SessionID
		} else if entry.SessionID != "" && entry.SessionID != s.SessionID && !mixedSessionWarned {
			// Upstream JSONL format invariant violated. Warn once per file
			// so the operator notices, but keep parsing under the first
			// session id (downstream IdempotencyKey will key to it).
			slog.Warn("JSONL file contains multiple sessionIds; counts will be attributed to the first",
				"path", logsafe.Str(path),
				"first_session", logsafe.Str(s.SessionID),
				"other_session", logsafe.Str(entry.SessionID),
			)
			mixedSessionWarned = true
		}
		// CWD and StartTime are captured from the first line that carries
		// each — NOT latched to the first line of the file. Current Claude
		// Code sessions lead with a last-prompt/permission-mode entry that
		// has a sessionId but no cwd; the real cwd lands on later
		// user/attachment lines (#96). Latching all metadata on the first
		// line left CWD empty, so filterSessionsByRepo/cwdMatchesAnyTarget
		// dropped the whole session as foreign and recorded $0. GitBranch is
		// already captured first-non-empty below; cwd and start follow suit.
		if s.CWD == "" && entry.CWD != "" {
			s.CWD = entry.CWD
		}
		if s.StartTime.IsZero() && !entry.Timestamp.IsZero() {
			s.StartTime = entry.Timestamp
		}
		if !entry.Timestamp.IsZero() {
			s.EndTime = entry.Timestamp
		}
		// 🔴 #490: a harness-invented worktree-agent branch carries NO attribution
		// signal, so it neither latches as the session branch nor overwrites the
		// inheritance latch — it only READS the latch, below.
		//
		// MEASURED on this repo's own session files (all 23 files carrying such a
		// branch, 2026-08-04): for all 190 spend-bearing worktree-agent messages,
		// "most recent preceding human-named branch in file order" gives the same
		// answer as walking the JSONL parentUuid chain to its first
		// non-worktree-agent ancestor — 190/190, no disagreements. The positional
		// latch is used instead of the causal chain because it is equivalent here,
		// needs no per-file uuid map, and survives incremental tail parses.
		//
		// ⚠️ Deliberately NOT the session-latched branch (s.GitBranch). That was
		// measured too and is WRONG: these sessions open on main and check out the
		// feature branch later, so s.GitBranch was "main" for all 68 of the 190
		// messages whose chain ancestor was a real issue branch. Falling back to it
		// would have resolved every message to something while recovering nothing.
		harnessBranch := IsHarnessWorktreeBranch(entry.GitBranch)
		if !harnessBranch {
			// Prefer the first non-HEAD, non-empty branch we see.
			if (s.GitBranch == "" || s.GitBranch == "HEAD") &&
				entry.GitBranch != "" && entry.GitBranch != "HEAD" {
				s.GitBranch = entry.GitBranch
			}
			if entry.GitBranch != "" && entry.GitBranch != "HEAD" {
				s.LastRealBranch = entry.GitBranch
			}
		}

		if entry.Type != "assistant" || entry.Message == nil || entry.Message.Usage == nil {
			continue
		}
		// Track the first non-empty model name we see; later entries should
		// match — model is stable across all entries sharing a message.id.
		if s.Model == "" && entry.Message.Model != "" {
			s.Model = entry.Message.Model
		}

		u := entry.Message.Usage
		// TTL split: if the nested cache_creation object is present (Anthropic
		// responses since the 1h-TTL feature shipped), it's authoritative.
		// If absent (legacy JSONL pre-dating the feature), bucket the rolled-up
		// number as 5m — that matches Anthropic's pre-feature default and is
		// the safest historical assumption per the user-decision matrix.
		var w5m, w1h int
		nested := u.CacheCreation != nil
		if nested {
			w5m = u.CacheCreation.Ephemeral5m
			w1h = u.CacheCreation.Ephemeral1h
		} else {
			w5m = u.CacheCreationInputTokens
		}
		// Clamp negative usage counts to zero at the JSONL wire boundary (#121).
		// A malformed/hostile session line carrying a negative count would
		// otherwise flow a negative cost_micro into the store and skew every SUM.
		// Clamp AFTER the TTL split so a negative nested cache field is caught,
		// and count once per event (not per field). No provider bills negative
		// tokens, so this is always a wire/parse defect — warn loudly.
		if ClampNegativeTokens(&u.InputTokens, &u.OutputTokens, &u.CacheReadInputTokens, &w5m, &w1h) {
			// RecordClamp fires per parsed assistant line (at the wire boundary,
			// before the message.id dedup below) — see its doc for the semantics.
			WarnClamp(SourceJSONL, entry.Message.Model)
			RecordClamp(SourceJSONL)
		}
		// Inherit the spawning session's branch for harness-invented worktree
		// names. If nothing has been latched yet (a tail parse whose window opens
		// on such a line, with no cached metadata) the name is left as-is and the
		// message buckets as before — degraded, never wrong.
		msgBranch := entry.GitBranch
		if harnessBranch && s.LastRealBranch != "" {
			msgBranch = s.LastRealBranch
		}
		cur := messageUsage{
			messageID:    entry.Message.ID,
			timestamp:    entry.Timestamp,
			gitBranch:    msgBranch,
			model:        entry.Message.Model,
			input:        u.InputTokens,
			output:       u.OutputTokens,
			cacheRead:    u.CacheReadInputTokens,
			cacheWrite5m: w5m,
			cacheWrite1h: w1h,
			nestedTTL:    nested,
		}

		key := entry.Message.ID
		if key == "" {
			// No id present — give this entry a unique slot so we don't
			// collapse multiple ID-less messages into one.
			fallbackSeq++
			key = fmt.Sprintf("__noid_%d", fallbackSeq)
		}
		// Keep the entry with the largest total — placeholders carry partial
		// counts; the final post-stream entry carries the full count. Ties
		// resolve later-wins (>=), matching the "definitive post-stream entry"
		// intuition. EXCEPT: a tie where the existing entry has nestedTTL
		// information and the incoming entry doesn't (legacy fallback)
		// preserves the existing — without this guard we'd silently downgrade
		// real TTL data into the all-5m fallback when both entries happen to
		// have identical totals (#55).
		//
		// parseOrder is captured on first-see and preserved across updates so
		// the ID-less fallback key (which embeds parseOrder) is stable across
		// re-scans of the same file regardless of which entry won the dedup.
		if existing, ok := messages[key]; !ok {
			cur.parseOrder = parseSeq
			parseSeq++
			messages[key] = cur
		} else if cur.total() > existing.total() ||
			(cur.total() == existing.total() && (cur.nestedTTL || !existing.nestedTTL)) {
			cur.parseOrder = existing.parseOrder
			messages[key] = cur
		}
	}
	if err := scanner.Err(); err != nil {
		// Most likely cause is bufio.ErrTooLong (a line exceeded maxJSONLLine).
		// Return what we managed to parse so far rather than discarding the
		// whole file — partial data beats none, and the warning surfaces the
		// underlying cause so the operator can investigate.
		slog.Warn("JSONL scan terminated early", "path", logsafe.Str(path), "err", err)
	}
	if skipped > 0 {
		// One aggregated event per file — easier to act on than N per-line
		// debug logs. Logged at warn because parsed-fewer-than-expected is
		// almost always a signal the operator wants to investigate.
		slog.Warn("skipped malformed JSONL lines", "path", logsafe.Str(path), "count", skipped)
	}

	// Carry the post-scan parseSeq forward so the watcher can persist it
	// into the cached metadata for the next debounce. Must be set BEFORE
	// the no-messages early-return so an incremental call that consumed
	// only ID-less placeholder bytes still advances the seq counter.
	s.NextParseSeq = parseSeq

	if len(messages) == 0 {
		// Only truly empty sessions (no assistant usage) are discarded. A
		// session with usage but no usable branch (empty gitBranch or a
		// detached-HEAD "HEAD") is KEPT and flows downstream: branchMatch
		// matches no commit ("" / "HEAD" never appear in commit Branches, which
		// parseBranches strips) and issueref.FromBranch("") is "", so the join
		// routes it to UnattributedIssueID rather than dropping its cost (#120,
		// spec section 4.4). Dropping it here was the #1 gaming vector: explore
		// on main / a detached HEAD, then branch only for the final generation.
		//
		// Even with no usable session, return the new offset (last complete
		// line) so the caller can advance and not re-read these bytes next
		// time. NextParseSeq on the discarded summary is lost — but if there
		// were no messages there were no ID-less keys either, so the counter is
		// unchanged.
		return nil, newOffset, nil
	}

	// Build the per-message slice in deterministic order. Sort by timestamp
	// first (matches the conversation order a user expects), then by
	// messageID as a stable tiebreaker for entries that share a timestamp
	// (e.g. ID-less messages whose timestamp resolution is per-second).
	s.Messages = make([]messageUsage, 0, len(messages))
	for _, m := range messages {
		s.Messages = append(s.Messages, m)
		s.InputTok += m.input
		s.OutputTok += m.output
		s.CacheRead += m.cacheRead
		s.CacheWrite5m += m.cacheWrite5m
		s.CacheWrite1h += m.cacheWrite1h
	}
	// SliceStable + parseOrder as the final tiebreaker means two ID-less
	// messages with identical (timestamp, messageID="") still sort
	// deterministically across re-scans — the slice order is fully determined
	// by parse-time observation order, which is itself deterministic for a
	// given file.
	sort.SliceStable(s.Messages, func(i, j int) bool {
		if !s.Messages[i].timestamp.Equal(s.Messages[j].timestamp) {
			return s.Messages[i].timestamp.Before(s.Messages[j].timestamp)
		}
		if s.Messages[i].messageID != s.Messages[j].messageID {
			return s.Messages[i].messageID < s.Messages[j].messageID
		}
		return s.Messages[i].parseOrder < s.Messages[j].parseOrder
	})
	return &s, newOffset, nil
}

// gitLog runs git log in repoPath and returns parsed commits since the given time.
func gitLog(ctx context.Context, repoPath string, since time.Time) ([]gitCommit, error) {
	// The trailing -07:00 offset is load-bearing: a zone-less --since string is
	// parsed by git in the HOST-LOCAL timezone, so on any non-UTC machine the
	// attribution window skews by the local UTC offset (issue #119). The instant
	// is computed in UTC; rendering it with an explicit numeric offset (yields
	// +00:00) keeps git's ISO8601 parser TZ-independent. Use -07:00, not Z07:00:
	// the numeric form is unambiguous across every git approxidate code path and
	// older git versions, whereas Z07:00 renders a bare "Z".
	sinceStr := since.UTC().Format("2006-01-02T15:04:05-07:00")
	// %H = hash, %ai = author date ISO8601, %s = subject, %D = ref names (decorate)
	cmd := exec.CommandContext(ctx, "git", "log",
		"--all",
		"--format=%H%x09%ai%x09%s%x09%D",
		"--since="+sinceStr,
	)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	var commits []gitCommit
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05 -0700", parts[1])
		if err != nil {
			continue
		}
		// Normalize to UTC before it flows further into the collector (#199).
		// git author dates are essentially always offset-bearing, and
		// modernc.org/sqlite compares DATETIME strings lexically: any commit
		// time that reaches storage in its source offset would window
		// incorrectly against the UTC query bounds fixed in #180. Canonicalizing
		// here keeps every collector-emitted time.Time in a single zone (UTC),
		// so instant comparisons and any downstream persistence stay correct.
		commit := gitCommit{
			Hash:      parts[0],
			Timestamp: t.UTC(),
			Subject:   parts[2],
			Branches:  parseBranches(parts[3]),
		}
		// Use issueref for consistent extraction across all data paths.
		for _, b := range commit.Branches {
			if id := issueref.FromBranch(b); id != "" {
				commit.IssueID = id
				break
			}
		}
		if commit.IssueID == "" {
			commit.IssueID = issueref.FromPRBody(commit.Subject)
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

// parseBranches extracts branch names from a git --decorate output fragment.
// Input example: "HEAD -> feature/42-auth, origin/feature/42-auth, tag: v1.0"
func parseBranches(decorateField string) []string {
	var branches []string
	for _, ref := range strings.Split(decorateField, ",") {
		ref = strings.TrimSpace(ref)
		// Strip "HEAD -> " prefix.
		if after, ok := strings.CutPrefix(ref, "HEAD -> "); ok {
			ref = after
		}
		// Skip tag refs and remote-tracking refs.
		if strings.HasPrefix(ref, "tag:") || strings.HasPrefix(ref, "origin/") {
			continue
		}
		if ref != "" && ref != "HEAD" {
			branches = append(branches, ref)
		}
	}
	return branches
}

// joinSessionsToCommits attributes each assistant message to a commit (or falls
// back to the branch name as the issue ID when no matching commit is found), then
// emits one TokenEvent per message.
//
// Per-message emission (closes #19) matches the proxy's granularity and
// produces cross-source-comparable IdempotencyKeys. A Claude call captured
// by both JSONL and the proxy emits the same MessageIdempotencyKey from
// either path and dedupes at the SQLite partial unique index.
//
// PER-MESSAGE ATTRIBUTION (#refocus, Option A): resolution is done for EACH
// message using that message's OWN branch (m.gitBranch), not one branch latched
// for the whole session. A session commonly opens on main before a feature
// checkout, so latching the first-seen branch routed the whole session's cost to
// UnattributedIssueID even after it moved onto feature/<N>. Resolving per message
// attributes each event by the branch it actually happened on, which roughly
// doubles measured attribution coverage. A message whose line carried no branch
// falls back to the session-latched s.GitBranch.
//
// Join key: the message's branch must match a commit branch AND the commit must
// fall within [msg.ts - 30min, msg.ts + 30min].
//
// BUG FIX (#refocus): a bare exploratory branch (main/master, a detached HEAD, or
// an empty branch — issueref.IsExploratoryBranch) must NOT inherit a windowed
// merge commit's `closes #N`. Real exploratory/planning cost on main that merely
// happened within ±30min of some issue's merge would otherwise be mis-attributed
// to that issue. A false attribution is worse than an honest bucket, so those
// branches skip commit matching entirely and route straight to their labeled
// unattributed bucket.
//
// Messages that resolve to no issue are NOT dropped: their cost is emitted under a
// labeled unattributed bucket (Option B — UnattributedMain / UnattributedDetachedHEAD
// / UnattributedNoIssue, all members of the UnattributedIssueID family) so it lands
// on the developer's denominator (spec section 4.4, #120) while naming WHY it did
// not attribute. Foreign-repo sessions are already removed upstream by
// filterSessionsByRepo, so the fallback cannot leak foreign-repo cost.
func joinSessionsToCommits(sessions []sessionSummary, commits []gitCommit, developer, repo string) []TokenEvent {
	var events []TokenEvent

	// Index commits by branch leaf name ONCE, so per-message resolution scans only
	// the commits on a matching branch instead of every commit. Resolution is now
	// per-message (not per-session), so without this the join would be
	// O(messages x commits); the index makes it O(messages x commits-on-this-branch).
	// The key is leafName(branch), which is exactly branchMatch's equality basis, so
	// this preserves branchMatch semantics; commit order within each bucket is the
	// original git-log order, preserving the first-windowed-match tie-break.
	commitsByLeaf := indexCommitsByLeaf(commits)

	for _, s := range sessions {
		for _, m := range s.Messages {
			// Per-message branch: the message's own branch, falling back to the
			// session-latched branch only when the line carried none.
			branch := m.gitBranch
			if branch == "" {
				branch = s.GitBranch
			}
			// The window is centered on the message's own timestamp (falling back
			// to session start for a timestamp-less message), so the ±30min match
			// tracks when THIS message happened.
			msgTS := m.timestamp
			if msgTS.IsZero() {
				msgTS = s.StartTime
			}
			issueID := resolveMessageIssue(branch, msgTS, commitsByLeaf)

			model := m.model
			if model == "" {
				// Defensive: fall back to the session-level model if the
				// per-message field is blank. Should not happen with current
				// Claude Code JSONL but older formats may have lacked it.
				model = s.Model
			}
			// Host is deliberately left empty (the store normalizes it to the
			// unknown sentinel): a session file records the CLIENT's view and
			// cannot tell which host served the call, so claiming one would
			// forge a host-qualified pricing key we cannot substantiate. The
			// empty host prices identically to the old model-only ComputeCost,
			// so this recovers the discarded billing_mode WITHOUT moving cost
			// (#525).
			cost, billingMode := store.ComputeCostHost("", model, store.CostUsage{
				Input:        m.input,
				Output:       m.output,
				CacheRead:    m.cacheRead,
				CacheWrite5m: m.cacheWrite5m,
				CacheWrite1h: m.cacheWrite1h,
			})
			// Reuse the message timestamp already resolved for the attribution
			// window (m.timestamp, or the session start fallback).
			ts := msgTS
			// Normalize the stored ts to UTC (#199). Message timestamps are
			// parsed from the session file and keep whatever offset the source
			// wrote (Claude Code writes Z today, but any tool may emit an
			// offset-bearing value); the StartTime fallback inherits the same.
			// modernc.org/sqlite compares DATETIME strings lexically, so an
			// offset-encoded ts would window incorrectly against the UTC query
			// bounds fixed in #180. .UTC() is a no-op for already-UTC and zero
			// times, so this is safe to apply unconditionally.
			ts = ts.UTC()

			// Idempotency key strategy (closes #19):
			//   - With message.id: MessageIdempotencyKey("anthropic", id) —
			//     same hash the proxy uses for the same upstream call.
			//   - Without message.id: source-prefixed (session id + the
			//     parse-time ordinal captured at first-see in
			//     parseSessionFile). Stable across re-scans because the
			//     file's contents — and thus the parse order — don't change.
			var key string
			if m.messageID != "" {
				key = MessageIdempotencyKey(string(ProviderAnthropic), m.messageID)
			} else {
				key = IdempotencyKey(SourceJSONL, s.SessionID, "noid", strconv.Itoa(m.parseOrder))
			}

			events = append(events, TokenEvent{
				Developer:      developer,
				IssueID:        issueID,
				Model:          model,
				InputTok:       m.input,
				OutputTok:      m.output,
				CacheRead:      m.cacheRead,
				CacheWrite5m:   m.cacheWrite5m,
				CacheWrite1h:   m.cacheWrite1h,
				CostMicro:      cost,
				Source:         SourceJSONL,
				Fidelity:       FidelityRealtime,
				IdempotencyKey: key,
				Repo:           repo,
				SessionID:      s.SessionID,
				BillingMode:    billingMode,
				Timestamp:      ts,
			})
		}
	}
	return events
}

// joinWindow is the ±tolerance around a message's timestamp within which a
// commit's branch match counts as attribution (#refocus, per-message analog of
// the former session-level [start-30, end+30] window).
const joinWindow = 30 * time.Minute

// indexCommitsBy leaf groups commits by the leaf name of each of their branches,
// the equality basis branchMatch uses (leafName(commitBranch) == leafName(session
// branch)). Building it once lets resolveMessageIssue scan only the commits on a
// message's branch. A commit with several branches is indexed under each leaf;
// commit order within a bucket follows the input (git-log) order, so the
// first-windowed-match semantics are preserved.
func indexCommitsByLeaf(commits []gitCommit) map[string][]gitCommit {
	idx := make(map[string][]gitCommit)
	for _, c := range commits {
		seen := map[string]bool{}
		for _, b := range c.Branches {
			leaf := leafName(b)
			// A commit whose branches share a leaf (rare) is added once per leaf,
			// not once per branch, so it can't be returned twice for one lookup.
			if seen[leaf] {
				continue
			}
			seen[leaf] = true
			idx[leaf] = append(idx[leaf], c)
		}
	}
	return idx
}

// resolveMessageIssue resolves ONE message's issue id from its branch and
// timestamp, returning either a real issue id or a labeled unattributed bucket
// (never the empty string). Order:
//
//  1. Commit match — ONLY for a branch that carries a feature identity. A commit
//     whose branch matches (same leaf name) and whose timestamp is within
//     ±joinWindow of the message contributes its issue. An exploratory branch
//     (main/master/HEAD/empty) is skipped here entirely: it must not inherit a
//     nearby merge's `closes #N` (the mis-attribution bug fix).
//  2. Branch-name derivation — issueref.FromBranch on a feature branch that names
//     its issue (feature/236-foo) even when no commit matched.
//  3. Labeled bucket — bucketForBranch classifies the still-unresolved branch into
//     main/detached-head/branch-without-issue so the cost is kept in the
//     denominator under an honest label rather than a silent single sentinel.
//
// commitsByLeaf is the leaf-name index from indexCommitsByLeaf.
//
// Attribution reflects commit state AT RESOLUTION TIME. On the stored path the
// resulting issue_id is pinned at first ingest (the store's ON CONFLICT clause
// updates only token counts, never issue_id), so a commit that lands AFTER a
// message was ingested does not retroactively re-attribute that stored row; a fresh
// parse (tierd score/doctor) always reflects the current commit set, which is why
// the two can differ if commits arrive between ingests.
func resolveMessageIssue(branch string, msgTS time.Time, commitsByLeaf map[string][]gitCommit) string {
	if !issueref.IsExploratoryBranch(branch) {
		lo := msgTS.Add(-joinWindow)
		hi := msgTS.Add(joinWindow)
		for _, c := range commitsByLeaf[leafName(branch)] {
			if c.Timestamp.Before(lo) || c.Timestamp.After(hi) {
				continue
			}
			if c.IssueID != "" {
				return c.IssueID
			}
		}
		if id := issueref.FromBranch(branch); id != "" {
			return id
		}
	}
	return bucketForBranch(branch)
}

// ProviderAnthropic is the provider tag JSONL uses when building cross-source
// MessageIdempotencyKeys. JSONL today is Claude-only (no JSONL format from
// OpenAI/Gemini exists); the constant lives here rather than in the proxy
// package to avoid an import cycle.
const ProviderAnthropic = "anthropic"

// branchMatch checks whether sessionBranch matches any of the commit's branches.
func branchMatch(sessionBranch string, commitBranches []string) bool {
	for _, b := range commitBranches {
		if b == sessionBranch {
			return true
		}
		if leafName(b) == leafName(sessionBranch) {
			return true
		}
	}
	return false
}

func leafName(branch string) string {
	parts := strings.Split(branch, "/")
	return parts[len(parts)-1]
}

// OSUsername is the exported form of osUsername, for collectors that live in
// their own package (internal/collector/codexrollout) and must resolve the
// developer identity through the SAME fallback chain. Two collectors disagreeing
// about who "the developer" is on one machine would split that machine's spend
// across two identities and silently halve both denominators.
func OSUsername() string { return osUsername() }

// osUsername returns the current OS user's login name. $USER/$LOGNAME first
// (cheap, honors su/sudo -E conventions), then os/user.Current() for
// environments that strip env vars but still resolve the UID (systemd units,
// cron, a container whose UID has an /etc/passwd entry), and only then the
// "unknown" sentinel. Under CGO_ENABLED=0 (this build's default via the pure-Go
// modernc.org/sqlite driver) user.Current() consults /etc/passwd directly, so
// an arbitrary-UID scratch/distroless container with no passwd entry still
// falls through to "unknown" -- which downstream now surfaces via the
// unjoined-identity WARN (#125) instead of silently pooling machines' spend
// under one fake identity. Setting $USER in such deployments is the fix.
func osUsername() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username // may be DOMAIN\name on Windows; stored verbatim
	}
	return "unknown"
}
