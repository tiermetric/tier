package collector

import (
	"sync"
	"testing"
)

func TestClampNegativeTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          []int
		want        []int
		wantClamped bool
	}{
		{
			name:        "all positive is untouched",
			in:          []int{1000, 500, 100, 200, 300},
			want:        []int{1000, 500, 100, 200, 300},
			wantClamped: false,
		},
		{
			name:        "all zero is untouched",
			in:          []int{0, 0, 0, 0, 0},
			want:        []int{0, 0, 0, 0, 0},
			wantClamped: false,
		},
		{
			name:        "one negative clamps only that field",
			in:          []int{-500, 200, 100, 0, 0},
			want:        []int{0, 200, 100, 0, 0},
			wantClamped: true,
		},
		{
			name:        "all negative clamps every field",
			in:          []int{-1, -2, -3, -4, -5},
			want:        []int{0, 0, 0, 0, 0},
			wantClamped: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Copy so each case owns its backing array; take addresses of the copy.
			vals := append([]int(nil), tt.in...)
			ptrs := make([]*int, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			got := ClampNegativeTokens(ptrs...)
			if got != tt.wantClamped {
				t.Errorf("ClampNegativeTokens returned %v, want %v", got, tt.wantClamped)
			}
			for i := range vals {
				if vals[i] != tt.want[i] {
					t.Errorf("field %d = %d, want %d", i, vals[i], tt.want[i])
				}
			}
		})
	}
}

// countingRecorder is a fake ClampRecorder that tallies Inc calls per source
// label, guarded by a mutex so concurrent RecordClamp callers don't race on the
// map itself (the atomic.Pointer seam guards the recorder swap, not the map).
type countingRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingRecorder() *countingRecorder {
	return &countingRecorder{counts: make(map[string]int)}
}

func (c *countingRecorder) Inc(labelValues ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	src := ""
	if len(labelValues) > 0 {
		src = labelValues[0]
	}
	c.counts[src]++
}

func (c *countingRecorder) get(src string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[src]
}

// TestClampRecorderCounts installs a fake recorder and parses a JSONL fixture
// carrying TWO negative usage fields in a single assistant entry, then asserts
// exactly ONE ("jsonl") increment — the counter is per clamped EVENT, not per
// field. The seam is atomic by design; the test restores it via t.Cleanup.
func TestClampRecorderCounts(t *testing.T) {
	rec := newCountingRecorder()
	SetClampRecorder(rec)
	t.Cleanup(func() { SetClampRecorder(nil) })

	dir := t.TempDir()
	// Single assistant entry with two negative fields (input and cache_read).
	line := `{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-neg","gitBranch":"feature/42-foo","cwd":"/repo","message":{"id":"msg_neg","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":-500,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":-10}}}`
	path := writeJSONL(t, dir, "neg.jsonl", []string{line})

	if _, err := parseSessionFile(path); err != nil {
		t.Fatalf("parseSessionFile: %v", err)
	}
	if got := rec.get(SourceJSONL); got != 1 {
		t.Errorf("jsonl clamp count = %d, want 1 (one event, not one per field)", got)
	}
	if got := rec.get(SourceProxy); got != 0 {
		t.Errorf("proxy clamp count = %d, want 0", got)
	}
}

// TestClampRecorder_ConcurrentSwapAndRecordRaceClean genuinely exercises the
// atomic.Pointer seam: one goroutine repeatedly swaps the recorder via
// SetClampRecorder while others call RecordClamp concurrently. Under -race this
// fails if the seam were a plain (unsynchronized) interface variable. It asserts
// no crash/torn read, not an exact count (swaps intentionally drop some bumps).
func TestClampRecorder_ConcurrentSwapAndRecordRaceClean(t *testing.T) {
	t.Cleanup(func() { SetClampRecorder(nil) })
	rec := newCountingRecorder()
	SetClampRecorder(rec)

	const workers = 8
	const iters = 500
	var wg sync.WaitGroup

	// Swapper: churns the installed recorder (rec <-> nil) under the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				SetClampRecorder(rec)
			} else {
				SetClampRecorder(nil)
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				RecordClamp(SourceProxy)
			}
		}()
	}
	wg.Wait()
}
