// Package scoring implements the TIER formula and team rollup.
//
// TIER = Σ(outcome_weight × quality_multiplier) / (total_AI_cost_USD / 1000)
//
// A TIER of 100 means 100 weighted outcome points per $1,000 of AI compute.
package scoring

import (
	"fmt"
	"sort"
	"strings"
)

// Evidence floor (#133, review C3). A developer's TIER is only sound enough to
// compare once there is enough evidence behind it: without a floor, one 0.5-weight
// PR against $0.0004 of cost yields TIER ≈ 1,250,000. A row clears the floor
// (Ranked) only when BOTH thresholds are met and no outcome is flagged (#136);
// below-floor rows are still LISTED (never hidden), in the same identifier order
// as every other row (#830), and carry a below-floor tag instead of a bar.
const (
	// MinRankedOutcomes is the minimum number of outcomes (merged PRs / resolved
	// issues) a developer must have before their TIER is ranked. review C3.
	MinRankedOutcomes = 3
	// MinRankedCostUSD is the minimum list-price AI spend (USD) a developer must
	// have accrued before their TIER is ranked. review C3.
	MinRankedCostUSD = 5.00
	// MinAttributableTokens is the zero-token-outcome tripwire floor (#136,
	// review C6 / adversarial G-02). An outcome whose (developer, issue) recorded
	// fewer than this many tokens inside the attributable window (see the store's
	// AttributableWindow) is flagged and unranks its developer.
	//
	// Rationale for 1,000: a single trivial Claude Code exchange is ~2–10K tokens
	// (G-02 suggests a 10K/day audit floor), so 1,000 sits conservatively below
	// any real AI-assisted PR. A flag therefore means the outcome was effectively
	// produced off-books (work done on a personal subscription tierd never saw,
	// the flagship G-02 gaming vector) or attribution is broken (identity mismatch
	// B1, worktree drop A11) — both are exactly what a reviewer must see. False
	// positives from genuinely-manual PRs are the honest outcome: TIER is an
	// AI-yield metric, and an outcome with no measured AI input should not lend
	// its (astronomic) TIER any ranking authority.
	MinAttributableTokens = 1000
	// AttributableWindowDays is the length, in days, of the store's
	// AttributableWindow, the look-back that MinAttributableTokens is counted over.
	// scoring cannot import store, so the value is mirrored here for the report's
	// floor legend and pinned equal by store's TestAttributableWindowMatchesScoring.
	AttributableWindowDays = 14
)

// tierCostScaleUSD is the dollar scale in the TIER formula: TIER is weighted
// points per this many dollars of AI cost (points / (cost / tierCostScaleUSD)).
// It is named once here because cost_per_point's confidence interval is derived
// as its reciprocal (CostPerPointCI = tierCostScaleUSD / tier); a bare literal at
// each site would let the CI silently desync from the formula it inverts if the
// scale ever changed.
const tierCostScaleUSD = 1000.0

// Outcome is a single resolved issue or merged PR contribution.
type Outcome struct {
	Developer string
	IssueID   string
	// Repo is the canonical "owner/repo" this outcome belongs to (#231), carried so
	// the API layer can key per-(developer, repo, issue) cost when building the joint
	// bootstrap CI (#495). ComputeDeveloper itself never reads it — scoring is
	// repo-agnostic; it is provenance the CI derivation needs.
	Repo string
	// WorkType is the work category this outcome belongs to (#187): one of the
	// store.WorkType* values. It is carried here so the API layer can PARTITION a
	// developer's outcomes by category and score each category separately — a
	// security outcome and a feature outcome by the same developer land in different
	// segments. ComputeDeveloper itself is category-agnostic (it sums whatever
	// outcomes it is handed); the partitioning is the caller's job.
	WorkType string
	Weight   float64 // size label or git heuristic
	Quality  float64 // derived from quality events; floors: 1.0 clean, 0.7 ci-fail, 0.8 strategic revert, 0.1 quality revert (#134)
	// ZeroToken marks an outcome whose (developer, issue) recorded fewer than
	// MinAttributableTokens tokens in its attributable window (#136). It is
	// computed in the API layer (which owns the token totals) and fed in here so
	// the ranking rule stays in one package with the #133 floors. A single
	// zero-token outcome unranks the developer (see ComputeDeveloper); the
	// outcome still contributes its full weight×quality — visibility, not score
	// surgery.
	ZeroToken bool
}

// DeveloperScore holds a developer's computed TIER score and supporting data.
//
// CoveragePercent and SpendLeverage are the two CFO-facing sidecars to TIER:
//   - CoveragePercent is "capture fidelity (of captured spend)" — the fraction
//     of the spend we DID record that arrived per-request (realtime proxy/JSONL)
//     rather than as a coarse daily/estimated total. It measures the fidelity of
//     what was captured, NOT the completeness of capture: a developer whose only
//     recorded spend is a JSONL read reads 100% here while doing most of their
//     work off-books (ChatGPT-web/Copilot) that tierd never saw. Completeness is
//     surfaced separately by the zero-token tripwire (#136), not by this number.
//   - SpendLeverage answers "how much cheaper is our enterprise contract than
//     list price?" — TotalCostUSD ÷ ActualPaidUSD, where TotalCostUSD comes
//     from the Reference Price Table (list) and ActualPaidUSD comes from the
//     finance-supplied actual_spend ledger.
type DeveloperScore struct {
	Developer      string
	TIER           float64
	WeightedPoints float64 // Σ(weight × quality)
	TotalCostUSD   float64 // list-price cost from RPT
	ActualPaidUSD  float64 // actual invoice total for the period (from actual_spend)
	SpendLeverage  float64 // TotalCostUSD ÷ ActualPaidUSD; 0 when no actual_spend recorded
	// CostPerPoint is USD per weighted outcome point — TotalCostUSD ÷
	// WeightedPoints — the inverse-unit dual of TIER (numerically 1000/TIER when
	// both are defined) and the natural constant-dollar benchmarking / trend unit
	// (#239). A CFO reads "$X per weighted point" directly. It is the RIGHT unit for
	// self-over-time trends; cross-org comparability is NECESSARY-BUT-NOT-SUFFICIENT
	// on a matched RubricVersion + price_table version — the matched version stamps
	// catch rubric/price drift, but both parties must ALSO score against the shared
	// normative calibration (docs/rubric.md), which is what actually neutralizes a
	// generous-vs-strict labeling difference. The stamp alone does not. Guarded on WeightedPoints > 0:
	// a zero-point row leaves it at 0 (rendered "—"), mirroring how TIER and
	// SpendLeverage leave a zero-denominator ratio at 0. Note the guard is on the
	// POINTS denominator here, not cost — a $0-cost row that has points yields a
	// legitimate 0.0, and such a row is already surfaced by the zero-token
	// tripwire (#136), so the 0 never masquerades as peak efficiency in a ranking.
	CostPerPoint float64
	// CoveragePercent is capture fidelity: the % of CAPTURED spend recorded
	// per-request (realtime proxy/JSONL) vs. as a coarse daily/estimated total.
	// It says nothing about spend tierd never saw — completeness is the
	// zero-token tripwire's job (#136), not this field's. The JSON field name
	// stays `coverage_pct` (#136 keeps the API stable over a relabel), so the
	// human label ("Fidelity") and the wire name ("coverage_pct") deliberately
	// differ — do not "fix" the mismatch by renaming the JSON key.
	CoveragePercent float64

	// FlaggedOutcomes is the count of this developer's zero-token-flagged
	// outcomes (#136): outcomes whose (developer, issue) recorded fewer than
	// MinAttributableTokens tokens in the attributable window. Any non-zero
	// count forces Ranked=false (see ComputeDeveloper) — a developer with even
	// one off-books/unattributed outcome cannot hold ranking authority — while
	// the flagged outcomes keep their full points.
	FlaggedOutcomes int

	// SampleN is the number of outcomes behind this score (#133). It is the
	// evidence count the ranking floor gates on and the resample size the
	// bootstrap CI draws.
	SampleN int
	// CILow and CIHigh are the 95% percentile-bootstrap interval bounds for TIER
	// (#133). They are populated by the API layer via BootstrapCI (which needs a
	// PRNG and so is kept out of this deterministic function); both stay 0 for
	// unranked rows, where a confidence interval would be meaningless.
	CILow  float64
	CIHigh float64
	// Ranked reports whether this row is sound enough to rank. It requires BOTH
	// #133 ranking floors (SampleN >= MinRankedOutcomes AND TotalCostUSD >=
	// MinRankedCostUSD) AND zero flagged outcomes (FlaggedOutcomes == 0, #136):
	// a zero-token outcome is another reason to be unranked, extending the same
	// listed-not-hidden semantics. Unranked rows are listed in identifier order
	// like every other row (#830), tagged below the evidence floor.
	Ranked bool

	// CapturedRealtimeUSD and CapturedNonRealtimeUSD are the parts of this row's
	// realtime and non-realtime cost a collector captured (every source but a
	// manual /costs row). The k-anonymity census judges a share's carriers on
	// them, per row, so a group's seat is filled only by captured cost in that
	// group (#943).
	CapturedRealtimeUSD, CapturedNonRealtimeUSD float64
}

