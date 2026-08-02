package collector

import (
	"path/filepath"
)

// RepoScopeResult classifies one recorded working directory against a target
// repository. It is the cross-repo bleed decision every session-file collector
// must make before attributing cost: a session that ran in a DIFFERENT repo on
// the same machine must never land on this repo's issues (#15).
type RepoScopeResult int

const (
	// RepoScopeForeign means the cwd is not at, inside, or a worktree of the
	// target repo. Its cost belongs to some other repo (or to nothing we can
	// name) and must not be attributed here.
	RepoScopeForeign RepoScopeResult = iota
	// RepoScopeDirect means the cwd is the target repo root or a subdirectory
	// of it — the ordinary checkout case.
	RepoScopeDirect
	// RepoScopeWorktree means the cwd sits OUTSIDE the target's path prefix but
	// resolves, via git's .git-file + commondir layout, to the target as its
	// main repository root. Counted separately so operators can see how much
	// capture arrives through the worktree fallback.
	RepoScopeWorktree
)

// RepoScope decides whether a recorded working directory belongs to one target
// repository, optionally memoizing the filesystem work so a scan over thousands
// of session files pays each resolution once.
//
// It is the single implementation of the "is this session in scope" rule, and as
// of #464 ALL THREE capture paths route through it:
//
//   - the Claude Code JSONL batch path — filterSessionsByRepo (jsonl.go),
//     memoizing, one scope per scan;
//   - the Claude Code LIVE watcher — matchTarget (watcher.go), non-memoizing,
//     one scope per target for the process lifetime;
//   - the Codex rollout-log collector — internal/collector/codexrollout,
//     memoizing, one scope per scan pass.
//
// Multi-target callers must use MatchScopes rather than looping Contains
// themselves, so the DIRECT-before-WORKTREE precedence is identical everywhere
// too. Keeping ONE implementation is the point: hand-rolled copies of a four-way
// symlink match plus a worktree fallback drift, and the failure mode of drift
// here is silent — one path attributes a session's dollars, the other drops them.
// (That drift was real: before #464 the live and batch paths were separate
// copies, and the collector's own first draft ordered its multi-target match
// differently from the watcher's.)
//
// Two matching stages, in order:
//
//  1. Path-prefix match — cwd is the target or a subdirectory of it (the common
//     case: an ordinary checkout).
//  2. Worktree fallback (only when the prefix match misses) — a session running
//     in a `git worktree` of the target has a cwd OUTSIDE the target's prefix
//     (the worktree lives under /tmp or a sibling dir), so stage 1 drops it and
//     its token cost vanishes. gitMainRepoRoot resolves the cwd's main
//     repository root from git's on-disk layout (no git exec) and re-runs the
//     prefix match against THAT. This maps a cwd to a repo root by identity; the
//     scope decision is unchanged, so a foreign repo's worktree still counts as
//     foreign. Residual limit: a worktree already REMOVED from disk cannot be
//     resolved (its .git file is gone) — those sessions stay foreign.
//
// Symlink handling: macOS commonly resolves /Users → /System/Volumes/Data/Users,
// and a developer may reach one repo by two paths (a symlink and its target). We
// compare both the cleaned (lexical) form and the EvalSymlinks-resolved form of
// each path, so a cwd recorded under either path still matches a target
// specified under either path. EvalSymlinks failure (cwd no longer on disk,
// permission denied) falls back to the cleaned lexical path — attempting a string
// match beats silently dropping every session.
//
// Case sensitivity caveat: on macOS APFS (default: case-insensitive,
// case-preserving) and on Windows NTFS, /Users/Alice and /Users/alice name the
// same directory but compare byte-unequal here. Same for NFC vs NFD Unicode
// normalisation. In practice the target and the recorded cwd come from the same
// shell on the same machine, so the byte forms match — but if a user reports
// "no sessions found" with a clearly-correct path, this is the first place to look.
//
// CONCURRENCY AND CACHE LIFETIME depend on which constructor built the scope:
//
//   - NewRepoScope memoizes, so Classify MUTATES the memo maps: NOT SAFE FOR
//     CONCURRENT USE, and its cached negatives go stale (a worktree created after
//     construction stays foreign). Build one per scan — the scan is short-lived,
//     which is exactly what makes both properties acceptable.
//   - NewLiveRepoScope does not memoize: Classify is read-only, so it IS safe for
//     concurrent use and never serves a stale answer. Use it for a scope that
//     outlives a single scan (the fsnotify watcher holds one per target for the
//     whole process and calls it from per-file debounce goroutines).
//
// Construction is cheap either way — one EvalSymlinks call.
type RepoScope struct {
	// targetAbs is the cleaned absolute target path; targetResolved is its
	// EvalSymlinks form (equal to targetAbs when resolution fails or is a no-op).
	targetAbs      string
	targetResolved string
	// targetGitRoot is the target's governing git repository root (gitMainRepoRoot
	// of targetAbs), with targetGitRootResolved its EvalSymlinks form. Used to
	// reject a cwd that is path-CONTAINED in the target but lives in its own
	// nested independent repository (#487): e.g. a model-bench/** benchmark run,
	// a separate git repo the parent .gitignores, whose spend was being charged to
	// the parent. Empty when the target is not inside any git repo, in which case
	// the git-root guard is skipped and the legacy containment verdict stands.
	targetGitRoot         string
	targetGitRootResolved string
	// symlinkCache memoizes EvalSymlinks per cleaned cwd — many sessions share
	// one cwd, and this collapses them to a single syscall. NIL in a live scope,
	// which disables memoization entirely (nil-map READS are legal in Go; only
	// writes panic, and the writes are guarded).
	symlinkCache map[string]string
	// worktreeCache memoizes the main-repo-root resolution per cleaned cwd,
	// INCLUDING negative results (root == ""), so a directory full of foreign
	// sessions does not re-walk the filesystem per file. Populated on BOTH the
	// stage-1 #487 nested-repo guard (the within==true / Direct path) and the
	// stage-2 worktree fallback — any Classify path that resolves a cwd's git
	// root. Nil in a live scope — a cached negative there would make a worktree
	// created after startup foreign FOREVER, silently dropping its capture.
	worktreeCache map[string]worktreeResolution
}

