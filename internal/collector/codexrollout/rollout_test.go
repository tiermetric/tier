package codexrollout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// ─────────────────────────────────────────────────────────────────────────────
// Ground truth, measured from the real captured fixtures.
//
// testdata/rollout-duplicate-token-count.jsonl is a VERBATIM copy of
// model-bench/codex/gpt-5.6-terra/cronspec/rep1/session.jsonl — the session that
// exposed the re-emitted token_count defect. Its cumulative series is
//
//	[17129, 37728, 58507, 58507, 80154, 101922, 124386]
//	                      ^^^^^ the duplicate
//
// so summing last_token_usage yields 145,165 while the truth is 124,386.
// ─────────────────────────────────────────────────────────────────────────────
const (
	dupFixture   = "rollout-duplicate-token-count.jsonl"
	cleanFixture = "rollout-clean-session.jsonl"

	// The honest session total, and the wrong one a last_token_usage sum produces.
	dupTrueTotalTokens  = 124386
	dupNaiveSumOfLast   = 145165
	dupTokenCountEvents = 7 // token_count events in the file...
	dupBillableCalls    = 6 // ...one of which differences to zero and is dropped.

	// Final cumulative snapshot of the duplicate fixture, split into the
	// non-overlapping classes this collector emits.
	dupFinalFreshInput = 12516  // input_tokens - cached_input_tokens
	dupFinalCacheRead  = 107520 // cached_input_tokens
	dupFinalOutput     = 4350   // output_tokens

	// Final cumulative snapshot of the clean fixture (4 token_count events, no
	// duplicate) — the control arm proving the differencing does not distort a
	// well-formed log.
	cleanBillableCalls   = 4
	cleanTotalTokens     = 64229
	cleanFinalFreshInput = 6722
	cleanFinalCacheRead  = 55296
	cleanFinalOutput     = 2211

	testBranch = "feature/464-codex-collector"
	// wantIssueID is what issueref derives from testBranch — the "issue-<n>"
	// form every other capture path stores, so Codex rows join outcomes.
	wantIssueID = "issue-464"
)

// testSince is well before every fixture timestamp, so nothing is window-filtered.
var testSince = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// quietLogger discards collector diagnostics so a passing run is readable. Tests
// that assert on warnings capture their own handler.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initGitRepo turns dir into a real (empty-history) git repository. `git init`
// rather than a bare .git directory, because NewIssueResolver actually runs
// `git log` there. Skips when git is not on PATH (minimal CI containers).
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v\n%s", dir, err, out)
	}
	return dir
}

// fixtureCWD extracts the cwd the fixture was really captured in, so stageFixture
// can rewrite it without a hardcoded absolute path that would rot.
func fixtureCWD(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		var l rolloutLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			continue
		}
		if l.Type == lineSessionMeta && l.Payload != nil && l.Payload.CWD != "" {
			return l.Payload.CWD
		}
	}
	t.Fatal("fixture has no session_meta cwd")
	return ""
}

// stageFixture copies a testdata rollout into a Codex-shaped sessions tree
// (<root>/2026/07/23/rollout-<name>.jsonl), rewriting the captured cwd to `cwd`
// and injecting `branch` into session_meta.git so repo scoping and issue
// attribution can be exercised against a temp repo. Returns the written path.
func stageFixture(t *testing.T, root, fixture, cwd, branch string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	body := string(raw)
	body = strings.ReplaceAll(body, fixtureCWD(t, body), cwd)
	if branch != "" {
		// The captured fixtures ran outside a git checkout, so Codex wrote an
		// empty git object. Fill it in the same shape Codex uses when it does
		// have git info.
		body = strings.ReplaceAll(body, `"git":{}`, fmt.Sprintf(`"git":{"branch":%q}`, branch))
	}
	return writeRollout(t, root, fixture, body)
}

