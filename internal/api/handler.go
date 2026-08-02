// Package api implements the TIER REST API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/health"
	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// Store is the subset of store.DB methods used by the API.
type Store interface {
	// CostCoverageStart returns this installation's COST HORIZON — the earliest
	// instant for which any token event was captured — and ok=false on an empty
	// store (#512). A score window starting before it divides a full window of
	// outcomes by a partial window of cost, silently inflating TIER.
	CostCoverageStart(ctx context.Context) (time.Time, bool, error)
	// SourceCoverageStart is the same horizon at per-source grain: capture paths
	// enabled on different dates have different horizons, so a window clearing
	// the global minimum can still predate one source entirely.
	SourceCoverageStart(ctx context.Context) (map[string]time.Time, error)
	InsertTokenEvent(ctx context.Context, e store.TokenEvent) error
	// InsertTokenEvents is the single-transaction bulk insert behind
	// POST /api/v1/events (#126); same MAX-on-conflict UPSERT per row as
	// InsertTokenEvent, so replayed batches are no-ops.
	InsertTokenEvents(ctx context.Context, events []store.TokenEvent) error
	// InsertManualCostEvent is InsertTokenEvent for the manual-import surface
	// (POST /api/v1/costs): identical on a matching-cost re-post, but returns
	// store.ErrCostConflict — surfaced as HTTP 409 — when a keyed re-post carries
	// a DIFFERENT cost_micro, instead of silently first-writer-wins dropping the
	// correction (#295). The automated ingester keeps the plain InsertTokenEvent
	// path, where per-message replays are cost-identical.
	InsertManualCostEvent(ctx context.Context, e store.TokenEvent) error
	InsertActualSpend(ctx context.Context, a store.ActualSpend) error
	InsertOrgActualSpend(ctx context.Context, o store.OrgActualSpend) error
	// OrgActualSpendTotals returns the net actual-paid roll-up per (org, period)
	// at or after since's month, summed across ALL sources (#42, #24), behind
	// GET /api/v1/org_actual_spend. org="" returns every org; non-empty filters
	// to an exact match.
	OrgActualSpendTotals(ctx context.Context, since time.Time, org string) ([]store.OrgActualSpendTotal, error)
	// InsertOutcome writes one outcome via the merge_commit_sha dedup path
	// (ON CONFLICT DO NOTHING) behind POST /api/v1/outcomes (#188). inserted is
	// false when the SHA already existed and the write was a no-op — the
	// authoritative dedup signal, correct even under a concurrent race, so a
	// replay can never double-insert and is reported as a duplicate.
	InsertOutcome(ctx context.Context, o store.Outcome) (inserted bool, err error)
	// OutcomeByMergeCommit fetches an already-recorded outcome so /outcomes can
	// echo its weight/source in the duplicate response.
	OutcomeByMergeCommit(ctx context.Context, sha string) (store.Outcome, bool, error)
	// DeveloperCostsWindow returns per-developer cost totals over the half-open
	// window [since, until) (#276); a zero `until` is open-ended (the pre-#276
	// [since, ∞) behavior), unchanged byte-for-byte.
	DeveloperCostsWindow(ctx context.Context, since, until time.Time) ([]store.DeveloperCost, error)
	// DeveloperIssueCostsWindow returns cost totals at (developer, issue) grain
	// (#187) over [since, until) (#276), the finer grain the work-type segmentation
	// attributes to a category: a token event's cost is charged to the work_type of
	// the outcome sharing its (developer, issue). Same window/realtime-split as
	// DeveloperCostsWindow, one level finer.
	DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time) ([]store.DevIssueCost, error)
	// CostCompositionWindow returns the cost-composition sidecar over [since, until)
	// (#234): cost by normalized model, per-class token composition, attributed vs
	// unattributed spend, and the cache-read/premium-model levers. A whole-window,
	// name-free aggregate; a zero `until` is open-ended.
	CostCompositionWindow(ctx context.Context, since, until time.Time) (store.CostComposition, error)
	// UnattributedBucketCostsWindow returns per-(developer, bucket) unattributed
	// spend over [since, until) (#refocus, Option B): the honest split of the single
	// unattributed mass the composition sidecar reports as one number, into the
	// labeled buckets (main/exploratory, detached-head, branch-without-issue, plus
	// the base sentinel for host-blind producers). A zero `until` is open-ended; the
	// handler folds it to an org split and per-developer exploratory shares, and
	// suppresses names in team-aggregation mode (#185).
	UnattributedBucketCostsWindow(ctx context.Context, since, until time.Time) ([]store.UnattributedBucketCost, error)
	// DistinctPriceVersionsWindow returns the ascending distinct price_table
	// versions that priced token_events in [since, until) (#293). Feeds the
	// mixed-version data_quality WARN on /scores: cost_micro is immutable per row
	// (#233), so a window can span multiple versions while the response stamps a
	// single active price_table.version — this read surfaces the mix. A zero `until`
	// is open-ended; an empty window returns nil.
	DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time) ([]int, error)
	// AllOutcomesWindow returns every outcome in [since, until) (#276); a zero
	// `until` is open-ended.
	AllOutcomesWindow(ctx context.Context, since, until time.Time) ([]store.Outcome, error)
	// OutcomeTokenTotals returns per-(developer, issue) token totals over each
	// outcome's attributable window, keyed by the raw token_events developer, for
	// the zero-token tripwire (#136). The caller canonicalizes the key (#125)
	// before comparing to scoring.MinAttributableTokens.
	OutcomeTokenTotals(ctx context.Context, outcomes []store.Outcome) (map[store.DevIssue]int64, error)
	// ActualSpendAllWindow returns per-developer actual paid spend over the
	// half-open period window [since, until) (#276), at monthly grain; a zero
	// `until` is open-ended. Feeds per-developer SpendLeverage without an N+1.
	ActualSpendAllWindow(ctx context.Context, since, until time.Time) (map[string]float64, error)
	OverBudgetPeriods(ctx context.Context, since time.Time) ([]store.OverBudgetPeriod, error)
	TeamsForDevelopers(ctx context.Context) (map[string]string, error)
	// DivisionsForDevelopers is the division-level (#270) counterpart to
	// TeamsForDevelopers with the identical bulk-fetch contract; the k-anon fold
	// swaps one for the other by aggregation level. See groupLabelResolvers.
	DivisionsForDevelopers(ctx context.Context) (map[string]string, error)
	UpsertDeveloperAlias(ctx context.Context, alias, canonical string) error
	DeleteDeveloperAlias(ctx context.Context, alias string) (bool, error)
	DeveloperAliases(ctx context.Context) (map[string]string, error)
	// UpsertHierarchy / UpsertHierarchies / EndMembership / ListHierarchy are the
	// org-hierarchy write surface (#232) that populates the team-aggregation
	// (#185) and org-seat-allocation (#41) tables. UpsertHierarchies is one
	// all-or-nothing transaction behind the bulk-import endpoint. The API layer
	// canonicalizes developer through the alias map (#125) before every call, so
	// hierarchy keys match the score-join's canonical keys.
	UpsertHierarchy(ctx context.Context, developer, team, division, org string) error
	UpsertHierarchies(ctx context.Context, rows []store.HierarchyRow) error
	EndMembership(ctx context.Context, developer, org, periodEnd string) error
	ListHierarchy(ctx context.Context) ([]store.HierarchyRow, error)
	// EraseDeveloper is the GDPR Art. 17 right-to-erasure primitive (#184): it
	// resolves id through the alias map (single-hop), then deletes every row for
	// the resolved identifier set across all developer-PII tables and the
	// developer_alias rows themselves in one transaction, returning per-table
	// deleted-row counts. All-zero counts mean nothing matched (idempotent).
	EraseDeveloper(ctx context.Context, id string) (map[string]int64, error)
	// ExportDeveloper is the GDPR Art. 15 access artifact (#184): every stored row
	// for the resolved identifier set, grouped by table. Empty (RowCount()==0)
	// means the developer has no data.
	ExportDeveloper(ctx context.Context, id string) (store.DeveloperExport, error)
	// ListTokenEvents / ListOutcomes are the keyset-paginated bulk-export reads
	// behind GET /api/v1/events and GET /api/v1/outcomes (#191): one page of raw
	// rows in (ts, id) order within [since, until), strictly after the cursor.
	// hasMore reports whether a further page exists (over-fetched internally); the
	// store clamps limit to store.MaxExportPageSize regardless of the request.
	ListTokenEvents(ctx context.Context, since, until time.Time, after store.PageCursor, limit int) (events []store.TokenEvent, hasMore bool, err error)
	ListOutcomes(ctx context.Context, since, until time.Time, after store.PageCursor, limit int) (outcomes []store.Outcome, hasMore bool, err error)
	// ListQualityEvents / ListQualityHistory are the same keyset-paginated bulk
	// reads behind GET /api/v1/quality_events and GET /api/v1/quality_history
	// (#242): the append-only quality signal + transition logs that make an
	// outcome's multiplier re-derivable, exported for external reconciliation.
	ListQualityEvents(ctx context.Context, since, until time.Time, after store.PageCursor, limit int) (events []store.QualityEvent, hasMore bool, err error)
	ListQualityHistory(ctx context.Context, since, until time.Time, after store.PageCursor, limit int) (history []store.QualityTransition, hasMore bool, err error)
	// DeveloperFidelity returns one capture-fidelity summary per RAW token_events
	// developer (#236) behind GET /api/v1/fidelity — event counts 7d/30d, last
	// event ts by source, the fidelity-level mix, and the unknown-model cost share.
	// The caller canonicalizes and merges the raw developer keys (#125).
	DeveloperFidelity(ctx context.Context, now time.Time) ([]store.DeveloperFidelitySignal, error)
}

// Handler handles REST API requests.
//
// apiToken, when non-empty, is required as `Authorization: Bearer <token>`
// on the write endpoints (POST /costs, POST /actual_spend) AND the score
// GETs (#59 — per-developer spend and ranking are sensitive; pre-#59 they
// were readable by anyone who could reach the listener). /health and
// /healthz stay open: they expose subsystem status only, never spend data,
// and liveness probes shouldn't need credentials.
// Empty apiToken disables the check entirely — acceptable only on a
// loopback bind, which cmd/tierd enforces fail-closed (#59).
//
// watcherState may be nil — for example when tierd serve is run without
// --watch-repo, no watcher is constructed and /healthz reports
// status=not_configured. New normalises a nil argument to a not_configured
// WatcherState and registers it into the health Registry (#48), so the
// Handler holds no per-subsystem health pointer at all — only the Registry.
type Handler struct {
	store    Store
	logger   *slog.Logger
	apiToken string
	// readToken, when non-empty, is a SECOND accepted bearer credential that is
	// authorized ONLY on the read routes (GET /scores, /scores/{developer},
	// /metrics — the data the dashboard renders) and REJECTED with 403 on every
	// mutating route and the admin/finance GETs (#190). It is the least-privilege
	// step short of SSO: a CFO/VP-Eng can be handed dashboard read access without
	// the write/erase power the apiToken confers. Empty = no read scope armed;
	// then the read routes accept only apiToken, exactly as before #190. It never
	// relaxes the fail-closed bind rule — validateBind (cmd/tierd) still requires
	// the write apiToken for a non-loopback bind. Set once before serving via
	// SetReadToken, mirroring the SetMetricsRegistry write-once contract.
	readToken string
	// subsystems is the extensible health Registry (#48) — the ONLY health
	// state the handler holds. The watcher registers here at New (so /healthz's
	// legacy top-level `watcher` block is derived from subsystems["watcher"]);
	// future collectors register via RegisterSubsystem before serving. This
	// replaces the per-subsystem state pointers the handler used to accumulate.
	subsystems *health.Registry
	version    string
	startedAt  time.Time
	limiter    *authLimiter      // per-IP failed-auth lockout (#36); nil/disabled = off
	metricsReg *metrics.Registry // #67; nil = no /metrics route mounted
	// identityGauge exports tier_identity_unjoined{side} (#125); nil = no-op.
	// Set once before serving via SetIdentityGauge, then only Set() from the
	// /scores read path.
	identityGauge *metrics.GaugeVec
	// pricingDivergence counts /events rows whose client-posted cost_usd diverged
	// from the server's authoritative price (#233) — a mixed-version-fleet signal.
	// nil = no-op (the `tierd score` / test path). Set once before serving via
	// SetPricingDivergenceCounter, then only Inc() from the /events write path.
	pricingDivergence *metrics.CounterVec
	// pricingDivergenceSeen dedups the divergence WARN to at most once per distinct
	// (model, sign-of-skew) per process (#233), mirroring identitySeen above. The
	// shipper re-posts a 90-day window every ~15 min, so without this a mixed-version
	// fleet would re-log every divergent event on every cycle — a self-flood. The
	// counter still moves per event; only the WARN is deduped. Keyed by
	// "model\x00sign" (model as key material only — never logged, per log-safety).
	pricingDivergenceSeen sync.Map
	// identitySeen dedups the unjoined-identity WARN to at most once per
	// identifier per process (#125), so a cron scraping /scores can't flood
	// the logs. Keyed by "side\x00identifier".
	identitySeen sync.Map
	// aggregation selects per-developer vs team-only reporting on the served
	// surfaces (#185). Zero value = scoring.AggregationDeveloper, so an unset
	// Handler (all existing tests, and any caller that never calls SetAggregation)
	// names developers exactly as before. cmd/tierd sets it explicitly from the
	// REQUIRED --aggregation setting; there is no silent default there.
	aggregation scoring.AggregationMode
	// kAnonymity is the cohort floor applied in EVERY anonymized mode -- team (#185)
	// and division (#270), i.e. whenever aggregation.Anonymized() is true: a cohort
	// with fewer than this many contributing developers collapses into the aggregate
	// "other" bucket. Set alongside aggregation via SetAggregation.
	kAnonymity int
	// retentionHorizon is the earliest instant for which raw token_events /
	// outcomes are GUARANTEED still present. The zero value means "no retention
	// pruning is configured — all history is retained", which is today's state:
	// retention (#252) is not built. When a future retention rollup prunes raw
	// rows, cmd/tierd will arm this via SetRetentionHorizon, and a score window
	// whose lower bound predates it is REJECTED (422) rather than answered from a
	// pruned zone that would silently underreport (#276 pre-registers this contract
	// for #252). Fail-loud is deliberate over clamp-with-flag: a clamp needs a
	// response-schema field that #277's period-comparison would have to interpret,
	// so the schema commitment is deferred to #252's design; until then the safe,
	// reversible default is to refuse the unanswerable window. See
	// checkWindowRetention.
	retentionHorizon time.Time
}

// New returns a new Handler. apiToken="" disables bearer auth everywhere;
// New emits a startup warning in that case so operators don't silently
// expose unauthenticated endpoints to a network. The pattern mirrors the
// existing webhook-secret warning at webhook/handler.go.
//
// watcherState is the shared health.WatcherState the supervisor updates;
// pass nil when no watcher is configured.
//
// version is the build version reported by /livez (the binary injects it via
// -ldflags; empty falls back to "dev"). startedAt is captured here rather than
// passed in: New runs during process startup, so handler-construction time is
// process-start time for any practical uptime reading.
// rateLimit configures the per-IP failed-auth lockout (#36). The zero value
// disables it; cmd/tierd passes a config built from the --auth-* flags (whose
// defaults come from DefaultRateLimitConfig: 10 / 60s / 15m). The limiter only
// ever engages when apiToken != "" (auth is on).
func New(s Store, logger *slog.Logger, apiToken string, watcherState *health.WatcherState, version string, rateLimit RateLimitConfig) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if apiToken == "" {
		// The bind guard (validateBind in cmd/tierd) enforces the loopback
		// restriction; this warning must not restate it, because the synthetic
		// read-only demo is deliberately exempt (#476) and would make the old
		// "refuses non-loopback binds" clause read as false right after it binds.
		logger.Warn("TIER_API_TOKEN is not set — writes, score GETs, and the proxies are unauthenticated (#59)")
	}
	if version == "" {
		version = "dev"
	}
	// Normalise a nil watcherState to a not_configured state (#48). Pre-#48
	// the nil case was synthesised at each /healthz hit; doing it once here
	// keeps the legacy `watcher` block and the subsystems entry consistent and
	// lets the watcher register into the Registry unconditionally. A
	// not_configured state is healthy, so running tierd without --watch-repo
	// still returns 200 exactly as before.
	ws := watcherState
	if ws == nil {
		ws = health.NewWatcherState()
	}
	reg := health.NewRegistry()
	reg.Register("watcher", ws)
	return &Handler{
		store:      s,
		logger:     logger,
		apiToken:   apiToken,
		subsystems: reg,
		version:    version,
		startedAt:  time.Now(),
		limiter:    newAuthLimiter(rateLimit, nil),
	}
}

// RegisterSubsystem adds a subsystem to the /healthz `subsystems` map under
// name (#48). It follows the write-once-before-serve wiring seam used by
// SetMetricsRegistry and friends: call it during startup, before Register
// mounts the routes. It panics on an empty/duplicate name or nil snapshotter
// (see health.Registry.Register) — a startup-wiring bug, caught at boot.
func (h *Handler) RegisterSubsystem(name string, s health.Snapshotter) {
	h.subsystems.Register(name, s)
}

// Register mounts all API routes on mux. Write endpoints and the admin GETs are
// wrapped with requireAuth (write scope): they 401 without a token and 403 for
// the read-only viewer token (#59, #190). The score GETs and /metrics are
// wrapped with requireRead (#190) so the read-only viewer token — the data the
// dashboard renders — is accepted alongside the write token. /health, /healthz,
// and /livez stay open for probes — status only, no spend data.
//
// Scope boundary (#190): the read token grants ONLY GET /scores,
// /scores/{developer}, /metrics (plus the static dashboard, which fetches
// /scores), and the bulk exports GET /events and GET /outcomes (#191) plus GET
// /quality_events and GET /quality_history (#242) — the CFO/BI read use case. The
// finance/admin reads — GET /org_actual_spend and GET /developer_alias — stay
// write-scoped, so a viewer cannot read raw invoice totals or the identity-alias map.
func (h *Handler) Register(mux *http.ServeMux) {
	h.registerWriteRoutes(mux)
	h.registerReadRoutes(mux)
}

// RegisterReadOnly mounts ONLY the read + health routes — every write/ingest/admin
// route is STRUCTURALLY ABSENT from the mux, so it 404s regardless of any token
// (defence in depth beyond the requireAuth 401/403 scope check). This is the mode
// for a publicly-exposed instance — the community demo at demo.tiermetric.org
// (#429): even if the write token leaks, no ingest or mutation endpoint exists to
// reach. It shares registerReadRoutes with Register, so a future READ route added
// there appears in both, while a future WRITE route added to registerWriteRoutes
// can never accidentally leak into read-only mode. Callers must ALSO omit EVERY
// other ingest/write subsystem they mount — the GitHub webhook, the ingest
// proxies, the JSONL watcher, and the coverage pollers (all mounted/started in
// cmd/tierd, not here); see runServe's --read-only choke point, which blanks their
// config in one place. Read routes stay OPEN per the token/aggregation config, so a
// read-only instance is public-safe only on synthetic data or with a read-token +
// k-anonymized aggregation.
func (h *Handler) RegisterReadOnly(mux *http.ServeMux) {
	h.registerReadRoutes(mux)
}

