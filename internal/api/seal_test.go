package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// sealNow is 1 July 2026: with sealGrace, May 2026 is the latest sealable month.
var sealNow = time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

const sealGrace = 14 * 24 * time.Hour

var (
	sealMay = Period{Kind: periodMonth, Start: sealFixtureMonth}
	// sealCfg arms sealing from a month before any fixture's cost coverage, so the
	// earliest sealable month is coverage's.
	sealCfg = sealConfig{grace: sealGrace, sealFrom: Period{Kind: periodMonth, Start: time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)}}
)

// newSealFixture is seedSealFixture in team mode at k=5, plus an April cost row
// that puts the cost horizon before May, so May is the earliest full month.
func newSealFixture(t *testing.T) (*Handler, *store.DB, *sealer) {
	t.Helper()
	h, db := newTestHandler(t)
	seedSealFixture(t, db)
	seedRepoCostAt(t, db, repoAlpha, "b1", "i-early", 1, sealFixtureMonth.AddDate(0, 0, -16))
	h.SetAggregation(scoring.AggregationTeam, 5)
	return h, db, newTestSealer(h)
}

func newTestSealer(h *Handler) *sealer {
	s, err := newSealer(h, sealCfg)
	if err != nil {
		panic(err)
	}
	s.now = func() time.Time { return sealNow }
	return s
}

// seedLateMayRow adds cost inside May to a counted person of team big.
func seedLateMayRow(t *testing.T, db *store.DB) {
	t.Helper()
	seedRepoCostAt(t, db, repoAlpha, "b2", "i-b2", 5.55, sealFixtureMonth.AddDate(0, 0, 19))
}

// foldedMayBody is May's folded body computed from the store as it is now.
func foldedMayBody(t *testing.T, h *Handler) ([]byte, []scoring.LabelInput) {
	t.Helper()
	start, end := sealMay.Bounds()
	resp, inputs, err := h.scoresForWindow(context.Background(), h.store, scoresQuery{since: start, until: end, scope: store.FleetWide}, true)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return body, inputs
}

// sealPass runs one background seal pass and fails the test on its error.
func sealPass(t *testing.T, s *sealer) {
	t.Helper()
	if err := s.sealDue(context.Background()); err != nil {
		t.Fatalf("seal pass: %v", err)
	}
}

