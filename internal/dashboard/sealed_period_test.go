package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// manifestLoadGenTouches is readManifest's share of loadGenerationViolations'
// count (#851): one capture plus one comparison per callback, so a stale manifest
// never overrides a newer load.
const manifestLoadGenTouches = 3

// sealedWholeBodies are the #913 functions pinned whole: the switch into team
// mode (load reads the manifest on every call, and the manifest's answer or a
// sealed 404 decides the mode), the way back out, and the month arithmetic the
// picker and compare are built from.
var sealedWholeBodies = []struct{ name, header, body string }{
	{"load", "function load() {",
		"var tokenField = $('token-input');" +
			"if (tokenField.value) { sessionStorage.setItem('tier_token', tokenField.value); tokenField.value = ''; }" +
			"readManifest();"},
	{"readManifest", "function readManifest() {",
		"resetViews();" +
			"var gen = ++loadGen;" +
			"fetchJSON('/api/v1/report_manifest')" +
			".then(function(m) {" +
			"  if (gen !== loadGen) { return; }" +
			"  if (m && m.manifest_schema === SEALED_MANIFEST) { enterSealedMode(m); } else { leaveSealedMode(); }" +
			"  loadView();" +
			"})" +
			".catch(function(e) {" +
			"  if (gen !== loadGen) { return; }" +
			"  if (sealedMissing(e)) { enterSealedMode(e.body); loadView(); return; }" +
			"  showStatus(failureText(e), true);" +
			"});"},
	{"leaveSealedMode", "function leaveSealedMode() {",
		"sealedMode = false;" +
			"$('window-controls').style.display = 'contents';" +
			"$('period-controls').style.display = 'none';" +
			"$('seal-note').style.display = 'none';" +
			"$('compare-controls').style.display = $('compare-toggle').checked ? '' : 'none';"},
	{"monthDay", "function monthDay(p, n) {",
		"if (!MONTH_RE.test(p)) { return ''; }" +
			"return new Date(Date.UTC(Number(p.slice(0, 4)), Number(p.slice(5, 7)) - 1 + n, 1)).toISOString().slice(0, 10);"},
}

