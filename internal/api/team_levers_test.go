package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

var leverNames = []string{"cache_read_share"}

// Each person costs $4. Half use cache reads; both input-side cells clear k.
func seedLeverGroup(t *testing.T, db *store.DB, team string, n int, at time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		dev := fmt.Sprintf("%s-%d", team, i)
		enrolIn(t, db, team, dev)
		event := store.TokenEvent{Developer: dev, Repo: repoAlpha, IssueID: "i-" + dev, Model: "claude-sonnet-4", CostMicro: 4_000_000, InputTok: 400, Source: "jsonl", Fidelity: "realtime", Timestamp: at}
		if i < n/2 {
			event.Model = "claude-opus-4-8"
			event.IssueID = store.UnattributedMainBucket
			event.InputTok, event.CacheRead, event.CacheWrite5m, event.CacheWrite1h = 100, 200, 50, 50
		}
		if err := db.InsertTokenEvent(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		if _, err := db.InsertOutcome(t.Context(), store.Outcome{Developer: dev, Repo: repoAlpha, IssueID: "i-" + dev, Weight: 1, Quality: 1, MergeCommitSHA: "sha-" + dev, Timestamp: at}); err != nil {
			t.Fatal(err)
		}
	}
}

// Exercise sealed-body assembly, where levers are allowed; live reads never
// carry them. Real seal/serve/refold is covered separately below.
func leverBody(t *testing.T, h *Handler) (map[string]any, []byte) {
	t.Helper()
	since, err := time.Parse("2006-01-02", scoresSince())
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := h.scoresForWindow(context.Background(), h.store, scoresQuery{since: since, until: time.Now().UTC().Add(time.Second), scope: store.FleetWide}, true)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw, body
}

// Cost carriers and work carriers independently clear the row floor, but only
// overlap seats carry BOTH. No #593 residual suppression can mask the lever gate.
func seedDisjointLeverGroup(t *testing.T, db *store.DB, team string, k, overlap int, at time.Time) {
	t.Helper()
	for i := 0; i < k; i++ {
		spender, worker := fmt.Sprintf("%s-spender-%d", team, i), fmt.Sprintf("%s-worker-%d", team, i)
		if i < overlap {
			worker = spender
		}
		enrolIn(t, db, team, spender, worker)
		cost := 2.0
		if i == 0 {
			cost = 7.77
		}
		seedCostAt(t, db, spender, "no-merge", cost, at)
		seedOutcomeAt(t, db, worker, "no-spend-"+worker, 1, 1, at)
	}
}

func assertPublishedLeverWithhold(t *testing.T, raw map[string]any, body []byte) {
	t.Helper()
	if rows, _ := raw["teams"].([]any); len(rows) != 2 || raw["total"] == nil {
		t.Fatalf("control: two row-safe groups and total must publish: %s", body)
	}
	dq := raw["data_quality"].(map[string]any)
	if dq["kanon_suppressed"] != nil || dq["team_levers_withheld"] != true || dq["team_levers_withheld_reason"] != "group_seats_below_k" {
		t.Fatalf("expected only lever-seat withholding: %s", body)
	}
	assertNoLevers(t, body)
}

func assertLeverControl(t *testing.T, h *Handler) {
	t.Helper()
	raw, body := leverBody(t, h)
	rows, _ := raw["teams"].([]any)
	if len(rows) == 0 {
		t.Fatalf("control needs a published team: %s", body)
	}
	for _, row := range rows {
		for _, name := range leverNames {
			if _, ok := row.(map[string]any)[name].(float64); !ok {
				t.Errorf("control missing numeric %s: %s", name, body)
			}
		}
	}
}

func assertNoLevers(t *testing.T, body []byte) {
	t.Helper()
	for _, name := range leverNames {
		if bytes.Contains(body, []byte(`"`+name+`"`)) {
			t.Errorf("withheld lever %s has a key: %s", name, body)
		}
	}
}

