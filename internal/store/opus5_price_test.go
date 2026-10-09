package store

import "testing"

// TestOpus5PricesExactly pins that claude-opus-5 resolves to an EXACT table entry
// at the audited $5/$25, not to the size-class guess.
//
// The control arm is the point. An exact hit and a heuristic fallback both return
// a plausible non-zero cost, so asserting "cost > 0" would pass either way — the
// silent-estimate hazard #267 exists to make observable. This test instead pins
// the exact expected micro-dollars, and asserts a deliberately unknown model
// produces a DIFFERENT cost, proving the table lookup is what answered.
func TestOpus5PricesExactly(t *testing.T) {
	// The control arm below deliberately drives the unknown-model guess path, which
	// WARNs to the default logger and inserts the model permanently into the
	// package-global unknownModelSeen map. Use the package's existing helpers so the
	// warning does not pollute test output and the entry does not leak into a later
	// test that asserts a distinct-model warn count.
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)

	const million = 1_000_000
	u := CostUsage{Input: million, Output: million}

	// $5.00/M input + $25.00/M output = $30.00 = 30_000_000 micro-dollars.
	const want = int64(30 * million)

	got := ComputeCost("claude-opus-5", u)
	if got != want {
		t.Fatalf("claude-opus-5 priced at %d micro-dollars, want %d ($5/$25 per M). "+
			"A mismatch here means the row is missing or wrong and Opus 5 traffic is "+
			"billing at a guessed rate", got, want)
	}

	// Opus 5 must match Opus 4.8 exactly — the generation advanced, the rate did not.
	if o48 := ComputeCost("claude-opus-4-8", u); got != o48 {
		t.Errorf("claude-opus-5 = %d but claude-opus-4-8 = %d; the two are documented as the same rate", got, o48)
	}

	// CONTROL: an unknown model must NOT land on the same number, or the assertion
	// above proves nothing about the lookup path.
	if guess := ComputeCost("claude-opus-99-nonexistent", u); guess == want {
		t.Fatalf("an unknown model also priced at %d — the heuristic fallback happens to "+
			"equal the audited rate, so this test cannot distinguish an exact hit from a guess", guess)
	}
}
