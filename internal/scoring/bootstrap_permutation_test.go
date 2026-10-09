package scoring

import (
	"math/rand/v2"
	"testing"
)

// bootstrapSeed1/2 mirror the fixed seed internal/api/handler.go uses for every
// /scores and /scores/compare confidence interval. They are duplicated here on
// purpose: the hazard this test documents is a property of ANY fixed seed, and
// importing the api package from scoring would invert the dependency.
const (
	permSeed1 uint64 = 0x9e3779b97f4a7c15
	permSeed2 uint64 = 0xc2b2ae3d27d4eb4f
)

// TestBootstrapCI_IsPermutationSensitive is a PERMANENT HAZARD PIN for #711, not
// a bug report. It asserts that BootstrapCI's output DEPENDS on the order of its
// inputs, so that anyone who later proposes to "simplify away" the ORDER BY on
// the store's outcome reads reddens a test that states exactly why they must not.
//
// The mechanism: BootstrapCI draws `k := rng.IntN(n)` from a PRNG the caller
// seeds with a FIXED constant, so the sequence of INDICES is identical on every
// call. A fixed index stream over a PERMUTED array selects DIFFERENT ELEMENTS.
// The resampled multiset therefore changes, and the resulting percentile bounds
// move by far more than a floating-point ulp — this is not the last-digit
// associativity drift that the point estimate suffers.
//
// Both slices are permuted by the SAME permutation, so each outcome keeps its
// own cost: this is exactly what a reordered SQL result set does. The multiset
// of (contribution, cost) pairs is unchanged; only their positions move.
//
// ⚠️ A run in which the two intervals come out EQUAL is itself a failure, and is
// reported as one below. Equality would mean the hazard had silently
// disappeared — at which point the ORDER BY's justification would need
// rewriting, not the test deleting.
func TestBootstrapCI_IsPermutationSensitive(t *testing.T) {
	const n = 40

	contribs := make([]float64, n)
	costs := make([]float64, n)
	for i := 0; i < n; i++ {
		// Deliberately heterogeneous: a uniform input would be permutation-
		// INSENSITIVE by construction and the test would prove nothing.
		contribs[i] = 1.0 + float64(i)*0.37
		costs[i] = 0.5 + float64((i*7)%13)*0.25
	}
	const fixedCost = 12.0

	loA, hiA := BootstrapCI(contribs, costs,
		fixedCost, DefaultBootstrapSamples,
		rand.New(rand.NewPCG(permSeed1, permSeed2)))
	if loA == 0 && hiA == 0 {
		t.Fatal("baseline interval is (0, 0) — BootstrapCI bailed out on a degenerate input, so the comparison below would compare two zeros and prove nothing")
	}

	// A fixed, explicit permutation: a reversal composed with a rotation. Written
	// out rather than randomized so the test is reproducible and so it cannot
	// accidentally be the identity.
	perm := make([]int, n)
	for i := range perm {
		perm[i] = (n - 1 - i + 7) % n
	}
	identity := true
	for i, p := range perm {
		if p != i {
			identity = false
			break
		}
	}
	if identity {
		t.Fatal("the permutation is the identity — nothing was permuted, so this test cannot fail")
	}

	pContribs := make([]float64, n)
	pCosts := make([]float64, n)
	for i, p := range perm {
		pContribs[i] = contribs[p]
		pCosts[i] = costs[p]
	}

	// 🔴 CONTROL: same multiset, so the POINT estimate is (near) unchanged. If
	// the permutation had altered the data rather than only its order, the
	// difference below would be uninteresting.
	var sumA, sumB, costA, costB float64
	for i := 0; i < n; i++ {
		sumA += contribs[i]
		sumB += pContribs[i]
		costA += costs[i]
		costB += pCosts[i]
	}
	if d := sumA - sumB; d > 1e-9 || d < -1e-9 {
		t.Fatalf("permuted contributions sum to %v, original to %v — the permutation changed the DATA, not just the order, so this is not the hazard under test", sumB, sumA)
	}
	if d := costA - costB; d > 1e-9 || d < -1e-9 {
		t.Fatalf("permuted costs sum to %v, original to %v — the permutation changed the DATA, not just the order", costB, costA)
	}

	loB, hiB := BootstrapCI(pContribs, pCosts,
		fixedCost, DefaultBootstrapSamples,
		rand.New(rand.NewPCG(permSeed1, permSeed2)))

	if loA == loB && hiA == hiB {
		t.Fatalf("BootstrapCI returned the IDENTICAL interval (%.10f, %.10f) for the same multiset in two different orders.\n"+
			"That is a FAILURE, not a pass: this test exists to prove the hazard is real, and the store's ORDER BY ts, id (#711, scoringOrderSQL) is justified by it.\n"+
			"If BootstrapCI has genuinely become order-independent, update scoringOrderSQL's rationale deliberately — do not delete this test.",
			loA, hiA)
	}

	// Report the magnitude so a reader sees this is not an ulp. A relative move
	// well above float noise is the whole point of the #711 escalation.
	relLo := (loB - loA) / loA
	if relLo < 0 {
		relLo = -relLo
	}
	t.Logf("order-only permutation moved the CI: (%.6f, %.6f) -> (%.6f, %.6f); relative move in ci_low = %.3e", loA, hiA, loB, hiB, relLo)
	if relLo < 1e-9 {
		t.Errorf("the two intervals differ, but ci_low moved by only %.3e relative — that is float noise, not the element-selection effect this test claims to demonstrate", relLo)
	}
}
