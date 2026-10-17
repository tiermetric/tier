package collector_test

import (
	"context"
	"encoding/json"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatcherPricingPartialFinalWithTTLAuthority(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	project := filepath.Join(root, "projects", strings.ReplaceAll(repo, string(filepath.Separator), "-"))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "watcher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	w := &collector.Watcher{ClaudeDir: root, Repos: []string{repo}, RepoSlugs: map[string]string{repo: "acme/pricing"}, DeveloperID: "alice", Ingester: ingester.Store(db), Checkpoints: db, DebounceDelay: 10 * time.Millisecond}
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("watcher shutdown timed out")
		}
	}()
	time.Sleep(100 * time.Millisecond)
	path := filepath.Join(project, "session.jsonl")
	appendLine := func(output int, nested bool) {
		t.Helper()
		u := map[string]any{"input_tokens": 100, "output_tokens": output, "cache_creation_input_tokens": 1000}
		if nested {
			u["cache_creation"] = map[string]any{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": 1000}
		}
		line := map[string]any{"type": "assistant", "sessionId": "pricing-watch", "gitBranch": "feature/1231-pricing", "cwd": repo, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": map[string]any{"id": "msg_pricing_watch", "role": "assistant", "model": "claude-sonnet-4", "usage": u}}
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(append(b, '\n'))
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	waitOutput := func(want int) store.TokenEvent {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			rows, _, err := db.ListTokenEvents(t.Context(), time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour), store.PageCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 1 && rows[0].OutputTok == want {
				return rows[0]
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("watcher did not promote output to %d", want)
		return store.TokenEvent{}
	}
	appendLine(1, false)
	partial := waitOutput(1)
	appendLine(30, true)
	final := waitOutput(30)
	want := store.ComputeCost(final.Model, store.CostUsage{Input: 100, Output: 30, CacheWrite1h: 1000})
	if final.CacheWrite5m != 0 || final.CacheWrite1h != 1000 || final.CostMicro != want || final.CostMicro <= partial.CostMicro {
		t.Fatalf("partial=%+v final=%+v want cost=%d", partial, final, want)
	}
	rows, err := db.CostConsistency(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "consistent" {
		t.Fatalf("post-fix E2 re-query: %+v", rows)
	}
	t.Log("real watcher replay: new rows=1, understated after=0; synthetic provider fixture, not a provider reconciliation")
}
