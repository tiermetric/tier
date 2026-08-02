package api

// GET /api/v1/events, GET /api/v1/outcomes, GET /api/v1/quality_events and
// GET /api/v1/quality_history — paginated, bounded bulk export of the raw
// token_events / outcomes rows (#191) and the quality audit chain (#242).
//
// Motivation: a CFO reconciling spend or a BI pipeline had no way to pull the
// underlying rows — /scores returns only the computed roll-up, and an all-rows
// dump would be unbounded for a 500-developer org. These endpoints add a
// keyset-paginated, hard-capped export in JSON (default) or CSV.
//
// AUTHZ (#190): READ-scoped (registered under requireRead) — this is the
// read use case, so the read-only viewer token is accepted alongside the write
// token; a viewer gets the data WITHOUT write/erase power. No token still 401s.
//
// COMPLIANCE (#185): in team-aggregation mode ALL FOUR endpoints 403. Raw rows
// carry a per-developer `developer` column; team mode is the deployment's
// works-council/GDPR commitment to suppress individual-level data, and a
// row-level export cannot be k-anonymized while remaining a useful export. The
// guard is at the TOP of each handler, before any query — see the per-handler
// comment for why a future reader must NOT "enable" it.

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// nextCursorHeader carries the opaque keyset cursor for the NEXT page on every
// export response (JSON and CSV). Empty value means the window is exhausted —
// the client stops paging when it comes back empty. JSON responses ALSO carry it
// in the body (next_cursor) so a JSON client need not read headers; CSV clients
// rely on this header exclusively.
const nextCursorHeader = "X-Next-Cursor"

// exportUntilOpen is the default upper bound when ?until is omitted: an
// effectively-open window. Year 9999 sorts lexically after every real UTC ts
// (all stored ts are >= 2020 and UTC), so `ts < ?` admits the newest rows. UTC
// so the bound renders in the same lexical space as the UTC-stored ts (#180) —
// a non-UTC bound would mis-window near the boundary exactly as sinceUTC guards.
var exportUntilOpen = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// eventsCSVHeader is the STABLE, documented CSV column order for GET
// /api/v1/events (#191). This ordering is a compatibility surface — a BI
// pipeline pins to it — so columns are only ever APPENDED, never reordered or
// removed. Mirrored in docs/how-it-works.md; keep the two in sync.
var eventsCSVHeader = []string{
	"id", "ts", "developer", "issue_id", "model",
	"input_tokens", "output_tokens", "cache_read_tokens",
	"cache_write_5m_tokens", "cache_write_1h_tokens",
	"cost_micro", "source", "fidelity", "idempotency_key",
	// repo APPENDED at the end (#231). issue_id's VALUES are deliberately
	// unchanged — a consumer that pins `issue_id` keeps reading "issue-42", and the
	// repository arrives in its own new column. Changing issue_id's shape in place
	// would have been an invisible semantic demotion of a pinned column, exactly
	// what #241 exists to forbid.
	"repo",
	// session_id APPENDED at the end (#238), same append-only contract. It is the
	// opaque Claude Code session UUID (NULL/"" for proxy/poller rows), and arrives
	// in its own new column so a consumer pinned to the pre-#238 order is unbroken.
	"session_id",
	// price_version APPENDED at the end (#233), same append-only contract. It is the
	// price-table version that produced cost_micro — the provenance a CFO needs to
	// tell a real spend trend from a table-bump artifact. 0 marks a pre-#233 row not
	// yet backfilled. Arrives in its own new column so a pinned consumer is unbroken.
	"price_version",
	// host + billing_mode APPENDED at the end (#304), same append-only contract.
	// host is the serving host that priced the row (store.HostUnknown "unknown" for
	// the first-party JSONL/poller paths and any host-blind producer). billing_mode
	// (per_token | subscription | self_hosted_amortized) is the discriminator that
	// tells whether cost_micro is a canonical per-token $/M or a DERIVED/APPROXIMATE
	// figure — it MUST accompany cost_micro at this export seam before #268 seeds any
	// subscription/amortized host rate, or a derived cost would ship with no adjacent
	// flag. Both arrive in their own new columns so a pinned consumer is unbroken.
	"host", "billing_mode",
}

