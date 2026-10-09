package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// sealAug1 is 1 August 2026: with sealGrace, May and June 2026 are sealable.
var sealAug1 = time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

// countSnapshots counts the seal snapshots s opens.
func countSnapshots(s *sealer) *atomic.Int64 {
	var n atomic.Int64
	s.inSnapshot = func() { n.Add(1) }
	return &n
}

// eventually polls cond until it holds or ten seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestSealDue_SealsOwedMonthsOldestFirst: one pass seals every owed month, May
// before June, one snapshot each; a second pass opens no snapshot.
func TestSealDue_SealsOwedMonthsOldestFirst(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return sealAug1 }
	snaps := countSnapshots(s)
	sealPass(t, s)
	var order []string
	rows, err := rawSealStore(t, db).Query(`SELECT period_start FROM sealed_report ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		order = append(order, p)
	}
	_ = rows.Close()
	if got := strings.Join(order, ","); got != "2026-05-01T00:00:00Z,2026-06-01T00:00:00Z" {
		t.Errorf("sealed in order %s, want May then June", got)
	}
	hl := s.health()
	if snaps.Load() != 2 || hl.failures != 0 || !hl.owedSince.IsZero() || !hl.armed || !hl.lastPass.Equal(sealAug1) {
		t.Errorf("%d snapshots, health %+v; want 2, no failure, nothing owed, armed, last pass at %s", snaps.Load(), hl, sealAug1)
	}
	sealPass(t, s)
	if snaps.Load() != 2 || sealedRows(t, db) != 2 {
		t.Errorf("second pass: %d snapshots, %d sealed; want no new snapshot and 2 sealed", snaps.Load(), sealedRows(t, db))
	}
}

// TestSealDue_NotArmedSealsNothing (#913-D4 ruling B′): with no seal_from and no
// pinned floor a pass seals nothing, pins nothing and counts no failure; every
// sealed read is a 404 saying sealing is not armed. A pinned floor arms it.
func TestSealDue_NotArmedSealsNothing(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	s.cfg.sealFrom = Period{}
	if err := s.sealDue(context.Background()); !errors.Is(err, errSealingNotArmed) {
		t.Fatalf("unarmed pass: %v, want errSealingNotArmed", err)
	}
	if hl := s.health(); hl.armed || hl.failures != 0 {
		t.Errorf("unarmed health %+v, want not armed and no failure", hl)
	}
	raw := rawSealStore(t, db)
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM sealed_report`) + rawCount(t, raw, `SELECT COUNT(*) FROM seal_floor`); n != 0 {
		t.Errorf("unarmed pass wrote %d sealed_report/seal_floor rows, want 0", n)
	}
	for _, path := range []string{"/api/v1/scores?period=2026-05", "/api/v1/scores", "/api/v1/report_manifest?period=2026-05"} {
		rec := sealedGet(t, h, path)
		if body := decodeJSON(t, rec); rec.Code != http.StatusNotFound || body["period"] != "2026-05" ||
			!strings.HasPrefix(body["error"].(string), "sealing not armed: ") {
			t.Errorf("%s unarmed: %d %s, want a 404 for 2026-05 saying sealing not armed", path, rec.Code, rec.Body)
		}
	}
	if rec := compareGet(t, h, "?period_a=2026-05&period_b=2026-06"); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "sealing not armed") {
		t.Errorf("compare unarmed: %d %s, want a 404 saying sealing not armed", rec.Code, rec.Body)
	}

	if _, err := raw.Exec(`INSERT INTO seal_floor (id, period_start) VALUES (1, '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	sealPass(t, s)
	if n := sealedRows(t, db); n != 1 || !s.health().armed {
		t.Errorf("armed by a pinned floor: %d sealed, armed %v; want May sealed", n, s.health().armed)
	}
}

// TestSealDue_ArmingFloorIsLaterOfSealFromAndCoverage (#913-D4 ruling B′): the
// earliest sealable month is the later of seal_from and the first full month of
// cost coverage (May), and the first seal pins it.
func TestSealDue_ArmingFloorIsLaterOfSealFromAndCoverage(t *testing.T) {
	for _, c := range []struct {
		sealFrom, floor, sealed string
	}{
		{"2026-06", "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z"},
		{"2026-01", "2026-05-01T00:00:00Z", "2026-05-01T00:00:00Z,2026-06-01T00:00:00Z"},
	} {
		h, db, s := newSealedReadHandler(t)
		s.now = func() time.Time { return sealAug1 }
		from, err := parsePeriod(c.sealFrom)
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.sealFrom = from
		sealPass(t, s)
		raw := rawSealStore(t, db)
		var floor, sealed string
		if err := raw.QueryRow(`SELECT period_start FROM seal_floor`).Scan(&floor); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT group_concat(period_start) FROM (SELECT period_start FROM sealed_report ORDER BY id)`).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if floor != c.floor || sealed != c.sealed {
			t.Errorf("seal_from %s: floor %s, sealed %s; want %s and %s", c.sealFrom, floor, sealed, c.floor, c.sealed)
		}
		// The pinned floor outlives a lowered seal_from.
		s.cfg.sealFrom = Period{Kind: periodMonth, Start: time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)}
		sealPass(t, s)
		if got, _, err := s.floor(context.Background(), db); err != nil || got.Start.Format(time.RFC3339) != c.floor {
			t.Errorf("seal_from %s lowered: floor %s %v, want the pinned %s", c.sealFrom, got, err, c.floor)
		}
		if c.sealFrom == "2026-06" {
			rec := sealedGet(t, h, "/api/v1/scores?period=2026-05")
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "before the earliest sealable month") || sealedRows(t, db) != 1 {
				t.Errorf("May before a June floor: %d %s, %d sealed; want a 404 and June alone", rec.Code, rec.Body, sealedRows(t, db))
			}
		}
	}
}

