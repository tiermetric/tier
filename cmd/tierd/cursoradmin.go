package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/collector/cursoradmin"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/metrics"
)

type cursorAdminSettings struct {
	apiKey, baseURL          string
	interval                 time.Duration
	settleDays, lookbackDays int
}

// resolveCursorAdminConfig uses the same secret boundary as the org pollers.
// Presence enables it; every invalid present value is a startup error.
func resolveCursorAdminConfig(c *config.CursorAdminConfig) (*cursorAdminSettings, error) {
	if c == nil {
		return nil, nil
	}
	raw := ""
	if c.APIKey != nil {
		raw = *c.APIKey
	}
	key, err := resolveSecretFlag("collectors.cursor_admin.api_key", raw)
	if err != nil {
		return nil, fmt.Errorf("collectors.cursor_admin.api_key: %w", err)
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("collectors.cursor_admin: api_key is required")
	}
	s := &cursorAdminSettings{apiKey: key, interval: cursoradmin.DefaultPollInterval, settleDays: cursoradmin.DefaultSettleDays, lookbackDays: cursoradmin.DefaultLookbackDays}
	if c.PollInterval != nil {
		d, err := time.ParseDuration(*c.PollInterval)
		if err != nil || d < time.Hour {
			return nil, fmt.Errorf("collectors.cursor_admin.poll_interval must be a duration >= 1h")
		}
		s.interval = d
	}
	if c.SettleDays != nil {
		s.settleDays = *c.SettleDays
	}
	if c.LookbackDays != nil {
		s.lookbackDays = *c.LookbackDays
	}
	if s.settleDays < 1 || s.lookbackDays < s.settleDays {
		return nil, fmt.Errorf("collectors.cursor_admin: require 1 <= settle_days <= lookback_days")
	}
	if c.APIBaseURL != nil {
		u, err := url.Parse(*c.APIBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("collectors.cursor_admin.api_base_url must be an HTTP(S) origin without credentials, query or fragment")
		}
		base, err := validateAPIBaseURL(u.String())
		if err != nil {
			return nil, fmt.Errorf("collectors.cursor_admin.api_base_url: %w", err)
		}
		s.baseURL = base
	}
	return s, nil
}

type cursorAdminMetrics struct{ polls, events, counts, auth *metrics.CounterVec }

func (m cursorAdminMetrics) PollComplete(ok bool) {
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.polls.Inc(outcome)
}
func (m cursorAdminMetrics) EventsIngested(n int) { m.events.Add(float64(n)) }
func (m cursorAdminMetrics) AuthFailure(status int) {
	label := "other"
	if status == 401 || status == 403 {
		label = fmt.Sprint(status)
	}
	m.auth.Inc(label)
}
func (m cursorAdminMetrics) Counts(c cursoradmin.Counts) {
	for _, v := range []struct {
		name string
		n    int
	}{{"fallback", c.Fallback}, {"uncosted", c.Uncosted}, {"byo", c.BYO}, {"nonchargeable", c.NonChargeable}, {"clamped", c.Clamped}} {
		m.counts.Add(float64(v.n), v.name)
	}
}

func checkCursorAdminConfig(ctx context.Context, c *config.CursorAdminConfig) []checkResult {
	s, err := resolveCursorAdminConfig(c)
	if err != nil {
		return []checkResult{{name: "cursor_admin credential", status: statusFail, detail: err.Error(), hint: "fix collectors.cursor_admin — tierd serve would refuse to start"}}
	}
	if s == nil {
		return nil
	}
	client := cursoradmin.NewClient(cursoradmin.ClientConfig{APIKey: s.apiKey, BaseURL: s.baseURL})
	pctx, cancel := context.WithTimeout(ctx, doctorCollectorProbeTimeout)
	defer cancel()
	r := client.Probe(pctx)
	status := statusOK
	if r.Err != nil || r.StatusCode == 401 || r.StatusCode == 403 {
		status = statusFail
	} else if r.StatusCode < 200 || r.StatusCode >= 300 {
		status = statusWarn
	}
	// Never print response fields other than status and count, including on 403.
	result := checkResult{name: "cursor_admin credential", status: status, detail: fmt.Sprintf("HTTP %d; event count %d", r.StatusCode, r.EventCount)}
	if r.StatusCode == 401 || r.StatusCode == 403 {
		result.hint = cursoradmin.AuthHint
	} else if status == statusWarn {
		result.hint = "re-run doctor shortly; if it persists, check the provider's status page"
	} else if r.Err != nil {
		result.hint = "check network access to the provider's API from this host; the poller cannot run without it"
	}
	return []checkResult{result}
}
