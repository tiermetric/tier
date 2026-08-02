// Package quality derives an outcome's quality multiplier from its append-only
// quality_events log (P2-03, #134). Quality is a PURE FUNCTION of the observed
// event set — never mutated in place — so it is re-derivable and replay-safe:
// re-running Resolve over the same events always yields the same multiplier.
//
// Phase 1 (quality-degradation-spec §9) implements a strict subset of the full
// eight-event model: CI pass/fail (Events 1-2), full-revert with
// strategic-vs-quality classification (Event 4), and the 30-minute flaky
// re-run rule. Follow-up fixes, partial reverts, hotfix branches, incident
// correlation, and downstream CI are DESIGNED in the spec but deliberately out
// of scope here; adding them later is a new event type plus a floor, not a
// rewrite of this resolver.
package quality

import (
	"regexp"
	"strings"

	"github.com/tiermetric/tier/internal/store"
)

// Event type identifiers. These MUST match the strings in store's
// validQualityEventTypes allowlist — they are the contract between the webhook
// append path (which writes them) and this resolver (which reads them).
const (
	// EventCIPass records a clean CI run on the merge commit. Audit value only;
	// it applies no floor.
	EventCIPass = "ci_pass"
	// EventCIFail records a failing CI run on the merge commit (floor 0.7),
	// unless neutralised by a matching EventCIFailFlaky.
	EventCIFail = "ci_fail"
	// EventCIFailFlaky reclassifies an earlier EventCIFail as flaky (a same-SHA
	// re-run succeeded within 30 minutes). It neutralises the failure rather
	// than mutating it, keeping quality_events append-only.
	EventCIFailFlaky = "ci_fail_flaky"
	// EventRevertQuality records a full revert attributed to a code problem
	// (floor 0.1) — the default when classification is ambiguous.
	EventRevertQuality = "revert_quality"
	// EventRevertStrategic records a full revert attributed to a business
	// decision (floor 0.8) — the code worked, but did not survive.
	EventRevertStrategic = "revert_strategic"
)

// Quality floors and clamp bounds (quality-degradation-spec §3, §5).
const (
	// FloorClean is the multiplier for an outcome with no degrading events.
	FloorClean = 1.0
	// FloorCIFail is the multiplier a CI failure on the merge commit floors to.
	FloorCIFail = 0.7
	// FloorRevertStrategic is the multiplier a business-reason revert floors to.
	FloorRevertStrategic = 0.8
	// FloorRevertQuality is the multiplier a code-problem revert floors to.
	FloorRevertQuality = 0.1
	// qualityMin / qualityMax are the hard clamp bounds. Quality never drops
	// below 0.1 (marginal learning value survives even a full revert — see spec
	// §3 Event 4 rationale) and never exceeds 1.0.
	qualityMin = 0.1
	qualityMax = 1.0
)

