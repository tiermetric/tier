package webhook

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/issueref/issuereftest"
	"github.com/tiermetric/tier/internal/store"
)

func TestStrictGrammarSharedProducerFixtures(t *testing.T) {
	g, err := issueref.NewGrammar(issuereftest.Keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range issuereftest.Fixtures() {
		t.Run(tc.Name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			h := New(db, testSecret, quietLogger(), WithGrammar(g))
			body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]any{"full_name": "owner/repo"}, "pull_request": map[string]any{"number": 733, "body": tc.Text, "merged": true, "merge_commit_sha": "strict", "head": map[string]string{"ref": tc.Branch}, "user": map[string]string{"login": "alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.handlePR(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			o, ok, err := db.OutcomeByMergeCommit(context.Background(), "strict")
			if err != nil {
				t.Fatal(err)
			}
			pushStore := newFakeStore()
			pushHandler := New(pushStore, testSecret, quietLogger(), WithGrammar(g), WithPushCapture(&fakePushCounter{}))
			dispatchPush(t, pushHandler, "refs/heads/main", "main", pushCommit("fixture-push", tc.Text, "alice", time.Now().UTC()))
			pushOutcomes := pushStore.pushSource()
			if tc.Commit.ID == "" {
				if len(pushOutcomes) != 0 {
					t.Fatalf("unexpected push outcomes: %+v", pushOutcomes)
				}
			} else if len(pushOutcomes) != 1 || (issueref.Ref{ID: pushOutcomes[0].IssueID, Rule: pushOutcomes[0].IssueRule}) != tc.Commit {
				t.Fatalf("push outcomes %+v want %+v", pushOutcomes, tc.Commit)
			}
			if tc.Revert || tc.PR.ID == "" {
				if ok {
					t.Fatal("unexpected outcome", o)
				}
				return
			}
			if !ok || (issueref.Ref{ID: o.IssueID, Rule: o.IssueRule}) != tc.PR {
				t.Fatalf("outcome %+v found=%v want %+v", o, ok, tc.PR)
			}
		})
	}
}

func TestStrictPushConfiguredKeys(t *testing.T) {
	g, err := issueref.NewGrammar("PROJ")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ subject, id string }{
		{"fix: PROJ-123", "PROJ-123"}, {"fix: OP-2024", "OP-2024"}, {"fix: OTHER-9", ""}, {"refs #42", ""}, {"fix: widget (#42)", ""}, {"fix: widget\n\nPROJ-123", ""}, {"`PROJ-123`", ""},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			st := newFakeStore()
			counter := &fakePushCounter{}
			h := New(st, testSecret, quietLogger(), WithGrammar(g), WithPushCapture(counter))
			dispatchPush(t, h, "refs/heads/main", "main", pushCommit("strict-push", tc.subject, "alice", time.Now().UTC()))
			outcomes := st.pushSource()
			if tc.id == "" {
				if len(outcomes) != 0 || counter.count() != 1 {
					t.Fatal(outcomes, counter.count())
				}
				return
			}
			if len(outcomes) != 1 || outcomes[0].IssueID != tc.id || outcomes[0].IssueRule != issueref.RuleCommitConfiguredKey {
				t.Fatal(outcomes)
			}
		})
	}
}

func TestStrictRevertConfiguredSubjectFallback(t *testing.T) {
	g, err := issueref.NewGrammar("PROJ")
	if err != nil {
		t.Fatal(err)
	}
	r := newReconcileHarness(t)
	r.h = New(r.db, testSecret, quietLogger(), WithGrammar(g))
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := r.db.RecordPROutcome(context.Background(), store.Outcome{Developer: "alice", IssueID: "PROJ-42", Repo: reconcileRepo, PRNumber: 7, Weight: 1, Quality: 1, MergeCommitSHA: wfMergeSHA, Timestamp: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r.push(t, pushCommit("strict-revert", `Revert "feat PROJ-42"`, "bob", now))
	o, ok, err := r.db.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
	if err != nil || !ok || o.Quality != 0.1 {
		t.Fatal(o, ok, err)
	}
}
