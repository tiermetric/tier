package main

// CLI tests for `tierd repair-repo` (#493).
//
// The store-level invariants (never overwrite a real repo, never guess an
// unresolvable row, re-run is a no-op, the before-image ledger) are proven in
// internal/store/repairrepo_test.go. These tests cover the CLI's own
// responsibilities: flag validation, the mapping parser, dispatch registration,
// and — the part an operator actually reads — that the report tells the truth
// about what did and did not happen.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// repairSeed is one synthetic row for a CLI fixture.
type repairSeed struct {
	developer string
	session   string
	repo      string // "" -> the insert path stores the 'unqualified' sentinel
	cost      int64
}

// seedRepairDB writes a fresh DB at a temp path and returns it. Every fixture
// here is synthetic: this workstation has ZERO unqualified rows left, so there
// is no real data to point the repair at (the issue body's reported event count and
// unqualified share are stale).
func seedRepairDB(t *testing.T, seeds []repairSeed) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repair.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()
	for i, s := range seeds {
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
			Developer: s.developer,
			IssueID:   "issue-493",
			Model:     "claude-sonnet-4",
			InputTok:  1000,
			CostMicro: s.cost,
			Source:    "jsonl",
			Fidelity:  "realtime",
			Repo:      s.repo,
			SessionID: s.session,
			// fmt, not string(rune('a'+i)): the truncation test seeds 27 rows, and
			// a rune-offset key would run past 'z' into punctuation — still unique
			// today, but a fixture whose uniqueness depends on ASCII arithmetic is
			// one seed away from silently colliding on the UPSERT and returning
			// FEWER rows than the test believes it created.
			IdempotencyKey: fmt.Sprintf("cli-repair-seed-%03d", i),
			Timestamp:      time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	return path
}

// storedRepairRepos reads back every row as (row id -> repo).
//
// ⚠️ wantRows IS LOAD-BEARING, NOT DEFENSIVE. The first draft keyed by SessionID
// and returned whatever it found, so a "nothing moved" assertion written as
// `for session, repo := range storedRepairRepos(...)` passed VACUOUSLY when the
// readback returned nothing — and the readback has three ways to return nothing
// (its fixed window, its row limit, a cursor change). The two tests that mattered
// most, dry-run-changes-nothing and re-run-is-a-no-op, were exactly the two that
// survived an empty readback. Keying by id also stops a fixture with two rows per
// session from silently collapsing to one assertion.
func storedRepairRepos(t *testing.T, path string, wantRows int) map[int64]string {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = db.Close() }()
	since := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	events, _, err := db.ListTokenEvents(context.Background(), since, since.Add(48*time.Hour), store.PageCursor{}, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != wantRows {
		t.Fatalf("read back %d row(s), want %d — an assertion looping over this map would otherwise pass vacuously", len(events), wantRows)
	}
	out := make(map[int64]string, len(events))
	for _, e := range events {
		out[e.ID] = e.Repo
	}
	return out
}

// storedRepairReposBySession is the by-session view, for the tests that assert on
// a named session rather than row-for-row. It shares the row-count guard.
func storedRepairReposBySession(t *testing.T, path string, wantRows int) map[string]string {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = db.Close() }()
	since := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	events, _, err := db.ListTokenEvents(context.Background(), since, since.Add(48*time.Hour), store.PageCursor{}, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != wantRows {
		t.Fatalf("read back %d row(s), want %d", len(events), wantRows)
	}
	out := make(map[string]string, len(events))
	for _, e := range events {
		out[e.SessionID] = e.Repo
	}
	return out
}

// TestRunRepairRepoCmd_RequiresDeveloper proves the required-selector guard at
// the CLI boundary: no --developer means exit 1, an actionable message, and no
// mutation.
func TestRunRepairRepoCmd_RequiresDeveloper(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--map", "sess-a=acme/app"}, &out, &errb); code != 1 {
		t.Errorf("exit code = %d, want 1 (missing --developer)", code)
	}
	if !strings.Contains(errb.String(), "--developer is required") {
		t.Errorf("stderr = %q, want it to say --developer is required", errb.String())
	}
	if got := storedRepairReposBySession(t, path, 1)["sess-a"]; got != repoid.Unqualified {
		t.Errorf("a rejected command mutated the row: repo = %q", got)
	}
}

// TestRunRepairRepoCmd_RequiresMapping proves a run with no mapping is REFUSED
// rather than reported as a clean zero-change repair. Without this, a forgotten
// --map prints "0 rows repaired", which is indistinguishable from "your history
// is already fine" — a false green on the exact command an operator reaches for
// when their history is NOT fine.
func TestRunRepairRepoCmd_RequiresMapping(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice"}, &out, &errb); code != 1 {
		t.Errorf("exit code = %d, want 1 (no mapping)", code)
	}
	if !strings.Contains(errb.String(), "no mapping supplied") {
		t.Errorf("stderr = %q, want it to say no mapping was supplied", errb.String())
	}
}

// TestRunRepairRepoCmd_NonexistentDBRejected proves the CLI refuses a --db path
// that does not exist rather than letting store.Open create an empty DB and
// report a misleading "examined 0 rows" for a typo'd path.
func TestRunRepairRepoCmd_NonexistentDBRejected(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", missing, "--developer", "alice", "--map", "sess-a=acme/app"}, &out, &errb)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (nonexistent --db)", code)
	}
	if !strings.Contains(errb.String(), "existing database") {
		t.Errorf("stderr = %q, want an 'existing database' message", errb.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("the rejected run CREATED the database file; it must not")
	}
}

// TestRunRepairRepoCmd_DryRunReportsButDoesNotMutate proves the default (no
// --commit) reports what would change and writes nothing — asserted row-for-row.
func TestRunRepairRepoCmd_DryRunReportsButDoesNotMutate(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-b", cost: 200},
	})
	before := storedRepairRepos(t, path, 2)

	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{
		"--db", path, "--developer", "alice",
		"--map", "sess-a=acme/app", "--map", "sess-b=acme/lib",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "DRY RUN") {
		t.Errorf("stdout = %q, want it to announce DRY RUN", s)
	}
	if !strings.Contains(s, "Re-run with --commit") {
		t.Errorf("stdout = %q, want the --commit hint", s)
	}
	if !strings.Contains(s, "acme/app") || !strings.Contains(s, "acme/lib") {
		t.Errorf("stdout = %q, want both target repos listed", s)
	}
	// Row-for-row: nothing moved.
	for id, repo := range storedRepairRepos(t, path, 2) {
		if repo != before[id] {
			t.Errorf("dry run mutated row %d: %q -> %q", id, before[id], repo)
		}
		if repo != repoid.Unqualified {
			t.Errorf("row %d repo = %q, want it still on the sentinel", id, repo)
		}
	}
}

// TestRunRepairRepoCmd_CommitApplies proves --commit repairs the rows and reports
// the audit ledgers it wrote.
func TestRunRepairRepoCmd_CommitApplies(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-b", cost: 200},
	})
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{
		"--db", path, "--developer", "alice", "--commit",
		"--map", "sess-a=acme/app", "--map", "sess-b=acme/lib",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "COMMITTED") {
		t.Errorf("stdout = %q, want it to announce COMMITTED", s)
	}
	if !strings.Contains(s, "repo_repair_audit") || !strings.Contains(s, "repo_repair_row_audit") {
		t.Errorf("stdout = %q, want both audit ledgers named", s)
	}
	got := storedRepairReposBySession(t, path, 2)
	if got["sess-a"] != "acme/app" || got["sess-b"] != "acme/lib" {
		t.Errorf("stored repos = %v, want sess-a=acme/app sess-b=acme/lib", got)
	}
}

