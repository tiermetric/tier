package collector

// Tests for #490 — spend by a worktree-isolated agent inheriting the branch of
// the session that spawned it.
//
// The defect: the agent harness INVENTS the branch name for a worktree-isolated
// agent ("worktree-agent-<hex>"), so no human ever had the opportunity to name it
// per the prefix/<issue>-slug convention and its spend could never attribute. It
// landed in unattributed:branch-without-issue next to genuine human sloppiness,
// which made that bucket's remedy ("name your branches better") wrong for a large
// slice of its contents.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestWatcher_WorktreeAgentInheritsSpawningBranchAcrossDebounce drives the REAL
// watcher — boot, fsnotify, debounce, process(), Ingester — over two debounce
// windows, and it is the only test in this file that does.
//
// 🔴 IT EXISTS BECAUSE ITS ABSENCE LET A BROKEN FIX SHIP GREEN. #490 built
// sessionMetadata at two sites in process(): the foreign-repo cache site, where
// nothing is ever ingested, and the success-path site, which is the ONLY path
// that ingests. The original change carried LastRealBranch on the first and not
// the second, and the entire package still passed — because every other test
// here hand-builds the sessionMetadata that production is supposed to build, and
// so asserts the author's intent rather than the watcher's behaviour.
//
// Deleting `LastRealBranch: s.LastRealBranch` from the success-path literal in
// watcher.go must fail THIS test. Nothing else catches it.
func TestWatcher_WorktreeAgentInheritsSpawningBranchAcrossDebounce(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	// Debounce 1: a human-named branch carrying issue 490. This is the line that
	// establishes the latch — and the tail window of debounce 2 will not include it.
	path := filepath.Join(projectsDir, "wt.jsonl")
	writeJSONLLine(t, path, "sess-wt", repo, "fix/490-attribution", 100)
	if got := waitForEvents(t, rec, 1, 3*time.Second); len(got) != 1 {
		t.Fatalf("debounce 1: got %d events, want 1", len(got))
	}

	// Debounce 2: the harness spawns a worktree-isolated agent and invents its
	// branch name. The watcher resumes from the saved offset, so the only branch
	// it can see in this window is the invented one; resolving it requires the
	// latch to have been carried forward in the state process() returned.
	appendJSONLLine(t, path, "sess-wt", repo, "worktree-agent-a000affda2a90126c", 2000)
	events := waitForEvents(t, rec, 2, 3*time.Second)
	if len(events) != 2 {
		t.Fatalf("debounce 2: got %d events, want 2", len(events))
	}

	// CONTROL: the first event is an ordinary attribution and must be untouched —
	// if it were broken too, the assertion below would not be isolating the latch.
	if events[0].IssueID != "issue-490" {
		t.Fatalf("control: debounce-1 event attributed to %q, want issue-490 — the plain path is broken, so this test is not measuring the latch", events[0].IssueID)
	}

	agent := events[1]
	if agent.InputTok != 2000 {
		t.Fatalf("second event is not the worktree-agent one (input_tok=%d, want 2000)", agent.InputTok)
	}
	if agent.IssueID == UnattributedNoIssue {
		t.Fatalf("LIVE WATCHER attributed worktree-agent spend to %q — the #490 latch is not surviving the debounce on the ingesting path, so `tierd serve` and `tierd score` now disagree about the same file", agent.IssueID)
	}
	if agent.IssueID != "issue-490" {
		t.Errorf("live-watcher worktree-agent spend attributed to %q, want issue-490 inherited from the spawning branch", agent.IssueID)
	}

	// The persisted checkpoint is built from the same state, so a restart must be
	// able to inherit too. An empty latch here means process() returned state that
	// resolved this window by luck and cannot resolve the next one.
	cps := rec.checkpointSnapshot()
	cp, ok := cps[path]
	if !ok {
		t.Fatalf("no checkpoint persisted for %s (have %d)", path, len(cps))
	}
	st, err := stateFromCheckpoint(cp)
	if err != nil {
		t.Fatalf("stateFromCheckpoint: %v", err)
	}
	if st.metadata.LastRealBranch != "fix/490-attribution" {
		t.Errorf("persisted checkpoint LastRealBranch = %q, want %q — a restart would resume with an empty latch and stop attributing mid-session", st.metadata.LastRealBranch, "fix/490-attribution")
	}
}

