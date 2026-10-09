package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// Tests for #914: an alias edit never moves an id's past between teams. Each
// raw id is placed only by its own dated hierarchy_membership rows, and every
// write that changes a raw id's placement appends rows at the server clock.
// The attack was measured on #912's head: in team mode, aliasing a
// never-assigned id to a named team's member moved the id's k-suppressed
// figures into that team for every past window, visible to a read token.

// aliasFixture is the measured reproduction: 6 in alpha, 6 in beta, and two ids
// (synth-u1, synth-u2) that were never assigned. Every id carries one event in
// window A and one in window B; both windows end before any write the test makes.
type aliasFixture struct {
	h      *Handler
	db     *store.DB
	c      datedClock
	mid    time.Time
	scores string // /scores over [since, beforeMove)
	cmp    string // /scores/compare, A = [since, mid), B = [mid, beforeMove)
}

func newAliasFixture(t *testing.T) aliasFixture {
	t.Helper()
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	var devs []datedDev
	for i := 1; i <= 6; i++ {
		devs = append(devs,
			datedDev{name: fmt.Sprintf("synth-a%d", i), team: "alpha", division: "div-alpha", cost: 10 + float64(i), points: 1 + 0.5*float64(i)},
			datedDev{name: fmt.Sprintf("synth-b%d", i), team: "beta", division: "div-beta", cost: 50 + float64(i), points: 2 + float64(i)})
	}
	seedDatedDevs(t, db, c, devs)
	mid := c.since.AddDate(0, 0, 20)
	for _, d := range devs {
		seedCostAt(t, db, d.name, "j-"+d.name, d.cost+100, mid.Add(time.Hour))
		seedOutcomeAt(t, db, d.name, "j-"+d.name, d.points, 1, mid.Add(time.Hour))
	}
	for _, u := range []struct {
		name         string
		cost, points float64
	}{{"synth-u1", 7, 3}, {"synth-u2", 9, 2}} {
		seedCostAt(t, db, u.name, "i-"+u.name, u.cost, c.past)
		seedOutcomeAt(t, db, u.name, "i-"+u.name, u.points, 1, c.past)
		seedCostAt(t, db, u.name, "j-"+u.name, u.cost, mid.Add(time.Hour))
		seedOutcomeAt(t, db, u.name, "j-"+u.name, u.points, 1, mid.Add(time.Hour))
	}
	// synth-u1/u2 sit in "other"; the padding lifts it to k in both windows so the
	// named rows publish beside it (#864).
	padResidual(t, db, 5, c.past, mid.Add(time.Hour))
	return aliasFixture{
		h: h, db: db, c: c, mid: mid,
		scores: c.query(c.beforeMove),
		cmp: "/api/v1/scores/compare?since_a=" + c.since.Format("2006-01-02") + "&until_a=" + mid.Format("2006-01-02") +
			"&since_b=" + mid.Format("2006-01-02") + "&until_b=" + c.beforeMove.Format("2006-01-02"),
	}
}

// snapshot is the /scores and /scores/compare bodies of the fixture's windows.
func (f aliasFixture) snapshot(t *testing.T) [2][]byte {
	t.Helper()
	return [2][]byte{rawScores(t, f.h, f.scores), rawScores(t, f.h, f.cmp)}
}

func (f aliasFixture) assertUnchanged(t *testing.T, step string, before [2][]byte) {
	t.Helper()
	after := f.snapshot(t)
	if !bytes.Equal(before[0], after[0]) {
		t.Errorf("%s: /scores over a window that ended before the write changed:\nbefore %s\nafter  %s", step, before[0], after[0])
	}
	if !bytes.Equal(before[1], after[1]) {
		t.Errorf("%s: /scores/compare over windows that ended before the write changed:\nbefore %s\nafter  %s", step, before[1], after[1])
	}
}

