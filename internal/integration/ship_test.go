//go:build integration

// End-to-end wire test for the #126 laptop-shipper leg: a Claude Code JSONL
// session fixture on the "laptop" side flows through the real JSONL
// collector into the HTTP shipper client, over the wire to the bearer-gated
// POST /api/v1/events, into the real SQLite store, and out through
// GET /api/v1/scores. The second ship run (a fresh, stateless client — the
// cron re-run case) must leave the scores byte-identical: over-shipping is
// the design, replay safety is the contract.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/shipper"
)

// initShipGitRepo creates a git repo with one commit so the collector's
// validateGitRepo passes; issue resolution rides the fixture's branch name.
func initShipGitRepo(t *testing.T) string {
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

// writeShipSessionFixture writes one JSONL session whose CWD points at repo.
// 1000 input + 500 output on claude-sonnet-4 → $0.0105 at the embedded RPT.
func writeShipSessionFixture(t *testing.T, claudeDir, repo string) {
	t.Helper()
	projDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-ship","gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg_ship_1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		repo,
	)
	if err := os.WriteFile(filepath.Join(projDir, "s1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// shipOnce builds a FRESH shipper client (statelessness is the point — no
// state survives between runs) and pushes the repo's JSONL events to srvURL.
func shipOnce(t *testing.T, srvURL, repo, claudeDir string) {
	t.Helper()
	client, err := shipper.New(shipper.Config{ServerURL: srvURL, APIToken: apiToken})
	if err != nil {
		t.Fatalf("shipper.New: %v", err)
	}
	c := &collector.JSONLCollector{
		RepoPath:    repo,
		ClaudeDir:   claudeDir,
		DeveloperID: "alice",
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := c.Run(context.Background(), since, client); err != nil {
		t.Fatalf("collector.Run: %v", err)
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := client.Shipped(); got != 1 {
		t.Fatalf("Shipped() = %d, want 1", got)
	}
}

// TestPipeline_ShipJSONLToScores is the #126 wire verification in test form:
// JSONL fixture → tierd-ship client → POST /api/v1/events → SQLite →
// GET /api/v1/scores, run twice, totals identical after the re-run.
func TestPipeline_ShipJSONLToScores(t *testing.T) {
	srv := newServer(t)
	repo := initShipGitRepo(t)
	claudeDir := t.TempDir()
	writeShipSessionFixture(t, claudeDir, repo)

	shipOnce(t, srv.URL, repo, claudeDir)

	a := alice(t, getScores(t, srv.URL))
	// Exact RPT-priced cost: 1000×$3/M + 500×$15/M = $0.0105. An exact
	// match proves the laptop-side price table applied and the dollar
	// value survived the wire round-trip into micro-dollars unchanged.
	if !approxEq(a.TotalCostUSD, 0.0105) {
		t.Fatalf("after first ship: total cost = %.6f, want 0.010500", a.TotalCostUSD)
	}

	// Re-ship from scratch — the stateless cron re-run. The idempotency
	// key (msg_ship_1) collides server-side; totals must not move.
	shipOnce(t, srv.URL, repo, claudeDir)

	a = alice(t, getScores(t, srv.URL))
	if !approxEq(a.TotalCostUSD, 0.0105) {
		t.Fatalf("after re-ship: total cost = %.6f, want 0.010500 (replay must be a no-op)", a.TotalCostUSD)
	}
}

// TestPipeline_EventsAuthBoundary extends the #59 exposure model to the new
// endpoint: an unauthenticated POST /api/v1/events is refused outright.
func TestPipeline_EventsAuthBoundary(t *testing.T) {
	srv := newServer(t)

	body := []byte(`[{"developer":"mallory","issue_id":"i","model":"m","cost_usd":1.0,` +
		`"source":"jsonl","fidelity":"realtime","idempotency_key":"k","timestamp":"2026-05-19T10:00:00Z"}]`)
	resp, err := http.Post(srv.URL+"/api/v1/events", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /events: %d, want 401; body=%s", resp.StatusCode, b)
	}
}

// TestPipeline_ShipCarriesRepoToStore is the #491 end-to-end control arm.
//
// Every other ship test builds its fixture repo WITHOUT a remote.origin.url, so
// the collector resolves repoid.Unqualified and the repo field is dropped. That
// means they all exercise the OMIT branch — the branch that fixes the bug was
// never run against the real handler, only against a recording stub that
// performs no validation. #491 survived precisely in that gap.
//
// This ships with a real slug through the real api.Handler into the real store
// and asserts the row landed qualified. Reintroducing the defect (removing
// wireEvent.Repo) fails it with repo="unqualified".
func TestPipeline_ShipCarriesRepoToStore(t *testing.T) {
	srv, _ := newServerWithStore(t)
	repo := initShipGitRepo(t)
	claudeDir := t.TempDir()
	writeShipSessionFixture(t, claudeDir, repo)

	client, err := shipper.New(shipper.Config{ServerURL: srv.URL, APIToken: apiToken})
	if err != nil {
		t.Fatalf("shipper.New: %v", err)
	}
	c := &collector.JSONLCollector{
		RepoPath:    repo,
		ClaudeDir:   claudeDir,
		DeveloperID: "alice",
		// The operator override — the flag whose help promises "without this
		// your cost never joins your outcomes", and which #491 discarded.
		RepoSlug: "acme/widgets",
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := c.Run(context.Background(), since, client); err != nil {
		t.Fatalf("collector.Run: %v", err)
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Read back through the public export rather than the store handle, so the
	// assertion rides the same surface an operator would use to check.
	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/api/v1/events?since=2026-01-01&until=2027-01-01", nil)
	if err != nil {
		t.Fatalf("build export request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("export status %d; body=%s", resp.StatusCode, b)
	}
	var export struct {
		Events []struct {
			Repo string `json:"repo"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&export); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(export.Events) != 1 {
		t.Fatalf("exported %d events, want 1", len(export.Events))
	}
	if export.Events[0].Repo != "acme/widgets" {
		t.Errorf("stored repo = %q, want acme/widgets — the slug did not survive the wire (#491)", export.Events[0].Repo)
	}
}