// TeamScore is the rollup of multiple developer scores into one team score.
type TeamScore struct {
	Team            string
	TIER            float64
	WeightedPoints  float64
	TotalCostUSD    float64
	ActualPaidUSD   float64
	SpendLeverage   float64
	CoveragePercent float64
	// CostPerPoint is the team's USD per weighted point — TotalCostUSD ÷
	// WeightedPoints on the summed team totals, the same inverse-unit as the
	// per-developer field (#239). Guarded on WeightedPoints > 0.
	CostPerPoint float64
	// Ranked reports whether this aggregate is sound enough to rank, on exactly
	// the developer rule (#133/#136) applied to the SUMMED inputs: total outcomes
	// >= MinRankedOutcomes AND TotalCostUSD >= MinRankedCostUSD AND zero flagged
	// outcomes across the members (#502). The #133 evidence floor was never
	// carried into the rollup, so a team that merged 28 points against $0.0001 of
	// measured spend published TIER 2.8e8 with full ranking authority.
	//
	// The gate reuses the developer constants deliberately — a second, team-only
	// floor would drift against the first, and the quantity being gated is the
	// same one: how much evidence stands behind THIS number.
	//
	// It is summed, not an AND over member Ranked flags: three developers with one
	// outcome and $2 each are individually below the floor, but the team number is
	// computed from their sums (3 outcomes, $6), and that is what the floor must
	// judge. TIER itself is untouched either way — the number is never altered,
	// only its ranking authority revoked (#136).
	Ranked     bool
	Developers []DeveloperScore
}

// NOTE ON WHAT IS DELIBERATELY *NOT* A FIELD HERE: the summed outcome count and
// flagged count that feed Ranked stay in RollupSums, which is never served. They
// are not withheld out of tidiness — publishing a team-level sample_n would hand
// the anonymized modes a second equation. `data_quality.attributed_outcome_share` is a ratio of
// outcome COUNTS that is safe to publish precisely because its denominator is
// published nowhere (see the k-anon strip block in internal/api/handler.go); a
// team sample_n would supply it. Keep the counts off TeamScore.

// ComputeDeveloper calculates the TIER score for a single developer.
//
// outcomes is the list of resolved issues/PRs for this developer.
// totalCostUSD is the sum of all AI costs attributed to this developer at list
// price (computed via the Reference Price Table).
// realtimeCostUSD is the subset of costs from realtime (proxy/jsonl) sources.
// actualPaidUSD is the finance-supplied invoice total for the same period; 0
// when the finance ledger has no entry for this developer, in which case
// SpendLeverage stays at 0 (rendered as "—" by the dashboard).
func ComputeDeveloper(developer string, outcomes []Outcome, totalCostUSD, realtimeCostUSD, actualPaidUSD float64) DeveloperScore {
	var points float64
	var flagged int
	for _, o := range outcomes {
		points += o.Weight * o.Quality
		// A zero-token outcome still contributes its full points (#136): the
		// number is never altered, only its ranking authority is revoked below.
		if o.ZeroToken {
			flagged++
		}
	}

	score := DeveloperScore{
		Developer:       developer,
		WeightedPoints:  points,
		TotalCostUSD:    totalCostUSD,
		ActualPaidUSD:   actualPaidUSD,
		SampleN:         len(outcomes),
		FlaggedOutcomes: flagged,
	}
	// Ranking gate: a row is ranked only with enough evidence behind it — both a
	// minimum outcome count AND a minimum spend (#133) — AND no zero-token
	// outcomes (#136). A single off-books/unattributed outcome unranks the
	// developer even when both floors are cleared: their number can't be trusted
	// to lead the board while part of the work behind it was never measured. This
	// is a pure function of the inputs; the bootstrap CI (which needs a PRNG) is
	// filled in later by the API layer for ranked rows only.
	score.Ranked = flagged == 0 &&
		score.SampleN >= MinRankedOutcomes && totalCostUSD >= MinRankedCostUSD
	if totalCostUSD > 0 {
		score.TIER = points / (totalCostUSD / tierCostScaleUSD)
		score.CoveragePercent = (realtimeCostUSD / totalCostUSD) * 100.0
	}
	// cost_per_point is guarded on the POINTS denominator (#239), NOT cost: a
	// zero-point row leaves it at 0 ("—"), while a $0-cost row with points yields
	// a legitimate 0.0 that the zero-token tripwire (#136) already surfaces.
	if points > 0 {
		score.CostPerPoint = totalCostUSD / points
	}
	if actualPaidUSD > 0 {
		score.SpendLeverage = totalCostUSD / actualPaidUSD
	}
	return score
}

// RollupTeam aggregates developer scores into a single team score.
// The team TIER uses summed points and summed costs — not an average of individual TIERs.
//
// It also decides Ranked (#502) from those same sums against the developer
// floors. The arithmetic is untouched: TIER stays points/(cost/1000) exactly, and
// a below-floor aggregate still carries its true quotient — presentation
// withholds the headline, the engine does not fabricate a number (#136).
func RollupTeam(team string, devScores []DeveloperScore) TeamScore {
	var s RollupSums
	for _, d := range devScores {
		s.add(d)
	}
	ts := s.team(team)
	ts.Developers = devScores
	return ts
}

