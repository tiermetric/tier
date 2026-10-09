package main

// The #718 verify-report guards.
//
// 🔴 EVERY ARM ASSERTS THE *RIGHT* ATTRIBUTION, NOT MERELY THE EXIT CODE. A tool
// that blamed every mismatch on every input would pass a naive `rc == 1` suite
// while being useless, so each divergence arm asserts BOTH the dimension that
// must have fired AND the ABSENCE of the wrong one. That absence half is the
// half a lazy implementation passes by accident.
//
// The arms map 1:1 to the issue's table: Positive, A (late events), B (price
// table), C (quality revision), D (could-not-check), plus the vacuity control
// that makes a `return 0` stub impossible to slip through.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// verifyWindowSince / verifyWindowUntil bound every fixture's report window.
// Fixed dates, not time.Now()-relative: a window that drifts with the wall clock
// makes a failing test's transcript unreproducible a day later.
const (
	verifyWindowSince = "2026-08-01"
	verifyWindowUntil = "2026-09-01"
)

// verifyFixture is a seeded database plus a manifest SERVED FROM IT by the real
// #715 emitter.
//
// 🔴 THE PRODUCER IS THE SERVED ENDPOINT, AND THAT IS THE POINT (#741). Until
// this change the fixture came from `verify-report --emit` — this command's own
// stopgap producer — so every arm below was a round trip between the verifier and
// a manifest writer that agreed with it BY CONSTRUCTION. Measured on d8beb84,
// the two had already diverged so far that feeding the real handler's bytes to
// loadManifest returned `json: cannot unmarshal string into Go struct field
// reportManifest.aggregation`: the served manifest could not be verified at all,
// on the one document this command exists to consume, and the whole suite was
// green. Driving the fixture through GET /api/v1/report_manifest is what makes
// that impossible to repeat — and it is what makes the #715 contract (store.
// ReportWatermarks, store.ReportDigests) a genuine input to these arms rather
// than a name in a comment.
type verifyFixture struct {
	dbPath       string
	manifestPath string
	outcomeIDs   map[string]int64 // merge SHA -> outcomes.id
}

// seedVerifyDB builds a database holding enough of alice's spend and outcomes
// for /scores to return a real, ranked row.
//
// The sizes are load-bearing, not arbitrary: each (developer, issue) clears
// scoring.MinAttributableTokens so no outcome is zero-token-flagged (#136), and
// the total clears scoring.MinRankedCostUSD so the row is ranked (#133). A
// fixture that tripped either floor would produce a degenerate report whose TIER
// could not move, and every arm below would pass vacuously.
func seedVerifyDB(t *testing.T) verifyFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verify.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// 🔴 ONE ROW BEFORE THE WINDOW, AND IT IS LOAD-BEARING. /scores reports the
	// installation's COST HORIZON (#512) in data_quality.cost_coverage_start,
	// derived from the earliest captured event in the WHOLE database — so without
	// a pre-window row the horizon sits at the window's own first event, and ANY
	// arm that inserts history outside the window would move the response body
	// for a reason unrelated to what it is testing. Measured: adding a June event
	// to a window-only fixture flipped window_predates_cost_capture and made a
	// ledger arm fail for the wrong reason. It also makes the fixture honest — a
	// real installation has history on both sides of any report window.
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "bob", IssueID: "issue-999", Repo: "other/repo",
		Model: "claude-sonnet-4", InputTok: 10_000,
		CostMicro: store.DollarsToMicro(0.50),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed pre-window token event: %v", err)
	}

	ids := map[string]int64{}
	for i, issue := range []string{"issue-100", "issue-101", "issue-102"} {
		ts := time.Date(2026, 8, 5+i, 12, 0, 0, 0, time.UTC)
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: "alice", IssueID: issue, Repo: "acme/tier",
			Model: "claude-sonnet-4", InputTok: 200_000, OutputTok: 20_000,
			CostMicro: store.DollarsToMicro(3.00),
			Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
			Timestamp: ts,
		}); err != nil {
			t.Fatalf("seed token event %s: %v", issue, err)
		}
		sha := "sha-alice-" + issue
		if _, err := db.InsertOutcome(ctx, store.Outcome{
			Developer: "alice", IssueID: issue, Repo: "acme/tier",
			Weight: 3.0, Quality: 1.0, MergeCommitSHA: sha,
			Source: "api-outcome", WorkType: store.WorkTypeFeature,
			Timestamp: ts,
		}); err != nil {
			t.Fatalf("seed outcome %s: %v", issue, err)
		}
		o, ok, err := db.OutcomeByMergeCommit(ctx, sha)
		if err != nil || !ok {
			t.Fatalf("read back outcome %s: ok=%v err=%v", sha, ok, err)
		}
		ids[sha] = o.ID
	}
	return verifyFixture{dbPath: path, outcomeIDs: ids}
}

// serveJSON routes one GET through the REAL read-only mux — the same handler,
// the same routes and the same auth posture `tierd serve --read-only` mounts —
// and returns the response body. No listener is opened; the request never leaves
// the process, exactly as rerunScores does it in production.
func serveJSON(t *testing.T, dbPath, target string) []byte {
	t.Helper()
	return serveJSONAs(t, dbPath, target, scoring.AggregationDeveloper, 0)
}

// serveAnonymizedJSON is serveJSON with the server in TEAM aggregation, so an arm
// can exercise the anonymized manifest — a genuinely different document (it
// carries `k` and `kanon_suppressed`, and withholds `watermarks.window` and both
// digests) that no developer-mode fixture can reach.
func serveAnonymizedJSON(t *testing.T, dbPath, target string) []byte {
	t.Helper()
	return serveJSONAs(t, dbPath, target, scoring.AggregationTeam, scoring.DefaultKAnonymity)
}

func serveJSONAs(t *testing.T, dbPath, target string, mode scoring.AggregationMode, k int) []byte {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// WithUnsealedRecompute: an anonymised live-window manifest, as a tierd
	// before #913's sealed months served it, is what verify-report still reads.
	h := api.New(db, logger, "", nil, version, api.RateLimitConfig{}, api.WithCommit(commit), api.WithUnsealedRecompute())
	if mode.Anonymized() {
		h.SetAggregation(mode, k)
	}
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", target, err)
	}
	req.RemoteAddr = "127.0.0.1:0"
	rec := &captureWriter{header: http.Header{}}
	mux.ServeHTTP(rec, req)
	if rec.status != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.status, rec.body.String())
	}
	return rec.body.Bytes()
}

// serveFixtureManifest writes the manifest the running server would publish for
// the fixture's window, with the /scores body for the SAME window attached under
// the #718 `results` extension.
//
// 🔑 THE MERGE IS DELIBERATELY A KEY INSERTION ON THE SERVED BYTES, not a
// re-encode of a Go struct. Everything except `results` is the emitter's own
// output verbatim, so a field this binary does not know about survives into the
// file and lands on the `unknown pins` line rather than being silently dropped by
// a round trip through the decode-side mirror — which is the drift #741 is about.
//
// ⚠️ `results` is attached HERE because the served emitter does not write it:
// #715 pins the INPUTS and stops, and the maintainer's ask has a third clause ("and it
// came up with THESE numbers"). An operator does the same two-request merge; the
// command's usage text says so.
func serveFixtureManifest(t *testing.T, f verifyFixture, query string) verifyFixture {
	t.Helper()
	f.manifestPath = filepath.Join(filepath.Dir(f.dbPath), "manifest.json")

	manifestBytes := serveJSON(t, f.dbPath, "/api/v1/report_manifest?"+query)
	scores := serveJSON(t, f.dbPath, "/api/v1/scores?"+query)

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(manifestBytes, &doc); err != nil {
		t.Fatalf("served manifest is not an object: %v\n%s", err, manifestBytes)
	}
	results, err := json.Marshal(manifestResults{Scores: scores})
	if err != nil {
		t.Fatalf("encode results: %v", err)
	}
	doc["results"] = results
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(f.manifestPath, append(out, '\n'), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// 🔴 THE FIXTURE MUST BE DECODABLE BY THE VERIFIER, AND THIS IS THE ASSERTION
	// THAT WOULD HAVE CAUGHT #741. loadManifest is the production decoder; if the
	// served encoding and the decode-side mirror drift again, every arm below
	// fails HERE, naming the field, instead of the suite staying green while the
	// only document this command exists to read becomes unreadable.
	m := readFixtureManifest(t, f.manifestPath)
	if m.Schema != manifestSchemaTag {
		t.Fatalf("served manifest_schema = %q, want %q", m.Schema, manifestSchemaTag)
	}
	if len(m.unknownFields) > 0 {
		t.Fatalf("the SERVED manifest carries %d field(s) this binary does not evaluate: %v. Every one of them "+
			"is a pin that goes unexamined on a green run — decode it or state why it is not a pin",
			len(m.unknownFields), m.unknownFields)
	}
	// Each of these is a pin one arm below depends on; a fixture missing any of
	// them makes that arm vacuous rather than failing it.
	if m.Results == nil || len(m.Results.Scores) == 0 {
		t.Fatal("fixture manifest carries no results — every output arm below would be vacuous")
	}
	if m.Watermarks == nil || m.window().MaxTokenEventID == nil || m.ledgers().MaxQualityHistoryID == nil {
		t.Fatal("served manifest carries no watermarks — arms A and C would be vacuous")
	}
	if m.PriceTable.TableHash == "" {
		t.Fatal("served manifest carries no price_table.table_hash — arm B would be vacuous")
	}
	// A fixture whose report has no developer row cannot show a TIER move, which
	// is the headline every divergence arm reads.
	var env scoresEnvelope
	if err := json.Unmarshal(m.Results.Scores, &env); err != nil {
		t.Fatalf("results.scores is not decodable: %v", err)
	}
	if len(env.Developers) == 0 {
		t.Fatalf("the report has no developer rows; the seed produced nothing to verify:\n%s", m.Results.Scores)
	}
	return f
}

func newVerifyFixture(t *testing.T) verifyFixture {
	t.Helper()
	return serveFixtureManifest(t, seedVerifyDB(t),
		"since="+verifyWindowSince+"&until="+verifyWindowUntil)
}

func readFixtureManifest(t *testing.T, path string) reportManifest {
	t.Helper()
	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("load emitted manifest: %v", err)
	}
	return m
}

// runVerify executes the command and returns (exit code, stdout, stderr).
func runVerify(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runVerifyReportCmd(args, &out, &errb)
	return code, out.String(), errb.String()
}

