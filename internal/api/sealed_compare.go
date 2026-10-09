package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// The sealed /scores/compare path (#913-D2 item 6): in an anonymised mode with a
// sealer, a comparison is of two sealed calendar months, refolded from their
// stored fold inputs. It never reads a live window and never differences two
// stored bodies: each body was floored alone, and #277's intersection (a label
// is named only when it clears the floor in BOTH months) needs the inputs.

// sealedCompareParams is /compare's sealed allowlist; it lists no
// freeBoundParams key (#913-D2 item 7).
var sealedCompareParams = []string{"period_a", "period_b"}

// Each compared month's period and seal time ride in headers, as a sealed
// /scores read's do; the install markers ride in sealMarkers.setHeaders'.
const (
	headerSealedPeriodA = "Tier-Period-A"
	headerSealedPeriodB = "Tier-Period-B"
	headerSealedAtA     = "Tier-Sealed-At-A"
	headerSealedAtB     = "Tier-Sealed-At-B"
)

// sealedConfigMismatchJSON is the 409 for two months sealed under different
// configs (#913-D1 ruling A): their rows split one population two ways, so they
// are never compared. It is also the 409 for two months sealed under a fold rule
// this binary does not refold.
type sealedConfigMismatchJSON struct {
	Error   string           `json:"error"`
	PeriodA string           `json:"period_a"`
	PeriodB string           `json:"period_b"`
	ConfigA sealedConfigJSON `json:"config_a"`
	ConfigB sealedConfigJSON `json:"config_b"`
}

// sealedSide is one compared month: its sealed report, its stored fold inputs,
// and the parts of its stored body a comparison serves.
type sealedSide struct {
	p      Period
	rep    store.SealedReport
	inputs []scoring.LabelInput
	stored struct {
		DataQuality *dataQualityJSON `json:"data_quality"`
		Total       *teamScoreJSON   `json:"total"`
	}
}

// sealed is nil when p is sealed, else the reason load gives.
func (s *sealer) sealed(ctx context.Context, p Period) error {
	_, _, err := s.load(ctx, p)
	return err
}

// loadSealedSide reads sealed month p and its stored fold inputs.
func (h *Handler) loadSealedSide(ctx context.Context, p Period) (sealedSide, error) {
	side := sealedSide{p: p}
	var err error
	if side.rep, err = h.store.SealedReport(ctx, p.Kind.String(), p.Start); err != nil {
		return sealedSide{}, err
	}
	if err := json.Unmarshal(side.rep.Body, &side.stored); err != nil {
		return sealedSide{}, fmt.Errorf("decode sealed %s body: %w", p, err)
	}
	rollups, persons, err := h.store.SealedFoldInputs(ctx, side.rep.ID)
	if err != nil {
		return sealedSide{}, err
	}
	side.inputs, err = foldInputsOf(rollups, persons)
	return side, err
}

// window is the side's compareWindowMeta: the month's bounds, and the live
// compare window's data_quality fields (dataQualityBlock's and withCostHorizon's)
// from the stored block. cost_coverage_safe_since is absent, as on the sealed
// body; the /scores-only fields and the month's own kanon_suppressed are never
// stated per window on /compare.
func (s sealedSide) window() compareWindowMeta {
	start, end := s.p.Bounds()
	w := compareWindowMeta{Since: start.Format("2006-01-02"), Until: end.Format("2006-01-02")}
	if dq := s.stored.DataQuality; dq != nil {
		w.DataQuality = &dataQualityJSON{
			ZeroTokenOutcomeCount:     dq.ZeroTokenOutcomeCount,
			AttributionCoverage:       dq.AttributionCoverage,
			ExcludesUnattributedSpend: dq.ExcludesUnattributedSpend,
			CostCoverageStart:         dq.CostCoverageStart,
			WindowPredatesCostCapture: dq.WindowPredatesCostCapture,
			SourceCoverageStart:       dq.SourceCoverageStart,
		}
	}
	return w
}

// total is the side of the comparison's total: the stored body's, so it equals
// /scores?period= for the month, or a zero side for a month with no rows.
func (s sealedSide) total() teamScoreJSON {
	if s.stored.Total == nil {
		return teamSideJSON(scoring.TeamScore{})
	}
	return *s.stored.Total
}

