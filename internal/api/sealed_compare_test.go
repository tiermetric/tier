package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

var updateCompareLiveGolden = flag.Bool("update-913-compare-golden", false, "rewrite testdata/anonymised_compare_live_913.golden.json")

var (
	compareMay  = time.Date(2026, time.May, 10, 3, 0, 0, 0, time.UTC)
	compareJune = time.Date(2026, time.June, 10, 3, 0, 0, 0, time.UTC)
	// compareNow makes June the latest sealable month; the floor is May.
	compareNow = time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	// compareLiveWindows is the live compare over the same two months.
	compareLiveWindows = "?since_a=2026-05-01&until_a=2026-06-01&since_b=2026-06-01&until_b=2026-07-01"
)

// seedCompareFixture seeds May and June 2026 at costs whose sums are exact in
// any order. "both" has 5 people in each month; "onlymay" 5 in May and 2 in
// June, so it clears k=5 in May alone; "junenew" 3 in June only. At k=5 the
// residual is 5 people in each month. An April row puts the horizon before May.
func seedCompareFixture(t *testing.T, db *store.DB) {
	t.Helper()
	seedCompareGroups(t, db, []compareGroup{
		{"both", "a", 5, 1, 2, compareMay},
		{"onlymay", "o", 5, 2, 1, compareMay},
		{"both", "c", 5, 1.5, 2, compareJune},
		{"onlymay", "p", 2, 0.5, 1, compareJune},
		{"junenew", "j", 3, 0.25, 3, compareJune},
	})
}

// compareGroup is n people of one team, each with one cost row and one outcome.
type compareGroup struct {
	team, prefix string
	n            int
	cost, weight float64
	at           time.Time
}

// seedCompareGroups seeds groups plus an April row that puts the cost horizon
// before May, so May is the earliest sealable month.
func seedCompareGroups(t *testing.T, db *store.DB, groups []compareGroup) {
	t.Helper()
	registerGated(t, db) // serve has registered its sources, none of them gated (#913-D9)
	seedRepoCostAt(t, db, repoAlpha, "early", "i-early", 1, time.Date(2026, time.April, 15, 0, 0, 0, 0, time.UTC))
	for _, g := range groups {
		for i := 1; i <= g.n; i++ {
			seedKAnonDev(t, db, g.team, fmt.Sprintf("%s%d-%s", g.prefix, i, g.at.Month()), g.cost, g.weight, g.at)
		}
	}
}

// newSealedCompareHandler is seedCompareFixture in team mode at k, with a
// sealer whose clock is compareNow.
func newSealedCompareHandler(t *testing.T, k int) (*Handler, *store.DB, *sealer) {
	t.Helper()
	h, db := newTestHandler(t)
	seedCompareFixture(t, db)
	h.SetAggregation(scoring.AggregationTeam, k)
	s := newTestSealer(h)
	s.now = func() time.Time { return compareNow }
	h.sealer = s
	return h, db, s
}

func compareGet(t *testing.T, h *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleGetScoresCompare(rec, httptest.NewRequest(http.MethodGet, "/api/v1/scores/compare"+query, nil))
	return rec
}

// liveCompare is the live comparison of the same two months on h's store.
func liveCompare(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	s := h.sealer
	h.sealer = nil
	defer func() { h.sealer = s }()
	rec := compareGet(t, h, compareLiveWindows)
	if rec.Code != http.StatusOK {
		t.Fatalf("live compare: %d %s", rec.Code, rec.Body)
	}
	return rec
}

// sealedState is every stored sealed row, so a test can assert a read wrote nothing.
func sealedState(t *testing.T, db *store.DB) string {
	t.Helper()
	raw := rawSealStore(t, db)
	var out strings.Builder
	for _, q := range []string{
		`SELECT id, level, period_start, k, config_digest, sealed_at, hex(body), body_digest FROM sealed_report ORDER BY id`,
		`SELECT report_id, label, weighted_points, total_cost_usd, actual_paid_usd, realtime_usd, sample_n,
			flagged_outcomes, has_points, has_cost, has_realtime, has_non_realtime, has_paid, contributes
			FROM sealed_rollup ORDER BY report_id, label`,
		`SELECT report_id, label, measure, hex(person_key) FROM sealed_person ORDER BY 1, 2, 3, 4`,
	} {
		rows, err := raw.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&out, vals...)
		}
		_ = rows.Close()
	}
	return out.String()
}

