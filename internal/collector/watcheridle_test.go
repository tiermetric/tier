package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// idleN is idleRelease written as a literal, so a changed constant fails here.
const idleN = 600 * time.Second

// idleRead appends lines, sets the transcript's mtime, and runs one read by w
// with its clock at now.
func (h *heldRig) idleRead(t *testing.T, w *Watcher, mtime, now time.Time, lines ...string) []string {
	t.Helper()
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := io.WriteString(f, l+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(h.path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return now }
	return h.read(t, w)
}

func wantHeld(t *testing.T, h *heldRig, offset int64, held bool) {
	t.Helper()
	if h.state.offset != offset || h.held != held {
		t.Fatalf("offset %d held %v, want offset %d held %v", h.state.offset, h.held, offset, held)
	}
}

// Pins: a session's final message is held while the file has had an append
// within the last 600 s, and released by the first read at 600 s, which
// reports nothing left to wake for.
func TestWatcher_IdleReleasesASessionsFinalMessage(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	mtime := f.wtAt(10)
	l0 := wtJSONLLine("msg_0", f.wtAt(1), f.repo, "main", wtText, 100)
	l1 := wtJSONLLine("msg_1", f.wtAt(2), f.repo, "main", wtText, 101)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN-time.Second), l0, l1), "100 unattributed:main branch")
	wantHeld(t, h, int64(len(l0)+1), true)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN)), "101 unattributed:main branch")
	wantHeld(t, h, int64(len(l0)+len(l1)+2), false)
}

// Pins: a subagent file, which has no turn-end line, releases its final
// message (its report) the same way.
func TestWatcher_IdleReleasesASubagentsFinalMessage(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	h.path = filepath.Join(filepath.Dir(h.path), "sess-823", "subagents", "agent-a1.jsonl")
	mkdirs(t, filepath.Dir(h.path))
	w := &Watcher{worktrees: f.x}
	agent := func(id string, n, out int) string {
		l := wtJSONLLine(id, f.wtAt(n), f.repo, "main", wtText, out)
		return strings.Replace(l, `"isSidechain":false`, `"isSidechain":true,"agentId":"a1"`, 1)
	}
	mtime := f.wtAt(10)
	l0, l1 := agent("msg_s0", 1, 300), agent("msg_s1", 2, 301)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN-time.Second), l0, l1), "300 unattributed:main branch")
	wantHeld(t, h, int64(len(l0)+1), true)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN)), "301 unattributed:main branch")
	wantHeld(t, h, int64(len(l0)+len(l1)+2), false)
}

// Pins: a message whose second line comes 599 s after its first is emitted
// once, from both lines. msg_m's first line alone names worktree A (issue-11
// worktree-toolpath); with its second, naming the main checkout, the whole
// message resolves to the main branch.
func TestWatcher_IdleReleaseNeverSplitsAMessageAtAShorterPause(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	t1 := f.wtAt(10)
	t2 := t1.Add(599 * time.Second)
	wantEvents(t, h.idleRead(t, w, t1, t2,
		wtJSONLLine("msg_0", f.wtAt(1), f.repo, "main", wtText, 200),
		wtJSONLLine("msg_m", f.wtAt(2), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 201),
	), "200 unattributed:main branch")
	wantEvents(t, h.idleRead(t, w, t2, t2,
		wtJSONLLine("msg_m", f.wtAt(3), f.repo, "main", wtRead(filepath.Join(f.repo, "m.go")), 201),
	))
	wantEvents(t, h.idleRead(t, w, t2, t2.Add(599*time.Second)))
	wantEvents(t, h.idleRead(t, w, t2, t2.Add(600*time.Second)), "201 unattributed:main branch")
}

