package cursoradmin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

var testNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
var testDay = testNow.Truncate(24*time.Hour).AddDate(0, 0, -1)

const testKey = "cursor-admin-secret-must-not-appear"

type metricSink struct {
	counts            Counts
	good, bad, events int
	auth              []int
}

func (m *metricSink) PollComplete(ok bool) {
	if ok {
		m.good++
	} else {
		m.bad++
	}
}
func (m *metricSink) EventsIngested(n int)   { m.events += n }
func (m *metricSink) Counts(c Counts)        { m.counts = c }
func (m *metricSink) AuthFailure(status int) { m.auth = append(m.auth, status) }

type fixture struct {
	db       *store.DB
	poller   *Poller
	events   []usageEvent
	requests []usageRequest
	logs     bytes.Buffer
	metrics  metricSink
	sleeps   []time.Duration
	handle   func(http.ResponseWriter, *http.Request, usageRequest) bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{db: db}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != usagePath || r.Method != http.MethodPost {
			t.Errorf("unexpected endpoint %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != testKey || pass != "" {
			t.Error("wrong Basic auth")
		}
		var req usageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		f.requests = append(f.requests, req)
		if f.handle != nil && f.handle(w, r, req) {
			return
		}
		respond(w, f.events, 1, 1, false)
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{APIKey: testKey, BaseURL: server.URL, Sleep: func(_ context.Context, d time.Duration) error { f.sleeps = append(f.sleeps, d); return nil }})
	f.poller = NewPoller(PollerConfig{Client: client, Store: db, SettleDays: 1, LookbackDays: 1, Now: func() time.Time { return testNow }, Logger: slog.New(slog.NewTextHandler(&f.logs, nil)), Metrics: &f.metrics})
	return f
}
func respond(w http.ResponseWriter, events []usageEvent, page, pages int, next bool) {
	rep := usageReport{Total: len(events) * pages, Events: events}
	rep.Pagination.CurrentPage = page
	rep.Pagination.NumPages = pages
	rep.Pagination.HasNextPage = next
	_ = json.NewEncoder(w).Encode(rep)
}
func event(model string, input int) usageEvent {
	return usageEvent{Timestamp: strconv.FormatInt(testDay.Add(time.Hour).UnixMilli(), 10), Email: "private@example.test", Model: model, IsTokenBasedCall: true, IsChargeable: true, TokenUsage: &tokenUsage{Input: input}}
}
func (f *fixture) poll(t *testing.T) Counts {
	t.Helper()
	counts, err := f.poller.pollOnce(t.Context(), ingester.Store(f.db))
	if err != nil {
		t.Fatal(err)
	}
	return counts
}
func (f *fixture) rows(t *testing.T) []store.TokenEvent {
	t.Helper()
	rows, more, err := f.db.ListTokenEvents(t.Context(), testDay, testDay.AddDate(0, 0, 1), store.PageCursor{}, 1000)
	if err != nil || more {
		t.Fatalf("read: %v more=%v", err, more)
	}
	return rows
}
func (f *fixture) net(t *testing.T) map[rowKey]total {
	t.Helper()
	rows, err := f.poller.stored(t.Context(), testDay)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func (f *fixture) roster(t *testing.T) {
	t.Helper()
	if err := f.db.UpsertHierarchy(t.Context(), "alice", "team", "division", "org", "operator"); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) alias(t *testing.T, email, dev string) {
	t.Helper()
	if err := f.db.UpsertDeveloperAlias(t.Context(), email, dev, "operator"); err != nil {
		t.Fatal(err)
	}
}

func TestCursorPerEventListPricing(t *testing.T) {
	f := newFixture(t)
	// Two 150K requests are each below Sonnet's 200K context threshold.
	f.events = []usageEvent{event("claude-sonnet-4-5", 150000), event("claude-sonnet-4-5-20250929", 150000)}
	f.poll(t)
	rows := f.rows(t)
	want, mode := store.ComputeCostHost("", "claude-sonnet-4-5", store.CostUsage{Input: 150000})
	repriced, _ := store.ComputeCostHost("", "claude-sonnet-4-5", store.CostUsage{Input: 300000})
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	r := rows[0]
	if r.CostMicro != 2*want || r.CostMicro == repriced || r.InputTok != 300000 || r.BillingMode != mode || r.PricingBasis != store.PricingTokenDerived {
		t.Fatalf("row=%+v; per-event=%d aggregate=%d", r, 2*want, repriced)
	}
	if r.Source != collector.SourceCursorAdmin || r.IssueID != collector.UnattributedIssueID || r.Fidelity != collector.FidelityDaily || !r.Timestamp.Equal(testDay) || r.SessionID != "" {
		t.Fatalf("provenance=%+v", r)
	}
	f.poll(t)
	if len(f.rows(t)) != 1 {
		t.Fatal("unchanged poll appended a row")
	}
}

func TestCursorFallbackAndUncosted(t *testing.T) {
	f := newFixture(t)
	unknown := event("unknown-new-model", 12)
	unknown.TokenUsage.TotalCents = 123.45
	unknown.ChargedCents = 999
	zero := event("unknown-zero", 20)
	request := event("request-model", 0)
	request.IsTokenBasedCall = false
	request.TokenUsage = nil
	request.ChargedCents = 25
	noCost := request
	noCost.Model = "request-zero"
	noCost.ChargedCents = 0
	free := noCost
	free.IsChargeable = false
	f.events = []usageEvent{unknown, zero, request, noCost, free}
	c := f.poll(t)
	if c.Fallback != 4 || c.Uncosted != 2 || c.NonChargeable != 1 || c.Events != 2 {
		t.Fatalf("counts=%+v", c)
	}
	rows := f.rows(t)
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	for _, r := range rows {
		want := int64(250000)
		if r.Model == unknown.Model {
			want = 1234500
			if r.InputTok != 12 {
				t.Fatalf("tokens=%+v", r)
			}
		}
		if r.CostMicro != want || r.PricingBasis != store.PricingCallerAuthoritative {
			t.Fatalf("fallback row=%+v want %d", r, want)
		}
	}
}

func TestCursorNonChargeablePricing(t *testing.T) {
	f := newFixture(t)
	priced := event("gpt-4o", 100)
	priced.IsChargeable = false
	unpriced := event("unknown-included-model", 100)
	unpriced.IsChargeable = false
	f.events = []usageEvent{priced, unpriced}
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	rows := f.rows(t)
	want, _ := store.ComputeCostHost("", priced.Model, store.CostUsage{Input: 100})
	if len(rows) != 1 || rows[0].Model != priced.Model || rows[0].CostMicro != want || rows[0].InputTok != 100 || rows[0].PricingBasis != store.PricingTokenDerived {
		t.Fatalf("non-chargeable priced tokens must retain list value: rows=%+v want cost=%d", rows, want)
	}
	if f.metrics.counts != (Counts{Events: 1, Fallback: 1, NonChargeable: 1}) || !strings.Contains(f.logs.String(), "nonchargeable_zero_cost_skipped=1") {
		t.Fatalf("only unvalued non-chargeable usage is skipped: counts=%+v logs=%s", f.metrics.counts, f.logs.String())
	}
}

func TestCursorBYOSkipNeverChangesRows(t *testing.T) {
	f := newFixture(t)
	ordinary := event("claude-sonnet-4-5", 100)
	f.events = []usageEvent{ordinary}
	f.poll(t)
	before := f.rows(t)
	byo := ordinary
	byo.Kind = "BYO API KeY traffic"
	byo.TokenUsage = &tokenUsage{Input: 2000000, TotalCents: 999999}
	f.events = append(f.events, byo)
	c := f.poll(t)
	if c.BYO != 1 || c.Events != 0 || !reflect.DeepEqual(before, f.rows(t)) {
		t.Fatalf("BYO altered rows, counts=%+v rows=%+v", c, f.rows(t))
	}
	f.events = []usageEvent{ordinary}
	c = f.poll(t)
	if c.Events != 0 || !reflect.DeepEqual(before, f.rows(t)) {
		t.Fatal("removing BYO event reduced a row")
	}
}

func TestCursorVerifiedUserAPIKeyKindSkipped(t *testing.T) {
	f := newFixture(t)
	// Pin the kind and flags observed live in Cursor on 2026-10-21.
	f.events = []usageEvent{{
		Timestamp:        strconv.FormatInt(testDay.Add(time.Hour).UnixMilli(), 10),
		Email:            "private@example.test",
		Model:            "claude-sonnet-4-5",
		Kind:             "User API Key",
		IsChargeable:     true,
		IsTokenBasedCall: false,
		ChargedCents:     1, // Synthetic fee amount; the observation established fee-only billing.
	}}
	if counts := f.poll(t); counts != (Counts{BYO: 1}) {
		t.Fatalf("verified BYO kind must be skipped, including Cursor's fee: %+v", counts)
	}
	if rows := f.rows(t); len(rows) != 0 {
		t.Fatalf("BYO event stored spend: %+v", rows)
	}
}

func TestCursorIdentityPrivacy(t *testing.T) {
	f := newFixture(t)
	f.roster(t)
	f.alias(t, "mapped@example.test", "alice")
	f.alias(t, "outside@example.test", "not-on-roster")
	for _, email := range []string{"mapped@example.test", "unmapped@example.test", "outside@example.test"} {
		e := event("gpt-4o", 100)
		e.Email = email
		f.events = append(f.events, e)
	}
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ usageRequest) bool {
		data, _ := json.Marshal(f.events)
		data = bytes.ReplaceAll(data, []byte(`"timestamp":`), []byte(`"conversationId":"private-conversation","unknownAccountName":"private-service-name","timestamp":`))
		_, _ = fmt.Fprintf(w, `{"totalUsageEventsCount":3,"pagination":{"currentPage":1,"numPages":1},"usageEvents":%s}`, data)
		return true
	}
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	rows := f.rows(t)
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	totals := map[string]int{}
	for _, r := range rows {
		totals[r.Developer] += r.InputTok
	}
	if totals["alice"] != 100 || totals[collector.UnattributedIssueID] != 200 {
		t.Fatalf("identity=%v", totals)
	}
	data, _ := json.Marshal(rows)
	for _, secret := range []string{"mapped@example.test", "unmapped@example.test", "outside@example.test", "private-conversation", "private-service-name", testKey} {
		if strings.Contains(string(data), secret) || strings.Contains(f.logs.String(), secret) {
			t.Fatalf("PII leaked: %s", secret)
		}
	}
	// Verify every key was constructed entirely from the allowed, non-email tuple.
	for _, r := range rows {
		want := collector.IdempotencyKey(collector.SourceCursorAdmin, testDay.Format("2006-01-02"), r.Developer, r.Model, r.PricingBasis, "1")
		if r.IdempotencyKey != want {
			t.Fatalf("key=%s want %s", r.IdempotencyKey, want)
		}
	}
}

