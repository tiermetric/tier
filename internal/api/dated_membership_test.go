package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// Tests for dated team membership on the score paths (#886). The attack they pin
// was measured on d22b454: read /scores in team mode, move one developer with
// PUT /api/v1/org_hierarchy/{dev}, read again, and the difference between the two
// reads of BOTH teams' rows was exactly that developer's figures, for any window.

// datedClock is the set of instants every test here derives its fixture from.
// The move always happens at the real server clock (the store stamps it; no test
// can choose it), so "before the move" is a window ending at today's UTC
// midnight and "after" is an event stamped just after the PUT returns.
type datedClock struct {
	since       time.Time // window start, two calendar months back
	past        time.Time // pre-move events
	spendPeriod string    // pre-move actual_spend period
	beforeMove  time.Time // exclusive end of a window that closes before the move
	spanEnd     time.Time // exclusive end of a window that spans the move
}

func newDatedClock() datedClock {
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	since := month.AddDate(0, -2, 0)
	return datedClock{
		since:       since,
		past:        since.AddDate(0, 0, 5).Add(12 * time.Hour),
		spendPeriod: since.Format("2006-01"),
		beforeMove:  now.Truncate(24 * time.Hour),
		spanEnd:     now.Truncate(24*time.Hour).AddDate(0, 0, 2),
	}
}

func (c datedClock) query(until time.Time) string {
	return "/api/v1/scores?since=" + c.since.Format("2006-01-02") + "&until=" + until.Format("2006-01-02")
}

// datedDev is one fixture developer: a baseline (pre-upgrade) placement plus one
// pre-move cost event, one pre-move outcome, and optionally a pre-move invoice.
type datedDev struct {
	name, team, division string
	cost, points, spend  float64
}

func seedDatedDevs(t *testing.T, db *store.DB, c datedClock, devs []datedDev) {
	t.Helper()
	ctx := context.Background()
	for _, d := range devs {
		if err := baselineHierarchy(db, ctx, d.name, d.team, d.division, "acme"); err != nil {
			t.Fatalf("baselineHierarchy(%s): %v", d.name, err)
		}
		seedCostAt(t, db, d.name, "i-"+d.name, d.cost, c.past)
		seedOutcomeAt(t, db, d.name, "i-"+d.name, d.points, 1, c.past)
		if d.spend != 0 {
			if err := db.InsertActualSpend(ctx, store.ActualSpend{
				Developer: d.name, Period: c.spendPeriod,
				ActualPaidMicro: store.DollarsToMicro(d.spend), Timestamp: c.past,
			}); err != nil {
				t.Fatalf("InsertActualSpend(%s): %v", d.name, err)
			}
		}
	}
}

// twoTeams is the measured reproduction's shape: 6 in alpha, 5 in beta, so both
// clear k=5 before and after one alpha developer leaves. Every alpha developer
// carries an invoice (a3's is $7.50, the rest $1), so the actual_paid_usd channel
// is inside the byte comparison and alpha's paid measure clears the floor (#856).
func twoTeams() []datedDev {
	var devs []datedDev
	for i := 1; i <= 6; i++ {
		devs = append(devs, datedDev{name: fmt.Sprintf("a%d", i), team: "alpha", division: "div-alpha",
			cost: 10 + float64(i), points: 1 + 0.5*float64(i)})
	}
	for i := 1; i <= 5; i++ {
		devs = append(devs, datedDev{name: fmt.Sprintf("b%d", i), team: "beta", division: "div-beta",
			cost: 50 + float64(i), points: 2 + float64(i)})
	}
	for i := 0; i < 6; i++ {
		devs[i].spend = 1
	}
	devs[2].spend = 7.5 // a3
	return devs
}

func teamRow(t *testing.T, rows []teamScoreJSON, team string) teamScoreJSON {
	t.Helper()
	for _, r := range rows {
		if r.Team == team {
			return r
		}
	}
	t.Fatalf("no %q row in %+v", team, rows)
	return teamScoreJSON{}
}

