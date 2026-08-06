package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// contendedStore is the real store with every REQUEST-PATH WRITER overridden to
// fail exactly as beginImmediateBounded does when another process holds SQLite's
// write lock.
//
// The wrapped error is built with the same `fmt.Errorf("%w: %w", ...)` shape
// store.beginImmediate uses (internal/store/store.go), not a bare sentinel, so the
// test exercises errors.Is through a wrap rather than an equality that would pass
// against a `err == store.ErrWriteLockUnavailable` implementation the real store
// would never satisfy.
type contendedStore struct {
	Store // everything else delegates to the real *store.DB
}

func contentionErr() error {
	return fmt.Errorf("%w: %w", store.ErrWriteLockUnavailable, context.DeadlineExceeded)
}

func (contendedStore) UpsertDeveloperAlias(context.Context, string, string) error {
	return contentionErr()
}

func (contendedStore) EraseDeveloper(context.Context, string) (map[string]int64, error) {
	return nil, contentionErr()
}

// The #346 sanctioned /costs override is the third request-path writer: it takes
// the bounded write lock (store.CorrectManualCostEvent) precisely because it is
// reached from an HTTP handler with r.Context().
func (contendedStore) CorrectManualCostEvent(context.Context, store.TokenEvent, string, string) (store.CostCorrection, error) {
	return store.CostCorrection{}, contentionErr()
}

// And the fourth: the NON-override half of the same route (#610). It is the
// busiest write path in the tree and, until #610, the one that answered
// contention with a 500 after a full 5000ms block while the override half beside
// it answered a bounded 503 — one endpoint, one URL, two verdicts on whether a
// lost race for the single write lock is worth retrying.
func (contendedStore) InsertManualCostEvent(context.Context, store.TokenEvent) error {
	return contentionErr()
}

// newContendedHandler builds a Handler over contendedStore. It reuses the ordinary
// test store for every method it does not override, so routing, auth, and the
// aggregation guards behave exactly as in production.
func newContendedHandler(t *testing.T) *Handler {
	t.Helper()
	return newHandlerOverStore(t, func(db *store.DB) Store { return contendedStore{Store: db} })
}