func TestCursorNullAutomationIdentity(t *testing.T) {
	const nullFields = `"serviceAccountId":"null","serviceAccountName":"null","cloudAgentId":"null","automationId":"null"`
	for _, tc := range []struct {
		name             string
		humanFields      string
		automationFields string
	}{
		{name: "null service account id", humanFields: `"serviceAccountId":"null"`},
		{name: "all null fields", humanFields: nullFields},
		{name: "trimmed case folded null fields", humanFields: `"serviceAccountId":" NuLl ","serviceAccountName":"\tNULL\n","cloudAgentId":" null ","automationId":"nUlL"`},
		{name: "real service account stays unattributed", humanFields: nullFields, automationFields: `"serviceAccountId":"sa_123","serviceAccountName":"null","cloudAgentId":"null","automationId":"null"`},
		{name: "headless stays unattributed", humanFields: nullFields, automationFields: nullFields + `,"isHeadless":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.roster(t)
			f.alias(t, "PRIVATE@example.test", "alice")
			// Use literal JSON strings from the API, not null or Go zero values.
			wireEvent := func(fields string) string {
				return fmt.Sprintf(`{"timestamp":"%d","userEmail":"private@EXAMPLE.test","model":"gpt-4o","isTokenBasedCall":true,"isChargeable":true,"tokenUsage":{"inputTokens":100},%s}`, testDay.Add(time.Hour).UnixMilli(), fields)
			}
			events := wireEvent(tc.humanFields)
			wantRows := 1
			if tc.automationFields != "" {
				events += "," + wireEvent(tc.automationFields)
				wantRows++
			}
			f.handle = func(w http.ResponseWriter, _ *http.Request, _ usageRequest) bool {
				_, _ = fmt.Fprintf(w, `{"totalUsageEventsCount":%d,"pagination":{"currentPage":1,"numPages":1},"usageEvents":[%s]}`, wantRows, events)
				return true
			}
			f.poll(t)
			rows := f.rows(t)
			if len(rows) != wantRows {
				t.Fatalf("rows=%+v; want %d", rows, wantRows)
			}
			wantCost, _ := store.ComputeCostHost("", "gpt-4o", store.CostUsage{Input: 100})
			seen := map[string]bool{}
			for _, row := range rows {
				if row.InputTok != 100 || row.CostMicro != wantCost || seen[row.Developer] {
					t.Fatalf("incorrect attribution or spend: %+v", rows)
				}
				seen[row.Developer] = true
			}
			if !seen["alice"] || (tc.automationFields != "" && !seen[collector.UnattributedIssueID]) {
				t.Fatalf("human must be attributed and automation unattributed: %+v", rows)
			}
		})
	}
}

func TestCursorServiceAndCloudAgentsUnattributed(t *testing.T) {
	f := newFixture(t)
	f.roster(t)
	f.alias(t, "private@example.test", "alice")
	for _, kind := range []string{"service", "service-name", "cloud", "automation", "headless"} {
		e := event("gpt-4o", 100)
		switch kind {
		case "service":
			e.ServiceAccountID = "service-id"
		case "service-name":
			e.ServiceAccountName = "service-name"
		case "cloud":
			e.CloudAgentID = "cloud-id"
		case "automation":
			e.AutomationID = "automation-id"
		case "headless":
			e.IsHeadless = true
		}
		f.events = append(f.events, e)
	}
	f.poll(t)
	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Developer != collector.UnattributedIssueID || rows[0].InputTok != 500 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestCursorPaginationAndInclusiveWindow(t *testing.T) {
	f := newFixture(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		if req.Page < 1 || req.Page > 2 || req.PageSize != 1000 || req.StartDate != testDay.UnixMilli() || req.EndDate != testDay.AddDate(0, 0, 1).UnixMilli()-1 {
			t.Errorf("request=%+v", req)
		}
		respond(w, []usageEvent{event("gpt-4o", 100)}, req.Page, 2, req.Page == 1)
		return true
	}
	f.poll(t)
	rows := f.rows(t)
	if len(f.requests) != 2 || len(rows) != 1 || rows[0].InputTok != 200 || len(f.sleeps) != 1 {
		t.Fatalf("requests=%v rows=%+v sleeps=%v", f.requests, rows, f.sleeps)
	}
}

func TestCursorRetriesAndLoudAuth(t *testing.T) {
	for _, status := range []int{429, 500, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFixture(t)
			attempts := 0
			f.handle = func(w http.ResponseWriter, _ *http.Request, _ usageRequest) bool {
				attempts++
				if attempts == 1 || status == 401 || status == 403 {
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(testKey + " private@example.test Enterprise access required"))
					return true
				}
				return false
			}
			f.events = []usageEvent{event("gpt-4o", 1)}
			f.poller.runPass(t.Context(), ingester.Store(f.db))
			if status == 401 || status == 403 {
				if attempts != 1 || f.metrics.bad != 1 || len(f.metrics.auth) != 1 || f.metrics.auth[0] != status || !strings.Contains(f.logs.String(), "level=ERROR") || !strings.Contains(f.logs.String(), AuthHint) || !strings.Contains(f.logs.String(), fmt.Sprint(status)) || len(f.rows(t)) != 0 {
					t.Fatalf("attempts=%d metrics=%+v logs=%s", attempts, f.metrics, f.logs.String())
				}
			} else {
				if attempts != 2 || f.metrics.good != 1 || len(f.rows(t)) != 1 || len(f.sleeps) < 1 {
					t.Fatalf("retry failed: attempts=%d metrics=%+v sleeps=%v", attempts, f.metrics, f.sleeps)
				}
				found := false
				for _, d := range f.sleeps {
					if d >= 2*time.Second {
						found = true
					}
				}
				if status == 429 && !found {
					t.Fatalf("Retry-After ignored: %v", f.sleeps)
				}
			}
			if strings.Contains(f.logs.String(), testKey) || strings.Contains(f.logs.String(), "private@example.test") {
				t.Fatalf("secret in log: %s", f.logs.String())
			}
		})
	}
}

func TestCursorIncompleteDayNeverReconciles(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("gpt-4o", 100)}
	f.poll(t)
	before := f.rows(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		if req.Page == 1 {
			respond(w, []usageEvent{event("gpt-4o", 1)}, 1, 2, true)
		} else {
			w.WriteHeader(503)
		}
		return true
	}
	_, err := f.poller.pollOnce(t.Context(), ingester.Store(f.db))
	if err == nil || !reflect.DeepEqual(before, f.rows(t)) {
		t.Fatalf("partial day reconciled: err=%v rows=%+v", err, f.rows(t))
	}
}

func TestCursorDayWideReconcileUpDownAliasMove(t *testing.T) {
	f := newFixture(t)
	f.roster(t)
	for _, n := range []int{100, 200, 50, 50} {
		f.events = []usageEvent{event("gpt-4o", n)}
		f.poll(t)
	}
	if len(f.rows(t)) != 3 {
		t.Fatalf("up/down/retry rows=%+v", f.rows(t))
	}
	f.alias(t, "private@example.test", "alice")
	f.poll(t)
	rows := f.rows(t)
	if len(rows) != 5 {
		t.Fatalf("alias move rows=%+v", rows)
	}
	negative := false
	for _, r := range rows {
		if r.Developer == collector.UnattributedIssueID && r.InputTok == -50 && r.CostMicro < 0 {
			negative = true
		}
	}
	if !negative {
		t.Fatal("alias move did not reverse old key")
	}
	for k, v := range f.net(t) {
		want := 0
		if k.developer == "alice" {
			want = 50
		}
		if v.usage.Input != want || v.cost < 0 {
			t.Fatalf("net key=%+v total=%+v", k, v)
		}
	}
	// Removing the last key must reconcile it to zero too, without touching other sources.
	if err := f.db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "other", Model: "gpt-4o", Source: collector.SourceProxy, InputTok: 7, CostMicro: 123, Timestamp: testDay}); err != nil {
		t.Fatal(err)
	}
	f.events = nil
	f.poll(t)
	for k, v := range f.net(t) {
		if v.usage != (store.CostUsage{}) || v.cost != 0 {
			t.Fatalf("disappeared key=%+v net=%+v", k, v)
		}
	}
	last := f.rows(t)
	found := false
	for _, r := range last {
		if r.Source == collector.SourceProxy {
			found = true
			if r.InputTok != 7 || r.CostMicro != 123 {
				t.Fatal("other source changed")
			}
		}
	}
	if !found {
		t.Fatal("other source missing")
	}
}

func TestCursorClampNeverMakesNetNegative(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("gpt-4o", 100)}
	f.poll(t)
	k := rowKey{collector.UnattributedIssueID, "gpt-4o", store.PricingTokenDerived}
	counts := Counts{}
	deltas, err := f.poller.reconcile(t.Context(), testDay, map[rowKey]total{k: {usage: store.CostUsage{Input: -200, Output: -1}, cost: -100}}, &counts)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Clamped != 3 || len(deltas) != 1 || deltas[0].InputTok != -100 || deltas[0].OutputTok != 0 {
		t.Fatalf("clamp=%+v deltas=%+v", counts, deltas)
	}
	for _, e := range deltas {
		if err := ingester.Store(f.db).Ingest(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range f.net(t) {
		if v.cost != 0 || v.usage != (store.CostUsage{}) {
			t.Fatalf("net=%+v", v)
		}
	}
}

func TestCursorTimestampString(t *testing.T) {
	for _, timestamp := range []string{`"` + strconv.FormatInt(testDay.UnixMilli(), 10) + `"`, `123`, `"private@example.test"`, `"1"`} {
		t.Run(timestamp, func(t *testing.T) {
			f := newFixture(t)
			f.handle = func(w http.ResponseWriter, _ *http.Request, _ usageRequest) bool {
				_, _ = fmt.Fprintf(w, `{"totalUsageEventsCount":1,"pagination":{"numPages":1,"currentPage":1},"usageEvents":[{"timestamp":%s,"model":"gpt-4o","isChargeable":true,"chargedCents":1}]}`, timestamp)
				return true
			}
			counts, err := f.poller.pollOnce(t.Context(), ingester.Store(f.db))
			valid := strings.HasPrefix(timestamp, `"17`)
			if valid {
				if err != nil || counts.Events != 1 {
					t.Fatalf("valid epoch string: counts=%+v err=%v", counts, err)
				}
			} else if err == nil || len(f.rows(t)) != 0 || strings.Contains(err.Error(), "private@example.test") {
				t.Fatalf("invalid timestamp counts=%+v err=%v", counts, err)
			}
		})
	}
}

