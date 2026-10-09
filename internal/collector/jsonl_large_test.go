package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONL_LargerThan64MiBAllEventsExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	content := `[{"type":"text","text":"` + strings.Repeat("x", 64<<10) + `"}]`
	var size int64
	count := 0
	for size < 65<<20 {
		line := wtJSONLLine(fmt.Sprint(count), time.Unix(0, 0).UTC(), "/repo", "main", content, 1)
		n, err := f.WriteString(line + "\n")
		if err != nil {
			t.Fatal(err)
		}
		size += int64(n)
		count++
	}
	info, err := f.Stat()
	if err != nil || info.Size() != size || size <= maxJSONLChunk {
		t.Fatalf("fixture size %d, err %v", size, err)
	}
	s, offset, err := parseSessionFileFromOffset(path, 0, sessionMetadata{}, true)
	if err != nil || s == nil || len(s.Messages) != count || s.OutputTok != count || offset != size {
		t.Fatalf("want %d messages, offset %d; got summary %v, offset %d, err %v", count, size, s != nil, offset, err)
	}
	seen := make(map[string]bool)
	for _, m := range s.Messages {
		if seen[m.messageID] || m.output != 1 {
			t.Fatalf("duplicate or incorrect message: %+v", m)
		}
		seen[m.messageID] = true
	}
	for i := 0; i < count; i++ {
		if !seen[fmt.Sprint(i)] {
			t.Fatalf("missing message %d", i)
		}
	}
	if next, end, err := parseSessionFileFromOffset(path, offset, metadataFromSummary(s), true); err != nil || next != nil || end != size {
		t.Fatalf("repeat read: summary %+v, offset %d, err %v", next, end, err)
	}
	t.Logf("parsed %d unique messages from %d bytes; final offset %d", count, size, offset)
}
