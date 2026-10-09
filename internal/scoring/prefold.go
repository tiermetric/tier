package scoring

import (
	"slices"
	"sort"
)

// LabelInput is one label's pre-fold input for one period (#913, #914): the sums
// RollupTeam reads and the person keys the k floor counts. It is stored with a
// sealed period and never served; AggregateFolded and CompareFolded refold from it.
//
// Every field is json:"-": a fold input marshals to {} on any path.
type LabelInput struct {
	Label string     `json:"-"`
	Sums  RollupSums `json:"-"`
	// People are the counted people with activity; Carriers[m] the counted people
	// filling measure m's seat; Has[m] whether any row publishes a figure for m.
	// Keys are sorted and distinct. Carriers and Has are indexed in measure order;
	// a stored form names each index by MeasureNames.
	People   []string              `json:"-"`
	Carriers [numMeasures][]string `json:"-"`
	Has      [numMeasures]bool     `json:"-"`
	// Contributes is contributes() over every row, counted or not.
	Contributes bool `json:"-"`
}

// MeasureNames is the stored name of each measure index of LabelInput.Carriers
// and Has. A name is never reassigned to another measure.
var MeasureNames = [numMeasures]string{
	measurePoints:      "points",
	measureCost:        "cost",
	measureRealtime:    "realtime",
	measureNonRealtime: "non_realtime",
	measurePaid:        "paid",
}

// PeopleSetName is the stored name of LabelInput.People's person set; no
// MeasureNames entry may equal it.
const PeopleSetName = "people"

// PreFold reduces one window's labeled rows to one LabelInput per label, sorted
// by label. Each label's sums are taken in AggregateLabeledKAnon's (developer,
// label) order, so a label published under its own name refolds bit-identically.
// key maps a canonical id to its stored person key; census sees the raw row.
func PreFold(rows []LabeledScore, census Census, key func(canonicalID string) string) []LabelInput {
	mustHaveCensus(census)
	sorted := append([]LabeledScore(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Label != sorted[j].Label {
			return sorted[i].Label < sorted[j].Label
		}
		return sorted[i].Score.Developer < sorted[j].Score.Developer
	})
	var out []LabelInput
	for i, r := range sorted {
		if i == 0 || r.Label != sorted[i-1].Label {
			out = append(out, LabelInput{Label: r.Label})
		}
		in := &out[len(out)-1]
		in.Sums.add(r.Score)
		in.Contributes = in.Contributes || r.Score.contributes()
		s := seatsOf(r.Score, census)
		id := key(r.Score.Developer)
		if s.person {
			in.People = append(in.People, id)
		}
		for m := range s.has {
			in.Has[m] = in.Has[m] || s.has[m]
			if s.with[m] {
				in.Carriers[m] = append(in.Carriers[m], id)
			}
		}
	}
	for i := range out {
		out[i].People = sortedDistinct(out[i].People)
		for m := range out[i].Carriers {
			out[i].Carriers[m] = sortedDistinct(out[i].Carriers[m])
		}
	}
	return out
}

func sortedDistinct(keys []string) []string {
	sort.Strings(keys)
	return slices.Compact(keys)
}

// AggregateFolded is AggregateLabeledKAnon over PreFold's inputs: the same named
// rule, per-measure floor and residual rule, with each person key filling one
// seat across all of the residual's labels. A named row equals
// AggregateLabeledKAnon's bit for bit. The residual adds whole labels in label
// order where AggregateLabeledKAnon adds rows in developer order, so its floats
// may differ: in the last places for non-negative addends, and without a
// relative bound when credit memos (negative paid) cancel. Its Ranked flag can
// differ at exactly MinRankedCostUSD (360 of 720 labelings of the #722 fixture).
// The order is fixed, so a re-fold is bit-identical (#722); any replay of a
// folded body must use the folded functions.
func AggregateFolded(inputs []LabelInput, k int) ([]TeamScore, KAnonSuppression) {
	if k < MinKAnonymity {
		k = MinKAnonymity
	}
	var named []TeamScore
	var other []LabelInput
	for _, in := range sortedInputs(inputs) {
		if in.Label != OtherCohort && in.Label != "" && foldedCensus([]LabelInput{in}).clears(k) {
			named = append(named, in.Sums.team(in.Label))
			continue
		}
		other = append(other, in)
	}
	var sup KAnonSuppression
	if len(other) > 0 {
		if unsafe, n := foldedResidualUnsafe(other, k); unsafe {
			sup = KAnonSuppression{Residual: true, Developers: n, K: k}
		} else {
			named = append(named, foldedSums(other).team(OtherCohort))
		}
	}
	return named, sup
}

