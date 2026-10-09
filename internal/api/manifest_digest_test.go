package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite" // the in-place-edit arm opens its own raw handle
)

// The #740 guards: the window digests (#716) reach the SERVED manifest.
//
// 🔴 WHY THESE LIVE HERE AND NOT IN internal/store. store's suite proved
// EventsDigest/OutcomesDigest correct in every particular — framing, injectivity,
// NULL coverage, a golden vector, an EQP plan pin — while NOTHING outside that
// package called either of them. `grep -rn '\.EventsDigest(' --include=*.go . |
// grep -v '^./internal/store/'` returned nothing, and the whole chain was green.
// A test of the digest FUNCTION cannot see an unwired function; only a test of
// the served bytes can. That is the arm whose absence let #740 exist, and it is
// the reason every assertion below reads the response body rather than calling
// the store.

// newDigestTestHandler is newTestHandlerWithTokenAndLimit with the database PATH
// returned, so an arm can reach the file over a second connection and construct
// a state no public writer can (an in-place edit that writes no ledger row).
func newDigestTestHandler(t *testing.T) (*Handler, *store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier-manifest-digest-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(path)
	})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(db, quiet, "", nil, "test", RateLimitConfig{}, WithUnsealedRecompute()), db, path
}

// digestBlock pulls one digest object off a decoded manifest, failing the test
// when the KEY is absent.
//
// A missing key and a zero-value struct are different facts, and decoding into
// reportManifestJSON would make them the same reading — the exact confusion #740
// exists to close one level up. It reports digests_omitted in the failure
// message, because "absent and declared" and "absent and silent" are the two
// states this whole issue is about telling apart.
func digestBlock(t *testing.T, m map[string]any, key string) (value string, rows float64) {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("%s is ABSENT from the served manifest (digests_omitted = %v)", key, m["digests_omitted"])
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", key, raw)
	}
	value, _ = obj["value"].(string)
	rows, _ = obj["rows"].(float64)
	return value, rows
}

// TestReportManifest_EmitsWindowDigests is THE WIRING PIN.
//
// NEGATIVE CONTROL, and it must stay runnable by hand: delete the
// `resp.EventsDigest = …` assignment in handleGetReportManifest and this test
// fails with "events_digest is ABSENT" — which is precisely the state the tree
// was in before #740, where the verifier printed NOT PINNED on an otherwise
// green rc-0 run and the absence of the strongest check was indistinguishable
// from that check passing.
func TestReportManifest_EmitsWindowDigests(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)
	seedOutcome(t, db, "alice", "issue-1", 3.0, 1.0)

	m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")

	for _, key := range []string{"events_digest", "outcomes_digest"} {
		value, rows := digestBlock(t, m, key)
		// The SCHEME TAG by literal, not merely "non-empty". A blanked assignment
		// yields "" and an untagged hex string yields something a verifier would
		// have to GUESS at; the tag is what lets it answer NOT CHECKED instead of
		// comparing two values it cannot know are commensurable. Same discipline
		// as the ManifestSchema pin.
		if !strings.HasPrefix(value, "tierdig1:") {
			t.Errorf("%s.value = %q, want a tierdig1-tagged digest", key, value)
		}
		if len(value) != len("tierdig1:")+64 {
			t.Errorf("%s.value = %q (len %d), want the scheme tag plus 64 hex characters",
				key, value, len(value))
		}
		// 🔴 rows IS THE DENOMINATOR THAT PROVES THE DIGEST WAS EARNED. An empty
		// window yields a perfectly well-formed value, so without this assertion
		// the whole arm passes over a window holding nothing.
		if rows != 1 {
			t.Errorf("%s.rows = %v, want 1 — the fixture seeded exactly one row per table, and a digest "+
				"published without a real denominator cannot be told apart from one that measured nothing",
				key, rows)
		}
	}
	// Present ⇒ no omission reason. The two states are mutually exclusive by
	// construction; a manifest carrying both would contradict itself.
	if v, ok := m["digests_omitted"]; ok {
		t.Errorf("digests_omitted = %v is published alongside the digests themselves", v)
	}
}

