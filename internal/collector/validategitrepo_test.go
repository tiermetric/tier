package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidateGitRepo_AcceptsGitDir confirms a normal clone (.git is a
// directory) is accepted: the baseline that must not regress.
func TestValidateGitRepo_AcceptsGitDir(t *testing.T) {
	repo := t.TempDir()
	initGitRepoWithCommit(t, repo)

	if err := validateGitRepo(repo); err != nil {
		t.Fatalf("validateGitRepo on a normal clone: got %v, want nil", err)
	}
}

// TestValidateGitRepo_AcceptsWorktreeGitFile pins the #342 fix: a git worktree
// stores its .git as a regular FILE ("gitdir: ..." pointer), not a directory. The
// collector path must accept it so `tierd score`/`backfill`/`seam-exercise` can
// run inside a worktree, matching the serve/--watch-repo path (resolveWatchRepos)
// which already accepts it.
func TestValidateGitRepo_AcceptsWorktreeGitFile(t *testing.T) {
	repo := t.TempDir()
	initGitRepoWithCommit(t, repo)
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt"), "feature/342-wt")

	// Guard the precondition: the worktree's .git must be a file, not a dir,
	// otherwise this test would pass vacuously.
	info, err := os.Stat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("stat worktree .git: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("worktree .git is a directory; expected a gitdir pointer file")
	}

	if err := validateGitRepo(wt); err != nil {
		t.Fatalf("validateGitRepo on a worktree (.git file): got %v, want nil", err)
	}
}

// TestValidateGitRepo_RejectsNonRepo confirms the fix does not over-loosen: a
// directory with NO .git entry at all is still rejected. This is the sole case
// validateGitRepo guards against (running git log on a non-repo path).
//
// It also pins the actionable-guidance contract (#478, D3): the not-a-repo
// error must not dead-end the operator — it must name a remedy. We assert the
// `--repo` hint substring rather than the whole message so wording can evolve
// without breaking the test; the load-bearing part is that a first-run user who
// lands here is told how to proceed.
func TestValidateGitRepo_RejectsNonRepo(t *testing.T) {
	notARepo := t.TempDir() // t.TempDir gives a bare dir with no .git

	err := validateGitRepo(notARepo)
	if err == nil {
		t.Fatalf("validateGitRepo on a non-repo dir: got nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--repo") {
		t.Errorf("not-a-repo error must offer the --repo remedy; got %q", msg)
	}
	// The path must survive into the message so the operator can see WHICH path
	// was rejected — a bare "not a git repository" with no path is a worse dead
	// end than the one we are fixing.
	if !strings.Contains(msg, notARepo) {
		t.Errorf("not-a-repo error must name the rejected path %q; got %q", notARepo, msg)
	}
}

// TestCollect_RunsInWorktree is the end-to-end proof for #342: collectEvents
// (reached via Collect, and the same path `tierd score` uses) must not fail
// repo validation when RepoPath is a git worktree whose .git is a file. Before
// the fix this returned a "repo validation" error; after it, the session
// recorded in the worktree is collected normally.
func TestCollect_RunsInWorktree(t *testing.T) {
	repo := t.TempDir()
	initGitRepoWithCommit(t, repo)
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt"), "feature/342-collect")

	claudeDir := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-wt","gitBranch":"feature/342-collect","cwd":%q,"message":{"id":"msg_wt","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		wt,
	)
	writeJSONL(t, projectsDir, "wt.jsonl", []string{line})

	c := &JSONLCollector{
		RepoPath:    wt,
		ClaudeDir:   claudeDir,
		DeveloperID: "alice",
	}

	events, err := c.Collect(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect in a worktree: got %v, want nil (repo validation must accept a .git file)", err)
	}
	// Assert the session was actually collected, not merely that validation
	// passed: a future cwd-resolution regression in filterSessionsByRepo could
	// pass validation yet silently drop the worktree session, leaving zero
	// events. Pinning the count keeps this an end-to-end proof, not just a
	// no-error smoke test.
	if len(events) != 1 {
		t.Fatalf("Collect in a worktree: got %d events, want 1 (worktree session must be collected)", len(events))
	}
}
