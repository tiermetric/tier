package prderive

import (
	"strings"
	"testing"
)

// 🔴 Why a guard needs its own selftest.
//
// TestPublishedSizeLabelsMatchDefaults reads free-form markdown. A pass/fail run cannot
// tell a working reader from one that quietly stopped seeing the table — both are green.
// So the reader is exercised over SYNTHETIC documents and every case asserts WHICH
// finding fires, in the style of a private-only selftest target and of the
// sibling selftest for the GitHeuristic table.
//
// ⚠️ Cases marked ⚠️ are the exact inputs that defeated the FIRST version of this reader
// on this tree, measured, not imagined. They are regression pins, not examples. Both
// were FALSE POSITIVES — the reader accusing a correct document — which #724 named as
// the failure mode to test for, because a guard that reddens a correct PR is a guard
// somebody deletes.

// claimsIn runs the real reader over a whole document. Driving documents rather than
// bare lines is deliberate: the table-region and is-this-a-size-table decisions are
// where both measured defects lived, so a line-level helper would exercise only the half
// that was never the problem.
func claimsIn(doc string) []sizeClaim {
	lines := strings.Split(doc, "\n")
	nonTable, _ := nonTableClaims(lines)
	return append(sizeClaimsIn(lines), nonTable...)
}

func labelSet(cs []sizeClaim) map[string]float64 {
	m := map[string]float64{}
	for _, c := range cs {
		if c.ok {
			m[c.label] = c.weight
		}
	}
	return m
}

