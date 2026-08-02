// Package collector defines the Collector interface and shared types for all
// token-event data sources: local JSONL files, the reverse proxy, and external
// API adapters (Copilot, Anthropic Admin).
package collector

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"slices"
	"strings"
	"time"
)

// Source identifies where a token event originated.
const (
	SourceJSONL          = "jsonl"           // Claude Code local session files
	SourceProxy          = "proxy"           // TIER reverse proxy (real-time)
	SourceCopilotAPI     = "copilot-api"     // GitHub Copilot Usage Metrics API
	SourceAnthropicAdmin = "anthropic-admin" // Anthropic Admin usage report API
	SourceOpenAIUsage    = "openai-usage"    // OpenAI org Usage/Costs API poller (#139)
	// SourceCodexRollout is the Codex CLI's local rollout logs
	// (~/.codex/sessions/**/rollout-*.jsonl), read by
	// internal/collector/codexrollout (#464). It is the Codex analogue of
	// SourceJSONL: local session files, no proxy, per-call granularity — and
	// the ONLY path that captures Codex spend, since Codex speaks the OpenAI
	// Responses API, which the proxy's Chat-Completions parser cannot read
	// (#463).
	SourceCodexRollout = "codex-rollout"
)

// allSources enumerates every Source constant above. It exists so that the
// shipping decision below cannot be skipped: TestAllSourcesEnumerated parses
// this package's AST and fails if a Source* constant is missing here, so adding
// a new collector forces an explicit answer to "can it ship?" (#492).
//
// Unexported, with AllSources() handing out a copy, because ShippableSources()
// reads this list to build the 400 body a rejected client sees: an exported
// `var` slice is assignable and resliceable by any importer, so a stray write
// would corrupt the endpoint's advertised contract from a distance. The accept
// DECISION is not exposed to that risk either way — ShippableSource switches on
// the constants directly, so no mutation here can widen what is accepted, only
// misreport it.
var allSources = []string{
	SourceJSONL,
	SourceProxy,
	SourceCopilotAPI,
	SourceAnthropicAdmin,
	SourceOpenAIUsage,
	SourceCodexRollout,
}

// AllSources returns every known token-event source, in declaration order.
func AllSources() []string { return slices.Clone(allSources) }

// ShippableSource reports whether a token event carrying this source may be
// accepted on POST /api/v1/events — the `tierd ship` wire from a developer
// laptop to a central tierd (#126).
//
// The wire carries LOCAL-COLLECTOR sources only: those read from per-call
// session logs on the developer's own machine at realtime fidelity. Two
// families are deliberately excluded, for different reasons:
//
//   - SourceProxy is captured server-side by the receiving tierd itself. A
//     client claiming source=proxy would be asserting something only the server
//     can know.
//   - The org-level pollers (SourceCopilotAPI, SourceAnthropicAdmin,
//     SourceOpenAIUsage) emit daily AGGREGATES, not per-call events. Admitting
//     them here would double-count against the same org's poller rows and mix
//     fidelities inside one endpoint; manual imports belong on /api/v1/costs.
//
// SourceCodexRollout ships for exactly the reason SourceJSONL does: it is read
// from local session files at per-call granularity (#464), it is the ONLY path
// that captures Codex spend (#463), and while it was rejected here, Codex work
// arrived at a central tierd with its outcomes counted but its cost missing —
// i.e. it read as FREE and silently inflated TIER (#492).
func ShippableSource(source string) bool {
	switch source {
	case SourceJSONL, SourceCodexRollout:
		return true
	default:
		return false
	}
}

// ShippableSources returns the accepted-source list, in declaration order, for
// error messages that must tell a rejected client what IS accepted.
func ShippableSources() []string {
	out := make([]string, 0, len(allSources))
	for _, s := range allSources {
		if ShippableSource(s) {
			out = append(out, s)
		}
	}
	return out
}

// Fidelity describes the temporal granularity and trust level of a token event.
const (
	FidelityRealtime  = "realtime"  // Per-request, exact token counts
	FidelityDaily     = "daily"     // Daily aggregate from a polling API
	FidelityEstimated = "estimated" // Heuristic or extrapolated value
)

