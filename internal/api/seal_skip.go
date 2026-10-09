package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// ErrSealSkipRefused wraps every refusal SkipSealing makes: developer mode, a
// malformed month or reason, sealing not armed or nothing sealed yet, a month
// that is sealed, a gap, open, inside its grace lag or not the first owed
// month, or a failure whose category was not confirmed. No gap is recorded.
var ErrSealSkipRefused = errors.New("no gap recorded")

// ErrSealSkipTransient is SkipSealing's refusal when the re-attempted seal, or
// the gap's write, failed in a way a later pass can clear (sealFailure.transient).
// No gap is recorded.
var ErrSealSkipTransient = errors.New("the failure is one a later seal pass can clear, so no gap is recorded")

// SealSkip is a skip's outcome for Month. Sealed is set when the re-attempted
// seal sealed Month, so no gap is needed and none is recorded; otherwise
// Category and Words are the re-attempt's failure and Observed its error, for
// the operator's terminal only (a gap stores the category, never the error).
// A dry run's Category is "" when the seal's computation succeeded, since the
// failures SealReport finds cannot be learned without writing.
type SealSkip struct {
	Month           string
	Sealed          bool
	Category, Words string
	Observed        error
}

// SkipSealing records month (YYYY-MM) as a permanent gap with the operator's
// reason (#913-D6 ruling D), so the months after it seal. Only the first owed
// month is skipped, and only after its seal is attempted again through the
// background sealer's path and fails in a way no later pass clears. grace and
// sealFrom ("" when unset) are serve's --report-grace and seal_from. A dryRun
// writes nothing: the seal is computed in its snapshot and stopped before
// SealReport. Otherwise the seal is attempted for real, and a failure is
// returned without a gap unless record is set; record refuses unless confirm
// is the failure's category, so "" always refuses.
func (h *Handler) SkipSealing(ctx context.Context, grace time.Duration, sealFrom, month, reason string, dryRun, record bool, confirm string) (SealSkip, error) {
	cfg := sealConfig{grace: grace}
	if sealFrom != "" {
		p, err := parsePeriod(sealFrom)
		if err != nil {
			return refuseSkip(err)
		}
		cfg.sealFrom = p
	}
	s, err := newSealer(h, cfg)
	if err != nil {
		return refuseSkip(err)
	}
	s.dryRun = dryRun
	return s.skip(ctx, month, reason, record, confirm)
}

func refuseSkip(err error) (SealSkip, error) {
	return SealSkip{}, fmt.Errorf("%w: %w", ErrSealSkipRefused, err)
}

// skip is SkipSealing under s's config and clock.
func (s *sealer) skip(ctx context.Context, month, reason string, record bool, confirm string) (SealSkip, error) {
	h := s.h
	if !h.aggregation.Anonymized() {
		return refuseSkip(errSealDeveloperMode)
	}
	p, err := parsePeriod(month)
	if err != nil {
		return refuseSkip(err)
	}
	if err := store.CheckSealedGapReason(reason); err != nil {
		return refuseSkip(err)
	}
	next, floor, owed, err := s.firstOwed(ctx)
	switch {
	case errors.Is(err, errSealingNotArmed):
		return refuseSkip(err)
	case err != nil:
		return SealSkip{}, err
	case !owed || !floor.Start.IsZero():
		// Only a seal pins the earliest month (#913-D5 ruling C′), so the first
		// month is never a gap.
		return refuseSkip(errors.New("no month is sealed yet, and the first month sealed cannot be skipped: " +
			"arm sealing from a later month with `tierd seal --arm YYYY-MM`"))
	}
	if p.Start.Before(next.Start) {
		what := "precedes the earliest sealed month"
		if _, err := h.store.SealedReport(ctx, p.Kind.String(), p.Start); err == nil {
			what = "is sealed already"
		} else if !errors.Is(err, store.ErrSealedReportNotFound) {
			return SealSkip{}, err
		}
		if err := s.gapped(ctx, p); errors.Is(err, errSealGapped) {
			what = "is recorded as a gap already"
		} else if err != nil {
			return SealSkip{}, err
		}
		return refuseSkip(fmt.Errorf("%s %s; only the first owed month, %s, can be skipped", p, what, next))
	}
	if last := lastSealable(s.now(), s.cfg.grace); p.Start.After(last.Start) {
		return refuseSkip(fmt.Errorf("%s is open or inside its grace lag until %s", p, sealableAt(p, s.cfg.grace).Format(time.RFC3339)))
	}
	if !p.Start.Equal(next.Start) {
		return refuseSkip(fmt.Errorf("%s is not the first owed month: months are sealed oldest first, and %s is owed first", p, next))
	}

	_, _, _, err = s.sealFloored(ctx, p, Period{}, false)
	switch {
	case errors.Is(err, errSealDryRun):
		return SealSkip{Month: p.String()}, nil
	case err == nil:
		return SealSkip{Month: p.String(), Sealed: true}, nil
	}
	if errors.Is(err, store.ErrSealedPeriodGapped) {
		return refuseSkip(fmt.Errorf("another process recorded %s as a gap meanwhile: %w", p, err))
	}
	f := sealFailureOf(err)
	if f.transient {
		return SealSkip{}, fmt.Errorf("%w: %s (%w)", ErrSealSkipTransient, f.words, err)
	}
	plan := SealSkip{Month: p.String(), Category: f.code, Words: f.words, Observed: err}
	if s.dryRun || !record {
		return plan, nil
	}
	if confirm == "" {
		return refuseSkip(fmt.Errorf("the seal of %s fails with %s, and no category was confirmed", p, f.code))
	}
	if confirm != f.code {
		return refuseSkip(fmt.Errorf("the seal of %s now fails with %s, not the confirmed %s", p, f.code, confirm))
	}
	start, end := p.Bounds()
	build := h.buildIdentity()
	_, err = h.store.RecordSealedGap(ctx, store.SealedGap{
		PeriodSize: p.Kind.String(), PeriodStart: start, PeriodEnd: end, Category: f.code, Reason: reason,
		ToolVersion: build.Version, ToolCommit: build.Commit,
	})
	// A committed gap is reported even when ctx was cancelled after the commit,
	// and a refusal stays a refusal under a cancelled ctx.
	switch {
	case err == nil:
		return plan, nil
	case errors.Is(err, store.ErrSealedPeriodSealed), errors.Is(err, store.ErrSealedPeriodGapped), errors.Is(err, store.ErrSealNotNext):
		return refuseSkip(fmt.Errorf("another process sealed or skipped a month meanwhile: %w", err))
	case errors.Is(err, store.ErrWriteLockUnavailable), ctx.Err() != nil:
		return SealSkip{}, fmt.Errorf("%w: %w", ErrSealSkipTransient, err)
	default:
		return SealSkip{}, err
	}
}
