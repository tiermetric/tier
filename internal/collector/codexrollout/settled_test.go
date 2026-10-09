package codexrollout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// settleRecorder records every settled call a collector makes.
func settleRecorder(c *Collector, since time.Time) *[][2]time.Time {
	var got [][2]time.Time
	c.from = since
	c.settled = func(_ context.Context, from, through time.Time) { got = append(got, [2]time.Time{from, through}) }
	return &got
}

var acceptAll = collector.IngesterFunc(func(context.Context, collector.TokenEvent) error { return nil })

// TestRunPass_SettledOnlyAfterACompletePass pins the seal gate's success
// signal (#913-D9): a pass whose ingest fails reports nothing; a pass that read
// and ingested everything reports Run's since through the pass's start.
func TestRunPass_SettledOnlyAfterACompletePass(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	writeRollout(t, sessions, "spaced.jsonl", syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)}))
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	got := settleRecorder(c, testSince)

	c.runPass(context.Background(), collector.IngesterFunc(func(context.Context, collector.TokenEvent) error {
		return errors.New("store is refusing writes")
	}))
	if len(*got) != 0 {
		t.Fatalf("a pass whose ingest failed settled %v, want nothing", *got)
	}
	before := time.Now().UTC()
	c.runPass(context.Background(), acceptAll)
	if len(*got) != 1 || !(*got)[0][0].Equal(testSince) || (*got)[0][1].Before(before) || (*got)[0][1].After(time.Now().UTC()) {
		t.Errorf("settled %v, want once from %s through the pass's start (after %s)", *got, testSince, before)
	}
}

// TestRunPass_UnresolvedRepoHoldsTheCursorAndSettlesNothing (#913-D9): a pass
// whose repo cannot be resolved read nothing it could attribute, so it settles
// nothing and its cursor holds; once the repo resolves again the run continues
// from Run's since, since no file was passed over.
func TestRunPass_UnresolvedRepoHoldsTheCursorAndSettlesNothing(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	writeRollout(t, sessions, "spaced.jsonl", syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)}))
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	got := settleRecorder(c, testSince)
	c.setCursor(scanWindow{fileFloor: testSince, eventFloor: testSince, gitFloor: testSince})
	gitDir, moved := filepath.Join(repo, ".git"), filepath.Join(repo, "git.moved")
	if err := os.Rename(gitDir, moved); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), acceptAll)
	if len(*got) != 0 {
		t.Errorf("a pass whose repo did not resolve settled %v, want nothing", *got)
	}
	c.mu.Lock()
	floor := c.cursor.fileFloor
	c.mu.Unlock()
	if !floor.Equal(testSince) {
		t.Errorf("cursor after the unresolved pass: %v, want it held at %s", floor, testSince)
	}
	if err := os.Rename(moved, gitDir); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), acceptAll)
	if len(*got) != 1 || !(*got)[0][0].Equal(testSince) {
		t.Errorf("settled %v after the repo resolved again, want once from %s", *got, testSince)
	}
}

// TestRunPass_FailingFileHoldsWhileItMayHeal (#913-D9 ruling R-8): a failing
// file the next pass re-reads may yet heal, so no pass settles while it is
// that fresh and none records it lost; once it heals the pass settles from
// Run's since.
func TestRunPass_FailingFileHoldsWhileItMayHeal(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	writeRollout(t, sessions, "spaced.jsonl", syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)}))
	bad := writeRollout(t, sessions, "nomodel.jsonl", synthetic(repo, testBranch, "", []tokenUsage{usage(1000, 400, 100, 40)}))
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	got := settleRecorder(c, testSince)
	var lost [][2]time.Time
	c.lost = func(_ context.Context, from, through time.Time) error {
		lost = append(lost, [2]time.Time{from, through})
		return nil
	}
	ing := &countingIngester{}
	c.runPass(context.Background(), ing)
	if ing.n == 0 {
		t.Fatal("control: the good file ingested nothing")
	}
	c.runPass(context.Background(), acceptAll)
	if len(*got) != 0 || len(lost) != 0 {
		t.Fatalf("passes with a fresh failing file settled %v and lost %v, want neither", *got, lost)
	}
	if err := os.WriteFile(bad, []byte(synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})), 0o644); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), acceptAll)
	if len(*got) != 1 || !(*got)[0][0].Equal(testSince) || len(lost) != 0 {
		t.Errorf("settled %v, lost %v once the file healed; want once from %s and nothing lost", *got, lost, testSince)
	}
}

