package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveWrite(h *Handler, method, target, contentType, body string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

// TestRequireJSON_NonJSONWriteRefused pins #893's measured attack: a tokenless
// server stored an alias and a cost posted as text/plain, the Content-Type a
// cross-site page can send without a CORS preflight. Form types and an absent
// Content-Type are the other preflight-free shapes.
func TestRequireJSON_NonJSONWriteRefused(t *testing.T) {
	const alias = `{"alias":"victim","canonical":"attacker"}`
	const cost = `{"developer":"d","issue_id":"1","model":"claude-sonnet-4","cost_usd":1}`
	cases := []struct{ name, method, target, ct, body string }{
		{"alias text/plain", http.MethodPost, "/api/v1/developer_alias", "text/plain", alias},
		{"alias text/plain charset", http.MethodPost, "/api/v1/developer_alias", "text/plain;charset=UTF-8", alias},
		{"alias form", http.MethodPost, "/api/v1/developer_alias", "application/x-www-form-urlencoded", alias},
		{"alias multipart", http.MethodPost, "/api/v1/developer_alias", "multipart/form-data; boundary=x", alias},
		{"alias absent", http.MethodPost, "/api/v1/developer_alias", "", alias},
		{"costs text/plain", http.MethodPost, "/api/v1/costs", "text/plain", cost},
		{"hierarchy PUT text/plain", http.MethodPut, "/api/v1/org_hierarchy/alice", "text/plain", `{"team":"t"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			rec := serveWrite(h, tc.method, tc.target, tc.ct, tc.body, nil)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415; body = %s", rec.Code, rec.Body)
			}
			aliases, err := db.DeveloperAliases(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(aliases) != 0 {
				t.Fatalf("alias stored despite 415: %v", aliases)
			}
			if costs := developerCosts(t, db); len(costs) != 0 {
				t.Fatalf("cost stored despite 415: %v", costs)
			}
		})
	}
}

// TestRequireJSON_JSONWritePasses pins that the guard admits every shape a real
// client sends: application/json with or without parameters, and a bodyless
// DELETE with no Content-Type (the quickstart's alias removal).
func TestRequireJSON_JSONWritePasses(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		rec := serveWrite(h, http.MethodPost, "/api/v1/developer_alias", ct, `{"alias":"a","canonical":"b"}`, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("Content-Type %q: status = %d, want 201; body = %s", ct, rec.Code, rec.Body)
		}
	}
	rec := serveWrite(h, http.MethodDelete, "/api/v1/developer_alias/a", "", "", nil)
	if rec.Code/100 != 2 {
		t.Fatalf("bodyless DELETE: status = %d, want 2xx; body = %s", rec.Code, rec.Body)
	}
}

// TestRequireJSON_AuthRunsFirst pins the order inside requireAuth: an
// unauthenticated text/plain write still gets 401, not a 415 that would tell an
// unauthenticated caller in token mode which body shape to try.
func TestRequireJSON_AuthRunsFirst(t *testing.T) {
	h, _ := newTestHandlerWithToken(t, "s3cret-893")
	rec := serveWrite(h, http.MethodPost, "/api/v1/costs", "text/plain", `{}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	rec = serveWrite(h, http.MethodPost, "/api/v1/costs", "text/plain", `{}`,
		http.Header{"Authorization": {"Bearer s3cret-893"}})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("token mode: status = %d, want 415", rec.Code)
	}
}

// TestWriteHandlers_BodyEndsAfterOneJSONValue pins #1042 on every write handler
// that decodes a JSON body: the body must END after its one value, apart from
// whitespace. A second value, a stray `]` or `}`, or trailing garbage is a 400
// naming the single-value rule, and never reaches the store: each endpoint's
// `stored` count is 0 after a rejected body and nonzero after an accepted one,
// so the probe is shown to see that endpoint's write. json.Decoder.More reports
// false at `]` and `}`, which is how `{...}]garbage` used to be accepted with a
// 2xx.
func TestWriteHandlers_BodyEndsAfterOneJSONValue(t *testing.T) {
	mustJSON := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	endpoints := []struct {
		name, method, target, valid string
		accept                      int
		rule                        string
		seed, stored                string // optional seed SQL; COUNT(*) of the endpoint's write
	}{
		{"POST /costs", http.MethodPost, "/api/v1/costs",
			`{"developer":"alice","issue_id":"issue-42","model":"claude-sonnet-4","cost_usd":0.0105,"input_tokens":1000,"output_tokens":500}`,
			http.StatusCreated, "exactly one JSON object", "", `SELECT COUNT(*) FROM token_events`},
		{"POST /events", http.MethodPost, "/api/v1/events",
			mustJSON([]map[string]any{validEvent("k-1042")}), http.StatusCreated, "exactly one JSON array",
			"", `SELECT COUNT(*) FROM token_events`},
		{"POST /outcomes", http.MethodPost, "/api/v1/outcomes",
			mustJSON(validOutcome("sha-1042", nil)), http.StatusCreated, "exactly one JSON object",
			"", `SELECT COUNT(*) FROM outcomes`},
		{"POST /actual_spend", http.MethodPost, "/api/v1/actual_spend",
			`{"developer":"alice","period":"2026-05","actual_paid_usd":200}`, http.StatusCreated, "exactly one JSON object",
			"", `SELECT COUNT(*) FROM actual_spend`},
		{"POST /org_actual_spend", http.MethodPost, "/api/v1/org_actual_spend",
			`{"org":"acme","period":"2026-05","actual_paid_usd":200}`, http.StatusCreated, "exactly one JSON object",
			"", `SELECT COUNT(*) FROM org_actual_spend`},
		{"POST /developer_alias", http.MethodPost, "/api/v1/developer_alias",
			`{"alias":"gh","canonical":"os"}`, http.StatusCreated, "exactly one JSON object",
			"", `SELECT COUNT(*) FROM developer_alias`},
		{"PUT /org_hierarchy/{developer}", http.MethodPut, "/api/v1/org_hierarchy/alice",
			`{"team":"eng","division":"plat","org":"acme"}`, http.StatusOK, "exactly one JSON object",
			"", `SELECT COUNT(*) FROM org_hierarchy`},
		{"POST /org_hierarchy (bulk)", http.MethodPost, "/api/v1/org_hierarchy",
			`[{"developer":"bob","team":"eng"}]`, http.StatusCreated, "exactly one JSON array",
			"", `SELECT COUNT(*) FROM org_hierarchy`},
		// An open membership to end: with none, EndMembership is a no-op and the
		// probe could not see an accepted write.
		{"POST /period_membership/{developer}/end", http.MethodPost, "/api/v1/period_membership/alice/end",
			`{"org":"acme","period_end":"2026-05"}`, http.StatusOK, "exactly one JSON object",
			`INSERT INTO period_membership (developer, org, period_start) VALUES ('alice', 'acme', '2026-01')`,
			`SELECT COUNT(*) FROM period_membership WHERE period_end IS NOT NULL`},
	}
	bodies := []struct {
		name   string
		body   func(valid string) string
		accept bool
	}{
		{"valid", func(v string) string { return v }, true},
		{"valid + trailing whitespace", func(v string) string { return v + " \n\t\r\n" }, true},
		{"second value", func(v string) string { return v + " " + v }, false},
		{"trailing garbage", func(v string) string { return v + "garbage" }, false},
		{"stray ] then garbage", func(v string) string { return v + "]garbage" }, false},
		{"stray }", func(v string) string { return v + "}" }, false},
		{"stray ]", func(v string) string { return v + "]" }, false},
		{"stray ] after whitespace", func(v string) string { return v + "\n]" }, false},
	}
	for _, ep := range endpoints {
		for _, b := range bodies {
			t.Run(ep.name+"/"+b.name, func(t *testing.T) {
				h, db := newTestHandler(t)
				raw := rawStore(t, db)
				if ep.seed != "" {
					if _, err := raw.Exec(ep.seed); err != nil {
						t.Fatalf("seed: %v", err)
					}
				}
				rec := serveWrite(h, ep.method, ep.target, "application/json", b.body(ep.valid), nil)
				n := rawCount(t, raw, ep.stored)
				if b.accept {
					if rec.Code != ep.accept {
						t.Fatalf("status = %d, want %d; body = %s", rec.Code, ep.accept, rec.Body)
					}
					if n == 0 {
						t.Fatalf("accepted write not visible to the probe %q, so a reject row's 0 would prove nothing", ep.stored)
					}
					return
				}
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 — a body that does not end after one JSON value was accepted; body = %s", rec.Code, rec.Body)
				}
				if !strings.Contains(rec.Body.String(), ep.rule) {
					t.Errorf("400 must come from the single-value rule (%q), not another check; body = %s", ep.rule, rec.Body)
				}
				if n != 0 {
					t.Errorf("rejected body reached the store: %q = %d, want 0", ep.stored, n)
				}
			})
		}
	}
}

// TestWriteHandlers_TrailingBytesOverCapIsASizeError pins that the handlers
// which tell a body-size rejection apart from malformed JSON keep doing so when
// the value fits the cap and only its trailing bytes exceed it: the 400 carries
// the same size message the first Decode would give, not the single-value rule.
func TestWriteHandlers_TrailingBytesOverCapIsASizeError(t *testing.T) {
	mustJSON := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	cases := []struct {
		name, target, valid, want string
		limit                     int
	}{
		{"POST /events", "/api/v1/events", mustJSON([]map[string]any{validEvent("k-1042-cap")}),
			fmt.Sprintf("request body exceeds %d bytes; split into smaller batches", MaxEventsBody), MaxEventsBody},
		{"POST /outcomes", "/api/v1/outcomes", mustJSON(validOutcome("sha-1042-cap", nil)),
			fmt.Sprintf("request body exceeds %d bytes", maxOutcomeBody), maxOutcomeBody},
		{"POST /org_hierarchy (bulk)", "/api/v1/org_hierarchy", `[{"developer":"bob","team":"eng"}]`,
			fmt.Sprintf("request body exceeds %d bytes; split into smaller batches", MaxHierarchyBody), MaxHierarchyBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			body := tc.valid + strings.Repeat(" ", tc.limit+1)
			rec := serveWrite(h, http.MethodPost, tc.target, "application/json", body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
			}
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode error body: %v; body = %s", err, rec.Body)
			}
			if got.Error != tc.want {
				t.Fatalf("error = %q, want the size message %q", got.Error, tc.want)
			}
		})
	}
}

