package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/quality"
	"github.com/tiermetric/tier/internal/store"
)

var fallbackTS = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func fallbackDB(t *testing.T, developers, repos []string, offsets []time.Duration) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i, dev := range developers {
		_, err := db.InsertOutcome(context.Background(), store.Outcome{Developer: dev, Repo: repos[i], IssueID: "issue-42", Weight: 3, Quality: 1, MergeCommitSHA: fmt.Sprintf("%040x", i+1), Timestamp: fallbackTS.Add(offsets[i])})
		if err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func fallbackCommit(message string) map[string]any {
	return map[string]any{"id": "revert-source", "message": message, "timestamp": fallbackTS}
}

func doFallbackPush(t *testing.T, db *store.DB, commits ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ref": "refs/heads/main", "repository": map[string]any{"default_branch": "main", "full_name": "acme/app"}, "commits": commits})
	if err != nil {
		t.Fatal(err)
	}
	if err := New(db, testSecret, quietLogger()).handlePush(context.Background(), body); err != nil {
		t.Fatal(err)
	}
}

func assertFallbackClean(t *testing.T, db *store.DB) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		o, found, err := db.OutcomeByMergeCommit(context.Background(), fmt.Sprintf("%040x", i))
		if err != nil {
			t.Fatal(err)
		}
		if found && o.Quality != 1 {
			t.Errorf("%s quality=%v, want 1", o.Developer, o.Quality)
		}
	}
}

func TestRevertIssueFallback_AmbiguousSkips(t *testing.T) {
	for _, count := range []int{2, 3} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("candidates=%d/reverse=%v", count, reverse), func(t *testing.T) {
				devs := []string{"bob", "carol"}
				offsets := []time.Duration{-48 * time.Hour, -time.Hour}
				repos := []string{"acme/app", "acme/app"}
				if count == 3 {
					devs = []string{"bob", "dave", "carol"}
					offsets = []time.Duration{-48 * time.Hour, -24 * time.Hour, -time.Hour}
					repos = append(repos, "acme/app")
				}
				if reverse {
					devs[0], devs[count-1] = devs[count-1], devs[0]
					offsets[0], offsets[count-1] = offsets[count-1], offsets[0]
				}
				db := fallbackDB(t, devs, repos, offsets)
				for replay := 0; replay < 2; replay++ {
					doFallbackPush(t, db, fallbackCommit(`Revert "fix (closes #42)"`))
				}
				assertFallbackClean(t, db)
				for i := 1; i <= count; i++ {
					o, _, _ := db.OutcomeByMergeCommit(context.Background(), fmt.Sprintf("%040x", i))
					events, err := db.QualityEventsForOutcome(context.Background(), o.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(events) != 1 || events[0].EventType != "revert_ambiguous" || events[0].SourceRef != "revert-source" {
						t.Errorf("%s events=%+v; want one durable ambiguous event", o.Developer, events)
					}
					if o.QualityStatus != "unknown" {
						t.Errorf("%s readback status=%q", o.Developer, o.QualityStatus)
					}
					if len(events) == 1 && (events[0].Reason != "ambiguous" || events[0].IsEvidence() || events[0].Status() != "unknown") {
						t.Errorf("ambiguous check misclassified: %+v", events[0])
					}
					if got := quality.Resolve(events); got != 1 {
						t.Errorf("resolved=%v", got)
					}
				}
			})
		}
	}
}

func TestRevertIssueFallback_BranchCommitsInMergePushUseFooterOnly(t *testing.T) {
	for _, mergeFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(mergeFirst), func(t *testing.T) {
			db := fallbackDB(t, []string{"bob"}, []string{"acme/app"}, []time.Duration{-time.Hour})
			branch := fallbackCommit(`Revert "fix (closes #42)"`)
			merge := map[string]any{"id": "merge", "message": "Merge pull request #1009 from acme/branch"}
			commits := []map[string]any{branch, merge}
			if mergeFirst {
				commits[0], commits[1] = commits[1], commits[0]
			}
			doFallbackPush(t, db, commits...)
			assertFallbackClean(t, db)
			branch["message"] = `Revert "fix (closes #42)"` + "\n\nThis reverts commit " + fmt.Sprintf("%040x", 1) + "."
			doFallbackPush(t, db, commits...)
			o, _, _ := db.OutcomeByMergeCommit(context.Background(), fmt.Sprintf("%040x", 1))
			if o.Quality != 0.1 {
				t.Errorf("exact footer quality=%v", o.Quality)
			}
		})
	}
}

