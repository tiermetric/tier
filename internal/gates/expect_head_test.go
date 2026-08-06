package gates

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the #615 wrong-tree guard: scripts/expect-head.sh plus its Makefile
// wiring.
//
// Two mutation points, and both need a NAMED test standing over them:
//
//	(a) the check itself — delete the comparison in the script and every gate
//	    still exits 0, which is exactly the false green being fixed;
//	(b) the WIRING — leave the script perfect and drop `expect-head` from
//	    `check`'s prerequisites, and the guard is dead while looking alive. This
//	    repository has already been bitten by a control arm that was wired into
//	    no make target, so the wiring gets its own assertions below.
//
// Every script arm runs against a throwaway git repository with a COPY of the
// script at the same scripts/<name> offset, so the script's own
// `dirname "$0"/..` resolution points at the throwaway tree. That keeps the
// production script free of any test-only --repo override — a seam that would
// itself be a way to aim the guard at the wrong tree.

// repoRoot is the tier checkout this test file lives in, found without git so
// the "not a git repository" arm cannot depend on the thing it is testing.
//
// It walks up from the package directory to the go.mod, rather than using
// runtime.Caller: a developer with GOFLAGS=-trimpath exported in their SHELL
// (the Makefile's own GOFLAGS does not leak, but an env var does) gets a
// module-relative path from Caller, and every test in this file then fails with
// "scripts/expect-head.sh is missing … the guard has been deleted". A gate that
// accuses you of deleting the guard when you did not is a gate that gets deleted.
// `go test` runs each test binary with cwd set to its package source directory,
// which is stable under -trimpath.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.mod; cannot locate the repository")
		}
		dir = parent
	}
}

func scriptPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), "scripts", "expect-head.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("scripts/expect-head.sh is missing (%v) — the #615 wrong-tree guard has been deleted", err)
	}
	return p
}

// TestMain makes a git-less machine impossible to mistake for a passing gate.
//
// Without it, `git` being absent skips almost every test in this package and
// `go test ./...` prints a plain `ok` — the guard suite reporting success having
// verified nothing, which is this package's entire stated defect class. The skips
// themselves stay (a missing tool is environmental, as with golangci-lint and
// govulncheck in the Makefile), but they cannot be quiet.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintf(os.Stderr,
			"\n🔴 internal/gates: git is NOT on PATH (%v).\n"+
				"   Nearly every #615 wrong-tree guard test will SKIP, and this package's `ok` line\n"+
				"   will mean 'nothing was verified', not 'the guard works'. Install git and re-run.\n\n", err)
	}
	os.Exit(m.Run())
}

// requireGit skips only when the git BINARY is absent. That is the one soft skip
// this repository accepts (see the golangci-lint/govulncheck guards in the
// Makefile): a missing tool is environmental. It practically never fires here,
// since the Makefile's VERSION already shells out to git — and when it does,
// TestMain above has already made the situation loud.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH (%v); the expect-head guard cannot be exercised", err)
	}
}

// baseEnv is a hermetic environment for the child process: no inherited
// EXPECT_HEAD, and no inherited MAKEFLAGS (a test that shells out to `make` from
// inside `make check` would otherwise pick up the parent's jobserver).
func baseEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_AUTHOR_NAME=tier gates test",
		"GIT_AUTHOR_EMAIL=gates@example.invalid",
		"GIT_COMMITTER_NAME=tier gates test",
		"GIT_COMMITTER_EMAIL=gates@example.invalid",
		// HOME above neutralises ~/.gitconfig; these neutralise the SYSTEM and
		// global files too, so a developer's `git config --system` cannot change a
		// fixture's behaviour. Ignored by git < 2.32, which is harmless.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
}

// gitIn runs git in dir under the hermetic environment and returns trimmed
// output. Fatal on failure: these are fixture operations, and a fixture that
// half-built would make every arm below meaningless.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = baseEnv(dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tempRepo builds a throwaway repository with two commits and a copy of the
// guard at scripts/expect-head.sh. It returns the directory, the SHA of HEAD,
// and the SHA of the commit BEFORE it.
//
// The "before" commit is the whole point of the fixture: the 2026-08-04 incident
// left HEAD on a real, valid, already-merged commit, not on garbage. A guard
// tested only against nonsense revs would not prove it catches that.
func tempRepo(t *testing.T) (dir, head, prev string) {
	t.Helper()
	requireGit(t)
	dir = t.TempDir()

	// The messages carry `dir` so that two fixtures built in the same test — and
	// therefore in the same wall-clock second, with the same author, the same
	// empty tree and the same message — do not hash to the SAME commit. They did:
	// git is content-addressed, and identical content is identical SHAs. A test
	// asserting "the guard must not confirm the OTHER repo's HEAD" is vacuous when
	// both repos' HEADs are the same object.
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first commit (the one HEAD gets stuck on) "+dir)
	prev = gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "second commit (the one we meant to gate) "+dir)
	head = gitIn(t, dir, "rev-parse", "HEAD")

	if prev == head || prev == "" || head == "" {
		t.Fatalf("fixture did not produce two distinct commits: prev=%q head=%q", prev, head)
	}

	installGuard(t, dir)
	return dir, head, prev
}

