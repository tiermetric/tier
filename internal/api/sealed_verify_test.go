package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// TestRefoldSealed_RealSealReproduces: a month sealed by the real sealer refolds
// from its stored inputs to its stored body, and a stored rollup edited after
// the seal does not.
func TestRefoldSealed_RealSealReproduces(t *testing.T) {
	ctx := context.Background()
	_, db, s := newSealFixture(t)
	if _, _, err := s.sealOrLoad(ctx, sealMay); err != nil {
		t.Fatal(err)
	}
	rep, err := db.SealedReport(ctx, "month", sealFixtureMonth)
	if err != nil {
		t.Fatal(err)
	}
	rollups, persons, err := db.SealedFoldInputs(ctx, rep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rollups) < 2 || len(persons) == 0 {
		t.Fatalf("control: %d rollups, %d person rows; the fixture must seal named rows", len(rollups), len(persons))
	}
	if err := RefoldSealed(rep, rollups, persons); err != nil {
		t.Fatalf("RefoldSealed over the real seal: %v", err)
	}
	if rule, ok := SealedFoldRuleOf(rep.Level, rep.PeriodSize, rep.K, rep.ConfigDigest); !ok || rule != sealFoldRule {
		t.Errorf("SealedFoldRuleOf(stored) = %d, %v; want %d, true", rule, ok, sealFoldRule)
	}

	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`DROP TRIGGER trg_sealed_rollup_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE sealed_rollup SET weighted_points = weighted_points + 1 WHERE report_id = ? AND label = ?`,
		rep.ID, rollups[0].Label); err != nil {
		t.Fatal(err)
	}
	edited, persons, err := db.SealedFoldInputs(ctx, rep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefoldSealed(rep, edited, persons); !errors.Is(err, ErrSealedRefoldDiffers) {
		t.Errorf("RefoldSealed after a stored rollup moved: %v, want ErrSealedRefoldDiffers", err)
	}

	other := rep
	other.ConfigDigest = sealConfigDigestOf(sealFoldRule+1, rep.Level, rep.PeriodSize, rep.K)
	if err := RefoldSealed(other, rollups, persons); !errors.Is(err, ErrSealedRuleNotRefolded) {
		t.Errorf("RefoldSealed under another rule's digest: %v, want ErrSealedRuleNotRefolded", err)
	}
	if _, ok := SealedFoldRuleOf(other.Level, other.PeriodSize, other.K, other.ConfigDigest); ok {
		t.Error("SealedFoldRuleOf knows a rule past sealFoldRule")
	}
}

// TestRefoldSealed_WithheldResidual: a body whose residual was withheld (#864)
// carries no rows and kanon_suppressed; it reproduces only with the refold's
// own count and floor, and never with rows.
func TestRefoldSealed_WithheldResidual(t *testing.T) {
	const k = 5
	paid := 4
	big := scoring.LabelInput{Label: "big", People: people("b", 6), Sums: scoring.RollupSums{TotalCostUSD: 60, WeightedPoints: 12, SampleN: 12}, Contributes: true}
	big.Carriers[0], big.Carriers[1] = big.People, big.People
	big.Has[0], big.Has[1] = true, true
	bots := scoring.LabelInput{Label: "bots", Sums: scoring.RollupSums{ActualPaidUSD: 100}, Contributes: true}
	bots.Has[paid] = true
	if scoring.MeasureNames[paid] != "paid" {
		t.Fatalf("measure %d is %q, want paid", paid, scoring.MeasureNames[paid])
	}
	inputs := []scoring.LabelInput{big, bots}
	_, sup := scoring.AggregateFolded(inputs, k)
	if !sup.Any() {
		t.Fatal("control: the fold must withhold the residual")
	}
	rs, ps, _ := sealRoundTrip(t, inputs, k)
	rep := store.SealedReport{Level: "team", PeriodSize: "month", K: k, ConfigDigest: sealConfigDigestOf(sealFoldRule, "team", "month", k)}
	withheld := func(devs, kk int) string {
		return fmt.Sprintf(`{"data_quality":{"kanon_suppressed":{"developers":%d,"k_anonymity":%d,"withheld_total":true,"withheld_teams":true}}}`, devs, kk)
	}
	for name, c := range map[string]struct {
		body string
		ok   bool
	}{
		"the served withhold":    {withheld(sup.Developers, sup.K), true},
		"another count":          {withheld(sup.Developers+1, sup.K), false},
		"another floor":          {withheld(sup.Developers, sup.K+1), false},
		"no declaration":         {`{"teams":null}`, false},
		"rows beside a withhold": {`{"teams":[{"team":"big"}],` + withheld(sup.Developers, sup.K)[1:], false},
	} {
		rep.Body = []byte(c.body)
		err := RefoldSealed(rep, rs, ps)
		if c.ok && err != nil || !c.ok && !errors.Is(err, ErrSealedRefoldDiffers) {
			t.Errorf("%s: RefoldSealed = %v, want ok=%v", name, err, c.ok)
		}
	}
}

