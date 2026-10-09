package store

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// These tests pin #921 part 2 (R-2026-09-28-12): a --prices override that gives
// a built-in per-token model a different billing_mode is WARNed at load time,
// never refused, and pricing is left exactly as the override says.

// overrideFromEmbedded returns the embedded prices.yaml with a fresh override
// version, with every model line that starts with one of drop removed, and with
// extra appended to the models map (the last top-level key in prices.yaml).
func overrideFromEmbedded(t *testing.T, extra string, drop ...string) string {
	t.Helper()
	src := string(defaultPriceTableYAML)
	versionRE := regexp.MustCompile(`(?m)^version: \d+$`)
	if n := len(versionRE.FindAllString(src, -1)); n != 1 {
		t.Fatalf("embedded prices.yaml has %d version lines, want 1", n)
	}
	src = versionRE.ReplaceAllString(src, "version: 1000")
	lines := strings.SplitAfter(src, "\n")
	var b strings.Builder
	dropped := 0
	for _, l := range lines {
		skip := false
		for _, d := range drop {
			if strings.HasPrefix(l, "  "+d+":") {
				skip = true
			}
		}
		if skip {
			dropped++
			continue
		}
		b.WriteString(l)
	}
	if dropped != len(drop) {
		t.Fatalf("dropped %d model lines, want %d (%v)", dropped, len(drop), drop)
	}
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	b.WriteString(extra)
	return b.String()
}

func parseOverride(t *testing.T, doc string) map[string]modelPrice {
	t.Helper()
	tbl, _, err := parsePriceTable([]byte(doc))
	if err != nil {
		t.Fatalf("parsePriceTable(override): %v", err)
	}
	return tbl
}

const glm53ZaiSubscriptionRow = `  "glm-5.3@zai-coding-plan": { input_per_m: 1.40, output_per_m: 4.40, provider: zai, billing_mode: subscription }` + "\n"

