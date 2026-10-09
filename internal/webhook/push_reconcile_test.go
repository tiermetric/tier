package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// #849: a squash merge's push and pull_request events, against the REAL store
// and the REAL handler, in every arrival order.

const reconcileRepo = "acme/app"

var reconcileDay = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

type reconcileHarness struct {
	db   *store.DB
	h    *Handler
	leak *fakePushCounter
}

func newReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	leak := &fakePushCounter{}
	h := New(db, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}), WithPushMergeLeakCounter(leak))
	return &reconcileHarness{db: db, h: h, leak: leak}
}

// deliver posts one signed event. A 503 means the write lock stayed held past
// the DSN's busy_timeout; GitHub does NOT redeliver it, so this harness retries
// only so a heavily loaded test box cannot flake the scenario under test.
func (r *reconcileHarness) deliver(t *testing.T, event string, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Errorf("marshal %s: %v", event, err)
		return
	}
	for attempt := 0; attempt < 50; attempt++ {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusNoContent:
			return
		case http.StatusServiceUnavailable:
			time.Sleep(time.Millisecond)
			continue
		default:
			t.Errorf("%s returned %d: %s", event, rec.Code, rec.Body.String())
			return
		}
	}
	t.Errorf("%s: still 503 after 50 attempts", event)
}

func (r *reconcileHarness) push(t *testing.T, commits ...map[string]any) {
	t.Helper()
	r.deliver(t, "push", map[string]any{
		"ref":        "refs/heads/main",
		"repository": map[string]any{"default_branch": "main", "full_name": reconcileRepo},
		"commits":    commits,
	})
}

// pr delivers the merged pull request #n whose merge commit is sha.
func (r *reconcileHarness) pr(t *testing.T, n int, sha, author string) {
	t.Helper()
	r.deliver(t, "pull_request", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": n, "merged": true, "merge_commit_sha": sha,
			"body": "closes #42", "head": map[string]any{"ref": "feature/42-thing"},
			"user": map[string]any{"login": author},
		},
		"repository": map[string]any{"full_name": reconcileRepo},
	})
}

