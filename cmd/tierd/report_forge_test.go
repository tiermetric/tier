package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// The #321 report-writer forge guards.
//
// WHY A SEPARATE FILE, AND WHY IT DID NOT EXIST. docs/security.md §8 cited "the
// per-sink *NotForgeable tests in internal/api, internal/webhook, and cmd/tierd".
// cmd/tierd had exactly one — TestRequestLogger_PathNotForgeable — and it covers
// the ACCESS LOG. No test covered a REPORT WRITER, which is a different sink
// class with a different threat model: the access log is slog (which escapes
// CR/LF even when the caller forgets), and a report writer is raw fmt.Fprintf
// (which escapes nothing). The doc sentence read as coverage of a class that had
// none, and reprice.go duly diverged from its correctly-wrapped sibling
// repairrepo.go without a single test going red.
//
// So the guards below are organised by SINK, not by field: every
// client-controlled value class a report writer interpolates gets a case, and a
// new one that arrives unwrapped fails here.
//
// The payload is the one a reviewer landed through the real stack (#321 review,
// 2026-08-04): POST /api/v1/events accepts CR/LF in developer and model
// (internal/api/events.go validates non-empty and a length cap, and applies no
// charset check anywhere), the value is stored verbatim, and the CLI reads it
// back. Raw stdout of a DRY RUN contained a standalone
//
//	time=... level=ERROR msg="reprice COMMITTED" rows=40000 repriced_by=root
//
// record, four lines above the run's own "NOTHING was written."

// forgedRecord is the second, standalone log record the payload tries to plant.
// Written as slog's own Text-handler shape because that is what makes it
// convincing to an operator grepping a maintenance log — and to a line-oriented
// SIEM, which never sees the line it was glued to.
const forgedRecord = `time=2026-08-04T00:00:00Z level=ERROR msg="reprice COMMITTED" rows=40000 repriced_by=root`

// forge wraps a benign diagnostic around the forged record, so a test can assert
// BOTH halves of the barrier's promise: the injection dies, the diagnostic
// survives. A sanitizer that returned "" would pass a forgery check alone.
func forge(diagnostic string) string {
	return diagnostic + "\r\n" + forgedRecord
}

// assertNoForgedLine is the shared assertion: the payload may appear as data on
// a line, but must never BE a line. Split on "\n" only — a lone CR is also a
// line break to a terminal, so a report that escaped LF but passed CR through
// would slip past a "\r\n" split.
func assertNoForgedLine(t *testing.T, report, diagnostic string) {
	t.Helper()

	for _, line := range strings.Split(strings.TrimRight(report, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), "time=") {
			t.Errorf("a client-controlled value forged a standalone report record.\n"+
				"  forged line: %q\n"+
				"  full report:\n%s\n"+
				"Every client-controlled value in a report writer must go through "+
				"internal/logsafe. fmt.Fprintf quotes nothing.", line, report)
		}
	}
	// Stripped, not merely escaped. logsafe REMOVES CR/LF; an implementation that
	// switched to %q alone would still pass the line check above (%q escapes the
	// newline) while losing the barrier the package doc names as primary — and
	// leaving a `\n` that any log viewer which unquotes the field resurrects.
	if strings.ContainsAny(report, "\r") {
		t.Errorf("report leaked a raw CR: %q", report)
	}
	// The control half. A barrier that ate the value would pass every assertion
	// above and make the report useless — and this is the direction the #321
	// review found broken in logsafe itself, where CR/LF padding consumed the
	// whole truncation budget and left "...(truncated)" alone.
	if !strings.Contains(report, diagnostic) {
		t.Errorf("the barrier destroyed the diagnostic: %q not found in\n%s", diagnostic, report)
	}
}