// newHandlerOverStore builds a Handler whose store is `wrap` applied to the
// ordinary test store, so every method the wrapper does not override behaves
// exactly as in production. One construction site: the New(...) argument list is
// long enough that a second copy would drift, which is the same reason the
// watcher's two sessionMetadata literals were collapsed.
func newHandlerOverStore(t *testing.T, wrap func(*store.DB) Store) *Handler {
	t.Helper()
	_, db := newTestHandler(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(wrap(db), quiet, "", nil, "test", RateLimitConfig{})
}

// TestWriteLockContentionIsRetryable503 is the guard for #598's contention
// sentinel actually reaching the operator.
//
// 🔴 THE SENTINEL WAS PRODUCED AND THEN THROWN AWAY. Both of these handlers
// collapsed every non-validation store error into 500 "store error", so a request
// that merely lost a race for the single write lock was indistinguishable from a
// corrupt database. 500 is not retryable; the condition is. An operator reading
// "store error" on a GDPR erasure has been told a compliance action failed when a
// retry one second later would have completed it.
//
// Deleting either `errors.Is(err, store.ErrWriteLockUnavailable)` block in
// handler.go must fail this test.
func TestWriteLockContentionIsRetryable503(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"POST /developer_alias", http.MethodPost, "/api/v1/developer_alias", developerAliasRequest{Alias: "alice-gh", Canonical: "alice"}},
		{"DELETE /developer/{id}", http.MethodDelete, "/api/v1/developer/alice", nil},
		// The #346 override. It matters more than the other two rather than less:
		// this is the only route in the project that REWRITES already-captured
		// money, so a finance client told 500 "store error" has been informed that
		// a correction to the ledger failed for an unknown reason, when the true
		// answer is that another writer held the lock and a retry would land it.
		{"POST /costs (override)", http.MethodPost, "/api/v1/costs",
			overridePayload("contention-001", 0.0206, true, "finance-alice", "Q3 invoice correction")},
		// #610: the plain, non-override half of the SAME route. Both keyed and
		// unkeyed, because the handler reaches InsertManualCostEvent either way
		// and a client that omits idempotency_key must not get a different
		// verdict on retryability than one that supplies it.
		{"POST /costs (plain, keyed)", http.MethodPost, "/api/v1/costs",
			overridePayload("contention-002", 0.0206, false, "", "")},
		{"POST /costs (plain, unkeyed)", http.MethodPost, "/api/v1/costs",
			overridePayload("", 0.0206, false, "", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newContendedHandler(t)

			var body io.Reader
			if c.body != nil {
				buf, err := json.Marshal(c.body)
				if err != nil {
					t.Fatalf("marshal body: %v", err)
				}
				body = bytes.NewReader(buf)
			}
			req := httptest.NewRequest(c.method, c.target, body)
			rec := httptest.NewRecorder()
			mux := http.NewServeMux()
			h.Register(mux)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 — write-lock contention is retryable, and %d tells the operator to go looking for a broken database instead of retrying; body = %s", rec.Code, rec.Code, rec.Body.String())
			}
			ra := rec.Header().Get("Retry-After")
			n, err := strconv.Atoi(ra)
			if err != nil || n <= 0 {
				t.Errorf("Retry-After = %q, want a positive integer seconds value — a 503 with no Retry-After leaves a client guessing whether to retry at all", ra)
			}
			// 🔴 AND AN UPPER BOUND, BECAUSE `n > 0` ALONE PINS ALMOST NOTHING.
			// MEASURED: writeLockUnavailableRetryAfter = 3600 leaves this whole
			// package green under the positive-only check. An hour-long Retry-After
			// for a lock held at most 250ms is a real defect — a well-behaved client
			// honouring the header stops writing for an hour over a race that
			// cleared before the response was serialised. 60s is a generous
			// ceiling that still fails any value in the "operator typed minutes
			// instead of seconds" class.
			if n > 60 {
				t.Errorf("Retry-After = %ds, want <= 60s. The condition this reports is another writer holding the SQLite write lock, capped at %v on the request path — a client that honours a much larger value stops writing for far longer than the contention could possibly last", n, 250*time.Millisecond)
			}

			// The body must name contention. "store error" here would satisfy the
			// status assertion while still sending the operator to the wrong place.
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal error body: %v (body = %s)", err, rec.Body.String())
			}
			if !bytes.Contains([]byte(resp["error"]), []byte("busy")) {
				t.Errorf("error message = %q, want it to name the contention so an operator knows to retry rather than to suspect corruption", resp["error"])
			}
		})
	}
}

// TestValidationErrorsStillAnswer400UnderTheReorderedChecks pins the OTHER half of the
// alias handler's classification: reordering the sentinel ahead of the
// "developer_alias:" prefix check must not sweep genuine validation failures into
// the 503.
//
// It cannot: the store returns validation failures as plain errors.New, wrapping
// nothing, so errors.Is(err, ErrWriteLockUnavailable) is false for every one of
// them. This test is what makes that argument falsifiable — a future change that
// gave a validation error the sentinel (say, by wrapping a shared "alias" error
// that itself wraps it) would turn a permanent 400 into a retry-forever 503.
func TestValidationErrorsStillAnswer400UnderTheReorderedChecks(t *testing.T) {
	// Genuine validation errors must still be 400, not swept into the 503.
	h, db := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias", developerAliasRequest{Alias: "alice", Canonical: "alice"})
	if code != http.StatusBadRequest {
		t.Errorf("self-mapping alias: status = %d, want 400; body = %s", code, body)
	}

	// And the chain guard, which is the other errors.New shape the prefix check
	// classifies. Two distinct validation messages, so the assertion is not
	// resting on one string.
	if err := db.UpsertDeveloperAlias(context.Background(), "bob-gh", "bob"); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	code, body = doRequest(t, h, http.MethodPost, "/api/v1/developer_alias", developerAliasRequest{Alias: "carol", Canonical: "bob-gh"})
	if code != http.StatusBadRequest {
		t.Errorf("chained alias: status = %d, want 400; body = %s", code, body)
	}
}

// prefixedContentionStore returns a write-lock contention error that ALSO carries
// the "developer_alias:" prefix the alias handler uses to classify caller-facing
// validation failures.
//
// 🔴 THIS IS THE MUTANT, KEPT. A reviewer produced it by editing
// UpsertDeveloperAlias's begin-failure return to `fmt.Errorf("developer_alias:
// %w", err)` — an edit that reads as tidying, matches the surrounding style, and
// left the entire tree green while making every contended alias write answer 400
// instead of 503. Rather than rely on nobody making that edit, the handler now
// checks the sentinel FIRST and this store proves the check survives it.
type prefixedContentionStore struct {
	Store
}

