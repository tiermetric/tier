package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// newPlainSealFixture is newSealFixture's data under a handler built as serve
// builds it: no WithUnsealedRecompute, and no sealer until EnableSealing.
func newPlainSealFixture(t *testing.T, opts ...Option) (*Handler, *store.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plain.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	registerTestStore(db, path)
	seedSealFixture(t, db)
	seedRepoCostAt(t, db, repoAlpha, "b1", "i-early", 1, sealFixtureMonth.AddDate(0, 0, -16))
	h := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", RateLimitConfig{}, opts...)
	h.SetAggregation(scoring.AggregationTeam, 5)
	return h, db
}

// TestSealedRead_NoSealerFailsClosed (#913 switch-on): in team and division
// mode a handler with no sealer answers /scores, /report_manifest and
// /scores/compare with a 503 naming the missing sealer, whatever the query,
// and never computes a live body. Developer mode, and a handler built
// WithUnsealedRecompute, read live: the controls that the 503 is the sealer's
// absence and not a broken fixture.
func TestSealedRead_NoSealerFailsClosed(t *testing.T) {
	window := "?since=2026-05-01&until=2026-06-01"
	paths := []string{"/api/v1/scores", "/api/v1/scores" + window, "/api/v1/scores?period=2026-05",
		"/api/v1/report_manifest", "/api/v1/report_manifest" + window}
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h, db := newPlainSealFixture(t)
		h.SetAggregation(mode, 5)
		for _, path := range paths {
			if rec := sealedGet(t, h, path); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "has no sealer") ||
				strings.Contains(rec.Body.String(), "teams") {
				t.Errorf("%s %s: %d %s, want a 503 naming the missing sealer", mode, path, rec.Code, rec.Body)
			}
		}
		for _, q := range []string{"", "?period_a=2026-04&period_b=2026-05", "?since_a=2026-04-01&until_a=2026-05-01&since_b=2026-05-01&until_b=2026-06-01"} {
			if rec := compareGet(t, h, q); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "has no sealer") {
				t.Errorf("%s compare %q: %d %s, want a 503 naming the missing sealer", mode, q, rec.Code, rec.Body)
			}
		}
		if n := sealedRows(t, db); n != 0 {
			t.Errorf("%s: %d months sealed, want 0", mode, n)
		}
	}

	dev, _ := newPlainSealFixture(t)
	dev.SetAggregation(scoring.AggregationDeveloper, 0)
	recompute, _ := newPlainSealFixture(t, WithUnsealedRecompute())
	for name, h := range map[string]*Handler{"developer mode": dev, "WithUnsealedRecompute": recompute} {
		for _, path := range []string{"/api/v1/scores" + window, "/api/v1/report_manifest" + window} {
			if rec := sealedGet(t, h, path); rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "" {
				t.Errorf("control, %s, %s: %d %s, want a live 200", name, path, rec.Code, rec.Body)
			}
		}
	}
}

// TestEnableSealing_BuildsOnlyInAnonymisedModes: EnableSealing builds a sealer
// under serve's grace and seal_from in team and division mode, none in
// developer mode, and refuses a grace below the minimum or a malformed month.
func TestEnableSealing_BuildsOnlyInAnonymisedModes(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h, _ := newPlainSealFixture(t)
		h.SetAggregation(mode, 5)
		if err := h.EnableSealing(sealGrace, "2026-05"); err != nil || h.sealer == nil {
			t.Fatalf("%s: err %v, sealer %v; want a sealer", mode, err, h.sealer)
		}
		if h.sealer.cfg.grace != sealGrace || h.sealer.cfg.sealFrom.String() != "2026-05" {
			t.Errorf("%s: cfg %+v, want grace %s and seal_from 2026-05", mode, h.sealer.cfg, sealGrace)
		}
	}
	dev, _ := newPlainSealFixture(t)
	dev.SetAggregation(scoring.AggregationDeveloper, 0)
	if err := dev.EnableSealing(sealGrace, ""); err != nil || dev.sealer != nil {
		t.Errorf("developer mode: err %v, sealer %v; want none", err, dev.sealer)
	}
	for _, c := range []struct {
		grace    time.Duration
		sealFrom string
	}{{time.Hour, ""}, {sealGrace, "2026-5"}, {sealGrace, "2026-Q2"}} {
		h, _ := newPlainSealFixture(t)
		if err := h.EnableSealing(c.grace, c.sealFrom); err == nil || h.sealer != nil {
			t.Errorf("grace %s, seal_from %q: err %v, sealer %v; want refused and no sealer", c.grace, c.sealFrom, err, h.sealer)
		}
	}
}

