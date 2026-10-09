package store

// price_table_registry (#714): bind each price-table VERSION integer to the
// #713 content identity of the table that was actually served under it, and
// refuse at startup when one integer would come to denote two different tables.
//
// Read the schema comment on price_table_registry in store.go first — it carries
// the WHY, the "no bypass flag, ever" rule, the UNKNOWN-stays-UNKNOWN posture and
// its limits, and the reason uniqueness is an INDEX and not a table-level primary
// key. This file is the mechanism.
//
// THE TWO LAYERS, AND WHY THERE ARE TWO:
//
//   - Layer 1 lives in LoadPriceTable (prices.go). It catches the collision
//     BEFORE any database is involved at all — an override that reuses the
//     EMBEDDED table's version with different content — so a store-less command
//     (`tierd score-log`) is guarded too. It compares against a table compiled
//     into this binary, so it needs no state and cannot be escaped by pointing
//     --db somewhere else.
//   - Layer 2 is recordPriceTableIdentity below, called from Open's Phase 1.5.
//     It catches everything layer 1 structurally cannot see: two DIFFERENT
//     override files sharing a version, an override edited between two boots, or
//     an embedded table that changed under a version some earlier binary already
//     served into this database. Its memory is the database itself.
//
// Neither subsumes the other: layer 1 has no database, layer 2 has no knowledge
// of the embedded table's identity beyond what was recorded. Deleting either one
// leaves a real collision class unguarded.
//
// ⚠️ ONE CLASS IS CAUGHT BY NEITHER, DELIBERATELY. Two DIFFERENT override files
// sharing a NON-embedded version, driven only through a store-less command
// (`tierd score-log --prices A` then `--prices B`, both `version: 1000`), is
// invisible to layer 1 (which compares only against the embedded table) and
// unreachable by layer 2 (there is no database). Closing it would need per-user
// persistent state outside any database, which is a worse disease than the cure.
// It is stated in docs/reference-price-table.md §9 rather than left as an
// inference from the guard table's phrasing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/tiermetric/tier/internal/logsafe"
)

// toolVersion is the binary identity stamped into a price_table_registry row's
// tool_version column: WHICH tierd first served this price-table version into
// this database. Empty until a caller installs one.
//
// ⚠️ THE atomic HERE IS NOT A CLAIM THAT THIS IS THE IMPORTANT GLOBAL — IT IS
// NOT, AND A REVIEW CAUGHT THE ASYMMETRY. This value is PURE PROVENANCE: the
// collision guard compares table_hash alone and never reads it. The global that
// actually gates Open is activePriceTableInfo (prices.go), which is a plain var
// protected only by the write-once-before-serve discipline documented on
// priceTable — the same discipline, now load-bearing for whether Open SUCCEEDS
// rather than for a log line. If you are hardening one of these two, harden that
// one. The atomic is here only because a test swaps this value while another
// test's Open is running, and -race is unforgiving about that.
var toolVersion atomic.Value // string

// SetToolVersion installs the build identity recorded on any price_table_registry
// row this process writes (#714). Call it ONCE at startup, before store.Open.
//
// 🔑 IT IS A PACKAGE SETTER, NOT AN Open PARAMETER, AND THAT IS DELIBERATE:
// threading a provenance string through every Open call site to serve one audit
// column would be a large, mechanical, review-noisy diff for no behavioural gain.
// Measured 2026-08-28: 58 `store.Open(` sites outside this package plus 94
// in-package `= Open(` sites — but that total is dominated by TESTS. The
// PRODUCTION surface is five commands (serve, reprice, backfill, repair-repo,
// demo). Both numbers matter and neither alone is honest: the first is the
// diff-size argument for the setter, the second is the operational blast radius
// of anything that changes Open's behaviour. Mirrors SetUnknownModelRecorder.
//
// Unset is not an error. The column is provenance, not identity: a registry row
// written by a binary that never called this records tool_version 'unknown',
// which is honest — nothing about the COLLISION GUARD depends on it.
func SetToolVersion(v string) { toolVersion.Store(v) }

