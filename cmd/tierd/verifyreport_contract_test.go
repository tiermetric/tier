package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// writeBytes writes a manifest FILE verbatim, without a round trip through the
// Go type. It is what lets an arm verify the emitter's OWN bytes rather than this
// binary's re-encoding of them — the two agreeing is the property under test, so
// a helper that re-encoded would assume it.
func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// statFile is os.Stat, named so an arm asserting a file's ABSENCE reads as an
// assertion rather than as a stray syscall.
func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

// mustParseDay parses a fixture window bound the way the served endpoint does.
func mustParseDay(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// seedPreWindowEvents adds n token events well OUTSIDE the report window and
// outside its 14-day attribution band, returning their ids.
//
// They exist so a ledger arm can write several row-images without tripping
// `reprice_row_audit`'s UNIQUE(reprice_id, token_event_id) — one run may touch a
// row only once. ⚠️ Dated 2026-06-02 onward, AFTER seedVerifyDB's 2026-06-01 row,
// so the installation's COST HORIZON (#512) does not move: that figure is derived
// from the earliest event in the WHOLE database and appears in the served
// /scores body, so shifting it would change the response for a reason unrelated
// to whatever the arm is testing.
func seedPreWindowEvents(t *testing.T, dbPath string, n int) []int64 {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("pre-window-%d", i)
		if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{
			Developer: "bob", IssueID: "issue-999", Repo: "other/repo",
			Model: "claude-sonnet-4", InputTok: 1_000,
			CostMicro: store.DollarsToMicro(0.10),
			Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
			IdempotencyKey: key,
			Timestamp:      time.Date(2026, 6, 2+i, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("seed pre-window event %d: %v", i, err)
		}
		ids = append(ids, lastTokenEventID(t, dbPath))
	}
	return ids
}

// lastTokenEventID reads the highest token_events id over its own handle.
func lastTokenEventID(t *testing.T, dbPath string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	var id int64
	if err := db.QueryRowContext(t.Context(), "SELECT COALESCE(MAX(id), 0) FROM token_events").Scan(&id); err != nil {
		t.Fatalf("read max token_events id: %v", err)
	}
	return id
}

// The #741 guards: verify-report CONSUMES the #715 contract instead of
// re-implementing it.
//
// 🔴 THE MEASUREMENT THAT MOTIVATED ALL OF THIS. On d8beb84 the served
// `tiermanifest1` and this command's decode-side mirror of `tiermanifest1` were
// MUTUALLY UNREADABLE. Feeding the real handler's bytes to loadManifest returned
//
//	parse manifest …: json: cannot unmarshal string into Go struct field
//	reportManifest.aggregation of type main.manifestAggregation
//
// — rc 2, on the one document this command exists to consume. Nothing reported
// it, because the only producer the suite exercised was `verify-report --emit`,
// this file's own stopgap, which agreed with the mirror by construction. Two
// emitters of one contract had not merely risked drift; they had completed it.

// TestVerifyReport_ConsumesTheServedManifest is the end-to-end arm: bytes served
// by the real #715 emitter, verified by the real #718 verifier, with no
// translation between them.
//
// It is the guard that would have caught the divergence above, and it is
// deliberately separate from the fixture's own decode assertion so a failure
// names THIS as the property that broke.
func TestVerifyReport_ConsumesTheServedManifest(t *testing.T) {
	f := seedVerifyDB(t)
	// The manifest EXACTLY as the server publishes it — no results block, no
	// edits. That is the document an operator has in hand.
	manifest := filepath.Join(filepath.Dir(f.dbPath), "served-only.json")
	writeBytes(t, manifest, serveJSON(t, f.dbPath,
		"/api/v1/report_manifest?since="+verifyWindowSince+"&until="+verifyWindowUntil))

	code, out, errb := runVerify(t, manifest, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("verifying the SERVED manifest exited %d, want %d. The emitter and the verifier are two "+
			"encodings of one scheme tag again.\nstdout=%s\nstderr=%s", code, rcReproduced, out, errb)
	}
	// Every data dimension the served manifest pins must have been EXAMINED, not
	// merely tolerated. Without this the arm passes on a decoder that silently
	// dropped the whole watermark block.
	for _, dim := range []string{
		"token_events", "outcomes", "quality revisions",
		"reprice", "cost corrections", "repo repairs", "push reconciliations",
		"events_digest", "outcomes_digest",
	} {
		assertDimStatus(t, out, dim, "UNCHANGED")
	}
	// A bare served manifest carries no results, and the report must SAY so
	// rather than let "REPRODUCED" imply the numbers were compared.
	if !strings.Contains(out, "pinned NO results") {
		t.Errorf("the served manifest pins no results and the report did not disclose it:\n%s", out)
	}
}

// TestVerifyReport_OpenEndedWindowVerifies covers the window shape every other
// arm in this suite avoids, and it is the DEFAULT one the emitter produces.
//
// 🔴 AN OMITTED `until` IS A DIFFERENT REPORT, NOT A MISSING FIELD. The manifest
// leaves the key out entirely (an open-ended window re-run tomorrow covers more
// days), so this exercises the zero-Time path through resolveWindow, the
// attribution SQL — which must drop its `ts <` conjunct rather than bind a zero
// time — and store.ReportDigests' open-ended window. A zero time.Time bound into
// SQLite compares as "0001-01-01 00:00:00 +0000 UTC", which sorts BELOW every
// real row, so a conjunct left in by mistake matches NOTHING and every dimension
// reports a confident UNCHANGED over an empty read.
func TestVerifyReport_OpenEndedWindowVerifies(t *testing.T) {
	f := serveFixtureManifest(t, seedVerifyDB(t), "since="+verifyWindowSince)
	m := readFixtureManifest(t, f.manifestPath)
	if m.Until != "" {
		t.Fatalf("until = %q on an open-ended request; this arm is not testing the open-ended path", m.Until)
	}
	// The denominator: the open-ended read must actually have covered the rows.
	// Without this the arm passes on a window that matched nothing.
	if m.EventsDigest == nil || m.EventsDigest.Rows == 0 {
		t.Fatalf("events_digest = %+v over an open-ended window — a digest over ZERO rows would make every "+
			"assertion below vacuous", m.EventsDigest)
	}

	if code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath); code != rcReproduced {
		t.Fatalf("exit = %d, want %d on a clean open-ended re-run; stdout=%s stderr=%s", code, rcReproduced, out, errb)
	}

	// And it still DIVERGES on an in-window change: a clean pass over a window
	// that reads nothing looks identical to a clean pass over one that reads
	// everything, so the positive arm above needs this negative beside it.
	id := tokenEventIDIn(t, f.dbPath, true)
	execOnFixture(t, f.dbPath, "UPDATE token_events SET billing_mode = 'subscription' WHERE id = ? AND billing_mode <> 'subscription'", id)

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; an open-ended window that cannot see an in-window edit is reading nothing.\n"+
			"stdout=%s\nstderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "events_digest", "CHANGED")
}

