package prderive

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestPublishedSizeLabelsMatchDefaults pins every PUBLISHED size-label weight table
// against defaultSizeLabels, the map that actually decides the weight.
//
// 🔴 Why this exists, and why it is not #642 again by coincidence.
//
// #642 existed because a retired log2 weight formula stayed published in four files
// after GitHeuristic stopped being one. Its guard (store.published_weight_rule_test.go)
// closed the FALLBACK path. This closes the PRIMARY one: labels are what weighs a PR
// when a human applied one, and GitHeuristic runs only when none did. So the more
// commonly hit of the two tables was the unguarded one.
//
// 🔴 AND #642'S ROOT CAUSE REPEATED INSIDE #724's OWN ISSUE BODY. That issue states the
// table is published in THREE files (README.md, docs/conventions.md,
// docs/how-it-works.md). Measured on this tree, it is published in FIVE — docs/rubric.md
// and docs/webhook-setup.md were missed. #642 propagated twice for exactly that reason:
// #635's guard was scoped to the one file somebody knew about. ⇒ THE CONTRADICTION ARM
// CARRIES NO FILE LIST. It sweeps every markdown file that ships publicly, so a table
// added to a SIXTH file tomorrow is covered without anyone editing this test. Only the
// completeness arm — "this doc must TEACH the whole table" — is scoped to a list, and a
// missing entry there costs coverage of one arm, not of the guard.
//
// ⭐ The reader extracts what each document CLAIMS and hands it to the code to judge.
// It never walks the map asking "is this mentioned", because that direction cannot see
// a doc-invented row: a published `size/xxl → 13` would satisfy every "is size/m
// mentioned" check ever written.

// sizeLabelTeachingDocs must publish the WHOLE table. Paths are relative to this
// package directory. Every other published markdown file is still swept for
// contradictions; it simply is not required to teach.
var sizeLabelTeachingDocs = []string{
	"../../README.md",
	"../../docs/conventions.md",
	"../../docs/how-it-works.md",
	"../../docs/rubric.md",
	"../../docs/webhook-setup.md",
}

// Finding codes. The selftest asserts WHICH arm fired, never merely that something did.
// #642's reader passed its own suite through four successive designs while checking the
// wrong thing; a bare pass/fail cannot tell a working guard from a vacuous one.
const (
	codeWeight     = "weight"     // a published weight disagrees with defaultSizeLabels
	codeUnknown    = "unknown"    // a published label the map does not contain
	codeUnreadable = "unreadable" // a size-label row whose weight cell cannot be parsed
	codeIncomplete = "incomplete" // a teaching doc omits a label from the table
	codeScale      = "scale"      // a published ORDERED SCALE is not the code's weight set
)

type sizeFinding struct {
	line int // 1-based; 0 when the finding is about the file as a whole
	code string
	msg  string
}

// sizeClaim is one label→weight pair a document states.
type sizeClaim struct {
	line  int
	label string
	// weight is the published value; ok is false when the row named a label but the
	// weight cell was not a number. That is reported, NOT skipped — a silent skip is
	// how a table survives a reformat that quietly stopped being checked.
	weight float64
	ok     bool
}

// ---------------------------------------------------------------------------
// Reading a document
// ---------------------------------------------------------------------------

// weightHeaderCell matches the column header above the weights. Anchored to the whole
// cell so a prose column such as "What it means (canonical calibration)" — which
// contains no such word — and, more importantly, a column named "Weight source" cannot
// claim the position.
var weightHeaderCell = regexp.MustCompile(`(?i)^[\s` + "`" + `*_]*weight[\s` + "`" + `*_]*$`)

// prefixedLabel matches the unambiguous `size/…` form. The body is deliberately WIDER
// than the five real labels: a retired or invented `size/xxl` must reach the map and be
// REJECTED by name, and a pattern that only matched the five valid ones would silently
// drop it — turning the doc-invented arm into a no-op.
var prefixedLabel = regexp.MustCompile(`(?i)^size/[a-z0-9._-]{1,8}$`)

