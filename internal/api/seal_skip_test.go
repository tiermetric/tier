package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// sealSep1 is 1 September 2026: with sealGrace, May through July are sealable.
var sealSep1 = time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)

// juneStuck is the sealed-read fixture plus six June developers of team big,
// with May sealed and June's seal failing for good (stored inputs that do not
// refold), at now; it returns a skip
// sealer at the same clock.
func juneStuck(t *testing.T, now time.Time) (*Handler, *store.DB, *sealer, *sealer) {
	t.Helper()
	h, db, s := newSealedReadHandler(t)
	for i := 1; i <= 6; i++ {
		seedKAnonDev(t, db, "big", fmt.Sprintf("j%d", i), 3+0.1*float64(i), 2, time.Date(2026, time.June, 10, 3, 0, 0, 0, time.UTC))
	}
	sealPass(t, s)
	s.now = func() time.Time { return now }
	h.store = skewedSealStore{db}
	sk := newTestSealer(h)
	sk.now = s.now
	return h, db, s, sk
}

func gapRows(t *testing.T, db *store.DB) int {
	t.Helper()
	return rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_gap`)
}

// TestSealSkip_GapRecordedAndSealingStepsOverIt (#913-D6 ruling D): June,
// whose seal fails for good, is recorded as a gap with its category and the
// operator's reason (a dry run first writes nothing); nothing seals over it;
// reading it is a 404 naming the category and reason, never the error; the
// next pass seals July after it; and the default read and the /compare default
// pair never land on it.
func TestSealSkip_GapRecordedAndSealingStepsOverIt(t *testing.T) {
	h, db, s, sk := juneStuck(t, sealAug1)
	ctx := context.Background()
	if err := s.sealDue(ctx); !errors.Is(err, errSealRefoldMismatch) {
		t.Fatalf("control: June's pass: %v, want errSealRefoldMismatch", err)
	}
	const reason = "June's inputs cannot refold (ticket OPS-12)"
	sk.dryRun = true
	if plan, err := sk.skip(ctx, "2026-06", reason, false, ""); err != nil || plan.Sealed || plan.Category != "" || plan.Month != "2026-06" {
		t.Fatalf("dry run: %+v, %v; want 2026-06 with no category (refold is found only by SealReport)", plan, err)
	}
	sk.dryRun = false
	if plan, err := sk.skip(ctx, "2026-06", reason, false, ""); err != nil || plan.Category != "refold_mismatch" || !errors.Is(plan.Observed, errSealRefoldMismatch) {
		t.Fatalf("retry without record: %+v, %v; want the refold_mismatch failure and its error", plan, err)
	}
	if n, g := sealedRows(t, db), gapRows(t, db); n != 1 || g != 0 {
		t.Fatalf("dry run and retry left %d sealed, %d gaps; want May alone and no gap", n, g)
	}
	got, err := sk.skip(ctx, "2026-06", reason, true, "refold_mismatch")
	if err != nil || got.Sealed || got.Category != "refold_mismatch" {
		t.Fatalf("skip: %+v, %v; want a refold_mismatch gap", got, err)
	}
	if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_gap WHERE period_start = '2026-06-01T00:00:00Z'
		AND period_end = '2026-07-01T00:00:00Z' AND category = 'refold_mismatch' AND reason = ?`, reason); n != 1 || sealedRows(t, db) != 1 {
		t.Fatalf("%d matching gap rows, %d sealed; want the June gap and May alone", n, sealedRows(t, db))
	}

	h.store = db
	if _, _, err := s.sealOrLoad(ctx, Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)}); !errors.Is(err, store.ErrSealedPeriodGapped) {
		t.Errorf("sealing June over its gap with a sound store: %v, want store.ErrSealedPeriodGapped", err)
	}
	rec := sealedGet(t, h, "/api/v1/scores?period=2026-06")
	if body := rec.Body.String(); rec.Code != http.StatusNotFound || !strings.Contains(body, "its stored inputs did not refold to its body") ||
		!strings.Contains(body, "OPS-12") || strings.Contains(body, errSealRefoldMismatch.Error()) || strings.Contains(body, "next seal pass") {
		t.Errorf("June's read: %d %s; want a 404 naming the category and reason, and no error text", rec.Code, body)
	}
	s.cfg.grace = 60 * 24 * time.Hour
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-06"); rec.Code != http.StatusNotFound || decodeJSON(t, rec)["sealable_at"] != nil {
		t.Errorf("June's read with a grace that puts it in its lag: %d %s; want a 404 with no sealable_at, since a gap never seals", rec.Code, rec.Body.String())
	}
	s.cfg.grace = sealGrace
	// Only May is sealed, so the default pair's later month steps over June.
	if rec := compareGet(t, h, ""); rec.Code != http.StatusNotFound || decodeJSON(t, rec)["period"] != "2026-07" {
		t.Errorf("compare default with May alone sealed: %d %s; want a 404 for 2026-07, past the gap", rec.Code, rec.Body.String())
	}

	s.now = func() time.Time { return sealSep1 }
	sealPass(t, s)
	if got := sealedOrder(t, db); got != "2026-05-01T00:00:00Z,2026-07-01T00:00:00Z" {
		t.Fatalf("pass after the gap sealed %s, want May then July", got)
	}
	if rec := sealedGet(t, h, "/api/v1/scores"); rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "2026-07" {
		t.Errorf("default read: %d period %q; want 200 for 2026-07", rec.Code, rec.Header().Get(headerSealedPeriod))
	}
	rec = compareGet(t, h, "")
	if a, b := rec.Header().Get(headerSealedPeriodA), rec.Header().Get(headerSealedPeriodB); rec.Code != http.StatusOK || a != "2026-05" || b != "2026-07" {
		t.Errorf("compare default: %d %s..%s %s; want 200 comparing 2026-05 with 2026-07", rec.Code, a, b, rec.Body.String())
	}
	if rec := compareGet(t, h, "?period_a=2026-06&period_b=2026-07"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "OPS-12") {
		t.Errorf("compare naming the gap: %d %s; want a 404 naming its reason", rec.Code, rec.Body.String())
	}

	// With July sealed after the June gap, a stuck August is first owed.
	for i := 1; i <= 6; i++ {
		seedKAnonDev(t, db, "big", fmt.Sprintf("a%d", i), 3+0.1*float64(i), 2, time.Date(2026, time.August, 10, 3, 0, 0, 0, time.UTC))
	}
	h.store = skewedSealStore{db}
	sk.now = func() time.Time { return time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC) }
	if got, err := sk.skip(ctx, "2026-08", "stuck too", true, "refold_mismatch"); err != nil || got.Category != "refold_mismatch" || gapRows(t, db) != 2 {
		t.Errorf("skip August after the June gap and a sealed July: %+v, %v, %d gaps; want a second gap", got, err, gapRows(t, db))
	}
}