func TestRepriceCursorDailySumsUntouched(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("claude-sonnet-4-5", 150000), event("claude-sonnet-4-5", 150000)}
	unknown := event("unknown-model", 100)
	unknown.TokenUsage.TotalCents = 20
	f.events = append(f.events, unknown)
	f.poll(t)
	before := f.rows(t)
	result, err := f.db.Reprice(t.Context(), store.RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.rows(t)) || result.ExcludedRowCount != int64(len(before)) {
		t.Fatalf("cursor daily sums repriced: result=%+v before=%+v after=%+v; requires #1339 source exclusion", result, before, f.rows(t))
	}
}

func TestCursorProbeOneMinimalRequest(t *testing.T) {
	f := newFixture(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		today := time.Now().UTC().Truncate(24 * time.Hour)
		if req.PageSize != 1 || req.Page != 1 || req.StartDate != today.AddDate(0, 0, -1).UnixMilli() || req.EndDate != today.UnixMilli()-1 {
			t.Errorf("probe=%+v", req)
		}
		_, _ = w.Write([]byte(`{"totalUsageEventsCount":73,"usageEvents":[{"userEmail":"private@example.test","conversationId":"never-store"}]}`))
		return true
	}
	r := f.poller.Client.Probe(t.Context())
	if r.Err != nil || r.StatusCode != 200 || r.EventCount != 73 || len(f.requests) != 1 || len(f.rows(t)) != 0 {
		t.Fatalf("probe=%+v requests=%v", r, f.requests)
	}
}

