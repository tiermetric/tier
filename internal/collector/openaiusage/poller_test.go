package openaiusage

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// fixedNow pins the settlement window: cutoff = now − 24h = 2026-06-19T12:00Z,
// window start (prev month) = 2026-05-01. Days ending on/before the cutoff settle.
var fixedNow = time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

const testAPIKey = "sk-openai-admin-SECRETVALUE-do-not-log"

// --- fakes -------------------------------------------------------------------

type fakeStore struct {
	captured map[string]map[string]store.CostUsage // day (YYYY-MM-DD) -> normModel -> usage
	net      map[string]int64                      // (period|source) -> micro
	posted   []store.OrgActualSpend
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		captured: map[string]map[string]store.CostUsage{},
		net:      map[string]int64{},
	}
}

func netKey(period, source string) string { return period + "|" + source }

func (f *fakeStore) CapturedTokensByDayModel(_ context.Context, day time.Time, _ string) (map[string]store.CostUsage, error) {
	return f.captured[day.UTC().Format("2006-01-02")], nil
}

// OrgActualSpendNet is source-scoped, mirroring the real store (#138 R1).
func (f *fakeStore) OrgActualSpendNet(_ context.Context, _, period, source string) (int64, error) {
	return f.net[netKey(period, source)], nil
}

func (f *fakeStore) InsertOrgActualSpend(_ context.Context, o store.OrgActualSpend) error {
	src := o.Source
	if src == "" {
		src = store.OrgSpendSourceManual // mirror the store's COALESCE default
	}
	f.posted = append(f.posted, o)
	f.net[netKey(o.Period, src)] += o.ActualPaidMicro // reflect accumulation, as the real store does
	return nil
}

type recordingIngester struct {
	events []collector.TokenEvent
}

func (r *recordingIngester) Ingest(_ context.Context, ev collector.TokenEvent) error {
	r.events = append(r.events, ev)
	return nil
}

// --- fixture builders (white-box: uses the package's response structs) --------

// dayBucket builds a usage bucket for the given UTC day (YYYY-MM-DD) with unix-
// second bounds — the OpenAI time format.
func dayBucket(day string, results ...usageResult) usageBucket {
	start, _ := time.Parse("2006-01-02", day)
	start = start.UTC()
	return usageBucket{
		StartTime: start.Unix(),
		EndTime:   start.Add(24 * time.Hour).Unix(),
		Results:   results,
	}
}

// modelResult builds one group_by=model usage row. inputTok is the RAW
// input_tokens (INCLUSIVE of cached, per OpenAI's surface); cached is
// input_cached_tokens (a subset of inputTok).
func modelResult(model string, inputTok, cached, outputTok int) usageResult {
	return usageResult{
		Model:             model,
		InputTokens:       inputTok,
		InputCachedTokens: cached,
		OutputTokens:      outputTok,
	}
}

// costDay builds one cost bucket for a UTC day with the nested dollars amount.
func costDay(day string, dollars ...float64) costBucket {
	start, _ := time.Parse("2006-01-02", day)
	start = start.UTC()
	results := make([]costResult, 0, len(dollars))
	for _, d := range dollars {
		results = append(results, costResult{Amount: costAmount{Value: d, Currency: "usd"}})
	}
	return costBucket{StartTime: start.Unix(), EndTime: start.Add(24 * time.Hour).Unix(), Results: results}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// newTestClient builds a Client pointed at srv with a no-op (recording) sleep so
// the retry path exercises with zero real delay.
func newTestClient(t *testing.T, srv *httptest.Server, sleeps *[]time.Duration) *Client {
	t.Helper()
	return NewClient(ClientConfig{
		APIKey:    testAPIKey,
		BaseURL:   srv.URL,
		RetryBase: time.Millisecond, // tiny; sleep is a no-op anyway
		Sleep: func(ctx context.Context, d time.Duration) error {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
			return ctx.Err()
		},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
}

func newTestPoller(client *Client, fs *fakeStore, logBuf *bytes.Buffer) *Poller {
	var logger *slog.Logger
	if logBuf != nil {
		logger = slog.New(slog.NewTextHandler(logBuf, nil))
	}
	return NewPoller(PollerConfig{
		Client:   client,
		Store:    fs,
		Org:      "acme",
		Interval: time.Hour,
		Logger:   logger,
		Now:      func() time.Time { return fixedNow },
	})
}

// eventKey identifies an emitted event for assertion.
type eventKey struct {
	model string
	day   string // YYYY-MM-DD of the settled day (bucket ending − 24h)
}

func indexEvents(events []collector.TokenEvent) map[eventKey]collector.TokenEvent {
	m := make(map[eventKey]collector.TokenEvent, len(events))
	for _, e := range events {
		day := e.Timestamp.UTC().Add(-24 * time.Hour).Format("2006-01-02")
		m[eventKey{e.Model, day}] = e
	}
	return m
}

// usageOnly serves the usage report on the usage path and an empty cost report
// everywhere else. costOnly is the mirror. Both assert the Bearer auth header.
func usageServer(t *testing.T, usage usageReport) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization header = %q, want Bearer <key>", got)
		}
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, usage))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
}

