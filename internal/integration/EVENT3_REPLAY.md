# Event 3 replay contract (U5, refs #1235)

`make event3-replay` is a mandatory `check-full` gate. All replay assertions must
pass with **zero skips**. A green replay never enables scoring; the operator
controls that separate switch.

Run from the repository root:

```sh
make event3-replay
# Equivalent scoped test run:
go test -race -tags event3replay -count=1 -timeout 30m -v ./internal/integration -run '^TestEvent3Replay_'
go vet -tags event3replay ./internal/integration
```

The script uses `${TMPDIR:-/tmp}` for disposable scratch files and respects the
caller's Go cache/temp environment. `GO_TEST_TIMEOUT` defaults to `30m`.
It verifies the pinned adjudicated fixture SHA-256, compiles the replay, runs
tagged vet, checks that at least 13 replay tests exist, and runs them with the
race detector. Compile, vet, and test failures have distinct `EVENT3_REPLAY`
labels; listing failures, missing tests, and any skip are test reds. It prints
the commit, dirty status, rules, fixture version/hash, test/case counts, and
verdict. Preserve complete output as the review receipt. The script exits 1
for a red; `make` reports a failed recipe as exit 2.

The 16 synthetic cases in `testdata/event3/adjudicated.jsonc` pin exact rational
projections, lifecycle dispositions, and hand arithmetic under rubric 2's
[corrective-work rules](../../docs/rubric.md#rubric-2-corrective-work-operator-opt-in). The oracle validates
canonical fractions and supplied sums; expected values are never generated
from production output. The fixture must remain byte-unchanged.

The replay opens isolated SQLite stores and drives public declaration,
`RecordPROutcome`, reconciliation, confirmation, dispute, withdrawal,
settlement, erasure, and scoring-policy operations. It covers same/cross-author
chains, reviewed growth, multi-parent dispositions, pending roots, floors,
sub-chain fractions, arrival order and duplicate delivery, quality evidence,
missing/excluded parents, time boundaries, historical placement, and bootstrap
sample floors. D3 dispute and post-confirm withdrawal retain allocations until
settlement.

Reader assertions cover persisted exact projections, headline/detail,
SampleN/OutcomeN, segments, company/team/division rollups, comparison, export,
manifest correction digests, and protected diagnostics. Developer comparison
totals retain the adjudicated 79/20 points and $4 per side; sub-k team/division
results assert suppression. PR102's dispute metadata must remain true after
both dispute and confirmed withdrawal in diagnostics, headline, detail, and
export. Flag-off readers retain legacy values and rubric 1.

API reads use `WithStoredScoringPolicy` and `WithUnsealedRecompute`, the verify
path, including team/division rollups. Sealed-byte preservation is checked
separately: the replay stores an API-produced June body through `SealReport`
and verifies its bytes/digest after late confirmation, dispute, withdrawal,
and erasure. It does not exercise serve's sealed-read path or concurrent seal
construction.
