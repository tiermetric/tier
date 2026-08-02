package store

import (
	"context"
	"testing"
)

// TestDivisionsForDevelopers mirrors TestTeamsForDevelopers for the #270 division
// rollup: an empty table yields a non-nil empty map; a populated table returns one
// entry per developer keyed to its division; a NULL or empty-string division is a
// present key with value "" (COALESCEd, distinct from an absent developer), so the
// k-anon fold treats it as the unnamed group and rolls it into "other"; a developer
// with no org_hierarchy row is omitted entirely.
func TestDivisionsForDevelopers(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Empty table -> non-nil empty map (the handler indexes it without a nil-check).
	empty, err := db.DivisionsForDevelopers(ctx)
	if err != nil {
		t.Fatalf("DivisionsForDevelopers (empty): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty org_hierarchy: got %v, want non-nil empty map", empty)
	}

	// alice/bob in "engineering"; cara in "research"; dan has division '' (empty);
	// erin has NULL division (column omitted on insert) -- both must read back as "".
	seeds := []struct {
		dev, team, div string
	}{
		{"alice", "platform", "engineering"},
		{"bob", "infra", "engineering"},
		{"cara", "ml", "research"},
		{"dan", "solo", ""},
	}
	for _, r := range seeds {
		if _, err := db.db.Exec(
			`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, ?, ?, 'acme')`,
			r.dev, r.team, r.div,
		); err != nil {
			t.Fatalf("seed org_hierarchy(%s): %v", r.dev, err)
		}
	}
	// erin: NULL division (column omitted) -- proves COALESCE(division,'') on read.
	if _, err := db.db.Exec(
		`INSERT INTO org_hierarchy (developer, team, org) VALUES ('erin', 'ops', 'acme')`,
	); err != nil {
		t.Fatalf("seed org_hierarchy(erin, NULL division): %v", err)
	}

	got, err := db.DivisionsForDevelopers(ctx)
	if err != nil {
		t.Fatalf("DivisionsForDevelopers: %v", err)
	}
	want := map[string]string{
		"alice": "engineering",
		"bob":   "engineering",
		"cara":  "research",
		"dan":   "", // stored empty string
		"erin":  "", // NULL COALESCEd to ""
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(want), want)
	}
	for dev, div := range want {
		if got[dev] != div {
			t.Errorf("division[%s] = %q, want %q", dev, got[dev], div)
		}
	}
	// A developer with no row is absent (not "" -- distinct from a stored empty).
	if _, present := got["ghost"]; present {
		t.Errorf("developer with no org_hierarchy row must be absent from the map; got %v", got)
	}
	// dan/erin are PRESENT keys with value "" -- distinct from absence.
	if _, present := got["dan"]; !present {
		t.Errorf("empty-string division must be a present key, not absent")
	}
}
