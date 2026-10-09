package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"
)

// Window digests (#716) — a recomputable content identity for the rows a
// published report was computed over.
//
// The question a digest answers is narrow and worth stating exactly: "are the
// rows selected by predicate P here byte-for-byte the rows that were here when
// the report for P was published?" P is a window and, since #747, optionally a
// repository — and it is hashed IN, so two predicates over the same rows are two
// different identities (see digestDomainFor).
// It is tamper-EVIDENCE, not a signature: anyone holding
// the store can recompute it, and nothing here proves WHO computed it. What it
// does buy is that a late-arriving row, a reprice, a deletion or an edited
// column inside an already-published window all become visible as a changed
// digest instead of a silently different report.
//
// 🔴 WHY THIS IS NOT A HASH OVER idempotency_key, WHICH IS THE OBVIOUS DESIGN.
// It would produce a confident hash over an INCOMPLETE set. Two facts, both
// re-measured on this tree before this file was written:
//
//   - token_events.idempotency_key is declared `idempotency_key TEXT` — nullable,
//     no NOT NULL, no default (see schemaTables).
//   - the unique index is PARTIAL: `CREATE UNIQUE INDEX
//     idx_token_events_idempotency ON token_events(idempotency_key) WHERE
//     idempotency_key IS NOT NULL` (see schemaPostMigration), and
//     insertTokenEventSQL binds it through `NULLIF(?, '')`.
//
// So every unkeyed producer writes SQL NULL: the proxy when the upstream
// response carries no id (see idempotencyKeyForProxy), the pollers, the manual
// /costs surface, and every pre-#19 row. A digest keyed on that column omits
// them without a word — the "green while measuring nothing" failure this
// program exists to prevent. The digest below is over the ROWS, and every
// column it covers is NOT NULL.
//
// Both facts above are asserted at runtime, not just asserted here:
// TestDigestCoversRowsWithNullIdempotencyKey reads the index definition back out
// of sqlite_master, requires the fixture to contain at least one NULL-keyed row,
// and then requires mutating that row to move the digest.
//
// 🔴 WHY ts IS READ AS `CAST(ts AS TEXT)` AND HASHED AS RAW STORED BYTES.
// This is the single most fragile decision in the file, and it is measured, not
// reasoned. The ts column is `DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP` and
// holds at least THREE different textual encodings today, side by side in the
// same column:
//
//	'2026-08-01 10:00:00 +0000 UTC'   -- a Go time.Time bound by the driver
//	'2026-08-01 11:00:00 +0100 X'     -- ditto, written in a non-UTC zone (#723)
//	'2026-08-29 00:55:35'             -- a CURRENT_TIMESTAMP default
//
// Scanning that column into a Go `string` does NOT return those bytes. Measured
// on modernc.org/sqlite v1.48.0: the driver parses text under a DATETIME
// decltype into a time.Time when it recognises the layout, and database/sql
// then renders it as RFC3339Nano — so row 1 reads back "2026-08-01T10:00:00Z",
// while row 2, whose layout the driver does NOT recognise, passes through
// verbatim. The projection is therefore a MIXTURE whose composition depends on
// the driver's internal format list. Hashing that would make the digest change
// when a dependency is bumped, with no row having changed — a false alarm on a
// published claim, which is the one failure mode a tamper-evidence surface may
// not have.
//
// Scanning into a time.Time is worse: measured, it HARD-ERRORS on the
// non-UTC-zone row ("unsupported Scan, storing driver.Value type string into
// type *time.Time"), so a single such row would make the whole window
// undigestable.
//
// `CAST(ts AS TEXT)` strips the DATETIME decltype from the result expression,
// the driver's conversion never fires, and all three rows come back as exactly
// the bytes `quote(ts)` shows — verified against quote() for each. Those bytes
// are also precisely what `ORDER BY ts` compares and what VACUUM copies, so the
// digest, the ordering it depends on, and the file all agree on one
// representation.
//
// ⚠️ THE COST OF THAT CHOICE, STATED SO IT IS NOT A SURPRISE LATER: a migration
// that REWRITES existing ts bytes into a canonical encoding (the shape #723's
// fix could take) changes the digest of an already-published window even though
// no instant moved. That is honest — the stored bytes did change — but it means
// such a migration must be treated as re-publishing every window, not as a
// transparent repair. The alternative (parse, then hash a normalized instant)
// was rejected because it requires this file to own a format list, which is the
// version-dependence above with an extra step, and it cannot represent the
// '+0100 X' rows at all.
//
// 🔴 WHY THE WINDOW BOUNDS ARE NOT IN THE DIGEST. The digest attests a ROW SET.
// The window that selected it is published beside it on the manifest, not
// folded into it, for two reasons: an open-ended window (a zero `until`, which
// tsWindow supports) has no stable upper bound to hash, and an auditor
// recomputing the digest from the exported rows must be able to do so from the
// rows alone. Read the pair as "window W contained exactly these rows" — the
// digest is the second half of that sentence, never the whole of it.
//
// 🔴 THE TWO DIGESTS ARE NOT A CONSISTENT SNAPSHOT OF EACH OTHER, AND THE
// MANIFEST CALLER MUST FIX THAT, NOT THIS FILE. EventsDigest and OutcomesDigest
// are independent reads on independent pooled connections. A row landing between
// them yields a pair describing two different instants — and because a manifest
// publishes them side by side as one claim about one window, an auditor
// recomputing both together would get a mismatch on one of them with nothing
// having been tampered with. That is a FALSE ALARM, the one failure mode a
// tamper-evidence surface may not have.
//
// It is left to the caller deliberately: the fix is to take both reads inside a
// single transaction (see beginRead), and doing that HERE would make every digest
// hold one of maxOpenConns connections for its whole run whether the caller needs
// the pairing or not — beginRead's own doc block explains what pool occupancy
// costs. ⇒ When #715's manifest wires these up, it must take the pair under one
// read transaction. A manifest that calls the two methods back to back is subtly
// wrong in a way no test in this file can see, because nothing here writes
// concurrently.

