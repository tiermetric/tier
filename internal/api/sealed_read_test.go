package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

var updateSealedGolden = flag.Bool("update-913-sealed-golden", false, "rewrite testdata/sealed_scores_913.golden.json")

// newSealedReadHandler is newSealFixture with its sealer wired into the handler:
// team mode, k=5, now 1 July 2026, so May 2026 is the latest sealable month and
// the earliest (the cost horizon is 15 April).
func newSealedReadHandler(t *testing.T) (*Handler, *store.DB, *sealer) {
	t.Helper()
	h, db, s := newSealFixture(t)
	h.sealer = s
	return h, db, s
}

// sealedGet serves path through /scores or /report_manifest's handler.
func sealedGet(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if strings.HasPrefix(path, "/api/v1/scores") {
		h.handleGetScores(rec, req)
	} else {
		h.handleGetReportManifest(rec, req)
	}
	return rec
}

func sealedRows(t *testing.T, db *store.DB) int {
	t.Helper()
	return rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_report`)
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestSealedRead_NoSealerServesLiveOutput pins the gate: with no sealer and
// WithUnsealedRecompute (newTestHandler's default; serve never passes it) an
// anonymised read is the live read, and a sealer in developer mode changes
// nothing. TestSealedRead_NoSealerFailsClosed pins the 503 without it. TestScores_AnonymisedLiveOutputUnchanged
// pins the live anonymised bytes themselves.
func TestSealedRead_NoSealerServesLiveOutput(t *testing.T) {
	window := "?since=2026-05-01&until=2026-06-01"
	live, db, _ := newSealFixture(t)
	for _, path := range []string{"/api/v1/scores" + window, "/api/v1/report_manifest" + window} {
		rec := sealedGet(t, live, path)
		if rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "" {
			t.Errorf("no sealer, %s: %d %q, want a live 200 with no period header", path, rec.Code, rec.Header().Get(headerSealedPeriod))
		}
		rec = sealedGet(t, live, strings.Split(path, "?")[0]+"?period=2026-05")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown query parameter(s): period") {
			t.Errorf("no sealer, ?period=: %d %s, want today's unknown-parameter 400", rec.Code, rec.Body.String())
		}
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("no sealer: %d periods sealed, want 0", n)
	}

	plain, _, _ := newSealFixture(t)
	sealed, db, _ := newSealedReadHandler(t)
	plain.SetAggregation(scoring.AggregationDeveloper, 0)
	sealed.SetAggregation(scoring.AggregationDeveloper, 0)
	want := sealedGet(t, plain, "/api/v1/scores"+window)
	got := sealedGet(t, sealed, "/api/v1/scores"+window)
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
		t.Errorf("developer mode with a sealer: %d %s\nwant %s", got.Code, got.Body, want.Body)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("developer mode with a sealer: %d periods sealed, want 0", n)
	}
}

// TestSealedRead_ServesStoredBytesVerbatim: a sealed /scores body is the stored
// sealed_report.body byte for byte, on the sealing read and every read after a
// late row; its markers ride in headers, and its sha256 is the manifest's
// body_digest.
func TestSealedRead_ServesStoredBytesVerbatim(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	sealPass(t, s)
	first := sealedGet(t, h, "/api/v1/scores?period=2026-05")
	if first.Code != http.StatusOK {
		t.Fatalf("%d %s", first.Code, first.Body)
	}
	var stored []byte
	var sealedAt string
	if err := rawSealStore(t, db).QueryRow(`SELECT body, sealed_at FROM sealed_report`).Scan(&stored, &sealedAt); err != nil {
		t.Fatal(err)
	}
	seedLateMayRow(t, db)
	again := sealedGet(t, h, "/api/v1/scores?period=2026-05")
	for _, rec := range []*httptest.ResponseRecorder{first, again} {
		if !bytes.Equal(rec.Body.Bytes(), stored) {
			t.Errorf("served %s\nwant the stored bytes %s", rec.Body, stored)
		}
		if p, at := rec.Header().Get(headerSealedPeriod), rec.Header().Get(headerSealedAt); p != "2026-05" || at != sealedAt {
			t.Errorf("markers %s=%q %s=%q, want 2026-05 and %s", headerSealedPeriod, p, headerSealedAt, at, sealedAt)
		}
	}
	man := decodeJSON(t, sealedGet(t, h, "/api/v1/report_manifest?period=2026-05"))
	sum := sha256.Sum256(first.Body.Bytes())
	if man["body_digest"] != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("manifest body_digest %v, want the sha256 of the served body", man["body_digest"])
	}
}

// TestSealedRead_Golden pins one sealed body as served (price_table masked).
func TestSealedRead_Golden(t *testing.T) {
	h, _, s := newSealedReadHandler(t)
	sealPass(t, s)
	got := decodeJSON(t, sealedGet(t, h, "/api/v1/scores?period=2026-05"))
	got["price_table"] = "<price_table>"
	out, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	path := filepath.Join("testdata", "sealed_scores_913.golden.json")
	if *updateSealedGolden {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Errorf("sealed May body changed; diff %s against:\n%s", path, out)
	}
}

// TestSealedRead_DefaultReadIsLatestSealed (#913-D2 item 3): no ?period= serves
// the latest sealed month, never defaultSince's window; with nothing sealable
// yet it is a 404 naming the earliest month and when it becomes sealable.
func TestSealedRead_DefaultReadIsLatestSealed(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	sealPass(t, s)
	rec := sealedGet(t, h, "/api/v1/scores")
	if rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "2026-05" || decodeJSON(t, rec)["since"] != "2026-05-01" {
		t.Fatalf("default read: %d %q %s, want May sealed", rec.Code, rec.Header().Get(headerSealedPeriod), rec.Body)
	}
	for k, v := range map[string]string{headerNextSealAt: "2026-07-15T00:00:00Z", headerEarliestPeriod: "2026-05", headerLatestPeriod: "2026-05"} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("default read %s = %q, want %q", k, got, v)
		}
	}
	if defaultSince := sinceUTC(time.Now().AddDate(0, 0, -30)).Format("2006-01-02"); defaultSince == "2026-05-01" {
		t.Fatal("control: defaultSince coincides with the sealed month")
	}
	if n := sealedRows(t, db); n != 1 {
		t.Errorf("default read sealed %d periods, want May alone", n)
	}

	h, db, s = newSealedReadHandler(t)
	s.now = func() time.Time { return time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC) }
	sealPass(t, s)
	for _, path := range []string{"/api/v1/scores", "/api/v1/report_manifest"} {
		rec = sealedGet(t, h, path)
		body := decodeJSON(t, rec)
		if rec.Code != http.StatusNotFound || body["aggregation"] != "team" || body["period"] != "2026-05" ||
			body["sealable_at"] != "2026-06-15T00:00:00Z" {
			t.Errorf("%s with nothing sealable: %d %s, want 404 naming May and its sealable_at", path, rec.Code, rec.Body)
		}
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("%d periods sealed, want 0", n)
	}

	// A grace raised after June sealed moves the latest sealable month back to
	// May; the default read still serves June, the latest sealed month.
	h, db, s = newSealedReadHandler(t)
	s.now = func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) }
	sealPass(t, s)
	if rec = sealedGet(t, h, "/api/v1/scores"); rec.Header().Get(headerSealedPeriod) != "2026-06" {
		t.Fatalf("default read on 1 Aug: %d %q, want June sealed", rec.Code, rec.Header().Get(headerSealedPeriod))
	}
	s.cfg.grace = 45 * 24 * time.Hour
	if last := lastSealable(s.now(), s.cfg.grace); last.String() != "2026-05" {
		t.Fatalf("control: the raised grace leaves the latest sealable month at %s", last)
	}
	rec = sealedGet(t, h, "/api/v1/scores")
	if rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "2026-06" || rec.Header().Get(headerLatestPeriod) != "2026-06" {
		t.Errorf("default read after the grace rose: %d %q latest %q, want June", rec.Code,
			rec.Header().Get(headerSealedPeriod), rec.Header().Get(headerLatestPeriod))
	}
	if m := decodeJSON(t, sealedGet(t, h, "/api/v1/report_manifest")); m["period"] != "2026-06" || m["latest_period"] != "2026-06" {
		t.Errorf("default manifest after the grace rose: period %v latest %v, want June", m["period"], m["latest_period"])
	}
	if n := sealedRows(t, db); n != 2 {
		t.Errorf("%d periods sealed, want May and June", n)
	}

	// The latest sealable month refused (it reaches into the retention horizon)
	// while May is sealed: the default read serves May.
	h, _, s = newSealedReadHandler(t)
	sealPass(t, s)
	s.now = func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) }
	h.retentionHorizon = time.Date(2026, time.June, 2, 0, 0, 0, 0, time.UTC)
	if rec = sealedGet(t, h, "/api/v1/scores?period=2026-06"); rec.Code != http.StatusNotFound {
		t.Fatalf("control: June inside the retention horizon: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec = sealedGet(t, h, "/api/v1/scores"); rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriod) != "2026-05" {
		t.Errorf("default read with June unsealable: %d %q %s, want May", rec.Code, rec.Header().Get(headerSealedPeriod), rec.Body)
	}
}

// TestSealedRead_Refusals: every refusal of a sealed read, none of which seals.
func TestSealedRead_Refusals(t *testing.T) {
	h, db, _ := newSealedReadHandler(t)
	for _, c := range []struct {
		path       string
		code       int
		contains   string
		sealableAt string
	}{
		{"/api/v1/scores?period=2026-06", 404, "grace", "2026-07-15T00:00:00Z"},
		{"/api/v1/scores?period=2026-09", 404, "grace", "2026-10-15T00:00:00Z"},
		{"/api/v1/scores?period=2026-04", 404, "earliest sealable", ""},
		{"/api/v1/report_manifest?period=2026-04", 404, "earliest sealable", ""},
		{"/api/v1/scores?since=2026-05-01", 400, "?period=", ""},
		{"/api/v1/scores?until=2026-06-01", 400, "?period=", ""},
		{"/api/v1/scores?before=2026-06-01", 400, "?period=", ""},
		{"/api/v1/scores?period=2026-05&since=2026-05-01", 400, "?period=", ""},
		{"/api/v1/report_manifest?since=2026-05-01", 400, "?period=", ""},
		{"/api/v1/scores?team=big", 400, "?team= not accepted with a sealed month", ""},
		{"/api/v1/scores?work_type=feature", 400, "?work_type= not accepted", ""},
		{"/api/v1/scores?repo=acme/alpha", 400, "?repo= not accepted", ""},
		{"/api/v1/report_manifest?repo=acme/alpha", 400, "?repo= not accepted", ""},
		{"/api/v1/report_manifest?team=big", 400, "unknown query parameter(s): team — accepted: period.", ""},
		{"/api/v1/scores?bogus=1", 400, "unknown query parameter(s): bogus — accepted: period.", ""},
		{"/api/v1/scores?period=2026-13", 400, "invalid period", ""},
		{"/api/v1/scores?period=", 400, "invalid period", ""},
		{"/api/v1/scores?period=2026-05&period=2026-04", 400, "repeated", ""},
	} {
		rec := sealedGet(t, h, c.path)
		body := decodeJSON(t, rec)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s: %d %s, want %d containing %q", c.path, rec.Code, rec.Body, c.code, c.contains)
		}
		if strings.Contains(rec.Body.String(), "developer") {
			t.Errorf("%s: the refusal names a mode switch: %s", c.path, rec.Body)
		}
		if got, _ := body["sealable_at"].(string); c.code == 404 && got != c.sealableAt {
			t.Errorf("%s: sealable_at %q, want %q", c.path, got, c.sealableAt)
		}
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("refusals sealed %d periods, want 0", n)
	}

	// On 1 May the earliest sealable month is May and April is inside its grace
	// lag: April never becomes sealable, so its 404 carries no sealable_at.
	h, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC) }
	rec := sealedGet(t, h, "/api/v1/scores?period=2026-04")
	if body := decodeJSON(t, rec); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "earliest sealable") || body["sealable_at"] != nil {
		t.Errorf("April before the floor, in grace: %d %s, want a 404 with no sealable_at", rec.Code, rec.Body)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("%d periods sealed, want 0", n)
	}
}

// TestSealedRead_AllowlistsExcludeFreeBounds: the sealed allowlists admit no
// free window bound, so the only route to a window is ?period=.
func TestSealedRead_AllowlistsExcludeFreeBounds(t *testing.T) {
	for name, list := range map[string][]string{"/scores": sealedScoresFilters, "/report_manifest": sealedManifestFilters} {
		for _, k := range append(append([]string(nil), sealedReadParams...), list...) {
			if slices.Contains(freeBoundParams, k) {
				t.Errorf("%s sealed allowlist admits free bound %q", name, k)
			}
		}
	}
	if !slices.Contains(sealedReadParams, "period") {
		t.Error("sealed allowlist lacks period")
	}
}

// TestSealedRead_EmptyMonthShape: an empty sealed month is a 200 with the same
// top-level and data_quality keys as a k-suppressed one, never a 404, except
// the two documented differences: kanon_suppressed (the #593 count of withheld
// people, none in an empty month) and attributed_outcome_share (a ratio over
// the month's outcomes, none in an empty month). Conditional quality signals
// (unjoined_developers, zero_token_outcome_count, ...) appear only when their
// condition holds; this fixture's suppressed month triggers none, so the key
// sets compare equal here without that being true of every suppressed month.
func TestSealedRead_EmptyMonthShape(t *testing.T) {
	keysOf := func(m map[string]any, drop ...string) []string {
		var ks []string
		for k := range m {
			if !slices.Contains(drop, k) {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		return ks
	}
	keys := func(rec *httptest.ResponseRecorder) []string { return keysOf(decodeJSON(t, rec)) }
	h, _, s := newSealedReadHandler(t)
	s.now = func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) }
	sealPass(t, s)
	empty := sealedGet(t, h, "/api/v1/scores?period=2026-06")
	suppressedH, _, suppressedS := newSealedReadHandler(t)
	suppressedH.SetAggregation(scoring.AggregationTeam, 20)
	sealPass(t, suppressedS)
	suppressed := sealedGet(t, suppressedH, "/api/v1/scores?period=2026-05")
	if empty.Code != http.StatusOK || suppressed.Code != http.StatusOK {
		t.Fatalf("empty %d %s; suppressed %d %s", empty.Code, empty.Body, suppressed.Code, suppressed.Body)
	}
	if !strings.Contains(suppressed.Body.String(), `"withheld_teams":true`) {
		t.Fatalf("control: the k=20 month is not suppressed: %s", suppressed.Body)
	}
	if e, s := keys(empty), keys(suppressed); !slices.Equal(e, s) {
		t.Errorf("empty month keys %v, suppressed month keys %v", e, s)
	}
	emptyDQ, _ := decodeJSON(t, empty)["data_quality"].(map[string]any)
	suppressedDQ, _ := decodeJSON(t, suppressed)["data_quality"].(map[string]any)
	if emptyDQ["attribution_coverage"] != "not_shown" {
		t.Errorf("empty month attribution_coverage %v, want not_shown", emptyDQ["attribution_coverage"])
	}
	documented := []string{"kanon_suppressed", "attributed_outcome_share"}
	if e, s := keysOf(emptyDQ, documented...), keysOf(suppressedDQ, documented...); !slices.Equal(e, s) {
		t.Errorf("empty month data_quality keys %v, suppressed month %v", e, s)
	}
	for _, k := range documented {
		if _, ok := suppressedDQ[k]; !ok {
			t.Errorf("control: the suppressed month lacks %s, so excepting it proves nothing", k)
		}
	}
}

// TestSealedRead_FloorPinnedAtFirstSeal (#913-D2 item 4): the earliest sealable
// month is pinned by the first seal, so cost back-dated afterwards never makes an
// earlier month sealable.
func TestSealedRead_FloorPinnedAtFirstSeal(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-03"); rec.Code != http.StatusNotFound {
		t.Fatalf("March before any seal: %d %s, want 404", rec.Code, rec.Body)
	}
	sealPass(t, s)
	seedRepoCostAt(t, db, repoAlpha, "b1", "i-jan", 1, time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC))
	seedRepoCostAt(t, db, repoAlpha, "b1", "i-mar", 1, time.Date(2026, time.March, 5, 0, 0, 0, 0, time.UTC))
	horizon, _, err := db.CostCoverageStart(context.Background(), store.FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if live, ok := earliestSealable(horizon); !ok || !live.Start.Before(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("control: the back-dated cost left the live earliest month at %s", live)
	}
	for _, p := range []string{"2026-03", "2026-02"} {
		rec := sealedGet(t, h, "/api/v1/scores?period="+p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s after back-dated cost: %d %s, want 404", p, rec.Code, rec.Body)
		}
	}
	if floor, ok, err := s.floor(context.Background(), s.h.store); err != nil || !ok || floor.String() != "2026-05" {
		t.Errorf("floor %s %v %v, want the pinned 2026-05", floor, ok, err)
	}
	sealPass(t, s)
	if n := sealedRows(t, db); n != 1 {
		t.Errorf("%d periods sealed, want May alone", n)
	}
}

// TestSealedManifest publishes the sealed month's provenance and config, names
// a config gap, and carries no late-arrival count.
func TestSealedManifest(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	sealPass(t, s)
	scores := sealedGet(t, h, "/api/v1/scores?period=2026-05")
	rec := sealedGet(t, h, "/api/v1/report_manifest?period=2026-05")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	m := decodeJSON(t, rec)
	want := map[string]any{
		"manifest_schema": SealedManifestSchema, "period": "2026-05",
		"period_start": "2026-05-01T00:00:00Z", "period_end": "2026-06-01T00:00:00Z",
		"sealed_at": scores.Header().Get(headerSealedAt), "next_seal_at": "2026-07-15T00:00:00Z",
		"earliest_period": "2026-05", "latest_period": "2026-05", "tool_version": h.buildIdentity().Version,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	for header, field := range map[string]string{headerNextSealAt: "next_seal_at", headerEarliestPeriod: "earliest_period", headerLatestPeriod: "latest_period"} {
		if got := scores.Header().Get(header); got == "" || got != m[field] {
			t.Errorf("/scores %s = %q, want the manifest's %s %v", header, got, field, m[field])
		}
	}
	cfg, _ := m["config"].(map[string]any)
	if cfg["aggregation"] != "team" || cfg["period_size"] != "month" || cfg["k"] != 5.0 || cfg["fold_rule"] != 1.0 ||
		cfg["digest"] != sealConfigDigest(sealFoldRule, scoring.AggregationTeam, periodMonth, 5) {
		t.Errorf("config %v", cfg)
	}
	if _, ok := m["config_gap"]; ok {
		t.Errorf("config_gap under an unchanged config: %v", m["config_gap"])
	}
	for _, k := range []string{"watermarks", "events_digest", "outcomes_digest", "since"} {
		if _, ok := m[k]; ok {
			t.Errorf("sealed manifest carries live-window field %q", k)
		}
	}

	seedLateMayRow(t, db)
	if late := sealedGet(t, h, "/api/v1/report_manifest?period=2026-05"); !bytes.Equal(late.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("a late May row changed the sealed manifest:\n%s\nwas\n%s", late.Body, rec.Body)
	}

	h.SetAggregation(scoring.AggregationTeam, 6)
	gap := decodeJSON(t, sealedGet(t, h, "/api/v1/report_manifest?period=2026-05"))
	cur, _ := gap["config_gap"].(map[string]any)["current"].(map[string]any)
	if cur["k"] != 6.0 || gap["config"].(map[string]any)["k"] != 5.0 {
		t.Errorf("after k=6: config %v, gap %v, want the sealed k=5 and a gap naming k=6", gap["config"], gap["config_gap"])
	}
	if again := sealedGet(t, h, "/api/v1/scores?period=2026-05"); !bytes.Equal(again.Body.Bytes(), scores.Body.Bytes()) {
		t.Error("after k=6 the sealed May body changed")
	}
}

// TestSealDue_InTransactionRefusals: refusals SealReport makes inside its
// transaction, after the pre-checks passed, stop the pass with that refusal and
// count a failure: a floor pinned meanwhile past the month, and an erase on
// every attempt.
func TestSealDue_InTransactionRefusals(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	s.beforeSeal = func() {
		if _, err := rawSealStore(t, db).Exec(`INSERT INTO seal_floor (id, period_start) VALUES (1, '2026-06-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.sealDue(context.Background()); !errors.Is(err, store.ErrSealNotNext) || s.health().failures != 1 {
		t.Errorf("floor pinned past May mid-seal: %v, %d failures; want ErrSealNotNext and 1", err, s.health().failures)
	}

	_, db, s = newSealedReadHandler(t)
	s.beforeSeal = func() {
		if _, err := db.EraseDeveloper(context.Background(), "nobody"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.sealDue(context.Background()); !errors.Is(err, store.ErrSealEraseRaced) || s.health().failures != 1 {
		t.Errorf("erase on every attempt: %v, %d failures; want ErrSealEraseRaced and 1", err, s.health().failures)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("%d periods sealed, want 0", n)
	}
}

// TestSeal_FirstSealMustBeTheEarliest (#913-D5 ruling C′): the first seal is
// the earliest sealable month, so sealing June first is refused and pins
// nothing; May then seals and pins May, and June follows it.
func TestSeal_FirstSealMustBeTheEarliest(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	s.now = func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) }
	june := Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)}
	if _, _, err := s.sealOrLoad(context.Background(), june); !errors.Is(err, store.ErrSealNotNext) {
		t.Fatalf("June first: %v, want store.ErrSealNotNext", err)
	}
	if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM seal_floor`); n != 0 {
		t.Fatalf("a refused first seal pinned %d floors, want 0", n)
	}
	for _, p := range []Period{sealMay, june} {
		if _, _, err := s.sealOrLoad(context.Background(), p); err != nil {
			t.Errorf("seal %s: %v", p, err)
		}
	}
	if floor, ok, err := s.floor(context.Background(), s.h.store); err != nil || !ok || floor.String() != "2026-05" {
		t.Errorf("pinned floor %s %v %v, want 2026-05", floor, ok, err)
	}
}

// TestSealDue_WriteLockContentionRetriedNextPass: a pass that meets another
// connection holding the write lock stops with ErrWriteLockUnavailable, counts a
// failure, reports the month owed since it became sealable, and seals nothing;
// the next pass seals the month and clears the gauge.
func TestSealDue_WriteLockContentionRetriedNextPass(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	ctx := context.Background()
	conn, err := rawSealStore(t, db).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	held := false
	s.beforeSeal = func() {
		s.beforeSeal = nil
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		held = true
	}
	err = s.sealDue(ctx)
	if held {
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
	}
	if !held {
		t.Fatal("control: the write lock was never held")
	}
	hl := s.health()
	if !errors.Is(err, store.ErrWriteLockUnavailable) || hl.failures != 1 || hl.owedSince.Format(time.RFC3339) != "2026-06-15T00:00:00Z" {
		t.Errorf("under write-lock contention: %v, health %+v; want ErrWriteLockUnavailable, 1 failure, owed since 2026-06-15", err, hl)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("%d periods sealed under contention, want 0", n)
	}
	sealPass(t, s)
	if hl := s.health(); sealedRows(t, db) != 1 || hl.failures != 1 || !hl.owedSince.IsZero() {
		t.Errorf("next pass: %d sealed, health %+v; want May sealed, 1 failure and no month owed", sealedRows(t, db), hl)
	}
}

// TestSealedConfigOf_UnknownFoldRule: a stored digest no fold rule this binary
// knows yields (sealed by a newer binary) names no fold_rule and echoes the
// digest; a known one names its rule.
func TestSealedConfigOf_UnknownFoldRule(t *testing.T) {
	newer := sealConfigDigestOf(sealFoldRule+1, "team", "month", 5)
	if c := sealedConfigOf("team", "month", 5, newer); c.FoldRule != 0 || c.Digest != newer {
		t.Errorf("newer rule's digest: fold_rule %d digest %q, want no rule and %q", c.FoldRule, c.Digest, newer)
	}
	known := sealConfigDigestOf(sealFoldRule, "team", "month", 5)
	if c := sealedConfigOf("team", "month", 5, known); c.FoldRule != sealFoldRule {
		t.Errorf("control: current rule's digest names fold_rule %d, want %d", c.FoldRule, sealFoldRule)
	}
}

// TestNewSealer_RefusesGraceBelowMinimum: a sealer is never built with a grace
// below minSealGrace, including the zero value.
func TestNewSealer_RefusesGraceBelowMinimum(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, g := range []time.Duration{0, minSealGrace - time.Second} {
		if s, err := newSealer(h, sealConfig{grace: g}); err == nil || s != nil {
			t.Errorf("grace %s: sealer %v err %v, want a refusal", g, s, err)
		}
	}
	if s, err := newSealer(h, sealConfig{grace: minSealGrace}); err != nil || s.cfg.grace != minSealGrace {
		t.Errorf("grace %s: %v, want a sealer under it", minSealGrace, err)
	}
}