// TestRunRepairRepoCmd_RerunIsANoOp proves the acceptance bullet at the CLI
// level: the second identical --commit changes nothing and SAYS so, rather than
// printing an audit line for a ledger that gained no rows.
func TestRunRepairRepoCmd_RerunIsANoOp(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	args := []string{"--db", path, "--developer", "alice", "--commit", "--map", "sess-a=acme/app"}

	var out, errb bytes.Buffer
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("first run exit = %d; stderr=%s", code, errb.String())
	}
	first := storedRepairRepos(t, path, 1)

	out.Reset()
	errb.Reset()
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("second run exit = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "nothing to repair") {
		t.Errorf("stdout = %q, want the no-op message on a re-run", s)
	}
	if strings.Contains(s, "repair_id") {
		t.Errorf("stdout = %q, must NOT report an audit id when no audit row was written", s)
	}
	for id, repo := range storedRepairRepos(t, path, 1) {
		if repo != first[id] {
			t.Errorf("re-run mutated row %d: %q -> %q", id, first[id], repo)
		}
	}
}

// TestRunRepairRepoCmd_ZeroChangeReasonIsNotAFalseGreen is a false-green guard
// found by running the REAL BINARY, not by a unit test: a mapping that disagrees
// with every stored repo repairs nothing, and the first draft described that
// outcome as "re-running a completed repair is a no-op". An operator whose
// mapping is wrong would read that as "already done" and stop looking.
//
// GUARD COVERAGE: collapse whyNothingChanged to a single unconditional string
// and this test fails.
func TestRunRepairRepoCmd_ZeroChangeReasonIsNotAFalseGreen(t *testing.T) {
	// Everything is already qualified, and the mapping disagrees with it.
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-real", repo: "acme/app", cost: 100},
	})
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--commit", "--map", "sess-real=evil/hijack"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "disagrees with rows that are already qualified") {
		t.Errorf("stdout = %q, want the zero-change summary to NAME the disagreement rather than calling it a completed repair", s)
	}
	if !strings.Contains(s, "WARNING") {
		t.Errorf("stdout = %q, want the loud WARNING block too", s)
	}

	// Control arm: a genuine completed-repair re-run DOES get the benign message,
	// so the assertion above is about the disagreement and not about the phrase
	// being absent everywhere.
	clean := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	args := []string{"--db", clean, "--developer", "alice", "--commit", "--map", "sess-a=acme/app"}
	out.Reset()
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("control first run exit = %d; stderr=%s", code, errb.String())
	}
	out.Reset()
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("control second run exit = %d; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "re-running a completed repair is a no-op") {
		t.Errorf("control stdout = %q, want the benign completed-repair message", out.String())
	}
	if strings.Contains(out.String(), "disagrees") {
		t.Errorf("control stdout = %q, must not mention a disagreement when there is none", out.String())
	}
}

// TestRunRepairRepoCmd_OnlySessionlessRowsLeftIsBenign proves the OTHER benign
// zero-change outcome is not mislabelled either: when the only unqualified rows
// left carry no session, telling the operator to "check your mapping keys" sends
// them hunting for a line they can never write.
func TestRunRepairRepoCmd_OnlySessionlessRowsLeftIsBenign(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "", cost: 200}, // structurally unresolvable
	})
	args := []string{"--db", path, "--developer", "alice", "--commit", "--map", "sess-a=acme/app"}
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("first run exit = %d; stderr=%s", code, errb.String())
	}
	out.Reset()
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("second run exit = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "no session id, which no mapping can repair") {
		t.Errorf("stdout = %q, want the session-less rows named as the benign reason", s)
	}
	if strings.Contains(s, "check --developer and your mapping keys") {
		t.Errorf("stdout = %q, must not send the operator hunting for a mapping line that cannot exist", s)
	}
}

// TestRunRepairRepoCmd_MapFileParsed proves --map-file reads pairs from disk and
// correctly ignores blank lines and whole-line comments.
func TestRunRepairRepoCmd_MapFileParsed(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-b", cost: 200},
	})
	mapPath := filepath.Join(t.TempDir(), "map.txt")
	body := "# derived from ~/.claude/projects\n\nsess-a=Acme/App\n   sess-b = acme/lib   \n"
	if err := os.WriteFile(mapPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write map file: %v", err)
	}

	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--commit", "--map-file", mapPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	got := storedRepairReposBySession(t, path, 2)
	// "Acme/App" must land canonicalized, or the repaired row would carry a join
	// key the webhook and the collector could never produce (#231).
	if got["sess-a"] != "acme/app" || got["sess-b"] != "acme/lib" {
		t.Errorf("stored repos = %v, want sess-a=acme/app sess-b=acme/lib", got)
	}
}

// TestRunRepairRepoCmd_MapFileMissingRejected proves an unreadable --map-file is
// a hard error, not a silently empty mapping.
func TestRunRepairRepoCmd_MapFileMissingRejected(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	missing := filepath.Join(t.TempDir(), "nope.txt")
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map-file", missing}, &out, &errb); code != 1 {
		t.Errorf("exit code = %d, want 1 (missing --map-file)", code)
	}
	if !strings.Contains(errb.String(), "--map-file") {
		t.Errorf("stderr = %q, want it to name --map-file", errb.String())
	}
}