// comparedTeams lists the team names of a compare body.
func comparedTeams(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Teams []struct {
			Team string `json:"team"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tm := range body.Teams {
		names = append(names, tm.Team)
	}
	return names
}

// TestSealedCompare_NoSealerServesLiveOutput pins the gate: with no sealer an
// anonymised compare is today's live compare, its raw bytes equal to the golden
// taken from a9abc21 (price_table masked), with no Tier-* header, and ?period_a=
// is an unknown parameter.
func TestSealedCompare_NoSealerServesLiveOutput(t *testing.T) {
	h, db := newTestHandler(t)
	seedCompareFixture(t, db)
	pt, err := json.Marshal(priceTableStamp(store.ActivePriceTableInfo()))
	if err != nil {
		t.Fatal(err)
	}
	field := append([]byte(`"price_table":`), pt...)
	var gotJSON bytes.Buffer
	for _, c := range []struct {
		name string
		mode scoring.AggregationMode
		k    int
	}{
		{"team-k5", scoring.AggregationTeam, 5},
		{"division-k5", scoring.AggregationDivision, 5},
		{"team-k11", scoring.AggregationTeam, 11},
	} {
		h.SetAggregation(c.mode, c.k)
		rec := compareGet(t, h, compareLiveWindows)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s, want a live 200", c.name, rec.Code, rec.Body)
		}
		for hdr := range rec.Header() {
			if strings.HasPrefix(hdr, "Tier-") {
				t.Errorf("%s: the live compare carries the sealed header %s", c.name, hdr)
			}
		}
		if n := bytes.Count(rec.Body.Bytes(), field); n != 1 {
			t.Fatalf("%s: price_table %s found %d times, want once: %s", c.name, pt, n, rec.Body)
		}
		body := bytes.Replace(rec.Body.Bytes(), field, []byte(`"price_table":"<price_table>"`), 1)
		fmt.Fprintf(&gotJSON, "== %s ==\n%s\n", c.name, bytes.TrimSuffix(body, []byte("\n")))
		rec = compareGet(t, h, "?period_a=2026-05&period_b=2026-06")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown query parameter(s): period_a") {
			t.Errorf("%s ?period_a=: %d %s, want today's unknown-parameter 400", c.name, rec.Code, rec.Body)
		}
	}
	path := filepath.Join("testdata", "anonymised_compare_live_913.golden.json")
	if *updateCompareLiveGolden {
		if err := os.WriteFile(path, gotJSON.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON.Bytes(), want) {
		t.Errorf("live anonymised compare changed:\n got %s\nwant %s", gotJSON.Bytes(), want)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("no sealer: %d periods sealed, want 0", n)
	}

	// A sealer in developer mode changes nothing either.
	dev, devDB, _ := newSealedCompareHandler(t, 5)
	dev.SetAggregation(scoring.AggregationDeveloper, 0)
	if rec := compareGet(t, dev, compareLiveWindows); rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriodA) != "" {
		t.Errorf("developer mode with a sealer: %d %s, want a live 200", rec.Code, rec.Body)
	}
	if n := sealedRows(t, devDB); n != 0 {
		t.Errorf("developer mode with a sealer: %d periods sealed, want 0", n)
	}
}

// TestSealedCompare_DefaultAndExplicitPair: neither period is the two latest
// sealed months; the explicit pair serves the same body; both carry each
// month's markers.
func TestSealedCompare_DefaultAndExplicitPair(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	sealPass(t, s)
	def := compareGet(t, h, "")
	if def.Code != http.StatusOK {
		t.Fatalf("default: %d %s", def.Code, def.Body)
	}
	if n := sealedRows(t, db); n != 2 {
		t.Errorf("%d periods sealed, want May and June", n)
	}
	explicit := compareGet(t, h, "?period_a=2026-05&period_b=2026-06")
	if !bytes.Equal(explicit.Body.Bytes(), def.Body.Bytes()) {
		t.Errorf("explicit pair %s\nwant the default pair's %s", explicit.Body, def.Body)
	}
	raw := rawSealStore(t, db)
	for _, rec := range []*httptest.ResponseRecorder{def, explicit} {
		for hdr, want := range map[string]string{
			headerSealedPeriodA: "2026-05", headerSealedPeriodB: "2026-06",
			headerEarliestPeriod: "2026-05", headerLatestPeriod: "2026-06",
			headerNextSealAt: "2026-08-15T00:00:00Z",
		} {
			if got := rec.Header().Get(hdr); got != want {
				t.Errorf("%s = %q, want %q", hdr, got, want)
			}
		}
		for hdr, start := range map[string]string{headerSealedAtA: "2026-05-01T00:00:00Z", headerSealedAtB: "2026-06-01T00:00:00Z"} {
			var at string
			if err := raw.QueryRow(`SELECT sealed_at FROM sealed_report WHERE period_start = ?`, start).Scan(&at); err != nil {
				t.Fatal(err)
			}
			if got := rec.Header().Get(hdr); got != at {
				t.Errorf("%s = %q, want the stored %q", hdr, got, at)
			}
		}
	}
	body := decodeJSON(t, def)
	wa, _ := body["window_a"].(map[string]any)
	wb, _ := body["window_b"].(map[string]any)
	if body["mode"] != "team" || wa["since"] != "2026-05-01" || wa["until"] != "2026-06-01" ||
		wb["since"] != "2026-06-01" || wb["until"] != "2026-07-01" {
		t.Errorf("mode/windows %v %v %v", body["mode"], wa, wb)
	}
	// mode and k are the sealed ones: after a switch to division at k=11, which
	// would withhold this comparison, the two months sealed under team at k=5
	// still compare, byte for byte.
	h.SetAggregation(scoring.AggregationDivision, 11)
	if rec := compareGet(t, h, ""); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), def.Body.Bytes()) {
		t.Errorf("after a switch to division at k=11: %d %s\nwant the team k=5 body %s", rec.Code, rec.Body, def.Body)
	}
}

// TestSealedCompare_Refusals: reversed, equal, half and free-bound pairs are
// 400s that name ?period_a=/?period_b= and never a mode; none seals anything.
func TestSealedCompare_Refusals(t *testing.T) {
	h, db, _ := newSealedCompareHandler(t, 5)
	for _, c := range []struct{ query, contains string }{
		{"?period_a=2026-06&period_b=2026-05", "must be earlier than ?period_b="},
		{"?period_a=2026-05&period_b=2026-05", "same period"},
		{"?period_a=2026-05", "together"},
		{"?since_a=2026-05-01&until_a=2026-06-01&since_b=2026-06-01&until_b=2026-07-01", "?period_a=YYYY-MM and ?period_b=YYYY-MM"},
		{"?since_b=2026-06-01", "Remove ?since_b= and select two months with ?period_a=YYYY-MM and ?period_b=YYYY-MM"},
		{"?before=2026-06-01", "?period_a=YYYY-MM and ?period_b=YYYY-MM"},
		{"?period_a=2026-05&period_b=2026-06&since=2026-05-01", "?period_a=YYYY-MM and ?period_b=YYYY-MM"},
		{"?period=2026-05", "unknown query parameter(s): period — accepted: period_a, period_b."},
	} {
		rec := compareGet(t, h, c.query)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s: %d %s, want a 400 containing %q", c.query, rec.Code, rec.Body, c.contains)
		}
		for _, banned := range []string{"developer", "aggregation"} {
			if strings.Contains(strings.ToLower(rec.Body.String()), banned) {
				t.Errorf("%s: the refusal names %q, a mode switch: %s", c.query, banned, rec.Body)
			}
		}
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("refusals sealed %d periods, want 0", n)
	}
	for _, k := range sealedCompareParams {
		if slices.Contains(freeBoundParams, k) {
			t.Errorf("sealed compare allowlist admits free bound %q", k)
		}
	}
}

// TestSealedCompare_FewerThanTwoSealable: on 1 July May is the only sealable
// month, so the default pair is a 404 naming June and when it becomes sealable;
// an explicit month before the floor is a 404 naming it.
func TestSealedCompare_FewerThanTwoSealable(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	s.now = func() time.Time { return sealNow }
	sealPass(t, s)
	rec := compareGet(t, h, "")
	body := decodeJSON(t, rec)
	if rec.Code != http.StatusNotFound || body["period"] != "2026-06" || body["aggregation"] != "team" ||
		body["sealable_at"] != "2026-07-15T00:00:00Z" {
		t.Errorf("default pair: %d %s, want a 404 naming 2026-06, sealable 2026-07-15", rec.Code, rec.Body)
	}
	if n := sealedRows(t, db); n != 1 {
		t.Errorf("%d periods sealed, want May alone", n)
	}
	rec = compareGet(t, h, "?period_a=2026-04&period_b=2026-05")
	if body := decodeJSON(t, rec); rec.Code != http.StatusNotFound || body["period"] != "2026-04" || body["sealable_at"] != nil {
		t.Errorf("April before the floor: %d %s, want a 404 naming 2026-04 with no sealable_at", rec.Code, rec.Body)
	}
	if n := sealedRows(t, db); n != 1 {
		t.Errorf("%d periods sealed after the April/May 404, want May alone", n)
	}
}

// TestSealedCompare_DefaultPairAfterGraceRaise: May through July are sealed,
// then a raised grace makes May the latest sealable month. The default pair is
// still the two latest sealed months, June and July: a raised grace never hides
// a month already sealed.
func TestSealedCompare_DefaultPairAfterGraceRaise(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	s.now = func() time.Time { return time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC) }
	for _, month := range []string{"2026-05", "2026-06", "2026-07"} {
		p, err := parsePeriod(month)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.sealOrLoad(t.Context(), p); err != nil {
			t.Fatalf("seal %s: %v", month, err)
		}
	}
	s.cfg.grace = 90 * 24 * time.Hour // the latest sealable month is now May
	rec := compareGet(t, h, "")
	if rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriodA) != "2026-06" || rec.Header().Get(headerSealedPeriodB) != "2026-07" {
		t.Errorf("default pair: %d %v %s, want 2026-06 and 2026-07", rec.Code, rec.Header(), rec.Body)
	}
	if n := sealedRows(t, db); n != 3 {
		t.Errorf("%d periods sealed, want May through July", n)
	}
}

// TestSealedCompare_DefaultPairWhileSealerLags (#913-D4): May and June are
// sealed and July is sealable but not yet sealed; the default pair is the two
// newest sealed months, and the read seals nothing.
func TestSealedCompare_DefaultPairWhileSealerLags(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	sealPass(t, s)
	s.now = func() time.Time { return compareNow.AddDate(0, 1, 0) }
	if rec := compareGet(t, h, "?period_a=2026-06&period_b=2026-07"); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "sealable but not yet sealed") {
		t.Fatalf("control: July sealable and unsealed: %d %s", rec.Code, rec.Body)
	}
	rec := compareGet(t, h, "")
	if rec.Code != http.StatusOK || rec.Header().Get(headerSealedPeriodA) != "2026-05" || rec.Header().Get(headerSealedPeriodB) != "2026-06" {
		t.Errorf("default pair: %d %v %s, want 2026-05 and 2026-06", rec.Code, rec.Header(), rec.Body)
	}
	if n := sealedRows(t, db); n != 2 {
		t.Errorf("%d periods sealed, want May and June only", n)
	}
}

// TestSealedCompare_CrossConfigRefused: May sealed at k=5 and June at k=6 split
// the population two ways, so their comparison is a 409 naming both configs.
func TestSealedCompare_CrossConfigRefused(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	if _, _, err := s.sealOrLoad(t.Context(), Period{Kind: periodMonth, Start: time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("seal May: %v", err)
	}
	h.SetAggregation(scoring.AggregationTeam, 6)
	before := sealedState(t, db)
	sealPass(t, s)
	rec := compareGet(t, h, "")
	body := decodeJSON(t, rec)
	ca, _ := body["config_a"].(map[string]any)
	cb, _ := body["config_b"].(map[string]any)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "different configs") ||
		ca["k"] != float64(5) || cb["k"] != float64(6) || body["teams"] != nil ||
		body["period_a"] != "2026-05" || body["period_b"] != "2026-06" {
		t.Errorf("cross-config pair: %d %s, want a 409 naming k=5 and k=6 and no rows", rec.Code, rec.Body)
	}
	if after := sealedState(t, db); !strings.HasPrefix(after, strings.SplitN(before, "\n", 2)[0]) || sealedRows(t, db) != 2 {
		t.Errorf("May's stored row changed or June was not sealed under k=6:\nbefore %s\nafter %s", before, after)
	}
	// Control: the same pair under one config compares.
	h2, _, s2 := newSealedCompareHandler(t, 6)
	sealPass(t, s2)
	if rec := compareGet(t, h2, ""); rec.Code != http.StatusOK {
		t.Errorf("same-config pair: %d %s, want 200", rec.Code, rec.Body)
	}
}

// TestSealedCompare_IntersectionNamesOnlyLabelsClearingBoth (#277): "onlymay"
// clears k in May alone, so the comparison folds it into "other" on both sides,
// although May's own sealed body names it.
func TestSealedCompare_IntersectionNamesOnlyLabelsClearingBoth(t *testing.T) {
	h, _, s := newSealedCompareHandler(t, 5)
	sealPass(t, s)
	may := sealedGet(t, h, "/api/v1/scores?period=2026-05")
	if !strings.Contains(may.Body.String(), `"team":"onlymay"`) {
		t.Fatalf("control: May's body does not name onlymay: %s", may.Body)
	}
	rec := compareGet(t, h, "")
	if got := strings.Join(comparedTeams(t, rec), ","); got != "both,other" {
		t.Errorf("compared teams %q, want both,other: %s", got, rec.Body)
	}
}

// TestSealedCompare_RefoldEqualsLiveCompare: the refold of two sealed months is
// the live compare over the same two windows (CompareLabeledKAnon plus the #864
// own-residual check), for a named row with a residual, an all-folded residual,
// and a withheld comparison.
//
// Each window's data_quality is the live window's, less cost_coverage_safe_since,
// which a sealed body omits; June's zero-token outcome makes the two months'
// blocks differ.
func TestSealedCompare_RefoldEqualsLiveCompare(t *testing.T) {
	for _, k := range []int{5, 6, 11} {
		h, db, s := newSealedCompareHandler(t, k)
		seedRepoOutcomeAt(t, db, repoAlpha, "c1-June", "i-zero-token", 1, compareJune)
		sealPass(t, s)
		sealed := decodeJSON(t, compareGet(t, h, ""))
		live := decodeJSON(t, liveCompare(t, h))
		for _, win := range []string{"window_a", "window_b"} {
			dq, _ := live[win].(map[string]any)["data_quality"].(map[string]any)
			if dq["cost_coverage_safe_since"] == nil {
				t.Fatalf("control: live %s has no cost_coverage_safe_since: %v", win, dq)
			}
			delete(dq, "cost_coverage_safe_since")
			if g, w := fmt.Sprint(sealed[win].(map[string]any)["data_quality"]), fmt.Sprint(dq); g != w {
				t.Errorf("k=%d %s data_quality: sealed %s\nlive %s", k, win, g, w)
			}
		}
		if fmt.Sprint(sealed["window_a"].(map[string]any)["data_quality"]) == fmt.Sprint(sealed["window_b"].(map[string]any)["data_quality"]) {
			t.Errorf("k=%d control: May's and June's data_quality are equal, so a swap would pass", k)
		}
		for _, key := range []string{"mode", "developers", "teams", "total", "kanon_suppressed"} {
			if g, w := fmt.Sprint(sealed[key]), fmt.Sprint(live[key]); g != w {
				t.Errorf("k=%d %s: sealed %s\nlive %s", k, key, g, w)
			}
		}
		for _, win := range []string{"window_a", "window_b"} {
			s, l := sealed[win].(map[string]any), live[win].(map[string]any)
			if s["since"] != l["since"] || s["until"] != l["until"] {
				t.Errorf("k=%d %s bounds: sealed %v live %v", k, win, s, l)
			}
		}
		if k == 11 && sealed["kanon_suppressed"] == nil {
			t.Errorf("k=11: comparison not withheld: %v", sealed)
		}
	}
}

// TestSealedCompare_NoLiveReadAndStoredBytesUnchanged: rows arriving after both
// months are sealed change the live compare (the control) but neither the sealed
// comparison nor any stored sealed row.
func TestSealedCompare_NoLiveReadAndStoredBytesUnchanged(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	sealPass(t, s)
	first := compareGet(t, h, "")
	if first.Code != http.StatusOK {
		t.Fatalf("%d %s", first.Code, first.Body)
	}
	state := sealedState(t, db)
	liveBefore := liveCompare(t, h).Body.String()
	for i := 1; i <= 5; i++ {
		team := "onlymay" // June's onlymay reaches 5 and clears k in both months
		if i > 3 {
			team = "junenew" // and keeps June's residual at k
		}
		seedKAnonDev(t, db, team, fmt.Sprintf("late%d", i), 4, 2, compareJune)
	}
	seedRepoCostAt(t, db, repoAlpha, "a1-May", "i-a1-May", 7, compareMay)
	liveAfter := liveCompare(t, h).Body.String()
	if liveAfter == liveBefore || !strings.Contains(liveAfter, `"team":"onlymay"`) {
		t.Fatalf("control: the late rows did not change the live compare: %s", liveAfter)
	}
	again := compareGet(t, h, "")
	if !bytes.Equal(again.Body.Bytes(), first.Body.Bytes()) {
		t.Errorf("sealed compare moved with late rows:\n got %s\nwant %s", again.Body, first.Body)
	}
	if got := sealedState(t, db); got != state {
		t.Errorf("stored sealed rows changed:\n got %s\nwant %s", got, state)
	}
}

// TestSealedCompare_OwnResidualWithholds (#864): "x" clears k in one month
// alone, so the intersection folds it with that month's sub-k team; that
// residual reaches k, but the month's own residual (the sub-k team alone) does
// not, and "other" minus that month's named x would be it. The whole comparison
// is withheld, as the live compare withholds it, whichever month holds it.
func TestSealedCompare_OwnResidualWithholds(t *testing.T) {
	for _, c := range []struct {
		name   string
		groups []compareGroup
	}{
		{"period_a", []compareGroup{
			{"x", "x", 5, 1, 2, compareMay}, {"y", "y", 2, 1, 2, compareMay},
			{"x", "u", 3, 1, 2, compareJune}, {"z", "z", 2, 1, 2, compareJune},
		}},
		{"period_b", []compareGroup{
			{"x", "u", 3, 1, 2, compareMay}, {"z", "z", 2, 1, 2, compareMay},
			{"x", "x", 5, 1, 2, compareJune}, {"y", "y", 2, 1, 2, compareJune},
		}},
	} {
		h, db := newTestHandler(t)
		seedCompareGroups(t, db, c.groups)
		h.SetAggregation(scoring.AggregationTeam, 5)
		s := newTestSealer(h)
		s.now = func() time.Time { return compareNow }
		h.sealer = s
		sealPass(t, s)

		sealed := decodeJSON(t, compareGet(t, h, ""))
		if sealed["kanon_suppressed"] == nil || sealed["teams"] != nil || sealed["total"] != nil {
			t.Errorf("%s: comparison not withheld: %v", c.name, sealed)
		}
		if live := decodeJSON(t, liveCompare(t, h)); fmt.Sprint(live["kanon_suppressed"]) != fmt.Sprint(sealed["kanon_suppressed"]) {
			t.Errorf("%s: kanon_suppressed: sealed %v, live %v", c.name, sealed["kanon_suppressed"], live["kanon_suppressed"])
		}
		// Control: the intersection alone withholds nothing here.
		ctx := t.Context()
		may, err := h.loadSealedSide(ctx, Period{Kind: periodMonth, Start: time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		june, err := h.loadSealedSide(ctx, Period{Kind: periodMonth, Start: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		if _, sup := scoring.CompareFolded(may.inputs, june.inputs, 5); sup.Any() {
			t.Fatalf("%s control: CompareFolded withholds on its own (%+v); this test no longer isolates the #864 check", c.name, sup)
		}
	}
}

// TestSealedCompare_EmptyMonths: two empty months compare with no teams, no
// total and nothing withheld; one empty month is a zero side of the total. Each
// equals the live compare.
func TestSealedCompare_EmptyMonths(t *testing.T) {
	for _, c := range []struct {
		name      string
		groups    []compareGroup
		wantTotal bool
	}{
		{"both empty", nil, false},
		{"June empty", []compareGroup{{"both", "a", 5, 1, 2, compareMay}}, true},
	} {
		h, db := newTestHandler(t)
		seedCompareGroups(t, db, c.groups)
		h.SetAggregation(scoring.AggregationTeam, 5)
		s := newTestSealer(h)
		s.now = func() time.Time { return compareNow }
		h.sealer = s
		sealPass(t, s)
		rec := compareGet(t, h, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		sealed, live := decodeJSON(t, rec), decodeJSON(t, liveCompare(t, h))
		if (sealed["total"] != nil) != c.wantTotal || sealed["kanon_suppressed"] != nil {
			t.Errorf("%s: total %v kanon_suppressed %v, want total %v and nothing withheld", c.name, sealed["total"], sealed["kanon_suppressed"], c.wantTotal)
		}
		for _, key := range []string{"teams", "total", "kanon_suppressed"} {
			if g, w := fmt.Sprint(sealed[key]), fmt.Sprint(live[key]); g != w {
				t.Errorf("%s %s: sealed %s\nlive %s", c.name, key, g, w)
			}
		}
	}
}

// TestSealedCompare_UnknownFoldRuleRefused: two months sealed under one config
// whose fold rule this binary does not refold are a 409, not a refold by this
// binary's rule.
func TestSealedCompare_UnknownFoldRuleRefused(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	sealPass(t, s)
	if rec := compareGet(t, h, ""); rec.Code != http.StatusOK {
		t.Fatalf("control: %d %s", rec.Code, rec.Body)
	}
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`DROP TRIGGER trg_sealed_report_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE sealed_report SET config_digest = ?`, sealConfigDigestOf(sealFoldRule+1, "team", "month", 5)); err != nil {
		t.Fatal(err)
	}
	rec := compareGet(t, h, "")
	body := decodeJSON(t, rec)
	ca, _ := body["config_a"].(map[string]any)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "fold rule") || ca["fold_rule"] != nil || body["teams"] != nil {
		t.Errorf("unknown fold rule: %d %s, want a 409 naming no fold_rule and no rows", rec.Code, rec.Body)
	}
}

// TestSealedCompare_TotalSidesAreStoredBodies: each side of total is its
// month's stored /scores total, never a recomputation, so the two endpoints
// agree for a month.
func TestSealedCompare_TotalSidesAreStoredBodies(t *testing.T) {
	h, db, s := newSealedCompareHandler(t, 5)
	sealPass(t, s)
	if rec := compareGet(t, h, ""); rec.Code != http.StatusOK {
		t.Fatalf("seal: %d %s", rec.Code, rec.Body)
	}
	raw := rawSealStore(t, db)
	const may = "2026-05-01T00:00:00Z"
	var stored []byte
	if err := raw.QueryRow(`SELECT body FROM sealed_report WHERE period_start = ?`, may).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(stored, &body); err != nil {
		t.Fatal(err)
	}
	body["total"].(map[string]any)["weighted_points"] = 12345.0
	marked, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TRIGGER trg_sealed_report_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE sealed_report SET body = ? WHERE period_start = ?`, marked, may); err != nil {
		t.Fatal(err)
	}
	got := decodeJSON(t, compareGet(t, h, ""))
	total, _ := got["total"].(map[string]any)
	if a, _ := total["a"].(map[string]any); a["weighted_points"] != 12345.0 {
		t.Errorf("total.a %v, want May's stored total (weighted_points 12345)", total["a"])
	}
}
