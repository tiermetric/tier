package main

// End-to-end tests for `tierd ship` (#126): JSONL fixture → runShip → real
// api.Handler + SQLite store over httptest → per-developer micro-dollar
// totals. Reuses the fixture helpers from score_test.go (same package).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/store"
)

// loadDeterministicPrices pins the ACTIVE price table for this test. The table
// is package-global (write-once-before-serve in production), so any test that
// asserts an exact priced cost sets the table it expects rather than trusting
// test order — self-defensive against any override another test may leave
// active. claude-sonnet-4 at RPT list price: $3/M input, $15/M output.
//
// Because the active table is package-global, this override would otherwise
// leak into later cmd/tierd tests under -shuffle. It registers the #172
// restoreEmbeddedPriceTable cleanup so the embedded default is reinstated when
// this test ends, closing the shuffle-order class symmetrically (#313).
func loadDeterministicPrices(t *testing.T) {
	t.Helper()
	restoreEmbeddedPriceTable(t)
	path := filepath.Join(t.TempDir(), "prices.yaml")
	yaml := "version: 99\neffective_date: \"2026-01-01\"\nmodels:\n" +
		"  claude-sonnet-4: {input_per_m: 3, output_per_m: 15, provider: anthropic}\n" +
		// The Codex fixture's model (#492). Mirrors its real prices.yaml row so
		// the shipped Codex cost is a deliberate figure rather than the
		// self-hosted-medium GUESS the unpriced-model fallback would apply.
		"  gpt-5.6-terra: {input_per_m: 2.50, output_per_m: 15, cache_read_mult: 0.10, provider: openai}\n" +
		"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write prices fixture: %v", err)
	}
	if _, err := store.LoadPriceTable(path); err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
}

// newShipTestServer builds the bearer-gated API composition runServe mounts,
// backed by a fresh on-disk SQLite store, and returns both.
func newShipTestServer(t *testing.T, token string) (*httptest.Server, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ship-test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.New(db, quiet, token, nil, "ship-test", api.RateLimitConfig{}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, db
}

// TestRunShip_EndToEnd drives the full laptop-shipper happy path in-process:
// JSONL fixture → collector → shipper client → POST /api/v1/events (bearer
// auth) → SQLite. Then re-runs the identical ship — the stateless
// over-shipping case — and asserts the micro-dollar totals are unchanged.
func TestRunShip_EndToEnd(t *testing.T) {
	const token = "ship-test-token-4242"
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, db := newShipTestServer(t, token)

	ship := func() string {
		return captureStdout(t, func() {
			runShip([]string{
				"--server", srv.URL,
				"--api-token", token,
				"--repo", repo,
				"--claude-dir", claudeDir,
				"--since", "2026-01-01",
				"--developer", "alice",
			})
		})
	}

	out := ship()
	if !strings.Contains(out, "Shipped 1 events") {
		t.Errorf("expected shipped-count summary; got:\n%s", out)
	}

	// Fixture: 1000 input + 500 output on claude-sonnet-4 = $0.0105 at RPT
	// list price → 10500 micro-dollars, realtime fidelity.
	assertCosts := func(when string) {
		t.Helper()
		costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("DeveloperCosts: %v", err)
		}
		if len(costs) != 1 {
			t.Fatalf("%s: %d developers, want 1: %v", when, len(costs), costs)
		}
		want := store.DeveloperCost{Developer: "alice", TotalCostMicro: 10500, RealtimeCostMicro: 10500}
		if costs[0] != want {
			t.Errorf("%s: costs = %+v, want %+v", when, costs[0], want)
		}
	}
	assertCosts("after first ship")

	// Over-shipping is safe: the identical re-run must be a server-side
	// no-op (idempotency keys collide on the partial unique index).
	ship()
	assertCosts("after re-ship")
}

