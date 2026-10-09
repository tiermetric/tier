#!/usr/bin/env bash
# expect-head.sh — assert the working tree is on the commit the caller BELIEVES
# it is on, before a gate spends five minutes proving something true of the
# wrong tree (#615).
#
# The failure this exists for is shaped exactly like success. On 2026-08-04 a
# `git switch` printed "Aborting" (modified files blocked the checkout), HEAD
# stayed on an ALREADY-MERGED commit, and `make check-full` then returned rc=0 —
# honestly, for that commit. Every visible signal said the branch had passed its
# gate. The only tell was the SHA, and nothing in the tree read it. Same class as
# seam-exercise skipping in a worktree: a gate reporting rc=0 without having
# tested what the caller believes it tested. That one at least prints a WARN;
# this one printed nothing at all.
#
# Usage:
#   EXPECT_HEAD=<rev> scripts/expect-head.sh     # assert HEAD == <rev>
#   scripts/expect-head.sh                       # EXPECT_HEAD unset: silent no-op
#
# Wired as the FIRST prerequisite of `make check` (and therefore of
# `make check-full`), so the assertion resolves before lint/build/test start.
# Failing after the suite has already run is nearly useless.
#
# Contract:
#   EXPECT_HEAD unset in the environment  -> exit 0, print nothing. This is the
#       ordinary local run, and it must stay frictionless: a gate that nags at
#       every invocation is a gate that gets switched off.
#   EXPECT_HEAD set and matching HEAD     -> exit 0, one confirming line.
#   EXPECT_HEAD set and NOT matching      -> exit 1, printing actual vs expected.
#   EXPECT_HEAD set but EMPTY             -> exit 1. NOT treated as "unset": the
#       realistic way to get here is `make check-full EXPECT_HEAD=$SHA` with $SHA
#       unset in the caller's shell, which is precisely the silent-no-assertion
#       shape this script exists to abolish. Make delivers command-line and
#       environment variables to a recipe's environment verbatim, preserving the
#       distinction, so "unset" reaches here as unset.
#   EXPECT_HEAD anchored on HEAD (HEAD, @, HEAD~1, @{0}, …) -> exit 1. Same
#       "assertion that cannot fail" category as the empty case, differently
#       spelled. See the case block below.
#
# 🔴 THE REV MUST COME FROM OUTSIDE THE TREE BEING GATED. This is the operational
# half of the guard, and no amount of code can enforce it. The denylist below
# catches the spellings that are tautological *syntactically*, but
#
#	make check-full EXPECT_HEAD=$(git rev-parse HEAD)
#
# is exactly as vacuous and is indistinguishable from a real assertion once the
# value arrives — it re-reads the very tree it is supposed to be checking. Take
# the SHA from the PR, from `gh pr view`, from the release notes, from whoever
# told you which commit to gate. If the SHA came out of the tree you are standing
# in, you have written `x == x` in a more expensive way.
#
# 🔴 A MISSING PRECONDITION IS A FAILURE HERE, NOT A SKIP.
# scripts/seam-exercise.sh chose WARN-and-skip when it cannot run (no .git
# directory), and that is right for it: its precondition is a property of WHERE
# you run it, it is an ADDITIONAL seal on an unrelated PR's mandatory gate, and
# hard-failing there would get the pre-push gate disabled. None of that transfers.
# This check runs ONLY when a caller explicitly opted in by naming the commit
# they expect, so there is no unrelated PR to protect — and a skip would return
# rc=0 without having verified the tree, reproducing the exact false green the
# issue is about. If git is absent, or this is not a repository, or EXPECT_HEAD
# names a commit this repository does not contain, we cannot confirm the caller's
# belief, so we refuse.
set -euo pipefail

# 🔴 `git -C <dir>` changes the working DIRECTORY; it does NOT override GIT_DIR /
# GIT_WORK_TREE from the environment. Left set, every git call below would read
# whatever repository those name, and this script would print its confirming OK
# line about a tree that is not the one it is standing in — rc=0, success-shaped,
# about the wrong repo. That is #615 committed by the #615 guard itself.
#
# Not theoretical: git exports GIT_DIR (and usually GIT_WORK_TREE and
# GIT_INDEX_FILE) into every hook process, and a pre-push hook running
# `make check EXPECT_HEAD=$(git rev-parse HEAD)` is the most obvious consumer
# this guard has. `git bisect run` and `git rebase --exec` are the same class.
# Measured, then re-measured with this line in place.
#
# GIT_CEILING_DIRECTORIES is deliberately NOT cleared: it only bounds git's
# upward search, so it cannot redirect us at another repository, and the test
# suite uses it to keep the "not a repository" arm honest inside TMPDIR.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR

# REPO is derived from where this SCRIPT lives, never from $PWD and never from
# `git rev-parse --show-toplevel`. Both alternatives let the guard be aimed at a
# tree nobody named: run it from inside another checkout and it would confirm
# THAT repository's HEAD and exit 0 — #615 with the wrongness moved up a level.
# The only tree `make check` is about to test is the one the Makefile and this
# script live in. Pinned by TestExpectHeadGuard_JudgesItsOwnTreeNotTheCwd.
#
# Parameter expansion rather than `dirname`, so this works with no PATH at all
# (the git-missing arm strips PATH; `$(dirname …)` would fail there and silently
# leave REPO=/ — harmless today only because the git refusal fires first).
# if/then, NOT `[ … ] && script_dir=.` — under `set -e` a bare `A && B` whose
# test is false makes the whole compound return 1 and kills the script.
script_dir="${0%/*}"
if [ "$script_dir" = "$0" ]; then # invoked as a bare name, no slash in $0
  script_dir="."
fi
REPO="$(cd "$script_dir/.." && pwd)"

fail() { echo "FAIL: expect-head: $*" >&2; exit 1; }

# ${VAR+set} distinguishes "unset" from "set to the empty string" — and is safe
# under `set -u`, unlike a bare ${VAR}. That distinction is load-bearing: see the
# EXPECT_HEAD-empty clause in the contract above.
if [ -z "${EXPECT_HEAD+set}" ]; then
  exit 0
fi

want_rev="$EXPECT_HEAD"