func TestCursorDefaultSettledWindowAndSeal(t *testing.T) {
	f := newFixture(t)
	var from, through time.Time
	f.poller = NewPoller(PollerConfig{Client: f.poller.Client, Store: f.db, Logger: f.poller.Logger, Now: func() time.Time { return testNow }, Settled: func(_ context.Context, a, b time.Time) { from, through = a, b }})
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if len(f.requests) != 7 || !from.Equal(testDay.AddDate(0, 0, -6)) || !through.Equal(testDay.AddDate(0, 0, 1)) || f.poller.Interval != time.Hour {
		t.Fatalf("requests=%v watermark=%s..%s interval=%s", f.requests, from, through, f.poller.Interval)
	}
	for i, req := range f.requests {
		want := testDay.AddDate(0, 0, i-6)
		if req.StartDate != want.UnixMilli() || req.EndDate != want.AddDate(0, 0, 1).UnixMilli()-1 {
			t.Fatalf("window request=%+v", req)
		}
	}
}

func TestCursorUncostedDoesNotSeal(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("unknown", 123)}
	settled := false
	f.poller.Settled = func(context.Context, time.Time, time.Time) { settled = true }
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if settled || f.metrics.counts.Uncosted != 1 || f.metrics.good != 1 || !strings.Contains(f.logs.String(), "uncosted=1") {
		t.Fatalf("settled=%v metrics=%+v logs=%s", settled, f.metrics, f.logs.String())
	}
}

