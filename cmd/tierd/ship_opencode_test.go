package main

// `tierd ship --opencode` (#719). The Opencode collector is a SHIPPABLE source
// (collector.ShippableSource), and #492 is the reason that matters: outcomes
// reach a central tierd by webhook whether or not the matching spend does, so a
// capture path that is shippable in principle but has no shipper in practice does
// not make Opencode invisible — it makes Opencode look FREE and inflates every
// developer's TIER. These tests pin the wire end to end.
//
// The fixture database is SYNTHETIC and built here. Nothing from a real Opencode
// store (which holds prompt text, OAuth tokens and API credentials) is committed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite"
)

// opencodeFixtureEvents is how many billable assistant messages
// stageOpencodeDB writes. Named so an assertion says what it is counting.
const opencodeFixtureEvents = 2

// stageOpencodeDB writes a synthetic Opencode SQLite store whose messages ran in
// repoPath, and returns its path. The schema mirrors Opencode's own (verified
// against a live store 2026-08-28); only the two tables the collector reads are
// created.
func stageOpencodeDB(t *testing.T, repoPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE `migration` (id TEXT PRIMARY KEY, time_completed INTEGER NOT NULL)"); err != nil {
		t.Fatalf("create migration: %v", err)
	}
	if _, err := db.Exec("INSERT INTO migration (id, time_completed) VALUES ('20260101_fixture', 1)"); err != nil {
		t.Fatalf("seed migration: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, " +
		"`time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL)"); err != nil {
		t.Fatalf("create message: %v", err)
	}
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC).UnixMilli()
	for i := 0; i < opencodeFixtureEvents; i++ {
		completed := base + int64(i)*1000
		blob := map[string]any{
			"role":       "assistant",
			"modelID":    "glm-5.3",
			"providerID": "zai-coding-plan",
			"cost":       0,
			"path":       map[string]any{"cwd": repoPath, "root": "/"},
			"time":       map[string]any{"created": completed - 500, "completed": completed},
			"tokens": map[string]any{
				"total": 1000 + 40 + 60 + 400, "input": 1000, "output": 40, "reasoning": 60,
				"cache": map[string]any{"read": 400, "write": 0},
			},
		}
		raw, err := json.Marshal(blob)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if _, err := db.Exec("INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)",
			fmt.Sprintf("msg_fixture_%d", i), "ses_fixture", completed-500, completed, string(raw)); err != nil {
			t.Fatalf("insert fixture message: %v", err)
		}
	}
	return path
}

// TestRunShip_Opencode is the end-to-end wire: --opencode ships the store's
// events, they are ACCEPTED by /api/v1/events (the #492 arm — a source the
// endpoint rejects would 400 the whole batch), and they land carrying real money.
func TestRunShip_Opencode(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	dbPath := stageOpencodeDB(t, repo)
	srv, db := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--opencode",
			"--opencode-db", dbPath,
			"--since", "2026-01-01",
			"--developer", "alice",
			// No Claude Code sessions in this fixture, so the run would otherwise
			// trip the #549 empty-repo exit even though Opencode spend DID land.
			"--allow-empty",
		})
	})

	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(rows) != opencodeFixtureEvents {
		t.Fatalf("want %d stored rows, got %d; output:\n%s", opencodeFixtureEvents, len(rows), out)
	}
	for _, r := range rows {
		if r.Source != collector.SourceOpencode {
			t.Errorf("stored row source = %q, want %q", r.Source, collector.SourceOpencode)
		}
		// #492's real failure was rows landing with NO money. A row that arrives
		// token-bearing but free is the same defect wearing a different hat.
		if r.CostMicro <= 0 {
			t.Errorf("stored row has cost_micro = %d with %d input tokens; Opencode work would read as FREE",
				r.CostMicro, r.InputTok)
		}
		// The mapping this whole collector exists for, asserted on the WIRE and
		// not only inside the collector's own package: 40 output + 60 reasoning.
		if r.OutputTok != 100 {
			t.Errorf("stored output_tok = %d, want 100 (output 40 + reasoning 60). 40 would mean reasoning was dropped somewhere on the ship path", r.OutputTok)
		}
	}
}

