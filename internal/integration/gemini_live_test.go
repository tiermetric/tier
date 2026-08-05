//go:build integration

// #459 task 4: live E2E for the newly-mounted /gemini/ reverse-proxy route.
// Before this batch, `tierd serve` mounted no /gemini/ route at all — any
// Gemini client pointed at TIER got a plain 404 (docs/how-it-works.md's ‡
// note documented this as "not capturable"). The mount now exists
// (cmd/tierd/main.go's --gemini-target / proxy.gemini_target), wired onto
// parseGemini (internal/proxy/proxy.go), which has existed since v1 (#1),
// extended by #122 (thinking/cache tokens) and #300 (host stamping), and has
// been unit-tested against synthetic response bodies the whole time — but has
// NEVER seen a real generativelanguage.googleapis.com response.
//
// Per the batch-9 ruling: implement + skip-loud. Gemini's free tier (a key
// from https://aistudio.google.com/apikey, no billing required for the
// default rate limits) makes this the cheapest of the four live E2Es to
// actually run — but this session has no such key, so it is exercised here
// only as a skip-loud, mutation-proven gate, exactly like #451.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/proxy"
	"github.com/tiermetric/tier/internal/store"
)

// liveGeminiDefaultModel is used when TIER_LIVE_GEMINI_MODEL is unset. Override
// via env if Google has retired it by the time this actually runs with a key —
// see docs/reference-price-table.md for current model ids.
//
// 🔴 IT MUST BE A MODEL internal/store/prices.yaml ACTUALLY PRICES (#452 review
// Y2). The previous default, gemini-2.0-flash-lite, was NOT in the table: a real
// run against it produced
//
//	WARN: model "gemini-2.0-flash-lite" not in the price table; priced by a
//	GUESS at the self-hosted-medium reference rate ($0.50/M combined)…
//
// and stored cost_micro=9, billing_mode=self_hosted_amortized. Since this test
// asserted only model/tokens/host/source/attribution, the ONE live run meant to
// retire "Gemini is live-unverified" would have come back green while writing a
// guessed figure — and been read as verified capture INCLUDING pricing.
// TestGeminiDefaultModel_IsPriced pins this hermetically, so the default cannot
// drift back off the table without a credential-free test failing.
const liveGeminiDefaultModel = "gemini-2.0-flash"

// liveGeminiCreds resolves the credential the #459 task 4 live E2E needs.
// This is the SINGLE discriminator every gated test in this file calls —
// TestLiveGeminiCreds_SkipGuard exercises it directly, so a mutation to this
// function (not just to a call site) is what the control arm proves against.
func liveGeminiCreds() (key, endpoint, model string, ok bool) {
	key = strings.TrimSpace(os.Getenv("TIER_LIVE_GEMINI_KEY"))
	endpoint = strings.TrimSpace(os.Getenv("TIER_LIVE_GEMINI_TARGET"))
	if endpoint == "" {
		endpoint = "https://generativelanguage.googleapis.com"
	}
	model = strings.TrimSpace(os.Getenv("TIER_LIVE_GEMINI_MODEL"))
	if model == "" {
		model = liveGeminiDefaultModel
	}
	return key, endpoint, model, key != ""
}

const liveGeminiSkipMessage = "TIER_LIVE_GEMINI_KEY not set — skipping live Gemini proxy E2E (#459 task 4). " +
	"To run: get a free-tier key from https://aistudio.google.com/apikey, then: " +
	"export TIER_LIVE_GEMINI_KEY=... (optionally TIER_LIVE_GEMINI_MODEL=<model-id> if the default " +
	"\"" + liveGeminiDefaultModel + "\" has been retired), then: " +
	"go test -tags integration ./internal/integration/... -run TestLive_Gemini -v"