const (
	// digestScheme tags every value this file returns, so a stored digest
	// declares which algorithm produced it and a future scheme can be
	// distinguished rather than silently compared. Same discipline as
	// priceTableHashScheme (#713).
	digestScheme = "tierdig1"

	// Domain separation. These are hashed as the FIRST frame of every digest,
	// which is what makes an empty window's digest a distinguishable sentinel
	// rather than sha256("") — the value a "we never ran" code path would most
	// naturally produce. It also keeps an empty token_events window from
	// colliding with an empty outcomes window, which are different claims.
	digestDomainTokenEvents = digestScheme + "/token_events"
	digestDomainOutcomes    = digestScheme + "/outcomes"

	// digestDomainRepoInfix separates the table domain from the repository slug
	// when a digest is SCOPED (#747). See digestDomainFor for why the predicate
	// belongs inside the hashed frame rather than beside it.
	digestDomainRepoInfix = "/repo/"

	// digestFramesPerTokenEvent / digestFramesPerOutcome are the FIXED number of
	// length-prefixed frames each row contributes. Fixed arity is load-bearing,
	// not decorative: it is what lets the frame stream be regrouped into records
	// unambiguously, and therefore what makes distinct row sets produce distinct
	// bytes. See the injectivity note on appendTokenEventFrames.
	digestFramesPerTokenEvent = 17
	digestFramesPerOutcome    = 11

	// digestRowBytesHint pre-sizes the per-row frame buffer so the steady-state
	// loop does not reallocate. 16 frames × 8 bytes of framing is 128 before a
	// single payload byte, and the payloads (developer, repo, model, host, a
	// ~29-byte ts and eight integers) run to roughly the same again.
	digestRowBytesHint = 320
)

// Digest is a window's content identity together with the row count it covers.
//
// Rows is not decoration and not derivable after the fact without a second full
// scan: it is the DENOMINATOR that proves a digest was earned. An empty window
// and a window whose read was cut short both yield a valid-looking Value, and
// only Rows distinguishes "this window genuinely holds nothing" from "this
// measured nothing". Callers publishing a Value should publish Rows beside it.
//
// Rows is deliberately NOT hashed into Value: with fixed per-row arity the
// count is already recoverable from the frame stream (frames ÷ frames-per-row),
// so folding it in would add no injectivity and one more thing to keep in step.
type Digest struct {
	// Value is "tierdig1:" followed by 64 lowercase hex characters. Never empty,
	// including for an empty window.
	Value string
	// Rows is the number of rows the digest covers.
	Rows int64
}

// tokenEventDigestRow is the cost-bearing identity of one token_events row: the
// columns whose change must be visible in the digest.
//
// The field ORDER is the frame order and is part of the wire format — reordering
// these fields changes every digest this store has ever published. Every field
// must be written by appendTokenEventFrames;
// TestDigestFramesCoverEveryTokenEventField pins that per-field by mutating each
// one in turn and requiring the frames to change, so a field that is added here
// and forgotten there cannot fall silently outside the identity.
//
// ⚠️ Notably ABSENT, each for a stated reason: session_id (an opaque grouping
// key that no published figure reads), billed_to (#854: a /costs admission
// declaration that no spend query reads), attribution_rule (#823: no published
// figure or spend query reads it as of #823 S1; the first report that reads it
// must move it into the digest with a digestScheme bump) and idempotency_key (the column this
// digest deliberately does not depend on — see the file header). fidelity USED to
// be on this list, justified as "provenance"; that was measured FALSE and it is
// now covered. See the Fidelity field below.
// cost_clamped is diagnostic history; no spend or scoring query reads it.
// poller_baseline records the baseline cutover; no spend or scoring query reads it.
//
// 🔴 Adding or removing any field here is a WIRE-FORMAT change: it re-identifies
// every digest previously published. Once anything is published, such a change
// requires a digestScheme bump. TestDigestGoldenVector is what makes that
// impossible to do silently.
type tokenEventDigestRow struct {
	ID           int64
	TS           string // raw stored bytes, via CAST(ts AS TEXT)
	Developer    string
	Repo         string
	IssueID      string
	Model        string
	Host         string
	InputTok     int64
	OutputTok    int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
	CostMicro    int64
	PriceVersion int64
	BillingMode  string
	Source       string
	// Fidelity is covered even though #716's enumerated column list omits it, and
	// the reason is measured rather than stylistic: it is NOT mere provenance.
	// store.go's DeveloperCosts reads
	//     SUM(CASE WHEN fidelity = 'realtime' THEN cost_micro ELSE 0 END)
	// (store.go:4781 and :4851), and that figure is published through
	// handler.go:2733, :3664 and :4319. Measured before it was added here:
	// flipping fidelity 'daily'->'realtime' on three in-window rows moved the
	// published realtime total from 0 to 4400 while leaving the digest byte-identical.
	// A silent restatement of a published cost split inside an already-published
	// window was therefore invisible — the same shape as the reprice this file
	// exists to catch. #716 says the digest covers the "cost-bearing identity";
	// fidelity is part of it, so the enumerated list was short of its own principle.
	Fidelity string
}