// Pins: a released message is not emitted again by the reads of later appends.
func TestWatcher_IdleReleaseThenAppendEmitsEachMessageOnce(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	line := func(id string, n, out int) string { return wtJSONLLine(id, f.wtAt(n), f.repo, "main", wtText, out) }
	t1 := f.wtAt(10)
	var got []string
	got = append(got, h.idleRead(t, w, t1, t1.Add(time.Second), line("msg_0", 1, 400), line("msg_1", 2, 401))...)
	got = append(got, h.idleRead(t, w, t1, t1.Add(idleN))...)
	t2 := t1.Add(2 * idleN)
	got = append(got, h.idleRead(t, w, t2, t2.Add(time.Second), line("msg_2", 3, 402), line("msg_3", 4, 403))...)
	got = append(got, h.idleRead(t, w, t2, t2.Add(idleN))...)
	got = append(got, h.idleRead(t, w, t2, t2.Add(2*idleN))...)
	wantEvents(t, got,
		"400 unattributed:main branch", "401 unattributed:main branch",
		"402 unattributed:main branch", "403 unattributed:main branch")
}

// Pins: with attribution off, an idle file is read as before: the open message
// is emitted, nothing is held, and the checkpoint is the one a fresh file gets.
func TestWatcher_AttributionOffIgnoresIdleness(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHeldRig(t, base, repo)
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	const text = `[{"type":"text","text":"ok"}]`
	l0 := wtJSONLLine("msg_a", at, repo, "feature/7-x", text, 400)
	l1 := wtJSONLLine("msg_b", at.Add(time.Second), repo, "feature/7-x", text, 401)
	wantEvents(t, h.idleRead(t, &Watcher{}, at, at.Add(10*idleN), l0, l1), "400 issue-7 ", "401 issue-7 ")
	wantHeld(t, h, int64(len(l0)+len(l1)+2), false)
	wantCheckpoint(t, h, int64(len(l0)+len(l1)+2),
		`{"SessionID":"sess-823","GitBranch":"feature/7-x","CWD":"`+repo+`","StartTime":"2026-09-29T10:00:00Z",`+
			`"Model":"claude-sonnet-4","LastRealBranch":"feature/7-x","NextParseSeq":2}`)
}

// Pins: a file holding back a message gets one more read once it has gone
// idle, with no further write to it, and that read is a single wake at the
// idle delay, not a read every debounce until then (at most 3 reads: one or two
// for the writes, one wake).
func TestWatcher_IdleWakeReadsAQuietFile(t *testing.T) {
	r := newEraseRig(t)
	r.worktrees = newWorktreeIndex(nil, nil)
	r.idle = 500 * time.Millisecond
	cps := &hookedCheckpoints{DB: r.db}
	r.run(t, cps)

	path := filepath.Join(r.projects, "quiet.jsonl")
	writeJSONLLine(t, path, "sess-quiet", r.repo, "feature/42-foo", 111)
	appendTaggedLine(t, path, "sess-quiet", r.repo, "b222", 222)
	waitUntil(t, "the held message released after the file went idle", func() bool {
		return len(r.sessionInputs(t, "sess-quiet")) == 2
	})
	if n := cps.loadCount(path); n > 3 {
		t.Errorf("the quiet file was read %d times before its release, want at most 3 (one wake at the idle delay)", n)
	}
}

// waitCheckpoint waits until path's checkpoint row is saved at offset.
func (r eraseRig) waitCheckpoint(t *testing.T, path string, offset int64) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("%s checkpointed at offset %d", filepath.Base(path), offset), func() bool {
		cp, ok := r.checkpoint(t, path)
		return ok && !cp.Tombstoned() && cp.Offset == offset
	})
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// Pins: a message held back when the watcher stopped is released by the next
// start, with no write to its file, once the file is idle; a file written
// within the last 600 s stays held at startup.
func TestWatcher_RestartReleasesAStrandedHeldMessage(t *testing.T) {
	r := newEraseRig(t)
	r.worktrees = newWorktreeIndex(nil, nil)
	stop := r.run(t, r.db)

	stranded := filepath.Join(r.projects, "stranded.jsonl")
	fresh := filepath.Join(r.projects, "fresh.jsonl")
	firstLen := map[string]int64{}
	for _, p := range []string{stranded, fresh} {
		sess := "sess-" + strings.TrimSuffix(filepath.Base(p), ".jsonl")
		writeJSONLLine(t, p, sess, r.repo, "feature/42-foo", 111)
		firstLen[p] = fileSize(t, p)
		appendTaggedLine(t, p, sess, r.repo, "b222", 222)
	}
	waitUntil(t, "both files' first messages stored, the second held back", func() bool {
		return len(r.sessionInputs(t, "sess-stranded")) == 1 && len(r.sessionInputs(t, "sess-fresh")) == 1
	})
	for _, p := range []string{stranded, fresh} {
		r.waitCheckpoint(t, p, firstLen[p])
	}
	stop()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stranded, old, old); err != nil {
		t.Fatal(err)
	}

	r.run(t, r.db)
	waitUntil(t, "the stranded message released at startup", func() bool {
		return len(r.sessionInputs(t, "sess-stranded")) == 2
	})
	time.Sleep(4 * eraseTestDebounce)
	if got := r.sessionInputs(t, "sess-fresh"); len(got) != 1 {
		t.Errorf("fresh file rows = %v, want only 111 (222 is held until the file is idle)", got)
	}
}

