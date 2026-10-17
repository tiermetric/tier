package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

const scoreUnknownPricingLabel = "share of tokens with an unknown pricing basis or without an exact entry in the current price table"

func TestScore_UnknownPricingExplicitBasis(t *testing.T) {
	loadEmbeddedPriceTable(t)
	for _, tc := range []struct {
		basis string
		want  float64
	}{
		{"token_derived", 0},
		{"guessed_estimate", 1},
		{store.PricingAggregateApprox, 1},
		{store.PricingLegacyAggregate, 1},
		{"basis_unavailable", 1},
	} {
		t.Run(tc.basis, func(t *testing.T) {
			var counts store.PricingTokenCounts
			addScorePricingTokens(&counts, collector.TokenEvent{Model: "claude-sonnet-4", InputTok: 100, PricingBasis: tc.basis})
			if counts.Share() != tc.want || (counts.CostLabel() == "estimated cost") != (tc.want > 0) {
				t.Fatalf("counts=%+v label=%q, want share=%v", counts, counts.CostLabel(), tc.want)
			}
		})
	}
}

func TestRunScore_UnknownPricingShareAndLabel(t *testing.T) {
	loadEmbeddedPriceTable(t)
	repo, claudeDir := initGitRepo(t), t.TempDir()
	for _, tc := range []struct {
		model, share string
		estimated    bool
	}{
		{"claude-sonnet-4", "0.00%", false},
		{"mystery-model", "100.00%", true},
		{"mystery-70b", "100.00%", true},
		{"self-hosted-medium", "0.00%", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			writeSessionFixtureModel(t, claudeDir, repo, tc.model)
			out := captureStdout(t, func() {
				runScore([]string{"--repo", repo, "--claude-dir", claudeDir, "--since", "2026-01-01"})
			})
			if !strings.Contains(out, scoreUnknownPricingLabel+": "+tc.share) {
				t.Errorf("missing share and exact label:\n%s", out)
			}
			if strings.Contains(out, "estimated cost") != tc.estimated {
				t.Errorf("estimated cost presence must be %v:\n%s", tc.estimated, out)
			}
		})
	}
}

func TestScore_UnknownPricingTokenWeightsAndCostRows(t *testing.T) {
	loadEmbeddedPriceTable(t)
	events := []collector.TokenEvent{
		{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", InputTok: 100, CostMicro: 9_000_000},
		{Developer: "bob", IssueID: "issue-2", Model: "mystery", OutputTok: 100, CacheRead: 100, CacheWrite5m: 100, CacheWrite1h: 100, CostMicro: 1},
		// Accepted spec consequence: a guessed cost with zero tokens has zero
		// token share, so both developer and issue rows omit the cost label.
		{Developer: "carol", IssueID: "issue-3", Model: "mystery", CostMicro: 1_000_000},
	}
	var summary, issues bytes.Buffer
	printCostReport(&summary, events, time.Now())
	printIssueCosts(&issues, events)
	if !strings.Contains(summary.String(), scoreUnknownPricingLabel+": 80.00%") {
		t.Errorf("share must weight all five token classes, not dollars or rows:\n%s", summary.String())
	}
	for _, tc := range []struct {
		out, key  string
		estimated bool
	}{
		{summary.String(), "alice", false},
		{summary.String(), "bob", true},
		{summary.String(), "carol", false},
		{summary.String(), "TOTAL", true},
		{issues.String(), "issue-1", false},
		{issues.String(), "issue-2", true},
		{issues.String(), "issue-3", false},
	} {
		found := false
		for _, line := range strings.Split(tc.out, "\n") {
			if strings.HasPrefix(line, tc.key+" ") {
				found = true
				if strings.Contains(line, "estimated cost") != tc.estimated {
					t.Errorf("row %s: estimated must be %v: %s", tc.key, tc.estimated, line)
				}
			}
		}
		if !found {
			t.Errorf("cost row %s missing:\n%s", tc.key, tc.out)
		}
	}
}

func TestScore_UnknownPricingPercentagePrecision(t *testing.T) {
	loadEmbeddedPriceTable(t)
	for _, tc := range []struct {
		name           string
		known, unknown int
		want           string
	}{
		{"empty", 0, 0, "0.00%"},
		{"known", 100, 0, "0.00%"},
		{"tiny", 999_999, 1, "<0.01%"},
		{"rounds-up", 19_999, 1, "<0.01%"},
		{"below-boundary", 10_000, 1, "<0.01%"},
		{"boundary", 9_999, 1, "0.01%"},
		{"all-unknown", 0, 100, "100.00%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printCostReport(&out, []collector.TokenEvent{
				{Developer: "alice", Model: "claude-sonnet-4", InputTok: tc.known},
				{Developer: "alice", Model: "mystery", InputTok: tc.unknown},
			}, time.Now())
			_, diagnostic, found := strings.Cut(out.String(), scoreUnknownPricingLabel+": ")
			if !found {
				t.Fatalf("pricing diagnostic missing: %s", out.String())
			}
			fields := strings.Fields(diagnostic)
			if len(fields) == 0 || fields[0] != tc.want {
				t.Errorf("percentage = %q, want %q", diagnostic, tc.want)
			}
		})
	}
}
