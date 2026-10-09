package collector

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RepoScope became SHARED API in #464 — the Claude Code batch path, the live
// fsnotify watcher and the Codex rollout collector all decide "is this session in
// scope" through it. Before this file, the exported surface the Codex path
// actually calls (Contains, MatchScopes, NewLiveRepoScope) had no direct test at
// all: it was exercised only incidentally, through whole-collector tests that
// would attribute a change in scoping to some other cause.

// scopeTestRepo makes dir a real git repo (RepoScope's worktree stage reads git's
// on-disk layout, so a fake .git directory is not enough).
func scopeTestRepo(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v\n%s", dir, err, out)
	}
	return dir
}

// TestRepoScopeContains covers the boolean face of the rule — the method the
// Codex collector calls — including the empty-cwd case, which must be FOREIGN
// rather than a match-everything wildcard: a session that recorded no working
// directory cannot be safely attributed to any repo.
func TestRepoScopeContains(t *testing.T) {
	repo := scopeTestRepo(t, t.TempDir())
	other := scopeTestRepo(t, t.TempDir())
	sub := filepath.Join(repo, "internal", "collector")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	scope := NewRepoScope(repo)
	cases := []struct {
		name string
		cwd  string
		want bool
	}{
		{"repo root", repo, true},
		{"subdirectory", sub, true},
		{"trailing slash", repo + string(filepath.Separator), true},
		{"different repo", other, false},
		{"sibling with the repo path as a name PREFIX", repo + "-other", false},
		{"empty cwd", "", false},
		{"parent of the repo", filepath.Dir(repo), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scope.Contains(tc.cwd); got != tc.want {
				t.Errorf("Contains(%q) = %v, want %v", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestRepoScopeEmptyCWDIsForeignNotPanic pins the empty-cwd contract on the
// Classify face too: the collectors count "no cwd recorded" separately for
// operator diagnostics, so the distinction must be made by the CALLER and the
// scope must answer Foreign rather than guessing.
func TestRepoScopeEmptyCWDIsForeignNotPanic(t *testing.T) {
	scope := NewRepoScope(t.TempDir())
	if got := scope.Classify(""); got != RepoScopeForeign {
		t.Errorf("Classify(\"\") = %v, want RepoScopeForeign", got)
	}
	if scope.Contains("") {
		t.Error("Contains(\"\") = true — an unrecorded working directory must never match a repo")
	}
}

// TestMatchScopesPrefersDirectOverWorktree is the #464 Y-G5 regression.
//
// The live watcher matches every target's DIRECT prefix before trying ANY
// target's worktree fallback; the Codex collector's first draft exhausted both
// stages of target 0 before looking at target 1. For a cwd that is inside target
// B and also a git worktree of target A, the two orders disagree — and the repo
// a session's dollars land on is not a detail. MatchScopes is the one place that
// order now lives.
func TestMatchScopesPrefersDirectOverWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	repoA := scopeTestRepo(t, t.TempDir())
	// Give repo A a commit so `git worktree add` has something to check out.
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoA
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// repoB CONTAINS the worktree of repoA, so the same cwd is simultaneously
	// "inside repo B" and "a worktree of repo A".
	repoB := scopeTestRepo(t, t.TempDir())
	wt := filepath.Join(repoB, "wt-of-a")
	cmd := exec.Command("git", "worktree", "add", "--detach", wt)
	cmd.Dir = repoA
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable: %v\n%s", err, out)
	}

	scopeA, scopeB := NewRepoScope(repoA), NewRepoScope(repoB)

	// Sanity: the ambiguity this test depends on really exists.
	if scopeA.Classify(wt) != RepoScopeWorktree {
		t.Fatalf("setup: %s is not seen as a worktree of repo A (got %v)", wt, scopeA.Classify(wt))
	}
	if scopeB.Classify(wt) != RepoScopeDirect {
		t.Fatalf("setup: %s is not seen as inside repo B (got %v)", wt, scopeB.Classify(wt))
	}

	// A listed FIRST, so a per-target loop would return A on its worktree stage.
	if got := MatchScopes([]*RepoScope{scopeA, scopeB}, wt); got != 1 {
		t.Errorf("MatchScopes = %d (repo A), want 1 (repo B): a DIRECT containment must beat another target's worktree fallback, whatever the target order", got)
	}
	// Order-independence: reversing the targets must not change the answer.
	if got := MatchScopes([]*RepoScope{scopeB, scopeA}, wt); got != 0 {
		t.Errorf("MatchScopes (reversed) = %d, want 0 (repo B)", got)
	}
	// No match and an empty cwd both report -1.
	if got := MatchScopes([]*RepoScope{scopeA}, t.TempDir()); got != -1 {
		t.Errorf("MatchScopes(foreign cwd) = %d, want -1", got)
	}
	if got := MatchScopes([]*RepoScope{scopeA, scopeB}, ""); got != -1 {
		t.Errorf("MatchScopes(empty cwd) = %d, want -1", got)
	}
}

// TestLiveRepoScopeDoesNotCacheNegatives pins the memoization tradeoff #464 R4
// made explicit.
//
// The watcher holds one scope per target for the LIFE OF THE PROCESS. A cached
// negative there never expires, so a `git worktree add` during a run would leave
// that worktree's sessions foreign until a restart — capture silently lost, with
// nothing in the logs. A batch scope may cache precisely because it lives for one
// scan, and this test pins BOTH halves so a future "let's just memoize
// everything" change has to confront the difference.
func TestLiveRepoScopeDoesNotCacheNegatives(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	repo := scopeTestRepo(t, t.TempDir())
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// OUTSIDE the repo's path prefix, so only the worktree stage can match it —
	// which is the stage whose negatives are cached.
	wt := filepath.Join(t.TempDir(), "late-worktree")

	live := NewLiveRepoScope(repo)
	memo := NewRepoScope(repo)
	if live.Contains(wt) || memo.Contains(wt) {
		t.Fatal("setup: a path that does not exist yet must not be in scope")
	}

	cmd := exec.Command("git", "worktree", "add", "--detach", wt)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable: %v\n%s", err, out)
	}

	if !live.Contains(wt) {
		t.Error("a LIVE scope missed a worktree created after construction — the watcher would silently drop that worktree's capture until the process restarted")
	}
	if memo.Contains(wt) {
		t.Error("a MEMOIZING scope answered from fresh filesystem state; the cached negative is what makes it cheap, and losing that would be a per-file re-walk on every batch scan (if this is now desired, delete the live/memo split rather than blurring it)")
	}
}

// TestRepoScopeNestedRepoIsForeign is the #487 regression: a nested INDEPENDENT
// git repository (a benchmark run under model-bench/**, a vendored clone, a
// scratch checkout) is path-contained in the watched repo but is its own repo,
// so its spend must NOT be charged to the parent. Path containment said Direct;
// git-root identity says Foreign, which is correct.
func TestRepoScopeNestedRepoIsForeign(t *testing.T) {
	parent := scopeTestRepo(t, t.TempDir())

	// A normal subdirectory of the parent (same repo) stays in scope.
	sub := filepath.Join(parent, "internal", "collector")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	// A nested INDEPENDENT repo, exactly the model-bench/** shape: its own git
	// repo living inside the parent's tree.
	nested := filepath.Join(parent, "model-bench", "run-3")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	scopeTestRepo(t, nested)
	nestedSub := filepath.Join(nested, "cmd")
	if err := os.MkdirAll(nestedSub, 0o755); err != nil {
		t.Fatalf("mkdir nestedSub: %v", err)
	}

	scope := NewRepoScope(parent)
	cases := []struct {
		name string
		cwd  string
		want RepoScopeResult
	}{
		{"parent root is direct", parent, RepoScopeDirect},
		{"parent subdir is direct", sub, RepoScopeDirect},
		{"nested repo root is FOREIGN", nested, RepoScopeForeign},
		{"nested repo subdir is FOREIGN", nestedSub, RepoScopeForeign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scope.Classify(tc.cwd); got != tc.want {
				t.Errorf("Classify(%q) = %v, want %v", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestRepoScopeNonGitTargetFallsBackToContainment pins the fail-open contract:
// when the target is not inside any git repo, the git-root guard is disabled and
// the legacy path-containment verdict stands, so #487's fix never widens or
// narrows scope for a non-git target.
func TestRepoScopeNonGitTargetFallsBackToContainment(t *testing.T) {
	// A plain directory tree, no git anywhere.
	target := t.TempDir()
	sub := filepath.Join(target, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scope := NewRepoScope(target)
	if scope.targetGitRoot != "" {
		t.Fatalf("expected no target git root for a non-git dir, got %q", scope.targetGitRoot)
	}
	if got := scope.Classify(sub); got != RepoScopeDirect {
		t.Errorf("non-git target: Classify(sub) = %v, want Direct (containment fallback)", got)
	}
	if got := scope.Classify(filepath.Dir(target)); got != RepoScopeForeign {
		t.Errorf("non-git target: Classify(parent) = %v, want Foreign", got)
	}
}

// TestRepoScopeWorktreeOfExternalRepoInsideTargetStaysDirect pins the boundary of
// the #487 guard: a git WORKTREE of an EXTERNAL repo that physically lives inside
// the target must remain the target's Direct session, NOT rejected as foreign.
// Its git root resolves elsewhere on disk, so it is not a repo NESTED inside the
// target — the guard's nesting test must let it through. This is the sibling of
// TestMatchScopesPrefersDirectOverWorktree, asserting the single-scope verdict
// the #487 narrowing preserves.
func TestRepoScopeWorktreeOfExternalRepoInsideTargetStaysDirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	external := scopeTestRepo(t, t.TempDir())
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = external
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	target := scopeTestRepo(t, t.TempDir())
	wt := filepath.Join(target, "wt-of-external")
	cmd := exec.Command("git", "worktree", "add", "--detach", wt)
	cmd.Dir = external
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable: %v\n%s", err, out)
	}

	// The external repo's worktree lives inside target, but its git root is the
	// external repo (elsewhere), so it is NOT nested-in-target: Direct, not foreign.
	if got := NewRepoScope(target).Classify(wt); got != RepoScopeDirect {
		t.Errorf("Classify(worktree-of-external-inside-target) = %v, want Direct — the #487 guard must reject only repos NESTED inside the target, not a worktree pointing elsewhere", got)
	}
}

// TestRepoScopeSubmoduleStaysDirect pins the DECIDED behavior for a git
// submodule, which the #487 guard leaves Direct (charged to the parent) — today
// only incidentally, because gitMainRepoRoot returns "" for a submodule's
// .git/modules/<name> layout. A submodule genuinely is an independent repo, so
// the intended verdict must be explicit: if anyone later teaches gitroot to
// resolve that layout, this test forces a conscious decision rather than a silent
// attribution flip.
func TestRepoScopeSubmoduleStaysDirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	child := scopeTestRepo(t, t.TempDir())
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = child
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	parent := scopeTestRepo(t, t.TempDir())
	// -c protocol.file.allow=always: recent git blocks file:// submodules by
	// default; this is a local test fixture, not a network fetch.
	cmd := exec.Command("git", "-c", "protocol.file.allow=always",
		"submodule", "add", child, "vendored")
	cmd.Dir = parent
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git submodule add unavailable in this environment: %v\n%s", err, out)
	}

	sub := filepath.Join(parent, "vendored")
	if got := NewRepoScope(parent).Classify(sub); got != RepoScopeDirect {
		t.Errorf("Classify(submodule) = %v, want Direct — a submodule is charged to its parent (decided behavior; if this changes, decide it deliberately)", got)
	}
}

// TestRepoScopeOwnWorktreeNestedInsideItselfStaysDirect pins that the target's
// OWN worktree, placed inside its own tree, stays Direct — its git root resolves
// (via commondir) back to the target, so the guard's sameGitRoot clause is true
// and it is never rejected. Correct today; locked so a guard simplification can't
// regress it.
func TestRepoScopeOwnWorktreeNestedInsideItselfStaysDirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	repo := scopeTestRepo(t, t.TempDir())
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	wt := filepath.Join(repo, "self-wt")
	cmd := exec.Command("git", "worktree", "add", "--detach", wt)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable: %v\n%s", err, out)
	}
	if got := NewRepoScope(repo).Classify(wt); got != RepoScopeDirect {
		t.Errorf("Classify(own worktree nested in self) = %v, want Direct", got)
	}
}