func moveDeveloper(t *testing.T, h *Handler, dev, team, division string) {
	t.Helper()
	code, body := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/"+dev,
		map[string]string{"team": team, "division": division, "org": "acme"})
	if code != http.StatusOK {
		t.Fatalf("PUT org_hierarchy/%s: %d %s", dev, code, body)
	}
}

func rawScores(t *testing.T, h *Handler, target string) []byte {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, target, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, code, body)
	}
	return body
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestDatedMembership_MoveDoesNotRewriteHistory is the measured attack. A window
// that ended before the move reads byte-identically after it — teams, work_types
// teams, total, everything — and a window spanning the move shows the moved
// developer's pre-move figures in the old team and post-move figures in the new.
func TestDatedMembership_MoveDoesNotRewriteHistory(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	devs := twoTeams()
	seedDatedDevs(t, db, c, devs)

	before := rawScores(t, h, c.query(c.beforeMove))

	// Non-vacuous: both teams are NAMED rows carrying the seeded sums, so an
	// identical second read cannot be two identical suppressions.
	resp := getScores(t, h, c.query(c.beforeMove))
	alpha, beta := teamRow(t, resp.Teams, "alpha"), teamRow(t, resp.Teams, "beta")
	if !near(alpha.TotalCostUSD, 11+12+13+14+15+16) || !near(beta.TotalCostUSD, 51+52+53+54+55) {
		t.Fatalf("pre-move rows: alpha $%v beta $%v, want $81 / $265", alpha.TotalCostUSD, beta.TotalCostUSD)
	}
	if !near(alpha.ActualPaidUSD, 12.5) {
		t.Fatalf("alpha actual_paid_usd = %v, want 12.5 (a3's 7.5 invoice plus five $1)", alpha.ActualPaidUSD)
	}

	moveDeveloper(t, h, "a3", "beta", "div-beta")

	if after := rawScores(t, h, c.query(c.beforeMove)); !bytes.Equal(before, after) {
		t.Fatalf("a window that ended before the move changed after it:\nbefore %s\nafter  %s", before, after)
	}

	// Post-move activity lands in beta; the pre-move history stays in alpha.
	post := time.Now().UTC().Add(time.Millisecond)
	seedCostAt(t, db, "a3", "i-a3-post", 4.25, post)
	seedOutcomeAt(t, db, "a3", "i-a3-post", 3, 1, post)
	span := getScores(t, h, c.query(c.spanEnd))
	sa, sb := teamRow(t, span.Teams, "alpha"), teamRow(t, span.Teams, "beta")
	if !near(sa.TotalCostUSD, alpha.TotalCostUSD) || !near(sa.WeightedPoints, alpha.WeightedPoints) || !near(sa.ActualPaidUSD, 12.5) {
		t.Errorf("spanning alpha = %+v, want exactly the pre-move alpha row %+v (a3's history stays)", sa, alpha)
	}
	if !near(sb.TotalCostUSD, beta.TotalCostUSD+4.25) || !near(sb.WeightedPoints, beta.WeightedPoints+3) || !near(sb.ActualPaidUSD, 0) {
		t.Errorf("spanning beta = %+v, want the pre-move beta row plus a3's post-move $4.25 / 3 pts only", sb)
	}
}