func rawSealStore(t *testing.T, db *store.DB) *sql.DB {
	t.Helper()
	p, ok := testStorePaths.Load(db)
	if !ok {
		t.Fatal("store not registered")
	}
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func rawCount(t *testing.T, raw *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestSeal_ConcurrentFirstReadsOneBody: a sealer that loses the seal race
// returns the winner's stored bytes and seal time, never its own computation;
// and racing first reads leave one sealed row and serve one body.
func TestSeal_ConcurrentFirstReadsOneBody(t *testing.T) {
	// A pooled read escaping a seal's snapshot deadlocks racing sealers on the
	// pool; the deadline turns that into a named failure.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Run("loser returns the winner's bytes", func(t *testing.T) {
		h, db, s := newSealFixture(t)
		own, _ := foldedMayBody(t, h)
		var winner []byte
		var winnerAt time.Time
		var winnerErr error
		s.beforeSeal = func() {
			s.beforeSeal = nil
			// The winner seals after a late row, so its body differs from the
			// loser's own computation, which was taken before the row.
			seedLateMayRow(t, db)
			winner, winnerAt, winnerErr = newTestSealer(h).sealOrLoad(ctx, sealMay)
		}
		got, gotAt, err := s.sealOrLoad(ctx, sealMay)
		if err != nil || winnerErr != nil {
			t.Fatalf("sealOrLoad: loser %v, winner %v", err, winnerErr)
		}
		if bytes.Equal(winner, own) {
			t.Fatal("control: the winner's body equals the loser's own computation, so this proves nothing")
		}
		if !bytes.Equal(got, winner) || !gotAt.Equal(winnerAt) {
			t.Errorf("loser returned %s at %s, want the winner's %s at %s", got, gotAt, winner, winnerAt)
		}
	})
	t.Run("racing first reads", func(t *testing.T) {
		h, db, _ := newSealFixture(t)
		const readers = 8
		type result struct {
			body []byte
			at   time.Time
			err  error
		}
		results := make(chan result, readers)
		start := make(chan struct{})
		for i := 0; i < readers; i++ {
			go func() {
				<-start
				body, at, err := newTestSealer(h).sealOrLoad(ctx, sealMay)
				results <- result{body, at, err}
			}()
		}
		close(start)
		var first *result
		completed := 0
		for i := 0; i < readers; i++ {
			r := <-results
			switch {
			case r.err == nil:
				completed++
				if first == nil {
					first = &r
				} else if !bytes.Equal(r.body, first.body) || !r.at.Equal(first.at) {
					t.Errorf("two first reads served different bodies:\n%s\n%s", first.body, r.body)
				}
			case errors.Is(r.err, store.ErrWriteLockUnavailable):
			default:
				t.Errorf("sealOrLoad: %v", r.err)
			}
		}
		if completed < 2 {
			t.Errorf("%d of %d first reads completed, want at least 2", completed, readers)
		}
		if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_report`); n != 1 {
			t.Errorf("%d sealed_report rows, want 1", n)
		}
	})
}

// TestSeal_BodyTeamsEqualRefoldOfStoredInputs: the sealed body's rows are
// AggregateFolded over the stored rollups, Has and Contributes columns and
// person sets alone, each checked equal to a recomputation of the same window.
func TestSeal_BodyTeamsEqualRefoldOfStoredInputs(t *testing.T) {
	h, db, s := newSealFixture(t)
	body, _, err := s.sealOrLoad(context.Background(), sealMay)
	if err != nil {
		t.Fatal(err)
	}
	_, recomputed := foldedMayBody(t, h)
	byLabel := map[string]scoring.LabelInput{}
	for _, in := range recomputed {
		byLabel[in.Label] = in
	}

	raw := rawSealStore(t, db)
	refold := map[string]*scoring.LabelInput{}
	rows, err := raw.Query(`SELECT label, weighted_points, total_cost_usd, actual_paid_usd, realtime_usd, sample_n, flagged_outcomes,
		has_points, has_cost, has_realtime, has_non_realtime, has_paid, contributes FROM sealed_rollup`)
	if err != nil {
		t.Fatal(err)
	}
	anyHas := false
	for rows.Next() {
		in := &scoring.LabelInput{}
		if err := rows.Scan(&in.Label, &in.Sums.WeightedPoints, &in.Sums.TotalCostUSD, &in.Sums.ActualPaidUSD,
			&in.Sums.RealtimeUSD, &in.Sums.SampleN, &in.Sums.FlaggedOutcomes,
			&in.Has[0], &in.Has[1], &in.Has[2], &in.Has[3], &in.Has[4], &in.Contributes); err != nil {
			t.Fatal(err)
		}
		re, ok := byLabel[in.Label]
		if !ok || re.Sums != in.Sums || re.Has != in.Has || re.Contributes != in.Contributes {
			t.Fatalf("label %q: stored %+v has %v contributes %v, recomputed %+v %v %v (present %v)",
				in.Label, in.Sums, in.Has, in.Contributes, re.Sums, re.Has, re.Contributes, ok)
		}
		anyHas = anyHas || in.Contributes
		for _, h := range in.Has {
			anyHas = anyHas || h
		}
		refold[in.Label] = in
	}
	_ = rows.Close()
	if !anyHas {
		t.Fatal("control: no stored Has or Contributes is set, so their columns are not checked")
	}
	measure := map[string]int{}
	for m, name := range scoring.MeasureNames {
		measure[name] = m
	}
	rows, err = raw.Query(`SELECT label, measure, person_key, tombstoned FROM sealed_person ORDER BY label, measure, person_key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var label, set string
		var key []byte
		var tombstoned int
		if err := rows.Scan(&label, &set, &key, &tombstoned); err != nil {
			t.Fatal(err)
		}
		if len(key) != 32 || tombstoned != 0 {
			t.Fatalf("%s/%s: key %x tombstoned %d, want a live 32-byte key", label, set, key, tombstoned)
		}
		in, ok := refold[label]
		if !ok {
			t.Fatalf("person row under label %q with no rollup", label)
		}
		if set == scoring.PeopleSetName {
			in.People = append(in.People, hex.EncodeToString(key))
		} else if m, ok := measure[set]; ok {
			in.Carriers[m] = append(in.Carriers[m], hex.EncodeToString(key))
		} else {
			t.Fatalf("person row under unknown set %q", set)
		}
	}
	_ = rows.Close()
	var inputs []scoring.LabelInput
	for _, in := range refold {
		if len(in.People) != len(byLabel[in.Label].People) {
			t.Fatalf("label %q: %d stored people, recomputed %d", in.Label, len(in.People), len(byLabel[in.Label].People))
		}
		inputs = append(inputs, *in)
	}
	teams, sup := scoring.AggregateFolded(inputs, max(h.kAnonymity, scoring.MinKAnonymity))
	want := []teamScoreJSON{}
	for _, ts := range teams {
		want = append(want, newTeamScoreJSON(ts))
	}
	var got scoresResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(want) < 2 || sup.Any() {
		t.Fatalf("control: the fixture must publish a named team and the residual; refold gave %d rows, suppression %+v", len(want), sup)
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got.Teams)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("sealed body teams differ from the refold of the stored inputs:\nbody   %s\nrefold %s", gotJSON, wantJSON)
	}
}

