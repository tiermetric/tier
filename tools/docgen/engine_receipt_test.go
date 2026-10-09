package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestEngineDocsReceipt(t *testing.T) {
	section := func(path, start, end string) string {
		t.Helper()
		text := readFile(t, "../../"+path)
		_, text, ok := strings.Cut(text, start)
		if !ok {
			t.Fatalf("%s: missing section %q", path, start)
		}
		text, _, _ = strings.Cut(text, end)
		return strings.Join(strings.Fields(text), " ")
	}
	for _, tc := range []struct {
		name, path, start, end, want string
	}{
		{"seal version", "docs/quickstart.md", "Before the first seal", "After success", "requires v0.5.2 or newer"},
		{"developer query", "docs/api-compatibility.md", "#### `GET /api/v1/scores`", "- `?repo=`", "Query in **developer mode**"},
		{"team query", "docs/api-compatibility.md", "#### `GET /api/v1/scores`", "- `?repo=`", "In **team/division mode**, only `?period=YYYY-MM` is accepted"},
		{"developer comparison", "docs/peer-learning.md", "Every delta", "**Worked example", "In **developer mode**, the endpoint reuses"},
		{"team comparison", "docs/peer-learning.md", "Every delta", "**Worked example", "intersection k floor"},
		{"dated memberships", "README.md", "The import only adds and updates", "The whole file", "a later move preserves earlier attribution"},
		{"cache pointer", "docs/reference-price-table.md", "## 1. Cloud", "### 1.1", "see §7 below"},
		{"fast pointer", "docs/reference-price-table.md", "| `claude-opus-5`", "\n", "[fast-mode caveat](../internal/store/prices.yaml)"},
		{"table source", "docs/reference-price-table.md", "**Important note on Opus", "---", "price table (`internal/store/prices.yaml`)"},
		{"grok input", "docs/reference-price-table.md", "### 1.3", "### 1.4", "| `grok-3-mini` | xAI | 0.30 | 0.50 |"},
		{"grok effective", "docs/reference-price-table.md", "### 3.1", "## 4.", "| grok-3-mini | xAI | 0.30 | 0.50 | 0.43 |"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(section(tc.path, tc.start, tc.end), tc.want) {
				t.Errorf("%s: missing %q", tc.path, tc.want)
			}
		})
	}
	t.Run("effective cost bands", func(t *testing.T) {
		text := readFile(t, "../../docs/reference-price-table.md")
		_, text, _ = strings.Cut(text, "### 3.1")
		text, _, _ = strings.Cut(text, "## 4.")
		band, rows := 0, 0
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "#### TIER ") {
				band++
			}
			cells := strings.Split(line, "|")
			if len(cells) != 7 || strings.TrimSpace(cells[1]) == "Model" || strings.HasPrefix(cells[1], "---") {
				continue
			}
			cost, err := strconv.ParseFloat(strings.TrimSpace(cells[5]), 64)
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if cost >= 10 {
				want = 1
			} else if cost >= 1 {
				want = 2
			}
			if band != want {
				t.Errorf("%s: effective $%.2f is in TIER %d, want TIER %d", strings.TrimSpace(cells[1]), cost, band, want)
			}
			rows++
		}
		if band != 3 || rows != 51 {
			t.Errorf("got %d bands and %d model rows, want 3 and 51", band, rows)
		}
	})
}
