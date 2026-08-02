package store

import (
	"context"
	"fmt"
)

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

// LoadWatcherCheckpoints returns every persisted watcher checkpoint. Called once
// at watcher startup to seed the in-memory tail-state map so the first
// post-restart write to a file resumes from its offset instead of byte 0.
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

// SaveWatcherCheckpoint upserts one file's tail state — called after each
// successful incremental parse. The UPSERT on the path primary key keeps exactly
// one row per file, always reflecting the latest parsed offset.
func (d *DB) SaveWatcherCheckpoint(ctx context.Context, cp WatcherCheckpoint) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO watcher_checkpoint (path, inode, byte_offset, head_crc, head_len, metadata, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(path) DO UPDATE SET
			inode       = excluded.inode,
			byte_offset = excluded.byte_offset,
			head_crc    = excluded.head_crc,
			head_len    = excluded.head_len,
			metadata    = excluded.metadata,
			updated_at  = excluded.updated_at`,
		cp.Path, int64(cp.Inode), cp.Offset, int64(cp.HeadCRC), cp.HeadLen, cp.Metadata,
	)
	if err != nil {
		return fmt.Errorf("save watcher checkpoint: %w", err)
	}
	return nil
}

// DeleteWatcherCheckpoint removes a file's tail state — called when the watcher
// drops a path (file removed or renamed away) so a stale checkpoint can't be
// loaded for an unrelated future file at the same path. A delete of an absent
// path is a no-op.
func (d *DB) DeleteWatcherCheckpoint(ctx context.Context, path string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM watcher_checkpoint WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("delete watcher checkpoint: %w", err)
	}
	return nil
}
