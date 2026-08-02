package api

// Tests for POST /api/v1/outcomes (#188) — the provider-neutral outcome ingest
// any CI (GitLab/Bitbucket/Gitea) posts a merged-PR/MR to. The contract these
// pin: same bearer gate as the other writes, merge_commit_sha dedup so a replay
// never double-inserts, weight/quality parity with the GitHub webhook (via the
// shared store.ResolveWeight), and source=api-outcome provenance on every row.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// recentMergedAt returns an RFC3339 timestamp inside the default /scores window
// so a posted outcome is visible without a ?since override.
func recentMergedAt() string {
	return time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
}

// validOutcome returns one well-formed /outcomes body. sha differentiates rows
// (dedup is on merge_commit_sha); overrides mutate individual fields.
func validOutcome(sha string, overrides map[string]any) map[string]any {
	o := map[string]any{
		"developer":        "alice",
		"issue_id":         "issue-42",
		"pr_number":        7,
		"merge_commit_sha": sha,
		"merged_at":        recentMergedAt(),
		"additions":        40,
		"deletions":        10,
		"changed_files":    3,
	}
	for k, v := range overrides {
		if v == nil {
			delete(o, k)
			continue
		}
		o[k] = v
	}
	return o
}

func postOutcome(t *testing.T, h *Handler, body map[string]any) (int, []byte) {
	t.Helper()
	return doRequest(t, h, http.MethodPost, "/api/v1/outcomes", body)
}

// outcomeBySHA reads a stored outcome straight from the store — the provenance
// / weight assertion surface for these tests.
func outcomeBySHA(t *testing.T, db *store.DB, sha string) store.Outcome {
	t.Helper()
	o, found, err := db.OutcomeByMergeCommit(context.Background(), sha)
	if err != nil {
		t.Fatalf("OutcomeByMergeCommit: %v", err)
	}
	if !found {
		t.Fatalf("no outcome recorded for sha %q", sha)
	}
	return o
}

// TestPostOutcome_SHANotForgeable is a security regression guard (#321, CodeQL
// go/log-injection; sibling of TestLogSafeErr and the webhook forge tests).
//
// merge_commit_sha is required and length-capped but NEVER charset-validated
// (validateOutcomeRequest), so a client fully controls its bytes and can embed a
// newline. The duplicate-replay Debug sink logs the SHA; logged raw, that newline
// forges a second, standalone log record. logsafe.Str must strip it so the forged
// record never stands alone while the diagnostic survives.
func TestPostOutcome_SHANotForgeable(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(db, logger, "", nil, "test", RateLimitConfig{})

	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedSHA := "evilsha\ntime=2026-07-12T00:00:00Z " + forgedMarker
	body := validOutcome(forgedSHA, nil)

	// First POST records the row; the second is a merge_commit_sha replay, which is
	// the path that reaches the SHA-logging Debug sink.
	if code, _ := postOutcome(t, h, body); code != http.StatusCreated {
		t.Fatalf("first POST status = %d, want 201", code)
	}
	if code, _ := postOutcome(t, h, body); code != http.StatusOK {
		t.Fatalf("replay POST status = %d, want 200 (duplicate)", code)
	}

	logs := buf.String()
	if !strings.Contains(logs, "outcome already recorded for merge commit") {
		t.Fatalf("replay diagnostic was not logged; lost:\n%s", logs)
	}
	// The forged record must never begin its own line.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("merge_commit_sha forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: CR/LF stripped and value %q-quoted, so the halves
	// join as `sha="\"evilshatime=...`. Removing logsafe.Str makes this fail.
	if !strings.Contains(logs, `sha="\"evilshatime=`) {
		t.Errorf("sha not stripped+rendered via logsafe.Str (expected `sha=\"\\\"evilshatime=...`):\n%s", logs)
	}
}

// TestPostOutcome_AuthRequired pins /outcomes behind the same bearer gate as
// every other write endpoint.
func TestPostOutcome_AuthRequired(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithToken(t, token)
	body := validOutcome("sha-auth-1", nil)

	code, _ := postOutcome(t, h, body)
	if code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", code)
	}

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	code, respBody := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/outcomes", body, header)
	if code != http.StatusCreated {
		t.Errorf("with token: status = %d, want 201; body = %s", code, respBody)
	}
}