// TestSeal_LateRowAfterSealIgnored: a sealed period is never recomputed.
func TestSeal_LateRowAfterSealIgnored(t *testing.T) {
	h, db, s := newSealFixture(t)
	ctx := context.Background()
	sealed, sealedAt, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	seedLateMayRow(t, db)
	if now, _ := foldedMayBody(t, h); bytes.Equal(now, sealed) {
		t.Fatal("control: the late row does not change May's body, so this proves nothing")
	}
	got, gotAt, err := newTestSealer(h).sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sealed) || !gotAt.Equal(sealedAt) {
		t.Errorf("after a late row: %s at %s, want the sealed %s at %s", got, gotAt, sealed, sealedAt)
	}
}

// moveMidMay moves dev from its baseline team to t1 on 15 May over a raw
// connection (no API writes a past valid_from), so May holds a membership
// boundary and groupWindow re-reads cost per sub-window.
func moveMidMay(t *testing.T, db *store.DB, dev string) {
	t.Helper()
	move := sealFixtureMonth.AddDate(0, 0, 14)
	raw := rawSealStore(t, db)
	if _, err := raw.Exec(`UPDATE hierarchy_membership SET valid_to = ? WHERE developer = ?`, move, dev); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by)
		VALUES (?, 't1', 'div-t1', ?, 'test:fixture')`, dev, move); err != nil {
		t.Fatal(err)
	}
}

// TestSeal_BodyExcludesWriteDuringComputation: a write committing while the
// period is computed never reaches the sealed body; every window read is one
// snapshot. The write is a late May cost row, which does not move the erase
// epoch, so SealReport cannot refuse it. The "mid-May move" arm puts the row
// in a membership sub-window, so groupWindow's per-cut cost reads are covered.
func TestSeal_BodyExcludesWriteDuringComputation(t *testing.T) {
	for name, move := range map[string]bool{"no boundary": false, "mid-May move": true} {
		t.Run(name, func(t *testing.T) {
			h, db, s := newSealFixture(t)
			if move {
				moveMidMay(t, db, "b6")
				members, err := db.HierarchyMembership(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				start, end := sealMay.Bounds()
				if len(newMembershipTimeline(members).cutsIn(start, end)) == 0 {
					t.Fatal("control: May holds no membership boundary, so groupWindow reads no sub-window")
				}
			}
			before, _ := foldedMayBody(t, h)
			// sealable reads the clock inside the snapshot, before any window read.
			var writes int
			s.now = func() time.Time {
				if writes++; writes == 1 {
					seedLateMayRow(t, db)
				}
				return sealNow
			}
			got, _, err := s.sealOrLoad(context.Background(), sealMay)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := foldedMayBody(t, h)
			if writes != 1 || bytes.Equal(after, before) {
				t.Fatalf("control: the clock ran %d times (want 1) and the late row changed May's body = %v (want true)", writes, !bytes.Equal(after, before))
			}
			if !bytes.Equal(got, before) {
				t.Errorf("sealed body holds a write that committed while it was computed:\nsealed %s\nwant   %s", got, before)
			}
		})
	}
}

// TestSeal_EraseInsideSnapshotIsRefused: an erase committing while the period is
// computed, after the snapshot's erase-epoch read, refuses that attempt; the
// retry seals the post-erase body and stores no key for the erased person. It
// fails if the epoch is read anywhere but inside the window's snapshot.
func TestSeal_EraseInsideSnapshotIsRefused(t *testing.T) {
	h, db, s := newSealFixture(t)
	ctx := context.Background()
	before, _ := foldedMayBody(t, h)
	calls := 0
	s.now = func() time.Time {
		if calls++; calls == 1 {
			if counts, err := db.EraseDeveloper(ctx, "b2"); err != nil || counts["token_events"] == 0 {
				t.Fatalf("erase inside the snapshot: %v %v, want b2's token events deleted", counts, err)
			}
		}
		return sealNow
	}
	got, _, err := s.sealOrLoad(ctx, sealMay)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := foldedMayBody(t, h)
	if bytes.Equal(after, before) {
		t.Fatal("control: erasing b2 does not change May's body, so this proves nothing")
	}
	if calls != 2 {
		t.Errorf("%d computations, want 2: the attempt that raced the erase must be refused and retried", calls)
	}
	if !bytes.Equal(got, after) {
		t.Errorf("sealed body is not the post-erase fold:\nsealed %s\nwant   %s", got, after)
	}
	counts, err := db.EraseDeveloper(ctx, "b2")
	if err != nil {
		t.Fatal(err)
	}
	if n := counts["sealed_person"]; n != 0 {
		t.Errorf("a second erase of b2 found %d sealed keys, want 0", n)
	}
	if counts, err = db.EraseDeveloper(ctx, "b3"); err != nil {
		t.Fatal(err)
	} else if counts["sealed_person"] == 0 {
		t.Error("control: erasing b3 found no sealed key, so live keys are not being sealed at all")
	}
}

// tearingReader runs write once, just before the first DeveloperIssueCostsWindow
// read, which loadWindow makes after DeveloperCostsWindow: the write lands
// between two of the window's reads.
type tearingReader struct {
	windowReader
	write func()
}

func (r *tearingReader) DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) ([]store.DevIssueCost, error) {
	if w := r.write; w != nil {
		r.write = nil
		w()
	}
	return r.windowReader.DeveloperIssueCostsWindow(ctx, since, until, scope)
}

// TestSeal_WindowReadsShareOneSnapshot: a write landing between two of a
// window's reads tears the folded body when each read is pooled, and never when
// the window reads one store.Snapshot, which a seal computes in.
func TestSeal_WindowReadsShareOneSnapshot(t *testing.T) {
	ctx := context.Background()
	start, end := sealMay.Bounds()
	fold := func(t *testing.T, h *Handler, r windowReader) []byte {
		t.Helper()
		resp, _, err := h.scoresForWindow(ctx, r, scoresQuery{since: start, until: end, scope: store.FleetWide}, true)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	h, db, _ := newSealFixture(t)
	before := fold(t, h, h.store)
	torn := fold(t, h, &tearingReader{h.store, func() { seedLateMayRow(t, db) }})
	after := fold(t, h, h.store)
	if bytes.Equal(torn, before) || bytes.Equal(torn, after) {
		t.Fatalf("control: pooled reads with a write between them did not tear the body (equals before: %v, after: %v)", bytes.Equal(torn, before), bytes.Equal(torn, after))
	}

	h, db, _ = newSealFixture(t)
	before = fold(t, h, h.store)
	var got []byte
	if err := db.ReadSnapshot(ctx, func(snap *store.Snapshot) error {
		got = fold(t, h, &tearingReader{snap, func() { seedLateMayRow(t, db) }})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := fold(t, h, h.store); bytes.Equal(after, before) {
		t.Fatal("control: the late row does not change May's body, so this proves nothing")
	}
	if !bytes.Equal(got, before) {
		t.Errorf("a snapshot's window holds a write that committed between its reads:\ngot  %s\nwant %s", got, before)
	}
}

// TestSeal_ConfigChangeRefusedOverlap pins #913-D1 ruling A: the config digest
// separates level, period size and the effective k; a sealed period is served
// under the config that sealed it; a span overlapping a period sealed under
// another config is refused.
func TestSeal_ConfigChangeRefusedOverlap(t *testing.T) {
	ctx := context.Background()
	base := sealConfigDigest(sealFoldRule, scoring.AggregationTeam, periodMonth, 5)
	for name, d := range map[string]string{
		"fold rule":   sealConfigDigest(sealFoldRule+1, scoring.AggregationTeam, periodMonth, 5),
		"k":           sealConfigDigest(sealFoldRule, scoring.AggregationTeam, periodMonth, 6),
		"level":       sealConfigDigest(sealFoldRule, scoring.AggregationDivision, periodMonth, 5),
		"period size": sealConfigDigest(sealFoldRule, scoring.AggregationTeam, periodKind(99), 5),
	} {
		if d == base {
			t.Errorf("a %s change leaves config_digest unchanged: %s", name, d)
		}
	}

	t.Run("sealed config pinned", func(t *testing.T) {
		h, db, s := newSealFixture(t)
		h.SetAggregation(scoring.AggregationTeam, scoring.MinKAnonymity-1) // clamped to MinKAnonymity
		sealed, _, err := s.sealOrLoad(ctx, sealMay)
		if err != nil {
			t.Fatal(err)
		}
		var k int
		var digest string
		if err := rawSealStore(t, db).QueryRow(`SELECT k, config_digest FROM sealed_report`).Scan(&k, &digest); err != nil {
			t.Fatal(err)
		}
		if k != scoring.MinKAnonymity || digest != sealConfigDigest(sealFoldRule, scoring.AggregationTeam, periodMonth, scoring.MinKAnonymity) {
			t.Errorf("stored k %d digest %s, want the effective k %d and its digest", k, digest, scoring.MinKAnonymity)
		}
		h.SetAggregation(scoring.AggregationTeam, 6)
		got, _, err := s.sealOrLoad(ctx, sealMay)
		if err != nil || !bytes.Equal(got, sealed) {
			t.Errorf("after a k change: %s (%v), want the body sealed under the clamped k=%d", got, err, scoring.MinKAnonymity)
		}
	})

	t.Run("overlap under another config refused", func(t *testing.T) {
		_, db, s := newSealFixture(t)
		q := sealFixtureMonth.AddDate(0, -1, 0)
		if _, _, err := db.SealReport(ctx, store.SealedReport{
			Level: "team", PeriodSize: "quarter", PeriodStart: q, PeriodEnd: q.AddDate(0, 3, 0), K: 5,
			ConfigDigest: "sha256:other", Body: []byte("{}"), BodyDigest: "sha256:x", ToolVersion: "t", ToolCommit: "c",
		}, nil, nil, store.SealCheck{Floor: q}); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.sealOrLoad(ctx, sealMay)
		if !errors.Is(err, store.ErrSealedPeriodOverlap) {
			t.Fatalf("sealOrLoad over a sealed quarter: %v, want ErrSealedPeriodOverlap", err)
		}
		if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_report WHERE period_size = 'month'`); n != 0 {
			t.Errorf("%d month rows sealed over the quarter, want 0", n)
		}
	})
}

