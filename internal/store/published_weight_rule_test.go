package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 🔴 Why this file exists (#642, after #635).
//
// The retired log2 weight formula `min(8, max(0.5, ceil(log2(lines + files×10 + 1))))`
// was published long after GitHeuristic became a bucketed step function. #635 fixed
// README.md and added TestREADMEWeightTableMatchesGitHeuristic to pin it — but that
// guard covered README ONLY, and the same wrong formula was still live at three more
// sites, in docs/conventions.md and docs/how-it-works.md (twice), rendering on into two
// generated HTML pages. It also published a worked example claiming a 50-line, 3-file
// PR weighs 7 where GitHeuristic returns 3.0.
//
// Weight is the TIER NUMERATOR. A doc that overstates it overstates the metric, in
// documentation for a measurement tool.
//
// 🔑 THE DESIGN, and it is the opposite of the obvious one. A guard that walks the
// CODE's buckets asking "is each one mentioned somewhere?" passes a doc that also
// states three wrong ones, and passes a doc publishing a bucket the code no longer
// has. So this walks the DOC's claims instead: every `<= N -> W` a published doc
// states is fed to GitHeuristic, and the function decides. Completeness (every real
// bucket is published) is a second, separate arm.
//
// ⚠️ THE SAME RULE APPLIES TO THE OPEN-ENDED TOP BUCKET, and getting that wrong is how
// this guard shipped a hole. An earlier draft checked the terminal weight by PRESENCE
// — "does an `otherwise` line somewhere carry an 8?" — which is exactly the "a correct
// statement elsewhere satisfies it" defect this file killed for the four bounded
// buckets. Measured, it passed a doc publishing `otherwise → 9`. Terminal claims are
// now read positionally and judged one by one, like every other claim.
//
// The contradiction arm globs every published doc; the completeness arm is scoped to
// the files that teach the rule. #642's root cause was a guard scoped to the one file
// known to be wrong, so the arm that can find a NEW wrong file carries no list.

// weightRuleDocs are the files that must TEACH the rule — every bucket published, and
// the terminal weight. Every other published doc is still checked for contradictions;
// it simply is not required to state the rule. Paths are relative to this package dir.
var weightRuleDocs = []string{
	"../../README.md",
	"../../docs/conventions.md",
	"../../docs/how-it-works.md",
}

// bucket is one step of GitHeuristic: every effort <= maxEffort scores weight.
type bucket struct {
	maxEffort int
	weight    float64
}

// rule is what the code says, derived once and handed to every check.
type rule struct {
	steps      []bucket // bounded buckets, ascending
	terminal   float64  // weight above the last boundary
	multiplier int      // the coefficient on files in the effort proxy
}

// lastBoundary is the highest bounded threshold.
func (r rule) lastBoundary() int { return r.steps[len(r.steps)-1].maxEffort }

// Finding codes. The selftest asserts WHICH arm fired, not merely that something did —
// three earlier designs of this guard passed their own tests while checking the wrong
// thing, and a bare pass/fail cannot tell those apart.
const (
	codeBucket     = "bucket"      // a published bucket disagrees with GitHeuristic
	codeTerminal   = "terminal"    // a published `otherwise` weight disagrees
	codeArith      = "arith"       // a worked example's arithmetic is wrong
	codeAttrib     = "attrib"      // a worked example lands in the wrong bucket
	codeCoeff      = "coeff"       // the published files coefficient is wrong
	codeIncomplete = "incomplete"  // a teaching doc omits a bucket
	codeNoTerminal = "no-terminal" // a teaching doc never states the top bucket
)

type finding struct {
	line int // 1-based; 0 when the finding is about the file as a whole
	code string
	msg  string
}

// ---------------------------------------------------------------------------
// Deriving the rule from the code
// ---------------------------------------------------------------------------

// sweepTo bounds the effort domain discoverBuckets probes. discoverBuckets asserts the
// function has gone flat well before this, so a boundary moved above it fails loudly
// instead of silently truncating the bucket list.
const sweepTo = 20000

func discoverRule(t *testing.T) rule {
	t.Helper()
	steps, terminal := discoverBuckets(t)
	r := rule{steps: steps, terminal: terminal}
	r.multiplier = discoverFilesMultiplier(t, r)
	return r
}

// discoverBuckets sweeps GitHeuristic across the effort domain and reports the steps it
// actually implements, plus the terminal weight above the last boundary. It probes
// GitHeuristic(effort, 0) because effort = lines + files*multiplier, so a files-free
// probe addresses the effort axis directly.
func discoverBuckets(t *testing.T) ([]bucket, float64) {
	t.Helper()

	var steps []bucket
	prev := GitHeuristic(0, 0)
	for effort := 1; effort <= sweepTo; effort++ {
		if got := GitHeuristic(effort, 0); got != prev {
			steps = append(steps, bucket{maxEffort: effort - 1, weight: prev})
			prev = got
		}
	}
	if len(steps) == 0 {
		t.Fatal("GitHeuristic is constant across the whole sweep — it is no longer a " +
			"bucketed step function, and every doc this test guards describes a rule " +
			"the code does not implement")
	}
	// The sweep must comfortably outrun the last boundary, or `prev` is not the terminal
	// weight but merely the last value seen before the probe ran out.
	if last := steps[len(steps)-1].maxEffort; last > sweepTo/2 {
		t.Fatalf("last bucket boundary is %d but the sweep only reaches %d — raise "+
			"sweepTo. Until then the 'terminal' weight %v is unproven.", last, sweepTo, prev)
	}
	if far := GitHeuristic(sweepTo*50, 0); far != prev {
		t.Fatalf("weight is still moving past the sweep: %v at %d but %v at %d",
			prev, sweepTo, far, sweepTo*50)
	}
	if prev != MaxOutcomeWeight {
		t.Fatalf("GitHeuristic's top bucket returns %v but MaxOutcomeWeight is %v — the "+
			"label scale and the heuristic scale have diverged, and the docs publish "+
			"them as one scale", prev, MaxOutcomeWeight)
	}
	return steps, prev
}

// discoverFilesMultiplier derives the weight the effort proxy gives a changed FILE, by
// finding the m for which GitHeuristic(lines, files) is indistinguishable from
// GitHeuristic(lines+files*m, 0). Deriving it is what lets the worked-example
// arithmetic arm assert the published multiplier rather than assume it.
func discoverFilesMultiplier(t *testing.T, r rule) int {
	t.Helper()

	// 🔴 The grid must be DENSE, and uniqueness must be ASSERTED. A first draft took the
	// first m fitting a sparse probe list and returned 9 — wrong, and silently so,
	// because a coarse step function agrees with many multipliers unless the probes
	// straddle its boundaries. Sweeping every `lines` value past the last boundary puts
	// a probe either side of each one; demanding a single survivor turns "several fit"
	// into a loud failure instead of a guess. The ceiling is derived from the discovered
	// buckets rather than hardcoded — the hardcode is what let the 9 through.
	ceiling := r.lastBoundary() + 100
	var fits []int
	for m := 1; m <= 100; m++ {
		ok := true
		for files := 0; files <= 4 && ok; files++ {
			for lines := 0; lines <= ceiling; lines++ {
				if GitHeuristic(lines, files) != GitHeuristic(lines+files*m, 0) {
					ok = false
					break
				}
			}
		}
		if ok {
			fits = append(fits, m)
		}
	}
	if len(fits) != 1 {
		t.Fatalf("the files multiplier in `effort = lines + files*m` is not uniquely "+
			"determined by GitHeuristic: %v fit (probed lines 0..%d). Until exactly one "+
			"does, the effort formula the docs publish cannot be checked against the "+
			"code.", fits, ceiling)
	}
	return fits[0]
}

// ---------------------------------------------------------------------------
// Reading what a doc claims — and WHERE it is willing to read
// ---------------------------------------------------------------------------

// 🔑 SCOPE DECISION (round three, made on measurement rather than instinct).
//
// This reader deliberately does NOT parse free prose. Claims are read from three
// structured regions only: a bucket TABLE whose header names the weight column, a
// fenced CODE BLOCK that names the effort proxy, and a line carrying the worked-example
// anchor `effort = A + B × C = D`.
//
// The measurement that decided it: over the 28 published docs the broad free-prose
// reader found 17 claims — 10 in tables, 5 in code blocks, 2 on worked-example lines,
// and **0 in free prose**. Prose reading contributed nothing to real coverage. What it
// did contribute, across two review rounds, was a new FALSE POSITIVE each time:
// "$0.15 per run" read as a weight, "42% of the corpus" read as a weight, a year in an
// added table column read as a weight, and — the one that decided it — `otherwise` and
// `else` are ordinary English discourse words, so "…; otherwise the score is 3 buckets
// up" published a terminal weight of 3. docs/how-it-works.md already carries
// `otherwise` on a line full of weights, passing only because `effort`/`lines`/`files`
// happen to fall outside the glue list.
//
// The asymmetry is the whole argument. A false negative costs one class of miss, with
// the completeness arm and the log2 string ban still standing behind it. A false
// positive reddens a correct PR, and the guard is then deleted — taking the log2 ban
// with it, which is the arm that catches the defect that has now shipped twice. A
// narrower guard that cannot falsely accuse survives to keep catching what it was
// built for.

// A number, with optional thousands grouping.
var numberToken = regexp.MustCompile(`[0-9]{1,3}(?:,[0-9]{3})+(?:\.[0-9]+)?|[0-9]+(?:\.[0-9]+)?`)

// boundMarker matches the ways a doc opens an upper-bound claim, plus the threshold.
//
// ⚠️ THE ALTERNATION ORDER IS LOAD-BEARING, and it belongs to THIS regex rather than to
// numberToken: the grouped form must come first, or `≤ 1,000 → 5.0` reads as threshold
// 1. TestBucketClaims pins that exact string. The threshold is also captured WITH any
// decimal part so a fractional bound is recognised and skipped — Go's RE2 has no
// negative lookahead, so `([0-9]+)\b` read the `1` out of `quality <= 1.00` (measured,
// on docs/quality-degradation-spec.md).
var boundMarker = regexp.MustCompile(
	`(?:≤|<=|&le;|&#8804;|≦|=<)\s*([0-9]{1,3}(?:,[0-9]{3})+(?:\.[0-9]+)?|[0-9]+(?:\.[0-9]+)?)`)

// terminalMarker matches the words introducing the open-ended top bucket. `else` is
// word-anchored so it cannot match inside "elsewhere".
var terminalMarker = regexp.MustCompile(`(?i)\b(?:otherwise|else)\b`)

// Multiplication signs a doc might plausibly use. The docs use `×` today, so a routine
// typographic edit to `x`/`X` would otherwise silently disable the coefficient arm —
// the one arm whose absence let "a changed file weighs 20" pass all three docs.
const multSigns = `(?:×|✕|⨯|·|∗|\*|x)`

var effortArithmetic = regexp.MustCompile(
	`(?i)effort\s*=\s*([0-9]+)\s*\+\s*([0-9]+)\s*` + multSigns + `\s*([0-9]+)\s*=\s*([0-9]+)`)

var filesCoefficient = regexp.MustCompile(
	"(?i)files(?:_changed)?\\s*`?\\s*" + multSigns + `\s*([0-9]+)`)

// weightHeader identifies the column of a bucket table carrying the weight. Selecting
// the column by its HEADER rather than by position is what stops a NEW column from
// being mistaken for the weight: measured, a rightmost-numeric-cell rule read a year
// (`| ≤ 15 | 0.5 | 2024 |`) and a sample count as published weights, and accused a
// correct table. A table whose header names no weight column is not read at all, and
// the completeness arm then reports the rule as unstated — a loud, honest "I can no
// longer read your table", never a wrong-arithmetic accusation.
var weightHeader = regexp.MustCompile(`(?i)\b(?:weight|weights|points|score)\b`)

var pureNumberCell = regexp.MustCompile("^[\\s`*_]*([0-9]+(?:\\.[0-9]+)?)[\\s`*_]*$")

var separatorCell = regexp.MustCompile(`^[\s:|-]*$`)

func parseNum(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return v, err == nil
}

func namesRule(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "effort") || strings.Contains(l, "githeuristic")
}

