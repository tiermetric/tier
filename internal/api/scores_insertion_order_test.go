package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// orderFixtureRow is one logical row of the #711 invariance fixture: a merged
// outcome plus the cost that funded it. It is deliberately a VALUE, so the same
// logical set can be replayed into two stores in two different sequences.
type orderFixtureRow struct {
	dev     string
	issue   string
	weight  float64
	quality float64
	costUSD float64
	ts      time.Time
}

// orderFixture builds the logical row set both stores are loaded with.
//
// ⚠️ EVERY ts IS DISTINCT, and that is a constraint of the fix, not an accident
// of the fixture. The reads are ordered by (ts, id) and `id` is a per-database
// rowid, so two SEPARATELY populated databases agree on the read order only when
// ts breaks every tie. Within ONE database — the production case #711 is about —
// (ts, id) is a total order that no query-plan change can permute. A fixture with
// tied ts would be asserting something the fix does not (and should not) claim.
//
// Every developer clears both ranking floors (>= scoring.MinRankedOutcomes
// outcomes and >= scoring.MinRankedCostUSD spend) so the bootstrap CI is actually
// computed — an unranked row ships (0, 0) and the CI, the thing with the real
// blast radius, would never be exercised.
func orderFixture(base time.Time) []orderFixtureRow {
	devs := []string{"alice", "bob", "carol", "dave"}
	var rows []orderFixtureRow
	n := 0
	for di, dev := range devs {
		for i := 0; i < 6; i++ {
			n++
			rows = append(rows, orderFixtureRow{
				dev:   dev,
				issue: fmt.Sprintf("%s-%d", dev, i),
				// Heterogeneous weights/qualities/costs: a uniform fixture is
				// permutation-insensitive by construction and would pass with the
				// ORDER BY removed.
				weight:  0.5 + float64((di*7+i*3)%11)*0.31,
				quality: 0.55 + float64((di*5+i*2)%9)*0.05,
				costUSD: 1.25 + float64((di*3+i*5)%13)*0.87,
				// Distinct instant per row, ordered by n so the canonical sequence
				// and the ts sequence coincide for store A and diverge for store B.
				ts: base.Add(time.Duration(n) * time.Minute),
			})
		}
	}
	return rows
}

// newOrderTestHandler builds a handler over a store at a path the caller keeps,
// so the raw-SQL vacuity probe below can open the SAME database file and see the
// physical row order the ORDER BY is hiding.
func newOrderTestHandler(t *testing.T) (*Handler, *store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier-order-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(path)
	})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(db, quiet, "", nil, "test", RateLimitConfig{}, WithUnsealedRecompute()), db, path
}

// loadOrderFixture replays rows into db in the given sequence and returns the
// issue ids in the order they were written — the "insertion order" the vacuity
// control compares.
func loadOrderFixture(t *testing.T, db *store.DB, rows []orderFixtureRow, seq []int) []string {
	t.Helper()
	ctx := context.Background()
	written := make([]string, 0, len(seq))
	for _, idx := range seq {
		r := rows[idx]
		// Cost first, at the SAME instant as the outcome, so the token spend lands
		// inside the outcome's attributable look-back window and no row trips the
		// #136 zero-token tripwire (which would unrank the developer and silently
		// remove the CI this test exists to pin).
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: r.dev,
			IssueID:   r.issue,
			Model:     "claude-sonnet-4",
			InputTok:  2000, // > scoring.MinAttributableTokens
			CostMicro: store.DollarsToMicro(r.costUSD),
			Source:    "jsonl",
			Fidelity:  "realtime",
			Timestamp: r.ts,
		}); err != nil {
			t.Fatalf("InsertTokenEvent %s/%s: %v", r.dev, r.issue, err)
		}
		if _, err := db.InsertOutcome(ctx, store.Outcome{
			Developer:      r.dev,
			IssueID:        r.issue,
			Weight:         r.weight,
			Quality:        r.quality,
			MergeCommitSHA: "sha-" + r.issue,
			Timestamp:      r.ts,
		}); err != nil {
			t.Fatalf("InsertOutcome %s/%s: %v", r.dev, r.issue, err)
		}
		written = append(written, r.issue)
	}
	return written
}

