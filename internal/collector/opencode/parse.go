package opencode

import (
	"encoding/json"
	"fmt"
)

// maxMessageTokens is the absolute sanity ceiling on any single token class in
// one message. The additive identity below is RELATIONAL: an
// internally-consistent but absurd row (~4e18 in every class) satisfies it at any
// scale. Without a magnitude bound such a row prices to a clamped MaxInt64
// cost_micro (~$9.2 trillion), and two of those overflow SQLite's integer SUM(),
// which ERRORS rather than saturating — and SUM(cost_micro) backs /api/v1/scores,
// so one corrupt local row would take down the read plane.
//
// ⚠️ IT IS DELIBERATELY SMALLER THAN codexrollout's 1e12, AND THE DIFFERENCE IS
// NOT COSMETIC. That constant bounds a CUMULATIVE session series; this one bounds
// ONE message. More importantly, the counts here are int64 and are narrowed to
// `int` on the way into store.CostUsage — and `int` is 32 bits on a 32-bit build
// (`go install` on GOARCH=386/arm/mips is supported even though the release
// matrix is amd64/arm64 only). A 1e12 ceiling wraps there: a count of 3e9 narrows
// to a NEGATIVE int and is priced as one.
//
// 2^29 keeps every narrowing safe by construction on every platform Go supports:
// each class fits in an int32, and the one DERIVED value that is also narrowed —
// output + reasoning — is at most 2^30, still inside int32. The five-class Total
// can reach 5*2^29, which is why Total is compared as int64 and never narrowed.
//
// The bound is generous, not tight: 2^29 is 536,870,912 tokens in ONE message,
// ~1000x the largest single message ever observed here (~5e5) and about $2,300 at
// Z.ai's output rate for that one message. Nothing legitimate approaches it.
const maxMessageTokens = 1 << 29

