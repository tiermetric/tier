package issueref

import (
	"slices"
	"testing"
)

func TestLegacyRules(t *testing.T) {
	for _, c := range []struct {
		input   string
		extract func(string) Ref
		want    Ref
	}{
		{"docs/notes-2026-10-14", FromBranchRef, Ref{}},
		{"fix/UTF-8", FromBranchRef, Ref{}},
		{"closes #42 PROJ-9", FromPRBodyRef, Ref{"issue-42", RuleBodyCloseDirective}},
		{"Use UTF-8 #42", FromPRBodyRef, Ref{}},
		{"PR #446", FromPRBodyRef, Ref{}},
		{"fix: closes #42 PROJ-9", FromCommitSubjectRef, Ref{"issue-42", RuleCommitCloseDirective}},
		{"fix: PROJ-9 (#42)", FromCommitSubjectRef, Ref{}},
		{"main", FromBranchRef, Ref{}}, {"see #42", FromCommitSubjectRef, Ref{}},
	} {
		t.Run(c.input, func(t *testing.T) {
			if got := c.extract(c.input); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
	if got := FromBranchOrBodyRef("feature/42", "closes #9"); got != (Ref{"issue-42", RuleBranchFirstSlot}) {
		t.Fatal(got)
	}
}
func TestRulesClosedSet(t *testing.T) {
	rules := Rules()
	if len(rules) != 12 {
		t.Fatal(rules)
	}
	for i, r := range rules {
		if r == "" || slices.Contains(rules[:i], r) || Validate(r) != nil {
			t.Fatal(r)
		}
	}
	for _, r := range []Rule{"legacy", "unknown", "Branch_first_numeric", "branch_first_numeric ", "/private/path"} {
		if Validate(r) == nil {
			t.Fatal(r)
		}
	}
	if Validate("") != nil {
		t.Fatal("empty rejected")
	}
}
