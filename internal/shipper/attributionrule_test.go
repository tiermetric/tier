package shipper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// TestClient_NoAttributionRuleIsByteIdenticalToPre823 pins that an event with no
// rule serialises to exactly the bytes a pre-#823 shipper sent. The golden was
// captured at ffe7dec, before the field existed. An older tierd decodes with
// DisallowUnknownFields, so any extra key here 400s every batch against it.
func TestClient_NoAttributionRuleIsByteIdenticalToPre823(t *testing.T) {
	const golden = `[{"developer":"alice","issue_id":"issue-42","model":"claude-sonnet-4",` +
		`"input_tokens":1000,"output_tokens":500,"cache_read_tokens":0,` +
		`"cache_write_5m_tokens":0,"cache_write_1h_tokens":0,"cost_usd":0.0105,` +
		`"source":"jsonl","fidelity":"realtime","idempotency_key":"k-golden",` +
		`"session_id":"sess-1","repo":"acme/widgets","timestamp":"2026-05-19T10:00:00Z"}]`

	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	ev := testEvent("k-golden")
	ev.SessionID = "sess-1"
	ev.Repo = "acme/widgets"
	c := newTestClient(t, srv, Config{})
	ctx := context.Background()
	if err := c.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if string(body) != golden {
		t.Errorf("wire bytes changed for a rule-less event:\n got: %s\nwant: %s", body, golden)
	}
}

// TestClient_InvalidAttributionRuleNeverSent pins that a rule outside the closed set
// is omitted from the wire while the event itself still ships, and that the drop
// logs exactly one WARN per client, carrying none of the dropped values.
func TestClient_InvalidAttributionRuleNeverSent(t *testing.T) {
	invalid := []store.AttributionRule{"Branch", " carry", "legacy", "unknown", "/Users/alice/repo/.git/worktrees/x"}
	logs := captureSlog(t)
	rs := &recordingServer{}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	ctx := context.Background()
	for client := 0; client < 2; client++ {
		c := newTestClient(t, srv, Config{BatchSize: len(invalid)})
		for i, r := range invalid {
			ev := testEvent("k-invalid-" + string(rune('a'+client)) + string(rune('a'+i)))
			ev.AttributionRule = r
			if err := c.Ingest(ctx, ev); err != nil {
				t.Fatalf("Ingest(%q): %v", r, err)
			}
		}
		if err := c.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	if len(rs.batches) != 2 || len(rs.batches[0]) != len(invalid) || len(rs.batches[1]) != len(invalid) {
		t.Fatalf("batches = %v, want two batches of %d", rs.batchSizes(), len(invalid))
	}
	for _, batch := range rs.batches {
		for i, got := range batch {
			if v, present := got["attribution_rule"]; present {
				t.Errorf("invalid rule %q reached the wire as %v", invalid[i], v)
			}
		}
	}
	assertOneRuleWarnPerSink(t, logs, 2, invalid)
}

// captureSlog routes slog.Default to a JSON buffer for the rest of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// assertOneRuleWarnPerSink pins the drop warning: exactly `sinks` WARN records
// mention attribution_rule, and no record anywhere carries a dropped value.
func assertOneRuleWarnPerSink(t *testing.T, logs *bytes.Buffer, sinks int, invalid []store.AttributionRule) {
	t.Helper()
	warns := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec[slog.LevelKey] == slog.LevelWarn.String() && strings.Contains(rec[slog.MessageKey].(string), "attribution_rule") {
			warns++
		}
		for _, v := range invalid {
			if strings.Contains(line, string(v)) {
				t.Errorf("log record carries dropped rule %q: %s", v, line)
			}
		}
	}
	if warns != sinks {
		t.Errorf("attribution_rule WARN records = %d, want %d (one per sink)\nlogs:\n%s", warns, sinks, logs)
	}
}

