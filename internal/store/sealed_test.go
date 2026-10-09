package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sealMonth returns 2026-<m>-01T00:00:00Z; m may run past 12 into the next year.
func sealMonth(m int) time.Time { return time.Date(2026, time.Month(m), 1, 0, 0, 0, 0, time.UTC) }

// testSealFloor is the earliest sealable period the store seal fixtures pass:
// July 2026, the month they seal first.
var testSealFloor = sealMonth(7)

func sealFixture(size string, start, end time.Time, config, body string) SealedReport {
	return SealedReport{
		Level: "team", PeriodSize: size, PeriodStart: start, PeriodEnd: end, K: 5,
		ConfigDigest: config, Body: []byte(body), BodyDigest: "sha256:" + body,
		ToolVersion: "v0.0.0-test", ToolCommit: "abc123",
	}
}

// sealOne seals July 2026 under cfg-a with one rollup and one person, alice.
func sealOne(t *testing.T, db *DB) SealedReport {
	t.Helper()
	r, _, err := db.SealReport(context.Background(), sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july"),
		[]SealedRollup{{Label: "core", WeightedPoints: 12.5, TotalCostUSD: 3.25, ActualPaidUSD: 1, RealtimeUSD: 2, SampleN: 6, FlaggedOutcomes: 1}},
		[]SealedPerson{{Label: "core", Measure: "cost", CanonicalID: "alice"}}, SealCheck{Floor: testSealFloor})
	if err != nil {
		t.Fatalf("SealReport: %v", err)
	}
	return r
}

// assertRefused runs a write that a trigger must abort, and checks the
// trigger's message and that the probe column is unchanged.
func assertRefused(t *testing.T, db *DB, stmt, probe string, want any) {
	t.Helper()
	_, err := db.db.Exec(stmt)
	if err == nil {
		t.Fatalf("%s succeeded — the append-only trigger is not in force", stmt)
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("%s: error = %v, want the trigger's RAISE(ABORT) naming append-only", stmt, err)
	}
	assertProbe(t, db, stmt, probe, want)
}

// assertIgnored runs a write that a first-wins trigger must turn into a no-op.
func assertIgnored(t *testing.T, db *DB, stmt, probe string, want any) {
	t.Helper()
	if _, err := db.db.Exec(stmt); err != nil {
		t.Fatalf("%s: %v, want RAISE(IGNORE)'s silent no-op", stmt, err)
	}
	assertProbe(t, db, stmt, probe, want)
}

func assertProbe(t *testing.T, db *DB, stmt, probe string, want any) {
	t.Helper()
	var got any
	if err := db.db.QueryRow(probe).Scan(&got); err != nil {
		t.Fatalf("probe %s: %v", probe, err)
	}
	if b, ok := got.([]byte); ok {
		got = string(b)
	}
	if got != want {
		t.Errorf("after %s: %s = %v, want %v unchanged", stmt, probe, got, want)
	}
}

// TestSealedReport_UpdateAborts: a sealed body is never rewritten (#914 A).
func TestSealedReport_UpdateAborts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	// Control: this database enforces constraints at all.
	if _, err := db.db.Exec(`INSERT INTO sealed_report (level) VALUES ('team')`); err == nil {
		t.Fatal("control: an INSERT violating NOT NULL succeeded, so a refused UPDATE below would prove nothing")
	}
	r := sealOne(t, db)
	assertRefused(t, db, `UPDATE sealed_report SET body = x'00'`,
		`SELECT body FROM sealed_report WHERE id = `+strconv.FormatInt(r.ID, 10), "july")
	assertRefused(t, db, `UPDATE sealed_report SET config_digest = 'cfg-b'`,
		`SELECT config_digest FROM sealed_report WHERE id = `+strconv.FormatInt(r.ID, 10), "cfg-a")
	// REPLACE deletes without firing UPDATE/DELETE triggers; the first-wins
	// BEFORE INSERT trigger keeps the row, whether the conflict is on the period
	// key or on the id.
	const cols = `level, period_size, period_start, period_end, k, config_digest, sealed_at, body, body_digest, tool_version, tool_commit`
	body := `SELECT body FROM sealed_report WHERE id = ` + strconv.FormatInt(r.ID, 10)
	assertIgnored(t, db, `INSERT OR REPLACE INTO sealed_report (`+cols+`)
		SELECT level, period_size, period_start, period_end, k, config_digest, sealed_at, x'00', body_digest, tool_version, tool_commit FROM sealed_report`, body, "july")
	assertIgnored(t, db, `REPLACE INTO sealed_report (id, `+cols+`)
		SELECT id, level, period_size, '1999-01-01T00:00:00Z', '1999-02-01T00:00:00Z', k, config_digest, sealed_at, x'00', body_digest, tool_version, tool_commit FROM sealed_report`, body, "july")
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 1)
}

