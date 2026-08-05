#!/usr/bin/env bash
# e2e-summary.sh — batch 9 (#451, #452, #269, #459): make the PASS/SKIP/FAIL
# count of the credential/endpoint-gated LIVE E2E tests visible at the TOP
# LEVEL of `make check-full`.
#
# WHY THIS EXISTS: `go test ./...` without -v prints only "ok" for a package
# where every test skipped — a green run is then indistinguishable from a
# VERIFIED one, which is exactly the false-green shape batch 9's ruling warns
# against ("the skip is the dangerous part"). This script runs the
# credential/endpoint-gated tests (named `TestLive_*` by convention) with -v,
# counts each outcome from go test's own `--- PASS|FAIL|SKIP:` lines, and
# prints an explicit summary + every skip reason so a reader never has to
# infer coverage from silence.
#
# This does NOT re-decide pass/fail — go test's own exit code is authoritative
# and is propagated unchanged. It adds three extra safety properties go test
# itself does not offer:
#   1. If the test-name pattern below ever stops matching ANY test (a rename, a
#      moved file, a build-tag typo), that is indistinguishable from
#      "everything skipped" unless something checks for it — so a zero-match
#      run is a hard FAIL here, not a quiet no-op.
#   1b. A zero check alone is not enough: PARTIAL drift is the likelier
#      accident. Renaming ONE live test off the convention drops the match
#      count without emptying it, and the summary still prints green. That
#      rename is one keystroke away — the sibling control arms in the same
#      files are already named TestLiveGeminiCreds_SkipGuard (no underscore
#      after "Live"), so the two conventions differ by a single character.
#      EXPECTED_MIN_LIVE_TESTS below is therefore a COMMITTED FLOOR, and a
#      count beneath it fails.
#   2. `-count=1` disables go's test cache. Without it, a run against a
#      now-DEAD endpoint can replay a STALE PASS from when it was last up —
#      go's cache tracks os.Getenv (so credential-gated tests invalidate
#      correctly) but NOT live network reachability (so an endpoint-gated
#      test like the Ollama one would not). A cached "PASS: ran for real"
#      is a worse false green than the plain `ok` this script exists to
#      replace, so this is non-negotiable.
#
# NOTE on double execution: `make check-full`'s main `go test -tags
# integration ./...` sweep already runs every TestLive_* test once (quietly).
# This script runs them a SECOND time, verbosely, purely for the summary.
# Accepted deliberately rather than restructured: the Ollama arm is free and
# fast, and the credential-gated arms (Anthropic Admin, Gemini) are single
# cheap API calls even when a real key is present — a human who set that env
# var for a local run is choosing this cost. Restructuring to run each
# TestLive_* exactly once would mean carving internal/integration's package
# tests into two `go test` invocations, which risks silently dropping
# coverage of the package's many NON-Live tests from the main sweep.
#
# Usage: e2e-summary.sh [--selftest]
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

# EXPECTED_MIN_LIVE_TESTS is the committed floor: how many TestLive_* tests the
# tree is known to contain. Verified at the time of writing with
#
#   grep -rc '^func TestLive_' internal/integration/*.go
#
# which found 4: TestLive_AnthropicAdminProbe_RealAPI,
# TestLive_AnthropicAdminPoller_RealAPI, TestLive_GeminiProxy_RealCompletion,
# TestLive_OllamaOpenAICompatProxy_RealCompletion.
#
# RAISE IT when you ADD a live test — that is the point of a floor, and the
# ratchet is what makes it worth having. Only LOWER it when a live test is
# deliberately deleted, in the same commit as the deletion, so the reason lands
# in the log rather than in a silently-shrinking count.
EXPECTED_MIN_LIVE_TESTS=4

