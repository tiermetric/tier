package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// get drives the handler for one path and returns the response plus its body.
// It uses the real *Handler (no fakes) so the tests exercise the embedded
// assets and headers exactly as production would serve them.
func get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = res.Body.Close()
	return res, string(body)
}

// scriptSrcDirective returns the "script-src ..." directive from a CSP header,
// isolated so a test can assert on it without matching 'unsafe-inline' that
// legitimately lives in the style-src directive.
func scriptSrcDirective(t *testing.T, csp string) string {
	t.Helper()
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, "script-src") {
			return d
		}
	}
	t.Fatalf("no script-src directive in CSP: %q", csp)
	return ""
}

func TestDashboard_RootServesHTMLWithExternalScript(t *testing.T) {
	res, body := get(t, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html...", ct)
	}
	// The token-bearing page carries nosniff too (defence-in-depth against MIME
	// confusion), matching the script asset.
	if nosniff := res.Header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	if !strings.Contains(body, `<script src="/dashboard.js" defer>`) {
		t.Error("HTML does not reference the external /dashboard.js script")
	}
	// The app script must now live only in the asset. A bare inline <script>
	// (opening tag not immediately followed by src=) would reintroduce the
	// 'unsafe-inline' requirement.
	if strings.Contains(body, "<script>") {
		t.Error("HTML still contains an inline <script> block")
	}
	// The token-handling code must have moved entirely to the JS asset.
	if strings.Contains(body, "sessionStorage") {
		t.Error("token-handling code (sessionStorage) still present in the HTML")
	}
}

func TestDashboard_CSPHasNoUnsafeInlineScript(t *testing.T) {
	res, _ := get(t, "/")
	csp := res.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header")
	}
	scriptSrc := scriptSrcDirective(t, csp)
	// Fails on main, where script-src is 'unsafe-inline'.
	if !strings.Contains(scriptSrc, "'self'") {
		t.Errorf("script-src = %q, want it to contain 'self'", scriptSrc)
	}
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src = %q, must not contain 'unsafe-inline'", scriptSrc)
	}
	// Scope pin: style-src still needs 'unsafe-inline' for the embedded <style>;
	// this task hardens script-src only. If a later change tightens style-src,
	// that is a different task and should update this assertion deliberately.
	if !strings.Contains(csp, "style-src 'unsafe-inline'") {
		t.Errorf("CSP = %q, want style-src to keep 'unsafe-inline'", csp)
	}
}

func TestDashboard_ScriptAssetServed(t *testing.T) {
	res, body := get(t, "/dashboard.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript...", ct)
	}
	if nosniff := res.Header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	// The script must run in strict mode: the directive prologue must be the
	// first statement (a leading comment is allowed and does not disable it).
	if !strings.Contains(body, `"use strict";`) {
		t.Error(`script missing "use strict"; directive`)
	}
	// Marker functions prove the real app body was carried over, not a stub.
	for _, marker := range []string{"authHeaders", "loadScores"} {
		if !strings.Contains(body, marker) {
			t.Errorf("script asset missing marker %q", marker)
		}
	}
	// Defence-in-depth: a direct navigation to the asset gets the same locked
	// CSP as the page.
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("script asset CSP = %q, want script-src 'self'", csp)
	}
}

func TestDashboard_UnknownPathStill404(t *testing.T) {
	res, _ := get(t, "/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

// TestDashboard_HTMLReferencesOnlyServedAssets guards the exact failure mode of
// this refactor: an asset referenced by the HTML (src=/href=) that the handler
// does not actually route would 404 at runtime. Every same-origin (absolute
// path) reference must resolve to a 200 from this handler.
func TestDashboard_HTMLReferencesOnlyServedAssets(t *testing.T) {
	_, body := get(t, "/")
	// Matches src="/..." and href="/..." with an absolute (same-origin) path.
	ref := regexp.MustCompile(`(?:src|href)="(/[^"]*)"`)
	matches := ref.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("no absolute-path asset references found in HTML — regex or page changed")
	}
	seen := map[string]bool{}
	for _, m := range matches {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		res, _ := get(t, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("HTML references %q but handler returned %d (not routed)", path, res.StatusCode)
		}
	}
}

// --- Engineering Yield Dashboard redesign (#274) ---------------------------
//
// These tests can only exercise the STATIC assets the Go handler serves (there
// is no JS runtime here), so they assert on structure and on the presence of
// the load-bearing rendering functions and CSS hooks. They guard the redesign's
// contract with the API shape and, most importantly, its security posture.

// TestDashboard_HTMLHasKPITiles asserts the org-level KPI instrument tiles are
// present in the served HTML: Org TIER (+ CI line), AI spend, spend leverage,
// and the capture-fidelity meter. renderKPIs targets these ids by name, so a
// rename here without a JS change would silently blank the tiles.
func TestDashboard_HTMLHasKPITiles(t *testing.T) {
	_, body := get(t, "/")
	for _, id := range []string{
		`id="kpi-tier"`, `id="kpi-tier-ci"`, `id="kpi-spend"`,
		`id="kpi-leverage"`, `id="kpi-fidelity"`, `id="kpi-fidelity-meter"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("KPI tile element %s missing from dashboard HTML", id)
		}
	}
}

// TestDashboard_HTMLHasCostCompositionPanel asserts the #234 cost-composition
// panel container and its fill targets are present in the served HTML.
// renderCostComposition targets these ids by name, so a rename here without a JS
// change would silently blank the panel.
func TestDashboard_HTMLHasCostCompositionPanel(t *testing.T) {
	_, body := get(t, "/")
	for _, id := range []string{
		`id="cost-comp"`, `id="cc-total"`, `id="cc-levers"`, `id="cc-models"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("cost-composition element %s missing from dashboard HTML", id)
		}
	}
}

// TestDashboard_ScriptRendersCostComposition pins that the JS carries the #234
// render path (called from renderScores off data.cost_composition), so the served
// asset is the real renderer, not a stub predating the panel.
func TestDashboard_ScriptRendersCostComposition(t *testing.T) {
	_, body := get(t, "/dashboard.js")
	for _, marker := range []string{
		"renderCostComposition", "data.cost_composition", "premium_model_share", "cache_read_share",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing cost-composition marker %q", marker)
		}
	}
}

// TestDashboard_InstrumentAesthetic pins the two visual invariants that make the
// page read as an instrument: tabular-numeral monospace for data, and a --yield
// custom property (green) that the KPI/bar styles consume. If either regresses
// the numbers stop aligning like a gauge or "green == yield" breaks.
func TestDashboard_InstrumentAesthetic(t *testing.T) {
	_, body := get(t, "/")
	for _, needle := range []string{
		"font-variant-numeric: tabular-nums",
		"--yield:",
		`:root[data-theme="light"]`,
		"prefers-color-scheme: light",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("dashboard HTML missing instrument/theme marker %q", needle)
		}
	}
}

// TestDashboard_RootDoesNotHardcodeTheme guards the first-paint contract
// (#274 review): the <html> element must NOT hardcode data-theme. A hardcoded
// data-theme would make the ":root:not([data-theme])" prefers-color-scheme
// block dead (first paint always dark) and would let the load-time stamp pin a
// theme in localStorage for a first-time visitor. The OS preference must drive
// first paint until the user explicitly toggles.
func TestDashboard_RootDoesNotHardcodeTheme(t *testing.T) {
	_, body := get(t, "/")
	htmlTag := regexp.MustCompile(`(?is)<html\b[^>]*>`).FindString(body)
	if htmlTag == "" {
		t.Fatal("no <html> tag found in dashboard HTML")
	}
	if strings.Contains(htmlTag, "data-theme") {
		t.Errorf("<html> tag must not hardcode data-theme (breaks OS-preference first paint, #274): %q", htmlTag)
	}
}

// TestDashboard_ScriptRendersYieldBars proves the leaderboard is rendered as
// ranked horizontal yield bars with CI whiskers and an explicit ranking-floor
// divider (#133/#274) -- not a plain table -- by requiring the functions and
// class hooks that build them.
func TestDashboard_ScriptRendersYieldBars(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"buildYieldBar", "buildFloorLine", "renderKPIs", "renderSegments",
		"ybar-fill", "ybar-whisker", "ranking floor",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing yield-bar rendering marker %q", marker)
		}
	}
}

// TestDashboard_DefaultWindowIs30d pins the #497 fix: first paint defaults the
// selected "From" to 30 days ago, not 90. Claude Code retains only ~30d of JSONL
// (the cost side) while backfill reconstructs outcomes across the full window, so
// a 90d default divides ~90d of outcomes by ~30d of cost and inflates the headline
// TIER ~2x on the very first thing a user sees. The control arm is the ABSENCE of
// the old 90d assignment; the 90d PRESET chip legitimately calls
// applyPreset(daysAgoISO(90)) — a distinct string — so a deliberate widen still
// works and is asserted present, and this never false-matches it.
func TestDashboard_DefaultWindowIs30d(t *testing.T) {
	_, body := get(t, jsPath)
	const want = "$('since-input').value = daysAgoISO(30)"
	if !strings.Contains(body, want) {
		t.Errorf("dashboard.js first-paint default must be 30d (#497): missing %q", want)
	}
	// Control arm: the inflated 90d first-paint default must be gone.
	const banned = "$('since-input').value = daysAgoISO(90)"
	if strings.Contains(body, banned) {
		t.Errorf("dashboard.js still sets the inflated 90d first-paint default (#497): found %q", banned)
	}
	// ...but the 90d preset chip must remain, so a user can still widen deliberately.
	if !strings.Contains(body, "applyPreset(daysAgoISO(90))") {
		t.Error("the 90d preset chip must remain so users can still widen the window (#497)")
	}
	// Also pin the setPreviousPeriod empty-From fallback (a distinct assignment
	// form) so a PARTIAL revert -- fixing first paint but leaving the compare-seed
	// fallback at 90d -- cannot slip through.
	if !strings.Contains(body, "fromS = daysAgoISO(30)") {
		t.Error("dashboard.js setPreviousPeriod empty-From fallback must also default to 30d (#497)")
	}
}

// TestDashboard_TeamModeSuppressesNames is the k-anonymity guard (#185): in
// team-aggregation mode a row must render the team name as plain text and MUST
// NOT build a per-developer drill-down link (which would name an individual).
// The renderer keys off Array.isArray(data.teams); the team branch of
// buildYieldBar sets label.textContent = d.team and never appends a dev-link.
func TestDashboard_TeamModeSuppressesNames(t *testing.T) {
	_, body := get(t, jsPath)
	if !strings.Contains(body, "label.textContent = d.team") {
		t.Error("team-mode branch must render the team name via textContent (k-anonymity, #185)")
	}
	if !strings.Contains(body, "var teamMode = Array.isArray(data.teams)") {
		t.Error("renderer must select team mode from the presence of data.teams (#185)")
	}
	// The developer drill-down link must be gated behind the non-team branch;
	// its href builder should reference d.developer, never d.team.
	if !strings.Contains(body, "encodeURIComponent(d.developer)") {
		t.Error("developer drill-down link builder missing (dev-mode only)")
	}
}

// TestDashboard_ScriptNeverUsesInnerHTML is the XSS posture assertion for #274:
// every user-supplied value (developer, team, issue_id, work_type, tokens) must
// reach the DOM via textContent. A single innerHTML assignment would reopen the
// token-exfiltration hole the CSP hardening (#145) closed, so we forbid the
// string outright in the served script.
func TestDashboard_ScriptNeverUsesInnerHTML(t *testing.T) {
	_, body := get(t, jsPath)
	if strings.Contains(body, "innerHTML") {
		t.Error("dashboard.js must never use innerHTML — user-supplied values must be set via textContent (#145)")
	}
	// The token must remain sessionStorage-only and travel in the Authorization
	// header; a ?token= query fallback would leak it to logs/history/Referer.
	if !strings.Contains(body, "sessionStorage") {
		t.Error("dashboard.js must keep the token in sessionStorage (#59)")
	}
	if strings.Contains(body, "?token=") || strings.Contains(body, "&token=") {
		t.Error("dashboard.js must never put the token in a URL query parameter")
	}
	if !strings.Contains(body, "'Authorization': 'Bearer '") {
		t.Error("dashboard.js must send the token in the Authorization header")
	}
}

// TestDashboard_LeverageLabelIsMeteredCost pins the Spend Leverage tile subtext
// to the accurate wording (#453). The leverage numerator is AI spend metered at
// API list rates — a cost-equivalence, not work-value (with cache reads dominating,
// the figure is what the work would have cost at list, not the value it produced).
// Yield (the TIER score) is the value/outcome metric; leverage is a cost metric.
// Labelling the numerator "output value" conflated the two, so the served script
// must say "metered cost / paid spend" and must never reintroduce "output value".
func TestDashboard_LeverageLabelIsMeteredCost(t *testing.T) {
	_, body := get(t, jsPath)
	if !strings.Contains(body, "metered cost / paid spend") {
		t.Error("dashboard.js Spend Leverage subtext must read 'metered cost / paid spend' (#453)")
	}
	if strings.Contains(body, "output value") {
		t.Error("dashboard.js must not label the metered-cost leverage numerator as 'output value' (#453) — it is cost-equivalence, not work-value")
	}
}

// --- Honest-coverage surfacing (#354) --------------------------------------
//
// These assert the dashboard SURFACES the #351 honest-coverage fields
// (attributed_cost_share, unjoined_developers) rather than keying only off the
// zero-token signal. As with the other dashboard tests there is no JS runtime
// here, so they pin the served HTML structure and the load-bearing JS render
// paths + guards that make the fields render when present, degrade when absent,
// and stay XSS-safe.

// TestDashboard_HTMLHasHonestCoverageElements asserts the two new banner
// containers and the fidelity-clarity caption exist in the served HTML. The JS
// renderers target these ids by name, so a rename without a JS change would
// silently blank the banners.
func TestDashboard_HTMLHasHonestCoverageElements(t *testing.T) {
	_, body := get(t, "/")
	for _, id := range []string{
		`id="attr-coverage"`, `id="attr-coverage-title"`, `id="attr-coverage-sub"`,
		`id="unjoined-strip"`, `id="unjoined-title"`, `id="unjoined-sub"`, `id="unjoined-list"`,
		`id="kpi-fidelity-sub"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("honest-coverage element %s missing from dashboard HTML", id)
		}
	}
	// The fidelity caption must state, in the static HTML, that the meter is NOT
	// attribution -- the #351/#354 point that the two coverages are different.
	if !strings.Contains(body, "not attribution") {
		t.Error("Capture-Fidelity tile must carry a static 'not attribution' clarifier (#354)")
	}
	// The low-coverage warning needs a danger palette distinct from the amber
	// trust colour. Since #520 the band carries severity on a left rule plus a
	// fill on attention rows only, so the live consumers are var(--danger) and
	// var(--danger-bg) — NOT --danger-border, which the four card shells used and
	// nothing now references. Assert what is actually consumed: these needles
	// previously carried a trailing colon and so only ever matched the :root
	// DEFINITIONS, which made the assertion weaker than its own comment claimed.
	for _, needle := range []string{
		"border-left-color: var(--danger)",
		"background: var(--danger-bg)",
		"#attr-coverage.warn",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("dashboard HTML missing danger-warning style hook %q", needle)
		}
	}
}

// TestDashboard_ScriptRendersAttributionCoverage pins that the JS carries the
// attribution-coverage render path: it is wired into renderScores off
// data.data_quality, reads attributed_cost_share, and has an explicit low-
// coverage warning threshold that flips the banner to the danger 'warn' state.
func TestDashboard_ScriptRendersAttributionCoverage(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"renderAttributionCoverage", "attributed_cost_share", "ATTR_WARN_THRESHOLD",
		"attr-coverage-title", "attr-coverage-sub", "'warn'",
		// The renderer must actually be INVOKED from renderScores off the
		// data_quality block -- a renderer that exists but is never called would
		// otherwise pass every marker above yet render nothing (QA #354 Y1).
		"renderAttributionCoverage(data.data_quality)",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing attribution-coverage marker %q", marker)
		}
	}
	// A genuine 0.0 must render (loudly), not be treated as absent: the guard
	// must test for a finite number, not truthiness (which would drop 0.0).
	if !strings.Contains(body, "typeof dq.attributed_cost_share === 'number'") {
		t.Error("attribution-coverage guard must accept a genuine 0.0 (finite-number test, not truthiness)")
	}
	// Pin the comparison DIRECTION, not just the threshold identifier: warn when
	// coverage is BELOW the threshold. A flipped comparison (warn on high
	// coverage) is a real honesty bug this guards against (QA #354).
	if !strings.Contains(body, "share < ATTR_WARN_THRESHOLD") {
		t.Error("low-coverage warning must fire when share < ATTR_WARN_THRESHOLD (direction guard)")
	}
	// Absence guard: a window with no spend omits the field, and the banner must
	// hide rather than render a spurious 0% (QA #354 Y2).
	if !strings.Contains(body, "share === null") {
		t.Error("attribution-coverage must hide (share === null) when the field is absent")
	}
}

// TestDashboard_ScriptRendersUnjoinedDevelopers pins the identity-mismatch
// callout render path: it is invoked from renderScores off
// data_quality.unjoined_developers, consumes the name lists (cost_only /
// outcome_only, populated only in developer mode) AND the always-present counts
// (cost_only_count / outcome_only_count), and carries the alias-mapping hint.
// The actual name suppression is a SERVER-side k-anon decision (#185); the
// client renders whatever names it is given, so these markers pin the wiring and
// field consumption, not a client-side suppression behavior.
func TestDashboard_ScriptRendersUnjoinedDevelopers(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"renderUnjoinedDevelopers", "unjoined_developers",
		"cost_only", "outcome_only", "cost_only_count", "outcome_only_count",
		"map their identity",
		// Must be INVOKED from renderScores, not merely defined (QA #354 Y1).
		"renderUnjoinedDevelopers(data.data_quality)",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing unjoined-developer marker %q", marker)
		}
	}
	// Absence guards (QA #354 Y2): the callout must hide both when the block is
	// absent (!uj) and when it is present but empty (total <= 0), so a clean
	// window never shows an empty red banner.
	for _, guard := range []string{"if (!uj)", "total <= 0"} {
		if !strings.Contains(body, guard) {
			t.Errorf("unjoined callout missing absence guard %q (would render empty on clean data)", guard)
		}
	}
}

// TestDashboard_UnjoinedNamesUseTextContent is the XSS posture assertion for the
// new callout: developer names (user-controlled) must reach the DOM via el()'s
// textContent path, never assembled into parsed markup. The global innerHTML ban
// (TestDashboard_ScriptNeverUsesInnerHTML) covers the file; this pins that the
// name-append helper specifically uses the textContent element builder.
func TestDashboard_UnjoinedNamesUseTextContent(t *testing.T) {
	_, body := get(t, jsPath)
	if !strings.Contains(body, "appendUnjoinedNames") {
		t.Fatal("dashboard.js missing appendUnjoinedNames helper")
	}
	// The helper must build list items through el(...) (which assigns
	// textContent), not via any string-concatenated markup sink. Loosely pinned
	// to el('li' so a benign refactor (loop-var or class rename) does not break
	// it (QA #354 Y4); the global innerHTML ban (TestDashboard_ScriptNeverUsesInnerHTML)
	// remains the hard XSS backstop.
	if !strings.Contains(body, "appendChild(el('li'") {
		t.Error("unjoined names must be rendered via el()'s textContent, not parsed markup (#145)")
	}
}

// --- Unattributed-spend breakdown (#360) -----------------------------------
//
// These complete the honesty-UI test surface: the dashboard must SURFACE the
// #360 unattributed_buckets + exploratory_cost_share fields, degrade gracefully
// when they are absent (fully-attributed window), and render the pass-through
// bucket label via textContent so a label containing markup is inert (the #360
// k-anon XSS flag). As with the other dashboard tests there is no JS runtime
// here, so they pin the served HTML structure and the load-bearing JS render
// paths + guards.

// TestDashboard_HTMLHasUnattributedBreakdownElements asserts the breakdown panel
// container and its fill targets exist in the served HTML. renderUnattributedBreakdown
// targets these ids by name, so a rename without a JS change would silently blank
// the panel.
func TestDashboard_HTMLHasUnattributedBreakdownElements(t *testing.T) {
	_, body := get(t, "/")
	for _, id := range []string{
		`id="unattr-breakdown"`, `id="unattr-exploratory"`,
		`id="unattr-exploratory-sub"`, `id="unattr-buckets"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("unattributed-breakdown element %s missing from dashboard HTML", id)
		}
	}
}

// TestDashboard_ScriptRendersUnattributedBreakdown pins that the JS carries the
// #360 render path: it is wired into renderScores off data.data_quality, reads
// both new fields, and is actually INVOKED (a renderer that exists but is never
// called would pass every marker yet render nothing).
func TestDashboard_ScriptRendersUnattributedBreakdown(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"renderUnattributedBreakdown", "buildBucketBar",
		"unattributed_buckets", "exploratory_cost_share",
		"unattr-exploratory", "unattr-buckets",
		// Must be invoked from renderScores off the data_quality block (QA Y1).
		"renderUnattributedBreakdown(data.data_quality)",
		// Divide-by-zero guard: an all-zero-share bucket set must not produce a
		// NaN bar width. Pin the guard so removing it is caught (QA Y2).
		"scale <= 0",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing unattributed-breakdown marker %q", marker)
		}
	}
	// A genuine 0.0 exploratory share must render (loudly), not be treated as
	// absent: the guard must test for a finite number, not truthiness (which
	// would drop 0.0). Mirrors the attributed_cost_share guard (#354). BOTH halves
	// are pinned: typeof===number keeps 0.0, and isFinite rejects NaN/Inf (which
	// are also typeof number and would otherwise render "NaN%") (QA Y1).
	if !strings.Contains(body, "typeof dq.exploratory_cost_share === 'number'") {
		t.Error("exploratory-share guard must accept a genuine 0.0 (finite-number test, not truthiness)")
	}
	if !strings.Contains(body, "isFinite(dq.exploratory_cost_share)") {
		t.Error("exploratory-share guard must reject NaN/Inf (isFinite), which are typeof number")
	}
	// Absence guard: a fully-attributed window omits BOTH fields, and the panel
	// must hide rather than render an empty list or a spurious 0% (QA Y2).
	if !strings.Contains(body, "buckets.length === 0 && expShare === null") {
		t.Error("unattributed breakdown must hide when both fields are absent (graceful degradation)")
	}
}

// TestDashboard_BucketLabelsUseTextContent is the XSS posture assertion for the
// new panel and the direct guard the #360 k-anon reviewer asked for: the bucket
// LABEL is a pass-through server string and must reach the DOM via el()'s
// textContent, never assembled into parsed markup. There is no JS runtime here,
// so this pins the source construct (el('div', 'ub-label', b.bucket)); the
// file-wide innerHTML ban (TestDashboard_ScriptNeverUsesInnerHTML) is the hard
// backstop that guarantees a markup label renders inert at runtime.
func TestDashboard_BucketLabelsUseTextContent(t *testing.T) {
	_, body := get(t, jsPath)
	if !strings.Contains(body, "el('div', 'ub-label', b.bucket)") {
		t.Error("bucket label must be rendered via el()'s textContent, not parsed markup (#360 k-anon XSS flag)")
	}
	// The label must never be routed through a markup sink. The file-wide ban
	// covers innerHTML/outerHTML; pin insertAdjacentHTML explicitly here since it
	// is the other common pass-through-string HTML sink and is not otherwise
	// forbidden in this file.
	if strings.Contains(body, "insertAdjacentHTML") {
		t.Error("dashboard.js must never use insertAdjacentHTML -- pass-through strings must be set via textContent (#360)")
	}
}

// --- cost_per_point + provenance + low-coverage headline (gap-3, #239/#354) ----
//
// These complete gap-3 of the PM re-validation: the /scores response carries
// cost_per_point (+ its self-relative CI) per row and a top-level rubric.version,
// and the headline TIER must de-emphasize when attribution coverage is thin. As
// with the other dashboard tests there is no JS runtime here, so they pin the
// served HTML structure and the load-bearing JS render paths + guards.

// TestDashboard_HTMLHasCostPerPointAndProvenanceElements asserts the new static
// containers exist: the provenance stamp, the provisional headline qualifier, and
// the CSS hooks (the .provisional dim, the stacked value cell's .ybar-cpp). The JS
// targets these by name/class, so a rename here without a JS change would silently
// blank them.
func TestDashboard_HTMLHasCostPerPointAndProvenanceElements(t *testing.T) {
	_, body := get(t, "/")
	for _, id := range []string{
		`id="provenance"`, `id="kpi-tier-provisional"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("gap-3 element %s missing from dashboard HTML", id)
		}
	}
	// Style hooks the JS relies on: the dim class on the headline value, the
	// stacked cost-per-point sub-line, and the tier line inside the value cell.
	for _, needle := range []string{
		".kpi-value.provisional", ".ybar-cpp", ".ybar-tier",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("dashboard HTML missing gap-3 style hook %q", needle)
		}
	}
	// #239=C door stays shut: no absolute good/ok/poor band language in the UI.
	for _, banned := range []string{"good/ok/poor", ">good<", ">poor<"} {
		if strings.Contains(body, banned) {
			t.Errorf("dashboard HTML must not introduce an absolute quality band (%q, #239=C)", banned)
		}
	}
}

// TestDashboard_ScriptRendersCostPerPoint pins the $/point render path: the value
// cell stacks cost_per_point under TIER via buildCostPerPoint, gates the "--"
// placeholder on weighted_points (a zero-POINT row has no interpretable unit), and
// carries the self-relative CI fields on hover.
func TestDashboard_ScriptRendersCostPerPoint(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"buildCostPerPoint", "cost_per_point", "ybar-cpp", "/pt",
		"cost_per_point_ci_low", "cost_per_point_ci_high",
		// Actually invoked from the row builder, not merely defined.
		"buildCostPerPoint(d, teamMode, isRanked)",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing cost-per-point marker %q", marker)
		}
	}
	// The zero-point "--" placeholder must gate on weighted_points, NOT on
	// cost_per_point: a genuine free (zero-cost) but non-zero-point row is an
	// honest "$0.00/pt", only a zero-POINT row is the undefined-unit case.
	if !strings.Contains(body, "num(d.weighted_points)") {
		t.Error("cost-per-point placeholder must gate on weighted_points (zero-point => no unit)")
	}
	if !strings.Contains(body, "points <= 0") {
		t.Error("cost-per-point must render the '--' placeholder when weighted_points <= 0")
	}
}

// TestDashboard_ScriptRendersProvenance pins the rubric/price provenance stamp: it
// is invoked from renderScores, reads both version stamps, and guards each as a
// finite number so an absent field degrades to omission (never "vundefined") and a
// no-field response hides the stamp.
func TestDashboard_ScriptRendersProvenance(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"renderProvenance", "data.rubric", "data.price_table",
		"rubric v", "price table v",
		// Must be invoked from renderScores, not merely defined.
		"renderProvenance(data)",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing provenance marker %q", marker)
		}
	}
	// Guard direction: render a version only when it is a finite number, so a
	// hypothetical absent field is omitted rather than printed as "vundefined".
	// Pin BOTH halves (typeof===number AND isFinite), matching the #360 precedent:
	// isFinite rejects NaN/Inf, which are also typeof number and would print "vNaN".
	if !strings.Contains(body, "typeof rubric.version === 'number'") {
		t.Error("provenance must render rubric.version only when it is a finite number")
	}
	if !strings.Contains(body, "isFinite(rubric.version)") {
		t.Error("provenance rubric.version guard must reject NaN/Inf (isFinite)")
	}
	if !strings.Contains(body, "typeof price.version === 'number'") {
		t.Error("provenance must render price_table.version only when it is a finite number")
	}
	if !strings.Contains(body, "isFinite(price.version)") {
		t.Error("provenance price_table.version guard must reject NaN/Inf (isFinite)")
	}
	// Stale-across-reload guard (QA gap-3 Y1): renderScores/renderCompare can early-
	// return ("No data for this period") BEFORE the render fns run, and
	// renderProvenance only self-hides when BOTH fields are absent -- so EVERY load
	// path MUST hide the stamp first or a stale rubric/price stamp lingers from a
	// prior window. The reset lives in resetViews(), shared by loadScores AND the
	// #278 compare loader. Pin all three halves so no mutation slips through:
	//   (a) #provenance is a MEMBER of resetViews' hide[] array (not merely mentioned
	//       somewhere -- a move to display:'' or a non-hidden list would bypass a bare
	//       token check), AND resetViews actually sets those members to display:none;
	//   (b) loadScores routes through resetViews;
	//   (c) loadCompare routes through resetViews (same early-return risk on the
	//       compare path, #278).
	resetViews := body[strings.Index(body, "function resetViews()"):]
	resetViews = resetViews[:strings.Index(resetViews, "function load()")]
	hideArr := resetViews[strings.Index(resetViews, "var hide = ["):]
	hideArr = hideArr[:strings.Index(hideArr, "]")]
	if !strings.Contains(hideArr, "'provenance'") {
		t.Error("resetViews hide[] must include #provenance (stale-stamp guard through the No-data early return)")
	}
	if !strings.Contains(resetViews, ".style.display = 'none'") {
		t.Error("resetViews must set its hide[] members to display:none (not merely reference them)")
	}
	loadScores := body[strings.Index(body, "function loadScores()"):]
	loadScores = loadScores[:strings.Index(loadScores, "function showDetail")]
	if !strings.Contains(loadScores, "resetViews()") {
		t.Error("loadScores must call resetViews() so the stale-stamp guard runs on every load")
	}
	loadCompare := body[strings.Index(body, "function loadCompare()"):]
	loadCompare = loadCompare[:strings.Index(loadCompare, "function renderCompare")]
	if !strings.Contains(loadCompare, "resetViews()") {
		t.Error("loadCompare must call resetViews() so the stale-stamp guard runs on the compare path too (#278)")
	}
}

// TestDashboard_ScriptDimsHeadlineOnLowCoverage pins the #354 re-validation item
// (a): the headline TIER is DE-EMPHASIZED (dim + a "provisional" qualifier), never
// hidden, when attributed_cost_share is below ATTR_WARN_THRESHOLD -- the SAME
// threshold that flips the #354 banner, so the two agree. Healthy/absent coverage
// must clear the treatment (the element is reused across loads).
func TestDashboard_ScriptDimsHeadlineOnLowCoverage(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"kpi-tier-provisional", "attributed_cost_share", "ATTR_WARN_THRESHOLD",
		"classList.add('provisional')", "classList.remove('provisional')",
		// Em-dash, not ASCII "--" (#520). The dashboard's static HTML already used
		// &mdash; while every JS-built string used "--", so the same typographic
		// role rendered two ways on one page — which on a public demo reads as
		// unrendered markdown. This marker moves with that copy fix deliberately:
		// it pins a user-visible string, so it SHOULD fail when the string changes.
		"provisional — ",
		// The dim and the qualifier note are two halves of ONE treatment: the
		// healthy/else branch must clear BOTH or a stale "provisional — N%" caption
		// lingers under a now-normal headline (QA gap-3 Y3). classList.remove above
		// pins the dim reset; these pin the note reset.
		"provisionalEl.textContent = ''",
		"provisionalEl.style.display = 'none'",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing low-coverage headline marker %q", marker)
		}
	}
	// Direction guard: dim BELOW the threshold (a flipped comparison would dim a
	// healthy headline and trust a thin one -- the exact honesty bug this closes).
	if !strings.Contains(body, "attrShare < ATTR_WARN_THRESHOLD") {
		t.Error("headline must dim when attrShare < ATTR_WARN_THRESHOLD (direction guard)")
	}
	// Finite-number guard: a genuine 0.0 coverage must dim (loudest case), and a
	// no-spend window (field absent) must NOT dim -- test for a number, not truthy.
	if !strings.Contains(body, "typeof dq.attributed_cost_share === 'number'") {
		t.Error("headline dim guard must accept a genuine 0.0 (finite-number test, not truthiness)")
	}
	// Both halves of the guard, mirroring the #360 exploratory-share precedent:
	// typeof===number keeps 0.0, and isFinite rejects NaN/Inf (also typeof number).
	// Without isFinite, +/-Infinity would compare live against the threshold and dim
	// with a "-Infinity% coverage"-class artifact (QA gap-3 YELLOW #1).
	if !strings.Contains(body, "isFinite(dq.attributed_cost_share)") {
		t.Error("headline dim guard must reject NaN/Inf (isFinite), which are typeof number")
	}
	if !strings.Contains(body, "attrShare !== null") {
		t.Error("headline must render normally (not dim) when attributed_cost_share is absent")
	}
	// Never-hidden invariant (#354 re-validation item a is emphatic: "DO NOT hide
	// it; qualify it"). The treatment must DE-EMPHASIZE, so the dim class must ride
	// opacity, never collapse the number via display:none / visibility:hidden /
	// opacity:0. Pin the CSS rule so a future change to a hiding treatment is caught
	// (QA gap-3 YELLOW #2).
	_, html := get(t, "/")
	if !regexp.MustCompile(`\.kpi-value\.provisional\s*\{[^}]*opacity:\s*0*\.[1-9]`).MatchString(html) {
		t.Error(".kpi-value.provisional must de-emphasize via a non-zero opacity, not hide the number (#354: qualify, do not hide)")
	}
	if regexp.MustCompile(`\.kpi-value\.provisional\s*\{[^}]*(display:\s*none|visibility:\s*hidden|opacity:\s*0\s*[;}])`).MatchString(html) {
		t.Error(".kpi-value.provisional must not hide the headline (display:none/visibility:hidden/opacity:0) -- the number is qualified, not hidden (#354)")
	}
}

// TestDashboard_CompareView pins the #278 before/after compare surface: the
// From/To range picker + presets, the compare toggle and baseline window, and
// the dumbbell renderer's honesty treatments. The acceptance criterion is that a
// non-significant or below-floor move is rendered as such (never as a confident
// change), and that the two-window semantics (Δ = B − A) match the server.
func TestDashboard_CompareView(t *testing.T) {
	_, html := get(t, "/")
	for _, id := range []string{
		`id="until-input"`, `id="preset-30"`, `id="preset-90"`, `id="preset-quarter"`,
		`id="compare-toggle"`, `id="compare-controls"`,
		`id="baseline-since-input"`, `id="baseline-until-input"`, `id="preset-prev"`,
		`id="compare-view"`, `id="cmp-windows"`, `id="cmp-total"`, `id="cmp-rows"`, `id="cmp-note"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing compare-view element %q", id)
		}
	}

	_, js := get(t, "/dashboard.js")
	for _, marker := range []string{
		"function loadCompare()", "function renderCompare(",
		"/api/v1/scores/compare",
		// Two-window contract: window B = selected/after, window A = baseline/before.
		"since_a=", "since_b=", "until_a=", "until_b=",
		// Significance is SERVER-authoritative (#277), never recomputed client-side.
		"row.significant",
		// Not-significant / below-floor / one-window treatments (the acceptance).
		"not significant", "below ranking floor", "only in selected period", "only in baseline period",
		// Team aggregates NEVER assert significance (no bootstrap CI in anon modes).
		"aggregate — not tested",
		// sample_n == 0 is treated as "no data" for that side, never a misleading 0.
		"num(side.sample_n) > 0",
		// Δ direction convention is documented in the on-page note.
		"selected period − baseline period",
		// A range preset dispatches through load() (compare-aware), not loadScores.
		"function load()",
	} {
		if !strings.Contains(js, marker) {
			t.Errorf("dashboard.js missing compare-view marker %q", marker)
		}
	}

	// Inline-style invariant (#145/#274): the dumbbell's only inline styles are dot
	// and connector POSITIONS, and they must route through pct() from finite numbers
	// -- never a user-supplied string. Pin that the position setters use pct().
	for _, styleCall := range []string{
		"conn.style.left = pct(", "conn.style.width = pct(",
		"a.style.left = pct(", "b.style.left = pct(",
	} {
		if !strings.Contains(js, styleCall) {
			t.Errorf("dashboard.js compare dot/connector position must go through pct(): missing %q", styleCall)
		}
	}

	// Two-window A/B mapping (highest-value semantic): a string-grep for the four
	// params alone would still pass if A and B were swapped -- which inverts every
	// Δ (a regression would render as an improvement). Pin that the SELECTED inputs
	// feed window B and the BASELINE inputs feed window A, so Δ = selected − baseline.
	for _, mapping := range []string{
		"sinceB = $('since-input')", "sinceA = $('baseline-since-input')",
		"since_a=' + encodeURIComponent(sinceA)", "since_b=' + encodeURIComponent(sinceB)",
	} {
		if !strings.Contains(js, mapping) {
			t.Errorf("dashboard.js compare A/B mapping must not be swapped (Δ = selected − baseline): missing %q", mapping)
		}
	}

	// Honesty pairing (the #278 acceptance): the below-floor / not-significant branch
	// must render the MUTED treatment (flat delta + dashed connector), and ONLY the
	// significant branch may set a directional (green/red) dot class. A grep for the
	// tag strings alone would pass even if that branch were switched to a confident
	// colour, so slice buildDevDumbbell and pin the pairing.
	devDb := js[strings.Index(js, "function buildDevDumbbell("):]
	devDb = devDb[:strings.Index(devDb, "function buildTeamDumbbell(")]
	// The significant branch is the sole source of a directional bClass, and since
	// #613 (2026-08-05) that direction is itself GATED on bothRanked. The dot's colour
	// is a third publication channel: `.cmp-db-dot.b.up`/`.down` paint yield-green and
	// danger-red, so an ungated `bClass = dir` publishes SIGN(Δ) for a Δ the row printed
	// as '—'. Pinning the gated form — the bare one is now the defect.
	if strings.Count(devDb, "bClass = bothRanked ? dir : 'below'") != 1 {
		t.Error("buildDevDumbbell: the significant branch must set its directional dot colour " +
			"GATED on bothRanked (`bClass = bothRanked ? dir : 'below'`). Ungated, the dot's " +
			"colour asserts the direction of a Δ this row withheld — the same cross-channel " +
			"leak #613 closed for the digits and the position")
	}
	if strings.Contains(devDb, "bClass = dir;") {
		t.Error("buildDevDumbbell still assigns an UNGATED directional dot colour (`bClass = dir;`)")
	}
	// The not-significant branch pairs the muted delta with the dashed connector.
	if !strings.Contains(devDb, "connectClass = 'insignificant'") || !strings.Contains(devDb, "deltaCls = 'flat'") {
		t.Error("buildDevDumbbell not-significant branch must render muted (deltaCls='flat' + connectClass='insignificant'), never a confident move")
	}

	// "Both aggregation modes" coverage: the mode DISPATCH must be reachable, or a
	// team-only string living in dead code would pass while team mode is broken.
	for _, dispatch := range []string{
		"data.mode === 'developer'",
		"buildDevDumbbell(rows[i], scale) : buildTeamDumbbell(rows[i], scale)",
		"sideData(row.present_a", "sideData(row.present_b", // sideData applied to BOTH sides
	} {
		if !strings.Contains(js, dispatch) {
			t.Errorf("dashboard.js compare mode dispatch/both-sides guard missing %q", dispatch)
		}
	}

	// The sample_n==0 no-data guard is inside sideData; pin its definition too so the
	// dispatch markers above resolve to a real present-and-sampled test.
	for _, guard := range []string{
		"function sideData(present, side)", "num(side.sample_n) > 0",
	} {
		if !strings.Contains(js, guard) {
			t.Errorf("dashboard.js sample_n==0 no-data guard missing %q", guard)
		}
	}
}

// TestDashboard_ScriptRendersNoScore pins #500: a window with spend but zero
// accepted outcomes must render "NO SCORE", not a "0.0" that would read as the
// worst row on the board. The gate is on weighted_points (mirroring the
// cost-per-point "--" placeholder), so the two sub-readings agree, and the
// distinction is voiced for screen readers too.
func TestDashboard_ScriptRendersNoScore(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"buildTierReading",
		"NO SCORE",
		// The gate must key on points, not on the raw tier value (which is a
		// misleading literal 0.0 for a no-score row).
		"num(d.weighted_points) <= 0",
		// The absence must be voiced, not sighted-only.
		"no accepted outcomes in this window",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing NO SCORE marker %q", marker)
		}
	}
	// Guard against regressing to the old behaviour: the TIER reading must not be
	// produced by an unconditional tierVal.toFixed(1) at the value cell. It now
	// flows through buildTierReading, which gates first.
	if strings.Contains(body, "el('div', 'ybar-tier num', tierVal.toFixed(1))") {
		t.Error("TIER value is rendered unconditionally as a number; a no-score row must gate to NO SCORE first (#500)")
	}
}

// TestDashboard_NoScoreStyled pins the CSS class the NO SCORE label needs so it
// reads as a faint label rather than a muted number.
func TestDashboard_NoScoreStyled(t *testing.T) {
	_, body := get(t, "/")
	if !strings.Contains(body, ".ybar-noscore") {
		t.Error("index.html missing .ybar-noscore style — NO SCORE would inherit number styling")
	}
}

// TestDashboard_KPINoScore pins the org-headline half of #500: renderKPIs must
// gate the ORG TIER tile on weighted_points too, so a window with spend but no
// accepted outcome headlines "NO SCORE" instead of "0.0" — and must still render
// the spend/leverage/fidelity tiles and re-show the row (an early return here
// previously blanked the whole KPI row).
func TestDashboard_KPINoScore(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"num(total.weighted_points) <= 0",
		"'NO SCORE'",
		"kpi-noscore",
		"no accepted outcomes across",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing KPI no-score marker %q", marker)
		}
	}
	// The row-show must still run for a no-score org (regression guard against the
	// early-return that blanked the row): kpi-row is shown unconditionally at the
	// end of renderKPIs, and the no-score branch must fall through to it.
	if strings.Count(body, "$('kpi-row').style.display = ''") != 1 {
		t.Error("kpi-row must be shown exactly once at the end of renderKPIs, reachable on the no-score path (#500)")
	}
}

// TestDashboard_ScriptRendersFree pins #499: a row/org with real points but ZERO
// recorded cost has unbounded yield, not zero, so it must read "FREE" — never the
// literal "0.0" the engine leaves (it divides only when cost>0). The NO SCORE
// gate must come FIRST, so a row with neither points nor cost is NO SCORE, not
// FREE.
func TestDashboard_ScriptRendersFree(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"'FREE'",
		"num(d.total_cost_usd) <= 0",
		"num(total.total_cost_usd) <= 0",
		"yield unbounded",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing FREE marker %q", marker)
		}
	}
	// Ordering guard: inside buildTierReading, the no-score (weighted_points) gate
	// MUST precede the free (cost) gate, or a zero-points/zero-cost row mislabels
	// as FREE instead of NO SCORE. This is the ONE place the order is load-bearing
	// (renderKPIs guards orgFree with !noScore, so it is order-independent there).
	//
	// Scope the search to buildTierReading's body: the same two gate strings also
	// appear earlier in buildYieldBar's aria-label branches, so an unscoped
	// strings.Index would measure THAT ordering and pass even if the gates were
	// swapped inside buildTierReading — the exact false-green this guard exists to
	// prevent.
	fnAt := strings.Index(body, "function buildTierReading")
	if fnAt < 0 {
		t.Fatal("buildTierReading not found — the ordering guard cannot be scoped")
	}
	fnBody := body[fnAt:]
	noScoreAt := strings.Index(fnBody, "num(d.weighted_points) <= 0")
	freeAt := strings.Index(fnBody, "num(d.total_cost_usd) <= 0")
	if noScoreAt < 0 || freeAt < 0 || noScoreAt > freeAt {
		t.Error("in buildTierReading, the weighted_points (NO SCORE) gate must come before the cost (FREE) gate (#499)")
	}
}

// TestDashboard_HTMLHasCostHorizonElements pins the cost-horizon banner's static
// scaffolding (#512): the three ids the renderer writes into, and the danger hook
// its loud state switches to.
func TestDashboard_HTMLHasCostHorizonElements(t *testing.T) {
	_, body := get(t, "/")
	for _, needle := range []string{
		`id="cost-horizon"`, `id="cost-horizon-title"`, `id="cost-horizon-sub"`,
		"#cost-horizon.warn",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("cost-horizon element/style hook %q missing from dashboard HTML", needle)
		}
	}
}

// TestDashboard_ScriptRendersCostHorizon pins the cost-horizon render path
// (#512), mirroring the attribution-coverage block above.
//
// Two of these markers guard defects that ALREADY happened once on this branch
// and were caught by hand rather than by a test:
//
//   - `display = 'flex'`. The banner originally ended with `display = ''`, which
//     hands control back to the id rule's `display: none`. It populated every
//     string correctly and rendered to nobody. Asserting on text or state could
//     never have caught it; only the literal assignment can.
//   - `'cost-horizon'` in resetViews. Without it the banner survives the no-data
//     early return, a fetch error and the switch into compare mode — and because
//     this is the one banner with a CALM state, a stale one does not merely
//     linger, it asserts that a window now on screen was verified when it was the
//     previous window that was.
func TestDashboard_ScriptRendersCostHorizon(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"renderCostHorizon", "cost_coverage_start", "window_predates_cost_capture",
		"cost_coverage_safe_since", "source_coverage_start",
		"cost-horizon-title", "cost-horizon-sub",
		// The renderer must actually be INVOKED from renderScores.
		"renderCostHorizon(data.data_quality",
		// Must be hidden by resetViews with every other result panel.
		"'cost-horizon'",
		// Must set an explicit display value, never '' (see doc comment above).
		"banner.style.display = 'flex'",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing cost-horizon marker %q", marker)
		}
	}
	// An explicit `false` is a real answer and MUST render: the server emits it
	// rather than omitting the field precisely so "checked and covered" stays
	// distinguishable from "no signal". A truthiness test would collapse the two,
	// the same trap the attribution guard avoids for a genuine 0.0.
	if !strings.Contains(body, "typeof dq.window_predates_cost_capture === 'boolean'") {
		t.Error("cost-horizon guard must accept an explicit false (typeof boolean test, not truthiness)")
	}
}

// NOTE: do NOT run `gofmt -w` on this file. The m-series gofmt in this toolchain
// curls the ASCII '' sequences in these comments into U+201D, which matters here
// more than anywhere else: this file's guards assert on literal source spellings,
// so a comment that no longer names the construct it documents is actively
// misleading. It is already reported by `gofmt -l` for that reason, exactly like
// internal/store/store.go -- that is expected; leave it.

// --- #516: the reveal guard --------------------------------------------------
//
// The defect: an element whose CSS rule declares `display: none` as its resting
// state, revealed with `el.style.display = ''`. That does not restore the element
// default — it drops the inline override and lets the stylesheet's `none` win.
// The element renders its content correctly and is never seen. Eight elements
// shipped that way, including BOTH halves of the compare view.
//
// SCOPE, stated so a green run is not over-read: this proves only that one family
// of spellings of that bug is absent from the asset source. It does NOT prove the
// dashboard renders. Only computed style or a bounding box in a real browser can
// do that, and no Go test here can.
//
// The id set is derived from the served assets rather than hand-maintained, so a
// newly added hidden element is covered the moment it exists — the whole failure
// was that nobody noticed.

// revealRe matches the spellings that reintroduce the bug: `= ''`, `= ""`,
// arbitrary spacing, and the TERNARY form `= cond ? '' : 'none'` — not
// hypothetical, it is the idiom four of this file's remaining reveal sites use.
// There is no JS linter in this repo (see the Makefile), so nothing normalizes
// spacing or quote style and the guard must accept all of it.
var revealRe = regexp.MustCompile(`(?:\$\('([-\w]+)'\)|(\w+))\.style\.display\s*=\s*(?:''|""|[^;\n]*\?\s*(?:''|""))`)

// explicitRe matches a reveal that DOES name a box, so the guard can check the
// box is the right one rather than merely non-empty.
var explicitRe = regexp.MustCompile(`(?:\$\('([-\w]+)'\)|(\w+))\.style\.display\s*=\s*'(\w+)'`)

// jsCommentRe strips line comments before matching. Without it the guard reads
// prose: this file's own REVEAL INVARIANT comment contains the literal
// `el.style.display = ''`, and it escapes only because `el` has no preceding $()
// binding. Move that sentence beside a real binding and the test fails on a
// comment.
var jsCommentRe = regexp.MustCompile(`(?m)//.*$`)

// hiddenIDs maps every id whose CSS rule declares display:none to whether that
// same rule makes it a flex container — which is how the guard knows the correct
// reveal value without a hand-maintained table.
func hiddenIDs(html string) map[string]bool {
	// ANCHORED to a rule boundary. Unanchored, this can begin matching mid-chain:
	// `#cost-horizon.calm .dq-sub { display:none }` fails to match from
	// `#cost-horizon`, then succeeds from `.dq-sub`, harvesting a bare class that
	// hides nothing on its own. Measured against the real asset that pulled
	// .dq-icon and .dq-sub into the hiding set and three ids with it
	// (attr-coverage-sub, cost-horizon-sub, unjoined-sub) — an over-broad set,
	// which this file's own negative arm declares is a different defect and not a
	// safer one. It is the containment-by-nearest-token bug from the ledger,
	// reintroduced inside the guard added to close a false green.
	ruleRe := regexp.MustCompile(`(?m)(?:^|[};])\s*([#.][-\w]+(?:\s*,\s*[#.][-\w]+)*)\s*\{([^}]*)\}`)
	// Case- and space-insensitive: `display : none`, `DISPLAY: none` and
	// `display: NONE` are all valid CSS and would otherwise drop an id out of the
	// set — a false negative, the direction that hides the bug.
	noneRe := regexp.MustCompile(`(?i)(?:^|[^-\w])display\s*:\s*none`)
	flexRe := regexp.MustCompile(`(?i)align-items|flex-direction|justify-content`)
	out := map[string]bool{}
	// CLASS rules that hide, collected alongside the id rules and resolved onto
	// ids in the second pass below.
	//
	// This is not hypothetical tidiness. #520 moved the four data-quality banners
	// into a shared `.dq-row` class and deleted their per-id `display:none`, and
	// an id-only scan silently dropped ALL FOUR — including #trust-strip and
	// #attr-coverage, two of the eight elements #516 was filed about. Every
	// dashboard test still passed, because the guard had simply stopped looking.
	// A guard that narrows its own input set is the purest form of this project's
	// dominant bug: a green that means "never ran".
	hiddenClasses := map[string]bool{}
	for _, m := range ruleRe.FindAllStringSubmatch(html, -1) {
		if !noneRe.MatchString(m[2]) {
			continue
		}
		isFlex := flexRe.MatchString(m[2])
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.TrimSpace(sel)
			switch {
			case strings.HasPrefix(sel, "#"):
				id := strings.TrimPrefix(sel, "#")
				out[id] = out[id] || isFlex
			case strings.HasPrefix(sel, "."):
				cls := strings.TrimPrefix(sel, ".")
				hiddenClasses[cls] = hiddenClasses[cls] || isFlex
			}
		}
	}
	if len(hiddenClasses) == 0 {
		return out
	}
	// Resolve hiding classes onto the ids that carry them. An element is hidden
	// by a class exactly as effectively as by its id, and it is revealed through
	// the same JS, so it belongs in the same contract.
	//
	// KNOWN BLIND SPOTS, written down because they fail SILENTLY (a missed tag
	// means the element never enters the set and the guard quietly stops covering
	// it — this project's dominant bug class): single-quoted attributes, and a
	// literal ">" inside a quoted attribute value. Neither occurs in this document
	// today; if you add one, this guard narrows without saying so.
	tagRe := regexp.MustCompile(`(?s)<[a-zA-Z][^>]*>`)
	attrRe := regexp.MustCompile(`(?s)\b(id|class)\s*=\s*"([^"]*)"`)
	tags := tagRe.FindAllString(html, -1)
	// Vacuity check, the third in this file (see the len(hidden)==0 and sites==0
	// fatals): a hiding class with zero tags scanned means the tag scan has gone
	// blind, not that no element carries the class.
	if len(tags) == 0 {
		panic("hiddenIDs: harvested hiding classes but parsed ZERO HTML tags — the tag scan is blind")
	}
	for _, tag := range tags {
		var id, class string
		for _, a := range attrRe.FindAllStringSubmatch(tag, -1) {
			if a[1] == "id" {
				id = a[2]
			} else {
				class = a[2]
			}
		}
		if id == "" || class == "" {
			continue
		}
		for _, cls := range strings.Fields(class) {
			if isFlex, hides := hiddenClasses[cls]; hides {
				out[id] = out[id] || isFlex
			}
		}
	}
	return out
}

// resolver maps an alias to the element id bound by the NEAREST PRECEDING
// declaration. It indexes EVERY var/let/const, not just $() bindings: indexing
// only bindings lets resolution reach back past an intervening
// `var panel = el('div', ...)` and blame a local node's reveal on a real element.
// `panel` is genuinely bound both ways in this file, so that is a live hazard.
func resolver(js string) func(alias string, at int) string {
	declRe := regexp.MustCompile(`(?:var|let|const)\s+(\w+)\s*=\s*([^;\n]*)`)
	bindRe := regexp.MustCompile(`^\$\(['"]([-\w]+)['"]\)`)
	type decl struct {
		pos  int
		name string
		id   string // "" when the declaration is not an element binding
	}
	var decls []decl
	for _, loc := range declRe.FindAllStringSubmatchIndex(js, -1) {
		id := ""
		if b := bindRe.FindStringSubmatch(strings.TrimSpace(js[loc[4]:loc[5]])); b != nil {
			id = b[1]
		}
		decls = append(decls, decl{pos: loc[0], name: js[loc[2]:loc[3]], id: id})
	}
	return func(alias string, at int) string {
		id := ""
		for _, d := range decls { // source order; nearest preceding wins
			if d.name == alias && d.pos < at {
				id = d.id
			}
		}
		return id
	}
}

// scanReveals reports hidden-by-default ids revealed with an empty display value,
// the explicit box each hidden id is revealed with, and how many reveal sites were
// parsed at all. That last count is the vacuity check: zero means the guard has
// stopped seeing the file.
func scanReveals(js string, hidden map[string]bool) (bad []string, boxes map[string]string, sites int) {
	js = jsCommentRe.ReplaceAllString(js, "")
	resolve := resolver(js)
	boxes = map[string]string{}

	idAt := func(loc []int, js string) string {
		if loc[2] >= 0 { // direct $('id') form
			return js[loc[2]:loc[3]]
		}
		return resolve(js[loc[4]:loc[5]], loc[0])
	}

	for _, loc := range revealRe.FindAllStringSubmatchIndex(js, -1) {
		sites++
		id := idAt(loc, js)
		if id == "" {
			continue
		}
		// MEMBERSHIP, not the map value. The value is isFlex, so testing it
		// directly silently skipped every non-flex hidden element -- i.e. all the
		// panels, including both halves of the compare view. The control-arm test
		// caught this; nothing else would have.
		if _, isHidden := hidden[id]; isHidden {
			bad = append(bad, id)
		}
	}
	for _, loc := range explicitRe.FindAllStringSubmatchIndex(js, -1) {
		box := js[loc[6]:loc[7]]
		if box == "none" {
			continue // a hide, not a reveal
		}
		if id := idAt(loc, js); id != "" {
			boxes[id] = box
		}
	}
	return bad, boxes, sites
}

func TestDashboard_NoEmptyStringReveal(t *testing.T) {
	_, html := get(t, "/")
	_, js := get(t, jsPath)

	hidden := hiddenIDs(html)
	if len(hidden) == 0 {
		t.Fatal("parsed no display:none id rules — the CSS shape changed and this guard is now vacuous")
	}

	bad, boxes, sites := scanReveals(js, hidden)
	// Vacuity control for the JS side, mirroring the CSS-side fatal above. If the
	// file switches to an unmatched spelling the loop body stops executing and the
	// test passes forever — a green meaning "never ran", the exact failure this
	// branch exists to eliminate.
	if sites == 0 {
		t.Fatal("matched no reveal sites at all — dashboard.js changed shape and this guard is now vacuous")
	}
	for _, id := range bad {
		t.Errorf("#%s is hidden by a CSS display:none rule but is revealed with an empty display value — "+
			"it will render its content and stay invisible (#516). Assign an explicit display value "+
			"matching its CSS box (flex for the banner family, block for panels).", id)
	}

	// Forbidding '' is not enough: `strip.style.display = 'block'` on a flex banner
	// passes that check and silently breaks the layout. Each hidden element must be
	// revealed with the box its OWN rule implies.
	for id, isFlex := range hidden {
		want := "block"
		if isFlex {
			want = "flex"
		}
		if got, ok := boxes[id]; ok && got != want {
			t.Errorf("#%s is revealed with display %q but its CSS rule implies %q", id, got, want)
		}
	}
}

// TestDashboard_HiddenIDsResolvesClassRules is the control arm for the class
// resolution added in #520, and it exists because that change was made in
// response to a false green I shipped into the working tree.
//
// #520 moved the four data-quality banners into a shared `.dq-row` and deleted
// their per-id `display:none`. An id-only scan dropped all four out of the hidden
// set — including two of the eight elements #516 was filed about — and the entire
// dashboard suite stayed green, because the guard had stopped looking rather than
// found nothing.
//
// Both arms are load-bearing. Without the negative arm a scan that added EVERY id
// would also pass the positive one, and an over-broad hidden set is its own defect:
// it demands an explicit display value from elements no rule hides.
func TestDashboard_HiddenIDsResolvesClassRules(t *testing.T) {
	// The fixture uses the REAL document's selector grammar, including the
	// descendant-with-compound-ancestor form. An earlier version had only
	// lone-class rules, so it never exercised the shape that actually appears in
	// index.html and the over-broad harvest went unnoticed.
	const fixture = `<style>
      .hidden-flex { display: none; align-items: flex-start; }
      .hidden-block { display: none; }
      #host.calm .desc { display: none; }
      .shown { color: red; }
    </style>
    <div id="by-class-flex" class="dq-row hidden-flex"></div>
    <div id="by-class-block" class="hidden-block"></div>
    <div id="class-first" class="hidden-block" data-x="1"></div>
    <div class="hidden-block"></div>
    <div id="not-hidden" class="shown"></div>
    <div id="desc-el" class="desc"></div>`

	got := hiddenIDs(fixture)

	for id, wantFlex := range map[string]bool{
		"by-class-flex":  true,
		"by-class-block": false,
		"class-first":    false,
	} {
		isFlex, ok := got[id]
		if !ok {
			t.Errorf("#%s is hidden by a CLASS rule but hiddenIDs missed it — every element "+
				"revealed through the same JS belongs in the same contract, and an id-only "+
				"scan is how four #516 elements silently left the guard's map (#520)", id)
			continue
		}
		if isFlex != wantFlex {
			t.Errorf("#%s isFlex = %v, want %v — the wrong box is demanded at the reveal site", id, isFlex, wantFlex)
		}
	}
	// NEGATIVE arm: a class with no display:none must not pull its element in.
	if _, ok := got["not-hidden"]; ok {
		t.Error("#not-hidden has no hiding rule but landed in the hidden set — an over-broad " +
			"scan demands an explicit display value from elements nothing hides, which is a " +
			"different defect, not a safer one")
	}
	// NEGATIVE arm, mid-chain: `.desc` hides only INSIDE #host.calm, so #desc-el is
	// not hidden at rest. An unanchored rule regex starts matching at `.desc` and
	// harvests it as a bare hiding class — measured doing exactly that against the
	// real asset before the anchor was added.
	if _, ok := got["desc-el"]; ok {
		t.Error("#desc-el was pulled in from `#host.calm .desc` — the rule scan matched " +
			"mid-chain and harvested a descendant class that hides nothing on its own")
	}
	// And an element with no id cannot be recorded at all.
	if len(got) != 3 {
		t.Errorf("hiddenIDs returned %d ids, want exactly 3 (%v) — a class-hidden element with "+
			"no id has nothing to key on and must be skipped silently", len(got), got)
	}
}

// TestDashboard_RevealGuardCatchesTheDefect is the control arm: it runs the guard
// against fixtures that CONTAIN the bug and fails if the guard reports clean.
// Without it, a regex that quietly stops matching turns the test above into a
// permanent green — and this project has had several incidents of exactly that
// shape.
func TestDashboard_RevealGuardCatchesTheDefect(t *testing.T) {
	hidden := map[string]bool{"trust-strip": true, "unattr-breakdown": false}

	mustCatch := []struct{ name, js string }{
		{"direct empty-string reveal", `$('trust-strip').style.display = '';`},
		{"aliased empty-string reveal", "var strip = $('trust-strip');\nstrip.style.display = '';"},
		{"double-quoted", "var p = $('unattr-breakdown');\np.style.display = \"\";"},
		{"no spaces", "var p = $('unattr-breakdown');\np.style.display='';"},
		{"ternary — the idiom four live sites use", "var strip = $('trust-strip');\nstrip.style.display = show ? '' : 'none';"},
	}
	for _, tt := range mustCatch {
		t.Run(tt.name, func(t *testing.T) {
			bad, _, sites := scanReveals(tt.js, hidden)
			if sites == 0 {
				t.Fatal("guard parsed no reveal sites in a fixture that contains one")
			}
			if len(bad) == 0 {
				t.Error("guard reported CLEAN on a fixture containing the #516 defect")
			}
		})
	}

	// It must also stay quiet on correct code, or it is noise nobody will keep.
	t.Run("explicit value is clean", func(t *testing.T) {
		bad, boxes, _ := scanReveals("var strip = $('trust-strip');\nstrip.style.display = 'flex';", hidden)
		if len(bad) != 0 {
			t.Errorf("guard fired on a correct explicit reveal: %v", bad)
		}
		if boxes["trust-strip"] != "flex" {
			t.Errorf("box for trust-strip = %q, want flex", boxes["trust-strip"])
		}
	})

	// A local DOM node sharing an alias name with an element binding must not be
	// attributed to that element — the false POSITIVE that the first draft of this
	// guard actually produced.
	t.Run("local node sharing an alias name is not blamed", func(t *testing.T) {
		js := "var panel = $('unattr-breakdown');\npanel.style.display = 'block';\n" +
			"function build() {\nvar panel = el('div', 'panel');\npanel.style.display = '';\n}"
		bad, _, _ := scanReveals(js, hidden)
		if len(bad) != 0 {
			t.Errorf("guard blamed a local node's reveal on a real element: %v", bad)
		}
	})

	// A comment quoting the defect must not fail the build.
	t.Run("the defect quoted in a comment is ignored", func(t *testing.T) {
		js := "var strip = $('trust-strip');\n// never write strip.style.display = '' here\nstrip.style.display = 'flex';"
		if bad, _, _ := scanReveals(js, hidden); len(bad) != 0 {
			t.Errorf("guard fired on a comment: %v", bad)
		}
	})
}

// TestDashboard_ScriptCapsIdentityLists pins the row caps added with #516. The
// caps truncate two DATA-QUALITY lists, so the risk they carry is understating a
// problem: the headline count must come from the true total, never the slice.
// TRUST_MAX_ROWS is also read by a function declared above it and survives only by
// `var` hoisting — if it ever resolved undefined, slice(0, undefined) returns the
// whole array and `length > undefined` is false, so the cap and its overflow line
// would both vanish with no error and no failing test.
func TestDashboard_ScriptCapsIdentityLists(t *testing.T) {
	_, body := get(t, jsPath)
	for _, marker := range []string{
		"var TRUST_MAX_ROWS = 12",
		"rows.slice(0, TRUST_MAX_ROWS)",
		"all.slice(0, TRUST_MAX_ROWS)",
		"rows.length > TRUST_MAX_ROWS",
		"all.length > TRUST_MAX_ROWS",
		// Titles must be computed from the TRUE totals, not the truncated lists.
		"rows.length + ' outcome'",
		// The overflow line must say what the sample IS — server-sorted by name,
		// so these are the alphabetically first, not the worst.
		"'Showing the first ' + TRUST_MAX_ROWS + ' of '",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing row-cap marker %q", marker)
		}
	}
	// The declaration must precede appendUnjoinedNames, so the cap does not depend
	// on hoisting to exist at call time.
	decl := strings.Index(body, "var TRUST_MAX_ROWS")
	use := strings.Index(body, "function appendUnjoinedNames")
	if decl < 0 || use < 0 || decl > use {
		t.Errorf("TRUST_MAX_ROWS must be declared before appendUnjoinedNames (decl=%d use=%d)", decl, use)
	}
}

// TestDashboard_EvidenceFoldedButCaveatsVisible pins the #519 layout contract.
//
// The constraint that matters is not "the page is shorter" — it is that folding
// EVIDENCE must never fold a WARNING. Each strip's headline (its count, its
// colour, its warn class) has to render with no interaction; only the row-by-row
// list may sit behind a disclosure. Get that wrong and the honesty UI is
// suppressed rather than tidied, which is the opposite of #516's point.
// servedMarkup returns the dashboard HTML with the <style> block and all HTML
// comments removed, so structural checks read MARKUP and not prose.
//
// This is load-bearing and has now bitten three times in three languages: the
// #519 CSS comment contains the literal "<details>", this file's own REVEAL
// INVARIANT comment contains a literal `style.display = ''`, and the first
// version of the tag-balance check counted the word "<details>" inside a comment
// as an unclosed element. A guard that reads its own documentation is a guard
// that fails on correct code.
func servedMarkup(t *testing.T) string {
	t.Helper()
	_, raw := get(t, "/")
	return regexp.MustCompile(`(?s)<style.*?</style>|<!--.*?-->`).ReplaceAllString(raw, "")
}

// detailsDepth reports how many <details> elements enclose the given prefix.
//
// Depth, not nearest-token. An earlier version compared LastIndex("<details")
// against LastIndex("</details>"), which equals containment only when no
// disclosure is nested and none is unbalanced — so folding an ENTIRE banner
// behind an outer <details>, with the headline placed after the inner evidence
// disclosure closed, read as "not inside" and passed. That is precisely the
// violation this test exists to catch.
func detailsDepth(prefix string) int {
	return strings.Count(prefix, "<details") - strings.Count(prefix, "</details>")
}

// TestDashboard_HTMLTagsBalanced is the structural floor under every layout test.
//
// The first attempt at the #519 panel move extracted the block by searching for
// the next </div> instead of balancing tags, dropped the closing tag, and nested
// the entire rest of the page inside #unattr-breakdown — which is display:none by
// default, so every panel below it vanished until the window happened to have
// unattributed spend. The above-the-fold measurement passed anyway, and so did
// every other test. Balance is the only check that sees that class of bug.
func TestDashboard_HTMLTagsBalanced(t *testing.T) {
	html := servedMarkup(t)
	for _, tag := range []string{"div", "details", "main"} {
		open := strings.Count(html, "<"+tag) - strings.Count(html, "</"+tag+">")
		// "<div" also prefixes nothing else in this document; </div> is exact.
		if open != 0 {
			t.Errorf("unbalanced <%s> in the dashboard HTML: %d unclosed", tag, open)
		}
	}
}

func TestDashboard_EvidenceFoldedButCaveatsVisible(t *testing.T) {
	html := servedMarkup(t)

	// The two evidence LISTS are inside <details>.
	for _, id := range []string{`id="trust-list"`, `id="unjoined-list"`} {
		idx := strings.Index(html, id)
		if idx < 0 {
			t.Fatalf("%s missing from the dashboard HTML", id)
		}
		if detailsDepth(html[:idx]) < 1 {
			t.Errorf("%s is not inside a <details> disclosure — the evidence list is not folded (#519)", id)
		}
	}

	// The HEADLINES are NOT. A title inside <details> would hide the warning
	// itself, not just its evidence.
	// Headlines AND the sub-prose. A count with its explanation folded away is not
	// "the warning is visible" — #unjoined-sub carries "their scores read 0 until
	// you map their identity", and #cost-horizon-sub carries the inflation
	// mechanism. Guarding only the titles let either be hidden.
	for _, id := range []string{
		`id="trust-title"`, `id="unjoined-title"`, `id="attr-coverage-title"`, `id="cost-horizon-title"`,
		`id="unjoined-sub"`, `id="attr-coverage-sub"`, `id="cost-horizon-sub"`,
	} {
		idx := strings.Index(html, id)
		if idx < 0 {
			t.Fatalf("%s missing from the dashboard HTML", id)
		}
		if detailsDepth(html[:idx]) >= 1 {
			t.Errorf("%s is INSIDE a <details> — a caveat must be visible without interaction (#519)", id)
		}
	}

	// Reading order: the data-quality banners qualify the number, so they stay
	// ABOVE the KPI row. The spend BREAKDOWN is content and moves below it.
	kpi := strings.Index(html, `id="kpi-row"`)
	if kpi < 0 {
		t.Fatal(`id="kpi-row" missing`)
	}
	for _, id := range []string{`id="trust-strip"`, `id="attr-coverage"`, `id="cost-horizon"`, `id="unjoined-strip"`} {
		if i := strings.Index(html, id); i < 0 || i > kpi {
			t.Errorf("%s must appear BEFORE the KPI row — it qualifies every number below it", id)
		}
	}
	if i := strings.Index(html, `id="unattr-breakdown"`); i < 0 || i < kpi {
		t.Error(`id="unattr-breakdown" must appear AFTER the KPI row (#519): it is content, not a caveat, ` +
			`and keeping it above pushed the yield number off the first screen`)
	}
}

// TestDashboard_DisclosuresStartClosedAndLabelled closes three mutants the
// containment check above sails past — each of which silently undoes #519.
//
// Measured by the frontend review: adding a single `open` attribute puts the KPI
// row back at 943px, straight below the fold this branch exists to clear. A
// `.dq-evidence { display: none }` rule is the #516 defect class outright
// (rendered != seen). An empty <summary> leaves the only interactive control in
// the banner unlabelled for anyone reaching it out of context.
func TestDashboard_DisclosuresStartClosedAndLabelled(t *testing.T) {
	html := servedMarkup(t)

	// Default-CLOSED. `open` is a boolean attribute, so any form of it counts.
	openRe := regexp.MustCompile(`(?s)<details\b[^>]*\bopen\b`)
	if loc := openRe.FindString(html); loc != "" {
		t.Errorf("a <details> ships with the `open` attribute (%q) — the evidence would render "+
			"expanded and push the KPI row back below the fold (#519)", loc)
	}

	// The disclosure must not be hidden by CSS: hiding it makes the evidence
	// unreachable rather than folded, which is #516 all over again.
	// Anchored to the BARE selector. A looser pattern matched
	// `.dq-evidence > summary::-webkit-details-marker { display: none }` -- a
	// legitimate rule that hides the UA triangle -- and failed on correct code.
	if regexp.MustCompile(`(?s)\.dq-evidence\s*\{[^}]*display\s*:\s*none`).MatchString(servedStyle(t)) {
		t.Error(".dq-evidence is hidden by a display:none rule — evidence must be foldable, not unreachable (#516/#519)")
	}

	// Every <summary> carries static, non-empty label text. The renderer replaces
	// it with a counted label, but the static text is what a no-JS or pre-render
	// reader sees, and an empty control is unusable from a rotor.
	sums := regexp.MustCompile(`(?s)<summary[^>]*>(.*?)</summary>`).FindAllStringSubmatch(html, -1)
	if len(sums) == 0 {
		t.Fatal("no <summary> elements found — the disclosure markup changed and this guard is vacuous")
	}
	for _, m := range sums {
		if strings.TrimSpace(m[1]) == "" {
			t.Error("a <summary> has no label text — the disclosure control is unlabelled")
		}
	}
}

// servedStyle returns just the dashboard's <style> block, for assertions that are
// genuinely about CSS (the inverse of servedMarkup).
func servedStyle(t *testing.T) string {
	t.Helper()
	_, raw := get(t, "/")
	m := regexp.MustCompile(`(?s)<style.*?</style>`).FindString(raw)
	if m == "" {
		t.Fatal("no <style> block in the dashboard HTML")
	}
	return m
}

// TestDashboard_ScriptRendersDataQualityBand pins the #520 band the way
// TestDashboard_ScriptRendersCostHorizon pins its banner. The band shipped with
// ZERO coverage — including of the exact resetViews omission its own source
// comment warns about, which is the hazard that test exists to close for every
// other panel.
func TestDashboard_ScriptRendersDataQualityBand(t *testing.T) {
	_, js := get(t, jsPath)
	_, html := get(t, "/")

	for _, marker := range []string{
		"function renderDataQualityBand",
		// Invoked from the render path at all.
		"renderDataQualityBand();",
		// In resetViews' hide list. A stale band does not merely linger — it
		// asserts how many checks ran on a window no longer on screen.
		"'dq-band'",
		// Explicit reveal, never the empty string (#516).
		"band.style.display = 'block'",
		// The denominator is the REGISTRY length, not the rendered-row count. A
		// numerator reported as a whole is what made the demo say "2 checks ran"
		// when four did.
		"var total = DQ_CHECKS.length",
	} {
		if !strings.Contains(js, marker) {
			t.Errorf("dashboard.js missing data-quality-band marker %q", marker)
		}
	}
	for _, id := range []string{`id="dq-band"`, `id="dq-frame"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing %s", id)
		}
	}
	// The band must never claim silence over a finding: #trust-strip renders ONLY
	// when outcomes were flagged, so it is a NOTE, never a clearance. A boolean
	// red/amber predicate got this wrong and produced "None need attention" above
	// a row itemising flagged outcomes.
	if !strings.Contains(js, "return 'note';") {
		t.Error("no 'note' state in DQ_CHECKS — a check that is amber AND a finding " +
			"(#trust-strip) must not be counted as a clearance")
	}
	// Substring matching on className inflates the very count the frame line
	// exists to make trustworthy ("warned", "no-warn", "warn-dismissed" all trip
	// indexOf). classList.contains is exact.
	if strings.Contains(js, "className.indexOf('warn')") {
		t.Error("DQ_CHECKS uses className.indexOf('warn') — use classList.contains('warn'), " +
			"a substring match errs in the inflating direction")
	}
}

// TestDashboard_DataQualityBandListsAgree is the drift guard.
//
// The band is defined in FOUR hand-maintained places that must agree: the row
// markup inside #dq-band, the CSS id rule that gives rows their box, the
// :not(.dq-first) divider list, and DQ_CHECKS. A fifth check added to the markup
// but missed in DQ_CHECKS silently under-counts the frame line — which is the
// "denominator lies" defect again, but permanent.
func TestDashboard_DataQualityBandListsAgree(t *testing.T) {
	html := servedMarkup(t)
	// RAW html for the CSS assertions: servedMarkup strips the <style> block, so
	// checking a selector against it would silently match nothing — a guard
	// asserting on an empty haystack.
	_, raw := get(t, "/")
	_, js := get(t, jsPath)

	band := strings.Index(html, `id="dq-band"`)
	if band < 0 {
		t.Fatal(`id="dq-band" missing`)
	}
	kpi := strings.Index(html, `id="kpi-row"`)
	if kpi < band {
		t.Fatal(`#kpi-row must follow #dq-band`)
	}
	// Row ids declared INSIDE the band (bounded by the KPI row). The band itself
	// is role="region" too, so exclude it rather than counting the container as
	// one of its own checks.
	rowRe := regexp.MustCompile(`id="([-\w]+)" role="region"`)
	var markup []string
	for _, m := range rowRe.FindAllStringSubmatch(html[band:kpi], -1) {
		if m[1] != "dq-band" {
			markup = append(markup, m[1])
		}
	}
	if len(markup) == 0 {
		t.Fatal("parsed zero rows out of #dq-band — the markup shape changed and this guard is vacuous")
	}

	// Ids registered in DQ_CHECKS.
	checkRe := regexp.MustCompile(`\{ id: '([-\w]+)'`)
	var registered []string
	for _, m := range checkRe.FindAllStringSubmatch(js, -1) {
		registered = append(registered, m[1])
	}
	if len(registered) == 0 {
		t.Fatal("parsed zero ids out of DQ_CHECKS — the registry shape changed and this guard is vacuous")
	}

	if !sameStringSet(markup, registered) {
		t.Errorf("the band's rows and DQ_CHECKS disagree.\n  markup:    %v\n  DQ_CHECKS: %v\n"+
			"A row missing from the registry is never counted, so the frame line under-reports "+
			"forever; a registry entry with no row throws on $().", markup, registered)
	}
	// Every row must also appear in the CSS id rule that gives it its box, and in
	// the divider list. Missing from the first is the class-wipe defect that made
	// border-left-width compute to 0px.
	for _, id := range markup {
		if !strings.Contains(raw, "#"+id+":not(.dq-first)") {
			t.Errorf("#%s is a band row but is missing from the :not(.dq-first) divider list", id)
		}
	}
}

// sameStringSet reports whether two slices hold the same set of strings.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// --- #502 org KPI tile: the ratio is WITHHELD below the evidence floor -------
//
// These guards exist because the naive translation of #502 into the tile is a
// MUTE, not a suppression: the per-row bars already print a below-floor number
// faintly with "insufficient sample to rank", and copying that treatment to the
// org headline leaves a muted 280,000,000.0 on the page. A muted number is a
// published number. Steve's ruling turns on exactly this distinction, so it gets
// a structural guard rather than a substring check — `strings.Contains(body,
// "NOT ENOUGH SPEND TO SCORE")` passes just as happily when the branch prints the
// badge AND the ratio.

// jsRegexAllowedAfter reports whether a '/' appearing after the given preceding
// significant byte starts a REGEX LITERAL rather than a division.
//
// 🔴 This distinction is not pedantry, it is a measured desync. dashboard.js line
// 1674 contains `if (/[",\r\n]/.test(s)) { … }` — a regex whose character class
// holds a double quote. Without regex handling, jsStrip and jsStripStrings read
// that `"` as opening a string literal that never closes on that line, and from
// there ALL THE WAY DOWN THE FILE their quote state is inverted: `//` comments
// stop being stripped, and string literals are blanked in antiphase. Measured on
// HEAD before this fix: every comment from line 1678 onward survived jsStrip, and
// jsStripStrings returned ok=false for renderCompareTotal — so the #605 guard was
// reporting "cannot read its input" and any softer guard scoped below line 1674
// would have been auditing comments.
//
// The heuristic is the standard one: a '/' is a regex only where an EXPRESSION may
// begin. After an identifier, a literal, a `)`, or a `]`, it is division. This
// errs toward "division", which merely leaves a regex unparsed as it was before —
// it never swallows real code.
func jsRegexAllowedAfter(prev byte) bool {
	switch prev {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	return false
}

// jsRegexKeywords are the keywords after which a '/' likewise begins an
// expression. Punctuation alone is not enough: `return /["]/.test(s)` ends in `n`,
// which reads as division, and the phantom string that opens inside the character
// class inverts the quote state for the rest of the file — bit for bit the desync
// this machinery was fixed to remove.
var jsRegexKeywords = []string{"return", "typeof", "case", "in", "of", "new", "delete", "void", "do", "else", "yield", "await"}

// jsRegexAllowedAfterToken extends jsRegexAllowedAfter with keyword context, given
// everything emitted so far.
func jsRegexAllowedAfterToken(prev byte, emitted string) bool {
	if jsRegexAllowedAfter(prev) {
		return true
	}
	if !isJSIdentByte(prev) {
		return false
	}
	// Read back the identifier that just ended.
	i := len(emitted)
	for i > 0 && isJSIdentByte(emitted[i-1]) {
		i--
	}
	word := emitted[i:]
	for _, kw := range jsRegexKeywords {
		if word == kw {
			return true
		}
	}
	return false
}

// jsSkipRegex returns the index just past the regex literal starting at src[at]
// (which must be '/'), or at itself when the literal is unterminated. Character
// classes are tracked because an unescaped '/' inside `[...]` does not close the
// literal.
func jsSkipRegex(src string, at int) int {
	inClass := false
	for i := at + 1; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '\n':
			return at // a regex literal cannot span a line: this was division after all
		case '/':
			if !inClass {
				return i + 1
			}
		}
	}
	return at
}

// jsStrip removes // line comments from JS source while respecting string
// literals, so the brace/paren walkers below are not thrown by a comment that
// mentions a paren (the #502 branch is heavily commented) and not fooled by a
// "//" inside a quoted string. Regex literals are skipped whole — see
// jsRegexAllowedAfter for why that is load-bearing rather than tidy.
func jsStrip(src string) string {
	var out strings.Builder
	var quote byte
	var prev byte // last significant byte emitted, tracked for the regex heuristic
	for i := 0; i < len(src); i++ {
		c := src[i]
		if quote == 0 && c == '/' && i+1 < len(src) && src[i+1] != '/' && src[i+1] != '*' &&
			jsRegexAllowedAfterToken(prev, out.String()) {
			if end := jsSkipRegex(src, i); end > i {
				out.WriteString(src[i:end])
				prev = src[end-1]
				i = end - 1
				continue
			}
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
		if quote != 0 {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		// 🔴 BLOCK comments too. Without this, a brace-balanced /* */ comment
		// containing a verbatim copy of a guarded header steals every
		// jsBlockAfter lookup for it: the guards then audit the COMMENT and the
		// real branch is never read. Measured — a decoy comment quoting the
		// below-floor gate silenced five named tests at once while the real branch
		// published the ratio. Removing comments before any block matching makes
		// that decoy invisible instead of authoritative.
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			for i += 2; i < len(src); i++ {
				if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
					i++
					break
				}
			}
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}

// jsBlockAfter returns the body of the block introduced by header (which must end
// in '{'), by brace matching. ok is false when the header is absent or the block
// is unterminated — never a silent empty string, which would make every "does not
// contain" assertion below pass vacuously.
func jsBlockAfter(src, header string) (body string, ok bool) {
	// Strip FIRST, always, rather than trusting each of a dozen call sites to
	// remember. jsStrip is idempotent, so pre-stripped input is unaffected — and a
	// caller that passes raw source can no longer be steered onto a decoy comment
	// that merely quotes the header it is looking for.
	src = jsStrip(src)
	at := strings.Index(src, header)
	if at < 0 {
		return "", false
	}
	open := at + len(header) - 1
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open+1 : i], true
			}
		}
	}
	return "", false
}

// jsMatchParen returns the index of the ')' that closes the '(' at open. It is
// string-aware: jsStrip removes comments but PRESERVES string literals, and a
// ')' inside a badge string is not a delimiter. ok is false when src[open] is
// not '(' or the call is unterminated — never a silent partial match.
func jsMatchParen(src string, open int) (int, bool) {
	if open < 0 || open >= len(src) || src[open] != '(' {
		return 0, false
	}
	depth := 0
	var quote byte
	var prev byte
	for i := open; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == '\\' {
				i++ // escape: the next byte is data, never a terminator
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		// Regex literals are skipped whole. jsStrip PRESERVES a regex's bytes, so a
		// quote inside a character class (`/[",\r\n]/`) reaches this walker intact and
		// would open a phantom string that swallows the rest of the call — the third
		// quote-tracking walker in this file, and the one the regex fix originally left
		// behind. No scanned body contains such a regex today; this keeps it that way by
		// construction rather than by luck.
		if c == '/' && i+1 < len(src) && src[i+1] != '/' && src[i+1] != '*' &&
			jsRegexAllowedAfterToken(prev, src[open:i]) {
			if end := jsSkipRegex(src, i); end > i {
				prev = '/'
				i = end - 1
				continue
			}
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// jsCallArgs returns the argument text of every call whose head matches prefix,
// where prefix is a literal call head such as `setText('kpi-tier',`. Each capture
// runs from the end of prefix to the call's OWN closing paren.
//
// ok is false — a HARD failure for the caller, never a silent short read — when
// prefix contains no '(' or a matched call is unterminated.
//
// 🔴 This function previously started paren matching at prefix's LAST BYTE, which
// for a prefix ending in ',' is the comma. depth started at 0, the call's own ')'
// drove it to -1, and the walker returned at whatever unrelated ')' later brought
// it back to 0. Captures then began and ended at arbitrary offsets. Measured over
// the real dashboard.js: the 'kpi-tier' capture OVER-RAN three statements past the
// call, and the 'kpi-tier-ci' capture was TRUNCATED at ~30 characters — so the one
// line through which the #502 ratio would be smuggled was inspected for its first
// thirty bytes. TestDashboard_JSCallArgsCapturesTheArgument pins both boundaries.
func jsCallArgs(src, prefix string) (args []string, ok bool) {
	lp := strings.Index(prefix, "(")
	if lp < 0 {
		return nil, false
	}
	for at := 0; ; {
		i := strings.Index(src[at:], prefix)
		if i < 0 {
			return args, true
		}
		start := at + i
		closeAt, matched := jsMatchParen(src, start+lp)
		if !matched {
			return nil, false
		}
		args = append(args, src[start+len(prefix):closeAt])
		at = closeAt + 1
	}
}

// jsStripStrings blanks every string literal (and any /* */ block comment) so a
// scan for identifiers or operators can never be fooled by prose inside a badge
// string. Quotes are preserved as an empty pair, so the result keeps the same
// shape.
//
// ok is false when a literal or block comment is unterminated. That is not
// pedantry: the inner loop runs to end-of-input, so an unbalanced quote silently
// DISCARDS everything after it, and every scan built on the result then reports
// clean on source it never read. Measured: `a = 'unterminated ; b = c / d;`
// collapses to an assignment of one empty literal, and the division ban then sees
// no division at all.
// Regex literals are skipped whole for the reason spelled out on
// jsRegexAllowedAfter: `/[",\r\n]/` holds an unpaired double quote, and reading it
// as a string literal inverts the quote state for the rest of the input.
func jsStripStrings(src string) (string, bool) {
	var out strings.Builder
	var prev byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '/' && i+1 < len(src) && src[i+1] != '*' && jsRegexAllowedAfterToken(prev, out.String()) {
			if end := jsSkipRegex(src, i); end > i {
				// Blanked to `//`, not copied: the pattern's own bytes are data, and a
				// character class holding `[` or a backtick would trip the syntax bans
				// that read this output.
				out.WriteString("//")
				prev = '/'
				i = end - 1
				continue
			}
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			closed := false
			for i += 2; i < len(src); i++ {
				if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
					i++
					closed = true
					break
				}
			}
			if !closed {
				return "", false
			}
			out.WriteByte(' ')
			continue
		}
		if c != '\'' && c != '"' && c != '`' {
			out.WriteByte(c)
			continue
		}
		quote := c
		out.WriteByte(quote)
		closed := false
		for i++; i < len(src); i++ {
			if src[i] == '\\' {
				i++
				continue
			}
			if src[i] == quote {
				closed = true
				break
			}
		}
		if !closed {
			return "", false
		}
		out.WriteByte(quote)
	}
	return out.String(), true
}

// jsIdentPathRe matches a dotted identifier path (`total.weighted_points`,
// `tierValueEl.classList.add`). Property accesses that follow a call — the
// `.toFixed` in `num(x).toFixed(1)` — match on their own, which is why toFixed is
// listed as a property below.
var jsIdentPathRe = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*`)

// unrankedRootAllow / unrankedPropAllow are the #502 ALLOWLIST, and the allowlist
// is the point.
//
// This guard used to be a DENYLIST of two spellings — "orgTIER" and "total.tier".
// A denylist of spellings cannot survive an alias or a re-derivation, and both
// were proven against the real file: `var muted = orgTIER` then `muted.toFixed(1)`
// (the ruling's own defect, one alias away), and
// `num(total.weighted_points) / (totalCost / 1000)` (the ratio recomputed from the
// two inputs the branch is allowed to show, banned identifier never appearing).
// Both parsed, both rendered the number, both reported CLEAN.
//
// The allowlist inverts the default. The branch may name ONLY the measured inputs
// criterion 4 requires on screen, the floor constants it compares them against,
// and the DOM mechanics that put a badge up. Every other identifier — and every
// field of `total` except weighted_points — is a violation by construction, so a
// future alias fails without anyone having to predict its name.
var unrankedRootAllow = map[string]bool{
	// Control flow and literals. const/let are listed even though the branch uses
	// neither: without them, modernising `var` here would fail with a message
	// accusing the author of publishing the ratio.
	"var": true, "const": true, "let": true,
	"if": true, "else": true, "return": true, "typeof": true,
	"true": true, "false": true, "null": true, "undefined": true,
	// The measured inputs the tile is required to keep on screen (criterion 4).
	"total": true, "totalCost": true,
	// The floor constants those inputs are compared against.
	"MIN_RANKED_COST_USD": true, "MIN_RANKED_OUTCOMES": true,
	// Locals derived from the above, and nothing else.
	"belowSpendFloor": true, "spendText": true,
	// DOM mechanics: the text sink and the two elements.
	"setText": true, "tierValueEl": true, "provisionalEl": true,
	// Formatters. Every argument they are handed is itself checked against this
	// allowlist, and TestDashboard_FormattersAreClosedOverTheirArguments pins that
	// their bodies read nothing but those arguments — without that, admitting a
	// name here would admit whatever its body chooses to reach for.
	"num": true, "usdText": true, "usdUnder": true, "spendTextFor": true,
}

var unrankedPropAllow = map[string]bool{
	// The ONLY field of `total` the branch may read. total.tier, total.sample_n,
	// total.cost_per_point and every future field are rejected by omission.
	"weighted_points": true,
	"toFixed":         true,
	"classList":       true, "add": true, "remove": true,
	"style": true, "display": true, "textContent": true,
}

// jsTextSinkRe finds direct writes to a node's text. Only ONE form is permitted
// in the branch (blanking the provisional qualifier); anything else is a second,
// unaudited way to put a number on screen.
//
// The terminator is `[;\n]`, not `;`: JavaScript's automatic semicolon insertion
// means the branch's LAST statement needs no semicolon, and a regex requiring one
// matched nothing there — a hole precisely where a smuggled write would sit.
var jsTextSinkRe = regexp.MustCompile(`\.(textContent|innerText|innerHTML|outerHTML)=[^;]*`)

// jsBannedSinkRe covers the sinks that are not assignments. It is DEFENCE IN DEPTH
// and redundant by construction: every name in it is a property unrankedPropAllow
// omits, so the identifier allowlist already refuses each one and no fixture can
// make this regex the sole defence. Kept so that widening the property allowlist for
// some unrelated reason cannot quietly open a DOM sink — but do NOT read a green
// here as evidence that it fired.
var jsBannedSinkRe = regexp.MustCompile(`\b(insertAdjacentHTML|insertAdjacentText|setAttribute|append|appendChild|replaceChildren|write)\s*\(`)

// unrankedIdentViolations applies the allowlist to one chunk of branch source.
func unrankedIdentViolations(where, src string) (bad []string) {
	code, ok := jsStripStrings(src)
	if !ok {
		return []string{where + " contains an unterminated string literal or block comment, " +
			"so everything after it went unscanned — a guard that cannot read its input " +
			"must not report clean"}
	}
	for _, loc := range jsIdentPathRe.FindAllStringIndex(code, -1) {
		path := code[loc[0]:loc[1]]
		seg := strings.Split(path, ".")
		first := 0
		// A match preceded by '.' is the continuation of a member expression whose
		// head was a call or an index — the `.toFixed` in `num(x).toFixed(1)`. The
		// regex cannot include that head, so every segment here is a PROPERTY, and
		// checking seg[0] against the ROOT allowlist would reject correct code.
		if loc[0] == 0 || code[loc[0]-1] != '.' {
			if !unrankedRootAllow[seg[0]] {
				bad = append(bad, where+" references "+path+", which is not on the #502 allowlist "+
					"(the below-floor branch may name only the measured inputs, the floor constants "+
					"and the badge mechanics)")
				continue
			}
			first = 1
		}
		for _, p := range seg[first:] {
			if !unrankedPropAllow[p] {
				bad = append(bad, where+" references "+path+": property "+p+
					" is not on the #502 allowlist")
				break
			}
		}
	}
	return bad
}

// unrankedTileViolations runs the #502 rule over a renderKPIs source: on the
// below-floor branch, neither the headline (kpi-tier) nor its cause line
// (kpi-tier-ci) may carry the TIER ratio — by name, by alias, or re-derived — and
// the branch must still apply the shared kpi-noscore treatment. It returns a
// violation list plus the number of text writes it actually parsed, so a caller
// can reject a clean-looking result that came from parsing nothing.
func unrankedTileViolations(renderKPIs string) (bad []string, writes int, ok bool) {
	body, ok := jsBlockAfter(jsStrip(renderKPIs), "} else if (orgUnranked) {")
	if !ok {
		return nil, 0, false
	}

	// (1) The whole branch, by allowlist. Kills the alias and anything that reads a
	// field of `total` other than weighted_points.
	bad = append(bad, unrankedIdentViolations("below-floor branch", body)...)

	// (2) SYNTAX THE ALLOWLIST CANNOT SEE THROUGH is banned outright, because the
	// allowlist's whole claim is that an unlisted identifier cannot appear:
	//
	//   - BACKTICKS. jsStripStrings blanks a template literal wholesale, so
	//     `` `TIER ${orgTIER.toFixed(1)}` `` scans as an empty string and reports
	//     clean — while rendering the ratio. Preserving ${…} is possible; banning
	//     backticks is simpler and costs nothing, since this file has none outside
	//     comments.
	//   - COMPUTED MEMBER ACCESS. `total['tier']` puts the field name inside a
	//     string literal, which is likewise blanked, so the property allowlist never
	//     sees `tier`. The branch has no legitimate use for a computed field.
	//
	// Both were measured reporting 0 violations while publishing the ratio.
	code, stripOK := jsStripStrings(body)
	if !stripOK {
		bad = append(bad, "below-floor branch has an unterminated string literal or block comment")
		code = ""
	}
	if strings.Contains(code, "`") {
		bad = append(bad, "below-floor branch uses a template literal — its ${} interpolations are "+
			"invisible to this guard, so the ratio could be published inside one (#502)")
	}
	// 🔴 NON-ASCII. jsIdentPathRe is `[A-Za-z_$][A-Za-z0-9_$]*`, but JavaScript
	// accepts any ID_Start character, so `var \u03c4 = orgTIER` above the gates and
	// `\u03c4.toFixed(1)` inside the branch scan as the single token `toFixed` — an
	// allowlisted property — and the ratio renders with every rule silent. Measured.
	//
	// Widening the regex to \p{L} would only move the line (homoglyphs, ZWJ, RTL
	// overrides). The scanned region is machine-facing code whose every legitimate
	// token is ASCII; prose lives in string literals, which are blanked before this
	// check, so the em-dashes and "≥" in the badge copy do not reach it.
	for _, r := range code {
		if r > 0x7F {
			bad = append(bad, "below-floor branch contains the non-ASCII character "+
				strconv.QuoteRune(r)+" outside a string literal — identifiers this guard "+
				"cannot even see are refused rather than scanned (#502)")
			break
		}
	}
	if strings.Contains(code, "[") {
		bad = append(bad, "below-floor branch uses `[` (computed member access, or an array "+
			"literal or index). Computed access hides the field name inside a string literal, "+
			"which the property allowlist then never sees — so `[` is refused outright (#502)")
	}

	// (2b) No INVERTING OPERATOR anywhere in the branch. `total.weighted_points` and
	// `totalCost` are both allowed on screen — criterion 4 requires them there — so an
	// allowlist alone cannot stop the quotient being rebuilt from them. Only banning
	// the operators that rebuild it can, and the branch has no legitimate use for one.
	//
	// There are TWO, which an earlier version of this rule got wrong: `/`, and `**`
	// with a negative exponent. `total.weighted_points * 1000 * totalCost ** -1` IS
	// the ratio, names no banned identifier, and was measured reporting zero
	// violations. (`Math.pow` is the third spelling and is already dead: `Math` is not
	// on the root allowlist. The truncate-to-the-cent lives in usdText, outside this
	// branch.)
	for _, op := range []string{"/", "**"} {
		if strings.Contains(code, op) {
			bad = append(bad, "below-floor branch uses `"+op+"` — the ratio must not be "+
				"re-derived from the inputs the branch is allowed to show (#502)")
		}
	}

	// (3) One permitted text sink, and it blanks. Any other textContent/innerHTML
	// write is a second, unaudited route onto the page that bypasses the setText
	// argument checks below.
	// Counted on the WHITESPACE-SQUASHED branch, and matched on `.prop` alone rather
	// than `ident.prop`. The previous regex required an identifier character
	// immediately before the dot, so `provisionalEl\n  .textContent = <expr>` matched
	// NOTHING — and since both names are allowlisted, nothing else fired either. A
	// line break defeated the rule this fixture is the sole defence for. Measured.
	squashedBody := squashSpace(body)
	sinks := jsTextSinkRe.FindAllString(squashedBody, -1)
	const blessedSink = ".textContent=''"
	for _, m := range sinks {
		if strings.HasPrefix(m, blessedSink) {
			continue
		}
		bad = append(bad, "below-floor branch writes "+m+
			"; the only permitted direct text write is blanking provisionalEl")
	}

	if m := jsBannedSinkRe.FindString(code); m != "" {
		bad = append(bad, "below-floor branch calls "+strings.TrimSuffix(m, "(")+
			" — the branch may reach the page only through the two audited setText calls (#502)")
	}

	// (4) The two audited sinks, argument by argument. Redundant with (1) by
	// construction — and deliberately so: this is the assertion that fails loudly
	// if the walker ever stops capturing the real argument.
	for _, id := range []string{"'kpi-tier'", "'kpi-tier-ci'"} {
		args, argsOK := jsCallArgs(body, "setText("+id+",")
		if !argsOK {
			return nil, 0, false // unterminated call: a short read, not a clean file
		}
		for _, arg := range args {
			writes++
			where := "setText(" + id + ", …)"
			bad = append(bad, unrankedIdentViolations(where, arg)...)
			// No exception here: the truncate-to-the-cent lives in usdText, outside
			// this branch, so nothing published into either sink needs to divide.
			argCode, argOK := jsStripStrings(arg)
			if !argOK {
				bad = append(bad, where+" has an unterminated string literal")
				continue
			}
			for _, op := range []string{"/", "**"} {
				if strings.Contains(argCode, op) {
					bad = append(bad, where+" uses `"+op+"` — the ratio must never be "+
						"re-derived inside the text the tile publishes (#502)")
				}
			}
			if strings.Contains(argCode, "`") || strings.Contains(argCode, "[") {
				bad = append(bad, where+" uses a template literal or computed member access, "+
					"either of which hides an expression from this guard (#502)")
			}
		}
	}

	if !strings.Contains(body, "classList.add('kpi-noscore')") {
		bad = append(bad, "below-floor branch does not apply the shared kpi-noscore treatment")
	}
	return bad, writes, true
}

// TestDashboard_JSCallArgsCapturesTheArgument is the guard on the GUARD. Every
// #502 assertion below rests on jsCallArgs capturing exactly the argument text of
// the call it was pointed at — and the first version did not. Starting paren
// matching at the prefix's trailing comma made depth start at 0, so the call's own
// ')' drove it to -1 and the walker returned at some later, unrelated ')'.
//
// The failure was silent in BOTH directions, which is why it survived review: the
// 'kpi-tier' capture over-ran three statements past its call (so two mutants were
// killed for the wrong reason, by text that was never the argument), and the
// 'kpi-tier-ci' capture stopped after about thirty characters (so the one line a
// ratio would be smuggled through was inspected for its opening fragment).
//
// Both boundaries are asserted here, on synthetic input where the answer is known
// exactly and on the real file where the regression actually happened.
func TestDashboard_JSCallArgsCapturesTheArgument(t *testing.T) {
	t.Run("synthetic: exact capture across nesting and quoted parens", func(t *testing.T) {
		src := "before();\nsetText('kpi-tier', a(b(c)) + ') not a delimiter (' + d);\nafter();\n"
		args, ok := jsCallArgs(src, "setText('kpi-tier',")
		if !ok {
			t.Fatal("jsCallArgs reported not-ok on a well-formed call")
		}
		if len(args) != 1 {
			t.Fatalf("captured %d args, want 1: %q", len(args), args)
		}
		const want = " a(b(c)) + ') not a delimiter (' + d"
		if args[0] != want {
			t.Errorf("capture = %q, want %q — the walker must start AT the argument and stop "+
				"at the call's own ')'", args[0], want)
		}
	})

	t.Run("synthetic: an unterminated call is a hard failure, not a short read", func(t *testing.T) {
		if _, ok := jsCallArgs("setText('kpi-tier', 'oops'", "setText('kpi-tier',"); ok {
			t.Error("jsCallArgs reported ok on an unterminated call; a silent short read is " +
				"exactly how a guard reports CLEAN on a file it never finished reading")
		}
		if _, ok := jsCallArgs("setText('kpi-tier', 'x');", "setText'kpi-tier',"); ok {
			t.Error("jsCallArgs reported ok for a prefix containing no '('")
		}
	})

	t.Run("real file: neither over-run nor truncated", func(t *testing.T) {
		_, js := get(t, jsPath)
		fn, ok := jsBlockAfter(js, "function renderKPIs(data, pooled, teamMode) {")
		if !ok {
			t.Fatal("renderKPIs not found")
		}
		body, ok := jsBlockAfter(jsStrip(fn), "} else if (orgUnranked) {")
		if !ok {
			t.Fatal("no below-floor branch")
		}
		for _, tc := range []struct{ id, mustEndWith string }{
			// The last fragment of each real argument. Present => not truncated.
			{"'kpi-tier'", "'NOT ENOUGH EVIDENCE TO SCORE'"},
			{"'kpi-tier-ci'", "accepted outcomes, or an outcome with no measured tokens.'"},
		} {
			args, ok := jsCallArgs(body, "setText("+tc.id+",")
			if !ok {
				t.Fatalf("jsCallArgs(%s) reported not-ok against the real file", tc.id)
			}
			if len(args) != 1 {
				t.Fatalf("captured %d writes to %s, want exactly 1: %q", len(args), tc.id, args)
			}
			arg := args[0]
			if !strings.Contains(arg, tc.mustEndWith) {
				t.Errorf("capture for %s is TRUNCATED — it does not reach %q. Captured:\n%s",
					tc.id, tc.mustEndWith, arg)
			}
			// Over-run detectors: a capture that ran past its own ')' would swallow the
			// statements that follow, none of which can legally appear inside an argument.
			for _, spill := range []string{"setText(", "classList", "provisionalEl"} {
				if strings.Contains(arg, spill) {
					t.Errorf("capture for %s OVER-RAN its call — it contains %q, which belongs to a "+
						"following statement. Captured:\n%s", tc.id, spill, arg)
				}
			}
		}
	})
}

// pinnedHelperBodies is the #502 allowlist's other half.
//
// unrankedRootAllow admits FUNCTIONS BY NAME — setText, num, usdText, usdUnder,
// spendTextFor — and a name says nothing about behaviour. Each of these is one to
// six lines and lives outside the guarded branch, so an edit there publishes the
// withheld ratio while the branch's own text stays byte-identical and every rule
// reports clean. Both were measured doing exactly that:
//
//   - setText rewritten to append ' (TIER …)' for id === 'kpi-tier-ci', reading
//     module-scope lastScores. Whole package green, ratio on screen. setText is
//     the branch's ONLY route to the page, and nothing pinned it.
//   - usdText rewritten with a regex literal `/'/` between two statements. That
//     shifts jsStripStrings' quote frame by one while keeping the quote count
//     even, so the identifier scan read blanked regions and returned ok. Package
//     green, ratio in the cause line AND the spend tile.
//
// Scanning these bodies more cleverly is the wrong answer — every scan has a blind
// spot and these functions are too small to need one. The bodies are pinned
// VERBATIM (whitespace-normalised). Any edit fails loudly and forces a re-audit,
// which for a five-line pure function is the correct cost.
var pinnedHelperBodies = map[string]string{
	"function $(id) {":              `return document.getElementById(id);`,
	"function setText(id, value) {": `$(id).textContent = value;`,
	"function num(v) {":             `return (typeof v === 'number' && isFinite(v)) ? v : 0;`,
	"function usdText(v) {": `var n = num(v); ` +
		`if (n > 0 && n < 0.01) { return '<$0.01'; } ` +
		`if (!isFinite(n * 100)) { return '$' + n.toFixed(2); } ` +
		`return '$' + (Math.floor(Number((n * 100).toFixed(6))) / 100).toFixed(2);`,
	"function usdUnder(v, floor) {": `if (num(v) >= num(floor)) { return '\u2265' + usdText(floor); } ` +
		`var s = usdText(v); ` +
		`return s === usdText(floor) ? '<' + s : s;`,
	"function spendTextFor(totalCostUSD) {": `return totalCostUSD < MIN_RANKED_COST_USD ` +
		`? usdUnder(totalCostUSD, MIN_RANKED_COST_USD) ` +
		`: usdText(totalCostUSD);`,
}

// TestDashboard_AllowlistedHelpersArePinned holds pinnedHelperBodies against the
// served asset, and — just as importantly — holds the PIN LIST against the
// allowlist, so the two cannot drift apart silently.
func TestDashboard_AllowlistedHelpersArePinned(t *testing.T) {
	_, js := get(t, jsPath)

	for header, want := range pinnedHelperBodies {
		body, ok := jsBlockAfter(js, header)
		if !ok {
			t.Errorf("%s not found — a helper the #502 allowlist admits by name is gone or "+
				"renamed; re-audit before updating this pin", header)
			continue
		}
		if got := squashSpace(body); got != squashSpace(want) {
			t.Errorf("%s body changed.\n  got:  %s\n  want: %s\nThe #502 branch calls this "+
				"function by a name the allowlist admits, so whatever this body can reach, the "+
				"below-floor tile can publish. Re-audit, then update pinnedHelperBodies.",
				header, got, squashSpace(want))
		}
	}

	// DRIFT GUARD. Every function-valued name on the root allowlist must be pinned
	// above. Without this, adding a helper to unrankedRootAllow silently re-opens
	// the hole this test closes.
	for name := range unrankedRootAllow {
		if !strings.Contains(js, "function "+name+"(") {
			continue // a keyword, a constant, or a local — not a helper
		}
		pinned := false
		for header := range pinnedHelperBodies {
			if strings.HasPrefix(header, "function "+name+"(") {
				pinned = true
				break
			}
		}
		if !pinned {
			t.Errorf("%q is on unrankedRootAllow and is a function in dashboard.js, but its "+
				"body is not pinned. The branch may call it, so its body is part of the "+
				"guard — add it to pinnedHelperBodies.", name)
		}
	}
}

// TestDashboard_NoDynamicPropertyRedefinition closes the last measured bypass that
// needs no change to the guarded branch at all.
//
// `Object.defineProperty(total, 'weighted_points', { get: … })` placed anywhere in
// renderKPIs turns the branch's own reviewed line —
// `num(total.weighted_points).toFixed(1)` — into a publisher of the ratio. Every
// #502 rule is about the branch's TEXT, and the text does not change. Measured:
// package green, ratio on screen.
//
// No rule about the branch can catch that, so the mechanism is banned file-wide.
// dashboard.js has never used any of these; it renders a JSON response into a
// static page and has no need to redefine an accessor.
func TestDashboard_NoDynamicPropertyRedefinition(t *testing.T) {
	_, js := get(t, jsPath)
	code, ok := jsStripStrings(jsStrip(js))
	if !ok {
		t.Fatal("dashboard.js has an unterminated string literal or block comment")
	}
	for _, mech := range []string{
		"defineProperty", "defineProperties", "__defineGetter__", "__defineSetter__",
		"Proxy", "Reflect.", "setPrototypeOf", "__proto__",
	} {
		if strings.Contains(code, mech) {
			t.Errorf("dashboard.js uses %s. A getter installed on the response object makes "+
				"the #502 branch publish the withheld ratio through its own reviewed line, "+
				"with the branch text unchanged and every guard silent — so the mechanism is "+
				"refused rather than audited.", mech)
		}
	}
}

// moneyConcatRe matches a `$` at the very end of a string literal that is being
// concatenated with an expression — i.e. any hand-rolled money rendering,
// whether the literal is bare ('$' + x) or carries a label ('spent $' + x).
var moneyConcatRe = regexp.MustCompile(`\$["']\+`)

// TestDashboard_MoneyHasOneFormatter pins that a USD amount has exactly one
// rendering on this page.
//
// The #502 review found the same quantity formatted two ways and the two
// disagreeing in public: the SPEND tile rounded totalCost while the cause line
// truncated it, so at $4.997 the tile read "$5.00" beside a caption asserting the
// spend was below the $5.00 floor. Fixing the tile alone would have moved the
// disagreement rather than ended it — the cost-composition drawer renders the same
// window's spend, one scroll down.
//
// So: no bare `'$' + …toFixed(2)` anywhere, with two exceptions that are listed
// rather than pattern-matched, because each needs a REASON and a reason cannot be
// expressed as a regex.
func TestDashboard_MoneyHasOneFormatter(t *testing.T) {
	_, js := get(t, jsPath)

	// The exceptions, by the expression they render. Neither is an AMOUNT of money:
	//   - cost_per_point is a RATE, $ per weighted point (#239). Truncating a rate
	//     to the cent is a different question from truncating a spend figure, and
	//     it belongs to that issue. (It does render "$0.00/pt" for a sub-cent rate,
	//     which is worth its own look.)
	//   - the compare view renders signed DELTAS whose semantics are under separate
	//     review (#605), explicitly out of scope here.
	type exemption struct{ match, why string }
	exempt := []exemption{
		{"cost_per_point", "cost_per_point is a RATE, $ per weighted point (#239) — not an " +
			"amount of spend. Whether a rate should truncate is that issue's question. " +
			"(It does render \"$0.00/pt\" for a sub-cent rate, which is worth its own look.)"},
		{"cost-per-point", "the same rate, in its CI title (#239)."},
		{"signStr(dCost)", "the compare view renders a signed DELTA, and delta semantics are " +
			"under separate review (#605) — explicitly out of scope here."},
	}

	var offenders []string
	for _, line := range strings.Split(jsStrip(js), "\n") {
		// Matches a `$` immediately before ANY string literal's closing quote, not
		// just a standalone '$'. The original #502 defect was
		// `' points from $' + totalCost.toFixed(2)` — squashed, that contains neither
		// `'$'+` nor `"$"+`, so the first version of this guard could not have caught
		// the bug its own doc comment cites. It also missed a live instance:
		// `'net credit balance $' + Math.abs(totalPaid).toFixed(2)`.
		squashed := squashSpace(line)
		if !moneyConcatRe.MatchString(squashed) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		// usdText's own body is where the one formatter lives.
		if strings.Contains(squashed, "Math.floor(Number((n*100)") ||
			strings.Contains(squashed, "if(!isFinite(n*100))") {
			continue
		}
		skip := false
		for _, e := range exempt {
			if strings.Contains(squashed, squashSpace(e.match)) {
				skip = true
				break
			}
		}
		if !skip {
			offenders = append(offenders, trimmed)
		}
	}
	for _, o := range offenders {
		t.Errorf("a USD amount is rendered outside usdText: %s\n\tTwo formatters for one "+
			"quantity is how \"$5.00 of measured AI spend — below the $5.00 evidence floor\" "+
			"happened. Route it through usdText, or add it to `exempt` above WITH a reason.", o)
	}

	// The exemption list must stay honest: an entry that no longer matches anything
	// is a stale licence for the next edit to hide behind.
	stripped := squashSpace(jsStrip(js))
	for _, e := range exempt {
		if !strings.Contains(stripped, squashSpace(e.match)) {
			t.Errorf("exemption %q (%s) matches nothing in dashboard.js — remove it rather "+
				"than leaving an unused licence to bypass the one-formatter rule", e.match, e.why)
		}
	}
}

// TestDashboard_JSStripStringsIsExact is the second guard-on-a-guard. Every
// allowlist and operator rule above reads jsStripStrings' OUTPUT, so a bug here
// makes all of them report clean on source they never saw — the same class of
// silent false-green jsCallArgs had.
func TestDashboard_JSStripStringsIsExact(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		ok             bool
	}{
		{
			// The escape clause. Without it the scanner takes the escaped quote as the
			// terminator, desynchronises, and swallows the code after it — orgTIER
			// disappears from the scan while remaining in the file.
			name: "an escaped quote does not terminate the literal",
			in:   `a = 'don\'t' + orgTIER;`, want: `a = '' + orgTIER;`, ok: true,
		},
		{"double quotes", `a = "x'y" + b;`, `a = "" + b;`, true},
		{"template literal is blanked whole", "a = `x${b}y` + c;", "a = `` + c;", true},
		{"block comment becomes one space", "a /* c */ + b", "a   + b", true},
		{"nothing to strip", "a + b.c;", "a + b.c;", true},
		// Both unterminated forms must REPORT, not truncate.
		{"unterminated literal", "a = 'oops; b = c / d;", "", false},
		{"unterminated block comment", "a /* oops; b = c / d;", "", false},
		{"trailing backslash at end of input", `a = 'x\`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := jsStripStrings(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("jsStripStrings(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDashboard_KPISuppressesUnrankedRatio is the criterion-3 guard: when the org
// rollup is below the evidence floor, the tile WITHHOLDS the ratio.
func TestDashboard_KPISuppressesUnrankedRatio(t *testing.T) {
	_, body := get(t, jsPath)
	fn, ok := jsBlockAfter(body, "function renderKPIs(data, pooled, teamMode) {")
	if !ok {
		t.Fatal("renderKPIs not found — the #502 guards cannot be scoped")
	}

	// LIVENESS of the sink rule, against the real file only (the synthetic fixtures
	// below legitimately omit statements they are not exercising). The real branch
	// blanks provisionalEl exactly once; if that count is ever 0 the sink regex is
	// matching nothing, and every unaudited write would pass unseen forever.
	if n := strings.Count(squashSpace(unrankedBranchSource(t)), "provisionalEl.textContent=''"); n != 1 {
		t.Errorf("found %d blessed `provisionalEl.textContent = ''` writes in the below-floor "+
			"branch, want exactly 1 — at 0 the text-sink rule is reading nothing", n)
	}

	bad, writes, ok := unrankedTileViolations(fn)
	if !ok {
		t.Fatal("the #502 guard could not read renderKPIs: either there is no `else if (orgUnranked)` " +
			"branch (the org tile never suppresses the ratio) or a setText call in it is unterminated. " +
			"Both are hard failures — a guard that parses nothing reports clean.")
	}
	if writes < 2 {
		t.Fatalf("parsed only %d text writes in the below-floor branch; the branch must set BOTH "+
			"the headline and its cause line, and a guard that parses nothing reports clean", writes)
	}
	for _, v := range bad {
		t.Error(v)
	}

	// The ranked branch MUST still print the ratio. This is the positive control
	// on the walkers themselves: if jsBlockAfter/jsCallArgs silently stopped
	// finding anything, this assertion fails instead of the suppression check
	// passing vacuously.
	ranked, ok := jsBlockAfter(jsStrip(fn), "} else {")
	if !ok {
		t.Fatal("no healthy `else` branch in renderKPIs")
	}
	if !strings.Contains(ranked, "orgTIER.toFixed(1)") {
		t.Error("the ranked branch no longer headlines orgTIER — #502 withholds the number below " +
			"the floor, it does not remove the headline")
	}

	// Copy and cause: the badge reuses the existing taxonomy, and the cause line
	// keeps the measured inputs on screen (criterion 4) rather than only naming a
	// verdict.
	for _, marker := range []string{
		"var orgUnranked = !noScore && !orgFree && !total.ranked",
		"'NOT ENOUGH SPEND TO SCORE'",
		"'NOT ENOUGH EVIDENCE TO SCORE'",
		"num(total.weighted_points).toFixed(1)",
		"' points from '",
		"Check token capture.",
		// Criterion 4 is only satisfied if the spend is legible. '$0.00' is not:
		// the canonical #502 window is $0.0001, and rounded it is both invisible
		// as evidence and indistinguishable from the FREE band one gate above.
		"spendTextFor(totalCost)",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js missing #502 marker %q", marker)
		}
	}
}

// squashSpace removes all whitespace, so an assertion can be about the RULE a line
// expresses rather than about one formatting of it.
func squashSpace(src string) string { return strings.Join(strings.Fields(src), "") }

// unrankedBranchSource returns the below-floor branch body of the SERVED
// dashboard.js, comments stripped. Fatal (never a soft skip) when it cannot be
// located, so a rename turns the assertions below red instead of vacuous.
func unrankedBranchSource(t *testing.T) string {
	t.Helper()
	_, js := get(t, jsPath)
	fn, ok := jsBlockAfter(js, "function renderKPIs(data, pooled, teamMode) {")
	if !ok {
		t.Fatal("renderKPIs not found")
	}
	body, ok := jsBlockAfter(jsStrip(fn), "} else if (orgUnranked) {")
	if !ok {
		t.Fatal("no `else if (orgUnranked)` branch in renderKPIs")
	}
	return body
}

// TestDashboard_UnrankedAdviceIsScopedToTheSpendArm pins WHERE "Check token
// capture." is allowed to appear.
//
// The branch has two arms and they describe different orgs. On the spend arm the
// meter really may not be reading. On the OTHER arm the org has cleared $5.00 of
// measured, captured spend and is unranked only because fewer than
// MIN_RANKED_OUTCOMES outcomes merged — a $120 window with 2 merged PRs used to
// render "…clear of the spend floor but short of the rest… Check token capture.",
// sending the reader to debug the one part of the system demonstrably working.
func TestDashboard_UnrankedAdviceIsScopedToTheSpendArm(t *testing.T) {
	body := unrankedBranchSource(t)

	const advice = "Check token capture."
	if n := strings.Count(body, advice); n != 1 {
		t.Fatalf("%q appears %d times in the below-floor branch, want exactly 1 — it is true of "+
			"the spend arm only", advice, n)
	}
	spendArm := strings.Index(body, "? 'below the '")
	otherArm := strings.Index(body, ": 'clear of the spend floor")
	at := strings.Index(body, advice)
	if spendArm < 0 || otherArm < 0 {
		t.Fatalf("could not locate both arms of the cause-line ternary (spend=%d other=%d)",
			spendArm, otherArm)
	}
	if spendArm >= at || at >= otherArm {
		t.Errorf("%q sits outside the belowSpendFloor arm (spendArm=%d advice=%d otherArm=%d). "+
			"An org that cleared the spend floor is not told to debug its capture.",
			advice, spendArm, at, otherArm)
	}
}

// TestDashboard_UnrankedCauseLineShowsTheThinSpend pins the two rendering rules on
// the spend figure, both of which exist because that figure is read AGAINST the
// floor named in the same sentence.
//
//   - It is never a bare "$0.00". The canonical #502 window is $0.0001; rounded to
//     the cent, criterion 4's "measured input on screen" is not on screen, and it
//     is visually identical to the FREE band one gate above, where cost is
//     genuinely zero and the yield is unbounded — a different reading entirely.
//   - It TRUNCATES rather than rounds. At totalCost in [4.995, 5.00) rounding
//     renders "$5.00 of measured AI spend — below the $5.00 evidence floor", a
//     sentence that contradicts itself. Truncating down cannot cross the floor.
func TestDashboard_UnrankedCauseLineShowsTheThinSpend(t *testing.T) {
	body := unrankedBranchSource(t)

	if strings.Contains(body, "totalCost.toFixed(") {
		t.Error("the below-floor cause line formats spend with a direct totalCost.toFixed() — " +
			"that rounds, so at $4.995 the line reads \"$5.00 of measured AI spend — below the " +
			"$5.00 evidence floor\", and at $0.0001 it reads \"$0.00\"")
	}
	// The below-floor arm compares against a floor IN THE SAME SENTENCE, so it must
	// use usdUnder, whose output cannot equal the floor's rendering. usdText alone
	// is not enough — see the property assertions below.
	if !strings.Contains(body, "spendTextFor(totalCost)") {
		t.Error("the below-floor cause line does not route its spend through spendTextFor — the " +
			"tile calls that function too, and two formatters for one number is how \"$5.00 of " +
			"measured AI spend — below the $5.00 evidence floor\" keeps coming back")
	}

	_, js := get(t, jsPath)
	// Whitespace-normalised: these assertions are about the RULE, not about one
	// spelling of it. A pin on the exact source substring fails on `n*100` — an edit
	// that changes nothing — and passes for any identically-spelled implementation
	// however it behaves.
	usd, ok := jsBlockAfter(js, "function usdText(v) {")
	if !ok {
		t.Fatal("usdText not found — the spend-formatting rules cannot be scoped")
	}
	usd = squashSpace(jsStrip(usd))
	if !strings.Contains(usd, "'<$0.01'") {
		t.Error("usdText has no sub-cent form — the canonical #502 window ($0.0001) renders as " +
			"'$0.00', which is both invisible as evidence and indistinguishable from the FREE " +
			"band, where cost is genuinely zero and yield is unbounded")
	}
	if !strings.Contains(usd, "Math.floor(") {
		t.Error("usdText no longer truncates; a ROUNDING formatter lets a figure print as " +
			"having reached a floor it is below ($4.995 -> \"$5.00\")")
	}
	if !strings.Contains(usd, "toFixed(6)") {
		t.Error("usdText truncates without absorbing binary representation error: a plain " +
			"Math.floor(n*100) renders a genuine $0.29 as \"$0.28\" and $8.20 as \"$8.19\", " +
			"because 0.29*100 is 28.999999999999996")
	}

	// usdUnder is what makes "cannot reach the floor" a STRUCTURAL property rather
	// than a tolerance. The tempting argument — that costs are micro-USD-quantised,
	// so nothing lands within toFixed(6)'s tolerance of $5.00 — is false:
	// total_cost_usd is a float64 SUM of such values (RollupTeam), and
	// 2.918582 + 0.956618 + 0.420133 + 0.704667 is 4.999999999999999, below the
	// floor and rendered "$5.00" by truncation alone.
	under, ok := jsBlockAfter(js, "function usdUnder(v, floor) {")
	if !ok {
		t.Fatal("usdUnder not found — nothing guarantees the cause line cannot contradict itself")
	}
	under = squashSpace(jsStrip(under))
	if !strings.Contains(under, "usdText(floor)") {
		t.Error("usdUnder does not compare its rendering against the FLOOR's rendering, so it " +
			"cannot guarantee the two differ — the self-contradiction is back")
	}
	if !strings.Contains(under, "'<'+s") {
		t.Error("usdUnder does not mark a value that renders ON the floor as strictly below it")
	}
	// …and it refuses inputs it cannot honestly describe. Without this, a future
	// caller that forgot to gate would get "<$5.00" for $5.004 — a false claim about
	// a value ABOVE the floor, trading one contradiction for another.
	if !strings.Contains(under, "num(v)>=num(floor)") {
		t.Error("usdUnder does not enforce its own precondition, so out of domain it asserts " +
			"a value is below a floor it is above")
	}
	// …and the out-of-domain answer must not be usdText(v). That returns "$5.00",
	// which the caller concatenates into '… below the $5.00 evidence floor',
	// rebuilding the exact sentence this function exists to delete.
	if !strings.Contains(under, `'\u2265'+usdText(floor)`) {
		t.Error("usdUnder's out-of-domain fallback can still read as agreeing with the floor; " +
			"it must answer with a form that cannot ('>=' the floor), not the plain rendering")
	}
}

// TestDashboard_SpendTileAndCauseLineAgree pins that the org's measured spend has
// exactly ONE rendering on the KPI row.
//
// renderKPIs prints totalCost twice — the SPEND tile and the below-floor cause
// line — both on screen at once, sixty lines apart. When the tile used
// toFixed(2) and the caption used usdText they contradicted each other in both
// directions a reader would notice: at $4.997 the tile read "$5.00" beside a
// sentence asserting the spend was below the $5.00 floor, and on the canonical
// $0.0001 window the tile read "$0.00" — in the larger typeface — while the
// caption read "<$0.01". Splitting the contradiction across two elements does not
// resolve it.
func TestDashboard_SpendTileAndCauseLineAgree(t *testing.T) {
	_, js := get(t, jsPath)
	fn, ok := jsBlockAfter(js, "function renderKPIs(data, pooled, teamMode) {")
	if !ok {
		t.Fatal("renderKPIs not found")
	}
	fn = jsStrip(fn)

	args, ok := jsCallArgs(fn, "setText('kpi-spend',")
	if !ok || len(args) != 1 {
		t.Fatalf("expected exactly one write to kpi-spend, parsed %d (ok=%v)", len(args), ok)
	}
	tile := squashSpace(args[0])

	// The cause line's spend expression, read from the branch rather than assumed.
	branch := unrankedBranchSource(t)
	var caption string
	for _, line := range strings.Split(branch, "\n") {
		if strings.Contains(line, "var spendText") {
			caption = squashSpace(strings.SplitN(line, "=", 2)[1])
		}
	}
	if caption == "" {
		t.Fatal("no `var spendText` assignment in the below-floor branch")
	}
	caption = strings.TrimSuffix(caption, ";")

	// AGREEMENT, not a spelling: both sinks must evaluate the SAME expression. Two
	// different formatters for one number is how this defect returned twice — first
	// round vs truncate, then truncate vs truncate-with-usdUnder, which differ only
	// on a float64 sum a few ulps under the floor (a window RollupTeam can produce).
	if tile != caption {
		t.Errorf("the SPEND tile and the below-floor cause line render totalCost by different "+
			"expressions:\n  tile:    %s\n  caption: %s\nThey are on screen together; they must "+
			"be the same call.", tile, caption)
	}
	if !strings.Contains(tile, "spendTextFor(") {
		t.Errorf("the org spend is not rendered by the shared spendTextFor (%s); a second "+
			"formatter can always drift from the first", tile)
	}
	if strings.Contains(tile, "totalCost.toFixed(") {
		t.Error("the SPEND tile rounds totalCost directly; at $4.997 it prints \"$5.00\" beside " +
			"a caption asserting the spend is below the $5.00 floor")
	}
}

// TestDashboard_UnrankedGuardCatchesTheDefect is the control arm. It runs the
// same guard over renderKPIs sources that CONTAIN the defect and fails if the
// guard reports clean. Without it, the test above cannot tell "the ratio is
// suppressed" from "the guard stopped looking".
func TestDashboard_UnrankedGuardCatchesTheDefect(t *testing.T) {
	const prefix = "function renderKPIs(data, pooled, teamMode) {\n" +
		"  if (noScore) {\n    setText('kpi-tier', 'NO SCORE');\n  } else if (orgFree) {\n" +
		"    setText('kpi-tier', 'FREE');\n"
	const suffix = "  } else {\n    setText('kpi-tier', orgTIER.toFixed(1));\n  }\n}\n"

	mustCatch := []struct{ name, branch string }{
		{
			// THE defect the ruling names: the per-row treatment copied to the tile.
			name: "mutes the ratio but still prints it",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', orgTIER.toFixed(1));\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'insufficient sample to rank');\n",
		},
		{
			name: "badge headline, ratio smuggled into the cause line",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'computed ' + orgTIER.toFixed(1) + ' — below the floor');\n",
		},
		{
			name: "badge and ratio concatenated into one headline",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE (' + num(total.tier).toFixed(1) + ')');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor');\n",
		},
		{
			name: "ratio withheld but the tile keeps its green yield styling",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    setText('kpi-tier-ci', 'below the floor');\n",
		},

		// ⬇⬇ The four below are PROVEN BYPASSES of the previous denylist guard. Each
		// one parsed, each one rendered the ratio on screen, and each one left the
		// whole dashboard suite green. They are the reason the rule is now an
		// allowlist plus a division ban rather than two banned spellings.
		{
			// The ruling's own defect, ONE ALIAS away. A denylist of spellings can
			// never see this coming; the allowlist rejects `orgTIER` and `muted` alike,
			// because neither is on it.
			name: "the defect laundered through a local alias",
			branch: "  } else if (orgUnranked) {\n" +
				"    var muted = orgTIER;\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor — ' + muted.toFixed(1));\n",
		},
		{
			// Re-derivation: the banned identifier never appears. The two operands ARE
			// on the allowlist — they are the measured inputs criterion 4 requires on
			// screen — so only the division ban can catch this.
			name: "ratio re-derived from the two inputs the branch is allowed to show",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', num(total.weighted_points).toFixed(1) + ' points, yield ' +\n" +
				"      (num(total.weighted_points) / (totalCost / 1000)).toFixed(1));\n",
		},
		{
			// Appended to the END of the real cause line. Under the broken walker the
			// kpi-tier-ci capture stopped ~30 characters in, so this was inspected by
			// nothing at all — yet it rendered, and was confirmed visible in headless
			// Chrome.
			name: "ratio appended past the end of the real cause line",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', num(total.weighted_points).toFixed(1) + ' points from $' +\n" +
				"      totalCost.toFixed(2) + ' of measured AI spend. Check token capture. Computed TIER ' +\n" +
				"      orgTIER.toFixed(1));\n",
		},
		{
			// Template literal: jsStripStrings blanks the whole literal, so the
			// interpolation was invisible and the guard reported clean.
			name: "ratio interpolated into a template literal",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', `below the floor — TIER ${orgTIER.toFixed(1)}`);\n",
		},
		{
			// Same hiding place, this time concealing the DIVISION as well.
			name: "ratio re-derived inside a template literal",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', `yield ${(num(total.weighted_points) / (totalCost / 1000)).toFixed(1)}`);\n",
		},
		{
			// Computed member access hides the field name inside a string literal,
			// so the property allowlist never sees `tier`.
			name: "banned field read through computed member access",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor — ' + num(total['tier']).toFixed(1));\n",
		},
		{
			// SOLE-DEFENCE fixture for jsTextSinkRe, and written that way on purpose.
			// `textContent` must stay on the property allowlist (the branch blanks
			// provisionalEl with it), so an unaudited textContent write is the ONE
			// sink the identifier rules cannot catch — every name here is allowlisted.
			// No trailing semicolon: ASI makes that legal as the branch's last
			// statement, and a sink regex requiring `;` matched nothing there.
			//
			// Earlier drafts of this fixture named orgTIER, so the ALLOWLIST killed it
			// and the sink regex could be disabled entirely with the suite still green
			// — the "killed for the wrong reason" trap, reintroduced.
			name: "unaudited textContent write, no trailing semicolon (ASI)",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor');\n" +
				"    provisionalEl.textContent = 'yield ' + num(total.weighted_points).toFixed(1)\n",
		},
		{
			// SOLE-DEFENCE for the whitespace-squash. Same allowlisted-names-only shape
			// as the fixture above, but with the sink split across a line break — the
			// form that matched NOTHING while both names stayed allowlisted, so no rule
			// fired at all. Measured green before the squash.
			name: "unaudited textContent write split across a line break",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor');\n" +
				"    provisionalEl\n" +
				"      .textContent = 'yield ' + num(total.weighted_points).toFixed(1)\n",
		},
		{
			// Killed by the property allowlist (insertAdjacentHTML is not on it), NOT
			// by jsBannedSinkRe — which is redundant by construction. Kept as a
			// statement about the sink, not as evidence that regex fires.
			name: "ratio inserted through insertAdjacentHTML",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor');\n" +
				"    tierValueEl.insertAdjacentHTML('beforeend', 'TIER ' + orgTIER.toFixed(1));\n",
		},
		{
			// The `**` re-derivation staged OUTSIDE the setText arguments, into an
			// allowlisted local. Only the WHOLE-BRANCH operator ban can catch this —
			// the per-argument check sees nothing but `num(spendText).toFixed(1)`, all
			// of it allowlisted. Without this fixture the branch-level ban had no
			// independent coverage: removing `**` from it left the suite green because
			// the argument-level ban still caught the inline form.
			name: "ratio staged into a local with ** -1, then printed",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    spendText = total.weighted_points * 1000 * totalCost ** -1;\n" +
				"    setText('kpi-tier-ci', num(spendText).toFixed(1));\n",
		},
		{
			// The same staging with plain division, so the branch-level `/` ban has
			// independent coverage too.
			name: "ratio staged into a local with /, then printed",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    spendText = num(total.weighted_points) / (totalCost / 1000);\n" +
				"    setText('kpi-tier-ci', num(spendText).toFixed(1));\n",
		},
		{
			// RED: `**` with a negative exponent is the SECOND inverting operator.
			// Every identifier here is allowlisted and no `/` appears, so only the
			// operator ban can catch it. Measured reporting zero violations before it.
			name: "ratio re-derived with ** -1 instead of division",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'yield ' +\n" +
				"      (total.weighted_points * 1000 * totalCost ** -1).toFixed(1));\n",
		},
		{
			// A second text sink, bypassing setText entirely.
			name: "ratio written straight to textContent, bypassing setText",
			branch: "  } else if (orgUnranked) {\n" +
				"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
				"    tierValueEl.classList.add('kpi-noscore');\n" +
				"    setText('kpi-tier-ci', 'below the floor');\n" +
				"    provisionalEl.textContent = 'TIER ' + orgTIER.toFixed(1);\n",
		},
	}
	for _, tt := range mustCatch {
		t.Run(tt.name, func(t *testing.T) {
			bad, writes, ok := unrankedTileViolations(prefix + tt.branch + suffix)
			if !ok {
				t.Fatal("guard failed to parse a fixture that contains the branch")
			}
			// Assert the WALKER ran too, not only that some rule fired. Without this a
			// fixture can be killed by the whole-branch rules while section (4) — the
			// one whose job is to fail loudly when the walker stops capturing — quietly
			// parses zero calls and says nothing.
			if writes != 2 {
				t.Errorf("guard parsed %d setText writes, want 2 — the argument walker found "+
					"nothing and the violations below came from the whole-branch rules alone", writes)
			}
			if len(bad) == 0 {
				t.Error("guard reported CLEAN on a renderKPIs that still publishes the below-floor ratio")
			}
		})
	}

	t.Run("correct suppression is clean", func(t *testing.T) {
		branch := "  } else if (orgUnranked) {\n" +
			"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
			"    tierValueEl.classList.add('kpi-noscore');\n" +
			"    var spendText = spendTextFor(totalCost);\n" +
			"    setText('kpi-tier-ci', num(total.weighted_points).toFixed(1) + ' points from ' + spendText);\n"
		bad, writes, ok := unrankedTileViolations(prefix + branch + suffix)
		if !ok {
			t.Fatal("guard failed to parse the correct fixture")
		}
		if writes != 2 {
			t.Errorf("guard parsed %d writes, want 2", writes)
		}
		if len(bad) != 0 {
			t.Errorf("guard fired on a correctly suppressed tile: %v", bad)
		}
	})

	// An unterminated literal must FAIL, not read clean. Everything after the
	// unbalanced quote is discarded, so a guard that shrugged here would report
	// clean on source it never scanned.
	t.Run("an unterminated string literal is a failure", func(t *testing.T) {
		// Every identifier BEFORE the unbalanced quote is on the allowlist, and the
		// re-derivation sits AFTER it — so if the strip result is silently truncated,
		// nothing is left to trip any other rule and the branch reads clean. A
		// fixture with an unlisted name before the quote would be killed by the
		// allowlist instead, proving nothing about this failure mode.
		branch := "  } else if (orgUnranked) {\n" +
			"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
			"    tierValueEl.classList.add('kpi-noscore');\n" +
			"    setText('kpi-tier-ci', 'below the floor');\n" +
			"    spendText = 'unterminated;\n" +
			"    return num(total.weighted_points) / (totalCost / 1000);\n"
		bad, _, ok := unrankedTileViolations(prefix + branch + suffix)
		if ok && len(bad) == 0 {
			t.Error("guard reported CLEAN on a branch it could not finish reading — " +
				"everything after the unbalanced quote was discarded unscanned")
		}
	})

	// The BLOCK-COMMENT arm of the same failure. jsStrip only removes `//` comments,
	// so an unterminated `/*` reaches jsStripStrings and discards the rest of the
	// branch exactly as an unbalanced quote does.
	t.Run("an unterminated block comment is a failure", func(t *testing.T) {
		branch := "  } else if (orgUnranked) {\n" +
			"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
			"    tierValueEl.classList.add('kpi-noscore');\n" +
			"    /* unterminated\n" +
			"    setText('kpi-tier-ci', 'below the floor');\n"
		bad, _, ok := unrankedTileViolations(prefix + branch + suffix)
		if ok && len(bad) == 0 {
			t.Error("guard reported CLEAN on a branch whose block comment swallowed the rest of it")
		}
	})

	// A comment quoting the defect must not fail the build (jsStrip's job).
	t.Run("the defect quoted in a comment is ignored", func(t *testing.T) {
		branch := "  } else if (orgUnranked) {\n" +
			"    // never setText('kpi-tier', orgTIER.toFixed(1)) here (that is the #502 defect)\n" +
			"    setText('kpi-tier', 'NOT ENOUGH SPEND TO SCORE');\n" +
			"    tierValueEl.classList.add('kpi-noscore');\n" +
			"    setText('kpi-tier-ci', 'below the floor');\n"
		if bad, _, _ := unrankedTileViolations(prefix + branch + suffix); len(bad) != 0 {
			t.Errorf("guard fired on a comment: %v", bad)
		}
	})
}

// TestDashboard_KPIGateOrderIsMonotoneInCost pins the ORDER of the three
// withheld-headline gates, which is what keeps the tile monotone in cost.
//
// Reading left to right as spend rises with points held: $0.00 is FREE (the
// ratio is UNDEFINED, not small — the engine never divides), $0.01–$4.99 is
// below-floor (defined but too thin to publish), $5.00+ headlines the number.
// The presentation never goes scored → unscored → scored: everything below the
// floor is one unranked band, and FREE is the more specific reading inside it.
// Swap orgFree and orgUnranked and a genuinely free org gets the "not enough
// spend" caption, which is true but strictly less informative; put orgUnranked
// ahead of noScore and an org that merged nothing gets told to check its token
// capture instead of being told nothing shipped.
func TestDashboard_KPIGateOrderIsMonotoneInCost(t *testing.T) {
	_, body := get(t, jsPath)
	fn, ok := jsBlockAfter(body, "function renderKPIs(data, pooled, teamMode) {")
	if !ok {
		t.Fatal("renderKPIs not found")
	}
	fn = jsStrip(fn)
	order := []string{"if (noScore) {", "} else if (orgFree) {", "} else if (orgUnranked) {", "} else {"}
	prev := -1
	for _, gate := range order {
		at := strings.Index(fn, gate)
		if at < 0 {
			t.Fatalf("renderKPIs has no %q gate", gate)
		}
		if at <= prev {
			t.Errorf("gate %q is out of order in renderKPIs; required order is %v", gate, order)
		}
		prev = at
	}
	// The below-floor gate must also EXCLUDE the two cases above it, or a
	// zero-point org reaches it and reads "not enough spend" when the honest
	// statement is that nothing was merged.
	if !strings.Contains(fn, "!noScore && !orgFree && !total.ranked") {
		t.Error("orgUnranked must exclude noScore and orgFree explicitly (#499/#500 must keep their captions)")
	}
}

// TestDashboard_KPIFloorsMatchEngine pins the cross-language duplication #502
// introduces: the tile names the floor it missed ("below the $5.00 evidence
// floor"), but the server sends only the verdict, so the thresholds are mirrored
// in JS. Move scoring.MinRankedCostUSD without touching dashboard.js and the
// caption starts citing a floor that no longer exists — a wrong explanation
// attached to a correct verdict, which is worse than no explanation. This test
// is the only thing tying the two together.
func TestDashboard_KPIFloorsMatchEngine(t *testing.T) {
	_, body := get(t, jsPath)
	for _, want := range []string{
		fmt.Sprintf("var MIN_RANKED_COST_USD = %.2f;", scoring.MinRankedCostUSD),
		fmt.Sprintf("var MIN_RANKED_OUTCOMES = %d;", scoring.MinRankedOutcomes),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard.js does not mirror the engine floor: want %q", want)
		}
	}
	// The dollar figure in the caption must be FORMATTED from the constant, never
	// re-typed, so there is exactly one place to change — and through the SAME
	// formatter as the spend it is compared against, so the two cannot skew.
	if !strings.Contains(body, "usdText(MIN_RANKED_COST_USD)") {
		t.Error("the caption must render the floor via usdText(MIN_RANKED_COST_USD), not " +
			"hardcode it and not format it a second way")
	}
	if !strings.Contains(body, "' accepted outcomes, or an outcome with no measured tokens.'") {
		t.Error("the non-spend cause line must name the remaining floors without asserting one of them")
	}
}

// TestDashboard_TeamRowsHonourRanked pins #603. `var isRanked = teamMode ? true :
// !!d.ranked` hardcoded EVERY team row to ranked-green evidence at any cost
// level, so a 2-outcome, $0.30 team rendered exactly like a fully evidenced one.
// It predates #502 — the field it now reads is new, but the hardcode was already
// a lie about rows the server never vouched for.
func TestDashboard_TeamRowsHonourRanked(t *testing.T) {
	_, body := get(t, jsPath)
	// Comment-stripped: the fix DOCUMENTS the defect it removes, quoting the old
	// expression verbatim, and that comment is worth keeping — a bare
	// strings.Contains over the raw asset would fail on the explanation of the fix.
	if strings.Contains(jsStrip(body), "teamMode ? true : !!d.ranked") {
		t.Error("dashboard.js still hardcodes team rows as ranked (#603)")
	}
	panelBody, ok := jsBlockAfter(body, "function buildPanel(seg, teamMode, since) {")
	if !ok {
		t.Fatal("buildPanel not found — the #603 guard cannot be scoped")
	}
	// Structural, not literal: any reintroduction of a mode-dependent ranked value
	// fails here, including spellings the string check above would miss.
	var assign string
	for _, line := range strings.Split(jsStrip(panelBody), "\n") {
		if strings.Contains(line, "var isRanked") {
			assign = strings.TrimSpace(line)
		}
	}
	if assign == "" {
		t.Fatal("no isRanked assignment in buildPanel")
	}
	if strings.Contains(assign, "teamMode") {
		t.Errorf("isRanked is decided by the display mode, not by the row: %q", assign)
	}
	if !strings.Contains(assign, "d.ranked") {
		t.Errorf("isRanked does not read the row's ranked field: %q", assign)
	}
}

// TestDashboard_BelowFloorRowSaysSoInText closes a WCAG 2.1 SC 1.4.1 (Use of
// Color) failure that #603 made reachable.
//
// Measured in Chrome on a team-mode payload with one below-floor team, BEFORE the
// fix: the below-floor row's label carried class "below" (muted colour), its
// aria-label was null, its TIER title was the empty string, and the floor divider
// count was 0. Muted colour was the row's ONLY signal, on every channel.
//
// Two gates caused it and both are asserted here: buildTierReading gated its
// explanatory title on !teamMode, and buildYieldBar's aria-label chain covered
// only ranked-developer / no-score / free rows. Neither mattered before #603,
// because no team row could be below the floor.
func TestDashboard_BelowFloorRowSaysSoInText(t *testing.T) {
	_, js := get(t, jsPath)

	// (a) The TIER number's title is no longer developer-only. A `!teamMode` gate
	// with nothing after it puts a below-floor team row back on colour alone.
	reading, ok := jsBlockAfter(js, "function buildTierReading(d, teamMode, isRanked) {")
	if !ok {
		t.Fatal("buildTierReading not found — the a11y guard cannot be scoped")
	}
	reading = jsStrip(reading)
	if !strings.Contains(reading, "if (!teamMode) {") {
		t.Fatal("buildTierReading's title gate changed shape; re-verify this guard")
	}
	teamArmAt := strings.Index(reading, "} else if (!isRanked) {")
	if teamArmAt < 0 {
		// Fatal, not Error: the slice below would otherwise index at -1 and PANIC,
		// which aborts the whole test binary and buries this diagnostic under a
		// stack trace — on exactly the regression this test exists to name.
		t.Fatal("buildTierReading gives a below-floor TEAM row no title (#603/WCAG 1.4.1): the " +
			"!teamMode gate has no else arm, so muted colour is the row's only signal")
	}
	if !strings.Contains(reading, "below ranking floor") {
		t.Error("buildTierReading does not state the ranking verdict in text; reuse the compare " +
			"view's wording ('below ranking floor') rather than minting a second phrase")
	}
	teamArm := reading[teamArmAt:]
	// Team rows carry no sample_n and no flagged count by construction (sample_n is
	// the k-anon denominator, deliberately withheld — see teamScoreJSON). A title
	// that voiced either would read a field the wire never sends and render "0".
	for _, absent := range []string{"sample_n", "flagged_outcomes", "unrankedReasons"} {
		if strings.Contains(teamArm, absent) {
			t.Errorf("the team below-floor title reads %s, which team rows do not carry — "+
				"it would render a fabricated 0", absent)
		}
	}
	// …and it must not assert the SPEND cause unconditionally. `ranked` is a
	// three-way conjunction, so "$120.00 of measured spend" placed straight after
	// "below ranking floor" names a cause that is false whenever the row cleared the
	// spend floor and fell short on outcomes — the exact fabrication renderKPIs is
	// written to avoid two hundred lines up.
	// Assert the CONDITION, not merely that the constant appears somewhere in the
	// title text — it does, inside the spend arm's own copy, so a `tier.title = true
	// ? …` mutant satisfied a bare Contains and survived.
	if !strings.Contains(squashSpace(teamArm), "tier.title=num(d.total_cost_usd)<MIN_RANKED_COST_USD?") {
		t.Error("the team below-floor title is not GATED on the row's spend clearing " +
			"MIN_RANKED_COST_USD, so it asserts one cause for a three-way conjunction: a $120 " +
			"team held back by its outcome count is told its spend is the problem")
	}
	if !strings.Contains(teamArm, "clear of the spend floor but short of the rest") {
		t.Error("the team below-floor title has no arm for a row that CLEARED the spend floor; " +
			"the KPI cause line's wording exists precisely for that case")
	}

	// (a2) The DEVELOPER arm has the same duty, and a developer row carries all
	// three inputs, so it can name the conditions it actually fails.
	if strings.Contains(reading, "insufficient sample to rank") {
		t.Error("the developer below-floor title still asserts 'insufficient sample to rank'; " +
			"that is false for a 20-outcome, $500 row held back by one zero-token outcome, " +
			"with both cleared numbers printed beside it (#136)")
	}
	if !strings.Contains(reading, "unrankedReasons(d)") {
		t.Error("the developer below-floor title does not route through unrankedReasons, so it " +
			"cannot name which of the three ranking conditions actually failed")
	}

	// (b) The row itself states the verdict for a screen reader. Without this the
	// verdict is a sighted-only affordance: team rows have no floor divider either.
	bar, ok := jsBlockAfter(js, "function buildYieldBar(d, teamMode, isRanked, scale, since) {")
	if !ok {
		t.Fatal("buildYieldBar not found")
	}
	barBody := jsStrip(bar)
	if !strings.Contains(barBody,
		"srNote = 'below ranking floor: insufficient evidence to rank'") {
		t.Error("a below-floor row carries no screen-reader text (#603/WCAG 1.4.1) — the aria " +
			"chain covers ranked-developer, no-score and free rows and drops through here")
	}
	// (c) …and it is REAL TEXT, not an aria-label. A .ybar-row is a bare <div>,
	// i.e. role="generic", and the accessible-name computation refuses to name a
	// generic element: an aria-label here is computed and then discarded, so the
	// fix would be inert. A role that accepts a name would also switch on the three
	// pre-existing labels, which were written for a row whose contents were assumed
	// unreadable and now duplicate the value cell. Off-screen text needs neither.
	if !strings.Contains(barBody, "el('span', 'ybar-sr', srNote)") {
		t.Error("the below-floor verdict is not appended as off-screen text — an aria-label on " +
			"a role=\"generic\" .ybar-row is discarded by the accessible-name computation and " +
			"reaches nobody (#274/#603)")
	}
	// The class must exist and must CLIP rather than remove: display:none would take
	// the text out of the accessibility tree, which is the only place it lives.
	style := servedStyle(t)
	if !regexp.MustCompile(`(?s)\.ybar-sr\s*\{[^}]*clip-path`).MatchString(style) {
		t.Error(".ybar-sr has no clip-path rule — the screen-reader text is either visible or " +
			"missing its styling entirely")
	}
	if regexp.MustCompile(`(?s)\.ybar-sr\s*\{[^}]*display\s*:\s*none`).MatchString(style) {
		t.Error(".ybar-sr uses display:none, which removes the text from the accessibility " +
			"tree — the one place it is meant to exist")
	}
}

// TestDashboard_PanelScaleIgnoresUnrankedRows pins that a suppressed ratio does
// not come back as bar LENGTH one scroll below the tile.
//
// buildPanel computed its scale over ALL rows. Measured with the ruling's own
// numbers — a below-floor row at 28 points / $0.0001 (TIER 2.8e8) beside a genuine
// ranked 4.2 — the below-floor row took 100% of the track and the honest ranked
// reading rendered at 0%: invisible. The tile withholds that number precisely
// because its denominator is meaningless; the panel must not re-publish it.
//
// Pre-existing, not introduced by #502/#603: the scale never consulted `ranked`,
// so below-floor DEVELOPER rows have always distorted it. Same value, same page.
func TestDashboard_PanelScaleIgnoresUnrankedRows(t *testing.T) {
	_, js := get(t, jsPath)
	panelBody, ok := jsBlockAfter(js, "function buildPanel(seg, teamMode, since) {")
	if !ok {
		t.Fatal("buildPanel not found — the scale guard cannot be scoped")
	}
	panelBody = jsStrip(panelBody)

	at := strings.Index(panelBody, "var scale = 0;")
	if at < 0 {
		t.Fatal("no `var scale = 0;` in buildPanel; the scale computation changed shape")
	}
	// Scope to the scale computation itself: `d.ranked` appears again further down
	// in the row loop, and an unscoped search would pass against the old code.
	end := strings.Index(panelBody[at:], "if (scale <= 0) { scale = 1; }")
	if end < 0 {
		t.Fatal("no all-zero scale guard after the scale computation")
	}
	scaleBlock := panelBody[at : at+end]

	if !strings.Contains(scaleBlock, "d.ranked") {
		t.Error("the panel scale is computed over every row including below-floor ones (#502): " +
			"a 2.8e8 unranked row takes the whole track and flattens every ranked bar to zero")
	}
	// There must be NO all-rows fallback. One was tried and re-imported the same
	// distortion one scope down: an all-below-floor panel — which per-work_type
	// splitting makes the common case in team mode — scaled to its own 2.8e8 outlier
	// and flattened two readable rows to zero width.
	if strings.Count(scaleBlock, "rows.forEach") != 1 {
		t.Errorf("the panel scale has %d row passes, want 1 — an all-rows fallback lets the "+
			"outlier set the scale again whenever no row is ranked",
			strings.Count(scaleBlock, "rows.forEach"))
	}

	// Excluding unranked rows from the SCALE is only half of it. Their own fill is
	// tierVal/scale and pct() clamps UP, so the withheld 2.8e8 came back as the
	// longest bar on the board. An unranked row draws no proportional bar at all.
	bar, ok := jsBlockAfter(js, "function buildYieldBar(d, teamMode, isRanked, scale, since) {")
	if !ok {
		t.Fatal("buildYieldBar not found")
	}
	var width string
	for _, line := range strings.Split(jsStrip(bar), "\n") {
		if strings.Contains(line, "fill.style.width") {
			width = strings.TrimSpace(line)
		}
	}
	if width == "" {
		t.Fatal("no fill.style.width assignment in buildYieldBar")
	}
	if !strings.Contains(width, "isRanked") {
		t.Errorf("an unranked row still draws a proportional bar (%q): pct() clamps to 100%%, "+
			"so a below-floor 2.8e8 renders as the longest bar on the panel — the length the "+
			"KPI tile withholds the number to avoid publishing", width)
	}
}

// --- #605 compare view: a delta derived from an unranked window is withheld ---
//
// The org TIER headline has THREE consumers and #502 fixed one. The other two
// RE-DERIVED the quotient from raw sums instead of reading the struct, so no
// amount of correctness in RollupTeam could reach them. This is the guard on the
// second one (the compare card); the third is FormatReport, guarded in
// internal/scoring.
//
// The rule the card must apply, in the sentence a maintainer cannot misapply:
// ANYTHING DERIVED FROM AN UNRANKED INPUT IS ITSELF UNRANKED.
//
// The guard is possible because renderCompareTotal is written in a SHAPE that
// makes it decidable: every TIER-bearing cell goes through cmpRankedCell, which
// takes its gate as an argument and discards the value when the gate is false. So
// "is this number gated?" is answered by reading four call sites, rather than by
// trying to prove dominance over an arbitrary expression — the analysis that would
// otherwise be needed, and the one an alias or a re-derivation defeats.

// jsSplitArgs splits an argument list on TOP-LEVEL commas, respecting nesting and
// string literals, so `cmpRankedCell('a', f(x, y), z)` yields three arguments and
// not four. ok is false on unbalanced input — never a silent short read, which
// would let a guard assert about arguments it mis-parsed.
func jsSplitArgs(src string) (args []string, ok bool) {
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ',':
			if depth == 0 {
				args = append(args, src[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 || quote != 0 {
		return nil, false
	}
	return append(args, src[start:]), true
}

// jsAssignCount counts ASSIGNMENTS to name — `name =` where the `=` is not part of
// a comparison and `name` is a whole identifier, not a suffix of a longer one.
//
// Both boundary checks are load-bearing rather than tidy. "aRanked" is a SUFFIX of
// "deltaRanked", so without the left-hand check a correct function reports two
// assignments to aRanked and the rule fires on clean code — and the input must NOT
// be whitespace-squashed, because squashing glues `var` onto the name and destroys
// the boundary in the other direction.
func jsAssignCount(code, name string) int {
	n := 0
	for at := 0; at < len(code); {
		i := strings.Index(code[at:], name)
		if i < 0 {
			return n
		}
		start := at + i
		end := start + len(name)
		at = end
		// Whole identifier on both sides, and not a property access (`x.aRanked = 1`
		// writes someone else's field, not this local).
		if start > 0 && (isJSIdentByte(code[start-1]) || code[start-1] == '.') {
			continue
		}
		j := end
		for j < len(code) && (code[j] == ' ' || code[j] == '\t' || code[j] == '\n' || code[j] == '\r') {
			j++
		}
		// Skip the COMPOUND/LOGICAL operator, if any, then require a single '='.
		//
		// 🔴 This loop is why `deltaRanked ||= true;` was measured reporting CLEAN: the
		// old form bailed whenever the byte before '=' was an operator, so every
		// compound assignment read as "not a write" and the gate could be reopened for
		// free. ES2021 logical assignment is available in every browser this asset
		// targets, and `||= true` holds the gate open on every payload.
		opStart := j
		for j < len(code) && strings.IndexByte("|&^+-*/%?", code[j]) >= 0 {
			j++
		}
		if j >= len(code) || code[j] != '=' {
			continue // a read, or a comparison operator — not a write
		}
		if j+1 < len(code) && code[j+1] == '=' {
			continue // ==, ===
		}
		if j == opStart && j > 0 && strings.IndexByte("!<>=", code[j-1]) >= 0 {
			continue // !=, <=, >=, === tail
		}
		n++
	}
	return n
}

// jsBareReassignments returns every bare-identifier assignment in code that is NOT
// a declaration — i.e. not preceded by var/let/const and not a further declarator
// in the same statement (`var a = 1, b = 2;`).
//
// Property writes are skipped: `grid.textContent = ''` and `card.style.display =
// 'none'` are audited by the sink rules, and a name preceded by '.' is someone
// else's field, not a variable this function owns.
func jsBareReassignments(code string) (bad []string) {
	for at := 0; at < len(code); {
		i := strings.IndexByte(code[at:], '=')
		if i < 0 {
			return bad
		}
		eq := at + i
		at = eq + 1
		// Comparisons are not writes.
		if eq+1 < len(code) && code[eq+1] == '=' {
			at = eq + 2
			continue
		}
		if eq > 0 && strings.IndexByte("!<>=", code[eq-1]) >= 0 {
			continue
		}
		// Walk back over the COMPOUND/LOGICAL operator, if any, then over whitespace to
		// the target. `cmpLeak += aT` and `cmpLeak ||= aT` are writes to an undeclared
		// name exactly as `cmpLeak = aT` is — the previous form skipped both, so the
		// cross-function leak this rule exists to close had a compound spelling that
		// walked straight past it.
		j := eq - 1
		for j >= 0 && strings.IndexByte("|&^+-*/%?", code[j]) >= 0 {
			j--
		}
		for j >= 0 && (code[j] == ' ' || code[j] == '\t' || code[j] == '\n' || code[j] == '\r') {
			j--
		}
		end := j + 1
		for j >= 0 && isJSIdentByte(code[j]) {
			j--
		}
		if end == j+1 {
			continue // not an identifier target (a `)` or `]`, e.g. a for-header)
		}
		if j >= 0 && code[j] == '.' {
			continue // property write
		}
		name := code[j+1 : end]
		// Walk back further: a declaration is introduced by var/let/const, or by a
		// comma continuing one.
		for j >= 0 && (code[j] == ' ' || code[j] == '\t' || code[j] == '\n' || code[j] == '\r') {
			j--
		}
		if j >= 0 && code[j] == ',' {
			continue // `var a = 1, b = 2;`
		}
		kwEnd := j + 1
		for j >= 0 && isJSIdentByte(code[j]) {
			j--
		}
		switch code[j+1 : kwEnd] {
		case "var", "let", "const":
			continue
		}
		bad = append(bad, name)
	}
	return bad
}

// jsAssignRHS returns the whitespace-squashed right-hand side of the assignment to
// name, up to the terminating `;`. found is false when there is no assignment —
// never a silent empty string, which would make every "must read" assertion pass
// vacuously.
//
// It takes UNSQUASHED code for the same reason jsAssignCount does: squashing glues
// `var` onto the name and destroys the left-hand word boundary, so `var aRanked =`
// becomes `varaRanked=` and the identifier is unfindable. Only the returned RHS is
// squashed, which is what the callers compare against.
func jsAssignRHS(code, name string) (rhs string, found bool) {
	for at := 0; at < len(code); {
		i := strings.Index(code[at:], name)
		if i < 0 {
			return "", false
		}
		start := at + i
		end := start + len(name)
		at = end
		if start > 0 && (isJSIdentByte(code[start-1]) || code[start-1] == '.') {
			continue // a suffix of a longer identifier, or a property access
		}
		j := end
		for j < len(code) && (code[j] == ' ' || code[j] == '\t' || code[j] == '\n' || code[j] == '\r') {
			j++
		}
		for j < len(code) && strings.IndexByte("|&^+-*/%?", code[j]) >= 0 {
			j++ // compound / logical assignment
		}
		if j >= len(code) || code[j] != '=' {
			continue // a read, not a write
		}
		if j+1 < len(code) && code[j+1] == '=' {
			continue // ==, ===
		}
		rest := code[j+1:]
		if semi := strings.IndexByte(rest, ';'); semi >= 0 {
			rest = rest[:semi]
		}
		return squashSpace(rest), true
	}
	return "", false
}

func isJSIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// cmpSpendCellAllow is the allowlist for the ONE ungated cmpTotalCell call left in
// renderCompareTotal — the Δ AI spend cell.
//
// It is an allowlist rather than a ban on the substring "tier" for the reason the
// #502 round learned the hard way: the branch's own locals are aliases. `aT` holds
// num(total.a.tier) and names no banned string, so `'was ' + aT.toFixed(1)` in the
// spend cell's sub-slot would publish the withheld baseline past every gate on the
// card while a denylist reported clean. Only the measured-spend names are listed,
// so any other local — present or future — is a violation by construction.
var cmpSpendCellAllow = map[string]bool{
	"dCost": true, "signStr": true, "Math": true, "num": true,
	"null": true, "true": true, "false": true, "undefined": true,
}

// cmpSpendPropAllow are the properties that call may name.
var cmpSpendPropAllow = map[string]bool{"abs": true, "toFixed": true}

// cmpTotalRootAllow / cmpTotalPropAllow are the #605 whole-body allowlist for
// renderCompareTotal. It may name its own locals, the payload fields it reads, the
// three cell constructors, and nothing else — so a sink nobody predicted (an
// existing helper like setText, a `title=`, a `dataset.*=`) is a violation by
// construction rather than by having been listed.
var cmpTotalRootAllow = map[string]bool{
	"var": true, "const": true, "let": true,
	"if": true, "else": true, "return": true, "typeof": true,
	"true": true, "false": true, "null": true, "undefined": true,
	// The payload and the two elements it renders into.
	"total": true, "card": true, "grid": true, "$": true,
	// Its own locals: the three gates, the four readings, the delta presentation.
	"aRanked": true, "bRanked": true, "deltaRanked": true,
	"aT": true, "bT": true, "dT": true, "dCost": true,
	"dir": true, "pctChange": true, "sub": true,
	// Formatters and the cell constructors. Nothing else may put anything on screen.
	"num": true, "Math": true, "deltaDir": true, "signStr": true,
	"cmpRankedCell": true, "cmpTotalCell": true,
}

var cmpTotalPropAllow = map[string]bool{
	// The ONLY fields of `total` this function may read. Every other field —
	// present or future — is rejected by omission.
	"a": true, "b": true, "ranked": true, "tier": true,
	"delta_tier": true, "delta_total_cost_usd": true,
	// Formatting and the two DOM mechanics it is allowed.
	"toFixed": true, "abs": true,
	"style": true, "display": true, "textContent": true, "appendChild": true,
}

// cmpAllowViolations2 applies a root/property allowlist to one chunk of source. It
// is the #502 unrankedIdentViolations rule, parameterised so #605 can point it at
// a whole function body as well as at a single argument list.
func cmpAllowViolations2(where, src string, rootAllow, propAllow map[string]bool) (bad []string) {
	code, ok := jsStripStrings(src)
	if !ok {
		return []string{where + " contains an unterminated string literal or block comment, so " +
			"everything after it went unscanned — a guard that cannot read its input must not " +
			"report clean"}
	}
	for _, loc := range jsIdentPathRe.FindAllStringIndex(code, -1) {
		path := code[loc[0]:loc[1]]
		seg := strings.Split(path, ".")
		first := 0
		// A match preceded by '.' continues a member expression whose head was a call —
		// the `.toFixed` in `num(x).toFixed(1)` — so every segment there is a PROPERTY.
		if loc[0] == 0 || code[loc[0]-1] != '.' {
			if !rootAllow[seg[0]] {
				bad = append(bad, where+" references "+path+", which is not on the #605 allowlist. "+
					"It may name only its own locals, the payload fields it reads and the cell "+
					"constructors — any other name is a route onto the page that no per-cell rule "+
					"inspects")
				continue
			}
			first = 1
		}
		for _, p := range seg[first:] {
			if !propAllow[p] {
				bad = append(bad, where+" references "+path+": property "+p+
					" is not on the #605 allowlist")
				break
			}
		}
	}
	return bad
}

// cmpBannedSinkRe is jsBannedSinkRe minus appendChild/append: this function's whole
// job is appending cells to the grid, so those two are legitimate here and the cell
// COUNT rule below is what bounds them.
var cmpBannedSinkRe = regexp.MustCompile(`\b(insertAdjacentHTML|insertAdjacentText|setAttribute|replaceChildren|write)\s*\(`)

// compareTotalViolations runs the #605 rule over a renderCompareTotal source. It
// returns a violation list plus the number of GATED cells it actually parsed, so a
// caller can reject a clean-looking result that came from parsing nothing.
func compareTotalViolations(src string) (bad []string, gated int, ok bool) {
	body, ok := jsBlockAfter(jsStrip(src), "function renderCompareTotal(total) {")
	if !ok {
		return nil, 0, false
	}
	code, stripOK := jsStripStrings(body)
	if !stripOK {
		return []string{"renderCompareTotal contains an unterminated string literal or block " +
			"comment, so everything after it went unscanned — a guard that cannot read its " +
			"input must not report clean"}, 0, true
	}
	squashed := squashSpace(body)

	// (1) SYNTAX THE GUARD CANNOT SEE THROUGH. jsStripStrings blanks a template
	// literal wholesale, so `${aT.toFixed(1)}` scans as an empty string; computed
	// member access hides a field name inside a literal the same way. Both were
	// measured publishing the ratio with every #502 rule silent, so both are refused
	// outright rather than parsed.
	if strings.Contains(code, "`") {
		bad = append(bad, "renderCompareTotal uses a template literal — its ${} interpolations "+
			"are invisible to this guard, so a withheld ratio could be published inside one")
	}
	if strings.Contains(code, "[") {
		bad = append(bad, "renderCompareTotal uses `[`. If that is computed member access it hides "+
			"a field name inside a string literal, which no property check then sees; if it is an "+
			"array or index (building the four cells in a loop, say) it is refused anyway, "+
			"because this guard cannot tell the two apart and the safe answer is the strict one")
	}

	// (1b) THE WHOLE BODY, BY ALLOWLIST. This is the rule that makes the others
	// exhaustive rather than a list of shapes someone thought of.
	//
	// Four measured bypasses walked past every per-call rule because the SINK was not
	// a cell at all: `setText('cmp-note', 'TIER ' + aT.toFixed(1))` (an existing
	// helper writing a real element on this view), `card.title = …`,
	// `grid.dataset.tier = …`, and a compound `cmpLeak += aT`. None names a banned
	// spelling; all of them publish. Enumerating sinks cannot win that race — the
	// function may name ONLY its own locals, the payload fields it reads, and the
	// three cell constructors, so a future sink fails without anyone predicting it.
	bad = append(bad, cmpAllowViolations2("renderCompareTotal", body, cmpTotalRootAllow, cmpTotalPropAllow)...)
	for _, r := range code {
		if r > 0x7F {
			bad = append(bad, "renderCompareTotal contains the non-ASCII character "+
				strconv.QuoteRune(r)+" outside a string literal — jsIdentPathRe is ASCII-only, "+
				"so an identifier this guard cannot even see is refused rather than scanned")
			break
		}
	}

	// (2) THE THREE GATES, read from their assignments. Each must be derived from the
	// field it claims to read, and there must be exactly one of each — a second
	// assignment could relax a gate after the first set it.
	// STRUCTURAL, not byte-exact. An earlier version pinned the three assignments
	// verbatim, which cried wolf on no-ops — rewriting `!!total.ranked` as
	// `Boolean(total.ranked)`, or `var` as `const`, failed with a message accusing the
	// author of client-side re-derivation — while catching no mutant the assignment
	// count and the field checks below did not already catch. What matters is WHICH
	// FIELD each gate reads, and (for the delta) that it reads one field rather than
	// rebuilding the conjunction.
	for _, g := range []struct {
		name, mustRead, why string
		mustNotRead         []string
	}{
		{
			name: "aRanked", mustRead: "total.a.ranked",
			why: "the baseline headline must be gated on the BASELINE side's own verdict, " +
				"exactly as the KPI tile is",
		},
		{
			name: "bRanked", mustRead: "total.b.ranked",
			why: "same, for the selected side — a ranked side keeps its honestly-earned " +
				"headline, which is what separates option B from option D",
		},
		{
			name: "deltaRanked", mustRead: "total.ranked",
			mustNotRead: []string{"total.a", "total.b", "&&", "||"},
			why: "the delta gate must READ the server-derived conjunction (compare.go's " +
				"teamDeltaJSON.Ranked), not recompute it. A client-side re-derivation is " +
				"option C by another name: it forfeits the propagation that let one field " +
				"fix three consumers",
		},
	} {
		rhs, found := jsAssignRHS(code, g.name)
		if !found {
			bad = append(bad, "renderCompareTotal never assigns `"+g.name+"` — "+g.why)
			continue
		}
		if !strings.Contains(rhs, g.mustRead) {
			bad = append(bad, "the `"+g.name+"` gate is `"+rhs+"`, which never reads `"+
				g.mustRead+"` — "+g.why)
		}
		for _, banned := range g.mustNotRead {
			if strings.Contains(rhs, banned) {
				bad = append(bad, "the `"+g.name+"` gate is `"+rhs+"`, which uses `"+banned+
					"` — "+g.why)
			}
		}
		// It must also fail CLOSED: a truthy coercion, not a raw value that could be
		// undefined. `!!x` and `Boolean(x)` both qualify; a bare `total.ranked` does not,
		// because `undefined` from a pre-#605 server is falsy today but a `ranked: null`
		// or a future string field would not be.
		if !strings.HasPrefix(rhs, "!!") && !strings.HasPrefix(rhs, "Boolean(") {
			bad = append(bad, "the `"+g.name+"` gate is `"+rhs+"`, which does not coerce to a "+
				"boolean; it must fail CLOSED so a server that does not say never publishes")
		}
	}
	// …and each gate is assigned EXACTLY ONCE. Pinning the `var` form alone is not
	// enough: a later bare reassignment (`deltaRanked = !!(total.a.ranked &&
	// total.b.ranked);`, or `deltaRanked = !!total.significant;`) leaves the pinned
	// line intact and silently relaxes the gate. Both were measured passing the rule
	// above. A gate that can be reopened after it is set is not a gate.
	for _, name := range []string{"aRanked", "bRanked", "deltaRanked"} {
		if n := jsAssignCount(code, name); n != 1 {
			bad = append(bad, "renderCompareTotal assigns `"+name+"` "+strconv.Itoa(n)+
				" times, want exactly 1 — a second assignment reopens a gate the first one "+
				"closed, and the pinned declaration above still reads correctly")
		}
	}

	// (3) EVERY TIER-BEARING CELL GOES THROUGH THE GATE, and the label→gate mapping
	// is pinned. Gating Δ Org TIER on aRanked alone would publish the reconstruction
	// this issue exists to prevent, and would look correct at a glance.
	// The VALUE is pinned too, not just the label and the gate. Measured bypass: leave
	// every gate correct and swap the two values —
	//   cmpRankedCell('Org TIER — baseline', aRanked, bT.toFixed(1), null, null)
	// — which publishes the unranked side's reading under the ranked side's verdict,
	// the exact failure the "gated on the wrong sides" fixture was written for,
	// reached by mutating the value instead of the gate. Reported CLEAN.
	// ALL FIVE arguments, per cell. Pinning the label and the gate alone left the
	// value free; pinning the value too left the SUB slot free, and
	//   cmpRankedCell('Org TIER — baseline', aRanked, aT.toFixed(1), 'sel ' + bT.toFixed(1), null)
	// publishes the unranked side under the ranked side's verdict with every other
	// rule satisfied. Measured CLEAN at each intermediate stage — which is the lesson:
	// a call whose arguments are only partly audited is a call that is not audited.
	wantGated := []struct{ label, flag, value, sub, dir string }{
		{"'Org TIER — baseline'", "aRanked", "aT.toFixed(1)", "null", "null"},
		{"'Org TIER — selected'", "bRanked", "bT.toFixed(1)", "null", "null"},
		{"'Δ Org TIER'", "deltaRanked", "signStr(dT)+Math.abs(dT).toFixed(1)", "sub", "dir"},
	}
	calls, callsOK := jsCallArgs(body, "cmpRankedCell(")
	if !callsOK {
		return nil, 0, false // an unterminated call is a short read, not a clean file
	}
	if len(calls) != len(wantGated) {
		bad = append(bad, "renderCompareTotal makes "+strconv.Itoa(len(calls))+
			" cmpRankedCell calls, want "+strconv.Itoa(len(wantGated))+
			" — the three TIER-bearing cells (both org headlines and the delta) are the "+
			"gated ones, and a cell that stops going through the gate stops being gated")
	}
	for i, call := range calls {
		args, argsOK := jsSplitArgs(call)
		if !argsOK || len(args) != 5 {
			bad = append(bad, "cmpRankedCell call "+strconv.Itoa(i)+" does not parse as five "+
				"arguments (label, ranked, value, sub, dir): "+call)
			continue
		}
		gated++
		if i >= len(wantGated) {
			continue
		}
		if got := strings.TrimSpace(args[0]); got != wantGated[i].label {
			bad = append(bad, "cmpRankedCell call "+strconv.Itoa(i)+" labels the cell "+got+
				", want "+wantGated[i].label+" — the cell order fixes the grid tracks, and the "+
				"label→gate mapping below is read positionally")
		}
		if got := strings.TrimSpace(args[1]); got != wantGated[i].flag {
			bad = append(bad, "the "+wantGated[i].label+" cell is gated on `"+got+"`, want `"+
				wantGated[i].flag+"`. Gating a derived delta on one side's verdict republishes "+
				"the other side's withheld number as `selected = baseline + Δ`")
		}
		for _, a := range []struct{ slot, got, want string }{
			{"value", squashSpace(args[2]), wantGated[i].value},
			{"sub", squashSpace(args[3]), wantGated[i].sub},
			{"dir", squashSpace(args[4]), wantGated[i].dir},
		} {
			if a.got != a.want {
				bad = append(bad, "the "+wantGated[i].label+" cell's "+a.slot+" slot is `"+a.got+
					"`, want `"+a.want+"`. A correct gate around the WRONG expression publishes "+
					"the unranked side's reading under the ranked side's verdict")
			}
		}
	}

	// (4) THE ONE UNGATED CELL, argument by argument. Δ AI spend is a measured input
	// and stays on screen (#502 criterion 4) — but it is also the only place left
	// where a number can reach the card without passing a gate, so its arguments are
	// held to an allowlist.
	spend, spendOK := jsCallArgs(body, "cmpTotalCell(")
	if !spendOK {
		return nil, 0, false
	}
	if len(spend) != 1 {
		bad = append(bad, "renderCompareTotal makes "+strconv.Itoa(len(spend))+
			" direct cmpTotalCell calls, want exactly 1 (the ungated Δ AI spend cell) — "+
			"every other cell must route through cmpRankedCell")
	}
	for _, arg := range spend {
		bad = append(bad, cmpAllowViolations("the ungated Δ AI spend cell", arg)...)
	}

	// (5) FOUR CELLS, ALWAYS. The card's shape is what got option D rejected: four
	// labelled cells in fixed grid tracks, so toggling windows never reflows the
	// section below. It is also a publication bound — a fifth appendChild is a fifth
	// route onto the card that none of the rules above inspect.
	if n := strings.Count(squashed, "grid.appendChild("); n != 4 {
		bad = append(bad, "renderCompareTotal appends "+strconv.Itoa(n)+" cells, want exactly 4 "+
			"— the card has two states and one shape, and an extra append is an uninspected "+
			"route onto it")
	}

	// (5b) …and the CELLS are the only things appended. Measured bypass: keeping the
	// four grid.appendChild calls intact and hanging an extra span off the ungated
	// spend CELL —
	//
	//   var spendCell = cmpTotalCell('Δ AI spend', …);
	//   spendCell.appendChild(el('span', 'cmp-total-sub', 'TIER ' + aT.toFixed(1)));
	//   grid.appendChild(spendCell);
	//
	// — satisfied every rule above (four grid appends, three gated cells, one clean
	// cmpTotalCell argument list) and published the withheld baseline. So the ONLY
	// appendChild permitted here is onto the grid, and renderCompareTotal may not
	// build DOM at all: cells come from the three audited constructors.
	if n, g := strings.Count(squashed, "appendChild("), strings.Count(squashed, "grid.appendChild("); n != g {
		bad = append(bad, "renderCompareTotal makes "+strconv.Itoa(n-g)+" appendChild call(s) onto "+
			"something other than the grid — an extra node hung off a cell is a route onto the "+
			"card that no cell rule inspects")
	}
	if strings.Contains(code, "el(") {
		bad = append(bad, "renderCompareTotal builds DOM directly with el(); every cell must come "+
			"from cmpRankedCell or the one audited cmpTotalCell call, or the gate can be "+
			"stepped around entirely")
	}

	// (5c) NO BARE REASSIGNMENT. Measured bypass: stashing the withheld reading in a
	// module-level variable (`cmpLeak = aT;`) that cmpTotalCell then published. The
	// leak crosses a function boundary, so no rule scoped to this body could see the
	// sink — but the ASSIGNMENT is here, and every legitimate assignment in this
	// function is a declaration.
	for _, name := range jsBareReassignments(code) {
		bad = append(bad, "renderCompareTotal assigns to `"+name+"` without declaring it — a write "+
			"to an outer variable carries a withheld value out of this function, where nothing "+
			"audits what reads it")
	}

	// (6) ONE TEXT SINK, AND IT BLANKS. Any other direct write to a node's text is a
	// second way onto the card that bypasses every cell rule above. Counted on the
	// whitespace-squashed body and matched on `.prop` alone, because a line break
	// between the node and the property defeated the previous spelling of this rule.
	//
	// EXACT match, never HasPrefix. `grid.textContent = '' + aT.toFixed(1);` squashes
	// to `.textContent=''+aT.toFixed(1)`, which HasPrefix accepted — and it publishes:
	// textContent replaces the grid's children with a text node and the four
	// appendChilds then land after it. Measured CLEAN under the prefix form.
	for _, m := range jsTextSinkRe.FindAllString(squashed, -1) {
		if m == ".textContent=''" {
			continue
		}
		bad = append(bad, "renderCompareTotal writes "+m+
			"; the only permitted direct text write is blanking the grid, and it must be "+
			"exactly that — `textContent = '' + <expr>` is a publication, not a blanking")
	}
	if m := cmpBannedSinkRe.FindString(code); m != "" {
		bad = append(bad, "renderCompareTotal calls "+strings.TrimSuffix(m, "(")+
			" — the card may be reached only through the audited cell constructors")
	}
	return bad, gated, true
}

// cmpAllowViolations applies the measured-spend allowlist to one argument list.
func cmpAllowViolations(where, src string) (bad []string) {
	code, ok := jsStripStrings(src)
	if !ok {
		return []string{where + " contains an unterminated string literal, so everything after " +
			"it went unscanned"}
	}
	for _, op := range []string{"/", "**"} {
		if strings.Contains(code, op) {
			bad = append(bad, where+" uses `"+op+"` — the two operators that rebuild a ratio "+
				"from the inputs the card is allowed to show")
		}
	}
	for _, loc := range jsIdentPathRe.FindAllStringIndex(code, -1) {
		path := code[loc[0]:loc[1]]
		seg := strings.Split(path, ".")
		first := 0
		// A match preceded by '.' continues a member expression whose head was a call
		// — the `.toFixed` in `num(x).toFixed(1)` — so every segment is a PROPERTY.
		if loc[0] == 0 || code[loc[0]-1] != '.' {
			if !cmpSpendCellAllow[seg[0]] {
				bad = append(bad, where+" references "+path+", which is not on the #605 "+
					"allowlist — it may name only the measured spend and its formatters, so a "+
					"local aliasing a withheld TIER cannot reach the one ungated cell")
				continue
			}
			first = 1
		}
		for _, p := range seg[first:] {
			if !cmpSpendPropAllow[p] {
				bad = append(bad, where+" references "+path+": property "+p+
					" is not on the #605 allowlist")
				break
			}
		}
	}
	return bad
}

// TestDashboard_CompareTotalSuppressesUnrankedRatio is the #605 guard on the real
// asset. Δ Org TIER and its % change are withheld when EITHER window is unranked,
// each org headline is withheld under its own side's verdict, and the one ungated
// cell is the measured spend.
func TestDashboard_CompareTotalSuppressesUnrankedRatio(t *testing.T) {
	_, js := get(t, jsPath)

	bad, gated, ok := compareTotalViolations(js)
	if !ok {
		t.Fatal("the #605 guard could not read renderCompareTotal: either the function is gone, " +
			"or a call inside it is unterminated. Both are hard failures — a guard that parses " +
			"nothing reports clean.")
	}
	if gated != 3 {
		t.Fatalf("parsed %d gated cells, want 3 — the argument walker found nothing and every "+
			"assertion below came from the whole-function rules alone", gated)
	}
	for _, v := range bad {
		t.Error(v)
	}

	// POSITIVE CONTROL on the walkers themselves: the ranked path must still publish
	// everything. If jsBlockAfter/jsCallArgs ever stopped finding anything, this
	// fails instead of the suppression checks passing vacuously.
	body, ok := jsBlockAfter(js, "function renderCompareTotal(total) {")
	if !ok {
		t.Fatal("renderCompareTotal not found")
	}
	for _, marker := range []string{
		"aT.toFixed(1)", "bT.toFixed(1)", // the two org headlines
		"signStr(dT) + Math.abs(dT).toFixed(1)", // the delta
		"pctChange",                             // the % change
		"'Δ AI spend'",                          // the measured input, never withheld
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("renderCompareTotal no longer computes %q — #605 WITHHOLDS the reading "+
				"below the floor, it does not delete the card", marker)
		}
	}
}

// TestDashboard_CompareGateCannotBeSeparatedFromTheRendering pins the two helpers
// the guard above rests on.
//
// compareTotalViolations proves each cell is handed the right gate. That is worth
// nothing if cmpRankedCell ignores it, or if the withheld cell it falls back to
// quietly renders a number. Both are two-line functions and both are pinned
// verbatim, because both are one edit away from making every other #605 assertion
// vacuous.
func TestDashboard_CompareGateCannotBeSeparatedFromTheRendering(t *testing.T) {
	_, js := get(t, jsPath)

	gate, ok := jsBlockAfter(js, "function cmpRankedCell(label, ranked, value, sub, dir) {")
	if !ok {
		t.Fatal("cmpRankedCell not found — the #605 gate cannot be scoped")
	}
	// Whitespace-squashed: the assertion is about the RULE (an unranked cell returns
	// the withheld cell and DISCARDS value/sub/dir), not about one formatting of it.
	const wantGate = "if(!ranked){returncmpWithheldCell(label);}returncmpTotalCell(label,value,sub,dir);"
	if got := squashSpace(jsStrip(gate)); got != wantGate {
		t.Errorf("cmpRankedCell is not the two-statement gate #605 rests on.\n  got:  %s\n  want: %s\n"+
			"Every other guard in this file assumes a false `ranked` DISCARDS value, sub and "+
			"dir; if it does not, they all pass while the card publishes.", got, wantGate)
	}

	withheld, ok := jsBlockAfter(js, "function cmpWithheldCell(label) {")
	if !ok {
		t.Fatal("cmpWithheldCell not found")
	}
	wsq := squashSpace(jsStrip(withheld))
	// 🔴 An explicit em dash plus a stated reason, NEVER a blank. All three
	// specialists raised this independently: a visible reason preserves the floor's
	// credibility; a blank trains readers to treat a withheld number as a bug.
	if !strings.Contains(wsq, "cmpTotalCell(label,'—',BELOW_FLOOR_TAG,null)") {
		t.Errorf("the withheld cell is not `cmpTotalCell(label, '—', BELOW_FLOOR_TAG, null)`: %s\n"+
			"Three things are pinned by that one call — the em dash (never a blank), the "+
			"reason beside it, and the NULL direction: a withheld delta must not render as a "+
			"confident green or red claim about a number we declined to publish.", wsq)
	}
	// The gate function is the only caller that may reach the withheld cell with
	// anything, and it reaches it with a label. Arity one is what makes that
	// structural: there is no parameter through which a value could arrive.
	if !strings.Contains(js, "function cmpWithheldCell(label) {") {
		t.Error("cmpWithheldCell takes more than a label; the withheld state must have no " +
			"value parameter at all, so no caller can smuggle a number into it")
	}
	// Criterion 6: ONE vocabulary. The tag is the shared constant, not a second
	// phrase minted for this card.
	if !strings.Contains(withheld, "BELOW_FLOOR_TAG") {
		t.Error("the withheld cell does not use BELOW_FLOOR_TAG; compare must name the verdict " +
			"in the words buildDevDumbbell already uses, not mint a second phrase for it")
	}
	if !strings.Contains(js, "var BELOW_FLOOR_TAG = 'below ranking floor';") {
		t.Error("BELOW_FLOOR_TAG is no longer the compare view's existing copy verbatim")
	}
	dev, ok := jsBlockAfter(js, "function buildDevDumbbell(row, scale) {")
	if !ok {
		t.Fatal("buildDevDumbbell not found")
	}
	if !strings.Contains(dev, "BELOW_FLOOR_TAG") {
		t.Error("buildDevDumbbell no longer shares BELOW_FLOOR_TAG with the withheld org cells, " +
			"so the two can drift into naming one verdict two ways")
	}

	// Criterion 7: the state reaches assistive tech as TEXT. An aria-label would be
	// INERT here — a .cmp-total-cell is a bare <div>, i.e. role="generic", and the
	// accessible-name computation refuses to name a generic element, so the label
	// would be computed and then discarded. Measured on the #603 team rows.
	if strings.Contains(withheld, "aria-label") {
		t.Error("the withheld cell carries an aria-label on a role=\"generic\" div, which the " +
			"accessible-name computation discards — use the off-screen text technique the " +
			"team rows use (#274/#603)")
	}
	// The span must exist AND carry text. `el('span', 'cmp-total-sr', '')` satisfies a
	// presence check and puts nothing in the accessibility tree — the empty-string
	// reveal defect this file already guards elsewhere, in its assistive form.
	srArgs, srOK := jsCallArgs(withheld, "el('span', 'cmp-total-sr',")
	if !srOK || len(srArgs) != 1 {
		t.Fatalf("the withheld cell states its verdict in no assistive channel (parsed %d "+
			"cmp-total-sr writes): without off-screen text the muted sub-line is the only "+
			"signal, and that is colour and size (WCAG 2.1 SC 1.4.1)", len(srArgs))
	}
	if srText := strings.TrimSpace(squashSpace(srArgs[0])); srText == "''" || srText == `""` || srText == "" {
		t.Errorf("the withheld cell's off-screen text is empty (%q) — the span is in the DOM "+
			"and says nothing, which is indistinguishable to a screen reader from not being "+
			"there at all", srText)
	}
	if !strings.Contains(srArgs[0], "BELOW_FLOOR_TAG") {
		t.Error("the withheld cell's off-screen text does not name the verdict in the shared " +
			"vocabulary; a screen-reader user must get the same reason a sighted one does")
	}
	style := servedStyle(t)
	if !regexp.MustCompile(`(?s)\.cmp-total-sr\s*\{[^}]*clip-path`).MatchString(style) {
		t.Error(".cmp-total-sr has no clip-path rule — the screen-reader text is either visible " +
			"or missing its styling entirely")
	}
	if regexp.MustCompile(`(?s)\.cmp-total-sr\s*\{[^}]*display\s*:\s*none`).MatchString(style) {
		t.Error(".cmp-total-sr uses display:none, which removes the text from the accessibility " +
			"tree — the one place it is meant to exist")
	}
	// position:absolute is what takes the span OUT of the cell's flex column. Without
	// it the clipped 1x1 box still occupies a row, so the withheld cells grow a fourth
	// line the ranked ones do not have and the grid stops being layout-stable —
	// criterion 8, defeated by a CSS property rather than by any JS.
	if !regexp.MustCompile(`(?s)\.cmp-total-sr\s*\{[^}]*position\s*:\s*absolute`).MatchString(style) {
		t.Error(".cmp-total-sr is not position:absolute, so the off-screen span stays in the " +
			"cell's flex column and adds a line to every withheld cell — the card's shape then " +
			"changes between its two states, which is what got option D rejected")
	}

	// CONTAINMENT: the three cell constructors may be called only from the three
	// functions audited above. A fourth call site anywhere in the file is a route
	// onto the card that compareTotalViolations never reads.
	for _, c := range []struct {
		call string
		want int
		why  string
	}{
		{"cmpRankedCell(", 3, "the three gated cells in renderCompareTotal"},
		{"cmpWithheldCell(", 1, "the single fallback inside cmpRankedCell"},
		{"cmpTotalCell(", 3, "the ungated spend cell, the gate's ranked arm, and the withheld cell"},
	} {
		// Count CALLS, not the definition: `function cmpRankedCell(` also contains the
		// call text, so the definition line is excluded by requiring no preceding
		// "function ".
		n := strings.Count(jsStrip(js), c.call) - strings.Count(jsStrip(js), "function "+c.call)
		if n != c.want {
			t.Errorf("%s is called %d times, want %d (%s). A call site outside the three "+
				"audited functions is a route onto the card that no #605 rule inspects",
				strings.TrimSuffix(c.call, "("), n, c.want, c.why)
		}
	}

	// …and the constructors are the only way to BUILD the card, not merely the only
	// blessed way. Measured bypass: a second renderer, called from renderCompare
	// beside renderCompareTotal, that assembled a cell out of el() and appended it
	// straight to $('cmp-total-grid'). It used none of the three constructors, so
	// every count above was satisfied and the card gained a fifth cell publishing
	// the withheld baseline.
	//
	// Pinning each of the card's id/class literals to ONE occurrence closes that:
	// the grid and the card can be reached from exactly one place, and a cell's
	// markup exists in exactly one place. A future renderer must go through the
	// audited constructors — where the gate is — or fail here.
	for _, lit := range []struct {
		text string
		home string
	}{
		{"$('cmp-total-grid')", "renderCompareTotal"},
		{"$('cmp-total')", "renderCompareTotal"},
		{"'cmp-total-cell'", "cmpTotalCell"},
		{"'cmp-total-label'", "cmpTotalCell"},
		{"'cmp-total-value num'", "cmpTotalCell"},
		{"'cmp-total-sub num'", "cmpTotalCell"},
		{"'cmp-total-sr'", "cmpWithheldCell"},
	} {
		if n := strings.Count(jsStrip(js), lit.text); n != 1 {
			t.Errorf("%s appears %d times in dashboard.js, want exactly 1 (in %s). A second "+
				"reference is a second way to build or reach the org delta card, and the #605 "+
				"gate lives in only one of them", lit.text, n, lit.home)
		}
	}
}

// TestDashboard_CompareGuardCatchesTheDefect is the control arm. It runs the same
// guard over renderCompareTotal sources that CONTAIN the defect and fails if the
// guard reports clean. Without it, the test above cannot tell "the ratio is
// suppressed" from "the guard stopped looking".
//
// The fixtures below are written the way the #502 round's were: after the obvious
// mutants, each one is an attempt to publish the withheld number while keeping the
// guard green — aliases, re-derivation, a wrong gate, a template literal, computed
// access, an extra cell, an unaudited text sink, ASI.
func TestDashboard_CompareGuardCatchesTheDefect(t *testing.T) {
	const head = "function renderCompareTotal(total) {\n" +
		"  var card = $('cmp-total');\n" +
		"  var grid = $('cmp-total-grid');\n" +
		"  grid.textContent = '';\n" +
		"  if (!total) { card.style.display = 'none'; return; }\n" +
		"  card.style.display = 'block';\n" +
		"  var aRanked = !!(total.a && total.a.ranked);\n" +
		"  var bRanked = !!(total.b && total.b.ranked);\n" +
		"  var deltaRanked = !!total.ranked;\n" +
		"  var aT = num(total.a && total.a.tier), bT = num(total.b && total.b.tier);\n" +
		"  var dT = num(total.delta_tier);\n" +
		"  var dir = deltaDir(dT);\n" +
		"  var pctChange = (aT !== 0) ? (dT / aT) * 100 : null;\n" +
		"  var sub = (pctChange === null) ? 'no baseline yield' : (signStr(pctChange) + '%');\n"
	const spendCell = "  var dCost = num(total.delta_total_cost_usd);\n" +
		"  grid.appendChild(cmpTotalCell('Δ AI spend', signStr(dCost) + '$' + Math.abs(dCost).toFixed(2), null, null));\n" +
		"}\n"
	const goodCells = "  grid.appendChild(cmpRankedCell('Org TIER — baseline', aRanked, aT.toFixed(1), null, null));\n" +
		"  grid.appendChild(cmpRankedCell('Org TIER — selected', bRanked, bT.toFixed(1), null, null));\n" +
		"  grid.appendChild(cmpRankedCell('Δ Org TIER', deltaRanked, signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));\n"

	mustCatch := []struct{ name, cells, spend string }{
		{
			// THE defect the issue was filed over: the card re-derives its reading
			// from the sides and never consults a verdict at all.
			name: "no gate: every cell published unconditionally",
			cells: "  grid.appendChild(cmpTotalCell('Org TIER — baseline', aT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpTotalCell('Org TIER — selected', bT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpTotalCell('Δ Org TIER', signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));\n",
		},
		{
			// Option C, the one all three specialists rejected outright. It looks
			// careful and it is the WORST case: a ranked baseline beside an unranked
			// selected window is exactly where `selected = baseline + Δ` reconstructs.
			name: "delta gated on the baseline side only (option C)",
			cells: "  grid.appendChild(cmpRankedCell('Org TIER — baseline', aRanked, aT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpRankedCell('Org TIER — selected', bRanked, bT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpRankedCell('Δ Org TIER', aRanked, signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));\n",
		},
		{
			// An OR instead of an AND: one ranked side is enough to publish. Renders
			// identically on the two symmetric fixtures and leaks on the mixed one.
			name: "delta gated on a client-side OR of the two sides",
			cells: "  var either = aRanked || bRanked;\n" + goodCells[:strings.Index(goodCells, "  grid.appendChild(cmpRankedCell('Δ Org TIER'")] +
				"  grid.appendChild(cmpRankedCell('Δ Org TIER', either, signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));\n",
		},
		{
			// The ruling's criterion 1, one re-derivation away: the AND recomputed on
			// the client. Renders correctly TODAY and forfeits the propagation, so the
			// next consumer guesses again.
			name:  "delta gate re-derived client-side instead of read",
			cells: "  deltaRanked = !!(total.a.ranked && total.b.ranked);\n" + goodCells,
		},
		{
			// ALIAS. The gate is intact and the value is smuggled into the ONE ungated
			// cell's sub-slot through a local that names nothing banned. A denylist of
			// spellings cannot see this; only the allowlist can.
			name:  "withheld baseline laundered into the spend cell through an alias",
			cells: goodCells + "  var shadow = aT;\n",
		},
		{
			// RE-DERIVATION into the ungated cell: the ratio rebuilt from two values
			// the card legitimately holds, banned identifier never appearing.
			name:  "ratio re-derived inside the ungated spend cell",
			cells: goodCells,
		},
		{
			// A FIFTH cell, publishing the withheld reading beside the four audited
			// ones. Every rule about the four is satisfied.
			name:  "an extra fifth cell publishes the withheld reading",
			cells: goodCells + "  grid.appendChild(cmpTotalCell('computed', aT.toFixed(1), null, null));\n",
		},
		{
			// TEMPLATE LITERAL: jsStripStrings blanks it wholesale, so the interpolation is
			// invisible. Measured green in the #502 round.
			//
			// The leak sits INSIDE the ungated spend cell's own argument list, keeping the
			// cell count at four — deliberately. An earlier version of this fixture added a
			// fifth cell, so the count rule killed it and the template ban had no fixture of
			// its own: the ban could have been deleted with the suite green. Measured: this
			// shape produces exactly one violation, from the template ban alone.
			name:  "withheld reading interpolated into a template literal",
			cells: goodCells,
			spend: "  var dCost = num(total.delta_total_cost_usd);\n" +
				"  grid.appendChild(cmpTotalCell('Δ AI spend', `${aT.toFixed(1)} was the baseline`, null, null));\n}\n",
		},
		{
			// COMPUTED MEMBER ACCESS hides the PROPERTY NAME inside a string literal, which
			// jsStripStrings then blanks — so the allowlist never sees it. Written as
			// `card.title = …` this is caught by the property allowlist (`title` is not on
			// it); written as `card['title'] = …` every visible name is allowlisted, the
			// walk-back in jsBareReassignments stops at a `]` rather than an identifier,
			// and only the `[` ban is left.
			//
			// The first draft of this fixture put `total.a['tier']` in the spend cell,
			// where the spend allowlist rejects `total` outright — so the `[` ban could be
			// deleted with this fixture still green. Measured, and the reason the shape
			// changed.
			name:  "withheld reading written through computed member access",
			cells: goodCells + "  card['title'] = 'TIER ' + aT.toFixed(1);\n",
		},
		{
			// A SECOND TEXT SINK, bypassing the cell constructors entirely. No trailing
			// semicolon: ASI makes that legal, and a sink rule requiring `;` matched
			// nothing there.
			name:  "withheld reading written straight to textContent (ASI, no semicolon)",
			cells: goodCells + "  card.textContent = 'TIER ' + aT.toFixed(1)\n",
		},
		{
			// The same sink split across a line break — the form that matched NOTHING
			// before the whitespace squash.
			name:  "unaudited text write split across a line break",
			cells: goodCells + "  card\n    .textContent = 'TIER ' + aT.toFixed(1);\n",
		},
		{
			// insertAdjacentHTML: a DOM sink that is not an assignment.
			name:  "withheld reading inserted through insertAdjacentHTML",
			cells: goodCells + "  grid.insertAdjacentHTML('beforeend', 'TIER ' + aT.toFixed(1));\n",
		},
		{
			// NON-ASCII IDENTIFIER. jsIdentPathRe is ASCII-only, but JavaScript accepts any
			// ID_Start character, so `τ` scans as nothing at all. Four cells, so the
			// non-ASCII ban is the only rule that can kill it.
			name:  "withheld reading laundered through a non-ASCII identifier",
			cells: goodCells + "  var τ = aT;\n",
			spend: "  var dCost = num(total.delta_total_cost_usd);\n" +
				"  grid.appendChild(cmpTotalCell('Δ AI spend', τ.toFixed(1), null, null));\n}\n",
		},
		{
			// The gate flag replaced by a constant. Every call still goes through
			// cmpRankedCell and every label is right.
			name: "gate replaced by a literal true",
			cells: "  grid.appendChild(cmpRankedCell('Org TIER — baseline', aRanked, aT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpRankedCell('Org TIER — selected', bRanked, bT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpRankedCell('Δ Org TIER', true, signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));\n",
		},
		{
			// The gate reads a field that is not the verdict — `significant` is always
			// false for an aggregate, so this LOOKS conservative and is unrelated.
			name:  "gate reads a different field than the verdict",
			cells: "  deltaRanked = !!total.significant;\n" + goodCells,
		},
		{
			// MEASURED BYPASS of the first version of this guard. The four grid appends
			// are intact, the three gated cells are intact, and the one ungated
			// cmpTotalCell argument list is clean — the extra node is hung off the
			// spend CELL instead of the grid. It published the withheld baseline with
			// the whole suite green.
			name: "extra span appended to the spend cell rather than the grid",
			cells: goodCells + "  var spendCell = cmpTotalCell('x', '1', null, null);\n" +
				"  spendCell.appendChild(el('span', 'cmp-total-sub', 'TIER ' + aT.toFixed(1)));\n" +
				"  grid.appendChild(spendCell);\n",
		},
		{
			// MEASURED BYPASS. The value is stashed in a module-level variable and
			// published by cmpTotalCell, so the SINK is outside this function entirely
			// and no rule scoped to the body could ever see it. What is inside the body
			// is the bare assignment — which is why every assignment here must be a
			// declaration.
			name:  "withheld value stashed in an outer variable for another function to publish",
			cells: goodCells + "  cmpLeak = aT;\n",
		},
		{
			// The baseline headline gated on the OTHER side's verdict. Symmetric
			// fixtures hide it; the mixed one publishes an unranked headline.
			name: "org headlines gated on the wrong sides",
			cells: "  grid.appendChild(cmpRankedCell('Org TIER — baseline', bRanked, aT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpRankedCell('Org TIER — selected', aRanked, bT.toFixed(1), null, null));\n" +
				"  grid.appendChild(cmpRankedCell('Δ Org TIER', deltaRanked, signStr(dT) + Math.abs(dT).toFixed(1), sub, dir));\n",
		},
	}
	// The re-derivation fixture needs a modified spend cell, so it is built here
	// rather than inline above.
	const rederivedSpend = "  var dCost = num(total.delta_total_cost_usd);\n" +
		"  grid.appendChild(cmpTotalCell('Δ AI spend', signStr(dCost) + ' yield ' + (num(total.weighted_points) / (num(total.total_cost_usd) / 1000)).toFixed(1), null, null));\n}\n"
	const aliasedSpend = "  var dCost = num(total.delta_total_cost_usd);\n" +
		"  grid.appendChild(cmpTotalCell('Δ AI spend', signStr(dCost) + ' was ' + shadow.toFixed(1), null, null));\n}\n"

	for _, tt := range mustCatch {
		t.Run(tt.name, func(t *testing.T) {
			tail := spendCell
			switch {
			case tt.spend != "":
				tail = tt.spend
			case tt.name == "ratio re-derived inside the ungated spend cell":
				tail = rederivedSpend
			case tt.name == "withheld baseline laundered into the spend cell through an alias":
				tail = aliasedSpend
			}
			src := head + tt.cells + tail
			bad, _, ok := compareTotalViolations(src)
			if !ok {
				t.Fatal("guard failed to parse a fixture that contains the function")
			}
			if len(bad) == 0 {
				t.Error("guard reported CLEAN on a renderCompareTotal that still publishes a " +
					"reading derived from an unranked window")
			}
		})
	}

	t.Run("correct suppression is clean", func(t *testing.T) {
		bad, gated, ok := compareTotalViolations(head + goodCells + spendCell)
		if !ok {
			t.Fatal("guard failed to parse the correct fixture")
		}
		if gated != 3 {
			t.Errorf("guard parsed %d gated cells, want 3", gated)
		}
		if len(bad) != 0 {
			t.Errorf("guard fired on a correctly gated card: %v", bad)
		}
	})

	// An unterminated literal must FAIL, not read clean: everything after the
	// unbalanced quote is discarded, so a guard that shrugged would report clean on
	// source it never scanned. Every identifier BEFORE the quote is legal, and the
	// re-derivation sits after it, so nothing else can trip.
	t.Run("an unterminated string literal is a failure", func(t *testing.T) {
		src := head + goodCells + "  var x = 'unterminated;\n" +
			"  grid.appendChild(cmpTotalCell('note', (aT / bT).toFixed(1), null, null));\n" + spendCell
		bad, _, ok := compareTotalViolations(src)
		if ok && len(bad) == 0 {
			t.Error("guard reported CLEAN on a function it could not finish reading")
		}
	})

	// A comment quoting the defect must not fail the build (jsStrip's job).
	t.Run("the defect quoted in a comment is ignored", func(t *testing.T) {
		src := head +
			"  // never cmpTotalCell('Δ Org TIER', signStr(dT) + '', sub, dir) here (that is #605)\n" +
			goodCells + spendCell
		if bad, _, _ := compareTotalViolations(src); len(bad) != 0 {
			t.Errorf("guard fired on a comment: %v", bad)
		}
	})
}

// compareScaleViolations and dumbbellPlotViolations hold the #605 RENDERING-
// INTEGRITY rules, factored out of their test so a control arm can run them over
// sources that CONTAIN the defect. They were not factored at first, and it cost:
// two complete reverts of this half were measured passing the whole suite, because
// a rule with no mutant fixture is a rule nobody has ever seen fail.

// compareScaleViolations: the shared dumbbell denominator must be computed over
// RANKED readings only, on BOTH sides of BOTH the rows and the org total.
func compareScaleViolations(js string) (bad []string) {
	scale, ok := jsBlockAfter(js, "function compareScale(rows, total) {")
	if !ok {
		return []string{"compareScale not found — the #605 scale guard cannot be scoped"}
	}
	sq := squashSpace(jsStrip(scale))

	// FOUR sides go into the denominator: rows[i].a, rows[i].b, total.a, total.b.
	// Counting them is what makes this rule exhaustive — an earlier version banned two
	// specific `num(x&&x.tier)` spellings, both naming only the `a` side (and one of
	// them naming a local the same commit had deleted, so it could never match). A
	// revert of either B side was measured reporting clean, including the org total's
	// SELECTED window, which is the exact 2.8e8 case that collapses every dot.
	if n := strings.Count(sq, "rankedTier("); n != 4 {
		bad = append(bad, "compareScale routes "+strconv.Itoa(n)+" sides through rankedTier, "+
			"want 4 (each row's a and b, and the org total's a and b). A side that reaches the "+
			"shared denominator ungated puts a below-floor 2.8e8 in it, which collapses EVERY "+
			"dot on the chart — ranked rows included — to zero width")
	}
	// …and nothing else may reach it. Any other `.tier` read in this function is a
	// side going in without its verdict, whatever it is spelled.
	if n := strings.Count(sq, ".tier"); n != 0 {
		bad = append(bad, "compareScale reads `.tier` directly "+strconv.Itoa(n)+" time(s); "+
			"every side must go through rankedTier, which is the single definition of a "+
			"plottable reading shared with the dot positions")
	}
	return bad
}

// dumbbellPlotViolations: excluding an unranked side from the SCALE is only half
// the fix. Its own dot is still tier/scale and pct() clamps UP, so the number the
// scale just refused returns pinned to the far right of the track.
func dumbbellPlotViolations(js string) (bad []string) {
	for _, fn := range []struct {
		header, name string
		wantPlotA    string
		wantPlotB    string
	}{
		{
			// Both builders now derive their plot flags from the SAME hoisted identifiers
			// (#613, 2026-08-05). buildDevDumbbell used to read `!!row.a.ranked` inline
			// here and nowhere else, which is exactly how the two channels drifted apart:
			// the flag that gated the dot had no name, so nothing could assert the flag
			// gating the digits was the same one.
			header: "function buildDevDumbbell(row, scale) {", name: "buildDevDumbbell",
			wantPlotA: "varplotA=hasA&&aRanked;", wantPlotB: "varplotB=hasB&&bRanked;",
		},
		{
			header: "function buildTeamDumbbell(row, scale) {", name: "buildTeamDumbbell",
			wantPlotA: "varplotA=hasA&&aRanked;", wantPlotB: "varplotB=hasB&&bRanked;",
		},
	} {
		raw, ok := jsBlockAfter(js, fn.header)
		if !ok {
			bad = append(bad, fn.name+" not found — the #605 plot guard cannot be scoped")
			continue
		}
		raw = jsStrip(raw)
		body := squashSpace(raw)
		for _, want := range []string{
			"varfracA=plotA?num(row.a.tier)/scale:0;",
			"varfracB=plotB?num(row.b.tier)/scale:0;",
		} {
			if !strings.Contains(body, want) {
				bad = append(bad, fn.name+" positions a dot without consulting whether the side is "+
					"plottable (missing `"+want+"`): pct() clamps to 100%, so a side excluded from "+
					"the scale renders pinned to the end of the track — the best reading in the view")
			}
		}
		if !strings.Contains(body, "buildDumbbellTrack(plotA,fracA,plotB,fracB,") {
			bad = append(bad, fn.name+" still passes the has-data flags to buildDumbbellTrack "+
				"rather than the plottable ones, so an unranked side draws a dot at position 0 "+
				"and a connector to it")
		}
		// The WHOLE expression, not a prefix. `var plotA = hasA && true;` satisfied a
		// `varplotA=hasA&&` prefix pin and was measured reverting this half of #605
		// completely, in both builders, with the suite green.
		for _, want := range []string{fn.wantPlotA, fn.wantPlotB} {
			if !strings.Contains(body, want) {
				bad = append(bad, fn.name+"'s plot flag is not has-data AND the side's own "+
					"verdict (want `"+want+"`) — a flag ANDed with a constant is not a gate")
			}
		}
		// An EMPTY track must still have a stated cause. A one-sided row whose single
		// present side is below the floor now plots no dot at all, and its tag talks only
		// about the window that is MISSING ("only in selected period") — so the reader
		// sees an empty track and a sentence that does not explain it. That is the blank
		// cell cmpWithheldCell exists to avoid, one component over.
		if fn.name == "buildDevDumbbell" &&
			!strings.Contains(body, "if((hasA&&!plotA)||(hasB&&!plotB)){tag=BELOW_FLOOR_TAG") {
			bad = append(bad, fn.name+" leaves a one-sided BELOW-FLOOR row with an empty track "+
				"and a tag that names only the missing window; an empty track with no stated "+
				"cause reads as a broken render, which is exactly what the withheld org cells "+
				"refuse to do")
		}

		// 🔴 DELETED 2026-08-05, and the deletion is the point. This rule used to assert
		// `strings.Contains(body, "abReadout(hasA,")` with the stated meaning "a
		// below-floor DEVELOPER row still states its number". Under the #613 ruling that
		// substring is STILL PRESENT — as the first argument of the gated call — so the
		// assertion would keep passing while the sentence it documents became false.
		// A guard that survives the change it was written to detect is worse than none:
		// it reports lockstep and is believed. Replaced by the identity rule below.

		// (5) THE CROSS-CHANNEL IDENTITY (#613, 2026-08-05) — the durable rule, and the
		// one whose absence WAS this defect. Publication gated on the row-level
		// conjunction while plotting gated per side, so a row printed '—'/'—' and still
		// placed a dot whose position recovered the withheld number exactly (pct() writes
		// three decimals: 17.5000 against an actual 17.5). Worse, through the shared
		// denominator that number was recoverable from OTHER rows' dots too.
		//
		// The rule: THE FLAG THAT WITHHOLDS THE DIGIT MUST BE THE FLAG THAT WITHHOLDS THE
		// DOT. Asserted STRUCTURALLY, by deriving the identifier from the plot flag and
		// requiring it in the readout's matching slot — not by a byte pin, which would cry
		// wolf on a consistent rename while catching no mutant the derivation misses.
		// Drift in EITHER direction fails, and nobody has to remember the case.
		gates := map[string]string{}
		for _, m := range dumbbellPlotFlagRe.FindAllStringSubmatch(raw, -1) {
			// The side letters must AGREE. `var plotA = hasB && aRanked;` is caught by the
			// whole-expression pin above today, but that pin is a byte pin this rule's own
			// comment argues against — so anyone who acts on the comment and removes it
			// would open the hole in the same commit. Close it here, structurally.
			if m[1] != m[2] {
				bad = append(bad, fn.name+" gates plot"+m[1]+" on has"+m[2]+" — a plot flag must "+
					"read its OWN side's has-data flag, or one side's absence silences the other")
				continue
			}
			gates[m[1]] = m[3]
		}
		if len(gates) != 2 {
			bad = append(bad, fn.name+" does not derive both plot flags as `has<Side> && "+
				"<verdict>` (found "+strconv.Itoa(len(gates))+" of 2) — the identity rule below "+
				"cannot be evaluated, and a rule that cannot run must not report clean")
			continue
		}
		readouts, roOK := jsCallArgs(raw, "abReadout(")
		if !roOK || len(readouts) != 1 {
			bad = append(bad, fn.name+" makes "+strconv.Itoa(len(readouts))+" abReadout calls, "+
				"want exactly 1 — a second call is a second, unaudited route to both digits")
			continue
		}
		roArgs, roArgsOK := jsSplitArgs(readouts[0])
		if !roArgsOK || len(roArgs) != 6 {
			bad = append(bad, fn.name+"'s abReadout call does not parse as six arguments "+
				"(hasA, aRanked, aTier, hasB, bRanked, bTier): "+readouts[0])
			continue
		}
		for _, s := range []struct {
			side string
			idx  int
			win  string
		}{{"A", 1, "baseline"}, {"B", 4, "selected"}} {
			if got := squashSpace(roArgs[s.idx]); got != gates[s.side] {
				bad = append(bad, fn.name+" gates its "+s.win+" DIGIT on `"+got+"` but its "+
					"DOT on `"+gates[s.side]+"` — these must be the same flag. Two gates on one "+
					"reading is how the withheld number stayed recoverable from the position "+
					"printed beside it, to three decimals")
			}
		}
	}
	return bad
}

// dumbbellPlotFlagRe extracts the verdict identifier a builder ANDs with the has-data
// flag to produce a plot flag — the `aRanked` in `var plotA = hasA && aRanked;`. It
// deliberately matches an IDENTIFIER only: `hasA && true`, `hasA && !!row.a.ranked`
// and `hasA && (x||y)` all fail to match, so the identity rule above reports "cannot
// be evaluated" rather than silently comparing against an empty string.
var dumbbellPlotFlagRe = regexp.MustCompile(`var\s+plot([AB])\s*=\s*has([AB])\s*&&\s*([A-Za-z_$][\w$]*)\s*;`)

// TestDashboard_CompareScaleIgnoresUnrankedSides pins the half of #605 that is a
// RENDERING-INTEGRITY bug rather than a publication one.
//
// compareScale is the shared denominator for every dot on the compare chart.
// Measured with the ruling's own numbers: a below-floor org total of 2.8e8 in that
// denominator collapses EVERY row's dot — ranked and unranked alike — to zero
// width, so the chart does not merely publish a bad number, it stops rendering.
// Gating the scale is therefore required for the chart to work at all.
//
// And gating the scale is only half of it, exactly as it was for buildPanel: an
// excluded side's own dot is still tier/scale and pct() clamps UP, so the number
// the scale just refused comes back pinned to the far right of the track — the
// best-looking reading in the view.
func TestDashboard_CompareScaleIgnoresUnrankedSides(t *testing.T) {
	_, js := get(t, jsPath)

	for _, v := range compareScaleViolations(js) {
		t.Error(v)
	}
	rt, ok := jsBlockAfter(js, "function rankedTier(side) {")
	if !ok {
		t.Fatal("rankedTier not found")
	}
	if !strings.Contains(squashSpace(jsStrip(rt)), "side.ranked") {
		t.Error("rankedTier does not read the side's `ranked` verdict")
	}
	if !strings.Contains(squashSpace(jsStrip(rt)), "?num(side.tier):0") {
		t.Error("rankedTier does not zero an unranked side, so the scale is unchanged")
	}

	// The dot half, in BOTH builders. An unranked side draws no positioned dot.
	for _, v := range dumbbellPlotViolations(js) {
		t.Error(v)
	}

	// The on-page LEGEND must describe what the chart now does. It used to promise
	// that "each row plots its TIER before and after on a shared scale" — true of
	// every row until an unranked side stopped being placed on it. A legend that
	// describes behaviour the code no longer has is this branch's own defect class,
	// one element over.
	note, ok := jsBlockAfter(js, "function compareNote(devMode) {")
	if !ok {
		t.Fatal("compareNote not found")
	}
	if !strings.Contains(note, "shared scale of ranked readings") {
		t.Error("the compare legend still promises that every row is plotted on the shared " +
			"scale; an unranked side is deliberately not placed on it, and the legend must say so")
	}
	// 🔴 REVERSED 2026-08-05 (#613 half b), deliberately and by ruling. This assertion
	// used to require the legend to say a below-floor side "states its number, but is
	// not placed" — i.e. #605 did not merely fail to rule on developer rows, it
	// ASSERTED AND TESTED that they publish their digits. The ruling overturns that
	// commitment, so the test that pins it is updated as PART of the ruling rather
	// than around it: leaving the old pin would have made the legend and the code
	// disagree, which is the defect class this whole guard exists for.
	if strings.Contains(note, "states its number, but is not placed") {
		t.Error("the compare legend still promises that a below-floor side states its number. " +
			"That was #605's rule and #613 overturned it: such a side is now neither placed " +
			"nor stated, at BOTH grains. A legend describing behaviour the code does not have " +
			"is the defect this guard exists to catch")
	}
	if !strings.Contains(note, "neither placed on that scale nor stated as a number") {
		t.Error("the compare legend does not state the rule the code now applies — a below-floor " +
			"side is neither plotted nor printed. Without it, two em dashes read as missing " +
			"data rather than as a reading deliberately not published")
	}
	// The Δ half of the rule is a SEPARATE claim and needs its own pin: a derived
	// figure needs BOTH sides, so it is withheld under a strictly weaker condition
	// than the digits. A legend that stated only the per-side rule would leave a
	// reader unable to explain a row printing one number beside an em-dash Δ.
	if !strings.Contains(note, "shown only when both periods are ranked") {
		t.Error("the compare legend does not tell the reader that a Δ needs BOTH periods " +
			"ranked; per-side publication means a row can print one reading and withhold " +
			"the difference, and nothing on screen would explain why")
	}
}

// TestDashboard_CompareTeamRowsHonourRanked pins #603's defect in its SECOND
// location. buildDevDumbbell has computed bothRanked and muted on it since #278;
// team rows read `ranked` on neither side, so a 3-developer, $0.30 team rendered as
// full-authority evidence — the same hardcoded-ranked lie the yield bars carried
// until #603, one view over.
func TestDashboard_CompareTeamRowsHonourRanked(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(js, "function buildTeamDumbbell(row, scale) {")
	if !ok {
		t.Fatal("buildTeamDumbbell not found — the #603 compare guard cannot be scoped")
	}
	sq := squashSpace(jsStrip(body))

	if !strings.Contains(sq, "varbothRanked=aRanked&&bRanked;") {
		t.Error("buildTeamDumbbell does not compute a both-ranked verdict from its two sides, " +
			"the way its sibling buildDevDumbbell has since #278")
	}
	for _, want := range []string{
		"varaRanked=!!(row.a&&row.a.ranked);",
		"varbRanked=!!(row.b&&row.b.ranked);",
	} {
		if !strings.Contains(sq, want) {
			t.Errorf("buildTeamDumbbell does not read a side's verdict (missing %q); `!!` also "+
				"fails closed, so a server that does not say never gets the unmuted treatment", want)
		}
	}
	// The row must LOOK different, and it must SAY so. Muted colour alone is a WCAG
	// 2.1 SC 1.4.1 failure — the finding #534 raised on the yield bars.
	// UNSQUASHED. squashSpace deletes the space inside `' below'`, so the squashed
	// form cannot tell the correct class from `'below'` — which concatenates to
	// "cmp-db-labelbelow", matches no CSS rule, and renders the row UNMUTED. That is
	// #603's exact defect, and the squashed assertion passed straight through it.
	if !strings.Contains(jsStrip(body), "'cmp-db-label' + (bothRanked ? '' : ' below')") {
		t.Error("a below-floor team row's label is not muted (the class must be ' below' with " +
			"its leading space, or it concatenates into a class name no rule matches), so it " +
			"renders as full-authority evidence at any cost level (#603)")
	}
	if !strings.Contains(jsStrip(body), "bothRanked ? 'aggregate — not tested' : BELOW_FLOOR_TAG") {
		t.Error("a below-floor team row still reads 'aggregate — not tested'. That says " +
			"significance was not TESTED, which is a statement about method and true of every " +
			"team row; it is not true that a below-floor row merely went untested — it lacks " +
			"the evidence to rank at all, and must name the verdict the rest of the dashboard " +
			"names (#603/WCAG 1.4.1)")
	}
	// Positive control: a RANKED team row keeps the method caveat and its normal
	// colour, or a guard hardcoded to "always muted" would satisfy every check above.
	if !strings.Contains(js, "aggregate — not tested") {
		t.Error("the 'aggregate — not tested' caveat is gone entirely; a ranked group aggregate " +
			"still carries no bootstrap CI and must still say so (#277)")
	}
}

// --- #613: a below-floor compare TEAM row publishes no digits ----------------
//
// #613 AMENDS #603. #603 ruled a below-floor team row "muted but printed"; that
// display rule is superseded, because muting is a styling HINT and cannot propagate
// through arithmetic. When the k-anonymity fold yields a SINGLE cohort, the one team
// row IS the org total: the card above withholds the org headline as '—' (#605) and
// the row republished it in full one line down — INCLUDING the delta, which was
// printed unconditionally and therefore reconstructed the withheld `Δ Org TIER`. In
// that case #605 was not merely contradicted, it was nullified.
//
// The ruled rule has no row count in it — a below-floor value is never printed at
// any grain (in this view) — precisely so it can be enforced from what the builder
// already holds. #136 is untouched: the number is unchanged and still on the wire.
//
// TWO leaks, TWO named tests. A guard that caught only the readout would have passed
// the code that produced this issue, because the delta cell alone reconstructs the
// withheld headline.

// teamWithheldRootAllow / teamWithheldPropAllow are the whole-body allowlist for
// cmpRankedSide. It is the rule that makes the slot pins exhaustive rather
// than a list of shapes someone thought of: the withheld column may name only its
// one parameter, the shared cell builder and the shared verdict phrase, so a sink
// nobody predicted (`val.title =`, `setText(...)`, a re-derivation from an outer
// variable) is a violation by construction.
// 🔴 RETARGETED 2026-08-05 at cmpRankedSide, the per-side gate. It may name its
// three parameters and num(); it may name NO DOM constructor at all, because it
// returns a STRING. That is stricter than the column-level rule it replaces: there
// is no `el`, no `appendChild`, no node to attach anything to.
var teamWithheldRootAllow = map[string]bool{
	"var": true, "const": true, "let": true, "return": true, "if": true,
	"has": true, "ranked": true, "tier": true, "num": true,
}

// teamWithheldPropAllow: toFixed is the ONE property the per-side gate may reach —
// it formats a number and returns it. appendChild, textContent, title, dataset and
// setAttribute are all absent by omission, and now cannot appear at all: this
// function never holds a node.
var teamWithheldPropAllow = map[string]bool{"toFixed": true}

// teamRowValueRootAllow / teamRowValuePropAllow: the shared three-cell builder may
// name its three parameters, its own local and el(). Nothing else.
var teamRowValueRootAllow = map[string]bool{
	"var": true, "const": true, "let": true, "return": true, "if": true,
	"val": true, "tag": true, "el": true,
	// The two gates and the three values they govern. `num` is deliberately ABSENT:
	// this function receives already-formatted strings and must never be able to
	// derive a number of its own.
	"deltaCls": true, "bothRanked": true, "deltaText": true, "ab": true, "srNote": true,
}

var teamRowValuePropAllow = map[string]bool{"appendChild": true}

// abReadoutRootAllow / abReadoutPropAllow: abReadout is a PURE FORMATTER and must
// stay one. It is passed as an ARGUMENT, so it runs on the withheld path as well —
// the gate discards its return value, never its side effects. It may name its SIX
// parameters and cmpRankedSide; it may not name a DOM node, a global, or a sink —
// and, since #613, not num() either (see below).
var abReadoutRootAllow = map[string]bool{
	"return": true, "var": true, "const": true, "let": true,
	"hasA": true, "aRanked": true, "aTier": true,
	"hasB": true, "bRanked": true, "bTier": true,
	// It now holds NO gate of its own: both halves go through cmpRankedSide, which is
	// scoped by its own rule. num() is gone from this list deliberately — a formatter
	// that can still reach num() can still format a tier it did not gate.
	"cmpRankedSide": true,
}

// 🔴 EMPTY as of 2026-08-05, and that is the assertion. abReadout composes two
// already-gated strings and concatenates them; it reaches NO property of anything.
// `toFixed` used to be here because this function formatted the numbers itself —
// which is exactly the capability the per-side gate took away from it.
var abReadoutPropAllow = map[string]bool{}

// renderCompareRootAllow / renderComparePropAllow: the CALLER's whole-body
// allowlist. It may name its own locals, the payload fields it dispatches on, and
// the builders/helpers it composes — nothing else. This replaces a `.tier` substring
// ban that two measured mutants walked straight past by re-deriving TIER from
// weighted_points and total_cost_usd and publishing through setText.
var renderCompareRootAllow = map[string]bool{
	"var": true, "const": true, "let": true,
	"if": true, "else": true, "for": true, "return": true,
	"true": true, "false": true, "null": true, "undefined": true,
	// Payload, locals, loop variable.
	"data": true, "devMode": true, "rows": true, "scale": true, "host": true, "i": true,
	// The helpers it is allowed to compose. Every one of them is scoped by its own
	// rule, here or in the #605 guard.
	"$": true, "el": true, "setText": true, "showStatus": true,
	"renderCompareWindows": true, "renderCompareTotal": true, "compareScale": true,
	"compareNote": true, "buildDevDumbbell": true, "buildTeamDumbbell": true,
}

var renderComparePropAllow = map[string]bool{
	// The ONLY fields of the payload it may read — note `tier`, `weighted_points`
	// and `total_cost_usd` are all absent, so both measured bypasses are refused by
	// omission rather than by having been listed.
	"mode": true, "developers": true, "teams": true, "total": true, "length": true,
	// DOM mechanics.
	"style": true, "display": true, "textContent": true, "appendChild": true,
}

// teamRowBuilderRootAllow / teamRowBuilderPropAllow: buildTeamDumbbell's whole body.
// Four measured bypasses of the #605 guard walked past every per-call rule because
// the SINK was not a cell at all, so the row builder gets the same treatment: it may
// name its locals, the row fields it reads and the builders it composes, and any
// other route onto the page fails without anyone having predicted it.
var teamRowBuilderRootAllow = map[string]bool{
	"var": true, "const": true, "let": true, "return": true,
	"true": true, "false": true, "null": true, "undefined": true,
	// Parameters and locals.
	"row": true, "scale": true, "hasA": true, "hasB": true, "d": true,
	"aRanked": true, "bRanked": true, "bothRanked": true,
	"plotA": true, "plotB": true, "fracA": true, "fracB": true,
	"rowEl": true, "tag": true,
	// Formatters, the shared vocabulary, and the three builders it composes.
	"num": true, "Math": true, "signStr": true, "abReadout": true,
	"el": true, "buildDumbbellTrack": true, "cmpRankedRowValue": true,
	"withheldSidesNote": true,
	"BELOW_FLOOR_TAG":   true,
}

var teamRowBuilderPropAllow = map[string]bool{
	// The ONLY fields of `row` this function may read.
	"a": true, "b": true, "ranked": true, "tier": true, "delta_tier": true, "team": true,
	// Formatting and the one DOM mechanic.
	"abs": true, "toFixed": true, "appendChild": true,
}

// jsBareReturnRe matches a `return` that ends its own line. ASI then inserts a
// semicolon and the expression below it becomes DEAD CODE.
//
// 🔴 MEASURED SURVIVING the first version of this guard. Rewriting the gate as
//
//	if (!ranked) { return
//	  cmpRankedSide(tag); }
//
// squashes to a byte-identical body — squashSpace deletes the newline the defect is
// MADE of — so the whole-body pin below reported clean on a gate that returns
// undefined and never builds the withheld column at all. Same lesson as #603's
// `' below'`: an assertion that squashes whitespace cannot decide a question whose
// answer IS whitespace.
var jsBareReturnRe = regexp.MustCompile(`\breturn[ \t]*\r?\n`)

// teamWithheldBody scopes the two per-cell rules to cmpRankedSide. The header
// pins the ARITY as a side effect, and that is load-bearing: the withheld state has
// no value to pass, so the only way to smuggle one in is to add a parameter — and a
// mutant that does stops being found here rather than being scanned leniently.
// 🔴 RETARGETED 2026-08-05. Withholding is no longer a whole-COLUMN state, so there
// is no cmpRankedSide to scope. The equivalent — and the place the ruling put
// the shape-not-taste property — is cmpRankedSide, the PER-SIDE gate: its `ranked`
// argument is the gate and its unranked path DISCARDS `tier`, so a guard still
// decides the question by reading one small function.
func teamWithheldBody(js string) (string, bool) {
	return jsBlockAfter(js, "function cmpRankedSide(has, ranked, tier) {")
}

// teamWithheldArityViolation turns the ONE mutant that used to die as a short read
// into a NAMED violation.
//
// Adding a parameter to the withheld column is the whole smuggling attack, and
// scoping on the exact header made it land as `ok=false` -> t.Fatal("could not
// scope cmpRankedSide"). That fails loudly, but it is the SAME signal a
// benign rename produces, so the reader cannot tell "someone smuggled a value in"
// from "someone renamed something". Look the function up by name, then say which it
// was. Returns ok=false only when the function is genuinely absent.
func teamWithheldArityViolation(js string) (bad string, ok bool) {
	params, found := jsCallArgs(jsStrip(js), "function cmpRankedSide(")
	if !found || len(params) == 0 {
		return "", false
	}
	// EXACTLY three, in this order. `has` and `ranked` are the two gates; `tier` is the
	// only value, and it is the one the unranked path must discard. A FOURTH parameter
	// is the smuggling channel the old arity rule existed to close, moved down a grain:
	// anything else this function can be handed, it can print.
	if got := squashSpace(params[0]); got != "has,ranked,tier" {
		return "cmpRankedSide takes (" + got + "), want (has, ranked, tier) — two gates and " +
			"ONE value, which is the only reason no caller can smuggle a second number past " +
			"the gate. Every additional parameter is a channel for exactly that", true
	}
	return "", true
}

// gateInputViolations pins what the three gate flags MEAN, for EITHER builder.
//
// 🔴 SHARED as of 2026-08-05, and the sharing is the fix. This block lived only in
// teamRowGateViolations. When #613's ruling made buildDevDumbbell a second caller of
// the gate, the mirror copied the CALL-SITE pins and not these — so on developer rows
// `var aRanked = true;` (#603's hardcoded-ranked lie) republished both the digit and
// the dot with the entire suite green, and `bothRanked = both && (aRanked || bRanked)`
// published a Δ that reconstructs the withheld side exactly. Measured: 0 findings from
// every guard in this file.
//
// Pinning that the gate ARGUMENT is the identifier `bothRanked` says nothing about
// what bothRanked MEANS. The cross-channel identity rule in dumbbellPlotViolations
// does not cover it either: that proves the digit gate and the dot gate are the SAME
// flag — which they are — and says nothing about whether that flag is derived from the
// payload. Orthogonal properties; only one of them was enforced.
//
// One function, called by both grains, so they cannot drift apart again.
func gateInputViolations(code, where string) (bad []string) {
	// STRUCTURAL, not byte-exact, for the reason #605 learned: a verbatim pin cries
	// wolf on `const` or `Boolean(x)` while catching no mutant the field checks miss.
	for _, g := range []struct {
		name, mustRead, why string
		mustNotRead         []string
		failClosed          bool
	}{
		{
			name: "aRanked", mustRead: "row.a.ranked", failClosed: true,
			why: "the baseline side's verdict must be READ from the payload, not assumed — a " +
				"hardcoded `true` here is #603's exact defect, which is what put this row's " +
				"digits on screen in the first place",
		},
		{
			name: "bRanked", mustRead: "row.b.ranked", failClosed: true,
			why: "same, for the selected side",
		},
		{
			name: "bothRanked", mustRead: "aRanked", mustNotRead: []string{"||"},
			why: "the gate must be the CONJUNCTION. With `||`, a ranked baseline beside an " +
				"unranked selected window publishes both readings — and `selected = baseline + Δ` " +
				"then reconstructs the withheld org headline exactly, which is the reconstruction " +
				"#605 was ruled on",
		},
	} {
		rhs, gFound := jsAssignRHS(code, g.name)
		if !gFound {
			bad = append(bad, where+" never assigns `"+g.name+"` — "+g.why)
			continue
		}
		if !strings.Contains(rhs, g.mustRead) {
			bad = append(bad, "the `"+g.name+"` gate input is `"+rhs+"`, which never reads `"+
				g.mustRead+"` — "+g.why)
		}
		for _, banned := range g.mustNotRead {
			if strings.Contains(rhs, banned) {
				bad = append(bad, "the `"+g.name+"` gate input is `"+rhs+"`, which uses `"+banned+
					"` — "+g.why)
			}
		}
		if g.name == "bothRanked" && !strings.Contains(rhs, "bRanked") {
			bad = append(bad, "the `bothRanked` gate input is `"+rhs+"`, which never reads "+
				"`bRanked` — "+g.why)
		}
		if g.failClosed && !strings.HasPrefix(rhs, "!!") && !strings.HasPrefix(rhs, "Boolean(") {
			bad = append(bad, "the `"+g.name+"` gate input is `"+rhs+"`, which does not coerce to "+
				"a boolean; it must fail CLOSED so a server that does not say never gets the "+
				"unwithheld treatment")
		}
		if n := jsAssignCount(code, g.name); n != 1 {
			bad = append(bad, where+" assigns `"+g.name+"` "+strconv.Itoa(n)+" times, "+
				"want exactly 1 — a second assignment reopens a gate the first one closed, and "+
				"the pinned declaration still reads correctly")
		}
	}

	return bad
}

// teamRowGateViolations is the half BOTH named tests share, because a broken gate
// leaks BOTH cells: it pins that buildTeamDumbbell builds no value cell itself, and
// that its single value-column call is gated on `bothRanked` with every one of its
// four arguments pinned.
//
// ALL FOUR arguments, per the #605 lesson: pinning the label and the gate there left
// the value free, and pinning the value left the sub slot free — each intermediate
// stage measured CLEAN while publishing a withheld number. A call whose arguments
// are only partly audited is a call that is not audited.
func teamRowGateViolations(js string) (bad []string, ok bool) {
	body, found := jsBlockAfter(js, "function buildTeamDumbbell(row, scale) {")
	if !found {
		return nil, false
	}
	code, stripOK := jsStripStrings(body)
	if !stripOK {
		return []string{"buildTeamDumbbell contains an unterminated string literal or block " +
			"comment, so everything after it went unscanned — a guard that cannot read its " +
			"input must not report clean"}, true
	}

	// (1) Syntax this guard cannot see through, refused outright rather than parsed.
	if strings.Contains(code, "`") {
		bad = append(bad, "buildTeamDumbbell uses a template literal — its ${} interpolations are "+
			"invisible to this guard, so a withheld reading could be published inside one")
	}
	if strings.Contains(code, "[") {
		bad = append(bad, "buildTeamDumbbell uses `[`; computed member access hides a field name "+
			"inside a string literal that no property check then sees, and this guard cannot "+
			"tell that from an index, so the strict answer is the safe one")
	}
	for _, r := range code {
		if r > 0x7F {
			bad = append(bad, "buildTeamDumbbell contains the non-ASCII character "+
				strconv.QuoteRune(r)+" outside a string literal — jsIdentPathRe is ASCII-only, so "+
				"an identifier this guard cannot even see is refused rather than scanned")
			break
		}
	}

	// (1c) ASI, which no squashed pin below can see. A newline after this function's
	// `return` makes `rowEl` unreachable, the builder returns undefined, and
	// renderCompare's `host.appendChild(undefined)` throws — the whole compare team
	// view stops rendering. That fails CLOSED rather than leaking, but an invariant
	// whose enforcement can be switched off by a newline is not enforced.
	if jsBareReturnRe.MatchString(body) {
		bad = append(bad, "buildTeamDumbbell has a `return` at the end of a line — ASI makes what "+
			"follows unreachable, so the builder returns undefined and the row never renders. "+
			"Every squashed pin in this rule is blind to it: squashing deletes the newline the "+
			"defect is made of")
	}

	// (2) The whole body, by allowlist.
	bad = append(bad, cmpAllowViolations2("buildTeamDumbbell", body,
		teamRowBuilderRootAllow, teamRowBuilderPropAllow)...)

	// (3) The row builder constructs NO value cell of its own. Every digit-bearing
	// cell goes through the gate, so a cell built here is a cell that bypassed it.
	for _, cls := range []string{"'cmp-db-value'", "'cmp-db-delta", "'cmp-db-ab", "'cmp-db-sr'"} {
		if strings.Contains(body, cls) {
			bad = append(bad, "buildTeamDumbbell builds a value cell itself ("+cls+"): the Δ and "+
				"the A→B readout must be built only by the gated cmpRankedRowValue, or a cell can "+
				"reach the page without passing its own gate")
		}
	}
	if n := strings.Count(body, "rowEl.appendChild("); n != 3 {
		bad = append(bad, "buildTeamDumbbell appends "+strconv.Itoa(n)+" children to the row, want "+
			"3 (label, track, value column) — a fourth is a surface no rule here inspects")
	}

	// (3b) EVERY el() CALL IN THE BODY, PINNED WHOLE — including the two surfaces that
	// SURVIVE withholding. This is the rule the first draft of this guard did not have,
	// and four mutants walked through the hole:
	//   el('div', 'cmp-db-row', num(row.a.tier).toFixed(1))   — el()'s third argument is
	//     textContent, so the row CONTAINER publishes the reading with no cell involved;
	//   row.team + ' ' + num(row.a.tier).toFixed(1)           — the LABEL cell, which is
	//     never withheld, carries the digits instead;
	// and the tag equivalent below. Each satisfied the allowlist (they name only
	// permitted locals), the appendChild count, and every argument pin on the gated
	// call. Withholding two cells means nothing if the row can print the number in a
	// third.
	elCalls, elOK := jsCallArgs(body, "el(")
	if !elOK {
		return nil, false
	}
	wantEls := []string{
		"'div','cmp-db-row'",
		"'div','cmp-db-label'+(bothRanked?'':'below'),row.team||'other'",
	}
	if len(elCalls) != len(wantEls) {
		bad = append(bad, "buildTeamDumbbell makes "+strconv.Itoa(len(elCalls))+" el() calls, want "+
			strconv.Itoa(len(wantEls))+" (the row container and the label) — every other element "+
			"on this row comes from a builder whose output is audited elsewhere, and an extra "+
			"el() here is an unaudited surface")
	}
	for i, call := range elCalls {
		if i >= len(wantEls) {
			break
		}
		if got := squashSpace(call); got != wantEls[i] {
			bad = append(bad, "buildTeamDumbbell's el() call "+strconv.Itoa(i)+" is `el("+got+
				")`, want `el("+wantEls[i]+")` — el()'s third argument is textContent, so an "+
				"element that is never withheld (the row container, the label) can publish the "+
				"reading the value column just declined to")
		}
	}
	// 🔴 UNSQUASHED, for the two literals whose meaning IS their internal whitespace.
	// squashSpace deletes spaces inside string literals too, so the squashed pins above
	// and below cannot tell these apart from the correct source — both were measured
	// SURVIVING on the shipped file:
	//   'cmp-db-label' + (bothRanked ? '' : ' below')  ->  … : 'below'
	//     concatenates to "cmp-db-labelbelow", matches no CSS rule, and the row renders
	//     UNMUTED. That is #603's defect verbatim, on the row #613 withholds.
	//   'aggregate — not tested'  ->  'aggregate—nottested'
	//     an unreadable tag on every ranked team row.
	// Same treatment the control arm already uses on cmpRankedRowValue's three cells, which
	// is why THAT rule catches 'cmp-db-delta num flat' -> 'cmp-db-deltanumflat'.
	for _, want := range []struct{ code, why string }{
		{"el('div', 'cmp-db-label' + (bothRanked ? '' : ' below'), row.team || 'other')",
			"the muting class must be ' below' WITH its leading space, or it concatenates into " +
				"a class name no CSS rule matches and the below-floor row renders at full authority"},
		{"bothRanked ? 'aggregate — not tested' : BELOW_FLOOR_TAG",
			"the method caveat must keep its spaces, and the below-floor arm must be the shared " +
				"BELOW_FLOOR_TAG rather than a second phrase for the same verdict"},
		// squashSpace joins on "", so 'insignificant' and 'in significant' are byte-
		// identical to the squashed pin below — and the second one is a PRODUCT defect,
		// not a typo. index.html's .cmp-db-connect.insignificant is what makes the
		// connector a faint dashed rule; split the literal and the class list becomes
		// `in` + `significant`, that rule stops matching, and the connector falls back to
		// the SOLID ACCENT BAR — the significant-move treatment. Every k-anonymized team
		// row would then assert a confident change, which an aggregate has no CI to
		// support and #277 forbids outright.
		{"buildDumbbellTrack(plotA, fracA, plotB, fracB, '', 'insignificant')",
			"the connector class must be the single literal 'insignificant'; split by a space " +
				"it stops matching .cmp-db-connect.insignificant and the row renders the solid " +
				"accent connector, asserting a significant move for an aggregate that was never " +
				"tested for one (#277)"},
	} {
		if !strings.Contains(body, want.code) {
			bad = append(bad, "buildTeamDumbbell is missing the exact source `"+want.code+
				"` — "+want.why+". This assertion is deliberately UNSQUASHED: squashing deletes "+
				"the whitespace the defect is made of")
		}
	}

	// The TAG is the third surviving surface, and the #603 assertion on it is a
	// Contains, so `… : BELOW_FLOOR_TAG + ' ' + signStr(d) + Math.abs(d).toFixed(1)`
	// keeps that assertion satisfied while printing the withheld Δ in the tag cell.
	// Measured passing every other rule here. Pin the whole expression.
	const wantTag = "bothRanked?'aggregate—nottested':BELOW_FLOOR_TAG"
	if rhs, found := jsAssignRHS(body, "tag"); !found {
		bad = append(bad, "buildTeamDumbbell never assigns `tag` — the status tag is the one cell "+
			"that survives withholding, and it must be the verdict alone")
	} else if rhs != wantTag {
		bad = append(bad, "buildTeamDumbbell's `tag` is `"+rhs+"`, want `"+wantTag+"` — the tag "+
			"cell is never withheld, so anything appended to it is published unconditionally")
	}
	// The TRACK's class arguments, for the same reason: they are the row's fourth
	// surviving surface, and this call's first four arguments are the ONLY part
	// dumbbellPlotViolations pins.
	if !strings.Contains(squashSpace(body), "buildDumbbellTrack(plotA,fracA,plotB,fracB,'','insignificant')") {
		bad = append(bad, "buildTeamDumbbell's buildDumbbellTrack call is not "+
			"`(plotA, fracA, plotB, fracB, '', 'insignificant')` — the two class arguments are "+
			"unpinned everywhere else, and a reading concatenated into a class name is still a "+
			"reading that left the process")
	}

	// (4) The gated call, with every argument pinned.
	calls, callsOK := jsCallArgs(body, "cmpRankedRowValue(")
	if !callsOK {
		return nil, false // an unterminated call is a short read, not a clean file
	}
	if len(calls) != 1 {
		bad = append(bad, "buildTeamDumbbell makes "+strconv.Itoa(len(calls))+
			" cmpRankedRowValue calls, want exactly 1 — the value column is the only surface "+
			"carrying this row's digits, and a second call is a second, unaudited one")
	}
	for _, call := range calls {
		args, argsOK := jsSplitArgs(call)
		if !argsOK || len(args) != 6 {
			bad = append(bad, "the cmpRankedRowValue call does not parse as six arguments "+
				"(tag, deltaCls, bothRanked, deltaText, ab, srNote): "+call)
			continue
		}
		for _, a := range []struct{ slot, got, want, why string }{
			{"tag", squashSpace(args[0]), "tag",
				"the tag is the one cell that survives withholding, and it must be the row's own " +
					"computed verdict"},
			{"deltaCls", squashSpace(args[1]), "'flat'",
				"a TEAM row's Δ class is the literal 'flat' — an aggregate has no CI, so its " +
					"magnitude is shown but never asserted as beyond noise (#277). A direction " +
					"here would paint a k-anonymized group's move confident green or red"},
			{"gate", squashSpace(args[2]), "bothRanked",
				"the Δ gate must be the both-sides verdict this function already computes. A " +
					"DERIVED figure needs both sides: with a ranked baseline beside an unranked " +
					"selected window, `selected = baseline + Δ` reconstructs the withheld side " +
					"exactly, which is why the Δ is strictly stricter than the digits"},
			{"delta", squashSpace(args[3]), "signStr(d)+Math.abs(d).toFixed(1)",
				"the Δ must be handed to the gate rather than printed beside it: this is the cell " +
					"that reconstructs the withheld `Δ Org TIER` when the fold yields one cohort"},
			{"readout", squashSpace(args[4]),
				"abReadout(hasA,aRanked,row.a&&row.a.tier,hasB,bRanked,row.b&&row.b.tier)",
				"the A→B readout must be handed the PER-SIDE flags — the same aRanked/bRanked " +
					"plotA/plotB read. Handing it `bothRanked` twice restores the row-level " +
					"conjunction whose gap against per-side plotting WAS the leak"},
			{"srNote", squashSpace(args[5]), "withheldSidesNote(hasA&&!aRanked,hasB&&!bRanked)",
				"the off-screen note must be derived from `has-data AND NOT ranked` per side. A " +
					"side with no data is ABSENT, not withheld, and calling it withheld claims " +
					"we are holding back a number that does not exist"},
		} {
			if a.got != a.want {
				bad = append(bad, "the cmpRankedRowValue "+a.slot+" argument is `"+a.got+"`, want `"+
					a.want+"` — "+a.why)
			}
		}
	}
	// (4c) THE GATE'S INPUT. Pinning that the gate argument is the identifier
	// `bothRanked` says nothing about what bothRanked MEANS. Both of these leak every
	// below-floor team row and leave every rule above satisfied:
	//
	//	var bothRanked = aRanked || bRanked;   // one ranked side unlocks the other
	//	var bRanked = true;                    // the #603 hardcoded-ranked lie, again
	//
	// They die only in the pre-existing TestDashboard_CompareTeamRowsHonourRanked,
	// which none of #613's tests reference — so loosening THAT test would make this
	// rule vacuous with its own guards silent. #613 owns its gate's input.
	//
	bad = append(bad, gateInputViolations(code, "buildTeamDumbbell")...)

	// `tag` assigned exactly once: a second assignment could relabel a withheld row
	// after the ternary set it, leaving the pinned declaration reading correctly.
	if n := jsAssignCount(code, "tag"); n != 1 {
		bad = append(bad, "buildTeamDumbbell assigns `tag` "+strconv.Itoa(n)+" times, want exactly 1 "+
			"— a second assignment reopens a verdict the first one set")
	}

	// (4b) THE CALLER. Withholding inside the row builder is worth nothing if the loop
	// that appends the row can append a second element beside it:
	//
	//	host.appendChild(devMode ? … : buildTeamDumbbell(rows[i], scale));
	//	host.appendChild(el('div', 'cmp-db-tag', num(rows[i].a.tier).toFixed(1)));
	//
	// publishes every below-floor team reading with this whole guard silent, because
	// the sink is one stack frame above everything it scopes. renderCompare touches no
	// reading at all today — it hands rows to a builder and a scale — so the honest
	// rule is that it must never name one.
	// 🔴 REWRITTEN. The first version of this rule was a `.tier` substring ban plus an
	// appendChild count, and it was FALSE ADVERTISING: two mutants published every
	// below-floor reading with zero named failures, because they named no `.tier` and
	// used a sink that is not appendChild —
	//
	//	setText('cmp-note', compareNote(devMode) + ' [' +
	//	  (rows[0].a.weighted_points / rows[0].a.total_cost_usd) + ']');
	//	setText('cmp-windows-extra', String(s.weighted_points / s.total_cost_usd));
	//
	// Enumerating two shapes is not a boundary. The row builder gets a whole-body
	// allowlist; so does its caller. The `[` ban CANNOT be copied across — renderCompare
	// must index `rows[i]` — so the division ban does that work instead: every
	// re-derivation of TIER needs a `/`, and this function legitimately has none.
	if host, hostOK := jsBlockAfter(js, "function renderCompare(data) {"); !hostOK {
		bad = append(bad, "renderCompare not found — the row builder's CALLER is unscoped, and a "+
			"leak there is invisible to every rule in this file")
	} else {
		hostCode, hostStripOK := jsStripStrings(host)
		if !hostStripOK {
			bad = append(bad, "renderCompare contains an unterminated string literal or block "+
				"comment, so everything after it went unscanned")
		} else {
			bad = append(bad, cmpAllowViolations2("renderCompare", host,
				renderCompareRootAllow, renderComparePropAllow)...)
			if strings.Contains(hostCode, "`") {
				bad = append(bad, "renderCompare uses a template literal — a reading inside a ${} "+
					"interpolation is invisible to this guard, and this function sits one stack "+
					"frame above every rule that withholds one")
			}
			if strings.Contains(hostCode, "/") || strings.Contains(hostCode, "**") {
				bad = append(bad, "renderCompare contains `/` or `**`. TIER is "+
					"weighted_points/(cost/1000), so every re-derivation of a withheld reading "+
					"needs one — and this function has no legitimate arithmetic at all. The `[` "+
					"ban that guards the row builder cannot be used here, because the loop must "+
					"index rows[i]; this is what replaces it")
			}
			for _, r := range hostCode {
				if r > 0x7F {
					bad = append(bad, "renderCompare contains the non-ASCII character "+
						strconv.QuoteRune(r)+" outside a string literal — jsIdentPathRe is "+
						"ASCII-only, so an identifier this guard cannot see is refused")
					break
				}
			}
			if jsBareReturnRe.MatchString(host) {
				bad = append(bad, "renderCompare has a `return` at the end of a line — ASI makes "+
					"what follows dead code, and the rows below it never render")
			}
		}
		if strings.Contains(host, ".tier") {
			bad = append(bad, "renderCompare reads `.tier`; it dispatches rows to a builder and "+
				"computes a scale, and a reading named in the loop is a reading published beside "+
				"the row that just withheld it")
		}
		if n := strings.Count(host, "host.appendChild("); n != 2 {
			bad = append(bad, "renderCompare appends "+strconv.Itoa(n)+" things to the row host, "+
				"want 2 (the empty-state note, and one row per iteration) — a third append is a "+
				"per-row surface no rule here inspects")
		}
	}

	// (4d) abReadout ITSELF. "The withheld path discards both values" is true of the
	// RETURN VALUES only — abReadout is an ARGUMENT, so it is CALLED on the withheld
	// path too, before the gate ever sees its result. It formats both raw TIERs, it is
	// shared with buildDevDumbbell, and until now no rule in this file scoped it at
	// all: a mutant adding `n.title = num(aTier).toFixed(1)` inside it published the
	// withheld readings on hover with every #613 rule green.
	//
	// It is a pure formatter and must stay one — no DOM, no sink, no state. The
	// allowlist is the whole rule, because enumerating sinks cannot win that race.
	if ab, abOK := jsBlockAfter(js, "function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {"); !abOK {
		bad = append(bad, "abReadout not found — it is EVALUATED on the withheld path (it is an "+
			"argument, not a return value), so it must be scoped or the withheld row has an "+
			"unaudited function formatting both of its raw TIERs")
	} else {
		if jsBareReturnRe.MatchString(ab) {
			bad = append(bad, "abReadout has a `return` at the end of a line — ASI makes the "+
				"expression under it dead code")
		}
		bad = append(bad, cmpAllowViolations2("abReadout", ab,
			abReadoutRootAllow, abReadoutPropAllow)...)
		if strings.Contains(ab, "`") || strings.Contains(ab, "[") {
			bad = append(bad, "abReadout uses a template literal or `[`; both hide a value from "+
				"this guard, and it runs on the path that is supposed to publish nothing")
		}
		if n := strings.Count(squashSpace(ab), "="); n != 0 {
			bad = append(bad, "abReadout contains "+strconv.Itoa(n)+" assignment(s); it must be a "+
				"pure formatter that RETURNS a string. Anything it writes, it writes on the "+
				"withheld path too, outside the gate")
		}
	}

	// (5) The gate function itself. Pinned whole: it must DISCARD both values on the
	// unranked path, so the decision cannot be separated from the rendering.
	gate, gateOK := jsBlockAfter(js,
		"function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {")
	if !gateOK {
		return bad, false
	}
	// ASI FIRST, because the pin below cannot see it: a `return` alone on its line
	// makes the withheld call dead code, and the squashed forms are byte-identical.
	if jsBareReturnRe.MatchString(gate) {
		bad = append(bad, "cmpRankedRowValue has a `return` at the end of a line — automatic "+
			"semicolon insertion makes the expression under it DEAD CODE, so the gate returns "+
			"undefined and the withheld column is never built. The whole-body pin below cannot "+
			"see this: squashing whitespace deletes the newline the defect is made of")
	}
	// The Δ GATE, pinned whole. Two claims live in this one line and both are the
	// ruling's:
	//   bothRanked ? deltaText : '—'      the unranked path DISCARDS deltaText
	//   bothRanked ? deltaCls  : 'flat'   and falls back to the LITERAL class
	// The second is not cosmetic. `deltaCls` carries up/down colour on developer rows
	// (#277); forwarding it on the withheld path would render an em dash in confident
	// green or red — a direction asserted about a number we declined to publish, which
	// is what cmpWithheldCell's null `dir` refuses one component over.
	const wantGate = "varval=el('div','cmp-db-value');" +
		"val.appendChild(el('div','cmp-db-deltanum'+(bothRanked?deltaCls:'flat')," +
		"bothRanked?deltaText:'—'));" +
		"val.appendChild(el('div','cmp-db-tag',tag));" +
		"val.appendChild(el('div','cmp-db-abnum',ab));" +
		"if(srNote){val.appendChild(el('span','cmp-db-sr',srNote));}" +
		"returnval;"
	if got := squashSpace(gate); got != wantGate {
		bad = append(bad, "cmpRankedRowValue's body is `"+got+"`, want `"+wantGate+"` — the Δ "+
			"gate must DISCARD deltaText on the unranked path and fall back to the literal "+
			"'flat' class; anything that forwards a value or a direction past the gate is not "+
			"a gate")
	}
	// The srNote span is CONDITIONAL, and that is a rule rather than a style. An
	// unconditional append puts an empty span in the DOM on every ranked row, which is
	// indistinguishable to a screen reader from no span at all — and it would make the
	// presence of the span meaningless as a signal that something WAS withheld.
	if !strings.Contains(squashSpace(gate), "if(srNote){") {
		bad = append(bad, "cmpRankedRowValue appends the cmp-db-sr span unconditionally; an "+
			"empty off-screen span on every ranked row says nothing to a screen reader and "+
			"destroys the span's meaning as the signal that a value was withheld")
	}
	return bad, true
}

// teamWithheldScopeViolations is the other shared half: cmpRankedSide may name
// nothing that could carry a number, and it must state the verdict in TEXT.
func teamWithheldScopeViolations(js string) (bad []string, ok bool) {
	if v, arityOK := teamWithheldArityViolation(js); arityOK && v != "" {
		return []string{v}, true // a named finding, not a short read
	}
	body, found := teamWithheldBody(js)
	if !found {
		return nil, false
	}
	code, stripOK := jsStripStrings(body)
	if !stripOK {
		return []string{"cmpRankedSide contains an unterminated string literal or block " +
			"comment, so everything after it went unscanned — a guard that cannot read its " +
			"input must not report clean"}, true
	}
	if strings.Contains(code, "`") {
		bad = append(bad, "cmpRankedSide uses a template literal — a withheld digit inside "+
			"a ${} interpolation is invisible to this guard")
	}
	if strings.Contains(code, "[") {
		bad = append(bad, "cmpRankedSide uses `[`; computed member access hides a field name "+
			"inside a string literal, and an index into the cells it just built is a route to "+
			"rewriting one")
	}
	for _, r := range code {
		if r > 0x7F {
			bad = append(bad, "cmpRankedSide contains the non-ASCII character "+
				strconv.QuoteRune(r)+" outside a string literal — jsIdentPathRe is ASCII-only, so "+
				"an identifier this guard cannot even see is refused rather than scanned")
			break
		}
	}
	if jsBareReturnRe.MatchString(body) {
		bad = append(bad, "cmpRankedSide has a `return` at the end of a line — ASI makes "+
			"what follows dead code, and the withheld column is returned undefined")
	}
	bad = append(bad, cmpAllowViolations2("cmpRankedSide", body,
		teamWithheldRootAllow, teamWithheldPropAllow)...)

	// The PER-SIDE GATE, pinned whole. Three outcomes and the order matters: absence is
	// tested BEFORE rank, so a side with no data reads 'n/a' rather than being reported
	// as withheld. The unranked path returns a literal and DISCARDS `tier` — that
	// discard is the whole shape-not-taste property, moved down from the column to the
	// side, and it is why no call site can carry a below-floor number past its gate.
	const wantSide = "if(!has){return'n/a';}if(!ranked){return'—';}returnnum(tier).toFixed(1);"
	if got := squashSpace(body); got != wantSide {
		bad = append(bad, "cmpRankedSide's body is `"+got+"`, want `"+wantSide+"` — the unranked "+
			"path must return the em dash and DISCARD `tier`. Note the ORDER: `has` is tested "+
			"first, so an absent side reads 'n/a' and only a side that HAS a number it will not "+
			"show reads '—'. Collapsing the two loses the distinction a screen reader depends on")
	}

	// The verdict must reach assistive tech as TEXT, and — since 2026-08-05 — must NAME
	// WHICH SIDE, because per-side withholding is asymmetric. The row-level sentence
	// this replaced said "this window", singular, on a row that has two.
	note, noteOK := jsBlockAfter(js, "function withheldSidesNote(withheldA, withheldB) {")
	if !noteOK {
		bad = append(bad, "withheldSidesNote not found — the withheld state reaches assistive "+
			"tech through it alone, so without it two em dashes and a muted tag are the only "+
			"signal, which is WCAG 2.1 SC 1.4.1 all over again")
		return bad, true
	}
	if jsBareReturnRe.MatchString(note) {
		bad = append(bad, "withheldSidesNote has a `return` at the end of a line — ASI makes the "+
			"expression under it dead code, and the note silently becomes undefined")
	}
	if !strings.Contains(note, "BELOW_FLOOR_TAG") {
		bad = append(bad, "withheldSidesNote does not name BELOW_FLOOR_TAG: compare must state "+
			"this verdict in the dashboard's one vocabulary rather than mint a second phrase")
	}
	// It must distinguish all THREE cases. A note that says "either period" whichever
	// side was withheld is worse than none: it tells a screen-reader user that both
	// readings are unavailable when one of them is printed on screen.
	for _, want := range []string{"'the selected period'", "'the baseline period'", "'both periods'"} {
		if !strings.Contains(note, want) {
			bad = append(bad, "withheldSidesNote never says "+want+" — per-side withholding is "+
				"ASYMMETRIC, and a note that cannot name which side is withheld contradicts the "+
				"digits printed beside it")
		}
	}
	// Empty when nothing is withheld: the span's PRESENCE is the signal, so a note
	// returned unconditionally makes it meaningless.
	if !strings.Contains(squashSpace(note), "if(!withheldA&&!withheldB){return'';}") {
		bad = append(bad, "withheldSidesNote does not return '' when neither side is withheld; "+
			"a note on every row makes the off-screen span's presence meaningless as the signal "+
			"that something WAS withheld")
	}
	return bad, true
}

// teamWithheldSlot names one withheld cell: which argument of the shared builder
// carries it, and what it costs to publish it.
//
// 🔴 As of 2026-08-05 the two gates live in two DIFFERENT functions — the Δ gate in
// cmpRankedRowValue, the per-side digit gate in cmpRankedSide — because they now
// answer different questions (a derived figure needs BOTH sides; a digit needs only
// its OWN). That makes the "two rules, failing independently" requirement structural
// rather than a convention someone has to keep.
type teamWithheldSlot struct {
	fnHeader string
	pin      string
	cell     string
	why      string
}

var teamWithheldDeltaSlot = teamWithheldSlot{
	fnHeader: "function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {",
	pin:      "bothRanked?deltaText:'—'",
	cell:     "cmp-db-delta",
	why: "this is the cell that NULLIFIES #605: it was printed unconditionally, so a " +
		"below-floor row republished the withheld `Δ Org TIER` — and the delta is a pure " +
		"function of the two withheld headlines, so it leaks directly rather than additively",
}

var teamWithheldReadoutSlot = teamWithheldSlot{
	fnHeader: "function cmpRankedSide(has, ranked, tier) {",
	pin:      "if(!ranked){return'—';}",
	cell:     "cmp-db-ab",
	why: "the A→B readout states both raw TIERs; when the k-anonymity fold yields a single " +
		"cohort those two numbers ARE the two org headlines the card above just withheld",
}

// teamWithheldSlotViolations pins ONE withheld cell to the em dash. Two rules rather
// than one, deliberately: a single rule catching only the readout would have passed
// the very code that produced #613, whose delta cell was the worse leak.
func teamWithheldSlotViolations(js string, slot teamWithheldSlot) (bad []string, ok bool) {
	if v, arityOK := teamWithheldArityViolation(js); arityOK && v != "" {
		return []string{v}, true // a named finding, not a short read
	}
	body, found := jsBlockAfter(js, slot.fnHeader)
	if !found {
		return nil, false
	}
	// An explicit em dash, never a blank: a blank cell trains the reader to treat a
	// withheld number as a rendering bug, and a floor whose output looks broken stops
	// being credible.
	if !strings.Contains(squashSpace(jsStrip(body)), slot.pin) {
		bad = append(bad, "the withheld row's "+slot.cell+" cell has lost its gate (`"+slot.pin+
			"` is not in "+slot.fnHeader+") — "+slot.why)
	}
	return bad, true
}

// teamRankedPassthroughViolations is the CONTROL ARM. Without it the guard is
// satisfied by suppressing everything: hardcode both cells to '—' inside the shared
// builder and every rule above still passes, while a RANKED team row — which earned
// its digits — silently stops printing them. #613 revokes the display authority of a
// below-floor reading, not of every reading.
func teamRankedPassthroughViolations(js string) (bad []string, ok bool) {
	// 🔴 The ranked path a RANKED SIDE takes. This is the arm that makes the whole
	// guard non-vacuous: without it, `return '—';` unconditionally inside cmpRankedSide
	// satisfies every withholding rule in this file while a row that EARNED its digits
	// silently stops printing them. #613 revokes the display authority of a below-floor
	// reading, not of every reading.
	if side, sideOK := jsBlockAfter(js, "function cmpRankedSide(has, ranked, tier) {"); !sideOK {
		bad = append(bad, "cmpRankedSide not found — the per-side control arm cannot be scoped")
	} else {
		if !strings.Contains(side, "return num(tier).toFixed(1);") {
			bad = append(bad, "cmpRankedSide no longer returns the formatted tier on its RANKED "+
				"path; suppressing every side passes every withholding rule here and publishes "+
				"nothing at all, which is a different defect rather than a safer one")
		}
		if !strings.Contains(side, "return 'n/a';") {
			bad = append(bad, "cmpRankedSide no longer distinguishes an ABSENT side ('n/a') from "+
				"a WITHHELD one ('—'); collapsing them tells the reader we are holding back a "+
				"number that does not exist")
		}
	}

	body, found := jsBlockAfter(js,
		"function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {")
	if !found {
		return nil, false
	}
	for _, want := range []struct{ code, why string }{
		{"el('div', 'cmp-db-delta num ' + (bothRanked ? deltaCls : 'flat'),",
			"a both-ranked row must still print its Δ in the class it was handed — that is where " +
				"#277's significance colour reaches a developer row, and hardcoding 'flat' here " +
				"would drop it silently"},
		{"el('div', 'cmp-db-tag', tag)",
			"the status tag survives withholding — it is what keeps the row's shape and states " +
				"the verdict a sighted reader sees"},
		{"el('div', 'cmp-db-ab num', ab)",
			"a ranked row must still print its A→B readout, from the parameter it was handed"},
	} {
		if !strings.Contains(body, want.code) {
			bad = append(bad, "cmpRankedRowValue is missing `"+want.code+"` — "+want.why)
		}
	}
	if n := strings.Count(body, "val.appendChild("); n != 4 {
		bad = append(bad, "cmpRankedRowValue appends "+strconv.Itoa(n)+" children, want exactly 4 "+
			"(Δ, tag, A→B, and the conditional off-screen note) — the withheld and ranked states "+
			"must have the SAME shape, or the grid reflows between them and the row's absence "+
			"of digits reads as a broken render")
	}
	// Every el() call, pinned whole — the CONTAINER included. `el('div',
	// 'cmp-db-value', ab)` publishes the readout as the column's own textContent while
	// all three cells still render their pinned arguments, and the withheld path routes
	// through this same function: measured passing every rule above.
	elCalls, elOK := jsCallArgs(body, "el(")
	if !elOK {
		return nil, false
	}
	wantEls := []string{
		"'div','cmp-db-value'",
		"'div','cmp-db-deltanum'+(bothRanked?deltaCls:'flat'),bothRanked?deltaText:'—'",
		"'div','cmp-db-tag',tag",
		"'div','cmp-db-abnum',ab",
		"'span','cmp-db-sr',srNote",
	}
	if len(elCalls) != len(wantEls) {
		bad = append(bad, "cmpRankedRowValue makes "+strconv.Itoa(len(elCalls))+" el() calls, want "+
			strconv.Itoa(len(wantEls))+" (the column, its three cells and the off-screen note)")
	}
	for i, call := range elCalls {
		if i >= len(wantEls) {
			break
		}
		if got := squashSpace(call); got != wantEls[i] {
			bad = append(bad, "cmpRankedRowValue's el() call "+strconv.Itoa(i)+" is `el("+got+
				")`, want `el("+wantEls[i]+")` — EVERY row's column is built by this same "+
				"function, withheld or not, so an unpinned argument here (the container's "+
				"textContent above all) publishes on the path that is supposed to withhold")
		}
	}
	if jsBareReturnRe.MatchString(body) {
		bad = append(bad, "cmpRankedRowValue has a `return` at the end of a line — ASI makes what "+
			"follows dead code, and every row's column comes from here")
	}
	bad = append(bad, cmpAllowViolations2("cmpRankedRowValue", body,
		teamRowValueRootAllow, teamRowValuePropAllow)...)
	return bad, true
}

// TestDashboard_CompareTeamRowGateIsTheOnlyRouteToDigits owns the half that is
// COMMON to both leaks, so the two per-cell tests below can fail for their own
// reason and only their own reason.
//
// It owns: the gate call and all four of its arguments; what `bothRanked` MEANS
// (a hardcoded side or a disjunction leaks both cells while the argument is still
// spelled `bothRanked`); every el() call in the row builder, including the label,
// the tag and the container, which SURVIVE withholding and can publish the reading
// the value column just declined to; the caller; and the withheld column's scope.
//
// Splitting this out is not tidiness. When the gate broke, both per-cell tests
// failed and neither name told you which cell was at risk — which is the wrong
// signal from a guard whose entire purpose is that the two cells are separable.
func TestDashboard_CompareTeamRowGateIsTheOnlyRouteToDigits(t *testing.T) {
	_, js := get(t, jsPath)

	gate, ok := teamRowGateViolations(js)
	if !ok {
		t.Fatal("the #613 gate guard could not read buildTeamDumbbell / cmpRankedRowValue")
	}
	for _, v := range gate {
		t.Error(v)
	}
	scope, ok := teamWithheldScopeViolations(js)
	if !ok {
		t.Fatal("the #613 guard could not find cmpRankedSide at all")
	}
	for _, v := range scope {
		t.Error(v)
	}
}

// TestDashboard_CompareTeamRowWithholdsUnrankedDelta is the NAMED test for the
// FIRST of #613's two leaks — the one this issue's own text did not name and the one
// that nullifies #605. `cmp-db-delta` was printed unconditionally: with a ranked
// baseline beside an unranked selected window, `selected = baseline + Δ` recovers
// the withheld headline exactly, and `Δ` itself is the withheld `Δ Org TIER`.
//
// It asserts the DELTA SLOT and nothing else, so a failure here names one cell.
func TestDashboard_CompareTeamRowWithholdsUnrankedDelta(t *testing.T) {
	_, js := get(t, jsPath)

	slot, ok := teamWithheldSlotViolations(js, teamWithheldDeltaSlot)
	if !ok {
		t.Fatal("the #613 delta-cell rule could not read the withheld column")
	}
	for _, v := range slot {
		t.Error(v)
	}
}

// TestDashboard_CompareTeamRowWithholdsUnrankedReadout is the NAMED test for the
// SECOND leak, and it fails INDEPENDENTLY of the delta one: a guard that caught only
// one of the two would have passed the code that produced #613. Measured — restoring
// one slot fails this test or the delta test, never both.
func TestDashboard_CompareTeamRowWithholdsUnrankedReadout(t *testing.T) {
	_, js := get(t, jsPath)

	slot, ok := teamWithheldSlotViolations(js, teamWithheldReadoutSlot)
	if !ok {
		t.Fatal("the #613 readout rule could not read the withheld column")
	}
	for _, v := range slot {
		t.Error(v)
	}
}

// TestDashboard_CompareTeamRowStillPrintsRankedDigits is the CONTROL ARM, and only
// that. #613 revokes the display authority of a below-floor reading, not of every
// reading: hardcode both cells to '—' inside the shared builder and every
// withholding rule in this file still passes, while a ranked team row that earned
// its digits silently stops printing them.
func TestDashboard_CompareTeamRowStillPrintsRankedDigits(t *testing.T) {
	_, js := get(t, jsPath)

	bad, ok := teamRankedPassthroughViolations(js)
	if !ok {
		t.Fatal("cmpRankedRowValue not found — the #613 control arm cannot be scoped")
	}
	for _, v := range bad {
		t.Error(v)
	}
}

// TestDashboard_CompareLegendMatchesTeamRowBehaviour is the legend half, split out
// because the control arm's name did not mention it and a legend failure read as a
// rendering failure.
//
// The on-page note must describe what the chart now DOES. Its shared sentence used
// to promise that a below-floor side "still states its number" — still true of a
// developer row, and false of a team one since #613. A legend describing behaviour
// the code does not have is this issue's own defect class, one element over.
func TestDashboard_CompareLegendMatchesTeamRowBehaviour(t *testing.T) {
	_, js := get(t, jsPath)

	note, found := jsBlockAfter(js, "function compareNote(devMode) {")
	if !found {
		t.Fatal("compareNote not found")
	}
	// 🔴 ASI FIRST, because every other assertion here greps the function's SOURCE
	// TEXT. A newline after either arm's `return` parses fine, and `compareNote(false)`
	// then returns undefined — the legend sentence #613 exists to add is gone from the
	// page while its literal sits in dead code, satisfying the Contains check below.
	// That is exactly the failure this test is named for: a legend describing
	// behaviour the code does not have.
	if jsBareReturnRe.MatchString(note) {
		t.Error("compareNote has a `return` at the end of a line — ASI makes the sentence under " +
			"it unreachable, so the legend renders as undefined while its text still greps clean " +
			"from this file. The literal being present is not the same as the legend being shown")
	}
	// The promise must have MOVED into the developer arm, not merely survived: the
	// team arm below is what the shared sentence would otherwise contradict.
	// 🔴 GUARD THE INDEX. `note[:strings.Index(...)]` panics on -1, and a panic aborts
	// the whole TEST BINARY — every other guard in this package, including both #613
	// withholding tests, stops reporting. In a suite whose premise is "fail loudly and
	// specifically", that is the one failure mode that destroys the other signals.
	// Measured: rewriting the source as `if (devMode === true) {` was enough.
	// t.Fatalf, not a slice: a guard that cannot find the arm boundary has not read
	// its input and must say so rather than guess where the shared sentence ends.
	split := strings.Index(note, "if (devMode)")
	if split < 0 {
		t.Fatalf("compareNote does not contain `if (devMode)`, so this guard cannot tell the "+
			"SHARED sentence from the developer arm and every assertion below would be scoped to "+
			"the wrong text. Body: %s", squashSpace(note))
	}
	base := note[:split]
	// 🔴 INVERTED 2026-08-05. The rule this guard was written for no longer holds: it
	// required the withholding sentence to live in the TEAM arm, because #613's first
	// answer applied to team rows only and the shared sentence would have contradicted
	// the developer arm. Both grains now follow ONE rule, so the sentence belongs in
	// the SHARED base and a per-arm restatement is what would drift.
	if strings.Contains(note, "states its number") {
		t.Error("the compare legend still promises that a below-floor side states its number. " +
			"Since 2026-08-05 that is false at BOTH grains — such a side is neither placed nor " +
			"stated — and a legend describing behaviour the code does not have is this issue's " +
			"own defect class one element over")
	}
	if !strings.Contains(base, "neither placed on that scale nor stated as a number") {
		t.Error("the compare legend's SHARED sentence does not state the withholding rule. It " +
			"must live in the base, not in one arm: one rule now governs both grains, and a " +
			"per-arm restatement is exactly what drifts out of step with the other")
	}
	if strings.Contains(note, "states no number at all") {
		t.Error("the compare legend still carries the TEAM-ONLY phrasing of the withholding " +
			"rule. That wording implied developer rows behaved differently, which is no longer " +
			"true and is the contradiction this guard exists to prevent")
	}
}

// TestDashboard_CompareTeamWithholdIsNotSightedOnly pins the CSS half of criterion
// 4. The withheld row states its verdict in off-screen TEXT, and that text is only
// real if the class clips it rather than removing it from the accessibility tree —
// and only free of reflow if it is taken out of the value cell's flex column.
func TestDashboard_CompareTeamWithholdIsNotSightedOnly(t *testing.T) {
	_, style := get(t, "/")
	if !regexp.MustCompile(`(?s)\.cmp-db-sr\s*\{[^}]*clip-path`).MatchString(style) {
		t.Error(".cmp-db-sr has no clip-path rule — the screen-reader text is either visible " +
			"(a fourth line under the tag) or has no rule at all")
	}
	if regexp.MustCompile(`(?s)\.cmp-db-sr\s*\{[^}]*display\s*:\s*none`).MatchString(style) {
		t.Error(".cmp-db-sr uses display:none, which removes the text from the accessibility " +
			"tree — the one place it is meant to exist")
	}
	if !regexp.MustCompile(`(?s)\.cmp-db-sr\s*\{[^}]*position\s*:\s*absolute`).MatchString(style) {
		t.Error(".cmp-db-sr is not position:absolute, so the off-screen span stays in the " +
			"value cell's flex column and opens a fourth line — the row reflows, which is the " +
			"layout stability that got the collapse-the-row option rejected")
	}
}

// TestDashboard_CompareTeamWithholdGuardCatchesTheDefect is the guard on the guard.
// Every rule above is exercised against source that BREAKS it — including the two
// one-cell mutants that prove the delta and readout rules fail independently, and a
// clean fixture that must stay silent so the suite cannot be satisfied by a rule
// that fires on everything.
func TestDashboard_CompareTeamWithholdGuardCatchesTheDefect(t *testing.T) {
	// A minimal but REAL fixture: the four functions the rules scope to, in the
	// shapes the asset uses.
	// The CALLER is part of the fixture: withholding inside the row builder is worth
	// nothing if the loop can append a second element beside the row it just built.
	// So is abReadout — it is passed as an ARGUMENT, so it RUNS on the withheld path;
	// the gate discards its return value, never its side effects.
	// 🔴 The pre-#613 four-argument abReadout used to be defined HERE as well as in
	// sideFn. The drift check looks functions up by their exact header, so it could not
	// see the dead copy at all — a second definition of a function this branch changed,
	// sitting in the fixture, invisible to the rule that exists to catch exactly that.
	// The real six-argument one lives in sideFn and is drift-checked.
	const builderHead = `function renderCompare(data) {
  var devMode = (data.mode === 'developer');
  var rows = (devMode ? data.developers : data.teams) || [];
  if (rows.length === 0 && !data.total) {
    showStatus('No data for the selected periods.');
    return;
  }

  $('compare-view').style.display = 'block';
  renderCompareWindows(data);
  renderCompareTotal(data.total);

  // A single shared scale (max TIER across every row-side and the org total) so
  // dot positions are comparable WITHIN the compare view -- the dumbbell analogue
  // of the per-panel yield-bar scale.
  var scale = compareScale(rows, data.total);
  var host = $('cmp-rows');
  host.textContent = '';
  if (rows.length === 0) {
    host.appendChild(el('div', 'cmp-empty',
      'No per-' + (devMode ? 'developer' : 'team') + ' rows in these periods; see the org-level change above.'));
  } else {
    for (var i = 0; i < rows.length; i++) {
      host.appendChild(devMode ? buildDevDumbbell(rows[i], scale) : buildTeamDumbbell(rows[i], scale));
    }
  }
  setText('cmp-note', compareNote(devMode));
  showStatus('');
}

function buildTeamDumbbell(row, scale) {
  var hasA = !!row.a, hasB = !!row.b;
  var d = num(row.delta_tier);
  var aRanked = !!(row.a && row.a.ranked);
  var bRanked = !!(row.b && row.b.ranked);
  var bothRanked = aRanked && bRanked;
  var plotA = hasA && aRanked;
  var plotB = hasB && bRanked;
  var fracA = plotA ? num(row.a.tier) / scale : 0;
  var fracB = plotB ? num(row.b.tier) / scale : 0;

  var rowEl = el('div', 'cmp-db-row');
  rowEl.appendChild(el('div', 'cmp-db-label' + (bothRanked ? '' : ' below'), row.team || 'other'));
  rowEl.appendChild(buildDumbbellTrack(plotA, fracA, plotB, fracB, '', 'insignificant'));

  var tag = bothRanked ? 'aggregate — not tested' : BELOW_FLOOR_TAG;
`
	const builderCall = `  rowEl.appendChild(cmpRankedRowValue(tag, 'flat', bothRanked,
    signStr(d) + Math.abs(d).toFixed(1),
    abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier),
    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));
  return rowEl;
}
`
	const gateFn = `
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num ' + (bothRanked ? deltaCls : 'flat'),
    bothRanked ? deltaText : '—'));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', ab));
  if (srNote) { val.appendChild(el('span', 'cmp-db-sr', srNote)); }
  return val;
}
`
	const sideFn = `
function cmpRankedSide(has, ranked, tier) {
  if (!has) { return 'n/a'; }
  if (!ranked) { return '—'; }
  return num(tier).toFixed(1);
}

function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {
  return cmpRankedSide(hasA, aRanked, aTier) + ' → ' + cmpRankedSide(hasB, bRanked, bTier);
}
`
	const noteFn = `
function withheldSidesNote(withheldA, withheldB) {
  if (!withheldA && !withheldB) { return ''; }
  var which = !withheldA ? 'the selected period' : (!withheldB ? 'the baseline period' : 'both periods');
  return 'withheld: ' + BELOW_FLOOR_TAG + ' — insufficient evidence to rank ' + which;
}
`
	clean := builderHead + builderCall + gateFn + sideFn + noteFn

	// 🔴 DRIFT. Every mutant below is a mutation of hand-copied text. If dashboard.js
	// changes shape and this copy does not, all of them quietly become mutations of
	// DEAD TEXT and "clean fixture is accepted" starts certifying a shape the product
	// no longer has — the suite stays green and proves nothing. Assert the four
	// functions here are still the four functions that ship.
	//
	// Compared squashed: jsBlockAfter strips comments, and the shipped bodies carry
	// comments this fixture deliberately omits, so the blank lines left behind differ.
	// The whitespace-sensitive rules run against the REAL asset unsquashed, so nothing
	// is lost — this check answers "same shape", not "same bytes".
	t.Run("fixture matches the shipped asset", func(t *testing.T) {
		_, shipped := get(t, jsPath)
		for _, fn := range []string{
			"function buildTeamDumbbell(row, scale) {",
			"function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {",
			"function cmpRankedSide(has, ranked, tier) {",
			"function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {",
			"function withheldSidesNote(withheldA, withheldB) {",
			// renderCompare is MUTATED by two rows below but was absent from this list, so a
			// shape change in the shipped caller would have turned both into mutations of
			// dead fixture text.
			"function renderCompare(data) {",
		} {
			want, okShipped := jsBlockAfter(shipped, fn)
			got, okFixture := jsBlockAfter(clean, fn)
			if !okShipped || !okFixture {
				t.Errorf("%s: shipped=%v fixture=%v — the fixture no longer contains the same "+
					"functions as the asset, so every mutant below mutates dead text", fn,
					okShipped, okFixture)
				continue
			}
			if squashSpace(want) != squashSpace(got) {
				t.Errorf("%s has DRIFTED from the shipped asset.\n shipped: %s\n fixture: %s\n"+
					"Update the fixture — until you do, every mutant in this test is a mutation "+
					"of text the product does not have, and the whole table proves nothing.",
					fn, squashSpace(want), squashSpace(got))
			}
		}
	})

	// The clean fixture must be silent under EVERY rule, or a rule that fires on
	// everything would "catch" every mutant below while proving nothing.
	t.Run("clean fixture is accepted", func(t *testing.T) {
		for _, r := range []struct {
			name string
			run  func(string) ([]string, bool)
		}{
			{"gate", teamRowGateViolations},
			{"withheld scope", teamWithheldScopeViolations},
			{"delta cell", func(js string) ([]string, bool) {
				return teamWithheldSlotViolations(js, teamWithheldDeltaSlot)
			}},
			{"readout cell", func(js string) ([]string, bool) {
				return teamWithheldSlotViolations(js, teamWithheldReadoutSlot)
			}},
			{"ranked passthrough", teamRankedPassthroughViolations},
		} {
			bad, ok := r.run(clean)
			if !ok {
				t.Errorf("%s rule could not read the clean fixture", r.name)
			}
			if len(bad) != 0 {
				t.Errorf("%s rule fires on the clean fixture: %v", r.name, bad)
			}
		}
	})

	// The two ONE-CELL mutants. Each must be caught by its own rule and MISSED by the
	// other, or the pair is one rule wearing two names — and a single rule catching
	// only the readout would have passed the code that produced this issue.
	t.Run("one-cell leaks fail independently", func(t *testing.T) {
		// The Δ gate deleted: the cell prints unconditionally, in the caller's class.
		// This is the #605-nullifying leak, restored.
		leakDelta := builderHead + builderCall + `
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num ' + deltaCls, deltaText));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', ab));
  if (srNote) { val.appendChild(el('span', 'cmp-db-sr', srNote)); }
  return val;
}
` + sideFn + noteFn
		// The per-side gate deleted: an unranked side prints its digits again, which is
		// #603's "muted but printed" treatment restored one grain down.
		leakReadout := builderHead + builderCall + gateFn + `
function cmpRankedSide(has, ranked, tier) {
  if (!has) { return 'n/a'; }
  return num(tier).toFixed(1);
}

function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {
  return cmpRankedSide(hasA, aRanked, aTier) + ' → ' + cmpRankedSide(hasB, bRanked, bTier);
}
` + noteFn

		for _, tt := range []struct {
			name         string
			src          string
			caught       teamWithheldSlot
			mustNotCatch teamWithheldSlot
		}{
			{"delta restored", leakDelta, teamWithheldDeltaSlot, teamWithheldReadoutSlot},
			{"readout restored", leakReadout, teamWithheldReadoutSlot, teamWithheldDeltaSlot},
		} {
			bad, ok := teamWithheldSlotViolations(tt.src, tt.caught)
			if !ok || len(bad) == 0 {
				t.Errorf("%s: the %s rule reports CLEAN on a mutant that republishes that cell",
					tt.name, tt.caught.cell)
			}
			other, ok := teamWithheldSlotViolations(tt.src, tt.mustNotCatch)
			if !ok {
				t.Errorf("%s: the %s rule could not read the mutant", tt.name, tt.mustNotCatch.cell)
			}
			if len(other) != 0 {
				t.Errorf("%s: the %s rule ALSO fires, so the two rules are not independent and "+
					"one of them has never been the sole defence of anything: %v",
					tt.name, tt.mustNotCatch.cell, other)
			}
		}
	})

	// Everything else: each mutant, and the rule that must be its sole defence.
	for _, tt := range []struct {
		name string
		src  string
		rule func(string) ([]string, bool)
	}{
		{
			// The pre-#613 code, verbatim in shape: the row builds its own cells.
			name: "revert — the row builds its value column inline",
			src: builderHead + `  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num flat', signStr(d) + Math.abs(d).toFixed(1)));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier)));
  rowEl.appendChild(val);
  return rowEl;
}
` + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the gate is hardcoded open",
			src: builderHead + `  rowEl.appendChild(cmpRankedRowValue(tag, 'flat', true,
    signStr(d) + Math.abs(d).toFixed(1),
    abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier)));
  return rowEl;
}
` + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// One side's verdict is not the row's: a ranked baseline beside an unranked
			// selected window is exactly the reconstruction #605 was ruled on.
			name: "the gate reads one side only",
			src: builderHead + `  rowEl.appendChild(cmpRankedRowValue(tag, 'flat', aRanked,
    signStr(d) + Math.abs(d).toFixed(1),
    abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier)));
  return rowEl;
}
` + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the gate forwards a value on the unranked path",
			src: builderHead + builderCall + `
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num ' + (bothRanked ? deltaCls : 'flat'),
    bothRanked ? deltaText : ab));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', ab));
  if (srNote) { val.appendChild(el('span', 'cmp-db-sr', srNote)); }
  return val;
}
` + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// A SECOND gated call: the row gets two value columns, and the extra one is
			// hardcoded open. (The comment here previously described an ASI mutant, which
			// is a different row further down — stale residue, removed.)
			name: "an extra gated call slips a second value column onto the row",
			src: builderHead + mustTrimSuffix(builderCall, "  return rowEl;\n}\n") +
				`  rowEl.appendChild(cmpRankedRowValue(tag, 'flat', true, signStr(d) + Math.abs(d).toFixed(1), abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier)));
  return rowEl;
}
` + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// A sink that is not a cell at all — the class of bypass that walked past every
			// per-call rule on #605.
			name: "the row leaks through a title attribute",
			src: builderHead + `  rowEl.title = 'TIER ' + num(row.a.tier).toFixed(1);
` + builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the row re-derives the reading from raw sums",
			src: builderHead + `  var leak = el('div', 'cmp-db-tag', String(row.a.weighted_points / row.a.total_cost_usd));
  rowEl.appendChild(leak);
` + builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the row uses a template literal",
			src: builderHead + "  var t = `${num(row.a.tier).toFixed(1)}`;\n" + builderCall +
				gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the row uses computed member access",
			src: builderHead + "  var t = row['a']['tier'];\n" + builderCall +
				gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the row hides a leak behind a non-ASCII identifier",
			src: builderHead + "  var tıer = num(row.a.tier);\n  rowEl.appendChild(el('div', 'cmp-db-tag', tıer));\n" +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "tag is reassigned after the verdict set it",
			src: builderHead + `  tag = 'aggregate — not tested';
` + builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// WHITESPACE INSIDE A LITERAL — invisible to every squashed pin.
			name: "the muting class loses its leading space (#603's defect verbatim)",
			src: strings.Replace(builderHead, "(bothRanked ? '' : ' below')",
				"(bothRanked ? '' : 'below')", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// 🔴 The two measured survivors of the FIRST caller rule. Both name no `.tier`
			// and publish through a sink that is not appendChild, so a substring ban plus an
			// append count saw neither.
			name: "the caller re-derives TIER and publishes it through setText",
			src: strings.Replace(builderHead, "  showStatus('');\n}\n\nfunction buildTeamDumbbell",
				"  showStatus('');\n  setText('cmp-note', String(rows[0].a.weighted_points / rows[0].a.total_cost_usd));\n}\n\nfunction buildTeamDumbbell", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the caller reads a payload field that is not on its allowlist",
			src: strings.Replace(builderHead, "  showStatus('');\n}\n\nfunction buildTeamDumbbell",
				"  showStatus('');\n  setText('cmp-note', String(rows[0].a.weighted_points));\n}\n\nfunction buildTeamDumbbell", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// abReadout runs on the WITHHELD path — it is an argument, so the gate discards
			// its return value and never its side effects.
			name: "abReadout gains a sink and publishes on the withheld path",
			src: builderHead + builderCall + gateFn + strings.Replace(sideFn,
				"  return cmpRankedSide(hasA, aRanked, aTier)",
				"  document.title = num(aTier).toFixed(1);\n  return cmpRankedSide(hasA, aRanked, aTier)", 1) +
				noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "abReadout hides a reading in a template literal",
			src: builderHead + builderCall + gateFn + strings.Replace(sideFn,
				"cmpRankedSide(hasA, aRanked, aTier)",
				"`${num(aTier).toFixed(1)}`", 1) +
				noteFn,
			rule: teamRowGateViolations,
		},
		{
			// Not a typo — a product defect. The class list becomes `in` + `significant`,
			// .cmp-db-connect.insignificant stops matching, and the connector renders as the
			// SOLID ACCENT BAR: every aggregate row asserts a significant move (#277).
			name: "the connector class splits into two, asserting a significant move",
			src: strings.Replace(builderHead, "fracB, '', 'insignificant'", "fracB, '', 'in significant'", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the method caveat loses its internal spaces",
			src: strings.Replace(builderHead, "'aggregate — not tested'", "'aggregate—nottested'", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// THE GATE'S INPUT. Both of these leak every below-floor team row while the
			// gate argument is still the identifier `bothRanked`.
			name: "bothRanked is a disjunction, so one ranked side unlocks the other",
			src: strings.Replace(builderHead, "var bothRanked = aRanked && bRanked;",
				"var bothRanked = aRanked || bRanked;", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "a side's verdict is hardcoded true (#603's defect, again)",
			src: strings.Replace(builderHead, "var bRanked = !!(row.b && row.b.ranked);",
				"var bRanked = true;", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "a side's verdict is reassigned after it was set",
			src: strings.Replace(builderHead, "var bothRanked = aRanked && bRanked;",
				"aRanked = true;\n  var bothRanked = aRanked && bRanked;", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// ASI in the ROW BUILDER, not the gate: `rowEl` becomes unreachable, the builder
			// returns undefined, and renderCompare's appendChild throws. Fails closed rather
			// than leaking, but every squashed pin in teamRowGateViolations is blind to it.
			name: "ASI — `return` alone on its line in the row builder",
			src: builderHead + strings.Replace(builderCall, "  return rowEl;", "  return\n    rowEl;", 1) +
				gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// The sink one stack frame ABOVE everything the other rules scope.
			name: "the CALLER appends a leaking cell beside each row",
			src: strings.Replace(builderHead,
				"buildTeamDumbbell(rows[i], scale));",
				"buildTeamDumbbell(rows[i], scale));\n      "+
					"host.appendChild(el('div', 'cmp-db-tag', num(rows[i].a.tier).toFixed(1)));", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// 🔴 MEASURED SURVIVING the first version of this guard, on the real asset. The
			// squashed body is byte-identical to the pinned one, because squashSpace deletes
			// the newline the defect is made of.
			name: "ASI — `return` alone on its line makes the withheld call dead code",
			src: builderHead + builderCall + `
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num ' + (bothRanked ? deltaCls : 'flat'),
    bothRanked ? deltaText : '—'));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', ab));
  if (srNote) { val.appendChild(el('span', 'cmp-db-sr', srNote)); }
  return
  val;
}
` + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// THE SURVIVING SURFACES. Each of these four leaks a digit while withholding
			// both audited cells, and each was measured passing the first draft of this
			// guard — allowlist, appendChild count, gate and all four argument pins.
			name: "the row CONTAINER publishes the reading as its own textContent",
			src: strings.Replace(builderHead, "el('div', 'cmp-db-row')",
				"el('div', 'cmp-db-row', num(row.a.tier).toFixed(1))", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the LABEL cell carries the digits (it is never withheld)",
			src: strings.Replace(builderHead, "row.team || 'other'",
				"(row.team || 'other') + ' ' + num(row.a.tier).toFixed(1)", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// Keeps the #603 assertion's substring intact and prints the withheld Δ anyway.
			name: "the TAG cell carries the withheld delta",
			src: strings.Replace(builderHead, ": BELOW_FLOOR_TAG;",
				": BELOW_FLOOR_TAG + ' ' + signStr(d) + Math.abs(d).toFixed(1);", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			name: "the TRACK's class argument carries the reading",
			src: strings.Replace(builderHead, "plotB, fracB, '', 'insignificant'",
				"plotB, fracB, '', 'insignificant' + num(row.a.tier)", 1) +
				builderCall + gateFn + sideFn + noteFn,
			rule: teamRowGateViolations,
		},
		{
			// EVERY row's column comes from cmpRankedRowValue now, withheld or not, so the
			// COLUMN's own textContent publishes on exactly the path meant to withhold.
			name: "the value column publishes as its own textContent",
			src: builderHead + builderCall +
				strings.Replace(gateFn, "el('div', 'cmp-db-value')",
					"el('div', 'cmp-db-value', ab)", 1) + sideFn + noteFn,
			rule: teamRankedPassthroughViolations,
		},
		{
			// The per-side gate gains a parameter so a caller can smuggle a second value in.
			// teamWithheldArityViolation names it rather than letting it die as a short read.
			name: "the per-side gate takes an extra value parameter",
			src: builderHead + builderCall + gateFn + `
function cmpRankedSide(has, ranked, tier, raw) {
  if (!has) { return 'n/a'; }
  if (!ranked) { return raw; }
  return num(tier).toFixed(1);
}

function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {
  return cmpRankedSide(hasA, aRanked, aTier, aTier) + ' → ' + cmpRankedSide(hasB, bRanked, bTier, bTier);
}
` + noteFn,
			rule: teamWithheldScopeViolations,
		},
		{
			// 🔴 THE 'n/a' / '—' COLLAPSE. Both states become one glyph, so a reader cannot
			// tell "this side has no data" from "this side has data we will not show" —
			// and the off-screen note then contradicts the digits printed beside it.
			name: "the per-side gate collapses absent into withheld",
			src: builderHead + builderCall + gateFn + `
function cmpRankedSide(has, ranked, tier) {
  if (!has || !ranked) { return '—'; }
  return num(tier).toFixed(1);
}

function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {
  return cmpRankedSide(hasA, aRanked, aTier) + ' → ' + cmpRankedSide(hasB, bRanked, bTier);
}
` + noteFn,
			rule: teamWithheldScopeViolations,
		},
		{
			// The off-screen note stops naming WHICH side, which is the half-(a) case: an
			// asymmetric withholding described by a sentence that cannot express asymmetry.
			name: "the off-screen note stops naming the side",
			src: builderHead + builderCall + gateFn + sideFn + `
function withheldSidesNote(withheldA, withheldB) {
  if (!withheldA && !withheldB) { return ''; }
  return 'withheld: ' + BELOW_FLOOR_TAG + ' — insufficient evidence to rank this window';
}
`,
			rule: teamWithheldScopeViolations,
		},
		{
			// The note is returned unconditionally, so every ranked row grows an off-screen
			// span and the span's PRESENCE stops meaning anything was withheld.
			name: "the off-screen note is unconditional",
			src: builderHead + builderCall + gateFn + sideFn + `
function withheldSidesNote(withheldA, withheldB) {
  var which = !withheldA ? 'the selected period' : (!withheldB ? 'the baseline period' : 'both periods');
  return 'withheld: ' + BELOW_FLOOR_TAG + ' — insufficient evidence to rank ' + which;
}
`,
			rule: teamWithheldScopeViolations,
		},
		{
			// The control arm's own mutant: suppress EVERYTHING and every withholding rule
			// above still passes, while a ranked row silently stops printing its digits.
			name: "suppress everything — the ranked row loses its digits too",
			src: builderHead + builderCall + gateFn + `
function cmpRankedSide(has, ranked, tier) {
  if (!has) { return 'n/a'; }
  return '—';
}

function abReadout(hasA, aRanked, aTier, hasB, bRanked, bTier) {
  return cmpRankedSide(hasA, aRanked, aTier) + ' → ' + cmpRankedSide(hasB, bRanked, bTier);
}
` + noteFn,
			rule: teamRankedPassthroughViolations,
		},
		{
			// The withheld Δ inherits the CALLER's class, so an em dash renders in confident
			// green or red — a direction asserted about a number we declined to publish.
			name: "the withheld delta keeps the caller's direction class",
			src: builderHead + builderCall + `
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-delta num ' + deltaCls, bothRanked ? deltaText : '—'));
  val.appendChild(el('div', 'cmp-db-tag', tag));
  val.appendChild(el('div', 'cmp-db-ab num', ab));
  if (srNote) { val.appendChild(el('span', 'cmp-db-sr', srNote)); }
  return val;
}
` + sideFn + noteFn,
			rule: teamRankedPassthroughViolations,
		},
		{
			name: "the shared builder reflows the row instead of keeping its shape",
			src: builderHead + builderCall + `
function cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText, ab, srNote) {
  var val = el('div', 'cmp-db-value');
  val.appendChild(el('div', 'cmp-db-tag', tag));
  return val;
}
` + sideFn + noteFn,
			rule: teamRankedPassthroughViolations,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 🔴 THE MUTATION MUST HAVE APPLIED. Several rows here are built with
			// strings.Replace against a fixture constant; when the text they target moves
			// to a different constant, the Replace silently becomes a no-op and `src` is
			// byte-identical to the clean fixture. The rule then reports CLEAN — correctly,
			// about the CLEAN SOURCE — and the row is scored as "the guard missed a leak"
			// or, worse, quietly passes in the inverse direction. A mutation result means
			// nothing until the mutation is proven to have applied (false-green ledger 8).
			// This caught two live rows the day it was added: both abReadout mutants had
			// stopped applying when abReadout moved out of builderHead.
			if tt.src == clean {
				t.Fatal("this mutant is byte-identical to the clean fixture — the mutation " +
					"never applied, so the verdict below would be about nothing. Re-derive the " +
					"fixture text this row targets.")
			}
			bad, ok := tt.rule(tt.src)
			// EVERY row must die on a NAMED violation, not on a short read. `ok == false`
			// means "the guard could not read its input", which is the same signal a benign
			// rename produces — it fails loudly but tells the reader nothing about WHICH
			// defect they introduced. One row used to land there (adding a parameter to the
			// withheld column); teamWithheldArityViolation now names it instead.
			if !ok {
				t.Error("the guard could not READ this mutant, so it dies as a short read rather " +
					"than a named finding — indistinguishable from a rename")
				return
			}
			if len(bad) == 0 {
				t.Error("the #613 guard reports CLEAN on a mutant that republishes a below-floor " +
					"reading (or suppresses a ranked one)")
			}
		})
	}
}

// TestDashboard_JSStripHandlesRegexLiterals is the guard on the guard, and it
// closes a MEASURED desync that silenced everything scoped below one line of the
// asset.
//
// dashboard.js:1674 is `if (/[",\r\n]/.test(s)) { return '"' + … }` — a regex whose
// character class holds a double quote. jsStrip and jsStripStrings read that `"` as
// opening a string literal. It never closes on that line, so from there to the end
// of the file their quote state was INVERTED: `//` comments stopped being stripped
// and string literals were blanked in antiphase.
//
// Measured on the tree before this fix: every comment from line 1678 onward
// survived jsStrip, and jsStripStrings returned ok=false for renderCompareTotal —
// i.e. the #605 guard could not read its input at all. It failed loudly, which is
// the behaviour those helpers were designed for; a softer guard scoped below line
// 1674 would instead have been auditing comment prose and reporting clean.
func TestDashboard_JSStripHandlesRegexLiterals(t *testing.T) {
	// (a) The exact construct, in isolation, with the answer known by inspection.
	const src = "function csv(s) {\n" +
		"  if (/[\",\\r\\n]/.test(s)) { return '\"' + s.replace(/\"/g, '\"\"') + '\"'; }\n" +
		"  // this comment must be stripped\n" +
		"  return s;\n" +
		"}\n"
	stripped := jsStrip(src)
	if strings.Contains(stripped, "must be stripped") {
		t.Error("jsStrip left a // comment after a regex literal containing a quote — its quote " +
			"state is inverted from that regex onward, and every walker built on it is reading " +
			"comments as code")
	}
	if _, ok := jsStripStrings(stripped); !ok {
		t.Error("jsStripStrings reports an unterminated literal on well-formed source: it read " +
			"the quote inside the regex character class as opening a string")
	}

	// (b) The REAL asset, end to end. This is the assertion that fails if the
	// heuristic ever regresses on the file the guards actually run against.
	_, js := get(t, jsPath)
	for i, line := range strings.Split(jsStrip(js), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			t.Fatalf("jsStrip left comment text at line %d of the served asset: %q. Its quote "+
				"state has desynced, so every guard scoped below that point is auditing prose.",
				i+1, trimmed)
		}
	}
	if _, ok := jsStripStrings(jsStrip(js)); !ok {
		t.Error("jsStripStrings cannot read the served asset end to end")
	}

	// (c) DIVISION must not be mistaken for a regex — the failure in the other
	// direction, which would silently swallow real code (and with it any violation
	// inside it). The heuristic errs toward division by design, so these must survive.
	for _, div := range []string{
		"var t = points / (cost / 1000);",
		"var f = a[i] / b;",
		"var g = fn(x) / 2;",
		"var h = 1 / 2 / 3;",
	} {
		if got := jsStrip(div); got != div {
			t.Errorf("jsStrip mangled a division as a regex literal:\n  in:  %s\n  out: %s", div, got)
		}
		code, ok := jsStripStrings(div)
		if !ok {
			t.Errorf("jsStripStrings failed on %q", div)
			continue
		}
		if !strings.Contains(code, "/") {
			t.Errorf("jsStripStrings swallowed a division as a regex literal: %q -> %q. The "+
				"operator bans in the #502/#605 guards read this output, so a swallowed `/` is "+
				"a re-derivation the guard can no longer see.", div, code)
		}
	}
}

// TestDashboard_CompareRenderingGuardCatchesTheDefect is the control arm for the
// #605 RENDERING-INTEGRITY half — the one that did not exist, and whose absence
// was measured costing two complete reverts.
//
// The publication half (renderCompareTotal) shipped with eighteen mutant fixtures.
// The rendering half shipped with none, and a review found that both of its rules
// could be fully reverted with the suite green: the scale rule inspected only the
// `a` side (one of its two banned spellings naming a local the same commit had
// deleted), and the plot rule pinned a prefix, so `hasA && true` satisfied it.
// Neither hole was subtle. Neither was findable without fixtures.
func TestDashboard_CompareRenderingGuardCatchesTheDefect(t *testing.T) {
	_, js := get(t, jsPath)

	// Sanity: the real asset is clean under both rules, or every "must catch" below
	// would be measuring a guard that fires on everything.
	if v := compareScaleViolations(js); len(v) != 0 {
		t.Fatalf("the scale rule fires on the real asset: %v", v)
	}
	if v := dumbbellPlotViolations(js); len(v) != 0 {
		t.Fatalf("the plot rule fires on the real asset: %v", v)
	}

	// Each mutation is applied to the REAL source, so a fixture can never drift away
	// from the code it claims to model — and a rename that breaks the mutation shows
	// up as "did not change the source" rather than as a silent pass.
	mustCatch := []struct {
		name, from, to string
		rule           func(string) []string
	}{
		{
			// MEASURED REVERT. The b side of a row, ungated.
			name: "a row's selected side bypasses rankedTier",
			from: "mx = Math.max(mx, rankedTier(rows[i].a), rankedTier(rows[i].b));",
			to:   "mx = Math.max(mx, rankedTier(rows[i].a), num(rows[i].b && rows[i].b.tier));",
			rule: compareScaleViolations,
		},
		{
			// MEASURED REVERT, and the worst of the two: this is the org total's SELECTED
			// window — the canonical 2.8e8 — back in the shared denominator.
			name: "the org total's selected side bypasses rankedTier",
			from: "if (total) { mx = Math.max(mx, rankedTier(total.a), rankedTier(total.b)); }",
			to:   "if (total) { mx = Math.max(mx, rankedTier(total.a), num(total.b && total.b.tier)); }",
			rule: compareScaleViolations,
		},
		{
			name: "the org total is dropped from the scale entirely and read raw",
			from: "if (total) { mx = Math.max(mx, rankedTier(total.a), rankedTier(total.b)); }",
			to:   "if (total) { mx = Math.max(mx, num(total.a.tier), num(total.b.tier)); }",
			rule: compareScaleViolations,
		},
		{
			// MEASURED REVERT. A flag ANDed with a constant is not a gate, and the prefix
			// pin could not tell the difference.
			//
			// 🔴 Both builders now write these two lines IDENTICALLY (#613 hoisted the dev
			// builder's inline `!!row.a.ranked` to the shared identifiers). strings.Replace
			// with n=1 takes the FIRST match, and buildDevDumbbell precedes
			// buildTeamDumbbell in the asset — so a bare `var plotA = hasA && aRanked;`
			// fixture would mutate the DEV builder in BOTH rows here, and the team arm
			// would silently never be exercised while reporting a pass. Each fixture is
			// therefore anchored on its own builder's preceding comment line.
			name: "dev plot flags ANDed with a constant",
			from: "  // what the text declined to state.\n  var plotA = hasA && aRanked;\n  var plotB = hasB && bRanked;",
			to:   "  // what the text declined to state.\n  var plotA = hasA && true;\n  var plotB = hasB && true;",
			rule: dumbbellPlotViolations,
		},
		{
			// The same revert in the team builder, which is #603's second location.
			name: "team plot flags ANDed with a constant",
			from: "  // channels — which is the identity dumbbellPlotViolations now derives and enforces.\n  var plotA = hasA && aRanked;\n  var plotB = hasB && bRanked;",
			to:   "  // channels — which is the identity dumbbellPlotViolations now derives and enforces.\n  var plotA = hasA && true;\n  var plotB = hasB && true;",
			rule: dumbbellPlotViolations,
		},
		{
			// 🔴 THE DEFECT THIS RULING CLOSED, as a mutant. Reverting publication to the
			// row-level conjunction leaves plotting per side — the exact state in which a
			// '—'/'—' row still placed a dot whose position recovered the withheld number.
			// Nothing else in this file catches it: every other rule here is about the
			// SCALE or the DOT, and both stay correct under this mutation.
			name: "dev publication reverts to the row-level conjunction",
			from: "abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier),\n    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));\n  return rowEl;\n}\n\nfunction buildTeamDumbbell",
			to:   "abReadout(hasA, bothRanked, row.a && row.a.tier, hasB, bothRanked, row.b && row.b.tier),\n    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));\n  return rowEl;\n}\n\nfunction buildTeamDumbbell",
			rule: dumbbellPlotViolations,
		},
		{
			// The same revert in the team builder.
			name: "team publication reverts to the row-level conjunction",
			from: "abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier),\n    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));\n  return rowEl;\n}\n\n// --- A dumbbell row's value column",
			to:   "abReadout(hasA, bothRanked, row.a && row.a.tier, hasB, bothRanked, row.b && row.b.tier),\n    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));\n  return rowEl;\n}\n\n// --- A dumbbell row's value column",
			rule: dumbbellPlotViolations,
		},
		{
			name: "dev dots positioned from the has-data flags again",
			from: "  var fracA = plotA ? num(row.a.tier) / scale : 0;\n  var fracB = plotB ? num(row.b.tier) / scale : 0;\n\n  var rowEl = el('div', 'cmp-db-row');\n  var label",
			to:   "  var fracA = hasA ? num(row.a.tier) / scale : 0;\n  var fracB = hasB ? num(row.b.tier) / scale : 0;\n\n  var rowEl = el('div', 'cmp-db-row');\n  var label",
			rule: dumbbellPlotViolations,
		},
		{
			// The stated cause on a one-sided below-floor row — an artifact #605 itself
			// introduced by no longer plotting unranked sides.
			name: "a one-sided below-floor row loses its stated cause",
			from: "    if ((hasA && !plotA) || (hasB && !plotB)) {\n      tag = BELOW_FLOOR_TAG + ', ' + tag;\n    }\n",
			to:   "",
			rule: dumbbellPlotViolations,
		},
		{
			name: "the track is handed the has-data flags rather than the plottable ones",
			from: "rowEl.appendChild(buildDumbbellTrack(plotA, fracA, plotB, fracB, bClass, connectClass));",
			to:   "rowEl.appendChild(buildDumbbellTrack(hasA, fracA, hasB, fracB, bClass, connectClass));",
			rule: dumbbellPlotViolations,
		},
	}
	for _, tt := range mustCatch {
		t.Run(tt.name, func(t *testing.T) {
			// Count, not Contains. Both builders now write several of these lines
			// IDENTICALLY, and strings.Replace(..., 1) takes the FIRST — so a non-unique
			// anchor silently mutates the wrong builder twice and the other arm is never
			// exercised while reporting a pass. That is the failure these two plot-flag
			// fixtures exist because of; Contains cannot prevent it recurring.
			if n := strings.Count(js, tt.from); n != 1 {
				t.Fatalf("the source this fixture mutates occurs %d times, want exactly 1:\n%q\n"+
					"Re-derive the fixture rather than deleting it — a mutation that does not "+
					"apply exactly once proves nothing.", n, tt.from)
			}
			mutated := strings.Replace(js, tt.from, tt.to, 1)
			if mutated == js {
				t.Fatal("mutation did not change the source")
			}
			if v := tt.rule(mutated); len(v) == 0 {
				t.Error("guard reported CLEAN on a compare chart that puts an unranked reading " +
					"back into the shared scale, or plots one on it")
			}
		})
	}
}

// --- The DEVELOPER row's gate (#613 half b, 2026-08-05) -----------------------
//
// devRowGateViolations is the mirror of teamRowGateViolations, and it exists because
// the ruling made buildDevDumbbell a SECOND caller of cmpRankedRowValue.
//
// 🔴 That is the gap this file would otherwise have opened. teamRowGateViolations
// scopes buildTeamDumbbell by name; a second caller is simply UNSCOPED, so all four
// of the bypass classes measured on #605 — the row CONTAINER's textContent, the
// LABEL cell, the TAG cell, and the TRACK's class argument — would have been
// unguarded on the developer row while every #613 rule reported clean. Withholding
// two cells means nothing if the row can print the number in a third.
//
// The developer builder is the harder of the two: three arms, a `sig` flag, a
// missing-side tag, and a delta CLASS that carries #277's significance colour. Each
// of those is a surface the team row does not have.
var devRowBuilderRootAllow = map[string]bool{
	"var": true, "const": true, "let": true, "return": true,
	"if": true, "else": true,
	"true": true, "false": true, "null": true, "undefined": true,
	// Parameters and locals.
	"row": true, "scale": true, "hasA": true, "hasB": true, "both": true, "d": true,
	"aRanked": true, "bRanked": true, "bothRanked": true, "sig": true, "dir": true,
	"plotA": true, "plotB": true, "fracA": true, "fracB": true,
	"rowEl": true, "label": true, "tag": true,
	"bClass": true, "connectClass": true, "deltaCls": true, "deltaText": true,
	// Formatters, shared vocabulary, and the builders it composes.
	"num": true, "Math": true, "signStr": true, "deltaDir": true,
	"sideData": true, "missingSideTag": true, "abReadout": true,
	"withheldSidesNote": true,
	"el":                true, "buildDumbbellTrack": true, "cmpRankedRowValue": true,
	"BELOW_FLOOR_TAG": true,
}

var devRowBuilderPropAllow = map[string]bool{
	// The ONLY fields of `row` this function may read.
	"a": true, "b": true, "ranked": true, "tier": true, "delta_tier": true,
	"developer": true, "present_a": true, "present_b": true, "significant": true,
	// Formatting and the two DOM mechanics it needs (the label's text is written
	// through textContent, unlike the team row's, which passes it to el()).
	"abs": true, "toFixed": true, "appendChild": true, "textContent": true,
}

func devRowGateViolations(js string) (bad []string, ok bool) {
	body, found := jsBlockAfter(js, "function buildDevDumbbell(row, scale) {")
	if !found {
		return nil, false
	}
	code, stripOK := jsStripStrings(body)
	if !stripOK {
		return []string{"buildDevDumbbell contains an unterminated string literal or block " +
			"comment, so everything after it went unscanned — a guard that cannot read its " +
			"input must not report clean"}, true
	}

	// Syntax this guard cannot see through, refused rather than parsed.
	if strings.Contains(code, "`") {
		bad = append(bad, "buildDevDumbbell uses a template literal — its ${} interpolations are "+
			"invisible to this guard, so a withheld reading could be published inside one")
	}
	if strings.Contains(code, "[") {
		bad = append(bad, "buildDevDumbbell uses `[`; computed member access hides a field name "+
			"inside a string literal that no property check then sees")
	}
	for _, r := range code {
		if r > 0x7F {
			bad = append(bad, "buildDevDumbbell contains the non-ASCII character "+
				strconv.QuoteRune(r)+" outside a string literal — jsIdentPathRe is ASCII-only, so "+
				"an identifier this guard cannot even see is refused rather than scanned")
			break
		}
	}
	if jsBareReturnRe.MatchString(body) {
		bad = append(bad, "buildDevDumbbell has a `return` at the end of a line — ASI makes what "+
			"follows unreachable, the builder returns undefined, and renderCompare's "+
			"appendChild(undefined) stops the whole developer view rendering")
	}

	// The whole body, by allowlist — the rule that makes the per-call pins exhaustive
	// rather than a list of shapes someone thought of.
	bad = append(bad, cmpAllowViolations2("buildDevDumbbell", body,
		devRowBuilderRootAllow, devRowBuilderPropAllow)...)

	// It builds NO value cell of its own. Every digit-bearing cell goes through the
	// gate, so a cell constructed here is a cell that bypassed it. This is the rule
	// that fails if anyone reinstates the pre-ruling inline column.
	for _, cls := range []string{"'cmp-db-value'", "'cmp-db-delta", "'cmp-db-ab", "'cmp-db-sr'"} {
		if strings.Contains(body, cls) {
			bad = append(bad, "buildDevDumbbell builds a value cell itself ("+cls+"): the Δ and "+
				"the A→B readout must be built only by the gated cmpRankedRowValue, or a cell "+
				"can reach the page without passing its own gate")
		}
	}
	if n := strings.Count(body, "rowEl.appendChild("); n != 3 {
		bad = append(bad, "buildDevDumbbell appends "+strconv.Itoa(n)+" children to the row, want "+
			"3 (label, track, value column) — a fourth is a surface no rule here inspects")
	}

	// THE SURVIVING SURFACES, pinned whole. The label is the developer row's own
	// version of the #605 bypass: it is never withheld, and `label.textContent =
	// row.developer + num(row.a.tier).toFixed(1)` publishes the reading with no cell
	// involved and every gated-call pin still green.
	for _, want := range []struct{ code, why string }{
		{"var label = el('div', 'cmp-db-label' + (bothRanked ? '' : ' below'));",
			"the label element must be built from the muting class alone. Note the leading " +
				"space in ' below': squashed pins cannot see it, and without it the class " +
				"concatenates into a name no CSS rule matches and the row renders UNMUTED"},
		{"label.textContent = row.developer;",
			"the label's text must be the developer name and nothing else — it is never " +
				"withheld, so anything concatenated here is published unconditionally"},
	} {
		if !strings.Contains(body, want.code) {
			bad = append(bad, "buildDevDumbbell is missing the exact source `"+want.code+
				"` — "+want.why+". Deliberately UNSQUASHED: squashing deletes the whitespace "+
				"the defect is made of")
		}
	}
	// EVERY el() CALL, PINNED WHOLE — including the two surfaces that survive
	// withholding. 🔴 Added after this guard's own mutation harness measured the
	// container bypass PASSING: `el('div', 'cmp-db-row', num(row.a.tier).toFixed(1))`
	// publishes the reading as the ROW's own textContent with no cell involved, and
	// every other rule here stayed green. That is #605's measured bypass class, and
	// the first draft of this mirror did not carry it.
	elCalls, elOK := jsCallArgs(body, "el(")
	if !elOK {
		return nil, false
	}
	wantEls := []string{
		"'div','cmp-db-row'",
		"'div','cmp-db-label'+(bothRanked?'':'below')",
	}
	if len(elCalls) != len(wantEls) {
		bad = append(bad, "buildDevDumbbell makes "+strconv.Itoa(len(elCalls))+" el() calls, want "+
			strconv.Itoa(len(wantEls))+" (the row container and the label) — every other element "+
			"on this row comes from a builder audited elsewhere, and an extra el() here is an "+
			"unaudited surface")
	}
	for i, call := range elCalls {
		if i >= len(wantEls) {
			break
		}
		if got := squashSpace(call); got != wantEls[i] {
			bad = append(bad, "buildDevDumbbell's el() call "+strconv.Itoa(i)+" is `el("+got+
				")`, want `el("+wantEls[i]+")` — el()'s third argument is textContent, so an "+
				"element that is never withheld (the row container, the label) can publish the "+
				"reading the value column just declined to")
		}
	}
	if !strings.Contains(squashSpace(body), "buildDumbbellTrack(plotA,fracA,plotB,fracB,bClass,connectClass)") {
		bad = append(bad, "buildDevDumbbell's buildDumbbellTrack call is not "+
			"`(plotA, fracA, plotB, fracB, bClass, connectClass)` — the two class arguments are "+
			"unpinned everywhere else, and a reading concatenated into a class name is still a "+
			"reading that left the process")
	}

	// The gated call, with every argument pinned.
	calls, callsOK := jsCallArgs(body, "cmpRankedRowValue(")
	if !callsOK {
		return nil, false
	}
	if len(calls) != 1 {
		bad = append(bad, "buildDevDumbbell makes "+strconv.Itoa(len(calls))+
			" cmpRankedRowValue calls, want exactly 1 — a second call is a second, unaudited "+
			"value column on the same row")
		return bad, true
	}
	args, argsOK := jsSplitArgs(calls[0])
	if !argsOK || len(args) != 6 {
		bad = append(bad, "the cmpRankedRowValue call does not parse as six arguments "+
			"(tag, deltaCls, bothRanked, deltaText, ab, srNote): "+calls[0])
		return bad, true
	}
	for _, a := range []struct{ slot, got, want, why string }{
		{"tag", squashSpace(args[0]), "tag",
			"the tag is the one cell that survives withholding, and it must be the row's own " +
				"computed verdict"},
		{"deltaCls", squashSpace(args[1]), "deltaCls",
			"a DEVELOPER row's Δ class is computed per arm and carries #277's significance " +
				"colour — hardcoding it here drops that colour silently, which is a regression " +
				"no withholding rule would catch"},
		{"gate", squashSpace(args[2]), "bothRanked",
			"the Δ gate is the both-sides verdict. 🔴 It must NOT be used per side: bothRanked " +
				"folds PRESENCE in, so it is false for every one-sided row, and using it as the " +
				"digit gate would withhold a legitimately RANKED one-sided reading"},
		{"delta", squashSpace(args[3]), "deltaText",
			"the Δ must be handed to the gate rather than printed beside it"},
		{"readout", squashSpace(args[4]),
			"abReadout(hasA,aRanked,row.a&&row.a.tier,hasB,bRanked,row.b&&row.b.tier)",
			"the readout must be handed the PER-SIDE flags — the same aRanked/bRanked that " +
				"plotA/plotB read. On a solo-developer install this row IS the org total, so " +
				"these two numbers are the headlines the card above withheld"},
		{"srNote", squashSpace(args[5]), "withheldSidesNote(hasA&&!aRanked,hasB&&!bRanked)",
			"the off-screen note must be derived from `has-data AND NOT ranked` per side; a " +
				"side with no data is ABSENT, not withheld"},
	} {
		if a.got != a.want {
			bad = append(bad, "the developer row's cmpRankedRowValue "+a.slot+" argument is `"+
				a.got+"`, want `"+a.want+"` — "+a.why)
		}
	}
	// (4c) THE GATE'S INPUT — shared with buildTeamDumbbell so the two grains cannot
	// drift. Without it, `var aRanked = true;` republishes every below-floor developer
	// digit AND its dot with the whole suite green. Measured before this was added.
	bad = append(bad, gateInputViolations(code, "buildDevDumbbell")...)
	// The dev builder's `bothRanked` must additionally fold PRESENCE in. Without
	// `both`, a ONE-SIDED row would be treated as having a comparable pair and could
	// publish a Δ derived from a side that does not exist.
	if rhs, rhsOK := jsAssignRHS(code, "bothRanked"); rhsOK && !strings.Contains(rhs, "both") {
		bad = append(bad, "buildDevDumbbell's `bothRanked` is `"+rhs+"`, which does not fold "+
			"PRESENCE (`both`) in — a one-sided row would then be treated as having a "+
			"comparable pair, and its Δ published from a side with no data")
	}

	// 🔴 THE TAG CELL, ALL FOUR ARMS. The tag is never withheld, so ANYTHING that
	// reaches it is published unconditionally — the team guard pins its single `tag`
	// expression whole for exactly this reason, and the first draft of this mirror
	// pinned only the `sig` arm. Measured survivors of that gap:
	//   tag = missingSideTag(…) + ' ' + num(row.a && row.a.tier).toFixed(1);
	//   tag = BELOW_FLOOR_TAG + ', ' + tag + ' ' + num(row.a && row.a.tier).toFixed(1);
	//   tag = 'not significant';
	// The first two print the withheld DIGIT in the tag cell. The third relabels a
	// withheld row "we tested and found nothing" instead of "we cannot rank this".
	//
	// ⚠️ And note WHICH arm the first draft pinned: the `sig` one, which cannot fire
	// for a below-floor row at all (the server computes Significant = a.Ranked &&
	// b.Ranked && ciDisjoint). It pinned the dead branch and left the live one open.
	for _, want := range []struct{ code, why string }{
		{"tag = missingSideTag(hasA, !!row.present_a, hasB, !!row.present_b);",
			"the one-sided arm's tag must be the missing-window verdict ALONE"},
		{"tag = BELOW_FLOOR_TAG + ', ' + tag;",
			"the one-sided BELOW-FLOOR arm names the floor first, then the missing window, and " +
				"nothing else — this arm is where a withheld digit is easiest to smuggle"},
		{"tag = bothRanked ? 'significant' : BELOW_FLOOR_TAG;",
			"the significant arm must be gated: 'should be unreachable' is not an enforcement"},
		{"tag = bothRanked ? 'not significant' : BELOW_FLOOR_TAG;",
			"the not-significant arm must be gated. THIS is the arm every two-sided below-floor " +
				"developer row actually reaches — the sig arm above cannot fire for one — so an " +
				"ungated 'not significant' mislabels the exact rows this ruling withholds"},
	} {
		if !strings.Contains(body, want.code) {
			bad = append(bad, "buildDevDumbbell is missing the exact source `"+want.code+"` — "+
				want.why+". Deliberately UNSQUASHED: squashing deletes the whitespace a split "+
				"string literal is made of")
		}
	}
	if n := jsAssignCount(code, "tag"); n != 4 {
		bad = append(bad, "buildDevDumbbell assigns `tag` "+strconv.Itoa(n)+" times, want exactly "+
			"4 (one per arm, plus the below-floor prefix) — a fifth assignment relabels a row "+
			"after the pinned expressions above set it")
	}
	// No tag expression may reach a NUMBER. The pins above are whole-expression, but
	// this is the property they exist to protect, stated once and independently.
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "tag =") && (strings.Contains(t, "num(") || strings.Contains(t, "toFixed")) {
			bad = append(bad, "buildDevDumbbell writes a NUMBER into the tag cell (`"+t+"`). The "+
				"tag is never withheld, so a digit concatenated here is published unconditionally "+
				"— the withholding two cells over means nothing")
		}
	}

	// The connector class, UNSQUASHED. Split into `in` + `significant` the CSS rule
	// stops matching and the row renders the SOLID ACCENT connector — the
	// significant-move treatment, asserted for a comparison never tested (#277).
	if !strings.Contains(body, "connectClass = 'insignificant'") {
		bad = append(bad, "buildDevDumbbell's not-significant arm does not set the single literal "+
			"'insignificant'; split by a space it stops matching .cmp-db-connect.insignificant "+
			"and the row renders the solid accent connector, asserting a move it never tested")
	}
	return bad, true
}

// TestDashboard_CompareDevRowGateIsTheOnlyRouteToDigits runs the mirror against the
// shipped asset, then proves it can fail — including on the ONE mutant that matters
// most, the over-withholding one, which is what a naive copy of the team gate ships.
func TestDashboard_CompareDevRowGateIsTheOnlyRouteToDigits(t *testing.T) {
	_, js := get(t, jsPath)
	bad, ok := devRowGateViolations(js)
	if !ok {
		t.Fatal("the developer-row gate guard could not read buildDevDumbbell")
	}
	for _, v := range bad {
		t.Error(v)
	}
}

// TestDashboard_CompareDevRowGuardCatchesTheDefect mutation-proves the mirror.
//
// Mutants are applied to the REAL ASSET rather than to a hand-copied fixture, so
// there is no second copy to drift out of step — the failure mode that needed a
// whole "fixture matches the shipped asset" subtest on the team side. Each row
// asserts the replacement APPLIED before the verdict is read: a mutation that never
// applied is a verdict about nothing (false-green ledger 8).
func TestDashboard_CompareDevRowGuardCatchesTheDefect(t *testing.T) {
	_, js := get(t, jsPath)

	// The clean asset must be SILENT, or a rule that fires on everything "catches"
	// every mutant below while proving nothing.
	if bad, ok := devRowGateViolations(js); !ok || len(bad) != 0 {
		t.Fatalf("the developer-row rule does not accept the shipped asset (ok=%v): %v", ok, bad)
	}

	for _, tt := range []struct {
		name, from, to, want string
	}{
		{
			// 🔴 THE ONE THAT MATTERS MOST. This is what a naive reuse of the team gate
			// ships: bothRanked folds PRESENCE in, so it is false for every one-sided row,
			// and using it per side withholds a legitimately RANKED one-sided reading.
			// Over-withholding is its own disclosure failure — it teaches the reader that
			// '—' sometimes means "we have this and won't say it".
			name: "the developer readout is gated on bothRanked instead of per side",
			from: "abReadout(hasA, aRanked, row.a && row.a.tier, hasB, bRanked, row.b && row.b.tier),\n    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));\n  return rowEl;\n}\n\nfunction buildTeamDumbbell",
			to:   "abReadout(hasA, bothRanked, row.a && row.a.tier, hasB, bothRanked, row.b && row.b.tier),\n    withheldSidesNote(hasA && !aRanked, hasB && !bRanked)));\n  return rowEl;\n}\n\nfunction buildTeamDumbbell",
			want: "readout argument",
		},
		{
			// The LABEL cell is never withheld, so digits concatenated into it publish
			// unconditionally — #605's measured bypass, on the developer row.
			name: "the label cell carries the digits",
			from: "  label.textContent = row.developer;",
			to:   "  label.textContent = row.developer + ' ' + num(row.a.tier).toFixed(1);",
			want: "label's text must be the developer name",
		},
		{
			// The row CONTAINER's own textContent — el()'s third argument — with no cell
			// involved at all.
			name: "the row container publishes the reading as its own textContent",
			from: "  var rowEl = el('div', 'cmp-db-row');\n  var label = el('div', 'cmp-db-label'",
			to:   "  var rowEl = el('div', 'cmp-db-row', num(row.a.tier).toFixed(1));\n  var label = el('div', 'cmp-db-label'",
			want: "el() call 0 is",
		},
		{
			// The TRACK's class argument: a reading concatenated into a class name is
			// still a reading that left the process.
			name: "the track's class argument carries the reading",
			from: "buildDumbbellTrack(plotA, fracA, plotB, fracB, bClass, connectClass)",
			to:   "buildDumbbellTrack(plotA, fracA, plotB, fracB, bClass + num(row.a.tier), connectClass)",
			want: "buildDumbbellTrack call is not",
		},
		{
			// The muting class loses its leading space — #603's defect verbatim, and
			// invisible to every squashed pin in this file.
			name: "the muting class loses its leading space",
			from: "var label = el('div', 'cmp-db-label' + (bothRanked ? '' : ' below'));",
			to:   "var label = el('div', 'cmp-db-label' + (bothRanked ? '' : 'below'));",
			want: "leading space",
		},
		{
			// The significant arm asserts a tested move about withheld numbers.
			name: "the significant arm's tag is ungated",
			from: "    tag = bothRanked ? 'significant' : BELOW_FLOOR_TAG;",
			to:   "    tag = 'significant';",
			want: "'should be unreachable' is not an enforcement",
		},
		{
			// The delta class is hardcoded, silently dropping #277's significance colour —
			// a regression no withholding rule would ever notice.
			name: "the developer delta class is hardcoded flat",
			from: "  rowEl.appendChild(cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText,",
			to:   "  rowEl.appendChild(cmpRankedRowValue(tag, 'flat', bothRanked, deltaText,",
			want: "deltaCls argument",
		},
		{
			// 🔴 #603's hardcoded-ranked lie, on the developer grain. Republishes both the
			// digit AND the dot. Measured passing the whole suite before gateInputViolations
			// was shared into this mirror.
			name: "the developer baseline verdict is hardcoded true",
			from: "  // `!!(row.x && …)` fails closed: a server that does not say never gets published.\n  var aRanked = !!(row.a && row.a.ranked);",
			to:   "  // `!!(row.x && …)` fails closed: a server that does not say never gets published.\n  var aRanked = true;",
			want: "which never reads `row.a.ranked`",
		},
		{
			// The Δ gate as a disjunction: one ranked side unlocks the other, and
			// `selected = baseline + Δ` reconstructs the withheld side exactly.
			name: "the developer delta gate is a disjunction",
			from: "  var bothRanked = both && aRanked && bRanked;",
			to:   "  var bothRanked = both || aRanked || bRanked;",
			want: "which uses `||`",
		},
		{
			// The gate input stops folding PRESENCE in, so a one-sided row is treated as a
			// comparable pair and publishes a Δ derived from a side with no data.
			name: "the developer delta gate drops the presence term",
			from: "  var bothRanked = both && aRanked && bRanked;",
			to:   "  var bothRanked = aRanked && bRanked;",
			want: "does not fold PRESENCE",
		},
		{
			// A side's verdict reopened after the pinned declaration still reads correctly.
			name: "a developer side's verdict is reassigned after it was set",
			from: "  var sig = both && !!row.significant;",
			to:   "  var sig = both && !!row.significant; aRanked = true;",
			want: "assigns `aRanked` 2 times",
		},
		{
			// 🔴 THE WITHHELD DIGIT, PRINTED IN THE TAG CELL. The tag is never withheld,
			// so this publishes unconditionally while both audited cells stay '—'.
			name: "the tag cell carries the withheld digit",
			from: "    tag = missingSideTag(hasA, !!row.present_a, hasB, !!row.present_b);",
			to:   "    tag = missingSideTag(hasA, !!row.present_a, hasB, !!row.present_b) + ' ' + num(row.a && row.a.tier).toFixed(1);",
			want: "writes a NUMBER into the tag cell",
		},
		{
			// The LIVE not-significant arm ungated — where every two-sided below-floor
			// developer row lands. Relabels it "we tested and found nothing".
			name: "the live not-significant arm's tag is ungated",
			from: "    tag = bothRanked ? 'not significant' : BELOW_FLOOR_TAG;",
			to:   "    tag = 'not significant';",
			want: "the arm every two-sided below-floor",
		},
		{
			// The baseline verdict stops failing closed: a server that does not say gets
			// the unwithheld treatment.
			name: "the developer baseline verdict stops failing closed",
			from: "  // `!!(row.x && …)` fails closed: a server that does not say never gets published.\n  var aRanked = !!(row.a && row.a.ranked);",
			to:   "  // `!!(row.x && …)` fails closed: a server that does not say never gets published.\n  var aRanked = (row.a && row.a.ranked);",
			want: "does not coerce to a boolean",
		},
		{
			// The pre-ruling shape: the row builds its own value column and bypasses the
			// gate entirely.
			name: "the row builds its value column inline again",
			from: "  rowEl.appendChild(cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText,",
			to:   "  rowEl.appendChild(el('div', 'cmp-db-ab num', abReadout(hasA, true, row.a && row.a.tier, hasB, true, row.b && row.b.tier)));\n  rowEl.appendChild(cmpRankedRowValue(tag, deltaCls, bothRanked, deltaText,",
			want: "builds a value cell itself",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if n := strings.Count(js, tt.from); n != 1 {
				t.Fatalf("this mutant's target text occurs %d times, want exactly 1 — the "+
					"replacement would not apply (or would apply somewhere unintended), so the "+
					"verdict below would be about nothing. Re-derive it:\n%q", n, tt.from)
			}
			mutated := strings.Replace(js, tt.from, tt.to, 1)
			if mutated == js {
				t.Fatal("the mutation did not change the source")
			}
			bad, ok := devRowGateViolations(mutated)
			if !ok {
				t.Fatal("the guard could not READ this mutant, so it dies as a short read " +
					"rather than a named finding — indistinguishable from a rename")
			}
			if len(bad) == 0 {
				t.Fatal("the developer-row guard reports CLEAN on a mutant that publishes a " +
					"below-floor reading (or withholds a ranked one)")
			}
			// 🔴 It must fail for its STATED reason, not merely fail. A mutant that trips a
			// different rule first is a verdict about nothing (false-green ledger 18, and
			// the 2026-08-01 fault injection that returned rc=2 instead of rc=1).
			joined := strings.Join(bad, " | ")
			if !strings.Contains(joined, tt.want) {
				t.Errorf("the guard fired, but not for the reason this mutant exists to "+
					"provoke.\n  want a finding mentioning: %q\n  got: %s", tt.want, joined)
			}
		})
	}
}

// mustTrimSuffix removes `suffix` from `src` and PANICS if it was not there.
//
// It replaces a `src[:len(src)-len(suffix)]` slice, which assumed the suffix rather
// than asserting it: if the fixture's tail ever changed, that arithmetic silently cut
// the WRONG bytes (or panicked with an unhelpful index error), and the harness's
// `tt.src == clean` guard cannot catch it — a wrongly-offset cut differs from `clean`
// exactly as a correct one does, so the row keeps "passing" while testing a shape
// nobody wrote. Panicking at construction is the loud failure that arithmetic wasn't.
func mustTrimSuffix(src, suffix string) string {
	out := strings.TrimSuffix(src, suffix)
	if out == src {
		panic("mutation fixture: expected suffix is gone, so this mutant would cut arbitrary " +
			"bytes and its verdict would be about nothing:\n" + suffix)
	}
	return out
}
