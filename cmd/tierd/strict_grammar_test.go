package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/issueref/issuereftest"
	"github.com/tiermetric/tier/internal/store"
)

func TestStrictGrammarSharedProducerFixtures(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	for _, tc := range issuereftest.Fixtures() {
		t.Run(tc.Name, func(t *testing.T) {
			merged := "2026-10-01T12:00:00Z"
			srv := newFakeGitHub(t, []fakePR{{number: 733, updatedAt: merged, mergedAt: merged, mergeSHA: "strict", branch: tc.Branch, body: tc.Text, login: "alice"}})
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "tier.db")
			var out, errOut bytes.Buffer
			code := dispatch([]string{"backfill", "--repo", "owner/repo", "--since", "2026-01-01", "--token", "x", "--db", path, "--github-api-url", srv.URL, "--tracker-keys", issuereftest.Keys}, &out, &errOut)
			if code != 0 {
				t.Fatalf("code=%d %s", code, &errOut)
			}
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			o, ok, err := db.OutcomeByMergeCommit(context.Background(), "strict")
			if err != nil {
				t.Fatal(err)
			}
			if tc.Revert || tc.PR.ID == "" {
				if ok {
					t.Fatal("unexpected outcome", o)
				}
				if tc.Revert && !strings.Contains(out.String(), "excluded=1") {
					t.Fatal(out.String())
				}
				return
			}
			if !ok || (issueref.Ref{ID: o.IssueID, Rule: o.IssueRule}) != tc.PR {
				t.Fatalf("outcome %+v found=%v want %+v", o, ok, tc.PR)
			}
		})
	}
}

func TestResolveTrackerGrammarPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, env, config string
		cli               []string
		want              string
	}{
		{name: "default"}, {name: "config", config: "CONF", want: "CONF"}, {name: "env over config", env: "ENV", config: "CONF", want: "ENV"},
		{name: "CLI over env and config", env: "ENV", config: "CONF", cli: []string{"--tracker-keys=CLI"}, want: "CLI"},
		{name: "CLI empty", env: "ENV", config: "CONF", cli: []string{"--tracker-keys="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TIER_TRACKER_KEYS", tc.env)
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			raw := addTrackerKeysFlag(fs)
			if err := fs.Parse(tc.cli); err != nil {
				t.Fatal(err)
			}
			path := ""
			if tc.config != "" {
				path = filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("attribution:\n  tracker_keys: "+tc.config+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			g, err := resolveTrackerGrammar(fs, *raw, path)
			if err != nil || strings.Join(g.Keys(), ",") != tc.want {
				t.Fatalf("keys=%v err=%v want=%s", g.Keys(), err, tc.want)
			}
		})
	}
	for _, source := range []string{"CLI", "env", "config"} {
		t.Run("invalid/"+source, func(t *testing.T) {
			t.Setenv("TIER_TRACKER_KEYS", "")
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			raw := addTrackerKeysFlag(fs)
			args := []string{}
			path := ""
			switch source {
			case "CLI":
				args = append(args, "--tracker-keys=bad-entry")
			case "env":
				t.Setenv("TIER_TRACKER_KEYS", "bad-entry")
				*raw = os.Getenv("TIER_TRACKER_KEYS")
			case "config":
				path = filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("attribution:\n  tracker_keys: bad-entry\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveTrackerGrammar(fs, *raw, path); err == nil || !strings.Contains(err.Error(), "bad-entry") {
				t.Fatal(err)
			}
		})
	}
}

func TestDoctorTrackerKeys(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"PROJ-1", "UTF-8", "UTF-8", "OP-42", "A-2", "not-a-key", "CVE-2026-1234"} {
		if _, err := db.InsertOutcome(context.Background(), store.Outcome{Developer: "alice", IssueID: id, Weight: 1, Quality: 1, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, keys := range []string{"", "PROJ"} {
		var out, errOut bytes.Buffer
		args := []string{"--repo", t.TempDir(), "--claude-dir", t.TempDir(), "--db", path, "--tracker-keys", keys}
		_ = runDoctor(args, &out, &errOut)
		want := "tracker keys: none configured (OP built in); unconfigured prefixes (suggest only): UTF (2), A (1), PROJ (1)"
		if keys != "" {
			want = "tracker keys: PROJ (OP built in); unconfigured prefixes (suggest only): UTF (2), A (1)"
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("out=%s stderr=%s want=%s", &out, &errOut, want)
		}
	}
}
