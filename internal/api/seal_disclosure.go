package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

var sealGraceNote = fmt.Sprintf("The default 14-day grace can leave %.0f-day revert windows open. Longer grace does not establish delivery completeness.", store.RevertObservationWindow.Hours()/24)

type sealDisclosureJSON struct {
	ScoringPolicy          string   `json:"scoring_policy"`
	CorrectionRuleVersion  string   `json:"correction_rule_version"`
	RubricVersion          *int     `json:"rubric_version"`
	CorrectionRevision     *int64   `json:"correction_revision"`
	SealedAt               string   `json:"sealed_at"`
	ConfiguredGraceSeconds *float64 `json:"configured_grace_seconds"`
	GraceStatus            string   `json:"grace_status"`
	DaysAfterPeriodEnd     float64  `json:"days_after_period_end"`
	RevertWindowsAtSeal    string   `json:"revert_windows_at_seal"`
	Note                   string   `json:"note"`
}

func sealDisclosureOf(r store.SealedReport) sealDisclosureJSON {
	d := sealDisclosureJSON{ScoringPolicy: "unknown", CorrectionRuleVersion: "unknown", RubricVersion: r.RubricVersion, CorrectionRevision: nil, SealedAt: r.SealedAt.UTC().Format(time.RFC3339), GraceStatus: "not_recorded", DaysAfterPeriodEnd: r.SealedAt.Sub(r.PeriodEnd).Hours() / 24, RevertWindowsAtSeal: "not_recorded", Note: sealGraceNote}
	if r.Level == "developer" {
		d.CorrectionRevision = r.CorrectionRevision
	}

	if r.ScoringPolicy != nil {
		d.ScoringPolicy = *r.ScoringPolicy
	}
	if r.CorrectionRuleVersion != nil {
		d.CorrectionRuleVersion = *r.CorrectionRuleVersion
	}

	if r.GraceNS != nil {
		seconds := float64(*r.GraceNS) / float64(time.Second)
		d.ConfiguredGraceSeconds = &seconds
		d.GraceStatus = "recorded"
	}
	// Outcome-dependent observations can identify a person in an anonymized cohort.
	if r.Level != "developer" {
		d.RevertWindowsAtSeal = "withheld"
		return d
	}
	if r.RevertWindowsOpen != nil {
		if *r.RevertWindowsOpen {
			d.RevertWindowsAtSeal = "open"
			d.Note = fmt.Sprintf("Some outcomes' %.0f-day revert windows were still open at sealing. ", store.RevertObservationWindow.Hours()/24) + d.Note
		} else {
			d.RevertWindowsAtSeal = "closed"
		}
	} else if !r.SealedAt.Before(r.PeriodEnd.Add(store.RevertObservationWindow)) {
		d.RevertWindowsAtSeal = "closed"
	}
	return d
}

func (d sealDisclosureJSON) setHeaders(h http.Header) {
	h.Set("Tier-Sealed-Scoring-Policy", d.ScoringPolicy)
	h.Set("Tier-Sealed-Correction-Rule", d.CorrectionRuleVersion)
	rubric := "unknown"
	if d.RubricVersion != nil {
		rubric = strconv.Itoa(*d.RubricVersion)
	}
	h.Set("Tier-Sealed-Rubric-Version", rubric)
	grace := "not_recorded"
	if d.ConfiguredGraceSeconds != nil {
		grace = strconv.FormatFloat(*d.ConfiguredGraceSeconds, 'f', -1, 64)
	}
	h.Set("Tier-Report-Grace-Seconds", grace)
	h.Set("Tier-Sealed-Days-After-End", fmt.Sprintf("%.6f", d.DaysAfterPeriodEnd))
	h.Set("Tier-Revert-Windows-At-Seal", d.RevertWindowsAtSeal)
	h.Set("Tier-Seal-Note", d.Note)
}
