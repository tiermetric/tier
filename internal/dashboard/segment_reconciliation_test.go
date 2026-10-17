package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboard_SegmentReconciliationGolden(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the segment reconciliation DOM behavior test")
	}
	out, err := exec.Command(node, "testdata/segment-reconciliation.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard rendering: %v\n%s", err, out)
	}
}

// This contract check still runs on Go-only hosts, where the DOM test skips.
func TestDashboard_SegmentReconciliationStructure(t *testing.T) {
	_, js := get(t, jsPath)
	render, ok := jsBlockAfter(js, "function renderSegments(data, teamMode, since, until) {")
	if !ok {
		t.Fatal("missing segment renderer")
	}
	for _, want := range []string{
		"container.textContent = '';",
		"var segments = teamMode ? [] : (data.work_types || []);",
		"if (!teamMode) {",
		"data.segment_reconciliation && data.segment_reconciliation.total",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("segment reconciliation lifetime/privacy contract missing %q", want)
		}
	}
	textElement, ok := jsBlockAfter(js, "function el(tag, cls, text) {")
	if !ok || !strings.Contains(textElement, "node.textContent = text;") {
		t.Fatal("element helper must render text via textContent")
	}
	reconciliation, ok := jsBlockAfter(js, "function appendSegmentReconciliation(panel, total, headingID) {")
	if !ok {
		t.Fatal("missing reconciliation renderer")
	}
	for _, want := range []string{
		"'outcome_linked_cost'",
		"'no_outcome_cost'",
		"'unattributed_cost'",
		"'window_cost'",
		"usdText(total[field[1] + '_usd'])",
		"total[field[1] + '_micro']",
	} {
		if !strings.Contains(reconciliation, want) {
			t.Errorf("reconciliation wire field contract missing %q", want)
		}
	}
	for _, banned := range []string{"innerHTML", "insertAdjacentHTML", "parseFloat", "parseInt", ".reduce("} {
		if strings.Contains(reconciliation, banned) {
			t.Errorf("reconciliation must render server values as text without recomputing costs: %s", banned)
		}
	}
	_, html := get(t, "/")
	style, ok := jsBlockAfter(html, ".segment-reconciliation {")
	if !ok || !strings.Contains(style, "overflow-wrap: anywhere;") {
		t.Error("reconciliation must wrap within its panel at narrow widths")
	}
}
