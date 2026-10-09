package store

// #714 guards. Read the schema comment on price_table_registry (store.go) and
// the package comment on pricetableregistry.go for what these are protecting.
//
// 🔴 THE COUPLING RULE THAT MAKES THE REFUSAL ARM MEAN ANYTHING. Mutating the
// comparison in recordPriceTableIdentity to always-pass (i.e. switching the guard
// OFF) MUST redden TestOpen_RefusesCollidingPriceTable while leaving
// TestOpen_RecordsPriceTableRegistryRow GREEN. If both redden, the two tests are
// coupled and the refusal arm proves nothing on its own — it would only be
// re-proving that recording works. Measured: that mutant reddens FOUR tests
// (RefusesColliding, RefusesCollisionBeforeRepricingRows, ForgetPriceTableVersion,
// DoesNotLeakHandleOnPriceTableCollision) and the recording test stays green.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// countTableRows returns the row count of one table via a raw connection
// (bypassing Open, so no migration runs and the read cannot itself mutate).
func countTableRows(t *testing.T, path, table string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %q`, table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestOpenRegistryDB_ReadOnly(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.db")
		db, err := openRegistryDB(path, true)
		if db != nil {
			_ = db.Close()
		}
		if err == nil {
			t.Error("read-only open of missing database succeeded")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing database was created: stat error = %v", err)
		}
	})
	t.Run("write refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "registry.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := openRegistryDB(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if err := db.Ping(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE forbidden (id INTEGER)`); err == nil {
			t.Fatal("write through read-only registry handle succeeded")
		}
	})
}

// readRegistryRow reads one registry row through the PRODUCTION reader. Use it
// only where the point is the value, not the column mapping — see
// readRegistryColumnsByName for why that distinction is load-bearing.
func readRegistryRow(t *testing.T, path string, version int) (PriceTableRegistryRow, bool) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	row, found, err := readPriceTableRegistry(raw, version)
	if err != nil {
		t.Fatalf("readPriceTableRegistry: %v", err)
	}
	return row, found
}

// readRegistryColumnsByName reads each column INDIVIDUALLY, by name, without
// touching the production reader.
//
// 🔴 THIS EXISTS BECAUSE A MEASURED MUTANT SURVIVED WITHOUT IT. A test that
// verifies the INSERT by calling readPriceTableRegistry is a tautology across any
// SYMMETRIC column-order change: swapping file_hash ↔ effective_date in BOTH the
// production INSERT column list AND the production SELECT column list left the
// entire suite green, while every row was persisted with its values in the wrong
// columns. That corruption is invisible until someone reads the database with
// anything other than this code — including `tierd prices list`, and including the
// --json archive that `forget-version` produces, which is the only surviving
// record of what a version meant. Reading one column at a time, by name, is what
// makes the assertion about the DATABASE rather than about a round trip.
func readRegistryColumnsByName(t *testing.T, path string, version int) map[string]string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	out := map[string]string{}
	for _, col := range []string{"table_hash", "file_hash", "effective_date", "model_count", "source", "tool_version", "first_seen"} {
		var v string
		q := fmt.Sprintf(`SELECT CAST(%s AS TEXT) FROM price_table_registry WHERE version = ?`, col)
		if err := raw.QueryRow(q, version).Scan(&v); err != nil {
			t.Fatalf("read %s for version %d: %v", col, version, err)
		}
		out[col] = v
	}
	return out
}

// syntheticPriceTableYAML builds a MINIMAL but valid price table at the given
// version whose one non-fallback model bills at inputPerM. Two calls with the
// same version and different rates are exactly the collision #714 refuses; two
// calls with the same version and the same rate are the same table said twice.
//
// It is built from a template rather than by editing the embedded prices.yaml so
// the fixtures cannot drift when a real rate changes.
func syntheticPriceTableYAML(version int, inputPerM float64) string {
	return fmt.Sprintf(`version: %d
effective_date: "2026-07-26"
models:
  synthetic-model:
    provider: anthropic
    input_per_m: %g
    output_per_m: 15.00
  self-hosted-small:
    provider: self-hosted
    input_per_m: 0.20
    combined: true
  self-hosted-medium:
    provider: self-hosted
    input_per_m: 0.50
    combined: true
  self-hosted-large:
    provider: self-hosted
    input_per_m: 1.00
    combined: true
`, version, inputPerM)
}

// writePriceTable writes a synthetic table to a temp file and returns the path.
//
// It also asserts the fixture's version is NOT the embedded one. Without that
// guard, an embedded `version:` bump onto one of these numbers would make LAYER 1
// fire inside the fixture's own LoadPriceTable, and the layer-2 test would fail
// for a reason that has nothing to do with what it is testing.
func writePriceTable(t *testing.T, dir, name string, version int, inputPerM float64) string {
	t.Helper()
	if emb := embeddedPriceTableInfo.Version; emb == version {
		t.Fatalf("fixture version %d now collides with the EMBEDDED table version — pick another; this test is about layer 2, and layer 1 would fire first", version)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(syntheticPriceTableYAML(version, inputPerM)), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// pinEmbeddedPriceTable loads the embedded default into the package globals NOW
// and restores it afterwards.
//
// 🔑 LOADING, NOT JUST RESTORING. restoreDefaultPriceTable only registers a
// Cleanup, so a test that reads ActivePriceTableInfo() without this is asserting
// against whatever the PREVIOUS test left in a package global. That is the exact
// order-dependence #313 already burned this package on, and it is invisible until
// a shuffled run reorders things.
func pinEmbeddedPriceTable(t *testing.T) {
	t.Helper()
	prevTbl, prevInfo := priceTable, activePriceTableInfo
	tbl, info, err := parseEmbeddedPriceTable()
	if err != nil {
		t.Fatalf("parseEmbeddedPriceTable: %v", err)
	}
	priceTable, activePriceTableInfo = tbl, info
	t.Cleanup(func() { priceTable, activePriceTableInfo = prevTbl, prevInfo })
}

// TestEmbeddedPriceTableIdentityIsPinned is the BUILD-TIME half of #714, and
// without it the feature's premise is honour-system on the one party whose
// mistake has the widest blast radius: us.
//
// 🔴 WHY THIS TEST IS THE HIGHEST-VALUE LINE IN THE CHANGE. Every other guard here
// fires at RUNTIME, on an operator's machine. Nothing else in the tree reddens
// when the EMBEDDED table's resolved content changes while `version:` stays put —
// TestPriceTableHash_GoldenVector pins a frozen synthetic fixture, not this table;
// the embedded-hash tests assert only the tierpt1 SHAPE; and
// TestREADMESamplePriceTableMatchesEmbedded fires when you CHANGE the version,
// never when you fail to. So a maintainer could edit a rate in prices.yaml — or
// change a provider-default multiplier in Go, which moves table_hash with the
// YAML untouched — ship a green `make check`, and every deployment that had
// already recorded that version would REFUSE TO START. For those operators the
// error's leading remedy ("bump 'version:'") is impossible: the table is inside
// the binary they were shipped. Their only in-reach fix would be to destroy the
// registry record. This test is what stops that release existing.
func TestEmbeddedPriceTableIdentityIsPinned(t *testing.T) {
	pinEmbeddedPriceTable(t)

	// Bump BOTH literals together, in the same commit as the rate change.
	const wantVersion = 12
	const wantTableHash = "tierpt1:3a27fb89ee0312ff4b82ac4116840a2ea5f522e5f74f494ee8e9c1fec59edb66"

	got := ActivePriceTableInfo()
	if got.Version != wantVersion || got.TableHash != wantTableHash {
		t.Errorf(`the EMBEDDED price table's identity moved.

  version:    have %d        want %d
  table_hash: have %s
              want %s

If you CHANGED RATES (in internal/store/prices.yaml, or by changing a provider-default
multiplier in prices.go — which moves table_hash with the YAML untouched), you MUST bump
'version:' in prices.yaml IN THE SAME COMMIT, then update both literals above.

If you do not, every deployment that already recorded version %d refuses to start (#714),
and those operators CANNOT bump the version themselves — the table is compiled into the
binary they were shipped.

If you deliberately bumped 'version:', just update both literals above.`,
			got.Version, wantVersion, got.TableHash, wantTableHash, wantVersion)
	}
}

// TestOpen_RecordsPriceTableRegistryRow is the POSITIVE arm: a fresh Open records
// exactly one row for the active table's version, and a SECOND Open of the same
// database records nothing further (the guard is idempotent, not append-per-boot).
//
// It deliberately does NOT touch the collision comparison, which is what lets it
// stay green when that comparison is mutated off — see the coupling rule above.
//
// Every column is read BY NAME, individually, without the production reader — see
// readRegistryColumnsByName for the measured mutant that survived otherwise.
func TestOpen_RecordsPriceTableRegistryRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	pinEmbeddedPriceTable(t)
	want := ActivePriceTableInfo()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if n := countTableRows(t, path, "price_table_registry"); n != 1 {
		t.Fatalf("price_table_registry rows after first Open = %d, want exactly 1", n)
	}
	cols := readRegistryColumnsByName(t, path, want.Version)
	for _, c := range []struct{ col, want string }{
		{"table_hash", want.TableHash},
		{"file_hash", want.FileHash},
		{"effective_date", want.EffectiveDate},
		{"model_count", fmt.Sprintf("%d", want.ModelCount)},
		// Source is PriceSourceEmbedded because no --prices override was applied.
		// This is the real stamp init() installed, not one the test wrote.
		{"source", PriceSourceEmbedded},
	} {
		if cols[c.col] != c.want {
			t.Errorf("column %s = %q, want %q", c.col, cols[c.col], c.want)
		}
	}
	// Guard the guard: empty-vs-empty would satisfy the hash arms above.
	if want.TableHash == "" || want.FileHash == "" {
		t.Fatal("the active table carries no hashes — the assertions above are vacuous")
	}
	// tool_version is never empty: an unset build identity records the explicit
	// "unknown" sentinel, which the NOT NULL column can carry honestly.
	if cols["tool_version"] == "" {
		t.Error("tool_version is empty; want a value (the 'unknown' sentinel at minimum)")
	}
	if cols["first_seen"] == "" {
		t.Error("first_seen is empty; want the CURRENT_TIMESTAMP default")
	}

	// Second Open of the SAME database: the version already matches, so this is
	// the no-op arm. Still exactly one row.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := countTableRows(t, path, "price_table_registry"); n != 1 {
		t.Errorf("price_table_registry rows after second Open = %d, want still exactly 1 (idempotent)", n)
	}
}