// TestReportManifest_DigestRowsMatchTheWatermarkCounts pins the ONE asymmetry a
// "just call WindowDigests(since, until)" implementation gets wrong.
//
// 🔴 THE TWO TABLES ARE NOT READ OVER THE SAME WINDOW. token_events is read over
// the ATTRIBUTION BAND [since - AttributableWindow, until) — OutcomeTokenTotals
// funds an outcome from up to 14 days before `since`, which is the whole reason
// the manifest publishes token_since — while outcomes are read over [since,
// until). The fixture puts a token event INSIDE the band and OUTSIDE the window,
// so a digest bounded at `since` covers 1 row while the watermark counts 2 and
// this test reddens. That is exactly the failure store.WindowWatermarks warns
// about ("a late-ingested row 20 days before `since` can … change /scores while a
// watermark bounded at `since` holds perfectly still"), one file over.
//
// It is also the manifest's INTERNAL CONSISTENCY guard: a consumer reading
// events_digest.rows beside watermarks.window.token_event_count is entitled to
// two counts of the SAME set on a fleet-wide manifest.
func TestReportManifest_DigestRowsMatchTheWatermarkCounts(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	ctx := context.Background()
	// A fixed calendar day, requested through the DATE grammar the endpoint
	// accepts, so `since` is exactly midnight and the band bound is exact.
	since := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// One row inside the report window, one inside the attribution band but
	// BEFORE `since`. The second is the row that discriminates.
	for _, at := range []time.Time{since.Add(6 * time.Hour), since.Add(-3 * 24 * time.Hour)} {
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
			InputTok: 2000, CostMicro: 1000, Source: "jsonl", Fidelity: "realtime",
			IdempotencyKey: at.Format(time.RFC3339Nano), Timestamp: at,
		}); err != nil {
			t.Fatalf("InsertTokenEvent(%s): %v", at, err)
		}
	}
	// TWO outcomes, mirroring the two events: one inside the report window and one
	// in the band-but-before-`since` slice.
	//
	// 🔴 THE SECOND ONE IS WHAT MAKES THE OUTCOMES ASSERTION MEAN ANYTHING. Without
	// it, `outcomes_digest.rows` and `outcome_count` are both 1 whether the outcomes
	// digest covers [since, until) or the widened band — so the arm would claim to
	// pin "the outcomes digest must NOT cover the band" while being blind to
	// exactly that mistake. Measured: widening outcomesDigestFrom to the band left
	// this whole package green before this row existed.
	for _, at := range []time.Time{since.Add(6 * time.Hour), since.Add(-3 * 24 * time.Hour)} {
		if _, err := db.InsertOutcome(ctx, store.Outcome{
			Developer: "alice", IssueID: "issue-1", Weight: 3, Quality: 1,
			MergeCommitSHA: "sha-" + at.Format(time.RFC3339), Timestamp: at,
		}); err != nil {
			t.Fatalf("InsertOutcome(%s): %v", at, err)
		}
	}

	m := decodeManifest(t, h, "/api/v1/report_manifest?since="+since.Format("2006-01-02"))

	wm, ok := m["watermarks"].(map[string]any)
	if !ok {
		t.Fatalf("watermarks missing: %v", m["watermarks"])
	}
	win, ok := wm["window"].(map[string]any)
	if !ok {
		t.Fatalf("watermarks.window missing: %v", wm["window"])
	}
	_, eventRows := digestBlock(t, m, "events_digest")
	_, outcomeRows := digestBlock(t, m, "outcomes_digest")

	// 🔴 THE VACUITY CONTROL COMES FIRST. If the band row had landed outside the
	// band, both sides would read 1 and the equality below would be satisfied by a
	// digest computed over the WRONG window.
	if eventRows != 2 {
		t.Fatalf("events_digest.rows = %v, want 2 (one in-window row plus one in the attribution band) — "+
			"the fixture is not exercising the asymmetry this test exists for", eventRows)
	}
	// The mirror-image control for the outcomes half: the band outcome must be
	// EXCLUDED, so 1 of the 2 seeded outcomes is covered. Reading 2 here means the
	// outcomes digest has been widened to the band; reading 1 with only one outcome
	// seeded would mean the fixture never posed the question.
	if outcomeRows != 1 {
		t.Fatalf("outcomes_digest.rows = %v, want 1 of the 2 seeded outcomes — the outcomes digest must cover "+
			"[since, until) only. An outcome before `since` is not in this report, so covering it would make an "+
			"edit that cannot touch this report a false alarm", outcomeRows)
	}
	if want := win["token_event_count"].(float64); eventRows != want {
		t.Errorf("events_digest.rows = %v but watermarks.window.token_event_count = %v — the digest and the "+
			"watermark are reading DIFFERENT row sets, so the manifest contradicts itself. The events digest "+
			"must cover [token_since, until), not [since, until)", eventRows, want)
	}
	if want := win["outcome_count"].(float64); outcomeRows != want {
		t.Errorf("outcomes_digest.rows = %v but watermarks.window.outcome_count = %v — the outcomes digest "+
			"must cover [since, until) and NOT the widened band; an outcome before `since` is not in this "+
			"report at all, so covering it would make an unrelated edit a false alarm", outcomeRows, want)
	}
}

