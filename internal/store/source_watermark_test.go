package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func wmDay(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

// watermarkOf is source's row, failing the test when it has none.
func watermarkOf(t *testing.T, db *DB, source string) SourceWatermark {
	t.Helper()
	rows, err := db.SourceWatermarks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range rows {
		if w.Source == source {
			return w
		}
	}
	t.Fatalf("no source_watermark row for %q in %+v", source, rows)
	return SourceWatermark{}
}

// TestSourceWatermark_AdvancesOnlyOverGapFreeCoverage (#913-D9 ruling C′
// condition 2): a registered source has no run until its first pass; a pass
// whose window starts at or before settled_through extends the run and never
// moves it back; a pass whose window starts after it (the key revoked on 25 June,
// fixed on 3 August) starts a new run there and records where the old one
// ended; an unregistered source is refused.
func TestSourceWatermark_AdvancesOnlyOverGapFreeCoverage(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := db.RegisterSources(ctx, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if w := watermarkOf(t, db, "admin"); w.Settled || w.Retired || !w.CoveredFrom.IsZero() {
		t.Fatalf("registered row: %+v, want no run and not retired", w)
	}
	steps := []struct {
		name                  string
		from, through         time.Time
		covered, settled, gap time.Time
	}{
		{"first pass", wmDay(time.April, 1), wmDay(time.May, 20), wmDay(time.April, 1), wmDay(time.May, 20), time.Time{}},
		{"contiguous", wmDay(time.May, 1), wmDay(time.June, 24), wmDay(time.April, 1), wmDay(time.June, 24), time.Time{}},
		{"window starting on the watermark", wmDay(time.June, 24), wmDay(time.June, 25), wmDay(time.April, 1), wmDay(time.June, 25), time.Time{}},
		{"never backwards", wmDay(time.May, 1), wmDay(time.June, 1), wmDay(time.April, 1), wmDay(time.June, 25), time.Time{}},
		{"gap", wmDay(time.July, 1), wmDay(time.August, 2), wmDay(time.July, 1), wmDay(time.August, 2), wmDay(time.June, 25)},
		{"after the gap", wmDay(time.July, 1), wmDay(time.August, 3), wmDay(time.July, 1), wmDay(time.August, 3), wmDay(time.June, 25)},
	}
	for _, s := range steps {
		if err := db.AdvanceSourceWatermark(ctx, "admin", s.from, s.through); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		w := watermarkOf(t, db, "admin")
		if !w.Settled || !w.CoveredFrom.Equal(s.covered) || !w.SettledThrough.Equal(s.settled) || !w.GapFrom.Equal(s.gap) {
			t.Errorf("%s: %+v, want run %s..%s, gap from %s", s.name, w, s.covered, s.settled, s.gap)
		}
	}
	if w := watermarkOf(t, db, "admin"); len(w.Earlier) != 1 || w.Earlier[0] != (SourceRun{wmDay(time.April, 1), wmDay(time.June, 25)}) ||
		!w.Covers(wmDay(time.May, 1), wmDay(time.June, 1)) || w.Covers(wmDay(time.June, 1), wmDay(time.July, 1)) {
		t.Errorf("after the gap: %+v, want the run before it kept, covering May and not June", w)
	}
	if err := db.AdvanceSourceWatermark(ctx, "openai", wmDay(time.July, 1), wmDay(time.July, 2)); !errors.Is(err, ErrSourceNotRegistered) {
		t.Errorf("unregistered source: %v, want ErrSourceNotRegistered", err)
	}
	if err := db.AdvanceSourceWatermark(ctx, "admin", wmDay(time.July, 2), wmDay(time.July, 1)); err == nil {
		t.Error("a window ending before it starts was recorded")
	}
}

// TestSourceWatermark_RegisterRetiresTheRest (#913-D9 condition 3): registering
// a set adds its rows and retires every other one; re-registering a retired
// source unretires it with its run kept, so its next pass is judged against it.
func TestSourceWatermark_RegisterRetiresTheRest(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := db.RegisterSources(ctx, []string{"admin", "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AdvanceSourceWatermark(ctx, "admin", wmDay(time.May, 1), wmDay(time.June, 2)); err != nil {
		t.Fatal(err)
	}
	if retired, err := db.RegisterSources(ctx, []string{"codex", "muse"}); err != nil || !slices.Equal(retired, []string{"admin"}) {
		t.Fatalf("retired %v, %v; want admin alone", retired, err)
	}
	got := map[string]bool{}
	rows, err := db.SourceWatermarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range rows {
		got[w.Source] = w.Retired
	}
	if want := map[string]bool{"admin": true, "codex": false, "muse": false}; len(got) != 3 || got["admin"] != want["admin"] ||
		got["codex"] != want["codex"] || got["muse"] != want["muse"] {
		t.Errorf("rows %v, want admin retired, codex and muse registered", got)
	}
	if _, err := db.RegisterSources(ctx, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if w := watermarkOf(t, db, "admin"); w.Retired || !w.SettledThrough.Equal(wmDay(time.June, 2)) {
		t.Errorf("re-registered admin: %+v, want unretired with its run kept", w)
	}
	if w := watermarkOf(t, db, "codex"); !w.Retired {
		t.Errorf("codex after a registration without it: %+v, want retired", w)
	}
}

// TestOpen_V5ToV6AddsSourceWatermark: a version-5 database, which has no
// source_watermark, source_run, source_loss or source_registration table, gains them, is stamped 6 with
// UpgradeNoticeSourceWatermark alone, records a source, and a later open prints
// nothing and keeps the row.
func TestOpen_V5ToV6AddsSourceWatermark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP TABLE source_watermark; DROP TABLE source_run; DROP TABLE source_loss; DROP TABLE source_registration`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	setUserVersion(t, path, 5)

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := db.UpgradeNotice(); n != UpgradeNoticeSourceWatermark || !strings.Contains(n, "schema version 6") {
		t.Errorf("v5 open: UpgradeNotice = %q, want the source-watermark notice alone", n)
	}
	ctx := context.Background()
	if _, err := db.RegisterSources(ctx, []string{"admin"}); err != nil {
		t.Fatalf("register on the upgraded database: %v", err)
	}
	_ = db.Close()
	if v := readUserVersion(t, path); v != 6 {
		t.Errorf("user_version after the upgrade = %d, want 6 so a version-5 binary refuses it", v)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("second open: UpgradeNotice = %q, want empty (printed once)", n)
	}
	if w := watermarkOf(t, db, "admin"); w.Retired {
		t.Errorf("row after reopening: %+v, want registered", w)
	}
}

// TestSourceWatermark_BackfillLowersTheRunStart (#913-D9, the subscription
// case): a pass that overlaps the run and reaches back before its start lowers
// covered_from; one that ends before the run starts, leaving a hole, does not.
func TestSourceWatermark_BackfillLowersTheRunStart(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := db.RegisterSources(ctx, []string{"subs"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		name          string
		from, through time.Time
		covered       time.Time
	}{
		{"first pass", wmDay(time.October, 1), wmDay(time.November, 1), wmDay(time.October, 1)},
		{"disjoint earlier pass", wmDay(time.January, 1), wmDay(time.February, 1), wmDay(time.October, 1)},
		{"backfill from active_since", wmDay(time.January, 1), wmDay(time.November, 1), wmDay(time.January, 1)},
	} {
		if err := db.AdvanceSourceWatermark(ctx, "subs", s.from, s.through); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if w := watermarkOf(t, db, "subs"); !w.CoveredFrom.Equal(s.covered) || !w.SettledThrough.Equal(wmDay(time.November, 1)) || !w.GapFrom.IsZero() {
			t.Errorf("%s: %+v, want run %s..%s and no gap", s.name, w, s.covered, wmDay(time.November, 1))
		}
	}
}

// TestSourceWatermarks_NotRegisteredUntilServeRegisters (#913-D9 condition 3):
// before any registration the rows cannot be read as "not configured"; a
// registration of no source at all records that one happened.
func TestSourceWatermarks_NotRegisteredUntilServeRegisters(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := db.SourceWatermarks(ctx); !errors.Is(err, ErrSourcesNotRegistered) {
		t.Fatalf("fresh database: %v, want ErrSourcesNotRegistered", err)
	}
	if _, err := db.RegisterSources(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.SourceWatermarks(ctx); err != nil || len(rows) != 0 {
		t.Errorf("after registering none: %+v %v, want no rows and no error", rows, err)
	}
}

// TestRecordSourceLoss_GatesOnlyTheMonthsItTouches (#913-D9 ruling R-8): a
// lost span keeps every month it touches uncertified however the run covers
// it, widens when recorded again from the same start, survives a reopen, and
// is refused for a source with no row.
func TestRecordSourceLoss_GatesOnlyTheMonthsItTouches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.RegisterSources(ctx, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AdvanceSourceWatermark(ctx, "codex", wmDay(time.January, 1), wmDay(time.November, 1)); err != nil {
		t.Fatal(err)
	}
	for _, through := range []time.Time{wmDay(time.August, 2), wmDay(time.July, 12)} {
		if err := db.RecordSourceLoss(ctx, "codex", wmDay(time.July, 10), through); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordSourceLoss(ctx, "muse", wmDay(time.July, 10), wmDay(time.July, 10)); !errors.Is(err, ErrSourceNotRegistered) {
		t.Errorf("unregistered source: %v, want ErrSourceNotRegistered", err)
	}
	_ = db.Close()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	w := watermarkOf(t, db, "codex")
	for m, want := range map[time.Month]bool{time.June: true, time.July: false, time.August: false, time.September: true} {
		if got := w.Covers(wmDay(m, 1), wmDay(m+1, 1)); got != want {
			t.Errorf("after the reopen, %s certified = %v, want %v (lost %+v)", m, got, want, w.Lost)
		}
	}
}
