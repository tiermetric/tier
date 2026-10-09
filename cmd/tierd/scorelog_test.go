package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// writeClaudeLog writes a minimal single-message Claude Code session JSONL
// file (bare, no repo/branch machinery — score-log needs none) and returns
// its path.
func writeClaudeLog(t *testing.T, dir, name, model string, input, output, cacheRead int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	line := fmt.Sprintf(`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-1","message":{"id":"msg_A","model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}}}`+"\n",
		model, input, output, cacheRead)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write claude log: %v", err)
	}
	return path
}

// decodeReport decodes runScoreLog's stdout JSON directly into scoreLogReport
// (every field is exported, so no separate test-side mirror type is needed)
// and fails the test with the raw bytes on any decode error.
func decodeReport(t *testing.T, stdout []byte) scoreLogReport {
	t.Helper()
	var r scoreLogReport
	if err := json.Unmarshal(stdout, &r); err != nil {
		t.Fatalf("decode score-log JSON: %v\nraw: %s", err, stdout)
	}
	return r
}

// TestRunScoreLog_ClaudePricesUsingComputeCostHost is the core #465
// regression: the command's cost_micro must equal
// store.ComputeCostHost("", model, usage) EXACTLY — the "same parsers, same
// table, one implementation" acceptance criterion. If score-log computed
// cost any other way (a second arithmetic path), this is the test that
// would catch the drift.
func TestRunScoreLog_ClaudePricesUsingComputeCostHost(t *testing.T) {
	// Pin the embedded default table (#172): store.LoadPriceTable mutates a
	// package-global, so this test must not trust whatever an EARLIER test in
	// this package left active — otherwise it silently starts asserting
	// against a leaked custom table instead of the real embedded default.
	loadEmbeddedPriceTable(t)
	dir := t.TempDir()
	path := writeClaudeLog(t, dir, "session.jsonl", "claude-sonnet-4", 1000, 500, 100)

	var stdout, stderr bytes.Buffer
	code := runScoreLog([]string{"--format", "claude", "--log", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	report := decodeReport(t, stdout.Bytes())
	if len(report.Calls) != 1 {
		t.Fatalf("len(Calls) = %d, want 1", len(report.Calls))
	}
	got := report.Calls[0]

	wantCost, wantMode := store.ComputeCostHost("", "claude-sonnet-4", store.CostUsage{Input: 1000, Output: 500, CacheRead: 100})
	if got.CostMicro != wantCost {
		t.Errorf("cost_micro = %d, want %d (store.ComputeCostHost directly)", got.CostMicro, wantCost)
	}
	if got.BillingMode != wantMode {
		t.Errorf("billing_mode = %q, want %q", got.BillingMode, wantMode)
	}
	if got.CostUSD != store.MicroToDollars(wantCost) {
		t.Errorf("cost_usd = %v, want %v", got.CostUSD, store.MicroToDollars(wantCost))
	}
	if report.Totals.CostMicro != wantCost {
		t.Errorf("totals.cost_micro = %d, want %d", report.Totals.CostMicro, wantCost)
	}
}

// TestRunScoreLog_AuditedVsGuessedDistinctionIsVisible is the design
// constraint #465 states explicitly: an unpriced model must be flagged
// billing_mode=self_hosted_amortized and audited=false, and a real
// price-table hit must NOT be — both directions asserted so this can fail in
// either.
func TestRunScoreLog_AuditedVsGuessedDistinctionIsVisible(t *testing.T) {
	// "claude-sonnet-4" being AUDITED depends on the embedded default table;
	// pin it (#172) so an earlier test's --prices override can't leak in and
	// make this subtest flip to "guessed" for the wrong reason.
	loadEmbeddedPriceTable(t)
	dir := t.TempDir()

	t.Run("audited_model", func(t *testing.T) {
		path := writeClaudeLog(t, dir, "audited.jsonl", "claude-sonnet-4", 100, 50, 0)
		var stdout, stderr bytes.Buffer
		if code := runScoreLog([]string{"--format", "claude", "--log", path}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if !report.Calls[0].Audited || !report.Audited {
			t.Errorf("audited model reported as GUESSED: call.audited=%v report.audited=%v billing_mode=%q",
				report.Calls[0].Audited, report.Audited, report.Calls[0].BillingMode)
		}
		if report.Totals.GuessedCalls != 0 {
			t.Errorf("guessed_calls = %d, want 0", report.Totals.GuessedCalls)
		}
	})

	t.Run("unpriced_model_is_flagged_guessed", func(t *testing.T) {
		path := writeClaudeLog(t, dir, "unpriced.jsonl", "totally-unknown-model-xyz", 100, 50, 0)
		var stdout, stderr bytes.Buffer
		if code := runScoreLog([]string{"--format", "claude", "--log", path}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if report.Calls[0].Audited || report.Audited {
			t.Errorf("unpriced model NOT flagged as guessed: call.audited=%v report.audited=%v billing_mode=%q",
				report.Calls[0].Audited, report.Audited, report.Calls[0].BillingMode)
		}
		if report.Calls[0].BillingMode != store.BillingSelfHostedAmortized {
			t.Errorf("billing_mode = %q, want %q", report.Calls[0].BillingMode, store.BillingSelfHostedAmortized)
		}
		if report.Totals.GuessedCalls != 1 || report.Totals.GuessedCostMicro != report.Totals.CostMicro {
			t.Errorf("totals = %+v, want the whole (only) call's cost counted as guessed", report.Totals)
		}
		// A guessed report is still a SUCCESS exit — a guess is allowed, as
		// long as it is flagged; only a silent misrepresentation is the bug.
		if !strings.Contains(stderr.String(), "GUESSED") {
			t.Errorf("stderr should warn about guessed pricing; got %q", stderr.String())
		}
	})
}

// TestRunScoreLog_ExactSelfHostedEntryIsAuditedNotGuessed is the #465 review
// fix: `audited` must be driven by store.IsAuditedRate (a real exact-hit
// test), NOT by comparing billing_mode against self_hosted_amortized. Both
// this test's cases resolve to billing_mode=self_hosted_amortized — the
// difference is that one is an EXACT, operator-provided price-table key
// (audited) and the other is the size-class GUESS/flat fallback (not). A
// billing_mode-based implementation would report BOTH as "not audited",
// which is exactly the false-negative the review flagged.
func TestRunScoreLog_ExactSelfHostedEntryIsAuditedNotGuessed(t *testing.T) {
	loadEmbeddedPriceTable(t)
	dir := t.TempDir()
	path := writeClaudeLog(t, dir, "session.jsonl", "irrelevant-log-model", 100, 50, 0)

	t.Run("exact_self_hosted_key_is_audited", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		// self-hosted-large is a REAL entry in the embedded default table
		// (internal/store/prices.yaml) — an EXACT hit, not a guess — even
		// though its billing_mode is self_hosted_amortized like every
		// self-hosted entry.
		code := runScoreLog([]string{"--format", "claude", "--log", path, "--model", "self-hosted-large"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if report.Calls[0].BillingMode != store.BillingSelfHostedAmortized {
			t.Fatalf("billing_mode = %q, want %q (test premise)", report.Calls[0].BillingMode, store.BillingSelfHostedAmortized)
		}
		if !report.Calls[0].Audited || !report.Audited {
			t.Errorf("an EXACT self-hosted price-table entry reported as GUESSED: call.audited=%v report.audited=%v", report.Calls[0].Audited, report.Audited)
		}
		if strings.Contains(stderr.String(), "GUESSED") {
			t.Errorf("stderr wrongly warns about a guess for an exact table hit: %q", stderr.String())
		}
	})

	t.Run("guessed_fallback_is_not_audited", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		// No exact entry anywhere in the table — the flat self-hosted-medium
		// fallback. Also billing_mode=self_hosted_amortized, but this time a
		// real guess — the negative control proving the positive case above
		// isn't just "always audited".
		code := runScoreLog([]string{"--format", "claude", "--log", path, "--model", "totally-unknown-self-hosted-xyz"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		if report.Calls[0].BillingMode != store.BillingSelfHostedAmortized {
			t.Fatalf("billing_mode = %q, want %q (test premise)", report.Calls[0].BillingMode, store.BillingSelfHostedAmortized)
		}
		if report.Calls[0].Audited || report.Audited {
			t.Errorf("a GUESSED fallback reported as audited: call.audited=%v report.audited=%v", report.Calls[0].Audited, report.Audited)
		}
	})
}

// TestRunScoreLog_SubscriptionBillingIsAuditedButFlagged covers the third
// billing_mode: a subscription-billed call resolves to a REAL price-table
// entry (audited=true, not a guess) but is a comparable-list-rate VALUATION
// of flat-fee tokens, not a metered price (#154) — score-log must surface
// that distinction on stderr rather than let it read as an ordinary bill.
func TestRunScoreLog_SubscriptionBillingIsAuditedButFlagged(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	pricesPath := writePriceTable(t, scoreSubscriptionTable)
	dir := t.TempDir()
	path := writeClaudeLog(t, dir, "session.jsonl", "glm-5.2", 1000, 500, 0)

	var stdout, stderr bytes.Buffer
	code := runScoreLog([]string{"--format", "claude", "--log", path, "--prices", pricesPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Calls[0].BillingMode != store.BillingSubscription {
		t.Fatalf("billing_mode = %q, want %q (test premise — check scoreSubscriptionTable)", report.Calls[0].BillingMode, store.BillingSubscription)
	}
	if !report.Calls[0].Audited || !report.Audited {
		t.Errorf("a subscription-billed EXACT table hit reported as guessed: call.audited=%v report.audited=%v", report.Calls[0].Audited, report.Audited)
	}
	if report.Totals.SubscriptionCalls != 1 {
		t.Errorf("totals.subscription_calls = %d, want 1", report.Totals.SubscriptionCalls)
	}
	if report.Totals.SubscriptionCostMicro != report.Totals.CostMicro {
		t.Errorf("totals.subscription_cost_micro = %d, want %d (the whole call's cost)", report.Totals.SubscriptionCostMicro, report.Totals.CostMicro)
	}
	if !strings.Contains(stderr.String(), "subscription") {
		t.Errorf("stderr should carry the subscription-valuation caveat; got %q", stderr.String())
	}
}

// TestRunScoreLog_ConfigPricesFile covers the #154 gap the review flagged:
// score-log must be reachable by --config (reading ONLY prices_file, exactly
// as tierd score does) so it can price identically to the serve it reports
// against — subscription routes only ever exist behind a --prices/--config
// override, so without this score-log could never see them at all.
func TestRunScoreLog_ConfigPricesFile(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	dir := t.TempDir()
	log := writeClaudeLog(t, dir, "session.jsonl", "custom-model-1", 1_000_000, 0, 0)

	priceYAML := "version: 1\neffective_date: \"2026-08-01\"\nmodels:\n" +
		"  \"custom-model-1\": { input_per_m: 3.0, output_per_m: 3.0, provider: anthropic }\n" +
		"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
	pricesPath := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(pricesPath, []byte(priceYAML), 0o600); err != nil {
		t.Fatalf("write prices: %v", err)
	}
	cfgPath := filepath.Join(dir, "tierd.yaml")
	if err := os.WriteFile(cfgPath, []byte("prices_file: \""+pricesPath+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Run("config_prices_file_reaches_pricing", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runScoreLog([]string{"--format", "claude", "--log", log, "--config", cfgPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		// 1,000,000 input tokens @ $3.00/M = exactly $3.00 = 3,000,000 micro-dollars.
		const want = int64(3_000_000)
		if report.Totals.CostMicro != want {
			t.Errorf("cost_micro = %d, want %d — --config's prices_file may not be reaching the pricing call", report.Totals.CostMicro, want)
		}
		if report.PriceTable.Source != pricesPath {
			t.Errorf("price_table.source = %q, want %q", report.PriceTable.Source, pricesPath)
		}
	})

	t.Run("explicit_prices_wins_over_config", func(t *testing.T) {
		otherYAML := "version: 1\neffective_date: \"2026-08-01\"\nmodels:\n" +
			"  \"custom-model-1\": { input_per_m: 9.0, output_per_m: 9.0, provider: anthropic }\n" +
			"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
			"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
			"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
		otherPath := filepath.Join(dir, "other-prices.yaml")
		if err := os.WriteFile(otherPath, []byte(otherYAML), 0o600); err != nil {
			t.Fatalf("write other prices: %v", err)
		}

		var stdout, stderr bytes.Buffer
		code := runScoreLog([]string{"--format", "claude", "--log", log, "--config", cfgPath, "--prices", otherPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		// 1,000,000 * $9.00/M = $9.00 = 9,000,000 micro — the --prices table,
		// NOT the config's $3.00/M table.
		const want = int64(9_000_000)
		if report.Totals.CostMicro != want {
			t.Errorf("cost_micro = %d, want %d — --prices must win over --config's prices_file", report.Totals.CostMicro, want)
		}
	})
}

// TestRunScoreLog_LogPathReportedAsGiven is the #465 review privacy fix: the
// report must echo --log exactly as the operator passed it, never resolved
// to an absolute path — an absolute path would inject the local username
// into a document this command is explicitly designed to be piped into CI
// and shared.
func TestRunScoreLog_LogPathReportedAsGiven(t *testing.T) {
	dir := t.TempDir()
	writeClaudeLog(t, dir, "session.jsonl", "claude-sonnet-4", 10, 5, 0)

	// A genuinely relative path, run from the CURRENT working directory —
	// os.Chdir into dir so "session.jsonl" resolves, then pass exactly that
	// relative string.
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	var stdout, stderr bytes.Buffer
	code := runScoreLog([]string{"--format", "claude", "--log", "session.jsonl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Log != "session.jsonl" {
		t.Errorf("log = %q, want the exact relative path %q given on the command line (must not be absolutized)", report.Log, "session.jsonl")
	}
}

// TestRunScoreLog_ModelOverride proves --model actually changes what is
// priced, in both directions: WITHOUT it, the log's own (audited) model
// prices the call; WITH an override to an unpriced model, the SAME usage
// numbers price as a guess. Two calls sharing input tokens but diverging
// only on override is the control arm — if the override flag were a no-op,
// both sub-tests would report identically.
func TestRunScoreLog_ModelOverride(t *testing.T) {
	// The baseline half depends on "claude-sonnet-4" being audited in the
	// embedded default table (#172) — pin it against a leaked override.
	loadEmbeddedPriceTable(t)
	dir := t.TempDir()
	path := writeClaudeLog(t, dir, "session.jsonl", "claude-sonnet-4", 1000, 500, 0)

	var stdoutBase, stderrBase bytes.Buffer
	if code := runScoreLog([]string{"--format", "claude", "--log", path}, &stdoutBase, &stderrBase); code != 0 {
		t.Fatalf("baseline exit = %d, want 0", code)
	}
	baseline := decodeReport(t, stdoutBase.Bytes())
	if !baseline.Audited {
		t.Fatalf("baseline (no override) should be audited; got billing_mode=%q", baseline.Calls[0].BillingMode)
	}

	var stdoutOv, stderrOv bytes.Buffer
	code := runScoreLog([]string{"--format", "claude", "--log", path, "--model", "totally-unknown-override-xyz"}, &stdoutOv, &stderrOv)
	if code != 0 {
		t.Fatalf("override exit = %d, want 0; stderr=%s", code, stderrOv.String())
	}
	overridden := decodeReport(t, stdoutOv.Bytes())
	if overridden.Audited {
		t.Fatalf("--model override to an unpriced key should make the report GUESSED; still audited")
	}
	if overridden.Calls[0].Model != "totally-unknown-override-xyz" {
		t.Errorf("calls[0].model = %q, want the override value", overridden.Calls[0].Model)
	}
	if overridden.ModelOverride != "totally-unknown-override-xyz" {
		t.Errorf("model_override = %q, want the flag value", overridden.ModelOverride)
	}
	if len(overridden.ModelsInLog) != 1 || overridden.ModelsInLog[0] != "claude-sonnet-4" {
		t.Errorf("models_in_log = %v, want [claude-sonnet-4] (the log's OWN model, unaffected by the override)", overridden.ModelsInLog)
	}
	if !strings.Contains(stderrOv.String(), "does not match any model recorded in the log") {
		t.Errorf("expected a mismatch WARNING on stderr; got %q", stderrOv.String())
	}
	if baseline.Calls[0].CostMicro == overridden.Calls[0].CostMicro {
		t.Errorf("cost_micro identical (%d) before and after the override on the same usage counts — the override did not change pricing", baseline.Calls[0].CostMicro)
	}
}

// TestRunScoreLog_ControlArmRefusesZeroUsage mirrors model-bench/score.py's
// own control arm: a log with no billable usage must FAIL the command, not
// print a quiet $0 report that looks identical to "this session cost
// nothing" (a real, legitimate outcome for a completed session).
func TestRunScoreLog_ControlArmRefusesZeroUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","sessionId":"sess-2"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runScoreLog([]string{"--format", "claude", "--log", path}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a zero-usage log", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty — a refused run must not also emit a report", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no billable usage") {
		t.Errorf("stderr = %q, want it to name the refusal reason", stderr.String())
	}

	// Negative control: the SAME command against a log that DOES have usage
	// must succeed — proves the check above isn't rejecting every input.
	real := writeClaudeLog(t, dir, "real.jsonl", "claude-sonnet-4", 10, 5, 0)
	var stdout2, stderr2 bytes.Buffer
	if code := runScoreLog([]string{"--format", "claude", "--log", real}, &stdout2, &stderr2); code != 0 {
		t.Fatalf("exit = %d, want 0 for a log with real usage; stderr=%s", code, stderr2.String())
	}
}

// TestRunScoreLog_FlagValidation covers the required-flag and format-enum
// guards, each with its own case so a broken guard names itself in the
// failure.
func TestRunScoreLog_FlagValidation(t *testing.T) {
	dir := t.TempDir()
	validLog := writeClaudeLog(t, dir, "ok.jsonl", "claude-sonnet-4", 10, 5, 0)

	cases := []struct {
		name   string
		args   []string
		errSub string
	}{
		{"missing --format", []string{"--log", validLog}, "--format must be"},
		{"bad --format", []string{"--format", "yaml", "--log", validLog}, "--format must be"},
		{"missing --log", []string{"--format", "claude"}, "--log is required"},
		{"log not found", []string{"--format", "claude", "--log", filepath.Join(dir, "nope.jsonl")}, "cannot read --log"},
		{"log is a directory", []string{"--format", "claude", "--log", dir}, "is a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runScoreLog(tc.args, &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.errSub) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tc.errSub)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty on a flag-validation failure", stdout.String())
			}
		})
	}

	// Positive control: valid flags must NOT hit any of the branches above.
	var stdout, stderr bytes.Buffer
	if code := runScoreLog([]string{"--format", "claude", "--log", validLog}, &stdout, &stderr); code != 0 {
		t.Fatalf("valid invocation exit = %d, want 0; stderr=%s", code, stderr.String())
	}
}

// TestRunScoreLog_PricesOverride proves BOTH that a bad --prices file fails
// the command (never falls back to the embedded default silently) and that
// a GOOD override actually prices the call under the override's rate, not
// the embedded table's — the second half is the control arm: without it, a
// --prices flag that is silently ignored would still pass every other test
// in this file (they never touch --prices).
func TestRunScoreLog_PricesOverride(t *testing.T) {
	// This test loads a custom --prices table into the package-global active
	// table; restore the embedded default on cleanup (#172) so it does not
	// leak into whichever test runs after it in this file/package.
	restoreEmbeddedPriceTable(t)
	dir := t.TempDir()
	log := writeClaudeLog(t, dir, "session.jsonl", "custom-model-1", 1_000_000, 0, 0)

	t.Run("bad_file_fails_loud", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runScoreLog([]string{"--format", "claude", "--log", log, "--prices", filepath.Join(dir, "does-not-exist.yaml")}, &stdout, &stderr)
		if code != 1 {
			t.Errorf("exit = %d, want 1 for a missing --prices file", code)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty — must not silently fall back to the embedded table", stdout.String())
		}
	})

	t.Run("override_actually_prices", func(t *testing.T) {
		priceYAML := "version: 1\neffective_date: \"2026-08-01\"\nmodels:\n" +
			"  \"custom-model-1\": { input_per_m: 7.0, output_per_m: 7.0, provider: anthropic }\n" +
			"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
			"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
			"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
		pricesPath := filepath.Join(dir, "prices.yaml")
		if err := os.WriteFile(pricesPath, []byte(priceYAML), 0o600); err != nil {
			t.Fatalf("write prices: %v", err)
		}

		var stdout, stderr bytes.Buffer
		code := runScoreLog([]string{"--format", "claude", "--log", log, "--prices", pricesPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
		}
		report := decodeReport(t, stdout.Bytes())
		// 1,000,000 input tokens @ $7.00/M = exactly $7.00 = 7,000,000 micro-dollars.
		const want = int64(7_000_000)
		if report.Totals.CostMicro != want {
			t.Errorf("cost_micro = %d, want %d ($7.00 at the OVERRIDE's rate) — --prices may not be reaching the pricing call", report.Totals.CostMicro, want)
		}
		if report.PriceTable.Source != pricesPath {
			t.Errorf("price_table.source = %q, want %q", report.PriceTable.Source, pricesPath)
		}
	})
}

// TestRunScoreLog_CodexFormat is a thin cross-package integration check: the
// codex path (a different collector package, a different logger plumbing)
// reaches the SAME cost arithmetic as the claude path. Full Codex parsing
// correctness (differencing, containment) is covered in
// internal/collector/codexrollout; this only proves the CLI wiring works.
func TestRunScoreLog_CodexFormat(t *testing.T) {
	dir := t.TempDir()
	body := `{"timestamp":"2026-07-23T00:14:00.000Z","type":"session_meta","payload":{"id":"cli-test-session","cwd":"/repo","git":{}}}
{"timestamp":"2026-07-23T00:14:01.000Z","type":"turn_context","payload":{"model":"gpt-5.6-terra"}}
{"timestamp":"2026-07-23T00:15:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":0,"output_tokens":100,"total_tokens":1100}}}}
`
	path := filepath.Join(dir, "rollout-test.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runScoreLog([]string{"--format", "codex", "--log", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())
	if report.SessionID != "cli-test-session" {
		t.Errorf("session_id = %q, want cli-test-session", report.SessionID)
	}
	if len(report.Calls) != 1 {
		t.Fatalf("len(Calls) = %d, want 1", len(report.Calls))
	}
	wantCost, wantMode := store.ComputeCostHost("", "gpt-5.6-terra", store.CostUsage{Input: 1000, Output: 100})
	if report.Calls[0].CostMicro != wantCost || report.Calls[0].BillingMode != wantMode {
		t.Errorf("call = %+v, want cost_micro=%d billing_mode=%q", report.Calls[0], wantCost, wantMode)
	}
}

// TestDispatch_ScoreLogRouting confirms `tierd score-log` is actually wired
// into dispatch (not just present as a standalone function nothing calls):
// it must reach runScoreLog and propagate its exit code, for both a valid
// and a failing invocation.
func TestDispatch_ScoreLogRouting(t *testing.T) {
	loadEmbeddedPriceTable(t)
	dir := t.TempDir()
	path := writeClaudeLog(t, dir, "session.jsonl", "claude-sonnet-4", 10, 5, 0)

	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"score-log", "--format", "claude", "--log", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dispatch(score-log) exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Error("dispatch(score-log) produced no stdout")
	}

	var stdout2, stderr2 bytes.Buffer
	code2 := dispatch([]string{"score-log", "--format", "bogus", "--log", path}, &stdout2, &stderr2)
	if code2 != 1 {
		t.Errorf("dispatch(score-log, bad format) exit = %d, want 1", code2)
	}
}

// TestRunScoreLog_StdoutIsPureJSON asserts stdout decodes as EXACTLY one JSON
// value with nothing else mixed in — diagnostics (provenance, guessed-price
// warnings) must all land on stderr so a CI check can pipe stdout straight
// into a JSON parser.
func TestRunScoreLog_StdoutIsPureJSON(t *testing.T) {
	dir := t.TempDir()
	// Deliberately triggers a stderr warning (unpriced model) to prove that
	// warning does NOT leak into stdout.
	path := writeClaudeLog(t, dir, "session.jsonl", "totally-unknown-xyz", 10, 5, 0)

	var stdout, stderr bytes.Buffer
	if code := runScoreLog([]string{"--format", "claude", "--log", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("expected a guessed-pricing warning on stderr — the fixture is set up to trigger one")
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout did not decode as JSON: %v\nraw: %s", err, stdout.String())
	}
	if dec.More() {
		t.Error("stdout contains more than one JSON value/trailing content")
	}
}
