package store

// RepairRepo (#493): the sanctioned, audited path to repair token_events rows
// that are stuck on the repoid.Unqualified sentinel because they were captured
// by a pre-#491 shipper.
//
// # WHY A SEPARATE MUTATOR IS THE ONLY WAY
//
// #491 fixed `tierd ship` dropping `repo` on the wire, but the fix is
// FORWARD-ONLY, and deliberately so. insertTokenEventSQL excludes `repo` from its
// ON CONFLICT ... DO UPDATE clause precisely so a repo-blind producer replaying a
// message can never downgrade a row that another producer already qualified. That
// exclusion is correct and stays. Its consequence is that the obvious operator
// response to #491 — upgrade, re-ship the same window — produces NO VISIBLE
// CHANGE: every event collides on idempotency_key, the conflict clause takes MAX
// on the token counts and touches nothing else, `repo` stays 'unqualified', and
// the server returns 201. Ingestion structurally cannot repair this history, so
// the repair needs its own explicit, audited, dry-run-by-default command — the
// same shape #294 established for cost with `tierd reprice`.
//
// # WHY NOT RE-INSERT
//
// A re-INSERT was rejected outright. It cannot work (the idempotency_key collides
// and the conflict clause ignores `repo` — that is the bug), and if it somehow
// did work it would be worse than not fixing it: a fresh INSERT would re-run the
// pricing path and could rewrite cost_micro, which #233 holds IMMUTABLE on the
// replay path, silently ratcheting historical spend as a side effect of an
// identity fix. This is a targeted UPDATE of exactly one column on exactly the
// rows the operator named.
//
// # HOW THE SLUG IS RESOLVED — AND WHY session_id IS THE KEY
//
// The repair NEVER guesses. It takes an explicit operator-supplied
// session_id -> "owner/repo" mapping and every value goes through
// repoid.Canonical, so the repair cannot invent an identity the collector would
// not have produced (the same canonicalization `tierd ship --repo-slug` applies).
//
// session_id is the only honest key available on a stored row. It is the
// collector's own per-session grouping identity (#238), and one Claude Code
// session belongs to exactly one working tree — which is exactly how the
// collector derives the slug in the first place (the session's cwd -> that
// checkout's remote.origin.url -> repoid.FromRemoteURL). developer/source/model
// are all many-to-many with repositories and would force a guess. Rows with NO
// session at all (proxy and poller rows, which structurally cannot know a
// repository) are therefore UNRESOLVABLE by construction: they are left
// 'unqualified' and REPORTED, never guessed.
//
// # SAFE BY DEFAULT
//
//   - Dry run unless Commit. A dry run computes the identical result a commit
//     would apply and writes NOTHING.
//   - Commit applies every row UPDATE, every per-row before-image, and every
//     aggregate audit row in a SINGLE transaction — all or nothing.
//   - Developer is REQUIRED, so a repair can never sweep the whole table (the
//     --from-version analogue). The mapping is the second bound: a row whose
//     session is not named in it is never touched.
//   - A row already carrying a REAL repo is never modified, even when the mapping
//     disagrees with it. The disagreement is counted and reported instead — it
//     almost always means the mapping is wrong, and silently skipping it would
//     hide that.
//   - Re-running is a no-op: the second run finds no 'unqualified' rows left for
//     the mapped sessions, so it changes nothing and writes no audit rows.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
)

// RepairRepoOptions parameterizes a RepairRepo run (#493).
type RepairRepoOptions struct {
	// Developer is REQUIRED and bounds the whole operation: only token_events
	// rows owned by this exact developer are ever examined or changed. It is the
	// structural analogue of reprice's --from-version — the guard that makes a
	// whole-table mutation impossible by accident.
	//
	// It is not merely paranoia. The mapping is derived from ONE machine's
	// ~/.claude session history, so it describes one person's working trees.
	// Applying it unscoped would let a session id that appears under a second
	// developer (a mis-set --developer during a backfill re-ships the same
	// sessions under another name) silently re-attribute that person's spend to
	// this operator's repository — a wrong answer that no later run could detect,
	// because the repaired row is indistinguishable from one that was always
	// qualified.
	Developer string

	// SlugBySession maps a token_events.session_id to the canonical repository
	// slug its spend belongs to. Values are canonicalized and validated through
	// repoid.Canonical here (not merely at the CLI boundary): this is the
	// mutation boundary, and a caller that hands over a non-canonical or
	// sentinel-forging slug must fail loudly rather than write a join key nothing
	// else in the pipeline can produce.
	//
	// A session absent from the map is NOT an instruction to guess — its rows stay
	// 'unqualified' and are reported.
	SlugBySession map[string]string

	// Commit gates the mutation. False (the default) is a DRY RUN: the result is
	// computed exactly as a commit would compute it but NOTHING is written — no
	// UPDATE, no before-image, no audit row.
	Commit bool

	// ToolVersion stamps the aggregate audit rows with the binary identity that
	// ran the repair (provenance: "which build rewrote this attribution"). Empty
	// is allowed (an unstamped dev build) and stored verbatim.
	ToolVersion string

	// unprepared forces the commit loop onto the pre-hoist path: re-sending the
	// SQL TEXT of both statements on every row instead of driving two prepared
	// statements. It exists so BenchmarkRepairRepoCommitUnprepared can measure
	// the control arm for the speedup the prepare block below CLAIMS — a claim
	// with no obtainable control arm is a number nobody can check, and the
	// previous one had drifted 5x from what the named harness actually printed.
	//
	// UNEXPORTED ON PURPOSE, and that is the whole safety argument: only code
	// inside package store can set it, so no caller, config file, or JSON body
	// can reach it and no production path constructs it as true. It is not a
	// tuning knob and must never become one — the prepared path is strictly
	// faster and has identical semantics (TestRepairRepoUnpreparedPathIsIdentical
	// pins the "identical" half, so the control arm cannot silently measure a
	// cheaper amount of work than the real loop does).
	unprepared bool
}

