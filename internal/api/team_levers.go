package api

import (
	"context"
	"fmt"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// Only the cache share may accompany a published anonymized group (#1368).
// A nil share omits its key; both input-side cells need k contributors.
type teamLeversJSON struct {
	CacheReadShare *float64 `json:"cache_read_share,omitempty"`
}

// loadGroupComposition uses the same membership cuts, raw-id placement and
// effective attribution as groupWindow. Bots/uncounted people's costs stay in
// the population; pseudo-developers' costs do not, exactly as on team rows.
func (h *Handler) loadGroupComposition(ctx context.Context, r windowReader, win windowScores, g windowGroups) (map[string][]store.DeveloperComposition, error) {
	label, err := h.groupLabelFunc()
	if err != nil {
		return nil, err
	}
	out := map[string][]store.DeveloperComposition{}
	cuts := g.timeline.cutsIn(win.since, win.until)
	lo := win.since
	for i := 0; i <= len(cuts); i++ {
		hi := win.until
		if i < len(cuts) {
			hi = cuts[i]
		}
		costs, err := r.DeveloperCompositionWindow(ctx, lo, hi, win.scope)
		if err != nil {
			return nil, fmt.Errorf("query group composition: %w", err)
		}
		for _, c := range costs {
			if store.ResemblesUnattributed(win.canon(c.Developer)) {
				continue
			}
			group := g.timeline.labelAt(c.Developer, lo, label)
			c.Developer = win.canon(c.Developer)
			out[group] = append(out[group], c)
		}
		lo = hi
	}
	return out, nil
}

// Reuse the row census, captured-cost predicates and effective k. Lever seats
// require BOTH captured spend and merged work in this same dated group. Counts
// are canonical distinct people, including when several labels fold into other.
func leverSeats(data scoresWindowData, teams []scoring.TeamScore) (map[string]map[string]bool, func(string) string) {
	published := map[string]bool{}
	for _, team := range teams {
		published[team.Team] = true
	}
	groupOf := func(label string) string {
		if label != "" && label != scoring.OtherCohort && published[label] {
			return label
		}
		return scoring.OtherCohort
	}
	merged := map[devLabel]bool{}
	for _, o := range data.win.outcomes {
		if o.Source != store.OutcomeSourcePush {
			merged[devLabel{data.win.canon(o.Developer), groupOf(data.groups.outcomeLabels[o.ID])}] = true
		}
	}
	census := data.census.scoring()
	seats := map[string]map[string]bool{}
	for _, r := range data.groups.rows {
		row := r.Score
		if census.Uncounted(row.Developer) || !merged[devLabel{row.Developer, groupOf(r.Label)}] ||
			(census.UncapturedCost(row, true) && census.UncapturedCost(row, false)) {
			continue
		}
		group := groupOf(r.Label)
		if seats[group] == nil {
			seats[group] = map[string]bool{}
		}
		seats[group][row.Developer] = true
	}
	return seats, groupOf
}

func leverSeatFloor(seats map[string]map[string]bool, teams []scoring.TeamScore, k int) bool {
	for _, team := range teams {
		// #593: an unsafe published lever group withholds all levers.
		if len(seats[team.Team]) < max(k, scoring.MinKAnonymity) {
			return false
		}
	}
	return true
}

// Shares cover every cost contributor, including non-seats. The cache partition
// needs k positive-input contributors with cache reads and k without them.
func groupLevers(data scoresWindowData, teams []scoring.TeamScore, k int) (map[string]teamLeversJSON, string) {
	seats, groupOf := leverSeats(data, teams)
	if !leverSeatFloor(seats, teams, k) {
		return nil, "group_seats_below_k"
	}
	k = max(k, scoring.MinKAnonymity)
	costs := map[string][]store.ModelClassCost{}
	carriers := map[string]map[string]bool{}
	// Score rows are ordered by developer then label; visit each label once.
	seen := map[string]bool{}
	for _, r := range data.groups.rows {
		if seen[r.Label] {
			continue
		}
		seen[r.Label] = true
		group := groupOf(r.Label)
		if carriers[group] == nil {
			carriers[group] = map[string]bool{}
		}
		for _, c := range data.groupComposition[r.Label] {
			costs[group] = append(costs[group], c.ModelClassCost)
			if c.InputTok+c.CacheRead+c.CacheWrite5m+c.CacheWrite1h > 0 {
				carriers[group][c.Developer] = carriers[group][c.Developer] || c.CacheRead > 0
			}
		}
	}
	out := map[string]teamLeversJSON{}
	reason := ""
	for _, team := range teams {
		c := store.BuildCostComposition(costs[team.Team])
		row := teamLeversJSON{}
		with := 0
		for _, cache := range carriers[team.Team] {
			if cache {
				with++
			}
		}
		if with < k || len(carriers[team.Team])-with < k {
			reason = "category_seats_below_k"
		} else {
			row.CacheReadShare = &c.CacheReadShare
		}
		out[team.Team] = row
	}
	return out, reason
}