func TestBillingModeChanges(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		drop  []string
		want  []billingModeChange
	}{
		{
			name:  "host-qualified subscription row outranks the embedded per-token row",
			extra: glm53ZaiSubscriptionRow,
			want: []billingModeChange{{
				Model: "glm-5.3", Host: "zai-coding-plan",
				EmbeddedMode: BillingPerToken, OverrideMode: BillingSubscription,
			}},
		},
		{
			name: "unchanged copy with a fresh version",
		},
		{
			name: "glm-5.3 dropped entirely is not a billing-mode change",
			drop: []string{"glm-5.3", "glm-5.3-flash"},
		},
		// Keys ComputeCostHost can never look up price nothing, so they do not WARN.
		{
			name:  "host not in normalizeHost form is unreachable",
			extra: `  "glm-5.3@ZAI-coding-plan": { input_per_m: 1.40, output_per_m: 4.40, provider: zai, billing_mode: subscription }` + "\n",
		},
		{
			name:  "HostUnknown host is unreachable",
			extra: `  "glm-5.3@unknown": { input_per_m: 1.40, output_per_m: 4.40, provider: zai, billing_mode: subscription }` + "\n",
		},
		{
			name:  "empty host is unreachable",
			extra: `  "glm-5.3@": { input_per_m: 1.40, output_per_m: 4.40, provider: zai, billing_mode: subscription }` + "\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := billingModeChanges(embeddedPriceTable, parseOverride(t, overrideFromEmbedded(t, c.extra, c.drop...)))
			if len(got) != len(c.want) {
				t.Fatalf("billingModeChanges = %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("change[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// glm53FlashSubscriptionRow flips the embedded per-token glm-5.3-flash row's
// own billing_mode (use with drop "glm-5.3-flash").
const glm53FlashSubscriptionRow = `  glm-5.3-flash: { input_per_m: 0.15, output_per_m: 0.50, cache_read_mult: 0.2, provider: zai, billing_mode: subscription }` + "\n"

// TestBillingModeChanges_SortedByModelThenHost pins the documented order. The
// changes are collected from a map, so one call passes a missing sort about
// half the time; 32 calls make a pass by luck vanishingly unlikely.
func TestBillingModeChanges_SortedByModelThenHost(t *testing.T) {
	override := parseOverride(t, overrideFromEmbedded(t, glm53ZaiSubscriptionRow+glm53FlashSubscriptionRow, "glm-5.3-flash"))
	want := []billingModeChange{
		{Model: "glm-5.3", Host: "zai-coding-plan", EmbeddedMode: BillingPerToken, OverrideMode: BillingSubscription},
		{Model: "glm-5.3-flash", EmbeddedMode: BillingPerToken, OverrideMode: BillingSubscription},
	}
	for attempt := range 32 {
		got := billingModeChanges(embeddedPriceTable, override)
		if !slices.Equal(got, want) {
			t.Fatalf("attempt %d: billingModeChanges = %+v, want %+v", attempt, got, want)
		}
	}
}

// TestLoadPriceTable_BillingModeBaselineIsEmbedded pins that the comparison
// baseline is the EMBEDDED table, not the currently loaded one: loading the same
// flipping override twice must WARN both times.
func TestLoadPriceTable_BillingModeBaselineIsEmbedded(t *testing.T) {
	restoreDefaultPriceTable(t)
	buf := captureUnknownModelLogger(t)
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(overrideFromEmbedded(t, glm53FlashSubscriptionRow, "glm-5.3-flash")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, load := range []string{"A", "B (identical to A)"} {
		buf.Reset()
		if _, err := LoadPriceTable(path); err != nil {
			t.Fatalf("load %s: %v", load, err)
		}
		if n := strings.Count(buf.String(), "different billing mode"); n != 1 {
			t.Fatalf("load %s: got %d billing-mode WARNs, want 1:\n%s", load, n, buf.String())
		}
	}
}

// TestBillingModeChanges_ModelOnlyRow pins the model-only arm: flipping an
// embedded per-token row's own billing_mode is a change with an empty host.
func TestBillingModeChanges_ModelOnlyRow(t *testing.T) {
	embedded := map[string]modelPrice{"m": {billingMode: BillingPerToken}}
	override := map[string]modelPrice{"m": {billingMode: BillingSubscription}}
	got := billingModeChanges(embedded, override)
	want := billingModeChange{Model: "m", EmbeddedMode: BillingPerToken, OverrideMode: BillingSubscription}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("billingModeChanges = %+v, want [%+v]", got, want)
	}
}

// TestLoadPriceTable_WarnsOnBillingModeChange drives the real load path: the
// override loads (never refused), prices glm-5.3 on the zai route as the
// override says, and emits exactly one WARN naming the model, host and modes.
func TestLoadPriceTable_WarnsOnBillingModeChange(t *testing.T) {
	restoreDefaultPriceTable(t)
	buf := captureUnknownModelLogger(t)
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(overrideFromEmbedded(t, glm53ZaiSubscriptionRow)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPriceTable(path); err != nil {
		t.Fatalf("LoadPriceTable refused the override: %v", err)
	}
	if _, mode := ComputeCostHost("zai-coding-plan", "glm-5.3", CostUsage{Input: 1}); mode != BillingSubscription {
		t.Errorf("glm-5.3@zai-coding-plan billing mode = %q, want the override's %q", mode, BillingSubscription)
	}
	out := buf.String()
	if n := strings.Count(out, "different billing mode"); n != 1 {
		t.Fatalf("got %d billing-mode WARNs, want 1:\n%s", n, out)
	}
	for _, want := range []string{
		"level=WARN",
		`model="\"glm-5.3\""`,
		`host="\"zai-coding-plan\""`,
		"embedded_billing_mode=per_token",
		"override_billing_mode=subscription",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WARN missing %q:\n%s", want, out)
		}
	}
}

// TestLoadPriceTable_NoBillingModeWarnForUnchangedCopy is the control arm: the
// same load path with an unchanged copy logs no billing-mode WARN.
func TestLoadPriceTable_NoBillingModeWarnForUnchangedCopy(t *testing.T) {
	restoreDefaultPriceTable(t)
	buf := captureUnknownModelLogger(t)
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(overrideFromEmbedded(t, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPriceTable(path); err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	if strings.Contains(buf.String(), "different billing mode") {
		t.Errorf("unexpected billing-mode WARN:\n%s", buf.String())
	}
}

// TestWarnBillingModeChanges_EscapesControlChars pins the logsafe barrier on
// both operator-controlled attributes: a control character in a model or host
// key must not reach the log as a raw byte.
func TestWarnBillingModeChanges_EscapesControlChars(t *testing.T) {
	buf := captureUnknownModelLogger(t)
	embedded := map[string]modelPrice{"evil\nmodel\x1b[2J": {billingMode: BillingPerToken}}
	override := map[string]modelPrice{"evil\nmodel\x1b[2J@host\rx": {billingMode: BillingSubscription}}
	changes := billingModeChanges(embedded, override)
	if len(changes) != 1 {
		t.Fatalf("billingModeChanges = %+v, want one change", changes)
	}
	warnBillingModeChanges(changes)
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("WARN is not a single line:\n%q", out)
	}
	for _, raw := range []string{"\r", "\x1b"} {
		if strings.Contains(out, raw) {
			t.Errorf("WARN carries raw %q:\n%q", raw, out)
		}
	}
	// logsafe strips CR/LF and %q-escapes the rest.
	if !strings.Contains(out, `model="\"evilmodel\\x1b[2J\""`) || !strings.Contains(out, `host="\"hostx\""`) {
		t.Errorf("WARN does not carry the sanitized model and host:\n%s", out)
	}
}
