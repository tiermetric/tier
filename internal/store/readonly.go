package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// ErrSchemaMismatch is OpenReadOnly's refusal of a database stored at a schema
// version other than this binary's.
var ErrSchemaMismatch = errors.New("schema version mismatch")

// ReadOnlyURI encodes an absolute filesystem path and a raw read-only query as a file: URI.
func ReadOnlyURI(path, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: query}
	return u.String()
}

// OpenReadOnly opens the existing SQLite database at path for reading only
// (#913-D8 ruling C, condition 6): no migration, no webhook payload prune, no
// chmod, and every write refused. It never creates or writes the database file
// itself, and never folds a leftover -wal into it. On a cleanly closed WAL
// database SQLite creates the empty -wal and -shm files it needs to read and
// leaves them, so the open fails where the directory is not writable. It refuses, with
// ErrSchemaMismatch, a database stored at any schema version but this binary's,
// since reading an older shape would need the migration this open never runs.
//
// The DSN is a file: URI on purpose. modernc.org/sqlite cuts a bare path's
// query off before the open, so a bare `path?mode=ro` opens read-write and
// creates a missing file (measured, v1.48.0); only the URI form reaches
// SQLite's mode=ro.
func OpenReadOnly(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	dsn := ReadOnlyURI(abs, fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(1)", dsnBusyTimeoutMS))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	var stored int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&stored); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	if stored != schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("%w: database %s has schema version %d and this binary reads only %d; "+
			"run the tierd release that serves it", ErrSchemaMismatch, path, stored, schemaVersion)
	}
	return &DB{db: db}, nil
}
