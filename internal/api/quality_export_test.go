package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// seedPushOutcome writes a source='push' outcome aggregated to the given UTC day,
// so the outcomes export tests can prove push_day round-trips (#242).
func seedPushOutcome(t *testing.T, db *store.DB, dev, issue, repo, day string, ts time.Time) {
	t.Helper()
	if _, err := db.UpsertPushOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: issue, Weight: 0.5, Quality: 1, Repo: repo, Timestamp: ts,
	}, day); err != nil {
		t.Fatalf("UpsertPushOutcome: %v", err)
	}
}

// seedQualityOutcome inserts an outcome under a known SHA and returns its id so a
// test can attach quality_events / quality_history rows.
func seedQualityOutcome(t *testing.T, db *store.DB, dev, issue, sha string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		Developer: dev, IssueID: issue, Weight: 1, Quality: 1,
		MergeCommitSHA: sha, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	o, ok, err := db.OutcomeByMergeCommit(ctx, sha)
	if err != nil || !ok {
		t.Fatalf("OutcomeByMergeCommit(%q): ok=%v err=%v", sha, ok, err)
	}
	return o.ID
}

// TestGetOutcomes_CSVIncludesPushDay proves push_day is the appended (#242) LAST
// CSV column, carries the UTC day for a push row, and is "" for a PR row.
func TestGetOutcomes_CSVIncludesPushDay(t *testing.T) {
	h, db := newTestHandler(t)
	seedPushOutcome(t, db, "alice", "issue-push", "acme/app", "2026-05-04",
		time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC))
	seedOutcome(t, db, "bob", "issue-pr", 3.0, 1.0)

	rec := doExport(t, h, "/api/v1/outcomes?since=2020-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	hdr := records[0]
	// push_day is appended, so it MUST be the last column.
	if hdr[len(hdr)-1] != "push_day" {
		t.Errorf("last outcomes CSV column = %q, want push_day (appended #242)", hdr[len(hdr)-1])
	}
	col := func(row []string, name string) string {
		for i, c := range hdr {
			if c == name {
				return row[i]
			}
		}
		t.Fatalf("column %q missing from header %v", name, hdr)
		return ""
	}
	byIssue := map[string][]string{}
	for _, row := range records[1:] {
		byIssue[col(row, "issue_id")] = row
	}
	if got := col(byIssue["issue-push"], "push_day"); got != "2026-05-04" {
		t.Errorf("push row push_day = %q, want 2026-05-04", got)
	}
	if got := col(byIssue["issue-pr"], "push_day"); got != "" {
		t.Errorf("PR row push_day = %q, want empty string", got)
	}
}

// TestGetOutcomes_JSONIncludesPushDay is the JSON analogue.
func TestGetOutcomes_JSONIncludesPushDay(t *testing.T) {
	h, db := newTestHandler(t)
	seedPushOutcome(t, db, "alice", "issue-push", "acme/app", "2026-05-04",
		time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC))
	seedOutcome(t, db, "bob", "issue-pr", 3.0, 1.0)

	rec := doExport(t, h, "/api/v1/outcomes?since=2020-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp outcomesExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byIssue := map[string]outcomeExportJSON{}
	for _, o := range resp.Outcomes {
		byIssue[o.IssueID] = o
	}
	if got := byIssue["issue-push"].PushDay; got != "2026-05-04" {
		t.Errorf("push row push_day = %q, want 2026-05-04", got)
	}
	if got := byIssue["issue-pr"].PushDay; got != "" {
		t.Errorf("PR row push_day = %q, want empty string", got)
	}
}

// seedCIPass appends one ci_pass quality_event for an outcome.
func seedCIPass(t *testing.T, db *store.DB, oid int64, dev, issue, ref string, ts time.Time) {
	t.Helper()
	if _, err := db.AppendQualityEvent(context.Background(), store.QualityEvent{
		OutcomeID: oid, Developer: dev, IssueID: issue,
		EventType: "ci_pass", SourceRef: ref, EventTS: ts,
	}); err != nil {
		t.Fatalf("AppendQualityEvent: %v", err)
	}
}

