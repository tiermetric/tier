package openaiusage

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

const (
	// provider is the price-table provider tag the poller reconciles against.
	// The usage report is OpenAI-only, but the captured-token baseline is
	// filtered to this provider (store.ProviderOf) so only OpenAI capture
	// offsets the OpenAI aggregate.
	provider = "openai"

	// settlementLag is how far in the past a day bucket's END must be before
	// TIER ingests it. 24h: only once both the realtime capture and the
	// provider's own aggregate have finalized is the remainder computed ONCE
	// against a stable pair of totals. Ingesting an open day would fight the
	// MAX-on-conflict upsert — a remainder that later SHRINKS (capture catching
	// up) would be ignored, freezing the stale larger value.
	settlementLag = 24 * time.Hour

	// DefaultPollInterval is used when config omits poll_interval. The config
	// floor (min 5m) is validated by cmd/tierd at startup: below that the org
	// poll adds provider load without materially fresher settled-day data
	// (settlement lags a full day regardless of cadence).
	DefaultPollInterval = time.Hour
)

// Store is the subset of *store.DB the poller needs. Declared as a
// consumer-side interface (matching anthropicadmin's Store) so tests inject a
// fake without a real SQLite file.
type Store interface {
	// CapturedTokensByDayModel returns per-normalized-model token totals already
	// captured per-request (jsonl/proxy) for the given UTC day, filtered to
	// provider — the baseline subtracted from the org aggregate.
	CapturedTokensByDayModel(ctx context.Context, day time.Time, provider string) (map[string]store.CostUsage, error)
	// OrgActualSpendNet returns the net recorded org_actual_spend micro-dollars
	// for (org, period) SCOPED TO source; 0 when none. Source-scoping keeps this
	// poller from cannibalizing another provider's spend (#138 review R1).
	OrgActualSpendNet(ctx context.Context, org, period, source string) (int64, error)
	// InsertOrgActualSpend appends one org_actual_spend delta row (tagged with
	// its Source).
	InsertOrgActualSpend(ctx context.Context, o store.OrgActualSpend) error
}

// Metrics is the optional counter sink. All methods must tolerate a nil
// receiver via the poller's nil-guard, so cmd/tierd can pass a real adapter and
// tests can pass nil.
type Metrics interface {
	PollComplete(ok bool)
	EventsIngested(n int)
	CostDeltasPosted(n int)
}

// Poller polls the OpenAI Usage/Costs API on an interval, ingesting settled-day
// remainder token events and reconciling org actual spend. It implements
// collector.Collector.
type Poller struct {
	client   *Client
	store    Store
	org      string
	interval time.Duration
	logger   *slog.Logger
	metrics  Metrics
	// now is injectable so tests pin the settlement window deterministically.
	now func() time.Time
}

// PollerConfig configures a Poller. Client, Store, and Org are required; the
// rest default.
type PollerConfig struct {
	Client   *Client
	Store    Store
	Org      string
	Interval time.Duration
	Logger   *slog.Logger
	Metrics  Metrics
	Now      func() time.Time
}

// NewPoller builds a Poller with defaults filled in.
func NewPoller(cfg PollerConfig) *Poller {
	p := &Poller{
		client:   cfg.Client,
		store:    cfg.Store,
		org:      cfg.Org,
		interval: cfg.Interval,
		logger:   cfg.Logger,
		metrics:  cfg.Metrics,
		now:      cfg.Now,
	}
	if p.interval <= 0 {
		p.interval = DefaultPollInterval
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p
}

// Name implements collector.Collector.
func (p *Poller) Name() string { return collector.SourceOpenAIUsage }

// Run implements collector.Collector. It performs one poll pass immediately,
// then repeats on the configured interval until ctx is cancelled.
//
// DELIBERATE ASYMMETRY with the JSONL watcher (cmd/tierd): a poll failure is
// logged at ERROR and retried on the next tick — it does NOT abort Run and must
// NOT kill serve. Org-level coverage polling is a reconciliation feed, not a
// liveness-critical data plane, so a transient provider outage should degrade
// the remainder feed, not take down the whole binary. Run therefore returns
// only on ctx cancellation (nil, a clean shutdown), never surfacing a poll
// error upward.
func (p *Poller) Run(ctx context.Context, _ time.Time, ing collector.Ingester) error {
	if ing == nil {
		return fmt.Errorf("openai-usage poller: Ingester is required")
	}
	p.runPass(ctx, ing)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.runPass(ctx, ing)
		}
	}
}

