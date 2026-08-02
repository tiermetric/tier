// Package shipper implements the laptop-side HTTP sink for `tierd ship`
// (#126, capture-topology Option A): a collector.Ingester that buffers token
// events into batches and POSTs them to a central tierd's
// POST /api/v1/events instead of a local SQLite store.
//
// The shipper is STATELESS BY DESIGN: it keeps no checkpoint of what has
// already been sent. Every event carries the collector's idempotency key, and
// the server's MAX-on-conflict UPSERT absorbs re-sends as no-ops — so
// over-shipping (re-scanning 90 days of JSONL on every cron tick) is free and
// replaces checkpoint bookkeeping entirely.
package shipper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

const (
	// DefaultBatchSize matches the server's per-request cap
	// (api.maxEventsPerBatch). The shipper flushes whenever the buffer
	// reaches this size; the final partial batch goes out via Flush.
	DefaultBatchSize = 500
	// DefaultMaxAttempts bounds retries per batch: the first attempt plus
	// two retries on 5xx / transport errors. 4xx never retries — a
	// validation failure is a client bug that must surface, not be
	// retried into oblivion or silently skipped.
	DefaultMaxAttempts = 3
	// DefaultBackoffBase is the first retry delay; each subsequent retry
	// doubles it, capped at maxBackoff. Tests inject a near-zero base.
	DefaultBackoffBase = 1 * time.Second
	// maxBackoff caps the exponential growth so a long outage does not
	// stretch a single batch's retry sleep unboundedly.
	maxBackoff = 30 * time.Second
	// maxErrorBodyBytes bounds how much of an error response body is read
	// into the returned error message — enough for the server's JSON
	// error, never an unbounded slurp of a misbehaving endpoint.
	maxErrorBodyBytes = 8 << 10
	// defaultHTTPTimeout bounds each POST when the caller does not supply
	// an http.Client. A batch is at most ~1 MiB, so 30s is generous.
	defaultHTTPTimeout = 30 * time.Second
)

// Config configures a Client. ServerURL and (usually) APIToken come from the
// tierd ship flags; the remaining fields exist for tests and default sanely
// when zero.
type Config struct {
	// ServerURL is the central tierd base URL, e.g. "https://tier.example".
	// Required; must be http or https. A trailing slash is tolerated. New
	// refuses http to a non-loopback host when APIToken is set (#182) — the
	// token would otherwise cross the wire in cleartext.
	ServerURL string
	// APIToken is sent as `Authorization: Bearer <token>`. Empty sends no
	// header — valid only against a loopback tierd running tokenless. A
	// non-empty token requires https unless the server is loopback (see New).
	APIToken string
	// HTTPClient overrides the default 30s-timeout client (tests point it
	// at an httptest.Server; production leaves it nil).
	HTTPClient *http.Client
	// BatchSize overrides DefaultBatchSize (tests use small batches to
	// exercise the flush boundary cheaply). Values above the server cap
	// would be rejected server-side, so New refuses them.
	BatchSize int
	// MaxAttempts overrides DefaultMaxAttempts.
	MaxAttempts int
	// BackoffBase overrides DefaultBackoffBase (tests pass 1ns so retry
	// paths run without wall-clock sleeps).
	BackoffBase time.Duration
}

// Client ships collector token events to a central tierd. It implements
// collector.Ingester, so it plugs directly into JSONLCollector.Run as the
// HTTP-backed alternative to the local ingester.Store adapter.
//
// Safe for concurrent Ingest callers (the Ingester contract): one mutex
// serializes buffer access and the flush it may trigger. For the CLI shipper
// the collector calls sequentially, so the lock is uncontended.
type Client struct {
	endpoint    string // ServerURL + "/api/v1/events"
	apiToken    string
	hc          *http.Client
	batchSize   int
	maxAttempts int
	backoffBase time.Duration

	mu      sync.Mutex
	buf     []wireEvent
	shipped int
}