// dimLine extracts the attribution line for one dimension, split into its STATUS
// TOKEN and its detail prose. It fails the test when the line is absent, because
// "the dimension I asserted about was never printed" and "the dimension printed
// UNCHANGED" are different facts and a strings.Contains over the whole report
// cannot tell them apart.
//
// 🔴 THE SPLIT IS WHY THE REPORT HAS A STATUS COLUMN. With the status folded into
// the prose, "UNCHANGED" contains "CHANGED", so every absence assertion below —
// the half that makes arms A and B mean anything — matched on a CORRECT report.
// Comparing the token EXACTLY is the only sound form of that assertion.
func dimLine(t *testing.T, report, dim string) (status, detail string) {
	t.Helper()
	prefix := "  " + dim + ":"
	for _, line := range strings.Split(report, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimLeft(strings.TrimPrefix(line, prefix), " ")
		// "UNKNOWN" is in the list even though no correct run produces it: it is
		// dimStatus's ZERO VALUE, so a dimension that fell through every branch
		// renders as UNKNOWN, and a failure that NAMES it ("status = UNKNOWN, want
		// CHANGED") says what happened, where "carries no recognised status token"
		// only says the line could not be parsed.
		for _, tok := range []string{"NOT CHECKED", "NOT PINNED", "UNCHANGED", "CHANGED", "DRIFTED", "UNKNOWN"} {
			if strings.HasPrefix(rest, tok) {
				return tok, strings.TrimSpace(strings.TrimPrefix(rest, tok))
			}
		}
		t.Fatalf("the %q line carries no recognised status token: %q\n%s", dim, line, report)
	}
	t.Fatalf("the report has no %q line at all — the assertion below would be vacuous:\n%s", dim, report)
	return "", ""
}

// assertDimStatus pins the status token EXACTLY. This is the assertion that
// carries the "and NOT the wrong attribution" half of arms A and B: asserting
// price_table is exactly UNCHANGED is the same statement as asserting it is not
// CHANGED, with no substring trap between them.
func assertDimStatus(t *testing.T, report, dim, want string) {
	t.Helper()
	got, detail := dimLine(t, report, dim)
	if got != want {
		t.Errorf("%s status = %q, want exactly %q (detail: %q)\nfull report:\n%s", dim, got, want, detail, report)
	}
}

func assertDimDetail(t *testing.T, report, dim, want string) {
	t.Helper()
	_, detail := dimLine(t, report, dim)
	if !strings.Contains(detail, want) {
		t.Errorf("%s detail = %q, want it to contain %q\nfull report:\n%s", dim, detail, want, report)
	}
}

func assertDimDetailLacks(t *testing.T, report, dim, unwanted string) {
	t.Helper()
	_, detail := dimLine(t, report, dim)
	if strings.Contains(detail, unwanted) {
		t.Errorf("%s detail = %q, which must NOT contain %q — this is the WRONG attribution, and a tool "+
			"that blames every input on every mismatch is useless\nfull report:\n%s", dim, detail, unwanted, report)
	}
}

// ---------------------------------------------------------------------------
// The five arms, in ONE test run
// ---------------------------------------------------------------------------

// TestVerifyReport_FiveArms drives every arm in a single test function so the
// VACUITY CONTROL is structural: the same run that asserts a 0 also asserts a 1
// and a 2, and it fails unless all three codes were actually observed. A
// `return 0` stub, a `return 1` stub, and an implementation that collapsed
// "could not check" into "diverged" each fail it.
func TestVerifyReport_FiveArms(t *testing.T) {
	observed := map[int]string{}

	t.Run("positive: a clean re-run reproduces", func(t *testing.T) {
		f := newVerifyFixture(t)
		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		observed[code] = "positive"
		if code != rcReproduced {
			t.Fatalf("exit = %d, want %d (REPRODUCED); stdout=%s stderr=%s", code, rcReproduced, out, errb)
		}
		if !strings.Contains(out, "REPRODUCED") {
			t.Errorf("stdout does not carry REPRODUCED:\n%s", out)
		}
		// Nothing may claim a divergence on a clean run.
		if strings.Contains(out, "FAIL:") {
			t.Errorf("a clean re-run printed a FAIL line:\n%s", out)
		}
		// The limitation is stated even on success — see printVerifyLimits.
		if !strings.Contains(out, "#717") {
			t.Errorf("a successful run must still state that it did NOT replay the historical population:\n%s", out)
		}
	})

	t.Run("A: late token_events are attributed to token_events, NOT price_table", func(t *testing.T) {
		f := newVerifyFixture(t)

		db, err := store.Open(f.dbPath)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		// Late-arriving rows carrying an IN-WINDOW ts — the legitimate case that
		// must be detected, not the trivially-different out-of-window one.
		for i := 0; i < 4; i++ {
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
				Developer: "alice", IssueID: "issue-100", Repo: "acme/tier",
				Model: "claude-sonnet-4", InputTok: 50_000,
				CostMicro: store.DollarsToMicro(1.25),
				Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
				IdempotencyKey: fmt.Sprintf("late-%d", i),
				Timestamp:      time.Date(2026, 8, 6, 9, 0, i, 0, time.UTC),
			}); err != nil {
				_ = db.Close()
				t.Fatalf("inject late event %d: %v", i, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		observed[code] = "A"
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d (diverged); stdout=%s stderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "token_events", "CHANGED")
		assertDimDetail(t, out, "token_events", "+4 rows arrived after watermark")
		assertDimDetail(t, out, "token_events", "+5.00 USD")
		// 🔴 The absence half. The price table did not move and the report must
		// not say it did.
		assertDimStatus(t, out, "price_table", "UNCHANGED")
		// Nor did anything else.
		assertDimStatus(t, out, "quality revisions", "UNCHANGED")
		assertDimStatus(t, out, "outcomes", "UNCHANGED")
		assertDimStatus(t, out, "reprice", "UNCHANGED")
		assertDimStatus(t, out, "cost corrections", "UNCHANGED")
		assertDimStatus(t, out, "repo repairs", "UNCHANGED")
		// The developer name reaches the headline through the logsafe barrier,
		// so the expectation is built the same way rather than hand-quoted.
		if want := "FAIL: TIER for " + logsafe.Str("alice") + " moved"; !strings.Contains(out, want) {
			t.Errorf("the headline does not name the developer whose TIER moved (want %q):\n%s", want, out)
		}
	})

	t.Run("B: a swapped price table is attributed to price_table, NOT token_events", func(t *testing.T) {
		f := newVerifyFixture(t)

		// 🔑 THE SWAP IS DONE ON THE MANIFEST SIDE, DELIBERATELY. A manifest that
		// attests to table X verified on a binary carrying table Y is exactly the
		// real scenario (an archived report, a later binary), and it is HERMETIC.
		m := readFixtureManifest(t, f.manifestPath)
		realHash := m.PriceTable.TableHash
		m.PriceTable.TableHash = "tierpt1:" + strings.Repeat("ab", 32)
		m.PriceTable.Version = m.PriceTable.Version - 1
		// The results block is dropped for this arm: with it in place the
		// manifest would ALSO carry the old table's stamp inside the served
		// /scores body, and the arm would no longer isolate the price dimension.
		m.Results = nil
		swapped := filepath.Join(filepath.Dir(f.manifestPath), "swapped.json")
		writeManifest(t, swapped, m)

		code, out, errb := runVerify(t, swapped, "--db", f.dbPath)
		observed[code] = "B"
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d (diverged); stdout=%s stderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "price_table", "CHANGED")
		assertDimDetail(t, out, "price_table", shortHash(realHash))
		// 🔴 The absence half: no row moved, and the report must not invent one.
		assertDimStatus(t, out, "token_events", "UNCHANGED")
		assertDimDetailLacks(t, out, "token_events", "arrived after watermark")
		assertDimStatus(t, out, "outcomes", "UNCHANGED")
		assertDimStatus(t, out, "quality revisions", "UNCHANGED")
	})

	t.Run("C: an in-place quality revision names the outcome id and the reason", func(t *testing.T) {
		f := newVerifyFixture(t)
		outcomeID := f.outcomeIDs["sha-alice-issue-101"]
		if outcomeID == 0 {
			t.Fatal("fixture did not record an outcome id")
		}

		db, err := store.Open(f.dbPath)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		// 🔴 A GENUINELY DIFFERENT VALUE. UpdateQualityForOutcome is a NO-OP when
		// the value is unchanged (store.go: `if old == quality { return
		// tx.Commit() }`), so a 1.0 -> 1.0 "revision" would write no
		// quality_history row and this arm would pass while testing nothing.
		if err := db.UpdateQualityForOutcome(context.Background(), outcomeID, 0.75, "revert_quality", "sha-revert"); err != nil {
			_ = db.Close()
			t.Fatalf("revise quality: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		observed[code] = "C"
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d (diverged); stdout=%s stderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "quality revisions", "CHANGED")
		for _, want := range []string{
			fmt.Sprintf("outcome %d", outcomeID), // the outcome id
			"1.00 -> 0.75",                       // the transition
			"revert_quality",                     // the reason string
		} {
			assertDimDetail(t, out, "quality revisions", want)
		}
		// 🔴 THE SHARP HALF, and the reason quality needs its own ledger at all:
		// quality is MUTATED IN PLACE, so NEITHER obvious watermark moves. A
		// manifest watching only token_events and outcomes would see nothing.
		assertDimStatus(t, out, "token_events", "UNCHANGED")
		assertDimStatus(t, out, "outcomes", "UNCHANGED")
		assertDimStatus(t, out, "price_table", "UNCHANGED")
		if strings.Contains(out, "UNATTRIBUTED") {
			t.Errorf("the report failed to attribute a quality revision it had the ledger for:\n%s", out)
		}
	})

	t.Run("D: a missing database is COULD NOT CHECK, not a divergence", func(t *testing.T) {
		f := newVerifyFixture(t)
		missing := filepath.Join(t.TempDir(), "does-not-exist.db")

		code, out, errb := runVerify(t, f.manifestPath, "--db", missing)
		observed[code] = "D"
		if code != rcCannotCheck {
			t.Fatalf("exit = %d, want %d (could not check); stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
		}
		// 🔴 ASSERTED EXPLICITLY, as the issue requires. Collapsing 2 into 1 is
		// the exact failure mode the three-valued discipline exists to prevent:
		// "I could not look" would then read as "I looked and it moved", and an
		// operator would go hunting for a data change that never happened.
		if code == rcDiverged {
			t.Fatal("a missing database reported DIVERGED; could-not-check must never be a divergence")
		}
		if !strings.Contains(errb, "NOTHING was verified") {
			t.Errorf("stderr does not say that nothing was verified:\n%s", errb)
		}
		// store.Open CREATES a missing file. If the guard ever regresses, this
		// catches it — the command must not have conjured a database.
		if _, err := os.Stat(missing); err == nil {
			t.Error("verify-report CREATED the missing database it was pointed at; the os.Stat guard is not holding")
		}
	})

	// --- the vacuity control -------------------------------------------------
	//
	// Every one of 0, 1 and 2 must have been produced by a DIFFERENT arm in THIS
	// run. A stub returning any single constant fails here even if it somehow
	// satisfied an individual arm's string assertions.
	for _, want := range []int{rcReproduced, rcDiverged, rcCannotCheck} {
		if _, ok := observed[want]; !ok {
			t.Errorf("VACUITY CONTROL: no arm in this run produced exit code %d. Observed: %v. "+
				"A suite that never sees all three codes cannot distinguish a working command from a stub.",
				want, observed)
		}
	}
	if len(observed) < 3 {
		t.Errorf("VACUITY CONTROL: only %d distinct exit codes observed (%v); want 3", len(observed), observed)
	}
}

func writeManifest(t *testing.T, path string, m reportManifest) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Could-not-check is a CLASS, not one case
// ---------------------------------------------------------------------------

// TestVerifyReport_CannotCheckIsNeverADivergence covers the other doors into rc
// 2. Each is a distinct way the verification does not happen, and every one of
// them must be distinguishable from "it changed".
func TestVerifyReport_CannotCheckIsNeverADivergence(t *testing.T) {
	f := newVerifyFixture(t)
	dir := filepath.Dir(f.manifestPath)

	unknownScheme := readFixtureManifest(t, f.manifestPath)
	unknownScheme.Schema = "tiermanifest2"
	unknownSchemePath := filepath.Join(dir, "future.json")
	writeManifest(t, unknownSchemePath, unknownScheme)

	// A manifest that pins NOTHING: no results, no watermarks, no price table.
	// Reporting REPRODUCED for it would be the purest fail-open — a green tick
	// over an empty contract.
	empty := reportManifest{
		Schema:      manifestSchemaTag,
		Since:       verifyWindowSince,
		Until:       verifyWindowUntil,
		Aggregation: "developer",
	}
	emptyPath := filepath.Join(dir, "empty.json")
	writeManifest(t, emptyPath, empty)

	// 🔴 A manifest pinning ONLY price_table and rubric. Both describe THIS
	// BINARY, not the database — rubric.version is a compiled-in constant and the
	// active price table travels with the binary, pricing nothing on the read
	// path (#233). Counting them toward the quorum let a manifest earn
	// "REPRODUCED: every pinned input is unchanged" by comparing the binary to
	// itself, having read not one row.
	constantsOnly := readFixtureManifest(t, f.manifestPath)
	constantsOnly.Watermarks = nil
	constantsOnly.Results = nil
	// ⭐ AND THE DIGESTS, which are the STRONGEST data pins this manifest can
	// carry (#740) — they read the row CONTENTS, where a watermark reads only its
	// id sequence. Leaving them in would make this manifest one that pins the data
	// after all, and the arm would be asserting the opposite of what it claims.
	constantsOnly.EventsDigest, constantsOnly.OutcomesDigest = nil, nil
	constantsOnlyPath := filepath.Join(dir, "constants-only.json")
	writeManifest(t, constantsOnlyPath, constantsOnly)

	// A manifest with no aggregation mode cannot be re-run AT ALL: guessing
	// "developer" would compare a k-anonymized published report against a report
	// that names individuals, which is both wrong and a disclosure. rc 2.
	noMode := readFixtureManifest(t, f.manifestPath)
	noMode.Aggregation = ""
	noModePath := filepath.Join(dir, "no-mode.json")
	writeManifest(t, noModePath, noMode)

	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad manifest: %v", err)
	}

	cases := []struct {
		name, manifest, wantStderr string
	}{
		{"unknown manifest scheme", unknownSchemePath, "cannot interpret"},
		{"manifest pins nothing", emptyPath, "nothing about the DATA was examined"},
		{"manifest pins only the binary's own constants", constantsOnlyPath, "only compares this binary to itself"},
		{"manifest names no aggregation mode", noModePath, "manifest aggregation:"},
		{"unparseable manifest", badJSON, "parse manifest"},
		{"manifest file absent", filepath.Join(dir, "no-such-manifest.json"), "read manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runVerify(t, tc.manifest, "--db", f.dbPath)
			if code != rcCannotCheck {
				t.Fatalf("exit = %d, want %d (could not check); stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
			}
			if code == rcDiverged {
				t.Fatal("could-not-check was reported as a divergence")
			}
			if !strings.Contains(errb, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q — the test is not reaching the refusal it claims", errb, tc.wantStderr)
			}
			// 🔴 STDOUT MUST NOT CLAIM A REPRODUCTION ON A NON-ZERO RUN. The
			// first draft printed the report before deciding the verdict, so a
			// manifest that pinned nothing exited 2 while stdout said
			// REPRODUCED — the CVE re-scan's "nothing was established" reported
			// as a pass, one layer down. Anything reading stdout (a human
			// skimming, a script grepping) would have believed it.
			if strings.Contains(out, "REPRODUCED") {
				t.Errorf("a rc=%d run printed REPRODUCED on stdout:\n%s", code, out)
			}
		})
	}
}

