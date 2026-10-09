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
	"runtime"
	"runtime/debug"
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
	// (POST /api/v1/costs), except that a keyed post never writes to a row that
	// already owns its key: it returns store.ErrCostConflict on a DIFFERENT
	// cost_micro (#295), store.ErrCostCorrectionIdentityMismatch on a matching
	// cost under a different identity (#871) — both surfaced as HTTP 409 — and
	// is a no-op otherwise. The automated ingester keeps the plain
	// InsertTokenEvent path, where per-message replays are cost-identical.
	//
	// It is a REQUEST-PATH writer: it takes the bounded write lock, so it may
	// also return store.ErrWriteLockUnavailable, which handlePostCosts answers with
	// 503 + Retry-After ahead of every other classification (#610).
	InsertManualCostEvent(ctx context.Context, e store.TokenEvent) error
	IsLegacyCostReplay(ctx context.Context, key string, cost int64) (bool, error)
	// CorrectManualCostEvent implements POST /api/v1/costs's sanctioned
	// override path (#346, ruling C — the follow-up to #295's ruling A above):
	// given override=true plus a required actor and reason, it may rewrite an
	// EXISTING keyed row's cost_micro instead of 409ing, and appends an
	// append-only cost_correction_audit row (old → new, actor, reason) when it
	// does. See store.DB.CorrectManualCostEvent for the full per-case contract.
	CorrectManualCostEvent(ctx context.Context, e store.TokenEvent, actor, reason string) (store.CostCorrection, error)
	// CorrectExistingManualCostEvent is CorrectManualCostEvent that returns
	// store.ErrNoManualCostRowToCorrect, writing nothing, when no row owns the key
	// (#854): an override for a polled provider may correct but never insert.
	CorrectExistingManualCostEvent(ctx context.Context, e store.TokenEvent, actor, reason string) (store.CostCorrection, error)
	InsertActualSpend(ctx context.Context, a store.ActualSpend) error
	InsertOrgActualSpend(ctx context.Context, o store.OrgActualSpend) error
	// OrgActualSpendTotals returns the net actual-paid roll-up per (org, period)
	// at or after since's month, summed across ALL sources (#42, #24), behind
	// GET /api/v1/org_actual_spend. org="" returns every org; non-empty filters
	// to an exact match.
	OrgActualSpendTotals(ctx context.Context, since time.Time, org string) ([]store.OrgActualSpendTotal, error)
	// RecordPROutcome writes one outcome via the merge_commit_sha dedup path
	// (ON CONFLICT DO NOTHING) behind POST /api/v1/outcomes (#188) and, in the
	// same transaction, removes that merge commit from the push-capture ledger
	// (#849). inserted is false when the SHA already existed and the write was a
	// no-op — the authoritative dedup signal, correct even under a concurrent
	// race, so a replay can never double-insert and is reported as a duplicate.
	// May return store.ErrWriteLockUnavailable (answered with a 503).
	RecordPROutcome(ctx context.Context, o store.Outcome) (inserted bool, err error)
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
	// DeveloperEvidenceWindow and BotDevelopers feed the k-anonymity census
	// (#856): per raw id, the captured vs manual token_events rows in the window,
	// and every id the webhook captured as a GitHub "Bot" author in the window.
	DeveloperEvidenceWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperCostEvidence, error)
	BotDevelopers(ctx context.Context, since, until time.Time) ([]string, error)
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
	// ReportWatermarks returns the as-of position of every mutation-bearing ledger
	// a report reads (#715) — the second half of a report's identity, since the
	// window predicate alone is unstable under late ingestion.
	//
	// 🔴 ITS SIX LEDGERS ARE NOT SIX READS OF CONVENIENCE, AND AN ALTERNATE
	// IMPLEMENTATION MUST READ THEM IN ONE SNAPSHOT. The whole product is a claim
	// about ONE INSTANT: read across separate transactions, a quality revision
	// committing between the outcomes read and the quality_history read yields a
	// manifest whose halves describe states that never coexisted — an as-of stamp
	// for a moment that never happened, which is worse than none because it looks
	// authoritative. store.DB does this with a single DEFERRED transaction; nothing
	// in this method set enforces it, which is why it is written here.
	//
	// The window and scope narrow the token_events / outcomes sequences ONLY. The
	// four mutation ledgers are read UNWINDOWED on purpose — their `ts` is the
	// instant of the mutation, not of the row mutated, so a revision made today to
	// a June outcome would be invisible to a June-windowed ledger read. See
	// store.Watermarks.
	ReportWatermarks(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.Watermarks, error)
	// ReportDigests returns the content identity of the token_events and outcomes
	// rows a report over [since, until) was computed over (#716, wired by #740) —
	// the strongest input identity in the program, and the only one that can see
	// an in-place UPDATE, which a MAX(id)/COUNT(*) watermark pair structurally
	// cannot.
	//
	// 🔴 THE PAIR MUST COME FROM ONE SNAPSHOT, AND CALLING EventsDigest THEN
	// OutcomesDigest IS THE TRAP. Those are two reads on two pooled connections;
	// a row landing between them yields a pair describing two different instants,
	// and because a manifest publishes them side by side as ONE claim about ONE
	// window, an auditor recomputing both together gets a mismatch with nothing
	// having been tampered with — a FALSE ALARM, the one failure mode a
	// tamper-evidence surface may not have. store.DB takes both inside a single
	// DEFERRED read transaction; an alternate implementation MUST too.
	//
	// It also owns the token/outcome window asymmetry (events over the
	// AttributableWindow-widened band, outcomes over the report window) — see
	// store.ReportDigests.
	//
	// 🔴 `scope` MUST BE THE SCOPE THE REPORT WAS SERVED UNDER (#747), and it is
	// the caller's job to pass the CANONICAL slug — parseRepoScope has already
	// run repoid.Canonical by the time the handler gets here. A fleet-wide digest
	// published beside a scoped report attests a SUPERSET of the rows that report
	// read, so another repository's ingestion reads as a divergence of a report it
	// cannot touch; a raw non-canonical slug matches ZERO rows and yields a
	// well-formed digest over nothing. Both failures are silent in the value and
	// visible only in `rows`.
	ReportDigests(ctx context.Context, since, until time.Time, scope store.RepoScope) (events, outcomes store.Digest, err error)
	// DistinctPriceVersionsWindow returns the ascending distinct price_table
	// versions that priced token_events in [since, until) (#293). Feeds the
	// mixed-version data_quality WARN on /scores: cost_micro is immutable per row
	// (#233), so a window can span multiple versions while the response stamps a
	// single active price_table.version — this read surfaces the mix. A zero `until`
	// is open-ended; an empty window returns nil.
	DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]int, error)
	// AllOutcomesWindow returns every outcome in [since, until) (#276); a zero
	// `until` is open-ended.
	//
	// 🔴 IT MUST RETURN ROWS IN THE TOTAL ORDER (ts, id), AND THAT IS PART OF THIS
	// INTERFACE'S CONTRACT, NOT AN IMPLEMENTATION DETAIL (#711). loadWindow builds
	// byDev straight from this slice, so its element order is BOTH the arrival
	// order of scoring.ComputeDeveloper's float point-sum AND the index domain of
	// the fixed-seed bootstrap CI (scoring.BootstrapCI draws `k := rng.IntN(n)`).
	// A permuted slice therefore moves ci_low/ci_high by far more than a rounding
	// artifact, and /scores/compare's `significant` is a CI-overlap test over the
	// same numbers — so an unordered implementation flips a published boolean.
	// That is measured, not reasoned:
	// TestScoresCompare_SignificanceFlipsOnRowOrder drives two stores holding the
	// same logical rows in two insertion sequences and, with the store's ORDER BY
	// reverted, one publishes significant=false while the other says true.
	//
	// ⚠️ "TOTAL" MEANS WITHIN ONE DATABASE. store.scoringOrderSQL's `id` is a
	// per-database rowid, so this contract fixes the order of one install's rows —
	// which is what every caller here needs. It is NOT a promise that two installs
	// fed the same webhook batch in different sequences publish the same CI; where
	// ts ties, they will not.
	//
	// 🔴 NOTHING ENFORCES THIS FOR AN ALTERNATE IMPLEMENTATION, AND THAT IS WHY IT
	// IS WRITTEN HERE. The guarantee is stated on the INTERFACE because an
	// alternate implementation — or a test double promoted to production —
	// satisfies the method set without inheriting store.scoringOrderSQL. The two
	// tests named above construct a real *store.DB and exercise the concrete SQL,
	// so they pin the CONCRETE implementation only; a second implementation would
	// ship unguarded, and this paragraph is the whole of its specification.
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
	// ActualSpendByPeriodWindow is ActualSpendAllWindow at (developer, period)
	// grain (#886), so a grouped read places each period's spend under the team
	// valid when the period began. Same no-scope rule as ActualSpendAllWindow.
	ActualSpendByPeriodWindow(ctx context.Context, since, until time.Time) ([]store.PeriodSpend, error)
	OverBudgetPeriods(ctx context.Context, since time.Time) ([]store.OverBudgetPeriod, error)
	// HierarchyMembership returns every dated team/division row (#886). It is
	// the ONLY developer->group source the score paths read: each event is
	// placed under the row valid at its own timestamp (see membership.go), never
	// under today's map.
	HierarchyMembership(ctx context.Context) ([]store.MembershipRow, error)
	// UpsertDeveloperAlias / DeleteDeveloperAlias also append dated membership
	// rows for every raw id whose placement the edit changes (#914), stamped by
	// the store; writtenBy is the credential fingerprint, as for UpsertHierarchy.
	// Both take the bounded write lock and may return
	// store.ErrWriteLockUnavailable or store.ErrMembershipClockBehind.
	UpsertDeveloperAlias(ctx context.Context, alias, canonical, writtenBy string) error
	DeleteDeveloperAlias(ctx context.Context, alias, writtenBy string) (bool, error)
	DeveloperAliases(ctx context.Context) (map[string]string, error)
	// UpsertHierarchy / UpsertHierarchies / EndMembership / ListHierarchy are the
	// org-hierarchy write surface (#232) that populates the team-aggregation
	// (#185) and org-seat-allocation (#41) tables. UpsertHierarchies is one
	// all-or-nothing transaction behind the bulk-import endpoint. The API layer
	// canonicalizes developer through the alias map (#125) before every call, so
	// hierarchy keys match the score-join's canonical keys.
	//
	// All three are REQUEST-PATH writers: each takes the bounded write lock
	// (#668), so each may also return store.ErrWriteLockUnavailable, which its
	// handler answers with 503 + Retry-After ahead of every other
	// classification. EndMembership is the one to be careful with — it can
	// return that sentinel OR store.ErrEndBeforeStart, and the contention check
	// must run FIRST, because misfiling a transient lock conflict as the
	// permanent 400 tells a client to fix input that was never wrong. Declared
	// on the interface, not just on *store.DB, because the handlers are written
	// against this contract and an alternate implementation cannot infer it.
	//
	// writtenBy (#886) is the writing credential's fingerprint (writerFingerprint),
	// recorded on the dated membership row; the store stamps the date itself.
	UpsertHierarchy(ctx context.Context, developer, team, division, org, writtenBy string) error
	UpsertHierarchies(ctx context.Context, rows []store.HierarchyRow, writtenBy string) error
	UpsertHierarchiesWithResult(ctx context.Context, rows []store.HierarchyRow, writtenBy string) (store.HierarchyImportResult, error)
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
	// SealedReport and SealReport are the sealed closed-period store (#913); the
	// sealer is their one caller.
	SealedReport(ctx context.Context, periodSize string, periodStart time.Time) (store.SealedReport, error)
	SealReport(ctx context.Context, r store.SealedReport, rollups []store.SealedRollup, persons []store.SealedPerson, check store.SealCheck) (sealed store.SealedReport, won bool, err error)
	SealFloor(ctx context.Context) (start time.Time, ok bool, err error)
	// SourceWatermarks is the seal gate's read (#913-D9).
	SourceWatermarks(ctx context.Context) ([]store.SourceWatermark, error)
	// ReadSnapshot runs fn with one read transaction; a seal computes its whole
	// period inside it (#913). GET /scores, GET /scores/compare and
	// GET /report_manifest compose their store reads inside one snapshot
	// (audit S03/S04). GET /scores/{developer} does not yet do so.
	ReadSnapshot(ctx context.Context, fn func(*store.Snapshot) error) error
	LatestSealedPeriod(ctx context.Context, periodSize string) (start time.Time, ok bool, err error)
	// SealedGap, LatestSealedGap and RecordSealedGap are the permanent seal gaps
	// (#913-D6); RecordSealedGap's one caller is `tierd seal --skip`.
	SealedGap(ctx context.Context, periodSize string, periodStart time.Time) (store.SealedGap, error)
	LatestSealedGap(ctx context.Context, periodSize string) (start time.Time, ok bool, err error)
	RecordSealedGap(ctx context.Context, g store.SealedGap) (store.SealedGap, error)
	SealedFoldInputs(ctx context.Context, id int64) ([]store.SealedRollup, []store.SealedPersonKey, error)
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
// loopback bind, which cmd/tierd enforces fail-closed (#59), except for the
// synthetic read-only demo (#476).
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

	// beforeWindowAssembly is a per-handler test hook at the boundary between
	// snapshot reads and CPU-only response assembly, including bootstrap CIs.
	beforeWindowAssembly func()

	// readToken, when non-empty, is a SECOND accepted bearer credential that is
	// authorized ONLY on the read routes (the requireRead-wrapped routes in
	// registerReadRoutes — the data the dashboard renders — plus GET /metrics in
	// developer mode, #944) and REJECTED with 403 on every mutating route and the
	// admin/finance GETs (#190). It is the least-privilege step short of SSO: a
	// CFO/VP-Eng can be handed dashboard read access without
	// the write/erase power the apiToken confers. Empty = no read scope armed;
	// then the read routes accept only apiToken, exactly as before #190. It never
	// relaxes the fail-closed bind rule — validateBind (cmd/tierd) still requires
	// the write apiToken for a non-loopback bind, except for the synthetic
	// read-only demo (#476). Set once before serving via
	// SetReadToken, mirroring the SetMetricsRegistry write-once contract.
	readToken string
	// metricsToken, when non-empty, is a THIRD bearer credential that opens
	// GET /metrics and nothing else (#944): requireMetrics is the only middleware
	// whose accepted set contains scopeMetrics. In an anonymised aggregation mode
	// it (or the write token) is the only way to scrape, because the read token
	// is refused there. Like readToken it has no effect without apiToken and
	// never relaxes the bind rule. Set once before serving via SetMetricsToken.
	metricsToken string
	// subsystems is the extensible health Registry (#48) — the ONLY health
	// state the handler holds. The watcher registers here at New (so /healthz's
	// legacy top-level `watcher` block is derived from subsystems["watcher"]);
	// future collectors register via RegisterSubsystem before serving. This
	// replaces the per-subsystem state pointers the handler used to accumulate.
	subsystems *health.Registry
	version    string
	startedAt  time.Time
	// buildCommit is the ldflags-injected commit (#638). It takes precedence over
	// the VCS stamps because the shipped CONTAINER has none: .dockerignore excludes
	// .git, so `buildvcs=auto` finds nothing and stamps nothing. Measured on the
	// published v0.4.0 image: zero vcs settings in the binary, while the release
	// TARBALL from the same workflow run carries vcs.revision=ca27d9f0…. The
	// container is precisely the deployment #638 was filed about, so without this
	// field the endpoint would be a no-op exactly where it matters most.
	buildCommit string
	// vcsOverride lets a test supply known stamps. A per-Handler field rather than
	// a package-level var: a mutable global swapped by tests races the moment any
	// test in this package calls t.Parallel(), which is an obviously-correct-looking
	// addition that would detonate on an unrelated PR.
	vcsOverride *vcsInfo
	limiter     *authLimiter      // per-IP failed-auth lockout (#36); nil/disabled = off
	metricsReg  *metrics.Registry // #67; nil = no /metrics route mounted
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
	// sealer serves /scores, /report_manifest and /scores/compare in an anonymised
	// mode from sealed calendar months (#913; sealed_read.go); EnableSealing sets
	// it. With none, those reads are a 503, never a live computation, unless
	// unsealedRecompute is set.
	sealer *sealer
	// unsealedRecompute lets an anonymised handler with no sealer read live; only
	// WithUnsealedRecompute sets it.
	unsealedRecompute bool
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
	// polledProviders is the set of price-table provider tags whose org usage
	// poller this process runs (#854), set once by WithPolledProviders. A manual
	// /costs row for one of them is refused unless it declares billed_to "other".
	// nil = no poller runs, and /costs admits every provider as before.
	polledProviders map[string]bool
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
// Option configures a Handler. Variadic so the 100+ existing New callers (almost
// all tests) are untouched, while the one production call site in cmd/tierd is
// explicit about what it injects.
type Option func(*Handler)