// sealedPeriodViolations enforces #913's dashboard half: a team or division
// server publishes only sealed calendar months, so the dashboard asks it for a
// month and never for a date window, while developer mode keeps its window.
//
// The two query statements are pinned whole, both arms, and each loader builds
// its query once and sends exactly that, so the sealed arm can carry no
// since/until. sealedMode is set in exactly three places: its declaration,
// leaveSealedMode and enterSealedMode. A sealed read's failure text reaches the
// page only through showStatus, which writes text.
func sealedPeriodViolations(js string) (bad []string) {
	src := jsStrip(js)
	code, ok := jsStripStrings(src)
	if !ok {
		return []string{"dashboard.js could not be string-stripped, so the #913 guard cannot read it"}
	}

	for _, w := range sealedWholeBodies {
		body, found := jsBlockAfter(src, w.header)
		if !found || squashSpace(body) != squashSpace(w.body) {
			bad = append(bad, w.name+": body is not the pinned #913 shape")
		}
	}

	pins := []struct{ name, header, stmt string }{
		{"loadScores", "function loadScores() {",
			"var period = sealedMode ? $('period-select').value : '';"},
		{"loadScores", "function loadScores() {",
			"var q = sealedMode ? '/api/v1/scores' + (period ? '?period=' + encodeURIComponent(period) : '')" +
				" : '/api/v1/scores?since=' + encodeURIComponent(since) + (until ? '&until=' + encodeURIComponent(until) : '');"},
		{"loadScores", "function loadScores() {",
			"var since = sealedMode ? monthDay(period, 0) : $('since-input').value;"},
		{"loadScores", "function loadScores() {",
			"var until = sealedMode ? monthDay(period, 1) : $('until-input').value;"},
		{"loadCompare", "function loadCompare() {",
			"var periodB = sealedMode ? $('period-select').value : '';"},
		{"loadCompare", "function loadCompare() {",
			"var q = sealedMode ? '/api/v1/scores/compare' + (MONTH_RE.test(periodB) && periodB !== sealedLatest ? '?period_a=' + " +
				"encodeURIComponent(monthDay(periodB, -1).slice(0, 7)) + '&period_b=' + encodeURIComponent(periodB) : '')" +
				" : '/api/v1/scores/compare' + '?since_a=' + encodeURIComponent(sinceA)" +
				" + (untilA ? '&until_a=' + encodeURIComponent(untilA) : '')" +
				" + '&since_b=' + encodeURIComponent(sinceB)" +
				" + (untilB ? '&until_b=' + encodeURIComponent(untilB) : '');"},
		{"enterSealedMode", "function enterSealedMode(m) {", "sealedMode = true;"},
		{"enterSealedMode", "function enterSealedMode(m) {", "sealedLatest = m.period;"},
		{"enterSealedMode", "function enterSealedMode(m) {", "$('window-controls').style.display = 'none';"},
		{"enterSealedMode", "function enterSealedMode(m) {", "$('compare-controls').style.display = 'none';"},
		{"enterSealedMode", "function enterSealedMode(m) {", "$('period-controls').style.display = 'contents';"},
		{"enterSealedMode", "function enterSealedMode(m) {", "var o = el('option', null, months[i]);"},
		{"enterSealedMode", "function enterSealedMode(m) {", "setText('seal-note', note + stallText(m));"},
		{"showStatus", "function showStatus(msg, isError) {", "elm.textContent = msg;"},
	}
	for _, p := range pins {
		body, found := jsBlockAfter(src, p.header)
		if !found {
			bad = append(bad, p.name+": not found — the #913 guard cannot be scoped")
			continue
		}
		if n := strings.Count(squashSpace(body), squashSpace(p.stmt)); n != 1 {
			bad = append(bad, p.name+": the statement "+p.stmt+" occurs "+strconv.Itoa(n)+" times, want 1")
		}
	}

	// Absence: each loader builds its query once, from inputs assigned once, and
	// sends exactly that query, so nothing adds a window to the sealed arm.
	for _, l := range []struct {
		name, header string
		vars         []string
	}{
		{"loadScores", "function loadScores() {", []string{"q", "period", "since", "until"}},
		{"loadCompare", "function loadCompare() {", []string{"q", "periodB"}},
	} {
		body, found := jsBlockAfter(src, l.header)
		bodyCode, sok := jsStripStrings(body)
		if !found || !sok {
			bad = append(bad, l.name+": cannot be read by the #913 absence rule")
			continue
		}
		for _, v := range l.vars {
			if n := jsAssignCount(bodyCode, v); n != 1 {
				bad = append(bad, l.name+": assigns "+v+" "+strconv.Itoa(n)+" times, want 1")
			}
		}
		if args, aok := jsCallArgs(body, "fetchJSON("); !aok || len(args) != 1 || strings.TrimSpace(args[0]) != "q" {
			bad = append(bad, l.name+": fetchJSON is called with something other than q, once")
		}
	}

	decls := len(regexp.MustCompile(`\bfunction\s+readManifest\s*\(`).FindAllStringIndex(code, -1))
	if refs := len(regexp.MustCompile(`\breadManifest\b`).FindAllStringIndex(code, -1)) - decls; decls != 1 || refs != 1 {
		bad = append(bad, "readManifest is called "+strconv.Itoa(refs)+" times, want 1 (load)")
	}
	for _, stmt := range []string{
		// A sealed server compares whole months, so it has no baseline row to show.
		"$('compare-controls').style.display = on && !sealedMode ? '' : 'none';",
		"$('period-select').addEventListener('change', loadView);",
	} {
		if n := strings.Count(squashSpace(src), squashSpace(stmt)); n != 1 {
			bad = append(bad, "the statement "+stmt+" occurs "+strconv.Itoa(n)+" times, want 1")
		}
	}

	if n := jsAssignCount(code, "sealedMode"); n != 3 {
		bad = append(bad, "sealedMode is assigned "+strconv.Itoa(n)+" times, want 3 (its declaration, "+
			"leaveSealedMode, enterSealedMode)")
	}
	if n := jsAssignCount(code, "sealedLatest"); n != 2 {
		bad = append(bad, "sealedLatest is assigned "+strconv.Itoa(n)+" times, want 2 (its declaration, enterSealedMode)")
	}
	// failureText's text is painted only by the two loaders' catch callbacks and
	// readManifest's.
	if n := len(regexp.MustCompile(`\bfailureText\b`).FindAllStringIndex(code, -1)); n != 4 {
		bad = append(bad, "failureText is referenced "+strconv.Itoa(n)+" times, want 4 (its declaration, "+
			"one showStatus call per loader and readManifest's)")
	}
	if n := strings.Count(squashSpace(src), "showStatus(failureText(e),!sealedMissing(e));"); n != 2 {
		bad = append(bad, "showStatus(failureText(e), !sealedMissing(e)) occurs "+strconv.Itoa(n)+" times, want 2")
	}
	return bad
}

