package api

import (
	"net/http"
	"time"

	"github.com/tiermetric/tier/internal/diagnostics"
	"github.com/tiermetric/tier/internal/store"
)

// Live diagnostic evidence is deliberately separate from reproducible scores.
func (h *Handler) handleCapturedDollars(w http.ResponseWriter, r *http.Request) {
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden, "captured-dollar diagnostics require developer mode")
		return
	}
	if !rejectUnknownQueryParams(w, r, "since", "until", "before", "repo") {
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	since = sinceUTC(since)
	until, ok := h.parseWindowUpperBound(w, r, since)
	if !ok {
		return
	}
	scope, ok := parseRepoScope(w, r)
	if !ok || !h.allowRepoScope(w, scope) {
		return
	}
	var rows []store.CapturedDollarRow
	err = h.store.ReadSnapshot(r.Context(), func(s *store.Snapshot) error {
		var err error
		rows, err = s.CapturedDollarRowsWindow(r.Context(), since, until, scope)
		return err
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "captured dollar diagnostics unavailable")
		return
	}
	end := ""
	if !until.IsZero() {
		end = until.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, diagnostics.BuildCaptured(rows, since.UTC().Format(time.RFC3339), end, scope.String(), h.engineEnablement))
}