// TestPostOutcome_HappyPath records a heuristic-weighted outcome and asserts the
// stored row: source=api-outcome (provenance), weight from the shared heuristic,
// quality defaulted to 1.0, and visibility in /api/v1/scores.
func TestPostOutcome_HappyPath(t *testing.T) {
	h, db := newTestHandler(t)
	// Seed a cost row for the same developer so the developer ranks in /scores.
	seedCosts(t, db, "alice", "issue-42", 0.05)

	code, body := postOutcome(t, h, validOutcome("sha-happy-1", nil))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	var resp outcomeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body = %s", err, body)
	}
	if resp.Status != "created" {
		t.Errorf("status = %q, want created", resp.Status)
	}
	// The 201 response echoes the resolved weight/source so a CI integrator can
	// confirm parity without a follow-up read.
	if resp.WeightSource != store.WeightSourceHeuristic {
		t.Errorf("response weight_source = %q, want %q", resp.WeightSource, store.WeightSourceHeuristic)
	}
	if resp.Weight != store.GitHeuristic(50, 3) {
		t.Errorf("response weight = %v, want %v", resp.Weight, store.GitHeuristic(50, 3))
	}

	o := outcomeBySHA(t, db, "sha-happy-1")
	if o.Source != store.OutcomeSourceAPI {
		t.Errorf("source = %q, want %q (provenance)", o.Source, store.OutcomeSourceAPI)
	}
	// additions+deletions = 50, changed_files = 3 → effort 50+30 = 80 → 3.0.
	wantWeight := store.GitHeuristic(50, 3)
	if o.Weight != wantWeight {
		t.Errorf("weight = %v, want %v (shared heuristic)", o.Weight, wantWeight)
	}
	if o.WeightSource != store.WeightSourceHeuristic {
		t.Errorf("weight_source = %q, want %q", o.WeightSource, store.WeightSourceHeuristic)
	}
	if o.Quality != 1.0 {
		t.Errorf("quality = %v, want 1.0 (default)", o.Quality)
	}
	if o.PRNumber != 7 {
		t.Errorf("pr_number = %d, want 7", o.PRNumber)
	}

	// End-to-end: the outcome must produce weighted points in /scores.
	scode, sbody := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if scode != http.StatusOK {
		t.Fatalf("GET /scores status = %d; body = %s", scode, sbody)
	}
	var scores scoresResponse
	if err := json.Unmarshal(sbody, &scores); err != nil {
		t.Fatalf("unmarshal scores: %v; body = %s", err, sbody)
	}
	var found bool
	for _, d := range scores.Developers {
		if d.Developer == "alice" {
			found = true
			if d.WeightedPoints != wantWeight*1.0 {
				t.Errorf("alice weighted_points = %v, want %v", d.WeightedPoints, wantWeight)
			}
		}
	}
	if !found {
		t.Errorf("alice not present in /scores; body = %s", sbody)
	}
}

// TestPostOutcome_DedupReplay is the replay-safety contract: re-posting the same
// merge_commit_sha returns 200 duplicate and inserts no second row, so a CI that
// retries a delivery cannot double-count points.
func TestPostOutcome_DedupReplay(t *testing.T) {
	h, db := newTestHandler(t)
	body := validOutcome("sha-dedup-1", nil)

	code, resp := postOutcome(t, h, body)
	if code != http.StatusCreated {
		t.Fatalf("first post: status = %d, want 201; body = %s", code, resp)
	}

	code, resp = postOutcome(t, h, body)
	if code != http.StatusOK {
		t.Fatalf("replay: status = %d, want 200; body = %s", code, resp)
	}
	var r outcomeResponse
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatalf("unmarshal replay response: %v", err)
	}
	if r.Status != "duplicate" {
		t.Errorf("replay status = %q, want duplicate", r.Status)
	}

	// Exactly one row survived the replay. AllOutcomesSince doesn't project
	// merge_commit_sha, but this is a fresh DB with a single posted outcome, so
	// the total row count is the dedup assertion surface.
	all, err := db.AllOutcomesSince(context.Background(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AllOutcomesSince: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("outcome rows = %d, want 1 (replay must not double-insert)", len(all))
	}
}

// TestPostOutcome_ExplicitLabelWeight pins weight parity with the webhook's
// label path: a client-supplied label-equivalent weight is stored verbatim and
// recorded as source 'label', not run through the heuristic.
func TestPostOutcome_ExplicitLabelWeight(t *testing.T) {
	h, db := newTestHandler(t)

	// Diff stats would heuristic to 3.0; the explicit weight 5.0 must win.
	code, body := postOutcome(t, h, validOutcome("sha-label-1", map[string]any{"weight": 5.0}))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	o := outcomeBySHA(t, db, "sha-label-1")
	if o.Weight != 5.0 {
		t.Errorf("weight = %v, want 5.0 (explicit label-equivalent)", o.Weight)
	}
	if o.WeightSource != store.WeightSourceLabel {
		t.Errorf("weight_source = %q, want %q", o.WeightSource, store.WeightSourceLabel)
	}
}

