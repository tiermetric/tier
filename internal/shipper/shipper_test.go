package shipper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// testEvent returns a realistic collector event; key differentiates events.
func testEvent(key string) collector.TokenEvent {
	return collector.TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		OutputTok:      500,
		CostMicro:      10500, // $0.0105
		Source:         collector.SourceJSONL,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: key,
		Timestamp:      time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC),
	}
}

// recordingServer captures every batch POSTed to /api/v1/events and returns
// the status codes queued in statuses (repeating the last one when exhausted).
type recordingServer struct {
	mu       sync.Mutex
	batches  [][]map[string]any
	auths    []string
	statuses []int
}

func (rs *recordingServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/events" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		rs.mu.Lock()
		rs.batches = append(rs.batches, batch)
		rs.auths = append(rs.auths, r.Header.Get("Authorization"))
		status := http.StatusCreated
		if len(rs.statuses) > 0 {
			status = rs.statuses[0]
			if len(rs.statuses) > 1 {
				rs.statuses = rs.statuses[1:]
			}
		}
		rs.mu.Unlock()
		if status == http.StatusCreated {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(batch)})
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "injected failure"})
	}
}

func (rs *recordingServer) batchSizes() []int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	sizes := make([]int, len(rs.batches))
	for i, b := range rs.batches {
		sizes[i] = len(b)
	}
	return sizes
}

func newTestClient(t *testing.T, srv *httptest.Server, cfg Config) *Client {
	t.Helper()
	cfg.ServerURL = srv.URL
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = time.Nanosecond // no wall-clock sleeps in tests
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestClient_BatchesAndFlushes drives 5 events through a BatchSize=2 client:
// two full batches ship from Ingest, the final partial ships on Flush, and
// every request carries the bearer token.
func TestClient_BatchesAndFlushes(t *testing.T) {
	rs := &recordingServer{}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{APIToken: "tok-123", BatchSize: 2})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := c.Ingest(ctx, testEvent(fmt.Sprintf("k-%d", i))); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got, want := rs.batchSizes(), []int{2, 2, 1}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("batch sizes = %v, want %v", got, want)
	}
	if got := c.Shipped(); got != 5 {
		t.Errorf("Shipped() = %d, want 5", got)
	}
	for i, a := range rs.auths {
		if a != "Bearer tok-123" {
			t.Errorf("request %d Authorization = %q, want Bearer tok-123", i, a)
		}
	}
}

// TestClient_WireShape pins the JSON contract one event crosses the wire
// with — field names must match the server's eventRequest exactly, cost in
// dollars, timestamp RFC3339, provenance passed through unchanged.
func TestClient_WireShape(t *testing.T) {
	rs := &recordingServer{}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{})
	ctx := context.Background()
	if err := c.Ingest(ctx, testEvent("k-wire")); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if len(rs.batches) != 1 || len(rs.batches[0]) != 1 {
		t.Fatalf("batches = %v, want one batch of one event", rs.batchSizes())
	}
	got := rs.batches[0][0]
	want := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"input_tokens":    float64(1000),
		"output_tokens":   float64(500),
		"source":          "jsonl",
		"fidelity":        "realtime",
		"idempotency_key": "k-wire",
		"timestamp":       "2026-05-19T10:00:00Z",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("wire field %s = %v, want %v", k, got[k], w)
		}
	}
	// Money round-trip: $0.0105 in dollars on the wire converts back to
	// exactly the original 10500 micro-dollars server-side.
	cost, ok := got["cost_usd"].(float64)
	if !ok {
		t.Fatalf("cost_usd missing or not a number: %v", got["cost_usd"])
	}
	if micro := store.DollarsToMicro(cost); micro != 10500 {
		t.Errorf("cost round-trip = %d micro, want 10500", micro)
	}
}

// TestClient_ShipsRepo pins the #491 regression: the collector resolves a
// canonical repo slug, and it must survive the wire.
//
// It did not. wireEvent carried no `repo` field at all, so every shipped event
// landed under the `unqualified` sentinel server-side. Cost could then never
// join outcomes per-repository, and two repos' issues sharing a number re-fused
// — the exact failure the repo column was added to prevent (#231). Measured
// before the fix: on a real multi-repo installation every shipped event was
// unqualified,
// while `--repo-slug` was parsed, validated and silently discarded.
func TestClient_ShipsRepo(t *testing.T) {
	rs := &recordingServer{}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	ev := testEvent("k-repo")
	ev.Repo = "acme/widgets"

	c := newTestClient(t, srv, Config{})
	ctx := context.Background()
	if err := c.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if len(rs.batches) != 1 || len(rs.batches[0]) != 1 {
		t.Fatalf("batches = %v, want one batch of one event", rs.batchSizes())
	}
	if got := rs.batches[0][0]["repo"]; got != "acme/widgets" {
		t.Errorf("wire repo = %v, want acme/widgets", got)
	}
}

