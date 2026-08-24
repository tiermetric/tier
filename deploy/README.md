# Deploying tierd

Operator artifacts for running `tierd serve` unattended: a systemd unit
(`tierd.service`), a Docker Compose file (`docker-compose.yml`), and an
environment template (`tierd.env.example`). Read this file first — four
non-obvious facts below will otherwise cost you an outage in the first week.

## 1. A restart policy is REQUIRED, not optional

`tierd serve` deliberately **exits the process with status 1** on terminal
failure. Two paths reach that exit:

- The JSONL watcher (`--watch-repo`) fails terminally. Its supervisor restarts
  it in-process with exponential backoff, but gives up after 5 failures inside a
  60-second window and forwards the error to the main goroutine, which exits 1.
- The HTTP listener crashes.

The design intent: **transient failures are handled in-process by the
supervisor; terminal failure is delegated to the init system.** tierd does not
attempt to resurrect itself past its restart budget — it exits so that whatever
supervises the process can restart it from a clean state.

That design is only safe when something restarts the process. Without a restart
policy (`Restart=always` in systemd, `restart: unless-stopped` in Compose),
tierd stays down after a terminal failure and **live capture silently stops
until a human notices.** Both artifacts here set the policy; do not remove it.

## 2. Probe paths and semantics

The health and liveness probes live under `/api/v1/`, **not** at the
conventional root `/healthz`. Wire them exactly as below — a probe at the wrong
path, or liveness pointed at the readiness endpoint, will restart-loop the
process during normal watcher backoff.

| Path | Purpose | Auth | Codes |
|---|---|---|---|
| `/api/v1/livez` | liveness — process is up | none | always 200 |
| `/api/v1/version` | build identity — WHICH build is running (#638) | none | ⚠️ **404 on every release published to date** — see below |
| `/api/v1/healthz` | readiness — subsystems healthy | none | 200 / 503 while watcher restarting |
| `/metrics` | Prometheus scrape | Bearer token | 200 / 401 |

There is also a legacy `/api/v1/health` that returns a static ok; prefer
`livez`/`healthz` for new wiring.

> 🔴 **`/api/v1/version` is NOT in any published release yet (#653).** It merged at
> `495a6a3`, *after* v0.4.0 was exported from `957c067`, so it answers **404** on
> v0.4.0 — measured on the published image. **Do not wire a probe or a deploy check
> to it** until a release built from a commit at or after `495a6a3` ships.
> ⚠️ A correctly deployed v0.4.0 and a deploy that never happened **both** return
> 404 here, so a check against this path cannot tell them apart. Until then, read
> `/api/v1/livez` → `.version` (which discriminates *releases*, not builds).
> The container reports `0.4.0`, the release tarball `v0.4.0` — strip the leading
> `v` before comparing.

**Wire liveness to `livez` and readiness to `healthz`.** Do NOT wire liveness to
`healthz`: `healthz` returns 503 while the watcher is in its backoff/restart
cycle, so a liveness probe pointed there would kill and restart the whole
process during exactly the transient condition the in-process supervisor exists
to ride out — defeating the supervisor. `livez` always returns 200 once the
listener can answer, which is the only thing a liveness probe should test.

Both `livez` and `healthz` are unauthenticated by design (status only, never
spend data), so a probe needs no credentials.

## 3. Secrets: environment or file indirection, never literal flags

Provide `TIER_API_TOKEN` and `TIER_WEBHOOK_SECRET` through the environment (an
`EnvironmentFile` for systemd, an `.env` file for Compose) or through the
`@/path/to/file` indirection form (e.g. `--api-token @/run/secrets/tier-token`,
which reads the token from that file). **Never** pass a secret as a literal flag
value: it leaks into `ps` output and shell history.

The bind is fail-closed: a non-loopback listen address (anything other than
`127.0.0.1`/`::1`) **refuses to start without `TIER_API_TOKEN`**. A read-only
token alone does not satisfy this — a network-facing listener always requires
the write/admin token.

Generate a token with `openssl rand -hex 32`.

## 4. `--aggregation` is REQUIRED and has no default

`tierd serve` **fails to start** unless the reporting mode is set to `team` or
`developer`, from a CLI flag, the `TIER_AGGREGATION` env var, or the
`aggregation` config key. There is deliberately no default: defaulting would
flip an existing deployment between naming individuals and not on upgrade, an EU
works-council / GDPR Art. 22 co-determination concern.

- `team` — emit only team-level aggregates; never name an individual. The safe
  posture under EU works-council regimes. Teams smaller than the k-anonymity
  floor (default 5, hard minimum 3) collapse into an aggregate `other` bucket.
- `developer` — keep named per-developer rows (only where per-individual
  measurement is lawful and consented).

Both artifacts here set `team` as the safe default. The systemd unit sets it via
the config file (`aggregation: team`); the Compose file sets it explicitly on
the command line. If you omit it, the process exits at startup with an error
telling you to choose.

## 5. Authenticated Prometheus scrape

`/metrics` is bearer-token-gated (scope: read). A default Prometheus scrape with
no credentials gets a silent 401. Configure the scrape with an authorization
credential:

```yaml
scrape_configs:
  - job_name: tierd
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/tier-api-token
    static_configs:
      - targets: ["tierd-host:8080"]
```

`/metrics` accepts **either** the write/admin `TIER_API_TOKEN` or the read-only
`TIER_READ_TOKEN`. Prefer the read-only token for Prometheus (least privilege):
a scrape credential should never carry write or erase power. Put that token
alone in `/etc/prometheus/tier-api-token`, owned by and readable only by the
Prometheus user (`chmod 0600`).

## Container run reference

The Dockerfile ships a hardened `docker run` example in its trailing comment
(read-only rootfs, all capabilities dropped, no-new-privileges). The
`docker-compose.yml` here mirrors that posture. The runtime image is a
Chainguard static base: non-root (uid 65532), no shell, no package manager. See
the note in `docker-compose.yml` about why there is no in-container healthcheck.

## Data directory ownership

tierd writes a single SQLite file and creates its parent directory mode `0700`;
the DB files themselves are `0600`. Run tierd as a dedicated, unprivileged user
that owns the data directory:

- systemd: `User=tier` plus `StateDirectory=tier` (systemd creates and chowns
  `/var/lib/tier`).
- Compose: the container already runs as uid 65532; the named `tier-data` volume
  is writable by that user.

Never run tierd as root.
