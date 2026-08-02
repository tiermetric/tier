package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"
)

// planString drains an EXPLAIN QUERY PLAN result set into its concatenated detail
// lines, so a test can assert which index the planner chose.
func planString(t *testing.T, rows *sql.Rows) string {
	t.Helper()
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		b.WriteString(detail)
		b.WriteString("\n")
	}
	return b.String()
}

// seedOutcomeForQuality inserts one API outcome and returns its id, so a test can
// hang quality_events / quality_history rows off a real outcome_id.
func seedOutcomeForQuality(t *testing.T, db *DB, dev, issue, sha string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: dev, IssueID: issue, Weight: 1, Quality: 1,
		WeightSource: WeightSourceHeuristic, Source: OutcomeSourceAPI,
		MergeCommitSHA: sha, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	var id int64
	if err := db.db.QueryRow(`SELECT id FROM outcomes WHERE merge_commit_sha = ?`, sha).Scan(&id); err != nil {
		t.Fatalf("select outcome id: %v", err)
	}
	return id
}

// TestListOutcomes_IncludesPushDay proves the outcomes bulk read surfaces push_day
// (#242): a source='push' row exports its UTC aggregation day, and a normal PR row
// exports "" (the NULL column), matching the merge_commit_sha convention.
func TestListOutcomes_IncludesPushDay(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// A push-captured outcome carries a non-null push_day.
	if _, err := db.UpsertPushOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-push", Weight: 0.5, Quality: 1,
		Repo: "acme/app", Timestamp: time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC),
	}, "2026-05-04"); err != nil {
		t.Fatalf("UpsertPushOutcome: %v", err)
	}
	// A normal PR outcome leaves push_day NULL.
	seedOutcomeForQuality(t, db, "bob", "issue-pr", "sha-pr")

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	outcomes, _, err := db.ListOutcomes(ctx, since, until, PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListOutcomes: %v", err)
	}
	byIssue := map[string]Outcome{}
	for _, o := range outcomes {
		byIssue[o.IssueID] = o
	}
	if got := byIssue["issue-push"].PushDay; got != "2026-05-04" {
		t.Errorf("push outcome push_day = %q, want 2026-05-04", got)
	}
	if got := byIssue["issue-pr"].PushDay; got != "" {
		t.Errorf("PR outcome push_day = %q, want empty string (NULL column)", got)
	}
}

// TestListQualityEvents_KeysetWalk pages the quality_events export and asserts the
// union of every page equals the full set exactly once, across event_ts ties.
func TestListQualityEvents_KeysetWalk(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	oid := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha-1")

	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	const total = 120
	for i := 0; i < total; i++ {
		ts := base
		if i%3 == 0 {
			ts = base.Add(time.Duration(i) * time.Minute)
		}
		// Distinct source_ref per row so the (outcome_id, event_type, source_ref)
		// unique key admits all; ci_pass is a valid event_type.
		if _, err := db.AppendQualityEvent(ctx, QualityEvent{
			OutcomeID: oid, Developer: "alice", IssueID: "issue-1",
			EventType: "ci_pass", SourceRef: "head-" + strconv.Itoa(i), EventTS: ts,
		}); err != nil {
			t.Fatalf("AppendQualityEvent %d: %v", i, err)
		}
	}

	since := base.Add(-time.Hour)
	until := base.Add(72 * time.Hour)
	const pageSize = 25
	seen := map[int64]int{}
	cursor := PageCursor{}
	got, pages := 0, 0
	for {
		events, hasMore, err := db.ListQualityEvents(ctx, since, until, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListQualityEvents page %d: %v", pages, err)
		}
		for _, e := range events {
			seen[e.ID]++
			got++
		}
		pages++
		if !hasMore {
			break
		}
		last := events[len(events)-1]
		cursor = PageCursor{TS: last.EventTS, ID: last.ID}
		if pages > total {
			t.Fatalf("walk did not terminate")
		}
	}
	if got != total || len(seen) != total {
		t.Fatalf("walked %d rows (%d distinct), want %d", got, len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("quality_event id=%d returned %d times, want 1", id, n)
		}
	}
}

// TestListQualityEvents_WindowAndEmpty proves the half-open [since, until) window
// and the empty-range case.
func TestListQualityEvents_WindowAndEmpty(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	oid := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha-1")

	mk := func(ref string, ts time.Time) {
		if _, err := db.AppendQualityEvent(ctx, QualityEvent{
			OutcomeID: oid, Developer: "alice", IssueID: "issue-1",
			EventType: "ci_pass", SourceRef: ref, EventTS: ts,
		}); err != nil {
			t.Fatalf("AppendQualityEvent: %v", err)
		}
	}
	mk("before", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	mk("in", time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))
	mk("at-until", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) // exactly until: excluded

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	events, hasMore, err := db.ListQualityEvents(ctx, since, until, PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListQualityEvents: %v", err)
	}
	if len(events) != 1 || events[0].SourceRef != "in" {
		t.Fatalf("window: got %+v, want only the in-window row", events)
	}
	if hasMore {
		t.Errorf("single-row page should report hasMore=false")
	}

	// Empty range.
	empty, hasMore, err := db.ListQualityEvents(ctx, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListQualityEvents empty: %v", err)
	}
	if len(empty) != 0 || hasMore {
		t.Errorf("empty range: got %d rows hasMore=%v, want 0/false", len(empty), hasMore)
	}
}