// TestRequireJSONEOF pins the helper on its own: nil only when the input ends
// after the first value apart from whitespace. The MaxBytesReader row is a body
// whose object fits the cap but whose trailing whitespace does not; the read
// error must be refused, not mistaken for the end of input.
func TestRequireJSONEOF(t *testing.T) {
	cases := []struct {
		name, body string
		ok         bool
	}{
		{"object", `{"a":1}`, true},
		{"array", `[1,2]`, true},
		{"trailing whitespace", "{\"a\":1} \n\t\r\n", true},
		{"second object", `{"a":1} {"b":2}`, false},
		{"second scalar", `{"a":1} 5`, false},
		{"stray ] then garbage", `{"a":1}]garbage`, false},
		{"stray }", `{"a":1}}`, false},
		{"stray ]", `{"a":1}]`, false},
		{"stray : ", `{"a":1}:`, false},
		{"stray ,", `{"a":1},`, false},
		{"garbage", `{"a":1}x`, false},
		{"truncated second value", `{"a":1} {"b"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tc.body))
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatalf("first Decode: %v", err)
			}
			err := requireJSONEOF(dec)
			if tc.ok && err != nil {
				t.Fatalf("requireJSONEOF = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("requireJSONEOF = nil, want an error")
			}
		})
	}

	t.Run("trailing bytes over the MaxBytesReader cap", func(t *testing.T) {
		const limit = 64
		body := `{"a":1}` + strings.Repeat(" ", 4*limit)
		rec := httptest.NewRecorder()
		dec := json.NewDecoder(http.MaxBytesReader(rec, io.NopCloser(strings.NewReader(body)), limit))
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("first Decode: %v", err)
		}
		var maxErr *http.MaxBytesError
		if err := requireJSONEOF(dec); !errors.As(err, &maxErr) {
			t.Fatalf("requireJSONEOF = %v, want *http.MaxBytesError", err)
		}
	})
}