// TestSealDue_ClockBackwardsRefused (#913-D4): a server clock earlier than a
// period already sealed refuses the seal and counts a failure; a seal time in
// the past lets the same pass seal.
func TestSealDue_ClockBackwardsRefused(t *testing.T) {
	for _, c := range []struct {
		sealedAt string
		wantErr  error
		sealed   int
	}{
		{"2999-01-01T00:00:00Z", store.ErrSealClockBehind, 1},
		{"2000-01-01T00:00:00Z", nil, 2},
	} {
		_, db, s := newSealedReadHandler(t)
		if _, err := rawSealStore(t, db).Exec(`INSERT INTO sealed_report
			(level, period_size, period_start, period_end, k, config_digest, sealed_at, body, body_digest, tool_version, tool_commit)
			VALUES ('team', 'month', '2026-04-01T00:00:00Z', '2026-05-01T00:00:00Z', 5, 'sha256:x', ?, x'7b7d', 'sha256:y', 'v', '')`,
			c.sealedAt); err != nil {
			t.Fatal(err)
		}
		err := s.sealDue(context.Background())
		failures := int64(0)
		if c.wantErr != nil {
			failures = 1
		}
		if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) || s.health().failures != failures || sealedRows(t, db) != c.sealed {
			t.Errorf("newest sealed_at %s: %v, %d failures, %d sealed; want %v, %d and %d",
				c.sealedAt, err, s.health().failures, sealedRows(t, db), c.wantErr, failures, c.sealed)
		}
	}
}

