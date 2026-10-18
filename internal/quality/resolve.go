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

	"github.com/tiermetric/tier/internal/qualityrule"
	"github.com/tiermetric/tier/internal/store"
)

// Event type identifiers. These MUST match the strings in store's
// validQualityEventTypes allowlist — they are the contract between the webhook
// append path (which writes them) and this resolver (which reads them).
const (
	// EventCIPass records a clean CI run on the merge commit. Audit value only;
	// it applies no floor.
	EventCIPass = qualityrule.EventCIPass
	// EventCIFail records a failing CI run on the merge commit (floor 0.7),
	// unless neutralised by a matching EventCIFailFlaky.
	EventCIFail = qualityrule.EventCIFail
	// EventCIFailFlaky reclassifies an earlier EventCIFail as flaky (a re-run
	// of the SAME WORKFLOW on the same merge commit succeeded within 30
	// minutes — #687; see CIKey). It neutralises the failure rather than
	// mutating it, keeping quality_events append-only.
	EventCIFailFlaky = qualityrule.EventCIFailFlaky
	// EventRevertQuality records a full revert attributed to a code problem
	// (floor 0.1) — the default when classification is ambiguous.
	EventRevertQuality = qualityrule.EventRevertQuality
	// EventRevertStrategic records a full revert attributed to a business
	// decision (floor 0.8) — the code worked, but did not survive.
	EventRevertStrategic = qualityrule.EventRevertStrategic
	// EventRevertAmbiguous records a checked, unresolved revert; never evidence or a floor.
	EventRevertAmbiguous = qualityrule.EventRevertAmbiguous
)

// Quality floors (quality-degradation-spec §3, §5).
const (
	// FloorClean is the multiplier for an outcome with no degrading events.
	FloorClean = qualityrule.FloorClean
	// FloorCIFail is the multiplier a CI failure on the merge commit floors to.
	FloorCIFail = qualityrule.FloorCIFail
	// FloorRevertStrategic is the multiplier a business-reason revert floors to.
	FloorRevertStrategic = qualityrule.FloorRevertStrategic
	// FloorRevertQuality is the multiplier a code-problem revert floors to.
	FloorRevertQuality = qualityrule.FloorRevertQuality
)

// Resolve computes the quality multiplier from an outcome's event set
// (quality-degradation-spec §5, Phase 1 subset). It takes the worst-of the
// applicable floors:
//
//	no events        -> 1.0 (clean ship)
//	ci_fail          -> 0.7 (neutralised by a matching ci_fail_flaky)
//	revert_strategic -> 0.8
//	revert_quality   -> 0.1
//	revert_ambiguous -> no effect (checked, unresolved; never evidence)
//	ci_pass          -> no effect (audit only)
//	ci_fail_flaky    -> no effect (it only neutralises a ci_fail)
//
// The result is selected from fixed levels within [0.1, 1.0]. Phase-2 event
// types (follow-up fix, partial revert, hotfix, ...) are not present in
// quality_events yet and, if they were, fall through the switch's default with
// no effect until their floors are added.
//
// Invariant relied on downstream: this returns one of a small set of EXACT
// float64 constants (selection + worst-of, never arithmetic). The
// store's UpdateQualityForOutcome no-op guard (old == quality) depends on that
// exactness. A future Phase-2 rule that COMPUTES a value (e.g. additive
// stacking) must revisit that equality guard (epsilon/normalised compare).
//
// Complexity: O(n) over the event slice, one auxiliary map keyed by
// (head SHA, workflow id) for flaky matching. Pure and safe for concurrent use
// (no shared state).
func Resolve(events []store.QualityEvent) float64 {
	facts := make([]qualityrule.Event, len(events))
	for i, e := range events {
		ref := ParseCIRef(e.SourceRef)
		facts[i] = qualityrule.Event{
			TriggerEvent: e.TriggerEvent, EventType: e.EventType, HeadSHA: ref.HeadSHA, WorkflowID: ref.WorkflowID,
		}
	}
	return qualityrule.Resolve(facts).Float64
}

