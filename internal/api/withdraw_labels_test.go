package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
)

func TestUnvalidatedBoundsWireLabels(t *testing.T) {
	h, db := newTestHandler(t)
	seedRankedDeveloper(t, db, "alice", "wa", 5, 0.5, 2, winAInstant)
	seedRankedDeveloper(t, db, "alice", "wb", 5, 5, 2, winBInstant)
	for _, path := range []string{"/api/v1/scores", "/api/v1/scores/alice", compareURL()} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			switch path {
			case "/api/v1/scores":
				h.handleGetScores(rec, req)
			case "/api/v1/scores/alice":
				req.SetPathValue("developer", "alice")
				h.handleGetDeveloperScore(rec, req)
			default:
				h.handleGetScoresCompare(rec, req)
			}
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("Tier-Inference") != "not_established" || rec.Header().Get("Tier-Bounds-Status") != "unvalidated_descriptive" {
				t.Errorf("bounds/inference interpretation missing: %v", rec.Header())
			}
		})
	}
	// Compatibility: this is only the raw non-overlap result, never an inference verdict.
	_, resp := getCompare(t, h, compareURL())
	if !devDelta(t, resp, "alice").Significant {
		t.Fatal("legacy raw non-overlap field changed")
	}
}

func TestSealDisclosureAtSealing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome time.Time
		want    string
	}{
		{"late_outcome_delayed_seal", time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), "withheld"},
		{"closed", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), "withheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, db, s := newSealedReadHandler(t)
			raw := rawSealStore(t, db)
			// Keep the established fold fixture, moving only occurrence times.
			if _, err := raw.Exec(`UPDATE outcomes SET ts = ?`, tc.outcome.Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			sealPass(t, s)
			before := sealedGet(t, h, "/api/v1/scores?period=2026-05")
			man := decodeJSON(t, sealedGet(t, h, "/api/v1/report_manifest?period=2026-05"))
			d, ok := man["seal_disclosure"].(map[string]any)
			if !ok {
				t.Fatalf("seal disclosure absent: %v", man)
			}
			if d["configured_grace_seconds"] != float64(14*24*60*60) || d["revert_windows_at_seal"] != tc.want || d["sealed_at"] != man["sealed_at"] {
				t.Errorf("wrong disclosure: %v", d)
			}
			if before.Header().Get("Tier-Report-Grace-Seconds") != "1209600" || before.Header().Get("Tier-Revert-Windows-At-Seal") != tc.want {
				t.Errorf("sealed score headers: %v", before.Header())
			}
			s.cfg.grace = 90 * 24 * time.Hour
			seedLateMayRow(t, db)
			again := sealedGet(t, h, "/api/v1/scores?period=2026-05")
			if !bytes.Equal(before.Body.Bytes(), again.Body.Bytes()) || again.Header().Get("Tier-Report-Grace-Seconds") != "1209600" {
				t.Fatal("read changed sealed bytes or substituted current grace")
			}
		})
	}
}

func TestSealDisclosureAtSealingOpenWithWallClock(t *testing.T) {
	sealNow := time.Now().UTC()
	end := time.Date(sealNow.Year(), sealNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	for sealNow.Before(end.Add(sealGrace)) {
		end = end.AddDate(0, -1, 0)
	}
	p := Period{Kind: periodMonth, Start: end.AddDate(0, -1, 0)}
	outcomeAt := end.Add(-time.Second)
	// The latest sealable month's last second is 14–45 days old, leaving ample
	// margin inside the 60-day window even though the store uses the real clock.
	h, db := newTestHandler(t)
	registerGated(t, db)
	seedKAnonDev(t, db, "singleton", "alice", 10, 1, outcomeAt)
	seedRepoCostAt(t, db, repoAlpha, "alice", "coverage", 1, p.Start.Add(-time.Second))
	h.SetAggregation(scoring.AggregationTeam, 5)
	s, err := newSealer(h, sealConfig{grace: sealGrace, sealFrom: p})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return sealNow }
	h.sealer = s
	sealPass(t, s)
	rep, err := db.SealedReport(context.Background(), p.Kind.String(), p.Start)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SealedAt.Before(end.Add(sealGrace)) || rep.SealedAt.After(outcomeAt.Add(60*24*time.Hour)) {
		t.Fatalf("fixture seal at %s is outside grace/open-window bounds for outcome %s", rep.SealedAt, outcomeAt)
	}
	if rep.RevertWindowsOpen == nil {
		t.Fatal("stored revert windows at seal = not recorded, want open")
	}
	if !*rep.RevertWindowsOpen {
		t.Fatalf("stored revert windows at seal = closed, want open (outcome %s, sealed %s)", outcomeAt, rep.SealedAt)
	}
	// Assert the stored observation without exposing it on the anonymized wire.
	scores := sealedGet(t, h, "/api/v1/scores?period="+p.String())
	if scores.Code != http.StatusOK || scores.Header().Get("Tier-Revert-Windows-At-Seal") != "withheld" {
		t.Fatalf("anonymized read: %d, headers %v", scores.Code, scores.Header())
	}
}

func TestSuppressedSealDisclosureWithholdsObservedStatus(t *testing.T) {
	h, db := newTestHandler(t)
	seedCompareGroups(t, db, []compareGroup{
		{"singleton", "m", 1, 1, 1, compareMay},
		{"singleton", "j", 1, 1, 1, compareJune},
	})
	h.SetAggregation(scoring.AggregationTeam, 5)
	s := newTestSealer(h)
	s.cfg.grace = 45 * 24 * time.Hour
	s.now = func() time.Time { return time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC) }
	h.sealer = s
	sealPass(t, s)
	scores := sealedGet(t, h, "/api/v1/scores?period=2026-05")
	if scores.Code != http.StatusOK || decodeJSON(t, scores)["data_quality"].(map[string]any)["kanon_suppressed"] == nil {
		t.Fatalf("control: scores are not suppressed: %d %s", scores.Code, scores.Body)
	}
	if scores.Header().Get("Tier-Revert-Windows-At-Seal") != "withheld" || scores.Header().Get("Tier-Seal-Note") != sealGraceNote {
		t.Errorf("suppressed scores disclose observed status: %v", scores.Header())
	}
	manifest := decodeJSON(t, sealedGet(t, h, "/api/v1/report_manifest?period=2026-05"))
	comparison := decodeJSON(t, compareGet(t, h, "?period_a=2026-05&period_b=2026-06"))
	if comparison["kanon_suppressed"] == nil {
		t.Fatalf("control: comparison is not suppressed: %v", comparison)
	}
	for _, d := range []any{manifest["seal_disclosure"], comparison["window_a"].(map[string]any)["seal_disclosure"], comparison["window_b"].(map[string]any)["seal_disclosure"]} {
		disclosure := d.(map[string]any)
		if disclosure["revert_windows_at_seal"] != "withheld" || disclosure["note"] != sealGraceNote || disclosure["configured_grace_seconds"] != float64(45*86400) || disclosure["sealed_at"] == "" {
			t.Errorf("suppressed response discloses observations or loses timing: %v", disclosure)
		}
	}
}
