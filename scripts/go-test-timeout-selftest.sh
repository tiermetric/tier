#!/usr/bin/env bash
# Usage: make -n check-full | bash scripts/go-test-timeout-selftest.sh 30m
set -euo pipefail

awk -v timeout="${1:-30m}" '
  /^[[:space:]]*#/ { next }
  /(^|[[:space:]])go test[[:space:]]/ {
    tests++
    if (index($0, "-timeout " timeout " ") == 0) {
      print "FAIL TestGoTestTimeoutMakeDryRun: missing -timeout " timeout ": " $0
      failed = 1
    }
    if ($0 !~ /cd tools\/docgen/ && $0 !~ /-race -count=1 /) {
      print "FAIL TestGoTestTimeoutMakeDryRun: missing race/count flags: " $0
      failed = 1
    }
  }
  END {
    if (tests < 1) {
      print "FAIL TestGoTestTimeoutMakeDryRun: expected at least 1 go test command, got " (tests + 0)
      failed = 1
    }
    if (failed) exit 1
    print "PASS TestGoTestTimeoutMakeDryRun: all " tests " commands use -timeout " timeout
  }
'

failed=0
e2e_rc=0
e2e=$(grep -Ev '^[[:space:]]*#' scripts/e2e-summary.sh) || e2e_rc=$?
e2e_tests=$(printf '%s\n' "$e2e" | grep -Ec '(^|&&)[[:space:]]*go test[[:space:]]' || true)
if [ "$e2e_rc" -gt 1 ]; then
  echo 'FAIL TestGoTestTimeoutE2ESummary: cannot read scripts/e2e-summary.sh'
  failed=1
elif [ "$e2e_tests" -eq 0 ] && [[ "$e2e" == *'go test'* ]]; then
  echo 'FAIL TestGoTestTimeoutE2ESummary: go test text present but zero anchored command matches'
  failed=1
elif [ "$e2e_tests" -eq 0 ] && [ "${GO_TEST_TIMEOUT_ALLOW_EMPTY_E2E_FIXTURE:-0}" != 1 ]; then
  echo 'FAIL TestGoTestTimeoutE2ESummary: expected at least 1 anchored go test command; only an explicit fixture may have zero'
  failed=1
elif printf '%s\n' "$e2e" | grep -E '(^|&&)[[:space:]]*go test[[:space:]]' | grep -Ev -- '-timeout[[:space:]]'; then
  echo 'FAIL TestGoTestTimeoutE2ESummary: go test command lacks -timeout'
  failed=1
else
  echo "PASS TestGoTestTimeoutE2ESummary: $e2e_tests anchored go test commands use -timeout"
fi

# '0s' is rejected because go test -timeout 0 disables the timeout entirely.
for bad in '30m -run XXX' '0s' '00m'; do
  status=0
  out=$(MAKEFLAGS= GO_TEST_TIMEOUT="$bad" make -n test EXPECT_HEAD= 2>&1) || status=$?
  if [ "$status" -eq 2 ] && [[ "$out" == *'GO_TEST_TIMEOUT must match ^[1-9][0-9]*(s|m|h)$'* ]]; then
    echo "PASS TestGoTestTimeoutInvalidValue: '$bad' rejected with exit 2"
  else
    echo "FAIL TestGoTestTimeoutInvalidValue: '$bad' expected parse-time rejection, got exit $status"
    failed=1
  fi
done
exit "$failed"
