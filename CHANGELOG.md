# Changelog

All notable changes to TIER are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project will
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches v1.

## [Unreleased]

## [0.3.0] - 2026-08-02

The container could not report whether it was healthy. That is the headline.

### Added

- **`tierd healthcheck` — a container health probe that works on a distroless
  image** (#571). The runtime image is Chainguard Wolfi static: no shell, no
  `wget`, no `curl`. Every shell-form `HEALTHCHECK` and every `CMD curl …` form
  is therefore unavailable, so the image declared no `HEALTHCHECK` at all —
  `docker inspect` reported none, `docker ps` could only ever show a bare `Up`,
  and operators had to assert liveness externally. `tierd` is already in the
  image, so it is the one thing a probe can call. The Dockerfile now declares an
  exec-form `HEALTHCHECK`.

  It asserts **liveness only** — that the port is bound and the server answers.
  Deliberately not `tierd version` (which proves the binary runs, not that the
  server listens) and not `tierd doctor` (which wants a git repo and an
  attribution floor, and would report unhealthy for reasons unrelated to
  serving). A healthcheck that fails for the wrong reason is worse than none.

  It probes `/api/v1/livez`, which stays open for probes and reports the build
  version, so a passing probe also identifies *which* binary answered. It does
  **not** gate on `/healthz`, whose 503 reflects subsystem health — restarting a
  container does not fix a degraded capture path. `--path` selects it, but note
  that for the shipped image you must replace the whole `HEALTHCHECK`
  instruction — exec form does no shell expansion, so a flag cannot be injected
  into it; the Dockerfile carries the full override line. `TIER_HEALTHCHECK_ADDR` retargets the probe when the server binds a
  non-default address, since an exec-form `HEALTHCHECK` does no shell expansion.

  A 2xx whose response never *completes* **fails** — deadline blown, or the
  connection dropped mid-response. A handler wedged after writing its status
  line still emits 200, and treating that as healthy would let `docker ps`
  report healthy forever while no response ever finishes. (A legitimately empty
  body, such as a `204`, still passes: truncation reads as a clean EOF, so only
  a genuinely broken exchange fails.)

- **A CVE re-scan of published images, on a schedule** (#560). A published
  digest is immutable, so it goes from clean to critical with no commit and no
  signal. Pinning answers "same bytes?", attestation answers "was it gated?";
  neither answers "is it vulnerable today?".

  ⚠️ **Read where it runs before relying on it.** The scheduled workflow is
  guarded to the development repository and is an explicit **no-op here** — it
  is shipped for reference, not running against this repository. It scans this
  project's own published tip (`ghcr.io/tiermetric/tierd:latest`) only: older
  tags are not re-scanned, and it does **not** scan any image *you* publish.
  `make cve-rescan` runs the identical code locally against targets you
  configure, which is the form an adopter would actually use.

- **A scope assertion for the demo tunnel** (#540). The demo's accepted
  `cloudflared` risk is bounded by the tunnel routing exactly one hostname, and
  that bound is now asserted by `scripts/demo-tunnel-scope-gate.sh` rather than
  stated in prose. See `deploy/DEMO.md` → "Accepted risk".

## [0.2.1] - 2026-07-30

The honesty UI did not render in 0.2.0. That is the headline of this release.

### Fixed

- **🔴 Eight dashboard elements rendered their content and were never visible**
  (#516). Every one is a caveat surface: the provenance stamp naming the price
  table, the attribution-coverage warning, the trust strip, the unjoined-developer
  strip, the unattributed-spend breakdown, the per-developer detail card, and BOTH
  halves of the compare view. Each carried a stylesheet `display: none` and was
  revealed with `el.style.display = ''`, which drops the inline override and hands
  control straight back to the rule that says `none`. 0.2.0 therefore showed
  confident numbers with every hedge suppressed, which is the opposite of what
  this project is for. Reveals now assign an explicit box, and a guard derives the
  hidden-element set from the assets rather than a hand-maintained list.
- **The dashboard's first paint defaulted to a 90-day window** (#497), the
  configuration that reads roughly twice too high in the flattering direction on
  an installation whose cost capture began recently. Now 30 days.
- **A window starting before cost capture began is now stated, not silently
  priced** (#512). `data_quality` carries the cost horizon, an explicit `false`
  when the window is covered (so "checked and clean" stays distinguishable from
  "no signal"), and the earliest `since` that clears the warning. `tierd doctor`
  gained a cost-horizon check.
- **`cost_per_point` is now null rather than `0` for a zero-point row** (#472). A
  lower-is-better field serialised its "no accepted outcome" case as the best
  possible value.
- **`billing_mode` was discarded by the JSONL collector and both org pollers**
  (#525), so stored rows claimed a per-token basis they had not earned in a column
  both `/export` surfaces publish. Cost is unchanged; only the discarded mode is
  recovered. Forward-only — `tierd reprice` repairs existing rows.
- **A damaged Codex rollout log read as an idle session** (#526). Malformed lines
  are still tolerated (the logs are appended live), but the loss is now counted
  and reported instead of reaching only a log line.
- **`tierd ship` silently dropped the repository on every event** (#491). The
  shipper's wire payload carried no `repo` field, so cost forwarded to a central
  tierd was stored under the `unqualified` sentinel and could never be joined to
  that repository's outcomes. `--repo-slug` was parsed and validated and then
  discarded, despite its help text stating that omitting it means "your cost
  never joins your outcomes". Multi-repo installs were additionally exposed to
  issue-number collisions across repositories — the exact fusion the `repo`
  column exists to prevent. Measured on one real multi-repo installation: every
  shipped event was unqualified.

  **Forward-only.** `repo` is intentionally excluded from the token-event upsert
  so a repo-blind producer can never downgrade a row another producer already
  qualified. Re-shipping an existing window therefore collides on the
  idempotency key and leaves stored rows unqualified — already-captured history
  is NOT repaired by upgrading. A repair path is tracked separately (#493).

### Changed

- **The four data-quality banners are one framed band** (#520) carrying a
  collective line ("TIER ran 4 data-quality checks on this window…"), ordered by
  observed severity. Same caveats, nothing hidden. Mobile is deliberately a
  scroll-to-the-number experience: no caveat is folded to fit a viewport.
- **Packaging fixes that made 0.2.0 unbuildable from the published tree.** `tools/`
  is now shipped, so `make check` passes on a clean clone; the docs index no longer
  links a file the export does not carry; and the internal `CLAUDE.md` is replaced
  by the slim public variant as the publish runbook always intended.
- **`serve --codex-rollout` without `--watch-repo` now refuses to start** rather
  than warning and continuing with Codex capture silently disabled (#464). An
  explicit request that cannot capture anything is a misconfiguration, and a
  startup warning was not enough to stop an operator believing Codex spend was
  being recorded. The error names the remedy. Note the asymmetry that motivated
  the change: outcomes still arrive by webhook and backfill, so uncaptured spend
  inflates TIER rather than lowering it.

## [0.2.0] - 2026-07-23

First release after the initial public tag. Everything here is a first-run /
remote-access fix: v0.1.0 shipped a dashboard you could not reach from another
machine, and a `demo --db` that could delete a real database.

### Added
- `docs/quickstart.md`, served by the running binary at `/docs/quickstart` and
  linked from the README — verified command-by-command against the binary,
  including how to reach the dashboard from another machine.
- `tierd demo --addr 0.0.0.0:PORT` now works. The synthetic read-only demo is
  exempt from the non-loopback bind guard via a structural, flag-unreachable
  signal; `serve` on real data still refuses a non-loopback bind without a token.

### Fixed
- **`tierd demo --db <path>` could delete a real capture database.** The guard
  is now fail-closed: it enumerates every user table and refuses any database
  holding rows outside the demo seeder's own tables — including tables with no
  developer/org column, such as `webhook_payloads`.
- `tierd -version` (and `-help`) work; previously only the bare `version`
  subcommand did.
- Every subcommand's `-h` exits 0 instead of 1.
- `go install`ed binaries report the module version instead of `dev`.
- `tierd score` outside a git repository now names the remedy (`--repo <path>`),
  and its closing tip points at the correct full-score path (`backfill`, then
  `serve`).

### Changed
- **`serve --codex-rollout` with no `--watch-repo` now fails at startup** rather
  than silently capturing nothing. `--read-only` warns instead of aborting.
- Documentation installs via `@latest` rather than a pinned tag, so the
  quickstart always matches the newest published release.


## [0.1.0] - 2026-07-19

The first public release.

### Added
- Deterministic TIER scoring — outcome per $1,000 of list-price AI spend — with
  Coverage % and cost-per-point companion metrics.
- Zero-setup laptop mode (`tierd score`), server mode (`tierd serve`: dashboard,
  GitHub-webhook outcomes, reverse proxy, live JSONL watching), and history
  reconstruction (`tierd backfill` for outcomes, `tierd ship` for 90-day cost).
- `tierd doctor` install-fidelity checks and `GET /api/v1/fidelity`.
- Honesty-first presentation: sub-50%-coverage rows dimmed, windowing skew
  documented, no absolute good/bad band.
- Team/developer/division aggregation with a k-anonymity floor for team mode.

[Unreleased]: https://github.com/tiermetric/tier/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/tiermetric/tier/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/tiermetric/tier/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/tiermetric/tier/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/tiermetric/tier/releases/tag/v0.1.0
