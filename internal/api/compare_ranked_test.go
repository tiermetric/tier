package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// --- #605: the compare total carries a DERIVED ranking verdict ---------------
//
// The rule is one sentence: anything derived from an unranked input is itself
// unranked. On the wire that is `total.ranked = a.ranked && b.ranked`, computed
// ONCE here so every consumer can read it instead of re-deriving the floor.
//
// The one-ranked-one-unranked case is the whole reason it is an AND. With a ranked
// baseline beside an unranked selected window, publishing Δ hands the reader
// `selected = baseline + Δ` — a perfect reconstruction of the number the floor just
// refused to rank. Both mixed arms are asserted below for that reason; an OR, or a
// verdict copied from either side alone, passes the two symmetric arms and fails
// exactly here.

// compareTotalRaw pulls the compare `total` block out as raw keys, so a test can
// tell an ABSENT field from one that decoded to its zero value. For a bool that
// distinction is the entire assertion: `ranked: false` and "no ranked key at all"
// both unmarshal into false, and only one of them is the contract — the same
// ambiguity #603 was filed over, one endpoint across.
func compareTotalRaw(t *testing.T, h *Handler, url string) map[string]json.RawMessage {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, url, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		Total map[string]json.RawMessage `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal compare: %v (body=%s)", err, body)
	}
	if resp.Total == nil {
		t.Fatalf("no total block in the compare response: %s", body)
	}
	return resp.Total
}

// seedRankedWindow seeds a window that clears BOTH #133 floors: 3 outcomes and $12
// of measured spend, with cost seeded at the outcome instant so nothing trips the
// #136 zero-token tripwire.
func seedRankedWindow(t *testing.T, db *store.DB, prefix string, instant time.Time) {
	t.Helper()
	for _, suffix := range []string{"1", "2", "3"} {
		issue := prefix + "-" + suffix
		seedOutcomeAt(t, db, "alice", issue, 2.0, 1.0, instant)
		seedCostAt(t, db, "alice", issue, 4.0, instant)
	}
}

// seedUnrankedWindow seeds the canonical #502 window: 2 outcomes worth 28 weighted
// points against $0.0001 of measured spend. TIER is a true 2.8e8 and the aggregate
// is below the floor on BOTH the outcome count and the spend.
func seedUnrankedWindow(t *testing.T, db *store.DB, prefix string, instant time.Time) {
	t.Helper()
	for _, suffix := range []string{"1", "2"} {
		issue := prefix + "-" + suffix
		seedOutcomeAt(t, db, "alice", issue, 14.0, 1.0, instant)
		seedCostAt(t, db, "alice", issue, 0.00005, instant)
	}
}

// seedTeamMemberAt is seedTeamMember with an explicit instant, so a contributing
// developer can be placed in a specific compare window. seedTeamMember itself
// seeds "now", which both compare windows deliberately exclude.
func seedTeamMemberAt(t *testing.T, db *store.DB, dev, issue, team string, cost, weight float64, instant time.Time) {
	t.Helper()
	seedCostAt(t, db, dev, issue, cost, instant)
	seedOutcomeAt(t, db, dev, issue, weight, 1.0, instant)
	if err := db.UpsertHierarchy(context.Background(), dev, team, "div", "org"); err != nil {
		t.Fatalf("UpsertHierarchy(%s,%s): %v", dev, team, err)
	}
}

func TestScoresCompare_TotalRankedIsTheConjunction(t *testing.T) {
	cases := []struct {
		name       string
		seedA      func(*testing.T, *store.DB, string, time.Time)
		seedB      func(*testing.T, *store.DB, string, time.Time)
		wantRanked bool
		why        string
	}{
		{
			name: "both windows ranked", seedA: seedRankedWindow, seedB: seedRankedWindow,
			wantRanked: true,
			why: "both sides clear the #133 floors, so the comparison rests on ranked " +
				"evidence at both ends and the delta is publishable",
		},
		{
			name:  "baseline ranked, selected below the floor",
			seedA: seedRankedWindow, seedB: seedUnrankedWindow, wantRanked: false,
			why: "THE reconstruction case: with the baseline published, `selected = " +
				"baseline + Δ` recovers the withheld ratio exactly. An OR, or a verdict " +
				"read off the baseline alone, would return true here",
		},
		{
			name:  "baseline below the floor, selected ranked",
			seedA: seedUnrankedWindow, seedB: seedRankedWindow, wantRanked: false,
			why: "the mirror case, and `% change = Δ/baseline` is a pure function of the " +
				"withheld baseline ratio — it leaks directly rather than additively",
		},
		{
			name:  "both windows below the floor",
			seedA: seedUnrankedWindow, seedB: seedUnrankedWindow, wantRanked: false,
			why: "neither end has evidence to rank",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			tc.seedA(t, db, "wa", winAInstant)
			tc.seedB(t, db, "wb", winBInstant)

			total := compareTotalRaw(t, h, compareURL())
			raw, ok := total["ranked"]
			if !ok {
				t.Fatalf("compare total carries no `ranked` key (#605) — the dashboard cannot "+
					"tell unranked from an older server that never said: %v", total)
			}
			var got bool
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode total.ranked: %v", err)
			}
			if got != tc.wantRanked {
				t.Errorf("total.ranked = %v, want %v — %s.\n  a.ranked=%s b.ranked=%s "+
					"a.tier=%s b.tier=%s a.cost=%s b.cost=%s",
					got, tc.wantRanked, tc.why,
					sideKey(t, total, "a", "ranked"), sideKey(t, total, "b", "ranked"),
					sideKey(t, total, "a", "tier"), sideKey(t, total, "b", "tier"),
					sideKey(t, total, "a", "total_cost_usd"), sideKey(t, total, "b", "total_cost_usd"))
			}

			// Criterion 2 of #502 still holds here: the wire is never scrubbed. The
			// engine keeps the number and only presentation withholds it, so a fix that
			// zeroed or omitted the quotient below the floor would be a DIFFERENT and
			// rejected option — and would silently break the ranked arms too.
			// The MAGNITUDE, not merely the key's presence. `sideKey` returns "" for an
			// absent key but "0" for a scrubbed one, so a presence check cannot detect the
			// scrubbing it claims to detect — and on the unranked fixtures it also pins the
			// fixture's own headline claim (28 points over $0.0001 really is ~2.8e8). Without
			// that, a seeding drift that put the outcomes outside the window would leave all
			// three unranked arms passing while measuring an EMPTY window rather than a thin
			// one.
			for _, side := range []string{"a", "b"} {
				raw := sideKey(t, total, side, "tier")
				if raw == "" {
					t.Errorf("total.%s has no tier key — an unranked aggregate keeps its exact "+
						"quotient; only its ranking authority is withheld (#136)", side)
					continue
				}
				var tier float64
				if err := json.Unmarshal([]byte(raw), &tier); err != nil {
					t.Errorf("total.%s.tier = %s, which is not a number", side, raw)
					continue
				}
				if tier <= 0 {
					t.Errorf("total.%s.tier = %v — the wire is never scrubbed below the floor; "+
						"the engine keeps the number and only presentation withholds it (#136). "+
						"A zeroed quotient is a DIFFERENT and rejected option", side, tier)
				}
			}
			// …and the below-floor fixtures really are the canonical thin window.
			if !tc.wantRanked {
				var aTier float64
				_ = json.Unmarshal([]byte(sideKey(t, total, "a", "tier")), &aTier)
				var bTier float64
				_ = json.Unmarshal([]byte(sideKey(t, total, "b", "tier")), &bTier)
				if aTier < 1e8 && bTier < 1e8 {
					t.Errorf("neither side of an unranked fixture reaches the ~2.8e8 this test "+
						"claims to measure (a=%v b=%v). The seeded outcomes may have fallen "+
						"outside the compare windows, in which case every unranked arm here is "+
						"measuring an EMPTY window, not a thin one", aTier, bTier)
				}
			}
		})
	}
}

