# Security hardening guide

This guide is written for the person deploying TIER and for the security
reviewer doing diligence on it. TIER's security model is a set of deliberate,
documented trade-offs: they are safe **only when the operator understands
them**. Every behavioral claim below carries a `(code: ...)` pointer so you can
verify it against the source in minutes rather than take it on faith.

Scope note: TIER today is **single-process, single-tenant**. There is no
per-organization isolation — developer identifiers are global. Do not deploy a
single `tierd` as a shared multi-organization service.

Related reading:

- [privacy.md](privacy.md) — the full, code-grounded account of what TIER reads
  and stores (this guide summarizes its headline guarantee).
- [legal-and-privacy.md](legal-and-privacy.md) — deploying where per-developer
  measurement is legally constrained.
- [webhook-setup.md](webhook-setup.md) — configuring the GitHub webhook.
- [conventions.md](conventions.md) — the attribution conventions TIER depends on.

---

## 1. What TIER stores — and what it never stores

> **TIER never persists prompt or completion content.** The token-capture path
> parses provider responses and Claude Code session files through an allowlist:
> from that path, only token counts, model names, timestamps, and attribution
> identifiers (developer, issue, repository) reach disk. Prompt text, completion
> text, source code, file contents, and diffs are never deserialized, never
> transmitted, and never stored.

Why the guarantee holds — the parsers are allowlist structs, so any field not
named in the struct is dropped by the JSON decoder before it exists in memory:

- JSONL session collector: `jsonlEntry` decodes only `type`, `timestamp`,
  `gitBranch`, `cwd`, `sessionId`, and `message.{id,model,role,usage.*}` — usage
  is token counts, not text (code: `internal/collector/jsonl.go`, `jsonlEntry`).
- Reverse proxies: the response parsers extract only `id`, `model`, and `usage`
  from provider response bodies (code: `internal/proxy/proxy.go`).
- Seam-capture fixtures enforce the same allowlist for any captured test data
  (code: `testdata/seam-jsonl-ingestion/README.md`).

What the capture path persists, exactly — the columns of `token_events` (code:
`internal/store/store.go`, `schemaTables`):