func TestTeamLeversKAnon_BelowFloorOmitsKeys(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			h.SetAggregation(mode, 3)
			seedLeverGroup(t, db, "healthy", 6, time.Now().UTC())
			assertLeverControl(t, h)
			seedDisjointLeverGroup(t, db, "disjoint", 3, 2, time.Now().UTC())
			raw, body := leverBody(t, h)
			assertPublishedLeverWithhold(t, raw, body)
		})
	}
}

func TestTeamLeversKAnon_OnlyCacheShareSamePopulation(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			h.SetAggregation(mode, 3)
			seedLeverGroup(t, db, "eng", 6, time.Now().UTC())
			// Spend-only people and bots do not lift the seat floor; their costs and
			// category memberships stay in the row population (three in this cell).
			enrolIn(t, db, "eng", "spend-only", "renovate[bot]", "spend-only-2")
			for _, dev := range []string{"spend-only", "renovate[bot]", "spend-only-2"} {
				seedCostAt(t, db, dev, store.UnattributedIssueID, 2, time.Now().UTC())
			}
			// Pseudo-developer spend is absent from both the team row and its levers.
			seedCostAt(t, db, store.UnattributedIssueID, store.UnattributedIssueID, 99, time.Now().UTC())
			raw, body := leverBody(t, h)
			var rows []map[string]any
			encoded, _ := json.Marshal(raw["teams"])
			if err := json.Unmarshal(encoded, &rows); err != nil || len(rows) != 1 {
				t.Fatalf("teams: %s (%v)", body, err)
			}
			row := rows[0]
			for _, row := range rows {
				for _, key := range []string{"premium_model_share", "unattributed_share"} {
					if _, present := row[key]; present {
						t.Errorf("team row contains forbidden %s: %s", key, body)
					}
				}
			}
			for name, want := range map[string]float64{"total_cost_usd": 30, "cache_read_share": 600.0 / 8400} {
				got, ok := row[name].(float64)
				if !ok || math.Abs(got-want) > 1e-9 {
					t.Errorf("%s = %v, want %v; %s", name, row[name], want, body)
				}
			}
			for _, key := range []string{"cost_composition", "by_model", "by_class", "work_types", "attributed_cost_usd", "unattributed_cost_usd"} {
				if bytes.Contains(body, []byte(`"`+key+`"`)) {
					t.Errorf("extra breakdown %s: %s", key, body)
				}
			}
			total := raw["total"].(map[string]any)
			for _, name := range leverNames {
				if _, ok := total[name]; ok {
					t.Errorf("org total has lever %s", name)
				}
			}
		})
	}
}

func TestTeamLeversKAnon_SpendOnlyCacheContributors(t *testing.T) {
	for _, mode := range anonymisedModes {
		for _, spenders := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/spenders-%d", mode, spenders), func(t *testing.T) {
				h, db := newTestHandler(t)
				h.SetAggregation(mode, 3)
				at := time.Now().UTC()
				for i := 0; i < 6+spenders; i++ {
					dev := fmt.Sprintf("eng-%d", i)
					enrolIn(t, db, "eng", dev)
					event := store.TokenEvent{Developer: dev, Repo: repoAlpha, IssueID: "i-" + dev, Model: "claude-sonnet-4", CostMicro: 4_000_000, InputTok: 100, Source: "jsonl", Fidelity: "realtime", Timestamp: at}
					if i >= 6 {
						event.Developer = "renovate-" + dev + "[bot]"
						enrolIn(t, db, "eng", event.Developer)
						event.CacheRead = 100
					}
					if err := db.InsertTokenEvent(t.Context(), event); err != nil {
						t.Fatal(err)
					}
					if i < 6 { // Six lever seats; cache contributors are spend-only bots.
						seedOutcomeAt(t, db, dev, "i-"+dev, 1, 1, at)
					}
				}
				if spenders == 3 {
					assertLeverControl(t, h)
					return
				}
				raw, body := leverBody(t, h)
				if rows, _ := raw["teams"].([]any); len(rows) != 1 || raw["total"] == nil {
					t.Fatalf("control: seat-safe cost row must publish: %s", body)
				}
				assertNoLevers(t, body)
				dq, _ := raw["data_quality"].(map[string]any)
				if dq["team_levers_withheld"] != true || dq["team_levers_withheld_reason"] != "category_seats_below_k" {
					t.Fatalf("spend-only cache withholding: %s", body)
				}
			})
		}
	}
}

