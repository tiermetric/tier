package openaiusage

// Pins the three claims Probe's doc comment makes (#452), each proven by a
// test that would fail if the claim stopped being true — a comment claim
// with no test is just a hope. Structural twin of anthropicadmin's
// client_probe_test.go.

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
// SHIPPED Authorized()/Unauthorized() predicates (#452 review Y5) — twin of
// anthropicadmin's, and deliberately duplicated rather than shared: the two
// ProbeResult types are separate on purpose (see this package's doc on the
// premature-shared-abstraction hazard), so a mutation to ONE provider's
// predicate must fail that provider's own table.
//
// The 403 row is the realistic one for THIS provider specifically: OpenAI
// answers 403 to a project-scoped key used against an org endpoint. Dropping
// `|| StatusForbidden` turns doctor's actionable "credential rejected → check
// collectors.openai_usage.api_key" into "unexpected status → check the
// provider's status page".
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

// TestProbe_NeverFollowsRedirects_GradesOriginStatus is the OpenAI half of the
// #452 review Y1 guard. The measured asymmetry is worth stating precisely: this
// provider's Bearer token IS stripped by net/http across a domain change (it is
// on the Authorization strip list), so the secret-leak half was Anthropic-only.
// The MISGRADING half was not — following a redirect made ProbeResult.StatusCode
// the redirect target's status here too, so a 302-answering origin was reported
// as `credential accepted (HTTP 200)`.
//
// The header assertion is kept anyway, as a regression guard rather than a
// current defect: it holds today only because of net/http's fixed allowlist,
// so a future switch to a non-Authorization header here would inherit
// Anthropic's leak silently. This pins the property to THIS package.
func TestProbe_NeverFollowsRedirects_GradesOriginStatus(t *testing.T) {
	var targetHits int32
	var sawAuth atomic.Value
	sawAuth.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		sawAuth.Store(r.Header.Get("Authorization"))
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

	res := NewClient(ClientConfig{APIKey: testAPIKey, BaseURL: origin.URL}).Probe(context.Background())

	if n := atomic.LoadInt32(&targetHits); n != 0 {
		t.Errorf("redirect target was contacted %d time(s) — the probe must stop at the origin", n)
	}
	if got := sawAuth.Load().(string); strings.Contains(got, testAPIKey) {
		t.Errorf("the org key traveled to the redirect target (%q)", got)
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

	client := NewClient(ClientConfig{APIKey: testAPIKey, BaseURL: srv.URL})
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

// TestProbe_KeyNeverInErrors pins "the org key travels only in the
// Authorization header and never enters the returned ProbeResult.Err" — the
// Probe-specific counterpart to TestPoll_KeyNeverInErrors, which covers
// doGet/fetchCost only.
func TestProbe_KeyNeverInErrors(t *testing.T) {
	// A dead server: guarantees res.Err is a real transport error whose
	// message embeds the request URL — exactly the shape that could leak a
	// query-carried secret (Probe's key is header-only, so this proves the
	// negative).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening

	client := NewClient(ClientConfig{APIKey: testAPIKey, BaseURL: url})
	res := client.Probe(context.Background())

	if res.Err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
	if strings.Contains(res.Err.Error(), testAPIKey) {
		t.Errorf("org key leaked into ProbeResult.Err: %q", res.Err.Error())
	}
}

// TestProbe_EmptyBaseURLOverrideKeepsProductionDefault pins the claim
// cmd/tierd/doctor.go's checkCollectorConfigsAt makes about its own
// BaseURL-override seam: "an empty override keeps each Client's own
// production default". This is the one guard that a future edit hard-coding
// a URL into that wrapper (or swapping the anthropic/openai override
// arguments) would trip — nothing else in the tree asserts it.
func TestProbe_EmptyBaseURLOverrideKeepsProductionDefault(t *testing.T) {
	client := NewClient(ClientConfig{APIKey: testAPIKey, BaseURL: ""})
	if client.baseURL != defaultBaseURL {
		t.Fatalf("NewClient with an empty BaseURL override = %q, want the production default %q", client.baseURL, defaultBaseURL)
	}
}
