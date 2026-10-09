package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests for #862: a DELETE must not leave the deleted bytes readable in the
// database file.

// TestOpenEnablesSecureDeleteOnEveryPooledConnection pins that secure_delete and
// journal_size_limit reach EVERY connection, not only the first one Open dials.
// It holds all maxOpenConns connections at once so each is a distinct pooled
// connection.
func TestOpenEnablesSecureDeleteOnEveryPooledConnection(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "sd.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	conns := make([]*sql.Conn, 0, maxOpenConns)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxOpenConns; i++ {
		c, err := db.db.Conn(ctx)
		if err != nil {
			t.Fatalf("reserve conn %d: %v", i, err)
		}
		conns = append(conns, c)
		var v int
		if err := c.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&v); err != nil {
			t.Fatalf("conn %d: read secure_delete: %v", i, err)
		}
		if v != 1 {
			t.Errorf("conn %d: PRAGMA secure_delete = %d, want 1", i, v)
		}
		var limit int64
		if err := c.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil {
			t.Fatalf("conn %d: read journal_size_limit: %v", i, err)
		}
		if limit != dsnJournalSizeLimit {
			t.Errorf("conn %d: PRAGMA journal_size_limit = %d, want %d", i, limit, dsnJournalSizeLimit)
		}
	}
}

// checkpointTruncate folds the WAL into the main file and empties the WAL, so
// the main file alone holds every committed page.
func checkpointTruncate(t *testing.T, db *DB) {
	t.Helper()
	var busy, logFrames, done int
	if err := db.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
		t.Fatalf("wal_checkpoint(TRUNCATE): %v", err)
	}
	if busy != 0 {
		t.Fatalf("wal_checkpoint(TRUNCATE) reported busy=%d; the file read below would not be complete", busy)
	}
}

// fileContains reports whether the main database file holds needle.
func fileContains(t *testing.T, path string, needle []byte) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	return bytes.Contains(raw, needle)
}

func uniqueMarker(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// TestDeletedRowBytesAreGoneFromTheFile pins that the bytes of a deleted row are
// absent from the main database file after a checkpoint, for both delete paths
// a privacy claim rests on. Each arm first asserts the marker IS in the file
// before the delete, so an absent marker afterwards cannot be a search that
// never could have matched.
func TestDeletedRowBytesAreGoneFromTheFile(t *testing.T) {
	t.Run("PruneWebhookPayloads", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "prune.db")
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		ctx := context.Background()

		// delivery_id holds the marker in a b-tree cell. The random body gzips to
		// an incompressible blob larger than a page, so a window of it sits on an
		// overflow page, which a delete returns to the freelist.
		marker := uniqueMarker(t, "prune-marker-")
		body := make([]byte, 16<<10)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		gz, err := gzipBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		blobWindow := gz[len(gz)/2 : len(gz)/2+64]

		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256, received_at)
			VALUES ('push', ?, ?, 'x', datetime('now','-91 days'))`, marker, gz); err != nil {
			t.Fatalf("insert stale row: %v", err)
		}
		// A fresh row keeps the table non-empty, so the pages are not simply
		// dropped wholesale.
		if err := db.InsertWebhookPayload(ctx, "push", "fresh", []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("insert fresh row: %v", err)
		}

		checkpointTruncate(t, db)
		if !fileContains(t, path, []byte(marker)) || !fileContains(t, path, blobWindow) {
			t.Fatal("control: marker or blob window not in the file before the delete; the search below proves nothing")
		}

		n, err := db.PruneWebhookPayloads(ctx)
		if err != nil {
			t.Fatalf("PruneWebhookPayloads: %v", err)
		}
		if n != 1 {
			t.Fatalf("pruned %d rows, want 1", n)
		}
		checkpointTruncate(t, db)
		if fileContains(t, path, []byte(marker)) {
			t.Error("pruned row's delivery_id is still readable in the db file (secure_delete off?)")
		}
		if fileContains(t, path, blobWindow) {
			t.Error("pruned row's body_gz is still readable in the db file's free pages (secure_delete off, or FAST, which leaves freed overflow pages)")
		}
	})

	t.Run("EraseDeveloper", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "erase.db")
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		ctx := context.Background()

		marker := uniqueMarker(t, "erase-marker-")
		now := time.Now().UTC().Truncate(time.Second)
		for _, dev := range []string{marker, "bystander"} {
			if err := db.InsertTokenEvent(ctx, TokenEvent{
				Developer: dev, IssueID: "issue-1", Model: "claude-sonnet-4",
				InputTok: 10, OutputTok: 5, CostMicro: 100,
				Source: "proxy", Fidelity: "realtime", Timestamp: now,
			}); err != nil {
				t.Fatalf("InsertTokenEvent %s: %v", dev, err)
			}
		}

		checkpointTruncate(t, db)
		if !fileContains(t, path, []byte(marker)) {
			t.Fatal("control: marker not in the file before the erase; the search below proves nothing")
		}

		counts, err := db.EraseDeveloper(ctx, marker)
		if err != nil {
			t.Fatalf("EraseDeveloper: %v", err)
		}
		if counts["token_events"] != 1 {
			t.Fatalf("erased %d token_events rows, want 1", counts["token_events"])
		}
		checkpointTruncate(t, db)
		if fileContains(t, path, []byte(marker)) {
			t.Error("erased developer id is still readable in the db file (secure_delete off?)")
		}
	})
}

// TestWALShrinksToJournalSizeLimitAfterALargeDelete pins that a large delete
// does not leave the -wal file at the size of that delete. With secure_delete
// on, every freed page is rewritten into the WAL; without journal_size_limit
// SQLite never shrinks the file, so it would stay at that size until restart.
func TestWALShrinksToJournalSizeLimitAfterALargeDelete(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// 1,500 stale rows with 8 KiB bodies: about 12 MiB of pages for the prune
	// to free, three times the limit.
	if _, err := db.db.ExecContext(ctx, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 1500)
		INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256, received_at)
		SELECT 'push', 'stale-' || i, randomblob(8192), 'x', datetime('now','-91 days') FROM n`); err != nil {
		t.Fatalf("seed stale rows: %v", err)
	}
	checkpointTruncate(t, db)

	n, err := db.PruneWebhookPayloads(ctx)
	if err != nil {
		t.Fatalf("PruneWebhookPayloads: %v", err)
	}
	if n != 1500 {
		t.Fatalf("pruned %d rows, want 1500", n)
	}
	walPath := path + "-wal"
	if sz := fileSize(t, walPath); sz <= dsnJournalSizeLimit {
		t.Fatalf("control: -wal is %d bytes after the prune, want > %d; the assertion below proves nothing", sz, dsnJournalSizeLimit)
	}

	var busy, logFrames, done int
	if err := db.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &done); err != nil {
		t.Fatalf("wal_checkpoint(PASSIVE): %v", err)
	}
	if busy != 0 || done != logFrames {
		t.Fatalf("wal_checkpoint(PASSIVE) incomplete: busy=%d log=%d checkpointed=%d", busy, logFrames, done)
	}
	// The next write restarts the WAL; that commit applies journal_size_limit.
	if err := db.InsertWebhookPayload(ctx, "push", "after", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("insert after prune: %v", err)
	}
	if sz := fileSize(t, walPath); sz > dsnJournalSizeLimit {
		t.Errorf("-wal is %d bytes after a checkpoint and the next write, want <= %d (journal_size_limit)", sz, dsnJournalSizeLimit)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