// TestSealedRollup_UpdateAborts: the unserved pre-fold sums are never rewritten.
func TestSealedRollup_UpdateAborts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	sealOne(t, db)
	const probe = `SELECT sample_n FROM sealed_rollup WHERE label = 'core'`
	assertRefused(t, db, `UPDATE sealed_rollup SET sample_n = 1`, probe, int64(6))
	assertRefused(t, db, `INSERT OR REPLACE INTO sealed_rollup
		SELECT report_id, label, weighted_points, total_cost_usd, actual_paid_usd, realtime_usd, 1, flagged_outcomes,
		has_points, has_cost, has_realtime, has_non_realtime, has_paid, contributes FROM sealed_rollup`, probe, int64(6))
	assertRefused(t, db, `INSERT OR REPLACE INTO sealed_rollup (rowid, report_id, label, weighted_points, total_cost_usd, actual_paid_usd, realtime_usd, sample_n, flagged_outcomes)
		SELECT rowid, report_id, 'other', 0, 0, 0, 0, 1, 0 FROM sealed_rollup`, probe, int64(6))
}

// TestInstallSecret_UpdateAborts: the install secret is neither rewritten nor
// removed, so Open can never re-mint it under stored person keys.
func TestInstallSecret_UpdateAborts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	secret, err := installSecret(context.Background(), db.db)
	if err != nil {
		t.Fatal(err)
	}
	probe := `SELECT hex(secret) FROM install_secret WHERE id = 1`
	var want string
	if err := db.db.QueryRow(probe).Scan(&want); err != nil || len(want) != 2*len(secret) {
		t.Fatalf("control: hex(secret) = %q, %v", want, err)
	}
	assertRefused(t, db, `UPDATE install_secret SET secret = zeroblob(32)`, probe, want)
	assertRefused(t, db, `DELETE FROM install_secret`, probe, want)
	assertIgnored(t, db, `INSERT OR REPLACE INTO install_secret (id, secret) VALUES (1, zeroblob(32))`, probe, want)
	assertIgnored(t, db, `REPLACE INTO install_secret (id, secret) VALUES (1, zeroblob(32))`, probe, want)
}

// TestSealedPerson_OnlyTombstoneUpdateAdmitted: the one UPDATE admitted on
// sealed_person is #914's erase — person_key replaced by a new value while
// tombstoned goes 0 -> 1 — and every other transition aborts.
func TestSealedPerson_OnlyTombstoneUpdateAdmitted(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	sealOne(t, db)
	const probe = `SELECT hex(person_key) || '/' || tombstoned || '/' || label || '/' || measure FROM sealed_person`
	alice := sealedKeyHex(t, db, 7, "alice")

	for _, stmt := range []string{
		`UPDATE sealed_person SET person_key = 'other'`,                               // re-key without tombstoning
		`UPDATE sealed_person SET tombstoned = 1`,                                     // tombstone keeping the real key
		`UPDATE sealed_person SET person_key = 'tomb', tombstoned = 1, label = 'x'`,   // re-home under another label
		`UPDATE sealed_person SET person_key = 'tomb', tombstoned = 1, measure = 'x'`, // or another measure
		`UPDATE sealed_person SET person_key = 'tomb', tombstoned = 1, report_id = report_id + 1`,
		`UPDATE sealed_person SET person_key = 'tomb', tombstoned = 1, rowid = rowid + 100`,
		`INSERT OR REPLACE INTO sealed_person (rowid, report_id, label, measure, person_key)
			SELECT rowid, report_id, label, measure, 'other' FROM sealed_person`, // REPLACE on the rowid
	} {
		assertRefused(t, db, stmt, probe, alice+"/0/core/cost")
	}

	// The admitted transition.
	if _, err := db.db.Exec(`UPDATE sealed_person SET person_key = 'tomb-1', tombstoned = 1`); err != nil {
		t.Fatalf("the erase transition was refused: %v", err)
	}
	var got string
	const tomb1 = "746F6D622D31/1/core/cost" // hex('tomb-1')
	if err := db.db.QueryRow(probe).Scan(&got); err != nil || got != tomb1 {
		t.Fatalf("after the admitted tombstone: %q, %v; want %s", got, err, tomb1)
	}

	// A tombstone is final: neither re-keyed nor un-tombstoned.
	assertRefused(t, db, `UPDATE sealed_person SET person_key = 'tomb-2', tombstoned = 1`, probe, tomb1)
	assertRefused(t, db, `UPDATE sealed_person SET person_key = x'`+alice+`', tombstoned = 0`, probe, tomb1)
	assertRefused(t, db, `INSERT OR REPLACE INTO sealed_person (report_id, label, measure, person_key, tombstoned)
		SELECT report_id, label, measure, person_key, 0 FROM sealed_person`, probe, tomb1)
}