func TestCursorEmptyDayReversesOldKey(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("gpt-4o", 1)}
	f.poll(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ usageRequest) bool {
		respond(w, nil, 1, 0, false)
		return true
	}
	c := f.poll(t)
	if c.Events != 1 {
		t.Fatalf("empty day did not reverse old key: %+v", c)
	}
	for _, a := range f.net(t) {
		if a.cost != 0 || a.usage != (store.CostUsage{}) {
			t.Fatalf("empty day net=%+v", a)
		}
	}
}

func TestCursorPaginationCountMismatchNeverReconciles(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("gpt-4o", 10)}
	f.poll(t)
	before := f.rows(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ usageRequest) bool {
		_, _ = w.Write([]byte(`{"totalUsageEventsCount":2,"pagination":{"currentPage":1,"numPages":1},"usageEvents":[]}`))
		return true
	}
	_, err := f.poller.pollOnce(t.Context(), ingester.Store(f.db))
	if err == nil || !reflect.DeepEqual(before, f.rows(t)) {
		t.Fatalf("truncated 200 response reconciled: err=%v", err)
	}
}

func TestCursorPricingBasisKeysStaySeparate(t *testing.T) {
	f := newFixture(t)
	token := event("gpt-4o", 100)
	request := event("gpt-4o", 0)
	request.IsTokenBasedCall = false
	request.TokenUsage = nil
	request.ChargedCents = 3
	f.events = []usageEvent{token, request}
	f.poll(t)
	if len(f.rows(t)) != 2 {
		t.Fatalf("pricing bases merged: %+v", f.rows(t))
	}
	f.events = []usageEvent{token}
	f.poll(t)
	for k, a := range f.net(t) {
		if k.basis == store.PricingCallerAuthoritative && a.cost != 0 {
			t.Fatalf("fallback did not reverse: %+v", a)
		}
		if k.basis == store.PricingTokenDerived && a.usage.Input != 100 {
			t.Fatalf("list row changed: %+v", a)
		}
	}
}