// serveSealedCompare answers /scores/compare from two sealed months: ?period_a=
// and ?period_b= together, a earlier than b, or neither for the two latest
// sealed months. An unsealed month is a 404 naming it, and a read never seals
// (#913-D4); two months sealed under different configs,
// or under a fold rule this binary does not refold, are a 409.
func (h *Handler) serveSealedCompare(w http.ResponseWriter, r *http.Request) {
	if h.noSealer(w) {
		return
	}
	if err := refuseFreeBoundsWith(r, h.aggregation, comparePeriodRemedy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !rejectUnknownQueryParams(w, r, sealedCompareParams...) {
		return
	}
	a, b, given, err := parseComparePeriods(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	m, err := h.sealer.markers(ctx)
	if err != nil {
		h.sealedReadFailed(w, b, err)
		return
	}
	switch {
	case !given:
		// The default read's month, or the month after the earliest when that is
		// later, so the pair never reaches before the earliest sealable month.
		b = m.defaultMonth()
		if m.hasEarliest && !b.Start.After(m.earliest.Start) {
			// The month after the earliest, or after the gaps that follow it.
			for b = m.earliest.next(); ; b = b.next() {
				if err = h.sealer.gapped(ctx, b); !errors.Is(err, errSealGapped) {
					break
				}
			}
			if err != nil {
				h.sealedReadFailed(w, b, err)
				return
			}
		}
		// a is the latest sealed month before b: the month before it unless that
		// is a gap or, after the latest sealable month (a raised grace), unsealed.
		a = b.prev()
		for last := lastSealable(h.sealer.now(), h.sealer.cfg.grace); m.hasEarliest && a.Start.After(m.earliest.Start); a = a.prev() {
			if err = h.sealer.sealed(ctx, a); err == nil ||
				(!a.Start.After(last.Start) && notSealable(err) && !errors.Is(err, errSealGapped)) {
				break
			}
			if !notSealable(err) {
				h.sealedReadFailed(w, a, err)
				return
			}
		}
	case !a.Start.Before(b.Start):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("?period_a=%s must be earlier than ?period_b=%s: "+
			"every delta is period_b minus period_a", a, b))
		return
	}
	// b is checked first, so a 404 for both names b.
	for _, p := range []Period{b, a} {
		if err = h.sealer.sealed(ctx, p); err != nil {
			h.writeSealErr(ctx, w, p, err)
			return
		}
	}
	var sides [2]sealedSide
	for i, p := range []Period{a, b} {
		if sides[i], err = h.loadSealedSide(ctx, p); err != nil {
			h.writeSealErr(ctx, w, p, err)
			return
		}
	}
	sa, sb := sides[0], sides[1]
	var refusal string
	switch {
	case sa.rep.ConfigDigest != sb.rep.ConfigDigest:
		refusal = fmt.Sprintf("%s and %s were sealed under different configs, which split the population "+
			"differently, so they are never compared (#913); compare two months sealed under one config", a, b)
	case sa.rep.ConfigDigest != sealConfigDigestOf(sealFoldRule, sa.rep.Level, sa.rep.PeriodSize, sa.rep.K):
		// The refold below is this binary's fold rule; another rule's inputs refold
		// to rows the months' own bodies were never folded by.
		refusal = fmt.Sprintf("%s and %s were sealed under a fold rule this binary does not refold "+
			"(it refolds rule %d), so they are not compared (#913); read each month on /scores?period=", a, b, sealFoldRule)
	}
	if refusal != "" {
		writeJSON(w, http.StatusConflict, sealedConfigMismatchJSON{
			Error: refusal, PeriodA: a.String(), PeriodB: b.String(),
			ConfigA: sealedConfigOf(sa.rep.Level, sa.rep.PeriodSize, sa.rep.K, sa.rep.ConfigDigest),
			ConfigB: sealedConfigOf(sb.rep.Level, sb.rep.PeriodSize, sb.rep.K, sb.rep.ConfigDigest),
		})
		return
	}

	resp := compareResponse{
		WindowA:    sa.window(),
		WindowB:    sb.window(),
		PriceTable: priceTableStamp(store.ActivePriceTableInfo()),
		Mode:       sa.rep.Level, // the level both months were sealed under
		Developers: []developerDeltaJSON{},
	}
	k := sa.rep.K
	rows, sup := scoring.CompareFolded(sa.inputs, sb.inputs, k)
	// #864: each month's own residual must reach k too, as in compareTeams.
	for _, in := range [][]scoring.LabelInput{sa.inputs, sb.inputs} {
		if _, own := scoring.AggregateFolded(in, k); own.Any() && (!sup.Any() || own.Developers > sup.Developers) {
			sup = own
		}
	}
	switch {
	case sup.Any():
		resp.KAnonSuppressed = compareSuppressedJSON(sup)
	case len(sa.inputs) > 0 || len(sb.inputs) > 0:
		for _, tc := range rows {
			resp.Teams = append(resp.Teams, newTeamDeltaJSON(tc.Team, tc.A, tc.B))
		}
		total := teamDeltaOfSides("", sa.total(), sb.total())
		resp.Total = &total
	}

	hdr := w.Header()
	hdr.Set(headerSealedPeriodA, a.String())
	hdr.Set(headerSealedPeriodB, b.String())
	hdr.Set(headerSealedAtA, sa.rep.SealedAt.UTC().Format(time.RFC3339))
	hdr.Set(headerSealedAtB, sb.rep.SealedAt.UTC().Format(time.RFC3339))
	m.setHeaders(hdr)
	writeJSON(w, http.StatusOK, resp)
}
