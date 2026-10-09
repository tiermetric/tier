package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// #723. modernc.org/sqlite stores a Go time.Time as TEXT in Go's own layout,
// "2006-01-02 15:04:05.999999999 -0700 MST", INCLUDING the zone suffix — so
// `ORDER BY ts` is a binary string sort over that text and the UTC offset sits
// AFTER the wall-clock field. Rows written with different offsets therefore sort
// by wall clock, not by instant, which inverts the (ts, id) total order #711
// established for reproducible scoring.
//
// These tests pin BOTH halves:
//
//   - the MECHANISM (TestTimestampZone_MixedOffsetsBreakSQLOrder) — a raw insert
//     that bypasses the store's writers, proving the hazard is real and that the
//     normalization below is load-bearing rather than incidentally satisfied.
//     If this ever stops inverting, the driver's storage format changed and the
//     constraint on every writer can be revisited.
//   - the FIX (TestOrderedTSWriters_NormalizeToUTC) — every Go writer of an
//     ordered ts column stores UTC even when handed a nonzero-offset input.
//
// ⚠️ Every zone here is an explicit time.FixedZone. None of these tests reads
// the machine's local zone, so the FIXTURES behave identically on a UTC CI box
// and on a developer's laptop in EDT — a zone test that depends on time.Local is
// this very bug wearing a test.
//
// 🔴 BUT A FIXED FIXTURE IS NOT ENOUGH FOR KILL POWER, and an earlier draft of
// this comment claimed it was. Measured: with normalizeTS mutated to t.Local(),
// this file PASSED under TZ=UTC and failed under every other zone — because on a
// zero-offset host t.Local() renders "+0000 UTC", identical to t.UTC(), and no
// assertion over the stored text can separate them. Docker images default to UTC,
// so that is a likely CI configuration, not an exotic one. The fix is that
// TestOrderedTSWriters_NormalizeToUTC now PINS time.Local to a nonzero offset,
// making the host zone irrelevant to what the MUTANT renders rather than merely
// irrelevant to what the fixture holds.

// edtZone is a fixed -04:00 zone. Fixed, never time.Local: see the note above.
func edtZone() *time.Location { return time.FixedZone("EDT", -4*60*60) }

// tsPairs returns two instants two hours apart, the LATER one rendered in a
// -04:00 zone and the EARLIER one in UTC. Rendering the later instant at a
// negative offset pulls its wall clock BELOW the earlier instant's, which is
// exactly the condition that makes a lexical sort disagree with instant order.
//
//	later  = 12:00Z, rendered "2026-04-01 08:00:00 -0400 EDT"
//	earlier= 10:00Z, rendered "2026-04-01 10:00:00 +0000 UTC"
//
// "08" < "10", so a string sort puts the LATER instant first.
func tsPairs() (later, earlier time.Time) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	return base.In(edtZone()), base.Add(-2 * time.Hour)
}

// trimZoneName drops the trailing zone NAME from a stored timestamp, leaving the
// date, clock and numeric offset that time.Parse can read back. The name is
// free-form ("UTC", "EDT", "+0000", "TIER-TEST-EDT") and time.Parse's MST verb
// cannot accept an arbitrary label, so it must be cut rather than parsed.
func trimZoneName(raw string) string {
	f := strings.Fields(raw)
	if len(f) < 3 {
		return raw // let time.Parse report the malformed value
	}
	return f[0] + " " + f[1] + " " + f[2]
}

