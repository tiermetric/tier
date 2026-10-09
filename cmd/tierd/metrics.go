package main

import (
	"errors"
	"sync/atomic"
	"syscall"

	"github.com/tiermetric/tier/internal/health"
	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

// durationBuckets are the HTTP request-latency histogram buckets, in seconds —
// the conventional Prometheus default ladder, fine for a sub-second JSON API.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// serveMetrics bundles the registry rendered by GET /metrics with the vectors
// the serve path records into (#67).
type serveMetrics struct {
	reg                 *metrics.Registry
	http                *httpMetrics
	watcherEvents       *metrics.CounterVec
	watcherWatchAddFail *metrics.CounterVec
	codexRolloutEvents  *metrics.CounterVec
	opencodeEvents      *metrics.CounterVec
	museEvents          *metrics.CounterVec
	proxyWrites         *metrics.CounterVec
	proxyUncaptured     *metrics.CounterVec
	proxyUnattributed   *metrics.CounterVec
	unknownModels       *metrics.CounterVec
	unknownModelCost    *costRecorder
	pricedCost          *costRecorder
	clampedNegTok       *metrics.CounterVec
	costClamps          *metrics.CounterVec
	identityUnjoined    *metrics.GaugeVec
	zeroOutcomeTripwire *metrics.GaugeVec
	walBytes            *metrics.GaugeVec
	walStatErrors       *metrics.CounterVec
	pushUnattributed    *metrics.CounterVec
	pushMergeLeak       *metrics.CounterVec
	pushMissingPushedAt *metrics.CounterVec
	adminPolls          *metrics.CounterVec
	adminEvents         *metrics.CounterVec
	adminCostDeltas     *metrics.CounterVec
	openaiPolls         *metrics.CounterVec
	openaiEvents        *metrics.CounterVec
	openaiCostDeltas    *metrics.CounterVec
	pricingDivergence   *metrics.CounterVec
}

// costRecorder adapts a micro-dollar CounterVec to store.UnknownModelCostRecorder
// (its Add mirrors metrics.CounterVec.Add(delta, labelValues...)) AND keeps a
// process-lifetime atomic total the serve-path fallback-share check reads
// directly (#135) — the metrics registry exposes no counter getter, so the
// ticker cannot read the rendered value back out. Safe for concurrent
// ComputeCost callers: the CounterVec is mutex-guarded internally and total is
// atomic. Add bounds each event to the capture ceiling before int64 conversion,
// so total holds whole micro-dollars without an out-of-range float conversion.
type costRecorder struct {
	counter *metrics.CounterVec
	total   atomic.Int64
}

// Add records one micro-dollar cost into both the exported counter and the
// in-process total. labelValues are forwarded to the (unlabelled) counter.
func (r *costRecorder) Add(v float64, labelValues ...string) {
	// Match captured spend before float-to-int conversion (MaxInt64 rounds to 2^63).
	v = min(v, float64(store.MaxTokenEventCostMicro))
	r.counter.Add(v, labelValues...)
	r.total.Add(int64(v))
}

// Total returns the process-lifetime micro-dollar sum recorded so far. Monotone
// (costs are non-negative), so a later-minus-earlier read is the interval delta.
func (r *costRecorder) Total() int64 { return r.total.Load() }

// newServeMetrics builds the fixed metric set. Label sets are static and
// low-cardinality (method/route/status-class, version) so no per-developer or
// per-issue value ever becomes a label.
func newServeMetrics(version string) *serveMetrics {
	reg := metrics.NewRegistry()
	reqs := reg.NewCounter(
		"tier_http_requests_total",
		"HTTP requests handled, by method, matched route, and status class.",
		"method", "route", "status")
	dur := reg.NewHistogram(
		"tier_http_request_duration_seconds",
		"HTTP request duration in seconds, by method and matched route.",
		durationBuckets, "method", "route")
	reg.NewGauge(
		"tier_build_info",
		"Build information; constant 1, labelled with the binary version.",
		"version").Set(1, version)
	watcherEvents := reg.NewCounter(
		"tier_watcher_events_total",
		"Token events the live watcher has successfully ingested.")
	// codexRolloutEvents counts token events the Codex rollout-log collector has
	// landed (#464). Kept SEPARATE from tier_watcher_events_total rather than
	// folded into it: the two capture different agents from different files, and
	// a single counter would make "is Codex capture alive?" unanswerable — which
	// is the question this metric exists for, since the collector logs its scan
	// failures instead of surfacing them anywhere else.
	codexRolloutEvents := reg.NewCounter(
		"tier_codex_rollout_events_total",
		"Token events the Codex rollout-log collector has successfully ingested.")
	// opencodeEvents counts token events the Opencode collector has landed
	// (#719), separate from the other two for the same reason they are separate
	// from each other: "is Opencode capture alive?" must be answerable on its own.
	// It matters more here than for the siblings, because this collector's
	// characteristic failure is a SILENT one — an Opencode schema change makes
	// every scan match nothing while still succeeding, and a flat counter is the
	// signal an operator can alert on.
	opencodeEvents := reg.NewCounter(
		"tier_opencode_events_total",
		"Token events the Opencode collector has successfully ingested.")
	// museEvents counts token events the Muse collector has landed (#895),
	// separate from its siblings so "is Muse capture alive?" is answerable alone.
	// Like the Codex counter it also counts re-ingests of a touched session file,
	// which the idempotency keys make no-ops at the store.
	museEvents := reg.NewCounter(
		"tier_muse_events_total",
		"Token events the Muse collector has successfully ingested.")
	// watcherWatchAddFail counts fsnotify watch registrations that failed while
	// the watcher (re)attached a directory tree (#142). A non-zero value means
	// capture may be silently degraded for the unwatched directories: on macOS
	// (kqueue) an aging ~/.claude/projects tree exhausts the process fd limit and
	// each subsequent dir fails with EMFILE; on Linux the inotify watch limit
	// yields ENOSPC. The errno label (emfile|enospc|other) keeps the two limit
	// classes greppable in Prometheus and is static/low-cardinality — no
	// per-developer or path value ever becomes a label.
	watcherWatchAddFail := reg.NewCounter(
		"tier_watcher_watch_add_failures_total",
		"fsnotify watch registrations that failed (EMFILE/ENOSPC/permission); capture may be degraded for unwatched directories.",
		"errno")
	// proxyWrites pairs the proxy's store-write log lines with a metric so a
	// silent InsertTokenEvent failure is observable (#70). The outcome label
	// gives both the failure count and its denominator (ok). provider is one
	// of anthropic/openai/gemini — low-cardinality, never a per-developer value.
	proxyWrites := reg.NewCounter(
		"tier_proxy_writes_total",
		"Proxy token-event store writes attempted, by provider and outcome (ok|error).",
		"provider", "outcome")
	// proxyUncaptured counts 2xx proxied responses the proxy intercepted but
	// produced NO token event from (#117) — the systematic-$0-capture signal
	// that was previously indistinguishable from "the developer did no AI work".
	// reason is one of encoding (non-identity Content-Encoding the proxy cannot
	// parse), parse (JSON body the parser rejected), stream_no_usage (SSE stream
	// that closed without usable usage), or too_large (JSON body past the read
	// cap, forwarded intact but uncaptured, #123). provider/reason are both
	// static and low-cardinality — no per-developer value ever becomes a label.
	proxyUncaptured := reg.NewCounter(
		"tier_proxy_uncaptured_responses_total",
		"Successful (2xx) proxied responses that produced no token event, by provider and reason (encoding|parse|stream_no_usage|too_large).",
		"provider", "reason")
	// proxyUnattributed counts intercepted 2xx proxied responses missing an
	// X-Tier-* attribution header (#129) — one increment per response per missing
	// header, resolved in modifyResponse BEFORE the capture branch, so it counts
	// the misconfigured client even when the response yields no stored event
	// (too_large / parse / no-usage SSE). Any event that IS captured for such a
	// response is stored under the "unattributed" sentinel. So a systematically
	// misconfigured client (never sending X-Tier-Developer) is visible in /metrics
	// instead of silently polluting per-developer aggregates. header is
	// developer|developer-forged|issue|issue-forged|repo — static and
	// low-cardinality; no per-developer value ever becomes a label.
	proxyUnattributed := reg.NewCounter(
		"tier_proxy_unattributed_total",
		`Intercepted 2xx proxied responses missing an X-Tier-* attribution header, counted once per response per missing header (developer|developer-forged|issue|issue-forged|repo); a captured event is then stored under "unattributed". The -forged variants are a client that SUPPLIED the server-assigned sentinel rather than omitting the header — same stored row, different operator signal. Alert on developer-forged (#619): a forged developer moves the sender's spend out of its own denominator and RAISES its score, unlike issue-forged (#466), which only shifts a dollar between buckets inside it.`,
		"header")
	// unknownModels counts API calls priced at the unknown-model fallback rate
	// ($0.50/M) because the model isn't in the price table (#68) — a mispriced-
	// spend signal that pairs with the one-time WARN log naming each model. The
	// guess_path label (#326) names WHICH of the two guess branches priced the
	// call — size_class (a parameter count in the string mapped to a self-hosted-*
	// class heuristic) or flat (nothing matched, flat self-hosted-medium
	// fallback). The label is BOUNDED at those two fixed values by construction
	// (store.GuessPathSizeClass / store.GuessPathFlat): the raw model string is
	// NEVER a label, so cardinality cannot explode on the unbounded model-name
	// space — "which model" stays in the WARN log per the discipline above.
	unknownModels := reg.NewCounter(
		"tier_unknown_model_events_total",
		"API calls priced at the unknown-model fallback rate (model not in the price table, #68), by guess_path (size_class|flat).",
		"guess_path")
	// unknownModelCost / pricedCost are the COST-weighted companions to the
	// event counter above (#135). The event count alone cannot tell an operator
	// whether a burst of fallbacks is noise or a large share of window spend, so
	// the serve-path share check divides the unknown-model fallback cost by the
	// total priced cost. Both are micro-dollars and deliberately UNLABELLED (same
	// low-cardinality discipline as the event counter). Wrapped in a costRecorder
	// so runServe can read the running totals without a counter getter.
	unknownModelCost := &costRecorder{counter: reg.NewCounter(
		"tier_unknown_model_cost_micro_total",
		"Micro-dollars of cost priced at the unknown-model fallback rate (#135).")}
	pricedCost := &costRecorder{counter: reg.NewCounter(
		"tier_priced_cost_micro_total",
		"Micro-dollars of cost priced by ComputeCost across all events; the denominator for the unknown-model fallback share (#135).")}
	// clampedNegTok counts token events whose negative usage counts were clamped
	// to zero at a parser boundary (#121) — a wire/parse defect, since no
	// provider bills negative tokens. The source label is one of jsonl/proxy
	// (static, low-cardinality); one increment per clamped event, not per field.
	clampedNegTok := reg.NewCounter(
		"tier_negative_tokens_clamped_total",
		"Token events whose negative usage counts were clamped to zero at a parser boundary (wire defect).",
		"source")
	// identityUnjoined reports developer identities present on only one side of
	// the cost/outcome join (#125) as of the last /scores computation, or in
	// team/division mode the last seal — the silent-TIER-0 / vanishing-developer
	// signal. A GAUGE, not a counter, because each computation Sets it. side is
	// cost|outcome — static, low-cardinality; no per-developer value is a label.
	identityUnjoined := reg.NewGauge(
		"tier_identity_unjoined",
		"Developer identities on one side of the cost/outcome join with no partner on the other, as of the last /scores computation (team/division mode: the last seal computation). side=cost|outcome.",
		"side")
	// zeroOutcomeTripwire is 1 while cost accrued in the tripwire window but ZERO
	// outcomes were recorded there (#189) — the fail-loud "team TIER will read ~0"
	// signal for trunk-based teams or a broken webhook. A GAUGE, not a counter:
	// the periodic check Sets it to 1 (tripped) or 0 (clear) each pass, so a scrape
	// reads the current condition. Deliberately UNLABELLED — single-tenant, one
	// process-wide condition.
	zeroOutcomeTripwire := reg.NewGauge(
		"tier_zero_outcome_tripwire",
		"1 when cost accrued in the tripwire window but zero outcomes were recorded (team TIER will read ~0); 0 otherwise (#189).")
	// walBytes is the size of the SQLite -wal sidecar, sampled periodically (#669).
	//
	// 🔴 WHY THIS METRIC EXISTS AT ALL, AND WHY IT ARRIVED WITH THE POOL RAISE.
	// A WAL is checkpointed PASSIVELY at commit, and a passive checkpoint can only
	// RESET the file when no reader holds an older snapshot. At the old pool of 1 a
	// reader and the writer shared the single connection, so a commit never found a
	// concurrent reader and the file pinned to SQLite's 1000-page autocheckpoint
	// ceiling (~4.1MB). Above 1 that is no longer guaranteed. MEASURED 2026-08-13,
	// 1200 writes per arm, same DB shape:
	//
	//	maxOpen=1, 2 readers                     -> 4.15MB  (1.01x the ceiling)
	//	maxOpen=4, 0 readers          [CONTROL]  -> 4.15MB  (1.01x)
	//	maxOpen=4, 2 readers, back-to-back reads -> 12.9MB  (3.15x, never resets)
	//	maxOpen=4, 2 readers, 500ms poll gap     -> 4.15MB  (1.01x)
	//
	// ⭐ The zero-reader CONTROL is the load-bearing arm: it shows the growth needs
	// BOTH the raised pool AND continuous read pressure, so this is not "the pool
	// makes the WAL grow". A gap as small as 500ms restores the healthy ceiling
	// completely, and tier's dashboard has no auto-refresh at all — so the hazard
	// is REACHABLE but not REACHED by today's workload. That is precisely why this
	// ships as an INSTRUMENT rather than a checkpointer: the failure would
	// otherwise present as DISK EXHAUSTION with no prior signal.
	//
	// A GAUGE and deliberately UNLABELLED, matching zeroOutcomeTripwire.
	// walStatErrors counts failures to stat the -wal sidecar (#669).
	//
	// 🔴 IT EXISTS BECAUSE THE GAUGE ALONE HAS A FALSE-GREEN MODE. checkWALSize
	// deliberately leaves walBytes UNTOUCHED on a stat error rather than flapping
	// it to 0, so a transient filesystem error cannot read as "the WAL is fine
	// now". The cost of that choice is that a PERSISTENT failure — a vanished bind
	// mount, ENOTDIR, EACCES on the directory — freezes the gauge at its last
	// healthy sample forever, and a scraper cannot tell that from a healthy WAL.
	// This counter is what distinguishes them: a rising count means the gauge is
	// STALE, not calm. ⚠️ Alert on the counter, not only on the gauge.
	walStatErrors := reg.NewCounter(
		"tier_sqlite_wal_stat_errors_total",
		"Failures to stat the SQLite -wal sidecar. Non-zero means tier_sqlite_wal_bytes is stale rather than healthy (#669).")
	walBytes := reg.NewGauge(
		"tier_sqlite_wal_bytes",
		"Size in bytes of the SQLite -wal sidecar at the last sample. Sustained growth far above ~4.1MB means passive checkpoints cannot reset the WAL because a reader is always open (#669).")
	// pushUnattributed counts direct commits to the default branch that push capture
	// could not attribute to an issue (no resolvable issue id, no GitHub author
	// login, or no usable commit id for the #849 ledger) and therefore did NOT
	// score (#196). Pairs with the per-commit INFO log
	// so a systematically un-referenced trunk workflow is visible in /metrics instead
	// of silently earning zero. Deliberately UNLABELLED — single-tenant, and no
	// per-developer/issue value ever becomes a label.
	pushUnattributed := reg.NewCounter(
		"tier_push_unattributed_total",
		"Direct commits to the default branch that could not be attributed (no issue id, no GitHub author login, or no usable commit id) and were not scored (#196, #849).")
	// pushMergeLeak counts default-branch pushes that carried a "Merge pull
	// request #N" commit AND had other commits captured beside it (#849). Those
	// commits are the merged PR's branch commits, so the work may count twice
	// until #934 closes it. A rebase merge leaves no merge commit and is not
	// counted. Deliberately UNLABELLED.
	pushMergeLeak := reg.NewCounter(
		"tier_push_merge_commit_captures_total",
		"Default-branch pushes carrying a PR merge commit whose other commits were also captured; they may double-count the PR (#849, #934).")
	// pushMissingPushedAt counts direct commits push capture recorded from a
	// payload with no usable repository.pushed_at (#938): each keeps its credit
	// but sorts last for its day row's ownership, so it never takes a row over.
	// Deliberately UNLABELLED.
	pushMissingPushedAt := reg.NewCounter(
		"tier_push_missing_pushed_at_total",
		"Direct commits recorded from a push payload with no usable repository.pushed_at; each sorts last for its day row's ownership (#938).")
	// Anthropic Admin poller observability (#138). All UNLABELLED except the poll
	// outcome (ok|error, static/low-cardinality) — no org or model value ever
	// becomes a label. The poller is a non-liveness-critical reconciliation feed, so
	// these counters (not /healthz, out of scope for #138) are how an operator sees
	// it is alive and ingesting.
	adminPolls := reg.NewCounter(
		"tier_anthropic_admin_polls_total",
		"Anthropic Admin API poll passes, by outcome (ok|error).",
		"outcome")
	adminEvents := reg.NewCounter(
		"tier_anthropic_admin_remainder_events_total",
		"Settled-day remainder token events the Anthropic Admin poller has ingested (#138).")
	adminCostDeltas := reg.NewCounter(
		"tier_anthropic_admin_cost_deltas_total",
		"org_actual_spend delta rows posted by the Anthropic Admin cost-report reconciliation (#138).")
	// OpenAI Usage poller observability (#139). Structural twin of the Anthropic
	// Admin counters above: all UNLABELLED except the poll outcome (ok|error,
	// static/low-cardinality) — no org or model value ever becomes a label.
	openaiPolls := reg.NewCounter(
		"tier_openai_usage_polls_total",
		"OpenAI Usage/Costs API poll passes, by outcome (ok|error).",
		"outcome")
	openaiEvents := reg.NewCounter(
		"tier_openai_usage_remainder_events_total",
		"Settled-day remainder token events the OpenAI Usage poller has ingested (#139).")
	openaiCostDeltas := reg.NewCounter(
		"tier_openai_usage_cost_deltas_total",
		"org_actual_spend delta rows posted by the OpenAI Costs-report reconciliation (#139).")
	// pricingDivergence counts /events rows whose client-posted cost_usd disagreed
	// with the server's authoritative price (#233) — the mixed-version-fleet signal.
	// The server ALWAYS stores its own price; this counts how often a shipper's
	// local table disagreed. Deliberately UNLABELLED (single-tenant; no per-model or
	// per-developer value becomes a label — the offending batch index and magnitudes
	// go to the WARN log, not the metric).
	pricingDivergence := reg.NewCounter(
		"tier_pricing_divergence_total",
		"POST /events rows whose client-posted cost_usd diverged from the server's authoritative price (#233).")
	return &serveMetrics{
		reg:                 reg,
		http:                &httpMetrics{requests: reqs, duration: dur},
		watcherEvents:       watcherEvents,
		watcherWatchAddFail: watcherWatchAddFail,
		codexRolloutEvents:  codexRolloutEvents,
		opencodeEvents:      opencodeEvents,
		museEvents:          museEvents,
		proxyWrites:         proxyWrites,
		proxyUncaptured:     proxyUncaptured,
		proxyUnattributed:   proxyUnattributed,
		unknownModels:       unknownModels,
		unknownModelCost:    unknownModelCost,
		pricedCost:          pricedCost,
		clampedNegTok:       clampedNegTok,
		costClamps:          reg.NewCounter("tier_token_event_cost_clamps_total", "Cost ceiling clamps attempted on capture inserts or committed by reprice."),
		identityUnjoined:    identityUnjoined,
		zeroOutcomeTripwire: zeroOutcomeTripwire,
		walBytes:            walBytes,
		walStatErrors:       walStatErrors,
		pushUnattributed:    pushUnattributed,
		pushMergeLeak:       pushMergeLeak,
		pushMissingPushedAt: pushMissingPushedAt,
		adminPolls:          adminPolls,
		adminEvents:         adminEvents,
		adminCostDeltas:     adminCostDeltas,
		openaiPolls:         openaiPolls,
		openaiEvents:        openaiEvents,
		openaiCostDeltas:    openaiCostDeltas,
		pricingDivergence:   pricingDivergence,
	}
}

// anthropicAdminMetrics adapts the serve-path counters to
// anthropicadmin.Metrics so the poller can record poll outcomes and ingest
// volume without importing the metrics registry directly.
type anthropicAdminMetrics struct {
	polls      *metrics.CounterVec
	events     *metrics.CounterVec
	costDeltas *metrics.CounterVec
}

// PollComplete records one poll pass outcome.
func (m anthropicAdminMetrics) PollComplete(ok bool) {
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.polls.Inc(outcome)
}

// EventsIngested records n remainder events landed in one pass.
func (m anthropicAdminMetrics) EventsIngested(n int) { m.events.Add(float64(n)) }

// CostDeltasPosted records n org_actual_spend delta rows posted in one pass.
func (m anthropicAdminMetrics) CostDeltasPosted(n int) { m.costDeltas.Add(float64(n)) }

// openaiUsageMetrics adapts the serve-path counters to openaiusage.Metrics so
// the poller records poll outcomes and ingest volume without importing the
// metrics registry directly. Structural twin of anthropicAdminMetrics.
type openaiUsageMetrics struct {
	polls      *metrics.CounterVec
	events     *metrics.CounterVec
	costDeltas *metrics.CounterVec
}

// PollComplete records one poll pass outcome.
func (m openaiUsageMetrics) PollComplete(ok bool) {
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.polls.Inc(outcome)
}

// EventsIngested records n remainder events landed in one pass.
func (m openaiUsageMetrics) EventsIngested(n int) { m.events.Add(float64(n)) }

// CostDeltasPosted records n org_actual_spend delta rows posted in one pass.
func (m openaiUsageMetrics) CostDeltasPosted(n int) { m.costDeltas.Add(float64(n)) }

// watcherEventRecorder bumps BOTH the health state's last-event timestamp (#50)
// and the watcher-events counter (#67) on each ingested event. It satisfies
// ingester.EventRecorder, so it drops straight into ingester.RecordingIngester.
type watcherEventRecorder struct {
	state   *health.WatcherState
	counter *metrics.CounterVec
}

func (r watcherEventRecorder) RecordEvent() {
	r.state.RecordEvent()
	r.counter.Inc()
}

// codexRolloutEventRecorder stamps the health state's last-event timestamp (#50)
// and the Codex-specific events counter (#464) on each ingested event, so a
// collector whose scans have quietly stopped producing rows is visible on
// /healthz and /metrics instead of only in the log. It satisfies
// ingester.EventRecorder.
type codexRolloutEventRecorder struct {
	state   *health.WatcherState
	counter *metrics.CounterVec
}

func (r codexRolloutEventRecorder) RecordEvent() {
	if r.state != nil {
		r.state.RecordEvent()
	}
	r.counter.Inc()
}

// opencodeEventRecorder is the Opencode analogue of codexRolloutEventRecorder
// (#719): it stamps the health state's last-event timestamp (#50) and
// tier_opencode_events_total on each ingested event. It satisfies
// ingester.EventRecorder.
type opencodeEventRecorder struct {
	state   *health.WatcherState
	counter *metrics.CounterVec
}

func (r opencodeEventRecorder) RecordEvent() {
	if r.state != nil {
		r.state.RecordEvent()
	}
	r.counter.Inc()
}

// museEventRecorder is the Muse analogue of opencodeEventRecorder (#895).
type museEventRecorder struct {
	state   *health.WatcherState
	counter *metrics.CounterVec
}

func (r museEventRecorder) RecordEvent() {
	if r.state != nil {
		r.state.RecordEvent()
	}
	r.counter.Inc()
}

// watchAddErrno maps an fsnotify Add failure onto the low-cardinality errno
// label for tier_watcher_watch_add_failures_total (#142). Only the two limit
// classes are distinguished — emfile (per-fd kqueue exhaustion, the macOS case)
// and enospc (the Linux inotify watch-count limit); every other failure
// (permission, missing dir) is "other". The error itself is never used as a
// label, keeping cardinality bounded and no path value on the metric.
func watchAddErrno(err error) string {
	switch {
	case errors.Is(err, syscall.EMFILE):
		return "emfile"
	case errors.Is(err, syscall.ENOSPC):
		return "enospc"
	default:
		return "other"
	}
}
