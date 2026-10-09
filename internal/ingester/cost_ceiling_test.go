package ingester_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/anthropicadmin"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/collector/muse"
	"github.com/tiermetric/tier/internal/collector/openaiusage"
	"github.com/tiermetric/tier/internal/collector/opencode"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/proxy"
	"github.com/tiermetric/tier/internal/store"
)

const hugeInput = 200_000_000 // Valid parser counts; fixture rates price these at $20k.

func captureDB(t *testing.T) (*store.DB, *sql.DB, *metrics.Registry) {
	t.Helper()
	// Exercise a loadable operator table without exceeding Muse's per-record token cap.
	prices := filepath.Join(t.TempDir(), "prices.yaml")
	writeCaptureFile(t, prices, `version: 99
effective_date: "2026-06-01"
models:
  claude-sonnet-4: {input_per_m: 100, output_per_m: 100, provider: anthropic}
  gpt-4o: {input_per_m: 100, output_per_m: 100, provider: openai}
  gemini-2.5-pro: {input_per_m: 100, output_per_m: 100, provider: google}
  muse-spark-1.3-contributor: {input_per_m: 100, output_per_m: 100, provider: meta}
  glm-5.3: {input_per_m: 100, output_per_m: 100, provider: zai}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
`)
	if _, err := store.LoadPriceTable(prices); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.LoadPriceTable("../store/prices.yaml"); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	})
	reg := metrics.NewRegistry()
	store.SetCostClampRecorder(reg.NewCounter("clamps", "test"))
	t.Cleanup(func() { store.SetCostClampRecorder(nil) })
	return db, raw, reg
}

func assertCapturePair(t *testing.T, raw *sql.DB, source, fidelity string, reg *metrics.Registry) {
	t.Helper()
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM token_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stored %d rows, want both", count)
	}
	rows, err := raw.Query(`SELECT input_tok, cost_micro, cost_clamped, source, fidelity FROM token_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var input int64
		var cost int64
		var marked sql.NullBool
		var gotSource, gotFidelity string
		if err := rows.Scan(&input, &cost, &marked, &gotSource, &gotFidelity); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			if input != hugeInput || cost != store.MaxTokenEventCostMicro || !marked.Valid || !marked.Bool {
				t.Fatalf("first: tokens=%d cost=%d mark=%v", input, cost, marked)
			}
		} else if input != 1000 || cost <= 0 || cost >= store.MaxTokenEventCostMicro || marked.Valid {
			t.Fatalf("normal: tokens=%d cost=%d mark=%v", input, cost, marked)
		}
		if gotSource != source || gotFidelity != fidelity {
			t.Fatalf("provenance changed: %s/%s", gotSource, gotFidelity)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stored %d rows, want both", n)
	}
	var out strings.Builder
	reg.Render(&out)
	if !strings.Contains(out.String(), "clamps 1\n") {
		t.Fatalf("clamp counter: %s", out.String())
	}
}

func captureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return repo
}

func writeCaptureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// Settled callbacks run only after a successful pass and cursor advance.
func runCapturePass(t *testing.T, run func(context.Context) error, settled <-chan time.Time, after time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case through := <-settled:
		if !through.After(after) {
			t.Fatalf("settled through %v, must pass %v", through, after)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not finish and advance its cursor")
	}
}

func TestCaptureCostCeilingProxy(t *testing.T) {
	for _, provider := range []proxy.Provider{proxy.ProviderAnthropic, proxy.ProviderOpenAI, proxy.ProviderGemini} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", provider, stream), func(t *testing.T) {
				db, raw, reg := captureDB(t)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					input := hugeInput
					if r.URL.Query().Get("normal") != "" {
						input = 1000
					}
					id := fmt.Sprintf("msg-%d", input)
					var body string
					switch provider {
					case proxy.ProviderAnthropic:
						body = fmt.Sprintf(`{"id":%q,"model":"claude-sonnet-4","usage":{"input_tokens":%d,"output_tokens":0}}`, id, input)
						if stream {
							body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + body + "}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
					case proxy.ProviderOpenAI:
						body = fmt.Sprintf(`{"id":%q,"model":"gpt-4o","usage":{"prompt_tokens":%d,"completion_tokens":0}}`, id, input)
						if stream {
							body = "data: " + body + "\n\ndata: [DONE]\n\n"
						}
					case proxy.ProviderGemini:
						body = fmt.Sprintf(`{"responseId":%q,"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":0,"totalTokenCount":%d}}`, id, input, input)
						if stream {
							body = "data: " + body + "\n\n"
						}
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					_, _ = fmt.Fprint(w, body)
				}))
				defer upstream.Close()
				target, err := url.Parse(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(proxy.New(target, provider, collector.SourceProxy, ingester.Store(db), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
				defer server.Close()
				for i, query := range []string{"", "?normal=1"} {
					req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages"+query, nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("X-Tier-Developer", "alice")
					req.Header.Set("X-Tier-Issue", "issue-1082")
					resp, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_, readErr := io.Copy(io.Discard, resp.Body)
					closeErr := resp.Body.Close()
					if readErr != nil || closeErr != nil || resp.StatusCode != http.StatusOK {
						t.Fatalf("response: %d %v %v", resp.StatusCode, readErr, closeErr)
					}
					// Wait for server-side body Close to store this row before the next request.
					deadline := time.Now().Add(5 * time.Second)
					for {
						var count int
						if err := raw.QueryRow(`SELECT COUNT(*) FROM token_events`).Scan(&count); err != nil {
							t.Fatal(err)
						}
						if count == i+1 {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("stored %d rows, want %d before deadline", count, i+1)
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				assertCapturePair(t, raw, collector.SourceProxy, collector.FidelityRealtime, reg)
			})
		}
	}
}

func jsonlCaptureBody(repo string) string {
	var body strings.Builder
	for i, input := range []int{hugeInput, 1000} {
		fmt.Fprintf(&body, `{"type":"assistant","timestamp":"2026-06-15T12:00:0%dZ","sessionId":"s","gitBranch":"fix/1082-cost","cwd":%q,"message":{"id":"msg-%d","model":"claude-sonnet-4","usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n", i, repo, i, input)
	}
	return body.String()
}

