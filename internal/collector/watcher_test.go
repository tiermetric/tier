package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// recordingStore captures every Ingest call and satisfies the watcher's two
// store seams at once: collector.Ingester (the shared event sink, #46) and
// CheckpointStore (#71 tail-state persistence). Used in lieu of the real SQLite
// store so the tests don't depend on a working modernc.org/sqlite build path and
// don't pay an Open() per test.
//
// Events are captured as collector.TokenEvent — the exact type the watcher now
// forwards through the Ingester — so assertions see every field the collector
// produced, including SessionID, which the pre-#46 hand-rolled store write
// silently dropped.
type recordingStore struct {
	mu          sync.Mutex
	events      []TokenEvent
	checkpoints map[string]store.WatcherCheckpoint
	// seedCheckpoints is returned by LoadWatcherCheckpoints to simulate a
	// restart resuming from persisted state; nil means "empty store".
	seedCheckpoints []store.WatcherCheckpoint
	// deletedPaths records every DeleteWatcherCheckpoint call so a test can
	// assert pruning (orphan/malformed rows) happened.
	deletedPaths []string
}

func (r *recordingStore) Ingest(_ context.Context, e TokenEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingStore) LoadWatcherCheckpoints(_ context.Context) ([]store.WatcherCheckpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seedCheckpoints, nil
}

func (r *recordingStore) SaveWatcherCheckpoint(_ context.Context, cp store.WatcherCheckpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkpoints == nil {
		r.checkpoints = map[string]store.WatcherCheckpoint{}
	}
	r.checkpoints[cp.Path] = cp
	return nil
}

func (r *recordingStore) DeleteWatcherCheckpoint(_ context.Context, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.checkpoints, path)
	r.deletedPaths = append(r.deletedPaths, path)
	return nil
}

func (r *recordingStore) deletedSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deletedPaths...)
}

func (r *recordingStore) snapshot() []TokenEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TokenEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingStore) checkpointSnapshot() map[string]store.WatcherCheckpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]store.WatcherCheckpoint, len(r.checkpoints))
	for k, v := range r.checkpoints {
		out[k] = v
	}
	return out
}

// waitForEvents polls until the recorder has at least n events or the
// deadline expires. fsnotify timing on macOS is loose enough that hard sleeps
// flake; polling with a generous deadline keeps tests deterministic without
// being slow in the happy path.
func waitForEvents(t *testing.T, r *recordingStore, n int, deadline time.Duration) []TokenEvent {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if snap := r.snapshot(); len(snap) >= n {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r.snapshot()
}

// writeJSONLLine writes a single JSONL line as a complete session file. The
// session passes parseSessionFile (has assistant + usage block), CWD is set
// to the supplied repo path, and the branch matches a feature/<issue> form.
func writeJSONLLine(t *testing.T, path string, sessionID, repo, branch string, totalTokens int) {
	t.Helper()
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":%q,"gitBranch":%q,"cwd":%q,"message":{"id":"msg_%s","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		sessionID, branch, repo, sessionID, totalTokens,
	)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendJSONLLine(t *testing.T, path string, sessionID, repo, branch string, totalTokens int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:01Z","sessionId":%q,"gitBranch":%q,"cwd":%q,"message":{"id":"msg2_%s","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		sessionID, branch, repo, sessionID, totalTokens,
	)
	if _, err := io.WriteString(f, line); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// startWatcher boots a watcher in a goroutine. t.Cleanup cancels it and
// waits for clean shutdown — callers don't need the cancel directly. The
// debounce is set short (50ms) so tests run fast; the production default is
// 1 second.
func startWatcher(t *testing.T, claudeDir string, repos []string, rec *recordingStore) *Watcher {
	t.Helper()
	return bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         repos,
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
	})
}

// bootWatcher runs an already-constructed Watcher in a goroutine and registers
// cleanup that cancels it and waits for clean shutdown. Tests that need to set
// fields startWatcher doesn't expose (e.g. gitLogFn for #99 attribution)
// build the Watcher themselves and boot it here.
func bootWatcher(t *testing.T, w *Watcher) *Watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("watcher did not exit within 2s of ctx cancel")
		}
	})
	// Give Run() a moment to register fsnotify watches before tests start
	// writing — racing the watcher startup misses events on Linux. 150ms is
	// loose enough for busy CI runners; 50ms flaked under load.
	time.Sleep(150 * time.Millisecond)
	return w
}

// TestWatcher_NewFileIngested covers the happy path: a JSONL file written
// into an existing project subdir is parsed and inserted as one TokenEvent.
func TestWatcher_NewFileIngested(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-1", repo, "feature/42-foo", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].IssueID != "issue-42" {
		t.Errorf("issue_id = %q, want issue-42", events[0].IssueID)
	}
	if events[0].InputTok != 100 {
		t.Errorf("input_tok = %d, want 100", events[0].InputTok)
	}
}

// TestWatcher_ForwardsSessionIDThroughIngester locks the one intentional
// behavior change of #46: routing the live watcher through the shared
// collector.Ingester (instead of the old hand-rolled store.TokenEvent literal)
// makes it persist SessionID.
//
// Before #46 the watcher's manual insert copied every field EXCEPT SessionID —
// #281 threaded session_id through the ingester adapter for the JSONL `score`
// path but never onto this live-watcher bypass, so the primary capture path
// silently dropped a session id it had already parsed. Now that the watcher
// funnels through the same adapter, the field survives. This asserts the fix and
// guards against a regression that would re-open the #27 asymmetry.
func TestWatcher_ForwardsSessionIDThroughIngester(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-xyz", repo, "feature/42-foo", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].SessionID != "sess-xyz" {
		t.Errorf("SessionID = %q, want sess-xyz — the live watcher must persist session_id through the shared Ingester (#46)", events[0].SessionID)
	}
}

