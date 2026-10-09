package opencode

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// TestOllamaCloudIsExcludedLoudlyWithACount is the explicit ollama-cloud
// decision (#719), asserted end to end.
//
// 🔴 SILENCE WAS NOT AN OPTION AND NEITHER IS AN UNCOUNTED EXCLUSION. Ollama's
// cloud tier publishes no per-token rate, so capturing it would route the
// measured ~342.3M tokens in the maintainer's store through the guessed
// self-hosted-medium fallback ($0.50/M) and inject roughly $171 of invented spend
// into the denominator of a cost-per-outcome metric. Excluding it silently is the
// other failure: the operator's totals would simply be smaller than their real
// usage, with nothing in the logs to say why.
//
// So the contract is BOTH halves, and both are asserted here: an INFO at startup
// that NAMES the exclusion and its reason, and a per-provider COUNT after every
// scan so the size of what is left out is visible.
func TestOllamaCloudIsExcludedLoudlyWithACount(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")
	completed := goldenCompleted

	rows := []msgRow{
		buildRow(t, msgSpec{
			ID: "msg_zai", SessionID: "s1", Provider: "zai-coding-plan", Model: "glm-5.3",
			Cwd: cwd, Created: completed - 1000, Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		}),
		buildRow(t, msgSpec{
			ID: "msg_oll1", SessionID: "s2", Provider: "ollama-cloud", Model: "kimi-k2.7-code",
			Cwd: cwd, Created: completed - 2000, Completed: &completed, TimeUpdated: completed + 1,
			Tokens: autoTotal(252_040_606, 911_540, 0, 0, 0),
		}),
		buildRow(t, msgSpec{
			ID: "msg_oll2", SessionID: "s3", Provider: "ollama-cloud", Model: "glm-5.2",
			Cwd: cwd, Created: completed - 3000, Completed: &completed, TimeUpdated: completed + 2,
			Tokens: autoTotal(87_754_404, 887_910, 0, 0, 0),
		}),
	}
	dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: rows})

	logger, cap := newTestLogger()
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g := installGuessRecorder(t)
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("only the priced provider may be captured; got %d events (%v)", len(events), events)
	}
	if events[0].Model != "glm-5.3" {
		t.Errorf("captured %q; ollama-cloud rows must not reach the store", events[0].Model)
	}
	// The whole point: nothing was guessed. Had the two ollama rows been captured,
	// 341.6M tokens would have priced at the $0.50/M fallback.
	if n, micro := g.snapshot(); n != 0 || micro != 0 {
		t.Errorf("the excluded rows still reached the pricing path: %d guessed events / %.0f guessed micro-dollars", n, micro)
	}

	// Half one: the startup INFO names the exclusion and its reason.
	policy := cap.find(slog.LevelInfo, "capture policy")
	if len(policy) != 1 {
		t.Fatalf("want exactly one capture-policy INFO at startup; got %d. Records:%s", len(policy), cap.dump())
	}
	if got := attrOf(policy[0], "excluded_providers"); !strings.Contains(got, "ollama-cloud") {
		t.Errorf("excluded_providers = %q, must name ollama-cloud", got)
	}
	// EVERY named exclusion must carry its reason, checked against the policy
	// itself rather than against one hardcoded key. The reasons used to be two
	// literal map lookups in the log call, so a third named exclusion would have
	// been announced with no reason attached — which defeats the whole
	// "both halves together are the disclosure" contract while still passing a
	// test that only looked for ollama-cloud.
	why := attrOf(policy[0], "excluded_why")
	for _, id := range namedExclusions() {
		if !strings.Contains(why, id+"=") {
			t.Errorf("excluded_why = %q, but %q is a named exclusion with no reason attached", why, id)
		}
		if r := providerPolicy[id].why; r == "" || !strings.Contains(why, r) {
			t.Errorf("excluded_why does not carry %q's recorded reason %q; got %q", id, r, why)
		}
	}
	if !strings.Contains(why, "no per-token rate") {
		t.Errorf("the ollama-cloud reason must say WHY it is excluded, not just that it is; excluded_why = %q", why)
	}
	if got := attrOf(policy[0], "priced_providers"); got != "zai-coding-plan" {
		t.Errorf("priced_providers = %q, want zai-coding-plan", got)
	}

	// Half two: the per-provider COUNT after the scan.
	summary := cap.find(slog.LevelInfo, "opencode scan complete")
	if len(summary) != 1 {
		t.Fatalf("want one scan summary; got %d. Records:%s", len(summary), cap.dump())
	}
	if got := attrOf(summary[0], "excluded_by_provider"); !strings.Contains(got, `"ollama-cloud"=2`) {
		t.Errorf("excluded_by_provider = %q, want it to report ollama-cloud=2 — an exclusion with no count hides its own size", got)
	}
}