// RollupSums are the member totals a TeamScore is derived from. SampleN and
// FlaggedOutcomes are ranking evidence that must never be served (see the note on
// TeamScore): a team-level sample_n on the wire would make
// attributed_outcome_share invertible in the anonymized modes. Every field is
// json:"-", so it marshals to {} on any path.
type RollupSums struct {
	WeightedPoints, TotalCostUSD, ActualPaidUSD float64 `json:"-"`
	// RealtimeUSD is the sum of TotalCostUSD × CoveragePercent/100, coverage_pct's
	// cost-weighted numerator.
	RealtimeUSD              float64 `json:"-"`
	SampleN, FlaggedOutcomes int     `json:"-"`
}

func (s *RollupSums) add(d DeveloperScore) {
	s.WeightedPoints += d.WeightedPoints
	s.TotalCostUSD += d.TotalCostUSD
	s.ActualPaidUSD += d.ActualPaidUSD
	s.RealtimeUSD += d.TotalCostUSD * (d.CoveragePercent / 100.0)
	s.SampleN += d.SampleN
	s.FlaggedOutcomes += d.FlaggedOutcomes
}

func (s *RollupSums) merge(o RollupSums) {
	s.WeightedPoints += o.WeightedPoints
	s.TotalCostUSD += o.TotalCostUSD
	s.ActualPaidUSD += o.ActualPaidUSD
	s.RealtimeUSD += o.RealtimeUSD
	s.SampleN += o.SampleN
	s.FlaggedOutcomes += o.FlaggedOutcomes
}

// team derives the TeamScore figures from the sums, with no Developers.
func (s RollupSums) team(name string) TeamScore {
	ts := TeamScore{
		Team:           name,
		WeightedPoints: s.WeightedPoints,
		TotalCostUSD:   s.TotalCostUSD,
		ActualPaidUSD:  s.ActualPaidUSD,
	}
	// Ranking gate on the SUMMED inputs (#502), the same three conditions and the
	// same two constants ComputeDeveloper applies — no team-only floor, because a
	// second floor drifts against the first. An empty team sums to zero and is
	// correctly unranked.
	ts.Ranked = s.FlaggedOutcomes == 0 &&
		s.SampleN >= MinRankedOutcomes && ts.TotalCostUSD >= MinRankedCostUSD
	if ts.TotalCostUSD > 0 {
		ts.TIER = ts.WeightedPoints / (ts.TotalCostUSD / tierCostScaleUSD)
		// Coverage is cost-weighted average across team members.
		ts.CoveragePercent = (s.RealtimeUSD / ts.TotalCostUSD) * 100.0
	}
	// cost_per_point on summed team totals, guarded on the points denominator
	// (#239) — same rule as the per-developer field.
	if ts.WeightedPoints > 0 {
		ts.CostPerPoint = ts.TotalCostUSD / ts.WeightedPoints
	}
	if ts.ActualPaidUSD > 0 {
		ts.SpendLeverage = ts.TotalCostUSD / ts.ActualPaidUSD
	}
	return ts
}

// AggregationMode selects whether the scoring surfaces (the served /scores API,
// the dashboard, and FormatReport) report named per-developer rows or team-only
// aggregates (#185). The zero value is AggregationDeveloper, so any caller that
// never sets a mode keeps the historical per-developer behavior; cmd/tierd
// deliberately makes the choice a REQUIRED, explicit operator decision at
// startup (no silent default there). Switching an existing deployment between
// the two modes changes who is named in every report — an EU works-council /
// GDPR Art. 22 co-determination concern, not a cosmetic toggle — so the default
// must never move silently.
type AggregationMode int

const (
	// AggregationDeveloper reports named per-developer rows. It is the historical
	// behavior and the zero value.
	AggregationDeveloper AggregationMode = iota
	// AggregationTeam reports team-only aggregates and never names an individual
	// developer, so TIER can run under EU works-council / GDPR Art. 22
	// co-determination regimes (Germany §87 BetrVG, France, Netherlands) that
	// restrict measuring or ranking named individuals.
	AggregationTeam
	// AggregationDivision rolls up ONE level higher than team (#270): named
	// division-only aggregates under the SAME k-anonymity floor as team mode,
	// still never naming an individual. It is the second ANONYMIZED level (see
	// Anonymized) and shares team mode's every suppression guard. The level is a
	// clean enum, not a hardcoded branch: adding org/department later is a new
	// value here plus the matching developer→label store read — the k-anon fold
	// (AggregateLabeledKAnon) is already level-agnostic and is reused unchanged.
	AggregationDivision
)

// String returns the lowercase level name used by the --aggregation flag, the
// `aggregation` API discriminator, and the text report. It is the inverse of
// cmd/tierd's resolveAggregationMode.
func (m AggregationMode) String() string {
	switch m {
	case AggregationDeveloper:
		return "developer"
	case AggregationTeam:
		return "team"
	case AggregationDivision:
		return "division"
	default:
		return "unknown"
	}
}

// Anonymized reports whether the mode suppresses individual identity behind a
// k-anonymized grouped rollup (#270). It is the SINGLE predicate every
// suppression guard keys on — the k-anon fold in /scores, the 403s on the
// per-developer export/fidelity/detail surfaces, and the empty-hierarchy startup
// warning — so a new anonymized level (org, department) inherits all of them by
// returning true here, with no guard edited one-by-one. AggregationDeveloper is
// the only non-anonymized mode.
func (m AggregationMode) Anonymized() bool {
	return m == AggregationTeam || m == AggregationDivision
}

// DefaultKAnonymity and MinKAnonymity bound the k-anonymity cohort floor applied
// in EVERY anonymized mode — AggregationTeam (#185) and AggregationDivision
// (#270). A group (team or division) with fewer than k COUNTED people (#856: see
// floorCensus) is collapsed into the OtherCohort bucket so no published aggregate can
// single out a group smaller than k. The default is 5; the HARD minimum is 3 —
// cmd/tierd refuses to start with a smaller k, because k of 1 or 2 would gut the
// anonymity set. These k bounds (default 5, hard minimum 3) are the #185
// k-anonymity contract.
const (
	DefaultKAnonymity = 5
	MinKAnonymity     = 3
)

// OtherCohort is the reserved team label for the k-anonymity suppression bucket
// (#185): every team below the k-floor is folded here so its cost and outcomes
// still count toward the honest grand total (they are NEVER dropped — dropping
// would silently understate team totals) while no sub-k cohort gets its own
// identifiable row. A real team literally named "other" simply merges into this
// bucket, which is harmless: the merged row is still an aggregate over one or
// more teams' developers and the totals stay exact.
const OtherCohort = "other"

