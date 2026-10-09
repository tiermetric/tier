package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// TestWatcher_DoesNotPruneAnotherCollectorsCheckpoint is the watcher half of the
// shared-table fix (#719). The Opencode half is
// opencode.TestCheckpointRowSurvivesTheJSONLWatcher.
//
// 🔴 WHAT WENT WRONG. watcher_checkpoint is keyed by `path`, and until #719 every
// row in it was an absolute JSONL file path, so the startup loop could assume it
// owned all of them. The Opencode collector now stores a database-scan watermark
// in the same table under `opencode-db:<path>`. That key:
//
//   - unmarshals CLEANLY into sessionMetadata, because encoding/json ignores
//     unknown fields — so stateFromCheckpoint succeeds with a zero-valued struct
//     and the malformed-row arm never fires;
//   - then fails os.Stat, because it is not a filesystem path;
//   - and so falls into the orphan prune, which DELETES it — silently, since a
//     missing file is the ordinary case for the watcher's own rows.
//
// Net effect before the fix: `tierd serve` destroyed the Opencode watermark on
// every boot, the collector re-scanned the whole store every restart, and nothing
// in any log or test said so.
//
// This test runs a REAL watcher against a seeded foreign row and asserts it is
// neither pruned nor adopted.
func TestWatcher_DoesNotPruneAnotherCollectorsCheckpoint(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "p1")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// The exact row shape internal/collector/opencode writes.
	const foreignKey = "opencode-db:/Users/someone/.local/share/opencode/opencode.db"
	foreign := store.WatcherCheckpoint{
		Path:     foreignKey,
		Metadata: `{"schema":1,"opencode_time_updated_ms":1787939641519,"opencode_migration_count":38}`,
	}
	// A genuine orphan alongside it — the CONTROL ARM. Without it, a watcher that
	// had simply stopped pruning ANYTHING would pass the assertion below while
	// silently reintroducing the unbounded table growth #71 exists to bound.
	orphanPath := filepath.Join(projectsDir, "gone.jsonl")
	orphan := store.WatcherCheckpoint{
		Path: orphanPath, Inode: 42, Offset: 100, HeadCRC: 7, HeadLen: 20,
		Metadata: `{"SessionID":"sess-gone","NextParseSeq":3}`,
	}

	rec := &recordingStore{seedCheckpoints: []store.WatcherCheckpoint{foreign, orphan}}
	startWatcher(t, claudeDir, []string{repo}, rec)

	// The control: the real orphan IS pruned. This also serialises the assertion
	// below — by the time the watcher has pruned the orphan it has finished the
	// same startup loop that would have pruned the foreign row.
	waitForDeletedCheckpoint(t, rec, orphanPath, 2*time.Second)

	for _, p := range rec.deletedSnapshot() {
		if p == foreignKey {
			t.Fatalf("the watcher pruned %q, which belongs to another collector; its scan watermark is destroyed on every startup", foreignKey)
		}
	}

	// Give any late prune a chance to land before declaring success, so this is
	// not merely a race the watcher happened to lose.
	time.Sleep(100 * time.Millisecond)
	for _, p := range rec.deletedSnapshot() {
		if p == foreignKey {
			t.Fatalf("the watcher pruned %q after a delay", foreignKey)
		}
	}
}

// TestIsFileCheckpointDiscriminatesOwners pins the predicate both halves rely on.
// Table-driven rather than a pair of spot checks because the failure modes are
// opposite and both are silent: too permissive and the watcher eats another
// collector's row, too strict and it skips its own and loses all resume state.
func TestIsFileCheckpointDiscriminatesOwners(t *testing.T) {
	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"/Users/alice/.claude/projects/p1/s1.jsonl", true, "an ordinary watcher row"},
		{"/tmp/s.jsonl", true, "a short absolute path"},
		{"opencode-db:/Users/alice/.local/share/opencode/opencode.db", false, "the Opencode collector's key"},
		{"opencode-db:relative.db", false, "a namespaced key with a relative payload"},
		{"some-future-collector:whatever", false, "a namespace nobody has written yet"},
		{"relative/path.jsonl", false, "a relative path is not a row this watcher ever writes"},
		{"", false, "the empty key"},
	}
	for _, tc := range cases {
		if got := store.IsFileCheckpoint(tc.path); got != tc.want {
			t.Errorf("IsFileCheckpoint(%q) = %v, want %v — %s", tc.path, got, tc.want, tc.why)
		}
	}
}
