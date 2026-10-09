package main

// `--muse` on serve and ship (#895). The fixture is the collector package's
// SYNTHETIC testdata session; nothing from a real Muse session is committed.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/muse"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
)

// museFixtureEvents is how many billed records the fixture holds: three
// model_completed and one automated_review_completed.
const museFixtureEvents = 4

// stageMuseHome writes the synthetic fixture session into a Muse home whose
// session ran in repo, and returns the home.
func stageMuseHome(t *testing.T, repo string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "collector", "muse", "testdata", "session.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "21", "ses-fixture-0001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(raw), "/WORKSPACE_ROOT", repo)
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestRunShip_Muse: --muse ships the session's events, /api/v1/events ACCEPTS
// them (a rejected source would 400 the batch), and they land with money, the
// carved token classes, and the run's branch as the issue.
func TestRunShip_Muse(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	t.Setenv("TIER_MUSE", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	home := stageMuseHome(t, repo)
	srv, db := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL, "--repo", repo, "--claude-dir", t.TempDir(),
			"--muse", "--muse-home", home,
			"--since", "2026-01-01", "--developer", "alice", "--allow-empty",
		})
	})

	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(rows) != museFixtureEvents {
		t.Fatalf("want %d stored rows, got %d; output:\n%s", museFixtureEvents, len(rows), out)
	}
	var in, read, outTok int
	var cost int64
	for _, r := range rows {
		if r.Source != collector.SourceMuse {
			t.Errorf("stored source = %q, want %q", r.Source, collector.SourceMuse)
		}
		if r.CostMicro <= 0 {
			t.Errorf("stored cost_micro = %d; Muse work would read as FREE", r.CostMicro)
		}
		if r.IssueID != "issue-895" {
			t.Errorf("stored issue = %q, want issue-895 from the run's recorded branch", r.IssueID)
		}
		in, read, outTok = in+r.InputTok, read+r.CacheRead, outTok+r.OutputTok
		cost += r.CostMicro
	}
	// Computed BY HAND at Meta's published muse-spark-1.3-contributor rate
	// ($0.10/M input, $0.20/M output, $0.002/M cached input) over the totals
	// below: 4400 + 250 + 112. The $0.50/M fallback, or a wrong row, fails here.
	if cost != 4762 {
		t.Errorf("stored cost_micro total = %d, want 4762 at the published Contributor rate", cost)
	}
	// The carved classes, asserted on the WIRE: the fixture's raw input_tokens
	// sum to 100000, of which 56000 are cached; output excludes nothing and adds
	// no reasoning.
	if in != 44000 || read != 56000 || outTok != 1250 {
		t.Errorf("stored totals input=%d cache_read=%d output=%d, want 44000/56000/1250", in, read, outTok)
	}
	if want := fmt.Sprintf("  muse (all repos): events_shipped=%d", museFixtureEvents); !strings.Contains(out, want) {
		t.Errorf("summary row %q missing; got:\n%s", want, out)
	}
	if strings.Contains(out, "Muse NOT included") {
		t.Errorf("the omission note appeared with --muse on; got:\n%s", out)
	}
}

// TestRunShip_CompletionLine_NamesMuseOmissionWhenOff: #549 arm 4 for Muse.
func TestRunShip_CompletionLine_NamesMuseOmissionWhenOff(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	t.Setenv("TIER_MUSE", "")
	repo := initGitRepo(t)
	srv, _ := newShipTestServer(t, "")
	out := captureStdout(t, func() {
		runShip([]string{"--server", srv.URL, "--repo", repo, "--claude-dir", t.TempDir(), "--since", "2026-01-01", "--allow-empty"})
	})
	if !strings.Contains(out, "Muse NOT included: pass --muse") {
		t.Errorf("completion line does not name the Muse omission; got:\n%s", out)
	}
	if strings.Contains(out, "muse (all repos)") {
		t.Errorf("the Muse row must not appear without --muse; got:\n%s", out)
	}
}

// TestMuseWatchCheck pins the serve fail-fast matrix for --muse.
func TestMuseWatchCheck(t *testing.T) {
	cases := []struct {
		name                string
		enabled, readOnly   bool
		repos               int
		wantWarn, wantFatal bool
	}{
		{name: "off", repos: 0},
		{name: "on_with_repos", enabled: true, repos: 1},
		{name: "on_no_repos_aborts", enabled: true, wantFatal: true},
		{name: "on_no_repos_readonly_warns", enabled: true, readOnly: true, wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, fatal := museWatchCheck(tc.enabled, tc.repos, tc.readOnly)
			if (warn != "") != tc.wantWarn || (fatal != "") != tc.wantFatal {
				t.Errorf("warn=%q fatal=%q, want warn=%v fatal=%v", warn, fatal, tc.wantWarn, tc.wantFatal)
			}
			if tc.wantFatal && !strings.Contains(fatal, "--watch-repo") {
				t.Errorf("the fatal must name the fix; got %q", fatal)
			}
		})
	}
}

// TestResolveMuseConfig: the flag alone enables with defaults, the block
// overrides the home and interval, and an invalid block fails startup.
func TestResolveMuseConfig(t *testing.T) {
	strp := func(s string) *string { return &s }
	if got, err := resolveMuseConfig(nil, false); got != nil || err != nil {
		t.Fatalf("no flag, no block: got (%v, %v), want disabled", got, err)
	}
	got, err := resolveMuseConfig(nil, true)
	if err != nil || got == nil || got.interval != muse.DefaultScanInterval || got.home != "" {
		t.Fatalf("flag alone: got (%+v, %v), want defaults", got, err)
	}
	got, err = resolveMuseConfig(&config.MuseConfig{Home: strp("  /custom/muse  "), ScanInterval: strp("90s")}, false)
	if err != nil || got.home != "/custom/muse" || got.interval != 90*time.Second {
		t.Fatalf("block overrides: got (%+v, %v)", got, err)
	}
	if _, err := resolveMuseConfig(&config.MuseConfig{ScanInterval: strp("banana")}, true); err == nil {
		t.Error("an unparseable scan_interval must fail startup")
	}
	if _, err := resolveMuseConfig(&config.MuseConfig{ScanInterval: strp("2s")}, true); err == nil || !strings.Contains(err.Error(), muse.MinScanInterval.String()) {
		t.Errorf("an interval below the floor must fail naming it; got %v", err)
	}
}
