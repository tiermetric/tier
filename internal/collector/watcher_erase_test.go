package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// Tests for #919 (ruling R-2026-09-28-9): an erasure tombstones the subject's
// watcher_checkpoint rows and the watcher OBEYS the tombstone — it never reads
// the erased session's file again, appends included, and never overwrites the
// marker, while a file whose head bytes differ at the same path is captured as a
// new session.

const eraseTestDebounce = 50 * time.Millisecond

// storeIngester forwards events to a real store field-for-field, as
// ingester.Store does in production. That package imports collector, so an
// in-package test cannot import it.
type storeIngester struct{ db *store.DB }

func (s storeIngester) Ingest(ctx context.Context, ev TokenEvent) error {
	return s.db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer:      ev.Developer,
		IssueID:        ev.IssueID,
		Model:          ev.Model,
		InputTok:       ev.InputTok,
		OutputTok:      ev.OutputTok,
		CacheRead:      ev.CacheRead,
		CacheWrite5m:   ev.CacheWrite5m,
		CacheWrite1h:   ev.CacheWrite1h,
		CostMicro:      ev.CostMicro,
		Source:         ev.Source,
		Fidelity:       ev.Fidelity,
		IdempotencyKey: ev.IdempotencyKey,
		Repo:           ev.Repo,
		SessionID:      ev.SessionID,
		Host:           ev.Host,
		BillingMode:    ev.BillingMode,
		Timestamp:      ev.Timestamp,
	})
}

// hookedCheckpoints is the real store with LoadWatcherCheckpoint instrumented:
// it counts the watcher's per-debounce reads of each path, and afterLoad runs
// after the read and BEFORE the watcher sees the result — the one point where a
// test can land an erasure between the watcher's tombstone check and its save.
type hookedCheckpoints struct {
	*store.DB
	mu        sync.Mutex
	loads     map[string]int
	afterLoad func(path string)
	// afterSave, when set, observes every SaveWatcherCheckpoint result.
	afterSave func(path string, err error)
}

func (h *hookedCheckpoints) SaveWatcherCheckpoint(ctx context.Context, cp store.WatcherCheckpoint) error {
	err := h.DB.SaveWatcherCheckpoint(ctx, cp)
	h.mu.Lock()
	hook := h.afterSave
	h.mu.Unlock()
	if hook != nil {
		hook(cp.Path, err)
	}
	return err
}

func (h *hookedCheckpoints) LoadWatcherCheckpoint(ctx context.Context, key string) (store.WatcherCheckpoint, bool, error) {
	cp, ok, err := h.DB.LoadWatcherCheckpoint(ctx, key)
	h.mu.Lock()
	if h.loads == nil {
		h.loads = map[string]int{}
	}
	h.loads[key]++
	hook := h.afterLoad
	h.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return cp, ok, err
}

func (h *hookedCheckpoints) loadCount(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loads[path]
}

type eraseRig struct {
	claudeDir, repo, projects string
	db                        *store.DB
	worktrees                 *worktreeIndex // the watcher's #823 option
	idle                      time.Duration  // the watcher's idleAfter
}

