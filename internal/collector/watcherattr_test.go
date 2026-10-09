package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

const wtText = `[{"type":"text","text":"ok"}]`

// wtAt is the fixture's time plus n seconds.
func (f *wtFixture) wtAt(n int) time.Time { return f.at.Add(time.Duration(n) * time.Second) }

func sidechain(l string) string {
	return strings.Replace(l, `"isSidechain":false`, `"isSidechain":true`, 1)
}

// restart round-trips the tail state through the persisted checkpoint, as a
// restarted watcher seeds it.
func (h *heldRig) restart(t *testing.T) {
	t.Helper()
	cp, err := checkpointFromState(h.path, h.state)
	if err != nil {
		t.Fatal(err)
	}
	if h.state, err = stateFromCheckpoint(cp); err != nil {
		t.Fatal(err)
	}
}

// Pins: the carry a message sets in one read attributes a message resolved in
// the next read.
func TestWatcher_CarrySurvivesAcrossReads(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	wantEvents(t, h.read(t, w,
		wtJSONLLine("msg_1", f.wtAt(1), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 300),
		wtJSONLLine("msg_2", f.wtAt(2), f.repo, "main", wtText, 301),
	), "300 issue-11 worktree-toolpath")
	if got := h.state.metadata.LastWorktree; got != f.wtA {
		t.Errorf("carried worktree after read 1 = %q, want %q", got, f.wtA)
	}
	wantEvents(t, h.read(t, w, wtJSONLLine("msg_3", f.wtAt(3), f.repo, "main", wtText, 302)), "301 issue-11 carry")
}

// Pins: the carry survives a restart through the checkpoint blob, and is
// re-verified there: a worktree removed while the watcher was down carries
// nothing.
func TestWatcher_CarrySurvivesARestart(t *testing.T) {
	for name, tc := range map[string]struct {
		remove bool
		want   string
	}{
		"worktree kept":    {false, "301 issue-11 carry"},
		"worktree removed": {true, "301 unattributed:main branch"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWTFixture(t)
			h := newHeldRig(t, f.base, f.repo)
			wantEvents(t, h.read(t, &Watcher{worktrees: f.x},
				wtJSONLLine("msg_1", f.wtAt(1), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 300),
				wtJSONLLine("msg_2", f.wtAt(2), f.repo, "main", wtText, 301),
			), "300 issue-11 worktree-toolpath")
			cp, err := checkpointFromState(h.path, h.state)
			if err != nil {
				t.Fatal(err)
			}
			var blob map[string]any
			if err := json.Unmarshal([]byte(cp.Metadata), &blob); err != nil || blob["LastWorktree"] != f.wtA {
				t.Fatalf("checkpoint blob %s: LastWorktree want %q (err %v)", cp.Metadata, f.wtA, err)
			}
			h.restart(t)
			if tc.remove {
				if err := os.RemoveAll(f.wtA); err != nil {
					t.Fatal(err)
				}
			}
			wantEvents(t, h.read(t, &Watcher{worktrees: f.x}, wtJSONLLine("msg_3", f.wtAt(3), f.repo, "main", wtText, 302)), tc.want)
		})
	}
}

// Pins: a message still open at a read's end is not emitted and does not move
// the carry; the checkpoint stops at its first line; the next read attributes
// it once from all its lines. msg_m names worktree A on its first line and the
// main checkout on its second, so only the whole message resolves to today's
// rule: its first line alone would give worktree-toolpath.
func TestWatcher_HoldsBackAMessageSplitAcrossReads(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	l0 := wtJSONLLine("msg_0", f.wtAt(1), f.repo, "main", wtText, 200)
	l1 := wtJSONLLine("msg_m", f.wtAt(2), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 201)
	wantEvents(t, h.read(t, w, l0, l1), "200 unattributed:main branch")
	if h.state.offset != int64(len(l0)+1) || h.state.metadata.LastWorktree != "" {
		t.Fatalf("after read 1: offset %d carried %q, want offset %d (msg_m's first line) and nothing carried",
			h.state.offset, h.state.metadata.LastWorktree, len(l0)+1)
	}
	l2 := `{"type":"user","sessionId":"sess-823","message":{"role":"user","content":"x"}}`
	l3 := wtJSONLLine("msg_m", f.wtAt(3), f.repo, "main", wtRead(filepath.Join(f.repo, "m.go")), 201)
	l4 := wtJSONLLine("msg_n", f.wtAt(4), f.repo, "main", wtText, 202)
	wantEvents(t, h.read(t, w, l2, l3, l4), "201 unattributed:main branch")
	if want := int64(len(l0) + len(l1) + len(l2) + len(l3) + 4); h.state.offset != want {
		t.Errorf("after read 2: offset %d, want %d (msg_n's first line)", h.state.offset, want)
	}
}

