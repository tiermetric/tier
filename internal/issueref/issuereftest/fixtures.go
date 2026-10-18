// Package issuereftest holds the shared producer parity fixtures.
package issuereftest

import "github.com/tiermetric/tier/internal/issueref"

const Keys = "TIER,JIRA,PROJ"

type Fixture struct {
	Name, Branch, Text string
	PR, Spend, Commit  issueref.Ref
	Revert             bool
}

func Fixtures() []Fixture {
	numeric := issueref.Ref{ID: "issue-42", Rule: issueref.RuleBranchFirstSlot}
	body := issueref.Ref{ID: "issue-42", Rule: issueref.RuleBodyCloseDirective}
	commit := issueref.Ref{ID: "issue-42", Rule: issueref.RuleCommitCloseDirective}
	fixtures := []Fixture{
		{Name: "numeric", Branch: "feature/42-auth", Text: "closes #9", PR: numeric, Spend: numeric, Commit: issueref.Ref{ID: "issue-9", Rule: issueref.RuleCommitCloseDirective}},
		{Name: "configured", Branch: "fix/TIER-99-crash", PR: issueref.Ref{ID: "TIER-99", Rule: issueref.RuleBranchConfiguredKey}, Spend: issueref.Ref{ID: "TIER-99", Rule: issueref.RuleBranchConfiguredKey}},
		{Name: "built-in", Branch: "feature/OP-2024-x", PR: issueref.Ref{ID: "OP-2024", Rule: issueref.RuleBranchConfiguredKey}, Spend: issueref.Ref{ID: "OP-2024", Rule: issueref.RuleBranchConfiguredKey}},
		{Name: "revert-component-fallback", Branch: "revert/42", Text: "closes #42", PR: body, Commit: commit},
		{Name: "body-and-subject-fallback", Branch: "docs/notes-2026-10-14", Text: "closes #42", PR: body, Spend: commit, Commit: commit},
		{Name: "structured-key-fallback", Branch: "fix/audit-s09-6-x", Text: "implements PROJ-123", PR: issueref.Ref{ID: "PROJ-123", Rule: issueref.RuleBodyConfiguredKey}, Spend: issueref.Ref{ID: "PROJ-123", Rule: issueref.RuleCommitConfiguredKey}, Commit: issueref.Ref{ID: "PROJ-123", Rule: issueref.RuleCommitConfiguredKey}},
		{Name: "relation-body-only", Branch: "feature/topic", Text: "refs #42", PR: issueref.Ref{ID: "issue-42", Rule: issueref.RuleBodyRelationRef}},
		{Name: "free-key-subject-only", Branch: "feature/topic", Text: "use PROJ-123", Spend: issueref.Ref{ID: "PROJ-123", Rule: issueref.RuleCommitConfiguredKey}, Commit: issueref.Ref{ID: "PROJ-123", Rule: issueref.RuleCommitConfiguredKey}},
		{Name: "bare-PR", Branch: "feature/topic", Text: "fix: widget (#446)"},
		{Name: "unconfigured", Branch: "fix/UTF-8", Text: "Use UTF-8"},
		{Name: "unconfigured-structured", Branch: "fix/OTHER-42", Text: "implements OTHER-42"},
		{Name: "quoted", Branch: "feature/topic", Text: "> closes #42"},
		{Name: "inline-code", Branch: "feature/topic", Text: "`closes #42`"},
		{Name: "fenced-code", Branch: "feature/topic", Text: "~~~\ncloses #42\n~~~"},
		{Name: "correction-only", Branch: "feature/topic", Text: "Corrects: #733"},
		{Name: "revert-with-fallback", Branch: "revert-45-fix/123-x", Text: "closes #42", Revert: true, Commit: commit},
	}
	for _, branch := range []string{"docs/notes-2026-07-23", "fix/audit-s09-6-x", "fix/435b-auth-50051-cutover", "dependabot/docker/node-26-alpine", "release/2024-42", "release/2024-hotfix-42", "feature/007-42", "claude/sweet-hopper-3", "fix/utf-8-handling", "chore/node-22-upgrade", "chore/bump-go-1-27"} {
		fixtures = append(fixtures, Fixture{Name: branch, Branch: branch})
	}
	return fixtures
}