// TestSealer_OneSnapshotAtATime (#913-D4): passes racing on one sealer never
// hold two seal snapshots at once, so a seal holds at most one pooled
// connection; the two owed months are each computed exactly once.
func TestSealer_OneSnapshotAtATime(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return sealAug1 }
	var open, most, calls atomic.Int64
	var mu sync.Mutex
	s.inSnapshot = func() {
		n := open.Add(1)
		mu.Lock()
		most.Store(max(most.Load(), n))
		mu.Unlock()
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		open.Add(-1)
	}
	const passes = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, passes)
	for range passes {
		wg.Go(func() {
			<-start
			errs <- s.sealDue(context.Background())
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("pass: %v", err)
		}
	}
	if most.Load() != 1 || calls.Load() != 2 || sealedRows(t, db) != 2 {
		t.Errorf("%d snapshots open at once, %d computed, %d sealed; want 1, 2 and 2", most.Load(), calls.Load(), sealedRows(t, db))
	}
}

// TestSealedRead_GetsNeverSeal (#913-D4 ruling B′): a read of a month that is
// sealable but not sealed opens no seal snapshot, writes nothing, and is a 404
// saying it is not yet sealed; the next pass seals it and the read serves it.
func TestSealedRead_GetsNeverSeal(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return sealAug1 }
	snaps := countSnapshots(s)
	before := sealedState(t, db)
	for _, path := range []string{"/api/v1/scores?period=2026-05", "/api/v1/scores", "/api/v1/report_manifest?period=2026-06"} {
		if rec := sealedGet(t, h, path); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "sealable but not yet sealed") {
			t.Errorf("%s: %d %s, want a 404 saying not yet sealed", path, rec.Code, rec.Body)
		}
	}
	for _, q := range []string{"", "?period_a=2026-05&period_b=2026-06"} {
		if rec := compareGet(t, h, q); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "sealable but not yet sealed") {
			t.Errorf("compare %q: %d %s, want a 404 saying not yet sealed", q, rec.Code, rec.Body)
		}
	}
	raw := rawSealStore(t, db)
	if after := sealedState(t, db); after != before || snaps.Load() != 0 || rawCount(t, raw, `SELECT COUNT(*) FROM seal_floor`) != 0 {
		t.Errorf("reads opened %d seal snapshots or wrote:\nbefore %q\nafter %q", snaps.Load(), before, after)
	}
	sealPass(t, s)
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-05"); rec.Code != http.StatusOK {
		t.Errorf("control: May after a pass: %d %s, want 200", rec.Code, rec.Body)
	}
}

// TestStartSealer_NeverStartsReadOnlyOrDeveloper (#913-D4): a read-only server,
// developer mode and a handler with no sealer never start the background
// sealer; a writable team-mode server does, and its startup pass seals.
func TestStartSealer_NeverStartsReadOnlyOrDeveloper(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readOnly, _, _ := newSealedReadHandler(t)
	developer, _, _ := newSealedReadHandler(t)
	developer.SetAggregation(scoring.AggregationDeveloper, 0)
	noSealer, _, _ := newSealFixture(t)
	for name, c := range map[string]struct {
		h        *Handler
		readOnly bool
	}{"read-only": {readOnly, true}, "developer": {developer, false}, "no sealer": {noSealer, false}} {
		done, started := c.h.StartSealer(ctx, c.readOnly)
		select {
		case <-done:
		default:
			t.Errorf("%s: done is open, want it closed so a shutdown never waits on a sealer that never started", name)
		}
		if started {
			t.Errorf("%s: started, want never started", name)
		}
	}

	h, db, _ := newSealedReadHandler(t)
	done, started := h.StartSealer(ctx, false)
	if !started {
		t.Fatal("control: a writable team-mode server did not start the sealer")
	}
	eventually(t, "the startup pass to seal May", func() bool { return sealedRows(t, db) == 1 })
	cancel()
	<-done
}

// TestSealer_StartupPassThenTicker: the startup pass runs at once, and each
// tick runs another, which seals a month that became owed after startup; the
// sealer stops when its context ends.
func TestSealer_StartupPassThenTicker(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	var now atomic.Int64
	now.Store(time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC).UnixNano())
	s.now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	s.tick = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, _ := h.StartSealer(ctx, false)
	eventually(t, "the startup pass", func() bool { return !s.health().lastPass.IsZero() })
	if n := sealedRows(t, db); n != 0 {
		t.Fatalf("startup pass on 10 June sealed %d periods, want 0", n)
	}
	now.Store(sealNow.UnixNano())
	eventually(t, "a tick to seal May", func() bool { return sealedRows(t, db) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sealer did not stop when its context ended")
	}
}