// TestSealedPerson_TombstoneCannotTakeAnotherRowsKey: UPDATE OR REPLACE onto a
// key another row holds would delete that row without firing its triggers
// (recursive_triggers is off); the tombstone trigger aborts it first.
func TestSealedPerson_TombstoneCannotTakeAnotherRowsKey(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	r, _, err := db.SealReport(context.Background(), sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july"), nil, nil, SealCheck{Floor: testSealFloor})
	if err != nil {
		t.Fatal(err)
	}
	// Literal keys, inserted directly, so the probe can name them.
	if _, err := db.db.Exec(`INSERT INTO sealed_person (report_id, label, measure, person_key)
		VALUES (?, 'core', 'cost', x'01'), (?, 'core', 'cost', x'02')`, r.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	const probe = `SELECT group_concat(hex(person_key) || ':' || tombstoned, ',') FROM (SELECT * FROM sealed_person ORDER BY person_key)`
	for _, verb := range []string{"UPDATE OR REPLACE", "UPDATE"} {
		assertRefused(t, db, verb+` sealed_person SET person_key = x'02', tombstoned = 1 WHERE person_key = x'01'`, probe, "01:0,02:0")
	}
}

// TestSealedReport_DeleteCascadesAndLoneChildDeleteAborts: foreign keys are off,
// so a period's fold inputs go with it by trigger, and neither can be deleted
// on its own while the period exists (an erase tombstones, never deletes).
func TestSealedReport_DeleteCascadesAndLoneChildDeleteAborts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	sealOne(t, db)
	assertRefused(t, db, `DELETE FROM sealed_person`, `SELECT COUNT(*) FROM sealed_person`, int64(1))
	assertRefused(t, db, `DELETE FROM sealed_rollup`, `SELECT COUNT(*) FROM sealed_rollup`, int64(1))
	if _, err := db.db.Exec(`DELETE FROM sealed_report`); err != nil {
		t.Fatalf("deleting a whole sealed period: %v", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_rollup`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_person`, 0)
}

// TestSealReport_InsertOrIgnoreKeepsFirst: sealing the identical span and config
// again returns the first seal's row and bytes and writes no second set of fold
// inputs, so concurrent first readers all serve the winner.
func TestSealReport_InsertOrIgnoreKeepsFirst(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	first := sealOne(t, db)
	if string(first.Body) != "july" || first.ID == 0 || first.SealedAt.IsZero() {
		t.Fatalf("first seal = %+v", first)
	}
	second, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "recomputed"),
		[]SealedRollup{{Label: "other", SampleN: 9}}, []SealedPerson{{Label: "other", Measure: "cost", CanonicalID: "k"}}, SealCheck{Floor: testSealFloor})
	if err != nil {
		t.Fatalf("second SealReport: %v", err)
	}
	if second.ID != first.ID || !bytes.Equal(second.Body, first.Body) || !second.SealedAt.Equal(first.SealedAt) {
		t.Errorf("second seal returned %+v, want the first row %+v", second, first)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_rollup WHERE label = 'core'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_rollup WHERE label = 'other'`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_person WHERE label = 'other'`, 0)

	// The lookup returns the stored config, whatever the caller's is now.
	got, err := db.SealedReport(ctx, "month", sealMonth(7))
	if err != nil || got.ID != first.ID || string(got.Body) != "july" ||
		got.ConfigDigest != "cfg-a" || got.Level != "team" || got.K != 5 {
		t.Errorf("SealedReport = %+v, %v; want the first row with its sealed config", got, err)
	}
	for _, miss := range []struct {
		size  string
		start time.Time
	}{{"quarter", sealMonth(7)}, {"month", sealMonth(8)}} {
		if _, err := db.SealedReport(ctx, miss.size, miss.start); !errors.Is(err, ErrSealedReportNotFound) {
			t.Errorf("SealedReport(%s, %s): err = %v, want ErrSealedReportNotFound", miss.size, miss.start, err)
		}
	}
}