// TestClient_DropsUnshippableRepo guards the half of #491 that is easy to get
// wrong in the obvious way.
//
// The naive fix — `Repo: ev.Repo` — is WORSE than the bug. The collector yields
// repoid.Unqualified (never "") when it cannot name a repository, and
// api.validateRepo REJECTS that sentinel explicitly so a client cannot opt out
// of repo-scoping on purpose. Because the batch is all-or-nothing, sending it
// verbatim 400s every batch from any repo without a resolvable origin, turning a
// silent degradation into a total capture outage.
//
// NOTE what this does and does not pin. An explicit "" and an absent key are
// equivalent to the server, so omitempty is NOT the guard — wireRepo is. Only
// the sentinel and malformed cases below can actually reach the server and fail;
// they are the rows that matter. The cased/spaced variants are unreachable from
// today's producers by construction and are pinned as defense-in-depth, because
// the server matches case-insensitively after trimming while a naive guard would
// use ==.
func TestClient_DropsUnshippableRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo string
	}{
		{"sentinel", repoid.Unqualified},
		{"sentinel_cased", "Unqualified"},
		{"sentinel_spaced", " unqualified "},
		{"empty", ""},
		{"malformed", "not-a-slug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &recordingServer{}
			srv := httptest.NewServer(rs.handler(t))
			defer srv.Close()

			ev := testEvent("k-" + tc.name)
			ev.Repo = tc.repo

			c := newTestClient(t, srv, Config{})
			ctx := context.Background()
			if err := c.Ingest(ctx, ev); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if err := c.Flush(ctx); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			if len(rs.batches) != 1 || len(rs.batches[0]) != 1 {
				t.Fatalf("batches = %v, want one batch of one event", rs.batchSizes())
			}
			if v, present := rs.batches[0][0]["repo"]; present {
				t.Errorf("repo=%q reached the wire as %v; a value the server will not accept must be dropped by wireRepo, or one bad element 400s the entire batch", tc.repo, v)
			}
		})
	}
}

// TestWireEventMatchesServerContract is the drift gate for the bug CLASS, not
// the instance.
//
// wireEvent and the server's api.eventRequest are two hand-maintained mirrors of
// one wire contract, in different packages, with nothing tying them together.
// That is why #491 happened — and why #238 happened one field earlier, when
// session_id was omitted the same way. TestClient_WireShape did not catch either,
// because it asserts a hand-written want map and so can only detect fields that
// are WRONG, never fields that are MISSING.
//
// Both structs are unexported, so a reflective cross-package comparison is not
// possible. This instead pins wireEvent's JSON key set exactly: adding a field to
// eventRequest without adding it here leaves this list stale, and any change to
// wireEvent fails here until someone consciously updates the contract and checks
// the other side.
func TestWireEventMatchesServerContract(t *testing.T) {
	// Every key api.eventRequest accepts. Keep sorted; see events.go.
	want := []string{
		"cache_read_tokens",
		"cache_write_1h_tokens",
		"cache_write_5m_tokens",
		"cost_usd",
		"developer",
		"fidelity",
		"idempotency_key",
		"input_tokens",
		"issue_id",
		"model",
		"output_tokens",
		"repo",
		"session_id",
		"source",
		"timestamp",
	}

	typ := reflect.TypeOf(wireEvent{})
	var got []string
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			t.Errorf("field %s has no json tag; every wire field must name its key explicitly", typ.Field(i).Name)
			continue
		}
		got = append(got, name)
	}
	sort.Strings(got)

	if !slices.Equal(got, want) {
		t.Errorf("wireEvent JSON keys drifted from the server contract:\n got: %v\nwant: %v\nIf you added a field to api.eventRequest, add it here AND to wireEvent — a field present on only one side is silently dropped in flight (#491).", got, want)
	}
}

// TestClient_Retries5xxThenSucceeds: two injected 503s, third attempt lands.
func TestClient_Retries5xxThenSucceeds(t *testing.T) {
	rs := &recordingServer{statuses: []int{503, 503, 201}}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{BatchSize: 1})
	if err := c.Ingest(context.Background(), testEvent("k-retry")); err != nil {
		t.Fatalf("Ingest should succeed after retries: %v", err)
	}
	if got := len(rs.batchSizes()); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if got := c.Shipped(); got != 1 {
		t.Errorf("Shipped() = %d, want 1", got)
	}
}

