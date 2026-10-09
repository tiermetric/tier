package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gapFixture is a month gap starting 2026-<m>-01.
func gapFixture(m int, category, reason string) SealedGap {
	return SealedGap{PeriodSize: "month", PeriodStart: sealMonth(m), PeriodEnd: sealMonth(m + 1),
		Category: category, Reason: reason, ToolVersion: "v0.0.0-test", ToolCommit: "abc123"}
}

// TestSealedGap_AppendOnly: a recorded gap is never rewritten or removed —
// UPDATE, DELETE, a second INSERT of its period and INSERT OR REPLACE (by
// period and by rowid) are all refused, and the row keeps its reason.
func TestSealedGap_AppendOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	sealOne(t, db)
	if _, err := db.RecordSealedGap(context.Background(), gapFixture(8, "refold_mismatch", "rows lost in the May outage")); err != nil {
		t.Fatalf("RecordSealedGap: %v", err)
	}
	const probe = `SELECT group_concat(period_start || ' ' || reason) FROM sealed_gap`
	const want = "2026-08-01T00:00:00Z rows lost in the May outage"
	for _, stmt := range []string{
		`UPDATE sealed_gap SET reason = 'rewritten'`,
		`DELETE FROM sealed_gap`,
		`INSERT INTO sealed_gap (period_size, period_start, period_end, category, reason, created_at, tool_version, tool_commit)
		 VALUES ('month', '2026-08-01T00:00:00Z', '2026-09-01T00:00:00Z', 'overlap', 'again', '2026-09-30T00:00:00Z', 'v', 'c')`,
		`INSERT OR REPLACE INTO sealed_gap (period_size, period_start, period_end, category, reason, created_at, tool_version, tool_commit)
		 VALUES ('month', '2026-08-01T00:00:00Z', '2026-09-01T00:00:00Z', 'overlap', 'replaced', '2026-09-30T00:00:00Z', 'v', 'c')`,
		`INSERT OR REPLACE INTO sealed_gap (id, period_size, period_start, period_end, category, reason, created_at, tool_version, tool_commit)
		 VALUES (1, 'month', '2026-12-01T00:00:00Z', '2027-01-01T00:00:00Z', 'overlap', 'replaced', '2026-09-30T00:00:00Z', 'v', 'c')`,
	} {
		assertRefused(t, db, stmt, probe, want)
	}
}

