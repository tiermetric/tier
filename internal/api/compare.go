package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// --- GET /api/v1/scores/compare (#277) ---
//
// Before/after period comparison: two half-open [since, until) windows (#276) in,
// per-row deltas plus a CI-overlap significance flag out. Window A is the "before"
// leg, window B the "after" leg; every delta is B - A. The endpoint reuses the same
// windowed scores computation as GET /scores (loadWindow), so a compared score can
// never diverge from what /scores reports for the same window.
//
// Three security/correctness invariants, ruled server-side for #277 (2026-07-12,
// #277=A), all enforced here rather than left to a client diffing
// two /scores calls:
//
//  1. Two-window k-anonymity INTERSECTION (anonymized modes): a group is a named row
//     only if it clears the k-floor in BOTH windows; sub-k in either folds to "other"
//     in both. Delegated to scoring.CompareTeamsKAnon so presence never differs across
//     windows and no sub-k aggregate leaks through a delta. See its doc comment. This
//     applies to team (#185) AND division (#270) — the level is just the label map.
//  2. Anonymized guard: in ANY anonymized mode (team/division) this endpoint NEVER
//     emits a named per-developer delta — Developers is an explicit empty array and
//     the rows are k-anonymized group aggregates, exactly as /scores behaves.
//  3. Per-window data_quality: each window carries its OWN data_quality block
//     (a mixed-version / zero-token signal can apply to one window and not the
//     other), built by the same dataQualityBlock the /scores path uses — so the
//     zero-token identities are name-suppressed to a bare count in an anonymized
//     mode on this surface too.

// compareResponse is the wire shape of GET /api/v1/scores/compare (#277).
type compareResponse struct {
	WindowA compareWindowMeta `json:"window_a"`
	WindowB compareWindowMeta `json:"window_b"`
	// PriceTable stamps the active price-table version (#233); a compare over two
	// windows priced under different tables surfaces that per window via each
	// window's data_quality.mixed_price_versions, not here.
	PriceTable priceTableJSON `json:"price_table"`
	// Mode echoes the server's aggregation mode (#185, #270) — "developer", "team",
	// or "division" — so a consumer knows whether Developers or Teams carries the
	// rows. It is scoring.AggregationMode.String(), the same discriminator
	// scoresResponse.Aggregation carries.
	Mode string `json:"mode"`
	// Developers is the per-developer deltas in developer mode; an explicit empty
	// array in an anonymized mode (never nil, never a named row) mirroring /scores.
	Developers []developerDeltaJSON `json:"developers"`
	// Teams is the per-group deltas in an anonymized mode only (#185, #270), each a
	// k-anonymized aggregate present in BOTH windows by construction.
	Teams []teamDeltaJSON `json:"teams,omitempty"`
	// Total is the name-free grand-rollup delta across every developer in each
	// window, present in both modes. nil when both windows are empty.
	Total *teamDeltaJSON `json:"total,omitempty"`
}

// compareWindowMeta is one window's echoed bounds and its OWN data_quality (#277
// caveat 3): the mixed-version / zero-token signal is per window, so it lives on
// the window rather than at the top level.
type compareWindowMeta struct {
	Since string `json:"since"`
	// Until is omitted when the window is open-ended (no until given).
	Until       string           `json:"until,omitempty"`
	DataQuality *dataQualityJSON `json:"data_quality,omitempty"`
}

