// Package webhook handles GitHub webhook events for TIER.
//
// Relevant events:
//   - pull_request (closed + merged) → record outcome (quality 1.0)
//   - workflow_run (completed)       → CI pass/fail signal on the merge commit;
//     a failure within 48h floors that outcome's quality to 0.7 (#134)
//   - push                           → detect reverts (default branch only); a quality revert floors
//     to 0.1, a strategic revert to 0.8, within a 60d window (#134). When
//     outcomes.push_capture is enabled (#196), a qualifying direct commit to the
//     default branch ALSO becomes a degraded (0.5) outcome, aggregated to one per
//     (issue, UTC day), so trunk-based teams aren't silently scored ~0.
//
// Quality is DERIVED, not mutated: each CI/revert signal is appended to the
// append-only quality_events log, and the affected outcome's quality is
// recomputed as the worst-of the applicable floors (internal/quality.Resolve).
// The unique (outcome_id, event_type, source_ref) key on quality_events makes
// replayed deliveries idempotent — re-deriving the same event set yields the
// same quality (this replaces the pre-#134 push-path reliance on UpdateQuality
// being an absolute assignment).
//
// Every PROCESSED delivery's raw body is also persisted (gzipped, bounded) to
// the webhook_payloads audit trail after signature validation, so a score input
// can be re-derived later (#137). Persistence is best-effort: a failure is
// logged at ERROR and never fails the delivery — the outcome write is the
// authority.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/prderive"
	"github.com/tiermetric/tier/internal/quality"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// Store is the subset of store.DB methods used by the webhook handler.
type Store interface {
	// RecordPROutcome writes a merged PR's outcome and, in the same transaction,
	// removes its merge commit from the push-capture ledger (#849): a push
	// outcome is deleted only when that commit was the last one it held, and is
	// otherwise re-owned to its earliest remaining commit. inserted=false means
	// the merge_commit_sha was already recorded (a redelivery) and nothing was
	// written. May return store.ErrWriteLockUnavailable (answered with a 503).
	RecordPROutcome(ctx context.Context, o store.Outcome) (inserted bool, err error)
	// RecordPushCommit folds one captured direct commit into its (repo, issue,
	// UTC day) push outcome (#196) and records the commit in the ledger (#849),
	// in one transaction. A commit that is a stored PR's merge commit, or that
	// the ledger already holds (a redelivery), writes nothing. pushedAt is the
	// payload's repository.pushed_at, zero when it had none (#938): it orders the
	// day row's ownership, and a zero one sorts after every push time and after
	// every commit recorded before it, so it never takes a row over. May return
	// store.ErrWriteLockUnavailable.
	RecordPushCommit(ctx context.Context, o store.Outcome, day, commitSHA string, pushedAt time.Time) (store.PushCommitStatus, error)
	OutcomeByMergeCommit(ctx context.Context, sha string) (store.Outcome, bool, error)
	// LatestOutcomeByIssue resolves the revert target within a repository (#231).
	// repo may be repoid.Unqualified; the store's match is tolerant.
	LatestOutcomeByIssue(ctx context.Context, repo, issueID string) (store.Outcome, bool, error)
	// AppendQualityEvent appends one quality_events row, idempotent on the
	// (outcome_id, event_type, source_ref) unique key (#134). inserted=false
	// means a replay hit an existing row.
	AppendQualityEvent(ctx context.Context, e store.QualityEvent) (inserted bool, err error)
	// QualityEventsForOutcome returns an outcome's event set for Resolve (#134).
	QualityEventsForOutcome(ctx context.Context, outcomeID int64) ([]store.QualityEvent, error)
	// UpdateQualityForOutcome writes the derived quality to one outcome row and
	// appends a quality_history transition (#134). Row-targeted (WHERE id = ?),
	// not issue-wide — the §C9 scoping fix.
	//
	// It takes the write lock up front (#668) and waits the store's full
	// busy_timeout for it, not the request-path cap: GitHub does not redeliver a
	// failed delivery (#863). It may still return store.ErrWriteLockUnavailable,
	// which ServeHTTP answers with 503 + Retry-After ahead of the generic 500.
	// Declared here because this handler is written against THIS interface, not
	// *store.DB — an alternate implementation has no other way to learn the
	// contract it must satisfy.
	UpdateQualityForOutcome(ctx context.Context, outcomeID int64, quality float64, reason, sourceRef string) error
	// InsertWebhookPayload persists the raw delivery body for audit (#137).
	// Called best-effort; a returned error is logged, not propagated to GitHub.
	InsertWebhookPayload(ctx context.Context, event, deliveryID string, rawBody []byte) error
}

// persistedEvents is the set of X-GitHub-Event types whose raw body we retain
// in the audit trail (#137): exactly the events the switch below processes.
// Recording ignored types (issue_comment, check_run, ...) would bloat the table
// with noise. workflow_run joined this set with P2-03 (#134) when it became a
// processed CI signal.
func persistedEvent(event string) bool {
	return event == "pull_request" || event == "push" || event == "workflow_run"
}

// PushUnattributedCounter is the metric bumped once per direct commit to the
// default branch that push-capture could not attribute to an issue (#196). A
// dropped commit must be OBSERVABLE, not silent — the handler logs it AND
// increments this counter. metrics.CounterVec satisfies it (Inc(...string)); the
// webhook package deliberately does not import internal/metrics to keep the
// dependency direction one-way (main wires the concrete counter in).
type PushUnattributedCounter interface {
	Inc(labelValues ...string)
}

// Option configures optional Handler behaviour at construction. Push-to-default-
// branch capture (#196) is OFF unless WithPushCapture is passed, so the default
// New(store, secret, logger) behaves exactly as before (reverts only on push).
type Option func(*Handler)

// WithPushCapture enables push-to-default-branch outcome capture (#196, config
// outcomes.push_capture). unattributed may be nil (capture still runs; unattributed
// commits are logged, just not counted).
func WithPushCapture(unattributed PushUnattributedCounter) Option {
	return func(h *Handler) {
		h.pushCapture = true
		h.pushUnattributed = unattributed
	}
}

// WithPushMergeLeakCounter installs the counter bumped once per default-branch
// push that carries a "Merge pull request #N" commit AND had other commits
// captured (#849). With a merge-commit merge, the PR's branch commits arrive in
// that push and are captured beside the PR's own outcome, so the work can count
// twice (#934). Counting it keeps that visible until #934 closes it. A rebase
// merge leaves no merge commit, so it is NOT seen here. nil is allowed (the WARN
// log still fires).
func WithPushMergeLeakCounter(c PushUnattributedCounter) Option {
	return func(h *Handler) {
		h.pushMergeLeak = c
	}
}

// WithPushMissingPushedAtCounter installs the counter bumped once per commit
// push capture recorded from a payload with no usable repository.pushed_at
// (#938). Such a commit keeps its credit but sorts last for its day row's
// ownership. nil is allowed.
func WithPushMissingPushedAtCounter(c PushUnattributedCounter) Option {
	return func(h *Handler) {
		h.pushMissingPushedAt = c
	}
}

// WithGeneratedPaths overrides the built-in generated/vendored deny-list used to
// exclude no-op churn from push capture (#240, config outcomes.generated_paths).
// cmd/tierd passes the resolved config value: a non-nil slice (including an explicit
// empty one, which disables exclusion) replaces defaultGeneratedPaths; when the
// config key is absent the caller does not apply this option, so New's default set
// stays in effect. See defaultGeneratedPaths for the pattern shapes.
func WithGeneratedPaths(patterns []string) Option {
	return func(h *Handler) {
		h.generatedPaths = patterns
	}
}

// WithSizeLabels overrides the built-in PR size-label → weight table (#244, config
// outcomes.size_labels). cmd/tierd passes the resolved config map when the key is
// present; prderive.NormalizeSizeLabels lowercases and trims the keys so matching
// stays case-insensitive, mirroring the label lookup. An EMPTY map (nil OR
// non-nil-empty) normalizes to nil and is a deliberate no-op — h.sizeLabels stays
// nil so prderive.SizeWeight falls back to the built-in table — so both an absent
// config key and an explicit `size_labels: {}` preserve today's behaviour exactly,
// matching what the config docs promise. The caller (config.Load) has already
// validated every weight is on the fixed 0.5/1/3/5/8 scale and that no two names
// collide once folded, so no re-validation happens here.
func WithSizeLabels(m map[string]float64) Option {
	return func(h *Handler) {
		if norm := prderive.NormalizeSizeLabels(m); norm != nil {
			h.sizeLabels = norm
		}
	}
}