// glueWords are the only words allowed between a threshold and the weight paired with
// it on the inline paths: a code-block line, or the weight cell of a table that states
// the whole rule in one cell.
//
// 🔴 This whitelist is the fix for the failure mode that gets a guard DELETED: taking
// "the first number after the threshold" reddened CORRECT prose. Measured: "effort ≤
// 200 the cost is $0.15 per run" was reported as a published weight of 0.15, and
// "effort ≤ 60 make up 42% of the corpus" as a weight of 42.
//
// ⚠️ It cuts both ways — residual gap (4) below. A WRONG claim whose connecting verb is
// off this list yields no claim rather than a finding. The verbs docs actually use to
// state this rule are listed; an unusual one is a silent miss, never a false accusation.
var glueWords = map[string]bool{
	"weight": true, "weights": true, "weighs": true, "weighted": true,
	"score": true, "scores": true, "scored": true, "scoring": true,
	"bucket": true, "buckets": true,
	"yields": true, "yield": true, "maps": true, "map": true, "becomes": true,
	"returns": true, "return": true, "rounds": true, "counts": true, "gives": true,
	"is": true, "are": true, "of": true, "to": true, "the": true, "a": true, "an": true,
	"gets": true, "get": true, "and": true, "then": true, "at": true, "in": true,
}

