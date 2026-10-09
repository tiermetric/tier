package store

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// These tests pin the built-in glm-5.3 row (internal/store/prices.yaml, provider
// zai, per_token — #786, R-2026-09-28-12). They read the embedded table itself
// rather than a copy of its rates: a rate asserted only against an inlined
// duplicate is not asserted against the table the binary boots with.
//
// The one constant that matters is cache_read_mult. Provider zai defaults the
// read multiplier to 1.0×, and on the measured mix below cache reads are 89.3% of
// all tokens — so omitting it overstates the total by 3.31×.
// TestGLM53_CacheReadMultIsLoadBearing removes it and measures that.
const (
	zaiHost    = "zai-coding-plan"
	zaiModel53 = "glm-5.3"
	zaiModel52 = "glm-5.2"
	zaiKey53   = zaiModel53 + "@" + zaiHost
	zaiKey52   = zaiModel52 + "@" + zaiHost
)

// Published rates, https://docs.z.ai/guides/overview/pricing.
const (
	zaiInputPerM     = 1.40
	zaiOutputPerM    = 4.40
	zaiCachedPerM    = 0.26
	zaiCacheReadMult = 0.18571428571428572 // the nearest float64 to $0.26 ÷ $1.40
	zaiDefaultMult   = 1.0                 // the provider-zai default the row overrides
)

// zaiCachedReadMicro is the cost of a 1M-token cache-read event: exactly the
// published $0.26/M.
//
// 🔴 THE GUARDS PIN THIS RATE, NOT THE MULTIPLIER. An assertion on
// cache_read_mult's own value accepts any future rounding of it, because the
// expected value gets edited in the same breath as the row. An assertion on the
// resulting RATE is checkable against the cited source: 0.186 prices at 260_400.
const zaiCachedReadMicro = 260_000

// zaiCacheReadMultLiteral is the multiplier EXACTLY as it appears in prices.yaml.
// Used only to anchor a text mutation; the numeric constant above is the value.
const zaiCacheReadMultLiteral = "0.18571428571428572"

// The real measured token mix from the maintainer's machine (~32 days of GLM-5.3
// through Opencode), 270,777,337 tokens total. Cache reads are 89.3% of it.
const (
	zaiMixInput     = 23_863_660
	zaiMixCacheRead = 241_709_760
	zaiMixOutput    = 788_691
	zaiMixReasoning = 4_415_226
)

// Expected micro-dollar totals for that mix, reasoning folded into Output.
//
// ⚠️ The reasoning convention is UNVERIFIED — see
// TestGLM53_MeasuredMixReproducesPublishedTotal. These constants are the
// arithmetic of the published rates, not a claim about how Z.ai bills reasoning.
//
//	input      23,863,660 × $1.40/M              = $ 33.409124
//	cache read 241,709,760 × $0.26/M             = $ 62.844538
//	output     (788,691 + 4,415,226) × $4.40/M   = $ 22.897235
//	                                               ──────────
//	                                               $119.150896
//
// The scaled fractional parts are .4, .0 and .8 — no ties — so DollarsToMicro's
// tie rule is never exercised.
const (
	zaiMixWantMicro        = 119_150_896 // with the correctly-rounded cache_read_mult
	zaiMixDefaultMultMicro = 394_700_023 // leaving the provider 1.0× default
)

// embeddedGLM53Line returns the built-in glm-5.3 row's line from prices.yaml,
// failing unless exactly one exists — the mutations below target it by text.
func embeddedGLM53Line(t *testing.T) string {
	t.Helper()
	lines := regexp.MustCompile(`(?m)^  glm-5\.3: \{[^\n]*$`).FindAllString(string(defaultPriceTableYAML), -1)
	if len(lines) != 1 {
		t.Fatalf("found %d `glm-5.3:` rows in the embedded prices.yaml, want 1", len(lines))
	}
	return lines[0]
}

