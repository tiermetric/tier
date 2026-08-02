package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openStoreAt opens a store at an explicit path (unlike newTestDB, which hides
// the path) so backup tests can point Backup at the source file.
func openStoreAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedBackupEvents inserts a fixed set of events and returns the source's
// DeveloperCosts totals for a wide window, so a backup can be compared against it.
func seedBackupEvents(t *testing.T, db *DB) []DeveloperCost {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	events := []TokenEvent{
		{Developer: "alice", IssueID: "i1", Model: "claude-sonnet-4", CostMicro: 12_345, Source: "proxy", Fidelity: "realtime", IdempotencyKey: "a1", Timestamp: now},
		{Developer: "alice", IssueID: "i2", Model: "claude-sonnet-4", CostMicro: 5_000, Source: "jsonl", Fidelity: "daily", IdempotencyKey: "a2", Timestamp: now},
		{Developer: "bob", IssueID: "i3", Model: "claude-sonnet-4", CostMicro: 9_999, Source: "proxy", Fidelity: "realtime", IdempotencyKey: "b1", Timestamp: now},
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}
	costs, err := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts(src): %v", err)
	}
	return costs
}

// costsEqual compares two DeveloperCost slices to the micro-dollar, order-
// independent.
func costsEqual(a, b []DeveloperCost) bool {
	if len(a) != len(b) {
		return false
	}
	idx := func(s []DeveloperCost) map[string]DeveloperCost {
		m := make(map[string]DeveloperCost, len(s))
		for _, c := range s {
			m[c.Developer] = c
		}
		return m
	}
	am, bm := idx(a), idx(b)
	for dev, ac := range am {
		bc, ok := bm[dev]
		if !ok || ac.TotalCostMicro != bc.TotalCostMicro || ac.RealtimeCostMicro != bc.RealtimeCostMicro {
			return false
		}
	}
	return true
}

// TestBackup_ConsistentUnderWAL backs up a live WAL-mode store (source still
// open, exercising the concurrent-writer snapshot path) and asserts the backup's
// DeveloperCosts equal the source's to the micro-dollar (#141).
func TestBackup_ConsistentUnderWAL(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src := openStoreAt(t, srcPath)
	want := seedBackupEvents(t, src)

	destPath := filepath.Join(dir, "backup.db")
	// Source deliberately left OPEN during Backup — VACUUM INTO must produce a
	// consistent snapshot even with a live writer holding the DB.
	if err := Backup(context.Background(), srcPath, destPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dest := openStoreAt(t, destPath)
	got, err := dest.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts(dest): %v", err)
	}
	if !costsEqual(want, got) {
		t.Errorf("backup costs = %+v, want %+v", got, want)
	}
}

// TestBackup_RefusesExistingDest pins the refuse-existing guard (#141): a
// pre-existing destination is rejected with a clear message and left untouched.
func TestBackup_RefusesExistingDest(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	openStoreAt(t, srcPath)

	destPath := filepath.Join(dir, "exists.db")
	const sentinel = "do-not-overwrite"
	if err := os.WriteFile(destPath, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("pre-create dest: %v", err)
	}
	err := Backup(context.Background(), srcPath, destPath)
	if err == nil {
		t.Fatal("Backup to an existing dest succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q missing 'already exists'", err.Error())
	}
	got, rerr := os.ReadFile(destPath)
	if rerr != nil {
		t.Fatalf("read dest: %v", rerr)
	}
	if string(got) != sentinel {
		t.Errorf("dest content = %q, want unchanged %q", got, sentinel)
	}
}

// TestBackup_EscapesQuoteInDestPath pins that a destination path containing a
// single quote is escaped, not injected, into VACUUM INTO (#141).
func TestBackup_EscapesQuoteInDestPath(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src := openStoreAt(t, srcPath)
	seedBackupEvents(t, src)

	// A subdirectory + filename both carrying a single quote.
	quotedDir := filepath.Join(dir, "o'brien")
	if err := os.MkdirAll(quotedDir, 0o700); err != nil {
		t.Fatalf("mkdir quoted dir: %v", err)
	}
	destPath := filepath.Join(quotedDir, "a'b.db")
	if err := Backup(context.Background(), srcPath, destPath); err != nil {
		t.Fatalf("Backup to quoted path: %v", err)
	}
	if _, err := os.Stat(destPath); err != nil {
		t.Fatalf("backup not written to quoted path: %v", err)
	}
	// The snapshot must be a usable DB.
	dest := openStoreAt(t, destPath)
	if _, err := dest.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Errorf("open quoted-path backup: %v", err)
	}
}

// TestBackup_FileMode0600 pins that the snapshot is owner-only (#130/#141): a
// world-readable backup would leak per-developer spend. POSIX-only.
func TestBackup_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-mode guarantee is POSIX-only")
	}
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	openStoreAt(t, srcPath)
	destPath := filepath.Join(dir, "backup.db")
	if err := Backup(context.Background(), srcPath, destPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	fi, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %o, want 600", perm)
	}
}