// RepairRepoDelta is the per-target-repository breakdown of a repair: how many
// unqualified rows resolved to this slug, how much spend they carry, and how many
// distinct sessions contributed. SessionCount is what tells an operator whether a
// suspiciously large row count came from one runaway session or from a genuinely
// broad history.
type RepairRepoDelta struct {
	// FromRepo is the value these rows actually HELD before the repair, read from
	// the scan rather than assumed. It is repoid.Unqualified for every row the
	// insert path could produce (normalizeRepo collapses "" to the sentinel), but
	// the candidate test is !repoid.IsReal, which ALSO admits "" — so hardcoding
	// the sentinel into the audit ledger would let it assert a before-value the
	// per-row before-images could contradict. An audit that can lie, even about an
	// unreachable case, is not an audit.
	FromRepo     string
	Repo         string
	RowCount     int64
	CostMicroSum int64
	SessionCount int
}

// RepairRepoUnresolved is one bucket of rows the mapping could NOT resolve —
// reported, never guessed (the #493 acceptance bullet). SessionID is the row's
// session, or "" for the structurally unresolvable rows that carry no session at
// all (proxy/poller). The distinction matters operationally: a named session
// means "you forgot a mapping entry", while "" means "no mapping could ever fix
// these rows".
type RepairRepoUnresolved struct {
	SessionID    string
	RowCount     int64
	CostMicroSum int64
}

// RepairRepoConflict is a row set the repair REFUSED to touch: the row already
// carries a real repo, and the supplied mapping names a different one. This is
// the invariant the insert-path conflict clause protects, surfaced rather than
// silently swallowed — a disagreement here almost always means the mapping is
// wrong, and a silent skip would let the operator believe otherwise.
type RepairRepoConflict struct {
	SessionID  string
	StoredRepo string
	MappedRepo string
	RowCount   int64
	// CostMicroSum is the spend sitting on the disputed rows, so an operator can
	// judge whether a disagreement is worth chasing. A one-row disagreement and a
	// nine-thousand-dollar one deserve different amounts of attention, and a bare
	// row count cannot tell them apart.
	CostMicroSum int64
}

// RepairRepoResult reports what a repair did (Committed) or WOULD do (dry run).
// Every count is computed by the SAME scan in both modes, so a dry run's numbers
// are exactly what a subsequent Commit applies.
type RepairRepoResult struct {
	Developer string
	Committed bool
	// RepairID is the audit-ledger correlation token minted for a committed run
	// that actually changed rows. Empty on a dry run AND on a commit that found
	// nothing to change (no audit row was written, so there is nothing to
	// correlate) — non-empty IFF the ledgers gained rows.
	RepairID string

	// ScannedRowCount is every token_events row owned by Developer. Zero means the
	// developer name matched nothing — almost always a typo, and the caller must
	// not report that as a clean no-op.
	ScannedRowCount int64
	// AlreadyQualifiedRowCount is the subset carrying a REAL repo. Never touched.
	AlreadyQualifiedRowCount int64
	// UnqualifiedRowCount is the subset on the sentinel — the repair's candidate
	// set. It always equals ChangedRowCount + UnresolvedRowCount.
	UnqualifiedRowCount int64
	// ChangedRowCount is the subset the mapping resolved (and that a Commit
	// rewrites), with the spend those rows carry.
	ChangedRowCount     int64
	ChangedCostMicroSum int64
	// UnresolvedRowCount is the candidate subset the mapping could not resolve,
	// of which UnresolvedNoSessionRowCount carry no session at all and are
	// therefore unresolvable by ANY mapping.
	UnresolvedRowCount          int64
	UnresolvedNoSessionRowCount int64
	// ConflictRowCount is the number of already-qualified rows whose session maps
	// to a DIFFERENT slug. Reported, never modified.
	ConflictRowCount int64

	// MappedSessionCount is how many session -> slug entries this run was given,
	// counted AFTER normalization, and UnmatchedSessions (ascending) names the
	// subset that matched NO row owned by Developer — not "no unqualified row",
	// no row at all.
	//
	// 🔴 THIS IS THE COMMAND'S MOST LIKELY FAILURE MODE AND IT WAS INVISIBLE.
	// Everything else in this struct describes the DATABASE side of the repair.
	// An operator supplying 400 mapping entries from a stale export, with a
	// typo'd UUID, or with a UTF-8 BOM on line 1 (strings.TrimSpace does NOT
	// strip U+FEFF, so the first session id silently carries three extra bytes)
	// gets entries that resolve zero rows — and without this the report happily
	// said "0 rows repaired" with no hint that the MAPPING, not the history, was
	// the problem. Reporting it makes the BOM case self-diagnosing: one entry
	// unmatched, and it is always the first line.
	//
	// An unmatched entry is REPORTED, never an error: mapping a session that
	// belongs to a different developer, or one whose rows are already qualified
	// and out of the window, is a legitimate way to run this command.
	MappedSessionCount int
	UnmatchedSessions  []string

	// AliasIdentities is the sibling raw identifiers that resolve to the same
	// person as Developer through developer_alias, and AliasUnqualifiedRowCount is
	// how many unqualified rows they hold — rows THIS RUN DID NOT EXAMINE.
	//
	// 🔴 WHY THIS IS REPORTED RATHER THAN REPAIRED. token_events.developer stores
	// the RAW producer id (the OS username from a JSONL capture, the GitHub login
	// from the webhook); the two are unified only at READ time, by developer_alias
	// (#125). So one human's rows routinely sit under two names, and the exact
	// `WHERE developer = ?` scoping this repair uses — which RepairRepoOptions.
	// Developer argues for at length, and which stays — repairs one of them and
	// reports success. Measured: 7 rows for one person on one session, 3 under
	// "devlead" and 4 under "dl" with an alias between them; repairing
	// "devlead" fixed 3, left 4, and printed no UNRESOLVED and no WARNING. It
	// was indistinguishable from a complete repair.
	//
	// Widening the scope to the alias set is the WRONG fix: the mapping comes from
	// one machine's session history, and applying it to rows stored under another
	// identifier is precisely the cross-person re-attribution the Developer bound
	// exists to prevent. So the scope stays exact and the gap is NAMED, with the
	// remedy (re-run per stored identity) in the message.
	AliasIdentities          []string
	AliasUnqualifiedRowCount int64

	// ByRepo is the per-target-repo breakdown, ascending by slug. Only repos that
	// gained rows appear, so a genuine no-op yields none.
	ByRepo []RepairRepoDelta
	// Unresolved is the unresolvable buckets, ascending by SessionID (the ""
	// no-session bucket sorts first).
	//
	// 🔴 COMPLETE, NEVER TRUNCATED — this is a CONTRACT, not an accident. The
	// report sums CostMicroSum over these buckets to state a total, while it takes
	// the row counts from the scalar counters above. The two agree only because
	// this slice holds every bucket. Cap it store-side (maxReportedBuckets exists
	// in the CLI precisely because someone expects thousands) and the printed
	// "carrying N micro-USD in total" silently UNDERSTATES while the row counts
	// stay right — a number that is not true, printed beside numbers that are,
	// which is the exact defect the UNRESOLVED section was rewritten to remove.
	// Truncation belongs in the printer, where it is visible as "... and N more".
	Unresolved []RepairRepoUnresolved
	// Conflicts is the refused disagreements, ascending by (SessionID, StoredRepo).
	Conflicts []RepairRepoConflict
}