// TestRunShip_NoSessions covers the empty-laptop path: with --allow-empty
// (#549 arm 3's escape hatch for the legitimately-idle case), zero sessions is
// still a success (exit 0 via normal return), with guidance output and zero
// posts. Without --allow-empty this same setup now exits non-zero — see
// TestShipSmoke_EveryRepoEmpty_ExitsNonZero (ship_exitcode_smoke_test.go,
// integration-tagged) for that arm, which needs a subprocess because runShip
// calls os.Exit directly and would kill this test binary in-process.
func TestRunShip_NoSessions(t *testing.T) {
	// #549 arm 4 depends on --codex-rollout's resolved value, which defaults
	// to envBool("TIER_CODEX_ROLLOUT") — our own docs tell operators to export
	// it. Without this guard the assertion below flips depending on the
	// developer's shell, not the code under test.
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	repo := initGitRepo(t)
	claudeDir := t.TempDir() // no projects/ content
	srv, db := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--allow-empty",
		})
	})
	if !strings.Contains(out, "No events shipped") {
		t.Errorf("missing no-sessions guidance; got:\n%s", out)
	}
	// #549 arm 2: the per-repo summary must show the zero row even on the
	// allow-empty happy path, not just on the failure path.
	if !strings.Contains(out, "sessions_with_events=0 events_shipped=0") {
		t.Errorf("missing the zero-row per-repo summary; got:\n%s", out)
	}
	// #549 arm 4: the Codex-omission note is documented as appearing on EVERY
	// completion line, success or empty alike — this run exercises the
	// "No events shipped" Printf specifically, which TestRunShip_EndToEnd's
	// and TestRunShip_CompletionLine_NamesCodexOmissionWhenOff's sibling
	// assertions never touch (they exercise the "Shipped N events" Printf).
	// Dropping the note from just this branch would still pass every other
	// #549 arm 4 test.
	if !strings.Contains(out, "Codex NOT included: pass --codex-rollout") {
		t.Errorf("no-sessions completion line does not name the Codex omission; got:\n%s", out)
	}
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 0 {
		t.Errorf("no events should land; got %v", costs)
	}
}

// TestAllReposEmpty is a direct unit test of the #549 arm 3 exit decision,
// split out as a pure function specifically so it is testable without
// invoking the os.Exit that runShip's actual non-zero exit goes through (see
// allReposEmpty's doc comment). Both directions are asserted: an all-zero
// summary set must read as empty, and a MIX with even one non-zero entry must
// not — the mixed case is the one a naive "any summary present" check would
// get wrong.
func TestAllReposEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []repoSummary
		want bool
	}{
		{"nil slice", nil, true},
		{"empty slice", []repoSummary{}, true},
		{"single zero", []repoSummary{{Path: "/a", SessionsWithEvents: 0}}, true},
		{"all zero, two repos", []repoSummary{{Path: "/a"}, {Path: "/b"}}, true},
		{"one non-zero among many", []repoSummary{{Path: "/a"}, {Path: "/b", SessionsWithEvents: 3}, {Path: "/c"}}, false},
		{"single non-zero", []repoSummary{{Path: "/a", SessionsWithEvents: 1}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allReposEmpty(c.in); got != c.want {
				t.Errorf("allReposEmpty(%+v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestRunShip_PerRepoSummary_ShowsZeroAndNonZeroRows is the #549 arm 2 test:
// with TWO --repo targets, one that matches the fixture session and one that
// matches nothing, the printed summary must show BOTH a non-zero row (proving
// the summary reports real counts, not just zeros) and a zero row (proving a
// dead target is visible without reading the log). Neither assertion alone
// would catch a summary that always prints one fixed value.
//
// Deliberately not all-empty (one repo matches), so this run reaches its
// normal exit-0 completion and needs no --allow-empty — arm 2 and arm 3 are
// independent, and this test isolates arm 2.
func TestRunShip_PerRepoSummary_ShowsZeroAndNonZeroRows(t *testing.T) {
	// See TestRunShip_NoSessions: --codex-rollout defaults from
	// TIER_CODEX_ROLLOUT, and this test's flag-parsing must not depend on the
	// developer's shell.
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	emptyRepo := initGitRepo(t) // valid git repo, but no session ever names it
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, _ := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--repo", emptyRepo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--developer", "alice",
		})
	})

	if !strings.Contains(out, "Per-repo summary:") {
		t.Fatalf("missing the per-repo summary header; got:\n%s", out)
	}
	if want := fmt.Sprintf("  %s: sessions_with_events=1 events_shipped=1", repo); !strings.Contains(out, want) {
		t.Errorf("summary missing the matching repo's non-zero row %q; got:\n%s", want, out)
	}
	if want := fmt.Sprintf("  %s: sessions_with_events=0 events_shipped=0", emptyRepo); !strings.Contains(out, want) {
		t.Errorf("summary missing the non-matching repo's zero row %q; got:\n%s", want, out)
	}
}

// TestRunShip_CompletionLine_NamesCodexOmissionWhenOff is the #549 arm 4
// positive arm: without --codex-rollout, the completion line must say so
// explicitly. This is what the 2026-07-30 dogfood backfill needed and did not
// have — "Shipped 132290 events" printed and exited 0 while the Codex path
// was never scanned, with nothing in the output naming the gap.
func TestRunShip_CompletionLine_NamesCodexOmissionWhenOff(t *testing.T) {
	// Reproduced: TIER_CODEX_ROLLOUT=1 go test ./cmd/tierd/ -run TestRunShip
	// fails without this — the flag defaults to envBool("TIER_CODEX_ROLLOUT")
	// and our docs tell operators to export it (see ship_codex_test.go's
	// TestRunShip_CodexRollout, which already carries this guard).
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, _ := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--developer", "alice",
			// deliberately no --codex-rollout
		})
	})
	if !strings.Contains(out, "Codex NOT included: pass --codex-rollout") {
		t.Errorf("completion line does not name the Codex omission; got:\n%s", out)
	}
}

