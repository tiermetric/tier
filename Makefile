BINARY  := tierd
MODULE  := github.com/tiermetric/tier
GOFLAGS := -trimpath
# VERSION is reported by /api/v1/livez (#49). git description, "dev" off-tree.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build lint test check check-full clean docker seam-exercise docs-html docs-html-check docs-test cve-rescan cve-rescan-selftest dns-latch-selftest mirror-audit mirror-audit-selftest

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
	@if command -v golangci-lint > /dev/null 2>&1; then golangci-lint run --build-tags integration ./...; else echo "golangci-lint not installed, skipping"; fi
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
	go test -race -count=1 ./...

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
	cd tools/docgen && go test ./...

check: lint build test docs-test docs-html-check cve-rescan-selftest dns-latch-selftest mirror-audit-selftest

check-full: check
	go test -race -count=1 -tags integration ./...
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
# Audit the PUBLISHED mirror over its FULL HISTORY (#586).
#
# publish-audit.sh audits the tree we are ABOUT TO push; this audits what IS
# public. The gap between them is how a leak survives: a 6.6MB compiled artifact
# shipped in v0.2.1 and sat public for three days across two releases while every
# export audit passed, because it was not in the export -- it was in the mirror.
# It also walks HISTORY, not just the tip: that artifact was deleted from the tip
# and stayed in history, which a tip-only scan calls clean.
#
# Network-touching, and its verdict is a fact about the world rather than about
# the working tree, so it is NOT in `make check` -- same reasoning as cve-rescan.
mirror-audit:
	$(CURDIR)/scripts/mirror-audit.sh

# Prove each arm fires on a violation planted in HISTORY with a CLEAN TIP -- the
# exact shape the real leak had. Offline, so it belongs in `make check`.
mirror-audit-selftest:
	$(CURDIR)/scripts/mirror-audit.sh --selftest

cve-rescan:
	$(CURDIR)/scripts/image-cve-rescan.sh

# Prove the scanner harness works without touching the network. Run this before
# trusting a green from the target above.
cve-rescan-selftest:
	$(CURDIR)/scripts/image-cve-rescan.sh --selftest

# The DNS readiness latch (#575) is INTERNAL and excluded from the public export
# (#580), so there is deliberately NO `make dns-latch` runner target: it would
# ship as a public target that can only ever fail, and its failure message would
# name an internal file. The latch is invoked by its scheduler at an absolute
# path (deploy/dns-latch.crontab), which needs no make target at all.
#
# Prove the latch's state machine offline. This IS in `make check`: the logic
# deciding "speak or stay silent" and "retire or keep waiting" is derivable from
# the tree, so it is a local gate like any other.
#
# 🔴 RUN TWICE, UNDER TWO INTERPRETERS, AND THE SECOND ONE IS THE POINT.
# `make` inherits an interactive PATH and finds Homebrew bash 5.x; macOS cron's
# PATH is /usr/bin:/bin, where `#!/usr/bin/env bash` resolves to Apple's bash
# 3.2.57. A previous revision used bash-4.3-only syntax: the gate was green
# while every scheduled run died with "bad array subscript". A gate that does
# not exercise the deployment's interpreter proves nothing about the deployment.
# The script is INTERNAL and is excluded from the public export (#580), so this
# target must not break `make check` in a published tree.
#
# 🔴 But a bare `[ -f script ]` skip cannot tell "public export, correctly skip"
# from "PRIVATE checkout, file missing, must NOT skip" -- in the private repo it
# would silently self-disable for a script that runs from cron, and `make check`
# would stay green. That is the dominant bug class, not a defence against it.
# `scripts/publish-audit.sh` is the discriminator: it is private-only (rm -f'd
# by the export recipe AND on must_not), so its presence proves this is not the
# published tree, and a missing dns-latch.sh there is an ERROR, not a skip.
dns-latch-selftest:
	@if [ -f "$(CURDIR)/scripts/dns-latch.sh" ]; then \
		"$(CURDIR)/scripts/dns-latch.sh" --selftest && \
		/bin/bash "$(CURDIR)/scripts/dns-latch.sh" --selftest; \
	elif [ -f "$(CURDIR)/scripts/publish-audit.sh" ]; then \
		echo "dns-latch-selftest: scripts/dns-latch.sh is MISSING from a PRIVATE checkout" >&2; \
		echo "  (scripts/publish-audit.sh is present, so this is NOT the public export)." >&2; \
		exit 1; \
	else \
		echo "dns-latch-selftest: SKIPPED -- scripts/dns-latch.sh absent (internal tool, not in the public export; see #580)"; \
	fi

