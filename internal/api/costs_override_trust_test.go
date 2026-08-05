package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// costMicroByKey reads one stored row's cost by idempotency_key. The
// developer-aggregating storedCostMicro cannot distinguish two rows owned by the
// same developer, which these tests deliberately create.
func costMicroByKey(t *testing.T, db *store.DB, key string) int64 {
	t.Helper()
	// Bounds in UTC: the write paths under test stamp ts as time.Now().UTC(), and
	// a local-zone bound would be compared against a UTC-formatted column.
	events, _, err := db.ListTokenEvents(context.Background(),
		time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	for _, e := range events {
		if e.IdempotencyKey == key {
			return e.CostMicro
		}
	}
	t.Fatalf("no stored row with idempotency_key %q (rows: %+v)", key, events)
	return 0
}

// TestPostCosts_Override_FidelityIsPartOfIdentity closes the gap between the
// identity check and the comment above it. That comment says it compares "every
// column the client's request itself claims" — and fidelity IS claimed by the
// client here (the handler validates it as a daily/estimated enum, exactly as it
// validates source), yet it was excluded. The measured consequence: a request
// stating the correct developer/issue_id/model but the WRONG fidelity got
// 200 {"corrected":true} instead of 409.
//
// Both directions are asserted. The mismatch is refused and mutates nothing; the
// SAME request with the correct fidelity succeeds — so the refusal is provably
// about fidelity, not a blanket rejection of the key.
func TestPostCosts_Override_FidelityIsPartOfIdentity(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "override-fidelity-001"

	owner := overridePayload(key, 0.0105, false, "", "")
	owner["fidelity"] = "daily"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", owner); code != http.StatusCreated {
		t.Fatalf("first POST (fidelity=daily): status = %d, want 201; body = %s", code, body)
	}

	// Wrong fidelity, everything else correct.
	wrong := overridePayload(key, 0.0206, true, "finance-mallory", "wrong fidelity")
	wrong["fidelity"] = "estimated"
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", wrong)
	if code != http.StatusConflict {
		t.Fatalf("override with a MISMATCHED fidelity: status = %d, want 409; body = %s", code, body)
	}
	if got := costMicroByKey(t, db, key); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged", got)
	}
	if n, err := db.CostCorrectionAuditCount(context.Background()); err != nil {
		t.Fatalf("CostCorrectionAuditCount: %v", err)
	} else if n != 0 {
		t.Errorf("cost_correction_audit row count = %d, want 0", n)
	}

	// An OMITTED fidelity is not a free pass either: the handler defaults it to
	// "estimated", which is still the wrong claim against a "daily" row.
	omitted := overridePayload(key, 0.0206, true, "finance-mallory", "omitted fidelity")
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", omitted); code != http.StatusConflict {
		t.Fatalf("override with an OMITTED fidelity: status = %d, want 409; body = %s", code, body)
	}

	// Positive control: correct fidelity, same divergent cost, succeeds.
	correct := overridePayload(key, 0.0206, true, "finance-alice", "legitimate correction")
	correct["fidelity"] = "daily"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", correct); code != http.StatusOK {
		t.Fatalf("override with the CORRECT fidelity: status = %d, want 200; body = %s", code, body)
	}
	if got := costMicroByKey(t, db, key); got != 20_600 {
		t.Errorf("stored cost_micro = %d, want 20_600", got)
	}
}