func (prefixedContentionStore) UpsertDeveloperAlias(context.Context, string, string) error {
	return fmt.Errorf("developer_alias: %w", contentionErr())
}

// TestContentionOutranksTheValidationPrefix pins the ORDER of the two branches in
// handleUpsertDeveloperAlias: the errors.Is sentinel check must run BEFORE the
// message-prefix check.
//
// A sentinel is an exact match on an identity the producer chose; a prefix is a
// heuristic over text. Putting the heuristic first means any error that happens to
// start with those bytes is classified as a permanent client error, whatever it
// actually is — and a client told 400 does not retry, so a transient lock race
// becomes a hard failure for the caller.
//
// Swapping the two blocks back must fail this test.
func TestContentionOutranksTheValidationPrefix(t *testing.T) {
	h := newHandlerOverStore(t, func(db *store.DB) Store { return prefixedContentionStore{Store: db} })

	// CONTROL: the error really does carry the prefix, so the prefix branch would
	// match it if it ran first. Without this, a fixture whose error had lost the
	// prefix would make the 503 below prove nothing about ordering.
	err := prefixedContentionStore{}.UpsertDeveloperAlias(context.Background(), "a", "b")
	if !strings.HasPrefix(err.Error(), "developer_alias:") {
		t.Fatalf("fixture error %q does not carry the validation prefix — the prefix branch would not match it, so this test would pass no matter which branch ran first", err)
	}
	if !errors.Is(err, store.ErrWriteLockUnavailable) {
		t.Fatalf("fixture error %q does not wrap the contention sentinel — nothing here would be classified as contention under either ordering", err)
	}

	code, body := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias", developerAliasRequest{Alias: "alice-gh", Canonical: "alice"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a contention error that happens to carry the \"developer_alias:\" prefix is being classified as caller validation, so a transient lock race is reported as a permanent 400 the client will never retry; body = %s", code, body)
	}
}

// mismatchingContentionStore returns a write-lock contention error that ALSO
// wraps store.ErrCostCorrectionIdentityMismatch — the sentinel the /costs override
// handler classifies as a permanent 409.
//
// 🔴 THIS IS THE MUTANT, KEPT — the /costs analogue of prefixedContentionStore,
// though NOT its equal in kind, and the difference is worth stating so neither is
// mistaken for the other. prefixedContentionStore reproduces a REACHABLE shape: a
// reviewer actually produced it by editing the store, and the tree stayed green.
// This one pins an INTERFACE-LEVEL ordering invariant instead. That is not a
// weaker justification: handlePostCosts is written against the Store interface, not
// *store.DB, so "the concrete store cannot produce this today" is a property of
// one implementation, not of the contract the handler enforces. Branch order must
// hold for ANY error satisfying both predicates.
//
// It is not a shape the store produces today, and that is the point: it makes the
// BRANCH ORDER in handlePostCosts falsifiable instead of merely intended. A single
// error satisfying both errors.Is checks is classified entirely by which check
// runs first, so moving the identity-mismatch block above the contention block —
// an edit that reads as grouping the two #346 sentinels together — flips every
// contended override from a retryable 503 to a 409 that says the client's key
// belongs to someone else. That is worse than the alias case it mirrors: the 409
// does not merely fail to retry, it tells a finance operator their key collided
// with another developer's row, sending them to investigate a money-attribution
// incident that never happened.
type mismatchingContentionStore struct {
	Store
}

func (mismatchingContentionStore) CorrectManualCostEvent(context.Context, store.TokenEvent, string, string) (store.CostCorrection, error) {
	return store.CostCorrection{}, fmt.Errorf("%w: %w", store.ErrCostCorrectionIdentityMismatch, contentionErr())
}