func TestRevertIssueFallback_UnresolvedFooter(t *testing.T) {
	db := fallbackDB(t, []string{"bob"}, []string{"acme/app"}, []time.Duration{-time.Hour})
	doFallbackPush(t, db, fallbackCommit(`Revert "fix (closes #42)"`+"\n\nThis reverts commit "+strings.Repeat("f", 40)+"."))
	assertFallbackClean(t, db)
	events, _, err := db.ListQualityEvents(context.Background(), fallbackTS.Add(-time.Hour), fallbackTS.Add(time.Hour), store.PageCursor{}, 100)
	if err != nil || len(events) != 0 {
		t.Fatalf("unresolved footer must store no candidate: %+v %v", events, err)
	}
}

func TestRevertIssueFallback_RepositoryAndWindow(t *testing.T) {
	for _, tc := range []struct {
		name, repo string
		offset     time.Duration
		clean      bool
	}{
		{"cross-repo", "acme/other", -time.Hour, true},
		{"legacy-unqualified", "unqualified", -time.Hour, true},
		{"legacy-empty", "", -time.Hour, true},
		{"future", "acme/app", time.Nanosecond, true},
		{"outside-window", "acme/app", -60*24*time.Hour - time.Nanosecond, true},
		{"at-window-start", "acme/app", -60 * 24 * time.Hour, false},
		{"at-event", "acme/app", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := fallbackDB(t, []string{"bob"}, []string{tc.repo}, []time.Duration{tc.offset})
			doFallbackPush(t, db, fallbackCommit(`Revert "fix (closes #42)"`))
			o, _, _ := db.OutcomeByMergeCommit(context.Background(), fmt.Sprintf("%040x", 1))
			want := 0.1
			if tc.clean {
				want = 1
			}
			if o.Quality != want {
				t.Errorf("quality=%v want %v", o.Quality, want)
			}
		})
	}
}

