package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestServe_Event3ScoringDefaultsOff(t *testing.T) {
	// The production serve entry point must register the default-off flag.
	found := false
	ast.Inspect(serveBody(t), func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || len(c.Args) < 2 {
			return true
		}
		lit, ok := c.Args[0].(*ast.BasicLit)
		if ok && lit.Value == `"event3-scoring"` {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("serve has no default-off event3-scoring flag")
	}
	path := filepath.Join(t.TempDir(), "tier.db")
	if got := u4ServeRubric(t, path, "", "", "--event3-scoring"); got != 2 {
		t.Fatal(got)
	}
	// A restart with no enabling setting restores legacy even after an on run.
	if got := u4ServeRubric(t, path, "", ""); got != 1 {
		t.Fatal("restart auto-enabled scoring", got)
	}

}
func TestServe_Event3FlagPrecedence(t *testing.T) {
	found := false
	ast.Inspect(serveBody(t), func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || exprName(c.Fun) != "applyBoolFromConfig" || len(c.Args) != 5 {
			return true
		}
		lit, ok := c.Args[3].(*ast.BasicLit)
		if ok && lit.Value == `"event3-scoring"` {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("serve does not apply Event 3 CLI > env > config > default precedence")
	}
	for _, tc := range []struct {
		name, env, cfg string
		args           []string
		want           int
	}{
		{"config", "", "true", nil, 2},
		{"env false over config true", "false", "true", nil, 1},
		{"env true over config false", "true", "false", nil, 2},
		{"CLI false over env/config true", "true", "true", []string{"--event3-scoring=false"}, 1},
		{"CLI true over env/config false", "false", "false", []string{"--event3-scoring"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := u4ServeRubric(t, filepath.Join(t.TempDir(), "tier.db"), tc.env, tc.cfg, tc.args...); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}

}

// This subprocess enters the actual serve parser, config resolver, store flip,
// mux and signal shutdown. It runs in the default (non-integration) test gate.
func TestServe_Event3Child(t *testing.T) {
	if args := os.Getenv("TIER_U4_CHILD_ARGS"); args != "" {
		os.Exit(runServe(strings.Split(args, "\n")))
	}
}
func u4ServeRubric(t *testing.T, dbPath, envValue, cfgValue string, args ...string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	text := "aggregation: team\n"
	if cfgValue != "" {
		text += "outcomes:\n  event3_scoring: " + cfgValue + "\n"
	}
	if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	// The long serve grace keeps background sealing away from the fixture months.
	childArgs := append([]string{"--addr", addr, "--db", dbPath, "--config", cfg, "--aggregation", "team", "--report-grace", "876000h"}, args...)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestServe_Event3Child$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TIER_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "TIER_U4_CHILD_ARGS="+strings.Join(childArgs, "\n"))
	if envValue != "" {
		cmd.Env = append(cmd.Env, "TIER_EVENT3_SCORING="+envValue)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			if err := <-done; err != nil {
				t.Logf("tierd exit after kill: %v", err)
			}
		}
	}()
	client := &http.Client{Timeout: time.Second}
	month := ""
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			exited = true
			t.Fatalf("serve exited: %v %s", err, stderr.String())
		default:
		}
		if month == "" {
			resp, err := client.Get("http://" + addr + "/api/v1/livez")
			if err != nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("livez response %d", resp.StatusCode)
			}
			month = u4SealScoresFixture(t, dbPath)
		}
		resp, err := client.Get("http://" + addr + "/api/v1/scores?period=" + month)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var b struct {
			Rubric struct {
				Version int `json:"version"`
			} `json:"rubric"`
		}
		err = json.NewDecoder(resp.Body).Decode(&b)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("score response %d: %v", resp.StatusCode, err)
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			exited = true
			if err != nil {
				t.Fatal(err, stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("shutdown timeout")
		}
		return b.Rubric.Version
	}
	t.Fatal("serve did not start")
	return 0
}

func u4SealScoresFixture(t *testing.T, dbPath string) string {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	latest, exists, err := db.LatestSealedPeriod(ctx, "month")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		start = latest.AddDate(0, 1, 0)
	}
	month := start.Format("2006-01")
	period, err := api.ParsePeriod(month)
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", api.RateLimitConfig{}, api.WithStoredScoringPolicy())
	h.SetAggregation(scoring.AggregationTeam, scoring.DefaultKAnonymity)
	body, err := h.RecomputeSealedBody(ctx, period)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := db.EraseEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := db.CorrectionScoringPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policyName := "legacy"
	if policy.Enabled {
		policyName = "event3"
	}
	_, won, err := db.SealReport(ctx, store.SealedReport{
		ScoringPolicy: &policyName, CorrectionRuleVersion: &policy.RuleVersion, RubricVersion: &policy.RubricVersion, CorrectionRevision: &policy.Revision,
		Level: "team", PeriodSize: "month", PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
		K: scoring.DefaultKAnonymity, ConfigDigest: "event3-test", Body: body,
		BodyDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(body)), ToolVersion: "test", ToolCommit: "test",
	}, nil, nil, store.SealCheck{EraseEpoch: epoch, Floor: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil || !won {
		t.Fatalf("seal scores fixture: won=%v err=%v", won, err)
	}
	return month
}
