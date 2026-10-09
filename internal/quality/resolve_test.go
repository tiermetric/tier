package quality

import (
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// ev is a terse constructor for a quality event in these table tests. Only the
// fields Resolve inspects (EventType, SourceRef) are set.
func ev(eventType, sourceRef string) store.QualityEvent {
	return store.QualityEvent{EventType: eventType, SourceRef: sourceRef}
}

// TestResolve mirrors quality-degradation-spec §10 test IDs where noted.
func TestResolve(t *testing.T) {
	const sha = "abc123"
	cases := []struct {
		name   string
		events []store.QualityEvent
		want   float64
	}{
		// ⚠️ The arms below spell source_refs as "sha:N" — the LEGACY
		// two-component form. They predate #687 and are left as-is on purpose:
		// they double as coverage that a pre-#687 event set still resolves the
		// way it always did. Current-format coverage is the ":42"-bearing arms
		// further down; do not read these as exercising the new encoding.
		{"clean ship, no events (TEST 1)", nil, 1.0},
		{"ci_pass only", []store.QualityEvent{ev(EventCIPass, sha+":1")}, 1.0},
		{"ci_fail (TEST 2)", []store.QualityEvent{ev(EventCIFail, sha+":1")}, 0.7},
		{
			"flaky re-run neutralises failure (TEST 3)",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventCIFailFlaky, sha+":2")},
			1.0,
		},
		{"quality revert (TEST 9)", []store.QualityEvent{ev(EventRevertQuality, "revsha")}, 0.1},
		{"strategic revert (TEST 10)", []store.QualityEvent{ev(EventRevertStrategic, "revsha")}, 0.8},
		{
			"worst-of floors: ci_fail + quality revert",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventRevertQuality, "revsha")},
			0.1,
		},
		{
			"worst-of floors: ci_fail + strategic revert",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventRevertStrategic, "revsha")},
			0.7,
		},
		{
			"flaky clears ci_fail but strategic revert still floors",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventCIFailFlaky, sha+":2"), ev(EventRevertStrategic, "r")},
			0.8,
		},
		{
			"flaky for a different SHA does NOT clear this failure",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventCIFailFlaky, "otherSHA:1")},
			0.7,
		},
		// --- #687: the workflow half of the match ---------------------------
		{
			"flaky for a DIFFERENT WORKFLOW does NOT clear this failure",
			[]store.QualityEvent{ev(EventCIFail, sha+":1:42"), ev(EventCIFailFlaky, sha+":1:99")},
			0.7,
		},
		{
			"flaky for the SAME workflow clears it",
			[]store.QualityEvent{ev(EventCIFail, sha+":1:42"), ev(EventCIFailFlaky, sha+":2:42")},
			1.0,
		},
		{
			"one workflow's flaky clears only its own failure",
			[]store.QualityEvent{
				ev(EventCIFail, sha+":1:42"),
				ev(EventCIFail, sha+":1:99"),
				ev(EventCIFailFlaky, sha+":2:42"),
			},
			0.7,
		},
		// --- #687 back-compat: rows written BEFORE the workflow id existed ---
		// Literal old-format strings, not values this package generates: these
		// are the bytes already on disk. Both unidentified => they still pair,
		// so an outcome resolved as 1.0 before this change resolves 1.0 after.
		{
			"pre-#687 pair still neutralises (legacy encoding on disk)",
			[]store.QualityEvent{ev(EventCIFail, "abc123:1"), ev(EventCIFailFlaky, "abc123:1")},
			1.0,
		},
		{
			"an identified flaky does NOT clear an unidentified legacy failure",
			[]store.QualityEvent{ev(EventCIFail, "abc123:1"), ev(EventCIFailFlaky, "abc123:2:42")},
			0.7,
		},
		{
			// The mirror of the row above. Both directions, because a future
			// edit could easily close one and leave the other.
			"a legacy flaky does NOT clear an identified failure",
			[]store.QualityEvent{ev(EventCIFail, "abc123:1:42"), ev(EventCIFailFlaky, "abc123:2")},
			0.7,
		},
		{
			// The shape a real deploy boundary produces: one outcome holding a
			// legacy pair AND an identified pair. Each clears its own.
			"a legacy pair and an identified pair coexist, each clearing its own",
			[]store.QualityEvent{
				ev(EventCIFail, "abc123:1"), ev(EventCIFailFlaky, "abc123:1"),
				ev(EventCIFail, "abc123:1:42"), ev(EventCIFailFlaky, "abc123:2:42"),
			},
			1.0,
		},
		{
			// 🔴 PINNING A DECISION, NOT A DISCOVERY. Resolve ignores
			// run_attempt, so a flaky success also clears a LATER failure of
			// the same workflow on the same commit. Two reviewers proposed
			// tightening this; see the long comment on flakyRuns in resolve.go
			// for why it was declined (one unchanged commit going fail -> pass
			// -> fail IS a flaky test). If you are here because you want to
			// change it, that argument is what you have to answer.
			"a flaky clears a LATER failure of the same workflow too (attempt deliberately ignored)",
			[]store.QualityEvent{
				ev(EventCIFail, sha+":1:42"),
				ev(EventCIFailFlaky, sha+":2:42"),
				ev(EventCIFail, sha+":3:42"),
			},
			1.0,
		},
		{
			"unknown/phase-2 event type has no effect",
			[]store.QualityEvent{ev("followup_fix", "x"), ev(EventCIPass, sha+":1")},
			1.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.events); got != tc.want {
				t.Errorf("Resolve = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolve_CleanShip pins spec TEST 1 as a named case (acceptance criterion).
func TestResolve_CleanShip(t *testing.T) {
	if got := Resolve(nil); got != 1.0 {
		t.Errorf("Resolve(no events) = %v, want 1.0", got)
	}
}

// TestResolve_CIFailure pins spec TEST 2.
func TestResolve_CIFailure(t *testing.T) {
	if got := Resolve([]store.QualityEvent{ev(EventCIFail, "sha:1")}); got != 0.7 {
		t.Errorf("Resolve(ci_fail) = %v, want 0.7", got)
	}
}

// TestResolve_FlakyRerunNeutralisesFailure pins spec TEST 3.
func TestResolve_FlakyRerunNeutralisesFailure(t *testing.T) {
	got := Resolve([]store.QualityEvent{ev(EventCIFail, "sha:1"), ev(EventCIFailFlaky, "sha:2")})
	if got != 1.0 {
		t.Errorf("Resolve(ci_fail + ci_fail_flaky) = %v, want 1.0", got)
	}
}

// TestResolve_QualityRevert pins spec TEST 9.
func TestResolve_QualityRevert(t *testing.T) {
	if got := Resolve([]store.QualityEvent{ev(EventRevertQuality, "r")}); got != 0.1 {
		t.Errorf("Resolve(revert_quality) = %v, want 0.1", got)
	}
}

// TestResolve_StrategicRevert pins spec TEST 10.
func TestResolve_StrategicRevert(t *testing.T) {
	if got := Resolve([]store.QualityEvent{ev(EventRevertStrategic, "r")}); got != 0.8 {
		t.Errorf("Resolve(revert_strategic) = %v, want 0.8", got)
	}
}

// TestResolve_WorstOfFloors: ci_fail + revert_quality resolves to the worst.
func TestResolve_WorstOfFloors(t *testing.T) {
	got := Resolve([]store.QualityEvent{ev(EventCIFail, "sha:1"), ev(EventRevertQuality, "r")})
	if got != 0.1 {
		t.Errorf("Resolve(ci_fail + revert_quality) = %v, want 0.1", got)
	}
}

// TestClassifyRevert exercises the spec §3 Event 4 keyword classification,
// including the ambiguity default (quality) and mixed-hit default (quality).
func TestClassifyRevert(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"quality keyword: caused OOM", `Revert "feat: cache"

This reverts commit deadbeef. caused OOM under load`, EventRevertQuality},
		{"quality keyword: broke prod", "Revert \"x\"\n\nbroke production", EventRevertQuality},
		// git revert's own subject shape. A `Revert: …` subject never reaches this
		// classifier: the webhook's revertLineRE gate requires whitespace after
		// "Revert" (TestHandlePush_RevertSubjectShape pins that).
		{"quality keyword: regression", "Revert \"feat: x\"\n\nintroduced a regression", EventRevertQuality},
		{"strategic: product decision", "Revert \"feat: widget\"\n\nproduct decision to remove feature", EventRevertStrategic},
		{"strategic: PM requested", "Revert \"feat: x\"\n\nPM requested we pull this", EventRevertStrategic},
		{"strategic: deprecated", "Revert \"feat: x\"\n\nfeature is deprecated now", EventRevertStrategic},
		{"bare revert defaults to quality", `Revert "feat: add foo"`, EventRevertQuality},
		{"mixed strategic + quality defaults to quality", "Revert \"x\"\n\nproduct decision, but it also broke prod", EventRevertQuality},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyRevert(tc.message); got != tc.want {
				t.Errorf("ClassifyRevert(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

func TestClassifyRevert_QualityWordBoundaries(t *testing.T) {
	for _, message := range []string{
		"deprecating the broker adapter",
		"removing the debug hooks / no longer needed",
		"no longer needed: breakfast scheduler",
		"no longer needed: coincident events",
		"no longer needed: metadata loss counter",
		"no longer needed: uncorrupted fixtures",
	} {
		t.Run(message, func(t *testing.T) {
			got := ClassifyRevert(message)
			if got != EventRevertStrategic {
				t.Fatalf("ClassifyRevert(%q) = %q, want strategic", message, got)
			}
			if floor := Resolve([]store.QualityEvent{ev(got, "revert")}); floor != 0.8 {
				t.Errorf("strategic revert floor = %v, want 0.8", floor)
			}
		})
	}
}

func TestClassifyRevert_QualityKeywordsAndInflections(t *testing.T) {
	for _, words := range [][]string{
		{"broke", "broken"},
		{"break", "breaks", "breaking", "breakage", "breakages"},
		{"crash", "crashes", "crashed", "crashing"},
		{"OOM", "OOMs", "OOMed", "OOMing"},
		{"out of memory"},
		{"regression", "regressions"},
		{"degradation", "degradations"},
		{"incident", "incidents"},
		{"outage", "outages"},
		{"bug", "bugs", "buggy", "bugfix", "bugfixes"},
		{"defect", "defects", "defective"},
		{"performance degraded", "performance degrades", "performance degrading", "performance degradation"},
		{"memory leak", "memory leaks"},
		{"data loss", "data losses"},
		{"corrupt", "corrupts", "corrupted", "corrupting", "corruption", "corruptions"},
		{"timeout", "timeouts"},
		{"deadlock", "deadlocks", "deadlocked", "deadlocking"},
		{"security vuln", "security vulns", "security vulnerable", "security vulnerability", "security vulnerabilities"},
	} {
		for _, word := range words {
			t.Run(word, func(t *testing.T) {
				// A strategic hit prevents the default-to-quality arm from hiding
				// a missed keyword. Punctuation exercises word boundaries too.
				message := "no longer needed; (" + word + ")"
				got := ClassifyRevert(message)
				if got != EventRevertQuality {
					t.Fatalf("ClassifyRevert(%q) = %q, want quality veto", message, got)
				}
				if floor := Resolve([]store.QualityEvent{ev(got, "revert")}); floor != 0.1 {
					t.Errorf("quality revert floor = %v, want 0.1", floor)
				}
			})
		}
	}
}

func TestClassifyRevert_WrappedPhrases(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"feature flag rollout\nwas disabled", EventRevertStrategic},
		{"business requirements\nhave changed", EventRevertStrategic},
		{"no longer needed; performance\nunder load degraded", EventRevertQuality},
		{"no longer needed; security\nreview found vulnerabilities", EventRevertQuality},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			if got := ClassifyRevert(tc.message); got != tc.want {
				t.Errorf("ClassifyRevert(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

// TestParseCIRef covers the source_ref parsing that flaky matching rests on.
//
// The first four rows are the ENTIRE pre-#687 TestHeadSHA table, kept verbatim
// as literal strings: whatever else changes, the head-SHA component of an
// already-stored source_ref must still read the same. The rest pin the new
// three-component form and the shapes that must NOT be mistaken for it.
func TestParseCIRef(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want CIRef
	}{
		// --- inherited from TestHeadSHA (pre-#687 rows, verbatim) ------------
		{"legacy two-component ref", "abc:1", CIRef{CIKey{"abc", ""}, 1}},
		{"three components", "abc:12:34", CIRef{CIKey{"abc", "34"}, 12}},
		{"no attempt at all (a revert commit sha)", "noattempt", CIRef{CIKey{"noattempt", ""}, 0}},
		{"empty", "", CIRef{CIKey{"", ""}, 0}},

		// --- the #687 form ---------------------------------------------------
		{"full identity", "deadbeef:2:9007199254740993", CIRef{CIKey{"deadbeef", "9007199254740993"}, 2}},
		{"attempt 0 is legal", "deadbeef:0:42", CIRef{CIKey{"deadbeef", "42"}, 0}},

		// --- shapes that must not be read as the #687 form -------------------
		// A workflow NAME is never encoded (BuildCISourceRef takes the numeric
		// id), but a name-shaped third component must not be honoured either:
		// non-decimal => fall back, head SHA still correct.
		{"colon-laden text is not an identity", "abc:1:build: test", CIRef{CIKey{"abc", ""}, 0}},
		{"non-decimal workflow id", "abc:1:ci", CIRef{CIKey{"abc", ""}, 0}},
		{"signed attempt is not decimal", "abc:+1", CIRef{CIKey{"abc", ""}, 0}},
		{"bare colon", "abc:", CIRef{CIKey{"abc", ""}, 0}},

		// --- right-anchored parsing: these are the FAITHFUL INVERSE of the
		// encoder for a head SHA that itself contains a colon, which the store
		// permits (outcomes.merge_commit_sha is unvalidated TEXT). Anchoring on
		// the left would read every one of them as "no identity" and silently
		// drop the workflow.
		{"colon-bearing head SHA keeps its identity", "abc:1:2:3", CIRef{CIKey{"abc:1", "3"}, 2}},
		{"colon-bearing head SHA, unidentified", "abc:1:0", CIRef{CIKey{"abc:1", ""}, 0}},
		{"colon-bearing head SHA, non-decimal middle", "abc:x:42", CIRef{CIKey{"abc:x", ""}, 42}},

		// --- overflow: parseDecimal must REJECT, not wrap or clamp ------------
		// If the overflow branch returned ok, the first row would read as an
		// identity at a nonsense attempt instead of falling back.
		{"overflowing attempt is not decimal", "abc:99999999999999999999:42",
			CIRef{CIKey{"abc:99999999999999999999", ""}, 42}},
		// The workflow id is compared as DIGITS, never converted, so an id wider
		// than int is still a perfectly good identity. This row is what fails if
		// someone "tidies" isPositiveDecimal into a strconv call.
		{"workflow id wider than int is still an identity", "abc:1:99999999999999999999",
			CIRef{CIKey{"abc", "99999999999999999999"}, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCIRef(tc.in); got != tc.want {
				t.Errorf("ParseCIRef(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildCISourceRef pins the encoding and its round trip. The encoder and
// the parser are the two halves of one contract; a test of either alone lets
// them drift into agreeing on nothing.
func TestBuildCISourceRef(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name       string
		attempt    int
		workflowID int64
		want       string
		wantParsed CIRef
	}{
		{"identified run", 2, 42, sha + ":2:42", CIRef{CIKey{sha, "42"}, 2}},
		{"first attempt", 1, 7, sha + ":1:7", CIRef{CIKey{sha, "7"}, 1}},
		{"absent workflow id falls back to the legacy form", 1, 0, sha + ":1", CIRef{CIKey{sha, ""}, 1}},
		{"negative workflow id falls back to the legacy form", 1, -5, sha + ":1", CIRef{CIKey{sha, ""}, 1}},
		{"negative attempt is clamped, not smuggled into the ref", -3, 42, sha + ":0:42", CIRef{CIKey{sha, "42"}, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildCISourceRef(sha, tc.attempt, tc.workflowID)
			if got != tc.want {
				t.Fatalf("BuildCISourceRef(%q, %d, %d) = %q, want %q", sha, tc.attempt, tc.workflowID, got, tc.want)
			}
			// Round trip: what was encoded is exactly what comes back out.
			if parsed := ParseCIRef(got); parsed != tc.wantParsed {
				t.Errorf("ParseCIRef(%q) = %+v, want %+v", got, parsed, tc.wantParsed)
			}
		})
	}
}

// TestParseCIRef_RoundTripsAnyHeadSHA is the guarantee that replaces the
// "head_sha is colon-free by construction" claim an earlier version of
// BuildCISourceRef's doc made. It is NOT colon-free: outcomes.merge_commit_sha
// is unvalidated TEXT (see internal/api/outcomes.go:136), so a colon can be
// stored and later named as a head_sha. Rather than assert an invariant nothing
// enforces, the parser is right-anchored and the round trip is total.
//
// ⚠️ An earlier draft of this test asserted `!strings.Contains(BuildCISourceRef(
// sha, 1, 42), "build")` — structurally vacuous, since the function has no name
// parameter, so it could not fail without a signature change that breaks the
// compile first. The payload-level guard that actually earns that claim is
// TestWorkflowNameNeverReachesTheSourceRef, which drives a real workflow_run
// body carrying a colon-laden `name`.
func TestParseCIRef_RoundTripsAnyHeadSHA(t *testing.T) {
	for _, sha := range []string{
		"0123456789abcdef0123456789abcdef01234567", // the only shape GitHub sends
		"abc:9",                        // one colon
		"CI: build: deploy",            // a name-shaped SHA, colons and spaces
		"0123456789abcdef:1:42",        // a SHA shaped like a source_ref
		"sha with spaces and : colons", // arbitrary text
	} {
		t.Run(sha, func(t *testing.T) {
			for _, attempt := range []int{1, 2, 17} {
				ref := BuildCISourceRef(sha, attempt, 42)
				got := ParseCIRef(ref)
				want := CIRef{CIKey{sha, "42"}, attempt}
				if got != want {
					t.Errorf("ParseCIRef(BuildCISourceRef(%q, %d, 42)) = %+v, want %+v (ref %q)",
						sha, attempt, got, want, ref)
				}
			}
		})
	}
}

// TestPoisonedHeadSHAIsFailClosed pins the residual-ambiguity claim in
// BuildCISourceRef's doc comment, which is the load-bearing half of the
// security argument once "the head SHA is 40 hex" is admitted to be false.
//
// A colon-bearing head SHA makes the UNIDENTIFIED encoding ambiguous: the ref
// no longer splits back into the same (sha, attempt) pair. The claim is that
// this can only ever DISABLE flaky neutralisation for that outcome, never cause
// one — because matching is scoped to a single outcome_id and
// outcomes.merge_commit_sha is UNIQUE, so every CI event on one outcome shares
// one head SHA. This walks the resulting event sets directly instead of
// asserting the reasoning.
func TestPoisonedHeadSHAIsFailClosed(t *testing.T) {
	// The control FIRST, so a bug that makes every arm unmatchable is caught
	// rather than read as a pass: with a clean head SHA, a flaky pair matches.
	clean := []store.QualityEvent{
		ev(EventCIFail, BuildCISourceRef("cleansha", 1, 42)),
		ev(EventCIFailFlaky, BuildCISourceRef("cleansha", 2, 42)),
	}
	if got := Resolve(clean); got != 1.0 {
		t.Fatalf("control: Resolve(clean flaky pair) = %v, want 1.0 — if this "+
			"fails, the arms below prove nothing", got)
	}

	for _, sha := range []string{"abc:9", "abc:1:42", "abc:"} {
		t.Run(sha, func(t *testing.T) {
			// Every attempt pairing, identified and unidentified. None may
			// produce a neutralised failure UNLESS the two refs genuinely
			// describe the same workflow — which for one outcome they do, so
			// the real assertion is the classifier-side one below: no pairing
			// may be BOTH matched and strictly-later, i.e. mintable.
			for _, wf := range []int64{0, 42} {
				for failAttempt := 0; failAttempt <= 4; failAttempt++ {
					for succAttempt := 0; succAttempt <= 4; succAttempt++ {
						failed := ParseCIRef(BuildCISourceRef(sha, failAttempt, wf))
						success := ParseCIRef(BuildCISourceRef(sha, succAttempt, wf))
						mintable := failed.CIKey == success.CIKey &&
							success.RunAttempt > failed.RunAttempt
						if mintable && failAttempt >= succAttempt {
							t.Errorf("head SHA %q wf %d: fail@%d + success@%d is "+
								"classifiable as flaky although the success did NOT "+
								"re-run the failure (refs %q / %q)",
								sha, wf, failAttempt, succAttempt,
								BuildCISourceRef(sha, failAttempt, wf),
								BuildCISourceRef(sha, succAttempt, wf))
						}
					}
				}
			}
		})
	}
}