// repairRepoSelectSQL pulls every row owned by the developer, INCLUDING the
// already-qualified ones. Selecting only the unqualified rows would be cheaper
// but would make the mapping-disagrees-with-a-real-repo case INVISIBLE, and that
// case is the loudest signal that a mapping is wrong. COALESCE renders the NULL
// session_id of a proxy/poller row as "" — the same collapse ListTokenEvents
// performs, so "" here means "no session", which no mapping key can ever equal
// (empty keys are rejected in normalizeRepairMapping).
//
// COMPLEXITY: O(rows for this developer). EXPLAIN QUERY PLAN reports
// "SEARCH token_events USING INDEX idx_token_events_scores (developer=?)" plus
// "USE TEMP B-TREE FOR ORDER BY" — that index leads with developer so the filter
// is index-served, but it covers neither repo nor session_id (heap lookup per
// candidate) nor id order (the temp b-tree). Measured at 123k rows the ORDER BY
// costs ~6ms of ~89ms, and it is kept because it makes the before-image insert
// order deterministic. This is an offline maintenance command run against a
// quiesced database, so that is the right trade — a dedicated covering index for
// a one-shot repair would be permanent write cost for a transient read.
const repairRepoSelectSQL = `
SELECT id, COALESCE(session_id, ''), repo, cost_micro
FROM token_events
WHERE developer = ?
ORDER BY id`

// repairRepoUpdateSQL is the ONLY statement that rewrites token_events.repo.
//
// 🔴 THE `AND repo = ?` IS LOAD-BEARING, NOT DECORATION. It binds the exact
// before-image value recorded in repo_repair_row_audit, making the write a
// compare-and-swap: the row is rewritten only while it still holds precisely the
// value the ledger claims it held. Two things depend on it. First, the #493
// acceptance invariant — a row carrying a real repo can never be downgraded or
// re-pointed, which is the same invariant insertTokenEventSQL's conflict clause
// protects on the ingest path. Second, ledger truthfulness: a before-image that
// does not match what was actually overwritten is worse than no ledger at all,
// because an inverse-undo would restore a value that was never there. RepairRepo
// asserts RowsAffected == 1 and aborts the whole transaction otherwise, so a
// blocked write fails LOUD instead of being counted as a repair that never
// happened.
//
// Guard coverage: TestRepairRepoUpdateSQLRefusesRealRepo drives this statement
// directly against a qualified row — delete the `AND repo = ?` and it fails.
const repairRepoUpdateSQL = `UPDATE token_events SET repo = ? WHERE id = ? AND repo = ?`

// repairRepoRowAuditSQL writes one per-row before-image. Hoisted to a const from
// an inline literal so the commit loop can PREPARE it once instead of handing the
// driver the same text to re-parse on every row (see the prepare block in
// RepairRepo for the measurement).
//
// session_id is deliberately absent — see the repo_repair_row_audit schema and
// TestRepairRepoRowAuditStoresNoPersonalData. The ledger records what changed,
// not who was working.
const repairRepoRowAuditSQL = `
INSERT INTO repo_repair_row_audit
    (repair_id, token_event_id, old_repo, new_repo)
VALUES (?, ?, ?, ?)`