// NewRepoScope builds a MEMOIZING scope for repoPath, for one batch scan.
//
// repoPath is promoted to absolute first: filepath.Clean alone does not promote
// a relative input (e.g. ".", "../tier") to absolute, but recorded session cwds
// are always absolute, so without filepath.Abs every prefix comparison would miss
// and the scan would silently produce an empty report. If Abs fails (exceptional
// — callers stat the path first), the raw input is used: a slim chance of a false
// match beats losing every session.
func NewRepoScope(repoPath string) *RepoScope {
	s := newRepoScope(repoPath)
	s.symlinkCache = make(map[string]string)
	s.worktreeCache = make(map[string]worktreeResolution)
	return s
}

// NewLiveRepoScope builds a NON-memoizing scope for a long-lived caller: every
// Classify re-reads the filesystem, so the answer is never stale and concurrent
// callers do not race. It is what the live watcher uses, preserving the
// no-per-call-cache behaviour that path has always had — deliberately, because a
// `git worktree add` during a watcher's run must be captured on the next session,
// not after a restart.
func NewLiveRepoScope(repoPath string) *RepoScope { return newRepoScope(repoPath) }

// newRepoScope resolves the target path; the caller decides on memoization.
func newRepoScope(repoPath string) *RepoScope {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	s := &RepoScope{targetAbs: filepath.Clean(abs)}
	s.targetResolved = s.targetAbs
	if r, err := filepath.EvalSymlinks(s.targetAbs); err == nil {
		s.targetResolved = filepath.Clean(r)
	}
	// Resolve the target's git root once (the target is fixed for the scope's
	// lifetime). "" leaves the git-root guard disabled — see targetGitRoot.
	if root := gitMainRepoRoot(s.targetAbs); root != "" {
		s.targetGitRoot = root
		s.targetGitRootResolved = root
		if r, err := filepath.EvalSymlinks(root); err == nil {
			s.targetGitRootResolved = filepath.Clean(r)
		}
	}
	return s
}

// MatchScopes returns the index of the scope that owns cwd, or -1 when no scope
// does (including an empty cwd).
//
// TWO PHASES, and the order is the contract: EVERY scope's direct path-prefix
// match is tried before ANY scope's worktree fallback. A cwd that is a
// subdirectory of target B and simultaneously a git worktree of target A
// therefore belongs to B — the exact, on-disk containment wins over the
// resolved-through-git one. Looping Contains per target instead would return A,
// because it exhausts both stages of target A before looking at B.
//
// Every multi-target caller must go through this function; that is what keeps the
// live watcher and the Codex collector from answering differently for the same
// machine (#464 Y-G5). Within a phase, FIRST match wins: nested targets are a
// misconfiguration, and deciding deterministically beats deciding cleverly.
func MatchScopes(scopes []*RepoScope, cwd string) int {
	if cwd == "" {
		return -1
	}
	for i, s := range scopes {
		if s != nil && s.Classify(cwd) == RepoScopeDirect {
			return i
		}
	}
	for i, s := range scopes {
		// On a memoizing scope this second pass is nearly free: the symlink and
		// worktree resolutions were cached by the first pass.
		if s != nil && s.Classify(cwd) == RepoScopeWorktree {
			return i
		}
	}
	return -1
}

// Target returns the cleaned absolute target path this scope was built for.
// Exposed for log lines so operators can see WHICH path a scan compared against.
func (s *RepoScope) Target() string { return s.targetAbs }

