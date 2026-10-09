package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// fidelityResponse is the body of GET /api/v1/fidelity (#236): per canonical
// developer capture-fidelity signals, plus the window bounds they are measured
// over. It answers "is this install capturing, for whom, and at what quality" —
// the second-org rollout question a bare TIER score cannot.
type fidelityResponse struct {
	// Now / Since7d / Since30d stamp the windows the counts below are measured
	// over, RFC3339 UTC, so a reader knows exactly what "7d" meant for this
	// response rather than assuming a server-local clock.
	Now        string                  `json:"now"`
	Since7d    string                  `json:"since_7d"`
	Since30d   string                  `json:"since_30d"`
	Developers []developerFidelityJSON `json:"developers"`
}

// developerFidelityJSON is one canonical developer's fidelity row. LastEventBySource
// and FidelityLevels are always-present (possibly empty) maps so a client can index
// them without a nil check.
type developerFidelityJSON struct {
	Developer     string `json:"developer"`
	EventCount7d  int64  `json:"event_count_7d"`
	EventCount30d int64  `json:"event_count_30d"`
	// LastEventBySource maps capture source -> RFC3339 UTC timestamp of the most
	// recent event from it, over all history. A source absent here has never
	// delivered for this developer.
	LastEventBySource map[string]string `json:"last_event_by_source"`
	// FidelityLevels maps fidelity level -> 30d event count. A developer capturing
	// only "estimated"/"daily" (no "realtime") is running a degraded capture path.
	FidelityLevels map[string]int64 `json:"fidelity_levels"`
	// UnknownModelCostShare is the fraction (0..1) of this developer's 30d spend
	// billed at the unknown-model pricing guess (#267) — high share means TIER is
	// pricing their spend at a rate it cannot audit.
	UnknownModelCostShare float64 `json:"unknown_model_cost_share"`
}

// handleGetFidelity serves the capture-fidelity signals (#236). It reads the raw
// per-developer signals, canonicalizes each raw developer key through the alias
// map (#125), and MERGES raw keys that resolve to the same canonical identity so a
// developer mid-rename is one row, not two. Read-scoped; 403 in team-aggregation
// mode (#185) because the response names individual developers.
func (h *Handler) handleGetFidelity(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation compliance guard (#185), same posture as GET /events: this
	// response names individual developers, which team mode's works-council/GDPR
	// promise forbids, and it cannot be k-anonymized while staying a per-developer
	// capture report. 403 BEFORE touching the store — do NOT "fix" by aggregating
	// the developer column; that silently breaks the same guarantee /scores enforces.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden,
			"per-developer fidelity is disabled in an anonymized aggregation mode (team #185 / division #270); it would expose individual-level capture data that anonymized modes suppress")
		return
	}

	now := time.Now().UTC()
	signals, err := h.store.DeveloperFidelity(r.Context(), now)
	if err != nil {
		h.logger.Error("developer fidelity", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	aliases, err := h.store.DeveloperAliases(r.Context())
	if err != nil {
		h.logger.Error("developer aliases", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	canon := func(dev string) string {
		if c, ok := aliases[dev]; ok {
			return c
		}
		return dev
	}

	// Merge raw signals by canonical developer. A canonical key can receive
	// contributions from several raw keys (the alias source AND its target), so
	// counts/costs sum, per-source timestamps take the max, and fidelity counts sum.
	merged := map[string]*mergedFidelity{}
	for _, s := range signals {
		key := canon(s.Developer)
		m := merged[key]
		if m == nil {
			m = &mergedFidelity{
				developer: key,
				lastBySrc: map[string]time.Time{},
				fidLevels: map[string]int64{},
			}
			merged[key] = m
		}
		m.count7d += s.EventCount7d
		m.count30d += s.EventCount30d
		for src, ts := range s.LastEventBySource {
			if prev, ok := m.lastBySrc[src]; !ok || ts.After(prev) {
				m.lastBySrc[src] = ts
			}
		}
		for level, n := range s.FidelityCounts {
			m.fidLevels[level] += n
		}
		m.unknownMicro += s.UnknownCostMicro30d
		m.totalMicro += s.TotalCostMicro30d
	}

	developers := make([]developerFidelityJSON, 0, len(merged))
	for _, m := range merged {
		developers = append(developers, m.toJSON())
	}
	sort.Slice(developers, func(i, j int) bool {
		return developers[i].Developer < developers[j].Developer
	})

	writeJSON(w, http.StatusOK, fidelityResponse{
		Now:        now.Format(time.RFC3339),
		Since7d:    now.Add(-store.FidelityWindow7d).Format(time.RFC3339),
		Since30d:   now.Add(-store.FidelityWindow30d).Format(time.RFC3339),
		Developers: developers,
	})
}

// mergedFidelity accumulates one canonical developer's fidelity signals across the
// raw token_events keys that resolve to it (#125). Kept as time.Time / int64
// internally so the max-per-source merge and the exact-integer cost-share ratio are
// computed before any lossy formatting; toJSON renders the wire shape once at the end.
type mergedFidelity struct {
	developer    string
	count7d      int64
	count30d     int64
	lastBySrc    map[string]time.Time
	fidLevels    map[string]int64
	unknownMicro int64
	totalMicro   int64
}

func (m *mergedFidelity) toJSON() developerFidelityJSON {
	last := make(map[string]string, len(m.lastBySrc))
	for src, ts := range m.lastBySrc {
		last[src] = ts.UTC().Format(time.RFC3339)
	}
	var share float64
	if m.totalMicro > 0 {
		share = float64(m.unknownMicro) / float64(m.totalMicro)
	}
	return developerFidelityJSON{
		Developer:             m.developer,
		EventCount7d:          m.count7d,
		EventCount30d:         m.count30d,
		LastEventBySource:     last,
		FidelityLevels:        m.fidLevels,
		UnknownModelCostShare: share,
	}
}
