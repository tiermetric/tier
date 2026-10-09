package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// heldAt is one (label, measure) a person id is sealed under.
type heldAt struct{ label, measure string }

// sealPersons seals month m of 2026 under cfg-a with one rollup per label and
// one person per id and heldAt, at the current erase epoch.
func sealPersons(t *testing.T, db *DB, m int, held map[string][]heldAt) int64 {
	t.Helper()
	ctx := context.Background()
	var persons []SealedPerson
	labels := map[string]bool{}
	for id, at := range held {
		for _, h := range at {
			persons = append(persons, SealedPerson{Label: h.label, Measure: h.measure, CanonicalID: id})
			labels[h.label] = true
		}
	}
	var rollups []SealedRollup
	for l := range labels {
		rollups = append(rollups, SealedRollup{Label: l, WeightedPoints: 4.5, TotalCostUSD: 2.25, ActualPaidUSD: 1, RealtimeUSD: 1.25, SampleN: 5, FlaggedOutcomes: 1})
	}
	epoch, err := db.EraseEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(m), sealMonth(m+1), "cfg-a", "body"), rollups, persons, SealCheck{EraseEpoch: epoch, Floor: sealMonth(m)})
	if err != nil {
		t.Fatalf("SealReport month %d: %v", m, err)
	}
	return r.ID
}

type personRow struct {
	label, measure string
	key            []byte
	tombstoned     int
}

