package store

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestREADMESamplePriceTableMatchesEmbedded pins the one line of the public README
// that quotes a value the binary computes.
//
// 🔴 Why this exists. README.md shows a sample `tierd score` run whose first line is
// the price-table stamp. Measured 2026-08-05, it read
// `version 8, 2026-07-22, 76 models` while the embedded table was
// `version 9, 2026-07-26, 77 models` — two revisions stale, in the file that is the
// FIRST thing a visitor to the published repository reads.
//
// Nothing caught it, and nothing could: the docs drift gate covers `docs/` → its
// generated HTML, and the number-drift gate that exists lives in the marketing-site
// repo and scans that repo's HTML (#632). Neither reads this file. So the repo's
// most-read published number was checked by nobody, and it was found only because
// #635 said to re-check the README by hand — which is not a gate, it is a person
// remembering.
//
// 🔴 THE EXPECTED LINE IS BUILT FROM THE PRINTER'S OWN FORMAT STRING, not hand-copied.
// The first version of this guard compared the README against ActivePriceTableInfo()
// while the README actually quotes a `fmt.Fprintf` in cmd/tierd — connected by
// nothing. Measured: mutating BOTH print sites from `version %d` to `v%d` changed what
// the binary prints, left the README stale, and produced ZERO additional failures
// across the entire suite. The comment claimed it "asks the program"; it asked a
// struct. Now store.EmbeddedPriceStampFormat is the single definition both the
// printers and this test use, so a format change breaks this at compile-coupled range.
func TestREADMESamplePriceTableMatchesEmbedded(t *testing.T) {
	// 🔴 SET the embedded table, do not merely assume it. `priceTable` and
	// `activePriceTableInfo` are package globals that four helpers in this package swap
	// (installPriceTable, loadSubscriptionTestTable, LoadPriceTable, ...), and Go runs
	// test files in alphabetical order — prices_test.go before this one. Every swapper
	// restores via t.Cleanup today, so this passes; but subscription_test.go states the
	// rule outright: a test that asserts about the default must also SET it, or it is
	// really asserting about whatever ran before it.
	//
	// Measured with one swapper's t.Cleanup removed, this guard failed telling a
	// maintainer their CORRECT README was stale. A guard that blames the wrong file is
	// worse than no guard, because it gets acted on.
	loadDefaultPriceTable(t)

	// The README lives at the repo root; tests run in the package directory.
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("cannot read README.md: %v", err)
	}
	readme := string(raw)

	info := ActivePriceTableInfo()
	want := fmt.Sprintf(EmbeddedPriceStampFormat, info.Version, info.EffectiveDate, info.ModelCount)

	// ANCHORED to whole lines. Unanchored, a mention of the stamp anywhere in PROSE
	// satisfied the guard — measured: deleting the line from the sample block entirely
	// and leaving a correct copy in a sentence at the end of the file PASSED, which
	// defeats the "line is missing" arm below. The README pins this as sample OUTPUT,
	// so it must appear as its own line.
	stampRe := regexp.MustCompile(`(?m)^price table: embedded default \(version \d+, \d{4}-\d{2}-\d{2}, \d+ models\)$`)
	found := stampRe.FindAllString(readme, -1)

	if len(found) == 0 {
		// NOT a skip, and deliberately fatal: the line vanishing is indistinguishable
		// from this guard being deleted, and a sample output with no price-table stamp
		// is itself a drift from what the binary prints.
		t.Fatalf("README.md no longer contains a price-table stamp line matching what the "+
			"binary prints (%q). Either the sample output was changed to something the "+
			"program does not emit, or this guard has quietly stopped guarding anything", want)
	}

	// EXACTLY ONE. FindSubmatch took the first match, so a stale duplicate LATER in the
	// file was invisible — measured: appending a second, two-revisions-stale stamp after
	// the correct one passed. A second stamp in the README is itself drift, because only
	// one of them can be the sample.
	if len(found) != 1 {
		t.Errorf("README.md contains %d price-table stamp lines, want exactly 1:\n  %s\n"+
			"Only one can be the sample output; a second is stale text that a first-match "+
			"check would never look at.", len(found), strings.Join(found, "\n  "))
	}

	for _, got := range found {
		if got != want {
			t.Errorf("README.md quotes a STALE price table.\n  README:   %s\n  binary:   %s\n"+
				"This is the first line a visitor to the public mirror sees under \"Output "+
				"shape\". Update the README in the same commit as prices.yaml — the same rule "+
				"internal/store/prices.go already states for docs/reference-price-table.md.",
				got, want)
		}
	}
}