// TestVerifyReport_HeadlineAndExitCodeCannotDisagree is the STRUCTURAL version
// of the assertion above: it drives printVerifyReport and verdict() over every
// shape of verifyResult and pins that the word on stdout matches the code the
// process returns, with no reachable combination in between.
//
// A behavioural test can only cover the shapes a fixture happens to produce.
// This covers the state space, and it is the guard that reddens if someone
// re-introduces a headline computed independently of the verdict.
func TestVerifyReport_HeadlineAndExitCodeCannotDisagree(t *testing.T) {
	// "token_events" is a QUORUM dimension (it says something about the data);
	// "price_table" deliberately is not. Both appear below so the table pins the
	// distinction rather than assuming it.
	pinned := verifyDim{Name: "token_events", Status: dimUnchanged, Detail: "x"}
	moved := verifyDim{Name: "token_events", Status: dimChanged, Detail: "x"}
	unpinned := verifyDim{Name: "token_events", Status: dimNotPinned, Detail: "x"}
	constantOnly := verifyDim{Name: "price_table", Status: dimUnchanged, Detail: "x"}
	withheld := verifyDim{Name: "events_digest", Status: dimNotCheckable, Detail: "x", bandWithheld: true}
	unchecked := verifyDim{Name: "events_digest", Status: dimNotCheckable, Detail: "x"}
	bandLine := verifyDim{Name: "attribution band", Status: dimNotCheckable, Detail: "x"}

	cases := []struct {
		name     string
		res      verifyResult
		wantRC   int
		wantWord string
	}{
		{"nothing pinned at all", verifyResult{Dims: []verifyDim{unpinned}}, rcCannotCheck, "COULD NOT CHECK"},
		{"no dims and no results", verifyResult{}, rcCannotCheck, "COULD NOT CHECK"},
		{"inputs pinned and unchanged, no results", verifyResult{Dims: []verifyDim{pinned}}, rcReproduced, "REPRODUCED"},
		{"inputs pinned and moved, no results", verifyResult{Dims: []verifyDim{moved}}, rcDiverged, "FAIL"},
		{"results pinned and identical", verifyResult{ResultsPinned: true, ResultsIdentical: true, Dims: []verifyDim{pinned}}, rcReproduced, "REPRODUCED"},
		{"results pinned and moved", verifyResult{ResultsPinned: true, Dims: []verifyDim{pinned},
			Moves: []scoreMove{{Label: "alice", Was: 1, Now: 2, Kind: "moved"}}}, rcDiverged, "FAIL"},
		{"results moved while every input is only NOT PINNED", verifyResult{ResultsPinned: true, Dims: []verifyDim{unpinned},
			Moves: []scoreMove{{Label: "alice", Was: 1, Now: 2, Kind: "moved"}}}, rcDiverged, "FAIL"},
		// Only a binary-describing dimension is pinned: nothing about the data
		// was examined, so this must not read as a reproduction.
		{"only a non-quorum dimension pinned", verifyResult{Dims: []verifyDim{constantOnly}}, rcCannotCheck, "COULD NOT CHECK"},
		// 🔴 THE REASSURING DIVERGENCE, which used to print the opposite of the
		// truth: results pinned AND identical, with an input that moved. The
		// headline must say the numbers did NOT move, not that no results were
		// pinned.
		{"input moved but the numbers are identical", verifyResult{ResultsPinned: true, ResultsIdentical: true,
			Dims: []verifyDim{moved}}, rcDiverged, "IDENTICAL to the"},
		// #1033: a band-withheld pin with nothing moved is rc 2 even though the
		// quorum holds — the withheld pin is the one that could have seen a move.
		{"band-withheld pin, nothing moved", verifyResult{ResultsPinned: true, ResultsIdentical: true,
			Dims: []verifyDim{pinned, withheld, bandLine}}, rcCannotCheck, "COULD NOT CHECK"},
		{"band-withheld pin, an input moved", verifyResult{Dims: []verifyDim{moved, withheld, bandLine}}, rcDiverged, "FAIL"},
		{"unchecked data pin, nothing moved", verifyResult{Dims: []verifyDim{pinned, unchecked}}, rcCannotCheck, "COULD NOT CHECK"},
		{"unchecked data pin, identical results", verifyResult{ResultsPinned: true, ResultsIdentical: true,
			Dims: []verifyDim{pinned, unchecked}}, rcCannotCheck, "COULD NOT CHECK"},
		{"unchecked data pin, an input moved", verifyResult{Dims: []verifyDim{moved, unchecked}}, rcDiverged, "FAIL"},
		{"unchecked data pin, results moved", verifyResult{ResultsPinned: true, Dims: []verifyDim{pinned, unchecked}}, rcDiverged, "FAIL"},
		// A CHANGED non-quorum pin with no quorum comparison left is still a
		// refutation: a detected change outranks the withheld quorum.
		{"no quorum left, a non-quorum pin moved", verifyResult{Dims: []verifyDim{
			{Name: "repo_scope_excluded", Status: dimChanged, Detail: "x"}, withheld, bandLine}}, rcDiverged, "FAIL"},
		// The numbers moved with no pin CHANGED under a band disagreement: the
		// UNATTRIBUTED checklist must name the band first.
		{"results moved under a band disagreement", verifyResult{ResultsPinned: true, Dims: []verifyDim{pinned, bandLine},
			Moves: []scoreMove{{Label: "alice", Was: 1, Now: 2, Kind: "moved"}}}, rcDiverged, "FIRST SUSPECT: the attribution band"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.verdict(); got != tc.wantRC {
				t.Fatalf("verdict() = %d, want %d", got, tc.wantRC)
			}
			var out bytes.Buffer
			printVerifyReport(&out, tc.res)
			if !strings.Contains(out.String(), tc.wantWord) {
				t.Errorf("headline for a rc=%d result does not contain %q:\n%s", tc.wantRC, tc.wantWord, out.String())
			}
			// The reassuring-divergence case must never claim no results were
			// pinned — that sentence was the defect.
			if tc.res.ResultsPinned && strings.Contains(out.String(), "pinned no results") {
				t.Errorf("a run WITH pinned results said the manifest pinned none:\n%s", out.String())
			}
			// The exclusion half: a non-zero verdict must never carry the
			// success word, and a zero verdict must never carry a failure word.
			if tc.wantRC != rcReproduced && strings.Contains(out.String(), "REPRODUCED") {
				t.Errorf("a rc=%d result printed REPRODUCED:\n%s", tc.wantRC, out.String())
			}
			if tc.wantRC == rcReproduced {
				for _, bad := range []string{"FAIL", "COULD NOT CHECK"} {
					if strings.Contains(out.String(), bad) {
						t.Errorf("a reproduced result printed %q:\n%s", bad, out.String())
					}
				}
			}
		})
	}
}