// runPass runs one poll pass, logging (not returning) any error.
func (p *Poller) runPass(ctx context.Context, ing collector.Ingester) {
	if err := p.pollOnce(ctx, ing); err != nil {
		if ctx.Err() != nil {
			return // shutting down; not a failure worth shouting about
		}
		p.logger.Error("openai-usage poll failed; will retry next tick", "err", err)
		p.recordPoll(false)
		return
	}
	p.recordPoll(true)
}

// Collect implements collector.Collector: one usage-report pass returning the
// settled-day remainder events. It does NOT run cost reconciliation — that
// writes org_actual_spend and belongs to the serve-time Run loop, not a
// read-only slice path.
//
// NOTE: nothing wires this poller into the `tierd score` CLI today (that path
// uses only the JSONL collector); the poller is instantiated solely in runServe
// via Run. Collect exists to satisfy the Collector interface. If it were ever
// wired into `score`, be aware it issues a LIVE Usage-API round-trip per
// invocation.
func (p *Poller) Collect(ctx context.Context, _ time.Time) ([]collector.TokenEvent, error) {
	now := p.now().UTC()
	buckets, err := p.client.fetchUsage(ctx, windowStart(now), now)
	if err != nil {
		return nil, fmt.Errorf("usage report: %w", err)
	}
	events, overCapture, err := p.buildRemainderEvents(ctx, buckets, now.Add(-settlementLag))
	if err != nil {
		return nil, err
	}
	p.warnOverCapture(overCapture)
	return events, nil
}

// pollOnce performs one full poll: usage → remainder events (ingested), then
// cost report → org_actual_spend delta reconciliation.
func (p *Poller) pollOnce(ctx context.Context, ing collector.Ingester) error {
	now := p.now().UTC()
	settledCutoff := now.Add(-settlementLag)
	start := windowStart(now)

	// --- Usage: settled-day remainder events ---
	usageBuckets, err := p.client.fetchUsage(ctx, start, now)
	if err != nil {
		return fmt.Errorf("usage report: %w", err)
	}
	events, overCapture, err := p.buildRemainderEvents(ctx, usageBuckets, settledCutoff)
	if err != nil {
		return err
	}
	for i := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ing.Ingest(ctx, events[i]); err != nil {
			return fmt.Errorf("ingest remainder event: %w", err)
		}
	}
	p.warnOverCapture(overCapture)
	p.recordEvents(len(events))

	// --- Cost: idempotent org_actual_spend delta reconciliation ---
	costBuckets, err := p.client.fetchCost(ctx, start, now)
	if err != nil {
		return fmt.Errorf("cost report: %w", err)
	}
	nDeltas, err := p.reconcileCost(ctx, costBuckets, settledCutoff)
	if err != nil {
		return err
	}
	p.recordCostDeltas(nDeltas)

	p.logger.Info("openai-usage poll complete",
		"day_buckets", countSettled(usageBuckets, settledCutoff),
		"events", len(events),
		"cost_deltas", nDeltas)
	return nil
}

// adminAgg accumulates one settled day's OpenAI-reported usage for a normalized
// model, keeping the raw model string for pricing/display.
type adminAgg struct {
	raw   string
	usage store.CostUsage
}

