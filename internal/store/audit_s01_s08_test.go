package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// S01-3: first-party model names routed through another host cannot offset
// that provider's org aggregate. Legacy unknown-host capture stays supported.
func TestAuditS01_3BaselineServingHost(t *testing.T) {
	for _, tc := range []struct{ provider, model string }{{"anthropic", "claude-sonnet-4"}, {"openai", "gpt-4o"}} {
		t.Run(tc.provider, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
			for _, host := range []string{"", tc.provider, " API." + tc.provider + ".COM ", "openrouter.ai", "bedrock", "localhost", "azure", "copilot"} {
				if err := db.InsertTokenEvent(ctx, TokenEvent{Developer: "alice", IssueID: "1", Model: tc.model,
					InputTok: 10, OutputTok: 20, CacheRead: 30, CacheWrite5m: 40, CacheWrite1h: 50,
					Source: "proxy", Host: host, Timestamp: day.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := db.CapturedTokensByDayModel(ctx, day, tc.provider)
			if err != nil {
				t.Fatal(err)
			}
			want := CostUsage{Input: 30, Output: 60, CacheRead: 90, CacheWrite5m: 120, CacheWrite1h: 150}
			if got[tc.model] != want {
				t.Errorf("baseline = %+v, want %+v (three first-party/legacy rows only)", got[tc.model], want)
			}
		})
	}
}

// S08-2: subscription capture is outside a provider's per-token API invoice.
func TestAuditS08_2BaselineExcludesSubscription(t *testing.T) {
	for _, tc := range []struct{ provider, model string }{{"anthropic", "claude-sonnet-4"}, {"openai", "gpt-4o"}} {
		t.Run(tc.provider, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
			for _, source := range []string{"jsonl", "proxy", "codex-rollout", "opencode"} {
				for _, mode := range []string{BillingPerToken, BillingSubscription, BillingSelfHostedAmortized} {
					if err := db.InsertTokenEvent(ctx, TokenEvent{Developer: "alice", IssueID: "1", Model: tc.model,
						InputTok: 10, OutputTok: 20, CacheRead: 30, CacheWrite5m: 40, CacheWrite1h: 50,
						Source: source, BillingMode: mode, Timestamp: day.Add(time.Hour)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := db.CapturedTokensByDayModel(ctx, day, tc.provider)
			if err != nil {
				t.Fatal(err)
			}
			want := CostUsage{Input: 40, Output: 80, CacheRead: 120, CacheWrite5m: 160, CacheWrite1h: 200}
			if got[tc.model] != want {
				t.Errorf("baseline = %+v, want %+v (per-token rows only)", got[tc.model], want)
			}
		})
	}
}

func TestPollerBaselineHostNormalization(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai"} {
		for _, tc := range []struct {
			name, suffix string
			want         bool
		}{
			{"lower_case", "", true},
			{"dns_dot", ".", true},
			{"https_port", ":443", true},
			{"dns_dot_and_port", ".:443", true},
			{"other_port", ":8443", false},
			{"hostname_suffix", ".evil.example", false},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				ctx := context.Background()
				day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
				model := "gpt-4o"
				if provider == "anthropic" {
					model = "claude-sonnet-4"
				}
				host := "api." + provider + ".com" + tc.suffix
				if tc.name == "lower_case" {
					host = strings.ToUpper(host)
				}
				// Bypass insert-time normalization to also cover existing stored hosts.
				_, err := db.db.ExecContext(ctx, `INSERT INTO token_events
					(developer, issue_id, model, input_tok, cost_micro, source, fidelity, host, ts)
					VALUES ('alice', '1', ?, 10, 1, 'proxy', 'realtime', ?, ?)`, model, host, day.Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				got, err := db.CapturedTokensByDayModel(ctx, day, provider)
				if err != nil {
					t.Fatal(err)
				}
				_, present := got[model]
				if present != tc.want {
					t.Errorf("host %q included = %v, want %v", host, present, tc.want)
				}
			})
		}
	}
}
