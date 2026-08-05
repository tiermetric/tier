package anthropicadmin

// Pins the three claims Probe's doc comment makes (#452), each proven by a
// test that would fail if the claim stopped being true — a comment claim
// with no test is just a hope.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestProbeResult_Classification is the hermetic classification table for the
// SHIPPED Authorized()/Unauthorized() predicates (#452 review Y5).
//
// It exists because nothing else drove them across the interesting statuses:
// cmd/tierd's doctor table test deliberately reimplements the predicate
// locally (see credProbe's doc there), and the wire tests only ever drive 200
// and 401 — so dropping `|| StatusForbidden` from Unauthorized, or widening
// Authorized to `< 400`, compiled and passed the entire suite.
//
// The 403 row is the realistic one, not a boundary curiosity: an operator who
// pastes an OpenAI PROJECT key where an org key is required gets 403. With
// that mutant, doctor stops saying "credential rejected → check
// collectors.*.api_key" and starts saying "unexpected status → check the
// provider's status page", sending them to debug the provider in the one
// scenario #452 was filed to catch.
func TestProbeResult_Classification(t *testing.T) {
	transportErr := errors.New("dial tcp: connection refused")
	cases := []struct {
		name                     string
		res                      ProbeResult
		authorized, unauthorized bool
	}{
		{"200 accepted", ProbeResult{StatusCode: 200}, true, false},
		{"299 upper 2xx boundary still accepted", ProbeResult{StatusCode: 299}, true, false},
		// 302 must be NEITHER: the Client no longer follows redirects
		// (noRedirect), so a 3xx is a real, un-validated origin answer. A
		// `StatusCode < 400` widening of Authorized() flips exactly this row.
		{"302 redirect is not an accepted credential", ProbeResult{StatusCode: 302}, false, false},
		{"401 rejected", ProbeResult{StatusCode: 401}, false, true},
		{"403 rejected (project-scoped key against an org endpoint)", ProbeResult{StatusCode: 403}, false, true},
		{"429 reached but unexpected — neither accepted nor rejected", ProbeResult{StatusCode: 429}, false, false},
		{"500 reached but unexpected — neither accepted nor rejected", ProbeResult{StatusCode: 500}, false, false},
		// A transport error must never classify as a CREDENTIAL verdict in
		// either direction: "the network is down" and "your key is wrong" have
		// opposite remedies, and StatusCode is 0 on this path.
		{"transport error is neither accepted nor rejected", ProbeResult{Err: transportErr}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.Authorized(); got != tc.authorized {
				t.Errorf("Authorized() = %v, want %v", got, tc.authorized)
			}
			if got := tc.res.Unauthorized(); got != tc.unauthorized {
				t.Errorf("Unauthorized() = %v, want %v", got, tc.unauthorized)
			}
		})
	}
}