// registerWriteRoutes mounts the write/ingest + admin routes, all requireAuth
// (write scope): they 401 without a token and 403 for the read-only viewer token
// (#59, #190). These are the routes RegisterReadOnly deliberately omits.
func (h *Handler) registerWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/costs", h.requireAuth(h.handlePostCosts))
	mux.HandleFunc("POST /api/v1/events", h.requireAuth(h.handlePostEvents))
	mux.HandleFunc("POST /api/v1/outcomes", h.requireAuth(h.handlePostOutcome))
	mux.HandleFunc("POST /api/v1/actual_spend", h.requireAuth(h.handlePostActualSpend))
	mux.HandleFunc("POST /api/v1/org_actual_spend", h.requireAuth(h.handlePostOrgActualSpend))
	// #42: finance read-back of what the org has recorded as actual-paid spend.
	// Write-scoped (#190): raw invoice totals are finance data a dashboard viewer
	// has no need for, so this stays requireAuth, not requireRead. The POST above
	// is the write half; this GET is the audit half.
	mux.HandleFunc("GET /api/v1/org_actual_spend", h.requireAuth(h.handleGetOrgActualSpend))
	// Developer identity mapping admin API (#125). Auth-gated like the other
	// writes and the score GETs: an alias edit retroactively re-joins spend to
	// outcomes, so it is an administrative, not a public, operation.
	mux.HandleFunc("POST /api/v1/developer_alias", h.requireAuth(h.handlePostDeveloperAlias))
	mux.HandleFunc("DELETE /api/v1/developer_alias/{alias}", h.requireAuth(h.handleDeleteDeveloperAlias))
	mux.HandleFunc("GET /api/v1/developer_alias", h.requireAuth(h.handleGetDeveloperAliases))
	// Org-hierarchy write surface (#232): the write path for the tables team
	// aggregation (#185) and org-seat allocation (#41) read. WRITE-scoped
	// (requireAuth): populating org structure is an administrative operation, and
	// GET /org_hierarchy discloses the full developer→team map, so — like the
	// admin developer_alias GET above — it is NOT granted to the read-only viewer
	// scope (#190). These stay available in team-aggregation mode by design: org
	// STRUCTURE is not per-developer score data (see the per-handler comments),
	// mirroring the #185 carve-out for the GDPR endpoints. POST is the
	// all-or-nothing bulk import (array body, mirrors /events); PUT is the single
	// per-developer upsert.
	mux.HandleFunc("PUT /api/v1/org_hierarchy/{developer}", h.requireAuth(h.handlePutHierarchy))
	mux.HandleFunc("POST /api/v1/org_hierarchy", h.requireAuth(h.handleBulkHierarchy))
	mux.HandleFunc("GET /api/v1/org_hierarchy", h.requireAuth(h.handleGetHierarchy))
	mux.HandleFunc("POST /api/v1/period_membership/{developer}/end", h.requireAuth(h.handleEndMembership))
	// GDPR data-subject rights (#184). Both are WRITE-scoped (requireAuth): the
	// export discloses a full individual PII record and the erase destroys data,
	// so — unlike the score GETs (#190) — the read-only viewer token is REJECTED
	// 403 here. They are admin compliance tooling, NOT reporting surfaces, so they
	// stay available to the admin token even in team-aggregation mode (#185); see
	// the per-handler comments for why they are deliberately NOT suppressed there.
	mux.HandleFunc("DELETE /api/v1/developer/{id}", h.requireAuth(h.handleEraseDeveloper))
	mux.HandleFunc("GET /api/v1/developer/{id}/export", h.requireAuth(h.handleExportDeveloper))
}

// registerReadRoutes mounts the read (requireRead, #190) + open health/probe
// routes — the surface a dashboard viewer and a public demo need, and the ONLY
// surface RegisterReadOnly exposes. Shared by Register and RegisterReadOnly so the
// two can never drift.
func (h *Handler) registerReadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/scores", h.requireRead(h.handleGetScores))
	// Before/after period comparison (#277): two half-open windows in, per-row
	// deltas + CI-overlap significance out. READ-scoped (requireRead, #190) and
	// under the SAME anonymized k-anon guard as /scores — see handleGetScoresCompare.
	// The static "compare" path is more specific than the "{developer}" wildcard
	// below, so ServeMux (Go 1.22+ precedence) routes it here, never to the
	// per-developer handler.
	mux.HandleFunc("GET /api/v1/scores/compare", h.requireRead(h.handleGetScoresCompare))
	mux.HandleFunc("GET /api/v1/scores/{developer}", h.requireRead(h.handleGetDeveloperScore))
	// Paginated bulk export of the raw token_events / outcomes rows (#191). The
	// POST halves (ingest) are the write routes; ServeMux routes by method+path,
	// so the GET halves coexist. READ-scoped (requireRead, #190): this is the
	// CFO-reconciliation / BI-pipeline READ use case, so the read-only viewer token
	// is accepted — a viewer can pull the data WITHOUT the write/erase power the
	// admin token confers. The handlers themselves 403 in team-aggregation mode
	// (#185); see the per-handler comments for why raw per-developer rows must stay
	// suppressed there.
	mux.HandleFunc("GET /api/v1/events", h.requireRead(h.handleGetEvents))
	mux.HandleFunc("GET /api/v1/outcomes", h.requireRead(h.handleGetOutcomes))
	// Bulk export of the quality audit chain (#242): the append-only quality_events
	// signal log and quality_history transition log that make an outcome's multiplier
	// re-derivable (quality == last new_quality). Same pagination/CSV/READ-scope
	// contract as the events/outcomes exports, and — because these rows also carry a
	// per-developer `developer` column — the same #185 team-mode 403 guard.
	mux.HandleFunc("GET /api/v1/quality_events", h.requireRead(h.handleGetQualityEvents))
	mux.HandleFunc("GET /api/v1/quality_history", h.requireRead(h.handleGetQualityHistory))
	// Capture-fidelity signals (#236): the rollout dashboard for "which developers
	// are (not) shipping, and at what quality". READ-scoped (requireRead, #190) —
	// the CFO/VP-Eng verifying an install needs it and it exposes no raw invoice
	// totals — and, like GET /events, 403s in team-aggregation mode (#185) because
	// it names individual developers. Distinct from /healthz (watcher liveness) and
	// /scores data_quality (#136): this validates a fresh install end to end.
	mux.HandleFunc("GET /api/v1/fidelity", h.requireRead(h.handleGetFidelity))
	mux.HandleFunc("GET /api/v1/health", h.handleHealth)
	mux.HandleFunc("GET /api/v1/healthz", h.handleHealthz)
	mux.HandleFunc("GET /api/v1/livez", h.handleLivez)
	// Prometheus scrape endpoint (#67). Read-scoped like the score GETs (#190):
	// /metrics exposes internal counters (and, via labels, operational signal
	// about a single-tenant deployment's spend activity), which the dashboard and
	// a read-only viewer legitimately need, so the read token is accepted here;
	// it is not world-readable. Mounted only when a registry is wired
	// (cmd/tierd); tests that don't set one get no route.
	if h.metricsReg != nil {
		mux.HandleFunc("GET /metrics", h.requireRead(h.handleMetrics))
	}
}

// SetMetricsRegistry wires the metrics registry rendered by GET /metrics. Call
// before Register; a nil registry (the default) leaves the route unmounted.
func (h *Handler) SetMetricsRegistry(reg *metrics.Registry) { h.metricsReg = reg }

// SetIdentityGauge wires the tier_identity_unjoined{side} gauge recomputed on
// every /scores read (#125). Call before Register; nil (the default) makes the
// gauge writes a no-op, so `tierd score` and tests that don't wire metrics keep
// working. Mirrors SetMetricsRegistry's write-once-before-serve contract.
//
// Semantics: the gauge reflects the MOST RECENT /scores computation's `since`
// window, not a fixed server-owned window — the window is not a label. With one
// scraper (the dashboard) this is stable; two readers passing different `since`
// values would make the series oscillate between their computations. That is
// acceptable for the single-tenant deployment this targets; if multiple
// distinct-window scrapers appear, add the window to the label set here.
func (h *Handler) SetIdentityGauge(g *metrics.GaugeVec) { h.identityGauge = g }

// SetPricingDivergenceCounter wires the tier_pricing_divergence_total counter
// bumped when an /events row's client-posted cost_usd disagrees with the server's
// authoritative price (#233). Call before Register; nil (the default) makes the
// bump a no-op, so `tierd score` and tests that don't wire metrics keep working.
// Mirrors SetMetricsRegistry's write-once-before-serve contract.
func (h *Handler) SetPricingDivergenceCounter(c *metrics.CounterVec) { h.pricingDivergence = c }

// SetRetentionHorizon records the earliest instant for which raw token_events /
// outcomes are guaranteed still present, arming the #276 fail-loud check that a
// score window cannot reach into a pruned retention zone (#252). Normalized to
// UTC so the comparison in checkWindowRetention is by instant, not wall-clock.
// The zero value (the default) disables the check — today's state, since
// retention pruning is not yet built and all history is retained. Call before
// serving; mirrors SetReadToken's write-once-before-serve contract.
func (h *Handler) SetRetentionHorizon(t time.Time) { h.retentionHorizon = t.UTC() }

// errWindowPredatesRetention is the fail-loud sentinel returned by
// checkWindowRetention. Its message is server-controlled and names no
// client-supplied value, so it is safe to surface in the 422 response body.
var errWindowPredatesRetention = errors.New(
	"requested window predates the earliest retained data (retention horizon)")

// checkWindowRetention fails loud when a score window's lower bound reaches into
// a pruned retention zone (#276 contract for #252). With no retention configured
// (zero horizon — today) it is a no-op. `since` is the earliest instant the
// scores path reads for cost and outcome sums, so it is the bound that decides
// answerability; `until` is the recent (upper) edge and can only sit in the
// pruned zone when since already does, so checking since covers both.
//
// One caveat #252's design must honor: the #136 zero-token tripwire looks back
// store.AttributableWindow BEFORE an outcome's merge (and thus before `since`),
// so once pruning exists the horizon it prunes to must leave that look-back
// intact — otherwise the tripwire could read a pruned zone and over-flag. That
// is a constraint on where the prune boundary is set, not on this check.
func (h *Handler) checkWindowRetention(since time.Time) error {
	if h.retentionHorizon.IsZero() {
		return nil
	}
	if since.Before(h.retentionHorizon) {
		return errWindowPredatesRetention
	}
	return nil
}

// SetReadToken wires the read-only viewer token (#190). Call before Register;
// "" (the default) leaves the read scope unarmed so the read routes accept only
// the write apiToken, exactly as before #190. A read token equal to the
// apiToken would silently grant write scope (write wins in classify), defeating
// least privilege — cmd/tierd rejects that at startup, so this setter trusts its
// caller to pass a distinct value. Mirrors SetMetricsRegistry's
// write-once-before-serve contract.
func (h *Handler) SetReadToken(token string) { h.readToken = token }

// SetAggregation wires the reporting mode and k-anonymity floor (#185). Call
// before Register; the zero value (scoring.AggregationDeveloper, k unused) is the
// default so an unset Handler names developers exactly as before. In
// AggregationTeam mode the served GET /scores, the dashboard it feeds, and GET
// /scores/{developer} NEVER surface an individual developer name: named rows are
// replaced by team aggregates and any team below the k floor collapses into an
// "other" bucket. Mirrors SetMetricsRegistry's write-once-before-serve contract.
func (h *Handler) SetAggregation(mode scoring.AggregationMode, k int) {
	h.aggregation = mode
	h.kAnonymity = k
}

// groupLabelResolvers maps each ANONYMIZED aggregation level to the store read
// that produces its developer->label map (#270). This is the seam that makes the
// level a clean parameter rather than a hardcoded branch: the k-anon fold
// (scoring.AggregateTeamsKAnon) and every suppression guard (via
// AggregationMode.Anonymized) are level-agnostic, so adding a new level — org,
// department — is exactly TWO mechanical edits: a new scoring.AggregationMode
// value and one store read registered here. No new branch in the score path.
//
// AggregationDeveloper is deliberately ABSENT: it names individuals and never
// folds, so it never resolves a group label. The values are method expressions
// on the Store interface, bound to a concrete store at call time.
var groupLabelResolvers = map[scoring.AggregationMode]func(Store, context.Context) (map[string]string, error){
	scoring.AggregationTeam:     Store.TeamsForDevelopers,
	scoring.AggregationDivision: Store.DivisionsForDevelopers,
}

// resolveGroupLabels returns the canonicalized developer->group-label map for the
// active anonymized aggregation level (#270). It centralizes what the team-mode
// code paths used to inline: pick the level's store read from groupLabelResolvers,
// then canonicalize the keys (#125) so an aliased identity resolves to the same
// label the scored row is keyed under. Only ever called when
// h.aggregation.Anonymized() is true; a mode with no registered resolver is a
// programming error surfaced as one, never a silent un-anonymized fallthrough.
func (h *Handler) resolveGroupLabels(ctx context.Context, canon func(string) string) (map[string]string, error) {
	resolve, ok := groupLabelResolvers[h.aggregation]
	if !ok {
		return nil, fmt.Errorf("no group-label resolver for aggregation mode %q (#270)", h.aggregation)
	}
	raw, err := resolve(h.store, ctx)
	if err != nil {
		return nil, err
	}
	labelOf := make(map[string]string, len(raw))
	for dev, label := range raw {
		labelOf[canon(dev)] = label
	}
	return labelOf, nil
}

// handleMetrics renders the Prometheus text exposition (#67). Content-Type is
// the v0.0.4 text format so a scraper parses it without negotiation.
func (h *Handler) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.metricsReg.Render(w)
}

// authBearerPrefix is the lowercase form of the "Bearer " scheme prefix.
// Per RFC 7235 §2.1 the auth-scheme name is case-insensitive — clients are
// free to send "bearer", "BEARER", or any mixed case. We compare with
// strings.EqualFold against this constant.
const authBearerPrefix = "bearer "

// authScope is the privilege a presented token satisfies (#190). Ordering is
// deliberate: scopeWrite is the superset — a write (admin) token is accepted
// everywhere the read scope is, so a route asking for scopeRead is satisfied by
// EITHER a read or a write token, while a route asking for scopeWrite is
// satisfied only by the write token.
type authScope int

const (
	scopeNone  authScope = iota // no valid token presented
	scopeRead                   // matched the read-only viewer token (#190)
	scopeWrite                  // matched the write/admin apiToken
)

// bearerCandidate extracts the token bytes from a case-insensitive
// `Authorization: Bearer <token>` header (RFC 7235 §2.1), or nil when the
// header is absent or uses another scheme. The scheme check is O(len(prefix))
// regardless of input, so it is not a data-dependent timing leak.
func bearerCandidate(r *http.Request) []byte {
	got := r.Header.Get("Authorization")
	if len(got) >= len(authBearerPrefix) &&
		strings.EqualFold(got[:len(authBearerPrefix)], authBearerPrefix) {
		return []byte(got[len(authBearerPrefix):])
	}
	return nil
}

// requireAuth wraps a WRITE (mutating or admin) route: it requires the
// write/admin apiToken (scopeWrite). The read-only viewer token (#190) is a
// valid credential but the wrong scope here, so it is rejected 403 — not 401 —
// and does NOT count against the brute-force limiter. When h.apiToken is empty,
// the wrapper is transparent (auth disabled; New logs a warning in that mode).
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiToken == "" {
			next(w, r)
			return
		}
		if !h.authorize(w, r, bearerCandidate(r), scopeWrite, "invalid token") {
			return
		}
		next(w, r)
	}
}

// requireRead wraps a READ route (GET /scores, /scores/{developer}, /metrics —
// the data the dashboard renders). It is satisfied by EITHER the read-only
// viewer token or the write apiToken (scopeRead, #190). When h.apiToken is
// empty, auth is disabled and the wrapper is transparent, exactly like
// requireAuth — the read token has no effect without the write token, matching
// the fail-closed loopback-only posture cmd/tierd enforces.
func (h *Handler) requireRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiToken == "" {
			next(w, r)
			return
		}
		if !h.authorize(w, r, bearerCandidate(r), scopeRead, "invalid token") {
			return
		}
		next(w, r)
	}
}

// classify reports the highest scope candidate satisfies (#190). It ALWAYS runs
// a full constant-time compare against the write secret, and against the read
// secret whenever one is configured, so the result carries no data-dependent
// timing signal about either token's bytes. The `h.readToken != ""` short-circuit
// is on a static config property (not attacker-controlled input), so it leaks
// nothing. scopeWrite wins when both somehow match — cmd/tierd refuses to start
// with readToken == apiToken, so that collision cannot occur in practice, but
// preferring write here fails safe rather than silently downgrading the admin.
func (h *Handler) classify(candidate []byte) authScope {
	writeMatch := constantTimeTokenCheck([]byte(h.apiToken), candidate)
	readMatch := h.readToken != "" && constantTimeTokenCheck([]byte(h.readToken), candidate)
	switch {
	case writeMatch:
		return scopeWrite
	case readMatch:
		return scopeRead
	default:
		return scopeNone
	}
}

// authorize runs the shared per-IP failed-auth lockout (#36) and the
// constant-time scope check for requireAuth (Bearer), requireRead (Bearer), and
// ProxyAuth (X-Tier-Token). They validate the same secrets, so they share one
// limiter keyed by client IP; gating only one surface would leave the others an
// unthrottled brute-force oracle. candidate is the raw token the caller
// extracted from its scheme-specific header; required is the scope the route
// demands.
//
// Order matters: a locked-out IP is rejected with 429 BEFORE any compare, so the
// lockout response is data-independent of the secrets (no timing/length leak).
// A wholly-invalid token (scopeNone) burns exactly one full-length compare per
// configured secret (see constantTimeTokenCheck) and is the only path that
// records a failure. Returns true only when the presented scope satisfies
// required; see the switch below for how each outcome touches the limiter.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, candidate []byte, required authScope, unauthMsg string) bool {
	ip := clientIP(r, h.limiter.trustedProxies())
	if d, locked := h.limiter.retryAfter(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed authentication attempts; retry later")
		return false
	}
	// scopeWrite satisfies every route; a read route (required==scopeRead) is
	// satisfied by any valid token. Three outcomes, three limiter dispositions:
	//   - authenticated AND authorized → success: clear the IP's failure counter
	//     (the #36 don't-punish-a-fat-fingered-operator behavior).
	//   - authenticated but WRONG scope (read token on a write route) → 403 and
	//     limiter-NEUTRAL: a valid credential is not a brute-force attempt, so it
	//     is not a failure; and it must not RESET the counter via a write route,
	//     which would blunt the lockout for the surrounding scopeNone guesses.
	//   - no valid token → failure: record it and 401.
	// Cases are evaluated top-to-bottom; scopeNone MUST come first so an absent /
	// invalid token on a read route is a 401 and never falls into the grant case
	// below (whose `required == scopeRead` disjunct would otherwise match any
	// request to a read route regardless of the token presented).
	switch got := h.classify(candidate); {
	case got == scopeNone:
		h.limiter.recordFailure(ip)
		writeError(w, http.StatusUnauthorized, unauthMsg)
		return false
	case got == scopeWrite || required == scopeRead:
		h.limiter.recordSuccess(ip)
		return true
	default: // got == scopeRead && required == scopeWrite
		writeError(w, http.StatusForbidden, "read-only token is not authorized for this endpoint")
		return false
	}
}

