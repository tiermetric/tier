package collector

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepoWithCommit turns dir into a git repository with one commit on the
// default branch. A commit is required so `git worktree add` succeeds (an
// empty-history repo has no HEAD to base a worktree on). user.email/user.name
// are set locally so the commit works on machines with no global git identity
// (minimal CI). Tests skip when git is not on PATH.
func initGitRepoWithCommit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "config", "user.email", "test@tier.local")
	runGit(t, dir, "config", "user.name", "tier test")
	// An empty commit gives the repo a HEAD without needing a tracked file.
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "initial")
}

// addWorktree creates a linked git worktree of mainRepo at wtPath on a new
// branch. Returns wtPath for call-site brevity.
func addWorktree(t *testing.T, mainRepo, wtPath, branch string) string {
	t.Helper()
	runGit(t, mainRepo, "worktree", "add", "--quiet", "-b", branch, wtPath)
	return wtPath
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// TestGitMainRepoRoot exercises the pure-file-read worktree resolver against
// every layout it must handle, plus the failure modes that must return "".
func TestGitMainRepoRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}

	t.Run("primary checkout root resolves to itself", func(t *testing.T) {
		repo := t.TempDir()
		initGitRepoWithCommit(t, repo)
		assertSameDir(t, gitMainRepoRoot(repo), repo)
	})

	t.Run("subdir of primary checkout resolves to root", func(t *testing.T) {
		repo := t.TempDir()
		initGitRepoWithCommit(t, repo)
		sub := filepath.Join(repo, "services", "foo")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir sub: %v", err)
		}
		assertSameDir(t, gitMainRepoRoot(sub), repo)
	})

	t.Run("linked worktree root resolves to main repo", func(t *testing.T) {
		repo := t.TempDir()
		initGitRepoWithCommit(t, repo)
		wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt"), "b1")
		assertSameDir(t, gitMainRepoRoot(wt), repo)
	})

	t.Run("subdir inside worktree resolves to main repo", func(t *testing.T) {
		repo := t.TempDir()
		initGitRepoWithCommit(t, repo)
		wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt"), "b2")
		sub := filepath.Join(wt, "nested", "dir")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir sub: %v", err)
		}
		assertSameDir(t, gitMainRepoRoot(sub), repo)
	})

	t.Run("non-repo dir resolves to empty", func(t *testing.T) {
		if got := gitMainRepoRoot(t.TempDir()); got != "" {
			t.Errorf("gitMainRepoRoot(non-repo) = %q, want \"\"", got)
		}
	})

	t.Run("worktree whose main repo was deleted resolves to empty without panic", func(t *testing.T) {
		mainParent := t.TempDir()
		repo := filepath.Join(mainParent, "main")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("mkdir repo: %v", err)
		}
		initGitRepoWithCommit(t, repo)
		wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt"), "b3")
		// Delete the main repo's .git entirely — the worktree's gitdir pointer
		// now dangles. Resolution must fail closed, not panic.
		if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
			t.Fatalf("rm main .git: %v", err)
		}
		if got := gitMainRepoRoot(wt); got != "" {
			t.Errorf("gitMainRepoRoot(dangling worktree) = %q, want \"\"", got)
		}
	})

	t.Run("hand-built .git file with relative gitdir resolves via ancestor", func(t *testing.T) {
		root := t.TempDir()
		// Main repo git dir: <root>/main/.git with a worktrees/w entry.
		mainGit := filepath.Join(root, "main", ".git")
		wDir := filepath.Join(mainGit, "worktrees", "w")
		if err := os.MkdirAll(wDir, 0o755); err != nil {
			t.Fatalf("mkdir wDir: %v", err)
		}
		// commondir points back at the main .git (../.. from worktrees/w).
		if err := os.WriteFile(filepath.Join(wDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
			t.Fatalf("write commondir: %v", err)
		}
		// The linked worktree: <root>/wt/.git is a FILE with a RELATIVE gitdir.
		wt := filepath.Join(root, "wt")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"),
			[]byte("gitdir: ../main/.git/worktrees/w\n"), 0o644); err != nil {
			t.Fatalf("write .git file: %v", err)
		}
		assertSameDir(t, gitMainRepoRoot(wt), filepath.Join(root, "main"))
	})
}

// assertSameDir fails unless got and want name the same directory. It compares
// EvalSymlinks forms so the assertion is stable whether git recorded a
// symlinked tempdir path (macOS /var → /private/var) resolved or not — the
// production match paths already compare both symlink forms, so equality
// modulo symlinks is exactly the contract gitMainRepoRoot must satisfy.
func assertSameDir(t *testing.T, got, want string) {
	t.Helper()
	if got == "" {
		t.Errorf("gitMainRepoRoot = \"\", want a path resolving to %q", want)
		return
	}
	rg := got
	if r, err := filepath.EvalSymlinks(got); err == nil {
		rg = r
	}
	rw := want
	if r, err := filepath.EvalSymlinks(want); err == nil {
		rw = r
	}
	if filepath.Clean(rg) != filepath.Clean(rw) {
		t.Errorf("gitMainRepoRoot = %q (resolved %q), want %q (resolved %q)", got, rg, want, rw)
	}
}