// TestOpen_RecordsEachVersionSeparately pins that the registry holds MANY
// versions at once — the unique index is on `version`, not a global one-row
// constraint. Without this arm, a guard that only ever kept a single row would
// pass every other test here.
func TestOpen_RecordsEachVersionSeparately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.db")
	restoreDefaultPriceTable(t)

	for _, v := range []int{4250, 4251} {
		p := writePriceTable(t, dir, fmt.Sprintf("v%d.yaml", v), v, 3.00)
		if _, err := LoadPriceTable(p); err != nil {
			t.Fatalf("LoadPriceTable(%d): %v", v, err)
		}
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open at version %d: %v", v, err)
		}
		_ = db.Close()
	}
	if n := countTableRows(t, path, "price_table_registry"); n != 2 {
		t.Errorf("price_table_registry rows = %d, want 2 (one per version)", n)
	}
	for _, v := range []int{4250, 4251} {
		if _, found := readRegistryRow(t, path, v); !found {
			t.Errorf("no registry row for version %d", v)
		}
	}
}

// TestOpen_RefusesCollidingPriceTable is the NEGATIVE CONTROL. It asserts:
//
//	(1) the Open failed;
//	(2) it failed for the RIGHT REASON — the message names "table_hash" AND the
//	    colliding version, so an unrelated failure ("database is locked", "no such
//	    table") cannot satisfy this test;
//	(3) the refused Open left the schema and the RECORDED IDENTITY untouched.
//
// ⚠️ ARM (3) IS NOT AN ORDERING PROOF, AND AN EARLIER VERSION OF THIS COMMENT
// CLAIMED IT WAS. Measured: moving Phase 1.5 below backfillPriceVersion leaves
// THIS TEST GREEN. It cannot detect ordering — `dumpSchema` compares a database
// whose schema the first boot already applied (so Phase 1/2/3 are no-ops either
// way), and repricing and stamping are UPDATEs, which no row COUNT can see.
// Ordering is proven by TestOpen_RefusesCollisionBeforeRepricingRows alone, which
// is the only test in the suite that kills that mutant. What arm (3) does prove
// is real and worth keeping: a refused Open neither rebinds the stored identity
// nor applies schema.
func TestOpen_RefusesCollidingPriceTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collide.db")
	restoreDefaultPriceTable(t)

	const collidingVersion = 4242
	first := writePriceTable(t, dir, "first.yaml", collidingVersion, 3.00)
	second := writePriceTable(t, dir, "second.yaml", collidingVersion, 3.01)

	// Boot 1: load the first table and record its identity under version 4242.
	infoA, err := LoadPriceTable(first)
	if err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open with the first table: %v", err)
	}
	// Seed a row so the token_events count below is a real comparison rather than
	// 0-vs-0 — an empty-set assertion that cannot fail for any reason.
	if err := db.InsertTokenEvents(context.Background(), []TokenEvent{{
		Developer: "alice", IssueID: "issue-1", Model: "synthetic-model",
		InputTok: 1000, CostMicro: 3000, Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: "collide-1",
	}}); err != nil {
		t.Fatalf("insert token event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Boot 2: a DIFFERENT table declaring the SAME version. Layer 1 cannot see
	// this (neither table is the embedded one), which is exactly why layer 2
	// exists.
	infoB, err := LoadPriceTable(second)
	if err != nil {
		t.Fatalf("LoadPriceTable(second): %v", err)
	}
	if infoA.TableHash == infoB.TableHash {
		t.Fatalf("fixture is broken: both tables hash to %q, so there is no collision to refuse", infoA.TableHash)
	}
	if infoA.Version != infoB.Version {
		t.Fatalf("fixture is broken: versions differ (%d vs %d), so this is not a collision", infoA.Version, infoB.Version)
	}

	before := dumpSchema(t, path)
	beforeRegistry := countTableRows(t, path, "price_table_registry")
	beforeEvents := countTableRows(t, path, "token_events")
	if beforeEvents == 0 {
		t.Fatal("precondition: no token_events seeded, so the row-count assertion below would be vacuous")
	}

	db2, err := Open(path)
	if db2 != nil {
		_ = db2.Close()
		t.Fatal("Open returned a usable *DB for a colliding price table; want nil")
	}
	// (1) it failed.
	if err == nil {
		t.Fatal("Open with a colliding price table succeeded; want refusal")
	}
	msg := err.Error()
	// (2) for the right reason. "table_hash" is the load-bearing substring: it is
	// the thing that disagreed. The version number pins WHICH version collided.
	if !strings.Contains(msg, "table_hash") {
		t.Errorf("error %q does not mention table_hash; a generic failure (e.g. \"database is locked\") would pass a bare non-nil check", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d", collidingVersion)) {
		t.Errorf("error %q does not name the colliding version %d", msg, collidingVersion)
	}
	// Both hashes must be named — the operator cannot act on "they differ".
	if !strings.Contains(msg, infoA.TableHash) {
		t.Errorf("error %q does not name the RECORDED hash %q", msg, infoA.TableHash)
	}
	if !strings.Contains(msg, infoB.TableHash) {
		t.Errorf("error %q does not name this binary's ACTIVE hash %q", msg, infoB.TableHash)
	}
	// The database must be named: every other Open failure names it, and "point
	// --db at a different file" is unactionable without it.
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not name the database %q", msg, path)
	}
	// And the remedy — in the EXACT flag form the CLI accepts. A positional
	// version silently discards --commit and fails; see
	// TestForgetVersionRemedyIsRunnable in cmd/tierd, which executes this string.
	for _, want := range []string{"Bump 'version:'", "--version " + fmt.Sprintf("%d", collidingVersion), "--commit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not carry the remedy substring %q", msg, want)
		}
	}

	// (3) nothing was mutated.
	if after := dumpSchema(t, path); after != before {
		t.Errorf("schema mutated by a refused Open:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := countTableRows(t, path, "price_table_registry"); n != beforeRegistry {
		t.Errorf("price_table_registry rows = %d, want unchanged %d — the refused Open recorded the disputed table", n, beforeRegistry)
	}
	if n := countTableRows(t, path, "token_events"); n != beforeEvents {
		t.Errorf("token_events rows = %d, want unchanged %d", n, beforeEvents)
	}
	// The RECORDED identity must still be the first table's — a refused Open must
	// not have overwritten what version 4242 means.
	row, found := readRegistryRow(t, path, collidingVersion)
	if !found {
		t.Fatalf("registry row for version %d disappeared across a refused Open", collidingVersion)
	}
	if row.TableHash != infoA.TableHash {
		t.Errorf("recorded table_hash = %q, want the ORIGINAL %q — the refused Open rebound the version", row.TableHash, infoA.TableHash)
	}
}