// constantTimeTokenCheck reports whether candidate equals expected. On
// length mismatch (including a nil/absent candidate) it burns an
// equal-length self-compare so every failure path costs O(len(expected))
// byte ops — see requireAuth's doc comment for the timing-attack rationale.
func constantTimeTokenCheck(expected, candidate []byte) bool {
	if len(candidate) == len(expected) {
		return subtle.ConstantTimeCompare(candidate, expected) == 1
	}
	_ = subtle.ConstantTimeCompare(expected, expected)
	return false
}

// ProxyTokenHeader carries the tierd API token on proxied provider requests.
// The Authorization header can't double for this: OpenAI-style clients send
// their upstream key there, and hijacking it would break them. A dedicated
// header keeps tierd auth orthogonal to provider auth for every client.
const ProxyTokenHeader = "X-Tier-Token"

// ProxyAuth gates a reverse-proxy handler behind the shared API token, read
// from the X-Tier-Token request header (#59 — pre-#59 the proxy was an open
// relay to the upstream providers). The header is stripped before the
// request is forwarded so the tierd token never reaches the provider.
//
// It is a method (not a package func) so it shares the Handler's #36 failed-auth
// limiter: the proxy validates the SAME token as the REST endpoints and carries
// real upstream Anthropic/OpenAI credentials, so it must not be an unthrottled
// brute-force oracle for that token. An empty apiToken disables the gate,
// matching requireAuth's contract; that mode is safe only because cmd/tierd
// refuses non-loopback binds without a token — except for the synthetic
// read-only demo (#476), where the proxy is structurally absent, so there is no
// upstream oracle for this reasoning to protect.
//
// The proxy requires the WRITE scope (#190): forwarding to the upstream provider
// on real credentials is a spend-incurring capability, not a read, so the
// read-only viewer token is rejected 403 here just like on the mutating routes.
func (h *Handler) ProxyAuth(next http.Handler) http.Handler {
	if h.apiToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate := []byte(r.Header.Get(ProxyTokenHeader))
		if !h.authorize(w, r, candidate, scopeWrite, "invalid or missing "+ProxyTokenHeader+" header") {
			return
		}
		r.Header.Del(ProxyTokenHeader)
		next.ServeHTTP(w, r)
	})
}

// --- POST /api/v1/costs ---

// maxCostsBody caps the request body. Matches the /actual_spend cap added in
// #17 — legitimate payloads are a few hundred bytes; anything larger is
// malicious or malformed.
const maxCostsBody = 1 << 20

type costRequest struct {
	Developer string `json:"developer"`
	IssueID   string `json:"issue_id"`
	Model     string `json:"model"`
	InputTok  int    `json:"input_tokens"`
	OutputTok int    `json:"output_tokens"`
	CacheRead int    `json:"cache_read_tokens"`
	// Preferred (since issue #55): clients supply cache-write tokens split
	// by TTL bucket to match Anthropic's 5m (1.25x) vs 1h (2x) pricing.
	CacheWrite5m int `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1h int `json:"cache_write_1h_tokens,omitempty"`
	// CacheWrite is the legacy single-bucket cache-write field. Deprecated
	// as of #55 and accepted for one release with a Warning: 299 response
	// header; mutually exclusive with the split fields above. When present
	// AND non-zero, the value is routed into CacheWrite5m (matches
	// Anthropic's pre-1h default — see plan in ~/.claude/plans/).
	//
	// An explicit `cache_write_tokens: 0` is treated as field-absent and
	// emits no Warning header. The deprecation signal exists for clients
	// that are still actively populating the field; a zero literal carries
	// no information and there's nothing for the client to migrate.
	//
	// json:omitempty here is request-side belt-and-braces: the handler
	// doesn't marshal costRequest back, but if a future code path ever does,
	// omitempty keeps the deprecated field out of new responses.
	CacheWrite int     `json:"cache_write_tokens,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	Source     string  `json:"source"`
	Fidelity   string  `json:"fidelity"`
	// IdempotencyKey is an optional client-supplied dedup token (#21).
	// When non-empty, identical re-submissions collide on the partial
	// unique index and the second insert is a silent no-op (with the
	// MAX-on-conflict semantics from #18, identical values are preserved
	// unchanged). When empty, the row inserts unkeyed and re-posts will
	// duplicate — the previous behaviour, retained for back-compat with
	// scripts that don't track keys.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func (h *Handler) handlePostCosts(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxCostsBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req costRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Reject trailing JSON after the first object — a second object would
	// otherwise silently succeed.
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}

	if req.Developer == "" || req.IssueID == "" || req.Model == "" {
		writeError(w, http.StatusBadRequest, "developer, issue_id, and model are required")
		return
	}
	if len(req.Developer) > maxIdentifierLen || len(req.IssueID) > maxIdentifierLen || len(req.Model) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "identifier fields must be <= 256 chars")
		return
	}
	// idempotency_key is optional and fully client-generated, so cap it at the
	// trust boundary just like the required identifiers above (#144). Without
	// this bound a client can persist a ~1 MiB string per row into the partial
	// unique index (#21) — the exact storage-DoS maxIdentifierLen already
	// prevents for developer/issue_id/model, which the 1 MiB body cap alone does
	// not. Separate, field-named message (not folded into the identifier-fields
	// error): the client generated this value and a precise message tells it
	// which field to trim. Empty stays allowed (unkeyed insert; documented
	// back-compat on costRequest.IdempotencyKey). /events applies the identical
	// cap in validateEventRequest — this closes the same gap on /costs.
	if len(req.IdempotencyKey) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("idempotency_key must be <= %d chars", maxIdentifierLen))
		return
	}
	if math.IsNaN(req.CostUSD) || math.IsInf(req.CostUSD, 0) {
		writeError(w, http.StatusBadRequest, "cost_usd must be finite")
		return
	}
	// Magnitude cap (#118): reject absurd totals at the trust boundary so a
	// hostile/buggy client can't drive DollarsToMicro past int64 range. The
	// conversion saturates deterministically as a backstop, but a saturated
	// $9.2-trillion row is still garbage — fail loud instead. Upper bound only;
	// negatives are already rejected by the sign check below.
	if req.CostUSD > store.MaxCostUSD {
		writeError(w, http.StatusBadRequest, "cost_usd must be <= 1e12")
		return
	}
	// Reject negatives: token counts and cost must be >= 0. Without this a
	// client could push negative values that survive the MAX-on-conflict
	// upsert (because MAX(positive, negative) = positive, so the negatives
	// would be invisible to dedup) but still skew SUM-based aggregates if
	// inserted with a fresh key.
	if req.InputTok < 0 || req.OutputTok < 0 || req.CacheRead < 0 ||
		req.CacheWrite5m < 0 || req.CacheWrite1h < 0 || req.CacheWrite < 0 ||
		req.CostUSD < 0 {
		writeError(w, http.StatusBadRequest, "token counts and cost_usd must be >= 0")
		return
	}
	// Reconcile legacy cache_write_tokens with the new TTL-split fields.
	// Mutually exclusive: clients on either side of the API change can
	// submit, but combining them is ambiguous (which TTL did the legacy
	// number belong to?). Reject that combination outright.
	if req.CacheWrite > 0 && (req.CacheWrite5m > 0 || req.CacheWrite1h > 0) {
		writeError(w, http.StatusBadRequest,
			"cache_write_tokens cannot be combined with cache_write_5m_tokens or cache_write_1h_tokens")
		return
	}
	if req.CacheWrite > 0 {
		// Legacy path: route into the 5m bucket and surface a deprecation
		// warning via the RFC 7234 §5.5 Warning header. The format is
		// `<code> <warn-agent> <warn-text>` — "tierd" is our warn-agent
		// (a server token identifying who attached the warning); strict
		// proxies drop headers that omit it. Code 299 is the only 2xx
		// warning that survives revalidation per §5.5, which is exactly
		// the semantic for "deprecation that intermediaries shouldn't
		// strip". Use Add not Set so any Warning a middleware already
		// attached survives — §5.5 allows multiple Warning values.
		req.CacheWrite5m = req.CacheWrite
		w.Header().Add("Warning",
			`299 tierd "cache_write_tokens is deprecated; use cache_write_5m_tokens and cache_write_1h_tokens — to be removed in the next minor release"`)
	}
	// #34: the manual REST endpoint may only attribute rows to source "api".
	// The JSONL collector and proxy set source internally ("jsonl"/"proxy");
	// letting a client claim an automated source would (a) misrepresent capture
	// provenance and (b) hand the row to recomputeKnownSourceCosts, which
	// silently overwrites cost_usd for source IN ('jsonl','proxy',...) — so a
	// forged source would let tierd clobber a client-posted cost. Accept only
	// "" (defaults to "api") or an explicit "api"; reject anything else. The
	// companion fidelity restriction below protects Coverage % / Spend Leverage.
	switch req.Source {
	case "":
		req.Source = "api"
	case "api":
		// explicit, allowed
	default:
		writeError(w, http.StatusBadRequest, `source must be "api" or omitted on /api/v1/costs`)
		return
	}
	// #82: "realtime" fidelity attests a per-request exact capture, which only
	// the JSONL collector and proxy can substantiate — a manual import cannot.
	// Coverage % is keyed on fidelity='realtime' (store.DeveloperCosts), so
	// letting a client claim it here would fabricate high-fidelity capture (the
	// same provenance laundering #34 closed for source). Default an omitted
	// fidelity to "estimated" (a manual POST asserts no cadence), accept
	// "daily"/"estimated", and reject "realtime" with 400. Scope: this closes
	// the realtime-NUMERATOR fabrication only; the cost_usd that feeds the
	// metric denominators is bounded by the bearer gate (#22), i.e. we trust an
	// authenticated client not to post garbage totals — a separate concern.
	// Exact-match by design (mirrors the source switch): "Realtime"/" realtime"
	// fall through to the enum-rejection branch, still a 400.
	switch req.Fidelity {
	case "":
		req.Fidelity = "estimated"
	case "daily", "estimated":
		// explicit, allowed
	case "realtime":
		writeError(w, http.StatusBadRequest,
			`fidelity "realtime" is reserved for automated capture (collector/proxy); use "daily" or "estimated" on /api/v1/costs`)
		return
	default:
		writeError(w, http.StatusBadRequest, `fidelity must be "daily", "estimated", or omitted on /api/v1/costs`)
		return
	}
	// Repo is deliberately unset here (#231), so every /costs row stores the
	// 'unqualified' sentinel and joins tolerantly, exactly as it did before.
	//
	// /costs is the UNTRUSTED manual-import surface (see the provenance note on
	// /events): it already forbids realtime fidelity and the jsonl source. Letting an
	// arbitrary caller assert a repository would let it aim a manual import at a real
	// repo's issue, which is a worse failure than the tolerant fusion it would fix.
	// The repo-aware ingest path is /api/v1/events, which is bearer-gated and shipper-
	// owned. If a manual importer ever needs repo scoping, that is its own issue.
	// Divergent-cost replay fails loud (#295, ruling A). A KEYED /costs re-post
	// (same idempotency_key) whose cost_micro differs from the stored row is
	// rejected with 409 by InsertManualCostEvent — cost_micro stays IMMUTABLE at
	// the first writer's value (#233; it is absent from the ON CONFLICT DO UPDATE
	// set), so a 409 REJECTS the correction, it never overwrites. An IDENTICAL
	// re-post (same key, same cost_micro) stays idempotent: the row's token counts
	// MAX-merge and the endpoint returns 201, exactly as before. Comparison is on
	// the stored INTEGER cost_micro, so an honest retry whose float cost_usd / FX /
	// rounding jitter lands on the same micro value is NOT a conflict. To CHANGE a
	// figure, post under a NEW idempotency_key (or delete + repost). A sanctioned
	// audited override (ruling C) is a separate follow-up.
	ev := store.TokenEvent{
		Developer:      req.Developer,
		IssueID:        req.IssueID,
		Model:          req.Model,
		InputTok:       req.InputTok,
		OutputTok:      req.OutputTok,
		CacheRead:      req.CacheRead,
		CacheWrite5m:   req.CacheWrite5m,
		CacheWrite1h:   req.CacheWrite1h,
		CostMicro:      store.DollarsToMicro(req.CostUSD),
		Source:         req.Source,
		Fidelity:       req.Fidelity,
		IdempotencyKey: req.IdempotencyKey,
		Timestamp:      time.Now().UTC(),
	}
	if err := h.store.InsertManualCostEvent(r.Context(), ev); err != nil {
		if errors.Is(err, store.ErrCostConflict) {
			// A keyed re-post changed the cost. cost_micro is immutable (#233), so
			// this is a rejection, not an overwrite — the stored figure is unchanged.
			// 409 tells the client its correction was NOT applied. The client-facing
			// remedy names ONLY the path a client can actually take over HTTP — a new
			// idempotency_key. (Deleting a single cost row is operator-level, direct
			// against the store; there is no delete-by-key API route, so advertising
			// it here would point the client at a door it cannot open.)
			writeError(w, http.StatusConflict,
				"idempotency_key already recorded with a different cost_usd; the stored cost is immutable. To correct it, re-post under a new idempotency_key")
			return
		}
		h.logger.Error("insert manual cost", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// --- POST /api/v1/actual_spend ---

// actualSpendRequest is the body posted by finance to record the
// enterprise-contract invoice total for a developer for a billing month.
//
// Period is YYYY-MM. Multiple posts for the same (developer, period)
// accumulate — credit memos and corrections enter as additive deltas
// (positive or negative) and the SUM at query time yields the net
// (#24). The audit trail lives in row history rather than overwriting.
type actualSpendRequest struct {
	Developer     string  `json:"developer"`
	Period        string  `json:"period"`
	ActualPaidUSD float64 `json:"actual_paid_usd"`
}

// maxActualSpendBody caps the request body at 1 MiB — anything larger is
// either malicious or malformed; the legitimate payload is a few hundred bytes.
const maxActualSpendBody = 1 << 20

// minPeriodYear, maxPeriodYear bound period inputs to a sane range so a typo
// like "0202-05" or "9999-12" doesn't silently corrupt lexicographic ordering
// forever. Adjust the upper bound if this code is still running in 2050.
const (
	minPeriodYear = 2020
	maxPeriodYear = 2050
)

// maxIdentifierLen caps the length of developer / org / issue_id strings.
// Real values are usernames or org slugs — none should exceed a few dozen
// characters. The cap prevents a client from filling SQLite with multi-KB
// identifier strings (a trivial storage DoS that the 1 MiB body limit
// would otherwise still allow per request).
const maxIdentifierLen = 256

// validateRepo normalizes an OPTIONAL client-supplied repository field (#231) into
// the value stored in the `repo` column.
//
// Empty -> repoid.Unqualified. That is deliberate and is what makes `repo` a purely
// additive API change: every pre-#231 client omits it and lands exactly where its
// rows landed before, joined tolerantly.
//
// The reserved sentinel cannot be supplied explicitly. A producer that could not
// determine its repository must say so by OMITTING the field; letting a client
// assert "unqualified" would let it opt out of repo-scoping on purpose and re-fuse
// two repos' issues. Same discipline that forbids forging collector.UnattributedIssueID.
func validateRepo(s string) (string, error) {
	if s == "" {
		return repoid.Unqualified, nil
	}
	if len(s) > maxIdentifierLen {
		return "", fmt.Errorf("repo must be <= %d chars", maxIdentifierLen)
	}
	if strings.EqualFold(strings.TrimSpace(s), repoid.Unqualified) {
		return "", fmt.Errorf("repo %q is reserved; omit the field when the repository is unknown", repoid.Unqualified)
	}
	slug, ok := repoid.Canonical(s)
	if !ok {
		return "", fmt.Errorf(`repo must be a canonical "owner/repo" slug (got %q)`, s)
	}
	return slug, nil
}

// handlePostActualSpend records the enterprise-contract invoice total for a
// developer for a billing month. Wrapped by requireAuth in Register so
// TIER_API_TOKEN is required when configured (#22 landed the gate).
func (h *Handler) handlePostActualSpend(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxActualSpendBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req actualSpendRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Reject trailing JSON after the first object — a second object would
	// otherwise be silently dropped and "succeed".
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}

	if req.Developer == "" {
		writeError(w, http.StatusBadRequest, "developer is required")
		return
	}
	if len(req.Developer) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "developer must be <= 256 chars")
		return
	}
	if err := validatePeriod(req.Period); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if math.IsNaN(req.ActualPaidUSD) || math.IsInf(req.ActualPaidUSD, 0) {
		writeError(w, http.StatusBadRequest, "actual_paid_usd must be finite")
		return
	}
	// Magnitude cap (#118): negatives are legal here (credit memos, #24), so
	// bound |value| — the negative-overflow case is the amd64 SUM-poison vector
	// (a bare float64→int64 of -1e19 yields a huge POSITIVE number there).
	// DollarsToMicro saturates deterministically as a backstop; this fails loud.
	if math.Abs(req.ActualPaidUSD) > store.MaxCostUSD {
		writeError(w, http.StatusBadRequest, "actual_paid_usd magnitude must be <= 1e12")
		return
	}
	// Negatives are accepted as credit memos / refunds (#24). The store sums
	// across rows at query time so a $500 invoice + a $-100 credit memo
	// nets to $400.
	if err := h.store.InsertActualSpend(r.Context(), store.ActualSpend{
		Developer:       req.Developer,
		Period:          req.Period,
		ActualPaidMicro: store.DollarsToMicro(req.ActualPaidUSD),
		Timestamp:       time.Now().UTC(),
	}); err != nil {
		h.logger.Error("insert actual_spend", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	h.warnOverBudget(r.Context(), req.Period)
	w.WriteHeader(http.StatusCreated)
}

// warnOverBudget emits a WARN data-quality signal for the just-written period
// if active members' tier-1 invoices now exceed an org's contract there — the
// condition that silently clamps the org-fallback share to 0 (#94 item 2),
// usually a data-entry error. Fired at INGESTION (not on the per-request read
// path) so the signal is event-driven, not re-logged on every /scores call.
//
// Best-effort: the spend write has already succeeded, so a lookup failure here
// must never fail the request — it only logs. We report only the period just
// written: a SPEND write to period p cannot change another period's
// over/under-budget status. (Membership changes CAN — they flow through a
// different write path and are out of scope for this ingestion-time signal.)
//
// OverBudgetPeriods is queried with since=period, so in steady state (writing
// the current month) it returns just that month — no over-scan. Only a
// historical-period correction scans later months too, which we discard via
// the p.Period filter; that residual is negligible at SQLite scale and keeps
// OverBudgetPeriods a single general method a future /diagnostics endpoint can
// reuse for all periods, rather than a narrower one-period variant.
func (h *Handler) warnOverBudget(ctx context.Context, period string) {
	since, err := time.Parse("2006-01", period)
	if err != nil {
		return // period was already validated upstream; defensive only
	}
	over, err := h.store.OverBudgetPeriods(ctx, since)
	if err != nil {
		h.logger.Warn("over-budget check failed", "period", period, "err", err)
		return
	}
	for _, p := range over {
		if p.Period != period {
			continue
		}
		h.logger.Warn("org over budget: active-member tier-1 invoices exceed the org contract for this period; org-fallback share clamped to 0 (likely a data-entry error)",
			"org", logSafeStr(p.Org), "period", p.Period,
			"org_total_usd", p.OrgTotal, "tier1_sum_usd", p.Tier1Sum, "overage_usd", p.Overage)
	}
}

// --- POST /api/v1/org_actual_spend ---

// orgActualSpendRequest is the body posted by finance to record the
// enterprise-contract invoice total for an entire org for a billing month.
// Used when one contract covers N developers (the common Anthropic / OpenAI
// enterprise case). For tools that produce per-seat invoices (Cursor
// Business, etc.) keep using POST /actual_spend instead.
//
// Period is YYYY-MM. Multiple posts for the same (org, period) accumulate
// — credit memos and corrections enter as additive deltas (#24).
type orgActualSpendRequest struct {
	Org           string  `json:"org"`
	Period        string  `json:"period"`
	ActualPaidUSD float64 `json:"actual_paid_usd"`
}

// handlePostOrgActualSpend records an org-level invoice total (#23). Same
// validation surface as handlePostActualSpend — the only schema difference
// is the key (org instead of developer). Wrapped by requireAuth in
// Register, matching the auth posture of the other finance-grade write.
func (h *Handler) handlePostOrgActualSpend(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxActualSpendBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req orgActualSpendRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}

	if req.Org == "" {
		writeError(w, http.StatusBadRequest, "org is required")
		return
	}
	if len(req.Org) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "org must be <= 256 chars")
		return
	}
	if err := validatePeriod(req.Period); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if math.IsNaN(req.ActualPaidUSD) || math.IsInf(req.ActualPaidUSD, 0) {
		writeError(w, http.StatusBadRequest, "actual_paid_usd must be finite")
		return
	}
	// Magnitude cap (#118): negatives are legal here (credit memos, #24), so
	// bound |value| — mirrors handlePostActualSpend. The negative-overflow case
	// is the amd64 SUM-poison vector; DollarsToMicro saturates as a backstop,
	// this fails loud at the boundary.
	if math.Abs(req.ActualPaidUSD) > store.MaxCostUSD {
		writeError(w, http.StatusBadRequest, "actual_paid_usd magnitude must be <= 1e12")
		return
	}
	// Negatives accepted as credit memos / refunds (#24); rows accumulate.
	if err := h.store.InsertOrgActualSpend(r.Context(), store.OrgActualSpend{
		Org:             req.Org,
		Period:          req.Period,
		ActualPaidMicro: store.DollarsToMicro(req.ActualPaidUSD),
		Timestamp:       time.Now().UTC(),
	}); err != nil {
		h.logger.Error("insert org_actual_spend", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	h.warnOverBudget(r.Context(), req.Period)
	w.WriteHeader(http.StatusCreated)
}

// validatePeriod parses a YYYY-MM string and rejects out-of-range years.
// Using time.Parse subsumes the regex check (it enforces 4-digit year and
// 01-12 month) AND gives us range validation in one pass.
func validatePeriod(s string) error {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return fmt.Errorf("period must be YYYY-MM (e.g. 2026-05)")
	}
	if t.Year() < minPeriodYear || t.Year() > maxPeriodYear {
		return fmt.Errorf("period year must be between %d and %d", minPeriodYear, maxPeriodYear)
	}
	// Re-format to canonical form so callers can't smuggle in oddities like
	// "2026-1" (which time.Parse accepts but breaks lexicographic ordering
	// against the canonical "2026-01"). Reject if the input doesn't round-trip.
	if t.Format("2006-01") != s {
		return fmt.Errorf("period must be YYYY-MM (e.g. 2026-05), got %q", s)
	}
	return nil
}

