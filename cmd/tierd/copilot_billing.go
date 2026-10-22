package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/collector/copilotbilling"
	"github.com/tiermetric/tier/internal/config"
)

type copilotBillingSettings struct {
	client                   *copilotbilling.Client
	interval                 time.Duration
	settleDays, lookbackDays int
}

// resolveCopilotBillingConfig is shared by serve and doctor. Resolve secrets
// before constructing the client; error messages contain field names only.
func resolveCopilotBillingConfig(c *config.CopilotBillingConfig, logger *slog.Logger) (*copilotBillingSettings, error) {
	if c == nil {
		return nil, nil
	}
	value := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	cc := copilotbilling.ClientConfig{
		Logger: logger,
		Org:    strings.TrimSpace(value(c.Org)), AppID: value(c.AppID), InstallationID: value(c.InstallationID), BaseURL: value(c.APIBaseURL),
	}
	s := &copilotBillingSettings{interval: copilotbilling.DefaultPollInterval, settleDays: copilotbilling.DefaultSettleDays, lookbackDays: copilotbilling.DefaultLookbackDays}
	var err error
	cc.Token, err = resolveSecretFlag("collectors.copilot_billing.token", value(c.Token))
	if err != nil {
		return nil, fmt.Errorf("collectors.copilot_billing.token: secret resolution failed")
	}
	cc.PrivateKey, err = resolveSecretFlag("collectors.copilot_billing.private_key", value(c.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("collectors.copilot_billing.private_key: secret resolution failed")
	}
	if c.PollInterval != nil {
		s.interval, err = time.ParseDuration(*c.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("collectors.copilot_billing.poll_interval: invalid duration")
		}
	}
	if s.interval < 15*time.Minute {
		return nil, fmt.Errorf("collectors.copilot_billing.poll_interval must be >= 15m")
	}
	if c.SettleDays != nil {
		s.settleDays = *c.SettleDays
	}
	if c.LookbackDays != nil {
		s.lookbackDays = *c.LookbackDays
	}
	// Bound each pass to at most 31 daily requests per rostered login.
	if s.settleDays < 1 || s.lookbackDays < s.settleDays || s.lookbackDays > 31 {
		return nil, fmt.Errorf("collectors.copilot_billing: require 1 <= settle_days <= lookback_days <= 31")
	}
	s.client, err = copilotbilling.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("collectors.copilot_billing: %w", err)
	}
	return s, nil
}

func checkCopilotBillingConfig(ctx context.Context, c *config.CopilotBillingConfig) []checkResult {
	s, err := resolveCopilotBillingConfig(c, slog.New(slog.DiscardHandler))
	if err != nil {
		return []checkResult{{name: "copilot_billing credential", status: statusFail, detail: err.Error(), hint: "fix collectors.copilot_billing; tierd serve would refuse to start"}}
	}
	if s == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, doctorCollectorProbeTimeout)
	defer cancel()
	res := s.client.Probe(pctx)
	r := checkResult{name: "copilot_billing credential", status: statusOK, detail: fmt.Sprintf("HTTP %d; items %d; auth mode %s", res.StatusCode, res.ItemCount, res.AuthMode)}
	if res.Err != nil {
		r.status = statusWarn
		if res.StatusCode == 0 {
			r.status = statusFail
			r.detail = fmt.Sprintf("endpoint unreachable: %v; %s", res.Err, r.detail)
			r.hint = "check network access to the provider's API from this host; the poller cannot run without it"
		} else if (res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404) && !res.RateLimited {
			r.status = statusFail
			r.hint = "needs org admin / App permission Administration: read; verify org-level usage support"
		}
	}
	return []checkResult{r}
}
