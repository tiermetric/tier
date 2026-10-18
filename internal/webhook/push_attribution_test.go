package webhook

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

// Reserved pseudo-developer logins must never create an outcome or ledger entry,
// but must still be observable through the push skip log and counter (#1191).
func TestHandlePush_CaptureReservedAuthorIsObservable(t *testing.T) {
	for _, c := range []struct {
		name       string
		author     string
		wantRows   int
		wantReason string
	}{
		{"reserved", "unattributed", 0, "author login is reserved for the unattributed pseudo-developer (#619)"},
		{"lookalike", " UnAtTrIbUtEd ", 0, "author login is reserved for the unattributed pseudo-developer (#619)"},
		{"labeled_lookalike", " UnAtTrIbUtEd:unknown ", 0, "author login is reserved for the unattributed pseudo-developer (#619)"},
		{"empty", "", 0, "no GitHub author login"},
		{"normal", "alice", 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newFakeStore()
			ctr := &fakePushCounter{}
			ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			logs := captureLogs(func(logger *slog.Logger) {
				h := New(st, testSecret, logger, WithPushCapture(ctr))
				dispatchPush(t, h, "refs/heads/main", "main",
					pushCommit("a1a1a1a1", "fix: thing (closes #12)", c.author, ts))
			})

			if got := st.outcomeCount(); got != c.wantRows {
				t.Errorf("outcomes = %d, want %d", got, c.wantRows)
			}
			st.mu.Lock()
			ledgerRows := len(st.ledger)
			st.mu.Unlock()
			if ledgerRows != c.wantRows {
				t.Errorf("push ledger rows = %d, want %d", ledgerRows, c.wantRows)
			}
			if got, want := ctr.count(), int64(1-c.wantRows); got != want {
				t.Errorf("unattributed counter = %d, want %d", got, want)
			}
			if c.wantReason != "" && !strings.Contains(logs, "push capture: direct commit not scored — "+c.wantReason) {
				t.Errorf("missing push skip reason %q in logs: %q", c.wantReason, logs)
			}
			if c.wantRows == 1 {
				outcomes := st.pushSource()
				if len(outcomes) != 1 || outcomes[0].Developer != c.author || outcomes[0].IssueID != "issue-12" {
					t.Errorf("push outcomes = %+v, want one for %s and issue-12", outcomes, c.author)
				}
			}
		})
	}
}

func TestHandlePush_RevertLeadingBlankLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{"control", ""},
		{"leading_newline", "\n"},
		{"leading_whitespace_lines", "\n \t\r\v\f\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			if _, err := st.InsertOutcome(context.Background(), store.Outcome{Repo: "acme/app",
				Developer: "bob", IssueID: "issue-12", PRNumber: 10,
				Weight: 5, Quality: 1, Timestamp: ts,
			}); err != nil {
				t.Fatal(err)
			}
			ctr := &fakePushCounter{}
			h := New(st, testSecret, quietLogger(), WithPushCapture(ctr))
			const sha = "abcdef0123456789abcdef0123456789abcdef01"
			dispatchPush(t, h, "refs/heads/main", "main",
				pushCommit(sha, tc.prefix+`Revert "fix: thing (closes #12)"`, "alice", ts))

			if got := len(st.pushSource()); got != 0 {
				t.Errorf("revert commit: push outcomes = %d, want 0", got)
			}
			if got := ctr.count(); got != 0 {
				t.Errorf("revert commit: unattributed counter = %d, want 0", got)
			}
			if len(st.qualityUpdates) != 1 {
				t.Fatalf("quality updates = %d, want 1 on original outcome", len(st.qualityUpdates))
			}
			u := st.qualityUpdates[0]
			if u.Developer != "bob" || u.IssueID != "issue-12" || u.Quality != 0.1 || u.Reason != "revert_quality" || u.SourceRef != sha {
				t.Errorf("quality update = %+v, want original author bob, issue-12, quality 0.1, revert_quality, source %s", u, sha)
			}
		})
	}
}