// TestDashboard_TeamModeRequestsSealedMonths pins #913's dashboard half on the
// shipped asset.
func TestDashboard_TeamModeRequestsSealedMonths(t *testing.T) {
	_, js := get(t, jsPath)
	for _, v := range sealedPeriodViolations(js) {
		t.Error(v)
	}
}

// TestDashboard_TeamModeGuardCatchesTheDefect is sealedPeriodViolations' control
// arm: each mutant is a defect the guard must reject, and n is how many of its
// rules fire (1 where the fixture is that rule's sole defence).
func TestDashboard_TeamModeGuardCatchesTheDefect(t *testing.T) {
	_, js := get(t, jsPath)
	if bad := sealedPeriodViolations(js); len(bad) != 0 {
		t.Fatalf("the #913 guard does not accept the shipped asset: %v", bad)
	}
	scoresFetch := "  fetchJSON(q)\n    .then(function(data) { if (gen !== loadGen) { return; } renderScores"
	compareFetch := "  fetchJSON(q)\n    .then(function(data) { if (gen !== loadGen) { return; } renderCompare"
	for _, tt := range []struct {
		name, from, to, want string
		n                    int
	}{
		// The switch into team mode.
		{"load never reads the manifest", "  readManifest();\n}\n", "}\n", "load: body", 2},
		{"the manifest is read from a second place", "function loadView() {\n",
			"function loadView() {\n  readManifest();\n", "readManifest is called 2 times", 1},
		{"a sealed 404 does not enter sealed mode",
			"      if (sealedMissing(e)) { enterSealedMode(e.body); loadView(); return; }\n", "", "readManifest: body", 1},
		{"an unknown mode falls back to a date window",
			"      showStatus(failureText(e), true);\n",
			"      if (e.status) { loadView(); return; }\n      showStatus(failureText(e), true);\n", "readManifest: body", 1},
		// #1012 engine review: a failed manifest read under a known mode sends the
		// view's request anyway (Codex 4ba82dda), and the manifest read leaves the
		// previous window's views up while it can still fail (Codex b39cbf3b).
		{"a failed manifest trusts the previous mode",
			"      showStatus(failureText(e), true);\n",
			"      if (e.status && sealedMode !== null) { loadView(); return; }\n      showStatus(failureText(e), true);\n",
			"readManifest: body", 1},
		{"the manifest read keeps the previous views", "function readManifest() {\n  resetViews();\n",
			"function readManifest() {\n", "readManifest: body", 1},
		{"the views are reset only once the manifest answers",
			"  resetViews();\n  var gen = ++loadGen;\n  fetchJSON('/api/v1/report_manifest')\n    .then(function(m) {\n      if (gen !== loadGen) { return; }\n",
			"  var gen = ++loadGen;\n  fetchJSON('/api/v1/report_manifest')\n    .then(function(m) {\n      if (gen !== loadGen) { return; }\n      resetViews();\n",
			"readManifest: body", 1},
		{"a stale manifest overrides a newer load", "      if (gen !== loadGen) { return; }\n      if (m && m.manifest_schema",
			"      if (m && m.manifest_schema", "readManifest: body", 1},
		{"an empty token field is stored",
			"  if (tokenField.value) {\n    sessionStorage.setItem('tier_token', tokenField.value);\n    tokenField.value = '';\n  }\n  readManifest();",
			"  sessionStorage.setItem('tier_token', tokenField.value);\n  tokenField.value = '';\n  readManifest();", "load: body", 1},
		{"a live manifest leaves the picker up", "  $('period-controls').style.display = 'none';\n", "", "leaveSealedMode: body", 1},
		// The month arithmetic.
		{"the month arithmetic drops the -1", "Number(p.slice(5, 7)) - 1 + n", "Number(p.slice(5, 7)) + n", "monthDay: body", 1},
		{"the month arithmetic does not roll over the year",
			"return new Date(Date.UTC(Number(p.slice(0, 4)), Number(p.slice(5, 7)) - 1 + n, 1)).toISOString().slice(0, 10);",
			"var mm = Number(p.slice(5, 7)) + n; return p.slice(0, 4) + '-' + (mm < 10 ? '0' + mm : mm) + '-01';",
			"monthDay: body", 1},
		// The queries.
		{"the sealed read drops its sealed arm",
			"var q = sealedMode ? '/api/v1/scores' + (period ? '?period=' + encodeURIComponent(period) : '')\n        : '/api/v1/scores?since='",
			"var q = '/api/v1/scores?since='", "loadScores: the statement var q", 1},
		{"the sealed read adds a window", scoresFetch,
			"  if (sealedMode) { q += '&since=' + encodeURIComponent(since); }\n" + scoresFetch, "loadScores: assigns q 2 times", 1},
		{"the picked month carries a window", "  var period = sealedMode ? $('period-select').value : '';\n",
			"  var period = sealedMode ? $('period-select').value : '';\n  period += '&until=' + $('until-input').value;\n",
			"loadScores: assigns period 2 times", 1},
		{"the sealed compare sends a window", compareFetch,
			"  fetchJSON(q + '&until_b=' + encodeURIComponent(untilB))\n    .then(function(data) { if (gen !== loadGen) { return; } renderCompare",
			"loadCompare: fetchJSON is called with", 1},
		{"the newest month's compare names a gap", "MONTH_RE.test(periodB) && periodB !== sealedLatest", "MONTH_RE.test(periodB)",
			"loadCompare: the statement var q", 1},
		{"the newest month is overwritten", "function loadView() {\n", "function loadView() {\n  sealedLatest = '';\n",
			"sealedLatest is assigned 3 times", 1},
		// The controls.
		{"the picker is never shown", "  $('period-controls').style.display = 'contents';\n", "",
			"enterSealedMode: the statement $('period-controls')", 1},
		{"the baseline row stays up in sealed mode", "  $('compare-controls').style.display = 'none';\n", "",
			"enterSealedMode: the statement $('compare-controls')", 1},
		{"the compare toggle shows the baseline in sealed mode", "on && !sealedMode ? '' : 'none'", "on ? '' : 'none'",
			"the statement $('compare-controls').style.display = on", 1},
		{"a picked month is never loaded", "$('period-select').addEventListener('change', loadView);\n", "",
			"the statement $('period-select')", 1},
		{"sealed mode is set from a third place", "function loadView() {\n", "function loadView() {\n  sealedMode = true;\n",
			"sealedMode is assigned 4 times", 1},
		// The text sinks.
		{"the failure text is painted elsewhere",
			"} showStatus('Error: ' + e.message, true); });\n}\n\n// showKAnonWithheld",
			"} showStatus(failureText(e), true); });\n}\n\n// showKAnonWithheld",
			"failureText is referenced 5 times", 1},
		{"a sealed 404 is painted as an error",
			"    .catch(function(e) { if (gen !== loadGen) { return; } showStatus(failureText(e), !sealedMissing(e)); });\n}\n\n// showDetail",
			"    .catch(function(e) { if (gen !== loadGen) { return; } showStatus(failureText(e), true); });\n}\n\n// showDetail",
			"occurs 1 times, want 2", 1},
		{"the status is written as markup", "  elm.textContent = msg;\n", "  elm.innerHTML = msg;\n", "showStatus: the statement", 1},
		{"a month option is written as markup", "var o = el('option', null, months[i]);",
			"var o = document.createElement('option'); o.innerHTML = months[i];", "enterSealedMode: the statement var o", 1},
		{"the seal note is written as markup", "setText('seal-note', note + stallText(m));",
			"$('seal-note').innerHTML = note + stallText(m);", "enterSealedMode: the statement setText", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if n := strings.Count(js, tt.from); n != 1 {
				t.Fatalf("this mutant's target text occurs %d times, want 1; the verdict would be "+
					"about nothing:\n%q", n, tt.from)
			}
			bad := sealedPeriodViolations(strings.Replace(js, tt.from, tt.to, 1))
			if len(bad) != tt.n || !strings.Contains(strings.Join(bad, " | "), tt.want) {
				t.Errorf("want %d violation(s) including %q, got %d: %v", tt.n, tt.want, len(bad), bad)
			}
		})
	}
}