// TestSeal_EraseDuringSealLeavesNoLiveKey: an erase that commits after the
// sealer's window reads and before SealReport discards that computation, so the
// period is sealed without the erased person, even when a row for them is
// re-ingested after the erase; a person not erased keeps a live, erasable key.
func TestSeal_EraseDuringSealLeavesNoLiveKey(t *testing.T) {
	for name, reingest := range map[string]bool{"erase": false, "erase then re-ingest": true} {
		t.Run(name, func(t *testing.T) {
			_, db, s := newSealFixture(t)
			ctx := context.Background()
			s.beforeSeal = func() {
				s.beforeSeal = nil
				counts, err := db.EraseDeveloper(ctx, "b3")
				if err != nil || counts["token_events"] == 0 {
					t.Fatalf("erase during seal: %v %v, want b3's token events deleted", counts, err)
				}
				if reingest {
					seedRepoCostAt(t, db, repoAlpha, "b3", "i-june", 1, sealFixtureMonth.AddDate(0, 1, 20))
				}
			}
			if _, _, err := s.sealOrLoad(ctx, sealMay); err != nil {
				t.Fatal(err)
			}
			raw := rawSealStore(t, db)
			if n := rawCount(t, raw, `SELECT COUNT(*) FROM sealed_person WHERE tombstoned = 1`); n != 0 {
				t.Errorf("%d tombstoned rows, want 0: the seal must be recomputed without b3", n)
			}
			counts, err := db.EraseDeveloper(ctx, "b3")
			if err != nil {
				t.Fatal(err)
			}
			if n := counts["sealed_person"]; n != 0 {
				t.Errorf("a second erase of b3 found %d live sealed keys, want 0", n)
			}
			counts, err = db.EraseDeveloper(ctx, "b2")
			if err != nil {
				t.Fatal(err)
			}
			if counts["sealed_person"] == 0 {
				t.Error("control: erasing b2 found no sealed key, so live keys are not being sealed at all")
			}
		})
	}
}