// outcomeDigestRow is the outcomes counterpart. Same field-order and
// wire-format contract as tokenEventDigestRow — read its doc block first.
//
// ⚠️ WHAT THIS DOES **NOT** COVER, ENUMERATED BECAUSE THE OMISSIONS ARE NOT
// OBVIOUS AND ONE OF THEM IS ARGUABLE. Measured: each of these edits leaves the
// digest byte-identical —
//
//	UPDATE outcomes SET weight_source = 'label'              -- digest unchanged
//	UPDATE outcomes SET work_type_source = 'label'           -- digest unchanged
//	UPDATE outcomes SET additions = 999999, deletions = 1    -- digest unchanged
//	UPDATE outcomes SET push_day = '2026-01-01'              -- digest unchanged
//	UPDATE outcomes SET author_type = 'Bot'                  -- digest unchanged
//
// TWO LINES ARE DRAWN, NOT ONE, AND THE SECOND IS THE ONE THAT IS EASY TO LOSE:
//
//  1. covered if it changes a PUBLISHED figure — weight, quality, work_type,
//     developer, repo, issue_id all feed scoring;
//  2. covered if it is the EVIDENCE the figure came from real work — pr_number
//     and merge_commit_sha, which move no number at all. See their field
//     comments; that second line is why they are in.
//
// What remains outside fails BOTH tests: additions / deletions / changed_files
// are raw diff stats retained for a future recalibration and read by nothing
// today; weight_source / work_type_source record HOW a value was derived rather
// than the value or its origin; push_day is a dedup key for the push-capture
// path. author_type (#856) is the arguable one: it is no figure, but it decides
// whether its author counts toward the k-anonymity floor, so editing it moves
// data_quality.uncounted_active_ids.bot and can fold or withhold a group. It
// stays out for the reason the roster and alias tables do, which play the same
// role: the digest attests the figures and their evidence, not which groups the
// k floor lets through. ⚠️ If any of them ever starts feeding a published figure — or becomes
// the thing a report points at as evidence — it moves across the line, and that
// is a WIRE-FORMAT change: extend the struct AND the appender, bump
// digestFramesPerOutcome, and bump digestScheme if anything has been published
// by then.
type outcomeDigestRow struct {
	ID        int64
	TS        string // raw stored bytes, via CAST(ts AS TEXT)
	Developer string
	Repo      string
	IssueID   string
	Weight    float64
	Quality   float64
	WorkType  string
	Source    string
	// PRNumber and MergeCommitSHA are covered even though they change NO
	// published figure, and that is exactly why the reason has to be written down
	// — the "it moves no number, so leave it out" argument is the one that would
	// take them back out again.
	//
	// 🔴 THE DIGEST IS THE TAMPER-EVIDENCE HALF OF A PROVENANCE CLAIM, NOT ONLY
	// AN ARITHMETIC ONE. These two columns are the evidence tying an outcome to
	// real merged work. If merge_commit_sha can be re-pointed at a different
	// commit with the digest unmoved, an outcome can be silently re-attributed to
	// work it did not come from — and a verify-report would return REPRODUCED,
	// asserting something FALSE about provenance while telling the truth about
	// every number. A digest that certifies the sums but not their origin
	// certifies the less interesting half.
	//
	// ⚠️ Widening the digest is cheap; a forgeable provenance field is a one-way
	// door once #715 publishes. That asymmetry is the whole argument — do not
	// re-derive it from "does this column appear in a report".
	//
	// NULL IS GENUINELY REACHABLE HERE, unlike every other column in either row
	// struct: a push-captured outcome has no PR and no merge commit, so both are
	// stored NULL (InsertOutcome NULLIFs an empty SHA). The projection's COALESCE
	// is therefore load-bearing rather than defensive, and it renders NULL exactly
	// as ListOutcomes does ('' and 0) so digest and export agree on the value.
	PRNumber       int64
	MergeCommitSHA string
}

// The projections below mirror ListTokenEvents / ListOutcomes' COALESCE choices
// for the shared columns, so a value that is NULL in storage renders the same way
// on both surfaces. The convergent NOT NULL DEFAULTs mean NULL is not reachable
// in these columns today (every one was added via addColumnIfMissing with a
// default); the COALESCEs keep digest and export in step if that ever changes.
//
// 🔴 BUT THE DIGEST IS **NOT** RECOMPUTABLE FROM /api/v1/events, AND AN EARLIER
// VERSION OF THIS COMMENT CLAIMED IT WAS. It said the two must agree "or an
// auditor recomputing the digest from /api/v1/events would get a mismatch on
// untampered data". They deliberately DISagree on ts, which is in every frame:
//
//	export publishes ts as  "2026-08-05T00:00:00Z"      (rendered via time.Time)
//	digest hashes ts as     "2026-08-05 00:00:00 +0000 UTC"  (raw stored bytes)
//
// That is the CAST decision above, working as intended — but it means an auditor
// needs SQL-level access to the stored bytes, not the JSON export. ⚠️ #715 must
// answer where that access comes from before it publishes a digest as
// independently checkable; today it is a claim only the server can verify.
// (Measured aside, and a genuine bug OUTSIDE these two files: over a window
// containing a non-UTC-zone row, ListTokenEvents fails outright —
// `sql: Scan error on column index 18, name "ts"` — because store.go scans ts
// into a time.Time. The digest reads the same window fine. Worth its own issue.)
//
// ORDER BY (ts, id) is served by idx_token_events_ts_id / idx_outcomes_ts_id —
// both already exist with exactly this shape — so the read is a window seek and
// an ordered walk, with no sort and no temp b-tree.
// TestDigestReadsAreOneWindowSeek pins that against the planner, and
// TestDigestSQLRequestsItsOrderExplicitly pins that the ORDER BY is asked for at
// all — which, measured, no behavioural test in the suite can see.
const (
	tokenEventDigestSelect = `
		SELECT id, CAST(ts AS TEXT), developer, COALESCE(repo, 'unqualified'),
		       issue_id, model, host, input_tok, output_tok, cache_read,
		       cache_write_5m, cache_write_1h, cost_micro, price_version,
		       billing_mode, source, fidelity
		FROM token_events WHERE `

	outcomeDigestSelect = `
		SELECT id, CAST(ts AS TEXT), developer, COALESCE(repo, 'unqualified'),
		       issue_id, weight, quality, COALESCE(work_type, 'feature'),
		       COALESCE(source, 'github-webhook'), COALESCE(pr_number, 0),
		       COALESCE(merge_commit_sha, '')
		FROM outcomes WHERE `

	digestOrderSQL = ` ORDER BY ts, id`
)