func TestVerifyResult_DataPinsNotChecked(t *testing.T) {
	cases := []struct {
		name string
		dim  verifyDim
		want bool
	}{
		{"unknown quorum pin", verifyDim{Name: "events_digest", Status: dimUnknown}, true},
		{"not checkable quorum pin", verifyDim{Name: "events_digest", Status: dimNotCheckable}, true},
		{"unchanged quorum pin", verifyDim{Name: "events_digest", Status: dimUnchanged}, false},
		{"absent quorum pin", verifyDim{Name: "events_digest", Status: dimNotPinned}, false},
		{"unknown non-quorum pin", verifyDim{Name: "price_table", Status: dimUnknown}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := verifyResult{Dims: []verifyDim{tc.dim}}
			if got := res.dataPinsNotChecked(); got != tc.want {
				t.Errorf("dataPinsNotChecked() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestVerifyReport_MissingWatermarkIsNotUnchanged is the fail-loud arm for the
// most dangerous conflation in the command.
//
// A manifest that pins no quality_history watermark or count reports NOT PINNED, not
// UNCHANGED — the two look identical to a skimming operator and mean opposite
// things. If the field were decoded into a plain int64 instead of a *int64, an
// absent watermark would read as 0 and EVERY quality revision in the database's
// history would be attributed to this window: a confident, spectacular, and
// entirely fabricated finding.
func TestVerifyReport_MissingWatermarkIsNotUnchanged(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	m.Watermarks.Ledgers.MaxQualityHistoryID = nil
	m.Watermarks.Ledgers.QualityHistoryCount = nil
	m.Results = nil
	path := filepath.Join(filepath.Dir(f.manifestPath), "no-quality-wm.json")
	writeManifest(t, path, m)

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("exit = %d, want %d; nothing that WAS pinned moved. stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}
	// 🔴 "the manifest did not pin this" and "this did not change" are different
	// facts that look identical to a skimming operator.
	assertDimStatus(t, out, "quality revisions", "NOT PINNED")
	assertDimDetail(t, out, "quality revisions", "pins no quality_history watermark")
	if !strings.Contains(out, "NOT PINNED: quality revisions") {
		t.Errorf("the LIMITS block does not disclose the unpinned dimension:\n%s", out)
	}
}

// TestVerifyReport_DigestHasThreeDistinctOutcomes pins that the digest dimension
// says THREE different things and never collapses them (#740).
//
// 🔴 IT USED TO SAY ONE THING. Before this change verifyDimEventsDigest returned
// dimNotCheckable UNCONDITIONALLY — "#716 is not in this binary" — and the
// manifest emitted no digest, so the pair produced `NOT PINNED` on an otherwise
// green rc-0 run. The three arms below exist so that neither half can silently
// become the only outcome again: a build that could not compute a digest would
// fail the "matching" arm, and a build that reported everything as CHANGED would
// fail it too.
//
// ⛔ THE `NOT CHECKED` ARM MUST STAY REACHABLE. It is the rollback seam for a
// future tierdig2 — the same role the manifest_schema tag plays one level up —
// and deleting it would make every archived manifest under an older scheme read
// as a forgery rather than as unreadable.
func TestVerifyReport_DigestHasThreeDistinctOutcomes(t *testing.T) {
	observed := map[string]string{}

	t.Run("pinned and matching: the served digest recomputes", func(t *testing.T) {
		f := newVerifyFixture(t)
		m := readFixtureManifest(t, f.manifestPath)
		// The fixture must actually carry one, or this arm proves nothing about
		// wiring — it would pass identically on a build that emits no digest.
		if m.EventsDigest == nil || m.EventsDigest.Value == "" {
			t.Fatal("the SERVED manifest carries no events_digest: #716 is in the binary but nothing wired it " +
				"to the manifest, which is exactly the #740 gap")
		}
		if m.EventsDigest.Rows == 0 {
			t.Fatalf("events_digest.rows = 0: the digest was published without a denominator, so a digest over "+
				"an empty window is indistinguishable from one that measured something (value %s)", m.EventsDigest.Value)
		}

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcReproduced {
			t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcReproduced, out, errb)
		}
		observed["UNCHANGED"] = "matching"
		assertDimStatus(t, out, "events_digest", "UNCHANGED")
		assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
		assertDimDetail(t, out, "events_digest", "every digested column")
	})

	t.Run("not pinned: an older manifest still verifies", func(t *testing.T) {
		f := newVerifyFixture(t)
		m := readFixtureManifest(t, f.manifestPath)
		m.EventsDigest, m.OutcomesDigest = nil, nil
		path := filepath.Join(filepath.Dir(f.manifestPath), "no-digest.json")
		writeManifest(t, path, m)

		code, out, errb := runVerify(t, path, "--db", f.dbPath)
		if code != rcReproduced {
			t.Fatalf("exit = %d, want %d (an unpinned dimension is not a divergence); stdout=%s stderr=%s",
				code, rcReproduced, out, errb)
		}
		observed["NOT PINNED"] = "absent"
		assertDimStatus(t, out, "events_digest", "NOT PINNED")
		// 🔴 The reason a digest matters is stated on the line an operator reads,
		// not only in a comment: this is the check whose absence lets an in-place
		// edit through.
		assertDimDetail(t, out, "events_digest", "in-place edit")
		if !strings.Contains(out, "NOT PINNED: events_digest") {
			t.Errorf("the LIMITS block does not disclose the unpinned digest:\n%s", out)
		}
	})

	t.Run("not checked: an unknown scheme is neither agreement nor divergence", func(t *testing.T) {
		f := newVerifyFixture(t)
		m := readFixtureManifest(t, f.manifestPath)
		m.EventsDigest = &manifestDigest{Value: "tierdig2:" + strings.Repeat("cd", 32), Rows: 3}
		m.OutcomesDigest = nil
		path := filepath.Join(filepath.Dir(f.manifestPath), "future-digest.json")
		writeManifest(t, path, m)

		code, out, errb := runVerify(t, path, "--db", f.dbPath)
		if code != rcCannotCheck {
			t.Errorf("exit = %d, want %d (an unchecked pinned digest prevents reproduction); stdout=%s stderr=%s",
				code, rcCannotCheck, out, errb)
		}
		if strings.Contains(out, "REPRODUCED") || !strings.Contains(out, "COULD NOT CHECK") {
			t.Errorf("an unchecked pinned digest must refuse reproduction:\n%s", out)
		}
		if !strings.Contains(errb, "pins data that was NOT CHECKED") {
			t.Errorf("stderr does not name the unchecked data pins:\n%s", errb)
		}
		if strings.Contains(errb, "pins no results") {
			t.Errorf("stderr describes unchecked data pins as absent:\n%s", errb)
		}
		if !strings.Contains(errb, "events_digest") || !strings.Contains(errb, "cannot compute") || strings.Contains(errb, "#1033") {
			t.Errorf("stderr must name the unsupported digest, not the band issue: %s", errb)
		}
		observed["NOT CHECKED"] = "unknown scheme"
		assertDimStatus(t, out, "token_events", "UNCHANGED")
		assertDimStatus(t, out, "outcomes", "UNCHANGED")
		assertDimStatus(t, out, "events_digest", "NOT CHECKED")
		assertDimDetail(t, out, "events_digest", "cannot compute")
		// The SIBLING, dropped above, must read NOT PINNED — not NOT CHECKED. One
		// unreadable pin must not make the whole block uncheckable, and a build
		// that collapsed the two statuses would pass every assertion above.
		assertDimStatus(t, out, "outcomes_digest", "NOT PINNED")
		if !strings.Contains(out, "NOT CHECKED: events_digest") {
			t.Errorf("the LIMITS block does not disclose the unchecked digest:\n%s", out)
		}
	})

	// The vacuity control, in the shape TestVerifyReport_FiveArms uses: all three
	// statuses must have been produced by DIFFERENT arms in THIS run. A build that
	// hard-coded any one of them — which is precisely what #740 found — fails here
	// even if an individual arm's string assertions somehow passed.
	for _, want := range []string{"UNCHANGED", "NOT PINNED", "NOT CHECKED"} {
		if _, ok := observed[want]; !ok {
			t.Errorf("VACUITY CONTROL: no arm produced digest status %q. Observed: %v. A dimension that can only "+
				"say one thing is what this issue was about.", want, observed)
		}
	}
}

// TestVerifyReport_ContentEditIsCaughtByTheDigestAlone is THE arm for #740, and
// it is the whole argument for the issue stated as a measurement.
//
// 🔴 BEFORE THIS CHANGE IT RETURNED rc 0 REPRODUCED. The edit below moves no id
// and no row count, so every one of the seven watermarks correctly holds still —
// store.Watermarks says in terms that a MAX(id)/COUNT(*) pair "CANNOT see an
// in-place UPDATE" — and it changes no published figure, so the results
// comparison holds still too. The digest over the row CONTENTS is the ONLY thing
// in the program that can see it.
//
// billing_mode is the column chosen deliberately, and the choice is load-bearing:
//   - it IS inside the digest (eventsdigest.go's tokenEventDigestRow),
//   - it is NOT published by /api/v1/scores, so the results comparison stays
//     IDENTICAL and this arm cannot pass for the wrong reason, and
//   - a real `tierd reprice` rewrites it in place, so the scenario is not exotic.
//
// The assertions therefore say more than rc == 1: they name the digest as the
// dimension that fired, assert every other dimension did NOT, and assert the
// headline says the numbers were IDENTICAL — which together mean the digest is
// carrying the finding alone.
func TestVerifyReport_ContentEditIsCaughtByTheDigestAlone(t *testing.T) {
	f := newVerifyFixture(t)
	id := tokenEventIDIn(t, f.dbPath, true)
	execOnFixture(t, f.dbPath, "UPDATE token_events SET billing_mode = 'subscription' WHERE id = ? AND billing_mode <> 'subscription'", id)

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d (diverged). An in-place content edit that moves no watermark and no "+
			"published figure is visible ONLY to the digest; rc 0 here means the digest is not wired or not "+
			"compared.\nstdout=%s\nstderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "events_digest", "CHANGED")
	// It must name WHAT kind of change: equal counts with an unequal digest is an
	// in-place edit, which is the reading an operator would otherwise get backwards.
	assertDimDetail(t, out, "events_digest", "in-place edit")

	// 🔴 THE ABSENCE HALF, and it is what makes this arm an attribution rather
	// than a bare exit code. Every watermark dimension is structurally blind to
	// this edit and must say so.
	for _, dim := range []string{
		"token_events", "outcomes", "quality revisions",
		"reprice", "cost corrections", "repo repairs", "push reconciliations",
		"price_table", "rubric",
	} {
		assertDimStatus(t, out, dim, "UNCHANGED")
	}
	// outcomes were not touched, so their digest must hold — a build that reported
	// every digest as CHANGED would pass every assertion above.
	assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
	// And the published numbers did NOT move, which is what proves the digest is
	// carrying this finding on its own rather than riding a results divergence.
	if !strings.Contains(out, "IDENTICAL to the") {
		t.Errorf("the recomputed report should be identical (billing_mode is not published by /scores), so the "+
			"headline must say a pinned INPUT moved while the numbers held:\n%s", out)
	}
}

// TestVerifyReport_UnattributedIsNamed pins the case an operator most needs
// spelled out: the numbers moved and nothing we could check moved with them.
//
// Without this line the report would be a wall of UNCHANGED under a FAIL
// headline, which reads as a broken tool rather than as the finding it is.
func TestVerifyReport_UnattributedIsNamed(t *testing.T) {
	f := newVerifyFixture(t)
	// Keep the results, drop every watermark AND both digests: the report will
	// differ (a late row is injected below) while no input dimension can see why.
	//
	// ⭐ THE DIGESTS HAVE TO GO TOO, and that is a finding rather than test
	// bookkeeping (#740): with them pinned, this arm no longer reaches
	// UNATTRIBUTED at all — the inserted row moves events_digest and the run is
	// attributed correctly. That is the digest closing part of the hole this
	// message exists to describe. The message still has to work for a manifest
	// that pins neither, which is what this arm now builds.
	m := readFixtureManifest(t, f.manifestPath)
	m.Watermarks = nil
	m.EventsDigest, m.OutcomesDigest = nil, nil
	path := filepath.Join(filepath.Dir(f.manifestPath), "no-watermarks.json")
	writeManifest(t, path, m)

	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "alice", IssueID: "issue-100", Repo: "acme/tier",
		Model: "claude-sonnet-4", InputTok: 90_000,
		CostMicro: store.DollarsToMicro(2.00),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		_ = db.Close()
		t.Fatalf("inject: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	if !strings.Contains(out, "UNATTRIBUTED") {
		t.Errorf("the numbers moved with no attributable input and the report did not say so:\n%s", out)
	}
	// An ABSENT watermarks block must produce SEVEN NOT PINNED lines, one per
	// dimension, each still carrying its own name. It is a single list here on
	// purpose: verifyDimsWatermarks used to special-case the nil block with a
	// second, hand-typed copy of these names, and a rename on one path would not
	// have reddened anything.
	for _, dim := range []string{
		"token_events", "outcomes", "quality revisions",
		"reprice", "cost corrections", "repo repairs", "push reconciliations",
	} {
		assertDimStatus(t, out, dim, "NOT PINNED")
	}
}

// ---------------------------------------------------------------------------
// Registration + usage
// ---------------------------------------------------------------------------

// TestDispatch_VerifyReportRegistered proves the subcommand is reachable through
// dispatch. The usage half is asserted by TestDispatch_Help, which requires every
// registered command name to appear in printUsage.
//
// ⚠️ An explicit --db even though this run never reaches store.Open: without it
// the flag defaults to defaultDBPath(), the operator's REAL ~/.tier/tier.db.
func TestDispatch_VerifyReportRegistered(t *testing.T) {
	f := newVerifyFixture(t)
	var out, errb bytes.Buffer
	code := dispatch([]string{"verify-report", f.manifestPath, "--db", f.dbPath}, &out, &errb)
	if code != rcReproduced {
		t.Fatalf("dispatch exit = %d, want %d; stdout=%s stderr=%s", code, rcReproduced, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "REPRODUCED") {
		t.Errorf("dispatch did not route to verify-report:\n%s", out.String())
	}
}

func TestRunVerifyReportCmd_HelpIsSuccess(t *testing.T) {
	code, _, _ := runVerify(t, "-h")
	if code != 0 {
		t.Fatalf("-h exit = %d, want 0", code)
	}
}

func TestRunVerifyReportCmd_NoManifestIsAUsageError(t *testing.T) {
	// 🔴 A usage error is 1, NOT 2. rc 2 is a claim about the DATA ("the
	// verification did not happen"); a missing argument is a claim about the
	// command line, and the operator is right there reading it.
	code, _, errb := runVerify(t, "--db", filepath.Join(t.TempDir(), "x.db"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a usage error; stderr=%s", code, errb)
	}
	if !strings.Contains(errb, "manifest path is required") {
		t.Errorf("stderr = %q, want the missing-manifest usage error", errb)
	}
}

// ---------------------------------------------------------------------------
// Forge guards (#321) — this command is a report writer
// ---------------------------------------------------------------------------

// TestPrintVerifyReport_NotForgeable covers the SINK: every client-controlled
// value class printVerifyReport interpolates gets a case, in the style
// report_forge_test.go establishes.
//
// The values here reach the printer from two different trust boundaries, and
// both are unvalidated: the developer/cohort labels come out of the database via
// the served /scores body (POST /api/v1/events accepts CR/LF in `developer`),
// and the schema/window/scope/mode strings come out of an operator-supplied
// manifest FILE, which nothing has ever charset-checked.
func TestPrintVerifyReport_NotForgeable(t *testing.T) {
	base := func() verifyResult {
		return verifyResult{
			Schema:              manifestSchemaTag,
			Since:               verifyWindowSince,
			Until:               verifyWindowUntil,
			Aggregation:         "developer",
			K:                   5,
			ResultsPinned:       true,
			ResultsIdentical:    false,
			ToolVersionNow:      "v0.4.1",
			ToolVersionManifest: "v0.4.0",
			Dims: []verifyDim{
				{Name: "price_table", Status: dimUnchanged, Detail: "UNCHANGED (tierpt1:aa3f0011…, version 9)"},
			},
		}
	}

	cases := []struct {
		name       string
		diagnostic string
		mutate     func(*verifyResult)
	}{
		{
			// The reviewer's exact #321 scenario, one sink over: a developer id
			// carrying a forged slog record, printed on the FAIL headline an
			// operator reads first.
			name:       "developer id on the failure headline",
			diagnostic: "alice",
			mutate: func(r *verifyResult) {
				r.Moves = []scoreMove{{Label: forge("alice"), Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
		{
			// A row that appeared takes a different branch of the headline.
			name:       "developer id on an appeared row",
			diagnostic: "mallory",
			mutate: func(r *verifyResult) {
				r.Moves = []scoreMove{{Label: forge("mallory"), Now: 12.5, Kind: "appeared"}}
			},
		},
		{
			// And one that vanished takes a third.
			name:       "developer id on a vanished row",
			diagnostic: "bob",
			mutate: func(r *verifyResult) {
				r.Moves = []scoreMove{{Label: forge("bob"), Was: 12.5, Kind: "vanished"}}
			},
		},
		{
			// The repo scope is manifest content, printed in the header line.
			name:       "repo scope from the manifest",
			diagnostic: "acme/tier",
			mutate: func(r *verifyResult) {
				r.Scope = forge("acme/tier")
				r.Moves = []scoreMove{{Label: "alice", Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
		{
			// So are the window bounds, the aggregation mode and the scheme tag.
			name:       "window bound from the manifest",
			diagnostic: "2026-08-01",
			mutate: func(r *verifyResult) {
				r.Since = forge("2026-08-01")
				r.Moves = []scoreMove{{Label: "alice", Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
		{
			name:       "aggregation mode from the manifest",
			diagnostic: "developer",
			mutate: func(r *verifyResult) {
				r.Aggregation = forge("developer")
				r.Moves = []scoreMove{{Label: "alice", Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
		{
			name:       "manifest scheme tag",
			diagnostic: manifestSchemaTag,
			mutate: func(r *verifyResult) {
				r.Schema = forge(manifestSchemaTag)
				r.Moves = []scoreMove{{Label: "alice", Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
		{
			// tool_version and commit are also manifest content, on the build
			// line that a divergent run points an operator at.
			name:       "tool_version from the manifest",
			diagnostic: "v0.4.0",
			mutate: func(r *verifyResult) {
				r.ToolVersionManifest = forge("v0.4.0")
				r.Moves = []scoreMove{{Label: "alice", Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
		{
			name:       "commit from the manifest",
			diagnostic: "deadbeef",
			mutate: func(r *verifyResult) {
				r.CommitManifest = forge("deadbeef")
				r.CommitNow = "cafef00d"
				r.Moves = []scoreMove{{Label: "alice", Was: 41.20, Now: 39.85, Kind: "moved"}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(&r)
			var out bytes.Buffer
			printVerifyReport(&out, r)
			assertNoForgedLine(t, out.String(), tc.diagnostic)
		})
	}
}

// TestRunVerifyReportCmd_QualityReasonIsNotForgeable is the END-TO-END forge
// guard, driven through the real subcommand against a real store.
//
// The quality `reason` is the sharpest value class this command prints: it is
// stored VERBATIM by UpdateQualityForOutcome (store.go INSERTs it unvalidated),
// it is free text with no enum by design, and it reaches an operator inside a
// line that already looks like a maintenance record. The assertion is about the
// bytes an operator actually sees, not about a struct field.
func TestRunVerifyReportCmd_QualityReasonIsNotForgeable(t *testing.T) {
	f := newVerifyFixture(t)
	outcomeID := f.outcomeIDs["sha-alice-issue-102"]

	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db.UpdateQualityForOutcome(context.Background(), outcomeID, 0.7, forge("ci-fail"), "sha-ci"); err != nil {
		_ = db.Close()
		t.Fatalf("revise quality: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	// The gate fired for the reason we think it did — otherwise this test would
	// pass while never reaching the sink it claims to cover.
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; the test is not reaching the quality sink. stderr=%s", code, rcDiverged, errb)
	}
	if !strings.Contains(out, fmt.Sprintf("outcome %d", outcomeID)) {
		t.Fatalf("the report never named the revised outcome; the sink was not reached:\n%s", out)
	}
	assertNoForgedLine(t, out, "ci-fail")
}

// ---------------------------------------------------------------------------
// Unit coverage for the dimension logic
// ---------------------------------------------------------------------------

// TestVerifyDimPriceTable covers the three-valued price dimension directly,
// including the branch a behavioural test cannot easily reach: an unchanged
// file_hash under a changed table_hash, which means the CANONICALIZATION moved
// rather than the prices — a different and far more alarming fact.
func TestVerifyDimPriceTable(t *testing.T) {
	now := store.PriceTableInfo{
		Version: 9, EffectiveDate: "2026-08-01",
		TableHash: "tierpt1:" + strings.Repeat("11", 32),
		FileHash:  "sha256:" + strings.Repeat("22", 32),
	}
	t.Run("unchanged", func(t *testing.T) {
		d := verifyDimPriceTable(manifestPriceTable{Version: 9, TableHash: now.TableHash, FileHash: now.FileHash}, now)
		if d.Status != dimUnchanged {
			t.Fatalf("status = %v, want unchanged (%q)", d.Status, d.Detail)
		}
	})
	t.Run("not pinned", func(t *testing.T) {
		d := verifyDimPriceTable(manifestPriceTable{}, now)
		if d.Status != dimNotPinned {
			t.Fatalf("status = %v, want not-pinned (%q)", d.Status, d.Detail)
		}
		if strings.Contains(d.Detail, "UNCHANGED") {
			t.Errorf("an unpinned price table rendered as UNCHANGED: %q", d.Detail)
		}
	})
	t.Run("changed prices", func(t *testing.T) {
		d := verifyDimPriceTable(manifestPriceTable{
			Version: 8, TableHash: "tierpt1:" + strings.Repeat("33", 32),
			FileHash: "sha256:" + strings.Repeat("44", 32),
		}, now)
		if d.Status != dimChanged {
			t.Fatalf("status = %v, want changed (%q)", d.Status, d.Detail)
		}
		if strings.Contains(d.Detail, "file_hash is IDENTICAL") {
			t.Errorf("a genuine price edit was reported as a canonicalization change: %q", d.Detail)
		}
	})
	t.Run("changed canonicalization, identical source bytes", func(t *testing.T) {
		d := verifyDimPriceTable(manifestPriceTable{
			Version: 9, TableHash: "tierpt1:" + strings.Repeat("33", 32),
			FileHash: now.FileHash,
		}, now)
		if d.Status != dimChanged {
			t.Fatalf("status = %v, want changed (%q)", d.Status, d.Detail)
		}
		if !strings.Contains(d.Detail, "file_hash is IDENTICAL") {
			t.Errorf("the same source bytes producing a different resolved table was not called out: %q", d.Detail)
		}
	})
}

// TestFormatTIERPair pins the rounding escape hatch: a real move smaller than a
// cent must not render as "41.20 -> 41.20", which reads as a bug in the tool
// rather than a finding about the data.
func TestFormatTIERPair(t *testing.T) {
	a, b := formatTIERPair(41.2000001, 41.2000002)
	if a == b {
		t.Fatalf("a sub-cent move rendered as two identical strings: %q -> %q", a, b)
	}
	a, b = formatTIERPair(41.20, 39.85)
	if a != "41.20" || b != "39.85" {
		t.Fatalf("a normal move did not render at two decimals: %q -> %q", a, b)
	}
}

// TestShortHash pins that abbreviation KEEPS the scheme tag. The tag is the
// rollback seam (#713): dropping it would make two different canonicalizations
// print as the same value.
func TestShortHash(t *testing.T) {
	got := shortHash("tierpt1:" + strings.Repeat("ab", 32))
	if !strings.HasPrefix(got, "tierpt1:") {
		t.Errorf("shortHash dropped the scheme tag: %q", got)
	}
	if short := shortHash("tierpt1:abc"); short != "tierpt1:abc" {
		t.Errorf("an already-short hash was mangled: %q", short)
	}
}

// ---------------------------------------------------------------------------
// Forward compatibility: a NEWER manifest is disclosed, not refused
// ---------------------------------------------------------------------------

// TestVerifyReport_UnknownPinIsDisclosedNotSilentlySkipped pins the ruling in
// unknownManifestFields, in BOTH directions — which is the whole point, because
// the two obvious implementations each fail one direction.
//
//   - json.Decoder.DisallowUnknownFields would refuse the manifest (rc 2). That
//     is what the first draft did, and #715's own spec adds row counts to the
//     watermark block — so the very next sibling PR would emit a tiermanifest1
//     this binary rejects entirely, and "written by a newer tierd" would be
//     indistinguishable from "corrupt file".
//   - Plain json.Unmarshal accepts it and says NOTHING, handing an operator a
//     green tick over a pin nobody looked at.
//
// The correct answer is neither: accept, and DISCLOSE by name.
func TestVerifyReport_UnknownPinIsDisclosedNotSilentlySkipped(t *testing.T) {
	f := newVerifyFixture(t)

	// Inject a field a future tierd might add, into the REAL emitted manifest so
	// everything else about it still verifies.
	var raw map[string]any
	b, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	raw["row_counts"] = map[string]any{"token_events": 3}
	raw["some_future_pin"] = "tierfuture1:abcdef"
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	path := filepath.Join(filepath.Dir(f.manifestPath), "newer.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	code, stdout, stderr := runVerify(t, path, "--db", f.dbPath)
	// NOT refused. A newer manifest whose known pins all match still verifies.
	if code != rcReproduced {
		t.Fatalf("exit = %d, want %d — a NEWER manifest must not be refused outright; stdout=%s stderr=%s",
			code, rcReproduced, stdout, stderr)
	}
	// And NOT silent.
	assertDimStatus(t, stdout, "unknown pins", "NOT CHECKED")
	for _, want := range []string{"row_counts", "some_future_pin", "NEWER tierd", "hand-written or misspelled key"} {
		assertDimDetail(t, stdout, "unknown pins", want)
	}
	if !strings.Contains(stdout, "NOT CHECKED: unknown pins") {
		t.Errorf("the LIMITS block does not disclose the unexamined pins:\n%s", stdout)
	}

	// The CONTROL half: a manifest this binary fully understands must print NO
	// unknown-pins line at all. Without this, an implementation that emitted the
	// line unconditionally would pass every assertion above while making the
	// disclosure meaningless noise.
	_, cleanOut, _ := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if strings.Contains(cleanOut, "unknown pins") {
		t.Errorf("a manifest with no unknown fields still printed an unknown-pins line:\n%s", cleanOut)
	}
}

// TestUnknownManifestFields_WalkIsNestedAndReflective is the COVERAGE PIN under
// unknownManifestFields, and it is structural on purpose.
//
// Two things it exists to stop, both of which a behavioural test cannot see:
//
//  1. A TOP-LEVEL-ONLY walk. The second draft was exactly that, and it missed
//     the extension its own justification comment cites — #715's row counts land
//     INSIDE `watermarks`, so a newer manifest's new pin produced no disclosure
//     at all.
//  2. A HAND-TYPED denominator. The same draft kept a `manifestKnownFields` map
//     beside the struct; a field added to one and not the other would have made
//     every manifest this binary emits report its own field as an unexamined
//     "unknown pin", forever, with nothing going red.
func TestUnknownManifestFields_WalkIsNestedAndReflective(t *testing.T) {
	// The nested unknown is TWO levels down (`watermarks.window.future_bound`),
	// which is deeper than the shape the second draft's top-level walk could
	// reach and deeper than the served block's own nesting — so a walk that
	// recursed exactly one level would still fail here.
	raw := []byte(`{
	  "manifest_schema": "tiermanifest1",
	  "since": "2026-08-01",
	  "watermarks": {
	    "window": {"max_token_event_id": 3, "future_bound": "x"},
	    "row_counts": {"token_events": 3}
	  },
	  "results": {"scores": {"developers": [{"anything": "at all"}]}},
	  "some_future_pin": "y"
	}`)
	got, err := unknownManifestFields(raw)
	if err != nil {
		t.Fatalf("unknownManifestFields: %v", err)
	}
	want := []string{"some_future_pin", "watermarks.row_counts", "watermarks.window.future_bound"}
	if len(got) != len(want) {
		t.Fatalf("unknown fields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unknown fields = %v, want %v (sorted, dotted paths)", got, want)
			break
		}
	}
	// 🔴 results.scores MUST NOT be walked. It holds the /api/v1/scores body
	// verbatim — a contract internal/api owns and grows routinely — so reporting
	// its fields as "unknown pins" would flood the disclosure with noise on every
	// single run and train an operator to ignore the line. The absence of
	// "results.scores.developers" above is that assertion; this is the sentence
	// that says it was deliberate.
	for _, g := range got {
		if strings.HasPrefix(g, "results.scores") {
			t.Errorf("the walk descended into the opaque served body: %q", g)
		}
	}
}

// TestUnknownManifestFields_CleanManifestFindsNothing is the found == 0 control.
// A walk that silently matched nothing would satisfy every "is this reported"
// assertion above by reporting everything, and every "is this quiet" assertion
// by reporting nothing — this pins which side it fails on.
func TestUnknownManifestFields_CleanManifestFindsNothing(t *testing.T) {
	f := newVerifyFixture(t)
	raw, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	got, err := unknownManifestFields(raw)
	if err != nil {
		t.Fatalf("unknownManifestFields: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a manifest this binary EMITTED reports %v as unknown to it — the walk's denominator "+
			"has drifted from the struct it is supposed to be derived from", got)
	}
	// The control's control: prove the same walk DOES find something, so a
	// zero above is an earned zero and not a walk that never ran.
	probe, err := unknownManifestFields([]byte(`{"manifest_schema":"tiermanifest1","definitely_not_a_field":1}`))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(probe) != 1 || probe[0] != "definitely_not_a_field" {
		t.Fatalf("the walk found %v on a manifest with a planted unknown field; the zero above proves nothing", probe)
	}
}

// ---------------------------------------------------------------------------
// The arms the review found missing
// ---------------------------------------------------------------------------

// execOnFixture runs a statement against a fixture database on its own handle,
// so a test can construct a state the store's public writers cannot reach
// (an audit-ledger row, a deletion). Deliberately NOT store.Open: these
// statements are the test's fixture, not a code path under test.
func execOnFixture(t *testing.T, dbPath, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(context.Background(), stmt, args...)
	if err != nil {
		t.Fatalf("exec fixture stmt: %v\n%s", err, stmt)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	// A fixture that silently affected nothing makes every assertion that
	// follows vacuous — the failure mode this file's arms exist to avoid.
	//
	// ⚠️ IT CANNOT CATCH A SAME-VALUE UPDATE. SQLite's changes() counts rows
	// MATCHED AND WRITTEN, not rows whose value differs, so `SET x = 'a'` on a row
	// already holding 'a' reports 1. A caller whose arm depends on the value
	// actually MOVING must say so in the predicate — see the billing_mode edits,
	// which carry `AND billing_mode <> 'subscription'` for exactly this reason.
	if n == 0 {
		t.Fatalf("fixture statement affected 0 rows; the arm below would be vacuous:\n%s", stmt)
	}
}

// tokenEventIDIn returns one token_events id inside (or outside) the fixture
// window, so a ledger row can be pointed at a specific one.
func tokenEventIDIn(t *testing.T, dbPath string, inWindow bool) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	cmp := ">="
	if !inWindow {
		cmp = "<"
	}
	var id int64
	err = db.QueryRowContext(context.Background(),
		"SELECT id FROM token_events WHERE ts "+cmp+" ? ORDER BY id LIMIT 1",
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)).Scan(&id)
	if err != nil {
		t.Fatalf("find token event (inWindow=%v): %v", inWindow, err)
	}
	return id
}

// TestVerifyReport_PriceTableHashCannotForgeALine is the R2 guard, and the sink
// it covers is the one the first draft missed.
//
// Every OTHER client-controlled value in this report is wrapped at the printer.
// price_table.table_hash is wrapped at its PRODUCER, verifyDimPriceTable, because
// by the time printVerifyReport sees it, it is inside a pre-formatted Detail
// string. shortHash is not a barrier: it truncates only when the value splits on
// ':' with a tail longer than 8 bytes, and otherwise returns the manifest's bytes
// VERBATIM. Measured on the unfixed code, a table_hash of
// "x\n  quality revisions   UNCHANGED   ..." rendered as two lines, the second of
// which this file's own dimLine helper parses as a genuine attribution — i.e. a
// manifest could make the verifier appear to assert the WRONG attribution, the
// one thing the status column exists to make unforgeable.
func TestVerifyReport_PriceTableHashCannotForgeALine(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	// 🔴 THE PAYLOAD CARRIES NO COLON OF ITS OWN, AND THAT IS THE WHOLE TEST.
	// An earlier version of this arm used forge("tierpt1:deadbeef") and PASSED
	// against the unfixed code — measured. shortHash cuts at the FIRST colon and
	// truncates the tail to 8 bytes, so a payload whose own colon comes first has
	// its injection sliced off by an accident of formatting, and the arm proves
	// nothing. With no leading colon the cut lands inside the forged timestamp
	// instead, the CR/LF ends up in the part shortHash returns UNTRUNCATED, and a
	// second line appears. That is the real shape, and it is why shortHash must
	// never be mistaken for a barrier.
	m.PriceTable.TableHash = forge("deadbeef")
	m.Results = nil
	path := filepath.Join(filepath.Dir(f.manifestPath), "forged-hash.json")
	writeManifest(t, path, m)

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	// The sink is only reached on a CHANGED price table; assert we got there
	// rather than passing because nothing was printed.
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; the forged hash never reached the sink. stderr=%s", code, rcDiverged, errb)
	}
	assertDimStatus(t, out, "price_table", "CHANGED")
	assertNoForgedLine(t, out, "deadbeef")
}

// insertVerifyEvent adds one in-window token event to a named repository through
// the ordinary write path, then closes the store so the next verify run reads a
// settled file. It returns the repository's token_events row count AFTER the
// insert.
//
// 🔴 IT COUNTS RATHER THAN TRUSTING THE WRITER, and the reason is measured. This
// helper sets up the arm that asserts another repository's write does NOT move a
// scoped digest — an assertion whose two readings ("nobody wrote" and "the write
// was correctly ignored") are IDENTICAL unless something proves the write landed.
// Measured: gutting this function's body to a no-op left
// TestVerifyReport_ScopedManifestIsNotBlind reporting ok. The risk is not
// hypothetical either — InsertTokenEvent returns nil on a duplicate
// idempotency_key, so a future edit that adds one would silently empty the arm.
//
// Its sibling editVerifyRowInPlace has carried a RowsAffected control from the
// start; this is the same discipline on the insert side.
func insertVerifyEvent(t *testing.T, dbPath, repo, dev, issue string, at time.Time) int64 {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen %s: %v", dbPath, err)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev, IssueID: issue, Repo: repo,
		Model: "claude-sonnet-4", InputTok: 60_000,
		CostMicro: store.DollarsToMicro(1.50),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: at,
	}); err != nil {
		_ = db.Close()
		t.Fatalf("insert %s event: %v", repo, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Counted over a SEPARATE handle after the close, so the number describes the
	// settled file the verifier will actually read.
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var n int64
	if err := raw.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM token_events WHERE repo = ?`, repo).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", repo, err)
	}
	return n
}

// editVerifyRowInPlace rewrites billing_mode on every token_events row of one
// repository — an edit that moves no id and no count, so it is invisible to every
// watermark and to the recomputed report, and ONLY the content digest can see it.
//
// It goes through a raw handle because no public writer can produce this state:
// that is exactly what makes it the right probe for a tamper-evidence surface.
func editVerifyRowInPlace(t *testing.T, dbPath, repo string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() { _ = raw.Close() }()
	res, err := raw.ExecContext(context.Background(),
		`UPDATE token_events SET billing_mode = 'subscription' WHERE repo = ? AND billing_mode <> 'subscription'`, repo)
	if err != nil {
		t.Fatalf("in-place edit: %v", err)
	}
	// The control that makes a later CHANGED attributable to THIS edit.
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("the in-place edit touched 0 rows in %s; the assertion it sets up would be vacuous", repo)
	}
}

// TestVerifyReport_ScopedManifestIsNotBlind is the R3 guard.
//
// 🔴 THE RE-RUN AND THE ATTRIBUTION CANONICALIZE DIFFERENTLY UNLESS SOMETHING
// MAKES THEM AGREE. /api/v1/scores puts ?repo= through repoid.Canonical
// (lowercase, strip .git, to a fixed point); the attribution SQL binds the
// manifest's string into `repo = ?`. So a manifest scoped to "Acme/Tier" scores
// acme/tier and, unfixed, attributes against "Acme/Tier" — matching ZERO rows,
// and printing seven confident UNCHANGED lines plus a fabricated UNATTRIBUTED
// finding over a window nothing looked at.
//
// Every other fixture in this file is fleet-wide, so nothing else can see it.
func TestVerifyReport_ScopedManifestIsNotBlind(t *testing.T) {
	// 🔑 THE REQUEST CARRIES A NON-CANONICAL SPELLING, so the emitter's own
	// canonicalization is under test rather than assumed. Feeding it "acme/tier"
	// and then asserting it echoed "acme/tier" would hold on an emitter that does
	// not canonicalize at all — the assertion below only means something because
	// the input needed normalizing to satisfy it.
	f := serveFixtureManifest(t, seedVerifyDB(t),
		"since="+verifyWindowSince+"&until="+verifyWindowUntil+"&repo=Acme/Tier.git")

	m := readFixtureManifest(t, f.manifestPath)
	if m.Repo != "acme/tier" {
		t.Fatalf("served repo = %q, want acme/tier — the emitter echoed the caller's spelling instead of the "+
			"canonical slug it actually scoped the read to", m.Repo)
	}
	// 🔴 A SCOPED MANIFEST NOW CARRIES A SCOPED DIGEST (#747), and both halves of
	// that are asserted: the digest is PRESENT, and no omission is declared beside
	// it. Until #747 the digest took no repo predicate, so the emitter withheld it
	// rather than attest a SUPERSET of the rows this report read; that withhold is
	// gone, and a build that reinstated it would fail here rather than quietly
	// downgrade every scoped manifest to watermark-only verification.
	if m.EventsDigest == nil || m.EventsDigest.Value == "" {
		t.Fatalf("the served SCOPED manifest carries no events_digest (%+v); #747 scoped the digest so it could be published here", m.EventsDigest)
	}
	// 🔴 THE DENOMINATOR. Three of the fixture's four token events belong to
	// acme/tier; the fourth is bob's pre-window `other/repo` row, which is outside
	// both the scope AND the window. A digest over ZERO rows is a well-formed value
	// over nothing — #718's failure — and would satisfy every arm below.
	if m.EventsDigest.Rows != 3 {
		t.Fatalf("events_digest.rows = %d on a manifest scoped to acme/tier, want 3 — a scope bound raw (or one "+
			"that matched nothing) publishes a confident digest over an empty set", m.EventsDigest.Rows)
	}
	if m.DigestsOmitted != "" {
		t.Errorf("the scoped manifest publishes its digests AND declares digests_omitted = %q; the two states are mutually exclusive", m.DigestsOmitted)
	}
	// The verifier RECOMPUTES it under the same scope and reports UNCHANGED. This
	// is the line that used to read NOT PINNED on an otherwise green run — the
	// absence of the strongest check being indistinguishable from that check
	// passing.
	if code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath); code != rcReproduced {
		t.Fatalf("exit = %d, want %d on a clean scoped re-run; stdout=%s stderr=%s", code, rcReproduced, out, errb)
	} else {
		assertDimStatus(t, out, "events_digest", "UNCHANGED")
		assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
	}

	// 🔴 THE ARM #747 EXISTS FOR, END TO END: ANOTHER REPOSITORY'S WRITE MUST NOT
	// MOVE THIS REPORT'S DIGEST. Team A ingests into other/repo, INSIDE this
	// report's window. Nothing about acme/tier has changed, so verify-report must
	// still say UNCHANGED — and it must say it because the digest genuinely did not
	// move, not because it declined to look. Before this change the fleet-wide
	// digest moved on exactly this write, which is why #740 published none at all.
	//
	// 🔴 THE CONTROL COMES FIRST, and it is what makes the UNCHANGED below mean
	// anything. "verify-report says UNCHANGED because the foreign write was
	// correctly ignored" and "verify-report says UNCHANGED because nobody wrote"
	// are the SAME READING otherwise. Measured: with insertVerifyEvent's body
	// gutted to a no-op, this whole test still reported ok before this assertion
	// existed. seedVerifyDB leaves exactly one other/repo row, so the insert must
	// take it to two.
	if n := insertVerifyEvent(t, f.dbPath, "other/repo", "bob", "issue-999",
		time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)); n != 2 {
		t.Fatalf("after the other/repo insert the repository holds %d token_events rows, want 2 — the write never "+
			"landed, so the UNCHANGED assertion below would pass over a database nobody touched", n)
	}
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("FALSE ALARM: a write to other/repo made an acme/tier-scoped report DIVERGE (exit %d, want %d). "+
			"stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}
	assertDimStatus(t, out, "events_digest", "UNCHANGED")

	// 🔴 AND THE ARM THAT STOPS THAT BEING VACUOUS: an in-place edit to a row
	// INSIDE the scope must still read CHANGED. A predicate that filtered
	// everything out would satisfy the arm above perfectly. billing_mode is chosen
	// because it is inside the digest, moves no id and no count, and is published
	// by nothing — so the ONLY line that can see it is this one.
	editVerifyRowInPlace(t, f.dbPath, "acme/tier")
	code, out, errb = runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("an in-place edit to an acme/tier row did not diverge (exit %d, want %d) — the scoped digest is "+
			"filtering out the very rows it attests, so the no-false-alarm result above was bought with silence. "+
			"stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "events_digest", "CHANGED")
	assertDimStatus(t, out, "token_events", "UNCHANGED")

	// Everything below would otherwise re-verify against a database that now
	// carries those two writes, so it takes its OWN fresh fixture — a clean
	// re-run has to start from a clean database or "still reproduces" means
	// nothing.
	f = serveFixtureManifest(t, seedVerifyDB(t),
		"since="+verifyWindowSince+"&until="+verifyWindowUntil+"&repo=Acme/Tier.git")
	m = readFixtureManifest(t, f.manifestPath)
	m.Repo = "Acme/Tier.git"
	noncanon := filepath.Join(filepath.Dir(f.dbPath), "scoped-noncanon.json")
	writeManifest(t, noncanon, m)

	// A clean re-run still reproduces: the scope resolves to the same rows.
	//
	// 🔴 AND THE DIGEST LINE IS THE STRICTEST WITNESS THAT IT DID (#747). The
	// recomputation binds the scope into `repo = ?`, so a raw "Acme/Tier.git"
	// would select ZERO rows, produce a well-formed digest over nothing, and print
	// `events_digest CHANGED` — a fabricated divergence, in the one tool whose
	// purpose is correct attribution. UNCHANGED here says the ONE canonicalization
	// in verifyAgainst reached the digest read as well as the re-run.
	if code, stdout, stderr := runVerify(t, noncanon, "--db", f.dbPath); code != rcReproduced {
		t.Fatalf("exit = %d, want %d on a clean scoped re-run; stdout=%s stderr=%s", code, rcReproduced, stdout, stderr)
	} else {
		assertDimStatus(t, stdout, "events_digest", "UNCHANGED")
		assertDimDetail(t, stdout, "events_digest", "over 3 row(s)")
	}

	// And the attribution SEES the window: inject in-scope, in-window rows.
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "alice", IssueID: "issue-100", Repo: "acme/tier",
		Model: "claude-sonnet-4", InputTok: 60_000,
		CostMicro: store.DollarsToMicro(1.50),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		Timestamp: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		_ = db.Close()
		t.Fatalf("inject: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	code, stdout, stderr := runVerify(t, noncanon, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, stdout, stderr)
	}
	assertDimStatus(t, stdout, "token_events", "CHANGED")
	if strings.Contains(stdout, "UNATTRIBUTED") {
		t.Errorf("a scoped manifest produced a fabricated UNATTRIBUTED finding — the attribution queries "+
			"are not seeing the same rows the re-run scored:\n%s", stdout)
	}
}

// TestVerifyReport_UncanonicalScopeIsCouldNotCheck pins the other half: a scope
// that cannot be resolved at all is rc 2, NEVER a silent fall-back to fleet-wide.
// An unscoped figure presented as a scoped one is #590 wearing a filter.
func TestVerifyReport_UncanonicalScopeIsCouldNotCheck(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	m.Repo = "not a repo slug at all!!"
	path := filepath.Join(filepath.Dir(f.manifestPath), "bad-scope.json")
	writeManifest(t, path, m)

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d (could not check); stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	if !strings.Contains(errb, "not a canonical owner/repo") {
		t.Errorf("stderr = %q, want the uncanonical-scope refusal", errb)
	}
}

// TestVerifyReport_DeletionIsSeen is the arm the row counts exist for.
//
// 🔴 A DELETION MOVES EVERY SIGNAL EXCEPT THE OBVIOUS ONES. MAX(id) is invariant
// under deleting a non-maximal row, and an erasure writes to none of the four
// audit ledgers — so before the row counts were pinned, a GDPR Art. 17
// EraseDeveloper over the window returned rc 0 "REPRODUCED: every pinned input
// is unchanged" against a database that had lost rows.
func TestVerifyReport_DeletionIsSeen(t *testing.T) {
	t.Run("a non-maximal in-window row deleted", func(t *testing.T) {
		f := newVerifyFixture(t)
		// Delete an in-window, NON-maximal token event, so MAX(id) genuinely does
		// not move and the arm tests what it claims to.
		id := tokenEventIDIn(t, f.dbPath, true)
		execOnFixture(t, f.dbPath, "DELETE FROM token_events WHERE id = ?", id)

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "token_events", "CHANGED")
		assertDimDetail(t, out, "token_events", "REMOVED")
		// The watermark itself did NOT move — which is the whole point.
		assertDimDetailLacks(t, out, "token_events", "arrived after watermark")
	})

	// Pins the `- count` in the survivors arithmetic (#1032): a late in-window
	// row must not stand in for a deleted pinned one. Counting every in-window
	// row as a survivor reads the pin back exactly and reports no removal.
	t.Run("a deletion offset by a late in-window row", func(t *testing.T) {
		f := newVerifyFixture(t)
		id := tokenEventIDIn(t, f.dbPath, true)
		execOnFixture(t, f.dbPath, "DELETE FROM token_events WHERE id = ?", id)
		db, err := store.Open(f.dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{
			Developer: "alice", IssueID: "issue-100", Repo: "acme/tier",
			Model: "claude-sonnet-4", InputTok: 40_000,
			CostMicro: store.DollarsToMicro(1.00),
			Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
			IdempotencyKey: "late-offsets-deletion",
			Timestamp:      time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC),
		}); err != nil {
			_ = db.Close()
			t.Fatalf("insert late event: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "token_events", "CHANGED")
		assertDimDetail(t, out, "token_events", "+1 rows arrived after watermark")
		assertDimDetail(t, out, "token_events", "and 1 of the")
	})

	// Pins the printed survivor count against the pin: a pre-watermark row moved
	// INTO the counted population makes the two differ, so a detail that prints
	// the pin where it claims the survivor count fails here. The status word is
	// deliberately not asserted (#1034 item 16).
	t.Run("a pre-watermark row moved into the counted population", func(t *testing.T) {
		f := newVerifyFixture(t)
		pinned := readFixtureManifest(t, f.manifestPath).window().TokenEventCount
		if pinned == nil {
			t.Fatal("fixture manifest pins no token_event_count; the arm below would be vacuous")
		}
		id := tokenEventIDIn(t, f.dbPath, false)
		execOnFixture(t, f.dbPath, "UPDATE token_events SET ts = ? WHERE id = ?",
			time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC), id)

		_, out, _ := runVerify(t, f.manifestPath, "--db", f.dbPath)
		assertDimDetail(t, out, "token_events", fmt.Sprintf("holds %d rows", *pinned+1))
		assertDimDetail(t, out, "token_events", fmt.Sprintf("%d pinned", *pinned))
	})
}

// TestVerifyReport_WrongDatabaseIsCouldNotCheck is the RED-3 guard.
//
// os.Stat proves a file exists, nothing more. Every attribution query asks "what
// arrived AFTER id N", and against an unrelated or empty tier database the
// honest answer to all seven is "nothing" — so a manifest with no results block
// earned rc 0 "REPRODUCED: every pinned input is unchanged" over a database that
// never held the rows it describes.
func TestVerifyReport_WrongDatabaseIsCouldNotCheck(t *testing.T) {
	f := newVerifyFixture(t)
	// A results-less manifest is the dangerous shape: with results pinned the
	// comparison would catch it anyway. This is the bare #715 manifest.
	m := readFixtureManifest(t, f.manifestPath)
	m.Results = nil
	path := filepath.Join(filepath.Dir(f.manifestPath), "no-results.json")
	writeManifest(t, path, m)

	empty := filepath.Join(t.TempDir(), "empty.db")
	db, err := store.Open(empty)
	if err != nil {
		t.Fatalf("create empty db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	code, out, errb := runVerify(t, path, "--db", empty)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d (could not check); stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	if !strings.Contains(errb, "cannot be the one this manifest describes") {
		t.Errorf("stderr = %q, want the wrong-database refusal", errb)
	}
	if strings.Contains(out, "REPRODUCED") {
		t.Errorf("verifying against the WRONG database printed REPRODUCED:\n%s", out)
	}
}

// TestVerifyReport_LedgerElsewhereIsNotADivergence is the RED-5 guard, and it is
// the difference between a usable tool and one that is permanently red.
//
// 🔴 The first draft counted audit-ledger ENTRIES GLOBALLY. Any `tierd reprice`,
// `repair-repo` or cost correction anywhere in the install — a different
// repository, a different year — flipped the dimension to CHANGED and made every
// manifest emitted before it diverge FOREVER, while printing a fleet-wide dollar
// figure beside a window that moved nothing.
//
// Both arms are required. The first proves an operation elsewhere does not
// diverge this report; the second proves the dimension has not simply been
// switched off.
func TestVerifyReport_LedgerElsewhereIsNotADivergence(t *testing.T) {
	t.Run("a reprice touching NO row in this window does not diverge", func(t *testing.T) {
		f := newVerifyFixture(t)
		// The fixture's pre-window row (see seedVerifyDB), repriced. Nothing new
		// is inserted: an insert would move the installation's cost horizon and
		// change the served body for a reason this arm is not about.
		outOfWindow := tokenEventIDIn(t, f.dbPath, false)
		execOnFixture(t, f.dbPath, `
			INSERT INTO reprice_audit (reprice_id, from_version, old_price_version, new_price_version,
			    row_count, old_cost_micro_sum, new_cost_micro_sum, price_effective_date, tool_version)
			VALUES ('run-elsewhere', 1, 8, 9, 1, 400000, 500000, '2026-08-01', 'test')`)
		execOnFixture(t, f.dbPath, `
			INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
			VALUES ('run-elsewhere', ?, 400000, 8, 'per_token')`, outOfWindow)

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcReproduced {
			t.Fatalf("exit = %d, want %d — a reprice that touched no row in this window is not a divergence "+
				"of this report; stdout=%s stderr=%s", code, rcReproduced, out, errb)
		}
		assertDimStatus(t, out, "reprice", "UNCHANGED")
		// But it is still DISCLOSED, as context. Silence would be its own defect.
		assertDimDetail(t, out, "reprice", "elsewhere, not this window")
	})

	t.Run("a reprice touching a row IN this window does diverge", func(t *testing.T) {
		f := newVerifyFixture(t)
		inWindow := tokenEventIDIn(t, f.dbPath, true)
		execOnFixture(t, f.dbPath, `
			INSERT INTO reprice_audit (reprice_id, from_version, old_price_version, new_price_version,
			    row_count, old_cost_micro_sum, new_cost_micro_sum, price_effective_date, tool_version)
			VALUES ('run-here', 1, 8, 9, 1, 2000000, 3000000, '2026-08-01', 'test')`)
		execOnFixture(t, f.dbPath, `
			INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
			VALUES ('run-here', ?, 2000000, 8, 'per_token')`, inWindow)

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "reprice", "CHANGED")
		assertDimDetail(t, out, "reprice", "touched 1 row(s) in this window")
		// 🔴 The absence half, and the reason this arm is paired with the one
		// above: nothing else moved.
		assertDimStatus(t, out, "token_events", "UNCHANGED")
		assertDimStatus(t, out, "price_table", "UNCHANGED")
		// The recomputed report is byte-for-byte what it was — cost_micro was not
		// actually rewritten by this synthetic ledger row — so the headline must
		// say exactly that rather than claiming no results were pinned.
		if !strings.Contains(out, "IDENTICAL to the") {
			t.Errorf("an input moved while the numbers held, and the headline did not say so:\n%s", out)
		}
	})
}
