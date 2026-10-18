package webhook

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func TestWebhookStoresIssueRule(t *testing.T) {
	for _, tc := range []struct {
		branch, body, id string
		rule             issueref.Rule
	}{
		{"feature/42", "closes #9", "issue-42", issueref.RuleBranchFirstSlot},
		{"fix/PROJ-9", "closes #42", "issue-42", issueref.RuleBodyCloseDirective},
		{"feature/topic", "closes #42", "issue-42", issueref.RuleBodyCloseDirective},
		{"feature/topic", "Use UTF-8", "", ""},
		{"feature/topic", "PR #446", "", ""},
	} {
		t.Run(string(tc.rule), func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			h := New(db, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}))
			body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]any{"full_name": "acme/app"}, "pull_request": map[string]any{"number": 42, "body": tc.body, "merged": true, "merge_commit_sha": "merge", "head": map[string]string{"ref": tc.branch}, "user": map[string]string{"login": "alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err = h.handlePR(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			o, ok, err := db.OutcomeByMergeCommit(context.Background(), "merge")
			if err != nil || ok != (tc.id != "") || o.IssueID != tc.id || o.IssueRule != tc.rule {
				t.Fatalf("%+v %v %v", o, ok, err)
			}
			ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			dispatchPush(t, h, "refs/heads/main", "main", pushCommit("close-sha", "closes #99", "alice", ts), pushCommit("tracker-sha", "fix: PROJ-12", "alice", ts))
			rows, _, err := db.ListOutcomes(context.Background(), time.Time{}, time.Now().Add(time.Hour), store.PageCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, r := range rows {
				if r.Source != store.OutcomeSourcePush {
					continue
				}
				seen++
				want := issueref.RuleCommitCloseDirective
				if r.IssueRule != want {
					t.Fatal(r.IssueID, r.IssueRule, want)
				}
			}
			if seen != 1 {
				t.Fatal(rows)
			}
			g, err := issueref.NewGrammar("PROJ")
			if err != nil {
				t.Fatal(err)
			}
			configured := New(db, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}), WithGrammar(g))
			dispatchPush(t, configured, "refs/heads/main", "main", pushCommit("configured-tracker-sha", "fix: PROJ-12", "alice", ts))
			rows, _, err = db.ListOutcomes(context.Background(), time.Time{}, time.Now().Add(time.Hour), store.PageCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range rows {
				if r.Source == store.OutcomeSourcePush && r.IssueID == "PROJ-12" {
					found = true
					if r.IssueRule != issueref.RuleCommitConfiguredKey {
						t.Fatal(r)
					}
				}
			}
			if !found {
				t.Fatal("configured tracker push missing", rows)
			}
		})
	}
}
