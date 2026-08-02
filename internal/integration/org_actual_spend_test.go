//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// orgActualSpendWire mirrors the wire shape of GET /api/v1/org_actual_spend
// (#42). Declared here (not imported from internal/api, whose types are
// unexported) so field drift between the API's JSON and this consumer is the
// kind of cross-package break this tier exists to catch.
type orgActualSpendWire struct {
	Since string `json:"since"`
	Orgs  []struct {
		Org           string  `json:"org"`
		Period        string  `json:"period"`
		ActualPaidUSD float64 `json:"actual_paid_usd"`
		Entries       int     `json:"entries"`
	} `json:"orgs"`
}

// TestPipeline_OrgActualSpendReadBack is the full #42 wire: finance POSTs an
// org invoice AND a credit memo over HTTP, then reads them back via
// GET /api/v1/org_actual_spend and sees the NET with entries=2. Exercises the
// symmetric write→read pair (#23 POST, #42 GET) plus the #24 accumulation model
// end-to-end through the real store and REST layer.
func TestPipeline_OrgActualSpendReadBack(t *testing.T) {
	srv := newServer(t)
	period := time.Now().UTC().Format("2006-01")

	// $500 invoice, then a −$100 credit memo for the same (org, period).
	postJSON(t, srv.URL+"/api/v1/org_actual_spend", map[string]any{
		"org": "acme", "period": period, "actual_paid_usd": 500.0,
	}, http.StatusCreated)
	postJSON(t, srv.URL+"/api/v1/org_actual_spend", map[string]any{
		"org": "acme", "period": period, "actual_paid_usd": -100.0,
	}, http.StatusCreated)

	// Read back, filtered to the org.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/org_actual_spend?org=acme", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /org_actual_spend: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /org_actual_spend: status %d, want 200; body=%s", resp.StatusCode, b)
	}
	var got orgActualSpendWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Orgs) != 1 {
		t.Fatalf("got %d orgs, want 1; %+v", len(got.Orgs), got)
	}
	e := got.Orgs[0]
	if e.Org != "acme" || e.Period != period {
		t.Errorf("entry key = %s/%s, want acme/%s", e.Org, e.Period, period)
	}
	if e.ActualPaidUSD != 400.0 {
		t.Errorf("actual_paid_usd = %v, want 400 (net of the −$100 credit memo)", e.ActualPaidUSD)
	}
	if e.Entries != 2 {
		t.Errorf("entries = %d, want 2 (invoice + credit memo)", e.Entries)
	}
}

// TestPipeline_OrgActualSpendReadBackRequiresAuth confirms the GET is gated by
// the same bearer token as the POST half when a token is configured (#59).
func TestPipeline_OrgActualSpendReadBackRequiresAuth(t *testing.T) {
	srv := newServer(t)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/org_actual_spend", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// No Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /org_actual_spend: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a bearer token", resp.StatusCode)
	}
}