# run_summary is the real behavior: run go test for the given pattern/package,
# print the PASS/SKIP/FAIL summary + skip reasons, and exit with go test's own
# code (or 1 on the zero-match / below-floor guards). Factored out so --selftest
# can drive it repeatably against a stub `go` on PATH instead of the real
# toolchain. `min` is a parameter rather than a read of the global above purely
# so the selftest can drive the floor guard from both sides.
run_summary() {
  local pattern="$1" pkg="$2" min="$3"
  local out rc
  out="$(mktemp)"
  # shellcheck disable=SC2064 # intentional early expansion of $out
  trap "rm -f '$out'" RETURN

  rc=0
  ( cd "$REPO" && go test -tags integration -count=1 -v "$pkg" -run "$pattern" ) >"$out" 2>&1 || rc=$?

  # Anchored to line START (^), matching the awk below exactly. An earlier,
  # unanchored version of these also matched indented SUBTEST result lines
  # (e.g. "    --- SKIP: TestX/case_a") and any t.Log line that happened to
  # contain the literal substring "--- PASS:" — inflating the count and, for
  # SKIP, silently detaching it from the reason-extracting awk (which was
  # always anchored), so a skipped subtest would be counted with NO reason
  # ever printed. No TestLive_* test currently has subtests, so this was
  # latent, not yet triggered — anchor both so it can't regress silently.
  local pass skip fail total
  pass=$(grep -c -- '^--- PASS:' "$out" || true)
  skip=$(grep -c -- '^--- SKIP:' "$out" || true)
  fail=$(grep -c -- '^--- FAIL:' "$out" || true)
  total=$((pass + skip + fail))

  echo "=== E2E live-test summary (batch 9: #451 #452 #269 #459) ==="
  echo "  pattern:  ${pattern} in ${pkg}"
  echo "  matched:  ${total} test(s)  (committed floor: ${min})"
  echo "  PASS:     ${pass}  (ran for real against a live endpoint/credential)"
  echo "  SKIP:     ${skip}  (endpoint/credential absent — see reasons below for how to enable)"
  echo "  FAIL:     ${fail}"
  if [ "$skip" -gt 0 ]; then
    echo
    echo "  --- skip reasons ---"
    # Accumulate every log line go test printed between "=== RUN <test>" and
    # its result line, and print that buffer only for tests that resolved
    # --- SKIP. (Not a fixed -B N grep: a skip message can legitimately span
    # more than one t.Log/t.Skip line, and a fixed lookback would silently
    # truncate it.)
    awk '
      /^=== RUN/   { buf = "" ; next }
      /^--- SKIP:/ { print; if (buf != "") printf "%s", buf; next }
      /^--- (PASS|FAIL):/ { next }
      { buf = buf "    " $0 "\n" }
    ' "$out"
  fi
  echo "=============================================================="

  if [ "$total" -eq 0 ]; then
    # Zero PASS/SKIP/FAIL lines is ALSO what a build failure looks like (no
    # test ever ran to emit one) — indistinguishable from "the pattern
    # matched nothing" by line-count alone. Disambiguate using go test's own
    # exit code: a genuine zero-match `-run` is documented to still exit 0
    # ("no tests to run" is not a go-test failure); any nonzero rc here means
    # something broke BEFORE tests could run (compile error, panic), which
    # deserves its own message and must still propagate go test's real rc,
    # not the generic "pattern drifted" 1.
    if [ "$rc" -eq 0 ]; then
      echo "e2e-summary.sh: FAIL — zero tests matched '${pattern}' in ${pkg}." >&2
      echo "  This must never silently report nothing: either the pattern drifted," >&2
      echo "  a live test file moved/was renamed, or the integration build tag broke." >&2
      cat "$out" >&2
      return 1
    fi
    echo "e2e-summary.sh: go test exited ${rc} before any test could run (e.g. a compile error or panic) — zero PASS/SKIP/FAIL lines to summarize. Full log follows:" >&2
    cat "$out" >&2
    return "$rc"
  fi

  # Below the committed floor: some live tests exist, but fewer than the tree
  # is known to have. The zero-match guard above cannot see this — it is the
  # PARTIAL-drift case (a rename off the TestLive_ convention, a file moved out
  # of the matched package, a build tag that excluded one file), and it prints
  # a perfectly green summary without this check.
  if [ "$total" -lt "$min" ]; then
    echo "e2e-summary.sh: FAIL — only ${total} test(s) matched '${pattern}' in ${pkg}, below the committed floor of ${min}." >&2
    echo "  Some live E2E tests stopped being counted. Likely causes: one was renamed off" >&2
    echo "  the TestLive_* convention (note the sibling guards are TestLiveXxx_, no" >&2
    echo "  underscore), moved out of ${pkg}, or lost its build tag." >&2
    echo "  If a live test was deliberately REMOVED, lower EXPECTED_MIN_LIVE_TESTS in" >&2
    echo "  this script in the same commit." >&2
    if [ "$rc" -eq 0 ]; then
      cat "$out" >&2
      return 1
    fi
    # rc is already nonzero — fall through so go test's own code propagates
    # unchanged rather than being flattened to 1.
  fi

  if [ "$rc" -ne 0 ]; then
    if [ "$fail" -eq 0 ]; then
      echo "e2e-summary.sh: go test exited ${rc} with ZERO '--- FAIL:' lines — a panic, timeout, or harness error, not a test assertion." >&2
    fi
    echo "e2e-summary.sh: go test exited ${rc}; the summary above did not include its full output — full log follows:" >&2
    cat "$out" >&2
  fi

  return "$rc"
}

