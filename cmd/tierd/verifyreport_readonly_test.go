package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
)

func TestReadOnlySnapshotDSN_NoCreateOrWrite(t *testing.T) {
	for _, name := range []string{"snapshot.db", "snapshot ?#%.db"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			ro, err := sql.Open("sqlite", readOnlySnapshotDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ro.Close() }()
			if err := ro.Ping(); err == nil {
				t.Error("read-only open of a missing database succeeded")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read-only open created the missing database (stat: %v)", err)
			}
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ro.Ping(); err != nil {
				t.Fatal(err)
			}
			_, err = ro.Exec(`CREATE TABLE read_only_probe (id INTEGER)`)
			var sqliteErr *sqlite.Error
			if !errors.As(err, &sqliteErr) || sqliteErr.Code() != 8 {
				t.Fatalf("write through snapshot handle: %v; want SQLITE_READONLY (8)", err)
			}
		})
	}
}