// rawTestDSN mirrors the DSN store.Open builds (store.go: busy_timeout +
// journal_mode) for the second connections this file opens onto the SAME file the
// handler's pool is holding.
//
// 🔴 THE busy_timeout IS NOT COSMETIC. A bare `sql.Open("sqlite", path)` inherits
// SQLite's DEFAULT busy_timeout of 0 — the first SQLITE_BUSY returns immediately
// instead of retrying. ANALYZE below is a WRITE against a WAL database the
// handler's pool also has open, so a bare connection makes this a latent hard
// flake that only shows up under load or on a slow machine. Production never
// opens without it; neither should a probe that writes.
func rawTestDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

// analyzeStore runs ANALYZE against the database file over a second connection.
//
// 🔴 THIS IS NOT FIXTURE DRESSING — IT IS THE TRIGGER, AND WITHOUT IT THIS TEST
// IS VACUOUS. Measured on modernc.org/sqlite v1.48.0 with this schema:
//
//	no stats: SELECT ... FROM outcomes WHERE ts >= ? AND ts < ?
//	          -> SEARCH outcomes USING INDEX idx_outcomes_ts_id
//	ANALYZEd: same statement
//	          -> SCAN outcomes                      (i.e. ROWID order)
//
// So before ANALYZE the unordered read happens to come back in ts order and the
// #711 defect is INVISIBLE: the negative control passes and the guard proves
// nothing. After ANALYZE the planner drops to a table scan and the read order
// becomes insertion order, which is precisely the "stable by accident, until
// ANALYZE / data growth / a SQLite upgrade" hazard the issue describes. The
// ORDERED statement stays on idx_outcomes_ts_id either way (pinned by
// TestAllOutcomesWindow_OrderIsIndexServed in internal/store).
//
// ⚠️ WHAT ANALYZE ACTUALLY CHANGES IS SQLITE_ENABLE_STAT4, and knowing that is
// what tells a red apart from a regression. modernc.org/sqlite v1.48.0 compiles
// stat4 IN (`pragma_compile_options` reports ENABLE_STAT4); ANALYZE writes
// sqlite_stat1 AND sqlite_stat4 sample rows, and only the stat4 samples let the
// planner see that this window covers most of the table — measured in
// internal/store, deleting sqlite_stat4 and reloading the statistics puts the
// plan straight back to the index seek. Consequences:
//
//   - Under a SQLite build WITHOUT stat4 (the macOS system `sqlite3` 3.51.0 is
//     one) the index plan is kept and vacuity control (3) below FIRES. That is
//     the guard reporting "this build cannot demonstrate #711", not the fix
//     regressing. Check STAT4 and the window's coverage fraction (measured
//     thresholds, swept in internal/store: >= 60% is SCAN at every table size
//     measured, <= 40% keeps the seek) before assuming otherwise.
//   - The driver is pure Go and version-pinned in go.mod, so there is no system
//     libsqlite3 in this build and a fresh CI checkout runs the identical
//     planner. The plan can only move via a deliberate dependency bump.
func analyzeStore(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatalf("open raw sqlite for ANALYZE: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(context.Background(), `ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
}

// unorderedReaderSequence runs the WHERE clause AllOutcomesWindow actually uses,
// with the ORDER BY removed, and returns the row sequence the planner produces.
// This — not a bare unqualified SELECT — is the sequence the #711 fix has to
// override, so it is what the vacuity control must compare.
func unorderedReaderSequence(t *testing.T, path string, since, until time.Time) []string {
	return rawSequence(t, path,
		`SELECT developer, issue_id FROM outcomes WHERE ts >= ? AND ts < ?`,
		since, until)
}

// rawOutcomeSequence reads (developer, issue_id) from the outcomes table with NO
// ORDER BY and no predicate, over a second connection to the same file.
func rawOutcomeSequence(t *testing.T, path string) []string {
	return rawSequence(t, path, `SELECT developer, issue_id FROM outcomes`)
}

func rawSequence(t *testing.T, path, query string, args ...any) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", rawTestDSN(path))
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("raw outcomes query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var dev, issue string
		if err := rows.Scan(&dev, &issue); err != nil {
			t.Fatalf("scan raw row: %v", err)
		}
		out = append(out, dev+"/"+issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("raw rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("raw unordered read returned NO rows — the vacuity control cannot conclude anything")
	}
	return out
}

// scoresStructure is the set of STRUCTURAL facts the two responses must share
// before any numeric comparison is meaningful.
type scoresStructure struct {
	devs    []string
	sampleN map[string]int
	ranked  map[string]bool
	flagged map[string]int
}

func getScoresStructure(t *testing.T, h *Handler, query string) (scoresStructure, []byte) {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores"+query, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores%s: status = %d; body = %s", query, code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /scores%s: %v; body = %s", query, err, body)
	}
	s := scoresStructure{
		sampleN: map[string]int{},
		ranked:  map[string]bool{},
		flagged: map[string]int{},
	}
	for _, d := range resp.Developers {
		s.devs = append(s.devs, d.Developer)
		s.sampleN[d.Developer] = d.SampleN
		s.ranked[d.Developer] = d.Ranked
		s.flagged[d.Developer] = d.FlaggedOutcomes
	}
	slices.Sort(s.devs)
	return s, body
}

// TestScores_InvariantUnderInsertionOrder is the primary #711 guard.
//
// Two stores are loaded with the SAME logical outcome+cost set, the second in a
// shuffled sequence drawn from a fixed-seed PRNG. GET /api/v1/scores against both
// must return BYTE-IDENTICAL bodies. One assertion pins tier, tier_ci_low,
// tier_ci_high, cost_per_point and weighted_points at once.
//
// ⚠️ IT DOES NOT PIN /scores/compare's `significant`. An earlier version of this
// comment claimed it did, on the reasoning that the boolean is a CI-overlap test
// (compare.go:131) over the SAME developerWindowCI derivation. The reasoning is
// sound and the derivation is shared — but a boolean is not pinned by an
// assertion on the numbers it is derived from unless something drives it across
// its threshold. TestScoresCompare_SignificanceFlipsOnRowOrder below is the test
// that does that, and it exists because this claim stood unmeasured for a review
// round.
//
// Why the CI is the point, not the ulp: scoring.BootstrapCI draws
// `k := rng.IntN(n)` from a FIXED-seed PRNG, so the index stream is identical on
// every call. A fixed index stream over a PERMUTED array selects DIFFERENT
// ELEMENTS — see TestBootstrapCI_IsPermutationSensitive in internal/scoring,
// which measures the resulting move.
//
// 🔴 STRUCTURAL FACTS ARE ASSERTED FIRST AND SEPARATELY. When the ORDER BY is
// reverted, ONLY the byte-identity arm may redden. If the row count, the
// developer list, sample_n, ranked or flagged_outcomes redden too, the fixture is
// broken (rows lost to dedup, a window boundary, or the zero-token tripwire) and
// the numeric arm would be failing for a reason that has nothing to do with #711.
func TestScores_InvariantUnderInsertionOrder(t *testing.T) {
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Minute)
	rows := orderFixture(base)

	canonical := make([]int, len(rows))
	for i := range canonical {
		canonical[i] = i
	}
	shuffled := slices.Clone(canonical)
	// Fixed seed: the shuffle must be reproducible, or a failure cannot be
	// re-run. Arbitrary constants (splitmix64), unrelated to the bootstrap seed.
	rng := rand.New(rand.NewPCG(0x243f6a8885a308d3, 0x13198a2e03707344))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	hA, dbA, pathA := newOrderTestHandler(t)
	hB, dbB, pathB := newOrderTestHandler(t)
	orderA := loadOrderFixture(t, dbA, rows, canonical)
	orderB := loadOrderFixture(t, dbB, rows, shuffled)

	// Collect table statistics on BOTH databases. See analyzeStore: without this
	// the planner serves the window from idx_outcomes_ts_id, the unordered read
	// comes back in ts order by accident, and the negative control passes — i.e.
	// the guard is vacuous. This was caught by actually running the mutation, not
	// by reading the code.
	analyzeStore(t, pathA)
	analyzeStore(t, pathB)

	// Window the read explicitly (the endpoint takes YYYY-MM-DD) so `since` is
	// echoed identically into both bodies and no clock tick between the two
	// requests can move a boundary and make the bodies differ for a reason that
	// is not row order.
	const dayLayout = "2006-01-02"
	winSince := base.Add(-24 * time.Hour).Truncate(24 * time.Hour)
	winUntil := base.Add(96 * time.Hour).Truncate(24 * time.Hour)
	query := "?since=" + winSince.Format(dayLayout) + "&until=" + winUntil.Format(dayLayout)

	// ── 🔴 VACUITY CONTROLS ──────────────────────────────────────────────────
	// (1) The insertion sequences must actually differ.
	if slices.Equal(orderA, orderB) {
		t.Fatal("vacuity control: the two stores were loaded in the SAME sequence — nothing was shuffled, so this test cannot fail")
	}
	// (2) A bare unordered read of the table must see that difference.
	rawA := rawOutcomeSequence(t, pathA)
	rawB := rawOutcomeSequence(t, pathB)
	if len(rawA) != len(rows) || len(rawB) != len(rows) {
		t.Fatalf("vacuity control: raw reads returned %d and %d rows, want %d each", len(rawA), len(rawB), len(rows))
	}
	if slices.Equal(rawA, rawB) {
		t.Fatalf("vacuity control: a raw `SELECT developer, issue_id FROM outcomes` (no ORDER BY) returned the SAME sequence on both databases:\n%v\nThe shuffle did not change the physical row order.", rawA)
	}
	// (3) 🔴 THE ONE THAT ACTUALLY MATTERS. Control (2) is necessary but NOT
	// sufficient: the reader carries a `ts >= ? AND ts < ?` predicate, and the
	// plan for THAT statement — not for a bare SELECT — is what the ORDER BY has
	// to override. A first version of this test asserted only (2), and the
	// mutation run passed: the windowed statement was index-served and already in
	// ts order. Assert on the sequence the reader's own WHERE clause produces.
	seekA := unorderedReaderSequence(t, pathA, winSince, winUntil)
	seekB := unorderedReaderSequence(t, pathB, winSince, winUntil)
	if len(seekA) != len(rows) || len(seekB) != len(rows) {
		t.Fatalf("vacuity control: the reader-shaped unordered read returned %d and %d rows, want %d each — the window does not cover the fixture", len(seekA), len(seekB), len(rows))
	}
	if slices.Equal(seekA, seekB) {
		t.Fatalf("vacuity control: AllOutcomesWindow's own WHERE clause WITHOUT an ORDER BY returned the SAME sequence on both databases:\n%v\nThe planner is serving it in ts order anyway, so removing the #711 ORDER BY would not change the response and this test cannot fail. (Fix the fixture — ANALYZE, row count, window width — do not weaken the assertion.)", seekA)
	}

	// ── STRUCTURAL ARMS (must stay green even when the fix is reverted) ──────
	structA, bodyA := getScoresStructure(t, hA, query)
	structB, bodyB := getScoresStructure(t, hB, query)

	if len(structA.devs) == 0 {
		t.Fatal("structural: /scores returned no developers — the window or the fixture is wrong and every assertion below is vacuous")
	}
	if !slices.Equal(structA.devs, structB.devs) {
		t.Fatalf("structural: developer lists differ: A=%v B=%v — the two stores do not hold the same logical data, so a numeric difference would not be attributable to row order", structA.devs, structB.devs)
	}
	for _, dev := range structA.devs {
		if structA.sampleN[dev] != structB.sampleN[dev] {
			t.Errorf("structural: sample_n for %s differs: A=%d B=%d — rows were lost on one side (dedup or window), the fixture is broken", dev, structA.sampleN[dev], structB.sampleN[dev])
		}
		if structA.flagged[dev] != structB.flagged[dev] {
			t.Errorf("structural: flagged_outcomes for %s differs: A=%d B=%d", dev, structA.flagged[dev], structB.flagged[dev])
		}
		if structA.ranked[dev] != structB.ranked[dev] {
			t.Errorf("structural: ranked for %s differs: A=%v B=%v", dev, structA.ranked[dev], structB.ranked[dev])
		}
		// The zero-token tripwire silently unranks a developer and zeroes the CI.
		// Assert it did not fire, or the byte-identity arm would be comparing two
		// pairs of zeros. (Memory of this repo's fixture trap: seeding cost after
		// the outcome puts it outside the attributable window and flags everything.)
		if structA.flagged[dev] != 0 {
			t.Fatalf("structural: %s has %d zero-token outcomes — the cost seed missed the attributable window, so this developer is unranked and its CI is (0,0); the numeric arm would prove nothing", dev, structA.flagged[dev])
		}
		if !structA.ranked[dev] {
			t.Fatalf("structural: %s is NOT ranked — no bootstrap CI is computed for an unranked row, so the CI (the whole point of #711) is not exercised", dev)
		}
	}

	// ── NUMERIC ARM (the ONLY arm that may redden when the ORDER BY is removed) ──
	if string(bodyA) != string(bodyB) {
		t.Errorf("GET /scores returned DIFFERENT bodies for the same logical data inserted in two different orders.\n"+
			"This is the #711 defect: the outcome read is unordered, so float summation order and the fixed-seed bootstrap's index domain both depend on physical row order.\n%s",
			firstDifference(bodyA, bodyB))
	}
}

