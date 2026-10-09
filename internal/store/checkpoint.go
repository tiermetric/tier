package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// CheckpointTombstoneMetadata is the metadata an erasure writes over a JSONL
// watcher checkpoint row (#919, ruling R-2026-09-28-9). The row keeps its path
// key, inode, byte offset and head fingerprint — none of them a personal field
// the ruling drops — and its metadata loses the cwd, branch and session id. The
// path key itself still names the working directory (Claude Code encodes it into
// the project folder name) and the session id (the file name). The watcher OBEYS
// the tombstone: it never parses the file at this path again unless a full read
// of the recorded head finds different bytes. See WatcherCheckpoint.Tombstoned.
//
// It is compared byte-for-byte, never parsed: a real watcher blob is a JSON
// object with the sessionMetadata fields, so it can never equal this string.
const CheckpointTombstoneMetadata = `{"erased":true}`

// CheckpointSessionIDKey is the top-level JSON key under which the JSONL
// watcher's metadata blob stores the session id. It is the join an erasure and
// an export use to find a subject's checkpoint rows from their
// token_events.session_id, and the ONLY thing the store reads out of the blob.
// collector's TestCheckpointMetadataCarriesTheStoreSessionKey fails if the
// watcher's blob stops carrying it — that failure is the only thing standing
// between a renamed field and an erasure that silently finds nothing.
const CheckpointSessionIDKey = "SessionID"

// ErrCheckpointTombstoned is returned by SaveWatcherCheckpoint when the row at
// that path is an erasure tombstone and the save was refused.
var ErrCheckpointTombstoned = errors.New("watcher checkpoint is an erasure tombstone")

// WatcherCheckpoint is one persisted tail-state row for the live watcher (#71):
// the byte Offset last parsed for a JSONL file, the Inode + head fingerprint
// (HeadCRC over the first HeadLen bytes) the watcher uses to detect
// rotation/truncation, and an opaque Metadata blob. The store treats Metadata
// as an opaque string (JSON owned by the collector — session id, cwd, branch,
// and the parse-sequence counter that keeps id-less message dedup keys stable
// on resume); it never parses it.
type WatcherCheckpoint struct {
	Path     string
	Inode    uint64
	Offset   int64
	HeadCRC  uint32
	HeadLen  int
	Metadata string
}

// Tombstoned reports whether the row is an erasure tombstone (#919). A consumer
// must test this BEFORE decoding Metadata: the tombstone decodes into a
// zero-valued session struct without error.
func (cp WatcherCheckpoint) Tombstoned() bool { return cp.Metadata == CheckpointTombstoneMetadata }

// IsFileCheckpoint reports whether a watcher_checkpoint row's Path names a
// FILESYSTEM PATH — i.e. whether the row belongs to the JSONL watcher, whose
// rows are absolute paths of tailed session files.
//
// 🔴 THE TABLE IS SHARED, AND ITS ORIGINAL CONSUMER DID NOT KNOW THAT. Every row
// was a JSONL file path until #719, when the Opencode collector began storing a
// database-scan watermark here under the namespaced key `opencode-db:<path>`.
// The watcher loads EVERY row at startup and PRUNES any whose path does not stat
// — correct garbage collection for a session file deleted while it was down, and
// silent destruction of another collector's row, since a namespaced key never
// stats. The two are indistinguishable to os.Stat; only the shape of the key can
// tell them apart.
//
// So a consumer of LoadWatcherCheckpoints must filter with this before treating a
// row as its own — and, just as importantly, before deleting one.
//
// The rule is deliberately structural rather than a list of known prefixes: a
// namespaced key is `<owner>:<...>`, which filepath.IsAbs rejects on every
// platform this builds for (on Windows a volume name is a single letter, so
// `opencode-db:` is not one), while a real absolute path is accepted on all of
// them. A future collector that shares this table needs no edit here, only a
// non-absolute key.
func IsFileCheckpoint(path string) bool { return filepath.IsAbs(path) }