// TestGetQualityEvents_JSONAndColumns proves the quality_events export returns the
// documented columns and values in JSON and CSV.
func TestGetQualityEvents_JSONAndColumns(t *testing.T) {
	h, db := newTestHandler(t)
	oid := seedQualityOutcome(t, db, "alice", "issue-1", "sha-1")
	seedCIPass(t, db, oid, "alice", "issue-1", "head-abc:1", time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))

	// JSON.
	rec := doExport(t, h, "/api/v1/quality_events?since=2020-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("json status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp qualityEventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.QualityEvents) != 1 {
		t.Fatalf("want 1 quality_event, got %d", len(resp.QualityEvents))
	}
	e := resp.QualityEvents[0]
	if e.OutcomeID != oid || e.EventType != "ci_pass" || e.SourceRef != "head-abc:1" || e.Developer != "alice" {
		t.Errorf("row = %+v, want the seeded ci_pass row", e)
	}
	if !strings.HasPrefix(e.EventTS, "2026-05-15T00:00:00") {
		t.Errorf("event_ts = %q, want RFC3339 UTC", e.EventTS)
	}
	// recorded_at is CURRENT_TIMESTAMP at insert; it must still surface as a
	// non-empty RFC3339 UTC string (a Z-suffixed instant), not be dropped.
	if !strings.HasSuffix(e.RecordedAt, "Z") || len(e.RecordedAt) < len("2026-05-15T00:00:00Z") {
		t.Errorf("recorded_at = %q, want a non-empty RFC3339 UTC string", e.RecordedAt)
	}

	// CSV column contract + data-row values.
	crec := doExport(t, h, "/api/v1/quality_events?since=2020-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	if crec.Code != http.StatusOK {
		t.Fatalf("csv status=%d body=%s", crec.Code, crec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(crec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want header + 1 row, got %d", len(records))
	}
	if strings.Join(records[0], ",") != strings.Join(qualityEventsCSVHeader, ",") {
		t.Errorf("CSV header = %v, want %v", records[0], qualityEventsCSVHeader)
	}
	col := func(name string) string {
		for i, c := range records[0] {
			if c == name {
				return records[1][i]
			}
		}
		t.Fatalf("column %q missing from header %v", name, records[0])
		return ""
	}
	if col("outcome_id") != strconv.FormatInt(oid, 10) || col("event_type") != "ci_pass" ||
		col("source_ref") != "head-abc:1" || col("developer") != "alice" {
		t.Errorf("CSV row cols mismatch: outcome_id=%q event_type=%q source_ref=%q developer=%q",
			col("outcome_id"), col("event_type"), col("source_ref"), col("developer"))
	}
	if !strings.HasPrefix(col("event_ts"), "2026-05-15T00:00:00") {
		t.Errorf("CSV event_ts = %q, want RFC3339 UTC", col("event_ts"))
	}
}

// TestGetQualityHistory_JSONAndColumns proves the quality_history export returns
// the documented columns and the old->new transition values.
func TestGetQualityHistory_JSONAndColumns(t *testing.T) {
	h, db := newTestHandler(t)
	oid := seedQualityOutcome(t, db, "alice", "issue-1", "sha-1")
	if err := db.UpdateQualityForOutcome(context.Background(), oid, 0.1, "revert_quality", "revert-sha"); err != nil {
		t.Fatalf("UpdateQualityForOutcome: %v", err)
	}

	rec := doExport(t, h, "/api/v1/quality_history?since=2020-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("json status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp qualityHistoryExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.QualityHistory) != 1 {
		t.Fatalf("want 1 quality_history row, got %d", len(resp.QualityHistory))
	}
	q := resp.QualityHistory[0]
	if q.OutcomeID != oid || q.OldQuality != 1.0 || q.NewQuality != 0.1 || q.Reason != "revert_quality" || q.SourceRef != "revert-sha" {
		t.Errorf("row = %+v, want the 1.0->0.1 revert transition", q)
	}

	crec := doExport(t, h, "/api/v1/quality_history?since=2020-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	records, err := csv.NewReader(strings.NewReader(crec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) != 2 || strings.Join(records[0], ",") != strings.Join(qualityHistoryCSVHeader, ",") {
		t.Fatalf("CSV header = %v (rows=%d), want %v", records[0], len(records), qualityHistoryCSVHeader)
	}
	col := func(name string) string {
		for i, c := range records[0] {
			if c == name {
				return records[1][i]
			}
		}
		t.Fatalf("column %q missing from header %v", name, records[0])
		return ""
	}
	if col("outcome_id") != strconv.FormatInt(oid, 10) || col("old_quality") != "1" ||
		col("new_quality") != "0.1" || col("reason") != "revert_quality" || col("source_ref") != "revert-sha" {
		t.Errorf("CSV row cols mismatch: outcome_id=%q old=%q new=%q reason=%q source_ref=%q",
			col("outcome_id"), col("old_quality"), col("new_quality"), col("reason"), col("source_ref"))
	}
	if !strings.HasSuffix(col("ts"), "Z") {
		t.Errorf("CSV ts = %q, want RFC3339 UTC (Z-suffixed)", col("ts"))
	}
}

// TestGetQualityHistory_KeysetWalk pages the quality_history export through the full
// HTTP mux — exercising the opaque-cursor round-trip against the second-precision
// string-bound store read (#242) — and asserts no gaps / no duplicates.
func TestGetQualityHistory_KeysetWalk(t *testing.T) {
	h, db := newTestHandler(t)
	const total = 30
	for i := 0; i < total; i++ {
		oid := seedQualityOutcome(t, db, "alice", "issue-"+strconv.Itoa(i), "sha-"+strconv.Itoa(i))
		if err := db.UpdateQualityForOutcome(context.Background(), oid, 0.1, "revert_quality", "ref-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("UpdateQualityForOutcome %d: %v", i, err)
		}
	}
	seen := map[int64]int{}
	cursor := ""
	pages := 0
	for {
		target := "/api/v1/quality_history?since=2020-01-01&limit=7"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := doExport(t, h, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status=%d body=%s", pages, rec.Code, rec.Body.String())
		}
		var resp qualityHistoryExportResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal page %d: %v", pages, err)
		}
		if got := rec.Header().Get(nextCursorHeader); got != resp.NextCursor {
			t.Fatalf("header cursor %q != body cursor %q", got, resp.NextCursor)
		}
		for _, q := range resp.QualityHistory {
			seen[q.ID]++
		}
		pages++
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
		if pages > total {
			t.Fatal("walk did not terminate")
		}
	}
	if len(seen) != total {
		t.Fatalf("distinct rows walked = %d, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row id=%d seen %d times, want 1", id, n)
		}
	}
	if pages < 2 {
		t.Errorf("expected multiple pages, got %d", pages)
	}
}