// sideKey renders one field of one compare side as raw JSON text, for diagnostics.
func sideKey(t *testing.T, total map[string]json.RawMessage, side, key string) string {
	t.Helper()
	raw, ok := total[side]
	if !ok {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return string(m[key])
}

// TestScoresCompare_TotalRankedAgreesWithItsSides is the anti-drift arm. The two
// tests above could both pass against a `ranked` computed from something else that
// happens to correlate with the floors on these fixtures — a re-derived cost
// threshold, say. This one asserts the RELATION rather than the values: whatever
// the sides say, the total must be their conjunction, on every fixture.
//
// It is the property the whole #605 client rests on. renderCompareTotal READS this
// field instead of recomputing the AND (that recomputation is what option C would
// have been), so if the field ever stops being the conjunction, the compare card
// silently publishes a delta over an unranked window with nothing else objecting.
func TestScoresCompare_TotalRankedAgreesWithItsSides(t *testing.T) {
	for _, tc := range []struct {
		name         string
		seedA, seedB func(*testing.T, *store.DB, string, time.Time)
	}{
		{"ranked/ranked", seedRankedWindow, seedRankedWindow},
		{"ranked/unranked", seedRankedWindow, seedUnrankedWindow},
		{"unranked/ranked", seedUnrankedWindow, seedRankedWindow},
		{"unranked/unranked", seedUnrankedWindow, seedUnrankedWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			tc.seedA(t, db, "wa", winAInstant)
			tc.seedB(t, db, "wb", winBInstant)

			code, resp := getCompare(t, h, compareURL())
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if resp.Total == nil {
				t.Fatal("compare total missing")
			}
			want := resp.Total.A.Ranked && resp.Total.B.Ranked
			if resp.Total.Ranked != want {
				t.Errorf("total.ranked = %v but a.ranked && b.ranked = %v (a=%v b=%v) — the "+
					"derived verdict is no longer the conjunction of its inputs (#605)",
					resp.Total.Ranked, want, resp.Total.A.Ranked, resp.Total.B.Ranked)
			}
			// …and both arms must actually be exercised across the four subtests, or a
			// hardcoded constant would satisfy the relation. Pin the fixture's claim.
			if tc.name == "ranked/ranked" && !resp.Total.Ranked {
				t.Error("the ranked/ranked fixture did not produce a ranked total; every other " +
					"arm of this test would then pass against `Ranked: false` hardcoded")
			}
		})
	}
}