// withoutHeadCount strips, from a /scores or /scores/compare body, the two
// fields that report WHO counts as one person toward the k floor (#856): every
// kanon_suppressed.developers and data_quality.uncounted_active_ids. The alias
// map is not dated, so an alias edit that joins an active id to a counted person
// changes those counts over past windows. That is #914's ruled head-count
// residual (operator ruling A, 2026-09-28), accepted until #913's sealed
// snapshots close it. Every figure, every label and every withheld_* flag stays
// in the body, so a comparison of stripped bodies still fails if any past figure
// or placement moves, or if a group starts or stops clearing k.
func withoutHeadCount(t *testing.T, body []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var strip func(any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ks, ok := x["kanon_suppressed"].(map[string]any); ok {
				delete(ks, "developers")
			}
			if dq, ok := x["data_quality"].(map[string]any); ok {
				delete(dq, "uncounted_active_ids")
			}
			for _, c := range x {
				strip(c)
			}
		case []any:
			for _, c := range x {
				strip(c)
			}
		}
	}
	strip(v)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertFiguresUnchanged is assertUnchanged less the ruled head-count residual
// (withoutHeadCount): for a step that joins an active id to a counted person.
func (f aliasFixture) assertFiguresUnchanged(t *testing.T, step string, before [2][]byte) {
	t.Helper()
	after := f.snapshot(t)
	for i, what := range []string{"/scores", "/scores/compare"} {
		if b, a := withoutHeadCount(t, before[i]), withoutHeadCount(t, after[i]); !bytes.Equal(b, a) {
			t.Errorf("%s: %s over a window that ended before the write changed beyond the head count:\nbefore %s\nafter  %s", step, what, b, a)
		}
	}
}

func postAlias(t *testing.T, h *Handler, alias, canonical string) {
	t.Helper()
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias",
		developerAliasRequest{Alias: alias, Canonical: canonical}); code != http.StatusCreated {
		t.Fatalf("POST developer_alias %s->%s: %d %s", alias, canonical, code, body)
	}
}

func deleteAlias(t *testing.T, h *Handler, alias string) {
	t.Helper()
	if code, body := doRequest(t, h, http.MethodDelete, "/api/v1/developer_alias/"+alias, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE developer_alias/%s: %d %s", alias, code, body)
	}
}

// TestAliasPlacement_NeverAssignedIdKeepsItsPast is #914's measured scenario:
// alias create, delete, re-point and delete again on the never-assigned
// synth-u1 and synth-u2. Every /scores and /scores/compare body over windows
// that ended before the writes keeps every figure and placement after each one,
// and is byte-identical after each delete. While an alias joins synth-u1 or
// synth-u2 to a counted person, that person also counts in the
// residual their unassigned events sit in, and the id is no longer an uncounted
// active id: the ruled head-count residual (withoutHeadCount).
func TestAliasPlacement_NeverAssignedIdKeepsItsPast(t *testing.T) {
	f := newAliasFixture(t)
	before := f.snapshot(t)
	// Non-vacuous: both teams are named, and the never-assigned ids are held out
	// of them (alpha = synth-a1..a6 only).
	resp := getScores(t, f.h, f.scores)
	if a := teamRow(t, resp.Teams, "alpha"); !near(a.TotalCostUSD, 81+(81+600)) {
		t.Fatalf("alpha = $%v before any alias write, want $762 (synth-a1..a6 in both windows)", a.TotalCostUSD)
	}
	teamRow(t, resp.Teams, "beta")

	postAlias(t, f.h, "synth-u1", "synth-a1")
	f.assertFiguresUnchanged(t, "alias synth-u1 -> synth-a1", before)
	deleteAlias(t, f.h, "synth-u1")
	f.assertUnchanged(t, "delete synth-u1", before)
	postAlias(t, f.h, "synth-u2", "synth-a1")
	f.assertFiguresUnchanged(t, "alias synth-u2 -> synth-a1", before)
	postAlias(t, f.h, "synth-u2", "synth-b1")
	f.assertFiguresUnchanged(t, "re-point synth-u2 -> synth-b1", before)
	deleteAlias(t, f.h, "synth-u2")
	f.assertUnchanged(t, "delete synth-u2", before)
}