// wireEvent is the JSON shape of one POST /api/v1/events element — the
// client-side mirror of the server's eventRequest. Cost crosses the wire in
// dollars (the endpoint's contract); the round-trip through
// store.MicroToDollars / DollarsToMicro is exact for any realistic magnitude
// (micro values are integers well below 2^53).
type wireEvent struct {
	Developer      string  `json:"developer"`
	IssueID        string  `json:"issue_id"`
	Model          string  `json:"model"`
	InputTok       int     `json:"input_tokens"`
	OutputTok      int     `json:"output_tokens"`
	CacheRead      int     `json:"cache_read_tokens"`
	CacheWrite5m   int     `json:"cache_write_5m_tokens"`
	CacheWrite1h   int     `json:"cache_write_1h_tokens"`
	CostUSD        float64 `json:"cost_usd"`
	Source         string  `json:"source"`
	Fidelity       string  `json:"fidelity"`
	IdempotencyKey string  `json:"idempotency_key"`
	// SessionID is the opaque Claude Code session UUID (#238), mirroring the
	// server's eventRequest.SessionID. Empty for a session-blind producer; the
	// server stores "" as SQL NULL. Without this field a remote laptop shipper
	// would silently drop session_id to NULL on every re-posted event.
	SessionID string `json:"session_id"`
	// Repo is the canonical "owner/repo" this cost was spent in (#231),
	// mirroring the server's eventRequest.Repo. Without this field the shipper
	// silently stored every remote event as the `unqualified` sentinel, so a
	// multi-repo org's cost could never join its outcomes and two repos' issues
	// sharing a number re-fused — exactly what the repo column exists to
	// prevent (#491).
	//
	// omitempty here merely matches the endpoint's documented omit-the-field
	// contract for an unknown repository; it is NOT what makes this safe.
	// api.validateRepo treats an explicit "" and an absent key identically, so
	// dropping omitempty changes nothing observable. wireRepo below is the
	// load-bearing part — see its comment.
	Repo      string `json:"repo,omitempty"`
	Timestamp string `json:"timestamp"`
}

// wireRepo renders a collector-resolved repo for the wire, and is LOAD-BEARING:
// deleting it turns a silent degradation into a total capture outage.
//
// api.validateRepo REJECTS an explicitly-supplied sentinel, so that a client
// cannot opt out of repo-scoping on purpose and re-fuse two repos' issues. The
// events batch is all-or-nothing, so ONE such element 400s the whole batch. The
// collector never yields "" — it yields a canonical slug or repoid.Unqualified
// (see JSONLCollector.repo) — so without this mapping every batch shipped from a
// repository with no resolvable origin would fail outright (#491).
//
// Canonical-or-drop rather than an equality test against the sentinel: the
// server compares case-insensitively and after trimming, so an exact `==` guard
// is strictly NARROWER than the rule it exists to satisfy, and "Unqualified" or
// " unqualified" would sail through and 400 the batch. repoid.Canonical rejects
// the sentinel in every cased/trimmed form. The deliberate trade: a malformed
// slug is omitted rather than rejected, which loses repo scoping for that row
// but can never take down capture — the right side to err on for a field whose
// absence is already a supported state.
func wireRepo(repo string) string {
	slug, ok := repoid.Canonical(repo)
	if !ok {
		return ""
	}
	return slug
}