// tokenEventDigestQuery / outcomeDigestQuery assemble the full statement from
// the projection, the caller's tsWindow clause and digestOrderSQL.
//
// 🔴 THESE EXIST BECAUSE ASSERTING ON digestOrderSQL ALONE GUARDS NOTHING.
// Measured: with the order pinned only as a constant, dropping `+digestOrderSQL`
// from the CALL SITE left the entire suite green — including the EQP test, whose
// plan is byte-identical either way (the ts predicate selects the same index
// whether or not an order was requested). The constant was guarded; its USE was
// not. Routing both methods through one builder gives the tests a single place
// that is both asserted on and actually executed, and
// TestDigestQueriesAreBuiltByTheSharedBuilder adds an AST arm so a future inline
// concatenation cannot slip past it either.
func tokenEventDigestQuery(clause string) string {
	return tokenEventDigestSelect + clause + digestOrderSQL
}

func outcomeDigestQuery(clause string) string {
	return outcomeDigestSelect + clause + digestOrderSQL
}

// digestQueryer is the read surface the digest core needs. Both *sql.DB and
// *sql.Tx satisfy it, which is what lets WindowDigests run the pair inside ONE
// transaction while the single-table methods stay on the pool.
type digestQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ErrDigestWindowInvalid reports a window that cannot contain anything.
//
// 🔴 IT IS AN ERROR RATHER THAN AN EMPTY DIGEST BECAUSE THE TWO ARE OTHERWISE
// INDISTINGUISHABLE. Measured before this guard existed: an inverted window
// (until < since) returned the empty-window sentinel with rows=0 and a nil
// error — byte-identical to a genuinely empty window. A manifest would then
// publish "window W contained no rows" for a window that was simply malformed.
// That is the same confusion the sentinel itself exists to prevent, one level up.
var ErrDigestWindowInvalid = errors.New("digest window is empty or inverted")