func TestHandlePush_MergeLeadingBlankLines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{"control", "Merge pull request #5 from x/y"},
		{"leading_newline", "\nMerge pull request #5 from x/y"},
		{"attributed_control", "Merge pull request #5 from x/y\ncloses #12"},
		{"attributed_leading_newline", "\nMerge pull request #5 from x/y\ncloses #12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			ctr := &fakePushCounter{}
			h := New(st, testSecret, quietLogger(), WithPushCapture(ctr))
			dispatchPush(t, h, "refs/heads/main", "main",
				pushCommit("abcdef0123456789abcdef0123456789abcdef01", tc.message, "alice", time.Now().UTC()))
			if got := len(st.pushSource()); got != 0 {
				t.Errorf("merge commit: push outcomes = %d, want 0", got)
			}
			if got := ctr.count(); got != 0 {
				t.Errorf("merge commit reached attribution: unattributed counter = %d, want 0", got)
			}
		})
	}
}

func TestHandlePush_RevertWrappedSubject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{"control", `Revert "fix: thing (closes #12)"`},
		{"wrapped_subject", "Revert\n\"fix: thing (closes #12)\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			if _, err := st.InsertOutcome(context.Background(), store.Outcome{Repo: "acme/app",
				Developer: "bob", IssueID: "issue-12", PRNumber: 10,
				Weight: 5, Quality: 1, Timestamp: ts,
			}); err != nil {
				t.Fatal(err)
			}
			ctr := &fakePushCounter{}
			h := New(st, testSecret, quietLogger(), WithPushCapture(ctr))
			const sha = "abcdef0123456789abcdef0123456789abcdef01"
			dispatchPush(t, h, "refs/heads/main", "main",
				pushCommit(sha, tc.message, "alice", ts))

			if got := len(st.pushSource()); got != 0 {
				t.Errorf("revert commit: push outcomes = %d, want 0", got)
			}
			if got := ctr.count(); got != 0 {
				t.Errorf("revert commit: unattributed counter = %d, want 0", got)
			}
			if len(st.qualityUpdates) != 1 {
				t.Fatalf("quality updates = %d, want 1 on original outcome", len(st.qualityUpdates))
			}
			u := st.qualityUpdates[0]
			if u.Developer != "bob" || u.IssueID != "issue-12" || u.Quality != 0.1 || u.Reason != "revert_quality" || u.SourceRef != sha {
				t.Errorf("quality update = %+v, want original author bob, issue-12, quality 0.1, revert_quality, source %s", u, sha)
			}
		})
	}
}

// Mirror #1017's commit-subject rule at the push webhook boundary, including
// observable unattributed commits and the collector's subject-only input.
func TestHandlePush_CaptureCommitSubjectAttribution(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
		keys    string
	}{
		{"closes", "fix: attribution (closes #12)", "issue-12", ""},
		{"fixes", "fix: attribution (fixes #1017)", "issue-1017", ""},
		{"resolves", "fix: attribution (resolves #1017)", "issue-1017", ""},
		{"tracker_key", "fix: attribution PROJ-123", "", ""},
		{"tracker_key_with_pr_number", "fix: attribution PROJ-123 (#909)", "", ""},
		{"bare_number", "docs: see #909 for context", "", ""},
		{"parenthesized_pr_number", "docs(rulings): record ruling (#909)", "", ""},
		{"no_reference", "chore: tidy up", "", ""},
		{"mixed_references", "docs: REL-4 and #909 (CLOSES #1017) (#909)", "issue-1017", ""},
		{"multiple_directives", "fix: attribution (closes #1017, fixes #42)", "issue-1017", ""},
		{"body_close_directive", "chore: tidy up\n\ncloses #12", "", ""},
		{"body_tracker_key", "chore: tidy up\n\nPROJ-123", "", ""},
		{"subject_tracker_body_close", "fix: attribution PROJ-123\n\ncloses #12", "", ""},
		{"configured_tracker_key", "fix: attribution PROJ-123", "PROJ-123", "PROJ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := issueref.NewGrammar(c.keys)
			if err != nil {
				t.Fatal(err)
			}
			st := newFakeStore()
			ctr := &fakePushCounter{}
			ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			logs := captureLogs(func(logger *slog.Logger) {
				h := New(st, testSecret, logger, WithPushCapture(ctr), WithGrammar(g))
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
			wantRule := issueref.RuleCommitCloseDirective
			if c.want == "PROJ-123" {
				wantRule = issueref.RuleCommitConfiguredKey
			}
			if outcomes[0].IssueRule != wantRule {
				t.Errorf("IssueRule = %q, want %q", outcomes[0].IssueRule, wantRule)
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
		{"wrapped_tracker_body_close", "chore: tidy\nPROJ-123\n\ncloses #12", ""},
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
