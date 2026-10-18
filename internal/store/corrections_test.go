package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
)

var correctionTables = []string{"correction_declarations", "corrective_relations", "correction_audit"}

func recordCorrection(t *testing.T, db *DB, o Outcome, body, text string) int64 {
	t.Helper()
	writer, ok := any(db).(interface {
		RecordCorrectionDeclaration(context.Context, Outcome, corrects.Declaration, string, string) (int64, error)
	})
	if !ok {
		t.Fatal("RecordCorrectionDeclaration is unavailable")
	}
	id, err := writer.RecordCorrectionDeclaration(context.Background(), o, corrects.Parse(body), text, "test-fetch-time")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func correctionOutcome(t *testing.T, db *DB, developer, repo, sha string, pr int) Outcome {
	t.Helper()
	o := Outcome{Developer: developer, Repo: repo, MergeCommitSHA: sha, PRNumber: pr, IssueID: fmt.Sprintf("issue-%d", pr), Weight: 3, Quality: 1, Timestamp: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	var err error
	_, err = db.InsertOutcome(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT id FROM outcomes WHERE merge_commit_sha=?`, sha).Scan(&o.ID); err != nil {
		t.Fatal(err)
	}
	return o
}

func correctionFixture(t *testing.T, db *DB) (Outcome, Outcome) {
	t.Helper()
	parent := correctionOutcome(t, db, "parent-login", "acme/parent", "parent-merge-sha", 11)
	child := correctionOutcome(t, db, "child-login", "acme/parent", "child-merge-sha", 22)
	recordCorrection(t, db, parent, "Corrects: none", "parent bounded text")
	recordCorrection(t, db, child, "Corrects: #11", "child bounded text parent-login acme/parent#11 parent-merge-sha")
	if _, err := db.db.Exec(`UPDATE corrective_relations SET disposition_text='reviewer rationale parent-login acme/parent#11 parent-merge-sha', appeal_text='appeal child-login acme/parent#22 child-merge-sha', resolution_reason='parent-login rationale'; UPDATE correction_audit SET reason_text='audit rationale parent-login child-login', actor='parent-login'`); err != nil {
		t.Fatal(err)
	}
	return parent, child
}

func TestCorrectionSchema_FreshAndMigratedConverge(t *testing.T) {
	fresh, cleanup := newTestDB(t)
	defer cleanup()
	path := filepath.Join(t.TempDir(), "legacy.db")
	// A real version-7 table layout, with no correction tables or cutover marker.
	sqlDB := openRawDB(t, path)
	if _, err := sqlDB.Exec(schemaTables + schemaPostMigration + schemaOutcomeExclusions + `; PRAGMA user_version=7`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range correctionTables {
		var a, b string
		if err := fresh.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, table).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := migrated.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, table).Scan(&b); err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("%s schemas differ", table)
		}
		var ai, bi string
		q := `SELECT group_concat(sql,';') FROM (SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL ORDER BY name)`
		if err := fresh.db.QueryRow(q, table).Scan(&ai); err != nil {
			t.Fatal(err)
		}
		if err := migrated.db.QueryRow(q, table).Scan(&bi); err != nil {
			t.Fatal(err)
		}
		if ai != bi {
			t.Fatalf("%s indexes differ", table)
		}
	}
	if schemaVersion != 12 || qualityTriggerSchemaVersion != 11 || readUserVersion(t, path) != schemaVersion || migrated.UpgradeNotice() != UpgradeNoticeCorrections+"\n"+UpgradeNoticePricingObservations+"\n"+UpgradeNoticeCorrectionProjection+"\n"+UpgradeNoticeQualityTriggers+"\n"+UpgradeNoticeIssueRule || fresh.UpgradeNotice() != "" {
		t.Fatal("schema version/notice incorrect")
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.UpgradeNotice() != "" {
		t.Fatal("upgrade notice repeated")
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
}

// Explain the production UPDATE, including its endpoint lookups and partial-index
// guards, so a standalone parent lookup cannot hide a reconciliation scan.
func TestCorrectionReconcile_EndpointQueryPlan(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing-version-8"}[reopen], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrections.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if reopen {
				// Model an already-current database created before the index.
				if _, err := db.db.Exec(`DROP INDEX IF EXISTS correction_relation_parent_endpoint`); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if db.UpgradeNotice() != "" {
					t.Fatal("additive index unexpectedly announced a schema upgrade")
				}
			}
			defer func() { _ = db.Close() }()
			args := []any{sql.Named("repo", "acme/app"), sql.Named("pr", 11), sql.Named("now", time.Now().UTC())}
			plan := explainPlan(t, db, reconcileCorrectionRelationsSQL, args...)
			for alias, index := range map[string]string{"child_edge": "correction_relation_edge", "parent_edge": "correction_relation_parent_endpoint"} {
				if !strings.Contains(plan, "SEARCH "+alias+" USING INDEX "+index+" (") &&
					!strings.Contains(plan, "SEARCH "+alias+" USING COVERING INDEX "+index+" (") {
					t.Errorf("reconciliation must SEARCH %s using %s: %s", alias, index, plan)
				}
			}
			if !strings.Contains(plan, "(parent_repo=? AND parent_pr_number=?)") {
				t.Errorf("parent search must constrain both endpoint columns: %s", plan)
			}
			if strings.Contains(plan, "SCAN r") || strings.Contains(plan, "SCAN child_edge") || strings.Contains(plan, "SCAN parent_edge") {
				t.Errorf("reconciliation scans relations: %s", plan)
			}
			// Prove the missing index makes the parent arm scan relations.
			if _, err := db.db.Exec(`DROP INDEX correction_relation_parent_endpoint`); err != nil {
				t.Fatal(err)
			}
			control := explainPlan(t, db, reconcileCorrectionRelationsSQL, args...)
			if !strings.Contains(control, "SCAN parent_edge") {
				t.Fatalf("missing-index control must expose a parent relation scan: %s", control)
			}
		})
	}
}

func TestCorrectionRelations_NoOutcomeDeleteCascade(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	p, c := correctionFixture(t, db)
	// foreign_keys is deliberately off; explicit erasure ordering protects copied
	// identifiers, rather than relying on the documentary SET NULL clauses.
	var enabled int
	if err := db.db.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("foreign_keys=%d err=%v", enabled, err)
	}
	for _, source := range []string{"outcomes", "correction_declarations"} {
		key := "outcome_id"
		if source == "correction_declarations" {
			key = "declaration_id"
		}
		query := `CREATE TRIGGER check_correction_scrub_` + source + ` BEFORE DELETE ON ` + source + ` BEGIN
 SELECT RAISE(ABORT,'correction evidence must be scrubbed before source delete')
 WHERE EXISTS (SELECT 1 FROM corrective_relations WHERE child_` + key + `=OLD.id OR parent_` + key + `=OLD.id OR supporting_text IS NOT NULL OR disposition_text IS NOT NULL OR appeal_text IS NOT NULL)
 OR EXISTS (SELECT 1 FROM correction_audit WHERE child_` + key + `=OLD.id OR parent_` + key + `=OLD.id OR reason_text IS NOT NULL OR actor IS NOT NULL);
 END`
		if _, err := db.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	// Prove that the trigger would catch deleting an outcome before scrubbing.
	if _, err := db.db.Exec(`DELETE FROM outcomes WHERE id=?`, p.ID); err == nil {
		t.Fatal("ordering trigger did not reject premature delete")
	}
	if _, err := db.db.Exec(`DELETE FROM correction_declarations WHERE outcome_id=?`, p.ID); err == nil {
		t.Fatal("ordering trigger did not reject premature declaration delete")
	}
	if _, err := db.EraseDeveloper(context.Background(), p.Developer); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes WHERE id=`+fmt.Sprint(c.ID), 1)
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit`, 1)
	for _, table := range correctionTables {
		rows, err := db.db.Query(`SELECT "on_delete" FROM pragma_foreign_key_list(?) WHERE "table"='outcomes'`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var action string
			if err := rows.Scan(&action); err != nil {
				t.Fatal(err)
			}
			if action == "CASCADE" {
				t.Error("outcome cascade")
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCorrectionRelations_MultipleParentsAreIdempotent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	child := correctionOutcome(t, db, "child", "acme/app", "child", 30)
	id := recordCorrection(t, db, child, "Corrects: #10, #20, #10", "evidence")
	for range 2 {
		if got := recordCorrection(t, db, child, "Corrects: #20, #10", "evidence"); got != id {
			t.Fatalf("declaration id changed: %d/%d", id, got)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations`, 2)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit`, 2)
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE parent_outcome_id IS NULL AND state='pending'`, 2)
	// Unattributed declarations are durable too, with independent canonical identities.
	recordCorrection(t, db, Outcome{Developer: "no-issue", Repo: "acme/app", PRNumber: 40, MergeCommitSHA: "unattributed"}, "Corrects: #10", "unattributed text")
	assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE outcome_id IS NULL`, 1)
	lookups, ok := any(db).(interface {
		CorrectionDeclarationByPR(context.Context, string, int, string) (map[string]any, bool, error)
		CorrectionRelationsByOutcome(context.Context, int64) ([]map[string]any, error)
		CorrectionRelationsByPR(context.Context, string, int) ([]map[string]any, error)
	})
	if !ok {
		t.Fatal("endpoint lookups unavailable")
	}
	row, found, err := lookups.CorrectionDeclarationByPR(context.Background(), "acme/app", 30, "child")
	if err != nil || !found || row["id"] != id {
		t.Fatalf("canonical lookup: %v %v %v", row, found, err)
	}
	_, found, err = lookups.CorrectionDeclarationByPR(context.Background(), "other/app", 30, "child")
	if err != nil || found {
		t.Fatal("cross-repo lookup")
	}

	unresolved, err := lookups.CorrectionRelationsByPR(context.Background(), "acme/app", 40)
	if err != nil || len(unresolved) != 1 {
		t.Fatalf("unattributed lookup: %v %v", unresolved, err)
	}
	if got, err := db.RecordCorrectionDeclaration(context.Background(), child, corrects.Parse("Corrects: #99"), "changed declaration", "test-fetch-time"); got != id || !errors.Is(err, ErrCorrectionDeclarationIgnored) {
		t.Fatalf("changed declaration replay: id=%d err=%v", got, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE child_outcome_id IS NOT NULL`, 2)
	rows, err := lookups.CorrectionRelationsByOutcome(context.Background(), child.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("child endpoint: %v %v", rows, err)
	}
}