// TestLoadRepairMapping_Table drives the parser directly across its rejections
// and its one tolerated duplicate. Each rejection is a guard whose deletion this
// test kills; the accepted cases are the control arms that stop the parser from
// simply refusing everything.
func TestLoadRepairMapping_Table(t *testing.T) {
	cases := []struct {
		name     string
		pairs    []string
		wantErr  string // substring; "" = must succeed
		wantSlug map[string]string
	}{
		{
			name:     "canonicalizes a hand-typed slug",
			pairs:    []string{"sess-a=Tiermetric/Tier.git"},
			wantSlug: map[string]string{"sess-a": "tiermetric/tier"},
		},
		{
			name:     "duplicate agreeing entry is tolerated",
			pairs:    []string{"sess-a=acme/app", "sess-a=Acme/App"},
			wantSlug: map[string]string{"sess-a": "acme/app"},
		},
		{
			// Last-wins would resolve this arbitrarily and invisibly, and the
			// wrong arbitrary answer is the exact mis-attribution this command
			// exists to fix.
			name:    "duplicate DISAGREEING entry is refused",
			pairs:   []string{"sess-a=acme/app", "sess-a=acme/lib"},
			wantErr: "mapped to both",
		},
		{
			name:    "missing '=' is refused",
			pairs:   []string{"sess-a acme/app"},
			wantErr: "expected <session-id>=<owner/repo>",
		},
		{
			name:    "empty session is refused",
			pairs:   []string{"=acme/app"},
			wantErr: "expected <session-id>=<owner/repo>",
		},
		{
			name:    "single-segment slug is refused",
			pairs:   []string{"sess-a=app"},
			wantErr: "not a canonical owner/repo slug",
		},
		{
			// A caller must not be able to forge the reserved sentinel back into
			// the column — the same discipline the ingest API applies.
			name:    "the reserved sentinel is refused",
			pairs:   []string{"sess-a=" + repoid.Unqualified},
			wantErr: "not a canonical owner/repo slug",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadRepairMapping(tc.pairs, "")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("loadRepairMapping(%v) succeeded, want error containing %q", tc.pairs, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadRepairMapping(%v): %v", tc.pairs, err)
			}
			if len(got) != len(tc.wantSlug) {
				t.Fatalf("got %v, want %v", got, tc.wantSlug)
			}
			for k, v := range tc.wantSlug {
				if got[k] != v {
					t.Errorf("mapping[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestRunRepairRepoCmd_UnknownDeveloperReportsCheckFlag proves a typo'd
// --developer is reported as "examined nothing (check --developer)" rather than
// masquerading as a successful no-op repair.
func TestRunRepairRepoCmd_UnknownDeveloperReportsCheckFlag(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alicia", "--commit", "--map", "sess-a=acme/app"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "check --developer") {
		t.Errorf("stdout = %q, want the check --developer message", s)
	}
	if strings.Contains(s, "nothing to repair") {
		t.Errorf("stdout = %q, must NOT report the benign no-op message when the developer matched no rows at all", s)
	}
	if got := storedRepairReposBySession(t, path, 1)["sess-a"]; got != repoid.Unqualified {
		t.Errorf("alice's row was mutated by a repair scoped to alicia: %q", got)
	}
}

// TestRunRepairRepoCmd_ConflictIsWarnedAndNotApplied is the CLI-level control
// arm for the most important invariant: a row already carrying a real repo is
// not modified even when the mapping disagrees, and the report says so LOUDLY
// instead of silently skipping it.
func TestRunRepairRepoCmd_ConflictIsWarnedAndNotApplied(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-real", repo: "acme/already-right", cost: 100},
		{developer: "alice", session: "sess-blank", cost: 200},
	})
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{
		"--db", path, "--developer", "alice", "--commit",
		"--map", "sess-real=evil/hijack", "--map", "sess-blank=acme/lib",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "WARNING") || !strings.Contains(s, "DISAGREES") {
		t.Errorf("stdout = %q, want a loud disagreement warning", s)
	}
	if !strings.Contains(s, "evil/hijack") || !strings.Contains(s, "acme/already-right") {
		t.Errorf("stdout = %q, want it to name BOTH the stored repo and what the mapping claimed", s)
	}
	got := storedRepairReposBySession(t, path, 2)
	if got["sess-real"] != "acme/already-right" {
		t.Errorf("already-qualified row repo = %q, want acme/already-right — a real repo is never overwritten", got["sess-real"])
	}
	// Control arm: the unqualified row in the SAME run WAS repaired, so the
	// refusal above is targeted, not a blanket abort.
	if got["sess-blank"] != "acme/lib" {
		t.Errorf("unqualified row repo = %q, want acme/lib", got["sess-blank"])
	}
}

// TestRunRepairRepoCmd_UnresolvedRowsAreReported proves rows the mapping cannot
// resolve are left alone and NAMED, and that the structurally unresolvable
// no-session rows are called out separately — an operator hunting for a missing
// --map line must not waste time on rows no mapping could ever fix.
func TestRunRepairRepoCmd_UnresolvedRowsAreReported(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-unmapped", cost: 200},
		{developer: "alice", session: "", cost: 300}, // proxy-shaped
	})
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--commit", "--map", "sess-a=acme/app"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "UNRESOLVED") {
		t.Errorf("stdout = %q, want an UNRESOLVED section", s)
	}
	if !strings.Contains(s, "sess-unmapped") {
		t.Errorf("stdout = %q, want the unmapped session NAMED (reported, not guessed)", s)
	}
	if !strings.Contains(s, "no session id at all") {
		t.Errorf("stdout = %q, want the session-less rows called out separately", s)
	}
	got := storedRepairReposBySession(t, path, 3)
	if got["sess-unmapped"] != repoid.Unqualified || got[""] != repoid.Unqualified {
		t.Errorf("unresolved rows were mutated: %v", got)
	}
	// Control arm: the resolvable row in the same run WAS repaired.
	if got["sess-a"] != "acme/app" {
		t.Errorf("mapped row repo = %q, want acme/app", got["sess-a"])
	}
}

// TestRunRepairRepoCmd_UnresolvedListIsTruncatedWithAnAccurateRemainder covers
// the report's truncation arithmetic, which is easy to get silently wrong by
// one: the no-session bucket lives in the same slice but is summarized on its own
// line and never enumerated, so it must be excluded from the "... and N more"
// remainder. An off-by-one here would misstate how much an operator still has to
// fix — the exact number they are reading the report for.
func TestRunRepairRepoCmd_UnresolvedListIsTruncatedWithAnAccurateRemainder(t *testing.T) {
	// 25 distinct unmapped sessions + one session-less row, against a cap of 20.
	const unmapped = 25
	seeds := make([]repairSeed, 0, unmapped+2)
	seeds = append(seeds, repairSeed{developer: "alice", session: "sess-mapped", cost: 1})
	for i := 0; i < unmapped; i++ {
		// Zero-padded so the sorted report order is stable and readable.
		seeds = append(seeds, repairSeed{developer: "alice", session: fmt.Sprintf("sess-u%02d", i), cost: 1})
	}
	seeds = append(seeds, repairSeed{developer: "alice", session: "", cost: 1})
	path := seedRepairDB(t, seeds)

	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-mapped=acme/app"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()

	// Exactly maxReportedBuckets enumerated lines, then an accurate remainder.
	listed := strings.Count(s, "    unmapped session ")
	if listed != maxReportedBuckets {
		t.Errorf("enumerated %d unmapped sessions, want the cap of %d", listed, maxReportedBuckets)
	}
	wantRemainder := fmt.Sprintf("... and %d more unmapped session(s)", unmapped-maxReportedBuckets)
	if !strings.Contains(s, wantRemainder) {
		t.Errorf("stdout = %q, want the remainder line %q (the session-less bucket must NOT be counted — it is summarized separately and never enumerated)", s, wantRemainder)
	}
	// The COUNTS above the list are never truncated: 26 rows stay unqualified.
	if !strings.Contains(s, fmt.Sprintf("UNRESOLVED: %d unqualified row(s)", unmapped+1)) {
		t.Errorf("stdout = %q, want the complete unresolved COUNT even though the list is truncated", s)
	}
}

// TestRunRepairRepoCmd_HelpIsSuccess proves -h exits 0 (flag.ContinueOnError +
// errors.Is(err, flag.ErrHelp)), matching every other subcommand.
func TestRunRepairRepoCmd_HelpIsSuccess(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"-h"}, &out, &errb); code != 0 {
		t.Errorf("exit code = %d, want 0 (an explicit help request is success)", code)
	}
}

// TestDispatch_RepairRepoRegistered proves the subcommand is reachable through
// dispatch — the convention requires registration in BOTH dispatch and
// printUsage, and the usage half is asserted in TestDispatch_Help.
func TestDispatch_RepairRepoRegistered(t *testing.T) {
	// ⚠️ An explicit --db even though this run never reaches store.Open. Without
	// it the flag defaults to defaultDBPath() — the operator's REAL ~/.tier/tier.db
	// — and the test is safe only because the --developer check happens to precede
	// the os.Stat. Reorder those two checks and this test would start opening
	// production data. Not a risk worth leaving on a tripwire.
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 1}})
	var out, errb bytes.Buffer
	code := dispatch([]string{"repair-repo", "--db", path}, &out, &errb)
	if code != 1 {
		t.Fatalf("dispatch exit = %d, want 1 (missing --developer)", code)
	}
	if strings.Contains(errb.String(), "unknown command") {
		t.Errorf("stderr = %q, want repair-repo to be a registered command", errb.String())
	}
	if !strings.Contains(errb.String(), "--developer is required") {
		t.Errorf("stderr = %q, want dispatch to have reached runRepairRepoCmd", errb.String())
	}
}