// TestDashboard_PeriodPickerMarkup pins the picker's markup: a labelled select
// the sealed mode reveals, the window group it hides, and the seal note, which is
// not a live region (#516: banners re-read on every refresh).
func TestDashboard_PeriodPickerMarkup(t *testing.T) {
	_, html := get(t, "/")
	for _, want := range []string{
		`<span id="window-controls" style="display:contents">`,
		`<span id="period-controls" style="display:none">`,
		`<label for="period-select">`,
		`<select id="period-select">`,
		`<p id="seal-note" class="subtitle" style="display:none">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}

// horizonDeveloperArm is renderCostHorizon's developer-mode predates arm, pinned
// whole: #1052 left its behaviour and wording unchanged.
const horizonDeveloperArm = "setText('cost-horizon-title'," +
	"'Cost horizon — this window starts before TIER captured any cost (' + startDay + ')');" +
	"setText('cost-horizon-sub'," +
	"'The TIER above is inflated: outcomes from the uncovered head of this window are counted ' +" +
	"'against none of their cost. Set the window start to ' + safeSince + ' or later to compare ' +" +
	"'like with like. The earliest AI spend this installation holds is ' + startDay + '. Shipping ' +" +
	"'older session logs (tierd ship --since) moves it earlier.');"

// horizonSealedArm is renderCostHorizon's sealed predates arm, pinned whole so
// each message stays on its own condition: the "none yet" text only when the
// first covered month is after sealedLatest, otherwise the picker remedy naming
// that month.
const horizonSealedArm = "var first = firstCoveredMonth(start);" +
	"setText('cost-horizon-title'," +
	"'Cost horizon — this month starts before TIER captured any cost (' + startDay + ')');" +
	"setText('cost-horizon-sub'," +
	"'The TIER above is inflated: outcomes from the uncovered head of this month are counted ' +" +
	"'against none of their cost. ' +" +
	"(first && MONTH_RE.test(sealedLatest) && first > sealedLatest" +
	" ? 'No sealed month is fully covered yet; the first will be ' + first + ' once it seals.'" +
	" : 'Pick ' + (first || 'a later month') + (first ? ' or later' : '') + ' in the month picker to compare like with like.') +" +
	"' Shipping older session logs affects only months not yet sealed.');"

// sealedHorizonViolations enforces #1052: in team or division mode the server
// refuses a date window and a sealed month never changes, so the cost-horizon
// banner's remedy is the first fully covered month (the sealer's earliestSealable
// rule, a start at exactly 00:00:00Z covering its month), or the honest "none yet"
// against sealedLatest. It never offers a window start or a re-ship as the fix.
func sealedHorizonViolations(js string) (bad []string) {
	src := jsStrip(js)
	fn, ok := jsBlockAfter(src, "function renderCostHorizon(dq, since, totalCostUSD) {")
	if !ok {
		return []string{"renderCostHorizon: not found — the #1052 guard cannot be scoped"}
	}
	first, ok := jsBlockAfter(src, "function firstCoveredMonth(iso) {")
	if !ok || squashSpace(first) != squashSpace("var ms = Date.parse(iso);"+
		"if (isNaN(ms)) { return ''; }"+
		"var m = new Date(ms).toISOString().slice(0, 7);"+
		"return Date.parse(monthDay(m, 0) + 'T00:00:00Z') >= ms ? m : monthDay(m, 1).slice(0, 7);") {
		bad = append(bad, "firstCoveredMonth: body is not the pinned #1052 shape")
	}
	if dev, ok := jsBlockAfter(fn, "} else if (predates) {"); !ok || squashSpace(dev) != squashSpace(horizonDeveloperArm) {
		bad = append(bad, "renderCostHorizon: the developer predates arm is not the pinned wording")
	}
	sealed, ok := jsBlockAfter(fn, "if (predates && sealedMode) {")
	if !ok {
		return append(bad, "renderCostHorizon: no sealed predates arm")
	}
	for _, banned := range []string{"safeSince", "window start", "ship --since"} {
		if strings.Contains(sealed, banned) {
			bad = append(bad, "renderCostHorizon: the sealed arm offers "+banned+" as the fix")
		}
	}
	if squashSpace(sealed) != squashSpace(horizonSealedArm) {
		bad = append(bad, "renderCostHorizon: the sealed predates arm is not the pinned #1052 shape")
	}
	if code, sok := jsStripStrings(sealed); !sok {
		bad = append(bad, "renderCostHorizon: the sealed arm cannot be string-stripped")
	} else if n := jsAssignCount(code, "first"); n != 1 {
		bad = append(bad, "renderCostHorizon: the sealed arm assigns first "+strconv.Itoa(n)+" times, want 1")
	}
	return bad
}

// TestDashboard_SealedHorizonNamesAMonth pins #1052 on the shipped asset.
func TestDashboard_SealedHorizonNamesAMonth(t *testing.T) {
	_, js := get(t, jsPath)
	for _, v := range sealedHorizonViolations(js) {
		t.Error(v)
	}
}

// TestDashboard_SealedHorizonGuardCatchesTheDefect is sealedHorizonViolations'
// control arm; n is how many rules fire (1 where the fixture is that rule's sole
// defence).
func TestDashboard_SealedHorizonGuardCatchesTheDefect(t *testing.T) {
	_, js := get(t, jsPath)
	if bad := sealedHorizonViolations(js); len(bad) != 0 {
		t.Fatalf("the #1052 guard does not accept the shipped asset: %v", bad)
	}
	sealedRemedy := "      (first && MONTH_RE.test(sealedLatest) && first > sealedLatest\n" +
		"        ? 'No sealed month is fully covered yet; the first will be ' + first + ' once it seals.'\n" +
		"        : 'Pick ' + (first || 'a later month') + (first ? ' or later' : '') + ' in the month picker to compare like with like.') +\n" +
		"      ' Shipping older session logs affects only months not yet sealed.');\n"
	for _, tt := range []struct {
		name, from, to, want string
		n                    int
	}{
		// The pre-#1052 remedy, back in the sealed arm.
		{"the sealed arm reverts to the window remedy", sealedRemedy,
			"      'Set the window start to ' + safeSince + ' or later to compare like with like. The earliest AI ' +\n" +
				"      'spend this installation holds is ' + startDay + '. Shipping older session logs ' +\n" +
				"      '(tierd ship --since) moves it earlier.');\n",
			"the sealed arm offers safeSince", 4},
		{"the sealed arm offers a re-ship", "affects only months not yet sealed.", "(tierd ship --since) moves it earlier.",
			"the sealed arm offers ship --since", 2},
		{"the sealed arm names a window start", "      'against none of their cost. ' +\n      (first",
			"      'against none of their cost. Move the window start. ' +\n      (first", "the sealed arm offers window start", 2},
		{"the sealed arm names the safe day", "'Pick ' + (first || 'a later month')", "'Pick ' + (first || safeSince)",
			"the sealed arm offers safeSince", 2},
		{"the sealed arm is wired to developer mode", "if (predates && sealedMode) {", "if (predates && !sealedMode) {",
			"no sealed predates arm", 1},
		{"no-covered-month is never said", "first && MONTH_RE.test(sealedLatest) && first > sealedLatest", "false",
			"the sealed predates arm is not the pinned", 1},
		{"the picker is not named", "' in the month picker to compare", "' to compare", "the sealed predates arm is not the pinned", 1},
		// #1055 local review: each message stays on its own condition, names the
		// month it was computed for, and that month is computed once.
		{"the two messages are swapped",
			"        ? 'No sealed month is fully covered yet; the first will be ' + first + ' once it seals.'\n" +
				"        : 'Pick ' + (first || 'a later month') + (first ? ' or later' : '') + ' in the month picker to compare like with like.') +\n",
			"        ? 'Pick ' + (first || 'a later month') + (first ? ' or later' : '') + ' in the month picker to compare like with like.'\n" +
				"        : 'No sealed month is fully covered yet; the first will be ' + first + ' once it seals.') +\n",
			"the sealed predates arm is not the pinned", 1},
		{"the picker remedy names the newest sealed month", "'Pick ' + (first || 'a later month')",
			"'Pick ' + (sealedLatest || 'a later month')", "the sealed predates arm is not the pinned", 1},
		{"the none-yet text names the newest sealed month", "the first will be ' + first + ' once it seals.'",
			"the first will be ' + sealedLatest + ' once it seals.'", "the sealed predates arm is not the pinned", 1},
		{"the first covered month is moved on a month", "    var first = firstCoveredMonth(start);\n",
			"    var first = firstCoveredMonth(start);\n    first = monthDay(first, 1).slice(0, 7);\n",
			"the sealed arm assigns first 2 times", 2},
		{"the first covered month is blanked", "    var first = firstCoveredMonth(start);\n",
			"    var first = firstCoveredMonth(start);\n    first = '';\n",
			"the sealed arm assigns first 2 times", 2},
		{"the sealed title says window", "'Cost horizon — this month starts before", "'Cost horizon — this window starts before",
			"the sealed predates arm is not the pinned", 2},
		// The month arithmetic.
		{"a start at exactly 00:00:00Z on the 1st is not covered", "+ 'T00:00:00Z') >= ms ?", "+ 'T00:00:00Z') > ms ?",
			"firstCoveredMonth: body", 1},
		{"the first covered month is the horizon's own month", "? m : monthDay(m, 1).slice(0, 7);", "? m : m;",
			"firstCoveredMonth: body", 1},
		// The developer arm.
		{"the developer arm's wording changes", "'against none of their cost. Set the window start to '",
			"'against none of their cost. Pick a start of '", "the developer predates arm", 1},
		{"the developer arm drops the re-ship", "'older session logs (tierd ship --since) moves it earlier.');",
			"'older session logs moves it earlier.');", "the developer predates arm", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if n := strings.Count(js, tt.from); n != 1 {
				t.Fatalf("this mutant's target text occurs %d times, want 1; the verdict would be "+
					"about nothing:\n%q", n, tt.from)
			}
			bad := sealedHorizonViolations(strings.Replace(js, tt.from, tt.to, 1))
			if len(bad) != tt.n || !strings.Contains(strings.Join(bad, " | "), tt.want) {
				t.Errorf("want %d violation(s) including %q, got %d: %v", tt.n, tt.want, len(bad), bad)
			}
		})
	}
}
