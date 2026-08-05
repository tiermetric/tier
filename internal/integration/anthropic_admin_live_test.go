//go:build integration

// #451: the sibling of anthropic_admin_test.go's wire-mock coverage. That
// file proves the poller's double-count-safety CONTRACT against an
// httptest.Server we wrote ourselves — it can never catch the response
// SHAPE being wrong, because we control both sides. This file is the one
// test in the tree that talks to the REAL api.anthropic.com Admin API, so
// the "verified against the live docs" claim in
// internal/collector/anthropicadmin/client.go's struct doc becomes a wire
// fact instead of a docs-read.
//
// BLOCKED on a real sk-ant-admin org key (Steve, org admin) — see #451.
// Per the batch-9 ruling: implement + skip-loud. Every test in this file is
// gated on liveAnthropicAdminCreds() and SKIPS with a specific, actionable
// message when the credential is absent, rather than being silently
// excluded from the build.
package integration

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/anthropicadmin"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

// probeMetrics adapts anthropicadmin.Metrics to a channel so
// TestLive_AnthropicAdminPoller_RealAPI can observe the poller's OWN
// success/failure signal for its first pass — Run()'s return value cannot be
// used for this (see that test's doc). done is buffered so a slow/never-read
// receiver cannot block the poller's goroutine.
type probeMetrics struct{ done chan bool }

func (m probeMetrics) PollComplete(ok bool) {
	select {
	case m.done <- ok:
	default:
	}
}
func (probeMetrics) EventsIngested(int)   {}
func (probeMetrics) CostDeltasPosted(int) {}

// liveAnthropicAdminCreds resolves the credential(s) the #451 live E2E needs
// from the environment. This is the SINGLE discriminator every gated test in
// this file calls — TestLiveAnthropicAdminCreds_SkipGuard below exercises it
// directly, so a mutation to this function (not just to a call site) is what
// the control arm proves against.
//
// org is a purely LOCAL label on the ingested org_actual_spend row — the
// Admin API itself takes no org parameter; the key alone scopes the account.
// It defaults to "live-e2e-default" so only the key is required to run.
func liveAnthropicAdminCreds() (key, org string, ok bool) {
	key = strings.TrimSpace(os.Getenv("TIER_LIVE_ANTHROPIC_ADMIN_KEY"))
	org = strings.TrimSpace(os.Getenv("TIER_LIVE_ANTHROPIC_ADMIN_ORG"))
	if org == "" {
		org = "live-e2e-default"
	}
	return key, org, key != ""
}

// liveAnthropicAdminSkipMessage is the loud, specific skip reason — naming
// exactly which env var is missing and the exact command to set it and
// re-run, per the batch-9 mandate ("not 'skipping'").
const liveAnthropicAdminSkipMessage = "TIER_LIVE_ANTHROPIC_ADMIN_KEY not set — skipping live Anthropic Admin API E2E (#451). " +
	"To run against a real org: export TIER_LIVE_ANTHROPIC_ADMIN_KEY=sk-ant-admin-... (an Admin-scoped org key; " +
	"optionally also TIER_LIVE_ANTHROPIC_ADMIN_ORG=<local-label>), then: " +
	"go test -tags integration ./internal/integration/... -run TestLive_AnthropicAdmin -v"

// TestLiveAnthropicAdminCreds_SkipGuard is the CONTROL ARM for this file's
// skip logic (batch-9 mandate: a loud skip is only trustworthy if something
// proves the gate itself still fires). It drives liveAnthropicAdminCreds()
// under both env states via t.Setenv, so inverting or deleting the
// `key != ""` check inside that function — the exact function every skip in
// this file calls — makes this test fail without needing a real credential.
func TestLiveAnthropicAdminCreds_SkipGuard(t *testing.T) {
	t.Run("absent env must skip", func(t *testing.T) {
		t.Setenv("TIER_LIVE_ANTHROPIC_ADMIN_KEY", "")
		if _, _, ok := liveAnthropicAdminCreds(); ok {
			t.Fatal("ok=true with TIER_LIVE_ANTHROPIC_ADMIN_KEY unset — the live E2E would attempt a real network call with an empty credential instead of skipping")
		}
	})
	t.Run("present env must run", func(t *testing.T) {
		t.Setenv("TIER_LIVE_ANTHROPIC_ADMIN_KEY", "sk-ant-admin-fake-for-guard-test")
		key, _, ok := liveAnthropicAdminCreds()
		if !ok || key != "sk-ant-admin-fake-for-guard-test" {
			t.Fatalf("ok=%v key=%q — the live E2E would incorrectly SKIP with a real credential present", ok, key)
		}
	})
	// Whitespace-only must behave as absent: a common CI-secret-templating
	// failure mode leaves an env var set to "" or " " rather than unset.
	t.Run("whitespace-only env must skip", func(t *testing.T) {
		t.Setenv("TIER_LIVE_ANTHROPIC_ADMIN_KEY", "   ")
		if _, _, ok := liveAnthropicAdminCreds(); ok {
			t.Fatal("ok=true with a whitespace-only key — must be treated the same as absent")
		}
	})
}

