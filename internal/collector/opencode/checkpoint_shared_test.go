package opencode

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestCheckpointRowSurvivesTheJSONLWatcher is the regression guard for a defect
// this collector INTRODUCED and that nothing else in the tree would have caught.
//
// 🔴 THE TABLE IS NOW SHARED. `watcher_checkpoint` was built for the JSONL
// watcher (#71): one row per tailed file, keyed by the file's absolute PATH. This
// collector stores its scan watermark in the same table under the namespaced key
// `opencode-db:<path>` (#719).
//
// The JSONL watcher loads EVERY row in that table at startup and prunes any whose
// path does not stat — a deliberate garbage-collection step for files removed
// while it was down. `opencode-db:/…` is not a filesystem path, so it never
// stats, so the watcher DELETED this collector's watermark on every boot of the
// one deployment where both run (`tierd serve`). Silently: the prune path logs
// nothing for a missing file, because for its own rows that is the normal case.
//
// The consequence was bounded but real — the watermark is a derived cache, so
// losing it costs one idempotent re-scan — yet it meant the persistence this
// collector claims to have was worth exactly nothing in production while every
// test in this package (which uses an in-memory checkpoint store) passed.
//
// The fix is store.IsFileCheckpoint: the watcher now skips rows that are not
// filesystem paths instead of assuming every row is its own. This test pins the
// half that belongs to THIS collector — that the key it writes is recognisably
// foreign. internal/collector's TestWatcherDoesNotPruneAnotherCollectorsCheckpoint
// pins the watcher's half against a real watcher run.
func TestCheckpointRowSurvivesTheJSONLWatcher(t *testing.T) {
	repo := repoDir(t)
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	logger, _ := newTestLogger()
	cp := &memCheckpoints{}
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger, Checkpoints: cp})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.setWatermark(context.Background(), 1_787_939_641_519, 3)

	saved, ok := cp.rows[c.checkpointKey()]
	if !ok {
		t.Fatalf("no checkpoint row was written; rows = %v", cp.rows)
	}
	if store.IsFileCheckpoint(saved.Path) {
		t.Errorf("checkpoint key %q looks like a filesystem path to store.IsFileCheckpoint, so the JSONL watcher will stat it, fail, and PRUNE it on every startup", saved.Path)
	}
	// CONTROL ARM. The assertion above is "the key is not a file path", which
	// would also pass if IsFileCheckpoint were broken and returned false for
	// everything — in which case the watcher would skip its OWN rows and lose all
	// resume state. Prove the predicate still says yes to a real one.
	if !store.IsFileCheckpoint(dbPath) {
		t.Fatalf("store.IsFileCheckpoint(%q) = false for a real absolute path — the predicate is broken, and the assertion above proves nothing", dbPath)
	}
}