// TestPrintRepairRepoResult_ReportsTheActualNumbers closes the suite's largest
// hole. Every other CLI test here asserts that a substring APPEARS; none asserted
// that the numbers next to it are the ones the store computed. Eleven separate
// mutations that made the report lie — hardcoding every count and sum to zero —
// survived the entire suite, and the worst printed
// "kept evil/hijack" on the disagreement line, telling the operator the hijack
// had WON while the row was in fact untouched.
//
// So this drives printRepairRepoResult directly with a hand-built result and
// asserts whole lines.
//
// GUARD COVERAGE: replace any count, sum, or slug in printRepairRepoResult with a
// constant and this test fails.
func TestPrintRepairRepoResult_ReportsTheActualNumbers(t *testing.T) {
	res := store.RepairRepoResult{
		Developer: "alice",
		Committed: true,
		RepairID:  "deadbeef",
		// The header's "N session(s) mapped" is read from the RESULT, not from a
		// separate printer argument, so a fixture must state it here — which also
		// means every fixture describes a state RepairRepo could actually return.
		MappedSessionCount:          7,
		ScannedRowCount:             9,
		AlreadyQualifiedRowCount:    3,
		UnqualifiedRowCount:         6,
		ChangedRowCount:             4,
		ChangedCostMicroSum:         1_500_000,
		UnresolvedRowCount:          2,
		UnresolvedNoSessionRowCount: 1,
		ConflictRowCount:            1,
		ByRepo: []store.RepairRepoDelta{
			{FromRepo: "unqualified", Repo: "acme/app", RowCount: 3, CostMicroSum: 1_200_000, SessionCount: 2},
			{FromRepo: "unqualified", Repo: "acme/lib", RowCount: 1, CostMicroSum: 300_000, SessionCount: 1},
		},
		Unresolved: []store.RepairRepoUnresolved{
			{SessionID: "", RowCount: 1, CostMicroSum: 777},
			{SessionID: "sess-unmapped", RowCount: 1, CostMicroSum: 4242},
		},
		Conflicts: []store.RepairRepoConflict{
			{SessionID: "sess-real", StoredRepo: "acme/kept", MappedRepo: "evil/hijack", RowCount: 1, CostMicroSum: 55},
		},
	}

	var out bytes.Buffer
	printRepairRepoResult(&out, res)
	s := out.String()

	for _, want := range []string{
		// header: the developer and the SUPPLIED mapping size. The developer is
		// quoted because it goes through logsafe.Str — see
		// TestPrintRepairRepoResult_DeveloperIdCannotForgeAReportLine.
		"repair-repo COMMITTED: developer \"alice\", 7 session(s) mapped",
		// the three-way split of what was examined
		"examined 9 row(s): 3 already qualified (never touched), 6 unqualified",
		// what moved, in rows and in money
		"4 row(s) repaired, carrying 1500000 micro-USD (1.500000 USD)",
		// per-repo: from, to, rows, DISTINCT sessions, spend
		"    unqualified -> acme/app: 3 row(s) across 2 session(s), 1200000 micro-USD",
		"    unqualified -> acme/lib: 1 row(s) across 1 session(s), 300000 micro-USD",
		// unresolved: the total on its own line, then ONE line per subset, each
		// naming the subset it describes. The subsets must add up to the total in
		// both rows (1+1=2) and spend (777+4242=5019).
		"UNRESOLVED: 2 unqualified row(s) stay unqualified, carrying 5019 micro-USD in total",
		"    1 of them carries no session id at all (proxy/poller rows), carrying 777 micro-USD that NO mapping can ever repair",
		"    1 of them carries a session id your mapping does not name, carrying 4242 micro-USD",
		// Session ids are operator/producer-supplied strings and go through
		// logsafe.Str (#321), so they render quoted.
		"      unmapped session \"sess-unmapped\": 1 row(s), 4242 micro-USD",
		// 🔴 the disagreement line must say the STORED repo was kept, never the
		// mapped one — printing "kept evil/hijack" would tell the operator the
		// hijack won while the row was untouched.
		"    session \"sess-real\": stored \"acme/kept\", mapping says evil/hijack (1 row(s), 55 micro-USD, kept \"acme/kept\")",
		// the audit line's two counts and the id
		"audit: 2 row(s) to repo_repair_audit + 4 per-row before-image(s) to repo_repair_row_audit (repair_id deadbeef)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report is missing or misstates:\n  want line: %s\n  got report:\n%s", want, s)
		}
	}
	// The no-session bucket is summarized on its own line and must never also be
	// enumerated as an "unmapped session" the operator could go fix.
	if strings.Contains(s, "unmapped session :") {
		t.Errorf("report enumerated the no-session bucket as an unmapped session:\n%s", s)
	}
}

// TestPrintRepairRepoResult_PluraliseCarry pins the singular/plural on the
// unresolved line. Small, but "1 row(s) carry" next to a set of numbers an
// operator is about to act on is the kind of wrongness that makes them distrust
// the numbers too.
func TestPrintRepairRepoResult_PluraliseCarry(t *testing.T) {
	base := store.RepairRepoResult{
		Developer: "alice", ScannedRowCount: 2, UnqualifiedRowCount: 2, MappedSessionCount: 1,
		UnresolvedRowCount: 2, Unresolved: []store.RepairRepoUnresolved{{SessionID: ""}},
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{1, "1 of them carries no session id"},
		{2, "2 of them carry no session id"},
	} {
		var out bytes.Buffer
		r := base
		r.UnresolvedNoSessionRowCount = tc.n
		printRepairRepoResult(&out, r)
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("n=%d: report = %q, want it to contain %q", tc.n, out.String(), tc.want)
		}
	}
}

// TestRunRepairRepoCmd_DryRunAlsoReportsConflictsAndUnresolved proves the two
// operator-facing warnings are not commit-only. A dry run is where an operator
// discovers their mapping is wrong — reporting a disagreement only after the
// change had been applied would defeat the point of having a dry run.
func TestRunRepairRepoCmd_DryRunAlsoReportsConflictsAndUnresolved(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-real", repo: "acme/kept", cost: 100},
		{developer: "alice", session: "sess-unmapped", cost: 200},
	})
	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-real=evil/hijack"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "DRY RUN") {
		t.Fatalf("stdout = %q, want a DRY RUN header", s)
	}
	if !strings.Contains(s, "WARNING") || !strings.Contains(s, "evil/hijack") {
		t.Errorf("stdout = %q, want the disagreement reported on the DRY RUN — that is when an operator can still fix the mapping", s)
	}
	if !strings.Contains(s, "sess-unmapped") {
		t.Errorf("stdout = %q, want unresolved sessions named on the dry run too", s)
	}
	// And nothing moved.
	for id, repo := range storedRepairRepos(t, path, 2) {
		if repo != "acme/kept" && repo != repoid.Unqualified {
			t.Errorf("dry run mutated row %d to %q", id, repo)
		}
	}
}

// TestRunRepairRepoCmd_MapFileOversizeLineIsAHardError pins the "surfaced, not
// truncated" claim on bufio's scan error. A line beyond bufio's 64 KiB buffer
// stops the scanner; without the sc.Err() check every REMAINING mapping line is
// dropped silently, producing a partial repair reported as a clean success —
// this command's cardinal sin.
//
// GUARD COVERAGE: delete the sc.Err() check in loadRepairMapping and this fails.
func TestRunRepairRepoCmd_MapFileOversizeLineIsAHardError(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-b", cost: 200},
	})
	mapPath := filepath.Join(t.TempDir(), "huge.txt")
	// ⚠️ ORDER IS LOAD-BEARING. A VALID entry comes FIRST, so that swallowing the
	// scan error yields a NON-empty but INCOMPLETE mapping. Put the oversize line
	// first instead and the mapping comes back empty, the CLI's separate
	// empty-mapping refusal fires, and the test passes for the wrong reason —
	// which is exactly what the first draft of this test did.
	body := "sess-a=acme/app\n" +
		"sess-huge=" + strings.Repeat("a", 100*1024) + "/repo\n" +
		"sess-b=acme/lib\n"
	if err := os.WriteFile(mapPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write map file: %v", err)
	}

	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--commit", "--map-file", mapPath}, &out, &errb); code != 1 {
		t.Errorf("exit code = %d, want 1 — a mapping the parser could not read in full must never produce a PARTIAL repair reported as success", code)
	}
	if !strings.Contains(errb.String(), "--map-file") {
		t.Errorf("stderr = %q, want it to name --map-file", errb.String())
	}
	// Neither the entry the parser DID read nor the one it never reached may be
	// applied: a truncated mapping is not a smaller mapping, it is an unknown one.
	got := storedRepairReposBySession(t, path, 2)
	if got["sess-a"] != repoid.Unqualified || got["sess-b"] != repoid.Unqualified {
		t.Errorf("stored repos = %v, want both untouched — a partially-read mapping must repair NOTHING", got)
	}
}

