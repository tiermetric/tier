package anthropicadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

const testAdminKey = "sk-ant-admin-SECRETVALUE-do-not-log"

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

func dayBucket(day string, results ...usageResult) usageBucket {
	start, _ := time.Parse("2006-01-02", day)
	start = start.UTC()
	return usageBucket{StartingAt: start, EndingAt: start.Add(24 * time.Hour), Results: results}
}

func modelResult(model string, in, out, cr, w5, w1 int) usageResult {
	return usageResult{
		Model:                model,
		UncachedInputTokens:  in,
		OutputTokens:         out,
		CacheReadInputTokens: cr,
		CacheCreation:        cacheCreation{Ephemeral5mInputTokens: w5, Ephemeral1hInputTokens: w1},
	}
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
		APIKey:    testAdminKey,
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

// --- tests -------------------------------------------------------------------

func TestPoll_MapsBucketsToRemainderEvents(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15",
			modelResult("claude-sonnet-4", 1000, 500, 200, 100, 50),
			modelResult("claude-opus-4", 2000, 0, 0, 0, 0)),
		dayBucket("2026-06-16",
			modelResult("claude-sonnet-4", 300, 0, 0, 0, 0), // fully captured → no event
			modelResult("claude-opus-4", 100, 50, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testAdminKey {
			t.Errorf("missing/incorrect x-api-key header")
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("missing anthropic-version header")
		}
		switch {
		case strings.HasPrefix(r.URL.Path, usagePath):
			_, _ = w.Write(mustJSON(t, usage))
		case strings.HasPrefix(r.URL.Path, costPath):
			_, _ = w.Write(mustJSON(t, costReport{}))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	fs := newFakeStore()
	fs.captured["2026-06-15"] = map[string]store.CostUsage{
		"claude-sonnet-4": {Input: 400, Output: 100}, // partial capture
	}
	fs.captured["2026-06-16"] = map[string]store.CostUsage{
		"claude-sonnet-4": {Input: 300}, // full capture
		"claude-opus-4":   {Input: 40},  // partial
	}

	client := newTestClient(t, srv, nil)
	poller := newTestPoller(client, fs, nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	idx := indexEvents(ing.events)
	if len(ing.events) != 3 {
		t.Fatalf("got %d events, want 3 (0615 sonnet+opus, 0616 opus; 0616 sonnet fully captured)", len(ing.events))
	}

	// 0615 sonnet: remainder = admin − captured, per class.
	sonnet := idx[eventKey{"claude-sonnet-4", "2026-06-15"}]
	wantRem := store.CostUsage{Input: 600, Output: 400, CacheRead: 200, CacheWrite5m: 100, CacheWrite1h: 50}
	if sonnet.InputTok != wantRem.Input || sonnet.OutputTok != wantRem.Output ||
		sonnet.CacheRead != wantRem.CacheRead || sonnet.CacheWrite5m != wantRem.CacheWrite5m ||
		sonnet.CacheWrite1h != wantRem.CacheWrite1h {
		t.Errorf("0615 sonnet remainder = %+v, want %+v", sonnet, wantRem)
	}
	if sonnet.Developer != collector.UnattributedIssueID || sonnet.IssueID != collector.UnattributedIssueID {
		t.Errorf("0615 sonnet developer/issue = %q/%q, want unattributed", sonnet.Developer, sonnet.IssueID)
	}
	if sonnet.Fidelity != collector.FidelityDaily || sonnet.Source != collector.SourceAnthropicAdmin {
		t.Errorf("0615 sonnet fidelity/source = %q/%q", sonnet.Fidelity, sonnet.Source)
	}
	if wantCost := store.ComputeCost("claude-sonnet-4", wantRem); sonnet.CostMicro != wantCost {
		t.Errorf("0615 sonnet CostMicro = %d, want %d (RPT list price of the REMAINDER)", sonnet.CostMicro, wantCost)
	}
	if sonnet.Timestamp != time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) {
		t.Errorf("0615 sonnet Timestamp = %v, want bucket end 2026-06-16T00:00Z", sonnet.Timestamp)
	}

	// 0615 opus: no capture → full admin as remainder.
	opus := idx[eventKey{"claude-opus-4", "2026-06-15"}]
	if opus.InputTok != 2000 {
		t.Errorf("0615 opus InputTok = %d, want 2000 (no prior capture)", opus.InputTok)
	}

	// 0616 opus: partial capture 40 → remainder 60/50.
	opus16 := idx[eventKey{"claude-opus-4", "2026-06-16"}]
	if opus16.InputTok != 60 || opus16.OutputTok != 50 {
		t.Errorf("0616 opus remainder = %d/%d, want 60/50", opus16.InputTok, opus16.OutputTok)
	}

	// 0616 sonnet: fully captured → absent.
	if _, ok := idx[eventKey{"claude-sonnet-4", "2026-06-16"}]; ok {
		t.Errorf("0616 sonnet emitted; a fully-captured day must produce no remainder event (double-count guard)")
	}
}

func TestPoll_SettlementLagSkipsOpenDays(t *testing.T) {
	// Day 2026-06-19 ends 2026-06-20T00:00Z — only 12h before now → NOT settled.
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-19", modelResult("claude-sonnet-4", 5000, 5000, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, usage))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
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
		dayBucket("2026-06-15", modelResult("claude-sonnet-4", 100, 100, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, usage))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	fs := newFakeStore()
	// Capture EXCEEDS admin (attribution slop) → negative remainder → clamp, no event, WARN.
	fs.captured["2026-06-15"] = map[string]store.CostUsage{
		"claude-sonnet-4": {Input: 500, Output: 500},
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

func TestPoll_IdempotencyKeyStableAcrossPolls(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("claude-sonnet-4", 1000, 0, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, usage))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
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
	want := collector.IdempotencyKey(collector.SourceAnthropicAdmin, "day", "2026-06-15", "claude-sonnet-4")
	if k1 != want {
		t.Errorf("idempotency key = %q, want %q", k1, want)
	}
}

func TestPoll_Pagination(t *testing.T) {
	page1 := usageReport{
		Data:     []usageBucket{dayBucket("2026-06-14", modelResult("claude-sonnet-4", 100, 0, 0, 0, 0))},
		HasMore:  true,
		NextPage: "PAGE2",
	}
	page2 := usageReport{
		Data: []usageBucket{dayBucket("2026-06-15", modelResult("claude-opus-4", 200, 0, 0, 0, 0))},
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
	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, &sleeps), fs, nil)
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

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	err := poller.pollOnce(context.Background(), &recordingIngester{})
	if err == nil {
		t.Fatalf("expected an error after exhausting retries")
	}
	// 1 initial + defaultMaxRetries.
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
	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, logBuf)
	err := poller.pollOnce(context.Background(), &recordingIngester{})
	if err == nil {
		t.Fatalf("expected an error on 401")
	}
	if strings.Contains(err.Error(), testAdminKey) {
		t.Errorf("admin key leaked into error: %q", err.Error())
	}
	// Also assert it isn't in the logs (runPass logs the error).
	poller.runPass(context.Background(), &recordingIngester{})
	if strings.Contains(logBuf.String(), testAdminKey) {
		t.Errorf("admin key leaked into logs: %q", logBuf.String())
	}
}

func TestCostReport_PostsDeltaOnly(t *testing.T) {
	// Settled day cost buckets in 2026-06 summing to $100 = 10000 cents.
	// amount is in CENTS as a decimal string.
	cost := costReport{Data: []costBucket{
		{
			StartingAt: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			EndingAt:   time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
			Results: []costResult{
				{Amount: "6000.0", Currency: "USD"}, // $60
				{Amount: "4000", Currency: "USD"},   // $40
			},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, costPath) {
			_, _ = w.Write(mustJSON(t, cost))
			return
		}
		_, _ = w.Write(mustJSON(t, usageReport{}))
	}))
	defer srv.Close()

	fs := newFakeStore()
	fs.net[netKey("2026-06", collector.SourceAnthropicAdmin)] = 80_000_000 // $80 already recorded (this source)
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)

	// First poll: API total $100, recorded $80 → one +$20 delta row, tagged with the
	// poller's source.
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce #1: %v", err)
	}
	if len(fs.posted) != 1 {
		t.Fatalf("got %d org_actual_spend rows, want 1", len(fs.posted))
	}
	if fs.posted[0].ActualPaidMicro != 20_000_000 || fs.posted[0].Period != "2026-06" ||
		fs.posted[0].Org != "acme" || fs.posted[0].Source != collector.SourceAnthropicAdmin {
		t.Errorf("delta row = %+v, want +$20 for acme/2026-06 source=anthropic-admin", fs.posted[0])
	}

	// Second poll: unchanged total → delta 0 → no new row.
	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce #2: %v", err)
	}
	if len(fs.posted) != 1 {
		t.Errorf("got %d org_actual_spend rows after re-poll, want 1 (idempotent, no churn)", len(fs.posted))
	}
}

