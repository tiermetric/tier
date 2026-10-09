package store

import (
	"context"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// These tests pin the three corruptions #231 fixed. Every one of them PASSES on a
// repo-blind schema for the wrong reason, so each asserts the two-repo case
// explicitly rather than trusting a single-repo happy path.

// Corruption 1: idx_outcomes_push_daily was keyed (issue_id, push_day). Two repos
// pushing to their own issue #42 on the same UTC day collided, and
// ON CONFLICT DO NOTHING silently discarded the second repo's outcome.
func TestUpsertPushOutcome_TwoReposSameIssueSameDay_BothSurvive(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	day := "2026-07-09"

	insertedA, err := db.UpsertPushOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-42", Weight: 0.5, Quality: 1.0,
		Repo: "acme/app", Timestamp: ts,
	}, day)
	if err != nil || !insertedA {
		t.Fatalf("repo A push: inserted=%v err=%v", insertedA, err)
	}
	insertedB, err := db.UpsertPushOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-42", Weight: 0.5, Quality: 1.0,
		Repo: "acme/web", Timestamp: ts,
	}, day)
	if err != nil {
		t.Fatalf("repo B push: %v", err)
	}
	if !insertedB {
		t.Fatal("repo B's issue-42 was silently dropped by the push-daily dedup — this is the #231 RED")
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM outcomes WHERE issue_id='issue-42'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("outcomes for issue-42 = %d, want 2 (one per repo)", n)
	}
}

// The #196 idempotency guard must SURVIVE the reshape: a replay within one repo on
// one day is still a no-op. If repo had been nullable, SQLite's DISTINCT-NULLs rule
// would have quietly turned every replay into a fresh row.
func TestUpsertPushOutcome_ReplaySameRepoSameDay_StillDeduped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	o := Outcome{Developer: "alice", IssueID: "issue-42", Weight: 0.5, Quality: 1.0, Repo: "acme/app", Timestamp: ts}

	if inserted, err := db.UpsertPushOutcome(ctx, o, "2026-07-09"); err != nil || !inserted {
		t.Fatalf("first: inserted=%v err=%v", inserted, err)
	}
	inserted, err := db.UpsertPushOutcome(ctx, o, "2026-07-09")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("replay inserted a second row: the push-daily idempotency guard is broken")
	}
}

// A repo-blind push (sentinel) must also still dedup. This is the case a nullable
// column would have broken most visibly.
func TestUpsertPushOutcome_UnqualifiedReplay_StillDeduped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	o := Outcome{Developer: "alice", IssueID: "issue-7", Weight: 0.5, Quality: 1.0, Timestamp: ts} // Repo empty -> sentinel

	if inserted, err := db.UpsertPushOutcome(ctx, o, "2026-07-09"); err != nil || !inserted {
		t.Fatalf("first: inserted=%v err=%v", inserted, err)
	}
	if inserted, err := db.UpsertPushOutcome(ctx, o, "2026-07-09"); err != nil || inserted {
		t.Fatalf("sentinel replay inserted=%v err=%v, want inserted=false (NULL would have made these DISTINCT)", inserted, err)
	}
}

// Corruption 2: LatestOutcomeByIssue matched on issue id alone, so a revert of repo
// B's issue #42 applied the quality penalty to repo A's issue #42.
func TestLatestOutcomeByIssue_ScopedToRepo(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-42", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-a", Repo: "acme/app", Timestamp: ts,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-42", Weight: 5, Quality: 1.0,
		MergeCommitSHA: "sha-b", Repo: "acme/web", Timestamp: ts.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	// bob's row is NEWER. Pre-#231, ORDER BY id DESC handed it back for any lookup
	// of issue-42 — so a revert in acme/app would have penalised bob.
	got, ok, err := db.LatestOutcomeByIssue(ctx, "acme/app", "issue-42")
	if err != nil || !ok {
		t.Fatalf("lookup acme/app: ok=%v err=%v", ok, err)
	}
	if got.Developer != "alice" || got.Repo != "acme/app" {
		t.Fatalf("acme/app revert resolved to %s/%s — the penalty would land on the wrong repo", got.Repo, got.Developer)
	}

	got, ok, err = db.LatestOutcomeByIssue(ctx, "acme/web", "issue-42")
	if err != nil || !ok {
		t.Fatalf("lookup acme/web: ok=%v err=%v", ok, err)
	}
	if got.Developer != "bob" {
		t.Fatalf("acme/web resolved to %s, want bob", got.Developer)
	}
}