// rows renders every stored outcome, sorted. A push row's ts is part of what is
// compared (its owner is re-derived from it); a PR row's is not, because the
// handler stamps a PR outcome with the delivery time.
func (r *reconcileHarness) rows(t *testing.T) []string {
	t.Helper()
	all, _, err := r.db.ListOutcomes(context.Background(), time.Time{},
		time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 1000)
	if err != nil {
		t.Fatalf("ListOutcomes: %v", err)
	}
	out := make([]string, 0, len(all))
	for _, o := range all {
		row := fmt.Sprintf("%s dev=%s issue=%s pr=%d w=%v sha=%s", o.Source, o.Developer, o.IssueID, o.PRNumber, o.Weight, o.MergeCommitSHA)
		if o.Source == store.OutcomeSourcePush {
			row += " ts=" + o.Timestamp.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	sort.Strings(out)
	return out
}

// state is rows plus every ledger and audit position a redelivery must not move.
func (r *reconcileHarness) state(t *testing.T) string {
	t.Helper()
	w, err := r.db.ReportWatermarks(context.Background(), time.Time{}, time.Time{}, store.FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks: %v", err)
	}
	var ledger int
	for _, dev := range []string{"alice", "bob", "carol"} {
		exp, err := r.db.ExportDeveloper(context.Background(), dev)
		if err != nil {
			t.Fatalf("ExportDeveloper(%s): %v", dev, err)
		}
		ledger += len(exp.PushOutcomeCommits)
	}
	return fmt.Sprintf("%v window=%+v ledgers=%+v ledgerEntries=%d", r.rows(t), w.Window, w.Ledgers, ledger)
}

func at(hour int) time.Time { return reconcileDay.Add(time.Duration(hour) * time.Hour) }

// The squash commit GitHub pushes to main: its SHA is the PR's merge_commit_sha
// and its subject carries the PR's "(closes #42)" so push capture qualifies.
func squash() map[string]any {
	return pushCommit("5a5a5a5a", "feat: the thing (closes #42) (#7)", "alice", at(12))
}

const prOnly = "github-webhook dev=alice issue=issue-42 pr=7 w=0.5 sha=5a5a5a5a"

// Assert the fixture really captured the squash before a PR can replace it.
func (r *reconcileHarness) assertSquashCaptured(t *testing.T) {
	t.Helper()
	const want = "push dev=alice issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T12:00:00Z"
	if got := r.rows(t); len(got) != 1 || got[0] != want {
		t.Fatalf("before PR: rows = %q, want exactly [%q]", got, want)
	}
	exp, err := r.db.ExportDeveloper(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	if len(exp.PushOutcomeCommits) != 1 || exp.PushOutcomeCommits[0].CommitSHA != "5a5a5a5a" {
		t.Fatalf("before PR: squash ledger = %+v, want the captured commit 5a5a5a5a", exp.PushOutcomeCommits)
	}
}

// TestReconcile_SquashCountsOnceInEitherOrder is the issue's repro: on main the
// push-first order stored the squash twice.
func TestReconcile_SquashCountsOnceInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, r *reconcileHarness)
	}{
		{"push-first", func(t *testing.T, r *reconcileHarness) {
			r.push(t, squash())
			r.assertSquashCaptured(t)
			r.pr(t, 7, "5a5a5a5a", "alice")
		}},
		{"pr-first", func(t *testing.T, r *reconcileHarness) { r.pr(t, 7, "5a5a5a5a", "alice"); r.push(t, squash()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileHarness(t)
			tc.run(t, r)
			if got := r.rows(t); len(got) != 1 || got[0] != prOnly {
				t.Fatalf("rows = %q, want exactly [%q]", got, prOnly)
			}
		})
	}
}

// TestReconcile_LaterSameDayCommitGivesTheSameRowsInEveryOrder: a squash merge
// and a later same-day direct commit by bob on the same issue. Every arrival
// order must store the same rows — including who owns the surviving push row.
func TestReconcile_LaterSameDayCommitGivesTheSameRowsInEveryOrder(t *testing.T) {
	direct := func() map[string]any {
		return pushCommit("d1d1d1d1", "fix: follow-up (closes #42)", "bob", at(15))
	}
	want := []string{
		prOnly,
		"push dev=bob issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T15:00:00Z",
	}
	sort.Strings(want)
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, r *reconcileHarness)
	}{
		{"pr, squash push, direct push", func(t *testing.T, r *reconcileHarness) {
			r.pr(t, 7, "5a5a5a5a", "alice")
			r.push(t, squash())
			r.push(t, direct())
		}},
		{"squash push, pr, direct push", func(t *testing.T, r *reconcileHarness) {
			r.push(t, squash())
			r.pr(t, 7, "5a5a5a5a", "alice")
			r.push(t, direct())
		}},
		{"squash push, direct push, pr", func(t *testing.T, r *reconcileHarness) {
			r.push(t, squash())
			r.push(t, direct())
			r.pr(t, 7, "5a5a5a5a", "alice")
		}},
		{"one push with both, then pr", func(t *testing.T, r *reconcileHarness) {
			r.push(t, squash(), direct())
			r.pr(t, 7, "5a5a5a5a", "alice")
		}},
		{"direct push first, squash push, pr", func(t *testing.T, r *reconcileHarness) {
			r.push(t, direct())
			r.push(t, squash())
			r.pr(t, 7, "5a5a5a5a", "alice")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileHarness(t)
			tc.run(t, r)
			if got := r.rows(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// TestReconcile_EarlierDirectCommitByAnotherDeveloperKeepsItsCredit is the case
// that refuted the no-schema fix: carol's own same-day commit on #42, pushed
// before the squash, is her credit and must survive the PR in either order.
func TestReconcile_EarlierDirectCommitByAnotherDeveloperKeepsItsCredit(t *testing.T) {
	carol := func() map[string]any {
		return pushCommit("c0c0c0c0", "feat: groundwork (closes #42)", "carol", at(9))
	}
	want := []string{prOnly, "push dev=carol issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T09:00:00Z"}
	sort.Strings(want)
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, r *reconcileHarness)
	}{
		{"squash push before pr", func(t *testing.T, r *reconcileHarness) {
			r.push(t, carol())
			r.push(t, squash())
			r.pr(t, 7, "5a5a5a5a", "alice")
		}},
		{"pr before squash push", func(t *testing.T, r *reconcileHarness) {
			r.push(t, carol())
			r.pr(t, 7, "5a5a5a5a", "alice")
			r.push(t, squash())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileHarness(t)
			tc.run(t, r)
			if got := r.rows(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// TestReconcile_RedeliveryWritesNothing: a redelivered push or pull_request —
// same body, no delivery GUID, so only the store can recognise it — moves no
// row, no ledger entry and no audit position, in any state.
func TestReconcile_RedeliveryWritesNothing(t *testing.T) {
	r := newReconcileHarness(t)
	direct := pushCommit("d1d1d1d1", "fix: follow-up (closes #42)", "bob", at(15))

	r.push(t, squash(), direct)
	before := r.state(t)
	r.push(t, squash(), direct)
	if after := r.state(t); after != before {
		t.Fatalf("redelivered push (before the PR) moved state:\n before %s\n after  %s", before, after)
	}

	r.pr(t, 7, "5a5a5a5a", "alice")
	before = r.state(t)
	r.pr(t, 7, "5a5a5a5a", "alice")
	r.push(t, squash(), direct)
	if after := r.state(t); after != before {
		t.Fatalf("redelivered pr/push (after the PR) moved state:\n before %s\n after  %s", before, after)
	}
	want := []string{prOnly, "push dev=bob issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T15:00:00Z"}
	sort.Strings(want)
	if got := r.rows(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestReconcile_ConcurrentDeliveriesYieldOneOutcome drives the squash's push and
// pull_request — each delivered twice — from four goroutines at once. On main any
// push that ran before the PR insert was never removed, so this stored two
// whenever a push goroutine won. It also catches a check-then-write race (the
// merge-commit check outside the write transaction), but only when the goroutines
// interleave inside that window, so it repeats: 25 iterations caught such a
// mutant 8 times in 9 runs. A second case first proves the push was captured,
// then races its redeliveries against the PR that must replace it.
func TestReconcile_ConcurrentDeliveriesYieldOneOutcome(t *testing.T) {
	for _, capturedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("captured-first=%t", capturedFirst), func(t *testing.T) {
			for i := 0; i < 100; i++ {
				r := newReconcileHarness(t)
				if capturedFirst {
					r.push(t, squash())
					r.assertSquashCaptured(t)
				}
				var wg sync.WaitGroup
				start := make(chan struct{})
				for g := 0; g < 4; g++ {
					wg.Add(1)
					go func(g int) {
						defer wg.Done()
						<-start
						if g%2 == 0 {
							r.push(t, squash())
						} else {
							r.pr(t, 7, "5a5a5a5a", "alice")
						}
					}(g)
				}
				close(start)
				wg.Wait()
				if got := r.rows(t); len(got) != 1 || got[0] != prOnly {
					t.Fatalf("iteration %d: rows = %q, want exactly [%q]", i, got, prOnly)
				}
			}
		})
	}
}

// TestReconcile_LegacyPushRowIsNeverDeleted: a push row written with no ledger
// entries (before the upgrade) is never deleted by a PR — neither when the squash
// commit folded into it after the upgrade, nor when the row IS the pre-upgrade
// squash (that double count predates the ledger and stays).
func TestReconcile_LegacyPushRowIsNeverDeleted(t *testing.T) {
	legacy := func(t *testing.T, r *reconcileHarness, dev string, ts time.Time) {
		t.Helper()
		if ok, err := r.db.UpsertPushOutcome(context.Background(), store.Outcome{
			Developer: dev, IssueID: "issue-42", Weight: 0.5, Quality: 1,
			Repo: reconcileRepo, Timestamp: ts,
		}, "2026-09-10"); err != nil || !ok {
			t.Fatalf("seed legacy push row: inserted=%v err=%v", ok, err)
		}
	}
	t.Run("squash folded into a legacy row after the upgrade", func(t *testing.T) {
		r := newReconcileHarness(t)
		legacy(t, r, "carol", at(9))
		r.push(t, squash())
		r.pr(t, 7, "5a5a5a5a", "alice")
		want := []string{prOnly, "push dev=carol issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T09:00:00Z"}
		sort.Strings(want)
		if got := r.rows(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})
	t.Run("the legacy row is the pre-upgrade squash", func(t *testing.T) {
		r := newReconcileHarness(t)
		legacy(t, r, "alice", at(12))
		r.pr(t, 7, "5a5a5a5a", "alice")
		want := []string{prOnly, "push dev=alice issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T12:00:00Z"}
		sort.Strings(want)
		if got := r.rows(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})
}

// TestReconcile_EraseCoversTheLedger: erasing alice removes her ledger entries
// and audit rows; the push row she owned is re-owned to bob, whose later commit
// on it survives with his credit.
func TestReconcile_EraseCoversTheLedger(t *testing.T) {
	r := newReconcileHarness(t)
	ctx := context.Background()
	// alice owns the day's row (earliest commit); bob's later commit joins it.
	r.pushAt(t, unixAt(12, 0),
		pushCommit("a1a1a1a1", "feat: start (closes #42)", "alice", at(8)),
		pushCommit("b1b1b1b1", "feat: more (closes #42)", "bob", at(10)))
	// carol's push, earlier but delivered later, re-owns the row, and her PR
	// merging that commit hands it back: two re-derivations, so alice has audit
	// rows to erase.
	r.pushAt(t, unixAt(7, 0), pushCommit("c1c1c1c1", "feat: earliest (closes #42)", "carol", at(6)))
	r.deliver(t, "pull_request", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 9, "merged": true, "merge_commit_sha": "c1c1c1c1",
			"body": "closes #42", "head": map[string]any{"ref": "feature/42-x"},
			"user": map[string]any{"login": "carol"},
		},
		"repository": map[string]any{"full_name": reconcileRepo},
	})
	exp, err := r.db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	if len(exp.PushOutcomeCommits) != 1 || len(exp.PushOutcomeAudit) == 0 {
		t.Fatalf("fixture: alice has %d ledger entries and %d audit rows, want 1 and >0 — the erase below would prove nothing",
			len(exp.PushOutcomeCommits), len(exp.PushOutcomeAudit))
	}
	counts, err := r.db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if counts["push_outcome_commits"] != 1 {
		t.Errorf("counts[push_outcome_commits] = %d, want 1 (alice's entry only)", counts["push_outcome_commits"])
	}
	exp, err = r.db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper after erase: %v", err)
	}
	if exp.RowCount() != 0 {
		t.Errorf("alice's export after erase has %d rows, want 0: %+v", exp.RowCount(), exp)
	}
	bob, err := r.db.ExportDeveloper(ctx, "bob")
	if err != nil {
		t.Fatalf("ExportDeveloper(bob): %v", err)
	}
	if len(bob.PushOutcomeCommits) != 1 {
		t.Errorf("bob has %d ledger entries after alice's erasure, want his 1", len(bob.PushOutcomeCommits))
	}
	want := []string{
		"github-webhook dev=carol issue=issue-42 pr=9 w=0.5 sha=c1c1c1c1",
		"push dev=bob issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T10:00:00Z",
	}
	if got := r.rows(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestReconcile_EraseKeepsCoContributorsCredit: bob's commit at 14:00 and
// alice's at 09:00, pushed earlier but delivered later, share a row, so alice
// owns it. Erasing alice
// must leave bob's credit — the row, re-owned to him — and no audit row of bob's
// may still name alice's commit.
func TestReconcile_EraseKeepsCoContributorsCredit(t *testing.T) {
	r := newReconcileHarness(t)
	ctx := context.Background()
	r.pushAt(t, unixAt(14, 5), pushCommit("b9b9b9b9", "feat: bob's part (closes #42)", "bob", at(14)))
	r.pushAt(t, unixAt(9, 5), pushCommit("a9a9a9a9", "feat: alice's part (closes #42)", "alice", at(9)))
	if _, err := r.db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	want := "push dev=bob issue=issue-42 pr=0 w=0.5 sha= ts=2026-09-10T14:00:00Z"
	if got := r.rows(t); len(got) != 1 || got[0] != want {
		t.Fatalf("rows after erasing alice = %q, want exactly [%q]", got, want)
	}
	bob, err := r.db.ExportDeveloper(ctx, "bob")
	if err != nil {
		t.Fatalf("ExportDeveloper(bob): %v", err)
	}
	if len(bob.PushOutcomeAudit) == 0 {
		t.Fatalf("fixture: bob has no audit rows, so the SHA check below proves nothing")
	}
	for _, a := range bob.PushOutcomeAudit {
		if a.CommitSHA == "a9a9a9a9" {
			t.Errorf("bob's audit row %d (%s) still names erased alice's commit", a.ID, a.Action)
		}
	}
}

// TestReconcile_MergeCommitPushIsCounted: a push carrying "Merge pull request #N"
// beside captured commits is the #934 leak; it is WARNed and counted once, and a
// redelivery (which captures nothing) does not count it again.
func TestReconcile_MergeCommitPushIsCounted(t *testing.T) {
	r := newReconcileHarness(t)
	merge := pushCommit("m1m1m1m1", "Merge pull request #7 from acme/feature\n\ncloses #42", "alice", at(12))
	branch := pushCommit("b2b2b2b2", "feat: branch work (closes #42)", "alice", at(11))
	r.push(t, merge)
	if n := r.leak.count(); n != 0 {
		t.Fatalf("a merge commit alone counted %d, want 0 (nothing was captured beside it)", n)
	}
	r.push(t, merge, branch)
	if n := r.leak.count(); n != 1 {
		t.Fatalf("merge commit + captured sibling counted %d, want 1", n)
	}
	r.push(t, merge, branch)
	if n := r.leak.count(); n != 1 {
		t.Fatalf("a redelivery counted again: %d, want 1", n)
	}
}

// TestReconcile_MergeLeakCountedOnlyOnACleanDelivery: a delivery that fails part
// way is answered 5xx and may be sent again; counting the leak on the failed
// attempt and again on the retry would count one push twice.
func TestReconcile_MergeLeakCountedOnlyOnACleanDelivery(t *testing.T) {
	fs := newFakeStore()
	fs.upsertErrs = 1 // the first captured commit fails; the second succeeds
	leak := &fakePushCounter{}
	h := New(fs, testSecret, quietLogger(), WithPushCapture(&fakePushCounter{}), WithPushMergeLeakCounter(leak))
	body, err := json.Marshal(map[string]any{
		"ref":        "refs/heads/main",
		"repository": map[string]any{"default_branch": "main", "full_name": reconcileRepo},
		"commits": []map[string]any{
			pushCommit("m1m1m1m1", "Merge pull request #7 from acme/feature", "alice", at(12)),
			pushCommit("b1b1b1b1", "feat: one (closes #42)", "alice", at(10)),
			pushCommit("b2b2b2b2", "feat: two (closes #42)", "alice", at(11)),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(); code != http.StatusInternalServerError {
		t.Fatalf("partial-failure delivery returned %d, want 500", code)
	}
	if n := leak.count(); n != 0 {
		t.Fatalf("a failed delivery counted the leak %d times, want 0", n)
	}
	if code := send(); code != http.StatusNoContent {
		t.Fatalf("retry returned %d, want 204", code)
	}
	if n := leak.count(); n != 1 {
		t.Fatalf("failed delivery + clean retry counted %d, want 1", n)
	}
}
