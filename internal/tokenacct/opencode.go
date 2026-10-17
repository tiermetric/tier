package tokenacct

import "fmt"

// OpencodeRuleVersion identifies the Opencode accounting rule.
const OpencodeRuleVersion = 1

// maxMessageTokens is the absolute sanity ceiling on any single token class in
// one message. The additive identity below is RELATIONAL: an
// internally-consistent but absurd row (~4e18 in every class) satisfies it at any
// scale. Without a magnitude bound such a row prices to a clamped MaxInt64
// cost_micro (~$9.2 trillion), and two of those overflow SQLite's integer SUM(),
// which ERRORS rather than saturating — and SUM(cost_micro) backs /api/v1/scores,
// so one corrupt local row would take down the read plane.
//
// ⚠️ IT IS DELIBERATELY SMALLER THAN maxCumulativeTokens (Codex's 1e12), AND THE
// DIFFERENCE IS NOT COSMETIC. That constant bounds a CUMULATIVE session series;
// this one bounds ONE message. More importantly, the counts here are int64 and
// are narrowed to int in Usage — and `int` is 32 bits on a 32-bit build
// (`go install` on GOARCH=386/arm/mips is supported even though the release
// matrix is amd64/arm64 only). A 1e12 ceiling wraps there: a count of 3e9 narrows
// to a NEGATIVE int and is priced as one.
//
// 2^29 keeps every narrowing safe by construction on every platform Go supports:
// each class fits in an int32, and the one DERIVED value that is also narrowed —
// output + reasoning — is at most 2^30, still inside int32. The five-class sum
// can reach 5*2^29, which is why it is compared as int64 and never narrowed.
// Total itself retains the same 2^29 ceiling as each individual class.
//
// The bound is generous, not tight: 2^29 is 536,870,912 tokens in ONE message,
// ~1000x the largest single message ever observed here (~5e5) and about $2,300 at
// Z.ai's output rate for that one message. Nothing legitimate approaches it.
const maxMessageTokens = 1 << 29

