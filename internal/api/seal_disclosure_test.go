package api

import (
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestSealDisclosureLegacyUnknownAndConditionalStatement(t *testing.T) {
	open, closed := true, false
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		delay time.Duration
		open  *bool
		want  string
	}{
		{"legacy_short_delay", 14 * 24 * time.Hour, nil, "not_recorded"},
		{"legacy_long_delay", 60 * 24 * time.Hour, nil, "closed"},
		{"recorded_open", 14 * 24 * time.Hour, &open, "open"},
		{"recorded_closed", 14 * 24 * time.Hour, &closed, "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := sealDisclosureOf(store.SealedReport{Level: "developer", PeriodEnd: end, SealedAt: end.Add(tc.delay), RevertWindowsOpen: tc.open})
			if d.ConfiguredGraceSeconds != nil || d.GraceStatus != "not_recorded" || d.RevertWindowsAtSeal != tc.want {
				t.Fatalf("invented legacy configuration: %+v", d)
			}
			hasStatement := strings.Contains(d.Note, "were still open")
			if hasStatement != (tc.want == "open") {
				t.Fatalf("conditional statement disagrees with evidence: %+v", d)
			}
		})
	}
}

func TestSealDisclosureAnonymizedWithholdsObservedStatus(t *testing.T) {
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	grace := int64(14 * 24 * time.Hour)
	for _, level := range []string{"team", "org"} {
		for _, open := range []bool{true, false} {
			d := sealDisclosureOf(store.SealedReport{Level: level, PeriodEnd: end, SealedAt: end.Add(45 * 24 * time.Hour), GraceNS: &grace, RevertWindowsOpen: &open})
			if d.RevertWindowsAtSeal != "withheld" || d.Note != sealGraceNote || d.DaysAfterPeriodEnd != 45 || d.ConfiguredGraceSeconds == nil || *d.ConfiguredGraceSeconds != 1209600 {
				t.Errorf("anonymized disclosure reveals observations or loses timing: %+v", d)
			}
		}
	}
}
