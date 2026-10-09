package issueref

import "testing"

func TestCloseDirectiveTerminators(t *testing.T) {
	for _, keyword := range []string{"close", "closes", "closed", "fix", "fixes", "fixed", "resolve", "resolves", "resolved"} {
		for _, suffix := range []string{"", " ", "\t", "\n", ",", ".", ";", ":", ")", "!", "?", "abc", "x", "é"} {
			t.Run(keyword+"/"+suffix, func(t *testing.T) {
				body := keyword + " #12" + suffix
				want := "issue-12"
				if suffix == "abc" || suffix == "x" || suffix == "é" {
					want = ""
				}
				if got := FromCommitSubject(body); got != want {
					t.Errorf("FromCommitSubject(%q) = %q, want %q", body, got, want)
				}
				if got := FromPRBody(body + " PROJ-9"); got != "PROJ-9" && want == "" || want != "" && got != want {
					t.Errorf("FromPRBody(%q) = %q, close directive = %q", body, got, want)
				}
				refs := ClosedIssues(body)
				if want == "" && len(refs) != 0 || want != "" && (len(refs) != 1 || refs[0] != want) {
					t.Errorf("ClosedIssues(%q) = %v, want %q", body, refs, want)
				}
			})
		}
	}
}

func TestCloseDirectiveColon(t *testing.T) {
	for _, body := range []string{"closes: #12", "fixes:#12", "CLOSES: #12", "CVE-2026-1234 closes: #12", "UTF-8 fixes:#12"} {
		t.Run(body, func(t *testing.T) {
			if got := FromCommitSubject(body); got != "issue-12" {
				t.Errorf("FromCommitSubject(%q) = %q, want issue-12", body, got)
			}
			if got := FromPRBody(body); got != "issue-12" {
				t.Errorf("FromPRBody(%q) = %q, want issue-12", body, got)
			}
			if got := ClosedIssues(body); len(got) != 1 || got[0] != "issue-12" {
				t.Errorf("ClosedIssues(%q) = %v, want [issue-12]", body, got)
			}
		})
	}
}

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

func TestFromCommitSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    string
	}{
		{"tracker_token", "docs(rulings): record ladder rung REL-4", "REL-4"},
		{"tracker_key", "fix: attribution PROJ-123", "PROJ-123"},
		{"tracker_key_with_pr_number", "fix: attribution PROJ-123 (#909)", "PROJ-123"},
		{"bare_pr_number", "Merge pull request #909 from feature/rulings", ""},
		{"parenthesized_pr_number", "docs(rulings): record ruling (#909)", ""},
		{"closes", "fix: attribution (closes #12)", "issue-12"},
		{"fixes", "fix: attribution (fixes #1017)", "issue-1017"},
		{"resolves", "fix: attribution (resolves #1017)", "issue-1017"},
		{"close", "fix: attribution (close #1017)", "issue-1017"},
		{"fix", "fix: attribution (fix #1017)", "issue-1017"},
		{"resolve", "fix: attribution (resolve #1017)", "issue-1017"},
		{"closed", "fix: attribution (closed #12)", "issue-12"},
		{"fixed", "fix: attribution (fixed #12)", "issue-12"},
		{"resolved", "fix: attribution (resolved #12)", "issue-12"},
		{"past_tense_case_insensitive", "fix: attribution (FiXeD #12)", "issue-12"},
		{"prefixes", "docs: prefixes #909", ""},
		{"discloses", "docs: discloses #909", ""},
		{"mixed_references", "docs: REL-4 and #909 (CLOSES #1017) (#909)", "issue-1017"},
		{"multiple_directives", "fix: attribution (closes #1017, fixes #42)", "issue-1017"},
		{"lookalike_tracker_key", "fix: mitigate CVE-2026-1234", "CVE-2026"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromCommitSubject(c.subject); got != c.want {
				t.Errorf("FromCommitSubject(%q) = %q, want %q", c.subject, got, c.want)
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
		{"close-beats-prefixed", "close #7, part of PROJ-123", "issue-7"},
		{"fix-beats-prefixed", "fix #7, part of PROJ-123", "issue-7"},
		{"resolve-beats-prefixed", "resolve #7, part of PROJ-123", "issue-7"},
		{"closed-beats-prefixed", "closed #7, part of PROJ-123", "issue-7"},
		{"fixed-beats-prefixed", "fixed #7, part of PROJ-123", "issue-7"},
		{"resolved-beats-prefixed", "resolved #7, part of PROJ-123", "issue-7"},
		{"past-tense-case-insensitive", "FiXeD #7, part of PROJ-123", "issue-7"},
		{"prefixes-not-close", "prefixes #909, part of PROJ-123", "PROJ-123"},
		{"discloses-not-close", "discloses #909, part of PROJ-123", "PROJ-123"},
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