// WithCommit supplies the build commit from the binary's ldflags. Prefer it over
// relying on the Go toolchain's VCS stamps: see Handler.buildCommit for why the
// container has none.
func WithCommit(commit string) Option { return func(h *Handler) { h.buildCommit = commit } }

// WithPolledProviders names the providers whose org usage poller this process
// actually starts (#854). The caller passes only pollers it starts, never ones a
// config merely mentions: the refusal it arms is right only where a poller runs.
// An empty tag is ignored, so a model the price table does not recognise
// (store.ProviderOf "") is never treated as polled. Such a row is admitted even
// when it names a polled model another way (anthropic/claude-sonnet-4, "Claude
// Sonnet 4"), whose usage the poller counted under the canonical id.
func WithPolledProviders(providers ...string) Option {
	return func(h *Handler) {
		for _, p := range providers {
			if p == "" {
				continue
			}
			if h.polledProviders == nil {
				h.polledProviders = make(map[string]bool)
			}
			h.polledProviders[p] = true
		}
	}
}

// WithUnsealedRecompute lets a team/division handler with no sealer compute
// /scores, /report_manifest and /scores/compare over a free window, as every
// install did before #913's sealed months. It is for verify-report's in-process
// replay of a live-window manifest, which binds no listener; serve never passes
// it, so an anonymised serve publishes only sealed months.
func WithUnsealedRecompute() Option { return func(h *Handler) { h.unsealedRecompute = true } }

// withVCSStamps is test-only: it pins the build stamps instead of reading this
// test binary's own. Unexported so it cannot become a production configuration
// knob by accident.
func withVCSStamps(v vcsInfo) Option { return func(h *Handler) { h.vcsOverride = &v } }

