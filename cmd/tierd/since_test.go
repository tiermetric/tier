package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

func sinceSessionFixture(t *testing.T) (string, string) {
	t.Helper()
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	projects := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, session := range []struct {
		id    string
		times []string
	}{
		{"straddling", []string{"2026-05-18T23:59:59Z", "2026-05-19T00:00:00Z", "2026-05-19T00:01:00Z"}},
		{"after", []string{"2026-05-19T01:00:00Z", "2026-05-19T01:01:00Z"}},
	} {
		var lines []string
		for i, ts := range session.times {
			branch := ""
			if i == 0 {
				branch = "feature/42-foo"
			}
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":%q,"gitBranch":%q,"cwd":%q,"message":{"id":%q,"model":"claude-sonnet-4","usage":{"input_tokens":1000,"output_tokens":500}}}`, ts, session.id, branch, repo, fmt.Sprintf("%s-%d", session.id, i)))
		}
		if err := os.WriteFile(filepath.Join(projects, session.id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo, claudeDir
}

func TestRunScore_SinceFiltersMessages(t *testing.T) {
	loadEmbeddedPriceTable(t)
	repo, claudeDir := sinceSessionFixture(t)
	out := captureStdout(t, func() {
		runScore([]string{"--repo", repo, "--claude-dir", claudeDir, "--since", "2026-05-19", "--developer", "alice"})
	})
	var total, developer []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "TOTAL" {
			total = fields
		}
		if len(fields) > 0 && fields[0] == "alice" {
			developer = fields
		}
	}
	if !reflect.DeepEqual(total, []string{"TOTAL", "0.0420"}) {
		t.Errorf("total = %v, want four in-window messages costing 0.0420", total)
	}
	if !reflect.DeepEqual(developer, []string{"alice", "4000", "2000", "0", "0", "0", "0.0420"}) {
		t.Errorf("developer totals = %v, want only in-window tokens and cost", developer)
	}
}

func TestJSONLCollector_SincePreservesKeptEvents(t *testing.T) {
	repo, claudeDir := sinceSessionFixture(t)
	c := &collector.JSONLCollector{RepoPath: repo, ClaudeDir: claudeDir, DeveloperID: "alice", RepoSlug: "owner/repo"}
	ctx := context.Background()
	all, err := c.Collect(ctx, time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	var want []collector.TokenEvent
	for _, ev := range all {
		if !ev.Timestamp.Before(since) {
			if ev.IssueID != "issue-42" {
				t.Fatalf("fixture attribution = %q, want issue-42", ev.IssueID)
			}
			want = append(want, ev)
		}
	}
	if len(all) != 5 || len(want) != 4 {
		t.Fatalf("fixture: all=%d, kept=%d, want 5 and 4", len(all), len(want))
	}
	got, err := c.Collect(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect returned %d events, want the four unchanged in-window events", len(got))
	}
	var streamed []collector.TokenEvent
	if err := c.Run(ctx, since, collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		streamed = append(streamed, ev)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamed, want) {
		t.Errorf("Run returned %d events, want the four unchanged in-window events", len(streamed))
	}
}

func TestRunShip_SinceFiltersMessages(t *testing.T) {
	loadDeterministicPrices(t)
	repo, claudeDir := sinceSessionFixture(t)
	const token = "since-test-token"
	srv, db := newShipTestServer(t, token)
	out := captureStdout(t, func() {
		runShip([]string{"--server", srv.URL, "--api-token", token, "--repo", repo, "--claude-dir", claudeDir, "--since", "2026-05-19", "--developer", "alice"})
	})
	if !strings.Contains(out, "Shipped 4 events") {
		t.Errorf("want four shipped events, got:\n%s", out)
	}
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[0].TotalCostMicro != 42000 {
		t.Errorf("stored costs = %+v, want only 42000 micro-dollars", costs)
	}
}

func TestJSONLCollector_SinceExcludesTimestampLessMessage(t *testing.T) {
	repo, claudeDir := sinceSessionFixture(t)
	path := filepath.Join(claudeDir, "projects", "straddling.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","sessionId":"straddling","cwd":%q,"message":{"id":"no-timestamp","model":"claude-sonnet-4","usage":{"input_tokens":777,"output_tokens":500}}}`, repo)
	if err := os.WriteFile(path, append(data, []byte(line+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &collector.JSONLCollector{RepoPath: repo, ClaudeDir: claudeDir, DeveloperID: "alice"}
	ctx := context.Background()
	all, err := c.Collect(ctx, time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range all {
		if ev.InputTok == 777 {
			found = true
			if want := time.Date(2026, 5, 18, 23, 59, 59, 0, time.UTC); !ev.Timestamp.Equal(want) {
				t.Fatalf("timestamp-less message timestamp = %v, want session start %v", ev.Timestamp, want)
			}
		}
	}
	if !found || len(all) != 6 {
		t.Fatalf("fixture: timestamp-less message found=%v, events=%d, want true and 6", found, len(all))
	}
	got, err := c.Collect(ctx, time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("Collect returned %d events, want four in-window messages", len(got))
	}
	for _, ev := range got {
		if ev.InputTok == 777 {
			t.Error("timestamp-less message from session starting before since was included")
		}
	}
}