// Pins: with attribution off, a start does not read a checkpointed file with
// unread bytes: it waits for a write, as before.
func TestWatcher_AttributionOffStartupReadsNothingUnwritten(t *testing.T) {
	r := newEraseRig(t)
	stop := r.run(t, r.db)
	path := filepath.Join(r.projects, "down.jsonl")
	writeJSONLLine(t, path, "sess-down", r.repo, "feature/42-foo", 111)
	waitUntil(t, "first message stored", func() bool { return len(r.sessionInputs(t, "sess-down")) == 1 })
	r.waitCheckpoint(t, path, fileSize(t, path))
	stop()
	appendTaggedLine(t, path, "sess-down", r.repo, "b222", 222)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	r.run(t, r.db)
	time.Sleep(4 * eraseTestDebounce)
	if got := r.sessionInputs(t, "sess-down"); len(got) != 1 {
		t.Fatalf("rows after restart = %v, want only 111 (nothing wrote to the file)", got)
	}
	appendTaggedLine(t, path, "sess-down", r.repo, "c333", 333)
	waitUntil(t, "both appends stored after a write", func() bool { return len(r.sessionInputs(t, "sess-down")) == 3 })
}

// Pins: a one-message file, whose first read holds everything back, is
// checkpointed at offset 0 by that read, so a start after the watcher stopped
// releases the message with no write to the file.
func TestWatcher_RestartReleasesAOneMessageFile(t *testing.T) {
	r := newEraseRig(t)
	r.worktrees = newWorktreeIndex(nil, nil)
	stop := r.run(t, r.db)

	path := filepath.Join(r.projects, "one.jsonl")
	writeJSONLLine(t, path, "sess-one", r.repo, "feature/42-foo", 111)
	r.waitCheckpoint(t, path, 0)
	if got := r.sessionInputs(t, "sess-one"); len(got) != 0 {
		t.Fatalf("rows before the file went idle = %v, want none (111 is held)", got)
	}
	stop()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	r.run(t, r.db)
	waitUntil(t, "the one-message file released at startup", func() bool {
		return len(r.sessionInputs(t, "sess-one")) == 1
	})
}

// hookIngester runs hook once, before forwarding the first event whose
// input_tok is tok.
type hookIngester struct {
	next Ingester
	tok  int
	hook func()
	once sync.Once
}

func (h *hookIngester) Ingest(ctx context.Context, ev TokenEvent) error {
	if ev.InputTok == h.tok {
		h.once.Do(h.hook)
	}
	return h.next.Ingest(ctx, ev)
}