// TestScoresCompare_TeamRowsCarryRanked covers the same derived field on the
// k-anonymized `teams[]` rows. They share newTeamDeltaJSON with `total`, and that
// sharing is the point — but nothing read the field on THESE rows, so a build site
// that forgot it would have been invisible.
func TestScoresCompare_TeamRowsCarryRanked(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	// "thin": 3 developers in each window but $0.30 of summed spend — clear of the
	// k-floor, far below the #133 spend floor, in both windows.
	for _, dev := range []string{"t1", "t2", "t3"} {
		seedTeamMemberAt(t, db, dev, "a-"+dev, "thin", 0.10, 3, winAInstant)
		seedTeamMemberAt(t, db, dev, "b-"+dev, "thin", 0.10, 3, winBInstant)
	}
	// "solid": 3 developers, $6 each — clear of both floors in both windows.
	for _, dev := range []string{"s1", "s2", "s3"} {
		seedTeamMemberAt(t, db, dev, "a-"+dev, "solid", 6.0, 3, winAInstant)
		seedTeamMemberAt(t, db, dev, "b-"+dev, "solid", 6.0, 3, winBInstant)
	}

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Teams) == 0 {
		t.Fatalf("no teams[] rows in the anonymized compare response: %+v", resp)
	}
	seen := map[string]bool{}
	for _, row := range resp.Teams {
		seen[row.Team] = true
		if want := row.A.Ranked && row.B.Ranked; row.Ranked != want {
			t.Errorf("teams[%q].ranked = %v, want %v (a=%v b=%v)",
				row.Team, row.Ranked, want, row.A.Ranked, row.B.Ranked)
		}
	}
	if !seen["thin"] || !seen["solid"] {
		t.Fatalf("fixture did not produce both a below-floor and a ranked team row; got %v. "+
			"With only one of the two, a hardcoded verdict passes this test", seen)
	}
	for _, row := range resp.Teams {
		switch row.Team {
		case "thin":
			if row.Ranked {
				t.Errorf("teams[thin].ranked = true on $%.2f/$%.2f of summed spend across the two "+
					"windows; the #133 spend floor is $%.2f",
					row.A.TotalCostUSD, row.B.TotalCostUSD, 5.00)
			}
		case "solid":
			if !row.Ranked {
				t.Errorf("teams[solid].ranked = false on $%.2f/$%.2f across 3 outcomes per window "+
					"— both floors are cleared, so a mapper hardcoded to false would pass the "+
					"below-floor arm above and withhold every honest headline",
					row.A.TotalCostUSD, row.B.TotalCostUSD)
			}
		}
	}
}

