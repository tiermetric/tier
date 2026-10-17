package api

import (
	"context"
	"github.com/tiermetric/tier/internal/scoring"
	"net/http"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// Suppress before parsing or reading: anonymized diagnostics must reveal no
// counts, relation existence, identifying links, or affected period identities.
func (h *Handler) correctionWindow(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, store.RepoScope, bool) {
	if h.aggregation.Anonymized() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_shown"})
		return time.Time{}, time.Time{}, store.FleetWide, false
	}
	if !rejectUnknownQueryParams(w, r, "since", "until", "before", "repo") {
		return time.Time{}, time.Time{}, store.FleetWide, false
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, 400, err.Error())
		return time.Time{}, time.Time{}, store.FleetWide, false
	}
	until, ok := h.parseWindowUpperBound(w, r, since)
	if !ok {
		return time.Time{}, time.Time{}, store.FleetWide, false
	}
	scope, ok := parseRepoScope(w, r)
	return since.UTC(), until, scope, ok
}
func (h *Handler) handleCorrectionDiagnostics(w http.ResponseWriter, r *http.Request) {
	since, until, scope, ok := h.correctionWindow(w, r)
	if !ok {
		return
	}
	var d store.CorrectionDiagnostics
	err := h.store.ReadSnapshot(r.Context(), func(s *store.Snapshot) error {
		var err error
		d, err = s.CorrectionDiagnosticsWindow(r.Context(), since, until, time.Now().UTC(), scope)
		return err
	})
	if err != nil {
		h.logger.Error("correction diagnostics", "err", err)
		writeError(w, 500, "db error")
		return
	}
	writeJSON(w, 200, d)
}
func (h *Handler) handleRepairSpend(w http.ResponseWriter, r *http.Request) {
	since, until, scope, ok := h.correctionWindow(w, r)
	if !ok {
		return
	}
	var d store.RepairSpend
	err := h.store.ReadSnapshot(r.Context(), func(s *store.Snapshot) error {
		var err error
		d, err = s.RepairSpendWindow(r.Context(), since, until, scope)
		return err
	})
	if err != nil {
		h.logger.Error("repair spend", "err", err)
		writeError(w, 500, "db error")
		return
	}
	writeJSON(w, 200, d)
}

func (h *Handler) correctionDisputes(ctx context.Context, r any, p scoring.ScoringPolicy, since, until time.Time, scope store.RepoScope) (any, error) {
	if !p.Event3 {
		return nil, nil
	}
	if h.aggregation.Anonymized() {
		return map[string]string{"status": "not_shown"}, nil
	}
	if cr, ok := r.(interface {
		CorrectionDisputesWindow(context.Context, time.Time, time.Time, store.RepoScope) (store.CorrectionDisputes, error)
	}); ok {
		return cr.CorrectionDisputesWindow(ctx, since, until, scope)
	}
	return nil, nil
}
