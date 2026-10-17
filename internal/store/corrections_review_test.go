package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/corrects"
)

func correctionExportContains(t *testing.T, db *DB, subject string, texts ...string) {
	t.Helper()
	exp, err := db.ExportDeveloper(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range texts {
		if !strings.Contains(string(raw), text) {
			t.Errorf("export for %s lacks %q", subject, text)
		}
	}
}

func TestCorrectionReviewRED1_ChildFirst(t *testing.T) {
	for _, source := range []string{"outcome", "declaration"} {
		t.Run(source, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			child := correctionOutcome(t, db, "child", "acme/app", "child-sha", 20)
			recordCorrection(t, db, child, "Corrects: #10", "pending parent-private evidence")
			parent := Outcome{Developer: "parent-alias", Repo: "acme/app", PRNumber: 10, MergeCommitSHA: "parent-sha"}
			if source == "outcome" {
				correctionOutcome(t, db, parent.Developer, parent.Repo, parent.MergeCommitSHA, parent.PRNumber)
			} else {
				recordCorrection(t, db, parent, "Corrects: none", "late parent declaration")
			}
			if _, err := db.db.Exec(`INSERT INTO developer_alias(alias,canonical) VALUES ('parent-alias','parent')`); err != nil {
				t.Fatal(err)
			}
			correctionExportContains(t, db, "parent", "pending parent-private evidence", "parent_pr_number")
			if _, err := db.EraseDeveloper(context.Background(), "parent"); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"corrective_relations", "correction_audit"} {
				assertCount(t, db, `SELECT COUNT(*) FROM `+table+` WHERE parent_repo IS NOT NULL OR parent_pr_number IS NOT NULL OR parent_declaration_id IS NOT NULL OR parent_developer IS NOT NULL`, 0)
			}
			assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE supporting_text IS NOT NULL`, 0)
			exp, err := db.ExportDeveloper(context.Background(), "parent")
			if err != nil || exp.RowCount() != 0 {
				t.Fatalf("post-erase export: %v %v", exp, err)
			}
		})
	}
}

func TestCorrectionReviewRED2_SharedSiblingEvidence(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	p := correctionOutcome(t, db, "parent", "acme/app", "p", 10)
	recordCorrection(t, db, p, "Corrects: none", "parent text")
	c := correctionOutcome(t, db, "child", "acme/app", "c", 30)
	recordCorrection(t, db, c, "Corrects: #10, #20", "private-parent shared evidence")
	if _, err := db.EraseDeveloper(context.Background(), "parent"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE supporting_text IS NOT NULL`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE reason_text IS NOT NULL`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE parent_pr_number=20`, 1)
}

func TestCorrectionReviewRED3_DeclarationStatsAndTime(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	p, _ := correctionFixture(t, db)
	id := recordCorrection(t, db, p, "Corrects: none", "replay")
	stamp := "2037-09-08 07:06:05"
	if _, err := db.db.Exec(`UPDATE correction_declarations SET additions=987654321,deletions=987654322,changed_files=987654323,recorded_at=? WHERE id=?`, stamp, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EraseDeveloper(context.Background(), p.Developer); err != nil {
		t.Fatal(err)
	}
	rows, err := correctionRows(context.Background(), db.db, `SELECT * FROM correction_declarations`, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"987654321", "987654322", "987654323", "2037-09-08"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("retained stats/timestamp %s: %s", secret, raw)
		}
	}
	assertCount(t, db, fmt.Sprintf(`SELECT COUNT(*) FROM correction_declarations WHERE id=%d`, id), 0)
}

func TestCorrectionReviewRED4_ReviewerOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	correctionFixture(t, db)
	if _, err := db.db.Exec(`UPDATE correction_audit SET actor='reviewer-only',reason_text='reviewer-only audit'; UPDATE corrective_relations SET disposition_text='reviewer-only disposition',appeal_text='reviewer-only appeal'`); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(context.Background(), "reviewer-only")
	if err != nil || len(exp.CorrectionAudit) != 1 || len(exp.CorrectiveRelations) != 0 || len(exp.CorrectionDeclarations) != 0 {
		t.Fatal(exp, err)
	}
	correctionExportContains(t, db, "reviewer-only", "reviewer-only audit")
	if _, err := db.EraseDeveloper(context.Background(), "reviewer-only"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE disposition_text IS NOT NULL OR appeal_text IS NOT NULL OR supporting_text IS NOT NULL`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE actor IS NOT NULL`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE reason_text='reviewer-only audit'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE child_developer IS NOT NULL AND parent_developer IS NOT NULL`, 1)
}

func TestCorrectionReviewRED5_OppositeDeclaration(t *testing.T) {
	for _, subject := range []string{"parent-login", "child-login"} {
		t.Run(subject, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			correctionFixture(t, db)
			if _, err := db.db.Exec(`UPDATE correction_declarations SET supporting_text=CASE developer WHEN 'parent-login' THEN 'unique opposite parent excerpt' ELSE 'unique opposite child excerpt' END`); err != nil {
				t.Fatal(err)
			}
			correctionExportContains(t, db, subject, "unique opposite parent excerpt", "unique opposite child excerpt")
			exp, err := db.ExportDeveloper(context.Background(), subject)
			if err != nil {
				t.Fatal(err)
			}
			counts, err := db.EraseDeveloper(context.Background(), subject)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range []struct {
				table string
				n     int
			}{{"correction_declarations", len(exp.CorrectionDeclarations)}, {"corrective_relations", len(exp.CorrectiveRelations)}, {"correction_audit", len(exp.CorrectionAudit)}} {
				if counts[entry.table] != int64(entry.n) {
					t.Errorf("%s erase/export counts: %d/%d", entry.table, counts[entry.table], entry.n)
				}
			}
		})
	}
	t.Run("child-first-late-counterparty", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		child := correctionOutcome(t, db, "child", "acme/app", "child", 20)
		recordCorrection(t, db, child, "Corrects: #10", "child excerpt")
		parent := correctionOutcome(t, db, "parent", "acme/app", "parent", 10)
		recordCorrection(t, db, parent, "Corrects: none", "late counterparty excerpt naming child")
		correctionExportContains(t, db, "child", "late counterparty excerpt naming child")
		if _, err := db.EraseDeveloper(context.Background(), "child"); err != nil {
			t.Fatal(err)
		}
		assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE supporting_text IS NOT NULL`, 0)
	})

}