// TestVerifyReport_DecodesTheAnonymizedServedManifest closes the decode gap the
// fleet-wide arms cannot reach.
//
// 🔴 AN ANONYMIZED MANIFEST IS A DIFFERENT DOCUMENT, not the same one with fewer
// rows. It carries `k` and `kanon_suppressed`, and it OMITS `watermarks.window`
// and both digests — the #593 withhold, because their row counts are unfloored
// counts of work over a caller-chosen window. Every other arm in this suite runs
// in developer mode, so without this one the four anonymized-only keys would go
// unexercised on the decode side: an undecoded key becomes an "unknown pin",
// which is a pin nobody examined sitting under a green tick.
//
// It asserts the DECODE and the three-valued reading, deliberately not a re-run:
// re-running a team report needs org_hierarchy and a cohort that clears the
// floor, which is /scores' territory and is covered there.
func TestVerifyReport_DecodesTheAnonymizedServedManifest(t *testing.T) {
	f := seedVerifyDB(t)
	manifest := filepath.Join(filepath.Dir(f.dbPath), "team.json")
	writeBytes(t, manifest, serveAnonymizedJSON(t, f.dbPath,
		"/api/v1/report_manifest?since="+verifyWindowSince+"&until="+verifyWindowUntil))

	m, err := loadManifest(manifest)
	if err != nil {
		t.Fatalf("the anonymized served manifest does not decode: %v", err)
	}
	if len(m.unknownFields) > 0 {
		t.Errorf("the anonymized served manifest carries %v as unknown pins — every one is a published field "+
			"that goes unexamined on a green run", m.unknownFields)
	}
	if m.Aggregation != "team" {
		t.Errorf("aggregation = %q, want team", m.Aggregation)
	}
	// 🔴 k IS A POINTER FOR A REASON: "no floor" and "a floor of zero" are
	// different states, and re-running a team report at k=0 would compute exactly
	// the cohort the floor exists to withhold.
	if m.K == nil || *m.K == 0 {
		t.Errorf("k = %v, want a real floor — an anonymized manifest that pins no k cannot be re-run safely", m.K)
	}
	if m.Watermarks == nil || m.Watermarks.Window != nil {
		t.Errorf("watermarks.window = %v: it must be WITHHELD in an anonymized mode (#593) and the ledgers half "+
			"must survive", m.window())
	}
	if m.Watermarks != nil && m.Watermarks.Ledgers == nil {
		t.Error("watermarks.ledgers was withheld too; it is install-wide and window-independent, so it carries " +
			"no count of the caller's chosen population and must stay")
	}
	if m.EventsDigest != nil || m.OutcomesDigest != nil {
		t.Errorf("the digests are published in team mode (%v / %v); their `rows` is the same unfloored count of "+
			"work watermarks.window is withheld for", m.EventsDigest, m.OutcomesDigest)
	}
	// And the absence is DECLARED, twice, by design: once as a reason a human
	// reads and once as a flag the k-anon record carries.
	if m.DigestsOmitted == "" {
		t.Error("digests_omitted is empty on a manifest that omitted them — an undeclared absence reads as " +
			"'this server publishes no digests', which is a different and false statement")
	}
	if m.KAnonSuppressed == nil || !m.KAnonSuppressed.WithheldDigests || !m.KAnonSuppressed.WithheldWindow {
		t.Errorf("kanon_suppressed = %+v, want both withhold flags set — the record has to be a COMPLETE "+
			"statement of what k-anonymity removed", m.KAnonSuppressed)
	}
}

// TestVerifyReport_WatermarksComeFromTheStoreContract pins that the manifest's
// watermarks ARE store.ReportWatermarks' output, and that the verifier's verdict
// moves when that output does.
//
// 🔴 A TEST THAT MERELY PASSES AFTER THE REFACTOR PROVES NOTHING — IT IS THE SAME
// TEST THAT PASSED BEFORE. So this one reads the store contract directly, requires
// the SERVED manifest to carry exactly those numbers, and then moves them.
//
// ⚠️ ITS UNIQUE CONTRIBUTION IS THE FIRST HALF, AND AN EARLIER VERSION OF THIS
// COMMENT OVERSTATED IT. It claimed "there is no way to satisfy it while carrying
// a private second reader", which is FALSE and was killed by measurement: a
// verifier that recomputes MAX(id) locally instead of using the manifest's pin is
// caught here AND by TestVerifyReport_FiveArms AND by the attribution-band arm.
// What nothing else asserts is the EMITTER-side equality below — that the
// manifest publishes the store contract's numbers rather than numbers of its own.
// The second half (move the contract, expect the verdict to follow) overlaps arm
// A and is kept because it states the end-to-end property in one place.
func TestVerifyReport_WatermarksComeFromTheStoreContract(t *testing.T) {
	f := seedVerifyDB(t)
	ctx := t.Context()

	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pinned, err := db.ReportWatermarks(ctx,
		mustParseDay(t, verifyWindowSince), mustParseDay(t, verifyWindowUntil), store.FleetWide)
	if err != nil {
		_ = db.Close()
		t.Fatalf("ReportWatermarks: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if pinned.Window.MaxTokenEventID == 0 || pinned.Window.TokenEventCount == 0 {
		t.Fatalf("the store contract reports %+v for the seeded window; every assertion below would be vacuous",
			pinned.Window)
	}

	// The manifest the SERVER publishes must carry exactly what the store
	// contract returns. This is the "one definition" half: two readers would show
	// up here as two numbers.
	f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil)
	m := readFixtureManifest(t, f.manifestPath)
	if got := m.window().MaxTokenEventID; got == nil || *got != pinned.Window.MaxTokenEventID {
		t.Fatalf("manifest max_token_event_id = %v, store.ReportWatermarks says %d — the emitter is not "+
			"publishing the store contract", got, pinned.Window.MaxTokenEventID)
	}
	if got := m.window().TokenEventCount; got == nil || *got != pinned.Window.TokenEventCount {
		t.Fatalf("manifest token_event_count = %v, store.ReportWatermarks says %d", got, pinned.Window.TokenEventCount)
	}

	// 🔴 NOW MOVE WHAT THE CONTRACT REPORTS AND REQUIRE THE VERDICT TO FOLLOW.
	// A new in-window token event raises MAX(id) and the count together, so the
	// verifier — reading the manifest's pin and asking "what arrived after it" —
	// must name token_events. A verifier still computing its own watermarks from
	// its own reader would compare like against like and report UNCHANGED.
	db, err = store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "alice", IssueID: "issue-100", Repo: "acme/tier",
		Model: "claude-sonnet-4", InputTok: 40_000,
		CostMicro: store.DollarsToMicro(1.00),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		IdempotencyKey: "contract-arm",
		Timestamp:      time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC),
	}); err != nil {
		_ = db.Close()
		t.Fatalf("insert: %v", err)
	}
	moved, err := db.ReportWatermarks(ctx,
		mustParseDay(t, verifyWindowSince), mustParseDay(t, verifyWindowUntil), store.FleetWide)
	if err != nil {
		_ = db.Close()
		t.Fatalf("ReportWatermarks: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The control that makes the assertion below mean something: the CONTRACT's
	// answer really did change.
	if moved.Window.MaxTokenEventID == pinned.Window.MaxTokenEventID {
		t.Fatalf("store.ReportWatermarks still reports max_token_event_id = %d after an insert; this arm is not "+
			"exercising a change in the contract at all", moved.Window.MaxTokenEventID)
	}

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d — store.ReportWatermarks moved and the verifier did not follow it, which "+
			"means it is reading a watermark of its own.\nstdout=%s\nstderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "token_events", "CHANGED")
	assertDimDetail(t, out, "token_events", "arrived after watermark")
}

// TestVerifyReport_TokenSideIsTheAttributionBandNotTheWindow is the correctness
// half of consuming the #715 contract, and it is a genuine bug fix rather than a
// tidy-up (#741).
//
// 🔴 store.WindowWatermarks watermarks token_events over [since -
// AttributableWindow, until), 14 days WIDER than the outcome side, because
// OutcomeTokenTotals funds an outcome from token events up to 14 days before
// `since`. Its doc block names the exact failure: "a late-ingested row 20 days
// before `since` can lift a (developer, issue) total past
// scoring.MinAttributableTokens, clear the tripwire, un-suppress a developer and
// change /scores — while a watermark bounded at `since` holds perfectly still".
// This command's own reader was bounded at `since`, so it had precisely that
// defect: the row below moved the published numbers and the token_events line
// said UNCHANGED, leaving the run in UNATTRIBUTED.
// It has TWO halves, and the FIRST is the one every fixture in this suite was
// blind to. `seedVerifyDB`'s only pre-window row is dated 2026-06-01 — 61 days
// before `since`, far outside the band — so in every other arm the band and the
// window select the SAME token_events and the two are indistinguishable. This
// arm seeds a row INSIDE the band BEFORE the manifest is served, which makes them
// differ, and then asserts a CLEAN run reproduces.
//
// 🔴 THAT CLEAN-RUN HALF IS WHAT CATCHES A VERIFIER BOUNDED AT `since`, AND THE
// FAILURE IT CATCHES IS A FALSE ALARM ON AN UNTOUCHED DATABASE. Measured with the
// verifier recomputing over [since, until) while the emitter covers the band:
//
//	events_digest:  CHANGED  "tierdig1:e2351006…" (4 rows) -> tierdig1:d6519360… (3 rows)
//
// Nothing had changed. That is "the one failure mode a tamper-evidence surface
// may not have" (internal/store/eventsdigest.go), reached on any install holding
// a token event in the 14 days before `since` — the common case, not a corner.
func TestVerifyReport_TokenSideIsTheAttributionBandNotTheWindow(t *testing.T) {
	base := seedVerifyDB(t)
	// BEFORE `since` (2026-08-01) and inside the 14-day band, seeded BEFORE the
	// manifest is served so the emitter's watermark, row count and digest all
	// cover it. It funds an in-window outcome's issue, so the report reads it.
	insertBandRow(t, base.dbPath, "band-row-pre", time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC))
	f := serveFixtureManifest(t, base, "since="+verifyWindowSince+"&until="+verifyWindowUntil)

	// --- half one: the recomputation must cover the SAME rows the emitter did ---
	m := readFixtureManifest(t, f.manifestPath)
	if got := m.window().TokenEventCount; got == nil || *got != 4 {
		t.Fatalf("token_event_count = %v, want 4 (three in-window rows plus the band row) — the fixture is not "+
			"making the band and the window differ, so this arm cannot discriminate", got)
	}
	if m.EventsDigest == nil || m.EventsDigest.Rows != 4 {
		t.Fatalf("events_digest = %+v, want 4 rows — same reason", m.EventsDigest)
	}
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("a CLEAN run over a database with a band row exited %d, want %d. The verifier is recomputing "+
			"over a different window than the emitter used, so it reports a divergence on data nobody touched — "+
			"a FALSE ALARM.\nstdout=%s\nstderr=%s", code, rcReproduced, out, errb)
	}
	assertDimStatus(t, out, "events_digest", "UNCHANGED")
	assertDimStatus(t, out, "token_events", "UNCHANGED")

	// --- half two: and a LATE band row must still be SEEN ---
	//
	// 🔴 Bounded at `since`, this dimension read UNCHANGED and the report fell
	// through to UNATTRIBUTED — telling an operator to go hunting through
	// developer_alias and org_hierarchy for a plain late ingest.
	insertBandRow(t, f.dbPath, "band-row-late", time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC))

	code, out, errb = runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "token_events", "CHANGED")
	assertDimDetail(t, out, "token_events", "arrived after watermark")
	assertDimStatus(t, out, "events_digest", "CHANGED")
	if strings.Contains(out, "UNATTRIBUTED") {
		t.Errorf("a token event inside the attribution band produced a fabricated UNATTRIBUTED finding: the "+
			"token_events dimension is bounded at `since` and cannot see the rows the report reads:\n%s", out)
	}
}