// TestSessionMetadataRoundTripsInFull guards checkpointFromState's stated
// contract — "the session metadata ... MUST round-trip in full" — which was a
// comment with no test behind it.
//
// 🔴 #490 NOW DEPENDS ON THAT CONTRACT. LastRealBranch is what lets a
// worktree-agent message inherit its spawning branch across a watcher restart;
// if a future change replaced the whole-struct json.Marshal with explicit
// per-field encoding and forgot it, the inheritance would silently stop working
// on the live path only, exactly as it silently did not work before #490.
//
// The reflection guard is what makes this test survive future edits: it FAILS if
// any field of sessionMetadata is left zero in the fixture, so adding a field
// without extending the fixture cannot leave a hole here. A round-trip assertion
// over a partly-zero fixture would pass while dropping the zero fields.
//
// 🔴 IT MUST GO THROUGH checkpointFromState/stateFromCheckpoint, NOT json.Marshal.
// An earlier version marshalled the struct directly, which made it a test of
// encoding/json: mutating the real checkpointFromState to clear LastRealBranch
// before marshalling compiled and left the package green. The persistence
// functions are the contract; the encoder they happen to use today is not.
func TestSessionMetadataRoundTripsInFull(t *testing.T) {
	orig := sessionMetadata{
		SessionID:      "sess-1",
		GitBranch:      "fix/490-attribution",
		CWD:            "/repo",
		StartTime:      time.Unix(1750000000, 0).UTC(),
		Model:          "claude-sonnet-4",
		LastRealBranch: "fix/490-attribution",
		NextParseSeq:   7,
	}
	v := reflect.ValueOf(orig)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("sessionMetadata.%s is the zero value in this fixture — a field that is zero here would round-trip 'correctly' even if the encoder dropped it, so this test could not detect the loss. Populate it.", v.Type().Field(i).Name)
		}
	}

	// The surrounding parseState is populated for the same reason: a checkpoint
	// that dropped offset/inode/fingerprint would be just as silent a loss, and
	// this is the only test that exercises the pair.
	want := parseState{offset: 4096, inode: 99, headCRC: 0xDEADBEEF, headLen: 512, metadata: orig}
	pv := reflect.ValueOf(want)
	for i := range pv.NumField() {
		if pv.Field(i).IsZero() {
			t.Fatalf("parseState.%s is the zero value in this fixture — populate it, or a checkpoint that dropped it would round-trip 'correctly'.", pv.Type().Field(i).Name)
		}
	}

	cp, err := checkpointFromState("/tmp/session.jsonl", want)
	if err != nil {
		t.Fatalf("checkpointFromState: %v", err)
	}
	got, err := stateFromCheckpoint(cp)
	if err != nil {
		t.Fatalf("stateFromCheckpoint: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("parseState did not round-trip through the checkpoint in full:\n got  %+v\n want %+v\nblob: %s", got, want, cp.Metadata)
	}
}

// TestMetadataFromSummaryCarriesEveryField guards the watcher's ONLY
// sessionMetadata literal, which is the seam #490's original RED escaped through.
//
// 🔴 WHY THE ROUND-TRIP TEST ABOVE CANNOT COVER THIS. Both tests reflect, but over
// different things. TestSessionMetadataRoundTripsInFull guards
// checkpointFromState/stateFromCheckpoint and builds its own fixture literal — so
// it proves the ENCODER keeps every field, and says nothing about whether the
// watcher ever put a field in. #490 shipped a version where the latch was present
// in the ingesting literal and absent from the foreign-repo one; that asymmetry
// was invisible to every test in this package. MEASURED: deleting LastRealBranch
// from the foreign-repo literal left the whole tree green.
//
// This test closes that by checking the CONSTRUCTOR, mechanically: for every field
// of sessionMetadata it finds the same-named field on sessionSummary, requires the
// fixture to have populated it, and requires the constructor to have copied it
// through unaltered. An eighth field added to sessionMetadata and forgotten in
// metadataFromSummary fails here without anyone remembering to extend a list.
//
// If a future field legitimately has NO same-named sessionSummary source (derived,
// or named differently), this test fails loudly rather than skipping it — extend it
// deliberately at that point.
func TestMetadataFromSummaryCarriesEveryField(t *testing.T) {
	s := &sessionSummary{
		SessionID:      "sess-490",
		GitBranch:      "worktree-agent-a000affda2a90126c",
		CWD:            "/repo/worktrees/a000affda2a90126c",
		StartTime:      time.Unix(1750000000, 0).UTC(),
		Model:          "claude-opus-4",
		LastRealBranch: "fix/490-attribution",
		NextParseSeq:   11,
	}

	got := metadataFromSummary(s)
	gv := reflect.ValueOf(got)
	sv := reflect.ValueOf(*s)
	for i := range gv.NumField() {
		name := gv.Type().Field(i).Name
		src := sv.FieldByName(name)
		if !src.IsValid() {
			t.Fatalf("sessionMetadata.%s has no same-named field on sessionSummary, so this test cannot check it mechanically — extend the test deliberately for this field rather than leaving it unguarded", name)
		}
		if src.IsZero() {
			t.Fatalf("fixture: sessionSummary.%s is the zero value — a field that is zero in the source would compare 'equal' even if metadataFromSummary never copied it. Populate it.", name)
		}
		if !reflect.DeepEqual(gv.Field(i).Interface(), src.Interface()) {
			t.Errorf("metadataFromSummary dropped or altered %s: got %v, want %v — the watcher caches this struct between debounces and stamps it into the checkpoint, so a dropped field silently degrades the live path only (a full `tierd score` re-parse would still be right), which is exactly the #490 shape", name, gv.Field(i).Interface(), src.Interface())
		}
	}
}

