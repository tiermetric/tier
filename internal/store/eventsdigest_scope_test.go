package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

// The #747 guards: a digest may be narrowed to ONE repository.
//
// 🔴 THE DEFECT THESE CLOSE, STATED ONCE. Until #747 every digest in this file
// covered the whole fleet. A manifest served for `?repo=beta/repo` would
// therefore have attested an identity over a SUPERSET of the rows that report
// read — so team A's ingestion into alpha/repo moves the digest, `verify-report`
// on team B's untouched scoped report prints `events_digest CHANGED`, and the
// operator is told their report is unreproducible when nothing about their data
// moved. That is a FALSE ALARM, the one failure mode a tamper-evidence surface
// may not have, and #740 chose to publish NO digest at all rather than a wrong
// one. Measured on the tree before this change, over the fixture below:
//
//	digest a beta/repo-scoped manifest would have pinned
//	    tierdig1:b8acbc6ab5ce257d6f724dd6eae9d3429fd211597f1e5d8b29beb2de9f0b6757 rows=1
//	after one alpha/repo insert
//	    tierdig1:8c5317d694b045cb6e456e353b75424a93a82433c97353ed240adb0d5f239327 rows=2
//
// ⚠️ EVERY ARM BELOW NEEDS ITS OPPOSITE. "The digest did not move" is satisfied
// perfectly by a predicate that filters EVERYTHING out, and "the digest moved"
// is satisfied by a predicate that filters nothing. Neither reading means
// anything alone, so each test here pairs the two.

const (
	scopeAlpha RepoScope = "alpha/repo"
	scopeBeta  RepoScope = "beta/repo"
)

// seedDigestScopedEvent inserts one in-window token_events row under an explicit repo.
// Unlike seedDigestEvent it does not hard-code a slug, because the whole point
// of this file is which repository a row belongs to. An empty repo lands as the
// reserved 'unqualified' sentinel (see normalizeRepo).
func seedDigestScopedEvent(t *testing.T, db *DB, repo, dev string, costMicro int64, at time.Time, key string) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer: dev, IssueID: "issue-" + dev, Model: "claude-sonnet-4",
		InputTok: 1000, OutputTok: 500, CostMicro: costMicro,
		Source: "jsonl", Fidelity: "realtime",
		Repo: repo, IdempotencyKey: key, Timestamp: at,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(repo=%q, dev=%q): %v", repo, dev, err)
	}
}

func seedDigestScopedOutcome(t *testing.T, db *DB, repo, dev string, weight float64, at time.Time, sha string) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), Outcome{
		Developer: dev, IssueID: "issue-" + dev, PRNumber: 1,
		Weight: weight, Quality: 1, MergeCommitSHA: sha,
		Source: "api", WorkType: "feature", Repo: repo, Timestamp: at,
	}); err != nil {
		t.Fatalf("InsertOutcome(repo=%q, dev=%q): %v", repo, dev, err)
	}
}

// seedTwoRepoWindow fills the standard window with one event and one outcome in
// EACH of two repositories, plus one repo-blind row of each carrying the
// 'unqualified' sentinel.
func seedTwoRepoWindow(t *testing.T, db *DB) {
	t.Helper()
	at := digestSince.AddDate(0, 0, 3)
	seedDigestScopedEvent(t, db, string(scopeAlpha), "alice", 1_000, at, "sc-a1")
	seedDigestScopedEvent(t, db, string(scopeBeta), "bob", 2_000, at, "sc-b1")
	seedDigestScopedEvent(t, db, "", "carol", 3_000, at, "sc-u1") // -> 'unqualified'
	seedDigestScopedOutcome(t, db, string(scopeAlpha), "alice", 3, at, "sha-sc-a1")
	seedDigestScopedOutcome(t, db, string(scopeBeta), "bob", 5, at, "sha-sc-b1")
	seedDigestScopedOutcome(t, db, "", "carol", 8, at, "sha-sc-u1")
}