// sealedOrder is the sealed months' starts in the order they were sealed.
func sealedOrder(t *testing.T, db *store.DB) string {
	t.Helper()
	var order string
	q := `SELECT COALESCE(group_concat(period_start), '') FROM (SELECT period_start FROM sealed_report ORDER BY id)`
	if err := rawSealStore(t, db).QueryRow(q).Scan(&order); err != nil {
		t.Fatal(err)
	}
	return order
}

// TestSealDue_StopsAtFirstFailure (#913-D4, R-2026-09-30-1): a pass whose
// oldest owed month (May) fails seals no later month (June), and June's read
// names May and why without promising the next pass; the next pass seals May
// then June and clears the stall.
func TestSealDue_StopsAtFirstFailure(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return sealAug1 }
	ctx := context.Background()
	conn, err := rawSealStore(t, db).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	held, calls := false, 0
	release := func() {
		if held {
			if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
				t.Fatal(err)
			}
			held = false
		}
	}
	// May's seal meets a held write lock; a later seal in the same pass frees
	// it first, so only stopping at May leaves June unsealed.
	s.beforeSeal = func() {
		if calls++; calls > 1 {
			release()
			return
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		held = true
	}
	err = s.sealDue(ctx)
	release()
	if !errors.Is(err, store.ErrWriteLockUnavailable) || !strings.Contains(err.Error(), "stopped at 2026-05") || sealedRows(t, db) != 0 {
		t.Fatalf("May failing: %v, %d sealed; want ErrWriteLockUnavailable at 2026-05 and nothing sealed", err, sealedRows(t, db))
	}
	rec := sealedGet(t, h, "/api/v1/scores?period=2026-06")
	if body := rec.Body.String(); rec.Code != http.StatusNotFound || strings.Contains(body, "next seal pass") ||
		!strings.Contains(body, "sealing is stalled at 2026-05 (the database write lock was busy)") {
		t.Errorf("June behind a failed May: %d %s, want a 404 naming 2026-05 and why, and no next pass", rec.Code, body)
	}
	sealPass(t, s)
	if got := sealedOrder(t, db); got != "2026-05-01T00:00:00Z,2026-06-01T00:00:00Z" || s.stall.Load() != nil {
		t.Errorf("next pass sealed %s, stall %v; want May then June and no stall", got, s.stall.Load())
	}
}

// lookupFailingStore fails LatestSealedPeriod, a pass's first read.
type lookupFailingStore struct{ *store.DB }

func (lookupFailingStore) LatestSealedPeriod(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("injected lookup failure")
}

// TestSealDue_LookupFailureCountsAndKeepsGauge: a pass whose first lookup fails
// counts a failure and leaves the lag gauge and armed as the last pass left
// them; with the store back, the next pass seals and clears the gauge.
func TestSealDue_LookupFailureCountsAndKeepsGauge(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	owed := time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC)
	s.owedSince.Store(owed.UnixNano())
	s.notArmed.Store(true)
	h.store = lookupFailingStore{db}
	err := s.sealDue(context.Background())
	if hl := s.health(); err == nil || hl.failures != 1 || !hl.owedSince.Equal(owed) || hl.armed {
		t.Errorf("failed lookup: %v, health %+v; want an error, 1 failure, owed since %s and armed unchanged", err, hl, owed)
	}
	h.store = db
	sealPass(t, s)
	if hl := s.health(); sealedRows(t, db) != 1 || !hl.owedSince.IsZero() || !hl.armed {
		t.Errorf("control: next pass %d sealed, health %+v; want May sealed, nothing owed, armed", sealedRows(t, db), hl)
	}
}

