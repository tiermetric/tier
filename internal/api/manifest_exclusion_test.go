package api

// The #751 emitter guards: a SCOPED manifest pins the repo_scope_excluded
// disclosure figure its own report published.
//
// 🔴 WHAT #747 LEFT OPEN. #747 scoped the #716 digests so a `?repo=` manifest
// attests exactly the rows its report read. Scoping is STRICT (store.RepoScope.
// clause is `repo = ?`), so the reserved `unqualified` sentinel rows are excluded
// — correctly, because a tolerant predicate would over-attribute every repo-blind
// row in the fleet to whichever repository the caller named (#590). But a scoped
// /scores STILL READS those rows, to publish
// data_quality.repo_scope_excluded.{token_events, cost_usd, outcomes}. That
// figure therefore sat outside every identity a scoped manifest ships, and a
// scoped manifest carries no fleet-wide digest either.
//
// ⭐ THE ARM THAT MATTERS MOST HERE IS THE CROSS-SURFACE ONE. A pin that agrees
// with itself proves nothing; what makes this field a reproducibility pin rather
// than a decoration is that it equals the number /scores served for the SAME
// request. TestReportManifest_ExclusionPinEqualsTheServedDisclosure is that arm.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// exclusionSince is the fixed lower bound every arm below uses. Fixed, not
// time.Now()-relative: an attribution-band arm measured against a drifting window
// is unreproducible the next day.
const exclusionSince = "2026-08-01"

var exclusionSinceTime = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// exclusionBlock pulls repo_scope_excluded off a decoded manifest, failing when
// the KEY is absent.
//
// 🔑 A MISSING KEY AND AN ALL-ZERO OBJECT ARE DIFFERENT FACTS, and decoding into
// reportManifestJSON would make them one reading. On this field they are the two
// states the whole design turns on: absent means "fleet-wide, or a pre-#751
// emitter"; all-zero is a POSITIVE claim that the window excluded nothing, which
// a later sentinel insert must be able to falsify.
func exclusionBlock(t *testing.T, m map[string]any) (tokenEvents, costMicro, outcomes float64) {
	t.Helper()
	raw, ok := m["repo_scope_excluded"]
	if !ok {
		t.Fatalf("repo_scope_excluded is ABSENT from the served manifest (repo = %v)", m["repo"])
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("repo_scope_excluded is not an object: %v", raw)
	}
	tokenEvents, _ = obj["token_events"].(float64)
	costMicro, _ = obj["cost_micro"].(float64)
	outcomes, _ = obj["outcomes"].(float64)
	return tokenEvents, costMicro, outcomes
}

