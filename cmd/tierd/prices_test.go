package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// collidingTableYAML renders a price table at the given version whose one
// non-fallback model bills at inputPerM.
func collidingTableYAML(version int, inputPerM float64) string {
	return fmt.Sprintf(`version: %d
effective_date: "2026-08-01"
models:
  "custom-model-1": { input_per_m: %g, output_per_m: 3.0, provider: anthropic }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`, version, inputPerM)
}

func writeCollidingTable(t *testing.T, dir, name string, version int, inputPerM float64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(collidingTableYAML(version, inputPerM)), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestRunScoreLog_RefusesEmbeddedVersionCollision proves the #714 layer-1 guard is
// REACHABLE FROM `tierd score-log`.
//
// 🔑 THAT REACHABILITY IS THE REQUIREMENT. score-log calls store.LoadPriceTable
// DIRECTLY (see runScoreLog), bypassing main.go's loadPricesOverride wrapper
// entirely — so a guard written into that wrapper would leave score-log unguarded
// while every serve-path test stayed green. score-log opens no database, so
// nothing here can be attributed to the Open-time (layer 2) guard.
//
// ⚠️ IT PROVES REACHABILITY, NOT PLACEMENT, and the distinction is worth stating:
// this test reddens both when the guard is MOVED to the wrapper and when it is
// DELETED outright, so it does not by itself distinguish those two mutants.
// Reachability is what the requirement actually is, so that is fine — but do not
// cite this test as proof that the guard lives in a particular function.
func TestRunScoreLog_RefusesEmbeddedVersionCollision(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	emb := store.ActivePriceTableInfo()
	if emb.Version < 1 || emb.TableHash == "" {
		t.Fatalf("precondition: no embedded table loaded (%+v)", emb)
	}

	dir := t.TempDir()
	log := writeClaudeLog(t, dir, "session.jsonl", "custom-model-1", 1000, 0, 0)
	// A table at the EMBEDDED version that cannot be the embedded table.
	colliding := writeCollidingTable(t, dir, "collide.yaml", emb.Version, 3.0)

	var stdout, stderr bytes.Buffer
	code := runScoreLog([]string{"--format", "claude", "--log", log, "--prices", colliding}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("score-log exit = 0 with a colliding --prices table; want a non-zero refusal.\nstdout=%s", stdout.String())
	}
	msg := stderr.String()
	for _, want := range []string{"table_hash", fmt.Sprintf("%d", emb.Version), emb.TableHash, "Bump 'version:'"} {
		if !strings.Contains(msg, want) {
			t.Errorf("score-log stderr %q missing %q", msg, want)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("score-log emitted a report despite refusing the price table: %s", stdout.String())
	}
}

// forgetVersionRemedyRE extracts the `tierd prices forget-version …` invocation
// out of a store collision error.
var forgetVersionRemedyRE = regexp.MustCompile(`'(tierd prices forget-version [^']*)'`)

// TestForgetVersionRemedyIsRunnable EXECUTES the remedy the fail-closed refusal
// hands the operator, instead of asserting a substring about it.
//
// 🔴 THIS IS THE TEST THAT WOULD HAVE CAUGHT THE BUG FOUR REVIEWS FOUND. The
// refusal used to print `tierd prices forget-version 9 --commit` — a POSITIONAL
// version. Go's flag package stops parsing at the first non-flag argument, so
// --commit was silently dropped and the command died with "--version is required"
// (measured, rc=1). Every existing assertion passed, because they all checked for
// the substring "forget-version" or drove the CLI with hand-written, CORRECT
// argument slices. A test that asserts about a surface it never drives cannot see
// this class of defect; this one splits the printed string and feeds it to
// dispatch.
func TestForgetVersionRemedyIsRunnable(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "remedy.db")

	// Produce a REAL collision so the message under test is the real one.
	first := writeCollidingTable(t, dir, "first.yaml", 5100, 3.0)
	second := writeCollidingTable(t, dir, "second.yaml", 5100, 3.5)
	if _, err := store.LoadPriceTable(first); err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()
	if _, err := store.LoadPriceTable(second); err != nil {
		t.Fatalf("LoadPriceTable(second): %v", err)
	}
	_, err = store.Open(dbPath)
	if err == nil {
		t.Fatal("precondition: no collision was produced")
	}

	m := forgetVersionRemedyRE.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("the refusal names no quoted 'tierd prices forget-version …' command:\n%v", err)
	}
	fields := strings.Fields(m[1])
	if len(fields) < 3 || fields[0] != "tierd" {
		t.Fatalf("extracted remedy is not a tierd invocation: %q", m[1])
	}
	// Drop the binary name, then undo the ONE transformation a shell would do for
	// the operator. The database path is rendered through logsafe.Str, which
	// %q-quotes it — deliberately: that is the CodeQL-recognized injection barrier
	// AND it is what makes a path containing spaces survive being pasted. A POSIX
	// shell strips those quotes before exec; strconv.Unquote is their exact
	// inverse, so this stays a test of the COMMAND, not of the quoting.
	args := make([]string, 0, len(fields)-1)
	for _, f := range fields[1:] {
		if unq, err := strconv.Unquote(f); err == nil {
			f = unq
		}
		args = append(args, f)
	}
	var stdout, stderr bytes.Buffer
	if code := dispatch(args, &stdout, &stderr); code != 0 {
		t.Fatalf("the remedy printed by the refusal FAILED when executed.\n  remedy: %s\n  args:   %q\n  exit:   %d\n  stderr: %s",
			m[1], args, code, stderr.String())
	}
	// It really did the thing: the row is gone and the same Open now succeeds.
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open still refused after running the printed remedy: %v", err)
	}
	_ = db2.Close()
}

