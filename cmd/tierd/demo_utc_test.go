package main

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite"
)

// TestSeedDemo_StampsUTCTimestamps guards the #723 fix: seedDemo derived every
// synthetic timestamp from a bare time.Now(), which carries the HOST's local
// zone. It was the only writer in the tree that did not normalize.
//
// Why a local zone is wrong here. modernc.org/sqlite stores a Go time.Time as
// TEXT in Go's own layout, "2006-01-02 15:04:05.999999999 -0700 MST", including
// the zone suffix — so `ORDER BY ts` is a binary string sort and the UTC offset
// sits AFTER the wall-clock field. A demo-seeded database is internally
// single-zone and therefore self-consistent, but an install east or west of UTC
// that seeds a demo and then ingests real (UTC) webhook rows is a MIXED-zone
// table, where the lexical sort disagrees with instant order and voids the
// (ts, id) total order #711 established. Same hazard the JSONL collector guards
// in internal/collector/jsonl_utc_test.go and parseSince guards in
// since_tz_test.go.
//
// ⚠️ time.Local is set EXPLICITLY to a nonzero-offset fixed zone and restored.
// Without that this test would pass for free on a UTC CI box and redden only for
// a developer elsewhere — which is precisely this bug wearing a test. Verified
// identical under TZ=UTC, TZ=Asia/Kolkata (+05:30) and TZ=Pacific/Chatham (+12:45).
//
// ⛔ DO NOT ADD t.Parallel() to this test or to
// TestDemoThenWebhook_OrderByTSIsInstantOrder. time.Local is PROCESS-GLOBAL, so a
// parallel test would leak this zone into every other test running beside it (and
// have its own zone yanked out from under it). It is safe today only because both
// tests are sequential: Go resumes parallel top-level tests after the sequential
// pass completes, so they never overlap with these. Package cmd/tierd does contain
// parallel tests (timeout_test.go) — none zone-sensitive. Checked with
// `-shuffle=on` and `-race`.
//
// LAYERING. actual_spend.ts is the arm that makes this guard independent: it is
// written straight from seedDemo's `now` and, unlike outcomes.ts and
// token_events.ts, is NOT covered by the store-boundary normalizeTS backstop
// (#723). So reverting demo.go's .UTC() alone reddens THIS test on the
// actual_spend arm, while reverting normalizeTS alone reddens
// TestOrderedTSWriters_NormalizeToUTC in the store — each layer fails on its own.
func TestSeedDemo_StampsUTCTimestamps(t *testing.T) {
	// A fixed -04:00 zone: deterministic, and never the machine's own.
	restore := time.Local
	time.Local = time.FixedZone("TIER-TEST-EDT", -4*60*60)
	defer func() { time.Local = restore }()

	// Vacuity control 1. If time.Now() does not actually pick up the substituted
	// zone, every assertion below passes for free and proves nothing.
	if _, off := time.Now().Zone(); off != -4*60*60 {
		t.Fatalf("time.Local substitution did not take (offset %d) — this guard "+
			"would be vacuous", off)
	}

	// Vacuity control 2 — THE INDEPENDENCE PROBE, and it guards an ABSENCE.
	//
	// This test's whole value as a guard on cmd/tierd/demo.go rests on
	// actual_spend.ts NOT being normalized at the store boundary: outcomes.ts and
	// token_events.ts are covered by normalizeTS, so for those two the store
	// would mask a reverted demo.go. actual_spend is the only arm that can still
	// fail on its own.
	//
	// That is a property of code in ANOTHER PACKAGE, and normalizeTS's own doc
	// describes itself as a backstop that "must be applied by every writer of an
	// ORDERED ts column" — a standing invitation for a future session to add it to
	// InsertActualSpend for consistency. Measured: with that change AND demo.go's
	// .UTC() reverted, every arm of this file passes and no control fires. So
	// probe the property directly rather than trusting a comment to hold.
	assertActualSpendIsNotStoreNormalized(t)

	dbPath := filepath.Join(t.TempDir(), "demo.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := seedDemo(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("seedDemo: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Read the RAW stored text, not a parsed time.Time: what SQLite compares is
	// these bytes, and a round-trip through the driver would hide the offset.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = raw.Close() }()

	for _, tbl := range []struct{ table, col string }{
		{"outcomes", "ts"},
		{"token_events", "ts"},
		{"actual_spend", "ts"}, // the independent arm — see the doc comment
	} {
		rows, err := raw.QueryContext(context.Background(),
			`SELECT CAST(`+tbl.col+` AS TEXT) FROM `+tbl.table)
		if err != nil {
			t.Fatalf("query %s: %v", tbl.table, err)
		}
		var n, bad int
		var firstBad string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				_ = rows.Close()
				t.Fatalf("scan %s: %v", tbl.table, err)
			}
			n++
			if !strings.HasSuffix(s, "+0000 UTC") {
				bad++
				if firstBad == "" {
					firstBad = s
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("rows %s: %v", tbl.table, err)
		}
		_ = rows.Close()

		// Coverage control: an empty table would make the loop above assert
		// nothing at all.
		if n == 0 {
			t.Fatalf("seedDemo wrote no rows to %s — nothing was checked", tbl.table)
		}
		if bad > 0 {
			t.Errorf("seedDemo stored %d/%d %s.%s values with a non-UTC offset; "+
				"first: %q\nORDER BY %s is a binary string sort over this text, so a "+
				"nonzero offset sorts by wall clock instead of by instant (#723).",
				bad, n, tbl.table, tbl.col, firstBad, tbl.col)
		}
	}
}

// assertActualSpendIsNotStoreNormalized probes, rather than assumes, the
// property TestSeedDemo_StampsUTCTimestamps depends on for independence: that
// the store does NOT apply normalizeTS to actual_spend.ts.
//
// It writes a nonzero-offset timestamp straight through InsertActualSpend — no
// demo code involved — and requires the offset to SURVIVE to disk. If a future
// change normalizes that writer, this fails LOUDLY here and says what to do,
// instead of silently turning the whole demo guard green on a tree where
// cmd/tierd/demo.go's .UTC() has been reverted (measured — that is exactly what
// happens without this probe).
//
// ⚠️ Note the inverted polarity: this is the ONE place in #723 that asserts a
// timestamp is NOT normalized. It is not a mistake and must not be "fixed" to
// match the others — see the DELIBERATE EXCLUSION block on normalizeTS.
func assertActualSpendIsNotStoreNormalized(t *testing.T) {
	t.Helper()
	probePath := filepath.Join(t.TempDir(), "probe.db")
	db, err := store.Open(probePath)
	if err != nil {
		t.Fatalf("probe open: %v", err)
	}
	offset := time.FixedZone("PROBE", -4*60*60)
	// Vacuity control for the probe itself: a zero-offset probe zone would make
	// the stored value read as UTC for the WRONG reason, and this helper would
	// report "still normalized" on a tree where nothing had changed.
	if _, off := time.Now().In(offset).Zone(); off == 0 {
		t.Fatal("probe zone has a zero offset — it cannot distinguish a normalized " +
			"write from an un-normalized one, so this probe is vacuous")
	}
	if err := db.InsertActualSpend(context.Background(), store.ActualSpend{
		Developer:       "probe",
		Period:          "2026-04",
		ActualPaidMicro: 1,
		Timestamp:       time.Date(2026, 4, 1, 12, 0, 0, 0, offset),
	}); err != nil {
		_ = db.Close()
		t.Fatalf("probe insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}

	raw, err := sql.Open("sqlite", probePath)
	if err != nil {
		t.Fatalf("probe raw open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var stored string
	if err := raw.QueryRowContext(context.Background(),
		`SELECT CAST(ts AS TEXT) FROM actual_spend`).Scan(&stored); err != nil {
		t.Fatalf("probe read: %v", err)
	}
	if strings.HasSuffix(stored, "+0000 UTC") {
		t.Fatalf("actual_spend.ts is now NORMALIZED at the store boundary (stored %q "+
			"from a -0400 input).\n"+
			"That silently makes TestSeedDemo_StampsUTCTimestamps VACUOUS as a guard "+
			"on cmd/tierd/demo.go: with outcomes.ts and token_events.ts already "+
			"covered by normalizeTS, actual_spend was the only arm that could still "+
			"fail when demo.go's .UTC() is reverted.\n"+
			"Either revert the normalizeTS on InsertActualSpend (see the DELIBERATE "+
			"EXCLUSION block in internal/store/store.go), or replace this test's "+
			"independence with a direct assertion on seedDemo's own stamped values.",
			stored)
	}
}

// TestDemoThenWebhook_OrderByTSIsInstantOrder is the END-TO-END arm: the actual
// #723 scenario, not a synthetic pair. It seeds a demo on a host at -04:00 (what
// the unfixed seeder produced) and then ingests a real UTC outcome the way the
// GitHub webhook does, then asserts `ORDER BY ts, id` over the resulting MIXED
// table agrees with instant order — the guarantee #711 established and #716's
// digest consumes.
//
// ⚠️ FIXTURE GEOMETRY IS LOAD-BEARING, and a first attempt at this test got it
// wrong. The inversion can only occur when two instants are CLOSER TOGETHER than
// the zone offset: at -04:00, a demo row's wall clock reads 4h below its instant,
// so a UTC row must sit within 4h of it to be overtaken. The demo merges are 3
// DAYS apart, so a webhook row placed "between two demo rows" never flips and the
// guard passes for free — measured, 0 inversions on a deliberately broken tree.
// The webhook row is therefore pinned 2h before the freshest demo merge, and the
// vacuity control below fails loudly if the demo data ever spreads out far enough
// that no such pair can exist.
func TestDemoThenWebhook_OrderByTSIsInstantOrder(t *testing.T) {
	restore := time.Local
	time.Local = time.FixedZone("TIER-TEST-EDT", -4*60*60)
	defer func() { time.Local = restore }()

	const offset = -4 * time.Hour
	if _, off := time.Now().Zone(); time.Duration(off)*time.Second != offset {
		t.Fatalf("time.Local substitution did not take (offset %d) — vacuous guard", off)
	}

	dbPath := filepath.Join(t.TempDir(), "mixed.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := seedDemo(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("seedDemo: %v", err)
	}
	// seedDemo's freshest merge is at now-7d (its `base`). Land the webhook row
	// 2h before it — inside the 4h offset window, so a wall-clock sort WOULD
	// overtake it. This is written exactly as the webhook handler writes: UTC.
	webhookAt := time.Now().AddDate(0, 0, -7).Add(-2 * time.Hour).UTC()
	inserted, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "real-dev", IssueID: "REAL-1", Weight: 1, Quality: 1,
		MergeCommitSHA: "ffffffffffffffffffffffffffffffffffffffff",
		Timestamp:      webhookAt,
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("insert webhook outcome: %v", err)
	}
	// Name the failure directly. If this row were ever deduped away, the vacuity
	// control below would fire ("no two rows within the offset") — correct, but it
	// would blame the fixture geometry for a missing row.
	if !inserted {
		_ = db.Close()
		t.Fatal("the webhook outcome was deduped away — without it there is no " +
			"mixed-zone pair and this guard is vacuous")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = raw.Close() }()

	rows, err := raw.QueryContext(context.Background(),
		`SELECT issue_id, CAST(ts AS TEXT) FROM outcomes ORDER BY ts, id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		id      string
		raw     string
		instant time.Time
	}
	var got []row
	for rows.Next() {
		var id, rawTS string
		if err := rows.Scan(&id, &rawTS); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// Parse date, clock and NUMERIC OFFSET only: the trailing zone NAME is
		// free-form and time.Parse's MST verb cannot accept an arbitrary label.
		f := strings.Fields(rawTS)
		if len(f) < 3 {
			t.Fatalf("unexpected stored ts %q", rawTS)
		}
		inst, perr := time.Parse("2006-01-02 15:04:05.999999999 -0700", f[0]+" "+f[1]+" "+f[2])
		if perr != nil {
			t.Fatalf("parse %q: %v", rawTS, perr)
		}
		got = append(got, row{id, rawTS, inst})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("only %d row(s) — nothing to order", len(got))
	}

	// VACUITY CONTROL. A wall-clock sort can only overtake an instant when two
	// instants are STRICTLY apart but by LESS than the zone offset. If the fixture
	// no longer contains such a pair (demo timings changed), this test would pass
	// on a broken tree — so fail here rather than report a green that means nothing.
	//
	// ⚠️ Two properties here are load-bearing and BOTH were measured wrong first:
	//
	//  1. The gap must be STRICTLY positive. seedDemo emits several outcomes at
	//     the IDENTICAL instant (different developers, same day offset), and a
	//     `>= 0` bound treats those 0-gap pairs as "close enough" — the control
	//     then reported a healthy fixture and PASSED on a deliberately broken
	//     tree, the exact failure it exists to prevent. Equal instants can never
	//     invert; `id` breaks that tie correctly.
	//
	//  2. It must be measured over ALL PAIRS, not adjacent rows in SQL order. On
	//     a broken tree the inverted pair is adjacent with a NEGATIVE gap, so an
	//     adjacent-pair scan skips precisely the pair that proves the geometry is
	//     right and reports "vacuous" on a fixture that is in fact correct.
	//     The geometry is a property of the instants, not of the row order.
	closest := time.Duration(math.MaxInt64)
	for i := range got {
		for j := i + 1; j < len(got); j++ {
			d := got[i].instant.Sub(got[j].instant)
			if d < 0 {
				d = -d
			}
			if d > 0 && d < closest {
				closest = d
			}
		}
	}
	if closest >= -offset {
		t.Fatalf("no two rows are STRICTLY within the %v zone offset (closest "+
			"non-zero gap %v) — a wall-clock sort cannot overtake an instant here, "+
			"so this guard is VACUOUS. Re-pin the webhook row against seedDemo's "+
			"freshest merge.", -offset, closest)
	}

	for i := 1; i < len(got); i++ {
		if got[i].instant.Before(got[i-1].instant) {
			t.Fatalf("ORDER BY ts, id is NOT in instant order (#723):\n"+
				"  position %d: %-10s raw=%q instant=%s\n"+
				"  position %d: %-10s raw=%q instant=%s\n"+
				"the later instant sorted first, because ORDER BY ts is a binary "+
				"string sort over an offset-bearing rendering",
				i-1, got[i-1].id, got[i-1].raw, got[i-1].instant.UTC().Format(time.RFC3339Nano),
				i, got[i].id, got[i].raw, got[i].instant.UTC().Format(time.RFC3339Nano))
		}
	}
}