// Resolve computes the quality multiplier from an outcome's event set
// (quality-degradation-spec §5, Phase 1 subset). It takes the worst-of the
// applicable floors:
//
//	no events        -> 1.0 (clean ship)
//	ci_fail          -> 0.7 (neutralised by a matching ci_fail_flaky)
//	revert_strategic -> 0.8
//	revert_quality   -> 0.1
//	ci_pass          -> no effect (audit only)
//	ci_fail_flaky    -> no effect (it only neutralises a ci_fail)
//
// The result is clamped to [0.1, 1.0]. Phase-2 event types (follow-up fix,
// partial revert, hotfix, ...) are not present in quality_events yet and, if
// they were, fall through the switch's default with no effect until their
// floors are added.
//
// Invariant relied on downstream: this returns one of a small set of EXACT
// float64 constants (selection + worst-of + clamp, never arithmetic). The
// store's UpdateQualityForOutcome no-op guard (old == quality) depends on that
// exactness. A future Phase-2 rule that COMPUTES a value (e.g. additive
// stacking) must revisit that equality guard (epsilon/normalised compare).
//
// Complexity: O(n) over the event slice, one auxiliary map keyed by head SHA
// for flaky matching. Pure and safe for concurrent use (no shared state).
func Resolve(events []store.QualityEvent) float64 {
	if len(events) == 0 {
		return FloorClean
	}

	// A ci_fail is neutralised iff a ci_fail_flaky exists for the SAME merge
	// commit (matched on the head-SHA component of source_ref, "head_sha:attempt").
	// The 30-minute re-run rule appends the reclassifying flaky event rather
	// than mutating the failure, so both rows coexist and resolution must know
	// to drop the failure. Every CI run for one outcome shares the merge commit
	// SHA, so in practice a single flaky success clears the failure; matching on
	// SHA is defensive against any future multi-SHA event stream.
	flakySHAs := make(map[string]struct{})
	for _, e := range events {
		if e.EventType == EventCIFailFlaky {
			flakySHAs[HeadSHA(e.SourceRef)] = struct{}{}
		}
	}

	quality := FloorClean
	for _, e := range events {
		var floor float64
		switch e.EventType {
		case EventCIFail:
			if _, flaky := flakySHAs[HeadSHA(e.SourceRef)]; flaky {
				continue // neutralised by a matching flaky re-run success
			}
			floor = FloorCIFail
		case EventRevertQuality:
			floor = FloorRevertQuality
		case EventRevertStrategic:
			floor = FloorRevertStrategic
		default:
			// ci_pass, ci_fail_flaky, and any not-yet-scored Phase-2 type.
			continue
		}
		if floor < quality {
			quality = floor
		}
	}

	if quality < qualityMin {
		quality = qualityMin
	}
	if quality > qualityMax {
		quality = qualityMax
	}
	return quality
}

// HeadSHA returns the head-commit SHA component of a CI event's source_ref,
// which the webhook writes as "head_sha:run_attempt". A source_ref with no
// colon (e.g. a revert commit SHA) is returned unchanged.
func HeadSHA(sourceRef string) string {
	if i := strings.IndexByte(sourceRef, ':'); i >= 0 {
		return sourceRef[:i]
	}
	return sourceRef
}

// strategicPatterns and qualityPatterns are the deterministic keyword lists
// from quality-degradation-spec §3 Event 4. They are compiled once at package
// init. A revert is STRATEGIC only when it hits a strategic keyword and no
// quality keyword; every other case (including no keyword at all) defaults to
// QUALITY, because "when in doubt, the conservative interpretation is that the
// code had a problem".
var (
	strategicPatterns = mustCompileAll(
		`product decision`,
		`PM requested`,
		`feature flag.*disable`,
		`business requirement.*changed`,
		`pivot`,
		`deprecat`,
		`sunset`,
		`removing feature`,
		`no longer needed`,
		`replaced by`,
	)
	qualityPatterns = mustCompileAll(
		`broke`, `break`, `broken`,
		`crash`, `OOM`, `out of memory`,
		`regression`, `degradation`,
		`incident`, `outage`,
		`bug`, `defect`,
		`performance.*degrad`,
		`memory leak`,
		`data loss`, `corrupt`,
		`timeout`, `deadlock`,
		`security.*vuln`,
	)
)

// mustCompileAll compiles each pattern case-insensitively, panicking on a bad
// pattern. Called only at package init with compile-time-constant patterns, so
// a panic here is a programming error caught on first import, never a runtime
// input failure.
func mustCompileAll(patterns ...string) []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(`(?i)` + p)
	}
	return res
}

// ClassifyRevert maps a revert commit message to its quality-event type
// (quality-degradation-spec §3 Event 4). It returns EventRevertStrategic only
// when the text matches at least one strategic keyword and zero quality
// keywords; otherwise it returns EventRevertQuality (the conservative default).
//
// Only the commit message is inspected: the push webhook payload carries no PR
// body, and the spec's classifier treats message and body as one concatenated
// text — the message alone is the available signal on the push path.
func ClassifyRevert(message string) string {
	strategicHits := 0
	for _, re := range strategicPatterns {
		if re.MatchString(message) {
			strategicHits++
		}
	}
	qualityHits := 0
	for _, re := range qualityPatterns {
		if re.MatchString(message) {
			qualityHits++
		}
	}
	if strategicHits > 0 && qualityHits == 0 {
		return EventRevertStrategic
	}
	return EventRevertQuality
}