// buildRemainderEvents maps settled day buckets to remainder token events.
//
// For each settled (day, normalized model): remainder = admin − captured,
// clamped per class to ≥ 0. An event is emitted only when some class has a
// positive remainder; it carries the REMAINDER token counts (never the full
// admin totals), so spend already captured per-request is never re-counted.
// CostMicro prices the REMAINDER usage at RPT list rates (TIER's denominator is
// list-rate by policy; the provider's own cost figure feeds actual_spend
// instead).
//
// CACHED-TOKEN CARVE-OUT (#139 / P0-01 / A1): OpenAI's input_tokens INCLUDES
// input_cached_tokens (verified against the live Usage API docs — the cached
// count is a subset, not additive). We map InputTok = input_tokens −
// input_cached_tokens and CacheRead = input_cached_tokens, both clamped ≥ 0, so
// the cached tokens are counted once (as CacheRead, priced at the cache-read
// rate) and never double-subtracted. This mirrors the proxy/JSONL parser's
// treatment of the same fields so the admin aggregate and the captured baseline
// use identical class semantics.
//
// Returns the events and the total over-capture (captured > admin) across all
// (day, model) classes — an attribution-slop signal the caller WARNs on.
func (p *Poller) buildRemainderEvents(ctx context.Context, buckets []usageBucket, settledCutoff time.Time) ([]collector.TokenEvent, int, error) {
	var out []collector.TokenEvent
	var overCapture int
	// unpriced collects models the usage report reported with positive tokens
	// but that ProviderOf cannot resolve to the price table — a newly-launched
	// OpenAI model shipped before prices.yaml was updated. These are DROPPED
	// from the remainder feed (the spec filters to price-table provider), so we
	// surface them as a WARN to make the coverage gap observable rather than
	// letting org spend silently vanish.
	unpriced := make(map[string]struct{})
	for _, b := range buckets {
		endingAt := unixToUTC(b.EndTime)
		if !isSettled(endingAt, settledCutoff) {
			continue
		}
		day := unixToUTC(b.StartTime)
		captured, err := p.store.CapturedTokensByDayModel(ctx, day, provider)
		if err != nil {
			return nil, 0, fmt.Errorf("captured tokens for %s: %w", day.Format("2006-01-02"), err)
		}

		// Aggregate admin results per normalized model (multiple raw variants of
		// one model on a day merge onto one key, matching the captured baseline).
		byModel := make(map[string]adminAgg)
		for _, r := range b.Results {
			if r.Model == "" {
				// The usage report was requested group_by=model, so a model-less
				// row is a contract violation, not a value to silently drop.
				return nil, 0, fmt.Errorf("usage bucket %s: result missing required model field", day.Format("2006-01-02"))
			}
			norm := store.NormalizeModel(r.Model)
			if store.ProviderOf(norm) != provider {
				// Not in the price table (or a non-OpenAI leak — the usage report
				// is OpenAI-only). Record it if it carried real usage so the drop
				// is visible; without a price we cannot honestly price its
				// remainder.
				if resultTokens(r) > 0 {
					unpriced[r.Model] = struct{}{}
				}
				continue
			}
			// Carve cached out of input (cached ⊆ input on OpenAI's surface).
			// clampNonNeg guards a malformed report where cached > input.
			uncached := clampNonNeg(r.InputTokens - r.InputCachedTokens)
			agg := byModel[norm]
			agg.raw = r.Model
			agg.usage.Input += uncached
			agg.usage.Output += r.OutputTokens
			agg.usage.CacheRead += r.InputCachedTokens
			// OpenAI has no cache-write SKU: CacheWrite5m/1h stay 0.
			byModel[norm] = agg
		}

		for norm, agg := range byModel {
			rem, over := remainderUsage(agg.usage, captured[norm])
			overCapture += over
			if rem == (store.CostUsage{}) {
				continue // fully captured already — nothing to add
			}
			// Host is deliberately left empty (the store normalizes it to the
			// unknown sentinel): the org usage report bills the ACCOUNT and names
			// no serving host, so claiming one would forge a host-qualified
			// pricing key. The empty host prices identically to the old model-only
			// ComputeCost, so this recovers the discarded billing_mode WITHOUT
			// moving cost (#525).
			//
			// Scope note, MEASURED rather than assumed: every model reaching this
			// point is an exact price-table hit — the ProviderOf check above
			// `continue`s past anything unpriced, so the size-class fallback is
			// UNREACHABLE from this producer and no row here can be a self-hosted
			// guess. Today that means the resolved mode is always per_token, i.e.
			// the same value the store's default produced by accident. What changes
			// is that it is now DERIVED from the entry that actually priced the row
			// instead of assumed.
			//
			// That reachability argument has a SECOND precondition worth stating,
			// because prices.yaml is expected to grow: ComputeCostHost's model-only
			// lookup is additionally gated on the normalized name containing no
			// host separator, so "exact hit" also requires that no host-qualified
			// `model@host` key ever carries THIS poller's provider. True today —
			// every host-qualified row is provider: self-hosted, never openai — but
			// a future `gpt-…@somehost` row with provider: openai would falsify the
			// word UNREACHABLE above.
			//
			// Why it is worth storing a value that does not change today:
			// prices.yaml can declare an explicit billing_mode, and this producer
			// now carries it through instead of overwriting it. (Every explicit one
			// today is on a host-qualified row, set to per_token by #268 — those are
			// unreachable from a host-blind caller. The first mode a bare model key
			// could carry is subscription, for the Ollama Cloud / GLM rows that
			// #113 anticipates and that are deliberately NOT seeded until #304
			// lands.)
			cost, billingMode := store.ComputeCostHost("", agg.raw, rem)
			out = append(out, collector.TokenEvent{
				// Org aggregates cannot honestly be split per developer; an
				// explicit "unattributed" row lands in the team denominator and
				// is visible as its own row (an honest coverage gap) rather than
				// corrupting the leaderboard. Per-api-key→developer mapping is a
				// FUTURE upgrade once identity mapping exists — the usage report
				// supports group_by=api_key_id; add the config shape then.
				Developer:      collector.UnattributedIssueID,
				IssueID:        collector.UnattributedIssueID,
				Model:          agg.raw,
				InputTok:       rem.Input,
				OutputTok:      rem.Output,
				CacheRead:      rem.CacheRead,
				CacheWrite5m:   rem.CacheWrite5m,
				CacheWrite1h:   rem.CacheWrite1h,
				CostMicro:      cost,
				Source:         collector.SourceOpenAIUsage,
				Fidelity:       collector.FidelityDaily,
				IdempotencyKey: collector.IdempotencyKey(collector.SourceOpenAIUsage, "day", day.Format("2006-01-02"), norm),
				BillingMode:    billingMode,
				Timestamp:      endingAt,
			})
		}
	}
	if len(unpriced) > 0 {
		p.logger.Warn("openai-usage: org usage reported for models missing from the price table; their remainder was NOT ingested (update prices.yaml)",
			"models", mapKeys(unpriced))
	}
	return out, overCapture, nil
}