// contributes reports whether this developer adds any measured quantity to a
// cohort — a non-zero outcome sample, list-price cost, or actual paid spend.
// Only contributing developers count toward the k-anonymity floor (#185):
// padding a team with pure zero-activity seats (a #39 allocated-but-idle seat)
// must NOT let a single real contributor's numbers masquerade as a k-sized
// aggregate, because the team total would then equal that one person's data and
// the anonymity would be illusory. A contributing id the caller's counting
// predicate rejects (#853, #856) still does not count — floorCensusOf applies that.
func (d DeveloperScore) contributes() bool {
	return d.SampleN != 0 || d.WeightedPoints != 0 ||
		d.TotalCostUSD != 0 || d.ActualPaidUSD != 0
}

// HasActivity is contributes() without paid spend: a row whose only
// non-zero figure is ActualPaidUSD fills no k seat, in any group. Under dated
// membership (#886) an invoice is placed at the start of its month, so every
// seat invoiced in the month a team was loaded or a developer moved reaches the
// residual as a paid-only row; counting those let a sub-k cohort's activity be
// published as "other" (the #593 disclosure). The row's figures still roll up;
// residualUnsafe still treats it as measured. The API's k-anonymity census uses
// the same rule for which ids are active in a window (#856).
func (d DeveloperScore) HasActivity() bool {
	return d.SampleN != 0 || d.WeightedPoints != 0 || d.TotalCostUSD != 0
}

// LabeledScore is one developer's score over the part of a window they spent
// in ONE group (#886): Label is the team (#185) or division (#270) valid at the
// timestamp of every cost event and outcome summed into Score. A developer who
// moved inside the window contributes one LabeledScore per group they held, so
// each group shows exactly the events that happened while the developer was in
// it. Score.Developer stays the real identity, which is what lets the k-floor
// count people rather than rows. An empty Label is the unassigned group.
type LabeledScore struct {
	Label string
	Score DeveloperScore
}

// sortLabeledScores puts labeled rows into canonical (developer, label) order.
// EVERY RollupTeam input assembled by ranging a map must pass through this first
// (#722): RollupTeam float-sums its slice and float addition is not associative,
// so a map-ordered input makes the residual irreproducible.
//
// Why identifier order and not "the order the groups happened to come out in": the
// residual is then invariant to the grouping AND to the caller's input order, so
// two runs over the same rows sum in the same sequence. The label is the second
// key because, under dated membership (#886), one developer can carry two rows
// into the same residual; without it their relative order would be unspecified.
func sortLabeledScores(rows []LabeledScore) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score.Developer != rows[j].Score.Developer {
			return rows[i].Score.Developer < rows[j].Score.Developer
		}
		return rows[i].Label < rows[j].Label
	})
}

// labeledScores extracts the scores of rows, in order.
func labeledScores(rows []LabeledScore) []DeveloperScore {
	out := make([]DeveloperScore, len(rows))
	for i, r := range rows {
		out[i] = r.Score
	}
	return out
}

// AggregateLabeledKAnon groups labeled developer scores into cohort rows under a
// k-anonymity floor (#185). It is LEVEL-AGNOSTIC: each row carries its own group
// label, and the function never inspects what that label MEANS — team labels for
// team mode (#185), division labels for division mode (#270). The label is the
// one valid when the row's events happened (#886); the caller resolves it from
// dated membership, never from today's map. A row labeled "" belongs to the
// unnamed group, which ALWAYS folds into "other" (never its own row) regardless
// of size — see the fold body.
//
// k-anonymity holds INDEPENDENTLY at each level because every level is a flat
// partition of the SAME developer set: a named group has >= k contributors by
// construction, so a cohort suppressed at one level (e.g. a sub-k team folded to
// "other") cannot be re-exposed at another — at the coarser level its developers
// are mixed into a group that itself cleared the floor.
// A team that clears the floor (see floorCensus.clears: k counted people, and k
// counted people behind each published measure) becomes its own named TeamScore;
// every other team is folded into a single OtherCohort aggregate so no cohort
// smaller than k is individually identifiable.
//
// Totals are preserved exactly ONLY WHEN NOTHING WAS SUPPRESSED (#593). When the
// residual clears the floor, every developer lands in exactly one output row and
// summing the returned TeamScores reproduces a straight RollupTeam over all devScores.
// When the residual is sub-k it is WITHHELD, the rows no longer sum to the window, and
// the second return value says so — see KAnonSuppression, which explains why that
// reconciliation property and k-anonymity cannot both hold.
//
// The returned slice is sorted by team name for a stable response, with the
// OtherCohort bucket (when present) always last so it reads as the residual. A
// k below MinKAnonymity is clamped up to MinKAnonymity: cmd/tierd already rejects
// such values at startup, but clamping keeps this function safe for any direct
// caller. Each returned TeamScore has its Developers slice cleared to nil — the
// names must never cross the anonymity boundary, even into a caller that might
// later serialize them.
//
// census is the window's counting rule (#853, #856); the API passes its
// k-anonymity census for the window. An uncounted id's figures stay in its row.
func AggregateLabeledKAnon(rows []LabeledScore, k int, census Census) ([]TeamScore, KAnonSuppression) {
	mustHaveCensus(census)
	if k < MinKAnonymity {
		k = MinKAnonymity
	}
	// Group rows by label. Each group's slice is in (developer, label) order.
	sorted := append([]LabeledScore(nil), rows...)
	sortLabeledScores(sorted)
	groups := map[string][]LabeledScore{}
	for _, r := range sorted {
		groups[r.Label] = append(groups[r.Label], r)
	}
	rollup := func(team string, devs []DeveloperScore) TeamScore {
		ts := RollupTeam(team, devs)
		ts.Developers = nil // #185: names never leave the anonymity boundary
		return ts
	}
	var named []TeamScore
	var otherRows []LabeledScore
	for team, grp := range groups {
		devs := labeledScores(grp)
		// A group clears the floor only with >= k contributing developers, AND only
		// a NAMED group ever gets its own row. Two labels are never named, folding
		// into "other" regardless of size:
		//   - OtherCohort ("other") itself — the reserved suppression label always
		//     reads as the residual aggregate, never a named team of that name.
		//   - the empty label "" — the unnamed/unassigned group (a row with no
		//     membership, or an empty division). It is conceptually the residual, so it
		//     merges into "other" rather than emitting a spurious blank-named row
		//     (which would serialize as a `team`-less object in the teams array).
		//     This matters more at the division level (#270): division is nullable,
		//     so an org that populates teams but not divisions has EVERY developer in
		//     the "" group — folding it to "other" keeps that honest instead of
		//     publishing one unlabeled >= k aggregate that masquerades as a division.
		if team != OtherCohort && team != "" && teamClearsFloor(devs, k, census) {
			named = append(named, rollup(team, devs))
			continue
		}
		otherRows = append(otherRows, grp...)
	}
	// #722: otherRows was accumulated by ranging `groups`, and Go randomizes map
	// iteration order PER RANGE. RollupTeam float-sums this slice, float addition is
	// not associative, so without this the residual row's WeightedPoints /
	// TotalCostUSD / ActualPaidUSD — and the tier, cost_per_point, coverage_pct AND
	// **ranked** derived from them — can differ between two runs over IDENTICAL data.
	// #710 promises a re-run reproduces bit-identically; a random summation order is
	// a direct counterexample.
	//
	// 🔴 `ranked` IS IN THAT LIST, AND IT IS NOT A ULP-CLASS FIELD. ts.Ranked gates on
	// `ts.TotalCostUSD >= MinRankedCostUSD`, a THRESHOLD at exactly $5.00, so a
	// last-ulp difference in the sum flips a PUBLISHED BOOLEAN — the same class of
	// defect as #711, not a cosmetic one. Micro→dollar division makes the boundary
	// MORE reachable, not less, because an exact $5.000000 total is a representable
	// landing point rather than a measure-zero accident. Measured over all 720
	// orders of the residual costs {4, 4, 4, 4, 9, 4999975} micro:
	//
	//	360 orders -> 5.0                 -> ranked TRUE
	//	360 orders -> 4.999999999999999   -> ranked FALSE
	//
	// TestAggregateTeamsKAnon_ResidualRankedIsStableOnTheCostFloor pins it.
	//
	// The asymmetry is the tell, and it is the whole bug: `named` has been sorted on
	// the line below since #185, so the named rows were reproducible while the
	// residual built from the same map by the same loop was not.
	sortLabeledScores(otherRows)
	otherDevs := labeledScores(otherRows)
	sort.Slice(named, func(i, j int) bool { return named[i].Team < named[j].Team })

	// Residual floor (#593, #853). The residual is emitted only when it is either
	// harmless or itself k-safe — see residualUnsafe for the rule.
	var sup KAnonSuppression
	if len(otherDevs) > 0 {
		if unsafe, n := residualUnsafe(otherDevs, k, census); unsafe {
			sup = KAnonSuppression{Residual: true, Developers: n, K: k}
		} else {
			named = append(named, rollup(OtherCohort, otherDevs))
		}
	}
	return named, sup
}