func TestSizeClaimReader(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      map[string]float64
		wantLen   int // -1 = don't care beyond want
	}{
		{
			name: "README / conventions shape: backticked alias pair",
			doc: "| Label (case-insensitive) | Weight |\n|---|---|\n" +
				"| `size/xs` or `xs` | 0.5 |\n| `size/s` or `s` | 1 |\n",
			want:    map[string]float64{"size/xs": 0.5, "xs": 0.5, "size/s": 1, "s": 1},
			wantLen: 4,
		},
		{
			name:    "⚠️ REGRESSION: the English `or` between two labels is not a label",
			doc:     "| Label | Weight |\n|---|---|\n| `size/xs` or `xs` | 0.5 |\n",
			want:    map[string]float64{"size/xs": 0.5, "xs": 0.5},
			wantLen: 2,
		},
		{
			name: "⚠️ REGRESSION: a work-type segment table is NOT a size-label table",
			// docs/rubric.md publishes exactly this. `bug` is three letters and the column
			// is headed `weight`; the first reader accused this correct table.
			doc: "| Segment    | weight | cost   |\n|---|---|---|\n" +
				"| `feature`  | 3      | $9.00  |\n| `bug`      | 1      | $1.50  |\n" +
				"| `security` | 5      | $20.00 |\n",
			want: map[string]float64{}, wantLen: 0,
		},
		{
			name: "rubric shape: the weight is the MIDDLE column, prose sits to its right",
			doc: "| Label            | Weight | What it means |\n|---|---|---|\n" +
				"| `size/m`  / `m`  | 3      | Medium: the everyday unit of work. |\n",
			want:    map[string]float64{"size/m": 3, "m": 3},
			wantLen: 2,
		},
		{
			name: "how-it-works shape: unmarked cells, prefixed form only",
			doc: "| Label    | Weight |\n| -------- | -----: |\n" +
				"| size/xs  |    0.5 |\n| size/xl  |    8.0 |\n",
			want:    map[string]float64{"size/xs": 0.5, "size/xl": 8},
			wantLen: 2,
		},
		{
			name: "webhook shape: comma-separated alias pair",
			doc: "| Label (case-insensitive)        | Weight |\n| --- | --- |\n" +
				"| `size/xs`, `xs`                 | 0.5    |\n",
			want:    map[string]float64{"size/xs": 0.5, "xs": 0.5},
			wantLen: 2,
		},
		{
			name:    "⚠️ a numeric column to the RIGHT of Weight is not the weight",
			doc:     "| Label | Weight | Since |\n|---|---|---|\n| `size/m` | 3 | 2024 |\n",
			want:    map[string]float64{"size/m": 3},
			wantLen: 1,
		},
		{
			name:    "⚠️ an Example column to the LEFT cannot shield a wrong weight",
			doc:     "| Label | Example | Weight |\n|---|---|---|\n| `size/m` | 50 | 9 |\n",
			want:    map[string]float64{"size/m": 9},
			wantLen: 1,
		},
		{
			name: "a table with no Weight header states nothing",
			doc:  "| Label | Meaning |\n|---|---|\n| `size/m` | medium |\n",
			want: map[string]float64{}, wantLen: 0,
		},
		{
			name: "free prose states nothing, including the range form",
			doc: "TIER maps `size/xs`…`size/xl` to `0.5`…`8`, and a `size/m` PR is 3 points.\n" +
				"Costs like $0.15 and shares like 42% appear in the same paragraph.\n",
			want: map[string]float64{}, wantLen: 0,
		},
		{
			name: "README's `_(none)_` row states the GitHeuristic rule, not a label",
			doc: "| Label | Weight |\n|---|---|\n| `size/xl` or `xl` | 8 |\n" +
				"| _(none)_ | bucketed on `effort = lines + files×10`: `<=15` → 0.5 |\n",
			want:    map[string]float64{"size/xl": 8, "xl": 8},
			wantLen: 2,
		},
		{
			name: "⚠️ REGRESSION: an UNMARKED alias pair does not publish the English `or`",
			// The `or` pin below this one uses code spans, which route through a different
			// branch of cellTokens. Review measured that the two mechanisms MASKED each
			// other: neutralising either alone left the suite green. This case reaches the
			// unmarked branch, which docs/how-it-works.md actually uses.
			doc:     "| Label | Weight |\n|---|---|\n| size/xs or xs | 0.5 |\n",
			want:    map[string]float64{"size/xs": 0.5, "xs": 0.5},
			wantLen: 2,
		},
		{
			name: "⚠️ a `Weight source` column cannot claim the Weight position",
			// weightHeaderCell is anchored to the WHOLE cell. Review measured that a bare
			// `(?i)weight` substring match left the suite green, and `weight_source` is a
			// real TIER column name that plausibly reaches a doc table.
			doc: "| Label | Weight source | Weight |\n|---|---|---|\n" +
				"| `size/m` | label | 3 |\n",
			want:    map[string]float64{"size/m": 3},
			wantLen: 1,
		},
		{
			name: "⚠️ a merged label-reference table: work-type rows are NOT size labels",
			// Cell granularity, not table granularity. A table-granular rule would let one
			// `size/` row license every short token in every other row of a consolidated
			// reference table — a realistic doc merge that would redden a correct PR.
			doc: "| Label | Weight |\n|---|---|\n| `size/m` | 3 |\n" +
				"| `bug` | 1 |\n| `wip` | 0 |\n",
			want:    map[string]float64{"size/m": 3},
			wantLen: 1,
		},
		{
			name: "⚠️ an ESCAPED pipe inside a cell does not shift the weight column",
			// `\|` is the standard markdown escape. Splitting on it moved every column
			// right, the weight cell landed on a backticked label, and a CORRECT document
			// was reported [unreadable].
			doc:     "| Label | Weight |\n|---|---|\n| `size/m` \\| `m` | 3 |\n",
			want:    map[string]float64{"size/m": 3, "m": 3},
			wantLen: 2,
		},
		{
			name: "⚠️ a BODY row whose first cell is `weight` is not a header",
			// docs/outcomes-api.md:52 is exactly this shape. Without requiring a separator
			// row beneath, it was promoted to a header at column 0 and the next size-label
			// row was reported [unreadable] — accusing the very document this reader's
			// scope comment names as deliberately out of scope.
			doc: "| Field | Description |\n|---|---|\n" +
				"| `weight` | the outcome weight |\n| `size/m` | the medium label |\n",
			want: map[string]float64{}, wantLen: 0,
		},
		{
			name:    "PROSE: the `LABEL (weight N)` form is read",
			doc:     "1. **A `size/s` (weight 1) bug fix that cost $2.00 of AI compute.**\n",
			want:    map[string]float64{"size/s": 1},
			wantLen: 1,
		},
		{
			name:    "PROSE: the bare `LABEL (N)` form is read",
			doc:     "a generous org calls the same change `size/l` (5) that a strict org calls `size/m` (3).\n",
			want:    map[string]float64{"size/l": 5, "size/m": 3},
			wantLen: 2,
		},
		{
			name: "PROSE: a label INSIDE parentheses states no weight",
			// "(`size/xs` … `size/xl`)" is a range, not a mapping. The parenthetical must
			// FOLLOW the label.
			doc:  "**PR size labels** (`size/xs` … `size/xl`) if a human applied one.\n",
			want: map[string]float64{}, wantLen: 0,
		},
		{
			name:    "INLINE: several pairs on one line publish the built-in table",
			doc:     "#   size/xs, xs: 0.5   size/s, s: 1   size/m, m: 3   size/l, l: 5   size/xl, xl: 8\n",
			want:    map[string]float64{"size/xs": 0.5, "xs": 0.5, "size/s": 1, "s": 1, "size/m": 3, "m": 3, "size/l": 5, "l": 5, "size/xl": 8, "xl": 8},
			wantLen: 10,
		},
		{
			name: "⚠️ INLINE: a YAML OVERRIDE block is an example, NOT the built-in table",
			// One pair per line. config.example.yaml:299-305 and docs/webhook-setup.md:83-88
			// both publish an org's DIFFERENT label names; `"xxl": 8` there is legitimate
			// and flagging it would redden a correct document.
			doc: "size_labels:\n  \"xs\": 0.5\n  \"s\": 1\n  \"m\": 3\n  \"l\": 5\n" +
				"  \"xl\": 8\n  \"xxl\": 8\n",
			want: map[string]float64{}, wantLen: 0,
		},
		{
			name: "a size table does not absorb the NEXT table after a blank line",
			doc: "| Label | Weight |\n|---|---|\n| `size/s` or `s` | 1 |\n\n" +
				"| Segment | weight |\n|---|---|\n| `bug` | 1 |\n",
			want:    map[string]float64{"size/s": 1, "s": 1},
			wantLen: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claimsIn(tc.doc)
			if tc.wantLen >= 0 && len(got) != tc.wantLen {
				t.Fatalf("read %d claims, want %d: %+v", len(got), tc.wantLen, got)
			}
			set := labelSet(got)
			for l, w := range tc.want {
				if set[l] != w {
					t.Errorf("claim %q = %v, want %v (all: %v)", l, set[l], w, set)
				}
			}
			for l := range set {
				if _, ok := tc.want[l]; !ok {
					t.Errorf("read an unexpected claim %q = %v", l, set[l])
				}
			}
		})
	}
}