// installGuard copies the production script into dir/scripts/, preserving the
// path offset the script uses to resolve its own repository.
func installGuard(t *testing.T, dir string) {
	t.Helper()
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatalf("read guard: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "expect-head.sh"), src, 0o755); err != nil {
		t.Fatalf("write guard: %v", err)
	}
}

// runGuard executes the copied guard in dir. expectHeadSet distinguishes "the
// variable is absent from the environment" from "the variable is present and
// empty" — the guard treats those differently on purpose, so the test harness
// must be able to produce both.
func runGuard(t *testing.T, dir string, expectHeadSet bool, expectHead string, extraEnv ...string) (output string, code int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(dir, "scripts", "expect-head.sh"))
	cmd.Dir = dir
	env := baseEnv(dir)
	if expectHeadSet {
		env = append(env, "EXPECT_HEAD="+expectHead)
	}
	cmd.Env = append(env, extraEnv...)

	return runCmd(t, cmd)
}

// runCmd returns combined output and the process exit code. A failure to START
// the process is fatal rather than "non-zero": these tests assert on exit codes,
// and a missing binary reported as a non-zero exit is exactly the rc=127
// false-kill that makes a mutation run worthless.
func runCmd(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("could not run %s: %v\n%s", cmd.Path, err, out)
	return "", -1
}

// --- the absent case: no friction for an ordinary local run --------------------

