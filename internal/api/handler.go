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
	"net/url"
	"slices"
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
	// scope (#590): a scoped read reports THAT repository's horizon. Left unscoped it
	// would assert coverage for a scoped window on another repository's evidence.
	CostCoverageStart(ctx context.Context, scope store.RepoScope) (time.Time, bool, error)
	// SourceCoverageStart is the same horizon at per-source grain: capture paths
	// enabled on different dates have different horizons, so a window clearing
	// the global minimum can still predate one source entirely.
	SourceCoverageStart(ctx context.Context, scope store.RepoScope) (map[string]time.Time, error)
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
	//
	// It is a REQUEST-PATH writer: it takes the bounded write lock, so it may
	// also return store.ErrWriteLockUnavailable, which handlePostCosts answers with
	// 503 + Retry-After ahead of every other classification (#610).
	InsertManualCostEvent(ctx context.Context, e store.TokenEvent) error
	// CorrectManualCostEvent implements POST /api/v1/costs's sanctioned
	// override path (#346, ruling C — the follow-up to #295's ruling A above):
	// given override=true plus a required actor and reason, it may rewrite an
	// EXISTING keyed row's cost_micro instead of 409ing, and appends an
	// append-only cost_correction_audit row (old → new, actor, reason) when it
	// does. See store.DB.CorrectManualCostEvent for the full per-case contract.
	CorrectManualCostEvent(ctx context.Context, e store.TokenEvent, actor, reason string) (store.CostCorrection, error)
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
	// scope narrows the read to ONE repository (#590), strictly: the 'unqualified'
	// sentinel is excluded, never folded in. store.FleetWide is the unscoped zero
	// value and reproduces the pre-#590 read exactly.
	DeveloperCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperCost, error)
	// DeveloperIssueCostsWindow returns cost totals at (developer, issue) grain
	// (#187) over [since, until) (#276), the finer grain the work-type segmentation
	// attributes to a category: a token event's cost is charged to the work_type of
	// the outcome sharing its (developer, issue). Same window/realtime-split as
	// DeveloperCostsWindow, one level finer.
	DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DevIssueCost, error)
	// CostCompositionWindow returns the cost-composition sidecar over [since, until)
	// (#234): cost by normalized model, per-class token composition, attributed vs
	// unattributed spend, and the cache-read/premium-model levers. A whole-window,
	// name-free aggregate; a zero `until` is open-ended.
	CostCompositionWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.CostComposition, error)
	// UnattributedBucketCostsWindow returns per-(developer, bucket) unattributed
	// spend over [since, until) (#refocus, Option B): the honest split of the single
	// unattributed mass the composition sidecar reports as one number, into the
	// labeled buckets (main/exploratory, detached-head, branch-without-issue, plus
	// the base sentinel for host-blind producers). A zero `until` is open-ended; the
	// handler folds it to an org split and per-developer exploratory shares, and
	// suppresses names in team-aggregation mode (#185).
	UnattributedBucketCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.UnattributedBucketCost, error)
	// DistinctPriceVersionsWindow returns the ascending distinct price_table
	// versions that priced token_events in [since, until) (#293). Feeds the
	// mixed-version data_quality WARN on /scores: cost_micro is immutable per row
	// (#233), so a window can span multiple versions while the response stamps a
	// single active price_table.version — this read surfaces the mix. A zero `until`
	// is open-ended; an empty window returns nil.
	DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]int, error)
	// AllOutcomesWindow returns every outcome in [since, until) (#276); a zero
	// `until` is open-ended.
	AllOutcomesWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.Outcome, error)
	// OutcomeTokenTotals returns per-(developer, issue) token totals over each
	// outcome's attributable window, keyed by the raw token_events developer, for
	// the zero-token tripwire (#136). The caller canonicalizes the key (#125)
	// before comparing to scoring.MinAttributableTokens.
	// Under a non-FleetWide scope the tripwire join goes strict too (#590) — see
	// store.OutcomeTokenTotals for why a scoped read must not let a repo-blind row
	// suppress a zero-token flag.
	OutcomeTokenTotals(ctx context.Context, outcomes []store.Outcome, scope store.RepoScope) (map[store.DevIssue]int64, error)
	// ActualSpendAllWindow returns per-developer actual paid spend over the
	// half-open period window [since, until) (#276), at monthly grain; a zero
	// `until` is open-ended. Feeds per-developer SpendLeverage without an N+1.
	//
	// 🔴 TAKES NO SCOPE, AND CANNOT (#590). actual_spend has no `repo` column and
	// never could meaningfully have one: it records what an organization actually
	// PAID a vendor over a period, which is not divisible by repository without
	// inventing an allocation. So under a repo scope this read is left unscoped and
	// its derived figure is SUPPRESSED rather than shown — dividing org-wide actual
	// spend by one repository's list-price cost would manufacture a leverage ratio
	// inflated by roughly the fleet-to-repo ratio. See scoresResponse.DataQuality's
	// spend-leverage suppression note.
	ActualSpendAllWindow(ctx context.Context, since, until time.Time) (map[string]float64, error)
	// UnqualifiedExclusionWindow reports what a strict repo scope excluded from the
	// window as repo-blind (#590) — the disclosure half of ruling C. Takes no scope
	// by design: the sentinel rows are the same set whichever repository was asked
	// for, because the sentinel means no repository could be determined at all.
	UnqualifiedExclusionWindow(ctx context.Context, since, until time.Time) (store.UnqualifiedExclusion, error)
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
	// Override, OverrideActor, and OverrideReason implement #346's sanctioned
	// "ruling C" exception to #295's default 409: a keyed re-post whose
	// cost_usd DIVERGES from the stored row is REJECTED unless the caller
	// explicitly sets override=true AND supplies both an actor and a reason.
	// This is deliberately NOT a plain last-writer-wins upsert switch — it is
	// the ONLY way a legitimate finance correction can land, and every use
	// writes an append-only audit row (see store.DB.CorrectManualCostEvent).
	// A bare key collision (override omitted or false) still 409s exactly as
	// before; there is no way to silently overwrite an audited cost.
	Override       bool   `json:"override,omitempty"`
	OverrideActor  string `json:"override_actor,omitempty"`
	OverrideReason string `json:"override_reason,omitempty"`
}