// TestZaiSingletonIsNamedNotDefaulted. The bare `zai` provider exists on exactly
// one row carrying zero tokens: a failed configuration attempt. It behaves
// identically to an unclassified provider, so this test is about the DIAGNOSIS,
// which is the only thing the entry buys.
//
// Without the named entry, the next operator to read "unclassified provider: zai"
// would reasonably conclude the collector forgot to support a route and add it —
// and since no `glm-5.3@zai` price row exists or should, that would silently
// price real GLM traffic at the $0.50/M guessed fallback.
func TestZaiSingletonIsNamedNotDefaulted(t *testing.T) {
	decision, why := classifyProvider("zai")
	if decision != providerExcludedNoRate {
		t.Errorf("classifyProvider(\"zai\") decision = %v, want providerExcludedNoRate (a NAMED exclusion, not the unclassified default)", decision)
	}
	if !strings.Contains(why, "failed config attempt") {
		t.Errorf("the `zai` entry must record WHY it is excluded so nobody re-adds it; why = %q", why)
	}
	// And it must appear in the startup disclosure alongside ollama-cloud.
	if !containsStr(namedExclusions(), "zai") {
		t.Errorf("namedExclusions() = %v, must include \"zai\"", namedExclusions())
	}
	// Control arm: the real coding plan is NOT excluded, so the two strings are
	// not being confused for one another.
	if d, _ := classifyProvider("zai-coding-plan"); d != providerPriced {
		t.Errorf("classifyProvider(%q) = %v, want providerPriced", "zai-coding-plan", d)
	}
}

// TestUnclassifiedProviderIsSkippedAndWarnedOnce: the default arm fails CLOSED
// (capture nothing) and says so — once per provider, not once per row.
func TestUnclassifiedProviderIsSkippedAndWarnedOnce(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")
	completed := goldenCompleted
	var rows []msgRow
	for i := 0; i < 3; i++ {
		rows = append(rows, buildRow(t, msgSpec{
			ID: string(rune('a'+i)) + "_msg", SessionID: "s", Provider: "some-future-provider",
			Cwd: cwd, Created: completed - 1000, Completed: &completed, TimeUpdated: completed + int64(i),
			Tokens: autoTotal(1000, 100, 50, 200, 0),
		}))
	}
	dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: rows})
	logger, cap := newTestLogger()
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g := installGuessRecorder(t)
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("an unclassified provider must capture NOTHING (fail closed); got %d events", len(events))
	}
	if n, _ := g.snapshot(); n != 0 {
		t.Errorf("an unclassified provider must never reach the pricing path; %d guesses fired", n)
	}
	warns := cap.find(slog.LevelWarn, "unclassified provider")
	if len(warns) != 1 {
		t.Errorf("want ONE warning for three rows of the same provider; got %d. Records:%s", len(warns), cap.dump())
	}
}

// TestEveryPricedProviderHasAPriceRow is the tripwire providers.go's own comment
// promises: admitting a provider to the priced arm without a
// `<model>@<providerID>` row behind it silently prices its traffic at the guessed
// self-hosted-medium fallback.
//
// It checks the CONSEQUENCE, not the table's contents: for each priced provider it
// prices a probe usage through the real pricing path with the built-in table
// active and requires the result to be an AUDITED hit, using the guessed-cost
// counter as the detector. A test that merely looked up a key would have to
// hard-code which models to look for.
func TestEveryPricedProviderHasAPriceRow(t *testing.T) {
	withZaiPrices(t)
	priced := pricedProviders()
	if len(priced) == 0 {
		t.Fatal("pricedProviders() is empty — this test would pass vacuously")
	}
	// The models this collector is known to see on each priced route.
	models := map[string][]string{"zai-coding-plan": {"glm-5.3"}}
	for _, p := range priced {
		ms, ok := models[p]
		if !ok {
			t.Errorf("provider %q is in the priced arm but this test knows no model for it; add one so its price rows are actually exercised", p)
			continue
		}
		for _, m := range ms {
			g := installGuessRecorder(t)
			cost, mode := store.ComputeCostHost(p, m, store.CostUsage{Input: 1_000_000})
			if n, _ := g.snapshot(); n != 0 {
				t.Errorf("%s@%s has NO audited price row: it priced at the guessed fallback. Add the row to the price table BEFORE admitting the provider", m, p)
			}
			if cost <= 0 {
				t.Errorf("%s@%s priced 1M input tokens at %d micro-dollars", m, p, cost)
			}
			if mode != store.BillingPerToken {
				t.Errorf("%s@%s billing_mode = %q, want %q", m, p, mode, store.BillingPerToken)
			}
		}
	}
}

// TestCollectorNameAndSource keeps the collector's identity aligned with the
// source constant the shipping allowlist answers for.
func TestCollectorNameAndSource(t *testing.T) {
	c, err := New(Config{DBPath: "/nonexistent/x.db", Repos: []RepoTarget{{Path: "/tmp"}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Name() != collector.SourceOpencode {
		t.Errorf("Name() = %q, want %q", c.Name(), collector.SourceOpencode)
	}
	if !collector.ShippableSource(collector.SourceOpencode) {
		t.Error("SourceOpencode is not shippable; Opencode outcomes would arrive at a central tierd with its cost missing, and the work would read as FREE (#492)")
	}
}

// TestNewRejectsAConfigThatCouldNeverCapture: fail fast, not silently disabled.
func TestNewRejectsAConfigThatCouldNeverCapture(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no repo target must fail: no target means no message can ever be attributed")
	}
	if _, err := New(Config{Repos: []RepoTarget{{Path: "  "}}}); err == nil {
		t.Error("New with a blank repo path must fail")
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