// CompareFolded is CompareLabeledKAnon over two windows' PreFold inputs: a label
// is named only when it clears the floor in BOTH windows, every other label folds
// into "other" on both sides, and the residual is withheld from both when either
// side is unsafe. Its float caveat is AggregateFolded's.
func CompareFolded(a, b []LabelInput, k int) ([]TeamComparison, KAnonSuppression) {
	if k < MinKAnonymity {
		k = MinKAnonymity
	}
	as, bs := sortedInputs(a), sortedInputs(b)
	var named []TeamComparison
	var otherA, otherB []LabelInput
	for i, j := 0, 0; i < len(as) || j < len(bs); {
		var label string
		var ga, gb []LabelInput
		switch {
		case j == len(bs) || (i < len(as) && as[i].Label < bs[j].Label):
			label, ga, i = as[i].Label, as[i:i+1], i+1
		case i == len(as) || bs[j].Label < as[i].Label:
			label, gb, j = bs[j].Label, bs[j:j+1], j+1
		default:
			label, ga, gb, i, j = as[i].Label, as[i:i+1], bs[j:j+1], i+1, j+1
		}
		if label != OtherCohort && label != "" && foldedCensus(ga).clears(k) && foldedCensus(gb).clears(k) {
			named = append(named, TeamComparison{Team: label, A: ga[0].Sums.team(label), B: gb[0].Sums.team(label)})
			continue
		}
		otherA = append(otherA, ga...)
		otherB = append(otherB, gb...)
	}
	var sup KAnonSuppression
	if len(otherA) > 0 || len(otherB) > 0 {
		unsafeA, nA := foldedResidualUnsafe(otherA, k)
		unsafeB, nB := foldedResidualUnsafe(otherB, k)
		if unsafeA || unsafeB {
			sup = KAnonSuppression{Residual: true, Developers: max(nA, nB), K: k}
		} else {
			named = append(named, TeamComparison{
				Team: OtherCohort,
				A:    foldedSums(otherA).team(OtherCohort),
				B:    foldedSums(otherB).team(OtherCohort),
			})
		}
	}
	return named, sup
}

// sortedInputs copies inputs in label order. A label appears once per window.
func sortedInputs(inputs []LabelInput) []LabelInput {
	out := append([]LabelInput(nil), inputs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	for i := 1; i < len(out); i++ {
		if out[i].Label == out[i-1].Label {
			panic("scoring: fold inputs carry label " + out[i].Label + " twice")
		}
	}
	return out
}

func foldedCensus(inputs []LabelInput) floorCensus {
	t := newSeatTally()
	for _, in := range inputs {
		for _, p := range in.People {
			t.people[p] = struct{}{}
		}
		for m := range in.Carriers {
			t.has[m] = t.has[m] || in.Has[m]
			for _, p := range in.Carriers[m] {
				t.with[m][p] = struct{}{}
			}
		}
	}
	return t.census()
}

func foldedResidualUnsafe(inputs []LabelInput, k int) (bool, int) {
	measured := false
	for _, in := range inputs {
		measured = measured || in.Contributes
	}
	return residualVerdict(foldedCensus(inputs), k, measured)
}

func foldedSums(inputs []LabelInput) RollupSums {
	var s RollupSums
	for _, in := range inputs {
		s.merge(in.Sums)
	}
	return s
}
