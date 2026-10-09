#!/usr/bin/env bash
# seam-exercise.sh — hermetic wire_check for the tier-jsonl-ingestion contract
# (captured-sample class).
# Replays a captured, scrubbed CURRENT-format Claude Code JSONL sample
# through a freshly built tier binary and asserts the ingestion contract:
#
#   PASS iff TOTAL != $0  AND  the parser reported empty_cwd == 0
#                         AND  the price-table table_hash matches
#                              ^tierpt1:[0-9a-f]{64}$ (#713)
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
# `make seam-exercise` uses this default, so a stale sample still cannot
# false-green there. `make check-full` alone sets SEAM_STALE_POLICY=warn: age-out
# is a wall-clock condition (not a code regression), and it must never hard-break
# the mandatory pre-push gate for an UNRELATED PR the day the sample ages out.
# In warn mode staleness prints a loud warning and continues; the actual
# ingestion-contract asserts below (TOTAL != $0, empty_cwd == 0 — the #96 drift
# guard — and the #713 table_hash shape) stay hard failures in BOTH modes, so
# real drift is still caught in check-full. Re-capturing the sample clears the
# warning. The warn carve-out is scoped to WALL-CLOCK conditions only: a sample
# aging out is not a code regression, whereas a malformed table_hash is, which
# is why the #713 assert does not consult SEAM_STALE_POLICY at all.
SEAM_STALE_POLICY="${SEAM_STALE_POLICY:-fail}"
case "$SEAM_STALE_POLICY" in
  fail|warn) : ;;
  *) echo "FAIL: SEAM_STALE_POLICY must be 'fail' or 'warn', got: '$SEAM_STALE_POLICY'" >&2; exit 1 ;;
esac

REPO="$(cd "$(dirname "$0")/.." && pwd -P)"
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

# --- can't-run-here preflight: resolve both normal clones' .git directories
# and worktrees' gitdir pointer files. Require a local .git entry and compare
# physical paths so a nested export cannot borrow an ancestor's checkout.
# Hook environments can redirect git despite -C; judge this script's checkout.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR
# A non-repo export still skips in warn
# mode and fails in fail mode, before build/score. A non-zero tierd score stays
# a hard failure in both modes so genuine ingestion drift is never masked.
if { [ ! -f "$REPO/.git" ] && [ ! -d "$REPO/.git" ]; } ||
   ! git_dir="$(git -C "$REPO" rev-parse --absolute-git-dir 2>/dev/null)" ||
   [ ! -d "$git_dir" ] ||
   ! git_toplevel="$(git -C "$REPO" rev-parse --show-toplevel 2>/dev/null)" ||
   ! git_toplevel="$(cd "$git_toplevel" && pwd -P)" ||
   [ "$git_toplevel" != "$REPO" ]; then
  if [ "$SEAM_STALE_POLICY" = "warn" ]; then
    echo "WARN: cannot resolve a git directory for $REPO; SKIPPING seam-exercise. Run 'make check-full' in a git checkout to exercise the seam." >&2
    exit 0
  fi
  fail "cannot resolve a git directory for $REPO — run in a git checkout"
fi

# --- negative_guard: staleness. A stale captured sample must not false-green. ---
captured="$(tr -d '[:space:]' < "$LAST_CAPTURED_FILE")"
# Validate strictly BEFORE date: BSD `date -j -f` would otherwise silently
# prefix-parse a malformed marker (e.g. "2026-06-22-x") to a wrong epoch on
# macOS while GNU rejects it on the Linux gate host — host divergence in the
# very guard meant to prevent false-greens.
case "$captured" in
  [0-9][0-9][0-9][0-9]-0[1-9]-0[1-9] | \
  [0-9][0-9][0-9][0-9]-0[1-9]-[12][0-9] | \
  [0-9][0-9][0-9][0-9]-0[1-9]-3[01] | \
  [0-9][0-9][0-9][0-9]-1[012]-0[1-9] | \
  [0-9][0-9][0-9][0-9]-1[012]-[12][0-9] | \
  [0-9][0-9][0-9][0-9]-1[012]-3[01]) : ;;
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
# Read via ENVIRON (awk -v interprets backslashes), JSON-escape each character,
# and splice literally (awk/sed replacement strings interpret & and backslashes).
TIER_REPO_PATH="$REPO" awk '
  BEGIN {
    for (i = 1; i < 32; i++) escapes[sprintf("%c", i)] = sprintf("\\u%04x", i)
    escapes["\""] = "\\\""
    escapes["\\"] = "\\\\"
    path = ENVIRON["TIER_REPO_PATH"]
    for (i = 1; i <= length(path); i++) {
      c = substr(path, i, 1)
      repo = repo ((c in escapes) ? escapes[c] : c)
    }
    token = "__TIER_REPO__"
  }
  {
    line = $0
    while ((pos = index(line, token)) != 0) {
      printf "%s%s", substr(line, 1, pos - 1), repo
      line = substr(line, pos + length(token))
    }
    print line
  }