// TestLiveGeminiCreds_SkipGuard is the CONTROL ARM for this file's skip
// logic. It drives liveGeminiCreds() under both env states via t.Setenv, so
// inverting or deleting the `key != ""` check makes this test fail without
// needing a real credential.
func TestLiveGeminiCreds_SkipGuard(t *testing.T) {
	t.Run("absent env must skip", func(t *testing.T) {
		t.Setenv("TIER_LIVE_GEMINI_KEY", "")
		if _, _, _, ok := liveGeminiCreds(); ok {
			t.Fatal("ok=true with TIER_LIVE_GEMINI_KEY unset — the live E2E would attempt a real network call with an empty credential instead of skipping")
		}
	})
	t.Run("present env must run", func(t *testing.T) {
		t.Setenv("TIER_LIVE_GEMINI_KEY", "fake-gemini-key-for-guard-test")
		key, _, _, ok := liveGeminiCreds()
		if !ok || key != "fake-gemini-key-for-guard-test" {
			t.Fatalf("ok=%v key=%q — the live E2E would incorrectly SKIP with a real credential present", ok, key)
		}
	})
	t.Run("whitespace-only env must skip", func(t *testing.T) {
		t.Setenv("TIER_LIVE_GEMINI_KEY", "   ")
		if _, _, _, ok := liveGeminiCreds(); ok {
			t.Fatal("ok=true with a whitespace-only key — must be treated the same as absent")
		}
	})
	t.Run("default model applies when unset", func(t *testing.T) {
		t.Setenv("TIER_LIVE_GEMINI_KEY", "k")
		t.Setenv("TIER_LIVE_GEMINI_MODEL", "")
		_, _, model, _ := liveGeminiCreds()
		if model != liveGeminiDefaultModel {
			t.Fatalf("model = %q, want default %q", model, liveGeminiDefaultModel)
		}
	})
	t.Run("model override is honored", func(t *testing.T) {
		t.Setenv("TIER_LIVE_GEMINI_KEY", "k")
		t.Setenv("TIER_LIVE_GEMINI_MODEL", "gemini-3.1-flash-lite")
		_, _, model, _ := liveGeminiCreds()
		if model != "gemini-3.1-flash-lite" {
			t.Fatalf("model = %q, want override %q", model, "gemini-3.1-flash-lite")
		}
	})
}

