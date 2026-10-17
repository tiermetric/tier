package shipper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientPRFieldsFallback(t *testing.T) {
	for _, tc := range []struct {
		name, message  string
		status, calls  int
		fallback, fail bool
	}{
		{"pr", `json: unknown field "pr_number"`, 400, 2, true, false},
		{"branch", `json: unknown field "branch_hash"`, 400, 2, true, false},
		{"cacheEvidence", `json: unknown field "cache_write_evidence"`, 400, 2, true, false},
		{"other400", `pr_number must be >= 1`, 400, 1, false, true},
		{"otherField", `json: unknown field "host"`, 400, 1, false, true},
		{"5xx", `json: unknown field "pr_number"`, 503, 2, false, true},
		{"2xx", `json: unknown field "branch_hash"`, 201, 1, false, false},
		{"once", `json: unknown field "pr_number"`, 400, 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var batch []map[string]any
				if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
					t.Error(err)
				}
				_, pr := batch[0]["pr_number"]
				_, hash := batch[0]["branch_hash"]
				_, evidence := batch[0]["cache_write_evidence"]
				wantPR := !tc.fallback || calls == 1 || !strings.Contains(tc.message, `"pr_number"`)
				wantHash := !tc.fallback || calls == 1 || !strings.Contains(tc.message, `"branch_hash"`)
				wantEvidence := !tc.fallback || calls == 1 || !strings.Contains(tc.message, `"cache_write_evidence"`)
				if pr != wantPR || hash != wantHash || evidence != wantEvidence {
					t.Errorf("call %d fields=(%v,%v), want %v", calls, pr, hash, wantPR)
				}
				status := tc.status
				if tc.fallback && calls > 1 && tc.name != "once" {
					status = 201
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.message})
			}))
			defer srv.Close()
			c := newTestClient(t, srv, Config{BatchSize: 1, MaxAttempts: 2})
			ev := testEvent("one")
			n, hash := 42, strings.Repeat("a", 64)
			ev.PRNumber, ev.BranchHash = &n, &hash
			ev.CacheWriteEvidence = "nested"
			err := c.Ingest(context.Background(), ev)
			if (err != nil) != tc.fail || calls != tc.calls || c.omitPRFields != tc.fallback {
				t.Fatalf("err=%v calls=%d omitted=%v", err, calls, c.omitPRFields)
			}
			if tc.fallback && !tc.fail {
				c = newTestClient(t, srv, Config{BatchSize: 1, MaxAttempts: 2})
				ev.IdempotencyKey = "two"
				if err := c.Ingest(context.Background(), ev); err != nil {
					t.Fatal(err)
				}
				if calls != 3 {
					t.Fatalf("future batch calls=%d", calls)
				}
			}
		})
	}
}

type prErrorTransport struct {
	calls int
	err   error
}

func TestClientPRFieldsFallbackAlreadyCachedDuringBatch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		_, present := batch[0]["cache_write_evidence"]
		if calls == 1 {
			if !present {
				t.Error("first batch omitted evidence before cache was populated")
			}
			// Another client learns this after our batch has been serialized.
			unsupportedPRFields.LoadOrStore("http://"+r.Host+"/api/v1/events\x00cache_write_evidence", true)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": `json: unknown field "cache_write_evidence"`})
			return
		}
		if present {
			t.Error("retry retained unsupported evidence")
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, Config{BatchSize: 1})
	ev := testEvent("cached-during-batch")
	ev.CacheWriteEvidence = "nested"
	if err := c.Ingest(t.Context(), ev); err != nil {
		t.Fatalf("cached field must still retry this batch: %v", err)
	}
	if calls != 2 || c.Shipped() != 1 {
		t.Fatalf("calls=%d shipped=%d, want 2/1", calls, c.Shipped())
	}
}

func TestClientPRFieldsFallbackSuccessiveRejections(t *testing.T) {
	fields := []string{"cache_write_evidence", "pr_number", "branch_hash"}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		for i, field := range fields {
			_, present := batch[0][field]
			if present != (calls <= i+1) {
				t.Errorf("call %d field %s present=%v", calls, field, present)
			}
		}
		for _, field := range fields {
			if _, present := batch[0][field]; present {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": `json: unknown field "` + field + `"`})
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	ev := testEvent("successive")
	n, hash := 42, strings.Repeat("a", 64)
	ev.PRNumber, ev.BranchHash, ev.CacheWriteEvidence = &n, &hash, "nested"
	for i := 0; i < 2; i++ {
		c := newTestClient(t, srv, Config{BatchSize: 1})
		if err := c.Ingest(t.Context(), ev); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		if calls != 4+i || c.Shipped() != 1 {
			t.Fatalf("batch %d: calls=%d shipped=%d", i, calls, c.Shipped())
		}
		ev.IdempotencyKey = "successive-new-client"
	}
}

func (tr *prErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.calls++
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), `"pr_number"`) || !strings.Contains(string(body), `"branch_hash"`) {
		return nil, errors.New("fields stripped on transport error")
	}
	return nil, tr.err
}

func TestClientPRFieldsTransportErrorsKeepState(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, errors.New("network down")} {
		tr := &prErrorTransport{err: cause}
		c, err := New(Config{ServerURL: "http://localhost", HTTPClient: &http.Client{Transport: tr}, BatchSize: 1, MaxAttempts: 2, BackoffBase: time.Nanosecond})
		if err != nil {
			t.Fatal(err)
		}
		ev := testEvent("transport")
		n, hash := 42, strings.Repeat("a", 64)
		ev.PRNumber, ev.BranchHash = &n, &hash
		ev.CacheWriteEvidence = "nested"
		if err := c.Ingest(context.Background(), ev); !errors.Is(err, cause) || tr.calls != 2 || c.omitPRFields {
			t.Fatalf("err=%v calls=%d omitted=%v", err, tr.calls, c.omitPRFields)
		}
	}
}
