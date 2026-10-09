package store

import (
	"context"
	"database/sql"
)

// readQuerier is the read surface of both *sql.DB and *sql.Tx.
type readQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// reader holds the reads a Snapshot shares with the pool. Each DB method of the
// same name runs it on d.db, one pooled connection per statement, except
// DB.DeveloperEvidenceWindow and DB.ReportDigests, which run it in their own read
// transaction; a Snapshot runs it on one read transaction.
type reader struct{ q readQuerier }

// Snapshot reads one database state: every method sees exactly what had
// committed at the snapshot's first read, whatever commits alongside it (#913).
// It is valid only inside the function passed to ReadSnapshot.
type Snapshot struct{ reader }

// ReadSnapshot runs fn with a Snapshot and rolls the snapshot back when fn
// returns. A DEFERRED read transaction starts its snapshot at its first read,
// not at begin, so the state fn reads is the one its first read sees. It holds
// one pooled connection until fn returns (see beginRead), so nothing inside fn
// may wait on another pooled read.
func (d *DB) ReadSnapshot(ctx context.Context, fn func(*Snapshot) error) error {
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return err
	}
	defer release()
	return fn(&Snapshot{reader{tx}})
}
