package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
)

// TestMonthlyFeeCapMatchesStore pins the CLAIM in config.go's maxMonthlyFeeUSD
// comment: the duplicated money cap equals store.MaxCostUSD.
//
// The duplication is deliberate (internal/config keeps no production dependency
// on internal/store, and this test lives in the EXTERNAL test package so it
// introduces none) — but a duplicated bound that is merely asserted to match is
// a drift waiting to happen, and the direction that matters is silent: loosen
// config's copy and a fee that store's own API would 400 on sails into
// org_actual_spend, where DollarsToMicro saturates and the allocation read's SUM
// starts erroring.
//
// It probes through the PUBLIC surface (Load), so it tests the bound actually
// enforced rather than a constant restated in the test.
func TestMonthlyFeeCapMatchesStore(t *testing.T) {
	cases := []struct {
		name string
		fee  float64
		want bool // should Load accept it?
	}{
		{"at the cap", store.MaxCostUSD, true},
		{"just over the cap", store.MaxCostUSD * 1.001, false},
		{"far over the cap", store.MaxCostUSD * 1000, false},
		{"an ordinary plan", 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tier.yaml")
			body := fmt.Sprintf("subscriptions:\n  - route_prefix: \"a@h\"\n    org: \"o\"\n    monthly_fee_usd: %g\n", tc.fee)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			_, err := config.Load(path)
			if accepted := err == nil; accepted != tc.want {
				t.Errorf("monthly_fee_usd %g: Load accepted=%v, want %v (store.MaxCostUSD = %g; err=%v)",
					tc.fee, accepted, tc.want, store.MaxCostUSD, err)
			}
		})
	}
}

// TestBackfillBoundMatchesStore pins the third duplicated bound in this pair of
// packages: config.maxBackfillPeriods and store.MaxPeriodRange are hard-coded
// twins at 240, and unlike the money cap and the period shape — both of which
// have a mirror test above precisely because a restated bound drifts — nothing
// held these two equal.
//
// The direction that hurts is config being the LOOSER one: an active_since that
// config waves through then dies inside store.PeriodRange, turning a typo into a
// runtime error in the reconciler's startup pass with a worse message, instead of
// a load-time refusal that names the year.
//
// Both sides are probed through their PUBLIC surfaces at the EDGE (the limit and
// one past it), not by comparing constants — a constant comparison would pass
// against two bounds that are equal but enforced with different comparison
// operators, which is exactly the `>=` vs `>` slip this pair is exposed to.
func TestBackfillBoundMatchesStore(t *testing.T) {
	// monthsBackFrom returns the period n months before the current one, so the
	// inclusive count active_since..current is n+1.
	monthsBackFrom := func(n int) string {
		return time.Now().UTC().AddDate(0, -n, 0).Format("2006-01")
	}
	for _, tc := range []struct {
		name   string
		months int  // inclusive period count active_since..current
		want   bool // should Load accept it?
	}{
		{"exactly at the bound", store.MaxPeriodRange, true},
		{"one past the bound", store.MaxPeriodRange + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			activeSince := monthsBackFrom(tc.months - 1)
			_, err := config.Load(writeConfigWithActiveSince(t, activeSince))
			if accepted := err == nil; accepted != tc.want {
				t.Errorf("active_since %q (%d periods): config accepted=%v, want %v (store.MaxPeriodRange = %d; err=%v)",
					activeSince, tc.months, accepted, tc.want, store.MaxPeriodRange, err)
			}
			// And the store must agree at the same edge, or the two bounds have
			// drifted even though each is individually self-consistent.
			got, err := store.PeriodRange(activeSince, store.CurrentPeriod(time.Now()))
			if storeAccepts := err == nil; storeAccepts != tc.want {
				t.Errorf("active_since %q (%d periods): store.PeriodRange accepted=%v (len %d), want %v — the two backfill bounds have drifted",
					activeSince, tc.months, storeAccepts, len(got), tc.want)
			}
		})
	}
}

