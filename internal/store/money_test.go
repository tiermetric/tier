package store

import (
	"math"
	"testing"
)

// TestDollarsToMicro pins the dollar→micro-dollar conversion (#69), including
// exact conversions, negatives (credit memos), and sub-micro truncation.
func TestDollarsToMicro(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int64
	}{
		{"zero", 0, 0},
		{"one dollar", 1.0, 1_000_000},
		{"exact 2.50", 2.50, 2_500_000},
		{"fractional cent 0.0105", 0.0105, 10_500},
		{"sub-cent 0.005", 0.005, 5_000},
		{"negative credit memo", -2.50, -2_500_000},
		{"negative fractional", -0.0105, -10_500},
		// Sub-micro magnitudes round to the nearest micro-dollar: $0.0000001
		// (0.1 micro) truncates to 0; $0.0000009 (0.9 micro) rounds to 1.
		{"0.1 micro rounds to 0", 0.0000001, 0},
		{"0.9 micro rounds to 1", 0.0000009, 1},
		// Large but exactly-representable: $1,000,000 → 1e12 micro, well within
		// the ~9e12-dollar int64 headroom and below 2^53 micro (~$9.007e9 is the
		// exactness ceiling for fractional dollars; whole-dollar millions stay
		// exact far higher because the product has no fractional part).
		{"one million dollars", 1_000_000.0, 1_000_000_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DollarsToMicro(c.in); got != c.want {
				t.Errorf("DollarsToMicro(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestMicroToDollars pins the inverse conversion and the round-trip identity for
// representable values.
func TestMicroToDollars(t *testing.T) {
	if got := MicroToDollars(1_000_000); got != 1.0 {
		t.Errorf("MicroToDollars(1_000_000) = %v, want 1.0", got)
	}
	if got := MicroToDollars(-2_500_000); got != -2.5 {
		t.Errorf("MicroToDollars(-2_500_000) = %v, want -2.5", got)
	}
	// Round-trip: dollars → micro → dollars is exact to within half a
	// micro-dollar for any input, since DollarsToMicro is the only rounding step.
	for _, d := range []float64{0, 0.0105, 2.50, 99.99, 1234.5678, -50.0} {
		if got := MicroToDollars(DollarsToMicro(d)); math.Abs(got-d) > 5e-7 {
			t.Errorf("round-trip MicroToDollars(DollarsToMicro(%v)) = %v, drift %v > 5e-7", d, got, math.Abs(got-d))
		}
	}
}

// TestDollarsToMicroRoundHalfToEven documents the half-to-even (banker's)
// rounding mechanism: ComputeCost sums many events, and round-half-away-from-zero
// would introduce a systematic upward bias. RoundToEven breaks exact ties toward
// the even neighbour. The inputs are constructed as integer micro-dollars plus an
// exact half so the ×1e6 product lands precisely on a .5 boundary.
func TestDollarsToMicroRoundHalfToEven(t *testing.T) {
	cases := []struct {
		microHalf float64 // the value, in micro-dollars, ending in .5
		want      int64
	}{
		{2.5, 2},   // ties to even (down)
		{3.5, 4},   // ties to even (up)
		{4.5, 4},   // ties to even (down)
		{-2.5, -2}, // symmetric: ties to even
		{-3.5, -4},
	}
	for _, c := range cases {
		// Build the dollar input from an exactly-representable micro value so
		// d*MicroPerUSD reproduces microHalf without decimal-repr drift.
		d := c.microHalf / float64(MicroPerUSD)
		if got := DollarsToMicro(d); got != c.want {
			t.Errorf("DollarsToMicro(%v) [%v micro] = %d, want %d (round-half-to-even)", d, c.microHalf, got, c.want)
		}
	}
}

// TestDollarsToMicro_SaturatesInsteadOfWrapping pins the deterministic
// saturation contract (#118). A bare float64→int64 conversion of an
// out-of-range value is implementation-defined in Go: arm64 (FCVTZS) saturates
// at ±MaxInt64, but amd64 (CVTTSD2SI) yields 0x8000000000000000 — a huge
// NEGATIVE number. That divergence lets one authenticated POST poison every
// SUM(cost_micro) aggregate on amd64. Saturation makes the result identical on
// both platforms. Exactness (== the named constant, not merely "same sign") is
// the cross-platform pin — the boundary case float64(math.MaxInt64) itself
// exercises the >= clamp branch on this arm64 dev box.
func TestDollarsToMicro_SaturatesInsteadOfWrapping(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int64
	}{
		{"1e13 dollars saturates high", 1e13, math.MaxInt64},
		{"-1e13 dollars saturates low", -1e13, math.MinInt64},
		{"9.3e12 dollars saturates high", 9.3e12, math.MaxInt64},
		{"MaxFloat64 saturates high", math.MaxFloat64, math.MaxInt64},
		{"-MaxFloat64 saturates low", -math.MaxFloat64, math.MinInt64},
		{"2^63 boundary (== float64(MaxInt64)) clamps high", float64(math.MaxInt64), math.MaxInt64},
		{"-2^63 boundary (== float64(MinInt64)) clamps low", float64(math.MinInt64), math.MinInt64},
		// NaN escapes the ordered clamp comparisons (all false), so it must be
		// pinned explicitly rather than left to implementation-defined
		// int64(NaN). Handlers reject NaN upstream; this pins the backstop.
		{"NaN maps to 0 deterministically", math.NaN(), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DollarsToMicro(c.in); got != c.want {
				t.Errorf("DollarsToMicro(%v) = %d, want %d (deterministic saturation)", c.in, got, c.want)
			}
		})
	}
}

// TestDollarsToMicro_InRangeUnchanged pins that the saturation backstop does
// not perturb the normal in-range path (#118): zero, banker's rounding at a
// sub-micro tie, negative credit memos, and a large-but-sane value all convert
// exactly as before. 9.2e12 dollars is inside int64 micro range (9.2e18 micro
// < MaxInt64 ≈ 9.223e18) and must round-trip to a positive value below
// MaxInt64, proving the clamp does not fire one boundary early.
func TestDollarsToMicro_InRangeUnchanged(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int64
	}{
		{"zero", 0, 0},
		{"banker's rounding 0.0000005 ties to even (down)", 0.0000005, 0},
		{"negative credit memo", -2.50, -2_500_000},
		{"one million dollars", 1_000_000.0, 1_000_000_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DollarsToMicro(c.in); got != c.want {
				t.Errorf("DollarsToMicro(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
	// 9.2e12 dollars is in range: micro value is positive and strictly below
	// MaxInt64 — the clamp must NOT fire here.
	if got := DollarsToMicro(9.2e12); got <= 0 || got == math.MaxInt64 {
		t.Errorf("DollarsToMicro(9.2e12) = %d, want a positive value < MaxInt64 (in range, no clamp)", got)
	}
}