// scoreSideJSON is one developer's score on one side (window) of a comparison. It
// mirrors the ranked-relevant fields of developerScoreJSON, including the bootstrap
// CI (#133) that drives the significance test. Zero-valued (and PresentA/PresentB
// false on the parent) when the developer had no data in that window.
type scoreSideJSON struct {
	TIER            float64 `json:"tier"`
	WeightedPoints  float64 `json:"weighted_points"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	ActualPaidUSD   float64 `json:"actual_paid_usd"`
	SpendLeverage   float64 `json:"spend_leverage"`
	CoveragePercent float64 `json:"coverage_pct"`
	// CostPerPoint is USD per weighted point (#239) for this developer on this side,
	// carried for contract parity with /scores' developer rows and the team compare
	// sides (via newTeamScoreJSON). It is the SAME scoring.DeveloperScore.CostPerPoint
	// /scores serializes — NULL for a zero-point side (#472), never a misleading 0 or
	// +Inf. No self-relative cost_per_point CI here: the CI is a significance
	// input driven by the TIER interval on the parent delta, not a per-side field.
	CostPerPoint *float64 `json:"cost_per_point"`
	SampleN      int      `json:"sample_n"`
	CILow        float64  `json:"ci_low"`
	CIHigh       float64  `json:"ci_high"`
	Ranked       bool     `json:"ranked"`
}

// developerDeltaJSON is one developer's before/after comparison (developer mode).
// PresentA/PresentB report whether the developer had data in each window; a delta
// and the significance flag are meaningful only when BOTH are true (they are 0 /
// false otherwise, never fabricated from an absent side).
type developerDeltaJSON struct {
	Developer           string        `json:"developer"`
	PresentA            bool          `json:"present_a"`
	PresentB            bool          `json:"present_b"`
	A                   scoreSideJSON `json:"a"`
	B                   scoreSideJSON `json:"b"`
	DeltaTIER           float64       `json:"delta_tier"`
	DeltaWeightedPoints float64       `json:"delta_weighted_points"`
	DeltaTotalCostUSD   float64       `json:"delta_total_cost_usd"`
	// Significant is true only when the developer is present AND ranked (#133) in
	// BOTH windows and the two bootstrap CIs do NOT overlap. Any overlap, or an
	// unranked/below-floor window, renders it false: the move is within noise or
	// the sample cannot support the claim.
	Significant bool `json:"significant"`
}

// teamDeltaJSON is one group's (or the "other" residual's) before/after comparison
// in an anonymized mode (#185, #270, #277). Team is omitempty so the name-free grand
// Total does not ship a misleading empty key. Significant is ALWAYS false in an
// anonymized mode: group aggregates carry no bootstrap CI (an interval is a
// per-developer signal, #133), so the endpoint never asserts a significance the data
// cannot back.
type teamDeltaJSON struct {
	Team                string        `json:"team,omitempty"`
	A                   teamScoreJSON `json:"a"`
	B                   teamScoreJSON `json:"b"`
	DeltaTIER           float64       `json:"delta_tier"`
	DeltaWeightedPoints float64       `json:"delta_weighted_points"`
	DeltaTotalCostUSD   float64       `json:"delta_total_cost_usd"`
	Significant         bool          `json:"significant"`
}

func (h *Handler) handleGetScoresCompare(w http.ResponseWriter, r *http.Request) {
	// Window A = before, window B = after. Each window's since_/until_ params share
	// the exact grammar and validation as /scores' since/until (#276): half-open,
	// UTC-anchored, until > since, retention-checked.
	sinceA, untilA, ok := h.parseCompareWindow(w, r, "since_a", "until_a")
	if !ok {
		return
	}
	sinceB, untilB, ok := h.parseCompareWindow(w, r, "since_b", "until_b")
	if !ok {
		return
	}

	winA, err := h.loadWindow(r.Context(), sinceA, untilA)
	if err != nil {
		h.logger.Error("compare load window a", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	winB, err := h.loadWindow(r.Context(), sinceB, untilB)
	if err != nil {
		h.logger.Error("compare load window b", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// Cost horizon (#512), attached to BOTH windows. Compare is the endpoint that
	// needs it most: window A is by construction the OLDER one, so it is the window
	// most likely to reach back past the horizon — and when it does, a full count of
	// its outcomes divided by near-zero cost renders as a dramatic apparent CHANGE
	// in TIER between the periods, carrying a significance flag, that is entirely an
	// artifact of when capture was installed.
	//
	// One lookup shared across both windows: the horizon is a property of the
	// installation, not of a window. A failure logs and omits on both, matching
	// /scores rather than 500-ing a whole comparison over a missing annotation.
	horizon, hasHorizon, herr := h.store.CostCoverageStart(r.Context())
	var perSource map[string]time.Time
	if herr != nil {
		h.logger.Error("compare query cost coverage start", "err", herr)
		hasHorizon = false
	} else if perSource, err = h.store.SourceCoverageStart(r.Context()); err != nil {
		h.logger.Error("compare query per-source coverage start", "err", err)
		perSource = nil
	}

	activePrices := store.ActivePriceTableInfo()
	resp := compareResponse{
		WindowA: compareWindowMeta{
			Since: sinceA.Format("2006-01-02"),
			Until: untilString(untilA),
			DataQuality: withCostHorizon(
				dataQualityBlock(h.aggregation, winA.zeroTokenOutcomes, winA.priceVersions),
				sinceA, horizon, hasHorizon, perSource),
		},
		WindowB: compareWindowMeta{
			Since: sinceB.Format("2006-01-02"),
			Until: untilString(untilB),
			DataQuality: withCostHorizon(
				dataQualityBlock(h.aggregation, winB.zeroTokenOutcomes, winB.priceVersions),
				sinceB, horizon, hasHorizon, perSource),
		},
		PriceTable: priceTableJSON{
			Version:       activePrices.Version,
			EffectiveDate: activePrices.EffectiveDate,
		},
		// Never nil: developer mode fills it, an anonymized mode leaves it an explicit
		// empty array so no consumer mistakes absence for "not loaded" or a named view.
		Developers: []developerDeltaJSON{},
	}
	resp.Mode = h.aggregation.String()

	if h.aggregation.Anonymized() {
		if err := h.compareTeams(r.Context(), &resp, winA, winB); err != nil {
			h.logger.Error("compare teams", "mode", h.aggregation.String(), "err", err)
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
	} else {
		resp.Developers = compareDevelopers(winA, winB)
	}

	// Grand-total delta: a name-free rollup over every developer in each window,
	// present in BOTH modes (it never names or singles out anyone).
	resp.Total = compareTotal(winA.devScores, winB.devScores)

	writeJSON(w, http.StatusOK, resp)
}

// compareTeams fills resp.Teams with the two-window k-anonymized group deltas (#277
// invariant 1). It runs under the SAME anonymized guard as /scores: Developers stays
// an explicit empty array (set by the caller) and every emitted row is a name-free
// aggregate that cleared the k-floor in BOTH windows, or the "other" residual. The
// level (team #185 / division #270) is just the label map resolveGroupLabels
// returns; group membership is not windowed, so one canonicalized map — built from
// winA.canon (aliases are not windowed, so winB.canon is identical) — serves both
// windows.
func (h *Handler) compareTeams(ctx context.Context, resp *compareResponse, winA, winB windowScores) error {
	labelOf, err := h.resolveGroupLabels(ctx, winA.canon)
	if err != nil {
		return err
	}
	for _, tc := range scoring.CompareTeamsKAnon(winA.devScores, winB.devScores, labelOf, h.kAnonymity) {
		resp.Teams = append(resp.Teams, teamDeltaJSON{
			Team:                tc.Team,
			A:                   teamSideJSON(tc.A),
			B:                   teamSideJSON(tc.B),
			DeltaTIER:           tc.B.TIER - tc.A.TIER,
			DeltaWeightedPoints: tc.B.WeightedPoints - tc.A.WeightedPoints,
			DeltaTotalCostUSD:   tc.B.TotalCostUSD - tc.A.TotalCostUSD,
			// Group aggregates carry no bootstrap CI (#133), so significance is never
			// asserted in an anonymized mode — see teamDeltaJSON.Significant.
			Significant: false,
		})
	}
	return nil
}

// compareDevelopers builds the per-developer deltas (developer mode). It emits the
// UNION of developers across both windows with PresentA/PresentB flags. Developer
// mode has no k-anonymity suppression — /scores already names every developer in
// any window — so exposing that a developer had data in one window and not the
// other leaks nothing a caller could not already get from two /scores calls. A
// delta and significance are computed ONLY when the developer is present in both
// windows; a one-window developer's delta stays 0 and Significant false rather than
// being fabricated against an absent (implicitly-zero) side.
func compareDevelopers(winA, winB windowScores) []developerDeltaJSON {
	a := indexWindowSides(winA)
	b := indexWindowSides(winB)

	names := make(map[string]struct{}, len(a)+len(b))
	for dev := range a {
		names[dev] = struct{}{}
	}
	for dev := range b {
		names[dev] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for dev := range names {
		sorted = append(sorted, dev)
	}
	sort.Strings(sorted)

	out := make([]developerDeltaJSON, 0, len(sorted))
	for _, dev := range sorted {
		sa, okA := a[dev]
		sb, okB := b[dev]
		row := developerDeltaJSON{
			Developer: dev,
			PresentA:  okA,
			PresentB:  okB,
			A:         sa,
			B:         sb,
		}
		if okA && okB {
			row.DeltaTIER = sb.TIER - sa.TIER
			row.DeltaWeightedPoints = sb.WeightedPoints - sa.WeightedPoints
			row.DeltaTotalCostUSD = sb.TotalCostUSD - sa.TotalCostUSD
			row.Significant = sa.Ranked && sb.Ranked &&
				ciDisjoint(sa.CILow, sa.CIHigh, sb.CILow, sb.CIHigh)
		}
		out = append(out, row)
	}
	return out
}

// indexWindowSides maps each developer in a window to its serialized side. The
// bootstrap CI (#133) comes from developerWindowCI — the SAME derivation /scores
// uses — so a developer's CI on the compare endpoint is bit-identical to what
// /scores reports for that window, and the significance test can never drift.
// Unranked rows keep a zero CI (an interval is meaningless there).
func indexWindowSides(win windowScores) map[string]scoreSideJSON {
	m := make(map[string]scoreSideJSON, len(win.devScores))
	for _, s := range win.devScores {
		lo, hi := developerWindowCI(win.byDev, win.issueCostIndex, s)
		m[s.Developer] = scoreSideJSON{
			TIER:            s.TIER,
			WeightedPoints:  s.WeightedPoints,
			TotalCostUSD:    s.TotalCostUSD,
			ActualPaidUSD:   s.ActualPaidUSD,
			SpendLeverage:   s.SpendLeverage,
			CoveragePercent: s.CoveragePercent,
			// The SAME source /scores' developer cost_per_point uses (#239): the
			// engine-computed, points-guarded DeveloperScore.CostPerPoint — so a
			// compare A-side is bit-identical to /scores for the same window.
			CostPerPoint: costPerPointOrNull(s.WeightedPoints, s.CostPerPoint),
			SampleN:      s.SampleN,
			CILow:        lo,
			CIHigh:       hi,
			Ranked:       s.Ranked,
		}
	}
	return m
}

// compareTotal is the name-free grand-rollup delta: RollupTeam over every developer
// in each window (the same rollup /scores' `total` uses), then B - A. Returns nil
// when both windows are empty so the response omits the key. It singles out no one,
// so it is safe in both developer and anonymized mode.
func compareTotal(aScores, bScores []scoring.DeveloperScore) *teamDeltaJSON {
	if len(aScores) == 0 && len(bScores) == 0 {
		return nil
	}
	a := scoring.RollupTeam("", aScores)
	b := scoring.RollupTeam("", bScores)
	return &teamDeltaJSON{
		A:                   teamSideJSON(a),
		B:                   teamSideJSON(b),
		DeltaTIER:           b.TIER - a.TIER,
		DeltaWeightedPoints: b.WeightedPoints - a.WeightedPoints,
		DeltaTotalCostUSD:   b.TotalCostUSD - a.TotalCostUSD,
		Significant:         false,
	}
}

// teamSideJSON maps a scoring.TeamScore onto the teamScoreJSON wire shape used for
// one side of a group (or total) comparison. It routes through newTeamScoreJSON so
// the side carries the SAME fields (including cost_per_point, #239) a /scores team
// row does, then BLANKS Team: the label lives on the parent teamDeltaJSON row, not
// on each side (and the grand Total row is name-free by construction). The
// Developers slice on TeamScore is already nil past the k-anon boundary (#185) and
// is never serialized here.
func teamSideJSON(ts scoring.TeamScore) teamScoreJSON {
	j := newTeamScoreJSON(ts)
	j.Team = "" // the name lives on the parent teamDeltaJSON row, not the side
	return j
}

// ciDisjoint reports whether two 95% bootstrap TIER intervals do NOT overlap (#277
// significance test). Non-overlapping intervals are the conservative signal that a
// before/after move is beyond sampling noise; any overlap renders the move NOT
// significant, so the dumbbell never implies a difference the sample cannot support
// (#133). The caller additionally requires both windows to be ranked.
func ciDisjoint(aLo, aHi, bLo, bHi float64) bool {
	return aHi < bLo || bHi < aLo
}

// parseCompareWindow parses one window's since_/until_ params for the compare
// endpoint, applying the identical grammar and validation as parseWindowUpperBound
// (#276): since defaults to 90 days ago and is UTC-anchored; until is optional
// (empty = open-ended) and, when set, must be strictly after since; the lower bound
// is checked against the retention horizon (#252). On any violation it writes the
// HTTP error itself and returns ok=false.
func (h *Handler) parseCompareWindow(w http.ResponseWriter, r *http.Request, sinceKey, untilKey string) (since, until time.Time, ok bool) {
	since, err := parseSince(r.URL.Query().Get(sinceKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+sinceKey+": "+err.Error())
		return time.Time{}, time.Time{}, false
	}
	since = sinceUTC(since)

	until, err = parseUntil(r.URL.Query().Get(untilKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+untilKey+": "+err.Error())
		return time.Time{}, time.Time{}, false
	}
	if !until.IsZero() {
		until = until.UTC()
		if !until.After(since) {
			writeError(w, http.StatusBadRequest,
				untilKey+" must be after "+sinceKey+" (half-open ["+sinceKey+", "+untilKey+") window)")
			return time.Time{}, time.Time{}, false
		}
	}
	if err := h.checkWindowRetention(since); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return time.Time{}, time.Time{}, false
	}
	return since, until, true
}

// untilString formats a window upper bound for the response echo: the empty string
// for an open-ended (zero) until so the JSON omits the key, else the UTC date.
func untilString(until time.Time) string {
	if until.IsZero() {
		return ""
	}
	return until.Format("2006-01-02")
}