func TestCaptureCostCeilingJSONL(t *testing.T) {
	db, raw, reg := captureDB(t)
	repo, home := captureRepo(t), t.TempDir()
	writeCaptureFile(t, filepath.Join(home, "projects", "p", "session.jsonl"), jsonlCaptureBody(repo))
	c := &collector.JSONLCollector{RepoPath: repo, ClaudeDir: home, DeveloperID: "alice", RepoSlug: "tiermetric/tier"}
	if err := c.Run(context.Background(), time.Time{}, ingester.Store(db)); err != nil {
		t.Fatal(err)
	}
	assertCapturePair(t, raw, collector.SourceJSONL, collector.FidelityRealtime, reg)
}

// A log handler signals the real watcher has installed its fsnotify watches.
type watcherReadyHandler struct{ ready chan struct{} }

func (h watcherReadyHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h watcherReadyHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "watcher started" {
		close(h.ready)
	}
	return nil
}
func (h watcherReadyHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h watcherReadyHandler) WithGroup(string) slog.Handler      { return h }

func TestCaptureCostCeilingWatcher(t *testing.T) {
	db, raw, reg := captureDB(t)
	repo, home := captureRepo(t), t.TempDir()
	path := filepath.Join(home, "projects", "p", "session.jsonl")
	writeCaptureFile(t, path, "")
	ready := make(chan struct{})
	w := &collector.Watcher{ClaudeDir: home, Repos: []string{repo}, Ingester: ingester.Store(db), Checkpoints: db, DeveloperID: "alice", DebounceDelay: 10 * time.Millisecond, Logger: slog.New(watcherReadyHandler{ready})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not start")
	}
	body := jsonlCaptureBody(repo)
	writeCaptureFile(t, path, body)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cp, ok, err := db.LoadWatcherCheckpoint(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if ok && cp.Offset == int64(len(body)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoint did not pass both events: %+v", cp)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertCapturePair(t, raw, collector.SourceJSONL, collector.FidelityRealtime, reg)
}

func TestCaptureCostCeilingCodex(t *testing.T) {
	db, raw, reg := captureDB(t)
	repo, root := captureRepo(t), t.TempDir()
	body := fmt.Sprintf(`{"timestamp":"2026-06-15T12:00:00Z","type":"session_meta","payload":{"id":"s","cwd":%q,"git":{"branch":"fix/1082-cost"}}}`+"\n", repo)
	body += `{"timestamp":"2026-06-15T12:00:00Z","type":"turn_context","payload":{"model":"gpt-4o"}}` + "\n"
	for i, total := range []int{hugeInput, hugeInput + 1000} {
		body += fmt.Sprintf(`{"timestamp":"2026-06-15T12:00:0%dZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d}}}}`+"\n", i+1, total, total)
	}
	writeCaptureFile(t, filepath.Join(root, "2026", "06", "15", "rollout-limit.jsonl"), body)
	settled := make(chan time.Time, 1)
	c, err := codexrollout.New(codexrollout.Config{SessionsDir: root, Repos: []codexrollout.RepoTarget{{Path: repo, Slug: "tiermetric/tier"}}, Interval: time.Hour, Settled: func(_ context.Context, _, through time.Time) { settled <- through }})
	if err != nil {
		t.Fatal(err)
	}
	runCapturePass(t, func(ctx context.Context) error { return c.Run(ctx, time.Time{}, ingester.Store(db)) }, settled, time.Date(2026, 6, 15, 12, 0, 2, 0, time.UTC))
	assertCapturePair(t, raw, collector.SourceCodexRollout, collector.FidelityRealtime, reg)
}

func TestCaptureCostCeilingMuse(t *testing.T) {
	db, raw, reg := captureDB(t)
	repo, home := captureRepo(t), t.TempDir()
	body := fmt.Sprintf(`{"schema_version":1,"id":"meta","stream":{"kind":"session","id":"s"},"sequence":1,"recorded_at":1781524800000000,"record_type":"event","payload_type":"runtime.session.metadata","payload":{"kind":"metadata","record":{"workspace_root":%q}}}`+"\n", repo)
	for i, input := range []int{hugeInput, 1000} {
		body += fmt.Sprintf(`{"schema_version":1,"id":"call-%d","stream":{"kind":"session","id":"s"},"sequence":%d,"recorded_at":%d,"record_type":"event","payload_type":"runtime.session","payload":{"kind":"run","run_id":"r","event":{"kind":"model_completed","model":"muse-spark-1.3-contributor","usage":{"input_tokens":%d,"output_tokens":0,"cached_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0}}}}`+"\n", i, i+2, 1781524801000000+int64(i)*1000000, input)
	}
	body += `{"schema_version":1,"id":"branch","stream":{"kind":"session","id":"s"},"sequence":4,"recorded_at":1781524803000000,"record_type":"event","payload_type":"session.workspace_branch.observed","payload":{"kind":"workspace_branch","record":{"command_id":"r","reference":{"kind":"branch","name":"fix/1082-cost"}}}}` + "\n"
	writeCaptureFile(t, filepath.Join(home, "sessions", "2026", "06", "15", "s", "session.jsonl"), body)
	settled := make(chan time.Time, 1)
	c, err := muse.New(muse.Config{Home: home, Repos: []muse.RepoTarget{{Path: repo, Slug: "tiermetric/tier"}}, Interval: time.Hour, Settled: func(_ context.Context, _, through time.Time) { settled <- through }})
	if err != nil {
		t.Fatal(err)
	}
	runCapturePass(t, func(ctx context.Context) error { return c.Run(ctx, time.Time{}, ingester.Store(db)) }, settled, time.Date(2026, 6, 15, 12, 0, 2, 0, time.UTC))
	assertCapturePair(t, raw, collector.SourceMuse, collector.FidelityRealtime, reg)
}

func TestCaptureCostCeilingOpencode(t *testing.T) {
	db, raw, reg := captureDB(t)
	repo := captureRepo(t)
	path := filepath.Join(t.TempDir(), "opencode.db")
	fixture, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.Close() }()
	for _, ddl := range []string{`CREATE TABLE migration(id TEXT PRIMARY KEY,time_completed INTEGER NOT NULL)`, `INSERT INTO migration VALUES ('fixture',1)`, `CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL)`} {
		if _, err := fixture.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	const updated int64 = 1781524800000
	for i, input := range []int{hugeInput, 1000} {
		body := fmt.Sprintf(`{"role":"assistant","modelID":"glm-5.3","providerID":"zai-coding-plan","path":{"cwd":%q,"root":"/"},"time":{"created":%d,"completed":%d},"tokens":{"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0},"total":%d}}`, repo, updated+int64(i), updated+int64(i), input, input)
		if _, err := fixture.Exec(`INSERT INTO message VALUES (?,?,?,?,?)`, fmt.Sprintf("msg-%d", i), "s", updated+int64(i), updated+int64(i), body); err != nil {
			t.Fatal(err)
		}
	}
	settled := make(chan time.Time, 1)
	c, err := opencode.New(opencode.Config{DBPath: path, Repos: []opencode.RepoTarget{{Path: repo, Slug: "tiermetric/tier"}}, Checkpoints: db, Interval: time.Hour, Settled: func(_ context.Context, _, through time.Time) { settled <- through }})
	if err != nil {
		t.Fatal(err)
	}
	runCapturePass(t, func(ctx context.Context) error { return c.Run(ctx, time.Time{}, ingester.Store(db)) }, settled, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	// Find the scope-keyed checkpoint without duplicating the collector's scope hash.
	prefix := "opencode-db:" + path + ":"
	var checkpointCount int
	var checkpointKey string
	if err := raw.QueryRow(`SELECT COUNT(*), COALESCE(MIN(path), '') FROM watcher_checkpoint WHERE substr(path, 1, length(?)) = ?`, prefix, prefix).Scan(&checkpointCount, &checkpointKey); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 1 {
		t.Fatalf("checkpoints with prefix %q: got %d, want 1", prefix, checkpointCount)
	}
	cp, ok, err := db.LoadWatcherCheckpoint(context.Background(), checkpointKey)
	if err != nil || !ok {
		t.Fatalf("checkpoint: %v %v", ok, err)
	}
	var md struct {
		Updated int64 `json:"opencode_time_updated_ms"`
	}
	if err := json.Unmarshal([]byte(cp.Metadata), &md); err != nil {
		t.Fatal(err)
	}
	if md.Updated != updated+1 {
		t.Fatalf("watermark=%d", md.Updated)
	}
	assertCapturePair(t, raw, collector.SourceOpencode, collector.FidelityRealtime, reg)
}

func TestCaptureCostCeilingPollers(t *testing.T) {
	for _, source := range []string{collector.SourceAnthropicAdmin, collector.SourceOpenAIUsage} {
		t.Run(source, func(t *testing.T) {
			db, raw, reg := captureDB(t)
			var buckets []any
			for i, input := range []int{hugeInput, 1000} {
				start := time.Date(2026, 6, 15+i, 0, 0, 0, 0, time.UTC)
				if source == collector.SourceAnthropicAdmin {
					buckets = append(buckets, map[string]any{"starting_at": start, "ending_at": start.Add(24 * time.Hour), "results": []any{map[string]any{"model": "claude-sonnet-4", "uncached_input_tokens": input, "output_tokens": 0}}})
				} else {
					buckets = append(buckets, map[string]any{"start_time": start.Unix(), "end_time": start.Add(24 * time.Hour).Unix(), "results": []any{map[string]any{"model": "gpt-4o", "input_tokens": input, "input_cached_tokens": 0, "output_tokens": 0}}})
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any = []any{}
				if strings.Contains(r.URL.Path, "usage") {
					data = buckets
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			settled := make(chan time.Time, 1)
			now := func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) }
			mark := func(_ context.Context, _, through time.Time) { settled <- through }
			var c collector.Collector
			if source == collector.SourceAnthropicAdmin {
				c = anthropicadmin.NewPoller(anthropicadmin.PollerConfig{Client: anthropicadmin.NewClient(anthropicadmin.ClientConfig{APIKey: "fixture", BaseURL: server.URL}), Store: db, Org: "acme", Interval: time.Hour, Now: now, Settled: mark})
			} else {
				c = openaiusage.NewPoller(openaiusage.PollerConfig{Client: openaiusage.NewClient(openaiusage.ClientConfig{APIKey: "fixture", BaseURL: server.URL}), Store: db, Org: "acme", Interval: time.Hour, Now: now, Settled: mark})
			}
			runCapturePass(t, func(ctx context.Context) error { return c.Run(ctx, time.Time{}, ingester.Store(db)) }, settled, time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC))
			assertCapturePair(t, raw, source, collector.FidelityDaily, reg)
		})
	}
}