// TestPrintRepriceResult_NotForgeable is the guard for the sink that was
// actually broken. printRepriceResult interpolates two client-controlled
// LISTS — Developers and GuessedModels — through joinOrNone, which used a bare
// strings.Join.
//
// GUARD COVERAGE: revert joinOrNone to strings.Join and this test fails on all
// three subtests. Revert only the store-side logsafe.Join in the GUESS-gate
// error and TestReprice_GuessGateErrorIsNotForgeable (internal/store) fails
// instead — two different sinks, two different guards.
func TestPrintRepriceResult_NotForgeable(t *testing.T) {
	cases := []struct {
		name       string
		res        store.RepriceResult
		diagnostic string
	}{
		{
			// The reviewer's exact scenario: a DRY RUN whose own last line says
			// nothing was written, carrying a forged "COMMITTED" record.
			name:       "developer list on a dry run",
			diagnostic: "alice",
			res: store.RepriceResult{
				FromVersion: 1, ToVersion: 2, EffectiveDate: "2026-08-01",
				RowCount: 40000, ChangedRowCount: 40000,
				ByOldVersion: []store.RepriceVersionDelta{
					{OldPriceVersion: 1, NewPriceVersion: 2, RowCount: 40000},
				},
				Developers: []string{forge("alice")},
			},
		},
		{
			// GuessedModels is the sharper case: by construction these are the
			// models ABSENT from the price table, i.e. exactly the strings nothing
			// in this repo has ever validated.
			name:       "guessed-model list",
			diagnostic: "ghost-model",
			res: store.RepriceResult{
				FromVersion: 1, ToVersion: 2, EffectiveDate: "2026-08-01",
				RowCount: 10, ChangedRowCount: 10, GuessedRowCount: 10,
				ByOldVersion: []store.RepriceVersionDelta{
					{OldPriceVersion: 1, NewPriceVersion: 2, RowCount: 10},
				},
				Developers:    []string{"alice"},
				GuessedModels: []string{forge("ghost-model")},
			},
		},
		{
			// A COMMITTED run takes a different tail branch (audit line instead of
			// the "NOTHING was written" line) and must be equally unforgeable.
			name:       "developer list on a commit",
			diagnostic: "alice",
			res: store.RepriceResult{
				FromVersion: 1, ToVersion: 2, EffectiveDate: "2026-08-01",
				Committed: true, RepriceID: "deadbeef",
				RowCount: 5, ChangedRowCount: 5,
				ByOldVersion: []store.RepriceVersionDelta{
					{OldPriceVersion: 1, NewPriceVersion: 2, RowCount: 5},
				},
				Developers: []string{forge("alice")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printRepriceResult(&out, tc.res)
			assertNoForgedLine(t, out.String(), tc.diagnostic)
		})
	}
}

// TestPrintRepriceResult_DryRunStillSaysNothingWasWritten is the other half of
// the reviewer's finding, and it is a claim about the REPORT, not the sanitizer.
//
// The forgery was dangerous specifically because the run it appeared in wrote
// nothing: the operator sees a COMMITTED record and stops reading. Pinning that
// the dry run's own verdict is present and correct alongside a forgery attempt
// keeps the two facts from drifting apart — a future change that made the dry
// run silent would restore the attack's payoff without touching logsafe at all.
func TestPrintRepriceResult_DryRunStillSaysNothingWasWritten(t *testing.T) {
	var out bytes.Buffer
	printRepriceResult(&out, store.RepriceResult{
		FromVersion: 1, ToVersion: 2, EffectiveDate: "2026-08-01",
		RowCount: 40000, ChangedRowCount: 40000,
		ByOldVersion: []store.RepriceVersionDelta{{OldPriceVersion: 1, NewPriceVersion: 2, RowCount: 40000}},
		Developers:   []string{forge("alice")},
	})
	s := out.String()
	if !strings.Contains(s, "reprice DRY RUN:") {
		t.Errorf("dry run lost its header verb:\n%s", s)
	}
	if !strings.Contains(s, "NOTHING was written.") {
		t.Errorf("dry run no longer states that it wrote nothing:\n%s", s)
	}
	if strings.Contains(s, "reprice COMMITTED: from-version") {
		t.Errorf("a dry run rendered the COMMITTED header:\n%s", s)
	}
}

// TestPrintRepriceResult_BoundsTheListItPrints pins the COUNT cap, which is a
// separate barrier from the per-element one and which logsafe.Str cannot supply.
//
// A producer that invents ten thousand distinct model strings floods an
// operator's terminal even when every individual name is capped at 256 bytes.
// The cap must also be VISIBLE: silently dropping models would make the guess
// warning — whose whole job is to name what to add to the price table — quietly
// incomplete, which is worse than a long list.
func TestPrintRepriceResult_BoundsTheListItPrints(t *testing.T) {
	models := make([]string, 500)
	for i := range models {
		models[i] = "ghost-model"
	}
	var out bytes.Buffer
	printRepriceResult(&out, store.RepriceResult{
		FromVersion: 1, ToVersion: 2, EffectiveDate: "2026-08-01",
		RowCount: 10, ChangedRowCount: 10, GuessedRowCount: 10,
		ByOldVersion:  []store.RepriceVersionDelta{{OldPriceVersion: 1, NewPriceVersion: 2, RowCount: 10}},
		Developers:    []string{"alice"},
		GuessedModels: models,
	})
	s := out.String()
	if !strings.Contains(s, "(+480 more)") {
		t.Errorf("the report did not cap-and-report a 500-element model list:\n%s", s)
	}
	if len(s) > 4096 {
		t.Errorf("a 500-element list produced a %d-byte report; the count cap is not holding", len(s))
	}
}

// TestRunRepriceCmd_StderrIsNotForgeable covers the OTHER path these models reach
// an operator by: store.Reprice's GUESS-gate error, printed to stderr.
//
// Driven through the real subcommand against a real store, so the assertion is
// about the bytes an operator actually sees, end to end.
//
// ⚠️ WHAT THIS TEST DOES AND DOES NOT PIN, stated exactly, because the obvious
// reading is wrong. There are TWO barriers on this path: the store sanitizes the
// model names at construction, and the CLI wraps the whole error again with
// logsafe.Err. This test kills a regression of the FIRST one only. Measured:
// reverting the sink's logsafe.Err to a bare %v leaves this test GREEN, because
// the only error reachable here is already safe by the time it arrives.
//
// The sink wrap is therefore deliberate, correct, and NOT independently
// killable. It is defense-in-depth for the errors nobody enumerated — a driver
// error echoing a column value, a price-table parse error naming a model — and
// exercising one would need fault injection this command has no seam for. Do not
// "fix" that by deleting the wrap: a barrier that holds only for the errors
// someone thought of is precisely the defect the #321 review found. The
// construction-side barrier is pinned by TestReprice_GuessGateErrorIsNotForgeable
// in internal/store.
func TestRunRepriceCmd_StderrIsNotForgeable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reprice-forge.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	// A model absent from the price table, so --commit trips the GUESS gate and
	// the error naming it reaches stderr.
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "alice", IssueID: "issue-forge", Model: forge("ghost-model-xyz"),
		InputTok: 1000, CostMicro: 999_000, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: 3, Timestamp: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		_ = db.Close()
		t.Fatalf("seed event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1", "--commit"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1 (the GUESS gate must refuse); stderr=%s", code, errb.String())
	}
	// The gate fired for the reason we think it did, not some unrelated failure —
	// otherwise this test would pass while never exercising the sink.
	if !strings.Contains(errb.String(), "allow-guessed") {
		t.Fatalf("stderr = %q, want the GUESS-gate error; the test is not reaching the sink it claims", errb.String())
	}
	assertNoForgedLine(t, errb.String(), "ghost-model-xyz")
}

