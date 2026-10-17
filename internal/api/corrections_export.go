package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// The export capability is optional for read-only store implementations. A
// missing capability refuses explicitly; it never emits an empty evidence page.
type correctionEvidenceReader interface {
	ListCorrectionEvidence(context.Context, string, time.Time, time.Time, store.PageCursor, int) ([]map[string]any, bool, error)
}

func (h *Handler) handleGetCorrectionEvidence(w http.ResponseWriter, r *http.Request) {
	// Suppression precedes parsing, interface discovery, and every evidence query.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden, "corrective evidence is not_shown in anonymized aggregation modes")
		return
	}
	kind := r.PathValue("kind")
	columns, err := store.CorrectionEvidenceColumns(kind)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	p, err := parseExportParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	reader, ok := h.store.(correctionEvidenceReader)
	if !ok {
		writeError(w, http.StatusInternalServerError, "correction evidence reader unavailable")
		return
	}
	rows, more, err := reader.ListCorrectionEvidence(r.Context(), kind, p.since, p.until, p.cursor, p.limit)
	if err != nil {
		h.logger.Error("list corrective evidence", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	next := ""
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		at, ok := last["recorded_at"].(time.Time)
		id, idOK := last["id"].(int64)
		if !ok || !idOK {
			writeError(w, http.StatusInternalServerError, "invalid correction cursor evidence")
			return
		}
		next = encodeCursor(at, id)
	}
	w.Header().Set(nextCursorHeader, next)
	if !p.csv {
		if rows == nil {
			rows = []map[string]any{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"next_cursor": next, kind: rows})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	cw := csv.NewWriter(w)
	if err = cw.Write(columns); err == nil {
		for _, row := range rows {
			record := make([]string, len(columns))
			for i, key := range columns {
				switch v := row[key].(type) {
				case nil:
				case time.Time:
					record[i] = v.UTC().Format(time.RFC3339Nano)
				case string:
					record[i] = csvText(v)
				default:
					record[i] = csvText(fmt.Sprint(v))
				}
			}
			if err = cw.Write(record); err != nil {
				break
			}
		}
	}
	cw.Flush()
	if err == nil {
		err = cw.Error()
	}
	if err != nil {
		h.logger.Error("write corrective evidence CSV", "err", err)
	}
}
