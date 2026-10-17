package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/corrects"
)

// RED-1: exclusion tombstones are the only author evidence after a child-first
// declaration and may name both an original owner and a trusted GitHub author.
func TestCorrectionEngineRED1_ExcludedParent(t *testing.T) {
	for _, source := range []string{"tombstone", "moved-outcome"} {
		for _, subject := range []string{"original-parent", "trusted-parent"} {
			t.Run(source+"/"+subject, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				ctx := context.Background()
				child := correctionOutcome(t, db, "child", "acme/app", "child-sha", 20)
				recordCorrection(t, db, child, "Corrects: #10", "private excluded parent evidence")
				parent := Outcome{Developer: "trusted-parent", Repo: "acme/app", PRNumber: 10, MergeCommitSHA: "parent-sha"}
				if source == "moved-outcome" {
					correctionOutcome(t, db, "original-parent", parent.Repo, parent.MergeCommitSHA, parent.PRNumber)
				}
				if err := db.ExcludePROutcome(ctx, parent, "revert evidence", "github-webhook"); err != nil {
					t.Fatal(err)
				}
				if source == "tombstone" {
					if _, err := db.db.Exec(`UPDATE outcome_exclusions SET developer='original-parent'`); err != nil {
						t.Fatal(err)
					}
				}
				assertCount(t, db, `SELECT COUNT(*) FROM outcomes WHERE pr_number=10`, 0)
				exp, err := db.ExportDeveloper(ctx, subject)
				if err != nil {
					t.Fatal(err)
				}
				// Late exclusion preserves the declaration audit and adds a
				// reconciliation revision; both must be exported and scrubbed.
				if len(exp.CorrectionDeclarations) != 1 || len(exp.CorrectiveRelations) != 1 || len(exp.CorrectionAudit) != 2 {
					t.Errorf("excluded-parent export omitted correction evidence: %+v", exp)
				}
				if _, err := db.EraseDeveloper(ctx, subject); err != nil {
					t.Fatal(err)
				}
				for _, table := range []string{"corrective_relations", "correction_audit"} {
					assertCount(t, db, `SELECT COUNT(*) FROM `+table+` WHERE parent_repo IS NOT NULL OR parent_pr_number IS NOT NULL OR parent_declaration_id IS NOT NULL OR parent_outcome_id IS NOT NULL OR parent_developer IS NOT NULL OR parent_merge_commit_sha IS NOT NULL OR parent_merged_at IS NOT NULL`, 0)
					want := 1
					if table == "correction_audit" {
						want = 2
					}
					assertCount(t, db, `SELECT COUNT(*) FROM `+table+` WHERE child_repo='acme/app' AND child_pr_number=20`, want)
				}
				assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE supporting_text IS NOT NULL OR provenance IS NOT NULL`, 0)
				assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE supporting_text IS NOT NULL`, 0)
				assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE reason_text IS NOT NULL OR actor IS NOT NULL`, 0)
				exp, err = db.ExportDeveloper(ctx, subject)
				if err != nil || exp.RowCount() != 0 {
					t.Fatalf("post-erase export: %+v %v", exp, err)
				}
			})
		}
	}
}

// RED-2: repo#PR identity must delete a declaration even before author/id fill.
func TestCorrectionEngineRED2_DeveloperlessDeclaration(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	child := correctionOutcome(t, db, "child", "acme/app", "child-sha", 20)
	recordCorrection(t, db, child, "Corrects: #10", "child excerpt")
	parent := Outcome{Repo: "acme/app", PRNumber: 10, MergeCommitSHA: "parent-sha"}
	id := recordCorrection(t, db, parent, "Corrects: none", "developerless parent excerpt")
	correctionOutcome(t, db, "parent", parent.Repo, parent.MergeCommitSHA, parent.PRNumber)
	if _, err := db.EraseDeveloper(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, fmt.Sprintf(`SELECT COUNT(*) FROM correction_declarations WHERE id=%d`, id), 0)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE developer='child'`, 1)
}

// YELLOW: retained, scrubbed correction rows contribute affected-row counts.
func TestCorrectionEngineYELLOW_EraseCountSemantics(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	correctionFixture(t, db)
	counts, err := db.EraseDeveloper(context.Background(), "parent-login")
	if err != nil {
		t.Fatal(err)
	}
	if counts["correction_declarations"] != 2 || counts["corrective_relations"] != 1 || counts["correction_audit"] != 1 {
		t.Fatalf("affected counts: %v", counts)
	}
	for _, table := range correctionTables {
		assertCount(t, db, `SELECT COUNT(*) FROM `+table, 1)
	}
	doc, err := os.ReadFile("../../docs/privacy.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"per-table affected-row counts", "correction counts are rows scrubbed", "not physical row deletions"} {
		if !strings.Contains(string(doc), phrase) {
			t.Errorf("erase count documentation lacks %q", phrase)
		}
	}
}

// YELLOW: editable PR declarations must produce a caller-visible replay status.
func TestCorrectionEngineYELLOW_EditedDeclaration(t *testing.T) {
	for _, body := range []string{"Corrects: #99", "Corrects: none", "no declaration", "Corrects: malformed"} {
		t.Run(body, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			o := Outcome{Developer: "child", Repo: "acme/app", PRNumber: 20, MergeCommitSHA: "child-sha"}
			id := recordCorrection(t, db, o, "Corrects: #10", "first evidence")
			got, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "edited evidence", "edited-fetch")
			if got != id || !errors.Is(err, ErrCorrectionDeclarationIgnored) || !strings.Contains(err.Error(), "unchanged-ignored") {
				t.Errorf("edited declaration status: id=%d error=%v", got, err)
			}
			assertCount(t, db, `SELECT COUNT(*) FROM correction_declarations WHERE supporting_text='first evidence' AND provenance='test-fetch-time' AND declaration_state='parents'`, 1)
			assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations WHERE parent_pr_number=10`, 1)
			assertCount(t, db, `SELECT COUNT(*) FROM corrective_relations`, 1)
			assertCount(t, db, `SELECT COUNT(*) FROM correction_audit`, 1)
		})
	}
}

func TestCorrectionEngineGREEN_ArgsCountMarks(t *testing.T) {
	// Question marks in SQL text are not repetitions of the two-subject marks.
	got := correctionArgs(`developer IN (?,?) OR author_developer IN (?,?) OR note='??'`, "?,?", []any{"canonical", "alias"})
	want := []any{"canonical", "alias", "canonical", "alias"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bound args=%v want %v", got, want)
	}
}
