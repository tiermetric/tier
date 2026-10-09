package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Store half of #919 (ruling R-2026-09-28-9): erasure tombstones or deletes the
// subject's watcher_checkpoint rows, found through token_events.session_id;
// the export discloses them; and the watcher's own writes cannot undo a
// tombstone.

// seedSessionCheckpoint stores one token_events row for dev in sessionID and a
// JSONL watcher checkpoint at path carrying that session in its metadata.
func seedSessionCheckpoint(t *testing.T, db *DB, dev, sessionID, path string) WatcherCheckpoint {
	t.Helper()
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity, session_id)
		 VALUES (?, 'issue-919', 'claude-sonnet-4', 1000, 'jsonl', 'realtime', ?)`, dev, sessionID); err != nil {
		t.Fatalf("seed token_events: %v", err)
	}
	cp := WatcherCheckpoint{
		Path: path, Inode: 4242, Offset: 512, HeadCRC: 99, HeadLen: 512,
		Metadata: `{"SessionID":"` + sessionID + `","GitBranch":"feature/919-x","CWD":"/Users/` + dev + `/src/proj"}`,
	}
	if err := db.SaveWatcherCheckpoint(ctx, cp); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	return cp
}

// existingFile creates a real file so erasure's stat sees the session file present.
func existingFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func mustLoadCheckpoint(t *testing.T, db *DB, path string) (WatcherCheckpoint, bool) {
	t.Helper()
	cp, ok, err := db.LoadWatcherCheckpoint(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadWatcherCheckpoint(%s): %v", path, err)
	}
	return cp, ok
}

// TestEraseDeveloper_TombstonesSubjectCheckpoints: the subject's row keeps its
// path, inode, offset and head fingerprint and loses cwd/branch/session id;
// another developer's row and another collector's namespaced row are untouched;
// a second erasure finds nothing.
func TestEraseDeveloper_TombstonesSubjectCheckpoints(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	alicePath := existingFile(t, "alice.jsonl")
	bobPath := existingFile(t, "bob.jsonl")
	aliceBefore := seedSessionCheckpoint(t, db, "alice", "sess-alice", alicePath)
	bobBefore := seedSessionCheckpoint(t, db, "bob", "sess-bob", bobPath)
	// A namespaced (non-file) row whose blob happens to carry alice's session id:
	// the erasure must not act on another collector's row (IsFileCheckpoint).
	foreign := WatcherCheckpoint{Path: "opencode-db:/x/opencode.db", Metadata: `{"SessionID":"sess-alice"}`}
	if err := db.SaveWatcherCheckpoint(ctx, foreign); err != nil {
		t.Fatalf("seed foreign row: %v", err)
	}

	counts, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if counts["watcher_checkpoint"] != 1 {
		t.Errorf("counts[watcher_checkpoint] = %d, want 1", counts["watcher_checkpoint"])
	}

	got, ok := mustLoadCheckpoint(t, db, alicePath)
	if !ok {
		t.Fatalf("alice's checkpoint was deleted; its session file exists, so it must be tombstoned")
	}
	if !got.Tombstoned() || got.Metadata != CheckpointTombstoneMetadata {
		t.Errorf("alice's checkpoint metadata = %q, want the tombstone %q", got.Metadata, CheckpointTombstoneMetadata)
	}
	if got.Inode != aliceBefore.Inode || got.Offset != aliceBefore.Offset ||
		got.HeadCRC != aliceBefore.HeadCRC || got.HeadLen != aliceBefore.HeadLen {
		t.Errorf("tombstone = %+v, want the pre-erase identity %+v kept", got, aliceBefore)
	}
	if b, _ := mustLoadCheckpoint(t, db, bobPath); b.Metadata != bobBefore.Metadata {
		t.Errorf("bob's checkpoint changed: %q, want %q", b.Metadata, bobBefore.Metadata)
	}
	if f, _ := mustLoadCheckpoint(t, db, foreign.Path); f.Metadata != foreign.Metadata {
		t.Errorf("the namespaced row changed: %q", f.Metadata)
	}

	again, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("second EraseDeveloper: %v", err)
	}
	if again["watcher_checkpoint"] != 0 {
		t.Errorf("second erase changed %d checkpoint rows, want 0", again["watcher_checkpoint"])
	}
	if got2, _ := mustLoadCheckpoint(t, db, alicePath); got2.Metadata != CheckpointTombstoneMetadata {
		t.Errorf("second erase disturbed the tombstone: %q", got2.Metadata)
	}
}

// TestEraseDeveloper_DeletesCheckpointWhoseSessionFileIsGone: no file, no
// tombstone to obey — the row is deleted outright.
func TestEraseDeveloper_DeletesCheckpointWhoseSessionFileIsGone(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	gone := filepath.Join(t.TempDir(), "gone.jsonl")
	seedSessionCheckpoint(t, db, "alice", "sess-gone", gone)
	counts, err := db.EraseDeveloper(context.Background(), "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if counts["watcher_checkpoint"] != 1 {
		t.Errorf("counts[watcher_checkpoint] = %d, want 1", counts["watcher_checkpoint"])
	}
	if _, ok := mustLoadCheckpoint(t, db, gone); ok {
		t.Errorf("checkpoint for a session file that no longer exists survived the erasure")
	}
}

// TestSaveWatcherCheckpoint_RefusesToOverwriteTombstone is the store-level race
// guard: a watcher save over a tombstone is refused and changes nothing. The
// control arm shows an ordinary row still updates.
func TestSaveWatcherCheckpoint_RefusesToOverwriteTombstone(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	live := existingFile(t, "live.jsonl")
	seedSessionCheckpoint(t, db, "bob", "sess-bob", live)
	moved := WatcherCheckpoint{Path: live, Inode: 1, Offset: 900, HeadCRC: 2, HeadLen: 3, Metadata: `{"SessionID":"sess-bob"}`}
	if err := db.SaveWatcherCheckpoint(ctx, moved); err != nil {
		t.Fatalf("control: save over an ordinary row: %v", err)
	}
	if cp, _ := mustLoadCheckpoint(t, db, live); cp.Offset != 900 {
		t.Fatalf("control: ordinary row not updated (offset %d)", cp.Offset)
	}

	erased := existingFile(t, "erased.jsonl")
	before := seedSessionCheckpoint(t, db, "alice", "sess-alice", erased)
	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	over := WatcherCheckpoint{Path: erased, Inode: before.Inode, Offset: 2048, HeadCRC: before.HeadCRC,
		HeadLen: before.HeadLen, Metadata: before.Metadata}
	if err := db.SaveWatcherCheckpoint(ctx, over); !errors.Is(err, ErrCheckpointTombstoned) {
		t.Fatalf("save over a tombstone returned %v, want ErrCheckpointTombstoned", err)
	}
	cp, _ := mustLoadCheckpoint(t, db, erased)
	if cp.Metadata != CheckpointTombstoneMetadata || cp.Offset != before.Offset {
		t.Errorf("refused save changed the tombstone: %+v", cp)
	}
}

// TestDeleteWatcherCheckpoint_KeepsTombstoneWhileFileExists: the watcher's
// delete (also called after an abandoned parse while the file exists) cannot
// remove a tombstone unless the file is gone; an ordinary row is deleted either
// way; DeleteWatcherTombstone removes a tombstone and nothing else.
func TestDeleteWatcherCheckpoint_KeepsTombstoneWhileFileExists(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	erased := existingFile(t, "erased.jsonl")
	seedSessionCheckpoint(t, db, "alice", "sess-alice", erased)
	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if err := db.DeleteWatcherCheckpoint(ctx, erased); err != nil {
		t.Fatalf("DeleteWatcherCheckpoint: %v", err)
	}
	if cp, ok := mustLoadCheckpoint(t, db, erased); !ok || !cp.Tombstoned() {
		t.Fatalf("DeleteWatcherCheckpoint removed a tombstone whose file still exists (present=%v)", ok)
	}

	ordinary := existingFile(t, "ordinary.jsonl")
	seedSessionCheckpoint(t, db, "bob", "sess-bob", ordinary)
	if err := db.DeleteWatcherTombstone(ctx, ordinary); err != nil {
		t.Fatalf("DeleteWatcherTombstone(ordinary): %v", err)
	}
	if _, ok := mustLoadCheckpoint(t, db, ordinary); !ok {
		t.Errorf("DeleteWatcherTombstone removed an ordinary row")
	}
	if err := db.DeleteWatcherCheckpoint(ctx, ordinary); err != nil {
		t.Fatalf("DeleteWatcherCheckpoint(ordinary): %v", err)
	}
	if _, ok := mustLoadCheckpoint(t, db, ordinary); ok {
		t.Errorf("an ordinary row survived DeleteWatcherCheckpoint while its file exists")
	}

	if err := os.Remove(erased); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := db.DeleteWatcherCheckpoint(ctx, erased); err != nil {
		t.Fatalf("DeleteWatcherCheckpoint after removal: %v", err)
	}
	if _, ok := mustLoadCheckpoint(t, db, erased); ok {
		t.Errorf("a tombstone survived DeleteWatcherCheckpoint after its file was removed")
	}

	reused := existingFile(t, "reused.jsonl")
	seedSessionCheckpoint(t, db, "carol", "sess-carol", reused)
	if _, err := db.EraseDeveloper(ctx, "carol"); err != nil {
		t.Fatalf("EraseDeveloper(carol): %v", err)
	}
	if err := db.DeleteWatcherTombstone(ctx, reused); err != nil {
		t.Fatalf("DeleteWatcherTombstone: %v", err)
	}
	if _, ok := mustLoadCheckpoint(t, db, reused); ok {
		t.Errorf("DeleteWatcherTombstone left the tombstone in place")
	}
}

// TestExportDeveloper_IncludesSubjectCheckpoints: the DSAR export carries the
// subject's checkpoint rows in full — path key and metadata — and no one
// else's. After erasure the tombstone no longer joins to the subject.
func TestExportDeveloper_IncludesSubjectCheckpoints(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	alicePath := existingFile(t, "alice.jsonl")
	aliceCP := seedSessionCheckpoint(t, db, "alice", "sess-alice", alicePath)
	seedSessionCheckpoint(t, db, "bob", "sess-bob", existingFile(t, "bob.jsonl"))
	if err := db.SaveWatcherCheckpoint(ctx, WatcherCheckpoint{Path: "opencode-db:/x", Metadata: `{"SessionID":"sess-alice"}`}); err != nil {
		t.Fatalf("seed foreign row: %v", err)
	}

	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	if len(exp.WatcherCheckpoint) != 1 {
		t.Fatalf("exported %d checkpoint rows, want exactly alice's one: %+v", len(exp.WatcherCheckpoint), exp.WatcherCheckpoint)
	}
	got := exp.WatcherCheckpoint[0]
	if got.Path != alicePath || got.Metadata != aliceCP.Metadata || got.ByteOffset != aliceCP.Offset ||
		got.Inode != int64(aliceCP.Inode) || got.HeadCRC != int64(aliceCP.HeadCRC) || got.HeadLen != int64(aliceCP.HeadLen) {
		t.Errorf("exported row = %+v, want alice's stored row %+v", got, aliceCP)
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("exported updated_at is zero")
	}

	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	after, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper after erase: %v", err)
	}
	if after.RowCount() != 0 {
		t.Errorf("export after erase has %d rows, want 0 (checkpoint rows: %+v)", after.RowCount(), after.WatcherCheckpoint)
	}
}
