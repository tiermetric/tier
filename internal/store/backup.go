package store

// backup.go implements `store.Backup`, the consistent-snapshot primitive behind
// the `tierd backup` subcommand (#141).
//
// Why not `cp`: the database runs in WAL mode, so at any instant committed data
// lives partly in tier.db and partly in tier.db-wal, and a checkpoint may be
// rewriting pages. A file-copy of tier.db alone loses everything still in the WAL
// and can capture a torn page mid-checkpoint. SQLite's `VACUUM INTO` instead
// writes a transactionally consistent, fully-checkpointed copy of the live
// database — safe to run WHILE a writer is active, which is the whole point.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Backup writes a transactionally consistent snapshot of the SQLite database at
// dbPath to destPath via `VACUUM INTO`.
//
// It opens its OWN minimal connection and runs NO migrations: a backup must never
// mutate the source database file (not even to stamp a schema version) and must
// work regardless of the source's schema version — including a version NEWER
// than this binary understands, which store.Open would refuse. busy_timeout
// rides in the DSN so a concurrently-held write lock is waited on rather than
// failing immediately; journal_mode is deliberately NOT forced.
//
// destPath must not already exist. Exclusive creation reserves an empty file
// before VACUUM INTO so failure cleanup only removes a destination we created.
// The file is owner-only from creation because backups contain per-developer
// spend and org invoice totals (#130). The mode guarantee is POSIX-only.
//
// dbPath must name a non-empty regular file. The read-only open (mode=ro)
// never creates or writes the database file, even if it disappears between
// Stat and open, and never checkpoints a leftover WAL into it (audit S02-1).
// A cleanly closed WAL database may acquire empty -wal/-shm sidecars.
func Backup(ctx context.Context, dbPath, destPath string) error {
	// Clear, operator-facing refusal of a missing or non-regular SOURCE before
	// anything is opened (audit S02-1): the bare-path DSN this function used to
	// build opens create-if-missing, so a typo in --db silently snapshotted a
	// brand-new empty database, printed "backup written … (4096 bytes)", exited
	// 0, and left that empty file behind — a useless backup reporting success
	// right where the README tells operators to run one before destructive
	// steps. A source that exists but is not a regular file (a directory, a
	// device) is refused the same way: there is no database there to snapshot.
	if fi, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("backup source %s does not exist; nothing was backed up", dbPath)
		}
		// A Stat error other than not-exist (e.g. a permission problem on a
		// parent directory) is itself a reason to stop — fail loud.
		return fmt.Errorf("stat backup source %s: %w", dbPath, err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("backup source %s is not a regular file; nothing was backed up", dbPath)
	} else if fi.Size() == 0 {
		return fmt.Errorf("backup source %s is empty; nothing was backed up", dbPath)
	}

	if destPath == "" {
		return errors.New("backup destination path is required")
	}
	// The pre-check gives a clear error; exclusive creation below closes the race.
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("backup destination %s already exists — refusing to overwrite", destPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A Stat error other than not-exist (e.g. a permission problem on the
		// parent directory) is itself a reason to stop — fail loud rather than
		// blunder into VACUUM INTO.
		return fmt.Errorf("stat backup destination %s: %w", destPath, err)
	}

	// The file: URI is required for mode=ro: modernc.org/sqlite opens a bare
	// path read-write even with ?mode=ro (see OpenReadOnly). VACUUM INTO works
	// with a read-only source; closing a read-write connection can checkpoint
	// its WAL into the source database file and remove the WAL.
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	dsn := ReadOnlyURI(abs, fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)", dsnBusyTimeoutMS))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()
	// 🔴 DELIBERATE, AND UNRELATED TO THE STORE'S maxOpenConns. Since #669 this is
	// the only literal 1 left in non-test code, so it reads like a site the pool
	// sweep missed. It is not: this is a SEPARATE handle with its own DSN, opened
	// for exactly one VACUUM INTO, which can never have more than one statement in
	// flight. A pool here would add connections nothing uses. Do not "align" it.
	//
	// ⚠️ Worth knowing at the other end: VACUUM INTO holds a whole-database READ
	// snapshot for its duration, which makes it the archetypal WAL-checkpoint
	// pinner. A `tierd backup` running against a busy `tierd serve` is an expected
	// transient spike in tier_sqlite_wal_bytes, not a starved WAL (#669).
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect source db: %w", err)
	}

	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup destination %s: %w", destPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close backup destination %s: %w", destPath, errors.Join(err, os.Remove(destPath)))
	}

	// VACUUM INTO has no placeholder support for its target, so the path is
	// spliced as a single-quoted SQL string literal. Double any embedded single
	// quote — the only metacharacter that matters inside a SQLite string literal —
	// so a path like /tmp/o'brien/backup.db is escaped rather than injected.
	escaped := strings.ReplaceAll(destPath, "'", "''")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`VACUUM INTO '%s'`, escaped)); err != nil {
		if rmErr := os.Remove(destPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("vacuum into %s: %w (failed to remove partial backup: %v)", destPath, err, rmErr)
		}
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}

	// Enforce owner-only permissions even if the process umask removed owner bits.
	if err := os.Chmod(destPath, 0o600); err != nil {
		if rmErr := os.Remove(destPath); rmErr != nil {
			return fmt.Errorf("restrict backup permissions %s: %w (and failed to remove the partial backup: %v)", destPath, err, rmErr)
		}
		return fmt.Errorf("restrict backup permissions %s: %w (removed the snapshot)", destPath, err)
	}
	return nil
}