func costServer(t *testing.T, cost costReport) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, costPath) {
			_, _ = w.Write(mustJSON(t, cost))
			return
		}
		_, _ = w.Write(mustJSON(t, usageReport{}))
	}))
}

// --- tests -------------------------------------------------------------------

// TestPoll_CachedTokensSubtractedFromInput is the A1-shaped pin (spec test
// plan): OpenAI's input_tokens INCLUDES input_cached_tokens, so the event must
// carry InputTok = input − cached, CacheRead = cached, and be priced on exactly
// that split — never double-subtracting the cached tokens.
func TestPoll_CachedTokensSubtractedFromInput(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("gpt-4o", 1000, 900, 0)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	fs := newFakeStore() // no prior capture → full admin usage is the remainder
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 1 {
		t.Fatalf("got %d events, want 1", len(ing.events))
	}
	ev := ing.events[0]
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (input_tokens 1000 − cached 900)", ev.InputTok)
	}
	if ev.CacheRead != 900 {
		t.Errorf("CacheRead = %d, want 900 (the cached subset)", ev.CacheRead)
	}
	if ev.CacheWrite5m != 0 || ev.CacheWrite1h != 0 {
		t.Errorf("cache writes = %d/%d, want 0/0 (OpenAI has no write SKU)", ev.CacheWrite5m, ev.CacheWrite1h)
	}
	// Integer micro assertion: cost equals ComputeCost of exactly the carved split.
	wantCost := store.ComputeCost("gpt-4o", store.CostUsage{Input: 100, CacheRead: 900})
	if ev.CostMicro != wantCost {
		t.Errorf("CostMicro = %d, want %d (RPT list price of the carved 100 input + 900 cache-read)", ev.CostMicro, wantCost)
	}
	// Sanity: the carved split must NOT price the same as treating all 1000 as
	// uncached input — otherwise the carve-out is a no-op and the pin is vacuous.
	if allInput := store.ComputeCost("gpt-4o", store.CostUsage{Input: 1000}); ev.CostMicro == allInput {
		t.Errorf("carved cost %d equals all-input cost %d; the cache-read discount was not applied", ev.CostMicro, allInput)
	}
}

