package muse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// TestRunPass_SettledOnlyWhenTheCursorAdvances pins the seal gate's success
// signal (#913-D9): a pass whose ingest fails reports nothing; a pass that
// advances the cursor reports Run's since through its start less settleGrace,
// the longest a quiescent run is held.
func TestRunPass_SettledOnlyWhenTheCursorAdvances(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, "s", repo, readFixture(t))
	now := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	c := newCollector(t, home, []string{repo}, func() time.Time { return now })
	var got [][2]time.Time
	c.settled = func(_ context.Context, from, through time.Time) { got = append(got, [2]time.Time{from, through}) }

	c.runPass(context.Background(), since, collector.IngesterFunc(func(context.Context, collector.TokenEvent) error {
		return errors.New("store is refusing writes")
	}))
	if len(got) != 0 {
		t.Fatalf("a pass whose ingest failed settled %v, want nothing", got)
	}
	ing := &memIngester{}
	c.runPass(context.Background(), since, ing)
	if len(ing.byKey) == 0 {
		t.Fatal("control: the fixture emitted nothing, so the failing pass above proved nothing")
	}
	if want := now.Add(-settleGrace); len(got) != 1 || !got[0][0].Equal(since) || !got[0][1].Equal(want) {
		t.Errorf("settled %v, want once from %s through %s", got, since, want)
	}
}

// TestRunPass_SettlesNoLaterThanTheOldestHeldCall (#913-D9): a run still being
// written holds its calls however old they are, and each is emitted later with
// its own timestamp, so a pass settles through that call's time, not through
// its start less settleGrace.
func TestRunPass_SettlesNoLaterThanTheOldestHeldCall(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
	c := newCollector(t, home, []string{repo}, nil)
	var got [][2]time.Time
	c.settled = func(_ context.Context, from, through time.Time) { got = append(got, [2]time.Time{from, through}) }
	ing := &memIngester{}
	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != 0 || len(c.pending) == 0 {
		t.Fatalf("control: emitted %d, pending %v; want the running run held", len(ing.byKey), c.pending)
	}
	evs, err := newCollector(t, home, []string{repo}, func() time.Time { return time.Now().Add(settleGrace + time.Minute) }).
		Collect(context.Background(), time.Time{})
	if err != nil || len(evs) == 0 {
		t.Fatalf("control: the held calls, once quiescent: %d events, %v", len(evs), err)
	}
	oldest := evs[0].Timestamp
	for _, e := range evs {
		if e.Timestamp.Before(oldest) {
			oldest = e.Timestamp
		}
	}
	if len(got) != 1 || got[0][1].After(oldest) {
		t.Errorf("settled %v, want once through no later than the oldest held call, %s", got, oldest)
	}
}

// TestRunPass_UnlistableSubdirectorySettlesNothing (#913-D9 ruling R-8): a
// directory under the root the walk cannot list may hold spend nobody read, so
// the pass settles nothing until it lists again.
func TestRunPass_UnlistableSubdirectorySettlesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, "s", repo, readFixture(t))
	hidden := filepath.Dir(writeSession(t, home, "t", repo, readFixture(t)))
	c := newCollector(t, home, []string{repo}, func() time.Time { return time.Now().Add(settleGrace + time.Minute) })
	var got int
	c.settled = func(context.Context, time.Time, time.Time) { got++ }
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	if got != 0 {
		t.Fatalf("a pass that could not list %s settled %d times, want none", hidden, got)
	}
	if err := os.Chmod(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	if got != 1 {
		t.Errorf("control: settled %d times once it lists again, want once", got)
	}
}

// TestRunPass_DeletedHeldFileIsRecordedLost (#913-D9 ruling R-8): a file
// deleted while it holds a call takes that spend with it, so the pass records
// it lost from the oldest held call through now before it settles past it.
func TestRunPass_DeletedHeldFileIsRecordedLost(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	path := writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
	c := newCollector(t, home, []string{repo}, nil)
	var lost [][2]time.Time
	c.lost = func(_ context.Context, from, through time.Time) error {
		lost = append(lost, [2]time.Time{from, through})
		return nil
	}
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	held, ok := c.pending[path]
	if !ok || len(lost) != 0 {
		t.Fatalf("control: pending %v, lost %v; want the running run held and nothing lost", c.pending, lost)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	if len(lost) != 1 || !lost[0][0].Equal(held) || lost[0][1].Before(held) || len(c.pending) != 0 {
		t.Errorf("lost %v, pending %v after the delete; want one span from %s and nothing pending", lost, c.pending, held)
	}
}