// TestScoresCompare_RowOrderIsAlphabeticalNotByTIER pins an ordering that nothing
// asked for and everything depends on (#613, 2026-08-05).
//
// #613 withholds a below-floor row's digits. That withholding is only worth
// anything while POSITION IN THE LIST carries no information: rows are sorted by
// NAME, so a withheld row sits wherever its name puts it. Sort them by TIER — an
// obvious, well-meant "improvement" — and every withheld value is instantly bounded
// between its two visible neighbours, at every grain, on every surface.
//
// 🔴 That is the property worth understanding before touching this: NO DISPLAY
// OPTION CAN CLOSE AN ORDERING LEAK. The dashboard can withhold every digit, gate
// every dot and clip every label, and a TIER-sorted list still publishes an
// interval for each withheld row. The suppression and the sort order are one
// mechanism, and only one half of it lives in the code that looks like it.
//
// Behavioural, not a source pin on sort.Strings: a source pin proves the call is
// spelled a certain way, and the property is about the bytes on the wire.
func TestScoresCompare_RowOrderIsAlphabeticalNotByTIER(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)

	// 🔴 TIER is deliberately NON-MONOTONIC in name order, and that is the whole
	// load-bearing property: alphabetical order must coincide with NEITHER ascending
	// nor descending TIER, so no TIER-based sort in either direction can reproduce the
	// sequence asserted below.
	//
	// TIER = weighted_points / (total_cost_usd / 1000), so HIGHER spend is LOWER yield.
	// Equal outcomes with costs aaa=12, mmm=6, zzz=30 give TIERs of 250 / 500 / 100 in
	// name order — mmm best, aaa middle, zzz worst.
	//
	// ⚠️ An earlier draft of this comment said "names ascend while TIER descends". That
	// is MONOTONIC, and the control arm below t.Fatalf's on exactly that — the comment
	// described a fixture that would fail its own test. Do not "restore" it.
	// All three clear the #133 spend floor in both windows, so all are ranked and the
	// comparison is about ORDER alone, not about withholding.
	for _, team := range []struct {
		name string
		cost float64
	}{{"aaa", 12.0}, {"mmm", 6.0}, {"zzz", 30.0}} {
		for _, sfx := range []string{"1", "2", "3"} {
			dev := team.name + sfx
			seedTeamMemberAt(t, db, dev, "a-"+dev, team.name, team.cost, 3, winAInstant)
			seedTeamMemberAt(t, db, dev, "b-"+dev, team.name, team.cost, 3, winBInstant)
		}
	}

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Teams) != 3 {
		t.Fatalf("got %d team rows, want 3 — the fixture did not produce the three cohorts "+
			"this ordering assertion compares, so its verdict would be about nothing",
			len(resp.Teams))
	}

	names := make([]string, len(resp.Teams))
	tiers := make([]float64, len(resp.Teams))
	for i, row := range resp.Teams {
		names[i] = row.Team
		tiers[i] = row.B.TIER
	}

	// 🔴 THE CONTROL ARM, and it must run BEFORE the assertion it protects.
	//
	// It is computed from the fixture's NAME→TIER mapping, never from the returned
	// positions. A first draft asserted `tiers[0] < tiers[len-1]`, which reverses along
	// with the rows: fault-injecting a reversed sort made THIS arm fail instead of the
	// ordering assertion, so the mutant died for the wrong reason and the ordering
	// claim was never exercised (false-green ledger 13/18).
	//
	// The fixture is deliberately NON-MONOTONIC — alphabetical order coincides with
	// neither ascending nor descending TIER — so EVERY TIER-based sort, in either
	// direction, produces a different sequence from the one asserted below.
	inNameOrder := tiersInNameOrder(t, names, tiers)
	if monotonic(inNameOrder) {
		t.Fatalf("the fixture's TIERs are monotonic in name order (%v for %v), so an "+
			"ascending- or descending-TIER sort is INDISTINGUISHABLE from the alphabetical "+
			"one and the assertion below cannot exclude it", inNameOrder, names)
	}

	if !sort.StringsAreSorted(names) {
		t.Errorf("compare team rows came back in the order %v, which is not alphabetical. "+
			"Row ORDER is a publication channel: with rows sorted by TIER, a withheld row's "+
			"value is bounded between its visible neighbours and #613's withholding is void "+
			"— and no change to what the dashboard PRINTS can close that", names)
	}
}