// TestReportManifest_DigestSeesAnInPlaceEditNoWatermarkCan is the argument for
// #740 stated as a measurement, on the served bytes.
//
// 🔴 store.Watermarks says it in terms: MAX(id) catches an INSERT, COUNT(*)
// catches a DELETE, and "neither catches an in-place UPDATE". The edit below
// moves no id and no count, so the entire watermark block is byte-identical
// before and after while the row's content is different. Before #740 the
// manifest carried NOTHING that could see it — it pinned the input class its own
// code documents as insufficient, while the sufficient one sat unused three files
// away.
//
// billing_mode is the column chosen deliberately: it is inside the digest, it is
// rewritten in place by a real `tierd reprice`, and it is NOT published by
// /scores — so the edit is invisible to the output comparison too. A column that
// moved the served figures would make this arm pass for the wrong reason.
func TestReportManifest_DigestSeesAnInPlaceEditNoWatermarkCan(t *testing.T) {
	h, db, path := newDigestTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)
	seedOutcome(t, db, "alice", "issue-1", 3.0, 1.0)

	before := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	beforeDigest, _ := digestBlock(t, before, "events_digest")

	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() { _ = raw.Close() }()
	res, err := raw.ExecContext(context.Background(),
		"UPDATE token_events SET billing_mode = 'subscription' WHERE billing_mode <> 'subscription'")
	if err != nil {
		t.Fatalf("in-place edit: %v", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("the fixture edit touched 0 rows; every assertion below would be vacuous")
	}

	after := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	afterDigest, _ := digestBlock(t, after, "events_digest")

	// The half that makes the other half mean anything: the watermarks did NOT
	// move. Compared as re-encoded JSON so a field added to either sub-struct is
	// included automatically, with no second list to keep in step.
	wmBefore, err := json.Marshal(before["watermarks"])
	if err != nil {
		t.Fatalf("re-encode watermarks: %v", err)
	}
	wmAfter, err := json.Marshal(after["watermarks"])
	if err != nil {
		t.Fatalf("re-encode watermarks: %v", err)
	}
	if !bytes.Equal(wmBefore, wmAfter) {
		t.Fatalf("the watermarks moved (%s -> %s); this arm is meant to exercise an edit NO watermark can see, "+
			"so it is no longer testing what it claims", wmBefore, wmAfter)
	}
	if beforeDigest == afterDigest {
		t.Errorf("an in-place content edit left events_digest at %s while every watermark held still — the "+
			"manifest now attests an identity for rows that changed underneath it", beforeDigest)
	}
}

// TestReportManifest_WithholdsDigestsInAnAnonymizedMode extends the #593 window
// withhold to the digests, for the same reason and with the same bluntness:
// `rows` is an unfloored count of work over a caller-chosen window, which is
// precisely the quantity watermarks.window is withheld for.
func TestReportManifest_WithholdsDigestsInAnAnonymizedMode(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)
	h.SetAggregation(scoring.AggregationTeam, 5)

	m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	for _, key := range []string{"events_digest", "outcomes_digest"} {
		if v, ok := m[key]; ok {
			t.Errorf("%s = %v is published in team mode; its `rows` is an unfloored count of work over a "+
				"caller-chosen window, the same quantity watermarks.window is withheld for (#593)", key, v)
		}
	}
	// 🔴 AND THE ABSENCE IS DECLARED. A manifest that goes silently quiet is the
	// failure this whole issue is about.
	if got, _ := m["digests_omitted"].(string); got != digestOmittedAnonymized {
		t.Errorf("digests_omitted = %q, want the anonymized-mode reason — an undeclared absence reads as "+
			"'this server publishes no digests', which is a different and false statement", got)
	}
	ks, ok := m["kanon_suppressed"].(map[string]any)
	if !ok {
		t.Fatalf("kanon_suppressed missing: %v", m["kanon_suppressed"])
	}
	if ks["withheld_digests"] != true {
		t.Errorf("kanon_suppressed.withheld_digests = %v, want true — this record has to be a COMPLETE "+
			"statement of what k-anonymity withheld from this manifest", ks["withheld_digests"])
	}
}

