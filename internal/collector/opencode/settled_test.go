package opencode

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// TestRunPass_SettledOnlyAfterAScanSucceeds pins the seal gate's success signal
// (#913-D9): a pass that scans and ingests everything reports Run's since
// through the pass's start less the watermark's lag; a failed ingest, and a
// database that is absent (the collector is enabled, Opencode is not
// installed), report nothing.
func TestRunPass_SettledOnlyAfterAScanSucceeds(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	done := int64(1_787_956_602_046)
	dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: []msgRow{buildRow(t, msgSpec{
		ID: "msg_a", SessionID: "ses_a", Cwd: filepath.Join(repo, "src"), Created: done - 1000, Completed: &done,
		TimeUpdated: done, Tokens: autoTotal(100, 10, 5, 50, 0),
	})}})
	now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	ok := collector.IngesterFunc(func(context.Context, collector.TokenEvent) error { return nil })
	failing := collector.IngesterFunc(func(context.Context, collector.TokenEvent) error { return errors.New("refused") })
	for _, c := range []struct {
		name, db string
		ing      collector.Ingester
		want     bool
	}{
		{"success", dbPath, ok, true},
		{"ingest failed", dbPath, failing, false},
		{"no database", filepath.Join(t.TempDir(), "absent.db"), ok, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, _ := newTestLogger()
			col, err := New(Config{DBPath: c.db, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger,
				Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			var got [][2]time.Time
			col.settled = func(_ context.Context, from, through time.Time) { got = append(got, [2]time.Time{from, through}) }
			col.runPass(context.Background(), since, c.ing)
			through := now.Add(-time.Duration(watermarkLagFactor) * col.interval)
			switch {
			case c.want && (len(got) != 1 || !got[0][0].Equal(since) || !got[0][1].Equal(through)):
				t.Errorf("settled %v, want once from %s through %s", got, since, through)
			case !c.want && len(got) != 0:
				t.Errorf("settled %v, want nothing", got)
			}
		})
	}
}

// TestRunPass_SkippedRowsAreRecordedLostFirst (#913-D9 ruling R-8): a pass
// that steps over an in-scope row whose spend it cannot record (JSON that does
// not decode, token counts that break the additive identity) records each
// lost, at its completion or else its update time, before the watermark moves
// past it; a foreign row is not ours to lose; a pass that cannot record the
// loss neither moves the watermark nor settles.
func TestRunPass_SkippedRowsAreRecordedLostFirst(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	done := int64(1_787_956_602_046)
	broken := tokenSpec{Total: i64(1), Input: 100, Output: 10}
	dbPath := newFixtureDB(t, dbSpec{Migrations: 3, Rows: []msgRow{
		buildRow(t, msgSpec{ID: "msg_a", SessionID: "s", Cwd: repo, Created: done - 9000, Completed: &done, TimeUpdated: done, Tokens: autoTotal(100, 10, 5, 50, 0)}),
		buildRow(t, msgSpec{ID: "msg_b", SessionID: "s", TimeUpdated: done + 1000, RawOverride: "{not json"}),
		buildRow(t, msgSpec{ID: "msg_c", SessionID: "s", Cwd: repo, Created: done - 9000, Completed: &done, TimeUpdated: done + 2000, Tokens: broken}),
		buildRow(t, msgSpec{ID: "msg_d", SessionID: "s", Cwd: t.TempDir(), Created: done - 9000, Completed: &done, TimeUpdated: done + 3000, Tokens: broken}),
	}})
	now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, fail := range []bool{true, false} {
		logger, _ := newTestLogger()
		col, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger,
			Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		var settled int
		var lost []time.Time
		col.settled = func(context.Context, time.Time, time.Time) { settled++ }
		col.lost = func(_ context.Context, from, _ time.Time) error {
			if fail {
				return errors.New("store refused")
			}
			lost = append(lost, from)
			return nil
		}
		col.runPass(context.Background(), since, collector.IngesterFunc(func(context.Context, collector.TokenEvent) error { return nil }))
		want := []time.Time{time.UnixMilli(done + 1000).UTC(), time.UnixMilli(done).UTC()}
		switch {
		case fail && (settled != 0 || col.floor() != 0):
			t.Errorf("a pass that could not record the loss settled %d times, watermark floor %d; want neither", settled, col.floor())
		case !fail && (settled != 1 || len(lost) != 2 || !lost[0].Equal(want[0]) || !lost[1].Equal(want[1])):
			t.Errorf("settled %d, lost %v; want the undecodable and the in-scope broken row lost (%v), then one settle", settled, lost, want)
		}
	}
}
