package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// createEmptyStore creates and migrates a fresh store DB at path (so a cmd-level
// backup test has a real, schema-stamped source to snapshot).
func createEmptyStore(t *testing.T, path string) error {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
}

// writeFile is a thin os.WriteFile wrapper for the existing-dest test.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// TestDispatch_BackupRequiresOut pins that `tierd backup` with no --out is a
// usage error (exit 1, message on stderr, stdout clean) (#141).
func TestDispatch_BackupRequiresOut(t *testing.T) {
	var out, errOut bytes.Buffer
	code := dispatch([]string{"backup"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "--out is required") {
		t.Errorf("stderr = %q, want '--out is required'", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestDispatch_BackupWritesSnapshot exercises the happy path end-to-end: a fresh
// DB is created, then backed up to a new destination, and dispatch reports the
// write on stdout with exit 0 (#141).
func TestDispatch_BackupWritesSnapshot(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "src.db")
	if err := createEmptyStore(t, dbPath); err != nil {
		t.Fatalf("seed source db: %v", err)
	}
	destPath := filepath.Join(dir, "backup.db")

	var out, errOut bytes.Buffer
	code := dispatch([]string{"backup", "--db", dbPath, "--out", destPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "backup written:") || !strings.Contains(out.String(), destPath) {
		t.Errorf("stdout = %q, want 'backup written: %s'", out.String(), destPath)
	}
}

// TestDispatch_BackupRefusesExistingDest pins the belt-and-braces refusal at the
// dispatch layer: an existing --out yields exit 1 (#141).
func TestDispatch_BackupRefusesExistingDest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "src.db")
	if err := createEmptyStore(t, dbPath); err != nil {
		t.Fatalf("seed source db: %v", err)
	}
	destPath := filepath.Join(dir, "exists.db")
	if err := writeFile(destPath, "x"); err != nil {
		t.Fatalf("pre-create dest: %v", err)
	}
	var out, errOut bytes.Buffer
	code := dispatch([]string{"backup", "--db", dbPath, "--out", destPath}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", errOut.String())
	}
}
