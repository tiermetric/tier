package api

// Guards for #722 (part of #710): actual_paid_usd must be bit-reproducible on both
// read paths that merge an alias family's actual_spend.
//
// Both paths float-accumulated over a map returned by ActualSpendAllWindow, and Go
// randomizes map iteration order per range:
//
//	loadWindow              canonSpend[canon(dev)] += paid     (fleet /scores)
//	handleGetDeveloperScore actualPaid += paid                 (/scores/{developer})
//
// The sum is ActualPaidUSD, and SpendLeverage is TotalCostUSD/ActualPaidUSD, so a
// last-ulp wobble in the sum propagates to both published fields. Neither field is
// rounded on the wire (see developerScoreJSON / developerDetailResponse), so the
// wobble is observable by a consumer diffing two responses.
//
// 🔴 THE FIXTURE IS THE HARD PART, AND IT HAS TWO INDEPENDENT WAYS TO GO VACUOUS.
//
//	(1) NO actual_spend AT ALL. Both accumulations run over an EMPTY map when the
//	    table is empty, so a guard written on the ordinary /scores fixture cannot
//	    fail — it would range nothing and compare 0 to 0 a thousand times. This is
//	    the exact shape #711's first attempt hit. Answered by
//	    TestSpendMergeFixtureIsExercised, which asserts the merged figure is
//	    non-zero and that the three raw rows collapsed to ONE developer.
//
//	(2) ONLY TWO raw identifiers. Float addition is COMMUTATIVE — a+b == b+a is
//	    exact — so a two-alias family can NEVER expose an ordering difference; only
//	    ASSOCIATIVITY fails, which needs three or more addends. The repo's existing
//	    TestGetScores_AliasMergesCostRowsAndActualSpend seeds exactly two, which is
//	    why it never caught this. This fixture seeds THREE, and
//	    TestSpendMergeFixtureIsOrderSensitive proves those three values actually
//	    produce more than one sum under real Go map iteration.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// spendIterations is the sample size for the map-order loops. Go's map iteration
// randomization is not uniform over permutations: the three values below were
// measured at roughly an 87/13 split between two distinct sums, so P(a broken
// implementation looks clean) ≈ 0.87^1000 ≈ 1e-60.
const spendIterations = 1000

// The alias family. THREE raw identifiers, one canonical target — the minimum that
// can expose a non-associative sum, and the shape canon() exists to serve.
const (
	spendCanonical = "alice.laptop"
	spendAliasA    = "alice.desktop"
	spendAliasB    = "alice.server"
)

// spendAmounts are the per-identifier actual-paid dollars. Chosen by MEASUREMENT,
// not by eye: 0.1+0.2+0.3 is the canonical non-associative float triple, and
// measured over a real three-entry map range it yields two distinct sums
// (0.6000000000000001 vs 0.5999999999999999). Every value is an exact multiple of
// a micro-dollar, so store.DollarsToMicro round-trips it without introducing a
// difference of its own.
var spendAmounts = map[string]float64{
	spendCanonical: 0.1,
	spendAliasA:    0.2,
	spendAliasB:    0.3,
}

// seedAliasedSpend builds the fixture: cost rows under the canonical id (so
// SpendLeverage has a numerator) plus one actual_spend row per raw identifier, with
// the two non-canonical ones aliased onto the canonical one.
func seedAliasedSpend(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	seedCosts(t, db, spendCanonical, "issue-1", 7.5)
	seedOutcome(t, db, spendCanonical, "issue-1", 3, 1)
	period := time.Now().UTC().Format("2006-01")
	for dev, usd := range spendAmounts {
		if err := db.InsertActualSpend(ctx, store.ActualSpend{
			Developer:       dev,
			Period:          period,
			ActualPaidMicro: store.DollarsToMicro(usd),
			Timestamp:       time.Now().UTC(),
		}); err != nil {
			t.Fatalf("InsertActualSpend(%s): %v", dev, err)
		}
	}
	for _, alias := range []string{spendAliasA, spendAliasB} {
		if err := db.UpsertDeveloperAlias(ctx, alias, spendCanonical, "test:fixture"); err != nil {
			t.Fatalf("UpsertDeveloperAlias(%s): %v", alias, err)
		}
	}
}