// TestClient_ExhaustsRetriesOn5xx: persistent 500s exhaust the 3 attempts
// and surface the failure.
func TestClient_ExhaustsRetriesOn5xx(t *testing.T) {
	rs := &recordingServer{statuses: []int{500}}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{BatchSize: 1})
	err := c.Ingest(context.Background(), testEvent("k-fail"))
	if err == nil {
		t.Fatal("Ingest should fail after exhausting retries")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry the status code; got %v", err)
	}
	if got := len(rs.batchSizes()); got != 3 {
		t.Errorf("attempts = %d, want exactly 3", got)
	}
	if got := c.Shipped(); got != 0 {
		t.Errorf("Shipped() = %d, want 0", got)
	}
}

// TestClient_FailsLoudOn4xxWithoutRetry: a 400 is a client validation bug —
// exactly one attempt, error surfaces with the server's message, nothing is
// silently skipped.
func TestClient_FailsLoudOn4xxWithoutRetry(t *testing.T) {
	rs := &recordingServer{statuses: []int{400}}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{BatchSize: 1})
	err := c.Ingest(context.Background(), testEvent("k-400"))
	if err == nil {
		t.Fatal("Ingest should fail on 400")
	}
	if !strings.Contains(err.Error(), "injected failure") {
		t.Errorf("error should carry the server body; got %v", err)
	}
	if got := len(rs.batchSizes()); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retry on 4xx)", got)
	}
}

// TestClient_OversizedBufferDrainsInChunks covers the pathological caller
// that ignores an Ingest error and keeps buffering: once the server
// recovers, the flush drains the backlog in batchSize-bounded chunks instead
// of one oversized POST the server would 400.
func TestClient_OversizedBufferDrainsInChunks(t *testing.T) {
	// First three responses (one Ingest-triggered flush = 3 attempts) fail;
	// everything after succeeds.
	rs := &recordingServer{statuses: []int{500, 500, 500, 201}}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{BatchSize: 2})
	ctx := context.Background()
	if err := c.Ingest(ctx, testEvent("k-0")); err != nil {
		t.Fatalf("Ingest(0): %v", err)
	}
	// Boundary flush fails after exhausting retries — deliberately ignored.
	if err := c.Ingest(ctx, testEvent("k-1")); err == nil {
		t.Fatal("Ingest(1) should fail while the server is down")
	}
	// Buffer now holds 2 unsent events; this Ingest makes it 3 and flushes.
	if err := c.Ingest(ctx, testEvent("k-2")); err != nil {
		t.Fatalf("Ingest(2) after recovery: %v", err)
	}

	sizes := rs.batchSizes()
	if len(sizes) != 5 || sizes[3] != 2 || sizes[4] != 1 {
		t.Errorf("batch sizes = %v, want 3 failed attempts then chunks [2, 1]", sizes)
	}
	if got := c.Shipped(); got != 3 {
		t.Errorf("Shipped() = %d, want 3", got)
	}
}

// TestClient_FlushEmptyIsNoOp: nothing buffered, nothing posted (the server
// rejects empty arrays, so the client must never send one).
func TestClient_FlushEmptyIsNoOp(t *testing.T) {
	rs := &recordingServer{}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv, Config{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(rs.batchSizes()); got != 0 {
		t.Errorf("posted %d batches, want 0", got)
	}
}

// TestClient_ContextCancelAbortsRetry: with a cancelled context the retry
// path returns promptly with ctx.Err() instead of sleeping out the backoff.
func TestClient_ContextCancelAbortsRetry(t *testing.T) {
	rs := &recordingServer{statuses: []int{500}}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	// A long backoff would hang this test if cancellation were broken.
	c := newTestClient(t, srv, Config{BatchSize: 1, BackoffBase: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Ingest(ctx, testEvent("k-cancel"))
	if err == nil {
		t.Fatal("Ingest should fail with a cancelled context")
	}
}

// TestNew_Validation pins the fail-fast constructor checks.
func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty URL", Config{ServerURL: ""}},
		{"no scheme", Config{ServerURL: "tier.example"}},
		{"bad scheme", Config{ServerURL: "ftp://tier.example"}},
		{"batch too large", Config{ServerURL: "http://tier.example", BatchSize: 501}},
		{"negative batch", Config{ServerURL: "http://tier.example", BatchSize: -1}},
		{"negative attempts", Config{ServerURL: "http://tier.example", MaxAttempts: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("New(%+v) should fail", tc.cfg)
			}
		})
	}

	// Trailing slash tolerated; endpoint normalized. A loopback host keeps the
	// #182 plaintext guard from firing on this http URL.
	c, err := New(Config{ServerURL: "http://127.0.0.1:8080/"})
	if err != nil {
		t.Fatalf("New with trailing slash: %v", err)
	}
	if c.endpoint != "http://127.0.0.1:8080/api/v1/events" {
		t.Errorf("endpoint = %q, want normalized /api/v1/events path", c.endpoint)
	}
}

