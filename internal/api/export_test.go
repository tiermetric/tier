package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// doExport routes a GET through the full mux and returns the raw recorder so a
// test can inspect status, body, AND response headers (X-Next-Cursor).
func doExport(t *testing.T, h *Handler, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

// seedEventAt inserts one token_event at a controlled ts (empty idempotency key,
// so every call lands a fresh row).
func seedEventAt(t *testing.T, db *store.DB, dev, issue string, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev, IssueID: issue, Model: "claude-sonnet-4",
		InputTok: 1000, CostMicro: 12_345, Source: "jsonl", Fidelity: "realtime",
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

// TestGetEvents_JSONWindow proves the JSON default returns only rows inside
// [since, until).
func TestGetEvents_JSONWindow(t *testing.T) {
	h, db := newTestHandler(t)
	seedEventAt(t, db, "alice", "in", time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))
	seedEventAt(t, db, "alice", "before", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	seedEventAt(t, db, "alice", "after", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))

	rec := doExport(t, h, "/api/v1/events?since=2026-05-01&until=2026-06-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp eventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].IssueID != "in" {
		t.Fatalf("expected only the in-window row, got %+v", resp.Events)
	}
	if resp.NextCursor != "" {
		t.Errorf("single-row page should have empty next_cursor, got %q", resp.NextCursor)
	}
}

// TestGetEvents_JSONIncludesPriceVersion proves the export surfaces the #233
// price_version provenance stamp — the version a CFO needs to tell a real spend
// trend from a table-bump artifact. A normal insert stamps the active version.
func TestGetEvents_JSONIncludesPriceVersion(t *testing.T) {
	h, db := newTestHandler(t)
	seedEventAt(t, db, "alice", "in", time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))

	rec := doExport(t, h, "/api/v1/events?since=2026-05-01&until=2026-06-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp eventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("expected 1 row, got %d", len(resp.Events))
	}
	if want := store.ActivePriceTableInfo().Version; resp.Events[0].PriceVersion != want {
		t.Errorf("export price_version = %d, want active version %d", resp.Events[0].PriceVersion, want)
	}
}

// seedEventHostAt inserts one token_event carrying an explicit serving host and
// billing_mode (#300) so the export tests can prove those columns round-trip a
// non-default value — not just the "unknown"/"per_token" normalization defaults.
func seedEventHostAt(t *testing.T, db *store.DB, dev, issue, host, billingMode string, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev, IssueID: issue, Model: "glm-4-6",
		InputTok: 1000, CostMicro: 12_345, Source: "proxy", Fidelity: "realtime",
		Host: host, BillingMode: billingMode, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

// TestGetEvents_JSONIncludesHostBillingMode proves the JSON export surfaces the
// #300 host + billing_mode discriminator (#304). This MUST land before #268 seeds
// a subscription/self_hosted_amortized host rate: once it does, cost_micro can be
// a DERIVED/APPROXIMATE figure, and an export boundary with no adjacent
// billing_mode flag would dress it as a canonical $/M — the exact hazard the
// discriminator exists to prevent.
func TestGetEvents_JSONIncludesHostBillingMode(t *testing.T) {
	h, db := newTestHandler(t)
	seedEventHostAt(t, db, "alice", "explicit", "openrouter", store.BillingSubscription,
		time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))
	// A default seed (no host set) MUST also serialize its normalized sentinels.
	// writeEventsJSON is a SEPARATE write path from writeEventsCSV, so the sentinel
	// case is asserted here too: an `omitempty` regression on the Host/BillingMode
	// JSON tags would silently drop "unknown"/"per_token" from the payload and break
	// a consumer expecting the fields — this row catches that.
	seedEventAt(t, db, "bob", "default", time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC))

	rec := doExport(t, h, "/api/v1/events?since=2026-05-01&until=2026-06-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp eventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(resp.Events))
	}
	byIssue := map[string]eventExportJSON{}
	for _, e := range resp.Events {
		byIssue[e.IssueID] = e
	}
	if got := byIssue["explicit"].Host; got != "openrouter" {
		t.Errorf("explicit row host = %q, want openrouter", got)
	}
	if got := byIssue["explicit"].BillingMode; got != store.BillingSubscription {
		t.Errorf("explicit row billing_mode = %q, want %q", got, store.BillingSubscription)
	}
	if got := byIssue["default"].Host; got != store.HostUnknown {
		t.Errorf("default row host = %q, want %q (omitempty regression?)", got, store.HostUnknown)
	}
	if got := byIssue["default"].BillingMode; got != store.BillingPerToken {
		t.Errorf("default row billing_mode = %q, want %q (omitempty regression?)", got, store.BillingPerToken)
	}
}