// firstDifference renders a short, readable window around the first differing
// byte so a failure names the field that moved instead of dumping two documents.
func firstDifference(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := i - 90
	if lo < 0 {
		lo = 0
	}
	hiA := i + 90
	if hiA > len(a) {
		hiA = len(a)
	}
	hiB := i + 90
	if hiB > len(b) {
		hiB = len(b)
	}
	return fmt.Sprintf("first difference at byte %d:\n  A: ...%s...\n  B: ...%s...", i, a[lo:hiA], b[lo:hiB])
}

// ── #711: the published significance boolean ────────────────────────────────
//
// Everything below exists to turn one claim from an inference into a
// measurement: that a permuted outcome read can flip
// /scores/compare's `significant`, a BOOLEAN this API shows a user, not a
// rounding artifact.

const (
	// compareFixtureDays is the span the fixture occupies. Window A is
	// [day 0, day 15) and window B is [day 15, day 20) — adjacent, disjoint, and
	// a perfectly ordinary before/after comparison.
	compareFixtureDays  = 20
	compareWindowASplit = 15
	// compareWindowAOutcomes / compareWindowBOutcomes put 12 of the table's 18
	// rows inside window A (67%) and 6 inside window B (33%).
	//
	// 🔴 THAT SPLIT IS THE EXPERIMENT. Measured (see internal/store's
	// scoringOrderSQL note): with stat4 statistics present, a `ts` window covering
	// <= 40% of the table keeps the index seek — which returns rows in (ts, id)
	// order anyway, so the defect is INVISIBLE — while >= 60% plans as SCAN at
	// every table size swept, and returns rowid order. So window A is permutable
	// and window B is not, which is exactly what makes the flip constructible: one
	// side's interval moves under a permutation while the other stays put.
	//
	// ⚠️ This is also why a first attempt to demonstrate the flip FAILED. It used
	// two one-day windows; both were selective, both stayed index-served, and the
	// two CIs came back bit-identical even with the fix reverted.
	compareWindowAOutcomes = 12
	compareWindowBOutcomes = 6
	// compareWindowBCostScale divides window B's costs, and TIER moves inversely
	// with cost, so it SLIDES window B's whole interval up the TIER axis. It is
	// tuned — swept 0.60 … 0.72 in the scratch search that built this fixture — to
	// park window B's ci_low in the gap between window A's ORDERED ci_high and its
	// PERMUTED ci_high. Measured over that sweep: at 0.68 and above the intervals
	// overlap in BOTH readings (significant is false either way); at 0.62 and below
	// they are disjoint in BOTH (true either way); only 0.64–0.67 is the band where
	// the answer DEPENDS on row order. 0.65 sits near its middle.
	compareWindowBCostScale = 0.65
	// compareCIHighPermutationMove is the measured movement of window A's ci_high
	// between the ordered read and the permuted one on this fixture:
	// 270.126 -> 287.672, i.e. +17.55 TIER (+6.5%). It is asserted against below
	// as a STALENESS GUARD, not as a published number — see the marginality arm.
	compareCIHighPermutationMove = 17.55
)