// TestSealSkip_CleanRetrySealsAndDryRunWritesNothing: when June now seals, a
// dry run reports no category and writes nothing, and a real skip seals June
// and records no gap.
func TestSealSkip_CleanRetrySealsAndDryRunWritesNothing(t *testing.T) {
	h, db, _, sk := juneStuck(t, sealAug1)
	h.store = db
	ctx := context.Background()
	sk.dryRun = true
	if plan, err := sk.skip(ctx, "2026-06", "stuck", false, ""); err != nil || plan.Sealed || plan.Category != "" {
		t.Fatalf("dry run: %+v, %v; want no category and nothing sealed", plan, err)
	}
	raw := rawSealStore(t, db)
	if n := sealedRows(t, db) + gapRows(t, db) + rawCount(t, raw, `SELECT COUNT(*) FROM seal_floor`); n != 2 {
		t.Fatalf("dry run: %d sealed_report + sealed_gap + seal_floor rows, want May's row and floor alone (2)", n)
	}
	sk.dryRun = false
	if got, err := sk.skip(ctx, "2026-06", "stuck", false, ""); err != nil || !got.Sealed {
		t.Fatalf("skip of a month that now seals: %+v, %v; want sealed", got, err)
	}
	if got := sealedOrder(t, db); got != "2026-05-01T00:00:00Z,2026-06-01T00:00:00Z" || gapRows(t, db) != 0 {
		t.Errorf("sealed %s with %d gaps; want May then June and no gap", got, gapRows(t, db))
	}
}