// TestSeal_AliasEditDuringSealLeavesNoLiveKey: an alias edit that commits after
// the sealer's window reads and before SealReport discards that computation, so
// erasing the person the alias now resolves to tombstones every sealed key of
// theirs; the retired canonical holds none.
func TestSeal_AliasEditDuringSealLeavesNoLiveKey(t *testing.T) {
	for name, c := range map[string]struct {
		edit  func(context.Context, *store.DB) error
		erase string
	}{
		"re-point": {func(ctx context.Context, db *store.DB) error {
			return db.UpsertDeveloperAlias(ctx, "b3", "b3-new", "test:fixture")
		}, "b3-new"},
		"delete": {func(ctx context.Context, db *store.DB) error {
			_, err := db.DeleteDeveloperAlias(ctx, "b3", "test:fixture")
			return err
		}, "b3"},
	} {
		t.Run(name, func(t *testing.T) {
			_, db, s := newSealFixture(t)
			ctx := context.Background()
			if err := db.UpsertDeveloperAlias(ctx, "b3", "b3-old", "test:fixture"); err != nil {
				t.Fatal(err)
			}
			s.beforeSeal = func() {
				s.beforeSeal = nil
				if err := c.edit(ctx, db); err != nil {
					t.Fatalf("alias edit during seal: %v", err)
				}
			}
			if _, _, err := s.sealOrLoad(ctx, sealMay); err != nil {
				t.Fatal(err)
			}
			counts, err := db.EraseDeveloper(ctx, c.erase)
			if err != nil {
				t.Fatal(err)
			}
			if counts["sealed_person"] == 0 {
				t.Errorf("erasing %s tombstoned no sealed key: the seal kept keys for the retired canonical", c.erase)
			}
			if counts, err = db.EraseDeveloper(ctx, "b3-old"); err != nil {
				t.Fatal(err)
			} else if n := counts["sealed_person"]; n != 0 {
				t.Errorf("the retired canonical b3-old still holds %d live sealed keys, want 0", n)
			}
		})
	}
}