// TestOpen_RefusesCollisionBeforeRepricingRows is the PHASE-ORDER arm, and it is
// the ONLY test in the suite that kills the moved-phase mutant (measured — the
// ordinary negative control above stays green under it).
//
// The guard sits at Phase 1.5: after Phase 1's CREATE TABLEs, before Phase 2.7
// recomputeKnownSourceCosts and Phase 2.85 backfillPriceVersion, both of which
// WRITE prices and STAMP price_version using the ACTIVE table. Moving it below
// either still produces a refused Open with a correct message — while rows have
// already been repriced and re-stamped under the very table being rejected.
//
// ⚠️ WHAT THE FIXTURE IS, STATED HONESTLY. It deletes both one-shot migration
// markers so those phases genuinely would run. That is NOT "exactly a first boot
// after upgrade": on a real first #714 boot there is no registry row to collide
// with, and the same Open that records one also records both markers. The seeded
// state (registry row present, markers absent) is reachable only via a crash
// between Phase 1.5 and Phase 2.85, or manual surgery. Its real value is forward
// -looking: it locks the invariant so that a later commit adding a price-writing
// phase after 1.5 — marker-gated or not — reddens here.
func TestOpen_RefusesCollisionBeforeRepricingRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "phaseorder.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4246
	first := writePriceTable(t, dir, "first.yaml", v, 3.00)
	second := writePriceTable(t, dir, "second.yaml", v, 30.00) // 10x — any reprice moves cost_micro a lot

	if _, err := LoadPriceTable(first); err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open with the first table: %v", err)
	}
	// A jsonl-source row with token counts: exactly the shape
	// recomputeKnownSourceCosts rewrites (a closed allowlist of collector sources)
	// and backfillPriceVersion stamps. cost_micro is seeded to the value the FIRST
	// table produces (1M input tokens at $3.00/M = $3.00), so a recompute under
	// the first table is a no-op and only a recompute under the SECOND ($30.00/M)
	// moves it — the signal this test looks for.
	const seededCostMicro = 3_000_000
	if err := db.InsertTokenEvents(ctx, []TokenEvent{{
		Developer: "alice", IssueID: "issue-1", Model: "synthetic-model",
		InputTok: 1_000_000, OutputTok: 0, CostMicro: seededCostMicro,
		Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "phase-order-1",
	}}); err != nil {
		t.Fatalf("insert token event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-arm both one-shot migrations by deleting their markers, so the next Open
	// genuinely WOULD reprice and re-stamp if it got that far.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := raw.Exec(
		`DELETE FROM tier_migrations WHERE name IN (?, ?)`,
		migrationRecomputeCacheTTL, migrationBackfillPriceVersion,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("clear migration markers: %v", err)
	}
	// Also blank price_version back to the pre-#233 sentinel so backfill has
	// something to stamp.
	if _, err := raw.Exec(`UPDATE token_events SET price_version = 0`); err != nil {
		_ = raw.Close()
		t.Fatalf("reset price_version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	beforeCost, beforeVersion := readEventCostAndVersion(t, path, "phase-order-1")
	if beforeCost <= 0 {
		t.Fatalf("precondition: seeded cost_micro = %d, want > 0", beforeCost)
	}
	if beforeVersion != 0 {
		t.Fatalf("precondition: price_version = %d, want the 0 sentinel", beforeVersion)
	}

	// The colliding table, at a rate 10x the recorded one.
	if _, err := LoadPriceTable(second); err != nil {
		t.Fatalf("LoadPriceTable(second): %v", err)
	}
	db2, err := Open(path)
	if db2 != nil {
		_ = db2.Close()
		t.Fatal("Open returned a usable *DB for a colliding price table; want nil")
	}
	if err == nil || !strings.Contains(err.Error(), "table_hash") {
		t.Fatalf("Open failed for the wrong reason (or not at all): %v", err)
	}

	afterCost, afterVersion := readEventCostAndVersion(t, path, "phase-order-1")
	if afterCost != beforeCost {
		t.Errorf("cost_micro = %d, want unchanged %d — the refusal ran AFTER Phase 2.7 recomputeKnownSourceCosts, so the row was repriced under the very table Open then rejected", afterCost, beforeCost)
	}
	if afterVersion != beforeVersion {
		t.Errorf("price_version = %d, want unchanged %d — the refusal ran AFTER Phase 2.85 backfillPriceVersion, so the row was stamped under a disputed version", afterVersion, beforeVersion)
	}
	// And the markers must still be absent: a refused Open must not have recorded
	// either migration as done, or a later successful Open would skip it.
	for _, marker := range []string{migrationRecomputeCacheTTL, migrationBackfillPriceVersion} {
		if migrationMarkerPresent(t, path, marker) {
			t.Errorf("tier_migrations marker %q was recorded by a REFUSED Open", marker)
		}
	}
}

// readEventCostAndVersion reads one token_events row's cost_micro and
// price_version by idempotency_key, via a raw connection.
func readEventCostAndVersion(t *testing.T, path, key string) (int64, int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var cost int64
	var version int
	if err := raw.QueryRow(
		`SELECT cost_micro, price_version FROM token_events WHERE idempotency_key = ?`, key,
	).Scan(&cost, &version); err != nil {
		t.Fatalf("read token event %s: %v", key, err)
	}
	return cost, version
}

// migrationMarkerPresent reports whether a tier_migrations marker row exists.
func migrationMarkerPresent(t *testing.T, path, name string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM tier_migrations WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("read tier_migrations: %v", err)
	}
	return n > 0
}

// TestOpen_AcceptsSameVersionSameContent pins the no-op outcome: re-loading the
// SAME table from a DIFFERENT path is not a collision. source is provenance, not
// identity, and file_hash is recorded but never compared — so a table copied to a
// new location, or a comment-only edit, must open cleanly.
//
// Without this arm the guard could be "correct" by refusing everything.
func TestOpen_AcceptsSameVersionSameContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.db")
	restoreDefaultPriceTable(t)

	const v = 4243
	a := writePriceTable(t, dir, "a.yaml", v, 3.00)
	// Same resolved rates, different FILE bytes (a leading comment) and a
	// different path — so file_hash and source both move while table_hash does not.
	b := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(b, []byte("# a comment-only edit\n"+syntheticPriceTableYAML(v, 3.00)), 0o600); err != nil {
		t.Fatalf("write b.yaml: %v", err)
	}

	infoA, err := LoadPriceTable(a)
	if err != nil {
		t.Fatalf("LoadPriceTable(a): %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open with a: %v", err)
	}
	_ = db.Close()

	infoB, err := LoadPriceTable(b)
	if err != nil {
		t.Fatalf("LoadPriceTable(b): %v", err)
	}
	if infoA.TableHash != infoB.TableHash {
		t.Fatalf("fixture is broken: the two files resolve to different table_hash (%q vs %q), so this is not the same-content case", infoA.TableHash, infoB.TableHash)
	}
	if infoA.FileHash == infoB.FileHash {
		t.Fatalf("fixture is broken: the two files have the SAME file_hash, so this does not prove file_hash is uncompared")
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open with the same table from a different path was refused: %v", err)
	}
	_ = db2.Close()

	if n := countTableRows(t, path, "price_table_registry"); n != 1 {
		t.Errorf("price_table_registry rows = %d, want 1 (same content must not add a row)", n)
	}
	// The recorded row keeps the FIRST source and file_hash. It is a record of
	// what was first seen, not a running last-writer-wins field.
	row, _ := readRegistryRow(t, path, v)
	if row.FileHash != infoA.FileHash {
		t.Errorf("file_hash = %q, want the first-seen %q", row.FileHash, infoA.FileHash)
	}
	if row.Source != a {
		t.Errorf("source = %q, want the first-seen %q", row.Source, a)
	}
}