func TestCursorLargeDailyCorrectionAndRestart(t *testing.T) {
	f := newFixture(t)
	request := event("request-based", 0)
	request.IsTokenBasedCall = false
	request.TokenUsage = nil
	request.ChargedCents = 2500000 // $25K > store's per-row ceiling
	f.events = []usageEvent{request}
	f.poll(t)
	if len(f.rows(t)) != 3 {
		t.Fatalf("daily correction clipped: %+v", f.rows(t))
	}
	for k, a := range f.net(t) {
		if a.cost != 25_000_000_000 || k.basis != store.PricingCallerAuthoritative {
			t.Fatalf("net=%+v key=%+v", a, k)
		}
	}
	// A fresh poller derives sequence state from stored rows, without amounts in keys.
	f.poller = NewPoller(f.poller.PollerConfig)
	f.poll(t)
	if len(f.rows(t)) != 3 {
		t.Fatal("restart duplicated daily total")
	}
	request.ChargedCents = 100
	f.events = []usageEvent{request}
	f.poll(t)
	for _, a := range f.net(t) {
		if a.cost != 1_000_000 {
			t.Fatalf("downward correction=%+v", a)
		}
	}
}

func TestCursorClientNeverFollowsRedirect(t *testing.T) {
	f := newFixture(t)
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; w.WriteHeader(200) }))
	defer target.Close()
	f.handle = func(w http.ResponseWriter, r *http.Request, _ usageRequest) bool {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		return true
	}
	r := f.poller.Client.Probe(t.Context())
	if r.StatusCode != 307 || r.Err != nil || targetCalls != 0 {
		t.Fatalf("redirect followed: probe=%+v targetCalls=%d", r, targetCalls)
	}
}

func TestCursorPerEventCacheAndLongContext(t *testing.T) {
	f := newFixture(t)
	e := event("claude-sonnet-4-5", 150000)
	e.TokenUsage.CacheRead = 100000
	e.TokenUsage.CacheWrite = 100
	e.TokenUsage.Output = 1000
	f.events = []usageEvent{e}
	f.poll(t)
	rows := f.rows(t)
	// 250100 input-context tokens select $6/$22.50 per M; cache read is
	// 0.1x and ordinary cache write is 1.25x. Each class is counted once.
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	r := rows[0]
	if r.CostMicro != 983250 || r.InputTok != 150000 || r.CacheRead != 100000 || r.CacheWrite5m != 100 || r.CacheWrite1h != 0 || r.OutputTok != 1000 {
		t.Fatalf("request pricing/mapping=%+v", r)
	}
}

func TestCursorFreeFallbackDoesNotStallSeal(t *testing.T) {
	f := newFixture(t)
	free := event("unknown-included-model", 123)
	free.IsChargeable = false
	f.events = []usageEvent{free}
	var through time.Time
	f.poller.Settled = func(_ context.Context, _, end time.Time) { through = end }
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if !through.Equal(testDay.AddDate(0, 0, 1)) || f.metrics.counts.Uncosted != 0 || f.metrics.counts.NonChargeable != 1 {
		t.Fatalf("through=%s counts=%+v", through, f.metrics.counts)
	}
}

func TestCursorLateUncostedDayRecordsLoss(t *testing.T) {
	f := newFixture(t)
	f.poller.LookbackDays = 3
	now, late := testNow, false
	f.poller.Now = func() time.Time { return now }
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		var events []usageEvent
		if late && req.StartDate == testDay.UnixMilli() {
			events = []usageEvent{event("unknown", 123)}
		}
		respond(w, events, 1, 1, false)
		return true
	}
	var losses [][2]time.Time
	f.poller.Lost = func(_ context.Context, from, through time.Time) error {
		losses = append(losses, [2]time.Time{from, through})
		return nil
	}
	var settled time.Time
	f.poller.Settled = func(_ context.Context, _, through time.Time) {
		if late && len(losses) != 1 {
			t.Error("late loss must be recorded before settling")
		}
		settled = through
	}
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if !settled.Equal(testDay.AddDate(0, 0, 1)) {
		t.Fatalf("clean pass did not certify D: through=%s", settled)
	}
	// An earlier clock must not lower the maximum certification boundary.
	now = testNow.AddDate(0, 0, -1)
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	now, late = testNow, true
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	want := [][2]time.Time{{testDay, testDay.AddDate(0, 0, 1)}}
	if !reflect.DeepEqual(losses, want) || len(f.poller.uncostedByDay) != 0 {
		t.Fatalf("late discovery: losses=%v pins=%v", losses, f.poller.uncostedByDay)
	}
}

func TestCursorFinalReadUncostedDayRecordsLoss(t *testing.T) {
	f := newFixture(t)
	f.poller.LookbackDays = 3
	now := testNow.AddDate(0, 0, -1)
	f.poller.Now = func() time.Time { return now }
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		var events []usageEvent
		if req.StartDate == testDay.UnixMilli() {
			events = []usageEvent{event("unknown", 123)}
		}
		respond(w, events, 1, 1, false)
		return true
	}
	var losses [][2]time.Time
	f.poller.Lost = func(_ context.Context, from, through time.Time) error {
		losses = append(losses, [2]time.Time{from, through})
		return nil
	}
	var through time.Time
	f.poller.Settled = func(_ context.Context, _, end time.Time) { through = end }
	f.poller.runPass(t.Context(), ingester.Store(f.db)) // Certify only through D's start.
	if !through.Equal(testDay) {
		t.Fatalf("initial through=%s, want D", through)
	}
	now = testNow
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if len(losses) != 0 || !through.Equal(testDay) || f.poller.uncostedByDay[testDay] != 1 {
		t.Fatalf("day with another read available must stay pinned: losses=%v through=%s", losses, through)
	}
	now = testNow.AddDate(0, 0, 2) // D is now the window start: its final read.
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	want := [][2]time.Time{{testDay, testDay.AddDate(0, 0, 1)}}
	if !reflect.DeepEqual(losses, want) || len(f.poller.uncostedByDay) != 0 || !through.Equal(now.Truncate(24*time.Hour)) {
		t.Fatalf("final read: losses=%v pins=%v through=%s", losses, f.poller.uncostedByDay, through)
	}
	now = now.AddDate(0, 0, 1)
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if len(losses) != 1 || !through.Equal(now.Truncate(24*time.Hour)) {
		t.Fatalf("lost day still pins next pass: losses=%v through=%s", losses, through)
	}
}