// compareOrderFixture builds the logical rows behind
// TestScoresCompare_SignificanceFlipsOnRowOrder: one developer, heterogeneous
// (weight, quality, cost) triples, every ts distinct.
//
// Heterogeneity is load-bearing twice over. A uniform fixture is
// permutation-insensitive by construction, and a fixture whose bootstrap interval
// is very wide or very narrow cannot be parked next to another interval's edge.
// The arithmetic below is arbitrary but FIXED: change it and the marginality arm
// will tell you the demonstration went stale.
func compareOrderFixture(base time.Time) []orderFixtureRow {
	var rows []orderFixtureRow
	for i := 0; i < compareWindowAOutcomes; i++ {
		rows = append(rows, orderFixtureRow{
			dev:     "alice",
			issue:   fmt.Sprintf("A-%d", i),
			weight:  0.5 + float64((i*7)%11)*0.35,
			quality: 0.55 + float64((i*5)%9)*0.05,
			costUSD: 1.10 + float64((i*3)%13)*1.30,
			ts:      base.AddDate(0, 0, i).Add(time.Duration(3+i) * time.Hour),
		})
	}
	for i := 0; i < compareWindowBOutcomes; i++ {
		rows = append(rows, orderFixtureRow{
			dev:     "alice",
			issue:   fmt.Sprintf("B-%d", i),
			weight:  0.5 + float64((i*4)%11)*0.35,
			quality: 0.60 + float64((i*3)%9)*0.05,
			costUSD: (1.40 + float64((i*5)%13)*1.10) * compareWindowBCostScale,
			ts:      base.AddDate(0, 0, compareWindowASplit+i/2).Add(time.Duration(2+i) * time.Hour),
		})
	}
	return rows
}

