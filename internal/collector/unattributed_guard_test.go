package collector

import (
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestUnattributedIssueIDMatchesStore pins collector.UnattributedIssueID equal to
// store.UnattributedIssueID. The capture paths in this package WRITE the sentinel
// and store.CostCompositionWindow (#234) SPLITS attributed vs unattributed spend
// on it; store cannot import collector (collector imports store), so the value is
// duplicated and this guard is the mechanical check that the two never drift —
// a drift would silently mis-bucket every exploration dollar.
func TestUnattributedIssueIDMatchesStore(t *testing.T) {
	if UnattributedIssueID != store.UnattributedIssueID {
		t.Fatalf("collector.UnattributedIssueID = %q, store.UnattributedIssueID = %q: the two MUST match",
			UnattributedIssueID, store.UnattributedIssueID)
	}
	// The labeled sub-buckets (#refocus, Option B) are written here and read by
	// store's family match + the /scores bucket split; pin each mirror equal so a
	// rename on one side cannot silently drop a bucket from the coverage math.
	for _, p := range []struct{ collectorVal, storeVal, name string }{
		{UnattributedMain, store.UnattributedMainBucket, "main"},
		{UnattributedDetachedHEAD, store.UnattributedDetachedHEADBucket, "detached-head"},
		{UnattributedNoIssue, store.UnattributedNoIssueBucket, "branch-without-issue"},
	} {
		if p.collectorVal != p.storeVal {
			t.Errorf("bucket %s: collector %q != store %q: the two MUST match",
				p.name, p.collectorVal, p.storeVal)
		}
		if !IsUnattributed(p.collectorVal) {
			t.Errorf("bucket %s: IsUnattributed(%q) = false, want true (family match broken)",
				p.name, p.collectorVal)
		}
	}
}
