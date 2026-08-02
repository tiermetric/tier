package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedExportEvent inserts one token_event at ts with an empty idempotency key
// (so every call lands a fresh, unkeyed row) and returns nothing — the test
// reconstructs the expected set from the store.
func seedExportEvent(t *testing.T, db *DB, dev, issue string, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer: dev,
		IssueID:   issue,
		Model:     "claude-sonnet-4",
		InputTok:  1000,
		CostMicro: 12_345,
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

// TestListTokenEvents_KeysetWalk_NoGapNoDup is the core pagination proof: seed
// more rows than a page holds — including MANY rows sharing an identical ts, the
// case the (ts, id) tiebreak exists for — then page through to exhaustion and
// assert the union of every page equals the full set exactly once (no gaps, no
// duplicates), in strict (ts, id) order.
func TestListTokenEvents_KeysetWalk_NoGapNoDup(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const total = 250
	// Half the rows share ONE ts to stress the id tiebreak; the rest are spread
	// across distinct seconds. All fall inside the window below.
	for i := 0; i < total; i++ {
		ts := base
		if i%2 == 0 {
			ts = base.Add(time.Duration(i) * time.Second)
		}
		seedExportEvent(t, db, "alice", "issue-1", ts)
	}

	since := base.Add(-time.Hour)
	until := base.Add(48 * time.Hour)
	const pageSize = 40

	seenIDs := map[int64]int{}
	var lastTS time.Time
	var lastID int64
	cursor := PageCursor{}
	pages := 0
	got := 0
	for {
		events, hasMore, err := db.ListTokenEvents(ctx, since, until, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListTokenEvents page %d: %v", pages, err)
		}
		if len(events) > pageSize {
			t.Fatalf("page %d returned %d rows, exceeds page size %d", pages, len(events), pageSize)
		}
		for _, e := range events {
			seenIDs[e.ID]++
			got++
			// Strict (ts, id) ordering across the whole walk.
			if pages > 0 || got > 1 {
				if e.Timestamp.Before(lastTS) || (e.Timestamp.Equal(lastTS) && e.ID <= lastID) {
					t.Fatalf("order violation: (%s,%d) not strictly after (%s,%d)",
						e.Timestamp, e.ID, lastTS, lastID)
				}
			}
			lastTS, lastID = e.Timestamp, e.ID
		}
		pages++
		if !hasMore {
			break
		}
		last := events[len(events)-1]
		cursor = PageCursor{TS: last.Timestamp, ID: last.ID}
		if pages > total {
			t.Fatalf("walk did not terminate; pages=%d", pages)
		}
	}
	if got != total {
		t.Fatalf("walked %d rows, want %d", got, total)
	}
	for id, n := range seenIDs {
		if n != 1 {
			t.Errorf("row id=%d returned %d times, want exactly 1", id, n)
		}
	}
	if len(seenIDs) != total {
		t.Errorf("distinct rows walked = %d, want %d", len(seenIDs), total)
	}
}

// TestListTokenEvents_WindowHalfOpen proves [since, until): a row exactly at
// since is included, a row exactly at until is excluded.
func TestListTokenEvents_WindowHalfOpen(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	since := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	seedExportEvent(t, db, "alice", "before", since.Add(-time.Second)) // excluded
	seedExportEvent(t, db, "alice", "at-since", since)                 // included
	seedExportEvent(t, db, "alice", "mid", since.Add(72*time.Hour))    // included
	seedExportEvent(t, db, "alice", "at-until", until)                 // excluded (half-open)
	seedExportEvent(t, db, "alice", "after", until.Add(time.Second))   // excluded

	events, hasMore, err := db.ListTokenEvents(ctx, since, until, PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore = true, want false (all rows fit one page)")
	}
	got := map[string]bool{}
	for _, e := range events {
		got[e.IssueID] = true
	}
	if !got["at-since"] || !got["mid"] {
		t.Errorf("expected at-since and mid in window; got %v", got)
	}
	for _, excluded := range []string{"before", "at-until", "after"} {
		if got[excluded] {
			t.Errorf("%q must be outside the half-open window [since, until)", excluded)
		}
	}
}

// TestListTokenEvents_EmptyRange returns an empty page and no next cursor when
// the window contains nothing.
func TestListTokenEvents_EmptyRange(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	seedExportEvent(t, db, "alice", "x", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	// Window entirely after the only row.
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	events, hasMore, err := db.ListTokenEvents(ctx, since, until, PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 0 || hasMore {
		t.Errorf("empty range: got %d rows hasMore=%v, want 0/false", len(events), hasMore)
	}
}

// TestListExports_ClampLimit proves the store never materializes more than
// MaxExportPageSize rows even when a caller asks for more, and defaults a
// non-positive limit.
func TestListExports_ClampLimit(t *testing.T) {
	if got := clampExportLimit(0); got != DefaultExportPageSize {
		t.Errorf("clampExportLimit(0) = %d, want default %d", got, DefaultExportPageSize)
	}
	if got := clampExportLimit(-5); got != DefaultExportPageSize {
		t.Errorf("clampExportLimit(-5) = %d, want default %d", got, DefaultExportPageSize)
	}
	if got := clampExportLimit(MaxExportPageSize + 1); got != MaxExportPageSize {
		t.Errorf("clampExportLimit(over-max) = %d, want %d", got, MaxExportPageSize)
	}
	if got := clampExportLimit(42); got != 42 {
		t.Errorf("clampExportLimit(42) = %d, want 42", got)
	}
}

// TestListOutcomes_KeysetWalk mirrors the token-events walk for outcomes,
// including ts ties, and asserts no gaps / no duplicates.
func TestListOutcomes_KeysetWalk(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	const total = 120
	for i := 0; i < total; i++ {
		ts := base
		if i%3 == 0 {
			ts = base.Add(time.Duration(i) * time.Minute)
		}
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer:    "alice",
			IssueID:      "issue-1",
			PRNumber:     i + 1,
			Weight:       1.0,
			Quality:      1.0,
			WeightSource: WeightSourceHeuristic,
			Source:       OutcomeSourceAPI,
			Timestamp:    ts,
			// distinct SHA per row so the merge-commit unique index admits all.
			MergeCommitSHA: "sha-" + strconv.Itoa(i),
		}); err != nil {
			t.Fatalf("InsertOutcome %d: %v", i, err)
		}
	}

	since := base.Add(-time.Hour)
	until := base.Add(72 * time.Hour)
	const pageSize = 25
	seen := map[int64]int{}
	cursor := PageCursor{}
	got, pages := 0, 0
	for {
		outcomes, hasMore, err := db.ListOutcomes(ctx, since, until, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListOutcomes page %d: %v", pages, err)
		}
		for _, o := range outcomes {
			seen[o.ID]++
			got++
		}
		pages++
		if !hasMore {
			break
		}
		last := outcomes[len(outcomes)-1]
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
			t.Errorf("outcome id=%d returned %d times, want 1", id, n)
		}
	}
}

