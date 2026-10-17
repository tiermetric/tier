#!/usr/bin/env bash
# Mandatory check-full gate and standalone local-review receipt. Never enables Event 3 scoring.
set -euo pipefail
cd "$(dirname "$0")/.."
GO_TEST_TIMEOUT="${GO_TEST_TIMEOUT:-30m}"
if [[ ! "$GO_TEST_TIMEOUT" =~ ^[1-9][0-9]*[smh]$ ]]; then
  echo 'GO_TEST_TIMEOUT must match ^[1-9][0-9]*(s|m|h)$' >&2
  exit 2
fi
scratch="$(mktemp -d "${TMPDIR:-/tmp}/event3-replay.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
printf 'Event 3 replay: commit=%s\n' "$(git rev-parse HEAD)"
if [[ -n "$(git status --porcelain)" ]]; then echo 'worktree=dirty (includes uncommitted replay authoring)'; else echo 'worktree=clean'; fi
printf 'rules=R-2026-10-14-19 D1/D2/D3 fixture_set=u5-d1-d2-d3-v1\n'
fixture=internal/integration/testdata/event3/adjudicated.jsonc
if command -v shasum >/dev/null 2>&1; then
  fixture_hash="$(shasum -a 256 "$fixture")"
else
  fixture_hash="$(sha256sum "$fixture")"
fi
printf '%s\n' "$fixture_hash"
if [[ "${fixture_hash%% *}" != 1ee66621df1b000461d7c2ebefacfdae1bc09ab0191802d4f514d18b51780d19 ]]; then
  echo 'EVENT3_REPLAY=TEST_RED; adjudicated fixture SHA-256 mismatch'
  exit 1
fi
echo 'scope=./internal/integration tags=event3replay non_gating=false'
if ! go test -c -timeout "$GO_TEST_TIMEOUT" -vet=off -tags event3replay -o "$scratch/replay.test" ./internal/integration; then
  echo 'EVENT3_REPLAY=COMPILE_RED; vet=NOT_RUN (compile failed; see diagnostics); tests=NOT_RUN'
  exit 1
fi
if ! go vet -tags event3replay ./internal/integration; then
  echo 'EVENT3_REPLAY=VET_RED; tests=NOT_RUN'
  exit 1
fi
# Reject missing/renamed tests and unexpected skips; an empty replay is never a receipt.
if ! "$scratch/replay.test" -test.list '^TestEvent3Replay_' > "$scratch/list"; then
  echo 'EVENT3_REPLAY=TEST_RED; replay test listing failed'
  exit 1
fi
count="$(awk '/^TestEvent3Replay_/ {n++} END {print n+0}' "$scratch/list")"
if [[ "$count" -lt 13 ]]; then
  echo "EVENT3_REPLAY=TEST_RED; only $count replay tests found (minimum 13)"
  exit 1
fi
status=0
go test -race -tags event3replay -count=1 -timeout "$GO_TEST_TIMEOUT" -v ./internal/integration -run '^TestEvent3Replay_' > "$scratch/results" 2>&1 || status=$?
cat "$scratch/results"
awk '
  /^--- PASS: TestEvent3Replay_/ {passed++}
  /^--- FAIL: TestEvent3Replay_/ {failed++}
  /--- SKIP:/ {skipped++}
  /    --- PASS: TestEvent3Replay_AdjudicatedFixtures\// {cases_passed++}
  /    --- FAIL: TestEvent3Replay_AdjudicatedFixtures\// {cases_failed++}
  END {printf "replay_tests: pass=%d fail=%d skip=%d; adjudicated_cases: pass=%d fail=%d\n", passed, failed, skipped, cases_passed, cases_failed}
' "$scratch/results"
# U1-U4 are merged: every replay assertion must run, with zero allowed skips.
if [[ "$status" -ne 0 ]] || ! awk '
  /--- SKIP:/ {skipped++}
  END {exit (skipped > 0)}
' "$scratch/results"; then
  echo 'EVENT3_REPLAY=TEST_RED; vet=GREEN (test failures or skipped replay cases)'
  exit 1
fi
echo 'EVENT3_REPLAY=GREEN; vet=GREEN; readers=store,headline,detail,segments,compare,rollups,export,manifest; api_path=unsealed-recompute; sealed_bytes=preserved'