// TestRecordPriceTableIdentity_AcceptsDifferentHashScheme pins the third
// comparison outcome: a stored hash computed under a DIFFERENT canonicalization
// scheme is "not comparable", not "not equal".
//
// 🔑 WHY THIS MATTERS MORE THAN IT LOOKS. #713 tags the digest
// ("tierpt1:<hex>") specifically so a downstream guard can tell a re-scheme from
// tampering, and this is that guard. If it compared the whole tagged string, the
// first legitimate fix to canonicalPriceTableBytes would make EVERY recorded
// version in EVERY database collide at once, and the prescribed recovery would be
// to delete the entire registry — a serialization bugfix bricking the fleet and
// then destroying the records the table exists to hold. It also pins the other
// half: we do NOT silently restamp the stored row under the new scheme, because
// that is precisely the rebinding this feature refuses.
func TestRecordPriceTableIdentity_AcceptsDifferentHashScheme(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheme.db")
	pinEmbeddedPriceTable(t)
	info := ActivePriceTableInfo()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	// Rewrite the recorded hash under a PRETEND older scheme, same hex.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	oldScheme := "tierpt0:" + strings.TrimPrefix(info.TableHash, priceTableHashScheme+":")
	if _, err := raw.Exec(`UPDATE price_table_registry SET table_hash = ? WHERE version = ?`, oldScheme, info.Version); err != nil {
		_ = raw.Close()
		t.Fatalf("rewrite stored hash: %v", err)
	}
	_ = raw.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open refused a row recorded under a DIFFERENT hash scheme: %v\nA re-canonicalization must not read as a collision — see recordPriceTableIdentity", err)
	}
	_ = db2.Close()

	// And the stored row is left exactly as it was: not restamped.
	row, found := readRegistryRow(t, path, info.Version)
	if !found {
		t.Fatal("registry row disappeared")
	}
	if row.TableHash != oldScheme {
		t.Errorf("stored table_hash = %q, want the untouched %q — accepting an incomparable scheme must not silently rebind the version to the new one", row.TableHash, oldScheme)
	}
	// Control: with the SAME scheme, a different hex still collides. Without this
	// the test above would also pass if the guard simply stopped comparing.
	raw2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := raw2.Exec(`UPDATE price_table_registry SET table_hash = ? WHERE version = ?`,
		priceTableHashScheme+":"+strings.Repeat("a", 64), info.Version); err != nil {
		_ = raw2.Close()
		t.Fatalf("rewrite stored hash: %v", err)
	}
	_ = raw2.Close()
	if _, err := Open(path); err == nil {
		t.Error("control failed: a SAME-scheme hash mismatch was accepted; the scheme arm has disabled the guard entirely")
	}
}

// TestRecordPriceTableIdentity_ConcurrentInsert exercises the ON CONFLICT arm and
// the mandatory re-read, using the deterministic seam in recordPriceTableIdentity.
//
// 🔴 BOTH HALVES WERE MEASURED SURVIVORS BEFORE THIS TEST: removing
// `ON CONFLICT(version) DO NOTHING`, and deleting the post-insert re-read and
// returning nil, each left the whole suite green. A naive parallel-Open test does
// NOT close this — measured, it fails earlier and elsewhere (db.Ping "database is
// locked", a UNIQUE violation on tier_migrations), so it would prove nothing about
// this code. The seam is what makes the window real.
//
// Two arms, because the two mutants are different:
//
//	AGREEING racer   -> the conflict must be swallowed, Open succeeds (kills the
//	                    "remove ON CONFLICT" mutant, which errors here)
//	DIVERGENT racer  -> the re-read must adjudicate on CONTENT and refuse (kills
//	                    the "skip the re-read" mutant, which succeeds here)
func TestRecordPriceTableIdentity_ConcurrentInsert(t *testing.T) {
	pinEmbeddedPriceTable(t)
	info := ActivePriceTableInfo()

	// insertRacer writes a registry row for the active version with the given
	// hash, simulating another process that got there first.
	insertRacer := func(t *testing.T, path, hash string) {
		t.Helper()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("racer sql.Open: %v", err)
		}
		defer func() { _ = raw.Close() }()
		if _, err := raw.Exec(
			`INSERT INTO price_table_registry (version, table_hash, file_hash, effective_date, model_count, source, tool_version)
			 VALUES (?, ?, 'sha256:racer', '2026-07-26', 1, 'racer', 'racer')`,
			info.Version, hash,
		); err != nil {
			t.Fatalf("racer insert: %v", err)
		}
	}

	t.Run("agreeing racer is swallowed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "race-agree.db")
		// Create the schema without tripping the seam.
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		_ = db.Close()
		// Clear the row so the next Open takes the "absent" branch, then have the
		// racer insert an AGREEING row inside the window.
		clearRegistry(t, path)
		testHookAfterRegistryRead = func() { insertRacer(t, path, info.TableHash) }
		t.Cleanup(func() { testHookAfterRegistryRead = nil })

		db2, err := Open(path)
		if err != nil {
			t.Fatalf("Open lost a race against an AGREEING concurrent recorder: %v\nthe INSERT must carry ON CONFLICT(version) DO NOTHING", err)
		}
		_ = db2.Close()
		if n := countTableRows(t, path, "price_table_registry"); n != 1 {
			t.Errorf("price_table_registry rows = %d, want 1", n)
		}
	})

	t.Run("divergent racer is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "race-diverge.db")
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		_ = db.Close()
		clearRegistry(t, path)
		other := priceTableHashScheme + ":" + strings.Repeat("b", 64)
		testHookAfterRegistryRead = func() { insertRacer(t, path, other) }
		t.Cleanup(func() { testHookAfterRegistryRead = nil })

		db2, err := Open(path)
		if db2 != nil {
			_ = db2.Close()
		}
		if err == nil {
			t.Fatal("Open accepted a DIVERGENT concurrent recorder's table; the post-insert re-read must adjudicate on content")
		}
		if !strings.Contains(err.Error(), "table_hash") {
			t.Errorf("Open failed for the wrong reason: %v", err)
		}
	})
}

// clearRegistry empties price_table_registry via a raw connection.
func clearRegistry(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`DELETE FROM price_table_registry`); err != nil {
		t.Fatalf("clear registry: %v", err)
	}
}

// TestRecordPriceTableIdentity_RefusesBlankIdentity pins the malformed-input
// guard, which was a measured survivor: deleting it left the suite green.
//
// Its own comment says an empty table_hash "would poison the very column the
// guard compares" — a row recorded with `table_hash = ""` would then match any
// future empty-hash load and silently disable the guard for that version.
func TestRecordPriceTableIdentity_RefusesBlankIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank.db")
	pinEmbeddedPriceTable(t)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, tc := range []struct {
		name string
		info PriceTableInfo
	}{
		{"zero value", PriceTableInfo{}},
		{"version 0", PriceTableInfo{Version: 0, TableHash: "tierpt1:abc"}},
		{"blank hash", PriceTableInfo{Version: 7, TableHash: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := recordPriceTableIdentity(db.db, path, tc.info)
			if err == nil {
				t.Fatal("recordPriceTableIdentity accepted a blank identity; want a loud error")
			}
			if !strings.Contains(err.Error(), "no active price table") {
				t.Errorf("error %q does not explain the problem", err)
			}
		})
	}
	// And nothing was written for the bogus versions.
	if _, found := readRegistryRow(t, path, 7); found {
		t.Error("a blank-hash identity was recorded")
	}
}