// TestSeal_EraseEveryAttemptGivesUp: an erase committing during every attempt
// ends in store.ErrSealEraseRaced after sealAttempts computations, sealing nothing.
func TestSeal_EraseEveryAttemptGivesUp(t *testing.T) {
	_, db, s := newSealFixture(t)
	ctx := context.Background()
	attempts := 0
	s.beforeSeal = func() {
		attempts++
		if _, err := db.EraseDeveloper(ctx, "nobody"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.sealOrLoad(ctx, sealMay); !errors.Is(err, store.ErrSealEraseRaced) {
		t.Fatalf("sealOrLoad: %v, want store.ErrSealEraseRaced", err)
	}
	if attempts != sealAttempts {
		t.Errorf("%d computations, want sealAttempts = %d", attempts, sealAttempts)
	}
	if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_report`); n != 0 {
		t.Errorf("%d rows sealed, want 0", n)
	}
}

// TestSeal_DeveloperModeNeverSeals: developer mode refuses before any read or
// write; the same store seals in team mode.
func TestSeal_DeveloperModeNeverSeals(t *testing.T) {
	h, db, s := newSealFixture(t)
	ctx := context.Background()
	h.SetAggregation(scoring.AggregationDeveloper, 5)
	if _, _, err := s.sealOrLoad(ctx, sealMay); !errors.Is(err, errSealDeveloperMode) {
		t.Fatalf("developer mode: %v, want errSealDeveloperMode", err)
	}
	raw := rawSealStore(t, db)
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM sealed_report`); n != 0 {
		t.Fatalf("developer mode sealed %d rows", n)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	if _, _, err := s.sealOrLoad(ctx, sealMay); err != nil {
		t.Fatal(err)
	}
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM sealed_report`); n != 1 {
		t.Errorf("control: team mode sealed %d rows, want 1", n)
	}
}

// TestSeal_UnsealablePeriodRefused: an open or in-grace month, a month before
// the earliest full month of cost coverage, May inside the retention horizon, and
// May on a store with outcomes but no cost at all are refused and not sealed;
// the same May seals on the fixture as it is.
func TestSeal_UnsealablePeriodRefused(t *testing.T) {
	ctx := context.Background()
	_, db, s := newSealFixture(t)
	for _, p := range []Period{sealMay.prev(), {Kind: periodMonth, Start: sealFixtureMonth.AddDate(0, 1, 0)}} {
		if _, _, err := s.sealOrLoad(ctx, p); !errors.Is(err, errPeriodNotSealable) {
			t.Errorf("%s: %v, want errPeriodNotSealable", p, err)
		}
	}
	if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_report`); n != 0 {
		t.Errorf("%d rows sealed, want 0", n)
	}

	retained, db, s := newSealFixture(t)
	retained.SetRetentionHorizon(sealFixtureMonth.AddDate(0, 0, 1))
	outcomesOnly, bare := newTestHandler(t)
	outcomesOnly.SetAggregation(scoring.AggregationTeam, 5)
	for i := 1; i <= 6; i++ {
		seedRepoOutcomeAt(t, bare, repoAlpha, fmt.Sprintf("o%d", i), "i-o", 1, sealFixtureMonth.AddDate(0, 0, 9))
	}
	for name, c := range map[string]struct {
		s  *sealer
		db *store.DB
	}{"retention horizon after May's start": {s, db}, "outcomes but no cost": {newTestSealer(outcomesOnly), bare}} {
		if _, _, err := c.s.sealOrLoad(ctx, sealMay); !errors.Is(err, errPeriodNotSealable) {
			t.Errorf("%s: %v, want errPeriodNotSealable", name, err)
		}
		if n := rawCount(t, rawSealStore(t, c.db), `SELECT COUNT(*) FROM sealed_report`); n != 0 {
			t.Errorf("%s: %d rows sealed, want 0", name, n)
		}
	}
	retained.SetRetentionHorizon(time.Time{})
	if _, _, err := s.sealOrLoad(ctx, sealMay); err != nil {
		t.Errorf("control: May with no retention horizon: %v", err)
	}
}

// TestSeal_SetsIdentitySignals: a team or division serve makes no live read, so
// a seal pass is what sets tier_identity_unjoined and WARNs each unjoined
// identity (#125); a verifier's recompute of the same month does neither.
func TestSeal_SetsIdentitySignals(t *testing.T) {
	h, db, s := newSealFixture(t)
	at := sealFixtureMonth.AddDate(0, 0, 9)
	seedRepoCostAt(t, db, repoAlpha, "lonely", "i-lonely", 1, at)
	seedRepoOutcomeAt(t, db, repoAlpha, "quiet", "i-quiet", 1, at)
	reg := metrics.NewRegistry()
	h.SetIdentityGauge(reg.NewGauge("tier_identity_unjoined", "test gauge", "side"))
	rendered := func() string { var sb bytes.Buffer; reg.Render(&sb); return sb.String() }
	warned := func(side, dev string) bool { _, ok := h.identitySeen.Load(side + "\x00" + dev); return ok }
	if _, err := h.RecomputeSealedBody(context.Background(), sealMay); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered(), "tier_identity_unjoined{") || warned("cost", "lonely") || warned("outcome", "quiet") {
		t.Fatalf("a verifier's recompute moved the signals: %s", rendered())
	}
	if err := s.sealDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`tier_identity_unjoined{side="cost"} 1`, `tier_identity_unjoined{side="outcome"} 1`} {
		if !strings.Contains(rendered(), line+"\n") {
			t.Errorf("after a seal pass the gauge lacks %q:\n%s", line, rendered())
		}
	}
	if !warned("cost", "lonely") || !warned("outcome", "quiet") {
		t.Errorf("after a seal pass: warned lonely %v, quiet %v, want both", warned("cost", "lonely"), warned("outcome", "quiet"))
	}
}