var letterRun = regexp.MustCompile(`[A-Za-z]+`)

// glueAdmissible reports whether the text between a marker and a candidate number is
// consistent with "this number is the weight for that bound".
func glueAdmissible(glue string) bool {
	if strings.ContainsAny(glue, "$#%") {
		return false
	}
	for _, w := range letterRun.FindAllString(glue, -1) {
		if !glueWords[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// tableCells splits a markdown table row into cells with their offsets, or returns nil
// when the line is not a table row. A leading blockquote marker is stripped so a quoted
// table is still read as a table.
func tableCells(line string) (cells []string, starts []int) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '>') {
		i++
	}
	if i >= len(line) || line[i] != '|' {
		return nil, nil
	}
	start := i + 1
	for j := start; j <= len(line); j++ {
		if j == len(line) || line[j] == '|' {
			cells = append(cells, line[start:j])
			starts = append(starts, start)
			start = j + 1
		}
	}
	if n := len(cells); n > 0 && strings.TrimSpace(cells[n-1]) == "" {
		cells, starts = cells[:n-1], starts[:n-1]
	}
	return cells, starts
}

// docCtx says whether a line may be read for claims, and how.
type docCtx struct {
	read      bool
	cells     []string
	starts    []int
	weightCol int // -1 => read inline (code block or worked example)
}

// inlineCtx is the context for a non-table readable line.
var inlineCtx = docCtx{read: true, weightCol: -1}

// ⚠️ RESIDUAL GAPS, stated in full because a partial list reads as a complete one.
// The reader does NOT see:
//  1. free prose — a bucket or terminal weight stated in a sentence outside the three
//     regions. This is the deliberate trade of the SCOPE DECISION above, not an
//     oversight;
//  2. a claim split across two lines (every check is line-scoped);
//  3. a weight stated BEFORE its threshold ("Weight 10.0 is awarded for effort <= 200");
//  4. a claim whose connecting verb is off the glueWords list — "yields"/"maps to" are
//     listed, "corresponds to" is not;
//  5. a bound written without a recognised marker ("under 201", "up to 200");
//  6. a bucket table whose Weight header has been renamed to something weightHeader
//     does not match.
//
// WHAT BACKSTOPS THEM, precisely. The completeness arm catches (1), (3), (5) and (6) on
// the three teaching docs, because it fails when a bucket goes unstated whatever the
// reason — a renamed header surfaces as `incomplete`, loudly, not as silence. The log2
// string ban and the retired-symbol ban glob all published docs regardless of region.
//
// WHAT IS GENUINELY UNCOVERED, said plainly: a teaching doc whose table is complete and
// correct can still carry a WRONG sentence about the rule in free prose, and nothing
// here will report it. That is the accepted cost of a guard that cannot falsely accuse.

// docContexts marks the readable regions of a document. See the SCOPE DECISION above
// for why these three shapes, and only these three.
func docContexts(lines []string) []docCtx {
	ctx := make([]docCtx, len(lines))
	for i := range ctx {
		ctx[i].weightCol = -1
	}

	// 1. Fenced code blocks that name the rule.
	isFence := func(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "```") }
	for i := 0; i < len(lines); i++ {
		if !isFence(lines[i]) {
			continue
		}
		j := i + 1
		for j < len(lines) && !isFence(lines[j]) {
			j++
		}
		named := false
		for k := i + 1; k < j && k < len(lines); k++ {
			if namesRule(lines[k]) {
				named = true
				break
			}
		}
		if named {
			for k := i + 1; k < j && k < len(lines); k++ {
				ctx[k].read = true
			}
		}
		i = j
	}

	// 2. Tables whose header names a weight column and which mention the rule.
	for i := 0; i < len(lines); i++ {
		hCells, _ := tableCells(lines[i])
		if hCells == nil {
			continue
		}
		j := i
		for j < len(lines) {
			if c, _ := tableCells(lines[j]); c == nil {
				break
			}
			j++
		}
		col := -1
		for k, c := range hCells {
			if weightHeader.MatchString(c) {
				col = k
				break
			}
		}
		named := namesRule(lines[i])
		for k := i + 1; k < j; k++ {
			if namesRule(lines[k]) {
				named = true
			}
		}
		if col >= 0 && named {
			for k := i + 1; k < j; k++ {
				cells, starts := tableCells(lines[k])
				if len(cells) == 0 || col >= len(cells) {
					continue
				}
				sep := true
				for _, c := range cells {
					if !separatorCell.MatchString(c) {
						sep = false
						break
					}
				}
				if sep {
					continue
				}
				ctx[k] = docCtx{read: true, cells: cells, starts: starts, weightCol: col}
			}
		}
		i = j - 1
	}

	// 3. Worked-example lines, anchored on the arithmetic itself.
	for i, line := range lines {
		if !ctx[i].read && effortArithmetic.MatchString(line) {
			ctx[i].read = true
		}
	}
	return ctx
}