// TestProbe_NeverFollowsRedirects_KeyStaysAtOrigin is the measured guard for
// #452 review Y1. Deleting the Client's CheckRedirect makes BOTH assertions
// below fail.
//
// Two independent defects, one cause. net/http follows up to 10 redirects by
// default and strips only a fixed sensitive-header set across a domain change
// (Authorization/WWW-Authenticate/Cookie/Cookie2). `x-api-key` is NOT on that
// list, so:
//
//   - the Admin key was delivered VERBATIM to whatever host the origin named
//     in a Location header; and
//   - ProbeResult.StatusCode became the redirect TARGET's status, so a probe
//     whose origin answered 302 was reported as `credential accepted (HTTP
//     200)` for a key the provider never validated.
//
// The cross-HOSTNAME hop matters: httptest binds 127.0.0.1, and Go compares
// hostnames (not ports) for the strip decision, so a 127.0.0.1→127.0.0.1
// redirect would be same-domain and prove nothing about the cross-domain
// case. Rewriting the target's host to "localhost" makes it a genuine domain
// change while still resolving to loopback.
func TestProbe_NeverFollowsRedirects_KeyStaysAtOrigin(t *testing.T) {
	var targetHits int32
	var sawKey atomic.Value
	sawKey.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		sawKey.Store(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusOK) // the status a follow would wrongly grade
	}))
	defer target.Close()
	crossHost := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	if crossHost == target.URL {
		t.Fatalf("test setup: expected an httptest URL on 127.0.0.1, got %q", target.URL)
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, crossHost+"/redirected", http.StatusFound)
	}))
	defer origin.Close()

	res := NewClient(ClientConfig{APIKey: testAdminKey, BaseURL: origin.URL}).Probe(context.Background())

	if n := atomic.LoadInt32(&targetHits); n != 0 {
		t.Errorf("redirect target was contacted %d time(s) — the probe must stop at the origin", n)
	}
	if got := sawKey.Load().(string); got != "" {
		t.Errorf("the Admin key traveled to the redirect target (%q) — x-api-key is NOT in net/http's cross-domain strip list, so redirects must not be followed", got)
	}
	if res.StatusCode != http.StatusFound {
		t.Errorf("StatusCode = %d, want %d — Probe must grade the ORIGIN's status, not a redirect target's", res.StatusCode, http.StatusFound)
	}
	if res.Authorized() {
		t.Error("Authorized() = true for an origin that answered 302 — doctor would print \"credential accepted\" for a key the provider never validated")
	}
	if res.Err != nil {
		t.Errorf("a completed 302 is a normal status result, not a transport Err: %v", res.Err)
	}
}

// TestProbe_ExactlyOneRequestNoRetries pins "makes exactly ONE authenticated
// GET… with NO retries". A mutant that routed Probe through doGet/fetchCost
// (the retrying path) would send up to 1+defaultMaxRetries requests against a
// 500 — this counts them and fails if it is ever more than one.
func TestProbe_ExactlyOneRequestNoRetries(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusInternalServerError) // retryable status, if Probe retried
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{APIKey: testAdminKey, BaseURL: srv.URL})
	res := client.Probe(context.Background())

	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("server saw %d requests, want exactly 1 — Probe must never retry", got)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}
	if res.Err != nil {
		t.Errorf("a completed 500 response is not a transport Err, got %v", res.Err)
	}
}

// TestProbe_KeyNeverInErrors pins "the Admin key travels only in the x-api-key
// header and never enters the returned ProbeResult.Err" — the Probe-specific
// counterpart to TestPoll_KeyNeverInErrors, which covers doGet/fetchCost only.
func TestProbe_KeyNeverInErrors(t *testing.T) {
	// A dead server: guarantees res.Err is a real transport error whose
	// message embeds the request URL — exactly the shape that could leak a
	// query-carried secret (Probe's key is header-only, so this proves the
	// negative).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening

	client := NewClient(ClientConfig{APIKey: testAdminKey, BaseURL: url})
	res := client.Probe(context.Background())

	if res.Err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
	if strings.Contains(res.Err.Error(), testAdminKey) {
		t.Errorf("admin key leaked into ProbeResult.Err: %q", res.Err.Error())
	}
}

// TestProbe_EmptyBaseURLOverrideKeepsProductionDefault pins the claim
// cmd/tierd/doctor.go's checkCollectorConfigsAt makes about its own
// BaseURL-override seam: "an empty override keeps each Client's own
// production default". This is the one guard that a future edit hard-coding
// a URL into that wrapper (or swapping the anthropic/openai override
// arguments) would trip — nothing else in the tree asserts it.
func TestProbe_EmptyBaseURLOverrideKeepsProductionDefault(t *testing.T) {
	client := NewClient(ClientConfig{APIKey: testAdminKey, BaseURL: ""})
	if client.baseURL != defaultBaseURL {
		t.Fatalf("NewClient with an empty BaseURL override = %q, want the production default %q", client.baseURL, defaultBaseURL)
	}
}