// teamClearsFloor reports whether a group may be published under the k-anonymity
// floor — the single eligibility rule (#185) shared by the one-window
// AggregateLabeledKAnon and the two-window CompareLabeledKAnon (#277), so both
// suppress on identical semantics and cannot drift apart. It does NOT judge the
// label itself — the OtherCohort/"" residual guard stays at each call site, since
// the reserved-label rule is a naming concern, not a floor-clearing one.
func teamClearsFloor(devs []DeveloperScore, k int, census Census) bool {
	return floorCensusOf(devs, census).clears(k)
}

// Census is one window's k-anonymity counting rule (#853, #856). The API builds
// it once per window and hands it, unchanged, to every fold that window feeds, so
// the single-window, segment and compare folds cannot count differently. No
// field may be nil.
type Census struct {
	// Uncounted reports an id that does NOT count as a person toward the floor:
	// a pseudo-developer, off the roster, a bot, or no captured evidence in the
	// window. Its figures still roll up into its row.
	Uncounted func(developer string) bool
	// UncapturedCost reports a row none of whose cost in its realtime share
	// (realtime true) or non-realtime share (realtime false) was captured by a
	// collector (manual /costs rows only). It is judged per row, never per
	// person-window, so a person fills a share's seat in a group only with
	// captured cost in that group (#943). Such a row fills no seat of that share,
	// and fills a cost seat only when one of its shares was captured: a manual
	// figure is set by whoever posted it, so it hides nobody.
	UncapturedCost func(row DeveloperScore, realtime bool) bool
	// Pseudo reports a pseudo-developer (#853): not a person, so a coverage share
	// carried by pseudo-developers alone needs no carriers. The API drops their
	// rows before any anonymised fold (#864), so it never meets one there; a fold
	// that is handed one still counts nobody for it.
	Pseudo func(developer string) bool
}

// floorCensus is one group's k-anonymity count (#185, #853, #856): the distinct
// COUNTED people in it, and how many of them carry each published measure.
//
// A person is counted when their row has activity (HasActivity) and the
// census's Uncounted is false for their id. An uncounted id's figures still roll
// up into its row: only the COUNT excludes it.
//
// People are DISTINCT (#886): under dated membership one person can carry two
// rows into one group, and counting rows would let one person fill two seats.
type floorCensus struct {
	people int
	// with[m] is the counted people carrying measure m; has[m] is whether ANY row
	// in the group, counted or not, carries it, i.e. whether the group publishes a
	// figure that m's carriers sum to.
	with [numMeasures]int
	has  [numMeasures]bool
}

// measure is one published figure a group's row sums over its members.
type measure int

const (
	measurePoints measure = iota
	measureCost
	// measureRealtime and measureNonRealtime are the two shares coverage_pct
	// splits cost into: total × coverage and total × (1 − coverage) are figures.
	measureRealtime
	measureNonRealtime
	// measurePaid is actual_paid_usd, which spend_leverage divides by.
	measurePaid
	numMeasures
)

// carries reports which measures d carries a non-zero figure for.
func (d DeveloperScore) carries() [numMeasures]bool {
	var c [numMeasures]bool
	c[measurePoints] = d.SampleN != 0 || d.WeightedPoints != 0
	c[measureCost] = d.TotalCostUSD != 0
	c[measureRealtime] = c[measureCost] && d.CoveragePercent != 0
	c[measureNonRealtime] = c[measureCost] && d.CoveragePercent != 100
	c[measurePaid] = d.ActualPaidUSD != 0
	return c
}

func floorCensusOf(devs []DeveloperScore, census Census) floorCensus {
	t := newSeatTally()
	for _, d := range devs {
		t.add(d.Developer, seatsOf(d, census))
	}
	return t.census()
}

// rowSeats is one row's part of its group's floorCensus.
type rowSeats struct {
	person bool              // a counted person with activity
	with   [numMeasures]bool // measures whose seat this row's counted person fills
	has    [numMeasures]bool // measures the row publishes a figure for
}

func seatsOf(d DeveloperScore, census Census) rowSeats {
	var s rowSeats
	carried := d.carries()
	pseudo := census.Pseudo(d.Developer)
	for m, ok := range carried {
		exempt := pseudo && (measure(m) == measureRealtime || measure(m) == measureNonRealtime)
		s.has[m] = ok && !exempt
	}
	if census.Uncounted(d.Developer) {
		return s
	}
	// Paid spend is placed at the start of its month (#886), so a counted
	// person's invoice can reach a group where they had no activity; it is
	// still a counted person's figure.
	s.with[measurePaid] = carried[measurePaid]
	if !d.HasActivity() {
		return s
	}
	s.person = true
	var captured [numMeasures]bool
	captured[measurePoints] = true
	captured[measureRealtime] = !census.UncapturedCost(d, true)
	captured[measureNonRealtime] = !census.UncapturedCost(d, false)
	captured[measureCost] = captured[measureRealtime] || captured[measureNonRealtime]
	for m, ok := range carried {
		if !ok || measure(m) == measurePaid || !captured[m] {
			continue
		}
		s.with[m] = true
	}
	return s
}