func TestCorrectionReviewRED6_Bounds(t *testing.T) {
	for _, tc := range []struct {
		name, text, provenance, repo string
		parent                       int
		valid                        bool
	}{
		{"4096", strings.Repeat("a", 4096), "fetch", "acme/app", 10, true},
		{"4097", strings.Repeat("a", 4097), "fetch", "acme/app", 10, false},
		{"multibyte4096", strings.Repeat("é", 2048), "fetch", "acme/app", 10, true},
		{"multibyte4097", strings.Repeat("é", 2048) + "a", "fetch", "acme/app", 10, false},
		{"provenance128", "text", strings.Repeat("é", 64), "acme/app", 10, true},
		{"provenance129", "text", strings.Repeat("é", 64) + "a", "acme/app", 10, false},
		{"self-parent", "text", "fetch", "acme/app", 20, false},
		{"unqualified", "text", "fetch", "unqualified", 10, false},
		{"bare-repo", "text", "fetch", "app", 10, false},
	} {
		for _, path := range []string{"writer", "sql"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				var err error
				if path == "writer" {
					_, err = db.RecordCorrectionDeclaration(context.Background(), Outcome{Developer: "child", Repo: tc.repo, PRNumber: 20, MergeCommitSHA: "sha"}, corrects.Declaration{State: corrects.StateParents, Parents: []int{tc.parent}}, tc.text, tc.provenance)
				} else {
					tx, e := db.db.Begin()
					if e != nil {
						t.Fatal(e)
					}
					_, err = tx.Exec(`INSERT INTO correction_declarations(developer,repo,pr_number,merge_commit_sha,declaration_state,supporting_text,provenance,recorded_at,rule_version) VALUES ('child',?,20,'sha','parents',?,?,CURRENT_TIMESTAMP,'event3-v1')`, tc.repo, tc.text, tc.provenance)
					if err == nil {
						_, err = tx.Exec(`INSERT INTO corrective_relations(child_declaration_id,child_repo,parent_repo,child_pr_number,parent_pr_number,state,revision,recorded_at,updated_at,rule_version) VALUES (1,?,?,20,?,'pending',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v1')`, tc.repo, tc.repo, tc.parent)
					}
					if err != nil {
						if e := tx.Rollback(); e != nil {
							t.Fatal(e)
						}
					} else {
						err = tx.Commit()
					}
				}
				// The writer retains self-parent evidence as invalid; SQL still
				// rejects a self-parent relation.
				selfParentWriter := tc.name == "self-parent" && path == "writer"
				valid := tc.valid || selfParentWriter
				if (err == nil) != valid {
					t.Errorf("valid=%v err=%v", valid, err)
				}
				if selfParentWriter && err == nil {
					assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE declaration_state='invalid'`, 1)
					assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations`, 0)
					assertCount(t, db, `SELECT COUNT(*) FROM correction_audit`, 0)
				}
				if !valid {
					for _, table := range correctionTables {
						assertCount(t, db, `SELECT COUNT(*) FROM `+table, 0)
					}
				}
			})
		}
	}
	for _, col := range []string{"actor", "actor_role"} {
		for _, n := range []int{128, 129} {
			t.Run(fmt.Sprintf("%s/%d", col, n), func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				_, err := db.db.Exec(`INSERT INTO correction_audit(revision,action,recorded_at,rule_version,`+col+`) VALUES (1,'review',CURRENT_TIMESTAMP,'event3-v1',?)`, strings.Repeat("é", n/2)+strings.Repeat("a", n%2))
				if (err == nil) != (n == 128) {
					t.Errorf("length=%d err=%v", n, err)
				}
				if n == 129 {
					assertCount(t, db, `SELECT COUNT(*) FROM correction_audit`, 0)
				}
			})
		}
	}
	// A constraint failure in the final audit insert must roll back the already
	// inserted declaration and edge, not leave partial fetch-time evidence.
	for _, n := range []int{128, 129} {
		t.Run(fmt.Sprintf("writer-actor/%d", n), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			_, err := db.RecordCorrectionDeclaration(context.Background(), Outcome{Developer: strings.Repeat("a", n), Repo: "acme/app", PRNumber: 20, MergeCommitSHA: "sha"}, corrects.Parse("Corrects: #10"), "text", "fetch")
			if (err == nil) != (n == 128) {
				t.Errorf("length=%d err=%v", n, err)
			}
			if n == 129 {
				for _, table := range correctionTables {
					assertCount(t, db, `SELECT COUNT(*) FROM `+table, 0)
				}
			}
		})
	}
	for _, tc := range []struct{ table, columns, values, col string }{
		{"corrective_relations", "state,revision,recorded_at,updated_at,rule_version", "'pending',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v1'", "supporting_text"},
		{"corrective_relations", "state,revision,recorded_at,updated_at,rule_version", "'pending',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v1'", "disposition_text"},
		{"corrective_relations", "state,revision,recorded_at,updated_at,rule_version", "'pending',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v1'", "appeal_text"},
		{"corrective_relations", "state,revision,recorded_at,updated_at,rule_version", "'pending',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v1'", "resolution_reason"},
		{"correction_audit", "revision,action,recorded_at,rule_version", "1,'review',CURRENT_TIMESTAMP,'event3-v1'", "reason_text"},
	} {
		for _, n := range []int{4096, 4097} {
			t.Run(fmt.Sprintf("sql-%s/%d", tc.col, n), func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				_, err := db.db.Exec(`INSERT INTO `+tc.table+`(`+tc.columns+`,`+tc.col+`) VALUES (`+tc.values+`,?)`, strings.Repeat("é", n/2)+strings.Repeat("a", n%2))
				if (err == nil) != (n == 4096) {
					t.Errorf("length=%d err=%v", n, err)
				}
				if n == 4097 {
					assertCount(t, db, `SELECT COUNT(*) FROM `+tc.table, 0)
				}
			})
		}
	}

}

func TestCorrectionReview_CanonicalRepo(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o := Outcome{Developer: "child", Repo: "ACME/app", PRNumber: 20, MergeCommitSHA: "sha"}
	id := recordCorrection(t, db, o, "Corrects: #10", "first")
	o.Repo = "acme/app"
	if got := recordCorrection(t, db, o, "Corrects: #10", "replay"); got != id {
		t.Errorf("case variants created declarations: %d/%d", id, got)
	}
	for _, repo := range []string{"ACME/app", "acme/app"} {
		row, ok, err := db.CorrectionDeclarationByPR(context.Background(), repo, 20, "sha")
		if err != nil || !ok || row["id"] != id {
			t.Errorf("lookup %s: %v %v %v", repo, row, ok, err)
		}
	}
	for _, repo := range []string{"ACME/app", "acme/app"} {
		rows, err := db.CorrectionRelationsByPR(context.Background(), repo, 20)
		if err != nil || len(rows) != 1 {
			t.Errorf("relation lookup %s: %v %v", repo, rows, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations`, 1)
}

func TestCorrectionArgs_Empty(t *testing.T) {
	if got := correctionArgs("", "", nil); len(got) != 0 {
		t.Fatalf("empty args: %v", got)
	}
}