// TestSealReport_NeverOverAGap: once August is a gap, SealReport refuses
// August and any span covering it with ErrSealedPeriodGapped and writes
// nothing; a raw INSERT is refused by the trigger, and a raw gap over a sealed
// month likewise.
func TestSealReport_NeverOverAGap(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	sealOne(t, db)
	if _, err := db.RecordSealedGap(ctx, gapFixture(8, "overlap", "sealed under an old config")); err != nil {
		t.Fatal(err)
	}
	// July ends where the gap starts: re-sealing it is the race loser's path,
	// which finds July sealed rather than overlapping the gap.
	if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july"), nil, nil,
		SealCheck{Floor: testSealFloor}); err != nil || won {
		t.Errorf("re-seal July beside the August gap: won %v, %v; want the stored July", won, err)
	}
	for _, r := range []SealedReport{
		sealFixture("month", sealMonth(8), sealMonth(9), "cfg-a", "august"),
		sealFixture("quarter", sealMonth(8), sealMonth(11), "cfg-a", "q3"),
	} {
		if _, won, err := db.SealReport(ctx, r, nil, nil, SealCheck{Floor: testSealFloor}); !errors.Is(err, ErrSealedPeriodGapped) || won {
			t.Errorf("seal %s %s over the gap: won %v, %v; want ErrSealedPeriodGapped", r.PeriodSize, r.Body, won, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report WHERE period_start >= '2026-08-01T00:00:00Z'`, 0)
	for _, stmt := range []string{
		`INSERT INTO sealed_report (level, period_size, period_start, period_end, k, config_digest, sealed_at, body, body_digest, tool_version, tool_commit)
		 VALUES ('team', 'month', '2026-08-01T00:00:00Z', '2026-09-01T00:00:00Z', 5, 'cfg-a', '2026-09-30T00:00:00Z', 'x', 'sha256:x', 'v', 'c')`,
		`INSERT INTO sealed_gap (period_size, period_start, period_end, category, reason, created_at, tool_version, tool_commit)
		 VALUES ('month', '2026-07-01T00:00:00Z', '2026-08-01T00:00:00Z', 'overlap', 'over a seal', '2026-09-30T00:00:00Z', 'v', 'c')`,
	} {
		if _, err := db.db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "(#913)") {
			t.Errorf("%s: %v, want the trigger's refusal", stmt, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_gap`, 1)
}

// TestSealReport_NextMonthAfterAGap: with July sealed, September is not next
// until August is recorded as a gap; then September seals and October follows.
func TestSealReport_NextMonthAfterAGap(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	sealOne(t, db)
	sept := sealFixture("month", sealMonth(9), sealMonth(10), "cfg-a", "september")
	if _, won, err := db.SealReport(ctx, sept, nil, nil, SealCheck{Floor: testSealFloor}); !errors.Is(err, ErrSealNotNext) || won {
		t.Fatalf("control: September with August owed: won %v, %v; want ErrSealNotNext", won, err)
	}
	if _, err := db.RecordSealedGap(ctx, gapFixture(8, "refold_mismatch", "cannot be sealed")); err != nil {
		t.Fatal(err)
	}
	// The gap is newer than every seal, so this pins that LatestSealedPeriod
	// never returns a gap.
	if start, ok, err := db.LatestSealedPeriod(ctx, "month"); err != nil || !ok || !start.Equal(sealMonth(7)) {
		t.Errorf("LatestSealedPeriod after the August gap = %s %v %v, want 2026-07-01", start, ok, err)
	}
	if _, won, err := db.SealReport(ctx, sept, nil, nil, SealCheck{Floor: testSealFloor}); err != nil || !won {
		t.Fatalf("September after the August gap: won %v, %v; want sealed", won, err)
	}
	if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(10), sealMonth(11), "cfg-a", "october"), nil, nil, SealCheck{Floor: testSealFloor}); err != nil || !won {
		t.Fatalf("October after September: won %v, %v; want sealed", won, err)
	}
	if start, ok, err := db.LatestSealedGap(ctx, "month"); err != nil || !ok || !start.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("LatestSealedGap = %s %v %v, want 2026-08-01", start, ok, err)
	}
	if start, ok, err := db.LatestSealedPeriod(ctx, "month"); err != nil || !ok || !start.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("LatestSealedPeriod = %s %v %v, want 2026-10-01", start, ok, err)
	}
}

