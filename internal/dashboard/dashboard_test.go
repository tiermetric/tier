package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
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
	// The significant branch is the sole source of a directional bClass.
	if strings.Count(devDb, "bClass = dir") != 1 {
		t.Error("buildDevDumbbell: exactly one branch (the significant one) may set a directional dot colour (bClass = dir)")
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