// TestAliasPlacement_AliasToAnIdleRosteredIdLeavesThePastCountAlone (#856 with
// #914): a person is on the roster in a window only through a raw id that
// carries their cost events or outcomes in it. idle-gh is on the roster for the
// whole window with no activity in it; synth-u1 acted in it and was never
// assigned. Aliasing synth-u1 to idle-gh must not roster synth-u1's past, so
// every body stays byte-identical, head count included.
func TestAliasPlacement_AliasToAnIdleRosteredIdLeavesThePastCountAlone(t *testing.T) {
	f := newAliasFixture(t)
	rosterOnly(t, f.db, "alpha", "idle-gh")
	before := f.snapshot(t)
	// Non-vacuous: synth-u1 and synth-u2 are the window's two off-roster ids.
	assertUncounted(t, before[0], "not_on_roster", 2)
	postAlias(t, f.h, "synth-u1", "idle-gh")
	f.assertUnchanged(t, "alias synth-u1 -> idle-gh", before)
}

// TestAliasPlacement_AssignedIdKeepsItsPast is the issue's control: aliasing an
// ASSIGNED id (synth-b2, beta) to an alpha member leaves every past window
// identical, and so does re-pointing and deleting it.
func TestAliasPlacement_AssignedIdKeepsItsPast(t *testing.T) {
	f := newAliasFixture(t)
	before := f.snapshot(t)
	postAlias(t, f.h, "synth-b2", "synth-a1")
	f.assertUnchanged(t, "alias synth-b2 -> synth-a1", before)
	postAlias(t, f.h, "synth-b2", "synth-a2")
	f.assertUnchanged(t, "re-point synth-b2 -> synth-a2", before)
	deleteAlias(t, f.h, "synth-b2")
	f.assertUnchanged(t, "delete synth-b2", before)
}

// TestAliasPlacement_AliasEditPlacesLaterEventsWithThePerson: the write that
// keeps the past still moves the id going forward. After synth-u1 is aliased to
// an alpha member, its next event scores under alpha; after the alias is
// deleted, its next event is unassigned again.
func TestAliasPlacement_AliasEditPlacesLaterEventsWithThePerson(t *testing.T) {
	f := newAliasFixture(t)
	postAlias(t, f.h, "synth-u1", "synth-a1")
	post := time.Now().UTC().Add(time.Millisecond)
	seedCostAt(t, f.db, "synth-u1", "k-synth-u1", 4.25, post)
	span := getScores(t, f.h, f.c.query(f.c.spanEnd))
	// alpha over the spanning window: A and B events of synth-a1..a6 plus u1's post-alias $4.25.
	if a := teamRow(t, span.Teams, "alpha"); !near(a.TotalCostUSD, 81+(81+600)+4.25) {
		t.Errorf("alpha over the spanning window = $%v, want $766.25 (synth-a1..a6 + synth-u1's post-alias $4.25 only)", a.TotalCostUSD)
	}

	deleteAlias(t, f.h, "synth-u1")
	later := time.Now().UTC().Add(time.Millisecond)
	seedCostAt(t, f.db, "synth-u1", "l-synth-u1", 2.5, later)
	span = getScores(t, f.h, f.c.query(f.c.spanEnd))
	if a := teamRow(t, span.Teams, "alpha"); !near(a.TotalCostUSD, 81+(81+600)+4.25) {
		t.Errorf("alpha after the delete = $%v, want $766.25 — synth-u1's post-delete event is its own again", a.TotalCostUSD)
	}
}