// TestPostOutcome_WeightBoundaryAccepted pins the accepted edge of the weight
// cap: exactly MaxOutcomeWeight (8.0) is valid, guarding against a `>=` vs `>`
// off-by-one (the reject side is covered by the validation table's "weight too
// large").
func TestPostOutcome_WeightBoundaryAccepted(t *testing.T) {
	h, db := newTestHandler(t)
	code, body := postOutcome(t, h, validOutcome("sha-max-1", map[string]any{"weight": store.MaxOutcomeWeight}))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	if o := outcomeBySHA(t, db, "sha-max-1"); o.Weight != store.MaxOutcomeWeight {
		t.Errorf("weight = %v, want %v", o.Weight, store.MaxOutcomeWeight)
	}
}

// TestPostOutcome_QualityOverride confirms a supplied quality is stored AND
// feeds weighted points end-to-end (weight*quality) in /scores, while absence
// defaults to 1.0 (covered in HappyPath).
func TestPostOutcome_QualityOverride(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-42", 0.05)

	code, body := postOutcome(t, h, validOutcome("sha-q-1", map[string]any{"quality": 0.5}))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	o := outcomeBySHA(t, db, "sha-q-1")
	if o.Quality != 0.5 {
		t.Errorf("quality = %v, want 0.5", o.Quality)
	}

	// weighted_points must reflect weight * quality (3.0 * 0.5 = 1.5).
	_, sbody := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	var scores scoresResponse
	if err := json.Unmarshal(sbody, &scores); err != nil {
		t.Fatalf("unmarshal scores: %v; body = %s", err, sbody)
	}
	want := store.GitHeuristic(50, 3) * 0.5
	var found bool
	for _, d := range scores.Developers {
		if d.Developer == "alice" {
			found = true
			if d.WeightedPoints != want {
				t.Errorf("weighted_points = %v, want %v (weight*quality)", d.WeightedPoints, want)
			}
		}
	}
	if !found {
		t.Errorf("alice not present in /scores; body = %s", sbody)
	}
}

// TestPostOutcome_ConcurrentSameSHA verifies the insert-first dedup under a real
// race (modelled on the webhook's concurrent-replay test): N simultaneous
// identical posts land exactly one row, and exactly one reports "created" (201)
// while the rest report "duplicate" (200) — the response label never lies.
func TestPostOutcome_ConcurrentSameSHA(t *testing.T) {
	h, db := newTestHandler(t)
	body := validOutcome("sha-race-1", nil)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	statuses := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			code, resp := postOutcome(t, h, body)
			codes[i] = code
			var r outcomeResponse
			if err := json.Unmarshal(resp, &r); err == nil {
				statuses[i] = r.Status
			}
		}(i)
	}
	wg.Wait()

	created, duplicate := 0, 0
	for i := 0; i < n; i++ {
		switch codes[i] {
		case http.StatusCreated:
			created++
			if statuses[i] != "created" {
				t.Errorf("201 with status %q, want created", statuses[i])
			}
		case http.StatusOK:
			duplicate++
			if statuses[i] != "duplicate" {
				t.Errorf("200 with status %q, want duplicate", statuses[i])
			}
		default:
			t.Errorf("unexpected status code %d", codes[i])
		}
	}
	if created != 1 {
		t.Errorf("created responses = %d, want exactly 1", created)
	}
	if duplicate != n-1 {
		t.Errorf("duplicate responses = %d, want %d", duplicate, n-1)
	}

	all, err := db.AllOutcomesSince(context.Background(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AllOutcomesSince: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("rows = %d, want exactly 1 under race", len(all))
	}
}