// TestGetQualityEvents_KeysetWalk pages the quality_events export through the full
// HTTP mux — exercising the opaque-cursor round-trip against event_ts (the keyset
// column, formed from EventTS not RecordedAt) — and asserts no gaps / no dups over
// a >1-page seed with event_ts ties.
func TestGetQualityEvents_KeysetWalk(t *testing.T) {
	h, db := newTestHandler(t)
	oid := seedQualityOutcome(t, db, "alice", "issue-1", "sha-1")
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	const total = 25
	for i := 0; i < total; i++ {
		ts := base
		if i%2 == 0 { // pairs share an event_ts to exercise the (event_ts, id) tiebreak
			ts = base.Add(time.Duration(i) * time.Minute)
		}
		seedCIPass(t, db, oid, "alice", "issue-1", "head-"+strconv.Itoa(i), ts)
	}
	seen := map[int64]int{}
	cursor := ""
	pages := 0
	for {
		target := "/api/v1/quality_events?since=2020-01-01&limit=7"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := doExport(t, h, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status=%d body=%s", pages, rec.Code, rec.Body.String())
		}
		var resp qualityEventsExportResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal page %d: %v", pages, err)
		}
		if got := rec.Header().Get(nextCursorHeader); got != resp.NextCursor {
			t.Fatalf("header cursor %q != body cursor %q", got, resp.NextCursor)
		}
		for _, e := range resp.QualityEvents {
			seen[e.ID]++
		}
		pages++
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
		if pages > total {
			t.Fatal("walk did not terminate")
		}
	}
	if len(seen) != total {
		t.Fatalf("distinct rows walked = %d, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row id=%d seen %d times, want 1", id, n)
		}
	}
	if pages < 2 {
		t.Errorf("expected multiple pages, got %d", pages)
	}
}