func newEraseRig(t *testing.T) eraseRig {
	t.Helper()
	r := eraseRig{claudeDir: t.TempDir(), repo: t.TempDir()}
	r.projects = filepath.Join(r.claudeDir, "projects", "p1")
	if err := os.MkdirAll(r.projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r.db = db
	return r
}

// run boots a watcher over the real store and returns once it is live (it has
// checkpointed a foreign-repo probe). stop cancels it and waits for Run to
// return; it is also registered as cleanup and is safe to call twice.
func (r eraseRig) run(t *testing.T, cps CheckpointStore) (stop func()) {
	t.Helper()
	return r.runWith(t, cps, storeIngester{r.db})
}

// runWith is run with the watcher's Ingester supplied by the test.
func (r eraseRig) runWith(t *testing.T, cps CheckpointStore, ing Ingester) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := &Watcher{
		ClaudeDir: r.claudeDir, Repos: []string{r.repo},
		Ingester: ing, Checkpoints: cps,
		DeveloperID: "alice", DebounceDelay: eraseTestDebounce,
		worktrees: r.worktrees, idleAfter: r.idle,
	}
	go func() { _ = w.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("watcher did not exit within 2s of ctx cancel")
			}
		})
	}
	t.Cleanup(stop)

	// A fresh probe name per run: a probe checkpointed by an earlier run in the
	// same test would read as "live" before this watcher has started.
	probe := filepath.Join(r.claudeDir, "projects", fmt.Sprintf("ready-probe-%d.jsonl", time.Now().UnixNano()))
	foreign := t.TempDir()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		writeJSONLLine(t, probe, "sess-ready-probe", foreign, "main", 1)
		// A second message, so a watcher holding back the open one (#823)
		// still checkpoints the first.
		appendTaggedLine(t, probe, "sess-ready-probe", foreign, "p2", 1)
		end := time.Now().Add(4*eraseTestDebounce + 200*time.Millisecond)
		for time.Now().Before(end) {
			if _, ok, _ := r.db.LoadWatcherCheckpoint(context.Background(), probe); ok {
				return stop
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatalf("watcher never went live: no checkpoint for probe %s", probe)
	return stop
}

// sessionInputs returns the input_tok of every stored token_events row for
// sessionID, read through the DSAR export (the store's exported view of the
// subject's rows). Each test line carries a distinct input_tok, so the values
// name which lines are stored.
func (r eraseRig) sessionInputs(t *testing.T, sessionID string) []int64 {
	t.Helper()
	exp, err := r.db.ExportDeveloper(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	var out []int64
	for _, ev := range exp.TokenEvents {
		if ev.SessionID != nil && *ev.SessionID == sessionID {
			out = append(out, ev.InputTok)
		}
	}
	return out
}

func (r eraseRig) checkpoint(t *testing.T, path string) (store.WatcherCheckpoint, bool) {
	t.Helper()
	cp, ok, err := r.db.LoadWatcherCheckpoint(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadWatcherCheckpoint: %v", err)
	}
	return cp, ok
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// captureFirstLine writes one line (input_tok 111) for sessionID at path and
// waits until it is stored AND checkpointed at the file's full size.
func (r eraseRig) captureFirstLine(t *testing.T, path, sessionID string) store.WatcherCheckpoint {
	t.Helper()
	writeJSONLLine(t, path, sessionID, r.repo, "feature/42-foo", 111)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	var cp store.WatcherCheckpoint
	waitUntil(t, "first line stored and checkpointed", func() bool {
		var ok bool
		cp, ok = r.checkpoint(t, path)
		return ok && !cp.Tombstoned() && cp.Offset == info.Size() && len(r.sessionInputs(t, sessionID)) == 1
	})
	return cp
}

// erase runs the real erasure for alice and asserts it tombstoned exactly the
// one checkpoint row the test created for the subject's session.
func (r eraseRig) erase(t *testing.T, path string, before store.WatcherCheckpoint) {
	t.Helper()
	counts, err := r.db.EraseDeveloper(context.Background(), "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if counts["watcher_checkpoint"] != 1 {
		t.Fatalf("erase changed %d watcher_checkpoint rows, want 1 (the subject's session file)", counts["watcher_checkpoint"])
	}
	assertTombstone(t, r, path, before)
}

func assertTombstone(t *testing.T, r eraseRig, path string, before store.WatcherCheckpoint) {
	t.Helper()
	cp, ok := r.checkpoint(t, path)
	if !ok {
		t.Fatalf("checkpoint row for %s is gone; want an erasure tombstone (the file still exists)", path)
	}
	if cp.Metadata != store.CheckpointTombstoneMetadata {
		t.Fatalf("checkpoint metadata = %s, want the tombstone %s — the erased session's cwd/branch/session id are back, and the file is no longer marked", cp.Metadata, store.CheckpointTombstoneMetadata)
	}
	if cp.Offset != before.Offset || cp.Inode != before.Inode || cp.HeadCRC != before.HeadCRC || cp.HeadLen != before.HeadLen {
		t.Fatalf("tombstone identity = %+v, want the pre-erase offset/inode/head %+v", cp, before)
	}
}

// appendTaggedLine appends one assistant line with its OWN message id (tag), so two
// appends are two stored rows rather than one deduplicated message — the
// shared appendJSONLLine reuses a single id per session.
func appendTaggedLine(t *testing.T, path, sessionID, repo, tag string, inputTok int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-05-19T10:00:02Z","sessionId":%q,"gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg_%s_%s","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		sessionID, repo, sessionID, tag, inputTok)
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// awaitSkippedAppend appends a line (input_tok 222) to the erased file, writes
// a control session that MUST be captured (proving the watcher is processing
// writes), waits until the watcher has read the erased file's row after the
// append, then gives a wrongly-ingesting watcher four debounces to land a row.
func (r eraseRig) awaitSkippedAppend(t *testing.T, path, sessionID string, loads func() int) {
	t.Helper()
	loadsBefore := loads()
	appendTaggedLine(t, path, sessionID, r.repo, "a222", 222)
	control := filepath.Join(r.projects, "control.jsonl")
	writeJSONLLine(t, control, "sess-control", r.repo, "feature/42-foo", 999)
	waitUntil(t, "control session captured (the watcher is processing writes)", func() bool {
		return len(r.sessionInputs(t, "sess-control")) == 1
	})
	waitUntil(t, "the watcher read the erased file's row after the append", func() bool {
		return loads() > loadsBefore
	})
	time.Sleep(4 * eraseTestDebounce)
}

// TestWatcher_EraseWhileRunningThenAppendReturnsNothing is ruling test (1):
// erase the subject while the watcher is running and holding the file's tail
// state in memory, then append to the subject's own file. Nothing returns — not
// the pre-offset line the erasure deleted, not the appended line — and the
// tombstone is intact.
func TestWatcher_EraseWhileRunningThenAppendReturnsNothing(t *testing.T) {
	r := newEraseRig(t)
	cps := &hookedCheckpoints{DB: r.db}
	r.run(t, cps)

	path := filepath.Join(r.projects, "erased.jsonl")
	before := r.captureFirstLine(t, path, "sess-erased")
	r.erase(t, path, before)
	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Fatalf("control: erase left rows %v for the session", got)
	}

	r.awaitSkippedAppend(t, path, "sess-erased", func() int { return cps.loadCount(path) })

	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Errorf("after erase + append, stored rows for the erased session = %v, want none (111 = an erased pre-offset line re-inserted, 222 = the append captured)", got)
	}
	assertTombstone(t, r, path, before)
}

// TestWatcher_EraseThenRestartReturnsNothing is ruling test (2): erase while the
// watcher is down, restart it, append. A restarted watcher has no in-memory
// state, so without the tombstone it would parse the file from byte 0 and
// re-insert every erased line.
func TestWatcher_EraseThenRestartReturnsNothing(t *testing.T) {
	r := newEraseRig(t)
	stop := r.run(t, r.db)
	path := filepath.Join(r.projects, "erased.jsonl")
	before := r.captureFirstLine(t, path, "sess-erased")
	stop()

	r.erase(t, path, before)

	cps := &hookedCheckpoints{DB: r.db}
	r.run(t, cps)
	r.awaitSkippedAppend(t, path, "sess-erased", func() int { return cps.loadCount(path) })

	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Errorf("after erase + restart + append, stored rows for the erased session = %v, want none", got)
	}
	assertTombstone(t, r, path, before)
}

