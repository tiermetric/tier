package collector

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// initEmptyGitRepo turns dir into a valid (empty-history) git repository.
// We use `git init` rather than just creating a .git/ directory so that the
// `git log` invocation inside Collect succeeds rather than failing with
// "fatal: not a git repository" (exit 128). Tests that depend on this helper
// skip on environments without the `git` binary on PATH (e.g. minimal CI
// containers) rather than failing with a confusing exec error.
func initEmptyGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not on PATH: %v", err)
	}
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v\n%s", dir, err, out)
	}
}

// writeJSONL writes the given lines (already JSON-encoded) to a temporary file
// and returns its path. The caller is responsible for cleanup via t.TempDir.
func writeJSONL(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write JSONL: %v", err)
	}
	return path
}

// appendLine appends one already-JSON-encoded line (plus a newline) to an
// existing JSONL file, mirroring how Claude Code grows a live session that the
// watcher tails incrementally.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestParseSessionFile_DedupsByMessageID is the core regression test for the
// streaming-placeholder undercount bug (gille.ai, Apr 2026). Claude Code emits
// many assistant entries per response — placeholders mid-stream, then a final
// entry with the definitive usage. Same message.id, different totals. Naive
// summing inflates cache fields and undercounts non-cache tokens. The fix:
// dedup by message.id, keep the largest-total entry per id.
func TestParseSessionFile_DedupsByMessageID(t *testing.T) {
	dir := t.TempDir()
	// Two distinct messages (msg_A, msg_B), each with one placeholder and one
	// definitive entry. Expected aggregate counts the definitive entries only.
	lines := []string{
		// msg_A — placeholder (streaming chunk): input=1 placeholder, low output
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-1","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_A","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		// msg_A — definitive (post-stream): the real numbers
		`{"type":"assistant","timestamp":"2026-05-18T10:00:05Z","sessionId":"sess-1","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_A","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":200,"cache_read_input_tokens":100}}}`,
		// msg_B — definitive only (no placeholder)
		`{"type":"assistant","timestamp":"2026-05-18T10:01:00Z","sessionId":"sess-1","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_B","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":300,"output_tokens":150,"cache_creation_input_tokens":0,"cache_read_input_tokens":50}}}`,
		// msg_B — placeholder arriving AFTER the definitive one (out-of-order safety)
		`{"type":"assistant","timestamp":"2026-05-18T10:01:01Z","sessionId":"sess-1","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_B","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":0,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	path := writeJSONL(t, dir, "session1.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}

	// Definitive sums: input = 1000+300, output = 500+150, cacheWrite = 200,
	// cacheRead = 100+50. Naive sum would inflate these significantly.
	// These fixtures have no nested cache_creation object, so the parser
	// buckets all writes into the 5m TTL (issue #55 legacy fallback).
	wantInput := 1000 + 300
	wantOutput := 500 + 150
	wantCacheWrite5m := 200
	wantCacheRead := 100 + 50

	if s.InputTok != wantInput {
		t.Errorf("InputTok = %d, want %d (naive sum would have counted placeholders)", s.InputTok, wantInput)
	}
	if s.OutputTok != wantOutput {
		t.Errorf("OutputTok = %d, want %d", s.OutputTok, wantOutput)
	}
	if s.CacheWrite5m != wantCacheWrite5m {
		t.Errorf("CacheWrite5m = %d, want %d (legacy fallback should bucket here)", s.CacheWrite5m, wantCacheWrite5m)
	}
	if s.CacheWrite1h != 0 {
		t.Errorf("CacheWrite1h = %d, want 0 (no nested cache_creation in fixture)", s.CacheWrite1h)
	}
	if s.CacheRead != wantCacheRead {
		t.Errorf("CacheRead = %d, want %d", s.CacheRead, wantCacheRead)
	}
	if s.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want claude-sonnet-4", s.Model)
	}
	if s.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", s.SessionID)
	}
}

// TestParseSessionFile_MessageIDless covers older JSONL formats or malformed
// entries that lack a message.id. Each such entry must be counted exactly once
// (the fallback per-file sequence key), not collapsed into one slot.
// TestParseSessionFile_ParsesCacheCreationTTLSplit verifies the parser reads
// Anthropic's nested cache_creation object (5m / 1h TTL split) when present,
// and falls back to bucketing the rolled-up cache_creation_input_tokens into
// the 5m bucket when the nested object is absent (legacy entries). This is
// the entry point for accurate Anthropic pricing per issue #55.
func TestParseSessionFile_ParsesCacheCreationTTLSplit(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		// Nested object, 5m-only — exercises the explicit-5m bucket path.
		`{"type":"assistant","timestamp":"2026-05-27T10:00:00Z","sessionId":"sess-ttl","gitBranch":"feature/55-cache-ttl","cwd":"/repo","message":{"id":"msg_5m","model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0}}}}`,
		// Nested object, 1h-only — what the user's "everything to 1h" setting produces.
		`{"type":"assistant","timestamp":"2026-05-27T10:01:00Z","sessionId":"sess-ttl","gitBranch":"feature/55-cache-ttl","cwd":"/repo","message":{"id":"msg_1h","model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":500,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":500}}}}`,
		// Nested object with both buckets populated — the mixed case.
		`{"type":"assistant","timestamp":"2026-05-27T10:02:00Z","sessionId":"sess-ttl","gitBranch":"feature/55-cache-ttl","cwd":"/repo","message":{"id":"msg_mix","model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":300,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":250}}}}`,
		// No nested object (legacy) — must fall back to 5m bucket per #55.
		`{"type":"assistant","timestamp":"2026-05-27T10:03:00Z","sessionId":"sess-ttl","gitBranch":"feature/55-cache-ttl","cwd":"/repo","message":{"id":"msg_legacy","model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":42,"cache_read_input_tokens":0}}}`,
		// Explicit JSON null — encoding/json leaves the pointer nil, so this
		// must behave identically to the legacy-no-field case (#55).
		`{"type":"assistant","timestamp":"2026-05-27T10:04:00Z","sessionId":"sess-ttl","gitBranch":"feature/55-cache-ttl","cwd":"/repo","message":{"id":"msg_null","model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":17,"cache_read_input_tokens":0,"cache_creation":null}}}`,
	}
	path := writeJSONL(t, dir, "session_ttl.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}

	// 5m sum: 100 (msg_5m) + 0 (msg_1h) + 50 (msg_mix) + 42 (msg_legacy) + 17 (msg_null fallback) = 209
	// 1h sum: 0 + 500 + 250 + 0 + 0 = 750
	wantW5m := 100 + 0 + 50 + 42 + 17
	wantW1h := 0 + 500 + 250 + 0 + 0
	if s.CacheWrite5m != wantW5m {
		t.Errorf("CacheWrite5m = %d, want %d (5m + legacy-fallback + null-fallback)", s.CacheWrite5m, wantW5m)
	}
	if s.CacheWrite1h != wantW1h {
		t.Errorf("CacheWrite1h = %d, want %d (1h-only + mixed)", s.CacheWrite1h, wantW1h)
	}

	// Per-message check on the legacy fallback: rolled-up 42 → 5m=42, 1h=0.
	// Locate by messageID — Messages slice order is timestamp-sorted, but we
	// don't rely on that here.
	var legacy *messageUsage
	for i := range s.Messages {
		if s.Messages[i].messageID == "msg_legacy" {
			legacy = &s.Messages[i]
			break
		}
	}
	if legacy == nil {
		t.Fatal("msg_legacy not found in parsed session")
	}
	if legacy.cacheWrite5m != 42 || legacy.cacheWrite1h != 0 {
		t.Errorf("legacy fallback: 5m=%d/1h=%d, want 42/0 (rolled-up should bucket to 5m)",
			legacy.cacheWrite5m, legacy.cacheWrite1h)
	}
}

func TestParseSessionFile_MessageIDless(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-2","gitBranch":"feature/7-bar","cwd":"/repo","message":{"model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"assistant","timestamp":"2026-05-18T10:00:10Z","sessionId":"sess-2","gitBranch":"feature/7-bar","cwd":"/repo","message":{"model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":200,"output_tokens":75,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	path := writeJSONL(t, dir, "session2.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	if s.InputTok != 300 || s.OutputTok != 125 {
		t.Errorf("got InputTok=%d OutputTok=%d, want 300/125 (each ID-less entry counted once)", s.InputTok, s.OutputTok)
	}
}

// TestParseSessionFile_CWDFromLaterLine is the #96 regression: current Claude
// Code session files lead with a last-prompt/permission-mode entry that carries
// a sessionId but NO cwd; the real cwd lands on later user/attachment lines.
// The parser used to latch all metadata (sessionId, cwd, branch, start) off the
// first line, so cwd stayed empty and filterSessionsByRepo dropped the whole
// session as foreign — recording $0 for every current-format session. cwd and
// startTime must be captured from the first line that actually carries each.
func TestParseSessionFile_CWDFromLaterLine(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir() // a real dir so filterSessionsByRepo's EvalSymlinks works
	// Mirror the current format: leading non-cwd lines that DO carry sessionId,
	// then the cwd-bearing attachment/user/assistant lines.
	lines := []string{
		`{"type":"last-prompt","timestamp":"2026-06-18T09:00:00Z","sessionId":"sess-96"}`,
		`{"type":"permission-mode","sessionId":"sess-96","permissionMode":"default"}`,
		fmt.Sprintf(`{"type":"attachment","timestamp":"2026-06-18T09:00:01Z","sessionId":"sess-96","cwd":%q}`, repo),
		fmt.Sprintf(`{"type":"user","timestamp":"2026-06-18T09:00:02Z","sessionId":"sess-96","gitBranch":"feature/96-foo","cwd":%q}`, repo),
		fmt.Sprintf(`{"type":"assistant","timestamp":"2026-06-18T09:00:03Z","sessionId":"sess-96","gitBranch":"feature/96-foo","cwd":%q,"message":{"id":"msg_A","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo),
	}
	path := writeJSONL(t, dir, "current-format.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	if s.CWD != repo {
		t.Errorf("CWD = %q, want %q (cwd must be read from the later attachment line, not the first)", s.CWD, repo)
	}
	if s.SessionID != "sess-96" {
		t.Errorf("SessionID = %q, want sess-96", s.SessionID)
	}
	if s.GitBranch != "feature/96-foo" {
		t.Errorf("GitBranch = %q, want feature/96-foo", s.GitBranch)
	}
	// StartTime must come from the first line that has a timestamp (the
	// last-prompt at 09:00:00), not be zero just because that line had no cwd.
	wantStart := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	if !s.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v (first line with a timestamp)", s.StartTime, wantStart)
	}
	if s.InputTok != 100 || s.OutputTok != 50 {
		t.Errorf("got InputTok=%d OutputTok=%d, want 100/50", s.InputTok, s.OutputTok)
	}

	// End-to-end: the session must survive the repo filter that previously
	// dropped it. This is the bug's actual blast radius.
	kept := filterSessionsByRepo([]sessionSummary{*s}, repo)
	if len(kept) != 1 {
		t.Fatalf("filterSessionsByRepo kept %d sessions, want 1 (empty cwd would drop it)", len(kept))
	}
}

// TestParseSessionFileFromOffset_CWDResumePreserved covers the #96 fix on the
// watcher's live/incremental path. The first debounce parses the
// current-format leading lines + first assistant entry from offset 0 and must
// capture cwd; the next debounce resumes from the saved offset with cached
// metadata and must KEEP that cwd (the new latch is emptiness-guarded, so it
// never clobbers good cached metadata) while still counting the appended
// tokens.
func TestParseSessionFileFromOffset_CWDResumePreserved(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	lines := []string{
		`{"type":"last-prompt","timestamp":"2026-06-18T09:00:00Z","sessionId":"sess-96r"}`,
		`{"type":"permission-mode","sessionId":"sess-96r","permissionMode":"default"}`,
		fmt.Sprintf(`{"type":"user","timestamp":"2026-06-18T09:00:01Z","sessionId":"sess-96r","gitBranch":"feature/96-foo","cwd":%q}`, repo),
		fmt.Sprintf(`{"type":"assistant","timestamp":"2026-06-18T09:00:02Z","sessionId":"sess-96r","gitBranch":"feature/96-foo","cwd":%q,"message":{"id":"msg_A","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo),
	}
	path := writeJSONL(t, dir, "resume.jsonl", lines)

	// First debounce: full parse from offset 0, tailMode (live file).
	s1, off1, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if s1 == nil {
		t.Fatal("first parse: expected a session, got nil")
	}
	if s1.CWD != repo {
		t.Fatalf("first parse CWD = %q, want %q", s1.CWD, repo)
	}
	meta := sessionMetadata{
		SessionID:    s1.SessionID,
		GitBranch:    s1.GitBranch,
		CWD:          s1.CWD,
		StartTime:    s1.StartTime,
		Model:        s1.Model,
		NextParseSeq: s1.NextParseSeq,
	}

	// Append a second assistant entry, then resume from the saved offset.
	appendLine(t, path, fmt.Sprintf(`{"type":"assistant","timestamp":"2026-06-18T09:00:05Z","sessionId":"sess-96r","gitBranch":"feature/96-foo","cwd":%q,"message":{"id":"msg_B","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo))

	s2, _, err := parseSessionFileFromOffset(path, off1, meta, true)
	if err != nil {
		t.Fatalf("resume parse: %v", err)
	}
	if s2 == nil {
		t.Fatal("resume parse: expected a session, got nil")
	}
	if s2.CWD != repo {
		t.Errorf("resume CWD = %q, want %q (cached cwd must survive the resume)", s2.CWD, repo)
	}
	if s2.InputTok != 7 || s2.OutputTok != 3 {
		t.Errorf("resume counted Input=%d Output=%d, want 7/3 (only the appended chunk)", s2.InputTok, s2.OutputTok)
	}
}

// TestParseSessionFile_NoCWDStillDropped pins the negative: a session with a
// valid branch and assistant usage but NO cwd on any line must still surface an
// empty CWD and be dropped by filterSessionsByRepo. Guards against the #96 fix
// accidentally making cwd "sticky" or defaulting it to something matchable.
func TestParseSessionFile_NoCWDStillDropped(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	lines := []string{
		`{"type":"last-prompt","timestamp":"2026-06-18T09:00:00Z","sessionId":"sess-nocwd"}`,
		`{"type":"assistant","timestamp":"2026-06-18T09:00:02Z","sessionId":"sess-nocwd","gitBranch":"feature/96-foo","message":{"id":"msg_A","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	path := writeJSONL(t, dir, "nocwd.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	if s.CWD != "" {
		t.Errorf("CWD = %q, want empty (no line carried a cwd)", s.CWD)
	}
	if kept := filterSessionsByRepo([]sessionSummary{*s}, repo); len(kept) != 0 {
		t.Errorf("filterSessionsByRepo kept %d sessions, want 0 (empty cwd is unattributable)", len(kept))
	}
}

// TestParseSessionFile_CWDOnlyOnAssistantLine covers the variant where cwd
// appears only on the assistant entry itself (no earlier user/attachment line
// carrying it). cwd is latched before the `entry.Type != "assistant"` early
// continue, so it must still be captured.
func TestParseSessionFile_CWDOnlyOnAssistantLine(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	lines := []string{
		`{"type":"last-prompt","timestamp":"2026-06-18T09:00:00Z","sessionId":"sess-asst"}`,
		`{"type":"permission-mode","sessionId":"sess-asst","permissionMode":"default"}`,
		fmt.Sprintf(`{"type":"assistant","timestamp":"2026-06-18T09:00:02Z","sessionId":"sess-asst","gitBranch":"feature/96-foo","cwd":%q,"message":{"id":"msg_A","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo),
	}
	path := writeJSONL(t, dir, "asstcwd.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	if s.CWD != repo {
		t.Errorf("CWD = %q, want %q (cwd on the assistant line must be captured)", s.CWD, repo)
	}
	if kept := filterSessionsByRepo([]sessionSummary{*s}, repo); len(kept) != 1 {
		t.Errorf("filterSessionsByRepo kept %d sessions, want 1", len(kept))
	}
}

// TestJoinSessionsToCommits_EmitsIdempotencyKey verifies that emitted events
// carry the cross-source MessageIdempotencyKey (closes #19) when an upstream
// message id is available, and that distinct messages get distinct keys.
//
// joinSessionsToCommits is called downstream of filterSessionsByRepo in the
// real Collect path. This test exercises join in isolation, so the foreign-
// branch issue-id fallback is intentionally allowed to fire here — that
// fallback is still the correct behavior once the upstream CWD filter has
// confirmed the session belongs to the target repo.
func TestJoinSessionsToCommits_EmitsIdempotencyKey(t *testing.T) {
	sessions := []sessionSummary{
		{
			SessionID: "sess-aaa",
			GitBranch: "feature/42-foo",
			Model:     "claude-sonnet-4",
			InputTok:  100, OutputTok: 50,
			StartTime: time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 5, 18, 10, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{
					messageID: "msg_aaa_1", timestamp: time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC),
					model: "claude-sonnet-4", input: 100, output: 50,
				},
			},
		},
		{
			SessionID: "sess-bbb",
			GitBranch: "feature/99-bar",
			Model:     "claude-sonnet-4",
			InputTok:  200, OutputTok: 100,
			StartTime: time.Date(2026, 5, 18, 11, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 5, 18, 11, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{
					messageID: "msg_bbb_1", timestamp: time.Date(2026, 5, 18, 11, 0, 0, 0, time.UTC),
					model: "claude-sonnet-4", input: 200, output: 100,
				},
			},
		},
	}
	// No commits — relies on FromBranch fallback for IssueID.
	events := joinSessionsToCommits(sessions, nil, "alice", "")
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].IdempotencyKey == "" || events[1].IdempotencyKey == "" {
		t.Fatal("IdempotencyKey must be non-empty on JSONL events")
	}
	if events[0].IdempotencyKey == events[1].IdempotencyKey {
		t.Errorf("different messages produced same idempotency key")
	}
	// Cross-source convention (#19): the key is MessageIdempotencyKey(provider,
	// upstream id) — exactly what the proxy emits for the same upstream call.
	wantA := MessageIdempotencyKey(ProviderAnthropic, "msg_aaa_1")
	if events[0].IdempotencyKey != wantA {
		t.Errorf("event[0] key = %q, want %q (MessageIdempotencyKey)", events[0].IdempotencyKey, wantA)
	}
}

// TestJSONLCollector_RunStreamsToIngester covers the #27 push path: Run
// pushes each TokenEvent into the Ingester instead of returning a slice.
// The recorder captures everything so we can verify same content as
// Collect's slice form.
func TestJSONLCollector_RunStreamsToIngester(t *testing.T) {
	repo := t.TempDir()
	initEmptyGitRepo(t, repo)

	claudeDir := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-run","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_run","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	writeJSONL(t, projectsDir, "s.jsonl", []string{line})

	c := &JSONLCollector{RepoPath: repo, ClaudeDir: claudeDir, DeveloperID: "alice"}

	var captured []TokenEvent
	ing := IngesterFunc(func(_ context.Context, ev TokenEvent) error {
		captured = append(captured, ev)
		return nil
	})
	if err := c.Run(context.Background(),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), ing); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("got %d events, want 1", len(captured))
	}
	if captured[0].IssueID != "issue-1" {
		t.Errorf("captured[0].IssueID = %q, want issue-1", captured[0].IssueID)
	}

	// Collect should return the same slice — back-compat for the CLI.
	slice, err := c.Collect(context.Background(),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(slice) != 1 || slice[0].IssueID != captured[0].IssueID {
		t.Errorf("Collect/Run divergence: Collect=%+v, Run=%+v", slice, captured)
	}
}

// TestJSONLCollector_RunAndCollectAgree is the regression guard for the
// "pure refactor, no behavior change" claim of #27. With multiple events
// in the same scan, Run's forwarded sequence and Collect's returned slice
// must be byte-identical and in the same order.
func TestJSONLCollector_RunAndCollectAgree(t *testing.T) {
	repo := t.TempDir()
	initEmptyGitRepo(t, repo)
	claudeDir := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Three distinct sessions across two issues so the ordering surface is
	// non-trivial.
	lines := []string{
		fmt.Sprintf(`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-A","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_A","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo),
		fmt.Sprintf(`{"type":"assistant","timestamp":"2026-05-18T10:01:00Z","sessionId":"sess-A","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_B","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":200,"output_tokens":80,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo),
		fmt.Sprintf(`{"type":"assistant","timestamp":"2026-05-18T11:00:00Z","sessionId":"sess-B","gitBranch":"feature/2-bar","cwd":%q,"message":{"id":"msg_C","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":300,"output_tokens":120,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, repo),
	}
	writeJSONL(t, projectsDir, "sA.jsonl", lines[:2])
	writeJSONL(t, projectsDir, "sB.jsonl", lines[2:])

	c := &JSONLCollector{RepoPath: repo, ClaudeDir: claudeDir, DeveloperID: "alice"}
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	slice, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var captured []TokenEvent
	if err := c.Run(context.Background(), since, IngesterFunc(func(_ context.Context, ev TokenEvent) error {
		captured = append(captured, ev)
		return nil
	})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !reflect.DeepEqual(slice, captured) {
		t.Errorf("Run / Collect diverged (must be byte-identical for the same input):\n Collect = %+v\n Run     = %+v", slice, captured)
	}
	if len(captured) < 2 {
		t.Errorf("expected at least 2 events to exercise the ordering surface; got %d", len(captured))
	}
}

// TestJSONLCollector_RunPropagatesIngestError: when the Ingester returns an
// error, Run stops and surfaces it. Partial progress (events already
// accepted by Ingest) is not rolled back — that's the documented contract.
func TestJSONLCollector_RunPropagatesIngestError(t *testing.T) {
	repo := t.TempDir()
	initEmptyGitRepo(t, repo)
	claudeDir := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-err","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_err","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	writeJSONL(t, projectsDir, "s.jsonl", []string{line})

	c := &JSONLCollector{RepoPath: repo, ClaudeDir: claudeDir, DeveloperID: "alice"}

	wantErr := fmt.Errorf("test-ingest-failure")
	err := c.Run(context.Background(),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		IngesterFunc(func(_ context.Context, _ TokenEvent) error { return wantErr }))
	if err == nil {
		t.Fatal("expected error from Run when Ingester fails")
	}
	if !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("got %v, want error to contain %q", err, wantErr.Error())
	}
}

// TestJSONLCollector_RunRejectsNilIngester guards a configuration mistake at
// the call site rather than crashing on first event.
func TestJSONLCollector_RunRejectsNilIngester(t *testing.T) {
	c := &JSONLCollector{RepoPath: t.TempDir()}
	err := c.Run(context.Background(), time.Now(), nil)
	if err == nil {
		t.Fatal("expected error for nil Ingester")
	}
}

// TestMessageIdempotencyKey_CrossSource is the headline #19 regression test:
// the JSONL collector and the proxy must emit IDENTICAL IdempotencyKeys for
// the same upstream Anthropic message. The previous code prefixed each path
// with its source ("jsonl" vs. "proxy"), producing distinct hashes and
// silently double-counting any call captured by both.
func TestMessageIdempotencyKey_CrossSource(t *testing.T) {
	const msgID = "msg_01CrossSource"

	// JSONL-side: the key emitted by joinSessionsToCommits when a message has
	// an upstream id. Provider is hardcoded to "anthropic" because Claude
	// Code JSONL is Claude-only.
	jsonlKey := MessageIdempotencyKey(ProviderAnthropic, msgID)

	// Proxy-side: the helper the proxy uses (mirrors idempotencyKeyForProxy
	// without importing the proxy package, since that would create a cycle).
	proxyKey := MessageIdempotencyKey("anthropic", msgID)

	if jsonlKey == "" || proxyKey == "" {
		t.Fatal("both keys must be non-empty when message id is present")
	}
	if jsonlKey != proxyKey {
		t.Errorf("cross-source keys differ — same Claude call captured by JSONL + proxy would double-count\n  jsonl=%s\n  proxy=%s",
			jsonlKey, proxyKey)
	}

	// Sanity: a different message id produces a different key.
	other := MessageIdempotencyKey(ProviderAnthropic, "msg_02Different")
	if other == jsonlKey {
		t.Error("different message ids must produce different keys")
	}
}

// TestJoinSessionsToCommits_IDlessFallback covers the fallback path: messages
// without a message.id (older JSONL formats, possibly malformed entries) must
// still produce stable keys so a re-scan dedupes against itself. The key is
// source-prefixed, so it cannot collide with a proxy event — that's fine,
// because without an upstream id we can't prove they're the same call anyway.
func TestJoinSessionsToCommits_IDlessFallback(t *testing.T) {
	sessions := []sessionSummary{
		{
			SessionID: "sess-idless",
			GitBranch: "feature/42-foo",
			Model:     "claude-sonnet-4",
			StartTime: time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 5, 18, 10, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{timestamp: time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC), model: "claude-sonnet-4", input: 100, parseOrder: 0},
				{timestamp: time.Date(2026, 5, 18, 10, 1, 0, 0, time.UTC), model: "claude-sonnet-4", input: 200, parseOrder: 1},
			},
		},
	}
	events := joinSessionsToCommits(sessions, nil, "alice", "")
	if len(events) != 2 {
		t.Fatalf("expected 2 events (one per ID-less message), got %d", len(events))
	}
	if events[0].IdempotencyKey == events[1].IdempotencyKey {
		t.Errorf("two ID-less messages from same session got the same key — must differ by position")
	}
	// Re-running join with the same input must produce the same keys (stable
	// across re-scans).
	again := joinSessionsToCommits(sessions, nil, "alice", "")
	for i := range events {
		if events[i].IdempotencyKey != again[i].IdempotencyKey {
			t.Errorf("event[%d] key changed across re-scan: %q vs %q",
				i, events[i].IdempotencyKey, again[i].IdempotencyKey)
		}
	}
}

// TestParseSessionFile_SkipsMalformedLines is the core regression test for
// issue #7. The previous json.Decoder implementation could stall (and in some
// pathological cases infinite-loop) on a syntax error mid-token because the
// decoder operates on tokens, not lines. The bufio.Scanner rewrite parses each
// line independently, so a single bad line must be skipped while valid lines
// before AND after still parse correctly.
func TestParseSessionFile_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		// Valid before the bad line.
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-recover","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_A","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		// Truncated mid-string — the kind of input that would stall json.Decoder.
		`{"type":"assistant","timestamp":"2026-05-18T10:00:05Z","sessionId":"sess-recover","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_B","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":99,"output_to`,
		// Valid after — the test that proves recovery actually works.
		`{"type":"assistant","timestamp":"2026-05-18T10:00:10Z","sessionId":"sess-recover","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_C","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":200,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	path := writeJSONL(t, dir, "with-truncated-line.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	// Only msg_A and msg_C should be counted; the truncated msg_B line must
	// be skipped without polluting the totals or causing parse failure.
	wantInput := 100 + 200
	wantOutput := 50 + 100
	if s.InputTok != wantInput || s.OutputTok != wantOutput {
		t.Errorf("got InputTok=%d OutputTok=%d, want %d/%d (truncated middle line must skip cleanly)",
			s.InputTok, s.OutputTok, wantInput, wantOutput)
	}
}

// TestParseSessionFile_GarbageLinesAreSkipped covers the broader malformed-line
// surface: non-JSON garbage, partial braces, trailing whitespace, and blank
// lines all mixed in. None should poison the parse; all valid lines must land.
func TestParseSessionFile_GarbageLinesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-garbage","gitBranch":"feature/7-bar","cwd":"/repo","message":{"id":"msg_1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":50,"output_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		``,                        // empty line — must be tolerated
		`   `,                     // whitespace-only — must be tolerated (TrimSpace)
		"\t{stray-tab",            // leading tab + garbage — must be skipped
		`not json at all`,         // garbage — must be skipped
		`{ "type": "assistant", `, // partial json — must be skipped
		`{"type":"assistant","timestamp":"2026-05-18T10:00:10Z","sessionId":"sess-garbage","gitBranch":"feature/7-bar","cwd":"/repo","message":{"id":"msg_2","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":75,"output_tokens":40,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	path := writeJSONL(t, dir, "with-garbage.jsonl", lines)

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	if s.InputTok != 125 || s.OutputTok != 65 {
		t.Errorf("got InputTok=%d OutputTok=%d, want 125/65 (garbage lines must not affect totals)", s.InputTok, s.OutputTok)
	}
}

// TestParseSessionFile_AllGarbageReturnsNil locks in the "nothing to emit"
// contract: a file with no parseable assistant entries must return
// (nil, nil), not an empty summary or an error. parseAllSessions relies on
// this to skip such files without warning.
func TestParseSessionFile_AllGarbageReturnsNil(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "all-garbage.jsonl",
		[]string{"not json", "{broken", "also bad"})
	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s != nil {
		t.Errorf("want nil summary for all-garbage file, got %+v", s)
	}
}

// TestParseSessionFile_NoTrailingNewline guards a real-world scenario: a
// Claude Code process killed mid-flush leaves a JSONL file whose final line
// has no trailing '\n'. bufio.Scanner handles this correctly (returns the
// final line on the last Scan), but we want a test to catch any future
// regression where someone "improves" the parser to require terminated lines.
func TestParseSessionFile_NoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-trailing-newline.jsonl")
	// Two lines, only the first terminated. We write directly (not via
	// writeJSONL, which always appends '\n').
	content := `{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-no-nl","gitBranch":"feature/9-baz","cwd":"/repo","message":{"id":"msg_X","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":50,"output_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-05-18T10:00:10Z","sessionId":"sess-no-nl","gitBranch":"feature/9-baz","cwd":"/repo","message":{"id":"msg_Y","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":75,"output_tokens":40,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` // no trailing \n
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a summary for two-line file without trailing newline, got nil")
	}
	if s.InputTok != 125 || s.OutputTok != 65 {
		t.Errorf("got InputTok=%d OutputTok=%d, want 125/65 (final unterminated line must still parse)",
			s.InputTok, s.OutputTok)
	}
}

// TestParseSessionFile_HandlesLongLines verifies the 64 KB default bufio.Scanner
// buffer no longer truncates: a single entry that fits in 1 MB of JSON
// (well under the 10 MB cap, well above the default) must parse cleanly.
// This guards against the regression where a long Anthropic streaming response
// silently dropped because Scanner's default buffer was too small.
func TestParseSessionFile_HandlesLongLines(t *testing.T) {
	dir := t.TempDir()
	// Pad the assistant message with a long text field so the line itself is
	// ~1 MB. We don't care what's in `text` — only that the JSONL line is
	// large enough to overflow the default Scanner buffer.
	padding := strings.Repeat("x", 1<<20) // 1 MB of padding
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-long","gitBranch":"feature/8-baz","cwd":"/repo","message":{"id":"msg_long","model":"claude-sonnet-4","role":"assistant","content":%q,"usage":{"input_tokens":12345,"output_tokens":6789,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		padding,
	)
	path := writeJSONL(t, dir, "long-line.jsonl", []string{line})

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary for the long line, got nil")
	}
	if s.InputTok != 12345 || s.OutputTok != 6789 {
		t.Errorf("got InputTok=%d OutputTok=%d, want 12345/6789 (long-line must not truncate)", s.InputTok, s.OutputTok)
	}
}

// TestFilterSessionsByRepo is the regression test for #15: prior to the filter,
// `tierd score --repo X` attributed every Claude Code session on the machine to
// X via the issue-id fallback in joinSessionsToCommits. The filter must drop
// sessions outside the target repo, accept sessions in subdirectories, and
// reject empty-CWD sessions outright.
func TestFilterSessionsByRepo(t *testing.T) {
	repo := t.TempDir()
	subdir := filepath.Join(repo, "services", "foo")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	// "near miss" sibling that shares a path prefix but is not the target repo
	// — exercises the trailing-separator guard in pathInside.
	near := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-other")
	if err := os.MkdirAll(near, 0o755); err != nil {
		t.Fatalf("mkdir near: %v", err)
	}

	sessions := []sessionSummary{
		{SessionID: "in-root", CWD: repo, GitBranch: "feature/1-foo"},
		{SessionID: "in-subdir", CWD: subdir, GitBranch: "feature/2-bar"},
		{SessionID: "foreign", CWD: t.TempDir(), GitBranch: "feature/3-baz"},
		{SessionID: "near-miss", CWD: near, GitBranch: "feature/4-qux"},
		{SessionID: "empty-cwd", CWD: "", GitBranch: "feature/5-quux"},
	}

	kept := filterSessionsByRepo(sessions, repo)

	keptIDs := make(map[string]bool, len(kept))
	for _, s := range kept {
		keptIDs[s.SessionID] = true
	}

	wantKept := []string{"in-root", "in-subdir"}
	for _, id := range wantKept {
		if !keptIDs[id] {
			t.Errorf("session %q was dropped, want kept", id)
		}
	}
	wantDropped := []string{"foreign", "near-miss", "empty-cwd"}
	for _, id := range wantDropped {
		if keptIDs[id] {
			t.Errorf("session %q was kept, want dropped", id)
		}
	}
}

// TestFilterSessionsByRepo_NormalisesRepoPath covers two repoPath shapes that
// look correct to a human but break a naive prefix match:
//   - trailing separator (`--repo /Users/me/repo/`) — Clean must strip it
//   - relative path (`--repo ./repo`) — Abs must promote it, because JSONL CWDs
//     are always absolute and a relative prefix would never match
func TestFilterSessionsByRepo_NormalisesRepoPath(t *testing.T) {
	repo := t.TempDir()
	sessions := []sessionSummary{{SessionID: "s1", CWD: repo, GitBranch: "feature/1-foo"}}

	t.Run("trailing slash", func(t *testing.T) {
		kept := filterSessionsByRepo(sessions, repo+string(filepath.Separator))
		if len(kept) != 1 {
			t.Fatalf("trailing slash dropped the session: got %d kept", len(kept))
		}
	})

	t.Run("relative path", func(t *testing.T) {
		parent := filepath.Dir(repo)
		rel, err := filepath.Rel(parent, repo)
		if err != nil {
			t.Fatalf("filepath.Rel: %v", err)
		}
		// Chdir scoped to this subtest only; Cleanup restores the prior wd so
		// later tests aren't affected by our wd swap.
		oldwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		if err := os.Chdir(parent); err != nil {
			t.Fatalf("Chdir to parent: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(oldwd) })

		kept := filterSessionsByRepo(sessions, rel)
		if len(kept) != 1 {
			t.Fatalf("relative path dropped the session: got %d kept (rel=%q parent=%q)", len(kept), rel, parent)
		}
	})
}

// TestFilterSessionsByRepo_FollowsSymlinks covers the macOS-typical case where
// /Users/foo/repo is a symlink to /Volumes/data/repo. Whichever path the user
// passed to --repo and whichever path Claude Code recorded in the session must
// resolve to the same target.
func TestFilterSessionsByRepo_FollowsSymlinks(t *testing.T) {
	// Real repo lives at a t.TempDir; the symlink lives in a separate tempdir
	// so removing it cannot disturb the real directory.
	real := t.TempDir()
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "repo-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink in test environment: %v", err)
	}

	// CWD recorded as the resolved path, repo passed as the symlink — and the
	// reverse. Both directions must match.
	cases := []struct {
		name     string
		cwd      string
		repo     string
		wantKept bool
	}{
		{"cwd=real, repo=link", real, link, true},
		{"cwd=link, repo=real", link, real, true},
		{"cwd=link, repo=link", link, link, true},
		{"cwd=real, repo=real", real, real, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := []sessionSummary{{SessionID: "s1", CWD: tc.cwd, GitBranch: "feature/1-foo"}}
			kept := filterSessionsByRepo(sessions, tc.repo)
			if got := len(kept) == 1; got != tc.wantKept {
				t.Errorf("got kept=%v, want kept=%v (cwd=%q repo=%q)", got, tc.wantKept, tc.cwd, tc.repo)
			}
		})
	}
}

// TestCollect_FiltersForeignSessions is the end-to-end regression test: build
// a fake ~/.claude/projects directory with one in-repo session and one
// foreign-repo session, then run Collect against the in-repo path. Without the
// CWD filter (the bug in #15), the foreign session is attributed via its
// branch-id fallback and shows up in the output.
func TestCollect_FiltersForeignSessions(t *testing.T) {
	repo := t.TempDir()
	initEmptyGitRepo(t, repo)
	foreignRepo := t.TempDir()

	claudeDir := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	// In-repo session — should be attributed.
	inRepoLine := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-in","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_in","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	writeJSONL(t, projectsDir, "in-repo.jsonl", []string{inRepoLine})

	// Foreign session — must be excluded. Issue-id 947 was the smoking-gun
	// number from the dogfooding discovery: prior to the fix it landed on the
	// tier repo's report.
	foreignLine := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-18T11:00:00Z","sessionId":"sess-foreign","gitBranch":"feature/947-not-tier","cwd":%q,"message":{"id":"msg_foreign","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":9999999,"output_tokens":9999999,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		foreignRepo,
	)
	writeJSONL(t, projectsDir, "foreign.jsonl", []string{foreignLine})

	c := &JSONLCollector{
		RepoPath:    repo,
		ClaudeDir:   claudeDir,
		DeveloperID: "alice",
	}

	events, err := c.Collect(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (foreign session must be excluded)", len(events))
	}
	if events[0].IssueID == "issue-947" {
		t.Errorf("foreign session leaked into output as %s", events[0].IssueID)
	}
	if events[0].InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (foreign 9_999_999 leaked into totals)", events[0].InputTok)
	}
}

// TestFilterSessionsByRepo_KeepsWorktreeSessions is the P0-11 regression: a
// session whose CWD is a git worktree of the target repo (CWD outside the
// repo's path prefix) must be kept via the worktree-root fallback, while a
// session in an UNRELATED repo's worktree stays dropped — proving the fallback
// maps cwd→root for identity but does not weaken the authorization decision.
func TestFilterSessionsByRepo_KeepsWorktreeSessions(t *testing.T) {
	repoA := t.TempDir()
	initGitRepoWithCommit(t, repoA)
	// Worktree of A under a DIFFERENT parent dir, so its CWD is outside A's
	// path prefix and only the worktree fallback can rescue it.
	wtA := addWorktree(t, repoA, filepath.Join(t.TempDir(), "wtA"), "feature/1-foo")
	subA := filepath.Join(wtA, "services", "x")
	if err := os.MkdirAll(subA, 0o755); err != nil {
		t.Fatalf("mkdir subA: %v", err)
	}

	// Unrelated repo B and a worktree of it — the control. B is not the target,
	// so its worktree session must be dropped as foreign.
	repoB := t.TempDir()
	initGitRepoWithCommit(t, repoB)
	wtB := addWorktree(t, repoB, filepath.Join(t.TempDir(), "wtB"), "feature/2-bar")

	sessions := []sessionSummary{
		{SessionID: "wt-subdir-of-A", CWD: subA, GitBranch: "feature/1-foo"},
		{SessionID: "wt-of-B", CWD: wtB, GitBranch: "feature/2-bar"},
	}
	kept := filterSessionsByRepo(sessions, repoA)

	keptIDs := map[string]bool{}
	for _, s := range kept {
		keptIDs[s.SessionID] = true
	}
	if !keptIDs["wt-subdir-of-A"] {
		t.Errorf("worktree session of target repo A was dropped, want kept")
	}
	if keptIDs["wt-of-B"] {
		t.Errorf("worktree session of unrelated repo B was kept — identity check leaked")
	}
}

// TestFilterSessionsByRepo_RemovedWorktreeStillDropped pins the documented
// residual limit: once a worktree is removed from disk its .git file is gone,
// so the fallback cannot resolve it and the session is dropped without error.
func TestFilterSessionsByRepo_RemovedWorktreeStillDropped(t *testing.T) {
	repo := t.TempDir()
	initGitRepoWithCommit(t, repo)
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt-gone"), "feature/9-gone")
	// Remove the worktree from disk — the .git file no longer exists.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}

	sessions := []sessionSummary{{SessionID: "gone", CWD: wt, GitBranch: "feature/9-gone"}}
	if kept := filterSessionsByRepo(sessions, repo); len(kept) != 0 {
		t.Errorf("removed-worktree session kept %d, want 0 (residual limit)", len(kept))
	}
}

// Benchmarks for #30: quantifies the incremental-parse speedup.
//
// Setup builds a ~10 MB JSONL session (the realistic upper bound for a
// long-running Claude Code session — typical files are well under 1 MB).
// Each benchmark iteration appends a single new line and measures the
// time to either (a) re-parse the whole file from byte 0 (the pre-#30
// path) or (b) parse only the appended bytes from the cached offset
// (the post-#30 path).
//
// Expected result: BenchmarkParseSession_Incremental should be 100×+
// faster than BenchmarkParseSession_FullReparse on a 10 MB file, because
// it scans ~1 KB (one new line) instead of ~10 MB. Run with:
//
//	go test -bench=ParseSession -benchmem ./internal/collector/...
func BenchmarkParseSession_FullReparse(b *testing.B) {
	path, _ := buildLargeSessionFile(b, 10<<20) // ~10 MB target
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Always start from offset 0 — simulates the pre-#30 watcher,
		// which re-parsed the entire file on every debounce.
		_, _, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}

func BenchmarkParseSession_Incremental(b *testing.B) {
	// Build a 10 MB file ONCE with one extra appended line at the end.
	// The benchmark then repeatedly parses just that last line via
	// (offset, meta) that point at it. No per-iteration file growth —
	// keeps the measurement stable across high b.N and across
	// -benchtime values, and avoids contaminating the timer with
	// open/write/close syscalls between iterations.
	path, _ := buildLargeSessionFile(b, 10<<20)
	s, baseOffset, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
	if err != nil || s == nil {
		b.Fatalf("seed parse: err=%v s=%v", err, s)
	}
	meta := sessionMetadata{
		SessionID:    s.SessionID,
		GitBranch:    s.GitBranch,
		CWD:          s.CWD,
		StartTime:    s.StartTime,
		Model:        s.Model,
		NextParseSeq: s.NextParseSeq,
	}
	// Append exactly one line so every iteration parses the same chunk
	// shape (one new message). We never advance the saved offset between
	// iterations, so the work is identical and the measurement reflects
	// the steady-state incremental cost of one appended line.
	newLine := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T12:00:00Z","sessionId":%q,"gitBranch":"feature/1-foo","cwd":"/tmp/r","message":{"id":"msg_bench_new","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		s.SessionID,
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(newLine); err != nil {
		b.Fatalf("append: %v", err)
	}
	_ = f.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := parseSessionFileFromOffset(path, baseOffset, meta, true)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}

// buildLargeSessionFile writes a JSONL file totalling roughly targetBytes
// by repeating the same assistant-entry template with incrementing
// message ids. Returns the path and the number of lines written.
func buildLargeSessionFile(tb testing.TB, targetBytes int) (string, int) {
	tb.Helper()
	dir := tb.TempDir()
	path := filepath.Join(dir, "bench.jsonl")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	const tmpl = `{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-bench","gitBranch":"feature/1-foo","cwd":"/tmp/r","message":{"id":"msg_bench_%d","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"

	var written, count int
	for written < targetBytes {
		line := fmt.Sprintf(tmpl, count)
		n, err := f.WriteString(line)
		if err != nil {
			tb.Fatalf("write: %v", err)
		}
		written += n
		count++
	}
	return path, count
}

// TestParseSessionFile_NegativeUsageClamped is the JSONL-boundary regression
// for #121: a hostile or malformed session line carrying negative usage counts
// must clamp per-field to zero (keeping the event) rather than flow a negative
// cost into the store. Fails on main, where u.InputTokens is copied unchecked.
func TestParseSessionFile_NegativeUsageClamped(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-neg","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_neg","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":-500,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	path := writeJSONL(t, dir, "neg.jsonl", []string{line})

	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session summary, got nil")
	}
	if s.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (negative input must clamp)", s.InputTok)
	}
	if s.OutputTok != 200 {
		t.Errorf("OutputTok = %d, want 200 (valid field untouched)", s.OutputTok)
	}
	if len(s.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(s.Messages))
	}
	m := s.Messages[0]
	if m.input != 0 || m.output != 200 {
		t.Errorf("message fields input=%d output=%d, want 0/200", m.input, m.output)
	}
	// The downstream event cost is ComputeCost of the CLAMPED values — never
	// negative. On main the unclamped -500 input yields a negative cost.
	cost := store.ComputeCost(m.model, store.CostUsage{
		Input:        m.input,
		Output:       m.output,
		CacheRead:    m.cacheRead,
		CacheWrite5m: m.cacheWrite5m,
		CacheWrite1h: m.cacheWrite1h,
	})
	if cost < 0 {
		t.Errorf("CostMicro = %d, want >= 0 after clamp", cost)
	}
}

// commitAtDate creates a single empty commit in an already-initialised git repo,
// pinning both the author and committer dates to isoDate (an ISO8601 string with
// an explicit offset, e.g. "2026-01-02T00:30:00+00:00"). Pinning via the
// GIT_AUTHOR_DATE / GIT_COMMITTER_DATE env vars makes the commit's instant
// deterministic regardless of the host clock or TZ, which is what lets the
// --since window tests below assert an exact commit count.
func commitAtDate(t *testing.T, dir, isoDate, subject string) {
	t.Helper()
	cmd := exec.Command("git",
		"-c", "user.name=tier-test",
		"-c", "user.email=tier-test@example.com",
		"commit", "--quiet", "--allow-empty", "-m", subject,
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+isoDate,
		"GIT_COMMITTER_DATE="+isoDate,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit (%s): %v\n%s", subject, err, out)
	}
}

// setHostTZMinus12 pins the process (and any child git) timezone to UTC−12 via
// the inverted-sign Etc/GMT+12 zone, skipping the test if the host tzdata cannot
// resolve that zone to the expected offset. The guard is load-bearing for the
// "fails on main" property: without it, a host missing the zone (e.g. a stripped
// container) silently falls back to UTC, at which point a zone-less --since is
// read as UTC and the bug is no longer exercised — the test would pass vacuously
// even on unfixed code. git reads the same system zoneinfo as time.LoadLocation
// (this build does not import time/tzdata), so a load failure here predicts
// git's UTC fallback, making the skip an honest signal rather than a false green.
func setHostTZMinus12(t *testing.T) {
	t.Helper()
	loc, err := time.LoadLocation("Etc/GMT+12")
	if err != nil {
		t.Skipf("Etc/GMT+12 zone unavailable in host tzdata: %v", err)
	}
	if _, offset := time.Now().In(loc).Zone(); offset != -12*60*60 {
		t.Skipf("Etc/GMT+12 resolved to offset %ds, want -43200 (UTC−12)", offset)
	}
	t.Setenv("TZ", "Etc/GMT+12") // inherited by the child git process
}

// TestGitLog_SinceIsTimezoneIndependent pins the regression for issue #119: the
// --since timestamp must carry an explicit UTC offset so git does not reparse a
// zone-less string in the host-local timezone.
//
// This test FAILS on main. Construction and arithmetic:
//   - TZ=Etc/GMT+12: the Etc/GMT zones have INVERTED signs, so Etc/GMT+12 is the
//     UTC−12 offset. t.Setenv propagates TZ into the child git process, which is
//     what makes the failure deterministic. A negative offset is required: git
//     reads main's zone-less --since string as a LATER instant, so main
//     OVER-excludes (returns 0) — the observable failure. A positive offset would
//     make main over-include instead, which this since choice cannot detect.
//   - The commit is pinned at 2026-01-02T00:30:00+00:00 (00:30 UTC).
//   - since is 2026-01-02T00:00:00 UTC — 30 minutes BEFORE the commit in real
//     time — so the commit MUST be inside the window.
//   - On main, gitLog emits --since=2026-01-02T00:00:00 (no zone). git under
//     TZ=UTC−12 reads that as 2026-01-02T00:00:00−12:00 = 2026-01-02T12:00:00Z,
//     which is 11.5h AFTER the 00:30Z commit, so the commit is excluded → 0
//     commits returned → test fails.
//   - With the fix, gitLog emits --since=2026-01-02T00:00:00+00:00, read as
//     00:00Z regardless of TZ, so the commit is inside the window → 1 commit.
func TestGitLog_SinceIsTimezoneIndependent(t *testing.T) {
	setHostTZMinus12(t) // Etc sign is inverted → UTC−12; skips if tzdata lacks it
	dir := t.TempDir()
	initEmptyGitRepo(t, dir)
	commitAtDate(t, dir, "2026-01-02T00:30:00+00:00", "commit inside window")

	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) // 30m before the commit
	commits, err := gitLog(context.Background(), dir, since)
	if err != nil {
		t.Fatalf("gitLog: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("gitLog returned %d commits, want 1 (main over-excludes under TZ=UTC−12 because the zone-less --since is read as a later local instant)", len(commits))
	}
}

// TestGitLog_SinceExcludesOlderCommits is the control for the fix: under the same
// negative-offset TZ, a commit dated a full day BEFORE the window must NOT be
// returned. This pins the other edge — it proves the fix corrected the offset
// rather than widening the window to include everything.
func TestGitLog_SinceExcludesOlderCommits(t *testing.T) {
	setHostTZMinus12(t) // same UTC−12 host zone as the sibling test
	dir := t.TempDir()
	initEmptyGitRepo(t, dir)
	// One commit a full day before the window, one 30m after it.
	commitAtDate(t, dir, "2026-01-01T00:00:00+00:00", "commit before window")
	commitAtDate(t, dir, "2026-01-02T00:30:00+00:00", "commit inside window")

	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	commits, err := gitLog(context.Background(), dir, since)
	if err != nil {
		t.Fatalf("gitLog: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("gitLog returned %d commits, want exactly 1 (the pre-window commit must be excluded)", len(commits))
	}
	if commits[0].Subject != "commit inside window" {
		t.Fatalf("gitLog returned subject %q, want %q", commits[0].Subject, "commit inside window")
	}
}

// TestJoinSessionsToCommits_UnattributableSessionCostLandsOnDeveloper is the
// #120 regression: a session that resolves to no issue (branch `main`, no
// matching commit) must NOT be silently dropped. Spec section 4.4 requires its
// cost to land on the developer under issue_id "unattributed" so unfocused /
// gaming exploration inflates the denominator. On main this session was
// `continue`d and its per-message cost vanished.
func TestJoinSessionsToCommits_UnattributableSessionCostLandsOnDeveloper(t *testing.T) {
	sessions := []sessionSummary{
		{
			SessionID: "sess-unattr",
			GitBranch: "main",
			Model:     "claude-sonnet-4",
			StartTime: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{
					messageID: "m1", timestamp: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
					model: "claude-sonnet-4", input: 1000, output: 500,
				},
				{
					messageID: "m2", timestamp: time.Date(2026, 6, 1, 10, 1, 0, 0, time.UTC),
					model: "claude-sonnet-4", input: 2000, output: 800,
				},
			},
		},
	}
	// Empty commit slice: no commit match, and FromBranch("main") == "" — the
	// classic unattributable case.
	events := joinSessionsToCommits(sessions, nil, "dev", "")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (per-message emission for the unattributable session)", len(events))
	}
	for i, e := range events {
		if e.Developer != "dev" {
			t.Errorf("event[%d].Developer = %q, want dev", i, e.Developer)
		}
		// #refocus, Option B: a main-branch no-issue event now lands in the
		// LABELED exploratory bucket (a member of the unattributed family), not the
		// bare sentinel — its cost still enters the developer's denominator.
		if !IsUnattributed(e.IssueID) {
			t.Errorf("event[%d].IssueID = %q, want an unattributed-family id", i, e.IssueID)
		}
		if e.IssueID != UnattributedMain {
			t.Errorf("event[%d].IssueID = %q, want %q (main is exploratory)", i, e.IssueID, UnattributedMain)
		}
		if e.CostMicro <= 0 {
			t.Errorf("event[%d].CostMicro = %d, want > 0 (real usage must carry real cost)", i, e.CostMicro)
		}
	}
}

// TestJoinSessionsToCommits_AttributedSessionsUnchanged pins that the #120
// fallback does not over-apply: a real issue branch still resolves to its issue
// id, not to "unattributed".
func TestJoinSessionsToCommits_AttributedSessionsUnchanged(t *testing.T) {
	sessions := []sessionSummary{
		{
			SessionID: "sess-attr",
			GitBranch: "feature/42-x",
			Model:     "claude-sonnet-4",
			StartTime: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{
					messageID: "m1", timestamp: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
					model: "claude-sonnet-4", input: 1000, output: 500,
				},
			},
		},
	}
	events := joinSessionsToCommits(sessions, nil, "dev", "")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].IssueID != "issue-42" {
		t.Errorf("IssueID = %q, want issue-42 (branch-name fallback, not the unattributed sentinel)", events[0].IssueID)
	}
	if events[0].IssueID == UnattributedIssueID {
		t.Error("attributed session was routed to the unattributed sentinel — fallback over-applied")
	}
}

// TestJoinSessionsToCommits_PerMessageBranchAttribution is the core #refocus
// Option A regression: a session that OPENS on main (the latched session branch)
// and later checks out a feature branch must attribute each message by ITS OWN
// branch. Under the old one-branch-per-session latch, the whole session routed to
// the latched `main` → unattributed, and the feature-branch work vanished from its
// issue. Now the main messages land in the exploratory bucket and the feature
// messages attribute to their issue — and the session's TOTAL cost is unchanged
// (nothing is dropped or double-counted; only the split changes).
func TestJoinSessionsToCommits_PerMessageBranchAttribution(t *testing.T) {
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	sessions := []sessionSummary{
		{
			SessionID: "sess-mixed",
			// Session LATCHED on main (opened there before the feature checkout).
			GitBranch: "main",
			Model:     "claude-sonnet-4",
			StartTime: base,
			EndTime:   base.Add(10 * time.Minute),
			Messages: []messageUsage{
				// Two early messages on main (exploration/planning).
				{messageID: "m1", timestamp: base, gitBranch: "main", model: "claude-sonnet-4", input: 1000, output: 500},
				{messageID: "m2", timestamp: base.Add(1 * time.Minute), gitBranch: "main", model: "claude-sonnet-4", input: 800, output: 400},
				// Then the developer checks out feature/47-x and keeps working.
				{messageID: "m3", timestamp: base.Add(5 * time.Minute), gitBranch: "feature/47-x", model: "claude-sonnet-4", input: 2000, output: 900},
				{messageID: "m4", timestamp: base.Add(6 * time.Minute), gitBranch: "feature/47-x", model: "claude-sonnet-4", input: 1500, output: 700},
			},
		},
	}
	events := joinSessionsToCommits(sessions, nil, "dev", "")
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4 (one per message)", len(events))
	}

	byIssue := map[string]int64{}
	var total int64
	for _, e := range events {
		byIssue[e.IssueID] += e.CostMicro
		total += e.CostMicro
	}
	// The feature-branch messages attribute to issue-47 (the latch fix); the main
	// messages land in the exploratory bucket.
	if byIssue["issue-47"] <= 0 {
		t.Errorf("feature-branch messages did not attribute to issue-47: %v", byIssue)
	}
	if byIssue[UnattributedMain] <= 0 {
		t.Errorf("main-branch messages did not land in the exploratory bucket: %v", byIssue)
	}
	// Under the OLD latch, the whole session would have been one issue id; assert
	// the split is genuinely per-message (both partitions non-empty and disjoint).
	if len(byIssue) != 2 {
		t.Errorf("expected exactly 2 partitions (issue-47 + exploratory), got %v", byIssue)
	}
	// Denominator invariant: the session's total cost is unchanged by the split —
	// exploratory cost stays IN the denominator, nothing dropped or duplicated.
	var want int64
	for _, m := range sessions[0].Messages {
		want += store.ComputeCost(m.model, store.CostUsage{Input: m.input, Output: m.output})
	}
	if total != want {
		t.Errorf("total cost = %d, want %d (per-message split must preserve total spend)", total, want)
	}
}

// TestResolveMessageIssue_MainNotMisattributedToNearbyCommit is the unit-level
// mis-attribution bug-fix regression (#refocus): a bare main message within the
// ±30min window of a merge commit whose subject closes an issue must NOT inherit
// that issue. It belongs in the exploratory bucket — a false attribution is worse
// than an honest one.
func TestResolveMessageIssue_MainNotMisattributedToNearbyCommit(t *testing.T) {
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	commits := []gitCommit{{
		Hash:      "abc",
		Timestamp: ts.Add(5 * time.Minute), // inside the ±30min window
		Subject:   "fix: thing (closes #98)",
		Branches:  []string{"main"},
		IssueID:   "issue-98",
	}}
	got := resolveMessageIssue("main", ts, indexCommitsByLeaf(commits))
	if got == "issue-98" {
		t.Fatal("main message was MIS-attributed to a nearby merge commit's issue-98")
	}
	if got != UnattributedMain {
		t.Errorf("resolveMessageIssue(main) = %q, want %q", got, UnattributedMain)
	}

	// Control: a real feature branch matching the same commit still attributes —
	// the fix narrows ONLY the exploratory branches, not attribution in general.
	featureCommits := []gitCommit{{
		Hash: "def", Timestamp: ts.Add(5 * time.Minute),
		Branches: []string{"feature/foo"}, IssueID: "issue-98",
	}}
	if got := resolveMessageIssue("feature/foo", ts, indexCommitsByLeaf(featureCommits)); got != "issue-98" {
		t.Errorf("resolveMessageIssue(feature/foo) = %q, want issue-98 (commit join must still work)", got)
	}

	// Boundary: a commit exactly at the +joinWindow edge is inside the inclusive
	// window and still attributes; one just past it does not.
	edgeCommits := []gitCommit{{
		Hash: "edge", Timestamp: ts.Add(joinWindow),
		Branches: []string{"feature/bar"}, IssueID: "issue-7",
	}}
	if got := resolveMessageIssue("feature/bar", ts, indexCommitsByLeaf(edgeCommits)); got != "issue-7" {
		t.Errorf("commit at +joinWindow edge: got %q, want issue-7 (inclusive boundary)", got)
	}
	pastCommits := []gitCommit{{
		Hash: "past", Timestamp: ts.Add(joinWindow + time.Second),
		Branches: []string{"feature/bar"}, IssueID: "issue-7",
	}}
	if got := resolveMessageIssue("feature/bar", ts, indexCommitsByLeaf(pastCommits)); got != UnattributedNoIssue {
		t.Errorf("commit just past +joinWindow: got %q, want the no-issue bucket (outside window)", got)
	}

	// A detached HEAD and an empty branch route to the detached-head bucket even with
	// a matching-time commit present: they are exploratory, so commit-matching is
	// skipped entirely.
	for _, branch := range []string{"HEAD", ""} {
		if got := resolveMessageIssue(branch, ts, indexCommitsByLeaf(commits)); got != UnattributedDetachedHEAD {
			t.Errorf("resolveMessageIssue(%q) = %q, want %q", branch, got, UnattributedDetachedHEAD)
		}
	}
}

// TestJoinSessionsToCommits_EmptyMessageBranchFallsBackToSession pins the
// per-message fallback (#refocus, Option A): a message whose line carried NO branch
// inherits the session-latched branch, so a feature session with a branch-less
// message still attributes that message to the session's issue rather than mis-
// bucketing it.
func TestJoinSessionsToCommits_EmptyMessageBranchFallsBackToSession(t *testing.T) {
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	sessions := []sessionSummary{{
		SessionID: "sess-fallback",
		GitBranch: "feature/88-x", // latched session branch
		Model:     "claude-sonnet-4",
		StartTime: base,
		EndTime:   base.Add(time.Minute),
		Messages: []messageUsage{
			// This message's line recorded no branch of its own.
			{messageID: "m1", timestamp: base, gitBranch: "", model: "claude-sonnet-4", input: 1000, output: 500},
		},
	}}
	events := joinSessionsToCommits(sessions, nil, "dev", "")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].IssueID != "issue-88" {
		t.Errorf("branch-less message attributed to %q, want issue-88 (session-branch fallback)", events[0].IssueID)
	}
}

// TestBucketForBranch pins each branch shape to its labeled bucket (#refocus,
// Option B) and that every bucket is a member of the unattributed family.
func TestBucketForBranch(t *testing.T) {
	cases := []struct {
		branch string
		want   string
	}{
		{"main", UnattributedMain},
		{"master", UnattributedMain},
		{"HEAD", UnattributedDetachedHEAD},
		{"", UnattributedDetachedHEAD},
		{"scratch", UnattributedNoIssue},
		{"wip/refactor", UnattributedNoIssue},
	}
	for _, c := range cases {
		got := bucketForBranch(c.branch)
		if got != c.want {
			t.Errorf("bucketForBranch(%q) = %q, want %q", c.branch, got, c.want)
		}
		if !IsUnattributed(got) {
			t.Errorf("bucketForBranch(%q) = %q is not in the unattributed family", c.branch, got)
		}
	}
}

// TestParseSessionFile_DetachedHEADSessionKept is a #120 regression: a session
// with usage but no usable branch (detached HEAD records gitBranch "HEAD", or
// an empty gitBranch) used to be discarded at the parser (drop site 2), so its
// cost never reached the join. Spec section 4.4 requires such usage to survive
// parsing and flow to the "unattributed" fallback downstream.
func TestParseSessionFile_DetachedHEADSessionKept(t *testing.T) {
	dir := t.TempDir()

	// Detached HEAD: every line carries gitBranch "HEAD".
	headLines := []string{
		`{"type":"assistant","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-head","gitBranch":"HEAD","cwd":"/repo","message":{"id":"msg_head","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	headPath := writeJSONL(t, dir, "head.jsonl", headLines)
	hs, err := parseSessionFile(headPath)
	if err != nil {
		t.Fatalf("parseSessionFile (HEAD): %v", err)
	}
	if hs == nil {
		t.Fatal("detached-HEAD session with usage was dropped at the parser, want kept")
	}
	if len(hs.Messages) != 1 {
		t.Fatalf("HEAD session: got %d messages, want 1", len(hs.Messages))
	}

	// Companion: an empty gitBranch (no branch recorded at all) is likewise kept.
	emptyLines := []string{
		`{"type":"assistant","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-nobranch","gitBranch":"","cwd":"/repo","message":{"id":"msg_nb","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	emptyPath := writeJSONL(t, dir, "nobranch.jsonl", emptyLines)
	es, err := parseSessionFile(emptyPath)
	if err != nil {
		t.Fatalf("parseSessionFile (empty branch): %v", err)
	}
	if es == nil {
		t.Fatal("empty-branch session with usage was dropped at the parser, want kept")
	}
	if len(es.Messages) != 1 {
		t.Fatalf("empty-branch session: got %d messages, want 1", len(es.Messages))
	}
}

// TestParseSessionFile_PerMessageBranchThreaded proves the #refocus Option A wiring
// end to end through the REAL parser (not a hand-built messageUsage): a single
// session file whose assistant lines carry DIFFERENT gitBranch values must produce
// per-message gitBranch, and joinSessionsToCommits must then attribute each message
// by its own branch — main lines to the exploratory bucket, feature lines to the
// issue. The session is latched on main (its first branch), which is exactly the
// case the old one-branch-per-session latch mis-routed wholesale.
func TestParseSessionFile_PerMessageBranchThreaded(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"type":"assistant","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-mix","gitBranch":"main","cwd":"/repo","message":{"id":"m1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500}}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:05:00Z","sessionId":"sess-mix","gitBranch":"feature/47-x","cwd":"/repo","message":{"id":"m2","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":2000,"output_tokens":800}}}`,
	}
	path := writeJSONL(t, dir, "mix.jsonl", lines)
	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s == nil || len(s.Messages) != 2 {
		t.Fatalf("got %v messages, want 2", s)
	}
	// The parser must have captured each line's own branch (the wiring under test).
	branchByID := map[string]string{}
	for _, m := range s.Messages {
		branchByID[m.messageID] = m.gitBranch
	}
	if branchByID["m1"] != "main" || branchByID["m2"] != "feature/47-x" {
		t.Fatalf("per-message gitBranch not threaded through parse: %v", branchByID)
	}
	// And the join attributes per message: m1 (main) -> exploratory bucket, m2 -> issue-47.
	events := joinSessionsToCommits([]sessionSummary{*s}, nil, "dev", "")
	got := map[string]string{}
	for _, e := range events {
		// key events by their token count to tell m1 (1500) from m2 (2800) apart.
		if e.InputTok == 1000 {
			got["m1"] = e.IssueID
		} else {
			got["m2"] = e.IssueID
		}
	}
	if got["m1"] != UnattributedMain {
		t.Errorf("m1 (main) attributed to %q, want %q", got["m1"], UnattributedMain)
	}
	if got["m2"] != "issue-47" {
		t.Errorf("m2 (feature/47-x) attributed to %q, want issue-47", got["m2"])
	}
}

// TestParseSessionFile_NoMessagesStillDropped is the control for the relaxed
// #120 discard condition: a session with a branch but zero assistant usage
// (len(messages) == 0) still yields a nil summary — an empty session carries no
// cost to route.
func TestParseSessionFile_NoMessagesStillDropped(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"type":"user","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess-empty","gitBranch":"feature/9-foo","cwd":"/repo","message":{"role":"user","content":"hi"}}`,
	}
	path := writeJSONL(t, dir, "empty.jsonl", lines)
	s, err := parseSessionFile(path)
	if err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if s != nil {
		t.Errorf("want nil summary for a session with no assistant usage, got %+v", s)
	}
}

// TestFilterStillScopesUnattributed pins that the #120 fallback never captures
// foreign-repo cost: sessions whose CWD is outside the target repo (or empty)
// are dropped by filterSessionsByRepo BEFORE the join can reach the
// unattributed fallback.
func TestFilterStillScopesUnattributed(t *testing.T) {
	repo := t.TempDir()
	foreign := t.TempDir()

	sessions := []sessionSummary{
		{SessionID: "foreign-unattr", CWD: foreign, GitBranch: ""},
		{SessionID: "empty-unattr", CWD: "", GitBranch: ""},
	}
	kept := filterSessionsByRepo(sessions, repo)
	if len(kept) != 0 {
		t.Errorf("filterSessionsByRepo kept %d sessions, want 0 (foreign/empty unattributed cost must not leak into the repo)", len(kept))
	}
}

// TestOsUsername_FallsBackToUserCurrent pins the #125 hardening: when both
// $USER and $LOGNAME are unset (systemd units, cron, bare containers),
// osUsername must fall back to os/user.Current() rather than returning the
// literal "unknown" sentinel, which would silently pool multiple machines'
// spend under one fake identity. On any dev/CI machine user.Current() resolves,
// so the result must be a real, non-empty, non-"unknown" login.
func TestOsUsername_FallsBackToUserCurrent(t *testing.T) {
	// user.Current() can legitimately error under CGO_ENABLED=0 in a minimal
	// container whose UID has no /etc/passwd entry; there "unknown" is the
	// correct (documented) result, so only assert the fallback when it can
	// actually resolve. On dev/CI machines it resolves and the assertion runs.
	if _, err := user.Current(); err != nil {
		t.Skipf("user.Current() unavailable in this environment (%v); fallback not exercisable", err)
	}
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")
	got := osUsername()
	if got == "" {
		t.Fatalf("osUsername() = %q; want a non-empty login from user.Current()", got)
	}
	if got == "unknown" {
		t.Fatalf("osUsername() = %q; want the user.Current() fallback, not the sentinel", got)
	}
}