// TestPostCosts_Override_CannotReachCapturedSpend pins a control that is real
// but was entirely undocumented and untested, and would therefore have vanished
// silently: the /costs handler FORCES source="api" on every request, and the
// identity check compares source — so the sanctioned override can only ever
// touch a row whose STORED source is "api" -- the manual-import lane.
// Automatically captured spend (every non-api source: jsonl, proxy,
// codex-rollout, copilot-api, the org pollers) is STRUCTURALLY unreachable from
// this endpoint, not merely unlikely to be hit.
//
// TWO refusal arms, because they fail for different reasons and only the second
// isolates source:
//
//   - A realtime-captured row (jsonl) is refused on source AND fidelity — the
//     handler rejects fidelity="realtime" outright (#82), so an override request
//     can never even claim it. Deleting the source term from the identity tuple
//     would NOT make this arm pass.
//   - An org-poller row (anthropic-admin) writes fidelity="daily", which IS a
//     legal /costs claim. Matching it leaves source as the ONLY divergence, so
//     that arm alone pins the source comparison at the HTTP layer.
//
// The positive control makes it more than a tautology: the same override shape
// against an api-sourced row DOES correct it.
func TestPostCosts_Override_CannotReachCapturedSpend(t *testing.T) {
	h, db := newTestHandler(t)
	const capturedKey = "jsonl-captured-001"

	// A collector-captured row, exactly as the JSONL watcher would write it.
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		OutputTok:      500,
		CostMicro:      10_500,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: capturedKey,
		Timestamp:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed captured row: %v", err)
	}

	// An override naming that key, with every client-variable field correct
	// EXCEPT source — which the caller cannot set to "jsonl" at all, because the
	// handler rejects any source but "api".
	attempt := overridePayload(capturedKey, 0.0206, true, "finance-mallory", "reaching for captured spend")
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", attempt)
	if code != http.StatusConflict {
		t.Fatalf("override against a jsonl-captured row: status = %d, want 409; body = %s", code, body)
	}
	if got := costMicroByKey(t, db, capturedKey); got != 10_500 {
		t.Errorf("captured row cost_micro = %d, want 10_500 UNCHANGED — the override must not reach captured spend", got)
	}

	// And the caller cannot talk its way around it by naming the real source.
	claimSource := overridePayload(capturedKey, 0.0206, true, "finance-mallory", "claiming jsonl")
	claimSource["source"] = "jsonl"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", claimSource); code != http.StatusBadRequest {
		t.Fatalf("override claiming source=jsonl: status = %d, want 400; body = %s", code, body)
	}
	if got := costMicroByKey(t, db, capturedKey); got != 10_500 {
		t.Errorf("captured row cost_micro = %d, want 10_500 UNCHANGED", got)
	}

	// The arm that ISOLATES source. An org poller writes fidelity="daily"
	// (collector.FidelityDaily), and "daily" is a legal claim on /costs — so a
	// request can match every client-variable column except source. This is the
	// only arm that fails if `storedSource != e.Source` is deleted from the
	// identity tuple.
	const pollerKey = "anthropic-admin-daily-001"
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		OutputTok:      500,
		CostMicro:      10_500,
		Source:         "anthropic-admin",
		Fidelity:       "daily",
		IdempotencyKey: pollerKey,
		Timestamp:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed poller row: %v", err)
	}
	poller := overridePayload(pollerKey, 0.0206, true, "finance-mallory", "reaching for poller spend")
	poller["fidelity"] = "daily" // matches the stored row; ONLY source diverges
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", poller); code != http.StatusConflict {
		t.Fatalf("override against an anthropic-admin row with matching fidelity: status = %d, want 409; body = %s", code, body)
	}
	if got := costMicroByKey(t, db, pollerKey); got != 10_500 {
		t.Errorf("poller row cost_micro = %d, want 10_500 UNCHANGED — org-poller spend must be unreachable too", got)
	}

	// CONTROL ARM: the same override shape against an api-sourced row works.
	const manualKey = "manual-control-001"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload(manualKey, 0.0105, false, "", "")); code != http.StatusCreated {
		t.Fatalf("seed manual row: status = %d, want 201; body = %s", code, body)
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload(manualKey, 0.0206, true, "finance-alice", "legitimate")); code != http.StatusOK {
		t.Fatalf("override against a manual row: status = %d, want 200; body = %s", code, body)
	}
	if got := costMicroByKey(t, db, manualKey); got != 20_600 {
		t.Errorf("manual row cost_micro = %d, want 20_600 (the control arm must actually correct)", got)
	}
}

// TestPostCosts_Override_ActorNotForgeable is the #321 barrier applied to the
// one client-controlled string sink on this path that skipped it. override_actor
// is unvalidated free text written straight into a structured log; every other
// client-controlled string sink in this package routes through logSafeStr, and
// an exception is the kind nobody notices until it matters.
func TestPostCosts_Override_ActorNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedActor := "evilactor\ntime=2026-08-04T00:00:00Z " + forgedMarker

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(db, logger, "", nil, "test", RateLimitConfig{})

	const key = "override-logsafe-001"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload(key, 0.0105, false, "", "")); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload(key, 0.0206, true, forgedActor, "forged actor")); code != http.StatusOK {
		t.Fatalf("override POST: status = %d, want 200; body = %s", code, body)
	}

	logs := buf.String()
	if !strings.Contains(logs, "sanctioned cost correction applied") {
		t.Fatalf("expected the correction INFO record; sink not reached:\n%s", logs)
	}
	// Secondary guard, for a hypothetical non-slog concatenation sink: slog's
	// TextHandler already %q-escapes an embedded newline, so this loop alone
	// would pass even with logSafeStr removed. The STRIP PROOF below is the
	// load-bearing assertion. Keep both.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("override_actor forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: CR/LF stripped and the value %q-quoted, so
	// the halves join as `actor="\"evilactortime=`. Unwrapping logSafeStr makes
	// this fail.
	if !strings.Contains(logs, `actor="\"evilactortime=`) {
		t.Errorf("override_actor not stripped+rendered via logSafeStr (expected `actor=\"\\\"evilactortime=...`):\n%s", logs)
	}
}

