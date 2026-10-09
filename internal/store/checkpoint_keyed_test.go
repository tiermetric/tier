package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestLoadWatcherCheckpoint_Keyed pins the accessor added by #719: a consumer
// that owns exactly ONE row asks for it by key instead of enumerating the whole
// table and picking its own out.
//
// The enumeration pattern is not merely wasteful. `watcher_checkpoint` is shared
// between the JSONL watcher and the Opencode collector, and a consumer holding
// every other consumer's rows in its hand is one missing filter away from acting
// on one — which is exactly the defect IsFileCheckpoint exists to prevent, in the
// other direction.
func TestLoadWatcherCheckpoint_Keyed(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ck.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	const mine = "opencode-db:/home/alice/.local/share/opencode/opencode.db"
	const theirs = "/home/alice/.claude/projects/p1/s1.jsonl"
	for _, cp := range []WatcherCheckpoint{
		{Path: mine, Metadata: `{"schema":1,"opencode_time_updated_ms":42}`},
		{Path: theirs, Inode: 7, Offset: 100, HeadCRC: 9, HeadLen: 20, Metadata: `{"SessionID":"s"}`},
	} {
		if err := db.SaveWatcherCheckpoint(ctx, cp); err != nil {
			t.Fatalf("save %s: %v", cp.Path, err)
		}
	}

	got, ok, err := db.LoadWatcherCheckpoint(ctx, mine)
	if err != nil || !ok {
		t.Fatalf("LoadWatcherCheckpoint(%q) = (_, %v, %v), want found", mine, ok, err)
	}
	if got.Path != mine {
		t.Errorf("Path = %q, want %q", got.Path, mine)
	}
	if got.Metadata != `{"schema":1,"opencode_time_updated_ms":42}` {
		t.Errorf("Metadata = %q — the wrong row was returned", got.Metadata)
	}

	// An absent key is (zero, false, nil) — NOT an error. A collector that has
	// never run yet is the ordinary first-boot case, and returning an error for it
	// would make every fresh install log a failure.
	if _, ok, err := db.LoadWatcherCheckpoint(ctx, "opencode-db:/nope"); ok || err != nil {
		t.Errorf("an absent key = (_, %v, %v), want (false, nil)", ok, err)
	}

	// CONTROL: the OTHER row is still there and still readable, so this accessor
	// reads rather than consumes, and the two consumers do not collide.
	if _, ok, err := db.LoadWatcherCheckpoint(ctx, theirs); !ok || err != nil {
		t.Errorf("the watcher's own row = (_, %v, %v), want found", ok, err)
	}
	// And the enumerating accessor still returns BOTH — the watcher's seeding path
	// is unchanged by the addition of a keyed one.
	all, err := db.LoadWatcherCheckpoints(ctx)
	if err != nil {
		t.Fatalf("LoadWatcherCheckpoints: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("LoadWatcherCheckpoints returned %d rows, want 2", len(all))
	}
}