// --- GET /api/v1/scores ---

// priceTableJSON is the top-level provenance stamp on every /scores response
// (#233): the version + effective_date of the price table that produced the cost
// figures in this response. A CFO comparing Q1 to Q3 needs to know whether a spend
// trend is real or partly a table-bump artifact, and an org needs to verify it
// priced against the same table another org did. Always present.
type priceTableJSON struct {
	Version       int    `json:"version"`
	EffectiveDate string `json:"effective_date"`
}

// rubricJSON stamps which canonical NORMATIVE-rubric version produced the
// weighted-point figures in this response (#239) — the weight-rubric analogue of
// priceTableJSON. Always present, read from scoring.RubricVersion. It is
// PROVENANCE: a matched rubric.version (with a matched price_table.version) is a
// NECESSARY condition for comparing a weighted point / TIER / cost_per_point
// across responses, and it makes a change to the rubric DEFINITION detectable
// over time. It is not SUFFICIENT for a cross-org comparison: two orgs on the
// same binary stamp the same version however generously each labels, so matched
// versions must be paired with the shared normative calibration (and matched
// outcomes.size_labels). See docs/rubric.md for the "what you may / may not
// compare" rules.
type rubricJSON struct {
	Version int `json:"version"`
}

type scoresResponse struct {
	Since string `json:"since"`
	// Aggregation names the ANONYMIZED grouping level whose rows populate Teams
	// (#270): "team" (#185) or "division". It is the discriminator that tells a
	// consumer what each Teams row's label means — a division mode response is
	// otherwise structurally identical to a team mode one. omitempty so developer
	// mode (which ships named Developers, no Teams) sends no key at all. This is
	// the extension seam: a future org/department level sets its own name here
	// with the SAME Teams array, no new response field.
	Aggregation string `json:"aggregation,omitempty"`
	// PriceTable stamps which price-table version produced the cost figures below
	// (#233) — always present, read from the server's active table.
	PriceTable priceTableJSON `json:"price_table"`
	// Rubric stamps the canonical weight-rubric version (#239) — always present,
	// read from scoring.RubricVersion — so a consumer can verify a comparison holds
	// the weight rubric constant, exactly as PriceTable pins the dollars. It is a
	// provenance stamp against rubric-DEFINITION drift over time; the cross-org
	// generosity guard is the shared NORMATIVE rubric (docs/rubric.md), not this
	// integer (see rubricJSON).
	Rubric rubricJSON `json:"rubric"`
	// Total is the rollup across ALL developers in this response, computed
	// server-side via scoring.RollupTeam (#25). Populated unconditionally
	// when there is at least one developer; nil when Developers is empty.
	// The dashboard reads this directly instead of re-summing client-side —
	// keeps one source of truth and avoids precision loss when reconstructing
	// CoveragePercent from a rounded percent × per-developer cost.
	Total      *teamScoreJSON       `json:"total,omitempty"`
	Developers []developerScoreJSON `json:"developers"`
	// Teams is populated ONLY in team-aggregation mode (#185): named per-developer
	// rows are replaced by k-anonymized team aggregates (each row is a team with
	// at least k contributing developers, or the residual "other" bucket), and
	// Developers is emitted empty. omitempty so developer mode ships no `teams`
	// key at all. No element ever carries an individual developer name.
	Teams []teamScoreJSON `json:"teams,omitempty"`
	// Team is populated only when ?team=NAME filters the response to one
	// team's developers. Retained for backwards compatibility with the
	// pre-#25 scoped-team workflow.
	Team *teamScoreJSON `json:"team,omitempty"`
	// DataQuality surfaces the zero-token-outcome tripwire (#136): outcomes
	// merged with fewer than scoring.MinAttributableTokens recorded tokens in
	// their attributable window. omitempty so a clean window ships no key at all
	// (the dashboard hides the panel when absent).
	DataQuality *dataQualityJSON `json:"data_quality,omitempty"`
	// WorkTypes is the type-segmented view (#187): one entry per work_type present
	// in the window (or the single entry a ?work_type filter selects), each a
	// self-contained score computed over ONLY that category's outcomes with cost
	// attributed at (developer, issue) grain. This is the authoritative surface for
	// comparing developers/teams — a security engineer against other security work,
	// never against feature work. The top-level Developers/Teams/Total above are the
	// POOLED population summary retained for back-compat; they are NOT a cross-type
	// ranking, and cross-type TIER comparison is UNSUPPORTED by design. Each segment
	// composes with team-aggregation mode (#185): in team mode its rows are
	// k-anonymized team aggregates WITHIN the type, never individual names. omitempty
	// so a window with no outcomes ships no key.
	WorkTypes []workTypeSegmentJSON `json:"work_types,omitempty"`
	// CostComposition surfaces WHERE the window's spend went (#234): cost by
	// normalized model, the per-class token composition, attributed vs unattributed
	// spend, and the two optimization levers (cache_read_share, premium_model_share).
	// A whole-window, name-free aggregate — strictly coarser than the pooled `total`
	// already present, so it carries in BOTH developer and team-aggregation mode
	// without re-exposing any sub-k cohort (#185). Pure sidecar: the TIER formula is
	// untouched, same discipline as data_quality (#136). omitempty so a window with
	// no token spend ships no key and the dashboard panel stays hidden.
	CostComposition *costCompositionJSON `json:"cost_composition,omitempty"`
}

// workTypeSegmentJSON is one work-type's scoped leaderboard (#187). Exactly one of
// Developers (developer mode) or Teams (team-aggregation mode, #185) carries rows;
// Total is the segment's rollup. The numbers are type-scoped: WeightedPoints counts
// only this category's outcomes and TotalCostUSD is the cost of the (developer,
// issue) pairs those outcomes attach to. ActualPaidUSD/SpendLeverage are always 0
// here — finance's actual_spend ledger is per (developer, period), not per category,
// so it cannot be split by work_type without inventing an allocation; the sidecar is
// deliberately left at the pooled top level.
type workTypeSegmentJSON struct {
	WorkType string `json:"work_type"`
	// Aggregation mirrors scoresResponse.Aggregation for this segment (#270): the
	// anonymized level ("team"/"division") whose rows populate Teams, omitted in
	// developer mode. Present per-segment so a consumer reading only work_types
	// still knows what each Teams row's label means.
	Aggregation string               `json:"aggregation,omitempty"`
	Developers  []developerScoreJSON `json:"developers,omitempty"`
	Teams       []teamScoreJSON      `json:"teams,omitempty"`
	Total       *teamScoreJSON       `json:"total,omitempty"`
}

// dataQualityJSON is the top-level data-quality block (#136). Today it carries
// only the zero-token outcomes; it is a struct (not an inline slice) so future
// data-quality signals can be added without another top-level key.
type dataQualityJSON struct {
	// ZeroTokenOutcomes lists the flagged (developer, issue) pairs in
	// per-developer mode. It is omitempty because team-aggregation mode (#185)
	// suppresses it entirely — the per-developer/issue identities would defeat
	// k-anonymity — and reports only the aggregate ZeroTokenOutcomeCount instead.
	ZeroTokenOutcomes []zeroTokenOutcomeJSON `json:"zero_token_outcomes,omitempty"`
	// ZeroTokenOutcomeCount is the name-free aggregate used in team-aggregation
	// mode (#185): the number of zero-token-flagged outcomes in the window, with
	// no developer or issue identity attached. omitempty so per-developer mode
	// (which ships the named list above) does not also emit this key.
	ZeroTokenOutcomeCount int `json:"zero_token_outcome_count,omitempty"`
	// MixedPriceVersions is present (ascending, len >= 2) ONLY when the window's
	// token_events span more than one price_table version (#293). cost_micro is
	// immutable per row (#233), so a window legitimately mixes rows priced under
	// older versions with rows at the active version; the single top-level
	// price_table.version stamp would otherwise imply the whole window priced under
	// one table. It lists the DISTINCT versions that priced rows in the window, so
	// len() is the count and the values name which tables. Name-free (integers only),
	// so it carries identically in developer and team-aggregation mode (#185), unlike
	// the zero-token identities above. omitempty so a uniform or empty window ships no
	// key and the dashboard mixed-version banner stays hidden.
	MixedPriceVersions []int `json:"mixed_price_versions,omitempty"`
	// AttributedCostShare is the TRUE attribution coverage of window spend (#351):
	// the fraction of window cost_micro that joins to a REAL issue rather than the
	// UnattributedIssueID sentinel (attributed / total, in [0,1]). It is the honest
	// coverage the earlier `coverage_pct` was mistaken for: coverage_pct is per-
	// developer CAPTURE FIDELITY (realtime vs. estimated of the spend we DID record)
	// and reads ~100% even when most spend never attributes to an issue; this field
	// is the completeness the adopter must see up front ("we can account for 22% of
	// your spend"). It measures issue-attribution, which is NECESSARY BUT NOT SUFFICIENT
	// for a score: spend on a real issue that has no OUTCOME still counts here yet
	// contributes to no TIER. So read it WITH AttributedOutcomeShare — the two measure
	// DIFFERENT joins (cost→issue here, outcome→cost there) and are not expected to
	// reconcile; a high cost_share + low outcome_share + non-empty UnjoinedDevelopers is
	// the silent-TIER=0 signature. A pointer so a genuine 0.0 (all spend unattributed) is emitted,
	// not dropped by omitempty; nil (omitted) only when the window has no spend at all.
	// Name-free, so it carries identically in developer and team-aggregation mode (#185).
	// Numerically this is 1 − cost_composition.unattributed_share (the same integer
	// micros, each divided independently, so the two floats reconcile to ~1 ULP);
	// it assumes non-negative attributed micros (token cost, never refunds here), so
	// the value stays in [0,1].
	AttributedCostShare *float64 `json:"attributed_cost_share,omitempty"`
	// AttributedOutcomeShare is the outcome-side join rate (#351): the fraction of the
	// window's outcomes whose canonical (developer, repo, issue) has ANY matching token
	// spend, in [0,1]. It falls to ~0 under the silent-identity-zero failure mode — cost
	// keyed to an OS username, outcomes to a GitHub login, un-aliased — so the two halves
	// never meet. Pointer for the same reason as AttributedCostShare; nil only when the
	// window has no outcomes. Name-free, carries in both modes.
	AttributedOutcomeShare *float64 `json:"attributed_outcome_share,omitempty"`
	// UnjoinedDevelopers flags the identity-mismatch failure mode (#351/#125): developers
	// present on only ONE side of the cost/outcome join — cost but no outcomes, or
	// outcomes but no cost — who otherwise read a silent TIER=0 while a misleading org
	// total still prints. Present only when at least one side is non-empty (omit-when-
	// clean). In developer mode it NAMES them so the operator can map the aliases; in
	// team-aggregation mode (#185) the names are suppressed and only the counts carry,
	// through the same k-anon guard as the zero-token identities above.
	UnjoinedDevelopers *unjoinedDevelopersJSON `json:"unjoined_developers,omitempty"`
	// ExploratoryCostShare is the org window's exploratory-overhead share (#refocus,
	// Option B): cost on a mainline branch with no issue (the "unattributed:main"
	// bucket) / total window cost, in [0,1]. It is the honest headline for the
	// ~work-without-an-issue overhead TIER deliberately KEEPS in the denominator —
	// exploratory/planning spend is part of the yield equation, so it is shown, not
	// excluded. Emitted alongside unattributed_buckets — i.e. ONLY when the window
	// has some unattributed spend; a fully-attributed or empty window omits it (nil).
	// When present it is a pointer so a genuine 0.0 (unattributed spend exists but
	// none of it is exploratory main) is emitted, not dropped by omitempty. Name-free,
	// carries in both developer and team-aggregation mode (#185).
	ExploratoryCostShare *float64 `json:"exploratory_cost_share,omitempty"`
	// UnattributedBuckets is the labeled split of the single unattributed mass the
	// cost-composition sidecar reports as one number (#refocus, Option B): one row per
	// reason the join could not tie spend to an issue (main/exploratory, detached-head,
	// branch-without-issue, plus the base sentinel for host-blind producers). The rows
	// sum to the composition's unattributed cost; each Share is of TOTAL window cost so
	// they compose with attributed_cost_share. Name-free (bucket labels only), so it
	// carries identically in both modes. Present only when the window has unattributed
	// spend (omit-when-clean).
	UnattributedBuckets []unattributedBucketJSON `json:"unattributed_buckets,omitempty"`
	// CostCoverageStart is this installation's COST HORIZON (#512): the RFC3339
	// instant of the earliest captured token event, or omitted when the store holds
	// no cost at all.
	//
	// It exists because outcomes and cost do not share a start date. Outcomes arrive
	// by webhook and backfill regardless of when TIER was installed, while cost only
	// exists from the horizon forward — so a window reaching back past the horizon
	// divides a FULL window of outcomes by a PARTIAL window of cost and reports a
	// silently INFLATED TIER. Measured at about twice on a real multi-repo install.
	//
	// This is NOT a log-retention artifact and must not be documented as one. Extracted
	// events are append-only forever and outlive the provider session logs they came
	// from; the horizon is simply the date capture began. It is permanent per install
	// and no amount of raw-log retention moves it — a brand-new install has a horizon
	// of today even with years of logs on disk.
	//
	// Name-free, so it carries identically in developer and team-aggregation mode (#185).
	CostCoverageStart string `json:"cost_coverage_start,omitempty"`
	// CostCoverageSafeSince is the earliest `since` value that will NOT predate the
	// horizon, as a plain date — the remedy, precomputed here so the dashboard, the
	// docs and `tierd doctor` cannot each derive it slightly differently and hand
	// operators three different instructions. See safeSinceDay for why it is not
	// simply CostCoverageStart's own day.
	CostCoverageSafeSince string `json:"cost_coverage_safe_since,omitempty"`
	// WindowPredatesCostCapture is true when the requested `since` is EARLIER than
	// CostCoverageStart — i.e. this response's TIER is inflated by the mismatch above,
	// and by how much depends on how many outcomes sit in the uncovered head of the
	// window. A pointer so a genuine false is emitted rather than dropped by omitempty:
	// "we checked and the window is fully covered" is a materially different statement
	// from "no signal", and a consumer must be able to tell them apart. nil only when
	// there is no horizon to compare against (an empty store).
	WindowPredatesCostCapture *bool `json:"window_predates_cost_capture,omitempty"`
	// SourceCoverageStart maps each capture source to its own horizon (#512). The
	// global horizon above is the LOOSEST bound: a window can clear it and still
	// predate a given source entirely, counting that source's outcomes against none
	// of its cost. Emitted only when more than one source has recorded cost, since a
	// single-source install learns nothing the global horizon did not already say.
	SourceCoverageStart map[string]string `json:"source_coverage_start,omitempty"`
}

// unattributedBucketJSON is one labeled slice of unattributed spend (#refocus,
// Option B). Bucket is the stable sentinel label ("unattributed:main",
// "unattributed:detached-head", "unattributed:branch-without-issue", or the base
// "unattributed"); CostUSD is its window spend; Share is of TOTAL window cost so a
// consumer can read it directly against attributed_cost_share. Name-free.
type unattributedBucketJSON struct {
	Bucket  string  `json:"bucket"`
	CostUSD float64 `json:"cost_usd"`
	Share   float64 `json:"share"`
}

