package main

// End-to-end tests for `tierd ship` (#126): JSONL fixture → runShip → real
// api.Handler + SQLite store over httptest → per-developer micro-dollar
// totals. Reuses the fixture helpers from score_test.go (same package).

import (
	"context"
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

// TestRunShip_NoSessions covers the empty-laptop path: nothing to ship is a
// success (exit 0 via normal return), with guidance output and zero posts.
func TestRunShip_NoSessions(t *testing.T) {
	repo := initGitRepo(t)
	claudeDir := t.TempDir() // no projects/ content
	srv, db := newShipTestServer(t, "")

	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
		})
	})
	if !strings.Contains(out, "nothing to ship") {
		t.Errorf("missing no-sessions guidance; got:\n%s", out)
	}
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 0 {
		t.Errorf("no events should land; got %v", costs)
	}
}