// TestRunPricesForgetVersion drives the escape-hatch CLI end to end through
// dispatch: dry run, --commit, --json, and the operator-error paths.
//
// The command IS the documented remedy printed in the Open-time refusal, so a
// broken flag surface here means the refusal names a way out that does not work.
func TestRunPricesForgetVersion(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	version := fmt.Sprintf("%d", store.ActivePriceTableInfo().Version)

	// Missing --version is a usage error, not a silent no-op.
	var out, errBuf bytes.Buffer
	if code := dispatch([]string{"prices", "forget-version", "--db", dbPath}, &out, &errBuf); code == 0 {
		t.Errorf("forget-version without --version exit = 0, want non-zero")
	}
	if !strings.Contains(errBuf.String(), "--version is required") {
		t.Errorf("stderr %q does not explain the missing flag", errBuf.String())
	}

	// A POSITIONAL version is refused loudly rather than silently dropping
	// --commit and doing a dry run. This is the pasted-remedy shape.
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "forget-version", "--db", dbPath, version, "--commit"}, &out, &errBuf); code == 0 {
		t.Error("a positional version exit = 0; want a loud refusal, not a silent dry run")
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("stderr %q does not name the stray positional", errBuf.String())
	}

	// DRY RUN: prints the row, deletes nothing.
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "forget-version", "--db", dbPath, "--version", version}, &out, &errBuf); code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, errBuf.String())
	}
	dry := out.String()
	for _, want := range []string{"would retire", "table_hash", "DRY RUN"} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry-run output %q missing %q", dry, want)
		}
	}

	// --json DRY RUN: machine-readable, and deleted=false is ABSENT (omitempty).
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "forget-version", "--db", dbPath, "--version", version, "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("json dry-run exit = %d, want 0; stderr=%s", code, errBuf.String())
	}
	var dryRow map[string]any
	if err := json.Unmarshal(out.Bytes(), &dryRow); err != nil {
		t.Fatalf("decode --json output %q: %v", out.String(), err)
	}
	if dryRow["table_hash"] != store.ActivePriceTableInfo().TableHash {
		t.Errorf("--json table_hash = %v, want the active %q", dryRow["table_hash"], store.ActivePriceTableInfo().TableHash)
	}
	if v, ok := dryRow["retired"]; ok && v == true {
		t.Error("--json reports retired=true on a DRY RUN")
	}

	// --json --commit: deleted MUST be true. Hardcoding this field to false was a
	// measured surviving mutant — it is the only thing distinguishing an archived
	// dry run from an archived real deletion.
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "forget-version", "--db", dbPath, "--version", version, "--json", "--commit"}, &out, &errBuf); code != 0 {
		t.Fatalf("json commit exit = %d, want 0; stderr=%s", code, errBuf.String())
	}
	// EXACTLY ONE top-level JSON document. An earlier draft emitted the row twice
	// (once before the write, once after) and the result was not parseable at all;
	// a decode is what catches that, where a substring assertion would not.
	var committedRow map[string]any
	if err := json.Unmarshal(out.Bytes(), &committedRow); err != nil {
		t.Fatalf("--json --commit output is not a single JSON document %q: %v", out.String(), err)
	}
	if committedRow["retired"] != true {
		t.Errorf("--json --commit reports retired=%v, want true — an archived retirement is indistinguishable from an archived dry run without it", committedRow["retired"])
	}
	if committedRow["forgotten_by"] == nil || committedRow["forgotten_by"] == "" {
		t.Error("--json --commit archive does not name who retired the identity")
	}
	// 🔴 IT DID NOT DELETE. The maintainer's ruling: the row is retained and stamped, so the
	// database can still answer what the version meant. An assertion that the
	// registry is now EMPTY would pass the implementation this replaced.
	rows, err := store.ListPriceTableRegistry(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("registry holds %d rows after --commit, want 1 (RETAINED and stamped)", len(rows))
	}
	if !rows[0].Forgotten() || rows[0].ForgottenBy == "" {
		t.Errorf("the retained row is not stamped: %+v", rows[0])
	}
	// And the act is in the ledger, in the same family as reprice/repair-repo.
	ledger, err := store.ListPriceForgetAudit(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("ListPriceForgetAudit: %v", err)
	}
	if len(ledger) != 1 || ledger[0].Version != store.ActivePriceTableInfo().Version {
		t.Errorf("ledger = %+v, want one entry for the retired version", ledger)
	}

	// Forgetting it again is an explicit "nothing to forget" with its OWN exit
	// code (2), distinct from a genuine failure (1) — the two are different
	// operator situations and an identical response made the branch dead code.
	out.Reset()
	errBuf.Reset()
	code := dispatch([]string{"prices", "forget-version", "--db", dbPath, "--version", version, "--commit"}, &out, &errBuf)
	if code != 2 {
		t.Errorf("second forget-version exit = %d, want 2 (nothing to forget, distinct from a failure)", code)
	}
	if !strings.Contains(errBuf.String(), "nothing to forget") {
		t.Errorf("stderr %q does not say the version was never recorded", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "prices list") {
		t.Errorf("stderr %q does not point at the command that answers 'which versions are recorded?'", errBuf.String())
	}
}

