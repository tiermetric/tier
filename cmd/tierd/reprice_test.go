package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// seedRepriceDB creates a DB at a temp path with one mispriced token event and
// returns its path. The stored cost_micro (999000) is deliberately wrong so a
// reprice has something to correct; version 3 is below the active table so the
// row is always in scope for --from-version 1.
func seedRepriceDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reprice.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer:    "alice",
		IssueID:      "issue-cli",
		Model:        "claude-sonnet-4",
		InputTok:     1000,
		CostMicro:    999_000,
		Source:       "jsonl",
		Fidelity:     "realtime",
		PriceVersion: 3,
		Timestamp:    time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return path
}

func storedCLICost(t *testing.T, path string) (cost int64, version int) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = db.Close() }()
	since := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	until := since.Add(48 * time.Hour)
	events, _, err := db.ListTokenEvents(context.Background(), since, until, store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	return events[0].CostMicro, events[0].PriceVersion
}

// TestRunRepriceCmd_RequiresFromVersion proves the CLI rejects a missing
// --from-version (the safe-by-default guard against a whole-table reprice).
func TestRunRepriceCmd_RequiresFromVersion(t *testing.T) {
	path := seedRepriceDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path}, &out, &errb); code != 1 {
		t.Errorf("exit code = %d, want 1 (missing --from-version)", code)
	}
	if !strings.Contains(errb.String(), "--from-version is required") {
		t.Errorf("stderr = %q, want it to mention --from-version is required", errb.String())
	}
	// Nothing mutated.
	if cost, ver := storedCLICost(t, path); cost != 999_000 || ver != 3 {
		t.Errorf("row mutated on a rejected command: cost=%d version=%d", cost, ver)
	}
}

// TestRunRepriceCmd_DryRunReportsButDoesNotMutate proves the default (no --commit)
// is a dry run: it prints what would change and writes nothing.
func TestRunRepriceCmd_DryRunReportsButDoesNotMutate(t *testing.T) {
	path := seedRepriceDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "DRY RUN") {
		t.Errorf("stdout = %q, want it to announce DRY RUN", s)
	}
	if !strings.Contains(s, "Re-run with --commit") {
		t.Errorf("stdout = %q, want the --commit hint", s)
	}
	if !strings.Contains(s, "alice") {
		t.Errorf("stdout = %q, want the affected developer listed", s)
	}
	// Nothing mutated.
	if cost, ver := storedCLICost(t, path); cost != 999_000 || ver != 3 {
		t.Errorf("dry run mutated the row: cost=%d version=%d, want 999000/3", cost, ver)
	}
}

// TestRunRepriceCmd_FloorTooHighReportsNoMatch proves a --from-version above every
// stored price_version reports "nothing examined (check --from-version)" rather than
// masquerading as a successful no-op — so a mis-typed floor is not read as success.
func TestRunRepriceCmd_FloorTooHighReportsNoMatch(t *testing.T) {
	path := seedRepriceDB(t) // one row at price_version 3
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "999"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "nothing examined") || !strings.Contains(s, "check --from-version") {
		t.Errorf("stdout = %q, want the no-match / check --from-version message", s)
	}
	if strings.Contains(s, "already priced correctly") {
		t.Errorf("stdout = %q, must NOT report the already-correct message when the floor matched no rows", s)
	}
}

// TestRunRepriceCmd_NonexistentDBRejected proves the CLI refuses a --db path that
// does not exist rather than creating an empty DB and reporting a misleading no-op.
func TestRunRepriceCmd_NonexistentDBRejected(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", missing, "--from-version", "1"}, &out, &errb); code != 1 {
		t.Errorf("exit code = %d, want 1 (nonexistent --db)", code)
	}
	if !strings.Contains(errb.String(), "existing database") {
		t.Errorf("stderr = %q, want an 'existing database' message", errb.String())
	}
}

// TestRunRepriceCmd_GuessedModelWarns proves the CLI surfaces a loud WARNING (naming
// the offending model) when a changed row is repriced at the self-hosted GUESS
// fallback. This is the DRY-RUN path — it reports and warns but is never gated.
func TestRunRepriceCmd_GuessedModelWarns(t *testing.T) {
	path := seedGuessDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "GUESS fallback") {
		t.Errorf("stdout = %q, want a GUESS-fallback warning", s)
	}
	if !strings.Contains(s, "totally-unknown-model-xyz") {
		t.Errorf("stdout = %q, want the warning to NAME the offending model", s)
	}
}

// seedGuessDB creates a DB with one mispriced event for a model NOT in the price
// table, so a reprice would price it at the self-hosted GUESS fallback.
func seedGuessDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guess.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "ken", IssueID: "issue-guess", Model: "totally-unknown-model-xyz",
		InputTok: 1000, CostMicro: 999_000, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: 3, Timestamp: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return path
}