// Classify reports how cwd relates to the target repo. An empty cwd is
// RepoScopeForeign: a session that recorded no working directory cannot be
// safely attributed anywhere. Callers that want to count "no cwd recorded"
// separately from "ran in another repo" must check for empty themselves before
// calling — the distinction matters for operator diagnostics, not for the scope
// decision, which is "not ours" either way.
func (s *RepoScope) Classify(cwd string) RepoScopeResult {
	if cwd == "" {
		return RepoScopeForeign
	}
	cwdAbs := filepath.Clean(cwd)
	// Nil-map read: a live (non-memoizing) scope always misses here and resolves
	// afresh, which is the point — see NewLiveRepoScope.
	cwdResolved, ok := s.symlinkCache[cwdAbs]
	if !ok {
		cwdResolved = cwdAbs
		if r, err := filepath.EvalSymlinks(cwdAbs); err == nil {
			cwdResolved = filepath.Clean(r)
		}
		if s.symlinkCache != nil {
			s.symlinkCache[cwdAbs] = cwdResolved
		}
	}
	if cwdWithinTarget(cwdAbs, cwdResolved, s.targetAbs, s.targetResolved) {
		// #487 guard: path containment is necessary but not sufficient. A cwd that
		// is inside the target's directory tree but whose OWN git root is a
		// distinct repository NESTED inside the target (model-bench/**, a vendored
		// clone, a scratch checkout) belongs to that repo, not the target —
		// charging its spend to the parent fuses two repos' costs.
		//
		// The nesting test is load-bearing, not decoration. A git WORKTREE of some
		// EXTERNAL repo A can physically live inside the target B (B/wt-of-a); its
		// git root resolves to A, which differs from B — but A lives elsewhere on
		// disk, so it is NOT nested inside B, and that case is a separate, already
		// decided disambiguation (a direct container beats another target's
		// worktree fallback). We must reject ONLY the truly-nested repo: roots
		// differ AND the cwd's root is itself contained in the target. Any
		// unresolved root leaves the containment verdict standing — the guard never
		// widens scope, only removes a proven false positive.
		if s.targetGitRoot != "" {
			if r := s.resolveWorktreeRoot(cwdAbs); r.root != "" &&
				!sameGitRoot(r.root, r.resolved, s.targetGitRoot, s.targetGitRootResolved) &&
				cwdWithinTarget(r.root, r.resolved, s.targetGitRoot, s.targetGitRootResolved) {
				return RepoScopeForeign
			}
		}
		return RepoScopeDirect
	}
	// Stage 2, off the common path: resolve a worktree cwd to its main repo root
	// and re-run the same four-way match against that.
	if res := s.resolveWorktreeRoot(cwdAbs); res.root != "" {
		if cwdWithinTarget(res.root, res.resolved, s.targetAbs, s.targetResolved) {
			return RepoScopeWorktree
		}
	}
	return RepoScopeForeign
}

// sameGitRoot reports whether two git-root paths name the same repository across
// either symlink form of each — the equality mirror of cwdWithinTarget's four-way
// containment check, so /var vs /private/var (macOS) or any symlinked checkout
// does not read as two different repos. All four arguments are already cleaned.
func sameGitRoot(aAbs, aResolved, bAbs, bResolved string) bool {
	return aAbs == bAbs || aResolved == bResolved ||
		aAbs == bResolved || aResolved == bAbs
}

// Contains is the boolean form of Classify for callers that do not need to
// distinguish a direct hit from a worktree hit.
func (s *RepoScope) Contains(cwd string) bool {
	return s.Classify(cwd) != RepoScopeForeign
}

// worktreeResolution is the cached result of resolving a cwd to its main repo
// root: root is the resolved main-repo path (or "" when the cwd is not a
// resolvable worktree), and resolved is root's EvalSymlinks form so the four-way
// symlink match never has to recompute it per session.
type worktreeResolution struct {
	root     string
	resolved string
}

// resolveWorktreeRoot memoizes gitMainRepoRoot (and the root's symlink
// resolution) by cleaned cwd. Negative results are cached too — but ONLY on a
// memoizing scope; a live scope re-resolves every time so a worktree created
// mid-run is picked up immediately.
func (s *RepoScope) resolveWorktreeRoot(cwdAbs string) worktreeResolution {
	if res, ok := s.worktreeCache[cwdAbs]; ok {
		return res
	}
	res := worktreeResolution{root: gitMainRepoRoot(cwdAbs)}
	if res.root != "" {
		res.resolved = res.root
		if r, err := filepath.EvalSymlinks(res.root); err == nil {
			res.resolved = filepath.Clean(r)
		}
	}
	if s.worktreeCache != nil {
		s.worktreeCache[cwdAbs] = res
	}
	return res
}