// TestRunPricesForgetVersion_UnwritableStdoutAbortsTheDelete is the CLI half of
// the evidence-preservation fix.
//
// 🔴 MEASURED BEFORE THE FIX: `forget-version --commit >&-` exited 0 with the row
// deleted and nothing printed, because the store deleted first and the CLI
// discarded every write error. The entire case for this command over a
// `--accept-rehash` flag rests on the operator keeping that evidence.
func TestRunPricesForgetVersion_UnwritableStdoutAbortsTheDelete(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = db.Close()
	version := fmt.Sprintf("%d", store.ActivePriceTableInfo().Version)

	var errBuf bytes.Buffer
	code := dispatch([]string{"prices", "forget-version", "--db", dbPath, "--version", version, "--commit"},
		failingWriter{}, &errBuf)
	if code == 0 {
		t.Error("exit = 0 although the evidence could not be written; want non-zero")
	}
	rows, err := store.ListPriceTableRegistry(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("registry holds %d rows, want 1", len(rows))
	}
	if rows[0].Forgotten() {
		t.Error("the identity was retired although the operator never saw the evidence")
	}
	ledger, err := store.ListPriceForgetAudit(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("ListPriceForgetAudit: %v", err)
	}
	if len(ledger) != 0 {
		t.Errorf("ledger holds %d entries, want 0 for an aborted operation", len(ledger))
	}
}

// failingWriter fails every write, standing in for a closed stdout, a full disk,
// or an EPIPE from `| head`.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("simulated: stdout is closed") }