// TestLoadRepairMapping_FileErrorsNameFileAndLine covers the --map-file branch of
// the parser, which TestLoadRepairMapping_Table never reaches (it only drives the
// --map pairs). The origin string is the whole point of that branch: it is what
// points an operator at the exact line to fix in a file of thousands.
func TestLoadRepairMapping_FileErrorsNameFileAndLine(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "map.txt")
	// Comments and blanks must NOT consume line numbers wrongly — the bad entry is
	// on physical line 4.
	body := "# header\n\nsess-ok=acme/app\nsess-bad=notaslug\n"
	if err := os.WriteFile(mapPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write map file: %v", err)
	}
	_, err := loadRepairMapping(nil, mapPath)
	if err == nil {
		t.Fatal("loadRepairMapping accepted a malformed slug from a file")
	}
	if !strings.Contains(err.Error(), "map.txt:4") {
		t.Errorf("error = %v, want it to name the file AND the physical line (map.txt:4)", err)
	}
}

// TestLoadRepairMapping_CrossSourceDisagreementIsRefused proves the ambiguity
// check spans BOTH input sources. A --map pair and a --map-file line naming
// different repositories for one session is the same ambiguity as two file lines,
// and letting one silently win is the mis-attribution this command exists to fix.
func TestLoadRepairMapping_CrossSourceDisagreementIsRefused(t *testing.T) {
	mapPath := filepath.Join(t.TempDir(), "map.txt")
	if err := os.WriteFile(mapPath, []byte("sess-a=acme/lib\n"), 0o600); err != nil {
		t.Fatalf("write map file: %v", err)
	}
	_, err := loadRepairMapping([]string{"sess-a=acme/app"}, mapPath)
	if err == nil {
		t.Fatal("a --map pair and a --map-file line disagreed about one session and were silently merged")
	}
	if !strings.Contains(err.Error(), "mapped to both") {
		t.Errorf("error = %v, want the ambiguity named", err)
	}

	// Control arm: agreeing sources across the two inputs are fine.
	got, err := loadRepairMapping([]string{"sess-a=Acme/App"}, filepath.Join(t.TempDir(), "none.txt"))
	if err == nil && got["sess-a"] != "acme/app" {
		t.Errorf("mapping = %v, want the canonicalized acme/app", got)
	}
}

// TestLoadRepairMapping_TrailingHashIsNotAComment pins a deliberate parser
// decision: only WHOLE-line comments are stripped. A "#" cannot appear in a
// canonical slug, so treating a trailing one as a comment would quietly turn a
// malformed line into a valid-looking one instead of failing on it.
func TestLoadRepairMapping_TrailingHashIsNotAComment(t *testing.T) {
	mapPath := filepath.Join(t.TempDir(), "map.txt")
	if err := os.WriteFile(mapPath, []byte("sess-a=acme/app # my repo\n"), 0o600); err != nil {
		t.Fatalf("write map file: %v", err)
	}
	if _, err := loadRepairMapping(nil, mapPath); err == nil {
		t.Error("a trailing # was treated as a comment; it must fail loudly instead of silently accepting a half-parsed slug")
	}
}

// TestRunRepairRepoCmd_AliasGapReachesTheSummaryLine is the sibling of
// TestRunRepairRepoCmd_ZeroChangeReasonIsNotAFalseGreen, and exists because the
// alias case was given the OPPOSITE treatment to the conflict case for no reason
// anyone could name.
//
// The alias NOTE prints early, but whyNothingChanged's summary is the LAST line
// an operator reads, and unappended it ended the run with "re-running a completed
// repair is a no-op" while a sibling identity of the same human still held
// unattributed spend. The conflict branch already appends rather than substitutes
// precisely so an ADDITIONAL fact cannot hide behind a benign reason; an alias gap
// is the same class of fact, one identity to the left.
//
// GUARD COVERAGE: delete the AliasUnqualifiedRowCount append in whyNothingChanged
// and this test fails.
func TestRunRepairRepoCmd_AliasGapReachesTheSummaryLine(t *testing.T) {
	// "alice" is fully qualified, so the run is a genuine completed-repair re-run
	// by its own accounting — the benign branch fires. Her alias "a.smith" still
	// carries an unqualified row the repair never examined.
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-done", repo: "acme/app", cost: 100},
		{developer: "a.smith", session: "sess-orphan", cost: 250},
	})
	seedRepairAlias(t, path, "a.smith", "alice")

	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-done=acme/app"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "sibling identity") {
		t.Errorf("stdout = %q, want the SUMMARY line to carry the alias gap — the NOTE alone is not enough, the summary is the last thing read", s)
	}
	// The benign reason must still be there: appended, never substituted. If the
	// append silently replaced the reason, the operator loses the diagnosis of why
	// nothing changed for the identity they actually named.
	if !strings.Contains(s, "re-running a completed repair is a no-op") {
		t.Errorf("stdout = %q, want the benign reason RETAINED alongside the alias gap, not replaced by it", s)
	}

	// Control arm: the same shape with no alias joined must NOT claim a sibling.
	// Without this, an unconditional append would pass the assertion above.
	clean := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-done", repo: "acme/app", cost: 100},
		{developer: "a.smith", session: "sess-orphan", cost: 250},
	})
	out.Reset()
	if code := runRepairRepoCmd([]string{"--db", clean, "--developer", "alice", "--map", "sess-done=acme/app"}, &out, &errb); code != 0 {
		t.Fatalf("control exit = %d; stderr=%s", code, errb.String())
	}
	if strings.Contains(out.String(), "sibling identity") {
		t.Errorf("control stdout = %q, must not claim a sibling identity when developer_alias joins nothing", out.String())
	}
}

// seedRepairAlias joins two raw producer ids in developer_alias (#125) on an
// already-seeded fixture, so a test can build the two-identities-one-human shape
// the repair cannot span.
func seedRepairAlias(t *testing.T, path, alias, canonical string) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen for alias: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.UpsertDeveloperAlias(context.Background(), alias, canonical); err != nil {
		t.Fatalf("upsert alias %s -> %s: %v", alias, canonical, err)
	}
}

// TestRunRepairRepoCmd_EmptyMapValueIsAHardError pins the one malformed --map
// form that used to vanish in silence.
//
// repeatableStringSlice.Set returned nil without appending on an empty value, so
// `--map ”` produced no entry, no message, and exit 0 — while EVERY other
// malformed form (`--map foo`, `--map =x`, `--map x=`) is a hard error naming the
// bad entry. A shell that ate a quote, or an unset variable in
// `--map "$SESSION=$SLUG"`, therefore silently shrank the mapping, and the repair
// went on to report a partial run as a clean one. Consistency here is not tidiness:
// it is the difference between a diagnosis and a false green.
func TestRunRepairRepoCmd_EmptyMapValueIsAHardError(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})

	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", ""}, &out, &errb)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; an empty --map must fail like every other malformed form. stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	// 🔴 THE MESSAGE ASSERTION IS THE LOAD-BEARING HALF — do not "simplify" it
	// away. Under the OLD silently-dropping Set, the mapping ends up empty and the
	// command already exits 1 via the "no mapping supplied" refusal, so the exit
	// code alone does not discriminate. Only the message distinguishes "your flag
	// was rejected" from "you passed no mapping at all", which are different
	// operator mistakes with different fixes.
	if !strings.Contains(errb.String(), "empty value") {
		t.Errorf("stderr = %q, want it to name the empty value (a bare usage dump leaves the operator guessing which flag was wrong)", errb.String())
	}
	// Nothing ran, so nothing moved.
	if got := storedRepairReposBySession(t, path, 1); got["sess-a"] != repoid.Unqualified {
		t.Errorf("row was mutated by a rejected invocation: %v", got)
	}

	// 🔴 CONTROL: the same command with a real value must still succeed, so the
	// rejection above is the EMPTY value and not a broken --map flag.
	var out2, errb2 bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-a=acme/app"}, &out2, &errb2); code != 0 {
		t.Fatalf("control: a well-formed --map returned %d; stderr=%s", code, errb2.String())
	}
}