// TestProcessCachesFullMetadataForAForeignRepo covers the OTHER call site — the
// one with no behavioural coverage at all.
//
// 🔴 THE CONSTRUCTOR TEST ABOVE DOES NOT COVER THE CALL SITES. It reflects over
// metadataFromSummary's output, so it proves the constructor is complete; it
// proves nothing about whether either `return` actually calls it. Re-inlining a
// literal at the foreign-repo site minus LastRealBranch — which is EXACTLY the
// edit that shipped as #490's original RED — leaves the constructor test green.
// And every watcher test in this package scopes the watcher to the repo the JSONL
// lives in, so the foreign-repo branch was never executed by anything.
//
// process() is driven directly with NIL targets: matchTarget returns false
// immediately for an empty target list, which is the foreign-repo branch, and no
// ingester or commit cache is reached on it.
func TestProcessCachesFullMetadataForAForeignRepo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreign.jsonl")
	// Same two-line shape as the live-watcher test: a human-named branch that sets
	// the latch, then a harness-invented one that must inherit it.
	writeJSONLLine(t, path, "sess-foreign", "/somewhere/else", "fix/490-attribution", 100)
	appendJSONLLine(t, path, "sess-foreign", "/somewhere/else", "worktree-agent-a000affda2a90126c", 2000)

	w := &Watcher{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	got, ok := w.process(context.Background(), path, parseState{}, nil, "dev", nil, logger)
	if !ok {
		t.Fatal("process reported failure for a foreign-repo session; it should cache metadata and continue")
	}

	// 🔴 CONTROL: this must really be the FOREIGN-repo return. If a future change
	// made an empty target list match, we would be re-testing the ingesting path
	// and the assertion below would prove nothing about the site it names.
	if _, matched := matchTarget("/somewhere/else", nil); matched {
		t.Fatal("control: matchTarget matched an empty target list — this test is no longer exercising the foreign-repo branch")
	}

	if got.metadata.SessionID == "" {
		t.Fatal("no session metadata cached for a foreign-repo file — the fixture did not parse, so the field assertions below would pass vacuously")
	}
	// The latch is the field #490 lost here. Assert the whole struct rather than
	// just that field, so a future eighth field dropped from THIS site fails too.
	if got.metadata.LastRealBranch != "fix/490-attribution" {
		t.Errorf("foreign-repo cached metadata LastRealBranch = %q, want %q — this return has stopped going through metadataFromSummary, which is precisely the shape of #490's original RED: the latch is cached on the path that ingests and dropped on the path that does not, so a session that moves into scope resumes with nothing to inherit", got.metadata.LastRealBranch, "fix/490-attribution")
	}
	if got.metadata.NextParseSeq == 0 {
		t.Errorf("foreign-repo cached metadata NextParseSeq = 0 — the incremental parse counter was dropped at this site, so a later window's idempotency keys can collide with this one's")
	}
}

