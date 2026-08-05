package scoring

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// --- #606: FormatReport's TEAM TOTAL honours the ranking floor ---------------
//
// FormatReport was the THIRD consumer of the org TIER headline and the last
// unguarded one. It re-derived `totalPoints / (totalCost / tierCostScaleUSD)`
// inline instead of reading RollupTeam, so it structurally could not see `Ranked`
// no matter how correct the engine became — while the per-developer rows three
// lines above it printed the "--- below ranking floor ---" separator correctly.
// One output, contradicting itself.
//
// Generalisable lesson, and the reason these tests exist rather than a comment: a
// field added to a struct reaches only the consumers that READ the struct. Two of
// the three re-derived the quotient. Enumerate re-derivation sites, not struct
// readers.

// reportTotalLine returns the TEAM TOTAL row of a report. Fatal when absent — a
// helper that returned "" would make every "does not contain" assertion below pass
// against a report that has no total row at all.
func reportTotalLine(t *testing.T, out string) string {
	t.Helper()
	at := strings.Index(out, "TEAM TOTAL ")
	if at < 0 {
		t.Fatalf("no TEAM TOTAL row in the report:\n%s", out)
	}
	return strings.SplitN(out[at:], "\n", 2)[0]
}

// reportTotalTIER returns the TIER column of the TEAM TOTAL row. The four numeric
// columns are positional (TIER, cost, points, fidelity), so it reads the first of
// the last four whitespace-separated fields rather than pattern-matching a number
// that could bleed in from another column.
func reportTotalTIER(t *testing.T, out string) string {
	t.Helper()
	return reportTotalCols(t, reportTotalLine(t, out))[0]
}

// belowFloorTotal is the canonical #502 window at org scale: 28 weighted points
// against $0.0001 of measured spend. TIER is a true 2.8e8 — a real quotient over a
// denominator too small to mean anything. It is below the floor on BOTH conditions
// the summed gate checks (2 outcomes < 3, $0.0001 < $5.00).
func belowFloorTotal() []DeveloperScore {
	return []DeveloperScore{
		{Developer: "alice", TIER: 2.8e8, WeightedPoints: 28, TotalCostUSD: 0.0001,
			CoveragePercent: 100, SampleN: 2},
	}
}

// rankedTotal clears both floors: 4 outcomes, $12 of measured spend.
func rankedTotal() []DeveloperScore {
	return []DeveloperScore{
		{Developer: "alice", TIER: 500, WeightedPoints: 4, TotalCostUSD: 8,
			CoveragePercent: 100, SampleN: 2, Ranked: true},
		{Developer: "bob", TIER: 500, WeightedPoints: 2, TotalCostUSD: 4,
			CoveragePercent: 100, SampleN: 2, Ranked: true},
	}
}

// TestFormatReport_TeamTotalWithheldBelowFloor is the defect arm. The number must
// not be printed AT ALL — not muted, not bracketed, not in a footnote. The
// per-developer rows can print a below-floor figure because the separator above
// them carries the verdict; TEAM TOTAL has no list around it.
func TestFormatReport_TeamTotalWithheldBelowFloor(t *testing.T) {
	out := FormatReport(belowFloorTotal(), "2026-01-01", AggregationDeveloper)

	if got := reportTotalTIER(t, out); got != "—" {
		t.Errorf("TEAM TOTAL TIER column = %q, want %q — 28 points over $0.0001 is 2.8e8, a true "+
			"quotient over a denominator too small to mean anything, and #502 withholds it "+
			"rather than printing it faintly.\nreport:\n%s", got, "—", out)
	}
	// Belt and braces, scoped to the aggregate FOOTER — from the TEAM TOTAL row to
	// the end. The org ratio must not reappear in any spelling a %.1f/%g/%e of 2.8e8
	// could produce: a withheld column beside a footnote quoting the number is not a
	// withheld number.
	//
	// Deliberately NOT the whole report. alice's own row above the fold prints the
	// same 2.8e8, and that is correct: a per-developer row below the separator states
	// its number, exactly as the yield bars do. The distinction #502 turns on is that
	// a headline read alone is not a row in a list.
	footer := out[strings.Index(out, "TEAM TOTAL "):]
	for _, spelling := range []string{"280000000", "2.8e+08", "2.8e8", "2.80000e+08"} {
		if strings.Contains(footer, spelling) {
			t.Errorf("the below-floor ratio appears in the aggregate footer as %q — the column is "+
				"withheld but the number is published beside it:\n%s", spelling, footer)
		}
	}
	// Criterion 4: the measured inputs stay on screen. Withholding the evidence too
	// would be its own dishonesty — the reader must see how thin it is.
	line := reportTotalLine(t, out)
	for _, want := range []string{"0.0001", "28.0"} {
		if !strings.Contains(line, want) {
			t.Errorf("TEAM TOTAL row no longer carries the measured input %q: %q", want, line)
		}
	}
	// …and the reason is STATED. A bare em dash with no cause trains a reader to
	// read a withheld number as a broken one.
	//
	// Asserted against the FOOTER, not the whole report. In developer mode the
	// per-developer floor separator prints the same constants a few lines up, so a
	// whole-report scan for either the phrase or the numbers is satisfied by that
	// separator and says nothing about this row. Measured: with the assertions
	// unscoped, hardcoding "n < 99 outcomes or < $99 cost" into the TEAM TOTAL
	// reason line left the scoring suite green.
	if !strings.Contains(footer, "below the ranking floor") {
		t.Errorf("the withheld TEAM TOTAL states no reason:\n%s", footer)
	}
	// The reason is built from the constants, so it can never cite a floor that no
	// longer exists — the same discipline the per-developer separator follows.
	wantFloor := fmt.Sprintf("n < %d outcomes or < $%.0f cost", MinRankedOutcomes, MinRankedCostUSD)
	if !strings.Contains(footer, wantFloor) {
		t.Errorf("the withheld TEAM TOTAL does not name the floor from the engine constants "+
			"(want %q):\n%s", wantFloor, footer)
	}
}

