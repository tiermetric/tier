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
	// 🔴 COMPARE THE SLICES, NOT A HARDCODED PAIR LIST (#466). A third table listing
	// the buckets by hand would be satisfied by any two sides that both forgot a new
	// one — which is exactly the failure that matters here, because
	// api.validateShippedIssueID derives the POST /api/v1/events allowlist from
	// store.UnattributedFamily(). A bucket added to ONE side ships from the collector
	// and is 400'd by the endpoint; that 4xx is terminal for the stateless shipper, so
	// it is not a degraded number, it is 100% permanent capture loss for every
	// developer who hits it. Length first, so a missing element is named as such.
	if len(UnattributedBucketIDs) != len(store.UnattributedBuckets) {
		t.Fatalf("bucket family diverged: collector has %d (%q), store has %d (%q). "+
			"A bucket added on one side only becomes a terminal 400 on /events.",
			len(UnattributedBucketIDs), UnattributedBucketIDs,
			len(store.UnattributedBuckets), store.UnattributedBuckets)
	}
	for i := range UnattributedBucketIDs {
		if UnattributedBucketIDs[i] != store.UnattributedBuckets[i] {
			t.Errorf("bucket[%d]: collector %q != store %q", i,
				UnattributedBucketIDs[i], store.UnattributedBuckets[i])
		}
	}
	// Every member of the family must satisfy the family matcher, and the bare
	// sentinel must be in UnattributedFamily() exactly once alongside them.
	fam := store.UnattributedFamily()
	if len(fam) != len(store.UnattributedBuckets)+1 || fam[0] != store.UnattributedIssueID {
		t.Errorf("UnattributedFamily() = %q; want the bare sentinel followed by all %d buckets",
			fam, len(store.UnattributedBuckets))
	}
	for _, id := range fam {
		if !IsUnattributed(id) {
			t.Errorf("UnattributedFamily() member %q is not matched by IsUnattributed", id)
		}
	}
	// The named constants stay pinned too, so a RENAME is still caught by name and not
	// merely by position.
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

// TestIsUnattributedMatchesStore pins collector.IsUnattributed and
// store.IsUnattributed to the SAME classification, over the same edge cases the
// store-side SQL agreement test uses.
//
// The two functions are byte-identical duplicates — store cannot import collector,
// because collector imports store — and store's doc comment claims exactly that. A
// claim with no test behind it is how the LIKE/GLOB drift survived its first guard:
// the CONSTANTS matched (TestUnattributedIssueIDMatchesStore above already pinned
// those) while the MATCHER diverged. Constants agreeing is not matchers agreeing.
//
// A divergence here would be the same defect shape as #466, one package over: the
// doctor's family match (cmd/tierd/doctor.go, via collector.IsUnattributed) and the
// /scores segment reconciliation (via store.IsUnattributed) would classify one
// stored row two different ways.
func TestIsUnattributedMatchesStore(t *testing.T) {
	cases := []struct {
		issueID string
		want    bool
	}{
		{store.UnattributedIssueID, true},
		{store.UnattributedMainBucket, true},
		{store.UnattributedDetachedHEADBucket, true},
		{store.UnattributedNoIssueBucket, true},
		{store.UnattributedIssueID + ":", true},
		{store.UnattributedIssueID + ":anything-at-all", true},
		// Case variants: the #466 forgery vector. BOTH must say false.
		{"UNATTRIBUTED", false},
		{"Unattributed", false},
		{"UNATTRIBUTED:main", false},
		{"Unattributed:Main", false},
		// Near-misses.
		{"unattributed-main", false},
		{"unattributedx", false},
		{"xunattributed:main", false},
		{"#42", false},
		{"ABC-123", false},
		{"", false},
	}
	for _, tc := range cases {
		gotCollector := IsUnattributed(tc.issueID)
		gotStore := store.IsUnattributed(tc.issueID)
		// Correctness first — two matchers wrong in the same way agree perfectly.
		if gotCollector != tc.want {
			t.Errorf("collector.IsUnattributed(%q) = %v, want %v", tc.issueID, gotCollector, tc.want)
		}
		if gotStore != tc.want {
			t.Errorf("store.IsUnattributed(%q) = %v, want %v", tc.issueID, gotStore, tc.want)
		}
		if gotCollector != gotStore {
			t.Errorf("MATCHER DRIFT on %q: collector=%v store=%v — the same stored row "+
				"would classify differently in the doctor and in /scores (#466)",
				tc.issueID, gotCollector, gotStore)
		}
	}
}