// seatTally counts a group's seats by person key, so one person fills one seat
// however many rows or labels they bring into the group.
type seatTally struct {
	people map[string]struct{}
	with   [numMeasures]map[string]struct{}
	has    [numMeasures]bool
}

func newSeatTally() *seatTally {
	t := &seatTally{people: map[string]struct{}{}}
	for m := range t.with {
		t.with[m] = map[string]struct{}{}
	}
	return t
}

func (t *seatTally) add(person string, s rowSeats) {
	if s.person {
		t.people[person] = struct{}{}
	}
	for m := range s.has {
		t.has[m] = t.has[m] || s.has[m]
		if s.with[m] {
			t.with[m][person] = struct{}{}
		}
	}
}

func (t *seatTally) census() floorCensus {
	c := floorCensus{people: len(t.people), has: t.has}
	for m := range t.with {
		c.with[m] = len(t.with[m])
	}
	return c
}

// clears is the per-measure floor (#856, E1). A group needs k counted people,
// AND every measure it publishes a non-zero figure for must itself be carried by
// k counted people. Without the second half, four people with points and no cost
// plus one person with cost form a five-person row whose cost is that one person's
// exact figure. Each derived figure divides measures that are floored here: tier
// and cost_per_point divide cost and points, spend_leverage cost and paid, and
// coverage_pct the realtime and non-realtime shares. A measure no row carries is
// exactly zero and identifies nobody.
func (c floorCensus) clears(k int) bool {
	if c.people < k {
		return false
	}
	for m := range c.has {
		if c.has[m] && c.with[m] < k {
			return false
		}
	}
	return true
}

// residualUnsafe is the residual floor (#593, #853, #856), shared by
// AggregateLabeledKAnon and each side of CompareLabeledKAnon. It returns whether the
// residual must be withheld, and its count of counted people:
//
//	clears the floor                    -> emit. It cleared the floor on its own.
//	does not, nothing measured at all   -> emit. An all-idle residual (#39 zero-cost
//	                                       seats) rolls up to zeros and identifies
//	                                       nobody, so withholding it would suppress
//	                                       the grand total for no gain.
//	does not, anything measured         -> WITHHOLD. With 1..k-1 counted people that
//	                                       is the #593 disclosure. With 0 it is
//	                                       uncounted ids alone (#853, #856): a group
//	                                       with no counted person is not a cohort that
//	                                       cleared the floor. With >= k people it is a
//	                                       measure carried by fewer than k (#856 E1).
//
// "Anything measured" is contributes() over EVERY developer, uncounted included.
func residualUnsafe(devs []DeveloperScore, k int, census Census) (bool, int) {
	measured := false
	for _, d := range devs {
		measured = measured || d.contributes()
	}
	return residualVerdict(floorCensusOf(devs, census), k, measured)
}

// residualVerdict is residualUnsafe's rule on a tallied census; measured is
// contributes() over every row.
func residualVerdict(c floorCensus, k int, measured bool) (bool, int) {
	if c.clears(k) {
		return false, c.people
	}
	return measured, c.people
}

// mustHaveCensus refuses a census with any nil predicate (#853, #856). The
// predicates are parameters, not an import, because scoring cannot import store (a
// store test imports scoring, so it would be a test import cycle). A nil default
// that meant "every id counts" would reopen #853 and #856 silently for any new
// caller; a panic on every call, not only when a contributor happens to be counted,
// makes the omission fail the first test that reaches it.
func mustHaveCensus(c Census) {
	if c.Uncounted == nil || c.UncapturedCost == nil || c.Pseudo == nil {
		panic("scoring: k-anonymity aggregation needs a census with every predicate (the API's k-anonymity census)")
	}
}

// KAnonSuppression reports what a k-anonymized aggregation WITHHELD beyond folding
// sub-k groups into the residual (#593). The zero value means nothing was withheld.
//
// 🔴 IT EXISTS BECAUSE SUPPRESSING A ROW IS NOT ENOUGH. Before #593 the residual
// "other" bucket was emitted whenever it was non-empty, with NO floor applied — so an
// org with one small team, or any window narrow enough to leave one active developer,
// published that cohort's exact figures under a label that reads as anonymized.
//
// Removing the row alone does NOT close it, and that is the whole reason this type
// exists rather than a quiet `continue`. Measured on a 6+2 developer fixture at k=5:
//
//	total          cost=66  points=16
//	named team A   cost=60  points=12
//	difference     cost= 6  points= 4   <- the suppressed 2-person cohort, exactly
//
// The grand total is an unfloored rollup of everyone, so subtracting the named rows
// reconstructs whatever was hidden. Any caller that suppresses the residual MUST also
// withhold every unfloored aggregate over the same population — the grand total and
// the cost-composition sidecar — or it has moved the disclosure rather than closed it.
// the maintainer ruled this shape (option A) on 2026-08-03, over complementary suppression and
// over merging the residual into a named group (which would publish a team figure that
// includes people not on that team — a confidently wrong number, which this project
// does not ship even to satisfy k).
//
// ⚠️ This retires the "totals are preserved exactly" property the aggregation used to
// document. That is deliberate: that property is precisely what leaks. A response
// whose rows no longer sum to its total is the honest shape here, and the API declares
// the suppression rather than leaving a consumer to discover the arithmetic does not
// close.
type KAnonSuppression struct {
	// Residual is true when a sub-k residual cohort was withheld entirely. When it is
	// true the caller must not emit any unfloored aggregate over the same population.
	Residual bool
	// Developers is how many COUNTED people (#856) were withheld. Reported so an
	// operator can tell "one person is invisible" from "a quarter of the org is",
	// which are different problems with different fixes (widen the window vs. fix the
	// org hierarchy). It is a count, never an identity.
	Developers int
	// K is the floor ACTUALLY IN FORCE, after the MinKAnonymity clamp — not the value
	// the caller requested.
	//
	// ⚠️ The distinction is not pedantic and it bit this change during development.
	// AggregateLabeledKAnon clamps any k below MinKAnonymity up to it, so a server
	// configured with k=2 enforces 3. Reporting the caller's 2 would tell an operator
	// their cohort of 2 should have been fine and leave them unable to explain the
	// suppression. Publish the number that decided the outcome, never the one that was
	// asked for.
	K int
}

// Any reports whether anything was withheld beyond the normal sub-k fold.
func (s KAnonSuppression) Any() bool { return s.Residual }

// TeamComparison is one group's paired before/after aggregate (#277): the same
// label rolled up independently over window A and window B. It is only ever
// produced for a group that clears the k-anonymity floor in BOTH windows, or for
// the reserved OtherCohort residual — see CompareLabeledKAnon. Like TeamScore it is
// LEVEL-AGNOSTIC: Team carries a team (#185) or division (#270) label depending on
// the labels passed to CompareLabeledKAnon.
type TeamComparison struct {
	Team string
	A    TeamScore
	B    TeamScore
}