func TestTeamLeversKAnon_ResidualCannotBeRecoveredByShareTimesCost(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			h.SetAggregation(mode, 5)
			seedLeverGroup(t, db, "eng", 10, time.Now().UTC())
			assertLeverControl(t, h)
			seedDisjointLeverGroup(t, db, "disjoint", 5, 2, time.Now().UTC())
			raw, body := leverBody(t, h)
			assertPublishedLeverWithhold(t, raw, body)
			// Enumerate signed sums of published costs and every share * its row cost.
			// This includes org total minus published teams, plus each lever's dollars.
			var dollars []float64
			rows, _ := raw["teams"].([]any)
			if total, ok := raw["total"].(map[string]any); ok {
				rows = append(rows, total)
			}
			for _, v := range rows {
				row := v.(map[string]any)
				cost, _ := row["total_cost_usd"].(float64)
				dollars = append(dollars, cost)
				for _, name := range leverNames {
					if share, ok := row[name].(float64); ok {
						dollars = append(dollars, share*cost)
					}
				}
			}
			sums := []float64{0}
			for _, dollars := range dollars {
				previous := append([]float64(nil), sums...)
				for _, x := range previous {
					sums = append(sums, x+dollars, x-dollars)
				}
			}
			for _, x := range sums {
				if math.Abs(x-7.77) < 1e-6 {
					t.Fatalf("published dollar combination recovers sec1's $7.77: %s", body)
				}
			}
		})
	}
}

func TestTeamLeversKAnon_BotsAndSpendWithoutMergedWorkDoNotCount(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	seedLeverGroup(t, db, "healthy", 6, time.Now().UTC())
	assertLeverControl(t, h)
	seedLeverGroup(t, db, "small", 2, time.Now().UTC())
	seedKAnonDevTyped(t, db, "small", "renovate[bot]", 1, 1, store.WorkTypeFeature, time.Now().UTC())
	enrolIn(t, db, "small", "spend-only")
	seedCostAt(t, db, "spend-only", "no-merge", 1, time.Now().UTC())
	_, body := leverBody(t, h)
	assertNoLevers(t, body)

	// Three cost carriers plus three different work carriers clear the existing
	// per-measure row floor, but nobody has BOTH. Even healthy's levers must go.
	h, db = newTeamModeHandler(t, 3)
	seedLeverGroup(t, db, "healthy", 6, time.Now().UTC())
	for i := 0; i < 3; i++ {
		spender, worker := fmt.Sprintf("spender-%d", i), fmt.Sprintf("worker-%d", i)
		enrolIn(t, db, "disjoint", spender, worker)
		seedCostAt(t, db, spender, "no-merge", 1, time.Now().UTC())
		seedOutcomeAt(t, db, worker, "no-spend", 1, 1, time.Now().UTC())
	}
	raw, body := leverBody(t, h)
	if rows, _ := raw["teams"].([]any); len(rows) != 2 {
		t.Fatalf("expected two published cost rows: %s", body)
	}
	assertNoLevers(t, body)
}