// correctFaultStore embeds a real *store.DB so every other method behaves
// normally, and fails CorrectManualCostEvent with a forged error. It models a
// store-layer failure whose message wraps caller-supplied text — the sink at
// handler.go's "correct manual cost" ERROR, which is unreachable through a
// healthy store.
type correctFaultStore struct {
	*store.DB
	err error
}

func (s correctFaultStore) CorrectManualCostEvent(context.Context, store.TokenEvent, string, string) (store.CostCorrection, error) {
	return store.CostCorrection{}, s.err
}

// insertFaultStore is the correctFaultStore twin for the NON-override path.
// It exists because the two /costs store-error sinks are separate lines: wrapping
// one and testing only that one is how the other stayed bare through a whole
// review cycle.
type insertFaultStore struct {
	*store.DB
	err error
}

func (s insertFaultStore) InsertManualCostEvent(context.Context, store.TokenEvent) error {
	return s.err
}

// TestPostCosts_Override_StoreErrorNotForgeable covers the other new sink on
// this path. A store error can wrap caller-supplied text, so it routes through
// logSafeErr like every other error sink in this package.
func TestPostCosts_Override_StoreErrorNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedErr := errors.New("correct failed: actor=evilactor\ntime=2026-08-04T00:00:00Z " + forgedMarker)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(correctFaultStore{DB: db, err: forgedErr}, logger, "", nil, "test", RateLimitConfig{})

	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("override-errsafe-001", 0.0206, true, "finance-alice", "reason"))
	if code != http.StatusInternalServerError {
		t.Fatalf("override POST against a failing store: status = %d, want 500; body = %s", code, body)
	}

	// A 500 must not have left a half-applied correction behind. The fault store
	// never reaches the real one, so this asserts the handler did not write
	// around it.
	if n, err := db.CostCorrectionAuditCount(context.Background()); err != nil {
		t.Fatalf("CostCorrectionAuditCount: %v", err)
	} else if n != 0 {
		t.Errorf("cost_correction_audit row count = %d after a failed correction, want 0", n)
	}

	logs := buf.String()
	if !strings.Contains(logs, "correct manual cost") {
		t.Fatalf("expected the store-error ERROR record; sink not reached:\n%s", logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("store error forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// STRIP PROOF: CR/LF stripped and %q-quoted, so the halves join as
	// `err="\"correct failed: actor=evilactortime=`.
	if !strings.Contains(logs, `evilactortime=`) {
		t.Errorf("store error not stripped+rendered via logSafeErr (expected the CR/LF stripped so the halves join):\n%s", logs)
	}
}

// TestPostCosts_NonOverrideStoreErrorNotForgeable covers the sibling sink that a
// review caught bare: the plain (non-override) /costs store-error log. It is the
// SAME request body reaching the SAME class of error as the override sink, 40
// lines apart in the same function, and it was the counter-example that made the
// override sink's "every other sink in this package" comment self-refuting.
//
// Its own existence is the point. The override test cannot reach this line, so
// without this one the wrap here is unpinned — and an unpinned wrap is exactly
// what silently reverts.
func TestPostCosts_NonOverrideStoreErrorNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedErr := errors.New("insert failed: developer=evildev\ntime=2026-08-04T00:00:00Z " + forgedMarker)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(insertFaultStore{DB: db, err: forgedErr}, logger, "", nil, "test", RateLimitConfig{})

	// override=false — the path that reaches the sibling sink.
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("plain-errsafe-001", 0.0105, false, "", ""))
	if code != http.StatusInternalServerError {
		t.Fatalf("plain POST against a failing store: status = %d, want 500; body = %s", code, body)
	}

	logs := buf.String()
	if !strings.Contains(logs, "insert manual cost") {
		t.Fatalf("expected the non-override store-error ERROR record; sink not reached:\n%s", logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("store error forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// STRIP PROOF: CR/LF stripped so the halves join. Unwrapping logSafeErr here
	// makes this fail.
	if !strings.Contains(logs, `evildevtime=`) {
		t.Errorf("store error not stripped+rendered via logSafeErr (expected the CR/LF stripped so the halves join):\n%s", logs)
	}
}
