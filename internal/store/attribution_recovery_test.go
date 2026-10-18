package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
)

func recoveryOutcome(t *testing.T, db *DB, number int, branch, issue string, opened, merged time.Time) Outcome {
	t.Helper()
	o := Outcome{Developer: "alice", Repo: "acme/a", IssueID: issue, PRNumber: number, Source: "backfill", HeadBranchHash: issueref.BranchHash("acme/a", branch), CreatedAt: &opened, MergedAt: &merged, Timestamp: merged, MergeCommitSHA: fmt.Sprint("sha-", number), Weight: 4, Quality: 1}
	if issueref.IsRevertBranch(branch) {
		o.HeadBranchHash = nil
	}
	return o
}

func TestRecoveryReceiptNewestFirstReusedBranch(t *testing.T) {
	run := func(reverse bool) []byte {
		db, cleanup := newTestDB(t)
		defer cleanup()
		ctx := t.Context()
		merge := fillMerge
		oldStart := merge.Add(-8 * 24 * time.Hour)
		type fixture struct {
			name, source, repo, branch, stored, want, disposition string
			at                                                    time.Time
			pr                                                    int
		}
		cases := []fixture{
			{"two-worktrees-a", "jsonl", "acme/a", "feature/shared", UnattributedNoIssueBucket, "issue-1", "unique_read_time_recovery", merge.Add(-time.Hour), 0},
			{"two-worktrees-b", "jsonl", "acme/a", "feature/second", UnattributedNoIssueBucket, "issue-3", "unique_read_time_recovery", merge.Add(-time.Hour), 0},
			{"branch-change-same-cwd", "jsonl", "acme/a", "feature/shared", UnattributedNoIssueBucket, "issue-2", "unique_read_time_recovery", merge.Add(time.Nanosecond), 0},
			{"deleted-worktree", "codex-rollout", "acme/a", "feature/shared", UnattributedIssueID, "issue-1", "unique_read_time_recovery", merge.Add(-time.Hour), 0},
			{"muse-recorded", "muse", "acme/a", "feature/shared", UnattributedIssueID, "issue-1", "unique_read_time_recovery", merge.Add(-time.Hour), 0},
			{"foreign-repo", "jsonl", "acme/b", "feature/shared", UnattributedIssueID, UnattributedIssueID, "unresolved", merge.Add(-time.Hour), 0},
			{"unqualified", "jsonl", "unqualified", "feature/shared", UnattributedForeignRepoBucket, UnattributedForeignRepoBucket, "unresolved", merge.Add(-time.Hour), 0},
			{"missing-evidence-opencode", "opencode", "acme/a", "", UnattributedIssueID, UnattributedIssueID, "unresolved", merge.Add(-time.Hour), 0},
			{"default", "jsonl", "acme/a", "main", UnattributedMainBucket, UnattributedMainBucket, "unresolved", merge.Add(-time.Hour), 0},
			{"revert-remainder", "jsonl", "acme/a", "revert-99-feature/shared", UnattributedNoIssueBucket, UnattributedNoIssueBucket, "unresolved", merge.Add(-time.Hour), 0},
			{"wrong-assigned-U93-A", "jsonl", "acme/a", "feature/shared", "issue-wrong", "issue-wrong", "assigned_reference", merge.Add(-time.Hour), 0},
			{"ambiguous-inclusive-boundary", "jsonl", "acme/a", "feature/shared", UnattributedIssueID, UnattributedIssueID, "ambiguous", merge, 0},
			{"lead-in-inclusive", "jsonl", "acme/a", "feature/shared", UnattributedIssueID, "issue-1", "unique_read_time_recovery", oldStart, 0},
			{"before-lead-in", "jsonl", "acme/a", "feature/shared", UnattributedIssueID, UnattributedIssueID, "unresolved", oldStart.Add(-time.Nanosecond), 0},
			{"after-final-merge", "jsonl", "acme/a", "feature/shared", UnattributedIssueID, UnattributedIssueID, "unresolved", merge.Add(48*time.Hour + time.Nanosecond), 0},
			{"pr-only", "jsonl", "acme/a", "", UnattributedIssueID, "issue-1", "permitted_existing_fill", merge.Add(-time.Hour), 1},
			{"pr-conflicts-with-branch", "jsonl", "acme/a", "feature/second", UnattributedIssueID, "issue-1", "permitted_existing_fill", merge.Add(-time.Hour), 1},
			{"equal-merge-claimants", "jsonl", "acme/a", "feature/tie", UnattributedIssueID, UnattributedIssueID, "ambiguous", merge.Add(-time.Hour), 0},
			{"not-github", "jsonl", "acme/a", "feature/api", UnattributedIssueID, UnattributedIssueID, "unresolved", merge.Add(-time.Hour), 0},
			{"existing-fill-wins", "jsonl", "acme/a", "feature/shared", UnattributedIssueID, "issue-fill", "permitted_existing_fill", merge.Add(-time.Hour), 999},
			{"sealed", "jsonl", "acme/a", "feature/sealed", UnattributedIssueID, UnattributedIssueID, "protected", sealMonth(7).Add(24 * time.Hour), 0},
			{"sealed-lead-band", "jsonl", "acme/a", "feature/band", UnattributedIssueID, UnattributedIssueID, "protected", sealMonth(7).Add(-14 * 24 * time.Hour), 0},
			{"sealed-existing-fill", "jsonl", "acme/a", "", UnattributedIssueID, "issue-fill", "protected", sealMonth(7).Add(24 * time.Hour), 999},
		}
		for i, tc := range cases {
			e := TokenEvent{Developer: "alice", Repo: tc.repo, IssueID: tc.stored, Source: tc.source, Fidelity: "realtime", CostMicro: int64(i + 1), InputTok: 2000, Timestamp: tc.at, IdempotencyKey: tc.name, SessionID: "one-cwd-many-branches", BranchHash: issueref.BranchHash(tc.repo, tc.branch)}
			if tc.pr > 0 {
				n := tc.pr
				e.PRNumber = &n
			}
			if err := db.InsertTokenEvent(ctx, e); err != nil {
				t.Fatal(tc.name, err)
			}
			if err := db.InsertTokenEvent(ctx, e); err != nil {
				t.Fatal("replay", err)
			}
		}
		old := recoveryOutcome(t, db, 1, "feature/shared", "issue-1", merge.Add(-24*time.Hour), merge)
		next := recoveryOutcome(t, db, 2, "feature/shared", "issue-2", merge.Add(24*time.Hour), merge.Add(48*time.Hour))
		outcomes := []Outcome{next, old, recoveryOutcome(t, db, 3, "feature/second", "issue-3", merge.Add(-time.Hour), merge), recoveryOutcome(t, db, 4, "feature/tie", "issue-4", merge.Add(-time.Hour), merge), recoveryOutcome(t, db, 5, "feature/tie", "issue-5", merge.Add(-time.Hour), merge), recoveryOutcome(t, db, 6, "revert-99-feature/shared", "issue-6", merge.Add(-time.Hour), merge), recoveryOutcome(t, db, 7, "feature/sealed", "issue-7", sealMonth(7), sealMonth(7).Add(48*time.Hour)), recoveryOutcome(t, db, 8, "feature/band", "issue-8", sealMonth(7).Add(-15*24*time.Hour), sealMonth(7).Add(-13*24*time.Hour))}
		api := recoveryOutcome(t, db, 9, "feature/api", "issue-9", merge.Add(-time.Hour), merge)
		api.Source = OutcomeSourceAPI
		outcomes = append(outcomes, api)
		if reverse {
			for i, j := 0, len(outcomes)-1; i < j; i, j = i+1, j-1 {
				outcomes[i], outcomes[j] = outcomes[j], outcomes[i]
			}
		}
		for _, o := range outcomes {
			var err error
			if o.Source == OutcomeSourceAPI {
				_, err = db.InsertOutcome(ctx, o)
			} else {
				_, err = db.RecordPROutcome(ctx, o)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		fill := recoveryOutcome(t, db, 999, "feature/fill", "issue-fill", merge, merge)
		if _, err := db.RecordPROutcome(ctx, fill); err != nil {
			t.Fatal(err)
		}
		sealed, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "sealed bytes"), nil, nil, SealCheck{Floor: sealMonth(7)})
		if err != nil {
			t.Fatal(err)
		}
		before, _, err := db.ListTokenEvents(ctx, sealMonth(5), sealMonth(8), PageCursor{}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		rawBefore, _ := json.Marshal(before)
		receipt, err := db.RecoveryReceipt(ctx, sealMonth(5), sealMonth(8), FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		if err := receipt.Validate(); err != nil {
			t.Fatal(err)
		}
		byID := map[int64]RecoveryReceiptEvent{}
		for _, e := range receipt.Events {
			byID[e.ID] = e
		}
		var expectedCandidateCost int64
		for i, tc := range cases {
			e := byID[int64(i+1)]
			if e.IssueID != tc.want || e.Disposition != tc.disposition {
				t.Errorf("%s: issue=%s disposition=%s want %s/%s tags=%v", tc.name, e.IssueID, e.Disposition, tc.want, tc.disposition, e.ReasonTags)
			}
			if tc.disposition != "assigned_reference" {
				expectedCandidateCost += int64(i + 1)
			}
		}
		correct, wrong := 0, 0
		for i, tc := range cases {
			if tc.disposition == "unique_read_time_recovery" {
				if byID[int64(i+1)].IssueID == tc.want {
					correct++
				} else {
					wrong++
				}
			}
		}
		t.Logf("fixture reverse=%v recovered=%d correct=%d wrong=%d recovered_candidate_cost_share=%.6f", reverse, receipt.Dispositions["unique_read_time_recovery"].Events, correct, wrong, receipt.RecoveredCostShare)
		if wrong != 0 || correct != int(receipt.Dispositions["unique_read_time_recovery"].Events) {
			t.Fatal("fixture recovery precision", correct, wrong)
		}
		if receipt.Candidates.Events != int64(len(cases)-1) || receipt.Candidates.CostMicro != expectedCandidateCost || len(receipt.Reasons) != len(recoveryReasonTags) || receipt.Reasons["sessions_jsonl_gone"].Measured {
			t.Fatal(receipt)
		}
		after, _, err := db.ListTokenEvents(ctx, sealMonth(5), sealMonth(8), PageCursor{}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		rawAfter, _ := json.Marshal(after)
		var sealedAfter []byte
		if err := db.db.QueryRow(`SELECT body FROM sealed_report WHERE id=?`, sealed.ID).Scan(&sealedAfter); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rawBefore, rawAfter) || !bytes.Equal(sealed.Body, sealedAfter) {
			t.Fatal("read changed stored evidence or sealed bytes")
		}
		// A second receipt and a switched-off live read cannot alter the stored row.
		again, err := db.RecoveryReceipt(ctx, sealMonth(5), sealMonth(8), FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := json.Marshal(receipt)
		replay, _ := json.Marshal(again)
		if !bytes.Equal(result, replay) {
			t.Fatal("receipt replay differs")
		}
		off, err := db.EffectiveAttributionsWindow(WithReadTimeRecovery(ctx, false), sealMonth(5), sealMonth(8), FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range off {
			if e.IssueID != e.StoredIssueID {
				t.Fatal("disabled recovery", e)
			}
		}
		return result
	}
	newest, oldest := run(false), run(true)
	if !bytes.Equal(newest, oldest) {
		t.Fatalf("insertion order changed receipt:\n%s\n%s", newest, oldest)
	}
}

func TestRecoveryReceiptRefusesBrokenAccounting(t *testing.T) {
	r := RecoveryReceipt{Dispositions: map[string]RecoveryTally{}, Candidates: RecoveryTally{Events: 1, CostMicro: 7}}
	for _, name := range recoveryDispositions {
		r.Dispositions[name] = RecoveryTally{}
	}
	if r.Validate() == nil {
		t.Fatal("accepted lost candidate")
	}
	r.Dispositions["unresolved"] = r.Candidates
	if r.Validate() == nil {
		t.Fatal("accepted missing terminal event")
	}
	r.Events = []RecoveryReceiptEvent{{EffectiveAttribution: EffectiveAttribution{ID: 1, CostMicro: 7}, Disposition: "unresolved"}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Events = append(r.Events, r.Events[0])
	if r.Validate() == nil {
		t.Fatal("accepted duplicate event")
	}
}

func TestRecoveryRecordedPRRequiresUniqueMergedWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := t.Context()
	pr := 42
	opened := fillMerge.Add(-time.Hour)
	for _, tc := range []struct {
		name, branch string
		at           time.Time
		want         string
	}{
		{"pr-only", "", opened, "issue-42"},
		{"conflicting-branch", "feature/else", opened, UnattributedIssueID},
		{"outside-window", "", opened.Add(-BranchFillLeadIn - time.Nanosecond), UnattributedIssueID},
	} {
		seedFillEvent(t, db, tc.name, "acme/a", tc.branch, UnattributedIssueID, tc.at, &pr)
	}
	o := fillOutcome("historical", opened, fillMerge)
	o.Source = "backfill"
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	rows, err := db.EffectiveAttributionsWindow(ctx, fillMerge.Add(-10*24*time.Hour), fillMerge.Add(time.Hour), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.StoredIssueID != UnattributedIssueID {
			t.Fatal("changed stored attribution", e)
		}
		want := UnattributedIssueID
		if e.Timestamp.Equal(opened) && e.ID == 1 {
			want = "issue-42"
		}
		if e.IssueID != want {
			t.Fatal(e, want)
		}
	}
	duplicate := o
	duplicate.MergeCommitSHA = "other-merge"
	duplicate.IssueID = "issue-other"
	if _, err := db.InsertOutcome(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.RecoveryReceipt(ctx, fillMerge.Add(-10*24*time.Hour), fillMerge.Add(time.Hour), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Dispositions["ambiguous"].Events != 1 || receipt.Dispositions["unique_read_time_recovery"].Events != 0 {
		t.Fatal(receipt)
	}
}

func BenchmarkMergedPRRecoveryWindow(b *testing.B) {
	db, err := Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := b.Context()
	at := fillMerge
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		hash := issueref.BranchHash("acme/a", fmt.Sprint("feature/", i+1))
		if _, err := tx.ExecContext(ctx, `INSERT INTO outcomes(developer,repo,issue_id,pr_number,weight,quality,source,created_at,merged_at,head_branch_hash,ts) VALUES('alice','acme/a',?,?,4,1,'backfill',?,?,?,?)`, fmt.Sprint("issue-", i+1), i+1, at.Add(-time.Hour), at, hash, at); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 40000; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO token_events(developer,issue_id,model,cost_micro,source,fidelity,repo,branch_hash,ts) VALUES('alice','unattributed','test',1,'jsonl','realtime','acme/a',?,?)`, issueref.BranchHash("acme/a", fmt.Sprint("feature/", i%500+1)), at.Add(-time.Minute)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.DeveloperIssueCostsWindow(ctx, at.Add(-time.Hour), at.Add(time.Hour), FleetWide)
		if err != nil {
			b.Fatal(err)
		}
		var cost int64
		for _, r := range rows {
			cost += r.ReadTimeRecoveryMicro
		}
		if len(rows) != 500 || cost != 40000 {
			b.Fatal(len(rows), cost)
		}
	}
}

type recoveryCountingQuerier struct {
	readQuerier
	calls int
}

func (q *recoveryCountingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "event_population AS MATERIALIZED") {
		q.calls++
	}
	return q.readQuerier.QueryContext(ctx, query, args...)
}

func TestRecoveryCostFoldOneSQLStatement(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := t.Context()
	at := fillMerge
	seedFillEvent(t, db, "recovery", "acme/a", "feature/topic", UnattributedIssueID, at, nil)
	o := recoveryOutcome(t, db, 42, "feature/topic", "issue-42", at.Add(-time.Hour), at.Add(time.Hour))
	if _, err := db.RecordPROutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	q := &recoveryCountingQuerier{readQuerier: db.db}
	costs, err := (reader{q}).DeveloperIssueCostsWindow(ctx, at.Add(-time.Hour), at.Add(time.Hour), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 || len(costs) != 1 || costs[0].IssueID != "issue-42" || costs[0].ReadTimeRecoveryEvents != 1 {
		t.Fatal(q.calls, costs)
	}
}

func TestRecoveryUnresolvedClaimantsStillMakeAmbiguity(t *testing.T) {
	for _, issue := range []string{"", UnattributedIssueID, UnattributedNoIssueBucket} {
		t.Run(issue, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			at := fillMerge
			seedFillEvent(t, db, "candidate", "acme/a", "feature/topic", UnattributedIssueID, at, nil)
			unknown := recoveryOutcome(t, db, 41, "feature/topic", issue, at.Add(-time.Hour), at.Add(time.Hour))
			if _, err := db.InsertOutcome(t.Context(), unknown); err != nil {
				t.Fatal(err)
			}
			receipt, err := db.RecoveryReceipt(t.Context(), at.Add(-time.Hour), at.Add(time.Hour), FleetWide)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Reasons["claimant_without_issue"].Events != 1 || receipt.Dispositions["unresolved"].Events != 1 || receipt.Events[0].IssueID != UnattributedIssueID {
				t.Fatal(receipt)
			}
			valid := recoveryOutcome(t, db, 42, "feature/topic", "issue-42", at.Add(-time.Hour), at.Add(time.Hour))
			if _, err := db.InsertOutcome(t.Context(), valid); err != nil {
				t.Fatal(err)
			}
			receipt, err = db.RecoveryReceipt(t.Context(), at.Add(-time.Hour), at.Add(time.Hour), FleetWide)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Dispositions["ambiguous"].Events != 1 || receipt.Events[0].IssueID != UnattributedIssueID {
				t.Fatal(receipt)
			}
		})
	}
}

func TestRecoveryIncompleteEvidenceAndNonPRSources(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Outcome, *TokenEvent)
		recover bool
	}{
		{"valid", func(*Outcome, *TokenEvent) {}, true},
		{"missing-created", func(o *Outcome, _ *TokenEvent) { o.CreatedAt = nil }, false},
		{"missing-merged", func(o *Outcome, _ *TokenEvent) { o.MergedAt = nil }, false},
		{"created-after-merged", func(o *Outcome, _ *TokenEvent) { at := o.MergedAt.Add(time.Hour); o.CreatedAt = &at }, false},
		{"not-a-pr", func(o *Outcome, _ *TokenEvent) { o.PRNumber = 0 }, false},
		{"missing-head", func(o *Outcome, _ *TokenEvent) { o.HeadBranchHash = nil }, false},
		{"pr-only-revert-without-head", func(o *Outcome, e *TokenEvent) { o.HeadBranchHash = nil; e.BranchHash = nil; e.PRNumber = &o.PRNumber }, false},
		{"api-source", func(o *Outcome, _ *TokenEvent) { o.Source = OutcomeSourceAPI }, false},
		{"unknown-source", func(o *Outcome, _ *TokenEvent) { o.Source = "unknown" }, false},
		{"missing-event-evidence", func(_ *Outcome, e *TokenEvent) { e.BranchHash = nil }, false},
		{"main-sentinel", func(_ *Outcome, e *TokenEvent) { e.IssueID = UnattributedMainBucket }, false},
		{"conflicting-pr", func(_ *Outcome, e *TokenEvent) { pr := 99; e.PRNumber = &pr }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			o := recoveryOutcome(t, db, 42, "feature/topic", "issue-42", fillMerge.Add(-time.Hour), fillMerge.Add(time.Hour))
			if tc.name == "pr-only-revert-without-head" {
				o = recoveryOutcome(t, db, 42, "revert-99-feature/topic", "issue-42", fillMerge.Add(-time.Hour), fillMerge.Add(time.Hour))
			}

			e := TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: UnattributedIssueID, CostMicro: 1, Source: "jsonl", Fidelity: "realtime", Timestamp: fillMerge, BranchHash: o.HeadBranchHash}
			tc.mutate(&o, &e)
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			if _, err := db.InsertOutcome(t.Context(), o); err != nil {
				t.Fatal(err)
			}
			rows, err := db.EffectiveAttributionsWindow(t.Context(), fillMerge.Add(-time.Hour), fillMerge.Add(time.Hour), FleetWide)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || (rows[0].Layer == AttributionRecovery) != tc.recover {
				t.Fatal(rows)
			}
		})
	}
}

func TestRecoverySealedProtectionNanosecondEdges(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := t.Context()
	cutoff := sealMonth(7).Add(-14 * 24 * time.Hour)
	times := []time.Time{cutoff.Add(-time.Nanosecond), cutoff, sealMonth(8).Add(-time.Nanosecond), sealMonth(8)}
	for i, at := range times {
		seedFillEvent(t, db, fmt.Sprint(i), "acme/a", "feature/topic", UnattributedIssueID, at, nil)
	}
	o := recoveryOutcome(t, db, 42, "feature/topic", "issue-42", cutoff.Add(-time.Hour), sealMonth(8).Add(time.Hour))
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(7), sealMonth(8), "cfg", "bytes"), nil, nil, SealCheck{Floor: sealMonth(7)}); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.RecoveryReceipt(ctx, sealMonth(6), sealMonth(9), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Dispositions["protected"].Events != 2 || receipt.Dispositions["unique_read_time_recovery"].Events != 2 {
		t.Fatal(receipt)
	}
	for _, e := range receipt.Events {
		want := AttributionRecovery
		if e.Timestamp.Equal(times[1]) || e.Timestamp.Equal(times[2]) {
			want = AttributionUnresolved
		}
		if e.Layer != want {
			t.Fatal(e, want)
		}
	}
}

func TestRecoveryReceiptRejectsIntegerOverflow(t *testing.T) {
	r := RecoveryReceipt{Candidates: RecoveryTally{Events: 2, CostMicro: math.MaxInt64}, Dispositions: map[string]RecoveryTally{}}
	for _, name := range recoveryDispositions {
		r.Dispositions[name] = RecoveryTally{}
	}
	r.Dispositions["unresolved"] = RecoveryTally{Events: 1, CostMicro: math.MaxInt64}
	r.Dispositions["ambiguous"] = RecoveryTally{Events: 1, CostMicro: 1}
	if r.Validate() == nil {
		t.Fatal("accepted overflowing terminal totals")
	}
	r.Dispositions["unresolved"] = r.Candidates
	r.Dispositions["ambiguous"] = RecoveryTally{}
	r.Events = []RecoveryReceiptEvent{{EffectiveAttribution: EffectiveAttribution{ID: 1, CostMicro: math.MaxInt64}, Disposition: "unresolved"}, {EffectiveAttribution: EffectiveAttribution{ID: 2, CostMicro: 1}, Disposition: "unresolved"}}
	if r.Validate() == nil {
		t.Fatal("accepted overflowing event totals")
	}
}

func TestOutcomeTokenTotalsRecoveryEvaluatedOnce(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	at := fillMerge
	var outcomes []Outcome
	for i := 0; i < tokenTotalsBatchSize+61; i++ {
		issue := fmt.Sprint("issue-", i)
		e := TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: UnattributedIssueID, CostMicro: 1, InputTok: 2000, Source: "jsonl", Fidelity: "realtime", Timestamp: at, BranchHash: issueref.BranchHash("acme/a", issue)}
		if err := db.InsertTokenEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		o := recoveryOutcome(t, db, i+1, issue, issue, at.Add(-time.Hour), at.Add(time.Hour))
		if _, err := db.InsertOutcome(t.Context(), o); err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, o)
	}
	q := &recoveryCountingQuerier{readQuerier: db.db}
	totals, err := (reader{q}).OutcomeTokenTotals(t.Context(), outcomes, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 || len(totals) != len(outcomes) {
		t.Fatalf("recovery evaluations=%d totals=%d outcomes=%d", q.calls, len(totals), len(outcomes))
	}
	for _, total := range totals {
		if total != 2000 {
			t.Fatal(total)
		}
	}
	q.calls = 0
	costs, err := (reader{q}).DeveloperIssueCostsWindow(t.Context(), at.Add(-time.Hour), at.Add(2*time.Hour), FleetWide)
	if err != nil || q.calls != 1 || len(costs) != len(outcomes) {
		t.Fatalf("cost recovery evaluations=%d costs=%d: %v", q.calls, len(costs), err)
	}
	for _, c := range costs {
		if c.ReadTimeRecoveryEvents != 1 {
			t.Fatal(c)
		}
	}

}

type droppingRecoveryQuerier struct{ readQuerier }

func (q droppingRecoveryQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "event_population AS MATERIALIZED") {
		query = "SELECT * FROM (" + query + ") WHERE id<0"
	}
	return q.readQuerier.QueryContext(ctx, query, args...)
}
func TestRecoveryReceiptRejectsDroppedProjectionRow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedFillEvent(t, db, "lost", "acme/a", "feature/topic", UnattributedIssueID, fillMerge, nil)
	_, err := (reader{droppingRecoveryQuerier{db.db}}).RecoveryReceipt(t.Context(), fillMerge.Add(-time.Hour), fillMerge.Add(time.Hour), RepoScope("acme/a"))
	if err == nil || !strings.Contains(err.Error(), "stored totals") {
		t.Fatalf("dropped event accepted: %v", err)
	}
}

func BenchmarkOutcomeTokenTotalsRecovery(b *testing.B) {
	for _, n := range []int{300, 1500} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			db, err := Open(b.TempDir() + "/bench.db")
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			at := fillMerge
			var outcomes []Outcome
			tx, err := db.db.BeginTx(b.Context(), nil)
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < n; i++ {
				issue := fmt.Sprint("issue-", i)
				hash := issueref.BranchHash("acme/a", issue)
				if _, err := tx.ExecContext(b.Context(), `INSERT INTO outcomes(developer,repo,issue_id,pr_number,weight,quality,source,created_at,merged_at,head_branch_hash,ts) VALUES('alice','acme/a',?,?,4,1,'backfill',?,?,?,?)`, issue, i+1, at.Add(-time.Hour), at, hash, at); err != nil {
					b.Fatal(err)
				}
				outcomes = append(outcomes, Outcome{Repo: "acme/a", IssueID: issue, Timestamp: at})
			}
			for i := 0; i < 40000; i++ {
				issue := fmt.Sprint("issue-", i%n)
				stored := issue
				if (i/n)%2 == 1 {
					stored = UnattributedIssueID
				}
				if _, err := tx.ExecContext(b.Context(), `INSERT INTO token_events(developer,issue_id,model,cost_micro,input_tok,source,fidelity,repo,branch_hash,ts) VALUES('alice',?,'test',1,2000,'jsonl','realtime','acme/a',?,?)`, stored, issueref.BranchHash("acme/a", issue), at.Add(-time.Minute)); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			var offNS float64
			for _, enabled := range []bool{false, true} {
				b.Run(fmt.Sprint(enabled), func(b *testing.B) {
					ctx := WithReadTimeRecovery(b.Context(), enabled)
					b.ResetTimer()
					started := time.Now()
					for i := 0; i < b.N; i++ {
						totals, err := db.OutcomeTokenTotals(ctx, outcomes, FleetWide)
						if err != nil || len(totals) != n {
							b.Fatal(len(totals), err)
						}
					}
					ns := float64(time.Since(started).Nanoseconds()) / float64(b.N)
					if !enabled {
						offNS = ns
					} else {
						ratio := ns / offNS
						b.ReportMetric(ratio, "vs-off")
						if ratio > 10 {
							b.Fatalf("recovery %.2fx exceeds 10x off", ratio)
						}
					}
				})
			}
		})
	}
}