// CompareLabeledKAnon pairs two windows' labeled developer scores into
// before/after group aggregates under a two-window k-anonymity INTERSECTION
// (#277). aRows and bRows are window A's and window B's rows, each labeled with
// the group (team #185 or division #270) valid when its events happened (#886) —
// so a move between or inside the windows changes neither window's history. The
// function never inspects what a label means, exactly like AggregateLabeledKAnon.
// k is the anonymity floor, clamped up to MinKAnonymity.
//
// Security invariant (the reason #277 is a server endpoint): a group is emitted as
// a NAMED paired row ONLY if it independently clears the k-floor of CONTRIBUTING
// developers in BOTH windows. Every group that is sub-k in EITHER window —
// including a group present in only one window — folds into the single OtherCohort
// bucket on BOTH sides. This makes a group's PRESENCE identical across the two
// windows: a consumer can never observe a group named in one window and absent in
// the other, which would otherwise let delta + public-window-value recover the
// suppressed window's aggregate for a sub-k cohort. The single-window
// AggregateLabeledKAnon applied per window and then diffed client-side does NOT hold
// this invariant (its "other" membership differs per window and a group can be
// named in one window only), which is exactly why the comparison is computed here,
// server-side, over both windows at once. The reserved OtherCohort and unnamed ""
// labels never earn a named row (they are the residual), matching
// AggregateLabeledKAnon's fold.
//
// Totals are preserved per window ONLY WHEN NOTHING WAS SUPPRESSED (#593): when the
// residual is emitted, every developer in a window lands in exactly one row on that
// window's side and summing a side reproduces that window's grand total. When the
// residual is withheld — because EITHER side is sub-k — the sides deliberately no
// longer reconcile, and the second return value reports it. Each returned TeamScore has its
// Developers slice cleared (names never cross the anonymity boundary). The result
// is sorted by label with the OtherCohort residual (when present) always last.
// censusA and censusB are AggregateLabeledKAnon's counting rule (#853, #856) for
// each window: evidence is judged per window, so an id counted in one window need
// not count in the other.
func CompareLabeledKAnon(aRows, bRows []LabeledScore, k int, censusA, censusB Census) ([]TeamComparison, KAnonSuppression) {
	mustHaveCensus(censusA)
	mustHaveCensus(censusB)
	if k < MinKAnonymity {
		k = MinKAnonymity
	}
	groupByLabel := func(rows []LabeledScore) map[string][]LabeledScore {
		sorted := append([]LabeledScore(nil), rows...)
		sortLabeledScores(sorted)
		g := map[string][]LabeledScore{}
		for _, r := range sorted {
			g[r.Label] = append(g[r.Label], r)
		}
		return g
	}
	aGroups := groupByLabel(aRows)
	bGroups := groupByLabel(bRows)
	rollup := func(team string, devs []DeveloperScore) TeamScore {
		ts := RollupTeam(team, devs)
		ts.Developers = nil // #185: names never leave the anonymity boundary
		return ts
	}

	// Union of labels seen in either window, so a group present in only one window
	// is still considered (and, being sub-k in the other, folds to "other").
	teamSet := map[string]struct{}{}
	for team := range aGroups {
		teamSet[team] = struct{}{}
	}
	for team := range bGroups {
		teamSet[team] = struct{}{}
	}

	var named []TeamComparison
	var otherARows, otherBRows []LabeledScore
	for team := range teamSet {
		aDevs := labeledScores(aGroups[team])
		bDevs := labeledScores(bGroups[team])
		// Intersection: named only if it clears the floor in BOTH windows and is a
		// real label (not the reserved OtherCohort residual, nor the unnamed ""
		// group — same fold as AggregateLabeledKAnon, so the nullable-division case
		// #270 stays honest). Otherwise fold BOTH sides into "other" so the group's
		// presence never differs across windows.
		if team != OtherCohort && team != "" && teamClearsFloor(aDevs, k, censusA) && teamClearsFloor(bDevs, k, censusB) {
			named = append(named, TeamComparison{
				Team: team,
				A:    rollup(team, aDevs),
				B:    rollup(team, bDevs),
			})
			continue
		}
		otherARows = append(otherARows, aGroups[team]...)
		otherBRows = append(otherBRows, bGroups[team]...)
	}
	// #722, exactly as in AggregateLabeledKAnon: both residual slices were assembled by
	// ranging `teamSet`, a map, so their order was randomized per call and the
	// float sums RollupTeam takes over them were not bit-reproducible. Sorted per
	// side — a developer present in only one window must not perturb the other
	// side's order.
	sortLabeledScores(otherARows)
	sortLabeledScores(otherBRows)
	otherA := labeledScores(otherARows)
	otherB := labeledScores(otherBRows)
	sort.Slice(named, func(i, j int) bool { return named[i].Team < named[j].Team })

	// Residual floor (#593), and it must mirror the NAMED rule five lines above, which
	// is an AND across both windows (teamClearsFloor(aDevs) && teamClearsFloor(bDevs)).
	//
	// 🔴 THE OBVIOUS COLLAPSE IS WRONG, AND IT SHIPPED IN THE FIRST DRAFT. Reducing the
	// two windows to a single count and testing that — max(nA, nB) < k — makes
	// suppression fire only when BOTH sides are sub-k. Measured with k=5, nA=2, nB=6:
	// no suppression, and the emitted row's A side was a rollup over TWO contributing
	// developers, published under an anonymized label. That is #593 itself, on this
	// endpoint, and window-A-narrow / window-B-recent is the DEFAULT compare shape, not
	// an exotic one. (min is wrong too: nA=0, nB=2 collapses to 0 and emits B's
	// 2-person row.)
	//
	// The predicate has to be applied PER SIDE, by residualUnsafe: a side is unsafe
	// when it carries a measured quantity but does not clear the floor (#853, #856:
	// only counted people fill a seat); an all-idle side identifies nobody. If either
	// side is unsafe the residual is withheld from BOTH, because a group whose
	// presence differs across windows is itself the #277 leak.
	//
	// Developers reports the larger of the two counts — the bigger of the two cohorts
	// that went unpublished. It is NOT a union: a developer contributing only in A does
	// not count toward B's total, so max is a lower bound on |A ∪ B|. The first draft's
	// comment claimed otherwise; the count is a magnitude hint for an operator, not a
	// set size.
	var sup KAnonSuppression
	if len(otherA) > 0 || len(otherB) > 0 {
		unsafeA, nA := residualUnsafe(otherA, k, censusA)
		unsafeB, nB := residualUnsafe(otherB, k, censusB)
		if unsafeA || unsafeB {
			n := nA
			if nB > n {
				n = nB
			}
			sup = KAnonSuppression{Residual: true, Developers: n, K: k}
		} else {
			named = append(named, TeamComparison{
				Team: OtherCohort,
				A:    rollup(OtherCohort, otherA),
				B:    rollup(OtherCohort, otherB),
			})
		}
	}
	return named, sup
}

