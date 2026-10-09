//go:build integration

// #269 / #459 task 1: a live E2E of the OpenAI-compatible reverse-proxy path
// against a REAL open-weights inference endpoint. Every existing proxy test
// (internal/proxy/proxy_test.go) exercises the OpenAI-compatible parser
// against an httptest.Server WE wrote — it can never catch a real provider's
// response shape drifting from what parseOpenAI expects. #269 was filed
// because no run has ever hit a real Ollama/DGX endpoint at all (no daemon,
// no pulled model, no fleet-provisioned credential).
//
// Per batch-9's ruling this is NOT credential-gated — Ollama needs no
// account — it is ENDPOINT-gated: skip loud unless a local Ollama daemon is
// actually reachable AND has at least one model pulled, checked live, never
// assumed. When it IS reachable this test sends one real completion through
// TIER's own proxy.New/ServeHTTP and asserts a real token_event landed —
// this is a genuine live capture proof, not a proxy for one.
package integration

import (
	"context"
	"encoding/json"
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

// liveOllamaEndpointFromEnv resolves which Ollama daemon to probe.
// TIER_LIVE_OLLAMA_HOST overrides the default local address — set it to point
// at a DGX Spark or remote Ollama host per #269's original ask.
func liveOllamaEndpointFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("TIER_LIVE_OLLAMA_HOST")); v != "" {
		return v
	}
	return "http://127.0.0.1:11434"
}

// ollamaTagsResponse is the subset of GET /api/tags this file needs.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// probeOllamaModels lists the models a live Ollama daemon at endpoint has
// pulled. A non-nil error means the daemon itself is unreachable (no
// process, wrong port, dead network) — DISTINCT from a reachable daemon with
// zero models (nil error, empty slice), because the remedy differs (`ollama
// serve` vs `ollama pull <model>`).
func probeOllamaModels(ctx context.Context, endpoint string) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/api/tags: HTTP %d", endpoint, resp.StatusCode)
	}
	var tags ollamaTagsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decode /api/tags: %w", err)
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// resolveLiveOllama is the SINGLE skip-decision function every gated test in
// this file calls — TestResolveLiveOllama_SkipGuard exercises it directly
// with an injected listModels, so a mutation to its decision logic (not just
// to a call site) is what the control arm below proves against.
//
// ok is true only when the endpoint is reachable (err == nil) AND at least
// one usable model is resolved: envModel itself if set and present, else the
// first pulled model. A reachable daemon with zero pulled models, or a
// requested-but-absent envModel, is correctly "not ok" — this is exactly the
// "check, don't assume" distinction #269 was filed over.
func resolveLiveOllama(ctx context.Context, endpoint string, listModels func(context.Context, string) ([]string, error), envModel string) (model string, ok bool) {
	models, err := listModels(ctx, endpoint)
	if err != nil || len(models) == 0 {
		return "", false
	}
	if envModel == "" {
		return models[0], true
	}
	for _, m := range models {
		if m == envModel {
			return envModel, true
		}
	}
	return "", false
}

// liveOllamaSkipMessage builds the loud, specific skip reason: which
// endpoint was tried and exactly what to run to make the test pass.
func liveOllamaSkipMessage(endpoint string, listErr error) string {
	if listErr != nil {
		return fmt.Sprintf(
			"no Ollama daemon reachable at %s (%v) — skipping live Ollama E2E (#269/#459 task 1). "+
				"To run: install Ollama (https://ollama.com), start it with `ollama serve`, pull a small model "+
				"e.g. `ollama pull qwen2.5:0.5b` (or point TIER_LIVE_OLLAMA_HOST at a reachable DGX Spark/remote "+
				"Ollama endpoint per #269), then: go test -tags integration ./internal/integration/... -run TestLive_Ollama -v",
			endpoint, listErr)
	}
	return fmt.Sprintf(
		"Ollama daemon at %s is reachable but has ZERO pulled models (or TIER_LIVE_OLLAMA_MODEL names one that "+
			"is not pulled) — skipping live Ollama E2E. To run: `ollama pull qwen2.5:0.5b` (or set "+
			"TIER_LIVE_OLLAMA_MODEL to a model you have pulled), then: "+
			"go test -tags integration ./internal/integration/... -run TestLive_Ollama -v",
		endpoint)
}

// TestResolveLiveOllama_SkipGuard is the CONTROL ARM for this file's skip
// logic. It drives resolveLiveOllama with an injected listModels function
// under every branch (unreachable, reachable-but-empty, requested-model
// absent, requested-model present, no-preference-picks-first) — entirely
// without a real Ollama daemon — so a mutation to resolveLiveOllama's
// decision logic makes this fail regardless of what happens to be running
// on the test machine.
func TestResolveLiveOllama_SkipGuard(t *testing.T) {
	unreachable := func(context.Context, string) ([]string, error) { return nil, fmt.Errorf("connection refused") }
	empty := func(context.Context, string) ([]string, error) { return nil, nil }
	twoModels := func(context.Context, string) ([]string, error) { return []string{"qwen2.5:0.5b", "llama3.2:1b"}, nil }

	t.Run("unreachable daemon must skip", func(t *testing.T) {
		if _, ok := resolveLiveOllama(context.Background(), "http://unused", unreachable, ""); ok {
			t.Fatal("ok=true for an unreachable daemon — the live test would attempt a real POST against nothing listening")
		}
	})
	t.Run("reachable but zero models must skip", func(t *testing.T) {
		if _, ok := resolveLiveOllama(context.Background(), "http://unused", empty, ""); ok {
			t.Fatal("ok=true with zero pulled models — there is nothing to send a completion request to")
		}
	})
	t.Run("no preference picks the first pulled model", func(t *testing.T) {
		model, ok := resolveLiveOllama(context.Background(), "http://unused", twoModels, "")
		if !ok || model != "qwen2.5:0.5b" {
			t.Fatalf("model=%q ok=%v, want (qwen2.5:0.5b, true)", model, ok)
		}
	})
	t.Run("requested model present is honored", func(t *testing.T) {
		model, ok := resolveLiveOllama(context.Background(), "http://unused", twoModels, "llama3.2:1b")
		if !ok || model != "llama3.2:1b" {
			t.Fatalf("model=%q ok=%v, want (llama3.2:1b, true)", model, ok)
		}
	})
	t.Run("requested model absent must skip, not silently substitute", func(t *testing.T) {
		if model, ok := resolveLiveOllama(context.Background(), "http://unused", twoModels, "not-pulled:1b"); ok {
			t.Fatalf("ok=true for an unpulled requested model (got model=%q) — must skip loud rather than silently run a different model than requested", model)
		}
	})
}