// TestLoadPriceTable_RefusesEmbeddedVersionCollision is layer 1: an override that
// reuses the EMBEDDED table's version with different rates is refused by
// LoadPriceTable itself — with NO database involved.
//
// 🔑 THAT PLACEMENT IS THE POINT. `tierd score-log` calls store.LoadPriceTable
// directly and never opens a store, so a guard in cmd/tierd's loadPricesOverride
// wrapper would leave it unguarded. This test opens no database at all, which is
// what demonstrates the guard is in the load path and not in Open.
func TestLoadPriceTable_RefusesEmbeddedVersionCollision(t *testing.T) {
	dir := t.TempDir()
	pinEmbeddedPriceTable(t)

	emb := ActivePriceTableInfo()
	if emb.Version < 1 || emb.TableHash == "" {
		t.Fatalf("precondition: no embedded table loaded (%+v)", emb)
	}
	// A table at the EMBEDDED version with content that cannot match it. Written
	// directly (not via writePriceTable, which refuses the embedded version).
	p := filepath.Join(dir, "collide.yaml")
	if err := os.WriteFile(p, []byte(syntheticPriceTableYAML(emb.Version, 3.01)), 0o600); err != nil {
		t.Fatalf("write collide.yaml: %v", err)
	}

	before := ActivePriceTableInfo()
	_, err := LoadPriceTable(p)
	if err == nil {
		t.Fatal("LoadPriceTable accepted an override reusing the embedded version with different content; want refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "table_hash") {
		t.Errorf("error %q does not mention table_hash", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d", emb.Version)) {
		t.Errorf("error %q does not name the colliding version %d", msg, emb.Version)
	}
	if !strings.Contains(msg, emb.TableHash) {
		t.Errorf("error %q does not name the embedded table_hash %q", msg, emb.TableHash)
	}
	if !strings.Contains(msg, "Bump 'version:'") {
		t.Errorf("error %q does not carry the remedy", msg)
	}
	// A refused load must leave the active table untouched — the same
	// never-a-silent-fallback discipline a parse error gets.
	if got := ActivePriceTableInfo(); got != before {
		t.Errorf("a refused LoadPriceTable changed the active table: %+v -> %+v", before, got)
	}
}

// TestLoadPriceTable_AcceptsEmbeddedVersionSameContent is layer 1's control arm:
// an override that reuses the embedded version but resolves to the SAME table is
// the same table said twice, and must load. Without this, "refuse everything at
// the embedded version" would pass the test above.
func TestLoadPriceTable_AcceptsEmbeddedVersionSameContent(t *testing.T) {
	dir := t.TempDir()
	pinEmbeddedPriceTable(t)

	// Write the embedded YAML back out verbatim: same version, same resolved
	// content, different path.
	p := filepath.Join(dir, "copy.yaml")
	if err := os.WriteFile(p, defaultPriceTableYAML, 0o600); err != nil {
		t.Fatalf("write copy.yaml: %v", err)
	}
	emb := ActivePriceTableInfo()

	info, err := LoadPriceTable(p)
	if err != nil {
		t.Fatalf("LoadPriceTable of a verbatim copy of the embedded table was refused: %v", err)
	}
	if info.TableHash != emb.TableHash {
		t.Fatalf("a verbatim copy resolved to a different table_hash (%q vs %q); the fixture, not the guard, is wrong", info.TableHash, emb.TableHash)
	}
	if info.Source != p {
		t.Errorf("source = %q, want the override path %q", info.Source, p)
	}
}

// TestPriceTableRegistry_UniquenessIsAnIndexNotAPrimaryKey is the TENANCY arm.
//
// It reads sqlite_master and asserts the uniqueness on (version) is expressed as
// CREATE UNIQUE INDEX DDL, and that the table definition does NOT declare version
// as a PRIMARY KEY. That is not style: a table-level PK embeds uniqueness in the
// TABLE definition, which the eventual tenant_id retrofit cannot recreate with an
// ALTER — it needs the full 12-step table rebuild. As index DDL the retrofit is a
// DROP INDEX + CREATE UNIQUE INDEX, and tenant_id LEADS: (tenant_id, version).
//
// It also pins `id INTEGER PRIMARY KEY AUTOINCREMENT`, because `version INTEGER
// PRIMARY KEY` would additionally ALIAS rowid and make the version the row
// address.
func TestPriceTableRegistry_UniquenessIsAnIndexNotAPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenancy.db")
	pinEmbeddedPriceTable(t)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()

	var tableSQL string
	if err := raw.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='price_table_registry'`,
	).Scan(&tableSQL); err != nil {
		t.Fatalf("read price_table_registry table DDL: %v", err)
	}
	var indexSQL string
	if err := raw.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_price_table_registry_live_version'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read idx_price_table_registry_live_version DDL (uniqueness must be an INDEX): %v", err)
	}

	// The index is UNIQUE and on (version) alone — the shape a tenant_id retrofit
	// prepends to.
	uniqueOnVersion := regexp.MustCompile(`(?is)CREATE\s+UNIQUE\s+INDEX\b.*\bON\s+price_table_registry\s*\(\s*version\s*\)`)
	if !uniqueOnVersion.MatchString(indexSQL) {
		t.Errorf("uniqueness DDL = %q, want CREATE UNIQUE INDEX ... ON price_table_registry(version)", indexSQL)
	}
	// 🔴 AND IT MUST BE PARTIAL. Without `WHERE forgotten_at IS NULL` the soft
	// delete is unusable: a retired row would keep binding (version), so the next
	// Open could never record the replacement identity and the escape hatch would
	// be broken. Keeping the evidence and keeping the remedy both depend on this
	// predicate.
	if !regexp.MustCompile(`(?is)\bWHERE\s+forgotten_at\s+IS\s+NULL\b`).MatchString(indexSQL) {
		t.Errorf("uniqueness DDL = %q, want a PARTIAL index qualified by WHERE forgotten_at IS NULL — a retired identity must not block re-registration", indexSQL)
	}
	// The pre-ruling TOTAL index must not coexist with it.
	if indexExists(t, path, "idx_price_table_registry_version") {
		t.Error("the pre-soft-delete TOTAL unique index is still present alongside the partial one")
	}

	// The TABLE must not carry the uniqueness. Strip comments first: the schema
	// text in store.go carries SQL comments that legitimately mention
	// "version INTEGER PRIMARY KEY" while explaining why it is NOT used, and
	// SQLite stores the CREATE statement verbatim including them.
	ddl := stripSQLComments(tableSQL)
	if regexp.MustCompile(`(?is)\bversion\b[^,)]*\bPRIMARY\s+KEY\b`).MatchString(ddl) {
		t.Errorf("price_table_registry declares version as a PRIMARY KEY; uniqueness must be index DDL so the tenant_id retrofit stays mechanical.\nDDL:\n%s", ddl)
	}
	if regexp.MustCompile(`(?is)\bUNIQUE\s*\(`).MatchString(ddl) || regexp.MustCompile(`(?is)\bversion\b[^,)]*\bUNIQUE\b`).MatchString(ddl) {
		t.Errorf("price_table_registry declares a table-level UNIQUE constraint; uniqueness must be index DDL.\nDDL:\n%s", ddl)
	}
	if !regexp.MustCompile(`(?is)\bid\s+INTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT\b`).MatchString(ddl) {
		t.Errorf("price_table_registry does not declare `id INTEGER PRIMARY KEY AUTOINCREMENT`; the surrogate key must be id, not version.\nDDL:\n%s", ddl)
	}
}

// stripSQLComments removes `-- ...` line comments so a DDL assertion cannot be
// satisfied (or defeated) by prose in the schema's own comments.
func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestForgetPriceTableVersion is the ESCAPE HATCH arm, and it proves the whole
// remedy loop, not just the DELETE:
//
//	refused Open -> dry run changes NOTHING (still refused) -> --commit
//	-> the same Open now SUCCEEDS -> the new identity is recorded.
//
// A fail-closed guard is only as good as its documented way out; this is that
// documentation, executable.
func TestForgetPriceTableVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forget.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4244
	first := writePriceTable(t, dir, "first.yaml", v, 3.00)
	second := writePriceTable(t, dir, "second.yaml", v, 3.01)

	infoA, err := LoadPriceTable(first)
	if err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open with the first table: %v", err)
	}
	_ = db.Close()

	if _, err := LoadPriceTable(second); err != nil {
		t.Fatalf("LoadPriceTable(second): %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("precondition: Open with the colliding table succeeded; there is nothing to escape from")
	}

	// DRY RUN: returns the row, deletes nothing, and hands it to confirm first.
	var confirmed []PriceTableRegistryRow
	capture := func(r PriceTableRegistryRow) error { confirmed = append(confirmed, r); return nil }
	row, err := ForgetPriceTableVersion(ctx, path, v, false, "tester", capture)
	if err != nil {
		t.Fatalf("ForgetPriceTableVersion dry run: %v", err)
	}
	if row.TableHash != infoA.TableHash {
		t.Errorf("dry run returned table_hash %q, want the recorded %q", row.TableHash, infoA.TableHash)
	}
	if len(confirmed) != 1 || confirmed[0].TableHash != infoA.TableHash {
		t.Errorf("confirm was not called with the row: %+v", confirmed)
	}
	if n := countTableRows(t, path, "price_table_registry"); n != 1 {
		t.Errorf("price_table_registry rows after a DRY RUN = %d, want still 1", n)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open succeeded after a DRY RUN; the dry run deleted the row")
	}

	// COMMIT: the row goes, and the same Open now succeeds.
	confirmed = nil
	deleted, err := ForgetPriceTableVersion(ctx, path, v, true, "tester", capture)
	if err != nil {
		t.Fatalf("ForgetPriceTableVersion commit: %v", err)
	}
	if deleted.TableHash != infoA.TableHash {
		t.Errorf("commit returned table_hash %q, want the deleted row's %q", deleted.TableHash, infoA.TableHash)
	}
	if len(confirmed) != 1 {
		t.Errorf("confirm was not called before the delete: %+v", confirmed)
	}
	// 🔴 THE ROW IS KEPT. The maintainer's ruling: a hatch that erases the record of its own
	// use contradicts the thing it protects. A test asserting the row is GONE would
	// pass the very implementation this replaced.
	if n := countTableRows(t, path, "price_table_registry"); n != 1 {
		t.Fatalf("price_table_registry rows after --commit = %d, want 1 (RETAINED and stamped, never deleted)", n)
	}
	if deleted.ForgottenAt == "" || deleted.ForgottenBy != "tester" {
		t.Errorf("returned row is not stamped: forgotten_at=%q forgotten_by=%q", deleted.ForgottenAt, deleted.ForgottenBy)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after forget-version was still refused: %v", err)
	}
	_ = db2.Close()

	// The LIVE identity for that version is now the SECOND table...
	after, found := readRegistryRow(t, path, v)
	if !found {
		t.Fatalf("no LIVE registry row for version %d after the successful Open — a retired row must not block re-registration", v)
	}
	if after.TableHash == infoA.TableHash {
		t.Errorf("live table_hash is still the retired %q; the successful Open did not record the new table", infoA.TableHash)
	}
	// ...and BOTH now coexist: the retired one and the live one, which is exactly
	// what the PARTIAL unique index buys. A total unique index on (version) would
	// have made this impossible.
	if n := countTableRows(t, path, "price_table_registry"); n != 2 {
		t.Errorf("price_table_registry rows = %d, want 2 (the retired identity AND its live replacement)", n)
	}

	// Forgetting a version that was never recorded is its own error, not a
	// generic failure — the CLI reports "nothing to forget" from it.
	if _, err := ForgetPriceTableVersion(ctx, path, v+1, true, "tester", nil); !errors.Is(err, ErrNoPriceTableRegistryRow) {
		t.Errorf("forgetting an unrecorded version returned %v, want ErrNoPriceTableRegistryRow", err)
	}
	// A missing database file is an error, NOT a silently-created empty one: the
	// likeliest cause is a mistyped --db, and conjuring a database there would
	// report "nothing to forget" for a file that never existed.
	missing := filepath.Join(dir, "does-not-exist.db")
	if _, err := ForgetPriceTableVersion(ctx, missing, v, true, "tester", nil); err == nil {
		t.Error("ForgetPriceTableVersion on a missing path succeeded; want an error")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("ForgetPriceTableVersion created a database at a path that did not exist")
	}
	// Version 0 / negatives are rejected before any file is touched.
	if _, err := ForgetPriceTableVersion(ctx, path, 0, true, "tester", nil); err == nil {
		t.Error("ForgetPriceTableVersion accepted version 0; want an error")
	}
}

// TestForgetPriceTableVersion_ConfirmFailureAbortsTheWrite is the RED fix, and
// the defect it pins was measured, not hypothetical.
//
// 🔴 BEFORE THIS, `tierd prices forget-version --commit >&-` EXITED 0 WITH THE ROW
// DELETED AND NOTHING PRINTED. The store deleted first and returned the row for
// printing afterwards, and the CLI discarded every write error — so a closed
// stdout, a full disk, or an EPIPE from `| head` destroyed the only record of what
// a version meant while reporting success. The whole justification for this
// command over a `--accept-rehash` flag rests on the operator keeping that
// evidence, so a silent loss of it is not a cosmetic bug.
func TestForgetPriceTableVersion_ConfirmFailureAbortsTheWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "confirm.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4247
	p := writePriceTable(t, dir, "t.yaml", v, 3.00)
	if _, err := LoadPriceTable(p); err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	writeErr := errors.New("simulated: stdout is closed")
	_, err = ForgetPriceTableVersion(ctx, path, v, true, "tester", func(PriceTableRegistryRow) error { return writeErr })
	if err == nil {
		t.Fatal("ForgetPriceTableVersion succeeded although the evidence could not be reported; want a refusal")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("error %v does not wrap the reporting failure", err)
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error %q does not tell the operator the state of the database", err)
	}
	if n := countTableRows(t, path, "price_table_registry"); n != 1 {
		t.Errorf("price_table_registry rows = %d, want 1", n)
	}
	// The row must still be LIVE: an aborted retirement must not have stamped it.
	row, found := readRegistryRow(t, path, v)
	if !found {
		t.Fatal("the row was retired despite the evidence not landing")
	}
	if row.Forgotten() {
		t.Errorf("the row was stamped forgotten_at=%q despite the evidence not landing", row.ForgottenAt)
	}
	if n := countTableRows(t, path, "price_forget_audit"); n != 0 {
		t.Errorf("price_forget_audit rows = %d, want 0 — a ledger entry was written for an aborted operation", n)
	}
}

// TestForgetPriceTableVersion_VanishedRowIsReported pins the zero-RowsAffected
// arm: if the row disappears between the read and the delete, the command must
// say so rather than print "deleted" over a row it did not delete.
//
// The confirm callback is the injection point — it runs between the two.
func TestForgetPriceTableVersion_VanishedRowIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vanish.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4248
	p := writePriceTable(t, dir, "t.yaml", v, 3.00)
	if _, err := LoadPriceTable(p); err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	// Delete the row from "another process" inside the window.
	_, err = ForgetPriceTableVersion(ctx, path, v, true, "tester", func(PriceTableRegistryRow) error {
		clearRegistry(t, path)
		return nil
	})
	if !errors.Is(err, ErrNoPriceTableRegistryRow) {
		t.Fatalf("error = %v, want ErrNoPriceTableRegistryRow for a row that vanished mid-flight", err)
	}
	if !strings.Contains(err.Error(), "retired between the read and the write") {
		t.Errorf("error %q does not explain what happened", err)
	}
}

// TestPriceTableRegistryCommands_OnADatabaseWithoutTheTable pins that both
// operator commands translate SQLite's "no such table" into the sentinel.
//
// This is the likeliest operator mistake for these commands — a wrong --db, or an
// old backup that predates the feature — and it arrives while they are already
// dealing with a refused startup. A raw driver string is the wrong thing to hand
// them at that moment.
func TestPriceTableRegistryCommands_OnADatabaseWithoutTheTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	ctx := context.Background()

	// A valid SQLite database with no price_table_registry.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	if _, err := ForgetPriceTableVersion(ctx, path, 9, false, "tester", nil); !errors.Is(err, ErrNoPriceTableRegistryRow) {
		t.Errorf("ForgetPriceTableVersion error = %v, want ErrNoPriceTableRegistryRow", err)
	}
	if _, err := ListPriceTableRegistry(ctx, path); !errors.Is(err, ErrNoPriceTableRegistryRow) {
		t.Errorf("ListPriceTableRegistry error = %v, want ErrNoPriceTableRegistryRow", err)
	}
}

// TestListPriceTableRegistry pins the READ surface — the answer to "a guard you
// cannot inspect is a guard you can only escape". It also pins that listing is
// read-only: the file is byte-identical afterwards, so an operator can safely
// inspect a database before deciding anything.
func TestListPriceTableRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	for _, v := range []int{4260, 4261} {
		p := writePriceTable(t, dir, fmt.Sprintf("v%d.yaml", v), v, 3.00)
		if _, err := LoadPriceTable(p); err != nil {
			t.Fatalf("LoadPriceTable(%d): %v", v, err)
		}
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open at %d: %v", v, err)
		}
		_ = db.Close()
	}

	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}

	rows, err := ListPriceTableRegistry(ctx, path)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Version != 4260 || rows[1].Version != 4261 {
		t.Errorf("versions = %d, %d; want 4260, 4261 ascending", rows[0].Version, rows[1].Version)
	}
	for _, r := range rows {
		if r.TableHash == "" || r.FileHash == "" || r.EffectiveDate == "" || r.ModelCount == 0 || r.Source == "" || r.ToolVersion == "" || r.FirstSeen == "" {
			t.Errorf("row %+v has an empty field; every column is NOT NULL and populated", r)
		}
	}

	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read db: %v", err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Error("listing the registry modified the database file; the read path must open ?mode=ro")
	}
}

// TestForgetPriceTableVersion_DatabaseStillAnswersWhatTheVersionMeant is THE
// GUARD FOR THE MAINTAINER'S RULING, and it is written the way it is on purpose.
//
// 🔴 IT ASSERTS THE RECORD SURVIVES — NOT THAT THE ROW IS GONE. A test asserting
// absence is exactly what the superseded hard-delete implementation would pass,
// which is the defect being fixed. The question this poses to the database is the
// ruling's own sentence: after `forget-version --commit`, can you still answer
// "version N once meant table_hash X, and that record was retired at T by U?"
//
// It asks TWICE, of two independent survivors, because they fail differently:
//   - the RETAINED registry row (soft delete), and
//   - the price_forget_audit LEDGER, which carries its own copy of table_hash and
//     therefore still answers even if the registry row is later hard-deleted by
//     hand.
//
// Then it asserts the hatch actually worked — the guard no longer binds, and the
// replacement identity registers — because a "guarantee" that also broke the
// escape hatch would be no fix at all.
func TestForgetPriceTableVersion_DatabaseStillAnswersWhatTheVersionMeant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "answers.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4270
	first := writePriceTable(t, dir, "first.yaml", v, 3.00)

	infoA, err := LoadPriceTable(first)
	if err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	// Retire it.
	const who = "sre-oncall"
	retired, err := ForgetPriceTableVersion(ctx, path, v, true, who, func(PriceTableRegistryRow) error { return nil })
	if err != nil {
		t.Fatalf("ForgetPriceTableVersion: %v", err)
	}

	// ---- ANSWER 1: the retained registry row -----------------------------
	all, err := ListPriceTableRegistry(ctx, path)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	var kept *PriceTableRegistryRow
	for i := range all {
		if all[i].Version == v && all[i].Forgotten() {
			kept = &all[i]
		}
	}
	if kept == nil {
		t.Fatalf("THE RULING IS VIOLATED: after forget-version --commit the database can no longer say what version %d meant. Rows: %+v", v, all)
	}
	if kept.TableHash != infoA.TableHash {
		t.Errorf("retained table_hash = %q, want %q — the retained row must record what the version MEANT", kept.TableHash, infoA.TableHash)
	}
	if kept.ForgottenAt == "" {
		t.Error("retained row has no forgotten_at — the database cannot say WHEN the record was retired")
	}
	if kept.ForgottenBy != who {
		t.Errorf("retained row forgotten_by = %q, want %q — the database cannot say BY WHOM", kept.ForgottenBy, who)
	}
	// The returned row must carry the same stamps, or the CLI prints blanks on the
	// one path where they are the entire point.
	if retired.ForgottenAt == "" || retired.ForgottenBy != who {
		t.Errorf("returned row is unstamped: forgotten_at=%q forgotten_by=%q", retired.ForgottenAt, retired.ForgottenBy)
	}

	// ---- ANSWER 2: the audit ledger --------------------------------------
	ledger, err := ListPriceForgetAudit(ctx, path)
	if err != nil {
		t.Fatalf("ListPriceForgetAudit: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("price_forget_audit rows = %d, want exactly 1 — the act must be recorded in the same ledger family as reprice/repair-repo", len(ledger))
	}
	e := ledger[0]
	if e.Version != v || e.TableHash != infoA.TableHash {
		t.Errorf("ledger entry = (v%d, %q), want (v%d, %q)", e.Version, e.TableHash, v, infoA.TableHash)
	}
	if e.Actor != who {
		t.Errorf("ledger actor = %q, want %q", e.Actor, who)
	}
	for _, f := range []struct{ name, got string }{
		{"forget_id", e.ForgetID}, {"file_hash", e.FileHash},
		{"effective_date", e.EffectiveDate}, {"source", e.Source},
		{"recorded_tool_version", e.RecordedToolVersion}, {"first_seen", e.FirstSeen},
		{"tool_version", e.ToolVersion}, {"ts", e.Timestamp},
	} {
		if f.got == "" {
			t.Errorf("ledger entry field %s is empty", f.name)
		}
	}
	// 🔑 THE LEDGER MUST OUTLIVE THE ROW IT DESCRIBES — the family convention, and
	// the reason it copies table_hash instead of holding a foreign key. Hard-delete
	// the registry row and the question must STILL be answerable.
	clearRegistry(t, path)
	survived, err := ListPriceForgetAudit(ctx, path)
	if err != nil {
		t.Fatalf("ListPriceForgetAudit after a hard delete of the registry: %v", err)
	}
	if len(survived) != 1 || survived[0].TableHash != infoA.TableHash {
		t.Errorf("the ledger no longer answers what version %d meant after the registry row was hard-deleted; it must carry its own copy of table_hash", v)
	}
}

// TestForgetPriceTableVersion_RefusesAnUnattributedRetirement pins that a
// --commit with no actor is rejected rather than silently stored.
//
// 🔴 A MEASURED SURVIVOR BEFORE THIS TEST. The CLI always supplies an actor (it
// defaults to the OS username), so no CLI-level test could ever reach this branch
// — but ForgetPriceTableVersion is EXPORTED, and the guard is what stops a future
// caller writing a blank forgotten_by. An attribution ledger whose attribution
// column is empty is not one, and the whole reason this operation retains a row
// rather than deleting it is to answer "retired at T BY WHOM".
//
// The dry-run arm is the control: reading is not attribution, so it must NOT
// require an actor.
func TestForgetPriceTableVersion_RefusesAnUnattributedRetirement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "actor.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4272
	p := writePriceTable(t, dir, "t.yaml", v, 3.00)
	if _, err := LoadPriceTable(p); err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	for _, actor := range []string{"", "   ", "\t\n"} {
		if _, err := ForgetPriceTableVersion(ctx, path, v, true, actor, nil); err == nil {
			t.Errorf("--commit with actor %q was accepted; want a refusal", actor)
		} else if !strings.Contains(err.Error(), "actor is required") {
			t.Errorf("actor %q: error %q does not explain the requirement", actor, err)
		}
	}
	// Nothing was written by any of the refused attempts.
	row, found := readRegistryRow(t, path, v)
	if !found || row.Forgotten() {
		t.Errorf("an unattributed retirement mutated the row: found=%v row=%+v", found, row)
	}
	if n := countTableRows(t, path, "price_forget_audit"); n != 0 {
		t.Errorf("price_forget_audit rows = %d, want 0", n)
	}
	// CONTROL: a DRY RUN needs no actor — reading is not attribution.
	if _, err := ForgetPriceTableVersion(ctx, path, v, false, "", nil); err != nil {
		t.Errorf("a dry run was refused for want of an actor: %v", err)
	}
}

// TestForgetPriceTableVersion_RetiredRowDoesNotBlockReRegistration pins the other
// half of the partial index: retiring an identity must actually WORK as an escape
// hatch. A soft delete behind a TOTAL unique index would keep the evidence and
// break the remedy — the guard could never record the replacement.
func TestForgetPriceTableVersion_RetiredRowDoesNotBlockReRegistration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rereg.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()

	const v = 4271
	first := writePriceTable(t, dir, "first.yaml", v, 3.00)
	second := writePriceTable(t, dir, "second.yaml", v, 3.01)

	infoA, err := LoadPriceTable(first)
	if err != nil {
		t.Fatalf("LoadPriceTable(first): %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	infoB, err := LoadPriceTable(second)
	if err != nil {
		t.Fatalf("LoadPriceTable(second): %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("precondition: the colliding table was accepted")
	}
	if _, err := ForgetPriceTableVersion(ctx, path, v, true, "tester", nil); err != nil {
		t.Fatalf("ForgetPriceTableVersion: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after retiring the identity was still refused: %v\nA retired row must not block re-registration — that is what the PARTIAL unique index is for", err)
	}
	_ = db2.Close()

	// The live identity is B; A survives as the retired one.
	live, found := readRegistryRow(t, path, v)
	if !found || live.TableHash != infoB.TableHash {
		t.Errorf("live identity = %+v, want table_hash %q", live, infoB.TableHash)
	}
	all, err := ListPriceTableRegistry(ctx, path)
	if err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("registry holds %d rows, want 2 (retired + live)", len(all))
	}
	var sawRetiredA, sawLiveB bool
	for _, r := range all {
		if r.Forgotten() && r.TableHash == infoA.TableHash {
			sawRetiredA = true
		}
		if !r.Forgotten() && r.TableHash == infoB.TableHash {
			sawLiveB = true
		}
	}
	if !sawRetiredA || !sawLiveB {
		t.Errorf("want the retired A identity AND the live B identity to coexist; got %+v", all)
	}
	// Retiring the same version twice is "nothing to forget" for the ALREADY
	// retired one — but B is live now, so this retires B, not A again. Assert we
	// never re-stamp an already-retired row (which would falsify its forgotten_at).
	if _, err := ForgetPriceTableVersion(ctx, path, v, true, "tester2", nil); err != nil {
		t.Fatalf("retiring the replacement identity: %v", err)
	}
	if _, err := ForgetPriceTableVersion(ctx, path, v, true, "tester3", nil); !errors.Is(err, ErrNoPriceTableRegistryRow) {
		t.Errorf("a third retire returned %v, want ErrNoPriceTableRegistryRow — there is no live identity left to retire", err)
	}
	ledger, err := ListPriceForgetAudit(ctx, path)
	if err != nil {
		t.Fatalf("ListPriceForgetAudit: %v", err)
	}
	if len(ledger) != 2 {
		t.Errorf("ledger holds %d entries, want 2 (one per retirement, none for the refused third)", len(ledger))
	}
}

// TestPriceTableRegistry_MigratesAPreSoftDeleteDatabase pins the Phase-1.5
// prologue against the shape this feature shipped with BEFORE the ruling: the
// columns absent and a TOTAL unique index on (version).
//
// 🔴 IT IS NOT HYPOTHETICAL, AND IT IS THE TRAP THE SCHEMA COMMENT WARNS ABOUT.
// Phase 1 is CREATE TABLE IF NOT EXISTS, a no-op on an existing table — so on such
// a database the soft-delete columns would still be missing when Phase 1.5 SELECTs
// them, and EVERY Open would fail. The partial index has the same problem one
// level down (`WHERE forgotten_at IS NULL` cannot compile before the column
// exists). Both must therefore be converged by migratePriceTableRegistry, in that
// order, ahead of the read.
func TestPriceTableRegistry_MigratesAPreSoftDeleteDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presoftdelete.db")
	pinEmbeddedPriceTable(t)
	ctx := context.Background()
	info := ActivePriceTableInfo()

	// Build the OLD shape by hand: no forgotten_* columns, a TOTAL unique index.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE price_table_registry (
	    id INTEGER PRIMARY KEY AUTOINCREMENT, version INTEGER NOT NULL,
	    table_hash TEXT NOT NULL, file_hash TEXT NOT NULL, effective_date TEXT NOT NULL,
	    model_count INTEGER NOT NULL, source TEXT NOT NULL, tool_version TEXT NOT NULL,
	    first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
	    CREATE UNIQUE INDEX idx_price_table_registry_version ON price_table_registry(version);`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed pre-soft-delete schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO price_table_registry (version, table_hash, file_hash, effective_date, model_count, source, tool_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		info.Version, info.TableHash, info.FileHash, info.EffectiveDate, info.ModelCount, PriceSourceEmbedded, "old-binary"); err != nil {
		_ = raw.Close()
		t.Fatalf("seed row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	// Open must migrate and succeed — the pre-existing identity matches.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-soft-delete database failed: %v\nthe columns and the partial index must be converged BEFORE Phase 1.5 reads the table", err)
	}
	_ = db.Close()

	// The old TOTAL index is gone; leaving it would silently defeat the ruling by
	// making a retired row block re-registration.
	if indexExists(t, path, "idx_price_table_registry_version") {
		t.Error("the pre-soft-delete TOTAL unique index survived; a retired row would block re-registration")
	}
	if !indexExists(t, path, "idx_price_table_registry_live_version") {
		t.Error("the partial live-version index was not created")
	}
	// The pre-existing row is LIVE (NULL forgotten_at is the convergent default).
	row, found := readRegistryRow(t, path, info.Version)
	if !found {
		t.Fatal("the migrated row is not live; ADD COLUMN must default to NULL == live")
	}
	if row.Forgotten() {
		t.Error("the migration marked a pre-existing identity as retired")
	}
	// And the hatch works end to end on the migrated database.
	if _, err := ForgetPriceTableVersion(ctx, path, info.Version, true, "tester", nil); err != nil {
		t.Fatalf("forget-version on a migrated database: %v", err)
	}
	ledger, err := ListPriceForgetAudit(ctx, path)
	if err != nil || len(ledger) != 1 {
		t.Errorf("ledger after retiring on a migrated database: %v, %d entries", err, len(ledger))
	}
}

