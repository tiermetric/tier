package tokenacct

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func ptr(n int64) *int64 { return &n }

func codexUsage() RawUsage {
	return RawUsage{
		Input: ptr(1000), CacheRead: ptr(400), Output: ptr(100),
		Reasoning: ptr(40), Total: ptr(1100),
	}
}

func TestCodex(t *testing.T) {
	ceiling := int64(maxCumulativeTokens)
	tests := []struct {
		name    string
		change  func(*RawUsage)
		want    Usage
		wantErr string
		skip32  bool
	}{
		{name: "control", want: Usage{Input: 600, CacheRead: 400, Output: 100}},
		{name: "absent total", change: func(u *RawUsage) { u.Total = nil }, want: Usage{Input: 600, CacheRead: 400, Output: 100}},
		{name: "zero total skips cross-check", change: func(u *RawUsage) { u.Total = ptr(0) }, want: Usage{Input: 600, CacheRead: 400, Output: 100}},
		{name: "negative input", change: func(u *RawUsage) { u.Input = ptr(-1) }, wantErr: "negative token count"},
		{name: "negative output", change: func(u *RawUsage) { u.Output = ptr(-1) }, wantErr: "negative token count"},
		{name: "negative cache read", change: func(u *RawUsage) { u.CacheRead = ptr(-1) }, wantErr: "negative token count"},
		{name: "negative reasoning", change: func(u *RawUsage) { u.Reasoning = ptr(-1) }, wantErr: "negative token count"},
		{name: "negative total", change: func(u *RawUsage) { u.Total = ptr(-1) }, wantErr: "negative token count"},
		{name: "input ceiling", change: func(u *RawUsage) { u.Input = ptr(ceiling + 1) }, wantErr: "sanity ceiling"},
		{name: "output ceiling", change: func(u *RawUsage) { u.Output = ptr(ceiling + 1) }, wantErr: "sanity ceiling"},
		{name: "cache read ceiling", change: func(u *RawUsage) { u.CacheRead = ptr(ceiling + 1) }, wantErr: "sanity ceiling"},
		{name: "total ceiling", change: func(u *RawUsage) { u.Total = ptr(ceiling + 1) }, wantErr: "sanity ceiling"},
		{name: "input ceiling inclusive", skip32: true, change: func(u *RawUsage) {
			u.Input, u.CacheRead = ptr(ceiling), ptr(ceiling)
			u.Output, u.Reasoning, u.Total = ptr(0), ptr(0), ptr(ceiling)
		}, want: Usage{CacheRead: int(ceiling)}},
		{name: "ceiling inclusive", skip32: true, change: func(u *RawUsage) {
			u.Input, u.CacheRead = ptr(0), ptr(0)
			u.Output, u.Reasoning, u.Total = ptr(ceiling), ptr(0), ptr(ceiling)
		}, want: Usage{Output: int(ceiling)}},
		{name: "absent optional counts", change: func(u *RawUsage) {
			u.CacheRead, u.Reasoning, u.Total = nil, nil, nil
		}, want: Usage{Input: 1000, Output: 100}},
		{name: "total mismatch", change: func(u *RawUsage) { u.Total = ptr(9999) }, wantErr: "total_tokens=9999 != input_tokens+output_tokens=1100"},
		{name: "cached exceeds input", change: func(u *RawUsage) { u.CacheRead = ptr(1001) }, wantErr: "cached_input_tokens=1001 > input_tokens=1000"},
		{name: "reasoning exceeds output", change: func(u *RawUsage) { u.Reasoning = ptr(101) }, wantErr: "reasoning_output_tokens=101 > output_tokens=100"},
		// Current Codex accounting omits a separate nonnegative write class.
		{name: "cache write class currently omitted", change: func(u *RawUsage) { u.CacheWrite = ptr(200) }, want: Usage{Input: 600, CacheRead: 400, Output: 100}},
		{name: "negative cache write", change: func(u *RawUsage) { u.CacheWrite = ptr(-1) }, wantErr: "negative token count"},
		{name: "cache write ceiling ignored", change: func(u *RawUsage) { u.CacheWrite = ptr(ceiling + 1) }, want: Usage{Input: 600, CacheRead: 400, Output: 100}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip32 && strconv.IntSize < 64 {
				t.Skip("sanity ceiling exceeds 32-bit int range")
			}
			raw := codexUsage()
			if tc.change != nil {
				tc.change(&raw)
			}
			got, err := Codex(raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Codex() error = %v, want %q", err, tc.wantErr)
				}
				if got != (Usage{}) {
					t.Fatalf("refused usage = %+v, want zero", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Codex() = %+v, %v; want %+v, nil", got, err, tc.want)
			}
		})
	}
}