// TestRunRepriceCmd_CommitBlocksGuessedWithoutAllowGuessed proves the CLI GUESS
// gate: --commit on a DB with a guessed row FAILS (exit 1), mentions --allow-guessed,
// and mutates nothing.
func TestRunRepriceCmd_CommitBlocksGuessedWithoutAllowGuessed(t *testing.T) {
	path := seedGuessDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1", "--commit"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1 (guessed commit without --allow-guessed); stderr=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "allow-guessed") {
		t.Errorf("stderr = %q, want it to mention --allow-guessed", errb.String())
	}
	if !strings.Contains(errb.String(), "totally-unknown-model-xyz") {
		t.Errorf("stderr = %q, want it to NAME the offending model", errb.String())
	}
	// Nothing mutated.
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()
	events, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 || events[0].CostMicro != 999_000 || events[0].PriceVersion != 3 {
		t.Errorf("row mutated by a blocked commit: %+v", events)
	}
}

// TestRunRepriceCmd_CommitAllowsGuessedWithFlag proves --allow-guessed lets the
// guessed commit through: it COMMITs and STILL prints the loud GUESS warning.
func TestRunRepriceCmd_CommitAllowsGuessedWithFlag(t *testing.T) {
	path := seedGuessDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1", "--commit", "--allow-guessed"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0 (guessed commit WITH --allow-guessed); stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "COMMITTED") {
		t.Errorf("stdout = %q, want COMMITTED", s)
	}
	if !strings.Contains(s, "GUESS fallback") {
		t.Errorf("stdout = %q, want the GUESS warning to still print", s)
	}
}

// TestRunRepriceCmd_CommitApplies proves --commit recomputes the cost, bumps the
// version to the active table, and reports the audit id.
func TestRunRepriceCmd_CommitApplies(t *testing.T) {
	path := seedRepriceDB(t)
	active := store.ActivePriceTableInfo().Version
	want := store.ComputeCost("claude-sonnet-4", store.CostUsage{Input: 1000})

	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1", "--commit"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "COMMITTED") {
		t.Errorf("stdout = %q, want it to announce COMMITTED", s)
	}
	if !strings.Contains(s, "reprice_audit") {
		t.Errorf("stdout = %q, want the audit line", s)
	}
	// Row repriced.
	if cost, ver := storedCLICost(t, path); cost != want || ver != active {
		t.Errorf("after commit cost=%d version=%d, want %d/%d", cost, ver, want, active)
	}
}

// seedProtectedOnlyDB creates a DB whose ONLY rows at or above the version floor
// are ones a reprice must never re-derive: a caller-authoritative manual import
// (source='api', e.g. a #346 sanctioned cost correction) and a money-carrying row
// with no token counts to explain that money.
func seedProtectedOnlyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "protected.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "alice", IssueID: "issue-invoice", Model: "claude-sonnet-4",
		InputTok: 1000, OutputTok: 500, CostMicro: 42_000_000,
		Source: "api", Fidelity: "estimated", IdempotencyKey: "invoice-1",
		PriceVersion: 3, Timestamp: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed manual row: %v", err)
	}
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "bob", IssueID: "issue-zero", Model: "claude-sonnet-4",
		CostMicro: 7_000_000, Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: "zero-1",
		PriceVersion:   3, Timestamp: time.Date(2026, 7, 11, 12, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed zero-token money row: %v", err)
	}
	return path
}

// TestRunRepriceCmd_ReportsProtectedRows proves the operator is TOLD when the
// version floor matched rows that were deliberately not repriced. Silence here
// would be the honesty regression the exclusion filter could otherwise introduce:
// the rows exist, they matched the floor, and they were protected on purpose.
func TestRunRepriceCmd_ReportsProtectedRows(t *testing.T) {
	path := seedProtectedOnlyDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "protected 2 row(s)") {
		t.Errorf("stdout = %q, want it to report the 2 protected rows", s)
	}
	// 🔴 The regression this guards: with every matching row protected, RowCount is
	// 0, and the pre-existing zero-rows branch would tell the operator to check
	// --from-version — blaming a flag that is perfectly correct.
	if strings.Contains(s, "check --from-version") {
		t.Errorf("stdout = %q, must NOT blame --from-version when the floor matched rows that were protected", s)
	}
	if !strings.Contains(s, "--from-version is not the problem") {
		t.Errorf("stdout = %q, want the protected-only explanation", s)
	}
}

// TestRunRepriceCmd_ProtectedRowsSurviveCommit is the end-to-end money assertion
// behind the report above: a committed sweep leaves both protected figures
// exactly as stored. A $42.00 sanctioned correction repriced to $0.00 is what
// made this RED.
func TestRunRepriceCmd_ProtectedRowsSurviveCommit(t *testing.T) {
	path := seedProtectedOnlyDB(t)
	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", path, "--from-version", "1", "--commit"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = db.Close() }()
	since := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	events, _, err := db.ListTokenEvents(context.Background(), since, since.Add(48*time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	got := map[string]int64{}
	for _, e := range events {
		got[e.IdempotencyKey] = e.CostMicro
	}
	if got["invoice-1"] != 42_000_000 {
		t.Errorf("manual-import cost_micro after --commit = %d, want 42000000 UNCHANGED", got["invoice-1"])
	}
	if got["zero-1"] != 7_000_000 {
		t.Errorf("zero-token money row cost_micro after --commit = %d, want 7000000 UNCHANGED", got["zero-1"])
	}
	if !strings.Contains(out.String(), "protected 2 row(s)") {
		t.Errorf("stdout = %q, want the protected report on a COMMIT too, not just a dry run", out.String())
	}
}