// TestNew_PlaintextGuard is the #182 fail-closed matrix: shipping a bearer
// token over cleartext http to a non-loopback host is refused at construction
// (the token AND every per-developer spend figure would otherwise cross the
// wire sniffable, and a captured token lets an attacker POST spend as any
// developer). Loopback http (127.0.0.1 / [::1] / localhost, any port), https
// anywhere, and tokenless http are all unaffected. This mirrors the server's
// validateBind #59 guard from the inverse direction: the server refuses to
// expose spend without a token; the client refuses to leak the token in
// cleartext.
func TestNew_PlaintextGuard(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		token     string
		wantError bool
	}{
		// Blocked: a token would cross the open network in cleartext.
		{"http non-loopback IP+port + token", "http://192.168.1.10:9000", "tok", true},
		{"http non-loopback IP no port + token", "http://10.0.0.5", "tok", true},
		{"http non-loopback hostname + token", "http://tier.example", "tok", true},
		{"http non-loopback hostname+port + token", "http://tier.example:8080", "tok", true},
		// Allowed: loopback http never leaves the host.
		{"http 127.0.0.1+port + token", "http://127.0.0.1:8080", "tok", false},
		{"http 127.0.0.1 no port + token", "http://127.0.0.1", "tok", false},
		{"http 127.x loopback range + token", "http://127.9.9.9:8080", "tok", false},
		{"http IPv6 loopback + token", "http://[::1]:8080", "tok", false},
		{"http IPv6 loopback no port + token", "http://[::1]", "tok", false},
		{"http localhost+port + token", "http://localhost:8080", "tok", false},
		{"http localhost no port + token", "http://localhost", "tok", false},
		// Allowed: https encrypts the token to any host.
		{"https non-loopback hostname + token", "https://tier.example", "tok", false},
		{"https non-loopback IP + token", "https://192.168.1.10:9000", "tok", false},
		// Allowed: no token means no secret to leak (spend still cleartext — WARNed).
		{"http non-loopback hostname no token", "http://tier.example", "", false},
		{"http non-loopback IP no token", "http://192.168.1.10:9000", "", false},
		{"http loopback no token", "http://127.0.0.1:8080", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{ServerURL: tc.url, APIToken: tc.token})
			if tc.wantError && err == nil {
				t.Errorf("New(%q, token set=%t) = nil error, want plaintext refusal", tc.url, tc.token != "")
			}
			if !tc.wantError && err != nil {
				t.Errorf("New(%q, token set=%t) = %v, want no error", tc.url, tc.token != "", err)
			}
		})
	}
}

// TestClient_DoesNotFollowRedirect pins the #182 redirect-downgrade guard: a
// 3xx from the server is terminal and never followed, so the bearer token is
// never re-sent to the redirect target (a same-host https->http downgrade would
// otherwise re-POST the token in cleartext, defeating the New() plaintext
// guard). The Location points at a second server that trips the test if it is
// ever reached.
func TestClient_DoesNotFollowRedirect(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/events", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	// BatchSize 1 → the single Ingest flushes immediately; MaxAttempts 1 → no
	// retry noise (a 3xx is terminal regardless).
	c := newTestClient(t, redirector, Config{APIToken: "tok-secret", BatchSize: 1, MaxAttempts: 1})
	if err := c.Ingest(context.Background(), testEvent("k-redir")); err == nil {
		t.Fatal("Ingest should fail on a 3xx redirect, not silently follow it")
	}
	if reached.Load() {
		t.Fatal("client followed the redirect and re-sent the bearer token to the redirect target")
	}
}

// TestNew_WarnsOnTokenlessCleartext: tokenless http to a non-loopback host is
// allowed (there is no bearer token to leak) but must emit a WARN — the spend
// batch itself still crosses the wire unencrypted, and the operator should be
// told rather than have it happen silently.
func TestNew_WarnsOnTokenlessCleartext(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	if _, err := New(Config{ServerURL: "http://tier.example", APIToken: ""}); err != nil {
		t.Fatalf("New tokenless http non-loopback: unexpected error %v", err)
	}
	if !strings.Contains(buf.String(), "cleartext") {
		t.Errorf("expected a cleartext WARN, got log: %q", buf.String())
	}
}
