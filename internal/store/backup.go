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
	"strings"

	_ "modernc.org/sqlite"
)

// Backup writes a transactionally consistent snapshot of the SQLite database at
// dbPath to destPath via `VACUUM INTO`.
//
// It opens its OWN minimal connection and runs NO migrations: a backup must never
// mutate the source (not even to stamp a schema version) and must work regardless
// of the source's schema version — including a version NEWER than this binary
// understands, which store.Open would refuse. busy_timeout rides in the DSN so a
// concurrently-held write lock is waited on rather than failing immediately;
// journal_mode is deliberately NOT forced, so the source is read exactly as it
// sits on disk.
//
// destPath must not already exist. This is checked twice: an os.Stat pre-check
// for a clear operator-facing message, and VACUUM INTO's own "output file already
// exists" refusal as a belt-and-braces second layer (it closes the tiny TOCTOU
// window between the Stat and the VACUUM). The snapshot is chmod'd to 0600 on
// success: the live DB files are 0600 (#130) because they hold per-developer
// spend and org invoice totals, and a world-readable backup would silently undo
// that. VACUUM INTO owns file creation (it refuses a pre-existing path), so the
// file exists as 0644 for the brief span between creation and this chmod; the
// mode guarantee, like the rest of #130, is POSIX-only.
//
// The caller is responsible for choosing a destPath outside any world-readable
// directory if the transient-creation window matters in their threat model.
func Backup(ctx context.Context, dbPath, destPath string) error {
	if destPath == "" {
		return errors.New("backup destination path is required")
	}
	// Clear, operator-facing refusal BEFORE touching SQLite. VACUUM INTO also
	// refuses an existing file, but its message ("output file already exists") is
	// less actionable, so we lead with this one and keep VACUUM INTO's as backstop.
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("backup destination %s already exists — refusing to overwrite", destPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A Stat error other than not-exist (e.g. a permission problem on the
		// parent directory) is itself a reason to stop — fail loud rather than
		// blunder into VACUUM INTO.
		return fmt.Errorf("stat backup destination %s: %w", destPath, err)
	}

	// Same busy_timeout DSN pragma as Open, but WITHOUT journal_mode: read the
	// source exactly as it is. A read-only intent is expressed by running only
	// VACUUM INTO; we do not open with mode=ro because VACUUM needs to allocate
	// its own temp state, and forcing read-only can reject on some builds.
	dsn := dbPath + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect source db: %w", err)
	}

	// VACUUM INTO has no placeholder support for its target, so the path is
	// spliced as a single-quoted SQL string literal. Double any embedded single
	// quote — the only metacharacter that matters inside a SQLite string literal —
	// so a path like /tmp/o'brien/backup.db is escaped rather than injected.
	escaped := strings.ReplaceAll(destPath, "'", "''")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`VACUUM INTO '%s'`, escaped)); err != nil {
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}

	// Tighten to owner-only (#130). VACUUM INTO created the file honoring the
	// process umask (typically 0644); chmod is the authoritative guarantee that
	// the snapshot carries the same 0600 confidentiality as the live DB. On
	// Windows os.Chmod only clears the read-only bit and the mode guarantee does
	// not apply, exactly as documented on Open.
	//
	// If the chmod fails we must NOT leave the just-written 0644 snapshot on disk:
	// it holds full per-developer spend and org invoice totals, so a "failed"
	// backup that left a world-readable copy is the worst outcome (the chmod IS the
	// confidentiality guarantee). Remove it before returning the error; the removal
	// is best-effort and its own failure is joined into the error so nothing is
	// swallowed.
	if err := os.Chmod(destPath, 0o600); err != nil {
		if rmErr := os.Remove(destPath); rmErr != nil {
			return fmt.Errorf("restrict backup permissions %s: %w (and failed to remove the world-readable partial: %v)", destPath, err, rmErr)
		}
		return fmt.Errorf("restrict backup permissions %s: %w (removed the world-readable snapshot)", destPath, err)
	}
	return nil
}
