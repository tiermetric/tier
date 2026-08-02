package api

import "net/http"

// orgActualSpendResponse is the body of GET /api/v1/org_actual_spend (#42): the
// net actual-paid timeline per (org, period), for finance read-back and SOC2
// evidence. Since echoes the resolved lower bound (YYYY-MM-DD) so a caller sees
// exactly which window produced the list. Orgs is always a non-nil slice so an
// empty result marshals to [] (not null).
type orgActualSpendResponse struct {
	Since string                `json:"since"`
	Orgs  []orgActualSpendEntry `json:"orgs"`
}

// orgActualSpendEntry is one (org, period) net roll-up. ActualPaidUSD is the
// SUM across every accumulation row for that (org, period) — across ALL sources
// (manual + provider pollers) — so credit memos and poller deltas net out.
// Entries is the row count behind the net (audit hint: 1 = a single invoice,
// >1 = corrections or poller deltas).
type orgActualSpendEntry struct {
	Org           string  `json:"org"`
	Period        string  `json:"period"`
	ActualPaidUSD float64 `json:"actual_paid_usd"`
	Entries       int     `json:"entries"`
}

// handleGetOrgActualSpend serves the #42 read-back. Query params:
//   - since (optional): reuses parseSince (default 90 days); periods >= since's
//     YYYY-MM month are included.
//   - org (optional): exact-match filter; absent/empty returns every org.
//
// Wrapped by requireAuth in Register, matching the auth posture of the score
// GETs and the POST half (spend data is sensitive, #59).
func (h *Handler) handleGetOrgActualSpend(w http.ResponseWriter, r *http.Request) {
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	// Normalize to UTC at the call site so the month window is computed against
	// the same instant regardless of the zone parseSince's default carries
	// (#180 discipline, matching handleGetScores).
	since = sinceUTC(since)

	org := r.URL.Query().Get("org")
	if len(org) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, "org must be <= 256 chars")
		return
	}

	totals, err := h.store.OrgActualSpendTotals(r.Context(), since, org)
	if err != nil {
		h.logger.Error("query org_actual_spend totals", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	entries := make([]orgActualSpendEntry, 0, len(totals))
	for _, t := range totals {
		entries = append(entries, orgActualSpendEntry{
			Org:           t.Org,
			Period:        t.Period,
			ActualPaidUSD: t.ActualPaidUSD,
			Entries:       t.Entries,
		})
	}
	writeJSON(w, http.StatusOK, orgActualSpendResponse{
		Since: since.Format("2006-01-02"),
		Orgs:  entries,
	})
}
