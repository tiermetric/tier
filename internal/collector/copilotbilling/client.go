// Package copilotbilling captures GitHub Copilot AI-credit GROSS dollars. This
// poller is the single owner of Copilot spend; Opencode's Copilot provider must
// stay excluded. The API supplies neither tokens nor repository attribution.
package copilotbilling

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL   = "https://api.github.com"
	apiVersion       = "2026-03-10"
	permissionHint   = "needs org admin / App permission Administration: read"
	maxResponseBytes = 32 << 20
	maxRetryBackoff  = 30 * time.Second
)

// ClientConfig accepts exactly one authentication mode. PrivateKey is resolved
// PEM, never an @file reference; cmd/tierd owns secret-file resolution.
type ClientConfig struct {
	Org, BaseURL, Token, AppID, InstallationID, PrivateKey string
	HTTPClient                                             *http.Client
	Logger                                                 *slog.Logger
	Now                                                    func() time.Time
	Sleep                                                  func(context.Context, time.Duration) error
	MaxRetries                                             int
}

// Client serializes credential refresh and the once-per-start scope warning.
// Errors deliberately omit URLs, response bodies and decoder/transport errors:
// those can contain the per-user query, upstream text, or credentials.
type Client struct {
	cfg           ClientConfig
	key           *rsa.PrivateKey
	mu            sync.Mutex
	token         string
	expires       time.Time
	scopesChecked bool
}

func NewClient(cfg ClientConfig) (*Client, error) {
	app := cfg.AppID != "" || cfg.InstallationID != "" || cfg.PrivateKey != ""
	if (cfg.Token != "") == app {
		return nil, fmt.Errorf("copilot-billing: configure exactly one of token or complete App credentials")
	}
	if cfg.Org == "" {
		return nil, fmt.Errorf("copilot-billing: org is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.ForceQuery {
		return nil, fmt.Errorf("copilot-billing: invalid api_base_url")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("copilot-billing: api_base_url requires https except on loopback")
		}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	// Copy even an injected client: redirects must never forward credentials or
	// make the probe report a redirect target's authorization as GitHub's.
	hc := http.Client{Timeout: 60 * time.Second}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
		if hc.Timeout == 0 {
			hc.Timeout = 60 * time.Second
		}
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTPClient = &hc
	c := &Client{cfg: cfg}
	if app {
		if cfg.AppID == "" || cfg.InstallationID == "" || cfg.PrivateKey == "" {
			return nil, fmt.Errorf("copilot-billing: app_id, installation_id and private_key are all required")
		}
		for _, id := range []string{cfg.AppID, cfg.InstallationID} {
			n, e := strconv.ParseUint(id, 10, 64)
			if e != nil || n == 0 {
				return nil, fmt.Errorf("copilot-billing: App IDs must be positive integers")
			}
		}
		block, _ := pem.Decode([]byte(cfg.PrivateKey))
		if block == nil {
			return nil, fmt.Errorf("copilot-billing: invalid RSA private key PEM")
		}
		key, e := x509.ParsePKCS1PrivateKey(block.Bytes)
		if e != nil {
			parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
			if e == nil {
				key, _ = parsed.(*rsa.PrivateKey)
			}
		}
		if key == nil || key.Validate() != nil {
			return nil, fmt.Errorf("copilot-billing: invalid RSA private key")
		}
		c.key = key
	}
	// Keep the parsed key, not another PEM copy.
	c.cfg.PrivateKey = ""
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) AuthMode() string {
	if c.key != nil {
		return "app"
	}
	return "token"
}

func (c *Client) jwt() (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	now := c.cfg.Now()
	claims, _ := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{c.cfg.AppID, now.Add(-time.Minute).Unix(), now.Add(9 * time.Minute).Unix()})
	unsigned := header + "." + enc(claims)
	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("copilot-billing: JWT signing failed")
	}
	return unsigned + "." + enc(sig), nil
}