// Handler processes GitHub webhook events.
type Handler struct {
	store      Store
	secret     string // HMAC-SHA256 secret; empty means fail-closed (#60)
	logger     *slog.Logger
	deliveries *deliverySet
	// pushCapture gates push-to-default-branch outcome capture (#196). When false
	// (the default), handlePush processes reverts ONLY, exactly as pre-#196.
	pushCapture bool
	// pushUnattributed counts direct commits dropped for lack of a resolvable issue
	// id (#196). nil is allowed — the drop is still logged.
	pushUnattributed PushUnattributedCounter
	// pushMergeLeak counts pushes that carry a PR merge commit and also had
	// sibling commits captured (#849, see WithPushMergeLeakCounter). nil allowed.
	pushMergeLeak PushUnattributedCounter
	// pushMissingPushedAt counts commits recorded without a usable pushed_at
	// (#938, see WithPushMissingPushedAtCounter). nil allowed.
	pushMissingPushedAt PushUnattributedCounter
	// generatedPaths is the generated/vendored deny-list used to exclude no-op churn
	// from push capture (#240). Defaults to defaultGeneratedPaths; WithGeneratedPaths
	// overrides it from config outcomes.generated_paths. Never nil after New unless an
	// explicit empty override was passed (which disables exclusion).
	generatedPaths []string
	// sizeLabels overrides the built-in size-label → weight table (#244). nil (the
	// default) means prderive falls back to its built-in default table; WithSizeLabels
	// installs a normalized (lowercased+trimmed) copy of config outcomes.size_labels
	// via prderive.NormalizeSizeLabels. Passed to prderive.SizeWeight per PR; a
	// configured match keeps weight_source='label'.
	sizeLabels map[string]float64
	// qualityMu serialises the classify→append→resolve→write critical section,
	// entered by BOTH appendAndResolve and appendAndResolveCISuccess (#134,
	// widened by #674). net/http serves deliveries concurrently and GitHub can
	// deliver in parallel; without this lock two events for the SAME outcome
	// could each resolve over a stale event set and clobber each other.
	//
	// ⚠️ THE `classify→` AND THE SECOND ENTRY POINT ARE #674 AND ARE NOT
	// COSMETIC. This comment said "the append→resolve→write critical section in
	// appendAndResolve" — one verb short and one entry point short — while the
	// ci_pass/ci_fail_flaky decision was made OUTSIDE the lock. #674 exists
	// because a comment's scope and a lock's scope disagreed, so leaving the
	// mutex's own declaration describing the narrower extent would reproduce the
	// bug at the exact spot a future editor reads before adding a caller.
	//
	// 🔴 IT IS LOAD-BEARING ON ITS OWN, and the reason matters more now than it
	// did. This used to add "the store is single-writer anyway
	// (SetMaxOpenConns(1)), so this adds no real contention" — a throughput
	// claim resting on a pool-size constant that #669 raises. The correctness
	// claim never rested on it: the critical section spans THREE separate store
	// calls (append, read, write), so no per-transaction guarantee at any pool
	// size covers it. This mutex is the only thing that does.
	//
	// ⚠️ And it excludes only other WEBHOOK goroutines. The write itself still
	// takes SQLite's write lock (#668) and can lose to a writer outside this
	// handler that holds it past busy_timeout; ServeHTTP answers that with a 503
	// rather than a 500.
	qualityMu sync.Mutex
}