// TestLive_GeminiProxy_RealCompletion sends ONE real generateContent request
// through TIER's actual /gemini/ reverse proxy (proxy.New(..., ProviderGemini,
// ...) + ServeHTTP — the exact code path `tierd serve --gemini-target` mounts)
// to the real Gemini API, and asserts a real token_event lands. This is the
// live proof that #459 task 4's mount is not just structurally present but
// actually forwards and captures real Gemini traffic — the gap the ‡ note in
// docs/how-it-works.md names explicitly.
func TestLive_GeminiProxy_RealCompletion(t *testing.T) {
	key, endpoint, model, ok := liveGeminiCreds()
	if !ok {
		t.Skip(liveGeminiSkipMessage)
	}
	t.Logf("live Gemini key present — exercising a REAL generateContent call against %s, model %q", endpoint, model)

	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "gemini-live.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	p := proxy.New(target, proxy.ProviderGemini, collector.SourceProxy, ingester.Store(db), nil, nil,
		slog.New(slog.NewTextHandler(quietWriter{}, nil)))
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// Gemini's REST API accepts the key EITHER as a `?key=` query parameter OR
	// as the `x-goog-api-key` header — both are documented, equally "the real
	// client contract", and the proxy forwards either unchanged. The header
	// form is used here deliberately: a query-string key rides inside every
	// *url.Error a transport failure produces (net/http's error formatting
	// includes the full request URL, and url.Error redacts only userinfo
	// passwords, never query params) — so a plain `%v` on a connection-refused
	// or DNS error below would print the live secret into test output/CI
	// logs. The header form has no such leak path: Go's transport errors never
	// include header values, matching how the Anthropic Admin key (x-api-key)
	// and OpenAI key (Authorization) are handled in the sibling live tests.
	reqPath := fmt.Sprintf("/v1beta/models/%s:generateContent", model)
	reqBody := `{"contents":[{"parts":[{"text":"Reply with exactly one word: OK."}]}]}`
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxySrv.URL+reqPath, strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)
	req.Header.Set("X-Tier-Developer", "live-e2e-dev")
	req.Header.Set("X-Tier-Issue", "live-e2e-issue")
	req.Header.Set("X-Tier-Repo", "tiermetric/tier")

	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST through the real proxy to the real Gemini API: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live Gemini call returned HTTP %d: %s", resp.StatusCode, body)
	}
	t.Logf("real Gemini response (truncated): %.300s", body)

	// The store write is SYNCHRONOUS within the request lifecycle (handleJSON
	// runs inside modifyResponse, per proxy.go's recordWrite doc), so the
	// event is already durable by the time httpClient.Do above returned. This
	// short poll is cheap insurance, not a wait for genuine async work.
	var events []store.TokenEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, _, err = db.ListTokenEvents(ctx, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour), store.PageCursor{}, 100)
		if err != nil {
			t.Fatalf("ListTokenEvents: %v", err)
		}
		if len(events) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Exactly one, not just "at least one": this also catches a double-capture
	// regression (one request emitting two events), which `> 0` cannot see.
	if len(events) != 1 {
		t.Fatalf("got %d token_event(s) after ONE real Gemini completion through the proxy, want exactly 1 (0 = capture silently dropped it; >1 = double-capture)", len(events))
	}
	ev := events[0]
	if ev.Model == "" {
		t.Error("captured Model is empty — Gemini's modelVersion field did not parse")
	}
	if ev.InputTok <= 0 || ev.OutputTok <= 0 {
		t.Errorf("captured usage InputTok=%d OutputTok=%d — a real model call must report nonzero usage on both sides", ev.InputTok, ev.OutputTok)
	}
	if ev.Host != target.Hostname() {
		t.Errorf("captured Host = %q, want %q", ev.Host, target.Hostname())
	}
	if ev.Source != collector.SourceProxy {
		t.Errorf("captured Source = %q, want %q", ev.Source, collector.SourceProxy)
	}
	if ev.Developer != "live-e2e-dev" || ev.IssueID != "live-e2e-issue" {
		t.Errorf("attribution not threaded through: developer=%q issue=%q", ev.Developer, ev.IssueID)
	}
	// PRICING, not just capture (#452 review Y2). Everything above would pass
	// on an event priced by the unaudited self-hosted-medium GUESS — which is
	// exactly what happened with the old unpriced default model. This test is
	// the one thing that retires "Gemini is live-unverified", so it must not
	// certify a fabricated cost.
	//
	// billing_mode is the discriminator: ComputeCostHost returns per_token only
	// for an exact price-table hit; BOTH guess paths (size-class heuristic and
	// flat fallback) resolve to self_hosted_amortized. Note this grades the
	// model the API actually REPORTED (Gemini's modelVersion), not the one we
	// requested — if Google answers with a versioned id absent from the table,
	// this SHOULD fail: real capture is being priced by a guess, and that is a
	// finding, not a test bug. Fix it by adding the id to prices.yaml (or
	// setting TIER_LIVE_GEMINI_MODEL), never by relaxing this assertion.
	if ev.BillingMode != store.BillingPerToken {
		t.Errorf("captured BillingMode = %q, want %q — model %q is not in the price table, so this cost is an unaudited GUESS at the self-hosted reference rate, not a real Gemini rate",
			ev.BillingMode, store.BillingPerToken, ev.Model)
	}
	if ev.CostMicro <= 0 {
		t.Errorf("captured CostMicro = %d, want > 0 for a priced Gemini call", ev.CostMicro)
	}
	if ev.PriceVersion == 0 {
		t.Error("captured PriceVersion = 0 — the row records no price-table provenance")
	}
	t.Logf("REAL Gemini capture verified: model=%s host=%s input_tok=%d output_tok=%d cost_micro=%d billing_mode=%s price_version=%d",
		ev.Model, ev.Host, ev.InputTok, ev.OutputTok, ev.CostMicro, ev.BillingMode, ev.PriceVersion)
}

// TestGeminiDefaultModel_IsPriced is the CREDENTIAL-FREE guard for the default
// model above (#452 review Y2). It needs no key and no network, so it runs on
// every `make check-full` — which matters, because the assertion it protects
// lives inside a test that SKIPS for everyone without a Gemini key. Without
// this, the default could silently drift back to an unpriced id and nobody
// would learn until someone ran the live test and believed its green.
//
// Deliberately NOT named TestLive_*: scripts/e2e-summary.sh counts that prefix
// as "ran for real against a live endpoint", and this one never touches the
// network.
func TestGeminiDefaultModel_IsPriced(t *testing.T) {
	usage := store.CostUsage{Input: 1000, Output: 1000}
	cost, mode := store.ComputeCostHost("generativelanguage.googleapis.com", liveGeminiDefaultModel, usage)
	if mode != store.BillingPerToken {
		t.Fatalf("liveGeminiDefaultModel %q resolves to billing_mode %q (cost_micro=%d) — it is NOT in internal/store/prices.yaml, so a live run would assert a GUESSED cost. Pick a model the table prices (e.g. gemini-2.0-flash, gemini-2.5-flash-lite) or add an audited entry.",
			liveGeminiDefaultModel, mode, cost)
	}
	if cost <= 0 {
		t.Fatalf("liveGeminiDefaultModel %q priced at %d micro-dollars for %+v — an exact table entry must produce a real cost", liveGeminiDefaultModel, cost, usage)
	}
}