// storeHooks wires c's settled and lost to a real store's codex row.
func storeHooks(t *testing.T, c *Collector, since time.Time) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.RegisterSources(context.Background(), []string{collector.SourceCodexRollout}); err != nil {
		t.Fatal(err)
	}
	hook(c, db, since)
	return db
}

func hook(c *Collector, db *store.DB, since time.Time) {
	c.from = since
	c.settled = func(ctx context.Context, from, through time.Time) {
		_ = db.AdvanceSourceWatermark(ctx, collector.SourceCodexRollout, from, through)
	}
	c.lost = func(ctx context.Context, from, through time.Time) error {
		return db.RecordSourceLoss(ctx, collector.SourceCodexRollout, from, through)
	}
}

// codexCovers reports whether the codex row certifies the month starting m.
func codexCovers(t *testing.T, db *store.DB, m time.Month) bool {
	t.Helper()
	rows, err := db.SourceWatermarks(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("source rows %+v, %v", rows, err)
	}
	start := time.Date(2026, m, 1, 0, 0, 0, 0, time.UTC)
	return rows[0].Covers(start, start.AddDate(0, 1, 0))
}

// TestRunPass_FailingFilePastTheCursorIsLostAndSurvivesARestart (#913-D9
// ruling R-8): a failing file the cursor moves past is recorded lost in the
// store at its session's day through its mtime, and the pass settles, so July
// stays gated while June and August are certified; a restart after the file
// is deleted re-reads from Run's since and still leaves July gated.
func TestRunPass_FailingFilePastTheCursorIsLostAndSurvivesARestart(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	writeRollout(t, sessions, "spaced.jsonl", syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)}))
	bad := writeRollout(t, sessions, "nomodel.jsonl", synthetic(repo, testBranch, "", []tokenUsage{usage(1000, 400, 100, 40)}))
	july := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(bad, july, july); err != nil {
		t.Fatal(err)
	}
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	db := storeHooks(t, c, testSince)
	c.runPass(context.Background(), acceptAll)
	if codexCovers(t, db, time.July) || !codexCovers(t, db, time.June) || !codexCovers(t, db, time.August) {
		t.Fatalf("after the pass: July %v, June %v, August %v; want July gated, the others certified",
			codexCovers(t, db, time.July), codexCovers(t, db, time.June), codexCovers(t, db, time.August))
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	restarted := newTestCollector(t, sessions, RepoTarget{Path: repo})
	hook(restarted, db, testSince)
	restarted.setCursor(scanWindow{fileFloor: testSince, eventFloor: testSince, gitFloor: testSince})
	restarted.runPass(context.Background(), acceptAll)
	if codexCovers(t, db, time.July) || !codexCovers(t, db, time.August) {
		t.Errorf("after the restart: July %v, August %v; want July still gated", codexCovers(t, db, time.July), codexCovers(t, db, time.August))
	}
}

// TestRunPass_DamagedLogIsRecordedLost (#913-D9 ruling R-8): a log in scope
// that lost a complete line to damage never gets it back, so the pass records
// the log's span lost, then settles; a foreign log's damage is not ours.
func TestRunPass_DamagedLogIsRecordedLost(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	writeRollout(t, sessions, "partial.jsonl", synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})+garbageLine)
	foreign := initGitRepo(t, t.TempDir())
	writeRollout(t, sessions, "foreign.jsonl", synthetic(foreign, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})+garbageLine)
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	got := settleRecorder(c, testSince)
	var lost [][2]time.Time
	c.lost = func(_ context.Context, from, through time.Time) error {
		lost = append(lost, [2]time.Time{from, through})
		return nil
	}
	c.runPass(context.Background(), acceptAll)
	if day := time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC); len(lost) != 1 || !lost[0][0].Equal(day) || lost[0][1].Before(day) {
		t.Errorf("lost %v, want the in-scope log alone, from %s", lost, day)
	}
	if len(*got) != 1 {
		t.Errorf("settled %v, want once, after the loss is recorded", *got)
	}
}

type countingIngester struct{ n int }

func (c *countingIngester) Ingest(context.Context, collector.TokenEvent) error { c.n++; return nil }