// TestRefoldSealed_WithheldFlags: a withheld body with the refold's count and
// floor still differs when any withheld flag is not the one the anonymised
// handler writes (withheld_total and withheld_teams true, the other two false).
func TestRefoldSealed_WithheldFlags(t *testing.T) {
	const k = 5
	paid := 4
	big := scoring.LabelInput{Label: "big", People: people("b", 6), Sums: scoring.RollupSums{TotalCostUSD: 60, WeightedPoints: 12, SampleN: 12}, Contributes: true}
	big.Carriers[0], big.Carriers[1] = big.People, big.People
	big.Has[0], big.Has[1] = true, true
	bots := scoring.LabelInput{Label: "bots", Sums: scoring.RollupSums{ActualPaidUSD: 100}, Contributes: true}
	bots.Has[paid] = true
	inputs := []scoring.LabelInput{big, bots}
	_, sup := scoring.AggregateFolded(inputs, k)
	if !sup.Any() {
		t.Fatal("control: the fold must withhold the residual")
	}
	rs, ps, _ := sealRoundTrip(t, inputs, k)
	rep := store.SealedReport{Level: "team", PeriodSize: "month", K: k, ConfigDigest: sealConfigDigestOf(sealFoldRule, "team", "month", k)}
	body := func(total, teams, cost, seg bool) string {
		return fmt.Sprintf(`{"data_quality":{"kanon_suppressed":{"developers":%d,"k_anonymity":%d,"withheld_total":%t,`+
			`"withheld_cost_composition":%t,"withheld_teams":%t,"withheld_segment_reconciliation":%t}}}`,
			sup.Developers, sup.K, total, cost, teams, seg)
	}
	for name, c := range map[string]struct {
		body string
		ok   bool
	}{
		"the served flags":                {body(true, true, false, false), true},
		"total not withheld":              {body(false, true, false, false), false},
		"teams not withheld":              {body(true, false, false, false), false},
		"cost composition withheld":       {body(true, true, true, false), false},
		"segment reconciliation withheld": {body(true, true, false, true), false},
	} {
		rep.Body = []byte(c.body)
		err := RefoldSealed(rep, rs, ps)
		if c.ok && err != nil || !c.ok && !errors.Is(err, ErrSealedRefoldDiffers) {
			t.Errorf("%s: RefoldSealed = %v, want ok=%v", name, err, c.ok)
		}
	}
}

// TestRecomputeSealedBody_SealsNothing: recomputing a sealable, unsealed month
// through a handler that HAS a sealer returns the body a seal would store and
// leaves no sealed row, floor or fold input behind.
func TestRecomputeSealedBody_SealsNothing(t *testing.T) {
	ctx := context.Background()
	h, db, s := newSealedReadHandler(t)
	raw := rawSealStore(t, db)
	counts := func() string {
		return fmt.Sprint(rawCount(t, raw, `SELECT COUNT(*) FROM sealed_report`), rawCount(t, raw, `SELECT COUNT(*) FROM sealed_rollup`),
			rawCount(t, raw, `SELECT COUNT(*) FROM sealed_person`), rawCount(t, raw, `SELECT COUNT(*) FROM seal_floor`))
	}
	before := counts()
	got, err := h.RecomputeSealedBody(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	if after := counts(); after != before || before != "0 0 0 0" {
		t.Fatalf("sealed_report/rollup/person/floor rows %s -> %s, want 0 0 0 0 throughout", before, after)
	}
	sealed, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sealed) {
		t.Errorf("recomputed body differs from the sealed one with no row moved:\nrecomputed %s\nsealed     %s", got, sealed)
	}
	seedLateMayRow(t, db)
	if again, err := h.RecomputeSealedBody(ctx, sealMay); err != nil || bytes.Equal(again, sealed) {
		t.Errorf("after a late May row the recompute still equals the sealed body (err %v)", err)
	}
}
