// Package anthropicadmin implements the Anthropic Admin API usage/cost poller
// (#138): an org-level REST poll that captures the REMAINDER of spend not already
// seen by the per-request paths (JSONL watcher, reverse proxy), plus an
// invoice-grade actual-cost feed for Spend Leverage reconciliation.
//
// It is the first Run+Ingester collector that talks to an external provider over
// HTTP carrying an org Admin key, so it is deliberately conservative: a dedicated
// HTTP client with a hard timeout, bounded retries with capped backoff, no
// proxying through tierd's own reverse proxy, and a strict rule that the Admin key
// never enters a log line or an error string.
//
// Double-count safety is the load-bearing property (see poller.go): the Admin API
// reports ALL org usage including calls the proxy/JSONL already captured, so the
// poller ingests only settled-day REMAINDERS (admin − captured, clamped ≥ 0) under
// the "unattributed" sentinel, and reconciles the provider's own cost figure into
// org_actual_spend via idempotent deltas — never re-counting captured spend.
package anthropicadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// defaultBaseURL is the Anthropic API origin. Overridable for tests
	// (httptest.Server) via ClientConfig.BaseURL.
	defaultBaseURL = "https://api.anthropic.com"

	// anthropicVersion is the required API version header. Pinned to the value the
	// #138 spec specifies; bump only when a response-shape change is verified.
	anthropicVersion = "2023-06-01"

	// usagePath / costPath are the two Admin API endpoints (verified against the
	// live docs at build time — see poller.go's struct doc for the field mapping).
	usagePath = "/v1/organizations/usage_report/messages"
	costPath  = "/v1/organizations/cost_report"

	// bucketWidthDaily is the only bucket width TIER ingests: daily aggregates are
	// what settle against the per-request capture (Fidelity "daily").
	bucketWidthDaily = "1d"

	// maxBucketsPerPage is the Admin API's documented max time buckets per response
	// for bucket_width=1d (default 7, max 31). Requesting the max minimizes the
	// number of paginated round-trips for a multi-week window.
	maxBucketsPerPage = 31

	// defaultHTTPTimeout bounds a single HTTP request+body read. 60s per the spec:
	// generous for a large paginated aggregate response, finite so a hung provider
	// connection cannot wedge the poll goroutine forever.
	defaultHTTPTimeout = 60 * time.Second

	// defaultMaxRetries is the number of RETRIES (in addition to the first attempt)
	// on a retryable status (5xx / 429) or a transient transport error.
	defaultMaxRetries = 3

	// defaultRetryBase is the base of the capped exponential backoff between retries
	// (base·2^attempt, capped at maxRetryBackoff). Tests inject a no-op Sleep so no
	// real time passes.
	defaultRetryBase = 500 * time.Millisecond

	// maxRetryBackoff caps a single backoff sleep so a long Retry-After or a high
	// attempt count cannot stall the poller past a sane bound.
	maxRetryBackoff = 30 * time.Second

	// maxResponseBytes caps the decoded response body (defense against a hostile or
	// malfunctioning upstream returning an unbounded stream despite the timeout).
	// 32 MiB is far above any realistic 31-bucket paginated aggregate.
	maxResponseBytes = 32 << 20

	// maxPages bounds the pagination follow loop. maxResponseBytes/defaultHTTPTimeout
	// bound a SINGLE response; without a page cap an upstream that returns has_more
	// forever (small bodies each) would spin the poll goroutine and grow the
	// accumulator without bound (OOM). 64 pages × 31 daily buckets = ~5.4 years of
	// days — unreachable for any real window, so hitting it means a malfunctioning
	// upstream and we fail loud rather than loop.
	maxPages = 64
)

// Client is a thin, retrying HTTP client for the two Admin API report endpoints.
//
// Concurrency: a Client is safe for use by one poll loop at a time; it holds no
// mutable per-request state beyond the shared *http.Client (itself concurrency
// safe). It is not designed for concurrent Poll callers and none exist.
type Client struct {
	apiKey     string // Admin key (sk-ant-admin…); NEVER logged or wrapped into errors
	baseURL    string
	httpClient *http.Client
	maxRetries int
	retryBase  time.Duration
	// sleep waits for d or until ctx cancels, returning ctx.Err() on cancel.
	// Injectable so tests exercise the retry path with zero real delay.
	sleep  func(ctx context.Context, d time.Duration) error
	logger *slog.Logger
}

// ClientConfig configures a Client. Only APIKey is required; the rest default to
// production-safe values. BaseURL/HTTPClient/Sleep exist so tests can point at an
// httptest.Server and drive the retry path deterministically.
type ClientConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	MaxRetries int
	RetryBase  time.Duration
	Sleep      func(ctx context.Context, d time.Duration) error
	Logger     *slog.Logger
}

