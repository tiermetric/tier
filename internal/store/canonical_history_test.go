package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// histT is hour h of 2026-09-01 UTC, the clock the #975 tests step through.
func histT(h int) time.Time { return time.Date(2026, 9, 1, h, 0, 0, 0, time.UTC) }

// historyPairs returns canonical_id_history in insertion order as
// "alias>canonical (since_seal,ended_seal]", with "?" for an unknown start and
// "" for an active link's end.
func historyPairs(t *testing.T, q interface {
	Query(string, ...any) (*sql.Rows, error)
}) []string {
	t.Helper()
	rows, err := q.Query(`SELECT alias || '>' || canonical || ' (' || COALESCE(since_seal, '?') || ',' ||
		COALESCE(ended_seal, '') || ']' FROM canonical_id_history ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read canonical_id_history: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// link renders one historyPairs entry from its seal-id watermarks; a negative
// one is an unknown start or an active end.
func link(alias, canonical string, since, ended int) string {
	s, e := "?", ""
	if since >= 0 {
		s = strconv.Itoa(since)
	}
	if ended >= 0 {
		e = strconv.Itoa(ended)
	}
	return alias + ">" + canonical + " (" + s + "," + e + "]"
}

// upsertAt runs UpsertDeveloperAlias with the clock at hour h.
func upsertAt(t *testing.T, db *DB, h int, alias, canonical string) {
	t.Helper()
	setClock(db, histT(h))
	if err := db.UpsertDeveloperAlias(context.Background(), alias, canonical, "test:fixture"); err != nil {
		t.Fatalf("UpsertDeveloperAlias(%s -> %s): %v", alias, canonical, err)
	}
}

// deleteAt runs DeleteDeveloperAlias with the clock at hour h.
func deleteAt(t *testing.T, db *DB, h int, alias string) {
	t.Helper()
	setClock(db, histT(h))
	if found, err := db.DeleteDeveloperAlias(context.Background(), alias, "test:fixture"); err != nil || !found {
		t.Fatalf("DeleteDeveloperAlias(%s) = %v, %v", alias, found, err)
	}
}

// sealAt seals month m for the given persons with the clock at hour h.
func sealAt(t *testing.T, db *DB, h, m int, persons ...string) int64 {
	t.Helper()
	setClock(db, histT(h))
	held := map[string][]heldAt{}
	for _, p := range persons {
		held[p] = []heldAt{{"core", "cost"}}
	}
	return sealPersons(t, db, m, held)
}

// liveKey reports whether id's month-m key is still held, untombstoned.
func liveKey(t *testing.T, db *DB, reportID int64, m int, id string) bool {
	t.Helper()
	r, ok := personRows(t, db, reportID)["core/cost/"+hex.EncodeToString(keyOf(t, db, m, id))]
	return ok && r.tombstoned == 0
}

// TestCanonicalHistory_AliasEditRecordsRetired pins #975: every alias edit that
// changes a mapping ends the alias's active link and (unless it deletes the
// alias) starts a new one at the seal watermark, in its own transaction; a
// re-point back starts a fresh link, and a no-op re-upsert or a refused edit
// records nothing.
func TestCanonicalHistory_AliasEditRecordsRetired(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	upsertAt(t, db, 1, "alice-gh", "alice")
	upsertAt(t, db, 2, "alice-gh", "alice.smith")
	upsertAt(t, db, 3, "alice-gh", "alice")
	upsertAt(t, db, 4, "bob-gh", "bob")
	setClock(db, histT(5))
	if err := db.UpsertDeveloperAlias(context.Background(), "carol-gh", "bob-gh", "test:fixture"); err == nil {
		t.Fatal("control: a chained alias was accepted")
	}
	deleteAt(t, db, 6, "bob-gh")
	upsertAt(t, db, 7, "alice-gh", "alice")
	sealAt(t, db, 8, 5, "alice")
	upsertAt(t, db, 9, "alice-gh", "alice.smith")
	want := []string{
		link("alice-gh", "alice", 0, 0),
		link("alice-gh", "alice.smith", 0, 0),
		link("alice-gh", "alice", 0, 1),
		link("bob-gh", "bob", 0, 0),
		link("alice-gh", "alice.smith", 1, -1),
	}
	if got := historyPairs(t, db.db); !reflect.DeepEqual(got, want) {
		t.Errorf("canonical_id_history =\n%v\nwant\n%v", got, want)
	}
}

// TestCanonicalHistory_AppendOnly pins the #604-style guards: an UPDATE that
// rewrites a link is refused, and so is an INSERT that is already ended or that
// collides with the alias's active row or an explicit rowid — INSERT OR REPLACE
// would otherwise delete that row without firing its UPDATE trigger.
func TestCanonicalHistory_AppendOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	upsertAt(t, db, 1, "alice-gh", "alice")
	var rowid int64
	if err := db.db.QueryRow(`SELECT rowid FROM canonical_id_history WHERE alias = 'alice-gh'`).Scan(&rowid); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"update":           `UPDATE canonical_id_history SET canonical = 'mallory'`,
		"replace by key":   `INSERT OR REPLACE INTO canonical_id_history (alias, canonical, since_seal) VALUES ('alice-gh', 'alice', 7)`,
		"replace by rowid": `INSERT OR REPLACE INTO canonical_id_history (rowid, alias, canonical, since_seal) VALUES (` + strconv.FormatInt(rowid, 10) + `, 'x', 'y', 7)`,
		"insert ended":     `INSERT INTO canonical_id_history (alias, canonical, since_seal, ended_seal) VALUES ('x', 'y', NULL, 7)`,
	} {
		_, err := db.db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want the append-only refusal", name, err)
		}
	}
	if got, want := historyPairs(t, db.db), []string{link("alice-gh", "alice", 0, -1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("canonical_id_history = %v, want %v", got, want)
	}
}

// TestCanonicalHistory_EndedAtSetOnceOnly pins the operator's link-interval
// ruling: the one UPDATE admitted sets an active link's ended_seal; an ended link's
// end can be neither changed nor cleared, the transition cannot carry another
// column's change, and a re-point after a delete starts a new row rather than
// reopening the old one.
func TestCanonicalHistory_EndedAtSetOnceOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	upsertAt(t, db, 1, "a", "c1")
	deleteAt(t, db, 2, "a")
	upsertAt(t, db, 3, "b", "c2")
	for name, stmt := range map[string]string{
		"change an end":      `UPDATE canonical_id_history SET ended_seal = 9 WHERE alias = 'a'`,
		"clear an end":       `UPDATE canonical_id_history SET ended_seal = NULL WHERE alias = 'a'`,
		"end and re-point":   `UPDATE canonical_id_history SET ended_seal = 9, canonical = 'z' WHERE alias = 'b'`,
		"end and re-date":    `UPDATE canonical_id_history SET ended_seal = 9, since_seal = NULL WHERE alias = 'b'`,
		"active, no end set": `UPDATE canonical_id_history SET since_seal = 7 WHERE alias = 'b'`,
	} {
		_, err := db.db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "only ending an active link") {
			t.Errorf("%s: err = %v, want the end-only refusal", name, err)
		}
	}
	upsertAt(t, db, 4, "a", "c1")
	want := []string{link("a", "c1", 0, 0), link("b", "c2", 0, -1), link("a", "c1", 0, -1)}
	if got := historyPairs(t, db.db); !reflect.DeepEqual(got, want) {
		t.Errorf("canonical_id_history = %v, want %v", got, want)
	}
	if _, err := db.db.Exec(`UPDATE canonical_id_history SET ended_seal = 9 WHERE alias = 'b'`); err != nil {
		t.Errorf("control: ending an active link was refused: %v", err)
	}
}

// TestErase_RetiredCanonicalOnlyWithinInterval pins the operator's ruling: a
// retired canonical's key is tombstoned only in periods sealed while the link
// was active, (since_seal, ended_seal] on sealed_report.id.
func TestErase_RetiredCanonicalOnlyWithinInterval(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	may := sealAt(t, db, 1, 5, "alice")
	upsertAt(t, db, 2, "alice-gh", "alice")
	june := sealAt(t, db, 2, 6, "alice") // sealed at the link's start: inside
	july := sealAt(t, db, 3, 7, "alice")
	upsertAt(t, db, 4, "alice-gh", "alice.smith")
	aug := sealAt(t, db, 4, 8, "alice") // sealed at the link's end: outside
	sep := sealAt(t, db, 5, 9, "alice")

	if n := eraseCounts(t, db, "alice.smith")["sealed_person"]; n != 2 {
		t.Errorf("counts[sealed_person] = %d, want 2 (June and July)", n)
	}
	for _, c := range []struct {
		name     string
		id       int64
		m        int
		wantLive bool
	}{{"May", may, 5, true}, {"June", june, 6, false}, {"July", july, 7, false}, {"August", aug, 8, true}, {"September", sep, 9, true}} {
		if got := liveKey(t, db, c.id, c.m, "alice"); got != c.wantLive {
			t.Errorf("%s: alice's key live = %v, want %v", c.name, got, c.wantLive)
		}
	}
}

// TestErase_RevertedMistakeTouchesNothing pins the operator's ruling: an alias
// created and deleted between two seals was active at neither, so erasing it
// touches none of the canonical's keys, which a later erase of the canonical
// still finds.
func TestErase_RevertedMistakeTouchesNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	july := sealAt(t, db, 1, 7, "bob")
	upsertAt(t, db, 2, "bobb", "bob")
	deleteAt(t, db, 3, "bobb")
	aug := sealAt(t, db, 4, 8, "bob")

	if n := eraseCounts(t, db, "bobb")["sealed_person"]; n != 0 {
		t.Errorf("erasing bobb: counts[sealed_person] = %d, want 0", n)
	}
	if !liveKey(t, db, july, 7, "bob") || !liveKey(t, db, aug, 8, "bob") {
		t.Errorf("bob's keys live: July %v, August %v, want both", liveKey(t, db, july, 7, "bob"), liveKey(t, db, aug, 8, "bob"))
	}
	if n := eraseCounts(t, db, "bob")["sealed_person"]; n != 2 {
		t.Errorf("erasing bob afterwards: counts[sealed_person] = %d, want 2", n)
	}
}

// TestErase_HistoryRowNamingErasedCanonicalDeleted pins the canonical arm of the
// erase's history delete: an alias that pointed at the erased person and now
// points at someone else keeps its current link, but the row naming the erased
// canonical goes, and is counted.
func TestErase_HistoryRowNamingErasedCanonicalDeleted(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	upsertAt(t, db, 1, "y", "alice.smith")
	upsertAt(t, db, 2, "y", "bob")

	counts := eraseCounts(t, db, "alice.smith")
	if got, want := historyPairs(t, db.db), []string{link("y", "bob", 0, -1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("canonical_id_history = %v, want %v", got, want)
	}
	if n := counts["canonical_id_history"]; n != 1 {
		t.Errorf("counts[canonical_id_history] = %d, want 1", n)
	}
}

// TestErase_RetiredIdNowOwnedByOtherPersonNotErased pins #975's collision rule:
// alice is a canonical the erased person's alias linked to when July was sealed,
// and a different person (Q, alice.jones) now owns it as a raw id. The erase
// tombstones July's key for alice but deletes none of Q's live rows, alias or
// history, and leaves Q's own key live.
func TestErase_RetiredIdNowOwnedByOtherPersonNotErased(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	seedDeveloperPII(t, db, "alice-gh")
	upsertAt(t, db, 1, "alice-gh", "alice")
	july := sealAt(t, db, 2, 7, "alice", "alice.jones")
	aliceKey := keyOf(t, db, 7, "alice")
	upsertAt(t, db, 3, "alice-gh", "alice.smith")
	upsertAt(t, db, 4, "alice", "alice.jones")
	seedDeveloperPII(t, db, "alice")
	seedDeveloperPII(t, db, "alice.jones")

	counts := eraseCounts(t, db, "alice.smith")
	if counts["token_events"] != 1 {
		t.Errorf("counts[token_events] = %d, want 1 (alice-gh's row only)", counts["token_events"])
	}
	var qRows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM token_events WHERE developer IN ('alice', 'alice.jones')`).Scan(&qRows); err != nil || qRows != 2 {
		t.Errorf("Q's token_events rows = %d (err %v), want 2: the erase deleted a different person's rows", qRows, err)
	}
	if aliases, err := db.DeveloperAliases(ctx); err != nil || !reflect.DeepEqual(aliases, map[string]string{"alice": "alice.jones"}) {
		t.Errorf("developer_alias = %v (err %v), want only Q's alice -> alice.jones", aliases, err)
	}
	if got, want := historyPairs(t, db.db), []string{link("alice", "alice.jones", 1, -1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("canonical_id_history = %v, want only Q's %v", got, want)
	}
	if counts["sealed_person"] != 1 {
		t.Errorf("counts[sealed_person] = %d, want 1 (the retired id alice)", counts["sealed_person"])
	}
	tombstonesOf(t, db, july, aliceKey)
	if !liveKey(t, db, july, 7, "alice.jones") {
		t.Error("Q's own key was tombstoned, want live")
	}
}

// TestCanonicalHistory_BackfillFromCurrentAliases pins #975's upgrade: opening a
// version-3 database, which has no history table, records every CURRENT alias
// mapping as an active link with an unknown start — nothing else, since a link
// ended before the upgrade was never recorded — stamps a version above 3 (which a
// version-3 binary refuses) with UpgradeNoticeV975 and every later notice, and a later open adds
// nothing and prints nothing. An unknown start is unbounded below: a period
// sealed before the upgrade is inside the link.
func TestCanonicalHistory_BackfillFromCurrentAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	may := sealAt(t, db, 1, 5, "alice.smith")
	if _, err := db.db.Exec(`DROP TABLE canonical_id_history;
		INSERT INTO developer_alias (alias, canonical, ts) VALUES
		    ('alice-gh', 'alice.smith', '2026-06-01 10:00:00'), ('bob-gh', 'bob', '2026-06-02 11:00:00')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	setUserVersion(t, path, 3)

	want := []string{link("alice-gh", "alice.smith", -1, -1), link("bob-gh", "bob", -1, -1)}
	for open := 1; open <= 2; open++ {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		wantNotice := map[int]string{1: UpgradeNoticeV975 + "\n" + UpgradeNoticeSealedGap + "\n" + UpgradeNoticeSourceWatermark, 2: ""}[open]
		if n := db.UpgradeNotice(); n != wantNotice {
			t.Errorf("open %d: UpgradeNotice = %q, want %q", open, n, wantNotice)
		}
		if got := historyPairs(t, db.db); !reflect.DeepEqual(got, want) {
			t.Errorf("open %d: canonical_id_history = %v, want %v", open, got, want)
		}
		if open == 2 {
			break
		}
		_ = db.Close()
		if v := readUserVersion(t, path); v <= 3 {
			t.Errorf("open %d: user_version = %d, want above 3 so a version-3 binary refuses it", open, v)
		}
	}
	defer func() { _ = db.Close() }()
	upsertAt(t, db, 5, "alice-gh", "alice.new")
	if n := eraseCounts(t, db, "alice.new")["sealed_person"]; n != 1 {
		t.Errorf("counts[sealed_person] = %d, want 1: May's alice.smith key, sealed before the backfilled link's unknown start", n)
	}
	if liveKey(t, db, may, 5, "alice.smith") {
		t.Error("May's alice.smith key is still live")
	}
}

// TestErase_SameSecondSealAndAliasEditOrderedByTransaction pins the engine
// review's RED (Codex 97dd82f2): a seal and an alias edit inside one second are
// ordered by the transaction that committed them, not by their whole-second
// timestamps. July is sealed at .1 and alice-gh re-pointed away from alice at
// .9, so July was sealed while the link was active; August is sealed at .1 and
// bob-gh first linked to bob at .9, so August was sealed before the link.
func TestErase_SameSecondSealAndAliasEditOrderedByTransaction(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	at := func(h, ms int) time.Time { return histT(h).Add(time.Duration(ms) * time.Millisecond) }
	upsertAt(t, db, 1, "alice-gh", "alice")
	setClock(db, at(2, 100))
	july := sealPersons(t, db, 7, map[string][]heldAt{"alice": {{"core", "cost"}}})
	setClock(db, at(2, 900))
	if err := db.UpsertDeveloperAlias(context.Background(), "alice-gh", "alice.smith", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	setClock(db, at(3, 100))
	aug := sealPersons(t, db, 8, map[string][]heldAt{"bob": {{"core", "cost"}}})
	setClock(db, at(3, 900))
	if err := db.UpsertDeveloperAlias(context.Background(), "bob-gh", "bob", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	upsertAt(t, db, 4, "bob-gh", "bob.new")

	if n := eraseCounts(t, db, "alice.smith")["sealed_person"]; n != 1 || liveKey(t, db, july, 7, "alice") {
		t.Errorf("erasing alice.smith: counts[sealed_person] = %d, July's alice key live = %v; want 1 and tombstoned (sealed before the edit that ended the link)", n, liveKey(t, db, july, 7, "alice"))
	}
	if n := eraseCounts(t, db, "bob.new")["sealed_person"]; n != 0 || !liveKey(t, db, aug, 8, "bob") {
		t.Errorf("erasing bob.new: counts[sealed_person] = %d, August's bob key live = %v; want 0 and live (sealed before the edit that started the link)", n, liveKey(t, db, aug, 8, "bob"))
	}
}

// TestErase_DeletedAliasLinkNotFollowed pins the one-hop scope docs/privacy.md
// discloses: links are followed only from the ids held at erase time, so a
// period attributed through an alias since deleted keeps its key live, and the
// documented remedy — erasing the retired id by name — tombstones it.
func TestErase_DeletedAliasLinkNotFollowed(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	upsertAt(t, db, 1, "alice-gh", "alice")
	july := sealAt(t, db, 2, 7, "alice")
	deleteAt(t, db, 3, "alice-gh")
	upsertAt(t, db, 4, "alice-work", "alice.smith")

	if n := eraseCounts(t, db, "alice.smith")["sealed_person"]; n != 0 || !liveKey(t, db, july, 7, "alice") {
		t.Errorf("erasing alice.smith: counts[sealed_person] = %d, July's alice key live = %v; want 0 and live", n, liveKey(t, db, july, 7, "alice"))
	}
	if n := eraseCounts(t, db, "alice")["sealed_person"]; n != 1 || liveKey(t, db, july, 7, "alice") {
		t.Errorf("erasing alice by name: counts[sealed_person] = %d, July's alice key live = %v; want 1 and tombstoned", n, liveKey(t, db, july, 7, "alice"))
	}
}
