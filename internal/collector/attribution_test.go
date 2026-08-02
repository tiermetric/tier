package collector

// Attribution-fallback coverage (issue #152 / task P4-05).
//
// When a Claude Code session cannot be attributed the clean way, TIER falls
// back through a chain — git-decorate parsing (parseBranches) → branch/leaf
// matching (branchMatch/leafName) → branch-name issue extraction → OS-username
// developer identity (osUsername). Cost lands (or silently vanishes) based on
// which link holds, so each link is characterized here against REAL and
// synthetic git output. Two current behaviors are deliberately PINNED, not
// fixed (non-origin remotes leak through parseBranches; a shared leaf name is a
// branchMatch false-positive) — changing them is a product decision for a
// follow-up issue, out of scope for this test-only task.

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// TestParseBranches pins the git `%D` decorate-field parser against realistic
// ref-name shapes. Each case notes the git situation that produces the input.
// parseBranches strips a leading "HEAD -> ", drops tag: refs, drops origin/
// remote-tracking refs, and drops bare HEAD/empty — everything else survives as
// a "branch". Two edges are characterization pins (see the flagged cases).
func TestParseBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		// undecorated commit — the overwhelmingly common `git log` line.
		{"empty", "", nil},
		// a commit that is the checked-out tip of a local branch.
		{"head_arrow", "HEAD -> feature/42-auth", []string{"feature/42-auth"}},
		// the doc-comment example: local tip + its remote-tracking ref + a tag.
		{"doc_comment_example", "HEAD -> feature/42-auth, origin/feature/42-auth, tag: v1.0", []string{"feature/42-auth"}},
		// a fully-decorated modern tip: local + tag + remote + remote HEAD.
		{"full_modern_decorate", "HEAD -> main, tag: v1.2.3, origin/main, origin/HEAD", []string{"main"}},
		// detached HEAD with no branch — attribution must find nothing.
		{"detached_head", "HEAD", nil},
		// detached HEAD sitting on a tag.
		{"detached_at_tag", "HEAD, tag: v1.0", nil},
		// an older commit decorated only by tags.
		{"tags_only", "tag: v1.0, tag: v1.1", nil},
		// a branch that exists only on the remote (deleted locally).
		{"remote_only", "origin/feature/9-x", nil},
		// two local branches pointing at one commit (e.g. just branched).
		{"two_local_branches", "main, hotfix/9-x", []string{"main", "hotfix/9-x"}},
		// git can emit padding around refs; trimming must be tolerant.
		{"whitespace_tolerance", " HEAD -> main ,  dev ", []string{"main", "dev"}},
		// CHARACTERIZATION HAZARD (pinned, not fixed): only the `origin/` prefix
		// is filtered, so a non-origin remote-tracking ref (upstream/, fork/, …)
		// leaks through and is treated as a local branch. Fixing this — filtering
		// all `<remote>/` refs — is a future product change; this pin makes the
		// current leak a conscious, test-visible decision rather than a surprise.
		{"characterization_non_origin_remote_leaks", "upstream/main", []string{"upstream/main"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseBranches(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseBranches(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestLeafName pins the last-`/`-segment extraction used by branchMatch. Empty
// input and a trailing slash both yield "" (a Split on "" is [""], and on
// "feature/" the final segment is "").
func TestLeafName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"feature/42-auth", "42-auth"},
		{"main", "main"},
		{"a/b/c", "c"},
		{"", ""},
		{"feature/", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := leafName(tc.in); got != tc.want {
				t.Errorf("leafName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBranchMatch pins the session-branch ↔ commit-branch matcher: an exact
// string match OR a shared leaf name (last path segment) counts as a match.
// Includes the pinned leaf false-positive characterization.
func TestBranchMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		sessionBranch  string
		commitBranches []string
		want           bool
	}{
		// exact string equality is the primary, highest-confidence match.
		{"exact_match", "feature/42-auth", []string{"feature/42-auth"}, true},
		// a bare-leaf session branch matches a fuller commit branch via leafName.
		{"leaf_match", "42-auth", []string{"feature/42-auth"}, true},
		// no shared name at all — no match.
		{"no_match", "feature/42-auth", []string{"main"}, false},
		// a commit with no branches (detached / tag-only decorate) matches nothing.
		{"empty_commit_branches", "feature/42-auth", nil, false},
		// CHARACTERIZATION HAZARD (pinned, not fixed): two DIFFERENT branches that
		// share a leaf segment ("feature/42-auth" vs "bugfix/42-auth") MATCH, so a
		// session's cost can attach to the wrong branch's commit. It has been
		// tolerable because a shared numeric leaf almost always means the same
		// issue number (42) — the attribution lands on the same issue anyway.
		// Tightening this (require exact match, or full-segment compare) is a
		// future product change; this pin makes the current imprecision explicit.
		{"characterization_leaf_false_positive", "feature/42-auth", []string{"bugfix/42-auth"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := branchMatch(tc.sessionBranch, tc.commitBranches); got != tc.want {
				t.Errorf("branchMatch(%q, %#v) = %v, want %v", tc.sessionBranch, tc.commitBranches, got, tc.want)
			}
		})
	}
}

// TestOSUsername pins today's developer-identity fallback chain:
// $USER → $LOGNAME → os/user.Current().Username → literal "unknown".
// The os/user.Current() link landed with P1-01 (#125, jsonl.go:1145); the
// terminal "unknown" branch is reached only when the running UID has no
// /etc/passwd entry (an arbitrary-UID distroless/scratch container), which this
// test environment cannot reproduce without a refactor — that residual branch
// is documented here, not pinned. Uses t.Setenv (auto-restores), so this test
// and its subtests must run sequentially (no t.Parallel).
func TestOSUsername(t *testing.T) {
	t.Run("USER_wins_over_LOGNAME", func(t *testing.T) {
		t.Setenv("USER", "alice")
		t.Setenv("LOGNAME", "bob")
		if got := osUsername(); got != "alice" {
			t.Errorf("osUsername() = %q, want %q (USER takes precedence)", got, "alice")
		}
	})
	t.Run("LOGNAME_used_when_USER_empty", func(t *testing.T) {
		t.Setenv("USER", "")
		t.Setenv("LOGNAME", "bob")
		if got := osUsername(); got != "bob" {
			t.Errorf("osUsername() = %q, want %q (LOGNAME is second in the chain)", got, "bob")
		}
	})
	t.Run("env_unset_falls_through_to_user_current", func(t *testing.T) {
		t.Setenv("USER", "")
		t.Setenv("LOGNAME", "")
		u, err := user.Current()
		if err != nil || u.Username == "" {
			// The literal-"unknown" terminal branch is reached only when the UID
			// has no /etc/passwd entry (arbitrary-UID distroless), which this test
			// env can't reproduce without a refactor — that residual branch is
			// documented, not pinned here.
			t.Skipf("user.Current() unavailable in this env (%v) — cannot pin the fall-through link", err)
		}
		if got := osUsername(); got != u.Username {
			t.Errorf("osUsername() = %q, want %q (both env vars empty → user.Current().Username)", got, u.Username)
		}
	})
}

var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// initGitRepoForLog builds a temp git repo with three commits on distinct
// branches so gitLog's happy parse loop sees real `git log --all` output:
//   - "base"                → subject "chore: init" (no issue)
//   - "feature/7-fallbacks" → subject "feat: thing (closes #99)" → issue-7 via branch segment
//   - "noissue"             → subject "fix: thing (closes #31)"  → issue-31 via subject fallback
//
// The feature commit's branch number (7) and subject number (99) DIFFER on
// purpose: it makes the "attributed via the branch-segment rule" claim
// self-proving. FromBranch runs before FromPRBody, so a correct implementation
// yields issue-7; a regression that lost the branch rule would fall through to
// the subject and yield issue-99, failing the assertion.
//
// Mirrors the -c user.email/-c user.name incantation used elsewhere
// (cmd/tierd/score_test.go). Skips (not fails) when git is absent, matching the
// house precedent (jsonl_test.go initEmptyGitRepo).
func initGitRepoForLog(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", repo,
			"-c", "user.email=t@test", "-c", "user.name=t",
			"-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	// base branch + its commit (explicit checkout -b so we never depend on the
	// host's init.defaultBranch name).
	run("checkout", "-q", "-b", "base")
	run("commit", "--allow-empty", "-q", "-m", "chore: init")
	// feature/7-fallbacks off base → issue-7 (branch-segment rule). The subject
	// deliberately closes a DIFFERENT number (#99) so the branch rule is the only
	// thing that can produce issue-7 — see the func doc.
	run("checkout", "-q", "-b", "feature/7-fallbacks", "base")
	run("commit", "--allow-empty", "-q", "-m", "feat: thing (closes #99)")
	// noissue off base → no numeric branch segment, so IssueID comes from the
	// subject via FromPRBody → issue-31.
	run("checkout", "-q", "-b", "noissue", "base")
	run("commit", "--allow-empty", "-q", "-m", "fix: thing (closes #31)")
	return repo
}

// TestGitLog exercises gitLog's dark happy path against REAL git output — the
// parse loop plus parseBranches fed by a real `%D` decorate field, which
// catches a git format drift a synthetic matrix cannot.
//
// Deliberately NOT asserted: the `--since` string format and any
// since-boundary precision (a far-past `since` is used) — those are P0-06's
// (#119) territory. The malformed-line skip branches (blank line, <4 fields,
// unparseable timestamp) are reachable only by injecting fake git output, which
// needs a production refactor to be testable without a real git binary — out of
// scope for this test-only task; that residual gap is intentionally unpinned.
func TestGitLog(t *testing.T) {
	t.Parallel()
	repo := initGitRepoForLog(t) // skips if git absent
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	commits, err := gitLog(context.Background(), repo, since)
	if err != nil {
		t.Fatalf("gitLog: %v", err)
	}
	// base-init + feature commit + noissue commit; `--all` lists each reachable
	// commit once. The unborn default branch (main/master) never materializes.
	if len(commits) != 3 {
		t.Fatalf("gitLog returned %d commits, want 3: %#v", len(commits), commits)
	}

	// Locate the two commits of interest by subject (hashes are non-deterministic).
	bySubject := func(subj string) *gitCommit {
		for i := range commits {
			if commits[i].Subject == subj {
				return &commits[i]
			}
		}
		return nil
	}

	branchCommit := bySubject("feat: thing (closes #99)")
	if branchCommit == nil {
		t.Fatalf("no commit with the feature subject in %#v", commits)
	}
	if !hex40.MatchString(branchCommit.Hash) {
		t.Errorf("branch commit Hash = %q, want 40-char lowercase hex", branchCommit.Hash)
	}
	if !containsStr(branchCommit.Branches, "feature/7-fallbacks") {
		t.Errorf("branch commit Branches = %#v, want to contain %q", branchCommit.Branches, "feature/7-fallbacks")
	}
	// issue-7 comes from the branch segment; the subject closes #99, so this
	// value can ONLY be produced by FromBranch winning over FromPRBody. A
	// regression that dropped the branch rule would yield issue-99 here.
	if branchCommit.IssueID != "issue-7" {
		t.Errorf("branch commit IssueID = %q, want %q (branch-segment rule must win over the subject's #99)", branchCommit.IssueID, "issue-7")
	}

	// Subject-fallback leg: branch carries no issue number, so IssueID must come
	// from the subject's close directive via FromPRBody.
	subjCommit := bySubject("fix: thing (closes #31)")
	if subjCommit == nil {
		t.Fatalf("no commit with the noissue subject in %#v", commits)
	}
	if subjCommit.IssueID != "issue-31" {
		t.Errorf("subject-fallback commit IssueID = %q, want %q (FromPRBody)", subjCommit.IssueID, "issue-31")
	}

	// Error path: a REAL directory that is not a git repo makes `git log` exit
	// non-zero ("fatal: not a git repository"); gitLog must surface the wrapped
	// error, not a nil/empty slice. Creating the dir (vs a non-existent path)
	// exercises git's own repo-detection failure rather than a fork/exec chdir
	// error — the production shape when repoPath exists but isn't a checkout.
	notRepo := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.Mkdir(notRepo, 0o755); err != nil {
		t.Fatalf("mkdir non-repo dir: %v", err)
	}
	if _, err := gitLog(context.Background(), notRepo, since); err == nil {
		t.Error("gitLog on a non-repo path returned nil error, want a wrapped 'git log' failure")
	} else if !regexp.MustCompile(`git log`).MatchString(err.Error()) {
		t.Errorf("gitLog error = %v, want it to wrap %q", err, "git log")
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