func TestPoll_MapsBucketsToRemainderEvents(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15",
			modelResult("gpt-4o", 1000, 200, 500), // input incl 200 cached
			modelResult("gpt-4.1", 2000, 0, 0)),
		dayBucket("2026-06-16",
			modelResult("gpt-4o", 300, 0, 0), // fully captured → no event
			modelResult("gpt-4.1", 100, 0, 50)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	fs := newFakeStore()
	// Captured baseline is in CARVED classes (Input excludes cache), matching how
	// the proxy/JSONL parser stores OpenAI events.
	fs.captured["2026-06-15"] = map[string]store.CostUsage{
		"gpt-4o": {Input: 400, Output: 100, CacheRead: 50}, // partial capture
	}
	fs.captured["2026-06-16"] = map[string]store.CostUsage{
		"gpt-4o":  {Input: 300}, // full capture
		"gpt-4.1": {Input: 40},  // partial
	}

	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	idx := indexEvents(ing.events)
	if len(ing.events) != 3 {
		t.Fatalf("got %d events, want 3 (0615 gpt-4o+gpt-4.1, 0616 gpt-4.1; 0616 gpt-4o fully captured)", len(ing.events))
	}

	// 0615 gpt-4o: admin carved = Input 800 (1000−200), CacheRead 200, Output 500.
	// remainder = admin − captured, per class.
	g4o := idx[eventKey{"gpt-4o", "2026-06-15"}]
	wantRem := store.CostUsage{Input: 400, Output: 400, CacheRead: 150}
	if g4o.InputTok != wantRem.Input || g4o.OutputTok != wantRem.Output || g4o.CacheRead != wantRem.CacheRead {
		t.Errorf("0615 gpt-4o remainder = in%d/out%d/cr%d, want %+v", g4o.InputTok, g4o.OutputTok, g4o.CacheRead, wantRem)
	}
	if g4o.Developer != collector.UnattributedIssueID || g4o.IssueID != collector.UnattributedIssueID {
		t.Errorf("0615 gpt-4o developer/issue = %q/%q, want unattributed", g4o.Developer, g4o.IssueID)
	}
	if g4o.Fidelity != collector.FidelityDaily || g4o.Source != collector.SourceOpenAIUsage {
		t.Errorf("0615 gpt-4o fidelity/source = %q/%q", g4o.Fidelity, g4o.Source)
	}
	if wantCost := store.ComputeCost("gpt-4o", wantRem); g4o.CostMicro != wantCost {
		t.Errorf("0615 gpt-4o CostMicro = %d, want %d", g4o.CostMicro, wantCost)
	}
	if g4o.Timestamp != time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) {
		t.Errorf("0615 gpt-4o Timestamp = %v, want bucket end 2026-06-16T00:00Z", g4o.Timestamp)
	}

	// 0615 gpt-4.1: no capture → full admin as remainder.
	g41 := idx[eventKey{"gpt-4.1", "2026-06-15"}]
	if g41.InputTok != 2000 {
		t.Errorf("0615 gpt-4.1 InputTok = %d, want 2000 (no prior capture)", g41.InputTok)
	}

	// 0616 gpt-4.1: partial capture 40 → remainder 60/50.
	g41_16 := idx[eventKey{"gpt-4.1", "2026-06-16"}]
	if g41_16.InputTok != 60 || g41_16.OutputTok != 50 {
		t.Errorf("0616 gpt-4.1 remainder = %d/%d, want 60/50", g41_16.InputTok, g41_16.OutputTok)
	}

	// 0616 gpt-4o: fully captured → absent.
	if _, ok := idx[eventKey{"gpt-4o", "2026-06-16"}]; ok {
		t.Errorf("0616 gpt-4o emitted; a fully-captured day must produce no remainder event (double-count guard)")
	}
}

func TestPoll_SettlementLagSkipsOpenDays(t *testing.T) {
	// Day 2026-06-19 ends 2026-06-20T00:00Z — only 12h before now → NOT settled.
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-19", modelResult("gpt-4o", 5000, 0, 5000)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 0 {
		t.Errorf("got %d events for an open (unsettled) day, want 0", len(ing.events))
	}
}

func TestPoll_NegativeRemainderClampsToZero(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("gpt-4o", 100, 0, 100)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	fs := newFakeStore()
	// Capture EXCEEDS admin (attribution slop) → negative remainder → clamp, WARN.
	fs.captured["2026-06-15"] = map[string]store.CostUsage{
		"gpt-4o": {Input: 500, Output: 500},
	}
	logBuf := &bytes.Buffer{}
	poller := newTestPoller(newTestClient(t, srv, nil), fs, logBuf)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 0 {
		t.Errorf("got %d events on over-capture, want 0 (clamped)", len(ing.events))
	}
	if !strings.Contains(logBuf.String(), "over_capture_tokens") {
		t.Errorf("expected an over-capture WARN; log = %q", logBuf.String())
	}
}

// TestPoll_CachedExceedsInputClamped guards the carve-out against a malformed
// report where input_cached_tokens > input_tokens (forbidden by the API's own
// semantics, but never trusted on the wire): InputTok must clamp to 0, not go
// negative, and CacheRead keeps the reported cached count.
func TestPoll_CachedExceedsInputClamped(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("gpt-4o", 100, 300, 0)), // cached > input
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 1 {
		t.Fatalf("got %d events, want 1", len(ing.events))
	}
	if ing.events[0].InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (clamped, never negative)", ing.events[0].InputTok)
	}
	if ing.events[0].CacheRead != 300 {
		t.Errorf("CacheRead = %d, want 300", ing.events[0].CacheRead)
	}
}

func TestPoll_IdempotencyKeyStableAcrossPolls(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("gpt-4o", 1000, 0, 0)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)

	ing1 := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing1); err != nil {
		t.Fatalf("pollOnce #1: %v", err)
	}
	ing2 := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing2); err != nil {
		t.Fatalf("pollOnce #2: %v", err)
	}
	if len(ing1.events) != 1 || len(ing2.events) != 1 {
		t.Fatalf("expected 1 event per poll, got %d and %d", len(ing1.events), len(ing2.events))
	}
	k1, k2 := ing1.events[0].IdempotencyKey, ing2.events[0].IdempotencyKey
	if k1 == "" {
		t.Fatalf("idempotency key is empty")
	}
	if k1 != k2 {
		t.Errorf("idempotency key drifted across polls: %q vs %q", k1, k2)
	}
	want := collector.IdempotencyKey(collector.SourceOpenAIUsage, "day", "2026-06-15", "gpt-4o")
	if k1 != want {
		t.Errorf("idempotency key = %q, want %q", k1, want)
	}
}

