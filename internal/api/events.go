package api

// POST /api/v1/events — batched collector-event ingest for the thin laptop
// shipper (#126, capture-topology Option A).
//
// PROVENANCE NOTE (read before touching the allowlists below): this endpoint
// deliberately re-opens, for AUTHENTICATED shippers only, the provenance
// surface that #34 (source) and #82 (fidelity) closed on /costs. A laptop
// running `tierd ship` re-posts events its local JSONL collector captured, so
// it legitimately carries source="jsonl" / fidelity="realtime" — claims a
// manual /costs client is forbidden to make because they feed
// recomputeKnownSourceCosts and the Coverage % numerator. The trust model:
// the bearer token is thereby attribution-grade (security review Y2 — treat
// it as an org-level secret; any holder can post realtime-fidelity events as
// any developer). Do NOT weaken the /costs guards to match this endpoint —
// /costs stays the untrusted manual-import surface.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

const (
	// maxEventsBody caps the request body at 1 MiB, matching the other write
	// endpoints. 500 maximally-sized events (256-char identifiers all round)
	// still fit comfortably, so the two caps never fight each other.
	maxEventsBody = 1 << 20
	// maxEventsPerBatch bounds the per-request insert transaction. The
	// shipper flushes at this size; anything larger here is a bug or abuse.
	maxEventsPerBatch = 500
	// maxEventClockSkew is how far AHEAD of server time a shipped event's
	// capture timestamp may sit before /events rejects it (#235). A JSONL
	// event carries when the tokens were spent, so it should be at or before
	// now; this window is the tolerance for a laptop clock that runs fast plus
	// in-flight batching latency. It is deliberately reject-not-clamp: silently
	// pulling the ts back to now would forge a capture time, and clamping a
	// whole skewed batch to a single instant would collapse its ordering.
	// Paired with the INSERT-only ts on conflict (store.insertTokenEventSQL),
	// this is what stops one skewed clock from pinning a far-future row inside
	// every future ?since window.
	maxEventClockSkew = 24 * time.Hour
	// minEventYear is the floor for a shipped capture timestamp — a value below
	// it is a corrupt or mis-parsed clock (epoch-zero, a two-digit year), not
	// real history. /events owns its own bounds here rather than borrowing the
	// billing-period constants (minPeriodYear/maxPeriodYear) it is unrelated to;
	// the upper bound is the dynamic now+maxEventClockSkew above, not a fixed year.
	minEventYear = 2020
)

// eventRequest is one element of the /events batch. It is the /costs shape
// plus a mandatory original-capture timestamp, minus the deprecated
// cache_write_tokens field — this contract is new, so the legacy
// single-bucket field is rejected via DisallowUnknownFields rather than
// carried forward. idempotency_key, optional on /costs, is REQUIRED here:
// the stateless shipper's replay safety rests entirely on the key colliding
// with the partial unique index on re-ship.
type eventRequest struct {
	Developer    string `json:"developer"`
	IssueID      string `json:"issue_id"`
	Model        string `json:"model"`
	InputTok     int    `json:"input_tokens"`
	OutputTok    int    `json:"output_tokens"`
	CacheRead    int    `json:"cache_read_tokens"`
	CacheWrite5m int    `json:"cache_write_5m_tokens"`
	CacheWrite1h int    `json:"cache_write_1h_tokens"`
	// CostUSD is the client's LOCALLY-computed cost. As of #233 the server is the
	// single pricing authority: it re-prices the raw token counts with its own
	// table and stores THAT, keeping this value only as a cross-check (a divergence
	// beyond a micro-dollar means the shipper priced under a different table
	// version — a mixed-version fleet — and is surfaced via metric + WARN). Still
	// bounds-checked (finite, non-negative, <= MaxCostUSD) at the trust boundary so
	// the cross-check math can't be driven to NaN/Inf or overflow.
	CostUSD        float64 `json:"cost_usd"`
	Source         string  `json:"source"`
	Fidelity       string  `json:"fidelity"`
	IdempotencyKey string  `json:"idempotency_key"`
	// Repo is the OPTIONAL canonical repository ("owner/repo") this cost was spent
	// in (#231). Omitted -> the 'unqualified' sentinel, which is exactly how every
	// pre-#231 client already behaves, so adding this field breaks no existing
	// integration. Supplying it is what lets a multi-repo org stop fusing issues
	// that share a number. The reserved sentinel may not be supplied explicitly.
	Repo string `json:"repo"`
	// SessionID is the OPTIONAL opaque Claude Code session UUID (#238) this event
	// belongs to — the grouping key for the context-bloat / rework-loop token-waste
	// signatures. Omitted -> "" -> stored as SQL NULL, exactly how a session-blind
	// producer (proxy / poller) behaves, so adding this field breaks no existing
	// integration. Mirrors Repo's optional-additive contract above.
	SessionID string `json:"session_id"`
	// Timestamp is the RFC3339 original capture time from the JSONL session
	// file. Required: shipped events carry when the tokens were spent, not
	// when the batch arrived — /scores windows would otherwise misattribute
	// history shipped after the fact.
	Timestamp string `json:"timestamp"`
}