// TestServeSealing_WritableSealsAndStops (#913 switch-on): serve's two calls,
// EnableSealing then StartSealer, on a writable team-mode handler seal May in
// the startup pass (clock 1 July), serve it, and stop when the context ends.
func TestServeSealing_WritableSealsAndStops(t *testing.T) {
	h, db := newPlainSealFixture(t)
	if err := h.EnableSealing(sealGrace, "2026-05"); err != nil {
		t.Fatal(err)
	}
	h.sealer.now = func() time.Time { return sealNow }
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-05"); rec.Code != http.StatusNotFound {
		t.Fatalf("before the pass: %d %s, want a 404", rec.Code, rec.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, started := h.StartSealer(ctx, false)
	if !started {
		t.Fatal("a writable team-mode handler did not start its sealer")
	}
	eventually(t, "the startup pass to seal May", func() bool { return sealedRows(t, db) == 1 })
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-05"); rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "2026-05" {
		t.Errorf("after the pass: %d %s, want May's sealed body", rec.Code, rec.Body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sealer did not stop when its context ended")
	}
}

// TestServeSealing_ReadOnlyServesButNeverSeals (#913-D4 ruling B′): a
// read-only team-mode serve builds its sealer, so it serves sealed months and
// names an unsealed one, but never starts it: StartSealer returns a closed done
// and seals nothing. The control: the same database's writable pass seals May,
// which the read-only handler then serves.
func TestServeSealing_ReadOnlyServesButNeverSeals(t *testing.T) {
	h, db := newPlainSealFixture(t)
	if err := h.EnableSealing(sealGrace, "2026-05"); err != nil {
		t.Fatal(err)
	}
	h.sealer.now = func() time.Time { return sealNow }
	done, started := h.StartSealer(context.Background(), true)
	select {
	case <-done:
	default:
		t.Fatal("read-only: done is open, want it closed")
	}
	if started || sealedRows(t, db) != 0 {
		t.Fatalf("read-only: started %v, %d months sealed; want never started and none", started, sealedRows(t, db))
	}
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-05"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "sealable but not yet sealed") {
		t.Errorf("read-only, May unsealed: %d %s, want the sealed read's 404", rec.Code, rec.Body)
	}
	sealPass(t, h.sealer)
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-05"); rec.Code != http.StatusOK {
		t.Errorf("control: read-only after a writable pass sealed May: %d %s, want 200", rec.Code, rec.Body)
	}
}

// TestSealConfigGap (#913-D1 ruling A): with May sealed at k=5, a handler at
// k=6 reports May, June as the first month its config applies to, and both
// configs; at k=5 it reports none, and so does a handler with no sealer.
func TestSealConfigGap(t *testing.T) {
	h, _ := newPlainSealFixture(t)
	if gap, err := h.SealConfigGap(context.Background()); err != nil || gap != (SealConfigGap{}) {
		t.Errorf("no sealer: %+v %v, want none", gap, err)
	}
	if err := h.EnableSealing(sealGrace, "2026-05"); err != nil {
		t.Fatal(err)
	}
	h.sealer.now = func() time.Time { return sealNow }
	if gap, err := h.SealConfigGap(context.Background()); err != nil || gap != (SealConfigGap{}) {
		t.Errorf("nothing sealed: %+v %v, want none", gap, err)
	}
	sealPass(t, h.sealer)
	if gap, err := h.SealConfigGap(context.Background()); err != nil || gap != (SealConfigGap{}) {
		t.Errorf("sealed under the current config: %+v %v, want none", gap, err)
	}
	h.SetAggregation(scoring.AggregationTeam, 6)
	gap, err := h.SealConfigGap(context.Background())
	if err != nil || gap.Newest != "2026-05" || gap.From != "2026-06" ||
		!strings.Contains(gap.Sealed, "k=5 ") || !strings.Contains(gap.Current, "k=6 ") || !strings.Contains(gap.Current, "aggregation=team") {
		t.Errorf("k raised to 6: %+v %v; want May sealed at k=5, June on at k=6", gap, err)
	}
}