// CIKey is the identity two CI events must SHARE for one to say anything about
// the other: the merge commit the runs were for, and the workflow they belong
// to. A ci_fail_flaky neutralises a ci_fail iff their CIKeys are equal.
//
// 🔴 WorkflowID IS THE #687 FIX. Until then the match was head SHA alone, so any
// green workflow on the merge commit within 30 minutes of any failing one wiped
// the failure — the ORDINARY case in a repo with CI + lint + docs + CodeQL, and
// exploitable with push rights alone (add one trivially-green workflow and this
// repo's CI failures stop scoring). It is the workflow's numeric id, never its
// name: a name is user-controlled, mutable — renaming a workflow would orphan
// every event already written — and may legally contain a colon, which is the
// delimiter. The id is assigned by GitHub and survives renames.
//
// WorkflowID is "" for a source_ref written BEFORE #687 ("head_sha:attempt"),
// which carried no workflow identity. "" equals only "", so:
//
//   - legacy <-> legacy still pairs on head SHA alone => re-resolving an outcome
//     whose events all predate this change yields the quality it always did.
//     History is not silently reinterpreted.
//   - legacy <-> post-#687 never pairs => across the deploy boundary a failure
//     STANDS rather than being cleared by an unidentifiable success. That is
//     the conservative direction, and it is reachable only by a failure and a
//     success straddling the deploy inside one 30-minute window.
//
// CIKey is comparable so callers can compare identities directly. The qualityrule
// resolver uses the equivalent (HeadSHA, WorkflowID) pair as its flaky map key.
type CIKey struct {
	HeadSHA    string
	WorkflowID string
}
type CIRef struct {
	CIKey
	RunAttempt int
}

func BuildCISourceRef(headSHA string, runAttempt int, workflowID int64) string {
	return qualityrule.BuildCISourceRef(headSHA, runAttempt, workflowID)
}
func ParseCIRef(sourceRef string) CIRef {
	ref := qualityrule.ParseCIRef(sourceRef)
	return CIRef{CIKey: CIKey{HeadSHA: ref.HeadSHA, WorkflowID: ref.WorkflowID}, RunAttempt: ref.RunAttempt}
}

// strategicPatterns and qualityPatterns are the deterministic keyword lists
// from quality-degradation-spec §3 Event 4. They are compiled once at package
// init. A revert is STRATEGIC only when it hits a strategic keyword and no
// quality keyword; every other case (including no keyword at all) defaults to
// QUALITY, because "when in doubt, the conservative interpretation is that the
// code had a problem". Bound keywords and their inflections to whole words so
// unrelated words cannot veto a strategic reason. Wildcard phrases span lines
// because commit-message bodies may wrap between the keywords.
var (
	strategicPatterns = mustCompileAll(
		`\bproduct decisions?\b`,
		`\bPM requested\b`,
		`(?s)\bfeature flags?\b.*\bdisabl(?:e|es|ed|ing)\b`,
		`(?s)\bbusiness requirements?\b.*\bchanged?\b`,
		`\bpivot(?:s|ed|ing)?\b`,
		`\bdeprecat(?:e[ds]?|ing|ions?)\b`,
		`\bsunset(?:s|ted|ting)?\b`,
		`\bremoving features?\b`,
		`\bno longer needed\b`,
		`\breplaced by\b`,
	)
	qualityPatterns = mustCompileAll(
		`\bbroke(?:n)?\b`, `\bbreak(?:s|ing|ages?)?\b`,
		`\bcrash(?:es|ed|ing)?\b`, `\bOOM(?:s|ed|ing)?\b`, `\bout of memory\b`,
		`\bregressions?\b`, `\bdegradations?\b`,
		`\bincidents?\b`, `\boutages?\b`,
		`\bbug(?:s|gy|fix(?:es)?)?\b`, `\bdefect(?:s|ive)?\b`,
		`(?s)\bperformance\b.*\bdegrad(?:e[ds]?|ing|ations?)\b`,
		`\bmemory leaks?\b`,
		`\bdata loss(?:es)?\b`, `\bcorrupt(?:s|ed|ing|ions?)?\b`,
		`\btimeouts?\b`, `\bdeadlock(?:s|ed|ing)?\b`,
		`(?s)\bsecurity\b.*\bvuln(?:s|erab(?:le|ility|ilities))?\b`,
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