// FormatReport renders a plain-text TIER report suitable for terminal output.
// Developer rows are listed by identifier, never by TIER (#830: no rank, no
// leaderboard). In AggregationTeam mode (#185) it omits the per-developer rows and
// prints only the aggregate team-total row — it never names an individual. Since
// FormatReport receives no team map it cannot break the total down by team here;
// the single grand-total row is the whole report in that mode.
func FormatReport(scores []DeveloperScore, since string, mode AggregationMode) string {
	if len(scores) == 0 {
		return "No data found for the requested period.\n"
	}

	// Ordered by identifier (#830), the same order the dashboard shows: the output
	// is deterministic and never ranks people by TIER. A below-floor row is marked
	// on its own line rather than grouped under a separator, because the list has
	// no ranked-first boundary to mark. SliceStable keeps input order within a tie.
	sorted := make([]DeveloperScore, len(scores))
	copy(sorted, scores)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Developer < sorted[j].Developer
	})

	rule := strings.Repeat("─", 72) + "\n"
	var b strings.Builder
	if mode.Anonymized() {
		// Anonymized modes — team (#185) or division (#270): no per-developer
		// header and no leaderboard, only the aggregate total below, so an
		// individual is never named in the report.
		fmt.Fprintf(&b, "TIER Report (%s aggregation) — since %s\n", mode.String(), since)
		fmt.Fprint(&b, rule)
		fmt.Fprintf(&b, "Individual developer rows are suppressed (%s-aggregation mode, #185).\n", mode.String())
		fmt.Fprint(&b, rule)
	} else {
		fmt.Fprintf(&b, "TIER Report — since %s\n", since)
		fmt.Fprint(&b, rule)
		fmt.Fprintf(&b, "%-24s  %8s  %10s  %8s  %8s\n",
			"Developer", "TIER", "Cost ($)", "Points", "Fidelity")
		fmt.Fprint(&b, rule)
	}

	// The below-floor marker's legend is built from the constants so its wording
	// can never drift from the gate that produces it (#133, #136). floorReason
	// is factored out because the TEAM TOTAL row names the same floor (#606) and one
	// report must not describe one gate two ways.
	// %g, not %.0f: the dollar floor prints exactly, so a fractional constant is
	// never rounded into a different threshold.
	floorReason := fmt.Sprintf("n < %d outcomes, an outcome with < %d AI tokens in the %d days "+
		"before it merged, or < $%g cost",
		MinRankedOutcomes, MinAttributableTokens, AttributableWindowDays, MinRankedCostUSD)
	// A one-character marker, not the phrase: the row is already 66 columns wide and
	// the rule is 72, so the words live in the legend below the rows.
	const floorMarker = "*"

	// The aggregate row comes from RollupTeam, the SAME rollup /scores' `total`
	// block and the compare endpoint's `total` use — it is not re-summed here
	// (#606). The old inline `totalPoints / (totalCost / tierCostScaleUSD)`
	// structurally could not see `Ranked`, so this row published a below-floor
	// quotient while the per-developer rows three lines up correctly printed the
	// floor verdict: one output contradicting itself. Reading the rollup is what
	// makes the verdict reach here at all — #502 proved that a field added to a
	// struct reaches only the consumers that READ the struct.
	//
	// Rolled up over `sorted`, which is `scores` reordered, so the aggregate is
	// identical in BOTH modes: an anonymized mode suppresses identity, never the
	// underlying data. (`sorted` rather than `scores` keeps this row bit-identical
	// to the report's previous inline arithmetic, which also summed in sorted order.
	// /scores rolls up its own unsorted slice, and float addition is not associative,
	// so the two can differ in the last ulp — never at %.1f/%.4f, but do not read
	// "the same rollup" as "the same summation order".)
	team := RollupTeam("", sorted)
	// #185/#270: RollupTeam binds the developer slice it was handed (ts.Developers =
	// devScores), and in an anonymized mode this function's entire contract is that
	// it never names an individual. Nothing reads the field today — but the engine's
	// own k-anon boundary nils it for exactly this reason (see CompareLabeledKAnon's
	// rollup closure), and a future `range team.Developers` here would be a leak in
	// the one function least able to afford one.
	team.Developers = nil

	anyBelowFloor := false
	for _, s := range sorted {
		if mode.Anonymized() {
			continue // no per-developer rows in an anonymized mode (#185, #270)
		}
		tag := ""
		if !s.Ranked {
			tag = " " + floorMarker
			anyBelowFloor = true
		}
		fmt.Fprintf(&b, "%-24s  %8.1f  %10.4f  %8.1f  %7.0f%%%s\n",
			s.Developer, s.TIER, s.TotalCostUSD, s.WeightedPoints, s.CoveragePercent, tag)
	}
	if anyBelowFloor {
		// One clause per line: the joined reason is wider than the 72-column rule.
		fmt.Fprintf(&b, "%s below evidence floor — too little evidence to compare its TIER:\n  %s\n",
			floorMarker, strings.ReplaceAll(floorReason, ", ", ",\n  "))
	}

	fmt.Fprint(&b, strings.Repeat("─", 72)+"\n")
	// Below the floor the QUOTIENT is withheld and the measured inputs stay — the
	// same treatment the dashboard's org KPI tile applies, for the same reason: the
	// ratio is a true quotient over a denominator too small to mean anything (the
	// canonical case is 28 points over $0.0001 = 2.8e8), and a printed number is a
	// published number whatever surrounds it. Cost, points and fidelity still print,
	// so a reader can see exactly how thin the evidence is.
	//
	// This matters most in an anonymized mode, where the loop above prints no
	// developer rows at all and this line IS the whole report.
	//
	// "—", never a muted or bracketed figure: the per-developer rows can print a
	// below-floor number because each carries its own below-floor marker. The TEAM
	// TOTAL row has no list around it — it is the report's headline, read alone and
	// quoted onward — so it is withheld, exactly as the KPI tile withholds the same
	// quantity (#502).
	// Defaults to WITHHELD and opts in to publishing, not the other way round. A
	// future edit that breaks this condition then withholds a number it should have
	// shown — a visible, reportable bug — rather than publishing one it should have
	// withheld, which is silent and is the whole subject of #502/#606.
	teamTIER := "—"
	if team.Ranked {
		teamTIER = fmt.Sprintf("%.1f", team.TIER)
	}
	// %8s, not %8.1f: fmt measures string width in RUNES, so the em dash still lands
	// in the TIER column.
	fmt.Fprintf(&b, "%-24s  %8s  %10.4f  %8.1f  %7.0f%%\n",
		"TEAM TOTAL", teamTIER, team.TotalCostUSD, team.WeightedPoints, team.CoveragePercent)
	if !team.Ranked {
		// The reason, in the same words and from the same constants as the
		// per-developer legend. A bare "—" with no stated cause trains a reader to
		// read a withheld number as a broken one.
		fmt.Fprintf(&b, "TEAM TOTAL is below the evidence floor (%s); TIER is withheld — "+
			"the measured cost, points and fidelity above stand.\n", floorReason)
	}
	fmt.Fprint(&b, strings.Repeat("─", 72)+"\n")
	fmt.Fprint(&b, "\nFormula: TIER = weighted_points / (total_cost_USD / $1,000)\n")
	fmt.Fprint(&b, "Fidelity: % of CAPTURED spend from per-request sources (proxy/JSONL); "+
		"completeness of capture is NOT measured — see zero-token flags\n")
	return b.String()
}