func (c *Client) bearer(ctx context.Context, retries int) (string, error) {
	if c.key == nil {
		return c.cfg.Token, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.cfg.Now().Before(c.expires.Add(-5*time.Minute)) {
		return c.token, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	var rep struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, err = c.request(ctx, http.MethodPost, "/app/installations/"+c.cfg.InstallationID+"/access_tokens", nil, jwt, &rep, retries)
	if err != nil {
		return "", err
	}
	if rep.Token == "" || !rep.ExpiresAt.After(c.cfg.Now().Add(5*time.Minute)) {
		return "", fmt.Errorf("copilot-billing: unusable installation token response")
	}
	c.token, c.expires = rep.Token, rep.ExpiresAt
	return c.token, nil
}

// usageReport deliberately ignores any seat lists or unknown user metadata.
// An org response carrying User is evidently requester-scoped and unusable.
type usageReport struct {
	TimePeriod struct {
		Year  int `json:"year"`
		Month int `json:"month"`
		Day   int `json:"day"`
	} `json:"timePeriod"`
	Organization string      `json:"organization"`
	User         string      `json:"user"`
	Product      string      `json:"product"`
	Model        string      `json:"model"`
	UsageItems   []usageItem `json:"usageItems"`
}
type usageItem struct {
	Product     string   `json:"product"`
	SKU         string   `json:"sku"`
	Model       string   `json:"model"`
	GrossAmount *float64 `json:"grossAmount"`
}

// usageNotFoundError identifies a 404 from usage after successful authentication,
// never a failed installation-token exchange.
type usageNotFoundError struct{}

func (usageNotFoundError) Error() string { return statusError(http.StatusNotFound).Error() }

func (c *Client) usage(ctx context.Context, day time.Time, login string, retries int) (usageReport, int, error) {
	var rep usageReport
	token, err := c.bearer(ctx, retries)
	if err != nil {
		return rep, statusOf(err), err
	}
	q := url.Values{"year": {strconv.Itoa(day.Year())}, "month": {strconv.Itoa(int(day.Month()))}, "day": {strconv.Itoa(day.Day())}}
	if login != "" {
		q.Set("user", login)
	}
	path := "/organizations/" + url.PathEscape(c.cfg.Org) + "/settings/billing/ai_credit/usage"
	status, err := c.request(ctx, http.MethodGet, path, q, token, &rep, retries)
	if err != nil {
		if status == http.StatusNotFound {
			return rep, status, usageNotFoundError{}
		}
		return rep, status, err
	}
	if rep.TimePeriod.Year != day.Year() || rep.TimePeriod.Month != int(day.Month()) || rep.TimePeriod.Day != day.Day() || !strings.EqualFold(rep.Organization, c.cfg.Org) || rep.UsageItems == nil || !strings.EqualFold(rep.User, login) || (login == "" && (rep.Product != "" || rep.Model != "")) {
		return usageReport{}, status, fmt.Errorf("copilot-billing: unusable usage response (scope, period, organization or items)")
	}
	return rep, status, nil
}

// Probe makes one org-level call for yesterday, with no usage retries. App
// authentication may first require the installation-token exchange.
type ProbeResult struct {
	StatusCode, ItemCount int
	AuthMode              string
	Err                   error
	RateLimited           bool
}

func (c *Client) Probe(ctx context.Context) ProbeResult {
	day := c.cfg.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	rep, status, err := c.usage(ctx, day, "", 0)
	_, rateLimited := err.(rateLimitError)
	return ProbeResult{StatusCode: status, ItemCount: len(rep.UsageItems), AuthMode: c.AuthMode(), Err: err, RateLimited: rateLimited}
}

// Rate-limit 403s must never be reported as a permission failure.
type rateLimitError struct{}

func (rateLimitError) Error() string { return "copilot-billing: HTTP 403; rate limit exhausted" }

type statusError int

func (e statusError) Error() string {
	if e == 401 || e == 403 || e == 404 {
		return fmt.Sprintf("copilot-billing: HTTP %d; %s", e, permissionHint)
	}
	return fmt.Sprintf("copilot-billing: HTTP %d", e)
}
func statusOf(err error) int {
	if _, ok := err.(usageNotFoundError); ok {
		return http.StatusNotFound
	}
	if _, ok := err.(rateLimitError); ok {
		return http.StatusForbidden
	}
	if s, ok := err.(statusError); ok {
		return int(s)
	}
	return 0
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, token string, out any, retries int) (int, error) {
	endpoint := c.cfg.BaseURL + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	var status int
	var lastErr error
	var retryAfter time.Duration
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			delay := 500 * time.Millisecond << min(attempt-1, 6)
			if err := c.cfg.Sleep(ctx, min(max(delay, retryAfter), maxRetryBackoff)); err != nil {
				return status, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
		if err != nil {
			return 0, fmt.Errorf("copilot-billing: cannot build request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		resp, err := c.cfg.HTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("copilot-billing: HTTP transport failed")
			retryAfter = 0
			continue
		}
		status = resp.StatusCode
		if c.key == nil {
			c.checkScopes(resp.Header)
		}
		if status >= 200 && status < 300 {
			err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out)
			_ = resp.Body.Close()
			if err != nil {
				return status, fmt.Errorf("copilot-billing: invalid JSON response")
			}
			return status, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		lastErr = statusError(status)
		rateLimited := status == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"
		if rateLimited {
			lastErr = rateLimitError{}
		}
		if !rateLimited && status != 429 && status < 500 {
			return status, lastErr
		}
		retryAfter = 0
		if rateLimited {
			if reset, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
				retryAfter = min(max(time.Unix(reset, 0).Sub(c.cfg.Now()), 0), maxRetryBackoff)
			}
		}
		if secs, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); e == nil && secs > 0 {
			retryAfter = max(retryAfter, time.Duration(min(secs, 30))*time.Second)
		}
	}
	return status, lastErr
}

func (c *Client) checkScopes(h http.Header) {
	raw, present := h[http.CanonicalHeaderKey("X-OAuth-Scopes")]
	if !present {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scopesChecked {
		return
	}
	c.scopesChecked = true
	var extra []string
	for _, scope := range strings.Split(strings.Join(raw, ","), ",") {
		scope = strings.TrimSpace(scope)
		if scope == "" || scope == "read:org" || scope == "admin:org" || scope == "manage_billing:copilot" {
			continue
		}
		// Only scope-name characters can enter the log; never echo raw headers.
		valid := len(scope) <= 80
		for _, ch := range scope {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != ':' && ch != '_' && ch != '-' {
				valid = false
			}
		}
		if valid && len(extra) < 20 {
			extra = append(extra, scope)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		c.cfg.Logger.Warn("copilot-billing: static token has broader OAuth scopes than required", "scopes", strings.Join(extra, ","))
	}
}