func TestExpectHeadGuard_AbsentEnvIsSilentNoOp(t *testing.T) {
	dir, _, _ := tempRepo(t)

	out, code := runGuard(t, dir, false, "")
	if code != 0 {
		t.Fatalf("EXPECT_HEAD unset must exit 0, got %d\n%s", code, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("EXPECT_HEAD unset must print NOTHING (a gate that nags gets switched off), got:\n%s", out)
	}
}

// --- the accept cases: short and full SHAs must agree --------------------------

func TestExpectHeadGuard_FullSHAAccepted(t *testing.T) {
	dir, head, _ := tempRepo(t)

	out, code := runGuard(t, dir, true, head)
	if code != 0 {
		t.Fatalf("full SHA %s matching HEAD must exit 0, got %d\n%s", head, code, out)
	}
	if !strings.Contains(out, head) {
		t.Fatalf("the OK line should name the resolved commit; got:\n%s", out)
	}
}

func TestExpectHeadGuard_ShortSHAAccepted(t *testing.T) {
	dir, head, _ := tempRepo(t)
	short := head[:7]

	out, code := runGuard(t, dir, true, short)
	if code != 0 {
		t.Fatalf("short SHA %s of HEAD %s must exit 0 — both sides resolve through rev-parse, "+
			"so a prefix mismatch here means the comparison went back to string matching. got %d\n%s",
			short, head, code, out)
	}
	// Not just rc=0: a guard reduced to `exit 0` would pass a bare code check.
	if !strings.Contains(out, head) {
		t.Fatalf("the OK line must name the resolved 40-char commit; got:\n%s", out)
	}
}

// --- THE CONTROL ARM: without this, "the check passes" is indistinguishable
// --- from "the check never ran" ------------------------------------------------

func TestExpectHeadGuard_WrongCommitRejected(t *testing.T) {
	dir, head, prev := tempRepo(t)

	// prev is a real, valid, reachable commit — the 2026-08-04 shape exactly.
	out, code := runGuard(t, dir, true, prev)
	if code == 0 {
		t.Fatalf("EXPECT_HEAD=%s while HEAD is %s MUST exit non-zero; got 0.\n"+
			"This is the #615 guard: a gate run on the wrong commit passes honestly and proves nothing.\n%s",
			prev, head, out)
	}
	if !strings.Contains(out, prev) || !strings.Contains(out, head) {
		t.Fatalf("the mismatch message must print BOTH the expected (%s) and the actual (%s) SHA "+
			"— the SHA is the only tell there is. got:\n%s", prev, head, out)
	}
}

func TestExpectHeadGuard_WrongShortSHARejected(t *testing.T) {
	dir, _, prev := tempRepo(t)

	out, code := runGuard(t, dir, true, prev[:7])
	if code == 0 {
		t.Fatalf("a wrong SHORT SHA must be rejected too, or the short-SHA path is a hole; got 0\n%s", out)
	}
}

// --- the cannot-verify cases: FAIL, never skip ---------------------------------

func TestExpectHeadGuard_UnknownCommitRejected(t *testing.T) {
	dir, _, _ := tempRepo(t)

	// A well-formed 40-hex object name no repository will contain. Being asked to
	// stand on a commit this tree has never heard of IS a wrong-tree symptom.
	const bogus = "0123456789abcdef0123456789abcdef01234567"
	out, code := runGuard(t, dir, true, bogus)
	if code == 0 {
		t.Fatalf("an unresolvable EXPECT_HEAD must exit non-zero, got 0\n%s", out)
	}
	if !strings.Contains(out, "does not resolve") {
		t.Fatalf("the unresolvable case needs its own diagnostic, not the mismatch one; got:\n%s", out)
	}

	// A too-short stub is rejected for the same reason (git refuses fewer than 4
	// characters, and anything short enough to be ambiguous fails --verify). This
	// is what stops a prefix-matching implementation from accepting a 2-character
	// "expectation" that matches almost any HEAD.
	if out, code := runGuard(t, dir, true, "0"); code == 0 {
		t.Fatalf("a 1-character EXPECT_HEAD must be rejected, got 0\n%s", out)
	}
}

func TestExpectHeadGuard_EmptyValueRejected(t *testing.T) {
	dir, _, _ := tempRepo(t)

	// `make check-full EXPECT_HEAD=$SHA` with $SHA unset in the caller's shell.
	// Silently treating this as "no assertion requested" would reinstate the exact
	// false green the guard exists to abolish.
	out, code := runGuard(t, dir, true, "")
	if code == 0 {
		t.Fatalf("EXPECT_HEAD set-but-empty must exit non-zero (it is an assertion that cannot fail), got 0\n%s", out)
	}
	if !strings.Contains(out, "EMPTY") {
		t.Fatalf("the empty case needs its own diagnostic; got:\n%s", out)
	}
}

func TestExpectHeadGuard_TautologicalRevRejected(t *testing.T) {
	dir, _, _ := tempRepo(t)

	// Same category as the empty value, reached by a different route: these
	// resolve to HEAD by definition, so the comparison is x != x and the guard can
	// only ever pass. A guard that cannot fail is the thing #615 is about.
	// Every one of these resolves to HEAD, and every one of them was measured
	// PASSING an earlier enumerated version of this check — which is why the
	// script uses a HEAD*/@* prefix match rather than a list.
	for _, rev := range []string{"HEAD", "@", "HEAD^0", "@^0", "HEAD~0", "HEAD^{}", "HEAD^{commit}", "@{0}", "HEAD@{0}"} {
		t.Run(rev, func(t *testing.T) {
			out, code := runGuard(t, dir, true, rev)
			if code == 0 {
				t.Fatalf("EXPECT_HEAD=%q is an assertion that cannot fail and must be rejected, got 0\n%s", rev, out)
			}
			if !strings.Contains(out, "anchored on HEAD") {
				t.Fatalf("the self-referential case needs its own diagnostic; got:\n%s", out)
			}
		})
	}
}

func TestExpectHeadGuard_BranchNameStillAccepted(t *testing.T) {
	dir, head, _ := tempRepo(t)

	// The counterweight to the test above: rejecting HEAD/@ must NOT collaterally
	// reject ref NAMES. `EXPECT_HEAD=main` is a real assertion — HEAD is at that
	// branch's tip — and is exactly what would have caught the aborted `git switch`
	// that motivated #615.
	branch := strings.TrimSpace(gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch == "" || branch == "HEAD" {
		t.Fatalf("fixture is on a detached HEAD; cannot exercise the branch-name path (%q)", branch)
	}
	if out, code := runGuard(t, dir, true, branch); code != 0 {
		t.Fatalf("EXPECT_HEAD=%q (the current branch, pointing at %s) must be accepted, got %d\n%s",
			branch, head, code, out)
	}
}

// TestExpectHeadGuard_GitDirEnvCannotRedirectTheGuard pins the sharpest hole this
// guard can have.
//
// `git -C <dir>` changes the working DIRECTORY; it does NOT override GIT_DIR /
// GIT_WORK_TREE from the environment. Without the `unset` at the top of the
// script, a caller whose environment names another repository gets a confirming
// "expect-head: OK" line about THAT repository while the tree actually under test
// is never read — rc=0, success-shaped, wrong tree. The #615 failure, committed by
// the #615 guard.
//
// Reachable, not theoretical: git exports GIT_DIR into every hook process, and a
// pre-push hook running `make check EXPECT_HEAD=$(git rev-parse HEAD)` is this
// guard's most obvious consumer.
func TestExpectHeadGuard_GitDirEnvCannotRedirectTheGuard(t *testing.T) {
	here, hereHead, _ := tempRepo(t)
	elsewhere, elsewhereHead, _ := tempRepo(t)

	if hereHead == elsewhereHead {
		t.Fatalf("the two fixture repos must have different HEADs, both are %s", hereHead)
	}

	// The guard lives in `here`. The environment points git at `elsewhere`, and
	// EXPECT_HEAD names `elsewhere`'s HEAD. A guard that reads its own tree must
	// reject this; a guard that honours GIT_DIR will happily confirm it.
	out, code := runGuard(t, here, true, elsewhereHead,
		"GIT_DIR="+filepath.Join(elsewhere, ".git"),
		"GIT_WORK_TREE="+elsewhere)
	if code == 0 {
		t.Fatalf("GIT_DIR redirected the guard at another repository and it reported OK — "+
			"it must read the tree it lives in (%s, HEAD %s), not %s (HEAD %s).\n%s",
			here, hereHead, elsewhere, elsewhereHead, out)
	}
	if !strings.Contains(out, here) {
		t.Fatalf("the diagnostic must name the tree the script actually lives in (%s); got:\n%s", here, out)
	}
}

// TestExpectHeadGuard_JudgesItsOwnTreeNotTheCwd pins the THIRD mutation point.
//
// The header of this file names two — the check and the wiring. There is a third:
// WHICH TREE the guard reads. Every other arm here invokes the guard with
// cmd.Dir equal to the directory the guard copy lives in, so "resolved from $0"
// and "resolved from $PWD" are indistinguishable, and `REPO="$PWD"` (or
// `git rev-parse --show-toplevel`, or an inherited GIT_DIR) passes the entire
// suite while letting the guard be aimed at a tree nobody named.
func TestExpectHeadGuard_JudgesItsOwnTreeNotTheCwd(t *testing.T) {
	here, hereHead, _ := tempRepo(t)
	elsewhere, elsewhereHead, _ := tempRepo(t)
	if hereHead == elsewhereHead {
		t.Fatalf("fixture collision: both repositories are at %s", hereHead)
	}

	// The guard lives in `here`, but is RUN from inside `elsewhere` and asked to
	// confirm `elsewhere`'s HEAD. A cwd-resolving guard prints OK — a green
	// verdict about a tree `make check` is not about to test.
	cmd := exec.Command(filepath.Join(here, "scripts", "expect-head.sh"))
	cmd.Dir = elsewhere
	cmd.Env = append(baseEnv(elsewhere), "EXPECT_HEAD="+elsewhereHead)
	out, code := runCmd(t, cmd)

	if code == 0 {
		t.Fatalf("the guard lives in %s (HEAD %s) but was run from %s (HEAD %s) and returned 0 —\n"+
			"it resolved its repository from the working directory, so it can be aimed at any tree.\n%s",
			here, hereHead, elsewhere, elsewhereHead, out)
	}
	if !strings.Contains(out, here) {
		t.Fatalf("the diagnostic must name the repository actually inspected (%s), not the cwd; got:\n%s", here, out)
	}
}

// TestExpectHeadGuard_WorksInsideALinkedWorktree is not an edge case here — it is
// the common case. Nearly every gate run in this repository happens under
// .claude/worktrees/*, where .git is a FILE holding a `gitdir:` pointer rather
// than a directory.
//
// `git rev-parse --git-dir` handles that shape; the obvious-looking
// `test -d "$REPO/.git"` does not, and written as `|| exit 0` it would become a
// silent skip in exactly the trees where the gate actually runs — which is the
// seam-exercise shape this guard's header explicitly refuses to repeat. Without
// this arm the two probes are indistinguishable: the fixture only ever builds
// ordinary repositories.
func TestExpectHeadGuard_WorksInsideALinkedWorktree(t *testing.T) {
	dir, head, prev := tempRepo(t)

	link := filepath.Join(t.TempDir(), "linked")
	add := exec.Command("git", "-C", dir, "worktree", "add", "-q", "-b", "gates-linked", link, head)
	add.Env = baseEnv(dir)
	if out, err := add.CombinedOutput(); err != nil {
		// Deliberately not a skip: skipping here restores the exact hole the test exists to close.
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(link, ".git")); err != nil || fi.IsDir() {
		t.Fatalf("fixture is not a linked worktree: .git must be a FILE (err=%v)", err)
	}
	installGuard(t, link)

	if out, code := runGuard(t, link, true, head); code != 0 {
		t.Fatalf("inside a linked worktree the guard must confirm HEAD (%s); got %d\n%s", head, code, out)
	}
	if out, code := runGuard(t, link, true, prev); code == 0 {
		t.Fatalf("inside a linked worktree the guard must still REJECT the wrong commit (%s); got 0\n%s", prev, out)
	}
}

// TestExpectHeadGuard_RunsUnderAppleBash32 exercises the DEPLOYMENT interpreter,
// not just the developer's.
//
// `#!/usr/bin/env bash` resolves through PATH, which on macOS finds Homebrew bash
// 5.x; /bin/bash is Apple's 3.2.57. This repository has already shipped a gate
// that was green under 5.x and died under 3.2 in cron (see the 🔴 note on
// dns-latch-selftest in the Makefile, which runs that script twice for the same
// reason). The script is 3.2-clean today, so this is a regression guard: it fails
// the moment someone modernises ${VAR+set} into [[ -v VAR ]], or reaches for a
// <<< here-string or ${var^^}.
func TestExpectHeadGuard_RunsUnderAppleBash32(t *testing.T) {
	const sysBash = "/bin/bash"
	if _, err := os.Stat(sysBash); err != nil {
		t.Skipf("%s is absent (%v); nothing to exercise", sysBash, err)
	}
	dir, head, prev := tempRepo(t)
	script := filepath.Join(dir, "scripts", "expect-head.sh")

	run := func(setEnv bool, val string) (string, int) {
		t.Helper()
		cmd := exec.Command(sysBash, script)
		cmd.Dir = dir
		env := baseEnv(dir)
		if setEnv {
			env = append(env, "EXPECT_HEAD="+val)
		}
		cmd.Env = env
		return runCmd(t, cmd)
	}

	if out, code := run(false, ""); code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("%s, EXPECT_HEAD unset: want silent exit 0, got %d\n%s", sysBash, code, out)
	}
	if out, code := run(true, head); code != 0 {
		t.Fatalf("%s, matching HEAD: want exit 0, got %d\n%s", sysBash, code, out)
	}
	if out, code := run(true, head[:7]); code != 0 {
		t.Fatalf("%s, matching short SHA: want exit 0, got %d\n%s", sysBash, code, out)
	}
	if out, code := run(true, prev); code == 0 {
		t.Fatalf("%s, wrong commit: want non-zero, got 0 — the guard is inert under the "+
			"deployment interpreter\n%s", sysBash, out)
	}
	if out, code := run(true, ""); code == 0 {
		t.Fatalf("%s, empty value: want non-zero, got 0\n%s", sysBash, out)
	}
}

func TestExpectHeadGuard_UnbornHeadRejected(t *testing.T) {
	requireGit(t)
	// A repository with zero commits: HEAD points at an unborn branch, so the
	// guard cannot read it. This is the one refusal branch with no arm over it —
	// rewritten as `|| exit 0` it becomes another silent skip, and the rest of the
	// suite stays green.
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	installGuard(t, dir)

	out, code := runGuard(t, dir, true, "0123456789abcdef0123456789abcdef01234567")
	if code == 0 {
		t.Fatalf("an unborn HEAD must FAIL, not exit 0 unverified; got 0\n%s", out)
	}
	if !strings.Contains(out, "cannot resolve HEAD") {
		t.Fatalf("the unresolvable-HEAD case needs its own diagnostic; got:\n%s", out)
	}
}

func TestExpectHeadGuard_NotAGitRepoRejected(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	installGuard(t, dir)

	// GIT_CEILING_DIRECTORIES stops git walking up out of the temp directory and
	// finding some unrelated enclosing repository, which would make this arm test
	// nothing.
	out, code := runGuard(t, dir, true, "0123456789abcdef0123456789abcdef01234567",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
	if code == 0 {
		t.Fatalf("a non-repository must FAIL, not skip: exiting 0 without reading HEAD is the very "+
			"false green this guard exists to catch (contrast seam-exercise, whose skip is correct for it). got 0\n%s", out)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Fatalf("the non-repository case needs its own diagnostic; got:\n%s", out)
	}
}

func TestExpectHeadGuard_GitMissingRejected(t *testing.T) {
	dir, head, _ := tempRepo(t)

	// A PATH containing bash and NOTHING else. Emptying PATH outright does not
	// test this: `#!/usr/bin/env bash` would fail to find its own interpreter and
	// the script would never run, which passes the assertion for the wrong reason.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on PATH (%v)", err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(bash, filepath.Join(binDir, "bash")); err != nil {
		t.Fatalf("symlink bash: %v", err)
	}
	if envBin, err := exec.LookPath("env"); err == nil {
		if err := os.Symlink(envBin, filepath.Join(binDir, "env")); err != nil {
			t.Fatalf("symlink env: %v", err)
		}
	}

	// Passing the CORRECT SHA makes the point sharp — the run would have succeeded
	// had git been reachable, so anything other than a hard failure here is a skip
	// dressed as a pass.
	cmd := exec.Command(filepath.Join(dir, "scripts", "expect-head.sh"))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + binDir, "HOME=" + dir, "EXPECT_HEAD=" + head}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git absent must FAIL, not exit 0 unverified; got success\n%s", out)
	}
	if !strings.Contains(string(out), "git is not on PATH") {
		t.Fatalf("the git-missing case needs its own diagnostic; got:\n%s", out)
	}
}

