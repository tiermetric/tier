package store

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyURI(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{"space name.db", "space%20name.db"},
		{"question?.db", "question%3F.db"},
		{"hash#.db", "hash%23.db"},
		{"percent%.db", "percent%25.db"},
		{"日本語.db", "%E6%97%A5%E6%9C%AC%E8%AA%9E.db"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, query := range []string{
				"mode=ro",
				"mode=ro&_pragma=busy_timeout(5000)",
				"mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)",
			} {
				path := filepath.FromSlash("C:/data/" + tt.name)
				want := "file:///C:/data/" + tt.want + "?" + query
				if got := ReadOnlyURI(path, query); got != want {
					t.Errorf("ReadOnlyURI(%q, %q) = %q; want %q", path, query, got, want)
				}
			}
		})
	}
}

// TestOpenReadOnly_NoCreateNoCheckpoint pins mode=ro, which query_only alone
// does not give: a missing path is refused and never created, and closing the
// only connection to a crash-left WAL leaves the main file's bytes unchanged.
func TestOpenReadOnly_NoCreateNoCheckpoint(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if db, err := OpenReadOnly(missing); err == nil {
		_ = db.Close()
		t.Error("OpenReadOnly of a missing path succeeded; want it refused")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenReadOnly created the missing path (stat: %v)", err)
	}

	src := filepath.Join(dir, "src.db")
	w, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	for _, stmt := range []string{`PRAGMA wal_checkpoint(TRUNCATE)`, `CREATE TABLE ro_probe (x)`, `INSERT INTO ro_probe VALUES (1)`} {
		if _, err := w.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	crash := filepath.Join(dir, "crash.db")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if suffix == "-wal" && len(b) == 0 {
			t.Fatal("fixture: the writer's -wal is empty, so there is nothing to fold")
		}
		if err := os.WriteFile(crash+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(crash)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnly(crash)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM ro_probe`).Scan(&n); err != nil || n != 1 {
		t.Errorf("read of the WAL's row: n %d, err %v; want 1", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(crash); err != nil || !bytes.Equal(before, after) {
		t.Errorf("closing the read-only open changed the main file (err %v): the -wal was folded into it", err)
	}
}