// repairRowExecer is the single-statement executor the commit loop drives, once
// per row. *sql.Stmt satisfies it as written — the interface exists only so the
// SAME loop can also be driven by the unprepared implementation below, which is
// what makes the prepared-statement speedup an obtainable measurement rather than
// a remembered one.
//
// This is a two-implementation interface with both implementations in this file,
// which is the only shape that justifies one here: the loop is the thing being
// measured, so a control arm that re-implemented the loop could drift from it and
// then report a ratio for code that no longer exists.
type repairRowExecer interface {
	ExecContext(ctx context.Context, args ...any) (sql.Result, error)
}

// repairTxExecer is the UNPREPARED implementation: it hands the driver the full
// SQL TEXT on every call, so the driver re-parses and re-plans it per row. That is
// exactly what the commit loop did before the prepared-statement hoist, and it is
// the control arm BenchmarkRepairRepoCommitUnprepared measures.
//
// It is reachable ONLY through RepairRepoOptions.unprepared, which is unexported.
type repairTxExecer struct {
	tx    *sql.Tx
	query string
}

func (e repairTxExecer) ExecContext(ctx context.Context, args ...any) (sql.Result, error) {
	return e.tx.ExecContext(ctx, e.query, args...)
}

// normalizeRepairMapping validates and canonicalizes an operator-supplied
// session -> slug mapping, returning a new map or a fail-loud error.
//
// Every value goes through repoid.Canonical, which lowercases, strips any ".git"
// suffix, requires at least two path segments, and REJECTS the reserved
// 'unqualified' sentinel. That is what guarantees the repair can only ever write
// an identity the collector itself could have produced — a hand-typed
// "Tiermetric/Tier" lands as "tiermetric/tier", byte-identical to what the
// webhook and the JSONL collector write, so the cost<->outcome join (#231) still
// meets.
//
// A bad entry is a hard error rather than a skipped line: an operator reaching
// for this command is fixing an attribution bug, and silently dropping the entry
// would leave exactly the wrong-repo attribution they came to fix — the same
// discipline parseRepoSlugPairs applies to `ship --repo-slug`.
//
// 🔴 EVERY OPERATOR-SUPPLIED VALUE IN THESE ERRORS GOES THROUGH logsafe.Str, and
// the rule is the same one aliasUnqualifiedRows states: an error message is not
// exempt from the sanitizer barrier just because it is an error. These three
// errors are Fprintf'd straight to stderr by the CLI and the session ids come out
// of an operator-supplied --map file (or a --map-file that a tool generated), so
// they are the same raw-sink path the #493 review filed as RED for session_id.
// `%q` alone escapes CR/LF at runtime, so this is not an exploitable gap — but
// the strip REMOVES what %q merely escapes, and an escaped newline is
// resurrected by any consumer that unquotes the field. For what CodeQL does and
// does not credit, read internal/logsafe's package doc and nothing else: it is
// the single source of truth, and this sentence used to carry its own (by then
// wrong) copy of the rule. More to the point, a rule stated in one function and
// contradicted two functions away is a rule the next author cannot apply.
// repoid.Unqualified stays on %q: it is a package constant, not input.
//
// Guard coverage, stated honestly because two of the three are reachable and one
// is not: TestNormalizeRepairMappingErrorsGoThroughTheLogsafeBarrier drives the
// empty-session-id and non-canonical-slug errors — revert either to %q and it
// fails. The FIXED-POINT error below is NOT covered and cannot be: it fires only
// if repoid.Canonical stops being idempotent, which repoid's own tests pin, so no
// input reaches it today. Its formatting is consistency with the other two, not a
// tested property; if the guard ever becomes reachable, add the third case.
func normalizeRepairMapping(m map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(m))
	for session, slug := range m {
		if session == "" {
			// An empty key could never match a row anyway (COALESCE renders a
			// NULL session as "" and those rows are unresolvable by definition),
			// so accepting it would silently do nothing. Refuse instead.
			return nil, fmt.Errorf("repair-repo: mapping has an empty session id (for slug %s); a session-less row cannot be resolved by any mapping", logsafe.Str(slug))
		}
		canon, ok := repoid.Canonical(slug)
		if !ok {
			return nil, fmt.Errorf("repair-repo: session %s maps to %s, which is not a canonical owner/repo slug (the reserved %q sentinel and single-segment names are rejected)", logsafe.Str(session), logsafe.Str(slug), repoid.Unqualified)
		}
		// 🔴 FIXED-POINT GUARD — defence in depth for the claim this function's
		// doc comment makes ("the repair cannot invent an identity the collector
		// would not have produced"). #493 review found that claim was FALSE:
		// repoid.Canonical was not idempotent, so "owner/repo.git/" canonicalized
		// to "owner/repo.git" — a join key nothing in the capture path can emit.
		// Cost repaired under it would never meet outcomes under "owner/repo",
		// and the failure is silent, permanent and NOT self-correcting: the row
		// then reads as already-qualified, so a corrective re-run is REFUSED by
		// the never-overwrite invariant, and there is no --undo.
		//
		// Canonical is now idempotent, which fixes the root cause. This check
		// stays because THIS is the mutation boundary: if that property ever
		// regresses, the repair must refuse to write rather than quietly mint an
		// unjoinable identity. It costs one call and it fails loud.
		if again, ok2 := repoid.Canonical(canon); !ok2 || again != canon {
			return nil, fmt.Errorf("repair-repo: session %s maps to %s, which canonicalizes to %s and then to %s — the value is not a fixed point, so it would write a repo identity the collector could never produce; supply the fully canonical slug", logsafe.Str(session), logsafe.Str(slug), logsafe.Str(canon), logsafe.Str(again))
		}
		out[session] = canon
	}
	return out, nil
}