// TestWatcher_SaveRacingAnEraseCannotOverwriteTheTombstone pins the store-level
// guard. The erasure lands AFTER the watcher's per-debounce tombstone check has
// read the row as live, so the watcher parses the appended chunk and saves its
// checkpoint over a row that is by then a tombstone. SaveWatcherCheckpoint's
// DO UPDATE ... WHERE must refuse that save; otherwise the subject's cwd, branch
// and session id are written back and the file is un-marked, so the NEXT append
// (333) is captured too.
//
// ⚠️ The appended chunk (222) the watcher was already reading when the erase
// landed IS stored — the erase ran before its insert. That is the known window
// between the check and the insert; this test pins what the guard closes (the
// marker, the pre-offset line, every later append), and does not assert on 222.
func TestWatcher_SaveRacingAnEraseCannotOverwriteTheTombstone(t *testing.T) {
	r := newEraseRig(t)
	path := filepath.Join(r.projects, "erased.jsonl")
	var armed atomic.Bool
	erased := make(chan struct{})
	saveErr := make(chan error, 1)
	cps := &hookedCheckpoints{DB: r.db}
	cps.afterLoad = func(p string) {
		if p != path || !armed.Load() {
			return
		}
		select {
		case <-erased:
			return // already injected
		default:
		}
		if _, err := r.db.EraseDeveloper(context.Background(), "alice"); err != nil {
			t.Errorf("EraseDeveloper inside the watcher's debounce: %v", err)
		}
		close(erased)
	}
	cps.afterSave = func(p string, err error) {
		if p != path || !armed.Load() {
			return
		}
		select {
		case <-erased:
		default:
			return
		}
		if armed.CompareAndSwap(true, false) {
			saveErr <- err
		}
	}
	r.run(t, cps)

	before := r.captureFirstLine(t, path, "sess-erased")
	armed.Store(true)
	appendTaggedLine(t, path, "sess-erased", r.repo, "a222", 222)
	select {
	case err := <-saveErr:
		if !errors.Is(err, store.ErrCheckpointTombstoned) {
			t.Fatalf("the save that raced the erase returned %v, want store.ErrCheckpointTombstoned — the store accepted a write over the tombstone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher never saved after the injected erase, so the guard was never exercised")
	}

	// The raced callback is finished. A later append must be skipped: the row is
	// still a tombstone for this file.
	loadsBefore := cps.loadCount(path)
	appendTaggedLine(t, path, "sess-erased", r.repo, "a333", 333)
	waitUntil(t, "the watcher read the row after the second append", func() bool {
		return cps.loadCount(path) > loadsBefore
	})
	time.Sleep(4 * eraseTestDebounce)

	assertTombstone(t, r, path, before)
	for _, v := range r.sessionInputs(t, "sess-erased") {
		if v == 111 {
			t.Errorf("the erased pre-offset line (111) was re-inserted")
		}
		if v == 333 {
			t.Errorf("the append after the raced erase (333) was captured — the file was un-marked")
		}
	}
}

// fileIdentity returns the tail-state identity of path as it is on disk now:
// the tombstone an erasure would write for it.
func fileIdentity(t *testing.T, path string) store.WatcherCheckpoint {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	crc, n, err := fingerprintHead(path, int(info.Size()))
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return store.WatcherCheckpoint{
		Path: path, Inode: inodeOf(info), Offset: info.Size(),
		HeadCRC: crc, HeadLen: n, Metadata: store.CheckpointTombstoneMetadata,
	}
}

// TestWatcher_ObeysTombstoneForTheSameFile is the unit form of "the watcher
// skips a tombstoned file entirely, appends included": the tombstone describes
// the file on disk, so an append produces no event, no save and no release.
func TestWatcher_ObeysTombstoneForTheSameFile(t *testing.T) {
	claudeDir, repo := t.TempDir(), t.TempDir()
	projects := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projects, "erased.jsonl")
	writeJSONLLine(t, path, "sess-erased", repo, "feature/42-foo", 111)
	rec := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{fileIdentity(t, path)}}
	startWatcher(t, claudeDir, []string{repo}, rec)

	before := rec.loadCount(path)
	appendTaggedLine(t, path, "sess-erased", repo, "a222", 222)
	control := filepath.Join(projects, "control.jsonl")
	writeJSONLLine(t, control, "sess-control", repo, "feature/42-foo", 999)
	waitForCheckpoint(t, rec, control, 5*time.Second)
	waitUntil(t, "the watcher read the tombstoned row after the append", func() bool {
		return rec.loadCount(path) > before
	})
	time.Sleep(4 * 50 * time.Millisecond)

	for _, ev := range rec.snapshot() {
		if ev.SessionID == "sess-erased" {
			t.Errorf("event captured from a tombstoned file: %+v", ev)
		}
	}
	if _, saved := rec.checkpointSnapshot()[path]; saved {
		t.Errorf("the watcher saved a checkpoint for a tombstoned file")
	}
	rec.mu.Lock()
	cur, ok := rec.current(path)
	released := append([]string(nil), rec.releasedTombstones...)
	rec.mu.Unlock()
	if !ok || !cur.Tombstoned() {
		t.Errorf("tombstone for the same file is gone (present=%v, metadata=%q)", ok, cur.Metadata)
	}
	if len(released) != 0 {
		t.Errorf("the watcher released a tombstone that describes the file on disk: %v", released)
	}
}