// TestWatcher_AppendEmitsOnlyNewMessages verifies the #30 incremental
// tailing contract: when a session file is appended to, only the new
// bytes are re-parsed and only the new message is emitted. The original
// message is NOT re-emitted (the previous full-re-parse behavior wasted
// work and relied on store-layer dedup to absorb the duplicate; the
// new behavior avoids the duplicate at the source).
//
// The recorder records every Ingest call. With incremental tailing, the
// total event count equals the total message count, with each message
// emitted exactly once across all debounce cycles.
func TestWatcher_AppendEmitsOnlyNewMessages(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")
	writeJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 100)
	_ = waitForEvents(t, rec, 1, 2*time.Second)

	// Append a second assistant turn. writeJSONLLine encodes message.id as
	// "msg_<sessionID>"; appendJSONLLine uses "msg2_<sessionID>", so the
	// two messages have distinct upstream ids.
	appendJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 250)

	// After the second debounce we should see exactly 2 events total: one
	// per distinct message.id, each emitted exactly once. The original
	// message is NOT re-emitted because the incremental parser advanced
	// past it on the first debounce.
	events := waitForEvents(t, rec, 2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (no double-emission under incremental tailing)", len(events))
	}

	// Two distinct keys — one per message.id. The recording store doesn't
	// enforce uniqueness; this assertion catches a regression where the
	// incremental path accidentally re-emits the prior message.
	keys := map[string]struct{}{}
	for _, e := range events {
		if e.IdempotencyKey == "" {
			t.Errorf("event has empty IdempotencyKey: %+v", e)
		}
		keys[e.IdempotencyKey] = struct{}{}
	}
	if len(keys) != 2 {
		t.Errorf("got %d distinct keys, want 2 (one per message.id); keys=%v", len(keys), keys)
	}
}

// waitForCheckpoint polls until a checkpoint for path is persisted. The save
// fires just AFTER the InsertTokenEvent that waitForEvents observes, so tests
// must wait for the checkpoint separately rather than assume it's already there.
func waitForCheckpoint(t *testing.T, r *recordingStore, path string, deadline time.Duration) store.WatcherCheckpoint {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cp, ok := r.checkpointSnapshot()[path]; ok {
			return cp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("checkpoint for %s not persisted within %s", path, deadline)
	return store.WatcherCheckpoint{}
}

func waitForCheckpointGone(t *testing.T, r *recordingStore, path string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if _, ok := r.checkpointSnapshot()[path]; !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("checkpoint for %s still present after %s", path, deadline)
}

// TestWatcher_PersistsAndDeletesCheckpoint covers the #71 save/delete wiring: a
// successful parse persists a checkpoint carrying the offset, rotation guard,
// and session metadata; removing the file drops the persisted row.
func TestWatcher_PersistsAndDeletesCheckpoint(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")
	writeJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 100)
	waitForEvents(t, rec, 1, 2*time.Second)

	cp := waitForCheckpoint(t, rec, path, 2*time.Second)
	if cp.Offset <= 0 {
		t.Errorf("checkpoint offset = %d, want > 0", cp.Offset)
	}
	if cp.Inode == 0 || cp.HeadLen == 0 {
		t.Errorf("checkpoint missing rotation guard: inode=%d headLen=%d", cp.Inode, cp.HeadLen)
	}
	// The metadata blob MUST carry the session id (and with it NextParseSeq);
	// without it a resume can't keep id-less message dedup keys stable.
	if !strings.Contains(cp.Metadata, `"SessionID":"sess-1"`) {
		t.Errorf("checkpoint metadata missing session id: %s", cp.Metadata)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitForCheckpointGone(t, rec, path, 2*time.Second)
}

// TestWatcher_ResumesFromPersistedCheckpoint is the #71 acceptance test: a fresh
// watcher seeded with a persisted checkpoint resumes a file from the saved
// offset instead of re-parsing from byte 0.
func TestWatcher_ResumesFromPersistedCheckpoint(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projectsDir, "s1.jsonl")

	// Phase 1: a watcher ingests message 1 and persists a checkpoint, then is
	// stopped (simulating a process exit). Built manually so it can be cancelled
	// before phase 2 — two live watchers on one file would both react to the
	// append and confound the count.
	rec1 := &recordingStore{}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	w1 := &Watcher{ClaudeDir: claudeDir, Repos: []string{repo}, Ingester: rec1, Checkpoints: rec1, DeveloperID: "alice", DebounceDelay: 50 * time.Millisecond}
	go func() { _ = w1.Run(ctx1); close(done1) }()
	time.Sleep(150 * time.Millisecond)

	writeJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 100)
	waitForEvents(t, rec1, 1, 2*time.Second)
	cp := waitForCheckpoint(t, rec1, path, 2*time.Second)

	cancel1()
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("phase-1 watcher did not stop within 2s")
	}

	// Phase 2: a fresh watcher seeded with the persisted checkpoint resumes the
	// SAME file. Appending message 2 must yield exactly ONE new event — only the
	// appended bytes get parsed. A byte-0 re-parse (no resume) would re-emit
	// message 1 too, so a settled count of 1 is the resume proof.
	rec2 := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{cp}}
	startWatcher(t, claudeDir, []string{repo}, rec2)

	appendJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 250)
	waitForEvents(t, rec2, 1, 2*time.Second)
	// Settle: a byte-0 re-parse would insert message 1 in the same debounce, so
	// give any erroneous extra event time to land before asserting the count.
	time.Sleep(300 * time.Millisecond)
	if got := rec2.snapshot(); len(got) != 1 {
		t.Fatalf("got %d events after resume, want exactly 1 (a byte-0 re-parse would re-emit message 1)", len(got))
	}
}