func TestCursorFailedLossKeepsDayPinned(t *testing.T) {
	f := newFixture(t)
	f.poller.LookbackDays = 2
	now := testNow
	f.poller.Now = func() time.Time { return now }
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		var events []usageEvent
		if req.StartDate == testDay.UnixMilli() {
			events = []usageEvent{event("unknown", 123)}
		}
		respond(w, events, 1, 1, false)
		return true
	}
	attempts, fail := 0, true
	f.poller.Lost = func(_ context.Context, from, through time.Time) error {
		attempts++
		if !from.Equal(testDay) || !through.Equal(testDay.AddDate(0, 0, 1)) {
			t.Errorf("loss span=%s..%s", from, through)
		}
		if fail {
			return fmt.Errorf("loss store unavailable")
		}
		return nil
	}
	var spans [][2]time.Time
	f.poller.Settled = func(_ context.Context, from, through time.Time) { spans = append(spans, [2]time.Time{from, through}) }
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	// Even after the final read, retry the failed loss and keep certification held.
	now = now.AddDate(0, 0, 2)
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if attempts != 2 || f.poller.uncostedByDay[testDay] != 1 || len(spans) != 1 || !spans[0][1].Equal(testDay) || !strings.Contains(f.logs.String(), "level=ERROR") || !strings.Contains(f.logs.String(), "could not record lost spend") {
		t.Fatalf("failed loss did not hold: attempts=%d pins=%v spans=%v logs=%s", attempts, f.poller.uncostedByDay, spans, f.logs.String())
	}
	fail = false
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if attempts != 3 || len(f.poller.uncostedByDay) != 0 || len(spans) != 2 || !spans[1][1].Equal(now.Truncate(24*time.Hour)) {
		t.Fatalf("successful retry did not release pin: attempts=%d pins=%v spans=%v", attempts, f.poller.uncostedByDay, spans)
	}
}

func TestCursorFreshPollerUncostedDayRecordsLoss(t *testing.T) {
	f := newFixture(t)
	f.poller.LookbackDays = 3 // D is not on its final read.
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		var events []usageEvent
		if req.StartDate == testDay.UnixMilli() {
			events = []usageEvent{event("unknown", 123)}
		}
		respond(w, events, 1, 1, false)
		return true
	}
	var losses [][2]time.Time
	f.poller.Lost = func(_ context.Context, from, through time.Time) error {
		losses = append(losses, [2]time.Time{from, through})
		return nil
	}
	var through time.Time
	f.poller.Settled = func(_ context.Context, _, end time.Time) {
		if len(losses) != 1 {
			t.Error("unknown prior certification requires loss before settling")
		}
		through = end
	}
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	want := [][2]time.Time{{testDay, testDay.AddDate(0, 0, 1)}}
	if !reflect.DeepEqual(losses, want) || len(f.poller.uncostedByDay) != 0 || !through.Equal(testDay.AddDate(0, 0, 1)) {
		t.Fatalf("fresh process: losses=%v pins=%v through=%s", losses, f.poller.uncostedByDay, through)
	}
}

func TestCursorUncostedPerDaySealGateSurvivesLookback(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.RegisterSources(t.Context(), []string{collector.SourceCursorAdmin}); err != nil {
		t.Fatal(err)
	}
	f.poller.Lost = func(ctx context.Context, from, through time.Time) error {
		return f.db.RecordSourceLoss(ctx, collector.SourceCursorAdmin, from, through)
	}
	f.poller.LookbackDays = 3
	now := testNow
	f.poller.Now = func() time.Time { return now }
	blocked := testDay.AddDate(0, 0, -1)
	uncosted := true
	f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
		var events []usageEvent
		if uncosted && req.StartDate == blocked.UnixMilli() {
			e := event("unknown", 123)
			e.Timestamp = strconv.FormatInt(blocked.Add(time.Hour).UnixMilli(), 10)
			events = []usageEvent{e}
		}
		respond(w, events, 1, 1, false)
		return true
	}
	var spans [][2]time.Time
	f.poller.Settled = func(ctx context.Context, from, through time.Time) {
		spans = append(spans, [2]time.Time{from, through})
		if err := f.db.AdvanceSourceWatermark(ctx, collector.SourceCursorAdmin, from, through); err != nil {
			t.Fatal(err)
		}
	}
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	// Unknown prior certification records a loss, replacing the in-memory pin.
	if len(spans) != 1 || !spans[0][1].Equal(testDay.AddDate(0, 0, 1)) {
		t.Errorf("recorded loss must release the settlement pin: %v", spans)
	}
	// Once the day ages out, settlement can advance; its month remains blocked.
	now = testNow.AddDate(0, 0, 4)
	uncosted = false
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if len(spans) != 2 || !spans[1][1].Equal(now.Truncate(24*time.Hour)) {
		t.Errorf("recorded loss still pinned settlement: %v", spans)
	}
	// A clean re-read cannot erase a recorded loss: the month needs seal --skip.
	now = testNow
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if len(spans) == 0 || !spans[len(spans)-1][1].Equal(testDay.AddDate(0, 0, 1)) {
		t.Fatalf("resolved day still blocked: %v", spans)
	}
	watermarks, err := f.db.SourceWatermarks(t.Context())
	if err != nil || len(watermarks) != 1 {
		t.Fatalf("watermarks=%v err=%v", watermarks, err)
	}
	if _, lost := watermarks[0].LostIn(blocked, blocked.AddDate(0, 0, 1)); !lost || watermarks[0].Covers(blocked, blocked.AddDate(0, 0, 1)) {
		t.Fatalf("uncosted day lost its persistent seal block: %+v", watermarks[0])
	}
}

