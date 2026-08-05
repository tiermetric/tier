package main

// tierd score-log (#465): a one-shot "price this session log" surface. Reads
// a single Claude Code session JSONL or Codex rollout JSONL file, prices it
// against the Reference Price Table, and prints the priced breakdown as JSON
// on stdout. No store, no daemon, no attribution join, no network.
//
// It exists to close a number-drift risk: model-bench/score.py is a SECOND
// implementation of exactly this measurement (sum a session's tokens, price
// them against prices.yaml), built because tierd had no equivalent surface.
// score-log reuses the SAME parsers and the SAME store.ComputeCostHost the
// live `tierd score`/`tierd serve` capture paths use, so there is one
// implementation of "what did this session cost", not two that can diverge.

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
)

// scoreLogFormatClaude / scoreLogFormatCodex are the only two --format values
// score-log accepts (#465's signature). Named constants so the flag
// validation and the format switch below can't drift apart.
const (
	scoreLogFormatClaude = "claude"
	scoreLogFormatCodex  = "codex"
)

// scoreLogCallJSON is one priced call in the report. Field names mirror the
// GET /api/v1/events export contract (input_tokens, cache_read_tokens, ...;
// see internal/api/export.go's eventExportJSON) so a consumer already parsing
// that shape needs no second mental model for this one.
type scoreLogCallJSON struct {
	Timestamp       string  `json:"ts"`
	Model           string  `json:"model"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	CacheReadTokens int     `json:"cache_read_tokens"`
	CacheWrite5m    int     `json:"cache_write_5m_tokens"`
	CacheWrite1h    int     `json:"cache_write_1h_tokens"`
	CostMicro       int64   `json:"cost_micro"`
	CostUSD         float64 `json:"cost_usd"`
	// BillingMode is the raw provenance ComputeCostHost resolved: per_token,
	// subscription (#154 — a comparable-list-rate VALUATION of flat-fee
	// tokens, not a metered price), or self_hosted_amortized (an estimate).
	// It is PROVENANCE, not the audited/guessed test by itself — see Audited.
	BillingMode string `json:"billing_mode"`
	// Audited is the real audited-vs-guessed signal (#465's design
	// constraint), driven by store.IsAuditedRate — the SAME exact-hit test
	// ComputeCostHost computes internally and throws away. It is
	// DELIBERATELY NOT derived from BillingMode: a self-hosted price-table
	// entry's billing_mode is always self_hosted_amortized whether it was
	// reached by an EXACT operator-provided key (audited) or the size-class
	// GUESS/flat fallback (not), and a --prices override could in principle
	// set billing_mode: per_token on the very fallback key ComputeCostHost
	// guesses through — either way, string-comparing billing_mode would lie.
	Audited bool `json:"audited"`
}

// scoreLogPriceTableJSON records which price table produced these numbers —
// the audit trail for "what prices priced this report", mirroring the stderr
// provenance line `tierd score` already prints.
type scoreLogPriceTableJSON struct {
	Source        string `json:"source"` // "embedded", the --prices path, or the --config prices_file path
	Version       int    `json:"version"`
	EffectiveDate string `json:"effective_date"`
	ModelCount    int    `json:"model_count"`
}

// scoreLogTotalsJSON aggregates scoreLogCallJSON across the whole log. The
// audited/guessed split is summed independently of the combined total so a
// caller (a CI gate, model-bench) can read "what share of this session's
// cost is unauditable" without re-walking Calls itself. The subscription
// split is separate again: a subscription-billed call IS audited (it hit a
// real price-table entry, so store.IsAuditedRate reports true for it) but is
// still a VALUATION of flat-fee tokens at a comparable list rate, not a
// metered price (#154) — callers that need to know "how much of this is a
// valuation, not a bill" read this instead of conflating it with guessed.
type scoreLogTotalsJSON struct {
	InputTokens           int     `json:"input_tokens"`
	OutputTokens          int     `json:"output_tokens"`
	CacheReadTokens       int     `json:"cache_read_tokens"`
	CacheWrite5m          int     `json:"cache_write_5m_tokens"`
	CacheWrite1h          int     `json:"cache_write_1h_tokens"`
	CostMicro             int64   `json:"cost_micro"`
	CostUSD               float64 `json:"cost_usd"`
	AuditedCostMicro      int64   `json:"audited_cost_micro"`
	AuditedCostUSD        float64 `json:"audited_cost_usd"`
	GuessedCostMicro      int64   `json:"guessed_cost_micro"`
	GuessedCostUSD        float64 `json:"guessed_cost_usd"`
	SubscriptionCostMicro int64   `json:"subscription_cost_micro"`
	SubscriptionCostUSD   float64 `json:"subscription_cost_usd"`
	Calls                 int     `json:"calls"`
	GuessedCalls          int     `json:"guessed_calls"`
	SubscriptionCalls     int     `json:"subscription_calls"`
}

// scoreLogReport is the whole `tierd score-log` JSON document printed to
// stdout on success.
type scoreLogReport struct {
	Format    string `json:"format"`
	Log       string `json:"log"`
	SessionID string `json:"session_id,omitempty"`
	StartTS   string `json:"start_ts,omitempty"`
	EndTS     string `json:"end_ts,omitempty"`
	// ModelOverride is the --model value when the operator forced pricing at
	// one model for every call instead of trusting the log's own per-call
	// model field. Empty (omitted from the JSON) is the honest default: each
	// call prices at the model IT recorded, matching tierd score/serve.
	ModelOverride string `json:"model_override,omitempty"`
	// ModelsInLog is the distinct set of RAW model strings the log itself
	// recorded, regardless of ModelOverride, so an operator using the
	// override can still see whether it agrees with what the log says.
	// Never nil (marshals as [] on a model-less log, not null): initialized
	// explicitly below so the field's shape doesn't depend on map iteration
	// happening to run at least once.
	ModelsInLog []string               `json:"models_in_log"`
	PriceTable  scoreLogPriceTableJSON `json:"price_table"`
	Calls       []scoreLogCallJSON     `json:"calls"`
	Totals      scoreLogTotalsJSON     `json:"totals"`
	// Audited is true iff every call priced at an audited rate — i.e.
	// Totals.GuessedCalls == 0. Repeated at the top level (not just inside
	// Totals) so a consumer gating on "no guessed spend in this log" has one
	// field to read instead of reaching into Totals.
	Audited bool `json:"audited"`
}

// runScoreLog implements `tierd score-log` (#465): read ONE session log,
// price it against the Reference Price Table, print the breakdown as JSON on
// stdout. Returns the process exit code; takes injected writers so it is
// unit-testable without os.Exit (matches doctor/backup/reprice's shape).
func runScoreLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("score-log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", `session log format: "claude" or "codex" (required)`)
	logPath := fs.String("log", "", "path to a single session log file (required)")
	model := fs.String("model", "", "override the model used to price EVERY call (optional). Omit to price each call at the model recorded in the log itself, matching tierd score/serve")
	pricesPath := fs.String("prices", os.Getenv("TIER_PRICES"), "path to a price-table YAML override (#68); empty uses the embedded default. A bad file fails the command")
	configPath := fs.String("config", "", "path to a tierd config YAML (#154); score-log reads ONLY prices_file from it, so the CLI prices identically to the serve it reports against. --prices wins over the file's prices_file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if *format != scoreLogFormatClaude && *format != scoreLogFormatCodex {
		_, _ = fmt.Fprintf(stderr, "score-log: --format must be %q or %q, got %q\n", scoreLogFormatClaude, scoreLogFormatCodex, *format)
		return 1
	}
	if *logPath == "" {
		_, _ = fmt.Fprintln(stderr, "score-log: --log is required (path to a single session log file)")
		return 1
	}
	if fi, err := os.Stat(*logPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "score-log: cannot read --log %s: %v\n", *logPath, err)
		return 1
	} else if fi.IsDir() {
		_, _ = fmt.Fprintf(stderr, "score-log: --log %s is a directory, not a session log file\n", *logPath)
		return 1
	}

	// Resolve the effective price table BEFORE pricing any call, exactly as
	// runScore does — a bad --prices/--config file fails the command loudly,
	// never a silent fallback to the embedded default (#68). Precedence
	// mirrors runScore: --prices (or TIER_PRICES) > --config's prices_file >
	// embedded default (#154) — without this, an operator whose `tierd serve`
	// runs a --prices/config override (the only place a
	// billing_mode: subscription route ever lives, since none ships in the
	// embedded default) would silently get different numbers from score-log
	// than from the server it's reporting against.
	resolvedPrices := *pricesPath
	if *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "score-log: --config: %v\n", err)
			return 1
		}
		if resolvedPrices == "" && cfg.PricesFile != nil {
			resolvedPrices = *cfg.PricesFile
		}
	}

	var ptInfo scoreLogPriceTableJSON
	if resolvedPrices != "" {
		info, err := store.LoadPriceTable(resolvedPrices)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "score-log: --prices: %v\n", err)
			return 1
		}
		ptInfo = scoreLogPriceTableJSON{Source: resolvedPrices, Version: info.Version, EffectiveDate: info.EffectiveDate, ModelCount: info.ModelCount}
	} else {
		info := store.ActivePriceTableInfo()
		ptInfo = scoreLogPriceTableJSON{Source: "embedded", Version: info.Version, EffectiveDate: info.EffectiveDate, ModelCount: info.ModelCount}
	}

	var sess collector.ScoreLogSession
	switch *format {
	case scoreLogFormatClaude:
		s, err := collector.ParseClaudeSessionLog(*logPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "score-log: %s: %v\n", *logPath, err)
			return 1
		}
		sess = s
	case scoreLogFormatCodex:
		// "warn" default level, matching shipCodexRollout: a clean score-log
		// run stays silent; a damaged/truncated rollout still surfaces on
		// stderr. TIER_LOG_LEVEL overrides it. Logs go to stderr, so stdout
		// stays pure JSON.
		logger, err := newLogger(stderr, "auto", cmp.Or(os.Getenv("TIER_LOG_LEVEL"), "warn"))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "score-log: %v\n", err)
			return 1
		}
		s, err := codexrollout.ScoreLog(*logPath, logger)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "score-log: %s: %v\n", *logPath, err)
			return 1
		}
		sess = s
	}

	// Control arm, mirroring model-bench/score.py's own: a completed session
	// log must carry real spend. A silent zero-call report is indistinguishable
	// from "pointed this at the wrong file" — refuse rather than print $0.
	if len(sess.Calls) == 0 {
		_, _ = fmt.Fprintf(stderr, "score-log: no billable usage found in %s — refusing to emit a quiet zero-cost report (wrong file, or a genuinely idle/aborted session)\n", *logPath)
		return 1
	}

	report := scoreLogReport{
		Format: *format,
		// Report the path AS GIVEN, not resolved to an absolute path (#465
		// review finding): absolutizing would inject the local username
		// (/Users/<name>/...) into a JSON document this command is explicitly
		// designed to have piped into CI and shared/published — an operator
		// passing a relative path has no way to opt back out of that. If an
		// absolute audit trail is ever genuinely needed, that belongs behind
		// its own opt-in flag, not silent default behavior.
		Log:           *logPath,
		SessionID:     sess.SessionID,
		ModelOverride: *model,
		PriceTable:    ptInfo,
		ModelsInLog:   []string{},
	}
	if !sess.StartTime.IsZero() {
		report.StartTS = sess.StartTime.Format(time.RFC3339)
	}
	if !sess.EndTime.IsZero() {
		report.EndTS = sess.EndTime.Format(time.RFC3339)
	}

	rawModels := map[string]bool{}
	var totals scoreLogTotalsJSON
	for _, call := range sess.Calls {
		if call.Model != "" {
			rawModels[call.Model] = true
		}
		effectiveModel := call.Model
		if *model != "" {
			effectiveModel = *model
		}
		// Host is deliberately "": a session log records the CLIENT's view
		// and cannot know which host served the call, exactly as the live
		// JSONL/rollout collectors leave it (#300/#525) — see
		// ParseClaudeSessionLog / codexrollout.ScoreLog.
		cost, billingMode := store.ComputeCostHost("", effectiveModel, call.Usage)
		// store.IsAuditedRate, NOT a billing_mode string comparison (#465
		// review finding) — see scoreLogCallJSON.Audited's doc comment for
		// why the two are not interchangeable.
		audited := store.IsAuditedRate("", effectiveModel)

		cj := scoreLogCallJSON{
			Timestamp:       call.Timestamp.Format(time.RFC3339),
			Model:           effectiveModel,
			InputTokens:     call.Usage.Input,
			OutputTokens:    call.Usage.Output,
			CacheReadTokens: call.Usage.CacheRead,
			CacheWrite5m:    call.Usage.CacheWrite5m,
			CacheWrite1h:    call.Usage.CacheWrite1h,
			CostMicro:       cost,
			CostUSD:         store.MicroToDollars(cost),
			BillingMode:     billingMode,
			Audited:         audited,
		}
		report.Calls = append(report.Calls, cj)

		totals.InputTokens += cj.InputTokens
		totals.OutputTokens += cj.OutputTokens
		totals.CacheReadTokens += cj.CacheReadTokens
		totals.CacheWrite5m += cj.CacheWrite5m
		totals.CacheWrite1h += cj.CacheWrite1h
		totals.CostMicro += cost
		totals.Calls++
		if audited {
			totals.AuditedCostMicro += cost
		} else {
			totals.GuessedCostMicro += cost
			totals.GuessedCalls++
		}
		if billingMode == store.BillingSubscription {
			totals.SubscriptionCostMicro += cost
			totals.SubscriptionCalls++
		}
	}
	totals.CostUSD = store.MicroToDollars(totals.CostMicro)
	totals.AuditedCostUSD = store.MicroToDollars(totals.AuditedCostMicro)
	totals.GuessedCostUSD = store.MicroToDollars(totals.GuessedCostMicro)
	totals.SubscriptionCostUSD = store.MicroToDollars(totals.SubscriptionCostMicro)
	report.Totals = totals
	report.Audited = totals.GuessedCalls == 0

	for m := range rawModels {
		report.ModelsInLog = append(report.ModelsInLog, m)
	}
	slices.Sort(report.ModelsInLog)

	// Cross-check, mirroring score.py: an operator-supplied --model that
	// names nothing the log itself recorded is very likely a typo or the
	// wrong key — surface it, but don't fail the run over it (the override is
	// deliberate, so honor it regardless).
	if *model != "" && len(report.ModelsInLog) > 0 && !rawModels[*model] {
		_, _ = fmt.Fprintf(stderr, "score-log: WARNING: --model %q does not match any model recorded in the log (%v) — pricing every call at the override anyway\n", *model, report.ModelsInLog)
	}
	if !report.Audited {
		_, _ = fmt.Fprintf(stderr, "score-log: WARNING: %d of %d calls priced at a GUESSED self-hosted reference rate (billing_mode=%s), totalling $%.6f — not an audited $/M price\n",
			totals.GuessedCalls, totals.Calls, store.BillingSelfHostedAmortized, totals.GuessedCostUSD)
	}
	// #154 caveat, mirroring runScore's SubscriptionRouteSummary provenance
	// line: a subscription-billed call IS audited (a real price-table entry,
	// not a guess) but its cost is a comparable-list-rate VALUATION of
	// flat-fee tokens, not a metered price. Silence here would let a
	// subscription figure read as an ordinary per-token bill.
	if totals.SubscriptionCalls > 0 {
		_, _ = fmt.Fprintf(stderr, "score-log: NOTE: %d of %d calls priced at a subscription (flat-fee) VALUATION rate (billing_mode=%s), totalling $%.6f — a comparable list-rate estimate, not a metered price\n",
			totals.SubscriptionCalls, totals.Calls, store.BillingSubscription, totals.SubscriptionCostUSD)
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		_, _ = fmt.Fprintf(stderr, "score-log: encode output: %v\n", err)
		return 1
	}
	return 0
}