// writeRollout writes body as a rollout log inside a date-partitioned sessions
// tree, mirroring Codex's ~/.codex/sessions/YYYY/MM/DD layout.
func writeRollout(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions tree: %v", err)
	}
	path := filepath.Join(dir, rolloutFilePrefix+name)
	if !strings.HasSuffix(path, rolloutFileExt) {
		path += rolloutFileExt
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// newTestCollector wires a Collector over one temp repo and one sessions root.
func newTestCollector(t *testing.T, sessionsDir string, repos ...RepoTarget) *Collector {
	t.Helper()
	c, err := New(Config{
		SessionsDir: sessionsDir,
		Repos:       repos,
		DeveloperID: "tester",
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// totals sums the emitted token classes.
type totals struct{ input, cacheRead, output int }

func sumEvents(evs []collector.TokenEvent) totals {
	var tt totals
	for _, e := range evs {
		tt.input += e.InputTok
		tt.cacheRead += e.CacheRead
		tt.output += e.OutputTok
	}
	return tt
}

// synthetic builds a minimal but structurally faithful rollout log from a list
// of cumulative snapshots. Used for the invariant control arms, where a precise
// one-field corruption is clearer than mutating a 100 KB captured file.
func synthetic(cwd, branch, model string, snaps []tokenUsage) string {
	var b strings.Builder
	git := `{}`
	if branch != "" {
		git = fmt.Sprintf(`{"branch":%q}`, branch)
	}
	fmt.Fprintf(&b, `{"timestamp":"2026-07-23T00:14:00.000Z","type":"session_meta","payload":{"id":"synthetic-session-0001","cwd":%q,"git":%s}}`+"\n", cwd, git)
	if model != "" {
		fmt.Fprintf(&b, `{"timestamp":"2026-07-23T00:14:01.000Z","type":"turn_context","payload":{"model":%q}}`+"\n", model)
	}
	for i, s := range snaps {
		usage, err := json.Marshal(s)
		if err != nil {
			panic(err) // test-local builder; a marshal failure is a programming error
		}
		fmt.Fprintf(&b, `{"timestamp":"2026-07-23T00:%02d:%02d.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":%s}}}`+"\n",
			15+i/60, i%60, usage)
	}
	return b.String()
}

// usage is a terse constructor for a well-formed cumulative snapshot.
func usage(input, cached, output, reasoning int) tokenUsage {
	return tokenUsage{
		InputTokens:           input,
		CachedInputTokens:     cached,
		OutputTokens:          output,
		ReasoningOutputTokens: reasoning,
		TotalTokens:           input + output,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE REGRESSION TEST
// ─────────────────────────────────────────────────────────────────────────────

// TestDuplicateTokenCountEventIsNotDoubleCounted is the reason this collector
// differences cumulative snapshots instead of summing last_token_usage.
//
// The fixture is a real captured Codex session in which one token_count event is
// RE-EMITTED. Summing the per-call `last_token_usage` field over it yields
// 145,165 tokens; the true session total is 124,386 — a 20,779-token (17%)
// overcount that would flow straight into the cost figure.
//
// If this test ever fails with a total of 145,165, someone has "simplified" the
// differencing in parse.go back into a sum. Do not.
func TestDuplicateTokenCountEventIsNotDoubleCounted(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, dupFixture, repo, testBranch)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := sumEvents(events)
	total := got.input + got.cacheRead + got.output
	if total == dupNaiveSumOfLast {
		t.Fatalf("total tokens = %d — that is the sum of last_token_usage, which DOUBLE-COUNTS the re-emitted token_count event; want the differenced cumulative total %d",
			total, dupTrueTotalTokens)
	}
	if total != dupTrueTotalTokens {
		t.Errorf("total tokens = %d, want %d (final cumulative total_tokens of the fixture)", total, dupTrueTotalTokens)
	}

	// Per-class split must match the final cumulative snapshot exactly: the
	// deltas telescope, so any drift here means a class was mis-carved.
	if got.input != dupFinalFreshInput {
		t.Errorf("fresh input = %d, want %d (input_tokens - cached_input_tokens)", got.input, dupFinalFreshInput)
	}
	if got.cacheRead != dupFinalCacheRead {
		t.Errorf("cache read = %d, want %d (cached_input_tokens)", got.cacheRead, dupFinalCacheRead)
	}
	if got.output != dupFinalOutput {
		t.Errorf("output = %d, want %d (output_tokens)", got.output, dupFinalOutput)
	}

	// The duplicate differences to zero and is dropped, so 7 token_count events
	// produce 6 billable calls.
	if len(events) != dupBillableCalls {
		t.Errorf("emitted %d events, want %d (%d token_count events minus the zero-delta duplicate)",
			len(events), dupBillableCalls, dupTokenCountEvents)
	}
	for i, e := range events {
		if e.InputTok == 0 && e.CacheRead == 0 && e.OutputTok == 0 {
			t.Errorf("event %d is all-zero — a zero-delta duplicate leaked into the output", i)
		}
	}
}

// TestCleanRolloutFixtureIsUndistorted is the control arm for the regression
// above: on a captured session with NO re-emitted event, differencing must
// reproduce the cumulative totals exactly and emit one event per token_count.
func TestCleanRolloutFixtureIsUndistorted(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, cleanFixture, repo, testBranch)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != cleanBillableCalls {
		t.Fatalf("emitted %d events, want %d", len(events), cleanBillableCalls)
	}
	got := sumEvents(events)
	if total := got.input + got.cacheRead + got.output; total != cleanTotalTokens {
		t.Errorf("total tokens = %d, want %d", total, cleanTotalTokens)
	}
	if got.input != cleanFinalFreshInput || got.cacheRead != cleanFinalCacheRead || got.output != cleanFinalOutput {
		t.Errorf("classes = (input %d, cacheRead %d, output %d), want (%d, %d, %d)",
			got.input, got.cacheRead, got.output,
			cleanFinalFreshInput, cleanFinalCacheRead, cleanFinalOutput)
	}
}

// TestEmittedEventShape asserts the provenance fields every downstream consumer
// depends on, on a real captured fixture.
func TestEmittedEventShape(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, cleanFixture, repo, testBranch)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for i, e := range events {
		if e.Source != collector.SourceCodexRollout {
			t.Errorf("event %d Source = %q, want %q", i, e.Source, collector.SourceCodexRollout)
		}
		if e.Fidelity != collector.FidelityRealtime {
			t.Errorf("event %d Fidelity = %q, want %q", i, e.Fidelity, collector.FidelityRealtime)
		}
		if e.Developer != "tester" {
			t.Errorf("event %d Developer = %q, want %q", i, e.Developer, "tester")
		}
		if e.SessionID == "" {
			t.Errorf("event %d has no SessionID", i)
		}
		if e.IdempotencyKey == "" {
			t.Errorf("event %d has no IdempotencyKey", i)
		}
		if e.Model == "" {
			t.Errorf("event %d has no Model — it would be priced at the self-hosted guess rate", i)
		}
		if e.CostMicro <= 0 {
			t.Errorf("event %d CostMicro = %d, want a positive priced cost", i, e.CostMicro)
		}
		if e.CacheWrite5m != 0 || e.CacheWrite1h != 0 {
			t.Errorf("event %d has cache writes (%d/%d) — OpenAI has no cache-write SKU",
				i, e.CacheWrite5m, e.CacheWrite1h)
		}
		if e.Host != "" {
			t.Errorf("event %d Host = %q, want empty (a local log cannot know the serving host)", i, e.Host)
		}
		if e.Timestamp.Location() != time.UTC {
			t.Errorf("event %d Timestamp is not UTC: %v", i, e.Timestamp)
		}
		// The branch names issue 464, so attribution must resolve to it rather
		// than fall into an unattributed bucket.
		if e.IssueID != wantIssueID {
			t.Errorf("event %d IssueID = %q, want %q (derived from branch %q)", i, e.IssueID, wantIssueID, testBranch)
		}
	}
}

// TestNoGitBranchRoutesToUnattributedBucket: a rollout captured outside a git
// checkout (Codex writes `"git":{}`) must keep its spend in the developer's
// denominator under a labeled unattributed bucket, never drop it.
func TestNoGitBranchRoutesToUnattributedBucket(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, cleanFixture, repo, "") // no branch injected

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != cleanBillableCalls {
		t.Fatalf("emitted %d events, want %d — branchless spend must NOT be dropped", len(events), cleanBillableCalls)
	}
	for i, e := range events {
		if !collector.IsUnattributed(e.IssueID) {
			t.Errorf("event %d IssueID = %q, want a labeled unattributed bucket", i, e.IssueID)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Containment invariants — each with a control arm that MUST fail
// ─────────────────────────────────────────────────────────────────────────────

// TestContainmentInvariantsAreFatal drives one deliberately corrupted log per
// invariant and asserts the whole session is REJECTED (fatal), not warned past.
//
// The first case is the control: an uncorrupted log through the identical code
// path, which must PASS. Without it, a parser that rejected everything would
// score a perfect pass on the corruption rows.
func TestContainmentInvariantsAreFatal(t *testing.T) {
	tests := []struct {
		name     string
		snaps    []tokenUsage
		wantFail bool
		wantErr  string
	}{
		{
			// CONTROL ARM: well-formed. Must pass, or the corruption rows below
			// prove nothing.
			name: "control/well-formed log is accepted",
			snaps: []tokenUsage{
				usage(1000, 400, 100, 40),
				usage(2500, 1200, 260, 90),
			},
			wantFail: false,
		},
		{
			name: "total != input + output",
			snaps: []tokenUsage{
				{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 100, TotalTokens: 9999},
			},
			wantFail: true,
			wantErr:  "total_tokens=9999",
		},
		{
			name: "cached > input",
			snaps: []tokenUsage{
				{InputTokens: 1000, CachedInputTokens: 1001, OutputTokens: 100, TotalTokens: 1100},
			},
			wantFail: true,
			wantErr:  "cached_input_tokens=1001 > input_tokens=1000",
		},
		{
			name: "reasoning > output",
			snaps: []tokenUsage{
				{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 100, ReasoningOutputTokens: 101, TotalTokens: 1100},
			},
			wantFail: true,
			wantErr:  "reasoning_output_tokens=101 > output_tokens=100",
		},
		{
			// #464 Y-S2: every OTHER invariant here is relational — it only
			// asserts the fields agree with each other — so a self-consistent
			// absurdity satisfies all of them. Unbounded, it prices to a clamped
			// MaxInt64 cost_micro (~$9.2 trillion), and two such rows overflow
			// SQLite's integer SUM(), which ERRORS rather than saturating: one
			// corrupt local file would take down /api/v1/scores.
			name: "self-consistent but implausible magnitude",
			snaps: []tokenUsage{
				usage(4_000_000_000_000_000_000, 0, 100, 0),
			},
			wantFail: true,
			wantErr:  "sanity ceiling",
		},
		{
			name: "negative token count",
			snaps: []tokenUsage{
				{InputTokens: -1000, CachedInputTokens: 0, OutputTokens: 100, TotalTokens: -900},
			},
			wantFail: true,
			wantErr:  "negative token count",
		},
		{
			name: "cumulative series decreases",
			snaps: []tokenUsage{
				usage(2000, 800, 200, 50),
				usage(1000, 400, 100, 20), // rewind — deltas would go negative
			},
			wantFail: true,
			wantErr:  "not monotonic",
		},
		{
			name: "cached delta outruns input delta",
			snaps: []tokenUsage{
				// Both snapshots satisfy cached <= input on their own, yet the
				// cached growth (+900) exceeds the input growth (+100), which
				// would carve a NEGATIVE fresh-input count.
				usage(1000, 100, 100, 0),
				usage(1100, 1000, 200, 0),
			},
			wantFail: true,
			wantErr:  "cached delta=900 exceeds input delta=100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := initGitRepo(t, t.TempDir())
			sessions := t.TempDir()
			writeRollout(t, sessions, "corrupt.jsonl",
				synthetic(repo, testBranch, "gpt-5.6-terra", tc.snaps))

			c := newTestCollector(t, sessions, RepoTarget{Path: repo})
			events, err := c.Collect(context.Background(), testSince)

			if !tc.wantFail {
				if err != nil {
					t.Fatalf("control arm must PASS, got error: %v", err)
				}
				if len(events) == 0 {
					t.Fatal("control arm produced no events — the corruption rows below would prove nothing")
				}
				return
			}
			if err == nil {
				t.Fatalf("corrupted log was ACCEPTED (%d events emitted); a violated containment invariant must be fatal", len(events))
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if len(events) != 0 {
				t.Errorf("corrupted log still emitted %d events; a failing file must contribute ZERO cost", len(events))
			}
		})
	}
}

// TestOneCorruptFileDoesNotBlindTheScan: a failing session must be named in the
// error AND dropped whole, while its healthy siblings still emit. Losing every
// other session's spend to one bad log would be its own silent failure.
func TestOneCorruptFileDoesNotBlindTheScan(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, cleanFixture, repo, testBranch)
	writeRollout(t, sessions, "corrupt.jsonl", synthetic(repo, testBranch, "gpt-5.6-terra",
		[]tokenUsage{{InputTokens: 100, CachedInputTokens: 999, OutputTokens: 10, TotalTokens: 110}}))

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err == nil {
		t.Fatal("want an error naming the corrupt file")
	}
	if !strings.Contains(err.Error(), "corrupt.jsonl") {
		t.Errorf("error must name the failing file, got %q", err)
	}
	if len(events) != cleanBillableCalls {
		t.Errorf("healthy sibling emitted %d events, want %d — one bad log must not blind the scan",
			len(events), cleanBillableCalls)
	}
	if total := sumEvents(events); total.input+total.cacheRead+total.output != cleanTotalTokens {
		t.Errorf("sibling total = %d, want %d (no partial spend from the corrupt file)",
			total.input+total.cacheRead+total.output, cleanTotalTokens)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Idempotency, scoping, and degenerate inputs
// ─────────────────────────────────────────────────────────────────────────────

// TestIdempotentReingest: scanning the same rollout twice must produce the same
// idempotency keys, so the store's partial unique index collapses the re-ingest
// into one set of rows rather than doubling the recorded spend.
func TestIdempotentReingest(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, dupFixture, repo, testBranch)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	first, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	second, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("re-scan produced %d events, first scan %d", len(second), len(first))
	}

	// Keys must be unique WITHIN a scan (or the store would silently drop real
	// calls) and identical ACROSS scans (or a re-scan would double the spend).
	keys := make(map[string]int, len(first))
	for i, e := range first {
		if prev, dup := keys[e.IdempotencyKey]; dup {
			t.Fatalf("events %d and %d share idempotency key %q — one real call would be dropped by the unique index",
				prev, i, e.IdempotencyKey)
		}
		keys[e.IdempotencyKey] = i
	}
	for i, e := range second {
		if _, ok := keys[e.IdempotencyKey]; !ok {
			t.Errorf("re-scan event %d has key %q not present in the first scan — a re-ingest would DOUBLE this spend",
				i, e.IdempotencyKey)
		}
	}

	// Simulate the store: dedupe by key and confirm the total is the true
	// session total, not twice it.
	deduped := make(map[string]collector.TokenEvent)
	for _, e := range append(append([]collector.TokenEvent{}, first...), second...) {
		deduped[e.IdempotencyKey] = e
	}
	var total int
	for _, e := range deduped {
		total += e.InputTok + e.CacheRead + e.OutputTok
	}
	if total != dupTrueTotalTokens {
		t.Errorf("deduped total across two ingests = %d, want %d", total, dupTrueTotalTokens)
	}
}

// TestCrossRepoFilteringDropsForeignSessions: a Codex session that ran in a
// DIFFERENT repo on the same machine must not have its dollars attributed to the
// target repo's issues (#15).
func TestCrossRepoFilteringDropsForeignSessions(t *testing.T) {
	target := initGitRepo(t, t.TempDir())
	foreign := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()

	// Two sessions, distinguishable by filename, in two different repos.
	stageFixture(t, sessions, cleanFixture, target, testBranch)
	foreignBody, err := os.ReadFile(filepath.Join("testdata", dupFixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	body := strings.ReplaceAll(string(foreignBody), fixtureCWD(t, string(foreignBody)), foreign)
	writeRollout(t, sessions, "foreign.jsonl", body)

	c := newTestCollector(t, sessions, RepoTarget{Path: target})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != cleanBillableCalls {
		t.Fatalf("emitted %d events, want only the %d from the target repo", len(events), cleanBillableCalls)
	}
	if total := sumEvents(events); total.input+total.cacheRead+total.output != cleanTotalTokens {
		t.Errorf("total = %d, want %d — foreign-repo spend bled into the target",
			total.input+total.cacheRead+total.output, cleanTotalTokens)
	}

	// CONTROL ARM: adding the foreign repo as a second target MUST pick it up.
	// Without this, a filter that dropped everything would pass the assertion above.
	c2 := newTestCollector(t, sessions, RepoTarget{Path: target}, RepoTarget{Path: foreign})
	all, err := c2.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect (both targets): %v", err)
	}
	if len(all) != cleanBillableCalls+dupBillableCalls {
		t.Errorf("with both repos targeted, emitted %d events, want %d — the filter is dropping in-scope sessions",
			len(all), cleanBillableCalls+dupBillableCalls)
	}
}

// TestSessionWithNoTokenCountEventsIsSilentlyEmpty: an aborted or purely
// conversational Codex session has no spend. That is not an error.
func TestSessionWithNoTokenCountEventsIsSilentlyEmpty(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	writeRollout(t, sessions, "idle.jsonl", synthetic(repo, testBranch, "gpt-5.6-terra", nil))

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("a session with no token_count events must not be an error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events from a session with no usage", len(events))
	}
}

// TestTruncatedFinalLineIsSkippedNotFatal: Codex may be mid-write when the scan
// runs, leaving a partial final line. That line must be skipped — and because
// the collector differences CUMULATIVE snapshots, a dropped token_count line
// merges into the next delta rather than losing its tokens. Summing per-call
// values could never offer that guarantee.
func TestTruncatedFinalLineIsSkippedNotFatal(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()

	full := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
		usage(4000, 2000, 400, 120),
	})
	// Chop the file mid-way through what would have been a fourth line.
	truncated := full + `{"timestamp":"2026-07-23T00:16:00.000Z","type":"event_msg","payload":{"type":"token_c`

	writeRollout(t, sessions, "truncated.jsonl", truncated)
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("a truncated final line must be skipped, not fatal: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("emitted %d events, want 3 (the complete snapshots)", len(events))
	}
	got := sumEvents(events)
	// The last COMPLETE cumulative snapshot is (input 4000, cached 2000, output 400).
	if got.input != 2000 || got.cacheRead != 2000 || got.output != 400 {
		t.Errorf("classes = (input %d, cacheRead %d, output %d), want (2000, 2000, 400)",
			got.input, got.cacheRead, got.output)
	}
}

// TestDroppedMiddleSnapshotStillTotalsExactly proves the property the previous
// test relies on: differencing is self-healing. A token_count line lost to
// corruption merges its usage into the NEXT delta, so the session TOTAL is
// unchanged — only the per-call split gets coarser.
func TestDroppedMiddleSnapshotStillTotalsExactly(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	snaps := []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
		usage(4000, 2000, 400, 120),
	}

	collectTotals := func(t *testing.T, body string) (totals, int) {
		t.Helper()
		sessions := t.TempDir()
		writeRollout(t, sessions, "s.jsonl", body)
		c := newTestCollector(t, sessions, RepoTarget{Path: repo})
		evs, err := c.Collect(context.Background(), testSince)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return sumEvents(evs), len(evs)
	}

	intact := synthetic(repo, testBranch, "gpt-5.6-terra", snaps)
	wantTotals, wantCount := collectTotals(t, intact)
	if wantCount != 3 {
		t.Fatalf("control: emitted %d events, want 3", wantCount)
	}

	// Corrupt the MIDDLE token_count line so it fails to parse.
	lines := strings.Split(strings.TrimRight(intact, "\n"), "\n")
	lines[3] = `{"type":"event_msg","payload":{"type":"token_count",` // unterminated JSON
	damaged := strings.Join(lines, "\n") + "\n"

	gotTotals, gotCount := collectTotals(t, damaged)
	if gotCount != 2 {
		t.Errorf("emitted %d events, want 2 (one snapshot was unparseable)", gotCount)
	}
	if gotTotals != wantTotals {
		t.Errorf("totals after dropping a middle snapshot = %+v, want %+v — cumulative differencing must be self-healing",
			gotTotals, wantTotals)
	}
}

// TestUnnamedModelIsFatal: pricing a model we cannot name falls through
// store.ComputeCost's self-hosted size-class GUESS and emits a confident dollar
// figure derived from nothing. Refuse instead.
func TestUnnamedModelIsFatal(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	// No turn_context line, so no model is ever named.
	writeRollout(t, sessions, "nomodel.jsonl",
		synthetic(repo, testBranch, "", []tokenUsage{usage(1000, 400, 100, 40)}))

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err == nil {
		t.Fatalf("a rollout that never names a model must be fatal; emitted %d events", len(events))
	}
	if !strings.Contains(err.Error(), "unnamed model") {
		t.Errorf("error = %q, want it to explain the unnamed-model refusal", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events from an unpriceable log", len(events))
	}
}

// TestMissingSessionsRootIsNotAnError: an operator may enable the collector
// before installing Codex. That is an empty scan, not a startup failure that
// would also take down the Claude Code capture path.
func TestMissingSessionsRootIsNotAnError(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	c := newTestCollector(t, filepath.Join(t.TempDir(), "does-not-exist"), RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("missing sessions root: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events from a nonexistent sessions root", len(events))
	}
}

// TestNewRejectsUnusableConfig: a collector with no repo target can never
// attribute anything, so it must fail at construction rather than run forever
// producing nothing.
func TestNewRejectsUnusableConfig(t *testing.T) {
	if _, err := New(Config{SessionsDir: t.TempDir()}); err == nil {
		t.Error("New with no repo targets must fail")
	}
	if _, err := New(Config{SessionsDir: t.TempDir(), Repos: []RepoTarget{{Path: "  "}}}); err == nil {
		t.Error("New with a blank repo path must fail")
	}
}

// TestRunIngestsAndHonoursCancellation exercises the push form: Run must ingest
// the first pass's events and return nil (clean shutdown) on ctx cancel.
func TestRunIngestsAndHonoursCancellation(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	stageFixture(t, sessions, cleanFixture, repo, testBranch)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	c.interval = time.Hour // never fires within the test

	ctx, cancel := context.WithCancel(context.Background())
	var got []collector.TokenEvent
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, testSince, collector.IngesterFunc(
			func(_ context.Context, ev collector.TokenEvent) error {
				got = append(got, ev)
				if len(got) == cleanBillableCalls {
					cancel()
				}
				return nil
			}))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("Run did not return after cancellation")
	}
	if len(got) != cleanBillableCalls {
		t.Errorf("ingested %d events, want %d", len(got), cleanBillableCalls)
	}
}