// TestDatedMembership_CompareHistoryUnchanged: /scores/compare over two windows
// that both ended before a move reads byte-identically after it.
func TestDatedMembership_CompareHistoryUnchanged(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	seedDatedDevs(t, db, c, twoTeams())
	// A second pre-move event per developer in window B, so every team clears the
	// floor in BOTH windows and the rows are named, not folded.
	mid := c.since.AddDate(0, 0, 20)
	for _, d := range twoTeams() {
		seedCostAt(t, db, d.name, "j-"+d.name, d.cost+100, mid.Add(time.Hour))
		seedOutcomeAt(t, db, d.name, "j-"+d.name, d.points, 1, mid.Add(time.Hour))
	}
	url := "/api/v1/scores/compare?since_a=" + c.since.Format("2006-01-02") + "&until_a=" + mid.Format("2006-01-02") +
		"&since_b=" + mid.Format("2006-01-02") + "&until_b=" + c.beforeMove.Format("2006-01-02")

	code, before := doRequest(t, h, http.MethodGet, url, nil)
	if code != http.StatusOK {
		t.Fatalf("compare: %d %s", code, before)
	}
	_, resp := getCompare(t, h, url)
	named := map[string]bool{}
	for _, r := range resp.Teams {
		named[r.Team] = true
	}
	if !named["alpha"] || !named["beta"] {
		t.Fatalf("compare teams = %+v, want alpha and beta named in both windows (non-vacuous)", resp.Teams)
	}

	moveDeveloper(t, h, "a3", "beta", "div-beta")

	if _, after := doRequest(t, h, http.MethodGet, url, nil); !bytes.Equal(before, after) {
		t.Fatalf("compare over two pre-move windows changed after the move:\nbefore %s\nafter  %s", before, after)
	}
}

// TestDatedMembership_DivisionMoveDoesNotRewriteHistory: division mode groups by
// the same dated rows, so a division move is held to the same rule.
func TestDatedMembership_DivisionMoveDoesNotRewriteHistory(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationDivision, 5)
	c := newDatedClock()
	seedDatedDevs(t, db, c, twoTeams())

	before := rawScores(t, h, c.query(c.beforeMove))
	resp := getScores(t, h, c.query(c.beforeMove))
	if a := teamRow(t, resp.Teams, "div-alpha"); !near(a.TotalCostUSD, 81) {
		t.Fatalf("div-alpha = %+v, want the six alpha developers' $81", a)
	}

	// Same team, new division: only the division timeline moves.
	moveDeveloper(t, h, "a3", "alpha", "div-beta")

	if after := rawScores(t, h, c.query(c.beforeMove)); !bytes.Equal(before, after) {
		t.Fatalf("division-mode history changed after a division move:\nbefore %s\nafter  %s", before, after)
	}
}

// TestDatedMembership_CostAndOutcomeStayWhereTheyHappened: one issue whose cost
// was incurred in team X and whose outcome merged after a move to Y. X shows the
// cost, Y the outcome, in the pooled rows and developer mode's team_rollups;
// nothing is re-homed. (An anonymised mode publishes no work-type rows, #864.)
func TestDatedMembership_CostAndOutcomeStayWhereTheyHappened(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, scoring.MinKAnonymity)
	c := newDatedClock()
	var devs []datedDev
	for i := 1; i <= 3; i++ {
		devs = append(devs,
			datedDev{name: fmt.Sprintf("x%d", i), team: "X", cost: 10, points: 1},
			datedDev{name: fmt.Sprintf("y%d", i), team: "Y", cost: 20, points: 2})
	}
	seedDatedDevs(t, db, c, devs)
	if err := baselineHierarchy(db, context.Background(), "dana", "X", "", "acme"); err != nil {
		t.Fatal(err)
	}
	seedCostAt(t, db, "dana", "i-dana", 7, c.past) // incurred in X

	moveDeveloper(t, h, "dana", "Y", "")
	post := time.Now().UTC().Add(time.Millisecond)
	seedOutcomeAt(t, db, "dana", "i-dana", 5, 1, post) // merged in Y

	check := func(where string, x, y teamScoreJSON) {
		t.Helper()
		if !near(x.TotalCostUSD, 30+7) || !near(x.WeightedPoints, 3) {
			t.Errorf("%s X = $%v / %v pts, want $37 / 3 (dana's cost, none of her points)", where, x.TotalCostUSD, x.WeightedPoints)
		}
		if !near(y.TotalCostUSD, 60) || !near(y.WeightedPoints, 6+5) {
			t.Errorf("%s Y = $%v / %v pts, want $60 / 11 (dana's points, none of her cost)", where, y.TotalCostUSD, y.WeightedPoints)
		}
	}
	resp := getScores(t, h, c.query(c.spanEnd))
	check("teams", teamRow(t, resp.Teams, "X"), teamRow(t, resp.Teams, "Y"))

	h.SetAggregation(scoring.AggregationDeveloper, 0)
	dev := getScores(t, h, c.query(c.spanEnd))
	var rx, ry teamScoreJSON
	for _, r := range dev.TeamRollups {
		switch r.Team {
		case "X":
			rx = r.teamScoreJSON
		case "Y":
			ry = r.teamScoreJSON
		}
	}
	check("team_rollups", rx, ry)
}