// New validates cfg and returns a ready Client. Fail-fast on a bad server
// URL or an out-of-range batch size — a misconfigured shipper must die at
// startup, not after collecting 90 days of events.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.ServerURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid server URL %q: need http(s)://host[:port]", cfg.ServerURL)
	}
	// Fail closed on cleartext to a non-loopback server. Over http to a routable
	// host the org bearer token AND every per-developer spend figure would cross
	// the wire unencrypted: sniffable on any shared segment, and a captured token
	// lets an attacker POST realtime-fidelity spend as any developer via
	// /api/v1/events. This is the shipper-side mirror of cmd/tierd validateBind's
	// #59 guard — the server refuses to expose spend without a token; the client
	// refuses to leak the token in cleartext — and, like it, dies at construction
	// rather than after collecting events. There is no override flag by design:
	// https to the same host, or a loopback target (SSH tunnel / port-map), is
	// always available and strictly safer, so an escape hatch would only invite
	// the cleartext-token deployment this guard exists to prevent.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		if cfg.APIToken != "" {
			return nil, fmt.Errorf("refusing to ship to %q: the API token would cross the network in cleartext over http to a non-loopback host, where it can be sniffed and replayed to POST spend as any developer — use https://, or target a loopback host (127.0.0.1 / [::1] / localhost)", cfg.ServerURL)
		}
		// No bearer token to leak, so this is allowed — but the spend batch
		// itself still crosses in cleartext, so surface it rather than ship
		// per-developer dollar figures over the open network silently.
		slog.Warn("shipping over cleartext http to a non-loopback host; per-developer spend crosses the network unencrypted, prefer https://",
			"server", cfg.ServerURL)
	}
	batch := cfg.BatchSize
	if batch == 0 {
		batch = DefaultBatchSize
	}
	if batch < 1 || batch > DefaultBatchSize {
		return nil, fmt.Errorf("batch size %d out of range [1, %d]", cfg.BatchSize, DefaultBatchSize)
	}
	attempts := cfg.MaxAttempts
	if attempts == 0 {
		attempts = DefaultMaxAttempts
	}
	if attempts < 1 {
		return nil, fmt.Errorf("max attempts %d must be >= 1", cfg.MaxAttempts)
	}
	backoff := cfg.BackoffBase
	if backoff == 0 {
		backoff = DefaultBackoffBase
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	// Never follow redirects (#182). Go re-sends the Authorization header across
	// a same-host redirect regardless of a scheme downgrade, so a server (mis-
	// configured or compromised) answering with 3xx Location: http://<same-host>
	// would make the client re-POST the bearer token AND the spend batch in
	// cleartext — the exact leak the plaintext guard above refuses at New().
	// A POST /api/v1/events reply has no legitimate reason to redirect, so
	// surface any 3xx as a terminal response (postOnce treats non-201 as an
	// error) instead of chasing it. Only set when the caller hasn't chosen a
	// policy, so a deliberately-configured client is left intact.
	if hc.CheckRedirect == nil {
		hc.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &Client{
		endpoint:    strings.TrimRight(u.String(), "/") + "/api/v1/events",
		apiToken:    cfg.APIToken,
		hc:          hc,
		batchSize:   batch,
		maxAttempts: attempts,
		backoffBase: backoff,
	}, nil
}

// isLoopbackHost reports whether host names the local machine by loopback
// convention. It replicates the loopback test in cmd/tierd's validateBind
// (main.go, the #59 bind guard) — which the shipper package cannot import —
// so the two guards classify hosts identically: a bare "localhost" is trusted
// by name, and any IP literal in the loopback range (127.0.0.0/8, ::1) counts.
//
// host is url.URL.Hostname(): the port is already stripped and IPv6 brackets
// removed, so "[::1]:8080" arrives here as "::1". Like validateBind, this does
// NO DNS resolution — a hostname other than "localhost" that happens to
// resolve to a loopback address is treated as non-loopback, because what it
// resolves to cannot be known statically and fail-closed beats guessing.
// "localhost" is trusted by convention even though an /etc/hosts entry could
// repoint it; that tradeoff is accepted to keep the common dev invocation
// working, matching validateBind exactly.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// Ingest implements collector.Ingester: it buffers ev and, when the buffer
// reaches the batch size, POSTs the batch. An error from the POST propagates
// to the collector's Run loop, which stops — fail loud, never skip events.
func (c *Client) Ingest(ctx context.Context, ev collector.TokenEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, wireEvent{
		Developer:      ev.Developer,
		IssueID:        ev.IssueID,
		Model:          ev.Model,
		InputTok:       ev.InputTok,
		OutputTok:      ev.OutputTok,
		CacheRead:      ev.CacheRead,
		CacheWrite5m:   ev.CacheWrite5m,
		CacheWrite1h:   ev.CacheWrite1h,
		CostUSD:        store.MicroToDollars(ev.CostMicro),
		Source:         ev.Source,
		Fidelity:       ev.Fidelity,
		IdempotencyKey: ev.IdempotencyKey,
		SessionID:      ev.SessionID,
		Repo:           wireRepo(ev.Repo),
		// RFC3339Nano keeps sub-second precision; the server parses it
		// with the RFC3339 layout (Go accepts fractional seconds there).
		Timestamp: ev.Timestamp.UTC().Format(time.RFC3339Nano),
	})
	// Flush on event count only, not serialized byte size. The server caps
	// both 500 events and 1 MiB, but with real collector events (short
	// identifiers, 64-hex idempotency keys) a 500-event batch sits well under
	// 1 MiB, so the byte cap is unreachable here. If a batch ever did exceed
	// it, the server returns a terminal 4xx (no retry) — fail-loud, not silent
	// truncation — so the count-only flush is a safe intentional asymmetry.
	if len(c.buf) < c.batchSize {
		return nil
	}
	return c.flushLocked(ctx)
}