// TestFormatReport_TeamTotalPublishedAboveFloor is the paired control. Without it
// a gate hardcoded to "always withhold" would satisfy the test above and delete
// every honest headline the report exists to print.
func TestFormatReport_TeamTotalPublishedAboveFloor(t *testing.T) {
	out := FormatReport(rankedTotal(), "2026-01-01", AggregationDeveloper)

	// 6 points over $12 -> TIER = 6/(12/1000) = 500.0.
	if got := reportTotalTIER(t, out); got != "500.0" {
		t.Errorf("TEAM TOTAL TIER column = %q, want %q on 4 outcomes and $12 of spend — both "+
			"floors are cleared.\nreport:\n%s", got, "500.0", out)
	}
	if strings.Contains(out, "below the ranking floor") {
		t.Errorf("a ranked TEAM TOTAL carries a below-floor caveat:\n%s", out)
	}
}

// TestFormatReport_AnonymizedTeamTotalHonoursTheFloor is where this matters most.
// In an anonymized mode (#185 team / #270 division) the per-developer loop prints
// nothing, so TEAM TOTAL is THE ENTIRE REPORT — there is no separator, no ordering
// and no neighbouring row to carry the verdict. If the floor does not reach this
// line, it reaches nothing at all in that mode.
func TestFormatReport_AnonymizedTeamTotalHonoursTheFloor(t *testing.T) {
	for _, mode := range []AggregationMode{AggregationTeam, AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			out := FormatReport(belowFloorTotal(), "2026-01-01", mode)

			if strings.Contains(out, "alice") {
				t.Fatalf("fixture broken: an anonymized report named a developer, so this test is "+
					"no longer measuring the rows-suppressed path:\n%s", out)
			}
			if got := reportTotalTIER(t, out); got != "—" {
				t.Errorf("anonymized TEAM TOTAL TIER = %q, want %q. This row is the whole report "+
					"in this mode, so an unguarded ratio here is not one line among many — it is "+
					"the only thing the reader sees.\nreport:\n%s", got, "—", out)
			}
			if strings.Contains(out, "280000000") {
				t.Errorf("the below-floor ratio is published in an anonymized report:\n%s", out)
			}
		})
	}
}

// TestFormatReport_ReadsTheRollupRatherThanReSumming is the STRUCTURAL half of
// #606, and it has to be a source assertion rather than a behavioural one.
//
// The defect was never a wrong number — the inline arithmetic computed the same
// quotient the rollup does. It was that the arithmetic lived SOMEWHERE ELSE, so a
// verdict added to the rollup could not reach it. Any correct re-derivation
// therefore produces byte-identical output, and no comparison of printed text
// against RollupTeam values can tell the two apart. Measured: replacing
// `team := RollupTeam("", sorted)` with an inline re-summation that recomputes
// points, cost, coverage, TIER and `Ranked` left the whole scoring suite green.
//
// So this reads the function's source. Crude, and deliberately so: the property
// being defended is "this consumer READS the struct", which is a property of the
// text, not of the output.
func TestFormatReport_ReadsTheRollupRatherThanReSumming(t *testing.T) {
	body := formatReportSource(t)

	if !strings.Contains(body, `RollupTeam("", sorted)`) {
		t.Fatal("FormatReport no longer calls RollupTeam. The TEAM TOTAL row must READ the " +
			"shared rollup — a local re-derivation cannot see `Ranked`, which is exactly how " +
			"this consumer missed #502 entirely (#606)")
	}
	// The banned shape: the quotient rebuilt from local sums. tierCostScaleUSD is the
	// tell — it appears in the report only as the FORMULA footer's prose, never as
	// arithmetic, once the rollup owns the division.
	if strings.Contains(body, "tierCostScaleUSD") {
		t.Error("FormatReport divides by tierCostScaleUSD itself; the TEAM TOTAL quotient must " +
			"come from RollupTeam, or a future verdict on the rollup will not reach this row " +
			"(#606)")
	}
	// …and the local accumulators the old row summed into are gone. Their return is
	// the re-derivation coming back under new names, which the ban above would miss
	// if the division moved into a helper.
	for _, banned := range []string{"totalPoints", "totalCost", "totalRealtime"} {
		if strings.Contains(body, banned) {
			t.Errorf("FormatReport re-introduces the local accumulator %q — the aggregate is "+
				"RollupTeam's, not a second summation that happens to agree today (#606)", banned)
		}
	}
	// The verdict is read, and it gates the printed column.
	if !strings.Contains(body, "team.Ranked") {
		t.Error("FormatReport never reads the rollup's Ranked verdict, so the TEAM TOTAL row " +
			"publishes a below-floor quotient while the developer rows above it print the " +
			"floor separator — one output contradicting itself (#606)")
	}
}