// TestRunPricesList pins the READ surface. Without it the only way to inspect the
// registry is a dry run of the destructive command.
func TestRunPricesList(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = db.Close()
	active := store.ActivePriceTableInfo()

	var out, errBuf bytes.Buffer
	if code := dispatch([]string{"prices", "list", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("prices list exit = %d, want 0; stderr=%s", code, errBuf.String())
	}
	text := out.String()
	for _, want := range []string{
		fmt.Sprintf("version %d", active.Version),
		active.TableHash,
		active.FileHash,
		"first_seen",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prices list output missing %q:\n%s", want, text)
		}
	}
	// It must state the limit of what a recorded row establishes — the registry
	// records identity, it does not certify rows written before first_seen.
	if !strings.Contains(text, "first_seen") || !strings.Contains(text, "not covered") {
		t.Errorf("prices list does not state the first_seen scoping limit:\n%s", text)
	}

	// --json round-trips.
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "list", "--db", dbPath, "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("prices list --json exit = %d; stderr=%s", code, errBuf.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("decode json %q: %v", out.String(), err)
	}
	if len(rows) != 1 {
		t.Fatalf("json rows = %d, want 1", len(rows))
	}
	if rows[0]["table_hash"] != active.TableHash {
		t.Errorf("json table_hash = %v, want %q", rows[0]["table_hash"], active.TableHash)
	}
	// `list` is not a deletion, so it must not carry the deleted field at all.
	if _, ok := rows[0]["deleted"]; ok {
		t.Error("prices list emitted a `deleted` field; that belongs only to forget-version")
	}
}

// TestRunPricesAudit_AnswersWhatAVersionMeantAfterRetirement is the CLI-level
// guard for the maintainer's ruling, and it deliberately asks the question the OPERATOR
// asks rather than the one the schema answers.
//
// 🔴 IT ASSERTS THE RECORD SURVIVES, NOT THAT THE ROW IS GONE. After
// `forget-version --commit`, both `prices list` and `prices audit` must still tell
// you that version N meant table_hash X, when it was retired, and by whom. An
// implementation that hard-deleted would pass an absence assertion and fail this
// one, which is the entire point.
func TestRunPricesAudit_AnswersWhatAVersionMeantAfterRetirement(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = db.Close()
	active := store.ActivePriceTableInfo()
	version := fmt.Sprintf("%d", active.Version)

	// Empty ledger reads as such, not as an error.
	var out, errBuf bytes.Buffer
	if code := dispatch([]string{"prices", "audit", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("prices audit on a clean database exit = %d; stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "no price-table identities have been retired") {
		t.Errorf("empty-ledger output = %q", out.String())
	}

	// Retire it, naming an explicit actor.
	const who = "sre-oncall"
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "forget-version", "--db", dbPath,
		"--version", version, "--actor", who, "--commit"}, &out, &errBuf); code != 0 {
		t.Fatalf("forget-version exit = %d; stderr=%s", code, errBuf.String())
	}
	// The success text states what was KEPT — "retired, not deleted" is the ruling
	// in one line, and it belongs on the path the operator actually reads.
	for _, want := range []string{"retired at", who, "KEPT, not deleted", "prices audit"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("forget-version output missing %q:\n%s", want, out.String())
		}
	}

	// ---- `prices list` still answers ------------------------------------
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "list", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("prices list exit = %d; stderr=%s", code, errBuf.String())
	}
	listed := out.String()
	for _, want := range []string{"RETIRED", active.TableHash, "forgotten_at", "forgotten_by", who} {
		if !strings.Contains(listed, want) {
			t.Errorf("prices list no longer answers what version %s meant — missing %q:\n%s", version, want, listed)
		}
	}

	// ---- `prices audit` answers independently ---------------------------
	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "audit", "--db", dbPath, "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("prices audit --json exit = %d; stderr=%s", code, errBuf.String())
	}
	var ledger []map[string]any
	if err := json.Unmarshal(out.Bytes(), &ledger); err != nil {
		t.Fatalf("decode audit json %q: %v", out.String(), err)
	}
	if len(ledger) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(ledger))
	}
	e := ledger[0]
	if e["table_hash"] != active.TableHash {
		t.Errorf("ledger table_hash = %v, want %q", e["table_hash"], active.TableHash)
	}
	if e["actor"] != who {
		t.Errorf("ledger actor = %v, want %q", e["actor"], who)
	}
	for _, k := range []string{"forget_id", "ts", "tool_version", "recorded_tool_version", "first_seen"} {
		if v, ok := e[k]; !ok || v == "" {
			t.Errorf("ledger entry field %q is missing or empty", k)
		}
	}

	// ---- and the hatch actually worked ----------------------------------
	// A guarantee that preserved the evidence but broke the remedy would be no fix.
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open after retiring the identity was refused: %v", err)
	}
	_ = db2.Close()
}

