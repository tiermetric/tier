package dashboard

import (
	"strings"
	"testing"
)

// S10-4: scope this guard to the provenance renderer; compare already warned
// about mixed versions and must not satisfy the single-window regression.
func TestDashboard_ProvenanceMixedPriceVersions(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(js, "function renderProvenance(data) {")
	if !ok {
		t.Fatal("renderProvenance missing")
	}
	for _, want := range []string{
		"var versions = data.data_quality && data.data_quality.mixed_price_versions;",
		"var mixed = Array.isArray(versions) && versions.length > 1;",
		"if (mixed) { parts.push('mixed price versions (' + versions.join(', ') + ')'); }",
		"mixed ? '. Matching current versions alone does not make captured costs comparable.'",
	} {
		if !strings.Contains(squashSpace(body), squashSpace(want)) {
			t.Errorf("provenance missing %q", want)
		}
	}
	// Mixed provenance must also render without a current price/rubric stamp.
	if strings.Index(body, "if (mixed)") > strings.Index(body, "if (parts.length === 0)") {
		t.Error("mixed versions must be appended before the empty-stamp early return")
	}
}

// No Node/exec.Command harness exists in this package. These control arms pin
// the clean branches structurally; they do not execute JavaScript.
func TestDashboard_ProvenanceMixedPriceVersionsControl(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(js, "function renderProvenance(data) {")
	if !ok {
		t.Fatal("renderProvenance missing")
	}
	for _, want := range []string{
		// Absent, empty, and single-version arrays must take the clean arm.
		"var mixed = Array.isArray(versions) && versions.length > 1;",
		"(mixed ? '. Matching current versions alone does not make captured costs comparable.' : '. Compare TIER / cost-per-point only across matching versions.')",
		// Without stamps or mixed versions, the row stays hidden.
		"if (parts.length === 0) { stamp.style.display = 'none'; return; }",
	} {
		if !strings.Contains(squashSpace(body), squashSpace(want)) {
			t.Errorf("clean provenance control missing %q", want)
		}
	}
	if strings.Count(body, "parts.push('mixed price versions (") != 1 {
		t.Error("mixed-version text must only be appended in the mixed branch")
	}
	if strings.Count(body, "Matching current versions alone") != 1 {
		t.Error("mixed-version caveat must only appear in the mixed ternary arm")
	}
}

// S10-5: each side must supply its own start, and a late source must warn even
// when the global window_predates_cost_capture flag is false. The existing
// dashboard harness checks the shipped JS structurally, without a JS runtime.
func TestDashboard_ComparePerSourceCostHorizon(t *testing.T) {
	_, js := get(t, jsPath)
	for _, side := range []string{"a", "b"} {
		which := "baseline"
		if side == "b" {
			which = "selected"
		}
		want := "appendWindowCaveat(host, '" + which + "', data.window_" + side +
			" && data.window_" + side + ".data_quality, anonymised, data.window_" + side + " && data.window_" + side + ".since, data.total && data.total." + side + " && data.total." + side + ".total_cost_usd);"
		if !strings.Contains(jsStrip(js), want) {
			t.Errorf("compare does not pass window %s's own start to its caveat", side)
		}
	}
	body, ok := jsBlockAfter(js, "function appendWindowCaveat(host, which, dq, anonymised, since, totalCostUSD) {")
	if !ok {
		t.Fatal("appendWindowCaveat must receive the window start")
	}
	for _, want := range []string{
		"var sinceMs = Date.parse(since);",
		"var perSource = dq.source_coverage_start;",
		"if (perSource && typeof perSource === 'object') {",
		"Object.keys(perSource).forEach(function(source) { var ms = Date.parse(perSource[source]); if (isNaN(ms) || isNaN(sinceMs)) { sourcesOK = false; } else if (ms > sinceMs) { lateSource = true; } });",
		"if (dq.window_predates_cost_capture === true) { notes.unshift('predates cost capture — TIER inflated'); } else if (lateSource) { notes.unshift('predates per-source cost capture — TIER inflated'); }",
		"if (!sourcesOK) { notes.push('per-source cost coverage could not be checked'); }",
	} {
		if !strings.Contains(squashSpace(body), squashSpace(want)) {
			t.Errorf("per-window caveat missing %q", want)
		}
	}
}

func TestDashboard_ComparePerSourceCostHorizonControl(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(js, "function appendWindowCaveat(host, which, dq, anonymised, since, totalCostUSD) {")
	if !ok {
		t.Fatal("appendWindowCaveat missing")
	}
	for _, want := range []string{
		"var lateSource = false;",
		"var sourcesOK = true;",
		// Missing, empty, equal, or earlier coverage must not mark a late source.
		"if (perSource && typeof perSource === 'object') { Object.keys(perSource).forEach(function(source) { var ms = Date.parse(perSource[source]); if (isNaN(ms) || isNaN(sinceMs)) { sourcesOK = false; } else if (ms > sinceMs) { lateSource = true; } }); }",
		"if (dq.window_predates_cost_capture === true) { notes.unshift('predates cost capture — TIER inflated'); } else if (lateSource) { notes.unshift('predates per-source cost capture — TIER inflated'); }",
		// A clean window emits no caveat element.
		"if (notes.length === 0) { return; } host.appendChild(el('span', 'cmp-caveat', which + ': ' + notes.join(', ')));",
	} {
		if !strings.Contains(squashSpace(body), squashSpace(want)) {
			t.Errorf("clean per-source control missing %q", want)
		}
	}
	if strings.Count(body, "predates per-source cost capture") != 1 {
		t.Error("per-source caveat must only appear in the late-source branch")
	}
}

// f9a162b4 / cc3509b3: a failed horizon query omits the boolean. Spend in
// that window must not read as checked-and-covered in the comparison.
func TestDashboard_CompareMissingCostHorizon(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(js, "function appendWindowCaveat(host, which, dq, anonymised, since, totalCostUSD) {")
	if !ok {
		t.Fatal("appendWindowCaveat must receive the window spend")
	}
	want := "if (num(totalCostUSD) > 0 && typeof dq.window_predates_cost_capture !== 'boolean') { notes.push('cost horizon could not be checked'); }"
	if !strings.Contains(squashSpace(body), squashSpace(want)) {
		t.Errorf("spending window with missing/null/non-boolean horizon must warn: missing %q", want)
	}
	if strings.Index(body, "notes.push('cost horizon could not be checked')") > strings.Index(body, "if (notes.length === 0)") {
		t.Error("unknown horizon note must precede the clean-window early return")
	}
}

func TestDashboard_CompareMissingCostHorizonControl(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(js, "function appendWindowCaveat(host, which, dq, anonymised, since, totalCostUSD) {")
	if !ok {
		t.Fatal("appendWindowCaveat missing")
	}
	// Explicit false/true are checked answers; absent/null answers in costless
	// windows stay silent. Both controls depend on this conjunction, not truthiness.
	guard, ok := jsBlockAfter(body, "if (num(totalCostUSD) > 0 && typeof dq.window_predates_cost_capture !== 'boolean') {")
	if !ok || squashSpace(guard) != squashSpace("notes.push('cost horizon could not be checked');") {
		t.Error("unknown horizon note must be gated by spend AND a non-boolean result")
	}
	if strings.Count(body, "cost horizon could not be checked") != 1 {
		t.Error("unknown horizon note must only appear in the guarded branch")
	}
	if !strings.Contains(squashSpace(body), squashSpace("if (notes.length === 0) { return; }")) {
		t.Error("clean/costless window must retain the no-caveat early return")
	}
}