// TestRunRequiresIngester guards the nil-sink programming error.
func TestRunRequiresIngester(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	c := newTestCollector(t, t.TempDir(), RepoTarget{Path: repo})
	if err := c.Run(context.Background(), testSince, nil); err == nil {
		t.Error("Run with a nil Ingester must fail")
	}
}

// TestName pins the source tag: it is persisted on every row and read back by
// provenance queries, so a rename is a data migration, not a refactor.
func TestName(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	c := newTestCollector(t, t.TempDir(), RepoTarget{Path: repo})
	if got := c.Name(); got != "codex-rollout" {
		t.Errorf("Name() = %q, want %q", got, "codex-rollout")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #464 review fixes — regression tests
// ─────────────────────────────────────────────────────────────────────────────

// captureLogger returns a logger writing to a buffer, for tests that assert on
// what an operator would (or must not) see. A TEXT handler is deliberate: unlike
// JSONHandler it does not escape control bytes on its own, so a log-injection
// test can see whether OUR sanitizer ran.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// newLoggingCollector is newTestCollector with a capturing logger.
func newLoggingCollector(t *testing.T, sessionsDir string, repos ...RepoTarget) (*Collector, *bytes.Buffer) {
	t.Helper()
	logger, buf := captureLogger()
	c, err := New(Config{
		SessionsDir: sessionsDir,
		Repos:       repos,
		DeveloperID: "tester",
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, buf
}

// syntheticSpaced builds a rollout log whose token_count events are ONE HOUR
// apart, so a test can distinguish "inside the cursor's safety lag" from
// "before the cursor" without sleeping.
func syntheticSpaced(cwd, branch, model string, snaps []tokenUsage) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"timestamp":"2026-07-23T00:00:00.000Z","type":"session_meta","payload":{"id":"spaced-session-0001","cwd":%q,"git":{"branch":%q}}}`+"\n", cwd, branch)
	fmt.Fprintf(&b, `{"timestamp":"2026-07-23T00:00:01.000Z","type":"turn_context","payload":{"model":%q}}`+"\n", model)
	for i, s := range snaps {
		usage, err := json.Marshal(s)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, `{"timestamp":"2026-07-23T%02d:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":%s}}}`+"\n",
			i+1, usage)
	}
	return b.String()
}

// TestOversizedFileIsFatal is the #464 R2 regression: io.LimitReader returns EOF
// at its limit with a NIL error, so a file over the read cap used to parse
// SUCCESSFULLY on its prefix and report a truncated total as if it were the
// session's real spend. The cap must fail the file, exactly as an over-long LINE
// already did.
func TestOversizedFileIsFatal(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	body := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	})

	// Shrink the cap rather than write 64 MB into every test run.
	orig := maxRolloutFile
	maxRolloutFile = int64(len(body)) - 1
	t.Cleanup(func() { maxRolloutFile = orig })

	writeRollout(t, sessions, "huge.jsonl", body)
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err == nil {
		t.Fatalf("a file over the read cap was ACCEPTED (%d events); its spend is truncated and must not be reported", len(events))
	}
	if !strings.Contains(err.Error(), "read cap") {
		t.Errorf("error = %q, want it to name the read cap", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events from a truncated read; want 0", len(events))
	}

	// CONTROL ARM: the same log one byte under the cap must parse fine, or the
	// assertion above would pass for a parser that rejected everything.
	maxRolloutFile = int64(len(body))
	c2 := newTestCollector(t, sessions, RepoTarget{Path: repo})
	if _, err := c2.Collect(context.Background(), testSince); err != nil {
		t.Fatalf("a file exactly AT the cap must parse: %v", err)
	}
}

// TestPartialFinalLineIsSilentButRealCorruptionWarns is the #464 Y-G2
// regression. A live Codex session is always mid-write, so classifying its
// unterminated tail as corruption made the malformed-lines WARN permanent —
// training operators to ignore the one signal that means the log is damaged.
func TestPartialFinalLineIsSilentButRealCorruptionWarns(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	full := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	})

	t.Run("mid-flush tail is silent", func(t *testing.T) {
		sessions := t.TempDir()
		writeRollout(t, sessions, "live.jsonl", full+`{"timestamp":"2026-07-23T00:16:00.000Z","type":"event_ms`)
		c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
		if _, err := c.Collect(context.Background(), testSince); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if strings.Contains(logs.String(), "skipped malformed rollout lines") {
			t.Errorf("a writer mid-flush raised the corruption warning; every running Codex session would warn every scan:\n%s", logs.String())
		}
	})

	t.Run("a complete bad line still warns", func(t *testing.T) {
		sessions := t.TempDir()
		// Same broken bytes, but TERMINATED — that is a complete line that failed
		// to decode, i.e. real damage.
		writeRollout(t, sessions, "damaged.jsonl", full+`{"timestamp":"2026-07-23T00:16:00.000Z","type":"event_ms`+"\n")
		c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
		if _, err := c.Collect(context.Background(), testSince); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if !strings.Contains(logs.String(), "skipped malformed rollout lines") {
			t.Errorf("a complete undecodable line must still warn — otherwise real damage is invisible:\n%s", logs.String())
		}
	})
}

