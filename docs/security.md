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

> **TIER never persists the prompt or completion content of the usage it
> captures.** The token-capture path
> parses provider responses and Claude Code session files through an allowlist:
> from that path, only token counts, model names, timestamps, and attribution
> identifiers (developer, issue, repository) reach disk. Prompt text, completion
> text, source code, file contents, and diffs are never deserialized and never
> stored. The optional reverse proxy is the one path that carries them: by design
> it forwards your requests to the provider and the responses back to you,
> reading each response in memory as it passes to find its usage. It sends them
> nowhere else. The one exception to never persisting such text is the free
> text of raw GitHub webhook payloads, stored as written, with whatever was
> pasted into it (below).

Why the guarantee holds — the parsers are allowlist structs, so any field not
named in the struct is dropped by the JSON decoder before it exists in memory:

- JSONL session collector: `jsonlEntry` decodes only `type`, `timestamp`,
  `gitBranch`, `cwd`, `sessionId`, and `message.{id,model,role,usage.*}` — usage
  is token counts, not text (code: `internal/collector/jsonl.go`, `jsonlEntry`).
  With worktree attribution on (`--worktree-attribution`, off by default, #823), a
  separate allowlist, `toolPathLine`, also decodes each content block's type and
  tool name and the `file_path`, `notebook_path` and `path` values of every tool
  call's input, whatever the tool, and keeps one path per call for seven tools;
  the rest of the input and every command line are skipped unread (code:
  `internal/collector/toolpath.go`; detail in
  [privacy.md](privacy.md#worktree-attribution-off-by-default)).
- Reverse proxies: the response parsers extract only `id`, `model`, and `usage`
  from provider response bodies (code: `internal/proxy/proxy.go`).
- Seam-capture fixtures enforce the same allowlist for any captured test data
  (code: `testdata/seam-jsonl-ingestion/README.md`).

What the capture path persists is the `token_events` table. Its columns are
defined once, in `internal/store/store.go` (`schemaTables`, the
`CREATE TABLE token_events` statement), and none holds prompt or completion
text. Which stored fields can identify a person, and how, is listed once in
[privacy.md § What IS stored](privacy.md#what-is-stored).

Branch names are read into memory to *derive* `issue_id`, and only the derived
identifier is written to a `token_events` row; the branch string is never stored
there (code: `internal/store/store.go`, the token-event insert path). The Claude
Code watcher's resume checkpoint (`watcher_checkpoint`) does store branch names,
the working directory and the session id; privacy.md lists its full contents
(code: `internal/collector/watcher.go`, `checkpointFromState`).

**The webhook table stores third-party free text, as written.** When the
GitHub webhook path is enabled, TIER retains the **raw GitHub webhook body**
(gzipped) for every processed delivery in the `webhook_payloads` table, so a
score input can be re-derived later (code: `internal/store/store.go`,
`webhook_payloads`; `internal/webhook/handler.go`, `InsertWebhookPayload`;
#137). That body **does contain personal data**: commit author names and email
addresses, PR titles and descriptions, and commit messages — the same PR/commit
metadata GitHub itself already stores. It is bounded by a 90-day age cap and a
50,000-row cap (code: `internal/store/audit.go`, `PruneWebhookPayloads`), stored
in the same `0600` single-tenant database, and is **deliberately excluded from
the GDPR erase route** (`DELETE /api/v1/developer/{id}`), so a contributor name
embedded in an old payload is aged out by retention, not by erasure (code:
`internal/store/store.go`, `EraseDeveloper` table list). It is the only table
that holds third-party PR/commit text; that text is stored as written, so it can
hold whatever someone pasted into it ([privacy.md](privacy.md#what-is-stored)),
and it exists only when you run the webhook. It is not the only
free text at rest: [privacy.md](privacy.md) lists every other stored field,
including the client-chosen and operator-supplied strings, and walks through the
allowlist in more depth.

---

## 2. The API token is org-secret-grade

A single `TIER_API_TOKEN` gates all writes, the score GETs, `/metrics`, and all
three reverse proxies (`/anthropic`, `/openai`, `/gemini`) (code: `internal/api/handler.go`, `requireAuth` / `requireRead`
/ `ProxyAuth`). You choose its value (`openssl rand -hex 32`; see
[README → What you need before you start](../README.md#what-you-need-before-you-start)).
Treat this token like a CI deploy key, **not** a per-user credential. Any holder
of the write token can:

- **Read every developer's spend and scores** — the score and export GETs
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

There is no per-developer authorization today: TIER cannot restrict a token to
one developer's own data. Per-viewer read tokens for `developer` mode (the
developer, their declared manager, and an optional skip-level) are planned
(#826); until they land, in `developer` mode any valid token reads every
developer's data. The server does not check that the developer named in a write
is the token holder; that stays deferred to a possible
enterprise tier (#65). **Consequence:** if forged attribution matters to you, do
not hand the write token to individual developers. Front `tierd` with your own
authenticating proxy that sets attribution from an authenticated identity, and
keep the TIER token server-side.

**Read-only viewer scope (shipped, #190).** A separate `--read-token` /
`TIER_READ_TOKEN` (generate it the same way, in its own file) grants a
least-privilege viewer credential: it is accepted on every route in
`registerReadRoutes` (`internal/api/handler.go`) — the score reads, the report
manifest, the bulk exports, the quality and fidelity reads, and, in `developer`
mode only, `GET /metrics` ([§ The metrics token](#the-metrics-token-944)) —
and is rejected with 403 on every write, on the finance/admin GETs
(`org_actual_spend`, `developer_alias`), on the GDPR export/erase routes, and on
the proxies (code: `internal/api/handler.go`, `requireRead` and the `Register`
route table). It lets you hand a CFO or VP-Eng dashboard access without the
write, erase, or forge power the write token confers. The read token must differ
from the write token — `tierd` refuses to start otherwise (code:
`cmd/tierd/main.go`, `checkReadToken`) — and, on its own, does **not** permit a
non-loopback bind: the bind check is gated on the write token alone (code:
`cmd/tierd/main.go`, `validateBind`).

**Rotation** is a restart with a new token — there is no session state to
invalidate. All three tokens support `@/path/to/file` indirection and env-var
sourcing so the secret stays out of `ps` output and shell history (code:
`cmd/tierd/main.go`, secret `@file` resolution; #37). If an `@file` secret is
readable by group or other users, `tierd` logs one WARN naming the setting and
mode, never the path or the contents, and still starts, since container secret mounts are often
`0444` (no check on Windows). Restrict the file to the user `tierd` runs as:
`chmod 600` if that user owns it; for a container or Kubernetes secret mount, set
the mount's file mode (`mode`/`defaultMode`) instead, only as far as `tierd`'s
user can still read it. A WARN on a mount `tierd` reads as another user is
expected (#920).

### The metrics token (#944)

`--metrics-token` / `TIER_METRICS_TOKEN` (or `@/path/to/file`, or the
`metrics_token` YAML key) is a scrape-only credential. It opens `GET /metrics`
and is refused with 403 on every other route and on the proxies (code:
`internal/api/handler.go`, `requireMetrics`). In `team` and `division` mode the
read token gets 403 on `/metrics`, so a scraper there needs the metrics token or
the write token; with no metrics token configured, only the write token scrapes.
In `developer` mode the read token still scrapes. The metrics token must differ
from the write and read tokens (`tierd` refuses to start otherwise; code:
`cmd/tierd/main.go`, `checkDistinctTokens`), shares the failed-auth lockout, and
on its own does not permit a non-loopback bind. With no token configured at all
(a loopback install), `/metrics` is open in every mode, like every read route.

**The metrics token is operator-only.** `/metrics` exports running counters of
spend and activity. Two scrapes a short interval apart give what arrived in
between, which in a quiet interval is one person's spend and activity timeline,
below the *k* floor the anonymised modes enforce everywhere else. The token
decides who can take that difference; it does not make the counters safe. Never
give it to a viewer, and never wire it into a dashboard other people can read.
Anything that can query the Prometheus that scrapes it (Grafana, federation,
remote-write) sees the same counters, so its readers must be operator-only too.

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
`X-Tier-Developer`, `X-Tier-Issue`, and `X-Tier-Repo` — and **strip every
`X-Tier-*` header from the outbound request before forwarding upstream**, so none
reaches the provider (code: `internal/proxy/proxy.go`, the `Rewrite` hook). That
includes the `X-Tier-Token` proxy credential, in every auth mode; it is checked
only when an API token is configured (`--api-token`, `TIER_API_TOKEN` or the
`http.api_token` config key; code: `internal/api/handler.go`, `ProxyAuth`). A non-canonical `X-Tier-Repo` value is
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

**Errors are the sixth class, and the rule for them is narrower than "wrap
everything".** An error is not exempt from the barrier just because it is an
error: a store or validation error can *wrap* a free-form identifier (a developer
id, org, team, or division that is length-capped but never charset-validated), and
those sinks go through `logSafeErr` — the hierarchy writes, the `/outcomes` insert,
and the per-developer score reads all do. The majority of `err` values are logged
bare instead, and the discriminator is whether the error's *text* can carry a
client value, not whether the operation touched one: the SQLite driver does not
echo bound parameters, so a failed `INSERT` whose arguments include a hostile
developer id still produces a constraint message that names only the table and
column. A sweep of the request-facing packages (`internal/api`,
`internal/webhook`, `internal/proxy`, `cmd/tierd`) found the values that *are*
interpolated into `internal/store` error text are either operator-supplied
(config route prefixes, DB paths, table names) or routed through `logsafe` at the
point of construction. Wrapping the rest would add noise and blur exactly the
client/operator distinction this barrier depends on.

An earlier revision of that sentence listed *"price-table model names"* among the
operator-supplied values. That was **backwards**, and it is corrected here rather
than quietly dropped because the error it excused was real. The model names
interpolated into `store.Reprice`'s GUESS-gate error are by construction the
models **absent** from the price table — the producer-supplied strings that
matched no operator-curated entry, arriving through `POST /api/v1/events`, which
length-caps `model` and applies no charset check. They are now routed through
`logsafe.Join` at construction, pinned by
`TestReprice_GuessGateErrorIsNotForgeable`.

Note on static analysis: an automated code scanner may report these constrained
bare fields as potential log-injection findings. They are false positives — the
scanner cannot prove the regex/allowlist constraint that the code guarantees —
and are triaged as such, rather than "fixed" by wrapping a value that already
cannot forge a record.

**What the barrier actually buys, measured (#321, 2026-08-04).** Two honest
qualifications, because "a sanitizer is applied" is not by itself a threat model:

- On a `log/slog` handler the CR/LF forge is *already* structurally blocked
  before `logsafe` runs: both `TextHandler` and `JSONHandler` quote or escape any
  attribute value containing a control byte, so a newline cannot open a second
  record through them. What `logsafe` adds on those sinks is the **length cap**
  and the static-analysis barrier. The cap is not theoretical: a 4 KB upstream
  header value reached one log line unbounded before this was fixed, which is what
  `TestProxy_ContentEncodingNotForgeable` now pins.
- The CR/LF strip is load-bearing for the sinks that are **not** `slog` — the
  operator-facing report writers in `cmd/tierd` that print to stdout, where
  nothing quotes — and for any log consumer that unquotes a field to render it.
  Escaping is reversible downstream; removal is not.

**Code scanning checks this class on the development repo.** The
`go/log-injection` query declares `@precision medium`, and GitHub's *default*
code-scanning suite includes only `precision: high|very-high`
(`misc/suite-helpers/code-scanning-selectors.yml` in the `github/codeql` repository), so the query runs only under
the `extended` suite. The private development repo, where every change is made,
runs CodeQL default setup with `query_suite: "extended"`, so it runs this query
(measured 2026-09-25). The public mirror ran the same `extended` setup on
2026-08-04; on 2026-09-25, while the mirror was private, GitHub reported code
scanning unavailable there. Either way, a clean Security tab is **not** proof
that this section holds — the named tests are (`internal/logsafe`,
`TestProxy_ContentEncodingNotForgeable`, and the per-sink `*NotForgeable` tests
in `internal/api`, `internal/webhook`, `internal/collector`, `internal/store`,
and `cmd/tierd`).

⚠️ **Read that list precisely, because it was once read too generously.** Until
2026-08-04 the only such test in `cmd/tierd` covered the **access log**
(`TestRequestLogger_PathNotForgeable`), and no test covered a **report writer** —
a different sink class with a different threat model, since the access log is
`slog` (which escapes CR/LF even when the caller forgets) and a report writer is
raw `fmt.Fprintf` (which escapes nothing). The sentence read as coverage of a
class that had none, and `cmd/tierd/reprice.go` diverged from its
correctly-wrapped sibling `repairrepo.go` without a single test going red. The
report writers are now covered by `TestPrintRepriceResult_NotForgeable` and
`TestPrintRepairRepoResult_NotForgeable`.

---

## 9. Reporting a vulnerability

If you find a security issue in TIER, report it **privately** — do not open a
public issue, and do not include a working exploit in a public channel.

Use GitHub's private vulnerability reporting on this repository
(**Security → Report a vulnerability**), which opens a disclosure channel
visible only to the maintainers.

The full disclosure policy, supported-version window, and response-time
commitment are in [SECURITY.md](../SECURITY.md).

---

## Planned / not yet shipped

For accuracy, these are **not** current capabilities — do not rely on them:

- **Per-viewer read tokens** for `developer` mode (#826), and **per-developer
  authorization of writes** on the API token (#65). Today one token reads and
  forges all attribution; see section 2.
- **Multi-tenant isolation.** TIER is single-tenant; developer identifiers are
  global (see the scope note at the top).
