package webhook

import (
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
)

// Mirror #1017's commit-subject rule at the push webhook boundary, including
// observable unattributed commits and the collector's subject-only input.
func TestHandlePush_CaptureCommitSubjectAttribution(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"closes", "fix: attribution (closes #12)", "issue-12"},
		{"fixes", "fix: attribution (fixes #1017)", "issue-1017"},
		{"resolves", "fix: attribution (resolves #1017)", "issue-1017"},
		{"tracker_key", "fix: attribution PROJ-123", "PROJ-123"},
		{"tracker_key_with_pr_number", "fix: attribution PROJ-123 (#909)", "PROJ-123"},
		{"bare_number", "docs: see #909 for context", ""},
		{"parenthesized_pr_number", "docs(rulings): record ruling (#909)", ""},
		{"no_reference", "chore: tidy up", ""},
		{"mixed_references", "docs: REL-4 and #909 (CLOSES #1017) (#909)", "issue-1017"},
		{"multiple_directives", "fix: attribution (closes #1017, fixes #42)", "issue-1017"},
		{"body_close_directive", "chore: tidy up\n\ncloses #12", ""},
		{"body_tracker_key", "chore: tidy up\n\nPROJ-123", ""},
		{"subject_tracker_body_close", "fix: attribution PROJ-123\n\ncloses #12", "PROJ-123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newFakeStore()
			ctr := &fakePushCounter{}
			ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			logs := captureLogs(func(logger *slog.Logger) {
				h := New(st, testSecret, logger, WithPushCapture(ctr))
				dispatchPush(t, h, "refs/heads/main", "main",
					pushCommit("sha1", c.message, "alice", ts))
			})

			outcomes := st.pushSource()
			if c.want == "" {
				if len(outcomes) != 0 {
					t.Errorf("unattributed commit: push outcomes = %+v, want none", outcomes)
				}
				if got := ctr.count(); got != 1 {
					t.Errorf("unattributed counter = %d, want 1", got)
				}
				if !strings.Contains(logs, "no resolvable issue id") {
					t.Errorf("missing unattributed log line: %q", logs)
				}
				return
			}
			if len(outcomes) != 1 {
				t.Fatalf("push outcomes = %+v, want one for %q", outcomes, c.want)
			}
			if got := outcomes[0].IssueID; got != c.want {
				t.Errorf("IssueID = %q, want %q", got, c.want)
			}
			if got := ctr.count(); got != 0 {
				t.Errorf("unattributed counter = %d, want 0", got)
			}
		})
	}
}

// The collector feeds FromCommitSubject git log's %s, which folds the first
// paragraph. Use real git output as the oracle so body references stay excluded.
func TestHandlePush_CaptureGitSubjectParity(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo,
			"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSuffix(string(out), "\n")
	}
	git("init", "--quiet")
	for _, c := range []struct {
		name    string
		message string
		want    string
	}{
		{"wrapped_subject", "chore: tidy\ncloses #12", "issue-12"},
		{"wrapped_directive", "chore: tidy\ncloses\n#12", "issue-12"},
		{"trimmed_lines", "chore: tidy \t\ncloses #12 \t", "issue-12"},
		{"indented_lines", " chore: tidy\n closes #12", "issue-12"},
		{"leading_blank_lines", "\n \t\nchore: tidy\ncloses #12", "issue-12"},
		{"body_after_blank_line", "chore: tidy\n\ncloses #12", ""},
		{"body_after_whitespace_line", "chore: tidy\n \t\ncloses #12", ""},
		{"wrapped_tracker_body_close", "chore: tidy\nPROJ-123\n\ncloses #12", "PROJ-123"},
	} {
		t.Run(c.name, func(t *testing.T) {
			git("-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
				"commit", "--quiet", "--allow-empty", "--cleanup=verbatim", "-m", c.message)
			subject := git("log", "-1", "--format=%s")
			if got := gitCommitSubject(c.message); got != subject {
				t.Fatalf("gitCommitSubject(%q) = %q, want git %%s %q", c.message, got, subject)
			}
			collectorIssue := issueref.FromCommitSubject(subject)
			if collectorIssue != c.want {
				t.Fatalf("collector helper for git subject %q = %q, want %q", subject, collectorIssue, c.want)
			}

			st := newFakeStore()
			ctr := &fakePushCounter{}
			h := New(st, testSecret, quietLogger(), WithPushCapture(ctr))
			dispatchPush(t, h, "refs/heads/main", "main",
				pushCommit("sha1", c.message, "alice", time.Now().UTC()))
			outcomes := st.pushSource()
			if collectorIssue == "" {
				if len(outcomes) != 0 || ctr.count() != 1 {
					t.Fatalf("body-only reference: outcomes = %+v, unattributed = %d; want none and 1", outcomes, ctr.count())
				}
			} else if len(outcomes) != 1 || outcomes[0].IssueID != collectorIssue || ctr.count() != 0 {
				t.Fatalf("push outcomes = %+v, unattributed = %d; want collector issue %q and 0", outcomes, ctr.count(), collectorIssue)
			}
		})
	}
}