// unjoinedDevelopersJSON is the wire shape of the unjoined-developer flag (#351). The
// two counts are ALWAYS present when the block is (they are name-free, the loud signal
// an operator and a scraper both read); the two name lists are populated ONLY in
// developer mode and suppressed in team-aggregation mode (#185) to preserve k-anonymity
// — mirroring how zero_token_outcomes collapses to a count in team mode.
type unjoinedDevelopersJSON struct {
	// CostOnly names developers with cost rows but no outcomes in the window (developer
	// mode only). omitempty so team mode ships no names.
	CostOnly []string `json:"cost_only,omitempty"`
	// OutcomeOnly names developers with outcomes but no cost rows (developer mode only).
	OutcomeOnly []string `json:"outcome_only,omitempty"`
	// CostOnlyCount and OutcomeOnlyCount are the name-free magnitudes, always emitted so
	// the signal survives in team mode and a machine consumer reads the count directly.
	CostOnlyCount    int `json:"cost_only_count"`
	OutcomeOnlyCount int `json:"outcome_only_count"`
}

// zeroTokenOutcomeJSON names one flagged (developer, issue) pair and the token
// total that tripped the flag (#136). Developer is the canonical identity.
type zeroTokenOutcomeJSON struct {
	Developer string `json:"developer"`
	IssueID   string `json:"issue_id"`
	Tokens    int64  `json:"tokens"`
}

// costCompositionJSON is the wire shape of the cost-composition sidecar (#234).
// Dollar figures are USD (converted once from exact micro-dollars at this
// boundary via store.MicroToDollars, like every other served cost); the shares
// are fractions in [0,1]. The reconciliation is exact in the underlying integer
// micro-dollars (store.CostComposition): attributed + unattributed == total and
// sum(by_model) == total with no residual bucket. Each field is converted to USD
// independently, so the served floats reconcile to micro-dollar precision (~1 ULP),
// not necessarily bit-exactly — the same rounding every other served cost carries.
type costCompositionJSON struct {
	TotalCostUSD        float64 `json:"total_cost_usd"`
	AttributedCostUSD   float64 `json:"attributed_cost_usd"`
	UnattributedCostUSD float64 `json:"unattributed_cost_usd"`
	UnattributedShare   float64 `json:"unattributed_share"`
	// CacheReadShare is the input-side cache-hit share (docs/pricing-philosophy.md
	// §4): cache_read / (input + cache_read + cache_write). PremiumModelShare is the
	// SPEND share on premium-tier models (store.IsPremiumModel). Both fractions.
	CacheReadShare    float64         `json:"cache_read_share"`
	PremiumModelShare float64         `json:"premium_model_share"`
	ByModel           []modelCostJSON `json:"by_model"`
	ByClass           classTokensJSON `json:"by_class"`
}

// modelCostJSON is one by-model row: normalized model, serving host (#300, so an
// open-weights model split across hosts stays two rows), USD spend, its share of
// window spend, and whether it prices premium-tier.
type modelCostJSON struct {
	Model   string  `json:"model"`
	Host    string  `json:"host"`
	CostUSD float64 `json:"cost_usd"`
	Share   float64 `json:"share"`
	Premium bool    `json:"premium"`
}

// classTokensJSON is the per-class TOKEN composition (#234) — exact counts from the
// token_events class columns, NOT allocated dollars (a stored cost_micro is a single
// blended figure per event, so a per-class dollar split is not exactly recoverable;
// counts are the honest primitive and drive cache_read_share). cache_write is the
// 5m + 1h buckets summed — the sidecar reports total cache-write volume, not the TTL
// split.
type classTokensJSON struct {
	InputTok   int64 `json:"input_tok"`
	OutputTok  int64 `json:"output_tok"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// newCostCompositionJSON maps the store's derived composition onto the wire shape,
// converting micro-dollars to USD once at this boundary. Returns nil when the
// window has no token spend (TotalCostMicro == 0) so the response omits the key and
// the dashboard panel stays hidden — mirrors the data_quality omit-when-empty rule.
func newCostCompositionJSON(c store.CostComposition) *costCompositionJSON {
	if c.TotalCostMicro == 0 {
		return nil
	}
	out := &costCompositionJSON{
		TotalCostUSD:        store.MicroToDollars(c.TotalCostMicro),
		AttributedCostUSD:   store.MicroToDollars(c.AttributedCostMicro),
		UnattributedCostUSD: store.MicroToDollars(c.UnattributedCostMicro),
		UnattributedShare:   c.UnattributedShare,
		CacheReadShare:      c.CacheReadShare,
		PremiumModelShare:   c.PremiumModelShare,
		ByModel:             make([]modelCostJSON, 0, len(c.ByModel)),
		ByClass: classTokensJSON{
			InputTok:   c.ByClass.InputTok,
			OutputTok:  c.ByClass.OutputTok,
			CacheRead:  c.ByClass.CacheRead,
			CacheWrite: c.ByClass.CacheWrite5m + c.ByClass.CacheWrite1h,
		},
	}
	for _, m := range c.ByModel {
		out.ByModel = append(out.ByModel, modelCostJSON{
			Model:   m.Model,
			Host:    m.Host,
			CostUSD: store.MicroToDollars(m.CostMicro),
			Share:   m.Share,
			Premium: m.Premium,
		})
	}
	return out
}

type developerScoreJSON struct {
	Developer       string  `json:"developer"`
	TIER            float64 `json:"tier"`
	WeightedPoints  float64 `json:"weighted_points"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	ActualPaidUSD   float64 `json:"actual_paid_usd"`
	SpendLeverage   float64 `json:"spend_leverage"`
	CoveragePercent float64 `json:"coverage_pct"`
	// ExploratoryCostShare is this developer's exploratory-overhead share (#refocus,
	// Option B): their cost on a mainline branch with no issue / their total window
	// cost, in [0,1]. The per-developer companion to data_quality.exploratory_cost_share.
	// It is naturally k-anon-safe (#185): developer rows are not emitted at all in
	// team-aggregation mode, so a per-developer share cannot leak a sub-k cohort — the
	// same suppression the other per-developer fields rely on. 0 when the developer has
	// no spend.
	ExploratoryCostShare float64 `json:"exploratory_cost_share"`
	// CostPerPoint is USD per weighted point (#239): the inverse-unit dual of TIER,
	// stamped alongside price_table.version (dollars) and rubric.version (weights)
	// so a self-over-time or matched-rubric cross-org comparison is well-founded.
	// NULL (a pointer) when weighted_points <= 0 (#472) — the complement of the engine's
	// points>0 guard, so it also nulls a net-negative-points row: "no accepted outcome"
	// is not "infinitely efficient", and a 0 there would sort as the MOST efficient row.
	// A zero-cost row WITH points keeps its honest 0 (a genuine FREE row is best).
	CostPerPoint *float64 `json:"cost_per_point"`
	// SampleN, CILow, CIHigh, and Ranked expose the ranking floor and bootstrap
	// CI (#133). The contract is the `ranked` flag: the server does NOT pre-sort
	// the array — both renderers apply the two-tier order themselves. CIs are 0
	// for unranked rows.
	SampleN int     `json:"sample_n"`
	CILow   float64 `json:"ci_low"`
	CIHigh  float64 `json:"ci_high"`
	// CostPerPointCILow/High are the 95% percentile-bootstrap interval for
	// cost_per_point (#239), derived by reciprocal transform (scoring.CostPerPointCI)
	// from the SAME TIER bootstrap that fills CILow/CIHigh — no second resample.
	// 0 for unranked rows, like the TIER CI. This is the SELF-relative interval —
	// cost_per_point against the developer's own resampled history; a cross-org
	// percentile RANK is a deferred follow-up (#239 item 4), not synthesized here.
	CostPerPointCILow  float64 `json:"cost_per_point_ci_low"`
	CostPerPointCIHigh float64 `json:"cost_per_point_ci_high"`
	Ranked             bool    `json:"ranked"`
	// FlaggedOutcomes counts this developer's zero-token-flagged outcomes (#136);
	// any non-zero value is why Ranked is false. Always present (mirrors sample_n)
	// so the contract is stable.
	FlaggedOutcomes int `json:"flagged_outcomes"`
}

