//go:build event3replay

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/quality"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

const replayWindow = "since=2026-06-01&until=2026-09-01"
const replayToken = "synthetic-event3-replay"

type replayFixture struct {
	Name   string        `json:"name"`
	PRs    []replayPR    `json:"prs"`
	Stages []replayStage `json:"stages"`
	Total  string        `json:"total"`
}
type replayPR struct {
	Number   int    `json:"number"`
	Author   string `json:"author"`
	Weight   string `json:"weight"`
	Quality  string `json:"quality"`
	Parents  []int  `json:"parents"`
	Merged   string `json:"merged"`
	Team     string `json:"team"`
	Repo     string `json:"repo"`
	WorkType string `json:"work_type"`
	Source   string `json:"source"`
	Excluded bool   `json:"excluded"`
}
type replayAction struct {
	Kind   string `json:"kind"`
	PR     int    `json:"pr"`
	Parent int    `json:"parent"`
	Own    string `json:"own"`
	Before string `json:"before"`
	After  string `json:"after"`
}
type replayStage struct {
	Name    string         `json:"name"`
	Actions []replayAction `json:"actions"`
	Want    []replayWant   `json:"want"`
}
type replayWant struct {
	PR       int    `json:"pr"`
	Points   string `json:"points"`
	Quality  string `json:"quality"`
	Share    string `json:"share"`
	Growth   string `json:"growth"`
	Eligible bool   `json:"eligible"`
	State    string `json:"state"`
}

func replayFixtures(t *testing.T) []replayFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/event3/adjudicated.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	// Only whole-line // comments are accepted. Strings are never rewritten.
	var clean bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		if !strings.HasPrefix(strings.TrimSpace(scanner.Text()), "//") {
			clean.Write(scanner.Bytes())
			clean.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&clean)
	dec.DisallowUnknownFields()
	var fixtures []replayFixture
	if err := dec.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 16 {
		t.Fatalf("fixture set unexpectedly shrank: %d", len(fixtures))
	}
	names := map[string]bool{}
	for _, f := range fixtures {
		if f.Name == "" || names[f.Name] || len(f.Stages) == 0 {
			t.Fatalf("invalid fixture %q", f.Name)
		}
		names[f.Name] = true
		ids := map[int]bool{}
		for _, p := range f.PRs {
			if ids[p.Number] {
				t.Fatal("duplicate fixture PR")
			}
			ids[p.Number] = true
			replayRat(t, p.Weight)
		}
		for _, s := range f.Stages {
			for _, w := range s.Want {
				if !ids[w.PR] {
					t.Fatalf("unknown expected PR %d", w.PR)
				}
				for _, r := range []string{w.Points, w.Quality, w.Share, w.Growth} {
					if r != "" {
						replayRat(t, r)
					}
				}
			}
		}
		total := new(big.Rat)
		for _, w := range f.Stages[len(f.Stages)-1].Want {
			total.Add(total, replayRat(t, w.Points))
		}
		if total.Cmp(replayRat(t, f.Total)) != 0 {
			t.Fatalf("%s: hand total %s != sum of hand terms %s", f.Name, f.Total, total)
		}
	}
	sum := sha256.Sum256(b)
	t.Logf("fixture_set=u5-d1-d2-d3-v1 sha256=%x rules=R-2026-10-14-19(D1/D2/D3) cases=%d", sum, len(fixtures))
	return fixtures
}
func replayFind(t *testing.T, name string) replayFixture {
	t.Helper()
	for _, f := range replayFixtures(t) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("missing fixture %s", name)
	return replayFixture{}
}
func (f replayFixture) pr(n int) replayPR {
	for _, p := range f.PRs {
		if p.Number == n {
			return p
		}
	}
	panic(fmt.Sprintf("unknown fixture PR %d", n))
}
func (p replayPR) repository() string {
	if p.Repo != "" {
		return p.Repo
	}
	return "replay/alpha"
}
func (p replayPR) sha() string { return fmt.Sprintf("%040x", p.Number) }
func (p replayPR) mergeTime(t *testing.T) time.Time {
	if p.Merged != "" {
		return replayTime(t, p.Merged)
	}
	if len(p.Parents) == 0 {
		return replayTime(t, "2026-07-01T12:00:00Z")
	}
	return replayTime(t, "2026-07-02T12:00:00Z").Add(time.Duration(p.Number-102) * time.Hour)
}
func (p replayPR) outcome(t *testing.T) store.Outcome {
	merged := p.mergeTime(t)
	q := "1"
	if p.Quality != "" {
		q = p.Quality
	}
	weight, _ := replayRat(t, p.Weight).Float64()
	qualityValue, _ := replayRat(t, q).Float64()
	work := p.WorkType
	if work == "" {
		work = store.WorkTypeFeature
	}
	o := store.Outcome{Developer: p.Author, IssueID: fmt.Sprintf("replay-%d", p.Number), PRNumber: p.Number,
		MergeCommitSHA: p.sha(), MergedAt: &merged, Timestamp: merged, Weight: weight, Quality: qualityValue,
		WeightSource: store.WeightSourceLabel, Repo: p.repository(), WorkType: work, Source: p.Source}
	// Genuine zero diff evidence is available. Confirmed reviewed sizing is carried
	// separately and must never be replaced by the padded raw label or these counts.
	zero := 0
	o.SetDiffStats(&zero, &zero, &zero)
	return o
}

func replayDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "event3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}
func replayIngest(ctx context.Context, db *store.DB, t *testing.T, p replayPR) error {
	o := p.outcome(t)
	declaration := "Corrects: none"
	if len(p.Parents) > 0 {
		refs := []string{}
		for _, n := range p.Parents {
			refs = append(refs, fmt.Sprintf("#%d", n))
		}
		declaration = "Corrects: " + strings.Join(refs, ", ")
	}
	// Capture declaration before outcome attribution, just as the public capture contract requires.
	if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(declaration), declaration, "synthetic-replay"); err != nil {
		return err
	}
	if p.Excluded {
		return db.ExcludePROutcome(ctx, o, "synthetic recognized revert", "operator")
	}
	if _, err := db.RecordPROutcome(ctx, o); err != nil {
		return err
	}
	n := p.Number
	return db.InsertTokenEvent(ctx, store.TokenEvent{Developer: p.Author, IssueID: o.IssueID, PRNumber: &n,
		Model: "gpt-4o", InputTok: 10000, CostMicro: 2_000_000, Source: "jsonl", Fidelity: "realtime", Repo: p.repository(),
		Timestamp: o.Timestamp, IdempotencyKey: fmt.Sprintf("replay-cost-%d", p.Number)})
}
func replayLoad(t *testing.T, f replayFixture, order []int, concurrent bool) *store.DB {
	t.Helper()
	db := replayDB(t)
	ctx := context.Background()
	var hierarchy []store.HierarchyRow
	seen := map[string]bool{}
	for _, p := range f.PRs {
		if !seen[p.Author] {
			team := p.Team
			if team == "" {
				team = "before"
			}
			hierarchy = append(hierarchy, store.HierarchyRow{Developer: p.Author, Team: team, Division: "replay-division", Org: "replay-org"})
			seen[p.Author] = true
		}
	}
	if err := db.SeedDemoMembershipBaseline(ctx, hierarchy); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertHierarchies(ctx, hierarchy, "synthetic-replay"); err != nil {
		t.Fatal(err)
	}
	// Establish captured-source horizons before the reporting window, without
	// adding points, in-window costs, people, or bootstrap observations.
	repos := map[string]bool{}
	for _, p := range f.PRs {
		if !repos[p.repository()] {
			repos[p.repository()] = true
			if err := db.InsertTokenEvent(ctx, store.TokenEvent{Developer: p.Author, IssueID: "replay-horizon", Repo: p.repository(), Source: "jsonl", Fidelity: "realtime", Model: "gpt-4o", InputTok: 1, Timestamp: replayTime(t, "2026-05-31T00:00:00Z"), IdempotencyKey: "replay-horizon-" + p.repository()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if order == nil {
		for i := range f.PRs {
			order = append(order, i)
		}
	}
	if concurrent {
		var wg sync.WaitGroup
		errors := make(chan error, len(order))
		start := make(chan struct{})
		for _, i := range order {
			wg.Add(1)
			go func(p replayPR) { defer wg.Done(); <-start; errors <- replayIngest(ctx, db, t, p) }(f.PRs[i])
		}
		close(start)
		wg.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
	} else {
		for _, i := range order {
			if err := replayIngest(ctx, db, t, f.PRs[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Duplicate delivery must not double outcomes, edges, costs, or allocations.
	for _, i := range order {
		if err := replayIngest(ctx, db, t, f.PRs[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range f.PRs {
		if !p.Excluded {
			o := replayStored(t, db, p)
			if err := replayReconcile(ctx, db, o.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Match serve's explicit startup policy configuration. Subsequent U2 and
	// quality writes must refresh projections through U4's enabled policy.
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	return db
}
func replayStored(t *testing.T, db *store.DB, p replayPR) store.Outcome {
	t.Helper()
	o, ok, err := db.OutcomeByMergeCommit(context.Background(), p.sha())
	if err != nil || !ok {
		t.Fatalf("outcome %d: exists=%v err=%v", p.Number, ok, err)
	}
	return o
}
func replayApply(t *testing.T, db *store.DB, f replayFixture, s replayStage) {
	t.Helper()
	for _, a := range s.Actions {
		replayDisposition(t, db, f, a)
	}
}
func replayCheck(t *testing.T, db *store.DB, f replayFixture, s replayStage) {
	t.Helper()
	for _, w := range s.Want {
		o := replayStored(t, db, f.pr(w.PR))
		got := replayProjected(t, db, o)
		for _, v := range []struct{ name, got, want string }{{"points", got.Points, w.Points}, {"quality", got.Quality, w.Quality}, {"share", got.Share, w.Share}, {"growth", got.Growth, w.Growth}} {
			if v.want != "" && v.got != v.want {
				t.Errorf("%s/%s PR%d %s=%s want exact %s", f.Name, s.Name, w.PR, v.name, v.got, v.want)
			}
		}
		if w.Growth == "" && got.Growth != "" {
			t.Errorf("%s/%s PR%d growth=%s want unknown (persisted allocation)", f.Name, s.Name, w.PR, got.Growth)
		}
		if got.Eligible != w.Eligible {
			t.Errorf("%s/%s PR%d eligibility=%v want %v", f.Name, s.Name, w.PR, got.Eligible, w.Eligible)
		}
		if w.State != "" {
			// U4 separates lifecycle state (e.g. declared) from projection
			// status (e.g. pending multi-root sizing). Compare the public
			// diagnostic status to the fixture's adjudicated scoring state.
			var diagnostics store.CorrectionDiagnostics
			err := db.ReadSnapshot(context.Background(), func(snapshot *store.Snapshot) error {
				var err error
				diagnostics, err = snapshot.CorrectionDiagnosticsWindow(context.Background(), replayTime(t, "2026-06-01T00:00:00Z"), replayTime(t, "2026-09-01T00:00:00Z"), replayTime(t, "2026-09-01T00:00:00Z"), store.FleetWide)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range diagnostics.Relations {
				if e.ChildPR == int64(w.PR) {
					found = true
					if e.Status != w.State {
						t.Errorf("%s/%s PR%d diagnostic status=%s (lifecycle=%s) want %s", f.Name, s.Name, w.PR, e.Status, e.State, w.State)
					}
				}
			}
			if !found {
				t.Errorf("%s/%s PR%d missing relation diagnostics", f.Name, s.Name, w.PR)
			}
		}
	}
	rows, err := db.AllOutcomesWindow(context.Background(), replayTime(t, "2026-06-01T00:00:00Z"), replayTime(t, "2026-09-01T00:00:00Z"), store.FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(s.Want) {
		t.Errorf("raw outcomes=%d want %d (multi-parent/replay duplication)", len(rows), len(s.Want))
	}
}

func TestEvent3Replay_AdjudicatedFixtures(t *testing.T) {
	for _, f := range replayFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			db := replayLoad(t, f, nil, false)
			for _, s := range f.Stages {
				replayApply(t, db, f, s)
				replayCheck(t, db, f, s)
				replayReaders(t, db, f, s, replayWindow, "")
			}
		})
	}
}
func TestEvent3Replay_SameAndCrossAuthorTrustBoundaries(t *testing.T) {
	for _, name := range []string{"cross-author-trust-and-lifecycle", "same-author-provisional-and-withdrawal", "mixed-own-and-foreign-roots-pending", "least-favourable-own-root"} {
		f := replayFind(t, name)
		t.Run(name, func(t *testing.T) {
			db := replayLoad(t, f, nil, false)
			for _, s := range f.Stages {
				replayApply(t, db, f, s)
				replayCheck(t, db, f, s)
			}
		})
	}
}
func TestEvent3Replay_AuthorPermutationPreservesConfirmedTreatment(t *testing.T) {
	f := replayFind(t, "degraded-terminal-own-points")
	for _, authors := range [][]string{{"ada", "ben"}, {"ben", "ada"}, {"cy", "cy"}, {"di", "eli"}} {
		t.Run(strings.Join(authors, "-"), func(t *testing.T) {
			f.PRs = slices.Clone(f.PRs)
			f.PRs[0].Author = authors[0]
			f.PRs[1].Author = authors[1]
			db := replayLoad(t, f, nil, false)
			s := f.Stages[0]
			replayApply(t, db, f, s)
			replayCheck(t, db, f, s)
		})
	}
}
func TestEvent3Replay_ChildFirstConcurrentAndHistoricalInsertion(t *testing.T) {
	for _, name := range []string{"reverse-canonical-floor-exhaustion", "repair-of-repair-and-multi-parent-disposition", "older-than-sixty-days-and-sealed-month"} {
		f := replayFind(t, name)
		for _, mode := range []string{"forward", "child-first", "shuffled", "concurrent"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				order := []int{}
				for i := range f.PRs {
					order = append(order, i)
				}
				if mode == "child-first" {
					slices.Reverse(order)
				}
				if mode == "shuffled" {
					rand.New(rand.NewPCG(1235, 5)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				}
				db := replayLoad(t, f, order, mode == "concurrent")
				for _, s := range f.Stages {
					replayApply(t, db, f, s)
					replayCheck(t, db, f, s)
				}
			})
		}
	}
}
func TestEvent3Replay_MultipleParentsNeverDuplicateChildCredit(t *testing.T) {
	for _, name := range []string{"repair-of-repair-and-multi-parent-disposition", "distinct-foreign-roots-pending", "mixed-own-and-foreign-roots-pending"} {
		f := replayFind(t, name)
		t.Run(name, func(t *testing.T) {
			db := replayLoad(t, f, nil, false)
			for _, s := range f.Stages {
				replayApply(t, db, f, s)
				replayCheck(t, db, f, s)
			}
		})
	}
}

func replayHTTP(t *testing.T, db *store.DB, enabled bool, mode scoring.AggregationMode) http.Handler {
	t.Helper()
	state, err := db.CorrectionScoringPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A reader must not repair a stale lifecycle projection. Only an explicit
	// flag transition uses the same policy flip as serve startup.
	if state.Enabled != enabled {
		if err := db.ConfigureCorrectionScoring(context.Background(), enabled); err != nil {
			t.Fatal(err)
		}
	}
	opts := []api.Option{api.WithStoredScoringPolicy(), api.WithUnsealedRecompute()}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), replayToken, nil, "event3-replay", api.RateLimitConfig{}, opts...)
	h.SetAggregation(mode, scoring.MinKAnonymity)
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}
func replayGet(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+replayToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

type replayScore struct {
	Developer string  `json:"developer"`
	Points    float64 `json:"weighted_points"`
	Cost      float64 `json:"total_cost_usd"`
	Sample    int     `json:"sample_n"`
	OutcomeN  *int    `json:"outcome_n"`
	Ranked    bool    `json:"ranked"`
	CILow     float64 `json:"ci_low"`
	CIHigh    float64 `json:"ci_high"`
}

func replayNear(t *testing.T, label string, got float64, want *big.Rat) {
	t.Helper()
	v, _ := want.Float64()
	if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-v) > 1e-10 {
		t.Errorf("%s=%g want %s", label, got, want)
	}
}
func replayReaders(t *testing.T, db *store.DB, f replayFixture, s replayStage, window, repo string) {
	t.Helper()
	h := replayHTTP(t, db, true, scoring.AggregationDeveloper)
	type totals struct {
		points      *big.Rat
		cost        int
		sample, raw int
	}
	byDev := map[string]*totals{}
	lower, _ := url.ParseQuery(window)
	for _, w := range s.Want {
		p := f.pr(w.PR)
		day := p.mergeTime(t).Format("2006-01-02")
		if day < lower.Get("since") || day >= lower.Get("until") || (repo != "" && p.repository() != repo) {
			continue
		}
		v := byDev[p.Author]
		if v == nil {
			v = &totals{points: new(big.Rat)}
			byDev[p.Author] = v
		}
		v.points.Add(v.points, replayRat(t, w.Points))
		v.cost += 2
		v.raw++
		if w.Eligible {
			v.sample++
		}
	}
	query := window
	if repo != "" {
		query += "&repo=" + url.QueryEscape(repo)
	}
	var headline struct {
		Developers []replayScore `json:"developers"`
		Rubric     struct {
			Version int `json:"version"`
		} `json:"rubric"`
		Policy struct {
			Name string `json:"scoring_policy"`
			Rule string `json:"correction_rule_version"`
		} `json:"correction_policy"`
	}
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores?"+query), &headline); err != nil {
		t.Fatal(err)
	}
	if headline.Rubric.Version != 2 || headline.Policy.Name != "event3" || headline.Policy.Rule != "event3-v2" {
		t.Errorf("%s/%s /scores policy=%+v rubric=%d want Event 3/rubric 2", f.Name, s.Name, headline.Policy, headline.Rubric.Version)
	}
	if len(headline.Developers) != len(byDev) {
		t.Fatalf("headline developers=%d want %d", len(headline.Developers), len(byDev))
	}
	for _, row := range headline.Developers {
		want := byDev[row.Developer]
		if want == nil {
			t.Fatalf("unexpected developer %s", row.Developer)
		}
		replayNear(t, "headline points", row.Points, want.points)
		if row.Cost != float64(want.cost) || row.Sample != want.sample || row.OutcomeN == nil || *row.OutcomeN != want.raw {
			t.Errorf("%s cost/sample/raw=%g/%d/%v want %d/%d/%d", row.Developer, row.Cost, row.Sample, row.OutcomeN, want.cost, want.sample, want.raw)
		}
		if row.Sample < 3 && (row.Ranked || row.CILow != 0 || row.CIHigh != 0) {
			t.Error("repairs unlocked ranking/bootstrap floor")
		}
		var detailRow replayScore
		if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores/"+row.Developer+"?"+query), &detailRow); err != nil {
			t.Fatal(err)
		}
		replayNear(t, "detail points", detailRow.Points, want.points)
		if detailRow.Sample != want.sample {
			t.Error("detail SampleN drift")
		}
	}
	// Compare currently supports fleet windows only.
	if repo == "" {
		compareQuery := "since_a=" + lower.Get("since") + "&until_a=" + lower.Get("until") + "&since_b=" + lower.Get("since") + "&until_b=" + lower.Get("until")
		var compare struct {
			Developers []struct {
				Developer string `json:"developer"`
				A, B      replayScore
			} `json:"developers"`
		}
		if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores/compare?"+compareQuery), &compare); err != nil {
			t.Fatal(err)
		}
		if len(compare.Developers) != len(byDev) {
			t.Error("compare dropped activity")
		}
		for _, row := range compare.Developers {
			w := byDev[row.Developer]
			if w == nil {
				t.Fatal("unexpected compare author")
			}
			for _, side := range []replayScore{row.A, row.B} {
				replayNear(t, "compare points", side.Points, w.points)
				if side.Sample != w.sample {
					t.Error("compare SampleN drift")
				}
			}
		}
	}
	// Bulk export is fleet-wide; scoped placement is checked above.
	if repo != "" {
		return
	}
	// Bulk-export projection is checked independently against exact store projections below.
	export := replayGet(t, h, "/api/v1/outcomes?"+query)
	var body struct {
		Outcomes []struct {
			PR     int      `json:"pr_number"`
			Points *float64 `json:"allocated_points"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(export, &body); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, w := range s.Want {
		p := f.pr(w.PR)
		day := p.mergeTime(t).Format("2006-01-02")
		if day >= lower.Get("since") && day < lower.Get("until") && (repo == "" || p.repository() == repo) {
			count++
		}
	}
	if len(body.Outcomes) != count {
		t.Errorf("export rows=%d want %d", len(body.Outcomes), count)
	}
	for _, row := range body.Outcomes {
		o := replayStored(t, db, f.pr(row.PR))
		if row.Points == nil {
			t.Errorf("export PR%d missing allocated_points", row.PR)
		} else {
			replayNear(t, fmt.Sprintf("export PR%d allocated points", row.PR), *row.Points, replayRat(t, replayProjected(t, db, o).Points))
		}
	}
}
func TestEvent3Replay_GrowthSampleFloorAndAllReadersAgree(t *testing.T) {
	for _, name := range []string{"degraded-terminal-own-points", "reviewed-growth-not-raw-padding", "repair-of-repair-and-multi-parent-disposition"} {
		f := replayFind(t, name)
		t.Run(name, func(t *testing.T) {
			db := replayLoad(t, f, nil, false)
			s := f.Stages[len(f.Stages)-1]
			replayApply(t, db, f, s)
			replayCheck(t, db, f, s)
			replayReaders(t, db, f, s, replayWindow, "")
		})
	}
}

func TestEvent3Replay_CostRepoWindowAndHistoricalTeamStayPlaced(t *testing.T) {
	f := replayFind(t, "older-than-sixty-days-and-sealed-month")
	// The adjudicated teams each have two contributors. Keep their arithmetic
	// unchanged and assert the ruled k=3 folding instead of padding costs.
	s := f.Stages[0]
	db := replayLoad(t, f, nil, false)
	replayApply(t, db, f, s)
	replayCheck(t, db, f, s)
	// Change current membership AFTER historical events. Past work must remain before/after.
	if err := db.UpsertHierarchy(context.Background(), "ada", "now", "replay-division", "replay-org", "operator"); err != nil {
		t.Fatal(err)
	}
	replayReaders(t, db, f, s, "since=2026-06-01&until=2026-07-01", "replay/alpha")
	replayReaders(t, db, f, s, "since=2026-08-01&until=2026-09-01", "replay/alpha")
	replayReaders(t, db, f, s, "since=2026-08-01&until=2026-09-01", "replay/beta")
	// Historical placement at store boundary, used by API's public team reader.
	memberships, err := db.HierarchyMembership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) < 3 {
		t.Error("historical memberships were replaced by current mapping")
	}
	// Neither historical team clears MinKAnonymity (#185). Their combined four
	// contributors clear the floor, so only the residual may be published. A
	// zero-cost third contributor would not clear the per-measure cost floor.
	team := replayHTTP(t, db, true, scoring.AggregationTeam)
	raw := replayGet(t, team, "/api/v1/scores?"+replayWindow)
	if bytes.Contains(raw, []byte(`"team":"now"`)) {
		t.Error("current team received historical points/cost")
	}
	var named struct {
		Developers []json.RawMessage `json:"developers"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		t.Fatal(err)
	}
	if len(named.Developers) != 0 {
		t.Error("team reader exposed developers")
	}
	var groups struct {
		Teams []struct {
			Team   string  `json:"team"`
			Points float64 `json:"weighted_points"`
			Cost   float64 `json:"total_cost_usd"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups.Teams) != 1 {
		t.Errorf("%s: /scores aggregation=team teams=%+v want only other(points=10,cost=10) at k=%d", f.Name, groups.Teams, scoring.MinKAnonymity)
	}
	for _, group := range groups.Teams {
		if group.Team != scoring.OtherCohort {
			t.Errorf("sub-k historical team exposed: %s", group.Team)
		}
		replayNear(t, "folded historical team points", group.Points, replayRat(t, "10"))
		if group.Cost != 10 {
			t.Errorf("folded historical team cost=%g want 10", group.Cost)
		}
	}
	expected := map[string]struct {
		points string
		cost   float64
	}{"before": {"8", 6}, "after": {"2", 4}}
	var developer struct {
		Teams []struct {
			Team   string  `json:"team"`
			Points float64 `json:"weighted_points"`
			Cost   float64 `json:"total_cost_usd"`
		} `json:"team_rollups"`
	}
	if err := json.Unmarshal(replayGet(t, replayHTTP(t, db, true, scoring.AggregationDeveloper), "/api/v1/scores?"+replayWindow), &developer); err != nil {
		t.Fatal(err)
	}
	if len(developer.Teams) != len(expected) {
		t.Errorf("named historical team rollups=%+v want before/after", developer.Teams)
	}
	for _, group := range developer.Teams {
		w, ok := expected[group.Team]
		if !ok {
			t.Errorf("unexpected named historical team %s", group.Team)
			continue
		}
		replayNear(t, "named historical team "+group.Team, group.Points, replayRat(t, w.points))
		if group.Cost != w.cost {
			t.Errorf("named historical team %s cost=%g want %g", group.Team, group.Cost, w.cost)
		}
	}
}

func TestEvent3Replay_QualityOrderingsAndSubchainIsolation(t *testing.T) {
	f := replayFind(t, "degraded-terminal-own-points")
	for _, order := range [][]string{{"ci_fail", "ci_fail_flaky", "revert_strategic"}, {"revert_strategic", "ci_fail_flaky", "ci_fail"}, {"ci_fail_flaky", "revert_strategic", "ci_fail"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			db := replayLoad(t, f, nil, false)
			replayApply(t, db, f, f.Stages[0])
			root := replayProjected(t, db, replayStored(t, db, f.pr(101)))
			p := f.pr(102)
			o := replayStored(t, db, p)
			for _, kind := range order {
				_, err := db.AppendQualityEvent(context.Background(), store.QualityEvent{OutcomeID: o.ID, Developer: p.Author, IssueID: o.IssueID, EventType: kind, SourceRef: quality.BuildCISourceRef(p.sha(), 1, 7), EventTS: p.mergeTime(t).Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				events, err := db.QualityEventsForOutcome(context.Background(), o.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateQualityForOutcome(context.Background(), o.ID, quality.Resolve(events), kind, "synthetic evidence"); err != nil {
					t.Fatal(err)
				}
				if got := replayProjected(t, db, replayStored(t, db, f.pr(101))); got != root {
					t.Errorf("terminal quality event changed root: %+v != %+v", got, root)
				}
			}
			got := replayProjected(t, db, replayStored(t, db, p))
			if got.Points != "4/5" || got.Quality != "4/5" {
				t.Errorf("flaky+strategic terminal=%+v want points=quality=4/5", got)
			}
		})
	}
	// A nonterminal follow-up's quality changes only itself and its descendants.
	sub := replayFind(t, "repair-of-repair-and-multi-parent-disposition")
	db := replayLoad(t, sub, nil, false)
	replayApply(t, db, sub, sub.Stages[0])
	root := replayProjected(t, db, replayStored(t, db, sub.pr(101)))
	p := replayStored(t, db, sub.pr(102))
	if err := db.UpdateQualityForOutcome(context.Background(), p.ID, .7, "ci_fail", "synthetic subchain failure"); err != nil {
		t.Fatal(err)
	}
	// 11/20*parent-share1, floor1/10 => parent1/10, child9/20. Root13/4 unchanged.
	if got := replayProjected(t, db, replayStored(t, db, sub.pr(101))); got != root {
		t.Error("descendant CI update recharged ancestor")
	}
	if got := replayProjected(t, db, replayStored(t, db, sub.pr(102))); got.Points != "1/10" || got.Quality != "11/20" {
		t.Errorf("subparent=%+v", got)
	}
	if got := replayProjected(t, db, replayStored(t, db, sub.pr(103))); got.Points != "9/20" {
		t.Errorf("subchild=%+v want9/20", got)
	}
}

func TestEvent3Replay_EraseEachPartyAndPreserveSurvivor(t *testing.T) {
	f := replayFind(t, "repair-of-repair-and-multi-parent-disposition")
	for _, erased := range f.PRs {
		t.Run(erased.Author, func(t *testing.T) {
			db := replayLoad(t, f, nil, false)
			replayApply(t, db, f, f.Stages[0])
			before := map[int]replayProjection{}
			for _, p := range f.PRs {
				if p.Author != erased.Author {
					before[p.Number] = replayProjected(t, db, replayStored(t, db, p))
				}
			}
			if _, err := db.EraseDeveloper(context.Background(), erased.Author); err != nil {
				t.Fatal(err)
			}
			for _, p := range f.PRs {
				if p.Author != erased.Author {
					o := replayStored(t, db, p)
					if err := replayReconcile(context.Background(), db, o.ID); err != nil && !errors.Is(err, store.ErrCorrectionFrozen) {
						t.Fatal(err)
					}
					if got := replayProjected(t, db, replayStored(t, db, p)); got != before[p.Number] {
						t.Errorf("survivor PR%d changed after erase+reconcile: %+v != %+v", p.Number, got, before[p.Number])
					}
					exported, err := db.ExportDeveloper(context.Background(), p.Author)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(exported)
					if err != nil {
						t.Fatal(err)
					}
					for _, s := range []string{`"` + erased.Author + `"`, erased.sha(), fmt.Sprintf("%s#%d", erased.repository(), erased.Number)} {
						if bytes.Contains(raw, []byte(s)) {
							t.Errorf("survivor DSAR leaked erased identity %q", s)
						}
					}
				}
			}
			subject, err := db.ExportDeveloper(context.Background(), erased.Author)
			if err != nil {
				t.Fatal(err)
			}
			if subject.RowCount() != 0 {
				t.Error("erased subject remains exportable")
			}
		})
	}
}

func replaySeal(t *testing.T, db *store.DB, body []byte) store.SealedReport {
	t.Helper()
	ctx := context.Background()
	epoch, err := db.EraseEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	start := replayTime(t, "2026-06-01T00:00:00Z")
	policy, err := db.CorrectionScoringPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := "legacy"
	if policy.Enabled {
		name = "event3"
	}
	sealed, won, err := db.SealReport(ctx, store.SealedReport{ScoringPolicy: &name, CorrectionRuleVersion: &policy.RuleVersion, RubricVersion: &policy.RubricVersion, CorrectionRevision: &policy.Revision, Level: "team", PeriodSize: "month", PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), K: scoring.MinKAnonymity, ConfigDigest: "synthetic-event3-policy", Body: body, BodyDigest: hex.EncodeToString(sum[:]), ToolVersion: "event3-replay", ToolCommit: "local-review"}, nil, nil, store.SealCheck{EraseEpoch: epoch, Floor: start, First: true})
	if err != nil || !won {
		t.Fatalf("seal June: won=%v err=%v", won, err)
	}
	return sealed
}
func replayAssertSeal(t *testing.T, db *store.DB, before store.SealedReport) {
	t.Helper()
	after, err := db.SealedReport(context.Background(), "month", before.PeriodStart)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.Body, before.Body) || after.BodyDigest != before.BodyDigest {
		t.Error("late correction/lifecycle/erasure rewrote sealed bytes or digest")
	}
}

// Pin PR102 -> PR101 and its boolean value: the JSON key alone is not evidence.
func replayAssertDisputedReaders(t *testing.T, h http.Handler, state string) {
	t.Helper()
	var diagnostics store.CorrectionDiagnostics
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/correction_diagnostics?"+replayWindow), &diagnostics); err != nil {
		t.Fatal(err)
	}
	var relationID int64
	for _, relation := range diagnostics.Relations {
		if relation.ChildPR == 102 && relation.ParentPR == 101 {
			relationID = relation.RelationID
			if relation.State != state || !relation.Disputed {
				t.Errorf("PR102 diagnostics state=%s disputed=%v want %s/true", relation.State, relation.Disputed, state)
			}
		}
	}
	if relationID == 0 {
		t.Fatal("PR102 -> PR101 diagnostic relation absent")
	}
	var headline struct {
		Corrections store.CorrectionDisputes `json:"correction_diagnostics"`
	}
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores?"+replayWindow), &headline); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, relation := range headline.Corrections.Relations {
		if relation.RelationID == relationID {
			found = true
			if relation.State != state || !relation.Disputed {
				t.Errorf("PR102 headline state=%s disputed=%v want %s/true", relation.State, relation.Disputed, state)
			}
		}
	}
	if !found {
		t.Error("PR102 headline dispute relation absent")
	}
	var detail struct {
		Issues []struct {
			PR         int                      `json:"pr_number"`
			Projection *store.OutcomeProjection `json:"projection"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores/ben?"+replayWindow), &detail); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, issue := range detail.Issues {
		if issue.PR == 102 {
			found = true
			if issue.Projection == nil || !issue.Projection.Disputed {
				t.Errorf("PR102 detail projection=%+v want disputed=true after %s", issue.Projection, state)
			}
		}
	}
	if !found {
		t.Error("PR102 detail issue absent")
	}
	var export struct {
		Outcomes []struct {
			PR       int  `json:"pr_number"`
			Disputed bool `json:"disputed"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/outcomes?"+replayWindow), &export); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, outcome := range export.Outcomes {
		if outcome.PR == 102 {
			found = true
			if !outcome.Disputed {
				t.Errorf("PR102 export disputed=false want true after %s", state)
			}
		}
	}
	if !found {
		t.Error("PR102 export outcome absent")
	}
}

func TestEvent3Replay_SealedBytesAndProtectedDiagnostics(t *testing.T) {
	f := replayFind(t, "older-than-sixty-days-and-sealed-month")
	db := replayLoad(t, f, nil, false)
	// Store an actual API-produced June body before late adjudication. Store API
	// preserves these exact bytes; building policy-aware seals is U4's own gate.
	body := replayGet(t, replayHTTP(t, db, true, scoring.AggregationTeam), "/api/v1/scores?since=2026-06-01&until=2026-07-01")
	sealed := replaySeal(t, db, body)
	replayApply(t, db, f, f.Stages[0])
	replayCheck(t, db, f, f.Stages[0])
	replayAssertSeal(t, db, sealed)
	h := replayHTTP(t, db, true, scoring.AggregationDeveloper)
	before := replayManifestCorrectionsDigest(t, replayGet(t, h, "/api/v1/report_manifest?"+replayWindow))
	replayDisposition(t, db, f, replayAction{Kind: "dispute", PR: 102, Parent: 101})
	after := replayManifestCorrectionsDigest(t, replayGet(t, h, "/api/v1/report_manifest?"+replayWindow))
	t.Run(f.Name+"/corrections-digest", func(t *testing.T) {
		// U3's corrections_digest pins dispute evidence; tierdig1
		// events/outcomes digests may legitimately remain unchanged.
		if before == after {
			t.Errorf("%s: /report_manifest dispute must change corrections_digest; before=%s after=%s", f.Name, before, after)
		}
	})
	replayAssertDisputedReaders(t, h, "disputed")
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		raw := replayGet(t, replayHTTP(t, db, true, mode), "/api/v1/scores?"+replayWindow)
		// not_shown is a required value; its exact placement is an adapter seam.
		if !bytes.Contains(raw, []byte("not_shown")) {
			t.Error("protected diagnostics must disclose not_shown")
		}
		for _, secret := range []string{`"ada"`, `"ben"`, `"disputed_at"`, `"dispute_revision"`, f.pr(101).sha(), f.pr(102).sha()} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("protected diagnostics leaked %s", secret)
			}
		}
	}
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		for _, path := range []string{"/api/v1/correction_diagnostics", "/api/v1/repair_spend"} {
			raw := replayGet(t, replayHTTP(t, db, true, mode), path+"?"+replayWindow)
			if strings.TrimSpace(string(raw)) != `{"status":"not_shown"}` {
				t.Errorf("%s %s diagnostic suppression=%s", mode, path, raw)
			}
		}
	}
	replayAssertSeal(t, db, sealed)
	replayDisposition(t, db, f, replayAction{Kind: "withdraw", PR: 102, Parent: 101})
	replayAssertDisputedReaders(t, h, "withdrawn-confirmed")
	replayAssertSeal(t, db, sealed)
	if _, err := db.EraseDeveloper(context.Background(), "ada"); err != nil {
		t.Fatal(err)
	}
	replayAssertSeal(t, db, sealed)
}

func TestEvent3Replay_FlagOffRetainsLegacyValues(t *testing.T) {
	f := replayFind(t, "degraded-terminal-own-points")
	db := replayLoad(t, f, nil, false)
	off := replayHTTP(t, db, false, scoring.AggregationDeveloper)
	path := "/api/v1/scores?" + replayWindow
	before := replayGet(t, off, path)
	replayApply(t, db, f, f.Stages[0])
	after := replayGet(t, off, path)
	if !bytes.Equal(before, after) {
		t.Errorf("flag-off legacy body changed after confirmation: before=%s after=%s", before, after)
	}
	replayCheck(t, db, f, f.Stages[0])
	replayReaders(t, db, f, f.Stages[0], replayWindow, "")
	restored := replayGet(t, replayHTTP(t, db, false, scoring.AggregationDeveloper), path)
	if !bytes.Equal(before, restored) {
		t.Error("explicit ON then OFF did not restore byte-identical legacy scores")
	}
	var response struct {
		Developers []replayScore `json:"developers"`
		Rubric     struct {
			Version int `json:"version"`
		} `json:"rubric"`
	}
	if err := json.Unmarshal(after, &response); err != nil {
		t.Fatal(err)
	}
	if response.Rubric.Version != 1 {
		t.Error("flag-off rubric must remain1")
	}
	want := map[string]string{"ada": "5", "ben": "7/10"}
	if len(response.Developers) != 2 {
		t.Fatal("legacy reader dropped an author")
	}
	for _, row := range response.Developers {
		replayNear(t, "legacy points", row.Points, replayRat(t, want[row.Developer]))
		if row.Sample != 1 {
			t.Error("flag-off samples changed")
		}
	}
}

func TestEvent3Replay_BootstrapKeepsFixedRepairPointsAndCost(t *testing.T) {
	f := replayFind(t, "bootstrap-fixed-points")
	db := replayLoad(t, f, nil, false)
	s := f.Stages[0]
	replayApply(t, db, f, s)
	replayCheck(t, db, f, s)
	replayReaders(t, db, f, s, replayWindow, "")
	var response struct {
		Developers []replayScore `json:"developers"`
	}
	if err := json.Unmarshal(replayGet(t, replayHTTP(t, db, true, scoring.AggregationDeveloper), "/api/v1/scores?"+replayWindow), &response); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range response.Developers {
		if row.Developer == "ben" {
			found = true
			if !row.Ranked || row.Sample != 3 || row.OutcomeN == nil || *row.OutcomeN != 4 {
				t.Errorf("bootstrap ranking/sample/raw=%+v", row)
			}
			replayNear(t, "bootstrap low", row.CILow, replayRat(t, "500"))
			replayNear(t, "bootstrap high", row.CIHigh, replayRat(t, "500"))
		}
	}
	if !found {
		t.Error("bootstrap developer disappeared")
	}
}

func TestEvent3Replay_SegmentsAndFoldedRollupsUseAllocatedPoints(t *testing.T) {
	f := replayFind(t, "degraded-terminal-own-points")
	f.PRs[1].WorkType = store.WorkTypeBug
	db := replayLoad(t, f, nil, false)
	s := f.Stages[0]
	replayApply(t, db, f, s)
	type totals struct {
		Points float64 `json:"weighted_points"`
		Cost   float64 `json:"total_cost_usd"`
	}
	var body struct {
		Total     *totals `json:"total"`
		WorkTypes []struct {
			Type       string        `json:"work_type"`
			Total      *totals       `json:"total"`
			Developers []replayScore `json:"developers"`
		} `json:"work_types"`
	}
	h := replayHTTP(t, db, true, scoring.AggregationDeveloper)
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores?"+replayWindow), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total == nil {
		t.Fatal("company rollup absent")
	}
	replayNear(t, "company points", body.Total.Points, replayRat(t, "79/20"))
	if body.Total.Cost != 4 {
		t.Error("company transferred or duplicated repair cost")
	}
	expected := map[string]string{store.WorkTypeFeature: "13/4", store.WorkTypeBug: "7/10"}
	if len(body.WorkTypes) != 2 {
		t.Fatal("missing work-type segment")
	}
	for _, segment := range body.WorkTypes {
		w, ok := expected[segment.Type]
		if !ok || segment.Total == nil {
			t.Fatal("unexpected work-type segment")
		}
		replayNear(t, "segment points", segment.Total.Points, replayRat(t, w))
		if segment.Total.Cost != 2 {
			t.Error("segment moved repair cost")
		}
		if len(segment.Developers) != 1 {
			t.Fatal("segment author changed")
		}
		if segment.Type == store.WorkTypeBug && segment.Developers[0].Sample != 0 {
			t.Error("repair segment gained a sample")
		}
	}
	query := "since_a=2026-06-01&until_a=2026-09-01&since_b=2026-06-01&until_b=2026-09-01"
	var compare struct {
		Total *struct{ A, B totals } `json:"total"`
	}
	if err := json.Unmarshal(replayGet(t, h, "/api/v1/scores/compare?"+query), &compare); err != nil {
		t.Fatal(err)
	}
	if compare.Total == nil {
		t.Fatal("developer compare total absent")
	}
	for i, side := range []totals{compare.Total.A, compare.Total.B} {
		replayNear(t, fmt.Sprintf("developer compare %s points", []string{"A", "B"}[i]), side.Points, replayRat(t, "79/20"))
		if side.Cost != 4 {
			t.Errorf("developer compare side %d cost=%g want 4", i, side.Cost)
		}
	}
	// Both published measures still have only two contributors. Neutral zero-cost
	// work cannot clear the cost floor without changing the adjudicated $4.
	// Assert suppression; the developer/segment checks above retain 79/20 and $4.
	type suppression struct {
		Developers    int  `json:"developers"`
		K             int  `json:"k_anonymity"`
		WithheldTotal bool `json:"withheld_total"`
		WithheldTeams bool `json:"withheld_teams"`
	}
	checkSuppressed := func(path string, total json.RawMessage, teams, developers []json.RawMessage, sup *suppression) {
		t.Helper()
		if len(total) != 0 && string(total) != "null" {
			t.Errorf("%s: %s exposed sub-k total: %s", f.Name, path, total)
		}
		if len(teams) != 0 || len(developers) != 0 {
			t.Errorf("%s: %s exposed sub-k rows: teams=%s developers=%s", f.Name, path, teams, developers)
		}
		if sup == nil || sup.Developers != 2 || sup.K != scoring.MinKAnonymity || !sup.WithheldTotal || !sup.WithheldTeams {
			t.Errorf("%s: %s suppression=%+v want two contributors withheld at k=%d", f.Name, path, sup, scoring.MinKAnonymity)
		}
	}
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		anon := replayHTTP(t, db, true, mode)
		var rollup struct {
			Total       json.RawMessage   `json:"total"`
			Teams       []json.RawMessage `json:"teams"`
			Developers  []json.RawMessage `json:"developers"`
			DataQuality struct {
				Suppressed *suppression `json:"kanon_suppressed"`
			} `json:"data_quality"`
		}
		if err := json.Unmarshal(replayGet(t, anon, "/api/v1/scores?"+replayWindow), &rollup); err != nil {
			t.Fatal(err)
		}
		checkSuppressed(fmt.Sprintf("/scores aggregation=%s", mode), rollup.Total, rollup.Teams, rollup.Developers, rollup.DataQuality.Suppressed)
		var compare struct {
			Total      json.RawMessage   `json:"total"`
			Teams      []json.RawMessage `json:"teams"`
			Developers []json.RawMessage `json:"developers"`
			Suppressed *suppression      `json:"kanon_suppressed"`
		}
		if err := json.Unmarshal(replayGet(t, anon, "/api/v1/scores/compare?"+query), &compare); err != nil {
			t.Fatal(err)
		}
		checkSuppressed(fmt.Sprintf("/scores/compare aggregation=%s", mode), compare.Total, compare.Teams, compare.Developers, compare.Suppressed)
	}
}

func replayManifestCorrectionsDigest(t *testing.T, b []byte) string {
	t.Helper()
	var m struct {
		Corrections *store.CorrectionsDigest `json:"corrections_digest"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Corrections == nil || !strings.HasPrefix(m.Corrections.Value, "tierdig2:") {
		t.Fatalf("developer manifest omitted tierdig2 corrections_digest: %s", b)
	}
	return m.Corrections.Value
}
