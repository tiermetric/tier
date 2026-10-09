package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SealFromEarliest is the `tierd seal --arm` argument that arms sealing from
// the first full month of cost coverage.
const SealFromEarliest = "earliest"

// ErrSealArmRefused wraps every refusal ArmSealing makes before it seals:
// developer mode, a malformed month, no cost data, a first month that is not
// sealable now or that a configured source has not settled past (#913-D9), or
// one that is not the confirmed month, including one that moves while the arm
// seals. Nothing is sealed or pinned.
var ErrSealArmRefused = errors.New("sealing not armed")

// SealArm is an arm's outcome. Month is the month the arm seals first, which
// the seal pins as the earliest sealed month. Pinned is set instead when a
// floor was pinned already, by this or any other process: sealing was armed,
// and this call sealed nothing.
type SealArm struct {
	Month, Pinned string
}

// ParseSealMonth parses a seal_from value: one calendar month, YYYY-MM.
func ParseSealMonth(s string) (time.Time, error) {
	p, err := parsePeriod(s)
	if err != nil {
		return time.Time{}, err
	}
	return p.Start, nil
}

// ArmSealing performs sealing's first seal (#913-D5 ruling C′) through the
// seal path the background sealer uses, so the floor is pinned only inside a
// successful seal. from is a month (YYYY-MM) or SealFromEarliest, and the month
// sealed is the later of it and the first full month of cost coverage, as the
// background sealer's floor is. grace is serve's --report-grace. With confirm
// "" it is a dry run, returning the month it would seal and sealing nothing;
// otherwise it seals only when that month is confirm, and refuses when it is
// another. The seal is refused inside its transaction unless it is the first.
func (h *Handler) ArmSealing(ctx context.Context, grace time.Duration, from, confirm string) (SealArm, error) {
	cfg := sealConfig{grace: grace, sealFrom: Period{Kind: periodMonth, Start: time.Date(minPeriodYear, time.January, 1, 0, 0, 0, 0, time.UTC)}}
	if from != SealFromEarliest {
		p, err := parsePeriod(from)
		if err != nil {
			return refuseArm(err)
		}
		cfg.sealFrom = p
	}
	s, err := newSealer(h, cfg)
	if err != nil {
		return refuseArm(err)
	}
	return s.arm(ctx, confirm)
}

func refuseArm(err error) (SealArm, error) {
	return SealArm{}, fmt.Errorf("%w: %w", ErrSealArmRefused, err)
}

// arm is ArmSealing under s's config and clock.
func (s *sealer) arm(ctx context.Context, confirm string) (SealArm, error) {
	h := s.h
	if !h.aggregation.Anonymized() {
		return refuseArm(errSealDeveloperMode)
	}
	if arm, armed, err := h.pinnedArm(ctx); err != nil || armed {
		return arm, err
	}
	floor, ok, err := s.floor(ctx, h.store)
	if err != nil {
		return SealArm{}, err
	}
	if !ok {
		return refuseArm(errors.New("no cost data is recorded, so no month can be sealed"))
	}
	if _, err := s.sealable(ctx, h.store, floor); errors.Is(err, errPeriodNotSealable) {
		return refuseArm(err)
	} else if err != nil {
		return SealArm{}, err
	}
	if err := s.gate(ctx, h.store, floor); errors.Is(err, errSourceBehind) {
		return refuseArm(err)
	} else if err != nil {
		return SealArm{}, err
	}
	if confirm == "" {
		return SealArm{Month: floor.String()}, nil
	}
	if floor.String() != confirm {
		return refuseArm(fmt.Errorf("the month to seal is now %s, not the confirmed %s: cost coverage changed", floor, confirm))
	}
	_, _, won, err := s.sealFloored(ctx, floor, floor, true)
	if err == nil && won {
		return SealArm{Month: floor.String()}, nil
	}
	// This seal lost to, or was refused after, another process's first seal.
	if arm, armed, perr := h.pinnedArm(ctx); perr == nil && armed {
		return arm, nil
	}
	// Coverage that moved inside the seal's snapshot makes the month to seal
	// other than the one confirmed: a refusal, as the check before the seal is.
	if errors.Is(err, errSealFloorMoved) || errors.Is(err, errPeriodNotSealable) || errors.Is(err, errSourceBehind) {
		return refuseArm(err)
	}
	if err == nil {
		err = fmt.Errorf("%s was sealed by another process, but no floor is pinned", floor)
	}
	return SealArm{}, err
}

// pinnedArm is the arm a pinned floor records; armed is false when none is.
func (h *Handler) pinnedArm(ctx context.Context) (arm SealArm, armed bool, err error) {
	start, ok, err := h.store.SealFloor(ctx)
	if err != nil || !ok {
		return SealArm{}, false, err
	}
	return SealArm{Pinned: monthOf(start).String()}, true, nil
}