// TestSpendMergeFixtureIsOrderSensitive is VACUITY CONTROL (2), and deliberately
// touches no handler: it ranges a three-entry map of the fixture's own values and
// asserts the sum is not always the same. If this ever reports 1 distinct sum, every
// determinism assertion in this file is passing for free and the values must be
// re-chosen before the guards mean anything again.
func TestSpendMergeFixtureIsOrderSensitive(t *testing.T) {
	if len(spendAmounts) < 3 {
		t.Fatalf("spendAmounts has %d identifiers, want >= 3: float addition is COMMUTATIVE, "+
			"so two aliases can never expose an ordering difference", len(spendAmounts))
	}
	seen := map[uint64]int{}
	ran := 0
	for i := 0; i < spendIterations; i++ {
		var sum float64
		for _, v := range spendAmounts { // map iteration order, as the pre-#722 code did
			sum += v
		}
		seen[math.Float64bits(sum)]++
		ran++
	}
	if ran != spendIterations {
		t.Fatalf("loop ran %d times, want %d", ran, spendIterations)
	}
	if len(seen) < 2 {
		t.Fatalf("the fixture's %d values summed to %d distinct float64 over %d map ranges, want >= 2.\n"+
			"The determinism guards in this file are VACUOUS until spendAmounts is replaced.",
			len(spendAmounts), len(seen), ran)
	}
	t.Logf("fixture order-sensitivity: %d distinct sums over %d map ranges (%v)", len(seen), ran, seen)
}

// TestSpendMergeFixtureIsExercised is VACUITY CONTROL (1): it proves the fixture
// actually reaches both accumulations. An empty actual_spend table — the state of
// every other /scores fixture in this package — makes both loops range nothing, and
// a determinism guard over nothing cannot fail.
func TestSpendMergeFixtureIsExercised(t *testing.T) {
	h, db := newTestHandler(t)
	seedAliasedSpend(t, db)

	spendAll, err := db.ActualSpendAllWindow(context.Background(), time.Now().UTC().AddDate(0, -1, 0), time.Time{})
	if err != nil {
		t.Fatalf("ActualSpendAllWindow: %v", err)
	}
	if len(spendAll) != len(spendAmounts) {
		t.Fatalf("actual_spend rows = %d, want %d distinct RAW identifiers — the merge under "+
			"test only exists when several raw ids canonicalize to one developer", len(spendAll), len(spendAmounts))
	}

	devs := scoresDevs(t, h)
	if len(devs) != 1 {
		t.Fatalf("got %d developer rows, want 1 merged row; devs=%v", len(devs), devs)
	}
	got, ok := devs[spendCanonical]
	if !ok {
		t.Fatalf("canonical developer %q absent; got %v", spendCanonical, devs)
	}
	if got.ActualPaidUSD == 0 {
		t.Fatalf("actual_paid_usd = 0: the fixture is not reaching the spend merge at all")
	}
	// 0.6 to within a float slack — the exact bits are the SUBJECT of the other
	// tests, so this arm asserts magnitude, not identity.
	if math.Abs(got.ActualPaidUSD-0.6) > 1e-9 {
		t.Errorf("actual_paid_usd = %v, want ~0.6 (0.1+0.2+0.3 merged)", got.ActualPaidUSD)
	}
	if got.SpendLeverage == 0 {
		t.Errorf("spend_leverage = 0, want non-zero: the derived field must be exercised too")
	}
}

// TestGetScores_MergedActualPaidIsBitReproducible is the fleet-path positive arm
// (loadWindow's canonSpend rebuild).
//
// NEGATIVE CONTROL: drop the sort.Strings(rawSpendDevs) hop in loadWindow and range
// actualSpend directly again — this must fail. Measured kill rate is recorded in the
// #722 PR; do not weaken the fixture without re-measuring it.
func TestGetScores_MergedActualPaidIsBitReproducible(t *testing.T) {
	h, db := newTestHandler(t)
	seedAliasedSpend(t, db)

	paid := map[uint64]int{}
	leverage := map[uint64]int{}
	ran := 0
	for i := 0; i < spendIterations; i++ {
		row := scoresRow(t, h, spendCanonical)
		paid[math.Float64bits(row.ActualPaidUSD)]++
		leverage[math.Float64bits(row.SpendLeverage)]++
		ran++
	}
	if ran != spendIterations {
		t.Fatalf("loop ran %d times, want %d", ran, spendIterations)
	}
	if len(paid) != 1 {
		t.Errorf("actual_paid_usd took %d distinct bit patterns over %d identical requests, want 1: %v", len(paid), ran, paid)
	}
	if len(leverage) != 1 {
		t.Errorf("spend_leverage took %d distinct bit patterns over %d identical requests, want 1: %v", len(leverage), ran, leverage)
	}
}

