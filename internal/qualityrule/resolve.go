// Package qualityrule resolves CI/revert quality and dormant Event 3 correction
// quality from occurrence facts, without storage or scoring dependencies.
package qualityrule

import (
	"math/big"
	"time"
)

const (
	EventCIPass          = "ci_pass"
	EventCIFail          = "ci_fail"
	EventCIFailFlaky     = "ci_fail_flaky"
	EventRevertAmbiguous = "revert_ambiguous"
	EventRevertQuality   = "revert_quality"
	EventRevertStrategic = "revert_strategic"

	FloorClean            = 1.0
	FloorCleanCorrection  = 0.85
	FloorCIFail           = 0.7
	FloorCIFailCorrection = 0.55
	FloorRevertStrategic  = 0.8
	FloorRevertQuality    = 0.1

	CIObservationWindow = 48 * time.Hour

	CorrectionWindow = 14 * 24 * time.Hour
)

// IsEvidence is an explicit allowlist. An unresolved check and every unknown
// future type are not evidence; row existence alone never establishes coverage.
func IsEvidence(eventType string) bool {
	switch eventType {
	case EventCIPass, EventCIFail, EventCIFailFlaky, EventRevertQuality, EventRevertStrategic:
		return true
	default:
		return false
	}
}

// ObservationStatus describes this check, independently of the numeric floor.
func ObservationStatus(eventType string) string {
	if IsEvidence(eventType) {
		return "observed"
	}
	return "unknown"
}

// Event carries parsed facts. A recorded flaky event neutralises all failures
// with the same SHA and workflow, regardless of run attempt or event order.
// An unidentified legacy workflow matches only another unidentified workflow.
// The adapter owns source-reference parsing and flaky-event adjudication.
type Event struct {
	TriggerEvent *string
	EventType    string
	HeadSHA      string
	WorkflowID   string
}

// Correction contains only merge occurrence evidence; recording and confirmation
// timestamps cannot stand in for it. Zero timestamps mean missing evidence.
// Callers supply relations for this original; both flags must hold to count.
type Correction struct {
	Active        bool
	Confirmed     bool
	ChildMergedAt time.Time
}

// Result supplies the canonical float literal and its exact policy rational.
// Rational is freshly allocated and belongs to the caller.
type Result struct {
	Rational *big.Rat
	Float64  float64
}

type level uint8

const (
	clean level = iota
	cleanCorrection
	ciFail
	ciFailCorrection
	strategicRevert
	qualityRevert
)

// Integer ratios are the source of truth, never a conversion from binary floats.
// Floats are selected literals to preserve store equality/no-op contracts.
var levels = [...]struct {
	numerator, denominator int64
	value                  float64
}{
	clean:            {1, 1, FloorClean},
	cleanCorrection:  {17, 20, FloorCleanCorrection},
	ciFail:           {7, 10, FloorCIFail},
	ciFailCorrection: {11, 20, FloorCIFailCorrection},
	strategicRevert:  {4, 5, FloorRevertStrategic},
	qualityRevert:    {1, 10, FloorRevertQuality},
}

// Resolve preserves legacy CI/flaky/revert quality. It never applies Event 3.
func Resolve(events []Event) Result {
	return resolve(events, false)
}

// ResolveWithCorrection applies the correction indicator once, then takes MIN
// with revert floors. This preview is not wired into any scoring path.
func ResolveWithCorrection(events []Event, originalMergedAt time.Time, corrections []Correction) Result {
	corrected := false
	if !originalMergedAt.IsZero() {
		// Strip monotonic readings: these are wall-clock merge occurrences.
		original := originalMergedAt.Round(0)
		for _, c := range corrections {
			if !c.Active || !c.Confirmed || c.ChildMergedAt.IsZero() {
				continue
			}
			delta := c.ChildMergedAt.Round(0).Sub(original)
			if delta >= 0 && delta <= CorrectionWindow {
				corrected = true
				break
			}
		}
	}
	return resolve(events, corrected)
}

