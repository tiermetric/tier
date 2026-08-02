# syntax=docker/dockerfile:1
#
# tierd container image (#66). modernc.org/sqlite is pure Go, so tierd builds as
# a fully static binary (CGO_ENABLED=0) and runs on a Chainguard Wolfi static
# base — no libc, no shell, no package manager.

# --- build stage ---
# golang:1.26.5 satisfies go.mod's `go 1.26.5`. 1.26.5 is the GO-2026-5856 fix (crypto/tls ECH
# privacy leak) — the build stage is what links crypto/tls into the static binary,
# so a CI govulncheck pass on an older builder does NOT make the shipped artifact
# clean. Digest-pinned (mandatory — admission control rejects
# non-digest-pinned refs in staging/prod). Refresh the digest when bumping the
# tag: `crane digest golang:<tag>`.
FROM golang:1.26.5@sha256:079e59808d2d252516e27e3f3a9c003740dee7f75e55aa71528766d52bcfc16a AS build
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

# CGO_ENABLED=0 → static binary with no dynamic libc, runnable on a scratch /
# minimal static base. -trimpath matches the Makefile build.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/tierd ./cmd/tierd

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