// TestSizeDocFindings asserts WHICH arm fires for each way a document can be wrong.
// Presence-of-any-error would be satisfied by a reader that reports one finding for
// every line it does not understand.
func TestSizeDocFindings(t *testing.T) {
	head := "| Label | Weight |\n|---|---|\n"
	for _, tc := range []struct {
		name, doc string
		wantCode  string
		wantIn    string // substring the message must carry, so the operator can act
	}{
		{
			name:     "a corrupted PUBLISHED weight",
			doc:      head + "| `size/m` | 4 |\n",
			wantCode: codeWeight,
			wantIn:   "size/m",
		},
		{
			name:     "a doc-invented label the map does not contain",
			doc:      head + "| `size/xxl` | 13 |\n",
			wantCode: codeUnknown,
			wantIn:   "size/xxl",
		},
		{
			name: "a RETIRED label left published — the actual #642 scenario",
			// Identical mechanism to codeUnknown, and that is the point: the arm that
			// catches an invented row is the same arm that catches a row the code dropped.
			doc:      head + "| `size/xs` | 0.5 |\n| `size/tiny` | 0.25 |\n",
			wantCode: codeUnknown,
			wantIn:   "size/tiny",
		},
		{
			name:     "a weight cell that is not a number is REPORTED, not skipped",
			doc:      head + "| `size/m` | see below |\n",
			wantCode: codeUnreadable,
			wantIn:   "size/m",
		},
		{
			// ⚠️ pureNumber is ANCHORED. Review measured that unanchoring it — finding a
			// number ANYWHERE in the cell — left the suite green, because the case above
			// has no digits at all and so cannot tell the two apart.
			name:     "a weight cell with a number AND a unit is unreadable, not read as 3",
			doc:      head + "| `size/m` | 3 points |\n",
			wantCode: codeUnreadable,
			wantIn:   "size/m",
		},
		{
			name:     "an ordered weight SCALE that is not the code's weight set",
			doc:      "Fibonacci-ish weights 0.5 / 1.0 / 4.0 / 5.0 / 8.0 apply to `size/xs` labels.\n",
			wantCode: codeScale,
			wantIn:   "0.5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, _ := checkSizeDoc(strings.Split(tc.doc, "\n"))
			if len(findings) != 1 {
				t.Fatalf("want exactly 1 finding, got %d: %+v", len(findings), findings)
			}
			if findings[0].code != tc.wantCode {
				t.Errorf("fired %q, want %q: %s", findings[0].code, tc.wantCode, findings[0].msg)
			}
			if !strings.Contains(findings[0].msg, tc.wantIn) {
				t.Errorf("message does not name %q, so it cannot be acted on: %s",
					tc.wantIn, findings[0].msg)
			}
			if findings[0].line == 0 {
				t.Error("finding carries no line number")
			}
		})
	}
}