// teamScoreJSON is the wire shape for a k-anonymized GROUP aggregate (team #185 or
// division #270). It deliberately has NO Developers field: this absence is
// load-bearing for k-anonymity — it is the type-level barrier that stops the named
// developer slice scoring.RollupTeam populates (and AggregateTeamsKAnon then nils at
// engine.go's anonymity boundary) from ever serializing through the anonymized Teams
// array. Do NOT add a Developers field here; a per-developer breakdown belongs on
// developerScoreJSON, which the anonymized modes never emit.
type teamScoreJSON struct {
	// Team is omitempty so the "total" rollup (#25, where the field is
	// empty by construction) doesn't ship a misleading `"team":""` key.
	// The filtered ?team= variant always sets a non-empty name and
	// renders normally.
	Team            string  `json:"team,omitempty"`
	TIER            float64 `json:"tier"`
	WeightedPoints  float64 `json:"weighted_points"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	ActualPaidUSD   float64 `json:"actual_paid_usd"`
	SpendLeverage   float64 `json:"spend_leverage"`
	CoveragePercent float64 `json:"coverage_pct"`
	// CostPerPoint is the team/segment inverse-unit (#239): USD per weighted point
	// on summed totals. No CI here — the bootstrap is a per-developer signal (#133),
	// so ci fields stay on developerScoreJSON only.
	CostPerPoint *float64 `json:"cost_per_point"`
}

// newTeamScoreJSON maps a scoring.TeamScore onto the wire shape (#25). One mapper
// keeps the five build sites — pooled `total`, the ?team filter, the k-anon
// `teams`, and the per-work_type segment `teams`/`total` — from drifting as
// fields are added (cost_per_point, #239, landed here as a single edit). Team
// flows straight from ts, so the empty-name rollup and a named ?team both work
// (teamScoreJSON.Team is omitempty).
// costPerPointOrNull encodes cost_per_point honestly (#472): nil (→ JSON null) when
// there are no accepted points, so "no accepted outcome" is never conflated with
// "infinitely efficient" — a 0 there sorts as the MOST efficient row for any consumer
// that ranks by the column, rendering pure waste as perfect efficiency. A row WITH
// points keeps its value, INCLUDING a legitimate 0 for a zero-cost (FREE) row, so
// genuine free work still reads as best. This is the presentation-boundary dual of the
// engine's rule that unshipped spend stays in the denominator.
func costPerPointOrNull(weightedPoints, costPerPoint float64) *float64 {
	if weightedPoints <= 0 {
		return nil
	}
	return &costPerPoint
}

func newTeamScoreJSON(ts scoring.TeamScore) teamScoreJSON {
	return teamScoreJSON{
		Team:            ts.Team,
		TIER:            ts.TIER,
		WeightedPoints:  ts.WeightedPoints,
		TotalCostUSD:    ts.TotalCostUSD,
		ActualPaidUSD:   ts.ActualPaidUSD,
		SpendLeverage:   ts.SpendLeverage,
		CoveragePercent: ts.CoveragePercent,
		CostPerPoint:    costPerPointOrNull(ts.WeightedPoints, ts.CostPerPoint),
	}
}

// newDeveloperScoreJSON maps a scoring.DeveloperScore plus its TIER bootstrap
// bounds onto the wire shape (#133/#239). ciLow/ciHigh are 0 for an unranked row;
// the cost_per_point self-relative CI is the reciprocal transform of them
// (scoring.CostPerPointCI), so an unranked row correctly gets (0,0) there too. One
// mapper for the two developer build sites (the pooled list and the per-work_type
// segment). The single-developer detail endpoint uses a different response struct
// and is intentionally not routed through here.
func newDeveloperScoreJSON(s scoring.DeveloperScore, ciLow, ciHigh float64) developerScoreJSON {
	cppLow, cppHigh := scoring.CostPerPointCI(ciLow, ciHigh)
	return developerScoreJSON{
		Developer:          s.Developer,
		TIER:               s.TIER,
		WeightedPoints:     s.WeightedPoints,
		TotalCostUSD:       s.TotalCostUSD,
		ActualPaidUSD:      s.ActualPaidUSD,
		SpendLeverage:      s.SpendLeverage,
		CoveragePercent:    s.CoveragePercent,
		CostPerPoint:       costPerPointOrNull(s.WeightedPoints, s.CostPerPoint),
		SampleN:            s.SampleN,
		CILow:              ciLow,
		CIHigh:             ciHigh,
		CostPerPointCILow:  cppLow,
		CostPerPointCIHigh: cppHigh,
		Ranked:             s.Ranked,
		FlaggedOutcomes:    s.FlaggedOutcomes,
	}
}

// warnUnjoined logs, at most once per (side, identifier) per process, that a
// canonical developer identity has cost rows but no outcomes (side="cost") or
// outcomes but no cost rows (side="outcome") — the silent-TIER-0 / vanishing-
// developer condition #125 makes visible. The sync.Map seen-set keeps a cron
// scraping /scores from re-logging the same identifiers on every read.
func (h *Handler) warnUnjoined(dev, side string) {
	key := side + "\x00" + dev
	if _, loaded := h.identitySeen.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	h.logger.Warn("developer identity has no join partner",
		"developer", logSafeStr(dev), "side", side,
		"hint", "map identities via POST /api/v1/developer_alias")
}

// bootstrapSeed1 and bootstrapSeed2 seed the per-request PRNG used for TIER
// confidence intervals (#133). A fixed seed makes /scores deterministic for
// identical data — the dashboard shows the same interval on every refresh
// instead of flickering with Monte-Carlo noise — while still exercising the
// full bootstrap. One rng is created per request and shared across developers
// (its stream advances between them), so the whole response is reproducible.
// The values are arbitrary (golden-ratio / splitmix64 constants).
const (
	bootstrapSeed1 uint64 = 0x9e3779b97f4a7c15
	bootstrapSeed2 uint64 = 0xc2b2ae3d27d4eb4f
)

// jointCIInputs derives the #495 joint-bootstrap inputs for one developer (or one
// work-type segment) from its outcomes and the canonicalized per-(developer, repo,
// issue) cost index. Each outcome contributes weight×quality to the numerator and
// its issue's list-price cost — split evenly across that issue's outcomes in this
// set, so a shared issue is not double-counted — to the resampled denominator.
// fixedCostUSD is the remainder of totalCostUSD (unattributed/exploratory spend plus
// the cost of issues with no outcome here): it never pairs with a resampled outcome,
// so holding it constant keeps the interval's centre on the point TIER, whose
// denominator is the developer's TOTAL cost. Clamped at 0 against float rounding —
// the per-issue costs are summed from the same rows as the developer total, so their
// sum never exceeds it.
func jointCIInputs(dev string, outcomes []scoring.Outcome, costIndex store.JoinIndex, totalCostUSD float64) (contribs, costs []float64, fixedCostUSD float64) {
	contribs = make([]float64, len(outcomes))
	costs = make([]float64, len(outcomes))
	// Outcomes per (developer, repo, issue) so a shared issue's cost is split evenly.
	perIssue := make(map[store.DevIssue]int, len(outcomes))
	for _, o := range outcomes {
		perIssue[store.DevIssue{Developer: dev, Repo: o.Repo, IssueID: o.IssueID}]++
	}
	var attributed float64
	for i, o := range outcomes {
		contribs[i] = o.Weight * o.Quality
		n := perIssue[store.DevIssue{Developer: dev, Repo: o.Repo, IssueID: o.IssueID}]
		if n == 0 {
			continue
		}
		costs[i] = store.MicroToDollars(costIndex.Sum(dev, o.Repo, o.IssueID)) / float64(n)
		attributed += costs[i]
	}
	// costIndex.Sum is repo-TOLERANT (store.RepoMatch): a repo-blind cost bucket can
	// be pulled toward more than one distinct outcome key, so Σ attributed can exceed
	// the developer's exact-partition total (totalCostUSD) under mixed capture. When it
	// does, scale the per-outcome costs down proportionally so the identity resample's
	// denominator equals the published total exactly — preserving the point TIER as the
	// interval's centre AND the relative weight↔cost distribution, which clamping only
	// the remainder to 0 would distort. (The work-type segment site can't reach this:
	// its total is itself Σ over the same tolerant Sums, so attributed == total there.)
	if attributed > totalCostUSD && attributed > 0 {
		scale := totalCostUSD / attributed
		for i := range costs {
			costs[i] *= scale
		}
		attributed = totalCostUSD
	}
	fixedCostUSD = totalCostUSD - attributed
	if fixedCostUSD < 0 {
		fixedCostUSD = 0 // float slack only; the scale above already caps attributed at total
	}
	return contribs, costs, fixedCostUSD
}

// developerWindowCI returns the 95% percentile-bootstrap TIER interval for one
// developer within a window (#133), or (0, 0) for an unranked row where an interval
// is meaningless. It is the SINGLE derivation of a developer's CI shared by the
// /scores developer branch and the /scores/compare significance test (#277): a fresh
// PRNG seeded with the fixed bootstrapSeed1/2 makes the interval a pure function of
// the developer's own outcomes and their per-issue cost (#495) — identical on /scores
// and compare for the same window — so the significance flag can never drift from
// what /scores would show. costIndex is windowScores.issueCostIndex (the shared
// per-outcome denominator). Bootstrap cost is ~b×n float ops per ranked developer, no
// caching — revisit if a deployment exceeds ~10k outcomes/dev/window.
func developerWindowCI(byDev map[string][]scoring.Outcome, costIndex store.JoinIndex, s scoring.DeveloperScore) (lo, hi float64) {
	if !s.Ranked {
		return 0, 0
	}
	contribs, costs, fixedCost := jointCIInputs(s.Developer, byDev[s.Developer], costIndex, s.TotalCostUSD)
	rng := rand.New(rand.NewPCG(bootstrapSeed1, bootstrapSeed2))
	return scoring.BootstrapCI(contribs, costs, fixedCost, scoring.DefaultBootstrapSamples, rng)
}

// windowScores is the canonicalized, joined result of one [since, until) window
// (#277): the shared output of loadWindow that both /scores and /scores/compare
// build their responses from. devScores is one row per canonical developer;
// byDev keeps each developer's per-outcome contributions for the bootstrap CI;
// costDevs marks which canonical identities had a cost row (for the /scores
// unjoined gauge); outcomes/canon/canonTokens are retained for the /scores
// work-type segmentation; totalMicro (per canonical developer) feeds the /scores
// per-developer exploratory_cost_share; joinedOutcomes feeds /scores'
// attributed_outcome_share; zeroTokenOutcomes and priceVersions feed the
// per-window data-quality block on BOTH endpoints. It never leaves the api
// package — no field is serialized directly.
type windowScores struct {
	devScores   []scoring.DeveloperScore
	byDev       map[string][]scoring.Outcome
	costDevs    map[string]bool
	outcomes    []store.Outcome
	canon       func(string) string
	canonTokens map[store.DevIssue]int64
	// issueCostIndex is the canonicalized per-(developer, repo, issue) list-price
	// cost, the per-outcome denominator the joint bootstrap CI resamples (#495).
	// Shared here so /scores and /scores/compare derive the SAME interval.
	issueCostIndex store.JoinIndex
	// issueCosts is the RAW per-(developer, issue) cost rows the index above was
	// built from, retained so the /scores work-type segment path reuses them instead
	// of re-scanning the same window (#333) — one DeveloperIssueCostsWindow read per
	// request feeds both the CI and the segments.
	issueCosts        []store.DevIssueCost
	totalMicro        map[string]int64
	joinedOutcomes    int
	zeroTokenOutcomes []zeroTokenOutcomeJSON
	priceVersions     []int
}

// loadWindow runs the store reads and the alias-canonicalized cost/outcome/token
// join for one half-open [since, until) window (#276), producing per-developer
// TIER scores and the zero-token tripwire (#136, #125). It is the single shared
// scores computation behind GET /scores and GET /scores/compare (#277), so the two
// endpoints can never diverge on how a developer's score — or the inputs to the
// k-anonymity aggregation — is derived from the raw rows. It deliberately does NOT
// touch the identity gauge or warnUnjoined (a /scores-only observability concern
// the caller owns), does NOT fold the #refocus unattributed buckets, and does NOT
// fetch the work-type / cost-composition sidecars (which /scores fetches itself and
// compare does not need) — those stay on the /scores path so a two-window compare
// pays only for the shared reads, twice.
func (h *Handler) loadWindow(ctx context.Context, since, until time.Time) (windowScores, error) {
	costs, err := h.store.DeveloperCostsWindow(ctx, since, until)
	if err != nil {
		return windowScores{}, fmt.Errorf("query costs: %w", err)
	}
	outcomes, err := h.store.AllOutcomesWindow(ctx, since, until)
	if err != nil {
		return windowScores{}, fmt.Errorf("query outcomes: %w", err)
	}
	// Zero-token tripwire (#136): windowed token totals per (developer, issue), one
	// bulk query (no per-outcome N+1). Keyed by the raw token_events developer;
	// canonicalized below through the same alias map as the cost join.
	tokenTotals, err := h.store.OutcomeTokenTotals(ctx, outcomes)
	if err != nil {
		return windowScores{}, fmt.Errorf("query token totals: %w", err)
	}
	// Bulk-fetch actual_spend so per-developer SpendLeverage is computed without an
	// N+1. Missing developer in the map → 0, which ComputeDeveloper treats as "no
	// actual_spend recorded yet".
	actualSpend, err := h.store.ActualSpendAllWindow(ctx, since, until)
	if err != nil {
		return windowScores{}, fmt.Errorf("query actual_spend: %w", err)
	}
	// Mixed-version signal (#293): the DISTINCT price_table versions that priced
	// token_events in this window. cost_micro is immutable per row (#233), so a
	// window legitimately spans versions while the response stamps a single active
	// price_table.version; this read surfaces the mix for the data-quality block.
	priceVersions, err := h.store.DistinctPriceVersionsWindow(ctx, since, until)
	if err != nil {
		return windowScores{}, fmt.Errorf("query price versions: %w", err)
	}
	// Alias map (#125): resolve each raw identifier (OS username on cost rows,
	// GitHub login on outcome rows) to its canonical developer BEFORE joining. One
	// bulk fetch, no N+1 (matches the #94 TeamsForDevelopers pattern); canon() is an
	// O(1) hash lookup per row, so the join stays linear in the rows it scans.
	aliases, err := h.store.DeveloperAliases(ctx)
	if err != nil {
		return windowScores{}, fmt.Errorf("query developer_alias: %w", err)
	}
	canon := func(id string) string {
		if c, ok := aliases[id]; ok {
			return c
		}
		return id
	}

	// Canonicalize + merge cost rows: sum list-price and realtime micro-dollars per
	// canonical developer (integer micro-dollars, exact — #69). costDevs records
	// which canonical identities have at least one cost row, for the unjoined
	// visibility signal the /scores caller computes.
	totalMicro := map[string]int64{}
	realtimeMicro := map[string]int64{}
	costDevs := map[string]bool{}
	for _, c := range costs {
		dev := canon(c.Developer)
		totalMicro[dev] += c.TotalCostMicro
		realtimeMicro[dev] += c.RealtimeCostMicro
		costDevs[dev] = true
	}

	// Re-key windowed token totals by canonical (developer, repo, issue) so the
	// tripwire compares like-for-like with the outcome's canonical identity
	// (#136 + #125): aliased tokens (recorded under an OS username) and the
	// outcome (recorded under a GitHub login) collapse to one key. repo stays on
	// the key (#231) — repo A's issue #42 and repo B's issue #42 are different
	// issues, and pooling them was the cost half of that bug. The TokenIndex then
	// answers lookups under the tolerant rule (store.RepoMatch).
	canonTokens := map[store.DevIssue]int64{}
	for k, tok := range tokenTotals {
		canonTokens[store.DevIssue{Developer: canon(k.Developer), Repo: k.Repo, IssueID: k.IssueID}] += tok
	}
	tokenIndex := store.BuildJoinIndex(canonTokens)

	// Per-(developer, repo, issue) list-price cost, canonicalized on the SAME alias
	// map as the token/cost joins (#125), for the joint bootstrap CI's per-outcome
	// denominator (#495). Window-bounded; the /scores handler fetches this again for
	// its segment/composition sidecars — a small duplicate scan (#333) kept here so
	// /scores/compare, which never reaches that code, shares one CI derivation.
	issueCosts, err := h.store.DeveloperIssueCostsWindow(ctx, since, until)
	if err != nil {
		return windowScores{}, fmt.Errorf("query issue costs: %w", err)
	}
	canonIssueCost := map[store.DevIssue]int64{}
	for _, ic := range issueCosts {
		canonIssueCost[store.DevIssue{Developer: canon(ic.Developer), Repo: ic.Repo, IssueID: ic.IssueID}] += ic.TotalCostMicro
	}
	issueCostIndex := store.BuildJoinIndex(canonIssueCost)

	// Index outcomes by canonical developer. This is the union member that fixes
	// the vanishing outcome-only developer (#125): an outcome with no cost row
	// still produces a canonical identity here. Each outcome is tagged ZeroToken
	// (#136) when its canonical (developer, issue) recorded fewer than
	// scoring.MinAttributableTokens tokens in the attributable window; the
	// flagged (developer, issue, tokens) tuples feed the data_quality block.
	// Note on counts: a developer's flagged_outcomes counts flagged OUTCOMES
	// (one per outcome, so two PRs reusing a tokenless issue id count twice),
	// while data_quality.zero_token_outcomes lists DISTINCT flagged (developer,
	// issue) pairs (deduped by seenFlag). They measure different things by design
	// — the per-developer count is a ranking signal, the panel is an operator
	// worklist — so the two can differ when an issue id is reused.
	byDev := map[string][]scoring.Outcome{}
	var zeroTokenOutcomes []zeroTokenOutcomeJSON
	seenFlag := map[store.DevIssue]bool{}
	// joinedOutcomes counts outcomes whose canonical (developer, repo, issue) has ANY
	// matching token spend — the numerator of attributed_outcome_share (#351). It uses
	// tokens > 0, a strictly looser bar than the zero-token tripwire's
	// MinAttributableTokens: this measures whether the two join hops MET at all (the
	// silent-identity-zero signal), not whether enough tokens landed to rank.
	var joinedOutcomes int
	for _, o := range outcomes {
		dev := canon(o.Developer)
		key := store.DevIssue{Developer: dev, Repo: o.Repo, IssueID: o.IssueID}
		tokens := tokenIndex.Sum(dev, o.Repo, o.IssueID)
		if tokens > 0 {
			joinedOutcomes++
		}
		zeroToken := tokens < scoring.MinAttributableTokens
		byDev[dev] = append(byDev[dev], scoring.Outcome{
			Developer: dev,
			IssueID:   o.IssueID,
			Repo:      o.Repo,
			Weight:    o.Weight,
			Quality:   o.Quality,
			ZeroToken: zeroToken,
		})
		if zeroToken && !seenFlag[key] {
			seenFlag[key] = true
			zeroTokenOutcomes = append(zeroTokenOutcomes, zeroTokenOutcomeJSON{
				Developer: dev,
				IssueID:   o.IssueID,
				Tokens:    tokens,
			})
		}
	}

	// Rebuild actual_spend keyed by canonical developer, summing collided values
	// (two aliased identities' allocations add).
	canonSpend := map[string]float64{}
	for dev, paid := range actualSpend {
		canonSpend[canon(dev)] += paid
	}

	// Union of canonical identities across all three sources, sorted for a stable
	// response ordering (all inputs are maps). Every developer with cost rows,
	// an allocated spend slice (#39 zero-cost seats), OR outcomes gets exactly
	// one row.
	union := map[string]struct{}{}
	for dev := range costDevs {
		union[dev] = struct{}{}
	}
	for dev := range canonSpend {
		union[dev] = struct{}{}
	}
	for dev := range byDev {
		union[dev] = struct{}{}
	}
	devs := make([]string, 0, len(union))
	for dev := range union {
		devs = append(devs, dev)
	}
	sort.Strings(devs)

	devScores := make([]scoring.DeveloperScore, 0, len(devs))
	for _, dev := range devs {
		devScores = append(devScores, scoring.ComputeDeveloper(
			dev, byDev[dev],
			store.MicroToDollars(totalMicro[dev]),
			store.MicroToDollars(realtimeMicro[dev]),
			canonSpend[dev],
		))
	}

	return windowScores{
		devScores:         devScores,
		byDev:             byDev,
		costDevs:          costDevs,
		outcomes:          outcomes,
		canon:             canon,
		canonTokens:       canonTokens,
		issueCostIndex:    issueCostIndex,
		issueCosts:        issueCosts,
		totalMicro:        totalMicro,
		joinedOutcomes:    joinedOutcomes,
		zeroTokenOutcomes: zeroTokenOutcomes,
		priceVersions:     priceVersions,
	}, nil
}

// dataQualityBlock assembles the zero-token + mixed-price portion of the
// data_quality wire block for one window under the active aggregation mode (#136,
// #293). In an anonymized mode (team #185, division #270) the per-(developer,
// issue) zero-token list is suppressed — it names individuals — and only the
// name-free count is reported; in developer mode the sorted named list is emitted.
// The mixed-version signal is name-free (version integers only) and attaches in
// BOTH modes. Returns nil when there is nothing to report, so a clean window omits
// these fields entirely. It is the single source of the mode-dependent suppression
// rule, shared by /scores (which then augments the returned block with the #351
// coverage shares) and by the per-window blocks of /scores/compare (#277). Sorts a
// COPY of the flagged slice so the caller's slice ordering is never mutated.
func dataQualityBlock(mode scoring.AggregationMode, zeroTokenOutcomes []zeroTokenOutcomeJSON, priceVersions []int) *dataQualityJSON {
	var dq *dataQualityJSON
	if len(zeroTokenOutcomes) > 0 {
		if mode.Anonymized() {
			dq = &dataQualityJSON{ZeroTokenOutcomeCount: len(zeroTokenOutcomes)}
		} else {
			sorted := make([]zeroTokenOutcomeJSON, len(zeroTokenOutcomes))
			copy(sorted, zeroTokenOutcomes)
			sort.Slice(sorted, func(i, j int) bool {
				if sorted[i].Developer != sorted[j].Developer {
					return sorted[i].Developer < sorted[j].Developer
				}
				return sorted[i].IssueID < sorted[j].IssueID
			})
			dq = &dataQualityJSON{ZeroTokenOutcomes: sorted}
		}
	}
	if len(priceVersions) > 1 {
		if dq == nil {
			dq = &dataQualityJSON{}
		}
		dq.MixedPriceVersions = priceVersions
	}
	return dq
}

// withCostHorizon attaches the cost-horizon signal (#512) to a data-quality block,
// allocating one if no other signal produced it.
//
// Unlike every other field here, this one is NOT omit-when-clean: a fully-covered
// window still emits cost_coverage_start and an explicit
// window_predates_cost_capture=false. That is deliberate. The other signals answer
// "is something wrong"; this one answers "how far back can this installation see at
// all", and a consumer must be able to distinguish "we checked, the window is
// covered" from "no signal emitted" — otherwise a client that simply forgot to
// check is indistinguishable from a clean bill of health, which is the exact
// silent-success failure the signal exists to end.
//
// hasHorizon=false (an empty store) emits nothing: with no captured cost there is
// no horizon to compare a window against, and inventing one would assert coverage
// that does not exist.
// safeSinceDay returns the earliest `since` value that will NOT predate the
// horizon — i.e. the remedy every surface prints.
//
// It exists because the two sides of the comparison have different precision.
// The horizon is an INSTANT (the first captured event, e.g. 10:01:01Z) but
// `since` only parses day/month/year layouts, so every value a caller can supply
// lands on midnight. Naively printing the horizon's own day as the remedy hands
// the operator an instruction that cannot work: 2026-06-23T00:00:00Z is still
// before 2026-06-23T10:01:01Z, so following it reproduces the warning verbatim,
// forever, and degrades the message to the self-contradicting "the window starts
// 2026-06-23 but capture began 2026-06-23".
//
// So when the horizon falls mid-day, the first FULLY covered day is the next one.
// We deliberately do not solve this by comparing against the horizon's day-start
// instead: those first hours really are uncovered, and widening the definition of
// "covered" to swallow them would trade a wrong instruction for a wrong answer.
// Losing a partial day of data is the honest cost.
func safeSinceDay(horizon time.Time) string {
	h := horizon.UTC()
	dayStart := h.Truncate(24 * time.Hour)
	if h.After(dayStart) {
		dayStart = dayStart.AddDate(0, 0, 1)
	}
	return dayStart.Format("2006-01-02")
}

func withCostHorizon(dq *dataQualityJSON, since, horizon time.Time, hasHorizon bool, perSource map[string]time.Time) *dataQualityJSON {
	if !hasHorizon {
		return dq
	}
	if dq == nil {
		dq = &dataQualityJSON{}
	}
	dq.CostCoverageStart = horizon.UTC().Format(time.RFC3339)
	predates := since.Before(horizon)
	dq.WindowPredatesCostCapture = &predates
	dq.CostCoverageSafeSince = safeSinceDay(horizon)
	// One source tells the reader nothing the global horizon did not; two or more
	// is where a window can clear the global bound yet still predate a path.
	if len(perSource) > 1 {
		m := make(map[string]string, len(perSource))
		for src, ts := range perSource {
			m[src] = ts.UTC().Format(time.RFC3339)
		}
		dq.SourceCoverageStart = m
	}
	return dq
}

func (h *Handler) handleGetScores(w http.ResponseWriter, r *http.Request) {
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	// Belt-and-braces: normalize to UTC at the call site so the windowed reads
	// all window by instant even if a future caller feeds parseSince a non-UTC
	// bound (#180).
	since = sinceUTC(since)

	// Upper bound of the half-open [since, until) window (#276). Omitted =
	// open-ended (today's behavior); a set until is validated > since and the
	// window is checked against the retention horizon. On any violation the helper
	// has already written the response.
	until, ok := h.parseWindowUpperBound(w, r, since)
	if !ok {
		return
	}

	// ?work_type filter (#187): when present it must be a canonical category, a
	// fail-loud 400 rather than a silent empty result, so a caller that typos a type
	// learns immediately. Empty = no filter (all type segments emitted). Validated
	// here at the trust boundary, before any store work.
	workTypeFilter := r.URL.Query().Get("work_type")
	if workTypeFilter != "" && !store.ValidWorkType(workTypeFilter) {
		writeError(w, http.StatusBadRequest, "invalid work_type: must be one of: "+store.WorkTypeList())
		return
	}

	// Load + canonicalize + join the window into per-developer scores. This is the
	// shared scores path (#277): loadWindow does exactly the store reads, alias
	// canonicalization, cost/outcome/token join and zero-token tripwire that both
	// /scores and /scores/compare need, so the two endpoints can never compute a
	// developer's score — or its k-anon inputs — differently.
	win, err := h.loadWindow(r.Context(), since, until)
	if err != nil {
		h.logger.Error("load window", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// The work-type segmentation (#187), cost-composition sidecar (#234), and
	// #refocus unattributed-bucket split are /scores-only surfaces; the compare
	// endpoint (#277) needs none of them, so they are fetched here rather than in
	// the shared loadWindow.
	//
	// Per-(developer, issue) cost denominates each work-type segment; the
	// composition read is the one non-index-covered read on this path (#333):
	// it groups by (host, model) but filters on ts, so a ts-window seek + per-row
	// heap lookup + GROUP BY temp b-tree runs on every /scores poll. Fine at
	// single-tenant scale (window-bounded, tens–low-hundreds ms).
	// Reuse the per-issue cost rows loadWindow already fetched (#495/#333): one
	// DeveloperIssueCostsWindow scan per request now feeds BOTH the joint CI and
	// these work-type segment sidecars, instead of scanning the same window twice.
	issueCosts := win.issueCosts
	costComposition, err := h.store.CostCompositionWindow(r.Context(), since, until)
	if err != nil {
		h.logger.Error("query cost composition", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	// Unattributed bucket split (#refocus, Option B): per-(developer, bucket) spend
	// over the SAME window, folded below into the org-level labeled split and each
	// developer's exploratory-overhead share. Raw token_events.developer, canonicalized
	// through the same alias map as the score rows.
	unattributedBuckets, err := h.store.UnattributedBucketCostsWindow(r.Context(), since, until)
	if err != nil {
		h.logger.Error("query unattributed buckets", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	// Fold the unattributed buckets (#refocus, Option B) into an org-level split
	// (bucket -> summed micros across developers) and each canonical developer's
	// exploratory-overhead micros (the "unattributed:main" bucket). One pass; the
	// developer names are canonicalized so a per-developer share keys under the same
	// identity the score rows use. exploratoryMicroByDev feeds the per-developer
	// exploratory_cost_share below; it is only READ in developer mode (any anonymized
	// mode — team or division — emits no developer rows), so it cannot leak a sub-k
	// cohort.
	orgBucketMicro := map[string]int64{}
	exploratoryMicroByDev := map[string]int64{}
	for _, b := range unattributedBuckets {
		orgBucketMicro[b.Bucket] += b.CostMicro
		if b.Bucket == store.UnattributedMainBucket {
			exploratoryMicroByDev[win.canon(b.Developer)] += b.CostMicro
		}
	}

	// Unjoined-identity visibility (#125): count identities present on only one
	// side of the cost/outcome join, export as a gauge, and WARN once per
	// identifier per process. Kept on the /scores path (not in loadWindow) so the
	// compare endpoint's two windows never clobber this single-most-recent gauge or
	// re-log the same identifiers twice per comparison. The name slices back the
	// data_quality flag (#351): win.devScores is already in sorted developer order,
	// so both stay sorted for a stable response.
	var unjoinedCost, unjoinedOutcome int
	var costOnlyDevs, outcomeOnlyDevs []string
	for _, s := range win.devScores {
		hasCost := win.costDevs[s.Developer]
		hasOutcome := len(win.byDev[s.Developer]) > 0
		switch {
		case hasCost && !hasOutcome:
			unjoinedCost++
			costOnlyDevs = append(costOnlyDevs, s.Developer)
			h.warnUnjoined(s.Developer, "cost")
		case hasOutcome && !hasCost:
			unjoinedOutcome++
			outcomeOnlyDevs = append(outcomeOnlyDevs, s.Developer)
			h.warnUnjoined(s.Developer, "outcome")
		}
	}
	if h.identityGauge != nil {
		h.identityGauge.Set(float64(unjoinedCost), "cost")
		h.identityGauge.Set(float64(unjoinedOutcome), "outcome")
	}

	// since is UTC-anchored (sinceUTC above, #180), so this echoed date is the
	// UTC calendar day — on a negative-offset host it may read one day earlier
	// than the operator's local "90 days ago", which is intended, not a shift.
	activePrices := store.ActivePriceTableInfo()
	resp := scoresResponse{
		Since: since.Format("2006-01-02"),
		PriceTable: priceTableJSON{
			Version:       activePrices.Version,
			EffectiveDate: activePrices.EffectiveDate,
		},
		Rubric: rubricJSON{Version: scoring.RubricVersion},
	}
	if h.aggregation.Anonymized() {
		// Anonymized mode — team (#185) or division (#270): NEVER emit an individual
		// developer name. Named per-developer rows are replaced by k-anonymized
		// GROUP aggregates (teams, or divisions one level up); any group with fewer
		// than h.kAnonymity contributing developers collapses into an "other" bucket
		// so no sub-k cohort is identifiable, while its cost and outcomes stay in the
		// totals (rolled into "other", never dropped). Developers is emitted as an
		// explicit empty array — never nil, so no consumer can mistake absence for
		// "not yet loaded" and re-request a named view. No CIs are computed: a
		// bootstrap interval is a per-developer signal. The `aggregation`
		// discriminator names which level the Teams rows carry.
		labelOf, err := h.resolveGroupLabels(r.Context(), win.canon)
		if err != nil {
			h.logger.Error("query group labels", "mode", h.aggregation.String(), "err", err)
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		resp.Aggregation = h.aggregation.String()
		resp.Developers = []developerScoreJSON{}
		for _, ts := range scoring.AggregateTeamsKAnon(win.devScores, labelOf, h.kAnonymity) {
			resp.Teams = append(resp.Teams, newTeamScoreJSON(ts))
		}
	} else {
		for _, s := range win.devScores {
			// CI via the shared derivation so /scores and /scores/compare never
			// diverge on a developer's interval (#277); (0,0) for an unranked row.
			ciLow, ciHigh := developerWindowCI(win.byDev, win.issueCostIndex, s)
			row := newDeveloperScoreJSON(s, ciLow, ciHigh)
			// Per-developer exploratory-overhead share (#refocus, Option B): main-branch
			// no-issue micros / this developer's total window micros. Keyed on the same
			// canonical identity as the score row and the folded maps.
			if tm := win.totalMicro[s.Developer]; tm > 0 {
				row.ExploratoryCostShare = float64(exploratoryMicroByDev[s.Developer]) / float64(tm)
			}
			resp.Developers = append(resp.Developers, row)
		}
	}

	// Data-quality block (#136, #293): the zero-token tripwire (name-suppressed to a
	// bare count in an anonymized mode) plus the mixed-version signal. dataQualityBlock
	// is the single source of the mode-dependent suppression rule, shared with the
	// per-window blocks the compare endpoint emits (#277); the #351 coverage shares
	// below augment whatever it returns, so a clean window that carries only those
	// still ships them. NOTE (#512): an empty WINDOW no longer implies an absent
	// data_quality key — the cost horizon below is a property of the INSTALLATION,
	// not of the window, so any store holding cost now emits the block even for a
	// window with nothing in it. Only a store with no captured cost at all omits it.
	resp.DataQuality = dataQualityBlock(h.aggregation, win.zeroTokenOutcomes, win.priceVersions)

	// Cost horizon (#512), attached in BOTH modes. A horizon lookup failure is
	// logged and the signal omitted rather than 500-ing the whole response — the
	// scores are still correct, they are merely unannotated — but it must NOT be
	// silently swallowed, because a permanently-absent signal would read as
	// "window is covered" to any consumer that treats missing as clean.
	if horizon, hasHorizon, err := h.store.CostCoverageStart(r.Context()); err != nil {
		h.logger.Error("query cost coverage start", "err", err)
	} else {
		perSource, err := h.store.SourceCoverageStart(r.Context())
		if err != nil {
			// Degrade to the global horizon rather than dropping the whole signal.
			h.logger.Error("query per-source coverage start", "err", err)
			perSource = nil
		}
		resp.DataQuality = withCostHorizon(resp.DataQuality, since, horizon, hasHorizon, perSource)
	}

	// Honest-coverage block (#351): the two attribution-coverage shares plus the
	// unjoined-developer flag, attached additively to data_quality in BOTH modes.
	// Unlike the exception-only signals above, the shares are ALWAYS present when the
	// window has the relevant data (spend / outcomes) — an adopter must see the true
	// coverage up front, not infer it from the absence of a warning. attachDataQuality
	// creates the block on first use so a window that carries only these fields still
	// ships them. As above, since #512 a truly empty window (no spend, no outcomes,
	// nothing flagged) still ships data_quality whenever the STORE holds any cost,
	// carrying the horizon alone; the key is absent only on a store with no cost.
	attachDataQuality := func() *dataQualityJSON {
		if resp.DataQuality == nil {
			resp.DataQuality = &dataQualityJSON{}
		}
		return resp.DataQuality
	}
	// attributed_cost_share: attributed / total from the SAME window's cost composition
	// (exact integer micro-dollars, #234). Pointer so a genuine 0.0 is emitted; nil when
	// the window has no spend (the composition read already reconciles the split).
	if costComposition.TotalCostMicro > 0 {
		total := float64(costComposition.TotalCostMicro)
		share := float64(costComposition.AttributedCostMicro) / total
		attachDataQuality().AttributedCostShare = &share

		// Labeled unattributed split (#refocus, Option B): the single unattributed
		// mass, broken into its honest reasons. Emitted only when there IS
		// unattributed spend (omit-when-clean, like the other exception signals), so
		// a fully-attributed window ships no bucket list. The main/exploratory bucket
		// also surfaces as the scalar exploratory_cost_share headline. Shares are of
		// TOTAL window cost so they compose with attributed_cost_share; the buckets +
		// attributed sum to 1.0 within rounding. Sorted by descending cost for a
		// stable, operator-useful order (ties broken by label).
		if costComposition.UnattributedCostMicro > 0 {
			buckets := make([]unattributedBucketJSON, 0, len(orgBucketMicro))
			for label, micro := range orgBucketMicro {
				buckets = append(buckets, unattributedBucketJSON{
					Bucket:  label,
					CostUSD: store.MicroToDollars(micro),
					Share:   float64(micro) / total,
				})
			}
			sort.Slice(buckets, func(i, j int) bool {
				if buckets[i].CostUSD != buckets[j].CostUSD {
					return buckets[i].CostUSD > buckets[j].CostUSD
				}
				return buckets[i].Bucket < buckets[j].Bucket
			})
			dq := attachDataQuality()
			dq.UnattributedBuckets = buckets
			exploratoryShare := float64(orgBucketMicro[store.UnattributedMainBucket]) / total
			dq.ExploratoryCostShare = &exploratoryShare
		}
	}
	// attributed_outcome_share: joined / total outcomes. Pointer so 0.0 (no outcome met
	// its cost — the silent-identity-zero) is emitted; nil when the window has none.
	if len(win.outcomes) > 0 {
		share := float64(win.joinedOutcomes) / float64(len(win.outcomes))
		attachDataQuality().AttributedOutcomeShare = &share
	}
	// Unjoined-developer flag: present only when a mismatch exists (omit-when-clean).
	// Counts always carry; names carry only in developer mode — ANY anonymized mode
	// (team #185, division #270) suppresses them through the same Anonymized() k-anon
	// guard as the zero-token identities, so a new level inherits the suppression.
	if len(costOnlyDevs) > 0 || len(outcomeOnlyDevs) > 0 {
		uj := &unjoinedDevelopersJSON{
			CostOnlyCount:    len(costOnlyDevs),
			OutcomeOnlyCount: len(outcomeOnlyDevs),
		}
		if !h.aggregation.Anonymized() {
			uj.CostOnly = costOnlyDevs
			uj.OutcomeOnly = outcomeOnlyDevs
		}
		attachDataQuality().UnjoinedDevelopers = uj
	}

	// Total: rollup across all developers in the response (#25). Server-side
	// computation guarantees the dashboard sees the same numbers
	// scoring.RollupTeam produces, instead of reconstructing them client-
	// side from rounded per-developer percentages.
	if len(win.devScores) > 0 {
		total := newTeamScoreJSON(scoring.RollupTeam("", win.devScores))
		resp.Total = &total
	}

	// Optional team filter — developer-mode only. In ANY anonymized mode (team
	// #185, division #270) this branch is deliberately skipped: it rolls up a
	// SINGLE named team with no k-floor, so honoring ?team= there would re-expose a
	// sub-k cohort's aggregate (a one- or two-developer team's numbers) and bypass
	// the anonymity set. The k-anonymized resp.Teams already gives every group's
	// aggregate under the floor.
	if team := r.URL.Query().Get("team"); team != "" && !h.aggregation.Anonymized() {
		// Resolve every developer's team in one query rather than one per
		// developer inside the loop. #39 surfaced zero-cost active seats, which
		// grew devScores and amplified the old per-developer N+1 (#94 item 3).
		teams, err := h.store.TeamsForDevelopers(r.Context())
		if err != nil {
			h.logger.Error("query teams", "err", err)
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		var teamDevs []scoring.DeveloperScore
		for _, s := range win.devScores {
			if teams[s.Developer] == team {
				teamDevs = append(teamDevs, s)
			}
		}
		if len(teamDevs) > 0 {
			ts := newTeamScoreJSON(scoring.RollupTeam(team, teamDevs))
			resp.Team = &ts
		}
	}

	// Work-type segmentation (#187): the type-scoped view the dashboard renders and
	// the surface for WITHIN-category comparison. Built from the same canonicalized
	// outcomes + token totals as the pooled view above, but partitioned by work_type
	// and denominated by per-(developer, issue) cost. Composes with the anonymized
	// modes (#185, #270): each segment's rows are k-anonymized GROUP aggregates
	// when h.aggregation.Anonymized() (team or division). workTypeFilter (validated
	// above) restricts the output to one segment; empty emits every type present.
	segments, err := h.buildWorkTypeSegments(r.Context(), win.outcomes, win.canon, win.canonTokens, issueCosts, workTypeFilter)
	if err != nil {
		h.logger.Error("build work-type segments", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	resp.WorkTypes = segments

	// Cost-composition sidecar (#234): nil (key omitted) when the window has no
	// token spend, so a clean window ships no `cost_composition` and the dashboard
	// panel stays hidden — same omit-when-empty discipline as data_quality (#136).
	resp.CostComposition = newCostCompositionJSON(costComposition)

	writeJSON(w, http.StatusOK, resp)
}

// buildWorkTypeSegments partitions the window's outcomes by work_type and computes
// a self-contained score per category (#187). Each segment denominates TIER on cost
// at (developer, issue) grain — a token event's cost is charged to the work_type of
// the outcome(s) sharing its (developer, issue) — so a security engineer's security
// TIER divides their security points by the cost of their security issues, never by
// their whole-window cost. Cross-type comparison is a category error and is not
// offered: there is no combined leaderboard, only these per-type groupings.
//
// Composition with the anonymized modes (#185, #270): when
// h.aggregation.Anonymized() (team or division) every segment's rows are
// k-anonymized GROUP aggregates (via AggregateTeamsKAnon with h.kAnonymity) at the
// active level, and Developers is an explicit empty array —
// the same suppression the pooled view applies, enforced INDEPENDENTLY within each
// type group so a developer who is sub-k within a type cannot be re-exposed by the
// segmentation. #133 ranking floors apply per segment in developer mode.
//
// filter, when non-empty (already validated by the caller), restricts the result to
// that single type; a type with no outcomes yields a segment with no rows so a
// filtered request still gets a well-formed (empty) answer.
func (h *Handler) buildWorkTypeSegments(
	ctx context.Context,
	outcomes []store.Outcome,
	canon func(string) string,
	canonTokens map[store.DevIssue]int64,
	issueCosts []store.DevIssueCost,
	filter string,
) ([]workTypeSegmentJSON, error) {
	// Canonicalize per-(developer, repo, issue) cost so it joins to the canonical
	// outcome identity exactly as the pooled path canonicalizes DeveloperCosts (#125).
	// repo stays on the key (#231); the JoinIndex applies the tolerant match so a
	// repo-blind cost row still charges a repo-qualified outcome.
	issueTotalMicro := map[store.DevIssue]int64{}
	issueRealtimeMicro := map[store.DevIssue]int64{}
	for _, c := range issueCosts {
		key := store.DevIssue{Developer: canon(c.Developer), Repo: c.Repo, IssueID: c.IssueID}
		issueTotalMicro[key] += c.TotalCostMicro
		issueRealtimeMicro[key] += c.RealtimeCostMicro
	}
	totalIndex := store.BuildJoinIndex(issueTotalMicro)
	realtimeIndex := store.BuildJoinIndex(issueRealtimeMicro)
	tokenIndex := store.BuildJoinIndex(canonTokens)

	// Partition outcomes: work_type -> canonical developer -> scoring outcomes, plus
	// the DISTINCT issue set per (type, developer) so the same issue's cost is charged
	// once even when a developer has several outcomes on it within a type.
	segByDev := map[string]map[string][]scoring.Outcome{}
	// #231: the distinct-issue set is keyed by (repo, issue), not issue alone —
	// otherwise a developer with issue #42 in two repos charges only one repo's cost.
	segIssues := map[string]map[string]map[store.DevIssue]bool{}
	for _, o := range outcomes {
		wt := o.WorkType
		if wt == "" {
			wt = store.WorkTypeFeature // defensive: AllOutcomesSince COALESCEs, but never trust an empty category
		}
		if filter != "" && wt != filter {
			continue
		}
		dev := canon(o.Developer)
		zeroToken := tokenIndex.Sum(dev, o.Repo, o.IssueID) < scoring.MinAttributableTokens
		if segByDev[wt] == nil {
			segByDev[wt] = map[string][]scoring.Outcome{}
			segIssues[wt] = map[string]map[store.DevIssue]bool{}
		}
		segByDev[wt][dev] = append(segByDev[wt][dev], scoring.Outcome{
			Developer: dev,
			IssueID:   o.IssueID,
			Repo:      o.Repo,
			WorkType:  wt,
			Weight:    o.Weight,
			Quality:   o.Quality,
			ZeroToken: zeroToken,
		})
		if segIssues[wt][dev] == nil {
			segIssues[wt][dev] = map[store.DevIssue]bool{}
		}
		segIssues[wt][dev][store.DevIssue{Developer: dev, Repo: o.Repo, IssueID: o.IssueID}] = true
	}

	// Group-label map only needed in an anonymized mode (team #185 / division
	// #270); fetch once here so each segment applies the SAME k-anonymity floor
	// within its type group, at whichever level is active.
	var labelOf map[string]string
	if h.aggregation.Anonymized() {
		var err error
		labelOf, err = h.resolveGroupLabels(ctx, canon)
		if err != nil {
			return nil, err
		}
	}

	// The set of types to emit. With a filter, emit exactly that one type (even when
	// empty, so a filtered request always gets a well-formed segment). Otherwise emit
	// every type present, sorted for a stable response.
	var types []string
	if filter != "" {
		types = []string{filter}
	} else {
		for wt := range segByDev {
			types = append(types, wt)
		}
		sort.Strings(types)
	}

	segments := make([]workTypeSegmentJSON, 0, len(types))
	for _, wt := range types {
		devMap := segByDev[wt]
		// Compute per-developer scores for this type. Cost is summed over the
		// developer's DISTINCT issues in this type. actualPaid is 0: finance's
		// actual_spend is per (developer, period), not per category (see the segment
		// JSON doc), so SpendLeverage stays at the pooled top level.
		devs := make([]string, 0, len(devMap))
		for dev := range devMap {
			devs = append(devs, dev)
		}
		sort.Strings(devs)
		var devScores []scoring.DeveloperScore
		for _, dev := range devs {
			var totalMicro, realtimeMicro int64
			for k := range segIssues[wt][dev] {
				totalMicro += totalIndex.Sum(dev, k.Repo, k.IssueID)
				realtimeMicro += realtimeIndex.Sum(dev, k.Repo, k.IssueID)
			}
			devScores = append(devScores, scoring.ComputeDeveloper(
				dev, devMap[dev],
				store.MicroToDollars(totalMicro),
				store.MicroToDollars(realtimeMicro),
				0,
			))
		}

		seg := workTypeSegmentJSON{WorkType: wt}
		if h.aggregation.Anonymized() {
			// k-anonymized GROUP aggregates WITHIN this type (#185 × #187, #270):
			// never an individual name, at team or division level. Developers is an
			// explicit empty array so a consumer cannot mistake absence for "not
			// loaded" and re-request named rows. The segment's `aggregation`
			// discriminator names the active level.
			seg.Aggregation = h.aggregation.String()
			seg.Developers = []developerScoreJSON{}
			for _, ts := range scoring.AggregateTeamsKAnon(devScores, labelOf, h.kAnonymity) {
				seg.Teams = append(seg.Teams, newTeamScoreJSON(ts))
			}
		} else {
			for _, s := range devScores {
				var ciLow, ciHigh float64
				if s.Ranked {
					// Segment-scoped joint CI (#495): the segment's own per-issue cost
					// index (totalIndex) and TotalCostUSD, so the fixed remainder is this
					// work-type's non-outcome cost.
					contribs, costs, fixedCost := jointCIInputs(s.Developer, devMap[s.Developer], totalIndex, s.TotalCostUSD)
					rng := rand.New(rand.NewPCG(bootstrapSeed1, bootstrapSeed2))
					ciLow, ciHigh = scoring.BootstrapCI(contribs, costs, fixedCost, scoring.DefaultBootstrapSamples, rng)
				}
				seg.Developers = append(seg.Developers, newDeveloperScoreJSON(s, ciLow, ciHigh))
			}
		}
		if len(devScores) > 0 {
			total := newTeamScoreJSON(scoring.RollupTeam("", devScores))
			seg.Total = &total
		}
		segments = append(segments, seg)
	}
	return segments, nil
}

// --- GET /api/v1/scores/{developer} ---

type developerDetailResponse struct {
	Developer       string  `json:"developer"`
	TIER            float64 `json:"tier"`
	WeightedPoints  float64 `json:"weighted_points"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	ActualPaidUSD   float64 `json:"actual_paid_usd"`
	SpendLeverage   float64 `json:"spend_leverage"`
	CoveragePercent float64 `json:"coverage_pct"`
	// CostPerPoint and its self-relative CI mirror developerScoreJSON (#239) so a
	// single-developer fetch carries the same inverse-unit surface as the list.
	CostPerPoint *float64 `json:"cost_per_point"`
	// Ranking floor + bootstrap CI (#133), mirroring developerScoreJSON so a
	// single-developer fetch carries the same evidence signal as the list.
	SampleN            int     `json:"sample_n"`
	CILow              float64 `json:"ci_low"`
	CIHigh             float64 `json:"ci_high"`
	CostPerPointCILow  float64 `json:"cost_per_point_ci_low"`
	CostPerPointCIHigh float64 `json:"cost_per_point_ci_high"`
	Ranked             bool    `json:"ranked"`
	// FlaggedOutcomes mirrors developerScoreJSON (#136): the count of this
	// developer's zero-token-flagged outcomes, any non-zero value being why
	// Ranked is false.
	FlaggedOutcomes int               `json:"flagged_outcomes"`
	Issues          []issueDetailJSON `json:"issues"`
}