// TestOrdinalIsFilePositionNotCallIndex pins the idempotency key's anchor
// (#464 Y3). The duplicate fixture has 7 token_count events; the 4th (index 3)
// differences to zero and is dropped, so the surviving calls must carry ordinals
// 0,1,2,4,5,6 — NOT the 0..5 an index into Calls would produce.
func TestOrdinalIsFilePositionNotCallIndex(t *testing.T) {
	logger := quietLogger()
	sess, err := parseRollout(filepath.Join("testdata", dupFixture), logger)
	if err != nil {
		t.Fatalf("parseRollout: %v", err)
	}
	got := make([]int, 0, len(sess.Calls))
	for _, c := range sess.Calls {
		got = append(got, c.Ordinal)
	}
	want := []int{0, 1, 2, 4, 5, 6}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ordinals = %v, want %v — the ordinal must be the token_count event's POSITION IN THE FILE (the dropped zero-delta duplicate still consumes ordinal 3), not an index into the emitted calls",
			got, want)
	}
}

// TestIdempotencyAcrossFileGrowth is the #464 Y4 regression. The existing
// idempotency test re-scans an UNCHANGED file, which is not the production case:
// a rollout log is appended to between scans, and the second scan must reproduce
// every key the first one emitted while adding only the new calls.
func TestIdempotencyAcrossFileGrowth(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()

	snaps := []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	}
	scan1Body := synthetic(repo, testBranch, "gpt-5.6-terra", snaps)
	// A partial final line, exactly as a live writer leaves it mid-flush.
	partial := `{"timestamp":"2026-07-23T00:17:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_to`
	path := writeRollout(t, sessions, "growing.jsonl", scan1Body+partial)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	first, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	if len(first) != len(snaps) {
		t.Fatalf("first scan emitted %d events, want %d", len(first), len(snaps))
	}

	// Now the file GROWS: the partial line completes, Codex RE-EMITS that
	// cumulative snapshot (the defect this collector exists for), and a genuinely
	// new call follows.
	grown := synthetic(repo, testBranch, "gpt-5.6-terra", append(append([]tokenUsage{}, snaps...),
		usage(4000, 2000, 400, 120), // the completed line
		usage(4000, 2000, 400, 120), // duplicate re-emit -> zero delta, dropped
		usage(6000, 3000, 550, 150), // a new call
	))
	if err := os.WriteFile(path, []byte(grown), 0o644); err != nil {
		t.Fatalf("append to rollout: %v", err)
	}

	second, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}

	// Every key from scan 1 must still be produced by scan 2 — otherwise the
	// store holds two rows for one call and the spend is doubled.
	keys2 := make(map[string]collector.TokenEvent, len(second))
	for _, e := range second {
		if _, dup := keys2[e.IdempotencyKey]; dup {
			t.Fatalf("scan 2 produced duplicate key %q within one scan", e.IdempotencyKey)
		}
		keys2[e.IdempotencyKey] = e
	}
	for i, e := range first {
		got, ok := keys2[e.IdempotencyKey]
		if !ok {
			t.Errorf("scan-1 event %d key %q is absent from scan 2 — its spend would be ingested TWICE under two keys", i, e.IdempotencyKey)
			continue
		}
		if got.InputTok != e.InputTok || got.CacheRead != e.CacheRead || got.OutputTok != e.OutputTok {
			t.Errorf("key %q changed classes between scans: %v/%v/%v -> %v/%v/%v",
				e.IdempotencyKey, e.InputTok, e.CacheRead, e.OutputTok, got.InputTok, got.CacheRead, got.OutputTok)
		}
	}

	// Store simulation: dedupe both scans by key; the total must equal the final
	// cumulative total (6000 input + 550 output), not more.
	deduped := make(map[string]collector.TokenEvent)
	for _, e := range append(append([]collector.TokenEvent{}, first...), second...) {
		deduped[e.IdempotencyKey] = e
	}
	var total int
	for _, e := range deduped {
		total += e.InputTok + e.CacheRead + e.OutputTok
	}
	if want := 6000 + 550; total != want {
		t.Errorf("deduped total across a growing file = %d, want %d (the final cumulative total)", total, want)
	}
}