func New(s Store, logger *slog.Logger, apiToken string, watcherState *health.WatcherState, version string, rateLimit RateLimitConfig, opts ...Option) *Handler {
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
	h := &Handler{
		store:      s,
		logger:     logger,
		apiToken:   apiToken,
		subsystems: reg,
		version:    version,
		startedAt:  time.Now(),
		limiter:    newAuthLimiter(rateLimit, nil),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// RegisterSubsystem adds a subsystem to the /healthz `subsystems` map under
// name (#48). It follows the write-once-before-serve wiring seam used by
// SetMetricsRegistry and friends: call it during startup, before Register
// mounts the routes. It panics on an empty/duplicate name or nil snapshotter
// (see health.Registry.Register) — a startup-wiring bug, caught at boot.
// Any new subsystem's Detail must redact raw errors to classes as the watcher does.
func (h *Handler) RegisterSubsystem(name string, s health.Snapshotter) {
	h.subsystems.Register(name, s)
}

// Register mounts all API routes on mux. Write endpoints and the admin GETs are
// wrapped with requireAuth (write scope): they 401 without a token and 403 for
// the read-only viewer token (#59, #190). The read routes (registerReadRoutes
// is the list — the data the dashboard renders) are wrapped with requireRead
// (#190) so the read-only viewer token is accepted alongside the write token. /health, /healthz,
// /livez and /version stay open — status and build identity only, no spend data.
//
// Scope boundary (#190): the read token grants ONLY the requireRead-wrapped routes
// in registerReadRoutes (plus the static dashboard, and GET /metrics in developer
// mode — requireMetrics, #944) — that function is the list;
// this comment does not repeat it. The finance/admin reads — GET /org_actual_spend
// and GET /developer_alias — stay write-scoped, so a viewer cannot read raw invoice
// totals or the identity-alias map.
func (h *Handler) Register(mux routeMux) {
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
func (h *Handler) RegisterReadOnly(mux routeMux) {
	h.registerReadRoutes(mux)
}

// routeMux is the one method Register, RegisterReadOnly and their route tables
// call. *http.ServeMux satisfies it; the interface exists so a test can record
// every pattern Register and RegisterReadOnly mount and walk each one (#944),
// which a bare ServeMux cannot enumerate.
type routeMux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// registerWriteRoutes mounts the write/ingest + admin routes, all requireAuth
// (write scope): they 401 without a token and 403 for the read-only viewer token
// (#59, #190). These are the routes RegisterReadOnly deliberately omits.
func (h *Handler) registerWriteRoutes(mux routeMux) {
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
	// scope (#190). The map itself stays write-scoped, but its consequences do not:
	// developer-mode /scores ships team_rollups to the read scope (#821 ruled B),
	// naming every team with its totals, from which membership can be inferred. These stay available in team-aggregation mode by design: org
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
func (h *Handler) registerReadRoutes(mux routeMux) {
	mux.HandleFunc("GET /api/v1/scores", h.requireRead(h.handleGetScores))
	// Before/after period comparison (#277): two half-open windows in, per-row
	// deltas + CI-overlap significance out. READ-scoped (requireRead, #190) and
	// under the SAME anonymized k-anon guard as /scores — see handleGetScoresCompare.
	// The static "compare" path is more specific than the "{developer}" wildcard
	// below, so ServeMux (Go 1.22+ precedence) routes it here, never to the
	// per-developer handler.
	mux.HandleFunc("GET /api/v1/scores/compare", h.requireRead(h.handleGetScoresCompare))
	mux.HandleFunc("GET /api/v1/scores/{developer}", h.requireRead(h.handleGetDeveloperScore))
	// Report manifest (#715): the (predicate, as-of) identity of the report
	// /scores returns for the same window and scope. READ-scoped, and NOT
	// unauthenticated — it carries the #713 price-table digests and per-window row
	// counts. See handleGetReportManifest.
	mux.HandleFunc("GET /api/v1/report_manifest", h.requireRead(h.handleGetReportManifest))
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
	// Build identity (#638). In registerReadRoutes, NOT registerWriteRoutes, so
	// RegisterReadOnly mounts it: the public demo is the deployment whose build is
	// hardest to identify any other way, and it is the one that runs read-only.
	// Unauthenticated alongside /healthz and /livez -- identifying a build is a
	// probe concern, and gating it behind a token would make it useless to the
	// operator verifying a deploy landed.
	mux.HandleFunc("GET /api/v1/version", h.handleVersion)
	// Prometheus scrape endpoint (#67). The dashboard does not read it. Its
	// running counters difference between two scrapes into one person's spend
	// and activity timeline, so in an anonymised aggregation mode the read token
	// is refused and only the metrics or write token scrapes it (#944); see
	// requireMetrics. Mounted only when a registry is wired (cmd/tierd); tests
	// that don't set one get no route.
	if h.metricsReg != nil {
		mux.HandleFunc("GET /metrics", h.requireMetrics(h.handleMetrics))
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

// SetMetricsToken wires the scrape-only metrics token (#944). Call before
// Register; "" (the default) leaves the metrics scope unarmed. cmd/tierd refuses
// to start when it equals the read or write token, so this setter trusts its
// caller to pass a distinct value, as SetReadToken does.
func (h *Handler) SetMetricsToken(token string) { h.metricsToken = token }

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

// authScope is the credential a presented token matched (#190, #944).
type authScope int

const (
	scopeNone    authScope = iota // no valid token presented
	scopeMetrics                  // matched the scrape-only metrics token (#944)
	scopeRead                     // matched the read-only viewer token (#190)
	scopeWrite                    // matched the write/admin apiToken
)

// scopeSet is the set of credentials a route accepts. Each middleware names its
// set explicitly: scopes are NOT ordered, because the metrics token and the read
// token each open a route the other may not.
type scopeSet uint8

func scopes(s ...authScope) scopeSet {
	var set scopeSet
	for _, a := range s {
		set |= 1 << a
	}
	return set
}

func (set scopeSet) has(a authScope) bool { return set&(1<<a) != 0 }

// The accepted sets. scopeMetrics appears only in the /metrics sets, so the
// metrics token opens nothing but GET /metrics (#944).
var (
	writeScopes             = scopes(scopeWrite)
	readScopes              = scopes(scopeRead, scopeWrite)
	metricsScopes           = scopes(scopeMetrics, scopeRead, scopeWrite)
	anonymisedMetricsScopes = scopes(scopeMetrics, scopeWrite)
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
	// Inside the auth check, so an unauthenticated caller still gets 401/403.
	next = requireJSON(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiToken == "" {
			next(w, r)
			return
		}
		if !h.authorize(w, r, bearerCandidate(r), writeScopes, "invalid token") {
			return
		}
		next(w, r)
	}
}

// requireRead wraps a READ route (registerReadRoutes is the list — the data the
// dashboard renders). It is satisfied by EITHER the read-only
// viewer token or the write apiToken (#190). When h.apiToken is
// empty, auth is disabled and the wrapper is transparent, exactly like
// requireAuth — the read token has no effect without the write token, matching
// the fail-closed loopback-only posture cmd/tierd enforces (except for the
// synthetic read-only demo, #476).
func (h *Handler) requireRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiToken == "" {
			next(w, r)
			return
		}
		if !h.authorize(w, r, bearerCandidate(r), readScopes, "invalid token") {
			return
		}
		next(w, r)
	}
}

// requireMetrics wraps GET /metrics, the only route the metrics token opens
// (#944). In developer mode it accepts the metrics, read or write token. In an
// anonymised aggregation mode it refuses the read token with 403: two scrapes
// difference into one person's spend and activity, which the k floor exists to
// withhold from viewers. With no metrics token configured there, only the write
// token scrapes. When h.apiToken is empty the wrapper is transparent, like every
// other read route.
func (h *Handler) requireMetrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiToken == "" {
			next(w, r)
			return
		}
		accepted := metricsScopes
		if h.aggregation.Anonymized() {
			accepted = anonymisedMetricsScopes
		}
		if !h.authorize(w, r, bearerCandidate(r), accepted, "invalid token") {
			return
		}
		next(w, r)
	}
}

// classify reports which configured secret candidate matches (#190, #944). It
// ALWAYS runs a full constant-time compare against the write secret, and against
// the read and metrics secrets whenever they are configured, so the result
// carries no data-dependent timing signal about any token's bytes. The `!= ""`
// short-circuits are on static config (not attacker-controlled input), so they
// leak nothing. cmd/tierd refuses to start with any two tokens equal; if two
// somehow matched, write wins, then read.
func (h *Handler) classify(candidate []byte) authScope {
	writeMatch := constantTimeTokenCheck([]byte(h.apiToken), candidate)
	readMatch := h.readToken != "" && constantTimeTokenCheck([]byte(h.readToken), candidate)
	metricsMatch := h.metricsToken != "" && constantTimeTokenCheck([]byte(h.metricsToken), candidate)
	switch {
	case writeMatch:
		return scopeWrite
	case readMatch:
		return scopeRead
	case metricsMatch:
		return scopeMetrics
	default:
		return scopeNone
	}
}

// Wrong-scope 403 bodies. They name a credential class and a flag, never a
// token value.
const (
	errReadTokenForbidden    = "read-only token is not authorized for this endpoint"
	errMetricsTokenForbidden = "metrics token is not authorized for this endpoint: it opens GET /metrics only"
	errReadTokenMetrics      = "the read token cannot scrape /metrics in team or division aggregation mode (#944): scrape with the token set by --metrics-token (TIER_METRICS_TOKEN)"
)

// authorize runs the shared per-IP failed-auth lockout (#36) and the
// constant-time scope check for requireAuth, requireRead, requireMetrics
// (Bearer) and ProxyAuth (X-Tier-Token). They validate the same secrets, so they
// share one limiter keyed by client IP; gating only one surface would leave the
// others an unthrottled brute-force oracle. candidate is the raw token the caller
// extracted from its scheme-specific header; accepted is the route's set.
//
// Order matters: a locked-out IP is rejected with 429 BEFORE any compare, so the
// lockout response is data-independent of the secrets (no timing/length leak).
// A wholly-invalid token (scopeNone) burns exactly one full-length compare per
// configured secret (see constantTimeTokenCheck) and is the only path that
// records a failure. Returns true only when the presented scope is in accepted.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, candidate []byte, accepted scopeSet, unauthMsg string) bool {
	ip := clientIP(r, h.limiter.trustedProxies())
	if d, locked := h.limiter.retryAfter(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed authentication attempts; retry later")
		return false
	}
	// Three outcomes, three limiter dispositions:
	//   - no valid token → failure: record it and 401. This case MUST come
	//     first so an absent / invalid token never reaches the accepted check.
	//   - a valid token the route accepts → success: clear the IP's failure
	//     counter (the #36 don't-punish-a-fat-fingered-operator behavior).
	//   - a valid token the route does NOT accept → 403 and limiter-NEUTRAL: a
	//     valid credential is not a brute-force attempt, so it is not a failure;
	//     and it must not RESET the counter, which would blunt the lockout for
	//     the surrounding scopeNone guesses.
	switch got := h.classify(candidate); {
	case got == scopeNone:
		h.limiter.recordFailure(ip)
		writeError(w, http.StatusUnauthorized, unauthMsg)
		return false
	case accepted.has(got):
		h.limiter.recordSuccess(ip)
		return true
	case got == scopeMetrics:
		writeError(w, http.StatusForbidden, errMetricsTokenForbidden)
		return false
	case accepted.has(scopeMetrics): // the read token on anonymised /metrics
		writeError(w, http.StatusForbidden, errReadTokenMetrics)
		return false
	default:
		writeError(w, http.StatusForbidden, errReadTokenForbidden)
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
// request is forwarded so the tierd token never reaches the provider. This strip
// runs only when a token is configured; the proxy's own Rewrite strips every
// X-Tier-* header in every mode, which is what covers the tokenless one (#865).
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
		if !h.authorize(w, r, candidate, writeScopes, "invalid or missing "+ProxyTokenHeader+" header") {
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
	// When non-empty, the outcome of a re-post is decided by
	// store.DB.InsertManualCostEvent's pre-check (#871): the same identity
	// and cost is a 201 no-op that writes nothing, token counts included;
	// a different cost or identity is a 409. When empty, the row inserts
	// unkeyed and re-posts will
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
	// BilledTo declares who billed this spend (#854). The only accepted value is
	// BilledToOther; omitted or "" declares nothing. It is required only for a
	// model whose provider has a running org poller: see
	// PolledProviderOverlapRemedy. Stored on the row, never compared on a re-post.
	BilledTo string `json:"billed_to,omitempty"`
}

// BilledToOther is the one accepted costRequest.BilledTo value: this spend is
// not billed to the org whose usage a running poller already records (#854).
const BilledToOther = "other"

// PolledProviderOverlapRemedy is the single remediation for manual /costs rows
// whose model's provider has an org usage poller running on this server (#854).
// The /costs 400 body, tierd serve's startup WARN and docs/api-compatibility.md's
// /costs entry all use it, so it covers a row being posted and a row already
// stored, per kind of row.
const PolledProviderOverlapRemedy = `This server runs the org usage poller for this provider, which already counts the polled org's usage, so a manual row for that usage is counted twice. ` +
	`To post spend billed where the poller cannot see it (Claude Max seats, Bedrock or Vertex, another org), send "billed_to": "other"; do not post the polled org's own usage. ` +
	`For a row already stored without billed_to: if it is Claude Max, Bedrock, Vertex or another org's usage, no action is needed; ` +
	`if it has an idempotency_key and repeats the polled org's usage, correct it to cost_usd 0 with the audited override, sending the stored row's idempotency_key, developer, issue_id, model and fidelity with override=true, override_actor and override_reason. ` +
	`The override corrects the cost only: the row's token counts stay counted. ` +
	`A row with no idempotency_key, or whose stored fidelity is neither daily nor estimated, cannot be corrected through the product.`

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
	if requireJSONEOF(dec) != nil {
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
	if req.CostUSD > store.MicroToDollars(store.MaxTokenEventCostMicro) {
		replay, err := h.store.IsLegacyCostReplay(r.Context(), req.IdempotencyKey, store.DollarsToMicro(req.CostUSD))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store error")
			return
		}
		if !replay {
			writeError(w, http.StatusBadRequest, "cost_usd must be <= 10000 ($10,000); split a large import across rows")
			return
		}
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
	// Apply the /events per-field cap after legacy cache-write normalization.
	if int64(req.InputTok) > store.MaxTokenCount || int64(req.OutputTok) > store.MaxTokenCount ||
		int64(req.CacheRead) > store.MaxTokenCount || int64(req.CacheWrite5m) > store.MaxTokenCount ||
		int64(req.CacheWrite1h) > store.MaxTokenCount {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("token counts must be <= %d; split a large import across rows", int64(store.MaxTokenCount)))
		return
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
	// #854: a row for a provider whose org poller runs here is that poller's
	// usage unless it declares otherwise, and the poller never subtracts a /costs
	// row, so admitting it would count the spend twice. Tokened and cost-only rows
	// alike: the refusal keys on the model, not on what the row carries.
	switch req.BilledTo {
	case "", BilledToOther:
	default:
		writeError(w, http.StatusBadRequest, `billed_to must be "other" or omitted on /api/v1/costs`)
		return
	}
	// An override may still CORRECT an existing undeclared row (the remedy for a
	// stored double count); whether a row owns the key is decided inside the
	// store's write-locked transaction (CorrectExistingManualCostEvent), and an
	// override that would insert a new row is refused like any other.
	var overlapRefusal string
	if req.BilledTo == "" {
		if provider := store.ProviderOf(store.NormalizeModel(req.Model)); h.polledProviders[provider] {
			overlapRefusal = fmt.Sprintf("model %q is billed by provider %q: %s", req.Model, provider, PolledProviderOverlapRemedy)
		}
	}
	if overlapRefusal != "" && !req.Override {
		writeError(w, http.StatusBadRequest, overlapRefusal)
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
	// A KEYED /costs re-post never writes to the row that already owns its key
	// (InsertManualCostEvent). A different cost_micro is a 409 divergent cost
	// (#295, ruling A) — cost_micro stays IMMUTABLE at the first writer's value
	// (#233); the same cost_micro under a different developer/issue_id/model/
	// source/fidelity is a 409 identity mismatch (#871); the same cost_micro and
	// identity is an idempotent 201 that leaves the stored row, token counts
	// included, as first recorded. Comparison is on the stored
	// INTEGER cost_micro, so an honest retry whose float cost_usd / FX / rounding
	// jitter lands on the same micro value is NOT a conflict. To CHANGE a figure,
	// use the sanctioned audited override (ruling C) immediately below; a new
	// idempotency_key is a second row that every spend read adds (#860).
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
		BilledTo:       req.BilledTo,
		Timestamp:      time.Now().UTC(),
	}

	// #346 (ruling C): override=true routes through CorrectManualCostEvent
	// instead of the plain InsertManualCostEvent path. CorrectManualCostEvent
	// itself decides whether this is an ACTUAL correction (an existing row
	// whose identity matches and whose cost diverges — audited, 200) or
	// nothing to correct at all (no existing row, inserted in the same
	// transaction as its lookup, or a matching re-post, which writes nothing),
	// so both behave identically to the override=false path and return
	// 201/idempotent-201.
	// override=false keeps the original code path untouched — #295's 409
	// default is not routed through the override machinery at all.
	if req.Override {
		correct := h.store.CorrectManualCostEvent
		if overlapRefusal != "" {
			correct = h.store.CorrectExistingManualCostEvent
		}
		result, err := correct(r.Context(), ev, req.OverrideActor, req.OverrideReason)
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
			if errors.Is(err, store.ErrTokenEventCostCeiling) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			if errors.Is(err, store.ErrNoManualCostRowToCorrect) {
				writeError(w, http.StatusBadRequest, overlapRefusal)
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
				writeError(w, http.StatusConflict, costIdentityMismatchMsg)
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
		// Nothing diverged (a fresh key, inserted, or a matching re-post, which
		// wrote nothing) — the same outcome as the override=false path, so the
		// same status code (and, matching every other 201 on this endpoint, no
		// body).
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
		if errors.Is(err, store.ErrTokenEventCostCeiling) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, store.ErrCostCorrectionIdentityMismatch) {
			// The cost matched, but the key belongs to a row with a different
			// developer/issue_id/model/source/fidelity (#871). Nothing was written;
			// before #871 a matching cost fell through to the upsert, MAX-merged
			// this request's token counts into that row and answered 201. Same
			// response as the override branch, and for the same non-disclosure
			// reason (see there).
			writeError(w, http.StatusConflict, costIdentityMismatchMsg)
			return
		}
		if errors.Is(err, store.ErrCostConflict) {
			// A keyed re-post changed the cost. cost_micro is immutable (#233), so
			// this is a rejection, not an overwrite — the stored figure is unchanged.
			// 409 tells the client its correction was NOT applied. The store judges
			// cost before identity, so the colliding row may not be the caller's;
			// the remedy is conditional and the caller decides: the #346 same-key
			// override for their own row, a distinct key for a genuinely distinct
			// cost. A new key for a correction double-counts (#860). Wording quotes
			// docs/api-compatibility.md.
			writeError(w, http.StatusConflict,
				"idempotency_key already recorded with a different cost_usd; the stored cost is immutable and this correction was NOT applied. If the earlier row is your own post and you are correcting it, use the sanctioned override: re-post this request with the SAME idempotency_key, adding override=true, override_actor and override_reason; it replaces the stored cost on the same row and writes an audit row. Do not re-post under a new idempotency_key to correct a figure: a new key is a new row, and every spend read adds both. If the earlier row is not yours, do not override: reuse a key only for a genuine retry of the same event, so post a genuinely distinct cost under its own distinct idempotency_key. A pre-#82 realtime row cannot be corrected through this endpoint at all: see docs/api-compatibility.md")
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

// costIdentityMismatchMsg is POST /api/v1/costs's 409 body for
// store.ErrCostCorrectionIdentityMismatch, on both the plain and the override
// path. It deliberately does not name the identity the key belongs to.
const costIdentityMismatchMsg = "idempotency_key exists but does not match this request's developer/issue_id/model/source/fidelity; refusing to correct a row this request does not identify"

// --- POST /api/v1/actual_spend ---

// actualSpendRequest is the body posted by finance to record the
// enterprise-contract invoice total for a developer for a billing month.
//
// Period is YYYY-MM. Multiple posts for the same (developer, period)
// accumulate — credit memos and corrections enter as additive deltas
// (positive or negative) and the SUM at query time yields the net
// (#24). The audit trail lives in row history rather than overwriting.
type actualSpendRequest struct {
	Developer     string   `json:"developer"`
	Period        string   `json:"period"`
	ActualPaidUSD *float64 `json:"actual_paid_usd"`
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
// So this endpoint ALLOWS the sentinel — but only in the EXACT, canonical, case-sensitive
// spellings of store.UnattributedFamily(). Every near-miss and every case variant is still rejected,
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
	if requireJSONEOF(dec) != nil {
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
	if req.ActualPaidUSD == nil {
		writeError(w, http.StatusBadRequest, "actual_paid_usd is required")
		return
	}
	if math.IsNaN(*req.ActualPaidUSD) || math.IsInf(*req.ActualPaidUSD, 0) {
		writeError(w, http.StatusBadRequest, "actual_paid_usd must be finite")
		return
	}
	// Magnitude cap (#118): negatives are legal here (credit memos, #24), so
	// bound |value| — the negative-overflow case is the amd64 SUM-poison vector
	// (a bare float64→int64 of -1e19 yields a huge POSITIVE number there).
	// DollarsToMicro saturates deterministically as a backstop; this fails loud.
	if math.Abs(*req.ActualPaidUSD) > store.MaxCostUSD {
		writeError(w, http.StatusBadRequest, "actual_paid_usd magnitude must be <= 1e12")
		return
	}
	// Negatives are accepted as credit memos / refunds (#24). The store sums
	// across rows at query time so a $500 invoice + a $-100 credit memo
	// nets to $400.
	if err := h.store.InsertActualSpend(r.Context(), store.ActualSpend{
		Developer:       req.Developer,
		Period:          req.Period,
		ActualPaidMicro: store.DollarsToMicro(*req.ActualPaidUSD),
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
	Org           string   `json:"org"`
	Period        string   `json:"period"`
	ActualPaidUSD *float64 `json:"actual_paid_usd"`
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
	if requireJSONEOF(dec) != nil {
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
	if req.ActualPaidUSD == nil {
		writeError(w, http.StatusBadRequest, "actual_paid_usd is required")
		return
	}
	if math.IsNaN(*req.ActualPaidUSD) || math.IsInf(*req.ActualPaidUSD, 0) {
		writeError(w, http.StatusBadRequest, "actual_paid_usd must be finite")
		return
	}
	// Magnitude cap (#118): negatives are legal here (credit memos, #24), so
	// bound |value| — mirrors handlePostActualSpend. The negative-overflow case
	// is the amd64 SUM-poison vector; DollarsToMicro saturates as a backstop,
	// this fails loud at the boundary.
	if math.Abs(*req.ActualPaidUSD) > store.MaxCostUSD {
		writeError(w, http.StatusBadRequest, "actual_paid_usd magnitude must be <= 1e12")
		return
	}
	// Negatives accepted as credit memos / refunds (#24); rows accumulate.
	if err := h.store.InsertOrgActualSpend(r.Context(), store.OrgActualSpend{
		Org:             req.Org,
		Period:          req.Period,
		ActualPaidMicro: store.DollarsToMicro(*req.ActualPaidUSD),
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
// TableHash / FileHash (#713) answer the question version cannot: "does
// price_table.version 9 mean the same PRICES here as it does there". Two
// installs can both stamp version 9 while one runs a --prices override that
// moved a rate, or while one runs a binary whose provider-default cache
// multiplier constant differs — table_hash is a digest of the RESOLVED
// in-memory table, so it moves in both cases and the version stamp does not.
// file_hash digests the raw source bytes, so a comment-only edit to prices.yaml
// moves file_hash and leaves table_hash alone.
//
// ⚠️ PriceTableInfo.Source is deliberately NOT surfaced here. It is a local
// filesystem path, and this response goes to clients that have no business
// learning the server's directory layout. `tierd score-log` (a local CLI) does
// print it. See PriceTableInfo.
//
// 🔴 RULED 2026-08-28 (the maintainer, on the #713 security review): the two digests are
// deliberately NOT exposed on the unauthenticated `GET /api/v1/version`. They
// live on the REPRODUCIBILITY surfaces only — `/scores`, `/scores/compare` and
// `tierd score-log`. `/version` uses versionPriceTableJSON below, which has no
// digest fields at all, so the gate is enforced by the TYPE rather than by a
// call site remembering.
//
// The reasoning, kept so nobody re-adds them for convenience:
//
// `/version` is mounted UNAUTHENTICATED — identifying a build is a probe
// concern. Before #713 it disclosed only `version` + `effective_date`: coarse,
// low-entropy, no fingerprint of operator content. Adding the digests would have
// made it disclose an exact fingerprint of a file the operator may have written.
// Because the canonicalization is public, deterministic and UNKEYED, and price
// tables are low-entropy, a digest is a CONFIRMATION ORACLE: someone who can
// guess a private `--prices` file confirms the guess in one hash, and an install
// running the shipped table with one negotiated rate edited is grid-searchable
// on that rate.
//
// The counter-argument — "answer 'same build AND same prices' from one probe" —
// was rejected because it buys nothing real: a third party verifying a published
// report reads the digests from the REPORT'S OWN manifest, and never needed a
// live unauthenticated endpoint to fetch them. So the cost of gating is zero and
// the oracle closes outright, rather than being delegated to ingress config that
// many installs will not have.
//
// ⛔ Do NOT "unify" versionPriceTableJSON back into priceTableJSON, and do not
// add the digests to buildIdentity. TestVersionEndpoint_OmitsPriceTableIdentity
// asserts their ABSENCE and will redden if you do.
type priceTableJSON struct {
	Version       int    `json:"version"`
	EffectiveDate string `json:"effective_date"`
	TableHash     string `json:"table_hash"`
	FileHash      string `json:"file_hash"`
}

// priceTableStamp builds the FULL provenance stamp, digests included, from a
// store.PriceTableInfo. It exists so the two response builders that carry the
// reproducibility block (/scores and /scores/compare) cannot drift into stamping
// different subsets of the same struct — which is exactly what happened when the
// hashes were added to a struct whose call sites were populated by hand.
//
// ⚠️ `/api/v1/version` is NOT a caller and must not become one: it is
// unauthenticated and takes versionPriceTableJSON instead. See the ruling on
// priceTableJSON above.
func priceTableStamp(info store.PriceTableInfo) priceTableJSON {
	return priceTableJSON{
		Version:       info.Version,
		EffectiveDate: info.EffectiveDate,
		TableHash:     info.TableHash,
		FileHash:      info.FileHash,
	}
}

// versionPriceTableJSON is the DELIBERATELY REDUCED price_table block for the
// unauthenticated GET /api/v1/version: version + effective_date, no digests.
//
// 🔴 It is a separate type on purpose, and that is the entire enforcement
// mechanism. The alternatives were both worse. Reusing priceTableJSON and
// leaving the digest fields zero would emit `"table_hash":""` (the struct has no
// omitempty, so the keys are always present) — publishing an empty string is not
// gating, it is a bug that looks like one. Adding omitempty instead would make
// ABSENT mean EMPTY everywhere, so a defect that blanked the digest on /scores
// would silently drop the field rather than show a wrong value. With a distinct
// type, handing /version the full stamp is a COMPILE error.
//
// See the ruling on priceTableJSON above for why the digests are gated here.
type versionPriceTableJSON struct {
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
	// key at all. No element ever carries an individual developer name. Absent
	// too when the "other" bucket does not reach k: the whole response is then
	// withheld, named rows included, and data_quality.kanon_suppressed says so
	// (#864).
	Teams []teamScoreJSON `json:"teams,omitempty"`
	// Team is populated only when ?team=NAME filters the response to one
	// team's developers. Retained for backwards compatibility with the
	// pre-#25 scoped-team workflow.
	Team *teamScoreJSON `json:"team,omitempty"`
	// TeamRollups (#821) partitions `total` by org_hierarchy team: one row per
	// team with a scored developer plus at most one Unassigned row, so the rows'
	// weighted_points and total_cost_usd sum to Total's. Present exactly when Total
	// is present in developer mode, over the same population (?team= and
	// ?work_type= do not narrow it). ABSENT in every anonymized mode and whenever
	// k-anonymity suppresses: it is a per-team rollup with no k-floor, the
	// differencing channel #593 closes. Not `teams`, whose meaning is the
	// k-anonymized array keyed by `aggregation`.
	TeamRollups []teamRollupJSON `json:"team_rollups,omitempty"`
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
	// ranking, and cross-type TIER comparison is UNSUPPORTED by design. Developer
	// mode only: an anonymised mode publishes one breakdown per window, its group
	// rows and the grand total, so this key is absent there and ?work_type= is a
	// 400 (#864). omitempty so a window with no outcomes ships no key.
	WorkTypes []workTypeSegmentJSON `json:"work_types,omitempty"`
	// SegmentReconciliation accounts for the window's whole spend against the
	// work_types segments above (#466): how much the segmented view can attribute to
	// a category, and the two disjoint reasons the rest cannot be. Without it the
	// segments silently exclude spend that produced no outcome and every per-type
	// TIER reads better than the pooled headline score. It is NOT filtered by
	// ?work_type — the gap is a property of the developer's window, not of the
	// segment a caller happened to request. Developer mode only, like the segments
	// it reconciles (#864). omitempty so a window with no spend at all ships no key.
	SegmentReconciliation *segmentReconciliationJSON `json:"segment_reconciliation,omitempty"`
	// CostComposition surfaces WHERE the window's spend went (#234): cost by
	// normalized model, the per-class token composition, attributed vs unattributed
	// spend, and the two optimization levers (cache_read_share, premium_model_share).
	// Developer mode only (#864): every field but the total breaks the window's cost
	// down a second way, and a model (or token class, or the unattributed share) that
	// fewer than k people carry publishes their figure. Pure sidecar: the TIER formula
	// is untouched, same discipline as data_quality (#136). omitempty so a window with
	// no token spend ships no key and the dashboard panel stays hidden.
	CostComposition *costCompositionJSON `json:"cost_composition,omitempty"`
}

// workTypeSegmentJSON is one work-type's scoped leaderboard (#187), developer mode
// only: an anonymised mode publishes no segments (#864), so the type carries no
// group rows and no suppression declaration. Total is the segment's rollup. The
// numbers are type-scoped: WeightedPoints counts only this category's outcomes and
// TotalCostUSD is the cost of the (developer, issue) pairs those outcomes attach
// to. ActualPaidUSD/SpendLeverage are always 0 here — finance's actual_spend ledger
// is per (developer, period), not per category, so it cannot be split by work_type
// without inventing an allocation; the sidecar is deliberately left at the pooled
// top level.
type workTypeSegmentJSON struct {
	WorkType   string               `json:"work_type"`
	Developers []developerScoreJSON `json:"developers,omitempty"`
	Total      *teamScoreJSON       `json:"total,omitempty"`
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
// Developer mode only: an anonymised mode publishes no segments to reconcile and
// no second breakdown of the window's cost (#864).
//
// SCOPE: this ships the DATA. internal/dashboard does not render it yet, so the
// segmented panel a reader actually looks at still shows only attributed cost. That
// is a real gap in the user-facing story and belongs in its own issue (it needs a
// UI/UX pass, not a handler change); nothing here should be read as claiming the
// dashboard surfaces the gap today.
type segmentReconciliationJSON struct {
	// Developers carries one row per developer with spend in the window, sorted by
	// name.
	Developers []developerCostReconciliationJSON `json:"developers,omitempty"`
	// Total is the name-free rollup across every developer in the window, and is
	// always present.
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
	// len() is the count and the values name which tables. Developer mode only
	// (#864): it reads every row of the window, pseudo-developer spend included, so
	// in an anonymised mode it would say whether such spend was priced under another
	// table. omitempty so a uniform or empty window ships no key and the dashboard
	// mixed-version banner stays hidden.
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
	// Developer mode only (#864): beside the group rows and the total, the share
	// bounds one person's spend, so an anonymised mode never publishes it and
	// AttributionCoverage says so. In developer mode
	// numerically this is 1 − cost_composition.unattributed_share (the same integer
	// micros, each divided independently, so the two floats reconcile to ~1 ULP);
	// it assumes non-negative attributed micros (token cost, never refunds here), so
	// the value stays in [0,1].
	AttributedCostShare *float64 `json:"attributed_cost_share,omitempty"`
	// AttributionCoverage is "not_shown" on every anonymised window with spend
	// (/scores and each /compare window), because those modes never publish
	// AttributedCostShare (#864). Absent in developer mode and on a window with no
	// spend.
	AttributionCoverage string `json:"attribution_coverage,omitempty"`
	// ExcludesUnattributedSpend is true on every anonymised response and absent in
	// developer mode (#864 D′): pseudo-developer spend is outside every figure. It is
	// constant by design, never per window: a per-window value would say which
	// windows held header-less or uncaptured spend, which can be one person's.
	ExcludesUnattributedSpend bool `json:"excludes_unattributed_spend,omitempty"`
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
	// none of it is exploratory main) is emitted, not dropped by omitempty. Developer
	// mode only (#864), like unattributed_buckets.
	ExploratoryCostShare *float64 `json:"exploratory_cost_share,omitempty"`
	// UnattributedBuckets is the labeled split of the single unattributed mass the
	// cost-composition sidecar reports as one number (#refocus, Option B): one row per
	// reason the join could not tie spend to an issue (main/exploratory, detached-head,
	// branch-without-issue, plus the base sentinel for host-blind producers). The rows
	// sum to the composition's unattributed cost; each Share is of TOTAL window cost so
	// they compose with attributed_cost_share. Developer mode only (#864): a bucket one
	// person's spend fills publishes that spend, so an anonymised mode omits the split.
	// Present only when the window has unattributed spend (omit-when-clean).
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
	// events are append-only and outlive the provider session logs they came from, so
	// deleting those logs never moves the horizon. The store's contents do: loading
	// older logs (`tierd ship --since`, a collector's first pass) moves it earlier, and
	// erasing a developer's rows can move it later (store.CostCoverageStart).
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
	// UncountedActiveIDs is how many active ids in the window did NOT count toward
	// the k-anonymity floor, split by reason (#856; see kanon_census.go). Anonymized
	// modes only, once per /scores window, org-wide: it does not move with
	// ?work_type= or ?team=, and it never appears on a row, a segment or /compare.
	// omitempty so a window where every active id counted ships no key.
	UncountedActiveIDs *uncountedActiveIDsJSON `json:"uncounted_active_ids,omitempty"`
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
	// Developers is how many counted people were withheld (#853, #856). It is 0 when
	// the withheld group holds only ids that do not count: manual-only, push-only,
	// off-roster or bot ids (pseudo-developers are dropped before the fold, #864).
	Developers int `json:"developers"`
	// KAnonymity echoes the floor in force, so a consumer can see WHY the cohort was
	// too small without having to know the server's configuration.
	KAnonymity int `json:"k_anonymity"`
	// WithheldTotal states that the grand total was dropped alongside the rows. A
	// consumer that finds `total` missing must be able to learn that it was withheld
	// deliberately rather than that the window was empty.
	WithheldTotal bool `json:"withheld_total"`
	// WithheldCostComposition and WithheldSegmentReconciliation are always false
	// since #864: only an anonymised mode withholds, and it never builds either
	// block, so neither was withheld. Kept on the wire for compatibility.
	WithheldCostComposition bool `json:"withheld_cost_composition"`
	// WithheldTeams states that the group rows were withheld too, named rows
	// included: #864 withholds the whole anonymised response when the "other"
	// bucket does not reach k.
	WithheldTeams                 bool `json:"withheld_teams"`
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

// teamRollupJSON is one row of scoresResponse.TeamRollups (#821). Unassigned is
// true on exactly the row for developers with no org_hierarchy team; that row has
// an empty Team, so it ships no `team` key. Like teamScoreJSON it has no
// Developers field.
type teamRollupJSON struct {
	teamScoreJSON
	Unassigned bool `json:"unassigned"`
}

// buildTeamRollups groups team-labeled rows (#886: each labeled with the team
// valid when its events happened) in one pass and rolls each group up with
// scoring.RollupTeam: named teams in ascending order, then the unassigned row.
// A row labeled "" is unassigned, so every event lands in exactly one row; a
// developer who moved inside the window contributes to each team they held.
// rows arrive in (developer, label) order, so each group sums in a fixed order.
func buildTeamRollups(rows []scoring.LabeledScore) []teamRollupJSON {
	byTeam := map[string][]scoring.DeveloperScore{}
	for _, r := range rows {
		byTeam[r.Label] = append(byTeam[r.Label], r.Score)
	}
	names := make([]string, 0, len(byTeam))
	for team := range byTeam {
		if team != "" {
			names = append(names, team)
		}
	}
	sort.Strings(names)
	out := make([]teamRollupJSON, 0, len(byTeam))
	for _, team := range names {
		out = append(out, teamRollupJSON{teamScoreJSON: newTeamScoreJSON(scoring.RollupTeam(team, byTeam[team]))})
	}
	if devs := byTeam[""]; len(devs) > 0 {
		out = append(out, teamRollupJSON{teamScoreJSON: newTeamScoreJSON(scoring.RollupTeam("", devs)), Unassigned: true})
	}
	return out
}

// newTeamScoreJSON maps a scoring.TeamScore onto the wire shape (#25). One mapper
// keeps the build sites — pooled `total`, the ?team filter, `team_rollups`, the
// k-anon `teams`, and the per-work_type segment `teams`/`total` — from drifting as
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
	// scoredOutcomes is outcomes[i] as the scoring.Outcome loadWindow built for it
	// (canonical developer, zero-token flag), index-aligned with outcomes, so the
	// dated-membership grouping (#886) splits byDev by each outcome's own
	// timestamp without re-deriving the join.
	scoredOutcomes []scoring.Outcome
	// since, until and scope are the window this was loaded for, so a grouping
	// read (#886) re-reads cost over exactly the same window.
	since, until time.Time
	scope        store.RepoScope
	// peopleOnly marks a window withoutPseudoDevelopers produced: groupWindow,
	// which re-reads cost across membership boundaries, drops the
	// pseudo-developers' rows it builds too.
	peopleOnly bool
}

// hasSpend reports whether the window's cost rows sum to more than zero.
func (w windowScores) hasSpend() bool {
	var total int64
	for _, c := range w.issueCosts {
		total += c.TotalCostMicro
	}
	return total > 0
}

// withoutPseudoDevelopers is the window an anonymised mode folds (#864 D′): the
// pseudo-developers' rows (store.ResemblesUnattributed: the org pollers' remainder
// and header-less proxy spend) are dropped before any fold, count or total reads
// it, so the group rows, "other" and the grand total cover the same people and no
// difference of them is pseudo spend. The census is unaffected: it never counts a
// pseudo-developer. Pseudo-developers never hold paid spend (POST /actual_spend
// refuses them and invoices split over rostered seats only), so spend_leverage
// divides list spend by paid spend over the same people.
func withoutPseudoDevelopers(win windowScores) windowScores {
	devScores := make([]scoring.DeveloperScore, 0, len(win.devScores))
	for _, s := range win.devScores {
		if !store.ResemblesUnattributed(s.Developer) {
			devScores = append(devScores, s)
		}
	}
	issueCosts := make([]store.DevIssueCost, 0, len(win.issueCosts))
	for _, c := range win.issueCosts {
		if !store.ResemblesUnattributed(win.canon(c.Developer)) {
			issueCosts = append(issueCosts, c)
		}
	}
	win.devScores, win.issueCosts, win.peopleOnly = devScores, issueCosts, true
	return win
}

// windowReader is every store read a scores window makes, in loadWindow,
// loadScoresWindow, groupWindow, kanonCensusFor and buildScopeDisclosure. Live
// scores and comparisons, like a seal, pass one *store.Snapshot so every read
// composing the response sees one database state (#913, audit S03/S04).
type windowReader interface {
	DeveloperCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperCost, error)
	DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DevIssueCost, error)
	DeveloperEvidenceWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperCostEvidence, error)
	BotDevelopers(ctx context.Context, since, until time.Time) ([]string, error)
	CostCompositionWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.CostComposition, error)
	UnattributedBucketCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.UnattributedBucketCost, error)
	CostCoverageStart(ctx context.Context, scope store.RepoScope) (time.Time, bool, error)
	SourceCoverageStart(ctx context.Context, scope store.RepoScope) (map[string]time.Time, error)
	DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]int, error)
	AllOutcomesWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.Outcome, error)
	OutcomeTokenTotals(ctx context.Context, outcomes []store.Outcome, scope store.RepoScope) (map[store.DevIssue]int64, error)
	ActualSpendAllWindow(ctx context.Context, since, until time.Time) (map[string]float64, error)
	ActualSpendByPeriodWindow(ctx context.Context, since, until time.Time) ([]store.PeriodSpend, error)
	UnqualifiedExclusionWindow(ctx context.Context, since, until time.Time) (store.UnqualifiedExclusion, error)
	HierarchyMembership(ctx context.Context) ([]store.MembershipRow, error)
	DeveloperAliases(ctx context.Context) (map[string]string, error)
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
func (h *Handler) loadWindow(ctx context.Context, r windowReader, since, until time.Time, scope store.RepoScope) (windowScores, error) {
	costs, err := r.DeveloperCostsWindow(ctx, since, until, scope)
	if err != nil {
		return windowScores{}, fmt.Errorf("query costs: %w", err)
	}
	outcomes, err := r.AllOutcomesWindow(ctx, since, until, scope)
	if err != nil {
		return windowScores{}, fmt.Errorf("query outcomes: %w", err)
	}
	// Zero-token tripwire (#136): windowed token totals per (developer, issue), one
	// bulk query (no per-outcome N+1). Keyed by the raw token_events developer;
	// canonicalized below through the same alias map as the cost join.
	tokenTotals, err := r.OutcomeTokenTotals(ctx, outcomes, scope)
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
		actualSpend, err = r.ActualSpendAllWindow(ctx, since, until)
		if err != nil {
			return windowScores{}, fmt.Errorf("query actual_spend: %w", err)
		}
	}
	// Mixed-version signal (#293): the DISTINCT price_table versions that priced
	// token_events in this window. cost_micro is immutable per row (#233), so a
	// window legitimately spans versions while the response stamps a single active
	// price_table.version; this read surfaces the mix for the data-quality block.
	priceVersions, err := r.DistinctPriceVersionsWindow(ctx, since, until, scope)
	if err != nil {
		return windowScores{}, fmt.Errorf("query price versions: %w", err)
	}
	// Alias map (#125): resolve each raw identifier (OS username on cost rows,
	// GitHub login on outcome rows) to its canonical developer BEFORE joining. One
	// bulk fetch, no N+1 (matches the #94 TeamsForDevelopers pattern); canon() is an
	// O(1) hash lookup per row, so the join stays linear in the rows it scans.
	aliases, err := r.DeveloperAliases(ctx)
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
	issueCosts, err := r.DeveloperIssueCostsWindow(ctx, since, until, scope)
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
	scoredOutcomes := make([]scoring.Outcome, 0, len(outcomes))
	for _, o := range outcomes {
		dev := canon(o.Developer)
		key := store.DevIssue{Developer: dev, Repo: o.Repo, IssueID: o.IssueID}
		tokens := tokenIndex.Sum(dev, o.Repo, o.IssueID)
		if tokens > 0 {
			joinedOutcomes++
		}
		zeroToken := tokens < scoring.MinAttributableTokens
		so := scoring.Outcome{
			Developer: dev,
			IssueID:   o.IssueID,
			Repo:      o.Repo,
			Weight:    o.Weight,
			Quality:   o.Quality,
			ZeroToken: zeroToken,
		}
		scoredOutcomes = append(scoredOutcomes, so)
		byDev[dev] = append(byDev[dev], so)
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
	//
	// Iterated in SORTED RAW-KEY order, never map order (#722). Go randomizes map
	// iteration per range and float addition is not associative, so three aliases
	// of one developer summed in a random order give a canonical total that can
	// differ in the last ulp between two runs over identical rows — and that total
	// is ActualPaidUSD, hence SpendLeverage. Two aliases would be safe by
	// commutativity (a+b == b+a exactly); three is where it starts to bite, which is
	// why the guard fixture seeds three.
	rawSpendDevs := make([]string, 0, len(actualSpend))
	for dev := range actualSpend {
		rawSpendDevs = append(rawSpendDevs, dev)
	}
	sort.Strings(rawSpendDevs)
	canonSpend := map[string]float64{}
	for _, dev := range rawSpendDevs {
		canonSpend[canon(dev)] += actualSpend[dev]
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
		scoredOutcomes:    scoredOutcomes,
		since:             since,
		until:             until,
		scope:             scope,
	}, nil
}

// dataQualityBlock assembles the zero-token + mixed-price portion of the
// data_quality wire block for one window under the active aggregation mode (#136,
// #293). In an anonymized mode (team #185, division #270) the per-(developer,
// issue) zero-token list is suppressed — it names individuals — and only the
// name-free count is reported; in developer mode the sorted named list is emitted.
// The mixed-version signal attaches in developer mode only (#864). An anonymised
// window with spend carries attribution_coverage "not_shown". Returns nil when
// there is nothing to report, so a clean window omits these fields entirely. It is the single source of the mode-dependent suppression
// rule, shared by /scores (which then augments the returned block with the #351
// coverage shares) and by the per-window blocks of /scores/compare (#277). Sorts a
// COPY of the flagged slice so the caller's slice ordering is never mutated.
func dataQualityBlock(mode scoring.AggregationMode, zeroTokenOutcomes []zeroTokenOutcomeJSON, priceVersions []int, hasSpend bool) *dataQualityJSON {
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
	if len(priceVersions) > 1 && !mode.Anonymized() {
		if dq == nil {
			dq = &dataQualityJSON{}
		}
		dq.MixedPriceVersions = priceVersions
	}
	if mode.Anonymized() {
		if dq == nil {
			dq = &dataQualityJSON{}
		}
		dq.ExcludesUnattributedSpend = true
		if hasSpend {
			dq.AttributionCoverage = "not_shown"
		}
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
	if h.sealedRead() {
		h.serveSealedScores(w, r)
		return
	}
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
	// #864: an anonymised install publishes ONE breakdown per window (its group
	// rows and the grand total), so a work-type view is refused, not narrowed.
	if workTypeFilter != "" && h.aggregation.Anonymized() {
		writeError(w, http.StatusBadRequest,
			"work_type is not available in "+h.aggregation.String()+"-aggregation mode (#864): an anonymised "+
				"install publishes one breakdown per window, its "+h.aggregation.String()+" rows and the grand "+
				"total, because a second breakdown differences against the first to a group below the "+
				"k-anonymity floor. Remove ?work_type=")
		return
	}

	// ?repo filter (#590), validated at the same trust boundary and for the same
	// reason as work_type: a bad value is a loud 400, never a silently different
	// result set.
	scope, ok := parseRepoScope(w, r)
	if !ok {
		return
	}
	if !h.allowRepoScope(w, scope) {
		return
	}
	q := scoresQuery{
		since: since, until: until, scope: scope, workType: workTypeFilter, team: r.URL.Query().Get("team"),
		reportUnjoined: true,
	}
	var data scoresWindowData
	err = h.store.ReadSnapshot(r.Context(), func(snap *store.Snapshot) error {
		var err error
		data, err = h.loadScoresWindow(r.Context(), snap, q)
		return err
	})
	if err != nil {
		h.logger.Error("scores snapshot", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	// ReadSnapshot has rolled back and returned its connection before any
	// bootstrap computation or response assembly begins.
	resp, _ := h.buildScoresResponse(data, q, false)
	writeJSON(w, http.StatusOK, resp)
}

// scoresQuery is one /scores read after its parameters are parsed and validated.
type scoresQuery struct {
	since, until   time.Time
	scope          store.RepoScope
	workType, team string
	// reportUnjoined sets tier_identity_unjoined and WARNs each unjoined
	// identity (#125): a live read and a seal do, a verifier's recompute never.
	reportUnjoined bool
}

// scoresForWindow computes the /scores body for one validated query; every
// error is a store failure, already logged. With folded set, an anonymised
// body's rows come from scoring.PreFold + scoring.AggregateFolded over the same
// groups and census the live fold reads, and the pre-fold inputs are returned
// keyed by canonical id; the live path keeps AggregateLabeledKAnon, whose
// residual floats and Ranked flag AggregateFolded does not reproduce.
func (h *Handler) scoresForWindow(ctx context.Context, r windowReader, q scoresQuery, folded bool) (scoresResponse, []scoring.LabelInput, error) {
	data, err := h.loadScoresWindow(ctx, r, q)
	if err != nil {
		return scoresResponse{}, nil, err
	}
	resp, inputs := h.buildScoresResponse(data, q, folded)
	return resp, inputs, nil
}

// scoresWindowData owns all inputs needed to assemble a scores response. It
// retains no reader: live requests can release their snapshot after loading it.
type scoresWindowData struct {
	win                 windowScores
	costComposition     store.CostComposition
	unattributedBuckets []store.UnattributedBucketCost
	groups, teamGroups  windowGroups
	census              *kanonCensus
	horizon             time.Time
	hasHorizon          bool
	perSource           map[string]time.Time
	disclosure          scopeDisclosure
}

func (h *Handler) loadScoresWindow(ctx context.Context, r windowReader, q scoresQuery) (scoresWindowData, error) {
	since, until, scope := q.since, q.until, q.scope
	// Load + canonicalize + join the window into per-developer scores. This is the
	// shared scores path (#277): loadWindow does exactly the store reads, alias
	// canonicalization, cost/outcome/token join and zero-token tripwire that both
	// /scores and /scores/compare need, so the two endpoints can never compute a
	// developer's score — or its k-anon inputs — differently.
	win, err := h.loadWindow(ctx, r, since, until, scope)
	if err != nil {
		h.logger.Error("load window", "err", err)
		return scoresWindowData{}, err
	}
	if h.aggregation.Anonymized() {
		win = withoutPseudoDevelopers(win)
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
	// 🔴 #864: the cost composition (by model, by token class, attributed vs
	// unattributed, the premium share) and the unattributed-bucket split each break
	// the window's cost down a second way, and any one category can be carried by
	// fewer than k people: a model one developer used publishes that developer's
	// spend. An anonymised install publishes ONE breakdown, so neither is read there,
	// and every figure derived from them below stays at its zero value and is omitted.
	var costComposition store.CostComposition
	var unattributedBuckets []store.UnattributedBucketCost
	if !h.aggregation.Anonymized() {
		costComposition, err = r.CostCompositionWindow(ctx, since, until, scope)
		if err != nil {
			h.logger.Error("query cost composition", "err", err)
			return scoresWindowData{}, err
		}
		// Unattributed bucket split (#refocus, Option B): per-(developer, bucket) spend
		// over the SAME window, folded below into the org-level labeled split and each
		// developer's exploratory-overhead share. Raw token_events.developer, canonicalized
		// through the same alias map as the score rows.
		unattributedBuckets, err = r.UnattributedBucketCostsWindow(ctx, since, until, scope)
		if err != nil {
			h.logger.Error("query unattributed buckets", "err", err)
			return scoresWindowData{}, err
		}
	}
	data := scoresWindowData{win: win, costComposition: costComposition, unattributedBuckets: unattributedBuckets}
	if h.aggregation.Anonymized() {
		label, err := h.groupLabelFunc()
		if err != nil {
			h.logger.Error("group label", "mode", h.aggregation.String(), "err", err)
			return scoresWindowData{}, err
		}
		data.groups, err = h.groupWindow(ctx, r, win, label)
		if err != nil {
			h.logger.Error("group window by membership", "mode", h.aggregation.String(), "err", err)
			return scoresWindowData{}, err
		}
		// #856: the ONE counting rule for this window.
		c, err := h.kanonCensusFor(ctx, r, win, data.groups.roster)
		if err != nil {
			h.logger.Error("k-anonymity census", "err", err)
			return scoresWindowData{}, err
		}
		data.census = &c
	}
	// Cost horizon (#512), attached in BOTH modes. A horizon lookup failure is
	// logged and the signal omitted rather than 500-ing the whole response — the
	// scores are still correct, they are merely unannotated — but it must NOT be
	// silently swallowed, because a permanently-absent signal would read as
	// "window is covered" to any consumer that treats missing as clean.
	data.horizon, data.hasHorizon, err = r.CostCoverageStart(ctx, scope)
	if err != nil {
		h.logger.Error("query cost coverage start", "err", err)
		data.hasHorizon = false
	} else {
		data.perSource, err = r.SourceCoverageStart(ctx, scope)
		if err != nil {
			// Degrade to the global horizon rather than dropping the whole signal.
			h.logger.Error("query per-source coverage start", "err", err)
			data.perSource = nil
		}
	}
	data.disclosure, err = h.buildScopeDisclosure(ctx, r, since, until, scope)
	if err != nil {
		h.logger.Error("build scope disclosure", "err", err)
		return scoresWindowData{}, err
	}
	if !h.aggregation.Anonymized() && (q.team != "" || len(win.devScores) > 0) {
		var err error
		if data.teamGroups, err = h.groupWindow(ctx, r, win, teamLabel); err != nil {
			h.logger.Error("group window by team", "err", err)
			return scoresWindowData{}, err
		}
	}
	return data, nil
}

// buildScoresResponse does no store work. The live caller ends its snapshot
// before entering this phase; sealed callers may keep their wider seal snapshot.
func (h *Handler) buildScoresResponse(data scoresWindowData, q scoresQuery, folded bool) (scoresResponse, []scoring.LabelInput) {
	if h.beforeWindowAssembly != nil {
		h.beforeWindowAssembly()
	}
	since, scope, workTypeFilter := q.since, q.scope, q.workType
	win, costComposition, unattributedBuckets := data.win, data.costComposition, data.unattributedBuckets
	issueCosts := win.issueCosts
	var inputs []scoring.LabelInput
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
			if q.reportUnjoined {
				h.warnUnjoined(s.Developer, "cost")
			}
		case hasOutcome && !hasCost:
			unjoinedOutcome++
			outcomeOnlyDevs = append(outcomeOnlyDevs, s.Developer)
			if q.reportUnjoined {
				h.warnUnjoined(s.Developer, "outcome")
			}
		}
	}
	if h.identityGauge != nil && q.reportUnjoined {
		h.identityGauge.Set(float64(unjoinedCost), "cost")
		h.identityGauge.Set(float64(unjoinedOutcome), "outcome")
	}

	// since is UTC-anchored (sinceUTC above, #180), so this echoed date is the
	// UTC calendar day — on a negative-offset host it may read one day earlier
	// than the operator's local "90 days ago", which is intended, not a shift.
	// ✅ Since #746 the echo is EXACT rather than approximate: the default bound
	// is snapped to the start of its UTC day and every explicit bound already
	// parses to midnight, so the day printed here is the day the window really
	// opens on. It previously printed "2026-05-31" for a window that opened at
	// 2026-05-31T05:00:58Z — a lossy truncation in the primary response body, and
	// the reason /report_manifest's honest instant could not be replayed.
	// #593: set when a sub-k residual cohort was withheld, which forces every
	// unfloored aggregate over the same population to be withheld too (see the
	// assignment site). Zero value = nothing suppressed, so developer mode — which
	// never aggregates — is untouched.
	var kanonSuppressed scoring.KAnonSuppression
	// census is the window's k-anonymity counting rule (#856), set only in an
	// anonymized mode, where the pooled fold is its one reader (#864).
	census := data.census
	activePrices := store.ActivePriceTableInfo()
	resp := scoresResponse{
		Since:      since.Format("2006-01-02"),
		PriceTable: priceTableStamp(activePrices),
		Rubric:     rubricJSON{Version: scoring.RubricVersion},
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
		//
		// Rows are grouped by DATED membership (#886): each cost event and outcome
		// counts toward the group its developer was in at that event's time, so a
		// later move cannot change this window's rows.
		resp.Aggregation = h.aggregation.String()
		resp.Developers = []developerScoreJSON{}
		var teams []scoring.TeamScore
		var sup scoring.KAnonSuppression
		if folded {
			inputs = scoring.PreFold(data.groups.rows, census.scoring(), func(id string) string { return id })
			teams, sup = scoring.AggregateFolded(inputs, h.kAnonymity)
		} else {
			teams, sup = scoring.AggregateLabeledKAnon(data.groups.rows, h.kAnonymity, census.scoring())
		}
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
	resp.DataQuality = dataQualityBlock(h.aggregation, win.zeroTokenOutcomes, win.priceVersions, win.hasSpend())

	resp.DataQuality = withCostHorizon(resp.DataQuality, since, data.horizon, data.hasHorizon, data.perSource)

	// A sealed body (#913) carries attribution_coverage "not_shown" even for a
	// month with no spend (#956 ruling A: an anonymised mode always reports it),
	// and no cost_coverage_safe_since: that remedy is a ?since=, which a sealed
	// read refuses.
	if folded {
		if resp.DataQuality == nil {
			resp.DataQuality = &dataQualityJSON{}
		}
		resp.DataQuality.AttributionCoverage = "not_shown"
		resp.DataQuality.CostCoverageSafeSince = ""
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
			KAnonymity:    kanonSuppressed.K,
			WithheldTotal: true,
			WithheldTeams: true,
		}
	}

	// #856: once per window, org-wide. Built from the unfiltered window before any
	// ?work_type= narrowing, so no filter can move it.
	if census != nil && census.uncountedActive.any() {
		u := census.uncountedActive
		attachDataQuality().UncountedActiveIDs = &u
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
	if !scope.IsFleetWide() {
		dq := attachDataQuality()
		dq.RepoScope = data.disclosure.Repo
		dq.RepoScopeExcluded = data.disclosure.Excluded
		dq.SpendLeverageSuppressed = data.disclosure.Suppressed
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

	// Developer-mode team roll-ups (#821) and the optional ?team= filter share ONE
	// dated-membership grouping of the window (#886), never a query per developer
	// or per team (#94 item 3): each event counts toward the team its developer was
	// in at that event's time, exactly as in the anonymized modes, so a move
	// changes no past window here either. Neither runs in ANY anonymized mode
	// (team #185, division #270): each rolls up named teams with no k-floor, so it
	// would re-expose a sub-k cohort's aggregate and bypass the anonymity set that
	// the k-anonymized resp.Teams enforces. k-anon suppression is only ever set in
	// anonymized mode, so it never meets team_rollups.
	teamFilter := q.team
	teamGroups := data.teamGroups
	if !h.aggregation.Anonymized() && resp.Total != nil {
		resp.TeamRollups = buildTeamRollups(teamGroups.rows)
	}
	if teamFilter != "" && !h.aggregation.Anonymized() {
		var teamDevs []scoring.DeveloperScore
		for _, row := range teamGroups.rows {
			if row.Label == teamFilter {
				teamDevs = append(teamDevs, row.Score)
			}
		}
		if len(teamDevs) > 0 {
			ts := newTeamScoreJSON(scoring.RollupTeam(teamFilter, teamDevs))
			resp.Team = &ts
		}
	}

	// Work-type segmentation (#187): the type-scoped view the dashboard renders and
	// the surface for WITHIN-category comparison. Built from the same canonicalized
	// outcomes + token totals as the pooled view above, but partitioned by work_type
	// and denominated by per-(developer, issue) cost. workTypeFilter (validated above)
	// restricts the output to one segment; empty emits every type present.
	//
	// 🔴 DEVELOPER MODE ONLY (#864). In an anonymised mode the segments are a second
	// breakdown of the window beside the group rows, and the two difference to a group
	// below k with NOTHING suppressed: pooled rows minus work-type totals recovered a
	// 3-person group's $9.21 while every published row had >= 5 people
	// (TestOneBreakdown_E2PooledRowsAndWorkTypeTotalsDoNotDifference). #937 is the
	// route back.
	var segments []workTypeSegmentJSON
	if !h.aggregation.Anonymized() {
		segments = h.buildWorkTypeSegments(win.outcomes, win.canon, win.canonTokens, issueCosts, workTypeFilter)
		// Segment reconciliation (#466): account for EVERY dollar of the window against
		// the segments just built, so the spend they structurally cannot categorize is
		// reported instead of silently dropped. Built from win.outcomes — the UNFILTERED
		// list — on purpose: ?work_type must not turn another category's spend into
		// "no outcome". See segmentReconciliationJSON. It reconciles the segments, so it
		// is built where they are.
		resp.SegmentReconciliation = buildSegmentReconciliation(
			win.outcomes, win.canon, issueCosts, h.logger)
	}

	// 🔴 #593 SECOND PASS — STRIP EVERY WINDOW-AGGREGATE FIGURE WHEN SUPPRESSED.
	//
	// This runs LAST, after every data_quality attach site, and that ordering is the
	// design. A per-site guard is one forgotten `if` away from re-opening the leak, and
	// a future field added above would default to LEAKING. Here the default is safe.
	// kanonSuppressed is set only by the anonymised pooled fold, which attached the
	// declaration above (#864: the segments that could also set it are no longer
	// built in an anonymised mode).
	//
	// Review measured the leak this closes, and it defeated the suppression completely:
	// with `total` and `cost_composition` withheld, the response still shipped
	//
	//   unattributed_buckets: [{cost_usd: 5.00, share: 0.3333}]
	//   attributed_cost_share: 0.6667
	//
	// and cost_usd / share == 15.00 — the withheld composition total, exactly, from a
	// single bucket. Since #864 kanonSuppressed ⇒ anonymised, where team_rollups,
	// cost_composition, segment_reconciliation and the buckets are never built, so
	// only the rows, the total and the floored attributed_cost_share are stripped.
	//
	// What stays, and why it is not the same class:
	//   - cost_coverage_* / source_coverage_start: properties of the INSTALLATION (when
	//     capture began), not aggregates over the window's population.
	//   - kanon_suppressed: the declaration itself, which is the point.
	if kanonSuppressed.Any() {
		// 🔴 #864: the WHOLE response goes, the named rows included. Named rows
		// published beside a withheld residual let another view whose residual folds
		// a named group in (/compare folds a group sub-k in its other window)
		// difference to the withheld cohort.
		resp.Teams = nil
		resp.Total = nil
		if dq := resp.DataQuality; dq != nil {
			dq.AttributedCostShare = nil
		}

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

	return resp, inputs
}

// buildWorkTypeSegments partitions the window's outcomes by work_type and computes
// a self-contained score per category (#187). Each segment denominates TIER on cost
// at (developer, issue) grain — a token event's cost is charged to the work_type of
// the outcome(s) sharing its (developer, issue) — so a security engineer's security
// TIER divides their security points by the cost of their security issues, never by
// their whole-window cost. Cross-type comparison is a category error and is not
// offered: there is no combined leaderboard, only these per-type groupings.
//
// Developer mode only: an anonymised mode publishes no segments (#864, see the
// call site), so every row here is a named developer row and #133 ranking floors
// apply per segment.
//
// filter, when non-empty (already validated by the caller), restricts the RESULT to
// that single type; a type with no outcomes yields a segment with no rows so a
// filtered request still gets a well-formed (empty) answer.
func (h *Handler) buildWorkTypeSegments(
	outcomes []store.Outcome,
	canon func(string) string,
	canonTokens map[store.DevIssue]int64,
	issueCosts []store.DevIssueCost,
	filter string,
) []workTypeSegmentJSON {
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

	// A filter naming a type with no outcomes still yields a well-formed empty segment,
	// so a filtered request always gets a shaped answer.
	types := make([]string, 0, len(segByDev)+1)
	for wt := range segByDev {
		if filter == "" || wt == filter {
			types = append(types, wt)
		}
	}
	if filter != "" && segByDev[filter] == nil {
		types = append(types, filter)
	}
	sort.Strings(types)

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
		if len(devScores) > 0 {
			total := newTeamScoreJSON(scoring.RollupTeam("", devScores))
			seg.Total = &total
		}
		segments = append(segments, seg)
	}
	return segments
}

// addSaturating returns a+b clamped to the int64 range instead of wrapping, and
// REPORTS whether it had to clamp (#466).
//
// The reconciliation sums cost_micro across every row in a window. Each row is bounded
// at ingest (store.MaxTokenEventCostMicro), but the SUM is not: enough max-value writes overflow
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
		// Sorted raw-key order, never map order (#722) — the same defect and the same
		// fix as the fleet path's canonSpend rebuild in loadWindow. The two paths
		// must agree bit-for-bit: /scores and /scores/{developer} publish the same
		// developer's actual_paid_usd, and a reader comparing them would otherwise
		// see two numbers that disagree in the last ulp for no reason in the data.
		spendDevs := make([]string, 0, len(spendAll))
		for dev := range spendAll {
			spendDevs = append(spendDevs, dev)
		}
		sort.Strings(spendDevs)
		for _, dev := range spendDevs {
			if canon(dev) == target {
				actualPaid += spendAll[dev]
			}
		}
	}

	// Keep the same outcome population as /scores when choosing the freshest
	// window for a reused (repo, issue). Filtering to this developer first would
	// let their older tokens clear the detail flag while the list flags it.
	tokenTotals, err := h.store.OutcomeTokenTotals(r.Context(), outcomes, scope)
	if err != nil {
		// target is the same client-controlled path value; sanitize via logsafe (#321).
		h.logger.Error("query token totals for developer", "developer", logSafeStr(target), "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	// Narrow the displayed and scored outcomes to the target.
	var targetOutcomes []store.Outcome
	for _, o := range outcomes {
		if canon(o.Developer) == target {
			targetOutcomes = append(targetOutcomes, o)
		}
	}

	// Windowed token totals for the tripwire (#136), re-keyed by canonical
	// identity so aliased tokens (#125) count toward the target.
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
	disc, err := h.buildScopeDisclosure(r.Context(), h.store, since, until, scope)
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
// mirrors handlePostCosts (MaxBytesReader, DisallowUnknownFields, requireJSONEOF
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
	if requireJSONEOF(dec) != nil {
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
	if err := h.store.UpsertDeveloperAlias(r.Context(), req.Alias, req.Canonical, h.writerFingerprint(r)); err != nil {
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
		if errors.Is(err, store.ErrMembershipClockBehind) {
			writeMembershipClockBehind(w)
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
	found, err := h.store.DeleteDeveloperAlias(r.Context(), alias, h.writerFingerprint(r))
	if err != nil {
		if errors.Is(err, store.ErrWriteLockUnavailable) {
			h.logger.Warn("delete developer_alias: write lock unavailable", "err", logSafeErr(err))
			writeStoreContention(w)
			return
		}
		if errors.Is(err, store.ErrMembershipClockBehind) {
			writeMembershipClockBehind(w)
			return
		}
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
	watcherSnap, ok := subsystems["watcher"].Detail.(health.WatcherSnapshot)
	// This route is outside auth: both watcher blocks expose only error
	// classes, even when a token is supplied.
	if watcherSnap.LastError != "" {
		watcherSnap.LastError = "watch_failed"
	}
	if watcherSnap.LastWatchAddError != "" {
		watcherSnap.LastWatchAddError = "watch_add_failed"
	}
	if ok {
		sub := subsystems["watcher"]
		sub.Detail = watcherSnap
		subsystems["watcher"] = sub
	}
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
//
// Commit was added by #638. It is ADDITIVE — every existing field keeps its name
// and type, so a probe parsing this today is unaffected. It is here as well as on
// /version because /livez is the endpoint operators already have wired, and the
// version string ALONE does not identify a build: a tagged release reports
// "0.4.0" whether it was built from the tag or rebuilt from a moved tag, so two
// different binaries are indistinguishable by `version`. The commit is what makes
// "this deployment is the build I think it is" an assertion rather than a hope.
type livezResponse struct {
	Status  string `json:"status"`
	UptimeS int64  `json:"uptime_s"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
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
		Commit:  h.buildIdentity().Commit,
	})
}

// --- GET /api/v1/version ---

// vcsInfo is what the Go toolchain stamped into this binary. Stamped
// distinguishes "the toolchain recorded nothing" from "it recorded a clean
// tree" — a distinction that MATTERS and that an earlier draft of this code got
// wrong: it reported `modified: false` unconditionally, so a binary with no
// stamps at all made a confident, unfounded claim that its tree was clean.
type vcsInfo struct {
	Stamped   bool
	Commit    string
	Modified  bool
	GoVersion string
}

// vcsStamps reads the build stamps ONCE per process. They are immutable for the
// lifetime of the binary, and debug.ReadBuildInfo is NOT a cached accessor — it
// re-parses the embedded modinfo and allocates a fresh BuildInfo, a Module per
// dependency and a BuildSetting per setting on every call. Measured on this
// binary's modinfo: ~1291 ns and 2776 B per call, versus ~1.6 ns and zero
// allocations behind OnceValue. /livez is a liveness probe that can be scraped
// every couple of seconds, so recomputing a process constant there is the exact
// thing sync.OnceValue exists to prevent.
var vcsStamps = sync.OnceValue(readVCSStamps)

func readVCSStamps() vcsInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return vcsInfo{}
	}
	out := vcsInfo{GoVersion: info.GoVersion}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs":
			// Present iff the toolchain stamped VCS data at all. This is the
			// discriminator: vcs.modified is emitted unconditionally when
			// stamping happens, while vcs.revision is conditional, so neither
			// one alone reliably answers "did stamping occur?".
			out.Stamped = s.Value != ""
		case "vcs.revision":
			out.Commit = s.Value
		case "vcs.modified":
			out.Modified = s.Value == "true"
		}
	}
	return out
}

// versionResponse is the full build identity of a running tierd (#638).
//
// 🔴 Why `version` alone was not enough. On 2026-08-01 a stale `tierd demo` from
// six days earlier answered every probe and its 200s were read as evidence about
// current main (false-green ledger 26). A tagged release reports the same
// `version` string however it was built, so two binaries from a moved tag are
// indistinguishable by it. Commit is what cannot agree by coincidence.
//
// ⚠️ Do NOT reinstate price_table.version as the discriminator: it bumps only
// when PRICES change. Measured 2026-08-06, the deployed dev demo reported price
// table v9 while main was also v9 — and the deployed binary was 0.3.0 against a
// published 0.4.0. The discriminator agreed while the artifact was a full release
// behind. PriceTable is here because it answers "which rates priced these
// numbers", NEVER as identity.
type versionResponse struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	// Modified is a POINTER so that "unknown" and "clean" are different states on
	// the wire. Absent = the toolchain stamped nothing (a container built without
	// .git, which is how this repo's image is built). false = stamped and clean.
	// A plain bool here published `modified: false` from binaries that had no idea
	// — a false attestation, which is strictly worse than saying nothing.
	Modified  *bool  `json:"modified,omitempty"`
	GoVersion string `json:"go_version,omitempty"`
	Platform  string `json:"platform"`
	// 🔴 versionPriceTableJSON, NOT priceTableJSON: no content digests on this
	// unauthenticated endpoint (ruled 2026-08-28, #713). See priceTableJSON.
	PriceTable versionPriceTableJSON `json:"price_table"`
}

// buildIdentity assembles the build facts for this handler.
//
// Commit precedence is INJECTED-then-stamped, and the order is load-bearing:
// this repo's Dockerfile builds with .git excluded via .dockerignore, so the
// shipped container has NO VCS stamps at all. Measured on the published v0.4.0
// image: zero vcs settings in the binary, while the release TARBALL from the same
// run carries vcs.revision=ca27d9f0…. The container therefore relies on the
// ldflags-injected value, which is exactly the deployment #638 was filed about.
func (h *Handler) buildIdentity() versionResponse {
	v := vcsStamps()
	if h.vcsOverride != nil {
		v = *h.vcsOverride
	}
	active := store.ActivePriceTableInfo()
	resp := versionResponse{
		Version:   h.version,
		Commit:    h.buildCommit,
		GoVersion: v.GoVersion,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		// Version + effective_date ONLY. The #713 content digests are gated off
		// this unauthenticated endpoint by ruling; the type makes it enforced
		// rather than remembered. See priceTableJSON's comment for the reasoning
		// and for why a third party verifying a report does not need them here.
		PriceTable: versionPriceTableJSON{
			Version:       active.Version,
			EffectiveDate: active.EffectiveDate,
		},
	}
	if resp.Commit == "" {
		resp.Commit = v.Commit
	}
	if v.Stamped {
		modified := v.Modified
		resp.Modified = &modified
	}
	return resp
}

// handleVersion reports build identity. UNAUTHENTICATED and mounted in read-only
// mode deliberately: the whole point is that an operator can identify a running
// deployment, and a public demo is exactly the deployment hardest to identify by
// other means. It discloses no spend data.
//
// ⚠️ Scope of that claim, stated precisely because an earlier draft overstated it:
// for a MIRROR-built artifact everything here is already public (the release
// binaries and image carry the same stamps). For a binary built from the PRIVATE
// tree and deployed, `commit` is a private-repo revision. That SHA is opaque —
// GitHub will not serve a commit, tree or blob from a private repo to an
// anonymous fetch-by-SHA — so it leaks no code; what it reveals, sampled over
// time, is deploy cadence. Judged acceptable: `version` was already open on
// /livez before this endpoint existed, so the fingerprint is not new.
func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.buildIdentity())
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
// a retryable 503 is the honest answer and a five-second stall is not.
//
// ⚠️ "behind the single connection" stood here until #669 raised the store's pool
// off 1. Do not restore it, and do not read its removal as the stall being gone:
// a blocked promote still holds its pool slot for the whole wait, so the
// process-wide version returns once maxOpenConns promotes block at once
// (measured: 4.75s for an unrelated read at 4 concurrent blockers, 393µs at 3).
// The threshold moved; the failure did not.
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
		//
		// 🔴 THE SNAP IS #746, AND IT REMOVES A LOSSY TRUNCATION RATHER THAN
		// ADDING ONE. Without it the default bound carries the current TIME OF
		// DAY, while /scores has always ECHOED it as a bare calendar day
		// (`since.Format("2006-01-02")`, documented in docs/api-compatibility.md
		// as "echoed window lower bound (UTC calendar day)") — so a response
		// saying "2026-05-31" actually bounded the window at 2026-05-31T05:00:58Z.
		// The report manifest publishes the honest instant, and `tierd
		// verify-report` then REFUSES it (rc 2, parseManifestBound) because
		// /scores accepts only whole-day bounds: the DEFAULT report shape was the
		// one shape that could not be re-run. Snapping makes the echoed day true
		// and the default manifest replayable.
		//
		// 🔴 .UTC() BEFORE AddDate, AND THE ORDER IS THE WHOLE #180 QUESTION AGAIN
		// — the snap is what turned a harmless skew into a load-bearing one.
		// AddDate is a WALL-CLOCK operation: on a local time it subtracts 90
		// entries from the HOST'S calendar, which is 90×24h ± the DST offset that
		// moved in between. Before the snap that hour of slop just shifted a
		// mid-day bound by an hour and nobody could see it. After the snap it
		// decides which side of a midnight the bound lands on, so an hour becomes
		// a WHOLE DAY of window. Measured over 2026 at 10-minute resolution,
		// local-first vs UTC-first pick DIFFERENT UTC days for 1086/52560 samples
		// — 2.07% of the year — on America/New_York, Europe/London and
		// Australia/Sydney alike, clustered at UTC hours 23 and 00, and in
		// OPPOSITE directions north and south. UTC and Asia/Kolkata (fixed
		// +05:30) measure 0.00%, which is what identifies the cause as DST rather
		// than the offset. Doing the arithmetic in UTC first makes the bound the
		// same instant on every host, which is the property #180 is about.
		//
		// ⛔ Truncate, NOT time.Date(y, m, d, 0, 0, 0, 0, t.Location()). Truncate
		// works on the absolute instant, so a 24h multiple always lands on UTC
		// midnight; time.Date rebuilds from the receiver's WALL CLOCK, which on a
		// non-UTC receiver is LOCAL midnight — a bound with an offset, and #180
		// yet again. ⚠️ Be honest about what this guard is: with .UTC() applied
		// first the receiver's Location IS UTC, so time.Date would be equivalent
		// TODAY. It is written down because it stops being equivalent the moment
		// somebody moves the .UTC(), and that reorder is exactly the edit the
		// paragraph above shows is easy to make and hard to see.
		//
		// ⛔ Snap BACKWARD only. Widening a window can only ever ADD spend that is
		// really there; rounding forward would silently drop a partial day of
		// cost out of a cost metric, which is the one direction a spend number
		// must never move on its own.
		//
		// 🔑 It lives HERE, in the one shared definition, rather than at the six
		// call sites. That makes "the manifest snapped but the scores did not" —
		// two surfaces bounding different windows while publishing one identity —
		// UNREPRESENTABLE, not merely guarded. The six call sites are
		// handleGetScores, handleGetDeveloperScore, handleGetReportManifest,
		// handleGetScoresCompare, handleGetOrgActualSpend and parseExportParams;
		// ⚠️ the last is ONE call site serving FOUR routes (/events, /outcomes,
		// /quality_events, /quality_history), so the endpoint count is NINE, not
		// six — do not write "/export", there is no such route (the one route
		// with `export` in its name, /developer/{id}/export, does not call this).
		// EXPLICIT bounds are untouched: parseWindowDate's layouts are zoneless
		// and already land on midnight.
		return defaultSince(time.Now()), nil
	}
	return parseWindowDate(s)
}

// defaultSince resolves the window lower bound used when `?since=` is omitted:
// the start of the UTC day 90 days before `now` (#746). It is a pure function of
// its argument SO THAT THE PROPERTY ABOVE CAN BE TESTED DETERMINISTICALLY.
//
// 🔴 THAT IS THE ENTIRE REASON IT IS NOT INLINED. The hazard this expression
// guards against — doing AddDate on a local wall clock — produces a bound one
// whole UTC day off, but only for about 2% of the year on a DST host and only
// near UTC midnight. Reached through time.Now(), a test for it is a coin flip
// that comes up wrong twice a hundred times and blames whatever changed last.
// Taking `now` as a parameter turns "is this correct on a DST host at 23:40Z?"
// into a table row. See TestDefaultSince_IsIndependentOfHostZone, which sweeps
// real DST transitions in both hemispheres and carries a positive control
// proving the hazard is real rather than hypothetical.
//
// Callers pass time.Now(); the Location it carries is deliberately irrelevant.
func defaultSince(now time.Time) time.Time {
	return now.UTC().AddDate(0, 0, -90).Truncate(24 * time.Hour)
}

// parseWindowDate parses a window-bound query value using the accepted date
// layouts, each yielding a UTC instant at the START of the named period (day,
// month, or year). It is the shared grammar for both ends of a score window so
// `since` and `until` parse identically (#276) — the only difference is what an
// EMPTY value means, which each caller decides (since defaults to defaultSince,
// the start of the UTC day 90 days back;
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

// allowRepoScope reports whether a caller-supplied ?repo= scope may be honored in
// this server's aggregation mode, writing the refusal itself when it may not.
//
// 🔴 REFUSED IN ANY ANONYMIZED MODE (#185 team, #270 division). This is the same
// carve-out ?team= gets, for the same reason, and it is not theoretical — it was
// REPRODUCED during review before this guard existed.
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
// change exists to close. (?team= merely SKIPS its branch, which is fail-safe there
// because the k-anonymized resp.Teams is still what ships; here the honest answer is
// that the question cannot be asked in this mode.)
//
// ⚠️ CORRECTED (#715). This comment used to end: "the unfloored residual bucket
// and unfloored `total` are a PRE-EXISTING hole, reachable today by narrowing
// ?since= alone … filed as #593". That was true when written and is now FALSE.
// #593 is CLOSED and its fix is the strip pass in handleGetScores, whose
// reproduction is a live test (TestKAnonResidual_TimeAxisReproduction): a sub-k
// window now withholds `total`, `cost_composition` and `segment_reconciliation`
// and declares `kanon_suppressed`.
//
// 🔴 THE STALENESS MATTERED IN THE DANGEROUS DIRECTION, WHICH IS WHY THE
// RETRACTION IS KEPT RATHER THAN THE LINE SIMPLY DELETED. A standing comment
// saying "the time axis is already conceded" is an argument for shipping the next
// unfloored window aggregate — and extracting it into a SHARED method doubled the
// number of call sites that would read it that way. The live rule is the opposite:
// the time axis IS floored, and any new window aggregate must join that strip pass
// or be withheld. `GET /report_manifest` withholds (see manifestWatermarksJSON).
//
// 🔑 IT IS A SHARED METHOD RATHER THAN AN INLINE BLOCK BECAUSE IT NOW HAS TWO CALL
// SITES (#715). /scores and /report_manifest must refuse the identical set of
// requests: the manifest publishes per-repo ROW COUNTS over a caller-chosen window,
// and a count is precisely the quantity a k-anonymity floor exists to protect. A
// second hand-copied guard is a guard that drifts, and the half that drifts is the
// one nobody re-reads.
//
// ⚠️ This closes the REPO axis only. The TIME axis is closed elsewhere, by the
// strip pass named above — so a new endpoint that adopts this guard is NOT thereby
// safe on windows; it must handle the time axis too.
func (h *Handler) allowRepoScope(w http.ResponseWriter, scope store.RepoScope) bool {
	if scope.IsFleetWide() || !h.aggregation.Anonymized() {
		return true
	}
	writeError(w, http.StatusBadRequest,
		"repo scoping is not available in "+h.aggregation.String()+"-aggregation mode (#185, #270): "+
			"narrowing to one repository can shrink a cohort below the k-anonymity floor and expose "+
			"an individual's figures. Remove ?repo=")
	return false
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
func (h *Handler) buildScopeDisclosure(ctx context.Context, r windowReader, since, until time.Time, scope store.RepoScope) (scopeDisclosure, error) {
	if scope.IsFleetWide() {
		return scopeDisclosure{}, nil
	}
	ex, err := r.UnqualifiedExclusionWindow(ctx, since, until)
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