// TestAliasPlacement_CanonicalFirstAssignmentKeepsInheritedPast (#914 case 3):
// asmith-gh is assigned eng and later aliased to alice.smith, who has no row of
// her own. Neither the alias nor alice.smith's first own assignment re-homes
// any event of a window that ended before them. The alias joins alice.smith's
// unassigned activity to a counted person, so only the ruled head-count
// residual (withoutHeadCount) may move; her assignment, a hierarchy write, moves
// nothing at all.
func TestAliasPlacement_CanonicalFirstAssignmentKeepsInheritedPast(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	var devs []datedDev
	for i := 1; i <= 5; i++ {
		devs = append(devs,
			datedDev{name: fmt.Sprintf("e%d", i), team: "eng", cost: 10 + float64(i), points: 1},
			datedDev{name: fmt.Sprintf("p%d", i), team: "payments", cost: 20 + float64(i), points: 2})
	}
	devs = append(devs, datedDev{name: "asmith-gh", team: "eng", cost: 7, points: 5})
	seedDatedDevs(t, db, c, devs)
	seedCostAt(t, db, "alice.smith", "i-alice.smith", 3, c.past)
	seedOutcomeAt(t, db, "alice.smith", "i-alice.smith", 4, 1, c.past)
	// alice.smith sits in "other"; the padding lifts it to k so the comparisons
	// below read published rows, not two withheld responses (#864).
	padResidual(t, db, 5, c.past)

	before := rawScores(t, h, c.query(c.beforeMove))
	var published scoresResponse
	if err := json.Unmarshal(before, &published); err != nil || len(published.Teams) < 3 {
		t.Fatalf("control: eng, payments and other must publish before the alias; body %s", before)
	}
	postAlias(t, h, "asmith-gh", "alice.smith")
	aliased := rawScores(t, h, c.query(c.beforeMove))
	if b, a := withoutHeadCount(t, before), withoutHeadCount(t, aliased); !bytes.Equal(b, a) {
		t.Fatalf("aliasing asmith-gh to alice.smith changed a past window beyond the head count:\nbefore %s\nafter  %s", b, a)
	}
	moveDeveloper(t, h, "alice.smith", "payments", "")
	if after := rawScores(t, h, c.query(c.beforeMove)); !bytes.Equal(aliased, after) {
		t.Fatalf("alice.smith's first own assignment changed a past window:\nbefore %s\nafter  %s", aliased, after)
	}
}

// TestAliasPlacement_LaterCanonicalMoveOutranksAnEarlierSortingAlias (#914 case
// 5, Codex 7599bd88 on #912): aaron-gh is assigned alpha, aliased to zoe, and
// zoe is then PUT into beta. aaron-gh sorts before zoe and still had an open
// alpha row, and under ascending-raw-id resolution every later aaron-gh event
// stayed in alpha. Each raw id now carries its own rows, so later activity
// under EITHER id scores under beta and the pre-move history stays in alpha.
// TestDatedMembership_MovedCanonicalPlacesAliasedEvents is the opposite
// ordering (alice.smith sorts before asmith-gh).
func TestAliasPlacement_LaterCanonicalMoveOutranksAnEarlierSortingAlias(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	c := newDatedClock()
	var devs []datedDev
	for i := 1; i <= 5; i++ {
		devs = append(devs,
			datedDev{name: fmt.Sprintf("a%d", i), team: "alpha", cost: 10 + float64(i), points: 1},
			datedDev{name: fmt.Sprintf("b%d", i), team: "beta", cost: 20 + float64(i), points: 2})
	}
	devs = append(devs, datedDev{name: "aaron-gh", team: "alpha", cost: 7, points: 5})
	seedDatedDevs(t, db, c, devs)

	before := rawScores(t, h, c.query(c.beforeMove))
	postAlias(t, h, "aaron-gh", "zoe")
	moveDeveloper(t, h, "zoe", "beta", "")
	if after := rawScores(t, h, c.query(c.beforeMove)); !bytes.Equal(before, after) {
		t.Fatalf("a window that ended before the alias and the move changed:\nbefore %s\nafter  %s", before, after)
	}

	post := time.Now().UTC().Add(time.Millisecond)
	seedCostAt(t, db, "aaron-gh", "i-aaron-gh-post", 4.25, post)
	seedCostAt(t, db, "zoe", "i-zoe-post", 1.5, post)
	span := getScores(t, h, c.query(c.spanEnd))
	if a := teamRow(t, span.Teams, "alpha"); !near(a.TotalCostUSD, 11+12+13+14+15+7) {
		t.Errorf("spanning alpha = $%v, want $72 (a1..a5 + aaron-gh's pre-move $7 only)", a.TotalCostUSD)
	}
	if b := teamRow(t, span.Teams, "beta"); !near(b.TotalCostUSD, 21+22+23+24+25+4.25+1.5) {
		t.Errorf("spanning beta = $%v, want $120.75 (b1..b5 + aaron-gh's $4.25 and zoe's $1.5 after the move)", b.TotalCostUSD)
	}
}

// membershipIDs returns the developers holding hierarchy_membership rows.
func membershipIDs(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	rows, err := db.HierarchyMembership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range rows {
		out[r.Developer]++
	}
	return out
}

