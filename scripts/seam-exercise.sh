#!/usr/bin/env bash
# seam-exercise.sh — hermetic wire_check for the tier-jsonl-ingestion contract
# (captured-sample class).
# Replays a captured, scrubbed CURRENT-format Claude Code JSONL sample
# through a freshly built tier binary and asserts the ingestion contract:
#
#   PASS iff TOTAL != $0  AND  the parser reported empty_cwd == 0
#   plus the captured-sample negative_guard: the sample must NOT be stale
#   (sample_age <= max_sample_age).
#
# Hermetic: no network, no credentials, no live service — only the Go toolchain,
# the tier binary, and the committed sample. Exits non-zero on any failure so it
# can drive a CI step or the Phase-2 e2e gate.
set -euo pipefail

# Source of truth is class_config.max_sample_age in the contract definition;
# kept in sync here.
MAX_SAMPLE_AGE_DAYS=90

# Staleness policy. Default "fail" (fail-loud house posture): a captured sample
# aged past max_sample_age is a hard error. A manual
# `make seam-exercise` both use this default, so a stale sample still cannot
# false-green there. `make check-full` alone sets SEAM_STALE_POLICY=warn: age-out
# is a wall-clock condition (not a code regression), and it must never hard-break
# the mandatory pre-push gate for an UNRELATED PR the day the sample ages out.
# In warn mode staleness prints a loud warning and continues; the actual
# ingestion-contract asserts below (TOTAL != $0, empty_cwd == 0 — the #96 drift
# guard) stay hard failures in BOTH modes, so real drift is still caught in
# check-full. Re-capturing the sample clears the warning.
SEAM_STALE_POLICY="${SEAM_STALE_POLICY:-fail}"
case "$SEAM_STALE_POLICY" in
  fail|warn) : ;;
  *) echo "FAIL: SEAM_STALE_POLICY must be 'fail' or 'warn', got: '$SEAM_STALE_POLICY'" >&2; exit 1 ;;
esac

REPO="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE_DIR="$REPO/testdata/seam-jsonl-ingestion"
SAMPLE="$FIXTURE_DIR/sample.jsonl"
LAST_CAPTURED_FILE="$FIXTURE_DIR/last_captured"

fail() { echo "FAIL: $*" >&2; exit 1; }

[ -f "$SAMPLE" ] || fail "missing captured sample: $SAMPLE"
[ -f "$LAST_CAPTURED_FILE" ] || fail "missing last_captured marker: $LAST_CAPTURED_FILE"

# Portable YYYY-MM-DD -> epoch seconds (GNU date first, then BSD/macOS date).
# Keep this as bare `date ... || date ...` (NOT `local e=$(...)`, whose rc is the
# `local` builtin's, masking failure). The strict format check below guarantees
# the input is well-formed, so BSD `date -j -f`'s lenient trailing-garbage
# parsing can't diverge from GNU `date -d`.
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f "%Y-%m-%d" "$1" +%s 2>/dev/null
}

# --- can't-run-here preflight: tierd score's validateGitRepo requires a real
# .git DIRECTORY, but a git worktree (the standard dev/gate context) has a .git
# FILE (a gitdir pointer), and a bare tarball export has no .git at all. Detect
# that specific precondition explicitly BEFORE build/score. In warn mode
# (check-full) skip-with-warning so the mandatory pre-push gate for an unrelated
# PR never hard-fails just because it runs from a worktree; in fail mode (the
# a standalone `make seam-exercise`) keeps it fail-loud. This is
# narrower than the staleness escape hatch below (which covers only sample age)
# and must NOT be conflated with a non-zero tierd score, which stays a hard
# failure so genuine ingestion drift is never masked.
if [ ! -d "$REPO/.git" ]; then          # worktree (.git is a file) or non-repo
  if [ "$SEAM_STALE_POLICY" = "warn" ]; then
    echo "WARN: $REPO/.git is not a directory (git worktree?) — tierd score's validateGitRepo needs a real .git dir; SKIPPING seam-exercise. Run 'make check-full' in a fresh clone to exercise the seam." >&2
    exit 0
  fi
  fail "$REPO/.git is not a directory — run in a real clone, not a worktree"
fi