type issueDetailJSON struct {
	IssueID  string  `json:"issue_id"`
	Weight   float64 `json:"weight"`
	Quality  float64 `json:"quality"`
	PRNumber int     `json:"pr_number,omitempty"`
	// ZeroToken marks an outcome whose (developer, issue) recorded fewer than
	// scoring.MinAttributableTokens tokens in its attributable window (#136).
	// Always present so a consumer can distinguish "not flagged" from "field
	// absent on an old server".
	ZeroToken bool `json:"zero_token"`
}

func (h *Handler) handleGetDeveloperScore(w http.ResponseWriter, r *http.Request) {
	// Any anonymized mode (team #185, division #270): the per-developer detail
	// endpoint names one individual by construction, so it is BLANKET-rejected
	// here — the same 404 for every {developer} path value, returned BEFORE any
	// store lookup or use of
	// the requested name. Blanket-and-early matters: a 404 only for non-existent
	// developers would be an existence oracle, and echoing the requested name back
	// would itself be a leak. This carve-out has no bypass: the dashboard omits the
	// per-developer drill-down link in team mode, and there is no other route to
	// an individual's score.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusNotFound, "per-developer score detail is disabled in "+h.aggregation.String()+"-aggregation mode (#185, #270)")
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	// Normalize to UTC before any ts >= ? store query (#180); see sinceUTC.
	since = sinceUTC(since)
	// Half-open [since, until) upper bound (#276), same contract as /scores.
	until, ok := h.parseWindowUpperBound(w, r, since)
	if !ok {
		return
	}
	// Canonicalize the path value through the alias map (#125): a request for the
	// raw GitHub login resolves to the same canonical row /scores builds.
	aliases, err := h.store.DeveloperAliases(r.Context())
	if err != nil {
		h.logger.Error("query developer_alias", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	canon := func(id string) string {
		if c, ok := aliases[id]; ok {
			return c
		}
		return id
	}
	target := canon(r.PathValue("developer"))

	// Outcomes: the SQL-side DeveloperOutcomes(developer=?) filter cannot see
	// aliases, so pull all outcomes in the window and filter by canonical id.
	outcomes, err := h.store.AllOutcomesWindow(r.Context(), since, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	costs, err := h.store.DeveloperCostsWindow(r.Context(), since, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// Sum every cost row whose canonical id is the target (aliased identities
	// merge). Integer micro-dollars, exact (#69).
	var totalMicro, realtimeMicro int64
	for _, c := range costs {
		if canon(c.Developer) == target {
			totalMicro += c.TotalCostMicro
			realtimeMicro += c.RealtimeCostMicro
		}
	}

	// Merge actual_spend by canonical id: sum every raw developer's allocation
	// that resolves to the target.
	spendAll, err := h.store.ActualSpendAllWindow(r.Context(), since, until)
	if err != nil {
		// target is canon(r.PathValue("developer")): a URL path segment is percent-
		// decoded, so it is client-controlled and CRLF-injectable, and is never
		// charset-validated here. Sanitize via the shared logsafe barrier (#321).
		h.logger.Error("query actual_spend for developer", "developer", logSafeStr(target), "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	var actualPaid float64
	for dev, paid := range spendAll {
		if canon(dev) == target {
			actualPaid += paid
		}
	}

	// Narrow to the target's outcomes before the token-totals query so it only
	// windows this developer's issues (not every issue in the period).
	var targetOutcomes []store.Outcome
	for _, o := range outcomes {
		if canon(o.Developer) == target {
			targetOutcomes = append(targetOutcomes, o)
		}
	}

	// Windowed token totals for the tripwire (#136), re-keyed by canonical
	// identity so aliased tokens (#125) count toward the target.
	tokenTotals, err := h.store.OutcomeTokenTotals(r.Context(), targetOutcomes)
	if err != nil {
		// target is the same client-controlled path value; sanitize via logsafe (#321).
		h.logger.Error("query token totals for developer", "developer", logSafeStr(target), "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	canonTokens := map[store.DevIssue]int64{}
	for k, tok := range tokenTotals {
		canonTokens[store.DevIssue{Developer: canon(k.Developer), Repo: k.Repo, IssueID: k.IssueID}] += tok
	}
	tokenIndex := store.BuildJoinIndex(canonTokens)

	// Per-(developer, repo, issue) cost for the target's joint bootstrap CI (#495),
	// same window and alias canonicalization as the tripwire above.
	detailIssueCosts, err := h.store.DeveloperIssueCostsWindow(r.Context(), since, until)
	if err != nil {
		h.logger.Error("query issue costs for developer", "developer", logSafeStr(target), "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	canonDetailCost := map[store.DevIssue]int64{}
	for _, ic := range detailIssueCosts {
		canonDetailCost[store.DevIssue{Developer: canon(ic.Developer), Repo: ic.Repo, IssueID: ic.IssueID}] += ic.TotalCostMicro
	}
	detailCostIndex := store.BuildJoinIndex(canonDetailCost)

	var sOutcomes []scoring.Outcome
	var issues []issueDetailJSON
	for _, o := range targetOutcomes {
		zeroToken := tokenIndex.Sum(target, o.Repo, o.IssueID) < scoring.MinAttributableTokens
		sOutcomes = append(sOutcomes, scoring.Outcome{
			Developer: target, IssueID: o.IssueID, Repo: o.Repo,
			Weight: o.Weight, Quality: o.Quality,
			ZeroToken: zeroToken,
		})
		issues = append(issues, issueDetailJSON{
			IssueID:   o.IssueID,
			Weight:    o.Weight,
			Quality:   o.Quality,
			PRNumber:  o.PRNumber,
			ZeroToken: zeroToken,
		})
	}

	s := scoring.ComputeDeveloper(target, sOutcomes,
		store.MicroToDollars(totalMicro), store.MicroToDollars(realtimeMicro), actualPaid)
	// Bootstrap CI for a ranked developer only (#133); unranked → 0,0. Fixed seed
	// keeps repeated fetches reproducible (see bootstrapSeed1/2).
	var ciLow, ciHigh float64
	if s.Ranked {
		ciContribs, ciCosts, ciFixed := jointCIInputs(target, sOutcomes, detailCostIndex, s.TotalCostUSD)
		rng := rand.New(rand.NewPCG(bootstrapSeed1, bootstrapSeed2))
		ciLow, ciHigh = scoring.BootstrapCI(ciContribs, ciCosts, ciFixed, scoring.DefaultBootstrapSamples, rng)
	}
	cppLow, cppHigh := scoring.CostPerPointCI(ciLow, ciHigh)
	writeJSON(w, http.StatusOK, developerDetailResponse{
		Developer:          s.Developer,
		TIER:               s.TIER,
		WeightedPoints:     s.WeightedPoints,
		TotalCostUSD:       s.TotalCostUSD,
		ActualPaidUSD:      s.ActualPaidUSD,
		SpendLeverage:      s.SpendLeverage,
		CoveragePercent:    s.CoveragePercent,
		CostPerPoint:       costPerPointOrNull(s.WeightedPoints, s.CostPerPoint),
		SampleN:            s.SampleN,
		CILow:              ciLow,
		CIHigh:             ciHigh,
		CostPerPointCILow:  cppLow,
		CostPerPointCIHigh: cppHigh,
		Ranked:             s.Ranked,
		FlaggedOutcomes:    s.FlaggedOutcomes,
		Issues:             issues,
	})
}

// --- /api/v1/developer_alias (#125) ---

// maxAliasBody caps the alias request body at 1 MiB — matches the /costs and
// /actual_spend caps; the legitimate payload is a few dozen bytes.
const maxAliasBody = 1 << 20

// developerAliasRequest is the POST body: map a raw identifier (alias) to the
// canonical developer identity used for scoring.
type developerAliasRequest struct {
	Alias     string `json:"alias"`
	Canonical string `json:"canonical"`
}

// handlePostDeveloperAlias upserts an alias->canonical mapping. Validation
// mirrors handlePostCosts (MaxBytesReader, DisallowUnknownFields, dec.More
// rejection, required + length-capped fields). Chain/self-map violations from
// the store surface as 400 with the store's message (the single-hop invariant
// is enforced there, atomically). Success is 201.
func (h *Handler) handlePostDeveloperAlias(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxAliasBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req developerAliasRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}
	if req.Alias == "" || req.Canonical == "" {
		writeError(w, http.StatusBadRequest, "alias and canonical are required")
		return
	}
	if len(req.Alias) > maxIdentifierLen || len(req.Canonical) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "alias and canonical must be <= 256 chars")
		return
	}
	if err := h.store.UpsertDeveloperAlias(r.Context(), req.Alias, req.Canonical); err != nil {
		// The store's validation errors (self-map, chain) are caller-facing 400s
		// with a descriptive message; only an unexpected failure is a 500. The
		// store returns plain errors.New for the validation cases, so match on the
		// "developer_alias:" prefix it stamps on every such message.
		if strings.HasPrefix(err.Error(), "developer_alias:") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.logger.Error("upsert developer_alias", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleDeleteDeveloperAlias removes the mapping named in the path. 204 when a
// row was deleted, 404 when the alias was not mapped.
func (h *Handler) handleDeleteDeveloperAlias(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if alias == "" {
		writeError(w, http.StatusBadRequest, "alias is required")
		return
	}
	found, err := h.store.DeleteDeveloperAlias(r.Context(), alias)
	if err != nil {
		h.logger.Error("delete developer_alias", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "alias not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// developerAliasesResponse is the GET list shape.
type developerAliasesResponse struct {
	Aliases map[string]string `json:"aliases"`
}

// handleGetDeveloperAliases returns the full alias->canonical map.
func (h *Handler) handleGetDeveloperAliases(w http.ResponseWriter, r *http.Request) {
	aliases, err := h.store.DeveloperAliases(r.Context())
	if err != nil {
		h.logger.Error("query developer_alias", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusOK, developerAliasesResponse{Aliases: aliases})
}

// --- GDPR Art. 15 (access) / Art. 17 (erasure) — #184 ---

// eraseDeveloperResponse is the DELETE /developer/{id} body: per-table
// deleted-row counts plus their sum, so an operator has an auditable receipt of
// exactly what the erasure removed.
type eraseDeveloperResponse struct {
	Deleted      map[string]int64 `json:"deleted"`
	TotalDeleted int64            `json:"total_deleted"`
}

// handleEraseDeveloper serves DELETE /api/v1/developer/{id} (GDPR Art. 17). It is
// write-scoped (requireAuth in Register): the read-only viewer token (#190) is
// rejected 403 — erasure is destructive admin power, not a dashboard read. The
// store resolves {id} through the alias map (single-hop) and cascades the delete
// across every developer-PII table + developer_alias in one transaction,
// returning per-table counts. An all-zero result (never-seen id or already
// erased) maps to 404, which makes a repeated erasure idempotent.
func (h *Handler) handleEraseDeveloper(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation mode (#185) carve-out: this is admin compliance tooling
	// (write-gated), NOT a reporting surface, so it deliberately REMAINS available
	// in team mode — unlike GET /scores/{developer}, which blanket-404s there. An
	// operator must be able to fulfil a GDPR erasure regardless of the dashboard's
	// reporting mode; suppressing it here would make DSAR compliance impossible in
	// team mode. Do NOT add an h.aggregation guard.
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "developer id is required")
		return
	}
	if len(id) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "developer id must be <= 256 chars")
		return
	}
	counts, err := h.store.EraseDeveloper(r.Context(), id)
	if err != nil {
		// Log server-side only; the client error never echoes the requested id
		// (no PII in responses).
		h.logger.Error("erase developer", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		writeError(w, http.StatusNotFound, "developer not found")
		return
	}
	writeJSON(w, http.StatusOK, eraseDeveloperResponse{Deleted: counts, TotalDeleted: total})
}

// handleExportDeveloper serves GET /api/v1/developer/{id}/export (GDPR Art. 15).
// It is write-scoped (requireAuth in Register): it discloses a full individual
// PII record, so the read-only viewer token (#190) is rejected 403 — the same
// authorization as the erasure endpoint, deliberately stricter than the score
// GETs. Resolves {id} through the alias map and returns every stored row for the
// resolved identifier set; an empty record maps to 404.
func (h *Handler) handleExportDeveloper(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation mode (#185) carve-out: same rationale as handleEraseDeveloper.
	// This endpoint NAMES an individual by construction (that is the point of a
	// DSAR), which is exactly why /scores/{developer} blanket-404s in team mode —
	// but this is the authorized-operator path to fulfil an Art. 15 access request
	// and MUST stay available in team mode. Do NOT add an h.aggregation guard.
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "developer id is required")
		return
	}
	if len(id) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "developer id must be <= 256 chars")
		return
	}
	exp, err := h.store.ExportDeveloper(r.Context(), id)
	if err != nil {
		h.logger.Error("export developer", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	if exp.RowCount() == 0 {
		writeError(w, http.StatusNotFound, "developer not found")
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

// --- GET /api/v1/health ---

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- GET /api/v1/healthz ---

// healthzResponse is the body shape /healthz returns (#48).
//
//   - Subsystems is the extensible map keyed by subsystem name
//     (`subsystems.watcher`, and future `subsystems.anthropic_admin`, ...).
//     A new collector adds a key here without any consumer needing new
//     per-subsystem code — the growth the hard-coded shape forced (#48).
//   - Healthy is the aggregate: true iff every registered subsystem is
//     healthy. It mirrors the HTTP status (200 vs 503) so a consumer that
//     ignores the code can still branch on the body.
//   - Watcher is RETAINED for backward compatibility: pre-#48 consumers read
//     the watcher block at the top level. It duplicates
//     Subsystems["watcher"].detail. Deprecated; prefer the subsystems map.
type healthzResponse struct {
	Watcher    health.WatcherSnapshot              `json:"watcher"`
	Subsystems map[string]health.SubsystemSnapshot `json:"subsystems"`
	Healthy    bool                                `json:"healthy"`
}

// handleHealthz returns the runtime state of supervised subsystems (#28).
//
// This is a READINESS probe specifically (#49): it 503s while the watcher is
// restarting so a k8s readiness check drops the pod from Service endpoints
// until it recovers. Do NOT wire a k8s *liveness* probe here — a transient
// backoff would restart the pod every cycle and defeat the supervisor. Use
// /api/v1/livez for liveness.
//
// Status code policy:
//   - 200 when every subsystem reports healthy (running or not_configured).
//   - 503 when at least one subsystem is restarting or stopped with error.
//
// The body is the same JSON in either case, so a dashboard that ignores
// status code still renders the state. Prometheus exporters can read either
// the code or the JSON.
func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	// A single registry snapshot drives the body AND the status code, so the
	// legacy `watcher` block, subsystems.watcher.detail, and the 200/503 code
	// are all derived from ONE consistent sample — no torn read across a
	// concurrent supervisor transition, and each subsystem is sampled once.
	subsystems := h.subsystems.Snapshot()
	// The legacy top-level `watcher` block is the same WatcherSnapshot carried
	// in subsystems["watcher"].detail. The comma-ok guards a future rename of
	// the key or a change to Detail's type: a miss degrades to a zero-value
	// block rather than panicking.
	watcherSnap, _ := subsystems["watcher"].Detail.(health.WatcherSnapshot)
	resp := healthzResponse{
		Watcher:    watcherSnap,
		Subsystems: subsystems,
		Healthy:    health.AllHealthy(subsystems),
	}
	code := http.StatusOK
	if !resp.Healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

// --- GET /api/v1/livez ---

// livezResponse is the body shape /livez returns — the minimal facts a
// liveness probe wants: that the process is up, for how long, and which build.
type livezResponse struct {
	Status  string `json:"status"`
	UptimeS int64  `json:"uptime_s"`
	Version string `json:"version"`
}

// handleLivez is the k8s LIVENESS probe (#49). It always returns 200: reaching
// this handler already proves the HTTP listener can answer, which is the only
// thing a liveness probe should test. Unlike /healthz it never 503s on watcher
// backoff — a liveness failure means "kill the pod", and a watcher that is
// restarting is exactly the case the supervisor exists to handle in-process.
func (h *Handler) handleLivez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, livezResponse{
		Status:  "alive",
		UptimeS: int64(time.Since(h.startedAt).Seconds()),
		Version: h.version,
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		// Bind the default 90-day lower bound in UTC. time.Now() carries the
		// host's local zone; modernc.org/sqlite renders a bound time.Time as an
		// offset-bearing DATETIME string and compares it lexically against the
		// UTC-stored ts column, so a non-UTC bound mis-windows ts >= ? on
		// non-UTC hosts (#180). time.Parse below already yields UTC for these
		// zone-less layouts; only this default branch could leak a local zone.
		return time.Now().AddDate(0, 0, -90).UTC(), nil
	}
	return parseWindowDate(s)
}

// parseWindowDate parses a window-bound query value using the accepted date
// layouts, each yielding a UTC instant at the START of the named period (day,
// month, or year). It is the shared grammar for both ends of a score window so
// `since` and `until` parse identically (#276) — the only difference is what an
// EMPTY value means, which each caller decides (since defaults to 90 days ago;
// until defaults to open-ended). The zoneless layouts already yield UTC.
func parseWindowDate(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, YYYY-MM, or YYYY, got %q", s)
}

// parseUntil parses the upper bound of a half-open score window [since, until)
// (#276). An EMPTY value returns the zero Time, which the store reads treat as
// "no upper bound" — so an omitted until is exactly today's open-ended behavior.
// A non-empty value uses the same grammar as since and denotes the EXCLUSIVE
// instant at the start of the named period: until=2026-04-01 means "up to, but
// not including, 2026-04-01T00:00:00Z", i.e. all of March. The handler validates
// until > since and normalizes to UTC before binding.
func parseUntil(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseWindowDate(s)
}

// parseWindowUpperBound reads the ?until= query param (with ?before= as an
// accepted synonym — it reads naturally for the BEFORE leg of a before/after
// comparison), parses it, validates until > since, and applies the retention
// fail-loud check (#276). On any violation it writes the HTTP error itself and
// returns ok=false, so the caller returns immediately without special-casing
// each failure. A zero `until` with ok=true means the window is open-ended (the
// param was omitted) — today's behavior, unchanged. `since` must already be
// UTC-normalized (both callers apply sinceUTC first).
func (h *Handler) parseWindowUpperBound(w http.ResponseWriter, r *http.Request, since time.Time) (until time.Time, ok bool) {
	untilStr := r.URL.Query().Get("until")
	if untilStr == "" {
		untilStr = r.URL.Query().Get("before")
	}
	until, err := parseUntil(untilStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid until: "+err.Error())
		return time.Time{}, false
	}
	if !until.IsZero() {
		until = until.UTC()
		if !until.After(since) {
			writeError(w, http.StatusBadRequest,
				"until must be after since (half-open [since, until) window)")
			return time.Time{}, false
		}
	}
	// Fail loud if the window's lower bound reaches into a pruned retention zone
	// (#252). No-op until retention is configured; see checkWindowRetention.
	if err := h.checkWindowRetention(since); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return time.Time{}, false
	}
	return until, true
}

// sinceUTC normalizes a since-window lower bound to UTC before it is bound into
// a ts >= ? comparison. modernc.org/sqlite compares DATETIME values as
// offset-bearing strings, so a bound carrying a non-UTC offset compares
// lexically (not temporally) against the UTC-stored ts column, silently
// mis-windowing rows near the boundary on non-UTC hosts (#180). Applying this
// at every store call site fixes the window regardless of the zone the caller's
// time.Time happens to carry.
func sinceUTC(t time.Time) time.Time { return t.UTC() }