// TestScopedDigest_IsBlindToAnotherRepositorysWrites IS THE POINT OF #747.
//
// 🔴 IT ASSERTS THE ABSENCE OF A FALSE ALARM, WHICH IS A DIFFERENT ASSERTION
// FROM "SOMETHING PASSED". The scoped digest must be byte-identical across an
// insert into another repository, an in-place EDIT of another repository's row,
// and a DELETE of one — all three, because a scope that only ignores inserts
// still cries wolf on the other two.
//
// 🔴 CONTROL, AND IT IS NOT OPTIONAL: the FLEET-WIDE digest over the same store
// must MOVE on each of those writes. Without it every assertion here is
// satisfied by a predicate that matches nothing, by a digest that ignores its
// input, or by a fixture whose writes silently failed.
func TestScopedDigest_IsBlindToAnotherRepositorysWrites(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedTwoRepoWindow(t, db)

	beta := eventsDigest(t, db, scopeBeta)
	betaOut := outcomesDigest(t, db, scopeBeta)
	fleet := eventsDigest(t, db, FleetWide)
	if betaOut.Rows != 1 {
		t.Fatalf("control: the beta/repo outcomes digest covers %d rows, want 1", betaOut.Rows)
	}
	if beta.Rows != 1 {
		t.Fatalf("control: the beta/repo digest covers %d rows, want 1 — a zero-row digest is a well-formed value over nothing and would satisfy every arm below", beta.Rows)
	}
	if fleet.Rows != 3 {
		t.Fatalf("control: the fleet-wide digest covers %d rows, want 3 — the fixture did not land", fleet.Rows)
	}

	// 🔴 `do` TAKES THE SUBTEST'S OWN *testing.T. Closing over the OUTER t and
	// calling Fatalf on it from inside t.Run aborts the parent goroutine: Go
	// reports "subtest may have called FailNow on a parent test" — pointing at the
	// harness rather than at the seed — and arms 2 and 3 never run at all. The
	// failure is loud either way, but it names the wrong thing and silently halves
	// the coverage, which is worse than a plain error.
	for _, w := range []struct {
		name string
		do   func(t *testing.T)
	}{
		{"an INSERT into alpha/repo", func(t *testing.T) {
			seedDigestScopedEvent(t, db, string(scopeAlpha), "dave", 4_000, digestSince.AddDate(0, 0, 5), "sc-a2")
		}},
		{"an in-place EDIT of an alpha/repo row", func(t *testing.T) {
			res, err := db.db.ExecContext(ctx,
				`UPDATE token_events SET cost_micro = cost_micro + 1 WHERE repo = ?`, string(scopeAlpha))
			if err != nil {
				t.Fatalf("edit alpha row: %v", err)
			}
			// The INSERT arm ran first, so alpha/repo holds two rows by now. Naming
			// the number pins the arm ORDER this fixture depends on, so a reordering
			// fails here rather than quietly weakening the arm below.
			if n, _ := res.RowsAffected(); n != 2 {
				t.Fatalf("the edit touched %d alpha/repo rows, want 2 — nothing was mutated, so the control below would report the wrong cause", n)
			}
		}},
		{"a DELETE of an alpha/repo row", func(t *testing.T) {
			res, err := db.db.ExecContext(ctx,
				`DELETE FROM token_events WHERE repo = ? AND developer = 'dave'`, string(scopeAlpha))
			if err != nil {
				t.Fatalf("delete alpha row: %v", err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("the delete removed %d alpha/repo rows, want 1 — the row the INSERT arm added is not there", n)
			}
		}},
	} {
		t.Run(w.name, func(t *testing.T) {
			w.do(t)

			gotBeta := eventsDigest(t, db, scopeBeta)
			if gotBeta.Value != beta.Value || gotBeta.Rows != beta.Rows {
				t.Errorf("FALSE ALARM: %s moved the beta/repo digest\n  before %s (rows=%d)\n  after  %s (rows=%d)\n"+
					"A verifier would report beta/repo's untouched report as CHANGED because another team wrote to their own repository",
					w.name, beta.Value, beta.Rows, gotBeta.Value, gotBeta.Rows)
			}
			// The OUTCOMES half, which is not merely symmetry: measured, making only
			// outcomesDigestFrom's predicate tolerant is caught by exactly one test
			// in the tree, and only through a row COUNT — so a wrong-population
			// outcomes predicate that preserved the count would pass everything.
			// Both tables publish into the same manifest and both need this arm.
			if gotBetaOut := outcomesDigest(t, db, scopeBeta); gotBetaOut.Value != betaOut.Value {
				t.Errorf("FALSE ALARM: %s moved the beta/repo OUTCOMES digest (%s -> %s)",
					w.name, betaOut.Value, gotBetaOut.Value)
			}

			// 🔴 THE CONTROL. The write must be visible SOMEWHERE, or the arm above
			// passes because nothing happened rather than because the scope worked.
			gotFleet := eventsDigest(t, db, FleetWide)
			if gotFleet.Value == fleet.Value {
				t.Errorf("control: %s left the FLEET-WIDE digest unchanged too, so the write never landed and the "+
					"scoped assertion above proves nothing", w.name)
			}
			fleet = gotFleet
		})
	}
}

// TestScopedDigest_SeesAnEditInsideItsOwnScope is the ANTI-VACUITY arm, and it
// is the one that stops the test above from being satisfied by a predicate that
// filters everything out.
//
// 🔑 IT USES AN IN-PLACE UPDATE DELIBERATELY. An insert or a delete moves
// COUNT(*), so a watermark would catch it; an in-place edit is exactly the class
// store.Watermarks documents itself as structurally blind to, and therefore the
// only thing a digest adds. If a scoped digest could not see it, scoping would
// have bought silence rather than precision.
func TestScopedDigest_SeesAnEditInsideItsOwnScope(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedTwoRepoWindow(t, db)

	before := eventsDigest(t, db, scopeBeta)
	beforeOut := outcomesDigest(t, db, scopeBeta)

	res, err := db.db.ExecContext(ctx,
		`UPDATE token_events SET cost_micro = cost_micro + 1 WHERE repo = ?`, string(scopeBeta))
	if err != nil {
		t.Fatalf("edit beta row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("control: the edit touched %d rows, want 1 — nothing was mutated, so an UNCHANGED below would be meaningless", n)
	}
	if _, err := db.db.ExecContext(ctx,
		`UPDATE outcomes SET merge_commit_sha = 'sha-repointed' WHERE repo = ?`, string(scopeBeta)); err != nil {
		t.Fatalf("edit beta outcome: %v", err)
	}

	after := eventsDigest(t, db, scopeBeta)
	if after.Value == before.Value {
		t.Errorf("an in-place edit to a beta/repo token_events row left the beta/repo digest unchanged (%s) — "+
			"the scope filtered out the very rows it is supposed to attest, which is what makes a 'no false alarm' "+
			"result meaningless", after.Value)
	}
	if after.Rows != before.Rows {
		t.Errorf("row count moved from %d to %d on an in-place edit; the fixture inserted or deleted instead of editing", before.Rows, after.Rows)
	}
	if got := outcomesDigest(t, db, scopeBeta); got.Value == beforeOut.Value {
		t.Errorf("re-pointing merge_commit_sha on a beta/repo outcome left the scoped outcomes digest unchanged (%s) — "+
			"the provenance half of the identity is not covered under a scope", got.Value)
	}
}

// TestScopedDigest_ExcludesTheUnqualifiedSentinel records the #747 RULING on
// repo-blind rows, and it is a deliberate choice rather than a fallout.
//
// 🔴 THE ANSWER IS: A SCOPED DIGEST EXCLUDES THEM, BECAUSE THE SCOPED REPORT
// DOES. RepoScope.clause() is strict `repo = ?` — never the tolerant
// `OR repo = 'unqualified'` RepoMatch uses for JOINING — and store.RepoScope's
// doc block gives the reason: a tolerant FILTER would attribute every repo-blind
// row in the fleet to whichever repository the caller happened to name. The
// digest inherits that predicate unchanged, exactly as it inherits tsWindow's,
// so it attests precisely the rows the report was computed over. The alternative
// — a digest that covered rows the report did not read — would make an edit to
// another producer's repo-blind row read as a divergence of a scoped report that
// never saw it: the same false alarm, arriving by a different door.
//
// ⚠️ NOTHING FALLS OUT OF EVERY IDENTITY. The sentinel rows remain covered by
// the FLEET-WIDE digest, whose projection still renders them through
// COALESCE(repo, 'unqualified'), and the under-count a strict scope causes is
// separately disclosed by UnqualifiedExclusionWindow on /scores.
func TestScopedDigest_ExcludesTheUnqualifiedSentinel(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedTwoRepoWindow(t, db)

	before := eventsDigest(t, db, scopeBeta)
	beforeOut := outcomesDigest(t, db, scopeBeta)
	fleetBefore := eventsDigest(t, db, FleetWide)
	fleetBeforeOut := outcomesDigest(t, db, FleetWide)

	res, err := db.db.ExecContext(ctx,
		`UPDATE token_events SET cost_micro = cost_micro + 1 WHERE repo = 'unqualified'`)
	if err != nil {
		t.Fatalf("edit unqualified row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("control: the edit touched %d sentinel rows, want 1 — the fixture has no repo-blind row, so neither assertion below means anything", n)
	}
	// The outcomes counterpart, so the ruling this test states for the SCHEME is
	// checked on both tables rather than on token_events alone.
	resOut, err := db.db.ExecContext(ctx,
		`UPDATE outcomes SET merge_commit_sha = 'sha-repointed' WHERE repo = 'unqualified'`)
	if err != nil {
		t.Fatalf("edit unqualified outcome: %v", err)
	}
	if n, _ := resOut.RowsAffected(); n != 1 {
		t.Fatalf("control: the edit touched %d sentinel outcomes, want 1", n)
	}

	if got := eventsDigest(t, db, scopeBeta); got.Value != before.Value {
		t.Errorf("editing an 'unqualified' row moved the beta/repo digest; a strict scope must not attest rows the "+
			"scoped report never read (%s -> %s)", before.Value, got.Value)
	}
	if got := outcomesDigest(t, db, scopeBeta); got.Value != beforeOut.Value {
		t.Errorf("editing an 'unqualified' outcome moved the beta/repo OUTCOMES digest (%s -> %s)", beforeOut.Value, got.Value)
	}
	if got := outcomesDigest(t, db, FleetWide); got.Value == fleetBeforeOut.Value {
		t.Errorf("editing an 'unqualified' outcome left the FLEET-WIDE outcomes digest unchanged too (%s) — the "+
			"sentinel outcomes have fallen out of EVERY identity", got.Value)
	}
	// ⭐ THE HALF THAT MAKES THE RULING HONEST: the row is not un-covered, only
	// covered elsewhere. Without this, "excluded from the scoped digest" would be
	// indistinguishable from "covered by no digest at all".
	if got := eventsDigest(t, db, FleetWide); got.Value == fleetBefore.Value {
		t.Errorf("editing an 'unqualified' row left the FLEET-WIDE digest unchanged too (%s) — the sentinel rows "+
			"have fallen out of EVERY identity, which is a silent hole rather than a scoping decision", got.Value)
	}

	// And the scope cannot be pointed AT the sentinel: repoid.Canonical refuses
	// it at the trust boundary, so a caller cannot ask to be scoped to
	// repo-blindness. Asserted here as the value this store would produce if one
	// somehow arrived — it selects the sentinel rows and nothing else, which is
	// why the refusal upstream is the thing that matters.
	if got := eventsDigest(t, db, RepoScope("unqualified")); got.Rows != 1 {
		t.Errorf("a scope of \"unqualified\" covered %d rows, want the 1 sentinel row — the strictness claim above "+
			"rests on `repo = ?` matching the stored value literally", got.Rows)
	}
}

// TestScopedDigest_DiffersFromTheFleetWideDigestOverTheSameRows pins the #747
// ruling that the PREDICATE is part of the digested frame.
//
// 🔴 THE SINGLE-REPOSITORY INSTALL IS THE CASE THAT MATTERS, and it is why this
// is not a philosophical point. On an install where every row belongs to
// acme/tier, a scoped read and a fleet-wide read return the SAME ROWS. Without
// the predicate in the frame the two digests are byte-identical, so:
//
//   - re-requesting a manifest WITHOUT ?repo= and comparing it against a scoped
//     pin reports UNCHANGED — agreement earned by a comparison nobody made; and
//   - the day a second repository is onboarded the two silently stop meaning the
//     same thing, with no reading anywhere marking the moment.
//
// Folding the scope into the domain frame makes a scope change a CHANGED digest.
// A reading is always better than the absence of one.
func TestScopedDigest_DiffersFromTheFleetWideDigestOverTheSameRows(t *testing.T) {
	db, _ := newDigestDB(t)
	at := digestSince.AddDate(0, 0, 3)
	// A SINGLE-REPOSITORY install: the two predicates select identical rows.
	seedDigestScopedEvent(t, db, string(scopeAlpha), "alice", 1_000, at, "one-a1")
	seedDigestScopedOutcome(t, db, string(scopeAlpha), "alice", 3, at, "sha-one-a1")

	scoped := eventsDigest(t, db, scopeAlpha)
	fleet := eventsDigest(t, db, FleetWide)
	if scoped.Rows != fleet.Rows || scoped.Rows != 1 {
		t.Fatalf("control: scoped covers %d rows and fleet-wide %d, want 1 and 1 — this arm only means something when the two predicates select the SAME rows", scoped.Rows, fleet.Rows)
	}
	if scoped.Value == fleet.Value {
		t.Errorf("the scoped and fleet-wide digests over an IDENTICAL row set are the same value (%s). A digest attests "+
			"'these rows, under this predicate'; identical bytes make a scope change invisible, so a fleet-wide "+
			"recomputation would silently satisfy a scoped pin", scoped.Value)
	}
	// 🔴 TWO DIFFERENT SCOPES MUST DIFFER, AND THIS IS ASSERTED ON THE FRAME, NOT
	// ON TWO DIGESTS. A digest comparison cannot make this claim: `repo = ?` means
	// two distinct scopes can never select the same rows, so any two scoped digests
	// differ by CONTENT whether or not the slug is in the frame. Measured: with the
	// slug dropped from digestDomainFor, a digest-vs-digest version of this arm
	// stays green. Only the frame itself can be asked.
	if digestDomainFor(digestDomainTokenEvents, scopeAlpha) == digestDomainFor(digestDomainTokenEvents, scopeBeta) {
		t.Error("two different scopes produce the same domain frame — it carries the FACT of being scoped but not " +
			"WHICH scope, so a fleet-wide-vs-scoped difference is the only thing the digest could ever show")
	}
	// And the frame must stay unambiguous when a slug contains the same separator
	// the infix uses — the nested "group/sub/proj" shape a GitLab install emits.
	// These are the concrete pairs a naive concatenation could collide.
	for _, c := range []struct{ a, b RepoScope }{
		{"a/b", "a"},
		{"group/sub/proj", "group"},
		{"x/repo", "x/repo/repo"},
	} {
		if digestDomainFor(digestDomainTokenEvents, c.a) == digestDomainFor(digestDomainTokenEvents, c.b) {
			t.Errorf("scopes %q and %q share a domain frame", c.a, c.b)
		}
	}
	// The digest-level version of the same claim is kept as the weaker,
	// behavioural companion: two scopes over different populations differ.
	other := eventsDigest(t, db, scopeBeta)
	if other.Value == scoped.Value {
		t.Errorf("beta/repo and alpha/repo digest identically (%s) over different populations", other.Value)
	}
	// The outcomes side takes the same rule, and its own domain: a scoped
	// token_events digest must never collide with a scoped outcomes digest.
	if outcomesDigest(t, db, scopeAlpha).Value == scoped.Value {
		t.Error("the scoped token_events and outcomes digests collide — domain separation was lost when the slug was added")
	}
}

// TestScopedDigest_RequiresACanonicalSlug is the #718 shape, one layer down.
//
// 🔴 A NON-CANONICAL SCOPE BINDS TO ZERO ROWS AND STILL RETURNS A WELL-FORMED
// DIGEST. `repo = 'Acme/Tier'` matches nothing on a store that holds
// 'acme/tier', and the value that comes back is a perfectly ordinary
// tierdig1: string — indistinguishable in SHAPE from one earned over the whole
// window. This store deliberately does NOT canonicalize (RepoScope's contract is
// that callers validate at the trust boundary); what it does is publish `rows`,
// which is the only thing that tells the two apart. This test pins both halves.
func TestScopedDigest_RequiresACanonicalSlug(t *testing.T) {
	db, _ := newDigestDB(t)
	at := digestSince.AddDate(0, 0, 3)
	seedDigestScopedEvent(t, db, "acme/tier", "alice", 1_000, at, "canon-1")

	canonical := eventsDigest(t, db, RepoScope("acme/tier"))
	if canonical.Rows != 1 {
		t.Fatalf("control: the canonical scope covered %d rows, want 1", canonical.Rows)
	}
	raw := eventsDigest(t, db, RepoScope("Acme/Tier"))
	if raw.Rows != 0 {
		t.Fatalf("a raw mixed-case scope covered %d rows; this test's premise (SQLite `repo = ?` is case-SENSITIVE for these values) is wrong and the canonicalization argument needs re-deriving", raw.Rows)
	}
	if !strings.HasPrefix(raw.Value, digestScheme+":") || len(raw.Value) != len(digestScheme)+1+64 {
		t.Errorf("the zero-row digest is %q, which is not a well-formed value — the hazard this test describes is that it IS well-formed", raw.Value)
	}
	if raw.Value == canonical.Value {
		t.Error("the zero-row digest equals the one-row digest, so `rows` is the ONLY discriminator and this test's premise is wrong")
	}
}

// goldenScopeSlug is the frozen scope for the golden vectors below. It is
// deliberately NESTED (three segments, a real GitLab shape), so the frozen bytes
// cover a slug containing the same "/" the infix uses and a future change to the
// infix cannot quietly re-identify GitLab-shaped scopes.
//
// ⚠️ IT IS A BYTE FREEZE, NOT AN INJECTIVITY PROOF, and an earlier comment here
// claimed the latter. With a fixed-length base and a fixed-position infix inside
// a length-prefixed frame, a "/" in the slug cannot produce a collision — there
// is no colliding pair to construct. The injectivity claim is made where it can
// actually be checked: the explicit pair table in
// TestScopedDigest_DiffersFromTheFleetWideDigestOverTheSameRows.
const goldenScopeSlug RepoScope = "group/sub/proj"

// goldenScopedTokenEventRows / goldenScopedOutcomeRows are the fleet-wide golden
// rows with `repo` set to goldenScopeSlug.
//
// 🔑 THE REPO OVERRIDE IS WHAT LETS ONE CONSTANT SERVE BOTH ARMS. `repo` is
// itself a digested frame, so a scoped vector whose rows carried a DIFFERENT
// repository could never be reproduced by a live read under that scope — the
// predicate would select none of them, and the constant would be forever
// unreachable from the database. Overriding it is what makes the frozen value
// checkable against the production read, which is the entire point of the
// live-read arm below.
var (
	goldenScopedTokenEventRows = scopedGoldenTokenEventRows()
	goldenScopedOutcomeRows    = scopedGoldenOutcomeRows()
)

func scopedGoldenTokenEventRows() []tokenEventDigestRow {
	out := append([]tokenEventDigestRow(nil), goldenTokenEventRows...)
	for i := range out {
		out[i].Repo = string(goldenScopeSlug)
	}
	return out
}

func scopedGoldenOutcomeRows() []outcomeDigestRow {
	out := append([]outcomeDigestRow(nil), goldenOutcomeRows...)
	for i := range out {
		out[i].Repo = string(goldenScopeSlug)
	}
	return out
}

// The frozen SCOPED digests. Same contract as TestDigestGoldenVector — read that
// test's doc block before touching a constant here.
//
// 📌 MINTED, NOT REGENERATED (#747). These two constants describe a value that
// had never been computed before this change, so writing down what the code
// produces is the act of freezing a NEW identity rather than accepting a moved
// one. That is the only circumstance in which pasting a computed digest into a
// golden vector is correct, and it is why the four fleet-wide constants in
// TestDigestGoldenVector had to stay untouched in the same commit: those DO
// describe already-published values.
const (
	goldenScopedEventDigest   = "tierdig1:cb3d45544cb782bd6ef08ebecc81ade224618613dfcff4f704ffa70a1f6c4b69"
	goldenScopedOutcomeDigest = "tierdig1:161fba6e1a5bbc22d05bb7e2777ba81dbb4335007602f6a1e6093fcbe93150aa"
)

// TestScopedDigestGoldenVector freezes the SCOPED wire format, and it exists for
// the same reason TestDigestGoldenVector does: every other test in this file
// compares one run of the hasher against another run of the same hasher, so any
// change to digestDomainFor moves both sides together and they stay green.
//
// ⭐ THE FLEET-WIDE CONSTANTS ARE THE OTHER HALF OF THIS TEST, AND THEY LIVE IN
// TestDigestGoldenVector UNTOUCHED. That is the #747 regression pin: every digest
// published before this change was fleet-wide, and a scheme that re-identified
// them would invalidate every manifest already in an operator's hands. The four
// constants there did not move, which is what says the scope was added ALONGSIDE
// the old identity rather than on top of it. If they ever move together with
// these, something wider changed than a scope — re-diagnose, do not re-paste.
//
// ⚠️ THIS ARM ALONE PINS ONLY THE SERIALIZER, and that is not enough. It runs the
// TEST's hasher, so it constrains digestDomainFor and the frame writers and
// NOTHING about the read that actually publishes a scoped digest.
// TestScopedDigestGoldenVectorMatchesTheLiveRead is the other half and is not
// optional — the measurement in its doc block is why.
func TestScopedDigestGoldenVector(t *testing.T) {
	if got := scopedEventsDigestOf(goldenScopedTokenEventRows, goldenScopeSlug); got != goldenScopedEventDigest {
		t.Errorf("scoped token_events golden vector changed:\n  got  %s\n  want %s\nThe SCOPED serialization of a FIXED row set moved. Read this test's doc block before touching the constant", got, goldenScopedEventDigest)
	}
	if got := scopedOutcomesDigestOf(goldenScopedOutcomeRows, goldenScopeSlug); got != goldenScopedOutcomeDigest {
		t.Errorf("scoped outcomes golden vector changed:\n  got  %s\n  want %s", got, goldenScopedOutcomeDigest)
	}
	// 🔴 CONTROL: the vector must be a function of the SCOPE, not only of the
	// rows, or a build that dropped digestDomainFor entirely would satisfy it.
	if scopedEventsDigestOf(goldenScopedTokenEventRows, "other/scope") == goldenScopedEventDigest {
		t.Fatal("control: changing the scope did not change the golden digest — the predicate is not in the frame and the vector pins nothing about scoping")
	}
	// 🔴 CONTROL: and it must not equal the FLEET-WIDE vector over the same rows.
	if goldenScopedEventDigest == goldenTokenEventDigest {
		t.Fatal("control: the scoped and fleet-wide golden vectors are the same constant")
	}
}

// TestScopedDigestGoldenVectorMatchesTheLiveRead ties the scoped constants to the
// actual database read, and it is the arm whose ABSENCE was measured to hide a
// real defect.
//
// 🔴 THE MEASUREMENT, BECAUSE IT IS THE WHOLE JUSTIFICATION. Make
// eventsDigestFrom / outcomesDigestFrom build their domain frame from a DIFFERENT
// slug than scope.clause() binds — so every published scoped digest is computed
// under a domain that does not correspond to its own predicate — and the entire
// suite stays GREEN without this test. Every behavioural arm survives, because a
// uniformly-wrong domain still differs from fleet-wide, still differs between two
// scopes, and still moves on an in-scope edit. Only a FROZEN constant compared
// against the LIVE read can see it.
//
// This is the scoped twin of TestDigestGoldenVectorMatchesTheLiveRead, whose doc
// block names the same defect class on the fleet-wide path ("the serializer and
// the SELECT disagree — which no other test in this file can see"). #747's first
// revision shipped the scoped path without it; a review found the gap by mutation.
func TestScopedDigestGoldenVectorMatchesTheLiveRead(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()

	for _, r := range goldenScopedTokenEventRows {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO token_events (id, developer, issue_id, model, input_tok, output_tok,
			    cache_read, cache_write_5m, cache_write_1h, cost_micro, source, fidelity,
			    repo, price_version, host, billing_mode, ts)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Developer, r.IssueID, r.Model, r.InputTok, r.OutputTok,
			r.CacheRead, r.CacheWrite5m, r.CacheWrite1h, r.CostMicro, r.Source, r.Fidelity,
			r.Repo, r.PriceVersion, r.Host, r.BillingMode, r.TS); err != nil {
			t.Fatalf("seed token_event %d: %v", r.ID, err)
		}
	}
	for _, r := range goldenScopedOutcomeRows {
		// NULLIF reproduces InsertOutcome's real behaviour, so the second row
		// exercises the projection's COALESCE under a scope as well.
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO outcomes (id, developer, issue_id, weight, quality, work_type,
			    source, repo, pr_number, merge_commit_sha, ts)
			VALUES (?,?,?,?,?,?,?,?,NULLIF(?,0),NULLIF(?,''),?)`,
			r.ID, r.Developer, r.IssueID, r.Weight, r.Quality, r.WorkType,
			r.Source, r.Repo, r.PRNumber, r.MergeCommitSHA, r.TS); err != nil {
			t.Fatalf("seed outcome %d: %v", r.ID, err)
		}
	}
	// 🔴 DECOY ROWS IN ANOTHER REPOSITORY, in the same window. Without them the
	// scope selects every row in the table and this arm would pass on a build with
	// no repo predicate at all: the live read has to be shown CHOOSING, not merely
	// reading.
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO token_events (developer, issue_id, model, input_tok, output_tok,
		    cache_read, cache_write_5m, cache_write_1h, cost_micro, source, fidelity,
		    repo, price_version, host, billing_mode, ts)
		VALUES ('mallory','issue-9','claude-sonnet-4',1,1,0,0,0,99999,'jsonl','realtime',
		        'decoy/repo',7,'unknown','per_token','2026-08-05 12:00:00 +0000 UTC')`); err != nil {
		t.Fatalf("seed decoy token_event: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO outcomes (developer, issue_id, weight, quality, work_type,
		    source, repo, pr_number, merge_commit_sha, ts)
		VALUES ('mallory','issue-9',9,1,'feature','api','decoy/repo',99,'deadbeef',
		        '2026-08-05 12:00:00 +0000 UTC')`); err != nil {
		t.Fatalf("seed decoy outcome: %v", err)
	}
	// 🔴 CONTROL FIRST: the decoys must really be there, or "the scope excluded
	// them" is a claim about an empty table.
	var decoys int
	if err := db.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM token_events WHERE repo='decoy/repo')
		      + (SELECT COUNT(*) FROM outcomes     WHERE repo='decoy/repo')`).Scan(&decoys); err != nil {
		t.Fatalf("count decoys: %v", err)
	}
	if decoys != 2 {
		t.Fatalf("control: %d decoy rows are present, want 2 — the scope was never asked to exclude anything", decoys)
	}

	got := eventsDigest(t, db, goldenScopeSlug)
	if got.Value != goldenScopedEventDigest {
		t.Errorf("the LIVE SCOPED token_events read does not reproduce the scoped golden vector:\n  live   %s\n  golden %s\n"+
			"The domain frame and the bound predicate disagree, or a column is transposed in tokenEventDigestSelect — "+
			"neither of which any behavioural arm in this file can see", got.Value, goldenScopedEventDigest)
	}
	if got.Rows != int64(len(goldenScopedTokenEventRows)) {
		t.Errorf("the live scoped read covered %d rows, want %d — a decoy leaked in, or a golden row was filtered out",
			got.Rows, len(goldenScopedTokenEventRows))
	}
	gotOut := outcomesDigest(t, db, goldenScopeSlug)
	if gotOut.Value != goldenScopedOutcomeDigest {
		t.Errorf("the LIVE SCOPED outcomes read does not reproduce the scoped golden vector:\n  live   %s\n  golden %s",
			gotOut.Value, goldenScopedOutcomeDigest)
	}
	if gotOut.Rows != int64(len(goldenScopedOutcomeRows)) {
		t.Errorf("the live scoped outcomes read covered %d rows, want %d", gotOut.Rows, len(goldenScopedOutcomeRows))
	}
}

func scopedEventsDigestOf(rows []tokenEventDigestRow, scope RepoScope) string {
	h := sha256.New()
	var buf bytes.Buffer
	writeLengthPrefixed(&buf, digestDomainFor(digestDomainTokenEvents, scope))
	foldFrames(h, &buf)
	for _, r := range rows {
		appendTokenEventFrames(&buf, r)
		foldFrames(h, &buf)
	}
	return finishDigest(h)
}

func scopedOutcomesDigestOf(rows []outcomeDigestRow, scope RepoScope) string {
	h := sha256.New()
	var buf bytes.Buffer
	writeLengthPrefixed(&buf, digestDomainFor(digestDomainOutcomes, scope))
	foldFrames(h, &buf)
	for _, r := range rows {
		appendOutcomeFrames(&buf, r)
		foldFrames(h, &buf)
	}
	return finishDigest(h)
}

// TestScopedDigestReadIsStillAWindowSeek extends the BUDGET PIN to the scoped
// read, and it asserts something WEAKER than its fleet-wide sibling on purpose.
//
// `repo` is in neither idx_token_events_ts_id nor idx_outcomes_ts_id, so a
// scoped read cannot be index-only: it seeks the window on the index and touches
// the table to evaluate the conjunct — the same asymmetry ReportWatermarks
// documents. What must NOT happen is a full table SCAN or a materialized sort,
// because either makes the cost proportional to the TABLE rather than the
// window, and the whole placement argument for putting digests on the manifest
// surface rests on the window bound.
func TestScopedDigestReadIsStillAWindowSeek(t *testing.T) {
	db, _ := newDigestDB(t)
	seedTwoRepoWindow(t, db)

	clause, args := tsWindow(digestSince, digestUntil)
	scopeSQL, scopeArgs := scopeBeta.clause()
	args = append(args, scopeArgs...)

	checked := 0
	for _, c := range digestPlanCases {
		plan := queryPlan(t, db, c.sel+clause+scopeSQL+digestOrderSQL, args...)
		checked++
		if strings.Contains(plan, "SCAN "+c.table) {
			t.Errorf("%s: the SCOPED digest read plans as a FULL SCAN (%q) — adding the repo conjunct cost the window seek, so this read is now proportional to the table", c.name, plan)
		}
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s: the SCOPED digest read does not use %s (%q)", c.name, c.index, plan)
		}
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s: the SCOPED digest read materializes a sort (%q), defeating the streaming hash's O(1) memory", c.name, plan)
		}
	}
	if checked != 2 {
		t.Fatalf("control: planned %d reads, expected 2", checked)
	}

	// 🔴 CONTROL: EXPLAIN QUERY PLAN must be CAPABLE of saying SCAN here. Two of
	// the three assertions above are NEGATIVE Contains checks, which are vacuously
	// true against an instrument that has gone quiet or whose plan text changed
	// shape. The fleet-wide sibling carries this same control for the same reason.
	control := queryPlan(t, db, `SELECT COUNT(*) FROM token_events WHERE model = ?`, "claude-sonnet-4")
	if !strings.Contains(control, "SCAN") {
		t.Fatalf("control: a query with no usable index planned as %q, which contains no \"SCAN\" — the planner is "+
			"not reporting scans in this environment, so the assertions above cannot fail", control)
	}
}