// TestReferencePriceDocMatchesEmbedded closes the asymmetry the README guard exposed.
//
// internal/store/prices.go states the rule that `prices.yaml` and
// `docs/reference-price-table.md` must change in the SAME commit — and nothing enforced
// it. No test read that file and no make target covered it, and the number-drift gate
// that exists is in another repo entirely (#632). So the branch that gated the README
// would have left the
// rule's ORIGINAL subject ungated, which is the kind of asymmetry that reads as
// "covered" from a distance.
//
// The doc deliberately carries no independent version number — it says so itself, so
// the two "cannot drift apart". That is a claim about intent; this is the check.
func TestReferencePriceDocMatchesEmbedded(t *testing.T) {
	loadDefaultPriceTable(t) // assert about the EMBEDDED table, not whatever ran before

	raw, err := os.ReadFile("../../docs/reference-price-table.md")
	if err != nil {
		t.Fatalf("cannot read docs/reference-price-table.md: %v", err)
	}
	doc := string(raw)
	info := ActivePriceTableInfo()

	// The doc describes the table in prose, so pin the two facts rather than a layout:
	// which version it claims to describe, and the effective date it stamps.
	wantVersion := fmt.Sprintf("`version: %d`", info.Version)
	if !strings.Contains(doc, wantVersion) {
		t.Errorf("docs/reference-price-table.md never mentions %s, so it is describing a "+
			"different table than the one tierd embeds (version %d, %s). prices.go states "+
			"that this file changes in the SAME commit as prices.yaml; until now nothing "+
			"checked it.", wantVersion, info.Version, info.EffectiveDate)
	}
	if !strings.Contains(doc, info.EffectiveDate) {
		t.Errorf("docs/reference-price-table.md never mentions the embedded table's "+
			"effective_date %s — the narrative companion has drifted from the table it "+
			"claims to describe", info.EffectiveDate)
	}
}

// TestREADMEWeightTableMatchesGitHeuristic pins the no-label weight rule the README
// publishes against the function that actually computes it.
//
// 🔴 Why. README.md published `min(8, max(0.5, ceil(log2(lines + files×10 + 1))))`
// long after GitHeuristic stopped being a log2 formula — store.go says outright that a
// bucketed step function "replaced the old log2 formula". For a 100-line, 4-file PR the
// README's formula gives 8 and the code returns 3.0.
//
// That is not a footnote: weight is the TIER NUMERATOR, so the front page of a
// measurement tool overstated the metric by 2.7x on a routine change. The same defect
// class as the stale price stamp above, but with arithmetic consequences rather than
// cosmetic ones — which is why it gets its own guard rather than a comment.
func TestREADMEWeightTableMatchesGitHeuristic(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("cannot read README.md: %v", err)
	}
	readme := string(raw)

	// Ask the function, not the prose. Each probe is a (lines, files) pair chosen to
	// land inside one bucket, so a boundary change moves the answer and fails here.
	for _, tc := range []struct{ lines, files int }{
		{5, 0}, {40, 1}, {100, 4}, {500, 10}, {5000, 50},
	} {
		want := GitHeuristic(tc.lines, tc.files)
		// The README states the rule as buckets on `effort`; assert the bucket VALUE it
		// publishes for this effort is the one the function returns.
		effort := tc.lines + tc.files*10
		if !strings.Contains(readme, fmtWeight(want)) {
			t.Errorf("README.md never mentions weight %s, which GitHeuristic returns for "+
				"effort %d (%d lines, %d files). The published rule and the implemented one "+
				"have drifted, and weight is the TIER numerator.",
				fmtWeight(want), effort, tc.lines, tc.files)
		}
	}

	// The retired formula must not come back. It is the specific wrong text that shipped.
	if strings.Contains(readme, "log2(") {
		t.Error("README.md publishes a log2 weight formula again. GitHeuristic is a bucketed " +
			"step function on `effort = lines + files*10`, and store.go records that the " +
			"buckets REPLACED the log2 form — publishing the old one overstates the metric's " +
			"numerator (measured: 8 vs an actual 3.0 for a 100-line, 4-file change)")
	}
}

func fmtWeight(w float64) string {
	return strconv.FormatFloat(w, 'f', -1, 64)
}
