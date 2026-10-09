package main

import (
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// TestResolveAggregationMode covers the REQUIRED-config fail-loud contract (#185):
// an empty value (unset from flag/env/config) is a hard error naming how to
// choose; "team"/"developer" resolve; anything else is rejected.
func TestResolveAggregationMode(t *testing.T) {
	t.Run("empty is a required-config error", func(t *testing.T) {
		_, err := resolveAggregationMode("")
		if err == nil {
			t.Fatal("empty --aggregation must be an error (no silent default, #185)")
		}
		// The message must guide the operator and flag the requirement.
		for _, want := range []string{"REQUIRED", "team", "developer"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error message missing %q; got: %v", want, err)
			}
		}
	})
	t.Run("team", func(t *testing.T) {
		m, err := resolveAggregationMode("team")
		if err != nil || m != scoring.AggregationTeam {
			t.Fatalf("resolveAggregationMode(team) = (%v, %v), want (AggregationTeam, nil)", m, err)
		}
	})
	t.Run("developer", func(t *testing.T) {
		m, err := resolveAggregationMode("developer")
		if err != nil || m != scoring.AggregationDeveloper {
			t.Fatalf("resolveAggregationMode(developer) = (%v, %v), want (AggregationDeveloper, nil)", m, err)
		}
	})
	// Exact-match only: case variants and stray values are rejected, never
	// silently coerced.
	for _, bad := range []string{"TEAM", "Developer", "teams", "dev", "none", " team"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := resolveAggregationMode(bad); err == nil {
				t.Errorf("resolveAggregationMode(%q) must error", bad)
			}
		})
	}
}

// TestValidateKAnonymity proves serve refuses a k below the hard minimum (3) and
// accepts the default (5) and the minimum itself (#185).
func TestValidateKAnonymity(t *testing.T) {
	for _, k := range []int{-1, 0, 1, 2} {
		if err := validateKAnonymity(k); err == nil {
			t.Errorf("validateKAnonymity(%d) must error (below hard minimum %d)", k, scoring.MinKAnonymity)
		}
	}
	for _, k := range []int{3, 5, 50} {
		if err := validateKAnonymity(k); err != nil {
			t.Errorf("validateKAnonymity(%d) = %v, want nil", k, err)
		}
	}
	// Pin the ruled values so a drift in the constants is caught here.
	if scoring.DefaultKAnonymity != 5 || scoring.MinKAnonymity != 3 {
		t.Errorf("k-anonymity constants drifted: default=%d min=%d, want default=5 min=3 (#185 ruling)",
			scoring.DefaultKAnonymity, scoring.MinKAnonymity)
	}
}