// outcomesCSVHeader is the STABLE, documented CSV column order for GET
// /api/v1/outcomes (#191). Same append-only contract as eventsCSVHeader.
var outcomesCSVHeader = []string{
	"id", "ts", "developer", "issue_id", "pr_number",
	"weight", "weight_source", "quality", "merge_commit_sha",
	"additions", "deletions", "changed_files", "source",
	// work_type + work_type_source APPENDED at the end (#187): the CSV column order
	// is an append-only compatibility contract, so new columns go last and never
	// disturb an existing consumer's positional parse.
	"work_type", "work_type_source",
	// repo APPENDED (#231), same contract. issue_id values are unchanged.
	"repo",
	// push_day APPENDED at the end (#242), same append-only contract. It is the UTC
	// calendar day a source='push' outcome aggregates to — the per-issue-per-day
	// dedup key the #196 partial unique index is built on. WITHOUT it a push row
	// exported with pr_number=0 and merge_commit_sha="" had NO visible aggregation
	// key, so an external org could not verify the one-outcome-per-day dedup from its
	// own export. Empty "" for a non-push row (NULL column), matching merge_commit_sha.
	// Arrives in its own new column so a consumer pinned to the pre-#242 order is unbroken.
	"push_day",
}

// exportParams is the parsed, validated query surface shared by both exports.
type exportParams struct {
	since  time.Time
	until  time.Time
	cursor store.PageCursor
	limit  int
	csv    bool
}

// parseExportParams parses and validates the export query string. Every bound is
// normalized to UTC (reusing parseSince/sinceUTC, #180/#199) and every value is
// returned as a typed field the store binds as a `?` parameter — nothing here is
// ever interpolated into SQL. Any malformed value is a 400 (the caller writes
// it), never a 500 or a full-table scan.
func parseExportParams(r *http.Request) (exportParams, error) {
	q := r.URL.Query()
	since, err := parseSince(q.Get("since"))
	if err != nil {
		return exportParams{}, fmt.Errorf("invalid since: %v", err)
	}
	until, err := parseExportUntil(q.Get("until"))
	if err != nil {
		return exportParams{}, fmt.Errorf("invalid until: %v", err)
	}
	limit, err := parseExportLimit(q.Get("limit"))
	if err != nil {
		return exportParams{}, err
	}
	cursor, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		return exportParams{}, fmt.Errorf("invalid cursor: %v", err)
	}
	return exportParams{
		since:  sinceUTC(since),
		until:  until,
		cursor: cursor,
		limit:  limit,
		csv:    wantsCSV(r),
	}, nil
}

// parseExportUntil parses the ?until upper bound, defaulting to an open window
// when absent. Same layouts as parseSince; UTC-normalized. The window is
// half-open [since, until), so a row exactly at `until` is excluded.
func parseExportUntil(s string) (time.Time, error) {
	if s == "" {
		return exportUntilOpen, nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, YYYY-MM, or YYYY, got %q", s)
}

// parseExportLimit parses ?limit, defaulting to store.DefaultExportPageSize and
// REJECTING an over-max request loudly (the read-side counterpart of the #144
// input-cap discipline) rather than silently clamping — a client asking for more
// than it can have should learn so, not get a quietly truncated page. The store
// clamps again as defense in depth.
func parseExportLimit(s string) (int, error) {
	if s == "" {
		return store.DefaultExportPageSize, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer, got %q", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer, got %d", n)
	}
	if n > store.MaxExportPageSize {
		return 0, fmt.Errorf("limit must be <= %d, got %d", store.MaxExportPageSize, n)
	}
	return n, nil
}

// wantsCSV reports whether the client asked for CSV via the Accept header.
// Anything else (including no Accept header) yields JSON, the documented default.
func wantsCSV(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/csv")
}

