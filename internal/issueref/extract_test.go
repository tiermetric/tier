package issueref

import "testing"

func TestFromBranch(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   string
	}{
		// Existing behavior — must stay green.
		{"slug-suffixed", "feature/42-auth-rewrite", "issue-42"},
		{"tracker-key-nested", "fix/TIER-123-something", "TIER-123"},
		{"tracker-key-leading", "JIRA-456-description", "JIRA-456"},
		{"main", "main", ""},
		{"master", "master", ""},
		{"head", "HEAD", ""},
		{"empty", "", ""},
		{"no-issue", "feature/no-issue", ""},
		// Gap 1 — tail issue numbers with no trailing separator.
		{"tail-number", "feature/42", "issue-42"},
		{"tail-number-4-digit-nonyear", "bugfix/1234", "issue-1234"},
		{"tail-number-underscore", "feature/42_auth", "issue-42"},
		{"tail-number-five-digit", "feature/12345", "issue-12345"},
		// Gap 2 — year-like segments must not be mis-attributed.
		{"year-prefixed", "release/2024-fix", ""},
		{"year-tail", "release/2024", ""},
		{"year-lower-bound-1900", "release/1900", ""},
		{"year-upper-bound-2099", "release/2099", ""},
		// Boundary: 4-digit values outside the year window are real issues.
		{"nonyear-below-window", "feature/1899", "issue-1899"},
		{"nonyear-above-window", "feature/2100", "issue-2100"},
		// A tracker key with a year-looking number is explicit → not guarded.
		{"tracker-key-year-number", "fix/REL-2024-hotfix", "REL-2024"},
		// Year guard skips (does not abort): a real issue after a date stamp is
		// still recovered.
		{"year-then-issue", "release/2024-42", "issue-42"},
		{"year-then-issue-multiseg", "release/2024-hotfix-42", "issue-42"},
		// Leading-zero / zero segments are not valid issue numbers.
		{"leading-zero-rejected", "feature/007", ""},
		{"zero-rejected", "feature/0", ""},
		{"leading-zero-then-real-issue", "feature/007-42", "issue-42"},
		// Underscore is a full segment delimiter (both sides).
		{"underscore-delimited", "feature_42", "issue-42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromBranch(c.branch); got != c.want {
				t.Errorf("FromBranch(%q) = %q, want %q", c.branch, got, c.want)
			}
		})
	}
}

func TestFromPRBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// Existing behavior — must stay green.
		{"closes", "closes #7", "issue-7"},
		{"fixes-inline", "fixes #99 for prod", "issue-99"},
		{"resolves", "resolves #123", "issue-123"},
		{"bare-hash", "see #42 for context", "issue-42"},
		{"markdown-heading", "## 3-tier architecture", ""},
		{"empty", "", ""},
		// Gap 3 — prefixed tracker key in a PR body with a generic branch.
		{"prefixed-key-only", "Implements PROJ-123 behaviour", "PROJ-123"},
		// Documented precedence: closes #N > prefixed key > bare #N.
		{"closes-beats-prefixed", "closes #7, part of PROJ-123", "issue-7"},
		{"prefixed-beats-bare", "PROJ-123 tracked, see #42", "PROJ-123"},
		// Leading-zero / bare "#0" is not a valid issue reference.
		{"closes-zero-rejected", "closes #0", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromPRBody(c.body); got != c.want {
				t.Errorf("FromPRBody(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

func TestFromBranchOrBody(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		body   string
		want   string
	}{
		{"branch-wins-over-body", "feature/42-auth", "closes #99", "issue-42"},
		{"body-fallback-when-branch-plain", "main", "closes #7", "issue-7"},
		// Gap 3 wired end to end: generic branch, prefixed key only in body.
		{"prefixed-key-body-only", "feature/no-issue", "Implements PROJ-123", "PROJ-123"},
		// Gap 2 interaction: a year-branch yields nothing, so the body is used.
		{"year-branch-falls-through-to-body", "release/2024-fix", "closes #55", "issue-55"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromBranchOrBody(c.branch, c.body); got != c.want {
				t.Errorf("FromBranchOrBody(%q, %q) = %q, want %q", c.branch, c.body, got, c.want)
			}
		})
	}
}