// inlineWeight takes the first number after markerEnd whose glue admits it. Numbers
// that are themselves thresholds are skipped: a number under its own bound marker
// belongs to a boundary list (…`≤ 15`, `≤ 60`…), never to the bound before it.
func inlineWeight(line string, markerEnd int) (float64, bool) {
	isThreshold := map[int]bool{}
	for _, loc := range boundMarker.FindAllStringSubmatchIndex(line, -1) {
		isThreshold[loc[2]] = true
	}
	for _, tl := range numberToken.FindAllStringIndex(line, -1) {
		if tl[0] < markerEnd || isThreshold[tl[0]] {
			continue
		}
		if tl[0] > 0 && letterRun.MatchString(line[tl[0]-1:tl[0]]) {
			continue // `v1.2`: a number welded to a word is not a weight
		}
		if tl[1] < len(line) && line[tl[1]] == '%' {
			continue
		}
		if !glueAdmissible(line[markerEnd:tl[0]]) {
			return 0, false
		}
		return parseNum(line[tl[0]:tl[1]])
	}
	return 0, false
}

// weightAtMarker resolves the weight a readable line assigns at a marker.
//
// In a bucket table the weight comes from the column the HEADER designates. When the
// marker sits inside that very column — README states the whole rule inside one cell —
// the cell is read inline, bounded to the cell so a neighbouring column cannot leak in.
func weightAtMarker(line string, ctx docCtx, markerEnd int) (float64, bool) {
	if ctx.weightCol < 0 {
		return inlineWeight(line, markerEnd)
	}
	marker := -1
	for i := range ctx.cells {
		if markerEnd > ctx.starts[i] && markerEnd <= ctx.starts[i]+len(ctx.cells[i]) {
			marker = i
			break
		}
	}
	if marker < 0 {
		return 0, false
	}
	if marker != ctx.weightCol {
		if m := pureNumberCell.FindStringSubmatch(ctx.cells[ctx.weightCol]); m != nil {
			return parseNum(m[1])
		}
		return 0, false
	}
	end := ctx.starts[marker] + len(ctx.cells[marker])
	return inlineWeight(line[:end], markerEnd)
}