// TestGetEvents_CSVIncludesHostBillingMode proves the CSV export appends host +
// billing_mode at the END of the documented column order (append-only contract,
// same as price_version #233 / session_id #238) and carries their values. It
// resolves the columns by NAME from the header so the assertion survives future
// appends. A default seed (no host set) reads back as the store's normalized
// sentinels — host "unknown", billing_mode "per_token".
func TestGetEvents_CSVIncludesHostBillingMode(t *testing.T) {
	h, db := newTestHandler(t)
	seedEventHostAt(t, db, "alice", "explicit", "together", store.BillingSelfHostedAmortized,
		time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))
	seedEventAt(t, db, "bob", "default", time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC))

	rec := doExport(t, h, "/api/v1/events?since=2026-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("want header + 2 data rows, got %d records", len(records))
	}
	// host + billing_mode are appended, so they must be the last two columns.
	hdr := records[0]
	if hdr[len(hdr)-2] != "host" || hdr[len(hdr)-1] != "billing_mode" {
		t.Errorf("last two CSV columns = %q,%q, want host,billing_mode (appended #304)",
			hdr[len(hdr)-2], hdr[len(hdr)-1])
	}
	col := func(row []string, name string) string {
		for i, h := range hdr {
			if h == name {
				return row[i]
			}
		}
		t.Fatalf("column %q missing from header %v", name, hdr)
		return ""
	}
	// Row order is (ts, id) ascending: the explicit-host row (2026-05-02) precedes
	// the default row (2026-05-03).
	if got := col(records[1], "host"); got != "together" {
		t.Errorf("explicit row host = %q, want together", got)
	}
	if got := col(records[1], "billing_mode"); got != store.BillingSelfHostedAmortized {
		t.Errorf("explicit row billing_mode = %q, want %q", got, store.BillingSelfHostedAmortized)
	}
	// A session-blind/default producer normalizes to the store sentinels.
	if got := col(records[2], "host"); got != store.HostUnknown {
		t.Errorf("default row host = %q, want %q", got, store.HostUnknown)
	}
	if got := col(records[2], "billing_mode"); got != store.BillingPerToken {
		t.Errorf("default row billing_mode = %q, want %q", got, store.BillingPerToken)
	}
}

// TestGetEvents_KeysetWalk_NoGapNoDup pages through the HTTP endpoint with a seed
// larger than the default page size and asserts the union of every page equals
// the full set exactly once.
func TestGetEvents_KeysetWalk_NoGapNoDup(t *testing.T) {
	h, db := newTestHandler(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	const total = 2500 // > store.DefaultExportPageSize (1000): forces >2 pages
	events := make([]store.TokenEvent, 0, total)
	for i := 0; i < total; i++ {
		// Pairs share a ts to exercise the (ts, id) tiebreak across page seams.
		ts := base.Add(time.Duration(i/2) * time.Second)
		events = append(events, store.TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
			InputTok: 1, CostMicro: 1, Source: "jsonl", Fidelity: "realtime", Timestamp: ts,
		})
	}
	if err := db.InsertTokenEvents(context.Background(), events); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	seen := map[int64]int{}
	cursor := ""
	pages := 0
	for {
		target := "/api/v1/events?since=2026-01-01"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := doExport(t, h, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status=%d body=%s", pages, rec.Code, rec.Body.String())
		}
		var resp eventsExportResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal page %d: %v", pages, err)
		}
		if len(resp.Events) > store.DefaultExportPageSize {
			t.Fatalf("page %d has %d rows > default cap", pages, len(resp.Events))
		}
		// The body next_cursor and the X-Next-Cursor header must agree.
		if got := rec.Header().Get(nextCursorHeader); got != resp.NextCursor {
			t.Fatalf("header cursor %q != body cursor %q", got, resp.NextCursor)
		}
		for _, e := range resp.Events {
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
	if pages < 3 {
		t.Errorf("expected at least 3 pages for %d rows at default page size, got %d", total, pages)
	}
}

// TestGetEvents_OverMaxLimitRejected proves an over-max ?limit is a loud 400, not
// a silently clamped page, and that other malformed limits 400 too.
func TestGetEvents_OverMaxLimitRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, q := range []string{"limit=10001", "limit=0", "limit=-3", "limit=abc"} {
		rec := doExport(t, h, "/api/v1/events?"+q, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%s", q, rec.Code, rec.Body.String())
		}
	}
	// At-max is accepted.
	rec := doExport(t, h, "/api/v1/events?limit=10000", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("limit=10000 should be accepted, got %d", rec.Code)
	}
}