// eventsResponse is the 201 body: how many events the batch carried. With
// the MAX-on-conflict UPSERT a replayed event is silently absorbed, so
// "accepted" counts events processed, not rows newly created.
type eventsResponse struct {
	Accepted int `json:"accepted"`
}

// handlePostEvents ingests a JSON array of collector token events. Wrapped by
// requireAuth in Register, sharing the #36 failed-auth lockout with every
// other bearer-gated route.
//
// The batch is all-or-nothing: every element is validated first, any invalid
// element 400s naming its index and nothing is inserted; a valid batch rides
// store.InsertTokenEvents (one transaction, MAX-on-conflict UPSERT per row),
// so a re-shipped batch is a no-op and a partially-new batch lands only its
// new rows.
func (h *Handler) handlePostEvents(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxEventsBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var reqs []eventRequest
	if err := dec.Decode(&reqs); err != nil {
		// Distinguish a body-size rejection from malformed JSON so the
		// operator sees the real cause (both are 400s, but "invalid JSON"
		// would mislabel an oversized-but-valid batch).
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("request body exceeds %d bytes; split into smaller batches", maxEventsBody))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Reject trailing JSON after the array — a concatenated second batch
	// would otherwise be silently dropped and "succeed".
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON array")
		return
	}
	// Fail-loud posture: the shipper never posts an empty batch, so one here
	// is a client bug that deserves a 400, not a vacuous 201.
	if len(reqs) == 0 {
		writeError(w, http.StatusBadRequest, "events batch must contain at least one event")
		return
	}
	if len(reqs) > maxEventsPerBatch {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("batch exceeds %d events; split into smaller batches", maxEventsPerBatch))
		return
	}

	// Validate the whole batch before inserting anything (all-or-nothing).
	// One server-time reading judges every element so a batch that straddles a
	// second boundary can't have siblings graded against different "now"s.
	now := time.Now()
	events := make([]store.TokenEvent, 0, len(reqs))
	for i, req := range reqs {
		ev, err := validateEventRequest(req, now)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("events[%d]: %v", i, err))
			return
		}
		events = append(events, ev)
	}

	if err := h.store.InsertTokenEvents(r.Context(), events); err != nil {
		h.logger.Error("insert token events batch", "err", err, "count", len(events))
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}

	// Observe pricing divergence AFTER a successful insert so the metric + WARN
	// describe STORED rows, not an aborted batch (#233). Server is the single
	// pricing authority: events[i].CostMicro is the server's price of the raw token
	// counts; the client-posted cost_usd is a CROSS-CHECK only — a divergence means
	// the shipping laptop priced under a different table version (a mixed-version
	// fleet) or has a client bug. events is built 1:1 from reqs in order, so
	// events[i] pairs with reqs[i]. Only numeric, non-client-controlled values are
	// logged (never the model / developer / issue strings) per the log-safety
	// discipline; the model is passed as dedup-key material only, never logged.
	for i := range events {
		h.observePricingDivergence(events[i].Model, events[i].CostMicro, store.DollarsToMicro(reqs[i].CostUSD))
	}
	writeJSON(w, http.StatusCreated, eventsResponse{Accepted: len(events)})
}