// TestScoresCompare_DeveloperRowOrderIsAlphabeticalNotByTIER is the same property one
// grain down, and it needs its OWN handler.
//
// 🔴 It was first written as a second assertion inside the team-mode test above, and
// it was VACUOUS: an anonymized team-mode compare returns `developers: []` (measured,
// 0 rows), so sort.StringsAreSorted over an empty slice is trivially true. Reversing
// the developer sort in compare.go produced NO failure — the arm asserted nothing at
// all. Caught only by fault-injecting the thing it claimed to guard.
//
// Hence the non-empty assertion below, BEFORE the ordering one. A negative claim is
// worth nothing until the match set is proven non-empty (false-green ledger 4 and 27).
func TestScoresCompare_DeveloperRowOrderIsAlphabeticalNotByTIER(t *testing.T) {
	h, db := newTestHandler(t)

	// Same NON-MONOTONIC construction as the team test: costs aaa=12, mmm=6, zzz=30
	// give TIERs 250 / 500 / 100 in name order, so neither TIER direction reproduces
	// the alphabetical sequence. See that test for why monotonic would be useless.
	for _, dev := range []struct {
		name string
		cost float64
	}{{"aaa", 12.0}, {"mmm", 6.0}, {"zzz", 30.0}} {
		seedCostAt(t, db, dev.name, "a-"+dev.name, dev.cost, winAInstant)
		seedOutcomeAt(t, db, dev.name, "a-"+dev.name, 3, 1.0, winAInstant)
		seedCostAt(t, db, dev.name, "b-"+dev.name, dev.cost, winBInstant)
		seedOutcomeAt(t, db, dev.name, "b-"+dev.name, 3, 1.0, winBInstant)
	}

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// THE NON-EMPTY ARM. Without it this whole test passes on a response with no
	// developer rows in it, which is exactly how its first draft shipped green.
	if len(resp.Developers) != 3 {
		t.Fatalf("got %d developer rows, want 3 — with an empty list every ordering "+
			"assertion below is trivially true and this test proves nothing", len(resp.Developers))
	}

	names := make([]string, len(resp.Developers))
	tiers := make([]float64, len(resp.Developers))
	for i, row := range resp.Developers {
		names[i] = row.Developer
		tiers[i] = row.B.TIER
	}
	// The control arm, order-INDEPENDENT for the reason spelled out in the team test:
	// a positional control reverses along with the rows and steals the failure from the
	// assertion it exists to protect.
	inNameOrder := tiersInNameOrder(t, names, tiers)
	if monotonic(inNameOrder) {
		t.Fatalf("the fixture's TIERs are monotonic in name order (%v for %v), so a "+
			"TIER-based sort is indistinguishable from the alphabetical one", inNameOrder, names)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("compare developer rows came back in the order %v, which is not alphabetical "+
			"— the same ordering channel as the team rows, one grain down, where the "+
			"neighbours bounding a withheld value are closer together", names)
	}
}

