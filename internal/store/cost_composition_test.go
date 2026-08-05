package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// approxEq compares two shares within a small epsilon — shares are float ratios
// and exact equality would be brittle.
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// --- BuildCostComposition (pure, DB-free) -----------------------------------

// TestBuildCostComposition_Empty pins the empty-input contract: a zero-value
// composition whose ByModel is a non-nil empty slice (so the API's
// omit-when-empty check keys on TotalCostMicro, not a nil slice) and every share
// is 0 with no divide-by-zero.
func TestBuildCostComposition_Empty(t *testing.T) {
	c := BuildCostComposition(nil)
	if c.TotalCostMicro != 0 || c.AttributedCostMicro != 0 || c.UnattributedCostMicro != 0 {
		t.Errorf("empty: costs = %d/%d/%d, want all 0", c.TotalCostMicro, c.AttributedCostMicro, c.UnattributedCostMicro)
	}
	if c.UnattributedShare != 0 || c.CacheReadShare != 0 || c.PremiumModelShare != 0 {
		t.Errorf("empty: shares = %v/%v/%v, want all 0", c.UnattributedShare, c.CacheReadShare, c.PremiumModelShare)
	}
	if c.ByModel == nil {
		t.Error("empty: ByModel is nil, want non-nil empty slice")
	}
	if len(c.ByModel) != 0 {
		t.Errorf("empty: ByModel has %d rows, want 0", len(c.ByModel))
	}
}

// TestBuildCostComposition_Reconciles is the headline invariant: the
// decomposition sums to the total with no residual bucket — attributed +
// unattributed == total, and the per-model costs sum to total.
func TestBuildCostComposition_Reconciles(t *testing.T) {
	rows := []ModelClassCost{
		{Host: HostUnknown, Model: "claude-opus-4-8", CostMicro: 100_000, UnattributedCostMicro: 0},
		{Host: HostUnknown, Model: "claude-sonnet-4", CostMicro: 40_000, UnattributedCostMicro: 0},
		{Host: HostUnknown, Model: "claude-sonnet-4", CostMicro: 10_000, UnattributedCostMicro: 10_000},
	}
	c := BuildCostComposition(rows)

	if c.TotalCostMicro != 150_000 {
		t.Errorf("total = %d, want 150000", c.TotalCostMicro)
	}
	if c.AttributedCostMicro+c.UnattributedCostMicro != c.TotalCostMicro {
		t.Errorf("attributed(%d)+unattributed(%d) != total(%d)", c.AttributedCostMicro, c.UnattributedCostMicro, c.TotalCostMicro)
	}
	if c.UnattributedCostMicro != 10_000 {
		t.Errorf("unattributed = %d, want 10000", c.UnattributedCostMicro)
	}
	var sumModel int64
	var sumShare float64
	for _, m := range c.ByModel {
		sumModel += m.CostMicro
		sumShare += m.Share
	}
	if sumModel != c.TotalCostMicro {
		t.Errorf("sum(by_model cost) = %d, want %d (no residual bucket)", sumModel, c.TotalCostMicro)
	}
	if !approxEq(sumShare, 1.0) {
		t.Errorf("sum(by_model share) = %v, want 1.0", sumShare)
	}
}