// validateEventRequest applies the element-level rules and converts to the
// store row. Bounds mirror handlePostCosts (required identifiers, 256-char
// caps, finite non-negative money/tokens); the provenance and timestamp
// rules are /events-specific:
//
//   - source: a LOCAL-COLLECTOR source, per collector.ShippableSource — today
//     "jsonl" or "codex-rollout" (#492). "proxy" events are born server-side
//     (the proxy inserts directly), "api" is what /costs issues, and the org
//     pollers emit daily aggregates; accepting any of them here would let a
//     client forge another capture path's rows.
//   - fidelity: exactly "realtime" — the only fidelity the local collectors
//     produce; see the provenance note at the top of this file.
//   - idempotency_key: required, non-empty, <= 256 chars.
//   - timestamp: required RFC3339, at or before now+maxEventClockSkew and not
//     before minEventYear, stored in UTC. now is passed in (not read here) so a
//     whole batch is judged against one instant and the bound is testable.
//
// Both allowlists are exact-match by design (mirrors the /costs switches):
// "Jsonl" / " realtime" fall through to rejection.
func validateEventRequest(req eventRequest, now time.Time) (store.TokenEvent, error) {
	var zero store.TokenEvent
	if req.Developer == "" || req.IssueID == "" || req.Model == "" {
		return zero, fmt.Errorf("developer, issue_id, and model are required")
	}
	if len(req.Developer) > maxIdentifierLen || len(req.IssueID) > maxIdentifierLen || len(req.Model) > maxIdentifierLen {
		return zero, fmt.Errorf("identifier fields must be <= %d chars", maxIdentifierLen)
	}
	// The unattributed sentinel may not be FORGED, but the JSONL collector that ships
	// here legitimately assigns it, so /events allows the four exact canonical
	// spellings and rejects every near-miss and case variant (#466). Deliberately NOT
	// validateIssueID: that strict client-surface rule would reject the collector's
	// own output and permanently destroy capture — see validateShippedIssueID.
	if err := validateShippedIssueID(req.IssueID); err != nil {
		return zero, err
	}
	// `developer`, by contrast, gets the STRICT rule (#619) — no allowlist, because
	// nothing that ships here legitimately assigns the sentinel to it. The two
	// collectors this wire admits (the `source` allowlist below is
	// collector.ShippableSource: "jsonl" and "codex-rollout") both label every event
	// with --developer or collector.OSUsername(), whose own no-identity fallback is
	// "unknown", never "unattributed". The producers that DO assign the developer
	// sentinel — the anthropic-admin and openai-usage org pollers — are excluded from
	// this endpoint twice over: their sources are not shippable, and cmd/tierd wires
	// them into ingester.Store(db) in-process so they never cross an HTTP boundary at
	// all. So the /events capture-outage hazard that forced validateShippedIssueID's
	// allowlist does not exist on this column, and an allowlist here would only make
	// forging as effective as honesty.
	if err := validateDeveloper(req.Developer); err != nil {
		return zero, err
	}
	if req.IdempotencyKey == "" {
		return zero, fmt.Errorf("events require idempotency_key; replay safety depends on it")
	}
	if len(req.IdempotencyKey) > maxIdentifierLen {
		return zero, fmt.Errorf("idempotency_key must be <= %d chars", maxIdentifierLen)
	}
	if math.IsNaN(req.CostUSD) || math.IsInf(req.CostUSD, 0) {
		return zero, fmt.Errorf("cost_usd must be finite")
	}
	// Magnitude cap (#118), mirroring handlePostCosts / handlePostActualSpend:
	// reject absurd totals at the trust boundary so a hostile/buggy shipper
	// can't drive DollarsToMicro past int64 range. /events is the only other
	// money-write endpoint, so it carries the same boundary reject. Upper bound
	// only; negatives are rejected by the sign check below.
	if req.CostUSD > store.MaxCostUSD {
		return zero, fmt.Errorf("cost_usd must be <= 1e12")
	}
	// Same negative-value rejection as /costs: negatives survive the
	// MAX-on-conflict upsert invisibly but skew SUM aggregates when
	// inserted under a fresh key.
	if req.InputTok < 0 || req.OutputTok < 0 || req.CacheRead < 0 ||
		req.CacheWrite5m < 0 || req.CacheWrite1h < 0 || req.CostUSD < 0 {
		return zero, fmt.Errorf("token counts and cost_usd must be >= 0")
	}
	// Bound the length BEFORE echoing it back. The old check compared against a
	// literal and quoted nothing; naming the offending source in the error (so a
	// rejected client can act on it) turns an unbounded request field into
	// unbounded response bytes, which is why every other identifier on this
	// endpoint is capped at the same boundary.
	if len(req.Source) > maxIdentifierLen {
		return zero, fmt.Errorf("source must be <= %d chars", maxIdentifierLen)
	}
	// Local-collector sources only. This was pinned to the single literal
	// "jsonl", which silently closed the central path to Codex: its cost never
	// arrived while its outcomes did, so Codex work read as FREE and inflated
	// TIER for anyone using it (#492). collector.ShippableSource is the single
	// source of truth — see its doc for why the pollers and the proxy stay out.
	if !collector.ShippableSource(req.Source) {
		return zero, fmt.Errorf("source %q is not accepted on /api/v1/events; it carries local-collector events only (%s). Proxy events are captured server-side; org-poller aggregates and manual imports use /api/v1/costs",
			req.Source, strings.Join(collector.ShippableSources(), ", "))
	}
	if req.Fidelity != "realtime" {
		return zero, fmt.Errorf(`fidelity must be "realtime" on /api/v1/events`)
	}
	if req.Timestamp == "" {
		return zero, fmt.Errorf("timestamp is required (RFC3339)")
	}
	ts, err := time.Parse(time.RFC3339, req.Timestamp)
	if err != nil {
		return zero, fmt.Errorf("timestamp must be RFC3339: %v", err)
	}
	// Bound the timestamp with /events-owned limits (#235). The value lands in
	// the ordered ts column that /scores windows on, so a far-future ts would
	// otherwise sit inside every future ?since window forever (and, with the
	// INSERT-only ts on conflict, be unpullable by a corrected re-ship). Reject
	// anything more than maxEventClockSkew ahead of server time; the RFC3339 in
	// the message is the already-parsed, canonicalised value, not raw client
	// bytes. The floor rejects a corrupt/mis-parsed clock.
	if ts.After(now.Add(maxEventClockSkew)) {
		return zero, fmt.Errorf("timestamp %s is more than %s in the future of server time; reject (client clock skew?)",
			ts.UTC().Format(time.RFC3339), maxEventClockSkew)
	}
	if y := ts.Year(); y < minEventYear {
		return zero, fmt.Errorf("timestamp year %d is before the earliest accepted event year %d", y, minEventYear)
	}
	// session_id is optional and free-form (an opaque upstream UUID), but bound its
	// length at the same trust boundary as the other identifiers so a hostile or
	// buggy shipper can't post an unbounded string. Empty is allowed -> SQL NULL.
	if len(req.SessionID) > maxIdentifierLen {
		return zero, fmt.Errorf("session_id must be <= %d chars", maxIdentifierLen)
	}
	repo, err := validateRepo(req.Repo)
	if err != nil {
		return zero, err
	}
	// Server-side pricing authority (#233): re-price the raw token counts with the
	// server's active table rather than trusting the client's cost_usd. This is the
	// fix for mixed-version-fleet unfairness — identical usage priced by whatever
	// table each laptop's binary happened to embed. cost_usd survives only as the
	// cross-check the caller compares against (see handlePostEvents). PriceVersion
	// is left 0 here so the store stamps the active version — the SAME table this
	// pricing call used — keeping cost and its provenance stamp consistent.
	//
	// ComputeCostHost, not ComputeCost, so the resolved billing_mode is KEPT (#300).
	// ComputeCost discards it, which left every shipped event at the normalized
	// default per_token — and codexrollout is the one collector that derives a
	// real billing_mode (rollout.go, from this same function). The result was that
	// one Codex session recorded self_hosted_amortized under `serve` and
	// per_token under `ship`, in a column both /export surfaces publish (#492).
	//
	// The host stays EMPTY on purpose. Only the proxy can know the host it dialed;
	// a shipped event carries a local log's client-side view, and the collector
	// itself refuses to claim one for exactly that reason. An empty host prices at
	// the model-only rate, which is what ComputeCost did — so cost is unchanged by
	// this switch, and only the discarded mode is recovered.
	cost, billingMode := store.ComputeCostHost("", req.Model, store.CostUsage{
		Input:        req.InputTok,
		Output:       req.OutputTok,
		CacheRead:    req.CacheRead,
		CacheWrite5m: req.CacheWrite5m,
		CacheWrite1h: req.CacheWrite1h,
	})
	return store.TokenEvent{
		Developer:      req.Developer,
		IssueID:        req.IssueID,
		Model:          req.Model,
		InputTok:       req.InputTok,
		OutputTok:      req.OutputTok,
		CacheRead:      req.CacheRead,
		CacheWrite5m:   req.CacheWrite5m,
		CacheWrite1h:   req.CacheWrite1h,
		CostMicro:      cost,
		BillingMode:    billingMode,
		Source:         req.Source,
		Fidelity:       req.Fidelity,
		IdempotencyKey: req.IdempotencyKey,
		Repo:           repo,
		SessionID:      req.SessionID,
		Timestamp:      ts.UTC(),
	}, nil
}

