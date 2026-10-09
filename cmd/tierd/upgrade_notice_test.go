package main

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestUpgradeNotice_PrintedOnceOnTheV1Migration: a command that opens a
// version-1 database prints the one-way upgrade notice to stderr on that run,
// and not on the next run against the same (now version-2) database (#886).
func TestUpgradeNotice_PrintedOnceOnTheV1Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	run := func() string {
		t.Helper()
		var out, errb bytes.Buffer
		// Only a committed run may migrate the live database.
		if code := runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-a=acme/app", "--commit"}, &out, &errb); code != 0 {
			t.Fatalf("repair-repo commit: rc=%d stderr=%s", code, errb.String())
		}
		return errb.String()
	}
	if first := run(); strings.Count(first, store.UpgradeNoticeV886) != 1 {
		t.Errorf("first run stderr = %q, want the upgrade notice exactly once", first)
	}
	if second := run(); strings.Contains(second, store.UpgradeNoticeV886) {
		t.Errorf("second run stderr = %q, want no upgrade notice", second)
	}
}
