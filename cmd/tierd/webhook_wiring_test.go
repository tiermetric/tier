package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
	"github.com/tiermetric/tier/internal/webhook"
)

// TestBuildWebhookOptions_GeneratedPathsReachHandler guards the #240 plumbing:
// outcomes.generated_paths was parsed and drift-guarded but never wired into the
// webhook handler, so the config key had zero runtime effect. This drives a real
// signed push through the handler built by buildWebhookOptions — the same seam
// runServe uses — and asserts the configured value actually changes behaviour.
//
// The probe commit touches ONLY a generated file (foo.pb.go), is attributable (a
// closing keyword plus an author login) and is not a revert, so whether it earns
// a degraded push outcome depends ENTIRELY on the handler's generated-paths list:
//   - config absent (nil) → the handler keeps its built-in defaults, which include
//     "*.pb.go", so the commit is all-generated and excluded (no outcome).
//   - a custom list that omits "*.pb.go" → the commit is no longer all-generated,
//     so it is captured (one outcome).
//
// With the pre-fix wiring (WithGeneratedPaths never applied) the custom case would
// ALSO keep the defaults and exclude the commit, so this test fails — exactly the
// regression it exists to catch.
func TestBuildWebhookOptions_GeneratedPathsReachHandler(t *testing.T) {
	const secret = "test-webhook-secret"
	body := generatedFilePushBody(t)

	cases := []struct {
		name           string
		generatedPaths []string
		wantCaptured   bool
	}{
		{"config absent keeps built-in defaults that exclude generated churn", nil, false},
		{"custom list omitting pb.go lets the commit through", []string{"vendor/"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingStore{}
			opts := buildWebhookOptions(true /* pushCapture */, nil /* unattributed */, tc.generatedPaths, nil /* sizeLabels */)
			h := webhook.New(st, secret, nil, opts...)

			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-GitHub-Delivery", "wiring-test-"+tc.name)
			req.Header.Set("X-Hub-Signature-256", signBody(secret, body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code/100 != 2 {
				t.Fatalf("ServeHTTP status = %d, want 2xx; body=%q", rec.Code, rec.Body.String())
			}
			if got := st.pushOutcomeCount(); (got > 0) != tc.wantCaptured {
				t.Fatalf("push outcomes recorded = %d, wantCaptured = %v", got, tc.wantCaptured)
			}
		})
	}
}

// TestBuildWebhookOptions_SizeLabelsReachHandler guards the #244 plumbing against
// the #240 regression class: outcomes.size_labels is parsed, drift-guarded, and
// validated, but until buildWebhookOptions applies WithSizeLabels the config key has
// zero runtime effect. This drives a real signed merged-PR delivery through the
// handler built by buildWebhookOptions — the same seam runServe uses — and asserts
// the configured map actually changes the weight the PR earns end-to-end.
//
// The probe PR carries ONE label, "size: m" (a space-separated org convention the
// built-in defaultSizeLabels does NOT recognise), and a tiny diff (1 addition, 1
// deletion, 0 files) whose git-heuristic weight is 0.5. So the weight the PR earns
// depends ENTIRELY on whether the operator's size_labels table reached the handler:
//   - config absent (nil) → the handler keeps its built-in defaults, "size: m" is
//     unrecognised → the heuristic wins → weight 0.5, source 'git-heuristic'.
//   - a custom table mapping "size: m" → 8 → the label wins → weight 8, source
//     'label'.
//
// With the pre-fix wiring (WithSizeLabels never applied) the custom case would ALSO
// keep the defaults and score 0.5/'git-heuristic', so this test fails — exactly the
// regression it exists to catch.
func TestBuildWebhookOptions_SizeLabelsReachHandler(t *testing.T) {
	const secret = "test-webhook-secret"
	body := mergedPRBody(t, "size: m", 1 /* additions */, 1 /* deletions */, 0 /* changedFiles */)

	cases := []struct {
		name       string
		sizeLabels map[string]float64
		wantWeight float64
		wantSource string
	}{
		{"config absent keeps built-in defaults so the custom label is unrecognised and the heuristic wins", nil, 0.5, store.WeightSourceHeuristic},
		{"custom table maps the org's label so the configured weight wins", map[string]float64{"size: m": 8}, 8, store.WeightSourceLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingStore{}
			opts := buildWebhookOptions(false /* pushCapture */, nil /* unattributed */, nil /* generatedPaths */, tc.sizeLabels)
			h := webhook.New(st, secret, nil, opts...)

			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("X-GitHub-Delivery", "size-wiring-test-"+tc.name)
			req.Header.Set("X-Hub-Signature-256", signBody(secret, body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code/100 != 2 {
				t.Fatalf("ServeHTTP status = %d, want 2xx; body=%q", rec.Code, rec.Body.String())
			}
			o, ok := st.lastPROutcome()
			if !ok {
				t.Fatalf("no PR outcome recorded; the merged-PR path did not reach InsertOutcome")
			}
			if o.Weight != tc.wantWeight || o.WeightSource != tc.wantSource {
				t.Fatalf("outcome weight/source = (%g, %q), want (%g, %q)",
					o.Weight, o.WeightSource, tc.wantWeight, tc.wantSource)
			}
		})
	}
}

// TestSizeLabelsConfigRoundTripReachesHandler closes the last link in the #244
// wiring that TestBuildWebhookOptions_SizeLabelsReachHandler leaves outside its
// boundary: that guard proves buildWebhookOptions' ARGUMENT reaches the handler, but
// the glue runServe adds — sizeLabelsCfg = cfg.Outcomes.SizeLabels, then passed as
// the 4th argument — is a separate hop. A #240-class regression could drop that
// assignment (or pass nil) and the boundary test would still pass. This test loads a
// real YAML through config.Load and threads cfg.Outcomes.SizeLabels into
// buildWebhookOptions EXACTLY as runServe does, so the guard runs end-to-end from the
// config FILE to the recorded outcome — the full config→handler contract, live.
func TestSizeLabelsConfigRoundTripReachesHandler(t *testing.T) {
	const secret = "test-webhook-secret"
	path := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(path, []byte("outcomes:\n  size_labels:\n    \"size: m\": 8\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Mirror runServe's wiring exactly: the config field flows into the 4th argument.
	sizeLabelsCfg := cfg.Outcomes.SizeLabels
	st := &recordingStore{}
	opts := buildWebhookOptions(false /* pushCapture */, nil /* unattributed */, nil /* generatedPaths */, sizeLabelsCfg)
	h := webhook.New(st, secret, nil, opts...)

	body := mergedPRBody(t, "size: m", 1 /* additions */, 1 /* deletions */, 0 /* changedFiles */)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "size-config-roundtrip")
	req.Header.Set("X-Hub-Signature-256", signBody(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code/100 != 2 {
		t.Fatalf("ServeHTTP status = %d, want 2xx; body=%q", rec.Code, rec.Body.String())
	}
	o, ok := st.lastPROutcome()
	if !ok {
		t.Fatal("no PR outcome recorded; config-file size_labels did not reach the handler")
	}
	if o.Weight != 8 || o.WeightSource != store.WeightSourceLabel {
		t.Fatalf("outcome weight/source = (%g, %q), want (8, %q) — config-file size_labels not live end-to-end",
			o.Weight, o.WeightSource, store.WeightSourceLabel)
	}
}

// mergedPRBody builds a pull_request webhook body for a single merged PR that
// carries one size label and the given diff stats, attributed to issue #244 via
// the head branch so handlePR records an outcome.
func mergedPRBody(t *testing.T, label string, additions, deletions, changedFiles int) []byte {
	t.Helper()
	payload := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           244,
			"merged":           true,
			"merge_commit_sha": "244abc00000000000000000000000000000abc44",
			"head":             map[string]any{"ref": "feature/244-configurable-size-labels"},
			"user":             map[string]any{"login": "alice"},
			"labels":           []map[string]any{{"name": label}},
			"additions":        additions,
			"deletions":        deletions,
			"changed_files":    changedFiles,
		},
		"repository": map[string]any{"full_name": "acme/widgets"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal PR payload: %v", err)
	}
	return body
}

// generatedFilePushBody builds a push webhook body for a single attributable,
// non-revert direct commit to the default branch whose only changed file is a
// generated protobuf.
func generatedFilePushBody(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{
		"ref": "refs/heads/main",
		"repository": map[string]any{
			"default_branch": "main",
			"full_name":      "acme/widgets",
		},
		"commits": []map[string]any{
			{
				"id":        "0123456789abcdef0123456789abcdef01234567",
				"message":   "chore: regenerate protobuf (closes #240)",
				"timestamp": time.Now().UTC(),
				"author":    map[string]any{"username": "alice"},
				"modified":  []string{"api/foo.pb.go"},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal push payload: %v", err)
	}
	return body
}

// signBody computes the X-Hub-Signature-256 header value for body.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// recordingStore is a minimal webhook.Store that counts push-captured outcomes.
// The non-revert push path only reaches OutcomeByMergeCommit (dedup) and
// UpsertPushOutcome; the remaining methods are inert no-ops that satisfy the
// interface.
type recordingStore struct {
	mu           sync.Mutex
	pushOutcomes int
	// prOutcomes captures the outcomes the merged-PR path records via InsertOutcome
	// so a wiring guard can assert the (weight, source) a webhook-delivered PR earns.
	prOutcomes []store.Outcome
}

func (s *recordingStore) pushOutcomeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushOutcomes
}

// lastPROutcome returns the most recent outcome recorded via InsertOutcome and
// whether any was recorded.
func (s *recordingStore) lastPROutcome() (store.Outcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.prOutcomes) == 0 {
		return store.Outcome{}, false
	}
	return s.prOutcomes[len(s.prOutcomes)-1], true
}

func (s *recordingStore) UpsertPushOutcome(_ context.Context, _ store.Outcome, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushOutcomes++
	return true, nil
}

func (s *recordingStore) OutcomeByMergeCommit(context.Context, string) (store.Outcome, bool, error) {
	return store.Outcome{}, false, nil
}

func (s *recordingStore) InsertOutcome(_ context.Context, o store.Outcome) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prOutcomes = append(s.prOutcomes, o)
	return true, nil
}

func (s *recordingStore) LatestOutcomeByIssue(context.Context, string, string) (store.Outcome, bool, error) {
	return store.Outcome{}, false, nil
}

func (s *recordingStore) AppendQualityEvent(context.Context, store.QualityEvent) (bool, error) {
	return true, nil
}

func (s *recordingStore) QualityEventsForOutcome(context.Context, int64) ([]store.QualityEvent, error) {
	return nil, nil
}

func (s *recordingStore) UpdateQualityForOutcome(context.Context, int64, float64, string, string) error {
	return nil
}

func (s *recordingStore) InsertWebhookPayload(context.Context, string, string, []byte) error {
	return nil
}