// rawTS reads the on-disk TEXT of the ts column, in ORDER BY ts, id — i.e. what
// SQLite actually compares, not what Go parses back out.
func rawTS(t *testing.T, db *DB, table, col string) []string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT CAST(`+col+` AS TEXT) FROM `+table+` ORDER BY `+col+`, id`)
	if err != nil {
		t.Fatalf("read %s.%s: %v", table, col, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan %s.%s: %v", table, col, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %s.%s: %v", table, col, err)
	}
	return out
}

// TestTimestampZone_MixedOffsetsBreakSQLOrder is the CHARACTERIZATION arm: it
// asserts that mixed offsets really do invert, by writing the ts column with raw
// SQL so no store-level normalization can intervene. Without this, a green
// TestOrderedTSWriters_NormalizeToUTC would prove nothing — a normalization is
// only load-bearing if the un-normalized case is actually broken.
func TestTimestampZone_MixedOffsetsBreakSQLOrder(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	later, earlier := tsPairs()

	// Raw INSERT — deliberately NOT InsertOutcome, whose normalizeTS would
	// (correctly) prevent the very inversion this arm has to demonstrate.
	for _, r := range []struct {
		issue string
		ts    time.Time
	}{
		{"LATER-instant-at-minus-0400", later},
		{"EARLIER-instant-at-UTC", earlier},
	} {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO outcomes (developer, issue_id, weight, quality,
			                      weight_source, source, work_type, work_type_source, repo, ts)
			VALUES ('dev', ?, 1.0, 1.0, 'legacy', 'github-webhook', 'feature', 'default', 'r/r', ?)`,
			r.issue, r.ts); err != nil {
			t.Fatalf("raw insert %s: %v", r.issue, err)
		}
	}

	got := rawTS(t, db, "outcomes", "ts")
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %q", len(got), got)
	}

	// Vacuity control: the fixture is only mixing zones if the two stored
	// renderings carry DIFFERENT offsets. If both read "+0000", the fixture
	// collapsed to one zone and every assertion below is meaningless.
	if strings.Contains(got[0], "+0000") == strings.Contains(got[1], "+0000") {
		t.Fatalf("fixture is not mixing zones — both rows share an offset, so this "+
			"guard is vacuous: %q", got)
	}
	if !strings.Contains(got[0], "-0400") {
		t.Fatalf("fixture lost the -0400 rendering; got %q", got)
	}

	// THE INVERSION. ORDER BY ts returns the 08:00 -0400 row (12:00Z, the LATER
	// instant) first, ahead of the 10:00 +0000 row (10:00Z, the EARLIER one).
	if !strings.HasPrefix(got[0], "2026-04-01 08:00:00 -0400") {
		t.Fatalf("expected the lexical sort to place the -0400 row first "+
			"(this is the #723 inversion); got order %q", got)
	}
	if !strings.HasPrefix(got[1], "2026-04-01 10:00:00 +0000") {
		t.Fatalf("expected the +0000 row second; got order %q", got)
	}

	// Same two instants, both written in UTC: the lexical sort now agrees with
	// instant order. This is the control that proves the inversion above is
	// caused by the OFFSET and not by anything else in the fixture.
	db2, cleanup2 := newTestDB(t)
	defer cleanup2()
	for _, r := range []struct {
		issue string
		ts    time.Time
	}{
		{"LATER-instant-at-UTC", later.UTC()},
		{"EARLIER-instant-at-UTC", earlier.UTC()},
	} {
		if _, err := db2.db.ExecContext(ctx, `
			INSERT INTO outcomes (developer, issue_id, weight, quality,
			                      weight_source, source, work_type, work_type_source, repo, ts)
			VALUES ('dev', ?, 1.0, 1.0, 'legacy', 'github-webhook', 'feature', 'default', 'r/r', ?)`,
			r.issue, r.ts); err != nil {
			t.Fatalf("raw insert %s: %v", r.issue, err)
		}
	}
	utcGot := rawTS(t, db2, "outcomes", "ts")
	if !strings.HasPrefix(utcGot[0], "2026-04-01 10:00:00 +0000") ||
		!strings.HasPrefix(utcGot[1], "2026-04-01 12:00:00 +0000") {
		t.Fatalf("all-UTC control did not sort in instant order: %q", utcGot)
	}
}