// TestAliasPlacement_EraseAndExportCoverAppendedRows: the rows an alias write
// appends for an aliased id are personal data of the person it resolves to.
// Export discloses them and erasure deletes them; a re-pointed alias's rows go
// with its new person, not its old one.
func TestAliasPlacement_EraseAndExportCoverAppendedRows(t *testing.T) {
	f := newAliasFixture(t)
	postAlias(t, f.h, "synth-u1", "synth-a1")
	postAlias(t, f.h, "synth-u2", "synth-a1")
	postAlias(t, f.h, "synth-u2", "synth-b1")
	ids := membershipIDs(t, f.db)
	if ids["synth-u1"] == 0 || ids["synth-u2"] == 0 {
		t.Fatalf("membership rows by developer = %v, want rows appended for synth-u1 and synth-u2 by the alias writes", ids)
	}

	code, body := doRequest(t, f.h, http.MethodGet, "/api/v1/developer/synth-a1/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export synth-a1: %d %s", code, body)
	}
	var exp struct {
		HierarchyMembership []struct {
			Developer string `json:"developer"`
		} `json:"hierarchy_membership"`
	}
	if err := json.Unmarshal(body, &exp); err != nil {
		t.Fatal(err)
	}
	exported := map[string]int{}
	for _, r := range exp.HierarchyMembership {
		exported[r.Developer]++
	}
	if exported["synth-u1"] != ids["synth-u1"] || exported["synth-a1"] != ids["synth-a1"] || exported["synth-u2"] != 0 {
		t.Errorf("synth-a1's export membership = %v, want every synth-a1 and synth-u1 row (%v) and none of re-pointed synth-u2's", exported, ids)
	}

	if code, body := doRequest(t, f.h, http.MethodDelete, "/api/v1/developer/synth-a1", nil); code != http.StatusOK {
		t.Fatalf("erase synth-a1: %d %s", code, body)
	}
	after := membershipIDs(t, f.db)
	if after["synth-a1"] != 0 || after["synth-u1"] != 0 {
		t.Errorf("rows left after erasing synth-a1 = %v, want none for synth-a1 or its alias synth-u1", after)
	}
	if after["synth-u2"] != ids["synth-u2"] || after["synth-b1"] != ids["synth-b1"] {
		t.Errorf("erasing synth-a1 touched synth-b1's person: before %v, after %v", ids, after)
	}

	if code, body := doRequest(t, f.h, http.MethodDelete, "/api/v1/developer/synth-u2", nil); code != http.StatusOK {
		t.Fatalf("erase synth-u2: %d %s", code, body)
	}
	if after := membershipIDs(t, f.db); after["synth-u2"] != 0 || after["synth-b1"] != 0 {
		t.Errorf("rows left after erasing synth-b1's person via synth-u2 = %v, want none", after)
	}
}

