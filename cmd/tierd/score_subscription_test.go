package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scoreSubscriptionTable is a price table whose ONLY non-fallback row is a
// MODEL-ONLY subscription entry. Model-only is the shape `tierd score` can
// actually reach: it reads Claude Code JSONL, which records no serving host, so
// every event prices host-blind and a host-qualified `model@host` row could
// never match. A test built on a host-qualified row would prove nothing about
// this command.
const scoreSubscriptionTable = `
version: 1
effective_date: "2026-08-01"
models:
  "glm-5.2": { input_per_m: 0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// scoreCheapSubscriptionTable is scoreSubscriptionTable with a DIFFERENT
// comparable rate, so a test can tell which of two tables actually priced a
// report. 1000×$0.10/M + 500×$1/M = $0.0006.
const scoreCheapSubscriptionTable = `
version: 1
effective_date: "2026-08-01"
models:
  "glm-5.2": { input_per_m: 0.10, output_per_m: 1.00, provider: self-hosted, billing_mode: subscription }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// writePriceTable drops a price-table YAML into a temp file and returns its path.
func writePriceTable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write price table: %v", err)
	}
	return path
}

// captureBothStreams runs fn with os.Stdout AND os.Stderr redirected, returning
// each. runScore prints its report to stdout but its price-table provenance and
// subscription warnings to STDERR, so a stdout-only capture cannot see the half
// of the output that carries the honesty affordances.
//
// It mutates process globals, so callers must never t.Parallel().
func captureBothStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = outW, errW

	type read struct {
		s   string
		err error
	}
	outCh, errCh := make(chan read, 1), make(chan read, 1)
	go func() { b, e := io.ReadAll(outR); outCh <- read{string(b), e} }()
	go func() { b, e := io.ReadAll(errR); errCh <- read{string(b), e} }()
	// Deferred restore runs even if fn panics, which also unblocks both readers.
	defer func() {
		_, _ = outW.Close(), errW.Close()
		os.Stdout, os.Stderr = oldOut, oldErr
	}()

	fn()
	_, _ = outW.Close(), errW.Close()
	o, e := <-outCh, <-errCh
	if o.err != nil || e.err != nil {
		t.Fatalf("read captured streams: %v / %v", o.err, e.err)
	}
	return o.s, e.s
}