// costCorrectionResponse is the 200 body on a #346 override that ACTUALLY
// corrected a row (CostCorrection.Corrected == true). Every other outcome on
// POST /api/v1/costs — 201, 409, 400 — carries no body, matching this
// endpoint's existing convention; this is the one case where the client has
// no other way to learn what changed without a second read.
type costCorrectionResponse struct {
	Corrected  bool    `json:"corrected"`
	OldCostUSD float64 `json:"old_cost_usd"`
	NewCostUSD float64 `json:"new_cost_usd"`
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
	// The unattributed sentinel is server-assigned and may not be forged, on EITHER
	// column it names: issue_id (#466) and developer (#619). The developer half is
	// checked first because it is the one that moves a dollar out of the forger's own
	// denominator rather than between buckets inside it.
	if err := validateDeveloper(req.Developer); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateIssueID(req.IssueID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	// #346 (ruling C): validate the override signal BEFORE it can reach
	// CorrectManualCostEvent, so a malformed override request 400s instead of
	// falling through to store-layer errors that were never meant to be an
	// HTTP contract. Three fail-loud rules, all "never silent":
	//   - override=true requires a non-empty idempotency_key: there is nothing
	//     to override without one (an unkeyed post can never conflict — #295 —
	//     so "override" is meaningless on it).
	//   - override=true requires BOTH override_actor and override_reason: an
	//     unattributed, unexplained override is exactly the silent overwrite
	//     ruling C exists to prevent.
	//   - override_actor / override_reason set WITHOUT override=true is
	//     rejected rather than silently ignored — a client that filled in
	//     both fields almost certainly meant to authorize an override and
	//     forgot the flag; silently discarding them would look like the
	//     override worked when the request instead just 409s (or is a plain
	//     unkeyed insert).
	if req.Override {
		if req.IdempotencyKey == "" {
			writeError(w, http.StatusBadRequest, "override requires a non-empty idempotency_key (nothing to override without one)")
			return
		}
		if req.OverrideActor == "" || req.OverrideReason == "" {
			writeError(w, http.StatusBadRequest, "override requires both override_actor and override_reason — an override must be attributed and explained, never silent")
			return
		}
		if len(req.OverrideActor) > maxIdentifierLen {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("override_actor must be <= %d chars", maxIdentifierLen))
			return
		}
		if len(req.OverrideReason) > maxOverrideReasonLen {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("override_reason must be <= %d chars", maxOverrideReasonLen))
			return
		}
	} else if req.OverrideActor != "" || req.OverrideReason != "" {
		writeError(w, http.StatusBadRequest, "override_actor/override_reason were set but override is not true — set override=true to authorize a cost correction, or omit both fields")
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
	//
	// 🔴 THIS IS ALSO THE #346 OVERRIDE'S BLAST-RADIUS CONTROL, and it is load-
	// bearing precisely because it is not obvious. Forcing source="api" here,
	// combined with CorrectManualCostEvent comparing source in its identity
	// check, means the sanctioned cost-correction override can only ever reach
	// a row whose STORED source is "api" -- the manual-import lane. (Deliberately
	// NOT "a row /costs itself created": store.InsertTokenEvent is exported and
	// writes source verbatim, so provenance is not what this proves. Stored
	// source is.) Automatically captured spend -- every non-api source: jsonl,
	// proxy, codex-rollout, copilot-api, the org pollers -- is STRUCTURALLY
	// unreachable from this endpoint, not merely unlikely to be hit. An override request naming a collector-captured row's
	// idempotency_key gets a 409 identity mismatch, because the stored source
	// can never equal the "api" this handler forces.
	//
	// Widening this switch to accept another source would silently hand the
	// override write access to captured spend. Do not widen it without
	// deciding that question explicitly. Pinned by
	// TestPostCosts_Override_CannotReachCapturedSpend.
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
	// audited override (ruling C) now follows immediately below.
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

	// #346 (ruling C): override=true routes through CorrectManualCostEvent
	// instead of the plain InsertManualCostEvent path. CorrectManualCostEvent
	// itself decides whether this is an ACTUAL correction (an existing row
	// whose identity matches and whose cost diverges — audited, 200) or
	// nothing to correct at all (no existing row, or a matching re-post —
	// both land through CorrectManualCostEvent's OWN insert path, in the same
	// transaction as its lookup, so they behave identically to the
	// override=false path and return 201/idempotent-201 exactly as before).
	// override=false keeps the original code path untouched — #295's 409
	// default is not routed through the override machinery at all.
	if req.Override {
		result, err := h.store.CorrectManualCostEvent(r.Context(), ev, req.OverrideActor, req.OverrideReason)
		if err != nil {
			// 🔴 CONTENTION IS CLASSIFIED FIRST, AND THE ORDER IS LOAD-BEARING.
			// CorrectManualCostEvent takes the write lock via
			// beginImmediateBounded, so it is a request-path writer that can
			// return store.ErrWriteLockUnavailable — a TRANSIENT failure whose
			// honest answer is 503 + Retry-After, not the 500 "store error"
			// sink below (which reads as corruption) and not the 409 above it.
			// Every other branch in this chain must stay downstream of this
			// one: the same reordering defect was MEASURED on the alias site
			// (see TestContentionOutranksTheValidationPrefix), where a
			// classification placed ahead of the sentinel silently turned a
			// retryable 503 into a permanent status no client would retry.
			//
			// ⚠️ Only the sentinel gets this treatment. beginImmediateBounded
			// wraps ErrWriteLockUnavailable ONLY around a genuine lock outcome
			// (isPromoteContention gates it on the SQLite result code), so a
			// read-only or full database — code 8 — falls through to the 500
			// rather than being advertised as "retry shortly", which would be
			// a permanent condition sold as transient.
			if errors.Is(err, store.ErrWriteLockUnavailable) {
				h.logger.Warn("correct manual cost: write lock unavailable", "err", logSafeErr(err))
				writeStoreContention(w)
				return
			}
			if errors.Is(err, store.ErrCostCorrectionIdentityMismatch) {
				// The idempotency_key exists but belongs to a different
				// (developer, issue_id, model, source, fidelity) than this
				// request claims — same status family as the plain
				// ErrCostConflict 409 below (a key that does not mean what the
				// client thinks it means), and deliberately does NOT echo back
				// which identity the key actually belongs to: a client that
				// merely guessed or reused a key must not learn whose row it
				// is. That non-disclosure is defense in depth against a BLIND
				// collision, not a secrecy guarantee — the read-scoped
				// GET /api/v1/events export already publishes every
				// idempotency_key next to its full identity tuple. See
				// store.ErrCostCorrectionIdentityMismatch for the endpoint's
				// stated trust model.
				writeError(w, http.StatusConflict,
					"idempotency_key exists but does not match this request's developer/issue_id/model/source/fidelity; refusing to correct a row this request does not identify")
				return
			}
			// Sanitized through the shared barrier (logSafeErr / logSafeStr):
			// a store error can wrap caller-supplied text. BOTH store-error
			// sinks on this endpoint route through it -- see the sibling on
			// the non-override path below. (Package-wide the barrier is the
			// convention, not yet a universal: bare "err", err sinks remain
			// on other routes. Do not restate this as "every sink in this
			// package" -- that was written here once and was false.)
			h.logger.Error("correct manual cost", "err", logSafeErr(err))
			writeError(w, http.StatusInternalServerError, "store error")
			return
		}
		if result.Corrected {
			// A money-mutating operation is worth an off-box record beyond
			// the in-DB audit row: the ledger lives in SQLite, this log line
			// is what ships to wherever operator logs go.
			//
			// override_actor is client-controlled free text, so it routes
			// through logSafeStr like every other client-controlled string
			// sink in this package — the convention is that no such value
			// reaches a structured log unsanitized, and an exception here
			// would be the one nobody notices. It is also a SELF-ASSERTED
			// claim: it records who the caller says they are, not a verified
			// principal (see cost_correction_audit's schema comment).
			h.logger.Info("sanctioned cost correction applied",
				"token_event_id", result.TokenEventID,
				"old_cost_micro", result.OldCostMicro,
				"new_cost_micro", result.NewCostMicro,
				"actor", logSafeStr(req.OverrideActor),
			)
			// An existing audited cost was actually rewritten, with an
			// append-only cost_correction_audit row recording the reason —
			// 200, not 201: this modified a resource, it did not create one.
			// The body echoes the old/new figures so a finance client can
			// confirm what actually changed without a second read.
			writeJSON(w, http.StatusOK, costCorrectionResponse{
				Corrected:  true,
				OldCostUSD: store.MicroToDollars(result.OldCostMicro),
				NewCostUSD: store.MicroToDollars(result.NewCostMicro),
			})
			return
		}
		// Nothing diverged (fresh key, or a matching re-post) — the row still
		// landed via CorrectManualCostEvent's own insert path, so this is the
		// same outcome as the override=false path and gets the same status
		// code (and, matching every other 201 on this endpoint, no body).
		w.WriteHeader(http.StatusCreated)
		return
	}

	if err := h.store.InsertManualCostEvent(r.Context(), ev); err != nil {
		// 🔴 CONTENTION IS CLASSIFIED FIRST HERE TOO, FOR THE SAME REASON AS THE
		// OVERRIDE BRANCH ABOVE — and #610 is the issue that made this branch
		// need it. InsertManualCostEvent now takes the write lock via
		// beginImmediateBounded (250ms) instead of a DEFERRED BeginTx, so it can
		// return store.ErrWriteLockUnavailable, and the honest answer to that is
		// the same retryable 503 + Retry-After the override half has always
		// given. Before #610 this branch had no contention case at all: it
		// blocked the DSN's full 5000ms and then fell into the 500 sink below,
		// so one endpoint told a caller that a lost race for the single write
		// lock was retryable or permanent depending on one request field.
		//
		// ORDER IS LOAD-BEARING, and the 409 below is exactly what it must
		// outrank: ErrCostConflict is a PERMANENT verdict (the stored cost is
		// immutable, retrying cannot help), and misclassifying a transient
		// condition as permanent destroys a retry that would have succeeded,
		// while the reverse merely costs a wasted one. Sentinels are exact
		// matches; anything heuristic — a message prefix, a text scan — must
		// stay downstream of every sentinel, which is the defect
		// TestContentionOutranksTheValidationPrefix records on the alias site.
		//
		// ⚠️ Only the sentinel gets this treatment. beginImmediateBounded wraps
		// ErrWriteLockUnavailable ONLY around a genuine lock outcome
		// (isPromoteContention gates it on the SQLite result code), so a
		// read-only or full database — code 8 — falls through to the 500 rather
		// than being advertised as "retry shortly", which would be a permanent
		// condition sold as transient.
		if errors.Is(err, store.ErrWriteLockUnavailable) {
			h.logger.Warn("insert manual cost: write lock unavailable", "err", logSafeErr(err))
			writeStoreContention(w)
			return
		}
		if errors.Is(err, store.ErrCostConflict) {
			// A keyed re-post changed the cost. cost_micro is immutable (#233), so
			// this is a rejection, not an overwrite — the stored figure is unchanged.
			// 409 tells the client its correction was NOT applied. The client-facing
			// remedy names BOTH paths a client can actually take: re-post under a new
			// idempotency_key, or (#346) opt into the sanctioned override with
			// override=true + override_actor + override_reason. (Deleting a single
			// cost row is operator-level, direct against the store; there is no
			// delete-by-key API route, so advertising it here would point the client
			// at a door it cannot open.)
			writeError(w, http.StatusConflict,
				"idempotency_key already recorded with a different cost_usd; the stored cost is immutable. To correct it, re-post under a new idempotency_key, or set override=true with override_actor and override_reason for a sanctioned, audited correction")
			return
		}
		// Sanitized for the same reason as the override sink above: this is the
		// SAME request body reaching the SAME kind of store error.
		h.logger.Error("insert manual cost", "err", logSafeErr(err))
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

// maxOverrideReasonLen caps the free-text override_reason field on a #346
// sanctioned cost-correction override — larger than maxIdentifierLen because
// a real correction reason ("Q3 invoice reconciliation, PO #4471") is a short
// sentence, not a bare identifier, but still bounded so a hostile/buggy
// client can't fill the append-only audit ledger with multi-KB rows per
// request (the same storage-DoS reasoning as maxIdentifierLen).
const maxOverrideReasonLen = 1024

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
// two repos' issues. Same discipline validateIssueID applies to the unattributed
// issue-id sentinel.
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

// validateIssueID rejects a client-supplied unattributed sentinel (#466). The sentinel
// family — the bare "unattributed" plus its ":<reason>" sub-buckets — is
// SERVER-ASSIGNED: the collector and the proxy write it when they genuinely could not
// resolve an issue. A client that forges it asserts "this spend has no issue" about
// spend it is simultaneously attributing to itself, moving its own dollars out of
// segment_reconciliation.no_outcome_cost_usd (the #466 thrash signal) and out of the
// attributed side of the cost_composition coverage split (#234).
//
// THIS IS THE STRICT, CLIENT-SURFACE RULE, and it belongs only on genuinely
// client-facing writes: POST /api/v1/costs (manual import) and POST /api/v1/outcomes.
// It must NOT be applied to POST /api/v1/events, which is the collector's own
// transport and legitimately ships the sentinel family — see validateShippedIssueID,
// which is the correct guard there. Applying this predicate to /events destroys
// capture for every developer who works on main or a detached HEAD.
//
// Case-INSENSITIVE, deliberately WIDER than the exact read-side matcher
// store.IsUnattributed. The read side must be exact (a case variant already in the
// table is data, and SQL and Go must classify it identically — see
// store's unattributedGlobPattern); the write side should refuse anything that merely
// resembles the sentinel, so a forged "UNATTRIBUTED:main" never becomes a row.
// Rejecting more at ingest than you match at read is the safe direction.
//
// The error text interpolates the CLIENT-SUPPLIED value; callers pass it to
// writeError, never to a logger. Anything that logs a rejected issue_id must wrap it
// in logsafe first.
func validateIssueID(s string) error {
	if store.ResemblesUnattributed(s) {
		return fmt.Errorf("issue_id %q is reserved; it is assigned by the server when an issue cannot be resolved", s)
	}
	return nil
}

// validateDeveloper rejects a client-supplied unattributed sentinel in the `developer`
// field (#619). It is the SAME rule validateIssueID applies to `issue_id`, on the other
// column the sentinel names — and it closes the worse half of the vector.
//
// 🔴 WHY THIS HALF IS WORSE, and why it is not merely symmetry. Forging `issue_id`
// moves a dollar BETWEEN BUCKETS INSIDE the forger's own denominator: out of
// segment_reconciliation.no_outcome and onto the unattributed side of the #234
// coverage split. The forger's headline TIER — points / (cost/1000) — is unchanged.
// Forging `developer` moves the dollar OUT OF THE FORGER'S DENOMINATOR ENTIRELY: the
// cost lands on the "unattributed" pseudo-developer instead, the forger's own cost
// falls, and their score RISES. That is a direct, self-interested incentive rather
// than an accident, and it is exactly the #1 gaming vector the sentinel's own doc
// (internal/collector/collector.go) says the sentinel exists to make VISIBLE.
//
// THE LEGITIMATE PRODUCERS ARE UNAFFECTED, AND THAT IS STRUCTURAL, NOT A CARVE-OUT.
// The org pollers (internal/collector/anthropicadmin, internal/collector/openaiusage)
// genuinely assign this sentinel to `developer`: an org-level invoice aggregate cannot
// honestly be split per person. They are untouched because they never cross this
// boundary — cmd/tierd wires them straight into ingester.Store(db) in-process, as it
// does the proxy's own capture. So `developer` needs NO /events allowlist, unlike
// `issue_id`, which needed one because the JSONL collector ships the sentinel family
// over the wire on every exploratory session. See validateEventRequest for the check
// that keeps that asymmetry honest.
func validateDeveloper(s string) error {
	if store.ResemblesUnattributed(s) {
		return fmt.Errorf("developer %q is reserved; it is assigned by the server when spend cannot be tied to a person", s)
	}
	return nil
}

// NOTE(#619): the predicate itself is store.ResemblesUnattributed, beside the
// IsUnattributed it must contain. The wrappers above are deliberately thin and each
// owns its FULL literal message rather than sharing a parameterized formatter: a
// (field, subject) parameter pair is transposable, and a transposition still produces
// a message containing the right field name, so no test could catch it. Share the
// predicate, not the prose.

// validateShippedIssueID is the /events variant of validateIssueID (#466). POST
// /api/v1/events is NOT a client surface — it is the transport the JSONL collector
// ships over (internal/shipper), and that collector legitimately assigns the whole
// sentinel family: internal/collector/jsonl.go routes a message that resolves to no
// issue into UnattributedMain / UnattributedDetachedHEAD / UnattributedNoIssue rather
// than dropping its cost, and the shipper forwards IssueID verbatim.
//
// So this endpoint ALLOWS the sentinel — but only in its four EXACT, canonical,
// case-sensitive spellings. Every near-miss and every case variant is still rejected,
// so a stored "UNATTRIBUTED:main" remains impossible and the forgery the strict rule
// closes stays closed; what changes is that the collector can ship what it honestly
// captured.
//
// Getting this wrong is not a degraded-reporting bug, it is total capture loss:
// handlePostEvents validates all-or-nothing, so one exploratory event fails a whole
// batch; shipper.postBatch treats 4xx as terminal with no retry and stops the
// collector's Run loop; and the CLI is stateless, so the next run re-ships the same
// batch and fails identically. A developer who has ever committed on main without an
// issue would lose 100% of their capture, permanently — including their well-formed
// attributed events. TestShipper_UnattributedFamilyRoundTrips (internal/shipper) is
// the end-to-end guard, because no shipper→real-handler test existed when that
// regression was first written.
func validateShippedIssueID(s string) error {
	// Derived from store.UnattributedFamily(), never a hardcoded copy. A retyped list
	// is how this becomes a capture outage: a fifth bucket added to internal/collector
	// would compile against a hardcoded switch, ship from the collector, and 400 the
	// whole batch terminally. store.UnattributedBuckets carries the enumeration and the
	// collector's mirror is pinned equal to it by
	// collector.TestUnattributedIssueIDMatchesStore, so the family cannot grow on one
	// side only.
	if slices.Contains(store.UnattributedFamily(), s) {
		// Exact canonical spelling from the collector — legitimate, allow.
		return nil
	}
	// Anything else that merely LOOKS like the sentinel is a forgery or a corrupt
	// value; fall through to the strict rule so near-misses and case variants are
	// rejected exactly as they are on the client surfaces.
	return validateIssueID(s)
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
	// #619, found while enumerating every client-supplied `developer` for that issue
	// and NOT listed in it. actual_spend is the tier-1 invoice ledger behind Spend
	// Leverage, so it is a denominator by another name: posting your own invoice under
	// the sentinel drops your actual-paid out of your row exactly as forging /costs
	// drops your metered cost. Same rule, same reason.
	if err := validateDeveloper(req.Developer); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	// SegmentReconciliation accounts for the window's whole spend against the
	// work_types segments above (#466): how much the segmented view can attribute to
	// a category, and the two disjoint reasons the rest cannot be. Without it the
	// segments silently exclude spend that produced no outcome and every per-type
	// TIER reads better than the pooled headline score. It is NOT filtered by
	// ?work_type — the gap is a property of the developer's window, not of the
	// segment a caller happened to request. omitempty so a window with no spend at
	// all ships no key.
	SegmentReconciliation *segmentReconciliationJSON `json:"segment_reconciliation,omitempty"`
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
	// KAnonSuppressed declares a suppression that happened INSIDE this segment
	// (#593). A segment has its own population and its own total, so it suppresses
	// independently of the top level — and review caught that without this field it
	// did so SILENTLY: a segment could ship with `total` missing and no signal
	// anywhere in the response. That is the failure the top-level declaration exists
	// to prevent, reproduced one nesting level down.
	//
	// Present only on a segment that actually suppressed; a segment where nothing was
	// withheld omits it, exactly like the top-level field.
	KAnonSuppressed *kanonSuppressedJSON `json:"kanon_suppressed,omitempty"`
}

// segmentReconciliationJSON accounts for ALL of the window's spend against the
// work-type segments (#466), so a reader can see how much spend the segmented view
// leaves out and why — rather than being told only that a gap exists.
//
// WHY THIS EXISTS. The segments denominate on outcome-linked cost: a token event's
// cost reaches a segment only via the work_type of an outcome sharing its
// (developer, issue). Spend that produced no outcome therefore lands in NO segment,
// so every per-type TIER is systematically better than the pooled headline score,
// which correctly keeps that spend in its denominator (DeveloperCostsWindow joins no
// outcomes). Un-reconciled, that bias is invisible and inverts the diagnosis: a team
// that thrashes sees the damage in the headline number, goes to the segment view for
// the cause, and finds the evidence removed. A caveat in the docs would not fix this;
// only a number that adds up does.
//
// Teams is deliberately absent: the block is emitted only in developer mode, and
// collapses to Total alone in an anonymized mode (#185, #270) — see the field docs.
//
// SCOPE: this ships the DATA. internal/dashboard does not render it yet, so the
// segmented panel a reader actually looks at still shows only attributed cost. That
// is a real gap in the user-facing story and belongs in its own issue (it needs a
// UI/UX pass, not a handler change); nothing here should be read as claiming the
// dashboard surfaces the gap today.
type segmentReconciliationJSON struct {
	// Developers carries one row per developer with spend in the window, sorted by
	// name. Omitted entirely in an anonymized mode (#185, #270), where a named
	// per-developer cost row would defeat the k-anonymity floor — the same collapse
	// dataQualityJSON makes when it drops ZeroTokenOutcomes for a bare count.
	Developers []developerCostReconciliationJSON `json:"developers,omitempty"`
	// Total is the name-free rollup across every developer in the window, and is
	// always present. It is strictly coarser than the pooled `total` already in the
	// response, so it carries in every aggregation mode without exposing a sub-k
	// cohort — the same argument that lets cost_composition (#234) ship in team mode.
	Total developerCostReconciliationJSON `json:"total"`
}

// developerCostReconciliationJSON splits one developer's whole-window spend into the
// part the work-type segments can see and the two disjoint reasons the rest cannot be
// filed under any work_type (#466).
//
// THE INVARIANT, and exactly how far it goes:
//
//	outcome_linked_cost_micro + no_outcome_cost_micro + unattributed_cost_micro
//	    == window_cost_micro
//
// That equality is EXACT and unconditional, and it is stated on the *_cost_micro
// fields deliberately. All four figures are folded from ONE snapshot — the same
// DeveloperIssueCostsWindow result — with every row classified into exactly one part
// and also added to the window total. The sums are integer micro-dollars (#69), so
// there is no rounding anywhere. The invariant is therefore an arithmetic IDENTITY,
// not a cross-check, and no concurrent writer can break it.
//
// 🔴 IT WAS BRIEFLY THE OTHER THING, AND THAT WAS A BUG. Anchoring window_cost_micro
// on DeveloperCostsWindow — a DIFFERENT query, several statements earlier, over a
// window whose upper bound is usually open — made the published invariant a race:
// rows written between the two reads land in one and not the other, so the response
// tells a consumer its own arithmetic does not add up. That consumer cannot tell that
// apart from corruption, and the doc simultaneously told it to ASSERT on these very
// fields. TestSegmentReconciliation_InvariantHoldsUnderConcurrentWrites measures it:
// with window anchored on DeveloperCostsWindow the invariant fails within a few
// hundred requests against one concurrent writer; folded from one snapshot it never
// fails. The genuine cross-query property (DeveloperCostsWindow agreeing with a
// fold of DeveloperIssueCostsWindow) is still pinned — as
// store.TestDeveloperCostsWindowFoldsToIssueCosts, over a fixed quiescent database,
// where a discrepancy really does mean a query bug. Do not move it back onto the
// wire; TestSegmentReconciliation_InvariantHoldsUnderConcurrentWrites fails if you do.
//
// In a quiescent read window_cost_micro still equals the developer's
// DeveloperCostsWindow total — the two queries read the same rows with the same
// predicate — so this remains the pooled TIER denominator, just sourced coherently.
//
// The *_cost_usd fields are a rendering convenience and DO NOT satisfy that equality
// bit-exactly. Each is converted independently via store.MicroToDollars (a float64
// division), and float division does not distribute over addition, so a consumer
// evaluating `a + b + c === d` on the dollar fields gets false for a substantial
// fraction of realistic inputs. A consumer that needs to ASSERT the invariant must use
// the micro fields; one that merely displays it should compare the dollars with a
// tolerance of ~1e-9. This is why the integer companions are on the wire at all.
//
// WHAT THIS DOES NOT SAY. OutcomeLinkedCostUSD is NOT the sum of the work-type
// segments' TotalCostUSD, and no invariant claims it is. The segments can legitimately
// double-count a cost row, for two independent reasons:
//
//  1. Work type. segIssues is keyed per work_type, so an issue carrying outcomes of
//     two different types has its whole cost charged to BOTH segments. This needs no
//     unusual data at all — one issue that fixed a bug and shipped a feature does it.
//  2. Repo. Under the tolerant join (store.RepoMatch, #231) a repo-blind cost row is
//     charged to EVERY qualified outcome sharing its issue id.
//
// Both over-counts are deliberate — they lower TIER, so ambiguity never flatters a
// developer — but together they mean "segments + gap == window" is false on ordinary
// data, not just at some exotic edge. This block therefore reconciles against the
// underlying cost ROWS, each counted exactly once, never against the segment totals.
type developerCostReconciliationJSON struct {
	// Developer is the canonical (alias-resolved, #125) identity. Empty on Total.
	Developer string `json:"developer,omitempty"`
	// WindowCostUSD is the developer's whole-window spend: the sum of every one of
	// their per-issue cost rows in the window, which is what DeveloperCostsWindow
	// reports for them and hence the denominator of their pooled headline TIER. It is
	// folded from the same snapshot as the three parts below so the partition is
	// exact (see the type doc); it is NOT read from DeveloperCostsWindow directly,
	// which would race.
	WindowCostUSD float64 `json:"window_cost_usd"`
	// The *Micro companions are the same four figures in integer micro-dollars
	// (1 USD = 1e6, #69) — the form the server actually sums. They are the ONLY
	// fields on which the partition invariant holds bit-exactly; see the type doc. A
	// consumer asserting the reconciliation (a test, a finance check, a dashboard
	// that refuses to render an inconsistent response) must read these, not the
	// dollars. Always present.
	WindowCostMicro        int64 `json:"window_cost_micro"`
	OutcomeLinkedCostMicro int64 `json:"outcome_linked_cost_micro"`
	NoOutcomeCostMicro     int64 `json:"no_outcome_cost_micro"`
	UnattributedCostMicro  int64 `json:"unattributed_cost_micro"`
	// OutcomeLinkedCostUSD is spend on (developer, repo, issue) keys that join to at
	// least one outcome in the window under the tolerant rule — the spend the
	// segmented view can attribute to some work_type. Named for the join, not for
	// "attributed": cost_composition (#234) already uses attributed/unattributed for
	// the different question of whether an issue id was resolvable at all.
	OutcomeLinkedCostUSD float64 `json:"outcome_linked_cost_usd"`
	// NoOutcomeCostUSD is the defect this block exists to surface: spend on a REAL
	// issue id that produced no outcome in the window — abandoned work, work still in
	// flight, or a PR that never merged. It is invisible to every segment because
	// work_type comes from the outcome, so there is no category to file it under.
	// Distinct from UnattributedCostUSD, with which it must never be conflated: here
	// the issue is known and the outcome is missing.
	NoOutcomeCostUSD float64 `json:"no_outcome_cost_usd"`
	// UnattributedCostUSD is spend the collector could not tie to any issue at all —
	// the store.IsUnattributed sentinel family (base plus the :main, :detached-head,
	// :branch-without-issue sub-buckets). This is the ESTABLISHED meaning of
	// "unattributed" in TIER and matches cost_composition's split; it is reported here
	// only so the three parts sum to the window total.
	UnattributedCostUSD float64 `json:"unattributed_cost_usd"`
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
	// RepoScope echoes the repository this response was narrowed to (#590), or is
	// omitted entirely on a fleet-wide read. It is the field that makes "scoped" and
	// "not scoped" DISTINGUISHABLE on the wire, which is the whole point of #590: the
	// original defect was not that repo= did nothing, it was that a caller could not
	// TELL it did nothing. A consumer that requires a scoped figure should assert on
	// this key's presence and value, never on having sent the parameter.
	RepoScope string `json:"repo_scope,omitempty"`
	// RepoScopeExcluded discloses what the strict scope DROPPED as repo-blind (#590,
	// the maintainer's ruling C). Present only on a scoped read that actually excluded
	// something; a scoped read over a fully-qualified window omits it, and that
	// absence is the clean signal.
	//
	// Read it as: this much of the window could not be placed in ANY repository, so
	// the scoped figures above are a LOWER BOUND, not a total. The excluded rows are
	// not claimed to belong to the scoped repository — they are unattributable by
	// construction, which is what the sentinel means.
	RepoScopeExcluded *repoScopeExcludedJSON `json:"repo_scope_excluded,omitempty"`
	// KAnonSuppressed is present when a sub-k residual cohort was WITHHELD from an
	// anonymized response (#593), along with the grand total and cost-composition
	// sidecar that would otherwise reconstruct it by subtraction.
	//
	// 🔴 IT IS MANDATORY THAT THIS IS SAID OUT LOUD. Without it, a suppressed response
	// is indistinguishable from "this window has no data" — and those demand opposite
	// reactions. "No data" means check your capture; "suppressed" means the answer
	// exists and cannot be shown at this k, and the remedy is to widen the window or
	// query a level with more people in it. Going silently quiet would be the same
	// failure this project keeps paying for: a response that cannot answer sharing a
	// shape with one that did.
	//
	// Name-free by construction: a count of withheld CONTRIBUTING developers and
	// nothing else. The count is itself useful — "1 developer withheld" and "40
	// withheld" are different problems (narrow window vs. an unpopulated org
	// hierarchy) with different fixes.
	KAnonSuppressed *kanonSuppressedJSON `json:"kanon_suppressed,omitempty"`
	// SpendLeverageSuppressed is true when a repo scope suppressed the spend-leverage
	// figures (#590). actual_spend records what the org PAID a vendor over a period
	// and carries no repository, so it cannot be scoped; dividing it by one
	// repository's list-price cost would manufacture a ratio inflated by roughly the
	// fleet-to-repo ratio. Suppressing and SAYING SO beats emitting a confidently
	// wrong number — the same reasoning as the exclusion disclosure above. A pointer
	// so the field is simply absent on unscoped reads rather than reading false.
	SpendLeverageSuppressed *bool `json:"spend_leverage_suppressed,omitempty"`
}

// kanonSuppressedJSON is the wire shape of a k-anonymity suppression (#593).
// Name-free: counts only.
type kanonSuppressedJSON struct {
	// Developers is how many contributing developers were withheld.
	Developers int `json:"developers"`
	// KAnonymity echoes the floor in force, so a consumer can see WHY the cohort was
	// too small without having to know the server's configuration.
	KAnonymity int `json:"k_anonymity"`
	// WithheldTotal and WithheldCostComposition state which aggregates were dropped
	// alongside the rows. A consumer that finds `total` missing must be able to learn
	// that it was withheld deliberately rather than that the window was empty.
	WithheldTotal           bool `json:"withheld_total"`
	WithheldCostComposition bool `json:"withheld_cost_composition"`
	// WithheldSegmentReconciliation states that the #466 block was dropped too.
	//
	// 🔴 IT HAS TO BE. segment_reconciliation.total.window_cost_micro is the window's
	// WHOLE cost — the same quantity as cost_composition.total_cost_usd, folded from
	// the same rows — so publishing it while cost_composition is nil'd hands back the
	// exact figure this pass withheld, in a field the pass had not been taught about.
	// The #466 block's own doc argues it is "strictly coarser than the pooled total
	// already in the response"; that argument is true in an unsuppressed response and
	// FALSE here, because the pooled total is precisely what is missing. Declared
	// rather than silently absent, for the same reason as the other two: absent must
	// never be confusable with "the window had no spend".
	WithheldSegmentReconciliation bool `json:"withheld_segment_reconciliation"`
}

// repoScopeExcludedJSON is the wire shape of what a strict repo scope excluded as
// repo-blind (#590). Name-free — counts and dollars only — so it carries identically
// in developer and team-aggregation mode (#185).
type repoScopeExcludedJSON struct {
	// TokenEvents and CostUSD are the repo-blind token_events in the window and their
	// summed cost. CostUSD is the number that matters: it is the size of the hole in
	// the scoped denominator.
	TokenEvents int64   `json:"token_events"`
	CostUSD     float64 `json:"cost_usd"`
	// Outcomes is the count of repo-blind outcome records in the window — the hole in
	// the scoped NUMERATOR, which moves a TIER score the other way.
	Outcomes int64 `json:"outcomes"`
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
	// Ranked mirrors developerScoreJSON.ranked for a GROUP aggregate (#502): the
	// #133/#136 evidence floor applied to the summed inputs. Not omitempty — a
	// false here is the load-bearing value, and omitting it would make "unranked"
	// indistinguishable from "an older server that never said", which is exactly
	// the ambiguity that let every team row render as ranked evidence (#603).
	//
	// TIER above is UNCHANGED by this flag. The wire still carries the true
	// quotient for a below-floor aggregate (the #502 case ships tier: 2.8e8), per
	// the house rule from #136: the number is never altered, only its ranking
	// authority revoked. Consumers must gate the HEADLINE on ranked, not expect a
	// scrubbed number.
	//
	// Deliberately NOT accompanied by a sample_n: the count that feeds this
	// boolean is the denominator that would make data_quality's
	// attributed_outcome_share invertible in the anonymized modes (see the k-anon
	// strip block below). A boolean discloses the threshold crossing, not the
	// count.
	Ranked bool `json:"ranked"`
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
		Ranked:          ts.Ranked,
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
// scope (#590) narrows every read below to ONE repository, strictly. It is threaded
// through EVERY read on this path rather than applied to a subset: a scoped response
// assembled from a mix of scoped and fleet-wide reads is the original #590 defect
// wearing a filter — it would look scoped and silently is not. The multi-repo
// control-arm tests assert on response FIELDS precisely so an unthreaded read shows
// up as a field that stayed fleet-wide.
func (h *Handler) loadWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) (windowScores, error) {
	costs, err := h.store.DeveloperCostsWindow(ctx, since, until, scope)
	if err != nil {
		return windowScores{}, fmt.Errorf("query costs: %w", err)
	}
	outcomes, err := h.store.AllOutcomesWindow(ctx, since, until, scope)
	if err != nil {
		return windowScores{}, fmt.Errorf("query outcomes: %w", err)
	}
	// Zero-token tripwire (#136): windowed token totals per (developer, issue), one
	// bulk query (no per-outcome N+1). Keyed by the raw token_events developer;
	// canonicalized below through the same alias map as the cost join.
	tokenTotals, err := h.store.OutcomeTokenTotals(ctx, outcomes, scope)
	if err != nil {
		return windowScores{}, fmt.Errorf("query token totals: %w", err)
	}
	// Bulk-fetch actual_spend so per-developer SpendLeverage is computed without an
	// N+1. Missing developer in the map → 0, which ComputeDeveloper treats as "no
	// actual_spend recorded yet".
	//
	// 🔴 SKIPPED UNDER A REPO SCOPE (#590), see the Store interface note on
	// ActualSpendAllWindow: actual_spend is what the org PAID and carries no
	// repository, so a scoped read has no honest per-repo actual-paid figure. An
	// empty map leaves every developer's SpendLeverage in the "not recorded" state
	// rather than dividing org-wide dollars by one repository's cost. The suppression
	// is declared on the wire by the caller, never left to be inferred.
	actualSpend := map[string]float64{}
	if scope.IsFleetWide() {
		actualSpend, err = h.store.ActualSpendAllWindow(ctx, since, until)
		if err != nil {
			return windowScores{}, fmt.Errorf("query actual_spend: %w", err)
		}
	}
	// Mixed-version signal (#293): the DISTINCT price_table versions that priced
	// token_events in this window. cost_micro is immutable per row (#233), so a
	// window legitimately spans versions while the response stamps a single active
	// price_table.version; this read surfaces the mix for the data-quality block.
	priceVersions, err := h.store.DistinctPriceVersionsWindow(ctx, since, until, scope)
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
	issueCosts, err := h.store.DeveloperIssueCostsWindow(ctx, since, until, scope)
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
	// Strict parameter allowlist FIRST, before any parsing or store work (#590): a
	// request carrying a parameter this endpoint does not implement is answered, not
	// quietly widened. `before` is the legacy alias for `until` that
	// parseWindowUpperBound still accepts, so it must appear here or the allowlist
	// would reject a parameter the handler honors.
	if !rejectUnknownQueryParams(w, r, "since", "until", "before", "team", "work_type", "repo") {
		return
	}
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

	// ?repo filter (#590), validated at the same trust boundary and for the same
	// reason as work_type: a bad value is a loud 400, never a silently different
	// result set.
	scope, ok := parseRepoScope(w, r)
	if !ok {
		return
	}
	// 🔴 REFUSED IN ANY ANONYMIZED MODE (#185 team, #270 division). This is the same
	// carve-out ?team= gets below, for the same reason, and it is not theoretical —
	// it was REPRODUCED during review before this guard existed.
	//
	// ?repo= is a caller-controlled POPULATION SELECTOR. Scoping narrows the cohort
	// BEFORE k-anonymity is applied, so a repository only one person works in drops
	// the whole team under the floor. Everything then folds into the residual "other"
	// bucket — which is emitted WITHOUT a floor — and `total` carries no floor at all.
	// Measured on a k=5 fixture: a 5-developer team scoped to a repo only `alice`
	// touched returned an "other" row and a `total` of exactly her figures (her TIER,
	// her cost, her points, her cost-per-point) in a mode whose whole contract is that
	// an individual's numbers never leave the server.
	//
	// The repository axis is a materially better attack primitive than the time axis:
	// repo↔developer association is stable, semantically meaningful, and knowable from
	// outside (every repository in an install is another ready-made probe), where a time slice needs
	// insider knowledge of who worked when. /api/v1/scores/{developer} is blanket-404'd
	// in these modes precisely to stop this read; ?repo= would have reintroduced it
	// around the side.
	//
	// REJECT, do not silently ignore. Quietly dropping the filter would hand back an
	// installation-wide aggregate that looks scoped — the exact #590 defect this whole
	// change exists to close. (?team= below merely SKIPS its branch, which is
	// fail-safe there because the k-anonymized resp.Teams is still what ships; here
	// the honest answer is that the question cannot be asked in this mode.)
	//
	// ⚠️ The unfloored residual bucket and unfloored `total` are a PRE-EXISTING hole,
	// reachable today by narrowing ?since= alone with no ?repo= involved. This guard
	// closes the new primitive, NOT that underlying defect — filed as #593, which
	// carries the reproduction for the time axis. Do not read this comment as a claim
	// that anonymized mode is now sound against cohort-narrowing generally.
	if !scope.IsFleetWide() && h.aggregation.Anonymized() {
		writeError(w, http.StatusBadRequest,
			"repo scoping is not available in "+h.aggregation.String()+"-aggregation mode (#185, #270): "+
				"narrowing to one repository can shrink a cohort below the k-anonymity floor and expose "+
				"an individual's figures. Remove ?repo=, or run the server in developer aggregation")
		return
	}

	// Load + canonicalize + join the window into per-developer scores. This is the
	// shared scores path (#277): loadWindow does exactly the store reads, alias
	// canonicalization, cost/outcome/token join and zero-token tripwire that both
	// /scores and /scores/compare need, so the two endpoints can never compute a
	// developer's score — or its k-anon inputs — differently.
	win, err := h.loadWindow(r.Context(), since, until, scope)
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
	// Per-(developer, issue) cost denominates each work-type segment.
	//
	// #333 MEASURED — decision: no index. (The issue is still open pending the
	// evidence being posted; this comment records the measurement, not a closure.)
	// This comment used to claim the
	// composition read was "the one non-index-covered read on this path". That was
	// FALSE, and the measurement is why (dogfood snapshot, 172,240 token_events
	// rows, 135,218 in the default 30d window, 2026-08-04):
	//
	//	GET /api/v1/scores (default window)  629 ms   total
	//	  DeveloperIssueCostsWindow          138 ms   <- the LARGEST window scan
	//	  CostCompositionWindow              132 ms
	//	  UnattributedBucketCostsWindow       85 ms
	//	  DeveloperCostsWindow                23 ms   (idx_token_events_scores)
	//
	// Composition is the second of THREE comparable ts-window scans, not a lone
	// offender, so indexing it leaves the shape of the endpoint unchanged: the
	// best case saves 64 ms of 629 (~10%, the (host, model, ts) option) and the
	// covering index saves 25 ms (~4%). Both measured worse than doing nothing:
	// a 10-column ts-leading covering index cut 132 ms -> 107 ms (-19%) for +19% DB
	// size and +14% insert latency on an append-hot table; a (host, model, ts)
	// index cut it to 68 ms at 30d but converts the window SEEK into a full-table
	// SCAN, so it is 3.3x SLOWER on a 1-day window (18.9 ms vs 5.6 ms) and gets
	// worse as token_events grows — the exact growth #333 was filed to protect
	// against. Its cost is window-proportional today (5.6 ms @1d, 35 ms @7d,
	// 133 ms @30d) and that is the property worth keeping.
	//
	// TestCostCompositionWindow_PlanStaysAWindowSeek pins it: if a future index
	// makes this query stop seeking the ts window, that test fails.
	// Reuse the per-issue cost rows loadWindow already fetched (#495/#333): one
	// DeveloperIssueCostsWindow scan per request now feeds BOTH the joint CI and
	// these work-type segment sidecars, instead of scanning the same window twice.
	issueCosts := win.issueCosts
	costComposition, err := h.store.CostCompositionWindow(r.Context(), since, until, scope)
	if err != nil {
		h.logger.Error("query cost composition", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	// Unattributed bucket split (#refocus, Option B): per-(developer, bucket) spend
	// over the SAME window, folded below into the org-level labeled split and each
	// developer's exploratory-overhead share. Raw token_events.developer, canonicalized
	// through the same alias map as the score rows.
	unattributedBuckets, err := h.store.UnattributedBucketCostsWindow(r.Context(), since, until, scope)
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
	// #593: set when a sub-k residual cohort was withheld, which forces every
	// unfloored aggregate over the same population to be withheld too (see the
	// assignment site). Zero value = nothing suppressed, so developer mode — which
	// never aggregates — is untouched.
	var kanonSuppressed scoring.KAnonSuppression
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
		teams, sup := scoring.AggregateTeamsKAnon(win.devScores, labelOf, h.kAnonymity)
		for _, ts := range teams {
			resp.Teams = append(resp.Teams, newTeamScoreJSON(ts))
		}
		// 🔴 #593: when the residual was withheld, EVERY unfloored aggregate over the
		// same population must go with it. resp.Total is a rollup of all devScores and
		// cost_composition is a whole-window sum, so leaving either in place lets a
		// caller subtract the named rows and reconstruct the suppressed cohort exactly
		// — measured at 6+2 developers, k=5: total(66) - namedA(60) = 6, the hidden
		// pair's cost to the cent. Suppressing the row alone MOVES the disclosure; it
		// does not close it. Flag recorded below for the data_quality block.
		kanonSuppressed = sup
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
	if horizon, hasHorizon, err := h.store.CostCoverageStart(r.Context(), scope); err != nil {
		h.logger.Error("query cost coverage start", "err", err)
	} else {
		perSource, err := h.store.SourceCoverageStart(r.Context(), scope)
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

	// #593: declare the suppression. Attached unconditionally when it happened — this
	// is the field that keeps a suppressed response distinguishable from an empty one.
	if kanonSuppressed.Any() {
		attachDataQuality().KAnonSuppressed = &kanonSuppressedJSON{
			Developers: kanonSuppressed.Developers,
			// The EFFECTIVE floor from the aggregation, not h.kAnonymity — the two
			// differ whenever the configured value is below scoring.MinKAnonymity and
			// gets clamped up. Reporting the configured value would leave an operator
			// unable to explain why a cohort at their configured k was suppressed.
			KAnonymity:              kanonSuppressed.K,
			WithheldTotal:           true,
			WithheldCostComposition: true,
			// #466: the reconciliation block republishes the whole-window cost that
			// withholding cost_composition just removed, so it goes with it.
			WithheldSegmentReconciliation: true,
		}
	}

	// Repo-scope disclosure (#590, the maintainer's ruling C). Attached in BOTH modes and
	// UNCONDITIONALLY on a scoped read — like the #351 coverage shares above and
	// unlike the exception-only signals, because the whole contract is that a scoped
	// response must be self-describing. A consumer must be able to answer "is this
	// figure scoped, and to what, and what did that cost me?" from the response alone,
	// without re-deriving it from the request it no longer has.
	//
	// A failure here is fatal to the response rather than logged-and-dropped, which is
	// the OPPOSITE of how the cost-horizon block above degrades, and deliberately so:
	// the horizon is an annotation on figures that are correct without it, whereas
	// these figures ARE scoped, and shipping them with the scope silently unstated
	// hands back something indistinguishable from a fleet aggregate. That is the exact
	// #590 defect. Fail loudly instead.
	if disc, err := h.buildScopeDisclosure(r.Context(), since, until, scope); err != nil {
		h.logger.Error("build scope disclosure", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	} else if !scope.IsFleetWide() {
		dq := attachDataQuality()
		dq.RepoScope = disc.Repo
		dq.RepoScopeExcluded = disc.Excluded
		dq.SpendLeverageSuppressed = disc.Suppressed
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
	// #593: withheld when a sub-k residual was suppressed — this rollup is the
	// differencing channel that makes row-suppression alone useless.
	if len(win.devScores) > 0 && !kanonSuppressed.Any() {
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
	segments, offFilterSup, err := h.buildWorkTypeSegments(r.Context(), win.outcomes, win.canon, win.canonTokens, issueCosts, workTypeFilter)
	if err != nil {
		h.logger.Error("build work-type segments", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	// Segment reconciliation (#466): account for EVERY dollar of the window against
	// the segments just built, so the spend they structurally cannot categorize is
	// reported instead of silently dropped. Built from win.outcomes — the UNFILTERED
	// list — on purpose: ?work_type must not turn another category's spend into
	// "no outcome". See segmentReconciliationJSON. Assigned before the #593 strip pass
	// below, which owns withholding it when k-anonymity suppresses.
	resp.SegmentReconciliation = buildSegmentReconciliation(
		win.outcomes, win.canon, issueCosts, h.aggregation.Anonymized(), h.logger)

	// 🔴 #593 ESCALATION — a suppressed SEGMENT forces the POOLED suppression.
	//
	// Weighted points partition EXACTLY across work types: every outcome carries
	// exactly one work_type (COALESCEd to 'feature'), so the segments are a complete
	// disjoint cover of the window. That makes the pooled total a cross-segment
	// differencing channel:
	//
	//   total.weighted_points - Σ(visible segments' totals) == the suppressed segment
	//
	// and subtracting that segment's own named rows yields the hidden cohort. Flooring
	// each segment independently does NOT close it; only withholding jointly does.
	//
	// So a segment-level suppression escalates to the whole response. The cost is real
	// — one narrow work type can suppress the pooled view — and it is the honest price
	// of a complete cover. Escalating here, after the segments exist, is why the strip
	// pass below sits at the end rather than at each attach site.
	//
	// 🔴 offFilterSup IS PART OF THIS LOOP, and leaving it out was a measured bypass.
	// The loop reads the EMITTED segments, so under ?work_type=<t> a sub-k segment of
	// some OTHER type is not in `segments` and nothing escalates — while `total`,
	// `cost_composition` and `segment_reconciliation` are all whole-window and ship
	// unfiltered. Measured on the #593 fixture: ?work_type=feature returned the pooled
	// total (13 points / $67.77) beside a visible feature segment (10 / $60.00),
	// recovering the hidden developer's 3 points and — via
	// segment_reconciliation.outcome_linked_cost_micro — their $7.77 to the cent.
	// buildWorkTypeSegments therefore builds every type and hands back the suppression
	// of any it filtered out, so the escalation is filter-independent.
	// TestKAnonResidual_WorkTypeFilterCannotBypassEscalation pins it.
	if offFilterSup.Any() && !kanonSuppressed.Any() {
		kanonSuppressed = scoring.KAnonSuppression{
			Residual:   true,
			Developers: offFilterSup.Developers,
			K:          offFilterSup.K,
		}
	}
	for _, seg := range segments {
		if seg.KAnonSuppressed != nil && !kanonSuppressed.Any() {
			kanonSuppressed = scoring.KAnonSuppression{
				Residual:   true,
				Developers: seg.KAnonSuppressed.Developers,
				K:          seg.KAnonSuppressed.KAnonymity,
			}
			break
		}
	}

	// 🔴 #593 SECOND PASS — STRIP EVERY WINDOW-AGGREGATE FIGURE WHEN SUPPRESSED.
	//
	// This runs LAST, after every data_quality attach site, and that ordering is the
	// design. A per-site guard is one forgotten `if` away from re-opening the leak, and
	// a future field added above would default to LEAKING. Here the default is safe.
	//
	// Review measured the leak this closes, and it defeated the suppression completely:
	// with `total` and `cost_composition` withheld, the response still shipped
	//
	//   unattributed_buckets: [{cost_usd: 5.00, share: 0.3333}]
	//   attributed_cost_share: 0.6667
	//
	// and cost_usd / share == 15.00 — the withheld composition total, exactly, from a
	// single bucket. Multiply by attributed_cost_share and you have the attributed
	// spend too. Over a one-developer window the bucket's cost_usd IS that person's
	// spend in dollars, with no arithmetic at all. Our own dogfood window is ~72%
	// unattributed, so a window with no unattributed spend is the unrealistic case.
	//
	// What stays, and why it is not the same class:
	//   - cost_coverage_* / source_coverage_start: properties of the INSTALLATION (when
	//     capture began), not aggregates over the window's population.
	//   - mixed_price_versions: a set of price-table version integers. Carries no
	//     figure and no count of people or work.
	//   - kanon_suppressed: the declaration itself, which is the point.
	if kanonSuppressed.Any() {
		// Owned here rather than only at their assignment sites: the escalation above
		// can flip suppression ON after resp.Total was already set, so the withhold has
		// to be re-asserted at the end. Belt and braces on the assignment-site guards,
		// which stay because they document intent where the value is produced.
		resp.Total = nil
		resp.CostComposition = nil
		// 🔴 #466 GOES WITH cost_composition, and it is the same leak, not a new one.
		// segment_reconciliation.total.window_cost_micro is the window's WHOLE cost —
		// the identical quantity as cost_composition.total_cost_usd, folded from the
		// same token_events rows — so leaving it in would hand back the figure the
		// line above just withheld, exactly, with no arithmetic. Its own doc argues it
		// is "strictly coarser than the pooled total already in the response"; that is
		// true of an unsuppressed response and FALSE here, where the pooled total is
		// precisely what is missing. The per-developer rows are already suppressed
		// upstream by the anonymized flag; this withholds the name-free rollup too.
		// TestSegmentReconciliation_WithheldWhenKAnonSuppresses pins it.
		resp.SegmentReconciliation = nil
		// 🔴 BIDIRECTIONAL, and the reverse direction is a SEPARATE leak review had to
		// point out. The escalation above handles segment-suppressed => pooled
		// suppressed. This handles pooled-suppressed => segments suppressed, which
		// leaks identically because the cover is disjoint in BOTH directions:
		//
		//   Σ segment.weighted_points - (named pooled rows) == the hidden cohort
		//
		// Reachable whenever a team is k-safe POOLED but sub-k within a segment: the
		// segment's residual absorbs it and clears k, so the segment publishes while
		// the pooled level suppresses. Measured: segments summing to 22 points / $106
		// minus a named pooled row of 20 / $100 returned exactly the two hidden
		// developers. Withholding one level and not the other closes nothing.
		for i := range segments {
			segments[i].Total = nil
			if segments[i].KAnonSuppressed == nil {
				segments[i].KAnonSuppressed = &kanonSuppressedJSON{
					Developers:    kanonSuppressed.Developers,
					KAnonymity:    kanonSuppressed.K,
					WithheldTotal: true,
					// Whole-window sidecars, not per-segment — see the segment attach
					// site. The strip pass above withholds both at the TOP level and
					// declares it there; saying `true` on a per-segment declaration
					// would misattribute a whole-window withhold to this segment.
					WithheldCostComposition:       false,
					WithheldSegmentReconciliation: false,
				}
			}
		}
		if resp.DataQuality == nil {
			resp.DataQuality = &dataQualityJSON{}
		}
		if resp.DataQuality.KAnonSuppressed == nil {
			resp.DataQuality.KAnonSuppressed = &kanonSuppressedJSON{
				Developers:    kanonSuppressed.Developers,
				KAnonymity:    kanonSuppressed.K,
				WithheldTotal: true,
				// 🔴 ALL THREE, and this fallback is the one that is easy to miss.
				// There are TWO ways kanonSuppressed becomes true. The pooled-first
				// path builds its declaration earlier, where every flag is already
				// set. The SEGMENT-ESCALATION path — pooled population k-safe, one
				// segment sub-k — flips it on AFTER that site has been skipped, so
				// this fallback is the only declaration the response gets. Omitting
				// a flag here withholds the figure and then reports that it was not
				// withheld, which is precisely the "absent must never be confusable
				// with 'the window had no spend'" rule these flags exist to enforce,
				// broken by the branch that needed it most.
				// TestKAnonResidual_SegmentSuppressionEscalatesBothWays pins it.
				WithheldCostComposition:       true,
				WithheldSegmentReconciliation: true,
			}
		}
		dq := resp.DataQuality
		// STRIP EXACTLY THE INVERTIBLE ONES — no more, no less. All three divide by
		// costComposition.TotalCostMicro, and publishing both halves of x/T publishes
		// T. Review recovered the withheld total to the cent by two independent
		// formulas: bucket.cost_usd / bucket.share, and
		// Σ buckets.cost_usd / (1 - attributed_cost_share).
		dq.AttributedCostShare = nil
		dq.UnattributedBuckets = nil
		dq.ExploratoryCostShare = nil

		// ⚠️ DELIBERATELY KEPT, and the restraint is the point. An earlier draft also
		// stripped attributed_outcome_share, unjoined_developers, zero_token_outcomes
		// and repo_scope_excluded. Review measured each and found none invertible:
		//   - attributed_outcome_share is a ratio of outcome COUNTS whose denominator
		//     is published nowhere (teamScoreJSON carries no sample_n), so there is no
		//     second equation to solve.
		//   - zero_token_* identities are already name-suppressed in anonymized mode,
		//     and the bare count pairs with nothing.
		//   - unjoined_developers carries counts only.
		//   - repo_scope_excluded is unreachable here: ?repo= is 400'd in anonymized
		//     mode before this block is built.
		// Stripping them cost real data-quality signal — they are the honest-coverage
		// fields an adopter needs most — and bought no privacy. Over-suppression is not
		// the safe default when it silently degrades the signals that tell an operator
		// their capture is broken; it just moves the harm somewhere less visible.
	}

	resp.WorkTypes = segments

	// Cost-composition sidecar (#234): nil (key omitted) when the window has no
	// token spend, so a clean window ships no `cost_composition` and the dashboard
	// panel stays hidden — same omit-when-empty discipline as data_quality (#136).
	// #593: the composition is an unfloored whole-window sum, so it restates the
	// suppressed cohort's spend just as surely as `total` does — measured leaking the
	// identical figure on the reproduction fixture. Withheld on the same condition.
	if !kanonSuppressed.Any() {
		resp.CostComposition = newCostCompositionJSON(costComposition)
	}

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
// filter, when non-empty (already validated by the caller), restricts the RESULT to
// that single type; a type with no outcomes yields a segment with no rows so a
// filtered request still gets a well-formed (empty) answer.
//
// 🔴 The filter narrows the OUTPUT ONLY — every present type is still built and still
// has its k-anonymity floor applied. The second return value carries a suppression
// fired by a type the filter EXCLUDED, so the caller's #593 escalation sees it. See
// the comment on `types` below for the measured differencing channel that shape
// closes; the short version is that a filter must never be a way to opt out of a
// suppression the same data triggers without it.
func (h *Handler) buildWorkTypeSegments(
	ctx context.Context,
	outcomes []store.Outcome,
	canon func(string) string,
	canonTokens map[store.DevIssue]int64,
	issueCosts []store.DevIssueCost,
	filter string,
) ([]workTypeSegmentJSON, scoring.KAnonSuppression, error) {
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
			return nil, scoring.KAnonSuppression{}, err
		}
	}

	// 🔴 EVERY type present is BUILT, even under a filter; only the OUTPUT is narrowed
	// (at the append below). Building just the requested type was a k-anonymity
	// differencing channel — measured, not theorised. With five k-safe `feature`
	// developers and one sub-k `security` developer, the UNFILTERED /scores correctly
	// withheld `total`, `cost_composition` and `segment_reconciliation` via the #593
	// escalation. But ?work_type=feature never CONSTRUCTED the security segment, so
	// nothing escalated and all three whole-window aggregates shipped in full:
	//
	//	total.weighted_points 13 - visible feature segment 10   == 3      (hidden points)
	//	outcome_linked_cost_micro 67_770_000 - segment $60.00   == $7.77  (hidden cost)
	//
	// A filter is a VIEW; it must never be a way to opt out of the suppression the
	// same data triggers without it.
	// TestKAnonResidual_WorkTypeFilterCannotBypassEscalation pins this, and
	// TestKAnonResidual_WorkTypeFilterStillNarrowsWhenKSafe is its control arm — the
	// filter must still narrow on k-safe data, or "suppress everything" would pass.
	//
	// A filter naming a type with no outcomes still yields a well-formed empty segment,
	// so a filtered request always gets a shaped answer.
	types := make([]string, 0, len(segByDev)+1)
	for wt := range segByDev {
		types = append(types, wt)
	}
	if filter != "" && segByDev[filter] == nil {
		types = append(types, filter)
	}
	sort.Strings(types)

	segments := make([]workTypeSegmentJSON, 0, len(types))
	// offFilterSup carries a suppression fired by a segment the FILTER excluded from
	// the response. It is returned rather than dropped because the caller's #593
	// escalation reads the emitted segments, and an excluded segment is exactly the
	// one an attacker would use to make that escalation invisible.
	var offFilterSup scoring.KAnonSuppression
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
		var segSup scoring.KAnonSuppression // #593, per-segment: see the assignment below
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
			teams, sup := scoring.AggregateTeamsKAnon(devScores, labelOf, h.kAnonymity)
			for _, ts := range teams {
				seg.Teams = append(seg.Teams, newTeamScoreJSON(ts))
			}
			// #593: a segment carries its OWN total over its OWN population, so it has
			// its own differencing channel and needs the same treatment. A per-type
			// segment is if anything a sharper lever than the whole window — narrowing
			// to `security` or `incident` work is a cheap way to shrink a cohort.
			segSup = sup
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
		// NOTE (#593): deliberately NOT gated on segSup here. Suppression of segment
		// totals is enforced in ONE place — the strip pass on the /scores path, which
		// nils every segment total whenever anything suppressed at either level.
		//
		// An assignment-site gate was written first and then removed: mutation testing
		// showed its deletion failed NO test, because the strip pass dominates it. A
		// guard that cannot fail is not defence in depth, it is dead code that reads
		// like protection — and this repo has been bitten by exactly that (a drift
		// check written so it could not fire on the one file that mattered). The strip
		// pass IS mutation-covered; keep the enforcement there.
		if len(devScores) > 0 {
			total := newTeamScoreJSON(scoring.RollupTeam("", devScores))
			seg.Total = &total
		}
		// Declare it on the segment. Without this a consumer reading work_types sees a
		// segment whose `total` simply is not there and cannot tell "withheld for
		// anonymity" from "this category had no data".
		if segSup.Any() {
			seg.KAnonSuppressed = &kanonSuppressedJSON{
				Developers:    segSup.Developers,
				KAnonymity:    segSup.K,
				WithheldTotal: true,
				// The composition sidecar is whole-window, not per-segment, so a
				// segment-local suppression does not withhold it. Saying `true` here
				// would be a false statement about what this suppression dropped.
				WithheldCostComposition: false,
				// Same reasoning for the #466 reconciliation: it is whole-window and
				// per-developer, not per-segment. The /scores strip pass is what
				// actually withholds it, and it declares that at the top level.
				WithheldSegmentReconciliation: false,
			}
		}
		// Narrow the OUTPUT here, AFTER segSup has been computed — so a sub-k segment
		// the caller filtered out of the response still reaches the escalation.
		if filter != "" && seg.WorkType != filter {
			if segSup.Any() && !offFilterSup.Any() {
				offFilterSup = segSup
			}
			continue
		}
		segments = append(segments, seg)
	}
	return segments, offFilterSup, nil
}

// addSaturating returns a+b clamped to the int64 range instead of wrapping, and
// REPORTS whether it had to clamp (#466).
//
// The reconciliation sums cost_micro across every row in a window. Each row is bounded
// at ingest (store.MaxCostUSD), but the SUM is not: enough max-value writes overflow
// int64. Wrapping would be silent AND self-concealing — all four accumulators wrap
// together, so the partition invariant still holds and a consumer following the
// documented "assert on the _micro fields" advice sees a perfectly consistent
// reconciliation whose totals are meaningless.
//
// 🔴 THE SECOND RETURN IS THE WHOLE POINT, and its absence was a bug. An earlier draft
// clamped silently and relied on a non-negativity check over the rollup to notice —
// but clamping is exactly what STOPS the sum going negative, so that check was
// unreachable by construction and the block published a saturated figure (~$9.2e12) as
// if it were a measurement. Saturation is only detectable at the moment it happens;
// callers must propagate this bool, not re-derive it from the result.
//
// The negative arm cannot fire on cost input (costs are non-negative) but is kept
// correct so the helper is not a trap if reused on a signed quantity.
func addSaturating(a, b int64) (sum int64, saturated bool) {
	sum = a + b
	// Overflow iff the operands share a sign and the result's sign differs. Note the
	// underflow arm needs `>= 0`, not `> 0`: MinInt64 + MinInt64 wraps to exactly 0.
	switch {
	case a > 0 && b > 0 && sum < 0:
		return math.MaxInt64, true
	case a < 0 && b < 0 && sum >= 0:
		return math.MinInt64, true
	}
	return sum, false
}

// newDeveloperCostReconciliationJSON builds one reconciliation row from the integer
// micro-dollar parts, emitting both the exact micros and their rendered dollars from
// the SAME values — so the two representations can never disagree about which number
// they describe. dev is empty for the name-free rollup.
func newDeveloperCostReconciliationJSON(dev string, window, linked, noOutcome, unattributed int64) developerCostReconciliationJSON {
	return developerCostReconciliationJSON{
		Developer:              dev,
		WindowCostUSD:          store.MicroToDollars(window),
		WindowCostMicro:        window,
		OutcomeLinkedCostUSD:   store.MicroToDollars(linked),
		OutcomeLinkedCostMicro: linked,
		NoOutcomeCostUSD:       store.MicroToDollars(noOutcome),
		NoOutcomeCostMicro:     noOutcome,
		UnattributedCostUSD:    store.MicroToDollars(unattributed),
		UnattributedCostMicro:  unattributed,
	}
}

// outcomeCoverKey is a (canonical developer, issue) pair. Deliberately NOT store.DevIssue,
// which carries a third Repo field: this index answers the repo question separately
// (via outcomeRepos), so a key that merely LEFT Repo zero would be one added field away
// from silently splitting into per-repo buckets and reclassifying joined spend as
// no-outcome. Two fields, no room for the mistake.
type outcomeCoverKey struct {
	developer string
	issueID   string
}

// outcomeRepos records, for one canonical (developer, issue), which repositories carry
// an outcome — enough to answer the tolerant repo join (store.RepoMatch, #231) in O(1)
// per cost row without rescanning the outcome list.
type outcomeRepos struct {
	// repoBlind is true when at least one outcome for this (developer, issue) is
	// itself repo-blind, in which case it joins a cost row in ANY repo.
	repoBlind bool
	// real holds the qualified repositories that carry an outcome. Allocated LAZILY:
	// a window is routinely all repo-blind (the proxy structurally cannot know a
	// repository, and every pre-#231 row carries the sentinel), and eagerly making a
	// map per (developer, issue) would allocate one per outcome that never holds a
	// key. Read through the nil map, which is legal and returns false.
	real map[string]bool
}

// outcomeCoverage answers, in O(1) per cost row, "does this (developer, repo, issue)
// cost row join at least one outcome?" — the same question store.JoinIndex answers
// with a value, reduced to a boolean.
//
// It is a named type with a method rather than a closure inside
// buildSegmentReconciliation so a test can call THE REAL PREDICATE. The rule below is
// a restatement of store.RepoMatch, and a restatement that is only asserted by a
// hand-built copy in a test is not asserted at all — see
// TestSegmentReconciliation_RepoJoinMatchesRepoMatch, which drives this method and
// store.RepoMatch over the full repo cross-product and fails on any disagreement.
type outcomeCoverage map[outcomeCoverKey]*outcomeRepos

// buildOutcomeCoverage indexes the window's outcomes by canonical (developer, issue)
// and by the repositories they were earned in.
//
// Developers are canonicalized through the same alias map as the cost rows (#125) so
// an OS-username cost row and a GitHub-login outcome collapse to one key. Without it
// every aliased developer's spend would misreport as no-outcome — the #466 gap would
// swallow the very spend it exists to explain.
func buildOutcomeCoverage(outcomes []store.Outcome, canon func(string) string) outcomeCoverage {
	cover := make(outcomeCoverage, len(outcomes))
	for _, o := range outcomes {
		key := outcomeCoverKey{developer: canon(o.Developer), issueID: o.IssueID}
		e := cover[key]
		if e == nil {
			e = &outcomeRepos{}
			cover[key] = e
		}
		if repoid.IsReal(o.Repo) {
			if e.real == nil {
				e.real = map[string]bool{}
			}
			e.real[o.Repo] = true
		} else {
			e.repoBlind = true
		}
	}
	return cover
}

// linked reports whether a cost row joins at least one outcome under store.RepoMatch
// (#231) — the rule JoinIndex.Sum applies when it charges that row into a work-type
// segment: a repo-blind cost row joins any outcome for its issue, and a qualified one
// joins its own repo's outcome OR a repo-blind outcome. Keeping the two rules identical
// is what makes the partition match what the segments actually counted; diverging in
// either direction silently moves real spend between outcome_linked and no_outcome.
//
// Stated against RepoMatch, not against Sum, because that is what is ASSERTED:
// TestSegmentReconciliation_RepoJoinMatchesRepoMatch drives this method and RepoMatch
// over the repo cross-product. The two differ on the empty repo — RepoMatch treats ""
// as blind, while Sum buckets the blind side on the literal repoid.Unqualified — which
// is unreachable from stored data (normalizeRepo maps "" to the sentinel and the column
// is NOT NULL DEFAULT 'unqualified'), but claiming exact Sum equivalence would be one
// step stronger than the test proves.
func (c outcomeCoverage) linked(developer, repo, issue string) bool {
	e := c[outcomeCoverKey{developer: developer, issueID: issue}]
	if e == nil {
		return false
	}
	// A repo-blind cost row matches every outcome for its issue — RepoMatch is true
	// whenever EITHER side is unqualified, so the row's own repo cannot discriminate.
	if !repoid.IsReal(repo) {
		return true
	}
	return e.repoBlind || e.real[repo]
}

// buildSegmentReconciliation accounts for every dollar of the window against the
// work-type segments (#466). See segmentReconciliationJSON for why this exists and
// developerCostReconciliationJSON for the invariant it maintains.
//
// It takes the UNFILTERED outcome list on purpose. A ?work_type=feature request must
// not report a developer's bugfix spend as "no outcome" — the gap being reconciled is
// a property of the window, and narrowing the outcome set to the requested segment
// would manufacture a gap that does not exist. Callers pass win.outcomes, never the
// filtered partition buildWorkTypeSegments works from.
//
// Returns nil when the window produced no per-issue cost rows at all — i.e. no
// token_events in the window — so a clean window ships no key. Note that is "no cost
// ROWS", not "no spend": a window containing only zero-cost events still yields rows
// and still gets a (zero-valued) block, which is correct, since a reader asking "where
// did the spend go" deserves the answer "there was none" rather than a missing key.
func buildSegmentReconciliation(
	outcomes []store.Outcome,
	canon func(string) string,
	issueCosts []store.DevIssueCost,
	anonymized bool,
	logger *slog.Logger,
) *segmentReconciliationJSON {
	if len(issueCosts) == 0 {
		return nil
	}
	// Package-level function taking the logger as a parameter, so a caller can pass
	// nil where the Handler's own constructor would have defaulted it. Guard rather
	// than panic: this is a reporting sidecar, and dying on the /scores path because a
	// diagnostic sink was unset would be a far worse failure than a stray log line.
	if logger == nil {
		logger = slog.Default()
	}

	// Index which (developer, issue) keys carry an outcome, and in which repos.
	cover := buildOutcomeCoverage(outcomes, canon)

	// windowMicro is the developer's window total folded from the SAME issueCosts
	// snapshot the three parts come from. It is deliberately NOT the
	// DeveloperCostsWindow total. loadWindow issues those as two separate,
	// non-transactional reads (handler.go: DeveloperCostsWindow, then several
	// intervening queries, then DeveloperIssueCostsWindow) over a window whose upper
	// bound is normally OPEN, so a token_events row written between them lands in one
	// and not the other and the PUBLISHED invariant breaks — not as corruption, but as
	// ordinary concurrency a consumer cannot distinguish from corruption.
	// TestSegmentReconciliation_InvariantHoldsUnderConcurrentWrites is the measurement:
	// it hammers /scores while writing, and it FAILS if this is moved back onto
	// DeveloperCostsWindow.
	//
	// Folding here makes the wire invariant an arithmetic IDENTITY that no concurrent
	// writer can break — the parts and the total are the same rows, summed once. The
	// genuine CROSS-QUERY property (DeveloperCostsWindow agreeing with a fold of
	// DeveloperIssueCostsWindow) is not given up, it is moved somewhere it can be
	// deterministic: store.TestDeveloperCostsWindowFoldsToIssueCosts pins it over a
	// fixed quiescent database, where a discrepancy really does mean a query bug.
	//
	// windowMicro lives on the SAME struct as the three parts, and that is what makes
	// the per-row invariant structural rather than a thing to remember: every cost row
	// adds to windowMicro and to exactly one part, in one place, so there is no way to
	// update one and forget the other.
	type parts struct {
		windowMicro                                    int64
		linkedMicro, noOutcomeMicro, unattributedMicro int64
	}
	byDev := map[string]*parts{}

	// saturated latches the moment ANY accumulator has to clamp. It must be captured
	// here, at the add: once a sum pins at math.MaxInt64 the four figures stay
	// mutually consistent and non-negative, so no property of the FINAL numbers can
	// reveal that they are no longer measurements. See addSaturating.
	saturated := false
	add := func(dst *int64, v int64) {
		sum, clamped := addSaturating(*dst, v)
		*dst = sum
		saturated = saturated || clamped
	}

	for _, c := range issueCosts {
		dev := canon(c.Developer)
		p := byDev[dev]
		if p == nil {
			p = &parts{}
			byDev[dev] = p
		}
		// Unconditional, BEFORE the classification below: a developer whose every
		// dollar is unlinked must still get a row rather than silently vanishing from
		// the reconciliation.
		add(&p.windowMicro, c.TotalCostMicro)
		switch {
		// The sentinel family is tested FIRST. Since #466 the outcome ingress rejects
		// a sentinel issue_id (validateOutcomeRequest -> validateIssueID), so a NEW
		// outcome can no longer be written on the sentinel and the two arms should
		// never both match. The ordering still matters for rows that predate that
		// guard or were inserted out of band: sentinel-first can only move cost OUT of
		// outcome_linked, never into it, so a legacy or forged outcome cannot inflate
		// the spend the segments appear to explain. Defence in depth behind the ingest
		// guard, not a substitute for it.
		case store.IsUnattributed(c.IssueID):
			add(&p.unattributedMicro, c.TotalCostMicro)
		case cover.linked(dev, c.Repo, c.IssueID):
			add(&p.linkedMicro, c.TotalCostMicro)
		default:
			add(&p.noOutcomeMicro, c.TotalCostMicro)
		}
	}

	// One row per developer with cost rows in this window, name-sorted so the response
	// is deterministic. Every key in byDev came from a cost row, and every cost row
	// added to windowMicro, so a developer whose every dollar is unlinked still gets a
	// row instead of silently vanishing from the reconciliation.
	devs := make([]string, 0, len(byDev))
	for dev := range byDev {
		devs = append(devs, dev)
	}
	sort.Strings(devs)

	out := &segmentReconciliationJSON{}
	var sumWindow, sumLinked, sumNoOutcome, sumUnattributed int64
	// negative latches PER DEVELOPER, and that is the point. Checking only the rollup
	// nets one developer's negative row against everyone else's positive spend, so a
	// single out-of-band negative is caught only if it drives the WHOLE FLEET negative
	// — while the affected developer's own published row ships a negative figure with
	// the partition invariant intact.
	negative := false
	for _, dev := range devs {
		p := byDev[dev]
		if p.windowMicro < 0 || p.linkedMicro < 0 || p.noOutcomeMicro < 0 || p.unattributedMicro < 0 {
			negative = true
		}
		add(&sumWindow, p.windowMicro)
		add(&sumLinked, p.linkedMicro)
		add(&sumNoOutcome, p.noOutcomeMicro)
		add(&sumUnattributed, p.unattributedMicro)
		if anonymized {
			continue
		}
		out.Developers = append(out.Developers, newDeveloperCostReconciliationJSON(
			dev, p.windowMicro, p.linkedMicro, p.noOutcomeMicro, p.unattributedMicro))
	}
	// Summed in micro-dollars and converted once, so the rollup is exact integer
	// arithmetic (#69) and cannot drift from the parts by float accumulation.
	out.Total = newDeveloperCostReconciliationJSON("", sumWindow, sumLinked, sumNoOutcome, sumUnattributed)
	// FITNESS TRIPWIRE — two independent conditions, and BOTH are needed.
	//
	//  1. saturated: an accumulator hit the int64 ceiling. This is the condition an
	//     earlier draft could not see. It clamped silently and then tested the FINAL
	//     numbers for negativity — but clamping is precisely what keeps them positive,
	//     so the check was unreachable by construction and the block shipped ~$9.2e12
	//     as a measurement, with the partition invariant intact and every figure
	//     non-negative. Nothing about the published numbers betrays it;
	//     TestSegmentReconciliation_SuppressedOnOverflow drives the real builder over a
	//     saturating fixture and fails if the block is published.
	//  2. negative: a genuinely negative figure. Costs are non-negative at ingest and
	//     there is no CHECK constraint on token_events.cost_micro, so this catches a
	//     negative row arriving out of band (or a future signed cost correction)
	//     rather than an overflow — a different fault with the same remedy. Latched
	//     PER DEVELOPER above as well as checked on the rollup here: a rollup-only
	//     test nets one person's negative against everyone else's spend and would
	//     publish their negative row while the fleet total looked healthy.
	//
	// Either way the block is not fit to publish, so it is dropped rather than served
	// as numbers a reader would reasonably trust. Logged, not 500'd: the rest of the
	// score response is unaffected and still worth serving. No client-controlled string
	// is logged — only the integers and two bools.
	//
	// ⚠️ THIS IS THE ONE ABSENCE THAT IS NOT DECLARED ON THE WIRE, and the exception is
	// deliberate rather than overlooked. Everywhere else this change insists that
	// "absent must never be confusable with 'the window had no spend'" — hence
	// withheld_segment_reconciliation. Here the absence signals a SERVER FAULT, not a
	// policy withhold, and a fault flag would be a new contract field whose only
	// reachable trigger is a fleet whose summed spend exceeds ~9.2e12 dollars. The
	// operator log is the signal; docs/api-compatibility.md names this third nil path
	// so a consumer is not left guessing.
	if saturated || negative || sumWindow < 0 || sumLinked < 0 || sumNoOutcome < 0 || sumUnattributed < 0 {
		logger.Error("segment reconciliation is not fit to publish; suppressing block",
			"saturated", saturated,
			"window_micro", sumWindow, "outcome_linked_micro", sumLinked,
			"no_outcome_micro", sumNoOutcome, "unattributed_micro", sumUnattributed)
		return nil
	}
	return out
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
	// RepoScope echoes the repository this detail was narrowed to (#590), omitted on
	// a fleet-wide read. Same contract as the /scores data_quality field of the same
	// name: assert on THIS to know a figure is scoped, never on having sent ?repo=.
	RepoScope string `json:"repo_scope,omitempty"`
	// 🔴 THERE IS DELIBERATELY NO repo_scope_excluded FIELD HERE, and it is not an
	// oversight — an earlier revision had one and review caught it.
	//
	// The exclusion measurement is ORG-WIDE by construction: store.UnqualifiedExclusionWindow
	// counts every repo-blind row in the window, with no developer predicate (and it
	// could not easily gain one — cost and outcomes live in different identity spaces
	// joined by the #125 alias map, which lives in this handler, not in SQL).
	//
	// On /scores, whose population IS the whole window, that grain matches. On a
	// SINGLE-DEVELOPER response it does not, and shipping it here was wrong twice
	// over. Measured: a request for alice's detail scoped to one repo returned
	// repo_scope_excluded = {cost_usd: 99} where all $99 was BOB's repo-blind spend —
	// so a reader would conclude alice's $3 might be understating by up to $99, when
	// none of it is hers. And an installation-wide absolute dollar figure inside a
	// response that declares itself scoped is squarely the embargoed shape this whole
	// issue exists to prevent.
	//
	// The org-wide disclosure lives on GET /api/v1/scores, where its grain is honest.
	// A consumer needing it for a scoped window reads it there.
	// SpendLeverageSuppressed is true when a repo scope suppressed ActualPaidUSD and
	// SpendLeverage above — they are org-wide by construction and cannot be scoped.
	// Without this field a scoped response's actual_paid_usd of 0 would be
	// indistinguishable from "this developer has no recorded actual spend", which are
	// materially different statements.
	SpendLeverageSuppressed *bool `json:"spend_leverage_suppressed,omitempty"`
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
	// Strict parameter allowlist + ?repo scope (#590), same posture and same reasons
	// as /scores. The issue names this endpoint explicitly: a per-developer figure
	// that silently spans every repository is the same defect at a finer grain, and
	// arguably a worse one, since a single developer's cost is the number most likely
	// to be quoted directly.
	if !rejectUnknownQueryParams(w, r, "since", "until", "before", "repo") {
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
	scope, ok := parseRepoScope(w, r)
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
	outcomes, err := h.store.AllOutcomesWindow(r.Context(), since, until, scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	costs, err := h.store.DeveloperCostsWindow(r.Context(), since, until, scope)
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
	//
	// 🔴 SKIPPED ENTIRELY UNDER A REPO SCOPE (#590). actual_spend has no repository
	// and cannot be divided by one, so under a scope there is no honest per-repo
	// actual-paid figure to report. Reading it anyway and dividing it by this
	// repository's list-price cost would inflate SpendLeverage by roughly the
	// fleet-to-repo ratio — a number that looks like a measurement and is an artifact.
	// Leaving actualPaid at 0 makes ComputeDeveloper treat it as "no actual_spend
	// recorded", which is the correct honest state for a scoped read, and the
	// suppression is DECLARED on the wire below rather than left to be inferred.
	var actualPaid float64
	if scope.IsFleetWide() {
		spendAll, err := h.store.ActualSpendAllWindow(r.Context(), since, until)
		if err != nil {
			// target is canon(r.PathValue("developer")): a URL path segment is percent-
			// decoded, so it is client-controlled and CRLF-injectable, and is never
			// charset-validated here. Sanitize via the shared logsafe barrier (#321).
			h.logger.Error("query actual_spend for developer", "developer", logSafeStr(target), "err", logSafeErr(err))
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		for dev, paid := range spendAll {
			if canon(dev) == target {
				actualPaid += paid
			}
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
	tokenTotals, err := h.store.OutcomeTokenTotals(r.Context(), targetOutcomes, scope)
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
	detailIssueCosts, err := h.store.DeveloperIssueCostsWindow(r.Context(), since, until, scope)
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
	disc, err := h.buildScopeDisclosure(r.Context(), since, until, scope)
	if err != nil {
		h.logger.Error("build scope disclosure for developer", "developer", logSafeStr(target), "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
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

		RepoScope: disc.Repo,
		// No RepoScopeExcluded — see the field's absence documented on
		// developerDetailResponse. disc.Excluded is org-grain and would be a fleet
		// absolute inside a single-developer response.
		SpendLeverageSuppressed: disc.Suppressed,
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
	// 🔴 #619, found while enumerating and NOT listed in the issue: WITHOUT THIS, THE
	// WHOLE #619 GUARD IS BYPASSABLE IN ONE HOP. The score join resolves every stored
	// developer through the alias map before aggregating, so an alias row is a rename
	// of the identity space applied retroactively to rows already in the table — the
	// write-side guards on /costs, /events, /outcomes and /actual_spend all check a
	// value that this endpoint can then relabel.
	//
	// BOTH columns, and they are two different attacks:
	//   - canonical == sentinel: {alias: "alice", canonical: "unattributed"} folds
	//     alice's entire cost and outcome history into the pseudo-developer. Her spend
	//     leaves the leaderboard — the #619 vector, achieved without ever naming the
	//     sentinel on a spend write.
	//   - alias == sentinel: {alias: "unattributed", canonical: "bob"} is the inverse,
	//     and worse for someone else: every org-poller aggregate and every
	//     proxy-unresolved dollar lands in BOB's denominator and destroys his score.
	//     Sabotage rather than self-dealing, but the same forged identity.
	//
	// An operator who genuinely wants an identity folded into the unattributed pool
	// has no business doing it by aliasing: that would make a real person's spend
	// indistinguishable from spend the server could not attribute, which is the one
	// distinction the sentinel exists to preserve.
	if err := validateDeveloper(req.Alias); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateDeveloper(req.Canonical); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.UpsertDeveloperAlias(r.Context(), req.Alias, req.Canonical); err != nil {
		// 🔴 ORDER IS LOAD-BEARING: THE SENTINEL RUNS FIRST. An errors.Is check is
		// an EXACT match on an identity the store deliberately wraps; the
		// "developer_alias:" prefix below is a HEURISTIC over message text. A
		// heuristic must never pre-empt an exact match, because the heuristic can
		// match something it was never meant to.
		//
		// This is not hypothetical. These two branches were the other way round,
		// and were correct only by the accident that beginImmediateBounded's error
		// happens not to start with "developer_alias:". MEASURED: stamping that
		// prefix onto the store's begin failure — `fmt.Errorf("developer_alias:
		// %w", err)`, a plausible "make the errors in this function consistent"
		// edit — left the ENTIRE tree green while turning a transient contention
		// into a permanent 400 that no client would ever retry. With the sentinel
		// first, that edit cannot change the status code at all: the defect is
		// unrepresentable rather than merely watched. Pinned by
		// TestContentionOutranksTheValidationPrefix.
		//
		// Contention is retryable and must not read as corruption — see
		// writeStoreContention. This site uses beginImmediateBounded, so it is one
		// of the request-path writers that can produce the sentinel.
		if errors.Is(err, store.ErrWriteLockUnavailable) {
			h.logger.Warn("upsert developer_alias: write lock unavailable", "err", err)
			writeStoreContention(w)
			return
		}
		// The store's validation errors (self-map, chain) are caller-facing 400s
		// with a descriptive message; only an unexpected failure is a 500. The
		// store returns plain errors.New for the validation cases — no sentinel to
		// match on — so match on the "developer_alias:" prefix it stamps on every
		// such message. Reordering cannot misclassify these: they wrap nothing, so
		// the errors.Is above is false for all of them
		// (TestValidationErrorsStillAnswer400UnderTheReorderedChecks).
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
		// Contention is retryable and must not read as corruption — see
		// writeStoreContention. Telling a DSAR operator "store error" when the
		// erasure merely lost a lock race invites them to report a failed
		// compliance action that a retry would have completed.
		if errors.Is(err, store.ErrWriteLockUnavailable) {
			h.logger.Warn("erase developer: write lock unavailable", "err", err)
			writeStoreContention(w)
			return
		}
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

// writeLockUnavailableRetryAfter is the Retry-After (seconds) advertised when a
// write-path request loses the race for SQLite's single write lock.
//
// One second, because the wait the client just absorbed was bounded at
// store.requestPathBusyTimeout (250ms) — so a retry a second later is past the
// window that just failed without making an interactive client feel hung.
const writeLockUnavailableRetryAfter = 1

// writeStoreContention answers a request that failed because ANOTHER writer holds
// the database write lock, and it must be gated on store.ErrWriteLockUnavailable —
// never on any other store error.
//
// 🔴 WHY THIS IS NOT A 500. The sentinel's own doc says callers gate an
// operator-facing hint on exactly it, and `tierd repair-repo` does
// (internal/store/repairrepo.go). The request-path writers that existed when this
// was written — the alias upsert and the GDPR erasure, since joined by the #346
// /costs override — produced the sentinel and then collapsed it into the generic
// 500 "store error", which reads as corruption: an operator who sees it goes
// looking for a damaged database when the true answer is "a concurrent writer had
// the lock; try again". 503 + Retry-After says the request is retryable, which a
// 500 explicitly does not.
//
// The 250ms cap this fires past is deliberately generous for the SERVING path,
// and the measurement is recorded here so the choice stays traceable: `repair-repo`
// keeps ONE transaction open across scan + per-row update + commit at ~3.46 µs/row —
// BenchmarkRepairRepoCommit on 5000 rows, `-benchtime 5x`, 17,289,550 ns/op
// (Apple M5 Max, 2026-08-04). 250ms therefore rides out a repair of roughly
// 72,000 rows before a request-path writer gives up.
//
// ⚠️ REPAIR-REPO IS NO LONGER THE SERVING-PATH CEILING, AND THIS SENTENCE HAS
// ALREADY BEEN WRONG ONCE (see the paragraph below). `tierd reprice --commit`
// (store.Reprice) now takes the write lock via beginImmediate BEFORE its scan
// rather than at its first UPDATE, so its hold spans scan + two writes per changed
// row + commit over EVERY token_events row at or above the version floor — a
// strict superset of repair-repo's repo-filtered subset, and unbounded by any
// developer or repo predicate. It is reachable while serving on exactly the terms
// repair-repo is (both tell the operator to run against a quiesced database and
// neither can enforce it). That does not make 250ms wrong — it is the same
// argument as the migrations below: a request arriving mid-reprice is precisely
// when a retryable 503 is the honest answer.
//
// ⚠️ THAT IS THE SERVING PATH, NOT THE TREE. An earlier version of this comment
// claimed `repair-repo` was the longest write in the tree; it is not.
// recomputeKnownSourceCosts, migrateCostUSDToMicro and migrateActualSpendToMicro
// (internal/store/store.go) each rewrite an ENTIRE table inside one UNBOUNDED
// beginImmediate. They are tier_migrations-marker-gated, so they run once — but
// that once is the first upgrade of an already-populated database, where the row
// count is all of token_events rather than repair-repo's repo-filtered subset,
// and it can exceed this cap comfortably. That does not make 250ms wrong: a
// request arriving while another process is mid-upgrade is exactly the case where
// a retryable 503 is the honest answer and a five-second stall behind the single
// connection is not.
func writeStoreContention(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(writeLockUnavailableRetryAfter))
	writeError(w, http.StatusServiceUnavailable, "database is busy: another writer holds the write lock, retry shortly")
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

// rejectUnknownQueryParams writes a 400 and returns false when the request carries
// ANY query parameter outside the endpoint's allowlist (#590).
//
// 🔴 This is not tidiness — it is half the fix, and the half without which the other
// half is a false green. net/http silently ignores unrecognized parameters, so before
// this, `?repo=x` on an endpoint with no repo support returned a FLEET aggregate that
// was byte-identical to a correctly scoped one. A caller who believed they had scoped
// a query got the whole fleet and had no way to find out. Adding the filter alone
// would leave exactly that footgun one typo away: `repos=`, `Repo=`, `repo_id=` would
// each silently widen a query back to fleet-wide while the caller's own assertion
// passed, asserting nothing.
//
// The rule this enforces: "could not scope" must never share a response shape with
// "scoped, and this is the result".
//
// Matching is EXACT and case-sensitive, deliberately. Accepting `Repo=` as a synonym
// would mean guessing which of several plausible spellings a caller meant, and a
// guess is how you end up silently answering a question nobody asked. An unknown
// parameter is a caller bug; the useful response says so and lists what IS accepted.
//
// Measured before adopting the strict posture (2026-08-03): internal/dashboard's
// assets/dashboard.js is the only known client of these endpoints and sends only
// `since` and `until`, so nothing in-tree breaks.
func rejectUnknownQueryParams(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	// 🔴 PARSE EXPLICITLY. Do NOT use r.URL.Query() here — it DISCARDS the parse
	// error and silently DROPS every pair it could not decode, which defeats this
	// entire check. Measured:
	//
	//   "since=X&repo=a/b;x=1"  -> Query() yields {since} only; repo VANISHES
	//   "since=X&repos=a/b;x=1" -> Query() yields {since} only; the UNKNOWN key vanishes
	//   "since=X&repo=%zz"      -> same
	//
	// Go 1.17+ rejects ';' as a pair separator, so a single semicolon anywhere in a
	// value voids the whole query string. Against Query() the allowlist then sees a
	// clean request, returns true, and the caller gets a FLEET-WIDE 200 — the exact
	// #590 defect, surviving its own fix, and reachable from an ordinary unquoted
	// shell variable: curl ".../scores?repo=$REPO".
	//
	// The lesson generalizes past this function: a validator built on a parser that
	// silently drops its own failures validates nothing. Check the error.
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			"malformed query string: "+err.Error()+
				". Query parameters are parsed strictly — a pair that cannot be decoded is "+
				"rejected rather than silently dropped, so a mistyped filter can never widen "+
				"the result set while looking filtered (#590)")
		return false
	}
	known := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		known[a] = true
	}
	var unknown, repeated []string
	for k, v := range q {
		if !known[k] {
			unknown = append(unknown, k)
			continue
		}
		// A repeated parameter is ambiguous, and every reader downstream takes the
		// FIRST via Get(). "?repo=a/b&repo=c/d" would scope to a/b and discard c/d
		// with no signal — a caller who believes they asked for c/d gets a/b's
		// figures. Same class as the silent drop above: answer the question asked,
		// or refuse; never answer a different one quietly.
		if len(v) > 1 {
			repeated = append(repeated, k)
		}
	}
	if len(repeated) > 0 {
		sort.Strings(repeated)
		writeError(w, http.StatusBadRequest,
			"repeated query parameter(s): "+strings.Join(repeated, ", ")+
				" — each may appear at most once; a repeat is ambiguous and only the first "+
				"value would take effect (#590)")
		return false
	}
	if len(unknown) == 0 {
		return true
	}
	// Sort both lists: map iteration order is randomized in Go, and an error message
	// that reshuffles between identical requests is one a test cannot pin and an
	// operator cannot diff.
	sort.Strings(unknown)
	sorted := append([]string(nil), allowed...)
	sort.Strings(sorted)
	writeError(w, http.StatusBadRequest,
		"unknown query parameter(s): "+strings.Join(unknown, ", ")+
			" — accepted: "+strings.Join(sorted, ", ")+
			". Parameters are matched exactly; an unrecognized one is rejected rather "+
			"than ignored, so a mistyped filter can never return a wider result set "+
			"that looks filtered (#590)")
	return false
}

// parseRepoScope reads and validates the ?repo= filter (#590), returning the scope to
// apply. An absent or empty parameter is store.FleetWide (unscoped). On an invalid
// value it has already written a 400 and returns ok=false.
//
// Validation goes through repoid.Canonical, so a caller's "Acme/Widget" matches
// rows stored as "acme/widget" instead of silently matching nothing.
//
// ⚠️ BE PRECISE ABOUT WHAT THIS CATCHES, because an earlier version of this comment
// over-claimed it. Canonical rejects MALFORMED values — fewer than two segments, an
// embedded URL (the "//" leaves an empty segment), illegal characters, over MaxLen,
// or the reserved sentinel. It does NOT and cannot reject a well-formed slug that
// simply names no repository we hold: "acme/alpah" canonicalizes fine and scopes to
// an empty window, and a host-qualified "github.com/acme/alpha" is accepted as a
// three-segment slug (the GitLab nested-group shape) rather than being stripped to
// "acme/alpha" — scheme/host stripping lives in repoid.FromRemoteURL, on the
// COLLECTOR's write path, not here.
//
// So the 400 buys "you sent something that is not a slug", not "you typo'd a repo
// name". A typo still yields an empty scoped result; what makes that detectable is
// the echoed data_quality.repo_scope, which reports the canonical slug actually
// queried. That is why the echo is part of the contract and not a convenience.
//
// Canonical also REFUSES the reserved 'unqualified' sentinel, which is the behavior we
// want at this boundary: "scope me to the rows whose repository is unknown" is not a
// question the scoped-figure contract can answer honestly. Those rows are surfaced as
// a disclosure (repo_scope_excluded), not as a scope you can select.
func parseRepoScope(w http.ResponseWriter, r *http.Request) (store.RepoScope, bool) {
	raw := r.URL.Query().Get("repo")
	if raw == "" {
		return store.FleetWide, true
	}
	canon, ok := repoid.Canonical(raw)
	if !ok {
		writeError(w, http.StatusBadRequest,
			"invalid repo: must be a canonical repository slug such as "+
				"\"owner/name\" (at least two non-empty segments, no scheme or host, "+
				"no trailing \".git\"); the reserved \"unqualified\" sentinel cannot be "+
				"selected as a scope")
		return store.FleetWide, false
	}
	return store.RepoScope(canon), true
}

// scopeDisclosure is the assembled wire disclosure for a scoped read (#590): what the
// scope was, what it excluded, and what it suppressed. The zero value is the
// fleet-wide case and emits no keys at all, so an unscoped response is byte-identical
// to its pre-#590 shape.
type scopeDisclosure struct {
	Repo       string
	Excluded   *repoScopeExcludedJSON
	Suppressed *bool
}

// buildScopeDisclosure measures what a scope cost this window and packages it for the
// wire (#590, ruling C). It is shared by /scores and the per-developer detail so the
// two can never describe the same scope differently — the drift that would otherwise
// let one endpoint disclose an exclusion the other hides.
//
// Fleet-wide reads short-circuit before touching the store: nothing was scoped, so
// nothing was excluded, and there is no honest disclosure to make.
func (h *Handler) buildScopeDisclosure(ctx context.Context, since, until time.Time, scope store.RepoScope) (scopeDisclosure, error) {
	if scope.IsFleetWide() {
		return scopeDisclosure{}, nil
	}
	ex, err := h.store.UnqualifiedExclusionWindow(ctx, since, until)
	if err != nil {
		return scopeDisclosure{}, fmt.Errorf("query unqualified exclusion: %w", err)
	}
	suppressed := true
	d := scopeDisclosure{Repo: scope.String(), Suppressed: &suppressed}
	// Omit-when-clean: a scoped window in which every row named a real repository
	// emits no exclusion key, and that ABSENCE is the signal the figure is a true
	// total rather than a lower bound. Emitting a zeroed block instead would make the
	// clean case and the excluded case look alike at a glance, which is the failure
	// this whole disclosure exists to prevent.
	if ex.Any() {
		d.Excluded = &repoScopeExcludedJSON{
			TokenEvents: ex.TokenEvents,
			CostUSD:     store.MicroToDollars(ex.CostMicro),
			Outcomes:    ex.OutcomeRecords,
		}
	}
	return d, nil
}