// waitForDeletedCheckpoint polls until path appears in the store's delete log.
func waitForDeletedCheckpoint(t *testing.T, r *recordingStore, path string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		for _, p := range r.deletedSnapshot() {
			if p == path {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("checkpoint for %s was not pruned within %s", path, deadline)
}

// TestWatcher_MalformedCheckpointSkippedAtStartup covers the load-path
// resilience (#71): a checkpoint whose metadata blob won't deserialize must not
// break startup — the row is pruned, the file is parsed from byte 0, and the
// watcher self-heals by writing a fresh valid checkpoint. This path runs on
// every restart, so a regression here would brick the watcher.
func TestWatcher_MalformedCheckpointSkippedAtStartup(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projectsDir, "s1.jsonl")
	// File exists on disk, but its persisted checkpoint has corrupt metadata.
	writeJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 100)
	bad := store.WatcherCheckpoint{Path: path, Inode: 999, Offset: 50, HeadCRC: 1, HeadLen: 10, Metadata: "{not valid json"}
	rec := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{bad}}
	startWatcher(t, claudeDir, []string{repo}, rec)

	// The corrupt row is pruned at startup, then a write drives a clean parse.
	waitForDeletedCheckpoint(t, rec, path, 2*time.Second)
	appendJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 250)
	if events := waitForEvents(t, rec, 1, 2*time.Second); len(events) == 0 {
		t.Fatal("watcher did not ingest after a malformed seed checkpoint")
	}
	cp := waitForCheckpoint(t, rec, path, 2*time.Second)
	if !strings.Contains(cp.Metadata, `"SessionID":"sess-1"`) {
		t.Errorf("post-recovery checkpoint metadata not valid: %s", cp.Metadata)
	}
}

// TestWatcher_PrunesOrphanedCheckpointAtStartup covers the unbounded-growth
// guard (#71): a checkpoint whose file was deleted while the watcher was DOWN
// (so the removal-time delete never fired) is pruned at startup rather than
// seeded, keeping the table bounded to live files.
func TestWatcher_PrunesOrphanedCheckpointAtStartup(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Checkpoint for a path with no file on disk (a deleted session).
	gonePath := filepath.Join(projectsDir, "gone.jsonl")
	orphan := store.WatcherCheckpoint{
		Path: gonePath, Inode: 42, Offset: 100, HeadCRC: 7, HeadLen: 20,
		Metadata: `{"SessionID":"sess-gone","NextParseSeq":3}`,
	}
	rec := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{orphan}}
	startWatcher(t, claudeDir, []string{repo}, rec)

	waitForDeletedCheckpoint(t, rec, gonePath, 2*time.Second)
}

// TestWatcher_TruncationTriggersFullReparse covers the #30 fallback path:
// if a file shrinks below the cached offset (truncate-and-rewrite, log
// rotation hand-back, manual edit), the cached state is stale. The
// watcher must full-re-parse from byte 0 so the new content gets
// ingested correctly.
//
// Setup: write a session, let it ingest. Then truncate the file and
// write entirely new content. The new content's message should appear
// in the recorder.
func TestWatcher_TruncationTriggersFullReparse(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")
	// Write a multi-line original session so the cached offset is large
	// enough that any single-line replacement after truncate is strictly
	// smaller — that's what trips the size-shrink check. (The
	// truncate-and-rewrite-to-similar-or-larger-size corner case is a
	// known limitation; see follow-up.)
	originalLine := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-A","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_orig_%%d","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	var original strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&original, originalLine+"\n", i)
	}
	if err := os.WriteFile(path, []byte(original.String()), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	_ = waitForEvents(t, rec, 5, 2*time.Second)

	// Truncate-and-rewrite: same inode, file shrinks from 5 lines to
	// 1 line. The size-shrink check makes the watcher discard cached
	// state and full-re-parse from byte 0.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	writeJSONLLine(t, path, "sess-B", repo, "feature/2-bar", 999)

	// All originals plus the post-truncate message should be in the
	// recorder: 5 from the original session + 1 from after truncation.
	events := waitForEvents(t, rec, 6, 2*time.Second)
	if len(events) < 6 {
		t.Fatalf("got %d events, want >= 6 (truncate-and-rewrite must full-re-parse)", len(events))
	}
	var sawBar bool
	for _, e := range events {
		if e.IssueID == "issue-2" {
			sawBar = true
		}
	}
	if !sawBar {
		t.Errorf("post-truncate message (issue-2) missing from recorder: %+v", events)
	}
}

