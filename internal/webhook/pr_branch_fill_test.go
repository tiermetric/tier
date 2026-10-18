package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func TestPRHeadBranchHashStoredWithoutRawRef(t *testing.T) {
	for _, branch := range []string{"feature/private-topic", "refs/heads/feature/private-topic", "main", "master", "HEAD", "trunk", "refs/heads/trunk", ""} {
		t.Run(branch, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tier.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			merged := time.Date(2026, 6, 10, 12, 0, 0, 123456789, time.UTC)
			opened := merged.Add(-time.Hour)
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: store.UnattributedIssueID, Model: "claude-sonnet-4", Source: "jsonl", Timestamp: opened, BranchHash: issueref.BranchHash("acme/a", branch)}); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]any{"full_name": "Acme/A", "default_branch": "trunk"}, "pull_request": map[string]any{"number": 42, "body": "closes #42", "merged": true, "merged_at": merged.Format(time.RFC3339Nano), "created_at": opened.Format(time.RFC3339Nano), "merge_commit_sha": "merged", "head": map[string]any{"ref": branch, "repo": map[string]string{"full_name": "Acme/A"}}, "user": map[string]string{"login": "alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := New(db, testSecret, quietLogger()).handlePR(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			var hash sql.NullString
			var created, merge time.Time
			if err := raw.QueryRow(`SELECT head_branch_hash, created_at, merged_at FROM outcomes`).Scan(&hash, &created, &merge); err != nil {
				t.Fatal(err)
			}
			want := issueref.BranchHash("acme/a", branch)
			if strings.TrimPrefix(branch, "refs/heads/") == "trunk" {
				want = nil
			}
			if (want == nil && hash.Valid) || (want != nil && (!hash.Valid || hash.String != *want)) || !created.Equal(opened) || !merge.Equal(merged) {
				t.Fatalf("metadata: hash=%v created=%v merged=%v", hash, created, merge)
			}
			var issue string
			if err := raw.QueryRow(`SELECT issue_id FROM token_events`).Scan(&issue); err != nil {
				t.Fatal(err)
			}
			wantIssue := store.UnattributedIssueID
			if want != nil {
				wantIssue = "issue-42"
			}
			if issue != wantIssue {
				t.Fatalf("issue=%s want=%s", issue, wantIssue)
			}
			// Inspect every outcome column's actual contents, not just its schema.
			rows, err := raw.Query(`SELECT * FROM outcomes`)
			if err != nil {
				t.Fatal(err)
			}
			cols, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			values, dest := make([]any, len(cols)), make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if !rows.Next() {
				t.Fatal("missing outcome")
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			for i, v := range values {
				if branch != "" && strings.Contains(fmt.Sprint(v), branch) {
					t.Errorf("raw head ref in column %s: %v", cols[i], v)
				}
			}
			_ = rows.Close()
		})
	}
}

func TestPRBranchEvidenceRepoAndCustomDefault(t *testing.T) {
	for _, tc := range []struct {
		name, headRepo, branch, defaultBranch string
		wantHash                              bool
	}{
		{"fork_same_named_branch", "fork/a", "feature/topic", "main", false},
		{"missing_head_repo", "", "feature/topic", "main", false},
		{"same_repo_case_insensitive", "ACME/A", "feature/topic", "main", true},
		{"custom_default", "Acme/A", "trunk", "trunk", false},
		{"custom_default_prefixed", "Acme/A", "refs/heads/trunk", "trunk", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			merged := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
			opened := merged.Add(-time.Hour)
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: store.UnattributedIssueID, Model: "claude-sonnet-4", Source: "jsonl", Timestamp: opened, BranchHash: issueref.BranchHash("acme/a", tc.branch)}); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]string{"full_name": "Acme/A", "default_branch": "main"}, "pull_request": map[string]any{"number": 42, "body": "closes #42", "merged": true, "merged_at": merged.Format(time.RFC3339Nano), "created_at": opened.Format(time.RFC3339Nano), "merge_commit_sha": "evidence", "user": map[string]string{"login": "alice"}, "head": map[string]any{"ref": tc.branch, "repo": map[string]string{"full_name": tc.headRepo}}, "base": map[string]any{"repo": map[string]string{"default_branch": tc.defaultBranch}}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := New(db, testSecret, quietLogger()).handlePR(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			exp, err := db.ExportDeveloper(context.Background(), "alice")
			if err != nil || len(exp.Outcomes) != 1 || len(exp.TokenEvents) != 1 {
				t.Fatalf("export: %+v %v", exp, err)
			}
			o := exp.Outcomes[0]
			if (o.HeadBranchHash != nil) != tc.wantHash {
				t.Errorf("hash=%v want present=%v", o.HeadBranchHash, tc.wantHash)
			}
			want := store.UnattributedIssueID
			if tc.wantHash {
				want = "issue-42"
			}
			if exp.TokenEvents[0].IssueID != want {
				t.Errorf("event issue=%s want=%s", exp.TokenEvents[0].IssueID, want)
			}
		})
	}
}

func TestRevertBranchesNeverFillRemainder(t *testing.T) {
	for _, branch := range []string{"Revert-12-x", "revert-foo", "REVERT/foo", "refs/heads/Revert-foo"} {
		t.Run(branch, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			merged := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
			opened := merged.Add(-time.Hour)
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: store.UnattributedNoIssueBucket, Model: "claude-sonnet-4", Source: "jsonl", Timestamp: opened, BranchHash: issueref.BranchHash("acme/a", branch)}); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]string{"full_name": "acme/a", "default_branch": "main"}, "pull_request": map[string]any{"number": 42, "body": "closes #42", "merged": true, "merged_at": merged.Format(time.RFC3339), "created_at": opened.Format(time.RFC3339), "merge_commit_sha": "revert-fill", "head": map[string]any{"ref": branch, "repo": map[string]string{"full_name": "acme/a"}}, "user": map[string]string{"login": "alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := New(db, testSecret, quietLogger()).handlePR(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			exp, err := db.ExportDeveloper(context.Background(), "alice")
			if err != nil || len(exp.TokenEvents) != 1 || exp.TokenEvents[0].IssueID != store.UnattributedNoIssueBucket {
				t.Fatalf("revert remainder %+v err=%v", exp.TokenEvents, err)
			}
			for _, o := range exp.Outcomes {
				if o.HeadBranchHash != nil {
					t.Fatal("revert branch supplied fill evidence", o)
				}
			}
		})
	}
}