// TestScopedReportDigests_CarryTheScopeThroughBothTables pins that the SCOPE
// reaches both halves of the pair a manifest publishes, and that the token side
// keeps its AttributableWindow widening while scoped.
//
// 🔑 THE TWO ASYMMETRIES ARE INDEPENDENT AND BOTH HAVE TO SURVIVE. A fix that
// threaded the scope through only the outcomes read, or that lost the widened
// band when a scope was present, would leave one of the pair attesting a
// population the report never read — and `events_digest.rows` would stop being
// comparable with `watermarks.window.token_event_count`, which is the manifest's
// own internal-consistency claim.
func TestScopedReportDigests_CarryTheScopeThroughBothTables(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedTwoRepoWindow(t, db)
	// One beta/repo token event inside the ATTRIBUTION BAND but before `since`,
	// and one alpha/repo event in the same place. Only the first may be covered.
	inBand := digestSince.Add(-AttributableWindow).Add(24 * time.Hour)
	seedDigestScopedEvent(t, db, string(scopeBeta), "bob", 500, inBand, "band-b")
	seedDigestScopedEvent(t, db, string(scopeAlpha), "alice", 500, inBand, "band-a")

	events, outcomes, err := db.ReportDigests(ctx, digestSince, digestUntil, scopeBeta)
	if err != nil {
		t.Fatalf("ReportDigests(beta): %v", err)
	}
	// 2 = the in-window beta event + the in-band beta event. Not 1 (the band was
	// lost) and not 4 (the scope was lost on the token side).
	if events.Rows != 2 {
		t.Errorf("scoped events digest covers %d rows, want 2 — the in-window beta row plus the one inside the "+
			"attribution band, and NOTHING from alpha/repo. 1 means the AttributableWindow widening was dropped "+
			"when a scope was present; 4 means the scope was dropped on the token side", events.Rows)
	}
	if outcomes.Rows != 1 {
		t.Errorf("scoped outcomes digest covers %d rows, want 1 — 3 means the scope never reached the outcomes read, "+
			"and 2 would mean the outcomes side wrongly inherited the widened band", outcomes.Rows)
	}

	// And it must agree with the single-table calls under the same scope and
	// windows — the pair is the same computation taken under one snapshot.
	wantEvents, err := db.EventsDigest(ctx, digestSince.Add(-AttributableWindow), digestUntil, scopeBeta)
	if err != nil {
		t.Fatalf("EventsDigest: %v", err)
	}
	if events.Value != wantEvents.Value {
		t.Errorf("ReportDigests' scoped events half = %s, want %s", events.Value, wantEvents.Value)
	}
	wantOutcomes, err := db.OutcomesDigest(ctx, digestSince, digestUntil, scopeBeta)
	if err != nil {
		t.Fatalf("OutcomesDigest: %v", err)
	}
	if outcomes.Value != wantOutcomes.Value {
		t.Errorf("ReportDigests' scoped outcomes half = %s, want %s", outcomes.Value, wantOutcomes.Value)
	}
}