// TestRecordSealedGap_Refusals: a gap is recorded only for one UTC calendar
// month that has ended and follows the newest sealed or gapped one, never while
// nothing is sealed, never over a sealed month or another gap, only with a
// SealGap* category and a visible, printable reason of at most 200 characters;
// each refusal writes nothing. The recorded row reads back whole.
func TestRecordSealedGap_Refusals(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	db.now = func() time.Time { return time.Date(2026, 11, 15, 8, 15, 0, 0, time.UTC) }
	span := func(size string, start, end time.Time) SealedGap {
		g := gapFixture(8, "refold_mismatch", "why")
		g.PeriodSize, g.PeriodStart, g.PeriodEnd = size, start, end
		return g
	}
	aug := sealMonth(8)
	if _, err := db.RecordSealedGap(ctx, gapFixture(7, "refold_mismatch", "why")); !errors.Is(err, ErrSealNotNext) {
		t.Errorf("gap with nothing sealed: %v, want ErrSealNotNext", err)
	}
	sealOne(t, db)
	for _, c := range []struct {
		name string
		g    SealedGap
		want error
	}{
		{"sealed month", gapFixture(7, "refold_mismatch", "why"), ErrSealedPeriodSealed},
		{"not next", gapFixture(9, "refold_mismatch", "why"), ErrSealNotNext},
		{"empty reason", gapFixture(8, "refold_mismatch", ""), ErrSealedGapReason},
		{"spaces-only reason", gapFixture(8, "refold_mismatch", "   "), ErrSealedGapReason},
		{"tab", gapFixture(8, "refold_mismatch", " \t "), ErrSealedGapReason},
		{"line separator", gapFixture(8, "refold_mismatch", "a\u2028b"), ErrSealedGapReason},
		{"paragraph separator", gapFixture(8, "refold_mismatch", "a\u2029b"), ErrSealedGapReason},
		{"private use", gapFixture(8, "refold_mismatch", "a\uE000b"), ErrSealedGapReason},
		{"unassigned", gapFixture(8, "refold_mismatch", "a\u0378b"), ErrSealedGapReason},
		{"noncharacter", gapFixture(8, "refold_mismatch", "a\uFFFFb"), ErrSealedGapReason},
		{"fillers only", gapFixture(8, "refold_mismatch", " \u3164\u115F \u1160\uFFA0\u2800 "), ErrSealedGapReason},
		{"combining marks only", gapFixture(8, "refold_mismatch", "\u0301\u034F\u0301"), ErrSealedGapReason},
		{"unknown category", gapFixture(8, "refold_mismatch: team \"payments\"", "why"), ErrSealedGapCategory},
		{"empty category", gapFixture(8, "", "why"), ErrSealedGapCategory},
		{"end in 2100", span("month", aug, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)), ErrSealedPeriodInvalid},
		{"two months", span("month", aug, sealMonth(10)), ErrSealedPeriodInvalid},
		{"14-day span", span("month", aug, aug.AddDate(0, 0, 14)), ErrSealedPeriodInvalid},
		{"quarter size", span("quarter", aug, sealMonth(9)), ErrSealedPeriodInvalid},
		{"mid-month start", span("month", aug.AddDate(0, 0, 1), sealMonth(9).AddDate(0, 0, 1)), ErrSealedPeriodInvalid},
		{"noon start", span("month", aug.Add(12*time.Hour), sealMonth(9).Add(12*time.Hour)), ErrSealedPeriodInvalid},
		{"future month", gapFixture(11, "refold_mismatch", "why"), ErrSealedPeriodInvalid},
		{"201 characters", gapFixture(8, "refold_mismatch", strings.Repeat("é", 201)), ErrSealedGapReason},
		{"line break", gapFixture(8, "refold_mismatch", "two\nlines"), ErrSealedGapReason},
		{"escape", gapFixture(8, "refold_mismatch", "red \x1b[31m"), ErrSealedGapReason},
		{"bidi override", gapFixture(8, "refold_mismatch", "abc\u202edef"), ErrSealedGapReason},
		{"invalid UTF-8", gapFixture(8, "refold_mismatch", "bad \xff byte"), ErrSealedGapReason},
		{"inverted span", SealedGap{PeriodSize: "month", PeriodStart: sealMonth(9), PeriodEnd: sealMonth(8),
			Category: "overlap", Reason: "why"}, ErrSealedPeriodInvalid},
	} {
		if _, err := db.RecordSealedGap(ctx, c.g); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_gap`, 0)
	reason := strings.Repeat("é", 200)
	got, err := db.RecordSealedGap(ctx, gapFixture(8, "overlap", reason))
	if err != nil {
		t.Fatalf("200-character reason: %v", err)
	}
	read, err := db.SealedGap(ctx, "month", sealMonth(8))
	if err != nil || read != got || read.Category != "overlap" || read.Reason != reason ||
		!read.CreatedAt.Equal(time.Date(2026, 11, 15, 8, 15, 0, 0, time.UTC)) || !read.PeriodEnd.Equal(sealMonth(9)) {
		t.Errorf("SealedGap = %+v, %v; want the recorded row %+v created at 2026-11-15T08:15:00Z", read, err, got)
	}
	if _, err := db.RecordSealedGap(ctx, gapFixture(8, "overlap", "again")); !errors.Is(err, ErrSealedPeriodGapped) {
		t.Errorf("second gap on August: %v, want ErrSealedPeriodGapped", err)
	}
	if _, err := db.SealedGap(ctx, "month", sealMonth(9)); !errors.Is(err, ErrSealedGapNotFound) {
		t.Errorf("SealedGap(September) = %v, want ErrSealedGapNotFound", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_gap`, 1)
}

