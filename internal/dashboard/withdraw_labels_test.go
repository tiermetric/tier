package dashboard

import (
	"os/exec"
	"testing"
)

func TestWithdrawInferenceLabelsRendered(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the dashboard DOM behavior test")
	}
	out, err := exec.Command(node, "testdata/withdraw-labels.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard rendering: %v\n%s", err, out)
	}
}
