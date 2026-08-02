package store

import "math"

// MicroPerUSD is the integer scale for money storage: 1 US dollar =
// 1,000,000 micro-dollars. Cost is stored and aggregated as int64 micro-dollars
// (issue #69) so SUM is exact integer arithmetic and finance reconciliation is
// not at the mercy of float64 accumulation error. The float dollar value only
// reappears at the external boundary (JSON responses, CLI/dashboard display)
// via MicroToDollars.
//
// int64 micro-dollars give ~9.2e12 dollars of headroom per value — unreachable
// at any realistic single- or multi-tenant scale. Should finer resolution ever
// be needed, micro→nano is a lossless ×1000 widening (the reason micro was
// chosen over nano up front: the wider headroom costs nothing today and the
// grain matches the cent-precision surfaces).
const MicroPerUSD = 1_000_000

// MaxCostUSD is the trust-boundary magnitude cap for a single dollar amount:
// one trillion dollars. It is absurd headroom for any real priced event or
// invoice (its micro value, 1e18, sits safely inside int64's ~9.22e18 ceiling),
// yet small enough that anything above it is provably garbage — a malformed or
// hostile client body, never a legitimate cost. The API handlers reject inputs
// beyond this bound with a 400 (#118). It is NOT enforced inside DollarsToMicro:
// that function's saturation clamp is a determinism backstop for internal
// callers (e.g. ComputeCost), whereas MaxCostUSD is input validation at the
// external trust boundary — fail loud, do not silently saturate a client's
// garbage into a $9.2-trillion row.
const MaxCostUSD = 1e12

// DollarsToMicro converts a float dollar amount to integer micro-dollars,
// rounding to the nearest micro-dollar with round-half-to-even (banker's
// rounding). RoundToEven — not math.Round (half-away-from-zero) — is deliberate:
// across millions of priced events the away-from-zero bias would systematically
// inflate totals, whereas half-to-even is unbiased. Negative inputs (credit
// memos / refunds, #24) round symmetrically.
//
// Saturation contract: for a scaled value outside int64 range the result is
// deterministically clamped to math.MaxInt64 (positive) or math.MinInt64
// (negative). This is required for correctness, not merely defensive: a bare
// float64→int64 conversion of an out-of-range value is implementation-defined
// in Go ("Conversions between numeric types"), so it diverges by CPU — arm64
// (FCVTZS) saturates at ±MaxInt64 while amd64 (CVTTSD2SI) yields
// 0x8000000000000000, a huge NEGATIVE number. Without the clamp one overflowing
// value would poison every SUM(cost_micro) aggregate differently per platform.
// The boundary test uses >= against float64(math.MaxInt64): that constant is
// 2^63 exactly (one ULP above MaxInt64), so >= catches every float that would
// not fit and every value strictly below it converts exactly; the negative side
// mirrors it, and float64(math.MinInt64) == -2^63 IS representable, so <= clamps.
//
// A NaN input maps to 0: since every ordered comparison with NaN is false, NaN
// would otherwise slip past both clamps into the same implementation-defined
// int64(NaN) conversion this function exists to eliminate (0 on arm64,
// math.MinInt64 on amd64 — another platform-divergent poison). Mapping to 0
// keeps the result deterministic; NaN should never reach here anyway (see next
// paragraph).
//
// The clamp here is a determinism backstop, NOT input validation. Callers at a
// trust boundary must additionally reject non-finite inputs (NaN/Inf) AND
// enforce MaxCostUSD before converting: the API handlers already 400 on both,
// so a value reaching here from that path is finite and in range, and the
// saturation only ever fires for internal callers with an internally-derived
// (never client-controlled) magnitude.
func DollarsToMicro(d float64) int64 {
	v := math.RoundToEven(d * MicroPerUSD)
	if math.IsNaN(v) {
		return 0 // NaN escapes the ordered comparisons below; pin it deterministically
	}
	if v >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if v <= float64(math.MinInt64) {
		return math.MinInt64
	}
	return int64(v)
}

// MicroToDollars converts integer micro-dollars back to a float dollar amount
// for presentation at the external boundary (JSON, CLI, dashboard). The result
// is exact for any micro value whose magnitude is below 2^53 (~$9e9), which
// bounds every realistic total.
func MicroToDollars(m int64) float64 {
	return float64(m) / MicroPerUSD
}
