package prderive

import (
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestSizeWeight_DefaultTable pins the built-in size-label -> weight mapping (the
// nil-table fallback). Consolidated from the former webhook.labelWeight and
// backfill.backfillLabelWeight unit tests, which pinned byte-identical copies of
// this table before #301 unified them here. Matching is case-insensitive and the
// first recognised label wins; an unrecognised label falls through to 0.
func TestSizeWeight_DefaultTable(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   float64
	}{
		{"xs slash", []string{"size/XS"}, 0.5},
		{"xs bare", []string{"xs"}, 0.5},
		{"s", []string{"size/S"}, 1.0},
		{"s bare", []string{"s"}, 1.0},
		{"m", []string{"size/M"}, 3.0},
		{"l", []string{"size/L"}, 5.0},
		{"xl", []string{"size/XL"}, 8.0},
		{"xl bare uppercase", []string{"XL"}, 8.0},
		{"first size wins", []string{"size/xl", "size/xs"}, 8.0},
		{"non-size label ignored, size still found", []string{"bug", "size/L"}, 5.0},
		{"no size label", []string{"enhancement"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SizeWeight(c.labels, nil); got != c.want {
				t.Errorf("SizeWeight(%v, nil) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
}

// TestSizeWeight_ConfiguredTable pins the #244 override semantics through the
// NormalizeSizeLabels -> SizeWeight pair both consumers use: a custom table
// REPLACES the defaults (a built-in name absent from the custom table no longer
// matches), keys and label names both fold case-insensitively, and the first
// matching label wins.
func TestSizeWeight_ConfiguredTable(t *testing.T) {
	table := NormalizeSizeLabels(map[string]float64{
		"size: xs": 0.5,
		"s":        1,
		"size-m":   3,
		"L":        5,
		"xxl":      8,
	})
	cases := []struct {
		name   string
		labels []string
		want   float64
	}{
		{"custom exact", []string{"size: xs"}, 0.5},
		{"case-insensitive value", []string{"SIZE-M"}, 3.0},
		{"case-insensitive key", []string{"l"}, 5.0},
		{"custom xxl", []string{"XXL"}, 8.0},
		{"first matching label wins", []string{"bug", "s"}, 1.0},
		{"builtin name not in custom table falls through", []string{"size/m"}, 0},
		{"unknown label falls through", []string{"enhancement"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SizeWeight(c.labels, table); got != c.want {
				t.Errorf("SizeWeight(%v, custom) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
}

// TestNormalizeSizeLabels_EmptyIsNil pins the load-bearing "use the defaults"
// signal (#244): both a nil and a non-nil EMPTY map normalize to nil, which
// SizeWeight then resolves against the built-in default table. config.Load yields a non-nil
// empty map for `size_labels: {}`, and the config docs promise `{}` means "use the
// defaults" — guarding only on nil would install an empty table that scores every
// PR by the git heuristic, the opposite of the documented behaviour.
func TestNormalizeSizeLabels_EmptyIsNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    map[string]float64
	}{
		{"nil", nil},
		{"non-nil empty", map[string]float64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := NormalizeSizeLabels(tc.m)
			if table != nil {
				t.Errorf("NormalizeSizeLabels(%s) = %v, want nil", tc.name, table)
			}
			if got := SizeWeight([]string{"size/l"}, table); got != 5.0 {
				t.Errorf("SizeWeight(size/l, %s) = %v, want 5 (must keep defaults)", tc.name, got)
			}
		})
	}
}

// TestWorkTypeFromLabels pins the label -> work-type derivation and its precedence
// (#187). Consolidated from the former webhook.workTypeFromLabels and
// backfill.backfillWorkType unit tests: exact and type:/kind: prefixed names match
// case-insensitively after trimming, highest-impact category wins on a multi-label
// tie, and no type label yields ('feature','default').
func TestWorkTypeFromLabels(t *testing.T) {
	cases := []struct {
		name       string
		labels     []string
		wantType   string
		wantSource string
	}{
		{"security exact", []string{"security"}, store.WorkTypeSecurity, store.WorkTypeSourceLabel},
		{"type:incident prefix", []string{"type:incident"}, store.WorkTypeIncident, store.WorkTypeSourceLabel},
		{"kind:research prefix", []string{"kind:research"}, store.WorkTypeResearch, store.WorkTypeSourceLabel},
		{"uppercase normalised", []string{"SECURITY"}, store.WorkTypeSecurity, store.WorkTypeSourceLabel},
		{"prefixed uppercase + spaces", []string{"Type: Tech-Debt"}, store.WorkTypeTechDebt, store.WorkTypeSourceLabel},
		{"feature label alone", []string{"feature"}, store.WorkTypeFeature, store.WorkTypeSourceLabel},
		{"no matching label", []string{"enhancement", "size/m"}, store.WorkTypeFeature, store.WorkTypeSourceDefault},
		{"empty labels", nil, store.WorkTypeFeature, store.WorkTypeSourceDefault},
		{"precedence security over feature", []string{"feature", "security"}, store.WorkTypeSecurity, store.WorkTypeSourceLabel},
		{"precedence bug over tech-debt", []string{"tech-debt", "bug"}, store.WorkTypeBug, store.WorkTypeSourceLabel},
		{"precedence incident over compliance", []string{"type:compliance", "incident"}, store.WorkTypeIncident, store.WorkTypeSourceLabel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotType, gotSource := WorkTypeFromLabels(c.labels)
			if gotType != c.wantType || gotSource != c.wantSource {
				t.Errorf("WorkTypeFromLabels(%v) = (%q,%q), want (%q,%q)",
					c.labels, gotType, gotSource, c.wantType, c.wantSource)
			}
		})
	}
}