// TestWatcher_InodeChangeTriggersFullReparse covers the rotation case:
// the path stays the same but a different file is behind it (logrotate-
// style: mv old; create new). os.Stat sees a different inode → watcher
// discards cached state and full-re-parses the new file.
//
// On Unix the file's inode comes from syscall.Stat_t.Ino. On platforms
// where inodeOf returns 0 (Windows), this test would still pass via the
// "size shrunk" branch since the new file is smaller — but the supported
// deployment target is Unix, so the inode path is the production case.
func TestWatcher_InodeChangeTriggersFullReparse(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")
	// Pad the original with extra content so the replacement file is
	// strictly larger — that way the inode-change path is the only
	// reason for a full re-parse, not the size-shrink path.
	original := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-A","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_orig","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	if err := os.WriteFile(path, []byte(original+"\n"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	_ = waitForEvents(t, rec, 1, 2*time.Second)

	// Replace via remove+create — different inode, larger content. Doing
	// it that way (rather than rename-in-place) ensures the inode
	// definitely changes on macOS/Linux APFS/ext4.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	replacement := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T11:00:00Z","sessionId":"sess-B","gitBranch":"feature/2-bar","cwd":%q,"message":{"id":"msg_new_after_rotation","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":500,"output_tokens":250,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	// Two lines so the new file is strictly larger than the original.
	if err := os.WriteFile(path, []byte(replacement+"\n"+replacement+"\n"), 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	events := waitForEvents(t, rec, 2, 2*time.Second)
	if len(events) < 2 {
		t.Fatalf("got %d events, want >= 2 (inode change must full-re-parse)", len(events))
	}
	var sawNew bool
	for _, e := range events {
		if e.IssueID == "issue-2" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Errorf("post-rotation message missing from recorder: %+v", events)
	}
}

// TestWatcher_TruncateAndRewriteToSameSizeFullReparses covers the
// head-fingerprint guard: same inode, same size, but the file head's
// CRC32 differs because the bytes underneath the cached offset were
// rewritten in place. The watcher must detect the head mismatch and
// full-re-parse from byte 0 — otherwise the rewritten content's tokens
// would be silently attributed to the OLD session's metadata.
//
// Setup: write session-A, let it ingest. Truncate-and-rewrite to a
// session-B line that's the same byte length. Without the head guard,
// the size check would pass (size >= prior.offset) and the watcher
// would skip directly to incremental parse with stale knownMeta.
func TestWatcher_TruncateAndRewriteToSameSizeFullReparses(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")
	// Two lines with identical structure so the rewritten file is the
	// same byte length as the original. Only the sessionId, branch, and
	// message id strings differ — chosen to be the same character length
	// so totals match exactly.
	original := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-A","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_origA","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	if err := os.WriteFile(path, []byte(original+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = waitForEvents(t, rec, 1, 2*time.Second)

	replacement := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T11:00:00Z","sessionId":"sess-B","gitBranch":"feature/2-bar","cwd":%q,"message":{"id":"msg_origB","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":500,"output_tokens":250,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	// Replacement size must be >= original size (otherwise the size-shrink
	// check would force a full reparse and we wouldn't be exercising the
	// head-fingerprint path). Off-by-a-few bytes is fine.
	if len(replacement) < len(original) {
		t.Fatalf("test setup invariant broken: replacement=%d bytes < original=%d — would trip size-shrink path instead",
			len(replacement), len(original))
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := os.WriteFile(path, []byte(replacement+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	events := waitForEvents(t, rec, 2, 2*time.Second)
	if len(events) < 2 {
		t.Fatalf("got %d events, want >= 2 (head-fingerprint mismatch must force full re-parse)", len(events))
	}
	var sawB bool
	for _, e := range events {
		if e.IssueID == "issue-2" {
			sawB = true
		}
	}
	if !sawB {
		t.Errorf("rewritten content (issue-2) missing — head guard didn't trigger full reparse; events=%+v", events)
	}
}

// TestWatcher_PartialLineNotConsumed exercises a writer-mid-flush case:
// the file ends with bytes that are NOT terminated by a newline. The
// parser must not advance the offset past the partial line — the bytes
// must be re-read once the writer flushes the terminating newline.
//
// Replicates the fsnotify "WRITE event fires before the line is
// complete" scenario that streaming JSONL emitters hit at 30-50 writes
// per second.
func TestWatcher_PartialLineNotConsumed(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")

	// Write the first message complete, then start a second line WITHOUT
	// its trailing newline.
	complete := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:00Z","sessionId":"sess-P","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_complete","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		repo,
	)
	partial := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:01:00Z","sessionId":"sess-P","gitBranch":"feature/1-foo","cwd":%q,"message":{"id":"msg_partial","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":200,"output_tokens"`,
		repo,
	)
	if err := os.WriteFile(path, []byte(complete+"\n"+partial), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First debounce — emits only the complete message; the partial line
	// is buffered in the file but not yet a parsable record.
	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("after partial write: got %d events, want 1 (partial line must not be consumed)", len(events))
	}

	// Now flush the rest of the partial line — should pick up the second
	// message via incremental tailing.
	rest := `:80,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(rest + "\n"); err != nil {
		t.Fatalf("append rest: %v", err)
	}
	_ = f.Close()

	events = waitForEvents(t, rec, 2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("after flush: got %d events, want 2 (the partial line, now complete, should be ingested)", len(events))
	}
}

// TestWatcher_ForeignRepoDropped is the live counterpart of #15: a session
// recorded in a CWD that is not one of the configured --watch-repo paths
// must not produce a TokenEvent. Mirrors filterSessionsByRepo's semantics so
// the live and batch paths agree.
func TestWatcher_ForeignRepoDropped(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	foreignRepo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	// Foreign session — must be dropped silently.
	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"),
		"sess-foreign", foreignRepo, "feature/947-not-tier", 9_999_999)
	// In-repo session — must be ingested.
	writeJSONLLine(t, filepath.Join(projectsDir, "s2.jsonl"),
		"sess-in", repo, "feature/42-foo", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (foreign session must be excluded)", len(events))
	}
	if events[0].IssueID == "issue-947" {
		t.Errorf("foreign session leaked: %+v", events[0])
	}
}

// fakeGitLog builds a gitLogFn that returns the supplied commits and counts
// its invocations, so tests can assert both attribution and that the per-repo
// commit cache (#99) does not re-shell git log on every event.
func fakeGitLog(calls *atomic.Int32, commits []gitCommit, err error) gitLogFunc {
	return func(_ context.Context, _ string, _ time.Time) ([]gitCommit, error) {
		calls.Add(1)
		return commits, err
	}
}

// TestWatcher_MainBranchAttributedViaCommit is the core #99 regression: a
// session on `main` (no issue number in the branch) used to be dropped at $0
// by the live watcher because it joined against a nil commit slice. With the
// per-repo commit cache it now attributes via the commit decorate/IssueID,
// matching the `tierd score` path.
// TestWatcher_MainBranchNotMisattributedToNearbyMergeCommit is the mis-attribution
// bug-fix regression (#refocus): a main-branch session that happens to fall within
// the ±30min join window of a merge commit whose subject says "closes #98" must NOT
// inherit issue-98. That cost is real exploratory/planning work on main, not work on
// #98 — a false attribution is worse than an honest bucket. It must land in the
// labeled exploratory bucket (unattributed:main) instead. The git log is still read
// (the commit cache is populated for the feature-branch sessions that DO join), so
// the assertion is about the routing decision, not whether git ran.
func TestWatcher_MainBranchNotMisattributedToNearbyMergeCommit(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	// The session's assistant line is stamped 2026-05-19T10:00:00Z (see
	// writeJSONLLine); the commit lands inside [start-30m, end+30m], so under the
	// OLD behavior the main session would have wrongly inherited issue-98.
	commitTime, err := time.Parse(time.RFC3339, "2026-05-19T10:00:00Z")
	if err != nil {
		t.Fatalf("parse commit time: %v", err)
	}
	var gitCalls atomic.Int32
	commit := gitCommit{
		Hash:      "deadbeef",
		Timestamp: commitTime,
		Subject:   "fix: thing (closes #98) (#98)",
		Branches:  []string{"main"},
		IssueID:   "issue-98",
	}

	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      fakeGitLog(&gitCalls, []gitCommit{commit}, nil),
	})

	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-main", repo, "main", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].IssueID == "issue-98" {
		t.Errorf("issue_id = %q: main-branch exploratory cost was MIS-attributed to a nearby merge commit's issue", events[0].IssueID)
	}
	if events[0].IssueID != UnattributedMain {
		t.Errorf("issue_id = %q, want %q (main is exploratory, not #98)", events[0].IssueID, UnattributedMain)
	}
}

// TestWatcher_MainBranchRoutedToUnattributedWhenNoCommitMatches is the #120
// inversion of the pre-existing #99 drop test: a main-branch session with no
// commit in the window is no longer dropped at $0. Its cost now lands on the
// developer under issue_id "unattributed" (spec section 4.4), while an
// attributable feature-branch session in the same repo still resolves to its
// issue. This is the live-path proof that the previously-vanished spend now
// enters the denominator.
func TestWatcher_MainBranchRoutedToUnattributedWhenNoCommitMatches(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	var gitCalls atomic.Int32
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      fakeGitLog(&gitCalls, nil, nil), // no commits
	})

	// Both sessions now emit: the main session routes to "unattributed", the
	// feature-branch session attributes via its branch name.
	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-main", repo, "main", 100)
	writeJSONLLine(t, filepath.Join(projectsDir, "s2.jsonl"), "sess-sentinel", repo, "feature/42-foo", 200)

	events := waitForEvents(t, rec, 2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 — the main session routes to unattributed and the feature session to issue-42", len(events))
	}
	byIssue := map[string]TokenEvent{}
	for _, e := range events {
		byIssue[e.IssueID] = e
	}
	unattr, ok := byIssue[UnattributedMain]
	if !ok {
		t.Errorf("main-branch session was not routed to the labeled exploratory bucket: %+v", events)
	} else if unattr.CostMicro <= 0 {
		// The old bug dropped this session "at $0"; a non-zero cost proves the
		// previously-vanished spend now enters the denominator.
		t.Errorf("unattributed event CostMicro = %d, want > 0 (spend must not vanish)", unattr.CostMicro)
	}
	if _, ok := byIssue["issue-42"]; !ok {
		t.Errorf("feature-branch session was not attributed to issue-42: %+v", events)
	}
}

// TestWatcher_UnattributedSessionIngested is the focused #120 live-path
// regression: a session whose cwd is inside the watched repo, on branch main,
// with no matching commit, must reach the store as an event with issue_id
// "unattributed" (spec section 4.4). On main this session was dropped at $0.
func TestWatcher_UnattributedSessionIngested(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	var gitCalls atomic.Int32
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      fakeGitLog(&gitCalls, nil, nil), // no commits
	})

	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-unattr", repo, "main", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (unattributable session must still be ingested)", len(events))
	}
	if !IsUnattributed(events[0].IssueID) {
		t.Errorf("issue_id = %q, want an unattributed-family id", events[0].IssueID)
	}
	if events[0].IssueID != UnattributedMain {
		t.Errorf("issue_id = %q, want %q (main is exploratory)", events[0].IssueID, UnattributedMain)
	}
	if events[0].Developer != "alice" {
		t.Errorf("developer = %q, want alice (cost lands on the session developer)", events[0].Developer)
	}
	if events[0].CostMicro <= 0 {
		// The drop this fix reverses lost the session "at $0" — assert the cost
		// survives at the live path where the drop used to bite (#120).
		t.Errorf("CostMicro = %d, want > 0 (previously-vanished spend must reach the store)", events[0].CostMicro)
	}
}

// TestWatcher_GitLogErrorFallsBackToBranchName proves the degradation path: a
// git log failure must not drop ingestion. A feature-branch session still
// attributes via the branch name (the pre-#99 behaviour) when the commit
// cache returns nil on error.
func TestWatcher_GitLogErrorFallsBackToBranchName(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	var gitCalls atomic.Int32
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      fakeGitLog(&gitCalls, nil, errors.New("git boom")),
	})

	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-feat", repo, "feature/42-foo", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (branch-name fallback must survive git log error)", len(events))
	}
	if events[0].IssueID != "issue-42" {
		t.Errorf("issue_id = %q, want issue-42", events[0].IssueID)
	}
}

// TestWatcher_MultiRepoAttributionRouting proves the commit cache is keyed by
// the matched repo: a main session in repo A must attribute via repo A's
// commits and a main session in repo B via repo B's, never crossed. A
// wrong-repo-key wiring bug at the cache.get(target.abs) site would cross or
// drop these; the per-repo isolation is only unit-tested in
// TestCommitCache_PerRepoKeying, so this exercises it through real process().
func TestWatcher_MultiRepoAttributionRouting(t *testing.T) {
	claudeDir := t.TempDir()
	repoA := t.TempDir()
	repoB := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	commitTime, err := time.Parse(time.RFC3339, "2026-05-19T10:00:00Z")
	if err != nil {
		t.Fatalf("parse commit time: %v", err)
	}
	// Use a non-exploratory, issueless feature branch as the vehicle: attribution
	// then comes ONLY from the commit join (issueref.FromBranch("feature/routing")
	// is ""), exercising the per-repo commit cache. A mainline branch would route
	// to the exploratory bucket regardless (#refocus bug fix), which is not what
	// this multi-repo-routing test is about.
	mainCommit := func(issue string) []gitCommit {
		return []gitCommit{{Hash: "h", Timestamp: commitTime, Branches: []string{"feature/routing"}, IssueID: issue}}
	}
	perRepoGitLog := func(_ context.Context, repoPath string, _ time.Time) ([]gitCommit, error) {
		switch filepath.Clean(repoPath) {
		case filepath.Clean(repoA):
			return mainCommit("issue-101"), nil
		case filepath.Clean(repoB):
			return mainCommit("issue-202"), nil
		}
		return nil, nil
	}

	rec := &recordingStore{}
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repoA, repoB},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      perRepoGitLog,
	})

	writeJSONLLine(t, filepath.Join(projectsDir, "a.jsonl"), "sess-a", repoA, "feature/routing", 100)
	writeJSONLLine(t, filepath.Join(projectsDir, "b.jsonl"), "sess-b", repoB, "feature/routing", 200)

	events := waitForEvents(t, rec, 2, 3*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (one per repo)", len(events))
	}
	byIssue := map[string]TokenEvent{}
	for _, e := range events {
		byIssue[e.IssueID] = e
	}
	if _, ok := byIssue["issue-101"]; !ok {
		t.Errorf("repo-A session not attributed to issue-101 via its commit: %+v", events)
	}
	if _, ok := byIssue["issue-202"]; !ok {
		t.Errorf("repo-B session not attributed to issue-202 via its commit: %+v", events)
	}
}

// TestWatcher_SameRepoReusesCachedCommits is the headline #99 performance
// property end-to-end: two sessions in the same repo within the TTL window
// must share one git log read, not shell out per event/session under the
// streaming-write fan-out. Proven at the unit level in
// TestCommitCache_CachesWithinTTL; this confirms it through the live debounce
// + process loop where the cache mutex meets real concurrency.
func TestWatcher_SameRepoReusesCachedCommits(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	commitTime, err := time.Parse(time.RFC3339, "2026-05-19T10:00:00Z")
	if err != nil {
		t.Fatalf("parse commit time: %v", err)
	}
	var gitCalls atomic.Int32
	// Issueless feature branch so attribution comes ONLY from the shared commit
	// cache (a mainline branch would bucket as exploratory — #refocus bug fix — and
	// not exercise the cache-reuse property this test pins).
	commit := gitCommit{Hash: "h", Timestamp: commitTime, Branches: []string{"feature/cache"}, IssueID: "issue-77"}

	rec := &recordingStore{}
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      fakeGitLog(&gitCalls, []gitCommit{commit}, nil),
	})

	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-1", repo, "feature/cache", 100)
	writeJSONLLine(t, filepath.Join(projectsDir, "s2.jsonl"), "sess-2", repo, "feature/cache", 200)

	events := waitForEvents(t, rec, 2, 3*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (both sessions attribute)", len(events))
	}
	for _, e := range events {
		if e.IssueID != "issue-77" {
			t.Errorf("issue_id = %q, want issue-77", e.IssueID)
		}
	}
	// TTL is 30s; both sessions land well inside it, so the second must hit
	// the cache. >1 means we re-shelled git log when we should not have.
	if got := gitCalls.Load(); got != 1 {
		t.Errorf("git log calls = %d, want 1 (second same-repo session must reuse the cache)", got)
	}
}

// TestWatcher_IncrementalAppendAttributesViaCommit covers the #99 × #30
// intersection: an issueless feature-branch session attributed via commit, then
// appended to across a debounce boundary. The incremental resume path (#30)
// rebuilds the session from cached metadata + the new chunk and must still join
// against the cached commit so the appended turn attributes too — and reuse the
// commit cache rather than re-fetching. (Formerly exercised via `main`, which now
// buckets as exploratory under the #refocus bug fix.)
func TestWatcher_IncrementalAppendAttributesViaCommit(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	commitTime, err := time.Parse(time.RFC3339, "2026-05-19T10:00:00Z")
	if err != nil {
		t.Fatalf("parse commit time: %v", err)
	}
	var gitCalls atomic.Int32
	commit := gitCommit{Hash: "h", Timestamp: commitTime, Branches: []string{"feature/inc"}, IssueID: "issue-98"}

	rec := &recordingStore{}
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      fakeGitLog(&gitCalls, []gitCommit{commit}, nil),
	})

	path := filepath.Join(projectsDir, "s1.jsonl")
	writeJSONLLine(t, path, "sess-inc", repo, "feature/inc", 100)
	if events := waitForEvents(t, rec, 1, 2*time.Second); len(events) != 1 {
		t.Fatalf("first turn: got %d events, want 1", len(events))
	}

	// Append a second assistant turn; the incremental path must re-attribute.
	appendJSONLLine(t, path, "sess-inc", repo, "feature/inc", 50)
	events := waitForEvents(t, rec, 2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("appended turn: got %d events, want 2 (incremental turn must also attribute)", len(events))
	}
	for _, e := range events {
		if e.IssueID != "issue-98" {
			t.Errorf("issue_id = %q, want issue-98", e.IssueID)
		}
	}
}

// TestWatcher_NewSubdirAttachedDynamically checks that a project subdir
// created after the watcher starts gets a fresh fsnotify subscription, so
// JSONL files written into it are captured. This is the everyday case for
// "developer opens Claude in a new repo while tierd serve is already up."
func TestWatcher_NewSubdirAttachedDynamically(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()

	rec := &recordingStore{}
	// Start the watcher BEFORE creating the project subdir.
	startWatcher(t, claudeDir, []string{repo}, rec)

	// Now create a new subdir — the watcher should pick it up via CREATE.
	projectsDir := filepath.Join(claudeDir, "projects", "newproject")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Attaching the fsnotify watch to the new subdir is asynchronous: the
	// watcher must observe the subdir CREATE and register the watch (via
	// addRecursive) before any write inside it will fire an event. That
	// registration only adds the watch — it does not rescan files already
	// present — so a write that lands BEFORE the watch attaches is lost, and
	// since nothing else modifies the file, the session is never ingested.
	// A single fixed-sleep-then-write raced that attach and intermittently
	// missed the write (the #340 flake). Instead, rewrite the file on a
	// bounded schedule so at least one WRITE lands AFTER the watch attaches,
	// and stop the instant the watcher ingests the session. Each rewrite is
	// spaced past the 50ms debounce, so the post-attach write flushes on its
	// own — but we break on the first event, so only one survives.
	path := filepath.Join(projectsDir, "s1.jsonl")
	var events []TokenEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		writeJSONLLine(t, path, "sess-late", repo, "feature/42-foo", 100)
		if events = waitForEvents(t, rec, 1, 200*time.Millisecond); len(events) >= 1 {
			break
		}
	}
	if len(events) < 1 {
		t.Fatalf("session in dynamically-created subdir never ingested "+
			"within deadline: new subdir not being watched (got %d events)",
			len(events))
	}
}

// TestWatcher_DebounceCoalesces verifies that rapid successive writes to the
// same file produce one ingestion, not N. With a 50ms debounce and 5 writes
// spaced 5ms apart, we should see exactly one insert (the file's final state).
func TestWatcher_DebounceCoalesces(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{repo}, rec)

	path := filepath.Join(projectsDir, "s1.jsonl")
	writeJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 100)
	for i := 0; i < 4; i++ {
		time.Sleep(5 * time.Millisecond)
		appendJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 50)
	}

	// Allow well beyond the debounce window for the last event to fire.
	time.Sleep(300 * time.Millisecond)
	events := rec.snapshot()
	// The file ends up with 2 distinct message.ids (writeJSONLLine uses
	// msg_<id>, appendJSONLLine uses msg2_<id> — all 4 appends share that
	// second id). After #19 the watcher emits one TokenEvent per message.id
	// per debounce flush. So:
	//   - 1 flush covering all 5 writes → 2 events (one per message.id).
	//   - 2 flushes if fsnotify split events on the 5ms gaps → up to 4
	//     events (two flushes × two ids), and a sane CI machine should
	//     coalesce them — but never 5 (which would mean the debounce
	//     window did nothing).
	if len(events) < 2 || len(events) > 4 {
		t.Errorf("debounced events = %d, want 2-4 (1-2 flushes × 2 distinct message.ids)", len(events))
	}
}

// TestWatcher_ShutdownIsClean verifies that cancelling the context exits the
// Run loop within a reasonable time. The startWatcher helper t.Cleanup
// asserts the 2-second bound on every test; this one exercises it explicitly.
func TestWatcher_ShutdownIsClean(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &recordingStore{}
	w := &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher did not return within 2s of cancel")
	}
}

// closedCheckingStore fails the test if an insert arrives after markClosed —
// simulating the caller closing the DB the moment Run returns. Regression
// guard for #61: the debounce re-arm timer used to be invisible to shutdown,
// and the WaitGroup Add happened outside the timer mutex, so writes could
// land after Run returned.
type closedCheckingStore struct {
	recordingStore
	t      *testing.T
	closed atomic.Bool
}

func (c *closedCheckingStore) Ingest(ctx context.Context, e TokenEvent) error {
	if c.closed.Load() {
		c.t.Errorf("Ingest after Run returned — write-after-db.Close race (#61)")
	}
	return c.recordingStore.Ingest(ctx, e)
}

// TestWatcher_NoStoreWriteAfterShutdown hammers a JSONL with appends right up
// to the moment of cancellation so debounce timers, in-flight process() calls,
// and the per-path re-arm path are all live when shutdown starts, then
// asserts no store write happens after Run returns. The race detector covers
// the map/WaitGroup orderings; the closed flag covers the semantic guarantee.
func TestWatcher_NoStoreWriteAfterShutdown(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := &closedCheckingStore{t: t}
	w := &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)

	path := filepath.Join(projectsDir, "s1.jsonl")
	writeJSONLLine(t, path, "sess-1", repo, "feature/42-foo", 100)
	// Keep events flowing while we cancel, so timers are mid-flight. The
	// goroutine appends inline rather than via appendJSONLLine: that helper
	// calls t.Fatalf, which is only valid from the test goroutine.
	stop := make(chan struct{})
	var appender sync.WaitGroup
	appender.Add(1)
	// Deferred (not just at the end of the happy path) so a t.Fatalf exit
	// can't leave the goroutine appending into a cleaned-up TempDir.
	defer func() {
		close(stop)
		appender.Wait()
	}()
	go func() {
		defer appender.Done()
		line := fmt.Sprintf(
			`{"type":"assistant","timestamp":"2026-05-19T10:00:01Z","sessionId":"sess-1","gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg2_sess-1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":50,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
			repo,
		)
		for {
			select {
			case <-stop:
				return
			default:
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Errorf("appender: open %s: %v", path, err)
					return
				}
				_, err = io.WriteString(f, line)
				_ = f.Close()
				if err != nil {
					t.Errorf("appender: append %s: %v", path, err)
					return
				}
				// The gap must exceed DebounceDelay (5ms): back-to-back
				// appends would reset the debounce timer forever and no
				// flush — hence no store write — would ever happen.
				time.Sleep(8 * time.Millisecond)
			}
		}
	}()

	// Prove the pipeline is actually live before cancelling — without at
	// least one insert the test would pass vacuously on both old and new
	// code.
	if got := waitForEvents(t, &rec.recordingStore, 1, 2*time.Second); len(got) == 0 {
		t.Fatalf("no events ingested before cancel — race window never exercised")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher did not return within 2s of cancel")
	}
	// The instant Run returns, the caller would close the DB — any insert
	// from here on is the #61 race. The appender keeps running (harmless:
	// the watcher is gone, its appends go nowhere) and is stopped by the
	// deferred close above.
	rec.closed.Store(true)
	// Give any orphaned timer (the pre-fix bug) time to fire and trip the
	// closed check. With the fix, nothing should be pending at all. Must
	// stay well above DebounceDelay or the orphan wouldn't fire in time.
	time.Sleep(100 * time.Millisecond)
}

// TestWatcher_NilStoreIsRejected catches the "you forgot to wire the store"
// configuration error at startup, not at the first event.
func TestWatcher_NilStoreIsRejected(t *testing.T) {
	w := &Watcher{}
	err := w.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when Store is nil")
	}
}

// TestCwdMatchesAnyTarget exercises the live-path CWD matcher directly so
// regressions are caught without filesystem timing flake. Mirrors the
// scenarios in TestFilterSessionsByRepo to keep live and batch paths
// honest about the same set of inputs.
func TestCwdMatchesAnyTarget(t *testing.T) {
	repo := t.TempDir()
	subdir := filepath.Join(repo, "services", "foo")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	near := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-other")
	if err := os.MkdirAll(near, 0o755); err != nil {
		t.Fatalf("mkdir near: %v", err)
	}
	targets := resolveTargets([]string{repo})

	cases := []struct {
		cwd     string
		wantHit bool
		comment string
	}{
		{repo, true, "repo root"},
		{subdir, true, "subdirectory of repo"},
		{near, false, "near-miss prefix sibling (/repo-other vs /repo)"},
		{t.TempDir(), false, "unrelated repo"},
		{"", false, "empty CWD"},
	}
	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			if got := cwdMatchesAnyTarget(tc.cwd, targets); got != tc.wantHit {
				t.Errorf("got %v, want %v for cwd=%q", got, tc.wantHit, tc.cwd)
			}
		})
	}
}

