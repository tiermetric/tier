package issueref

import (
	"reflect"
	"testing"
)

// TestClosedIssues enumerates every issue a PR body closes, in deterministic
// left-to-right order, deduplicated (#189). The forms below are exactly the
// multi-issue shapes a PR author writes: comma-separated after one keyword,
// "and"-joined, space-separated, one keyword per issue, and mixed keywords.
func TestClosedIssues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"single close", "closes #42", []string{"issue-42"}},
		{"numeric prefix", "closes #42x", nil},
		{"invalid second ref", "closes #12, #15abc", []string{"issue-12"}},
		{"invalid first ref", "closes #12abc, #15", nil},
		{"valid later directive", "fixed #12abc; closes #42", []string{"issue-42"}},
		{"colon multiple refs", "fixes:#12, #15 and #18", []string{"issue-12", "issue-15", "issue-18"}},
		{"close", "close #12", []string{"issue-12"}},
		{"fix", "fix #12", []string{"issue-12"}},
		{"resolve", "resolve #12", []string{"issue-12"}},
		{"closed", "closed #12", []string{"issue-12"}},
		{"fixed", "fixed #12", []string{"issue-12"}},
		{"resolved", "resolved #12", []string{"issue-12"}},
		{"past tense case insensitive", "ClOsEd #12, FiXeD #15, ReSoLvEd #18", []string{"issue-12", "issue-15", "issue-18"}},
		{"prefixes", "prefixes #909", nil},
		{"discloses", "discloses #909", nil},
		{"comma one keyword", "closes #12, #15", []string{"issue-12", "issue-15"}},
		{"space separated", "closes #12 #15", []string{"issue-12", "issue-15"}},
		{"and joined", "fixes #12 and #15", []string{"issue-12", "issue-15"}},
		{"keyword per issue", "closes #12, closes #15", []string{"issue-12", "issue-15"}},
		{"mixed keywords", "fixes #7 resolves #9", []string{"issue-7", "issue-9"}},
		{"three refs one clause", "closes #1, #2 and #3", []string{"issue-1", "issue-2", "issue-3"}},
		{"dedup repeated ref", "closes #5, closes #5", []string{"issue-5"}},
		{"none", "just a description with no refs", nil},
		{"bare ref is not a close", "see #99 for context", nil},
		{"markdown heading ignored", "## Section 42", nil},
		{"case insensitive", "CLOSES #8", []string{"issue-8"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClosedIssues(c.body)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ClosedIssues(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestClosedIssuesPrimaryMatchesFromPRBody: the PRIMARY (attributed) issue is the
// FIRST element of ClosedIssues, and it must agree with what FromPRBody selects —
// the documented rule is "attribute the one outcome to the primary (leftmost)
// closed issue" (#189). If these ever diverged, the logged secondaries would be
// inconsistent with the credited issue.
func TestClosedIssuesPrimaryMatchesFromPRBody(t *testing.T) {
	bodies := []string{
		"closes #12, #15",
		"closes #12, closes #15",
		"fixes #7 and #9",
		"resolves #3",
	}
	for _, b := range bodies {
		closed := ClosedIssues(b)
		if len(closed) == 0 {
			t.Fatalf("ClosedIssues(%q) empty, expected at least one", b)
		}
		if got := FromPRBody(b); got != closed[0] {
			t.Errorf("FromPRBody(%q) = %q, want primary %q (= ClosedIssues[0])", b, got, closed[0])
		}
	}
}