// Opencode returns disjoint billable counts: Input and both cache classes pass
// through, and Output = output + reasoning.
//
// # Guarantees
//
// Nil Input or Output, or nil Reasoning together with nil Total, returns
// ErrUnmeasured; other nil counts are zero. When Total is present,
// total == input + output + reasoning + cache.read + cache.write, or an error
// is returned. A nil Total skips the cross-check; a present zero Total IS checked
// (unlike Codex). Output = output + reasoning; cache counts pass through and are
// never subtracted from Input. Negative counts and counts above the per-message
// ceiling are refused before arithmetic or narrowing to int.
//
// # 🔴 READ THIS BEFORE COPYING ANYTHING FROM codexrollout
//
// The two sources report reasoning tokens under OPPOSITE conventions, and an
// ingester written by analogy is not slightly wrong, it is wrong by most of the
// output-side bill.
//
//	Codex     reasoning ⊆ output           tokenacct.Codex asserts it; nothing adds it.
//	Opencode  reasoning BESIDE output      total = input + output + reasoning + cache.read
//
// (The check the code actually runs carries a fifth term, `+ cache.write`. It is
// UNMEASURED — cache.write is 0 on every one of the 9,267 assistant rows in the
// maintainer's store — see Assumptions below for why it is included
// anyway rather than assumed away.)
//
// Measured over every GLM-5.3 assistant row in the maintainer's store
// (2026-07-27 → 2026-08-28, 3,943 completed rows):
//
//	rows  total == in+out+reasoning+cache_read   reasoning > output
//	3943            3943  (100%)                       3025  (77%)
//
// The consequences of getting it backwards, both measured on that corpus:
//
//   - Mapping OutputTok = output alone drops 4,415,226 of 5,203,917 output-side
//     tokens — 84.8% of the output bill.
//   - Reusing Codex's `reasoning <= output` containment check REJECTS 77% of the
//     corpus, because here reasoning routinely exceeds output several-fold.
//
// So Opencode maps Output = output + reasoning (the Gemini
// `thoughtsTokenCount` convention TokenEvent already documents), and its
// containment check is the ADDITIVE identity, not a subset assertion.
//
// # Assumptions
//
// ⚠️ The pricing side of that convention is UNVERIFIED: Z.ai does not publish
// whether reasoning tokens bill at the output rate. What IS verified is the
// arithmetic — the tokens exist, they are not inside `output`, and dropping them
// would under-report. Folding them into Output is the convention this tree
// already uses for additive reasoning; if Z.ai ever publishes a separate
// reasoning rate, that is a price-table change, not a change here.
//
// The five-term identity assumes cache writes belong in total.
// cache.write is 0 on every one of the 9,267 assistant rows in the maintainer's
// store — every provider, every model — so the identity as measured is really the
// four-term one, and the fifth term is a prediction about a shape nobody has
// observed. It is included rather than ignored because the alternative is worse:
// if Opencode ever reports a cache write and counts it in `total`, a four-term
// check would reject that row, while a five-term check prices it. If instead it
// reports a write OUTSIDE `total`, the five-term check rejects it loudly — which
// is the correct outcome, because we would then not know whether the write is
// billable, and TIER's contract is that no figure beats a wrong figure.
//
// # Limits of the cross-check
//
// ⚠️ WHAT THIS CHECK CANNOT SEE. A Codex-shaped row whose reasoning happens to be
// ZERO satisfies the additive identity too (total == input + output, and adding a
// zero reasoning changes nothing), so it is accepted and priced identically under
// either convention. That is not a gap: where the two conventions agree, the
// arithmetic agrees, and there is nothing to disambiguate. The check bites on
// exactly the rows where they disagree.
func Opencode(raw RawUsage) (Usage, error) {
	if raw.Input == nil || raw.Output == nil || (raw.Reasoning == nil && raw.Total == nil) {
		return Usage{}, ErrUnmeasured
	}
	input, output, reasoning := count(raw.Input), count(raw.Output), count(raw.Reasoning)
	cacheRead, cacheWrite, total := count(raw.CacheRead), count(raw.CacheWrite), count(raw.Total)
	if input < 0 || output < 0 || reasoning < 0 || cacheRead < 0 ||
		cacheWrite < 0 || total < 0 {
		return Usage{}, fmt.Errorf("implausible usage: a negative token count (total=%d input=%d output=%d reasoning=%d cache_read=%d cache_write=%d)",
			total, input, output, reasoning, cacheRead, cacheWrite)
	}
	if input > maxMessageTokens || output > maxMessageTokens ||
		reasoning > maxMessageTokens || cacheRead > maxMessageTokens ||
		cacheWrite > maxMessageTokens || total > maxMessageTokens {
		return Usage{}, fmt.Errorf("implausible usage: a token count exceeds the %d-token sanity ceiling (total=%d input=%d output=%d reasoning=%d cache_read=%d cache_write=%d)",
			maxMessageTokens, total, input, output, reasoning, cacheRead, cacheWrite)
	}
	// Sum in int64: the five bounded terms can exceed a 32-bit int.
	if got := input + output + reasoning + cacheRead + cacheWrite; raw.Total != nil && total != got {
		return Usage{}, fmt.Errorf("additive identity broken: total=%d != input+output+reasoning+cache.read+cache.write=%d "+
			"(input=%d output=%d reasoning=%d cache_read=%d cache_write=%d). "+
			"Opencode's reasoning tokens sit BESIDE output, not inside it; a row that does not sum is a shape this collector has never seen and cannot price honestly",
			total, got, input, output, reasoning, cacheRead, cacheWrite)
	}
	// Each class is at most 2^29 and output + reasoning at most 2^30, so
	// every billable count fits in int on all supported platforms.
	return Usage{Input: int(input), Output: int(output + reasoning), CacheRead: int(cacheRead), CacheWrite: int(cacheWrite)}, nil
}
