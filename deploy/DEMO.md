# Deploying the public demo (demo.tiermetric.org)

Runbook for the **public, read-only demo instance** — the live board a visitor
sees at `demo.tiermetric.org`. It is deliberately different from the real
deployment (`docker-compose.yml` + `README.md`): it serves **synthetic data**
(`tierd demo`), it is **read-only at the binary** (#429), and it is exposed
through a **Cloudflare Tunnel to loopback**, so `tierd` is never directly on the
network and needs no API token.

> This is a manually-executed runbook. There is no CI/CD deploy pipeline (see the
> repo `CLAUDE.md` — all gates are local); a human runs these steps and then runs
> the verification checklist. **Merged ≠ deployed ≠ verified.**

## What it is (and is not)

- **Data:** synthetic only — `demo-*` developers, `DEMO-*` issues, an
  `ACME (DEMO)` org, recreated on every start. The console prints a `SYNTHETIC
  DATA, NOT REAL SCORES` banner. The real dogfood database **never** goes here.
- **Writes:** structurally absent. `tierd demo` runs in `--read-only` mode (#429):
  every write/ingest/admin route, the GitHub webhook, the ingest proxies, the
  JSONL watcher, and the coverage pollers are off (404 / not started). A leaked
  request reaches no mutation because no write route exists on the mux.
- **Reads:** open by design. `--read-only` governs writes only; the score/board
  reads are public. That is safe **because the data is synthetic** — do not point
  this stack at a real DB.
- **Aggregation:** `developer` mode (the demo shows each synthetic developer's own
  TIER, the richest board). The names are obviously fake. If a stricter public
  posture is wanted, this is the one thing to revisit (team mode = no named rows).

## Topology

```
Internet ──TLS──▶ Cloudflare edge ──tunnel──▶ cloudflared ──▶ 127.0.0.1:8080 (tierd demo)
                                              └─ shares tierd-demo's network namespace ─┘
```

`tierd demo` binds `127.0.0.1:8080` **inside the container**. The `cloudflared`
sidecar uses `network_mode: service:tierd-demo`, so it shares that network
namespace and reaches the loopback listener; the tunnel routes
`demo.tiermetric.org` → `http://127.0.0.1:8080`. No container port is published,
so `validateBind` (#59) is satisfied (loopback) and there is no **API** token to manage. (The tunnel's *connector* token is a separate thing and is very much managed — see "Accepted risk" below.)

## Ownership

| Step | Owner |
|---|---|
| Build/publish the `tierd` image | tier (release lane on the `v0.1.0` tag, or `make docker` for a host build) |
| The compose manifest + this runbook | tier (`deploy/docker-compose.demo.yml`) |
| Cloudflare Zero Trust: create the Named Tunnel, get its connector token | infrastructure-management |
| DNS: `demo.tiermetric.org` public-hostname route → `http://127.0.0.1:8080` | infrastructure-management |
| Run the stack on the demo host; edge rate-limiting | infrastructure-management |

## Prerequisites (infrastructure-management, one-time)

1. In **Cloudflare Zero Trust → Access → Tunnels**, create a **DEDICATED** Named
   Tunnel for the demo host. Copy its **connector token**.
   🔴 **Do NOT reuse an existing multi-hostname tunnel.** This is not a
   preference — it is the condition the #540 CVE acceptance rests on (see
   "Accepted risk" below). Asserted by `scripts/demo-tunnel-scope-gate.sh`.
2. Add **exactly one Public Hostname** on that tunnel: `demo.tiermetric.org` →
   `http://127.0.0.1:8080` (HTTP; TLS is terminated at the Cloudflare edge).
3. Put the token in the untracked env file:
   ```sh
   cp deploy/cloudflared.env.example deploy/cloudflared.env
   # edit deploy/cloudflared.env → CLOUDFLARE_TUNNEL_TOKEN=<token>   (chmod 0600)
   ```
4. ✅ **Pin the cloudflared image digest — DONE (#536, 2026-07-29).** Pinned in
   `docker-compose.demo.yml` by tag AND digest — that file is the single source of
   truth for the value, so it is deliberately not repeated here. The digest is what
   Docker enforces; the tag keeps the line readable and the upgrade reviewable.
   This was a DEPLOY GATE, not a later hardening step — the demo is itself public
   and cloudflared holds the tunnel-ingress credential while sharing tierd's
   network namespace. **To upgrade, re-resolve BOTH** (`crane digest
   cloudflare/cloudflared:<tag>`) and change them together. ⚠️ A tag bumped
   **without** its digest does NOT revert to a mutable pull — Docker keeps
   honouring the old digest and ignores the new tag, so the file lies about what is
   running and the upgrade never shipped. (Verified in checklist step 0.)
5. Configure **rate-limiting** at the Cloudflare edge for `demo.tiermetric.org`
   (a single-writer SQLite process behind one connector is trivially overwhelmed;
   the demo is read-only but reads still hit the DB).

## Accepted risk — cloudflared CVEs (#540)

**Ruled on 2026-08-01 (#540): Option A — accept, bounded by blast radius.**

`cloudflare/cloudflared` ships with known-unfixed HIGH advisories and **no
patched release exists on any channel** (Docker Hub, apt, GitHub binaries and
`master` were all checked — all the same build). Re-pinning is not a remedy, so
this is accepted rather than fixed.

**The bound — this is the acceptance, and it holds by SHAPE, not by version:**

> Total compromise of the `cloudflared` sidecar buys an attacker the ability to
> serve a fake-data page at `demo.tiermetric.org`, and nothing else.

That is acceptable because everything reachable from the compromised side is
already public by design: the demo's data is **synthetic**, the board is
**read-only** with write/ingest/admin/webhook routes structurally absent, and the
DB is **recreated on every start**. The only asset of real value is the connector
token — whose worst case is exactly the sentence above, *provided the tunnel
routes one hostname*.

**Conditions. The acceptance is void if any of these stops being true:**

| # | Condition | Enforced or checked by |
|---|---|---|
| 1 | The tunnel routes **exactly one** public hostname, and its catch-all is terminal | **Enforced:** `scripts/demo-tunnel-scope-gate.sh` (checklist step 0b) |
| 2 | The demo serves **synthetic** data only | **Enforced:** `tierd demo` unconditionally deletes and reseeds its DB on every start. (The `guardDemoDBPath` check is a *different* control — it refuses to CLOBBER a real DB, protecting the operator's data, not the demo's synthetic-ness. Do not cite it for this condition.) |
| 3 | Write routes stay structurally absent | *Checked* post-deploy: steps 5–6 |
| 4 | The image stays **digest-pinned** | *Checked* pre-deploy: step 0 |

⚠️ Rows 3 and 4 say **checked**, not **enforced**, on purpose: they are runbook
steps a human performs, not controls that fire on their own. Only rows 1 and 2
hold without someone remembering.

**Deliberately NOT the basis for this acceptance: a reachability argument.**
A trace was run (`govulncheck -mode=binary`, recorded on #540) and it is *less*
clean than the prose assessments claimed — it clears the `os.Root` CVE but finds
grpc client symbols reachable per upstream's own symbol list, plus a third
stdlib advisory that `trivy` never named. A reachability argument also expires
on every new advisory, on a component we do not control. The bound above does
not. **Do not "upgrade" this acceptance to a reachability claim.**

**Owner of record: infrastructure-management** — they hold the Cloudflare
account, mint the connector token, and run the host. Risk cannot be accepted by
a party that does not hold the asset; tier's deliverable is this impact
statement and the gate, not the acceptance itself.

**Reopen when:** Cloudflare ships a release fixing the advisories (re-scan and
re-pin), **or** any condition above is broken, **or** the demo is ever pointed at
non-synthetic data — which would invalidate the bound entirely, not weaken it.

⚠️ Not a calendar reminder, and **do not assume a scanner is watching this
image — nothing in this repo re-scans it.** **Re-scan at deploy time**, as part
of running this runbook. A re-check nobody performs is the same as no re-check, and an acceptance
that silently depends on one is worse than an honest unmonitored acceptance.

## Run

`make docker` needs the repo cloned on the host **plus a Go toolchain**. On a
host that has neither, pull the release image instead (published by the `v0.1.0`
release lane) and retag it `tierd:latest`, or set the compose `image:` to the
release ref — **by DIGEST** (`crane digest ghcr.io/tiermetric/tierd:<tag>`), never
by a mutable tag. Deploy-gate step 0 only inspects cloudflared, so an unpinned
`tierd` here passes it silently.

```sh
make docker                      # builds tierd:latest from the repo Dockerfile
docker compose -f deploy/docker-compose.demo.yml \
  --env-file deploy/cloudflared.env up -d
```

## Verification checklist (run after every deploy)

```sh
# 0. DEPLOY GATE — the cloudflared image is digest-pinned, not :latest (prereq 4).
#    Anchored to the `image:` LINE, with a negative arm. An unanchored grep matches
#    a digest anywhere in the file — including one left behind in a COMMENT while
#    `image:` was reverted to a tag, which is exactly the artifact the upgrade
#    instruction above invites. Mutation-tested: unanchored passed all three of
#    (pin-in-comment, second unpinned service, digest-in-a-label).
#    The {64} also rejects a TRUNCATED digest, which the older pattern accepted.
grep -qE '^[[:space:]]*image:[[:space:]]*cloudflare/cloudflared[^[:space:]]*@sha256:[0-9a-f]{64}[[:space:]]*$' deploy/docker-compose.demo.yml \
  && ! grep -qE '^[[:space:]]*image:[[:space:]]*cloudflare/cloudflared([^@[:space:]]*)?$' deploy/docker-compose.demo.yml \
  && echo "cloudflared pinned — OK" || echo "FAIL: pin the cloudflared digest before going public"
```

```sh
# 0b. DEPLOY GATE — the tunnel routes EXACTLY ONE public hostname (#540).
#     This is the assertion the CVE acceptance rests on: it is what makes
#     "a compromised cloudflared can only serve a fake-data demo page" TRUE
#     rather than merely intended. Run it as infrastructure (the token needs
#     Account > Cloudflare Tunnel > READ — never a write-capable token).
#
#     rc 0 = bound holds · rc 1 = bound BROKEN · rc 2 = could not run.
#     🔴 rc 2 is NOT a pass. Read it un-piped; do not `| tee` this.
CF_API_TOKEN=<read-only-token> CF_ACCOUNT_ID=<id> CF_TUNNEL_ID=<uuid> \
  scripts/demo-tunnel-scope-gate.sh
rc=$?
case $rc in
  0) echo "tunnel scope — OK" ;;
  1) echo "FAIL: #540 bound BROKEN — do not go public" ;;
  *) echo "FAIL: COULD NOT CHECK (rc=$rc) — this is NOT a pass" ;;
esac

# ⚠️ CF_TUNNEL_ID is hand-supplied and NOTHING ties it to the tunnel this stack
#    actually runs. A stale or mistyped UUID gives a green gate for an asset that
#    is not in use — a pass about the wrong thing. Confirm it matches the tunnel
#    whose connector token is in deploy/cloudflared.env before trusting rc=0.
#
# Prove the gate itself works before trusting it (no credentials needed).
# 14 arms incl. the two false-pass shapes a review caught: a catch-all pointing
# at the app, and a locally-managed tunnel whose API config is not in force.
scripts/demo-tunnel-scope-gate.sh --selftest
```

From the demo host:

```sh
# 1. tierd is live on loopback (inside the tierd-demo namespace / on the host)
docker compose -f deploy/docker-compose.demo.yml exec -T tierd-demo \
  /usr/local/bin/tierd version        # image runs; or check `docker compose logs tierd-demo`
```

Then, publicly through the tunnel:

```sh
# 2. Liveness is green
curl -fsS https://demo.tiermetric.org/api/v1/livez            # 200

# 3. The board renders (dashboard HTML)
curl -fsS https://demo.tiermetric.org/ | grep -q '<title>TIER' && echo "dashboard OK"

# 4. Scores read (synthetic board)
curl -fsS "https://demo.tiermetric.org/api/v1/scores?since=2000-01-01" | head -c 80

# 5. SECURITY SPOT-CHECK — a write route must be structurally absent (404, NOT 401;
#    401 would mean the route is mounted and merely token-gated). The 404 (vs a
#    method-mismatch 405) is supplied by the dashboard "/" catch-all, which stays
#    mounted in read-only mode — that is why absence reads as 404 here.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  https://demo.tiermetric.org/api/v1/events)
[ "$code" = "404" ] && echo "writes absent (404) — OK" || echo "FAIL: POST /events = $code"

# 6. The webhook, proxies, AND the destructive GDPR erase route are absent too
#    (the erase route is the one an operator most wants proven unreachable).
for r in "POST /webhook/github" "POST /anthropic/v1/messages" "DELETE /api/v1/developer/demo-ada"; do
  m=${r% *}; p=${r#* }
  curl -s -o /dev/null -w "$m $p -> %{http_code}\n" -X "$m" "https://demo.tiermetric.org$p"
done   # expect 404 for each

# 7. Positive check — the board is SYNTHETIC (the demo-* cast is present)
curl -fsS "https://demo.tiermetric.org/api/v1/scores?since=2000-01-01" \
  | grep -q 'demo-' && echo "synthetic data confirmed — OK" || echo "FAIL: demo-* cast not found"
```

The boot log should carry both the `SYNTHETIC DATA, NOT REAL SCORES` and the
`READ-ONLY mode ENABLED (#429)` banners
(`docker compose -f deploy/docker-compose.demo.yml logs tierd-demo`).

## Operational notes

- **Fresh data on restart:** `tierd demo` recreates the synthetic DB on every
  start, so a restart is a clean reseed — there is no state to back up.
- **Stat freshness:** the demo's numbers are the synthetic demo dataset, not the
  dogfood figures cited on the marketing site. If the marketing site's dogfood
  stats are refreshed at reveal, that is a separate step (owned by tier) — the
  demo instance itself needs no stat refresh.
- **No writable rootfs needed:** `tierd-demo` runs `read_only: true`; only the
  `demo-data` volume is writable. `cloudflared` keeps a writable rootfs (it may
  write connection state), caps dropped.
- **Restart policy is required** (`restart: unless-stopped`) — `tierd` exits 1 on
  a terminal listener failure and relies on the runtime to bring it back.
- **Restart coupling (expected):** because `cloudflared` shares `tierd-demo`'s
  network namespace, a `tierd-demo` crash/restart tears that namespace down and
  `cloudflared` must restart too — so the public endpoint blips (and `cloudflared`
  may briefly restart-loop until `tierd` is listening again) on any `tierd` crash.
  Both `restart: unless-stopped` policies converge, so it self-heals; don't page on
  a short tunnel drop that coincides with a `tierd-demo` restart.