// --- the WIRING: a perfect guard reachable from no make target is worthless ----

// makefileTargets maps every target named in the Makefile to its prerequisites.
//
// It handles the two shapes this Makefile actually uses and that a naive parser
// silently drops on the floor — which would make the assertions above pass
// vacuously:
//
//   - MULTI-TARGET rules (`a b c: dep`), which is exactly how the guard edges are
//     written. A parser rejecting any left-hand side containing a space skips
//     that line entirely and sees no edges at all.
//   - BACKSLASH CONTINUATIONS, which that same rule spans. Reading line-by-line
//     would see a first line with no colon (skipped) and a second line naming
//     only the last three targets.
//
// Prerequisites are MERGED across rules rather than overwritten: make itself
// merges them, and `docs-html-check` genuinely has prerequisites declared in two
// places.
func makefileTargets(t *testing.T) map[string][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("open Makefile: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	targets := map[string][]string{}
	sc := bufio.NewScanner(f)
	var logical string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			logical = ""
			continue
		}
		if strings.HasSuffix(line, `\`) { // continuation: accumulate and read on
			logical += strings.TrimSuffix(line, `\`) + " "
			continue
		}
		logical += line
		line, logical = logical, ""

		lhs, rhs, ok := strings.Cut(line, ":")
		// `:=` is a variable assignment, not a rule.
		if !ok || strings.HasPrefix(rhs, "=") || strings.ContainsAny(lhs, "=") {
			continue
		}
		for _, name := range strings.Fields(lhs) {
			if strings.HasPrefix(name, ".") { // .PHONY, .NOTPARALLEL, …
				continue
			}
			targets[name] = append(targets[name], strings.Fields(rhs)...)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan Makefile: %v", err)
	}
	return targets
}

// TestExpectHeadGuard_MakeCheckStopsBeforeTheSuiteRuns is the only arm that
// asserts the ISSUE'S HEADLINE CONTRACT — "hard-fail before any test starts" —
// by executing it. The wiring test below parses prerequisite text;
// TestExpectHeadGuard_MakeTargetInvokesTheScript runs only `make expect-head` in
// isolation. Neither notices if the guard runs CONCURRENTLY with the suite it is
// supposed to precede, which is what happens under `-j` when the guard is an
// ordering convention rather than a dependency edge. Measured: with the edges
// removed, `make -j8 check EXPECT_HEAD=<bogus>` still exits non-zero — nothing
// looks broken — but `go vet` is echoed and the suite runs anyway.
func TestExpectHeadGuard_MakeCheckStopsBeforeTheSuiteRuns(t *testing.T) {
	requireGit(t)
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is not on PATH (%v)", err)
	}

	// A throwaway tree holding ONLY the real Makefile and the real guard. `make
	// check` is driven HERE and never in the checkout: these tests run INSIDE
	// `make check`, so driving it in the real root would recurse and would race
	// the parent run's docs-html regeneration. Nothing past the guard can succeed
	// in this tree, which is fine — the assertion is about WHEN the run dies.
	dir := t.TempDir()
	mk, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	installGuard(t, dir)
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "the only commit "+dir)

	runCheck := func(extra ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("make", append([]string{"check"}, extra...)...)
		cmd.Dir = dir
		cmd.Env = append(baseEnv(dir), "MAKEFLAGS=", "MAKELEVEL=", "MFLAGS=")
		return runCmd(t, cmd)
	}

	// `lint` is the first prerequisite after the guard and its recipe echoes
	// `go vet ./...` (no @ prefix). That echo is the witness that the suite began.
	const suiteStarted = "go vet"
	const bogus = "0123456789abcdef0123456789abcdef01234567"

	out, code := runCheck("EXPECT_HEAD=" + bogus)
	if code == 0 {
		t.Fatalf("`make check EXPECT_HEAD=%s` must fail\n%s", bogus, out)
	}
	if strings.Contains(out, suiteStarted) {
		t.Fatalf("a mismatched EXPECT_HEAD reached %q — the guard is wired in but not first in "+
			"PRACTICE, so a wrong-tree run still pays for the whole suite.\n%s", suiteStarted, out)
	}

	// Control arm. Without it, "the suite did not start" is indistinguishable from
	// "this temp tree could never have started the suite at all".
	if out, _ := runCheck(); !strings.Contains(out, suiteStarted) {
		t.Fatalf("control arm broken: with EXPECT_HEAD unset, `make check` must get PAST the guard and "+
			"reach %q; got:\n%s", suiteStarted, out)
	}

	// And under -j, which is the case list-ordering alone does not survive.
	if out, code := runCheck("-j8", "EXPECT_HEAD="+bogus); code == 0 || strings.Contains(out, suiteStarted) {
		t.Fatalf("`make -j8 check` with a mismatched EXPECT_HEAD must still die before %q — that is what "+
			"the `…: expect-head` dependency edges are for; code=%d\n%s", suiteStarted, code, out)
	}
}

