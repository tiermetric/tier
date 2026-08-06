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

// Ranking floor (#133, review C3). A developer's TIER is only sound enough to
// rank once there is enough evidence behind it: without a floor, one 0.5-weight
// PR against $0.0004 of cost yields TIER ≈ 1,250,000 and tops the leaderboard.
// A row is ranked only when BOTH thresholds are met; below-floor rows are still
// LISTED (never hidden) but sort after every ranked row and are excluded from
// the leaderboard ordering.
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
	// listed-not-hidden semantics. Unranked rows are listed but sorted after
	// every ranked row.
	Ranked bool
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
// flagged count that feed Ranked stay function-local. They are not withheld out
// of tidiness — publishing a team-level sample_n would hand the anonymized modes
// a second equation. `data_quality.attributed_outcome_share` is a ratio of
// outcome COUNTS that is safe to publish precisely because its denominator is
// published nowhere (see the k-anon strip block in internal/api/handler.go); a
// team sample_n would supply it. Keep the counts local.

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
	ts := TeamScore{
		Team:       team,
		Developers: devScores,
	}
	// sampleN and flagged are the summed ranking evidence. They stay local on
	// purpose (see the note on TeamScore): a team-level sample_n on the wire would
	// make attributed_outcome_share invertible in the anonymized modes.
	var sampleN, flagged int
	for _, d := range devScores {
		ts.WeightedPoints += d.WeightedPoints
		ts.TotalCostUSD += d.TotalCostUSD
		ts.ActualPaidUSD += d.ActualPaidUSD
		sampleN += d.SampleN
		flagged += d.FlaggedOutcomes
	}
	// Ranking gate on the SUMMED inputs (#502), the same three conditions and the
	// same two constants ComputeDeveloper applies — no team-only floor, because a
	// second floor drifts against the first. An empty team sums to zero and is
	// correctly unranked.
	ts.Ranked = flagged == 0 &&
		sampleN >= MinRankedOutcomes && ts.TotalCostUSD >= MinRankedCostUSD
	if ts.TotalCostUSD > 0 {
		ts.TIER = ts.WeightedPoints / (ts.TotalCostUSD / tierCostScaleUSD)
		// Coverage is cost-weighted average across team members.
		var realtimeTotal float64
		for _, d := range devScores {
			realtimeTotal += d.TotalCostUSD * (d.CoveragePercent / 100.0)
		}
		ts.CoveragePercent = (realtimeTotal / ts.TotalCostUSD) * 100.0
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
	// (AggregateTeamsKAnon) is already level-agnostic and is reused unchanged.
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
// (#270). A group (team or division) whose count of CONTRIBUTING developers is
// below k is collapsed into the OtherCohort bucket so no published aggregate can
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
// the anonymity would be illusory.
func (d DeveloperScore) contributes() bool {
	return d.SampleN != 0 || d.WeightedPoints != 0 ||
		d.TotalCostUSD != 0 || d.ActualPaidUSD != 0
}

// AggregateTeamsKAnon groups developer scores into cohort rows under a
// k-anonymity floor (#185). It is LEVEL-AGNOSTIC: teamOf maps a (canonical)
// developer identity to a group label, and the function never inspects what that
// label MEANS — pass a developer→team map for team mode (#185) or a
// developer→division map for division mode (#270) and it folds identically. This
// is why adding a new aggregation level is a map-swap, not a fork: the same call
// with another label map yields that level's k-anonymized rollup. A developer
// absent from the map, or mapped to "", belongs to the unnamed group, which
// ALWAYS folds into "other" (never its own row) regardless of size — see the
// fold body.
//
// k-anonymity holds INDEPENDENTLY at each level because every level is a flat
// partition of the SAME developer set: a named group has >= k contributors by
// construction, so a cohort suppressed at one level (e.g. a sub-k team folded to
// "other") cannot be re-exposed at another — at the coarser level its developers
// are mixed into a group that itself cleared the floor.
// A team with at least k CONTRIBUTING developers (see contributes) becomes its
// own named TeamScore; every team below k is folded into a single OtherCohort
// aggregate so no cohort smaller than k is individually identifiable.
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
func AggregateTeamsKAnon(devScores []DeveloperScore, teamOf map[string]string, k int) ([]TeamScore, KAnonSuppression) {
	if k < MinKAnonymity {
		k = MinKAnonymity
	}
	// Group developers by team name.
	groups := map[string][]DeveloperScore{}
	for _, d := range devScores {
		team := teamOf[d.Developer]
		groups[team] = append(groups[team], d)
	}
	rollup := func(team string, devs []DeveloperScore) TeamScore {
		ts := RollupTeam(team, devs)
		ts.Developers = nil // #185: names never leave the anonymity boundary
		return ts
	}
	var named []TeamScore
	var otherDevs []DeveloperScore
	for team, devs := range groups {
		// A group clears the floor only with >= k contributing developers, AND only
		// a NAMED group ever gets its own row. Two labels are never named, folding
		// into "other" regardless of size:
		//   - OtherCohort ("other") itself — the reserved suppression label always
		//     reads as the residual aggregate, never a named team of that name.
		//   - the empty label "" — the unnamed/unassigned group (a developer absent
		//     from teamOf, or mapped to ""). It is conceptually the residual, so it
		//     merges into "other" rather than emitting a spurious blank-named row
		//     (which would serialize as a `team`-less object in the teams array).
		//     This matters more at the division level (#270): division is nullable,
		//     so an org that populates teams but not divisions has EVERY developer in
		//     the "" group — folding it to "other" keeps that honest instead of
		//     publishing one unlabeled >= k aggregate that masquerades as a division.
		if team != OtherCohort && team != "" && teamClearsFloor(devs, k) {
			named = append(named, rollup(team, devs))
			continue
		}
		otherDevs = append(otherDevs, devs...)
	}
	sort.Slice(named, func(i, j int) bool { return named[i].Team < named[j].Team })

	// Residual floor (#593). The residual is emitted only when it is either harmless
	// or itself k-safe:
	//
	//   contributors == 0  -> emit. An all-idle residual (#39 zero-cost seats) rolls
	//                         up to zeros and identifies nobody, so withholding it
	//                         would suppress the grand total for no gain.
	//   1 <= contributors < k -> SUPPRESS, and tell the caller. This is the live
	//                         disclosure: a cohort too small to hide behind, published
	//                         under a label that reads as anonymized.
	//   contributors >= k  -> emit. It cleared the floor on its own.
	var sup KAnonSuppression
	if len(otherDevs) > 0 {
		n := contributingCount(otherDevs)
		if n > 0 && n < k {
			sup = KAnonSuppression{Residual: true, Developers: n, K: k}
		} else {
			named = append(named, rollup(OtherCohort, otherDevs))
		}
	}
	return named, sup
}

// teamClearsFloor reports whether a group's developer slice has at least k
// CONTRIBUTING developers (see contributes) — the single k-anonymity eligibility
// rule (#185) shared by the one-window AggregateTeamsKAnon and the two-window
// CompareTeamsKAnon (#277), so both suppress on identical semantics and cannot
// drift apart. It counts contributors only: padding a group with idle #39 seats
// never lets a sub-k cohort masquerade as k-sized. It does NOT judge the label
// itself — the OtherCohort/"" residual guard stays at each call site, since the
// reserved-label rule is a naming concern, not a floor-clearing one.
func teamClearsFloor(devs []DeveloperScore, k int) bool {
	return contributingCount(devs) >= k
}

// contributingCount is teamClearsFloor's numerator, split out because the residual
// suppression rule (#593) needs the COUNT, not just the verdict: a residual with zero
// contributors carries no figures and is harmless, while one with 1..k-1 is a live
// disclosure.
func contributingCount(devs []DeveloperScore) int {
	n := 0
	for _, d := range devs {
		if d.contributes() {
			n++
		}
	}
	return n
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
	// Developers is how many CONTRIBUTING developers were withheld. Reported so an
	// operator can tell "one person is invisible" from "a quarter of the org is",
	// which are different problems with different fixes (widen the window vs. fix the
	// org hierarchy). It is a count, never an identity.
	Developers int
	// K is the floor ACTUALLY IN FORCE, after the MinKAnonymity clamp — not the value
	// the caller requested.
	//
	// ⚠️ The distinction is not pedantic and it bit this change during development.
	// AggregateTeamsKAnon clamps any k below MinKAnonymity up to it, so a server
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
// the reserved OtherCohort residual — see CompareTeamsKAnon. Like TeamScore it is
// LEVEL-AGNOSTIC: Team carries a team (#185) or division (#270) label depending on
// the label map passed to CompareTeamsKAnon.
type TeamComparison struct {
	Team string
	A    TeamScore
	B    TeamScore
}

// CompareTeamsKAnon pairs two windows' developer scores into before/after group
// aggregates under a two-window k-anonymity INTERSECTION (#277). aScores and
// bScores are the per-developer scores of window A and window B; teamOf maps a
// (canonical) developer identity to its group label (team #185 or division #270 —
// the function never inspects what the label means, exactly like
// AggregateTeamsKAnon). Membership is not windowed, so one map serves both windows.
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
// AggregateTeamsKAnon applied per window and then diffed client-side does NOT hold
// this invariant (its "other" membership differs per window and a group can be
// named in one window only), which is exactly why the comparison is computed here,
// server-side, over both windows at once. The reserved OtherCohort and unnamed ""
// labels never earn a named row (they are the residual), matching
// AggregateTeamsKAnon's fold.
//
// Totals are preserved per window ONLY WHEN NOTHING WAS SUPPRESSED (#593): when the
// residual is emitted, every developer in a window lands in exactly one row on that
// window's side and summing a side reproduces that window's grand total. When the
// residual is withheld — because EITHER side is sub-k — the sides deliberately no
// longer reconcile, and the second return value reports it. Each returned TeamScore has its
// Developers slice cleared (names never cross the anonymity boundary). The result
// is sorted by label with the OtherCohort residual (when present) always last.
func CompareTeamsKAnon(aScores, bScores []DeveloperScore, teamOf map[string]string, k int) ([]TeamComparison, KAnonSuppression) {
	if k < MinKAnonymity {
		k = MinKAnonymity
	}
	groupByTeam := func(scores []DeveloperScore) map[string][]DeveloperScore {
		g := map[string][]DeveloperScore{}
		for _, d := range scores {
			g[teamOf[d.Developer]] = append(g[teamOf[d.Developer]], d)
		}
		return g
	}
	aGroups := groupByTeam(aScores)
	bGroups := groupByTeam(bScores)
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
	var otherA, otherB []DeveloperScore
	for team := range teamSet {
		aDevs := aGroups[team]
		bDevs := bGroups[team]
		// Intersection: named only if it clears the floor in BOTH windows and is a
		// real label (not the reserved OtherCohort residual, nor the unnamed ""
		// group — same fold as AggregateTeamsKAnon, so the nullable-division case
		// #270 stays honest). Otherwise fold BOTH sides into "other" so the group's
		// presence never differs across windows.
		if team != OtherCohort && team != "" && teamClearsFloor(aDevs, k) && teamClearsFloor(bDevs, k) {
			named = append(named, TeamComparison{
				Team: team,
				A:    rollup(team, aDevs),
				B:    rollup(team, bDevs),
			})
			continue
		}
		otherA = append(otherA, aDevs...)
		otherB = append(otherB, bDevs...)
	}
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
	// The predicate has to be applied PER SIDE. A side is unsafe when it carries
	// between 1 and k-1 contributors; a side with 0 is an empty aggregate that
	// identifies nobody. If either side is unsafe the residual is withheld from BOTH,
	// because a group whose presence differs across windows is itself the #277 leak.
	//
	// Developers reports the larger of the two counts — the bigger of the two cohorts
	// that went unpublished. It is NOT a union: a developer contributing only in A does
	// not count toward B's total, so max is a lower bound on |A ∪ B|. The first draft's
	// comment claimed otherwise; the count is a magnitude hint for an operator, not a
	// set size.
	var sup KAnonSuppression
	if len(otherA) > 0 || len(otherB) > 0 {
		nA, nB := contributingCount(otherA), contributingCount(otherB)
		unsafe := func(n int) bool { return n > 0 && n < k }
		if unsafe(nA) || unsafe(nB) {
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

// FormatReport renders a plain-text TIER report suitable for terminal output. In
// AggregationTeam mode (#185) it omits the per-developer leaderboard entirely and
// prints only the aggregate team-total row — it never names an individual. Since
// FormatReport receives no team map it cannot break the total down by team here;
// the single grand-total row is the whole report in that mode.
func FormatReport(scores []DeveloperScore, since string, mode AggregationMode) string {
	if len(scores) == 0 {
		return "No data found for the requested period.\n"
	}

	// Two-tier ordering (#133): every ranked developer first (by TIER desc),
	// then every below-floor developer (by WeightedPoints desc). One rule shared
	// with the dashboard comparator — the leaderboard never ranks a low-evidence
	// row above a real one. SliceStable keeps input order stable within a tie.
	sorted := make([]DeveloperScore, len(scores))
	copy(sorted, scores)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Ranked != sorted[j].Ranked {
			return sorted[i].Ranked // ranked (true) sorts before unranked (false)
		}
		if sorted[i].Ranked {
			return sorted[i].TIER > sorted[j].TIER
		}
		return sorted[i].WeightedPoints > sorted[j].WeightedPoints
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

	// The below-floor separator is built from the constants so its wording can
	// never drift from the gate that produces it (#133). floorReason is factored out
	// because the TEAM TOTAL row names the same floor (#606) and one report must not
	// describe one gate two ways.
	floorReason := fmt.Sprintf("n < %d outcomes or < $%.0f cost",
		MinRankedOutcomes, MinRankedCostUSD)
	floorSeparator := "--- below ranking floor (" + floorReason + ") ---"

	// The aggregate row comes from RollupTeam, the SAME rollup /scores' `total`
	// block and the compare endpoint's `total` use — it is not re-summed here
	// (#606). The old inline `totalPoints / (totalCost / tierCostScaleUSD)`
	// structurally could not see `Ranked`, so this row published a below-floor
	// quotient while the per-developer rows three lines up correctly printed the
	// floor separator: one output contradicting itself. Reading the rollup is what
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
	// own k-anon boundary nils it for exactly this reason (see CompareTeamsKAnon's
	// rollup closure), and a future `range team.Developers` here would be a leak in
	// the one function least able to afford one.
	team.Developers = nil

	printedSeparator := false
	for _, s := range sorted {
		if mode.Anonymized() {
			continue // no per-developer rows in an anonymized mode (#185, #270)
		}
		if !s.Ranked && !printedSeparator {
			fmt.Fprint(&b, floorSeparator+"\n")
			printedSeparator = true
		}
		fmt.Fprintf(&b, "%-24s  %8.1f  %10.4f  %8.1f  %7.0f%%\n",
			s.Developer, s.TIER, s.TotalCostUSD, s.WeightedPoints, s.CoveragePercent)
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
	// below-floor number because the separator above them and the ordering around
	// them carry the verdict. The TEAM TOTAL row has no list around it — it is the
	// report's headline, read alone and quoted onward — so it is withheld, exactly as
	// the KPI tile withholds the same quantity (#502).
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
		// per-developer separator. A bare "—" with no stated cause trains a reader to
		// read a withheld number as a broken one.
		fmt.Fprintf(&b, "TEAM TOTAL is below the ranking floor (%s); TIER is withheld — "+
			"the measured cost, points and fidelity above stand.\n", floorReason)
	}
	fmt.Fprint(&b, strings.Repeat("─", 72)+"\n")
	fmt.Fprint(&b, "\nFormula: TIER = weighted_points / (total_cost_USD / $1,000)\n")
	fmt.Fprint(&b, "Fidelity: % of CAPTURED spend from per-request sources (proxy/JSONL); "+
		"completeness of capture is NOT measured — see zero-token flags\n")
	return b.String()
}