func TestPoll_Pagination(t *testing.T) {
	page1 := usageReport{
		Data:     []usageBucket{dayBucket("2026-06-14", modelResult("gpt-4o", 100, 0, 0))},
		HasMore:  true,
		NextPage: "PAGE2",
	}
	page2 := usageReport{
		Data: []usageBucket{dayBucket("2026-06-15", modelResult("gpt-4.1", 200, 0, 0))},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, usagePath):
			if r.URL.Query().Get("page") == "PAGE2" {
				_, _ = w.Write(mustJSON(t, page2))
			} else {
				_, _ = w.Write(mustJSON(t, page1))
			}
		default:
			_, _ = w.Write(mustJSON(t, costReport{}))
		}
	}))
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 2 {
		t.Errorf("got %d events, want 2 (both pages consumed)", len(ing.events))
	}
}

func TestPoll_UnixSecondBucketsWindowCorrectly(t *testing.T) {
	// Assert start_time/end_time are sent as unix seconds (the OpenAI format),
	// not RFC3339 — a regression guard against copy-pasting the Anthropic query.
	var sawStart string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			sawStart = r.URL.Query().Get("start_time")
			_, _ = w.Write(mustJSON(t, usageReport{}))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	poller := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), nil)
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	// window start = first of previous month (2026-05-01) at unix seconds.
	want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix()
	if sawStart != strconvI(want) {
		t.Errorf("start_time query = %q, want unix seconds %d (2026-05-01)", sawStart, want)
	}
}

func TestPoll_429RespectsRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, costReport{}))
			return
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(mustJSON(t, usageReport{}))
	}))
	defer srv.Close()

	var sleeps []time.Duration
	poller := newTestPoller(newTestClient(t, srv, &sleeps), newFakeStore(), nil)
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("usage endpoint called %d times, want 2 (429 then success)", calls)
	}
	if len(sleeps) == 0 || sleeps[0] != 7*time.Second {
		t.Errorf("backoff sleeps = %v, want first = 7s (Retry-After honored)", sleeps)
	}
}

func TestPoll_5xxRetriesThenErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	poller := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), nil)
	err := poller.pollOnce(context.Background(), &recordingIngester{})
	if err == nil {
		t.Fatalf("expected an error after exhausting retries")
	}
	if want := int32(1 + defaultMaxRetries); atomic.LoadInt32(&calls) != want {
		t.Errorf("usage endpoint called %d times, want %d (initial + %d retries)", calls, want, defaultMaxRetries)
	}
}

func TestPoll_KeyNeverInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force a non-retryable 401 to produce an error string.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	logBuf := &bytes.Buffer{}
	poller := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), logBuf)
	err := poller.pollOnce(context.Background(), &recordingIngester{})
	if err == nil {
		t.Fatalf("expected an error on 401")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("api key leaked into error: %q", err.Error())
	}
	// Also assert it isn't in the logs (runPass logs the error).
	poller.runPass(context.Background(), &recordingIngester{})
	if strings.Contains(logBuf.String(), testAPIKey) {
		t.Errorf("api key leaked into logs: %q", logBuf.String())
	}
}

func TestCosts_PostsDeltaOnly(t *testing.T) {
	// Settled day cost buckets in 2026-06 summing to $100. amount.value is DOLLARS.
	cost := costReport{Data: []costBucket{costDay("2026-06-15", 60.0, 40.0)}}
	srv := costServer(t, cost)
	defer srv.Close()

	fs := newFakeStore()
	fs.net[netKey("2026-06", collector.SourceOpenAIUsage)] = 80_000_000 // $80 already recorded (this source)
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)

	// First poll: API total $100, recorded $80 → one +$20 delta row, tagged source.
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce #1: %v", err)
	}
	if len(fs.posted) != 1 {
		t.Fatalf("got %d org_actual_spend rows, want 1", len(fs.posted))
	}
	if fs.posted[0].ActualPaidMicro != 20_000_000 || fs.posted[0].Period != "2026-06" ||
		fs.posted[0].Org != "acme" || fs.posted[0].Source != collector.SourceOpenAIUsage {
		t.Errorf("delta row = %+v, want +$20 for acme/2026-06 source=openai-usage", fs.posted[0])
	}

	// Second poll: unchanged total → delta 0 → no new row.
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce #2: %v", err)
	}
	if len(fs.posted) != 1 {
		t.Errorf("got %d org_actual_spend rows after re-poll, want 1 (idempotent, no churn)", len(fs.posted))
	}
}