// The arms below seed through seedRepoCost (scores_repo_filter_test.go), NOT the
// package's shared seedCosts: the latter leaves `repo` unset, so every row lands
// on the reserved 'unqualified' sentinel and cannot pose any question this file
// asks about scoping.

// TestReportManifest_ScopedManifestCarriesAScopedDigest IS #747 ON THE SERVED
// BYTES, and it replaces TestReportManifest_OmitsDigestsForARepoScopedManifest.
//
// 🔴 THE ARM THAT IS THE WHOLE POINT: write to repo A, then fetch a manifest
// scoped to repo B. B's digest must be UNCHANGED. It asserts the ABSENCE of a
// false alarm, which is a strictly different claim from "the request succeeded"
// — before #747 the served manifest carried no digest at all for a scoped
// request, precisely because the unscoped one would have moved here.
//
// 🔴 AND THE ARM THAT STOPS THAT BEING VACUOUS: an in-place content edit to a row
// INSIDE scope B must still move B's digest. A predicate that filtered
// everything out would satisfy the first arm perfectly, and would have bought
// silence rather than precision.
func TestReportManifest_ScopedManifestCarriesAScopedDigest(t *testing.T) {
	h, db, path := newDigestTestHandler(t)
	seedRepoCost(t, db, "acme/beta", "bob", "issue-b", 2.0)
	seedRepoCost(t, db, "acme/alpha", "alice", "issue-a", 1.0)

	const scopedURL = "/api/v1/report_manifest?since=2020-01-01&repo=acme/beta"
	m := decodeManifest(t, h, scopedURL)

	// A scoped manifest now PUBLISHES the digests, and therefore declares no
	// omission. Both halves matter: a build that omitted them would satisfy the
	// "did not move" arm below trivially.
	beta, betaRows := digestBlock(t, m, "events_digest")
	if _, ok := m["digests_omitted"]; ok {
		t.Errorf("digests_omitted = %v is published on a scoped manifest that also carries its digests", m["digests_omitted"])
	}
	// 🔴 THE DENOMINATOR. A zero-row digest is a well-formed value over nothing —
	// the #718 failure — and would make every comparison below meaningless.
	if betaRows != 1 {
		t.Fatalf("events_digest.rows = %v on ?repo=acme/beta, want 1: exactly one row in the fixture belongs to "+
			"that repository, and a digest published over zero rows would satisfy every arm below", betaRows)
	}

	// The scoped value must not be the FLEET-WIDE value, so a fleet-wide
	// recomputation cannot silently satisfy a scoped pin.
	//
	// ⚠️ THIS ARM DOES NOT PROVE THE PREDICATE IS IN THE HASHED FRAME, and an
	// earlier comment here said it did. Beta covers 1 row and fleet-wide covers 2,
	// so the two differ by CONTENT and would differ even with the scope absent
	// from store.digestDomainFor. The frame property is owned by
	// store.TestScopedDigest_DiffersFromTheFleetWideDigestOverTheSameRows, which
	// asserts it on the frame directly and on a single-repository fixture where
	// content cannot account for the difference.
	fleet, fleetRows := digestBlock(t, decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01"), "events_digest")
	if fleetRows != 2 {
		t.Fatalf("control: the fleet-wide manifest covers %v rows, want 2 — the fixture did not land", fleetRows)
	}
	if beta == fleet {
		t.Errorf("the scoped and fleet-wide events_digest are the same value (%s); a scoped manifest is publishing "+
			"a fleet-wide identity", beta)
	}

	// 🔴 THE DECISIVE ARM. Another repository ingests. Nothing about acme/beta's
	// report has moved, so its digest may not move either.
	seedRepoCost(t, db, "acme/alpha", "alice", "issue-a2", 5.0)
	afterForeign, afterForeignRows := digestBlock(t, decodeManifest(t, h, scopedURL), "events_digest")
	if afterForeign != beta || afterForeignRows != betaRows {
		t.Errorf("FALSE ALARM: an insert into acme/alpha moved the acme/beta manifest's events_digest\n"+
			"  before %s (rows=%v)\n  after  %s (rows=%v)\n"+
			"verify-report would tell acme/beta's operator their untouched report is unreproducible because "+
			"another team wrote to their own repository", beta, betaRows, afterForeign, afterForeignRows)
	}
	// CONTROL: that write must be visible SOMEWHERE, or the arm above passed
	// because nothing happened.
	if got, _ := digestBlock(t, decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01"), "events_digest"); got == fleet {
		t.Fatalf("control: the acme/alpha insert left the FLEET-WIDE digest at %s too, so the write never landed "+
			"and the scoped assertion above proves nothing", fleet)
	}

	// 🔴 THE ANTI-VACUITY ARM. An in-place edit INSIDE acme/beta — the class no
	// watermark can see — must still move the scoped digest.
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() { _ = raw.Close() }()
	res, err := raw.ExecContext(context.Background(),
		"UPDATE token_events SET billing_mode = 'subscription' WHERE repo = 'acme/beta'")
	if err != nil {
		t.Fatalf("in-place edit: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("control: the in-scope edit touched %d rows, want 1 — nothing was mutated, so a CHANGED below "+
			"would be attributable to something else", n)
	}
	afterEdit, afterEditRows := digestBlock(t, decodeManifest(t, h, scopedURL), "events_digest")
	if afterEdit == beta {
		t.Errorf("an in-place edit to an acme/beta row left the acme/beta digest at %s — the scope filtered out the "+
			"very rows it is supposed to attest, so 'no false alarm' above was bought with silence", beta)
	}
	if afterEditRows != betaRows {
		t.Errorf("rows moved from %v to %v on an in-place edit; the fixture inserted or deleted instead of editing", betaRows, afterEditRows)
	}
}

// TestReportManifest_ScopedDigestIsCanonicalized is the #718 trap, one program
// over.
//
// 🔴 /api/v1/scores PUTS ?repo= THROUGH repoid.Canonical AND THE DIGEST SQL BINDS
// `repo = ?`. A handler that passed the RAW query value would bind "Acme/Tier",
// match ZERO rows, and publish a perfectly well-formed digest over nothing —
// which a verifier would then reproduce exactly, printing a confident UNCHANGED
// over a window nothing looked at. #718's first draft shipped precisely that bug
// because every fixture in that round was fleet-wide.
//
// ⭐ THE MIXED-CASE SPELLING IS THE TEST. Feeding "acme/tier" and asserting the
// digests match would hold on a handler that does not canonicalize at all; the
// assertion only means something because the input NEEDED normalizing. And both
// digests must be NON-EMPTY — two equal zero-row digests would satisfy an
// equality check while proving the opposite of what it claims.
func TestReportManifest_ScopedDigestIsCanonicalized(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	seedRepoCost(t, db, "acme/tier", "alice", "issue-1", 1.0)
	seedRepoCost(t, db, "acme/other", "bob", "issue-2", 2.0)
	// An outcome too, or the outcomes half of the loop below compares two ZERO-row
	// digests and passes on exactly the bug it exists to catch.
	seedRepoOutcome(t, db, "acme/tier", "alice", "issue-1", 3.0)
	seedRepoOutcome(t, db, "acme/other", "bob", "issue-2", 5.0)

	canonical := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01&repo=acme/tier")
	mixed := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01&repo=Acme/Tier.git")

	if got := mixed["repo"]; got != "acme/tier" {
		t.Fatalf("manifest repo = %v, want acme/tier — the emitter echoed the caller's spelling instead of the "+
			"canonical slug, so a verifier binding it would match zero rows", got)
	}
	for _, key := range []string{"events_digest", "outcomes_digest"} {
		wantValue, wantRows := digestBlock(t, canonical, key)
		gotValue, gotRows := digestBlock(t, mixed, key)
		if gotValue != wantValue || gotRows != wantRows {
			t.Errorf("?repo=Acme/Tier.git produced %s = %s (rows=%v) but ?repo=acme/tier produced %s (rows=%v) — "+
				"the scope reached the digest un-canonicalized", key, gotValue, gotRows, wantValue, wantRows)
		}
	}
	// 🔴 NON-EMPTY, ASSERTED SEPARATELY, ON BOTH. Two zero-row digests are equal to
	// each other, so the loop above passes on exactly the bug it is meant to catch.
	for _, key := range []string{"events_digest", "outcomes_digest"} {
		if _, rows := digestBlock(t, canonical, key); rows != 1 {
			t.Errorf("%s.rows = %v on ?repo=acme/tier, want 1 — the two spellings agree because BOTH covered "+
				"nothing, which is the #718 failure rather than evidence against it", key, rows)
		}
	}
}