// bareLabel matches the short alias form, and — unlike prefixedLabel — it is an EXACT
// alternation of the five real aliases.
//
// 🔴 IT WAS `^[a-z]{1,3}$` AND THAT WAS WRONG IN TWO MEASURED WAYS. A width-based
// pattern read the English `or` in "`size/xs` or `xs`" as a published label, and read
// `bug` from docs/rubric.md's work-type segment table (which has a `weight` column) as a
// size label. Both were the guard ACCUSING A CORRECT DOCUMENT — the failure mode #724
// named, because a guard that reddens a correct PR is a guard somebody deletes.
//
// ⭐ Narrowing costs NO coverage: the doc-invented / retired-label arm is carried
// entirely by prefixedLabel, which is deliberately wide (`size/xxl` and `size/tiny` both
// reach the map and are rejected by name). ⚠️ THE ONE THING IT DOES GIVE UP, stated
// rather than hidden: a doc publishing a bare-only invented label (`| xxl | 13 |`) inside
// a size table is not seen. The code-side counterpart of that — a bare-only key added to
// defaultSizeLabels — IS caught, by TestSizeLabelAliasesMatchPrefixedForms below.
var bareLabel = regexp.MustCompile(`(?i)^(?:xs|s|m|l|xl)$`)

// labelToken splits a table cell into candidate tokens. `/` is kept because it is part
// of the label; everything else that separates words is a boundary, so "`size/xs` or
// `xs`", "`size/xs`, `xs`" and "`size/xs` / `xs`" — all three shapes in this tree — read
// identically.
var labelToken = regexp.MustCompile(`[A-Za-z0-9/._-]+`)

// codeSpan matches a markdown inline code span.
var codeSpan = regexp.MustCompile("`([^`]+)`")

// pureNumber matches a weight cell that is a bare number, allowing markdown emphasis.
var pureNumber = regexp.MustCompile("^[\\s`*_]*([0-9]+(?:\\.[0-9]+)?)[\\s`*_]*$")

// separatorRow matches a markdown table's `|---|---|` line.
var separatorRow = regexp.MustCompile(`^[\s:|-]+$`)

// tableCells splits a markdown table row into its cells, or returns nil for a line that
// is not one. A row must carry at least two cells; a line merely containing a pipe (a
// shell snippet, a regex alternation) is not a table.
func tableCells(line string) []string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") {
		return nil
	}
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")

	// ⚠️ `\|` is the standard markdown escape for a literal pipe inside a cell, and
	// splitting on it shifts every column to its right. Measured: `| \`size/m\` \| \`m\` |
	// 3 |` — a plausible reformat of README's current `or` row — became three cells, the
	// weight column landed on a backticked label, and a CORRECT document was reported
	// [unreadable]. Split on unescaped pipes only, then unescape.
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(t); i++ {
		switch {
		case t[i] == '\\' && i+1 < len(t) && t[i+1] == '|':
			cur.WriteByte('|')
			i++
		case t[i] == '|':
			cells = append(cells, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(t[i])
		}
	}
	cells = append(cells, cur.String())
	if len(cells) < 2 {
		return nil
	}
	return cells
}

// weightColumn reports the index of the Weight column in a header row, or -1.
//
// ⭐ THE WEIGHT IS TAKEN FROM THE COLUMN THE HEADER NAMES — never "the rightmost
// number", never "the second cell". docs/rubric.md publishes Label | Weight | What it
// means, so the weight is in the MIDDLE; a rightmost-cell reader would score its prose
// and a second-cell reader would break the moment an Example column is inserted on the
// left. #642's review measured both of those failures on the sibling table.
func weightColumn(header []string) int {
	for i, c := range header {
		if weightHeaderCell.MatchString(c) {
			return i
		}
	}
	return -1
}

// cellTokens returns the candidate tokens in a cell.
//
// Inline code spans win when the cell has any: every published table marks its labels up
// as code, and honouring that markup is what keeps the English between them out. A cell
// with no code spans falls back to whitespace tokens, because docs/how-it-works.md
// publishes `| size/xs  | 0.5 |` unmarked and that table is correct.
func cellTokens(cell string) []string {
	var toks []string
	if spans := codeSpan.FindAllStringSubmatch(cell, -1); len(spans) > 0 {
		for _, m := range spans {
			toks = append(toks, labelToken.FindAllString(m[1], -1)...)
		}
		return toks
	}
	return labelToken.FindAllString(cell, -1)
}

