//go:build integration

package main

// Subprocess-based exit-code tests for `tierd ship --repo` (#549 arm 3).
//
// runShip is one of this codebase's two legacy os.Exit subcommands (see
// allReposEmpty's doc comment in ship.go for why it is not refactored to
// `return int` here). Calling it in-process with a genuinely all-empty
// --repo set would invoke os.Exit(1) and kill THIS test binary — the exit
// code it produces is therefore only observable from OUTSIDE the process
// that runs it.
//
// These tests re-exec the test binary itself as `tierd`, reusing the exact
// self-re-exec mechanism serve_smoke_test.go's TestMain already provides
// (TIERD_SMOKE_CHILD_ARGS -> dispatch(...)) — that hook is generic over the
// subcommand, so no server-specific machinery is needed to drive `ship`
// through it. Gated behind the `integration` build tag like the rest of the
// smoke suite, so `make check`'s untagged run is unaffected and this only
// runs under `make check-full`.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runShipChild starts `tierd ship <args...>` as a child process, waits for it
// to exit, and returns its exit code (0 for success), captured stdout, and
// captured stderr. Unlike the serve smoke tests, `ship` is a one-shot batch
// command — it does not block waiting for a signal — so a plain cmd.Run()
// suffices; there is no "wait until live" phase to poll.
//
// t.Context() bounds the child to the test's lifetime: without it, a child
// that wedges (e.g. hangs waiting on stdin, or deadlocks) blocks Run()
// forever and the whole package pays the full 10-minute `go test` panic-timeout
// instead of failing this one test promptly.
//
// stdout is captured, not discarded: runShip's per-repo summary (#549 arm 2 —
// the entire deliverable of that arm) prints to stdout, and a test that only
// inspects stderr can never observe it.
func runShipChild(t *testing.T, args ...string) (exitCode int, stdout, stderr string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	childArgs := append([]string{"ship"}, args...)
	cmd := exec.CommandContext(t.Context(), self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join(childArgs, "\n"))
	outBuf := &lockedBuffer{}
	errBuf := &lockedBuffer{}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	if runErr == nil {
		return 0, outBuf.String(), errBuf.String()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode(), outBuf.String(), errBuf.String()
	}
	t.Fatalf("start/run tierd ship child: %v (stdout: %s, stderr: %s)", runErr, outBuf.String(), errBuf.String())
	return 0, "", "" // unreachable; t.Fatalf stops the goroutine
}

// TestShipSmoke_EveryRepoEmpty_ExitsNonZero is the #549 arm 3 regression: a
// --repo target that matches no sessions must make `ship` exit non-zero,
// reproducing the dogfood incident where a wrong --repo path logged kept=0
// and exited 0 anyway. This is the arm that PROVES the guard exists — see the
// control arm below for proof the mechanism can also pass.
func TestShipSmoke_EveryRepoEmpty_ExitsNonZero(t *testing.T) {
	repo := initGitRepo(t)
	claudeDir := t.TempDir() // no projects/ content: guaranteed zero sessions
	srv, _ := newShipTestServer(t, "")

	code, stdout, stderr := runShipChild(t,
		"--server", srv.URL,
		"--repo", repo,
		"--claude-dir", claudeDir,
		"--since", "2026-01-01",
		// deliberately no --allow-empty
	)
	// Exactly 1, not merely non-zero: the guard's own os.Exit(1) is the exit
	// code under test, and asserting only "!= 0" would still pass if that
	// call were mutated to os.Exit(3), or if the child had instead panicked
	// (exit code 2) or hit a flag-parse error (also 2) — none of which prove
	// the #549 arm 3 guard fired.
	if code != 1 {
		t.Fatalf("exit code = %d for an every-repo-empty run, want exactly 1 (this is the exact false green #549 exists to close); stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--allow-empty") {
		t.Errorf("stderr should name the escape hatch so an operator who genuinely expects zero knows what to pass; got:\n%s", stderr)
	}
	// #549 arm 2: the per-repo summary is arm 2's entire operator-facing
	// value, and it must survive the arm-3 exit-1 path — an operator staring
	// at a failed run needs to see WHICH repo(s) were empty without also
	// having to re-run with different flags. Printed to stdout, so this is
	// the one place in the suite that can see whether the print call sits
	// where the code comment says it does (after Flush, before the exit
	// checks) rather than being skipped on the failure branch.
	if want := fmt.Sprintf("  %s: sessions_with_events=0 events_shipped=0", repo); !strings.Contains(stdout, want) {
		t.Errorf("failing run's stdout missing the per-repo zero row %q — the summary must print even when the run exits non-zero; got:\n%s", want, stdout)
	}
}

// TestShipSmoke_MatchingRepoExitsZero is the CONTROL arm for the test above:
// with a real matching session, the identical child invocation (same server,
// same flags otherwise) must exit 0. Without this, a `ship` that ALWAYS
// exits non-zero would pass the empty-repo test above for the wrong reason.
func TestShipSmoke_MatchingRepoExitsZero(t *testing.T) {
	// No loadDeterministicPrices(t) here: that helper mutates the
	// package-global price table in THIS (parent) process, but the child
	// below is a separate process that loads its own embedded price table
	// from a fresh process image — the override never crosses the exec
	// boundary. Calling it here pins nothing and reads as if it does; cost is
	// deliberately not asserted in this test, only the exit code.
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, _ := newShipTestServer(t, "")

	code, stdout, stderr := runShipChild(t,
		"--server", srv.URL,
		"--repo", repo,
		"--claude-dir", claudeDir,
		"--since", "2026-01-01",
		"--developer", "alice",
	)
	if code != 0 {
		t.Fatalf("exit code = %d for a run with a matching session, want 0; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// TestShipSmoke_AllowEmptyExitsZero proves --allow-empty is a real escape
// hatch and not just documentation: the SAME every-repo-empty setup that
// exits non-zero above must exit 0 once the flag is added.
func TestShipSmoke_AllowEmptyExitsZero(t *testing.T) {
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	srv, _ := newShipTestServer(t, "")

	code, stdout, stderr := runShipChild(t,
		"--server", srv.URL,
		"--repo", repo,
		"--claude-dir", claudeDir,
		"--since", "2026-01-01",
		"--allow-empty",
	)
	if code != 0 {
		t.Fatalf("exit code = %d with --allow-empty set, want 0; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// scrubbedEnv drops every TIER_* variable from a parent environment before it
// is handed to a re-exec'd child.
//
// 🔴 WITHOUT THIS THE TEST READS THE DEVELOPER'S MACHINE. runShip consumes
// TIER_API_TOKEN, TIER_CODEX_ROLLOUT and TIER_LOG_LEVEL. Review measured both
// failure shapes on this very file: TIER_CODEX_ROLLOUT=yes makes the child die
// on flag parsing ("must be a boolean"), and the VALID value is worse —
// TIER_CODEX_ROLLOUT=1 enables Codex in the child with no --codex-sessions-dir
// override, so it walks the operator's real, machine-global ~/.codex/sessions
// and ships foreign spend into the test's temp DB. That is the same scope-leak
// class the codexrollout scope gate exists to prevent.
//
// PATH/HOME/TMPDIR are kept: the child needs to exec and to resolve temp dirs.
func scrubbedEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "TIER_") {
			continue
		}
		out = append(out, e)
	}
	return out
}