// checkDigestWindow rejects a window that cannot contain a row. A zero `until`
// is tsWindow's documented open-ended form and is explicitly allowed.
func checkDigestWindow(since, until time.Time) error {
	if !until.IsZero() && !until.After(since) {
		return fmt.Errorf("%w: [%s, %s)", ErrDigestWindowInvalid,
			since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	}
	return nil
}

// digestDomainFor renders the domain-separation frame for one table under one
// repo scope (#747). Fleet-wide it is the bare table domain, byte-for-byte what
// this file hashed before scoping existed; scoped it carries the slug.
//
// 🔴 THE PREDICATE IS INSIDE THE DIGESTED FRAME, DELIBERATELY, AND THE
// ALTERNATIVE IS WORSE THAN IT LOOKS. A digest answers "were these rows, under
// this predicate, byte-for-byte what was here before" — the predicate is half
// the sentence. Leave it out and a fleet-wide digest over a single-repository
// install is BYTE-IDENTICAL to that install's scoped digest, so an operator who
// re-requests a manifest without `?repo=` gets an UNCHANGED that asserts
// something the run never checked; the day a second repository is onboarded the
// two silently stop meaning the same thing, and no comparison anywhere can see
// the moment it happened. Folding the slug in makes a scope change a CHANGED
// digest, which is a reading, where the absence of one is not.
//
// ⚠️ IT IS A DOMAIN FRAME, NOT AN EXTRA FRAME, AND THAT IS A WIRE-FORMAT
// REQUIREMENT rather than a stylistic one. appendTokenEventFrames' injectivity
// argument rests on FIXED per-row arity with a fixed-size prologue; appending a
// conditional second prologue frame would make the prologue variable-arity and
// weaken the regrouping argument the whole scheme stands on. Rewriting the ONE
// prologue frame keeps the stream exactly as decodable as before.
//
// 🔑 FLEET-WIDE BYTES ARE UNCHANGED, WHICH IS WHY digestScheme IS NOT BUMPED.
// Every digest published before #747 was fleet-wide, and every one of them still
// recomputes to the same value — TestDigestGoldenVector's frozen constants are
// the pin. A scoped digest is a NEW value that never existed, so it invalidates
// nothing.
func digestDomainFor(base string, scope RepoScope) string {
	if scope.IsFleetWide() {
		return base
	}
	return base + digestDomainRepoInfix + string(scope)
}

// EventsDigest returns the content identity of the token_events rows in the
// half-open window [since, until) that match `scope`. A zero `until` means "no
// upper bound"; store.FleetWide means "every repository".
//
// The window predicate is tsWindow's, unchanged and unconditionally — the same
// helper every windowed read in this store uses. That is a correctness
// requirement, not tidiness: the digest must cover EXACTLY the rows the report
// covered, so any boundary quirk in tsWindow must apply identically here. If
// tsWindow's semantics ever change, digest and report move together; were this
// to hand-roll its own predicate, they could silently diverge and the digest
// would attest a set nobody published.
//
// ⚠️ ONE SUCH QUIRK IS REAL, MEASURED, AND WORTH KNOWING BEFORE YOU TRUST A
// BOUNDARY. tsWindow binds a time.Time, which the driver renders as
// "2026-08-01 00:00:00 +0000 UTC", and SQLite compares it LEXICALLY against the
// stored encodings. For a row written by Go the half-open contract holds exactly.
// But a row carrying the bare CURRENT_TIMESTAMP encoding ("2026-09-01 00:00:00",
// no zone suffix) is a SHORTER string that sorts before the zone-suffixed bound,
// so such a row at exactly `since` is EXCLUDED and one at exactly `until` is
// INCLUDED — effectively (since, until] for that encoding.
// ⛔ Do NOT "fix" that here: the report reads through the same helper and drops
// the same rows, and a digest that disagreed with the report would be worse than
// one that inherits its quirk. TestDigestWindowBoundsAreHalfOpen pins both
// behaviours so a future normalization migration has a test that changes.
//
// The hash is streamed: one row's frames are built in a reused buffer and folded
// into the running sha256, so memory is O(1) in the window size rather than
// O(rows). This is a full-window scan — a fifth one alongside the four /scores
// already pays — and belongs on the manifest surface, not on a scoring path.
// BenchmarkEventsDigest carries the measured cost and the measured split between
// this code and the driver; read it before optimizing anything here.
//
// 🔴 THE REPO PREDICATE IS RepoScope.clause()'s, UNCHANGED AND UNCONDITIONALLY,
// for exactly the reason the window predicate is tsWindow's (#747). A scoped
// report reads through that same strict `repo = ?` conjunct, so the digest covers
// precisely the rows the report covered. ⛔ In particular the 'unqualified'
// sentinel is EXCLUDED from a scoped digest, and that is the right answer rather
// than a limitation: a scoped /scores drops those rows from every SCORED figure
// too (see RepoScope's "strict, and it deliberately diverges from RepoMatch"
// block), so a digest that covered them would attest rows no published score was
// computed from.
//
// 🔴 ONE THING A SCOPED DIGEST THEREFORE DOES NOT ATTEST, AND AN EARLIER VERSION
// OF THIS COMMENT DENIED IT. It said the sentinel rows "remain fully covered by
// the FLEET-WIDE digest, so nothing falls out of every identity". That is true of
// the identity SET as a whole and FALSE of the set a scoped manifest ships, which
// contains no fleet-wide digest at all. A scoped /scores still READS those rows —
// api.buildScopeDisclosure calls UnqualifiedExclusionWindow over this very band
// and publishes data_quality.repo_scope_excluded.{token_events,cost_usd,outcomes}.
// Measured: an in-place reprice of a repo-blind row moved repo_scope_excluded's
// cost_usd from 9 to 14 while the scoped digest AND the scoped watermarks stayed
// byte-identical, so verify-report prints REPRODUCED over changed served bytes.
// ⇒ Same class as the cost_coverage_start gap named in cmd/tierd/verifyreport.go's
// header.
//
// ✅ CLOSED BY #751, AND NOT HERE — the manifest now pins the exclusion COUNTS as
// their own quantity (api.manifestRepoScopeExcludedJSON), which is why nothing in
// this file changed. The alternative on the table, publishing the fleet-wide
// digest ALONGSIDE the scoped one, was rejected: its `rows` is an unfloored count
// of work over a caller-chosen window handed to a caller who asked about ONE
// repository, which is the #593 disclosure shape.
// ⛔ Do not "fix" it by widening this predicate either — that reintroduces the
// over-attribution #590 exists to close. Two guards redden if you try:
// TestScopedDigest_ExcludesTheUnqualifiedSentinel here, and the #751
// manifest-exclusion arm in internal/api, which requires the scoped digest to
// stay byte-identical across exactly the repo-blind reprice the new pin catches.
// ⚠️ That second one is named in prose rather than cited by identifier ON
// PURPOSE: TestDigestDocCitationsResolve resolves every Test/Benchmark name in
// this file against THIS package's declarations, so citing a cross-package guard
// by name would be a claim it cannot check — and it fails loudly if you do.
//
// ⚠️ THE EXCLUSION COMES FROM THE TRUST BOUNDARY, NOT FROM SQL, and the
// difference matters if you ever move the validation. `repo = 'unqualified'`
// matches the sentinel rows perfectly well — measured, in
// TestScopedDigest_ExcludesTheUnqualifiedSentinel's last arm. What makes the
// sentinel unreachable as a SCOPE is repoid.Canonical refusing it upstream, so a
// caller cannot ask to be scoped to repo-blindness. This method does not
// re-check that; see ReportDigests on why the slug must already be canonical.
//
// ⚠️ THE PLAN CHANGES WHEN SCOPED, and the budget note above assumes it. `repo`
// is in neither idx_token_events_ts_id nor idx_outcomes_ts_id, so a scoped read
// still SEEKS the window on the index but must also touch the table to evaluate
// the conjunct — the same asymmetry ReportWatermarks documents. It stays
// proportional to the WINDOW, not the table, which is what the placement argument
// actually needs. Two guards, one per arm: TestDigestReadsAreOneWindowSeek pins
// the fleet-wide plan and TestScopedDigestReadIsStillAWindowSeek the scoped one.
func (d *DB) EventsDigest(ctx context.Context, since, until time.Time, scope RepoScope) (Digest, error) {
	return eventsDigestFrom(ctx, d.db, since, until, scope)
}

// OutcomesDigest is EventsDigest's counterpart over the outcomes table. See that
// method's doc block for the window, scope, streaming and ordering contract,
// which is identical.
func (d *DB) OutcomesDigest(ctx context.Context, since, until time.Time, scope RepoScope) (Digest, error) {
	return outcomesDigestFrom(ctx, d.db, since, until, scope)
}

// WindowDigests returns BOTH digests for one window, read inside a SINGLE
// transaction so they describe the same instant.
//
// 🔴 THIS IS THE CALL A MANIFEST MUST USE, AND THE SINGLE-TABLE METHODS ARE THE
// TRAP. EventsDigest followed by OutcomesDigest runs two independent reads on two
// pooled connections; a row landing between them yields a pair describing two
// different instants. Because a manifest publishes the pair side by side as ONE
// claim about ONE window, an auditor recomputing both together gets a mismatch on
// one of them with nothing having been tampered with — a FALSE ALARM, the one
// failure mode a tamper-evidence surface may not have.
//
// ⚠️ An earlier version of this file only DOCUMENTED that hazard and told the
// caller to "take both reads inside a single transaction (see beginRead)". That
// instruction was unactionable: beginRead is unexported, so the #715 consumer in
// internal/api could not follow it. Naming a fix the caller cannot perform is the
// same as not fixing it.
//
// COST: this holds one of maxOpenConns connections for the duration of BOTH
// scans — see beginRead's doc block for what pool occupancy costs. A caller that
// genuinely needs only one table should use the single-table method and pay
// nothing extra.
func (d *DB) WindowDigests(ctx context.Context, since, until time.Time, scope RepoScope) (events, outcomes Digest, err error) {
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return Digest{}, Digest{}, fmt.Errorf("WindowDigests: %w", err)
	}
	defer release()

	events, err = eventsDigestFrom(ctx, tx, since, until, scope)
	if err != nil {
		return Digest{}, Digest{}, err
	}
	outcomes, err = outcomesDigestFrom(ctx, tx, since, until, scope)
	if err != nil {
		return Digest{}, Digest{}, err
	}
	return events, outcomes, nil
}