// sealRoundTrip seals inputs as the sealer stores them, with refoldCheck against
// their own fold at k, and returns the fold inputs the store read back.
func sealRoundTrip(t *testing.T, inputs []scoring.LabelInput, k int) ([]store.SealedRollup, []store.SealedPersonKey, func([]store.SealedRollup, []store.SealedPersonKey) error) {
	t.Helper()
	_, db := newTestHandler(t)
	teams, sup := scoring.AggregateFolded(inputs, k)
	check, err := refoldCheck(k, teamRows(teams), sup)
	if err != nil {
		t.Fatal(err)
	}
	var rs []store.SealedRollup
	var ps []store.SealedPersonKey
	rollups, persons := sealedInputs(inputs)
	start, end := sealMay.Bounds()
	if _, _, err := db.SealReport(context.Background(), store.SealedReport{
		Level: "team", PeriodSize: "month", PeriodStart: start, PeriodEnd: end, K: k, ConfigDigest: "d",
		Body: []byte("{}"), BodyDigest: "b", ToolVersion: "t", ToolCommit: "c",
	}, rollups, persons, store.SealCheck{Floor: sealFixtureMonth, Refold: func(r []store.SealedRollup, p []store.SealedPersonKey) error {
		rs, ps = r, p
		return check(r, p)
	}}); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return rs, ps, check
}

// people is n distinct canonical ids with prefix p.
func people(p string, n int) []string {
	var ids []string
	for i := range n {
		ids = append(ids, fmt.Sprintf("%s%d", p, i))
	}
	return ids
}

// TestSeal_FoldInputLeaksStayClosedAfterRoundTrip pins #913-D3 ruling A': Has
// and Contributes survive the store, so a refold withholds exactly what the
// original fold withheld, and inputs read back without them refold differently.
func TestSeal_FoldInputLeaksStayClosedAfterRoundTrip(t *testing.T) {
	const k = 5
	paid := slices.Index(scoring.MeasureNames[:], "paid")
	big := scoring.LabelInput{Label: "big", People: people("b", 6), Sums: scoring.RollupSums{TotalCostUSD: 60, WeightedPoints: 12, SampleN: 12}, Contributes: true}
	big.Carriers[0], big.Carriers[1] = big.People, big.People
	big.Has[0], big.Has[1] = true, true
	// A credit memo cancels the label's paid spend to 0 with one paid carrier.
	credit := scoring.LabelInput{Label: "credit", People: people("c", 5), Sums: scoring.RollupSums{TotalCostUSD: 50, WeightedPoints: 5, SampleN: 5}, Contributes: true}
	credit.Carriers[0], credit.Carriers[1], credit.Carriers[paid] = credit.People, credit.People, credit.People[:1]
	credit.Has[0], credit.Has[1], credit.Has[paid] = true, true, true
	// Uncounted people (bots) carry paid spend and fill no seat.
	bots := scoring.LabelInput{Label: "bots", Sums: scoring.RollupSums{ActualPaidUSD: 100}, Contributes: true}
	bots.Has[paid] = true

	for name, c := range map[string]struct {
		inputs []scoring.LabelInput
		leak   func([]scoring.TeamScore, scoring.KAnonSuppression) bool
	}{
		"credit memo label stays folded": {[]scoring.LabelInput{big, credit}, func(teams []scoring.TeamScore, _ scoring.KAnonSuppression) bool {
			return slices.ContainsFunc(teams, func(ts scoring.TeamScore) bool { return ts.Team == "credit" })
		}},
		"uncounted paid-only residual stays withheld": {[]scoring.LabelInput{big, bots}, func(_ []scoring.TeamScore, sup scoring.KAnonSuppression) bool {
			return !sup.Residual
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if c.leak(scoring.AggregateFolded(c.inputs, k)) {
				t.Fatal("control: the original fold already publishes the hidden group")
			}
			rs, ps, check := sealRoundTrip(t, c.inputs, k)
			loaded, err := foldInputsOf(rs, ps)
			if err != nil {
				t.Fatal(err)
			}
			if c.leak(scoring.AggregateFolded(loaded, k)) {
				t.Error("the refold of the stored inputs publishes the hidden group")
			}
			for i := range rs {
				rs[i].Has, rs[i].Contributes = [len(store.SealedMeasures)]bool{}, false
			}
			stripped, err := foldInputsOf(rs, ps)
			if err != nil {
				t.Fatal(err)
			}
			if !c.leak(scoring.AggregateFolded(stripped, k)) {
				t.Error("control: inputs without Has and Contributes still withhold the group, so this proves nothing")
			}
			if err := check(rs, ps); !errors.Is(err, errSealRefoldMismatch) {
				t.Errorf("refold check over inputs without Has and Contributes: %v, want errSealRefoldMismatch", err)
			}
		})
	}
}

