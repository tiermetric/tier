package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

// TestBackup_ConsistentUnderWAL checks stable costs and atomic row pairs while
// a concurrent writer inserts into a WAL-mode source (#141).
func TestBackup_ConsistentUnderWAL(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src := openStoreAt(t, srcPath)
	want := seedBackupEvents(t, src)
	if _, err := src.db.Exec(`CREATE TABLE receipt (value INTEGER, padding BLOB);
		INSERT INTO receipt VALUES (1, zeroblob(4194304)), (-1, zeroblob(4194304))`); err != nil {
		t.Fatal(err)
	}
	stop, started := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var writes atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := src.db.Exec(`INSERT INTO receipt (value) VALUES (1), (-1)`); err != nil {
				done <- err
				return
			}
			if writes.Add(1) == 1 {
				close(started)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		if err := <-done; err != nil {
			t.Errorf("concurrent writer: %v", err)
		}
	})
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("start writer: %v", err)
	}

	destPath := filepath.Join(dir, "backup.db")
	before := writes.Load()
	if err := Backup(context.Background(), srcPath, destPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if writes.Load() <= before {
		t.Fatal("writer made no progress during Backup")
	}

	dest := openStoreAt(t, destPath)
	var integrity string
	if err := dest.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("backup integrity = %q, error = %v", integrity, err)
	}
	var count, sum int
	if err := dest.db.QueryRow(`SELECT count(*), sum(value) FROM receipt`).Scan(&count, &sum); err != nil {
		t.Fatal(err)
	}
	if count < 4 || count%2 != 0 || sum != 0 {
		t.Errorf("inconsistent row pairs: count=%d, sum=%d", count, sum)
	}
	got, err := dest.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts(dest): %v", err)
	}
	if !costsEqual(want, got) {
		t.Errorf("backup costs = %+v, want %+v", got, want)
	}
}

func TestBackup_PreservesUncheckpointedSource(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "live.db")
	live, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	live.SetMaxOpenConns(1)
	if _, err := live.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
		CREATE TABLE receipt (value TEXT); INSERT INTO receipt VALUES ('in WAL')`); err != nil {
		t.Fatal(err)
	}

	// Copy while the writer is open so the source has committed, uncheckpointed pages.
	srcPath := filepath.Join(dir, "src.db")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(livePath + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("empty fixture file %s", livePath+suffix)
		}
		if err := os.WriteFile(srcPath+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(dir, "backup.db")
	if err := Backup(context.Background(), srcPath, destPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("source main file changed: %d bytes before, %d after", len(before), len(after))
	}
	if _, err := os.Stat(srcPath + "-wal"); err != nil {
		t.Errorf("source WAL must still exist after Backup: %v", err)
	}
	dest, err := sql.Open("sqlite", destPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dest.Close() })
	var value string
	if err := dest.QueryRow(`SELECT value FROM receipt`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "in WAL" {
		t.Errorf("backup value = %q, want in WAL", value)
	}
}

func TestBackup_RefusesEmptySource(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "empty.db")
	destPath := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(srcPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Backup(context.Background(), srcPath, destPath)
	want := fmt.Sprintf("backup source %s is empty; nothing was backed up", srcPath)
	if err == nil || err.Error() != want {
		t.Errorf("Backup error = %v, want %q", err, want)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Errorf("destination must not exist, stat error = %v", err)
	}
	fi, err := os.Stat(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("source size = %d, want 0", fi.Size())
	}
}

func TestBackup_RemovesPartialOnVacuumFailure(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	if _, err := src.Exec(`CREATE TABLE receipt (value TEXT); INSERT INTO receipt VALUES ('private spend')`); err != nil {
		t.Fatal(err)
	}
	var root, pageSize int
	if err := src.QueryRow(`SELECT rootpage FROM sqlite_schema WHERE name = 'receipt'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := src.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	// An invalid table page leaves the schema readable but fails the VACUUM copy.
	data[(root-1)*pageSize] = 0xff
	if err := os.WriteFile(srcPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(dir, "backup.db")
	err = Backup(context.Background(), srcPath, destPath)
	if err == nil || !strings.Contains(err.Error(), "vacuum into") || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("Backup error = %v, want malformed database during VACUUM", err)
	}
	if fi, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("partial destination remains: info=%v, stat error=%v", fi, err)
	}
}

func TestBackup_MissingSourceBeforeBadDestination(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "missing.db")
	for _, destPath := range []string{"", dir, filepath.Join(dir, "missing", "backup.db")} {
		err := Backup(context.Background(), srcPath, destPath)
		if err == nil || !strings.Contains(err.Error(), "backup source "+srcPath+" does not exist") {
			t.Errorf("Backup to %q: %v, want missing source", destPath, err)
		}
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

// TestBackup_RefusesMissingSource pins the S02-1 refusal: a --db path that does
// not exist is an error, not an implicit snapshot of a brand-new empty database.
// Backup must also not CREATE the source at that path (SQLite's default open
// would), so the path must still be missing after the call.
func TestBackup_RefusesMissingSource(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "typo.db")
	destPath := filepath.Join(dir, "backup.db")

	err := Backup(context.Background(), srcPath, destPath)
	if err == nil {
		t.Fatal("Backup of a missing source succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q missing 'does not exist'", err.Error())
	}
	if _, statErr := os.Stat(srcPath); !os.IsNotExist(statErr) {
		t.Errorf("source %s must not exist after Backup, stat error = %v", srcPath, statErr)
	}
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Errorf("dest %s must not be written for a missing source, stat error = %v", destPath, statErr)
	}
}

// TestBackup_RefusesNonRegularSource pins the other half of the S02-1 source
// guard: a --db path that exists but is not a regular file (e.g. a directory)
// is refused with a clear error rather than vacuumed into a nonsense snapshot.
func TestBackup_RefusesNonRegularSource(t *testing.T) {
	dir := t.TempDir()
	err := Backup(context.Background(), dir, filepath.Join(dir, "backup.db"))
	if err == nil {
		t.Fatal("Backup of a directory-as-source succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error %q missing 'not a regular file'", err.Error())
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
