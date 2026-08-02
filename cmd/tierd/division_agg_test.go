package main

import (
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// TestResolveAggregationMode_Division proves the #270 division level is a valid,
// exact-match --aggregation value resolving to scoring.AggregationDivision, and
// that the required-config / rejection error messages now name it too.
func TestResolveAggregationMode_Division(t *testing.T) {
	m, err := resolveAggregationMode("division")
	if err != nil || m != scoring.AggregationDivision {
		t.Fatalf("resolveAggregationMode(division) = (%v, %v), want (AggregationDivision, nil)", m, err)
	}

	// The empty-value required-config error must guide the operator to all three
	// levels now that division exists.
	_, emptyErr := resolveAggregationMode("")
	if emptyErr == nil {
		t.Fatal("empty --aggregation must be an error")
	}
	for _, want := range []string{"team", "developer", "division"} {
		if !strings.Contains(emptyErr.Error(), want) {
			t.Errorf("required-config error missing %q; got: %v", want, emptyErr)
		}
	}

	// Exact-match only: near-misses are still rejected, never coerced.
	for _, bad := range []string{"Division", "divisions", "div", " division"} {
		if _, err := resolveAggregationMode(bad); err == nil {
			t.Errorf("resolveAggregationMode(%q) must error", bad)
		}
	}
}