// TestZeroTimestampIsFatal (#464 Y5): an event with no timestamp, in a file with
// none to fall back on, would store as year 1 — present in the table, absent
// from every dashboard window. Spend that silently disappears is worse than a
// named failure.
func TestZeroTimestampIsFatal(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	body := fmt.Sprintf(`{"type":"session_meta","payload":{"id":"no-ts-0001","cwd":%q,"git":{"branch":%q}}}`+"\n"+
		`{"type":"turn_context","payload":{"model":"gpt-5.6-terra"}}`+"\n"+
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":100,"total_tokens":1100}}}}`+"\n",
		repo, testBranch)
	writeRollout(t, sessions, "nots.jsonl", body)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err == nil {
		t.Fatalf("a timestamp-less rollout was accepted (%d events); its spend would land in year 1 and vanish from every window", len(events))
	}
	if !strings.Contains(err.Error(), "no timestamp") {
		t.Errorf("error = %q, want it to name the missing timestamp", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events; want 0", len(events))
	}
}

// TestNonRegularRolloutFileIsRefused is the #464 Y-S3 regression for the symlink
// vector: WalkDir uses Lstat, so a symlink NAMED like a rollout log would be
// followed and read from outside the sessions root.
func TestNonRegularRolloutFileIsRefused(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()

	// A real rollout log living OUTSIDE the sessions root...
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(outside, []byte(synthetic(repo, testBranch, "gpt-5.6-terra",
		[]tokenUsage{usage(1000, 400, 100, 40)})), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// ...reachable only through a rollout-named symlink inside it.
	dir := filepath.Join(sessions, "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "rollout-link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events by following a symlink out of the sessions root; want 0", len(events))
	}
	if !strings.Contains(logs.String(), "non-regular file") {
		t.Errorf("the refusal must be observable, not silent:\n%s", logs.String())
	}
}

// TestLogPathIsNotForgeable is the #464 Y-S4 guard (sibling of the #321 webhook
// and proxy forge tests). POSIX filenames may contain CR/LF, and the scan path
// reaches the logs, so a rollout log NAMED with an embedded newline could forge
// a standalone log record. logsafe.Str must strip it.
func TestLogPathIsNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A rollout log whose NAME carries a newline plus a forged record, and whose
	// CONTENT has a complete malformed line so the path-bearing warning fires.
	name := "rollout-evil\ntime=2026-07-12T00:00:00Z " + forgedMarker + ".jsonl"
	body := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)}) +
		"{not json at all}\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Skipf("filesystem rejects newline in filenames: %v", err)
	}

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	if _, err := c.Collect(context.Background(), testSince); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "skipped malformed rollout lines") {
		t.Fatalf("the path-bearing diagnostic never fired, so this test proves nothing:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("a rollout filename forged a standalone log record — log injection:\n%s", out)
		}
	}
	// MECHANISM PIN: logsafe.Str STRIPS the CR/LF (rather than escaping it), so
	// the halves join. Removing the barrier makes this fail.
	if !strings.Contains(out, `rollout-eviltime=`) {
		t.Errorf("path not stripped+quoted via logsafe.Str (expected the halves joined as `rollout-eviltime=`):\n%s", out)
	}
}

