package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// Tokens shared by the #944 /metrics scope tests. Distinct, equal length.
const (
	scopeTestWrite   = "write-admin-token-of-len-32-aaaa"
	scopeTestRead    = "read-viewer-token-of-len-32-bbbb"
	scopeTestMetrics = "metrics-scrape-token-len-32-cccc"
)

func bearerHeader(tok string) http.Header {
	if tok == "" {
		return http.Header{}
	}
	return http.Header{"Authorization": []string{"Bearer " + tok}}
}

// TestMetrics_ReadTokenRefusedWhenAnonymised pins #944 with NO metrics token
// configured: in team and division mode the read token gets 403 on /metrics,
// with a body naming the --metrics-token flag, and only the write token scrapes
// (fail closed). The read token still opens the other read routes there.
func TestMetrics_ReadTokenRefusedWhenAnonymised(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			h, _ := newTestHandlerWithScopes(t, scopeTestWrite, scopeTestRead)
			h.SetAggregation(mode, scoring.DefaultKAnonymity)

			code, body := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil, bearerHeader(scopeTestRead))
			if code != http.StatusForbidden {
				t.Errorf("read token GET /metrics = %d, want 403; body = %s", code, body)
			}
			if !strings.Contains(string(body), "--metrics-token") {
				t.Errorf("read token 403 body = %s, want it to name --metrics-token", body)
			}
			if code, body := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil, bearerHeader(scopeTestWrite)); code != http.StatusOK {
				t.Errorf("write token GET /metrics = %d, want 200; body = %s", code, body)
			}
			if code, _ := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil, bearerHeader("")); code != http.StatusUnauthorized {
				t.Errorf("no token GET /metrics = %d, want 401", code)
			}
			// Control: the refusal is /metrics-specific, not a broken read token.
			if code, body := doRequestWithHeader(t, h, http.MethodGet, "/api/v1/scores", nil, bearerHeader(scopeTestRead)); code != http.StatusOK {
				t.Errorf("read token GET /api/v1/scores = %d, want 200; body = %s", code, body)
			}
		})
	}
}

// TestMetrics_DeveloperModeReadTokenScrapes pins that developer mode is
// unchanged by #944: its read token already sees per-person figures.
func TestMetrics_DeveloperModeReadTokenScrapes(t *testing.T) {
	h, _ := newTestHandlerWithScopes(t, scopeTestWrite, scopeTestRead)
	h.SetAggregation(scoring.AggregationDeveloper, scoring.DefaultKAnonymity)
	for _, tok := range []string{scopeTestRead, scopeTestWrite} {
		if code, body := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil, bearerHeader(tok)); code != http.StatusOK {
			t.Errorf("developer mode GET /metrics with %.5s… = %d, want 200; body = %s", tok, code, body)
		}
	}
}

// TestMetrics_TokenlessOpenInEveryMode pins the tokenless loopback install: with
// no token configured, /metrics stays open in every mode, like every read route.
func TestMetrics_TokenlessOpenInEveryMode(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision} {
		h, _ := newTestHandlerWithScopes(t, "", "")
		h.SetAggregation(mode, scoring.DefaultKAnonymity)
		if code, body := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil, bearerHeader("")); code != http.StatusOK {
			t.Errorf("%s mode, no tokens configured: GET /metrics = %d, want 200; body = %s", mode, code, body)
		}
	}
}