// TestPrivacyDocDescribesEverySessionMetadataField ties docs/privacy.md to the
// struct it describes.
//
// 🔴 IT IS A DISCLOSURE, NOT A COMMENT. privacy.md enumerates what the
// watcher_checkpoint `metadata` blob holds — an absolute working-directory path, a
// git branch, a session UUID — and presents that list as complete. sessionMetadata
// IS that blob (checkpointFromState marshals the whole struct), so a field added
// here and not there makes a published privacy statement quietly untrue, and
// nothing in the tree noticed. #490 added TWO fields to this struct; that they
// were documented was down to someone remembering.
//
// The mapping is explicit rather than name-matched because the doc is prose for a
// reader, not a field dump — "an internal parse-sequence counter", not
// "NextParseSeq". What makes it self-enforcing anyway is the FAILURE on an
// unmapped field: an eighth field cannot pass this test without someone deciding,
// in writing, what the doc says about it.
func TestPrivacyDocDescribesEverySessionMetadataField(t *testing.T) {
	// field name -> a phrase docs/privacy.md must contain to have disclosed it.
	// Phrases are matched case-insensitively against the doc with all whitespace
	// runs collapsed, so reflowing a paragraph cannot break this test.
	described := map[string]string{
		"SessionID":      "session id",
		"GitBranch":      "git branch",
		"CWD":            "`cwd`",
		"StartTime":      "session start time",
		"Model":          "model name",
		"LastRealBranch": "most recent human-named git branch",
		"NextParseSeq":   "parse-sequence counter",
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "privacy.md"))
	if err != nil {
		t.Fatalf("read docs/privacy.md: %v", err)
	}

	// 🔴 SCOPE THE SEARCH TO THE BULLET THAT MAKES THE CLAIM. Matching the whole
	// document is not a weaker version of this test, it is a different test:
	// privacy.md discusses OTHER datastores in the same words, so "session id"
	// matches the token_events.session_id bullet, "`cwd`" matches six places about
	// reading JSONL, and "model name" matches the pricing section. MEASURED:
	// deleting the entire watcher-checkpoint bullet still left those three
	// "documented". Slicing to the bullet means each phrase must appear in the
	// disclosure it actually belongs to.
	const bullet = "- **Watcher tail-state**"
	start := strings.Index(string(raw), bullet)
	if start < 0 {
		t.Fatalf("docs/privacy.md no longer contains a %q bullet — the disclosure of what the watcher checkpoint stores is gone entirely, or was renamed; this test cannot verify a claim it cannot find", bullet)
	}
	rest := string(raw)[start+len(bullet):]
	if end := strings.Index(rest, "\n- **"); end >= 0 {
		rest = rest[:end]
	}
	doc := strings.ToLower(strings.Join(strings.Fields(rest), " "))

	tp := reflect.TypeOf(sessionMetadata{})
	for i := range tp.NumField() {
		name := tp.Field(i).Name
		phrase, ok := described[name]
		if !ok {
			t.Errorf("sessionMetadata.%s is persisted into the watcher_checkpoint metadata blob but this test has no phrase for it — docs/privacy.md enumerates that blob's contents as complete, so either document the field there and map it here, or record here why it needs no disclosure", name)
			continue
		}
		if !strings.Contains(doc, strings.ToLower(phrase)) {
			t.Errorf("docs/privacy.md no longer contains %q, the phrase that discloses sessionMetadata.%s — the published statement of what the checkpoint stores has drifted from what it stores", phrase, name)
		}
	}
}

// TestIsHarnessWorktreeBranch pins the shape test, and specifically pins that it
// is NARROW.
//
// 🔴 THE NEGATIVE CASES ARE THE POINT. A branch that merely contains the words —
// "fix/512-worktree-agent-naming" — is a normal human branch carrying a real
// issue number. Matching it here would discard that number and make the message
// inherit some other session's issue instead: a silent MIS-attribution, strictly
// worse than the unattributed bucket this change exists to drain. A loosened
// pattern (strings.Contains, or dropping either anchor) must fail this test.
func TestIsHarnessWorktreeBranch(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   bool
	}{
		{"canonical harness name", "worktree-agent-a000affda2a90126c", true},
		{"short hex handle", "worktree-agent-abc123", true},
		{"surrounding whitespace tolerated", "  worktree-agent-deadbeef  ", true},

		{"human branch mentioning the words", "fix/512-worktree-agent-naming", false},
		{"prefixed harness-looking name", "feature/worktree-agent-cleanup", false},
		{"harness name as a path segment", "chore/worktree-agent-a000aff", false},
		{"no hex handle at all", "worktree-agent-", false},
		{"non-hex handle", "worktree-agent-zzzz", false},
		{"trailing slug after the handle", "worktree-agent-abc123-cleanup", false},
		{"ordinary branch", "fix/490-attribution", false},
		{"mainline", "main", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsHarnessWorktreeBranch(c.branch); got != c.want {
				t.Errorf("IsHarnessWorktreeBranch(%q) = %v, want %v", c.branch, got, c.want)
			}
		})
	}
}

