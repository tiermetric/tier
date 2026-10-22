# Privacy

TIER is built so that adopting it does not mean handing over your source code or
your prompts. This document states exactly what TIER reads, what it stores, and
what it never touches — each claim grounded in the code so a security reviewer
can audit it.

JSONL token events store a branch only as nullable `branch_hash`, a pseudonymised SHA-256 of
canonical `repo + "\x00" + full branch name` with `refs/heads/` removed. Raw branch names are
never stored in `token_events`. Local watcher checkpoints keep the raw branch name in the
plaintext JSON fields `GitBranch` and `LastRealBranch`. Default/unknown branches and unknown
repositories produce NULL hashes. This is not
anonymisation: guessable branch names can be reversed by hashing candidates with the known
repo. Merged-PR outcomes store the same pseudonymised hash as `head_branch_hash`, never
raw `head.ref`, plus nullable PR `created_at` and `merged_at`. Webhooks and backfill use
head refs when available; API outcomes lack head refs and leave the hash NULL. Main,
master, HEAD and a payload-supplied repository default branch have NULL hashes.
Recognised revert exclusions retain the matching raw head ref or quoted title as
audit evidence in `outcome_exclusions`, under the PR author's access and erasure scope.

Branch credit fires only for still-unattributed events in the same repo and head-branch
hash inside `[max(previous merge on that repo and head branch, PR opened − 7 days), merge]`.
It never changes existing attribution or sealed months; overlapping recorded claims are
skipped. Reusing a branch bounds the next PR's credit at the previous merge; unrecorded
merges cannot be considered. Missing head/open/merge evidence disables branch credit.
The hash does not conceal guessable names. Existing webhook audit payloads retain their
original body under the separate audit retention policy below.

## The core guarantee