// TestRunPricesForgetVersion_DefaultsActorToTheOSUser pins that forgotten_by is
// always populated even when --actor is omitted. An attribution ledger whose
// attribution column is routinely blank is not one.
func TestRunPricesForgetVersion_DefaultsActorToTheOSUser(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = db.Close()

	var out, errBuf bytes.Buffer
	code := dispatch([]string{"prices", "forget-version", "--db", dbPath,
		"--version", fmt.Sprintf("%d", store.ActivePriceTableInfo().Version), "--commit"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errBuf.String())
	}
	rows, err := store.ListPriceTableRegistry(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if len(rows) != 1 || !rows[0].Forgotten() {
		t.Fatalf("rows = %+v, want one retired row", rows)
	}
	if rows[0].ForgottenBy == "" {
		t.Error("forgotten_by is empty with no --actor; it must default to the OS username")
	}
	if rows[0].ForgottenBy != collector.OSUsername() {
		t.Errorf("forgotten_by = %q, want the OS username %q", rows[0].ForgottenBy, collector.OSUsername())
	}
}

// TestDispatchInstallsToolVersion pins the WIRING, not just the setter.
//
// 🔴 A MEASURED SURVIVOR BEFORE THIS TEST: deleting
// `store.SetToolVersion(versionString())` from dispatch left the whole suite
// green, and every registry row a real tierd wrote would silently have read
// tool_version="unknown" forever. The store-level test exercises the setter
// directly and never goes through dispatch, so it cannot see the wire being cut.
func TestDispatchInstallsToolVersion(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tier.db")

	// Route ANY command through dispatch first — the install happens before the
	// subcommand switch, so even `version` arms it.
	var out, errBuf bytes.Buffer
	if code := dispatch([]string{"version"}, &out, &errBuf); code != 0 {
		t.Fatalf("dispatch version exit = %d", code)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = db.Close()

	rows, err := store.ListPriceTableRegistry(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("registry rows = %d, want 1", len(rows))
	}
	if rows[0].ToolVersion == "unknown" || rows[0].ToolVersion == "" {
		t.Errorf("tool_version = %q — dispatch did not install the build identity, so every registry row a real tierd writes is unattributed", rows[0].ToolVersion)
	}
	if rows[0].ToolVersion != versionString() {
		t.Errorf("tool_version = %q, want %q", rows[0].ToolVersion, versionString())
	}
}

// TestRunPricesUsage pins that `tierd prices` with no subcommand and with an
// unknown one are usage errors, that the usage block names a WORKING invocation
// for each subcommand (a remedy nobody can spell is not a remedy), and that the
// top-level help lists the command at all.
func TestRunPricesUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := dispatch([]string{"prices"}, &out, &errBuf); code == 0 {
		t.Error("bare `tierd prices` exit = 0, want non-zero usage error")
	}
	usage := errBuf.String()
	for _, want := range []string{"forget-version", "list", "--version", "--db", "--commit"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage %q does not name %q", usage, want)
		}
	}

	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"prices", "no-such-thing"}, &out, &errBuf); code == 0 {
		t.Error("unknown prices subcommand exit = 0, want non-zero")
	}

	out.Reset()
	errBuf.Reset()
	if code := dispatch([]string{"help"}, &out, &errBuf); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "prices") {
		t.Errorf("top-level usage does not list `prices`:\n%s", out.String())
	}
}
