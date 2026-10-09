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
	"strconv"
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
	// EventCIFailFlaky reclassifies an earlier EventCIFail as flaky (a re-run
	// of the SAME WORKFLOW on the same merge commit succeeded within 30
	// minutes — #687; see CIKey). It neutralises the failure rather than
	// mutating it, keeping quality_events append-only.
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
// Complexity: O(n) over the event slice, one auxiliary map keyed by
// (head SHA, workflow id) for flaky matching. Pure and safe for concurrent use
// (no shared state).
func Resolve(events []store.QualityEvent) float64 {
	if len(events) == 0 {
		return FloorClean
	}

	// A ci_fail is neutralised iff a ci_fail_flaky exists for the SAME merge
	// commit AND THE SAME WORKFLOW (CIKey — #687). The 30-minute re-run rule
	// appends the reclassifying flaky event rather than mutating the failure, so
	// both rows coexist and resolution must know to drop the failure.
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
	// (fail@1, flaky@2, fail@3 -> 1.0; pinned in TestResolve). Reviewers have
	// twice proposed gating the attempt rule on `WorkflowID != ""`, which would
	// leave every legacy pair untouched and make that later failure stand.
	// DECLINED, on the merits rather than only on back-compat: the events all
	// describe ONE unchanged commit, and a workflow that went fail -> pass ->
	// fail on identical code is the textbook definition of a flaky test. The
	// 0.7 floor is for "this merge broke the build", and a run that has
	// demonstrably passed on this exact tree has not shown that. If this is
	// ever revisited, revisit that argument — not the back-compat one, which
	// only explains why it must not be applied to legacy rows.
	flakyRuns := make(map[CIKey]struct{})
	for _, e := range events {
		if e.EventType == EventCIFailFlaky {
			flakyRuns[ParseCIRef(e.SourceRef).CIKey] = struct{}{}
		}
	}

	quality := FloorClean
	for _, e := range events {
		var floor float64
		switch e.EventType {
		case EventCIFail:
			if _, flaky := flakyRuns[ParseCIRef(e.SourceRef).CIKey]; flaky {
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
// It is a comparable struct so Resolve can use it as a map key directly.
type CIKey struct {
	HeadSHA    string
	WorkflowID string
}

// CIRef is a parsed CI source_ref: the CIKey identity plus the run attempt.
// RunAttempt is 0 when the source_ref carries no parseable attempt (a revert
// commit SHA, or anything malformed).
type CIRef struct {
	CIKey
	RunAttempt int
}

// BuildCISourceRef encodes a CI event's source_ref as
// "head_sha:run_attempt:workflow_id".
//
// 🔑 WHY source_ref AND NOT A NEW COLUMN. The unique key is
// (outcome_id, event_type, source_ref). With workflow identity OUTSIDE it, two
// different workflows failing on the same commit at the same attempt collide on
// that key and the second ci_fail is dropped by ON CONFLICT DO NOTHING — so the
// failure a later matching flaky success must NOT clear would never have been
// stored to begin with. Identity has to be in the key, and once it is in the key
// a parallel column is a second representation of the same fact that can
// disagree with it. Resolve also sees only []store.QualityEvent, so source_ref
// is the one channel that reaches it without a schema change.
//
// UNAMBIGUITY, stated as what is ENFORCED rather than what is hoped.
//
// The last two components are colon-free by construction — they are strconv
// output over non-negative ints, /^[0-9]+$/ — and ParseCIRef anchors on the
// RIGHT, so the head-SHA field may contain anything at all and still round
// trips. The workflow NAME, the only user-controlled colon-admitting field in
// the payload, is never parsed and never encoded. ⇒ no GitHub input, legal or
// not, can make one shape read as another.
//
// ⚠️ THE HEAD SHA IS NOT VALIDATED, AND AN EARLIER VERSION OF THIS COMMENT
// CLAIMED IT WAS. It said head_sha "is matched against a stored 40-hex SHA".
// It is not: outcomes.merge_commit_sha is TEXT with no CHECK, and
// validateOutcomeRequest enforces only non-empty and a length cap — the comment
// at internal/api/outcomes.go:136 says so outright. A bearer-token holder can
// therefore store a colon-bearing merge_commit_sha, and (with the webhook HMAC
// secret, which is what it takes to name it as a head_sha) reach this encoder
// with a colon in headSHA. Right-anchored parsing is what makes that harmless
// for the three-component form: "a:b" at attempt 1 of workflow 42 encodes to
// "a:b:1:42" and parses back to exactly ("a:b", 1, 42).
//
// ⚠️ ONE RESIDUAL AMBIGUITY, AND IT IS INHERENT TO A DELIMITER-FREE ENCODING
// OVER AN UNVALIDATED FIELD: in the UNIDENTIFIED fallback below, headSHA "a:7"
// at attempt 1 and headSHA "a" at attempt 7 both encode to "a:7:1"... in fact
// the first is "a:7:1" and the second is "a:7", so they differ — but headSHA
// "a:7" at attempt 1 (unidentified) collides with headSHA "a" at attempt 7 of
// workflow 1. It is not exploitable: matching is scoped to ONE outcome_id and
// merge_commit_sha is UNIQUE, so every event on an outcome shares one headSHA,
// and a colon-bearing one makes RunAttempt constant across the event set while
// CIKey varies — which makes `success.RunAttempt > failed.RunAttempt`
// unsatisfiable. ⇒ a poisoned head SHA can only DISABLE flaky neutralisation
// for that outcome, never cause one. Fail-closed, and pinned by
// TestPoisonedHeadSHAIsFailClosed.
//
// A non-positive workflowID (absent from the payload, or crafted) falls back to
// the legacy two-component form: an event whose workflow cannot be identified
// gets the identity "unknown" rather than a fabricated one, and unknown pairs
// only with unknown. A negative runAttempt (impossible from GitHub, reachable
// only by a forged HMAC-signed payload) is clamped to 0 so the ref stays
// parseable rather than silently degrading to the opaque shape.
func BuildCISourceRef(headSHA string, runAttempt int, workflowID int64) string {
	if runAttempt < 0 {
		runAttempt = 0
	}
	ref := headSHA + ":" + strconv.Itoa(runAttempt)
	if workflowID <= 0 {
		return ref
	}
	return ref + ":" + strconv.FormatInt(workflowID, 10)
}

// ParseCIRef parses a CI event's source_ref. It accepts, in order:
//
//	"head_sha:attempt:workflow_id"  (#687)   -> full identity
//	"head_sha:attempt"              (legacy) -> WorkflowID ""
//	anything else (e.g. a revert commit SHA) -> HeadSHA = text before the first
//	                                            colon, WorkflowID "", attempt 0
//
// The final case preserves the pre-#687 HeadSHA contract for every non-CI
// source_ref, so nothing that used to parse now parses differently.
func ParseCIRef(sourceRef string) CIRef {
	// 🔑 ANCHOR ON THE RIGHT, NOT THE LEFT. The trailing two fields are the
	// encoder's own strconv output; the head-SHA field is the only one that can
	// contain anything. Splitting from the left and demanding exactly three
	// fields would silently LOSE the identity of any event whose head SHA
	// carries a colon (it would read as four fields and fall through to the
	// opaque branch), which is the one case where getting it right matters.
	if wfAt := strings.LastIndexByte(sourceRef, ':'); wfAt > 0 {
		if attemptAt := strings.LastIndexByte(sourceRef[:wfAt], ':'); attemptAt >= 0 {
			attempt, aok := parseDecimal(sourceRef[attemptAt+1 : wfAt])
			if aok && isPositiveDecimal(sourceRef[wfAt+1:]) {
				return CIRef{
					CIKey:      CIKey{HeadSHA: sourceRef[:attemptAt], WorkflowID: sourceRef[wfAt+1:]},
					RunAttempt: attempt,
				}
			}
		}
		// Legacy two-component form.
		if attempt, ok := parseDecimal(sourceRef[wfAt+1:]); ok {
			return CIRef{CIKey: CIKey{HeadSHA: sourceRef[:wfAt]}, RunAttempt: attempt}
		}
	}
	// Not a CI source_ref shape (a revert commit SHA, or anything malformed).
	// The head-SHA component is the text before the FIRST colon — the pre-#687
	// HeadSHA contract, preserved verbatim so nothing that used to parse now
	// parses differently.
	if i := strings.IndexByte(sourceRef, ':'); i >= 0 {
		return CIRef{CIKey: CIKey{HeadSHA: sourceRef[:i]}}
	}
	return CIRef{CIKey: CIKey{HeadSHA: sourceRef}}
}

// isPositiveDecimal reports whether s is a run of ASCII digits containing at
// least one non-zero — i.e. a workflow id the encoder would have emitted.
//
// ⚠️ It deliberately does NOT convert to an integer. strconv.Atoi is int-wide,
// so on a 32-bit build a workflow_id >= 2^31 would FAIL to parse on read while
// BuildCISourceRef (int64 / FormatInt) wrote it happily — a silent read/write
// asymmetry that would degrade such an event to the legacy form and reinstate
// #687 for it. Comparing digits has no width at all.
//
// Consequence worth knowing: the workflow id is kept as the raw substring, so
// "007" and "7" are DISTINCT identities. Unreachable from BuildCISourceRef,
// which never emits leading zeros; it would only bite a hand-written or
// imported row, and it errs toward not-matching. Do not "normalise" it without
// re-reading the back-compat rule on CIKey.
func isPositiveDecimal(s string) bool {
	nonZero := false
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		if s[i] != '0' {
			nonZero = true
		}
	}
	return nonZero
}

// parseDecimal accepts only a non-empty run of ASCII digits, so "+1", " 1" and
// "0x1" are all rejected. strconv.Atoi alone would accept a leading sign, which
// would let two spellings of one attempt produce two different source_refs for
// one run.
func parseDecimal(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false // overflow on an absurdly long digit run
	}
	return n, true
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