// personRows returns a period's sealed_person rows keyed "label/measure/hexkey".
func personRows(t *testing.T, db *DB, reportID int64) map[string]personRow {
	t.Helper()
	rows, err := db.db.Query(`SELECT label, measure, person_key, tombstoned FROM sealed_person WHERE report_id = ?`, reportID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]personRow{}
	for rows.Next() {
		var r personRow
		if err := rows.Scan(&r.label, &r.measure, &r.key, &r.tombstoned); err != nil {
			t.Fatal(err)
		}
		out[r.label+"/"+r.measure+"/"+hex.EncodeToString(r.key)] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// keyOf is the person key id holds in month m under db's install secret.
func keyOf(t *testing.T, db *DB, m int, id string) []byte {
	t.Helper()
	secret, err := installSecret(context.Background(), db.db)
	if err != nil {
		t.Fatal(err)
	}
	return personKey(secret, "month", sealMonth(m), id)
}

// sealedKeyHex is keyOf as SQLite's hex() renders it.
func sealedKeyHex(t *testing.T, db *DB, m int, id string) string {
	t.Helper()
	return strings.ToUpper(hex.EncodeToString(keyOf(t, db, m, id)))
}

// tombstonesOf returns the tombstoned rows of a period by label/measure, and
// fails if any row still holds one of keys.
func tombstonesOf(t *testing.T, db *DB, reportID int64, keys ...[]byte) map[string][][]byte {
	t.Helper()
	out := map[string][][]byte{}
	for _, r := range personRows(t, db, reportID) {
		for _, k := range keys {
			if bytes.Equal(r.key, k) {
				t.Errorf("report %d %s/%s still holds an erased person's key (tombstoned=%d)", reportID, r.label, r.measure, r.tombstoned)
			}
		}
		if r.tombstoned == 1 {
			out[r.label+"/"+r.measure] = append(out[r.label+"/"+r.measure], r.key)
		}
	}
	return out
}

func eraseCounts(t *testing.T, db *DB, id string) map[string]int64 {
	t.Helper()
	counts, err := db.EraseDeveloper(context.Background(), id)
	if err != nil {
		t.Fatalf("EraseDeveloper(%s): %v", id, err)
	}
	return counts
}

// TestErase_OneTombstonePerPeriodAcrossLabels pins #914 ruling A: an erase
// replaces the person's key in a period with ONE fresh tombstone under every
// label and measure they held there, deletes nothing, leaves other people's
// keys alone, and a second erase tombstones nothing.
func TestErase_OneTombstonePerPeriodAcrossLabels(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	july := sealPersons(t, db, 7, map[string][]heldAt{
		"alice": {{"core", "cost"}, {"core", "points"}, {"infra", "cost"}},
		"bob":   {{"core", "cost"}, {"infra", "cost"}},
	})
	alice, bob := keyOf(t, db, 7, "alice"), keyOf(t, db, 7, "bob")

	if n := eraseCounts(t, db, "alice")["sealed_person"]; n != 3 {
		t.Errorf("counts[sealed_person] = %d, want 3 (alice's three sealed rows)", n)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sealed_person`, 5)
	tombs := tombstonesOf(t, db, july, alice)
	if len(tombs) != 3 {
		t.Fatalf("tombstoned rows by label/measure = %v, want core/cost, core/points and infra/cost", tombs)
	}
	var tomb []byte
	for at, ks := range tombs {
		if len(ks) != 1 || len(ks[0]) != len(alice) {
			t.Fatalf("%s: tombstones %x, want one %d-byte key", at, ks, len(alice))
		}
		if tomb == nil {
			tomb = ks[0]
		} else if !bytes.Equal(ks[0], tomb) {
			t.Errorf("%s: tombstone %x differs from %x — the ruling reuses one tombstone per period across labels and measures", at, ks[0], tomb)
		}
	}
	if bytes.Equal(tomb, bob) {
		t.Fatalf("control: the tombstone equals bob's key")
	}
	for _, at := range []string{"core/cost", "infra/cost"} {
		if r, ok := personRows(t, db, july)[at+"/"+hex.EncodeToString(bob)]; !ok || r.tombstoned != 0 {
			t.Errorf("bob's %s row = %+v (present %v), want untouched", at, r, ok)
		}
	}

	if n := eraseCounts(t, db, "alice")["sealed_person"]; n != 0 {
		t.Errorf("second erase: counts[sealed_person] = %d, want 0", n)
	}
}

// TestErase_TombstonesDifferAcrossPeriods: each period gets its own fresh
// tombstone, so a person's tombstones cannot be linked across periods.
func TestErase_TombstonesDifferAcrossPeriods(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	held := map[string][]heldAt{"alice": {{"core", "cost"}}, "bob": {{"core", "cost"}}}
	july, aug := sealPersons(t, db, 7, held), sealPersons(t, db, 8, held)

	if n := eraseCounts(t, db, "alice")["sealed_person"]; n != 2 {
		t.Errorf("counts[sealed_person] = %d, want 2 (one row in each period)", n)
	}
	j := tombstonesOf(t, db, july, keyOf(t, db, 7, "alice"))["core/cost"]
	a := tombstonesOf(t, db, aug, keyOf(t, db, 8, "alice"))["core/cost"]
	if len(j) != 1 || len(a) != 1 {
		t.Fatalf("tombstones July %x, August %x, want one each", j, a)
	}
	if bytes.Equal(j[0], a[0]) {
		t.Errorf("July and August share tombstone %x, want one fresh tombstone per period", j[0])
	}
}

// sealedBytes dumps every sealed_report and sealed_rollup column, byte for byte.
func sealedBytes(t *testing.T, db *DB) string {
	t.Helper()
	var b bytes.Buffer
	for _, q := range []string{
		`SELECT id || '|' || level || '|' || period_size || '|' || period_start || '|' || period_end || '|' || k || '|' ||
			config_digest || '|' || sealed_at || '|' || hex(body) || '|' || body_digest || '|' || tool_version || '|' || tool_commit
			FROM sealed_report ORDER BY id`,
		`SELECT report_id || '|' || label || '|' || quote(weighted_points) || '|' ||
			quote(total_cost_usd) || '|' || quote(actual_paid_usd) || '|' || quote(realtime_usd) || '|' || sample_n || '|' || flagged_outcomes
			FROM sealed_rollup ORDER BY report_id, label`,
	} {
		rows, err := db.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(s + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return b.String()
}

// TestErase_SealedBodyAndRollupsByteIdentical: an erase never touches a sealed
// body or a pre-fold rollup (#914 ruling A).
func TestErase_SealedBodyAndRollupsByteIdentical(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	sealPersons(t, db, 7, map[string][]heldAt{"alice": {{"core", "cost"}, {"infra", "cost"}}})
	sealPersons(t, db, 8, map[string][]heldAt{"alice": {{"core", "points"}}, "bob": {{"core", "points"}}})
	before := sealedBytes(t, db)
	if n := len(bytes.Split([]byte(before), []byte("\n"))) - 1; n != 5 {
		t.Fatalf("control: dumped %d sealed_report and sealed_rollup rows, want 5", n)
	}

	if n := eraseCounts(t, db, "alice")["sealed_person"]; n != 3 {
		t.Fatalf("counts[sealed_person] = %d, want 3", n)
	}
	if after := sealedBytes(t, db); after != before {
		t.Errorf("sealed bodies or rollups changed across the erase:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// TestErase_AliasedIdsTombstoned: an erase tombstones the keys of every id in
// the person's identifier set, whichever id names them. A raw id sealed as a
// separate person before it was aliased holds its own key, even under the
// same label and measure as the canonical id: both rows are tombstoned, no row
// is deleted, and they keep different tombstones (one would take the other's
// primary key), so the period still counts what it was sealed with.
func TestErase_AliasedIdsTombstoned(t *testing.T) {
	for _, eraseBy := range []string{"alice", "alice-gh"} {
		t.Run("by-"+eraseBy, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			july := sealPersons(t, db, 7, map[string][]heldAt{
				"alice":    {{"core", "cost"}},
				"alice-gh": {{"core", "cost"}, {"infra", "points"}},
				"bob":      {{"core", "cost"}},
			})
			if err := db.UpsertDeveloperAlias(context.Background(), "alice-gh", "alice", "test:fixture"); err != nil {
				t.Fatal(err)
			}

			if n := eraseCounts(t, db, eraseBy)["sealed_person"]; n != 3 {
				t.Errorf("counts[sealed_person] = %d, want 3 (alice's and alice-gh's rows)", n)
			}
			assertCount(t, db, `SELECT COUNT(*) FROM sealed_person`, 4)
			tombs := tombstonesOf(t, db, july, keyOf(t, db, 7, "alice"), keyOf(t, db, 7, "alice-gh"))
			core, infra := tombs["core/cost"], tombs["infra/points"]
			if len(core) != 2 || len(infra) != 1 {
				t.Fatalf("tombstones core/cost %x, infra/points %x, want two and one", core, infra)
			}
			if bytes.Equal(core[0], core[1]) {
				t.Errorf("core/cost: both rows hold tombstone %x, want two distinct", core[0])
			}
			if !bytes.Equal(core[0], infra[0]) && !bytes.Equal(core[1], infra[0]) {
				t.Errorf("alice-gh's infra/points tombstone %x matches neither core/cost tombstone %x — one key's rows must share one tombstone", infra[0], core)
			}
			bob := keyOf(t, db, 7, "bob")
			if r, ok := personRows(t, db, july)["core/cost/"+hex.EncodeToString(bob)]; !ok || r.tombstoned != 0 {
				t.Errorf("bob's row = %+v (present %v), want untouched", r, ok)
			}
		})
	}
}

// TestErase_NoSealedRowsIsNoop: with no sealed period, or none holding the
// person, an erase tombstones nothing and still succeeds.
func TestErase_NoSealedRowsIsNoop(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedDeveloperPII(t, db, "alice")
	counts := eraseCounts(t, db, "alice")
	if n, ok := counts["sealed_person"]; !ok || n != 0 {
		t.Errorf("no sealed periods: counts[sealed_person] = %d (present %v), want 0", n, ok)
	}
	if counts["token_events"] == 0 {
		t.Errorf("control: the erase deleted no token_events, so it did not run on seeded data: %v", counts)
	}

	july := sealPersons(t, db, 7, map[string][]heldAt{"bob": {{"core", "cost"}}})
	before := personRows(t, db, july)
	if n := eraseCounts(t, db, "alice")["sealed_person"]; n != 0 {
		t.Errorf("period without alice: counts[sealed_person] = %d, want 0", n)
	}
	after := personRows(t, db, july)
	if len(after) != len(before) {
		t.Fatalf("sealed_person rows %v, want %v", after, before)
	}
	for k, r := range before {
		if a, ok := after[k]; !ok || a.tombstoned != r.tombstoned {
			t.Errorf("row %s = %+v (present %v), want %+v", k, a, ok, r)
		}
	}
}

// TestPersonKey_PinnedEncoding pins the person key's bytes: HMAC-SHA256 keyed
// by the install secret over period_size ‖ 0x00 ‖ period_start (RFC 3339 UTC,
// whole seconds) ‖ 0x00 ‖ canonical_id. The vector was computed outside Go
// (Python hmac and openssl dgst -mac HMAC agree). A sealer and a later erase
// must derive the same key, so any change to these bytes is a break.
func TestPersonKey_PinnedEncoding(t *testing.T) {
	secret := make([]byte, installSecretLen)
	for i := range secret {
		secret[i] = byte(i)
	}
	const want = "ee5c714da47f3b1dc85cb88af121df55bca60284457b2a94943c7c70e9b4d779"
	july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if got := hex.EncodeToString(personKey(secret, "month", july, "alice")); got != want {
		t.Errorf("personKey(month, 2026-07-01T00:00:00Z, alice) = %s, want %s", got, want)
	}
	edt := time.FixedZone("EDT", -4*3600)
	if got := hex.EncodeToString(personKey(secret, "month", july.In(edt), "alice")); got != want {
		t.Errorf("the same instant in another zone = %s, want %s (period_start is encoded in UTC)", got, want)
	}

	base := personKey(secret, "month", july, "alice")
	for name, k := range map[string][]byte{
		"another period":      personKey(secret, "month", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "alice"),
		"another period size": personKey(secret, "quarter", july, "alice"),
		"another id":          personKey(secret, "month", july, "alice-gh"),
		"another secret":      personKey(make([]byte, installSecretLen), "month", july, "alice"),
	} {
		if bytes.Equal(k, base) {
			t.Errorf("%s gives the same key %x", name, k)
		}
	}

	// SealReport stores exactly this key for the period it seals.
	db, cleanup := newTestDB(t)
	defer cleanup()
	july7 := sealPersons(t, db, 7, map[string][]heldAt{"alice": {{"core", "cost"}}})
	if _, ok := personRows(t, db, july7)["core/cost/"+hex.EncodeToString(keyOf(t, db, 7, "alice"))]; !ok {
		t.Errorf("sealed rows %v, want alice's personKey under this database's install secret", personRows(t, db, july7))
	}
}

// TestErase_TombstoneIsFreshRandom: a tombstone is drawn at random, never
// derived from the install secret, the period or the id — any such derivation
// would let whoever holds the file recompute it from a guessed id. Two copies
// of one database (one install secret, the same rows) erasing the same person
// must write different tombstones.
func TestErase_TombstoneIsFreshRandom(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	july := sealPersons(t, db, 7, map[string][]heldAt{"alice": {{"core", "cost"}}, "bob": {{"core", "cost"}}})
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if _, err := db.db.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	other, err := Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if !bytes.Equal(keyOf(t, other, 7, "alice"), keyOf(t, db, 7, "alice")) {
		t.Fatal("control: the copy does not share the install secret")
	}

	var tombs [][]byte
	for _, d := range []*DB{db, other} {
		if n := eraseCounts(t, d, "alice")["sealed_person"]; n != 1 {
			t.Fatalf("counts[sealed_person] = %d, want 1", n)
		}
		ks := tombstonesOf(t, d, july, keyOf(t, d, 7, "alice"))["core/cost"]
		if len(ks) != 1 {
			t.Fatalf("core/cost tombstones %x, want one", ks)
		}
		tombs = append(tombs, ks[0])
	}
	if bytes.Equal(tombs[0], tombs[1]) {
		t.Errorf("both copies wrote tombstone %x for alice: it is derived, not fresh random", tombs[0])
	}
}

// TestErase_RetiredCanonicalIdTombstoned pins #975 (closing DEBT
// 2026-09-29-913b-1): a canonical id that an alias edit retired after sealing is
// found through canonical_id_history, so erasing the current canonical
// tombstones the retired id's sealed key in a period sealed while the link was
// active, and deletes the person's history.
func TestErase_RetiredCanonicalIdTombstoned(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedDeveloperPII(t, db, "alice-gh")
	upsertAt(t, db, 1, "alice-gh", "alice")
	july := sealAt(t, db, 2, 7, "alice", "bob")
	aliceKey := keyOf(t, db, 7, "alice")
	bob := "core/cost/" + hex.EncodeToString(keyOf(t, db, 7, "bob"))
	upsertAt(t, db, 3, "alice-gh", "alice.smith")

	counts := eraseCounts(t, db, "alice.smith")
	if counts["token_events"] == 0 {
		t.Fatalf("control: erasing alice.smith deleted no token_events, so it did not reach alice-gh: %v", counts)
	}
	if n := counts["sealed_person"]; n != 1 {
		t.Errorf("counts[sealed_person] = %d, want 1: the retired id alice's key", n)
	}
	if got := tombstonesOf(t, db, july, aliceKey)["core/cost"]; len(got) != 1 {
		t.Errorf("core/cost tombstones = %d, want 1", len(got))
	}
	if r, ok := personRows(t, db, july)[bob]; !ok || r.tombstoned != 0 {
		t.Errorf("bob's sealed row = %+v (present %v), want live", r, ok)
	}
	if got := historyPairs(t, db.db); len(got) != 0 {
		t.Errorf("canonical_id_history after the erase = %v, want empty", got)
	}
	again := eraseCounts(t, db, "alice")
	if n := again["sealed_person"]; n != 0 {
		t.Errorf("erasing the retired id by name afterwards: counts[sealed_person] = %d, want 0", n)
	}
	if n, ok := again["canonical_id_history"]; !ok || n != 0 {
		t.Errorf("an erase with no history rows: counts[canonical_id_history] = %d (present %v), want present and 0", n, ok)
	}
}

// TestSealReport_PersonInsertOrderShuffled: SealReport inserts person rows in
// random order, so a row's rowid (which its tombstone keeps) does not reveal
// where the person sorted. 20 seals of 8 persons all landing in input order
// has probability (1/8!)^19 under a uniform shuffle.
func TestSealReport_PersonInsertOrderShuffled(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	persons := make([]SealedPerson, len(ids))
	for i, id := range ids {
		persons[i] = SealedPerson{Label: "core", Measure: "cost", CanonicalID: id}
	}
	const runs = 20
	shuffled := 0
	for m := 1; m <= runs; m++ {
		r, _, err := db.SealReport(ctx, sealFixture("month", sealMonth(m), sealMonth(m+1), "cfg-a", "body"), nil, persons, SealCheck{Floor: sealMonth(1)})
		if err != nil {
			t.Fatal(err)
		}
		byKey := map[string]string{}
		for _, id := range ids {
			byKey[hex.EncodeToString(keyOf(t, db, m, id))] = id
		}
		rows, err := db.db.Query(`SELECT person_key FROM sealed_person WHERE report_id = ? ORDER BY rowid`, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var k []byte
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			id, ok := byKey[hex.EncodeToString(k)]
			if !ok {
				t.Fatalf("month %d: stored key %x is no input person's key", m, k)
			}
			got = append(got, id)
		}
		_ = rows.Close()
		if len(got) != len(ids) {
			t.Fatalf("month %d: %d rows %v, want %d", m, len(got), got, len(ids))
		}
		if strings.Join(got, "") != strings.Join(ids, "") {
			shuffled++
		}
	}
	if shuffled == 0 {
		t.Errorf("all %d seals inserted persons in input order: the insert order is not shuffled", runs)
	}
}