// TestSealReport_OverlapUnderOtherConfigRefused is #913-D1 ruling A: a period
// sealed under any config blocks a second seal of an overlapping span, including
// month ⊂ quarter, which the unique index cannot see.
func TestSealReport_OverlapUnderOtherConfigRefused(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	sealOne(t, db) // July 2026, cfg-a

	_, _, err := db.SealReport(ctx, sealFixture("quarter", sealMonth(7), sealMonth(10), "cfg-b", "q3"), nil, nil, SealCheck{Floor: testSealFloor})
	if !errors.Is(err, ErrSealedPeriodOverlap) {
		t.Fatalf("Q3 under cfg-b over a sealed July: err = %v, want ErrSealedPeriodOverlap", err)
	}
	levelChanged := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "x")
	levelChanged.Level = "division"
	kChanged := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "x")
	kChanged.K = 9
	for name, r := range map[string]SealedReport{
		// The identical July span under another config: the ruling's core case.
		"same span, cfg-b": sealFixture("month", sealMonth(7), sealMonth(8), "cfg-b", "x"),
		// Same size and start, longer span.
		"wider span at July's start": sealFixture("month", sealMonth(7), sealMonth(9), "cfg-a", "x"),
		// Same size and end, later start.
		"later start, July's end": sealFixture("month", sealMonth(7).AddDate(0, 0, 14), sealMonth(8), "cfg-a", "x"),
		// Same span and digest, but a level or k the digest failed to carry.
		"same span, other level": levelChanged,
		"same span, other k":     kChanged,
	} {
		if _, _, err := db.SealReport(ctx, r, nil, nil, SealCheck{Floor: testSealFloor}); !errors.Is(err, ErrSealedPeriodOverlap) {
			t.Errorf("%s: err = %v, want ErrSealedPeriodOverlap", name, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 1)

	// Controls: non-overlapping periods seal under another config, each starting
	// exactly where the one before ends (spans are half-open).
	for _, r := range []SealedReport{
		sealFixture("month", sealMonth(8), sealMonth(9), "cfg-b", "aug"),
		sealFixture("quarter", sealMonth(9), sealMonth(12), "cfg-c", "q"),
	} {
		if _, _, err := db.SealReport(ctx, r, nil, nil, SealCheck{Floor: testSealFloor}); err != nil {
			t.Errorf("non-overlapping %s from %s: %v", r.PeriodSize, r.PeriodStart, err)
		}
	}
	got, err := db.SealedReportsOverlapping(ctx, sealMonth(7), sealMonth(9))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0].Body) != "july" || string(got[1].Body) != "aug" {
		t.Errorf("SealedReportsOverlapping(July..August) = %d rows %+v, want July then August", len(got), got)
	}
}