// TestMatchTarget_ResolvesWorktreeToMainRepo is the P0-11 live-path regression:
// a CWD inside a git worktree of the target repo (outside the repo's path
// prefix) must match via the worktree fallback, and the returned target.abs
// must be the MAIN repo root — that's the commit-cache key contract that makes
// join-to-commit read git log from the repo the worktree branches live in. A
// worktree of an unrelated repo must still miss.
func TestMatchTarget_ResolvesWorktreeToMainRepo(t *testing.T) {
	repoA := t.TempDir()
	initGitRepoWithCommit(t, repoA)
	wtA := addWorktree(t, repoA, filepath.Join(t.TempDir(), "wtA"), "feature/1-foo")
	sub := filepath.Join(wtA, "svc")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	targets := resolveTargets([]string{repoA})

	got, ok := matchTarget(sub, targets)
	if !ok {
		t.Fatalf("matchTarget(worktree subdir) missed, want match against repo A")
	}
	if got.abs != targets[0].abs {
		t.Errorf("matched target.abs = %q, want %q (main repo root — commit-cache key contract)", got.abs, targets[0].abs)
	}

	// Control: a worktree of an unrelated repo must not match repo A's targets.
	repoB := t.TempDir()
	initGitRepoWithCommit(t, repoB)
	wtB := addWorktree(t, repoB, filepath.Join(t.TempDir(), "wtB"), "feature/2-bar")
	if _, ok := matchTarget(wtB, targets); ok {
		t.Errorf("matchTarget(foreign worktree) matched repo A — identity check leaked")
	}
}

