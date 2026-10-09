package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// heldRig drives Watcher.process directly, one read at a time, over one
// transcript booked to repo, carrying the returned tail state between reads as
// the watcher's offsets map does.
type heldRig struct {
	path    string
	repo    string
	targets []resolvedPath
	cache   *commitCache
	rec     *recordingStore
	state   parseState
	held    bool // the last read's held result
}

func newHeldRig(t *testing.T, base, repo string) *heldRig {
	t.Helper()
	dir := filepath.Join(base, "claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	noCommits := func(context.Context, string, time.Time) ([]gitCommit, error) { return nil, nil }
	return &heldRig{
		path: filepath.Join(dir, "s.jsonl"), repo: repo, targets: resolveTargets([]string{repo}),
		cache: newCommitCache(time.Hour, time.Hour, time.Second, noCommits, time.Now), rec: &recordingStore{},
	}
}

// read appends lines to the transcript, runs one read by w, and returns the
// events it emitted, each as "<output_tok> <issue> <rule>".
func (h *heldRig) read(t *testing.T, w *Watcher, lines ...string) []string {
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
	before := len(h.rec.snapshot())
	w.Ingester = h.rec
	w.RepoSlugs = map[string]string{h.repo: "owner/repo"}
	st, ok, held := w.process(context.Background(), h.path, h.state, h.targets, "dev", h.cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !ok {
		t.Fatal("process reported failure")
	}
	h.state, h.held = st, held
	var out []string
	for _, e := range h.rec.snapshot()[before:] {
		out = append(out, fmt.Sprintf("%d %s %s", e.OutputTok, e.IssueID, e.AttributionRule))
	}
	return out
}

// Pins: with #823 attribution off, the watcher still emits a message that is
// open at a read's end, re-emits it when its next line arrives, and writes the
// checkpoint offset and metadata blob exactly as it did before C7 (these
// literals also pass on 0a517fb).
func TestWatcher_AttributionOffLeavesReadsAndCheckpointsUnchanged(t *testing.T) {
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
	l2 := wtJSONLLine("msg_b", at.Add(2*time.Second), repo, "feature/7-x", wtRead("/somewhere/x.go"), 401)

	wantEvents(t, h.read(t, &Watcher{}, l0, l1), "400 issue-7 ", "401 issue-7 ")
	wantCheckpoint(t, h, int64(len(l0)+len(l1)+2),
		`{"SessionID":"sess-823","GitBranch":"feature/7-x","CWD":"`+repo+`","StartTime":"2026-09-29T10:00:00Z",`+
			`"Model":"claude-sonnet-4","LastRealBranch":"feature/7-x","NextParseSeq":2}`)

	wantEvents(t, h.read(t, &Watcher{}, l2), "401 issue-7 ")
	wantCheckpoint(t, h, int64(len(l0)+len(l1)+len(l2)+3),
		`{"SessionID":"sess-823","GitBranch":"feature/7-x","CWD":"`+repo+`","StartTime":"2026-09-29T10:00:00Z",`+
			`"Model":"claude-sonnet-4","LastRealBranch":"feature/7-x","NextParseSeq":3}`)
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events:\n got %q\nwant %q", got, want)
	}
}

func wantCheckpoint(t *testing.T, h *heldRig, offset int64, metadata string) {
	t.Helper()
	cp, err := checkpointFromState(h.path, h.state)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Offset != offset || cp.Metadata != metadata {
		t.Errorf("checkpoint offset %d metadata\n %s\nwant offset %d metadata\n %s", cp.Offset, cp.Metadata, offset, metadata)
	}
}
