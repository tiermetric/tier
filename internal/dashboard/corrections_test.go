package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Corrective context inherits its parent's captured generation and checks it
// once in each asynchronous callback. The synchronous renderer captures the
// current generation once; corrective context never advances it.
const correctionLoadGenTouches = 3

func TestDashboard_CorrectiveContextKeepsWindowAndPrivacy(t *testing.T) {
	_, js := get(t, jsPath)
	body, ok := jsBlockAfter(jsStrip(js), "function loadCorrectionContext(since, until, gen, anonymized) {")
	if !ok {
		t.Fatal("missing corrective context loader")
	}
	for _, callback := range []struct{ call, prefix string }{
		{".then(", "function(parts){" + squashSpace(loadGenGuard)},
		{".catch(", "function(){" + squashSpace(loadGenGuard)},
	} {
		args, ok := jsCallArgs(body, callback.call)
		if !ok || len(args) != 1 || !strings.HasPrefix(squashSpace(args[0]), callback.prefix) {
			t.Fatal("stale corrective context can render", callback.call)
		}
	}
	sq := squashSpace(body)
	suppression := strings.Index(sq, "if(anonymized||sealedMode)")
	fetch := strings.Index(sq, "fetchJSON(")
	if suppression < 0 || fetch < suppression {
		t.Fatal("anonymized corrective context fetches personal evidence")
	}
	reset, ok := jsBlockAfter(jsStrip(js), "function resetViews() {")
	if !ok || !strings.Contains(reset, "'correction-context'") {
		t.Fatal("mode/window switch retains old corrective context")
	}
}

func TestCorrectionReview_R13LabelsAndPrivacy(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the correction renderer DOM behavior test")
	}
	out, err := exec.Command(node, "testdata/correction-labels.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
