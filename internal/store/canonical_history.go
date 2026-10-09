package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaCanonicalHistory is the canonical-id history (#975): every link from a
// raw id or alias to a canonical id through developer_alias, with the interval
// it was active, so EraseDeveloper can find sealed person keys computed from a
// canonical id an alias edit has since retired. Open applies it after
// schemaSealed and never rebuilds it.
//
// One row per link: since_seal is the highest sealed_report.id committed before
// the alias edit that made it, ended_seal the same for the edit that re-pointed
// or deleted the alias (NULL while active). Seals and alias edits are both
// write transactions, which SQLite serialises, and sealed_report.id is
// AUTOINCREMENT, so the link was active when sealed_report s was sealed exactly
// when since_seal < s <= ended_seal: ordered by commit, never by timestamps,
// which sealTime truncates to whole seconds. A link that already existed when
// #975 first opened the database has since_seal NULL: its start is unknown. An
// unmapped id is its own canonical and has no row. At most one active row per
// alias. Tenant retrofit: tenant_id LEADS -> (tenant_id, alias) WHERE
// ended_seal IS NULL.
//
// Append-only as #604, like sealed_person: the one UPDATE admitted sets an
// active row's ended_seal, once; every other UPDATE aborts. An INSERT must be
// active and must not collide with an explicit rowid or the alias's active
// row, because INSERT OR REPLACE would delete that row without firing its
// UPDATE trigger (recursive_triggers is off; see schemaSealed). DELETE is not
// refused: EraseDeveloper deletes the erased person's rows.
const schemaCanonicalHistory = `
CREATE TABLE IF NOT EXISTS canonical_id_history (
    alias      TEXT NOT NULL,
    canonical  TEXT NOT NULL,
    since_seal INTEGER,
    ended_seal INTEGER,
    CHECK (alias <> canonical)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_canonical_id_history_active
    ON canonical_id_history(alias) WHERE ended_seal IS NULL;
CREATE TRIGGER IF NOT EXISTS trg_canonical_id_history_end_only
BEFORE UPDATE ON canonical_id_history
WHEN NOT (OLD.ended_seal IS NULL AND NEW.ended_seal IS NOT NULL
          AND NEW.rowid = OLD.rowid
          AND NEW.alias = OLD.alias
          AND NEW.canonical = OLD.canonical
          AND NEW.since_seal IS OLD.since_seal)
BEGIN
    SELECT RAISE(ABORT, 'canonical_id_history is append-only: only ending an active link is permitted (#975)');
END;
CREATE TRIGGER IF NOT EXISTS trg_canonical_id_history_no_replace
BEFORE INSERT ON canonical_id_history
WHEN NEW.ended_seal IS NOT NULL
  OR EXISTS (SELECT 1 FROM canonical_id_history WHERE rowid = NEW.rowid
             OR (alias = NEW.alias AND ended_seal IS NULL))
BEGIN
    SELECT RAISE(ABORT, 'canonical_id_history is append-only: insert one active link per alias (#975)');
END;
`

// backfillCanonicalHistory records, with an unknown start, every current
// developer_alias mapping whose alias has no active link. It runs on every Open
// and is idempotent; after the first it finds nothing, since every alias edit
// records its own links.
func backfillCanonicalHistory(db *sql.DB) error {
	if _, err := db.Exec(`INSERT INTO canonical_id_history (alias, canonical, since_seal)
		SELECT a.alias, a.canonical, NULL FROM developer_alias a
		WHERE a.alias <> a.canonical AND NOT EXISTS (SELECT 1 FROM canonical_id_history h
		                  WHERE h.alias = a.alias AND h.ended_seal IS NULL)`); err != nil {
		return fmt.Errorf("backfill canonical_id_history: %w", err)
	}
	return nil
}

// sealWatermark is the highest sealed_report.id committed before the current
// write transaction, 0 before the first seal.
const sealWatermark = `(SELECT COALESCE(MAX(id), 0) FROM sealed_report)`

// endCanonicalLinkTx ends alias's active link, inside the alias edit's
// transaction that re-points or deletes alias; previous is the canonical alias
// resolved to before the edit.
func endCanonicalLinkTx(ctx context.Context, tx *sql.Tx, alias, previous string) error {
	res, err := tx.ExecContext(ctx, `UPDATE canonical_id_history SET ended_seal = `+sealWatermark+`
		WHERE alias = ? AND ended_seal IS NULL`, alias)
	if err != nil {
		return fmt.Errorf("end canonical_id_history link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n > 0 || previous == alias {
		return err
	}
	// Defensive: every current mapping has an active link, recorded by its edit
	// or by the Open backfill, so this records nothing today.
	if _, err := tx.ExecContext(ctx, `INSERT INTO canonical_id_history (alias, canonical, since_seal)
		VALUES (?, ?, NULL)`, alias, previous); err != nil {
		return fmt.Errorf("record canonical_id_history link: %w", err)
	}
	return endCanonicalLinkTx(ctx, tx, alias, alias)
}

// startCanonicalLinkTx records alias's new link to canonical, active from this
// transaction on.
func startCanonicalLinkTx(ctx context.Context, tx *sql.Tx, alias, canonical string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO canonical_id_history (alias, canonical, since_seal)
		VALUES (?, ?, `+sealWatermark+`)`, alias, canonical); err != nil {
		return fmt.Errorf("record canonical_id_history link: %w", err)
	}
	return nil
}

// canonicalLink is one canonical_id_history row, its watermarks read with an
// unknown start as 0 and an active link's end as the largest id.
type canonicalLink struct {
	canonical    string
	since, ended int64
}

// activeAt reports whether the link was active when sealed_report sealID was
// sealed: since < sealID <= ended.
func (l canonicalLink) activeAt(sealID int64) bool {
	return l.since < sealID && sealID <= l.ended
}

// canonicalLinksTx returns every canonical_id_history link from one of ids.
func canonicalLinksTx(ctx context.Context, tx *sql.Tx, ids []string) ([]canonicalLink, error) {
	placeholders, args := inClause(ids)
	rows, err := tx.QueryContext(ctx, `SELECT canonical, COALESCE(since_seal, 0), COALESCE(ended_seal, 9223372036854775807)
		FROM canonical_id_history WHERE alias IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read canonical_id_history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []canonicalLink
	for rows.Next() {
		var l canonicalLink
		if err := rows.Scan(&l.canonical, &l.since, &l.ended); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
