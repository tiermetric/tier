package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
)

const scopeTestIngest = "ingest-upload-token-1144-dddddddd"

func TestIngestToken_RealMuxAndProxy(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision} {
		for _, readOnly := range []bool{false, true} {
			t.Run(mode.String()+map[bool]string{false: "/full", true: "/read-only"}[readOnly], func(t *testing.T) {
				h := newTestHandlerWithMetricsScope(t, mode)
				h.SetIngestToken(scopeTestIngest)
				mux := http.NewServeMux()
				rec := &recordingMux{}
				if readOnly {
					h.RegisterReadOnly(rec)
					h.RegisterReadOnly(mux)
				} else {
					h.Register(rec)
					h.Register(mux)
				}
				seenEvents, seenMetrics := false, false
				request := func(p, tok string) *httptest.ResponseRecorder {
					method, path, _ := strings.Cut(p, " ")
					body := []byte("{}")
					if p == "POST /api/v1/events" {
						body, _ = json.Marshal([]map[string]any{validEvent("scope-1144")})
					}
					req := httptest.NewRequest(method, wildcardRE.ReplaceAllString(path, "x"), strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					if tok != "" {
						req.Header.Set("Authorization", "Bearer "+tok)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					return w
				}
				for _, pattern := range rec.patterns {
					if pattern == "GET /metrics" {
						seenMetrics = true
					}
					got := request(pattern, scopeTestIngest)
					switch {
					case pattern == "POST /api/v1/events":
						seenEvents = true
						if got.Code != http.StatusCreated {
							t.Errorf("ingest %s=%d: %s", pattern, got.Code, got.Body)
						}
						if admin := request(pattern, scopeTestWrite); admin.Code != http.StatusCreated {
							t.Errorf("admin events=%d: %s", admin.Code, admin.Body)
						}
					case openRoutes[pattern]:
						anon := request(pattern, "")
						if got.Code != anon.Code || got.Body.String() != anon.Body.String() {
							t.Errorf("ingest grants access beyond anonymous on %s", pattern)
						}
					default:
						if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "ingest token is not authorized") {
							t.Errorf("ingest %s=%d, want 403: %s", pattern, got.Code, got.Body)
						}
					}
				}
				if seenEvents == readOnly || !seenMetrics {
					t.Fatalf("incomplete route walk: events=%v metrics=%v readOnly=%v", seenEvents, seenMetrics, readOnly)
				}
				req := httptest.NewRequest("POST", "/v1/messages", nil)
				req.Header.Set(ProxyTokenHeader, scopeTestIngest)
				w := httptest.NewRecorder()
				h.ProxyAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("ingest reached upstream") })).ServeHTTP(w, req)
				if w.Code != http.StatusForbidden {
					t.Errorf("proxy=%d, want 403", w.Code)
				}
			})
		}
	}
}

func TestIngestToken_ClassifyAndLockout(t *testing.T) {
	h, _ := newTestHandlerWithTokenAndLimit(t, scopeTestWrite, RateLimitConfig{MaxFailures: 2, Window: time.Minute, Lockout: time.Minute})
	h.SetReadToken(scopeTestRead)
	h.SetMetricsToken(scopeTestMetrics)
	h.SetIngestToken(scopeTestIngest)
	for _, tc := range []struct {
		token string
		want  authScope
	}{{scopeTestIngest, scopeIngest}, {scopeTestWrite, scopeWrite}, {scopeTestRead, scopeRead}, {scopeTestMetrics, scopeMetrics}, {scopeTestIngest[:10], scopeNone}, {scopeTestIngest + "x", scopeNone}, {"", scopeNone}} {
		if got := h.classify([]byte(tc.token)); got != tc.want {
			t.Errorf("classify=%v want %v", got, tc.want)
		}
	}
	mux := http.NewServeMux()
	h.Register(mux)
	request := func(path, tok string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader("[]"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	// Wrong scope neither adds failures nor clears a previous failure.
	for i := 0; i < 3; i++ {
		if got := request("/api/v1/costs", scopeTestIngest); got != 403 {
			t.Fatalf("wrong scope=%d", got)
		}
	}
	if got := request("/api/v1/events", "wrong"); got != 401 {
		t.Fatal(got)
	}
	if got := request("/api/v1/costs", scopeTestIngest); got != 403 {
		t.Fatal(got)
	}
	if got := request("/api/v1/events", "wrong"); got != 401 {
		t.Fatal(got)
	}
	if got := request("/api/v1/events", scopeTestIngest); got != 429 {
		t.Fatalf("valid ingest after failed auth=%d, want 429", got)
	}
	h.SetIngestToken("")
	if got := h.classify([]byte(scopeTestIngest)); got != scopeNone {
		t.Fatal("unarmed ingest matched")
	}
}

func TestIngestToken_SuccessPreservesAdminLockout(t *testing.T) {
	h, _ := newTestHandlerWithTokenAndLimit(t, scopeTestWrite, DefaultRateLimitConfig())
	h.SetIngestToken(scopeTestIngest)
	mux := http.NewServeMux()
	h.Register(mux)
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.114:1144"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 9; i++ {
		if w := request("GET", "/api/v1/scores", "admin-guess", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: status=%d", i+1, w.Code)
		}
	}
	body, err := json.Marshal([]map[string]any{validEvent("lockout-1144")})
	if err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "/api/v1/events", scopeTestIngest, string(body)); w.Code != http.StatusCreated {
		t.Fatalf("ingest status=%d: %s", w.Code, w.Body)
	}
	if w := request("GET", "/api/v1/scores", "admin-guess", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("tenth guess status=%d", w.Code)
	}
	if w := request("GET", "/api/v1/scores", "admin-guess", ""); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("eleventh guess status=%d, want 429 with Retry-After: %s", w.Code, w.Body)
	}
}
