// Package tokenacct normalises raw engine usage into disjoint billable counts.
package tokenacct

import (
	"errors"
	"fmt"

	"github.com/tiermetric/tier/internal/store"
)

// RawUsage carries the five nullable counts exported by review-run usage.
// Input and Output must be measured; other nil counts are absent and count as
// zero, subject to each engine's requirements. Total is an optional cross-check
// supplied by Codex rollout logs and Opencode messages; turn.completed usage
// does not supply it.
// CacheWrite is retained for other engines; Codex() currently omits a separate
// write class. Its billing meaning and containment are unvalidated for that route.
type RawUsage struct {
	Input      *int64 `json:"tokens_input"`
	Output     *int64 `json:"tokens_output"`
	Reasoning  *int64 `json:"tokens_reasoning"`
	CacheRead  *int64 `json:"tokens_cache_read"`
	CacheWrite *int64 `json:"tokens_cache_write"`
	Total      *int64 `json:"-"`
}

// Usage contains normalised counts: Input is fresh input, and each cache class
// is disjoint from it. Output includes reasoning for both engines, by opposite
// rules: Codex reports reasoning inside output (contained, never added); Opencode
// reports it separately and Opencode() adds it.
type Usage struct {
	Input      int
	CacheRead  int
	CacheWrite int
	Output     int
}

// ErrUnmeasured identifies usage that cannot be priced because a required count
// is missing: input or output (every engine), or, for Opencode, reasoning when
// no total is present to reconcile against.
var ErrUnmeasured = errors.New("unmeasured usage: a required token count is missing")

// CodexRuleVersion identifies the Codex accounting rule.
const CodexRuleVersion = 1

// maxCumulativeTokens is the absolute sanity ceiling on any single cumulative
// count in one session (see Codex). 1e12 is ~10^6 times the largest session
// observed locally (the biggest of the 31 captured rollouts is well under 10^6
// tokens) and still orders of magnitude below the int64 cost overflow it exists
// to prevent, so it can only ever reject a corrupt file.
const maxCumulativeTokens = store.MaxTokenCount

// Codex returns disjoint billable counts: Input = input - cached, and reasoning
// is never added to Output. Nil Input or Output returns ErrUnmeasured; other nil
// counts are zero, and the total check is skipped when Total is nil or zero.
// CacheWrite is checked for negativity but is not carried into Usage. Separate
// pricing and containment in Input are not yet validated for this route; the
// total identity does not prove containment. The current calculation does not
// account for a separate write class (see the warn-once in
// internal/collector/codexrollout/parse.go).
// Codex asserts the invariants Codex's own numbers must satisfy,
// matching score.py from the tiermetric/model-bench repository — the working
// reference implementation that priced the Codex half of the TIER Model Report.
// (That working copy is .gitignore'd here, so the citation is to the repo, not
// to a path that survives a fresh clone.)
//
// These are FATAL, not warnings, by explicit design (#464). Every one of them
// failing means the log is self-contradictory, and any cost we computed from it
// would be a confident wrong number. No figure beats a wrong figure.
//
// THREE RELATIONAL CHECKS PLUS NEGATIVE AND ABSOLUTE REFUSALS. The relational
// checks below only assert the fields agree with EACH OTHER, so an internally-consistent but
// absurd snapshot (~4e18 input and output) satisfies every one of them; the
// ceiling is what bounds magnitude. Without it such a row prices to a clamped
// MaxInt64 cost_micro (~$9.2 trillion), and two of those overflow SQLite's
// integer SUM() — which ERRORS rather than saturating, and SUM(cost_micro) backs
// /api/v1/scores, so one corrupt local file would take down the read plane.
//
// CAVEAT worth knowing before trusting a session: total == input + output is the
// strongest of these (it is the only one that cross-checks the two classes we
// actually bill), and it is SKIPPED when total_tokens is absent or zero. A Codex
// build that stopped emitting total_tokens would therefore keep passing while
// losing the check that matters most — visible only as this comment, since the
// degradation is deliberate (see below).
func Codex(raw RawUsage) (Usage, error) {
	if raw.Input == nil || raw.Output == nil {
		return Usage{}, ErrUnmeasured
	}
	input, cached, output := count(raw.Input), count(raw.CacheRead), count(raw.Output)
	reasoning, total := count(raw.Reasoning), count(raw.Total)
	cacheWrite := count(raw.CacheWrite)
	if input < 0 || output < 0 || cached < 0 ||
		reasoning < 0 || total < 0 || cacheWrite < 0 {
		return Usage{}, fmt.Errorf("implausible usage: a negative token count (input=%d cached=%d output=%d reasoning=%d total=%d cache_write=%d)",
			input, cached, output, reasoning, total, cacheWrite)
	}
	// Absolute sanity ceiling, checked after negative counts because it bounds
	// magnitude while the relational checks below can all pass at any scale.
	// 1e12 cumulative tokens is ~10^6x the largest observed session and over $1M
	// at current Codex rates: nothing legitimate approaches it, and anything that
	// does is a corrupt file, not a bill.
	if input > maxCumulativeTokens || output > maxCumulativeTokens ||
		cached > maxCumulativeTokens || total > maxCumulativeTokens {
		return Usage{}, fmt.Errorf("implausible usage: a cumulative count exceeds the %d-token sanity ceiling (input=%d cached=%d output=%d total=%d)",
			int64(maxCumulativeTokens), input, cached, output, total)
	}
	// total == input + output. Guarded on total != 0 (as score.py is) so a
	// future Codex build that omits total_tokens degrades to "unverified" rather
	// than failing every session. cached and reasoning are SUBSETS of input and
	// output respectively, which is exactly why they are absent from this sum.
	if total != 0 && total != input+output {
		return Usage{}, fmt.Errorf("containment broken: total_tokens=%d != input_tokens+output_tokens=%d",
			total, input+output)
	}
	// cached ⊆ input. If this ever failed, carving cached out of input would
	// produce a NEGATIVE fresh-input count.
	if cached > input {
		return Usage{}, fmt.Errorf("containment broken: cached_input_tokens=%d > input_tokens=%d",
			cached, input)
	}
	// reasoning ⊆ output. We never add reasoning on top of output; this asserts
	// the assumption that makes that correct.
	if reasoning > output {
		return Usage{}, fmt.Errorf("containment broken: reasoning_output_tokens=%d > output_tokens=%d",
			reasoning, output)
	}
	// The sanity ceiling exceeds a 32-bit int; refuse counts that cannot be
	// represented in Usage before narrowing them.
	maxInt := int64(int(^uint(0) >> 1))
	if input-cached > maxInt || cached > maxInt || output > maxInt {
		return Usage{}, fmt.Errorf("implausible usage: a billable count exceeds the %d-token int range (input=%d cached=%d output=%d)",
			maxInt, input-cached, cached, output)
	}
	// Carve cached out of input so the two token classes never overlap —
	// identical to the proxy's parseOpenAI. Codex's input_tokens is
	// INCLUSIVE of cached_input_tokens (Responses API semantics), and the
	// price table applies a separate cache-read multiplier, so leaving them
	// overlapped would bill the cached prefix at both rates.
	return Usage{Input: int(input - cached), CacheRead: int(cached), Output: int(output)}, nil
}

func count(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