// formatReportSource returns FormatReport's body from engine.go. Fatal when it
// cannot be located, so a rename turns the assertions above red rather than vacuous.
func formatReportSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	const header = "func FormatReport(scores []DeveloperScore, since string, mode AggregationMode) string {"
	at := strings.Index(string(src), header)
	if at < 0 {
		t.Fatal("FormatReport not found in engine.go — the #606 structural guard cannot be scoped")
	}
	body := string(src)[at:]
	// Up to the next top-level declaration.
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	// Comments quote the defect they removed, verbatim; a guard that reads its own
	// documentation fails on the explanation of the fix.
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// TestFormatReport_TeamTotalMatchesRollupTeam is the behavioural half: whatever the
// row prints agrees with the rollup, field by field and positionally.
func TestFormatReport_TeamTotalMatchesRollupTeam(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scores []DeveloperScore
	}{
		{"ranked", rankedTotal()},
		{"below floor", belowFloorTotal()},
		{"zero cost", []DeveloperScore{{Developer: "carol", WeightedPoints: 2, SampleN: 4}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := RollupTeam("", tc.scores)
			line := reportTotalLine(t, FormatReport(tc.scores, "2026-01-01", AggregationDeveloper))
			cols := reportTotalCols(t, line)

			// Cost, points and fidelity print in every state, ranked or not, and each is
			// compared POSITIONALLY. A strings.Contains over the whole line would match
			// "6.0" inside a "16.0" one column over and report agreement that is not there.
			//
			// Fidelity is in this list deliberately: CoveragePercent is the one field whose
			// derivation actually MOVED in #606 — out of the interleaved print loop and
			// into RollupTeam's own second pass over the same slice — so it is the field
			// most able to drift, and it was the one this loop originally omitted.
			for _, f := range []struct {
				what string
				col  int
				text string
			}{
				{"total_cost_usd", 1, trimNum(want.TotalCostUSD, 4)},
				{"weighted_points", 2, trimNum(want.WeightedPoints, 1)},
				{"fidelity", 3, trimNum(want.CoveragePercent, 0) + "%"},
			} {
				if cols[f.col] != f.text {
					t.Errorf("TEAM TOTAL %s = %q, RollupTeam says %q — the row is re-summing its "+
						"own inputs again (#606). Line: %q", f.what, cols[f.col], f.text, line)
				}
			}
			// The TIER column agrees with the rollup's verdict AND its value.
			if got := cols[0]; want.Ranked {
				if got != trimNum(want.TIER, 1) {
					t.Errorf("TEAM TOTAL TIER = %q, RollupTeam says %q", got, trimNum(want.TIER, 1))
				}
			} else if got != "—" {
				t.Errorf("RollupTeam reports ranked=false but the row printed %q — the report is "+
					"not reading the verdict it claims to read", got)
			}
		})
	}
}

// reportTotalCols returns the TEAM TOTAL row's four numeric columns (TIER, cost,
// points, fidelity) POSITIONALLY. Column comparison rather than a substring scan:
// strings.Contains(line, "6.0") matches the "16.0" sitting one column over and
// reports an agreement that is not there.
func reportTotalCols(t *testing.T, line string) [4]string {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 4 {
		t.Fatalf("TEAM TOTAL row malformed: %q", line)
	}
	var cols [4]string
	copy(cols[:], fields[len(fields)-4:])
	return cols
}

// trimNum formats a float the way the report's columns do, without their padding.
func trimNum(v float64, prec int) string {
	return strings.TrimSpace(fmt.Sprintf("%.*f", prec, v))
}