func TestPoll_UnpricedModelDroppedWithWarn(t *testing.T) {
	// A model absent from the price table (a newly-launched Claude before
	// prices.yaml is updated). ProviderOf can't resolve it → dropped, WARN.
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15", modelResult("claude-newmodel-not-in-table", 1000, 500, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, usagePath) {
			_, _ = w.Write(mustJSON(t, usage))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	logBuf := &bytes.Buffer{}
	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, logBuf)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(ing.events) != 0 {
		t.Errorf("got %d events for an unpriced model, want 0 (cannot price its remainder)", len(ing.events))
	}
	if !strings.Contains(logBuf.String(), "missing from the price table") ||
		!strings.Contains(logBuf.String(), "claude-newmodel-not-in-table") {
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
				Data:     []usageBucket{dayBucket("2026-06-15", modelResult("claude-sonnet-4", 1, 0, 0, 0, 0))},
				HasMore:  true,
				NextPage: "never-ends",
			}))
			return
		}
		_, _ = w.Write(mustJSON(t, costReport{}))
	}))
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
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

// TestCostReport_SourceScopedDoesNotCannibalizeOtherProvider is the R1 regression
// (#138 review): a foreign-source row (a manual / other-provider entry) for the same
// (org, period) must NOT be netted against or overwritten. The poller reconciles its
// Anthropic total ($100) against ONLY its own source's net (here $0), posts +$100
// under anthropic-admin, and leaves the pre-existing $40 manual row intact.
func TestCostReport_SourceScopedDoesNotCannibalizeOtherProvider(t *testing.T) {
	cost := costReport{Data: []costBucket{
		{
			StartingAt: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			EndingAt:   time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
			Results:    []costResult{{Amount: "10000", Currency: "USD"}}, // $100
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, costPath) {
			_, _ = w.Write(mustJSON(t, cost))
			return
		}
		_, _ = w.Write(mustJSON(t, usageReport{}))
	}))
	defer srv.Close()

	fs := newFakeStore()
	// A pre-existing OTHER-provider (manual) row for the same org/period.
	fs.net[netKey("2026-06", store.OrgSpendSourceManual)] = 40_000_000 // $40
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)

	if err := poller.pollOnce(context.Background(), &recordingIngester{}); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	// Exactly one row posted, tagged anthropic-admin, for the full $100 (net was 0
	// for THIS source — the $40 manual row is invisible to the reconciliation).
	if len(fs.posted) != 1 {
		t.Fatalf("posted %d rows, want 1", len(fs.posted))
	}
	if fs.posted[0].Source != collector.SourceAnthropicAdmin || fs.posted[0].ActualPaidMicro != 100_000_000 {
		t.Errorf("posted row = %+v, want +$100 under anthropic-admin", fs.posted[0])
	}
	// The manual row is UNTOUCHED — no −$40 cannibalization delta was posted.
	if got := fs.net[netKey("2026-06", store.OrgSpendSourceManual)]; got != 40_000_000 {
		t.Errorf("manual-source net = %d, want 40000000 (poller must not touch another source)", got)
	}
	if got := fs.net[netKey("2026-06", collector.SourceAnthropicAdmin)]; got != 100_000_000 {
		t.Errorf("anthropic-admin net = %d, want 100000000", got)
	}
}