// insertBandRow adds a token event BEFORE the fixture window's `since` but inside
// the 14-day attribution band, funding an in-window outcome's issue — a row the
// report genuinely reads and a naive `[since, until)` bound cannot see.
func insertBandRow(t *testing.T, dbPath, key string, at time.Time) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{
		Developer: "alice", IssueID: "issue-100", Repo: "acme/tier",
		Model: "claude-sonnet-4", InputTok: 80_000,
		CostMicro: store.DollarsToMicro(2.50),
		Source:    "jsonl", Fidelity: "realtime", PriceVersion: 9,
		IdempotencyKey: key, Timestamp: at,
	}); err != nil {
		t.Fatalf("insert band row %s: %v", key, err)
	}
}

// TestVerifyReport_LedgerWatermarkIsTheRowLedgerNotTheAggregate is the arm that
// makes the ROW-vs-AGGREGATE distinction observable (#741).
//
// 🔴 THEY ARE INDEPENDENT AUTOINCREMENT SEQUENCES WEARING THE SAME NAME.
// `reprice_audit` gets ONE row per reprice RUN; `reprice_row_audit` gets one per
// MUTATED ROW. #715 watermarks the row ledger. This command used to bound its
// query on the AGGREGATE table's id while consuming a pin taken from the row
// ledger — two unrelated numbers compared as if commensurable.
//
// The fixture makes the two sequences diverge in the direction that HIDES a
// change rather than inventing one, which is the dangerous direction: one run
// touching five rows leaves reprice_audit at id 1 and reprice_row_audit at id 5.
// A later run that repriced an in-window row writes reprice_audit id 2 and
// reprice_row_audit id 6. Reading the row pin (5) against the aggregate table
// asks for `reprice_audit.id > 5` — which matches NOTHING — so the dimension
// reports UNCHANGED over a reprice that rewrote this window's money.
func TestVerifyReport_LedgerWatermarkIsTheRowLedgerNotTheAggregate(t *testing.T) {
	f := seedVerifyDB(t)
	inWindow := tokenEventIDIn(t, f.dbPath, true)
	// FIVE distinct out-of-window rows: reprice_row_audit is UNIQUE on
	// (reprice_id, token_event_id), so one run touches a row at most once.
	outOfWindow := seedPreWindowEvents(t, f.dbPath, 5)

	// ONE historical run, FIVE row images: this is what drives the two sequences
	// apart. Every image points at an out-of-window row, so the run itself is not
	// a divergence of this report.
	execOnFixture(t, f.dbPath, `
		INSERT INTO reprice_audit (reprice_id, from_version, old_price_version, new_price_version,
		    row_count, old_cost_micro_sum, new_cost_micro_sum, price_effective_date, tool_version)
		VALUES ('run-history', 1, 7, 8, 5, 100000, 110000, '2026-07-01', 'test')`)
	for _, id := range outOfWindow {
		execOnFixture(t, f.dbPath, `
			INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
			VALUES ('run-history', ?, 100000, 7, 'per_token')`, id)
	}

	f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil)
	m := readFixtureManifest(t, f.manifestPath)
	pin := m.ledgers().MaxRepriceRowAuditID
	if pin == nil || *pin != 5 {
		t.Fatalf("manifest max_reprice_row_audit_id = %v, want 5 — the fixture is not making the two id "+
			"sequences diverge, so this arm cannot tell the two tables apart", pin)
	}

	// A NEW run that reprices an IN-WINDOW row. reprice_audit reaches id 2 — below
	// the row pin of 5 — while reprice_row_audit reaches 6, above it.
	execOnFixture(t, f.dbPath, `
		INSERT INTO reprice_audit (reprice_id, from_version, old_price_version, new_price_version,
		    row_count, old_cost_micro_sum, new_cost_micro_sum, price_effective_date, tool_version)
		VALUES ('run-now', 1, 8, 9, 1, 2000000, 3000000, '2026-08-01', 'test')`)
	execOnFixture(t, f.dbPath, `
		INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
		VALUES ('run-now', ?, 2000000, 8, 'per_token')`, inWindow)

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d — a reprice that rewrote an in-window row went unreported, which is what "+
			"happens when the row-ledger pin is compared against the AGGREGATE ledger's id sequence.\n"+
			"stdout=%s\nstderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "reprice", "CHANGED")
	assertDimDetail(t, out, "reprice", "touched 1 row(s) in this window")
	// The absence half: the historical run touched no row this report scores.
	assertDimStatus(t, out, "token_events", "UNCHANGED")
	assertDimStatus(t, out, "cost corrections", "UNCHANGED")
	assertDimStatus(t, out, "repo repairs", "UNCHANGED")
}