| Column | Meaning |
|---|---|
| `developer` | attribution identifier (not a real name unless you make it one) |
| `issue_id` | derived issue identifier |
| `repo` | canonical `owner/repo`, or the `unqualified` sentinel (#231) |
| `model` | model name |
| `input_tok`, `output_tok`, `cache_read`, `cache_write_5m`, `cache_write_1h` | token counts |
| `cost_micro` | cost in integer micro-dollars |
| `source`, `fidelity` | capture provenance |
| `ts` | timestamp |

Branch names are read into memory to *derive* `issue_id`, but only the derived
identifier is written — the branch string itself is never persisted (code:
`internal/store/store.go`, the token-event insert path).

**One other table stores free text — and it is not prompt content.** When the
GitHub webhook path is enabled, TIER retains the **raw GitHub webhook body**
(gzipped) for every processed delivery in the `webhook_payloads` table, so a
score input can be re-derived later (code: `internal/store/store.go`,
`webhook_payloads`; `internal/webhook/handler.go`, `InsertWebhookPayload`;
#137). That body **does contain personal data**: commit author names and email
addresses, PR titles and descriptions, and commit messages — the same PR/commit
metadata GitHub itself already stores. It is bounded by a 90-day age cap and a
50,000-row cap (code: `internal/store/store.go`, `PruneWebhookPayloads`), stored
in the same `0600` single-tenant database, and is **deliberately excluded from
the GDPR erase route** (`DELETE /api/v1/developer/{id}`), so a contributor name
embedded in an old payload is aged out by retention, not by erasure (code:
`internal/store/store.go`, `EraseDeveloper` table list). This is the only
free-text-at-rest surface, it is PR/commit metadata rather than prompts or
code, and it exists only when you run the webhook. See [privacy.md](privacy.md)
for the full operator-facing disclosure and the deeper allowlist walkthrough.

---

## 2. The API token is org-secret-grade

A single `TIER_API_TOKEN` gates all writes, the score GETs, `/metrics`, and both
reverse proxies (code: `internal/api/handler.go`, `requireAuth` / `requireRead`
/ `ProxyAuth`). Treat this token like a CI deploy key, **not** a per-user
credential. Any holder of the write token can:

- **Read every developer's spend and ranking** — the score and export GETs
  return organization-wide data to any valid token.
- **Forge cost attribution.** `POST /api/v1/costs` accepts an arbitrary
  `developer` field in the request body (code: `internal/api/handler.go`,
  `handlePostCosts`), and the reverse proxy attributes spend from a
  client-supplied `X-Tier-Developer` request header (code:
  `internal/proxy/proxy.go`). Nothing checks that the caller *is* that
  developer.
- **Forge outcomes (inflate the score numerator).** `POST /api/v1/outcomes` is
  write-scoped to the same token (code: `internal/api/handler.go`,
  `handlePostOutcome`), so a holder can fabricate merged-PR outcomes and inflate
  any developer's TIER score — not just the cost denominator. This is the same
  capability the webhook HMAC protects against for the GitHub path (see below).

There is no per-developer authorization: TIER cannot today restrict a token to
one developer's own data. That capability is deliberately deferred to a possible
enterprise tier (#65). **Consequence:** if forged attribution matters to you, do
not hand the write token to individual developers. Front `tierd` with your own
authenticating proxy that sets attribution from an authenticated identity, and
keep the TIER token server-side.

**Read-only viewer scope (shipped, #190).** A separate `--read-token` /
`TIER_READ_TOKEN` grants a least-privilege viewer credential: it is accepted on
`GET /scores`, `GET /scores/{developer}`, `GET /metrics`, and the bulk exports
(`GET /events`, `GET /outcomes`), and is rejected with 403 on every write, on
the finance/admin GETs (`org_actual_spend`, `developer_alias`), on the GDPR
export/erase routes, and on the proxies (code: `internal/api/handler.go`,
`requireRead` and the `Register` route table). It lets you hand a CFO or
VP-Eng dashboard access without the write, erase, or forge power the write
token confers. The read token must differ from the write token — `tierd` refuses
to start otherwise (code: `cmd/tierd/main.go`, `checkReadToken`) — and, on its
own, does **not** permit a non-loopback bind: the bind check is gated on the
write token alone (code: `cmd/tierd/main.go`, `validateBind`).

**Rotation** is a restart with a new token — there is no session state to
invalidate. Both tokens support `@/path/to/file` indirection and env-var
sourcing so the secret stays out of `ps` output and shell history (code:
`cmd/tierd/main.go`, secret `@file` resolution; #37).

---

## 3. Tokenless mode and its trust boundary

Running with an empty `TIER_API_TOKEN` disables bearer auth. This is **safe by
default, not unconditionally.** `tierd` fails closed: with no token it refuses
any non-loopback bind at startup (code: `cmd/tierd/main.go`, `validateBind`;
#59). Three residual caveats you must understand:

1. **The literal hostname `localhost` is trusted by convention.** `validateBind`
   accepts `localhost` without resolving it. An attacker-controlled `/etc/hosts`
   entry that points `localhost` at a routable address can therefore defeat the
   check. Bind to the `127.0.0.1` literal if you want the IP-level guarantee.
2. **Loopback can be re-exposed from outside the process.** An SSH tunnel, or a
   container port-map such as `docker run -p 0.0.0.0:8080:8080`, re-publishes a
   loopback listener to a network. The bind check cannot see past the process
   boundary — that exposure is on you.
3. **Zoned IPv6 literals are refused** (for example `[::1%lo0]`) — deliberately,
   because the safe direction is to ask for the plain form rather than guess.

In tokenless mode the read token has no effect, and the reverse proxies are an
unauthenticated relay to the upstream provider — which is exactly why the bind
is restricted to loopback. If you need to expose TIER on a network, set a token.

---

## 4. Filesystem hardening

**The SQLite database and its `-wal` / `-shm` sidecars are created mode `0600`
(owner read/write only), and legacy files are repaired to `0600` on every
start** (code: `internal/store/store.go`, `Open`; #130). Files are created tight
(`O_CREATE` with `0600`), so there is no transient world-readable window, and a
`chmod` sweep at the end of `Open` tightens any pre-existing `0644` file. The
containing directory is created `0700` (code: `cmd/tierd/main.go`) — but if you
pre-create the state directory yourself, `MkdirAll` leaves its existing mode
untouched, so set it `0700` in that case. You do **not** need to `chmod` the
database by hand.

Caveats:

- **POSIX only.** The `0600` guarantee relies on POSIX mode bits. On Windows
  `os.Chmod` only toggles the read-only attribute, so this protection does not
  apply there.
- **Re-tightened every restart.** Because `Open` re-applies `0600` on each start,
  a manual relaxation does not stick. A backup agent or group that needs to read
  the DB must run **as the `tierd` user**, not rely on group-readable bits.
- **Run `tierd` under a dedicated, unprivileged user** whose home holds the
  state directory, so the `0700`/`0600` owner-only permissions actually isolate
  the data from other accounts on the host.
- **Backups inherit the sensitivity.** Any copy of the DB (or its sidecars) holds
  the same per-developer spend and attribution data. Protect the backup
  destination exactly as you protect the live file — the mode bits do not travel
  with a copy you make yourself.

---

## 5. Rate-limit lockout topology

`tierd` has a per-IP failed-authentication lockout: after `--auth-max-failures`
failures within `--auth-failure-window`, the offending IP is locked out with a
429 for `--auth-lockout` (defaults: 10 failures / 60s → 15 minutes) (code:
`internal/api/ratelimit.go`, `DefaultRateLimitConfig`). It keys on the **direct
TCP peer** and, by default, **ignores `X-Forwarded-For`** — because a client
controls that header, and honoring it would let an attacker mint unlimited
lockout buckets and defeat the limiter entirely (code:
`internal/api/ratelimit.go`, `clientIP`).

**Consequence behind a shared TLS terminator, reverse proxy, or NAT:** every
client arrives from the same direct peer address, so they all share **one**
lockout bucket. A single misconfigured client sending bad tokens can lock out
the whole organization for the lockout window.

Mitigations, in order of preference:

1. **Configure `--trusted-proxy-cidr` (shipped, #131).** When the direct peer is
   inside a trusted CIDR you supply, the limiter instead keys on the real client
   from `X-Forwarded-For` — specifically the rightmost hop *not* inside a trusted
   CIDR, which is the address your own edge appended and thus the one value a
   client cannot forge (code: `cmd/tierd/main.go`, `--trusted-proxy-cidr`;
   `internal/api/ratelimit.go`, `clientIP`). This flag is also settable in the
   config file as `http.trusted_proxy_cidrs`. Set it to your terminator's
   address range and per-client lockout is restored — this works only if your
   edge actually sets a trustworthy `X-Forwarded-For`; if the trusted peer
   forwards no XFF, `clientIP` falls back to the shared peer address.
2. Fix the failing client (it is usually a stale or wrong token).
3. Raise `--auth-max-failures`, or rate-limit at your terminator instead.

Note: the limiter keys on the full client address; there is no coarser subnet
bucketing, so distinct clients get distinct buckets once `--trusted-proxy-cidr`
is configured correctly.

---

## 6. Webhook authentication

The GitHub webhook is authenticated with **HMAC-SHA256** over the request body,
validated against the `X-Hub-Signature-256` header on every request (code:
`internal/webhook/handler.go`, `verifySignature`). It is **fail-closed**: with
no `TIER_WEBHOOK_SECRET` configured, the route is not even mounted, and if the
handler is reached without a secret it rejects every request with 403 (code:
`cmd/tierd/main.go`, webhook mount guard; `internal/webhook/handler.go`, #60).
An unauthenticated webhook would let anyone who can reach the listener forge
merged-PR outcomes.

Supply the secret via `TIER_WEBHOOK_SECRET` or the `@/path/to/file` indirection
so it stays out of `ps` output and shell history (code: `cmd/tierd/main.go`,
#37). See [webhook-setup.md](webhook-setup.md) for the GitHub-side
configuration.

---

## 7. Proxy header hygiene

The reverse proxies attribute spend from three internal request headers —
`X-Tier-Developer`, `X-Tier-Issue`, and `X-Tier-Repo` — and **strip all three
from the outbound request before forwarding upstream**, so they never reach the
provider (code: `internal/proxy/proxy.go`, the `Rewrite` hook). The
`X-Tier-Token` proxy credential is likewise stripped before forwarding (code:
`internal/api/handler.go`, `ProxyAuth`). A non-canonical `X-Tier-Repo` value is
ignored and never logged.

---

## 8. Log hygiene (log-injection resistance)

Every value derived from an untrusted request that reaches a log record flows
through a sanitizer (`internal/logsafe`, wrapped at call sites as `logSafeStr` /
`logSafeErr`). It **strips carriage returns and line feeds** — the explicit
CR/LF removal that forms the forged-log-record barrier — then `%q`-quotes the
result to escape any remaining control bytes, invalid UTF-8, or quotes, and caps
the length. A structured logger plus this sanitizer means an attacker cannot
inject a newline to forge a second, attacker-authored log line.

A small, deliberate set of logged fields are left un-wrapped because they are
**provably constrained by construction** and cannot carry a control byte: GitHub
issue references (`issue-<digits>`), commit SHAs (`[0-9a-f]{40}`), the webhook
event type (allowlist-bounded to `pull_request` / `push` / `workflow_run`),
upstream-validated period strings, and numeric fields such as PR numbers. Each is
documented at its call site.

Note on static analysis: an automated code scanner may report these constrained
bare fields as potential log-injection findings. They are false positives — the
scanner cannot prove the regex/allowlist constraint that the code guarantees —
and are triaged as such, rather than "fixed" by wrapping a value that already
cannot forge a record.

---

## 9. Reporting a vulnerability

If you find a security issue in TIER, report it **privately** — do not open a
public issue, and do not include a working exploit in a public channel.

Use GitHub's private vulnerability reporting on this repository
(**Security → Report a vulnerability**), which opens a disclosure channel
visible only to the maintainers.

A dedicated `SECURITY.md` will carry the full disclosure policy, supported-version
window, and response-time commitment before public release. Until it lands, the
private reporting channel above is the supported path.

---

## Planned / not yet shipped

For accuracy, these are **not** current capabilities — do not rely on them:

- **Per-developer authorization** on the API token (#65). Today one token reads
  and forges all attribution; see section 2.
- **Multi-tenant isolation.** TIER is single-tenant; developer identifiers are
  global (see the scope note at the top).