// TestParseSessionFile_WorktreeAgentInheritsSpawningBranchNotSessionBranch is the
// #490 fix, and it encodes the one design decision that was MEASURED rather than
// guessed.
//
// 🔴 THE FIXTURE'S SHAPE IS THE ARGUMENT. The session OPENS on main and checks
// out the feature branch later — which is what real sessions do, and what makes
// the obvious cheap fix wrong. The collector already falls back to the
// session-latched branch (sessionSummary.GitBranch) for a message with no branch
// of its own, so "treat a harness name as no branch" looks like a one-line fix
// that reuses a tested path. It is not: the session latch is the FIRST branch
// seen, which here is main, so that fix would have resolved every worktree-agent
// message to unattributed:main and recovered NOTHING.
//
// Measured on this repo's own session files (2026-08-04): of 190 spend-bearing
// worktree-agent messages, the session latch said "main" for all 68 whose true
// spawning branch was a real issue branch. The most-recent-preceding branch —
// what this test asserts — agreed with the JSONL parentUuid causal chain on
// 190/190.
//
// So the assertion below is deliberately "issue-490, and NOT unattributed:main":
// the wrong-but-plausible implementation produces exactly the latter.
func TestParseSessionFile_WorktreeAgentInheritsSpawningBranchNotSessionBranch(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		// Session opens on main — this is what the session latch would capture.
		`{"type":"user","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-wt","gitBranch":"main","cwd":"/repo","message":{"role":"user","content":"start"}}`,
		// Human checks out the real branch for issue 490.
		`{"type":"assistant","timestamp":"2026-06-01T10:05:00Z","sessionId":"sess-wt","gitBranch":"fix/490-attribution","cwd":"/repo","message":{"id":"parent","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50}}}`,
		// A worktree-isolated agent is spawned; the harness invents its branch.
		`{"type":"assistant","timestamp":"2026-06-01T10:06:00Z","sessionId":"sess-wt","gitBranch":"worktree-agent-a000affda2a90126c","cwd":"/repo","message":{"id":"agent","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":2000,"output_tokens":900}}}`,
	}
	path := writeJSONL(t, dir, "worktree.jsonl", lines)
	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil || len(s.Messages) != 2 {
		t.Fatalf("got %v messages, want 2", s)
	}

	// CONTROL: the session latch really is "main" here. If a future change made
	// it something else, the assertions below would stop discriminating between
	// the correct fix and the cheap one, and this test would quietly go blind.
	if s.GitBranch != "main" {
		t.Fatalf("fixture: session-latched branch = %q, want \"main\" — this test's whole point is that inheriting the SESSION branch would be wrong, so it must actually differ from the spawning branch", s.GitBranch)
	}

	branchByID := map[string]string{}
	for _, m := range s.Messages {
		branchByID[m.messageID] = m.gitBranch
	}
	// CONTROL: an ordinary message is untouched.
	if branchByID["parent"] != "fix/490-attribution" {
		t.Errorf("parent message branch = %q, want it left alone", branchByID["parent"])
	}
	if got := branchByID["agent"]; got != "fix/490-attribution" {
		t.Errorf("worktree-agent message branch = %q, want %q inherited from the spawning session", got, "fix/490-attribution")
	}

	// End to end: the spend must actually attribute to the parent's issue.
	events := joinSessionsToCommits([]sessionSummary{*s}, nil, "dev", "")
	var agentIssue string
	for _, e := range events {
		if e.InputTok == 2000 {
			agentIssue = e.IssueID
		}
	}
	if agentIssue == UnattributedMain {
		t.Fatalf("worktree-agent spend attributed to %q — this is the signature of inheriting the SESSION-latched branch (main) instead of the spawning branch, which recovers nothing", agentIssue)
	}
	if agentIssue != "issue-490" {
		t.Errorf("worktree-agent spend attributed to %q, want issue-490", agentIssue)
	}
}