// Pins: across reads and restarts every message is emitted exactly once, when
// streams interleave so a finished message (msg_x) has a line after an open
// one's (msg_p) start: the checkpoint holds msg_x back with it, so no fresh
// read returns a fragment of an emitted message.
func TestWatcher_NoMessageEmittedTwiceAcrossRestarts(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	line := func(id string, n int, content string, out int) string {
		return wtJSONLLine(id, f.wtAt(n), f.repo, "main", content, out)
	}
	l0 := line("msg_0", 1, wtText, 100)
	var got []string
	got = append(got, h.read(t, &Watcher{worktrees: f.x},
		l0,
		line("msg_x", 2, wtText, 101),
		sidechain(line("msg_p", 3, wtText, 102)),
		line("msg_x", 4, wtRead(filepath.Join(f.wtA, "a.go")), 101),
		line("msg_y", 5, wtText, 103),
	)...)
	if h.state.offset != int64(len(l0)+1) {
		t.Fatalf("after read 1: offset %d, want %d (msg_x's first line)", h.state.offset, len(l0)+1)
	}
	h.restart(t)
	got = append(got, h.read(t, &Watcher{worktrees: f.x},
		sidechain(line("msg_p2", 6, wtText, 104)),
		line("msg_z", 7, wtText, 105),
	)...)
	h.restart(t)
	got = append(got, h.read(t, &Watcher{worktrees: f.x},
		line("msg_q", 8, wtText, 106),
		sidechain(line("msg_p3", 9, wtText, 107)),
	)...)
	wantEvents(t, got,
		"100 unattributed:main branch",
		"102 issue-11 carry", "101 issue-11 worktree-toolpath", "103 issue-11 carry",
		"104 issue-11 carry", "105 issue-11 carry",
	)
}

// Pins: with attribution on, an erasure tombstone is still obeyed: the held
// message and later appends are never stored and the tombstone stays intact.
func TestWatcher_HeldBackReadsObeyTheEraseTombstone(t *testing.T) {
	r := newEraseRig(t)
	r.worktrees = newWorktreeIndex(nil, nil)
	cps := &hookedCheckpoints{DB: r.db}
	r.run(t, cps)

	path := filepath.Join(r.projects, "erased.jsonl")
	writeJSONLLine(t, path, "sess-erased", r.repo, "feature/42-foo", 111)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	appendTaggedLine(t, path, "sess-erased", r.repo, "b333", 333)
	var before store.WatcherCheckpoint
	waitUntil(t, "first message stored, the second held back", func() bool {
		var ok bool
		before, ok = r.checkpoint(t, path)
		return ok && before.Offset == first.Size() && len(r.sessionInputs(t, "sess-erased")) == 1
	})
	r.erase(t, path, before)

	loads := cps.loadCount(path)
	appendTaggedLine(t, path, "sess-erased", r.repo, "a222", 222)
	control := filepath.Join(r.projects, "control.jsonl")
	writeJSONLLine(t, control, "sess-control", r.repo, "feature/42-foo", 999)
	appendTaggedLine(t, control, "sess-control", r.repo, "c1", 1)
	waitUntil(t, "control session captured", func() bool { return len(r.sessionInputs(t, "sess-control")) == 1 })
	waitUntil(t, "the erased file's row read after the append", func() bool { return cps.loadCount(path) > loads })
	time.Sleep(4 * eraseTestDebounce)

	if got := r.sessionInputs(t, "sess-erased"); len(got) != 0 {
		t.Errorf("stored rows for the erased session = %v, want none (333 = the held message, 222 = the append)", got)
	}
	assertTombstone(t, r, path, before)
}