// TestBuildCostComposition_ModelFoldSortAndShares pins that raw rows fold by
// normalized model, that the by-model list is sorted by descending cost, and
// that per-model shares are correct. The two sonnet rows (one raw-cased) must
// collapse onto one normalized "claude-sonnet-4" bucket.
func TestBuildCostComposition_ModelFoldSortAndShares(t *testing.T) {
	rows := []ModelClassCost{
		{Host: HostUnknown, Model: "claude-sonnet-4", CostMicro: 40_000},
		{Host: HostUnknown, Model: "claude-opus-4-8", CostMicro: 100_000},
		{Host: HostUnknown, Model: "CLAUDE-SONNET-4", CostMicro: 10_000}, // folds via NormalizeModel (lowercased)
	}
	c := BuildCostComposition(rows)

	if len(c.ByModel) != 2 {
		t.Fatalf("ByModel has %d rows, want 2 (sonnet folded)", len(c.ByModel))
	}
	// Sorted by descending cost: opus (100k) then sonnet (50k).
	if c.ByModel[0].Model != "claude-opus-4-8" || c.ByModel[0].CostMicro != 100_000 {
		t.Errorf("row[0] = %+v, want claude-opus-4-8 @ 100000", c.ByModel[0])
	}
	if c.ByModel[1].Model != "claude-sonnet-4" || c.ByModel[1].CostMicro != 50_000 {
		t.Errorf("row[1] = %+v, want claude-sonnet-4 @ 50000 (folded)", c.ByModel[1])
	}
	if !approxEq(c.ByModel[0].Share, 100_000.0/150_000.0) {
		t.Errorf("opus share = %v, want %v", c.ByModel[0].Share, 100_000.0/150_000.0)
	}
	if !approxEq(c.ByModel[1].Share, 50_000.0/150_000.0) {
		t.Errorf("sonnet share = %v, want %v", c.ByModel[1].Share, 50_000.0/150_000.0)
	}
}

// TestBuildCostComposition_PremiumAndCacheShares pins the two headline levers.
// premium_model_share is the SPEND share on premium-tier models (opus premium,
// sonnet not); cache_read_share is cache_read / (input + cache_read +
// cache_write), an input-side ratio that excludes output tokens.
func TestBuildCostComposition_PremiumAndCacheShares(t *testing.T) {
	rows := []ModelClassCost{
		{Host: HostUnknown, Model: "claude-opus-4-8", CostMicro: 100_000, InputTok: 1000, OutputTok: 200, CacheRead: 3000, CacheWrite5m: 500},
		{Host: HostUnknown, Model: "claude-sonnet-4", CostMicro: 50_000, InputTok: 2500, OutputTok: 400, CacheRead: 1000},
	}
	c := BuildCostComposition(rows)

	// premium = opus only: 100000 / 150000.
	if !approxEq(c.PremiumModelShare, 100_000.0/150_000.0) {
		t.Errorf("premium_model_share = %v, want %v", c.PremiumModelShare, 100_000.0/150_000.0)
	}
	if !c.ByModel[0].Premium {
		t.Errorf("opus row Premium = false, want true")
	}
	// The sonnet row must be non-premium.
	for _, m := range c.ByModel {
		if m.Model == "claude-sonnet-4" && m.Premium {
			t.Errorf("sonnet row Premium = true, want false")
		}
	}
	// cache_read = 4000; input-side = input(3500) + cache_read(4000) + cw5m(500) = 8000.
	wantCache := 4000.0 / 8000.0
	if !approxEq(c.CacheReadShare, wantCache) {
		t.Errorf("cache_read_share = %v, want %v", c.CacheReadShare, wantCache)
	}
	// Output tokens are NOT in the cache denominator — proven by the share above
	// being 0.5 despite 600 output tokens present.
	if c.ByClass.OutputTok != 600 {
		t.Errorf("by_class output_tok = %d, want 600", c.ByClass.OutputTok)
	}
}

