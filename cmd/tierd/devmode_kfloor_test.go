package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/scoring"
)

func TestServe_RefusesNonSyntheticDeveloperMode(t *testing.T) {
	for _, token := range []string{"", "devmode-test-admin"} {
		for _, addr := range []string{":8080", "0.0.0.0:8080", "127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
			for _, source := range []string{"cli", "env", "config"} {
				for _, readOnly := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/readOnly=%t/token=%t", addr, source, readOnly, token != ""), func(t *testing.T) {
						// A directory bounds startup if the guard regresses. Tokenless loopback
						// also pins that developer refusal precedes validateBind.
						args := []string{"serve", "--addr", addr, "--db", t.TempDir()}
						if token != "" {
							args = append(args, "--api-token", token)
						}
						if readOnly {
							args = append(args, "--read-only")
						}
						env := map[string]string{}
						switch source {
						case "cli":
							args = append(args, "--aggregation", "developer")
						case "env":
							env["TIER_AGGREGATION"] = "developer"
						case "config":
							path := filepath.Join(t.TempDir(), "config.yaml")
							if err := os.WriteFile(path, []byte("aggregation: developer\n"), 0600); err != nil {
								t.Fatal(err)
							}
							args = append(args, "--config", path)
						}
						code, out := runSecretEnvChild(t, env, args...)
						if code != 1 || !strings.Contains(out, "non-synthetic developer mode is disabled") || !strings.Contains(out, "#826") {
							t.Fatalf("exit=%d; want developer-mode refusal naming #826, got:\n%s", code, out)
						}
					})
				}
			}
		}
	}
}

func TestScore_IssueAttributedClaudeCodeSpend(t *testing.T) {
	cases := []struct {
		name   string
		events []collector.TokenEvent
		want   string
	}{
		{"mixed", []collector.TokenEvent{{IssueID: "42", CostMicro: 1_000_000}, {IssueID: "unattributed:main", CostMicro: 3_000_000}}, "25.0%"},
		{"attributed", []collector.TokenEvent{{IssueID: "42", CostMicro: 1}}, "100.0%"},
		{"unattributed", []collector.TokenEvent{{IssueID: "unattributed", CostMicro: 1}}, "0.0%"},
		{"empty issue", []collector.TokenEvent{{CostMicro: 1}}, "0.0%"},
		{"zero spend", []collector.TokenEvent{{IssueID: "42"}}, "unavailable (no spend)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printCostReport(&out, tc.events, time.Now())
			// Pin computed values while allowing the explanatory prose to change.
			pct := regexp.MustCompile(`: ([0-9]+\.[0-9]+%|unavailable \(no spend\))`).FindStringSubmatch(out.String())
			if len(pct) != 2 || pct[1] != tc.want {
				t.Fatalf("share=%v, want %s; report:\n%s", pct, tc.want, out.String())
			}
			floor := regexp.MustCompile(`([0-9]+) outcomes,.*\$([0-9]+\.[0-9]+)`).FindStringSubmatch(out.String())
			if len(floor) != 3 {
				t.Fatalf("ranking constants missing:\n%s", out.String())
			}
			outcomes, err := strconv.Atoi(floor[1])
			if err != nil {
				t.Fatal(err)
			}
			cost, err := strconv.ParseFloat(floor[2], 64)
			if err != nil {
				t.Fatal(err)
			}
			if outcomes != scoring.MinRankedOutcomes || cost != scoring.MinRankedCostUSD {
				t.Errorf("ranking constants=(%d, %.2f), want (%d, %.2f)", outcomes, cost, scoring.MinRankedOutcomes, scoring.MinRankedCostUSD)
			}
		})
	}
}
