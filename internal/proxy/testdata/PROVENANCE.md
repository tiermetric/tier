# Fixture provenance — `internal/proxy/testdata`

## 🔴 These fixtures are SYNTHETIC. They were NOT recorded from live traffic.

`openai-responses-completed.json` and `openai-responses-stream.sse` were written
by hand from the **documented** OpenAI Responses-API response/event schema while
implementing #459 task 2. No OpenAI credential exists in this workspace, so no
`/v1/responses` call was ever made, and no Codex CLI session was ever routed
through the proxy to produce them. Do not cite them as evidence that the wire
format is what tier thinks it is.

What they DO establish: that `parseOpenAIResponses` /
`openAIResponsesStreamParser` extract the intended numbers from the shape tier
believes it will see, that the cached carve-out is applied, and that the
Chat Completions path is unaffected by the discriminator. That is a parser
contract, not a wire observation.

### What is real, and how the token semantics were decided

The one load-bearing semantic choice — that `usage.input_tokens` is
**inclusive** of `usage.input_tokens_details.cached_tokens` rather than additive
— is not derived from these files. It comes from
`internal/collector/codexrollout`, which parses the same usage numbers out of
Codex's own rollout logs and whose `checkContainment` asserts, on every event of
every captured session and without ever tripping, both:

- `total_tokens == input_tokens + output_tokens`, and
- `cached_input_tokens <= input_tokens`.

Worked example — the first `token_count` event of
`codexrollout/testdata/rollout-duplicate-token-count.jsonl`: `input=17011`,
`cached=11008`, `output=118`, `total=17129`. And `17011 + 118 == 17129` exactly,
with a nonzero cached count. That **excludes** the additive reading in which
`total_tokens` counts every billable token — which would have to report `28137`
here.

What it does **not** exclude: a reading where cached sits outside `input_tokens`
*and* outside `total_tokens`. That one satisfies both identities, and is ruled
out by what the field is named rather than by the data. So this is strong
evidence, not a proof — the same hedge `codexrollout/parse.go` already makes
about the sibling `cache_write_input_tokens` field. What closes the residual is
that the rest of the tree already *prices* real Codex traffic on the inclusive
reading, plus the Chat Completions precedent (#114: OpenAI documents
`prompt_tokens` as inclusive of `prompt_tokens_details.cached_tokens`).

The same source establishes that `reasoning_output_tokens` is a subset of
`output_tokens`, which is why `output_tokens_details.reasoning_tokens` is
deliberately not read here.

Field names differ between the two surfaces (`cached_input_tokens` in the
rollout log vs. `input_tokens_details.cached_tokens` on the wire) — the rollout
log is Codex's re-serialization of the same Responses usage block, so this is
strong evidence about the semantics, not a byte-level capture of the wire.

### What would close the gap

#459 **task 3** — the env-gated live E2E (`TIER_LIVE_OPENAI_KEY`), currently
blocked on a provisioned key. When a real `/v1/responses` exchange is recorded,
drop it in beside these files, point the tests at it, and rewrite this note to
say which parts became observations.