// TestPrintIssueCosts_NotForgeable guards the third report writer: the
// cost-by-issue table in `tierd score`'s zero-setup mode.
//
// Its Model column is session content — it comes off the JSONL entry's
// Message.Model, which nothing validates for charset — and it reaches a raw
// fmt.Fprintf. This one had NO test until a mutant proved it: stripping the
// logsafe wrap from the model column survived the entire cmd/tierd suite,
// because printIssueCosts printed straight to os.Stdout and nothing could
// observe it. The io.Writer parameter exists so this test can.
//
// The Issue column is deliberately NOT wrapped, and that is a measured
// distinction rather than an oversight — see
// TestPrintIssueCosts_IssueLabelIsProvablyBounded below.
func TestPrintIssueCosts_NotForgeable(t *testing.T) {
	var out bytes.Buffer
	printIssueCosts(&out, []collector.TokenEvent{{
		IssueID:   "issue-42",
		Model:     forge("claude-opus-4"),
		CostMicro: 1_000_000,
	}})
	assertNoForgedLine(t, out.String(), "claude-opus-4")
}

// TestPrintIssueCosts_IssueLabelIsProvablyBounded pins the reason the Issue
// column is printed bare, since "we looked at it and it was fine" is not a
// property anything can re-check.
//
// issueLabel returns one of this package's own constants for the unattributed
// buckets and passes a real issue id through unchanged. Issue ids are derived
// from git refnames, which cannot carry control bytes — so the value is provably
// bounded and qualifies for logsafe's documented bare-logging carve-out. If a
// future path ever admits an unconstrained issue id, this is the test that has to
// be confronted before that column can stay bare.
func TestPrintIssueCosts_IssueLabelIsProvablyBounded(t *testing.T) {
	for _, id := range []string{
		collector.UnattributedIssueID,
		collector.UnattributedMain,
		collector.UnattributedDetachedHEAD,
		collector.UnattributedNoIssue,
		"issue-42",
	} {
		if got := issueLabel(id); strings.ContainsAny(got, "\r\n") {
			t.Errorf("issueLabel(%q) = %q, which contains a raw CR/LF. The Issue column in "+
				"printIssueCosts is printed BARE on the strength of issue ids being "+
				"refname-derived and therefore control-byte-free. Either restore that bound "+
				"or route the column through logsafe.Str.", id, got)
		}
	}
}