// TestSeal_FoldInputColumnsNamedByMeasure pins sealed_rollup's has_<name>
// columns to scoring.MeasureNames by name: each measure's Has lands in the
// column named for it and nowhere else.
func TestSeal_FoldInputColumnsNamedByMeasure(t *testing.T) {
	if store.SealedMeasures != scoring.MeasureNames {
		t.Fatalf("store.SealedMeasures = %q, want scoring.MeasureNames %q", store.SealedMeasures, scoring.MeasureNames)
	}
	var inputs []scoring.LabelInput
	for m, name := range scoring.MeasureNames {
		in := scoring.LabelInput{Label: name}
		in.Has[m] = true
		inputs = append(inputs, in)
	}
	inputs = append(inputs, scoring.LabelInput{Label: "zz-contributes", Contributes: true})
	_, db := newTestHandler(t)
	rollups, _ := sealedInputs(inputs)
	start, end := sealMay.Bounds()
	if _, _, err := db.SealReport(context.Background(), store.SealedReport{
		Level: "team", PeriodSize: "month", PeriodStart: start, PeriodEnd: end, K: 5, ConfigDigest: "d",
		Body: []byte("{}"), BodyDigest: "b", ToolVersion: "t", ToolCommit: "c",
	}, rollups, nil, store.SealCheck{Floor: sealFixtureMonth}); err != nil {
		t.Fatal(err)
	}
	raw := rawSealStore(t, db)
	want := map[string]string{"contributes": "zz-contributes"}
	for _, name := range scoring.MeasureNames {
		want["has_"+name] = name
	}
	for name, want := range want {
		var labels string
		if err := raw.QueryRow(`SELECT group_concat(label) FROM sealed_rollup WHERE ` + name + ` = 1`).Scan(&labels); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if labels != want {
			t.Errorf("column %s is set for %q, want only %q", name, labels, want)
		}
	}
}

// skewedSealStore stores every rollup with one extra cent of cost, so the stored
// inputs no longer refold to the body being sealed.
type skewedSealStore struct{ *store.DB }

func (s skewedSealStore) SealReport(ctx context.Context, r store.SealedReport, rollups []store.SealedRollup, persons []store.SealedPerson, check store.SealCheck) (store.SealedReport, bool, error) {
	for i := range rollups {
		rollups[i].TotalCostUSD += 0.01
	}
	return s.DB.SealReport(ctx, r, rollups, persons, check)
}

// TestSeal_RefoldMismatchRollsBack: the sealer refuses, and SealReport rolls
// back, a seal whose stored inputs do not refold to the body; the same store
// without the skew seals.
func TestSeal_RefoldMismatchRollsBack(t *testing.T) {
	h, db, s := newSealFixture(t)
	h.store = skewedSealStore{db}
	if _, _, err := s.sealOrLoad(context.Background(), sealMay); !errors.Is(err, errSealRefoldMismatch) {
		t.Fatalf("sealOrLoad over skewed stored inputs: %v, want errSealRefoldMismatch", err)
	}
	if n := rawCount(t, rawSealStore(t, db), `SELECT COUNT(*) FROM sealed_report`); n != 0 {
		t.Fatalf("%d rows sealed, want 0", n)
	}
	h.store = db
	if _, _, err := s.sealOrLoad(context.Background(), sealMay); err != nil {
		t.Errorf("control: the unskewed store: %v", err)
	}
}
