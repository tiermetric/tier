package store

import (
	"context"
	"testing"
)

// TestWatcherCheckpoint_RoundTrip covers save → load → upsert → delete for the
// #71 checkpoint table, including the lossless round-trip of the unsigned
// inode/head_crc through SQLite's signed INTEGER columns.
func TestWatcherCheckpoint_RoundTrip(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if cps, err := db.LoadWatcherCheckpoints(ctx); err != nil || len(cps) != 0 {
		t.Fatalf("empty load = (%v, %v), want (nil, nil)", cps, err)
	}

	cp := WatcherCheckpoint{
		Path:     "/home/alice/.claude/projects/x/sess.jsonl",
		Inode:    0xFFFFFFFF12345678, // high bit set — exercises uint64↔int64 reinterpret
		Offset:   4096,
		HeadCRC:  0xDEADBEEF, // full-range uint32
		HeadLen:  4096,
		Metadata: `{"SessionID":"sess-1","NextParseSeq":7}`,
	}
	if err := db.SaveWatcherCheckpoint(ctx, cp); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := db.LoadWatcherCheckpoints(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checkpoints, want 1", len(got))
	}
	if got[0] != cp {
		t.Errorf("round-trip diverged:\n got = %+v\nwant = %+v", got[0], cp)
	}

	// Upsert on the path PK: an advanced offset must update in place, not append.
	cp.Offset = 8192
	cp.Metadata = `{"SessionID":"sess-1","NextParseSeq":12}`
	if err := db.SaveWatcherCheckpoint(ctx, cp); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ = db.LoadWatcherCheckpoints(ctx)
	if len(got) != 1 {
		t.Fatalf("after upsert got %d rows, want 1 (path PK must dedup)", len(got))
	}
	if got[0] != cp {
		t.Errorf("upsert didn't update in place:\n got = %+v\nwant = %+v", got[0], cp)
	}

	// Delete removes it; deleting an absent path is a silent no-op.
	if err := db.DeleteWatcherCheckpoint(ctx, cp.Path); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.DeleteWatcherCheckpoint(ctx, "/does/not/exist.jsonl"); err != nil {
		t.Fatalf("delete absent path should be a no-op, got: %v", err)
	}
	if got, _ := db.LoadWatcherCheckpoints(ctx); len(got) != 0 {
		t.Fatalf("after delete got %d rows, want 0", len(got))
	}
}