// repairRepoAliasCountSQL counts the unqualified rows sitting under a set of
// sibling developer identifiers. The predicate mirrors repoid.IsReal exactly
// (negated): a row is a repair candidate when its repo is "" or the sentinel.
// Keeping the two definitions in step matters — if this SQL drifted, the NOTE the
// command prints would name a number the repair could not actually act on.
//
// `repo` is NOT NULL DEFAULT 'unqualified' (see schemaTables), so there is no
// NULL arm; the main scan proves it by scanning `repo` straight into a string.
const repairRepoAliasCountSQL = `
SELECT COUNT(1)
FROM token_events
WHERE developer IN (%s)
  AND (repo = '' OR repo = ?)`

// aliasUnqualifiedRows reports the raw identifiers that name the SAME person as
// developer but are not developer itself, plus how many unqualified rows they
// hold. Both are for REPORTING only — see RepairRepoResult.AliasIdentities for
// why widening the repair's scope to cover them would be a bug, not a fix.
//
// Returns (nil, 0, nil) for the overwhelmingly common case of an identifier with
// no aliases: developerIdentifierSet resolves an unmapped id to itself, the
// sibling set is empty, and no count query is issued at all.
//
// It runs inside the repair's own transaction, so the count is taken from the
// same snapshot as the scan — a probe from outside could name a number that never
// coexisted with the repair's view of the table.
//
// COMPLEXITY: one full read of developer_alias (a tiny operator-maintained table)
// plus, only when siblings exist, one COUNT per RUN — not per row. Verified
// index-served: EXPLAIN QUERY PLAN reports
// "SEARCH token_events USING INDEX idx_token_events_scores (developer=?)".
//
// tx is CONCRETE rather than an interface on purpose: this helper has exactly one
// caller and MUST run inside the repair's transaction, so naming *sql.Tx makes
// that a compile-time fact instead of a comment. An interface here would only
// advertise a flexibility that would be a bug to use.
//
// ⚠️ This used to add "developerIdentifierSet takes the package's rowQuerier
// because it is genuinely called with both a *DB and a *sql.Tx". #673 made that
// false: ExportDeveloper was the last production *sql.DB caller and now passes
// its read transaction, so all three call sites pass a *sql.Tx. The interface
// survives for the TESTS (see rowQuerier), not for a production caller. ⭐ The
// argument in this paragraph is unaffected — and #673 is the case study for it,
// since an unenclosed multi-read there tore a GDPR export.
func aliasUnqualifiedRows(ctx context.Context, tx *sql.Tx, developer string) (siblings []string, unqualified int64, err error) {
	_, ids, err := developerIdentifierSet(ctx, tx, developer)
	if err != nil {
		return nil, 0, fmt.Errorf("repair-repo: resolve developer aliases: %w", err)
	}
	for _, id := range ids {
		if id != developer {
			siblings = append(siblings, id)
		}
	}
	if len(siblings) == 0 {
		return nil, 0, nil
	}
	placeholders, args := inClause(siblings)
	args = append(args, repoid.Unqualified)
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(repairRepoAliasCountSQL, placeholders), args...,
	).Scan(&unqualified); err != nil {
		// The identifiers are sanitized even here. They are raw token_events.developer
		// values — length-bounded at ingest but never charset-validated — and this
		// error is Fprintf'd straight to stderr by the CLI, which is the same raw-sink
		// path the #493 review filed as RED for session_id. An error message is not
		// exempt from the barrier just because it is an error.
		safe := make([]string, 0, len(siblings))
		for _, id := range siblings {
			safe = append(safe, logsafe.Str(id))
		}
		return nil, 0, fmt.Errorf("repair-repo: count unqualified rows under alias identities %s: %w", strings.Join(safe, ", "), err)
	}
	return siblings, unqualified, nil
}