// UnattributedIssueID is the sentinel issue id for token events that cannot be
// tied to a real issue: no matching commit and no issue number derivable from
// the branch (e.g. exploration on main or a detached HEAD).
//
// Per cost-normalization spec section 4.4, such cost is summed into the
// developer's total cost denominator at the developer level. It inflates the
// denominator without adding to any issue's numerator, naturally penalizing
// unfocused usage. Routing it here instead of dropping it closes the #1 gaming
// vector: explore expensively on main or a detached HEAD, then switch to the
// issue branch only for the final generation so the exploration cost never
// enters the TIER denominator.
//
// BOTH capture paths must use this constant so they cannot drift about the
// same dollar: the JSONL join fallback (joinSessionsToCommits) and the proxy
// header fallback (X-Tier-Issue missing). The sentinel matches no issue, so it
// only ever inflates the denominator, never a numerator.
const UnattributedIssueID = "unattributed"

// Labeled unattributed buckets (#refocus, Option B). Unattributed spend is real
// spend that TIER keeps in the denominator (exploratory/planning overhead is part
// of the yield equation), but pooling it all under one opaque `unattributed` line
// hid WHY it did not attribute. These sub-sentinels split that single mass into
// the three honest reasons the JSONL join could not tie a message to an issue.
//
// They are a FAMILY of UnattributedIssueID: each is the base sentinel plus a
// ":<reason>" suffix, so IsUnattributed matches them all. Every read path that
// splits attributed vs. unattributed spend (store.CostCompositionWindow, doctor's
// attribution rate, the score CLI's cost-by-issue) treats the whole family as
// unattributed — the aggregate attribution coverage is unchanged by the split;
// only the labeling of the remainder changes. Because they remain distinct
// issue_id values under the same developer, the per-developer cost denominator is
// byte-identical to the single-sentinel world and no outcome ever joins them
// (outcomes never carry an "unattributed*" issue), so TIER itself is unchanged.
const (
	// UnattributedMain is exploratory/planning cost on a mainline branch
	// (main/master) with no issue — the ~63% of spend that is genuinely
	// issue-less exploration, now a labeled line instead of a silent hole.
	UnattributedMain = UnattributedIssueID + ":main"
	// UnattributedDetachedHEAD is cost on a detached HEAD (gitBranch "HEAD") or a
	// message that recorded no branch at all — no branch name to attribute by.
	UnattributedDetachedHEAD = UnattributedIssueID + ":detached-head"
	// UnattributedNoIssue is cost on a NAMED branch that carries no parseable
	// issue number (e.g. "scratch", "wip-refactor") and matched no closing commit.
	UnattributedNoIssue = UnattributedIssueID + ":branch-without-issue"
)

// IsUnattributed reports whether an issue id is the base unattributed sentinel or
// any of its labeled sub-buckets (UnattributedMain/DetachedHEAD/NoIssue). Consumers
// that split attributed vs. unattributed spend MUST use this rather than an exact
// `== UnattributedIssueID` compare, or the labeled buckets would be mis-counted as
// attributed and silently inflate coverage. The base sentinel (no suffix) is still
// written by the proxy and org-level pollers, which see no branch, so it stays a
// valid family member.
func IsUnattributed(issueID string) bool {
	return issueID == UnattributedIssueID ||
		strings.HasPrefix(issueID, UnattributedIssueID+":")
}

// bucketForBranch classifies a branch that resolved to NO issue into its labeled
// unattributed sub-bucket. Called only after issue resolution has failed, so the
// branch is known to be issue-less: a mainline branch (exploratory), a detached
// HEAD or missing branch, or a named branch without an issue number.
func bucketForBranch(branch string) string {
	switch strings.TrimSpace(branch) {
	case "main", "master":
		return UnattributedMain
	case "", "HEAD":
		return UnattributedDetachedHEAD
	default:
		return UnattributedNoIssue
	}
}