// Tolerance, not strictness: a pre-#231 (sentinel) outcome must STILL be reachable
// by a repo-qualified revert, or every historical revert would silently stop
// applying its penalty. This is the regression guard against someone "fixing" the
// join to a strict composite key.
func TestLatestOutcomeByIssue_TolerantTowardLegacyRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-9", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-legacy", Timestamp: ts, // Repo empty -> sentinel, like every pre-#231 row
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.LatestOutcomeByIssue(ctx, "acme/app", "issue-9")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a repo-qualified revert could not reach a legacy sentinel outcome; the join went strict and every historical revert now silently no-ops")
	}
	if got.Developer != "alice" {
		t.Fatalf("resolved to %s, want alice", got.Developer)
	}
}

// An exact repo match must WIN over a sentinel row, so tolerance decays to
// strictness on its own as qualified data accrues. No migration, no flag.
func TestLatestOutcomeByIssue_ExactRepoBeatsSentinel(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	// Sentinel row inserted LAST, so a naive "ORDER BY id DESC" would return it.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-5", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-1", Repo: "acme/app", Timestamp: ts,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "legacy", IssueID: "issue-5", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-2", Timestamp: ts.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.LatestOutcomeByIssue(ctx, "acme/app", "issue-5")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Developer != "alice" {
		t.Fatalf("resolved to %s, want alice — an exact repo match must outrank a sentinel row", got.Developer)
	}
}

// Within ONE repo, recency is insertion order (id DESC), NOT timestamp order.
// The repo-preference leg of the ORDER BY (`(repo = ?) DESC`) is a tie here — both
// rows are the same exact repo — so the id-vs-ts tiebreak is what this pins, which
// none of the two-repo / sentinel cases above exercise. A refactor to
// `ORDER BY ... ts DESC` flips it; the contract is
// `ORDER BY (repo = ?) DESC, id DESC` (store.go LatestOutcomeByIssue).
func TestLatestOutcomeByIssue_SameRepoRecencyIsInsertionOrder(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// bob inserted FIRST, carrying the LATER timestamp.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-9", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-bob", Repo: "acme/app", Timestamp: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// carol inserted SECOND, so she owns the higher id, but with the EARLIER ts.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "carol", IssueID: "issue-9", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-carol", Repo: "acme/app", Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.LatestOutcomeByIssue(ctx, "acme/app", "issue-9")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Developer != "carol" {
		t.Fatalf("resolved to %s, want carol — highest id wins; an ORDER BY ts DESC refactor would return bob", got.Developer)
	}
}

