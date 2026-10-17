package opencode

import (
	"encoding/json"
	"fmt"

	"github.com/tiermetric/tier/internal/tokenacct"
)

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

// rawUsage preserves the collector's decoding convention: absent input and
// output have already been resolved to zero. The shared rule itself requires
// measured input and output; callers supplying raw nullable counts get
// ErrUnmeasured when either is absent.
func (u usage) rawUsage() tokenacct.RawUsage {
	raw := tokenacct.RawUsage{
		Input: &u.Input, Output: &u.Output, Reasoning: &u.Reasoning,
		CacheRead: &u.CacheRead, CacheWrite: &u.CacheWrite,
	}
	if u.TotalPresent {
		raw.Total = &u.Total
	}
	return raw
}

// AnyNonZero reports whether this row carries any token at all.
//
// ⚠️ IT CHECKS EACH FIELD, IT DOES NOT SUM THEM, and that is the point. Summing
// first lets a malformed row cancel itself out — `input=5, output=-5` sums to
// zero and would be filed under the QUIET zero-token counter instead of reaching
// the loud negative-count arm in tokenacct.Opencode. Both drop the row, so no
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