# --- selftest: exercise run_summary against a STUB `go`, offline -----------
#
# Precedent: scripts/dns-latch.sh / scripts/mirror-audit.sh both ship a
# --selftest that make check runs unconditionally. This one is the control
# arm for the zero-match hard-fail (the whole reason this script exists over
# a bare `go test`) and for the skip-reason extraction — nothing else ever
# exercises either path, so without this they can rot into a no-op unnoticed.
selftest() {
  echo "e2e-summary --selftest  (bash ${BASH_VERSION})"
  echo
  local pass_n=0 fails=0
  local stub_dir
  stub_dir="$(mktemp -d)"
  trap 'rm -rf "$stub_dir"' RETURN

  # A stub `go` ahead of the real one on PATH, dispatching on $GO_STUB_MODE so
  # each case below drives run_summary against canned `go test -v` output
  # instead of a real toolchain / real tests.
  cat >"$stub_dir/go" <<'STUB'
#!/usr/bin/env bash
case "${GO_STUB_MODE:-}" in
mixed)
  cat <<'EOF'
=== RUN   TestLive_Foo
    foo_test.go:10: some setup log line
--- PASS: TestLive_Foo (0.01s)
=== RUN   TestLive_Bar
    bar_test.go:20: TIER_LIVE_BAR_KEY not set — skipping (fake reason,
    spanning two lines)
--- SKIP: TestLive_Bar (0.00s)
PASS
ok  	fake/pkg	0.02s
EOF
  exit 0
  ;;
allfail)
  cat <<'EOF'
=== RUN   TestLive_Broken
    broken_test.go:5: assertion failed: got 1 want 2
--- FAIL: TestLive_Broken (0.01s)
FAIL
FAIL	fake/pkg	0.01s
EOF
  exit 1
  ;;
panic)
  cat <<'EOF'
=== RUN   TestLive_Panics
panic: boom

goroutine 1 [running]:
FAIL	fake/pkg	0.01s [build failed]
EOF
  exit 2
  ;;
partialpanic)
  # One test resolved, THEN the binary died — so there ARE result lines to
  # count (unlike `panic`), and rc is 2 rather than 1. The combination the
  # below-floor guard must not flatten.
  cat <<'EOF'
=== RUN   TestLive_Ok
--- PASS: TestLive_Ok (0.01s)
=== RUN   TestLive_Panics
panic: boom

goroutine 1 [running]:
FAIL	fake/pkg	0.02s
EOF
  exit 2
  ;;
nomatch)
  cat <<'EOF'
testing: warning: no tests to run
PASS
ok  	fake/pkg	0.00s [no tests to run]
EOF
  exit 0
  ;;
*)
  echo "e2e-summary selftest: stub go invoked with unknown GO_STUB_MODE=${GO_STUB_MODE:-<unset>}" >&2
  exit 99
  ;;
