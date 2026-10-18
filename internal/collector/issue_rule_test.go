package collector

import (
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func TestCollectorIssueRuleSelection(t *testing.T) {
	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, branch string
		commits      []gitCommit
		want         issueref.Ref
	}{
		{"numeric fallback", "feature/42", nil, issueref.Ref{ID: "issue-42", Rule: issueref.RuleBranchFirstSlot}},
		{"tracker fallback", "fix/UTF-8", nil, issueref.Ref{ID: UnattributedNoIssue}},
		{"subject match", "feature/topic", []gitCommit{{Timestamp: ts, Branches: []string{"feature/topic"}, IssueID: "issue-9", IssueRule: issueref.RuleCommitCloseDirective}}, issueref.Ref{ID: "issue-9", Rule: issueref.RuleCommitCloseDirective}},
		{"own branch tie", "feature/42", []gitCommit{{Timestamp: ts, Branches: []string{"fix/PROJ-9", "feature/42"}, IssueID: "PROJ-9", IssueRule: issueref.RuleBranchTrackerShape}}, issueref.Ref{ID: "issue-42", Rule: issueref.RuleBranchFirstSlot}},
		{"bucket", "feature/topic", nil, issueref.Ref{ID: UnattributedNoIssue}},
		{"main skips commit", "main", []gitCommit{{Timestamp: ts, Branches: []string{"main"}, IssueID: "issue-9", IssueRule: issueref.RuleCommitCloseDirective}}, issueref.Ref{ID: UnattributedMain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grammar, err := issueref.NewGrammar("PROJ")
			if err != nil {
				t.Fatal(err)
			}
			resolver := &IssueResolver{grammar: grammar, commitsByLeaf: indexCommitsByLeaf(tc.commits)}
			if got := resolver.ResolveRef(tc.branch, ts); got != tc.want {
				t.Fatalf("Ref=%+v want %+v", got, tc.want)
			}
			events := joinSessionsToCommits([]sessionSummary{{GitBranch: tc.branch, Messages: []messageUsage{{gitBranch: tc.branch, timestamp: ts, model: "claude-sonnet-4", input: 10}}}}, tc.commits, "alice", "acme/app", grammar)
			if len(events) != 1 || events[0].IssueID != tc.want.ID || events[0].IssueRule != tc.want.Rule {
				t.Fatalf("events=%+v want %+v", events, tc.want)
			}
		})
	}
	t.Run("foreign verdict", func(t *testing.T) {
		events := joinSessionsToCommits([]sessionSummary{{
			GitBranch:   "feature/42",
			Messages:    []messageUsage{{messageID: "foreign", gitBranch: "feature/42", timestamp: ts, model: "claude-sonnet-4", input: 10}},
			attribution: map[string]messageAttribution{"foreign": {Rule: store.AttributionRuleWorktreeToolPath, Branch: "feature/42", Foreign: true}},
		}}, nil, "alice", "acme/app", issueref.Grammar{})
		if len(events) != 1 || events[0].IssueID != UnattributedForeignRepo || events[0].IssueRule != "" {
			t.Fatalf("foreign events=%+v", events)
		}
	})
	var resolver *IssueResolver
	if got := resolver.ResolveRef("feature/42", ts); got != (issueref.Ref{ID: UnattributedNoIssue}) {
		t.Fatal(got)
	}
}