// TestWatcher_NewFileHeadAtTombstonedPathIsANewSession: a file whose head no
// longer matches the tombstone — rewritten in place, so same path and inode and
// no remove event — is a new session. The tombstone is released and the new
// file is captured from byte 0, with none of the erased session's lines.
func TestWatcher_NewFileHeadAtTombstonedPathIsANewSession(t *testing.T) {
	claudeDir, repo := t.TempDir(), t.TempDir()
	projects := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projects, "reused.jsonl")
	writeJSONLLine(t, path, "sess-erased", repo, "feature/42-foo", 111)
	rec := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{fileIdentity(t, path)}}
	startWatcher(t, claudeDir, []string{repo}, rec)

	writeJSONLLine(t, path, "sess-new-and-longer", repo, "feature/42-foo", 444) // O_TRUNC: same inode, new head at least as long as the tombstone's
	waitUntil(t, "the new session is captured", func() bool {
		for _, ev := range rec.snapshot() {
			if ev.SessionID == "sess-new-and-longer" {
				return true
			}
		}
		return false
	})
	cp := waitForCheckpoint(t, rec, path, 5*time.Second)
	if cp.Tombstoned() {
		t.Fatalf("checkpoint for the new session is still the tombstone")
	}
	var meta sessionMetadata
	if err := json.Unmarshal([]byte(cp.Metadata), &meta); err != nil || meta.SessionID != "sess-new-and-longer" {
		t.Errorf("checkpoint metadata = %s (err %v), want the new session's", cp.Metadata, err)
	}
	for _, ev := range rec.snapshot() {
		if ev.SessionID == "sess-erased" {
			t.Errorf("an erased session's line was captured: %+v", ev)
		}
	}
	rec.mu.Lock()
	released := append([]string(nil), rec.releasedTombstones...)
	rec.mu.Unlock()
	if len(released) != 1 || released[0] != path {
		t.Errorf("released tombstones = %v, want exactly [%s]", released, path)
	}
}

