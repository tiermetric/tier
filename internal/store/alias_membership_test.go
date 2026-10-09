package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Tests for #914: every write that changes a raw id's placement appends dated
// rows for that id at the server clock, and the upgrade gives each legacy alias
// its person's whole dated history.

// span is a membership row reduced to what placement reads.
type span struct {
	team     string
	from, to time.Time
}

func spansOf(t *testing.T, db *DB, dev string) []span {
	t.Helper()
	var out []span
	for _, r := range membershipOf(t, db, dev) {
		out = append(out, span{r.Team, r.ValidFrom, r.ValidTo})
	}
	return out
}

func assertSpans(t *testing.T, db *DB, dev string, want ...span) {
	t.Helper()
	if got := spansOf(t, db, dev); !reflect.DeepEqual(got, want) {
		t.Errorf("%s rows = %+v, want %+v", dev, got, want)
	}
}

// TestAliasWrites_AppendAtTheClock pins the write side on one person: an alias
// create gives the alias its person's placement from the write on; a re-point
// moves it to the new person's placement and a delete ends it, each at the
// clock; and a hierarchy write on the canonical moves every alias with it.
// Nothing written before a write's instant changes.
func TestAliasWrites_AppendAtTheClock(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	setClock(db, memT1)
	for dev, team := range map[string]string{"alice": "alpha", "bob": "beta"} {
		if err := db.UpsertHierarchy(ctx, dev, team, "", "acme", "fp"); err != nil {
			t.Fatal(err)
		}
	}
	t2, t4, t5 := memT2, memT3.AddDate(0, 1, 0), memT3.AddDate(0, 2, 0)

	setClock(db, t2)
	if err := db.UpsertDeveloperAlias(ctx, "u1", "alice", "fp"); err != nil {
		t.Fatal(err)
	}
	assertSpans(t, db, "u1", span{"alpha", t2, time.Time{}})

	setClock(db, memT3)
	if err := db.UpsertDeveloperAlias(ctx, "u1", "bob", "fp"); err != nil {
		t.Fatal(err)
	}
	assertSpans(t, db, "u1", span{"alpha", t2, memT3}, span{"beta", memT3, time.Time{}})

	setClock(db, t4)
	if err := db.UpsertHierarchy(ctx, "bob", "gamma", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	assertSpans(t, db, "u1", span{"alpha", t2, memT3}, span{"beta", memT3, t4}, span{"gamma", t4, time.Time{}})

	setClock(db, t5)
	if found, err := db.DeleteDeveloperAlias(ctx, "u1", "fp"); err != nil || !found {
		t.Fatalf("DeleteDeveloperAlias = %v, %v", found, err)
	}
	assertSpans(t, db, "u1", span{"alpha", t2, memT3}, span{"beta", memT3, t4}, span{"gamma", t4, t5})
	// The people themselves only ever moved by their own hierarchy writes.
	assertSpans(t, db, "alice", span{"alpha", memT1, time.Time{}})
	assertSpans(t, db, "bob", span{"beta", memT1, t4}, span{"gamma", t4, time.Time{}})
}

// TestAliasWrites_PersonPlacementPrefersTheCanonical (#914 cases 3 and 5): with
// no row of its own the person takes an alias's org_hierarchy placement (the
// ascending-first alias that has one), and once the canonical is assigned its
// own placement outranks every alias's older row — for all of the person's ids,
// from that write on.
func TestAliasWrites_PersonPlacementPrefersTheCanonical(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	setClock(db, memT1)
	for dev, team := range map[string]string{"aaron-gh": "alpha", "zed-gh": "sre"} {
		if err := db.UpsertHierarchy(ctx, dev, team, "", "acme", "fp"); err != nil {
			t.Fatal(err)
		}
	}
	setClock(db, memT2)
	for _, a := range []string{"aaron-gh", "zed-gh"} {
		if err := db.UpsertDeveloperAlias(ctx, a, "zoe", "fp"); err != nil {
			t.Fatal(err)
		}
	}
	// zoe inherits aaron-gh's alpha (ascending-first alias with a row), and so
	// does zed-gh, from the alias write.
	assertSpans(t, db, "zoe", span{"alpha", memT2, time.Time{}})
	assertSpans(t, db, "zed-gh", span{"sre", memT1, memT2}, span{"alpha", memT2, time.Time{}})

	setClock(db, memT3)
	if err := db.UpsertHierarchy(ctx, "zoe", "beta", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	for _, dev := range []string{"zoe", "aaron-gh", "zed-gh"} {
		got := spansOf(t, db, dev)
		if last := got[len(got)-1]; last.team != "beta" || !last.from.Equal(memT3) || !last.to.IsZero() {
			t.Errorf("%s rows = %+v, want an open beta row from the canonical's move", dev, got)
		}
	}
	assertSpans(t, db, "aaron-gh", span{"alpha", memT1, memT3}, span{"beta", memT3, time.Time{}})
}

// TestAliasWrites_ClockBehindRefusesTheWholeWrite: an alias write whose clock
// reads earlier than a row it would close is refused, and the alias map is left
// as it was — the edit and its rows are one transaction.
func TestAliasWrites_ClockBehindRefusesTheWholeWrite(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	setClock(db, memT2)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertHierarchy(ctx, "u1", "beta", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	setClock(db, memT1)
	err := db.UpsertDeveloperAlias(ctx, "u1", "alice", "fp")
	if err == nil || !errors.Is(err, ErrMembershipClockBehind) {
		t.Fatalf("alias write with the clock behind = %v, want ErrMembershipClockBehind", err)
	}
	if aliases, _ := db.DeveloperAliases(ctx); len(aliases) != 0 {
		t.Errorf("aliases after the refused write = %v, want none", aliases)
	}
}

// legacyAliasStore builds a store as #912 left it — aliases written with no
// membership rows — and reopens it so the #914 migration runs. rows are
// (developer, team, from, to) with a zero to for the open row.
func legacyAliasStore(t *testing.T, aliases map[string]string, orgs map[string]string, rows []MembershipRow) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for dev, team := range orgs {
		if _, err := db.db.Exec(`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, ?, '', 'acme')`, dev, team); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		res, err := db.db.Exec(`INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES (?, ?, '', ?, 'fp')`, r.Developer, r.Team, r.ValidFrom)
		if err != nil {
			t.Fatal(err)
		}
		if !r.ValidTo.IsZero() {
			id, _ := res.LastInsertId()
			if _, err := db.db.Exec(`UPDATE hierarchy_membership SET valid_to = ? WHERE id = ?`, r.ValidTo, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for a, c := range aliases {
		if _, err := db.db.Exec(`INSERT INTO developer_alias (alias, canonical) VALUES (?, ?)`, a, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = ?`, MigrationAliasMembershipHistory); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// TestCopyAliasMembershipHistory_CopiesTheFullHistory: a legacy alias with no
// rows receives its person's WHOLE dated history — the move included, not a
// single baseline row — and the canonical's own rows are untouched. A second
// pass without the marker changes nothing.
func TestCopyAliasMembershipHistory_CopiesTheFullHistory(t *testing.T) {
	base := MembershipBaselineFrom
	db, path := legacyAliasStore(t,
		map[string]string{"m-gh": "m.os"},
		map[string]string{"m.os": "beta"},
		[]MembershipRow{
			{Developer: "m.os", Team: "alpha", ValidFrom: base, ValidTo: memT1},
			{Developer: "m.os", Team: "beta", ValidFrom: memT1},
		})
	want := []span{{"alpha", base, memT1}, {"beta", memT1, time.Time{}}}
	assertSpans(t, db, "m-gh", want...)
	assertSpans(t, db, "m.os", want...)
	var by string
	if err := db.db.QueryRow(`SELECT written_by FROM hierarchy_membership WHERE developer = 'm-gh' LIMIT 1`).Scan(&by); err != nil || by != MembershipWriterAliasHistory {
		t.Errorf("copied row written_by = %q (%v), want %q", by, err, MembershipWriterAliasHistory)
	}

	before, err := db.HierarchyMembership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = ?`, MigrationAliasMembershipHistory); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	after, err := db2.HierarchyMembership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a second pass changed the rows:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestCopyAliasMembershipHistory_KeepsTheResolvedPastAndReconcilesNow: #886
// placed a person by the ascending-first raw id covering each instant, so
// aaron-gh's open alpha row outranked zoe's later beta row. The migration
// writes that resolved past into both ids unchanged, then moves both to the
// person's current placement (zoe's own beta) at the upgrade instant — the
// past windows read as before, and later activity scores under beta.
func TestCopyAliasMembershipHistory_KeepsTheResolvedPastAndReconcilesNow(t *testing.T) {
	base := MembershipBaselineFrom
	start := time.Now().UTC()
	db, _ := legacyAliasStore(t,
		map[string]string{"aaron-gh": "zoe"},
		map[string]string{"aaron-gh": "alpha", "zoe": "beta"},
		[]MembershipRow{
			{Developer: "aaron-gh", Team: "alpha", ValidFrom: base},
			{Developer: "zoe", Team: "beta", ValidFrom: memT1},
		})
	for _, dev := range []string{"aaron-gh", "zoe"} {
		got := spansOf(t, db, dev)
		if len(got) != 2 || got[0].team != "alpha" || !got[0].from.Equal(base) ||
			got[1].team != "beta" || !got[1].to.IsZero() || !got[0].to.Equal(got[1].from) ||
			got[1].from.Before(start) {
			t.Errorf("%s rows = %+v, want alpha from the baseline to the upgrade, then open beta from it", dev, got)
		}
	}
}

// TestCopyAliasMembershipHistory_KeepsEachRowsWriter pins the #886 writer
// invariant through the rewrite: a span an id already held with the same
// placement keeps that row's written_by; only a span copied from another raw id
// is written as MembershipWriterAliasHistory. a-gh (ascending first) outranks
// m.os's alpha before memT1, so m.os is rewritten — its own alpha and beta spans
// must still read 'fp'.
func TestCopyAliasMembershipHistory_KeepsEachRowsWriter(t *testing.T) {
	base := MembershipBaselineFrom
	db, _ := legacyAliasStore(t,
		map[string]string{"a-gh": "m.os"},
		map[string]string{"m.os": "beta"},
		[]MembershipRow{
			{Developer: "a-gh", Team: "gamma", ValidFrom: base, ValidTo: memT1},
			{Developer: "m.os", Team: "alpha", ValidFrom: base, ValidTo: memT2},
			{Developer: "m.os", Team: "beta", ValidFrom: memT2},
		})
	want := []span{{"gamma", base, memT1}, {"alpha", memT1, memT2}, {"beta", memT2, time.Time{}}}
	assertSpans(t, db, "a-gh", want...)
	assertSpans(t, db, "m.os", want...)
	copied := MembershipWriterAliasHistory
	for dev, wantBy := range map[string][]string{
		"m.os": {copied, "fp", "fp"},
		"a-gh": {"fp", copied, copied},
	} {
		var got []string
		rows, err := db.db.Query(`SELECT written_by FROM hierarchy_membership WHERE developer = ? ORDER BY valid_from, id`, dev)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var by string
			if err := rows.Scan(&by); err != nil {
				t.Fatal(err)
			}
			got = append(got, by)
		}
		_ = rows.Close()
		if !reflect.DeepEqual(got, wantBy) {
			t.Errorf("%s written_by = %q, want %q", dev, got, wantBy)
		}
	}
}
