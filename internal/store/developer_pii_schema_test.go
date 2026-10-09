package store

import (
	"context"
	"testing"
)

// developerColumnTables returns every table in the migrated schema that has a
// column named `developer`.
func developerColumnTables(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT m.name
		   FROM sqlite_master AS m
		   JOIN pragma_table_info(m.name) AS p
		  WHERE m.type = 'table' AND p.name = 'developer'
		  ORDER BY m.name`)
	if err != nil {
		t.Fatalf("list developer-column tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate developer-column tables: %v", err)
	}
	return tables
}

// TestEveryDeveloperColumnTableIsErasedOrExcluded pins #814: a table with a
// `developer` column that is in neither developerPIITables nor
// developerColumnExclusions is personal data that EraseDeveloper never deletes.
// It proves erasure-list membership only. ExportDeveloper reads each table with
// its own hand-written query, so this test says nothing about the export. It
// matches only a column named exactly `developer`: a table that keys a person
// under any other column name is invisible to it. It also refuses an exclusion
// entry that names a table without a `developer` column, so the list cannot
// hold stale names.
func TestEveryDeveloperColumnTableIsErasedOrExcluded(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	tables := developerColumnTables(t, db)
	if len(tables) == 0 {
		t.Fatal("found no table with a developer column: the schema query compared nothing")
	}

	covered := make(map[string]bool, len(developerPIITables))
	for _, name := range developerPIITables {
		covered[name] = true
	}
	hasDeveloper := make(map[string]bool, len(tables))
	for _, name := range tables {
		hasDeveloper[name] = true
		_, excluded := developerColumnExclusions[name]
		if !covered[name] && !excluded {
			t.Errorf("table %q has a developer column but is in neither developerPIITables nor developerColumnExclusions: EraseDeveloper skips it", name)
		}
	}
	for name := range developerColumnExclusions {
		if !hasDeveloper[name] {
			t.Errorf("developerColumnExclusions names %q, which has no developer column in the migrated schema", name)
		}
	}
	t.Logf("checked %d developer-column tables: %v", len(tables), tables)
}