// LoadWatcherCheckpoints returns every persisted watcher checkpoint, INCLUDING
// rows owned by collectors other than the JSONL watcher — see IsFileCheckpoint.
// Called once at watcher startup to seed the in-memory tail-state map so the
// first post-restart write to a file resumes from its offset instead of byte 0.
func (d *DB) LoadWatcherCheckpoints(ctx context.Context) ([]WatcherCheckpoint, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT path, inode, byte_offset, head_crc, head_len, metadata FROM watcher_checkpoint`)
	if err != nil {
		return nil, fmt.Errorf("load watcher checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WatcherCheckpoint
	for rows.Next() {
		var cp WatcherCheckpoint
		// inode/head_crc are stored in signed INTEGER columns (SQLite has no
		// unsigned type) and scanned back through the matching width. A real
		// inode number and a CRC32 both fit, so the round-trip is lossless.
		var inode, headCRC int64
		if err := rows.Scan(&cp.Path, &inode, &cp.Offset, &headCRC, &cp.HeadLen, &cp.Metadata); err != nil {
			return nil, fmt.Errorf("scan watcher checkpoint: %w", err)
		}
		cp.Inode = uint64(inode)
		cp.HeadCRC = uint32(headCRC)
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watcher checkpoints: %w", err)
	}
	return out, nil
}

// LoadWatcherCheckpoint returns the single checkpoint stored under key, and
// whether it existed.
//
// It exists so a collector that owns exactly ONE row does not have to enumerate
// the whole table and pick its own out of it (#719). That pattern is not merely
// wasteful — it is the shape that produced the defect IsFileCheckpoint above
// exists to prevent: a consumer holding every other consumer's rows in its hand
// is one missing filter away from acting on one. Enumeration belongs to the JSONL
// watcher, which genuinely seeds a map of all its files; everyone else asks for
// their key.
func (d *DB) LoadWatcherCheckpoint(ctx context.Context, key string) (WatcherCheckpoint, bool, error) {
	var cp WatcherCheckpoint
	var inode, headCRC int64
	err := d.db.QueryRowContext(ctx,
		`SELECT path, inode, byte_offset, head_crc, head_len, metadata FROM watcher_checkpoint WHERE path = ?`,
		key,
	).Scan(&cp.Path, &inode, &cp.Offset, &headCRC, &cp.HeadLen, &cp.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return WatcherCheckpoint{}, false, nil
	}
	if err != nil {
		return WatcherCheckpoint{}, false, fmt.Errorf("load watcher checkpoint %q: %w", key, err)
	}
	cp.Inode = uint64(inode)
	cp.HeadCRC = uint32(headCRC)
	return cp, true, nil
}

// SaveWatcherCheckpoint upserts one file's tail state — called after each
// successful incremental parse. The UPSERT on the path primary key keeps exactly
// one row per file, always reflecting the latest parsed offset.
//
// 🔴 IT NEVER OVERWRITES AN ERASURE TOMBSTONE, AND THE GUARD IS IN THE SQL (#919).
// The watcher and EraseDeveloper run in one process on one database: a debounce
// that checked the row, found no tombstone, and parsed a chunk can reach this
// save AFTER an erase has tombstoned the row. Overwriting the marker would put
// the subject's cwd, branch and session id back and, worse, un-mark the file, so
// the next restart would resume capture of an erased session. An in-memory check
// cannot close that window; the DO UPDATE ... WHERE does, because it is evaluated
// under SQLite's write lock against the committed row. A refused save returns
// ErrCheckpointTombstoned and changes nothing.
func (d *DB) SaveWatcherCheckpoint(ctx context.Context, cp WatcherCheckpoint) error {
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO watcher_checkpoint (path, inode, byte_offset, head_crc, head_len, metadata, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(path) DO UPDATE SET
			inode       = excluded.inode,
			byte_offset = excluded.byte_offset,
			head_crc    = excluded.head_crc,
			head_len    = excluded.head_len,
			metadata    = excluded.metadata,
			updated_at  = excluded.updated_at
		WHERE watcher_checkpoint.metadata <> ?`,
		cp.Path, int64(cp.Inode), cp.Offset, int64(cp.HeadCRC), cp.HeadLen, cp.Metadata,
		CheckpointTombstoneMetadata,
	)
	if err != nil {
		return fmt.Errorf("save watcher checkpoint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("save watcher checkpoint: rows affected: %w", err)
	}
	if n == 0 {
		return ErrCheckpointTombstoned
	}
	return nil
}