// TestCostOverrideContentionOutranksTheIdentityMismatch pins the ORDER of the
// classification branches in handlePostCosts's override path: the
// store.ErrWriteLockUnavailable check must run BEFORE every other check in that
// chain, including the ErrCostCorrectionIdentityMismatch 409 and the trailing 500
// "store error" sink.
//
// The general rule this is an instance of: a TRANSIENT condition must be
// classified before any PERMANENT one, because misclassifying transient-as-
// permanent destroys the retry that would have succeeded, while the reverse merely
// costs a wasted retry. Swapping the two blocks must fail this test.
func TestCostOverrideContentionOutranksTheIdentityMismatch(t *testing.T) {
	h := newHandlerOverStore(t, func(db *store.DB) Store { return mismatchingContentionStore{Store: db} })

	// CONTROL: the fixture error really does satisfy BOTH classifications, so
	// either branch would match it and the ordering is the only thing that can
	// decide the status. Without this, an error that had quietly lost one of the
	// two sentinels would make the assertion below prove nothing about order.
	_, err := mismatchingContentionStore{}.CorrectManualCostEvent(context.Background(), store.TokenEvent{}, "a", "b")
	if !errors.Is(err, store.ErrCostCorrectionIdentityMismatch) {
		t.Fatalf("fixture error %v does not wrap ErrCostCorrectionIdentityMismatch — the 409 branch would not match it, so this test would pass no matter which branch ran first", err)
	}
	if !errors.Is(err, store.ErrWriteLockUnavailable) {
		t.Fatalf("fixture error %v does not wrap the contention sentinel — nothing here would be classified as contention under either ordering", err)
	}

	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("order-001", 0.0206, true, "finance-alice", "Q3 invoice correction"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a write-lock contention error that also carries the identity-mismatch sentinel is being classified as a permanent key collision, so a transient lock race is reported to a finance client as someone else's money; body = %s", code, body)
	}
}

// conflictingContentionStore is the NON-override analogue of
// mismatchingContentionStore (#610): a write-lock contention error that ALSO
// wraps store.ErrCostConflict, the sentinel the plain /costs branch classifies as
// a permanent 409.
//
// The two sentinels in that branch are the whole classification chain, so a
// single error satisfying both is decided entirely by which errors.Is runs first.
// Grouping the "cost" sentinels together — moving the ErrCostConflict block back
// above the contention block, which reads as tidying, since #295's 409 is the
// branch's headline behaviour — turns every contended plain /costs post into a
// 409 telling the client its cost_usd conflicts with an immutable stored figure.
// That is worse than the 500 #610 replaced: a 500 at least reads as "something
// broke", while the 409 names a specific, permanent, and entirely fictional
// cause, and a client that believes it will stop re-posting the row forever.
type conflictingContentionStore struct {
	Store
}

func (conflictingContentionStore) InsertManualCostEvent(context.Context, store.TokenEvent) error {
	return fmt.Errorf("%w: %w", store.ErrCostConflict, contentionErr())
}

// TestCostInsertContentionOutranksTheCostConflict pins the ORDER of the
// classification branches in handlePostCosts's PLAIN path: the
// store.ErrWriteLockUnavailable check must run BEFORE the ErrCostConflict 409 and
// before the trailing 500 "store error" sink.
//
// Same rule as its override-path sibling
// (TestCostOverrideContentionOutranksTheIdentityMismatch): a TRANSIENT condition
// must be classified before any PERMANENT one, because misclassifying
// transient-as-permanent destroys the retry that would have succeeded, while the
// reverse merely costs a wasted retry. Swapping the two blocks must fail this
// test.
func TestCostInsertContentionOutranksTheCostConflict(t *testing.T) {
	h := newHandlerOverStore(t, func(db *store.DB) Store { return conflictingContentionStore{Store: db} })

	// CONTROL: the fixture error really does satisfy BOTH classifications, so
	// either branch would match it and the ordering is the only thing that can
	// decide the status. Without this, an error that had quietly lost one of the
	// two sentinels would make the assertion below prove nothing about order.
	err := conflictingContentionStore{}.InsertManualCostEvent(context.Background(), store.TokenEvent{})
	if !errors.Is(err, store.ErrCostConflict) {
		t.Fatalf("fixture error %v does not wrap ErrCostConflict — the 409 branch would not match it, so this test would pass no matter which branch ran first", err)
	}
	if !errors.Is(err, store.ErrWriteLockUnavailable) {
		t.Fatalf("fixture error %v does not wrap the contention sentinel — nothing here would be classified as contention under either ordering", err)
	}

	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("order-002", 0.0206, false, "", ""))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a write-lock contention error that also carries ErrCostConflict is being classified as a permanent divergent-cost collision, so a transient lock race is reported as an immutable stored cost the client can never re-post; body = %s", code, body)
	}
}