// TestShipper_AttributionRuleRoundTrips ships every valid rule, an event with no
// rule and an event with an invalid rule, in ONE batch, through the real api.Handler
// and store, and reads the stored rule back.
func TestShipper_AttributionRuleRoundTrips(t *testing.T) {
	srv, db := realTierdServer(t)

	want := map[string]store.AttributionRule{} // idempotency key -> stored rule
	var events []collector.TokenEvent
	add := func(key string, sent, stored store.AttributionRule) {
		e := testEvent(key)
		e.AttributionRule = sent
		e.Timestamp = time.Now().UTC().Add(-time.Duration(len(events)+1) * time.Minute)
		events = append(events, e)
		want[key] = stored
	}
	for _, r := range store.AttributionRules() {
		add("k-rule-"+string(r), r, r)
	}
	add("k-rule-none", store.AttributionRuleNone, store.AttributionRuleNone)
	add("k-rule-invalid", "Worktree-CWD", store.AttributionRuleNone)

	c, err := New(Config{ServerURL: srv.URL, BatchSize: len(events), BackoffBase: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for _, e := range events {
		if err := c.Ingest(ctx, e); err != nil {
			t.Fatalf("Ingest(%s): %v", e.IdempotencyKey, err)
		}
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	stored := storedEvents(t, db)
	for key, rule := range want {
		ev, ok := stored[key]
		if !ok {
			t.Errorf("event %s did not reach the store", key)
			continue
		}
		if ev.AttributionRule != rule {
			t.Errorf("event %s stored attribution_rule %q, want %q", key, ev.AttributionRule, rule)
		}
	}
}

// TestShipper_ForeignRepoBucketShipsWithEmptyRepo pins the #823 Q3 wire rule: the
// server refuses unattributed:foreign-repo with any repo, and the batch is
// all-or-nothing, so the shipper omits repo for that bucket whatever the collector
// resolved, including a real slug. The attributed event shares the batch and keeps
// its repo.
func TestShipper_ForeignRepoBucketShipsWithEmptyRepo(t *testing.T) {
	srv, db := realTierdServer(t)

	repos := map[string]string{ // idempotency key -> collector-resolved repo
		"k-foreign-slug":        "acme/foreign",
		"k-foreign-unqualified": repoid.Unqualified,
		"k-foreign-empty":       "",
	}
	c, err := New(Config{ServerURL: srv.URL, BatchSize: len(repos) + 1, BackoffBase: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	i := 0
	for key, repo := range repos {
		i++
		e := testEvent(key)
		e.IssueID = collector.UnattributedForeignRepo
		e.Repo = repo
		e.Timestamp = time.Now().UTC().Add(-time.Duration(i) * time.Minute)
		if err := c.Ingest(ctx, e); err != nil {
			t.Fatalf("Ingest(%s): %v", key, err)
		}
	}
	attributed := testEvent("k-attributed")
	attributed.Repo = "acme/widgets"
	attributed.Timestamp = time.Now().UTC().Add(-10 * time.Minute)
	if err := c.Ingest(ctx, attributed); err != nil {
		t.Fatalf("Ingest(attributed): %v", err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v — the server refused the batch, so every event in it was lost", err)
	}

	stored := storedEvents(t, db)
	for key := range repos {
		ev, ok := stored[key]
		if !ok {
			t.Errorf("foreign-repo event %s did not reach the store", key)
			continue
		}
		if ev.IssueID != collector.UnattributedForeignRepo || ev.Repo != repoid.Unqualified {
			t.Errorf("event %s stored (issue %q, repo %q), want (%q, %q)",
				key, ev.IssueID, ev.Repo, collector.UnattributedForeignRepo, repoid.Unqualified)
		}
	}
	if got := stored["k-attributed"].Repo; got != "acme/widgets" {
		t.Errorf("attributed event stored repo %q, want acme/widgets", got)
	}
}

// storedEvents reads every token_events row of the last day, keyed by idempotency key.
func storedEvents(t *testing.T, db *store.DB) map[string]store.TokenEvent {
	t.Helper()
	now := time.Now().UTC()
	rows, _, err := db.ListTokenEvents(context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute), store.PageCursor{}, store.MaxExportPageSize)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	out := make(map[string]store.TokenEvent, len(rows))
	for _, r := range rows {
		out[r.IdempotencyKey] = r
	}
	return out
}