// encodeCursor renders a keyset position as an opaque, URL-safe token. The
// payload is "<RFC3339Nano ts>|<id>" base64url-encoded: opaque to the client
// (it echoes the token back verbatim in ?cursor) but trivially decoded
// server-side. RFC3339Nano is lossless for the stored ts and contains no '|', so
// the last '|' unambiguously splits ts from id.
//
// It is deliberately NOT signed: the cursor only selects a pagination offset,
// and decodeCursor binds both fields as typed SQL parameters, so a tampered
// cursor can at worst return a differently-windowed (still authorized, still
// in-[since,until)) page — never SQL injection and never data outside the
// window the same request already authorizes.
func encodeCursor(ts time.Time, id int64) string {
	raw := ts.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses an opaque cursor back to a keyset position. Empty → the
// zero cursor (start of window). Any malformed input (bad base64, missing
// separator, unparseable ts, non-numeric/negative id) is a typed error the
// handler turns into a 400 — never a 500 and never a silent full-scan.
func decodeCursor(s string) (store.PageCursor, error) {
	if s == "" {
		return store.PageCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.PageCursor{}, fmt.Errorf("cursor is not valid base64url")
	}
	sep := bytes.LastIndexByte(raw, '|')
	if sep < 0 {
		return store.PageCursor{}, fmt.Errorf("cursor is malformed")
	}
	ts, err := time.Parse(time.RFC3339Nano, string(raw[:sep]))
	if err != nil {
		return store.PageCursor{}, fmt.Errorf("cursor timestamp is malformed")
	}
	id, err := strconv.ParseInt(string(raw[sep+1:]), 10, 64)
	if err != nil || id < 0 {
		return store.PageCursor{}, fmt.Errorf("cursor id is malformed")
	}
	return store.PageCursor{TS: ts.UTC(), ID: id}, nil
}

// --- GET /api/v1/events ---

// eventExportJSON is one row in the JSON export of token_events (#191). Field
// order mirrors eventsCSVHeader so the JSON and CSV contracts read identically.
type eventExportJSON struct {
	ID              int64  `json:"id"`
	Timestamp       string `json:"ts"`
	Developer       string `json:"developer"`
	IssueID         string `json:"issue_id"`
	Model           string `json:"model"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
	CacheWrite5m    int    `json:"cache_write_5m_tokens"`
	CacheWrite1h    int    `json:"cache_write_1h_tokens"`
	CostMicro       int64  `json:"cost_micro"`
	Source          string `json:"source"`
	Fidelity        string `json:"fidelity"`
	IdempotencyKey  string `json:"idempotency_key"`
	Repo            string `json:"repo"`
	SessionID       string `json:"session_id"`
	PriceVersion    int    `json:"price_version"`
	Host            string `json:"host"`
	BillingMode     string `json:"billing_mode"`
}

// eventsExportResponse is the JSON page body. NextCursor is the opaque cursor for
// the following page, empty when the window is exhausted.
type eventsExportResponse struct {
	NextCursor string            `json:"next_cursor"`
	Events     []eventExportJSON `json:"events"`
}

func (h *Handler) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation mode (#185) compliance guard — MUST stay, MUST run first.
	// token_events rows carry a per-developer `developer` column. Team mode is the
	// deployment's committed (works-council/GDPR) promise NOT to expose
	// individual-level data; a bulk raw-row export flatly contradicts that and
	// cannot be k-anonymized while remaining a useful row-level export. So 403 here
	// BEFORE parsing params or touching the store. Do NOT "fix" this by filtering
	// or aggregating the developer column — that silently breaks the same guarantee
	// /scores and /scores/{developer} enforce (#185). In developer mode it works.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden,
			"bulk per-developer export is disabled in an anonymized aggregation mode (team #185 / division #270); it would expose individual-level rows that anonymized modes suppress")
		return
	}
	p, err := parseExportParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	events, hasMore, err := h.store.ListTokenEvents(r.Context(), p.since, p.until, p.cursor, p.limit)
	if err != nil {
		h.logger.Error("list token events", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	next := ""
	if hasMore && len(events) > 0 {
		last := events[len(events)-1]
		next = encodeCursor(last.Timestamp, last.ID)
	}
	if p.csv {
		h.writeEventsCSV(w, events, next)
		return
	}
	h.writeEventsJSON(w, events, next)
}

func (h *Handler) writeEventsJSON(w http.ResponseWriter, events []store.TokenEvent, next string) {
	// Materializing one page is bounded by store.MaxExportPageSize, so this is NOT
	// the unbounded-buffer anti-pattern the streaming requirement targets — the
	// whole result set is the window, which we never buffer; only this capped page.
	out := make([]eventExportJSON, len(events))
	for i, e := range events {
		out[i] = eventExportJSON{
			ID:              e.ID,
			Timestamp:       e.Timestamp.UTC().Format(time.RFC3339Nano),
			Developer:       e.Developer,
			IssueID:         e.IssueID,
			Model:           e.Model,
			InputTokens:     e.InputTok,
			OutputTokens:    e.OutputTok,
			CacheReadTokens: e.CacheRead,
			CacheWrite5m:    e.CacheWrite5m,
			CacheWrite1h:    e.CacheWrite1h,
			CostMicro:       e.CostMicro,
			Source:          e.Source,
			Fidelity:        e.Fidelity,
			IdempotencyKey:  e.IdempotencyKey,
			Repo:            e.Repo,
			SessionID:       e.SessionID,
			PriceVersion:    e.PriceVersion,
			Host:            e.Host,
			BillingMode:     e.BillingMode,
		}
	}
	w.Header().Set(nextCursorHeader, next)
	writeJSON(w, http.StatusOK, eventsExportResponse{NextCursor: next, Events: out})
}

func (h *Handler) writeEventsCSV(w http.ResponseWriter, events []store.TokenEvent, next string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set(nextCursorHeader, next)
	w.WriteHeader(http.StatusOK)
	// Stream rows straight to the ResponseWriter through csv.Writer, which escapes
	// any comma / quote / newline embedded in an issue id or source string per RFC
	// 4180 — no hand-rolled quoting. Errors here can only be a broken client
	// connection mid-stream; the header/status are already sent, so we log and stop.
	cw := csv.NewWriter(w)
	if err := cw.Write(eventsCSVHeader); err != nil {
		h.logger.Error("write events csv header", "err", err)
		return
	}
	rec := make([]string, len(eventsCSVHeader))
	for _, e := range events {
		rec[0] = strconv.FormatInt(e.ID, 10)
		rec[1] = e.Timestamp.UTC().Format(time.RFC3339Nano)
		rec[2] = e.Developer
		rec[3] = e.IssueID
		rec[4] = e.Model
		rec[5] = strconv.Itoa(e.InputTok)
		rec[6] = strconv.Itoa(e.OutputTok)
		rec[7] = strconv.Itoa(e.CacheRead)
		rec[8] = strconv.Itoa(e.CacheWrite5m)
		rec[9] = strconv.Itoa(e.CacheWrite1h)
		rec[10] = strconv.FormatInt(e.CostMicro, 10)
		rec[11] = e.Source
		rec[12] = e.Fidelity
		rec[13] = e.IdempotencyKey
		rec[14] = e.Repo
		rec[15] = e.SessionID
		rec[16] = strconv.Itoa(e.PriceVersion)
		rec[17] = e.Host
		rec[18] = e.BillingMode
		if err := cw.Write(rec); err != nil {
			h.logger.Error("write events csv row", "err", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.logger.Error("flush events csv", "err", err)
	}
}

// --- GET /api/v1/outcomes ---

// outcomeExportJSON is one row in the JSON export of outcomes (#191). Field order
// mirrors outcomesCSVHeader.
type outcomeExportJSON struct {
	ID             int64   `json:"id"`
	Timestamp      string  `json:"ts"`
	Developer      string  `json:"developer"`
	IssueID        string  `json:"issue_id"`
	PRNumber       int     `json:"pr_number"`
	Weight         float64 `json:"weight"`
	WeightSource   string  `json:"weight_source"`
	Quality        float64 `json:"quality"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Additions      int     `json:"additions"`
	Deletions      int     `json:"deletions"`
	ChangedFiles   int     `json:"changed_files"`
	Source         string  `json:"source"`
	WorkType       string  `json:"work_type"`
	WorkTypeSource string  `json:"work_type_source"`
	Repo           string  `json:"repo"`
	// PushDay is the #242 append-only tail column: the UTC day a push outcome
	// aggregates to (the per-issue-per-day dedup key), "" for a non-push row.
	PushDay string `json:"push_day"`
}

// outcomesExportResponse is the JSON page body for the outcomes export.
type outcomesExportResponse struct {
	NextCursor string              `json:"next_cursor"`
	Outcomes   []outcomeExportJSON `json:"outcomes"`
}

func (h *Handler) handleGetOutcomes(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation mode (#185) compliance guard — same rationale as
	// handleGetEvents: outcomes rows are per-developer, so a bulk export is
	// suppressed in team mode. MUST run first, MUST NOT be relaxed to a filtered or
	// aggregated variant. Developer mode works normally.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden,
			"bulk per-developer export is disabled in an anonymized aggregation mode (team #185 / division #270); it would expose individual-level rows that anonymized modes suppress")
		return
	}
	p, err := parseExportParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	outcomes, hasMore, err := h.store.ListOutcomes(r.Context(), p.since, p.until, p.cursor, p.limit)
	if err != nil {
		h.logger.Error("list outcomes", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	next := ""
	if hasMore && len(outcomes) > 0 {
		last := outcomes[len(outcomes)-1]
		next = encodeCursor(last.Timestamp, last.ID)
	}
	if p.csv {
		h.writeOutcomesCSV(w, outcomes, next)
		return
	}
	h.writeOutcomesJSON(w, outcomes, next)
}

func (h *Handler) writeOutcomesJSON(w http.ResponseWriter, outcomes []store.Outcome, next string) {
	out := make([]outcomeExportJSON, len(outcomes))
	for i, o := range outcomes {
		out[i] = outcomeExportJSON{
			ID:             o.ID,
			Timestamp:      o.Timestamp.UTC().Format(time.RFC3339Nano),
			Developer:      o.Developer,
			IssueID:        o.IssueID,
			PRNumber:       o.PRNumber,
			Weight:         o.Weight,
			WeightSource:   o.WeightSource,
			Quality:        o.Quality,
			MergeCommitSHA: o.MergeCommitSHA,
			Additions:      o.Additions,
			Deletions:      o.Deletions,
			ChangedFiles:   o.ChangedFiles,
			Source:         o.Source,
			WorkType:       o.WorkType,
			WorkTypeSource: o.WorkTypeSource,
			Repo:           o.Repo,
			PushDay:        o.PushDay,
		}
	}
	w.Header().Set(nextCursorHeader, next)
	writeJSON(w, http.StatusOK, outcomesExportResponse{NextCursor: next, Outcomes: out})
}

func (h *Handler) writeOutcomesCSV(w http.ResponseWriter, outcomes []store.Outcome, next string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set(nextCursorHeader, next)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	if err := cw.Write(outcomesCSVHeader); err != nil {
		h.logger.Error("write outcomes csv header", "err", err)
		return
	}
	rec := make([]string, len(outcomesCSVHeader))
	for _, o := range outcomes {
		rec[0] = strconv.FormatInt(o.ID, 10)
		rec[1] = o.Timestamp.UTC().Format(time.RFC3339Nano)
		rec[2] = o.Developer
		rec[3] = o.IssueID
		rec[4] = strconv.Itoa(o.PRNumber)
		rec[5] = strconv.FormatFloat(o.Weight, 'f', -1, 64)
		rec[6] = o.WeightSource
		rec[7] = strconv.FormatFloat(o.Quality, 'f', -1, 64)
		rec[8] = o.MergeCommitSHA
		rec[9] = strconv.Itoa(o.Additions)
		rec[10] = strconv.Itoa(o.Deletions)
		rec[11] = strconv.Itoa(o.ChangedFiles)
		rec[12] = o.Source
		rec[13] = o.WorkType
		rec[14] = o.WorkTypeSource
		rec[15] = o.Repo
		rec[16] = o.PushDay
		if err := cw.Write(rec); err != nil {
			h.logger.Error("write outcomes csv row", "err", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.logger.Error("flush outcomes csv", "err", err)
	}
}

// --- GET /api/v1/quality_events and GET /api/v1/quality_history (#242) ---
//
// The quality audit chain: quality_events is the append-only log of observed CI /
// revert signals, and quality_history the transition log written on every quality
// write. Together they make an outcome's multiplier re-derivable ("quality == last
// new_quality") — but before #242 they were only reachable via the erasure-scoped
// DSAR export, so an external org running trunk-based capture could not verify the
// derivation from its own BI export. These two endpoints add the same
// keyset-paginated, hard-capped, JSON-or-CSV bulk export the events/outcomes
// exports provide. Both are READ-scoped and BOTH carry the #185 team-mode 403 guard
// (the rows carry a per-developer `developer` column).

// qualityEventsCSVHeader is the STABLE, documented CSV column order for GET
// /api/v1/quality_events (#242). Append-only, same contract as the other exports.
var qualityEventsCSVHeader = []string{
	"id", "outcome_id", "developer", "issue_id",
	"event_type", "source_ref", "event_ts", "recorded_at",
}

// qualityHistoryCSVHeader is the STABLE, documented CSV column order for GET
// /api/v1/quality_history (#242). Append-only, same contract.
var qualityHistoryCSVHeader = []string{
	"id", "outcome_id", "developer", "issue_id",
	"old_quality", "new_quality", "reason", "source_ref", "ts",
}

// qualityEventExportJSON is one row in the JSON export of quality_events (#242).
// Field order mirrors qualityEventsCSVHeader. event_ts / recorded_at are RFC3339Nano
// UTC strings, matching the ts formatting of the events/outcomes exports.
type qualityEventExportJSON struct {
	ID         int64  `json:"id"`
	OutcomeID  int64  `json:"outcome_id"`
	Developer  string `json:"developer"`
	IssueID    string `json:"issue_id"`
	EventType  string `json:"event_type"`
	SourceRef  string `json:"source_ref"`
	EventTS    string `json:"event_ts"`
	RecordedAt string `json:"recorded_at"`
}

// qualityEventsExportResponse is the JSON page body. NextCursor is the opaque cursor
// for the following page, empty when the window is exhausted.
type qualityEventsExportResponse struct {
	NextCursor    string                   `json:"next_cursor"`
	QualityEvents []qualityEventExportJSON `json:"quality_events"`
}

// qualityHistoryExportJSON is one row in the JSON export of quality_history (#242).
// Field order mirrors qualityHistoryCSVHeader.
type qualityHistoryExportJSON struct {
	ID         int64   `json:"id"`
	OutcomeID  int64   `json:"outcome_id"`
	Developer  string  `json:"developer"`
	IssueID    string  `json:"issue_id"`
	OldQuality float64 `json:"old_quality"`
	NewQuality float64 `json:"new_quality"`
	Reason     string  `json:"reason"`
	SourceRef  string  `json:"source_ref"`
	Timestamp  string  `json:"ts"`
}

// qualityHistoryExportResponse is the JSON page body for the quality_history export.
type qualityHistoryExportResponse struct {
	NextCursor     string                     `json:"next_cursor"`
	QualityHistory []qualityHistoryExportJSON `json:"quality_history"`
}

func (h *Handler) handleGetQualityEvents(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation mode (#185) compliance guard — MUST stay, MUST run first,
	// same rationale as handleGetEvents: quality_events rows carry a per-developer
	// `developer` column, so a bulk raw-row export is suppressed in team mode and
	// cannot be relaxed to a filtered/aggregated variant. Developer mode works.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden,
			"bulk per-developer export is disabled in an anonymized aggregation mode (team #185 / division #270); it would expose individual-level rows that anonymized modes suppress")
		return
	}
	p, err := parseExportParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	events, hasMore, err := h.store.ListQualityEvents(r.Context(), p.since, p.until, p.cursor, p.limit)
	if err != nil {
		h.logger.Error("list quality events", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	next := ""
	if hasMore && len(events) > 0 {
		last := events[len(events)-1]
		// The keyset column is event_ts (see store.ListQualityEvents), so the cursor
		// is formed from EventTS, not RecordedAt.
		next = encodeCursor(last.EventTS, last.ID)
	}
	if p.csv {
		h.writeQualityEventsCSV(w, events, next)
		return
	}
	h.writeQualityEventsJSON(w, events, next)
}

func (h *Handler) writeQualityEventsJSON(w http.ResponseWriter, events []store.QualityEvent, next string) {
	out := make([]qualityEventExportJSON, len(events))
	for i, e := range events {
		out[i] = qualityEventExportJSON{
			ID:         e.ID,
			OutcomeID:  e.OutcomeID,
			Developer:  e.Developer,
			IssueID:    e.IssueID,
			EventType:  e.EventType,
			SourceRef:  e.SourceRef,
			EventTS:    e.EventTS.UTC().Format(time.RFC3339Nano),
			RecordedAt: e.RecordedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	w.Header().Set(nextCursorHeader, next)
	writeJSON(w, http.StatusOK, qualityEventsExportResponse{NextCursor: next, QualityEvents: out})
}

func (h *Handler) writeQualityEventsCSV(w http.ResponseWriter, events []store.QualityEvent, next string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set(nextCursorHeader, next)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	if err := cw.Write(qualityEventsCSVHeader); err != nil {
		h.logger.Error("write quality_events csv header", "err", err)
		return
	}
	rec := make([]string, len(qualityEventsCSVHeader))
	for _, e := range events {
		rec[0] = strconv.FormatInt(e.ID, 10)
		rec[1] = strconv.FormatInt(e.OutcomeID, 10)
		rec[2] = e.Developer
		rec[3] = e.IssueID
		rec[4] = e.EventType
		rec[5] = e.SourceRef
		rec[6] = e.EventTS.UTC().Format(time.RFC3339Nano)
		rec[7] = e.RecordedAt.UTC().Format(time.RFC3339Nano)
		if err := cw.Write(rec); err != nil {
			h.logger.Error("write quality_events csv row", "err", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.logger.Error("flush quality_events csv", "err", err)
	}
}

func (h *Handler) handleGetQualityHistory(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation mode (#185) compliance guard — same posture as the other
	// exports: quality_history rows are per-developer, so 403 before any query.
	if h.aggregation.Anonymized() {
		writeError(w, http.StatusForbidden,
			"bulk per-developer export is disabled in an anonymized aggregation mode (team #185 / division #270); it would expose individual-level rows that anonymized modes suppress")
		return
	}
	p, err := parseExportParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	history, hasMore, err := h.store.ListQualityHistory(r.Context(), p.since, p.until, p.cursor, p.limit)
	if err != nil {
		h.logger.Error("list quality history", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	next := ""
	if hasMore && len(history) > 0 {
		last := history[len(history)-1]
		next = encodeCursor(last.Timestamp, last.ID)
	}
	if p.csv {
		h.writeQualityHistoryCSV(w, history, next)
		return
	}
	h.writeQualityHistoryJSON(w, history, next)
}

func (h *Handler) writeQualityHistoryJSON(w http.ResponseWriter, history []store.QualityTransition, next string) {
	out := make([]qualityHistoryExportJSON, len(history))
	for i, q := range history {
		out[i] = qualityHistoryExportJSON{
			ID:         q.ID,
			OutcomeID:  q.OutcomeID,
			Developer:  q.Developer,
			IssueID:    q.IssueID,
			OldQuality: q.OldQuality,
			NewQuality: q.NewQuality,
			Reason:     q.Reason,
			SourceRef:  q.SourceRef,
			Timestamp:  q.Timestamp.UTC().Format(time.RFC3339Nano),
		}
	}
	w.Header().Set(nextCursorHeader, next)
	writeJSON(w, http.StatusOK, qualityHistoryExportResponse{NextCursor: next, QualityHistory: out})
}

func (h *Handler) writeQualityHistoryCSV(w http.ResponseWriter, history []store.QualityTransition, next string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set(nextCursorHeader, next)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	if err := cw.Write(qualityHistoryCSVHeader); err != nil {
		h.logger.Error("write quality_history csv header", "err", err)
		return
	}
	rec := make([]string, len(qualityHistoryCSVHeader))
	for _, q := range history {
		rec[0] = strconv.FormatInt(q.ID, 10)
		rec[1] = strconv.FormatInt(q.OutcomeID, 10)
		rec[2] = q.Developer
		rec[3] = q.IssueID
		rec[4] = strconv.FormatFloat(q.OldQuality, 'f', -1, 64)
		rec[5] = strconv.FormatFloat(q.NewQuality, 'f', -1, 64)
		rec[6] = q.Reason
		rec[7] = q.SourceRef
		rec[8] = q.Timestamp.UTC().Format(time.RFC3339Nano)
		if err := cw.Write(rec); err != nil {
			h.logger.Error("write quality_history csv row", "err", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.logger.Error("flush quality_history csv", "err", err)
	}
}
