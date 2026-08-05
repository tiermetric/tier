package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// TestParseSince covers the since-flag forms runScore accepts (#64 — the
// score command path had near-zero coverage).
func TestParseSince(t *testing.T) {
	cases := []struct {
		in      string
		want    string // formatted 2006-01-02; "" means "about 90 days ago"
		wantErr bool
	}{
		{"", "", false},
		{"2026-05-01", "2026-05-01", false},
		{"2026-05", "2026-05-01", false},
		{"2026", "2026-01-01", false},
		{"05-01-2026", "", true},
		{"yesterday", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSince(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseSince(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.want == "" {
				// Default window: ~90 days ago. Bound loosely so the test
				// doesn't flake across midnight.
				d := time.Since(got)
				if d < 89*24*time.Hour || d > 91*24*time.Hour {
					t.Errorf("default since = %v (%.1f days ago), want ~90 days", got, d.Hours()/24)
				}
				return
			}
			if got.Format("2006-01-02") != tc.want {
				t.Errorf("parseSince(%q) = %s, want %s", tc.in, got.Format("2006-01-02"), tc.want)
			}
		})
	}
}

func TestResolveRepo(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveRepo(dir)
	if err != nil {
		t.Fatalf("resolveRepo(%q): %v", dir, err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolveRepo returned non-absolute path %q", got)
	}
	if _, err := resolveRepo(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("resolveRepo on missing path: want error, got nil")
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. runScore prints its report via the fmt package directly, so
// this is the only way to observe it without restructuring main.
//
// It mutates the process-global os.Stdout, so callers must never run in
// parallel (no t.Parallel): concurrent writers would interleave or lose output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	type readResult struct {
		out string
		err error
	}
	// Buffered so the reader can always send and exit even if fn panics and
	// no one is left to receive — the goroutine never blocks or leaks.
	done := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(r)
		done <- readResult{string(b), err}
	}()
	// Deferred close restores stdout and unblocks the reader if fn panics;
	// double-Close on the happy path is a harmless no-op error.
	defer func() {
		_ = w.Close()
		os.Stdout = old
	}()
	fn()
	_ = w.Close()
	res := <-done
	if res.err != nil {
		t.Fatalf("read captured stdout: %v", res.err)
	}
	return res.out
}

// initGitRepo creates a git repo with one commit. Its only load-bearing job
// is satisfying the collector's validateGitRepo; the fixture session's
// branch (feature/42-foo) doesn't exist here, so issue resolution goes via
// the branch-name fallback, NOT the git-log commit join (still an untested
// path — noted in #64). Skips when git is absent.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@test", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "feat: scaffold (closes #42)"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

// writeSessionFixture writes one Claude Code JSONL session under
// claudeDir/projects/ whose CWD points at repo — the shape the collector
// scans for. Mirrors the watcher tests' fixture line.
func writeSessionFixture(t *testing.T, claudeDir, repo string) {
	t.Helper()
	writeSessionFixtureModel(t, claudeDir, repo, "claude-sonnet-4")
}

// writeSessionFixtureModel is writeSessionFixture with the model string as a
// parameter, so a test can drive `tierd score` over a session recorded against
// a subscription-billed route (#154).
func writeSessionFixtureModel(t *testing.T, claudeDir, repo, model string) {
	t.Helper()
	projDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-score","gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg_score_1","model":%q,"role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		repo, model,
	)
	if err := os.WriteFile(filepath.Join(projDir, "s1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// TestRunScore_EndToEnd drives the full `tierd score` happy path in-process:
// JSONL fixture → collector → aggregation → printed report (#64).
func TestRunScore_EndToEnd(t *testing.T) {
	// Pin the embedded default price table this assertion depends on rather than
	// trusting whatever a predecessor test left in the package-global (#313), so
	// the 0.0105 check below is order-independent. Pinning the real embedded table
	// (not a look-alike fixture) keeps this test a canary for embedded-price drift.
	loadEmbeddedPriceTable(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)

	out := captureStdout(t, func() {
		runScore([]string{
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--developer", "alice",
		})
	})

	for _, want := range []string{
		"TIER Cost Attribution — since 2026-01-01",
		"alice",
		"Cost by Issue",
		"issue-42",
		"tierd serve",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
	// 1000 input + 500 output on claude-sonnet-4 at RPT list price:
	// 1000×$3/M + 500×$15/M = $0.0105. An exact match proves the price
	// table was applied, not just that a row rendered.
	if !strings.Contains(out, "0.0105") {
		t.Errorf("report missing the RPT-priced cost 0.0105; got:\n%s", out)
	}
}

// TestRunScore_NoSessions covers the empty-claude-dir guidance path.
func TestRunScore_NoSessions(t *testing.T) {
	repo := initGitRepo(t)
	claudeDir := t.TempDir() // no projects/ content

	out := captureStdout(t, func() {
		runScore([]string{
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
		})
	})
	if !strings.Contains(out, "No Claude Code sessions found") {
		t.Errorf("missing no-sessions guidance; got:\n%s", out)
	}
}

// embeddedPricesPath is the same prices.yaml the store embeds, resolved through
// the public LoadPriceTable seam. go test runs with the working directory set to
// this package's source dir, so the store's source YAML resolves at a stable
// relative path.
const embeddedPricesPath = "../../internal/store/prices.yaml"

// restoreEmbeddedPriceTable reloads tierd's embedded default price table on
// cleanup, so a test that swaps the package-global active table via
// store.LoadPriceTable does not leak its override into later cmd/tierd tests
// (#172). The store's active table is a write-once-before-serve global with no
// public getter, so — mirroring the store package's own restoreDefaultPriceTable
// cleanup — we reload the known baseline; a bad path fails loudly rather than
// silently leaving the override active.
func restoreEmbeddedPriceTable(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := store.LoadPriceTable(embeddedPricesPath); err != nil {
			t.Fatalf("restore embedded price table from %s: %v", embeddedPricesPath, err)
		}
	})
}

// loadEmbeddedPriceTable pins the embedded default price table ACTIVE for the
// duration of the calling test and restores it on cleanup. A test asserting an
// exact default-table price uses this to be order-independent (it no longer
// trusts whatever a predecessor left in the package-global) while still failing
// loudly if the embedded default itself drifts — the pin is the real embedded
// table, not a fixture that merely happens to match it (#313).
func loadEmbeddedPriceTable(t *testing.T) {
	t.Helper()
	if _, err := store.LoadPriceTable(embeddedPricesPath); err != nil {
		t.Fatalf("pin embedded price table from %s: %v", embeddedPricesPath, err)
	}
	restoreEmbeddedPriceTable(t)
}

// TestLoadPricesOverride covers the --prices glue helper (#68): an empty path is
// a no-op that leaves the embedded default active; a valid path loads and reports
// applied=true. (The bad-path branch calls os.Exit and is covered by the manual
// smoke test + the store-level LoadPriceTable error tests.)
func TestLoadPricesOverride(t *testing.T) {
	// This test swaps the package-global active price table to a 3-model
	// self-hosted table; restore the embedded default on cleanup so it does not
	// leak into later tests that assert default-table prices (#172).
	restoreEmbeddedPriceTable(t)

	// Empty path → no override applied, no metadata.
	if info, ok := loadPricesOverride(""); ok || info != (store.PriceTableInfo{}) {
		t.Errorf("loadPricesOverride(\"\") = (%+v, %v), want (zero, false)", info, ok)
	}

	// Valid path → applied=true with the file's metadata.
	path := filepath.Join(t.TempDir(), "prices.yaml")
	yaml := "version: 4\neffective_date: \"2031-02-03\"\nmodels:\n" +
		"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, ok := loadPricesOverride(path)
	if !ok {
		t.Fatal("loadPricesOverride(valid) applied=false, want true")
	}
	if info.Version != 4 || info.EffectiveDate != "2031-02-03" || info.ModelCount != 3 {
		t.Errorf("info = %+v, want version=4 date=2031-02-03 models=3", info)
	}
}