// resultTokens is the total token count across all classes of one usage result
// — used only to decide whether an unpriced/dropped model carried real usage
// worth a WARN. input_cached_tokens is a subset of input_tokens, so it is NOT
// added here (that would double-count) — the input+output sum already reflects
// real usage.
func resultTokens(r usageResult) int {
	return r.InputTokens + r.OutputTokens
}

// mapKeys returns the keys of a set as a slice (for structured logging).
func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// reconcileCost posts the DELTA between the provider's settled month-to-date
// cost (summed over settled day buckets, by month) and the recorded
// org_actual_spend net for each period. An unchanged provider total yields
// delta 0 and no row, so the reconciliation is naturally idempotent and rides
// the #24 accumulation model. Returns the number of delta rows posted.
//
// SOURCE-SCOPED reconciliation (mirrors #138 review R1): both the net read and
// the written row are scoped to source='openai-usage', so this poller
// reconciles its OpenAI month-to-date total ONLY against its own prior rows. A
// manual row or another provider's poller (anthropic-admin) sharing the same
// (org, period) is neither netted against nor overwritten — its spend cannot be
// cannibalized. The allocation read (the `inv` CTE) sums across sources, so the
// org total stays complete once multiple feeds coexist.
//
// The read (OrgActualSpendNet) → compute → write (InsertOrgActualSpend) is not
// one transaction. Safe today: the sole poll goroutine and SQLite's
// single-writer serialization (SetMaxOpenConns(1)) mean no concurrent writer to
// the poller's own source-scoped rows can interleave, so no double-post is
// possible. A second writer of openai-usage rows (none exists) would warrant a
// BeginTx wrap.
//
// UNITS: OpenAI's amount.value is in DOLLARS (not cents, unlike Anthropic), so
// the per-period sum converts to micro-dollars directly via DollarsToMicro,
// rounding ONCE at the micro boundary.
func (p *Poller) reconcileCost(ctx context.Context, buckets []costBucket, settledCutoff time.Time) (int, error) {
	dollarsByPeriod := make(map[string]float64)
	for _, b := range buckets {
		if !isSettled(unixToUTC(b.EndTime), settledCutoff) {
			continue
		}
		period := unixToUTC(b.StartTime).Format("2006-01")
		for _, r := range b.Results {
			// A valid JSON number cannot encode Inf/NaN, so this guard is
			// belt-and-braces — but if a malformed value ever slipped through,
			// DollarsToMicro would pin it to MaxInt64/0 and write invoice-grade
			// garbage into org_actual_spend. Reject loudly (matches #138's
			// fail-loud posture on the cost feed).
			if math.IsInf(r.Amount.Value, 0) || math.IsNaN(r.Amount.Value) {
				return 0, fmt.Errorf("cost bucket %s: non-finite amount %v", period, r.Amount.Value)
			}
			dollarsByPeriod[period] += r.Amount.Value
		}
	}

	posted := 0
	for period, dollars := range dollarsByPeriod {
		totalMicro := store.DollarsToMicro(dollars)
		// Scope BOTH the net read and the written row to this poller's own
		// source (mirrors #138 review R1): reconcile the OpenAI month-to-date
		// total only against previously-posted openai-usage rows, and tag the
		// delta the same, so a manual or other-provider row sharing (org,
		// period) is never netted against or overwritten.
		net, err := p.store.OrgActualSpendNet(ctx, p.org, period, collector.SourceOpenAIUsage)
		if err != nil {
			return posted, fmt.Errorf("org actual spend net %s: %w", period, err)
		}
		delta := totalMicro - net
		if delta == 0 {
			continue
		}
		if err := p.store.InsertOrgActualSpend(ctx, store.OrgActualSpend{
			Org:             p.org,
			Period:          period,
			ActualPaidMicro: delta,
			Source:          collector.SourceOpenAIUsage,
			Timestamp:       p.now().UTC(),
		}); err != nil {
			return posted, fmt.Errorf("insert org actual spend %s: %w", period, err)
		}
		posted++
	}
	return posted, nil
}

