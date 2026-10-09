BINARY  := tierd
MODULE  := github.com/tiermetric/tier
GOFLAGS := -trimpath
# The default 10m is about 75% of internal/api's measured -race integration time on the gate hosts.
GO_TEST_TIMEOUT ?= 30m
# Validate before expanding the timeout into any recipe, including under -j.
$(if $(shell printf '%s\n' '$(subst ','"'"',$(GO_TEST_TIMEOUT))' | LC_ALL=C awk 'END { if (NR == 1 && $$0 ~ /^[1-9][0-9]*(s|m|h)$$/) print "valid" }'),,$(error GO_TEST_TIMEOUT must match ^[1-9][0-9]*(s|m|h)$$))
# VERSION is reported by /api/v1/livez (#49). git description, "dev" off-tree.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# COMMIT is the exact revision, which VERSION cannot supply: a tagged build
# reports the tag however it was built, so two binaries from a moved tag are
# indistinguishable by version alone (#638).
COMMIT  := $(shell git rev-parse HEAD 2>/dev/null || echo "")
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build lint test check check-full clean docker seam-exercise docs-html docs-html-check docs-test cve-rescan cve-rescan-selftest e2e-summary e2e-summary-selftest go-test-timeout-selftest expect-head vet-386

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/tierd

# Hermetic wire_check for the JSONL ingestion contract (captured-sample class).
# Replays a scrubbed real
# current-format Claude Code session through `tierd score` and asserts the
# ingestion contract (TOTAL != $0, empty_cwd == 0, sample not stale). No network
# or services — drives a CI step or the Phase-2 e2e gate. See testdata/seam-jsonl-ingestion/.
seam-exercise:
	@./scripts/seam-exercise.sh

lint:
	go vet ./...
	go vet -tags integration ./...
	# golangci-lint uses the if/else form (NOT `A && B || C`) so a real finding
	# fails the recipe (#307). With `&& golangci-lint run ... || echo`, a non-zero
	# exit from a real staticcheck/errcheck finding is caught by the `||`, printing
	# the misleading "not installed, skipping" AND exiting 0, so findings never
	# failed the gate. The `command -v` guard distinguishes "genuinely not
	# installed" (soft skip) from "installed and found issues" (hard fail).
	# Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest.
	#
	# GOLANGCI_LINT_CACHE is pinned per checkout (#626). golangci-lint's default
	# cache is ONE directory under $$HOME shared by every checkout on the machine,
	# and it is keyed by package content rather than by location. Agents work in
	# nested git worktrees under .claude/worktrees/, so the main tree and a
	# worktree routinely hold byte-identical packages: whichever lints first
	# stores the issues, and the second REPLAYS them carrying the FIRST runner's
	# absolute path. That is how a main-tree run reported a finding in
	# .claude/worktrees/impl-615/... . The replay alone is what fails the gate,
	# whether or not that worktree still exists; deleting it only adds the
	# "no such file or directory" warnings, because the cached path can no
	# longer be opened to render the source line. Note the exit code: on
	# findings golangci-lint exits 1, and `make` reports 2 for the failed
	# recipe -- the 2 in #626 is make's, not the linter's.
	#
	# Pointing the cache at $$(CURDIR) gives every checkout its own, so no tree
	# can hold an entry into another tree, and it lives under bin/ because that
	# is already gitignored, dockerignored, removed by `make clean` and allowed
	# by the export audit -- one location four gates already agree on.
	# This trades cache SHARING for cache CORRECTNESS: each worktree now pays a
	# cold lint instead of warm-hitting a sibling's entries. That is the right
	# trade, so do not "optimise" it back.
	#
	# Do NOT "fix" this in .golangci.yml instead: a path exclusion on .claude/
	# suppresses the replayed REPORT while the poisoned entry stays, which
	# measurably turns a real rc=1 finding in the main tree's own internal/
	# package into a green rc=0.
	# Must be absolute -- golangci-lint rejects a relative cache dir.
	@if command -v golangci-lint > /dev/null 2>&1; then GOLANGCI_LINT_CACHE="$(CURDIR)/bin/.golangci-lint-cache" golangci-lint run --build-tags integration ./...; else echo "golangci-lint not installed, skipping"; fi
	# govulncheck is wired here (#52 interim mitigation) so a future GO-ID against
	# any required module trips the build. It is the fail-loud backstop for the
	# archived-parser risk that motivated the yaml.v3 -> go.yaml.in/yaml/v3 swap:
	# an un-patchable CVE would now surface at lint time. The if/else (like the
	# golangci-lint line above, and unlike the `A && B || C` idiom #307 removed) is
	# deliberate: it keeps ONLY the missing-tool case soft while letting
	# govulncheck's non-zero exit on a real finding fail the recipe.
	# `&& govulncheck ./... || echo` would instead swallow findings: govulncheck
	# exits 3 on a vuln, the `||` branch runs, and the recipe would exit 0 while
	# printing a misleading "not installed".
	# Install: go install golang.org/x/vuln/cmd/govulncheck@latest.
	@if command -v govulncheck > /dev/null 2>&1; then govulncheck ./...; else echo "govulncheck not installed, skipping"; fi

test:
	go test -race -count=1 -timeout $(GO_TEST_TIMEOUT) ./...

# Regenerate the committed HTML docs (internal/docs/html/) from the markdown in
# docs/. The generator is a SEPARATE nested module (tools/docgen) so goldmark +
# bluemonday never enter the release binary's dependency graph — hence the `cd`
# into it. See tools/docgen/main.go.
docs-html:
	cd tools/docgen && go run . -in ../../docs -out ../../internal/docs/html

# No-drift gate: regenerate the docs and fail if the committed output differs from
# a fresh render. Wired into `check` so a markdown edit that was never re-rendered
# (or a hand-edit of the generated HTML) cannot merge. `git status --porcelain`
# (not `git diff`) is used deliberately: it catches untracked ADDS (a brand-new
# rendered page) and DELETES (a page pruned because its source was removed) too —
# both of which `git diff --exit-code` would miss, since diff ignores untracked
# files and a prune of a still-tracked file is caught but an add is not.
docs-html-check: docs-html
	@test -z "$$(git status --porcelain -- internal/docs/html)" || { echo 'docs HTML drift: regenerate + commit internal/docs/html'; git status --porcelain -- internal/docs/html; exit 1; }

# The docgen module is a SEPARATE nested module, so the root `go test ./...` below
# does NOT reach it. Run its tests explicitly here so the non-negotiable sanitizer
# (<script> stripping), link-integrity, anchor, orphan, and prune tests actually
# gate every `make check`.
docs-test:
	cd tools/docgen && go test -timeout $(GO_TEST_TIMEOUT) ./...

# Wrong-tree guard (#615). Opt-in: with EXPECT_HEAD unset this is a silent
# no-op, which is the whole design — an ordinary local `make check` must feel
# exactly as it did before, or the guard gets switched off. With EXPECT_HEAD set
# it hard-fails on a HEAD mismatch. See scripts/expect-head.sh for why a missing
# precondition fails here rather than skipping the way seam-exercise does.
#
# 🔴 THE RECIPE IS A BARE INVOCATION AND MUST STAY ONE. Do not "helpfully" pass
# the value along, i.e. never:
#
#	@... EXPECT_HEAD='$(EXPECT_HEAD)' $(CURDIR)/scripts/expect-head.sh
#
# That splices the value into a shell command line. Measured with
# `EXPECT_HEAD="a'; touch /tmp/CANARY; echo '"`: the canary file was created (the
# injected text ran), the guard script was NEVER INVOKED, and make exited 0. A
# malformed value that both executes arbitrary text and silently disarms the
# wrong-tree guard while reporting success is the exact bug class this guard
# exists to abolish. Other values fail differently — a lone quote is a shell
# syntax error (rc=2) — so do not rely on the failure mode; rely on not splicing.
# Pinned by TestExpectHeadGuard_MakeTargetInvokesTheScript, whose probe is the
# canary FILE, not a string in the output (the guard's diagnostic quotes the
# offending value back, so a string match would prove nothing).
#
# No `export` directive is needed or wanted: GNU Make already places variables
# that came from the command line or the environment into every recipe's
# environment, byte-for-byte and with no shell interpretation, preserving the
# unset-vs-set-but-empty distinction the script relies on. Measured on GNU Make
# 3.81, which is what macOS ships and what `make` resolves to here. An explicit
# `export EXPECT_HEAD` would be a no-op no test could ever kill.
expect-head:
	@"$(CURDIR)/scripts/expect-head.sh"

# 🔴 EVERY GATE STEP DEPENDS ON THE GUARD — AS AN EDGE, NOT AS LIST ORDER.
#
# Listing `expect-head` first in `check`'s prerequisites (below) is honoured
# left-to-right only when make runs SERIALLY. Under `make -j8` the guard and the
# whole suite start together — measured on a structural replica: `go vet` was
# echoed and the siblings ran to completion alongside the failing guard. The
# guard still exits non-zero, so nothing looks broken, but its entire value
# proposition ("a mismatch costs one git rev-parse, not five minutes") is gone
# and a concurrent lane can print a verdict beside the failure.
#
# These are real dependency edges, honoured under `-j` too, so the guard resolves
# first under any invocation. Chosen over a global `.NOTPARALLEL:` — which an
# earlier revision of this change used, and which also works — because it keeps
# `-j` available downstream (`scripts/` and `internal/` ship wholesale to the
# public mirror), because the edges are visible to the wiring test in
# internal/gates rather than being an untestable global switch, and because it
# extends the guard to a bare `make test` / `make build`, which the prerequisite
# list alone never reached. `.WAIT` would be the surgical form, but it needs GNU
# Make 4.4 and macOS ships 3.81 — which is what `make` resolves to here.
#
# This block must stay BELOW `build:`: make takes its default goal from the first
# target of the first rule in the file, so hoisting these names above `build`
# silently turns a bare `make` into `make lint`.
lint build test docs-html docs-test docs-html-check cve-rescan-selftest \
e2e-summary-selftest go-test-timeout-selftest: expect-head

# Optional fragment, present only in the maintainers' own checkout: it sets
# PRIVATE_CHECKS (selftests of tooling that is not published) and defines their
# targets. Without it PRIVATE_CHECKS is empty and `check` runs only what this file
# defines. It must stay ABOVE `check:`, whose prerequisites are expanded when the
# rule is read, and below `build:`, for the default-goal reason given above.
-include .private-tree

# expect-head is FIRST here too — belt and braces with the edges above, and it is
# what makes the serial ordering readable at a glance. `check-full: check` below
# means check-full inherits it. Do not move it down the list.
check: expect-head lint vet-386 build test docs-test docs-html-check cve-rescan-selftest e2e-summary-selftest go-test-timeout-selftest $(PRIVATE_CHECKS)

vet-386: expect-head ; @"$(CURDIR)/scripts/vet-386.sh"

check-full: check
	go test -race -count=1 -timeout $(GO_TEST_TIMEOUT) -tags integration ./...
	# e2e-summary (batch 9: #451/#452/#269/#459): the line above prints only
	# "ok" for a package where every credential/endpoint-gated live test
	# skipped — a green run that is indistinguishable from a VERIFIED one.
	# This makes the PASS/SKIP/FAIL split of those tests visible at the top
	# level, with every skip's specific reason, instead of requiring -v on
	# the whole suite to notice.
	$(MAKE) e2e-summary
	# seam-exercise runs LAST — after the integration tests. Ordering rationale:
	# a compile/test failure should surface as itself, not as a seam-exercise
	# build failure; the exercise is the final "the real current-format Claude
	# Code JSONL still ingests non-zero" seal (the #96 drift guard). The fast gate
	# `make check` deliberately does NOT run it: the exercise builds a binary and
	# shells out, and the fast gate stays pure lint+build+test.
	# SEAM_STALE_POLICY=warn: an aged-out captured sample is a wall-clock
	# condition, not a code regression, so it must NEVER hard-break this mandatory
	# pre-push gate for an unrelated PR. Here staleness warns; the ingestion
	# contract (TOTAL non-zero, empty_cwd zero) stays a hard failure. Standalone
	# `make seam-exercise` keeps staleness fail-loud (default).
	SEAM_STALE_POLICY=warn $(MAKE) seam-exercise

clean:
	rm -rf bin/

# Build the container image, stamping the same VERSION as `make build` so the
# image's `tierd version` / livez report matches the source tree (#66).
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) -t $(BINARY):latest .

# Print the PASS/SKIP/FAIL summary of the credential/endpoint-gated live E2E
# tests (batch 9: #451/#452/#269/#459) — see scripts/e2e-summary.sh's header
# for why this exists as its own step rather than trusting `go test`'s plain
# "ok" output. Standalone-runnable (`make e2e-summary`) as well as wired into
# check-full above.
e2e-summary:
	GO_TEST_TIMEOUT=$(GO_TEST_TIMEOUT) "$(CURDIR)/scripts/e2e-summary.sh"

# The control arm for the target above, and the reason it is in `check` rather
# than `check-full`: the two properties that make e2e-summary worth having over
# a bare `go test` -- the zero-match hard-fail and the below-floor guard -- are
# exercised by NOTHING else in the tree. `check-full` runs the REAL path, where
# both guards are (correctly) silent, so a run there proves only that they did
# not fire. Only the selftest proves they still CAN. It drives a stub `go` on
# PATH, so it is offline and derivable from the tree: a local gate like any
# other. Same wiring as cve-rescan-selftest below.
e2e-summary-selftest:
	"$(CURDIR)/scripts/e2e-summary.sh" --selftest

go-test-timeout-selftest:
	@make -n check-full EXPECT_HEAD= | bash "$(CURDIR)/scripts/go-test-timeout-selftest.sh" $(GO_TEST_TIMEOUT)
	@status=0; printf '%s %s\n' go 'test -race -count=1 ./...' | bash "$(CURDIR)/scripts/go-test-timeout-selftest.sh" $(GO_TEST_TIMEOUT) || status=$$?; \
	test "$$status" -eq 1 && echo 'PASS TestGoTestTimeoutMissingTimeout: rejected with exit 1'

# Re-scan PUBLISHED images for CVEs disclosed since they were built (#560).
#
# Deliberately NOT part of `make check` / `check-full`. Those gates are a
# deterministic function of the working tree, so a human can run them and get
# truth. This is a function of TIME and an external vulnerability database: the
# same bytes go from clean to critical with no commit. Wiring it into the
# pre-push gate would make an unrelated PR fail because someone else's CVE
# landed, which is how a gate gets disabled.
#
# It pulls images and needs network. Exit: 0 clean / 1 findings / 2 could-not-run.
# ⚠️ THIS TARGET IS LOSSY AND IS FOR HUMANS ONLY (#682). The script returns
# 0 CLEAN / 1 FINDINGS / 2 COULD-NOT-RUN, but GNU make REPLACES the recipe's
# code -- 2 for a failed recipe whatever it returned (measured: `@exit 7` -> 2),
# or 0 where errors are ignored (-i, .IGNORE, a `-`-prefixed recipe line). It
# never passes the recipe's own code through in either direction, so
# `make cve-rescan` reports a FINDING as a COULD-NOT-RUN. That is
# fine at a terminal, where you read the verdict line. It is NOT fine in CI,
# where the exit code is the whole signal -- image-cve-rescan.yml therefore
# calls the script directly, and the script's --selftest fails if it stops.
# Anything scripting on the exit code must call scripts/image-cve-rescan.sh.
cve-rescan:
	"$(CURDIR)/scripts/image-cve-rescan.sh"

# Prove the scanner harness works without touching the network. Run this before
# trusting a green from the target above.
cve-rescan-selftest:
	"$(CURDIR)/scripts/image-cve-rescan.sh" --selftest