// TestListExports_UseKeysetIndex asserts the planner satisfies the export scan
// with the (ts, id) covering index — no full-window sort / temp b-tree — on both
// tables. Mirrors the #72 planner-shape guard.
func TestListExports_UseKeysetIndex(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	cases := []struct {
		table string
		sql   string
		index string
	}{
		{
			table: "token_events",
			index: "idx_token_events_ts_id",
			sql: `SELECT id, developer FROM token_events
			      WHERE ts >= ? AND ts < ? AND (ts > ? OR (ts = ? AND id > ?))
			      ORDER BY ts, id LIMIT ?`,
		},
		{
			table: "outcomes",
			index: "idx_outcomes_ts_id",
			sql: `SELECT id, developer FROM outcomes
			      WHERE ts >= ? AND ts < ? AND (ts > ? OR (ts = ? AND id > ?))
			      ORDER BY ts, id LIMIT ?`,
		},
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		rows, err := db.db.Query("EXPLAIN QUERY PLAN "+tc.sql,
			base, base.Add(time.Hour), base, base, int64(0), 10)
		if err != nil {
			t.Fatalf("%s EXPLAIN: %v", tc.table, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				_ = rows.Close()
				t.Fatalf("scan plan: %v", err)
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		_ = rows.Close()
		p := plan.String()
		if !strings.Contains(p, tc.index) {
			t.Errorf("%s export does not use %s; plan:\n%s", tc.table, tc.index, p)
		}
		if strings.Contains(p, "USE TEMP B-TREE") {
			t.Errorf("%s export sorts via temp b-tree (index order not used); plan:\n%s", tc.table, p)
		}
	}
}