// TestLive_AnthropicAdminProbe_RealAPI is the cheapest live proof: one
// authenticated GET (via the #452 Probe method) against the real endpoint,
// classified reachable/authorized. It exists as a fast, isolated first
// signal before the heavier full-poller test below, so a credential problem
// is diagnosed precisely rather than surfacing as an opaque poller error.
func TestLive_AnthropicAdminProbe_RealAPI(t *testing.T) {
	key, _, ok := liveAnthropicAdminCreds()
	if !ok {
		t.Skip(liveAnthropicAdminSkipMessage)
	}
	client := anthropicadmin.NewClient(anthropicadmin.ClientConfig{APIKey: key})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := client.Probe(ctx)
	if res.Err != nil {
		t.Fatalf("probe transport error against the REAL api.anthropic.com: %v (network/DNS/TLS issue, not necessarily a bad key)", res.Err)
	}
	if res.Unauthorized() {
		t.Fatalf("probe HTTP %d: the live key was REJECTED by api.anthropic.com — check TIER_LIVE_ANTHROPIC_ADMIN_KEY is a valid, unexpired sk-ant-admin-... key with usage/cost report scope", res.StatusCode)
	}
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
		// Matches checkCollectorCredential's own classification (cmd/tierd/doctor.go):
		// a 429/5xx is transient/provider-side, not a credential failure — the
		// product itself calls this a WARN, not a FAIL, so this live test must
		// not disagree with it by treating the same status as fatal.
		t.Skipf("probe HTTP %d (rate-limited or provider-side) — not a credential failure, re-run later", res.StatusCode)
	}
	if !res.Authorized() {
		t.Fatalf("probe HTTP %d: reached the real endpoint but got neither a clean 2xx nor 401/403 — unexpected provider response", res.StatusCode)
	}
	t.Logf("live Anthropic Admin API probe: HTTP %d, reachable and authorized", res.StatusCode)
}

// TestLive_AnthropicAdminPoller_RealAPI drives the REAL anthropicadmin.Poller
// (real HTTP client, no BaseURL override) through one poll pass against
// api.anthropic.com, against a real on-disk SQLite store — the actual code
// path `tierd serve` runs, not a proxy for it.
//
// What this DOES prove when it runs: the live response for both the usage
// report and cost report endpoints decodes cleanly into our structs — the
// exact "docs-read, not a wire fact" gap #451 was filed to close. It proves
// this via the poller's own anthropicadmin.Metrics.PollComplete(ok) hook
// (probeMetrics below), NOT via poller.Run's return value: Run's doc
// (poller.go:121-126) is explicit that a poll failure is logged and RETRIED,
// never surfaced up through Run's error return — Run returns non-nil only on
// ctx cancellation. An earlier version of this test asserted on Run's error
// instead and could not fail on a revoked key, a 401, or a decode error: all
// three still return Run() == nil. PollComplete(false) is the one signal the
// poller actually emits when a pass fails.
//
// What this deliberately does NOT hard-assert: that a NONZERO
// org_actual_spend delta lands. reconcileCost only posts a row when the
// provider's cost total for (org, period) differs from what TIER already
// recorded (poller.go's `if delta == 0 { continue }`) — so a maintainer
// re-running this test twice inside the same settled month will correctly
// see a zero delta on the second run. That is idempotency working, not a
// test failure. The remainder (if any) is logged, not asserted nonzero, so
// this test stays meaningful (and green) on repeat runs against a real
// account rather than becoming flaky on account state it does not control.
func TestLive_AnthropicAdminPoller_RealAPI(t *testing.T) {
	key, org, ok := liveAnthropicAdminCreds()
	if !ok {
		t.Skip(liveAnthropicAdminSkipMessage)
	}

	// A plain store, not newServerWithStore's full API+webhook mux: this test
	// never touches HTTP, only the real Poller against the real store, so an
	// httptest.Server + mux would be pure unused overhead.
	db, err := store.Open(filepath.Join(t.TempDir(), "anthropic-admin-live.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	client := anthropicadmin.NewClient(anthropicadmin.ClientConfig{
		APIKey: key,
		Logger: slog.New(slog.NewTextHandler(quietWriter{}, nil)),
	})
	probe := probeMetrics{done: make(chan bool, 1)}
	poller := anthropicadmin.NewPoller(anthropicadmin.PollerConfig{
		Client:   client,
		Store:    db,
		Org:      org,
		Interval: time.Hour, // long: only the immediate first pass runs in-test
		Logger:   slog.New(slog.NewTextHandler(quietWriter{}, nil)),
		Now:      time.Now,
		Metrics:  probe,
	})

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- poller.Run(runCtx, time.Time{}, ingester.Store(db)) }()

	select {
	case passOK := <-probe.done:
		if !passOK {
			t.Fatal("the first live poll pass FAILED (see the poller's ERROR-level log above, from p.logger) — the usage-report and/or cost-report fetch+decode against the REAL Admin API did not succeed. This is exactly the failure batch-9's first version of this test could NOT catch.")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("first live poll pass never signaled completion within 60s (PollComplete was never called) — the poller may be wedged")
	}
	cancel()
	if err := <-runErrCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("poller.Run returned an unexpected error after cancel: %v", err)
	}

	period := time.Now().UTC().Format("2006-01")
	net, err := db.OrgActualSpendNet(ctx, org, period, collector.SourceAnthropicAdmin)
	if err != nil {
		t.Fatalf("OrgActualSpendNet after a real poll: %v", err)
	}
	if net < 0 {
		t.Fatalf("org_actual_spend net for source=%s went NEGATIVE (%d micro) — the clamp-to-zero remainder logic is broken", collector.SourceAnthropicAdmin, net)
	}
	t.Logf("live Anthropic Admin poll complete: org_actual_spend net for (org=%s, period=%s, source=%s) = $%s",
		org, period, collector.SourceAnthropicAdmin, strconv.FormatFloat(float64(net)/1_000_000, 'f', 6, 64))
}