// TestRunShip_Opencode_StoresTheAuditedRateNotTheGuess is the guard for the
// defect that made `ShippableSource(opencode) = true` a liability rather than a
// fix.
//
// 🔴 WHAT WAS WRONG. The server re-prices every shipped event from its raw token
// counts (#233) and can only price at the host the client sends. `wireEvent`
// carried no host, and `/events` hardcoded `ComputeCostHost("", model, …)` — sound
// for jsonl and codex-rollout, which genuinely have no host, and false for
// Opencode, whose rates then lived ONLY under host-qualified rows (#712). So every
// shipped Opencode event landed at the guessed self-hosted-medium fallback
// ($0.50/M), and the divergence detector could not see it: the shipper, priced
// with the same table, produced the same guess.
//
// Since #786 the built-in model-only glm-5.3 row prices glm-5.3 on any host, so
// the cost arm pins "the audited rate, not the guess" and the HOST arm is what
// pins the host crossing the wire.
func TestRunShip_Opencode_StoresTheAuditedRateNotTheGuess(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	t.Setenv("TIER_PRICES", "")
	loadEmbeddedPriceTable(t)

	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	dbPath := stageOpencodeDB(t, repo)
	srv, db := newShipTestServer(t, "")

	// BOTH sides price with the built-in table: its glm-5.3 row (provider zai,
	// per_token, #786) is the audited rate.
	usage := store.CostUsage{Input: 1000, Output: 100, CacheRead: 400} // 40 output + 60 reasoning
	wantMicro, wantMode := store.ComputeCostHost("zai-coding-plan", "glm-5.3", usage)
	if !store.IsAuditedRate("zai-coding-plan", "glm-5.3") {
		t.Fatal("the built-in table has no audited glm-5.3 rate; this test's premise is gone")
	}
	// The wrong answer, so a failure names which one landed.
	guessMicro := store.ComputeCost("self-hosted-medium", usage)
	if wantMicro == guessMicro {
		t.Fatalf("the audited and guessed prices are identical (%d micro); this fixture cannot tell them apart", wantMicro)
	}

	captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL, "--repo", repo, "--claude-dir", claudeDir,
			"--opencode", "--opencode-db", dbPath,
			"--since", "2026-01-01", "--developer", "alice", "--allow-empty",
		})
	})

	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(rows) != opencodeFixtureEvents {
		t.Fatalf("want %d stored rows, got %d", opencodeFixtureEvents, len(rows))
	}
	for _, r := range rows {
		if r.CostMicro != wantMicro {
			t.Errorf("stored cost_micro = %d, want the AUDITED %d. %d would mean the server re-priced at the guessed self-hosted-medium fallback",
				r.CostMicro, wantMicro, guessMicro)
		}
		if r.Host != "zai-coding-plan" {
			t.Errorf("stored host = %q, want %q — the host never crossed the wire, and a row STORED host-blind reads back as unauditable spend (#236)", r.Host, "zai-coding-plan")
		}
		if r.BillingMode != wantMode {
			t.Errorf("stored billing_mode = %q, want %q", r.BillingMode, wantMode)
		}
	}
}