// TestPrintRepairRepoResult_NotForgeable is the sibling guard for the report
// that was already correct — kept because "correct today" and "tested" are
// different properties, and the gap between them is exactly how reprice.go
// diverged from this file without anything going red.
//
// It covers every client-controlled value class the repair report interpolates,
// not just the developer id that
// TestPrintRepairRepoResult_DeveloperIdCannotForgeAReportLine already pins:
// session ids from the --map file, stored repo slugs, and alias identities.
func TestPrintRepairRepoResult_NotForgeable(t *testing.T) {
	cases := []struct {
		name       string
		res        store.RepairRepoResult
		diagnostic string
	}{
		{
			name:       "developer id",
			diagnostic: "alice",
			res: store.RepairRepoResult{
				Developer: forge("alice"), MappedSessionCount: 1,
				ScannedRowCount: 1, AlreadyQualifiedRowCount: 1,
			},
		},
		{
			// Session ids come out of an operator-supplied --map file, or a
			// --map-file some tool generated. Not client-controlled in the HTTP
			// sense, but not validated either.
			name:       "unresolved session id",
			diagnostic: "sess-unmapped",
			res: store.RepairRepoResult{
				Developer: "alice", MappedSessionCount: 1,
				ScannedRowCount: 2, UnqualifiedRowCount: 1, UnresolvedRowCount: 1,
				Unresolved: []store.RepairRepoUnresolved{
					{SessionID: forge("sess-unmapped"), RowCount: 1, CostMicroSum: 42},
				},
			},
		},
		{
			// StoredRepo is read back from token_events. Rows predating repo
			// validation, or written by any future path that skips it, are not
			// covered by repoid.Canonical's allowlist.
			name:       "conflict stored repo",
			diagnostic: "acme/kept",
			res: store.RepairRepoResult{
				Developer: "alice", MappedSessionCount: 1,
				ScannedRowCount: 2, AlreadyQualifiedRowCount: 1, ConflictRowCount: 1,
				Conflicts: []store.RepairRepoConflict{
					{SessionID: "sess-real", StoredRepo: forge("acme/kept"), MappedRepo: "evil/hijack", RowCount: 1},
				},
			},
		},
		{
			// Alias identities are developer ids by another name, and reach the
			// report through developer_alias rather than --developer.
			name:       "alias identity",
			diagnostic: "a-smith",
			res: store.RepairRepoResult{
				Developer: "alice", MappedSessionCount: 1,
				ScannedRowCount: 1, AlreadyQualifiedRowCount: 1,
				AliasIdentities:          []string{forge("a-smith")},
				AliasUnqualifiedRowCount: 3,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printRepairRepoResult(&out, tc.res)
			assertNoForgedLine(t, out.String(), tc.diagnostic)
		})
	}
}

// TestRepairReportBareSlugsStayProvablyBounded pins the ASSUMPTION under the one
// carve-out in these reports: printRepairRepoResult prints ByRepo.FromRepo,
// ByRepo.Repo, and Conflicts.MappedRepo BARE, with an inline rationale that they
// "came through repoid.Canonical and are provably bounded to [a-z0-9._/-]".
//
// That rationale is a claim about a function in ANOTHER package, and nothing
// made it fail when the claim went false. logsafe's own package doc allows the
// carve-out ("a value that is provably bounded ... is logged bare with an inline
// rationale instead") — which makes the proof, not the rationale, the thing that
// has to hold. Loosen repoid.validSegment to admit a control byte and this test
// fails, pointing at the three bare sinks that silently depend on it.
func TestRepairReportBareSlugsStayProvablyBounded(t *testing.T) {
	for _, bad := range []string{
		"owner/repo\nforged",
		"owner/repo\rforged",
		"owner/re po",
		"owner/repo\x1b[2J",
		"owner/repo\x00",
	} {
		if got, ok := repoid.Canonical(bad); ok {
			t.Errorf("repoid.Canonical(%q) = %q, true — it now admits a value that is NOT "+
				"bounded to [a-z0-9._/-].\n"+
				"Three sinks in cmd/tierd/repairrepo.go print canonical slugs BARE on the "+
				"strength of that bound (ByRepo.FromRepo, ByRepo.Repo, Conflicts.MappedRepo). "+
				"Either restore the allowlist or route those three through logsafe.Str.", bad, got)
		}
	}
}