// TestRunScore_FlagPricesBeatsConfigPricesFile pins the precedence runScore's
// comment claims — "an explicit --prices wins over the config file". Both are
// supplied, at DIFFERENT rates, so the assertion identifies which table priced
// the report rather than merely observing that one of them did.
func TestRunScore_FlagPricesBeatsConfigPricesFile(t *testing.T) {
	loadEmbeddedPriceTable(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixtureModel(t, claudeDir, repo, "glm-5.2")

	cheap := writePriceTable(t, scoreCheapSubscriptionTable) // → 0.0006
	dear := writePriceTable(t, scoreSubscriptionTable)       // → 0.0044
	cfgPath := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(cfgPath, []byte("prices_file: \""+cheap+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := captureStdout(t, func() {
		runScore([]string{
			"--repo", repo, "--claude-dir", claudeDir, "--since", "2026-01-01",
			"--developer", "alice", "--config", cfgPath, "--prices", dear,
		})
	})
	if !strings.Contains(out, "0.0044") {
		t.Errorf("--prices did not win over the config's prices_file (want the 0.0044 table); got:\n%s", out)
	}
	if strings.Contains(out, "0.0006") {
		t.Errorf("the config's prices_file priced the report; --prices must win. got:\n%s", out)
	}
}

// TestRunScore_PricesSubscriptionRouteWithoutPanicking is acceptance criterion 2
// of this batch, at the surface the issue names (#154).
//
// The abandoned design made this a live landmine: subscription entries carried
// no rates, comparables were injected at `tierd serve` startup from a config
// `tierd score` never loads, and ComputeCost PANICKED if asked to price one
// un-injected. Under the maintainer's list-rate inversion the comparable rate lives in
// the table, so there is nothing to inject and nothing to panic about — and
// this test is what stops that claim from being merely architectural.
//
// The assertion is the EXACT priced dollars, not "it didn't crash": 1000 input ×
// $0.875/M + 500 output × $7/M = $0.004375. A cost-agnostic assertion would pass
// against the $0.50/M unknown-model fallback, which is one of the two outcomes
// (the other being $0) that #113's ruling forbids for these tokens.
func TestRunScore_PricesSubscriptionRouteWithoutPanicking(t *testing.T) {
	// Pin the embedded default ACTIVE first (the price table is a package global
	// and a predecessor test may have swapped it), so the control arm below is
	// genuinely the embedded table and not somebody else's leftover override.
	loadEmbeddedPriceTable(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixtureModel(t, claudeDir, repo, "glm-5.2")
	prices := writePriceTable(t, scoreSubscriptionTable)
	args := []string{
		"--repo", repo,
		"--claude-dir", claudeDir,
		"--since", "2026-01-01",
		"--developer", "alice",
	}

	// CONTROL ARM, run FIRST because --prices mutates the package-global table
	// for the rest of the process: the embedded default has no glm-5.2 row, so
	// the same session falls to the $0.50/M combined self-hosted-medium guess —
	// 1500 tokens × $0.50/M = $0.00075. If both runs printed the same number, the
	// subscription row would not be doing the pricing and the real assertion
	// below would be vacuous.
	outDefault := captureStdout(t, func() { runScore(args) })
	if !strings.Contains(outDefault, "0.0008") {
		t.Errorf("control run does not show the unknown-model fallback cost ~0.00075; got:\n%s", outDefault)
	}

	out := captureStdout(t, func() { runScore(append(append([]string(nil), args...), "--prices", prices)) })
	if !strings.Contains(out, "0.0044") {
		t.Errorf("report does not carry the comparable-rate cost ~0.004375; got:\n%s", out)
	}
}

// TestRunScore_ConfigSuppliesPricesFile covers the --config flag #154 asks for.
// Its ONE job is to let the CLI reach the same price table the server it
// reports against uses — which matters most for subscription routes, since none
// ships in the embedded default and they therefore only ever exist in a
// --prices override.
func TestRunScore_ConfigSuppliesPricesFile(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixtureModel(t, claudeDir, repo, "glm-5.2")
	prices := writePriceTable(t, scoreSubscriptionTable)

	cfgPath := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(cfgPath, []byte("prices_file: \""+prices+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := captureStdout(t, func() {
		runScore([]string{
			"--repo", repo,
			"--claude-dir", claudeDir,
			"--since", "2026-01-01",
			"--developer", "alice",
			"--config", cfgPath,
		})
	})
	if !strings.Contains(out, "0.0044") {
		t.Errorf("--config did not apply the config's prices_file; got:\n%s", out)
	}
}

// TestRunScore_WarnsButProceedsOnUncoveredSubscription pins the severity split
// `score` needs on a `subscriptions:` block.
//
// `score` never POSTS a fee — no database, no Spend Leverage — so a fee
// misconfiguration must not stop it printing costs. But it must not be SILENT
// either: the config below is the exact shape `tierd serve` REFUSES to boot on,
// and if the CLI accepted it without comment the same typo would be fatal on one
// surface and invisible on the other — precisely when an operator has reached
// for the CLI to debug that config.
func TestRunScore_WarnsButProceedsOnUncoveredSubscription(t *testing.T) {
	loadEmbeddedPriceTable(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixtureModel(t, claudeDir, repo, "glm-5.2")
	prices := writePriceTable(t, scoreSubscriptionTable)

	cfgPath := filepath.Join(t.TempDir(), "tier.yaml")
	body := "prices_file: \"" + prices + "\"\n" +
		"subscriptions:\n" +
		"  - route_prefix: \"not-in-any-table@nowhere.example\"\n" +
		"    org: \"acme\"\n" +
		"    monthly_fee_usd: 100.00\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, errOut := captureBothStreams(t, func() {
		runScore([]string{
			"--repo", repo, "--claude-dir", claudeDir, "--since", "2026-01-01",
			"--developer", "alice", "--config", cfgPath,
		})
	})
	// PROCEEDS: the report is printed and correctly priced.
	if !strings.Contains(out, "0.0044") {
		t.Errorf("score did not complete normally with a subscriptions block present; got:\n%s", out)
	}
	// WARNS: and says which prefix, and that serve would refuse it.
	for _, want := range []string{"not-in-any-table@nowhere.example", "REFUSE to start"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr does not mention %q; got:\n%s", want, errOut)
		}
	}
	// The provenance line naming the subscription routes is the only place this
	// report admits its dollars are a comparable-rate VALUATION — the table it
	// prints has no billing_mode column. Assert it reaches the operator.
	if !strings.Contains(errOut, "subscription-billed routes in this table:") {
		t.Errorf("stderr omits the subscription-route provenance line; got:\n%s", errOut)
	}
	// #154 item 3 asks for the RATES, not just the names — the rate is the only
	// auditable part ("is $0.875/$7 a fair peer for this model?"); a bare route
	// name tells a reader nothing they can check.
	for _, want := range []string{"glm-5.2", "in $0.875/M", "out $7/M", "not a metered price"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("provenance line omits %q; got:\n%s", want, errOut)
		}
	}

	// CONTROL ARM: with a COVERED prefix the warning must not fire. A warning
	// that appears regardless of the config is noise an operator learns to skip.
	cfgOK := filepath.Join(t.TempDir(), "tier-ok.yaml")
	okBody := "prices_file: \"" + prices + "\"\n" +
		"subscriptions:\n" +
		"  - route_prefix: \"glm-5.2\"\n" +
		"    org: \"acme\"\n" +
		"    monthly_fee_usd: 100.00\n"
	if err := os.WriteFile(cfgOK, []byte(okBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, errOut2 := captureBothStreams(t, func() {
		runScore([]string{
			"--repo", repo, "--claude-dir", claudeDir, "--since", "2026-01-01",
			"--developer", "alice", "--config", cfgOK,
		})
	})
	if strings.Contains(errOut2, "REFUSE to start") {
		t.Errorf("the warning fired for a COVERED route; got:\n%s", errOut2)
	}
}
