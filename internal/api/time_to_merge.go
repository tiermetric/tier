package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

type timeToMergeFigure struct {
	Team          string `json:"team,omitempty"`
	Count         *int   `json:"count"`
	MedianSeconds *int64 `json:"median_seconds"`
	P90Seconds    *int64 `json:"p90_seconds"`
	NullReason    string `json:"null_reason"`
}

type timeToMergeWindow struct {
	Since       string `json:"since"`
	Until       string `json:"until,omitempty"`
	Aggregation string `json:"aggregation"`
}

type timeToMergeExcluded struct {
	Count      *int           `json:"count"`
	Reasons    map[string]int `json:"reasons"`
	NullReason string         `json:"null_reason"`
}

type timeToMergeResponse struct {
	Definition string              `json:"definition"`
	Window     timeToMergeWindow   `json:"window"`
	Groups     []timeToMergeFigure `json:"groups"`
	Residual   *timeToMergeFigure  `json:"residual,omitempty"`
	Total      timeToMergeFigure   `json:"total"`
	Excluded   timeToMergeExcluded `json:"excluded"`
}

// timeToMergeCaptureCutover is the created_at column's release date, not this
// installation's upgrade date. Later upgrades classify post-cutover missing
// evidence as no_created_at.
const timeToMergeCaptureCutover = "2026-10-11"

// mergeSample retains integer nanoseconds until the final statistics are
// converted to whole seconds. No intermediate per-PR rounding changes a median.
type mergeSample struct {
	durations []int64
	authors   map[string]bool
}

func (s *mergeSample) add(duration time.Duration, author string, counted bool) {
	s.durations = append(s.durations, int64(duration))
	if s.authors == nil {
		s.authors = map[string]bool{}
	}
	if counted {
		s.authors[author] = true
	}
}