func TestCostReport_NonFiniteAmountFailsLoud(t *testing.T) {
	cost := costReport{Data: []costBucket{
		{
			StartingAt: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			EndingAt:   time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
			Results:    []costResult{{Amount: "Inf", Currency: "USD"}},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, costPath) {
			_, _ = w.Write(mustJSON(t, cost))
			return
		}
		_, _ = w.Write(mustJSON(t, usageReport{}))
	}))
	defer srv.Close()

	fs := newFakeStore()
	poller := newTestPoller(newTestClient(t, srv, nil), fs, nil)
	err := poller.pollOnce(context.Background(), &recordingIngester{})
	if err == nil {
		t.Fatalf("expected a non-finite-amount error")
	}
	if !strings.Contains(err.Error(), "non-finite amount") {
		t.Errorf("error = %q, want 'non-finite amount'", err.Error())
	}
	if len(fs.posted) != 0 {
		t.Errorf("posted %d org_actual_spend rows on non-finite amount, want 0 (must not write garbage)", len(fs.posted))
	}
}

func TestCostReport_SkipsUnsettledDays(t *testing.T) {
	// Day 2026-06-19 ends 2026-06-20T00:00Z → 12h before now → NOT settled.
	cost := costReport{Data: []costBucket{
		{
			StartingAt: time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC),
			EndingAt:   time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
			Results:    []costResult{{Amount: "5000", Currency: "USD"}},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, costPath) {
			_, _ = w.Write(mustJSON(t, cost))
			return
		}
		_, _ = w.Write(mustJSON(t, usageReport{}))
	}))
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