# --- negative_guard: staleness. A stale captured sample must not false-green. ---
captured="$(tr -d '[:space:]' < "$LAST_CAPTURED_FILE")"
# Validate strictly BEFORE date: BSD `date -j -f` would otherwise silently
# prefix-parse a malformed marker (e.g. "2026-06-22-x") to a wrong epoch on
# macOS while GNU rejects it on linux1 — host divergence in the very guard meant
# to prevent false-greens.
case "$captured" in
  [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) : ;;
  *) fail "last_captured must be YYYY-MM-DD, got: '$captured'" ;;
esac
captured_epoch="$(to_epoch "$captured")" || fail "unparseable last_captured: '$captured'"
now_epoch="$(date -u +%s)"
age_days=$(( (now_epoch - captured_epoch) / 86400 ))
if [ "$age_days" -gt "$MAX_SAMPLE_AGE_DAYS" ]; then
  stale_msg="captured sample is stale: age ${age_days}d > max_sample_age ${MAX_SAMPLE_AGE_DAYS}d (re-capture via scripts/seam-capture.py — see testdata/seam-jsonl-ingestion/README.md 'Refreshing the sample' — and re-run make seam-exercise)"
  if [ "$SEAM_STALE_POLICY" = "warn" ]; then
    # check-full path: warn, do not hard-break the gate for an unrelated PR.
    echo "WARN: $stale_msg" >&2
    echo "WARN: SEAM_STALE_POLICY=warn — continuing; the ingestion-contract check below still hard-fails on real format drift." >&2
  else
    fail "$stale_msg"
  fi
else
  echo "sample age: ${age_days}d (<= ${MAX_SAMPLE_AGE_DAYS}d) — fresh"
fi

# --- build tier ---
echo "building tierd..."
( cd "$REPO" && go build -o bin/tierd ./cmd/tierd ) || fail "go build failed"

# --- stage a hermetic claude-dir with the placeholder cwd bound to this repo ---
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/projects/seam-exercise"
# Bind __TIER_REPO__ to the real checkout so the session's cwd matches --repo.
# Use | as the sed delimiter since the replacement is a filesystem path.
sed "s|__TIER_REPO__|$REPO|g" "$SAMPLE" > "$TMP/projects/seam-exercise/sample.jsonl"

# --- run the wire_check ---
echo "running: tierd score --repo \$REPO --claude-dir \$TMP --since 2026-01-01"
set +e
out="$("$REPO/bin/tierd" score --repo "$REPO" --claude-dir "$TMP" --since 2026-01-01 2> "$TMP/stderr.log")"
rc=$?
set -e
[ "$rc" -eq 0 ] || { cat "$TMP/stderr.log" >&2; fail "tierd score exited $rc"; }

# empty_cwd is logged by the collector (slog key "empty_cwd", text or JSON form)
# only when a session is dropped for an empty cwd; a value > 0 is the #96
# false-green. Absence of the log line == 0 dropped.
if grep -q "empty_cwd" "$TMP/stderr.log"; then
  empty="$(sed -n 's/.*empty_cwd[=:][[:space:]]*\([0-9][0-9]*\).*/\1/p' "$TMP/stderr.log" | head -1)"
  if [ -n "$empty" ] && [ "$empty" -ne 0 ]; then
    cat "$TMP/stderr.log" >&2
    fail "parser dropped ${empty} session(s) with empty cwd (the #96 trap)"
  fi
fi

# TOTAL line: "TOTAL                <number>". Anchor on the exact first field so
# a future row like "TOTAL COST" can't be mismatched.
total="$(printf '%s\n' "$out" | awk '$1=="TOTAL"{print $2}')"
[ -n "$total" ] || { printf '%s\n' "$out" >&2; fail "no TOTAL line in score output"; }

# Non-zero check without bc: strip a leading 0/./- and see if any nonzero digit
# remains (e.g. 0.0000 -> fail; 0.5512 -> pass).
if printf '%s' "$total" | grep -Eq '[1-9]'; then
  echo "PASS: TOTAL=\$${total} (non-zero) AND empty_cwd==0 — JSONL ingestion contract holds"
  exit 0
fi
printf '%s\n' "$out" >&2
fail "TOTAL is \$${total} (zero) — current-format session recorded \$0 (the #96 class)"