// TestRecordSealedGap_AfterAGap: with August and September both unsealable,
// August is recorded, then September, then October seals.
func TestRecordSealedGap_AfterAGap(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	db.now = func() time.Time { return time.Date(2026, 11, 15, 8, 15, 0, 0, time.UTC) }
	sealOne(t, db)
	for _, m := range []int{8, 9} {
		if _, err := db.RecordSealedGap(ctx, gapFixture(m, "refold_mismatch", "cannot be sealed")); err != nil {
			t.Fatalf("gap on month %d: %v", m, err)
		}
	}
	if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(10), sealMonth(11), "cfg-a", "october"), nil, nil,
		SealCheck{Floor: testSealFloor}); err != nil || !won {
		t.Fatalf("October after the August and September gaps: won %v, %v; want sealed", won, err)
	}
}

// TestSealClock_CountsGaps: a clock behind the newest gap, though not behind
// the newest seal, refuses both a gap and a seal with ErrSealClockBehind.
func TestSealClock_CountsGaps(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	at := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return at }
	sealOne(t, db)
	db.now = func() time.Time { return at.Add(time.Hour) }
	if _, err := db.RecordSealedGap(ctx, gapFixture(8, "refold_mismatch", "cannot be sealed")); err != nil {
		t.Fatal(err)
	}
	db.now = func() time.Time { return at.Add(30 * time.Minute) }
	if _, err := db.RecordSealedGap(ctx, gapFixture(9, "refold_mismatch", "cannot be sealed")); !errors.Is(err, ErrSealClockBehind) {
		t.Errorf("gap with the clock behind the newest gap: %v, want ErrSealClockBehind", err)
	}
	if _, won, err := db.SealReport(ctx, sealFixture("month", sealMonth(9), sealMonth(10), "cfg-a", "september"), nil, nil,
		SealCheck{Floor: testSealFloor}); !errors.Is(err, ErrSealClockBehind) || won {
		t.Errorf("seal with the clock behind the newest gap: won %v, %v; want ErrSealClockBehind", won, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_gap`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_report`, 1)
}

// TestOpen_V4ToV5AddsSealedGap: a version-4 database, which has no sealed_gap
// table or gap triggers, gains them, is stamped 6 with UpgradeNoticeSealedGap
// and the later notice, refuses a seal over a gap it then records, and a later open prints
// nothing.
func TestOpen_V4ToV5AddsSealedGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sealOne(t, db)
	if _, err := db.db.Exec(`DROP TABLE sealed_gap; DROP TRIGGER trg_sealed_report_not_over_gap`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%gap%'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("control: %d gap schema objects remain (%v)", left, err)
	}
	_ = db.Close()
	setUserVersion(t, path, 4)

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := db.UpgradeNotice(); n != UpgradeNoticeSealedGap+"\n"+UpgradeNoticeSourceWatermark || !strings.Contains(n, "schema version 5") {
		t.Errorf("v4 open: UpgradeNotice = %q, want the sealed-gap notice and the later one", n)
	}
	for _, trg := range []string{"trg_sealed_gap_no_update", "trg_sealed_gap_no_delete", "trg_sealed_gap_no_replace",
		"trg_sealed_gap_not_over_seal", "trg_sealed_report_not_over_gap"} {
		assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = '`+trg+`'`, 1)
	}
	ctx := context.Background()
	if _, err := db.RecordSealedGap(ctx, gapFixture(8, "overlap", "why")); err != nil {
		t.Fatalf("gap on the upgraded database: %v", err)
	}
	if _, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(8), sealMonth(9), "cfg-a", "august"), nil, nil, SealCheck{}); !errors.Is(err, ErrSealedPeriodGapped) {
		t.Errorf("seal over the gap on the upgraded database: %v, want ErrSealedPeriodGapped", err)
	}
	_ = db.Close()
	if v := readUserVersion(t, path); v <= 4 {
		t.Errorf("user_version after the upgrade = %d, want above 4 so a version-4 binary refuses it", v)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("second open: UpgradeNotice = %q, want empty (printed once)", n)
	}
}