func TestCursorEmailMatchingCaseFolded(t *testing.T) {
	f := newFixture(t)
	f.roster(t)
	f.alias(t, "Private@EXAMPLE.test", "alice")
	e := event("gpt-4o", 100)
	e.Email = "PRIVATE@example.TEST"
	f.events = []usageEvent{e}
	f.poll(t)
	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Developer != "alice" {
		t.Fatalf("case-folded identity: %+v", rows)
	}
}

func TestCursorCaseFoldedAliasCollision(t *testing.T) {
	for _, sameDeveloper := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameDeveloper=%v", sameDeveloper), func(t *testing.T) {
			f := newFixture(t)
			f.roster(t)
			if err := f.db.UpsertHierarchy(t.Context(), "bob", "team", "division", "org", "operator"); err != nil {
				t.Fatal(err)
			}
			other, want := "bob", collector.UnattributedIssueID
			if sameDeveloper {
				other, want = "alice", "alice"
			}
			f.alias(t, "Person@x", "alice")
			f.alias(t, "person@x", other)
			f.alias(t, "PERSON@x", "alice")
			e := event("gpt-4o", 100)
			e.Email = "pErSoN@x"
			f.events = []usageEvent{e}
			// Repeated map iterations must never restore an ambiguous identity.
			for range 20 {
				f.poll(t)
				rows := f.rows(t)
				if len(rows) != 1 || rows[0].Developer != want {
					t.Fatalf("case-folded collision: rows=%+v want developer=%s", rows, want)
				}
				data, err := json.Marshal(rows)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(strings.ToLower(string(data)), "person@x") || strings.Contains(strings.ToLower(f.logs.String()), "person@x") {
					t.Fatal("case-folded collision stored or logged email")
				}
			}
		})
	}
}

func TestCursorIncompletePassDoesNotAddCounters(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			f := newFixture(t)
			f.poller.LookbackDays = 2
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// First day ingests a delta; second day fails or cancels after observations exist.
			f.handle = func(w http.ResponseWriter, _ *http.Request, req usageRequest) bool {
				if req.StartDate == testDay.UnixMilli() {
					if cancelled {
						cancel()
					}
					w.WriteHeader(403)
				} else {
					e := event("unknown", 1)
					e.Timestamp = strconv.FormatInt(req.StartDate, 10)
					e.TokenUsage.TotalCents = 1
					respond(w, []usageEvent{e}, 1, 1, false)
				}
				return true
			}
			f.poller.runPass(ctx, ingester.Store(f.db))
			if f.metrics.counts != (Counts{}) || f.metrics.events != 0 {
				t.Fatalf("partial counts published: %+v", f.metrics)
			}
			if cancelled && f.metrics.good+f.metrics.bad != 0 {
				t.Fatalf("cancelled pass counted: %+v", f.metrics)
			}
			if !cancelled && f.metrics.bad != 1 {
				t.Fatalf("failed pass not counted: %+v", f.metrics)
			}
		})
	}
}

type unexpectedBasisStore struct{ Store }

func (s unexpectedBasisStore) ListTokenEvents(ctx context.Context, from, through time.Time, cursor store.PageCursor, limit int) ([]store.TokenEvent, bool, error) {
	rows, more, err := s.Store.ListTokenEvents(ctx, from, through, cursor, limit)
	for i := range rows {
		if rows[i].Source == collector.SourceCursorAdmin {
			rows[i].PricingBasis = "basis_unavailable"
		}
	}
	return rows, more, err
}

func TestCursorUnexpectedStoredBasisFailsDay(t *testing.T) {
	f := newFixture(t)
	f.events = []usageEvent{event("gpt-4o", 100)}
	f.poll(t)
	before := f.rows(t)
	f.poller.Store = unexpectedBasisStore{f.db}
	settled := false
	f.poller.Settled = func(context.Context, time.Time, time.Time) { settled = true }
	f.poller.runPass(t.Context(), ingester.Store(f.db))
	if f.metrics.bad != 1 || settled || !strings.Contains(f.logs.String(), "unexpected stored pricing basis") || !reflect.DeepEqual(before, f.rows(t)) {
		t.Fatalf("unexpected basis must fail without deltas: metrics=%+v settled=%v logs=%s rows=%+v", f.metrics, settled, f.logs.String(), f.rows(t))
	}
}