// TokenEvent is a single recorded AI API call or aggregate usage entry.
//
// Cache fields carry provider-specific semantics. For Anthropic: CacheRead is
// `cache_read_input_tokens`; CacheWrite5m / CacheWrite1h come from the nested
// `cache_creation.ephemeral_{5m,1h}_input_tokens` object (legacy responses
// without that nested object bucket all writes into CacheWrite5m). For OpenAI:
// CacheRead is `prompt_tokens_details.cached_tokens`; both write fields are 0.
// OpenAI reports `prompt_tokens` inclusive of `cached_tokens`, so the parser
// carves cached out — the stored InputTok is `prompt_tokens − cached_tokens`
// and the token classes never overlap (same non-overlapping semantics as
// Anthropic, whose `input_tokens` already excludes cache classes).
//
// For Gemini (#122): CacheRead is `cachedContentTokenCount`, a SUBSET of
// `promptTokenCount` — the parser carves it out the same way, so InputTok
// EXCLUDES it. Gemini's reasoning tokens (`thoughtsTokenCount`) are billed at
// the output rate and are NOT included in `candidatesTokenCount`, so OutputTok
// stores billing-equivalent output = candidates + thoughts (there is no
// separate thinking column — TIER's contract is dollar value, and one has no
// driving requirement yet). Both write fields are 0. For other providers: all
// three cache fields are 0 today.
type TokenEvent struct {
	Developer      string
	IssueID        string // extracted from branch name, header, or env var
	Model          string // raw model string from the provider response
	InputTok       int
	OutputTok      int
	CacheRead      int    // cache hits — Anthropic cache_read_input_tokens / OpenAI cached_tokens
	CacheWrite5m   int    // 5-minute cache writes (Anthropic ephemeral_5m_input_tokens)
	CacheWrite1h   int    // 1-hour cache writes (Anthropic ephemeral_1h_input_tokens)
	CostMicro      int64  // integer micro-dollars from the Reference Price Table (#69)
	Source         string // one of the Source* constants
	Fidelity       string // one of the Fidelity* constants
	IdempotencyKey string // see IdempotencyKey() — empty allowed for legacy/unkeyed
	// Repo is the canonical "owner/repo" this cost was spent in (#231), or
	// repoid.Unqualified when the producer cannot determine it (the proxy, the
	// org-level admin pollers). Empty is normalized to the sentinel at insert.
	Repo string
	// SessionID is the opaque upstream session identity this event belongs to
	// (#238) — the JSONL path stamps the Claude Code `sessionId`. It is the
	// grouping key behind the token-waste signatures (context bloat: per-message
	// input_tok growing within one session; rework loop: N sessions re-burning
	// one issue). Producers that cannot know a session (the proxy, org-level
	// pollers) leave it empty; the store persists empty as SQL NULL, so the
	// column stays provenance-honest exactly as Fidelity does. It carries no
	// prompt content — the id is an opaque UUID.
	SessionID string
	// Host is the serving host this cost was incurred against (#300) — the
	// property that prices an open-weights model, since the coarse provider tag
	// cannot tell OpenRouter from Together from a self-hosted Ollama. Only the
	// proxy can know it (it sees the --target URL); session-file and org-poller
	// producers leave it empty, which the store normalizes to the HostUnknown
	// sentinel at insert. Carried here (not just on the proxy's direct write) so
	// every collector funnels through the one ingester.Store adapter without a
	// producer silently dropping the field on the way (#46 closes exactly that
	// asymmetry).
	Host string
	// BillingMode records how the cost was derived (#300) — e.g. list-rate vs a
	// self-hosted amortized basis — decided alongside CostMicro by
	// store.ComputeCostHost.
	//
	// ⚠️ NOT the same provenance rule as Host, and the difference is the point of
	// #525. A producer that cannot know the serving host can still know how the
	// price table billed the row: the JSONL collector and both org pollers leave
	// Host empty and DO carry the resolved mode. Only a producer that derives no
	// cost of its own (the /costs manual import, the demo seeder) leaves this
	// empty. Do not "restore consistency" by dropping it wherever Host is empty —
	// that is precisely the defect #525 fixed.
	//
	// Note this rides the direct-write path (watcher → ingester → InsertTokenEvent).
	// `tier ship` does not carry it on the wire; the receiving server re-derives it
	// in /events (#492) from the same (host="", model) inputs.
	BillingMode string
	Timestamp   time.Time
}