// Flush sends any buffered partial batch. Call once after every collector
// Run completes; a no-op when the buffer is empty (the server 400s empty
// arrays, so we never post one).
func (c *Client) Flush(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 {
		return nil
	}
	return c.flushLocked(ctx)
}

// Shipped reports how many events the server has accepted so far.
func (c *Client) Shipped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shipped
}

// flushLocked drains the buffer in batchSize-bounded chunks, POSTing each
// and dropping it on success. Caller must hold c.mu. Normally the buffer
// holds at most one batch (Ingest flushes at the boundary), so the loop runs
// once; the chunking guards the pathological caller that ignores an Ingest
// error and keeps buffering — the server would 400 an oversized batch. On
// failure the unsent remainder is retained: the CLI exits non-zero and the
// next stateless run re-ships everything anyway.
func (c *Client) flushLocked(ctx context.Context) error {
	for len(c.buf) > 0 {
		n := min(len(c.buf), c.batchSize)
		body, err := json.Marshal(c.buf[:n])
		if err != nil {
			// Unreachable for this struct shape (no NaN/Inf fields survive
			// the collector), but must not vanish if that ever changes.
			return fmt.Errorf("marshal batch: %w", err)
		}
		if err := c.postBatch(ctx, body); err != nil {
			return fmt.Errorf("ship batch of %d events: %w", n, err)
		}
		c.shipped += n
		c.buf = c.buf[n:]
	}
	return nil
}

// postBatch sends one JSON batch with retry. Transport errors and 5xx
// responses retry up to maxAttempts with doubling, capped backoff; any 4xx
// fails immediately (client-side validation bug — retrying cannot fix it and
// skipping it would silently drop spend data). Context cancellation aborts
// both the request and any pending backoff sleep.
func (c *Client) postBatch(ctx context.Context, body []byte) error {
	var lastErr error
	delay := c.backoffBase
	for attempt := 1; ; attempt++ {
		lastErr = c.postOnce(ctx, body)
		if lastErr == nil {
			return nil
		}
		var re *retryableError
		if !errors.As(lastErr, &re) || attempt >= c.maxAttempts {
			return lastErr
		}
		// Capped exponential backoff, interruptible by ctx so SIGINT does
		// not hang the CLI mid-sleep.
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if delay *= 2; delay > maxBackoff {
			delay = maxBackoff
		}
	}
}

// retryableError marks failures worth retrying (transport errors, 5xx).
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// postOnce performs a single POST attempt. 201 is success; 5xx and transport
// failures return a retryableError; anything else (4xx, unexpected 2xx/3xx)
// is terminal and carries the server's error body for diagnosis.
func (c *Client) postOnce(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return &retryableError{fmt.Errorf("POST %s: %w", c.endpoint, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusCreated {
		// Drain so the transport can reuse the connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	err = fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	if resp.StatusCode >= 500 {
		return &retryableError{err}
	}
	// Every 4xx (including 429) is deliberately terminal. The only 429 source
	// is the server's #36 failed-auth lockout, which fires on a wrong token —
	// retrying cannot fix it and re-hammering would extend the lockout.
	return err
}
