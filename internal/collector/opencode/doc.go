// Package opencode captures Opencode spend from the SQLite session store the
// Opencode client writes locally to ~/.local/share/opencode/opencode.db (#719).
//
// It is the third LOCAL per-call collector, alongside the Claude Code JSONL
// watcher and internal/collector/codexrollout: same provenance (a per-message log
// written by the client on the developer's own machine), same fidelity
// (realtime), same cwd-based repo scoping. The only structural difference is the
// container — one JSON blob per row in a `message` table instead of one JSON
// object per line in a file.
//
// Token accounting is defined by internal/tokenacct.Opencode. Read its
// opposite-of-Codex warning before changing the mapping or validation rule.
// A rule violation skips one row and is logged at WARN with a counter. Rows
// are independent per-message facts, so a bad row costs exactly itself;
// Codex instead fails the whole file because differencing cumulative snapshots
// lets one bad snapshot poison its neighbours.
//
// # Never import the client's cost
//
// Every Opencode row carries `cost: 0`, on a subscription route where the client
// computes nothing. This package therefore does not even DECLARE a cost field on
// its decode struct — the field is structurally unreachable, not merely unused —
// and prices every event through store.ComputeCostHost like every other
// producer. TestNeverDecodesTheClientsCost pins the absence.
//
// # Completed messages only
//
// A message row is written when the turn starts and UPDATED as it streams, so an
// in-flight row carries PARTIAL token counts. store.insertTokenEventSQL MAXes the
// token counters on conflict but deliberately never re-MAXes cost_micro ("re-MAXing
// cost_micro would silently reprice history", #233) — so ingesting a partial row
// freezes that message's cost at its partial value FOREVER while its token counts
// later ratchet up to the truth. The result is a row whose cost is not
// ComputeCostHost(its own tokens), and no counter anywhere would notice. Rows with
// no `time.completed` are therefore skipped and re-read on a later pass, when the
// UPDATE that completes them has bumped `time_updated` past the watermark.
//
// # Read-only, and never immutable
//
// The DB is opened `file:…?mode=ro`. It is NEVER opened with `immutable=1`:
// Opencode is a live process holding an active WAL, and `immutable=1` tells SQLite
// to ignore the WAL and any locking protocol — which does not fail, it returns
// stale or torn data with full confidence. Read transactions are kept short: one
// query per BATCH (a backfill issues ceil(rows/maxRowsPerPass) of them), each
// drained and closed before any ingest, because a long-lived reader pins
// Opencode's WAL and bloats THEIR file.
//
// ⚠️ `mode=ro` on a WAL database still requires WRITE access to the `-shm` file
// or its directory — that is SQLite's WAL protocol, not a choice this code makes.
// Fine for the same-user laptop case this is built for; a different-uid or
// read-only-mount deployment gets an open error naming a permission problem
// rather than naming this, which is why it is written down.
package opencode