// TestMissingSessionsRootDoesNotWarnEveryScan is the #464 Y-G1 regression. The
// old code returned nil from the walk callback for EVERY error, so WalkDir
// itself returned nil and the fs.ErrNotExist branch was unreachable dead code —
// while the real behaviour for the most ordinary case (collector enabled before
// Codex is installed) was a walk-error WARN every scan, forever.
func TestMissingSessionsRootDoesNotWarnEveryScan(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	c, logs := newLoggingCollector(t, missing, RepoTarget{Path: repo})

	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("missing sessions root: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("emitted %d events from a nonexistent sessions root", len(events))
	}
	if strings.Contains(logs.String(), "walk error") {
		t.Errorf("a missing sessions root produced a walk-error WARN; at a 5-minute cadence that is permanent log noise for a supported configuration:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "does not exist") {
		t.Errorf("the missing root must still be stated once, at INFO:\n%s", logs.String())
	}
}

// TestRepeatedParseFailureIsNotReportedForever is the #464 Y-G3 regression: a
// permanently-corrupt log used to be named at ERROR on every tick, forever.
func TestRepeatedParseFailureIsNotReportedForever(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	corrupt := synthetic(repo, testBranch, "gpt-5.6-terra",
		[]tokenUsage{{InputTokens: 100, CachedInputTokens: 999, OutputTokens: 10, TotalTokens: 110}})
	path := writeRollout(t, sessions, "corrupt.jsonl", corrupt)

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	if _, err := c.Collect(context.Background(), testSince); err == nil {
		t.Fatal("first scan must report the corrupt file")
	}
	if _, err := c.Collect(context.Background(), testSince); err != nil {
		t.Errorf("the SAME unchanged corrupt file was reported again; an unattended collector must not emit an unbounded ERROR stream about a condition that will never change: %v", err)
	}

	// It must NOT go quiet forever: a file that CHANGED is worth reporting again,
	// because the change might be the one that matters.
	if err := os.WriteFile(path, []byte(corrupt+corrupt), 0o644); err != nil {
		t.Fatalf("grow corrupt file: %v", err)
	}
	if _, err := c.Collect(context.Background(), testSince); err == nil {
		t.Error("a corrupt file that CHANGED must be reported again — suppression is per (path, size, mtime), not permanent")
	}
}

// TestOneUnresolvableRepoDoesNotBlindTheScan is the #464 Y2 regression: a repo
// target that cannot be resolved (renamed, unmounted, .git removed) used to
// abort the entire scan, silently stopping capture for every OTHER repo.
func TestOneUnresolvableRepoDoesNotBlindTheScan(t *testing.T) {
	good := initGitRepo(t, t.TempDir())
	bad := filepath.Join(t.TempDir(), "not-a-repo") // exists nowhere
	sessions := t.TempDir()
	stageFixture(t, sessions, cleanFixture, good, testBranch)

	c := newTestCollector(t, sessions, RepoTarget{Path: bad}, RepoTarget{Path: good})
	events, err := c.Collect(context.Background(), testSince)
	if err == nil {
		t.Error("the unresolvable repo must still be named in the error")
	} else if !strings.Contains(err.Error(), "not-a-repo") {
		t.Errorf("error = %q, want it to name the failing repo", err)
	}
	if len(events) != cleanBillableCalls {
		t.Fatalf("healthy repo emitted %d events, want %d — one unresolvable target must not blind the scan",
			len(events), cleanBillableCalls)
	}
}

// TestOneSessionIDInTwoFilesWarns (#464 Y8): the idempotency key is
// (session id, ordinal) with no file component, so a session id appearing in two
// files would have the second file's calls silently swallowed by the store's
// unique index. The assumption that this cannot happen is verified, not
// enforced — so it must be loud if it ever breaks.
func TestOneSessionIDInTwoFilesWarns(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	body := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})
	writeRollout(t, sessions, "a.jsonl", body)
	writeRollout(t, sessions, "b.jsonl", body) // same synthetic session id

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	if _, err := c.Collect(context.Background(), testSince); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(logs.String(), "TWO rollout files") {
		t.Errorf("a session id seen in two files must warn — otherwise the second file's spend vanishes into the unique index with no signal:\n%s", logs.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The scan cursor (#464 R3)
// ─────────────────────────────────────────────────────────────────────────────

// TestScanCursorBoundsLaterPasses proves the cursor actually stops the re-walk.
//
// The proof is deliberately indirect and therefore strong: after a clean pass the
// file is REPLACED with unparseable bytes and back-dated. If the next pass still
// opened it, the parse would fail loudly. Silence means the file was never
// reopened — which is the property (bounded work), not a proxy for it.
func TestScanCursorBoundsLaterPasses(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	path := writeRollout(t, sessions, "spaced.jsonl", syntheticSpaced(repo, testBranch, "gpt-5.6-terra",
		[]tokenUsage{usage(1000, 400, 100, 40), usage(2500, 1200, 260, 90)}))

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	c.interval = time.Second // lag = 2s, so the safety overlap is negligible here

	var ingested []collector.TokenEvent
	sink := collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		ingested = append(ingested, ev)
		return nil
	})

	c.setCursor(scanWindow{}) // zero == the first pass's full backfill
	c.runPass(context.Background(), sink)
	if len(ingested) != 2 {
		t.Fatalf("first pass ingested %d events, want 2 (%s)", len(ingested), logs.String())
	}

	// Replace the contents with garbage AND back-date the file well before the
	// cursor. A pass that still parses it fails loudly; a bounded pass never
	// looks at it.
	if err := os.WriteFile(path, []byte("{not json}\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	before := len(ingested)
	logs.Reset()
	c.runPass(context.Background(), sink)
	if len(ingested) != before {
		t.Errorf("second pass ingested %d new events from a file untouched since the cursor", len(ingested)-before)
	}
	if strings.Contains(logs.String(), "skipped malformed") || strings.Contains(logs.String(), "scan reported failing") {
		t.Errorf("the second pass re-opened a file whose mtime predates the cursor — the scan is still unbounded in history size:\n%s", logs.String())
	}
}

// TestScanCursorDoesNotAdvanceOnIngestFailure is the ordering constraint the
// cursor MUST honour: if the ingest aborted, the un-ingested tail has to be
// re-scanned. Advancing anyway would drop that spend permanently, and nothing
// downstream would ever know it existed.
func TestScanCursorDoesNotAdvanceOnIngestFailure(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	path := writeRollout(t, sessions, "spaced.jsonl", syntheticSpaced(repo, testBranch, "gpt-5.6-terra",
		[]tokenUsage{usage(1000, 400, 100, 40), usage(2500, 1200, 260, 90)}))
	// Back-date the file well beyond the cursor's safety lag. This is what makes
	// the assertion SHARP: a cursor that wrongly advanced past a failed ingest
	// would put the mtime floor after this file's mtime, and the second pass
	// would never reopen it — which is exactly the permanent spend loss under
	// test. Without the back-dating the lag window hides the bug.
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	c.interval = time.Second

	failing := collector.IngesterFunc(func(_ context.Context, _ collector.TokenEvent) error {
		return fmt.Errorf("store is refusing writes")
	})
	c.setCursor(scanWindow{})
	c.runPass(context.Background(), failing)

	var got []collector.TokenEvent
	ok := collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		got = append(got, ev)
		return nil
	})
	c.runPass(context.Background(), ok)
	if len(got) != 2 {
		t.Errorf("after a failed ingest the next pass produced %d events, want all 2 — the cursor advanced over spend that was never stored", len(got))
	}
}