// legacyMembershipDB builds one store whose rows were written by #912, which
// never appended membership rows on an alias write: the canonical ids hold the
// dated history and the aliases hold none (or, for s-gh, the only row). It
// returns the path, closed. With aliased=false it records every event under the
// canonical id instead, with no aliases — the placement #912 read the aliased
// store as, and which reads the same under every version.
func legacyMembershipDB(t *testing.T, c datedClock, aliased bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	registerTestStore(db, path)
	var devs []datedDev
	for i := 1; i <= 5; i++ {
		devs = append(devs,
			datedDev{name: fmt.Sprintf("a%d", i), team: "alpha", division: "div-alpha", cost: 10 + float64(i), points: 1},
			datedDev{name: fmt.Sprintf("b%d", i), team: "beta", division: "div-beta", cost: 20 + float64(i), points: 2})
	}
	seedDatedDevs(t, db, c, devs)
	// id is where an event is recorded: the raw alias in the aliased store, the
	// canonical in the reference store.
	id := func(alias, canonical string) string {
		if aliased {
			return alias
		}
		return canonical
	}
	move := c.since.AddDate(0, 0, 20)
	// m.os moved alpha -> beta at move; m-gh aliases to it and holds no row.
	seedCostAt(t, db, "m.os", "i-m.os", 3, c.past)
	seedCostAt(t, db, id("m-gh", "m.os"), "i-m-gh", 5, c.past)
	seedCostAt(t, db, id("m-gh", "m.os"), "j-m-gh", 6, move.AddDate(0, 0, 5))
	seedOutcomeAt(t, db, id("m-gh", "m.os"), "j-m-gh", 4, 1, move.AddDate(0, 0, 5))
	// s.os has no row; its alias s-gh holds beta, so #912 placed the person there.
	seedCostAt(t, db, "s.os", "i-s.os", 2, c.past)
	seedCostAt(t, db, id("s-gh", "s.os"), "i-s-gh", 8, c.past)
	// synth-u1 was never assigned and aliases to a1 (alpha).
	seedCostAt(t, db, id("synth-u1", "a1"), "i-synth-u1", 7, c.past)
	seedOutcomeAt(t, db, id("synth-u1", "a1"), "i-synth-u1", 3, 1, c.past)
	_ = db.Close()

	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO org_hierarchy (developer, team, division, org) VALUES ('m.os', 'beta', 'div-beta', 'acme')`, nil},
		{`INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES ('m.os', 'alpha', 'div-alpha', ?, 'fp')`, []any{store.MembershipBaselineFrom}},
		{`UPDATE hierarchy_membership SET valid_to = ? WHERE developer = 'm.os'`, []any{move}},
		{`INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES ('m.os', 'beta', 'div-beta', ?, 'fp')`, []any{move}},
		{`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'beta', 'div-beta', 'acme')`, []any{id("s-gh", "s.os")}},
		{`INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES (?, 'beta', 'div-beta', ?, 'fp')`, []any{id("s-gh", "s.os"), store.MembershipBaselineFrom}},
	}
	if aliased {
		stmts = append(stmts, struct {
			q    string
			args []any
		}{`INSERT INTO developer_alias (alias, canonical) VALUES ('m-gh', 'm.os'), ('s-gh', 's.os'), ('synth-u1', 'a1')`, nil})
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	return path
}

// TestAliasPlacement_UpgradeMovesNothing: the #914 migration gives each legacy
// alias its person's full dated history, so a store written by #912 reads, after
// the upgrade, exactly as #912 read it — byte-identical to a reference store
// holding the same events under the canonical ids. The control arm skips the
// migration on the same store and must read differently, or the equality
// above proves nothing.
func TestAliasPlacement_UpgradeMovesNothing(t *testing.T) {
	c := newDatedClock()
	read := func(path string, rewind bool) []byte {
		t.Helper()
		if rewind {
			raw, err := sql.Open("sqlite", rawTestDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`DELETE FROM tier_migrations WHERE name = ?`, store.MigrationAliasMembershipHistory); err != nil {
				t.Fatal(err)
			}
			_ = raw.Close()
		}
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		h := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", RateLimitConfig{}, WithUnsealedRecompute())
		h.SetAggregation(scoring.AggregationTeam, 5)
		var out []byte
		for _, q := range []string{c.query(c.beforeMove),
			"/api/v1/scores/compare?since_a=" + c.since.Format("2006-01-02") + "&until_a=" + c.since.AddDate(0, 0, 20).Format("2006-01-02") +
				"&since_b=" + c.since.AddDate(0, 0, 20).Format("2006-01-02") + "&until_b=" + c.beforeMove.Format("2006-01-02")} {
			out = append(append(out, rawScores(t, h, q)...), '\n')
		}
		return out
	}

	want := read(legacyMembershipDB(t, c, false), false)
	upgraded := legacyMembershipDB(t, c, true)
	if got := read(upgraded, true); !bytes.Equal(want, got) {
		t.Fatalf("the upgrade moved history:\n#912 read %s\nupgraded  %s", want, got)
	}
	// Idempotent: a second pass over the migrated store changes nothing.
	if got := read(upgraded, true); !bytes.Equal(want, got) {
		t.Fatalf("re-running the migration moved history:\n#912 read %s\nre-run    %s", want, got)
	}

	control := legacyMembershipDB(t, c, true)
	raw, err := sql.Open("sqlite", rawTestDSN(control))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT OR IGNORE INTO tier_migrations (name) VALUES (?)`, store.MigrationAliasMembershipHistory); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if got := read(control, false); bytes.Equal(want, got) {
		t.Fatalf("control: skipping the migration read the same as the #912 reference — the fixture cannot tell whether the migration ran")
	}
}