// TestGetDeveloper_MergedActualPaidIsBitReproducible is the same guard for the
// single-developer detail path, which repeats the accumulation independently. It is
// asserted separately rather than trusted to follow the fleet path: the two are
// different code with no shared helper, which is exactly how they drift.
//
// NEGATIVE CONTROL: drop the sort.Strings(spendDevs) hop in handleGetDeveloperScore
// and this must fail while the fleet-path test above still passes.
func TestGetDeveloper_MergedActualPaidIsBitReproducible(t *testing.T) {
	h, db := newTestHandler(t)
	seedAliasedSpend(t, db)

	paid := map[uint64]int{}
	leverage := map[uint64]int{}
	ran := 0
	for i := 0; i < spendIterations; i++ {
		detail := developerDetail(t, h, spendCanonical)
		if detail.ActualPaidUSD == 0 {
			t.Fatalf("iteration %d: actual_paid_usd = 0 — the detail path is not merging spend", i)
		}
		paid[math.Float64bits(detail.ActualPaidUSD)]++
		leverage[math.Float64bits(detail.SpendLeverage)]++
		ran++
	}
	if ran != spendIterations {
		t.Fatalf("loop ran %d times, want %d", ran, spendIterations)
	}
	if len(paid) != 1 {
		t.Errorf("actual_paid_usd took %d distinct bit patterns over %d identical requests, want 1: %v", len(paid), ran, paid)
	}
	if len(leverage) != 1 {
		t.Errorf("spend_leverage took %d distinct bit patterns over %d identical requests, want 1: %v", len(leverage), ran, leverage)
	}
}

// TestScoresAndDeveloperDetailAgreeBitForBit pins the two paths to EACH OTHER. Both
// publish the same developer's actual_paid_usd, and a reader comparing /scores with
// /scores/{developer} must not see two numbers that disagree in the last ulp for no
// reason present in the data.
//
// It is LOOPED for the same reason the others are: a one-sided fix leaves one path
// wobbling between two values while the other is pinned, so a single paired read
// agrees by luck most of the time (measured ~87%). Only the loop makes "one of the
// two sites was left unsorted" a reliable failure.
func TestScoresAndDeveloperDetailAgreeBitForBit(t *testing.T) {
	h, db := newTestHandler(t)
	seedAliasedSpend(t, db)

	ran := 0
	for i := 0; i < spendIterations; i++ {
		fleet := scoresRow(t, h, spendCanonical)
		detail := developerDetail(t, h, spendCanonical)
		if math.Float64bits(fleet.ActualPaidUSD) != math.Float64bits(detail.ActualPaidUSD) {
			t.Fatalf("iteration %d: actual_paid_usd differs between paths: /scores=%v (%#x) /scores/{developer}=%v (%#x)",
				i, fleet.ActualPaidUSD, math.Float64bits(fleet.ActualPaidUSD),
				detail.ActualPaidUSD, math.Float64bits(detail.ActualPaidUSD))
		}
		ran++
	}
	if ran != spendIterations {
		t.Fatalf("loop ran %d times, want %d", ran, spendIterations)
	}
}

// scoresRow GETs /api/v1/scores and returns one developer's row, failing if absent.
func scoresRow(t *testing.T, h *Handler, developer string) developerScoreJSON {
	t.Helper()
	row, ok := scoresDevs(t, h)[developer]
	if !ok {
		t.Fatalf("developer %q absent from /scores", developer)
	}
	return row
}

// developerDetail GETs /api/v1/scores/{developer} and decodes the detail response.
func developerDetail(t *testing.T, h *Handler, developer string) developerDetailResponse {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/"+developer, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores/%s: status = %d, body = %s", developer, code, body)
	}
	var resp developerDetailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal developer detail: %v", err)
	}
	return resp
}
