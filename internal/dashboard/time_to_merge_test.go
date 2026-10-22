package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

const timeToMergeLoadGenTouches = 3

func TestTimeToMergeDashboardWindowAndPrivacyGuards(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(jsStrip(js), "function loadTimeToMerge(since, until, gen) {")
	if !ok {
		t.Fatal("missing time-to-merge loader")
	}
	for _, callback := range []struct{ call, prefix string }{
		{".then(", "function(report){" + squashSpace(loadGenGuard)},
		{".catch(", "function(e){" + squashSpace(loadGenGuard)},
	} {
		args, ok := jsCallArgs(body, callback.call)
		if !ok || len(args) != 1 || !strings.HasPrefix(squashSpace(args[0]), callback.prefix) {
			t.Fatal("stale time-to-merge callback", callback.call)
		}
	}
	if !strings.Contains(body, "encodeURIComponent(since)") || !strings.Contains(body, "encodeURIComponent(until)") || !strings.Contains(body, "e.status === 403") || !strings.Contains(body, "?period=") || !strings.Contains(body, "encodeURIComponent(selectedPeriod)") {
		t.Fatal("missing window or real-data developer guard")
	}
	_, html := get(t, "/")
	for _, text := range []string{"Time to merge · PR created → merged (includes draft time)", "smaller or later-created PRs merge faster; not a quality measure", "overflow-wrap: anywhere; white-space: normal;", `class="cmp-db-sr" id="kpi-time-to-merge-p90"`} {
		if !strings.Contains(html, text) {
			t.Fatal("missing tile wording or wrapping:", text)
		}
	}
	start := strings.Index(html, `id="kpi-time-to-merge"`)
	if start < strings.Index(html, `id="kpi-tier"`) || start > strings.Index(html, `id="kpi-spend"`) {
		t.Fatal("tile must be beside headline")
	}
}

func TestTimeToMergeDashboard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for dashboard DOM behavior test")
	}
	out, err := exec.Command(node, "testdata/time-to-merge.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
