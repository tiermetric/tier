# Fixed-input partition replay (M-K1-C, #1233)

Run `go test -run '^TestPartitionReplay$' -v ./internal/integration`.
This harness reads no production data and changes no scoring or stored state.
It uses the production diff-weight heuristic and scoring engine with Event 3
scoring off, no correction declarations, quality 1, 1,200 changed lines,
8 changed files and $20 captured cost in every scenario. Each four-PR split
assigns 300 lines and 2 distinct files per PR; no file or line is duplicated.
The issue association alone differs between the two split scenarios.

| Scenario | PRs | Issues | Points | Points per $1,000 | Amplification |
|---|---:|---:|---:|---:|---:|
| Unsplit | 1 | 1 | 8 | 400 | 1× |
| Same-issue split | 4 | 1 | 20 | 1,000 | 2.5× |
| Cross-issue split | 4 | 4 | 20 | 1,000 | 2.5× |

The verbose test prints JSON receipts and asserts each result. This synthetic
fixture demonstrates sensitivity to partitioning under the current buckets;
it estimates neither how often splitting happens nor the dominance of one
split strategy. The scoring unit and weights are unchanged. Event 3 opt-in
allocation for confirmed corrections is a separate rule.
