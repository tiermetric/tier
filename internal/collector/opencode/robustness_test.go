package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// fixedClock returns a deterministic now().
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// testNow is the instant every clock-horizon test measures against. The golden
// completion stamp sits just before it, so ordinary fixtures are inside the
// horizon and a poison row can be placed outside it without any sleeping.
var testNow = time.UnixMilli(goldenCompleted).Add(time.Minute)

// runOnePass drives one real Run pass (loadWatermark + runPass) and returns the
// events it ingested. Used instead of Collect where the WATERMARK is the thing
// under test — Collect is stateless with respect to it by design.
func runOnePass(t *testing.T, c *Collector) []collector.TokenEvent {
	t.Helper()
	var got []collector.TokenEvent
	c.runPass(context.Background(), time.Time{}, collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		got = append(got, ev)
		return nil
	}))
	return got
}

// ─────────────────────────────────────────────────────────────────────────────
// THE CLOCK HORIZON
// ─────────────────────────────────────────────────────────────────────────────

// TestFutureTimeUpdatedDoesNotPoisonTheWatermark is the regression guard for the
// failure mode that is worst precisely because it looks healthiest.
//
// 🔴 WHAT HAPPENED WITHOUT THE HORIZON, measured end to end: one row carrying a
// microsecond value in the millisecond `time_updated` column (a unit mix-up or a
// skewed device — no attacker needed) advanced the watermark to the year 58628
// and PERSISTED it. `setWatermark` only moves forward and the checkpoint survives
// restarts, so no real row ever matched `time_updated >= floor` again. And because
// the poison row itself kept matching, `rowsRead` stayed non-zero, so every loud
// arm in reportPass was bypassed: "opencode scan complete" at INFO, every five
// minutes, forever, capturing nothing.
//
// The test asserts the three things that together make that impossible: the
// watermark does not move past the horizon, a legitimate row inserted afterwards
// is still captured, and the skip is counted.
func TestFutureTimeUpdatedDoesNotPoisonTheWatermark(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")
	good := goldenCompleted
	// A microsecond value in a millisecond column — 1000x too large.
	poison := goldenCompleted * 1000

	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{
			buildRow(t, msgSpec{
				ID: "msg_good", SessionID: "ses_good", Cwd: cwd,
				Created: good - 1000, Completed: &good, TimeUpdated: good,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
			}),
			buildRow(t, msgSpec{
				ID: "msg_poison", SessionID: "ses_poison", Cwd: cwd,
				Created: good - 1000, Completed: &good, TimeUpdated: poison,
				Tokens: autoTotal(1000, 10, 5, 50, 0),
			}),
		},
	})

	logger, cap := newTestLogger()
	cp := &memCheckpoints{}
	c, err := New(Config{
		DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
		Logger: logger, Checkpoints: cp, Interval: time.Millisecond,
		Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.loadWatermark(context.Background())
	got := runOnePass(t, c)

	// The poison row is DROPPED, and the good one is not: a corrupt `time_updated`
	// is not a reason to lose everything else in the store.
	if len(got) != 1 {
		t.Fatalf("want exactly the plausible row captured; got %d events (%v)", len(got), got)
	}
	if got[0].SessionID != "ses_good" {
		t.Errorf("captured %q; the future-stamped row must not be the one that survived", got[0].SessionID)
	}
	if c.watermark > testNow.Add(maxClockSkew).UnixMilli() {
		t.Fatalf("the watermark advanced to %d, past the clock horizon — one corrupt row has pinned the resume point and the collector will capture nothing from here on",
			c.watermark)
	}
	if c.watermark != good {
		t.Errorf("watermark = %d, want the newest PLAUSIBLE time_updated %d", c.watermark, good)
	}
	if saved, ok := cp.rows[c.checkpointKey()]; ok && strings.Contains(saved.Metadata, fmt.Sprintf("%d", poison)) {
		t.Errorf("the poisoned watermark was PERSISTED, so it survives a restart: %s", saved.Metadata)
	}
	if got := cap.find(slog.LevelWarn, "opencode scan complete"); len(got) != 1 {
		t.Errorf("a pass that skipped a future-stamped row must summarise at WARN, not INFO — a quiet summary is exactly how this hid. Records:%s", cap.dump())
	} else if n := attrOf(got[0], "skipped_future_time_updated"); n != "1" {
		t.Errorf("skipped_future_time_updated = %q, want \"1\"", n)
	}
}

// TestImplausibleCompletionStampIsSkippedLoudly guards the OTHER half of the same
// hazard: the completion stamp becomes the event's `ts`, which every window query
// reads. A far-future ts satisfies `ts >= since` for every window from now until
// then, so a corrupt row's cost is added to that developer's spend indefinitely —
// and the store holds ts INSERT-only (#235), so a corrected re-read cannot pull it
// back. A non-positive stamp is refused for the mirror-image reason: it stores as
// 1970-or-earlier and silently drops out of every window.
func TestImplausibleCompletionStampIsSkippedLoudly(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")

	cases := []struct {
		name      string
		completed int64
	}{
		{"far future", testNow.Add(365 * 24 * time.Hour).UnixMilli()},
		{"zero", 0},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completed := tc.completed
			dbPath := newFixtureDB(t, dbSpec{
				Migrations: 3,
				Rows: []msgRow{buildRow(t, msgSpec{
					ID: "msg_ts", SessionID: "ses_ts", Cwd: cwd,
					Created: goldenCompleted, Completed: &completed, TimeUpdated: goldenCompleted,
					Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
				})},
			})
			logger, cap := newTestLogger()
			c, err := New(Config{
				DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
				Logger: logger, Now: fixedClock(testNow),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			events, err := c.Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("a row stamped %d must produce NO event; got one at ts %s", tc.completed, events[0].Timestamp)
			}
			if got := cap.find(slog.LevelWarn, "outside the plausible range"); len(got) != 1 {
				t.Errorf("the skip must be LOUD; got %d matching WARNs. Records:%s", len(got), cap.dump())
			}
		})
	}

	t.Run("CONTROL: an ordinary stamp is accepted", func(t *testing.T) {
		// Without this, every assertion above would also pass on an implementation
		// that rejected EVERY timestamp — which is total capture loss dressed up as
		// a working guard.
		completed := goldenCompleted
		dbPath := newFixtureDB(t, dbSpec{
			Migrations: 3,
			Rows: []msgRow{buildRow(t, msgSpec{
				ID: "msg_ok", SessionID: "ses_ok", Cwd: cwd,
				Created: completed - 1000, Completed: &completed, TimeUpdated: completed,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
			})},
		})
		logger, _ := newTestLogger()
		c, err := New(Config{
			DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
			Logger: logger, Now: fixedClock(testNow),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		events, err := c.Collect(context.Background(), time.Time{})
		if err != nil || len(events) != 1 {
			t.Fatalf("an ordinary stamp must be accepted; got %d events, err %v", len(events), err)
		}
	})
}

// TestPersistedWatermarkBeyondTheHorizonIsDiscarded closes the door the in-scan
// guard cannot: a checkpoint written by an older binary, or corrupted in place,
// would otherwise pin the resume point past every real row forever, because
// setWatermark only moves FORWARD. The checkpoint is a derived cache, so
// discarding it costs one idempotent re-scan — the cheap side of a very
// asymmetric trade.
func TestPersistedWatermarkBeyondTheHorizonIsDiscarded(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	dbPath := filepath.Join(t.TempDir(), "x.db")
	logger, cap := newTestLogger()
	c, err := New(Config{
		DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger,
		Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	poison := testNow.Add(1000 * 24 * time.Hour).UnixMilli()
	c.checkpoints = &memCheckpoints{rows: map[string]store.WatcherCheckpoint{
		c.checkpointKey(): {
			Path:     c.checkpointKey(),
			Metadata: fmt.Sprintf(`{"schema":%d,"opencode_time_updated_ms":%d,"opencode_migration_count":3}`, checkpointSchema, poison),
		},
	}}
	c.loadWatermark(context.Background())

	if c.watermark != 0 {
		t.Errorf("watermark = %d, want 0 (a poisoned checkpoint must be DISCARDED, not resumed)", c.watermark)
	}
	if got := cap.find(slog.LevelError, "beyond the plausible clock horizon"); len(got) != 1 {
		t.Errorf("discarding a poisoned watermark must be an ERROR, not a silent reset; got %d. Records:%s", len(got), cap.dump())
	}

	// CONTROL: a plausible checkpoint is still resumed, so the guard has not simply
	// disabled persistence.
	logger2, _ := newTestLogger()
	c2, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger2, Now: fixedClock(testNow)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c2.checkpoints = &memCheckpoints{rows: map[string]store.WatcherCheckpoint{
		c2.checkpointKey(): {
			Path:     c2.checkpointKey(),
			Metadata: fmt.Sprintf(`{"schema":%d,"opencode_time_updated_ms":%d,"opencode_migration_count":3}`, checkpointSchema, goldenCompleted),
		},
	}}
	c2.loadWatermark(context.Background())
	if c2.watermark != goldenCompleted {
		t.Errorf("a plausible watermark must be resumed; got %d, want %d", c2.watermark, goldenCompleted)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE BATCHING LOOP
// ─────────────────────────────────────────────────────────────────────────────

// withBatchLimit shrinks maxRowsPerPass for the calling test.
//
// It is why maxRowsPerPass is a var: proving the multi-batch loop with a real
// 20,000-row fixture would put a slow, large test into every `make check`, so the
// loop would stay unproven — and a loop that silently stops after its first batch
// is a capture hole that looks like a healthy scan.
func withBatchLimit(t *testing.T, n int) {
	t.Helper()
	prev := maxRowsPerPass
	maxRowsPerPass = n
	t.Cleanup(func() { maxRowsPerPass = prev })
}

// TestScanSpansMultipleBatches: every row is emitted exactly once across the
// batch boundary. Exactly-once is both halves — a loop that stopped early would
// under-report, and one whose floor did not advance would re-emit.
func TestScanSpansMultipleBatches(t *testing.T) {
	withZaiPrices(t)
	withBatchLimit(t, 2)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")

	const n = 5
	rows := make([]msgRow, 0, n)
	for i := 0; i < n; i++ {
		completed := goldenCompleted - int64(n-i)*1000
		rows = append(rows, buildRow(t, msgSpec{
			ID: fmt.Sprintf("msg_%02d", i), SessionID: "ses_batch", Cwd: cwd,
			Created: completed - 100, Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(100, 10, 5, 50, 0),
		}))
	}
	dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: rows})

	logger, _ := newTestLogger()
	c, err := New(Config{
		DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
		Logger: logger, Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	seen := map[string]int{}
	for _, ev := range events {
		seen[ev.IdempotencyKey]++
	}
	if len(events) != n {
		t.Errorf("got %d events from %d rows across %d-row batches; a short read means the loop stopped early",
			len(events), n, maxRowsPerPass)
	}
	if len(seen) != n {
		t.Errorf("got %d DISTINCT events from %d rows — the batch floor is re-reading rows it already emitted", len(seen), n)
	}
	for k, count := range seen {
		if count != 1 {
			t.Errorf("event %s emitted %d times; the overlap is re-emitting within a single pass", k, count)
		}
	}
}

// TestTieGroupLargerThanABatchIsPagedThrough is the case a timestamp-only cursor
// cannot handle: MORE ROWS SHARING ONE `time_updated` MILLISECOND THAN FIT IN A
// BATCH. With a `>= maxTS` continuation the identical batch comes back forever
// (a spinning collector that logs healthy scans); with `> maxTS` the unread
// remainder of the group is skipped forever (silent capture loss). The keyset on
// (time_updated, id) is what makes neither happen — and this test is the only
// thing that would notice a regression to either.
func TestTieGroupLargerThanABatchIsPagedThrough(t *testing.T) {
	withZaiPrices(t)
	withBatchLimit(t, 2)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")

	completed := goldenCompleted
	var rows []msgRow
	for i := 0; i < 3; i++ { // 3 rows, ONE timestamp, batch limit 2
		rows = append(rows, buildRow(t, msgSpec{
			ID: fmt.Sprintf("msg_tie_%d", i), SessionID: "ses_tie", Cwd: cwd,
			Created: completed - 100, Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(100, 10, 5, 50, 0),
		}))
	}
	dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: rows})

	logger, cap := newTestLogger()
	c, err := New(Config{
		DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
		Logger: logger, Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type result struct {
		events []collector.TokenEvent
		err    error
	}
	done := make(chan result, 1)
	go func() {
		evs, err := c.Collect(context.Background(), time.Time{})
		done <- result{evs, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan did not terminate — the cursor is not advancing through the tie group")
	}
	if res.err != nil {
		t.Fatalf("Collect: %v", res.err)
	}
	// EXACTLY ONCE, both halves. Three rows share one millisecond and the batch
	// holds two, so the group necessarily straddles a batch boundary.
	seen := map[string]int{}
	for _, ev := range res.events {
		seen[ev.IdempotencyKey]++
	}
	if len(res.events) != 3 {
		t.Errorf("got %d events for a 3-row tie group; fewer means the remainder was skipped, more means the boundary was re-read", len(res.events))
	}
	if len(seen) != 3 {
		t.Errorf("got %d distinct events for 3 rows", len(seen))
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("event %s emitted %d times", k, n)
		}
	}
	if got := cap.find(slog.LevelError, ""); len(got) != 0 {
		t.Errorf("paging through a tie group is ordinary and must log no ERROR; got %d. Records:%s", len(got), cap.dump())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PER-ROW SKIPS — each one is counted, and the loud ones are loud
// ─────────────────────────────────────────────────────────────────────────────

// TestPerRowSkipsAreCountedAndClassified walks every skip arm and asserts BOTH
// that the row produced no event AND which counter moved. A single "skipped"
// total cannot tell a healthy exclusion from a broken parse, and this collector's
// characteristic failure is a quiet scan — so the classification is the contract,
// not a nicety.
func TestPerRowSkipsAreCountedAndClassified(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")
	completed := goldenCompleted

	cases := []struct {
		name        string
		row         msgRow
		wantCounter string
		wantWarn    string // "" = must NOT be loud
	}{
		{
			name: "zero-token row (an aborted turn; 13 of 9,267 real rows)",
			row: buildRow(t, msgSpec{
				ID: "msg_zero", SessionID: "s", Cwd: cwd, Created: completed - 1,
				Completed: &completed, TimeUpdated: completed,
				Tokens: tokenSpec{}, // no total, all classes zero
			}),
			wantCounter: "skipped_zero_token",
			// Deliberately QUIET: 13 such rows exist in the real store and warning
			// about each would train an operator to ignore the warning that matters.
			wantWarn: "",
		},
		{
			name: "tokens but no total to verify them against",
			row: buildRow(t, msgSpec{
				ID: "msg_nototal", SessionID: "s", Cwd: cwd, Created: completed - 1,
				Completed: &completed, TimeUpdated: completed,
				Tokens: tokenSpec{Input: 100, Output: 10},
			}),
			wantCounter: "skipped_unverifiable",
			wantWarn:    "no `tokens.total`",
		},
		{
			name:        "undecodable JSON",
			row:         msgRow{ID: "msg_bad", SessionID: "s", TimeUpdated: completed, Data: `{"role":"assistant"` /* truncated */},
			wantCounter: "skipped_undecodable",
			wantWarn:    "did not decode",
		},
		{
			name: "a cwd outside every watched repo",
			row: buildRow(t, msgSpec{
				ID: "msg_foreign", SessionID: "s", Cwd: "/somewhere/else", Created: completed - 1,
				Completed: &completed, TimeUpdated: completed,
				Tokens: autoTotal(100, 10, 5, 50, 0),
			}),
			wantCounter: "skipped_foreign_repo",
			wantWarn:    "",
		},
		{
			name: "still streaming (no time.completed)",
			row: buildRow(t, msgSpec{
				ID: "msg_stream", SessionID: "s", Cwd: cwd, Created: completed - 1,
				Completed: nil, TimeUpdated: completed,
				Tokens: autoTotal(100, 10, 5, 50, 0),
			}),
			wantCounter: "skipped_incomplete",
			wantWarn:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: []msgRow{tc.row}})
			logger, cap := newTestLogger()
			// The repo carries an explicit Slug so the unrelated
			// "cannot determine repository slug" WARN cannot fire — otherwise the
			// QUIET assertion below would be measuring the wrong warning.
			c, err := New(Config{
				DBPath: dbPath, Repos: []RepoTarget{{Path: repo, Slug: "tiermetric/tier"}},
				DeveloperID: "alice", Logger: logger, Now: fixedClock(testNow),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			events, err := c.Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("want no event, got %d", len(events))
			}
			summary := cap.find(slog.LevelInfo, "opencode scan complete")
			if len(summary) != 1 {
				t.Fatalf("want one pass summary; got %d. Records:%s", len(summary), cap.dump())
			}
			if got := attrOf(summary[0], tc.wantCounter); got != "1" {
				t.Errorf("%s = %q, want \"1\" — the row was dropped but under the wrong counter, so an operator cannot tell a healthy exclusion from a broken parse",
					tc.wantCounter, got)
			}
			warns := cap.find(slog.LevelWarn, "")
			switch {
			case tc.wantWarn == "" && len(warns) != 0:
				t.Errorf("this skip must be QUIET (it is an ordinary condition); got %d WARNs. Records:%s", len(warns), cap.dump())
			case tc.wantWarn != "":
				if len(cap.find(slog.LevelWarn, tc.wantWarn)) == 0 {
					t.Errorf("this skip must be LOUD and name %q. Records:%s", tc.wantWarn, cap.dump())
				}
			}
		})
	}
}

// TestUnreadableRowCostsOnlyItself: a NULL in a column this collector requires is
// a third-party data defect. Scanning into plain Go types would return an error
// for the WHOLE batch, so one bad row would stop every row after it from ever
// being ingested — on every pass, forever. That contradicts the per-row posture
// the rest of the scan takes: a bad row must cost exactly itself.
func TestUnreadableRowCostsOnlyItself(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")
	completed := goldenCompleted

	good := buildRow(t, msgSpec{
		ID: "msg_zz_good", SessionID: "ses_good", Cwd: cwd, Created: completed - 1,
		Completed: &completed, TimeUpdated: completed,
		Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
	})
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		// The DDL drops NOT NULL on `data` so a NULL can actually be stored — the
		// shape a schema change or a partial write produces.
		MessageDDL: "CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, " +
			"`time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text)",
		Rows: []msgRow{good},
	})
	// Insert the NULL row directly; the fixture builder cannot express it.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture for the NULL row: %v", err)
	}
	if _, err := raw.Exec("INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_aa_null','s',?,?,NULL)",
		completed-1, completed-1); err != nil {
		t.Fatalf("insert NULL row: %v", err)
	}
	_ = raw.Close()

	logger, cap := newTestLogger()
	c, err := New(Config{
		DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
		Logger: logger, Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("a single unreadable row must not fail the whole scan: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the GOOD row must still be captured; got %d events", len(events))
	}
	if got := cap.find(slog.LevelWarn, "NULL in a column"); len(got) != 1 {
		t.Errorf("the unreadable row must be LOUD; got %d. Records:%s", len(got), cap.dump())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WINDOW, SLUG, AND THE OVERLAP ARITHMETIC
// ─────────────────────────────────────────────────────────────────────────────

// TestSinceWindowDropsOlderEvents: `ship --since` is an operator knob, and until
// this test nothing exercised the branch it controls.
func TestSinceWindowDropsOlderEvents(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")
	newer := goldenCompleted
	older := goldenCompleted - 30*24*60*60*1000 // 30 days earlier

	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{
			buildRow(t, msgSpec{ID: "msg_old", SessionID: "ses_old", Cwd: cwd,
				Created: older - 1, Completed: &older, TimeUpdated: older,
				Tokens: autoTotal(100, 10, 5, 50, 0)}),
			buildRow(t, msgSpec{ID: "msg_new", SessionID: "ses_new", Cwd: cwd,
				Created: newer - 1, Completed: &newer, TimeUpdated: newer,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0)}),
		},
	})
	logger, _ := newTestLogger()
	c, err := New(Config{
		DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
		Logger: logger, Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A window starting one day before the newer row.
	since := time.UnixMilli(newer).Add(-24 * time.Hour)
	events, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != 1 || events[0].SessionID != "ses_new" {
		t.Fatalf("want only the row inside the window; got %d events (%v)", len(events), events)
	}
	// CONTROL: the zero window admits both, so the filter is the thing being
	// tested rather than the fixture being wrong.
	all, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("the zero window must admit both rows; got %d", len(all))
	}
}

// TestRepoSlugIsStampedOnTheEvent. Nothing else asserts ev.Repo, and a wrong repo
// identity is the #231 failure: cost that can never join its outcomes, and two
// repos' issues sharing a number re-fusing.
func TestRepoSlugIsStampedOnTheEvent(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_slug", SessionID: "s", Cwd: filepath.Join(repo, "src"),
			Created: completed - 1, Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})

	t.Run("operator override wins", func(t *testing.T) {
		events, _ := collectFrom(t, dbPath, repo) // collectFrom sets Slug: tiermetric/tier
		if len(events) != 1 {
			t.Fatalf("want 1 event, got %d", len(events))
		}
		if events[0].Repo != "tiermetric/tier" {
			t.Errorf("Repo = %q, want the operator override %q", events[0].Repo, "tiermetric/tier")
		}
	})

	t.Run("no override and no remote degrades to the sentinel, loudly", func(t *testing.T) {
		logger, cap := newTestLogger()
		c, err := New(Config{
			DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice",
			Logger: logger, Now: fixedClock(testNow),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		events, err := c.Collect(context.Background(), time.Time{})
		if err != nil || len(events) != 1 {
			t.Fatalf("got %d events, err %v", len(events), err)
		}
		// Degrading is deliberate rather than fatal — a repo we cannot NAME still
		// produces true per-developer cost — but it must be observable.
		if events[0].Repo == "" {
			t.Error("Repo must never be empty; the store normalizes empty to the sentinel, but the producer should stamp it")
		}
		if got := cap.find(slog.LevelWarn, "cannot determine repository slug"); len(got) != 1 {
			t.Errorf("degrading to the unqualified sentinel must WARN; got %d. Records:%s", len(got), cap.dump())
		}
	})
}

// TestFloorClampsToZeroRatherThanGoingNegative. `floor()` subtracts an overlap
// from the watermark; on an early-epoch watermark that subtraction would go
// negative, and a negative floor would silently widen every scan to the whole
// store forever.
func TestFloorClampsToZeroRatherThanGoingNegative(t *testing.T) {
	repo := repoDir(t)
	c, err := New(Config{
		DBPath: filepath.Join(t.TempDir(), "x.db"), Repos: []RepoTarget{{Path: repo}},
		Interval: time.Hour, Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.floor(); got != 0 {
		t.Errorf("an unset watermark must floor at 0; got %d", got)
	}
	// A watermark smaller than the overlap: the subtraction would go negative.
	c.watermark = 1000
	if got := c.floor(); got != 0 {
		t.Errorf("floor() = %d for a watermark below the overlap; it must clamp to 0, never go negative", got)
	}
	// And an ordinary watermark is reduced by exactly the overlap.
	c.watermark = goldenCompleted
	wantLag := int64(watermarkLagFactor) * time.Hour.Milliseconds()
	if got, want := c.floor(), goldenCompleted-wantLag; got != want {
		t.Errorf("floor() = %d, want %d (watermark minus %d ms of overlap)", got, want, wantLag)
	}
}