// NewClient builds a Client, filling in defaults. It does not validate APIKey
// (an empty key is the caller's "poller disabled" signal, handled upstream in
// cmd/tierd); a Client built with an empty key would fail every request with 401,
// which is the correct fail-closed behavior if one is ever constructed by mistake.
func NewClient(cfg ClientConfig) *Client {
	c := &Client{
		apiKey:     cfg.APIKey,
		baseURL:    cfg.BaseURL,
		httpClient: cfg.HTTPClient,
		maxRetries: cfg.MaxRetries,
		retryBase:  cfg.RetryBase,
		sleep:      cfg.Sleep,
		logger:     cfg.Logger,
	}
	if c.baseURL == "" {
		c.baseURL = defaultBaseURL
	}
	if c.httpClient == nil {
		// Dedicated client, hard timeout, default TLS. NOT tierd's reverse proxy.
		// CheckRedirect: see noRedirect — this Client NEVER follows a redirect.
		c.httpClient = &http.Client{Timeout: defaultHTTPTimeout, CheckRedirect: noRedirect}
	}
	if c.maxRetries == 0 {
		c.maxRetries = defaultMaxRetries
	}
	if c.retryBase == 0 {
		c.retryBase = defaultRetryBase
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// noRedirect makes an http.Client stop at the FIRST response and hand the 3xx
// back to the caller verbatim, instead of net/http's default "follow up to 10
// redirects" behavior. Both properties it buys are load-bearing here, and both
// were MEASURED to be broken without it (#452 review Y1):
//
//  1. SECRET CONTAINMENT. net/http strips only a fixed sensitive-header set
//     (Authorization, WWW-Authenticate, Cookie, Cookie2) when a redirect leaves
//     the original domain — see shouldCopyHeaderOnRedirect. `x-api-key` is NOT
//     on that list, so an upstream (or anything able to answer as one) that
//     replies 302 to another host receives the Admin key VERBATIM. Measured
//     against a cross-hostname httptest pair: the target saw the full key.
//     OpenAI's Bearer token happens to be stripped by that same list, which is
//     precisely why this must be fixed at the client and not left to luck —
//     the two providers were silently asymmetric while sharing one doc comment.
//
//  2. HONEST GRADING. ProbeResult.StatusCode is the status Authorized()
//     classifies. Following a redirect makes that the REDIRECT TARGET's status,
//     so a probe whose real origin answered 302 was reported to the operator as
//     `credential accepted (HTTP 200)` for a credential the provider never
//     validated — the exact misdiagnosis #452 exists to eliminate.
//
// SCOPE: deliberately applied to the shared client, so doGet/fetchCost stop
// following redirects too, not just Probe. Scoping it to the probe would leave
// the key-leak vector (1) open on the path that actually carries the key on
// every poll, forever, rather than once at doctor time — the strictly worse
// half to leave unfixed. Neither Admin API endpoint redirects, so nothing legitimate
// is lost: doGet already treats a non-2xx, non-429/5xx status as a
// fail-loud non-retryable error, which is the correct handling of an
// unexpected 3xx from an API that should never emit one.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// sleepCtx waits for d or until ctx cancels.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- Response shapes (verified against the live Admin API docs) ---------------
//
// Only the fields TIER consumes are tagged; json.Decoder ignores unknown fields
// (forward-compatible). Missing required fields are caught by the poller
// (empty model on a model-grouped usage result, empty cost amount) — see poller.go.

// usageReport is the Messages Usage Report envelope.
type usageReport struct {
	Data     []usageBucket `json:"data"`
	HasMore  bool          `json:"has_more"`
	NextPage string        `json:"next_page"`
}

// usageBucket is one time bucket. StartingAt is the bucket start (inclusive,
// UTC-snapped day boundary for bucket_width=1d); EndingAt is exclusive.
type usageBucket struct {
	StartingAt time.Time     `json:"starting_at"`
	EndingAt   time.Time     `json:"ending_at"`
	Results    []usageResult `json:"results"`
}

// usageResult is one group_by row. Token field names are verbatim from the API:
// uncached_input_tokens (→ InputTok), output_tokens (→ OutputTok),
// cache_read_input_tokens (→ CacheRead), and the nested cache_creation object
// carrying the 5m/1h TTL split (→ CacheWrite5m/CacheWrite1h). The API DOES split
// TTLs, so both write buckets map 1:1 (no legacy-bucket-into-5m fallback needed).
type usageResult struct {
	Model                string        `json:"model"`
	UncachedInputTokens  int           `json:"uncached_input_tokens"`
	OutputTokens         int           `json:"output_tokens"`
	CacheReadInputTokens int           `json:"cache_read_input_tokens"`
	CacheCreation        cacheCreation `json:"cache_creation"`
}

// cacheCreation is the nested TTL-split cache-creation object.
type cacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

// costReport is the Cost Report envelope.
type costReport struct {
	Data     []costBucket `json:"data"`
	HasMore  bool         `json:"has_more"`
	NextPage string       `json:"next_page"`
}

// costBucket is one daily cost bucket.
type costBucket struct {
	StartingAt time.Time    `json:"starting_at"`
	EndingAt   time.Time    `json:"ending_at"`
	Results    []costResult `json:"results"`
}

// costResult is one cost line item.
//
// IMPORTANT UNITS: `amount` is a decimal string in the currency's LOWEST unit
// (cents for USD) — e.g. "123.45" USD means $1.2345, NOT $123.45. The #138 spec
// loosely called this "USD"; the live docs are explicit that it is cents. The
// poller converts cents→micro-dollars precisely (see reconcileCost). Currency is
// currently always "USD".
type costResult struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// fetchUsage returns every usage bucket in [start, end), following pagination to
// exhaustion. It requests daily buckets grouped by model.
func (c *Client) fetchUsage(ctx context.Context, start, end time.Time) ([]usageBucket, error) {
	var out []usageBucket
	page := ""
	for p := 0; p < maxPages; p++ {
		q := c.baseQuery(start, end)
		q.Add("group_by[]", "model")
		if page != "" {
			q.Set("page", page)
		}
		var rep usageReport
		if err := c.doGet(ctx, usagePath, q, &rep); err != nil {
			return nil, err
		}
		out = append(out, rep.Data...)
		if !rep.HasMore || rep.NextPage == "" {
			return out, nil
		}
		page = rep.NextPage
	}
	// Reached the page cap with has_more still set: a malfunctioning upstream.
	return nil, fmt.Errorf("usage report: pagination exceeded %d pages (upstream returned has_more without terminating)", maxPages)
}

// fetchCost returns every cost bucket in [start, end), following pagination to
// exhaustion. No group_by: TIER needs only the org-level total per settled day,
// summed by month in reconcileCost.
func (c *Client) fetchCost(ctx context.Context, start, end time.Time) ([]costBucket, error) {
	var out []costBucket
	page := ""
	for p := 0; p < maxPages; p++ {
		q := c.baseQuery(start, end)
		if page != "" {
			q.Set("page", page)
		}
		var rep costReport
		if err := c.doGet(ctx, costPath, q, &rep); err != nil {
			return nil, err
		}
		out = append(out, rep.Data...)
		if !rep.HasMore || rep.NextPage == "" {
			return out, nil
		}
		page = rep.NextPage
	}
	return nil, fmt.Errorf("cost report: pagination exceeded %d pages (upstream returned has_more without terminating)", maxPages)
}

// ProbeResult is the outcome of one live reachability+authorization check
// against the Admin API, used by `tierd doctor` (#452) to catch a
// misconfigured or expired Admin key at setup time, rather than an adopter
// discovering it later as a silently-empty Spend Leverage tile.
type ProbeResult struct {
	// StatusCode is the HTTP status of the single probe request — always the
	// ORIGIN's status, never a redirect target's, because the Client never
	// follows redirects (see noRedirect). Zero means the request never got a
	// response — see Err.
	StatusCode int
	// Err is set only for a transport-level failure (DNS, connection refused,
	// TLS, timeout, context deadline). A completed non-2xx HTTP response is
	// NOT an Err — it is a normal, fully-formed StatusCode result.
	Err error
}

// Authorized reports whether the endpoint was reached and accepted the key
// (a 2xx response, no transport error).
func (r ProbeResult) Authorized() bool {
	return r.Err == nil && r.StatusCode >= 200 && r.StatusCode < 300
}

// Unauthorized reports whether the endpoint was reached but rejected the
// key (401/403) — the specific case doctor exists to catch, distinct from
// "endpoint unreachable" (r.Err != nil).
func (r ProbeResult) Unauthorized() bool {
	return r.Err == nil && (r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden)
}

// Probe makes exactly ONE authenticated GET against the cost-report endpoint
// over a minimal (trailing 24h) window, with NO retries — a doctor check
// should fail fast and cheap, not spend a real poll's retry/backoff budget —
// and classifies the raw result without decoding, ingesting, or mutating
// anything. Bypasses doGet/fetchCost deliberately: doctor needs the raw
// status code to distinguish "unauthorized" from "unreachable" from
// "unexpected", and doGet's non-retryable-status error collapses all of
// those into one opaque string.
//
// Error hygiene matches doGet: the Admin key travels only in the x-api-key
// header and never enters the returned ProbeResult.Err.
func (c *Client) Probe(ctx context.Context) ProbeResult {
	// UTC-midnight aligned, matching the shape every real poll sends
	// (windowStart in poller.go): a real poll never requests an arbitrary
	// time-of-day boundary for bucket_width=1d, so neither should the probe —
	// an unaligned starting_at risks a 400 from API-side bucket validation on
	// a perfectly valid key, which would misclassify as "unexpected status"
	// (WARN) instead of the clean OK a valid key should get.
	end := time.Now().UTC().Truncate(24 * time.Hour)
	q := c.baseQuery(end.AddDate(0, 0, -1), end)
	fullURL := c.baseURL + costPath + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return ProbeResult{Err: fmt.Errorf("build probe request: %w", err)}
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ProbeResult{Err: fmt.Errorf("probe GET %s: %w", costPath, err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()
	return ProbeResult{StatusCode: resp.StatusCode}
}

// baseQuery builds the shared query params (starting_at/ending_at RFC3339 UTC,
// bucket_width=1d, limit).
func (c *Client) baseQuery(start, end time.Time) url.Values {
	q := url.Values{}
	q.Set("starting_at", start.UTC().Format(time.RFC3339))
	q.Set("ending_at", end.UTC().Format(time.RFC3339))
	q.Set("bucket_width", bucketWidthDaily)
	q.Set("limit", strconv.Itoa(maxBucketsPerPage))
	return q
}

// doGet performs one authenticated GET with bounded retries and decodes the JSON
// body into out. Retries fire on transient transport errors and on 429/5xx, with
// capped exponential backoff; a 429 Retry-After (seconds) raises the wait for that
// attempt. Non-retryable statuses (other 4xx) fail immediately.
//
// Error hygiene: no error returned here contains the Admin key. The key travels
// only in the x-api-key request header, never in the URL/query, and is never
// formatted into a message — a security invariant covered by TestPoll_KeyNeverInErrors.
func (c *Client) doGet(ctx context.Context, path string, q url.Values, out any) error {
	fullURL := c.baseURL + path + "?" + q.Encode()
	var lastErr error
	var lastRetryAfter time.Duration // Retry-After from the most recent 429, if any
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		// Backoff BEFORE every attempt after the first. The wait is the plain capped
		// exponential backoff, raised to a 429 Retry-After when the previous failure
		// carried one, so the sleep happens in exactly one place.
		if attempt > 0 {
			if err := c.sleep(ctx, c.nextWait(attempt, lastRetryAfter)); err != nil {
				return err // ctx cancelled during backoff
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		// Admin key: header only. sk-ant-admin… keys authenticate via x-api-key.
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)
		req.Header.Set("accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Transport error (DNS, connection refused, TLS, timeout). Retry — the
			// error carries the URL (no secret) but never the header.
			lastErr = fmt.Errorf("http GET %s: %w", path, err)
			lastRetryAfter = 0
			continue
		}

		if resp.StatusCode == http.StatusOK {
			err := decodeJSON(resp.Body, out)
			_ = resp.Body.Close()
			if err != nil {
				return fmt.Errorf("decode %s response: %w", path, err)
			}
			return nil
		}

		// Drain+close so the connection can be reused across retries.
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		status := resp.StatusCode
		_ = resp.Body.Close()

		if status == http.StatusTooManyRequests || status >= 500 {
			lastErr = fmt.Errorf("http GET %s: status %d", path, status)
			lastRetryAfter = retryAfter // honored by the backoff on the next loop
			continue
		}
		// Non-retryable (401/403/400/404/…). Fail loud, no key in the message.
		return fmt.Errorf("http GET %s: non-retryable status %d", path, status)
	}
	return fmt.Errorf("http GET %s: exhausted %d retries: %w", path, c.maxRetries, lastErr)
}

// nextWait returns the backoff before the given attempt (1-based): the capped
// exponential base·2^(attempt-1), raised to retryAfter when a 429 supplied one.
func (c *Client) nextWait(attempt int, retryAfter time.Duration) time.Duration {
	d := c.retryBase << (attempt - 1)  // base·2^(attempt-1)
	if d > maxRetryBackoff || d <= 0 { // <=0 guards a shift overflow at high attempts
		d = maxRetryBackoff
	}
	return maxDuration(d, retryAfter)
}

// decodeJSON decodes a bounded response body into out. json.Decoder ignores
// unknown fields, keeping the parser forward-compatible with new API fields.
func decodeJSON(body io.Reader, out any) error {
	dec := json.NewDecoder(io.LimitReader(body, maxResponseBytes))
	return dec.Decode(out)
}

// parseRetryAfter parses a Retry-After header value expressed as delta-seconds
// (the form the Anthropic API uses on 429). An HTTP-date form or a malformed value
// yields 0 (fall back to plain backoff). Negative/absurd values are ignored.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	return d
}

// maxDuration returns the larger of a, b.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