// TestGetEvents_MalformedCursor400 proves a bad cursor is a 400, never a 500 or a
// full-table scan.
func TestGetEvents_MalformedCursor400(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, c := range []string{
		"not-base64-!!!",     // invalid base64url characters
		"Zm9vYmFy",           // valid base64 of "foobar" — no separator
		"MjAyNi0wMS0wMXwxMg", // "2026-01-01|12" — ts not RFC3339Nano
	} {
		rec := doExport(t, h, "/api/v1/events?cursor="+url.QueryEscape(c), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("cursor=%q: status = %d, want 400; body=%s", c, rec.Code, rec.Body.String())
		}
	}
}

// TestGetEvents_CSVColumnContract proves Accept: text/csv yields the documented
// column order with a header row, and that embedded commas/quotes/newlines are
// escaped (encoding/csv, RFC 4180).
func TestGetEvents_CSVColumnContract(t *testing.T) {
	h, db := newTestHandler(t)
	// An issue id containing a comma, a quote, and a newline stresses CSV escaping.
	nasty := "iss,\"ue\n1"
	seedEventAt(t, db, "alice", nasty, time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))

	rec := doExport(t, h, "/api/v1/events?since=2026-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want header + 1 data row, got %d records", len(records))
	}
	if strings.Join(records[0], ",") != strings.Join(eventsCSVHeader, ",") {
		t.Errorf("CSV header = %v, want documented order %v", records[0], eventsCSVHeader)
	}
	// Column index 3 is issue_id per the contract; it must round-trip exactly.
	if records[1][3] != nasty {
		t.Errorf("issue_id round-trip = %q, want %q (escaping broken)", records[1][3], nasty)
	}
}

// TestGetOutcomes_CSVColumnContract asserts the outcomes CSV column order.
func TestGetOutcomes_CSVColumnContract(t *testing.T) {
	h, db := newTestHandler(t)
	seedOutcome(t, db, "alice", "issue-9", 3.0, 1.0)

	rec := doExport(t, h, "/api/v1/outcomes?since=2020-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) < 1 || strings.Join(records[0], ",") != strings.Join(outcomesCSVHeader, ",") {
		t.Fatalf("outcomes CSV header = %v, want %v", records[0], outcomesCSVHeader)
	}
	// The append-only contract is a statement about the PREFIX, not about which
	// column happens to be last. Pin the historical prefix literally: any reorder,
	// rename, or removal breaks a positional BI consumer and must fail here. New
	// columns may only be appended after it.
	//
	// This list is frozen history. Do NOT regenerate it from outcomesCSVHeader —
	// that would make the test tautological and it would stop guarding anything.
	frozenPrefix := []string{
		"id", "ts", "developer", "issue_id", "pr_number",
		"weight", "weight_source", "quality", "merge_commit_sha",
		"additions", "deletions", "changed_files", "source",
		"work_type", "work_type_source", // appended by #187
	}
	got := records[0]
	if len(got) < len(frozenPrefix) {
		t.Fatalf("outcomes CSV lost columns: got %v", got)
	}
	for i, want := range frozenPrefix {
		if got[i] != want {
			t.Errorf("outcomes CSV column %d = %q, want %q (append-only contract: never reorder or rename)", i, got[i], want)
		}
	}
	// #242 appended push_day last; #231's repo is now the penultimate column.
	// Both remain present and in append order — issue_id KEPT its position and
	// meaning throughout.
	if got[len(got)-1] != "push_day" {
		t.Errorf("last outcomes CSV column = %q, want push_day (appended #242)", got[len(got)-1])
	}
	if got[len(got)-2] != "repo" {
		t.Errorf("penultimate outcomes CSV column = %q, want repo (appended #231)", got[len(got)-2])
	}
}