// mutateEmbeddedGLM53 returns the embedded table with old replaced by new inside
// the glm-5.3 row only, failing on a no-op edit (which would prove nothing).
func mutateEmbeddedGLM53(t *testing.T, old, new string) string {
	t.Helper()
	line := embeddedGLM53Line(t)
	mutated := strings.Replace(line, old, new, 1)
	if mutated == line {
		t.Fatalf("mutation %q -> %q changed nothing in %q — it can prove nothing", old, new, line)
	}
	return strings.Replace(string(defaultPriceTableYAML), line, mutated, 1)
}

// TestGLM53_CachedRateEqualsPublishedRate pins the RATE the row produces against
// the RATE its source cites: a 1M cache-read event must cost exactly 260_000
// micro-dollars, on the coding-plan host and hostless alike (the row is
// model-only, #786).
func TestGLM53_CachedRateEqualsPublishedRate(t *testing.T) {
	loadDefaultPriceTable(t)

	for _, host := range []string{zaiHost, HostUnknown} {
		got, mode := ComputeCostHost(host, zaiModel53, CostUsage{CacheRead: 1_000_000})
		if got != zaiCachedReadMicro {
			t.Errorf("host %q: 1M cache read = %d micro ($%.6f/M), want %d ($%.2f/M as published at docs.z.ai)",
				host, got, MicroToDollars(got), zaiCachedReadMicro, zaiCachedPerM)
		}
		if mode != BillingPerToken {
			t.Errorf("host %q: billing_mode = %q, want %q — GLM is costed per token at list price (#786)", host, mode, BillingPerToken)
		}
	}

	// The literal is the CORRECTLY-ROUNDED float64 quotient of $0.26 ÷ $1.40.
	//
	// 🔴 THE COMPARISON BELOW MUST USE float64 VARIABLES. As untyped CONSTANTS Go
	// folds at arbitrary precision and both `0.26/1.40 == mult` and
	// `1.40*mult == 0.26` are FALSE (diff 8e-18); as float64 VARIABLES — what
	// priceCall performs — both are TRUE. `published, inputRate := ...` makes them
	// variables on purpose, so turning them into constants reddens this subtest.
	t.Run("multiplier is the correctly-rounded quotient", func(t *testing.T) {
		published, inputRate := zaiCachedPerM, zaiInputPerM
		if quotient := published / inputRate; quotient != zaiCacheReadMult {
			t.Errorf("$0.26 ÷ $1.40 as float64 = %.17g, but this file's constant is %.17g", quotient, zaiCacheReadMult)
		}
		if got := priceTable[zaiModel53].cacheReadMult; got != zaiCacheReadMult {
			t.Errorf("embedded glm-5.3 cache_read_mult parsed as %.17g, want %.17g (the correctly-rounded $0.26 ÷ $1.40)", got, zaiCacheReadMult)
		}
		// A NECESSARY condition, not the justification: nextUp(mult) also
		// multiplies to bitwise 0.26 (nextDown does not).
		roundTrip := inputRate * zaiCacheReadMult
		if math.Float64bits(roundTrip) != math.Float64bits(float64(published)) {
			t.Errorf("$1.40 × %.17g = %.17g, want bitwise-equal to $0.26 (error %.3e)",
				zaiCacheReadMult, roundTrip, roundTrip-published)
		}
	})

	// COUNTER-ARM: reintroduce the 0.186 convenience rounding and confirm the rate
	// guard catches it.
	t.Run("counter-arm: the 0.186 rounding is caught", func(t *testing.T) {
		installPriceTable(t, mutateEmbeddedGLM53(t, "cache_read_mult: "+zaiCacheReadMultLiteral, "cache_read_mult: 0.186"))
		got, _ := ComputeCostHost(zaiHost, zaiModel53, CostUsage{CacheRead: 1_000_000})
		if got == zaiCachedReadMicro {
			t.Fatal("a 0.186 multiplier still priced at the published rate — this guard cannot distinguish the defect it was written for")
		}
		if got != 260_400 {
			t.Errorf("0.186 priced 1M cache read at %d micro, want 260_400 ($0.2604/M)", got)
		}
	})
}

