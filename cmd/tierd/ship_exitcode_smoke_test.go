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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
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
	if want := fmt.Sprintf("  %s: sessions_with_events=0 events_shipped=0", logsafe.Str(repo)); !strings.Contains(stdout, want) {
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

func TestShipSmoke_GitLogFailureSkipsRepo(t *testing.T) {
	for _, order := range []string{"broken-first", "broken-last", "all-broken"} {
		t.Run(order, func(t *testing.T) {
			broken := initGitRepo(t)
			healthy := initGitRepo(t)
			claudeDir := t.TempDir()
			writeSessionFixture(t, claudeDir, broken)
			fixture := filepath.Join(claudeDir, "projects", "p1", "s1.jsonl")
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.ReplaceAll(string(data), "score", "broken"))
			if err := os.WriteFile(filepath.Join(filepath.Dir(fixture), "broken.jsonl"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			writeSessionFixture(t, claudeDir, healthy)
			if err := os.WriteFile(filepath.Join(broken, ".git", "refs", "heads", "dangling"), []byte(strings.Repeat("1", 40)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			srv, db := newShipTestServer(t, "")
			repos := []string{broken, healthy}
			wantEvents := 1
			switch order {
			case "broken-last":
				repos = []string{healthy, broken}
			case "all-broken":
				repos = []string{broken}
				wantEvents = 0
			}
			args := []string{"--server", srv.URL, "--claude-dir", claudeDir, "--since", "2026-01-01", "--allow-empty"}
			for _, repo := range repos {
				args = append(args, "--repo", repo)
			}
			args = append(args, "--repo-slug", healthy+"=test/healthy", "--repo-slug", broken+"=test/broken")
			code, stdout, stderr := runShipChild(t, args...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, broken) || !strings.Contains(stderr, "git log") || !strings.Contains(stderr, "exit status 128") {
				t.Errorf("stderr must identify the repo and git log failure; got:\n%s", stderr)
			}
			if want := fmt.Sprintf("  %s: sessions_with_events=0 events_shipped=0 failed=true\n", logsafe.Str(broken)); strings.Count(stdout, want) != 1 {
				t.Errorf("stdout must contain exactly one failed repo row %q; got:\n%s", want, stdout)
			}
			events, _, err := db.ListTokenEvents(t.Context(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != wantEvents {
				t.Fatalf("stored events = %d, want %d from healthy repo; stdout:\n%s\nstderr:\n%s", len(events), wantEvents, stdout, stderr)
			}
			for _, ev := range events {
				if ev.Repo != "test/healthy" {
					t.Errorf("shipped repo = %q, want test/healthy", ev.Repo)
				}
			}
		})
	}
}

func TestShipSmoke_AllGitLogsFail_OpencodeStillShips(t *testing.T) {
	repos := []string{initGitRepo(t), initGitRepo(t)}
	claudeDir := t.TempDir()
	for i, repo := range repos {
		writeSessionFixture(t, claudeDir, repo)
		projects := filepath.Join(claudeDir, "projects")
		if err := os.Rename(filepath.Join(projects, "p1"), filepath.Join(projects, fmt.Sprintf("repo-%d", i))); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", "dangling"), []byte(strings.Repeat("1", 40)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, repo := range repos {
		t.Run(fmt.Sprintf("repo-%d", i), func(t *testing.T) {
			dbPath := stageOpencodeDB(t, repo)
			srv, db := newShipTestServer(t, "")
			args := []string{
				"--server", srv.URL, "--claude-dir", claudeDir, "--since", "2026-01-01",
				"--opencode", "--opencode-db", dbPath,
			}
			for j, target := range repos {
				args = append(args, "--repo", target, "--repo-slug", fmt.Sprintf("%s=test/broken-%d", target, j))
			}
			code, stdout, stderr := runShipChild(t, args...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			for _, target := range repos {
				if want := fmt.Sprintf("  %s: sessions_with_events=0 events_shipped=0 failed=true\n", logsafe.Str(target)); strings.Count(stdout, want) != 1 {
					t.Errorf("stdout must contain exactly one failed repo row %q; got:\n%s", want, stdout)
				}
				if want := fmt.Sprintf("ship %q: %q; skipping repo", target, "git log: git log: exit status 128"); !strings.Contains(stderr, want) {
					t.Errorf("stderr must identify the repo's git log failure: %s; got:\n%s", target, stderr)
				}
			}
			if want := fmt.Sprintf("opencode (all repos): events_shipped=%d", opencodeFixtureEvents); !strings.Contains(stdout, want) {
				t.Errorf("stdout missing Opencode summary %q; got:\n%s", want, stdout)
			}
			events, _, err := db.ListTokenEvents(t.Context(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != opencodeFixtureEvents {
				t.Fatalf("stored events = %d, want %d from Opencode; stdout:\n%s\nstderr:\n%s", len(events), opencodeFixtureEvents, stdout, stderr)
			}
			for _, ev := range events {
				if ev.Source != "opencode" || ev.Repo != fmt.Sprintf("test/broken-%d", i) {
					t.Errorf("shipped source/repo = %q/%q, want opencode/test/broken-%d", ev.Source, ev.Repo, i)
				}
			}
		})
	}
}

func TestShipSmoke_CancellationStopsBeforeNextRepo(t *testing.T) {
	for _, signal := range []string{"INT", "TERM"} {
		t.Run(signal, func(t *testing.T) {
			repo := initGitRepo(t)
			nextRepo := initGitRepo(t)
			claudeDir := t.TempDir()
			writeSessionFixture(t, claudeDir, repo)
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = log ]; then\n" +
				"  kill -" + signal + " \"$PPID\"\n" +
				"  exec sleep 30\n" +
				"fi\n" +
				"exec \"$SHIP_TEST_REAL_GIT\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SHIP_TEST_REAL_GIT", git)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			srv, _ := newShipTestServer(t, "")
			code, stdout, stderr := runShipChild(t,
				"--server", srv.URL, "--repo", repo, "--repo", nextRepo,
				"--claude-dir", claudeDir, "--since", "2026-01-01", "--allow-empty",
			)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if want := fmt.Sprintf("ship %s: context canceled", repo); !strings.Contains(stderr, want) {
				t.Errorf("stderr must report cancellation for the first repo %q; got:\n%s", want, stderr)
			}
			if strings.Contains(stderr, nextRepo) || strings.Contains(stderr, "skipping repo") || stdout != "" {
				t.Errorf("canceled collection continued instead of exiting immediately; stdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
		})
	}
}