// TestDatedMembership_UpgradeBaselineAndLateAssignment runs the real upgrade: a
// store holding org_hierarchy and events but no dated table is reopened, the
// #886 migration baselines the existing map, and past windows score under it. A
// developer first assigned AFTER the upgrade keeps their earlier history out of
// the team — in the unassigned row in developer mode, never in the named team.
func TestDatedMembership_UpgradeBaselineAndLateAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := newDatedClock()
	for _, d := range twoTeams() {
		if err := db.UpsertHierarchy(ctx, d.name, d.team, d.division, "acme", "pre-upgrade"); err != nil {
			t.Fatal(err)
		}
		seedCostAt(t, db, d.name, "i-"+d.name, d.cost, c.past)
		seedOutcomeAt(t, db, d.name, "i-"+d.name, d.points, 1, c.past)
	}
	seedCostAt(t, db, "newbie", "i-newbie", 9, c.past)
	seedOutcomeAt(t, db, "newbie", "i-newbie", 4, 1, c.past)
	// Five people each alone in a team fold into "other" beside newbie's history,
	// so the residual reaches k and the named rows publish (#864).
	for i := 1; i <= 5; i++ {
		pad := fmt.Sprintf("pad-%d", i)
		if err := db.UpsertHierarchy(ctx, pad, "pad-team-"+pad, "pad-div-"+pad, "acme", "pre-upgrade"); err != nil {
			t.Fatal(err)
		}
		seedCostAt(t, db, pad, "i-"+pad, 1, c.past)
		seedOutcomeAt(t, db, pad, "i-"+pad, 1, 1, c.past)
	}
	_ = db.Close()

	// Rewind to the pre-#886 shape: org_hierarchy populated, no dated table.
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE hierarchy_membership`,
		`DELETE FROM tier_migrations WHERE name = 'hierarchy_membership_baseline_v886'`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = raw.Close()

	db, err = store.Open(path) // the upgrade
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", RateLimitConfig{}, WithUnsealedRecompute())
	moveDeveloper(t, h, "newbie", "alpha", "div-alpha") // first assignment, post-upgrade

	h.SetAggregation(scoring.AggregationTeam, 5)
	resp := getScores(t, h, c.query(c.spanEnd))
	if a := teamRow(t, resp.Teams, "alpha"); !near(a.TotalCostUSD, 81) || !near(a.WeightedPoints, 1.5+2+2.5+3+3.5+4) {
		t.Errorf("alpha = $%v / %v pts, want the six baselined developers only ($81 / 16.5) — newbie's pre-assignment history must not join it", a.TotalCostUSD, a.WeightedPoints)
	}
	if b := teamRow(t, resp.Teams, "beta"); !near(b.TotalCostUSD, 265) {
		t.Errorf("beta = $%v, want $265 — the upgrade must keep existing members' history in their team", b.TotalCostUSD)
	}

	h.SetAggregation(scoring.AggregationDeveloper, 0)
	dev := getScores(t, h, c.query(c.spanEnd))
	var unassigned *teamRollupJSON
	for i := range dev.TeamRollups {
		if dev.TeamRollups[i].Unassigned {
			unassigned = &dev.TeamRollups[i]
		}
	}
	if unassigned == nil || !near(unassigned.TotalCostUSD, 9) || !near(unassigned.WeightedPoints, 4) {
		t.Errorf("unassigned rollup = %+v, want exactly newbie's pre-assignment $9 / 4 pts", unassigned)
	}
}

// TestDatedMembership_ClientCannotSupplyADate: no request field carries a date.
// A PUT or bulk import naming valid_from / valid_to is REFUSED (400) rather than
// silently ignored, and the membership row is untouched.
func TestDatedMembership_ClientCannotSupplyADate(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	if err := baselineHierarchy(db, ctx, "alice", "alpha", "", "acme"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"valid_from", "valid_to"} {
		code, body := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/alice",
			map[string]string{"team": "beta", field: "2020-01-01T00:00:00Z"})
		if code != http.StatusBadRequest {
			t.Errorf("PUT with %s: %d %s, want 400", field, code, body)
		}
		code, body = doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy",
			[]map[string]string{{"developer": "alice", "team": "beta", field: "2020-01-01T00:00:00Z"}})
		if code != http.StatusBadRequest {
			t.Errorf("bulk import with %s: %d %s, want 400", field, code, body)
		}
	}
	rows, err := db.HierarchyMembership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Team != "alpha" || !rows[0].ValidFrom.Equal(store.MembershipBaselineFrom) || !rows[0].ValidTo.IsZero() {
		t.Errorf("membership = %+v, want alice's baseline alpha row untouched", rows)
	}

	// Control arm: the same PUT without a date is accepted and APPENDS — the
	// refusals above are about the field, not a route that refuses everything.
	before := time.Now().UTC()
	moveDeveloper(t, h, "alice", "beta", "")
	rows, err = db.HierarchyMembership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Team != "alpha" || rows[0].ValidTo.Before(before) ||
		rows[1].Team != "beta" || !rows[1].ValidFrom.Equal(rows[0].ValidTo) {
		t.Errorf("membership after a dateless move = %+v, want alpha closed at the server clock and beta opened at the same instant", rows)
	}
}

// TestDatedMembership_WrittenByIsAFingerprint: the dated row records which
// credential wrote it, as a fingerprint — never the token itself.
func TestDatedMembership_WrittenByIsAFingerprint(t *testing.T) {
	const token = "write-admin-token-of-len-32-aaaa"
	h, db := newTestHandlerWithToken(t, token)
	auth := http.Header{"Authorization": {"Bearer " + token}}
	if code, body := doRequestWithHeader(t, h, http.MethodPut, "/api/v1/org_hierarchy/alice",
		map[string]string{"team": "alpha"}, auth); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/org_hierarchy",
		[]map[string]string{{"developer": "bob", "team": "beta"}}, auth); code != http.StatusCreated {
		t.Fatalf("bulk: %d %s", code, body)
	}

	p, _ := testStorePaths.Load(db)
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	sum := sha256.Sum256([]byte("tier/hierarchy-writer/v1\x00" + token))
	want := "sha256:" + hex.EncodeToString(sum[:8])
	for _, dev := range []string{"alice", "bob"} {
		var got string
		if err := raw.QueryRow(`SELECT written_by FROM hierarchy_membership WHERE developer = ?`, dev).Scan(&got); err != nil {
			t.Fatalf("%s: %v", dev, err)
		}
		if got != want {
			t.Errorf("%s written_by = %q, want %q", dev, got, want)
		}
		if strings.Contains(got, token) || strings.Contains(got, token[:12]) {
			t.Errorf("%s written_by %q carries the token", dev, got)
		}
	}
}

// teamFigures is the per-group figures a differencing read compares.
type teamFigures struct{ cost, points, paid float64 }

func figuresByTeam(rows []teamScoreJSON) map[string]teamFigures {
	out := map[string]teamFigures{}
	for _, r := range rows {
		out[r.Team] = teamFigures{r.TotalCostUSD, r.WeightedPoints, r.ActualPaidUSD}
	}
	return out
}

// aliasedPerson seeds R1/R2's shape (#886 engine review): e1..e4 in eng and
// p1..p5 in payments at the baseline, plus asmith-gh assigned eng at the
// baseline and aliased to alice.smith, who has no membership row of her own
// until the alias write appends one at the server clock (#914).
// Each seeded id carries one pre-move cost event and outcome; so does the raw
// id alice.smith, the canonical person's own capture identity.
func aliasedPerson(t *testing.T, h *Handler, db *store.DB, c datedClock) {
	t.Helper()
	var devs []datedDev
	for i := 1; i <= 4; i++ {
		devs = append(devs, datedDev{name: fmt.Sprintf("e%d", i), team: "eng", cost: 10 + float64(i), points: 1})
	}
	for i := 1; i <= 5; i++ {
		devs = append(devs, datedDev{name: fmt.Sprintf("p%d", i), team: "payments", cost: 20 + float64(i), points: 2})
	}
	devs = append(devs, datedDev{name: "asmith-gh", team: "eng", cost: 7, points: 5})
	seedDatedDevs(t, db, c, devs)
	seedCostAt(t, db, "alice.smith", "i-alice.smith", 3, c.past)
	seedOutcomeAt(t, db, "alice.smith", "i-alice.smith", 4, 1, c.past)
	// alice.smith's own pre-alias events sit in "other"; the padding lifts it to k
	// so the named rows publish beside it (#864).
	padResidual(t, db, 5, c.past)
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias",
		developerAliasRequest{Alias: "asmith-gh", Canonical: "alice.smith"}); code >= 300 {
		t.Fatalf("POST developer_alias: %d %s", code, body)
	}
}

// TestDatedMembership_AssigningCanonicalKeepsAliasedHistory (Codex R1): the
// canonical person alice.smith inherits eng through her alias asmith-gh's row,
// from the alias write on (#914). A later PUT assigning alice.smith to the same
// team is a hierarchy write with no alias edit, so every row of a window that
// ended before it is unchanged. The events recorded under alice.smith herself
// predate the alias, when she held no row, so they stay in "other" throughout.
func TestDatedMembership_AssigningCanonicalKeepsAliasedHistory(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	aliasedPerson(t, h, db, c)

	before := rawScores(t, h, c.query(c.beforeMove))
	// Non-vacuous: eng is named and carries both of alice.smith's ids' figures.
	eng := teamRow(t, getScores(t, h, c.query(c.beforeMove)).Teams, "eng")
	if !near(eng.TotalCostUSD, 11+12+13+14+7) {
		t.Fatalf("pre-PUT eng cost = $%v, want $57 (e1..e4 + asmith-gh $7; alice.smith's $3 predates the alias)", eng.TotalCostUSD)
	}

	moveDeveloper(t, h, "alice.smith", "eng", "")

	if after := rawScores(t, h, c.query(c.beforeMove)); !bytes.Equal(before, after) {
		t.Fatalf("a window that ended before the PUT changed after it:\nbefore %s\nafter  %s", before, after)
	}
}

// TestDatedMembership_MovedCanonicalPlacesAliasedEvents (Codex R2): asmith-gh
// is assigned eng and aliased to alice.smith; alice.smith is then moved to
// payments. An event recorded under asmith-gh AFTER the move is the canonical
// person's activity and scores under payments; the pre-move history stays in eng.
func TestDatedMembership_MovedCanonicalPlacesAliasedEvents(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	aliasedPerson(t, h, db, c)

	moveDeveloper(t, h, "alice.smith", "payments", "")
	post := time.Now().UTC().Add(time.Millisecond)
	seedCostAt(t, db, "asmith-gh", "i-asmith-gh-post", 4.25, post)
	seedOutcomeAt(t, db, "asmith-gh", "i-asmith-gh-post", 3, 1, post)

	span := getScores(t, h, c.query(c.spanEnd))
	eng, pay := teamRow(t, span.Teams, "eng"), teamRow(t, span.Teams, "payments")
	if !near(eng.TotalCostUSD, 11+12+13+14+7) {
		t.Errorf("spanning eng cost = $%v, want $57 (the pre-move history only; alice.smith's $3 predates the alias)", eng.TotalCostUSD)
	}
	if !near(pay.TotalCostUSD, 21+22+23+24+25+4.25) {
		t.Errorf("spanning payments cost = $%v, want $119.25 (p1..p5 + asmith-gh's post-move $4.25)", pay.TotalCostUSD)
	}
}

// TestDatedMembership_RepoScopedSubWindows: with a move inside the window, cost
// is re-read per membership sub-window, and those reads must keep ?repo=. Two
// repos, one move; the scoped team rows and team_rollups carry only that repo's
// cost on both sides of the move.
func TestDatedMembership_RepoScopedSubWindows(t *testing.T) {
	h, db := newTestHandler(t)
	c := newDatedClock()
	const repoA, repoB = "acme/a", "acme/b"
	var names []string
	for i := 1; i <= 5; i++ {
		names = append(names, fmt.Sprintf("p%d", i), fmt.Sprintf("q%d", i))
	}
	for _, n := range names {
		team := "P"
		if n[0] == 'q' {
			team = "Q"
		}
		if err := baselineHierarchy(db, context.Background(), n, team, "", "acme"); err != nil {
			t.Fatal(err)
		}
		seedRepoCostAt(t, db, repoA, n, "a-"+n, 1, c.past)
		seedRepoOutcomeAt(t, db, repoA, n, "a-"+n, 1, c.past)
		seedRepoCostAt(t, db, repoB, n, "b-"+n, 100, c.past)
		seedRepoOutcomeAt(t, db, repoB, n, "b-"+n, 1, c.past)
	}
	moveDeveloper(t, h, "p1", "Q", "") // a boundary inside the spanning window
	post := time.Now().UTC().Add(time.Millisecond)
	seedRepoCostAt(t, db, repoA, "p1", "a-p1-post", 0.5, post)
	seedRepoCostAt(t, db, repoB, "p1", "b-p1-post", 50, post)

	// ?repo= is refused in the anonymized modes (#185), so the scoped team rows
	// are developer mode's ?team= block and team_rollups.
	h.SetAggregation(scoring.AggregationDeveloper, 0)
	url := c.query(c.spanEnd) + "&repo=" + repoA
	var rollups []teamScoreJSON
	for _, r := range getScores(t, h, url).TeamRollups {
		rollups = append(rollups, r.teamScoreJSON)
	}
	got := figuresByTeam(rollups)
	if !near(got["P"].cost, 5) || !near(got["Q"].cost, 5.5) {
		t.Errorf("?repo=%s team_rollups = %+v, want P $5 (repo A's pre-move $1 x5) and Q $5.50 (+ p1's post-move $0.50), no repo B cost", repoA, got)
	}
	for team, want := range map[string]float64{"P": 5, "Q": 5.5} {
		tb := getScores(t, h, url+"&team="+team).Team
		if tb == nil || !near(tb.TotalCostUSD, want) {
			t.Errorf("?repo=%s&team=%s block = %+v, want cost $%v (repo A only)", repoA, team, tb, want)
		}
	}
}

// TestDatedMembership_CompareAcrossTheMove: window A ends before a move and
// window B starts after it. The mover's history stays in the old team in A,
// their new activity lands in the new team in B, and A reads byte-identically
// to the same window read before the move.
func TestDatedMembership_CompareAcrossTheMove(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	seedDatedDevs(t, db, c, twoTeams())

	aOnly := "/api/v1/scores/compare?since_a=" + c.since.Format("2006-01-02") + "&until_a=" + c.beforeMove.Format("2006-01-02") +
		"&since_b=" + c.since.Format("2006-01-02") + "&until_b=" + c.beforeMove.Format("2006-01-02")
	_, before := getCompare(t, h, aOnly)

	moveDeveloper(t, h, "a3", "beta", "div-beta")
	post := time.Now().UTC().Add(time.Millisecond)
	for _, d := range twoTeams() {
		seedCostAt(t, db, d.name, "post-"+d.name, 1, post)
		seedOutcomeAt(t, db, d.name, "post-"+d.name, 1, 1, post)
	}

	url := "/api/v1/scores/compare?since_a=" + c.since.Format("2006-01-02") + "&until_a=" + c.beforeMove.Format("2006-01-02") +
		"&since_b=" + c.beforeMove.Format("2006-01-02") + "&until_b=" + c.spanEnd.Format("2006-01-02")
	code, resp := getCompare(t, h, url)
	if code != http.StatusOK {
		t.Fatalf("compare: %d", code)
	}
	find := func(rows []teamDeltaJSON, team string) teamDeltaJSON {
		for _, r := range rows {
			if r.Team == team {
				return r
			}
		}
		t.Fatalf("no %q row in %+v", team, rows)
		return teamDeltaJSON{}
	}
	a, b := find(resp.Teams, "alpha"), find(resp.Teams, "beta")
	ba, bb := find(before.Teams, "alpha"), find(before.Teams, "beta")
	if !near(a.A.TotalCostUSD, ba.A.TotalCostUSD) || !near(b.A.TotalCostUSD, bb.A.TotalCostUSD) {
		t.Errorf("window A after the move: alpha $%v beta $%v, want the pre-move $%v / $%v", a.A.TotalCostUSD, b.A.TotalCostUSD, ba.A.TotalCostUSD, bb.A.TotalCostUSD)
	}
	// Window B: five $1 events stay in alpha, six (five + the mover) land in beta.
	if !near(a.B.TotalCostUSD, 5) || !near(b.B.TotalCostUSD, 6) {
		t.Errorf("window B: alpha $%v beta $%v, want $5 / $6 (a3's new activity in beta)", a.B.TotalCostUSD, b.B.TotalCostUSD)
	}
}

// TestDatedMembership_ClockBehindIs409: a host clock earlier than the open row's
// start refuses the move with 409 and a message naming the clock, not a bare 500.
func TestDatedMembership_ClockBehindIs409(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	if err := baselineHierarchy(db, ctx, "alice", "alpha", "", "acme"); err != nil {
		t.Fatal(err)
	}
	// An open row that starts a year ahead of the clock: what a clock that
	// stepped backwards leaves behind. No API writes this; the raw file does.
	p, _ := testStorePaths.Load(db)
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`DELETE FROM hierarchy_membership WHERE developer = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO hierarchy_membership (developer, team, division, valid_from, valid_to, written_by)
		VALUES ('alice', 'alpha', '', ?, NULL, 'test')`, time.Now().UTC().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	code, body := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/alice", map[string]string{"team": "beta", "org": "acme"})
	if code != http.StatusConflict || !strings.Contains(string(body), "clock") {
		t.Errorf("PUT with the clock behind: %d %s, want 409 naming the clock", code, body)
	}
	code, body = doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy",
		[]map[string]string{{"developer": "alice", "team": "beta", "org": "acme"}})
	if code != http.StatusConflict || !strings.Contains(string(body), "clock") {
		t.Errorf("bulk import with the clock behind: %d %s, want 409 naming the clock", code, body)
	}

	// The alias routes append rows too (#914). alice-gh's own open row is a
	// year ahead and in another team: aliasing it to alice moves it, and
	// unaliasing it (no org_hierarchy row of its own) ends it.
	if _, err := raw.Exec(`INSERT INTO hierarchy_membership (developer, team, division, valid_from, valid_to, written_by)
		VALUES ('alice-gh', 'gamma', '', ?, NULL, 'test')`, time.Now().UTC().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	code, body = doRequest(t, h, http.MethodPost, "/api/v1/developer_alias", developerAliasRequest{Alias: "alice-gh", Canonical: "alice"})
	if code != http.StatusConflict || !strings.Contains(string(body), "clock") {
		t.Errorf("POST /developer_alias with the clock behind: %d %s, want 409 naming the clock", code, body)
	}
	if _, err := raw.Exec(`INSERT INTO developer_alias (alias, canonical) VALUES ('alice-gh', 'alice')`); err != nil {
		t.Fatal(err)
	}
	code, body = doRequest(t, h, http.MethodDelete, "/api/v1/developer_alias/alice-gh", nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "clock") {
		t.Errorf("DELETE /developer_alias with the clock behind: %d %s, want 409 naming the clock", code, body)
	}
}