// New returns a new Handler. The X-Hub-Signature-256 header is validated on
// every request; an empty secret puts the handler in fail-closed mode where
// every request is rejected (#60 — cmd/tierd doesn't even mount the route in
// that state, but the handler must not fail open if embedded elsewhere).
func New(s Store, secret string, logger *slog.Logger, opts ...Option) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if secret == "" {
		logger.Warn("TIER_WEBHOOK_SECRET is not set — webhook is fail-closed: every request will be rejected (#60)")
	}
	h := &Handler{store: s, secret: secret, logger: logger, deliveries: newDeliverySet(deliveryWindow), generatedPaths: defaultGeneratedPaths}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Fail closed (#60): without a secret there is no way to authenticate
	// GitHub, and an unauthenticated webhook lets anyone who can reach the
	// listener forge merged-PR outcomes or tank a developer's quality with
	// fake revert pushes. Pre-#60 this skipped validation entirely.
	if h.secret == "" {
		http.Error(w, "webhook secret not configured", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB limit
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	if err := verifySignature(h.secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		h.logger.Warn("webhook signature mismatch", "err", err)
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")

	// Both are HTTP headers, so neither is covered by the body HMAC. deliveryID is
	// free-form and must be sanitized wherever it is logged (#288, go/log-injection).
	// event is deliberately NOT wrapped: every site that logs it is allowlist-bounded
	// to one of {"pull_request","push","workflow_run"} (persistedEvent below, the
	// dedup guard, and the post-switch error path), so it can never carry an
	// injection - do not "fix" the asymmetry by wrapping it.

	// Audit trail (#137): persist the raw body for processed event types, AFTER
	// signature validation (so unauthenticated garbage never lands — DoS
	// hygiene) and BEFORE the delivery-dedup check (so a legitimate GitHub
	// redelivery, which reuses its GUID, still leaves its own copy — the
	// webhook_payloads index is non-unique by design). Best-effort: a persist
	// failure is logged at ERROR and processing continues, because the outcome
	// write is the authority and audit must never fail a delivery.
	if persistedEvent(event) {
		if err := h.store.InsertWebhookPayload(r.Context(), event, deliveryID, body); err != nil {
			h.logger.Error("persist webhook payload for audit", "event", event, "delivery", logSafeStr(deliveryID), "err", err)
		}
	}

	// Delivery dedup (#60): GitHub assigns each delivery a GUID and reuses
	// it on redeliveries. Checked AFTER signature validation so an
	// unauthenticated client can't churn the window, and only for event
	// types we actually process so chatty ignored subscriptions
	// (issue_comment, check_run, ...) don't shrink the effective window.
	// The key is event+GUID: GitHub GUIDs are globally unique, but the
	// composite keeps a reused GUID on a different event type (manual
	// redelivery tooling, test clients) from suppressing the wrong
	// delivery. 204 (not an error) on a duplicate so GitHub records the
	// redelivery as succeeded. A missing header (non-GitHub test client)
	// is processed without dedup — the content-level guard in handlePR
	// still applies.
	var deliveryKey string
	if deliveryID != "" && (event == "pull_request" || event == "push" || event == "workflow_run") {
		deliveryKey = event + "\x00" + deliveryID
		if !h.deliveries.firstSeen(deliveryKey) {
			h.logger.Debug("duplicate webhook delivery skipped", "event", event, "delivery", logSafeStr(deliveryID))
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	var handlerErr error
	switch event {
	case "pull_request":
		handlerErr = h.handlePR(r.Context(), body)
	case "push":
		handlerErr = h.handlePush(r.Context(), body)
	case "workflow_run":
		handlerErr = h.handleWorkflowRun(r.Context(), body)
	default:
		// Ignore other events silently. check_run is deliberately NOT handled:
		// workflow_run already reports whole-pipeline pass/fail on the merge
		// commit, so consuming check_run too would double-count one CI run.
	}

	if handlerErr != nil {
		// The GUID was recorded before we knew processing would fail —
		// forget it, or a manual redelivery of this exact delivery (same
		// GUID) would be skipped as a duplicate and the outcome silently
		// lost (#60 review RED).
		if deliveryKey != "" {
			h.deliveries.forget(deliveryKey)
		}
		// 🔴 CONTENTION IS NOT A FAULT, AND IT MUST NOT BE LOGGED AS ONE (#668).
		// Every delivery write waits the store's full busy_timeout for the
		// write lock (#849, #863), so store.ErrWriteLockUnavailable here means
		// another writer held it longer than that — a condition that clears by
		// itself. A single-statement write that times out on the lock returns a
		// raw SQLITE_BUSY rather than the sentinel and so takes the 500 branch.
		// Both branches below return 5xx and GitHub redelivers NEITHER
		// automatically, so the delivery is lost unless an operator redelivers
		// it by hand, either way (docs/webhook-setup.md#redelivering-a-failed-delivery);
		// what differs is the OPERATOR signal.
		// An ERROR line saying "webhook handler error" points a human at a
		// broken database for something that resolved on its own, which is the
		// exact defect writeStoreContention exists to prevent one package over.
		//
		// 503 + Retry-After rather than 500: it is the honest status for a
		// transient lock conflict. GitHub does not act on Retry-After; a manual
		// redelivery or a non-GitHub client can use it.
		//
		// ⚠️ `event` IS logSafeStr-WRAPPED, and that is not cosmetic: it is the
		// raw X-GitHub-Event REQUEST HEADER, so an attacker controls it and an
		// unwrapped value is a log-injection sink. CodeQL flagged exactly this
		// line (go/log-injection) when it was first written bare. logSafeStr is
		// a measured CodeQL barrier — wrapping is the fix, NOT a dismissal.
		if errors.Is(handlerErr, store.ErrWriteLockUnavailable) {
			h.logger.Warn("webhook handler: write lock unavailable; the delivery was not fully processed and GitHub does not redeliver it automatically — redeliver it from the webhook's Recent Deliveries",
				"event", logSafeStr(event), "delivery", logSafeStr(deliveryID), "err", logsafe.Err(handlerErr))
			w.Header().Set("Retry-After", "1")
			http.Error(w, "database is busy: retry shortly", http.StatusServiceUnavailable)
			return
		}
		// Wrapped for the same reason as the branch above. This line predates
		// #668 and its bare form is one of the adjudicated-false-positive
		// alerts; it is corrected here rather than left, because leaving one
		// wrapped and one bare on adjacent lines invites a future reader to
		// "fix" the inconsistency in the wrong direction.
		h.logger.Error("webhook handler error", "event", logSafeStr(event), "delivery", logSafeStr(deliveryID), "err", logsafe.Err(handlerErr))
		// 500 marks the delivery failed in GitHub's Recent Deliveries; GitHub
		// does not redeliver it automatically (see the 503 branch above).
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deliveryWindow bounds the dedup set. GitHub never redelivers on its own, so
// the window only has to catch duplicate POSTs and near-term manual
// redeliveries; delivery volume is low, and 1024 GUIDs covers that with room to
// spare while capping memory at ~40 KB.
const deliveryWindow = 1024

// deliverySet remembers recently-seen X-GitHub-Delivery GUIDs so a
// redelivered webhook (manual redelivery, duplicate POST) is
// processed exactly once. Fixed-size ring: the newest entry overwrites the
// oldest, so memory is bounded without timer bookkeeping. In-memory only —
// a restart forgets the window, which is why handlePR keeps a durable
// content-level guard on merge_commit_sha as the backstop.
type deliverySet struct {
	mu   sync.Mutex
	seen map[string]struct{}
	ring []string
	next int
}

func newDeliverySet(n int) *deliverySet {
	return &deliverySet{seen: make(map[string]struct{}, n), ring: make([]string, n)}
}

// firstSeen records id and reports whether this is its first sighting.
// Eviction is FIFO by first sighting, not LRU: a duplicate sighting does
// not refresh an entry's position, so even a frequently-redelivered GUID
// ages out after deliveryWindow NEW GUIDs, after which the durable
// content-level guards are the backstop; don't assume hot entries are protected.
func (d *deliverySet) firstSeen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.seen[id]; dup {
		return false
	}
	if old := d.ring[d.next]; old != "" {
		delete(d.seen, old)
	}
	d.ring[d.next] = id
	d.next = (d.next + 1) % len(d.ring)
	d.seen[id] = struct{}{}
	return true
}

// forget removes id so a failed handler doesn't suppress a manual redelivery
// of the same delivery. The ring slot is cleared too: leaving the stale string
// behind would, after id is re-admitted into a NEW slot, let the old slot's
// eventual eviction delete the live map entry.
func (d *deliverySet) forget(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; !ok {
		return
	}
	delete(d.seen, id)
	for i, v := range d.ring {
		if v == id {
			d.ring[i] = ""
			break
		}
	}
}

// verifySignature validates a GitHub HMAC-SHA256 webhook signature.
// sigHeader is expected to be in the form "sha256=<hex>".
func verifySignature(secret string, body []byte, sigHeader string) error {
	after, ok := strings.CutPrefix(sigHeader, "sha256=")
	if !ok {
		return errors.New("missing sha256= prefix in X-Hub-Signature-256")
	}
	sig, err := hex.DecodeString(after)
	if err != nil {
		return fmt.Errorf("invalid hex in signature: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}

// --- PR event ---

type prPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number         int             `json:"number"`
		Title          string          `json:"title"`
		Body           string          `json:"body"`
		Merged         bool            `json:"merged"`
		MergedAt       json.RawMessage `json:"merged_at"`
		MergeCommitSHA string          `json:"merge_commit_sha"`
		Head           struct {
			Ref string `json:"ref"`
		} `json:"head"`
		User struct {
			Login string `json:"login"`
			// Type is the GitHub account type ("User", "Bot", ...). Stored on the
			// outcome so a bot author never counts toward the k-anonymity floor
			// (#856); the Copilot coding agent authors as login "Copilot", type
			// "Bot", with no [bot] suffix to recognise it by.
			Type string `json:"type"`
		} `json:"user"`
		Labels       []prLabel `json:"labels"`
		Additions    int       `json:"additions"`
		Deletions    int       `json:"deletions"`
		ChangedFiles int       `json:"changed_files"`
	} `json:"pull_request"`
	// Repository.FullName ("Owner/Repo") qualifies the issue id (#231). GitHub sends
	// it on every delivery; it was simply never parsed before. Canonicalized through
	// repoid so it agrees byte-for-byte with the collector's remote.origin.url.
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// prLabel matches the label struct used in the GitHub PR payload.
type prLabel struct {
	Name string `json:"name"`
}

// time.Parse accepts out-of-range offsets and some non-RFC3339 syntax.
// Check syntax and offset ranges first; parsing still validates the date/time.
var mergedAtRFC3339RE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func (h *Handler) handlePR(ctx context.Context, body []byte) error {
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("parse PR payload: %w", err)
	}
	if p.Action != "closed" || !p.PullRequest.Merged {
		return nil
	}

	issueID := issueref.FromBranchOrBody(p.PullRequest.Head.Ref, p.PullRequest.Body)
	if issueID == "" {
		h.logger.Debug("PR merged but no issue ID found", "pr", p.PullRequest.Number)
		return nil
	}

	// Multi-issue attribution rule (#189): a PR that closes several issues
	// ("closes #a, #b") is attributed to the single PRIMARY issue (issueID above,
	// the leftmost close directive or the branch id). One outcome per merged PR is
	// mandatory — outcomes.merge_commit_sha is UNIQUE (#60) — so the secondaries
	// cannot each get their own row; log them so they are observable, not silent.
	//
	// `closed` is logged bare (NOT via logSafeStr) and is provably non-injectable:
	// issueref.ClosedIssues emits only "issue-"+digits (hashNumRE = `#([1-9]\d*)`),
	// so every element is `issue-<digits>` with no control chars and cannot forge a
	// log record (#288). This is the 4th deliberately-bare sink, alongside m[1]
	// (regex `[0-9a-f]{40}`), issueID (issueref-constrained), and event (allowlist-
	// bounded); do not "fix" the asymmetry by wrapping it.
	if closed := issueref.ClosedIssues(p.PullRequest.Body); len(closed) > 1 {
		h.logger.Info("PR closes multiple issues; outcome attributed to the primary only (#189)",
			"pr", p.PullRequest.Number, "primary", issueID, "closed", closed)
	}

	// Record which scale produced the weight (#132): a label-derived weight and
	// a heuristic-derived one live on the same 0.5-8 range now, but a report
	// still needs to distinguish them (a developer whose points are 100%
	// heuristic has no size labels and less trustworthy weights). The raw diff
	// stats are persisted too so a future recalibration CAN re-score — the old
	// heuristic discarded them, so pre-#132 rows are unrecoverable ('legacy').
	// store.ResolveWeight is the shared branching the provider-neutral
	// POST /api/v1/outcomes also uses (#188): a non-zero label weight wins
	// (source 'label'), else the diff-size heuristic (source 'git-heuristic'),
	// so an outcome scores identically whether it arrived by webhook or that
	// endpoint. prderive.SizeWeight resolves the label against the operator's
	// configured size-label table (h.sizeLabels from outcomes.size_labels, #244) or
	// the built-in defaults when that is nil, and returns 0 when no size label
	// matches — exactly the "fall through to heuristic" input ResolveWeight expects.
	// Only the label NAMES are configurable; the weight scale and weight_source='label'
	// provenance are unchanged.
	//
	// GENERATED-FILE EXCLUSION (#240, audit C4) is NOT applied here. The heuristic
	// weighs aggregate additions/deletions/changed_files, and the pull_request
	// payload carries no per-file list — TIER makes no outbound GitHub API call and
	// keeps no local clone by design (docs/how-it-works.md) — so generated churn
	// cannot be attributed to individual files and subtracted. The diff-size
	// heuristic is a FALLBACK PROXY for unlabeled PRs, not a measure of value: a
	// size label is the operator's own judgment and the defensible weight source.
	// Generated-file exclusion is applied only where the webhook has real per-file
	// data: the push-capture path (see defaultGeneratedPaths).
	//
	// names is extracted ONCE and fed to both derivations (#301): the shared prderive
	// helpers take []string, so this avoids re-walking the label slice per signal.
	names := labelNames(p.PullRequest.Labels)
	weight, weightSource := store.ResolveWeight(prderive.SizeWeight(names, h.sizeLabels),
		p.PullRequest.Additions, p.PullRequest.Deletions, p.PullRequest.ChangedFiles)

	// Work-type category from the PR labels (#187): a security / incident / research
	// PR is scored WITHIN its category, not against feature work.
	// prderive.WorkTypeFromLabels falls back to feature/default when no type label is
	// present, exactly as the size-label weight falls through to the heuristic. It is
	// the shared derivation the backfill path also calls (#301), so a PR's work_type
	// is identical however it was ingested.
	workType, workTypeSource := prderive.WorkTypeFromLabels(names)

	// A login spelled like the unattributed sentinel (#619) would merge its author
	// into the pseudo-developer pool, which never counts as a person (#853, #856)
	// and absorbs spend that no one owns. The outcome is not recorded, as
	// POST /api/v1/outcomes refuses the same developer.
	if store.ResemblesUnattributed(p.PullRequest.User.Login) {
		h.logger.Warn("PR author login is reserved for the unattributed pseudo-developer; outcome not recorded (#619)",
			"pr", logSafeStr(fmt.Sprint(p.PullRequest.Number)))
		return nil
	}

	// Use the merge time for reporting and observation windows even on a late
	// delivery. Missing, malformed, zero or future times fall back to receipt.
	outcomeTS := time.Now().UTC()
	var mergedAt string
	if json.Unmarshal(p.PullRequest.MergedAt, &mergedAt) == nil && mergedAtRFC3339RE.MatchString(mergedAt) {
		if ts, err := time.Parse(time.RFC3339, mergedAt); err == nil && !ts.IsZero() && !ts.After(outcomeTS) {
			outcomeTS = ts.UTC()
		}
	}

	o := store.Outcome{
		Developer:      p.PullRequest.User.Login,
		IssueID:        issueID,
		PRNumber:       p.PullRequest.Number,
		Weight:         weight,
		WeightSource:   weightSource,
		Quality:        1.0,
		MergeCommitSHA: p.PullRequest.MergeCommitSHA,
		Additions:      p.PullRequest.Additions,
		Deletions:      p.PullRequest.Deletions,
		ChangedFiles:   p.PullRequest.ChangedFiles,
		WorkType:       workType,
		WorkTypeSource: workTypeSource,
		Repo:           canonicalRepo(p.Repository.FullName),
		AuthorType:     p.PullRequest.User.Type,
		Timestamp:      outcomeTS,
	}
	// Durable replay guard (#60) and push reconciliation (#849), in ONE store
	// transaction. The HMAC covers only the body, so a captured request can be
	// replayed with a fresh X-GitHub-Delivery header past the in-memory dedup;
	// the merge_commit_sha unique index makes that replay write nothing. The
	// same transaction removes the merge commit from the push ledger, so a
	// squash merge counts once whichever of its push and pull_request events
	// arrives first — and two concurrent deliveries cannot both insert.
	inserted, err := h.store.RecordPROutcome(ctx, o)
	if err != nil {
		return err
	}
	if !inserted {
		h.logger.Debug("outcome already recorded for merge commit, skipping",
			"sha", logSafeStr(o.MergeCommitSHA), "pr", logSafeStr(fmt.Sprint(p.PullRequest.Number)))
	}
	return nil
}

// canonicalRepo canonicalizes a GitHub `repository.full_name` for storage (#231).
// An absent or malformed value degrades to the 'unqualified' sentinel rather than
// failing the delivery: a webhook we cannot repo-qualify is still a real outcome,
// and dropping it would lose the numerator entirely. The tolerant join then treats
// it exactly as pre-#231 rows are treated.
func canonicalRepo(fullName string) string {
	if slug, ok := repoid.Canonical(fullName); ok {
		return slug
	}
	return repoid.Unqualified
}

// labelNames projects the GitHub PR label payload onto the plain []string of label
// names the shared prderive derivation consumes (#301). The webhook and backfill
// paths carry different label struct shapes but both reduce to a list of names, so
// the shared package accepts the least-specific input both can supply.
func labelNames(labels []prLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// gitHeuristic maps diff size onto the SAME 0.5/1/3/5/8 scale the size-label table uses,
// so unlabeled PRs are commensurate with labeled ones (#132/C1). The bucket
// logic itself now lives in store.GitHeuristic — the single source of truth the
// provider-neutral POST /api/v1/outcomes shares (#188) so the two ingestion
// paths cannot drift. This wrapper preserves the webhook's internal call site
// and unit test; see store.GitHeuristic for the bucket rationale and the #132
// history (the old log2 formula this replaced).
func gitHeuristic(linesChanged, filesChanged int) float64 {
	return store.GitHeuristic(linesChanged, filesChanged)
}

// defaultGeneratedPaths is the built-in deny-list of generated / vendored path
// patterns whose churn carries no engineering outcome (#240, audit finding C4). It
// gates the push-capture path only: a direct-to-default-branch commit whose EVERY
// changed file matches one of these — a regenerated protobuf, a `go mod vendor`
// sync, a lockfile bump — is excluded so it cannot earn a spurious degraded (0.5)
// outcome. Overridable via config outcomes.generated_paths (WithGeneratedPaths).
//
// This does NOT touch the PR weight path. The pull_request webhook payload carries
// only aggregate additions/deletions/changed_files — no per-file list — and TIER
// makes no outbound GitHub API call and keeps no local clone by design (see
// docs/how-it-works.md). So the diff-size heuristic cannot attribute lines to
// individual files to subtract generated churn; the honest mitigation there is the
// label path, which C4 documents as the defensible weight source.
//
// Pattern shapes (kept deliberately simple — not a full glob engine):
//   - trailing "/"  → directory-segment prefix, matched at the repo root OR any
//     nested segment ("vendor/", "node_modules/")
//   - leading "*"   → filename suffix ("*.pb.go", "*_generated.go", "*.gen.go")
//   - otherwise     → exact basename anywhere in the tree ("go.sum", lockfiles)
var defaultGeneratedPaths = []string{
	"vendor/",
	"node_modules/",
	"*.pb.go",
	"*_generated.go",
	"*.gen.go",
	"go.sum",
	"package-lock.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"Cargo.lock",
	"poetry.lock",
	"Gemfile.lock",
	"composer.lock",
}

// isGeneratedPath reports whether a repo-relative, forward-slash path matches any
// deny-list pattern (#240). path is client-controlled (a webhook payload field) and
// must never be logged unquoted. Matching is case-sensitive: GitHub paths are
// case-exact and the deny-list entries are lowercase by convention.
func isGeneratedPath(path string, patterns []string) bool {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	for _, p := range patterns {
		switch {
		case strings.HasSuffix(p, "/"):
			if strings.HasPrefix(path, p) || strings.Contains(path, "/"+p) {
				return true
			}
		case strings.HasPrefix(p, "*"):
			if strings.HasSuffix(path, p[1:]) {
				return true
			}
		default:
			if base == p {
				return true
			}
		}
	}
	return false
}

// allGenerated reports whether EVERY path is generated/vendored (#240). Empty input
// is false: a commit with no listed files is never treated as all-generated (the
// caller also gates on len>0), so the exclusion can only ever REMOVE a spurious
// outcome, never suppress a real one when the payload omits file data.
func allGenerated(paths, patterns []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !isGeneratedPath(p, patterns) {
			return false
		}
	}
	return true
}

// logSafeStr renders a client-controlled string for a structured log, stripping
// CR/LF and quoting the remainder (#321, CodeQL go/log-injection).
//
// A push payload's commit id, ref, subject, and file paths are attacker-controlled
// JSON — GitHub validates a real delivery's signature, but the fields inside are
// never charset-constrained by us. Logging such a value verbatim lets a caller
// embed a newline and forge a log record ("<sha>\ntime=... level=ERROR msg=...")
// that an operator, or a line-oriented SIEM, reads as genuine.
//
// The barrier itself lives in the shared internal/logsafe package so the webhook,
// API, proxy, and tierd access-log sinks all sanitize identically (#321): it
// STRIPS "\r"/"\n", then %q-quotes to escape any remaining control bytes, quotes,
// and invalid UTF-8, and caps the length. This wrapper is retained so the
// handler's many call sites stay unchanged.
//
// See logsafe's package doc for why the strip is the primary barrier and %q the
// backstop — the reasoning is subtler than it looks, and this comment previously
// stated it wrongly.
func logSafeStr(s string) string {
	return logsafe.Str(s)
}

// --- workflow_run event (CI pass/fail signals, #134) ---

// Observation windows (quality-degradation-spec §2). CI signals lock in Phase 1
// (48h); reverts are accepted through the full Phase 2 window (60d). A signal
// arriving after its window is a stale/latent event and is ignored — the
// original author is not penalised for a break surfacing months later.
const (
	ciObservationWindow     = 48 * time.Hour
	revertObservationWindow = 60 * 24 * time.Hour
	// flakyRerunWindow is the "30-minute re-run rule" (spec §3 Event 2): a
	// success that RE-RAN an earlier failure — same merge commit, same
	// workflow, strictly later run_attempt (#687) — within this span
	// reclassifies that failure as flaky. The window alone is not the rule;
	// see isFlakyRerunLocked for all three conditions.
	flakyRerunWindow = 30 * time.Minute
)

// workflowRunPayload is the subset of the GitHub workflow_run webhook we use.
// Only whole-pipeline conclusions on the merge commit's default-branch run
// carry a quality signal; every other field of the (large) payload is ignored.
type workflowRunPayload struct {
	Action      string `json:"action"` // process only "completed"
	WorkflowRun struct {
		HeadSHA    string    `json:"head_sha"`
		HeadBranch string    `json:"head_branch"`
		Conclusion string    `json:"conclusion"` // "success" | "failure" | others ignored
		RunAttempt int       `json:"run_attempt"`
		UpdatedAt  time.Time `json:"updated_at"` // event timestamp for window math
		// WorkflowID identifies WHICH workflow ran (#687). Flaky matching is
		// per-workflow: without it, a green docs run neutralises a red CI run on
		// the same commit. The numeric id, not `name`: the name is mutable
		// (a rename would orphan every event already written), user-controlled,
		// and may contain the colon that delimits source_ref. `name` is
		// deliberately NOT parsed — see BuildCISourceRef.
		WorkflowID int64 `json:"workflow_id"`
	} `json:"workflow_run"`
	// No FullName here on purpose (#231): handleWorkflowRun resolves its target via
	// OutcomeByMergeCommit(HeadSHA), and a merge-commit SHA is globally unique, so
	// the workflow_run path needs no repository qualifier. Parsing a field we never
	// read would be a comment that lies.
	Repository struct {
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

// handleWorkflowRun consumes a completed CI run on the merge commit and appends
// the corresponding quality event (#134, Events 1-2). It processes only:
//   - Action == "completed"
//   - HeadBranch == Repository.DefaultBranch (the spec's "merge commit SHA on
//     the target branch" — CI on unmerged PR heads is the common noise case)
//   - Conclusion ∈ {success, failure}
//
// A failure appends ci_fail (floor 0.7). A success appends ci_fail_flaky (which
// neutralises the failure in Resolve) only when it is a RE-RUN OF THE FAILING
// WORKFLOW — same head SHA, same workflow_id, a strictly later run_attempt,
// inside flakyRerunWindow (#687); any other success appends ci_pass (audit
// only). Signals more than 48h after merge, or for a head_sha with no recorded
// outcome, are ignored.
func (h *Handler) handleWorkflowRun(ctx context.Context, body []byte) error {
	var p workflowRunPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("parse workflow_run payload: %w", err)
	}
	wr := p.WorkflowRun
	if p.Action != "completed" {
		return nil
	}
	// Only the merge commit's run on the default branch is a merged-code signal.
	if wr.HeadBranch == "" || wr.HeadBranch != p.Repository.DefaultBranch {
		return nil
	}
	if wr.Conclusion != "success" && wr.Conclusion != "failure" {
		return nil
	}

	origin, found, err := h.store.OutcomeByMergeCommit(ctx, wr.HeadSHA)
	if err != nil {
		return fmt.Errorf("workflow_run: lookup outcome by head sha: %w", err)
	}
	if !found {
		// CI runs for unmerged branches / PR heads never match a merge commit —
		// the common case, not an error.
		h.logger.Debug("workflow_run for unrecorded merge commit, ignoring", "sha", logSafeStr(wr.HeadSHA))
		return nil
	}

	eventTS := wr.UpdatedAt
	if eventTS.IsZero() {
		eventTS = time.Now().UTC()
	}
	if eventTS.After(origin.Timestamp.Add(ciObservationWindow)) {
		h.logger.Debug("workflow_run outside 48h observation window, ignoring",
			"sha", logSafeStr(wr.HeadSHA), "merged_at", origin.Timestamp, "event_ts", eventTS)
		return nil
	}

	// source_ref = head_sha:run_attempt:workflow_id makes each attempt of each
	// workflow a distinct event (so a retry inserts rather than colliding, and
	// two workflows failing on one commit no longer collapse into one row) while
	// carrying the (head SHA, workflow) identity Resolve matches ci_fail against
	// ci_fail_flaky on. The encoding lives in internal/quality next to its
	// parser so the two cannot drift.
	// Both identity fields are warned about rather than fatal: dropping a real
	// CI signal is worse than recording a weakly-identified one. But each
	// disables a DIFFERENT one of the three flaky conditions, and each fails
	// SILENTLY — the whole package stays green, no test notices, and quality
	// drifts. A gate that never fires looks exactly like a gate with nothing to
	// catch, so both say so out loud.
	if wr.WorkflowID <= 0 {
		// GitHub always sends workflow_id on workflow_run. Without it the event
		// gets the legacy unidentified ref, which pairs only with other
		// unidentified refs.
		h.logger.Warn("workflow_run without workflow_id: this event has no workflow identity, so the per-workflow flaky match (#687) cannot apply to it",
			"sha", logSafeStr(wr.HeadSHA), "conclusion", logSafeStr(wr.Conclusion))
	}
	if wr.RunAttempt <= 0 {
		// The worse of the two, and the reason it gets its own line. GitHub
		// numbers attempts from 1. If the field ever vanished, EVERY event would
		// parse at attempt 0, `success.RunAttempt > failed.RunAttempt` would be
		// unsatisfiable, and NOTHING would ever be classified flaky again —
		// quality biased permanently downward with nothing failing anywhere.
		h.logger.Warn("workflow_run without a positive run_attempt: the strictly-later-attempt condition of the flaky re-run rule cannot be satisfied for this event",
			// ⚠️ A DERIVED BOOLEAN, NOT THE RAW VALUE. CodeQL raises go/log-injection
			// on wr.RunAttempt here. It is an `int` set by encoding/json, so it can
			// only hold a number and a number cannot carry the newline injection
			// needs — a true false positive, and the same class as 8 of the 16
			// already dismissed on this repo. It was dismissed once; changing the
			// code is STRICTLY BETTER, because a dismissal is a standing claim that
			// requires the next reader to trust an argument, while code that raises
			// no alert requires trusting nothing.
			// The diagnostic that mattered is preserved: this branch already means
			// "not positive", so the only remaining question is whether the field was
			// ABSENT (0) or MALFORMED (negative) — which this boolean answers.
			"sha", logSafeStr(wr.HeadSHA), "run_attempt_negative", wr.RunAttempt < 0)
	}
	sourceRef := quality.BuildCISourceRef(wr.HeadSHA, wr.RunAttempt, wr.WorkflowID)

	// A success is classified INSIDE qualityMu (#674): the ci_pass vs
	// ci_fail_flaky decision reads the same event set the append then mutates,
	// so it belongs in the same critical section. A failure needs no read and
	// takes the fixed-type path.
	//
	// ⚠️ STILL A `switch` WITH AN EXPLICIT `default`, NOT `if success { … } else
	// ci_fail`. The conclusion guard 30-odd lines up already returns for
	// anything but success/failure, so the else-branch is provably `failure`
	// TODAY — but the guard and the classifier can drift apart with no compile
	// or runtime signal, and an if/else would then silently record a `cancelled`
	// or `timed_out` run as ci_fail, i.e. a real 0.7 quality penalty from a run
	// that never failed. The old switch failed LOUD in that case (empty
	// eventType → AppendQualityEvent rejects it); this keeps that posture and
	// costs nothing.
	switch wr.Conclusion {
	case "success":
		return h.appendAndResolveCISuccess(ctx, origin, sourceRef, eventTS)
	case "failure":
		return h.appendAndResolve(ctx, origin, quality.EventCIFail, sourceRef, eventTS)
	default:
		return fmt.Errorf("workflow_run: unclassifiable conclusion %q reached the classifier — the guard above and this switch have drifted", wr.Conclusion)
	}
}

// isFlakyRerunLocked reports whether the CI success described by success (a
// parsed source_ref) at successTS is a re-run of an existing ci_fail — the spec
// §3 Event 2 "30-minute re-run" rule. A read error is surfaced (the caller
// aborts) rather than silently treated as not-flaky, so a transient store
// failure doesn't mis-classify a genuine flake as a clean pass.
//
// THREE conditions, all required (#687):
//
//	same CIKey       — same merge commit AND same workflow. Head SHA alone was
//	                   the whole match until #687, which made any green workflow
//	                   on the commit erase any red one.
//	later attempt    — the issue's optional tightening, TAKEN. GitHub increments
//	                   run_attempt for a re-run of a run and keeps the run id, so
//	                   a genuine re-run always has a strictly higher attempt than
//	                   the failure it re-ran. Requiring it costs no legitimate
//	                   case and rejects the one shape same-workflow matching
//	                   still admits: two INDEPENDENT runs of one workflow on one
//	                   commit (a push run and a scheduled run, both attempt 1),
//	                   where the green one is not a re-run of the red one.
//	inside the window — 0 <= successTS - failTS <= flakyRerunWindow.
//
// 🔴 THE CALLER MUST HOLD qualityMu (#674). This reads the outcome's event set
// to make a decision that the append immediately following it depends on, so the
// read and the append have to be one critical section. It ran OUTSIDE the lock
// until 2026-08-14, which made appendAndResolve's doc comment wider than the
// code: the doc promised the whole append→read→resolve→write was atomic with
// respect to other quality mutations, while the DECISION about what to append
// was made against an unprotected read. Two concurrent same-outcome deliveries
// could each classify against the same pre-append state.
//
// The `Locked` suffix is the contract: there is no unlocked caller, and adding
// one reintroduces the bug silently — nothing in the type system enforces this.
//
// Order assumption: classification happens at append time and depends on the
// failure ALREADY being persisted when the success arrives. This holds for the
// normal timeline (a re-run success follows its failure). If GitHub delivered
// the success before the failure (rare out-of-order redelivery), the success is
// recorded as ci_pass and the later failure stands at 0.7. Making this
// order-independent is a Phase-2 refinement, not required here.
//
// ⛔ AND IF YOU DO IT, DO NOT DO IT THE WAY THIS COMMENT USED TO SUGGEST. It
// said "neutralise inside Resolve when any same-SHA success exists within 30
// min of a failure". That is #687 written down as a plan: ANY same-SHA success
// is precisely the match that let a green docs run erase a red CI run. An
// order-independent rule has to carry the same three conditions as the
// classifier below — same CIKey, strictly later run_attempt, inside the window
// — and it would have to reach Resolve through ci_pass rows, which Resolve
// currently ignores entirely.
func (h *Handler) isFlakyRerunLocked(ctx context.Context, outcomeID int64, success quality.CIRef, successTS time.Time) (bool, error) {
	events, err := h.store.QualityEventsForOutcome(ctx, outcomeID)
	if err != nil {
		return false, fmt.Errorf("workflow_run: read events for flaky check: %w", err)
	}
	for _, e := range events {
		if e.EventType != quality.EventCIFail {
			continue
		}
		failed := quality.ParseCIRef(e.SourceRef)
		if failed.CIKey != success.CIKey || success.RunAttempt <= failed.RunAttempt {
			continue
		}
		delta := successTS.Sub(e.EventTS)
		if delta >= 0 && delta <= flakyRerunWindow {
			return true, nil
		}
	}
	return false, nil
}

// appendAndResolve records one quality event against origin, then recomputes the
// outcome's quality from its FULL event set and writes it row-targeted (#134).
//
// The whole append→read→resolve→write is done under qualityMu so it is atomic
// with respect to any other quality mutation: each write therefore resolves over
// the latest committed event set, and two concurrent same-outcome deliveries
// cannot clobber each other with stale resolves.
//
// ✅ #674: that promise is now TRUE OF THE CLASSIFICATION TOO. The ci_pass vs
// ci_fail_flaky decision used to be made by the caller outside this lock, so
// this comment described a critical section wider than the code implemented.
// The fix was to widen the CODE (appendAndResolveCISuccess), not to narrow this
// comment — a comment trimmed to match a weaker guarantee makes the next reader
// trust less than they should without telling them why.
//
// Replay / crash safety: it recomputes and reconciles unconditionally, even when
// AppendQualityEvent reports inserted=false (a unique-key replay). The event set
// is the authority; re-deriving over it is idempotent, and if a prior delivery
// crashed after appending its event but before writing quality, this reconciles
// the stored value. UpdateQualityForOutcome re-reads the current quality inside
// its own transaction and no-ops when it already equals the derived value, so a
// pure replay writes nothing (no duplicate quality_history row) while a
// genuinely-stale outcome is healed.
func (h *Handler) appendAndResolve(ctx context.Context, origin store.Outcome, eventType, sourceRef string, eventTS time.Time) error {
	h.qualityMu.Lock()
	defer h.qualityMu.Unlock()
	return h.appendAndResolveLocked(ctx, origin, eventType, sourceRef, eventTS)
}

// appendAndResolveCISuccess classifies a CI success as ci_pass or ci_fail_flaky
// and appends it — both under ONE acquisition of qualityMu (#674).
//
// This exists because the classification is a read-then-decide over exactly the
// event set the append then mutates. Doing it in the caller left a window where
// two concurrent deliveries for the same outcome both read the pre-append state.
// qualityMu is a sync.Mutex and is NOT reentrant, so the classify step cannot
// simply call appendAndResolve — hence the split into ...Locked.
//
// ⚠️ The pre-append read here is deliberate and is NOT the one appendAndResolve
// does after appending. The event type has to be decided BEFORE the event is
// written, because the type IS what gets written; the post-append read then
// resolves over the full set including it. Two reads, two different questions.
// Only the CI-success path pays for the extra read — a failure needs no
// classification and keeps the single-read path. Measured 2026-08-14: the
// relocated read is 14.5 µs, index-served (idx_quality_events_uq leads on
// outcome_id), and grows the critical section ~140 µs → ~155 µs.
//
// ⚠️ The classification key is DERIVED from sourceRef, never passed alongside
// it. They overlap by construction (sourceRef IS the encoded ref), and two
// adjacent parameters that must agree is a transposition waiting to happen — one
// that compiles and then silently classifies every success against the wrong
// commit or workflow, i.e. as ci_pass.
func (h *Handler) appendAndResolveCISuccess(ctx context.Context, origin store.Outcome, sourceRef string, eventTS time.Time) error {
	h.qualityMu.Lock()
	defer h.qualityMu.Unlock()

	flaky, err := h.isFlakyRerunLocked(ctx, origin.ID, quality.ParseCIRef(sourceRef), eventTS)
	if err != nil {
		return err
	}
	eventType := quality.EventCIPass
	if flaky {
		eventType = quality.EventCIFailFlaky
	}
	return h.appendAndResolveLocked(ctx, origin, eventType, sourceRef, eventTS)
}

// appendAndResolveLocked is appendAndResolve's body. 🔴 THE CALLER MUST HOLD
// qualityMu. It is separate only because sync.Mutex is not reentrant and
// appendAndResolveCISuccess needs to classify inside the same critical section.
func (h *Handler) appendAndResolveLocked(ctx context.Context, origin store.Outcome, eventType, sourceRef string, eventTS time.Time) error {
	inserted, err := h.store.AppendQualityEvent(ctx, store.QualityEvent{
		OutcomeID: origin.ID,
		Developer: origin.Developer,
		IssueID:   origin.IssueID,
		EventType: eventType,
		SourceRef: sourceRef,
		EventTS:   eventTS,
	})
	if err != nil {
		return fmt.Errorf("append quality event %q: %w", eventType, err)
	}
	if !inserted {
		h.logger.Debug("quality event already recorded (replay); reconciling from event set",
			"outcome", origin.ID, "event_type", eventType, "source_ref", logSafeStr(sourceRef))
	}

	events, err := h.store.QualityEventsForOutcome(ctx, origin.ID)
	if err != nil {
		return fmt.Errorf("read quality events for outcome %d: %w", origin.ID, err)
	}
	q := quality.Resolve(events)
	if err := h.store.UpdateQualityForOutcome(ctx, origin.ID, q, eventType, sourceRef); err != nil {
		return fmt.Errorf("update quality for outcome %d: %w", origin.ID, err)
	}
	return nil
}

// --- Push event (revert detection) ---

type pushPayload struct {
	// Ref is the fully-qualified ref the push updated, e.g. "refs/heads/main".
	// Revert detection and push capture (#196) process only pushes to the
	// default branch, comparing Ref against Repository.DefaultBranch below.
	Ref     string `json:"ref"`
	Commits []struct {
		ID        string    `json:"id"` // commit SHA — surfaced in slog.Info when no issue id can be derived, and used as the revert event's source_ref
		Message   string    `json:"message"`
		Timestamp time.Time `json:"timestamp"` // commit time — event timestamp for the 60d window (revert) and the UTC aggregation day (#196 capture)
		Author    struct {
			Username string `json:"username"`
		} `json:"author"`
		// Added/Removed/Modified are the commit's changed-file paths (repo-relative,
		// forward-slash). Push capture (#240) uses them to exclude a commit whose every
		// file is generated/vendored. Client-controlled: never log a path unquoted.
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Repository struct {
		DefaultBranch string `json:"default_branch"`
		// FullName ("Owner/Repo") qualifies the issue id (#231).
		FullName string `json:"full_name"`
		// PushedAt orders push-row ownership (#938). Kept raw so a malformed value
		// cannot fail the payload's parse (and with it revert handling); see
		// parsePushedAt.
		PushedAt json.RawMessage `json:"pushed_at"`
	} `json:"repository"`
}

// parsePushedAt reads repository.pushed_at: an integer of Unix seconds (the
// push event's documented shape, octokit/webhooks push payload example) or an
// RFC 3339 string (the schema's other branch). Anything else — absent, null, a
// fraction, a non-positive value — is not usable, and the caller passes a zero
// time rather than substituting the server's clock.
func parsePushedAt(raw json.RawMessage) (time.Time, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil || t.Unix() <= 0 {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	secs, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

// revertLineRE matches the first line of revert commits:
//
//	Revert "feat: do something"
//	Revert feat: do something
var revertLineRE = regexp.MustCompile(`(?i)^revert\s+["']?(.+?)["']?\s*$`)

// revertsCommitRE matches the standard footer `git revert <sha>` adds to its
// auto-generated commit message:
//
//	This reverts commit abcdef0123456789abcdef0123456789abcdef01.
//
// The capture group is lowercase-only ([0-9a-f]) and exactly 40 characters
// because GitHub and git both write full-length lowercase SHAs into this
// footer. Short SHAs and uppercase characters are intentionally rejected: a
// short SHA can't equal the full 40-char value stored in merge_commit_sha,
// and accepting uppercase would silently produce a case-mismatched lookup
// that always misses. (?i:...) makes only the prefix text case-insensitive
// so a future client that writes "THIS REVERTS COMMIT ..." still matches.
// The trailing \b prevents a 41+ hex blob from yielding a truncated match.
var revertsCommitRE = regexp.MustCompile(`(?i:This reverts commit )([0-9a-f]{40})\b`)

// mergeCommitRE matches the subject git auto-generates for a 2-parent merge
// commit, so push capture (#196, constraint #2) can skip it: a merge commit is an
// integration commit, not authored work, and a PR merge already arrives via the
// pull_request webhook. GitHub push payloads carry NO parent list, so a true
// parent-count check is impossible here — this matches the canonical git subjects
// ("Merge pull request #N …", "Merge branch '…'", "Merge remote-tracking branch
// '…'") instead. This is a DOCUMENTED heuristic: PR merge commits are additionally
// caught by the merge_commit_sha dedup below (their SHA already lives on the PR
// outcome), so this regex's real job is the direct-local-merge-pushed-to-main case
// that has no PR backstop. Anchored and specific to avoid false-positiving an
// ordinary subject that merely starts with the word "Merge".
var mergeCommitRE = regexp.MustCompile(`^Merge (pull request #\d+|branch |remote-tracking branch )`)

// prMergeRE is the subset of mergeCommitRE GitHub writes for a PR merged with a
// merge commit (#849's leak signal).
var prMergeRE = regexp.MustCompile(`^Merge pull request #\d+`)

func (p pushPayload) onDefaultBranch() bool {
	return p.Repository.DefaultBranch != "" && p.Ref == "refs/heads/"+p.Repository.DefaultBranch
}

// handlePush detects revert commits on the default branch and degrades quality
// on the originating outcome. The detector has three resolution tiers (#20):
//
//  1. "This reverts commit <sha>" footer → look up the outcome by
//     merge_commit_sha → apply the penalty to the ORIGINAL PR's author +
//     issue. Most reliable: git revert's auto-generated message always
//     includes the footer, and the SHA is unambiguous.
//  2. issueref.FromCommitSubject on git's %s subject → look up the latest
//     outcome for that issue → apply the penalty to its author. Covers
//     human-authored reverts with a close directive or tracker key in the
//     subject; bare #N mentions and body references never select an outcome.
//  3. Neither succeeded — emit slog.Info with the revert commit hash so the
//     gap is at least discoverable. Quality stays at 1.0.
//
// The reverter's username (c.Author.Username) is NEVER used as the penalty
// target. The developer who shipped the bug bears the quality hit, not the
// developer who reverted it. Previously the code passed the reverter's
// username straight into UpdateQuality, which silently no-opped because the
// outcome row was owned by the original author. That bug is what #20 fixes.
//
// REPLAY NOTE (#60, updated #134): push events carry no merge SHA, so there is
// no durable content-level guard on the delivery itself — only the in-memory
// delivery dedup. Replay-safety now rests on the quality_events unique key
// (outcome_id, event_type, source_ref) with source_ref = the revert commit SHA:
// a replayed revert re-inserts nothing (ON CONFLICT DO NOTHING via
// appendAndResolve), so quality is re-derived from an unchanged event set and
// stays put. This supersedes the pre-#134 reliance on UpdateQuality being an
// absolute assignment, and is robust even though quality is no longer a single
// fixed value.
func (h *Handler) handlePush(ctx context.Context, body []byte) error {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("parse push payload: %w", err)
	}
	if !p.onDefaultBranch() {
		if p.Repository.DefaultBranch == "" {
			h.logger.Warn("revert detection and push capture disabled: push has no default branch",
				"repo", logSafeStr(p.Repository.FullName), "ref", logSafeStr(p.Ref))
		} else {
			h.logger.Debug("skipping revert detection for non-default-branch push",
				"ref", logSafeStr(p.Ref), "default_branch", logSafeStr(p.Repository.DefaultBranch))
		}
		return nil
	}
	var lastErr error
	for _, c := range p.Commits {
		firstLine := strings.SplitN(c.Message, "\n", 2)[0]
		if !revertLineRE.MatchString(firstLine) {
			continue
		}

		var origin store.Outcome
		var found bool

		// Tier 1: footer → SHA lookup. Most reliable.
		if m := revertsCommitRE.FindStringSubmatch(c.Message); m != nil {
			o, ok, err := h.store.OutcomeByMergeCommit(ctx, m[1])
			if err != nil {
				h.logger.Error("lookup outcome by merge commit", "err", err, "sha", m[1])
				lastErr = err
				continue
			}
			if ok {
				origin = o
				found = true
			}
		}

		// Tier 2: issue id in the git subject → latest-outcome lookup.
		// Only attempted when tier-1 didn't resolve; the SHA lookup is
		// strictly more precise (one merge commit ↔ one outcome).
		if !found {
			if issueID := issueref.FromCommitSubject(gitCommitSubject(c.Message)); issueID != "" {
				// #231: scope the lookup to THIS repository. Before, a revert of repo B's
				// issue #42 applied the quality penalty to repo A's issue #42. The store's
				// match is tolerant, so pre-#231 (sentinel) outcomes are still reachable.
				o, ok, err := h.store.LatestOutcomeByIssue(ctx, canonicalRepo(p.Repository.FullName), issueID)
				if err != nil {
					h.logger.Error("lookup outcome by issue", "err", err, "issue", issueID)
					lastErr = err
					continue
				}
				if ok {
					origin = o
					found = true
				}
			}
		}

		// Tier 3: nothing resolved. Log so an operator who notices "this PR
		// was reverted but the score didn't change" has something to grep.
		if !found {
			h.logger.Info("revert detected but no issue id derivable",
				"commit", logSafeStr(c.ID),
				"subject", logSafeStr(firstLine),
				"author", logSafeStr(c.Author.Username),
			)
			continue
		}

		// 60d observation window (spec §3 Event 4): a revert landing more than
		// 60 days after merge is treated as maintenance / a strategic decision,
		// not a quality signal against the original author.
		eventTS := c.Timestamp
		if eventTS.IsZero() {
			eventTS = time.Now().UTC()
		}
		if eventTS.After(origin.Timestamp.Add(revertObservationWindow)) {
			h.logger.Debug("revert outside 60d observation window, ignoring",
				"issue", logSafeStr(origin.IssueID), "commit", logSafeStr(c.ID), "merged_at", origin.Timestamp, "event_ts", eventTS)
			continue
		}

		// Classify the revert (strategic 0.8 vs quality 0.1) from the commit
		// message; source_ref = the revert commit SHA makes replays idempotent.
		eventType := quality.ClassifyRevert(c.Message)
		if err := h.appendAndResolve(ctx, origin, eventType, c.ID, eventTS); err != nil {
			h.logger.Error("apply revert quality event", "err", err, "issue", logSafeStr(origin.IssueID), "commit", logSafeStr(c.ID))
			lastErr = err
		}
	}

	// Push-to-default-branch capture (#196) runs AFTER revert handling and only
	// when explicitly enabled (outcomes.push_capture). When disabled, only
	// default-branch revert detection runs. A capture error is folded into
	// lastErr so a store failure fails the delivery (5xx) rather than reporting
	// it as processed; GitHub does not redeliver it automatically.
	if h.pushCapture {
		if err := h.capturePushOutcomes(ctx, p); err != nil && lastErr == nil {
			lastErr = err
		}
	}
	return lastErr
}

// capturePushOutcomes records outcomes for qualifying direct commits to the
// DEFAULT branch (#196, RULING B). It is the trunk-based-team capture path: teams
// that commit straight to main behind feature flags never fire a pull_request
// merged event, so without this their AI cost scores ~0 (the #189 tripwire warns
// about exactly this). Contract:
//
//   - Only the default branch. A push to any other ref returns early: work on a
//     feature branch is captured (if at all) when its PR merges via the PR path.
//   - Skip revert commits — the revert path already degrades the ORIGINAL outcome;
//     a revert is not new productive work and must not earn its own 0.5.
//   - Skip 2-parent merge commits (constraint #2) — integration commits, handled
//     via the PR webhook (see mergeCommitRE for the payload-has-no-parents caveat).
//   - Squash double-fire dedup (constraint #1, made order-independent by #849):
//     store.RecordPushCommit skips a commit whose SHA is a stored merge_commit_sha,
//     and records every other captured commit in a per-commit ledger so a PR that
//     arrives LATER removes exactly its merge commit's contribution. Both checks
//     and the write are one store transaction.
//   - A commit with no id cannot be deduplicated and is unattributed.
//   - Issue id from the commit subject via issueref.FromCommitSubject (#1017,
//     #1069): close directives, then tracker keys; never a bare #N or the body.
//     A commit with no resolvable issue (or no GitHub author login to score) is
//     UNATTRIBUTED and must be observable: logged AND counted, never silently dropped.
//   - Aggregation grain (RULING B): one 0.5-weight outcome per (issue_id, UTC day)
//     via the idempotent store.RecordPushCommit — NOT 0.5×N-commits (that re-opens the
//     commit-splitting inflation vector the grain exists to close).
//
// QUALITY MAPPING GAP (constraint #7, documented per the issue): #134 CI signals
// (workflow_run) resolve their target outcome by merge_commit_sha, which push
// outcomes leave NULL, so CI pass/fail floors do NOT reach a push-captured outcome.
// Revert degradation still applies via the issue-id tier (LatestOutcomeByIssue now
// returns push outcomes). Closing the CI gap is deferred with the Option C batch
// enrichment; see docs/how-it-works.md.
func (h *Handler) capturePushOutcomes(ctx context.Context, p pushPayload) error {
	// Defence in depth: handlePush already gates on the default branch.
	if !p.onDefaultBranch() {
		return nil
	}
	pushedAt, pushedAtOK := parsePushedAt(p.Repository.PushedAt)
	var lastErr error
	var captured, unordered int
	var prMergeSeen bool
	for _, c := range p.Commits {
		firstLine := strings.SplitN(c.Message, "\n", 2)[0]

		// Reverts are handled by the revert path above; never capture one.
		if revertLineRE.MatchString(firstLine) {
			continue
		}
		// Skip 2-parent merge commits (constraint #2).
		if mergeCommitRE.MatchString(firstLine) {
			if prMergeRE.MatchString(firstLine) {
				prMergeSeen = true
			}
			continue
		}
		// Generated-file exclusion (#240, audit C4): a direct commit whose EVERY
		// changed file is generated/vendored (regenerated protobuf, `go mod vendor`,
		// a lockfile bump) carries no engineering outcome and must not earn a degraded
		// capture — a passive gaming vector otherwise. Gated on a NON-EMPTY file set:
		// a payload that omits the arrays is left to behave exactly as before, so the
		// exclusion can only remove a spurious outcome, never suppress one on missing
		// data. The paths are client-controlled, so only their COUNT is logged; the
		// commit id (also client-controlled) is escaped via logSafeStr (#240,
		// go/log-injection) so it cannot forge a log line.
		changedFiles := make([]string, 0, len(c.Added)+len(c.Removed)+len(c.Modified))
		changedFiles = append(changedFiles, c.Added...)
		changedFiles = append(changedFiles, c.Removed...)
		changedFiles = append(changedFiles, c.Modified...)
		if allGenerated(changedFiles, h.generatedPaths) {
			h.logger.Info("push capture: direct commit skipped — all changed files are generated/vendored (#240)",
				"commit", logSafeStr(c.ID), "files", len(changedFiles))
			continue
		}
		// Match the collector's git %s subject-only rule (#1017): close directives and
		// tracker keys attribute; a bare #N does not. No branch is consulted:
		// the push is to the default branch (main/master), which issueref ignores.
		subject := gitCommitSubject(c.Message)
		issueID := issueref.FromCommitSubject(subject)
		if issueID == "" || c.Author.Username == "" || !store.ValidPushCommitID(c.ID) {
			// Unattributed direct commit — MUST be observable (log + counter), never
			// a silent drop. Three distinct causes: no issue id in the subject, no
			// GitHub author login to score the outcome to, or no usable commit id
			// for the ledger to deduplicate on (#849).
			reason := "no resolvable issue id"
			switch {
			case issueID == "":
			case c.Author.Username == "":
				reason = "no GitHub author login"
			default:
				reason = "no usable commit id"
			}
			h.logger.Info("push capture: direct commit not scored — "+reason,
				"commit", logSafeStr(c.ID), "subject", logSafeStr(subject), "author", logSafeStr(c.Author.Username), "issue", logSafeStr(issueID))
			if h.pushUnattributed != nil {
				h.pushUnattributed.Inc()
			}
			continue
		}

		// UTC aggregation day (RULING B). Reuse the commit timestamp, normalized to
		// UTC (#199) — do NOT hand-roll timezone logic; time.Time.UTC is the shared
		// primitive. A zero/absent timestamp falls back to now (UTC).
		eventTS := c.Timestamp
		if eventTS.IsZero() {
			eventTS = time.Now()
		}
		eventTS = eventTS.UTC()
		day := eventTS.Format("2006-01-02")

		// Weight = the 0.5 degraded floor (RULING B). Sourced through GitHeuristic's
		// zero-effort bucket (the same 0.5 ResolveWeight returns for a diff-less
		// input) but recorded as weight_source="push", NOT "git-heuristic": the 0.5
		// here is a capture-fidelity floor, not a measured tiny diff, and must stay
		// segmentable from real heuristic weights.
		o := store.Outcome{
			Developer:    c.Author.Username,
			IssueID:      issueID,
			Weight:       store.GitHeuristic(0, 0),
			WeightSource: store.WeightSourcePush,
			Quality:      1.0,
			Source:       store.OutcomeSourcePush,
			// #231: the leading column of idx_outcomes_push_daily_repo. Without it,
			// two repos pushing to their own issue #42 on the same UTC day collided
			// and ON CONFLICT DO NOTHING silently discarded the second repo's outcome.
			Repo:      canonicalRepo(p.Repository.FullName),
			Timestamp: eventTS,
		}
		status, err := h.store.RecordPushCommit(ctx, o, day, c.ID, pushedAt)
		if err != nil {
			// Contention clears by itself: WARN, not ERROR (the delivery-level
			// line in ServeHTTP says what was lost).
			level := slog.LevelError
			if errors.Is(err, store.ErrWriteLockUnavailable) {
				level = slog.LevelWarn
			}
			h.logger.Log(ctx, level, "push capture: record commit", "err", logSafeStr(err.Error()), "issue", logSafeStr(issueID), "day", day, "commit", logSafeStr(c.ID))
			lastErr = err
			continue
		}
		switch status {
		case store.PushCommitRecorded:
			captured++
			if !pushedAtOK {
				unordered++
				if h.pushMissingPushedAt != nil {
					h.pushMissingPushedAt.Inc()
				}
			}
		case store.PushCommitPRCaptured:
			h.logger.Debug("push commit already captured via PR merge, skipping", "sha", logSafeStr(c.ID))
		case store.PushCommitDuplicate:
			h.logger.Debug("push commit already in the ledger (redelivery), skipping", "sha", logSafeStr(c.ID))
		}
	}
	if unordered > 0 {
		h.logger.Warn("push capture: push has no usable repository.pushed_at; its commits were recorded but sort last for day-row ownership (#938)",
			"recorded", unordered, "repo", logSafeStr(p.Repository.FullName))
	}
	// #849 P1: a merge-commit merge brings the PR's branch commits to the
	// default branch in this push, and they were just captured beside the PR's
	// own outcome — a double count this handler cannot yet undo (#934). Say so
	// once per push, only when this delivery captured something (so a redelivery
	// does not count it again) and only when it fully succeeded (a failed one is
	// answered 5xx and may be sent again, capturing the rest then).
	if prMergeSeen && captured > 0 && lastErr == nil {
		h.logger.Warn("push capture: push carries a PR merge commit and captured other commits beside it; those may be the merged PR's branch commits, also counted by the PR's outcome (#934)",
			"captured", captured, "repo", logSafeStr(p.Repository.FullName))
		if h.pushMergeLeak != nil {
			h.pushMergeLeak.Inc()
		}
	}
	return lastErr
}

// gitCommitSubject mirrors git pretty.c's %s: skip leading blank lines, trim
// trailing ASCII whitespace from each subject line, and join the first paragraph
// with single spaces. The body starts at the next blank line and is excluded.
func gitCommitSubject(message string) string {
	var lines []string
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimRight(line, " \t\r\v\f")
		if line == "" {
			if len(lines) == 0 {
				continue
			}
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " ")
}