// messageData is the subset of Opencode's `message.data` JSON blob this collector
// reads.
//
// 🔴 THERE IS DELIBERATELY NO `cost` FIELD. Every Opencode row carries `cost: 0`
// (subscription route; the client prices nothing), and importing that zero would
// persist a free-looking event whose tokens are real — the exact shape that makes
// work read as FREE and inflates TIER. Omitting the field from the struct makes
// the value structurally unreachable rather than merely unused: there is no
// variable holding it that a later edit could wire up by accident.
// TestNeverDecodesTheClientsCost pins that absence against the AST.
//
// Counts are *int64 so ABSENT is distinguishable from ZERO. That distinction is
// load-bearing: `tokens.total` is omitted entirely on rows Opencode aborted
// (measured: 13 of 9,267 assistant rows, every one of them all-zero), and a plain
// int64 would render those as total=0 and silently pass an identity check that
// was never actually performed.
type messageData struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Path       struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
	// Only `completed` is decoded. `time.created` exists in the blob and is NOT
	// declared here for the same reason `cost` is not: a decoded-but-unused field
	// is one edit away from being used, and this collector timestamps events by
	// COMPLETION — a row is created when a turn starts, long before its token
	// counts are final.
	Time struct {
		Completed *int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Total     *int64 `json:"total"`
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		Reasoning *int64 `json:"reasoning"`
		Cache     struct {
			Read  *int64 `json:"read"`
			Write *int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// roleAssistant is the only role that carries token usage. User rows exist in the
// same table and carry no `tokens` object at all.
const roleAssistant = "assistant"

// usage is one message's token counts, resolved from the optional JSON fields.
// TotalPresent records whether `tokens.total` was in the blob, because the
// identity check is only meaningful when it was.
type usage struct {
	Total        int64
	Input        int64
	Output       int64
	Reasoning    int64
	CacheRead    int64
	CacheWrite   int64
	TotalPresent bool
}

// BillableParts returns the sum of every class that participates in the additive
// identity. Named rather than inlined so the check below and the error message it
// produces cannot drift apart.
func (u usage) BillableParts() int64 {
	return u.Input + u.Output + u.Reasoning + u.CacheRead + u.CacheWrite
}

// AnyNonZero reports whether this row carries any token at all.
//
// ⚠️ IT CHECKS EACH FIELD, IT DOES NOT SUM THEM, and that is the point. Summing
// first lets a malformed row cancel itself out — `input=5, output=-5` sums to
// zero and would be filed under the QUIET zero-token counter instead of reaching
// the loud negative-count arm in checkAdditiveIdentity. Both drop the row, so no
// wrong money either way; the difference is whether anyone finds out. Summing
// five near-MaxInt64 values would also overflow int64 before any ceiling check
// had a chance to run.
func (u usage) AnyNonZero() bool {
	return u.Total != 0 || u.Input != 0 || u.Output != 0 ||
		u.Reasoning != 0 || u.CacheRead != 0 || u.CacheWrite != 0
}

// decodeMessage unmarshals one `message.data` blob.
func decodeMessage(raw []byte) (messageData, error) {
	var m messageData
	if err := json.Unmarshal(raw, &m); err != nil {
		return messageData{}, fmt.Errorf("decode message JSON: %w", err)
	}
	return m, nil
}

// readUsage resolves the optional token fields into a usage.
func readUsage(m messageData) usage {
	deref := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	u := usage{
		Input:        deref(m.Tokens.Input),
		Output:       deref(m.Tokens.Output),
		Reasoning:    deref(m.Tokens.Reasoning),
		CacheRead:    deref(m.Tokens.Cache.Read),
		CacheWrite:   deref(m.Tokens.Cache.Write),
		TotalPresent: m.Tokens.Total != nil,
	}
	u.Total = deref(m.Tokens.Total)
	return u
}

// checkAdditiveIdentity asserts the invariants an Opencode row's own numbers must
// satisfy before we are willing to price it.
//
// 🔴 THE IDENTITY IS ADDITIVE, WHICH IS THE OPPOSITE OF codexrollout's
// checkContainment. Codex asserts `reasoning <= output` because Codex's reasoning
// count sits INSIDE its output count; asserting that here would reject 77% of the
// real corpus, where reasoning routinely exceeds output several-fold. What holds
// here — on 3,943 of 3,943 completed GLM-5.3 rows — is
//
//	total == input + output + reasoning + cache.read + cache.write
//
// ⚠️ THE `cache.write` TERM IS UNMEASURED, and saying so precisely matters.
// cache.write is 0 on every one of the 9,267 assistant rows in the maintainer's
// store — every provider, every model — so the identity as measured is really the
// four-term one, and the fifth term is a prediction about a shape nobody has
// observed. It is included rather than ignored because the alternative is worse:
// if Opencode ever reports a cache write and counts it in `total`, a four-term
// check would reject that row, while a five-term check prices it. If instead it
// reports a write OUTSIDE `total`, the five-term check rejects it loudly — which
// is the correct outcome, because we would then not know whether the write is
// billable, and this collector's contract is that no figure beats a wrong figure.
//
// ⚠️ WHAT THIS CHECK CANNOT SEE. A Codex-shaped row whose reasoning happens to be
// ZERO satisfies the additive identity too (total == input + output, and adding a
// zero reasoning changes nothing), so it is accepted and priced identically under
// either convention. That is not a gap: where the two conventions agree, the
// arithmetic agrees, and there is nothing to disambiguate. The check bites on
// exactly the rows where they disagree.
//
// A violation is a SKIP, not a fatal, and the caller logs it at WARN with a
// counter — the same posture codexrollout uses for implausible usage
// (parse.go's checkContainment) with one deliberate difference: Codex fails the
// whole FILE because its per-call values are derived by differencing a cumulative
// series, so one bad snapshot poisons its neighbours. Opencode rows are
// independent per-message facts, so a bad row costs exactly itself.
func checkAdditiveIdentity(u usage) error {
	if u.Input < 0 || u.Output < 0 || u.Reasoning < 0 || u.CacheRead < 0 ||
		u.CacheWrite < 0 || u.Total < 0 {
		return fmt.Errorf("implausible usage: a negative token count (total=%d input=%d output=%d reasoning=%d cache_read=%d cache_write=%d)",
			u.Total, u.Input, u.Output, u.Reasoning, u.CacheRead, u.CacheWrite)
	}
	if u.Input > maxMessageTokens || u.Output > maxMessageTokens ||
		u.Reasoning > maxMessageTokens || u.CacheRead > maxMessageTokens ||
		u.CacheWrite > maxMessageTokens || u.Total > maxMessageTokens {
		return fmt.Errorf("implausible usage: a token count exceeds the %d-token sanity ceiling (total=%d input=%d output=%d reasoning=%d cache_read=%d cache_write=%d)",
			maxMessageTokens, u.Total, u.Input, u.Output, u.Reasoning, u.CacheRead, u.CacheWrite)
	}
	// The identity is only checkable when `total` was actually present. A row
	// with no total and NO tokens at all is an aborted turn and is dropped
	// earlier as zero-token; a row with no total but real tokens reaches the
	// caller's unverifiable arm, which is loud. Neither is silently trusted.
	if !u.TotalPresent {
		return nil
	}
	if got := u.BillableParts(); u.Total != got {
		return fmt.Errorf("additive identity broken: total=%d != input+output+reasoning+cache.read+cache.write=%d "+
			"(input=%d output=%d reasoning=%d cache_read=%d cache_write=%d). "+
			"Opencode's reasoning tokens sit BESIDE output, not inside it; a row that does not sum is a shape this collector has never seen and cannot price honestly",
			u.Total, got, u.Input, u.Output, u.Reasoning, u.CacheRead, u.CacheWrite)
	}
	return nil
}
