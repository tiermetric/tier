package store

import (
	"database/sql"
	"fmt"
)

const correctionProjectionSchemaVersion = 10

var UpgradeNoticeCorrectionProjection = fmt.Sprintf("tierd: this database was upgraded to schema version %d (Event 3 assessments, allocations and endpoint-aware DSAR, #1235 U1). ", correctionProjectionSchemaVersion) + upgradeOneWayNotice

// NULL means unknown historical provenance. Never UPDATE old seal bodies,
// digests, policy columns or rollup bytes to invent provenance retrospectively.
func migrateCorrectionSealColumns(db *sql.DB) error {
	for _, entry := range []struct{ table, column, definition string }{
		{"sealed_report", "scoring_policy", "TEXT"},
		{"sealed_report", "correction_rule_version", "TEXT"},
		{"sealed_report", "rubric_version", "INTEGER"},
		{"sealed_report", "correction_revision", "INTEGER"},
		{"sealed_rollup", "outcome_n", "INTEGER"},
	} {
		if err := addColumnIfMissing(db, entry.table, entry.column, entry.definition); err != nil {
			return err
		}
	}
	return nil
}