// TestRepeatableStringSlice_SetRejectsEmptyValue is the unit-level half of the
// test above. The type backs --map, --watch-repo, --trusted-proxy-cidr and
// --repo-slug, and an empty occurrence is never a meaningful instruction for any
// of them — so the rejection belongs in the type, not in one subcommand.
func TestRepeatableStringSlice_SetRejectsEmptyValue(t *testing.T) {
	var s repeatableStringSlice
	if err := s.Set(""); err == nil {
		t.Error("Set(\"\") returned nil — an empty occurrence is silently dropped, which is the one malformed form that produces no diagnostic")
	}
	if len(s) != 0 {
		t.Errorf("Set(\"\") appended %q; a rejected value must not land in the slice", s)
	}
	// Control: a real value still appends, and repeats accumulate in order.
	for _, v := range []string{"a", "b"} {
		if err := s.Set(v); err != nil {
			t.Fatalf("Set(%q) = %v, want nil", v, err)
		}
	}
	if got := s.String(); got != "a,b" {
		t.Errorf("String() = %q, want \"a,b\"", got)
	}
}

// TestRunRepairRepoCmd_UnmatchedMappingEntriesAreReported drives the whole binary
// through the command's most likely failure mode: a mapping whose entries match
// nothing. Every other number in the report describes the database, so before
// this line a stale export and a healthy history printed the same thing.
func TestRunRepairRepoCmd_UnmatchedMappingEntriesAreReported(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})

	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{
		"--db", path, "--developer", "alice",
		"--map", "sess-a=acme/app",
		"--map", "sess-stale=acme/app",
		"--map", "sess-typo=acme/app",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "MAPPING: 2 of your 3 mapping entries matched NO row") {
		t.Errorf("stdout = %q, want the 'N of your M' mapping-gap line", s)
	}
	for _, want := range []string{`unmatched mapping entry "sess-stale"`, `unmatched mapping entry "sess-typo"`} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout = %q, want it to name the unmatched entry: %s", s, want)
		}
	}
	// 🔴 CONTROL: the entry that DID match is not listed. Naming it would send the
	// operator to fix a line that is already correct.
	if strings.Contains(s, `unmatched mapping entry "sess-a"`) {
		t.Errorf("stdout = %q, listed a mapping entry that matched a row", s)
	}
}

// TestRunRepairRepoCmd_NoMappingGapLineWhenEveryEntryMatched is the control arm:
// a clean run must not print the MAPPING section at all. A gap line that always
// appears is a gap line an operator learns to ignore.
func TestRunRepairRepoCmd_NoMappingGapLineWhenEveryEntryMatched(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-a=acme/app"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	if strings.Contains(out.String(), "MAPPING:") {
		t.Errorf("stdout = %q, want NO mapping-gap section when every entry matched", out.String())
	}
	// 🔴 POSITIVE ANCHOR. Everything above is an absence assertion, and absence is
	// also what a stubbed printer, a report routed to stderr, or an early return
	// produces. Without this the control cannot tell "correctly suppressed" from
	// "nothing printed at all" — and it exists precisely to be that discriminator.
	if !strings.Contains(out.String(), "1 row(s) would be repaired") {
		t.Errorf("stdout = %q, want the report to have actually been printed", out.String())
	}
}

// TestRunRepairRepoCmd_AliasIdentityGapIsReported drives the measured
// partial-repair scenario end to end: 7 rows for one human on one session, split
// 3/4 across two raw producer ids joined by developer_alias. Repairing one
// identity fixes 3 and leaves 4 — with no UNRESOLVED bucket and no WARNING,
// because from the repair's point of view nothing went wrong.
//
// The NOTE is the only thing standing between that and an operator concluding
// the job is done.
func TestRunRepairRepoCmd_AliasIdentityGapIsReported(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "devlead", session: "sess-a", cost: 10},
		{developer: "devlead", session: "sess-a", cost: 20},
		{developer: "devlead", session: "sess-a", cost: 30},
		{developer: "dl", session: "sess-a", cost: 40},
		{developer: "dl", session: "sess-a", cost: 50},
		{developer: "dl", session: "sess-a", cost: 60},
		{developer: "dl", session: "sess-a", cost: 70},
	})
	seedRepairAlias(t, path, "dl", "devlead")

	var out, errb bytes.Buffer
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "devlead", "--commit", "--map", "sess-a=acme/app"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, `NOTE: developer "devlead" has alias(es) "dl" carrying 4 unqualified row(s) this run did NOT examine`) {
		t.Errorf("stdout = %q, want the alias NOTE naming the sibling identity and its unrepaired row count", s)
	}
	if !strings.Contains(s, "--developer <alias>") {
		t.Errorf("stdout = %q, want the NOTE to state the remedy (re-run per stored identity), not just the problem", s)
	}
	// The scope really did stay exact: 3 repaired, 4 left alone.
	// Leading space: "3 row(s) repaired" is a substring of "13 row(s) repaired".
	if !strings.Contains(s, " 3 row(s) repaired") {
		t.Errorf("stdout = %q, want exactly the named identity's 3 rows repaired", s)
	}
	repos := storedRepairRepos(t, path, 7)
	unqualified := 0
	for _, r := range repos {
		if r == repoid.Unqualified {
			unqualified++
		}
	}
	if unqualified != 4 {
		t.Errorf("%d row(s) left unqualified, want 4 — the repair must NOT widen to the alias identity (that would re-attribute spend stored under another id)", unqualified)
	}
}

// TestRunRepairRepoCmd_NoAliasNoteWhenThereAreNoAliases is the control arm. A
// NOTE printed on every run is noise, and noise is how a real NOTE gets missed.
func TestRunRepairRepoCmd_NoAliasNoteWhenThereAreNoAliases(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 10},
		{developer: "bob", session: "sess-a", cost: 20},
	})
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-a=acme/app"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	if strings.Contains(out.String(), "has alias(es)") {
		t.Errorf("stdout = %q, want NO alias NOTE — bob is a different person, not an alias", out.String())
	}
	// 🔴 POSITIVE ANCHOR — see the note in the mapping-gap control above. An
	// absence assertion alone is satisfied by an empty report.
	if !strings.Contains(out.String(), "1 row(s) would be repaired") {
		t.Errorf("stdout = %q, want the report to have actually been printed", out.String())
	}
}

// TestRunRepairRepoCmd_DeveloperHelpStatesAliasScope pins the flag help, which is
// where the operator is standing when they make the choice the NOTE can only
// diagnose afterwards. "developer" reads as "the person"; the column holds a raw
// producer id, and the difference is a silently partial repair.
func TestRunRepairRepoCmd_DeveloperHelpStatesAliasScope(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"-h"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	help := errb.String()
	for _, want := range []string{"aliases are NOT resolved", "once per stored identity"} {
		if !strings.Contains(help, want) {
			t.Errorf("--developer help = %q, want it to contain %q", help, want)
		}
	}
}