func TestRevertIssueFallback_ExistingPenaltyUnchanged(t *testing.T) {
	db := fallbackDB(t, []string{"bob", "carol"}, []string{"acme/app", "acme/app"}, []time.Duration{-time.Hour, -time.Hour})
	ctx := context.Background()
	o, _, err := db.OutcomeByMergeCommit(ctx, fmt.Sprintf("%040x", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQualityForOutcome(ctx, o.ID, 0.1, "revert_quality", "historical"); err != nil {
		t.Fatal(err)
	}
	doFallbackPush(t, db, fallbackCommit(`Revert "fix (closes #42)"`))
	o, _, err = db.OutcomeByMergeCommit(ctx, fmt.Sprintf("%040x", 1))
	if err != nil || o.Quality != 0.1 {
		t.Fatalf("old penalty rewritten: %+v %v", o, err)
	}
	history, err := db.QualityHistoryForOutcome(ctx, o.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("unresolved check added quality transition: %+v %v", history, err)
	}
}

type revertReasonCounter struct{ reasons []string }

func (c *revertReasonCounter) Inc(labels ...string) { c.reasons = append(c.reasons, labels...) }

func TestRevertIssueFallback_WarnAndReasonCounter(t *testing.T) {
	for _, tc := range []struct {
		name, repo, message, reason string
		n                           int
	}{
		{"ambiguous", "acme/app", `Revert "fix (closes #42)"`, "ambiguous", 2},
		{"legacy", "unqualified", `Revert "fix (closes #42)"`, "legacy_only", 1},
		{"footer", "acme/app", `Revert "fix (closes #42)"` + "\n\nThis reverts commit " + strings.Repeat("f", 40) + ".", "footer_unresolved", 1},
		{"merge-push", "acme/app", `Revert "fix (closes #42)"`, "merge_push", 1},
		{"missing", "acme/other", `Revert "fix (closes #42)"`, "no_candidate", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devs, repos, offsets := []string{}, []string{}, []time.Duration{}
			for i := 0; i < tc.n; i++ {
				devs = append(devs, fmt.Sprint("dev", i))
				repos = append(repos, tc.repo)
				offsets = append(offsets, -time.Hour)
			}
			db := fallbackDB(t, devs, repos, offsets)
			counter := &revertReasonCounter{}
			var logs bytes.Buffer
			h := New(db, testSecret, slog.New(slog.NewTextHandler(&logs, nil)), WithRevertAmbiguousCounter(counter))
			commit := fallbackCommit(tc.message)
			commit["id"] = "evil\ncommit"
			commit["author"] = map[string]any{"username": "evil\nauthor"}
			commits := []any{commit}
			if tc.reason == "merge_push" {
				commits = append(commits, map[string]any{"id": "merge", "message": "Merge pull request #1009 from acme/branch"})
			}
			body, err := json.Marshal(map[string]any{"ref": "refs/heads/main", "repository": map[string]any{"default_branch": "main", "full_name": "acme/app"}, "commits": commits})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.handlePush(context.Background(), body); err != nil {
				t.Fatal(err)
			}
			if len(counter.reasons) != 1 || counter.reasons[0] != tc.reason {
				t.Fatalf("counter=%v", counter.reasons)
			}
			if !strings.Contains(logs.String(), "level=WARN") || strings.Count(logs.String(), "\n") != 1 || strings.Contains(logs.String(), `evil\ncommit`) || strings.Contains(logs.String(), `evil\nauthor`) {
				t.Fatalf("unsafe/non-WARN diagnostic: %s", logs.String())
			}
		})
	}
}

func TestRevertIssueFallback_FooterPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   float64
	}{
		{"exact-among-ambiguous", -time.Hour, 0.1},
		{"footer-clock-skew-no-fallback", time.Nanosecond, 0.1},
		{"stale-footer-no-fallback", -61 * 24 * time.Hour, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := fallbackDB(t, []string{"bob", "carol"}, []string{"acme/app", "acme/app"}, []time.Duration{tc.offset, -30 * time.Minute})
			doFallbackPush(t, db, fallbackCommit(`Revert "fix (closes #42)"`+"\n\nThis reverts commit "+fmt.Sprintf("%040x", 1)+"."))
			bob, _, err := db.OutcomeByMergeCommit(context.Background(), fmt.Sprintf("%040x", 1))
			if err != nil {
				t.Fatal(err)
			}
			carol, _, err := db.OutcomeByMergeCommit(context.Background(), fmt.Sprintf("%040x", 2))
			if err != nil {
				t.Fatal(err)
			}
			if bob.Quality != tc.want || carol.Quality != 1 {
				t.Fatalf("footer chose wrong outcome: bob=%v carol=%v", bob.Quality, carol.Quality)
			}
			events, err := db.QualityEventsForOutcome(context.Background(), carol.ID)
			if err != nil || len(events) != 0 {
				t.Fatalf("footer fell through: %+v %v", events, err)
			}
		})
	}
}

func TestRevertIssueFallback_EraseRestartReplayStaysAmbiguous(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "replay.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i, dev := range []string{"bob", "carol"} {
		if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: dev, Repo: "acme/app", IssueID: "issue-42", Weight: 3, Quality: 1, MergeCommitSHA: fmt.Sprintf("%040x", i+1), Timestamp: fallbackTS.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	commit := fallbackCommit(`Revert "fix (closes #42)"`)
	doFallbackPush(t, db, commit)
	erased, err := db.EraseDeveloper(ctx, "bob")
	if err != nil || erased["outcomes"] != 1 {
		t.Fatalf("erase=%v err=%v", erased, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	doFallbackPush(t, db, commit)
	carol, found, err := db.OutcomeByMergeCommit(ctx, fmt.Sprintf("%040x", 2))
	if err != nil || !found || carol.Quality != 1 || carol.QualityStatus != "unknown" {
		t.Fatalf("replayed ambiguous revert changed survivor: %+v found=%v err=%v", carol, found, err)
	}
	events, err := db.QualityEventsForOutcome(ctx, carol.ID)
	if err != nil || len(events) != 1 || events[0].EventType != quality.EventRevertAmbiguous {
		t.Fatalf("survivor events=%+v err=%v", events, err)
	}
	// A separate revert SHA can still resolve the now unique candidate.
	commit["id"] = "different-revert"
	doFallbackPush(t, db, commit)
	carol, _, err = db.OutcomeByMergeCommit(ctx, fmt.Sprintf("%040x", 2))
	if err != nil || carol.Quality != 0.1 {
		t.Fatalf("distinct revert=%+v err=%v", carol, err)
	}
}