// tiersInNameOrder returns the TIERs in ALPHABETICAL name order, independent of the
// order the server returned — which is the whole point: the control arm must not be
// computable from the sequence it is protecting. A positional control reverses along
// with the rows, so a reversed-sort mutant kills the CONTROL instead of the ordering
// assertion and dies for the wrong reason.
//
// It takes both slices so the two can never be handed to each other mismatched, and
// fails loudly rather than silently examining a shorter slice: a duplicate name would
// otherwise collapse the pairing and leave a 2-element result, which is ALWAYS
// monotonic and would fire the control arm with a message blaming monotonicity.
func tiersInNameOrder(t *testing.T, names []string, tiers []float64) []float64 {
	t.Helper()
	if len(names) != len(tiers) {
		t.Fatalf("tiersInNameOrder: %d names but %d tiers", len(names), len(tiers))
	}
	m := make(map[string]float64, len(names))
	for i, n := range names {
		m[n] = tiers[i]
	}
	if len(m) != len(names) {
		t.Fatalf("tiersInNameOrder: duplicate row names in %v — the pairing collapses and the "+
			"control arm below would examine a shorter, trivially monotonic slice", names)
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := make([]float64, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, m[n])
	}
	return out
}

// monotonic reports whether the values ascend or descend — i.e. whether SOME
// TIER-based sort would reproduce the alphabetical sequence, which is exactly the
// fixture degeneracy the control arm must reject. sort.Float64sAreSorted is
// non-strict, so an all-equal fixture reads as monotonic and fails closed.
func monotonic(v []float64) bool {
	return sort.Float64sAreSorted(v) || sort.IsSorted(sort.Reverse(sort.Float64Slice(v)))
}

