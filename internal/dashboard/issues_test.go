package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

const issueLoadGenTouches = 3

func TestIssueReportWindowAndPrivacyGuards(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(jsStrip(js), "function loadIssueReport(since, until, gen, anonymized) {")
	if !ok {
		t.Fatal("missing issue report loader")
	}
	guard, ok := jsBlockAfter(body, "if (anonymized || sealedMode) {")
	if !ok || squashSpace(guard) != "return;" {
		t.Fatal("issue report must return in anonymized or sealed mode")
	}
	sq := squashSpace(body)
	suppression, fetch := strings.Index(sq, "if(anonymized||sealedMode)"), strings.Index(sq, "fetchJSON(")
	if suppression < 0 || fetch < suppression {
		t.Fatal("issue report fetch precedes privacy guard")
	}
	for _, callback := range []struct{ call, prefix string }{
		{".then(", "function(report){" + squashSpace(loadGenGuard)},
		{".catch(", "function(){" + squashSpace(loadGenGuard)},
	} {
		args, ok := jsCallArgs(body, callback.call)
		if !ok || len(args) != 1 || !strings.HasPrefix(squashSpace(args[0]), callback.prefix) {
			t.Fatal("stale issue report can render", callback.call)
		}
	}
	reset, ok := jsBlockAfter(jsStrip(js), "function resetViews() {")
	if !ok || !strings.Contains(reset, "'issue-report'") {
		t.Fatal("reset retains old issue report")
	}
	_, html := get(t, "/")
	if strings.Index(html, `id="issue-report"`) < strings.Index(html, `id="segments-note"`) || !strings.Contains(html, `id="issue-report" role="region" aria-labelledby="issue-report-title"`) {
		t.Fatal("issue report needs its own labelled region below segments note")
	}
}

func TestIssueReportDashboard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for dashboard DOM behavior test")
	}
	out, err := exec.Command(node, "testdata/issues.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
