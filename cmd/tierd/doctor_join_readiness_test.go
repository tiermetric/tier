package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/diagnostics"
	"github.com/tiermetric/tier/internal/store"
)

func TestDoctorCapturedDollars_MainCheckoutWithWorktrees(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	linked := filepath.Join(root, "linked")
	admin := filepath.Join(main, ".git", "worktrees", "linked")
	if err := os.MkdirAll(admin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linked, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(admin, "gitdir"), filepath.Join(linked, ".git")+"\n")
	write(filepath.Join(linked, ".git"), "gitdir: "+admin+"\n")
	// The diagnostic receives stored paths only: no session/transcript file exists.
	rows := []store.CapturedDollarRow{{Engine: "claude-code", Bucket: "unattributed:main", Origin: "subagent", CWD: main, CostMicro: 8000000, SampleSize: 2}, {Engine: "codex-rollout", Bucket: "issue-1", Origin: "unknown", CostMicro: 2000000, SampleSize: 1}}
	d := diagnostics.BuildCaptured(rows, "2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z", "acme/repo", map[string]string{"claude-code": "enabled", "codex-rollout": "disabled"})
	var s scoresResponse
	// A branch-only /scores field must not populate the internal receipt.
	body, err := json.Marshal(map[string]any{"data_quality": map[string]any{"captured_dollar_diagnostics": d}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	if s.DataQuality.CapturedDollarDiagnostics != nil {
		t.Fatal("decoded captured-dollar diagnostics from /scores")
	}
	// The separate diagnostics route supplies this internal field.
	s.DataQuality.CapturedDollarDiagnostics = d
	got := checkCapturedDollars(s)
	if len(got) != 1 || got[0].status != statusWarn {
		t.Fatalf("check: %+v", got)
	}
	for _, want := range []string{"80%", "3", "acme/repo"} {
		if !strings.Contains(got[0].detail, want) {
			t.Errorf("detail missing %q: %s", want, got[0].detail)
		}
	}
	if got[0].hint == "" || !strings.Contains(got[0].hint, diagnostics.WorktreeExplanationURL) {
		t.Fatal("missing explanation link")
	}
	if !strings.Contains(got[0].detail, "2026-10-01 to 2026-11-01") || strings.Contains(got[0].detail, "T00:") {
		t.Fatalf("window: %s", got[0].detail)
	}
	d.Until = ""
	s.DataQuality.CapturedDollarDiagnostics = d
	if detail := checkCapturedDollars(s)[0].detail; strings.Contains(detail, " to ") {
		t.Fatalf("open until printed: %s", detail)
	}
	if diagnostics.MainCheckoutWithWorktrees(linked) {
		t.Fatal("linked checkout misidentified as main")
	}
	otherAdmin := filepath.Join(t.TempDir(), ".git", "worktrees", "x")
	if err := os.MkdirAll(otherAdmin, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(linked, ".git"), "gitdir: "+otherAdmin+"\n")
	if diagnostics.MainCheckoutWithWorktrees(main) {
		t.Fatal("unrelated backlink accepted")
	}
	if checkCapturedDollars(scoresResponse{}) != nil || checkJoinReadiness(scoresResponse{}) != nil {
		t.Fatal("absent receipt inferred counts")
	}
}

func TestDoctorJoinReadinessMirrorsReceipt(t *testing.T) {
	var s scoresResponse
	if err := json.Unmarshal([]byte(`{"data_quality":{"join_readiness":{"outcome_only_repos":[{"repo":"acme/a","outcome_count":2,"points":3}],"incompatible_repos":[{}],"unqualified_cost_share":0.25}}}`), &s); err != nil {
		t.Fatal(err)
	}
	got := checkJoinReadiness(s)
	if len(got) != 2 || got[0].status != statusWarn || got[1].status != statusInfo || !strings.Contains(got[0].detail, "acme/a") || !strings.Contains(got[1].detail, "25%") {
		t.Fatalf("check: %+v", got)
	}
}

func TestDoctorJoinReadinessCoverageIsInfo(t *testing.T) {
	var s scoresResponse
	if err := json.Unmarshal([]byte(`{"data_quality":{"join_readiness":{"cost_only_repos":[{"repo":"acme/a","captured_cost_usd":5}],"issue_spend_without_same_developer_outcome":[{}],"issue_spend_without_same_developer_outcome_total":80,"unqualified_cost_share":0.25}}}`), &s); err != nil {
		t.Fatal(err)
	}
	got := checkJoinReadiness(s)
	if len(got) != 2 || got[0].status != statusOK || got[1].status != statusInfo || got[1].status.label() != "INFO" || !strings.Contains(got[1].detail, "80") {
		t.Fatalf("checks: %+v", got)
	}
}

func TestDoctorReadsSeparateCapturedDollars(t *testing.T) {
	seen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer read-token" {
			t.Error("missing read auth")
		}
		switch r.URL.Path {
		case "/api/v1/scores":
			_, _ = fmt.Fprintf(w, `{"price_table":{"version":%d}}`, store.ActivePriceTableInfo().Version)
		case "/api/v1/diagnostics/captured-dollars":
			seen = true
			_, _ = w.Write([]byte(`{"captured_cost_usd":10,"sample_size":1,"since":"2026-10-01T00:00:00Z","scope":"fleet"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	got := checkServer(context.Background(), srv.Client(), srv.URL, "read-token", time.Now())
	found := false
	for _, r := range got {
		if r.name == "captured-dollar diagnostics" {
			found = true
		}
	}
	if !seen || !found {
		t.Fatalf("doctor missed live receipt: %+v", got)
	}
}

func TestDoctorJoinReceiptsDoNotSilentlySkipOversizedResponses(t *testing.T) {
	for _, path := range []string{"/api/v1/scores", "/api/v1/diagnostics/captured-dollars"} {
		t.Run(path, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(strings.Repeat(" ", (1<<20)+1)))
			}))
			defer srv.Close()
			var got []checkResult
			if path == "/api/v1/scores" {
				got = checkServer(context.Background(), srv.Client(), srv.URL, "", time.Now())
			} else {
				got = readCapturedDollars(context.Background(), srv.Client(), srv.URL, "")
			}
			if len(got) != 1 || got[0].status != statusWarn || got[0].detail == "" {
				t.Fatalf("oversized receipt silently skipped: %+v", got)
			}
		})
	}
}
