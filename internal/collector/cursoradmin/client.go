// Package cursoradmin reads Cursor team usage, prices requests individually,
// and reconciles daily developer totals. Admin credentials never leave this client.
// Days inside the lookback reflect the active price table: a table change
// re-costs those days through delta rows. Older days are frozen.
// Non-chargeable tokens on priced models are valued at list rate for included
// or flat-fee usage. Non-chargeable usage on unpriced models with zero vendor
// cents is skipped and counted as NonChargeable.
package cursoradmin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL   = "https://api.cursor.com"
	usagePath        = "/teams/filtered-usage-events"
	AuthHint         = "needs a Cursor Admin API key; the Admin API may require an Enterprise plan"
	maxPages         = 10000
	maxResponseBytes = 32 << 20
)

// ClientConfig follows the org pollers' bounded retry and injectable sleep seam.
type ClientConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	MaxRetries int
	Sleep      func(context.Context, time.Duration) error
}

type Client struct {
	key, baseURL string
	http         *http.Client
	retries      int
	sleep        func(context.Context, time.Duration) error
	lastRequest  time.Time
}

func NewClient(cfg ClientConfig) *Client {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	h := http.Client{Timeout: 60 * time.Second}
	if cfg.HTTPClient != nil {
		h = *cfg.HTTPClient
		if h.Timeout <= 0 {
			h.Timeout = 60 * time.Second
		}
	}
	// A redirect must neither leak the Basic credential nor make doctor grade a target.
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	retries := cfg.MaxRetries
	if retries == 0 {
		retries = 3
	}
	if retries < 0 {
		retries = 0
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	return &Client{key: cfg.APIKey, baseURL: base, http: &h, retries: retries, sleep: sleep}
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

// Only identity evidence needed for alias lookup is decoded. Conversation IDs
// are deliberately absent. Service account names only detect non-human identity.
type usageEvent struct {
	Timestamp          string      `json:"timestamp"`
	Email              string      `json:"userEmail"`
	ServiceAccountID   string      `json:"serviceAccountId"`
	ServiceAccountName string      `json:"serviceAccountName"`
	CloudAgentID       string      `json:"cloudAgentId"`
	AutomationID       string      `json:"automationId"`
	Model              string      `json:"model"`
	Kind               string      `json:"kind"`
	IsTokenBasedCall   bool        `json:"isTokenBasedCall"`
	IsChargeable       bool        `json:"isChargeable"`
	IsHeadless         bool        `json:"isHeadless"`
	TokenUsage         *tokenUsage `json:"tokenUsage"`
	ChargedCents       float64     `json:"chargedCents"`
}
type tokenUsage struct {
	Input      int     `json:"inputTokens"`
	Output     int     `json:"outputTokens"`
	CacheWrite int     `json:"cacheWriteTokens"`
	CacheRead  int     `json:"cacheReadTokens"`
	TotalCents float64 `json:"totalCents"`
}
type usageReport struct {
	Total      int `json:"totalUsageEventsCount"`
	Pagination struct {
		NumPages    int  `json:"numPages"`
		CurrentPage int  `json:"currentPage"`
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pagination"`
	Events []usageEvent `json:"usageEvents"`
}
type usageRequest struct {
	StartDate int64 `json:"startDate"`
	EndDate   int64 `json:"endDate"`
	Page      int   `json:"page"`
	PageSize  int   `json:"pageSize"`
}

// StatusError contains only the HTTP status, never response bodies or request data.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	if e.Status == 401 || e.Status == 403 {
		return fmt.Sprintf("cursor-admin HTTP %d: %s", e.Status, AuthHint)
	}
	return fmt.Sprintf("cursor-admin HTTP %d", e.Status)
}

func (c *Client) request(ctx context.Context, day time.Time, page, size int) (*http.Response, error) {
	// Space ALL attempts by at least a second: the team quota is 60/min, shared
	// across pages and retries. One poll loop owns this client, like its siblings.
	if !c.lastRequest.IsZero() {
		if wait := time.Until(c.lastRequest.Add(time.Second)); wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
	}
	body, _ := json.Marshal(usageRequest{day.UnixMilli(), day.AddDate(0, 0, 1).UnixMilli() - 1, page, size})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+usagePath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cursor-admin: cannot build request")
	}
	req.SetBasicAuth(c.key, "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.lastRequest = time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Transport errors may embed a URL or an upstream-controlled string.
		return nil, fmt.Errorf("cursor-admin: HTTP transport failed")
	}
	return resp, nil
}

func decode(resp *http.Response, out *usageReport) error {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return fmt.Errorf("cursor-admin: response read failed or exceeded limit")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("cursor-admin: invalid usage response")
	}
	return nil
}

// fetchDay never exposes partial results as a complete day. The consumer builds
// an in-memory aggregate; reconciliation begins only after pagination succeeds.
func (c *Client) fetchDay(ctx context.Context, day time.Time, consume func([]usageEvent) error) error {
	expected, seen := -1, 0
	for page := 1; page <= maxPages; page++ {
		var rep usageReport
		var lastErr error
		wait := time.Duration(0)
		for attempt := 0; attempt <= c.retries; attempt++ {
			if attempt > 0 {
				if err := c.sleep(ctx, wait); err != nil {
					return err
				}
			}
			resp, err := c.request(ctx, day, page, 1000)
			if err == nil {
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					lastErr = decode(resp, &rep)
					break
				}
				status := resp.StatusCode
				wait = min(500*time.Millisecond*time.Duration(1<<attempt), 30*time.Second)
				if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
					wait = max(wait, time.Duration(min(seconds, 30))*time.Second)
				}
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				lastErr = &StatusError{status}
				if status != 429 && status < 500 {
					return lastErr
				}
			} else {
				lastErr = err
				wait = min(500*time.Millisecond*time.Duration(1<<attempt), 30*time.Second)
			}
		}
		if lastErr != nil {
			return lastErr
		}
		if expected < 0 {
			expected = rep.Total
		}
		if rep.Total < 0 || rep.Total != expected || len(rep.Events) > 1000 {
			return fmt.Errorf("cursor-admin: inconsistent event count")
		}
		seen += len(rep.Events)
		empty := page == 1 && rep.Total == 0 && len(rep.Events) == 0 && rep.Pagination.NumPages == 0 && !rep.Pagination.HasNextPage
		if rep.Pagination.CurrentPage != page || (rep.Pagination.NumPages < page && !empty) {
			return fmt.Errorf("cursor-admin: invalid pagination")
		}
		if err := consume(rep.Events); err != nil {
			return err
		}
		if !rep.Pagination.HasNextPage {
			if (rep.Pagination.NumPages != page && !empty) || seen != expected {
				return fmt.Errorf("cursor-admin: incomplete pagination")
			}
			return nil
		}
	}
	return fmt.Errorf("cursor-admin: pagination exceeded %d pages", maxPages)
}

// Probe makes exactly one minimal request for yesterday; no retries or ingestion.
type ProbeResult struct {
	StatusCode, EventCount int
	Err                    error
}

func (c *Client) Probe(ctx context.Context) ProbeResult {
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	resp, err := c.request(ctx, day, 1, 1)
	if err != nil {
		return ProbeResult{Err: err}
	}
	result := ProbeResult{StatusCode: resp.StatusCode}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var rep usageReport
		result.Err = decode(resp, &rep)
		result.EventCount = rep.Total
	} else {
		_ = resp.Body.Close()
	}
	return result
}