// TestGLM53_CacheReadMultIsLoadBearing measures what the multiplier is worth on
// the real mix: $119.15 as shipped, $394.70 with the key REMOVED from the
// glm-5.3 row (provider zai then supplies its 1.0× default) — 3.31× overstated.
func TestGLM53_CacheReadMultIsLoadBearing(t *testing.T) {
	usage := CostUsage{
		Input:     zaiMixInput,
		CacheRead: zaiMixCacheRead,
		Output:    zaiMixOutput + zaiMixReasoning,
	}

	loadDefaultPriceTable(t)
	shipped, mode := ComputeCostHost(zaiHost, zaiModel53, usage)
	if shipped != zaiMixWantMicro {
		t.Errorf("built-in glm-5.3 prices the measured mix at %d micro ($%.6f), want %d ($%.6f)",
			shipped, MicroToDollars(shipped), zaiMixWantMicro, MicroToDollars(zaiMixWantMicro))
	}
	if mode != BillingPerToken {
		t.Errorf("billing_mode = %q, want %q", mode, BillingPerToken)
	}

	// With the multiplier removed — the ONLY difference.
	installPriceTable(t, mutateEmbeddedGLM53(t, "cache_read_mult: "+zaiCacheReadMultLiteral+", ", ""))
	withDefault, _ := ComputeCostHost(zaiHost, zaiModel53, usage)
	if withDefault != zaiMixDefaultMultMicro {
		t.Errorf("without cache_read_mult the measured mix prices at %d micro ($%.6f), want %d ($%.6f) — the zai 1.0× default bills every cached read at the full $1.40 input rate",
			withDefault, MicroToDollars(withDefault), zaiMixDefaultMultMicro, MicroToDollars(zaiMixDefaultMultMicro))
	}

	// The ratio is computed from the two MEASURED totals, not from the constants.
	if shipped == 0 {
		t.Fatal("shipped total is 0 — the ratio below would be meaningless")
	}
	if ratio := float64(withDefault) / float64(shipped); ratio < 3.30 || ratio > 3.32 {
		t.Errorf("measured overstatement ratio = %.4f×, want ~3.31×", ratio)
	}
}

// TestGLM53_MeasuredMixReproducesPublishedTotal prices the real measured token
// mix under BOTH reasoning conventions.
//
// 🔴 WHICH CONVENTION Z.AI BILLS IS UNVERIFIED. Reasoning ADDITIONAL to output
// (the Gemini convention, which Opencode's data shows — reasoning 4,415,226
// exceeds output 788,691, so it cannot be a subset): $119.15. Reasoning INSIDE
// output (the OpenAI convention): $99.72. Both are asserted so a producer change
// moves a number this test names.
func TestGLM53_MeasuredMixReproducesPublishedTotal(t *testing.T) {
	loadDefaultPriceTable(t)

	folded, mode := ComputeCostHost(zaiHost, zaiModel53, CostUsage{
		Input:     zaiMixInput,
		CacheRead: zaiMixCacheRead,
		Output:    zaiMixOutput + zaiMixReasoning,
	})
	if folded != zaiMixWantMicro {
		t.Errorf("measured mix (reasoning billed as output) = %d micro ($%.6f), want %d ($%.6f)",
			folded, MicroToDollars(folded), zaiMixWantMicro, MicroToDollars(zaiMixWantMicro))
	}
	if mode != BillingPerToken {
		t.Errorf("billing_mode = %q, want %q", mode, BillingPerToken)
	}

	excluded, _ := ComputeCostHost(zaiHost, zaiModel53, CostUsage{
		Input:     zaiMixInput,
		CacheRead: zaiMixCacheRead,
		Output:    zaiMixOutput,
	})
	const wantExcluded = 99_723_902
	if excluded != wantExcluded {
		t.Errorf("measured mix (reasoning excluded) = %d micro ($%.6f), want %d ($%.6f)",
			excluded, MicroToDollars(excluded), wantExcluded, MicroToDollars(wantExcluded))
	}
}