// TestScoresCompare_RankedImpliesPresentAndSampled pins the SERVER invariant the
// dashboard's shared-scale claim rests on (#613).
//
// 🔴 Why this lives here and not in the dashboard. The compare chart's denominator
// (`compareScale`/`rankedTier` in dashboard.js) admits a side on `ranked` ALONE, while
// publication and plotting both gate on `has && ranked` — where `has` additionally
// requires `sample_n > 0` on a developer row. So the denominator's gate is strictly
// WEAKER than the publish gate.
//
// That matters because every published dot renders at `printedTier / scale` as an
// inline percentage to three decimals, so `scale` inverts out of any visible row. A
// side that entered the denominator while being neither plotted nor printed would
// hand its TIER back through rows that ARE published — the exact channel #613 closed
// for the withheld row's own dot.
//
// The branch's claim that "publication granularity equals plotting granularity" is
// therefore true only because the server guarantees `ranked ⟹ present && sample_n >=
// MinRankedOutcomes`. That guarantee was ASSUMED, not asserted, anywhere. It is
// asserted here, so the day it stops holding this fails rather than the chart quietly
// becoming invertible.
func TestScoresCompare_RankedImpliesPresentAndSampled(t *testing.T) {
	h, db := newTestHandler(t)

	// A ranked developer, an unranked-by-spend one, and one with outcomes in only a
	// single window — so the loop below sees ranked and unranked sides of both kinds.
	// Ranking needs BOTH #133 floors: sample_n >= MinRankedOutcomes (3 SEPARATE
	// outcomes, one per issue) and total_cost_usd >= the spend floor. Seeding one
	// outcome with weight 3 satisfies neither — the control arm at the end caught
	// exactly that on the first draft of this fixture.
	for _, i := range []string{"1", "2", "3"} {
		seedCostAt(t, db, "rich", "a-rich"+i, 15.0, winAInstant)
		seedOutcomeAt(t, db, "rich", "a-rich"+i, 3, 1.0, winAInstant)
		seedCostAt(t, db, "rich", "b-rich"+i, 15.0, winBInstant)
		seedOutcomeAt(t, db, "rich", "b-rich"+i, 3, 1.0, winBInstant)
		// Present in BOTH windows but far below the spend floor: ranked=false with
		// real data, which is the state the chart withholds rather than treats as absent.
		seedCostAt(t, db, "thin", "a-thin"+i, 0.02, winAInstant)
		seedOutcomeAt(t, db, "thin", "a-thin"+i, 1, 1.0, winAInstant)
		seedCostAt(t, db, "thin", "b-thin"+i, 0.02, winBInstant)
		seedOutcomeAt(t, db, "thin", "b-thin"+i, 1, 1.0, winBInstant)
		// Present in the SELECTED window only: the one-sided case, where a ranked side
		// sits beside a side that is absent rather than withheld.
		seedCostAt(t, db, "onesided", "b-one"+i, 12.0, winBInstant)
		seedOutcomeAt(t, db, "onesided", "b-one"+i, 3, 1.0, winBInstant)
	}
	// 🔴 THE ROW THAT MAKES THIS TEST ABLE TO FAIL. Plenty of spend, but only ONE
	// outcome — so it clears the #133 cost floor and misses the sample floor. Without
	// it, no row in this fixture could ever become "ranked with sample_n < 3", and
	// fault-injecting MinRankedOutcomes out of the ranking predicate left the test
	// GREEN: it asserted a property nothing could violate. Measured, not assumed.
	seedCostAt(t, db, "sparse", "a-sparse", 30.0, winAInstant)
	seedOutcomeAt(t, db, "sparse", "a-sparse", 3, 1.0, winAInstant)
	seedCostAt(t, db, "sparse", "b-sparse", 30.0, winBInstant)
	seedOutcomeAt(t, db, "sparse", "b-sparse", 3, 1.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Developers) == 0 {
		t.Fatal("no developer rows — the invariant below would hold vacuously")
	}

	ranked := 0
	for _, row := range resp.Developers {
		for _, s := range []struct {
			win     string
			side    scoreSideJSON
			present bool
		}{
			{"baseline", row.A, row.PresentA},
			{"selected", row.B, row.PresentB},
		} {
			if !s.side.Ranked {
				continue
			}
			ranked++
			if !s.present {
				t.Errorf("developers[%q].%s is RANKED but present_%s is false — it would enter "+
					"the chart's shared denominator while never being plotted or printed, and "+
					"every published dot inverts that denominator",
					row.Developer, s.win, s.win[:1])
			}
			if n := s.side.SampleN; n < scoring.MinRankedOutcomes {
				t.Errorf("developers[%q].%s is RANKED on sample_n=%d (< MinRankedOutcomes=3) — "+
					"the dashboard treats ranked-with-no-samples as absent, so this side would "+
					"set the denominator without ever appearing on screen",
					row.Developer, s.win, n)
			}
		}
	}
	// The control arm: without a ranked side the loop body never runs and every
	// assertion above is vacuously satisfied.
	if ranked == 0 {
		t.Fatal("the fixture produced NO ranked side, so this test asserted nothing at all")
	}
}
