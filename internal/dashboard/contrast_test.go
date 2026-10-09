package dashboard

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// --- WCAG contrast, measured from the SERVED stylesheet (#534) ----------------
//
// Nothing in this repo measured colour contrast before this file. #534 was filed
// after a review spotted `--text-faint` by eye; the token then sat failing AA in
// BOTH themes for weeks because there was no gate that could notice.
//
// 🔴 Two things this guard exists to prevent, both of which already happened:
//
//  1. **A fix measured against the wrong background.** #534's own recorded remedy
//     (`#6e7781` for light) was computed against `--surface` alone and measures
//     4.20:1 on `--bg` — where `#provenance`, the price-table provenance stamp,
//     actually sits. It would have shipped as a fix and still failed. A token is
//     only as accessible as its WORST background, so this test enumerates the
//     backgrounds from the CSS rather than trusting one.
//
//  2. **A silent regression.** Any future palette edit that darkens a surface or
//     lightens a token now fails here with the measured ratio, instead of being
//     found by the next person who happens to look.
//
// The bar is WCAG 2.1 SC 1.4.3: 4.5:1 for body text, 3.0:1 for "large" text
// (>=24px, or >=18.66px when bold) and for non-text UI boundaries (SC 1.4.11).

// contrastRatio implements the WCAG 2.1 relative-luminance formula.
//
// It returns 0 for anything it cannot parse rather than panicking. That is not
// defensive noise: the first draft indexed hex[0:2] unguarded, so a token missing from
// the palette map produced `slice bounds out of range` — which aborts the whole test
// BINARY, taking every other test in this package with it and reporting a slice error
// instead of the missing token. 0 is also the fail-closed answer: it is below every
// bar, so an unparseable colour fails loudly at the assertion that cares.
func contrastRatio(a, b string) float64 {
	lum := func(hex string) float64 {
		hex = strings.TrimPrefix(hex, "#")
		// Accept the 3-digit form by expanding it. A palette written #999 used to fall
		// out of the token map entirely and take its rules' assertions with it.
		if len(hex) == 3 {
			hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
		}
		if len(hex) != 6 {
			return math.NaN()
		}
		ch := func(i int) float64 {
			v, _ := strconv.ParseInt(hex[i:i+2], 16, 0)
			c := float64(v) / 255.0
			if c <= 0.03928 {
				return c / 12.92
			}
			return math.Pow((c+0.055)/1.055, 2.4)
		}
		return 0.2126*ch(0) + 0.7152*ch(2) + 0.0722*ch(4)
	}
	hi, lo := lum(a), lum(b)
	if math.IsNaN(hi) || math.IsNaN(lo) {
		return 0
	}
	if hi < lo {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

var (
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssTokenRe   = regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{3}(?:[0-9a-fA-F]{3})?)\b`)
	cssRuleRe    = regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
	cssColorRe   = regexp.MustCompile(`(?:^|[;{\s])color:\s*var\(--([a-z0-9-]+)\)`)
	cssSizeRe    = regexp.MustCompile(`font-size:\s*([0-9.]+)(rem|px)`)
	// 700+ only: WCAG's large-text carve-out says BOLD, and 600 is semibold. Including
	// it would hand a 3.0 bar to text that has not earned it.
	cssWeightRe = regexp.MustCompile(`font-weight:\s*(700|800|900|bold)`)
)

// themeTokens pulls one theme's token table out of the stylesheet.
//
// ⚠️ The end markers must be text that SURVIVES comment stripping. A first draft
// closed the light block on `/* Respect the OS preference`, which this file strips
// before parsing — so the marker was never found, every theme lookup t.Fatal'd, and
// the guard could not run at all. It failed loudly, which is the only reason it was
// not mistaken for a pass. The two themes
// are read SEPARATELY and asserted separately: a token that passes in dark and fails
// in light is exactly the defect #534 was filed for, and averaging or merging them
// would hide it.
func themeTokens(t *testing.T, style, open, closeAt string) map[string]string {
	t.Helper()
	i := strings.Index(style, open)
	if i < 0 {
		t.Fatalf("theme block %q not found in the served stylesheet", open)
	}
	j := strings.Index(style[i:], closeAt)
	if j < 0 {
		t.Fatalf("end of theme block %q not found", open)
	}
	out := map[string]string{}
	for _, m := range cssTokenRe.FindAllStringSubmatch(style[i:i+j], -1) {
		out[m[1]] = m[2]
	}
	if len(out) < 10 {
		t.Fatalf("theme block %q yielded only %d tokens — the parser is not reading the "+
			"stylesheet and every assertion below would be vacuous", open, len(out))
	}
	return out
}

// knownFailing records tokens measured BELOW AA that this issue deliberately does not
// change, with the ratio measured on 2026-08-05. They are NOT silently skipped: each
// is asserted not to get WORSE, so the exemption is live rather than a dead line that
// reads as reviewed while covering nothing.
//
// 🔴 The baseline is the WORST background, not a convenient one. The first draft of
// this table recorded the --surface figures (4.18 / 4.13) and the guard immediately
// reported both tokens as "REGRESSED" on --bg (3.85 / 3.81) — i.e. this file
// committed, inside itself, the very defect it was written to catch. An exemption
// measured against one background forgives less than it appears to.
//
// ⚠️ Neither is a "wontfix". Both are real AA failures in the published dashboard and
// both need their own decision, because changing them is a BRAND change rather than a
// contrast fix:
//   - `--trust` is the amber that means "investigate"; #534's own body names it and
//     says it belongs with the token review.
//   - `--yield` is the green this product reserves exclusively for return-on-token.
//     Darkening it changes the one colour the dashboard's whole visual language is
//     built on, which is a design ruling, not an a11y patch.
//
// The key is "theme/token". There is deliberately no separate `theme` field: an
// earlier draft carried one and compared it, which is always true when the key already
// encodes the theme — a check that cannot fail, reading as though it can.
var knownFailing = map[string]struct {
	measured float64
	why      string
}{
	"light/trust": {3.85, "#b56a06; worst of the checked backgrounds is --bg (3.85), --surface is 4.18. #a35f05 measures 4.71 on --surface and is a candidate — named in #534's own body"},
	"light/yield": {3.81, "#1a8f4e; worst of the checked backgrounds is --bg (3.81), --surface is 4.13. GREEN is reserved for yield, so changing it is a brand decision, not an a11y patch"},
}

// largeTextAllow names the selectors permitted the WCAG large-text bar (3.0:1). It is
// an allowlist rather than a computation because the computation is not sound: the
// font-size is read from the declaring rule, and the cascade can shrink text elsewhere.
var largeTextAllow = map[string]bool{
	".ub-exploratory-value": true,
}

func TestDashboard_TextTokensMeetWCAG_AA(t *testing.T) {
	style := cssCommentRe.ReplaceAllString(servedStyle(t), "")

	themes := map[string]map[string]string{
		"dark":  themeTokens(t, style, ":root {", ":root[data-theme=\"light\"]"),
		"light": themeTokens(t, style, ":root[data-theme=\"light\"] {", "@media (prefers-color-scheme"),
	}

	// 🔴 THE THIRD DECLARATION SITE. The light palette is written TWICE — once in
	// :root[data-theme="light"] for the toggle, and once in the
	// @media (prefers-color-scheme: light) fallback that governs FIRST PAINT. The light
	// table above is sliced up to the @media marker, so the fallback lies entirely
	// outside every assertion in this file.
	//
	// Measured, not assumed: reverting ONLY the fallback's --text-faint to the failing
	// #99a2ac (2.39:1) left this whole file reporting `ok`. The guard's input was
	// byte-identical, so it could not have done anything else. That is the exact defect
	// #534's own commit message names as the risk — "changing one and not the other
	// would fix the theme toggle and leave first paint failing" — and the guard written
	// alongside it could not see it. The `checked < 40` arm does not help: the light
	// table is fully populated, just the WRONG light table.
	//
	// Equality against the already-measured block is the right invariant and the cheap
	// one: every ratio asserted below then holds for first paint too, for all 14 tokens
	// rather than only the one this issue touched.
	firstPaint := themeTokens(t, style, ":root:not([data-theme]) {", "}")
	for tok, want := range themes["light"] {
		if got := firstPaint[tok]; got != want {
			t.Errorf("--%s is %s under :root[data-theme=\"light\"] but %s in the "+
				"prefers-color-scheme fallback. The toggle and FIRST PAINT disagree, so every "+
				"contrast ratio proved below is proved only for the toggled theme — a palette "+
				"fixed in two of its three declaration sites is the defect, not the fix.",
				tok, want, got)
		}
	}

	// The backgrounds a text role can sit on. THREE of the seven faint text roles are
	// on --bg, not one: #provenance, plus .cmp-windows .cmp-arrow and .cmp-mode —
	// #cmp-windows is a direct child of #compare-view, which sets only `display`. The
	// other four are inside panels, which all declare --surface.
	//
	// ⚠️ --surface-2 is deliberately NOT here, and the reason is measured rather than
	// convenient. It is darker than --surface, so requiring --text-faint to clear 4.5:1
	// on it forces dark #808891 — whose separation from --text-muted falls to 1.15:1,
	// below the ~1.2 at which the tier stops reading as deliberate. In other words the
	// blanket list would assert a requirement that CANNOT be met without deleting the
	// tier, to protect an arrangement that does not exist: --surface-2 has exactly one
	// consumer and its text is --text, which clears 13:1.
	//
	// The real risk — someone putting faint text inside a --surface-2 container later —
	// is closed structurally by TestDashboard_SurfaceTwoHasOneConsumer below, which
	// fails the moment that assumption stops holding. That is the honest shape: assert
	// the thing that is true, and guard the assumption it rests on.
	backgrounds := []string{"bg", "surface"}

	checked := 0
	perToken := map[string]int{}
	// An INDEPENDENT count, with a deliberately LOOSER pattern than the one the loop
	// uses. Every form the strict regex would miss is legal CSS a person could write
	// innocently — `var( --x )` with spaces, and `var(--x, #999)` with a fallback — and
	// each was measured erasing all seven --text-faint rules while `checked` stayed
	// comfortably above its floor. Comparing a loose count against the strict one turns
	// "the parser silently stopped seeing a category" into a named failure.
	looseColorRe := regexp.MustCompile(`(?:^|[;{\s])color:\s*var\(\s*--([a-z0-9-]+)`)
	looseCount := 0
	for _, rule := range cssRuleRe.FindAllStringSubmatch(style, -1) {
		looseCount += len(looseColorRe.FindAllString(rule[2], -1))
	}

	strictCount := 0
	for _, rule := range cssRuleRe.FindAllStringSubmatch(style, -1) {
		sel, body := strings.TrimSpace(rule[1]), rule[2]
		m := cssColorRe.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		strictCount++
		tok := m[1]

		// The bar depends on size. When a rule inherits its size we take the STRICTER
		// 4.5 — assuming "large" would be assuming the thing that lowers the bar.
		bar, kind := 4.5, "body text"
		if sm := cssSizeRe.FindStringSubmatch(body); sm != nil {
			px, _ := strconv.ParseFloat(sm[1], 64)
			if sm[2] == "rem" {
				px *= 16.0
			}
			if px >= 24 || (px >= 18.66 && cssWeightRe.MatchString(body)) {
				// 🔴 OPT-IN ONLY. The size is read from the DECLARING rule, so a later, more
				// specific rule that shrinks the text would buy a silent 4.5 -> 3.0 downgrade
				// — two real failures measured disappearing that way. Requiring the selector
				// to be named here means the downgrade is a decision someone made, not a
				// side effect of cascade order.
				if !largeTextAllow[sel] {
					t.Errorf("%s declares font-size %.0fpx, which would take the WCAG large-text "+
						"bar (3.0:1) — but it is not in largeTextAllow. The bar is read from this "+
						"rule alone, so a more specific rule shrinking the text would lower the bar "+
						"silently. Add it deliberately, or keep the 4.5:1 bar.", sel, px)
				} else {
					bar, kind = 3.0, "large text"
				}
			}
		}

		for themeName, tokens := range themes {
			fg, ok := tokens[tok]
			if !ok {
				// NOT a silent skip. A `color: var(--x)` naming something no theme declares
				// is either broken CSS or a token this guard can no longer see — and
				// "stopped looking" is the failure mode this whole file exists to prevent.
				t.Errorf("%s uses color: var(--%s), which the %s theme does not declare. Either "+
					"the rule is broken or the token parser stopped seeing it; both make every "+
					"assertion about this rule vacuous", sel, tok, themeName)
				continue
			}
			// An exempted token is judged ONCE, on its WORST background — not per
			// background. The baseline is a property of the token, so comparing it against
			// each background in turn made it a property of the background LIST: widening
			// that list re-broke the baseline twice in a row (first when --bg was added
			// beside --surface, then again when --surface-2 was). Same defect both times,
			// and the same one #534's recorded remedy had. Judge the worst, once.
			if kf, exempt := knownFailing[themeName+"/"+tok]; exempt {
				worst, worstBg := math.Inf(1), ""
				for _, bgName := range backgrounds {
					if r := contrastRatio(fg, tokens[bgName]); r < worst {
						worst, worstBg = r, bgName
					}
				}
				checked += len(backgrounds)
				perToken[tok] += len(backgrounds)
				if worst < kf.measured-0.01 {
					t.Errorf("%s: --%s in the %s theme has REGRESSED to %.2f:1 on --%s (was %.2f "+
						"when exempted). %s", sel, tok, themeName, worst, worstBg, kf.measured, kf.why)
				}
				// 🔴 THE EXPIRY ARM. Without it, fixing an exempted token produces SILENCE:
				// the entry survives and thereafter licenses a slide back down to the old
				// floor. An exemption with no exit is a permanent floor-lowering, which is
				// the opposite of what this table says it is for — and a dead exemption
				// reads as reviewed while covering nothing.
				if worst >= bar {
					t.Errorf("--%s in the %s theme now measures %.2f:1 on its worst background "+
						"and PASSES the %.1f:1 bar. Its knownFailing exemption is STALE — delete "+
						"the entry (and close the issue tracking it) rather than leaving a floor "+
						"that permits a regression back to %.2f:1.",
						tok, themeName, worst, bar, kf.measured)
				}
				continue
			}

			for _, bgName := range backgrounds {
				bg := tokens[bgName]
				if bg == "" {
					t.Fatalf("theme %s has no --%s; the comparison would be against an empty "+
						"string and every ratio meaningless", themeName, bgName)
				}
				got := contrastRatio(fg, bg)
				checked++
				perToken[tok]++
				if got < bar {
					t.Errorf("%s\n    --%s (%s) on --%s (%s) in the %s theme = %.2f:1, want >= %.1f:1 "+
						"(%s, WCAG 2.1 SC 1.4.3).\n    A token is only as accessible as its WORST "+
						"background — measuring against one and shipping is how #534's own recorded "+
						"remedy was wrong.", sel, tok, fg, bgName, bg, themeName, got, bar, kind)
				}
			}
		}
	}

	// 🔴 THE NON-EMPTY ARM. Every assertion above lives inside a loop over parsed
	// rules; if the parser matched nothing they are all vacuously satisfied and this
	// test reports a confident green about nothing.
	// 🔴 THE NON-EMPTY ARMS, and a bare total is not enough. `checked` is dominated by
	// --text-muted's ~30 rules, so it stays far above any global floor even when every
	// single --text-faint rule silently drops out of the parse. Measured: two innocent
	// CSS edits did exactly that at 264 pairs.
	if strictCount != looseCount {
		t.Errorf("the strict colour matcher found %d declarations but a looser one found %d. "+
			"The difference is rules written in a form this guard cannot see — `var( --x )` "+
			"with spaces, or `var(--x, #fallback)` — each of which silently removes a whole "+
			"category from every assertion above while the totals stay plausible",
			strictCount, looseCount)
	}
	for _, tok := range []string{"text-faint", "text-muted", "trust", "yield", "danger"} {
		if perToken[tok] < 2 {
			t.Errorf("--%s produced %d checked pairs. The parser is not seeing its rules, so "+
				"every assertion about that token passed vacuously — which a global total "+
				"cannot detect, because the other tokens keep it high", tok, perToken[tok])
		}
	}
	t.Logf("checked %d colour/background pairs across both themes", checked)
}

// TestDashboard_TextFaintIsStillADistinctTier is the CONTROL ARM for the fix, and it
// is what makes #534's stated hypothesis testable rather than a matter of taste.
//
// #534 argued the faint tier "may simply not be viable at AA" — that darkening it far
// enough to pass would make it indistinguishable from --text-muted, leaving a token
// with no job. That is a measurable claim, and it is FALSE: at the minimum passing
// values the two tiers remain ~1.25:1 apart, above the ~1.2 at which a step reads as
// deliberate rather than as a rendering artefact.
//
// Without this arm, "fix the contrast" is satisfied by setting --text-faint equal to
// --text-muted, which passes every assertion in the test above while silently deleting
// a tier the design uses in seven places.
func TestDashboard_TextFaintIsStillADistinctTier(t *testing.T) {
	style := cssCommentRe.ReplaceAllString(servedStyle(t), "")
	for _, tc := range []struct{ name, open, closeAt string }{
		{"dark", ":root {", ":root[data-theme=\"light\"]"},
		{"light", ":root[data-theme=\"light\"] {", "@media (prefers-color-scheme"},
	} {
		tokens := themeTokens(t, style, tc.open, tc.closeAt)
		faint, muted := tokens["text-faint"], tokens["text-muted"]
		if faint == "" || muted == "" {
			t.Fatalf("%s theme: --text-faint=%q --text-muted=%q; one of the two tiers is gone",
				tc.name, faint, muted)
		}
		if faint == muted {
			t.Errorf("%s theme: --text-faint and --text-muted are the SAME colour (%s). The "+
				"contrast test is satisfied by collapsing the tier, which deletes a distinction "+
				"the design uses in seven roles — if that is the intent it needs a ruling, not a "+
				"token edit", tc.name, faint)
			continue
		}
		sep := contrastRatio(faint, muted)
		if sep < 1.2 {
			t.Errorf("%s theme: --text-faint (%s) and --text-muted (%s) differ by only %.2f:1. "+
				"Below ~1.2 the step stops reading as a deliberate tier and reads as a rendering "+
				"artefact — #534's hypothesis was that AA forces exactly this collapse",
				tc.name, faint, muted, sep)
		}
		t.Logf("%s: faint %s vs muted %s = %.2f:1 separation", tc.name, faint, muted, sep)
	}
}

// TestDashboard_TextFaintNonTextRolesMeetWCAG covers the FIVE uses of --text-faint
// that are not text at all — a bar fill, a legend swatch border, the dashed connector
// and two dumbbell dots. #534's title says "seven text roles"; that is right by
// coincidence, because there are twelve uses and five of them are non-text.
//
// They are judged against SC 1.4.11 (3.0:1 for UI components and graphical objects)
// and against the colours they actually sit on — which is NOT "--grid for all five".
// .cmp-legend .lg-a sits in .cmp-db-head inside #cmp-dumbbells, i.e. on --surface; the
// other four are on the --grid tracks. Both are checked, so the enumeration error was
// harmless — but it was the same mistake the issue's own remedy made, one level up.
func TestDashboard_TextFaintNonTextRolesMeetWCAG(t *testing.T) {
	style := cssCommentRe.ReplaceAllString(servedStyle(t), "")
	// 🔴 The five roles are ASSERTED to exist, not merely named in an error string.
	// They were prose: deleting .ybar-fill.below, or repointing .cmp-legend .lg-a at a
	// different token, left this test passing while still claiming to cover all five.
	// A test whose scope lives only in its failure message cannot notice its scope
	// shrinking.
	for _, want := range []string{
		".ybar-fill.below", ".cmp-legend .lg-a", ".cmp-db-connect.insignificant",
		".cmp-db-dot.a", ".cmp-db-dot.b.below",
	} {
		if !strings.Contains(style, want+" {") && !strings.Contains(style, want+" ,") &&
			!strings.Contains(style, want+"\n") {
			t.Errorf("the non-text role %q is no longer a rule in the stylesheet, but this test "+
				"still claims to cover it", want)
		}
	}

	for _, tc := range []struct{ name, open, closeAt string }{
		{"dark", ":root {", ":root[data-theme=\"light\"]"},
		{"light", ":root[data-theme=\"light\"] {", "@media (prefers-color-scheme"},
	} {
		tokens := themeTokens(t, style, tc.open, tc.closeAt)
		// --bg is in the list because .cmp-db-dot.a is a 2px ring between the --grid track
		// OUTSIDE it and its own --bg fill INSIDE it, so its inner edge is judged against
		// --bg. It was covered only transitively, by the text test happening to check the
		// same token on --bg at a stricter bar — coverage that would evaporate the moment
		// --text-faint stopped being used for text.
		for _, bg := range []string{"grid", "surface", "bg"} {
			got := contrastRatio(tokens["text-faint"], tokens[bg])
			if got < 3.0 {
				t.Errorf("%s theme: --text-faint (%s) on --%s (%s) = %.2f:1, want >= 3.0:1. "+
					"Five non-text roles use this token (.ybar-fill.below, .cmp-legend .lg-a, "+
					".cmp-db-connect.insignificant, .cmp-db-dot.a, .cmp-db-dot.b.below) and are "+
					"graphical objects under WCAG 2.1 SC 1.4.11",
					tc.name, tokens["text-faint"], bg, tokens[bg], got)
			}
		}
	}
}

// TestDashboard_SurfaceTwoHasOneConsumer guards the ASSUMPTION the contrast test above
// rests on, rather than leaving it as a comment.
//
// --surface-2 is darker than --surface, so any text placed on it needs a stronger
// token than --text-faint can provide without collapsing into --text-muted (measured:
// dark would need #808891, separation 1.15:1). The contrast test therefore does not
// check it — which is only safe while --surface-2 carries exactly one element whose
// text is --text.
//
// This is the guard that makes that safe. Add a second --surface-2 consumer and it
// fails, forcing the question to be answered rather than silently assumed. An
// assumption a guard depends on is part of the guard.
func TestDashboard_SurfaceTwoHasOneConsumer(t *testing.T) {
	style := cssCommentRe.ReplaceAllString(servedStyle(t), "")

	var consumers []string
	for _, rule := range cssRuleRe.FindAllStringSubmatch(style, -1) {
		sel, body := strings.TrimSpace(rule[1]), rule[2]
		if !strings.Contains(body, "var(--surface-2)") {
			continue
		}
		if strings.Contains(body, ":") && regexp.MustCompile(`--surface-2:\s*#`).MatchString(body) {
			continue // the token's own declaration, not a use
		}
		consumers = append(consumers, sel)
		// The consumer's own text must be a token that clears AA on --surface-2.
		if m := cssColorRe.FindStringSubmatch(body); m != nil && m[1] != "text" {
			t.Errorf("%s sets background --surface-2 AND color --%s. The contrast test "+
				"deliberately does not check --surface-2 (requiring --text-faint to pass there "+
				"forces a value 1.15:1 from --text-muted, i.e. deletes the tier), so a non---text "+
				"colour here is unmeasured by construction", sel, m[1])
		}
	}
	if len(consumers) != 1 {
		t.Errorf("--surface-2 now has %d consumers (%v), want exactly 1. The contrast test omits "+
			"--surface-2 on the measured grounds that its single consumer uses --text; a second "+
			"one makes that omission a blind spot rather than a scoped decision", len(consumers), consumers)
	}
}