// Pins: a write during a held read arms a debounce that the read's idle wake
// (1 h here) does not postpone: 222, finished by that write, is stored within
// the waitUntil bound.
func TestWatcher_IdleWakeNeverPostponesAPendingDebounce(t *testing.T) {
	r := newEraseRig(t)
	r.worktrees = newWorktreeIndex(nil, nil)
	r.idle = time.Hour
	path := filepath.Join(r.projects, "busy.jsonl")
	line := fmt.Sprintf(`{"type":"assistant","timestamp":"2026-05-19T10:00:03Z","sessionId":"sess-busy","gitBranch":"feature/42-foo","cwd":%q,`+
		`"message":{"id":"msg_busy_c333","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":333,"output_tokens":0}}}`+"\n", r.repo)
	ing := &hookIngester{next: storeIngester{r.db}, tok: 111, hook: func() {
		// The read storing 111 holds 222; this write finishes 222.
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			_, err = io.WriteString(f, line)
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			t.Errorf("append 333: %v", err)
		}
		time.Sleep(4 * eraseTestDebounce)
	}}
	r.runWith(t, r.db, ing)

	writeJSONLLine(t, path, "sess-busy", r.repo, "feature/42-foo", 111)
	appendTaggedLine(t, path, "sess-busy", r.repo, "b222", 222)
	waitUntil(t, "222 stored by the debounce of the write made during the held read", func() bool {
		return len(r.sessionInputs(t, "sess-busy")) == 2
	})
}

// failOnceIngester fails the first event whose input_tok is tok with err, and
// forwards every other.
type failOnceIngester struct {
	next   Ingester
	tok    int
	err    error
	failed atomic.Bool
}

func (f *failOnceIngester) Ingest(ctx context.Context, ev TokenEvent) error {
	if ev.InputTok == f.tok && f.failed.CompareAndSwap(false, true) {
		return f.err
	}
	return f.next.Ingest(ctx, ev)
}

// Pins: a read releasing a finished file's message that fails (an insert
// error, or a cancelled insert, the !ok path) is retried by the idle wake with
// no further write to the file.
func TestWatcher_IdleReleaseRetriesAFailedRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"insert error", errors.New("injected insert failure")},
		{"cancelled insert", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEraseRig(t)
			r.worktrees = newWorktreeIndex(nil, nil)
			r.idle = 300 * time.Millisecond
			ing := &failOnceIngester{next: storeIngester{r.db}, tok: 111, err: tc.err}
			r.runWith(t, r.db, ing)

			path := filepath.Join(r.projects, "done.jsonl")
			writeJSONLLine(t, path, "sess-done", r.repo, "feature/42-foo", 111)
			waitUntil(t, "111 stored by a retry after its release failed", func() bool {
				return ing.failed.Load() && len(r.sessionInputs(t, "sess-done")) == 1
			})
		})
	}
}

// Pins: an idle read of a file ending in an unterminated line releases the
// messages before it, stops at that line's start, and reports nothing held, so
// a torn last line does not re-arm a wake.
func TestWatcher_IdleReadEndingInAPartialLineIsNotHeld(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	l0 := wtJSONLLine("msg_0", f.wtAt(1), f.repo, "main", wtText, 100)
	l1 := wtJSONLLine("msg_1", f.wtAt(2), f.repo, "main", wtText, 101)
	l2 := wtJSONLLine("msg_2", f.wtAt(3), f.repo, "main", wtText, 102)
	if err := os.WriteFile(h.path, []byte(l0+"\n"+l1+"\n"+l2[:len(l2)/2]), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := f.wtAt(10)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN)),
		"100 unattributed:main branch", "101 unattributed:main branch")
	wantHeld(t, h, int64(len(l0)+len(l1)+2), false)
}