> **TIER never stores prompt or completion content, and never sends it anywhere
> you have not already sent it.** Every capture path parses with an allowlist. The
> Claude Code JSONL parser deserializes only `type`, `timestamp`, `gitBranch`,
> `cwd`, `sessionId`, and `message.{id,model,role,usage.*}` — token counts, not
> text; the optional reverse proxy passes your prompts on to the provider you chose
> and reads each response as it passes back to extract `id`/`model`/`usage`,
> which is all it keeps; the Codex
> rollout-log parser reads six fields, none of which is content; the Opencode
> reader touches one table (`message`) and six fields inside its JSON blob, never
> the tables holding prompts, tool output, or credentials; and the Muse Code
> reader decodes ids, timestamps, token counts, model names, the workspace root
> and branch records, never prompt, reply or tool text. One opt-in switch widens
> the Claude Code read: with **worktree attribution** on (`--worktree-attribution`,
> **off by default**), the parser also reads the file or folder *paths* in tool
> calls and uses the one that seven tools name, never the file's contents, the
> tool's other arguments, or a command line. See
> [Worktree attribution](#worktree-attribution-off-by-default).

## Why the guarantee holds

TIER parses inputs with **allowlist structs**. Only the fields named in the
struct are deserialized; every other JSON field — including any prompt or
completion text — is silently dropped by the JSON decoder and never enters
memory as a named value, let alone the database.

- Read-time recovery can attribute one person’s spend to the issue of a merged PR (theirs or a colleague’s) on the same branch, without storing anything.

- **JSONL capture** (`internal/collector/jsonl.go`, the `jsonlEntry` struct):
  reads `type`, `timestamp`, `gitBranch`, `cwd`, `sessionId`, and under
  `message`: `id`, `model`, `role`, and the `usage` token counters
  (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens`, and the 5m/1h cache-creation split). There is **no
  content or text field** in the struct. With worktree attribution on, a second,
  separate allowlist struct reads tool-call paths; it is described in
  [Worktree attribution](#worktree-attribution-off-by-default).

- **Codex rollout-log capture** (`internal/collector/codexrollout/parse.go`, the
  `rolloutLine` struct, #464): reads exactly six fields — the line `type` and
  `timestamp`, and under `payload`: `id` (the session UUID), `cwd`, `git.branch`,
  `model`, plus the `token_count` event's `info.total_token_usage` counters.
  Codex rollout logs contain a great deal more than that — the agent's full system
  prompt (`base_instructions`), your prompts, the model's messages and reasoning
  traces, tool output, and patch bodies — and **none of it is deserialized**: those
  fields have no counterpart in the struct, so the JSON decoder discards them. The
  `cwd` is read to decide which repository a session belongs to (a session outside
  every watched repo is dropped) and is **not stored** by this path — and, since it
  is never stored, it is never forwarded either, including by `tierd ship
  --codex-rollout` (#492). What that flag sends is the same usage-event shape as
  every other collector, listed under "Where it goes" below; enabling Codex capture
  adds no new *category* of data to what leaves the machine.

- **Opencode capture** (`internal/collector/opencode`, opt-in with `--opencode`,
  #719): Opencode keeps its sessions in a **SQLite database** rather than JSONL
  files, and TIER opens it **read-only** (`mode=ro`; never `immutable=1`, which
  would ignore Opencode's live write-ahead log). It reads **one table**,
  `message`, and from each row's JSON blob only the `role`, `modelID`,
  `providerID`, the `tokens` counts, `time.completed`, and the `path.cwd` used to
  decide which repository the message belongs to. That database also contains
  OAuth access and refresh tokens (`account`), API credentials (`credential`), and
  prompt and tool-output text (`part`) — **none of which TIER reads, stores, or
  forwards.** The `cwd` is used for the repo decision and is not stored, exactly as
  in the two paths above, so `tierd ship --opencode` adds no new *category* of data
  to what leaves the machine either.

- **Muse Code capture** (`internal/collector/muse/parse.go`, opt-in with
  `--muse`, #895): Muse Code's `session.jsonl` logs hold your prompts, the
  model's replies, tool arguments and tool output. The reader decodes each line
  in **two stages**: first only the fields every record carries (the record
  `id`, `recorded_at`, the session id `stream.id`, the run id, and the record
  and event kinds), and then, only for the five record kinds it uses, a second narrow
  struct for that kind: the model name and token counts of a billed call
  (`model_completed` or `automated_review_completed`, plus the review's
  `completed_at_ms`), the `workspace_root` of a `metadata` record, the run
  id, `workspace_root`, VCS and branch reference of a `workspace_branch`
  record, and the two ids of a `memory_reminder_child_session_linked` record
  (`parent_run_id` and `child_session_id`, which tie a subagent session to the
  run that started it, #901). **No prompt, reply or tool text is ever decoded**, and
  `TestDecoderAllowlist` fails if a field is added to any of those structs. It
  opens only files named `session.jsonl`, not the other files Muse keeps beside
  them. The `workspace_root` decides which repository a session belongs to and is
  **not stored**; the branch becomes an issue id, and the branch name itself is
  not stored. The Muse **session id is stored** (see "Session id" below). The
  collector keeps its scan position in memory and writes no checkpoint row, so
  `tierd ship --muse` adds no new *category* of data to what leaves the machine.

- **Copilot billing** (`collectors.copilot_billing`, new in v0.6.2,
  experimental): reads GitHub's organisation AI-credit billing reports. It
  stores the mapped developer identity (the user login when that is the
  canonical developer id), model, product, day and gross amount, plus an
  unattributed organisation remainder. Product is preserved in the
  reconciliation key; queried GitHub logins are not stored separately from
  the mapped developer. It stores no token counts, repository or issue linkage
  from this API.

- **Cursor Admin** (`collectors.cursor_admin`, new in v0.6.2, experimental):
  reads team usage events and stores the mapped developer, model, day, cost
  and available token counts. Email is used only to look up a case-folded
  alias to a rostered developer and is never stored. A colliding alias,
  unmapped user or service/cloud agent is unattributed. Conversation id is
  neither decoded nor stored. BYO-key events labelled `User API Key` are
  skipped, including Cursor's fee on those requests.

- **Reverse-proxy capture** (`internal/proxy/proxy.go`): the provider response
  parsers read only the message `id`, the `model`, and the `usage` token counts
  from the response body. The client gets the same response content
  the provider sent; TIER extracts usage and forwards the rest untouched. (Not
  always the same bytes: the proxy drops the client's `Accept-Encoding`, so a response the
  client asked to have compressed arrives uncompressed.)

Because the parsers are allowlists, adding no new fields is the default —
capturing content would require deliberately adding a content field, which does
not exist.

## Captured-dollar diagnostics

In developer mode, `GET /api/v1/diagnostics/captured-dollars` reads stored cwd,
session id and session-file path from `watcher_checkpoint` to group session-level
origin and working-directory evidence. The server also reads on-disk git worktree
metadata at those stored cwd paths: `.git` directories, worktree admin `gitdir`
files and linked checkout `.git` backlinks. It does not reopen session transcripts
or run git maintenance commands. It emits counts and captured-dollar shares,
never stored paths or session ids. Parent and subagent checkpoints sharing a
session id produce session-level origin, not per-event subagent attribution.
These live checks are separate from `/scores` and report manifests. Doctor reads
and formats the server's receipt; the endpoint returns no receipt in team or
division modes. See the [diagnostic limitations](technical-reference.md#captured-dollar-diagnostics).

## Worktree attribution (off by default)

**Why it exists.** Claude Code writes, on every line of a session file, the
folder the session started in (`cwd`) and that folder's git branch
(`gitBranch`). Many developers, and most coding agents, start Claude Code in the
main checkout of a repository and then do the work in a **git worktree**: a
second checkout of the same repository, on its own branch, in its own folder
(made with `git worktree add`). The session file still says "main", so TIER
books that spend to `unattributed:main` instead of to the issue the work was
for. Worktree attribution (#823) finds the worktree the session actually worked
in, from the paths its tool calls named. That needs a wider read of the session
file than the token parser makes, so it is **off unless you turn it on**. It is
an opt-in preview: its measured accuracy, and why a wrong attribution is
permanent, are in
[how-it-works.md](how-it-works.md#measured-accuracy-read-this-before-turning-it-on).
How to turn it on, and the rules it applies, are in
[how-it-works.md](how-it-works.md#worktree-attribution-823-off-by-default). This
section states what it reads and what it keeps.

Parents no longer carry (#1016); only message groups labelled `isSidechain:true` can use carry. Worktree attribution stays opt-in and **off by default**. Activation requires recorded gates, the operator's security rulings and an explicit operator decision. No new accuracy figures have been measured.

**What it reads from the session file.** A second allowlist struct
(`toolPathLine` in `internal/collector/toolpath.go`), separate from the token
parser's, decodes from each assistant line only:

- each content block's `type`, and its tool `name`;
- from each block's tool input, whatever the tool (MCP tools included), the
  string values of three keys, `file_path`, `notebook_path` and `path`, spelled
  exactly so. The decoder copies those three values into memory and skips every
  other member of the input unread.

It then **keeps one path per tool call, for seven tools only**: `file_path` for
`Read`, `Edit`, `MultiEdit` and `Write`; `notebook_path` for `NotebookEdit`;
`path` for `Glob` and `Grep`. Every other decoded value, including a `path`
that an MCP tool or `Bash` carries, is discarded there, before any disk read,
and never leaves memory. What the decoder skips is never copied: the text
`Edit` removes and inserts, the content `Write` writes, the pattern `Grep`
searches for, and `Bash`'s `command`. A relative path, a network path
(starting `//` or `\\`), a path over 4,096 bytes and a path containing a NUL byte
are dropped; at most 16 distinct paths are kept from one line
(`maxToolPathsPerLine`), from at most 64 content blocks (`maxToolBlocksPerLine`).
It also reads two markers each line already carries, `isSidechain` and
`agentId`, which tell a subagent's lines apart from its parent's
(`messageLineHead` in `internal/collector/messagemerge.go`). Only `isSidechain:true` permits carry; an absent, false or wrongly typed label does not.

**Which session files.** `tierd ship` and `tierd score` decode the tool paths
of every Claude Code session file whose session ended on or after `--since`,
and look up the git files below for every path they keep, **before** they drop
the sessions whose `cwd` is outside the repositories you named with `--repo`
(`parseSessionFileAttributed` in `internal/collector/jsonl.go`). So the `.git`
pointer files and worktree admin folders of other projects on the machine are
read too, to recognise them as another repository's; nothing from a dropped
session is stored. The live watcher also decodes the tool paths of every
session file it reads, but looks up git files only for sessions whose `cwd` is
in a watched repository.

**Command lines are not read.** Reading `Bash` commands (for `cd <path>` or
`git -C <path>`) was considered and is **not built**. The operator ruled
(via #826, recorded on #823) that it comes later, as its own change, behind its
own switch, off by default.

**What it reads from disk.** For each path it keeps, and for the session's own
`cwd` (rule 1 in how-it-works.md), it reads git's own bookkeeping files, never
the file the path names, and it never runs `git` to do it (`worktreeIndex` in
`internal/collector/worktreeindex.go`, `branchAt` in
`internal/collector/worktreereflog.go`). TIER still runs `git log` on the
repositories you configured, as it does with the switch off.

- the `.git` entry found by walking up from the path. In a linked worktree this
  is a small file pointing at the worktree's admin folder,
  `<main checkout>/.git/worktrees/<name>/`;
- in that admin folder, `commondir` and `gitdir`, which must point back at the
  same worktree (a moved, pruned or forged worktree fails this check and is not
  used), and `HEAD`;
- the worktree's reflog, `logs/HEAD` in the same folder, at most 1 MiB. It lists
  every checkout with a time (`checkout: moving from A to B`), which is how
  TIER learns which branch the worktree had checked out when each message was
  written. The whole file is read into memory; each line also carries the
  committer's name and email and, for a commit, its subject line, and none of
  them is kept;
- owners, permissions and modification times of those files. What is checked
  differs by file. The `.git` pointer file and the admin folder must be owned by
  the OS user running `tierd`. `HEAD`, the `logs` folder and `logs/HEAD` must be
  owned by that user **and** not writable by everyone. `commondir` and `gitdir`
  get neither check of their own; they are trusted only because they sit in the
  checked admin folder and must point back at the same worktree. A file that
  fails its check is not trusted, and the path falls back to today's rule.

Paths under `/tmp` and `~/.claude` are ignored.

**What it stores.**

- For every Claude Code event it records, one fixed label naming the rule that
  chose the issue: `branch`, `worktree-cwd`, `worktree-toolpath`, `carry`
  or `pr-number-at-insert` (#1074: server-assigned, never client-sent)
  (`token_events.attribution_rule`, see [What IS stored](#what-is-stored)). It
  is **never path text**: TIER refuses or drops any other value.
- Spend in a worktree of a repository you did not configure (not one of the
  command's `--repo` or watched repositories) goes to the
  fixed issue label `unattributed:foreign-repo`, with the repository stored as
  `unqualified`. No event stores that repository's name or path, and
  `tierd ship` never sends them. The watcher's local checkpoint is the
  exception: see item (c) below and **Watcher tail-state** under
  [What IS stored](#what-is-stored). Items (a) and (b) below are the known
  cases where spend tied to another repository is booked to your configured
  repository instead.
- **Known gaps, with this switch on or off (tier#1018; operator ruling
  2026-10-18: disclosed now, fixed after launch).** Items (a) and (b) book spend
  to the wrong issue in a repository you configured. A booking is permanent:
  TIER keeps the first one it writes and does not rewrite it.
  - (a) When an isolated-agent message inherits the most recent human-named
    branch seen in its session file, that branch can come from another
    repository. The message can then be booked to that branch's `issue-N` in
    your configured repository: with rule `branch` when this switch is on, and
    with no rule (shown as `legacy`) when it is off.
  - (b) A session that starts in a configured repository and later moves into
    another repository's main checkout (not a linked worktree) records that
    repository's branch on its later messages, and they can be booked to that
    branch's `issue-N` in the configured repository.
  - (c) The watcher's checkpoint keeps the raw branch names and the absolute
    working-directory path of each session file it tails, including session
    files in repositories you did not configure. So another repository's path
    and branch names can sit in that local row. They stay in the local database
    and are never shipped. A data-subject export
    (`GET /api/v1/developer/{id}/export`) returns the rows it can link to that
    person through their events, so in case (a) it can return another
    repository's branch name; rows for sessions with no events stay local.
    For sessions in configured repositories this is by design, because a
    restart needs them to resume. The rows for other repositories are to be
    cut down to the session id after launch.

  For (a) and (b), the event's `branch_hash` is a hash of your configured
  repository plus the other repository's branch name. It is not the name, but
  for a common branch name the hash can be guessed.

  The #823 gate run (one machine, about seven days) recorded that "the latch
  passed only `main` (0 of 229 cases)" and found no row with another
  repository's working directory. That is one machine, not a guarantee.
- In live watch mode only, the watcher's checkpoint row for a session file also
  holds the absolute path of the worktree it is carrying from one message to the
  next, so a restart resumes correctly (see **Watcher tail-state** under
  [What IS stored](#what-is-stored)). That checkpoint stays in the local database
  and is never sent.

**What it never stores or sends.** No tool-call path is written to an event or
sent by `tierd ship`, and the code that reads and classifies the paths logs
nothing. The dry-run audit
(`tierd score --worktree-attribution`) prints separate tool-path and carry counts and USD costs, repository names, issue
labels, session ids and times, never a path or any message content, and stores
nothing. With the switch off, none of this is read and events carry no rule.

**Messages held back until they finish (live watch mode only).** One reply can be
written as several lines of the session file, spread over minutes. With worktree
attribution on, the live watcher (`tierd serve --watch-repo`) does not store a
message until it has all of its lines, so each message is judged once, from
every path it named. A message still open at the end of the file is held back:
no event is stored for it meanwhile, though the file's checkpoint row (its
path, and the session id) is saved. When the file has had no new line for
**600 seconds** (`idleRelease` in `internal/collector/watcherattr.go`), the next
read releases it and stores it. The longest pause between two lines of one
message was 417 seconds when this was measured (2026-09-30, over 156,834
multi-line messages in 8,532 session files on one machine), so the wait is
longer than any measured pause. Idleness is judged by the file's modification
time, and after a restart the watcher reads at once any file whose checkpoint
stops short of its end. **A known gap:** if a line of a message arrives after
that message was released, it is read as a fragment and stored again under the
same message key; the store keeps the larger token counts and the **first**
row's issue, rule, cost and price version, so a late fragment cannot move a
message to a different issue, and that row's cost stays priced on the counts it
was first stored with while its token counts rise. A message held back when an
erasure runs can be stored after it: see the erasure limits under
[Data-subject rights](#data-subject-rights-access-and-erasure-gdpr-art-15--art-17).
`tierd ship` reads each file whole in one pass and holds nothing back.

## Shipper credential and identity limits (#1144)

The recommended credential for `tierd ship` is the server's `--ingest-token`
(`TIER_INGEST_TOKEN`, config `http.ingest_token`, or `@/path/to/file`). Supply it
on the laptop through `ship --api-token @/path/to/ingest-token` or
`TIER_API_TOKEN=@/path/to/ingest-token`. The admin token still works. The ingest
token opens only `POST /api/v1/events`; it cannot read scores or exports, post
outcomes, erase data, or use the provider proxies.

**Limits until per-developer binding (#1145, gated on #826):** a holder can post
events under any developer id, rostered or not: there is no roster check, and
this can pad a k-anonymity group.
A holder can also pre-claim an idempotency key a teammate would later use,
suppressing that row. The token does not identify a developer.
A holder who knows a key can replay it under the same developer with larger
counts, raising that row's token counts and cost (recomputed at the row's original
rates, #1231). Replaying a teammate's key this way raises that teammate's cost
(the TIER denominator); keys are visible to read-token holders via
`GET /api/v1/events` in developer mode.

A later cross-developer collision is rejected and reported as
`rejected_cross_developer` in the response, with the original row unchanged.
The server logs a WARN and the shipper prints the rejected count, without
failing the ship or retrying. Same-developer replays still deduplicate, with a
zero rejection count. Global idempotency stays unchanged; the guard cannot
reverse a pre-claim that was inserted first.

## What IS stored

TIER stores, in a **single local SQLite file** with **no external
transmission**:

- **Developer identifier** — an OS username (JSONL) or GitHub login (PR
  outcomes), and any alias→canonical mapping you configure.
- **Issue id** — derived from the branch name or PR/commit body (e.g.
  `issue-42`, `TIER-99`), or a fixed `unattributed:…` label naming why none
  was found. With worktree attribution on, spend in a worktree of a repository
  you did not configure gets the fixed label `unattributed:foreign-repo`
  and the repository `unqualified`, so that repository is not named.
- **Repository** (`token_events.repo`, `outcomes.repo`, #231) — the canonical
  `owner/repo` slug a cost was spent in or an outcome was earned in, taken from
  a per-repo `repo:` override, the repository's `remote.origin.url`, a GitHub
  webhook's `repository.full_name`, or the optional `repo` field of
  `POST /api/v1/events` or `POST /api/v1/outcomes`. It is the reserved value
  `unqualified` when the producer could not know the repository. A slug can
  identify a person: a personal repository such as `alice/side-project` names
  its owner.
- **Model names** — e.g. `claude-opus-4-8`.
- **Token counts** — input, output, and the cache read/write counters.
- **Cost** — list-price cost in integer micro-dollars, computed from the token
  counts via a versioned reference price table.
- **Pricing provenance** (`token_events.price_version`, #233;
  `token_events.host` and `token_events.billing_mode`, #300) — the version of
  the price table that computed the row's cost (`0` for a row no version was
  stamped on); the serving host the cost was priced against (for example the
  reverse proxy's upstream host name or an Opencode provider id), or `unknown`
  when the producer does not know it; and how that host bills: `per_token`,
  `subscription` or `self_hosted_amortized`. A caller of `POST /api/v1/events`
  may send its own `host` (up to 256 bytes), and TIER stores that string trimmed
  and lowercased but otherwise as sent, so it holds whatever the caller put in
  it — and a self-hosted host name such as `alice-laptop.local` can name a
  person.
- **Session id** (`token_events.session_id`, #238) — the **opaque agent session
  UUID** an event belongs to (Claude Code's `sessionId`, the Codex rollout
  log's `session_meta.id`, the `session_id` column of Opencode's `message`
  table, or the Muse Code log's `stream.id`), so context-bloat and rework-loop token-waste can be
  diagnosed at session grain. It is a random identifier only; it **carries no
  prompt content, no completion text, and no file contents**, and is **NULL** for
  rows a session-blind producer (the reverse proxy or an Admin-API poller)
  captured.
- **Idempotency key** (`token_events.idempotency_key`, #21) — the dedup key that
  makes a replayed event update one row instead of adding a second. TIER's own
  collectors and the reverse proxy store a SHA-256 hex digest. A caller of
  `POST /api/v1/costs` or `POST /api/v1/events` chooses its own key, and TIER
  stores that string as sent, so it holds whatever the caller put in it — which
  can include personal data such as an email address. It is **NULL** for rows
  that were stored without a key.
- **Billing declaration** (`token_events.billed_to`, #854) — `other` when a
  `POST /api/v1/costs` caller declared the row's spend is not billed to an org
  whose usage poller the server runs; **NULL** on every other row.
- **Attribution rule** (`token_events.attribution_rule`, #823) — which rule
  assigned the row's issue id: one of the fixed labels `branch`, `worktree-cwd`,
  `worktree-toolpath`, `carry`, `branch-at-merge` or `pr-number-at-insert` (#1074: server-assigned,
  never client-sent), **never path text**. It is **NULL** on older
  rows and on rows no rule was recorded for, is written only when the row is
  first inserted, is included in the data-subject export, and is erased with the
  row.
- **Issue extraction rule** (`outcomes.issue_rule`, `token_events.issue_rule`,
  and the existing `outcome_exclusions.issue_rule` snapshot column, schema 12) — a fixed closed-set label, never branch, path or message text.
  Outcomes retain server-derived webhook/backfill evidence; events retain
  client assertions. Existing rows and unknown/absent event rules are NULL,
  rendered as `legacy` in event/outcome exports; the data-subject access export
  preserves JSON `null`. The field is included last and erased with the existing
  outcome/event row. The identifier-free `issue_rule_cutover` singleton records
  the UTC migration time and maximum outcome/event IDs once and survives erasure;
  these boundaries distinguish the pre-upgrade band from later NULL rows
  ([cutover details](technical-reference.md#4-where-the-outcomes-come-from-prs-weights-quality-floors-direct-commits-outcomes-api)).
- **PR metadata** — PR number, author login, size-derived weight, quality, and
  the merge commit SHA.
- **Watcher tail-state** (**live watch mode only** — `--watch-repo` or the
  `watch.repos` config key) — when live ingestion is enabled, TIER persists one
  operational checkpoint row per JSONL file it tails (`watcher_checkpoint`),
  including session files whose repository you did not configure, so a restart
  resumes tailing from the last
  parsed byte instead of re-reading each file from the start. Each row's
  `metadata` column holds a small JSON blob describing the session being tailed:
  the **absolute working-directory path** (`cwd`), the **git branch** (and, when
  an isolated-agent session inherits one, the most recent human-named git branch
  seen so far in that file), and the **session id** (a UUID), plus the session
  start time, the first-observed model name, and an internal parse-sequence
  counter. For a session file in a repository you did not configure, that path
  and those branch names are that other repository's (tier#1018 (c)). With
  worktree attribution on (#823), it also holds the **absolute
  path of the linked git worktree** currently carried, if any.
  Nothing here is captured unless you run the watcher; a one-shot
  `tierd score` writes no checkpoint. Each row is keyed by the session file's
  **absolute path**. For Claude Code that file sits under the home directory of
  the account that ran the session, so the key itself names the **OS username**,
  the session's **working directory** (Claude Code names the project folder
  after the absolute working-directory path, so the folder name is effectively
  that path), and the session file's name, which is the session UUID. The row also holds the file's inode, the byte
  offset read so far, and a CRC-32 of the file's first bytes. An erasure keeps
  the key; see **Data-subject rights** below.
- **Opencode scan watermark** (**only with the Opencode collector enabled** —
  `--opencode` or the `collectors.opencode` config key) — the Opencode collector
  shares the same `watcher_checkpoint` table, under a row whose key is
  `opencode-db:<path to opencode.db>:<scope digest>`. The digest is SHA-256
  of the sorted configured repository paths (absolute where possible) and slugs;
  changing their order does not change the key. Adding a repository changes the
  key and triggers an idempotent backfill, including its rows older than the
  previous watermark. Legacy keys without the digest are not resumed, so an
  upgrade also triggers one idempotent backfill. Its `metadata` blob holds **two integers
  and a schema tag, and nothing else**: the highest Opencode `message.time_updated`
  already processed (epoch milliseconds), and the number of rows in Opencode's own
  `migration` table (a schema-drift tripwire). It carries **no cwd, no branch, no
  session id, no model name and no developer identifier** — unlike the JSONL
  watcher's blob above, which describes a session; this one describes only how far
  a scan got.
- **Raw GitHub webhook payloads** (**only when the GitHub webhook path is
  enabled** — i.e. you set `webhook_secret` / `TIER_WEBHOOK_SECRET` and point
  GitHub deliveries at `POST /webhook/github`; that endpoint is fail-closed and
  stays disabled without a secret) — for each *processed* delivery
  (`pull_request`, `push`, `workflow_run`), TIER retains the **raw GitHub webhook
  request body**, gzipped, in the `webhook_payloads` table so a score input can
  be re-derived months later (#137). Unlike every token-capture path above, this
  raw body **does contain personal data**: commit author **names and email
  addresses**, **PR titles and descriptions**, and **commit messages** — whatever
  GitHub places in a standard webhook payload (up to a 1 MB body). It is the same
  PR/commit metadata GitHub itself already stores, and TIER adds no prompt text,
  completion text or file contents to it. ⚠️ **The titles, descriptions and commit
  messages are free text, stored as written:** anything someone pasted into them
  (code, a prompt, a secret) is stored with them. A body is kept for every
  delivery of those event types, before any filter on action or branch, so pull
  requests that never merge and pushes to any branch are kept too; per GitHub's
  documentation, an `edited` delivery also carries the previous title and
  description. Editing text out on GitHub does not remove TIER's copy, and the
  per-developer erasure does not reach it: it stays until the retention bound
  below prunes it, unless you delete the rows directly (see *Known residual —
  `webhook_payloads`*).
  Retention is **bounded**: rows are pruned at startup and then every 24 hours
  while the server runs, by two limits — a **90-day age cap** and a **50,000-row cap** (oldest evicted first),
  enforced by `PruneWebhookPayloads` (`internal/store/audit.go`). Both bounds are
  currently compile-time constants (`webhookPayloadRetentionDays` /
  `webhookPayloadMaxRows` in `internal/store/audit.go`), **not config keys** —
  there is no setting to change the window or cap without a code change, and no
  flag to keep the webhook path enabled while suppressing retention. The one
  operator control is the path itself: **to store none of this, run TIER without
  the webhook path** (JSONL capture and `tierd ship` never populate
  `webhook_payloads`).
- **Sealed reports** (`sealed_report`, #913) — the finished aggregate report for
  a closed reporting period, stored once and never recomputed, with the settings
  it was built under. It holds only aggregate figures.
- **Sealed per-label totals** (`sealed_rollup`, #913) — for each label in a
  sealed period, the totals the report was built from (points, costs, outcome
  counts) before small groups were folded together. They are never served, and
  a label with a single member holds that one person's totals.
- **Sealed person keys** (`sealed_person`, #913) — for each label and measure in
  a sealed period, one keyed hash per person counted there. The hash is computed
  from the person's canonical id under the install secret, not stored as the id,
  but whoever holds the database file can recompute it from a guessed id.
- **Install secret** (`install_secret`, #913) — one random key per database,
  used for the sealed person keys. It names no one.
- **Canonical-id history** (`canonical_id_history`, #975) — every link from an
  alias to a canonical id, with the last sealed period committed before the
  alias edit that made it and before the edit that re-pointed or deleted the
  alias, so an erasure can
  find sealed person keys computed from a canonical id an alias edit has since
  retired. A link that already existed when schema version 4 first opened the
  database has no start time: when it began is unknown. Rows are never
  rewritten, except that a link's end is set once; an erasure deletes rows.

This list is not every table: the other per-developer tables (`actual_spend`,
`org_hierarchy`, `period_membership`, `quality_events`, `quality_history`,
`repo_repair_audit`, `push_outcome_commits` and `push_outcome_audit`) are in the erasure table under **Data-subject rights**
below, which also covers the audit ledgers and the operator-identifying fields
they carry (`actor`, `reason`, `forgotten_by`).

None of this is prompt text, completion text, code, file contents, or diffs, with
these exceptions: free text in raw webhook payloads (for example PR titles,
descriptions and commit messages) and the matching raw head ref or quoted title
in `outcome_exclusions` are stored as written, with whatever was pasted into them. The `session_id` above is likewise an **opaque UUID** — it groups a
session's events without persisting anything the session said or produced. Two of the entries above are conditional and hold data an auditor
should note. An ordinary watcher tail-state row is **derived operational state,
safe to delete** — the watcher rebuilds it from the live file on the next change.
⚠️ **An erasure tombstone is not** (see **Data-subject rights** below): deleting
one while its session file still exists lets the watcher read that file from the
start and store the erased session again. The tail-state contains no prompt or
completion text and no file contents; its `cwd`, its path key and (with worktree
attribution on) its worktree path are absolute *paths* (filesystem locations, not
the contents of any file), which can reveal an OS username or an internal project
name. Raw webhook payloads hold third-party personal data (contributor names and
emails) **at rest**, bounded to ~90 days. Revert exclusion evidence retains a
matching raw branch or title, which can also contain third-party identifiers;
it persists until the author's erasure or database deletion. Operators should
treat the local database file (mode `0600`, single-tenant) accordingly — and may
decline the webhook path entirely to store none of the webhook PII.

## Data-subject rights: access and erasure (GDPR Art. 15 / Art. 17)

Because TIER stores personal data keyed to a named developer, it ships two
bearer-gated (write/admin-scoped) admin endpoints so an operator can honour a
data-subject access or erasure request without hand-editing the database (#184):

- **Erasure** — `DELETE /api/v1/developer/{id}` deletes every row stored under
  that person's identifiers, and erases their watcher tail-state (see
  **Watcher tail-state** below), in **one transaction** (all-or-nothing). It
  first resolves `{id}` through the alias map (single-hop) and then removes rows
  stored under the canonical id *or* any raw login that aliases to it, across
  **all** developer-PII tables:

  | Table | What it holds |
  |---|---|
  | `token_events` | per-developer token counts and list-price cost |
  | `outcomes` | per-developer PR/issue outcomes, weight, quality |
  | `outcome_exclusions` | recognised revert PR tombstones and whole moved outcomes, PR author, classification evidence (the matching raw head ref or quoted title), source actor and timestamp; included in access exports and hard-deleted on erasure |
  | `actual_spend` | per-developer actual-paid amounts |
  | `org_hierarchy` | the developer's team/division/org |
  | `period_membership` | the developer's org-membership windows |
  | `hierarchy_membership` | the developer's dated team/division history, including the rows each alias carries from the day it was added (#886, #914) |
  | `quality_events` | per-developer CI/revert quality signals |
  | `quality_history` | per-developer quality-transition log |
  | `repo_repair_audit` | which repositories a `tierd repair-repo` run moved this developer's stored spend into |
  | `push_outcome_commits` | the SHA, repository, commit time and GitHub push time (`push_order`, #938) of each direct commit push capture folded into an outcome (#849); erasure removes only this developer's entries: a push outcome that also holds another developer's commit is kept and re-owned to that developer |
  | `push_outcome_audit` | each time a push outcome in this developer's name was superseded by a merged PR or re-owned (#849); erasure also blanks this developer's commit SHAs in other developers' rows |
  | `developer_alias` | the developer's alias→canonical mappings |
  | `canonical_id_history` | every link from one of the developer's ids to a canonical id, and every link from another alias to one of the developer's ids (#975) |

  **Watcher tail-state (`watcher_checkpoint`) is erased by a tombstone, not
  always deleted (#919).** The table has no `developer` column. The erasure
  finds the person's rows through the session ids of their stored
  `token_events` rows, read before those rows are deleted. For each row it
  finds: if the session file no longer exists, the row is deleted. Otherwise
  its `metadata` (the `cwd`, branch, session id, model, start time,
  counters and worktree path) is replaced by the marker `{"erased":true}`, and the row keeps its
  path key, inode, byte offset and head CRC. The watcher reads that row before
  every read of the file and skips the file unless a full read of its first
  bytes (as many as the row recorded) finds different content: appends
  included, across restarts, and when the file is replaced by a byte-identical
  copy with a new inode (a backup restore, `rsync`, an atomic save). A file at
  the same path whose first bytes differ is a new session and is captured
  normally.
  Seven limits, stated plainly:
  - **The path key is kept.** It names the OS username, the session's working
    directory and the session UUID (see **Watcher tail-state** above). Before the erasure
    the export returns it. After the erasure no stored row links it to the
    person, so the export no longer returns it; the path itself still says what
    it says.
  - **A tombstoned row lasts as long as its session file.** The watcher removes
    it once the file is deleted: at startup, or when it sees the removal. An
    operator can also delete the session file. No age limit removes it: TIER has
    no age-based pruning of per-developer data today (the per-developer
    retention planned in #826 is not built), and a retention limit would not
    remove it either, the planned 365-day ceiling included, because removing it
    while the file exists would let the watcher store the erased session again.
  - **Unjoinable rows are not found.** A row whose session left no stored
    `token_events` row cannot be linked to a person, so neither the erasure nor
    the export can find it. For example, the watcher can record tail-state for
    a session whose working directory is outside every watched repository, and
    such a session stores no events. These rows hold the same `cwd`, branch and
    session id.
    Nothing ages them out today. They go when their session file is deleted.
    ⚠️ Do not delete such a row while its session file exists: the watcher
    then reads that file again from the start and stores whatever it can
    attribute. To retire one while its file exists, tombstone it instead: set
    its `metadata` to `{"erased":true}`. Erasures made before this release did
    not touch this table at all, so each one left that person's rows here as
    ordinary rows that nothing now links to them; retire those the same way.
  - **Lines being read during the erasure are stored after it.** If the erasure
    commits while the watcher is already reading new lines from that person's
    file, those lines are stored after the erasure; calling the erasure
    endpoint again removes them. Where the file already had a row, the
    tombstone stops every later read. A file on its very first read has no row
    yet, so it gets no tombstone: the watcher saves an ordinary row for it and
    goes on capturing it. Calling the erasure endpoint again once any of that
    session's lines are stored removes them and tombstones the file. There is
    one gap: if the erasure commits after the watcher stored lines from a file
    with no row but before it saved the row, the erasure removes those lines
    and the watcher then saves an ordinary row after them. The erased lines do
    not return, but no stored line now joins that row, so neither the erasure
    nor the export can find it, and it keeps the session's `cwd`, branch and
    session id. It stays that way until the session writes a new line: the
    watcher stores that line, and calling the erasure endpoint again removes it
    and tombstones the file. If the session never writes again, the row goes
    when its session file is deleted. The same holds for a new session file at
    a path whose tombstone the watcher has just released.
  - **A message held back by worktree attribution can be stored after the
    erasure.** With worktree attribution on, the watcher holds back a message
    still being written until its session file has had no new line for 600
    seconds (see
    [Worktree attribution](#worktree-attribution-off-by-default)). If the
    person's session has **no stored event yet** when the erasure commits (a
    session that has just started, for example), its file's checkpoint row
    names the session id, but no stored `token_events` row carries that id, so
    the erasure cannot link the row to the person and does not tombstone it.
    When the hold ends, the watcher stores the message under the person, as an
    ordinary row, and goes on capturing the file. A session that already has a
    stored event is found and tombstoned as usual. If worktree attribution is on
    for the server's watcher, call the erasure endpoint again after the
    person's session files have had no new line for more than 600 seconds, and
    check the person's export: that call removes what was stored in between and
    tombstones the file.
  - **A moved or renamed session file is not covered.** Once the old path is
    gone the watcher deletes its tombstone, and it reads the file at its new
    path from the start (a move within the watched projects directory is seen
    at once), storing the erased session again. Calling the erasure endpoint
    again after that removes those rows and tombstones the file at its new
    path.
  - **`tierd ship` is not covered.** The tombstone protects only the watcher on
    the server host. `tierd ship` re-posts every event in its `--since` window
    (90 days by default) on each run, and the server de-duplicates by
    idempotency key. After an erasure there is nothing left to de-duplicate
    against, so the next ship run stores those events again for as long as the
    lines are still in the JSONL files on the shipping machine. Stop running
    `tierd ship` for that person before erasing.

  **Sealed reporting periods keep the person's contribution (#913, #914).** An
  erase removes everything attributable to the person, but a sealed k-anonymous
  aggregate keeps their contribution — including an unserved pre-fold total for
  a label they alone held in that period — because removing it would let a
  reader recover their figures by subtraction. A sealed period stores its served
  report, per-label pre-fold totals, and for each label and measure the people
  counted, each as a keyed hash of their canonical id (HMAC-SHA256 under a random
  per-install secret). The erasure deletes none of these rows and changes no
  report or total. It replaces with a fresh random tombstone each keyed hash
  computed from an id the person has at erase time (their canonical id and
  every alias pointing to it), and, in each period, each keyed hash computed
  from a canonical id that one of those ids was linked to when that period was
  sealed (`canonical_id_history`, #975; a period counts when it was sealed
  after the alias edit that made the link and before the edit that ended it,
  in the order the database committed them, and a link with no start time
  counts from the beginning). For example, if `alice-gh` pointed at `alice` when July was
  sealed and was later re-pointed at `alice.smith`, erasing `alice.smith`
  tombstones July's key for `alice`; an alias created by mistake and removed
  before any period was sealed tombstones nothing. One tombstone is used per
  period, reused under every label and measure the hash was held under, so no
  stored key can be recomputed from those ids afterwards. The erasure's
  `sealed_person` count (which `total_deleted` includes) is the rows this
  erasure tombstoned. What remains:
  - each replaced row keeps a `tombstoned` flag, so a label the person alone
    held in a period still shows that its only member was later erased;
  - ⚠️ **the erasure deletes stored rows only under the ids the person has at
    erase time, never under a retired canonical id.** A retired id's own rows
    (for example events recorded under `alice` itself) are not deleted, because
    that id may now belong to someone else. Its keys in periods sealed while the
    person's alias was linked to it are still tombstoned, and that changes no
    figure.
  - ⚠️ **links are followed only from the ids the person has at erase time.**
    A period attributed through an alias that has since been deleted, or
    re-pointed at someone else, keeps its key live (for example: `alice-gh`
    pointed at `alice` when July was sealed and was later deleted; erasing
    `alice.smith` does not reach July's key for `alice`), because following
    links that point at a retired canonical id could tombstone another
    person's keys. In that case **also erase the retired id by name**, after
    checking it is not now another person's id.

  The companion `repo_repair_row_audit` ledger is **not** listed because it has
  no `developer` column to erase by: only a repair id, a row id, and the
  before/after repository slug (`old_repo`, `new_repo`). It deliberately does not
  copy the resolving `session_id`, precisely so that an audit record designed to
  outlive the row it describes cannot carry a session id past an erasure.
  ⚠️ **Its repository slugs DO survive an erasure — a residual, stated plainly.**
  A slug can identify a person (a personal repository such as
  `alice/side-project` names its owner; see **Repository** above). They are kept
  because this ledger is the per-row before-image a `tierd repair-repo` run is
  reversed from by hand (there is no `--undo`), and like the other audit ledgers
  it is built to outlive the rows it describes. TIER has no command, endpoint or
  retention job that removes these rows today; retention of the audit ledgers is
  deferred (#141).

  **All five audit ledgers** — `reprice_audit`, `reprice_row_audit`,
  `repo_repair_audit`, `repo_repair_row_audit`, `cost_correction_audit` — are
  **append-only by construction: no code path mutates them; the tables
  themselves are not protected against direct database access.** One of them
  carries more than that, and it is narrow: `cost_correction_audit` (the
  money-rewrite ledger) refuses `UPDATE` at the schema level, so no code path
  can **silently** rewrite a recorded correction — a mutation would have to
  drop the trigger first. `DELETE` is deliberately **not** refused on any of them — erasure
  (below) and future retention/GC are lawful deletes, and no database
  constraint can tell a lawful delete from tampering. None of this is
  tamper-proofing against someone holding the database file.

  The `cost_correction_audit` ledger (#346, the sanctioned `POST /costs`
  cost-correction override) follows the SAME pattern and is **also not
  listed**: it holds no `developer` column, and — after an earlier draft got
  this backwards — deliberately does **not** copy the correction's
  `idempotency_key` either, precisely because that field is often
  client-chosen and could embed personal data (an email, a ticket
  reference), and this ledger is designed to outlive the row it describes.
  It carries only a server-generated `token_event_id` (which simply stops
  resolving once the row it names is erased or pruned — correct, not a bug)
  plus `old_cost_micro`/`new_cost_micro` and the operator-supplied `actor`/
  `reason` that explain the correction. `actor`/`reason` name the OPERATOR
  who made the correction, not the data subject whose spend was corrected —
  the same third-party-identifier class as the `webhook_payloads` residual
  below — so they are not erased by this endpoint. Operators must not put a
  data subject's own name or email in `reason`; nothing technical enforces
  that today.

  The `price_table_registry` table (#714 — the record binding each price-table
  `version:` to the content identity of the table served under it) is **also not
  listed, and holds no personal data**: a version integer, two content hashes, an
  effective date, a model count, a source, a `tool_version`, a `first_seen`
  timestamp, and — once retired — `forgotten_at` plus a `forgotten_by` that names
  the OPERATOR who retired it (self-asserted, and a third party to any data
  subject whose spend was measured — the same class as `cost_correction_audit`'s
  `actor`). The companion `price_forget_audit` ledger carries the same fields and
  is likewise not personal data about a data subject. It describes a
  **price table, not a person**, and it carries no `developer` column to erase by.
  ⚠️ One honest caveat, stated rather than glossed: its `source` is a filesystem
  path, and on a workstation install that path can incidentally contain an OS
  username (`/Users/alice/prices.yaml`). That is operator-environment provenance —
  the same third-party/operator class as `tool_version` and the `webhook_payloads`
  residual below, not personal data *about* the data subject whose spend was
  measured. It is never served over HTTP.
  ⚠️ **`tierd prices forget-version --commit` does NOT remove it.** It *retires*
  the identity — the row is kept and stamped, and a `price_forget_audit` entry is
  written — deliberately, so the record of what a price version meant survives its
  own retirement. Treat that as a **retention** decision: neither command erases
  this row, and the same override path is in any case written to stderr at startup
  and into `tierd score-log --json`, both far likelier to leave the machine than a
  database row. There is no erasure path for it today; if a deployment ever needs
  one, it belongs alongside the other audit-ledger retention questions, not as a
  side effect of the escape hatch.

  ⚠️ **`actor` is a self-asserted claim, not a verified identity — the audit
  trail records who the caller says they are.** It is unvalidated free text
  the caller chooses (length-capped only), and nothing checks it against the
  credential that made the request. Nothing can today: `POST /api/v1/costs` is
  gated — when a token is configured at all — by a single global write token
  with no subject, so there is no
  principal for the server to record instead. Treat every `actor` value as a
  claim to be corroborated, not as an attribution. Binding it to a verified
  principal requires the identity layer tracked as #65.

  The same holds for the corrected row's `developer`: nothing binds the
  authenticating principal to it either, which is the half that matters for
  data accuracy (Art. 5(1)(d)). What *is* bounded is **reach** —
  `POST /api/v1/costs` forces `source="api"` and the correction path compares
  it, so an override can only ever touch a manually imported row.
  Automatically captured spend cannot be corrected through this endpoint at
  all.

  The erasure endpoint returns per-table affected-row counts under the historical
  JSON names `deleted` and `total_deleted`, not physical row deletions. The
  correction counts are rows scrubbed: `correction_declarations` includes both
  the subject's declarations subsequently deleted and shared declarations retained
  with text cleared; `corrective_relations` and `correction_audit` count retained
  rows with identifiers and shared text cleared. `total_deleted` sums these
  affected-row counts, including retained rows. The
  `watcher_checkpoint` count (and so `total_deleted`) is rows tombstoned plus
  rows deleted. It is
  **idempotent** (a second call, or a call for an unknown developer, deletes
  nothing and returns `404`).

- **Access (DSAR)** — `GET /api/v1/developer/{id}/export` returns every row
  stored under the same resolved identifier set, plus the person's watcher
  tail-state rows (path key and `metadata` included), found by the same
  session-id join the erasure uses, as JSON — the portable artifact you hand to
  the data subject. It cannot return the unjoinable tail-state rows described
  under **Erasure** above, because nothing stored links them to the person.
  ⚠️ It does not list the person's sealed person keys either (which sealed
  periods, labels and measures counted them), although TIER could compute them.
  That is a known residual; whether the export should list them is not yet
  decided. The same holds for the person's `canonical_id_history` rows (#975),
  which the erasure deletes but the export does not return; that question is
  also still open.

**Known residual — `webhook_payloads`.** The structured erasure above does **not**
rewrite the raw GitHub webhook bodies retained in `webhook_payloads` (only present
when the webhook path is enabled). As described in *What IS stored* above, those
raw bodies may embed a contributor's name or email address. Surgically editing a
gzipped raw audit body to redact one identifier is out of scope for the erasure
endpoint; the **mitigation is the retention bound** — every such row is pruned at
startup and then every 24 hours while the server runs, by a 90-day age cap and a
50,000-row cap (`PruneWebhookPayloads`),
so any identifier embedded there ages out within ~90 days. An operator who must
guarantee immediate removal of that residual must delete the affected
`webhook_payloads` rows directly. Running TIER without the webhook path stops new
payloads from being stored, but rows already stored stay until the retention bound
removes them or someone deletes them. The bound still runs with the webhook path
off: each time tierd opens the database, and every 24 hours while `tierd serve`
runs (`cmd/tierd/webhook_prune.go`).
`org_actual_spend` is org-level (no developer column) and holds no personal data,
so it is correctly outside the erasure/export scope.

**Deleted bytes are overwritten at the next checkpoint.** TIER opens its database
with SQLite's `secure_delete` on, so a prune or an erasure zeroes the deleted rows
once SQLite next checkpoints its write-ahead log into the database file
(automatically after about 1,000 page writes, and at shutdown) instead of leaving
them readable in free pages (#862). Until then the old page can remain readable in
the database file or the `-wal` file.

The `-wal` file also keeps its own copy of recently written rows after that
checkpoint. SQLite trims the file back to 4 MiB, but that removes only what lies
beyond the first 4 MiB; a row inside them stays readable until SQLite writes over
it. The `-wal` file is removed only when the last connection to the database closes
cleanly. If TIER crashes or is killed, or another program such as `tierd backup`
still has the database open when TIER stops, the file stays on disk until the
database is next opened and closed cleanly.

**This covers deletes made by this version and later.** `secure_delete` zeroes a
page when a delete frees it; it does not go back over pages that were already free.
Rows pruned or erased by an earlier TIER version therefore stay readable in the
database file after you upgrade, until SQLite reuses those pages. To clear them
now, replace the database with a compacted copy:

1. Stop `tierd serve`.
2. As the user that runs TIER, run
   `tierd backup --db <path> --out <new-path>`, where `<path>` is the `--db` you
   give `tierd serve` (default `~/.tier/tier.db`) and `<new-path>` does not exist
   yet. `tierd backup` writes the copy with `VACUUM INTO`, which rebuilds the file
   from its live rows only, so the copy has no free pages and no deleted rows.
3. Move the old database file, and its `-wal` and `-shm` files if they exist, out
   of the directory. Rename the copy to the old database's name.
4. Start `tierd serve` and confirm your data is there. Then delete the old files
   you moved aside: they still hold the deleted rows.

## Where it goes

Nowhere by default. The database is a local file (SQLite). The only outbound
network traffic TIER makes is:

- the **reverse proxies** (`/anthropic/`, `/openai/`, `/gemini/`) forwarding your
  API calls to their upstream: by default `api.anthropic.com`, `api.openai.com`
  and `generativelanguage.googleapis.com`, or whatever `--anthropic-target`,
  `--openai-target` (any OpenAI-compatible host) and `--gemini-target` name. This
  happens only for traffic you route through them;
- the **Anthropic Admin API** and **OpenAI Usage API pollers**, which fetch your
  organization's usage and cost reports from `api.anthropic.com` and
  `api.openai.com` — only when the `collectors.anthropic_admin` or
  `collectors.openai_usage` config block is present;
- the **Copilot billing poller**, which calls `api.github.com` by default for
  organisation AI-credit billing usage and, in GitHub App mode, the installation
  token exchange. It sends the organisation, requested day and, for per-user
  requests, a lowercased query identifier, with a bearer credential. Query
  identifiers come from every rostered developer id and every alias key whose
  canonical target is rostered, if the text matches
  `^[A-Za-z0-9][A-Za-z0-9-]{0,38}$` with no trailing or doubled hyphen; the reserved
  unattributed developer is excluded. These identifiers can be local OS usernames
  or configured developer ids, not just GitHub logins. An alias key that is a
  local username but happens to be a different org member's GitHub login is
  queried as that login, so its spend is attributed to the alias's developer;
  keep alias keys that are not GitHub logins unambiguous. App mode
  sends an App-signed JWT and installation id to obtain the bearer token; the
  private key stays local. It runs only with `collectors.copilot_billing`
  configured (or a configured doctor probe);
- the **Cursor Admin poller**, which calls `api.cursor.com` by default for team
  usage events. It sends the team Admin API key as HTTP Basic authentication
  with an empty password, the day's start/end timestamps and pagination fields.
  It sends no email or conversation id in the request. It runs only with
  `collectors.cursor_admin` configured (or a configured doctor probe). Both new
  pollers allow an explicit `api_base_url` override; outbound requests then go
  to that configured origin;
- `tierd backfill`, which reads a repository's merged pull requests from the
  GitHub REST API (`api.github.com`, or the `--github-api-url` you pass) — only
  when you run it;
- `tierd hierarchy import`, which sends the rows of your team CSV (`developer`,
  `team`, `division`, `org`) to a central `tierd` you operate, as
  `POST /api/v1/org_hierarchy` to the `--server` URL — only when you run it
  without `--dry-run`;
- `tierd doctor --server`, which sends `GET /api/v1/scores` to the central
  `tierd` you name, to check that it answers, accepts your token, and agrees on
  the clock and the price table, and then `GET /api/v1/report_manifest`, to
  check whether sealing is stalled — only when you pass `--server`;
- `tierd seal --status --server`, which sends one
  `GET /api/v1/report_manifest` to the `tierd` you name, to read why sealing is
  stalled — only when you pass `--server`;
- `tierd issues --server`, which sends one `GET /api/v1/issues` to the `tierd`
  you name — only when you run it; and
- `tierd ship` forwarding **captured usage events** (the fields listed above,
  never content) to a central `tierd` you operate — from Claude Code sessions by
  default, from Codex rollout logs when `--codex-rollout` is passed, from the
  Opencode session store when `--opencode` is passed, and from Muse Code session
  logs when `--muse` is passed. All four produce the same event shape; none
  forwards a `cwd`, a Muse `workspace_root` or any content. With
  `--worktree-attribution` on, Claude Code events also carry their
  `attribution_rule` label and may carry the `unattributed:foreign-repo` issue
  label; no tool-call path is forwarded.

TIER does not call home, and it does not transmit your data to the project
maintainers or any third party.

## Legal deployment guidance

This document bounds what TIER stores as *content*. It does not resolve the
*labor-law* question: per-developer cost attribution is personal data about a
named individual, and in TIER's EU target markets (DE Betriebsrat
co-determination, FR/NL works councils, GDPR Art. 22 profiling) measuring
named individuals is co-determined or restricted. For deployment
guidance — works-council prerequisites, DPIA pointers, and a team-only vs
per-developer decision table by jurisdiction — see
[legal-and-privacy.md](legal-and-privacy.md). (Positioning guidance, not legal
advice.)

## Corrective work evidence

`correction_declarations`, `corrective_relations`, and `correction_audit` are
personal data covered by access/export and erasure. Each endpoint includes
nullable declaration and outcome ids, developer login, canonical repository and
PR number (`repo#PR`), merge SHA, and occurrence timestamps. Bounded supporting text,
disposition and appeal reasons, audit reason text and reviewer rationale are
subject-bearing content, bounded to 4,096 bytes per text field. Audit actor and
role are self-asserted; they do not establish authenticated identity.

The admin (`api-token`) scope opens `GET /api/v1/corrections/{kind}` for
`relations`, `assessments`, `allocations` and `audit`, as keyset-paginated JSON or
CSV. It includes all stored evidence fields described here, including free text,
actor/reviewer identities and exact allocation values. Read tokens cannot open
this route; it is absent from read-only demo mode and refused in team/division
aggregation modes.

Data-subject access exports all stored columns for either endpoint, including
shared declaration text and the counterparty's identifiers on shared relations.
For a reviewer who is neither endpoint, access returns only audit rows where
they are the actor and assessments where they are the reviewer; it does not
return other developers' allocations, relations or declarations.
Canonical repo/PR matching includes excluded outcomes under either
their original developer or trusted author, even when no outcome row remains.
Erasure resolves aliases, scrubs the subject's copied identifiers and
indirect declaration links, and clears shared text and audit actors before deleting
the subject's declarations and outcomes, including their declaration stats and
recorded timestamps. Relations and audit rows remain without the erased endpoint;
surviving evidence is retained. Evidence capture alone does not enable scoring.
When the operator enables Event 3 scoring, resolved allocations affect live
points as described below. Public GitHub can still link the surviving endpoint
to the erased subject through its PR history; this residual linkage is accepted by design.
Erasing a reviewer who is neither endpoint clears only matching actor/reviewer
identity columns. It preserves roles, reasons, supporting/disposition text and
reviewed sizing, and never freezes or reallocates the parties' allocations.
Schema version 8 prevents older erasers leaving this data behind.
Canonical declaration replays preserve the first evidence. A replay with an edited
declaration state or parent set returns the existing declaration id and the distinct
`unchanged-ignored` status (`ErrCorrectionDeclarationIgnored`) to the store caller;
it leaves declarations, relations, and audit rows unchanged. An identical state
and parent set is a successful no-op, including reordered parents.
The identifier-free API diff-stat cutover marker remains: pre-upgrade all-zero
API evidence is conservatively unavailable, including genuine zeros recorded by
the pointer-aware version-7 API writer, because the marker cannot distinguish
those from older omitted stats stored as zeros.

Event 3 also stores `correction_assessments` and `correction_allocations` as
endpoint-linked personal evidence. Assessments include reviewed original/own and
before/after cumulative sizes, comparison commit identities, bounded scope and
rationale, reviewer identity and review time. Allocations retain raw size, own
quality, share, final allocated points and marginal growth as distinct exact
rational values, plus pending/provisional/disputed and ranking eligibility state.
Export includes only rows where the subject's outcome (including aliases) is an
endpoint or participant, the subject is the reviewer, or the relation is a direct
subject relation. Export does not traverse the connected chain. Erasure deletes
the subject's own assessment and allocation rows, scrubs comparison identities,
scope, rationale, reviewer and review time from direct subject evidence, and
freezes every connected surviving allocation after severing its relation and
parent links. Reviews of unrelated survivors remain intact. Only survivors'
numeric allocations remain unchanged; they cannot be upgraded or reallocated
after any party is erased. Counts for these tables include rows deleted,
scrubbed or frozen. These rows do not enable scoring: the stored policy defaults off. Historical sealed bodies and
digests are preserved; the reserved policy/rubric columns remain NULL for old seals.

See [Enable Event 3 scoring](technical-reference.md#enable-event-3-scoring) for configuration, rollback and startup behavior.

### Enabled evidence and disputed state

Enabling scoring uses the stored reviewed assessments and allocations: reviewed
cumulative growth is separate from individual allocated share. A pure repair can
receive points transferred from the original without adding growth. Eligible
unconfirmed same-author pairs use a provisional churn/file-count approximation;
unconfirmed cross-author declarations cannot debit the other author. These
figures are evidence with stated limits, not independently verified scope.

Actor and role are self-asserted local CLI input, not authentication. A
single-operator installation confirms its own pairs. Requiring an actor label
different from the declarer is advisory; it does not prove a separate reviewer.
A dispute or withdrawal after confirmation keeps the previous treatment until
operator settlement. Disputed state remains visible in developer/operator
views, authorized exports, the corrective digest and new seals; team/division
views return `not_shown` for the diagnostics. This suppression does not conceal
the underlying rows from an operator who can read the database. Existing sealed
reports and digests never change after enablement, disputes, settlement, erasure,
or late evidence.

See [Event 3 coverage](technical-reference.md#event-3-coverage) for recorded declarations and limits.
