# syntax=docker/dockerfile:1
#
# tierd container image (#66). modernc.org/sqlite is pure Go, so tierd builds as
# a fully static binary (CGO_ENABLED=0) and runs on a Chainguard Wolfi static
# base — no libc, no shell, no package manager.

# --- build stage ---
# golang:1.26.6 satisfies go.mod's `go 1.26.6`. This build stage is what links the
# stdlib into the static binary, so a LOCAL govulncheck pass does NOT make the
# shipped artifact clean.
#
# 🔑 THE MECHANISM, because it is the whole lesson of #683: govulncheck measures
# the toolchain that RUNS it — never the one pinned here or in go.mod. On
# 2026-08-14 `brew upgrade go` put 1.26.6 on the build machine, so the local gate
# went green while go.mod and this FROM line both still said 1.26.5 and the
# already-published image stayed vulnerable. A green tree is not a clean artifact.
#
# 1.26.6 closes eight fixable HIGH stdlib CVEs in any FUTURE build (#694).
# ⚠️ 1.26.7 EXISTS and was deliberately not taken (measured 2026-08-22:
# `golang:1.26` -> go1.26.7). Reviewers are right that pinning the OLDEST version
# clearing today's database is what makes #694 repeatable — but the floor in
# go.mod must stay buildable on the machine that runs the gates, and m5pro is on
# go1.26.6 by an authorised `brew upgrade`; a 1.26.7 floor would fail `make lint`
# there under GOTOOLCHAIN=local. Take 1.26.7 in the same change that upgrades the
# build machine, not before.
# ⛔ It does NOT retroactively fix ghcr.io/tiermetric/tierd:latest — that image is
# immutable and keeps all eight until a rebuild AND a re-release on the mirror
# (#683, operator-gated). 1.26.5 was the earlier GO-2026-5856 fix (crypto/tls ECH
# privacy leak). Digest-pinned (mandatory — admission control rejects
# non-digest-pinned refs in staging/prod).
#
# THE TWO STEPS ARE DIFFERENT JOBS, and conflating them is how a wrong digest ships:
#   RESOLVE (once, to obtain a candidate):  crane digest golang:<tag>
#   VERIFY  (always, before committing it): docker run --rm golang@sha256:<d> go version
# ⚠️ Docker official-image tags are re-pushed when their base OS updates, so
# `crane digest golang:1.26.6` may stop matching the digest below at any time.
# (Measured 2026-08-22: `crane digest golang:1.26.5` already returns 705e964a…,
# NOT the 079e5980… this line pinned before today; both are genuine 1.26.5,
# verified by running them.) A match is definitive; only a MISMATCH is ambiguous,
# and for THIS pin — the builder — it is expected and harmless, because none of
# its base-OS layers reach the runtime image.
#
# 🔴 THAT REASONING IS SCOPED TO THIS LINE AND DOES NOT TRANSFER TO THE RUNTIME
# BASE PIN BELOW. That one IS the shipped filesystem, so a moved tag there is a
# real question, not a shrug. (Measured 2026-08-22: it has in fact drifted —
# `cgr.dev/chainguard/static:latest` now resolves to f68e3a82…, not the 77d8b892…
# pinned below. Scanned the PINNED digest at every severity: 0 findings, so this
# is not an exposure — but do not file it under "the pin working".)
#
# ⚠️ And note the limit of the verification above: `go version` proves the
# TOOLCHAIN, which is all this stage contributes. It cannot see base-OS content,
# so it is not a general "this digest is fine" check.
# Resolved and verified 2026-08-22 on darwin/arm64; the pin is the multi-platform
# index digest, which also covers the linux/amd64 the release lane builds.
FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build
WORKDIR /src

# Module cache layer — only re-downloads when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION stamps main.version — the same value `tierd version` and
# /api/v1/livez report (#49/#66). Pass it from CI / goreleaser, e.g.
# `--build-arg VERSION=$(git describe --tags --always --dirty)`. Defaults to
# "docker" so an unstamped image is still identifiable rather than claiming "dev".
ARG VERSION=docker
# COMMIT is injected because this build CANNOT discover it: .dockerignore excludes
# .git, so the Go toolchain's buildvcs stamping finds no repository and records
# nothing. Without this the shipped image reports no build identity at all (#638).
# Empty is honest — internal/api omits the field rather than guessing.
ARG COMMIT=""

