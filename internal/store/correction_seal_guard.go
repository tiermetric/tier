package store

import (
	"database/sql"
	"fmt"
)

// Durable triggers extend the EXISTING seal epoch, including for an older v10
// writer. No new guard argument can be omitted by that writer: its SealReport
// already checks erase_epoch. The INSERT guard also refuses old writers that
// cannot pin an enabled policy. Additive, no new schema version is required.
func installCorrectionSealGuard(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"correction_declarations", "corrective_relations", "correction_assessments", "correction_allocations", "outcomes", "developer_alias"} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			invalidate := ""
			if table != "correction_allocations" && table != "correction_assessments" {
				invalidate = `UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1;`
			}
			when := "WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1"
			if table == "correction_assessments" {
				ref := "NEW"
				if op == "DELETE" {
					ref = "OLD"
				}
				invalidate = "UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1 AND " + ref + ".basis='reviewed';"
			}
			query := fmt.Sprintf(`DROP TRIGGER IF EXISTS trg_u4_%s_%s; CREATE TRIGGER trg_u4_%s_%s AFTER %s ON %s %s BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 %s END`, table, op, table, op, op, table, when, invalidate)
			if _, err := tx.Exec(query); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`DROP TRIGGER IF EXISTS trg_u4_policy_epoch; CREATE TRIGGER trg_u4_policy_epoch AFTER UPDATE ON correction_policy
 WHEN (OLD.scoring_enabled=1 OR NEW.scoring_enabled=1) AND (OLD.scoring_enabled IS NOT NEW.scoring_enabled OR OLD.rubric_version IS NOT NEW.rubric_version OR OLD.rule_version IS NOT NEW.rule_version OR OLD.projection_revision IS NOT NEW.projection_revision)
 BEGIN INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1; END;
 CREATE TRIGGER IF NOT EXISTS trg_u4_seal_policy BEFORE INSERT ON sealed_report
 WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 AND
 (NEW.scoring_policy IS NOT 'event3' OR NEW.rubric_version IS NOT 2 OR NEW.correction_rule_version IS NOT 'event3-v2'
 OR NEW.correction_revision IS NOT (SELECT projection_revision FROM correction_policy WHERE id=1)
 OR (SELECT rule_version FROM correction_policy WHERE id=1) IS NOT 'event3-v2')
 BEGIN SELECT RAISE(ABORT,'Event 3 seal requires resolved pinned policy'); END;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