// ReportDigests is WindowDigests for a REPORT's window: the pair a manifest
// publishes for [since, until), read inside ONE transaction so they describe the
// same instant (#740).
//
// 🔴 IT EXISTS BECAUSE WindowDigests CANNOT EXPRESS THE ONE ASYMMETRY A REPORT
// HAS, AND USING IT HERE WOULD BE WRONG IN BOTH DIRECTIONS. A report does not
// read the two tables over the same window:
//
//	outcomes     [since, until)
//	token_events [since - AttributableWindow, until)   <- 14 days wider
//
// That is not a quirk of this file; it is WindowWatermarks' documented contract,
// and it is the predicate the scoring path actually uses (OutcomeTokenTotals
// builds a per-outcome band [merge - AttributableWindow, merge], which is why
// the manifest publishes `token_since`). Digesting BOTH tables over [since,
// until) leaves a late-ingested token event 10 days before `since` — one that
// can lift a (developer, issue) total past scoring.MinAttributableTokens,
// clear the #136 tripwire and change /scores — OUTSIDE the digest, which is
// exactly the silent miss a content digest exists to close. Digesting both over
// the WIDER band is the opposite error: an outcome 10 days before `since` is not
// in the report at all, so covering it makes an unrelated edit a FALSE ALARM,
// the one failure mode a tamper-evidence surface may not have.
//
// ⇒ The events digest covers the ATTRIBUTION BAND and the outcomes digest covers
// the report window. A consumer reading `events_digest.rows` beside
// `watermarks.window.token_event_count` therefore sees two counts of the SAME set
// (fleet-wide). Both halves of that are pinned:
// TestReportDigests_EventsCoverTheAttributionBandAndOutcomesDoNot here, and the
// served manifest's own row-count consistency arm in internal/api.
//
// ⚠️ THE ONE-SNAPSHOT PROPERTY IS GUARDED **STRUCTURALLY, NOT SEMANTICALLY**, and
// a reader should know which. Nothing here writes concurrently, so no behavioural
// test in this package can observe the pair straddling a write; what catches a
// regression is TestConvertedSiteTableIsAnExhaustiveCensus, which AST-walks for
// beginRead call sites and fails on an unclassified one. Measured: replacing the
// shared transaction with two independent reads is killed by that census and by
// nothing else. ⛔ So if that census is ever relaxed, this contract loses its only
// guard — do not treat "the suite is green" as evidence that the pairing holds.
//
// 🔑 IT IS SCOPED (#747), AND THE SCOPE IS THE CALLER'S, NEVER INFERRED. Pass
// store.FleetWide for a fleet-wide report and the canonical slug the report was
// served under for a scoped one. Publishing a FLEET-WIDE digest beside a
// repo-scoped report was the defect this parameter closes: it attests a SUPERSET
// of the rows that report read, so another team's ingestion moves the digest and
// the operator is told their untouched report is unreproducible — a FALSE ALARM,
// the one failure mode a tamper-evidence surface may not have.
//
// ⛔ THE SLUG MUST ALREADY BE CANONICAL — nothing here calls repoid.Canonical,
// exactly as RepoScope's own doc block requires ("callers validate at the trust
// boundary"). A raw "Acme/Tier" bound into `repo = ?` matches ZERO rows and
// yields a well-formed digest over nothing, which is #718's failure re-created
// here. `rows` is the denominator that makes that visible; publish it.
func (d *DB) ReportDigests(ctx context.Context, since, until time.Time, scope RepoScope) (events, outcomes Digest, err error) {
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return Digest{}, Digest{}, fmt.Errorf("ReportDigests: %w", err)
	}
	defer release()
	return reader{tx}.ReportDigests(ctx, since, until, scope)
}