if [ -z "$want_rev" ]; then
  fail "EXPECT_HEAD is set but EMPTY — refusing to run an assertion that cannot fail.
      This is what \`make check-full EXPECT_HEAD=\$SHA\` looks like when \$SHA is
      unset in the calling shell. Pass a commit, or do not set EXPECT_HEAD at all."
fi

# The same "assertion that cannot fail" category as the empty case above, reached
# by a different route: a rev ANCHORED ON HEAD is read out of the very tree under
# test, so the comparison below is `x != x` (always true) or `x != x~1` (always
# false). Either way the operator learns nothing about the tree.
#
# Deliberately a PREFIX match on HEAD/@ rather than an enumerated list. An
# enumeration cannot be complete — HEAD, @, HEAD^0, HEAD~0, HEAD^{}, HEAD^{commit},
# @{0}, HEAD@{0}, @^0 all resolve to HEAD, and every one of those was measured
# passing an earlier enumerated version of this check. The prefix form also
# refuses HEAD~1 and @{u}, which ARE meaningful expressions; that over-rejection
# is intentional and costs nothing, because it is a LOUD refusal naming the fix,
# not a silent pass. Pass `origin/main` instead of `@{u}`.
#
# Branch and tag names are NOT in this class and must keep working:
# `EXPECT_HEAD=main` is a real assertion (HEAD sits at that branch's tip) and is
# precisely what would have caught the aborted `git switch` behind #615.
case "$want_rev" in
  HEAD* | @*)
    fail "EXPECT_HEAD='$want_rev' is anchored on HEAD, so this assertion cannot fail:
      it asks whether HEAD is HEAD. Name the commit you BELIEVE you are on, taken
      from OUTSIDE this tree (the PR, the release notes, whoever told you which
      commit to gate). A branch or tag name is fine — 'origin/main', 'v1.2.0'."
    ;;
esac

command -v git > /dev/null 2>&1 ||
  fail "git is not on PATH, so HEAD cannot be read — but EXPECT_HEAD=$want_rev asked for it to be verified.
      Refusing to exit 0 on an unverified tree (see the skip-vs-fail note in this script's header)."

git -C "$REPO" rev-parse --git-dir > /dev/null 2>&1 ||
  fail "$REPO is not a git repository (or git refused to read it), so HEAD cannot be read —
      but EXPECT_HEAD=$want_rev asked for it to be verified.
      Refusing to exit 0 on an unverified tree (see the skip-vs-fail note in this script's header)."

# Both sides resolved through rev-parse, then compared as canonical 40-char
# object names. NOT a string-prefix match: prefix matching would call
# "bf880a4" and its own 40-char form unequal in one direction and would happily
# accept a 4-character stub in the other. ^{commit} additionally rejects a rev
# that resolves to a tree/blob/annotated-tag-of-a-tree.
actual="$(git -C "$REPO" rev-parse --verify --quiet 'HEAD^{commit}')" ||
  fail "cannot resolve HEAD in $REPO (unborn branch, or a corrupt/empty repository)."

# --verify --quiet exits non-zero, silently, for: an unknown object, a short SHA
# that is AMBIGUOUS across two objects, and a malformed rev. All three mean the
# caller's expectation cannot be confirmed against this tree, which is itself a
# wrong-tree signal worth failing on: a commit you expected to be sitting on and
# that this repository has never heard of is the loudest possible symptom.
#
# The ^{commit} peel is what makes an unknown FULL sha fail at all — measured:
# `git rev-parse --verify --quiet <unknown-40-hex>` exits 0 and echoes the input
# straight back, because a full-length hex string is a syntactically valid object
# name. Without the peel, any 40-hex typo would sail through to the comparison
# and be reported as a plain mismatch. Do not "simplify" it away.
want="$(git -C "$REPO" rev-parse --verify --quiet "${want_rev}^{commit}")" || {
  echo "FAIL: expect-head: EXPECT_HEAD='$want_rev' does not resolve to a commit in $REPO." >&2
  echo "      Either the tree is wrong (the commit was never fetched here), or the rev is a" >&2
  echo "      typo, or it carries stray whitespace/newlines from the shell that produced it," >&2
  echo "      or it is a short SHA that is ambiguous in this repository." >&2
  echo "      HEAD is currently $actual" >&2
  exit 1
}

if [ "$actual" != "$want" ]; then
  actual_branch="$(git -C "$REPO" rev-parse --abbrev-ref HEAD 2> /dev/null || echo '?')"
  actual_subject="$(git -C "$REPO" log -1 --format=%s "$actual" 2> /dev/null || echo '?')"
  want_subject="$(git -C "$REPO" log -1 --format=%s "$want" 2> /dev/null || echo '?')"
  {
    echo "FAIL: expect-head: HEAD is NOT the commit this run was supposed to gate."
    echo "      repo:     $REPO"
    echo "      expected: $want  (EXPECT_HEAD='$want_rev')"
    echo "                $want_subject"
    echo "      actual:   $actual  (on '$actual_branch')"
    echo "                $actual_subject"
    echo
    echo "      Nothing was tested. A gate run on the wrong commit passes honestly and"
    echo "      proves nothing about the commit you meant — that is #615. Check whether a"
    echo "      'git switch' printed 'Aborting' (modified files block a checkout and leave"
    echo "      HEAD exactly where it was), then re-run the gate."
  } >&2
  exit 1
fi

echo "expect-head: OK — HEAD is $actual (EXPECT_HEAD='$want_rev')"

# HEAD matching is necessary but not sufficient: a tree with modified TRACKED
# files is not the commit it claims to be, and modified files are what caused the
# 2026-08-04 incident in the first place (they are why the `git switch` aborted).
# A WARN rather than a failure — untracked files are permanently present in this
# checkout, and hard-failing on a dirty tree would make the guard unusable
# mid-edit, which is how a gate gets switched off. --untracked-files=no keeps it
# to changes that actually alter what is being tested.
dirty="$(git -C "$REPO" status --porcelain --untracked-files=no 2> /dev/null || true)"
if [ -n "$dirty" ]; then
  {
    echo "WARN: expect-head: HEAD matches, but the tree has UNCOMMITTED changes to tracked files."
    echo "      What this gate is about to test is $actual PLUS those edits, not $actual."
    echo "$dirty" | while IFS= read -r entry; do echo "        $entry"; done
  } >&2
fi