// pricingDivergenceToleranceMicro is the micro-dollar slack below which a
// client/server cost difference is treated as float-serialization noise rather
// than a real table-version divergence (#233). One micro-dollar ($0.000001) is far
// below any real per-model rate difference, so a genuine mixed-version fleet always
// clears it while JSON float round-tripping of an identical price never does.
const pricingDivergenceToleranceMicro = 1

// observePricingDivergence records a client/server cost mismatch on a STORED
// /events row (#233): serverMicro is the server's authoritative price (the value
// actually persisted); a clientMicro that differs by more than the tolerance means
// the shipper priced under a different table version. Always bumps the (nil-safe)
// per-event counter — counters are meant to move. The WARN, by contrast, is
// rate-limited to at most once per distinct (model, sign-of-skew) per process via
// pricingDivergenceSeen, so a shipper re-posting its 90-day window every ~15 min
// can't re-log the same divergence class every cycle. The WARN carries only
// numeric, non-client-controlled values — never the model, developer, or issue
// strings, per the log-safety discipline; model is used as dedup-key material only.
func (h *Handler) observePricingDivergence(model string, serverMicro, clientMicro int64) {
	delta := serverMicro - clientMicro
	diff := delta
	if diff < 0 {
		diff = -diff
	}
	if diff <= pricingDivergenceToleranceMicro {
		return
	}
	if h.pricingDivergence != nil {
		h.pricingDivergence.Inc()
	}
	// Dedup the WARN by (model, sign-of-skew): one entry per model per direction of
	// divergence, which is exactly the granularity of a mixed-version-fleet signal.
	// The key uses the raw model string as key material only — it is never logged.
	sign := "+"
	if delta < 0 {
		sign = "-"
	}
	key := model + "\x00" + sign
	if _, loaded := h.pricingDivergenceSeen.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	h.logger.Warn("events cost divergence: client cost_usd disagrees with server price; storing server price (#233)",
		"server_cost_micro", serverMicro,
		"client_cost_micro", clientMicro,
		"delta_micro", delta,
	)
}