func TestCodexTurnCompletedWithoutTotal(t *testing.T) {
	// turn.completed's input_tokens, cached_input_tokens and output_tokens
	// mapped into the five nullable tokens_* fields; no total is exported.
	var raw RawUsage
	if err := json.Unmarshal([]byte(`{"tokens_input":1000,"tokens_output":100,"tokens_cache_read":400,"tokens_reasoning":null,"tokens_cache_write":null}`), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Total != nil {
		t.Fatal("turn.completed usage unexpectedly has a total")
	}
	got, err := Codex(raw)
	want := Usage{Input: 600, CacheRead: 400, Output: 100}
	if err != nil || got != want {
		t.Fatalf("Codex() = %+v, %v; want %+v, nil", got, err, want)
	}
}

func TestCodexReasoningIsSubsetNeverAdded(t *testing.T) {
	for _, reasoning := range []int{0, 40, 100, 101} {
		raw := codexUsage()
		raw.Reasoning = ptr(int64(reasoning))
		got, err := Codex(raw)
		if reasoning > 100 {
			if err == nil || !strings.Contains(err.Error(), "reasoning_output_tokens=101 > output_tokens=100") {
				t.Fatalf("reasoning outside output accepted: %+v, %v", got, err)
			}
			continue
		}
		want := Usage{Input: 600, CacheRead: 400, Output: 100}
		if err != nil || got != want {
			t.Fatalf("reasoning=%d: Codex() = %+v, %v; want %+v, nil", reasoning, got, err, want)
		}
	}
}

func TestCodexCachedCarvedFromInput(t *testing.T) {
	for _, cached := range []int{0, 400, 1000, 1001} {
		raw := codexUsage()
		raw.CacheRead = ptr(int64(cached))
		got, err := Codex(raw)
		if cached > 1000 {
			if err == nil || !strings.Contains(err.Error(), "cached_input_tokens=1001 > input_tokens=1000") {
				t.Fatalf("cache outside input accepted: %+v, %v", got, err)
			}
			continue
		}
		want := Usage{Input: 1000 - cached, CacheRead: cached, Output: 100}
		if err != nil || got != want {
			t.Fatalf("cached=%d: Codex() = %+v, %v; want %+v, nil", cached, got, err, want)
		}
	}
}

func TestCodexUnmeasured(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  RawUsage
	}{
		{"all nil", RawUsage{}},
		{"input nil", RawUsage{Output: ptr(0)}},
		{"output nil", RawUsage{Input: ptr(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Codex(tc.raw)
			if !errors.Is(err, ErrUnmeasured) || got != (Usage{}) {
				t.Fatalf("Codex() = %+v, %v; want zero usage, ErrUnmeasured", got, err)
			}
		})
	}
}

func TestRawUsageCountsAreInt64(t *testing.T) {
	typ := reflect.TypeOf(RawUsage{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != reflect.TypeOf((*int64)(nil)) {
			t.Errorf("RawUsage.%s type = %v, want *int64", field.Name, field.Type)
		}
	}
}

func TestCodexDocumentationScope(t *testing.T) {
	source, err := os.ReadFile("tokenacct.go")
	if err != nil {
		t.Fatal(err)
	}
	const paragraph = "Per-snapshot containment (cached ⊆ input)"
	if strings.Contains(string(source), paragraph) {
		t.Fatal("caller snapshot/delta explanation remains in tokenacct")
	}
	collector, err := os.ReadFile("../collector/codexrollout/parse.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(collector), "func deltaFrom(")
	if start < 0 {
		t.Fatal("deltaFrom declaration missing")
	}
	body := string(collector)[start:]
	paragraphAt := strings.Index(body, paragraph)
	callAt := strings.Index(body, "tokenacct.Codex(")
	if paragraphAt < 0 || paragraphAt >= callAt {
		t.Fatal("snapshot/delta explanation must precede the Codex call in deltaFrom")
	}
}

func TestCodexWideCounts(t *testing.T) {
	wide := int64(10_000_000_000)
	for _, tc := range []struct {
		name string
		json string
		want Usage
	}{
		{"input", `{"tokens_input":10000000000,"tokens_output":0}`, Usage{Input: int(wide)}},
		{"cache read", `{"tokens_input":10000000000,"tokens_cache_read":10000000000,"tokens_output":0}`, Usage{CacheRead: int(wide)}},
		{"output", `{"tokens_input":0,"tokens_output":10000000000}`, Usage{Output: int(wide)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw RawUsage
			if err := json.Unmarshal([]byte(tc.json), &raw); err != nil {
				t.Fatal(err)
			}
			got, err := Codex(raw)
			if strconv.IntSize == 32 {
				if err == nil || !strings.Contains(err.Error(), "int range") || got != (Usage{}) {
					t.Fatalf("Codex() = %+v, %v; want zero usage, int range refusal", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Codex() = %+v, %v; want %+v, nil", got, err, tc.want)
			}
		})
	}
}