// remainderUsage returns the per-class remainder (admin − captured, clamped ≥ 0)
// and the total over-capture (Σ per-class captured-beyond-admin). Clamping is
// per-class because capture and the org aggregate can attribute the same dollar
// to slightly different token classes; the total over-capture surfaces gross
// discrepancies via a WARN.
func remainderUsage(admin, captured store.CostUsage) (rem store.CostUsage, overCapture int) {
	sub := func(a, c int) (remainder, over int) {
		if a >= c {
			return a - c, 0
		}
		return 0, c - a
	}
	var o int
	rem.Input, o = sub(admin.Input, captured.Input)
	overCapture += o
	rem.Output, o = sub(admin.Output, captured.Output)
	overCapture += o
	rem.CacheRead, o = sub(admin.CacheRead, captured.CacheRead)
	overCapture += o
	rem.CacheWrite5m, o = sub(admin.CacheWrite5m, captured.CacheWrite5m)
	overCapture += o
	rem.CacheWrite1h, o = sub(admin.CacheWrite1h, captured.CacheWrite1h)
	overCapture += o
	return rem, overCapture
}

// clampNonNeg returns n when n >= 0, else 0. Used to guard the cached-token
// carve-out against a malformed report where input_cached_tokens > input_tokens
// (which the API's own semantics forbid, but we never trust the wire).
func clampNonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func (p *Poller) warnOverCapture(overCapture int) {
	if overCapture > 0 {
		p.logger.Warn("openai-usage: captured tokens exceeded org report (attribution slop); remainder clamped to 0",
			"over_capture_tokens", overCapture)
	}
}

// --- nil-safe metric helpers ---

func (p *Poller) recordPoll(ok bool) {
	if p.metrics != nil {
		p.metrics.PollComplete(ok)
	}
}

func (p *Poller) recordEvents(n int) {
	if p.metrics != nil && n > 0 {
		p.metrics.EventsIngested(n)
	}
}

func (p *Poller) recordCostDeltas(n int) {
	if p.metrics != nil && n > 0 {
		p.metrics.CostDeltasPosted(n)
	}
}

// --- window / settlement helpers ---

// unixToUTC converts a unix-second timestamp (the OpenAI bucket time format)
// to a UTC time.Time.
func unixToUTC(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// isSettled reports whether a bucket ending at endingAt has finalized: its end
// is at or before the settlement cutoff (now − settlementLag).
func isSettled(endingAt, settledCutoff time.Time) bool {
	return !endingAt.UTC().After(settledCutoff)
}

// countSettled counts settled buckets (for the poll-complete log line).
func countSettled(buckets []usageBucket, settledCutoff time.Time) int {
	n := 0
	for _, b := range buckets {
		if isSettled(unixToUTC(b.EndTime), settledCutoff) {
			n++
		}
	}
	return n
}

// windowStart is the start of the poll window: the first day of the PREVIOUS
// calendar month (UTC). Wide enough that (a) the just-closed month fully
// settles after a month rollover, and (b) the current month's cost
// month-to-date is summed from its true start — a narrower rolling window would
// under-report month-to-date mid-month. All emitted usage days are still gated
// by the settlement filter, so the wide window never re-opens the
// remainder-shrink hazard.
func windowStart(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	firstOfThisMonth := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return firstOfThisMonth.AddDate(0, -1, 0)
}