// ReportDigests is DB.ReportDigests' body, run on r's querier. On a Snapshot it
// shares the snapshot's one transaction with every other read the caller takes
// there (#1038); see DB.ReportDigests for the window and scope contract.
func (r reader) ReportDigests(ctx context.Context, since, until time.Time, scope RepoScope) (events, outcomes Digest, err error) {
	events, err = eventsDigestFrom(ctx, r.q, since.Add(-AttributableWindow), until, scope)
	if err != nil {
		return Digest{}, Digest{}, err
	}
	outcomes, err = outcomesDigestFrom(ctx, r.q, since, until, scope)
	if err != nil {
		return Digest{}, Digest{}, err
	}
	return events, outcomes, nil
}

// digestPredicate assembles the FULL WHERE body a digest read runs under — the
// window conjunct and the repo conjunct — together with their bind args in the
// SAME TEXT ORDER the fragment places them.
//
// 🔑 ONE DEFINITION, FOR THE REASON tokenEventDigestQuery ALREADY EXISTS. The two
// digest readers are byte-identical in this respect, and a hand-rolled copy in
// each is a place for the order to be wrong in exactly one of them — which is a
// SILENT wrong-population read, not a compile error: `repo = ?` bound with a
// timestamp and `ts >= ?` bound with a slug both execute fine and return nothing.
// Routing both through one function means the invariant has a single site that
// tests can reach, which is the discipline this file states for digestOrderSQL
// ("the constant was guarded; its USE was not") and for digestIsCheckable.
//
// ⚠️ RepoScope.clause() leads with " AND " and must therefore be APPENDED to an
// existing WHERE, never used alone; see its doc block for why it must also stay a
// pure conjunct.
func digestPredicate(since, until time.Time, scope RepoScope) (where string, args []any) {
	where, args = tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	return where + scopeSQL, append(args, scopeArgs...)
}