esac
STUB
  chmod +x "$stub_dir/go"

  check() {
    local desc="$1" want_rc="$2" got_rc="$3"
    if [ "$got_rc" = "$want_rc" ]; then
      printf '  PASS  %-56s (rc=%s)\n' "$desc" "$got_rc"; pass_n=$((pass_n + 1))
    else
      printf '  FAIL  %-56s want rc=%s got rc=%s\n' "$desc" "$want_rc" "$got_rc"; fails=$((fails + 1))
    fi
  }
  check_contains() {
    local desc="$1" needle="$2" haystack="$3"
    case "$haystack" in
    *"$needle"*) printf '  PASS  %-56s (found)\n' "$desc"; pass_n=$((pass_n + 1)) ;;
    *) printf '  FAIL  %-56s missing %q\n' "$desc" "$needle"; fails=$((fails + 1)) ;;
    esac
  }

  local got out

  # Each case below prefixes the function call with GO_STUB_MODE=... PATH=...,
  # NOT a separate `VAR=val;` statement — the prefix form exports both for the
  # duration of that call, including to the `go` binary run_summary execs as a
  # real subprocess (a plain `VAR=val; cmd` statement would set a shell
  # variable but NOT export it, so the stub subprocess would never see it).

  # mixed PASS+SKIP: exit 0, and the multi-line skip reason must appear in full
  # (this is the RED-2 regression guard: an unanchored/misaligned counter would
  # either miscount or silently drop the reason block). min=2 matches the stub's
  # 2 tests exactly, so this is also the floor's ON-BOUNDARY control arm: it
  # proves the floor passes at total == min and is not simply always-failing.
  out="$(GO_STUB_MODE=mixed PATH="$stub_dir:$PATH" run_summary '^TestLive_' './fake' 2 2>&1)" && got=0 || got=$?
  check "mixed PASS+SKIP, total == floor -> exit 0" 0 "$got"
  check_contains "mixed: reports PASS: 1" "PASS:     1" "$out"
  check_contains "mixed: reports SKIP: 1" "SKIP:     1" "$out"
  check_contains "mixed: skip reason line 1 present" "TIER_LIVE_BAR_KEY not set" "$out"
  check_contains "mixed: skip reason line 2 (multi-line) present" "spanning two lines" "$out"

  # BELOW the floor: the partial-drift guard. Same stub output as above (2
  # tests, go test exits 0, zero failures, a summary that reads green) — the
  # ONLY difference is that the tree is supposed to have 3. Without the floor
  # check this run is indistinguishable from a healthy one.
  out="$(GO_STUB_MODE=mixed PATH="$stub_dir:$PATH" run_summary '^TestLive_' './fake' 3 2>&1)" && got=0 || got=$?
  check "total below the committed floor -> exit 1" 1 "$got"
  check_contains "below-floor: names the floor in the failure" "below the committed floor of 3" "$out"
  check_contains "below-floor: prints the count alongside the floor" "committed floor: 3" "$out"

  # zero-match: the whole point of this script. A pattern nothing matches must
  # hard-FAIL even though the stub `go` itself exits 0 ("no tests to run" is
  # not a failure to plain `go test`).
  out="$(GO_STUB_MODE=nomatch PATH="$stub_dir:$PATH" run_summary '^TestLive_Nonexistent' './fake' 1 2>&1)" && got=0 || got=$?
  check "zero-match pattern -> exit 1 (the core guard)" 1 "$got"
  check_contains "zero-match: names the pattern in the failure" "zero tests matched" "$out"
  # Zero is also below the floor, but it must keep its OWN diagnostic: "the
  # pattern matched nothing / the build broke" and "one test drifted off the
  # convention" send a reader to different places.
  check_contains "zero-match: keeps its own message, not the floor's" "pattern drifted" "$out"

  # a real assertion failure must propagate go test's rc unchanged.
  out="$(GO_STUB_MODE=allfail PATH="$stub_dir:$PATH" run_summary '^TestLive_' './fake' 1 2>&1)" && got=0 || got=$?
  check "assertion FAIL -> exit 1, propagated from go test" 1 "$got"
  check_contains "FAIL: reports FAIL: 1" "FAIL:     1" "$out"

  # a build failure / panic (rc=2, zero '--- FAIL:' lines) must NOT be
  # misreported as "0 failures" — the rc is what a caller (make) acts on, and
  # the diagnostic note must fire.
  out="$(GO_STUB_MODE=panic PATH="$stub_dir:$PATH" run_summary '^TestLive_' './fake' 1 2>&1)" && got=0 || got=$?
  check "panic/build failure -> exit 2, propagated" 2 "$got"
  check_contains "panic: distinguished from a zero-match (names 'before any test could run')" "before any test could run" "$out"

  # A nonzero go-test rc must NOT be flattened to 1 by the floor guard: the
  # caller acts on the rc, and a mid-run panic (2) is a different condition
  # from a drifted pattern (1). partialpanic is the only stub that produces
  # BOTH a countable result line and rc=2, which is what makes this reach the
  # floor guard's fall-through at all (the plain `panic` stub emits no result
  # lines, so it exits via the zero-match branch first).
  out="$(GO_STUB_MODE=partialpanic PATH="$stub_dir:$PATH" run_summary '^TestLive_' './fake' 9 2>&1)" && got=0 || got=$?
  check "below floor AND nonzero rc -> go test's rc wins (2, not 1)" 2 "$got"
  check_contains "below-floor: still names the floor even when rc is nonzero" "below the committed floor of 9" "$out"

  echo
  echo "  $pass_n passed, $fails failed"
  if [ "$fails" -eq 0 ]; then
    echo "  selftest green."
    return 0
  fi
  return 1
}

case "${1:-}" in
--selftest)
  selftest
  exit $?
  ;;
"")
  # Every live E2E test in the tree is named TestLive_* by convention
  # (TestLive_AnthropicAdminProbe_RealAPI,
  # TestLive_OllamaOpenAICompatProxy_RealCompletion, etc.) specifically so
  # this one pattern keeps catching new ones without per-issue script changes.
  run_summary '^TestLive_' './internal/integration/...' "$EXPECTED_MIN_LIVE_TESTS"
  exit $?
  ;;
*)
  echo "usage: e2e-summary.sh [--selftest]" >&2
  exit 1
  ;;
esac
