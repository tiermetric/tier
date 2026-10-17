package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/tiermetric/tier/internal/corrects"
	"path/filepath"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

func correctionPRBody(t *testing.T, text, head string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action": "closed", "repository": map[string]string{"full_name": "acme/app"},
		"pull_request": map[string]any{
			"number": 22, "merged": true, "merge_commit_sha": "child-merge", "body": text,
			"merged_at": "2026-10-01T12:00:00Z", "created_at": "2026-09-30T12:00:00Z",
			"additions": 0, "deletions": nil, "changed_files": 1,
			"head": map[string]string{"ref": head}, "user": map[string]string{"login": "alice"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestHandlePR_CorrectsRecordedBeforeNoIssueReturn(t *testing.T) {
	for text, state := range map[string]string{"Corrects: #11": "parents", "Corrects: none": "none", "Corrects: nonsense": "invalid", "Corrects: #22": "invalid", "ordinary prose": "missing"} {
		t.Run(text, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()
			h := New(db, testSecret, quietLogger())
			for i := 0; i < 2; i++ {
				if err := h.handlePR(ctx, correctionPRBody(t, text, "repair")); err != nil {
					t.Fatal(err)
				}
			}
			d, found, err := db.CorrectionDeclarationByPR(ctx, "acme/app", 22, "child-merge")
			if err != nil || !found {
				t.Fatalf("declaration lost before no-issue return: found=%v err=%v", found, err)
			}
			if d["declaration_state"] != state || d["outcome_id"] != nil || d["merged_at"] == nil || d["opened_at"] == nil || d["additions"] != int64(0) || d["deletions"] != nil || d["changed_files"] != int64(1) || d["provenance"] != "github-webhook:merge-delivery" {
				t.Fatalf("declaration evidence: %+v", d)
			}
			if _, found, err := db.OutcomeByMergeCommit(ctx, "child-merge"); err != nil || found {
				t.Fatalf("invented outcome: %v %v", found, err)
			}
		})
	}
}

func TestCorrectionChild_RecognizedRevertRemainsExcluded(t *testing.T) {
	for name, heads := range map[string][]string{
		"revert-first":   {"revert-11-repair", "repair"},
		"credited-first": {"repair", "revert-11-repair", "repair"},
	} {
		t.Run(name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()
			h := New(db, testSecret, quietLogger())
			for _, head := range heads {
				if err := h.handlePR(ctx, correctionPRBody(t, "Corrects: #11\nCloses #33", head)); err != nil {
					t.Fatal(err)
				}
			}
			if _, found, err := db.CorrectionDeclarationByPR(ctx, "acme/app", 22, "child-merge"); err != nil || !found {
				t.Fatalf("excluded child's declaration missing: %v %v", found, err)
			}
			rels, err := db.CorrectionRelationsByPR(ctx, "acme/app", 22)
			if err != nil || len(rels) != 1 || rels[0]["state"] != "excluded" || rels[0]["resolution_reason"] != "excluded-child" || rels[0]["child_outcome_id"] != nil {
				t.Fatalf("excluded relation: %+v %v", rels, err)
			}
			if _, found, err := db.OutcomeByMergeCommit(ctx, "child-merge"); err != nil || found {
				t.Fatalf("revert resurrected: %v %v", found, err)
			}
			exp, err := db.ExportDeveloper(ctx, "alice")
			if err != nil || len(exp.OutcomeExclusions) != 1 {
				t.Fatalf("exclusion count: %+v %v", exp.OutcomeExclusions, err)
			}
		})
	}
}

func TestHandlePRCorrectionReplay_ExistingOutcomeGetsDeclaration(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	o := store.Outcome{Developer: "alice", Repo: "acme/app", PRNumber: 22, MergeCommitSHA: "child-merge", IssueID: "issue-99", Weight: 5, Quality: 0.7}
	if inserted, err := db.RecordPROutcome(ctx, o); err != nil || !inserted {
		t.Fatalf("seed: %v %v", inserted, err)
	}
	h := New(db, testSecret, quietLogger())
	for _, text := range []string{"Corrects: #11", "Corrects: #11\nCloses #33"} {
		if err := h.handlePR(ctx, correctionPRBody(t, text, "repair")); err != nil {
			t.Fatal(err)
		}
	}
	stored, found, err := db.OutcomeByMergeCommit(ctx, o.MergeCommitSHA)
	if err != nil || !found || stored.Weight != o.Weight || stored.Quality != o.Quality || stored.IssueID != o.IssueID {
		t.Fatalf("replay changed outcome: %+v %v", stored, err)
	}
	d, found, err := db.CorrectionDeclarationByPR(ctx, o.Repo, o.PRNumber, o.MergeCommitSHA)
	if err != nil || !found || d["outcome_id"] != stored.ID {
		t.Fatalf("missing replay evidence: %+v %v", d, err)
	}
}

func TestHandlePRCorrectionReplay_ChangedDeclarationContinues(t *testing.T) {
	for _, head := range []string{"repair", "revert-11-repair"} {
		t.Run(head, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()
			o := store.Outcome{Developer: "alice", Repo: "acme/app", PRNumber: 22, MergeCommitSHA: "child-merge"}
			id, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse("Corrects: #11"), "first evidence", "first-fetch")
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			h := New(db, testSecret, slog.New(slog.NewTextHandler(&logs, nil)))
			body := correctionPRBody(t, "Corrects: #12\nCloses #33", head)
			req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			outcome, found, err := db.OutcomeByMergeCommit(ctx, "child-merge")
			if err != nil || found != (head == "repair") {
				t.Fatalf("outcome=%+v found=%v err=%v", outcome, found, err)
			}
			if found && (outcome.Quality != 1 || outcome.IssueID != "issue-33") {
				t.Fatalf("changed attribution/quality: %+v", outcome)
			}
			d, found, err := db.CorrectionDeclarationByPR(ctx, o.Repo, o.PRNumber, o.MergeCommitSHA)
			if err != nil || !found || d["id"] != id || d["supporting_text"] != "first evidence" || d["provenance"] != "first-fetch" {
				t.Fatalf("first evidence lost: %+v %v", d, err)
			}
			rels, err := db.CorrectionRelationsByPR(ctx, o.Repo, o.PRNumber)
			if err != nil || len(rels) != 1 || rels[0]["parent_pr_number"] != int64(11) {
				t.Fatalf("first parents lost: %+v %v", rels, err)
			}
			if head != "repair" && rels[0]["state"] != "excluded" {
				t.Fatalf("revert not excluded: %+v", rels)
			}
			if !strings.Contains(logs.String(), `pr="\"22\""`) {
				t.Fatalf("missing PR log: %s", logs.String())
			}
		})
	}
}

func TestHandlePRCorrectionReplay_WrappedIgnoredIsNonfatal(t *testing.T) {
	s := newFakeStore()
	s.correctionErr = fmt.Errorf("replay: %w", store.ErrCorrectionDeclarationIgnored)
	h := New(s, testSecret, quietLogger())
	if err := h.handlePR(context.Background(), correctionPRBody(t, "Corrects: #12\nCloses #33", "repair")); err != nil {
		t.Fatal(err)
	}
	if len(s.outcomes) != 1 {
		t.Fatalf("outcomes=%d", len(s.outcomes))
	}
	s.correctionErr = fmt.Errorf("capture failed")
	if err := h.handlePR(context.Background(), correctionPRBody(t, "Closes #33", "repair")); err == nil {
		t.Fatal("unexpected capture failure swallowed")
	}
}