// TestGetQualityExports_BadParams proves the per-handler 400 wiring (each handler
// re-checks parseExportParams' error) is present on BOTH new endpoints: an
// over-max limit and a malformed cursor are loud 400s, not a 500 or silent clamp.
func TestGetQualityExports_BadParams(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, base := range []string{"/api/v1/quality_events", "/api/v1/quality_history"} {
		if rec := doExport(t, h, base+"?limit=10001", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s limit=10001: status=%d, want 400; body=%s", base, rec.Code, rec.Body.String())
		}
		if rec := doExport(t, h, base+"?cursor=not-base64-%21%21%21", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad cursor: status=%d, want 400; body=%s", base, rec.Code, rec.Body.String())
		}
	}
}

// TestGetQualityExports_TeamModeForbidden proves the #185 guard: both quality
// exports 403 in team mode, with a developer-mode 200 control on the same fixture.
func TestGetQualityExports_TeamModeForbidden(t *testing.T) {
	for _, target := range []string{"/api/v1/quality_events", "/api/v1/quality_history"} {
		th, tdb := newTeamModeHandler(t, 3)
		oid := seedQualityOutcome(t, tdb, "alice", "issue-1", "sha-team")
		seedCIPass(t, tdb, oid, "alice", "issue-1", "ref", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
		_ = tdb.UpdateQualityForOutcome(context.Background(), oid, 0.1, "revert_quality", "r")
		rec := doExport(t, th, target, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s team mode: status = %d, want 403; body=%s", target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "#185") {
			t.Errorf("%s team-mode 403 should cite #185; body=%s", target, rec.Body.String())
		}

		dh, ddb := newTestHandler(t)
		doid := seedQualityOutcome(t, ddb, "alice", "issue-1", "sha-dev")
		seedCIPass(t, ddb, doid, "alice", "issue-1", "ref", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
		_ = ddb.UpdateQualityForOutcome(context.Background(), doid, 0.1, "revert_quality", "r")
		if rec := doExport(t, dh, target, nil); rec.Code != http.StatusOK {
			t.Errorf("%s developer mode: status = %d, want 200", target, rec.Code)
		}
	}
}

// TestGetQualityExports_Authz covers the #190 read-scope matrix for both new
// endpoints: read token accepted, write token accepted, wrong/absent token 401.
func TestGetQualityExports_Authz(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"
	bearer := func(tok string) http.Header {
		return http.Header{"Authorization": []string{"Bearer " + tok}}
	}
	for _, target := range []string{"/api/v1/quality_events", "/api/v1/quality_history"} {
		h, _ := newTestHandlerWithScopes(t, writeToken, readToken)
		if rec := doExport(t, h, target, bearer(readToken)); rec.Code != http.StatusOK {
			t.Errorf("%s: read token got %d, want 200", target, rec.Code)
		}
		if rec := doExport(t, h, target, bearer(writeToken)); rec.Code != http.StatusOK {
			t.Errorf("%s: write token got %d, want 200", target, rec.Code)
		}
		if rec := doExport(t, h, target, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: no token got %d, want 401", target, rec.Code)
		}
		if rec := doExport(t, h, target, bearer("wrong-token-of-the-same-len-3200")); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: wrong token got %d, want 401", target, rec.Code)
		}
	}
}

// TestGetQualityExports_EmptyRange returns an empty page and empty cursor on an
// empty DB for both endpoints.
func TestGetQualityExports_EmptyRange(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doExport(t, h, "/api/v1/quality_events?since=2020-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("quality_events status=%d", rec.Code)
	}
	var eresp qualityEventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &eresp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(eresp.QualityEvents) != 0 || eresp.NextCursor != "" {
		t.Errorf("quality_events empty: %d rows cursor=%q", len(eresp.QualityEvents), eresp.NextCursor)
	}

	rec = doExport(t, h, "/api/v1/quality_history?since=2020-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("quality_history status=%d", rec.Code)
	}
	var hresp qualityHistoryExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hresp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(hresp.QualityHistory) != 0 || hresp.NextCursor != "" {
		t.Errorf("quality_history empty: %d rows cursor=%q", len(hresp.QualityHistory), hresp.NextCursor)
	}
}
