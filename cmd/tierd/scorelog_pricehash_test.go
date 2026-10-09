package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// priceHashTableYAML is a minimal, valid price table used by the tests below.
// The comment lines are load-bearing: the comment-only-edit case rewrites them.
// Its version is in the operator-override range (>= 1000): an override that reuses
// the embedded table's version is refused at load (#714), so a fixture version below
// 1000 breaks the day the embedded table reaches it.
const priceHashTableYAML = `# source: https://example.invalid/pricing
version: 1000
effective_date: "2026-08-01"
models:
  "custom-model-1": { input_per_m: 3.0, output_per_m: 3.0, provider: anthropic }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// TestRunScoreLog_PriceTableIdentityIsSurfaced pins that `tierd score-log`
// actually EMITS the #713 identity on the default (embedded) path.
//
// A stamp that only appears behind --prices would leave every zero-config
// install unidentified, which is the majority of them.
func TestRunScoreLog_PriceTableIdentityIsSurfaced(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	loadEmbeddedPriceTable(t) // assert about the EMBEDDED table, not a predecessor's leftovers

	dir := t.TempDir()
	log := writeClaudeLog(t, dir, "session.jsonl", "custom-model-1", 1000, 0, 0)

	var stdout, stderr bytes.Buffer
	if code := runScoreLog([]string{"--format", "claude", "--log", log}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	report := decodeReport(t, stdout.Bytes())

	active := store.ActivePriceTableInfo()
	if report.PriceTable.TableHash != active.TableHash {
		t.Errorf("price_table.table_hash = %q, want the ACTIVE table's %q",
			report.PriceTable.TableHash, active.TableHash)
	}
	if report.PriceTable.FileHash != active.FileHash {
		t.Errorf("price_table.file_hash = %q, want %q", report.PriceTable.FileHash, active.FileHash)
	}
	// Guard the guard: empty-vs-empty would satisfy both arms above.
	if active.TableHash == "" || active.FileHash == "" {
		t.Fatal("the active price table carries no hashes — the assertions above are vacuous")
	}
	if !regexp.MustCompile(`^tierpt1:[0-9a-f]{64}$`).MatchString(report.PriceTable.TableHash) {
		t.Errorf("table_hash = %q, want ^tierpt1:[0-9a-f]{64}$ — the scheme tag is what lets a "+
			"future canonicalization fix be told apart from tampering",
			report.PriceTable.TableHash)
	}
	if !strings.HasPrefix(report.PriceTable.FileHash, "sha256:") {
		t.Errorf("file_hash = %q, want a sha256: prefix", report.PriceTable.FileHash)
	}
}

// TestRunScoreLog_CommentOnlyEditMovesFileHashOnly is the issue's "done when"
// demonstration, executed in-process: run against a price file, run again
// against a comment-touched copy, and assert table_hash is IDENTICAL while
// file_hash and source both move.
//
// This is the feature's whole claim in one test. It is here rather than only in
// internal/store because the store-level test proves the hasher behaves; this
// proves the CLI surface actually carries the hasher's answer out to a consumer.
func TestRunScoreLog_CommentOnlyEditMovesFileHashOnly(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	dir := t.TempDir()
	log := writeClaudeLog(t, dir, "session.jsonl", "custom-model-1", 1_000_000, 0, 0)

	pathA := filepath.Join(dir, "prices-a.yaml")
	if err := os.WriteFile(pathA, []byte(priceHashTableYAML), 0o600); err != nil {
		t.Fatalf("write prices A: %v", err)
	}
	// The same table with every comment line rewritten — exactly what
	// `sed 's|^# |# (touched) |'` does to internal/store/prices.yaml.
	touched := strings.ReplaceAll(priceHashTableYAML, "\n# ", "\n# (touched) ")
	if strings.HasPrefix(priceHashTableYAML, "# ") {
		touched = "# (touched) " + strings.TrimPrefix(touched, "# ")
	}
	pathB := filepath.Join(dir, "prices-b.yaml")
	if err := os.WriteFile(pathB, []byte(touched), 0o600); err != nil {
		t.Fatalf("write prices B: %v", err)
	}

	// 🔴 CONTROL: identical inputs make every assertion below vacuous.
	rawA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("read prices A: %v", err)
	}
	rawB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("read prices B: %v", err)
	}
	if bytes.Equal(rawA, rawB) {
		t.Fatal("control: inputs identical — assertions vacuous")
	}

	run := func(path string) scoreLogReport {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runScoreLog([]string{"--format", "claude", "--log", log, "--prices", path}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d for --prices %s, want 0; stderr=%s", code, path, stderr.String())
		}
		return decodeReport(t, stdout.Bytes())
	}
	a, b := run(pathA), run(pathB)

	if a.PriceTable.TableHash != b.PriceTable.TableHash {
		t.Errorf("a COMMENT-ONLY edit moved table_hash:\n  %s\n  %s\n"+
			"table_hash must identify the RESOLVED prices, so the source-URL comments stay "+
			"editable without re-identifying the table", a.PriceTable.TableHash, b.PriceTable.TableHash)
	}
	if a.PriceTable.FileHash == b.PriceTable.FileHash {
		t.Errorf("a comment-only edit did NOT move file_hash (both %s) — the raw-bytes digest "+
			"is not reading the raw bytes, and the source comments are then unauditable",
			a.PriceTable.FileHash)
	}
	if a.PriceTable.Source == b.PriceTable.Source {
		t.Errorf("both runs reported source = %q, but they were given different --prices paths",
			a.PriceTable.Source)
	}
	if a.PriceTable.Source != pathA || b.PriceTable.Source != pathB {
		t.Errorf("source = (%q, %q), want (%q, %q)",
			a.PriceTable.Source, b.PriceTable.Source, pathA, pathB)
	}
	// And the identity must actually MOVE on a real rate change — without this,
	// a constant table_hash satisfies the equality arm above.
	priced := strings.Replace(priceHashTableYAML, "input_per_m: 3.0", "input_per_m: 4.0", 1)
	if priced == priceHashTableYAML {
		t.Fatal("control: the rate-change substitution did not change the document")
	}
	pathC := filepath.Join(dir, "prices-c.yaml")
	if err := os.WriteFile(pathC, []byte(priced), 0o600); err != nil {
		t.Fatalf("write prices C: %v", err)
	}
	if c := run(pathC); c.PriceTable.TableHash == a.PriceTable.TableHash {
		t.Errorf("a real rate change (3.0 → 4.0) left table_hash unchanged (%s) — the identity "+
			"is a constant and certifies nothing", c.PriceTable.TableHash)
	}
}

// TestRunScore_PrintsPriceIdentityStamp pins the STDERR line, which is a
// separate surface from the JSON above and the one scripts/seam-exercise.sh
// extracts. It also pins that the README-pinned stamp line is still emitted
// verbatim: the identity had to be a NEW line precisely because widening
// EmbeddedPriceStampFormat would put a per-edit-volatile hash inside a line the
// public README quotes as sample output.
//
// 🔴 IT RUNS runScore. The first version of this test rendered the two shared
// format constants with fmt.Sprintf and asserted against its OWN output, which
// proves the CONSTANTS are well-shaped and nothing else. Review caught it:
// deleting the fmt.Fprintf at the price-identity print site in main.go, or
// converting it to logger.Info (which wraps the text as `msg="..."` and defeats
// seam-exercise.sh's `^` anchor), left that version GREEN. And the only other
// place the real binary's real stderr is inspected is seam-exercise.sh, which
// SKIPS ITSELF in a git worktree — the standard dev context — so the proxy
// was the entire effective coverage of the print site.
func TestRunScore_PrintsPriceIdentityStamp(t *testing.T) {
	loadEmbeddedPriceTable(t)
	info := store.ActivePriceTableInfo()
	// Guard the guard: an unstamped active table would make the value assertion
	// below compare "" against "".
	if info.TableHash == "" || info.FileHash == "" {
		t.Fatal("the active price table carries no hashes — the assertions below are vacuous")
	}

	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)

	stdout, errOut := captureBothStreams(t, func() {
		runScore([]string{
			"--repo", repo, "--claude-dir", claudeDir,
			"--since", "2026-01-01", "--developer", "alice",
		})
	})

	// The README-pinned line must still be emitted, unwidened.
	if !strings.Contains(errOut, "price table: embedded default (version ") {
		t.Errorf("runScore no longer prints the README-pinned price-table stamp; stderr was:\n%s", errOut)
	}
	// The identity line must carry the ACTIVE table's REAL hashes, not merely a
	// well-shaped string — a hardcoded placeholder passes a shape check.
	want := fmt.Sprintf(store.PriceIdentityStampFormat, info.TableHash, info.FileHash)
	if !strings.Contains(errOut, want) {
		t.Errorf("runScore did not print the price-table identity for the ACTIVE table.\n"+
			"  want line: %s\n  stderr:\n%s", want, errOut)
	}
	// 🔴 WHOLE LINE, COLUMN-ANCHORED. scripts/seam-exercise.sh extracts with a
	// `^`-anchored sed, so an identity appended to another line, indented, or
	// wrapped by slog would make that guard silently extract nothing while a
	// substring check here still passed.
	anchored := regexp.MustCompile(`(?m)^price table identity: table_hash=tierpt1:[0-9a-f]{64} file_hash=sha256:[0-9a-f]{64}$`)
	if !anchored.MatchString(errOut) {
		t.Errorf("the identity stamp is not a WHOLE, column-anchored stderr line of the "+
			"expected shape — scripts/seam-exercise.sh anchors its extraction at ^ and would "+
			"silently extract nothing.\nstderr:\n%s", errOut)
	}
	// STDERR specifically. stdout is the operator's report; a provenance line
	// mixed into it would corrupt a piped or parsed report, and the seam script
	// reads the two streams from different files.
	if strings.Contains(stdout, "price table identity:") {
		t.Errorf("the price-table identity leaked onto STDOUT, which carries the report:\n%s", stdout)
	}
}

// TestRunRepriceCmd_PrintsPriceIdentityStamp closes the second half of the
// stderr surface. `tierd reprice` REWRITES stored costs, so "which exact table
// did this run price against" is the audit question the version stamp alone
// cannot answer — and it was the one production site of this feature with zero
// coverage of any kind: seam-exercise.sh drives `tierd score` only, and it skips
// itself in a git worktree anyway.
//
// runRepriceCmd takes explicit writers, so unlike runScore (which calls os.Exit
// on a bad flag) it is directly drivable.
//
// ⚠️ It must be driven on the REAL path, not a rejecting one. A first draft
// invoked it with no --from-version on the assumption that the provenance
// stamps print before argument validation; measured, they do not — reprice
// validates --from-version and stats --db FIRST, so the rejecting form emits
// only the error and this test failed. Left as a note because the same wrong
// assumption would make a future edit "fix" the test by weakening it.
//
// This runs the default DRY RUN (no --commit), so nothing is mutated.
func TestRunRepriceCmd_PrintsPriceIdentityStamp(t *testing.T) {
	loadEmbeddedPriceTable(t)
	info := store.ActivePriceTableInfo()
	if info.TableHash == "" || info.FileHash == "" {
		t.Fatal("the active price table carries no hashes — the assertions below are vacuous")
	}

	var out, errb bytes.Buffer
	if code := runRepriceCmd([]string{"--db", seedRepriceDB(t), "--from-version", "1"}, &out, &errb); code != 0 {
		t.Fatalf("reprice dry run exited %d, want 0; stderr=%s", code, errb.String())
	}
	stderr := errb.String()

	want := fmt.Sprintf(store.PriceIdentityStampFormat, info.TableHash, info.FileHash)
	if !strings.Contains(stderr, want) {
		t.Errorf("runRepriceCmd did not print the price-table identity for the ACTIVE table.\n"+
			"  want line: %s\n  stderr:\n%s", want, stderr)
	}
	if !regexp.MustCompile(`(?m)^price table identity: table_hash=tierpt1:[0-9a-f]{64} file_hash=sha256:[0-9a-f]{64}$`).MatchString(stderr) {
		t.Errorf("the reprice identity stamp is not a whole, column-anchored line:\n%s", stderr)
	}
	// The README-pinned stamp must still precede it, unwidened.
	if !strings.Contains(stderr, "price table: embedded default (version ") {
		t.Errorf("runRepriceCmd no longer prints the README-pinned price-table stamp:\n%s", stderr)
	}
}