// Corruption 3: token cost pooled across repos on (developer, issue_id).
func TestDeveloperIssueCosts_NotPooledAcrossRepos(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	base := TokenEvent{Developer: "alice", IssueID: "issue-42", Model: "m", Source: "jsonl", Fidelity: "realtime", Timestamp: ts}
	a := base
	a.Repo, a.CostMicro, a.IdempotencyKey = "acme/app", 1000, "k1"
	b := base
	b.Repo, b.CostMicro, b.IdempotencyKey = "acme/web", 7000, "k2"
	for _, e := range []TokenEvent{a, b} {
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.DeveloperIssueCosts(ctx, ts.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byRepo := map[string]int64{}
	for _, c := range got {
		byRepo[c.Repo] += c.TotalCostMicro
	}
	if len(byRepo) != 2 {
		t.Fatalf("cost buckets = %v, want one per repo (pre-#231 both pooled into one)", byRepo)
	}
	if byRepo["acme/app"] != 1000 || byRepo["acme/web"] != 7000 {
		t.Fatalf("cost per repo = %v, want acme/app=1000 acme/web=7000", byRepo)
	}
}

// The tolerant join rule, unit-tested directly so the two call sites cannot drift.
func TestRepoMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"acme/app", "acme/app", true},
		{"acme/app", "acme/web", false}, // the #231 fix
		{repoid.Unqualified, "acme/app", true},
		{"acme/app", repoid.Unqualified, true},
		{repoid.Unqualified, repoid.Unqualified, true},
		{"", "acme/app", true},
	}
	for _, c := range cases {
		if got := RepoMatch(c.a, c.b); got != c.want {
			t.Errorf("RepoMatch(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestJoinIndex_TolerantSum(t *testing.T) {
	ix := BuildJoinIndex(map[DevIssue]int64{
		{Developer: "alice", Repo: "acme/app", IssueID: "issue-42"}:         100,
		{Developer: "alice", Repo: "acme/web", IssueID: "issue-42"}:         700,
		{Developer: "alice", Repo: repoid.Unqualified, IssueID: "issue-42"}: 5,
	})

	// A qualified outcome gets its own repo plus the repo-blind bucket.
	if got := ix.Sum("alice", "acme/app", "issue-42"); got != 105 {
		t.Errorf("qualified sum = %d, want 105 (own repo 100 + sentinel 5)", got)
	}
	if got := ix.Sum("alice", "acme/web", "issue-42"); got != 705 {
		t.Errorf("qualified sum = %d, want 705", got)
	}
	// A repo-blind outcome matches every bucket for its issue.
	if got := ix.Sum("alice", repoid.Unqualified, "issue-42"); got != 805 {
		t.Errorf("sentinel sum = %d, want 805 (all repos)", got)
	}
	// Never leaks across developers.
	if got := ix.Sum("bob", "acme/app", "issue-42"); got != 0 {
		t.Errorf("other developer sum = %d, want 0", got)
	}
}

// tokenTotalsBatchSize * params-per-window must stay under SQLite's 999-variable
// statement limit. #231 took the per-window parameter count from 3 to 4; at the old
// batch of 300 that is 1200 bound params and the statement fails — a latent crash
// reachable only on a large outcome set, i.e. only at a customer's site.
func TestOutcomeTokenTotals_ExceedsOneBatch(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	const n = tokenTotalsBatchSize + 25 // forces a second batch
	outcomes := make([]Outcome, 0, n)
	for i := range n {
		issue := "issue-" + itoa(i)
		outcomes = append(outcomes, Outcome{Developer: "alice", IssueID: issue, Repo: "acme/app", Timestamp: ts})
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: issue, Model: "m", InputTok: 1,
			Source: "jsonl", Fidelity: "realtime", Repo: "acme/app",
			IdempotencyKey: "k" + itoa(i), Timestamp: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}

	totals, err := db.OutcomeTokenTotals(ctx, outcomes, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals over %d windows: %v (SQLite variable limit? check tokenTotalsBatchSize)", n, err)
	}
	if len(totals) != n {
		t.Fatalf("totals = %d, want %d", len(totals), n)
	}
	if tokenTotalsBatchSize*4 >= 999 {
		t.Fatalf("tokenTotalsBatchSize*4 = %d, must stay < 999 SQLite bound-variable limit", tokenTotalsBatchSize*4)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The index is RENAMED, not edited. SQLite's CREATE UNIQUE INDEX IF NOT EXISTS is a
// no-op when an index of that NAME exists, whatever its columns — so an upgraded DB
// that kept 'idx_outcomes_push_daily' would silently retain the broken two-column
// shape while every migration step reported success. This test simulates exactly
// that upgrade: it plants the OLD index, reopens, and asserts the swap happened.
func TestMigration_PushDailyIndexIsReshapedNotSkipped(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier.db"

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Plant the pre-#231 index under its old name, as an upgraded DB would carry it.
	if _, err := db.db.Exec(`DROP INDEX IF EXISTS idx_outcomes_push_daily_repo`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_outcomes_push_daily
		ON outcomes(issue_id, push_day) WHERE source = 'push'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: Open() must drop the old name and create the repo-leading one.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after planting the legacy index: %v", err)
	}
	defer func() { _ = db2.Close() }()

	indexExists := func(name string) bool {
		var n int
		if err := db2.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if indexExists("idx_outcomes_push_daily") {
		t.Error("the old two-column index survived the migration; two repos' same-day pushes to the same issue number will still collide")
	}
	if !indexExists("idx_outcomes_push_daily_repo") {
		t.Fatal("the repo-leading unique index was not created")
	}

	// And it must actually enforce the new grain end to end.
	ctx := context.Background()
	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	for _, repo := range []string{"acme/app", "acme/web"} {
		inserted, err := db2.UpsertPushOutcome(ctx, Outcome{
			Developer: "alice", IssueID: "issue-42", Weight: 0.5, Quality: 1.0,
			Repo: repo, Timestamp: ts,
		}, "2026-07-09")
		if err != nil || !inserted {
			t.Fatalf("push for %s: inserted=%v err=%v", repo, inserted, err)
		}
	}
}

// REGRESSION (review RED, 2026-07-09): a repo-BLIND outcome must see the tokens its
// collector captured under a REAL repo slug.
//
// The first cut of OutcomeTokenTotals bound k.repo into the predicate
// unconditionally, so a sentinel window became `repo='unqualified' OR
// repo='unqualified'` and fetched only sentinel rows. Every GitLab/Bitbucket shop
// that POSTs outcomes without the optional `repo` field, while running the JSONL
// collector (which DOES resolve a real slug), would have had its well-funded
// outcomes falsely flagged zero-token — inverting the stated invariant that
// over-counting "can only fail to raise a flag, never raise a false one against an
// innocent developer".
func TestOutcomeTokenTotals_RepoBlindOutcomeSeesRealRepoTokens(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	// Collector captured cost under a real slug.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice", IssueID: "issue-42", Model: "m", InputTok: 5000,
		Source: "jsonl", Fidelity: "realtime", Repo: "group/proj",
		IdempotencyKey: "k1", Timestamp: ts,
	}); err != nil {
		t.Fatal(err)
	}

	// CI posted the outcome without a repo (the optional field), so it is sentinel.
	outcome := Outcome{Developer: "alice", IssueID: "issue-42", Timestamp: ts}

	totals, err := db.OutcomeTokenTotals(ctx, []Outcome{outcome}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	ix := BuildJoinIndex(totals)
	got := ix.Sum("alice", repoid.Unqualified, "issue-42")
	if got != 5000 {
		t.Fatalf("repo-blind outcome saw %d tokens, want 5000 — its real-repo cost was invisible, so the zero-token tripwire would raise a FALSE flag", got)
	}
}

// REGRESSION (review YELLOW): sentinel token rows must not be double-counted when an
// issue's windows straddle a batch boundary. Batches are cut on issue boundaries so
// one GROUP BY collapses an issue's sentinel rows exactly once.
func TestOutcomeTokenTotals_SentinelNotDoubleCountedAcrossBatches(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()

	// One repo-blind cost row on the shared issue.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice", IssueID: "shared", Model: "m", InputTok: 90,
		Source: "proxy", Fidelity: "realtime", // Repo empty -> sentinel
		IdempotencyKey: "s1", Timestamp: ts,
	}); err != nil {
		t.Fatal(err)
	}

	// Enough windows to force multiple batches, with the shared issue carried by two
	// different real repos.
	outcomes := []Outcome{
		{Developer: "alice", IssueID: "shared", Repo: "acme/one", Timestamp: ts},
		{Developer: "alice", IssueID: "shared", Repo: "acme/two", Timestamp: ts},
	}
	for i := range tokenTotalsBatchSize + 10 {
		outcomes = append(outcomes, Outcome{
			Developer: "alice", IssueID: "filler-" + itoa(i), Repo: "acme/one", Timestamp: ts,
		})
	}

	totals, err := db.OutcomeTokenTotals(ctx, outcomes, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	got := totals[DevIssue{Developer: "alice", Repo: repoid.Unqualified, IssueID: "shared"}]
	if got != 90 {
		t.Fatalf("sentinel bucket for 'shared' = %d, want 90 (counted once, not once per batch)", got)
	}
}
