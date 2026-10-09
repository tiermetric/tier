package api

// Dated team membership on the score paths (#886).
//
// Every surface that groups developers by team or division places each cost
// event and each outcome under the group the developer belonged to at THAT
// event's own timestamp — cost at the token event's ts, an outcome at its merge
// time, an actual-spend period at the start of the period. Nothing reads today's
// map for a past window. Before #886 a single current developer->team map was
// applied to every window, so moving one developer and reading /scores again
// changed both teams' historical rows by exactly that developer's figures: a
// k-anonymity bypass that needed one visible destination team and any k.
//
// Placement never moves once the event's instant has passed, because every
// membership write is stamped with the server clock and rows are append-only
// (store.appendMembershipTx and the schema triggers). The one exception is the
// one-time #914 upgrade (store.copyAliasMembershipHistory): it rewrites an
// aliased person's identifiers' rows to the placement #886 already resolved, so
// no past placement changes, and a span an identifier already held keeps its
// row's written_by. An issue whose cost fell
// while its developer was in team X and whose outcome merged after a move to Y
// shows the cost in X and the outcome in Y; nothing is re-homed.
//
// Each event is placed by the dated rows of the RAW id it was recorded under,
// never by its canonical person's rows (#914). The alias map is not dated, so a
// placement read through it would move an id's past whenever an alias was
// created, re-pointed or deleted. Instead every write that changes a raw id's
// placement — an alias edit, or a hierarchy write on its person — appends rows
// for that id at the server clock (store.syncPersonMembershipTx). The score row
// an event joins is still keyed by the canonical person.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// membershipSpan is one hierarchy_membership row: the placement held over
// [from, to). A zero to is the open row.
type membershipSpan struct {
	from, to       time.Time
	team, division string
}

func (s membershipSpan) covers(t time.Time) bool {
	return !t.Before(s.from) && (s.to.IsZero() || t.Before(s.to))
}

// overlaps reports whether the span shares an instant with [since, until); a
// zero until is open-ended.
func (s membershipSpan) overlaps(since, until time.Time) bool {
	return (until.IsZero() || s.from.Before(until)) && (s.to.IsZero() || s.to.After(since))
}

// groupLabelOf maps each ANONYMIZED aggregation level to the membership field it
// groups by (#270). Adding a level (org, department) is a new AggregationMode
// value plus one entry here. AggregationDeveloper is deliberately ABSENT: it
// never folds, so it never resolves a group label; its team_rollups name the
// team level explicitly (teamLabel).
var groupLabelOf = map[scoring.AggregationMode]func(membershipSpan) string{
	scoring.AggregationTeam:     teamLabel,
	scoring.AggregationDivision: divisionLabel,
}

func teamLabel(s membershipSpan) string     { return s.team }
func divisionLabel(s membershipSpan) string { return s.division }

// membershipTimeline answers "which group was this raw id in at instant t"
// from the dated rows.
type membershipTimeline struct {
	// byRaw maps a raw developer id to its own spans, in valid_from order.
	byRaw map[string][]membershipSpan
	// bounds is every distinct valid_from / valid_to instant, ascending.
	bounds []time.Time
}