// Pins carryAt: a persisted root restores its one linked classification, and
// carries nothing when empty, or when the worktree verifies for two targets.
func TestWorktreeIndex_CarryAtReverifiesTheRoot(t *testing.T) {
	f := newWTFixture(t)
	wantA := pathClassification{Class: worktreeLinked, Target: f.repo, Root: f.wtA, AdminDir: filepath.Join(f.repo, ".git", "worktrees", "wtA")}
	if got := f.x.carryAt(f.wtA); got != wantA {
		t.Errorf("carryAt(wtA) = %+v, want %+v", got, wantA)
	}
	if got := f.x.carryAt(""); got != (pathClassification{}) {
		t.Errorf(`carryAt("") = %+v, want nothing`, got)
	}
	svc := filepath.Join(f.repo, "svc")
	mkdirs(t, svc)
	two := newWorktreeIndex([]string{f.repo, svc}, nil)
	if got := two.carryAt(f.wtA); got != (pathClassification{}) {
		t.Errorf("carryAt(wtA) with two targets in the worktree = %+v, want nothing", got)
	}
}

// Pins End: the offset of a message's last line, a stale line of a finished
// message included.
func TestMessageMerger_EndIsTheLastLineStaleIncluded(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	var m messageMerger
	m.add([]byte(wtJSONLLine("msg_a", at, "/r", "main", wtText, 1)), 100)
	m.add([]byte(wtJSONLLine("msg_a", at, "/r", "main", wtText, 1)), 200)
	m.add([]byte(wtJSONLLine("msg_b", at, "/r", "main", wtText, 1)), 300)
	m.add([]byte(wtJSONLLine("msg_a", at, "/r", "main", wtText, 1)), 400)
	m.flush()
	var got []string
	for _, g := range m.take() {
		got = append(got, fmt.Sprintf("%s %d-%d", g.ID, g.Start, g.End))
	}
	wantEvents(t, got, "msg_a 100-400", "msg_b 300-300")
}

func TestWatcher_OverLongLineDoesNotStallAHeldRead(t *testing.T) {
	f := newWTFixture(t)
	for _, w := range []*Watcher{{}, {worktrees: f.x}} {
		h := newHeldRig(t, t.TempDir(), f.repo)
		line := func(id string, n int) string {
			return wtJSONLLine(id, f.wtAt(n), f.repo, "feature/7-x", wtText, n)
		}
		rule := ""
		if w.worktrees != nil {
			rule = "branch"
		}
		at := f.wtAt(10)
		a, b := line("a", 100), line("b", 200)
		bad := strings.Repeat("x", maxJSONLLine+17)
		got := h.idleRead(t, w, at, at, a, bad, b)
		if w.worktrees != nil {
			wantHeld(t, h, int64(len(a)+len(bad)+2), true)
			got = append(got, h.idleRead(t, w, at, at.Add(idleN))...)
		}
		wantEvents(t, got, "100 issue-7 "+rule, "200 issue-7 "+rule)
		wantHeld(t, h, int64(len(a)+len(bad)+len(b)+3), false)
		h.restart(t)
		wantEvents(t, h.idleRead(t, w, at, at.Add(idleN), line("c", 300)), "300 issue-7 "+rule)
		wantEvents(t, h.idleRead(t, w, at, at.Add(idleN)))
	}
}