// TestGLM53_NoGuessedCostShare pins that GLM-5.3 traffic on the coding plan is
// priced at an AUDITED rate: it fires neither the unknown-model event counter nor
// the guessed-cost recorder, and emits no WARN. The control arm proves the
// recorders are wired — an unpriced model on the same host MUST fire both.
func TestGLM53_NoGuessedCostShare(t *testing.T) {
	loadDefaultPriceTable(t)
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)
	t.Cleanup(func() {
		SetUnknownModelRecorder(nil)
		SetUnknownModelCostRecorder(nil)
	})

	events := &countingRecorder{}
	guessed := &fakeCostRecorder{}
	SetUnknownModelRecorder(events)
	SetUnknownModelCostRecorder(guessed)

	usage := CostUsage{Input: zaiMixInput, CacheRead: zaiMixCacheRead, Output: zaiMixOutput + zaiMixReasoning}
	if _, mode := ComputeCostHost(zaiHost, zaiModel53, usage); mode != BillingPerToken {
		t.Errorf("%s billing_mode = %q, want %q", zaiModel53, mode, BillingPerToken)
	}
	if events.n != 0 || guessed.calls != 0 || guessed.sum != 0 {
		t.Errorf("GLM-5.3 priced as a GUESS: unknown-model events=%d, guessed-cost calls=%d sum=%v, want all zero",
			events.n, guessed.calls, guessed.sum)
	}
	if out := buf.String(); out != "" {
		t.Errorf("GLM-5.3 pricing emitted a WARN, want silence for an audited rate: %q", out)
	}

	// CONTROL: the recorders are wired and DO fire. The control is glm-5.2, which
	// has no row (R-2026-09-28-12) and which serve's startup check no longer
	// probes, so this unknown-model WARN is the signal that names it.
	ctl, _ := ComputeCostHost(zaiHost, zaiModel52, CostUsage{Input: 1_000_000})
	if events.n == 0 || guessed.calls == 0 {
		t.Fatalf("control arm did not fire: an unpriced model on the same host recorded events=%d guessed=%d — the assertions above are vacuous",
			events.n, guessed.calls)
	}
	if ctl != 500_000 {
		t.Errorf("control model priced at %d micro, want 500_000 (the self-hosted-medium fallback)", ctl)
	}
	if out := buf.String(); !strings.Contains(out, zaiModel52) || !strings.Contains(out, "priced by a GUESS") {
		t.Errorf("%s priced at the guessed fallback without the unknown-model WARN naming it: %q", zaiModel52, out)
	}
}

