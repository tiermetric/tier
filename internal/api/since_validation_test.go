package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestGetScores_SinceValidation pins the `?since=` contract of
// GET /api/v1/scores over the real registered mux (was: parseSince at 33.3%,
// the accepted YYYY / YYYY-MM / YYYY-MM-DD forms and the 400-on-garbage
// contract pinned by nothing). It drives the endpoint through doRequest (which
// builds the production mux via h.Register) so the whole request path --
// routing, parseSince, and the handler's status/echo -- is exercised, not
// parseSince in isolation.
//
// Guards three regressions that would otherwise merge green: a dropped date
// layout (a valid form starts 400ing), a garbage value silently defaulting to
// the 90-day window instead of erroring, and a garbage value turning into a
// 500 instead of a 400.
func TestGetScores_SinceValidation(t *testing.T) {
	h, _ := newTestHandler(t)

	t.Run("valid forms -> 200 with echoed start-of-period date", func(t *testing.T) {
		cases := []struct {
			since    string
			wantEcho string // handleScores formats since as YYYY-MM-DD
		}{
			{"2026-06-15", "2026-06-15"}, // day: verbatim
			{"2026-06", "2026-06-01"},    // month: start of month
			{"2026", "2026-01-01"},       // year: start of year
		}
		for _, tc := range cases {
			code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since="+tc.since, nil)
			if code != http.StatusOK {
				t.Errorf("since=%q: status = %d, want 200 (body: %s)", tc.since, code, body)
				continue
			}
			var resp struct {
				Since string `json:"since"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Errorf("since=%q: unmarshal response: %v", tc.since, err)
				continue
			}
			if resp.Since != tc.wantEcho {
				t.Errorf("since=%q: echoed since = %q, want %q", tc.since, resp.Since, tc.wantEcho)
			}
		}
	})

	t.Run("absent since -> 200 with a non-empty default window", func(t *testing.T) {
		// The default is now-90d, which is clock-relative -- assert only that the
		// request succeeds and the response echoes some non-empty bound, not the
		// exact date.
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", code, body)
		}
		var resp struct {
			Since string `json:"since"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Since == "" {
			t.Error("absent since must still echo a non-empty default bound")
		}
	})

	t.Run("malformed forms -> 400 invalid since (not 500, not a silent default)", func(t *testing.T) {
		bad := []string{
			"garbage",              // not a date at all
			"15-06-2026",           // wrong field order
			"2026-13",              // impossible month
			"2026-06-15T00:00:00Z", // RFC3339 is not an accepted layout
		}
		for _, v := range bad {
			code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since="+v, nil)
			if code != http.StatusBadRequest {
				t.Errorf("since=%q: status = %d, want 400 (body: %s)", v, code, body)
				continue
			}
			assertInvalidSinceError(t, v, body)
		}
	})

	// The per-developer handler is a SECOND parseSince call site; a fix applied to
	// one endpoint but not the other would slip through without this mirror.
	t.Run("per-developer endpoint mirrors the 400 contract", func(t *testing.T) {
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/alice?since=garbage", nil)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", code, body)
		}
		assertInvalidSinceError(t, "garbage", body)
	})
}

// assertInvalidSinceError checks a 400 body carries the JSON {"error": "invalid
// since: ..."} contract the dashboard and CLI surface to operators.
func assertInvalidSinceError(t *testing.T, since string, body []byte) {
	t.Helper()
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Errorf("since=%q: unmarshal error response: %v (body: %s)", since, err, body)
		return
	}
	const want = "invalid since:"
	if len(resp.Error) < len(want) || resp.Error[:len(want)] != want {
		t.Errorf("since=%q: error = %q, want it to start with %q", since, resp.Error, want)
	}
}