// TestVerifyReport_LedgerRowsRemovedIsSeen pins the four COUNT pins the served
// manifest publishes alongside the four ledger MAX(id)s.
//
// 🔴 ONLY THE COUNT CAN SEE A REMOVAL. MAX(id) is invariant under deleting any
// non-maximal row, so without this the four counts would be decoded and never
// examined — a pin under a green tick, which is the defect class both #740 and
// #741 are about. The event is reachable and legitimate: EraseDeveloper (GDPR
// Art. 17) hard-deletes from these tables. It is still a change to the very
// evidence this report's provenance rests on, so it is reported rather than
// swallowed.
func TestVerifyReport_LedgerRowsRemovedIsSeen(t *testing.T) {
	f := seedVerifyDB(t)
	outOfWindow := seedPreWindowEvents(t, f.dbPath, 2)
	execOnFixture(t, f.dbPath, `
		INSERT INTO reprice_audit (reprice_id, from_version, old_price_version, new_price_version,
		    row_count, old_cost_micro_sum, new_cost_micro_sum, price_effective_date, tool_version)
		VALUES ('run-history', 1, 7, 8, 2, 100000, 110000, '2026-07-01', 'test')`)
	for _, id := range outOfWindow {
		execOnFixture(t, f.dbPath, `
			INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
			VALUES ('run-history', ?, 100000, 7, 'per_token')`, id)
	}

	f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil)
	m := readFixtureManifest(t, f.manifestPath)
	if got := m.ledgers().RepriceRowAuditCount; got == nil || *got != 2 {
		t.Fatalf("manifest reprice_row_audit_count = %v, want 2 — the count pin is not on the wire, so the "+
			"assertion below would be vacuous", got)
	}

	// Delete the NON-maximal row, so MAX(id) genuinely does not move and the arm
	// tests what it claims to.
	execOnFixture(t, f.dbPath, "DELETE FROM reprice_row_audit WHERE id = (SELECT MIN(id) FROM reprice_row_audit)")

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d — an append-only audit ledger lost a row and nothing said so.\n"+
			"stdout=%s\nstderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "reprice", "CHANGED")
	assertDimDetail(t, out, "reprice", "are GONE")
	// The watermark itself did NOT move — which is the whole point.
	assertDimDetailLacks(t, out, "reprice", "touched")
}

// TestVerifyReport_LedgerRemovalIsNotMaskedByANewRow pins that a removed pinned
// ledger row is seen even when an unrelated row is added after the manifest
// (#1032).
//
// 🔴 The removal check used to count the WHOLE table, so deleting one
// non-maximal pinned row and inserting one row that touches no row in this
// window left COUNT(*) equal to the pin, MAX(id) above it and the scoped query
// empty: every reading UNCHANGED and rc 0 over lost provenance evidence. The
// check must count only rows at or below the pinned watermark.
//
// Each arm deletes ONE pinned row and inserts TWO, so the removal count is
// exactly 1 only under `id <= watermark`: `id > ?` (2 survivors), `id >= ?` (3),
// `id < ?` (0, the deleted row was the minimum) and the whole table (3) all
// report a different number. With one insert, `id > ?` also read 1.
func TestVerifyReport_LedgerRemovalIsNotMaskedByANewRow(t *testing.T) {
	t.Run("scoped ledger: reprice", func(t *testing.T) {
		f := seedVerifyDB(t)
		outOfWindow := seedPreWindowEvents(t, f.dbPath, 4)
		for _, id := range outOfWindow[:2] {
			execOnFixture(t, f.dbPath, `
				INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
				VALUES ('run-history', ?, 100000, 7, 'per_token')`, id)
		}

		f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil)
		m := readFixtureManifest(t, f.manifestPath)
		led := m.ledgers()
		if led.RepriceRowAuditCount == nil || *led.RepriceRowAuditCount != 2 ||
			led.MaxRepriceRowAuditID == nil {
			t.Fatalf("manifest reprice_row_audit pins = (count %v, max %v), want count 2 and a max — the "+
				"arm below would be vacuous", led.RepriceRowAuditCount, led.MaxRepriceRowAuditID)
		}

		execOnFixture(t, f.dbPath, "DELETE FROM reprice_row_audit WHERE id = (SELECT MIN(id) FROM reprice_row_audit)")
		// Operations on OUT-OF-WINDOW rows: they touch nothing this report scores.
		for _, id := range outOfWindow[2:] {
			execOnFixture(t, f.dbPath, `
				INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
				VALUES ('run-later', ?, 100000, 7, 'per_token')`, id)
		}
		assertWholeTableCountMasksRemoval(t, f.dbPath, "reprice_row_audit",
			*led.RepriceRowAuditCount, *led.MaxRepriceRowAuditID)

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d — a pinned audit row was deleted and a new row elsewhere hid it.\n"+
				"stdout=%s\nstderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "reprice", "CHANGED")
		assertDimDetail(t, out, "reprice", "1 of the 2 pinned reprice_row_audit row(s) are GONE")
		// The new row touched no in-window row, so it is context, never an attribution.
		assertDimDetailLacks(t, out, "reprice", "touched")
		assertDimDetail(t, out, "reprice", "elsewhere")
	})

	t.Run("quality_history", func(t *testing.T) {
		f := seedVerifyDB(t)
		// outcome_id 0 names no outcome, so none of these rows joins to this
		// report's window: only the removal check can see them.
		for i := 0; i < 2; i++ {
			execOnFixture(t, f.dbPath, `
				INSERT INTO quality_history (outcome_id, developer, issue_id, old_quality, new_quality, reason)
				VALUES (0, 'bob', 'issue-999', 1.0, 0.5, 'revert_quality')`)
		}

		f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil)
		m := readFixtureManifest(t, f.manifestPath)
		led := m.ledgers()
		if led.QualityHistoryCount == nil || *led.QualityHistoryCount != 2 ||
			led.MaxQualityHistoryID == nil {
			t.Fatalf("manifest quality_history pins = (count %v, max %v), want count 2 and a max — the "+
				"arm below would be vacuous", led.QualityHistoryCount, led.MaxQualityHistoryID)
		}

		execOnFixture(t, f.dbPath, "DELETE FROM quality_history WHERE id = (SELECT MIN(id) FROM quality_history)")
		for i := 0; i < 2; i++ {
			execOnFixture(t, f.dbPath, `
				INSERT INTO quality_history (outcome_id, developer, issue_id, old_quality, new_quality, reason)
				VALUES (0, 'bob', 'issue-999', 0.5, 1.0, 'reopen')`)
		}
		assertWholeTableCountMasksRemoval(t, f.dbPath, "quality_history",
			*led.QualityHistoryCount, *led.MaxQualityHistoryID)

		code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
		if code != rcDiverged {
			t.Fatalf("exit = %d, want %d — a pinned quality_history row was deleted and a new row hid it.\n"+
				"stdout=%s\nstderr=%s", code, rcDiverged, out, errb)
		}
		assertDimStatus(t, out, "quality revisions", "CHANGED")
		assertDimDetail(t, out, "quality revisions", "1 of the 2 pinned quality_history row(s) are GONE")
		assertDimDetailLacks(t, out, "quality revisions", "new revision")
	})
}