// Pins: a held read checkpoints the #490 latch and NextParseSeq as a parse
// stopped at the cut has them, not as the whole chunk left them, so the held
// harness-branch message later inherits the branch named before it.
func TestWatcher_HeldReadCheckpointsTheStateAtTheCut(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	l0 := wtJSONLLine("msg_0", f.wtAt(1), f.repo, "feature/7-x", wtText, 500)
	wantEvents(t, h.read(t, w,
		l0,
		wtJSONLLine("msg_h", f.wtAt(2), f.repo, "worktree-agent-a1b2", wtText, 501),
		`{"type":"user","sessionId":"sess-823","gitBranch":"feature/9-y","message":{"role":"user","content":"x"}}`,
		sidechain(wtJSONLLine("", f.wtAt(3), f.repo, "feature/9-y", wtText, 502)),
	), "500 issue-7 branch")
	cut := int64(len(l0) + 1)
	want, _, err := parseSessionLines(h.path, 0, sessionMetadata{}, true, nil, cut)
	if err != nil || want == nil {
		t.Fatalf("parse stopped at the cut: %v, %v", want, err)
	}
	if m := h.state.metadata; h.state.offset != cut || m.LastRealBranch != "feature/7-x" ||
		m.LastRealBranch != want.LastRealBranch || m.NextParseSeq != want.NextParseSeq {
		t.Fatalf("after read 1: offset %d latch %q seq %d, want offset %d latch %q seq %d",
			h.state.offset, m.LastRealBranch, m.NextParseSeq, cut, want.LastRealBranch, want.NextParseSeq)
	}
	wantEvents(t, h.read(t, w, wtJSONLLine("msg_z", f.wtAt(4), f.repo, "feature/9-y", wtText, 503)),
		"501 issue-7 branch", "502 issue-9 branch")
}

// Pins heldFrom's repetition: lowering the cut to msg_B's Start makes msg_A,
// visited before msg_B, straddle it, so the cut falls again to msg_A's Start.
func TestHeldFrom_LowersTheCutUntilNothingStraddles(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	main := func(id string) string { return wtJSONLLine(id, at, "/r", "main", wtText, 1) }
	side := func(id string) string { return sidechain(main(id)) }
	agent := strings.Replace(main("msg_O"), `"isSidechain":false`, `"isSidechain":true,"agentId":"x"`, 1)
	var m messageMerger
	for i, l := range []string{main("msg_0"), side("msg_A"), main("msg_B"), side("msg_A"), agent, main("msg_B"), side("msg_A2"), main("msg_C")} {
		m.add([]byte(l), int64(i*10))
	}
	open, ok := m.pendingFrom()
	if !ok || open != 40 {
		t.Fatalf("pendingFrom = %d, %v; want 40 (msg_O)", open, ok)
	}
	if got := heldFrom(m.take(), open); got != 10 {
		t.Errorf("heldFrom = %d, want 10 (msg_A's Start)", got)
	}
}

// Pins: a transcript replaced by a new file starts with no carry, whatever the
// old file's checkpoint carried.
func TestWatcher_ReplacedTranscriptStartsWithNoCarry(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	wantEvents(t, h.read(t, w,
		wtJSONLLine("msg_1", f.wtAt(1), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 300),
		wtJSONLLine("msg_2", f.wtAt(2), f.repo, "main", wtText, 301),
	), "300 issue-11 worktree-toolpath")
	if got := h.state.metadata.LastWorktree; got != f.wtA {
		t.Fatalf("carried worktree after read 1 = %q, want %q", got, f.wtA)
	}
	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, h.read(t, w,
		wtJSONLLine("msg_x", f.wtAt(3), f.repo, "main", wtText, 400),
		wtJSONLLine("msg_y", f.wtAt(4), f.repo, "main", wtText, 401),
	), "400 unattributed:main branch")
}

// Pins: a held read whose lines before the cut carry no usage does not
// checkpoint past them, so the branch a user line there names reaches the
// held harness-branch message: it books issue-9, not the issue-7 latch the
// previous read checkpointed.
func TestWatcher_HeldReadKeepsTheLatchOfAUsageFreePrefix(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	l0 := wtJSONLLine("", f.wtAt(1), f.repo, "feature/7-x", wtText, 600)
	wantEvents(t, h.read(t, w, l0), "600 issue-7 branch")
	if h.state.offset != int64(len(l0)+1) || h.state.metadata.LastRealBranch != "feature/7-x" {
		t.Fatalf("after read 1: offset %d latch %q, want offset %d latch feature/7-x",
			h.state.offset, h.state.metadata.LastRealBranch, len(l0)+1)
	}
	wantEvents(t, h.read(t, w,
		`{"type":"user","sessionId":"sess-823","gitBranch":"feature/9-y","message":{"role":"user","content":"x"}}`,
		wtJSONLLine("msg_h", f.wtAt(2), f.repo, "worktree-agent-a1b2", wtText, 601),
	))
	wantEvents(t, h.read(t, w, wtJSONLLine("msg_z", f.wtAt(3), f.repo, "feature/9-y", wtText, 602)),
		"601 issue-9 branch")
}