// TestListQualityHistory_KeysetWalk drives many quality transitions (whose ts is
// the second-precision SQLite CURRENT_TIMESTAMP, so ties within a second are the
// norm) and asserts the keyset walk loses/duplicates nothing — the case that broke
// when the cursor bound was a time.Time instead of the stored-layout string (#242).
func TestListQualityHistory_KeysetWalk(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const total = 60
	for i := 0; i < total; i++ {
		oid := seedOutcomeForQuality(t, db, "alice", "issue-"+strconv.Itoa(i), "sha-"+strconv.Itoa(i))
		// 1.0 -> 0.1 is a real transition, so each drives exactly one history row.
		if err := db.UpdateQualityForOutcome(ctx, oid, 0.1, "revert_quality", "ref-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("UpdateQualityForOutcome %d: %v", i, err)
		}
	}
	// Force EVERY row onto one identical second so the keyset MUST resolve the whole
	// block by its id tiebreak (ts = ? AND id > ?). CURRENT_TIMESTAMP would usually
	// but not deterministically produce ties; pinning them makes the tiebreak path —
	// the exact case that broke under a time.Time cursor bound — non-negotiable here.
	if _, err := db.db.ExecContext(ctx, `UPDATE quality_history SET ts = '2026-06-15 12:00:00'`); err != nil {
		t.Fatalf("pin ts: %v", err)
	}

	since := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	const pageSize = 7
	seen := map[int64]int{}
	cursor := PageCursor{}
	got, pages := 0, 0
	for {
		history, hasMore, err := db.ListQualityHistory(ctx, since, until, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListQualityHistory page %d: %v", pages, err)
		}
		for _, q := range history {
			seen[q.ID]++
			got++
		}
		pages++
		if !hasMore {
			break
		}
		last := history[len(history)-1]
		cursor = PageCursor{TS: last.Timestamp, ID: last.ID}
		if pages > total {
			t.Fatalf("walk did not terminate")
		}
	}
	if got != total || len(seen) != total {
		t.Fatalf("walked %d rows (%d distinct), want %d", got, len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("quality_history id=%d returned %d times, want 1", id, n)
		}
	}
	if pages < 2 {
		t.Errorf("expected multiple pages for %d rows at pageSize %d, got %d", total, pageSize, pages)
	}
}

// TestListQualityHistory_Empty returns an empty page and no cursor when the window
// holds no rows.
func TestListQualityHistory_Empty(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	history, hasMore, err := db.ListQualityHistory(ctx,
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListQualityHistory: %v", err)
	}
	if len(history) != 0 || hasMore {
		t.Errorf("empty range: got %d rows hasMore=%v, want 0/false", len(history), hasMore)
	}
}

// TestQualityExports_UseKeysetIndex asserts the planner satisfies the two quality
// exports with their (ts, id) covering index — no full-window sort — mirroring the
// TestListExports_UseKeysetIndex guard for the events/outcomes exports.
func TestQualityExports_UseKeysetIndex(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// quality_events keysets on event_ts with time.Time bounds (Go-written column).
	rowsE, err := db.db.Query(`EXPLAIN QUERY PLAN
		SELECT id FROM quality_events
		WHERE event_ts >= ? AND event_ts < ? AND (event_ts > ? OR (event_ts = ? AND id > ?))
		ORDER BY event_ts, id LIMIT ?`,
		base, base.Add(time.Hour), base, base, int64(0), 10)
	if err != nil {
		t.Fatalf("quality_events EXPLAIN: %v", err)
	}
	planE := planString(t, rowsE)
	if !strings.Contains(planE, "idx_quality_events_ts_id") {
		t.Errorf("quality_events export does not use idx_quality_events_ts_id; plan:\n%s", planE)
	}
	if strings.Contains(planE, "USE TEMP B-TREE") {
		t.Errorf("quality_events export sorts via temp b-tree; plan:\n%s", planE)
	}

	// quality_history keysets on ts with second-precision STRING bounds (the column
	// is written by CURRENT_TIMESTAMP), so the EXPLAIN binds strings too.
	const layout = "2006-01-02 15:04:05"
	sinceStr := base.Format(layout)
	untilStr := base.Add(time.Hour).Format(layout)
	rowsH, err := db.db.Query(`EXPLAIN QUERY PLAN
		SELECT id FROM quality_history
		WHERE ts >= ? AND ts < ? AND (ts > ? OR (ts = ? AND id > ?))
		ORDER BY ts, id LIMIT ?`,
		sinceStr, untilStr, sinceStr, sinceStr, int64(0), 10)
	if err != nil {
		t.Fatalf("quality_history EXPLAIN: %v", err)
	}
	planH := planString(t, rowsH)
	if !strings.Contains(planH, "idx_quality_history_ts_id") {
		t.Errorf("quality_history export does not use idx_quality_history_ts_id; plan:\n%s", planH)
	}
	if strings.Contains(planH, "USE TEMP B-TREE") {
		t.Errorf("quality_history export sorts via temp b-tree; plan:\n%s", planH)
	}
}