// RepairRepo rewrites token_events.repo from the repoid.Unqualified sentinel to
// the canonical slug an operator-supplied session -> slug mapping names, for one
// developer, and (on commit) writes both audit ledgers in the same transaction
// (#493). See the file-level comment for why this exists as a separate mutator
// and why a re-INSERT was rejected.
//
// SAFE BY DEFAULT: with opts.Commit false it is a DRY RUN — it performs the
// identical scan and classification a commit would and writes NOTHING.
//
// INVARIANTS, all four straight from the #493 acceptance list:
//
//  1. A DB whose rows are all 'unqualified' is repairable to real slugs, with one
//     before-image row per change.
//  2. Re-running is a no-op: after a commit no mapped session has an unqualified
//     row left, so the second run changes nothing and writes no audit.
//  3. A row already carrying a real repo is NEVER modified, even when the mapping
//     disagrees — it is counted into Conflicts and reported.
//  4. Rows the mapping cannot resolve stay 'unqualified' and are reported in
//     Unresolved, never guessed.
//
// COMPLEXITY: one pass over the developer's rows (O(n) time), buffering only the
// CHANGED rows (O(changed) memory) plus one accumulator per distinct repo /
// unresolved session / conflict. On commit it issues two statements per changed
// row inside one transaction.
//
// CONCURRENCY: safe for concurrent use in the sense that every other DB method
// is (database/sql pools connections). A COMMIT run takes the SQLite write lock
// up front (see the lock-acquisition comment below) and holds it for the whole
// transaction, so it WILL contend with a live `tierd serve` ingest path — run it
// against a quiesced database or in a maintenance window. A dry run stays
// genuinely read-only and takes no write lock.
func (d *DB) RepairRepo(ctx context.Context, opts RepairRepoOptions) (RepairRepoResult, error) {
	if opts.Developer == "" {
		return RepairRepoResult{}, fmt.Errorf("repair-repo: developer is required (refusing to repair every developer's rows at once)")
	}
	if len(opts.SlugBySession) == 0 {
		// An empty mapping would scan, resolve nothing, and report a clean
		// "0 changed" — indistinguishable from "your history is already fine".
		// Refusing keeps a forgotten --map from reading as success.
		return RepairRepoResult{}, fmt.Errorf("repair-repo: mapping is empty (nothing to resolve); supply at least one session -> owner/repo entry")
	}
	mapping, err := normalizeRepairMapping(opts.SlugBySession)
	if err != nil {
		return RepairRepoResult{}, err
	}

	res := RepairRepoResult{Developer: opts.Developer}

	// One transaction spans BOTH the read and (on commit) every write, so the dry
	// run reads a consistent snapshot and a committed run is all-or-nothing. The
	// deferred Rollback is a no-op after a successful Commit; on the dry-run path
	// it is what discards the (read-only) transaction.
	//
	// 🔴 A COMMIT MUST TAKE THE WRITE LOCK BEFORE IT READS, which is exactly what
	// beginImmediate does and what a plain BeginTx does NOT — including a BeginTx
	// passed sql.LevelSerializable, which modernc.org/sqlite ignores outright. The
	// commit path is read-then-write, the classic deferred-upgrade hazard under
	// WAL: if any other connection commits between our read snapshot and our first
	// write, SQLite fails the upgrade with SQLITE_BUSY_SNAPSHOT (517), which
	// busy_timeout does NOT retry — it is returned immediately. On the 123k-row
	// repair this issue exists for, one concurrent event insert would throw the
	// entire scan away with an opaque "database is locked", after all the work.
	// Fail at acquisition instead, where a failure costs nothing.
	//
	// A DRY RUN deliberately does NOT promote. It writes nothing, so it must not
	// contend with a live `tierd serve` — a diagnostic an operator cannot run
	// without taking the database down is a diagnostic nobody runs.
	var tx *sql.Tx
	if opts.Commit {
		tx, err = beginImmediate(ctx, d.db)
		// The contention hint is attached ONLY to the contention error. beginImmediate
		// also fails when BeginTx itself fails — a cancelled context (this command
		// installs a SIGINT handler), a closed handle — and telling an operator who
		// just pressed Ctrl-C that a `tierd serve` is ingesting sends them to check
		// the wrong thing. A diagnosis naming the wrong cause is worse than none.
		switch {
		case errors.Is(err, ErrWriteLockUnavailable):
			return RepairRepoResult{}, fmt.Errorf("repair-repo: %w (is a tierd serve ingesting into this database? run the repair against a quiesced database)", err)
		case err != nil:
			return RepairRepoResult{}, fmt.Errorf("repair-repo: %w", err)
		}
	} else {
		tx, err = d.db.BeginTx(ctx, nil)
		if err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: begin tx: %w", err)
		}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// SIBLING-IDENTITY PROBE — see aliasUnqualifiedRows. Runs INSIDE the same
	// transaction as the scan so the number it reports and the rows the repair
	// examines come from one snapshot; a probe from outside could name a count
	// that never coexisted with the repair's own view.
	res.AliasIdentities, res.AliasUnqualifiedRowCount, err = aliasUnqualifiedRows(ctx, tx, opts.Developer)
	if err != nil {
		return RepairRepoResult{}, err
	}

	// change is one row the commit will rewrite. oldRepo is carried forward from
	// the scan so both the before-image and the compare-and-swap guard use the
	// value actually observed, never a re-read of the row we are about to
	// overwrite.
	//
	// The session id is NOT carried here, and that is deliberate on two counts.
	// The per-repo session count is accumulated during the scan into
	// repoAcc.sessions (a set, so it de-duplicates); a per-change copy would be a
	// second structure over the same fact with nothing reading it. And the ledger
	// stores no session id at all — see the repo_repair_row_audit schema and
	// TestRepairRepoRowAuditStoresNoPersonalData — so buffering one for every
	// changed row would retain a personal identifier for the length of the
	// transaction with no consumer. At the 123k-row scale this command was
	// written for that is ~123k strings held for nothing.
	type change struct {
		id      int64
		oldRepo string
		newRepo string
	}
	// repoAcc accumulates one (from_repo -> to_repo) group. The session set lives
	// INSIDE it rather than in a parallel map keyed the same way: two maps on one
	// key is a reconciliation pass waiting to drift apart.
	type repoAcc struct {
		delta    RepairRepoDelta
		sessions map[string]struct{}
	}
	var changes []change
	byRepo := map[string]*repoAcc{}
	unresolved := map[string]*RepairRepoUnresolved{}
	conflicts := map[string]*RepairRepoConflict{}
	// matchedSessions records which MAPPING KEYS the scan actually saw on a row.
	// Deliberately marked for EVERY row the key matched — changed, conflicting, or
	// already-qualified-and-agreeing — because the question it answers is "did
	// this entry of yours correspond to anything at all", not "did it repair
	// something". An entry that matched an already-qualified row did its job; an
	// entry that matched nothing is a typo, a stale export, or a BOM.
	matchedSessions := make(map[string]struct{}, len(mapping))

	rows, err := tx.QueryContext(ctx, repairRepoSelectSQL, opts.Developer)
	if err != nil {
		return RepairRepoResult{}, fmt.Errorf("repair-repo: select candidates: %w", err)
	}
	for rows.Next() {
		var (
			id          int64
			session     string
			repo        string
			costMicro   int64
			mapped      string
			haveMapping bool
		)
		if err := rows.Scan(&id, &session, &repo, &costMicro); err != nil {
			_ = rows.Close()
			return RepairRepoResult{}, fmt.Errorf("repair-repo: scan candidate: %w", err)
		}
		res.ScannedRowCount++
		if session != "" {
			mapped, haveMapping = mapping[session]
			if haveMapping {
				matchedSessions[session] = struct{}{}
			}
		}

		// Already-qualified rows are the untouchable set. repoid.IsReal is the
		// single shared definition of "this names an actual repository" — the same
		// predicate the tolerant cost<->outcome join uses — so the repair's notion
		// of untouchable cannot drift from the rest of the pipeline.
		if repoid.IsReal(repo) {
			res.AlreadyQualifiedRowCount++
			if haveMapping && mapped != repo {
				// The mapping disagrees with stored reality. Refuse and REPORT.
				// Keyed by (session, storedRepo) so a session whose rows somehow
				// carry two different real repos surfaces as two distinct
				// conflicts rather than collapsing into one misleading line.
				res.ConflictRowCount++
				key := session + "\x00" + repo
				c := conflicts[key]
				if c == nil {
					c = &RepairRepoConflict{SessionID: session, StoredRepo: repo, MappedRepo: mapped}
					conflicts[key] = c
				}
				c.RowCount++
				c.CostMicroSum += costMicro
			}
			continue
		}

		res.UnqualifiedRowCount++
		if !haveMapping {
			// Unresolvable: reported, never guessed. "" is the no-session bucket
			// (proxy/poller rows), which no mapping could ever fix.
			res.UnresolvedRowCount++
			if session == "" {
				res.UnresolvedNoSessionRowCount++
			}
			u := unresolved[session]
			if u == nil {
				u = &RepairRepoUnresolved{SessionID: session}
				unresolved[session] = u
			}
			u.RowCount++
			u.CostMicroSum += costMicro
			continue
		}

		res.ChangedRowCount++
		res.ChangedCostMicroSum += costMicro
		// Grouped by (observed from_repo -> to_repo), NOT by target alone, so the
		// aggregate ledger records the value these rows actually held instead of
		// assuming the sentinel. See RepairRepoDelta.FromRepo.
		key := repo + "\x00" + mapped
		acc := byRepo[key]
		if acc == nil {
			acc = &repoAcc{
				delta:    RepairRepoDelta{FromRepo: repo, Repo: mapped},
				sessions: map[string]struct{}{},
			}
			byRepo[key] = acc
		}
		acc.delta.RowCount++
		acc.delta.CostMicroSum += costMicro
		acc.sessions[session] = struct{}{}
		changes = append(changes, change{id: id, oldRepo: repo, newRepo: mapped})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RepairRepoResult{}, fmt.Errorf("repair-repo: iterate candidates: %w", err)
	}
	// Close the cursor BEFORE issuing any UPDATE: the transaction holds a single
	// connection and database/sql cannot run a write on it while a read cursor is
	// still open.
	if err := rows.Close(); err != nil {
		return RepairRepoResult{}, fmt.Errorf("repair-repo: close candidate cursor: %w", err)
	}

	res.ByRepo = make([]RepairRepoDelta, 0, len(byRepo))
	for _, acc := range byRepo {
		acc.delta.SessionCount = len(acc.sessions)
		res.ByRepo = append(res.ByRepo, acc.delta)
	}
	// Ascending by (from, to) so the report, the audit-row insert order, and the
	// tests are all deterministic — Go map iteration order is randomized, so
	// without this the ledger's row order would differ run to run.
	sort.Slice(res.ByRepo, func(i, j int) bool {
		if res.ByRepo[i].FromRepo != res.ByRepo[j].FromRepo {
			return res.ByRepo[i].FromRepo < res.ByRepo[j].FromRepo
		}
		return res.ByRepo[i].Repo < res.ByRepo[j].Repo
	})
	res.Unresolved = sortedRepairUnresolved(unresolved)
	res.Conflicts = sortedRepairConflicts(conflicts)

	// The mapping side of the ledger. Computed here, before the dry-run return, so
	// a dry run reports it too — catching a broken mapping BEFORE the commit is
	// the entire point of having a dry run.
	res.MappedSessionCount = len(mapping)
	for session := range mapping {
		if _, ok := matchedSessions[session]; !ok {
			res.UnmatchedSessions = append(res.UnmatchedSessions, session)
		}
	}
	// Ascending: Go map iteration is randomized, and a report whose lines reorder
	// between two runs of the same command is a report an operator cannot diff.
	sort.Strings(res.UnmatchedSessions)

	if !opts.Commit {
		// Dry run: report only. The deferred Rollback discards the read tx.
		return res, nil
	}

	// A commit with nothing to change writes no rows and no audit — there is no
	// mutation to record, and this is exactly the second-run no-op path.
	// Committed reflects that the request was honored; RepairID stays empty
	// because the ledgers gained nothing.
	if len(changes) == 0 {
		res.Committed = true
		return res, nil
	}

	repairID, err := newAuditID()
	if err != nil {
		return RepairRepoResult{}, fmt.Errorf("repair-repo: mint repair id: %w", err)
	}

	// Prepare BOTH statements once, outside the loop. The loop body is unchanged
	// in meaning: tx.ExecContext re-parses and re-plans the identical SQL text on
	// every iteration, and at repair scale that dominates.
	//
	// MEASURED, AND THE CONTROL ARM IS IN THE TREE — which is the only reason this
	// number is worth anything. Reproduce it with:
	//
	//	go test ./internal/store -run '^$' \
	//	  -bench 'BenchmarkRepairRepoCommit$|BenchmarkRepairRepoCommitUnprepared$' \
	//	  -benchtime=3x -count=3
	//
	// 5000 rows (repairRepoBenchRows), darwin/arm64, Apple M5 Max. Stated as the
	// RANGE over seven samples (three invocations of the command above), because a
	// single median implies a precision this does not have — the invocations
	// differed by ~3% on the absolutes:
	//
	//	unprepared  37.0ms - 38.2ms per repair
	//	prepared    16.6ms - 17.4ms per repair
	//	ratio       2.2x (2.15x - 2.29x over the same samples)
	//
	// The RATIO is the durable part; the absolutes drift with the machine. A
	// seventh sample taken after the first six landed at 16.63/37.01 — outside
	// the six-sample absolute range on both ends, inside the ratio band at
	// 2.23x. The absolute bounds above were widened to admit it rather than
	// left to read as tighter than the machine actually is.
	//
	// ⚠️ READ THE RATIO CORRECTLY: both arms time the WHOLE RepairRepo call, not
	// the loop. The candidate scan, the classification and the COMMIT are inside
	// the timed region and are identical in both arms, so 2.23x is a LOWER BOUND on
	// the loop's own speedup, not a measurement of it. Do not restate it as a
	// loop-level figure.
	//
	// 🔴 AND DO NOT WRITE A NUMBER HERE THAT THE NAMED HARNESS DOES NOT PRINT. The
	// previous version of this block quoted figures for 5000 rows while the
	// checked-in benchmark seeded 2000 and had no unprepared arm at all: running it
	// exactly as documented printed 7.3ms, which is neither number, and the ratio
	// could not be obtained without hand-reverting the optimization. The figures
	// happened to be right for the size they named — that is precisely how it
	// survived review. Change repairRepoBenchRows and this block is stale.
	//
	// Statements are transaction-scoped, so they die with the tx; Close is
	// DEFERRED rather than called after the loop so an early return on the
	// fail-loud path below does not leak them.
	var auditStmt, updateStmt repairRowExecer
	if opts.unprepared {
		// BENCHMARK CONTROL ARM ONLY (opts.unprepared is unexported). This is the
		// pre-hoist shape, kept executable so the ratio above is a command anyone
		// can run rather than a number to be trusted.
		auditStmt = repairTxExecer{tx: tx, query: repairRepoRowAuditSQL}
		updateStmt = repairTxExecer{tx: tx, query: repairRepoUpdateSQL}
	} else {
		prepAudit, err := tx.PrepareContext(ctx, repairRepoRowAuditSQL)
		if err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: prepare before-image insert: %w", err)
		}
		defer func() { _ = prepAudit.Close() }()
		prepUpdate, err := tx.PrepareContext(ctx, repairRepoUpdateSQL)
		if err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: prepare guarded update: %w", err)
		}
		defer func() { _ = prepUpdate.Close() }()
		auditStmt, updateStmt = prepAudit, prepUpdate
	}

	for _, c := range changes {
		// Before-image FIRST, from the buffered pre-repair value (NOT a re-read —
		// the UPDATE below overwrites it). It commits or rolls back in lockstep
		// with the UPDATE and the aggregate ledger.
		if _, err := auditStmt.ExecContext(ctx,
			repairID, c.id, c.oldRepo, c.newRepo,
		); err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: write row before-image (row %d): %w", c.id, err)
		}
		out, err := updateStmt.ExecContext(ctx, c.newRepo, c.id, c.oldRepo)
		if err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: update row %d: %w", c.id, err)
		}
		n, err := out.RowsAffected()
		if err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: rows affected for row %d: %w", c.id, err)
		}
		// FAIL LOUD, never silent-fallback. Zero here means the compare-and-swap
		// guard blocked the write: the row no longer holds the value the
		// before-image just recorded. Inside one transaction that should be
		// impossible, so it means either a classification bug or a concurrent
		// writer that broke the single-writer assumption. Either way, committing
		// a ledger that describes a change that did not happen is the one outcome
		// worse than failing — abort and roll the whole repair back.
		if n != 1 {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: row %d: expected to update exactly 1 row holding repo %q, updated %d — the row changed underneath this repair; nothing has been committed", c.id, c.oldRepo, n)
		}
	}

	// One aggregate row per distinct target repo — the durable record of what this
	// run moved, keyed by the shared repair_id.
	// `del`, not `d`: `d` is this method's *DB receiver, and shadowing it inside a
	// loop that may one day need db access is a trap. Reprice names it `del` too.
	for _, del := range res.ByRepo {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO repo_repair_audit
			    (repair_id, developer, from_repo, to_repo, row_count, cost_micro_sum, tool_version)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			repairID, opts.Developer, del.FromRepo, del.Repo, del.RowCount, del.CostMicroSum, opts.ToolVersion,
		); err != nil {
			return RepairRepoResult{}, fmt.Errorf("repair-repo: write audit row (repo %s): %w", del.Repo, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return RepairRepoResult{}, fmt.Errorf("repair-repo: commit: %w", err)
	}
	committed = true
	res.Committed = true
	res.RepairID = repairID
	return res, nil
}

// sortedRepairUnresolved orders the unresolvable buckets ascending by session id.
// The "" (no-session) bucket sorts first by construction, which is where an
// operator most wants it: those rows can never be repaired by any mapping.
func sortedRepairUnresolved(m map[string]*RepairRepoUnresolved) []RepairRepoUnresolved {
	out := make([]RepairRepoUnresolved, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// sortedRepairConflicts orders refused disagreements by (session, stored repo) so
// the report is deterministic.
func sortedRepairConflicts(m map[string]*RepairRepoConflict) []RepairRepoConflict {
	out := make([]RepairRepoConflict, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].StoredRepo < out[j].StoredRepo
	})
	return out
}