// TestCosts_SourceScopedDoesNotCannibalizeOtherProvider is the R1 regression
// (mirrors #138): a foreign-source row (an Anthropic-admin / manual entry) for
// the same (org, period) must NOT be netted against or overwritten. The poller
// reconciles its OpenAI total ($100) against ONLY its own source's net (here
// $0), posts +$100 under openai-usage, and leaves the pre-existing $40 row
// intact.
func TestCosts_SourceScopedDoesNotCannibalizeOtherProvider(t *testing.T) {
	cost := costReport{Data: []costBucket{costDay("2026-06-15", 100.0)}}
	srv := costServer(t, cost)
	defer srv.Close()

	fs := newFakeStore()
	// A pre-existing OTHER-provider (anthropic-admin) row for the same org/period.
	fs.net[netKey("2026-06", collector.SourceAnthropicAdmin)] = 40_000_000 // $40
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)

	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(fs.posted) != 1 {
		t.Fatalf("posted %d rows, want 1", len(fs.posted))
	}
	if fs.posted[0].Source != collector.SourceOpenAIUsage || fs.posted[0].ActualPaidMicro != 100_000_000 {
		t.Errorf("posted row = %+v, want +$100 under openai-usage", fs.posted[0])
	}
	// The anthropic-admin row is UNTOUCHED — no −$40 cannibalization delta.
	if got := fs.net[netKey("2026-06", collector.SourceAnthropicAdmin)]; got != 40_000_000 {
		t.Errorf("anthropic-admin net = %d, want 40000000 (poller must not touch another source)", got)
	}
	if got := fs.net[netKey("2026-06", collector.SourceOpenAIUsage)]; got != 100_000_000 {
		t.Errorf("openai-usage net = %d, want 100000000", got)
	}
}

func TestCosts_SkipsUnsettledDays(t *testing.T) {
	// Day 2026-06-19 ends 2026-06-20T00:00Z → 12h before now → NOT settled.
	cost := costReport{Data: []costBucket{costDay("2026-06-19", 50.0)}}
	srv := costServer(t, cost)
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(fs.posted) != 0 {
		t.Errorf("posted %d rows for an unsettled cost day, want 0", len(fs.posted))
	}
}

func TestPoll_UnpricedModelDroppedWithWarn(t *testing.T) {
	// A model absent from the price table (a newly-launched OpenAI model before
	// prices.yaml is updated). ProviderOf can't resolve it → dropped, WARN.
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("gpt-6-not-in-table", 1000, 0, 500)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	logBuf := &bytes.Buffer{}
	poller := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), logBuf)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 0 {
		t.Errorf("got %d events for an unpriced model, want 0 (cannot price its remainder)", len(ing.events))
	}
	if !strings.Contains(logBuf.String(), "missing from the price table") ||
		!strings.Contains(logBuf.String(), "gpt-6-not-in-table") {
		t.Errorf("expected a WARN naming the unpriced model; log = %q", logBuf.String())
	}
}

func TestPoll_PaginationCapFailsLoud(t *testing.T) {
	// A malfunctioning upstream that always sets has_more with a fresh next_page.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			atomic.AddInt32(&calls, 1)
			_, _ = w.Write(mustJSON(t, usageReport{
				Data:     []usageBucket{dayBucket("2026-06-15", modelResult("gpt-4o", 1, 0, 0))},
				HasMore:  true,
				NextPage: "never-ends",
			}))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	poller := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), nil)
	err := poller.pollOnce(context.Background(), &recordingIngester{})
	if err == nil {
		t.Fatalf("expected a pagination-cap error against a non-terminating upstream")
	}
	if !strings.Contains(err.Error(), "pagination exceeded") {
		t.Errorf("error = %q, want a 'pagination exceeded' message", err.Error())
	}
	if got := atomic.LoadInt32(&calls); got != maxPages {
		t.Errorf("made %d requests, want exactly maxPages=%d (loop must not run unbounded)", got, maxPages)
	}
}

// strconvI is a tiny int64→string helper for the query-param assertion.
func strconvI(n int64) string { return strconv.FormatInt(n, 10) }
