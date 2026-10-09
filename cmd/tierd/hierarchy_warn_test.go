package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// fakeTeamStore returns canned developer→team and developer→division maps (or an
// error) for the empty-hierarchy startup tripwire (#232, #270). divisions is
// consulted only in division mode; leaving it nil models "no division assigned".
type fakeTeamStore struct {
	teams     map[string]string
	divisions map[string]string
	err       error
}

func (f fakeTeamStore) TeamsForDevelopers(context.Context) (map[string]string, error) {
	return f.teams, f.err
}

func (f fakeTeamStore) DivisionsForDevelopers(context.Context) (map[string]string, error) {
	return f.divisions, f.err
}

// TestCheckEmptyTeamHierarchy pins the #232 startup tripwire predicate across the
// four states: it fires ONLY in team mode with an empty hierarchy, is silent in
// developer mode and when the hierarchy is populated, and treats a query error as
// non-firing (surfaced, but never blocks serve).
func TestCheckEmptyTeamHierarchy(t *testing.T) {
	cases := []struct {
		name         string
		mode         scoring.AggregationMode
		store        fakeTeamStore
		wantFired    bool
		wantLogParts []string
	}{
		{
			name:         "team mode + empty hierarchy -> fires",
			mode:         scoring.AggregationTeam,
			store:        fakeTeamStore{teams: map[string]string{}},
			wantFired:    true,
			wantLogParts: []string{"names no team", "aggregate into 'other'", "#232"},
		},
		{
			name:      "team mode + populated hierarchy -> silent",
			mode:      scoring.AggregationTeam,
			store:     fakeTeamStore{teams: map[string]string{"alice": "eng"}},
			wantFired: false,
		},
		{
			// #270: team is NOT NULL but division IS. A hierarchy with every team
			// populated yet every division blank has a non-empty team map, so the
			// OLD table-empty probe stayed silent — but division mode collapses
			// everyone to "other". The level-specific probe must fire here.
			name:         "division mode + teams set but divisions all blank -> fires",
			mode:         scoring.AggregationDivision,
			store:        fakeTeamStore{teams: map[string]string{"alice": "eng"}, divisions: map[string]string{"alice": ""}},
			wantFired:    true,
			wantLogParts: []string{"names no division", "aggregate into 'other'", "#232"},
		},
		{
			name:      "division mode + a named division -> silent",
			mode:      scoring.AggregationDivision,
			store:     fakeTeamStore{divisions: map[string]string{"alice": "engineering"}},
			wantFired: false,
		},
		{
			name:      "developer mode -> silent regardless of hierarchy",
			mode:      scoring.AggregationDeveloper,
			store:     fakeTeamStore{teams: map[string]string{}},
			wantFired: false,
		},
		{
			name:         "team mode + query error -> not fired, error surfaced",
			mode:         scoring.AggregationTeam,
			store:        fakeTeamStore{err: errors.New("db down")},
			wantFired:    false,
			wantLogParts: []string{"could not read org_hierarchy"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, buf := warnCapture()
			got := checkEmptyTeamHierarchy(context.Background(), c.mode, c.store, logger)
			if got != c.wantFired {
				t.Errorf("checkEmptyTeamHierarchy fired = %v, want %v", got, c.wantFired)
			}
			out := buf.String()
			for _, part := range c.wantLogParts {
				if !strings.Contains(out, part) {
					t.Errorf("WARN log missing %q:\n%s", part, out)
				}
			}
			if len(c.wantLogParts) == 0 && buf.Len() != 0 {
				t.Errorf("expected no WARN, got: %q", out)
			}
		})
	}
}