// TestLive_OllamaOpenAICompatProxy_RealCompletion sends ONE real completion
// through TIER's actual reverse proxy (proxy.New + ServeHTTP — the same code
// `tierd serve --openai-target` mounts, not a stand-in for it) to a live
// local Ollama daemon, and asserts a real token_event lands with the right
// host tag and nonzero usage — the local half of #269, and #459 task 1.
func TestLive_OllamaOpenAICompatProxy_RealCompletion(t *testing.T) {
	ctx := context.Background()
	endpoint := liveOllamaEndpointFromEnv()
	envModel := strings.TrimSpace(os.Getenv("TIER_LIVE_OLLAMA_MODEL"))

	models, listErr := probeOllamaModels(ctx, endpoint)
	model, ok := resolveLiveOllama(ctx, endpoint, func(context.Context, string) ([]string, error) { return models, listErr }, envModel)
	if !ok {
		t.Skip(liveOllamaSkipMessage(endpoint, listErr))
	}
	t.Logf("live Ollama daemon found at %s with model %q — exercising REAL completion (not a mock)", endpoint, model)

	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "ollama-live.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	p := proxy.New(target, proxy.ProviderOpenAI, collector.SourceProxy, ingester.Store(db), nil, nil,
		slog.New(slog.NewTextHandler(quietWriter{}, nil)))
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Reply with exactly one word: OK."}],"stream":false}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxySrv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "live-e2e-dev")
	req.Header.Set("X-Tier-Issue", "live-e2e-issue")
	req.Header.Set("X-Tier-Repo", "tiermetric/tier")

	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST through the real proxy to the real Ollama daemon: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live completion returned HTTP %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"choices"`) {
		t.Fatalf("response forwarded to the client does not look like a chat completion: %s", body)
	}
	t.Logf("real completion response (truncated): %.200s", body)

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
		t.Fatalf("got %d token_event(s) after ONE real completion through the proxy, want exactly 1 (0 = capture silently dropped it; >1 = double-capture)", len(events))
	}
	ev := events[0]
	if ev.Model != model {
		t.Errorf("captured Model = %q, want %q", ev.Model, model)
	}
	if ev.InputTok <= 0 || ev.OutputTok <= 0 {
		t.Errorf("captured usage InputTok=%d OutputTok=%d — a real model call must report nonzero usage on both sides", ev.InputTok, ev.OutputTok)
	}
	if ev.Host != target.Hostname() {
		t.Errorf("captured Host = %q, want %q (#300 host-qualified pricing depends on this)", ev.Host, target.Hostname())
	}
	if ev.Source != collector.SourceProxy {
		t.Errorf("captured Source = %q, want %q", ev.Source, collector.SourceProxy)
	}
	if ev.Developer != "live-e2e-dev" || ev.IssueID != "live-e2e-issue" {
		t.Errorf("attribution not threaded through: developer=%q issue=%q", ev.Developer, ev.IssueID)
	}
	// PRICING PROVENANCE (#452 review Y3). The assertion here used to be
	// `ev.CostMicro < 0`, which NO mutant could break: parseOpenAI clamps
	// negative wire counts before pricing (internal/proxy/proxy.go, #121) and
	// every table rate is non-negative, so the cost is unconditionally >= 0 and
	// the check could only ever pass. Meanwhile the interesting property of
	// this run went unasserted entirely: the model here is deliberately absent
	// from prices.yaml, so this is the tree's one LIVE exercise of the
	// guess-pricing fallback.
	//
	// A model with no audited rate MUST be flagged self_hosted_amortized, never
	// presented as a canonical per_token $/M — that flag is what #267/#294's
	// reprice guard and the unknown-model spend share key off, so a silent
	// per_token here would launder an estimate into an audited-looking figure.
	if ev.BillingMode != store.BillingSelfHostedAmortized {
		t.Errorf("captured BillingMode = %q, want %q — %q has no audited rate in prices.yaml, so its cost is an ESTIMATE and must be flagged as one rather than published as a per-token rate",
			ev.BillingMode, store.BillingSelfHostedAmortized, ev.Model)
	}
	// Provenance, not just the mode: an event with no price-table version
	// cannot be re-priced or audited later (#233).
	if ev.PriceVersion == 0 {
		t.Error("captured PriceVersion = 0 — the row records no price-table provenance, so a later reprice cannot tell what it was costed under")
	}
	t.Logf("REAL capture verified: model=%s host=%s input_tok=%d output_tok=%d cost_micro=%d billing_mode=%s price_version=%d",
		ev.Model, ev.Host, ev.InputTok, ev.OutputTok, ev.CostMicro, ev.BillingMode, ev.PriceVersion)
}