// Pins: a positive release stops parseHeldBack's read there: the offset is the
// release, and no message after it is returned.
func TestParseHeldBack_StopsAtTheRelease(t *testing.T) {
	f := newWTFixture(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	l0 := wtJSONLLine("msg_0", f.wtAt(1), f.repo, "main", wtText, 100)
	l1 := wtJSONLLine("msg_1", f.wtAt(2), f.repo, "main", wtText, 101)
	l2 := wtJSONLLine("msg_2", f.wtAt(3), f.repo, "main", wtText, 102)
	if err := os.WriteFile(path, []byte(l0+"\n"+l1+"\n"+l2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release := int64(len(l0) + len(l1) + 2)
	_, off, groups, _, err := parseHeldBack(path, 0, sessionMetadata{}, release)
	if err != nil {
		t.Fatal(err)
	}
	if off != release {
		t.Errorf("offset %d, want the release %d", off, release)
	}
	var ids []string
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	if fmt.Sprint(ids) != "[msg_0 msg_1]" {
		t.Errorf("groups %v, want [msg_0 msg_1] (msg_2 starts at the release)", ids)
	}
}

// Pins: an idle read whose window exceeds heldChunkCap ends at the cap and
// reports the file held, so its wake reads the rest; the next idle read
// releases it.
func TestWatcher_IdleReadPastTheChunkCapStaysHeld(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	line := func(id string, n, out int) string { return wtJSONLLine(id, f.wtAt(n), f.repo, "main", wtText, out) }
	la, lb, lc, ld := line("msg_a", 1, 701), line("msg_b", 2, 702), line("msg_c", 3, 703), line("msg_d", 4, 704)
	prev := heldChunkCap
	heldChunkCap = int64(len(la) + len(lb) + 2)
	t.Cleanup(func() { heldChunkCap = prev })
	mtime := f.wtAt(10)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN), la, lb, lc, ld),
		"701 unattributed:main branch", "702 unattributed:main branch")
	wantHeld(t, h, int64(len(la)+len(lb)+2), true)
	wantEvents(t, h.idleRead(t, w, mtime, mtime.Add(idleN)),
		"703 unattributed:main branch", "704 unattributed:main branch")
	wantHeld(t, h, int64(len(la)+len(lb)+len(lc)+len(ld)+4), false)
}

// Pins (#919 under #823): a subagent file whose only message is held back,
// sharing session sess-S with a parent transcript that has stored events, is
// tombstoned by erasing the parent's developer during the hold, so neither the
// idle wake nor a restart ingests anything from it after the erase.
func TestWatcher_EraseDuringAHoldTombstonesAHeldOnlyFile(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			r := newEraseRig(t)
			r.worktrees = newWorktreeIndex(nil, nil)
			r.idle = 1500 * time.Millisecond
			if restart {
				r.idle = time.Hour
			}
			parent := filepath.Join(r.projects, "sess-S.jsonl")
			sub := filepath.Join(r.projects, "sess-S", "subagents", "agent-a1.jsonl")
			mkdirs(t, filepath.Dir(sub))
			cps := &hookedCheckpoints{DB: r.db}
			stop := r.run(t, cps)

			writeJSONLLine(t, parent, "sess-S", r.repo, "feature/42-foo", 111)
			appendTaggedLine(t, parent, "sess-S", r.repo, "b222", 222)
			want := 1
			if !restart {
				want = 2 // released before the subagent file exists, so no parent read races the erase
			}
			waitUntil(t, "the parent's events stored", func() bool { return len(r.sessionInputs(t, "sess-S")) == want })
			if err := os.WriteFile(sub, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			appendTaggedLine(t, sub, "sess-S", r.repo, "s333", 333)
			r.waitCheckpoint(t, sub, 0)

			if restart {
				stop()
			}
			loads := cps.loadCount(sub)
			counts, err := r.db.EraseDeveloper(context.Background(), "alice")
			if err != nil {
				t.Fatalf("EraseDeveloper: %v", err)
			}
			if counts["watcher_checkpoint"] != 2 {
				t.Errorf("erase changed %d watcher_checkpoint rows, want 2 (the parent and the held-only subagent file)", counts["watcher_checkpoint"])
			}
			if restart {
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(sub, old, old); err != nil {
					t.Fatal(err)
				}
				cps = &hookedCheckpoints{DB: r.db}
				r.run(t, cps)
				loads = 0
				appendTaggedLine(t, sub, "sess-S", r.repo, "s444", 444)
			}
			waitUntil(t, "the watcher read the subagent file's row after the erase", func() bool {
				return cps.loadCount(sub) > loads
			})
			time.Sleep(4 * eraseTestDebounce)

			if got := r.sessionInputs(t, "sess-S"); len(got) != 0 {
				t.Errorf("rows for the erased session after the erase = %v, want none (333/444 = the subagent file ingested after the erase)", got)
			}
			if cp, ok := r.checkpoint(t, sub); !ok || !cp.Tombstoned() {
				t.Errorf("subagent checkpoint = %+v (found %v), want an erasure tombstone", cp, ok)
			}
		})
	}
}