func TestTeamLeversKAnon_SealedOnlyAndDeveloperUnchanged(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	// The existing seal fixture has a withheld residual; add an independent k-safe month.
	at := sealFixtureMonth.AddDate(0, 1, 2)
	seedLeverGroup(t, db, "eng", 10, at)
	seedLeverGroup(t, db, "next", 10, at.AddDate(0, 1, 0))
	s.now = func() time.Time { return sealAug1.AddDate(0, 1, 0) }
	sealPass(t, s)
	rec := sealedGet(t, h, "/api/v1/scores?period=2026-06")
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"cache_read_share"`)) {
		t.Errorf("sealed month lacks levers: %d %s", rec.Code, rec.Body)
	}
	rec = sealedGet(t, h, "/api/v1/scores?since=2026-06-01")
	if rec.Code == http.StatusOK {
		t.Errorf("free window accepted: %s", rec.Body)
	}
	rec = compareGet(t, h, "?period_a=2026-06&period_b=2026-07")
	if rec.Code != http.StatusOK {
		t.Errorf("safe comparison status %d: %s", rec.Code, rec.Body)
	}
	assertNoLevers(t, rec.Body.Bytes())
	live, liveDB := newTestHandler(t)
	seedLeverGroup(t, liveDB, "live", 10, time.Now().UTC())
	live.SetAggregation(scoring.AggregationTeam, 5)
	_, liveBody := getScoresRaw(t, live, "?since="+scoresSince())
	assertNoLevers(t, []byte(liveBody))
	// Developer mode is also the synthetic demo's score path.
	h.SetAggregation(scoring.AggregationDeveloper, 3)
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-06-01&until=2026-07-01", nil)
	if code != http.StatusOK {
		t.Fatalf("developer status %d: %s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["cost_composition"] == nil || len(out["developers"].([]any)) != 10 {
		t.Errorf("developer/demo composition or rows changed: %s", body)
	}
	h.syntheticDemo = true
	_, demoBody := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-06-01&until=2026-07-01", nil)
	if !bytes.Equal(body, demoBody) {
		t.Errorf("synthetic demo changed the developer payload")
	}
	if _, ok := out["total"].(map[string]any)["cache_read_share"]; ok {
		t.Errorf("developer total gained team levers: %s", body)
	}
}

// leverTestData retains distinct canonical identities and per-person category
// inputs, so category tests cannot accidentally count models or event rows.
func leverTestData() scoresWindowData {
	return scoresWindowData{
		win:              windowScores{canon: func(id string) string { return id }},
		groups:           windowGroups{outcomeLabels: map[int64]string{}},
		census:           &kanonCensus{counted: map[string]bool{}},
		groupComposition: map[string][]store.DeveloperComposition{},
	}
}

func addLeverSeat(data *scoresWindowData, label, dev string, c store.ModelClassCost) {
	data.census.counted[dev] = true
	data.groups.rows = append(data.groups.rows, scoring.LabeledScore{Label: label, Score: scoring.DeveloperScore{
		Developer: dev, TotalCostUSD: store.MicroToDollars(c.CostMicro), CoveragePercent: 100, CapturedRealtimeUSD: store.MicroToDollars(c.CostMicro),
		WeightedPoints: 1, SampleN: 1,
	}})
	id := int64(len(data.win.outcomes) + 1)
	data.win.outcomes = append(data.win.outcomes, store.Outcome{ID: id, Developer: dev})
	data.groups.outcomeLabels[id] = label
	data.groupComposition[label] = append(data.groupComposition[label], store.DeveloperComposition{Developer: dev, ModelClassCost: c})
}

func TestTeamLeversKAnon_CacheBothSides(t *testing.T) {
	for _, with := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("with-%d", with), func(t *testing.T) {
			data := leverTestData()
			for i := 0; i < 6; i++ {
				c := store.ModelClassCost{CostMicro: 1_000_000, InputTok: 100}
				if i < with {
					c.CacheRead = 100
				}
				addLeverSeat(&data, "eng", fmt.Sprintf("e%d", i), c)
				data.groupComposition["eng"] = append(data.groupComposition["eng"], store.DeveloperComposition{Developer: fmt.Sprintf("e%d", i), ModelClassCost: c})
			}
			levers, reason := groupLevers(data, []scoring.TeamScore{{Team: "eng"}}, 3)
			if present := levers["eng"].CacheReadShare != nil; present != (with == 3) {
				t.Errorf("cache present=%t with %d/6", present, with)
			}
			if (reason == "category_seats_below_k") != (with != 3) {
				t.Errorf("withheld reason = %q, with %d/6", reason, with)
			}
		})
	}
}

func TestTeamLeversKAnon_ZeroInputCannotPadCachePartition(t *testing.T) {
	for _, readers := range []int{2, 3} {
		for _, cost := range []int64{0, 1_000_000} {
			t.Run(fmt.Sprintf("readers-%d/zero-input-cost-%d", readers, cost), func(t *testing.T) {
				data := leverTestData()
				for i := 0; i < readers+1; i++ {
					c := store.ModelClassCost{CostMicro: 1_000_000, InputTok: 100}
					if i < readers {
						c.CacheRead = 100
					}
					addLeverSeat(&data, "eng", fmt.Sprintf("e%d", i), c)
				}
				for i := 0; i < 5; i++ {
					addLeverSeat(&data, "eng", fmt.Sprintf("zero%d", i), store.ModelClassCost{CostMicro: cost, OutputTok: int64(i % 2)})
				}
				levers, reason := groupLevers(data, []scoring.TeamScore{{Team: "eng"}}, 3)
				if levers["eng"].CacheReadShare != nil || reason != "category_seats_below_k" {
					t.Fatalf("zero-input padding published cache: %+v, reason %q", levers, reason)
				}
			})
		}
	}
}

func TestTeamLeversKAnon_InputClassesAndDistinctContributors(t *testing.T) {
	data := leverTestData()
	for i, c := range []store.ModelClassCost{
		{CacheRead: 100}, {CacheRead: 100}, {CacheRead: 100},
		{InputTok: 100}, {CacheWrite5m: 100}, {CacheWrite1h: 100},
	} {
		c.CostMicro = 1_000_000
		dev := fmt.Sprintf("e%d", i)
		addLeverSeat(&data, "eng", dev, c)
		// Another model/event for the same contributor must not add a seat or
		// erase cache membership when this row has no input-side tokens.
		data.groupComposition["eng"] = append(data.groupComposition["eng"], store.DeveloperComposition{Developer: dev})
	}
	levers, reason := groupLevers(data, []scoring.TeamScore{{Team: "eng"}}, 3)
	if share := levers["eng"].CacheReadShare; share == nil || *share != 0.5 || reason != "" {
		t.Fatalf("input-side classes withheld: %+v, reason %q", levers, reason)
	}
}

func TestTeamLeversKAnon_FoldedOtherIncludesSmallTeamCost(t *testing.T) {
	data := leverTestData()
	for i := 0; i < 3; i++ { // one named team exactly at k
		addLeverSeat(&data, "named", fmt.Sprintf("n%d", i), store.ModelClassCost{CostMicro: 1_000_000, Model: "claude-sonnet-4", InputTok: 100})
	}
	// A below-k team contributes $8, all premium/unattributed, and 200 cached
	// tokens. Four one-person teams pad other to six people: another $2 and
	// 100 cache reads in those categories, plus $3 / 300 input tokens outside.
	for i := 0; i < 6; i++ {
		label := "small"
		if i >= 2 {
			label = fmt.Sprintf("padding-%d", i)
		}
		c := store.ModelClassCost{CostMicro: 1_000_000, Model: "claude-sonnet-4", InputTok: 100}
		if i < 3 {
			c.CostMicro, c.Model, c.CacheRead = 2_000_000, "claude-opus-4-8", 100
			if i < 2 {
				c.CostMicro = 4_000_000
			}
			c.UnattributedCostMicro = c.CostMicro
		}
		addLeverSeat(&data, label, fmt.Sprintf("s%d", i), c)
	}
	teams, sup := scoring.AggregateFolded(scoring.PreFold(data.groups.rows, data.census.scoring(), func(id string) string { return id }), 3)
	if sup.Any() || len(teams) != 2 || teams[1].Team != scoring.OtherCohort || teams[1].TotalCostUSD != 13 {
		t.Fatalf("control: named at k and row-safe other: %+v %+v", teams, sup)
	}
	levers, _ := groupLevers(data, teams, 3)
	other := levers[scoring.OtherCohort]
	if share := other.CacheReadShare; share == nil || math.Abs(*share-300.0/900) > 1e-12 {
		t.Errorf("other cache=%v, want %v", share, 300.0/900)
	}
}

func TestTeamLeversKAnon_ClampTwoToThree(t *testing.T) {
	data := leverTestData()
	for i := 0; i < 3; i++ {
		addLeverSeat(&data, "eng", fmt.Sprintf("e%d", i), store.ModelClassCost{CostMicro: 1_000_000, Model: "claude-sonnet-4"})
	}
	// All three people carry row cost and points; push-only work does not fill
	// the third lever seat. A requested k=2 still needs three lever seats.
	data.win.outcomes[2].Source = store.OutcomeSourcePush
	teams, sup := scoring.AggregateFolded(scoring.PreFold(data.groups.rows, data.census.scoring(), func(id string) string { return id }), 2)
	if sup.Any() || len(teams) != 1 {
		t.Fatalf("control: three row-people publish: %+v %+v", teams, sup)
	}
	levers, reason := groupLevers(data, teams, 2)
	if levers != nil || reason != "group_seats_below_k" {
		t.Fatalf("k=2 bypassed minimum 3: %+v %q", levers, reason)
	}
}

type leverCompositionReader struct {
	windowReader
	calls int
	err   error
}

func (r *leverCompositionReader) DeveloperCompositionWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DeveloperComposition, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.windowReader.DeveloperCompositionWindow(ctx, since, until, scope)
}

func TestTeamLevers_CompositionReadOnlyWhenEligibleAndErrorsLogged(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	seedLeverGroup(t, db, "healthy", 6, time.Now().UTC())
	since, err := time.Parse("2006-01-02", scoresSince())
	if err != nil {
		t.Fatal(err)
	}
	q := scoresQuery{since: since, until: time.Now().UTC().Add(time.Second), scope: store.FleetWide}
	for _, tc := range []struct {
		name           string
		folded, legacy bool
		want           int
	}{
		{"live", false, false, 0}, {"seal", true, false, 1}, {"legacy-recompute", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &leverCompositionReader{windowReader: db}
			query := q
			query.legacyLevers = tc.legacy
			if _, _, err := h.scoresForWindow(t.Context(), r, query, tc.folded); err != nil {
				t.Fatal(err)
			}
			if r.calls != tc.want {
				t.Fatalf("composition reads %d, want %d", r.calls, tc.want)
			}
		})
	}
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	failure := errors.New("composition unavailable")
	r := &leverCompositionReader{windowReader: db, err: failure}
	if _, _, err := h.scoresForWindow(t.Context(), r, q, true); !errors.Is(err, failure) {
		t.Fatalf("lost composition error: %v", err)
	}
	if !strings.Contains(logs.String(), "query group composition") || !strings.Contains(logs.String(), failure.Error()) {
		t.Fatalf("composition failure not logged: %s", &logs)
	}
	seedDisjointLeverGroup(t, db, "disjoint", 3, 2, time.Now().UTC())
	r.calls = 0
	if _, _, err := h.scoresForWindow(t.Context(), r, q, true); err != nil || r.calls != 0 {
		t.Fatalf("seat-unsafe response queried composition: calls=%d err=%v", r.calls, err)
	}
}