// TestGLM53_CacheWriteInheritsProviderDefault pins how the one rate Z.ai does NOT
// publish resolves. Its pricing page lists a "Cached Input Storage" SKU
// ("Limited-time Free") — a storage price with no class in CostUsage — and no
// per-token cache-write rate. So the row sets no cache_write_*_mult and both
// write classes inherit the zai default of 1.0×: a cache write bills at the full
// $1.40 input rate. The row must carry NO cache-write key: an invented 1.0× and
// an inherited 1.0× price identically and could not be told apart later.
//
// 🔴 A genuinely free write class is INEXPRESSIBLE today: parsePriceTable treats
// `cache_write_5m_mult: 0` as "inherit the provider default". The last subtest
// asserts that, so this note cannot rot.
func TestGLM53_CacheWriteInheritsProviderDefault(t *testing.T) {
	line := embeddedGLM53Line(t)
	for _, key := range []string{"cache_write_5m_mult", "cache_write_1h_mult"} {
		if strings.Contains(line, key) {
			t.Errorf("the embedded glm-5.3 row sets %s — Z.ai publishes no cache-write rate: %q", key, line)
		}
	}

	loadDefaultPriceTable(t)
	p, ok := priceTable[zaiModel53]
	if !ok {
		t.Fatalf("embedded price table has no row %q", zaiModel53)
	}
	if p.cacheWrite5mMult != zaiDefaultMult || p.cacheWrite1hMult != zaiDefaultMult {
		t.Errorf("%s cache-write multipliers = %v/%v, want %v/%v (the zai provider default)",
			zaiModel53, p.cacheWrite5mMult, p.cacheWrite1hMult, zaiDefaultMult, zaiDefaultMult)
	}
	write5m, _ := ComputeCostHost(zaiHost, zaiModel53, CostUsage{CacheWrite5m: 1_000_000})
	write1h, _ := ComputeCostHost(zaiHost, zaiModel53, CostUsage{CacheWrite1h: 1_000_000})
	if write5m != 1_400_000 || write1h != 1_400_000 {
		t.Errorf("1M cache-write = %d/%d micro (5m/1h), want 1_400_000 each — the full $1.40 input rate", write5m, write1h)
	}

	t.Run("an explicit 0 cannot express free", func(t *testing.T) {
		installPriceTable(t, mutateEmbeddedGLM53(t, "cache_read_mult:", "cache_write_5m_mult: 0, cache_read_mult:"))
		got, _ := ComputeCostHost(zaiHost, zaiModel53, CostUsage{CacheWrite5m: 1_000_000})
		if got == 0 {
			t.Fatal("`cache_write_5m_mult: 0` priced a cache write at $0 — the parser now expresses a free class; update this test and its doc comment")
		}
		if got != 1_400_000 {
			t.Errorf("`cache_write_5m_mult: 0` priced 1M cache-write at %d micro, want 1_400_000 — 0 means \"inherit the provider default\" (1.0×), not zero", got)
		}
	})
}

// TestEmbeddedPriceTable_GLM53OnlyNoSubscriptionRow pins R-2026-09-28-12: the
// built-in table prices GLM-5.3 through its model-only per_token row and carries
// no glm-5.2 row, no host-qualified GLM row (one would win over the model-only
// row and could re-label its spend), and no subscription row
// (TestSubscriptionRoutes_EmbeddedDefaultSeedsNone).
func TestEmbeddedPriceTable_GLM53OnlyNoSubscriptionRow(t *testing.T) {
	loadDefaultPriceTable(t)
	if _, ok := priceTable[zaiModel53]; !ok {
		t.Fatalf("embedded price table has no %q row", zaiModel53)
	}
	for _, key := range []string{zaiKey53, zaiKey52, zaiModel52} {
		if _, ok := priceTable[key]; ok {
			t.Errorf("embedded price table contains %q — GLM-5.3 only, per token (R-2026-09-28-12, #786)", key)
		}
	}
	if got := SubscriptionRoutes(); len(got) != 0 {
		t.Errorf("embedded price table ships subscription routes %v, want none", got)
	}
}

// TestEmbeddedPriceTable_VersionBelowOperatorRange pins the reserved high version
// range: operator overrides number from 1000, so the embedded table (which counts
// upward) must stay below it or an override's price_version stamp could collide.
func TestEmbeddedPriceTable_VersionBelowOperatorRange(t *testing.T) {
	const operatorFloor = 1000
	_, embedded, err := parsePriceTable(defaultPriceTableYAML)
	if err != nil {
		t.Fatalf("parse embedded table: %v", err)
	}
	if embedded.Version >= operatorFloor {
		t.Errorf("the EMBEDDED table reached version %d, colliding with the operator range floor %d", embedded.Version, operatorFloor)
	}
}