// TestOrderedTSWriters_NormalizeToUTC is the COVERAGE arm. Every Go writer of an
// ordered ts column funnels through the store, so asserting the invariant here
// covers cmd/tierd/demo.go, the webhook handler, /api/v1/outcomes, /api/v1/events,
// cmd/tierd/backfill and every collector at once — a source-level scan of those
// call sites would be both weaker (it cannot see a value's provenance) and more
// brittle.
//
// Each case hands the writer a nonzero-offset timestamp and asserts what lands on
// disk is UTC. Remove normalizeTS from any one writer and exactly that subtest
// reddens, naming the offset it found.
func TestOrderedTSWriters_NormalizeToUTC(t *testing.T) {
	// Pin the process zone to a nonzero offset for the duration of this test.
	//
	// This is about MUTANT KILL POWER, not about the fixture (already an explicit
	// FixedZone). A normalizeTS mutated to t.Local() is indistinguishable from the
	// correct t.UTC() on a zero-offset host: both render "+0000 UTC". With
	// time.Local pinned here that mutant renders "+1245" and dies on every host,
	// including a UTC CI box. Restored on return.
	//
	// ⛔ Do NOT add t.Parallel() to this test: time.Local is process-global. It is
	// safe today only because this test is sequential, and Go resumes parallel
	// top-level tests after the sequential pass completes.
	restoreLocal := time.Local
	time.Local = time.FixedZone("TIER-TEST-CHATHAM", 12*60*60+45*60)
	defer func() { time.Local = restoreLocal }()
	if _, off := time.Now().Zone(); off == 0 {
		t.Fatal("time.Local substitution did not take — a t.Local() mutant would " +
			"survive this test undetected")
	}

	// 12:00Z rendered as 08:00 -0400 — the value the demo seeder used to produce.
	//
	// ⚠️ THE NANOSECONDS ARE LOAD-BEARING. With a whole-second fixture, a
	// `return t.UTC().Truncate(time.Second)` implementation of normalizeTS passes
	// this entire test — measured. That is a realistic "tidy the stored format"
	// edit, and it silently destroys the sub-second resolution #711's (ts, id)
	// order needs to separate near-simultaneous events. A nonzero nanosecond
	// component, checked by the instant comparison below, kills that mutant.
	nonUTC := time.Date(2026, 4, 1, 12, 0, 0, 123456789, time.UTC).In(edtZone())

	// Guard the fixture itself: if this ever renders "+0000" the input is already
	// UTC and every subtest below passes for free (#723's fixture-vacuity trap).
	if _, off := nonUTC.Zone(); off == 0 {
		t.Fatalf("fixture is not non-UTC — offset is 0, so this guard is vacuous")
	}
	if nonUTC.Nanosecond() == 0 {
		t.Fatalf("fixture lost its sub-second component — a truncating normalizeTS " +
			"would then pass this guard undetected")
	}

	cases := []struct {
		name  string
		table string
		col   string
		write func(t *testing.T, db *DB, ts time.Time)
	}{
		{
			name: "InsertOutcome", table: "outcomes", col: "ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				if _, err := db.InsertOutcome(context.Background(), Outcome{
					Developer: "dev", IssueID: "#1", Weight: 1, Quality: 1,
					MergeCommitSHA: "sha-insert-outcome", Timestamp: ts,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "UpsertPushOutcome", table: "outcomes", col: "ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				if _, err := db.UpsertPushOutcome(context.Background(), Outcome{
					Developer: "dev", IssueID: "#2", Weight: 0.5, Quality: 1,
					Repo: "r/r", Timestamp: ts,
				}, ts.UTC().Format("2006-01-02")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "InsertTokenEvent", table: "token_events", col: "ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				if err := db.InsertTokenEvent(context.Background(), TokenEvent{
					Developer: "dev", IssueID: "#3", Model: "m",
					Source: "jsonl", Fidelity: "realtime",
					IdempotencyKey: "k-single", Timestamp: ts,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "InsertTokenEvents", table: "token_events", col: "ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				if err := db.InsertTokenEvents(context.Background(), []TokenEvent{{
					Developer: "dev", IssueID: "#4", Model: "m",
					Source: "jsonl", Fidelity: "realtime",
					IdempotencyKey: "k-batch", Timestamp: ts,
				}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "RecordPushCommit", table: "push_outcome_commits", col: "ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				if _, err := db.RecordPushCommit(context.Background(), Outcome{
					Developer: "dev", IssueID: "#6", Weight: 0.5, Quality: 1,
					Repo: "r/r", Timestamp: ts,
				}, ts.UTC().Format("2006-01-02"), "sha-ledger", time.Time{}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The superseded push row's before-image.
			name: "RecordPROutcome audit", table: "push_outcome_audit", col: "outcome_ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				ctx := context.Background()
				if _, err := db.RecordPushCommit(ctx, Outcome{
					Developer: "dev", IssueID: "#7", Weight: 0.5, Quality: 1,
					Repo: "r/r", Timestamp: ts,
				}, ts.UTC().Format("2006-01-02"), "sha-squash", time.Time{}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.RecordPROutcome(ctx, Outcome{
					Developer: "dev", IssueID: "#7", Weight: 1, Quality: 1,
					MergeCommitSHA: "sha-squash", Repo: "r/r", Timestamp: ts,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Pre-existing since #134 — pinned here so the invariant is stated
			// once for every ordered ts column, not just the two #723 changed.
			name: "AppendQualityEvent", table: "quality_events", col: "event_ts",
			write: func(t *testing.T, db *DB, ts time.Time) {
				ctx := context.Background()
				if _, err := db.InsertOutcome(ctx, Outcome{
					Developer: "dev", IssueID: "#5", Weight: 1, Quality: 1,
					MergeCommitSHA: "sha-for-quality-event", Timestamp: ts,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.AppendQualityEvent(ctx, QualityEvent{
					OutcomeID: 1, Developer: "dev", IssueID: "#5",
					EventType: "revert_quality", SourceRef: "ref", EventTS: ts,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			tc.write(t, db, nonUTC)

			got := rawTS(t, db, tc.table, tc.col)
			if len(got) == 0 {
				t.Fatalf("%s wrote no row to %s.%s — the guard cannot observe anything",
					tc.name, tc.table, tc.col)
			}
			for _, raw := range got {
				if !strings.HasSuffix(raw, "+0000 UTC") {
					t.Errorf("%s stored a non-UTC %s.%s: %q\n"+
						"ORDER BY %s is a binary string sort over this text, so a nonzero "+
						"offset sorts by wall clock instead of by instant (#723).",
						tc.name, tc.table, tc.col, raw, tc.col)
				}
				// The instant must be preserved EXACTLY, to the nanosecond —
				// normalizeTS changes the rendering, never the value.
				//
				// ⚠️ This is a real instant comparison, not a string prefix. An
				// earlier draft asserted HasPrefix(raw, "2026-04-01 12:00:00"),
				// which fires for ANY non-UTC rendering — so it duplicated the
				// offset check above and reported "changed the INSTANT" on a
				// failure where the instant was preserved perfectly. Parsing and
				// comparing makes this arm independent of the suffix arm, and it
				// is what catches a truncating or rounding normalizeTS.
				stored, perr := time.Parse("2006-01-02 15:04:05.999999999 -0700", trimZoneName(raw))
				if perr != nil {
					t.Errorf("%s stored an unparseable %s.%s %q: %v",
						tc.name, tc.table, tc.col, raw, perr)
					continue
				}
				if !stored.Equal(nonUTC) {
					t.Errorf("%s changed the INSTANT, not just its rendering:\n"+
						"  stored %s\n  want   %s\n"+
						"normalizeTS must re-render the value, never alter it "+
						"(a truncating or rounding implementation lands here).",
						tc.name,
						stored.UTC().Format(time.RFC3339Nano),
						nonUTC.UTC().Format(time.RFC3339Nano))
				}
			}
		})
	}
}