// TestRunShip_PerRepoSummary_OpencodeRow pins BOTH arms of the summary line.
//
// The absent arm is not decoration: "Opencode ran and found nothing" and
// "Opencode never ran" are different facts, and a build that always printed the
// row would pass the present arm alone. It is the same control #549 needed for
// the Codex row.
func TestRunShip_PerRepoSummary_OpencodeRow(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	loadDeterministicPrices(t)

	run := func(t *testing.T, enable bool) string {
		t.Helper()
		repo := initGitRepo(t)
		claudeDir := t.TempDir()
		writeSessionFixture(t, claudeDir, repo) // 1 Claude Code event
		dbPath := stageOpencodeDB(t, repo)
		srv, _ := newShipTestServer(t, "")
		args := []string{
			"--server", srv.URL, "--repo", repo, "--claude-dir", claudeDir,
			"--opencode-db", dbPath, "--since", "2026-01-01", "--developer", "alice",
		}
		if enable {
			args = append(args, "--opencode")
		}
		return captureStdout(t, func() { runShip(args) })
	}

	t.Run("present with the right count when enabled", func(t *testing.T) {
		out := run(t, true)
		// The EXACT count, not just presence: the row used to be derived by
		// subtracting the per-repo Claude tallies from the run total, which a
		// second cross-repo source silently breaks. A direct tally is what makes
		// this number trustworthy — and asserting it is what proves the tally is
		// wired to THIS source rather than to the run total.
		want := fmt.Sprintf("  opencode (all repos): events_shipped=%d", opencodeFixtureEvents)
		if !strings.Contains(out, want) {
			t.Errorf("summary row %q not found with --opencode on; got:\n%s", want, out)
		}
		// And the Codex row must NOT appear, so the two cross-repo rows are not
		// being printed from one shared counter.
		if strings.Contains(out, "codex-rollout (all repos)") {
			t.Errorf("the Codex row appeared with --codex-rollout off; got:\n%s", out)
		}
	})

	t.Run("absent when disabled", func(t *testing.T) {
		out := run(t, false)
		if strings.Contains(out, "opencode (all repos)") {
			t.Errorf("the Opencode row must not appear without --opencode; got:\n%s", out)
		}
	})
}

// TestRunShip_CompletionLine_NamesOpencodeOmissionWhenOff is #549 arm 4 applied
// to the new source: silence is what let the --codex-rollout omission recur
// during the 2026-07-30 dogfood backfill, where "Shipped 132290 events" printed
// and exited 0 while zero Codex rows landed. The note must appear on a run that
// otherwise looks completely healthy — which is exactly when the omission is
// invisible.
func TestRunShip_CompletionLine_NamesOpencodeOmissionWhenOff(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, _ := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL, "--repo", repo, "--claude-dir", claudeDir,
			"--since", "2026-01-01", "--developer", "alice",
			// deliberately no --opencode
		})
	})
	if !strings.Contains(out, "Opencode NOT included: pass --opencode") {
		t.Errorf("completion line does not name the Opencode omission; got:\n%s", out)
	}
	// Both omissions are named on the same line — a note that silently replaced
	// the Codex clause with the Opencode one would be a regression of #549 arm 4,
	// not a fix.
	if !strings.Contains(out, "Codex NOT included: pass --codex-rollout") {
		t.Errorf("adding the Opencode clause dropped the Codex one; got:\n%s", out)
	}
}

// TestRunShip_OpencodeAbsentDatabaseIsNotAFailure: an operator who enables
// Opencode capture on a machine where Opencode has not run gets a named message
// and exit 0, not a failed cron job. The condition is premature configuration,
// not a fault.
func TestRunShip_OpencodeAbsentDatabaseIsNotAFailure(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	srv, _ := newShipTestServer(t, "")

	// runShip calls os.Exit(1) on a real failure, which would kill the test
	// process — so reaching the completion line at all IS the assertion that the
	// absent database did not fail the run.
	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL, "--repo", repo, "--claude-dir", claudeDir,
			"--opencode", "--opencode-db", filepath.Join(t.TempDir(), "nope.db"),
			"--since", "2026-01-01", "--developer", "alice",
		})
	})
	if !strings.Contains(out, "Shipped 1 events") {
		t.Errorf("the Claude Code half must still ship; got:\n%s", out)
	}
	if !strings.Contains(out, "opencode (all repos): events_shipped=0") {
		t.Errorf("the Opencode row must still print at zero — \"it ran and found nothing\" and \"it never ran\" are different facts; got:\n%s", out)
	}
}