// TestSealReport_RefusesInvalidSpan: an empty, inverted or sub-second span is
// refused before anything is written.
func TestSealReport_RefusesInvalidSpan(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, r := range []SealedReport{
		sealFixture("month", sealMonth(7), sealMonth(7), "cfg-a", "empty"),
		sealFixture("month", sealMonth(8), sealMonth(7), "cfg-a", "inverted"),
		sealFixture("month", sealMonth(7).Add(time.Millisecond), sealMonth(8), "cfg-a", "subsecond"),
		sealFixture("mo\x00nth", sealMonth(7), sealMonth(8), "cfg-a", "nul-in-size"),
	} {
		if _, _, err := db.SealReport(context.Background(), r, nil, nil, SealCheck{Floor: testSealFloor}); !errors.Is(err, ErrSealedPeriodInvalid) {
			t.Errorf("%s: err = %v, want ErrSealedPeriodInvalid", r.Body, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 0)

	// A constraint violation surfaces rather than being ignored (a nil Body binds
	// NULL into a NOT NULL column).
	noBody := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "")
	noBody.Body = nil
	if _, _, err := db.SealReport(context.Background(), noBody, nil, nil, SealCheck{Floor: testSealFloor}); err == nil || !strings.Contains(err.Error(), "NOT NULL") {
		t.Errorf("nil body: err = %v, want the NOT NULL constraint error", err)
	}
}

// TestSealReport_ConcurrentSealersLeaveOneRow: racing sealers of one period, and
// of overlapping periods under different configs, leave exactly one sealed row;
// every loser is refused or answers the winner's row.
func TestSealReport_ConcurrentSealersLeaveOneRow(t *testing.T) {
	ctx := context.Background()
	const rounds, sealers = 20, 8
	completed := 0
	for round := 0; round < rounds; round++ {
		// A fresh database per round: each round's month is the first one sealed.
		db, cleanup := newTestDB(t)
		defer cleanup()
		jul := sealMonth(7 + 12*round)
		var want []SealedReport
		for i := 0; i < sealers; i++ {
			switch {
			case round%2 == 0: // one period, one config
				want = append(want, sealFixture("month", jul, jul.AddDate(0, 1, 0), "cfg-a", "july"))
			case i%2 == 0:
				want = append(want, sealFixture("month", jul, jul.AddDate(0, 1, 0), "cfg-a", "july"))
			default:
				want = append(want, sealFixture("quarter", jul, jul.AddDate(0, 3, 0), "cfg-b", "q3"))
			}
		}
		type result struct {
			r   SealedReport
			won bool
			err error
		}
		results := make(chan result, sealers)
		start := make(chan struct{})
		for _, r := range want {
			go func(r SealedReport) {
				<-start
				got, won, err := db.SealReport(ctx, r, nil, nil, SealCheck{Floor: jul})
				results <- result{got, won, err}
			}(r)
		}
		close(start)
		var winner int64
		wins := 0
		for i := 0; i < sealers; i++ {
			res := <-results
			if res.won {
				wins++
			}
			switch {
			case res.err == nil:
				completed++
				if winner == 0 {
					winner = res.r.ID
				} else if res.r.ID != winner {
					t.Errorf("round %d: two sealers succeeded with rows %d and %d", round, winner, res.r.ID)
				}
			case errors.Is(res.err, ErrSealedPeriodOverlap):
				completed++
			case errors.Is(res.err, ErrWriteLockUnavailable):
			default:
				t.Errorf("round %d: unexpected error %v", round, res.err)
			}
		}
		rows, err := db.SealedReportsOverlapping(ctx, jul, jul.AddDate(0, 3, 0))
		if err != nil || len(rows) != 1 {
			t.Errorf("round %d: %d sealed rows over the span (%v), want exactly 1", round, len(rows), err)
		}
		if wins != 1 {
			t.Errorf("round %d: %d sealers reported won, want exactly the one whose row is sealed", round, wins)
		}
	}
	// A floor, so sealers that all lost the lock cannot pass vacuously.
	if floor := rounds * 2; completed < floor {
		t.Errorf("%d of %d seal attempts completed, want at least %d", completed, rounds*sealers, floor)
	}
}

// TestInstallSecret_StableAcrossOpens: Open creates the secret once and every
// later Open keeps it; a different database gets a different one.
func TestInstallSecret_StableAcrossOpens(t *testing.T) {
	ctx := context.Background()
	read := func(path string) []byte {
		t.Helper()
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		s, err := installSecret(ctx, db.db)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	dir := t.TempDir()
	first := read(filepath.Join(dir, "a.db"))
	if len(first) != installSecretLen || bytes.Equal(first, make([]byte, installSecretLen)) {
		t.Fatalf("install secret = %x, want %d random bytes", first, installSecretLen)
	}
	if again := read(filepath.Join(dir, "a.db")); !bytes.Equal(again, first) {
		t.Errorf("second Open: secret %x, want %x unchanged", again, first)
	}
	if other := read(filepath.Join(dir, "b.db")); bytes.Equal(other, first) {
		t.Errorf("another database got the same secret %x", other)
	}
}

// makePreSealDB builds a database in the shape before #913: the current schema
// minus every #913 table (their triggers go with them).
func makePreSealDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "preseal.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"sealed_gap", "sealed_person", "sealed_rollup", "sealed_report", "install_secret", "seal_floor"} {
		if _, err := db.db.Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatal(err)
		}
	}
	var left int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%seal%' OR name LIKE '%install_secret%'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("control: %d #913 schema objects remain (%v), so the fixture is not pre-#913 shaped", left, err)
	}
	_ = db.Close()
	return path
}

