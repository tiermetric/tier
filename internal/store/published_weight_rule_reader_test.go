package store

import (
	"sort"
	"strings"
	"testing"
)

// 🔴 Why a guard needs its own selftest.
//
// FOUR successive designs of TestPublishedWeightRuleMatchesGitHeuristic passed their
// own run while checking the wrong thing, and adversarial review broke each one. Every
// defect was in the READER, never in a document:
//
//   - "some line pairs them correctly" passed a corrupted table row, because a correct
//     statement elsewhere in the file satisfied it;
//   - skipping candidates above the maximum weight read `| ≤ 200 | 10.0 |` as no claim
//     at all rather than an overstated one;
//   - the terminal bucket was checked by PRESENCE, so `otherwise → 9` passed;
//   - "first number after the threshold" reddened CORRECT prose ($0.15, 42%, v1.2);
//   - "rightmost numeric cell" then accused a CORRECT table the moment a column of
//     bare numbers (a year, a sample count) was added to its right;
//   - and reading `otherwise`/`else` in free prose — ordinary English discourse words —
//     turned "otherwise the score is 3 buckets up" into a published terminal weight.
//
// A pass/fail run cannot distinguish any of those from a working guard; every one was
// green. So the arms are exercised over synthetic DOCUMENTS — region detection
// included — and each case asserts WHICH finding fires, in the style of a
// private-only selftest target. Cases marked ⚠️ are the exact inputs that
// defeated an earlier design. They are regression pins, not examples.

// claimsInDoc runs the real region detection over a document and returns everything the
// reader believes it says. Driving whole documents (not bare lines) is deliberate: the
// scope decision lives in docContexts, so a line-level helper would test the half of
// the reader that was never the problem.
func claimsInDoc(doc string) ([]bucket, []float64) {
	lines := strings.Split(doc, "\n")
	ctxs := docContexts(lines)
	var bs []bucket
	var ts []float64
	for i, line := range lines {
		bs = append(bs, bucketClaims(line, ctxs[i])...)
		ts = append(ts, terminalClaims(line, ctxs[i])...)
	}
	return bs, ts
}

// bucketTable wraps rows in a header this guard recognises.
func bucketTable(rows ...string) string {
	return "| `effort = lines + files × 10` | Weight |\n| --- | ---: |\n" +
		strings.Join(rows, "\n") + "\n"
}

