package quality

import (
	"github.com/tiermetric/tier/internal/store"
	"testing"
)

func TestResolve_RevertAmbiguousNeverPenaltyOrEvidence(t *testing.T) {
	for _, kind := range []string{"revert_ambiguous", "future_type"} {
		events := []store.QualityEvent{{EventType: kind, SourceRef: "sha:1:7"}}
		if Resolve(events) != 1 || events[0].IsEvidence() || events[0].Status() != "unknown" {
			t.Fatalf("unresolved check misclassified: %+v", events)
		}
		events = append(events, store.QualityEvent{EventType: EventCIFail, SourceRef: "sha:1:7"})
		if Resolve(events) != 0.7 {
			t.Fatal("unknown/ambiguous clears CI failure")
		}
		events = append(events, store.QualityEvent{EventType: EventRevertQuality})
		if Resolve(events) != 0.1 {
			t.Fatal("unknown/ambiguous changes existing revert penalty")
		}
	}
}