// bucketClaims extracts every bucket claim a readable line makes.
func bucketClaims(line string, ctx docCtx) []bucket {
	if !ctx.read {
		return nil
	}
	var out []bucket
	for _, loc := range boundMarker.FindAllStringSubmatchIndex(line, -1) {
		thr, err := strconv.Atoi(strings.ReplaceAll(line[loc[2]:loc[3]], ",", ""))
		if err != nil {
			continue // a fractional bound is not an effort threshold
		}
		if w, ok := weightAtMarker(line, ctx, loc[3]); ok {
			out = append(out, bucket{maxEffort: thr, weight: w})
		}
	}
	return out
}

// terminalClaims extracts every weight a readable line assigns to the top bucket.
func terminalClaims(line string, ctx docCtx) []float64 {
	if !ctx.read {
		return nil
	}
	var out []float64
	for _, loc := range terminalMarker.FindAllStringIndex(line, -1) {
		if w, ok := weightAtMarker(line, ctx, loc[1]); ok {
			out = append(out, w)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The checks
// ---------------------------------------------------------------------------

// checkDoc returns every contradiction a doc's lines contain, the buckets it states,
// and whether it states the terminal weight. Splitting this out from the test body is
// what makes the arm-selftest below possible: it drives the same code the real sweep
// does, over synthetic documents.
func checkDoc(r rule, lines []string) (out []finding, stated map[bucket]bool, terminalStated bool) {
	stated = map[bucket]bool{}
	ctxs := docContexts(lines)

	for i, line := range lines {
		ctx := ctxs[i]
		n := i + 1

		for _, c := range bucketClaims(line, ctx) {
			want := GitHeuristic(c.maxEffort, 0)
			if c.weight != want {
				out = append(out, finding{n, codeBucket, fmt.Sprintf(
					"publishes `effort <= %d → weight %s`, but GitHeuristic returns %s for "+
						"that effort. Weight is the TIER numerator, so a published bucket "+
						"that overstates it overstates the metric:\n\t%s",
					c.maxEffort, fmtWeight(c.weight), fmtWeight(want), strings.TrimSpace(line))})
				continue
			}
			stated[c] = true
		}

		// The open-ended top bucket, judged claim by claim rather than by presence.
		for _, w := range terminalClaims(line, ctx) {
			above := GitHeuristic(r.lastBoundary()+1, 0)
			if w != above {
				out = append(out, finding{n, codeTerminal, fmt.Sprintf(
					"publishes `otherwise → %s`, but GitHeuristic returns %s for any effort "+
						"above %d:\n\t%s",
					fmtWeight(w), fmtWeight(above), r.lastBoundary(), strings.TrimSpace(line))})
				continue
			}
			terminalStated = true
		}

		// The coefficient and worked-example arms stay BROAD — unlike a bare `<= N`,
		// their shapes (`files × N`, `effort = A + B × C = D`) are specific enough that
		// prose cannot imitate them, so they cost no false positives and they cover the
		// prose sentence at docs/how-it-works.md that states the proxy symbolically.
		if !namesRule(line) && !ctx.read {
			continue
		}
		for _, m := range filesCoefficient.FindAllStringSubmatch(line, -1) {
			if k, err := strconv.Atoi(m[1]); err == nil && k != r.multiplier {
				out = append(out, finding{n, codeCoeff, fmt.Sprintf(
					"publishes `files × %d`, but GitHeuristic weighs a changed file as %d. "+
						"The effort proxy is the denominator of every worked example on this "+
						"page:\n\t%s", k, r.multiplier, strings.TrimSpace(line))})
			}
		}

		// Worked-example arithmetic, checked rather than trusted. #642's published
		// example was an ARITHMETIC claim, and no amount of bucket-table checking
		// reaches one.
		for _, m := range effortArithmetic.FindAllStringSubmatch(line, -1) {
			nlines, _ := strconv.Atoi(m[1])
			nfiles, _ := strconv.Atoi(m[2])
			mult, _ := strconv.Atoi(m[3])
			effort, _ := strconv.Atoi(m[4])
			if mult != r.multiplier {
				out = append(out, finding{n, codeCoeff, fmt.Sprintf(
					"publishes `%d + %d × %d`, but GitHeuristic weighs a changed file as "+
						"%d:\n\t%s", nlines, nfiles, mult, r.multiplier, strings.TrimSpace(line))})
			}
			if got := nlines + nfiles*mult; got != effort {
				out = append(out, finding{n, codeArith, fmt.Sprintf(
					"publishes `%d + %d × %d = %d`, which is arithmetically %d:\n\t%s",
					nlines, nfiles, mult, effort, got, strings.TrimSpace(line))})
			}
			// The bucket the example attributes the effort to must be the one that
			// effort actually lands in — a worked example can be built entirely out of
			// individually correct pairs and still send the reader to the wrong bucket.
			if idx := strings.Index(line, m[0]); idx >= 0 {
				if cs := bucketClaims(line[idx+len(m[0]):], inlineCtx); len(cs) > 0 {
					if GitHeuristic(effort, 0) != GitHeuristic(cs[0].maxEffort, 0) {
						out = append(out, finding{n, codeAttrib, fmt.Sprintf(
							"attributes effort %d to the `<= %d` bucket, but effort %d scores "+
								"%s while that bucket scores %s:\n\t%s",
							effort, cs[0].maxEffort, effort, fmtWeight(GitHeuristic(effort, 0)),
							fmtWeight(GitHeuristic(cs[0].maxEffort, 0)), strings.TrimSpace(line))})
					}
				}
			}
		}
	}
	return out, stated, terminalStated
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

func TestPublishedWeightRuleMatchesGitHeuristic(t *testing.T) {
	r := discoverRule(t)
	t.Logf("GitHeuristic implements %d buckets, terminal weight %s, effort = lines + files*%d",
		len(r.steps), fmtWeight(r.terminal), r.multiplier)

	files := publishedDocFiles(t)
	t.Logf("sweeping %d published docs", len(files))
	if len(files) < 10 {
		t.Fatalf("only %d published docs found — the glob is not reaching docs/, so a "+
			"pass here would mean nothing", len(files))
	}

	// ARM 1 (globbed) — no published doc may CONTRADICT the function.
	statedBy := map[string]map[bucket]bool{}
	terminalBy := map[string]bool{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		fs, stated, term := checkDoc(r, strings.Split(string(raw), "\n"))
		statedBy[path], terminalBy[path] = stated, term
		for _, f := range fs {
			t.Errorf("[%s] %s:%d %s", f.code, path, f.line, f.msg)
		}
	}

	// ARM 2 (scoped) — the docs that teach the rule must publish EVERY bucket and the
	// terminal weight.
	for _, path := range weightRuleDocs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Logf("states %d of %d buckets", len(statedBy[path]), len(r.steps))
			for _, b := range r.steps {
				if !statedBy[path][b] {
					t.Errorf("[%s] %s never states that effort <= %d scores weight %s, in a "+
						"form this guard reads: a table whose header names a Weight column, a "+
						"fenced code block naming `effort`, or a line carrying "+
						"`effort = A + B x C = D`. Either the bucket is missing, or the rule "+
						"moved into free prose, which is deliberately out of scope.",
						codeIncomplete, path, b.maxEffort, fmtWeight(b.weight))
				}
			}
			if !terminalBy[path] {
				t.Errorf("[%s] %s never states the terminal weight %s that GitHeuristic "+
					"returns above effort %d, in a form this guard reads (see the "+
					"incomplete-bucket message above for the shapes) — the published rule "+
					"stops short of the value that scores the largest PRs.",
					codeNoTerminal, path, fmtWeight(r.terminal), r.lastBoundary())
			}
		})
	}

	// ARM 3 — the retired formula must not come back in ANY published doc.
	//
	// The ban is deliberately unconditional. `tier-outcome-weight-algorithm.md` (the
	// deferred v2 scorer) uses log2 legitimately, but it lives at the repo root and is
	// stripped from the public export — if it is ever promoted into docs/, this fails
	// LOUDLY and a human decides, the correct polarity for a formula that has now
	// shipped wrong twice. The spellings are listed because banning `log2(` alone bans
	// one typography, not the idea.
	t.Run("no_log2_in_published_docs", func(t *testing.T) {
		spellings := []string{"log2", "log₂", "log_2", "binary logarithm", "log base 2",
			"logarithm base 2", "base-2 logarithm"}
		t.Logf("swept %d published docs for %d spellings of the retired formula",
			len(files), len(spellings))
		for _, path := range files {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}
			lower := strings.ToLower(string(raw))
			for _, s := range spellings {
				if strings.Contains(lower, s) {
					t.Errorf("%s publishes a log2 weight formula (%q). GitHeuristic is a "+
						"bucketed step function on `effort = lines + files*%d`, and store.go "+
						"records that the buckets REPLACED the log2 form. Publishing the old "+
						"one overstates the TIER numerator (measured: the retired formula "+
						"gives 7 for a 50-line/3-file PR where GitHeuristic returns 3.0, and "+
						"8 for a 100-line/4-file PR where it also returns 3.0).",
						path, s, r.multiplier)
				}
			}
		}
	})

	// ARM 4 — retired SYMBOL names, the same defect class one level down.
	//
	// 🔴 Why this exists. The formula ban would not have caught `labelWeight`: the docs
	// kept naming it in a sequence diagram and a table long after #301 consolidated it
	// into prderive.SizeWeight, sending readers to a function that is not there. The
	// review of #642's own fix found it — the numeric ban was scoped to numbers, exactly
	// as #635's ban was scoped to README.
	t.Run("no_retired_symbols_in_published_docs", func(t *testing.T) {
		retired := []struct{ name, replacement string }{
			{"labelWeight", "prderive.SizeWeight"},
		}
		// Prove each name is STILL retired before banning it, so the list cannot stand on
		// a stale premise and start failing docs that are correct.
		for _, x := range retired {
			if where := declaresFunc(t, x.name); where != "" {
				t.Fatalf("%q is on the retired list but %s declares it again. Either the "+
					"symbol came back (drop it from the list) or something was renamed into "+
					"it; do not leave the ban standing on a stale premise.", x.name, where)
			}
		}
		t.Logf("swept %d published docs for %d retired symbol name(s)", len(files), len(retired))
		for _, path := range files {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}
			for _, x := range retired {
				if strings.Contains(string(raw), x.name) {
					t.Errorf("%s names %q, which no longer exists in the tree — use %s. A "+
						"published doc pointing at a removed symbol sends a reader to code "+
						"that is not there, which is how the retired weight formula survived "+
						"two fixes.", path, x.name, x.replacement)
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// funcDecl matches a package-level function OR a method with the given name. Methods
// matter: were `labelWeight` to return as `func (h *Handler) labelWeight(`, a
// plain-prefix matcher would miss it, the ban would stay in force on a false premise,
// and a doc correctly naming the revived method would be the thing that errored.
func funcDecl(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?` + regexp.QuoteMeta(name) + `\s*\(`)
}

// declaresFunc reports the first Go file under internal/ or cmd/ that declares a
// function or method with the given name, or "" if none does. It is how the
// retired-symbol ban proves its own premise rather than asserting it in a comment.
func declaresFunc(t *testing.T, name string) string {
	t.Helper()

	re := funcDecl(name)
	var found string
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || found != "" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if re.Match(raw) {
				found = path
			}
			return nil
		})
		if err != nil {
			t.Fatalf("cannot walk %s: %v", root, err)
		}
	}
	return found
}

// publishedDocFiles lists every markdown file that ships to the public repository:
// docs/ (including subdirectories) plus the root-level files the release recipe exports.
//
// The generated pages under internal/docs/html/ are deliberately NOT swept: they are
// rendered from docs/*.md and `make docs-html-check` fails on any drift, so guarding
// the markdown guards the HTML. That is a real dependency, not an assumption — if the
// no-drift gate is ever removed, this sweep stops covering the published HTML.
func publishedDocFiles(t *testing.T) []string {
	t.Helper()

	var files []string
	for _, name := range []string{
		"README.md", "SECURITY.md", "CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "CHANGELOG.md",
	} {
		path := "../../" + name
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	err := filepath.WalkDir("../../docs", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk docs/: %v", err)
	}
	return files
}