// coverageLaterStore reports cost coverage from 1 June outside a seal's
// snapshot, where the store's coverage (15 April) makes May the earliest month.
type coverageLaterStore struct{ *store.DB }

func (coverageLaterStore) CostCoverageStart(context.Context, store.RepoScope) (time.Time, bool, error) {
	return time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC), true, nil
}

// TestSealDue_FloorMovedDuringPassRefused: a pass whose earliest sealable month
// (June, read before the seal) is not its seal snapshot's (May) seals and pins
// nothing, so May is never skipped; the next pass seals May then June.
func TestSealDue_FloorMovedDuringPassRefused(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return sealAug1 }
	h.store = coverageLaterStore{db}
	err := s.sealDue(context.Background())
	raw := rawSealStore(t, db)
	if n := sealedRows(t, db) + rawCount(t, raw, `SELECT COUNT(*) FROM seal_floor`); !errors.Is(err, errSealFloorMoved) || n != 0 {
		t.Fatalf("floor moved: %v, %d sealed_report/seal_floor rows; want errSealFloorMoved and none", err, n)
	}
	h.store = db
	sealPass(t, s)
	if got := sealedOrder(t, db); got != "2026-05-01T00:00:00Z,2026-06-01T00:00:00Z" {
		t.Errorf("next pass sealed %s, want May then June", got)
	}
}

// TestNewSealer_RefusesSealFromNotACalendarMonth: a set sealFrom must be a
// month period starting on the 1st; an unset one leaves sealing unarmed.
func TestNewSealer_RefusesSealFromNotACalendarMonth(t *testing.T) {
	h, _ := newTestHandler(t)
	may := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		from Period
		ok   bool
	}{
		"mid-month": {Period{Kind: periodMonth, Start: may.AddDate(0, 0, 14)}, false},
		"no kind":   {Period{Start: may}, false},
		"month":     {Period{Kind: periodMonth, Start: may}, true},
		"unset":     {Period{}, true},
	} {
		if s, err := newSealer(h, sealConfig{grace: minSealGrace, sealFrom: c.from}); (err == nil) != c.ok || (s != nil) != c.ok {
			t.Errorf("%s: sealer %v err %v, want accepted %v", name, s, err, c.ok)
		}
	}
}

// TestSealer_PanicIsAnInternalFailure: a seal that panics is logged, commits
// nothing and fails its pass as internal, and the background sealer keeps
// running and seals the month on a later pass.
func TestSealer_PanicIsAnInternalFailure(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.tick = 10 * time.Millisecond
	var calls atomic.Int64
	s.inSnapshot = func() {
		if calls.Add(1) <= 2 {
			panic("boom")
		}
	}
	if err := s.sealDue(context.Background()); !errors.Is(err, errSealPanicked) {
		t.Fatalf("panicking pass: %v, want errSealPanicked", err)
	}
	st := s.stall.Load()
	if n := sealedRows(t, db); n != 0 || st == nil || st.code != store.SealGapInternal || !st.month.Start.Equal(sealMay.Start) || s.health().failures != 1 {
		t.Fatalf("after a panic: %d sealed, stall %+v, health %+v; want 0 sealed, May stalled as internal, 1 failure", n, st, s.health())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, _ := h.StartSealer(ctx, false)
	eventually(t, "a pass after a panicking startup pass to seal May", func() bool { return sealedRows(t, db) == 1 })
	cancel()
	<-done
	if calls.Load() < 3 || s.stall.Load() != nil {
		t.Errorf("%d seal snapshots, stall %+v; want the startup pass to panic and a later one to seal", calls.Load(), s.stall.Load())
	}
	if n := strings.Count(logs.String(), `msg="seal panicked" month=2026-05 panic="\"boom\""`); n != 2 {
		t.Errorf("logged %d seal panics, want 2:\n%s", n, logs.String())
	}
}