// TestOpen_PreSealDatabaseGainsSealedTables: an existing database without the
// #913 tables gains them, their triggers and an install secret, and seals.
func TestOpen_PreSealDatabaseGainsSealedTables(t *testing.T) {
	path := makePreSealDB(t)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, trg := range []string{
		"trg_sealed_report_no_update", "trg_sealed_rollup_no_update", "trg_sealed_person_tombstone_only",
		"trg_install_secret_no_update", "trg_install_secret_no_delete", "trg_install_secret_first_wins",
		"trg_sealed_report_first_wins", "trg_sealed_rollup_no_replace", "trg_sealed_person_no_replace",
		"trg_sealed_rollup_no_lone_delete", "trg_sealed_person_no_lone_delete", "trg_sealed_report_delete_children",
		"trg_seal_floor_no_update", "trg_seal_floor_no_delete", "trg_seal_floor_first_wins",
	} {
		if n := triggerCount(t, db, trg); n != 1 {
			t.Errorf("trigger %s count = %d, want 1", trg, n)
		}
	}
	if s, err := installSecret(context.Background(), db.db); err != nil || len(s) != installSecretLen {
		t.Errorf("installSecret after migration = %x, %v", s, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM seal_floor`, 0)
	sealOne(t, db)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_person`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM seal_floor`, 1)
}

// TestOpen_SealedRollupGainsFoldColumnsOnlyWhenEmpty pins #913-D3's migration: a
// sealed_rollup that predates the fold-input columns gains them when it is
// empty, and a non-empty one refuses to open, because its rows' fold inputs can
// never be recovered and a DEFAULT would invent them.
func TestOpen_SealedRollupGainsFoldColumnsOnlyWhenEmpty(t *testing.T) {
	for _, rows := range []int{0, 1} {
		path := filepath.Join(t.TempDir(), "old-rollup.db")
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`DROP TABLE sealed_rollup;
			CREATE TABLE sealed_rollup (report_id INTEGER NOT NULL, label TEXT NOT NULL,
				weighted_points REAL NOT NULL, total_cost_usd REAL NOT NULL, actual_paid_usd REAL NOT NULL,
				realtime_usd REAL NOT NULL, sample_n INTEGER NOT NULL, flagged_outcomes INTEGER NOT NULL,
				PRIMARY KEY (report_id, label))`); err != nil {
			t.Fatal(err)
		}
		if rows == 1 {
			if _, err := db.db.Exec(`INSERT INTO sealed_rollup VALUES (1, 'core', 1, 1, 1, 1, 1, 0)`); err != nil {
				t.Fatal(err)
			}
		}
		_ = db.Close()
		db, err = Open(path)
		if rows == 1 {
			if err == nil || !strings.Contains(err.Error(), "NOT NULL") {
				t.Errorf("Open over a non-empty sealed_rollup without fold columns: %v, want the NOT NULL refusal", err)
			}
			if db != nil {
				_ = db.Close()
			}
			continue
		}
		if err != nil {
			t.Fatalf("Open over an empty sealed_rollup: %v", err)
		}
		for _, col := range sealedRollupFoldCols {
			if ok, err := columnExists(db.db, "sealed_rollup", col); err != nil || !ok {
				t.Errorf("column %s after migration: %v %v", col, ok, err)
			}
		}
		sealOne(t, db)
		_ = db.Close()
	}
}

// TestSealReport_FoldInputsRoundTripAndRefoldGate: Has and Contributes are
// stored per label and read back inside the transaction; a Refold error rolls
// the whole seal back.
func TestSealReport_FoldInputsRoundTripAndRefoldGate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	want := []SealedRollup{
		{Label: "a", TotalCostUSD: 2, SampleN: 1, Has: [len(SealedMeasures)]bool{true, false, false, false, true}},
		{Label: "b", ActualPaidUSD: 0, Contributes: true, Has: [len(SealedMeasures)]bool{false, true, true, true, false}},
	}
	persons := []SealedPerson{{Label: "a", Measure: "people", CanonicalID: "alice"}}
	refused := errors.New("refold differs")
	july := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july")
	if _, _, err := db.SealReport(ctx, july, want, persons, SealCheck{
		Floor:  testSealFloor,
		Refold: func([]SealedRollup, []SealedPersonKey) error { return refused },
	}); !errors.Is(err, refused) {
		t.Fatalf("SealReport with a failing refold: %v, want it returned", err)
	}
	for _, tbl := range []string{"sealed_report", "sealed_rollup", "sealed_person"} {
		assertCount(t, db, `SELECT COUNT(*) FROM `+tbl, 0)
	}
	var got []SealedRollup
	var keys []SealedPersonKey
	if _, _, err := db.SealReport(ctx, july, want, persons, SealCheck{Floor: testSealFloor, Refold: func(r []SealedRollup, k []SealedPersonKey) error {
		got, keys = r, k
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if len(keys) != 1 || keys[0].Label != "a" || keys[0].Measure != "people" || len(keys[0].Key) != 32 {
		t.Errorf("read back person keys %+v, want alice's one 32-byte key under a/people", keys)
	}
}

// TestSealReport_EraseEpochMovedRefused: every erase bumps the epoch, even one
// that finds nothing, and a seal computed before it is refused with
// ErrSealEraseRaced and writes nothing; the current epoch seals.
func TestSealReport_EraseEpochMovedRefused(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	before, err := db.EraseEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EraseDeveloper(ctx, "nobody"); err != nil {
		t.Fatal(err)
	}
	after, err := db.EraseEpoch(ctx)
	if err != nil || after != before+1 {
		t.Fatalf("erase epoch %d -> %d (%v), want one bump", before, after, err)
	}
	july := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july")
	persons := []SealedPerson{{Label: "core", Measure: "cost", CanonicalID: "alice"}}
	if _, _, err := db.SealReport(ctx, july, nil, persons, SealCheck{EraseEpoch: before, Floor: testSealFloor}); !errors.Is(err, ErrSealEraseRaced) {
		t.Fatalf("seal at a stale epoch: %v, want ErrSealEraseRaced", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_person`, 0)
	if _, _, err := db.SealReport(ctx, july, nil, persons, SealCheck{EraseEpoch: after, Floor: testSealFloor}); err != nil {
		t.Fatalf("control: seal at the current epoch: %v", err)
	}
}

// TestSealReport_FloorPinnedAtFirstSeal (#913-D2 item 4): the first seal pins
// its Floor in the same transaction; a later seal cannot move it, a period before
// it is refused and rolled back, a zero Floor is refused, and seal_floor refuses
// UPDATE and DELETE.
func TestSealReport_FloorPinnedAtFirstSeal(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, ok, err := db.SealFloor(ctx); ok || err != nil {
		t.Fatalf("floor before any seal: %v %v, want none", ok, err)
	}
	if _, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july"), nil, nil, SealCheck{}); !errors.Is(err, ErrSealBeforeFloor) {
		t.Fatalf("zero Floor: %v, want ErrSealBeforeFloor", err)
	}
	// The first seal must be its Floor: neither before it nor after it.
	if _, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(5), sealMonth(6), "cfg-a", "may"), nil, nil, SealCheck{Floor: sealMonth(6)}); !errors.Is(err, ErrSealNotNext) {
		t.Fatalf("period before its own Floor: %v, want ErrSealNotNext", err)
	}
	if _, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july"), nil, nil, SealCheck{Floor: sealMonth(4)}); !errors.Is(err, ErrSealNotNext) {
		t.Fatalf("period after its own Floor: %v, want ErrSealNotNext", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM seal_floor`, 0)
	if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(4), sealMonth(5), "cfg-a", "april"), nil, nil, SealCheck{Floor: sealMonth(4), First: true}); err != nil || !won {
		t.Fatalf("April at its Floor, as the first seal: won %v, %v; want sealed", won, err)
	}
	// Once armed, a seal that must be the first is refused, even of the next
	// period or of the identical sealed one.
	for _, r := range []SealedReport{sealFixture("month", sealMonth(5), sealMonth(6), "cfg-a", "may"), sealFixture("month", sealMonth(4), sealMonth(5), "cfg-a", "april")} {
		if _, won, err := db.SealReport(ctx, r, nil, nil, SealCheck{Floor: sealMonth(4), First: true}); !errors.Is(err, ErrSealNotNext) || won {
			t.Fatalf("first seal of %s once armed: won %v, %v; want ErrSealNotNext", r.Body, won, err)
		}
	}
	// A later seal passes an earlier Floor, as back-dated cost would produce.
	if _, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(3), sealMonth(4), "cfg-a", "march"), nil, nil, SealCheck{Floor: sealMonth(1)}); !errors.Is(err, ErrSealNotNext) {
		t.Fatalf("March after April was pinned: %v, want ErrSealNotNext", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 1)
	if floor, ok, err := db.SealFloor(ctx); !ok || err != nil || !floor.Equal(sealMonth(4)) {
		t.Errorf("floor %s %v %v, want the first seal's 2026-04", floor, ok, err)
	}
	const probe = `SELECT group_concat(period_start) FROM seal_floor`
	assertRefused(t, db, `UPDATE seal_floor SET period_start = '2026-01-01T00:00:00Z'`, probe, "2026-04-01T00:00:00Z")
	assertRefused(t, db, `DELETE FROM seal_floor`, probe, "2026-04-01T00:00:00Z")
	assertIgnored(t, db, `INSERT INTO seal_floor (id, period_start) VALUES (1, '2026-01-01T00:00:00Z')`, probe, "2026-04-01T00:00:00Z")
	assertIgnored(t, db, `INSERT OR REPLACE INTO seal_floor (id, period_start) VALUES (1, '2026-01-01T00:00:00Z')`, probe, "2026-04-01T00:00:00Z")
}

// TestSealReport_OnlyTheNextPeriodSeals (#913-D5 ruling C′): after the first
// seal a new period seals only where the newest sealed one ends, so a process
// that planned a later month from its own floor is refused rather than leaving
// a gap; won is true only for the insert that sealed the period.
func TestSealReport_OnlyTheNextPeriodSeals(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	march := sealFixture("month", sealMonth(3), sealMonth(4), "cfg-a", "march")
	if _, won, err := db.SealReport(ctx, march, nil, nil, SealCheck{Floor: sealMonth(3)}); err != nil || !won {
		t.Fatalf("the arming seal of March: won %v, %v", won, err)
	}
	// A serve pass that computed its floor as May before March was pinned.
	if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(5), sealMonth(6), "cfg-a", "may"), nil, nil, SealCheck{Floor: sealMonth(5)}); !errors.Is(err, ErrSealNotNext) || won {
		t.Fatalf("May with April unsealed: won %v, %v; want ErrSealNotNext", won, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report WHERE period_start = '2026-05-01T00:00:00Z'`, 0)
	// Losing the race for a sealed period returns the winner's row, not won.
	got, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(3), sealMonth(4), "cfg-a", "recomputed"), nil, nil, SealCheck{Floor: sealMonth(3)})
	if err != nil || won || string(got.Body) != "march" {
		t.Fatalf("March again: body %q won %v, %v; want March's stored body, not won", got.Body, won, err)
	}
	for _, m := range []int{4, 5} {
		if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(m), sealMonth(m+1), "cfg-a", "next"), nil, nil, SealCheck{Floor: sealMonth(1)}); err != nil || !won {
			t.Fatalf("2026-%02d after its predecessor: won %v, %v", m, won, err)
		}
	}
	if floor, ok, err := db.SealFloor(ctx); !ok || err != nil || !floor.Equal(sealMonth(3)) {
		t.Errorf("floor %s %v %v, want 2026-03", floor, ok, err)
	}
}
