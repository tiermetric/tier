package store

import (
	"context"
	"testing"
	"time"
)

func TestModelIsExact(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"claude-sonnet-4", true},                // exact table entry
		{"claude-sonnet-4-20250514", true},       // date suffix normalized off
		{"claude-sonnet-5", true},                // current Sonnet 5 — audited, not guessed (#268 refresh)
		{"claude-mythos-5", true},                // Claude 5 flagship (Glasswing) — audited, not guessed
		{"totally-made-up-model", false},         // flat fallback → guessed
		{"mystery-70b", false},                   // size-class heuristic → still a guess
		{"claude-sonnet-4@openrouter.ai", false}, // host-key separator → treated as unknown
	}
	for _, tt := range tests {
		if got := modelIsExact(tt.model); got != tt.want {
			t.Errorf("modelIsExact(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

// TestModelIsExactHost proves the host-aware form falls back to the model-only
// entry when no host-qualified rate is seeded (#268 not yet landed), so an audited
// model with a host set is NOT miscounted as unknown, while a genuinely unknown
// model stays unknown regardless of host.
func TestModelIsExactHost(t *testing.T) {
	if !modelIsExactHost("openrouter.ai", "claude-sonnet-4") {
		t.Errorf("a known model with a host should be exact (model-only fallback)")
	}
	if modelIsExactHost("openrouter.ai", "totally-made-up-model") {
		t.Errorf("an unknown model stays unknown regardless of host")
	}
}

// TestIsAuditedRate pins the exported wrapper (#465 review finding — a
// billing_mode-based "is this audited" check is a lying proxy on a
// self-hosted entry, since self_hosted_amortized is the billing_mode
// whether the entry was reached EXACTLY or by a size-class/flat GUESS). It
// must agree with modelIsExactHost byte for byte, since it delegates
// directly — this test exists so a future refactor that breaks that
// delegation fails here, not only in cmd/tierd/scorelog_test.go.
func TestIsAuditedRate(t *testing.T) {
	tests := []struct {
		host, model string
		want        bool
	}{
		{"", "claude-sonnet-4", true},               // exact model-only entry
		{"", "self-hosted-large", true},             // exact self-hosted entry — audited despite self_hosted_amortized billing_mode
		{"", "totally-made-up-model", false},        // flat fallback guess
		{"", "mystery-70b", false},                  // size-class heuristic guess
		{"openrouter.ai", "claude-sonnet-4", true},  // host-aware form falls back to the model-only entry
		{"openrouter.ai", "totally-made-up", false}, // unknown regardless of host
	}
	for _, tt := range tests {
		if got := IsAuditedRate(tt.host, tt.model); got != tt.want {
			t.Errorf("IsAuditedRate(%q, %q) = %v, want %v", tt.host, tt.model, got, tt.want)
		}
		if got, want := IsAuditedRate(tt.host, tt.model), modelIsExactHost(tt.host, tt.model); got != want {
			t.Errorf("IsAuditedRate(%q, %q) = %v, disagrees with modelIsExactHost = %v — the exported wrapper must delegate exactly", tt.host, tt.model, got, want)
		}
	}
}

func TestDeveloperFidelity(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(dev, source, fidelity, model string, cost int64, ts time.Time) TokenEvent {
		return TokenEvent{
			Developer: dev, IssueID: "issue-1", Model: model,
			InputTok: 100, CostMicro: cost, Source: source, Fidelity: fidelity, Timestamp: ts,
		}
	}
	events := []TokenEvent{
		// alice: two sources, mixed fidelity, one unknown-model cost.
		mk("alice", "jsonl", "realtime", "claude-sonnet-4", 1_000_000, now.Add(-1*time.Hour)),    // 7d + 30d, known
		mk("alice", "jsonl", "realtime", "mystery-model", 3_000_000, now.Add(-2*time.Hour)),      // 7d + 30d, unknown
		mk("alice", "proxy", "estimated", "claude-sonnet-4", 500_000, now.Add(-10*24*time.Hour)), // 30d only, known
		// bob: single source, all known, only 30d.
		mk("bob", "api", "daily", "gpt-4o", 2_000_000, now.Add(-20*24*time.Hour)),
		// carol: outside the 30d window entirely — should not count, but her last
		// event by source must still surface.
		mk("carol", "jsonl", "realtime", "claude-sonnet-4", 9_000_000, now.Add(-40*24*time.Hour)),
	}
	for _, e := range events {
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := db.DeveloperFidelity(ctx, now)
	if err != nil {
		t.Fatalf("DeveloperFidelity: %v", err)
	}
	byDev := map[string]DeveloperFidelitySignal{}
	for _, s := range got {
		byDev[s.Developer] = s
	}

	// alice: 2 events in 7d, 3 in 30d.
	a := byDev["alice"]
	if a.EventCount7d != 2 {
		t.Errorf("alice EventCount7d = %d, want 2", a.EventCount7d)
	}
	if a.EventCount30d != 3 {
		t.Errorf("alice EventCount30d = %d, want 3", a.EventCount30d)
	}
	if len(a.LastEventBySource) != 2 {
		t.Errorf("alice LastEventBySource = %v, want 2 sources", a.LastEventBySource)
	}
	// The jsonl last-seen must be the MAX (the -1h event), not the -2h min: a
	// regression returning MIN or an arbitrary row would flip a live shipper to
	// "looks dead". The ~1h gap makes the two unambiguous under microsecond rounding.
	if d := a.LastEventBySource["jsonl"].Sub(now.Add(-1 * time.Hour)); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("alice jsonl last = %v, want ~-1h (the max), off by %v", a.LastEventBySource["jsonl"], d)
	}
	if a.FidelityCounts["realtime"] != 2 || a.FidelityCounts["estimated"] != 1 {
		t.Errorf("alice FidelityCounts = %v", a.FidelityCounts)
	}
	// 30d total = 1M + 3M + 0.5M = 4.5M; unknown = 3M.
	if a.TotalCostMicro30d != 4_500_000 || a.UnknownCostMicro30d != 3_000_000 {
		t.Errorf("alice cost total=%d unknown=%d, want 4500000/3000000", a.TotalCostMicro30d, a.UnknownCostMicro30d)
	}

	// bob: all known → zero unknown cost.
	b := byDev["bob"]
	if b.EventCount30d != 1 || b.UnknownCostMicro30d != 0 {
		t.Errorf("bob count30d=%d unknown=%d, want 1/0", b.EventCount30d, b.UnknownCostMicro30d)
	}

	// carol: no events in 30d, but her last-seen source is still reported.
	c := byDev["carol"]
	if c.EventCount30d != 0 {
		t.Errorf("carol EventCount30d = %d, want 0 (outside window)", c.EventCount30d)
	}
	if d := c.LastEventBySource["jsonl"].Sub(now.Add(-40 * 24 * time.Hour)); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("carol jsonl last = %v, want ~-40d, off by %v", c.LastEventBySource["jsonl"], d)
	}
}

func TestDeveloperFidelityEmpty(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	got, err := db.DeveloperFidelity(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("DeveloperFidelity on empty DB: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no rows on empty DB, got %d", len(got))
	}
}