// labelsIn returns the label tokens a first cell names, given whether the enclosing
// table has been identified as a size-label table.
//
// 🔴 BARE ALIASES ARE ONLY READ IN A CELL THAT ALSO CARRIES A `size/…` TOKEN — per CELL,
// not per table. ⚠️ Measured: docs/rubric.md publishes a work-type segment table
// (`| \`bug\` | 1 | $1.50 |`) with a `weight` column and a three-letter first cell, and a
// bare-token reader accused that CORRECT table. A table-granular rule would still accuse
// a merged "Label reference" table listing size labels and work-type labels together —
// a realistic doc consolidation — because one `size/` row anywhere would license every
// short token in every other row. Cell granularity is what makes the alias pair
// "`size/xs` or `xs`" readable without licensing the row below it.
func labelsIn(cell string) []string {
	toks := cellTokens(cell)
	var prefixed, bare []string
	hasPrefixed := false
	for _, tok := range toks {
		if prefixedLabel.MatchString(tok) {
			hasPrefixed = true
		}
	}
	for _, tok := range toks {
		low := strings.ToLower(tok)
		switch {
		case prefixedLabel.MatchString(tok):
			prefixed = append(prefixed, low)
		case hasPrefixed && bareLabel.MatchString(tok):
			bare = append(bare, low)
		}
	}
	return append(prefixed, bare...)
}