// Pins: a held read whose window exceeds heldChunkCap reads up to the cap and
// finishes every message open there, so an open message pinning the cut
// cannot stall the file: every message is emitted once across the reads.
func TestWatcher_HeldReadPastTheChunkCapReleases(t *testing.T) {
	f := newWTFixture(t)
	h := newHeldRig(t, f.base, f.repo)
	w := &Watcher{worktrees: f.x}
	line := func(id string, n, out int) string { return wtJSONLLine(id, f.wtAt(n), f.repo, "main", wtText, out) }
	lp := sidechain(line("msg_p", 1, 700))
	la, lb, lc := line("msg_a", 2, 701), line("msg_b", 3, 702), line("msg_c", 4, 703)
	prev := heldChunkCap
	heldChunkCap = int64(len(lp) + len(la) + len(lb) + len(lc) + 3)
	t.Cleanup(func() { heldChunkCap = prev })
	var got []string
	got = append(got, h.read(t, w, lp, la, lb, lc, line("msg_d", 5, 704), line("msg_e", 6, 705))...)
	if want := int64(len(lp) + len(la) + len(lb) + 3); h.state.offset != want {
		t.Fatalf("after read 1: offset %d, want %d (the last line inside the cap)", h.state.offset, want)
	}
	got = append(got, h.read(t, w)...)
	got = append(got, h.read(t, w, line("msg_f", 7, 706))...)
	wantEvents(t, got,
		"700 unattributed:main branch", "701 unattributed:main branch", "702 unattributed:main branch",
		"703 unattributed:main branch", "704 unattributed:main branch", "705 unattributed:main branch")
}

func TestWatcher_OverLongLineAcrossHeldCaps(t *testing.T) {
	f := newWTFixture(t)
	prev := heldChunkCap
	heldChunkCap = maxJSONLLine + 1024
	t.Cleanup(func() { heldChunkCap = prev })
	for _, prefix := range []bool{false, true} {
		t.Run(fmt.Sprint(prefix), func(t *testing.T) {
			h := newHeldRig(t, t.TempDir(), f.repo)
			w := &Watcher{worktrees: f.x}
			line := func(id string, n int) string {
				return wtJSONLLine(id, f.wtAt(n), f.repo, "feature/7-x", wtText, n)
			}
			lines := []string{strings.Repeat("x", int(3*heldChunkCap)+17), line("b", 200), line("c", 300)}
			want := []string{"200 issue-7 branch", "300 issue-7 branch"}
			if prefix {
				lines = append([]string{line("a", 100)}, lines...)
				want = append([]string{"100 issue-7 branch"}, want...)
			}
			at := f.wtAt(10)
			got := h.idleRead(t, w, at, at.Add(idleN), lines...)
			for i := 0; i < 6 && h.state.offset < fileSize(t, h.path); i++ {
				h.restart(t)
				got = append(got, h.idleRead(t, w, at, at.Add(idleN))...)
			}
			wantEvents(t, got, want...)
			wantHeld(t, h, fileSize(t, h.path), false)
		})
	}
}

func TestWatcher_AttributeHeldUsesClock(t *testing.T) {
	f := newWTFixture(t)
	w := &Watcher{worktrees: f.x, now: func() time.Time { return f.at.Add(365 * 24 * time.Hour) }}
	s := &sessionSummary{CWD: f.repo, Messages: []messageUsage{{messageID: "msg_x", cwd: f.repo, lineBranch: "main"}}}
	w.attributeHeld(s, []messageGroup{f.g(filepath.Join(f.wtA, "a.go"))}, "")
	if got := s.attribution["msg_x"]; got != (messageAttribution{}) || s.LastWorktree != "" {
		t.Fatalf("expired reflog attribution = %+v, carry = %q; want no worktree attribution or carry", got, s.LastWorktree)
	}
}