// indexExists reports whether a named index is present in sqlite_master.
func indexExists(t *testing.T, path, name string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	return n > 0
}

// TestPriceTableRegistryReads_DoNotConvertJournalMode pins that BOTH read paths —
// `prices list` and a `forget-version` DRY RUN — open the database READ-ONLY.
//
// 🔑 WHY A JOURNAL-MODE ASSERTION AND NOT A BYTE COMPARISON. sqliteDSN carries
// journal_mode(WAL), and WAL is PERSISTENT: connecting read-write to a
// DELETE-mode file — a restored backup, or a database an operator points --db at
// while diagnosing — converts it and creates -wal/-shm sidecars. A byte
// comparison cannot see this on a database that is ALREADY in WAL mode, which is
// every database Open has touched, so a test built on one lets the regression
// through (measured: it did). The fixture is deliberately a delete-mode file.
func TestPriceTableRegistryReads_DoNotConvertJournalMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deletemode.db")
	ctx := context.Background()

	// Build a DELETE-mode database with a registry row, without going through
	// Open (which would set WAL).
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		_ = raw.Close()
		t.Fatalf("set delete mode: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE price_table_registry (
	    id INTEGER PRIMARY KEY AUTOINCREMENT, version INTEGER NOT NULL,
	    table_hash TEXT NOT NULL, file_hash TEXT NOT NULL, effective_date TEXT NOT NULL,
	    model_count INTEGER NOT NULL, source TEXT NOT NULL, tool_version TEXT NOT NULL,
	    first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	    forgotten_at DATETIME, forgotten_by TEXT)`); err != nil {
		_ = raw.Close()
		t.Fatalf("create table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO price_table_registry
	    (version, table_hash, file_hash, effective_date, model_count, source, tool_version)
	    VALUES (99, 'tierpt1:aa', 'sha256:bb', '2026-07-26', 1, 'embedded', 'test')`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	journalMode := func() string {
		t.Helper()
		r, err := sql.Open("sqlite", path+"?mode=ro")
		if err != nil {
			t.Fatalf("open for journal_mode: %v", err)
		}
		defer func() { _ = r.Close() }()
		var m string
		if err := r.QueryRow(`PRAGMA journal_mode`).Scan(&m); err != nil {
			t.Fatalf("read journal_mode: %v", err)
		}
		return strings.ToLower(m)
	}
	if got := journalMode(); got != "delete" {
		t.Fatalf("precondition: journal_mode = %q, want \"delete\" — the fixture cannot detect a conversion", got)
	}

	if _, err := ListPriceTableRegistry(ctx, path); err != nil {
		t.Fatalf("ListPriceTableRegistry: %v", err)
	}
	if got := journalMode(); got != "delete" {
		t.Errorf("after `prices list`, journal_mode = %q, want unchanged \"delete\" — the read path must open ?mode=ro", got)
	}

	if _, err := ForgetPriceTableVersion(ctx, path, 99, false, "tester", nil); err != nil {
		t.Fatalf("forget-version dry run: %v", err)
	}
	if got := journalMode(); got != "delete" {
		t.Errorf("after a forget-version DRY RUN, journal_mode = %q, want unchanged \"delete\" — a dry run must not open the file read-write", got)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("a read-only operation created %s", sidecar)
		}
	}
}

// TestSetToolVersion pins that the package setter — not an Open parameter, so the
// Open call sites stay untouched — is what lands in the registry row's
// tool_version column, and that an unset identity records the explicit "unknown"
// sentinel rather than an empty string.
func TestSetToolVersion(t *testing.T) {
	// Restore the RAW stored value, not currentToolVersion()'s "unknown"
	// substitution — otherwise this test permanently converts an unset global
	// into the literal string "unknown" for everything that runs after it.
	prev, hadPrev := toolVersion.Load().(string)
	t.Cleanup(func() {
		if hadPrev {
			toolVersion.Store(prev)
		} else {
			toolVersion.Store("")
		}
	})
	pinEmbeddedPriceTable(t)

	SetToolVersion("")
	if got := currentToolVersion(); got != "unknown" {
		t.Errorf("currentToolVersion with an empty identity = %q, want %q", got, "unknown")
	}

	const stamp = "tierd v9.9.9-test"
	SetToolVersion(stamp)
	path := filepath.Join(t.TempDir(), "toolversion.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	cols := readRegistryColumnsByName(t, path, ActivePriceTableInfo().Version)
	if cols["tool_version"] != stamp {
		t.Errorf("tool_version = %q, want %q", cols["tool_version"], stamp)
	}
}