func TestExpectHeadGuard_WiredFirstIntoMakeCheck(t *testing.T) {
	targets := makefileTargets(t)

	check, ok := targets["check"]
	if !ok {
		t.Fatal("no `check:` target in the Makefile")
	}

	// Every step `check` runs must ALSO depend on the guard as a real edge, not
	// merely sit after it in an unordered list — see the 🔴 block in the Makefile.
	for _, step := range check {
		if step == "expect-head" {
			continue
		}
		deps := targets[step]
		var guarded bool
		for _, d := range deps {
			if d == "expect-head" {
				guarded = true
			}
		}
		if !guarded {
			t.Errorf("`%s` is a prerequisite of `check` but does not depend on `expect-head` (deps: %v).\n"+
				"Without that edge it starts concurrently with the guard under `make -j`, and the guard's "+
				"whole point — costing one git rev-parse instead of the full suite — is lost.", step, deps)
		}
	}
	if len(check) == 0 || check[0] != "expect-head" {
		t.Fatalf("`expect-head` must be the FIRST prerequisite of `check`, got %v.\n"+
			"Failing after a five-minute suite has already run is nearly useless — the whole point "+
			"is that the mismatch costs one git rev-parse.", check)
	}

	full, ok := targets["check-full"]
	if !ok {
		t.Fatal("no `check-full:` target in the Makefile")
	}
	var reaches bool
	for _, p := range full {
		if p == "check" || p == "expect-head" {
			reaches = true
		}
	}
	if !reaches {
		t.Fatalf("`check-full` must reach the expect-head guard (directly or via `check`), got %v", full)
	}

	if _, ok := targets["expect-head"]; !ok {
		t.Fatal("no `expect-head:` target in the Makefile — the guard is unreachable from make")
	}
}

