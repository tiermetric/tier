package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// What `tierd verify-report` needs to check a sealed month (#913) without a
// sealer: every function here reads, and none can seal.

// ErrSealedRefoldDiffers is RefoldSealed's answer when a sealed month's stored
// fold inputs refold to rows or a suppression its stored body does not carry.
var ErrSealedRefoldDiffers = errors.New("stored fold inputs do not refold to the stored body")

// ErrSealedRuleNotRefolded is RefoldSealed's refusal of a month sealed under a
// fold rule other than this binary's: another rule's inputs refold to rows the
// month's body was never folded by.
var ErrSealedRuleNotRefolded = errors.New("sealed under a fold rule this binary does not refold")

// ParsePeriod reads a period the way ?period= does.
func ParsePeriod(s string) (Period, error) { return parsePeriod(s) }

// SealedFoldRuleOf is the fold rule whose config digest over level, size and k
// is digest; ok is false when no rule this binary knows yields it.
func SealedFoldRuleOf(level, size string, k int, digest string) (rule int, ok bool) {
	for r := 1; r <= sealFoldRule; r++ {
		if sealConfigDigestOf(r, level, size, k) == digest {
			return r, true
		}
	}
	return 0, false
}

// RefoldSealed is SealReport's in-transaction refold check run against a stored
// month: rep's fold inputs, refolded at rep.K, must yield exactly the body's
// team rows, or exactly the kanon_suppressed the anonymised handler writes when
// the residual was withheld (#864 then drops the rows): the refold's count and
// floor, withheld_total and withheld_teams true, the other two flags false. It
// checks neither total nor data_quality's other fields, which the fold does not
// produce.
func RefoldSealed(rep store.SealedReport, rollups []store.SealedRollup, persons []store.SealedPersonKey) error {
	if rep.ConfigDigest != sealConfigDigestOf(sealFoldRule, rep.Level, rep.PeriodSize, rep.K) {
		return ErrSealedRuleNotRefolded
	}
	inputs, err := foldInputsOf(rollups, persons)
	if err != nil {
		return err
	}
	var body struct {
		Teams       json.RawMessage `json:"teams"`
		DataQuality *struct {
			KAnonSuppressed *kanonSuppressedJSON `json:"kanon_suppressed"`
		} `json:"data_quality"`
	}
	if err := json.Unmarshal(rep.Body, &body); err != nil {
		return fmt.Errorf("decode sealed body: %w", err)
	}
	var gotSup *kanonSuppressedJSON
	if body.DataQuality != nil {
		gotSup = body.DataQuality.KAnonSuppressed
	}
	gotTeams := body.Teams
	if gotTeams == nil {
		gotTeams = json.RawMessage("null")
	}
	teams, sup := scoring.AggregateFolded(inputs, rep.K)
	want := []byte("null")
	if !sup.Any() {
		if want, err = json.Marshal(teamRows(teams)); err != nil {
			return err
		}
	}
	switch {
	case !bytes.Equal(gotTeams, want):
		return fmt.Errorf("%w: the refolded team rows differ from the body's", ErrSealedRefoldDiffers)
	case sup.Any() != (gotSup != nil):
		return fmt.Errorf("%w: the refold withholds the residual %t, the body %t", ErrSealedRefoldDiffers, sup.Any(), gotSup != nil)
	case gotSup != nil && (gotSup.Developers != sup.Developers || gotSup.KAnonymity != sup.K):
		return fmt.Errorf("%w: the refold withholds %d at k=%d, the body %d at k=%d", ErrSealedRefoldDiffers,
			sup.Developers, sup.K, gotSup.Developers, gotSup.KAnonymity)
	case gotSup != nil && (!gotSup.WithheldTotal || !gotSup.WithheldTeams ||
		gotSup.WithheldCostComposition || gotSup.WithheldSegmentReconciliation):
		return fmt.Errorf("%w: the body's withheld flags are total=%t teams=%t cost_composition=%t segment_reconciliation=%t, "+
			"want true true false false", ErrSealedRefoldDiffers, gotSup.WithheldTotal, gotSup.WithheldTeams,
			gotSup.WithheldCostComposition, gotSup.WithheldSegmentReconciliation)
	}
	return nil
}

// RecomputeSealedBody computes period p's body from the rows as they are now,
// the way a first seal would (every read through one snapshot), and seals
// nothing: a verifier reports whether a seal today would differ, never changes
// what was sealed.
func (h *Handler) RecomputeSealedBody(ctx context.Context, p Period) ([]byte, error) {
	start, end := p.Bounds()
	var resp scoresResponse
	err := h.store.ReadSnapshot(ctx, func(snap *store.Snapshot) error {
		var err error
		resp, _, err = h.scoresForWindow(ctx, snap, scoresQuery{since: start, until: end, scope: store.FleetWide}, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(resp)
}