// TestAcceptedMonthlyFeeIsRepresentableInMicroDollars pins the CLAIM in
// config.go's minMonthlyFeeUSD comment, from the only side that matters: EVERY
// fee Load accepts must survive the trip into the store's integer micro-dollars.
//
// A fee that rounds to zero micro-dollars is not a rounding error, it is a
// permanent one — #155's backfill treats "this period has any row" as posted, so
// a zero-valued row marks the month covered forever, the 0 return reads as
// "already covered" so nothing is logged, and no later config correction can
// repair it.
//
// The invariant is one-directional on purpose. It asserts accepted ⇒
// representable, NOT the converse: a fee between 5e-7 and 1e-6 would in fact
// round to one micro-dollar and is still rejected, because "one micro-dollar" is
// the honest unit to state in an operator-facing error. Pinning the converse
// would pin that deliberate slack as if it were the contract.
//
// It probes through the PUBLIC surface (Load) rather than the unexported
// constant, so it tests the bound actually enforced — and it lives in the
// EXTERNAL test package for the same reason the two tests around it do: this is
// where the store dependency is allowed to exist.
func TestAcceptedMonthlyFeeIsRepresentableInMicroDollars(t *testing.T) {
	probes := []float64{1e-30, 4e-7, 9.99e-7, 1e-6, 0.000002, 0.01, 1, 100, 1e6, store.MaxCostUSD}
	var accepted, rejected int
	for _, fee := range probes {
		path := filepath.Join(t.TempDir(), "tier.yaml")
		body := fmt.Sprintf("subscriptions:\n  - route_prefix: \"a@h\"\n    org: \"o\"\n    monthly_fee_usd: %g\n", fee)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write yaml: %v", err)
		}
		if _, err := config.Load(path); err != nil {
			rejected++
			continue
		}
		accepted++
		if micro := store.DollarsToMicro(fee); micro < 1 {
			t.Errorf("Load ACCEPTED monthly_fee_usd %g, which stores as %d micro-dollars — a zero-valued posting would mark its period permanently covered", fee, micro)
		}
	}
	// CONTROL ARM: the probe set must actually exercise both sides. Without this,
	// a Load that rejected (or accepted) everything would satisfy the loop above.
	if accepted == 0 || rejected == 0 {
		t.Errorf("probe set exercised only one side: %d accepted, %d rejected", accepted, rejected)
	}
}

// writeConfigWithActiveSince writes a minimal config carrying ONE subscription
// whose only variable is active_since, so a Load failure can only come from the
// active_since validation. The value is single-quoted so a YAML scalar like
// `2020-01-01` (which would otherwise decode as a timestamp, not a string)
// still reaches the validator as the operator typed it.
func writeConfigWithActiveSince(t *testing.T, activeSince string) string {
	t.Helper()
	body := "subscriptions:\n" +
		"  - route_prefix: \"a@h\"\n" +
		"    org: \"o\"\n" +
		"    monthly_fee_usd: 1\n" +
		"    active_since: '" + activeSince + "'\n"
	path := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

// TestActiveSincePeriodShapeMatchesStore pins the CLAIM made in config.go's
// periodRE comment: the config package's copy of the billing-period shape
// accepts exactly what store.ValidPeriod accepts.
//
// The duplication is deliberate (internal/config has no production dependency
// on internal/store, and this test lives in the EXTERNAL test package so it
// introduces none), but a duplicated rule that is merely asserted to match is a
// drift waiting to happen: loosen one side and a config that loads cleanly then
// fails at INSERT against the org_actual_spend period CHECK, hours after
// startup. Tighten one side and an operator's valid active_since is rejected.
// Either way the failure surfaces far from the edit — so pin the equality.
//
// It exercises config's copy through the PUBLIC surface (Load → the active_since
// validation) so it is testing the shape actually enforced, not a copy of the
// regexp restated in the test.
func TestActiveSincePeriodShapeMatchesStore(t *testing.T) {
	// The well-formed candidates are derived from LAST year, not hard-coded, and
	// last year specifically. Two bounds have to hold simultaneously for this test
	// to be measuring SHAPE rather than something else:
	//
	//   - every candidate must be in the PAST, or config's future-active_since arm
	//     rejects what store.ValidPeriod accepts (a false drift report). That rules
	//     out the current year, whose later months are still ahead of us.
	//   - every candidate must be within the 240-period backfill bound, or config's
	//     typo guard rejects it — which is what a hard-coded year would eventually
	//     drift into.
	//
	// Last year satisfies both for as long as this code exists.
	year := time.Now().UTC().Year() - 1
	candidates := []string{
		fmt.Sprintf("%d-01", year), fmt.Sprintf("%d-09", year),
		fmt.Sprintf("%d-10", year), fmt.Sprintf("%d-12", year),
		fmt.Sprintf("%d-00", year), fmt.Sprintf("%d-13", year),
		fmt.Sprintf("%d-1", year), fmt.Sprintf("%d-013", year),
		"20206-01", "202-01", "2020_01", "2020-01-01",
		"", " 2020-01", "2020-01 ", "abcd-ef",
	}
	for _, c := range candidates {
		if c == "" {
			continue // absent active_since is a legal "current period only", not a shape
		}
		t.Run(c, func(t *testing.T) {
			path := writeConfigWithActiveSince(t, c)
			_, err := config.Load(path)
			configAccepts := err == nil
			storeAccepts := store.ValidPeriod(c)
			if configAccepts != storeAccepts {
				t.Errorf("active_since %q: config accepts=%v but store.ValidPeriod=%v (err=%v) — "+
					"the two period shapes have drifted", c, configAccepts, storeAccepts, err)
			}
		})
	}
}
