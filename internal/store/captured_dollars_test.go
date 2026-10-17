package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCapturedDollarRowsWindow_StoredMetadataAndScope(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []TokenEvent{
		{Developer: "alice", Repo: "acme/a", IssueID: "unattributed:main", Source: "jsonl", SessionID: "sub", CostMicro: 3000000},
		{Developer: "alice", Repo: "acme/a", IssueID: "issue-1", Source: "codex-rollout", SessionID: "codex", CostMicro: 1000000},
		{Developer: "alice", Repo: "acme/a", IssueID: "issue-1", Source: "api", CostMicro: 9000000},
		{Developer: "alice", Repo: "acme/b", IssueID: "unattributed:main", Source: "jsonl", SessionID: "foreign", CostMicro: 8000000},
	} {
		e.Model = "claude-sonnet-4"
		e.Fidelity = "realtime"
		e.Timestamp = now
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, cp := range []WatcherCheckpoint{
		{Path: "/no-such-logs/subagents/agent-x.jsonl", Metadata: `{"SessionID":"sub","CWD":"/stored/main"}`},
		{Path: "/no-such-logs/foreign.jsonl", Metadata: `{"SessionID":"foreign","CWD":"/foreign/path"}`},
		{Path: "/no-such-logs/bad.jsonl", Metadata: `not json`},
	} {
		if err := db.SaveWatcherCheckpoint(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	var rows []CapturedDollarRow
	if err := db.ReadSnapshot(ctx, func(s *Snapshot) error {
		var err error
		rows, err = s.CapturedDollarRowsWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour), RepoScope("acme/a"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Engine == "claude-code" {
			if r.Origin != "subagent" || r.CWD != "/stored/main" || r.CostMicro != 3000000 || r.SampleSize != 1 {
				t.Fatalf("stored evidence: %+v", r)
			}
		} else if r.Origin != "unknown" || r.CWD != "" {
			t.Fatalf("invented origin: %+v", r)
		}
	}
	// The unique parent supplies cwd when subagent metadata disagrees.
	if err := db.SaveWatcherCheckpoint(ctx, WatcherCheckpoint{Path: "/no-such-logs/duplicate.jsonl", Metadata: `{"SessionID":"sub","CWD":"/other/main"}`}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.CapturedDollarRowsWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour), RepoScope("acme/a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Engine == "claude-code" && (r.Origin != "mixed" || r.CWD != "/other/main") {
			t.Fatalf("ambiguous metadata used: %+v", r)
		}
	}
	rows, err = db.CapturedDollarRowsWindow(ctx, now.Add(-2*time.Hour), now.Add(-time.Hour), FleetWide)
	if err != nil || len(rows) != 0 {
		t.Fatalf("window: %+v, %v", rows, err)
	}
}

func TestCapturedDollarRowsWindow_SessionLevelEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                string
		paths, cwds         []string
		wantCWD, wantOrigin string
	}{
		{"parent only", []string{"parent.jsonl"}, []string{"/main"}, "/main", "main_session"},
		{"subagent only", []string{"subagents/a.jsonl"}, []string{"/main"}, "/main", "subagent"},
		{"mixed agreement", []string{"parent.jsonl", "subagents/a.jsonl"}, []string{"/main", "/main"}, "/main", "mixed"},
		{"mixed unique parent", []string{"parent.jsonl", "subagents/a.jsonl", "subagents/b.jsonl"}, []string{"/parent", "/a", "/b"}, "/parent", "mixed"},
		{"mixed ambiguous parents", []string{"parent.jsonl", "other.jsonl", "subagents/a.jsonl"}, []string{"/a", "/b", "/a"}, "", "mixed"},
		{"multiple parents agree", []string{"parent.jsonl", "other.jsonl"}, []string{"/main", "/main"}, "/main", "main_session"},
		{"subagents disagree", []string{"subagents/a.jsonl", "subagents/b.jsonl"}, []string{"/a", "/b"}, "", "subagent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			now := time.Now().UTC()
			if err := db.InsertTokenEvent(ctx, TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: "unattributed:main", Source: "jsonl", SessionID: "parent-session", CostMicro: 1000000, Model: "claude-sonnet-4", Fidelity: "realtime", Timestamp: now}); err != nil {
				t.Fatal(err)
			}
			for i, p := range tc.paths {
				metadata, err := json.Marshal(map[string]string{CheckpointSessionIDKey: "parent-session", CheckpointCWDKey: tc.cwds[i]})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.SaveWatcherCheckpoint(ctx, WatcherCheckpoint{Path: "/missing/" + p, Metadata: string(metadata)}); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := db.CapturedDollarRowsWindow(ctx, now.Add(-time.Hour), time.Time{}, FleetWide)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].CWD != tc.wantCWD || rows[0].Origin != tc.wantOrigin {
				t.Fatalf("session evidence: %+v", rows)
			}
		})
	}
}