// TestParseSessionFile_WorktreeAgentBranchWithoutSpawnerStillBuckets is the
// degradation control. When nothing has been latched — a file that opens directly
// on a harness branch — there is no branch to inherit, and the message must fall
// back to the pre-#490 bucket rather than inventing an attribution.
//
// It also guards the latch itself: a harness name must never BECOME the latched
// branch, or the next agent would inherit the previous agent's invented name.
func TestParseSessionFile_WorktreeAgentBranchWithoutSpawnerStillBuckets(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"type":"assistant","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-orphan","gitBranch":"worktree-agent-abc123","cwd":"/repo","message":{"id":"orphan","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":500,"output_tokens":100}}}`,
	}
	path := writeJSONL(t, dir, "orphan.jsonl", lines)
	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil || len(s.Messages) != 1 {
		t.Fatalf("got %v messages, want 1", s)
	}
	if s.LastRealBranch != "" {
		t.Errorf("LastRealBranch = %q, want empty — a harness-invented name must never become the branch a later agent inherits", s.LastRealBranch)
	}
	events := joinSessionsToCommits([]sessionSummary{*s}, nil, "dev", "")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].IssueID != UnattributedNoIssue {
		t.Errorf("orphan worktree-agent spend attributed to %q, want %q — with no spawning branch recorded there is nothing to inherit, and guessing would be worse than bucketing", events[0].IssueID, UnattributedNoIssue)
	}
}

// TestParseSessionFile_WorktreeAgentLatchSurvivesIncrementalParse covers the path
// the live watcher actually uses.
//
// 🔴 WITHOUT THIS THE FIX IS BATCH-ONLY, AND SILENTLY SO. The watcher re-parses
// each session file incrementally from a saved offset, so a debounce window
// commonly opens AFTER the line that named the branch. If LastRealBranch did not
// round-trip through sessionMetadata, `tierd score` (a full re-parse) would
// attribute this spend and the live watcher would not — the same file producing
// two different answers depending on which reader got there first.
func TestParseSessionFile_WorktreeAgentLatchSurvivesIncrementalParse(t *testing.T) {
	dir := t.TempDir()
	head := []string{
		`{"type":"assistant","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-inc","gitBranch":"fix/490-attribution","cwd":"/repo","message":{"id":"h1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50}}}`,
	}
	path := writeJSONL(t, dir, "incremental.jsonl", head)

	first, offset, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, false)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first == nil {
		t.Fatal("first parse returned nil summary")
	}
	if first.LastRealBranch != "fix/490-attribution" {
		t.Fatalf("first parse LastRealBranch = %q, want the human branch — the latch is not being recorded, so nothing can carry it forward", first.LastRealBranch)
	}

	// Append a worktree-agent run AFTER the consumed window, exactly as a
	// spawned agent would while the watcher tails the file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(`{"type":"assistant","timestamp":"2026-06-01T10:06:00Z","sessionId":"sess-inc","gitBranch":"worktree-agent-a000affda2a90126c","cwd":"/repo","message":{"id":"h2","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":3000,"output_tokens":700}}}` + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The watcher carries forward exactly these fields.
	meta := sessionMetadata{
		SessionID:      first.SessionID,
		GitBranch:      first.GitBranch,
		CWD:            first.CWD,
		StartTime:      first.StartTime,
		Model:          first.Model,
		LastRealBranch: first.LastRealBranch,
		NextParseSeq:   first.NextParseSeq,
	}
	second, _, err := parseSessionFileFromOffset(path, offset, meta, false)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if second == nil || len(second.Messages) != 1 {
		t.Fatalf("second parse: got %v, want 1 message", second)
	}
	if got := second.Messages[0].gitBranch; got != "fix/490-attribution" {
		t.Errorf("incremental worktree-agent message branch = %q, want %q inherited via the carried latch — the tail window never saw the line that named it", got, "fix/490-attribution")
	}

	// CONTROL: with the latch NOT carried, the same window cannot resolve. This is
	// what the code did before sessionMetadata.LastRealBranch existed, and it is
	// the failure this test exists to keep from coming back.
	bare := meta
	bare.LastRealBranch = ""
	unresolved, _, err := parseSessionFileFromOffset(path, offset, bare, false)
	if err != nil {
		t.Fatalf("control parse: %v", err)
	}
	if unresolved == nil || len(unresolved.Messages) != 1 {
		t.Fatalf("control parse: got %v, want 1 message", unresolved)
	}
	if got := unresolved.Messages[0].gitBranch; got != "worktree-agent-a000affda2a90126c" {
		t.Errorf("control: without the carried latch the branch resolved to %q — the inheritance is coming from somewhere other than sessionMetadata.LastRealBranch, so the positive arm above is not testing what it claims", got)
	}
}