// servedExclusion reads data_quality.repo_scope_excluded from a /scores body,
// reporting whether the key was present at all.
func servedExclusion(t *testing.T, h *Handler, target string) (tokenEvents int64, costUSD float64, outcomes int64, present bool) {
	t.Helper()
	code, body := doRequest(t, h, "GET", target, nil)
	if code != 200 {
		t.Fatalf("GET %s = %d, body %s", target, code, body)
	}
	var env struct {
		DataQuality struct {
			RepoScopeExcluded *struct {
				TokenEvents int64   `json:"token_events"`
				CostUSD     float64 `json:"cost_usd"`
				Outcomes    int64   `json:"outcomes"`
			} `json:"repo_scope_excluded"`
		} `json:"data_quality"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode /scores: %v (body %s)", err, body)
	}
	if env.DataQuality.RepoScopeExcluded == nil {
		return 0, 0, 0, false
	}
	e := env.DataQuality.RepoScopeExcluded
	return e.TokenEvents, e.CostUSD, e.Outcomes, true
}

// TestReportManifest_ExclusionPinEqualsTheServedDisclosure is the arm that makes
// this field a PIN rather than a decoration.
//
// 🔴 THE MANIFEST MUST ATTEST THE NUMBER /scores SERVED, NOT A NEIGHBOUR OF IT.
// The two reads share store.UnqualifiedExclusionWindow, which owns a deliberate
// asymmetry — the token side reaches back `since - AttributableWindow` while the
// outcome side does not — so a manifest that re-derived its own band would pin a
// DIFFERENT population and a verifier would compare two things that were never
// the same. This arm seeds a repo-blind row INSIDE the look-back precisely so
// that a narrower band fails here.
func TestReportManifest_ExclusionPinEqualsTheServedDisclosure(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	seedRepoCostAt(t, db, "acme/beta", "bob", "issue-b", 2.0, exclusionSinceTime.Add(48*time.Hour))
	// Two repo-blind token events: one INSIDE the window, one in the ATTRIBUTION
	// LOOK-BACK before `since`. store.UnqualifiedExclusionWindow counts both.
	seedRepoCostAt(t, db, "", "carol", "issue-c", 9.0, exclusionSinceTime.Add(72*time.Hour))
	seedRepoCostAt(t, db, "", "carol", "issue-d", 4.0, exclusionSinceTime.Add(-7*24*time.Hour))
	seedRepoOutcomeAt(t, db, "", "carol", "issue-c", 3.0, exclusionSinceTime.Add(72*time.Hour))

	const q = "?since=" + exclusionSince + "&repo=acme/beta"
	wantTokens, wantUSD, wantOutcomes, present := servedExclusion(t, h, "/api/v1/scores"+q)
	if !present {
		t.Fatal("scoped /scores published NO repo_scope_excluded; the fixture's repo-blind rows never landed, " +
			"so every comparison below would be between two zeroes")
	}
	// CONTROL on the look-back: $9 in-window + $4 in the 14-day look-back. A
	// disclosure that measured [since, until) alone would read 9.00 and 1 event.
	if wantTokens != 2 || wantUSD != 13.0 {
		t.Fatalf("served disclosure = %d token_events / $%.2f, want 2 / $13.00 — the fixture is not exercising "+
			"the attribution look-back that UnqualifiedExclusionWindow deliberately widens", wantTokens, wantUSD)
	}
	// 🔴 THE OUTCOMES LEG NEEDS ITS OWN LITERAL, AND LEAVING IT OUT WAS A REAL HOLE.
	// The manifest/`/scores` comparison below reads the SAME store method on both
	// sides, so it agrees with itself: measured, appending `AND 1=0` to
	// UnqualifiedExclusionWindow's outcomes statement left every arm in this file
	// green, because 0 == 0. Only a literal can see that.
	if wantOutcomes != 1 {
		t.Fatalf("served disclosure = %d repo-blind outcome(s), want 1 — a comparison between two zeroes is "+
			"agreement bought with silence", wantOutcomes)
	}

	gotTokens, gotMicro, gotOutcomes := exclusionBlock(t, decodeManifest(t, h, "/api/v1/report_manifest"+q))
	if int64(gotTokens) != wantTokens || int64(gotOutcomes) != wantOutcomes {
		t.Errorf("manifest pins %v token_events / %v outcomes where /scores served %d / %d — the manifest is "+
			"attesting a DIFFERENT population than the report published",
			gotTokens, gotOutcomes, wantTokens, wantOutcomes)
	}
	// 🔑 THE MANIFEST PINS MICRO-DOLLARS AND /scores PUBLISHES DOLLARS, ON PURPOSE:
	// the pin is compared for EQUALITY, and float equality over a
	// JSON-round-tripped dollar figure can manufacture a divergence. The two must
	// still name the same money, by the same pure function.
	if got := store.MicroToDollars(int64(gotMicro)); got != wantUSD {
		t.Errorf("manifest pins cost_micro %v (= $%.2f) where /scores served cost_usd $%.2f", gotMicro, got, wantUSD)
	}
}

// TestReportManifest_ExclusionPinIsScopedOnlyAndUnconditional covers the two
// presence rules, which are deliberately NOT the same as /scores'.
func TestReportManifest_ExclusionPinIsScopedOnlyAndUnconditional(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	seedRepoCostAt(t, db, "acme/beta", "bob", "issue-b", 2.0, exclusionSinceTime.Add(48*time.Hour))
	seedRepoCostAt(t, db, "", "carol", "issue-c", 9.0, exclusionSinceTime.Add(72*time.Hour))

	// ⛔ FLEET-WIDE: ABSENT. A fleet-wide report excluded nothing — the sentinel
	// rows are inside its own digests — so there is no published figure to pin, and
	// emitting an install-wide repo-blind cost on a request that named no scope
	// would be adding disclosure to close a reproducibility gap.
	fleet := decodeManifest(t, h, "/api/v1/report_manifest?since="+exclusionSince)
	if _, ok := fleet["repo_scope_excluded"]; ok {
		t.Errorf("the fleet-wide manifest carries repo_scope_excluded = %v", fleet["repo_scope_excluded"])
	}
	// CONTROL: the fixture DOES hold repo-blind spend, so the absence above is a
	// decision rather than an empty database.
	if _, _, _, present := servedExclusion(t, h, "/api/v1/scores?since="+exclusionSince+"&repo=acme/beta"); !present {
		t.Fatal("control: the fixture has no repo-blind rows, so the fleet-wide absence proves nothing")
	}

	// 🔴 SCOPED AND CLEAN: PRESENT, ALL ZERO. This is the deliberate divergence
	// from /scores, which OMITS its block when the window is clean because there
	// the absence is the signal to a human reader. Here the block is a PIN, and an
	// omit-when-clean pin is vacuous exactly when it matters most: a window clean
	// at publish time into which a sentinel row is later inserted would verify as
	// NOT PINNED, and an absent check reading as a passing one is #740's defect.
	h2, db2, _ := newDigestTestHandler(t)
	seedRepoCostAt(t, db2, "acme/beta", "bob", "issue-b", 2.0, exclusionSinceTime.Add(48*time.Hour))
	const cleanQ = "?since=" + exclusionSince + "&repo=acme/beta"
	if _, _, _, present := servedExclusion(t, h2, "/api/v1/scores"+cleanQ); present {
		t.Fatal("control: /scores emitted repo_scope_excluded on a CLEAN window, so the divergence this arm " +
			"asserts does not exist")
	}
	tokens, micro, outcomes := exclusionBlock(t, decodeManifest(t, h2, "/api/v1/report_manifest"+cleanQ))
	if tokens != 0 || micro != 0 || outcomes != 0 {
		t.Errorf("clean scoped manifest pins %v/%v/%v, want all zero", tokens, micro, outcomes)
	}
}

// TestReportManifest_ExclusionPinIsUnreachableInAnAnonymizedMode pins the claim
// the emitter comment makes and nothing tested.
//
// 🔴 THE EMIT BLOCK SITS ABOVE THE k-ANON BRANCH AND IS GUARDED ONLY BY
// `!scope.IsFleetWide()`. Its comment says the pin "cannot appear in an anonymized
// mode: ?repo= is refused there" — true, but true because of allowRepoScope in a
// DIFFERENT function. If that guard were ever relaxed, install-wide repo-blind
// counts would ship inside a k-anonymized document: the #593 shape the window
// block and both digests are withheld for. A claim resting entirely on a distant
// `return` needs a test at the seam, not a comment.
//
// Two arms, because "the request is refused" and "the anonymized document carries
// no such key" are different facts and only the pair excludes the failure.
func TestReportManifest_ExclusionPinIsUnreachableInAnAnonymizedMode(t *testing.T) {
	h, db, _ := newDigestTestHandler(t)
	seedRepoCostAt(t, db, "acme/beta", "bob", "issue-b", 2.0, exclusionSinceTime.Add(48*time.Hour))
	seedRepoCostAt(t, db, "", "carol", "issue-c", 9.0, exclusionSinceTime.Add(72*time.Hour))
	h.SetAggregation(scoring.AggregationTeam, scoring.DefaultKAnonymity)

	// Arm 1: a SCOPED manifest is refused outright, so the emit block is never
	// reached. 400, not a 200 carrying a withhold declaration.
	code, body := doRequest(t, h, "GET", "/api/v1/report_manifest?since="+exclusionSince+"&repo=acme/beta", nil)
	if code != 400 {
		t.Fatalf("scoped manifest in team mode = %d, want 400 — ?repo= must be refused in an anonymized mode "+
			"(#185, #270), and the #751 pin's whole safety argument rests on it. Body: %s", code, body)
	}

	// Arm 2: the FLEET-WIDE anonymized manifest carries no exclusion key either.
	// Arm 1 alone would still hold on an emitter that leaked the counts here.
	m := decodeManifest(t, h, "/api/v1/report_manifest?since="+exclusionSince)
	if _, ok := m["repo_scope_excluded"]; ok {
		t.Errorf("the anonymized manifest carries repo_scope_excluded = %v — an install-wide count of repo-blind "+
			"work inside a k-anonymized document is exactly what watermarks.window is withheld for (#593)",
			m["repo_scope_excluded"])
	}
	// CONTROL: this document really is the anonymized one, so the absence above is
	// a decision rather than a request that never reached the handler.
	if _, ok := m["kanon_suppressed"]; !ok {
		t.Fatalf("the manifest carries no kanon_suppressed block, so it is not the anonymized document this "+
			"arm claims to inspect: %v", m)
	}
}

// TestReportManifest_ExclusionPinMovesWhereTheScopedDigestCannot is #751's probe
// at the emitter, and it is the whole reason the field exists.
func TestReportManifest_ExclusionPinMovesWhereTheScopedDigestCannot(t *testing.T) {
	h, db, path := newDigestTestHandler(t)
	seedRepoCostAt(t, db, "acme/beta", "bob", "issue-b", 2.0, exclusionSinceTime.Add(48*time.Hour))
	seedRepoCostAt(t, db, "", "carol", "issue-c", 9.0, exclusionSinceTime.Add(72*time.Hour))

	const q = "?since=" + exclusionSince + "&repo=acme/beta"
	before := decodeManifest(t, h, "/api/v1/report_manifest"+q)
	beforeDigest, beforeRows := digestBlock(t, before, "events_digest")
	_, beforeMicro, _ := exclusionBlock(t, before)
	if beforeRows != 1 {
		t.Fatalf("control: events_digest covers %v rows, want 1 — the scoped fixture did not land", beforeRows)
	}

	// The issue's probe verbatim: an in-place reprice of the sentinel rows.
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() { _ = raw.Close() }()
	res, err := raw.ExecContext(context.Background(),
		"UPDATE token_events SET cost_micro = cost_micro + 5000000 WHERE repo = 'unqualified'")
	if err != nil {
		t.Fatalf("reprice: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("control: the reprice touched %d row(s), want 1 — the probe never landed", n)
	}

	after := decodeManifest(t, h, "/api/v1/report_manifest"+q)
	_, afterMicro, _ := exclusionBlock(t, after)
	if afterMicro == beforeMicro {
		t.Errorf("#751: an in-place reprice of the `unqualified` sentinel rows left the manifest's pinned "+
			"cost_micro at %v — the disclosure figure this scoped report published moved and the manifest "+
			"attests no identity for it", beforeMicro)
	}
	// 🔴 AND THE HALF THAT MAKES IT THE ISSUE RATHER THAN A DUPLICATE: the scoped
	// digest is BLIND to this write, correctly, because the sentinel rows sit
	// outside the strict `repo = ?` predicate. ⛔ A "fix" that widened the digest
	// predicate to cover them would satisfy the arm above and reinstate exactly the
	// over-attribution #590 exists to close.
	afterDigest, afterRows := digestBlock(t, after, "events_digest")
	if afterDigest != beforeDigest || afterRows != beforeRows {
		t.Errorf("the SCOPED events_digest moved on a repo-blind reprice (%s/%v -> %s/%v); the scope predicate "+
			"has been widened to cover the sentinel, which over-attributes every repo-blind row in the fleet to "+
			"acme/beta (#590)", beforeDigest, beforeRows, afterDigest, afterRows)
	}
}
