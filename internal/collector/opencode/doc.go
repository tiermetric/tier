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
// # 🔴 READ THIS BEFORE COPYING ANYTHING FROM codexrollout
//
// The two sources report reasoning tokens under OPPOSITE conventions, and an
// ingester written by analogy is not slightly wrong, it is wrong by most of the
// output-side bill.
//
//	Codex     reasoning ⊆ output           checkContainment asserts it; nothing adds it.
//	Opencode  reasoning BESIDE output      total = input + output + reasoning + cache.read
//
// (The check the code actually runs carries a fifth term, `+ cache.write`. It is
// UNMEASURED — cache.write is 0 on every one of the 9,267 assistant rows in the
// maintainer's store — and checkAdditiveIdentity explains why it is included
// anyway rather than assumed away.)
//
// Measured over every GLM-5.3 assistant row in the maintainer's store
// (2026-07-27 → 2026-08-28, 3,943 completed rows):
//
//	rows  total == in+out+reasoning+cache_read   reasoning > output
//	3943            3943  (100%)                       3025  (77%)
//
// The consequences of getting it backwards, both measured on that corpus:
//
//   - Mapping OutputTok = output alone drops 4,415,226 of 5,203,917 output-side
//     tokens — 84.8% of the output bill.
//   - Reusing Codex's `reasoning <= output` containment check REJECTS 77% of the
//     corpus, because here reasoning routinely exceeds output several-fold.
//
// So this package maps OutputTok = output + reasoning (the Gemini
// `thoughtsTokenCount` convention TokenEvent already documents), and its
// containment check is the ADDITIVE identity, not a subset assertion.
//
// ⚠️ The pricing side of that convention is UNVERIFIED: Z.ai does not publish
// whether reasoning tokens bill at the output rate. What IS verified is the
// arithmetic — the tokens exist, they are not inside `output`, and dropping them
// would under-report. Folding them into Output is the convention this tree
// already uses for additive reasoning; if Z.ai ever publishes a separate
// reasoning rate, that is a price-table change, not a change here.
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
