package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector/opencode"
	"github.com/tiermetric/tier/internal/config"
)

// TestOpencodeWatchCheck pins the fail-fast matrix (#719): the Opencode
// collector attributes to the SAME repos as --watch-repo, so enabling it with no
// watched repo is a knob that does nothing. It aborts a normal serve, warns under
// --read-only (which deliberately disables all capture, so the mismatch is
// expected), and is silent whenever there is a repo or the collector is off.
func TestOpencodeWatchCheck(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		repos     int
		readOnly  bool
		wantWarn  bool
		wantFatal bool
	}{
		{name: "off_no_repos", enabled: false, repos: 0},
		{name: "off_with_repos", enabled: false, repos: 2},
		{name: "on_with_repos", enabled: true, repos: 1},
		{name: "on_no_repos_aborts", enabled: true, repos: 0, wantFatal: true},
		{name: "on_no_repos_readonly_warns", enabled: true, repos: 0, readOnly: true, wantWarn: true},
		// A read-only serve WITH repos is silent: read-only nulls the settings, so
		// there is nothing to warn about.
		{name: "on_with_repos_readonly", enabled: true, repos: 1, readOnly: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, fatal := opencodeWatchCheck(tc.enabled, tc.repos, tc.readOnly)
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn = %q, wantWarn = %v", warn, tc.wantWarn)
			}
			if (fatal != "") != tc.wantFatal {
				t.Errorf("fatal = %q, wantFatal = %v", fatal, tc.wantFatal)
			}
			if warn != "" && fatal != "" {
				t.Error("a single configuration must not be both a warning and a fatal")
			}
			if tc.wantFatal && !strings.Contains(fatal, "--watch-repo") {
				t.Errorf("the fatal must name the fix; got %q", fatal)
			}
		})
	}
}

// TestOpencodeProxyDoubleCountWarn pins the configuration in which Opencode spend
// is captured twice (#719).
//
// It must NOT fire for either half alone, or it becomes noise operators learn to
// ignore — and the wording must say what makes this unfixable by a better key:
// the two idempotency keys are unrelatable, so the rows ADD rather than collide.
func TestOpencodeProxyDoubleCountWarn(t *testing.T) {
	if got := opencodeProxyDoubleCountWarn(false, false); got != "" {
		t.Errorf("neither half enabled must be silent; got %q", got)
	}
	if got := opencodeProxyDoubleCountWarn(true, false); got != "" {
		t.Errorf("the collector alone must be silent; got %q", got)
	}
	if got := opencodeProxyDoubleCountWarn(false, true); got != "" {
		t.Errorf("a proxy alone must be silent; got %q", got)
	}
	got := opencodeProxyDoubleCountWarn(true, true)
	if got == "" {
		t.Fatal("both enabled must warn")
	}
	for _, want := range []string{"TWICE", "message.id", "response id", "do not dedup"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning must mention %q so an operator can tell it from the Codex one; got %q", want, got)
		}
	}
}

// TestOpencodeUnauditedStartupModels pins serve --opencode's price-table probe:
// under the embedded table both supported GLM-5.3 models price at an audited
// rate, so the startup WARN loop gets nothing; under a --prices table that
// dropped those rows it gets exactly glm-5.3 and glm-5.3-flash, in that order.
func TestOpencodeUnauditedStartupModels(t *testing.T) {
	loadEmbeddedPriceTable(t)
	if got := opencodeUnauditedStartupModels(); len(got) != 0 {
		t.Errorf("under the embedded table no model may WARN; got %q", got)
	}

	loadDeterministicPrices(t)
	want := []string{"glm-5.3", "glm-5.3-flash"}
	if got := opencodeUnauditedStartupModels(); !slices.Equal(got, want) {
		t.Errorf("under a table lacking the glm-5.3 rows = %q, want %q", got, want)
	}
}

// TestResolveOpencodeConfig covers the enablement rule: the collector needs no
// credential, so the --opencode FLAG alone enables it and the config block exists
// only to override defaults. A present-but-invalid block is a fail-fast startup
// error even when the flag alone would have enabled it — an operator who wrote
// scan_interval: "2s" needs to be told, not quietly given 5m.
func TestResolveOpencodeConfig(t *testing.T) {
	strp := func(s string) *string { return &s }

	t.Run("no_flag_no_block_disabled", func(t *testing.T) {
		got, err := resolveOpencodeConfig(nil, false)
		if err != nil || got != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
		}
	})
	t.Run("flag_alone_enables_with_defaults", func(t *testing.T) {
		got, err := resolveOpencodeConfig(nil, true)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got == nil {
			t.Fatal("the flag alone must enable the collector")
		}
		if got.interval != opencode.DefaultScanInterval {
			t.Errorf("interval = %s, want the package default %s", got.interval, opencode.DefaultScanInterval)
		}
		if got.dbPath != "" {
			t.Errorf("dbPath = %q, want empty (the collector resolves its own default)", got.dbPath)
		}
	})
	t.Run("block_alone_enables", func(t *testing.T) {
		got, err := resolveOpencodeConfig(&config.OpencodeConfig{}, false)
		if err != nil || got == nil {
			t.Fatalf("the block alone must enable the collector; got (%v, %v)", got, err)
		}
	})
	t.Run("block_overrides", func(t *testing.T) {
		got, err := resolveOpencodeConfig(&config.OpencodeConfig{
			DBPath:       strp("  /custom/opencode.db  "),
			ScanInterval: strp("90s"),
		}, false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.dbPath != "/custom/opencode.db" {
			t.Errorf("dbPath = %q, want the trimmed override", got.dbPath)
		}
		if got.interval != 90*time.Second {
			t.Errorf("interval = %s, want 90s", got.interval)
		}
	})
	t.Run("unparseable_interval_is_fatal", func(t *testing.T) {
		if _, err := resolveOpencodeConfig(&config.OpencodeConfig{ScanInterval: strp("banana")}, true); err == nil {
			t.Error("an unparseable scan_interval must fail startup, not fall back to the default")
		}
	})
	t.Run("interval_below_the_floor_is_fatal", func(t *testing.T) {
		_, err := resolveOpencodeConfig(&config.OpencodeConfig{ScanInterval: strp("2s")}, true)
		if err == nil {
			t.Fatal("an interval below the floor must fail startup")
		}
		if !strings.Contains(err.Error(), opencode.MinScanInterval.String()) {
			t.Errorf("the error must name the floor; got %v", err)
		}
	})
}