// TestSizeDocScaleContextExcludesVersionNumbers is the control for the ordered-scale arm.
// ⚠️ MEASURED FALSE POSITIVE: docs/reference-price-table.md:253 publishes
// `| claude-opus-4.5 / 4.6 / 4.7 / 4.8 / 5 | Anthropic | 5.00 | 25.00 | 18.00 |` — five
// slash-separated numbers that are MODEL VERSIONS, on a line naming no label, weight or
// scale. Without the context gate the arm accuses a correct price table.
func TestSizeDocScaleContextExcludesVersionNumbers(t *testing.T) {
	doc := "| claude-opus-4.5 / 4.6 / 4.7 / 4.8 / 5 | Anthropic | 5.00 | 25.00 | 18.00 |\n"
	findings, _ := checkSizeDoc(strings.Split(doc, "\n"))
	if len(findings) != 0 {
		t.Fatalf("the model-version row was accused: %+v", findings)
	}
	// Positive control: the SAME numbers on a line that names the scale DO get judged, so
	// the gate is a discriminator and not a blanket off-switch.
	live := "the scale is 4.5 / 4.6 / 4.7 / 4.8 / 5\n"
	findings, _ = checkSizeDoc(strings.Split(live, "\n"))
	if len(findings) != 1 || findings[0].code != codeScale {
		t.Fatalf("the context gate suppressed a real scale claim: %+v", findings)
	}
}

// TestSizeDocCorrectDocumentIsSilent is the control every arm above needs: the shapes
// that must fire are only meaningful if the correct shape does NOT.
func TestSizeDocCorrectDocumentIsSilent(t *testing.T) {
	doc := "| Label (case-insensitive) | Weight |\n|---|---|\n" +
		"| `size/xs` or `xs` | 0.5 |\n| `size/s` or `s` | 1 |\n" +
		"| `size/m` or `m` | 3 |\n| `size/l` or `l` | 5 |\n| `size/xl` or `xl` | 8 |\n"
	findings, stated := checkSizeDoc(strings.Split(doc, "\n"))
	if len(findings) != 0 {
		t.Fatalf("a correct table produced findings: %+v", findings)
	}
	for label := range defaultSizeLabels {
		if !stated[label] {
			t.Errorf("a table publishing every row did not register %q as stated", label)
		}
	}
}

// TestSizeDocIncompleteTeachingDoc pins the completeness arm, and pins the RULING that
// bare aliases are not required per-doc — docs/how-it-works.md publishes prefixed rows
// only and is correct.
func TestSizeDocIncompleteTeachingDoc(t *testing.T) {
	partial := "| Label | Weight |\n|---|---|\n| `size/xs` | 0.5 |\n| `size/s` | 1 |\n"
	_, stated := checkSizeDoc(strings.Split(partial, "\n"))
	// ⭐ CALLS THE GUARD'S OWN FUNCTION. An earlier version reimplemented this loop, so
	// deleting the arm — or weakening it to fire only when a doc published nothing —
	// left the entire package green.
	// Drive the ARM, not just its input: an extracted helper whose caller is untested is
	// still an untested arm.
	if f := completenessFindings(stated); len(f) != 1 || f[0].code != codeIncomplete {
		t.Errorf("a doc publishing 2 of 5 prefixed rows produced %+v, want one [incomplete]", f)
	} else if !strings.Contains(f[0].msg, "size/m") {
		t.Errorf("the [incomplete] message does not name the missing labels: %s", f[0].msg)
	}
	missing := missingPrefixedLabels(stated)
	if len(missing) != 3 {
		t.Errorf("a table publishing 2 of 5 prefixed rows reports %d missing, want 3: %v",
			len(missing), missing)
	}

	full := "| Label | Weight |\n|---|---|\n| size/xs | 0.5 |\n| size/s | 1 |\n" +
		"| size/m | 3 |\n| size/l | 5 |\n| size/xl | 8 |\n"
	_, stated = checkSizeDoc(strings.Split(full, "\n"))
	if got := completenessFindings(stated); len(got) != 0 {
		t.Errorf("prefixed-only table (the how-it-works.md shape) is accused: %+v; "+
			"that document is correct and this arm would redden it", got)
	}

	// ⚠️ A WRONG WEIGHT STILL COUNTS AS STATED. Review measured that a doc publishing
	// `size/m` with the wrong weight drew BOTH [weight] and "[incomplete] … never states
	// [size/m]" — and the second sentence is FALSE. It doubled the noise on the single
	// most likely real failure while telling the operator something untrue.
	wrong := "| Label | Weight |\n|---|---|\n| size/xs | 0.5 |\n| size/s | 1 |\n" +
		"| size/m | 4 |\n| size/l | 5 |\n| size/xl | 8 |\n"
	findings, stated := checkSizeDoc(strings.Split(wrong, "\n"))
	if got := missingPrefixedLabels(stated); len(got) != 0 {
		t.Errorf("a doc that states `size/m` WRONGLY is reported as never stating %v", got)
	}
	if len(findings) != 1 || findings[0].code != codeWeight {
		t.Errorf("want exactly one [weight] finding, got %+v", findings)
	}
}