// assertWholeTableCountMasksRemoval proves the fixture is in the state #1032
// describes: the whole-table count is still at least the pin and MAX(id) has
// moved above the watermark, so a whole-table count cannot see the removal.
func assertWholeTableCountMasksRemoval(t *testing.T, dbPath, table string, pinnedCount, pinnedMax int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count, maxID int64
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*), COALESCE(MAX(id), 0) FROM "+table).Scan(&count, &maxID); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	if count < pinnedCount || maxID <= pinnedMax {
		t.Fatalf("%s holds count %d, max id %d; want count >= %d (the pin) and max id above %d — the fixture "+
			"does not reproduce the masking state", table, count, maxID, pinnedCount, pinnedMax)
	}
}

// TestVerifyReport_DisagreeingAttributionBandIsNotCheckable pins the arm that
// turns a WRONG ANSWER into a stated non-answer.
//
// 🔴 token_since IS A PIN, AND IT WAS DECODED AND NEVER COMPARED. It is the
// emitter's statement of the lower bound it watermarked and digested
// token_events over. THREE dimensions here RE-DERIVE that bound as `since -
// store.AttributableWindow`: token_events, events_digest and — since #751 —
// repo_scope_excluded, whose token leg store.UnqualifiedExclusionWindow widens by
// the same constant. If the emitting build used a different band, the
// recomputation covers a different ROW SET than the emitter did, and the
// difference lands as `token_events CHANGED` / `events_digest CHANGED` /
// `repo_scope_excluded CHANGED`: a confident misattribution in the one tool whose
// whole purpose is correct attribution.
//
// ⚠️ THIS COMMENT SAID "TWO" AFTER #751 MADE IT THREE, and so did the printed
// detail — caught by review, not by a test, because nothing asserted the
// ENUMERATION. The last arm below now does, which is why the disclosure cannot
// drift away from the dimensions it describes again.
//
// ⛔ THE MANIFEST'S VALUE IS COMPARED, NEVER ADOPTED. Binding it would let a
// manifest choose the band it is audited against, and a narrower one hides
// exactly the late row the band exists to catch.
func TestVerifyReport_DisagreeingAttributionBandIsNotCheckable(t *testing.T) {
	f := newVerifyFixture(t)

	// 🔴 THE CONTROL COMES FIRST: on an ordinary run the bands AGREE and the line
	// must not appear at all. Without this, an implementation that printed the
	// warning unconditionally would pass the assertion below while making it noise.
	_, clean, _ := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if strings.Contains(clean, "attribution band") {
		t.Errorf("a manifest whose token_since agrees with this binary still printed an attribution-band "+
			"line:\n%s", clean)
	}

	m := readFixtureManifest(t, f.manifestPath)
	if m.TokenSince == "" {
		t.Fatal("the served manifest pins no token_since; this arm would be vacuous")
	}
	// A band of 21 days where this binary derives 14 — the shape a differently
	// configured emitting build produces.
	m.TokenSince = mustParseDay(t, verifyWindowSince).AddDate(0, 0, -21).Format(time.RFC3339)
	path := filepath.Join(filepath.Dir(f.manifestPath), "other-band.json")
	writeManifest(t, path, m)

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	// NOT a divergence: we have no reading either way, and reporting one would be
	// the fabrication this arm exists to prevent. Nor a reproduction (#1033): the
	// withheld pins are the ones that could have seen a token-side change.
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d — a band this binary cannot reproduce is a COULD-NOT-CHECK on the affected "+
			"dimensions, never a divergence and never a reproduction; stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	assertDimStatus(t, out, "attribution band", "NOT CHECKED")
	assertDimDetail(t, out, "attribution band", "NOT commensurable")
	// 🔴 THE DISCLOSURE MUST NAME EVERY PIN THE BAND FEEDS, AND THIS ARM IS WHY.
	// The detail used to enumerate three artifacts and close with "Those three".
	// #751 added a FOURTH pin over the same widened band (repo_scope_excluded's
	// token leg) and nobody re-read the sentence — so an operator was told that
	// exactly three named things were incommensurable while a fourth sat beside
	// them reporting a confident CHANGED. Assert the NAMES, never a count: a count
	// is falsified by the next addition and says nothing about which pins it means.
	for _, pin := range []string{"token_events", "events_digest", "repo_scope_excluded"} {
		assertDimDetail(t, out, "attribution band", pin)
	}
	if !strings.Contains(out, "NOT CHECKED: attribution band") {
		t.Errorf("the LIMITS block does not disclose the band disagreement:\n%s", out)
	}

	// The THIRD outcome: a token_since this binary cannot parse at all. It must be
	// NOT CHECKED for a DIFFERENT stated reason, not silently treated as absent —
	// an unparseable pin is still a pin, and "I could not read it" is a fact an
	// operator has to be told.
	m.TokenSince = "not-a-date-at-all"
	bad := filepath.Join(filepath.Dir(f.manifestPath), "unparseable-band.json")
	writeManifest(t, bad, m)
	code, out, errb = runVerify(t, bad, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	assertDimStatus(t, out, "attribution band", "NOT CHECKED")
	assertDimDetail(t, out, "attribution band", "cannot parse")
	// An unknown band withholds the band-dependent pins exactly as a different one
	// does: comparing them would be comparing against a band nobody can name.
	for _, dim := range []string{"token_events", "events_digest"} {
		assertDimStatus(t, out, dim, "NOT CHECKED")
		assertDimDetail(t, out, dim, "attribution band")
	}

	// A valid RFC3339 instant that is not midnight is a DIFFERENT band, not an
	// unreadable one: the /scores midnight rule is about re-running a window, and
	// token_since is never re-run.
	m.TokenSince = mustParseDay(t, verifyWindowSince).AddDate(0, 0, -14).Add(time.Second).Format(time.RFC3339)
	odd := filepath.Join(filepath.Dir(f.manifestPath), "non-midnight-band.json")
	writeManifest(t, odd, m)
	code, out, errb = runVerify(t, odd, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	assertDimDetail(t, out, "attribution band", "DIFFERENT attribution band")
	assertDimDetailLacks(t, out, "attribution band", "cannot parse")
}

// TestVerifyReport_PinsFromADifferentBandAreNotADivergence pins #1033: the
// band-dependent pins of a manifest emitted over a DIFFERENT attribution band are
// NOT CHECKED, never compared — so an unchanged database cannot exit rcDiverged.
//
// The arm above rewrites only token_since over pins this binary computed, so its
// pins still match and it cannot see a comparison that should not have run. Here
// the pins really are the other band's: token rows sit in the 7 days between a
// 21-day band and this binary's 14-day one, and the token_events watermark/count,
// events_digest and repo_scope_excluded's token leg are recomputed over 21 days.
func TestVerifyReport_PinsFromADifferentBandAreNotADivergence(t *testing.T) {
	const query = "since=" + verifyWindowSince + "&until=" + verifyWindowUntil + "&repo=acme/tier"
	f := seedVerifyDBWithSentinel(t)
	// Inside a 21-day band, outside this binary's 14-day one (2026-07-18): a scoped
	// row and a repo-blind row, so all three band-dependent pins differ.
	gap := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	insertBandRow(t, f.dbPath, "wider-band-row", gap)
	insertSentinelTokenEvent(t, f.dbPath, gap)
	f = serveManifestNoResults(t, f, query)

	m := readFixtureManifest(t, f.manifestPath)
	since := mustParseDay(t, verifyWindowSince)
	until := mustParseDay(t, verifyWindowUntil)
	// The other build's band: its pins are what store computes for a `since` 7
	// days earlier, token side only — the outcome side keeps the real window.
	otherSince := since.AddDate(0, 0, -7)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	marks, err := db.ReportWatermarks(t.Context(), otherSince, until, store.RepoScope("acme/tier"))
	if err != nil {
		t.Fatalf("wider-band watermarks: %v", err)
	}
	events, _, err := db.ReportDigests(t.Context(), otherSince, until, store.RepoScope("acme/tier"))
	if err != nil {
		t.Fatalf("wider-band digest: %v", err)
	}
	ex, err := db.UnqualifiedExclusionWindow(t.Context(), otherSince, until)
	if err != nil {
		t.Fatalf("wider-band exclusion: %v", err)
	}
	_ = db.Close()

	// CONTROL: every band-dependent pin genuinely differs, or this arm is vacuous.
	if m.Watermarks == nil || m.Watermarks.Window == nil || m.EventsDigest == nil || m.RepoScopeExcluded == nil {
		t.Fatalf("the served scoped manifest lacks a band-dependent pin: %+v", m)
	}
	if *m.Watermarks.Window.TokenEventCount == marks.Window.TokenEventCount ||
		m.EventsDigest.Value == events.Value ||
		m.RepoScopeExcluded.TokenEvents == ex.TokenEvents || m.RepoScopeExcluded.CostMicro == ex.CostMicro {
		t.Fatalf("the wider band selects the same token rows as this binary's band — the fixture does not "+
			"separate them (count %d vs %d, digest %s vs %s, excluded %d/%d vs %d/%d)",
			*m.Watermarks.Window.TokenEventCount, marks.Window.TokenEventCount, m.EventsDigest.Value, events.Value,
			m.RepoScopeExcluded.TokenEvents, m.RepoScopeExcluded.CostMicro, ex.TokenEvents, ex.CostMicro)
	}

	m.TokenSince = since.AddDate(0, 0, -21).Format(time.RFC3339)
	m.Watermarks.Window.MaxTokenEventID = &marks.Window.MaxTokenEventID
	m.Watermarks.Window.TokenEventCount = &marks.Window.TokenEventCount
	m.EventsDigest = &manifestDigest{Value: events.Value, Rows: events.Rows}
	m.RepoScopeExcluded.TokenEvents = ex.TokenEvents
	m.RepoScopeExcluded.CostMicro = ex.CostMicro
	path := filepath.Join(filepath.Dir(f.manifestPath), "wider-band.json")
	writeManifest(t, path, m)

	// The database is untouched: nothing moved, so this must not diverge. The
	// outcome side and the ledgers are band-independent and still examine the
	// data, but the withheld token-side pins make it rc 2, never a reproduction.
	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d — pins computed over a different attribution band must be NOT CHECKED: "+
			"neither a divergence on an unchanged database nor a reproduction; stdout=%s stderr=%s",
			code, rcCannotCheck, out, errb)
	}
	for _, dim := range []string{"token_events", "events_digest", "repo_scope_excluded"} {
		assertDimStatus(t, out, dim, "NOT CHECKED")
		assertDimDetail(t, out, dim, "attribution band")
	}
	assertDimStatus(t, out, "attribution band", "NOT CHECKED")
	assertDimStatus(t, out, "outcomes", "UNCHANGED")
	assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")

	// With ONLY band-dependent data pins, nothing about the data was examined:
	// rc 2, never a reproduction over zero comparisons.
	tok := m
	tokWin := *m.Watermarks.Window
	tokWin.MaxOutcomeID, tokWin.OutcomeCount = nil, nil
	tok.Watermarks = &manifestWatermarks{Window: &tokWin}
	tok.OutcomesDigest = nil
	tokenOnly := filepath.Join(filepath.Dir(f.manifestPath), "wider-band-token-only.json")
	writeManifest(t, tokenOnly, tok)
	code, out, errb = runVerify(t, tokenOnly, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d — every data pin left was band-dependent and NOT CHECKED, so nothing was "+
			"verified; stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	// rc 2 is also the error path's code, so the report itself must say why.
	if !strings.Contains(out, "COULD NOT CHECK:") || strings.Contains(out, "pins nothing about the DATA") {
		t.Errorf("rc 2 must carry the COULD NOT CHECK headline, and not the nothing-pinned one — data pins "+
			"exist here:\n%s", out)
	}
	assertDimStatus(t, out, "token_events", "NOT CHECKED")
	assertDimStatus(t, out, "events_digest", "NOT CHECKED")

	// 🔴 A REAL TOKEN-SIDE CHANGE UNDER A BAND MISMATCH IS NOT A REPRODUCTION.
	// A late in-window token event moves exactly the pins the band withholds, so
	// nothing reads CHANGED — and rc 0 "every pinned input is unchanged" would be a
	// pass over the very pins that could have seen it. It is rc 2.
	insertBandRow(t, f.dbPath, "late-in-window-row", time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC))
	code, out, errb = runVerify(t, path, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d — a late in-window token event under a band mismatch was reported as "+
			"something other than a COULD-NOT-CHECK; stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
	}
	assertDimStatus(t, out, "token_events", "NOT CHECKED")
	assertDimStatus(t, out, "events_digest", "NOT CHECKED")

	// The band-independent dimensions are still COMPARED, not swept up with the
	// token side: a late in-window outcome, scoped and repo-blind, must still
	// diverge — on the outcomes line AND on repo_scope_excluded's outcome leg.
	insertSentinelOutcome(t, f.dbPath, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	odb, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := odb.InsertOutcome(t.Context(), store.Outcome{
		Developer: "alice", IssueID: "issue-103", Repo: "acme/tier",
		Weight: 3.0, Quality: 1.0, MergeCommitSHA: "sha-alice-issue-103-late",
		Source: "api-outcome", WorkType: store.WorkTypeFeature,
		Timestamp: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("insert late outcome: %v", err)
	}
	_ = odb.Close()
	code, out, errb = runVerify(t, path, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d — in-window outcomes moved and the band disagreement hid them; "+
			"stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "outcomes", "CHANGED")
	assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
	assertDimDetail(t, out, "repo_scope_excluded", "outcomes 1 -> 2")
	assertDimStatus(t, out, "token_events", "NOT CHECKED")
	assertDimStatus(t, out, "events_digest", "NOT CHECKED")

	// 🔴 A DETECTED CHANGE OUTRANKS A WITHHELD QUORUM. The token-only manifest has
	// no quorum comparison left, but its repo_scope_excluded outcome leg moved:
	// that is a refutation (rc 1), never "nothing was refuted" (rc 2).
	code, out, errb = runVerify(t, tokenOnly, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d — a pinned input read CHANGED with no quorum comparison left; "+
			"stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "repo_scope_excluded", "CHANGED")
	if strings.Contains(out, "COULD NOT CHECK") || strings.Contains(errb, "Refusing") {
		t.Errorf("a CHANGED pin was reported as a could-not-check:\nstdout=%s\nstderr=%s", out, errb)
	}
}

// A pinned count cannot be checked without its matching watermark.
func TestVerifyReport_PinnedCountWithNoWatermarkIsDisclosed(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	m.Results = nil
	w, l := m.Watermarks.Window, m.Watermarks.Ledgers
	for _, tc := range []struct {
		name             string
		watermark, count **int64
	}{
		{"token_events", &w.MaxTokenEventID, &w.TokenEventCount},
		{"outcomes", &w.MaxOutcomeID, &w.OutcomeCount},
		{"quality revisions", &l.MaxQualityHistoryID, &l.QualityHistoryCount},
		{"reprice", &l.MaxRepriceRowAuditID, &l.RepriceRowAuditCount},
		{"cost corrections", &l.MaxCostCorrectionAuditID, &l.CostCorrectionAuditCount},
		{"repo repairs", &l.MaxRepoRepairRowAuditID, &l.RepoRepairRowAuditCount},
		{"push reconciliations", &l.MaxPushOutcomeAuditID, &l.PushOutcomeAuditCount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wm, count := *tc.watermark, *tc.count
			defer func() { *tc.watermark, *tc.count = wm, count }()
			if wm == nil || count == nil {
				t.Fatal("fixture must pin both watermark and count")
			}
			*tc.watermark = nil
			writeManifest(t, f.manifestPath, m)
			code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcCannotCheck {
				t.Errorf("exit = %d, want %d; stdout=%s stderr=%s", code, rcCannotCheck, out, errb)
			}
			assertDimStatus(t, out, tc.name, "NOT CHECKED")
			assertDimDetail(t, out, tc.name, "was NOT examined")
			if !strings.Contains(errb, tc.name) || !strings.Contains(errb, "watermark") || strings.Contains(errb, "#1033") {
				t.Errorf("stderr must name the stranded count, not the band issue: %s", errb)
			}
			*tc.count = nil
			writeManifest(t, f.manifestPath, m)
			code, plain, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcReproduced {
				t.Errorf("unpinned count exit = %d, want 0; stdout=%s stderr=%s", code, plain, errb)
			}
			assertDimStatus(t, plain, tc.name, "NOT PINNED")
			assertDimDetailLacks(t, plain, tc.name, "was NOT examined")
			*tc.watermark = wm
			writeManifest(t, f.manifestPath, m)
			_, plain, _ = runVerify(t, f.manifestPath, "--db", f.dbPath)
			assertDimDetail(t, plain, tc.name, "no row count pinned, so a DELETION would be invisible")
		})
	}
}

func TestVerifyReport_DeletedTokenEventWithStrandedCountCannotReproduce(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	m.Watermarks.Window.MaxTokenEventID = nil
	m.EventsDigest, m.Results = nil, nil
	writeManifest(t, f.manifestPath, m)
	execOnFixture(t, f.dbPath, "DELETE FROM token_events WHERE id = ?", tokenEventIDIn(t, f.dbPath, true))

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcCannotCheck || strings.Contains(out, "REPRODUCED") {
		t.Errorf("deleted event with a stranded count: exit = %d, want 2; stdout=%s stderr=%s", code, out, errb)
	}
	assertDimStatus(t, out, "token_events", "NOT CHECKED")
	assertDimStatus(t, out, "outcomes", "UNCHANGED")
}

// TestVerifyReport_DigestValueMatchingWithADifferentRowCountIsNotCheckable pins
// that the digest's DENOMINATOR is checked, not merely reprinted.
//
// 🔴 `rows` IS PUBLISHED BECAUSE A DIGEST OVER AN EMPTY WINDOW IS A WELL-FORMED
// VALUE AND A USELESS CLAIM. Under the fixed-arity framing #716 uses, an equal
// value IMPLIES an equal row count — so a manifest whose value matches while its
// count does not is CONTRADICTING ITSELF, and reporting UNCHANGED for it would be
// agreement earned by ignoring half the pin.
func TestVerifyReport_DigestValueMatchingWithADifferentRowCountIsNotCheckable(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	if m.EventsDigest == nil || m.EventsDigest.Rows == 0 {
		t.Fatalf("the served manifest carries no usable events_digest (%+v)", m.EventsDigest)
	}
	// The VALUE is left untouched, so it still recomputes equal; only the
	// denominator is made to disagree.
	m.EventsDigest.Rows++
	path := filepath.Join(filepath.Dir(f.manifestPath), "bad-denominator.json")
	writeManifest(t, path, m)

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Errorf("exit = %d, want %d (a self-contradictory pinned digest prevents reproduction); stdout=%s stderr=%s",
			code, rcCannotCheck, out, errb)
	}
	if strings.Contains(out, "REPRODUCED") || !strings.Contains(out, "COULD NOT CHECK") {
		t.Errorf("a self-contradictory pinned digest must refuse reproduction:\n%s", out)
	}
	assertDimStatus(t, out, "token_events", "UNCHANGED")
	assertDimStatus(t, out, "outcomes", "UNCHANGED")
	assertDimStatus(t, out, "events_digest", "NOT CHECKED")
	assertDimDetail(t, out, "events_digest", "CONTRADICTS ITSELF")
	if !strings.Contains(errb, "events_digest") || !strings.Contains(errb, "CONTRADICTS ITSELF") || strings.Contains(errb, "#1033") {
		t.Errorf("stderr must name the contradictory digest, not the band issue: %s", errb)
	}
	// The control: the untouched sibling still reads UNCHANGED, so this arm is
	// about the denominator and not about digests generally.
	assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
}

// TestVerifyReport_StackedRepoRepairsDoNotPrintADoubleCountedFigure pins the
// money guard on the repair ledger.
//
// 🔴 TWO RUNS CAN TOUCH ONE ROW. repo_repair_row_audit is
// UNIQUE(repair_id, token_event_id), so ONE run touches a row once — but a repo
// repaired A->B and then B->C writes two before-images for the same token event.
// SUM(te.cost_micro) then counts that row's spend TWICE while
// COUNT(DISTINCT token_event_id) counts it once, and printing that as fact is "a
// number that is wrong in a direction nobody can see". The reprice ledger has
// carried this guard since #718; the repair ledger has the same shape and did not.
func TestVerifyReport_StackedRepoRepairsDoNotPrintADoubleCountedFigure(t *testing.T) {
	f := newVerifyFixture(t)
	inWindow := tokenEventIDIn(t, f.dbPath, true)

	for _, id := range []string{"repair-1", "repair-2"} {
		execOnFixture(t, f.dbPath, `
			INSERT INTO repo_repair_audit (repair_id, developer, from_repo, to_repo,
			    row_count, cost_micro_sum, tool_version)
			VALUES (?, 'alice', 'unqualified', 'acme/tier', 1, 3000000, 'test')`, id)
		execOnFixture(t, f.dbPath, `
			INSERT INTO repo_repair_row_audit (repair_id, token_event_id, old_repo, new_repo)
			VALUES (?, ?, 'unqualified', 'acme/tier')`, id, inWindow)
	}

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "repo repairs", "CHANGED")
	assertDimDetail(t, out, "repo repairs", "2 before-images across 1 rows")
	assertDimDetail(t, out, "repo repairs", "NOT summable")
	// 🔴 THE ASSERTION THAT MATTERS: no dollar figure. Its absence is the guard —
	// the `else` branch prints "(+X.XX USD ...)" and that is what must not appear.
	assertDimDetailLacks(t, out, "repo repairs", "USD")
}

// TestVerifyReport_EmitFlagIsAUsageErrorNotANoOp pins the deletion of the
// stopgap producer (#741).
//
// ⛔ A REMOVED FLAG THAT PARSES AND DOES NOTHING IS WORSE THAN ONE THAT EXISTS:
// `verify-report --emit m.json` returning 0 having written nothing is a
// "verification" that never ran, reported as a pass — the exact fail-open shape
// this command's three-valued exit discipline exists to refuse. rc 1, because a
// removed flag is a USAGE error; rc 2 is reserved for a claim about the DATA.
func TestVerifyReport_EmitFlagIsAUsageErrorNotANoOp(t *testing.T) {
	f := newVerifyFixture(t)
	out := filepath.Join(filepath.Dir(f.dbPath), "should-not-exist.json")

	for _, args := range [][]string{
		{"--emit", out, "--db", f.dbPath},
		{"-emit", out, "--db", f.dbPath},
		{"--emit=" + out, "--db", f.dbPath},
		// The four options that existed ONLY to configure --emit. Each has no
		// verify-time meaning, so each must be refused by name rather than look
		// like a flag that might still do something.
		{"--since", verifyWindowSince, "--db", f.dbPath},
		{"--until", verifyWindowUntil, "--db", f.dbPath},
		{"--repo", "acme/tier", "--db", f.dbPath},
		{"--aggregation", "team", "--db", f.dbPath},
		{"-k", "5", "--db", f.dbPath},
	} {
		t.Run(strings.Join(args[:1], " "), func(t *testing.T) {
			code, stdout, stderr := runVerify(t, args...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (usage error); stdout=%s stderr=%s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "was REMOVED (#741)") {
				t.Errorf("stderr does not name the removal: %q", stderr)
			}
			// It must point at the replacement. A removal that leaves an operator
			// with no way to obtain a manifest is a removal that gets reverted.
			if !strings.Contains(stderr, "/api/v1/report_manifest") {
				t.Errorf("stderr does not name the served emitter: %q", stderr)
			}
			if stdout != "" {
				t.Errorf("a refused invocation wrote to stdout: %q", stdout)
			}
		})
	}
	// 🔴 AND IT REALLY DID NOT WRITE. The whole hazard is a flag that parses and
	// silently does nothing; this is the assertion that the opposite — parsing and
	// silently DOING something — is not happening either.
	if _, err := statFile(out); err == nil {
		t.Errorf("--emit was refused and yet %s exists", out)
	}

	// 🔴 THE FALSE-POSITIVE CONTROL, and without it the arms above are satisfied
	// by a scan that refuses everything. The refusal reads RAW argv before
	// flag.Parse, so it must not mistake the VALUE of a surviving flag for a
	// removed flag name. A database file named "-k" is pathological and legal;
	// this run must reach the ordinary "no such file" refusal (rc 2, a claim about
	// the data) rather than the removed-flag one (rc 1, a usage error).
	code, _, stderr := runVerify(t, f.manifestPath, "--db", "-k")
	if code != rcCannotCheck {
		t.Errorf("`--db -k` exited %d, want %d: the value of a surviving flag was read as a removed flag name; "+
			"stderr=%q", code, rcCannotCheck, stderr)
	}
	if strings.Contains(stderr, "was REMOVED") {
		t.Errorf("`--db -k` was refused as a removed flag: %q", stderr)
	}
}

// TestVerifyReport_NonMidnightWindowIsCouldNotCheck pins the one manifest this
// build genuinely cannot re-run, and pins that it says so LOUDLY.
//
// 🔴 THE TWO SURFACES DISAGREE ON THE WINDOW GRAMMAR, AND NEITHER IS WRONG. The
// manifest publishes RFC3339 INSTANTS (internal/api explains why at length).
// /api/v1/scores accepts ONLY whole-day bounds. So a manifest whose `since` is
// not midnight UTC cannot be replayed through the serving path at all.
//
// ⚠️ THE PRODUCER OF SUCH A BOUND CHANGED WITH #746 AND THE ARM DID NOT. It used
// to be this server's own default (`?since=` omitted resolved to now-90d
// carrying a time of day); that bound is now snapped to midnight, and
// TestVerifyReport_DefaultWindowIsReplayable covers the default shape end to
// end. What this test pins is the REFUSAL, which stays reachable for every
// manifest this server did not emit — hand-written, or from a pre-#746 build.
//
// ⛔ THE TEMPTING FIX IS TO TRUNCATE, AND IT IS A FABRICATED VERDICT. Truncating
// re-runs a DIFFERENT window and compares its numbers against this manifest's;
// the answer is then wrong in whichever direction the rows happen to fall, and it
// is reported with full confidence. rc 2 — "I could not look" — is the honest
// answer, and closing the gap for real is a contract decision about the two
// grammars that belongs in its own issue.
func TestVerifyReport_NonMidnightWindowIsCouldNotCheck(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	m.Since = "2026-08-01T09:30:00Z"
	path := filepath.Join(filepath.Dir(f.manifestPath), "mid-day.json")
	writeManifest(t, path, m)

	code, out, errb := runVerify(t, path, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want %d (could not check) — a bound the serving path cannot accept must never be "+
			"truncated into one it can.\nstdout=%s\nstderr=%s", code, rcCannotCheck, out, errb)
	}
	if !strings.Contains(errb, "not midnight UTC") {
		t.Errorf("stderr does not name the reason: %q", errb)
	}
	if strings.Contains(out, "REPRODUCED") {
		t.Errorf("a rc=%d run printed REPRODUCED:\n%s", code, out)
	}
	// The CONTROL: the same manifest with a midnight bound verifies. Without it
	// this arm passes on a build that refuses every manifest.
	m.Since = "2026-08-01T00:00:00Z"
	ok := filepath.Join(filepath.Dir(f.manifestPath), "midnight.json")
	writeManifest(t, ok, m)
	if code, stdout, stderr := runVerify(t, ok, "--db", f.dbPath); code != rcReproduced {
		t.Fatalf("the midnight control exited %d, want %d; stdout=%s stderr=%s", code, rcReproduced, stdout, stderr)
	}
}