// TestPrintRepairRepoResult_UnresolvedSubtotalsDoNotContradict pins the exact
// output a real-binary run produced, which read as self-contradictory:
//
//	UNRESOLVED: 1 unqualified row(s) stay unqualified — 0 of them carry no
//	  session id at all, carrying 0 micro-USD that NO mapping can ever repair
//	    unmapped session sess-z: 1 row(s), 3000 micro-USD
//
// The zeros described the no-session subset and were true, but they sat directly
// above a 3000 and there was nothing on the line to say which subset each number
// belonged to. An operator who cannot tell which number to believe stops
// believing all of them.
func TestPrintRepairRepoResult_UnresolvedSubtotalsDoNotContradict(t *testing.T) {
	res := store.RepairRepoResult{
		Developer:                   "alice",
		MappedSessionCount:          1,
		ScannedRowCount:             1,
		UnqualifiedRowCount:         1,
		UnresolvedRowCount:          1,
		UnresolvedNoSessionRowCount: 0,
		Unresolved:                  []store.RepairRepoUnresolved{{SessionID: "sess-z", RowCount: 1, CostMicroSum: 3000}},
	}
	var out bytes.Buffer
	printRepairRepoResult(&out, res)
	s := out.String()

	// The empty subset is not mentioned AT ALL, so no zero can sit beside the
	// non-zero it does not describe.
	if strings.Contains(s, "no session id at all") {
		t.Errorf("report = %q, want the no-session subset omitted entirely when it is empty", s)
	}
	// Both needles are ANCHORED, and the anchors are the point. "0 micro-USD" is a
	// substring of "3000 micro-USD", and "0 of them" is a substring of "10 of
	// them" / "20 of them" — either bare form would fail on CORRECT output the
	// moment a fixture grew. The line is indented, so a leading space pins the
	// digit boundary.
	if strings.Contains(s, "carrying 0 micro-USD") || strings.Contains(s, " 0 of them") {
		t.Errorf("report = %q, want no zero-valued subtotal next to the non-zero list below it", s)
	}
	// And the surviving subset states which subset it is, and agrees with the list.
	if !strings.Contains(s, "UNRESOLVED: 1 unqualified row(s) stay unqualified, carrying 3000 micro-USD in total") {
		t.Errorf("report = %q, want the total stated on its own line", s)
	}
	if !strings.Contains(s, "1 of them carries a session id your mapping does not name, carrying 3000 micro-USD") {
		t.Errorf("report = %q, want the named-session subtotal to say which subset it describes", s)
	}

	// 🔴 CONTROL: the mirror case — a session-less-only run must omit the
	// named-session subtotal instead, and must NOT print a bare "0 of them".
	res2 := store.RepairRepoResult{
		Developer:                   "alice",
		MappedSessionCount:          1,
		ScannedRowCount:             1,
		UnqualifiedRowCount:         1,
		UnresolvedRowCount:          1,
		UnresolvedNoSessionRowCount: 1,
		Unresolved:                  []store.RepairRepoUnresolved{{SessionID: "", RowCount: 1, CostMicroSum: 777}},
	}
	var out2 bytes.Buffer
	printRepairRepoResult(&out2, res2)
	s2 := out2.String()
	if !strings.Contains(s2, "1 of them carries no session id at all (proxy/poller rows), carrying 777 micro-USD that NO mapping can ever repair") {
		t.Errorf("report = %q, want the no-session subtotal when that IS the whole set", s2)
	}
	if strings.Contains(s2, "does not name") {
		t.Errorf("report = %q, want the named-session subtotal omitted when there are none", s2)
	}
}

// TestRunRepairRepoCmd_AliasNoteFiresWhenTheNamedIdentityOwnsNoRows covers the
// worst form of the alias trap, which the first fix MISSED.
//
// Rows are captured under one identity only; the operator names the OTHER one —
// the obvious mistake, since the two are the same human and developer_alias says
// so. The zero-row branch printed "check --developer" and returned, throwing away
// an answer the result was already carrying: RepairRepo populates the alias
// fields BEFORE the scan, so they survive a scan that matched nothing. Telling an
// operator to go find a name the report knows is the same false-green class this
// whole command is built to avoid.
func TestRunRepairRepoCmd_AliasNoteFiresWhenTheNamedIdentityOwnsNoRows(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "devlead", session: "sess-a", cost: 10},
		{developer: "devlead", session: "sess-a", cost: 20},
	})
	seedRepairAlias(t, path, "dl", "devlead")

	var out, errb bytes.Buffer
	// --developer dl owns ZERO rows of its own.
	code := runRepairRepoCmd([]string{"--db", path, "--developer", "dl", "--map", "sess-a=acme/app"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "owns no token_events rows at all") {
		t.Fatalf("fixture: stdout = %q, want the zero-row branch to be the one under test", s)
	}
	if !strings.Contains(s, `has alias(es) "devlead" carrying 2 unqualified row(s)`) {
		t.Errorf("stdout = %q, want the alias NOTE on the ZERO-ROW path — this is the exact moment 'check --developer' is asked and the answer is already in the result", s)
	}

	// 🔴 CONTROL: an unknown developer with NO alias must still get the plain
	// zero-row message and NO note, so the note is not simply always printed.
	var out2, errb2 bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "nobody", "--map", "sess-a=acme/app"}, &out2, &errb2); code != 0 {
		t.Fatalf("control exit code = %d; stderr=%s", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "owns no token_events rows at all") {
		t.Errorf("control stdout = %q, want the zero-row message", out2.String())
	}
	if strings.Contains(out2.String(), "has alias(es)") {
		t.Errorf("control stdout = %q, want NO alias note for an identity with no aliases", out2.String())
	}
}

// TestPrintRepairRepoResult_DeveloperIdCannotForgeAReportLine proves the
// logsafe.Str barrier is doing work rather than just adding quotes.
//
// --developer is operator-supplied and this report is routinely piped into a
// maintenance log, so a CR/LF in the identifier could otherwise emit a SECOND
// line that a human, or a line-oriented SIEM, reads as a genuine report row —
// including a forged "0 rows" that contradicts the real one. logsafe.Str STRIPS
// CR/LF (the transformation CodeQL's go/log-injection credits; %q alone is not
// credited) and then quotes, so the payload is glued inside one line's quotes.
func TestPrintRepairRepoResult_DeveloperIdCannotForgeAReportLine(t *testing.T) {
	res := store.RepairRepoResult{
		Developer:                "alice\n  UNRESOLVED: 0 unqualified row(s) stay unqualified, carrying 0 micro-USD in total",
		MappedSessionCount:       1,
		ScannedRowCount:          1,
		AlreadyQualifiedRowCount: 1,
	}
	var out bytes.Buffer
	printRepairRepoResult(&out, res)
	s := out.String()

	// The forged text survives as DATA on the header line; it must not start a
	// line of its own.
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.HasPrefix(line, "  UNRESOLVED:") {
			t.Errorf("a developer id forged a standalone report line:\n%s", s)
		}
	}
	if strings.Contains(s, "alice\n") {
		t.Errorf("report = %q, want the newline STRIPPED, not merely escaped — an escaped-but-present newline is not the barrier CodeQL credits", s)
	}
	// Control: the diagnostic still survives, so the barrier informs rather than
	// blanks. A sanitizer that ate the value would make the report useless.
	if !strings.Contains(s, "alice") {
		t.Errorf("report = %q, want the developer id still readable after sanitisation", s)
	}
}

// TestRunRepairRepoCmd_DryRunSaysRowsWouldBeRepaired pins the dry-run verb. The
// sentence is "%d row(s) %s", so the active "would repair" rendered as "3 row(s)
// would repair" — the rows doing the repairing. It is one word, in the first line
// an operator reads, and nothing asserted it either way.
func TestRunRepairRepoCmd_DryRunSaysRowsWouldBeRepaired(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-a", cost: 100}})
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-a=acme/app"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "1 row(s) would be repaired") {
		t.Errorf("stdout = %q, want the passive dry-run verb", out.String())
	}
	// Control: the committed form stays "repaired", not "would be repaired".
	var out2, errb2 bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--commit", "--map", "sess-a=acme/app"}, &out2, &errb2); code != 0 {
		t.Fatalf("commit exit code = %d; stderr=%s", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "1 row(s) repaired") || strings.Contains(out2.String(), "would be repaired") {
		t.Errorf("commit stdout = %q, want the past-tense committed verb", out2.String())
	}
}