// TestWatcher_StartupDeletesTombstoneOfGoneFileAndKeepsLiveOne: at startup a
// tombstone seeds no tail state; one whose file is gone is deleted (the ruling's
// "a row whose session file is gone is deleted"), one whose file exists stays.
func TestWatcher_StartupDeletesTombstoneOfGoneFileAndKeepsLiveOne(t *testing.T) {
	claudeDir, repo := t.TempDir(), t.TempDir()
	projects := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	live := filepath.Join(projects, "live.jsonl")
	writeJSONLLine(t, live, "sess-live", repo, "feature/42-foo", 111)
	gone := filepath.Join(projects, "gone.jsonl")
	goneTomb := store.WatcherCheckpoint{Path: gone, Inode: 7, Offset: 10, HeadCRC: 1, HeadLen: 10, Metadata: store.CheckpointTombstoneMetadata}
	rec := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{fileIdentity(t, live), goneTomb}}
	startWatcher(t, claudeDir, []string{repo}, rec)

	rec.mu.Lock()
	_, goneStill := rec.current(gone)
	liveCP, liveStill := rec.current(live)
	rec.mu.Unlock()
	if goneStill {
		t.Errorf("tombstone for a session file that no longer exists survived startup")
	}
	if !liveStill || !liveCP.Tombstoned() {
		t.Errorf("tombstone for an existing session file was removed at startup (present=%v)", liveStill)
	}
}