func TestCorrectionDSAR_ExportsEitherEndpointAndAllText(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	p, c := correctionFixture(t, db)
	for _, subject := range []string{p.Developer, c.Developer} {
		exp, err := db.ExportDeveloper(context.Background(), subject)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(exp)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, table := range correctionTables {
			rows, ok := decoded[table].([]any)
			want := 1
			if table == "correction_declarations" {
				want = 2
			}
			if !ok || len(rows) != want {
				t.Fatalf("%s export for %s: %s", table, subject, raw)
			}
			for _, col := range storedColumns(t, db, table) {
				if _, ok := rows[0].(map[string]any)[col]; !ok {
					t.Errorf("%s omitted %s", table, col)
				}
			}
		}
		for _, text := range []string{"reviewer rationale", "appeal child-login", "audit rationale"} {
			if !strings.Contains(string(raw), text) {
				t.Fatalf("export lacks %q", text)
			}
		}
		if exp.RowCount() != 5 {
			t.Fatalf("RowCount=%d want 5", exp.RowCount())
		}
	}
}

func TestCorrectionErase_ScrubsAllIdentifiersAndText(t *testing.T) {
	for _, party := range []string{"parent", "child"} {
		t.Run(party, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			p, c := correctionFixture(t, db)
			erased, survivor := p, c
			if party == "child" {
				erased, survivor = c, p
			}
			// Use unique stats and time so the all-column retained-value scan also
			// catches a scrubbed-but-retained declaration that GitHub could re-identify.
			if _, err := db.db.Exec(`UPDATE correction_declarations SET additions=987654321,deletions=987654322,changed_files=987654323,recorded_at='2037-09-08 07:06:05' WHERE outcome_id=?`, erased.ID); err != nil {
				t.Fatal(err)
			}
			// The subject set must include raw aliases in endpoint rows.
			if _, err := db.db.Exec(`INSERT INTO developer_alias(alias,canonical) VALUES (?,?)`, erased.Developer, "canonical-subject"); err != nil {
				t.Fatal(err)
			}
			before, _, err := db.OutcomeByMergeCommit(context.Background(), survivor.MergeCommitSHA)
			if err != nil {
				t.Fatal(err)
			}
			counts, err := db.EraseDeveloper(context.Background(), "canonical-subject")
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range correctionTables {
				want := int64(1)
				if table == "correction_declarations" {
					want = 2
				}
				if counts[table] != want {
					t.Fatalf("%s count=%d", table, counts[table])
				}
			}
			prefix := party + "_"
			for _, table := range []string{"corrective_relations", "correction_audit"} {
				var n int
				if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE ` + prefix + `outcome_id IS NOT NULL OR ` + prefix + `declaration_id IS NOT NULL OR ` + prefix + `developer IS NOT NULL OR ` + prefix + `repo IS NOT NULL OR ` + prefix + `pr_number IS NOT NULL OR ` + prefix + `merge_commit_sha IS NOT NULL OR ` + prefix + `merged_at IS NOT NULL`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("%s endpoint retained: %d %v", table, n, err)
				}
			}
			assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE outcome_id=`+fmt.Sprint(erased.ID), 0)
			// Serialize every retained column, not just selected id columns.
			for _, table := range correctionTables {
				var data string
				columns := storedColumns(t, db, table)
				expr := make([]string, len(columns))
				for i, col := range columns {
					expr[i] = "COALESCE(CAST(" + col + " AS TEXT),'')"
				}
				if err := db.db.QueryRow(`SELECT group_concat(` + strings.Join(expr, " || ' ' || ") + `, ' ') FROM ` + table).Scan(&data); err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{erased.Developer, erased.MergeCommitSHA, fmt.Sprintf("%s#%d", erased.Repo, erased.PRNumber), party + " bounded text", "reviewer rationale", "audit rationale", "appeal child-login", "987654321", "987654322", "987654323", "2037-09-08"} {
					if strings.Contains(data, secret) {
						t.Errorf("%s retained %q: %s", table, secret, data)
					}
				}
			}
			after, found, err := db.OutcomeByMergeCommit(context.Background(), survivor.MergeCommitSHA)
			if err != nil || !found || !reflect.DeepEqual(before, after) {
				t.Fatalf("survivor changed: %v %v", found, err)
			}
			exp, err := db.ExportDeveloper(context.Background(), "canonical-subject")
			if err != nil || exp.RowCount() != 0 {
				t.Fatalf("erased DSAR: %d %v", exp.RowCount(), err)
			}
			again, err := db.EraseDeveloper(context.Background(), "canonical-subject")
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range again {
				if n != 0 {
					t.Fatal("non-idempotent erasure")
				}
			}
		})
	}
}