# CGO_ENABLED=0 → static binary with no dynamic libc, runnable on a scratch /
# minimal static base. -trimpath matches the Makefile build.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/tierd ./cmd/tierd

# Create the data dir owned by the runtime nonroot uid (65532) so a freshly
# created volume mounted at /data is writable by the unprivileged runtime user.
RUN install -d -o 65532 -g 65532 /out/data

# --- runtime stage ---
# Chainguard Wolfi static — the canonical minimal base. Minimal attack
# surface (no shell, no package manager), ships CA certificates (sets
# SSL_CERT_FILE; the reverse proxy makes outbound HTTPS to api.anthropic.com /
# api.openai.com), and runs as non-root uid 65532 by default — matching the
# /data ownership below. Digest-pinned per D2 (mandatory). Refresh the digest:
# `crane digest cgr.dev/chainguard/static:latest`.
FROM cgr.dev/chainguard/static:latest@sha256:77d8b8925dc27970ec2f48243f44c7a260d52c49cd778288e4ee97566e0cb75b
# Only the tierd binary is shipped in the runtime image.
COPY --from=build /out/tierd /usr/local/bin/tierd
COPY --from=build --chown=65532:65532 /out/data /data

# HOME=/data so tierd's default DB path (~/.tier/tier.db via os.UserHomeDir)
# resolves inside the writable volume — a `serve` that omits --db still lands in
# /data rather than the non-writable /home/nonroot.
ENV HOME=/data

# Persist the SQLite DB here; point --db at it. The default ~/.tier path is not
# writable under the nonroot user, so an explicit --db /data/tier.db is expected.
VOLUME ["/data"]
WORKDIR /data

EXPOSE 8080

# tierd is the entrypoint; pass the subcommand + flags as args. A container that
# serves traffic OUT of the container must bind 0.0.0.0 AND set a token — the
# default bind is loopback and a non-loopback bind without a token is refused
# (fail-closed, #59). Provide the token via env (never a literal flag, #37):
#   docker run --read-only --cap-drop=ALL --security-opt no-new-privileges \
#     -p 8080:8080 -e TIER_API_TOKEN=… -v tier-data:/data tierd \
#     serve --addr 0.0.0.0:8080 --db /data/tier.db
# (--read-only is safe: tierd needs no writable rootfs, only the /data volume.)
# The default CMD just prints the version (a safe, side-effect-free default that
# never starts an unauthenticated listener by accident).
# The container probe (#571). This base has NO shell, NO wget and NO curl, so
# every shell-form HEALTHCHECK and every `CMD curl …` form is unavailable — the
# only executable in the image is tierd, which is why `tierd healthcheck`
# exists. EXEC form (JSON array) is mandatory here: shell form would be run as
# `/bin/sh -c …`, and there is no /bin/sh.
#
# Declaring it here is what makes `docker ps` report healthy/unhealthy instead
# of a bare "Up" — nothing is applied unless the image declares it. Note a
# downstream `FROM` DOES inherit this instruction (which is why `HEALTHCHECK
# NONE` exists, to switch an inherited one off).
#
# It probes /api/v1/livez on loopback: liveness only. It deliberately does not
# gate on /healthz, whose 503 reflects SUBSYSTEM health (e.g. a degraded
# watcher) — restarting the container does not fix a degraded capture path, and
# marking the dashboard unhealthy for it would take down a server that is
# serving correctly. Operators who want subsystem gating can override:
#   HEALTHCHECK CMD ["/usr/local/bin/tierd","healthcheck","--path","/api/v1/healthz"]
#
# The probe assumes a `serve`/`demo` container. This image can also run
# one-shot commands (the default CMD is `version`; `doctor` and `reprice` are
# others) — those bind no port, so they are marked unhealthy despite working.
# Docker itself takes no action on that, but compose `condition:
# service_healthy` and swarm do; give such containers `--no-healthcheck`.
#
# An exec-form CMD does NO shell expansion, so --addr cannot be templated here.
# A container serving on a non-default address retargets the probe with
# `-e TIER_HEALTHCHECK_ADDR=0.0.0.0:9090`, not by editing this line.
#
# Docker's --timeout MUST exceed the probe's own default (2s) so the probe
# self-bounds and reports WHY before Docker kills it for taking too long.
# start-period covers boot (DB open + migrations) without counting failures.
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/tierd", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tierd"]
CMD ["version"]