// TestWatcher_IngestsWorktreeSession is the P0-11 end-to-end proof: a live
// session whose CWD is a worktree of the watched repo is ingested. The branch
// ("main") does NOT parse to an issue id, so attribution can succeed ONLY if
// matchTarget resolves the worktree CWD to repo, keys the commit cache on the
// MAIN repo root, and joins against its git log — exactly the chain P0-11 wires.
func TestWatcher_IngestsWorktreeSession(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	initGitRepoWithCommit(t, repo)
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "wt"), "feature/77-wt")
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	commitTime, err := time.Parse(time.RFC3339, "2026-05-19T10:00:00Z")
	if err != nil {
		t.Fatalf("parse commit time: %v", err)
	}
	commit := gitCommit{Hash: "h", Timestamp: commitTime, Branches: []string{"feature/wt"}, IssueID: "issue-77"}
	// Capture the repoPath the commit cache keys git log on, so we can assert the
	// join reads from the MAIN repo root (matchTarget's target.abs) — not the
	// worktree. Guarded by a mutex because gitLogFn runs on a process() goroutine.
	var mu sync.Mutex
	var gotRepoPath string
	gitLogFn := func(_ context.Context, repoPath string, _ time.Time) ([]gitCommit, error) {
		mu.Lock()
		gotRepoPath = repoPath
		mu.Unlock()
		return []gitCommit{commit}, nil
	}
	rec := &recordingStore{}
	bootWatcher(t, &Watcher{
		ClaudeDir:     claudeDir,
		Repos:         []string{repo},
		Ingester:      rec,
		Checkpoints:   rec,
		DeveloperID:   "alice",
		DebounceDelay: 50 * time.Millisecond,
		gitLogFn:      gitLogFn,
	})

	// CWD is the worktree (outside repo's path prefix); the issueless branch
	// "feature/wt" yields no issue id on its own — only worktree resolution + the
	// commit join (in the MAIN repo's git log) can attribute.
	writeJSONLLine(t, filepath.Join(projectsDir, "s1.jsonl"), "sess-wt", wt, "feature/wt", 100)

	events := waitForEvents(t, rec, 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (worktree session must be ingested)", len(events))
	}
	if events[0].IssueID != "issue-77" {
		t.Errorf("issue_id = %q, want issue-77 (attributed via main repo's commit log)", events[0].IssueID)
	}
	// The commit-cache key contract: git log is read from the MAIN repo root, the
	// abs form resolveTargets records for repo — where the worktree branch's
	// commits live. Without worktree resolution returning the main target this
	// would be the worktree path (or the event would never land at all).
	wantRepoPath := resolveTargets([]string{repo})[0].abs
	mu.Lock()
	got := gotRepoPath
	mu.Unlock()
	if got != wantRepoPath {
		t.Errorf("commit cache read git log from %q, want main repo root %q", got, wantRepoPath)
	}
}

// TestPathInside locks in the trailing-separator guard. Without it,
// /Users/me/repo-other matches /Users/me/repo and cross-repo bleed comes
// back. This is the single line that protects the #15 fix in the live path.
func TestPathInside(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a/b-other", "/a/b", false},
		{"/x", "/a/b", false},
	}
	for _, tc := range cases {
		if got := pathInside(tc.child, tc.parent); got != tc.want {
			t.Errorf("pathInside(%q, %q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}