func TestCorrectionPrivacyClassification_IsExplicit(t *testing.T) {
	doc, err := os.ReadFile("../../docs/privacy.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range append(correctionTables, "supporting", "disposition", "appeal", "rationale", "endpoint", "merge SHA") {
		if !strings.Contains(string(doc), word) {
			t.Errorf("privacy inventory lacks %q", word)
		}
	}
	db, cleanup := newTestDB(t)
	defer cleanup()
	// A text-only actor is also a data subject, even without either endpoint.
	if _, err := db.db.Exec(`INSERT INTO correction_audit(revision,action,actor,actor_role,recorded_at,rule_version,reason_text) VALUES (1,'review','reviewer','operator',CURRENT_TIMESTAMP,'event3-v1','rationale')`); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(context.Background(), "reviewer")
	if err != nil || exp.RowCount() != 1 {
		t.Fatalf("actor DSAR: %d %v", exp.RowCount(), err)
	}
	counts, err := db.EraseDeveloper(context.Background(), "reviewer")
	if err != nil || counts["correction_audit"] != 1 {
		t.Fatalf("actor erasure: %v %v", counts, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE actor IS NOT NULL`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE reason_text='rationale' AND actor_role='operator'`, 1)
}

func TestCorrectionLegacyAPIZeroMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw := openRawDB(t, path)
	if _, err := raw.Exec(schemaTables + schemaPostMigration + schemaOutcomeExclusions + `; PRAGMA user_version=7; INSERT INTO outcomes(developer,issue_id,weight,quality,source,additions,deletions,changed_files,merge_commit_sha,repo,ts) VALUES ('alice','legacy',1,1,'api-outcome',0,0,0,'legacy','acme/app','2026-10-01 00:00:00'),('alice','nonzero',1,1,'api-outcome',1,0,0,'nonzero','acme/app','2026-10-01 00:00:00'),('alice','webhook',1,1,'github-webhook',0,0,0,'webhook','acme/app','2026-10-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	o := Outcome{Developer: "alice", IssueID: "later", Source: OutcomeSourceAPI, Weight: 1, Quality: 1, MergeCommitSHA: "later", Repo: "acme/app", Timestamp: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	o.SetDiffStats(&zero, &zero, &zero)
	if _, err := db.InsertOutcome(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	check := func(o Outcome) {
		t.Helper()
		a, d, f := o.DiffStats()
		available := a != nil && d != nil && f != nil
		if available != (o.IssueID != "legacy") {
			t.Errorf("%s availability=%v", o.IssueID, available)
		}
	}
	for _, sha := range []string{"legacy", "nonzero", "webhook", "later"} {
		o, found, err := db.OutcomeByMergeCommit(context.Background(), sha)
		if err != nil || !found {
			t.Fatal(err)
		}
		check(o)
	}
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 2, 0)
	list, err := db.DeveloperOutcomes(context.Background(), "alice", since)
	if err != nil || len(list) != 4 {
		t.Fatalf("developer %d %v", len(list), err)
	}
	for _, o := range list {
		check(o)
	}
	list, err = db.AllOutcomesWindow(context.Background(), since, until, FleetWide)
	if err != nil || len(list) != 4 {
		t.Fatalf("window %d %v", len(list), err)
	}
	for _, o := range list {
		check(o)
	}
	list, _, err = db.ListOutcomes(context.Background(), since, until, PageCursor{}, 100)
	if err != nil || len(list) != 4 {
		t.Fatalf("list %d %v", len(list), err)
	}
	for _, o := range list {
		check(o)
	}
	o, found, err := db.LatestOutcomeByIssue(context.Background(), "acme/app", "legacy", until)
	if err != nil || !found {
		t.Fatal(err)
	}
	check(o)
	if err := db.ReadSnapshot(context.Background(), func(s *Snapshot) error {
		list, err := s.AllOutcomesWindow(context.Background(), since, until, FleetWide)
		if err != nil {
			return err
		}
		for _, o := range list {
			check(o)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
