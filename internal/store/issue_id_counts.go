package store

import (
	"context"
	"database/sql"
	"path/filepath"
)

// IssueIDCounts reads existing outcome IDs without changing attribution.
func (d *DB) IssueIDCounts(ctx context.Context) (map[string]int, error) {
	return issueIDCounts(ctx, d.db)
}

// InspectIssueIDCounts opens the existing database read-only, without migrations.
func InspectIssueIDCounts(ctx context.Context, path string) (map[string]int, error) {
	db, err := openInspectionDB(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return issueIDCounts(ctx, db)
}

func issueIDCounts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT issue_id, COUNT(*) FROM outcomes GROUP BY issue_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// openInspectionDB opens path read-only without migrations; modernc honors
// mode=ro only in a file: URI.
func openInspectionDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", ReadOnlyURI(abs, "mode=ro"))
}