// TestPriceTableOverride_MergesIntoAFullCopy exercises the recipe the docs give a
// mixed fleet (docs/open-weights-capture.md §4): `--prices` REPLACES the table, so
// an operator adding one row copies internal/store/prices.yaml, moves its version
// into the operator range and appends the row. It must be appendable — same
// indentation, no duplicate keys, still parses — and keep every embedded row.
func TestPriceTableOverride_MergesIntoAFullCopy(t *testing.T) {
	const opRow = `  "llama-3.3-70b@gpu.internal": { input_per_m: 0.60, output_per_m: 0.80, provider: self-hosted }` + "\n"

	versionLine := regexp.MustCompile(`(?m)^version: \d+$`)
	embedded := string(defaultPriceTableYAML)
	if !versionLine.MatchString(embedded) {
		t.Fatal("embedded prices.yaml has no `version: <n>` line — the merge recipe rewrites it")
	}
	installPriceTable(t, versionLine.ReplaceAllString(embedded, "version: 1000")+"\n"+opRow)

	if got, _ := ComputeCostHost("gpu.internal", "llama-3.3-70b", CostUsage{Input: 1_000_000}); got != 600_000 {
		t.Errorf("merged table: 1M input on the appended row = %d micro, want 600_000", got)
	}
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	if !modelIsExact("claude-opus-5") || !modelIsExact(zaiModel53) {
		t.Error("merged table lost an embedded row (claude-opus-5 or glm-5.3) — the merge must keep them")
	}
	if info := ActivePriceTableInfo(); info.Version != 1000 {
		t.Errorf("merged table version = %d, want 1000", info.Version)
	}
}

// TestSelfHostedDefaults_ArePricedNotBypassed pins the claim providerDefaultMults'
// doc comment makes, because that comment previously said the OPPOSITE and was
// believed for as long as it stood: "self-hosted entries never reach this via
// ComputeCost (the combined path bills a single rate)".
//
// They do reach it. Only the three self-hosted-{large,medium,small} fallbacks are
// combined; every #268 host-qualified open-weights row is provider: self-hosted and
// NOT combined, so it takes priceCall's per-class path and bills its cached reads at
// whatever providerDefaultMults resolved - 1.0x, the full input rate, unless the row
// overrides it.
//
// The same 1.0x read default applies to provider zai, which is why the built-in
// glm-5.3 row sets cache_read_mult (TestGLM53_CacheReadMultIsLoadBearing).
func TestSelfHostedDefaults_ArePricedNotBypassed(t *testing.T) {
	loadDefaultPriceTable(t)

	var nonCombined []string
	for name, p := range priceTable {
		if p.provider == providerSelfHosted && !p.combined {
			nonCombined = append(nonCombined, name)
		}
	}
	if len(nonCombined) == 0 {
		t.Fatal("the embedded table has NO non-combined self-hosted rows - if that is now true by design, providerDefaultMults' doc comment and the Z.ai override's rationale both need revisiting")
	}
	t.Logf("embedded table carries %d non-combined provider:self-hosted rows (the path providerDefaultMults feeds)", len(nonCombined))

	// Each inherits the 1.0x defaults unless it overrides them, and 1.0x on a cached
	// read means the FULL input rate. Demonstrated on one such row.
	sort.Strings(nonCombined) // deterministic pick: map iteration order is randomized
	sample := nonCombined[0]
	p := priceTable[sample]
	if p.cacheReadMult == 0 {
		t.Errorf("%s: cacheReadMult baked as 0 - a resolved multiplier is never 0 (0 in YAML means inherit)", sample)
	}
	if p.cacheReadMult == 1.0 {
		in := ComputeCost(sample, CostUsage{Input: 1_000_000})
		read := ComputeCost(sample, CostUsage{CacheRead: 1_000_000})
		if in != read {
			t.Errorf("%s inherits cache_read_mult 1.0x but 1M input (%d) != 1M cached read (%d) - the default is not reaching priceCall", sample, in, read)
		}
		t.Logf("%s inherits 1.0x: 1M cached read costs %d micro, identical to 1M input - the full input rate", sample, read)
	}
}