// writeMultiEventSessionFixture writes ONE Claude Code session (one
// sessionId) that produced TWO billable assistant messages, under
// claudeDir/projects/. writeSessionFixture's single-event session makes
// SessionsWithEvents and EventsShipped numerically identical (1 and 1) on
// every test that uses it, which cannot distinguish repoTally actually
// deduping by session from a bug that reports the event count under both
// names — or a printRepoSummary that prints one field twice.
func writeMultiEventSessionFixture(t *testing.T, claudeDir, repo string) {
	t.Helper()
	projDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-multi","gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg_multi_1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-05-19T10:01:00Z","sessionId":"sess-multi","gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg_multi_2","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":200,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		repo, repo,
	)
	if err := os.WriteFile(filepath.Join(projDir, "s1.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// TestRunShip_PerRepoSummary_DistinguishesSessionsFromEvents is the YELLOW-2
// gap: every other #549 arm 2 test uses writeSessionFixture, whose one
// session yields exactly one event, so sessions_with_events and
// events_shipped are always equal and neither column is provably the right
// one. This fixture forces them apart — one session, two events — so a
// printRepoSummary that printed EventsShipped under both labels, or a
// repoTally.WithEvents that counted events instead of distinct sessions
// (defeating the dedup repoTally's own doc comment argues for), fails here
// even though every 1-and-1 test above would still pass it.
func TestRunShip_PerRepoSummary_DistinguishesSessionsFromEvents(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeMultiEventSessionFixture(t, claudeDir, repo)
	srv, _ := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--developer", "alice",
		})
	})

	if want := fmt.Sprintf("  %s: sessions_with_events=1 events_shipped=2", repo); !strings.Contains(out, want) {
		t.Errorf("summary row %q not found (one session, two events must NOT read as two sessions or one event); got:\n%s", want, out)
	}
}

// TestRunShip_DefaultRepoTargetsCWD covers the no-`--repo`-given path: repos
// defaults to []string{"."}, and it must resolve against the PROCESS cwd like
// any other relative --repo value, not silently no-op. t.Chdir (not
// os.Chdir) so Go restores the real working directory even if this test
// fails, and because this mutates process-global state it must never run
// with t.Parallel.
func TestRunShip_DefaultRepoTargetsCWD(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, db := newShipTestServer(t, "")

	t.Chdir(repo)
	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--developer", "alice",
			// deliberately no --repo: exercises the repos = {"."} default
		})
	})

	if !strings.Contains(out, "Shipped 1 events") {
		t.Errorf("default --repo (\".\") did not ship the fixture session; got:\n%s", out)
	}
	// resolveRepo resolves "." via os.Getwd/filepath.Abs, which may or may not
	// match the raw t.TempDir() string on a platform where TMPDIR sits behind
	// a symlink (macOS: /var -> /private/var) — Getwd's PWD-vs-syscall
	// resolution is platform- and even call-dependent. Accept either the raw
	// or the EvalSymlinks-resolved form, so this assertion is about the
	// default-repo behavior under test, not about which of the two equally
	// valid spellings of the same directory the platform happened to return.
	resolved := repo
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		resolved = r
	}
	wantRaw := fmt.Sprintf("  %s: sessions_with_events=1 events_shipped=1", repo)
	wantResolved := fmt.Sprintf("  %s: sessions_with_events=1 events_shipped=1", resolved)
	if !strings.Contains(out, wantRaw) && !strings.Contains(out, wantResolved) {
		t.Errorf("summary row for %q (raw) or %q (resolved) not found — the default \".\" did not resolve to the cwd repo; got:\n%s", wantRaw, wantResolved, out)
	}
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 || costs[0].Developer != "alice" {
		t.Errorf("costs after default-repo ship = %v, want one row for alice", costs)
	}
}