' "$SAMPLE" > "$TMP/projects/seam-exercise/sample.jsonl"

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
# Check every diagnostic, including malformed ones following a parsed zero.
while IFS= read -r diagnostic || [ -n "$diagnostic" ]; do
  case "$diagnostic" in *empty_cwd*) ;; *) continue ;; esac
  empty="$(printf '%s\n' "$diagnostic" | sed -En '/empty_cwd"?[[:space:]]*[=:][[:space:]]*([0-9]+)([[:space:],}]|$)/{s/.*empty_cwd"?[[:space:]]*[=:][[:space:]]*([0-9]+)([[:space:],}]|$).*/\1/p;q;}')"
  if [ -z "$empty" ]; then
    cat "$TMP/stderr.log" >&2
    fail "unparseable empty_cwd diagnostic (the #96 trap)"
  fi
  case "$empty" in
    *[1-9]*)
      cat "$TMP/stderr.log" >&2
      fail "parser dropped ${empty} session(s) with empty cwd (the #96 trap)"
      ;;
  esac
done < "$TMP/stderr.log"

# --- price-table content identity (#713) ---
# tierd prints "price table identity: table_hash=tierpt1:<64 hex> file_hash=..."
# on stderr right after the version stamp. Pin the SHAPE here, outside `go test`:
# the scheme tag is the rollback seam a later canonicalization fix depends on, and
# a hash silently reverting to an untagged or short digest would still pass every
# in-package test that compares one run against another. This is also the ONLY
# place the REAL BINARY's real stderr is inspected -- the Go-side format test
# renders the shared constants, which cannot see the print site being deleted.
#
# Extract with sed (not grep -o, which is not POSIX). The `q` quits at the first
# match instead of piping to `head -1`: under `set -o pipefail` a second matching
# line would kill head, hand sed a SIGPIPE, and exit the script 141 with NO
# message -- the one exit path that would not go through fail(). Measured: with
# 200k matching lines the `| head -1` form exits 141 silently.
#
# ⚠️ The q MUST hang off an ADDRESS BLOCK (`/re/{s///p;q;}`), never off the `s`
# command as `s/.../.../{p;q;}`. Measured on BSD sed (macOS): the latter is a
# hard parse error -- `bad flag in substitute command: '{'` -- because BSD reads
# `{` as a substitute FLAG. It happens to work on GNU sed, which is exactly the
# host divergence the GNU/BSD `date` notes above exist to prevent.
table_hash="$(sed -n '/^price table identity: table_hash=/{s/^price table identity: table_hash=\([^ ]*\).*/\1/p;q;}' "$TMP/stderr.log")"

# Two DISTINCT failures, reported distinctly. Both produce an empty capture, and
# a single message asserting "the hash is malformed" sends an operator hunting
# the wrong defect: the line may be gone entirely, or may have stopped starting
# at column 0 (e.g. routed through slog, which wraps it as `time=... msg="..."`
# and defeats the ^ anchor).
if [ -z "$table_hash" ]; then
  cat "$TMP/stderr.log" >&2
  fail "no 'price table identity:' line at the START of a stderr line (#713) — the stamp is missing, or it is no longer column-anchored (slog-wrapped?). tierd score prints it right after the price-table version stamp"
fi
# Spell the hex class out rather than writing [0-9a-f]: POSIX leaves a range
# expression's meaning UNSPECIFIED outside the C locale, and glibc orders ranges
# by COLLATION -- in en_US.UTF-8 the sequence interleaves case (a A b B ...), so
# [a-f] can admit A-E and an uppercase digest (a %X-for-%x regression) would pass
# on Linux while failing on BSD. This script does not pin LC_ALL, so it inherits
# the caller's locale. Same host-divergence class as the GNU/BSD `date` notes above.
if ! printf '%s' "$table_hash" | grep -Eq '^tierpt1:[0123456789abcdef]{64}$'; then
  cat "$TMP/stderr.log" >&2
  fail "price-table table_hash is '${table_hash}', want ^tierpt1:[0-9a-f]{64}\$ in LOWERCASE hex (#713 — the scheme tag is what lets a future canonicalization change be told apart from tampering)"
fi
# Distinct prefix from the producer's own line: if a caller ever merges this
# script's stdout and stderr into one log and re-runs the extraction above, a
# shared prefix would let the wrong line be picked up.
echo "OK: price-table table_hash shape verified (${table_hash})"

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
