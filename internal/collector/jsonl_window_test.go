package collector

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func smallJSONLWindow(t *testing.T, size int64) {
	t.Helper()
	old := jsonlWindowSize
	jsonlWindowSize = size
	t.Cleanup(func() { jsonlWindowSize = old })
}

func TestJSONLWindows_AllEventsAndOffsets(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	line := func(id, branch string, out int) string { return wtJSONLLine(id, at, "/repo", branch, wtText, out) }
	lines := []string{line("a", "feature/7-x", 100), line("b", "worktree-agent-a1b2", 200), line("a", "feature/7-x", 300), line("", "feature/7-x", 400), line("", "feature/7-x", 500)}
	path := writeJSONL(t, t.TempDir(), "session.jsonl", lines)
	want, end, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
	if err != nil || want == nil || len(want.Messages) != 4 || want.OutputTok != 1400 || want.NextParseSeq != 4 {
		t.Fatalf("baseline: %+v, %v", want, err)
	}
	for _, size := range []int64{int64(len(lines[0]) + 17), 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			smallJSONLWindow(t, size)
			got, offset, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
			if err != nil || offset != end || !reflect.DeepEqual(got, want) {
				t.Fatalf("window %d: summary %+v, offset %d, err %v; want %+v, offset %d", size, got, offset, err, want, end)
			}
			tail := line("c", "worktree-agent-a1b2", 600)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"+tail), 0o644); err != nil {
				t.Fatal(err)
			}
			got, offset, err = parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
			if err != nil || offset != end || !reflect.DeepEqual(got, want) {
				t.Fatalf("partial tail: %+v, %d, %v", got, offset, err)
			}
			got, offset, err = parseSessionFileFromOffset(path, end, metadataFromSummary(want), false)
			if err != nil || got == nil || len(got.Messages) != 1 || got.OutputTok != 600 || got.Messages[0].gitBranch != "feature/7-x" || offset != end+int64(len(tail)) {
				t.Fatalf("CLI tail: %+v, %d, %v", got, offset, err)
			}
			if err := os.Truncate(path, end); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJSONLWindows_WatcherResumesOnAppend(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHeldRig(t, base, repo)
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	line := func(id string, n int) string {
		return wtJSONLLine(id, at.Add(time.Duration(n)*time.Second), repo, "feature/7-x", wtText, n)
	}
	a, b := line("a", 100), line("b", 200)
	smallJSONLWindow(t, int64(len(a)+17))
	wantEvents(t, h.read(t, &Watcher{}, a, b), "100 issue-7 ", "200 issue-7 ")
	if h.state.offset != int64(len(a)+len(b)+2) {
		t.Fatalf("offset = %d", h.state.offset)
	}
	h.restart(t)
	c := line("c", 300)
	wantEvents(t, h.read(t, &Watcher{}, c), "300 issue-7 ")
	if h.state.offset != int64(len(a)+len(b)+len(c)+3) || h.state.metadata.NextParseSeq != 3 {
		t.Fatalf("state = %+v", h.state)
	}
	wantEvents(t, h.read(t, &Watcher{}))
}

func TestJSONLWindows_SkipsOverLongLine(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	line := wtJSONLLine("a", time.Now().UTC(), "/repo", "main", wtText, 100)
	last := strings.Replace(line, `"id":"a"`, `"id":"b"`, 1)
	path := writeJSONL(t, t.TempDir(), "session.jsonl", []string{line, strings.Repeat("x", 2*maxJSONLLine+17), last})
	for _, tail := range []bool{true, false} {
		logs.Reset()
		s, offset, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, tail)
		info, _ := os.Stat(path)
		if err != nil || s == nil || len(s.Messages) != 2 || s.OutputTok != 200 || offset != info.Size() {
			t.Fatalf("tail %v: summary %+v, offset %d, err %v", tail, s, offset, err)
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "count=1") {
			t.Fatalf("want one skipped-line warning, got %s", &logs)
		}
	}
}

func TestJSONLWindows_TruncatedAfterStat(t *testing.T) {
	for _, keep := range []int64{0, 7} {
		path := writeJSONL(t, t.TempDir(), "session.jsonl", []string{"first", "second"})
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		r := &jsonlWindowReader{file: f, end: info.Size(), buffer: make([]byte, info.Size())}
		if err := os.Truncate(path, keep); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "first\nsecond\n"[:keep] || r.offset != keep {
			t.Errorf("keep %d: got %q, offset %d, err %v", keep, got, r.offset, err)
		}
	}
}

func TestJSONLWindows_LineLimitBoundary(t *testing.T) {
	line := wtJSONLLine("large", time.Now().UTC(), "/repo", "main", wtText, 100)
	last := strings.Replace(line, `"id":"large"`, `"id":"last"`, 1)
	for _, size := range []int{maxJSONLLine - 1, maxJSONLLine, maxJSONLLine + (1 << 19)} {
		for _, prefix := range []int{0, 37} {
			t.Run(fmt.Sprintf("%d/%d", size, prefix), func(t *testing.T) {
				large := line + strings.Repeat(" ", size-len(line))
				path := writeJSONL(t, t.TempDir(), "session.jsonl", []string{strings.Repeat(" ", prefix), large, last})
				for _, tail := range []bool{false, true} {
					s, offset, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, tail)
					want := 1
					if size < maxJSONLLine {
						want = 2
					}
					if err != nil || s == nil || len(s.Messages) != want || offset != fileSize(t, path) {
						t.Fatalf("tail %v: summary %+v, offset %d, err %v; want %d messages and offset %d", tail, s, offset, err, want, fileSize(t, path))
					}
				}
			})
		}
	}
}

func TestJSONLWindowReader_EOFAtTruncatedOffset(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "session.jsonl", []string{"first", "second"})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r := &jsonlWindowReader{file: f, offset: 6, end: fileSize(t, path), buffer: make([]byte, 7)}
	if err := os.Truncate(path, r.offset); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 7)); n != 0 || err != io.EOF {
		t.Fatalf("Read at truncated offset = %d, %v; want 0, EOF", n, err)
	}
}