func TestExpectHeadGuard_MakeTargetInvokesTheScript(t *testing.T) {
	requireGit(t)
	root := repoRoot(t)
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is not on PATH (%v)", err)
	}
	// Both scripts/ and internal/ ship wholesale to the public mirror, so this
	// test runs for anyone who downloads the source ZIP rather than cloning — and
	// there is no HEAD to read there. Skip on the same environmental-precondition
	// grounds as requireGit; the temp-repo arms above still prove the guard.
	if err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Run(); err != nil {
		t.Skipf("%s is not a git checkout (%v) — source archive rather than a clone", root, err)
	}

	runMake := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("make", append([]string{"expect-head"}, args...)...)
		cmd.Dir = root
		// MAKEFLAGS is scrubbed: this test can itself run inside `make check`, and
		// inheriting the parent's jobserver descriptors makes the child noisy.
		cmd.Env = append(baseEnv(os.Getenv("HOME")), "MAKEFLAGS=", "MAKELEVEL=", "MFLAGS=")
		return runCmd(t, cmd)
	}

	// Control arm at the MAKE level, not just the script level: proves the recipe
	// actually runs the guard and propagates its exit code, rather than being a
	// target that exists and does nothing.
	const bogus = "0123456789abcdef0123456789abcdef01234567"
	if out, code := runMake("EXPECT_HEAD=" + bogus); code == 0 {
		t.Fatalf("`make expect-head EXPECT_HEAD=%s` must exit non-zero, got 0\n%s", bogus, out)
	}

	// Absent: unchanged behaviour, exit 0.
	if out, code := runMake(); code != 0 {
		t.Fatalf("`make expect-head` with EXPECT_HEAD unset must exit 0, got %d\n%s", code, out)
	}

	// And the matching case must pass, so the failure above is the guard firing on
	// a mismatch rather than the target being broken for every input.
	shaCmd := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	sha, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD in %s: %v", root, err)
	}
	head := strings.TrimSpace(string(sha))
	if out, code := runMake("EXPECT_HEAD=" + head); code != 0 {
		t.Fatalf("`make expect-head EXPECT_HEAD=%s` (the real HEAD) must exit 0, got %d\n%s", head, code, out)
	}
	if out, code := runMake("EXPECT_HEAD=" + head[:7]); code != 0 {
		t.Fatalf("`make expect-head EXPECT_HEAD=%s` (short form of the real HEAD) must exit 0, got %d\n%s",
			head[:7], code, out)
	}

	// The set-but-EMPTY distinction must survive the MAKE lane, not just a direct
	// env var: the recipe is a bare invocation and relies entirely on make
	// exporting command-line variables verbatim. That is a property of the make on
	// this machine, so it needs an executable pin rather than a comment.
	if out, code := runMake("EXPECT_HEAD="); code == 0 || !strings.Contains(out, "EMPTY") {
		t.Fatalf("`make expect-head EXPECT_HEAD=` must reach the script as SET-but-empty and fail; code=%d\n%s",
			code, out)
	}

	// A value containing a shell quote must reach the script VERBATIM and be
	// rejected as an unresolvable rev. If the recipe ever goes back to splicing
	// the value into a shell command line, this value both executes the injected
	// text and leaves EXPECT_HEAD unset in the script's environment — so the guard
	// silently exits 0 and a wrong tree sails through. Measured: that is exactly
	// what the earlier `EXPECT_HEAD='$(EXPECT_HEAD)'` recipe did.
	//
	// The probe is a FILESYSTEM side effect, not a string in the output: the
	// guard's own diagnostic quotes the offending value back at the reader, so a
	// marker string would appear in the output whether or not it was ever
	// executed. Only a file that exists proves execution.
	// The trailing `echo '` matters. With the spliced recipe this exact value was
	// measured producing rc=0 — injected text executed, guard never invoked, make
	// reporting success. A value ending `; '` instead leaves an empty command name
	// and dies rc=127, which the code!=0 assertion would pass for the WRONG reason.
	marker := filepath.Join(t.TempDir(), "injected")
	hostile := `a'; touch ` + marker + `; echo '`
	out, code := runMake("EXPECT_HEAD=" + hostile)
	if code == 0 {
		t.Fatalf("a shell-quote-bearing EXPECT_HEAD must be REJECTED, not silently treated as absent; got 0\n%s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("EXPECT_HEAD was spliced into a shell command line and EXECUTED (%s exists) — the recipe "+
			"must stay a BARE invocation so make delivers the value via the environment (see the 🔴 block "+
			"above `expect-head:` in the Makefile). got:\n%s", marker, out)
	}
	if !strings.Contains(out, hostile) {
		t.Fatalf("the hostile value must reach the script BYTE-FOR-BYTE (the diagnostic should quote it back); got:\n%s", out)
	}
}
