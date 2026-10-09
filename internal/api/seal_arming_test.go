package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// armState is what an arm left in the database: the sealed months in seal
// order and the pinned floor ("" when none).
func armState(t *testing.T, db *store.DB) (sealed, floor string) {
	t.Helper()
	raw := rawSealStore(t, db)
	if err := raw.QueryRow(`SELECT COALESCE(group_concat(period_start), '') FROM (SELECT period_start FROM sealed_report ORDER BY id)`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COALESCE(MAX(period_start), '') FROM seal_floor`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	return sealed, floor
}

func monthArg(t *testing.T, s string) Period {
	t.Helper()
	p, err := parsePeriod(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestArm_FirstSealPinsTheFloorThenReadsArmed (#913-D5 ruling C′): an arm seals
// the earliest sealable month itself and pins it; a second arm seals nothing and
// reports the pinned month. A month before coverage arms from coverage's first
// full month, as the background sealer's floor does.
func TestArm_FirstSealPinsTheFloorThenReadsArmed(t *testing.T) {
	for _, from := range []string{"2020-01", "2026-01", "2026-05"} {
		h, db, s := newSealFixture(t)
		s.cfg.sealFrom = monthArg(t, from)
		got, err := s.arm(context.Background(), "2026-05")
		if err != nil || got != (SealArm{Month: "2026-05"}) {
			t.Fatalf("arm from %s: %+v %v, want May sealed by this call", from, got, err)
		}
		if sealed, floor := armState(t, db); sealed != "2026-05-01T00:00:00Z" || floor != "2026-05-01T00:00:00Z" {
			t.Errorf("arm from %s left sealed %q, floor %q; want May and May", from, sealed, floor)
		}
		again, err := newTestSealer(h).arm(context.Background(), "2026-05")
		if err != nil || again != (SealArm{Pinned: "2026-05"}) {
			t.Errorf("second arm: %+v %v, want already armed at 2026-05", again, err)
		}
		if sealed, _ := armState(t, db); sealed != "2026-05-01T00:00:00Z" {
			t.Errorf("second arm sealed %q, want May alone", sealed)
		}
	}
}

// TestArm_DryRunWritesNothing: a dry run reports the month it would seal and
// writes no seal and no floor; once armed, a dry run reports the pinned month.
func TestArm_DryRunWritesNothing(t *testing.T) {
	_, db, s := newSealFixture(t)
	got, err := s.arm(context.Background(), "")
	if err != nil || got != (SealArm{Month: "2026-05"}) {
		t.Fatalf("dry run: %+v %v, want 2026-05", got, err)
	}
	if sealed, floor := armState(t, db); sealed != "" || floor != "" {
		t.Fatalf("dry run left sealed %q, floor %q; want nothing", sealed, floor)
	}
	if _, err := s.arm(context.Background(), "2026-05"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.arm(context.Background(), ""); err != nil || got != (SealArm{Pinned: "2026-05"}) {
		t.Errorf("dry run once armed: %+v %v, want already armed at 2026-05", got, err)
	}
}

// TestArm_Refusals: every precondition refuses with ErrSealArmRefused, names
// why, and writes nothing. A month not yet sealable names its sealable_at.
func TestArm_Refusals(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(t *testing.T) (SealArm, *store.DB, error)
		want string
	}{
		{"developer mode", func(t *testing.T) (SealArm, *store.DB, error) {
			h, db, s := newSealFixture(t)
			h.SetAggregation(scoring.AggregationDeveloper, 5)
			got, err := s.arm(context.Background(), "2026-05")
			return got, db, err
		}, "developer mode never seals"},
		{"malformed month", func(t *testing.T) (SealArm, *store.DB, error) {
			h, db, _ := newSealFixture(t)
			got, err := h.ArmSealing(context.Background(), sealGrace, "2026-5", "2026-05")
			return got, db, err
		}, "YYYY-MM"},
		{"grace below the minimum", func(t *testing.T) (SealArm, *store.DB, error) {
			h, db, _ := newSealFixture(t)
			got, err := h.ArmSealing(context.Background(), time.Hour, SealFromEarliest, "2026-05")
			return got, db, err
		}, "below the minimum"},
		{"no cost data", func(t *testing.T) (SealArm, *store.DB, error) {
			h, db := newTestHandler(t)
			h.SetAggregation(scoring.AggregationTeam, 5)
			got, err := newTestSealer(h).arm(context.Background(), "2026-05")
			return got, db, err
		}, "no cost data"},
		{"month later than the latest sealable", func(t *testing.T) (SealArm, *store.DB, error) {
			_, db, s := newSealFixture(t)
			s.cfg.sealFrom = monthArg(t, "2026-06")
			got, err := s.arm(context.Background(), "2026-06")
			return got, db, err
		}, "2026-06 is open or inside its grace lag until 2026-07-15T00:00:00Z"},
		{"month not sealable now (retention)", func(t *testing.T) (SealArm, *store.DB, error) {
			h, db, s := newSealFixture(t)
			h.SetRetentionHorizon(time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC))
			got, err := s.arm(context.Background(), "2026-05")
			return got, db, err
		}, "period is not sealable"},
		{"not the confirmed month", func(t *testing.T) (SealArm, *store.DB, error) {
			_, db, s := newSealFixture(t)
			got, err := s.arm(context.Background(), "2026-04")
			return got, db, err
		}, "the month to seal is now 2026-05, not the confirmed 2026-04"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, db, err := c.arm(t)
			if !errors.Is(err, ErrSealArmRefused) || !strings.Contains(err.Error(), c.want) || got != (SealArm{}) {
				t.Errorf("%+v %v; want ErrSealArmRefused naming %q", got, err, c.want)
			}
			if sealed, floor := armState(t, db); sealed != "" || floor != "" {
				t.Errorf("refusal left sealed %q, floor %q; want nothing", sealed, floor)
			}
		})
	}
}

// TestArm_MonthMovedDuringSealRefused: coverage that moves after the arm's
// check, inside its seal's snapshot, is refused with ErrSealArmRefused and
// writes nothing: a back-dated cost row making April the earliest month
// (errSealFloorMoved), and May becoming unsealable (errPeriodNotSealable).
func TestArm_MonthMovedDuringSealRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		move  func(h *Handler, db *store.DB)
		cause error
	}{
		{"back-dated cost moves the floor earlier", func(_ *Handler, db *store.DB) {
			seedRepoCostAt(t, db, repoAlpha, "b1", "i-backdated", 1, time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC))
		}, errSealFloorMoved},
		{"month made unsealable", func(h *Handler, _ *store.DB) {
			h.SetRetentionHorizon(time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC))
		}, errPeriodNotSealable},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, db, s := newSealFixture(t)
			moved := false
			s.inSnapshot = func() {
				if !moved {
					moved = true
					c.move(h, db)
				}
			}
			got, err := s.arm(context.Background(), "2026-05")
			if !moved || !errors.Is(err, ErrSealArmRefused) || !errors.Is(err, c.cause) || got != (SealArm{}) {
				t.Errorf("moved %v: %+v %v; want ErrSealArmRefused wrapping %v", moved, got, err, c.cause)
			}
			if sealed, floor := armState(t, db); sealed != "" || floor != "" {
				t.Errorf("refusal left sealed %q, floor %q; want nothing", sealed, floor)
			}
		})
	}
}

// TestArm_RacingServeThatWonReadsArmed: a background pass that seals first,
// between the arm's check and its seal, leaves the arm reporting the pinned
// month, never as armed by this call: when the pass sealed the arm's own month
// (the arm's insert loses) and when it sealed a later one (the next-month rule
// refuses the arm's).
func TestArm_RacingServeThatWonReadsArmed(t *testing.T) {
	aug1 := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		serveFrom, pinned string
	}{{"2026-05", "2026-05"}, {"2026-06", "2026-06"}} {
		h, db, s := newSealFixture(t)
		s.now = func() time.Time { return aug1 }
		serve := newTestSealer(h)
		serve.now = s.now
		serve.cfg.sealFrom = monthArg(t, c.serveFrom)
		raced := false
		s.beforeSeal = func() {
			if !raced {
				raced = true
				sealPass(t, serve)
			}
		}
		got, err := s.arm(context.Background(), "2026-05")
		if err != nil || got != (SealArm{Pinned: c.pinned}) {
			t.Errorf("serve from %s won: %+v %v, want already armed at %s", c.serveFrom, got, err, c.pinned)
		}
		if _, floor := armState(t, db); floor != c.pinned+"-01T00:00:00Z" {
			t.Errorf("serve from %s won: floor %q, want %s", c.serveFrom, floor, c.pinned)
		}
	}
}

// TestSealDue_RefusedAfterArmSealsAnEarlierMonth (#913-D5 ruling C′): a pass
// that planned July from its own seal_from while an arm seals May meanwhile is
// refused by the next-month rule instead of leaving June unsealed; the next
// pass seals June, then July.
func TestSealDue_RefusedAfterArmSealsAnEarlierMonth(t *testing.T) {
	sep1 := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	h, db, serve := newSealFixture(t)
	serve.now = func() time.Time { return sep1 }
	serve.cfg.sealFrom = monthArg(t, "2026-07")
	cli := newTestSealer(h)
	cli.now = serve.now
	armed := false
	serve.beforeSeal = func() {
		if !armed {
			armed = true
			if got, err := cli.arm(context.Background(), "2026-05"); err != nil || got != (SealArm{Month: "2026-05"}) {
				t.Errorf("arm inside the pass: %+v %v, want May", got, err)
			}
		}
	}
	if err := serve.sealDue(context.Background()); !errors.Is(err, store.ErrSealNotNext) {
		t.Fatalf("pass that planned July: %v, want store.ErrSealNotNext", err)
	}
	if sealed, floor := armState(t, db); sealed != "2026-05-01T00:00:00Z" || floor != "2026-05-01T00:00:00Z" {
		t.Fatalf("after the refused pass: sealed %q, floor %q; want May alone", sealed, floor)
	}
	sealPass(t, serve)
	if sealed, _ := armState(t, db); sealed != "2026-05-01T00:00:00Z,2026-06-01T00:00:00Z,2026-07-01T00:00:00Z" {
		t.Errorf("next pass sealed %q, want May, June, July in order", sealed)
	}
}

// TestArm_RacingEarlierFirstSealReadsArmed: another process's first seal of an
// earlier month, committed after the arm planned its own, leaves the arm
// reporting that pinned month and sealing nothing, though the arm's month is
// then the next one to seal.
func TestArm_RacingEarlierFirstSealReadsArmed(t *testing.T) {
	aug1 := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	h, db, s := newSealFixture(t)
	s.now = func() time.Time { return aug1 }
	s.cfg.sealFrom = monthArg(t, "2026-06")
	other := newTestSealer(h)
	other.now = s.now
	may := monthArg(t, "2026-05")
	raced := false
	s.beforeSeal = func() {
		if !raced {
			raced = true
			if _, _, _, err := other.sealFloored(context.Background(), may, may, false); err != nil {
				t.Errorf("racing first seal of May: %v", err)
			}
		}
	}
	got, err := s.arm(context.Background(), "2026-06")
	if err != nil || got != (SealArm{Pinned: "2026-05"}) {
		t.Errorf("arm of June after May was pinned: %+v %v, want already armed at 2026-05", got, err)
	}
	if sealed, floor := armState(t, db); sealed != "2026-05-01T00:00:00Z" || floor != "2026-05-01T00:00:00Z" {
		t.Errorf("sealed %q, floor %q; want May alone, pinned", sealed, floor)
	}
}