// currentToolVersion returns the installed build identity, or the explicit
// "unknown" sentinel. It is never the empty string: an empty tool_version in a
// ledger reads as "the column was not populated", which is indistinguishable
// from a bug, whereas "unknown" is a claim the schema can carry NOT NULL.
func currentToolVersion() string {
	if v, ok := toolVersion.Load().(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// PriceTableRegistryRow is one recorded price table — the identity a version
// integer was bound to the first time this deployment served it.
//
// FirstSeen is a DISPLAY STRING, not a timestamp, and the distinction matters.
// The column is written by SQLite's CURRENT_TIMESTAMP default (UTC), and
// modernc.org/sqlite recognises the DATETIME declared type and hands the driver's
// own rendering back on Scan — measured as RFC3339 "2026-08-29T00:01:23Z", not
// the 'YYYY-MM-DD HH:MM:SS' text stored on disk. It is kept as a string
// deliberately: it is only ever shown to an operator, and exposing it as a
// time.Time would invite a caller to bind it back into a range or keyset
// comparison against the STORED form — the CURRENT_TIMESTAMP binding hazard this
// codebase has already been bitten by on quality_history.ts, where a bound
// time.Time silently matches zero rows.
type PriceTableRegistryRow struct {
	id            int64 // existing database identity, kept private from operator output
	Version       int
	TableHash     string
	FileHash      string
	EffectiveDate string
	ModelCount    int
	Source        string
	ToolVersion   string
	FirstSeen     string
	// ForgottenAt and ForgottenBy are empty on a LIVE row. On a forgotten one they
	// are the evidence the maintainer's ruling exists to preserve: this database can still
	// answer "version N once meant table_hash X, forgotten at T by U". ForgottenBy
	// is SELF-ASSERTED — see the schema comment.
	ForgottenAt string
	ForgottenBy string
}

// Forgotten reports whether this recorded identity has been retired by
// `tierd prices forget-version --commit`. A forgotten row is still readable, and
// still answers what the version once meant; it simply no longer binds the guard.
func (r PriceTableRegistryRow) Forgotten() bool { return r.ForgottenAt != "" }

// migratePriceTableRegistry converges the registry table to its current shape and
// creates the partial unique index. It runs at the TOP of Phase 1.5, before
// anything reads the table.
//
// 🔴 WHY IT IS NOT IN schemaTables, AND WHY IT CANNOT BE. Phase 1.5 SELECTs named
// columns from price_table_registry, and Phase 1's CREATE TABLE IF NOT EXISTS is a
// NO-OP on a database that already holds the table in its pre-soft-delete shape.
// On such a database the columns would still be missing at the moment the guard
// reads them, and every Open would fail. The partial index has the same problem
// one level down: `WHERE forgotten_at IS NULL` cannot be compiled before the
// column exists, so creating it in Phase 1 would fail with "no such column".
// Both therefore live here, in this order: ADD COLUMN, swap the index, then (in
// the caller) read. This is the concrete instance of the warning carried on the
// price_table_registry schema — every future column on that table faces exactly
// the same ordering constraint.
//
// Convergent by construction: on a FRESH database schemaTables already created
// both columns, so the ALTERs no-op and only the index work happens. On an
// upgraded one the ALTERs add them with a NULL default, which is precisely
// "live" — every already-recorded identity stays live, which is correct, because
// nothing has been forgotten.
func migratePriceTableRegistry(db *sql.DB) error {
	// DATETIME/TEXT with no NOT NULL and no DEFAULT: existing rows get NULL,
	// i.e. LIVE. That is the convergent default — it matches the CREATE TABLE in
	// schemaTables, so fresh and upgraded databases end up identical.
	if err := addColumnIfMissing(db, "price_table_registry", "forgotten_at", "DATETIME"); err != nil {
		return fmt.Errorf("migrate price_table_registry forgotten_at: %w", err)
	}
	if err := addColumnIfMissing(db, "price_table_registry", "forgotten_by", "TEXT"); err != nil {
		return fmt.Errorf("migrate price_table_registry forgotten_by: %w", err)
	}
	// Drop the pre-soft-delete TOTAL unique index if it is still there. Leaving it
	// would silently defeat the whole ruling: it enforces uniqueness on (version)
	// across forgotten rows too, so the next Open after a forget could not record
	// the replacement identity and the escape hatch would be unusable. IF EXISTS,
	// because a fresh database never had it.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_price_table_registry_version`); err != nil {
		return fmt.Errorf("drop pre-soft-delete price_table_registry index: %w", err)
	}
	if _, err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_price_table_registry_live_version
		     ON price_table_registry(version) WHERE forgotten_at IS NULL`,
	); err != nil {
		return fmt.Errorf("create price_table_registry live-version index: %w", err)
	}
	return nil
}

// hashScheme returns the scheme tag of a scheme-tagged digest ("tierpt1" from
// "tierpt1:<hex>"), or "" when there is no tag.
func hashScheme(h string) string {
	if i := strings.Index(h, ":"); i >= 0 {
		return h[:i]
	}
	return ""
}

// recordPriceTableIdentity is layer 2 of the #714 collision guard, called from
// Open's Phase 1.5 — after the schema exists and BEFORE any phase that prices or
// stamps a row (see the call site; the ordering is load-bearing).
//
// Outcomes:
//
//	absent          -> INSERT this version's identity
//	hash matches    -> no-op (the overwhelmingly common path: every later boot)
//	scheme differs  -> ACCEPT — a re-canonicalization is not tampering, see below
//	hash differs    -> return an error; Open aborts
//
// Only table_hash is compared. file_hash and source are RECORDED but never
// compared: a comment-only edit to the YAML moves file_hash while the resolved
// rates are identical, and the same table loaded from two paths is the same
// table. Making either one binding would turn a harmless edit into a startup
// outage and train operators to route around the guard.
//
// 🔴 THE SCHEME-TAG ARM IS NOT AN ESCAPE HATCH — IT IS THE WHOLE REASON #713 TAGS
// THE HASH, AND A REVIEW CAUGHT THAT THE FIRST DRAFT IGNORED IT. priceTableHash
// returns "tierpt1:<hex>", and canonicalPriceTableBytes' own comment instructs a
// future author to bump priceTableHashScheme in the same commit as any layout
// change "so a downstream guard can tell a re-scheme from tampering". THIS IS
// THAT DOWNSTREAM GUARD. Comparing the whole tagged string would mean the first
// legitimate fix to the canonicalization makes EVERY recorded version in EVERY
// database collide simultaneously, with the only remedy being to forget-version
// the entire registry away — a serialization bugfix would brick the fleet and the
// prescribed recovery would destroy the very records this table exists to hold.
// So a scheme MISMATCH is "not comparable", not "not equal": we accept, and we
// deliberately do NOT rewrite the stored row, because silently restamping it
// under the new scheme is exactly the rebinding this feature refuses. The guard
// is inert for that version until an operator re-records it, and that is the
// correct trade — availability plus an honest record, over a confident
// comparison between two values that were never computed the same way.
func recordPriceTableIdentity(db *sql.DB, path string, info PriceTableInfo) error {
	// A zero/blank identity means the caller has no active price table, which
	// package init makes impossible for real callers — so this is a programmer
	// error, and recording a row with an empty table_hash would poison the very
	// column the guard compares. Fail loud rather than silently skip: a guard
	// that quietly does nothing when its input is malformed is not a guard.
	if info.Version < 1 || info.TableHash == "" {
		return fmt.Errorf("record price table identity: no active price table (version=%d, table_hash=%s) — store.Open requires a loaded price table", info.Version, logsafe.Str(info.TableHash))
	}
	stored, found, err := readPriceTableRegistry(db, info.Version)
	if err != nil {
		return err
	}
	// Test-only seam. A concurrent recorder has to land BETWEEN the read above and
	// the insert below to exercise the ON CONFLICT arm and the re-read, and that
	// window cannot be hit reliably from outside: a naive parallel-Open test fails
	// earlier and elsewhere (measured — db.Ping "database is locked" and a
	// UNIQUE violation on tier_migrations), so it would prove nothing about this
	// code. nil in production; see TestRecordPriceTableIdentity_ConcurrentInsert.
	if testHookAfterRegistryRead != nil {
		testHookAfterRegistryRead()
	}
	if !found {
		// ON CONFLICT DO NOTHING, not a bare INSERT: two tierd processes can open
		// the same database file concurrently, and between the SELECT above and
		// this INSERT the other one may have recorded the same version. The unique
		// index makes that a constraint violation, which must not be a startup
		// failure when the recorded table AGREES with ours — so we swallow the
		// conflict here and let the re-read below adjudicate on content.
		if _, err := db.Exec(
			`INSERT INTO price_table_registry
			     (version, table_hash, file_hash, effective_date, model_count, source, tool_version)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(version) WHERE forgotten_at IS NULL DO NOTHING`,
			info.Version, info.TableHash, info.FileHash, info.EffectiveDate,
			info.ModelCount, info.Source, currentToolVersion(),
		); err != nil {
			return fmt.Errorf("record price table version %d: %w", info.Version, err)
		}
		// Re-read unconditionally. On the ordinary path this returns the row we
		// just wrote and the comparison below is trivially true; on the racing
		// path it returns the OTHER process's row, and a divergent one is refused
		// exactly as if it had been there all along.
		stored, found, err = readPriceTableRegistry(db, info.Version)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("record price table version %d: the row was not recorded and is not present (concurrent delete?)", info.Version)
		}
	}
	if stored.TableHash == info.TableHash {
		return nil
	}
	// Not comparable across canonicalization schemes — see the long note above.
	if hashScheme(stored.TableHash) != hashScheme(info.TableHash) {
		return nil
	}
	return newPriceTableCollisionError(path, info, stored)
}

// testHookAfterRegistryRead runs between recordPriceTableIdentity's read and its
// insert. It is nil in every production path and exists only so the concurrent
// recorder case has a deterministic window; see its call site.
var testHookAfterRegistryRead func()

// newPriceTableCollisionError builds the refusal. It is a function, and its text
// is deliberately long, because THE MESSAGE IS THE REMEDY: the operator meets
// this at startup, on a fail-closed path with no override flag, and everything
// they need to act — which database, which version, which two hashes, where the
// recorded one came from, when, and the way out — has to be in the one line they
// get. An error that says only "price table mismatch" is how a fail-closed guard
// ends up commented out.
//
// 🔴 THE forget-version INVOCATION MUST BE COPY-PASTEABLE, AND FOUR INDEPENDENT
// REVIEWS CAUGHT THAT IT WAS NOT. The first draft printed
// "tierd prices forget-version 9 --commit" — a POSITIONAL version, which the
// command does not accept: Go's flag package stops parsing at the first non-flag
// argument, so --commit was never seen either and the line failed with
// "--version is required" (measured, rc=1). A remedy that does not run is worse
// than no remedy: the operator concludes the escape hatch is broken at the one
// moment they need it. TestForgetVersionRemedyIsRunnable now feeds the string
// this function produces to the REAL FlagSet, so the two cannot drift again.
//
// It also names `path`, which every other Open failure does (the schema-version
// gate, the chmod sweep, `create db file`) and the first draft did not: on a host
// running a prod and a staging database, "point --db at a different file" is
// unactionable until you know which file you are pointed at now.
func newPriceTableCollisionError(path string, active PriceTableInfo, stored PriceTableRegistryRow) error {
	// 🔑 THE EMBEDDED-vs-EMBEDDED CASE NEEDS ITS OWN SENTENCE, because for it the
	// LEADING remedy is impossible. "Bump 'version:'" assumes the operator can
	// edit the table; when both sides are the compiled-in default they cannot —
	// the table is inside the binary they were shipped. Left unsaid, the message
	// steers them straight to forget-version, i.e. to destroying the record as a
	// workaround for OUR release mistake. TestEmbeddedPriceTableIdentityIsPinned
	// is what is supposed to stop such a release ever shipping.
	remedy := "Bump 'version:' in the price table (this is the fix in almost every case), point --db at a different file, or — only if the RECORDED row is the wrong one — " +
		fmt.Sprintf("run 'tierd prices forget-version --db %s --version %d --commit' to drop it", logsafe.Str(path), active.Version)
	if stored.Source == PriceSourceEmbedded && active.Source == PriceSourceEmbedded {
		remedy = "BOTH tables are this binary's EMBEDDED default, so you cannot bump 'version:' yourself — this is a tierd release that changed rates without bumping the table version. Report it. Do NOT run forget-version to work around it: that destroys the only record of what this version meant. Downgrade to the previous tierd, or point --db at a different file"
	}
	return fmt.Errorf(
		"database %s: price table version %d in this binary hashes table_hash %s but this database already recorded version %d as table_hash %s "+
			"(recorded %s from source %s by %s) — two different price tables share one version number, and every row stamped price_version=%d "+
			"under each is indistinguishable forever. %s. There is deliberately no flag that accepts the collision",
		logsafe.Str(path),
		active.Version, logsafe.Str(active.TableHash),
		stored.Version, logsafe.Str(stored.TableHash),
		logsafe.Str(stored.FirstSeen), logsafe.Str(stored.Source), logsafe.Str(stored.ToolVersion),
		active.Version, remedy,
	)
}

// readPriceTableRegistry returns the LIVE recorded identity for one version. The
// second result distinguishes "no row" from "a row" so callers never have to read
// absence out of a zero value.
//
// 🔴 "LIVE" IS THE WHOLE POINT OF THE PREDICATE. A forgotten row must not make the
// guard refuse — that is what forgetting means — but it must still be READABLE as
// evidence, which is why it is soft-deleted rather than removed. Callers that want
// the history (ListPriceTableRegistry, and the operator commands) deliberately do
// NOT use this function.
//
// A missing TABLE is deliberately NOT translated here: Open's caller wants the
// real driver error (the table always exists by Phase 1.5, so its absence is a
// genuine fault). ForgetPriceTableVersion, whose caller is an operator with a
// possibly-mistyped --db, maps it itself.
func readPriceTableRegistry(q rowQueryer, version int) (PriceTableRegistryRow, bool, error) {
	row := PriceTableRegistryRow{Version: version}
	err := q.QueryRow(
		`SELECT id, table_hash, file_hash, effective_date, model_count, source, tool_version, first_seen
		   FROM price_table_registry WHERE version = ? AND forgotten_at IS NULL`, version,
	).Scan(&row.id, &row.TableHash, &row.FileHash, &row.EffectiveDate, &row.ModelCount,
		&row.Source, &row.ToolVersion, &row.FirstSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return PriceTableRegistryRow{}, false, nil
	}
	if err != nil {
		return PriceTableRegistryRow{}, false, fmt.Errorf("read price table registry version %d: %w", version, err)
	}
	return row, true, nil
}

// rowQueryer is the one method readPriceTableRegistry needs, so it composes with
// both a *sql.DB and the raw handle the operator commands open.
type rowQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ErrNoPriceTableRegistryRow reports that the version an operator asked about has
// no recorded identity — either the row is absent, or the table itself does not
// exist because no identity-recording tierd has ever opened this database. It is
// a distinct error so the CLI can say "nothing to forget" instead of "failed",
// which are different operator situations.
var ErrNoPriceTableRegistryRow = errors.New("no price_table_registry row for that version")

// isMissingRegistryTable reports whether err is SQLite's "no such table" for the
// registry — i.e. this database was never opened by an identity-recording tierd.
func isMissingRegistryTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: price_table_registry")
}

// openRegistryDB opens the database at path DIRECTLY, bypassing store.Open.
//
// ⚠️ THE BYPASS IS THE POINT, AND IT IS BROADER THAN "SKIPS MIGRATIONS". These
// commands must reach a database Open is currently REFUSING, so they skip all of
// Open: the migration chain AND the #141 refuse-if-newer schema gate. That means
// an OLD binary can read or forget a registry row in a database a NEWER tierd
// migrated. Acceptable here and only here, because both operations touch exactly
// one table whose shape this binary fully understands and which no migration has
// ever altered — but it is not a licence to add more bypassing commands.
//
// They also do not inherit Open's other post-conditions: no pool sizing, no
// migration, no #130 0600 chmod sweep over the db and its -wal/-shm sidecars.
// Only the DSN is shared, and only so the two paths cannot drift on the two
// connection-scoped pragmas that matter.
//
// readOnly selects `?mode=ro`, and that is what makes a DRY RUN literally mutate
// nothing: sqliteDSN carries journal_mode(WAL), and WAL is PERSISTENT — so
// connecting read-write to a delete-mode file (a restored backup an operator
// points --db at) converts it and creates sidecars. A command that claims to
// change nothing must not do that.
func openRegistryDB(path string, readOnly bool) (*sql.DB, error) {
	// Refuse a path that does not exist rather than letting sql.Open conjure an
	// empty database and report "nothing to forget" — which would look like a
	// successful no-op to an operator who simply mistyped --db.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open database %s: %w", logsafe.Str(path), err)
	}
	dsn := sqliteDSN(path)
	if readOnly {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("open database %s: %w", logsafe.Str(path), err)
		}
		dsn = ReadOnlyURI(abs, "mode=ro")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", logsafe.Str(path), err)
	}
	return db, nil
}

// ListPriceTableRegistry returns every recorded price-table identity in this
// database, ascending by version.
//
// 🔑 IT EXISTS BECAUSE A GUARD YOU CANNOT INSPECT IS A GUARD YOU CAN ONLY ESCAPE.
// Three independent reviews landed on the same gap: before this, the ONLY way to
// see what a version was bound to was a dry run of the DELETE command — a poor
// thing to make an operator reach for while diagnosing a refused startup, and it
// can only show a version whose number you already know. Read access is also what
// lets an operator check a database BEFORE upgrading, rather than discovering a
// collision when the daemon will not come up.
//
// Like ForgetPriceTableVersion it opens the database directly and READ-ONLY, so
// it works on a database Open is currently refusing.
func ListPriceTableRegistry(ctx context.Context, path string) ([]PriceTableRegistryRow, error) {
	db, err := openRegistryDB(path, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	// Ordered by (version, first_seen) and INCLUDING forgotten rows: a version may
	// legitimately have several — one live identity plus every identity previously
	// forgotten under that number — and reading them in the order they were
	// recorded is what makes the history legible.
	rows, err := db.QueryContext(ctx,
		`SELECT version, table_hash, file_hash, effective_date, model_count, source, tool_version, first_seen,
		        COALESCE(forgotten_at, ''), COALESCE(forgotten_by, '')
		   FROM price_table_registry ORDER BY version, first_seen, id`)
	if err != nil {
		if isMissingRegistryTable(err) {
			return nil, fmt.Errorf("%s has no price_table_registry table — no identity-recording tierd has ever opened it: %w", path, ErrNoPriceTableRegistryRow)
		}
		return nil, fmt.Errorf("list price table registry in %s: %w", logsafe.Str(path), err)
	}
	defer func() { _ = rows.Close() }()
	var out []PriceTableRegistryRow
	for rows.Next() {
		var r PriceTableRegistryRow
		if err := rows.Scan(&r.Version, &r.TableHash, &r.FileHash, &r.EffectiveDate,
			&r.ModelCount, &r.Source, &r.ToolVersion, &r.FirstSeen,
			&r.ForgottenAt, &r.ForgottenBy); err != nil {
			return nil, fmt.Errorf("scan price table registry row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ForgetPriceTableVersion is THE ESCAPE HATCH for the fail-closed guard above. It
// RETIRES one recorded price-table identity so a refused Open can proceed —
// without destroying the record of what that version meant.
//
// 🔑 WHY IT EXISTS AT ALL. A fail-closed guard whose only remedy is "hand-edit
// production SQLite" is a guard that gets commented out the first time it fires
// at 3am. This is the sanctioned, in-binary, documented way out.
//
// 🔴 IT IS A SOFT DELETE PLUS AN AUDIT ROW, BY RULING, AND THE FIRST VERSION OF
// THIS FUNCTION WAS WRONG. It hard-DELETEd. A security review found that although
// that beat a `--accept-rehash` flag on REACHABILITY — out-of-band and deliberate,
// versus every boot and silent — it was equivalent on EVIDENCE: afterwards the
// database held the new hash under a reused version with no trace that another
// table had ever claimed it, which is precisely the end state a bypass flag
// produces. The maintainer's ruling: "this whole program exists so provenance is not lost.
// An escape hatch that erases the record of its own use contradicts the thing it
// protects." So the row is RETAINED and stamped (forgotten_at, forgotten_by), and
// a price_forget_audit row records the act. After this call the database can still
// answer: version N once meant table_hash X, forgotten at T by U.
//
// ⇒ It is still not a way to keep two real tables under one version. The remedy
// for that is, always, bump the version. What changed is that using this hatch now
// leaves a mark instead of a hole.
//
// ⚠️ IT OPENS THE DATABASE DIRECTLY, NOT THROUGH Open, AND IT MUST — the whole
// point is to reach a database Open is currently REFUSING. See openRegistryDB for
// the full scope of that bypass.
//
// actor is recorded as forgotten_by and in the ledger. It is SELF-ASSERTED: a
// local CLI has no authenticated principal to bind it to, so nothing validates it
// (the same honesty cost_correction_audit.actor carries). Empty is rejected rather
// than silently stored, because an unattributed entry in an attribution ledger is
// worse than no ledger.
//
// confirm is called with the row AFTER it is read and BEFORE anything is written.
// If it returns an error, NOTHING is written and that error is returned. This is
// what makes "prints the row it retires" structurally true rather than a comment:
// the caller renders the evidence there and reports a failed write, so a closed
// stdout, a full disk, or an EPIPE from `| head` aborts the operation instead of
// silently completing it. confirm may be nil only when the caller genuinely has no
// evidence to present (store-level tests).
//
// ⚠️ HOW THE ABORT IS PROVEN, AND HOW IT IS NOT. The guarantee is pinned by
// TestForgetPriceTableVersion_ConfirmFailureAbortsTheWrite (store) and
// TestRunPricesForgetVersion_UnwritableStdoutAbortsTheDelete (through dispatch,
// with a writer that fails), plus a mutation that ignores confirm — killed by tests
// in both packages. Do NOT try to demonstrate it from a shell on macOS: every
// obvious probe is a false witness there, measured. `>&-` closes fd 1 and the
// process's next open() REUSES it, so the write "succeeds" into an unrelated
// descriptor and no guard can see it. `| head -1` buffers the whole row into the
// 64KiB pipe before head exits, so the evidence really did land. `ulimit -f 0`
// kills the entire subshell with SIGXFSZ before the comparison means anything (the
// control — a bare `echo > file` — dies the same way).
//
// Returns the row that was (or, on a dry run, would be) retired.
func ForgetPriceTableVersion(ctx context.Context, path string, version int, commit bool, actor string, confirm func(PriceTableRegistryRow) error) (PriceTableRegistryRow, error) {
	if version < 1 {
		return PriceTableRegistryRow{}, fmt.Errorf("price table version must be >= 1, got %d", version)
	}
	if commit && strings.TrimSpace(actor) == "" {
		return PriceTableRegistryRow{}, errors.New("forget price table version: an actor is required — the ledger records WHO retired an identity, and an unattributed entry in an attribution ledger is worse than none")
	}
	// The read is read-only even on the --commit path; the write below reopens
	// read-write. A dry run therefore never opens the file for writing at all.
	roDB, err := openRegistryDB(path, true)
	if err != nil {
		return PriceTableRegistryRow{}, err
	}
	if err := roDB.PingContext(ctx); err != nil {
		_ = roDB.Close()
		return PriceTableRegistryRow{}, fmt.Errorf("connect database %s: %w", logsafe.Str(path), err)
	}
	row, found, err := readPriceTableRegistry(roDB, version)
	_ = roDB.Close()
	if err != nil {
		// A database that predates this feature has no such table. That is the
		// likeliest operator mistake here (a wrong --db, an old backup), and a raw
		// driver string is the wrong thing to hand someone already dealing with a
		// refused startup.
		if isMissingRegistryTable(err) {
			return PriceTableRegistryRow{}, fmt.Errorf("%s has no price_table_registry table — no identity-recording tierd has ever opened it, so there is nothing to forget: %w", path, ErrNoPriceTableRegistryRow)
		}
		return PriceTableRegistryRow{}, err
	}
	if !found {
		// Deliberately reported as "nothing to forget" even when a FORGOTTEN row
		// exists for this version: forgetting is idempotent from the operator's
		// point of view, and re-stamping an already-retired identity would falsify
		// its forgotten_at. `tierd prices list` shows the retired row.
		return PriceTableRegistryRow{}, fmt.Errorf("version %d in %s: %w", version, path, ErrNoPriceTableRegistryRow)
	}
	// Hand the evidence to the caller and REQUIRE it to have landed. Everything
	// after this point mutates the database.
	if confirm != nil {
		if err := confirm(row); err != nil {
			return PriceTableRegistryRow{}, fmt.Errorf("refusing to retire the price_table_registry row for version %d: its contents could not be reported first (%w) — nothing was changed", version, err)
		}
	}
	if !commit {
		return row, nil
	}
	forgetID, err := newAuditID()
	if err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("mint forget id: %w", err)
	}
	db, err := openRegistryDB(path, false)
	if err != nil {
		return PriceTableRegistryRow{}, err
	}
	defer func() { _ = db.Close() }()

	// ONE TRANSACTION. The stamp and the ledger row commit together or not at all:
	// a retired identity with no ledger entry, or a ledger entry for an identity
	// still live, would each be a worse record than either alone. Same discipline
	// reprice uses for its row updates and audit rows.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("begin forget price table version %d: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// Compare the confirmed row's identity, not just its reusable version. If
	// another operator retires it and registers a replacement before this write,
	// we must neither retire the replacement nor ledger the old row for that act.
	res, err := tx.ExecContext(ctx,
		`UPDATE price_table_registry
		    SET forgotten_at = CURRENT_TIMESTAMP, forgotten_by = ?
		  WHERE id = ? AND forgotten_at IS NULL`, actor, row.id)
	if err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("forget price table version %d in %s: %w", version, logsafe.Str(path), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("forget price table version %d in %s: could not confirm the update: %w", version, logsafe.Str(path), err)
	}
	if n == 0 {
		return PriceTableRegistryRow{}, fmt.Errorf("version %d in %s: %w (it was retired between the read and the write)", version, path, ErrNoPriceTableRegistryRow)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO price_forget_audit
		     (forget_id, version, table_hash, file_hash, price_effective_date, model_count,
		      source, recorded_tool_version, first_seen, actor, tool_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		forgetID, row.Version, row.TableHash, row.FileHash, row.EffectiveDate, row.ModelCount,
		row.Source, row.ToolVersion, row.FirstSeen, actor, currentToolVersion(),
	); err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("record price_forget_audit for version %d: %w", version, err)
	}
	// Re-read so the caller reports the row as it now STANDS, with the stamps the
	// database actually wrote. Returning the pre-update copy would have the command
	// print forgotten_at as empty on the one path where it is the whole point.
	// Read this identity inside the transaction so a later retirement of the same
	// version cannot substitute its row in the result.
	stamped, err := readForgottenPriceTableRegistry(tx, version, row.id)
	if err != nil {
		return PriceTableRegistryRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("commit forget price table version %d: %w", version, err)
	}
	return stamped, nil
}

// readForgottenPriceTableRegistry reads back the row this run just retired. It
// keys on the confirmed id, because a version may be retired more than once.
func readForgottenPriceTableRegistry(q rowQueryer, version int, id int64) (PriceTableRegistryRow, error) {
	row := PriceTableRegistryRow{id: id, Version: version}
	err := q.QueryRow(
		`SELECT table_hash, file_hash, effective_date, model_count, source, tool_version, first_seen,
		        COALESCE(forgotten_at, ''), COALESCE(forgotten_by, '')
		   FROM price_table_registry
		  WHERE id = ? AND forgotten_at IS NOT NULL`, id,
	).Scan(&row.TableHash, &row.FileHash, &row.EffectiveDate, &row.ModelCount,
		&row.Source, &row.ToolVersion, &row.FirstSeen, &row.ForgottenAt, &row.ForgottenBy)
	if err != nil {
		return PriceTableRegistryRow{}, fmt.Errorf("read back the retired price table registry row for version %d: %w", version, err)
	}
	return row, nil
}

// PriceForgetAuditRow is one entry in the append-only ledger of retired
// price-table identities — the price-identity sibling of a reprice_audit or
// repo_repair_audit row.
type PriceForgetAuditRow struct {
	ForgetID            string
	Version             int
	TableHash           string
	FileHash            string
	EffectiveDate       string
	ModelCount          int
	Source              string
	RecordedToolVersion string
	FirstSeen           string
	Actor               string
	ToolVersion         string
	Timestamp           string
}

// ListPriceForgetAudit returns every recorded forget-version operation, oldest
// first. Read-only, and like the other registry readers it works on a database
// Open is currently refusing.
//
// It exists so the ruling's guarantee is answerable from the ledger as well as
// from the retained row: the two survive different accidents. A later hard DELETE
// of the registry row leaves this intact, which is exactly why it carries its own
// copy of table_hash rather than a foreign key.
func ListPriceForgetAudit(ctx context.Context, path string) ([]PriceForgetAuditRow, error) {
	db, err := openRegistryDB(path, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx,
		`SELECT forget_id, version, table_hash, file_hash, price_effective_date, model_count,
		        source, recorded_tool_version, first_seen, actor, tool_version, ts
		   FROM price_forget_audit ORDER BY ts, id`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: price_forget_audit") {
			return nil, fmt.Errorf("%s has no price_forget_audit table — no identity-recording tierd has ever opened it: %w", path, ErrNoPriceTableRegistryRow)
		}
		return nil, fmt.Errorf("list price forget audit in %s: %w", logsafe.Str(path), err)
	}
	defer func() { _ = rows.Close() }()
	var out []PriceForgetAuditRow
	for rows.Next() {
		var r PriceForgetAuditRow
		if err := rows.Scan(&r.ForgetID, &r.Version, &r.TableHash, &r.FileHash, &r.EffectiveDate,
			&r.ModelCount, &r.Source, &r.RecordedToolVersion, &r.FirstSeen,
			&r.Actor, &r.ToolVersion, &r.Timestamp); err != nil {
			return nil, fmt.Errorf("scan price forget audit row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