func resolve(events []Event, corrected bool) Result {
	// A ci_fail is neutralised iff a ci_fail_flaky exists for the SAME merge
	// commit AND THE SAME WORKFLOW (head SHA, workflow id — #687). The 30-minute
	// re-run rule appends the reclassifying flaky event rather than mutating the
	// failure, so both rows coexist and resolution must know to drop the failure.
	//
	// ⚠️ The workflow half is not belt-and-braces on top of the classifier. The
	// classifier decides what to WRITE at delivery time; this decides what a
	// stored set MEANS, and it runs over rows the classifier never saw — replays,
	// crash recovery, and every future re-derivation. Matching here on head SHA
	// alone would let one workflow's flaky success clear another's failure no
	// matter how carefully the write side was gated.
	//
	// ⚠️ AND IT DELIBERATELY IGNORES run_attempt, which the classifier does not.
	// Pairing here answers "which failure does this flaky row speak for", and a
	// stored ci_fail_flaky was already adjudicated a genuine re-run when it was
	// written. Re-imposing the attempt rule here would re-judge history: the
	// most common pre-#687 row pair is a ci_fail and a ci_fail_flaky BOTH at
	// attempt 1 (the bug's own signature), and outcomes scored 1.0 for months
	// would silently drop to 0.7 the next time anything re-resolved them.
	//
	// ⚠️ AND THE COST OF THAT CHOICE, STATED SO IT IS A DECISION AND NOT AN
	// ACCIDENT: once a workflow's flaky success is on record, a LATER failure
	// of that same workflow on that same commit is also neutralised
	// (fail@1, flaky@2, fail@3 -> 1.0; pinned in quality.TestResolve). Reviewers
	// twice proposed gating the attempt rule on `WorkflowID != ""`, which would
	// leave every legacy pair untouched and make that later failure stand.
	// DECLINED, on the merits rather than only on back-compat: the events all
	// describe ONE unchanged commit, and a workflow that went fail -> pass ->
	// fail on identical code is the textbook definition of a flaky test. The
	// 0.7 floor is for "this merge broke the build", and a run that has
	// demonstrably passed on this exact tree has not shown that. If this is
	// ever revisited, revisit that argument — not the back-compat one, which
	// only explains why it must not be applied to legacy rows.
	flaky := make(map[[2]string]struct{})
	for _, e := range events {
		if IsCI(e.EventType) && e.TriggerEvent != nil && *e.TriggerEvent != "push" {
			continue
		}
		if e.EventType == EventCIFailFlaky {
			flaky[[2]string{e.HeadSHA, e.WorkflowID}] = struct{}{}
		}
	}

	ci, revert := clean, clean
	for _, e := range events {
		if IsCI(e.EventType) && e.TriggerEvent != nil && *e.TriggerEvent != "push" {
			continue
		}
		switch e.EventType {
		case EventCIFail:
			if _, ok := flaky[[2]string{e.HeadSHA, e.WorkflowID}]; !ok {
				ci = ciFail
			}
		case EventRevertQuality:
			revert = qualityRevert
		case EventRevertStrategic:
			if revert != qualityRevert {
				revert = strategicRevert
			}
		}
	}
	if corrected {
		if ci == ciFail {
			ci = ciFailCorrection
		} else {
			ci = cleanCorrection
		}
	}
	chosen := levels[ci]
	if floor := levels[revert]; floor.value < chosen.value {
		chosen = floor
	}
	return Result{big.NewRat(chosen.numerator, chosen.denominator), chosen.value}
}

// IsCI is closed over the CI event kinds.
func IsCI(kind string) bool {
	switch kind {
	case EventCIPass, EventCIFail, EventCIFailFlaky:
		return true
	}
	return false
}

// NormalizeTrigger bounds untrusted GitHub trigger provenance to a fixed vocabulary.
func NormalizeTrigger(event string) string {
	switch event {
	case "push", "pull_request", "schedule", "workflow_dispatch", "merge_group", "pull_request_target", "release", "workflow_run", "repository_dispatch":
		return event
	}
	return "other"
}