// TestBuildCostComposition_CacheWrite1hInDenominator pins that the 1h cache-write
// bucket is not dropped: it must feed both the by_class.cache_write total (via the
// API's 5m+1h sum) and the cache_read_share denominator. A bug that omitted the 1h
// term would shift the share upward and undercount cache_write.
func TestBuildCostComposition_CacheWrite1hInDenominator(t *testing.T) {
	rows := []ModelClassCost{
		// input 1000 + cache_read 2000 + cw5m 500 + cw1h 1500 => input-side 5000.
		{Host: HostUnknown, Model: "claude-sonnet-4", CostMicro: 10_000, InputTok: 1000, CacheRead: 2000, CacheWrite5m: 500, CacheWrite1h: 1500},
	}
	c := BuildCostComposition(rows)

	if c.ByClass.CacheWrite1h != 1500 {
		t.Errorf("by_class cw1h = %d, want 1500", c.ByClass.CacheWrite1h)
	}
	// cache_read_share = 2000 / (1000 + 2000 + 500 + 1500) = 2000/5000 = 0.4.
	if !approxEq(c.CacheReadShare, 0.4) {
		t.Errorf("cache_read_share = %v, want 0.4 (1h bucket in denominator)", c.CacheReadShare)
	}
	// Dropping the 1h term would give 2000/3500 ~= 0.571 — guard against that.
	if approxEq(c.CacheReadShare, 2000.0/3500.0) {
		t.Error("cache_read_share denominator is missing the cache_write_1h term")
	}
}

// TestBuildCostComposition_ZeroCostNonZeroTokens pins the layering at a $0-cost,
// nonzero-token window. The builder still computes the TOKEN-based cache_read_share
// (its denominator is token counts, guarded on inputSide > 0, NOT on cost), while
// the COST-based shares (premium, unattributed) stay 0 with no divide-by-zero. The
// omission of the whole sidecar for a $0 window is therefore purely an API-layer
// decision (newCostCompositionJSON keys on TotalCostMicro == 0), asserted in the
// api package — not something the builder does. Unreachable today (priced tokens
// cost > 0); pinned so the coupling is a conscious choice if a zero-rate source is
// ever added.
func TestBuildCostComposition_ZeroCostNonZeroTokens(t *testing.T) {
	rows := []ModelClassCost{
		{Host: HostUnknown, Model: "claude-sonnet-4", CostMicro: 0, InputTok: 1000, CacheRead: 3000},
	}
	c := BuildCostComposition(rows)
	if c.TotalCostMicro != 0 {
		t.Fatalf("total = %d, want 0", c.TotalCostMicro)
	}
	// Cost-based shares are 0 (guarded on total > 0); the token-based cache share
	// is still computed: 3000 / (1000 + 3000) = 0.75.
	if c.PremiumModelShare != 0 || c.UnattributedShare != 0 {
		t.Errorf("cost-based shares = premium %v / unattributed %v, want both 0 at zero total", c.PremiumModelShare, c.UnattributedShare)
	}
	if !approxEq(c.CacheReadShare, 0.75) {
		t.Errorf("cache_read_share = %v, want 0.75 (token-based, computed even at zero cost)", c.CacheReadShare)
	}
	if c.ByClass.InputTok != 1000 || c.ByClass.CacheRead != 3000 {
		t.Errorf("by_class = %+v, want input 1000 / cache_read 3000", c.ByClass)
	}
}

// TestBuildCostComposition_GuessedModelNotPremium pins, THROUGH the builder, that
// an unknown/guessed model's spend is excluded from premium_model_share — the
// classification runs per row via the host-aware IsPremiumModel, and a guessed
// self-hosted fallback is never premium.
func TestBuildCostComposition_GuessedModelNotPremium(t *testing.T) {
	rows := []ModelClassCost{
		{Host: HostUnknown, Model: "claude-opus-4-8", CostMicro: 60_000},     // premium
		{Host: HostUnknown, Model: "some-unpriced-model", CostMicro: 40_000}, // guessed, not premium
	}
	c := BuildCostComposition(rows)
	// Only the opus spend counts: 60000 / 100000.
	if !approxEq(c.PremiumModelShare, 0.6) {
		t.Errorf("premium_model_share = %v, want 0.6 (guessed model excluded)", c.PremiumModelShare)
	}
	for _, m := range c.ByModel {
		if m.Model == "some-unpriced-model" && m.Premium {
			t.Error("guessed model classified premium, want non-premium")
		}
	}
}

