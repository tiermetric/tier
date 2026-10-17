package qualityrule

import "testing"

func TestUnresolvedIsNeverEvidenceOrPenalty(t *testing.T) {
	for _, kind := range []string{EventRevertAmbiguous, "future_type", ""} {
		t.Run(kind, func(t *testing.T) {
			if IsEvidence(kind) || ObservationStatus(kind) != "unknown" {
				t.Fatal("unresolved/unknown type accepted as evidence")
			}
			events := []Event{{EventType: kind, HeadSHA: "sha", WorkflowID: "1"}}
			if got := Resolve(events).Float64; got != 1 {
				t.Fatalf("unresolved check applies floor %v", got)
			}
			events = append(events, Event{EventType: EventCIFail, HeadSHA: "sha", WorkflowID: "1"})
			if got := Resolve(events).Float64; got != 0.7 {
				t.Fatalf("unresolved check clears failure: %v", got)
			}
			events = append(events, Event{EventType: EventRevertQuality})
			if got := Resolve(events).Float64; got != 0.1 {
				t.Fatalf("unresolved check changes existing penalty: %v", got)
			}
		})
	}
	for _, kind := range []string{EventCIPass, EventCIFail, EventCIFailFlaky, EventRevertQuality, EventRevertStrategic} {
		if !IsEvidence(kind) || ObservationStatus(kind) != "observed" {
			t.Errorf("known evidence %q rejected", kind)
		}
	}
}