// DeleteWatcherCheckpoint removes a file's tail state — called when the watcher
// drops a path (file removed or renamed away, or a parse it abandons) so a stale
// checkpoint can't be loaded for an unrelated future file at the same path. A
// delete of an absent path is a no-op.
//
// 🔴 AN ERASURE TOMBSTONE IS DELETED ONLY WHEN THE FILE IS GONE (#919). The
// watcher also calls this after an abandoned parse (a cancelled insert, a parse
// error) while the file still exists; if an erase tombstoned the row in the
// meantime, deleting it would un-mark the file and the next parse would read it
// from byte 0 — re-inserting the erased events. So a tombstone survives this call
// unless os.Stat reports the path does not exist, which is the ruling's "a row
// whose session file is gone is deleted". To drop a tombstone for a file that
// still exists — a new file at the same path — use DeleteWatcherTombstone.
func (d *DB) DeleteWatcherCheckpoint(ctx context.Context, path string) error {
	q := `DELETE FROM watcher_checkpoint WHERE path = ? AND metadata <> ?`
	args := []any{path, CheckpointTombstoneMetadata}
	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		q, args = `DELETE FROM watcher_checkpoint WHERE path = ?`, []any{path}
	}
	if _, err := d.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("delete watcher checkpoint: %w", err)
	}
	return nil
}

// DeleteWatcherTombstone removes the erasure tombstone at path, and nothing else
// — the watcher calls it when the file now at path is NOT the file the tombstone
// describes (a different inode or head: a new session), so capture of the new
// file can resume and its checkpoint can be saved (#919). A non-tombstone row is
// left alone.
func (d *DB) DeleteWatcherTombstone(ctx context.Context, path string) error {
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM watcher_checkpoint WHERE path = ? AND metadata = ?`,
		path, CheckpointTombstoneMetadata); err != nil {
		return fmt.Errorf("delete watcher tombstone: %w", err)
	}
	return nil
}

// subjectCheckpointPredicate is the WHERE clause selecting the JSONL watcher
// checkpoint rows that belong to a data subject: those whose metadata session id
// is one of the subject's token_events.session_id values (#919). placeholders is
// inClause's `?,?,…` for the subject's identifier set, bound by the caller.
//
// It is the one definition EraseDeveloper and ExportDeveloper share, so the rows
// an export discloses are exactly the rows an erasure clears. ⚠️ It can only find
// a row whose session left at least one token_events row: a session that was
// tailed but never produced a stored event has no join and is not found — the
// "unjoinable residual" docs/privacy.md names.
func subjectCheckpointPredicate(placeholders string) string {
	return `json_valid(metadata) AND json_extract(metadata, '$.` + CheckpointSessionIDKey + `') IN (
		SELECT te.session_id FROM token_events te
		 WHERE te.developer IN (` + placeholders + `)
		   AND te.session_id IS NOT NULL AND te.session_id <> '')`
}

// eraseSubjectCheckpoints tombstones or deletes the subject's watcher checkpoint
// rows inside EraseDeveloper's transaction (#919, R-2026-09-28-9), and returns
// how many rows it changed.
//
// 🔴 IT MUST RUN BEFORE token_events IS DELETED: the rows are found through the
// subject's token_events.session_id, so after that DELETE there is nothing left
// to join on and the rows are unreachable for good.
//
// Per row: if the session file no longer exists the row is deleted outright;
// otherwise its metadata becomes CheckpointTombstoneMetadata (path, inode, offset
// and head fingerprint kept) so the running watcher and every later start skip
// the file. A stat error other than not-exist tombstones — the direction that
// keeps the file un-captured. Only filesystem-path rows are touched
// (IsFileCheckpoint); another collector's namespaced row is never this method's.
func eraseSubjectCheckpoints(ctx context.Context, tx *sql.Tx, placeholders string, args []any) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path FROM watcher_checkpoint WHERE `+subjectCheckpointPredicate(placeholders), args...)
	if err != nil {
		return 0, fmt.Errorf("find subject watcher checkpoints: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan subject watcher checkpoint: %w", err)
		}
		if IsFileCheckpoint(p) {
			paths = append(paths, p)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate subject watcher checkpoints: %w", err)
	}
	_ = rows.Close()

	var changed int64
	for _, p := range paths {
		q := `UPDATE watcher_checkpoint SET metadata = ?, updated_at = CURRENT_TIMESTAMP WHERE path = ?`
		qargs := []any{CheckpointTombstoneMetadata, p}
		if _, statErr := os.Stat(p); errors.Is(statErr, fs.ErrNotExist) {
			q, qargs = `DELETE FROM watcher_checkpoint WHERE path = ?`, []any{p}
		}
		res, err := tx.ExecContext(ctx, q, qargs...)
		if err != nil {
			return 0, fmt.Errorf("erase watcher checkpoint: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("erase watcher checkpoint: rows affected: %w", err)
		}
		changed += n
	}
	return changed, nil
}