// IdempotencyKey returns a stable cross-source dedup key.
//
// Each component is length-prefixed before hashing so the construction is
// injective: IdempotencyKey("p","a","b") and IdempotencyKey("p","a:b") never
// produce the same digest, regardless of which delimiter would have been used.
//
// Two conventions sit on top of this primitive:
//
//   - MessageIdempotencyKey(provider, upstream_id) — used by both JSONL and
//     proxy paths when an upstream message id is available. Same input → same
//     key, so a Claude call captured by both paths collides on the partial
//     unique index and is stored exactly once (closes #19).
//
//   - Source-prefixed keys (this function, source-first) — fallback for paths
//     that lack an upstream id (older JSONL formats without message.id, Vertex
//     Gemini without responseId, manual /api/v1/costs posts without a key).
//     These keys collide WITHIN their source on re-scan/retry, but never
//     across sources — which is fine, because without a shared id we can't
//     prove two events describe the same upstream call anyway.
func IdempotencyKey(source string, parts ...string) string {
	if source == "" || len(parts) == 0 {
		return ""
	}
	h := sha256.New()
	writeLP := func(s string) {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		// io.WriteString avoids the allocation that []byte(s) would cause when
		// the writer doesn't expose a StringWriter shortcut.
		_, _ = io.WriteString(h, s)
	}
	writeLP(source)
	for _, p := range parts {
		writeLP(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MessageIdempotencyKey returns the cross-source dedup key for a single
// upstream message. Both the JSONL collector (extracting `message.id` from
// session files) and the proxy (extracting the response id from the body)
// call this with the same arguments for the same upstream call. The result
// is a stable hash that collides on the partial unique index in
// token_events.idempotency_key, so a Claude call captured by both paths is
// stored exactly once.
//
// The "msg" sentinel is a source-independent namespace marker — it is NOT
// equal to any value in the Source* constants, so a future codebase audit
// that greps for SourceProxy/SourceJSONL key constructions cannot match
// this format by accident.
//
// Returns "" when upstreamID is empty, mirroring the previous behavior of
// idempotencyKeyForProxy: empty keys are stored as SQL NULL via NULLIF and
// the partial unique index does not enforce uniqueness over NULLs, so the
// row inserts without dedup protection.
func MessageIdempotencyKey(provider, upstreamID string) string {
	if upstreamID == "" {
		return ""
	}
	return IdempotencyKey("msg", provider, upstreamID)
}

// Collector is implemented by every token-event data source.
//
// Two ways for a collector to surface events:
//   - Collect (the legacy slice-returning form, retained for the on-demand
//     CLI path where the caller wants to render every event).
//   - Run + Ingester (the push form, used by anything that streams into a
//     persistent sink — proxy, watcher, future v1.5 admin-API pollers).
//     Run is the recommended shape for new collectors so each one doesn't
//     re-implement the "build a slice then forward to a sink" dance.
type Collector interface {
	// Name returns a short human-readable identifier, e.g. "jsonl", "proxy".
	Name() string
	// Collect returns all token events since the given time. Convenience
	// wrapper around Run for callers that need every event in memory at
	// once (e.g. the tierd score CLI which then formats them).
	Collect(ctx context.Context, since time.Time) ([]TokenEvent, error)
	// Run forwards events into the supplied Ingester in order. Returns
	// the first Ingest error encountered (or ctx.Err() if the context
	// cancels mid-loop); partial progress (events already sent
	// successfully) is not rolled back.
	Run(ctx context.Context, since time.Time, ing Ingester) error
}

// Ingester is the sink shared by every collector (closes #27). The
// production adapter that forwards into SQLite is `ingester.Store(*store.DB)`
// in package internal/ingester; tests inject in-memory recorders via
// IngesterFunc.
//
// Ingest is called once per TokenEvent. Implementations must be safe to
// call from any goroutine — collectors that run on a goroutine (e.g. the
// fsnotify watcher, which ingests from its debounced processing loop, and
// the proxy, which ingests from request and SSE-close goroutines) call
// Ingest concurrently.
//
// Returning an error stops the calling collector's Run loop. The caller
// decides whether to log-and-continue or abort — the policy is upstream of
// the Ingester so adapter implementations don't need to care.
//
// TODO(v1.5): introduce checkpointing for streaming collectors so a
// crash between Ingest calls doesn't re-deliver everything on restart.
type Ingester interface {
	Ingest(ctx context.Context, ev TokenEvent) error
}

// IngesterFunc adapts a plain function to the Ingester interface. Useful
// for tests and in-process recorders. The function must be safe for
// concurrent callers — same contract as the Ingester interface — so
// goroutine-driven collectors (the watcher's debounced timer callbacks,
// for one) can use it without surprises.
type IngesterFunc func(ctx context.Context, ev TokenEvent) error

// Ingest implements Ingester.
func (f IngesterFunc) Ingest(ctx context.Context, ev TokenEvent) error {
	return f(ctx, ev)
}