// newMembershipTimeline indexes rows (ordered by developer, valid_from) under
// the raw id they were written for. No row is read for any other id: an id
// with no row covering t is unassigned at t, whoever its canonical person is.
func newMembershipTimeline(rows []store.MembershipRow) *membershipTimeline {
	tl := &membershipTimeline{byRaw: map[string][]membershipSpan{}}
	seen := map[time.Time]struct{}{}
	for _, r := range rows {
		tl.byRaw[r.Developer] = append(tl.byRaw[r.Developer], membershipSpan{
			from: r.ValidFrom, to: r.ValidTo, team: r.Team, division: r.Division,
		})
		seen[r.ValidFrom] = struct{}{}
		if !r.ValidTo.IsZero() {
			seen[r.ValidTo] = struct{}{}
		}
	}
	all := make([]time.Time, 0, len(seen))
	for t := range seen {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
	for _, t := range all {
		if n := len(tl.bounds); n == 0 || !tl.bounds[n-1].Equal(t) {
			tl.bounds = append(tl.bounds, t)
		}
	}
	return tl
}

// labelAt returns raw id's group label at instant t, or "" (unassigned, which
// the k-anon fold folds into "other") when none of its own rows covers t —
// which is where the history of an id first placed after the #886 upgrade, or
// first aliased after #914, stays.
func (tl *membershipTimeline) labelAt(raw string, t time.Time, label func(membershipSpan) string) string {
	for _, sp := range tl.byRaw[raw] {
		if sp.covers(t) {
			return label(sp)
		}
	}
	return ""
}

// cutsIn returns the membership boundaries strictly inside (since, until), in
// ascending order; a zero until is open-ended. Between two consecutive cuts no
// developer's placement changes, so one cost read per sub-window, labeled at its
// start, places every event in it under the placement valid at its own ts.
func (tl *membershipTimeline) cutsIn(since, until time.Time) []time.Time {
	var out []time.Time
	for _, b := range tl.bounds {
		if b.After(since) && (until.IsZero() || b.Before(until)) {
			out = append(out, b)
		}
	}
	return out
}

// devLabel keys a developer's share of a window by the group it fell in.
type devLabel struct {
	dev, label string
}

// windowGroups is one window split by dated membership at one level.
type windowGroups struct {
	// rows is one scoring row per (canonical developer, label) with any figure,
	// in (developer, label) order — the input to the k-anon folds.
	rows []scoring.LabeledScore
	// roster is every canonical developer on the roster in the window (#856):
	// one of the raw ids carrying their cost events or outcomes in the window
	// holds its own membership row overlapping it. A raw id idle in the window
	// never lends its rows to one that acted, so an alias edit cannot put a past
	// window's activity on the roster (#914). It is its own flag, never inferred
	// from a label: division is nullable, so a rostered person can sit in the ""
	// group at division level.
	roster map[string]bool
}

// groupWindow splits a loaded window by dated membership (#886), labeling with
// label (teamLabel / divisionLabel). Outcomes are placed by their own merge
// time from the rows loadWindow already read. Cost is re-read per sub-window
// between membership boundaries — one DeveloperIssueCostsWindow read per
// sub-window, over the same ts index the whole-window read seeks, so the rows
// scanned sum to the whole window's; with no boundary inside the window it
// reuses loadWindow's rows and reads nothing. Actual spend (fleet-wide windows
// only, as in loadWindow) is placed at the start of its monthly period.
func (h *Handler) groupWindow(ctx context.Context, r windowReader, win windowScores, label func(membershipSpan) string) (windowGroups, error) {
	members, err := r.HierarchyMembership(ctx)
	if err != nil {
		return windowGroups{}, fmt.Errorf("query hierarchy membership: %w", err)
	}
	tl := newMembershipTimeline(members)

	// active is every raw id with a cost event or an outcome in the window.
	active := map[string]bool{}
	outcomesBy := map[devLabel][]scoring.Outcome{}
	for i, o := range win.outcomes {
		active[o.Developer] = true
		so := win.scoredOutcomes[i]
		k := devLabel{so.Developer, tl.labelAt(o.Developer, o.Timestamp, label)}
		outcomesBy[k] = append(outcomesBy[k], so)
	}

	g := windowGroups{roster: map[string]bool{}}
	costMicro := map[devLabel]int64{}
	realtimeMicro := map[devLabel]int64{}
	capturedRealtimeMicro := map[devLabel]int64{}
	capturedNonRealtimeMicro := map[devLabel]int64{}
	addCosts := func(costs []store.DevIssueCost, at time.Time) {
		for _, c := range costs {
			active[c.Developer] = true
			dev := win.canon(c.Developer)
			l := tl.labelAt(c.Developer, at, label)
			k := devLabel{dev, l}
			costMicro[k] += c.TotalCostMicro
			realtimeMicro[k] += c.RealtimeCostMicro
			capturedRealtimeMicro[k] += c.CapturedRealtimeMicro
			capturedNonRealtimeMicro[k] += c.CapturedNonRealtimeMicro
		}
	}
	cuts := tl.cutsIn(win.since, win.until)
	if len(cuts) == 0 {
		addCosts(win.issueCosts, win.since)
	} else {
		lo := win.since
		for i := 0; i <= len(cuts); i++ {
			hi := win.until
			if i < len(cuts) {
				hi = cuts[i]
			}
			costs, err := r.DeveloperIssueCostsWindow(ctx, lo, hi, win.scope)
			if err != nil {
				return windowGroups{}, fmt.Errorf("query issue costs [%s, %s): %w", lo.Format(time.RFC3339Nano), hi.Format(time.RFC3339Nano), err)
			}
			addCosts(costs, lo)
			lo = hi
		}
	}
	for raw := range active {
		for _, sp := range tl.byRaw[raw] {
			if sp.overlaps(win.since, win.until) {
				g.roster[win.canon(raw)] = true
				break
			}
		}
	}

	// Actual spend: skipped under a repo scope exactly as loadWindow skips it
	// (#590). PeriodSpend arrives sorted by (raw developer, period), so each
	// group's float sum runs in a fixed order (#722).
	spendUSD := map[devLabel]float64{}
	if win.scope.IsFleetWide() {
		periods, err := r.ActualSpendByPeriodWindow(ctx, win.since, win.until)
		if err != nil {
			return windowGroups{}, fmt.Errorf("query actual_spend by period: %w", err)
		}
		for _, p := range periods {
			start, err := time.Parse("2006-01", p.Period)
			if err != nil {
				return windowGroups{}, fmt.Errorf("actual_spend period %q: %w", p.Period, err)
			}
			dev := win.canon(p.Developer)
			spendUSD[devLabel{dev, tl.labelAt(p.Developer, start.UTC(), label)}] += p.USD
		}
	}

	keySet := map[devLabel]struct{}{}
	for k := range outcomesBy {
		keySet[k] = struct{}{}
	}
	for k := range costMicro {
		keySet[k] = struct{}{}
	}
	for k := range spendUSD {
		keySet[k] = struct{}{}
	}
	keys := make([]devLabel, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dev != keys[j].dev {
			return keys[i].dev < keys[j].dev
		}
		return keys[i].label < keys[j].label
	})
	g.rows = make([]scoring.LabeledScore, 0, len(keys))
	for _, k := range keys {
		if win.peopleOnly && store.ResemblesUnattributed(k.dev) {
			continue
		}
		score := scoring.ComputeDeveloper(k.dev, outcomesBy[k],
			store.MicroToDollars(costMicro[k]),
			store.MicroToDollars(realtimeMicro[k]),
			spendUSD[k])
		score.CapturedRealtimeUSD = store.MicroToDollars(capturedRealtimeMicro[k])
		score.CapturedNonRealtimeUSD = store.MicroToDollars(capturedNonRealtimeMicro[k])
		g.rows = append(g.rows, scoring.LabeledScore{Label: k.label, Score: score})
	}
	return g, nil
}

// groupLabelFunc returns the membership field the active anonymized level groups
// by. A mode with no registered level is a programming error surfaced as one,
// never a silent un-anonymized fallthrough (#270).
func (h *Handler) groupLabelFunc() (func(membershipSpan) string, error) {
	f, ok := groupLabelOf[h.aggregation]
	if !ok {
		return nil, fmt.Errorf("no group label for aggregation mode %q (#270)", h.aggregation)
	}
	return f, nil
}