// TestCheckpointMetadataCarriesTheStoreSessionKey pins the join the erasure and
// the export depend on: the store finds a subject's checkpoint rows by reading
// store.CheckpointSessionIDKey out of this blob. Rename the field without a
// matching key and both would silently find nothing.
func TestCheckpointMetadataCarriesTheStoreSessionKey(t *testing.T) {
	cp, err := checkpointFromState("/x/s.jsonl", parseState{inode: 1, headLen: 1, metadata: sessionMetadata{SessionID: "sess-key"}})
	if err != nil {
		t.Fatalf("checkpointFromState: %v", err)
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(cp.Metadata), &blob); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := blob[store.CheckpointSessionIDKey]; got != "sess-key" {
		t.Errorf("metadata[%q] = %v, want the session id; metadata = %s", store.CheckpointSessionIDKey, got, cp.Metadata)
	}
}

// TestWatcher_ByteIdenticalRewriteThroughRenameStaysErased: the erased
// session's file is rewritten with the SAME bytes through a temp file and a
// rename — a backup restore, rsync or atomic save. The inode changes, the
// content does not. At a Claude Code path the file name is the session UUID,
// so this is still the erased session: the tombstone must hold, and neither
// the erased line (111) nor an append (222) may be stored.
func TestWatcher_ByteIdenticalRewriteThroughRenameStaysErased(t *testing.T) {
	r := newEraseRig(t)
	stop := r.run(t, r.db)
	path := filepath.Join(r.projects, "erased.jsonl")
	before := r.captureFirstLine(t, path, "sess-erased")
	stop()
	r.erase(t, path, before)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tmp := path + ".restore"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if inodeOf(info) == before.Inode {
		t.Fatalf("control: the rewrite kept inode %d, so it does not exercise an inode change", before.Inode)
	}

	cps := &hookedCheckpoints{DB: r.db}
	r.run(t, cps)
	r.awaitSkippedAppend(t, path, "sess-erased", func() int { return cps.loadCount(path) })

	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Errorf("after a byte-identical rewrite + append, stored rows for the erased session = %v, want none (the tombstone was released for the same session)", got)
	}
	assertTombstone(t, r, path, before)
}

// cancelIngester fails every insert for one session with context.Canceled —
// the watcher's cancelled-insert failure path — and forwards the rest.
type cancelIngester struct {
	next     Ingester
	session  string
	attempts atomic.Int32
}

func (c *cancelIngester) Ingest(ctx context.Context, ev TokenEvent) error {
	if ev.SessionID == c.session {
		c.attempts.Add(1)
		return context.Canceled
	}
	return c.next.Ingest(ctx, ev)
}

// TestWatcher_FailedReadKeepsCheckpointSoEraseTombstonesIt: a read that fails
// while the session file still exists must keep the file's checkpoint row. If
// the failure path deletes it, the erasure finds no row to tombstone, and the
// next start re-reads the file from byte 0 and stores the erased session again.
func TestWatcher_FailedReadKeepsCheckpointSoEraseTombstonesIt(t *testing.T) {
	r := newEraseRig(t)
	stop := r.run(t, r.db)
	path := filepath.Join(r.projects, "erased.jsonl")
	before := r.captureFirstLine(t, path, "sess-erased")
	stop()

	ing := &cancelIngester{next: storeIngester{r.db}, session: "sess-erased"}
	cps := &hookedCheckpoints{DB: r.db}
	stop = r.runWith(t, cps, ing)
	loads := cps.loadCount(path)
	appendTaggedLine(t, path, "sess-erased", r.repo, "f501", 501)
	waitUntil(t, "the appended line's insert was attempted and cancelled", func() bool {
		return ing.attempts.Load() > 0
	})
	// Per-path serialization: a second change to the path is processed only
	// after the first callback has returned, so a second row read proves the
	// failure path above has finished.
	appendTaggedLine(t, path, "sess-erased", r.repo, "f502", 502)
	waitUntil(t, "the failed callback finished (a later change to the path was processed)", func() bool {
		return cps.loadCount(path) >= loads+2
	})
	stop()

	counts, err := r.db.EraseDeveloper(context.Background(), "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if counts["watcher_checkpoint"] != 1 {
		t.Errorf("erase changed %d watcher_checkpoint rows, want 1 — the failure path deleted the row, so there was nothing to tombstone", counts["watcher_checkpoint"])
	}

	cps2 := &hookedCheckpoints{DB: r.db}
	r.run(t, cps2)
	r.awaitSkippedAppend(t, path, "sess-erased", func() int { return cps2.loadCount(path) })

	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Errorf("after failed read + erase + restart + append, stored rows for the erased session = %v, want none", got)
	}
	assertTombstone(t, r, path, before)
}

