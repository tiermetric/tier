package tokenacct

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func opencodeUsage() RawUsage {
	return RawUsage{
		Input: ptr(100), Output: ptr(20), Reasoning: ptr(5),
		CacheRead: ptr(10), CacheWrite: ptr(15), Total: ptr(150),
	}
}

func TestOpencode(t *testing.T) {
	ceiling := int64(maxMessageTokens)
	want := Usage{Input: 100, Output: 25, CacheRead: 10, CacheWrite: 15}
	type testCase struct {
		name    string
		change  func(*RawUsage)
		want    Usage
		wantErr string
	}
	tests := []testCase{
		{name: "five-term identity", want: want},
		{name: "cache write outside total", change: func(u *RawUsage) { u.Total = ptr(135) }, wantErr: "additive identity broken"},
		{name: "Codex-shaped total", change: func(u *RawUsage) { u.Total = ptr(120) }, wantErr: "additive identity broken"},
		{name: "absent total", change: func(u *RawUsage) { u.Total = nil }, want: want},
		{name: "present zero total checked", change: func(u *RawUsage) { u.Total = ptr(0) }, wantErr: "additive identity broken"},
		{name: "absent optional counts", change: func(u *RawUsage) {
			u.Reasoning, u.CacheRead, u.CacheWrite, u.Total = nil, nil, nil, ptr(120)
		}, want: Usage{Input: 100, Output: 20}},
		{name: "measured zero counts", change: func(u *RawUsage) {
			*u = RawUsage{Input: ptr(0), Output: ptr(0), Total: ptr(0)}
		}, want: Usage{}},
		{name: "cache read exceeds fresh input", change: func(u *RawUsage) {
			u.CacheRead, u.Total = ptr(200), ptr(340)
		}, want: Usage{Input: 100, Output: 25, CacheRead: 200, CacheWrite: 15}},
		{name: "ceiling inclusive", change: func(u *RawUsage) {
			*u = RawUsage{Input: ptr(ceiling), Output: ptr(0), Total: ptr(ceiling)}
		}, want: Usage{Input: int(ceiling)}},
		{name: "derived output fits int32", change: func(u *RawUsage) {
			*u = RawUsage{Input: ptr(0), Output: ptr(ceiling), Reasoning: ptr(ceiling)}
		}, want: Usage{Output: int(2 * ceiling)}},
		{name: "five-term sum exceeds int32", change: func(u *RawUsage) {
			*u = RawUsage{Input: ptr(ceiling), Output: ptr(ceiling), Reasoning: ptr(ceiling),
				CacheRead: ptr(ceiling), CacheWrite: ptr(ceiling), Total: ptr(ceiling)}
		}, wantErr: "input+output+reasoning+cache.read+cache.write=2684354560"},
	}
	// Every raw field participates in both refusals, including optional Total.
	fields := []struct {
		name string
		set  func(*RawUsage, *int64)
	}{
		{"input", func(u *RawUsage, n *int64) { u.Input = n }},
		{"output", func(u *RawUsage, n *int64) { u.Output = n }},
		{"reasoning", func(u *RawUsage, n *int64) { u.Reasoning = n }},
		{"cache read", func(u *RawUsage, n *int64) { u.CacheRead = n }},
		{"cache write", func(u *RawUsage, n *int64) { u.CacheWrite = n }},
		{"total", func(u *RawUsage, n *int64) { u.Total = n }},
	}
	for _, field := range fields {
		tests = append(tests,
			testCase{name: "negative " + field.name, change: func(u *RawUsage) { field.set(u, ptr(-1)) }, wantErr: "negative token count"},
			testCase{name: "ceiling " + field.name, change: func(u *RawUsage) { field.set(u, ptr(ceiling+1)) }, wantErr: "sanity ceiling"},
		)
	}
	tests = append(tests, testCase{name: "refuse before overflowing sum", change: func(u *RawUsage) {
		*u = RawUsage{Input: ptr(math.MaxInt64), Output: ptr(math.MaxInt64), Reasoning: ptr(math.MaxInt64),
			CacheRead: ptr(math.MaxInt64), CacheWrite: ptr(math.MaxInt64), Total: ptr(math.MaxInt64)}
	}, wantErr: "sanity ceiling"})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := opencodeUsage()
			if tc.change != nil {
				tc.change(&raw)
			}
			got, err := Opencode(raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != (Usage{}) {
					t.Fatalf("Opencode() = %+v, %v; want zero usage, error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Opencode() = %+v, %v; want %+v, nil", got, err, tc.want)
			}
		})
	}
}

func TestOpencodeReasoningIsAdditive(t *testing.T) {
	for _, reasoning := range []int64{0, 5, 20, 180} {
		raw := opencodeUsage()
		raw.Reasoning, raw.Total = ptr(reasoning), ptr(145+reasoning)
		got, err := Opencode(raw)
		want := Usage{Input: 100, Output: int(20 + reasoning), CacheRead: 10, CacheWrite: 15}
		if err != nil || got != want {
			t.Fatalf("reasoning=%d: Opencode() = %+v, %v; want %+v, nil", reasoning, got, err, want)
		}
	}
}

func TestOpencodeAcceptsReasoningAboveOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  RawUsage
		want Usage
	}{
		{"measured row", RawUsage{Input: ptr(26455), Output: ptr(8), Reasoning: ptr(14), CacheRead: ptr(128), Total: ptr(26605)},
			Usage{Input: 26455, Output: 22, CacheRead: 128}},
		{"reasoning nine times output", RawUsage{Input: ptr(100), Output: ptr(20), Reasoning: ptr(180), Total: ptr(300)},
			Usage{Input: 100, Output: 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Opencode(tc.raw)
			if err != nil || got != tc.want {
				t.Fatalf("Opencode() = %+v, %v; want %+v, nil", got, err, tc.want)
			}
		})
	}
}

func TestOpencodeUnmeasured(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  RawUsage
	}{
		{"all nil", RawUsage{}},
		{"input nil", RawUsage{Output: ptr(0)}},
		{"output nil", RawUsage{Input: ptr(0)}},
		{"reasoning and total nil", RawUsage{Input: ptr(100), Output: ptr(20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Opencode(tc.raw)
			if !errors.Is(err, ErrUnmeasured) || got != (Usage{}) {
				t.Fatalf("Opencode() = %+v, %v; want zero usage, ErrUnmeasured", got, err)
			}
		})
	}
}