// compareDelta fetches one developer's row from GET /api/v1/scores/compare.
func compareDelta(t *testing.T, h *Handler, query, dev string) developerDeltaJSON {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/compare"+query, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores/compare%s: status = %d; body = %s", query, code, body)
	}
	var resp compareResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /scores/compare%s: %v; body = %s", query, err, body)
	}
	for _, d := range resp.Developers {
		if d.Developer == dev {
			return d
		}
	}
	t.Fatalf("/scores/compare returned no row for %q — the window or the fixture is wrong: %s", dev, body)
	return developerDeltaJSON{}
}

// TestScoresCompare_SignificanceFlipsOnRowOrder demonstrates #711's headline
// claim end to end: an unordered outcome read can flip the PUBLISHED
// `significant` boolean on /scores/compare.
//
// 🔴 IT IS A DEMONSTRATION, AND THE CLAIM IT DEMONSTRATES WAS UNMEASURED WHEN IT
// WAS FIRST PUBLISHED. The issue body, the store comment and the api interface
// comment all asserted the flip; three reviews accepted the mechanism and none of
// them could produce it, because the obvious probe (two one-day compare windows)
// is selective enough that the planner keeps the index seek and the read is
// already in (ts, id) order. The fixture above is built specifically to reach the
// regime where the defect bites.
//
// The construction, in one line: two stores hold the SAME logical rows in two
// insertion sequences; window A covers 67% of the table (SCAN, so its read order
// is rowid order and therefore differs between the two stores) and window B
// covers 33% (index seek, identical on both); window B's interval is parked just
// above window A's, close enough that window A's permutation-sized CI movement
// crosses it.
//
// Measured, with the two stores side by side:
//
//	ORDERED (as shipped)   both stores: A ci=[116.807, 270.126]  B ci=[279.388, 352.499]
//	                                    disjoint  -> significant = TRUE on both
//	scoringOrderSQL REVERTED  canonical: A ci=[116.807, 270.126] -> significant = TRUE
//	                          shuffled : A ci=[116.235, 287.672] -> OVERLAPS B
//	                                                             -> significant = FALSE
//
// Same logical data. Same request. Two different published booleans, decided by
// the sequence rows happened to be written in.
//
// 🔴 STRUCTURAL AND MARGINALITY ARMS COME FIRST, AND ONLY THE SIGNIFICANCE ARM
// MAY REDDEN UNDER THE MUTATION. If presence, ranked, sample_n or the fixture's
// marginality redden too, the fixture has drifted out of the regime it was tuned
// for and the significance arm would be failing for a reason that is not #711.
func TestScoresCompare_SignificanceFlipsOnRowOrder(t *testing.T) {
	base := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(compareFixtureDays + 20))
	rows := compareOrderFixture(base)

	canonical := make([]int, len(rows))
	for i := range canonical {
		canonical[i] = i
	}
	shuffled := slices.Clone(canonical)
	// Fixed seed, and the specific one the scratch search selected: it produces the
	// largest separation between the ordered and permuted ci_high of window A, so
	// the fixture sits as far from both edges of the flip as this construction
	// allows. Arbitrary constants; unrelated to the bootstrap seed.
	rng := rand.New(rand.NewPCG(9, 0x9e3779b97f4a7c15))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	// The canonical store is written in ts order, so its rowid order EQUALS its
	// (ts, id) order and the mutation cannot move it. It is the reference leg: the
	// value it publishes is the correct one, and the shuffled store is what
	// diverges.
	hCanon, dbCanon, pathCanon := newOrderTestHandler(t)
	hShuf, dbShuf, pathShuf := newOrderTestHandler(t)
	orderCanon := loadOrderFixture(t, dbCanon, rows, canonical)
	orderShuf := loadOrderFixture(t, dbShuf, rows, shuffled)

	analyzeStore(t, pathCanon)
	analyzeStore(t, pathShuf)

	const dayLayout = "2006-01-02"
	sinceA := base
	untilA := base.AddDate(0, 0, compareWindowASplit)
	untilB := base.AddDate(0, 0, compareFixtureDays)
	query := "?since_a=" + sinceA.Format(dayLayout) + "&until_a=" + untilA.Format(dayLayout) +
		"&since_b=" + untilA.Format(dayLayout) + "&until_b=" + untilB.Format(dayLayout)

	// ── 🔴 VACUITY CONTROLS ──────────────────────────────────────────────────
	// (1) The two stores really were loaded differently.
	if slices.Equal(orderCanon, orderShuf) {
		t.Fatal("vacuity control: both stores were loaded in the SAME sequence — nothing was shuffled, so this test cannot fail")
	}
	// (2) WINDOW A's own predicate, unordered, must produce DIFFERENT sequences on
	// the two databases. This is the SCAN the whole construction depends on: if
	// the planner is serving window A from idx_outcomes_ts_id, both sides come
	// back in ts order, the CIs are bit-identical, and reverting the fix changes
	// nothing. (See analyzeStore for STAT4 and the coverage threshold.)
	seekCanon := unorderedReaderSequence(t, pathCanon, sinceA, untilA)
	seekShuf := unorderedReaderSequence(t, pathShuf, sinceA, untilA)
	if len(seekCanon) != compareWindowAOutcomes || len(seekShuf) != compareWindowAOutcomes {
		t.Fatalf("vacuity control: window A's unordered read returned %d and %d rows, want %d each — the window does not cover the fixture as designed",
			len(seekCanon), len(seekShuf), compareWindowAOutcomes)
	}
	if slices.Equal(seekCanon, seekShuf) {
		t.Fatalf("vacuity control: window A's own WHERE clause WITHOUT an ORDER BY returned the SAME sequence on both databases:\n%v\nThe planner is serving it in (ts, id) order, so window A's CI cannot move under a permutation and this test cannot demonstrate the flip. Fix the fixture (coverage fraction, ANALYZE) — do not weaken the assertion.", seekCanon)
	}

	// ── STRUCTURAL ARMS (must stay green even when the fix is reverted) ──────
	dCanon := compareDelta(t, hCanon, query, "alice")
	dShuf := compareDelta(t, hShuf, query, "alice")

	for _, c := range []struct {
		name string
		d    developerDeltaJSON
	}{{"canonical", dCanon}, {"shuffled", dShuf}} {
		if !c.d.PresentA || !c.d.PresentB {
			t.Fatalf("structural (%s): alice is not present in both windows (a=%v b=%v) — a delta and its significance are 0/false for a one-window developer, so nothing below is exercised",
				c.name, c.d.PresentA, c.d.PresentB)
		}
		if !c.d.A.Ranked || !c.d.B.Ranked {
			t.Fatalf("structural (%s): alice is not ranked in both windows (a=%v b=%v) — an unranked side ships a (0,0) CI and `significant` is false by construction, not by overlap",
				c.name, c.d.A.Ranked, c.d.B.Ranked)
		}
	}
	if dCanon.A.SampleN != dShuf.A.SampleN || dCanon.B.SampleN != dShuf.B.SampleN {
		t.Fatalf("structural: sample_n differs between the two stores (a: %d vs %d, b: %d vs %d) — they do not hold the same logical data, so a numeric difference would not be attributable to row order",
			dCanon.A.SampleN, dShuf.A.SampleN, dCanon.B.SampleN, dShuf.B.SampleN)
	}
	if dCanon.A.SampleN != compareWindowAOutcomes || dCanon.B.SampleN != compareWindowBOutcomes {
		t.Fatalf("structural: window sample_n = (%d, %d), want (%d, %d) — the windows no longer split the fixture as the coverage-fraction argument assumes",
			dCanon.A.SampleN, dCanon.B.SampleN, compareWindowAOutcomes, compareWindowBOutcomes)
	}

	// ── MARGINALITY ARM: is the demonstration still live? ────────────────────
	// The flip is only constructible while window B's interval sits just ABOVE
	// window A's — disjoint (so the shipped answer is `true`) by a margin SMALLER
	// than the permutation moves window A's ci_high (so reverting the fix closes
	// the gap and the answer becomes `false`). Assert both halves. A failure here
	// means the fixture drifted, not that the fix regressed: re-tune
	// compareWindowBCostScale and re-measure compareCIHighPermutationMove.
	gap := dCanon.B.CILow - dCanon.A.CIHigh
	if gap <= 0 {
		t.Fatalf("marginality: window B's ci_low (%.3f) is not above window A's ci_high (%.3f) — the intervals already OVERLAP in the ordered reading, so `significant` is false for a reason that has nothing to do with row order. Re-tune compareWindowBCostScale.",
			dCanon.B.CILow, dCanon.A.CIHigh)
	}
	if gap >= compareCIHighPermutationMove {
		t.Fatalf("marginality: the ordered intervals are disjoint by %.3f TIER, which is WIDER than the %.2f the permutation moves window A's ci_high — reverting scoringOrderSQL would no longer close the gap, so this test would pass under the mutation and demonstrates nothing. Re-tune compareWindowBCostScale (lower = window B slides up).",
			gap, compareCIHighPermutationMove)
	}

	// ── THE ARM THAT MAY REDDEN UNDER THE MUTATION ───────────────────────────
	// Name the moved number first, so a failure says WHICH bound drifted before it
	// says the boolean disagreed.
	if dCanon.A.CILow != dShuf.A.CILow || dCanon.A.CIHigh != dShuf.A.CIHigh {
		t.Errorf("#711: window A's bootstrap CI DIFFERS between two stores holding the same logical rows: canonical=[%.6f, %.6f] shuffled=[%.6f, %.6f].\nThe outcome read is unordered, so the fixed-seed bootstrap's index domain is physical row order.",
			dCanon.A.CILow, dCanon.A.CIHigh, dShuf.A.CILow, dShuf.A.CIHigh)
	}
	if dCanon.B.CILow != dShuf.B.CILow || dCanon.B.CIHigh != dShuf.B.CIHigh {
		t.Errorf("#711: window B's bootstrap CI DIFFERS between the two stores: canonical=[%.6f, %.6f] shuffled=[%.6f, %.6f]",
			dCanon.B.CILow, dCanon.B.CIHigh, dShuf.B.CILow, dShuf.B.CIHigh)
	}
	if dCanon.Significant != dShuf.Significant {
		t.Errorf("🔴 #711: GET /scores/compare published DIFFERENT `significant` verdicts for the SAME logical data inserted in two different orders: canonical=%v shuffled=%v.\ncanonical A ci=[%.6f, %.6f] B ci=[%.6f, %.6f]\nshuffled  A ci=[%.6f, %.6f] B ci=[%.6f, %.6f]\nThis is the published boolean #711 is about — not a rounding artifact.",
			dCanon.Significant, dShuf.Significant,
			dCanon.A.CILow, dCanon.A.CIHigh, dCanon.B.CILow, dCanon.B.CIHigh,
			dShuf.A.CILow, dShuf.A.CIHigh, dShuf.B.CILow, dShuf.B.CIHigh)
	}
	// And the verdict must be the TRUE one. Without this the test would still pass
	// if a future change made both stores report `false` — the intervals having
	// drifted apart into a regime where no permutation can flip anything.
	if !dCanon.Significant {
		t.Errorf("#711: `significant` is false in the ordered reading (A ci=[%.6f, %.6f], B ci=[%.6f, %.6f]) — the fixture is no longer in the marginal-disjoint regime this test needs, so the flip it exists to demonstrate is no longer reachable",
			dCanon.A.CILow, dCanon.A.CIHigh, dCanon.B.CILow, dCanon.B.CIHigh)
	}
}