// TestPostOutcome_Validation rejects malformed requests with a 400 and inserts
// nothing. Table-driven over the required-field, bounds, and body-shape rules.
func TestPostOutcome_Validation(t *testing.T) {
	cases := []struct {
		name     string
		body     map[string]any
		wantWord string
	}{
		{"missing developer", validOutcome("v1", map[string]any{"developer": nil}), "developer"},
		{"empty developer", validOutcome("v2", map[string]any{"developer": ""}), "developer"},
		{"missing issue_id", validOutcome("v3", map[string]any{"issue_id": nil}), "issue_id"},
		{"over-long issue_id", validOutcome("v3b", map[string]any{"issue_id": strings.Repeat("x", maxIdentifierLen+1)}), "identifier fields"},
		{"missing merge_commit_sha", validOutcome("", map[string]any{"merge_commit_sha": nil}), "merge_commit_sha"},
		{"empty merge_commit_sha", validOutcome("", map[string]any{"merge_commit_sha": ""}), "merge_commit_sha"},
		{"over-long merge_commit_sha", validOutcome("", map[string]any{"merge_commit_sha": strings.Repeat("a", maxIdentifierLen+1)}), "merge_commit_sha must be"},
		{"missing merged_at", validOutcome("v4", map[string]any{"merged_at": nil}), "merged_at"},
		{"bad merged_at", validOutcome("v5", map[string]any{"merged_at": "not-a-time"}), "merged_at"},
		{"merged_at year out of range", validOutcome("v6", map[string]any{"merged_at": "1999-01-02T00:00:00Z"}), "out of range"},
		{"pr_number zero", validOutcome("v7", map[string]any{"pr_number": 0}), "pr_number"},
		{"pr_number negative", validOutcome("v8", map[string]any{"pr_number": -3}), "pr_number"},
		{"weight zero", validOutcome("v9", map[string]any{"weight": 0.0}), "weight"},
		{"weight negative", validOutcome("v10", map[string]any{"weight": -1.0}), "weight"},
		{"weight too large", validOutcome("v11", map[string]any{"weight": 9.0}), "weight"},
		{"quality above one", validOutcome("v12", map[string]any{"quality": 1.5}), "quality"},
		{"quality negative", validOutcome("v13", map[string]any{"quality": -0.1}), "quality"},
		{"negative additions", validOutcome("v14", map[string]any{"additions": -1}), "additions"},
		{"negative deletions", validOutcome("v14b", map[string]any{"deletions": -1}), "additions"},
		{"negative changed_files", validOutcome("v14c", map[string]any{"changed_files": -1}), "additions"},
		{"over-long developer", validOutcome("v15", map[string]any{"developer": strings.Repeat("x", maxIdentifierLen+1)}), "identifier fields"},
		{"unknown field", validOutcome("v16", map[string]any{"bogus": "x"}), "invalid JSON"},
		{"invalid work_type", validOutcome("v17", map[string]any{"work_type": "bogus"}), "work_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			code, body := postOutcome(t, h, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			if tc.wantWord != "" && !strings.Contains(string(body), tc.wantWord) {
				t.Errorf("error should mention %q; body = %s", tc.wantWord, body)
			}
			all, err := db.AllOutcomesSince(context.Background(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("AllOutcomesSince: %v", err)
			}
			if len(all) != 0 {
				t.Errorf("invalid request must insert nothing; got %d rows", len(all))
			}
		})
	}
}

// TestPostOutcome_WorkType pins the #187 work_type ingest: an explicit valid
// category stores that type with source 'api', and an absent field falls back to
// feature/default (identical to a labelless webhook PR). Invalid values are covered
// by TestPostOutcome_Validation.
func TestPostOutcome_WorkType(t *testing.T) {
	t.Run("explicit research -> api", func(t *testing.T) {
		h, db := newTestHandler(t)
		code, body := postOutcome(t, h, validOutcome("sha-wt-1", map[string]any{"work_type": "research"}))
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", code, body)
		}
		var resp outcomeResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.WorkType != store.WorkTypeResearch {
			t.Errorf("response work_type = %q, want research", resp.WorkType)
		}
		o := outcomeBySHA(t, db, "sha-wt-1")
		if o.WorkType != store.WorkTypeResearch || o.WorkTypeSource != store.WorkTypeSourceAPI {
			t.Errorf("stored work_type = (%q,%q), want (research,api)", o.WorkType, o.WorkTypeSource)
		}
	})
	t.Run("absent -> feature/default", func(t *testing.T) {
		h, db := newTestHandler(t)
		code, body := postOutcome(t, h, validOutcome("sha-wt-2", nil))
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", code, body)
		}
		o := outcomeBySHA(t, db, "sha-wt-2")
		if o.WorkType != store.WorkTypeFeature || o.WorkTypeSource != store.WorkTypeSourceDefault {
			t.Errorf("stored work_type = (%q,%q), want (feature,default)", o.WorkType, o.WorkTypeSource)
		}
	})
}

// TestPostOutcome_TrailingJSON rejects a body with a second concatenated object
// so a smuggled outcome cannot ride in unnoticed (mirrors handlePostCosts).
func TestPostOutcome_TrailingJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	first, err := json.Marshal(validOutcome("sha-trail-1", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(validOutcome("sha-trail-2", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := append(first, second...)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/outcomes", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.Bytes())
	}
	if !strings.Contains(rec.Body.String(), "exactly one JSON object") {
		t.Errorf("error should name the trailing-object rule; body = %s", rec.Body.String())
	}
}