func (s mergeSample) figure(team string, k int) timeToMergeFigure {
	f := timeToMergeFigure{Team: team, NullReason: "authors<k"}
	if len(s.durations) == 0 {
		f.NullReason = "no_prs"
		return f
	}
	if k < scoring.MinKAnonymity {
		k = scoring.MinKAnonymity
	}
	if len(s.authors) < k || len(s.durations) < k {
		return f
	}
	n := len(s.durations)
	values := append([]int64(nil), s.durations...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	median := values[n/2]
	if n%2 == 0 {
		// Nonnegative durations; this mean cannot overflow.
		lo, hi := values[n/2-1], values[n/2]
		median = lo + (hi-lo)/2
	}
	median /= int64(time.Second)
	f.Count, f.MedianSeconds = &n, &median
	f.NullReason = "n<10" // only p90 is null in this case
	if n >= 10 {
		p90 := values[n-n/10-1] / int64(time.Second) // ceil(0.9*n)-1
		f.P90Seconds, f.NullReason = &p90, ""
	}
	return f
}

func (h *Handler) handleGetTimeToMerge(w http.ResponseWriter, r *http.Request) {
	if !h.aggregation.Anonymized() && !h.syntheticDemo {
		writeError(w, http.StatusForbidden, "time to merge is disabled in developer aggregation mode outside the synthetic demo")
		return
	}
	var since, until time.Time
	var err error
	if h.sealedRead() {
		p, _, _, ok := h.resolveSealed(w, r, nil)
		if !ok {
			return
		}
		since, until = p.Bounds()
	} else {
		if !rejectUnknownQueryParams(w, r, "since", "until", "before") {
			return
		}
		since, err = parseSince(r.URL.Query().Get("since"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
			return
		}
		since = sinceUTC(since)
		var ok bool
		until, ok = h.parseWindowUpperBound(w, r, since)
		if !ok {
			return
		}
	}
	// Developer mode in the synthetic demo still emits no personal figures: use
	// the shared team fold. No request parameter can change the server's mode.
	reader := h
	if !reader.aggregation.Anonymized() {
		reader = &Handler{
			logger: h.logger, store: h.store, scoringPolicy: h.scoringPolicy,
			storedScoringPolicy: h.storedScoringPolicy,
			aggregation:         scoring.AggregationTeam, kAnonymity: h.kAnonymity,
		}
	}
	var response timeToMergeResponse
	err = h.store.ReadSnapshot(r.Context(), func(snap *store.Snapshot) error {
		q := scoresQuery{since: since, until: until, scope: store.FleetWide}
		data, err := reader.loadScoresWindow(r.Context(), snap, q)
		if err != nil {
			return err
		}
		evidence, err := snap.TimeToMergeOutcomesWindow(r.Context(), since, until, store.FleetWide)
		if err != nil {
			return err
		}
		response = reader.buildTimeToMerge(data, evidence)
		return nil
	})
	if err != nil {
		h.logger.Error("time to merge snapshot", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) buildTimeToMerge(data scoresWindowData, evidence []store.Outcome) timeToMergeResponse {
	win, census := data.win, data.census
	resp := timeToMergeResponse{
		Definition: "pr_created_to_merged",
		Window:     timeToMergeWindow{Since: win.since.Format("2006-01-02"), Aggregation: h.aggregation.String()},
		Groups:     []timeToMergeFigure{},
		Excluded: timeToMergeExcluded{Count: new(int), Reasons: map[string]int{
			"no_created_at_before_capture": 0, "no_created_at": 0, "no_merged_at": 0, "bot": 0,
		}},
	}
	if !win.until.IsZero() {
		resp.Window.Until = win.until.Format("2006-01-02")
	}
	inputs := scoring.PreFold(data.groups.rows, census.scoring(), func(id string) string { return id })
	teams, sup := scoring.AggregateFolded(inputs, h.kAnonymity)
	named := map[string]bool{}
	for _, team := range teams {
		if team.Team != scoring.OtherCohort {
			named[team.Team] = true
		}
	}
	byID := make(map[int64]store.Outcome, len(evidence))
	for _, o := range evidence {
		byID[o.ID] = o
	}
	samples := map[string]*mergeSample{}
	var total mergeSample
	populationAuthors := map[string]bool{}
	for _, o := range win.outcomes {
		dev := win.canon(o.Developer)
		if store.ResemblesUnattributed(dev) {
			continue // same population as /scores
		}
		if census.counted[dev] && !census.bots[dev] && !isBotLogin(dev) {
			populationAuthors[dev] = true
		}
		e := byID[o.ID]
		reason := ""
		switch {
		case census.bots[dev] || isBotLogin(dev):
			reason = "bot"
		case e.CreatedAt == nil && (o.Source == store.OutcomeSourceGitHubWebhook || o.Source == "backfill") && o.Timestamp.UTC().Format("2006-01-02") < timeToMergeCaptureCutover:
			reason = "no_created_at_before_capture"
		case e.CreatedAt == nil || (e.MergedAt != nil && e.CreatedAt.After(*e.MergedAt)):
			reason = "no_created_at"
		case e.MergedAt == nil:
			reason = "no_merged_at"
		}
		if reason != "" {
			(*resp.Excluded.Count)++
			resp.Excluded.Reasons[reason]++
			continue
		}
		label := data.groups.outcomeLabels[o.ID]
		if !named[label] {
			label = scoring.OtherCohort
		}
		if samples[label] == nil {
			samples[label] = &mergeSample{}
		}
		samples[label].add(e.MergedAt.Sub(*e.CreatedAt), dev, census.counted[dev])
		total.add(e.MergedAt.Sub(*e.CreatedAt), dev, census.counted[dev])
	}
	figure := func(label string) timeToMergeFigure {
		if s := samples[label]; s != nil {
			return s.figure(label, h.kAnonymity)
		}
		return timeToMergeFigure{Team: label, NullReason: "no_prs"}
	}
	// /scores withholds ALL figures when its residual is unsafe. Preserve that
	// guard before applying the stricter PR-author floor to any surviving row.
	if !sup.Residual {
		for _, team := range teams {
			f := figure(team.Team)
			if team.Team == scoring.OtherCohort {
				resp.Residual = &f
			} else {
				resp.Groups = append(resp.Groups, f)
			}
		}
	}
	if sup.Residual || len(populationAuthors) < max(h.kAnonymity, scoring.MinKAnonymity) {
		reason := "authors<k"
		if len(total.durations) == 0 && *resp.Excluded.Count == 0 {
			reason = "no_prs"
		}
		resp.Excluded = timeToMergeExcluded{NullReason: reason}
	}
	// The org figure uses every included PR, never group medians. Even when the
	// org itself clears k, no total may expose a cohort whose figures are hidden.
	// A short-sample p90 is unavailable, not a withheld cohort: its count and
	// median remain present and it does not trigger complementary suppression.
	resp.Total = total.figure("", h.kAnonymity)
	if resp.Total.Count != nil {
		withheld := sup.Residual && len(samples) > 0
		for label, sample := range samples {
			// Only a cohort with included PRs can conceal part of the total.
			withheld = withheld || sample.figure(label, h.kAnonymity).Count == nil
		}
		if withheld {
			resp.Total = timeToMergeFigure{NullReason: "withheld_with_group"}
		}
	}
	return resp
}