func eventsDigestFrom(ctx context.Context, q digestQueryer, since, until time.Time, scope RepoScope) (Digest, error) {
	if err := checkDigestWindow(since, until); err != nil {
		return Digest{}, fmt.Errorf("EventsDigest: %w", err)
	}
	clause, args := digestPredicate(since, until, scope)
	rows, err := q.QueryContext(ctx, tokenEventDigestQuery(clause), args...)
	if err != nil {
		return Digest{}, fmt.Errorf("EventsDigest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	h := sha256.New()
	buf := bytes.NewBuffer(make([]byte, 0, digestRowBytesHint))
	writeLengthPrefixed(buf, digestDomainFor(digestDomainTokenEvents, scope))
	foldFrames(h, buf)

	var n int64
	for rows.Next() {
		var r tokenEventDigestRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Developer, &r.Repo, &r.IssueID,
			&r.Model, &r.Host, &r.InputTok, &r.OutputTok, &r.CacheRead,
			&r.CacheWrite5m, &r.CacheWrite1h, &r.CostMicro, &r.PriceVersion,
			&r.BillingMode, &r.Source, &r.Fidelity); err != nil {
			return Digest{}, fmt.Errorf("EventsDigest: scan: %w", err)
		}
		appendTokenEventFrames(buf, r)
		foldFrames(h, buf)
		n++
	}
	// 🔴 A row error MUST abort, and MUST NOT return the digest computed so far.
	// A partial walk hashes a PREFIX of the window; returning it would publish a
	// perfectly well-formed identity over a truncated set — the same silent
	// omission the idempotency_key design would have had. Measured: an earlier AST
	// guard here accepted `return Digest{Value: finishDigest(h), Rows: n}, nil`
	// inside this very branch, because it only asked whether the branch returned
	// at all. It now requires the returned error expression to be non-nil.
	if err := rows.Err(); err != nil {
		return Digest{}, fmt.Errorf("EventsDigest: %w", err)
	}
	return Digest{Value: finishDigest(h), Rows: n}, nil
}

func outcomesDigestFrom(ctx context.Context, q digestQueryer, since, until time.Time, scope RepoScope) (Digest, error) {
	if err := checkDigestWindow(since, until); err != nil {
		return Digest{}, fmt.Errorf("OutcomesDigest: %w", err)
	}
	clause, args := digestPredicate(since, until, scope)
	rows, err := q.QueryContext(ctx, outcomeDigestQuery(clause), args...)
	if err != nil {
		return Digest{}, fmt.Errorf("OutcomesDigest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	h := sha256.New()
	buf := bytes.NewBuffer(make([]byte, 0, digestRowBytesHint))
	writeLengthPrefixed(buf, digestDomainFor(digestDomainOutcomes, scope))
	foldFrames(h, buf)

	var n int64
	for rows.Next() {
		var r outcomeDigestRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Developer, &r.Repo, &r.IssueID,
			&r.Weight, &r.Quality, &r.WorkType, &r.Source,
			&r.PRNumber, &r.MergeCommitSHA); err != nil {
			return Digest{}, fmt.Errorf("OutcomesDigest: scan: %w", err)
		}
		appendOutcomeFrames(buf, r)
		foldFrames(h, buf)
		n++
	}
	if err := rows.Err(); err != nil {
		return Digest{}, fmt.Errorf("OutcomesDigest: %w", err)
	}
	return Digest{Value: finishDigest(h), Rows: n}, nil
}

// appendTokenEventFrames writes one row as digestFramesPerTokenEvent
// length-prefixed frames, reusing #713's writeLengthPrefixed verbatim.
//
// 🔴 THE INJECTIVITY ARGUMENT, INHERITED FROM canonicalPriceTableBytes AND JUST
// AS FRAGILE HERE. Distinct row sets must produce distinct bytes or the
// tamper-evidence claim is false. A length-prefixed stream is uniquely decodable
// into a frame sequence for ARBITRARY payload bytes — which matters because
// developer, repo, issue_id and model are user-controlled and can contain any
// separator a concatenating serializer might have picked. Because each row emits
// exactly digestFramesPerTokenEvent frames, that sequence regroups into rows
// unambiguously.
//
// ⚠️ That middle step is what breaks first. Wrap any line below in a condition —
// `if r.Host != "" { … }`, the sort of "skip the empty fields" tidy-up that looks
// free — and rows become variable-arity, the stream stops being groupable, and
// two distinct windows CAN collide. Nothing about the digest looks wrong when
// that happens. TestDigestFramingIsUniquelyDecodable decodes the stream and pins
// the arity, which is the assertion this paragraph actually makes.
func appendTokenEventFrames(buf *bytes.Buffer, r tokenEventDigestRow) {
	writeLengthPrefixed(buf, strconv.FormatInt(r.ID, 10))
	writeLengthPrefixed(buf, r.TS)
	writeLengthPrefixed(buf, r.Developer)
	writeLengthPrefixed(buf, r.Repo)
	writeLengthPrefixed(buf, r.IssueID)
	writeLengthPrefixed(buf, r.Model)
	writeLengthPrefixed(buf, r.Host)
	writeLengthPrefixed(buf, strconv.FormatInt(r.InputTok, 10))
	writeLengthPrefixed(buf, strconv.FormatInt(r.OutputTok, 10))
	writeLengthPrefixed(buf, strconv.FormatInt(r.CacheRead, 10))
	writeLengthPrefixed(buf, strconv.FormatInt(r.CacheWrite5m, 10))
	writeLengthPrefixed(buf, strconv.FormatInt(r.CacheWrite1h, 10))
	writeLengthPrefixed(buf, strconv.FormatInt(r.CostMicro, 10))
	writeLengthPrefixed(buf, strconv.FormatInt(r.PriceVersion, 10))
	writeLengthPrefixed(buf, r.BillingMode)
	writeLengthPrefixed(buf, r.Source)
	writeLengthPrefixed(buf, r.Fidelity)
}

// appendOutcomeFrames is the outcomes counterpart. Same fixed-arity contract as
// appendTokenEventFrames — read its doc block before touching this one.
//
// weight and quality go through canonPriceFloat, #713's canonical float
// renderer, REUSED rather than re-derived: strconv.FormatFloat(f,'x',-1,64) is
// the exact, shortest round-tripping hexadecimal form — platform-independent and
// lossless, so two weights differing in the last ulp digest differently, which
// no 'f'/'g' precision guarantees — with negative zero collapsed to positive
// zero so an arithmetically identical value cannot false-mismatch.
func appendOutcomeFrames(buf *bytes.Buffer, r outcomeDigestRow) {
	writeLengthPrefixed(buf, strconv.FormatInt(r.ID, 10))
	writeLengthPrefixed(buf, r.TS)
	writeLengthPrefixed(buf, r.Developer)
	writeLengthPrefixed(buf, r.Repo)
	writeLengthPrefixed(buf, r.IssueID)
	writeLengthPrefixed(buf, canonPriceFloat(r.Weight))
	writeLengthPrefixed(buf, canonPriceFloat(r.Quality))
	writeLengthPrefixed(buf, r.WorkType)
	writeLengthPrefixed(buf, r.Source)
	writeLengthPrefixed(buf, strconv.FormatInt(r.PRNumber, 10))
	writeLengthPrefixed(buf, r.MergeCommitSHA)
}

// foldFrames folds buf's accumulated frames into the running hash and empties
// buf for the next row. This is what makes the digest streaming: exactly one
// row's frames are resident at a time.
//
// Ignoring the Write error is safe by the INTERFACE's own contract, not by a
// claim about the caller: hash.Hash documents "It never returns an error" for
// the embedded io.Writer. That is the distinction writeLengthPrefixed's doc
// block draws when it refuses io.Writer — there, "cannot fail" would have been a
// property of whichever writer a future caller passed, enforced by nothing.
func foldFrames(h hash.Hash, buf *bytes.Buffer) {
	h.Write(buf.Bytes()) //nolint:errcheck // hash.Hash.Write never returns an error
	buf.Reset()
}

// finishDigest renders the running hash as a scheme-tagged value.
func finishDigest(h hash.Hash) string {
	return digestScheme + ":" + hex.EncodeToString(h.Sum(nil))
}