func TestBucketClaims(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      []bucket
	}{
		{"table row", bucketTable("| `≤ 200` | 3.0 |"), []bucket{{200, 3}}},
		{
			"fenced code block",
			"```\neffort = lines + files × 10\n\neffort ≤ 200   → 3\n```\n",
			[]bucket{{200, 3}},
		},
		{
			"whole rule inside one weight cell (the README shape)",
			"| Label | Weight |\n|---|---|\n| _(none)_ | bucketed on `effort = lines + " +
				"files×10`: `<=15` → 0.5, `<=60` → 1.0 |\n",
			[]bucket{{15, 0.5}, {60, 1}},
		},
		{
			"worked-example line",
			"`effort = 50 + 3 × 10 = 80`, which falls in the `≤ 200` bucket → weight **3.0**.\n",
			[]bucket{{200, 3}},
		},
		{
			"⚠️ grouped threshold reads as 1000, not 1",
			"```\neffort = lines + files × 10\neffort ≤ 1,000 → 5.0\n```\n",
			[]bucket{{1000, 5}},
		},
		{
			"a boundary LIST states no weights",
			"```\neffort = lines + files*10\n(`≤ 15`, `≤ 60`, `≤ 200`, `≤ 1000`, else)\n```\n",
			nil,
		},

		// ---- ⚠️ NEW RED-1: a correct table gaining a numeric column ---------------
		{
			"⚠️ a year column to the right of Weight is not the weight",
			"| `effort = lines + files × 10` | Weight | Since |\n| --- | ---: | --- |\n" +
				"| `≤ 15` | 0.5 | 2024 |\n",
			[]bucket{{15, 0.5}},
		},
		{
			"⚠️ a sample-count column is not the weight",
			"| `effort = lines + files × 10` | Weight | Samples |\n| --- | ---: | --- |\n" +
				"| `≤ 200` | 3.0 | 128 |\n",
			[]bucket{{200, 3}},
		},
		{
			"⚠️ a bare version column is not the weight",
			"| `effort = lines + files × 10` | Weight | Rev |\n| --- | ---: | --- |\n" +
				"| `≤ 200` | 3.0 | 1.2 |\n",
			[]bucket{{200, 3}},
		},
		{
			"⚠️ an example column to the LEFT cannot shield a wrong weight",
			"| `effort = lines + files × 10` | Example | Weight |\n| --- | --- | ---: |\n" +
				"| `≤ 200` | 3 files | 10.0 |\n",
			[]bucket{{200, 10}},
		},
		{
			"⚠️ a bare-number example column is not read as the weight",
			"| `effort = lines + files × 10` | Example | Weight |\n| --- | --- | ---: |\n" +
				"| `≤ 200` | 50 | 3.0 |\n",
			[]bucket{{200, 3}},
		},

		// ---- free prose is out of scope BY DESIGN --------------------------------
		{
			"⚠️ prose with a dollar amount is not read",
			"When effort is ≤ 200 the cost is $0.15 per run.\n",
			nil,
		},
		{
			"⚠️ prose with a percentage is not read",
			"Runs with effort ≤ 60 make up 42% of the corpus.\n",
			nil,
		},
		{
			"⚠️ an unrelated page is not read",
			"when the net `actual_paid_usd` is ≤ 0 (credit memos exceeded invoices), " +
				"`SpendLeverage` stays 0.\n",
			nil,
		},
		{
			"a table with no weight column is not read",
			"| effort | Notes |\n| --- | --- |\n| `≤ 200` | 7.0 |\n",
			nil,
		},
		{
			"a code block that never names the rule is not read",
			"```\nfoo ≤ 200 → 7\n```\n",
			nil,
		},
		{
			"issue reference is not a weight",
			"```\neffort = lines + files × 10\neffort ≤ 200 (see #132)\n```\n",
			nil,
		},
		{
			"fractional bound is not an effort threshold",
			"```\neffort budget\nquality >= 0.10 AND quality <= 1.00\n```\n",
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := claimsInDoc(tc.doc)
			if len(got) != len(tc.want) {
				t.Fatalf("claims = %v, want %v\ndoc:\n%s", got, tc.want, tc.doc)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("claims[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestTerminalClaims(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      []float64
	}{
		{"table row", bucketTable("| otherwise | 8.0 |"), []float64{8}},
		{
			"fenced code block",
			"```\neffort = lines + files × 10\notherwise      → 8\n```\n",
			[]float64{8},
		},
		{
			"inside the README weight cell",
			"| Label | Weight |\n|---|---|\n| _(none)_ | bucketed on `effort = lines + " +
				"files×10`: `<=1000` → 5.0, else 8.0 |\n",
			[]float64{8},
		},
		{"a wrong terminal is reported", bucketTable("| otherwise | 9.0 |"), []float64{9}},

		// ---- ⚠️ NEW RED-2: `otherwise`/`else` are ordinary English ----------------
		{
			"⚠️ prose discourse `otherwise` is not read",
			"The effort proxy is capped; otherwise the score is 3 buckets up.\n",
			nil,
		},
		{
			"⚠️ prose discourse `else` is not read",
			"effort must be an int, or else the 4 buckets are meaningless.\n",
			nil,
		},
		{
			"⚠️ the real how-it-works list item is not read",
			"2. **Git heuristic** otherwise: `effort = lines + files*10`, bucketed onto " +
				"that same 0.5 / 1.0 / 3.0 / 5.0 / 8.0 scale (`≤ 15`, `≤ 60`, `≤ 200`, " +
				"`≤ 1000`, else) — a step function, not a continuous formula.\n",
			nil,
		},
		{
			"⚠️ \"elsewhere\" is not the word \"else\"",
			"```\neffort handling is documented elsewhere; see the 8 buckets above\n```\n",
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := claimsInDoc(tc.doc)
			if len(got) != len(tc.want) {
				t.Fatalf("terminal claims = %v, want %v\ndoc:\n%s", got, tc.want, tc.doc)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("terminal[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCheckDocArms drives the whole checker and asserts WHICH arm fires.
func TestCheckDocArms(t *testing.T) {
	r := discoverRule(t)

	goodTable := bucketTable(
		"| `≤ 15` | 0.5 |",
		"| `≤ 60` | 1.0 |",
		"| `≤ 200` | 3.0 |",
		"| `≤ 1000` | 5.0 |",
		"| otherwise | 8.0 |")

	for _, tc := range []struct {
		name string
		doc  string
		want []string
	}{
		{"correct table", goodTable, nil},
		{
			"correct one-line rule",
			"| Label | Weight |\n|---|---|\n| _(none)_ | bucketed on `effort = lines + " +
				"files×10`: `<=15` → 0.5, `<=60` → 1.0, `<=200` → 3.0, `<=1000` → 5.0, " +
				"else 8.0 |\n",
			nil,
		},
		{
			"correct worked example",
			"A 50-line PR touching 3 files: `effort = 50 + 3 × 10 = 80`, which falls in " +
				"the `≤ 200` bucket → weight **3.0**.\n",
			nil,
		},

		// ---- must stay silent -----------------------------------------------------
		{"⚠️ prose with a dollar amount", "When effort is ≤ 200 the cost is $0.15 per run.\n", nil},
		{"⚠️ prose with a percentage", "Runs with effort ≤ 60 make up 42% of the corpus.\n", nil},
		{"⚠️ prose with a grouped number", "Budget effort ≤ 1,000 lines per review, 2 reviewers.\n", nil},
		{
			"⚠️ prose discourse `otherwise`",
			"The effort proxy is capped; otherwise the score is 3 buckets up.\n",
			nil,
		},
		{
			"⚠️ correct table gains a year column",
			"| `effort = lines + files × 10` | Weight | Since |\n| --- | ---: | --- |\n" +
				"| `≤ 15` | 0.5 | 2024 |\n| `≤ 60` | 1.0 | 2024 |\n" +
				"| `≤ 200` | 3.0 | 2024 |\n| `≤ 1000` | 5.0 | 2024 |\n" +
				"| otherwise | 8.0 | 2024 |\n",
			nil,
		},
		{
			"⚠️ correct table gains a bare-number example column",
			"| `effort = lines + files × 10` | Example | Weight |\n| --- | --- | ---: |\n" +
				"| `≤ 200` | 50 | 3.0 |\n",
			nil,
		},

		// ---- bucket contradictions ------------------------------------------------
		{
			"wrong weight on a bucket",
			strings.Replace(goodTable, "| `≤ 200` | 3.0 |", "| `≤ 200` | 7.0 |", 1),
			[]string{codeBucket, codeIncomplete},
		},
		{
			"⚠️ weight ABOVE the top of the scale is reported, not skipped",
			strings.Replace(goodTable, "| `≤ 200` | 3.0 |", "| `≤ 200` | 10.0 |", 1),
			[]string{codeBucket, codeIncomplete},
		},
		{
			"⚠️ an example column cannot shield a wrong weight",
			"| `effort = lines + files × 10` | Example | Weight |\n| --- | --- | ---: |\n" +
				"| `≤ 200` | 3 files | 10.0 |\n",
			[]string{codeBucket},
		},
		{
			"a bucket the code does not have",
			"```\neffort = lines + files × 10\neffort ≤ 300 → 4\n```\n",
			[]string{codeBucket},
		},
		{
			"⚠️ blockquoted table is still read",
			"> | `effort = lines + files × 10` | Weight |\n> | --- | ---: |\n" +
				"> | `≤ 200` | 7.0 |\n",
			[]string{codeBucket},
		},

		// ---- terminal bucket ------------------------------------------------------
		{
			"⚠️ wrong terminal weight",
			strings.Replace(goodTable, "| otherwise | 8.0 |", "| otherwise | 9.0 |", 1),
			[]string{codeNoTerminal, codeTerminal},
		},
		{
			"⚠️ a correct terminal does not excuse a second, wrong one",
			goodTable + "\n```\neffort = lines + files × 10\notherwise → 9\n```\n",
			[]string{codeTerminal},
		},

		// ---- coefficient ----------------------------------------------------------
		{"wrong files coefficient", "`effort = lines + files × 20`\n", []string{codeCoeff}},
		{"⚠️ capital X sign is still read", "`effort = lines + files X 20`\n", []string{codeCoeff}},
		{"⚠️ U+2715 sign is still read", "`effort = lines + files ✕ 20`\n", []string{codeCoeff}},

		// ---- worked examples ------------------------------------------------------
		{
			"worked-example arithmetic is wrong",
			"`effort = 50 + 3 × 10 = 90`, which falls in the `≤ 200` bucket → weight **3.0**.\n",
			[]string{codeArith},
		},
		{
			"worked example built from correct pairs but the wrong bucket",
			"`effort = 50 + 3 × 10 = 80`, which falls in the `≤ 60` bucket → weight **1.0**.\n",
			[]string{codeAttrib},
		},

		// ---- completeness ---------------------------------------------------------
		{
			"a teaching doc that states nothing",
			"effort is computed from the diff.\n",
			[]string{codeIncomplete, codeIncomplete, codeIncomplete, codeIncomplete, codeNoTerminal},
		},
		{
			"⚠️ a table whose Weight header was renamed is UNREADABLE, not wrong",
			"| effort | Notes |\n| --- | --- |\n| `≤ 15` | 0.5 |\n| `≤ 60` | 1.0 |\n",
			[]string{codeIncomplete, codeIncomplete, codeIncomplete, codeIncomplete, codeNoTerminal},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, stated, term := checkDoc(r, strings.Split(tc.doc, "\n"))

			var got []string
			for _, f := range fs {
				got = append(got, f.code)
			}
			for _, b := range r.steps {
				if !stated[b] {
					got = append(got, codeIncomplete)
				}
			}
			if !term {
				got = append(got, codeNoTerminal)
			}

			wantsCompleteness := false
			for _, w := range tc.want {
				if w == codeIncomplete || w == codeNoTerminal {
					wantsCompleteness = true
				}
			}
			if !wantsCompleteness {
				var trimmed []string
				for _, g := range got {
					if g != codeIncomplete && g != codeNoTerminal {
						trimmed = append(trimmed, g)
					}
				}
				got = trimmed
			}

			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("codes = %v, want %v\ndoc:\n%s\nfindings:", got, want, tc.doc)
				for _, f := range fs {
					t.Errorf("  [%s] line %d: %s", f.code, f.line, f.msg)
				}
			}
		})
	}
}

// TestFuncDeclMatchesMethods pins the matcher the retired-symbol ban proves its premise
// with. A premise-check blind to methods would leave the ban standing after the symbol
// came back — failing docs that had correctly started naming it again.
func TestFuncDeclMatchesMethods(t *testing.T) {
	re := funcDecl("labelWeight")
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"func labelWeight(l []string) float64 {", true},
		{"func (h *Handler) labelWeight(l []string) float64 {", true},
		{"func (h Handler) labelWeight() float64 {", true},
		{"func  labelWeight (l []string) float64 {", true},
		{"// see labelWeight for the mapping", false},
		{"x := labelWeight(names)", false},
		{"func labelWeightTable() {", false},
	} {
		if got := re.MatchString(tc.src); got != tc.want {
			t.Errorf("funcDecl(labelWeight).MatchString(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}