// TestBuildCostComposition_HostNotCollapsed pins #300: the same open-weights
// model served from two hosts stays two by-model rows, never pooled onto the
// weights.
func TestBuildCostComposition_HostNotCollapsed(t *testing.T) {
	rows := []ModelClassCost{
		{Host: "openrouter.ai", Model: "meta-llama/llama-3.3-70b-instruct", CostMicro: 30_000},
		{Host: "api.together.ai", Model: "meta-llama/llama-3.3-70b-instruct", CostMicro: 20_000},
	}
	c := BuildCostComposition(rows)
	if len(c.ByModel) != 2 {
		t.Fatalf("ByModel has %d rows, want 2 (hosts not collapsed)", len(c.ByModel))
	}
	hosts := map[string]bool{}
	for _, m := range c.ByModel {
		hosts[m.Host] = true
	}
	if !hosts["openrouter.ai"] || !hosts["api.together.ai"] {
		t.Errorf("hosts = %v, want both openrouter.ai and api.together.ai", hosts)
	}
}

// --- CostCompositionWindow (integration) ------------------------------------

// TestCostCompositionWindow_Empty pins the empty-DB case: a zero composition
// with a non-nil empty ByModel (the API maps TotalCostMicro==0 to a nil sidecar
// and omits the key).
func TestCostCompositionWindow_Empty(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	c, err := db.CostCompositionWindow(context.Background(), time.Now().UTC().Add(-24*time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("CostCompositionWindow: %v", err)
	}
	if c.TotalCostMicro != 0 {
		t.Errorf("empty DB: total = %d, want 0", c.TotalCostMicro)
	}
	if c.ByModel == nil || len(c.ByModel) != 0 {
		t.Errorf("empty DB: ByModel = %v, want non-nil empty", c.ByModel)
	}
}

// TestCostCompositionWindow_AggregatesAndReconciles inserts a realistic mix and
// pins the whole read end to end: totals, the attributed/unattributed split, the
// premium and cache levers, and per-model folding — all from real stored rows.
func TestCostCompositionWindow_AggregatesAndReconciles(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	events := []TokenEvent{
		{Developer: "alice", IssueID: "issue-1", Model: "claude-opus-4-8", InputTok: 1000, OutputTok: 200, CacheRead: 3000, CacheWrite5m: 500, CostMicro: 100_000, Source: "proxy", Fidelity: "realtime", Timestamp: now},
		{Developer: "bob", IssueID: "issue-2", Model: "claude-sonnet-4", InputTok: 2000, OutputTok: 400, CacheRead: 1000, CostMicro: 40_000, Source: "proxy", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedIssueID, Model: "claude-sonnet-4", InputTok: 500, CostMicro: 10_000, Source: "proxy", Fidelity: "realtime", Timestamp: now},
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}

	c, err := db.CostCompositionWindow(ctx, now.Add(-time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("CostCompositionWindow: %v", err)
	}

	if c.TotalCostMicro != 150_000 {
		t.Errorf("total = %d, want 150000", c.TotalCostMicro)
	}
	if c.AttributedCostMicro+c.UnattributedCostMicro != c.TotalCostMicro {
		t.Errorf("attributed(%d)+unattributed(%d) != total(%d)", c.AttributedCostMicro, c.UnattributedCostMicro, c.TotalCostMicro)
	}
	if c.UnattributedCostMicro != 10_000 {
		t.Errorf("unattributed = %d, want 10000", c.UnattributedCostMicro)
	}
	if !approxEq(c.UnattributedShare, 10_000.0/150_000.0) {
		t.Errorf("unattributed_share = %v, want %v", c.UnattributedShare, 10_000.0/150_000.0)
	}
	if !approxEq(c.PremiumModelShare, 100_000.0/150_000.0) {
		t.Errorf("premium_model_share = %v, want %v", c.PremiumModelShare, 100_000.0/150_000.0)
	}
	// cache_read = 4000; input-side = input(3500) + cache_read(4000) + cw5m(500) = 8000.
	if !approxEq(c.CacheReadShare, 0.5) {
		t.Errorf("cache_read_share = %v, want 0.5", c.CacheReadShare)
	}
	if len(c.ByModel) != 2 {
		t.Fatalf("ByModel has %d rows, want 2", len(c.ByModel))
	}
	// Sorted desc: opus (100k) then sonnet (50k = 40k attributed + 10k unattributed).
	if c.ByModel[0].Model != "claude-opus-4-8" || c.ByModel[0].CostMicro != 100_000 {
		t.Errorf("row[0] = %+v, want claude-opus-4-8 @ 100000", c.ByModel[0])
	}
	if c.ByModel[1].Model != "claude-sonnet-4" || c.ByModel[1].CostMicro != 50_000 {
		t.Errorf("row[1] = %+v, want claude-sonnet-4 @ 50000", c.ByModel[1])
	}
	// by_class token totals are exact sums.
	if c.ByClass.InputTok != 3500 || c.ByClass.OutputTok != 600 || c.ByClass.CacheRead != 4000 || c.ByClass.CacheWrite5m != 500 {
		t.Errorf("by_class = %+v, want input 3500 / output 600 / cache_read 4000 / cw5m 500", c.ByClass)
	}
}

// TestCostCompositionWindow_UnattributedFamily pins the #refocus family match: the
// LABELED unattributed buckets (unattributed:main / :detached-head /
// :branch-without-issue) count as unattributed, exactly like the base sentinel. An
// exact `= 'unattributed'` match would have counted the buckets as ATTRIBUTED and
// silently inflated coverage — this is the regression guard against that.
func TestCostCompositionWindow_UnattributedFamily(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	events := []TokenEvent{
		{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", CostMicro: 40_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedIssueID, Model: "claude-sonnet-4", CostMicro: 10_000, Source: "proxy", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedMainBucket, Model: "claude-sonnet-4", CostMicro: 30_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedDetachedHEADBucket, Model: "claude-sonnet-4", CostMicro: 15_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedNoIssueBucket, Model: "claude-sonnet-4", CostMicro: 5_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}

	c, err := db.CostCompositionWindow(ctx, now.Add(-time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("CostCompositionWindow: %v", err)
	}
	// unattributed = base(10k) + main(30k) + detached(15k) + no-issue(5k) = 60k.
	if c.UnattributedCostMicro != 60_000 {
		t.Errorf("unattributed = %d, want 60000 (whole family, not just the base sentinel)", c.UnattributedCostMicro)
	}
	if c.AttributedCostMicro != 40_000 {
		t.Errorf("attributed = %d, want 40000 (only the real issue)", c.AttributedCostMicro)
	}
	if c.AttributedCostMicro+c.UnattributedCostMicro != c.TotalCostMicro {
		t.Errorf("split does not reconcile: %d + %d != %d", c.AttributedCostMicro, c.UnattributedCostMicro, c.TotalCostMicro)
	}
}

// TestUnattributedBucketCostsWindow pins the labeled-bucket breakdown read
// (#refocus, Option B): per-(developer, bucket) unattributed spend, and that the
// buckets sum to the composition's single unattributed figure (no double-count, no
// gap). Attributed spend is excluded by construction.
func TestUnattributedBucketCostsWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	events := []TokenEvent{
		{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", CostMicro: 40_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedMainBucket, Model: "claude-sonnet-4", CostMicro: 30_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: UnattributedNoIssueBucket, Model: "claude-sonnet-4", CostMicro: 5_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "bob", IssueID: UnattributedMainBucket, Model: "claude-sonnet-4", CostMicro: 20_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "bob", IssueID: UnattributedDetachedHEADBucket, Model: "claude-sonnet-4", CostMicro: 15_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}

	rows, err := db.UnattributedBucketCostsWindow(ctx, now.Add(-time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("UnattributedBucketCostsWindow: %v", err)
	}

	// No attributed (issue-1) row should appear.
	var sum int64
	perDevMain := map[string]int64{}
	for _, r := range rows {
		if r.Bucket == "issue-1" {
			t.Errorf("attributed issue-1 leaked into the unattributed breakdown: %+v", r)
		}
		sum += r.CostMicro
		if r.Bucket == UnattributedMainBucket {
			perDevMain[r.Developer] += r.CostMicro
		}
	}
	// The breakdown sums to the same unattributed figure the composition reports.
	c, err := db.CostCompositionWindow(ctx, now.Add(-time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("CostCompositionWindow: %v", err)
	}
	if sum != c.UnattributedCostMicro {
		t.Errorf("bucket breakdown sum = %d, want %d (must equal the composition's unattributed)", sum, c.UnattributedCostMicro)
	}
	if perDevMain["alice"] != 30_000 || perDevMain["bob"] != 20_000 {
		t.Errorf("per-developer main bucket = %v, want alice=30000 bob=20000", perDevMain)
	}
}

// TestCostCompositionWindow_UpperBoundExclusive pins the half-open [since, until)
// window (#276): an event at exactly `until` is excluded.
func TestCostCompositionWindow_UpperBoundExclusive(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	events := []TokenEvent{
		{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", InputTok: 100, CostMicro: 10_000, Source: "proxy", Fidelity: "realtime", Timestamp: base},
		{Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", InputTok: 100, CostMicro: 99_000, Source: "proxy", Fidelity: "realtime", Timestamp: base.Add(time.Hour)},
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}

	// Window [base, base+time.Hour): the second event is at the exclusive upper
	// bound and must be excluded, so only the 10000-micro event counts.
	c, err := db.CostCompositionWindow(ctx, base.Add(-time.Minute), base.Add(time.Hour), FleetWide)
	if err != nil {
		t.Fatalf("CostCompositionWindow: %v", err)
	}
	if c.TotalCostMicro != 10_000 {
		t.Errorf("windowed total = %d, want 10000 (upper bound exclusive)", c.TotalCostMicro)
	}
}

// TestIsPremiumModel pins the host-aware premium classifier that
// premium_model_share keys on: frontier-tier models (>= $5/M base input) are
// premium, workhorse models are not, and a guessed self-hosted price is never
// premium.
func TestIsPremiumModel(t *testing.T) {
	cases := []struct {
		host, model string
		want        bool
	}{
		{HostUnknown, "claude-opus-4-8", true},        // $5/M input
		{HostUnknown, "claude-opus-4-1", true},        // $15/M input
		{HostUnknown, "claude-fable-5", true},         // $10/M input
		{HostUnknown, "gpt-5.4-pro", true},            // $30/M input
		{HostUnknown, "claude-sonnet-4", false},       // $3/M input
		{HostUnknown, "claude-haiku-4-5", false},      // $1/M input
		{HostUnknown, "gpt-5.4", false},               // $2.50/M input
		{HostUnknown, "totally-unknown-model", false}, // guessed self-hosted fallback, never premium
		// Host-awareness (#300): an audited host-qualified open-weights rate is
		// consulted host-first and is a cheap workhorse, so it is non-premium. The
		// converse — a model premium on host A but cheap on host B — is not
		// representable in today's price table (no host-qualified entry prices at or
		// above the $5/M premium floor); the host-first resolution that WOULD flip it
		// is proven by premiumLookup mirroring ComputeCostHost.
		{"openrouter.ai", "meta-llama/llama-3.3-70b-instruct", false},         // audited host rate $0.10/M
		{"api.together.ai", "meta-llama/llama-3.3-70b-instruct-turbo", false}, // audited host rate $1.04/M
	}
	for _, tc := range cases {
		if got := IsPremiumModel(tc.host, tc.model); got != tc.want {
			t.Errorf("IsPremiumModel(%q, %q) = %v, want %v", tc.host, tc.model, got, tc.want)
		}
	}
}
