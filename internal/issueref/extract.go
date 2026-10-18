// Package issueref provides shared issue ID extraction from branch names and
// commit/PR messages. Used by both the JSONL collector and the webhook handler
// to ensure consistent attribution across all data paths.
package issueref

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/tiermetric/tier/internal/corrects"
)

// Year-guard window. A bare 4-digit branch segment inside this inclusive range
// is treated as a calendar year (e.g. "release/2024-fix") rather than an issue
// number, because release/date-stamped branches are far more common than issues
// numbered in the ~1900–2099 band. See the tradeoff note in FromBranch.
const (
	yearGuardLow  = 1900
	yearGuardHigh = 2099
)

const mainlineBranches = `^(?:main|master|HEAD)?$`
const issueTerminator = `(?:$|[\s\p{P}])`

var (
	mainlineRE = regexp.MustCompile(mainlineBranches)
	commitPRRE = regexp.MustCompile(`(?:^|[\s(])#([1-9]\d*)` + issueTerminator)
	// closesRE matches "closes #42", "fixes #99", "resolves #7" (case-insensitive).
	// The captured number uses [1-9]\d* so a leading zero or bare "#0" never
	// yields a bogus issue id — GitHub issue numbers are >= 1 and never
	// zero-padded (matches spaceHashIssueRE's guard).
	closesRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?)(?:\s+|:\s*)#([1-9]\d*)` + issueTerminator)
	// closeClauseRE matches a close directive and the WHOLE run of issue refs that
	// belong to it (captured group 1): "closes #12", "closes #12, #15",
	// "fixes #12 and #15", "resolves #12 #15". Separators inside the run are a
	// comma, the word "and", or bare whitespace, so both the GitHub-strict
	// "closes #a, closes #b" (two clauses) and the shorthand "closes #a, #b" (one
	// clause, two refs) enumerate. ClosedIssues then pulls each #N out of the run.
	// The run stops at the first token that is not a #N ref, so trailing prose is
	// never swept in.
	closeClauseRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?)(?:\s+|:\s*)(#[1-9]\d*(?:(?:\s*,\s*|\s+and\s+|\s+)#[1-9]\d*)*)` + issueTerminator)
	// hashNumRE extracts each "#N" issue number from a close-clause run.
	hashNumRE = regexp.MustCompile(`#([1-9]\d*)` + issueTerminator)
)

// IsExploratoryBranch identifies branches with no feature identity. These
// branches never inherit a nearby commit's issue in the collector.
func IsExploratoryBranch(branch string) bool {
	branch = strings.TrimSpace(branch)
	return mainlineRE.MatchString(branch)
}

// FromBranch uses the default-empty tracker configuration.
func FromBranch(branch string) string             { return (Grammar{}).FromBranch(branch) }
func FromBranchRef(branch string) Ref             { return (Grammar{}).FromBranchRef(branch) }
func FromPRBody(body string) string               { return (Grammar{}).FromPRBody(body) }
func FromPRBodyRef(body string) Ref               { return (Grammar{}).FromPRBodyRef(body) }
func FromCommitSubject(subject string) string     { return (Grammar{}).FromCommitSubject(subject) }
func FromCommitSubjectRef(subject string) Ref     { return (Grammar{}).FromCommitSubjectRef(subject) }
func FromBranchOrBody(branch, body string) string { return (Grammar{}).FromBranchOrBody(branch, body) }
func FromBranchOrBodyRef(branch, body string) Ref {
	return (Grammar{}).FromBranchOrBodyRef(branch, body)
}

// FromCommitSubjectPR captures PR metadata only when the subject has no issue evidence.
func FromCommitSubjectPR(subject string) *int { return (Grammar{}).FromCommitSubjectPR(subject) }
func (g Grammar) FromCommitSubjectPR(subject string) *int {
	subject = corrects.StripDeclarationLines(subject)
	if g.FromCommitSubject(subject) != "" {
		return nil
	}
	m := commitPRRE.FindStringSubmatch(subject)
	if len(m) != 2 {
		return nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return &n
}

// ClosedIssues enumerates closing references left to right, deduplicated.
// Corrects declarations never contribute. Enumeration is independent of the
// attribution grammar's Markdown sanitisation and branch precedence.
func ClosedIssues(body string) []string {
	body = corrects.StripDeclarationLines(body)
	var out []string
	seen := map[string]bool{}
	for _, clause := range closeClauseRE.FindAllStringSubmatch(body, -1) {
		for _, ref := range hashNumRE.FindAllStringSubmatch(clause[1], -1) {
			id := "issue-" + ref[1]
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// A four-digit first slot in this window is not numeric issue evidence.
func isLikelyYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	return n >= yearGuardLow && n <= yearGuardHigh
}