// sizeClaimsIn returns every label→weight pair a document states.
//
// Its scope is a markdown table with a Weight column and at least one first cell naming a
// `size/…` label. Prose and the inline YAML form are read separately, by nonTableClaims.
//
// ⚠️ STATED LIMITS, because a guard's blind spots belong in the file, not in a review:
//
//   - A table publishing ONLY bare aliases, with no `size/` row anywhere, is not
//     recognised as a size table and goes unread. All six published sites carry the
//     prefixed form, and requiring it is what keeps docs/rubric.md's work-type table from
//     being misread. The completeness arm makes a bare-only teaching doc fail there
//     instead, so the hole is not silent where it matters.
//   - Fenced code blocks are NOT skipped. A document that showed an EXAMPLE markdown size
//     table inside a fence would have it judged as a real publication. No such block
//     exists today; it is a plausible future false positive and is called out here so the
//     fix is a known one rather than a rediscovery.
//   - docs/outcomes-api.md:61 writes the range form "`size/xs`…`size/xl` → `0.5`…`8`",
//     which states the ENDPOINTS of a scale rather than any row. It is deliberately not a
//     claim. (An earlier version of this comment said docs/how-it-works.md wrote the same
//     form. It does not — :341 is a table lead-in and :508 is the ordered scale, which IS
//     now read, by the orderedScale arm.)
func sizeClaimsIn(lines []string) []sizeClaim {
	var out []sizeClaim

	for i := 0; i < len(lines); i++ {
		cells := tableCells(lines[i])
		if cells == nil {
			continue
		}
		// 🔴 A HEADER IS A ROW FOLLOWED BY A SEPARATOR ROW. Without that requirement any
		// table BODY row whose first cell is `weight` is promoted to a header at column 0.
		// docs/outcomes-api.md:52 is exactly such a row (`| \`weight\` | number | no | … |`),
		// and that document was one field-reference row away from being accused —
		// the very document this reader's scope comment cites as deliberately out of scope.
		if i+1 >= len(lines) || !separatorRow.MatchString(strings.Join(tableCells(lines[i+1]), "|")) {
			continue
		}
		weightCol := weightColumn(cells)
		if weightCol == -1 {
			continue
		}
		// Collect the body of this table: every row until the first line that is not one.
		type row struct {
			line  int
			cells []string
		}
		var body []row
		sizeTable := false
		for j := i + 1; j < len(lines); j++ {
			rc := tableCells(lines[j])
			if rc == nil {
				i = j
				break
			}
			i = j
			if separatorRow.MatchString(strings.Join(rc, "|")) {
				continue
			}
			body = append(body, row{line: j + 1, cells: rc})
			for _, tok := range cellTokens(rc[0]) {
				if prefixedLabel.MatchString(tok) {
					sizeTable = true
				}
			}
		}
		if !sizeTable {
			continue
		}
		for _, r := range body {
			labels := labelsIn(r.cells[0])
			if len(labels) == 0 {
				continue // e.g. README's `_(none)_` row, which states the GitHeuristic rule
			}
			w, ok := 0.0, false
			if weightCol < len(r.cells) {
				if m := pureNumber.FindStringSubmatch(r.cells[weightCol]); m != nil {
					w, _ = strconv.ParseFloat(m[1], 64)
					ok = true
				}
			}
			for _, l := range labels {
				out = append(out, sizeClaim{line: r.line, label: l, weight: w, ok: ok})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Shapes that are NOT a markdown table
// ---------------------------------------------------------------------------

// 🔴 WHY THESE EXIST. An earlier version of this guard read TABLES ONLY and said so in a
// comment, citing #642's measurement that reading numbers out of prose reddened correct
// documents. Review refuted the defence and then the exclusion: the sibling guard
// (internal/store/published_weight_rule_test.go:452) READS PROSE in this same tree and is
// green, and SIX live label->weight pairs were published outside any table — five in
// docs/rubric.md and one in docs/how-it-works.md, BOTH teaching docs, one of them named
// by #724 itself. Measured: corrupting all of them left `go test ./...` entirely green.
//
// ⇒ #642 was a stale number surviving IN PROSE across four files. A guard that closes the
// table shape and leaves the prose shape open re-opens the exact defect it was built for.

// proseLabelWeight matches the `LABEL (weight N)` and `LABEL (N)` forms — docs/rubric.md
// publishes both. The label must be a code span and the parenthetical must FOLLOW it, so
// the range form "(`size/xs` … `size/xl`)" — a label INSIDE parentheses, followed by
// nothing — states no weight and is correctly read as no claim.
var proseLabelWeight = regexp.MustCompile("`(size/[A-Za-z0-9._-]{1,8})`\\s*\\((?:weight\\s+)?([0-9]+(?:\\.[0-9]+)?)\\)")

// orderedScale matches four or more slash-separated numbers: the "Fibonacci-ish weights
// 0.5 / 1.0 / 3.0 / 5.0 / 8.0" form. It states the whole weight SET at once rather than
// any single label, so it gets its own finding code.
var orderedScale = regexp.MustCompile(`(?:^|[^0-9./])([0-9]+(?:\.[0-9]+)?(?:\s*/\s*[0-9]+(?:\.[0-9]+)?){3,})`)

// scaleContext gates orderedScale. ⚠️ MEASURED FALSE POSITIVE, and the reason this gate
// exists: docs/reference-price-table.md:253 publishes
// `| claude-opus-4.5 / 4.6 / 4.7 / 4.8 / 5 | Anthropic | ... |` — five slash-separated
// numbers that are MODEL VERSIONS. Requiring the line to also name a size label, a
// weight or a scale excludes it and admits both real sites.
var scaleContext = regexp.MustCompile(`(?i)size/[a-z]|\bweights?\b|\bscale\b`)

// inlinePairs matches the `a, b: N   c, d: M …` form — several label->weight pairs on ONE
// line. 🔴 config.example.yaml:295 publishes the COMPLETE built-in table this way, in a
// YAML comment, on the public export allowlist, and nothing guarded it.
//
// ⚠️ IT MUST MATCH THE QUOTED YAML FORM `"m": 3` TOO, and that is not cosmetic. When the
// pattern required a bare label, the YAML override blocks matched NOTHING — so the
// minInlinePairs ruling below was never what excluded them, and the control asserting
// they stay silent passed for the wrong reason. Measured: dropping the threshold to 1 left
// the suite green, i.e. the threshold was guarding nothing. Quotes are now consumed so the
// pair COUNT is genuinely the discriminator.
var inlinePairs = regexp.MustCompile(`["']?([A-Za-z][A-Za-z0-9/_-]*)["']?(?:\s*,\s*["']?[A-Za-z][A-Za-z0-9/_-]*["']?)*\s*:\s*([0-9]+(?:\.[0-9]+)?)`)

// ⭐ THE INLINE ARM REQUIRES AT LEAST TWO PAIRS ON THE LINE, and that threshold is a
// deliberate ruling, not a tuning knob. A YAML BLOCK with one `"name": weight` per line is
// an OVERRIDE EXAMPLE — a different org's label names — and both config.example.yaml
// (:299-305, an "S".."XXL" scheme) and docs/webhook-setup.md (:83-88) publish one. Those
// are configuration illustrations, NOT publications of the built-in table, and flagging
// their `"xxl": 8` row as an unknown label would redden a correct document. The
// multi-pair-on-one-line form is only ever used to state the built-in table.
// Measured across all 44 published files: the rule matches exactly ONE line, and it is
// the unguarded one.
const minInlinePairs = 2

// nonTableClaims reads the shapes above. It returns label claims plus any ordered scales.
func nonTableClaims(lines []string) ([]sizeClaim, []scaleClaim) {
	var out []sizeClaim
	var scales []scaleClaim
	for i, line := range lines {
		for _, m := range proseLabelWeight.FindAllStringSubmatch(line, -1) {
			w, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				continue
			}
			out = append(out, sizeClaim{line: i + 1, label: strings.ToLower(m[1]), weight: w, ok: true})
		}
		if scaleContext.MatchString(line) {
			for _, m := range orderedScale.FindAllStringSubmatch(line, -1) {
				var nums []float64
				for _, tok := range strings.Split(m[1], "/") {
					f, err := strconv.ParseFloat(strings.TrimSpace(tok), 64)
					if err != nil {
						nums = nil
						break
					}
					nums = append(nums, f)
				}
				if len(nums) > 0 {
					scales = append(scales, scaleClaim{line: i + 1, weights: nums})
				}
			}
		}
		var pairs []sizeClaim
		for _, m := range inlinePairs.FindAllString(line, -1) {
			colon := strings.LastIndex(m, ":")
			if colon < 0 {
				continue
			}
			w, err := strconv.ParseFloat(strings.TrimSpace(m[colon+1:]), 64)
			if err != nil {
				continue
			}
			for _, tok := range strings.Split(m[:colon], ",") {
				tok = strings.ToLower(strings.Trim(strings.TrimSpace(tok), `"'`))
				if prefixedLabel.MatchString(tok) || bareLabel.MatchString(tok) {
					pairs = append(pairs, sizeClaim{line: i + 1, label: tok, weight: w, ok: true})
				}
			}
		}
		if distinctLines(pairs) >= minInlinePairs {
			out = append(out, pairs...)
		}
	}
	return out, scales
}

// scaleClaim is an ordered weight SET a document publishes without naming labels.
type scaleClaim struct {
	line    int
	weights []float64
}

// distinctLines counts how many distinct WEIGHTS a set of inline pairs states, which is
// the "at least two pairs" test. Counting pairs rather than labels matters because
// `size/xs, xs: 0.5` is one pair naming two labels.
func distinctLines(cs []sizeClaim) int {
	seen := map[float64]bool{}
	for _, c := range cs {
		seen[c.weight] = true
	}
	return len(seen)
}

// codeWeightSet is the ascending distinct weight set defaultSizeLabels implements.
func codeWeightSet() []float64 {
	seen := map[float64]bool{}
	var out []float64
	for _, w := range defaultSizeLabels {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Float64s(out)
	return out
}

// ---------------------------------------------------------------------------
// Judging a document against the code
// ---------------------------------------------------------------------------

// checkSizeDoc judges one document and reports which labels it managed to teach.
func checkSizeDoc(lines []string) (out []sizeFinding, stated map[string]bool) {
	stated = map[string]bool{}

	claims := sizeClaimsIn(lines)
	nonTable, scales := nonTableClaims(lines)
	claims = append(claims, nonTable...)

	want := codeWeightSet()
	for _, sc := range scales {
		if len(sc.weights) != len(want) {
			out = append(out, sizeFinding{sc.line, codeScale, fmt.Sprintf(
				"publishes an ordered weight scale of %d values %v, but defaultSizeLabels "+
					"implements %d: %v", len(sc.weights), sc.weights, len(want), want)})
			continue
		}
		for i := range want {
			if sc.weights[i] != want[i] {
				out = append(out, sizeFinding{sc.line, codeScale, fmt.Sprintf(
					"publishes the ordered weight scale %v, but defaultSizeLabels implements "+
						"%v. The scale is stated without naming labels, so a reader takes it "+
						"as the whole truth about what a weight can be.", sc.weights, want)})
				break
			}
		}
	}

	for _, c := range claims {
		w, known := defaultSizeLabels[c.label]
		if !known {
			out = append(out, sizeFinding{c.line, codeUnknown, fmt.Sprintf(
				"publishes label %q, which defaultSizeLabels does not contain. Either the "+
					"label was retired in code and left in the docs, or the doc invented it; "+
					"a reader who applies it gets the git heuristic, not the weight shown.",
				c.label)})
			continue
		}
		if !c.ok {
			out = append(out, sizeFinding{c.line, codeUnreadable, fmt.Sprintf(
				"names label %q in a table with a Weight column, but that column does not "+
					"hold a bare number on this row, so the published weight cannot be "+
					"checked. Reported rather than skipped: an unreadable row is how a table "+
					"quietly stops being guarded.", c.label)})
			continue
		}
		// ⭐ STATED IS RECORDED BEFORE THE VALUE IS JUDGED, and that ordering is the fix
		// for a measured double-report: a doc publishing `size/m` with the WRONG weight
		// used to draw both [weight] and "[incomplete] … never states [size/m]". The
		// second sentence is FALSE — the document does state it, wrongly — and it doubled
		// the noise on the single most likely real failure while telling the operator
		// something untrue about their file. Completeness asks "is the row present"; the
		// value is a different question, already answered on its own line.
		stated[c.label] = true
		if c.weight != w {
			out = append(out, sizeFinding{c.line, codeWeight, fmt.Sprintf(
				"publishes %s -> %s, but defaultSizeLabels returns %s. Weight is the TIER "+
					"NUMERATOR, so a wrong published weight overstates or understates the "+
					"metric for every reader who trusts the docs.",
				c.label, fmtSizeWeight(c.weight), fmtSizeWeight(w))})
		}
	}
	return out, stated
}

func fmtSizeWeight(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// ---------------------------------------------------------------------------
// The guard
// ---------------------------------------------------------------------------

// missingPrefixedLabels reports which `size/…` keys a document failed to state.
//
// 🔴 IT IS A FUNCTION, NOT A LOOP INSIDE THE GUARD, and that is the whole point. Review
// measured that when the completeness arm was inline, BOTH of these left the entire
// package green: deleting the arm outright, and weakening its threshold so it fired only
// when a doc published nothing at all. The selftest had REIMPLEMENTED the loop rather
// than driving it, so it proved the reimplementation. ⇒ the guard and its selftest now
// call the SAME function, and a future session cannot weaken one without the other.
//
// ⭐ The completeness ruling itself, made deliberately as #724 requires: only the `size/`
// forms are required per teaching document. The bare aliases are an input-FOLDING rule,
// not five more weights — docs/how-it-works.md publishes prefixed rows and states the
// alias in prose, and that document is CORRECT; demanding ten rows of it would redden a
// correct file. The aliases are covered on the code side by
// TestSizeLabelAliasesMatchPrefixedForms and on the doc side by the contradiction arm,
// which checks the weight of any bare form a document DOES publish.
func missingPrefixedLabels(stated map[string]bool) []string {
	var missing []string
	for label := range defaultSizeLabels {
		if !strings.HasPrefix(label, "size/") {
			continue
		}
		if !stated[label] {
			missing = append(missing, label)
		}
	}
	sort.Strings(missing)
	return missing
}

// completenessFindings is the completeness ARM itself, extracted for the same reason
// missingPrefixedLabels was. ⚠️ Measured: with the arm inline, BOTH deleting it and
// weakening its threshold to `len(missing) > 4` left the whole package green — the
// selftest was driving missingPrefixedLabels and never the branch that turns its result
// into a finding. An extracted helper whose CALLER is untested is still an untested arm.
func completenessFindings(stated map[string]bool) []sizeFinding {
	missing := missingPrefixedLabels(stated)
	if len(missing) == 0 {
		return nil
	}
	return []sizeFinding{{0, codeIncomplete, fmt.Sprintf(
		"teaches the size-label table but never states %v. A partial table reads as the "+
			"whole one.", missing)}}
}

func TestPublishedSizeLabelsMatchDefaults(t *testing.T) {
	// Premise check. Every arm below is worthless if the ground truth is empty, and an
	// empty map would make the contradiction sweep pass by finding nothing to compare.
	if len(defaultSizeLabels) == 0 {
		t.Fatal("defaultSizeLabels is empty: this guard has nothing to judge against")
	}

	teaching := map[string]bool{}
	for _, p := range sizeLabelTeachingDocs {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("cannot resolve %s: %v", p, err)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("teaching doc %s is unreadable: %v. A guard whose subject vanished "+
				"must fail, not silently cover one file fewer.", p, err)
		}
		teaching[abs] = true
	}

	// The contradiction sweep carries no list; see the header comment.
	docs := publishedTextFiles(t)
	sweptTeaching := 0
	claimsSeen := 0

	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		lines := strings.Split(string(raw), "\n")
		findings, stated := checkSizeDoc(lines)
		claimsSeen += len(stated) + len(findings)

		abs, _ := filepath.Abs(path)
		if teaching[abs] {
			sweptTeaching++
			findings = append(findings, completenessFindings(stated)...)
		}

		for _, f := range findings {
			where := path
			if f.line > 0 {
				where = fmt.Sprintf("%s:%d", path, f.line)
			}
			t.Errorf("[%s] %s %s", f.code, where, f.msg)
		}
	}

	// Vacuity controls. A sweep that read nothing passes every arm above.
	if sweptTeaching != len(sizeLabelTeachingDocs) {
		t.Errorf("swept %d of %d teaching docs: the sweep does not reach them all, so the "+
			"completeness arm is partly inert", sweptTeaching, len(sizeLabelTeachingDocs))
	}
	if claimsSeen == 0 {
		t.Error("read ZERO size-label claims across every published doc. Every arm above is " +
			"then satisfied by finding nothing, which is indistinguishable from passing")
	}
	t.Logf("read %d size-label claims across %d published docs (%d of them teaching docs)",
		claimsSeen, len(docs), sweptTeaching)
}

// TestSizeLabelAliasesMatchPrefixedForms is the code-side half of the completeness
// ruling above. The docs are allowed to publish `size/xs` and say "or `xs`" in prose
// precisely because the two keys are guaranteed here to carry the same weight. If that
// guarantee ever breaks, the docs become wrong without a single doc changing — so the
// invariant the prose relies on is asserted rather than assumed.
func TestSizeLabelAliasesMatchPrefixedForms(t *testing.T) {
	checked := 0
	for label, w := range defaultSizeLabels {
		if !strings.HasPrefix(label, "size/") {
			continue
		}
		bare := strings.TrimPrefix(label, "size/")
		got, ok := defaultSizeLabels[bare]
		if !ok {
			t.Errorf("%q has no bare alias %q, but every published table tells readers the "+
				"`size/` prefix is optional", label, bare)
			continue
		}
		if got != w {
			t.Errorf("%q -> %s but its alias %q -> %s; the docs publish them as one row",
				label, fmtSizeWeight(w), bare, fmtSizeWeight(got))
		}
		checked++
	}
	if checked == 0 {
		t.Error("no `size/` keys found: this test asserted nothing")
	}

	// 🔴 THE REVERSE DIRECTION, and it closes a measured hole. Adding a BARE-ONLY key —
	// `"xxl": 13.0` — to defaultSizeLabels left the entire package green: the completeness
	// arm skips non-prefixed keys, the forward loop above never looks at it, and no
	// document publishes it so the contradiction arm has nothing to contradict. TIER would
	// have scored a PR labelled `xxl` at 13, off the fixed 0.5/1/3/5/8 scale and
	// undocumented in all six published sites, with the guard silent.
	for label := range defaultSizeLabels {
		if strings.HasPrefix(label, "size/") {
			continue
		}
		if _, ok := defaultSizeLabels["size/"+label]; !ok {
			t.Errorf("%q is a bare key with no `size/%s` counterpart. Every published table "+
				"presents the two forms as ONE row, so a bare-only key is a weight no "+
				"document describes and no completeness arm can miss.", label, label)
		}
	}
}

// publishedTextFiles lists every file that SHIPS PUBLICLY and could carry a weight
// table: the export roots the release recipe copies wholesale, plus the root-level
// files its allowlist names.
//
// 🔴 IT IS NOT `docs/*.md`, AND THAT WAS A REAL HOLE. An earlier version of this sweep
// walked docs/ plus five root files while its own comment claimed to list "every
// markdown file that ships publicly". Measured, it missed SEVEN published markdown files
// — deploy/DEMO.md (which the release recipe names explicitly as public), deploy/README.md,
// testdata/seam-jsonl-ingestion/README.md, internal/proxy/testdata/PROVENANCE.md, and the
// three .github templates. A PR template is a particularly plausible place to list size
// labels and their weights.
//
// 🔴 AND IT IS NOT MARKDOWN. config.example.yaml:295 publishes the COMPLETE built-in
// table — all ten keys — in a YAML comment, ships on the §2 allowlist, and was guarded by
// nothing: a code-side weight change reddened five markdown docs and left the published
// example silently wrong. That is #642's defect verbatim, one file over from where the
// guard was looking. So the sweep reads .md, .yaml and .yml, and the reader below has an
// arm for the inline form.
//
// The generated pages under internal/docs/html/ are deliberately skipped: they are
// rendered from docs/*.md and `make docs-html-check` (Makefile:94, wired into `check:` at
// :164) fails on drift, so guarding the markdown guards the HTML. That is a checked
// dependency, not an assumption — if the no-drift gate is removed, this stops covering
// the published HTML.
func publishedTextFiles(t *testing.T) []string {
	t.Helper()

	// 🔴 THE ROOT LIST IS WALKED, NOT HARDCODED. A literal list of root files is the same
	// "scoped to the files somebody knew about" failure #642 died of, one level up: add a
	// sixth exported root document tomorrow and a hardcoded sweep silently misses it.
	// Non-exported root files (session notes, the release runbook, CLAUDE.md) are swept too, and
	// that is deliberate — reading a file that does not ship costs nothing, while deciding
	// which ones ship is exactly the judgement that goes stale.
	var files []string
	rootEntries, err := os.ReadDir("../..")
	if err != nil {
		t.Fatalf("cannot read the repository root: %v", err)
	}
	for _, e := range rootEntries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".md") || n == "config.example.yaml" {
			files = append(files, "../../"+n)
		}
	}
	// The directories the release recipe copies wholesale.
	for _, root := range []string{
		"../../docs", "../../deploy", "../../testdata", "../../.github",
		"../../scripts", "../../tools", "../../cmd", "../../internal",
	} {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		werr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasSuffix(path, "internal/docs/html") {
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case strings.HasSuffix(path, ".md"),
				strings.HasSuffix(path, ".yaml"),
				strings.HasSuffix(path, ".yml"):
				files = append(files, path)
			}
			return nil
		})
		if werr != nil {
			t.Fatalf("cannot walk %s: %v", root, werr)
		}
	}
	// A floor, not a zero-check: a sweep that found three files would satisfy every arm
	// below by reading almost nothing, and "zero" is not the only vacuous number.
	if len(files) < 20 {
		t.Fatalf("published-file sweep found only %d files; it is not reaching the tree, "+
			"so every contradiction arm is inert", len(files))
	}
	return files
}