// TestGetOutcomes_CSVIncludesWorkTypeValue proves the appended #187 columns carry
// the stored category and provenance, not just a header. seedOutcome writes a
// feature/default row (the labelless baseline).
func TestGetOutcomes_CSVIncludesWorkTypeValue(t *testing.T) {
	h, db := newTestHandler(t)
	seedOutcome(t, db, "alice", "issue-9", 3.0, 1.0)

	rec := doExport(t, h, "/api/v1/outcomes?since=2020-01-01",
		http.Header{"Accept": []string{"text/csv"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want header + 1 data row, got %d", len(records))
	}
	// Address the appended columns by NAME, resolved from the header, rather than by
	// a negative offset — the offsets shift every time a column is appended, which is
	// exactly what the append-only contract permits.
	row := records[1]
	col := func(name string) string {
		for i, h := range records[0] {
			if h == name {
				return row[i]
			}
		}
		t.Fatalf("column %q missing from header %v", name, records[0])
		return ""
	}
	if col("work_type") != "feature" || col("work_type_source") != "default" {
		t.Errorf("data row work_type cols = %q,%q, want feature,default", col("work_type"), col("work_type_source"))
	}
	// #231: a seeded outcome carries no repository, so it reads back as the sentinel
	// rather than "" — the NOT NULL column has a convergent DEFAULT.
	if got := col("repo"); got != "unqualified" {
		t.Errorf("data row repo = %q, want unqualified", got)
	}
}

// TestGetExports_EmptyRange returns an empty page and empty cursor.
func TestGetExports_EmptyRange(t *testing.T) {
	h, db := newTestHandler(t)
	seedEventAt(t, db, "alice", "x", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	rec := doExport(t, h, "/api/v1/events?since=2026-06-01&until=2026-07-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp eventsExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Events) != 0 || resp.NextCursor != "" {
		t.Errorf("empty range: %d events, cursor=%q; want 0/empty", len(resp.Events), resp.NextCursor)
	}
	if rec.Header().Get(nextCursorHeader) != "" {
		t.Errorf("empty range should carry empty %s header", nextCursorHeader)
	}
}

// TestGetExports_Authz covers the #190 read-scope + no-token matrix in developer
// mode: read token accepted, write token accepted, wrong/absent token 401.
func TestGetExports_Authz(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"
	bearer := func(tok string) http.Header {
		return http.Header{"Authorization": []string{"Bearer " + tok}}
	}
	for _, target := range []string{"/api/v1/events", "/api/v1/outcomes"} {
		h, _ := newTestHandlerWithScopes(t, writeToken, readToken)
		// Read-only viewer token (#190) — accepted on these read exports.
		if rec := doExport(t, h, target, bearer(readToken)); rec.Code != http.StatusOK {
			t.Errorf("%s: read token got %d, want 200 (#190 read scope)", target, rec.Code)
		}
		// Write/admin token — also accepted (superset scope).
		if rec := doExport(t, h, target, bearer(writeToken)); rec.Code != http.StatusOK {
			t.Errorf("%s: write token got %d, want 200", target, rec.Code)
		}
		// No token — 401.
		if rec := doExport(t, h, target, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: no token got %d, want 401", target, rec.Code)
		}
		// Wrong token — 401.
		if rec := doExport(t, h, target, bearer("wrong-token-of-the-same-len-3200")); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: wrong token got %d, want 401", target, rec.Code)
		}
	}
}

// TestGetExports_TeamModeForbidden proves the #185 compliance guard: BOTH exports
// 403 in team-aggregation mode, with a developer-mode 200 control on the same
// fixture.
func TestGetExports_TeamModeForbidden(t *testing.T) {
	for _, target := range []string{"/api/v1/events", "/api/v1/outcomes"} {
		// Team mode → 403, before any query.
		th, tdb := newTeamModeHandler(t, 3)
		seedEventAt(t, tdb, "alice", "issue-1", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
		seedOutcome(t, tdb, "alice", "issue-1", 3.0, 1.0)
		rec := doExport(t, th, target, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s team mode: status = %d, want 403; body=%s", target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "#185") {
			t.Errorf("%s team-mode 403 should cite #185; body=%s", target, rec.Body.String())
		}

		// Developer-mode control on the same fixture → 200.
		dh, ddb := newTestHandler(t)
		seedEventAt(t, ddb, "alice", "issue-1", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
		seedOutcome(t, ddb, "alice", "issue-1", 3.0, 1.0)
		if rec := doExport(t, dh, target, nil); rec.Code != http.StatusOK {
			t.Errorf("%s developer mode: status = %d, want 200", target, rec.Code)
		}
	}
}

// TestGetOutcomes_JSONWindow is the outcomes analogue of the events window test.
func TestGetOutcomes_JSONWindow(t *testing.T) {
	h, db := newTestHandler(t)
	seedOutcome(t, db, "alice", "issue-1", 3.0, 1.0)

	rec := doExport(t, h, "/api/v1/outcomes?since=2020-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp outcomesExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Outcomes) != 1 || resp.Outcomes[0].IssueID != "issue-1" {
		t.Fatalf("expected one outcome row, got %+v", resp.Outcomes)
	}
}