// TestRunRepairRepoCmd_MappingGapListIsTruncatedWithAnAccurateRemainder covers
// the mapping-gap section's truncation arithmetic. Its sibling in the UNRESOLVED
// section earned its own test because the remainder is easy to get silently wrong
// by one; this one had none, and it is the line that tells an operator how big
// their remaining problem is. A wrong number here understates exactly the thing
// they are reading the report for.
func TestRunRepairRepoCmd_MappingGapListIsTruncatedWithAnAccurateRemainder(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{{developer: "alice", session: "sess-mapped", cost: 1}})

	const unmatched = 25
	args := []string{"--db", path, "--developer", "alice", "--map", "sess-mapped=acme/app"}
	for i := 0; i < unmatched; i++ {
		// Zero-padded so the sorted report order is stable and readable.
		args = append(args, "--map", fmt.Sprintf("sess-gone%02d=acme/app", i))
	}
	var out, errb bytes.Buffer
	if code := runRepairRepoCmd(args, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()

	if listed := strings.Count(s, "unmatched mapping entry "); listed != maxReportedBuckets {
		t.Errorf("enumerated %d unmatched entries, want the cap of %d", listed, maxReportedBuckets)
	}
	wantRemainder := fmt.Sprintf("... and %d more unmatched mapping entries", unmatched-maxReportedBuckets)
	if !strings.Contains(s, wantRemainder) {
		t.Errorf("stdout = %q, want the remainder line %q", s, wantRemainder)
	}
	// The COUNT above the list is never truncated: 25 of 26 matched nothing.
	if !strings.Contains(s, fmt.Sprintf("MAPPING: %d of your %d mapping entries", unmatched, unmatched+1)) {
		t.Errorf("stdout = %q, want the complete counts even though the list is truncated", s)
	}
}

// TestRunRepairRepoCmd_BOMInMapFileIsSelfDiagnosing is the end-to-end proof of
// the claim the whole MAPPING section is justified by.
//
// A --map-file saved by an editor that writes a UTF-8 BOM carries EF BB BF before
// the first session id. strings.TrimSpace does NOT strip U+FEFF, so entry one
// silently carries three invisible bytes and can never match a row — and on
// screen it is byte-for-byte indistinguishable from the correct id. Before the
// MAPPING section, the only symptom was a repair that quietly did less than the
// operator asked for.
//
// Two things are asserted, and the store-level test can prove neither: that the
// BOM really does survive the parser, and that the report renders it VISIBLY.
func TestRunRepairRepoCmd_BOMInMapFileIsSelfDiagnosing(t *testing.T) {
	path := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-b", cost: 200},
	})
	mapFile := filepath.Join(t.TempDir(), "sessions.map")
	// \xEF\xBB\xBF is a real UTF-8 BOM, written as bytes exactly as an editor would.
	body := "\xEF\xBB\xBFsess-a=acme/app\nsess-b=acme/lib\n"
	if err := os.WriteFile(mapFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write map file: %v", err)
	}

	var out, errb bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--commit", "--map-file", mapFile}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, errb.String())
	}
	s := out.String()

	// The BOM'd first entry matched nothing and is NAMED, with the invisible byte
	// made visible by logsafe.Str's %q — that escape IS the diagnosis.
	// The needle is BUILT, not typed: a literal U+FEFF in Go source is a compile
	// error, which is itself a fair measure of how invisible this byte is.
	wantBOMLine := "unmatched mapping entry " + strconv.Quote("\ufeffsess-a")
	if !strings.Contains(s, wantBOMLine) {
		t.Errorf("stdout = %q, want %q — the BOM'd entry named with U+FEFF rendered visibly; printed bare it is indistinguishable from a correct id", s, wantBOMLine)
	}
	if !strings.Contains(s, "MAPPING: 1 of your 2 mapping entries matched NO row") {
		t.Errorf("stdout = %q, want the gap counted", s)
	}

	// 🔴 CONTROL: the un-BOM'd second entry DID apply, so this is a genuinely
	// partial repair — the exact silent outcome the section exists to expose.
	got := storedRepairReposBySession(t, path, 2)
	if got["sess-a"] != repoid.Unqualified {
		t.Errorf("sess-a repo = %q, want it left unqualified (its mapping key carries a BOM and cannot match)", got["sess-a"])
	}
	if got["sess-b"] != "acme/lib" {
		t.Errorf("sess-b repo = %q, want acme/lib — the clean entry must still apply", got["sess-b"])
	}

	// And the SAME file without the BOM repairs both, proving the BOM was the
	// only difference.
	path2 := seedRepairDB(t, []repairSeed{
		{developer: "alice", session: "sess-a", cost: 100},
		{developer: "alice", session: "sess-b", cost: 200},
	})
	clean := filepath.Join(t.TempDir(), "clean.map")
	if err := os.WriteFile(clean, []byte(strings.TrimPrefix(body, "\xEF\xBB\xBF")), 0o600); err != nil {
		t.Fatalf("write clean map file: %v", err)
	}
	var out2, errb2 bytes.Buffer
	if code := runRepairRepoCmd([]string{"--db", path2, "--developer", "alice", "--commit", "--map-file", clean}, &out2, &errb2); code != 0 {
		t.Fatalf("control exit code = %d; stderr=%s", code, errb2.String())
	}
	if strings.Contains(out2.String(), "MAPPING:") {
		t.Errorf("control stdout = %q, want no gap section for a BOM-free file", out2.String())
	}
	got2 := storedRepairReposBySession(t, path2, 2)
	if got2["sess-a"] != "acme/app" {
		t.Errorf("control: sess-a repo = %q, want acme/app — without the BOM the identical entry applies", got2["sess-a"])
	}
}

// TestApplyRepeatableConfigList_BlankEntryIsFatal pins the BOOT-PATH consequence
// of making repeatableStringSlice.Set reject an empty value.
//
// The type backs --map (repair-repo), but also serve's --watch-repo and
// --trusted-proxy-cidr, and those two are fed from the CONFIG FILE. So an
// explicit `- ""` in watch.repos, which used to be skipped in silence, now
// refuses to start the server. That is the right answer — a rendered template
// whose variable came out empty is the config analogue of an unset shell variable
// in --map, and silently watching one fewer repo is exactly the kind of quiet
// shrinkage this change exists to stop — but it is a serve-startup behaviour
// change, and an unasserted behaviour change is an accident waiting to be
// "fixed" by someone who thinks it was one.
func TestApplyRepeatableConfigList_BlankEntryIsFatal(t *testing.T) {
	var repos repeatableStringSlice
	err := applyRepeatableConfigList(&repos, []string{"/srv/app", "", "/srv/other"}, "watch.repos")
	if err == nil {
		t.Fatal("a blank config entry was accepted; serve would start silently watching fewer repos than the operator listed")
	}
	for _, want := range []string{"watch.repos", "empty value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q so the operator can find the offending key", err, want)
		}
	}
	// Fails on the FIRST bad entry: the good entry before it is kept, the one
	// after is never reached. Asserted so a future "collect all errors" rewrite
	// is a deliberate choice rather than a silent change of shape.
	if len(repos) != 1 || repos[0] != "/srv/app" {
		t.Errorf("repos = %q, want only the entry preceding the failure", repos)
	}

	// 🔴 CONTROL: a well-formed list still applies in full and in order, so the
	// guard rejects blanks rather than rejecting config lists.
	var ok repeatableStringSlice
	if err := applyRepeatableConfigList(&ok, []string{"/srv/app", "/srv/other"}, "watch.repos"); err != nil {
		t.Fatalf("control: a well-formed list was rejected: %v", err)
	}
	if got := ok.String(); got != "/srv/app,/srv/other" {
		t.Errorf("control: repos = %q, want both entries in order", got)
	}
	// Control: an EMPTY list is not an error — an absent config key is normal.
	var none repeatableStringSlice
	if err := applyRepeatableConfigList(&none, nil, "watch.repos"); err != nil {
		t.Errorf("an absent config list must not be an error: %v", err)
	}
}