// failFirstLoad fails the watcher's first per-debounce row read for one path.
type failFirstLoad struct {
	*store.DB
	path   string
	failed atomic.Bool
}

func (f *failFirstLoad) LoadWatcherCheckpoint(ctx context.Context, key string) (store.WatcherCheckpoint, bool, error) {
	if key == f.path && f.failed.CompareAndSwap(false, true) {
		return store.WatcherCheckpoint{}, false, errors.New("injected checkpoint read failure")
	}
	return f.DB.LoadWatcherCheckpoint(ctx, key)
}

// TestWatcher_UnreadableRowRetriesTheChange: when the row read fails, the
// debounce skips the file but re-arms, so the change is captured without a
// further write to the file.
func TestWatcher_UnreadableRowRetriesTheChange(t *testing.T) {
	r := newEraseRig(t)
	path := filepath.Join(r.projects, "retry.jsonl")
	cps := &failFirstLoad{DB: r.db, path: path}
	r.run(t, cps)

	writeJSONLLine(t, path, "sess-retry", r.repo, "feature/42-foo", 111)
	waitUntil(t, "the change is captured after the failed row read, with no further write", func() bool {
		return len(r.sessionInputs(t, "sess-retry")) == 1
	})
	if !cps.failed.Load() {
		t.Fatal("control: the injected row-read failure never fired")
	}
}

// failTokIngester fails, with an ordinary (non-cancellation) error, every insert
// whose input_tok is failTok, and forwards the rest.
type failTokIngester struct {
	next     Ingester
	failTok  int
	attempts atomic.Int32
}

func (f *failTokIngester) Ingest(ctx context.Context, ev TokenEvent) error {
	if ev.InputTok == f.failTok {
		f.attempts.Add(1)
		return errors.New("injected insert failure")
	}
	return f.next.Ingest(ctx, ev)
}

// TestWatcher_PartialIngestFailureOnFirstReadStaysErasable pins the partial
// insert-failure path on a file's FIRST read: one line (111) is stored, the
// other (112) fails with an ordinary error, and the watcher saves a row at
// offset 0 so the chunk is retried. That row must carry the session id the
// stored line was written under, or the erasure cannot join it to the subject:
// it then deletes 111 but leaves an ordinary offset-0 row, and the next start
// re-reads the file from byte 0 and stores the erased session again.
func TestWatcher_PartialIngestFailureOnFirstReadStaysErasable(t *testing.T) {
	r := newEraseRig(t)
	ing := &failTokIngester{next: storeIngester{r.db}, failTok: 112}
	stop := r.runWith(t, r.db, ing)

	// Both lines land in one write (a rename into the watched directory), so
	// the file's first read sees both.
	staging := filepath.Join(t.TempDir(), "staged.jsonl")
	writeJSONLLine(t, staging, "sess-erased", r.repo, "feature/42-foo", 111)
	appendTaggedLine(t, staging, "sess-erased", r.repo, "p112", 112)
	path := filepath.Join(r.projects, "erased.jsonl")
	if err := os.Rename(staging, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	var before store.WatcherCheckpoint
	waitUntil(t, "first read stored 111, failed 112 and saved its row", func() bool {
		var ok bool
		before, ok = r.checkpoint(t, path)
		return ok && before.Inode != 0 && ing.attempts.Load() > 0 &&
			len(r.sessionInputs(t, "sess-erased")) == 1
	})
	stop()
	if before.Offset != 0 {
		t.Fatalf("control: the failed first read saved offset %d, want 0 (the chunk is retried from the start)", before.Offset)
	}

	r.erase(t, path, before)

	cps := &hookedCheckpoints{DB: r.db}
	r.run(t, cps)
	r.awaitSkippedAppend(t, path, "sess-erased", func() int { return cps.loadCount(path) })

	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Errorf("after partial ingest failure + erase + restart + append, stored rows for the erased session = %v, want none", got)
	}
	assertTombstone(t, r, path, before)
}