// TestSealSkip_TransientFailureRecordsNoGap: a retry that meets a held write
// lock, or is cancelled, refuses with ErrSealSkipTransient and records nothing.
func TestSealSkip_TransientFailureRecordsNoGap(t *testing.T) {
	_, db, _, sk := juneStuck(t, sealAug1)
	ctx := context.Background()
	conn, err := rawSealStore(t, db).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sk.beforeSeal = func() {
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
	}
	_, err = sk.skip(ctx, "2026-06", "stuck", true, "refold_mismatch")
	if _, rerr := conn.ExecContext(ctx, `ROLLBACK`); rerr != nil {
		t.Fatal(rerr)
	}
	if !errors.Is(err, ErrSealSkipTransient) || !errors.Is(err, store.ErrWriteLockUnavailable) {
		t.Errorf("write lock held: %v, want ErrSealSkipTransient wrapping ErrWriteLockUnavailable", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	sk.beforeSeal = cancel
	if _, err := sk.skip(cctx, "2026-06", "stuck", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipTransient) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v, want ErrSealSkipTransient wrapping context.Canceled", err)
	}
	if n, g := sealedRows(t, db), gapRows(t, db); n != 1 || g != 0 {
		t.Errorf("%d sealed, %d gaps; want May alone and no gap", n, g)
	}
}

// TestSealSkip_Refusals: each precondition refuses with ErrSealSkipRefused,
// says why, and records nothing.
func TestSealSkip_Refusals(t *testing.T) {
	ctx := context.Background()
	h, db, _, sk := juneStuck(t, sealSep1)
	for _, c := range []struct{ month, reason, confirm, want string }{
		{"2026-6", "r", "", "YYYY-MM"},
		{"2026-06", "", "", "printable text"},
		{"2026-06", "two\nlines", "", "printable text"},
		{"2026-04", "r", "", "2026-04 precedes the earliest sealed month; only the first owed month, 2026-06"},
		{"2026-07", "r", "", "2026-07 is not the first owed month"},
		{"2026-08", "r", "", "2026-08 is open or inside its grace lag until"},
		{"2026-06", "r", "overlap", "now fails with refold_mismatch, not the confirmed overlap"},
		{"2026-06", "r", "", "fails with refold_mismatch, and no category was confirmed"},
	} {
		if _, err := sk.skip(ctx, c.month, c.reason, true, c.confirm); !errors.Is(err, ErrSealSkipRefused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("skip %s %q confirm %q: %v, want a refusal naming %q", c.month, c.reason, c.confirm, err, c.want)
		}
	}
	if g := gapRows(t, db); g != 0 {
		t.Fatalf("refusals recorded %d gaps", g)
	}
	if _, err := sk.skip(ctx, "2026-06", "r", true, "refold_mismatch"); err != nil {
		t.Fatalf("control: skip June confirming its category: %v", err)
	}
	for month, want := range map[string]string{"2026-05": "2026-05 is sealed already", "2026-06": "2026-06 is recorded as a gap already"} {
		if _, err := sk.skip(ctx, month, "r", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || !strings.Contains(err.Error(), want) {
			t.Errorf("skip %s: %v, want a refusal naming %q", month, err, want)
		}
	}
	if g := gapRows(t, db); g != 1 {
		t.Errorf("%d gaps, want June's alone", g)
	}

	h.SetAggregation(scoring.AggregationDeveloper, 5)
	if _, err := sk.skip(ctx, "2026-07", "r", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || !errors.Is(err, errSealDeveloperMode) {
		t.Errorf("developer mode: %v, want a refusal", err)
	}
	fresh, freshDB := newTestHandler(t)
	fresh.SetAggregation(scoring.AggregationTeam, 5)
	seedSealFixture(t, freshDB)
	unarmed, err := newSealer(fresh, sealConfig{grace: sealGrace})
	if err != nil {
		t.Fatal(err)
	}
	unarmed.now = func() time.Time { return sealSep1 }
	if _, err := unarmed.skip(ctx, "2026-05", "r", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || !errors.Is(err, errSealingNotArmed) {
		t.Errorf("not armed: %v, want a refusal", err)
	}
	armedNothingSealed := newTestSealer(fresh)
	armedNothingSealed.now = unarmed.now
	if _, err := armedNothingSealed.skip(ctx, "2026-05", "r", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || !strings.Contains(err.Error(), "no month is sealed yet") {
		t.Errorf("nothing sealed: %v, want a refusal", err)
	}
	// seal_from arms sealing for --skip as it does for serve.
	if _, err := fresh.SkipSealing(ctx, sealGrace, "2026-05", "2026-05", "r", false, true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || !strings.Contains(err.Error(), "no month is sealed yet") {
		t.Errorf("armed by seal_from, nothing sealed: %v, want the nothing-sealed refusal", err)
	}
}

// errProbeRead is a store read failure no sealFailures entry names.
var errProbeRead = errors.New("probe: disk read failed")

// readFailStore fails every seal's read snapshot.
type readFailStore struct{ skewedSealStore }

func (readFailStore) ReadSnapshot(context.Context, func(*store.Snapshot) error) error {
	return errProbeRead
}

// TestSealSkip_ComputationFailureDryRunWritesNothing: when computing the seal
// fails for good, a dry run (even asked to record) and a retry without record
// return its category and error and record nothing; a record run records it.
func TestSealSkip_ComputationFailureDryRunWritesNothing(t *testing.T) {
	h, db, _, sk := juneStuck(t, sealAug1)
	h.store = readFailStore{skewedSealStore{db}}
	ctx := context.Background()
	for _, dry := range []bool{true, false} {
		sk.dryRun = dry
		plan, err := sk.skip(ctx, "2026-06", "unreadable", dry, "internal")
		if err != nil || plan.Category != "internal" || !errors.Is(plan.Observed, errProbeRead) || gapRows(t, db) != 0 {
			t.Fatalf("dry run %v: %+v, %v, %d gaps; want the internal failure and no gap", dry, plan, err, gapRows(t, db))
		}
	}
	if got, err := sk.skip(ctx, "2026-06", "unreadable", true, "internal"); err != nil || got.Category != "internal" || gapRows(t, db) != 1 {
		t.Errorf("record: %+v, %v, %d gaps; want one internal gap", got, err, gapRows(t, db))
	}
}

// gapFailStore's RecordSealedGap fails with err, as a racing writer makes it.
type gapFailStore struct {
	skewedSealStore
	err error
}

func (s gapFailStore) RecordSealedGap(context.Context, store.SealedGap) (store.SealedGap, error) {
	return store.SealedGap{}, s.err
}

// TestSealSkip_RaceRefusals: a gap's write that loses to another seal or gap is
// refused (exit 1), a busy write lock is transient (exit 2), and a gap recorded
// by another process between the check and the retry is refused.
func TestSealSkip_RaceRefusals(t *testing.T) {
	h, db, _, sk := juneStuck(t, sealAug1)
	ctx := context.Background()
	for _, c := range []struct {
		err       error
		sentinel  error
		transient bool
	}{
		{store.ErrSealNotNext, ErrSealSkipRefused, false},
		{store.ErrSealedPeriodSealed, ErrSealSkipRefused, false},
		{store.ErrWriteLockUnavailable, ErrSealSkipTransient, true},
	} {
		h.store = gapFailStore{skewedSealStore{db}, c.err}
		_, err := sk.skip(ctx, "2026-06", "r", true, "refold_mismatch")
		if !errors.Is(err, c.sentinel) || !errors.Is(err, c.err) || strings.Contains(err.Error(), "meanwhile") == c.transient {
			t.Errorf("gap write failing with %v: %v, want %v", c.err, err, c.sentinel)
		}
	}
	h.store = skewedSealStore{db}
	sk.beforeSeal = func() {
		start, end := Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)}.Bounds()
		if _, err := db.RecordSealedGap(ctx, store.SealedGap{PeriodSize: "month", PeriodStart: start, PeriodEnd: end,
			Category: store.SealGapInternal, Reason: "the other process"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sk.skip(ctx, "2026-06", "r", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || !strings.Contains(err.Error(), "as a gap meanwhile") {
		t.Errorf("gap recorded during the retry: %v, want a refusal", err)
	}
	if g := gapRows(t, db); g != 1 {
		t.Errorf("%d gaps, want the other process's alone", g)
	}
}

// cancelOnGapStore's RecordSealedGap returns err (delegating to the store when
// err is nil) and then cancels, as a SIGINT landing after the write does.
type cancelOnGapStore struct {
	skewedSealStore
	cancel context.CancelFunc
	err    error
}

func (s cancelOnGapStore) RecordSealedGap(ctx context.Context, g store.SealedGap) (store.SealedGap, error) {
	defer s.cancel()
	if s.err != nil {
		return store.SealedGap{}, s.err
	}
	return s.skewedSealStore.RecordSealedGap(ctx, g)
}

// TestSealSkip_CancelAfterGapWrite: a cancellation that lands after the gap's
// write returns the recorded gap, not a transient error; and a write refused
// because another process moved on stays a refusal (exit 1) under it.
func TestSealSkip_CancelAfterGapWrite(t *testing.T) {
	h, db, _, sk := juneStuck(t, sealAug1)
	ctx, cancel := context.WithCancel(context.Background())
	h.store = cancelOnGapStore{skewedSealStore{db}, cancel, store.ErrSealNotNext}
	if _, err := sk.skip(ctx, "2026-06", "r", true, "refold_mismatch"); !errors.Is(err, ErrSealSkipRefused) || errors.Is(err, ErrSealSkipTransient) || ctx.Err() == nil {
		t.Errorf("refused write, then cancelled: %v (ctx %v); want a refusal", err, ctx.Err())
	}
	ctx, cancel = context.WithCancel(context.Background())
	h.store = cancelOnGapStore{skewedSealStore{db}, cancel, nil}
	plan, err := sk.skip(ctx, "2026-06", "r", true, "refold_mismatch")
	if err != nil || plan.Month != "2026-06" || plan.Category != "refold_mismatch" || ctx.Err() == nil || gapRows(t, db) != 1 {
		t.Errorf("gap written, then cancelled: %+v, %v (ctx %v), %d gaps; want the recorded plan", plan, err, ctx.Err(), gapRows(t, db))
	}
}

// TestSealFailureOf_StoredCodes pins the category codes a gap stores and which
// failures a skip refuses as transient.
func TestSealFailureOf_StoredCodes(t *testing.T) {
	for _, c := range []struct {
		err       error
		code      string
		transient bool
	}{
		{store.ErrWriteLockUnavailable, "write_lock_busy", true},
		{store.ErrSealEraseRaced, "erase_raced", true},
		{store.ErrSealClockBehind, "clock_behind", true},
		{errSealRefoldMismatch, "refold_mismatch", false},
		{errSealFloorMoved, "floor_moved", true},
		{store.ErrSealNotNext, "not_next", true},
		{store.ErrSealedPeriodOverlap, "overlap", false},
		{store.ErrSealedPeriodGapped, "gapped", true},
		{errPeriodNotSealable, "not_sealable", true},
		{context.Canceled, "cancelled", true},
		{context.DeadlineExceeded, "cancelled", true},
		{errors.New("disk on fire"), "internal", false},
		{sqliteErr(t, "BEGIN IMMEDIATE"), "storage", true},
		{sqliteErr(t, "INSERT INTO f VALUES (randomblob(1 << 20))"), "storage", true},
		{sqliteErr(t, "SELEC 1"), "internal", false},
	} {
		if f := sealFailureOf(fmt.Errorf("seal period 2026-06: %w", c.err)); f.code != c.code || f.transient != c.transient {
			t.Errorf("%v: code %q transient %v, want %q %v", c.err, f.code, f.transient, c.code, c.transient)
		}
	}
	if w := sealFailureWords("refold_mismatch"); w != "its stored inputs did not refold to its body" {
		t.Errorf("words for refold_mismatch = %q", w)
	}
	if w := sealFailureWords("from_a_newer_binary"); w != "from_a_newer_binary" {
		t.Errorf("words for an unknown code = %q, want the code itself", w)
	}
}

// TestSealStallWords: every stall_reason a read can serve has words, including
// the two that are never a gap's category, and an unknown code has none.
func TestSealStallWords(t *testing.T) {
	for _, f := range append([]sealFailure{sealUnreported, sealFailureStorage, sealFailureInternal}, sealFailures...) {
		if w, ok := SealStallWords(f.code); !ok || w != f.words {
			t.Errorf("SealStallWords(%q) = %q, %v; want %q", f.code, w, ok, f.words)
		}
	}
	if w, ok := SealStallWords("from_a_newer_binary"); ok || w != "" {
		t.Errorf("unknown code: %q, %v; want \"\", false", w, ok)
	}
}

// sqliteErr is the driver's error from running stmt on a scratch database
// holding table f, limited to its current pages: SQLITE_FULL for an insert,
// SQLITE_ERROR for a syntax error, and SQLITE_BUSY for BEGIN IMMEDIATE, whose
// write lock another connection holds.
func sqliteErr(t *testing.T, stmt string) error {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "e.db") + "?_pragma=busy_timeout(0)"
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open()
	if _, err := db.ExecContext(ctx, `CREATE TABLE f(x)`); err != nil {
		t.Fatal(err)
	}
	if stmt == "BEGIN IMMEDIATE" {
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = holder.ExecContext(ctx, `ROLLBACK`); _ = holder.Close() })
		db = open()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA max_page_count = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, stmt); err == nil {
		t.Fatalf("%s succeeded", stmt)
	}
	return err
}
