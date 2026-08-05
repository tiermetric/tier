package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/collector/openaiusage"
	"github.com/tiermetric/tier/internal/config"
)

// TestServe_TrustedProxyCIDRValidation exercises the fail-loud parse of the
// --trusted-proxy-cidr list (#131) via the extracted parseTrustedProxies
// helper. A valid list parses to prefixes; a bare IP (the most common operator
// mistake) is rejected with an error naming the flag and suggesting a prefix.
func TestServe_TrustedProxyCIDRValidation(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := parseTrustedProxies([]string{"10.0.0.0/8", "2001:db8::/32", "fe80::/10"})
		if err != nil {
			t.Fatalf("parseTrustedProxies: unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("parseTrustedProxies len = %d, want 3", len(got))
		}
	})
	t.Run("empty_is_nil_no_error", func(t *testing.T) {
		got, err := parseTrustedProxies(nil)
		if err != nil {
			t.Fatalf("parseTrustedProxies(nil): unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("parseTrustedProxies(nil) = %v, want nil", got)
		}
	})
	t.Run("bare_ip_rejected", func(t *testing.T) {
		_, err := parseTrustedProxies([]string{"10.0.0.5"})
		if err == nil {
			t.Fatal("expected error for a bare IP without a prefix length")
		}
		if !strings.Contains(err.Error(), "trusted-proxy-cidr") {
			t.Errorf("error should name the flag; got %v", err)
		}
		// The message must guide the operator to the fix (/32 or /128).
		if !strings.Contains(err.Error(), "/32") || !strings.Contains(err.Error(), "/128") {
			t.Errorf("error should suggest /32 or /128; got %v", err)
		}
	})
	t.Run("garbage_rejected", func(t *testing.T) {
		if _, err := parseTrustedProxies([]string{"not-a-cidr"}); err == nil {
			t.Fatal("expected error for a malformed CIDR")
		}
	})
	t.Run("ipv4_mapped_ipv6_prefix_rejected", func(t *testing.T) {
		// Unmap()'d peers/hops never match a v4-mapped-v6 prefix, so accepting
		// one would be a silently-never-trusted proxy — reject it fail-loud.
		_, err := parseTrustedProxies([]string{"::ffff:10.0.0.0/104"})
		if err == nil {
			t.Fatal("expected error for an IPv4-mapped IPv6 CIDR")
		}
		if !strings.Contains(err.Error(), "native IPv4 form") {
			t.Errorf("error should point at the native IPv4 form; got %v", err)
		}
	})
}

// TestVersionString pins the user-facing format contract (#66): the "tierd
// <version> " prefix, the slash-joined GOOS/GOARCH, and the Go toolchain — not
// just substring presence, so a reorder or a dropped "/" separator fails.
func TestVersionString(t *testing.T) {
	got := versionString()
	if !strings.HasPrefix(got, "tierd "+version+" ") {
		t.Errorf("versionString() = %q, want 'tierd %s ' prefix", got, version)
	}
	if !strings.Contains(got, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("versionString() = %q, want slash-joined %q", got, runtime.GOOS+"/"+runtime.GOARCH)
	}
	if !strings.Contains(got, runtime.Version()) {
		t.Errorf("versionString() = %q, missing Go toolchain %q", got, runtime.Version())
	}
}

// TestDispatch_Version covers the actual #66 feature: all three aliases route to
// versionString(), print to stdout (not stderr), and exit 0.
func TestDispatch_Version(t *testing.T) {
	// "-version" (single dash, full word) is the form Steve typed and got
	// "unknown command" for (#477); it must work alongside the others.
	for _, arg := range []string{"version", "--version", "-version", "-v"} {
		var out, errOut bytes.Buffer
		code := dispatch([]string{arg}, &out, &errOut)
		if code != 0 {
			t.Errorf("dispatch(%q) exit = %d, want 0", arg, code)
		}
		if got := strings.TrimSpace(out.String()); got != versionString() {
			t.Errorf("dispatch(%q) stdout = %q, want %q", arg, got, versionString())
		}
		if errOut.Len() != 0 {
			t.Errorf("dispatch(%q) stderr = %q, want empty", arg, errOut.String())
		}
	}
}

// TestCodexWatchCheck pins the #479 fail-fast matrix: codex enabled with no
// watched repo aborts a normal serve, warns under --read-only, and is silent
// whenever there is a repo to attribute to or codex is off.
func TestCodexWatchCheck(t *testing.T) {
	cases := []struct {
		name      string
		codex     bool
		repos     int
		readOnly  bool
		wantWarn  bool
		wantFatal bool
	}{
		{"off entirely", false, 0, false, false, false},
		{"off, read-only", false, 0, true, false, false},
		{"on with a repo", true, 1, false, false, false},
		{"on, no repo, normal -> FATAL", true, 0, false, false, true},
		{"on, no repo, read-only -> WARN", true, 0, true, true, false},
		{"on with a repo, read-only", true, 2, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, fatal := codexWatchCheck(tc.codex, tc.repos, tc.readOnly)
			if (fatal != "") != tc.wantFatal {
				t.Errorf("fatal = %q, wantFatal = %v", fatal, tc.wantFatal)
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn = %q, wantWarn = %v", warn, tc.wantWarn)
			}
		})
	}
}

// TestCodexProxyDoubleCountWarn pins the one configuration in which Codex spend
// is captured twice (#459 task 2): the OpenAI proxy mounted AND the rollout
// collector enabled. Both docs/how-it-works.md §3b and the parseOpenAIResponses
// doc claim tierd says so at startup — this is the test that keeps that claim
// true. The warning must NOT fire for either half alone, or it becomes noise
// that operators learn to ignore.
func TestCodexProxyDoubleCountWarn(t *testing.T) {
	cases := []struct {
		name           string
		codex, openAI  bool
		wantWarn       bool
		wantMentions   []string
		forbidMentions []string
	}{
		{name: "neither", codex: false, openAI: false, wantWarn: false},
		{name: "proxy only (ordinary OpenAI traffic)", codex: false, openAI: true, wantWarn: false},
		{name: "collector only (the supported Codex path)", codex: true, openAI: false, wantWarn: false},
		{
			name: "both -> WARN", codex: true, openAI: true, wantWarn: true,
			// The message must name the hazard, the reason the rows cannot
			// dedup, and the way out — a warning that only says "careful" is
			// not actionable.
			wantMentions: []string{"TWICE", "dedup", "--codex-rollout"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexProxyDoubleCountWarn(tc.codex, tc.openAI)
			if (got != "") != tc.wantWarn {
				t.Fatalf("warn = %q, wantWarn = %v", got, tc.wantWarn)
			}
			for _, want := range tc.wantMentions {
				if !strings.Contains(got, want) {
					t.Errorf("warning must mention %q; got: %s", want, got)
				}
			}
		})
	}
}

// TestResolveVersion covers the go-install fallback (#477): a stamped ldflag
// wins; the "dev" fallback defers to the build-info module version when the
// toolchain embedded one.
func TestResolveVersion(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v9.9.9"
	if got := resolveVersion(); got != "v9.9.9" {
		t.Errorf("stamped version: resolveVersion() = %q, want v9.9.9", got)
	}

	// With version unstamped, resolveVersion never returns empty — it is either
	// the build-info module version (under `go test` this is typically "(devel)"
	// or empty, in which case it falls back to "dev") or "dev". It must never
	// panic and never return "".
	version = "dev"
	if got := resolveVersion(); got == "" {
		t.Error("unstamped version: resolveVersion() = empty, want non-empty")
	}
}

// TestDispatch_NoArgsAndUnknown covers the two failure paths: no args and an
// unknown command both exit 1 and write to stderr, leaving stdout clean.
func TestDispatch_NoArgsAndUnknown(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		errSub string
	}{
		{"no args", nil, "usage: tierd"},
		{"unknown command", []string{"bogus"}, "unknown command: bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := dispatch(tc.args, &out, &errOut); code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(errOut.String(), tc.errSub) {
				t.Errorf("stderr = %q, want substring %q", errOut.String(), tc.errSub)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty (diagnostics go to stderr)", out.String())
			}
		})
	}
}

// TestDispatch_Help covers #378: an explicit help request via help/--help/-h
// prints the usage listing to STDOUT (not stderr) and exits 0. The first
// command a new user types must read as success, not "unknown command".
func TestDispatch_Help(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-help", "-h"} {
		var out, errOut bytes.Buffer
		code := dispatch([]string{arg}, &out, &errOut)
		if code != 0 {
			t.Errorf("dispatch(%q) exit = %d, want 0", arg, code)
		}
		if !strings.Contains(out.String(), "usage: tierd") {
			t.Errorf("dispatch(%q) stdout = %q, want usage text", arg, out.String())
		}
		// Every subcommand must be discoverable from the help output.
		for _, cmd := range []string{"score", "score-log", "serve", "ship", "backfill", "backup", "doctor", "reprice", "repair-repo", "version", "help"} {
			if !strings.Contains(out.String(), cmd) {
				t.Errorf("dispatch(%q) stdout missing subcommand %q", arg, cmd)
			}
		}
		if errOut.Len() != 0 {
			t.Errorf("dispatch(%q) stderr = %q, want empty (help is success, not an error)", arg, errOut.String())
		}
	}
}

// TestApplyStringFromConfig_Precedence exercises the documented precedence:
// CLI > env > config > builtin default. The helper is the single point of
// truth for that rule; without this test the env > config inversion bug
// caught in #29 review would have shipped silently.
func TestApplyStringFromConfig_Precedence(t *testing.T) {
	ptr := func(s string) *string { return &s }

	cases := []struct {
		name        string
		flagDefault string
		cliSet      bool
		envSet      bool
		cfgValue    *string
		wantValue   string
	}{
		{
			name:        "no_overrides: builtin default wins",
			flagDefault: "default-value",
			cliSet:      false,
			envSet:      false,
			cfgValue:    nil,
			wantValue:   "default-value",
		},
		{
			name:        "config_only: config wins over default",
			flagDefault: "default-value",
			cliSet:      false,
			envSet:      false,
			cfgValue:    ptr("config-value"),
			wantValue:   "config-value",
		},
		{
			name:        "env_set_blocks_config: env wins over config",
			flagDefault: "env-value", // env-var defaults are wired as flag defaults
			cliSet:      false,
			envSet:      true,
			cfgValue:    ptr("config-value"),
			wantValue:   "env-value",
		},
		{
			name:        "cli_set_blocks_config",
			flagDefault: "cli-value",
			cliSet:      true,
			envSet:      false,
			cfgValue:    ptr("config-value"),
			wantValue:   "cli-value",
		},
		{
			name:        "cli_set_blocks_env_and_config",
			flagDefault: "cli-value",
			cliSet:      true,
			envSet:      true,
			cfgValue:    ptr("config-value"),
			wantValue:   "cli-value",
		},
		{
			name:        "config_can_be_empty_string",
			flagDefault: "default-value",
			cliSet:      false,
			envSet:      false,
			cfgValue:    ptr(""),
			wantValue:   "", // explicit empty wins over builtin default
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			val := fs.String("addr", tc.flagDefault, "")
			setFlags := map[string]bool{}
			if tc.cliSet {
				setFlags["addr"] = true
			}
			envVarSet := map[string]bool{}
			if tc.envSet {
				envVarSet["addr"] = true
			}
			applyStringFromConfig(fs, setFlags, envVarSet, "addr", tc.cfgValue)
			if *val != tc.wantValue {
				t.Errorf("got %q, want %q", *val, tc.wantValue)
			}
		})
	}
}

// TestApplyBoolFromConfig_Precedence pins the #196 push-capture flag's
// CLI > env > config > builtin-default precedence, mirroring the string case.
func TestApplyBoolFromConfig_Precedence(t *testing.T) {
	ptr := func(b bool) *bool { return &b }
	cases := []struct {
		name        string
		flagDefault bool
		cliSet      bool
		envSet      bool
		cfgValue    *bool
		want        bool
	}{
		{"no overrides: builtin default (off) wins", false, false, false, nil, false},
		{"config only: config true wins over default", false, false, false, ptr(true), true},
		{"config only: config false wins over default", true, false, false, ptr(false), false},
		{"env set blocks config", true, false, true, ptr(false), true}, // env wired as flag default
		{"cli set blocks config", true, true, false, ptr(false), true},
		{"cli set blocks env and config", true, true, true, ptr(false), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			val := fs.Bool("push-capture", tc.flagDefault, "")
			setFlags := map[string]bool{}
			if tc.cliSet {
				setFlags["push-capture"] = true
			}
			envVarSet := map[string]bool{}
			if tc.envSet {
				envVarSet["push-capture"] = true
			}
			applyBoolFromConfig(fs, setFlags, envVarSet, "push-capture", tc.cfgValue)
			if *val != tc.want {
				t.Errorf("got %v, want %v", *val, tc.want)
			}
		})
	}
}

// TestApplyIntFromConfig pins the int-typed config helper (#85 auth-max-failures,
// reused for zero-outcome-window-days / k-anonymity) across its precedence: nil is a
// no-op leaving the flag default; a config value renders via strconv.Itoa through
// fs.Set; and both a CLI-set and an env-set flag block the config value.
func TestApplyIntFromConfig(t *testing.T) {
	ptr := func(i int) *int { return &i }
	cases := []struct {
		name        string
		flagDefault int
		cliSet      bool
		envSet      bool
		cfgValue    *int
		want        int
	}{
		{"nil config is a no-op: builtin default wins", 10, false, false, nil, 10},
		{"config only: config wins over default", 10, false, false, ptr(5), 5},
		{"config zero is applied (explicit disable)", 10, false, false, ptr(0), 0},
		{"env set blocks config", 10, false, true, ptr(5), 10}, // env wired as flag default
		{"cli set blocks config", 10, true, false, ptr(5), 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			val := fs.Int("auth-max-failures", tc.flagDefault, "")
			setFlags := map[string]bool{}
			if tc.cliSet {
				setFlags["auth-max-failures"] = true
			}
			envVarSet := map[string]bool{}
			if tc.envSet {
				envVarSet["auth-max-failures"] = true
			}
			applyIntFromConfig(fs, setFlags, envVarSet, "auth-max-failures", tc.cfgValue)
			if *val != tc.want {
				t.Errorf("got %d, want %d", *val, tc.want)
			}
		})
	}
}

// TestApplyAuthFromConfig_Precedence exercises the #85 duration knobs
// (--auth-failure-window, --auth-lockout) through applyDurationFromConfig across the
// documented CLI > env > config > builtin-default precedence. Durations flow in as
// strings and are asserted via the flag's own String() form (the value.String() of a
// flag.Duration), so a config-sourced "30s" and the builtin default 60s are compared
// on the same resolved footing. The auth flags have NO env var, but the env dimension
// is still exercised to prove the helper honours it uniformly.
func TestApplyAuthFromConfig_Precedence(t *testing.T) {
	ptr := func(s string) *string { return &s }
	cases := []struct {
		name        string
		flagName    string
		flagDefault time.Duration
		cliSet      bool
		envSet      bool
		cfgValue    *string
		want        string // flag.Duration.String() of the resolved value
	}{
		{"failure_window: no overrides -> builtin default", "auth-failure-window", 60 * time.Second, false, false, nil, "1m0s"},
		{"failure_window: config only -> config wins", "auth-failure-window", 60 * time.Second, false, false, ptr("30s"), "30s"},
		{"failure_window: cli set blocks config", "auth-failure-window", 60 * time.Second, true, false, ptr("30s"), "1m0s"},
		{"failure_window: env set blocks config", "auth-failure-window", 60 * time.Second, false, true, ptr("30s"), "1m0s"},
		{"lockout: no overrides -> builtin default", "auth-lockout", 15 * time.Minute, false, false, nil, "15m0s"},
		{"lockout: config only -> config wins", "auth-lockout", 15 * time.Minute, false, false, ptr("10m"), "10m0s"},
		{"lockout: cli set blocks config", "auth-lockout", 15 * time.Minute, true, false, ptr("10m"), "15m0s"},
		{"lockout: env set blocks config", "auth-lockout", 15 * time.Minute, false, true, ptr("10m"), "15m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.Duration(tc.flagName, tc.flagDefault, "")
			setFlags := map[string]bool{}
			if tc.cliSet {
				setFlags[tc.flagName] = true
			}
			envVarSet := map[string]bool{}
			if tc.envSet {
				envVarSet[tc.flagName] = true
			}
			applyDurationFromConfig(fs, setFlags, envVarSet, tc.flagName, tc.cfgValue)
			if got := fs.Lookup(tc.flagName).Value.String(); got != tc.want {
				t.Errorf("%s resolved to %q, want %q", tc.flagName, got, tc.want)
			}
		})
	}
}

// TestParseConfigDuration covers the #85 duration validator that backs
// applyDurationFromConfig's fail-loud path: a valid Go duration string parses; a
// malformed one (failure_window: "banana") returns an error naming the config key and
// the offending value — the same time.ParseDuration the CLI flag uses, so config and
// flag reject identically. This is the unit-testable core of the "bad duration fails
// startup" behavior (the helper os.Exits on this error at the call site).
func TestParseConfigDuration(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		d, err := parseConfigDuration("auth-lockout", "15m")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d != 15*time.Minute {
			t.Errorf("got %v, want 15m", d)
		}
	})
	t.Run("zero_is_valid_not_rejected_here", func(t *testing.T) {
		// 0s must parse cleanly — the >0 coherence check is validateAuthLockout's job
		// on the resolved values, not this parser's, so a config {max_failures:5,
		// lockout:"0s"} reaches and trips that later guard.
		if _, err := parseConfigDuration("auth-lockout", "0s"); err != nil {
			t.Errorf("0s should parse; got %v", err)
		}
	})
	t.Run("malformed_names_flag_and_value", func(t *testing.T) {
		_, err := parseConfigDuration("auth-failure-window", "banana")
		if err == nil {
			t.Fatal("expected error for a malformed duration")
		}
		if !strings.Contains(err.Error(), "auth-failure-window") {
			t.Errorf("error should name the config key; got %v", err)
		}
		if !strings.Contains(err.Error(), "banana") {
			t.Errorf("error should quote the offending value; got %v", err)
		}
	})
}

// TestValidateAuthLockout_AppliesToConfigSourcedValues pins the #36 coherence rule as
// enforced on RESOLVED values (#85): a non-zero max_failures with a non-positive
// window or lockout is refused. The final sub-test drives the values END-TO-END
// through the config-apply helpers into a flagset — proving a config-only combo
// (max_failures:5 + lockout:"0s") reaches the same validation a flag-sourced combo
// would, so YAML cannot smuggle a disabled lockout past the guard.
func TestValidateAuthLockout_AppliesToConfigSourcedValues(t *testing.T) {
	t.Run("predicate", func(t *testing.T) {
		cases := []struct {
			name        string
			maxFailures int
			window      time.Duration
			lockout     time.Duration
			wantErr     bool
		}{
			{"disabled: max_failures 0 permits any durations", 0, 0, 0, false},
			{"armed: positive window and lockout ok", 5, 60 * time.Second, 15 * time.Minute, false},
			{"armed with zero lockout is refused", 5, 60 * time.Second, 0, true},
			{"armed with zero window is refused", 5, 0, 15 * time.Minute, true},
			{"armed with negative lockout is refused", 5, 60 * time.Second, -1, true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := validateAuthLockout(tc.maxFailures, tc.window, tc.lockout)
				if (err != nil) != tc.wantErr {
					t.Errorf("validateAuthLockout(%d,%v,%v) err=%v, wantErr=%v",
						tc.maxFailures, tc.window, tc.lockout, err, tc.wantErr)
				}
			})
		}
	})
	t.Run("config_sourced_combo_trips_validation", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		maxF := fs.Int("auth-max-failures", 10, "")
		win := fs.Duration("auth-failure-window", 60*time.Second, "")
		lock := fs.Duration("auth-lockout", 15*time.Minute, "")
		// Config supplies both an armed max_failures and a lockout of 0 — nothing on
		// the CLI, no env layer.
		setFlags := map[string]bool{}
		envVarSet := map[string]bool{}
		five, zero := 5, "0s"
		applyIntFromConfig(fs, setFlags, envVarSet, "auth-max-failures", &five)
		applyDurationFromConfig(fs, setFlags, envVarSet, "auth-lockout", &zero)
		if err := validateAuthLockout(*maxF, *win, *lock); err == nil {
			t.Fatal("config-sourced {max_failures:5, lockout:0s} must trip the >0 validation")
		}
	})
}

// TestValidateBind pins the #59 fail-closed rule: tokenless serving is
// loopback-only; any token unlocks any bind. The syntheticDemo column pins the
// #476 exemption: the demo bit alone unlocks a non-loopback bind with no token.
func TestValidateBind(t *testing.T) {
	cases := []struct {
		name          string
		addr          string
		token         string
		syntheticDemo bool
		wantErr       bool
	}{
		{"default loopback, no token", "127.0.0.1:8080", "", false, false},
		{"loopback /8 variant, no token", "127.1.2.3:8080", "", false, false},
		{"ipv6 loopback, no token", "[::1]:8080", "", false, false},
		// Pins non-obvious stdlib behavior: ParseIP normalizes the
		// IPv4-mapped form so IsLoopback sees 127.0.0.1, and the OS binds
		// it to loopback — allowing it is correct.
		{"ipv4-mapped loopback, no token", "[::ffff:127.0.0.1]:8080", "", false, false},
		// Zoned literals are refused in tokenless mode (ParseIP rejects
		// zones; fail-closed) — see the validateBind doc comment.
		{"zoned ipv6 loopback, no token", "[::1%lo0]:8080", "", false, true},
		{"localhost, no token", "localhost:8080", "", false, false},
		{"all interfaces (empty host), no token", ":8080", "", false, true},
		{"all interfaces (0.0.0.0), no token", "0.0.0.0:8080", "", false, true},
		{"all interfaces (ipv6 ::), no token", "[::]:8080", "", false, true},
		{"lan address, no token", "192.168.1.10:8080", "", false, true},
		{"hostname other than localhost, no token", "tier.internal:8080", "", false, true},
		{"missing port, no token", "127.0.0.1", "", false, true},
		{"all interfaces WITH token", ":8080", "s3cret", false, false},
		{"lan address WITH token", "192.168.1.10:8080", "s3cret", false, false},
		// #476: the synthetic-demo bit alone permits a non-loopback bind with no
		// token — the demo dataset is invented and read-only.
		{"all interfaces, demo bit, no token", "0.0.0.0:8080", "", true, false},
		{"lan address, demo bit, no token", "192.168.1.10:8080", "", true, false},
		// Control arm: the SAME non-loopback bind with the bit UNSET (the only
		// state a `serve` invocation can reach) must still be refused. If this ever
		// passes, the exemption has leaked to the serve path.
		{"all interfaces, NO demo bit, no token (control)", "0.0.0.0:8080", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBind(tc.addr, tc.token, tc.syntheticDemo)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateBind(%q, %q, %v) = %v, wantErr %v", tc.addr, tc.token, tc.syntheticDemo, err, tc.wantErr)
			}
		})
	}
}

// TestCheckReadToken covers the #190 distinctness guard: only an equal,
// non-empty (read, api) pair is refused; every other combination is allowed.
func TestCheckReadToken(t *testing.T) {
	cases := []struct {
		name      string
		apiToken  string
		readToken string
		wantErr   bool
	}{
		{"both empty", "", "", false},
		{"api only", "write-tok", "", false},
		{"read only, no api", "", "read-tok", false},
		{"distinct pair", "write-tok", "read-tok", false},
		{"equal non-empty pair -> refused", "same-tok", "same-tok", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReadToken(tc.apiToken, tc.readToken)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkReadToken(%q, %q) = %v, wantErr %v", tc.apiToken, tc.readToken, err, tc.wantErr)
			}
		})
	}
}

// TestResolveSecretFlag covers the #37 `@file` indirection: a literal value
// passes through unchanged, an `@/path` value is read from disk with the
// trailing newline trimmed, and bad paths error rather than silently yielding
// an empty (auth-disabling) secret.
func TestResolveSecretFlag(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "token")
	if err := os.WriteFile(secretFile, []byte("s3cr3t-value\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	crlfFile := filepath.Join(dir, "token_crlf")
	if err := os.WriteFile(crlfFile, []byte("s3cr3t-value\r\n"), 0o600); err != nil {
		t.Fatalf("write crlf file: %v", err)
	}
	noNewlineFile := filepath.Join(dir, "token_bare")
	if err := os.WriteFile(noNewlineFile, []byte("s3cr3t-value"), 0o600); err != nil {
		t.Fatalf("write no-newline file: %v", err)
	}
	multiNewlineFile := filepath.Join(dir, "token_multi")
	if err := os.WriteFile(multiNewlineFile, []byte("s3cr3t-value\n\n"), 0o600); err != nil {
		t.Fatalf("write multi-newline file: %v", err)
	}
	emptyFile := filepath.Join(dir, "token_empty")
	if err := os.WriteFile(emptyFile, []byte("\n"), 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"literal passes through", "s3cr3t-value", "s3cr3t-value", false},
		{"empty literal passes through (auth disabled)", "", "", false},
		{"file read trims trailing newline", "@" + secretFile, "s3cr3t-value", false},
		{"file read trims CRLF", "@" + crlfFile, "s3cr3t-value", false},
		{"file without trailing newline is verbatim", "@" + noNewlineFile, "s3cr3t-value", false},
		{"file with multiple trailing newlines is trimmed", "@" + multiNewlineFile, "s3cr3t-value", false},
		{"newline-only file errors (fail closed, not empty token)", "@" + emptyFile, "", true},
		{"missing file errors", "@" + filepath.Join(dir, "nope"), "", true},
		{"directory errors (not a regular file)", "@" + dir, "", true},
		{"bare @ errors", "@", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSecretFlag(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveSecretFlag(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("resolveSecretFlag(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			// On error the returned secret must be empty, never a partial/half
			// secret that could silently weaken auth.
			if tc.wantErr && got != "" {
				t.Errorf("resolveSecretFlag(%q) returned %q on error, want empty", tc.raw, got)
			}
		})
	}
}

// TestResolveOpenAIUsageConfig covers the #139 poller config resolution: absent
// block → disabled (nil, nil); a present block requires api_key + org; the
// poll_interval floor is enforced; and a resolved key is returned for a valid
// block. Mirrors the anthropic_admin resolution contract.
func TestResolveOpenAIUsageConfig(t *testing.T) {
	strp := func(s string) *string { return &s }

	t.Run("absent_block_disabled", func(t *testing.T) {
		got, err := resolveOpenAIUsageConfig(nil)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != nil {
			t.Errorf("settings = %+v, want nil (disabled)", got)
		}
	})

	t.Run("missing_key_errors", func(t *testing.T) {
		_, err := resolveOpenAIUsageConfig(&config.OpenAIUsageConfig{Org: strp("acme")})
		if err == nil || !strings.Contains(err.Error(), "api_key is required") {
			t.Errorf("err = %v, want an api_key-required error", err)
		}
	})

	t.Run("missing_org_errors", func(t *testing.T) {
		_, err := resolveOpenAIUsageConfig(&config.OpenAIUsageConfig{APIKey: strp("sk-openai-x")})
		if err == nil || !strings.Contains(err.Error(), "org is required") {
			t.Errorf("err = %v, want an org-required error", err)
		}
	})

	t.Run("interval_below_floor_errors", func(t *testing.T) {
		_, err := resolveOpenAIUsageConfig(&config.OpenAIUsageConfig{
			APIKey: strp("sk-openai-x"), Org: strp("acme"), PollInterval: strp("30s"),
		})
		if err == nil || !strings.Contains(err.Error(), "poll_interval must be >=") {
			t.Errorf("err = %v, want a poll_interval floor error", err)
		}
	})

	t.Run("valid_defaults_interval", func(t *testing.T) {
		got, err := resolveOpenAIUsageConfig(&config.OpenAIUsageConfig{
			APIKey: strp("sk-openai-x"), Org: strp("acme"),
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got == nil || got.org != "acme" || got.apiKey != "sk-openai-x" {
			t.Fatalf("settings = %+v, want acme/sk-openai-x", got)
		}
		if got.interval != openaiusage.DefaultPollInterval {
			t.Errorf("interval = %s, want default %s", got.interval, openaiusage.DefaultPollInterval)
		}
	})
}

// TestResolveCodexRolloutConfig covers the one place the Codex collector's
// enablement rule differs from its two poller siblings: it needs no credential,
// so the --codex-rollout FLAG alone enables it, and the config block exists only
// to override defaults.
func TestResolveCodexRolloutConfig(t *testing.T) {
	strp := func(s string) *string { return &s }

	t.Run("no_flag_no_block_disabled", func(t *testing.T) {
		got, err := resolveCodexRolloutConfig(nil, false)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != nil {
			t.Errorf("settings = %+v, want nil (disabled)", got)
		}
	})

	t.Run("flag_alone_enables_with_defaults", func(t *testing.T) {
		got, err := resolveCodexRolloutConfig(nil, true)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got == nil {
			t.Fatal("settings = nil, want enabled — the flag needs no config block because the collector needs no credential")
		}
		if got.interval != codexrollout.DefaultScanInterval {
			t.Errorf("interval = %s, want default %s", got.interval, codexrollout.DefaultScanInterval)
		}
		if got.sessionsDir != "" {
			t.Errorf("sessionsDir = %q, want empty (the collector's ~/.codex/sessions default)", got.sessionsDir)
		}
	})

	t.Run("block_alone_enables", func(t *testing.T) {
		got, err := resolveCodexRolloutConfig(&config.CodexRolloutConfig{}, false)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got == nil {
			t.Fatal("settings = nil, want enabled by block presence")
		}
	})

	t.Run("overrides_applied", func(t *testing.T) {
		got, err := resolveCodexRolloutConfig(&config.CodexRolloutConfig{
			SessionsDir: strp("/srv/codex/sessions"), ScanInterval: strp("90s"),
		}, false)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got.sessionsDir != "/srv/codex/sessions" || got.interval != 90*time.Second {
			t.Errorf("settings = %+v, want /srv/codex/sessions @ 90s", got)
		}
	})

	t.Run("interval_below_floor_errors_even_with_flag", func(t *testing.T) {
		// A present-but-invalid block must fail loud even when the flag alone
		// would have enabled the collector — silently substituting the default
		// would leave the operator believing their 2s cadence took effect.
		_, err := resolveCodexRolloutConfig(&config.CodexRolloutConfig{ScanInterval: strp("2s")}, true)
		if err == nil || !strings.Contains(err.Error(), "scan_interval must be >=") {
			t.Errorf("err = %v, want a scan_interval floor error", err)
		}
	})

	t.Run("unparseable_interval_errors", func(t *testing.T) {
		_, err := resolveCodexRolloutConfig(&config.CodexRolloutConfig{ScanInterval: strp("soon")}, false)
		if err == nil || !strings.Contains(err.Error(), "scan_interval") {
			t.Errorf("err = %v, want a scan_interval parse error", err)
		}
	})
}

// TestIssueLabel pins the cost-by-issue rendering of the labeled unattributed
// buckets (#refocus, Option B): each family member expands to a human-readable
// string, and a real issue id passes through unchanged.
func TestIssueLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{collector.UnattributedIssueID, "unattributed (unlabeled)"},
		{collector.UnattributedMain, "unattributed: exploratory (main)"},
		{collector.UnattributedDetachedHEAD, "unattributed: detached HEAD"},
		{collector.UnattributedNoIssue, "unattributed: branch, no issue #"},
		{"issue-42", "issue-42"},
		{"TIER-9", "TIER-9"},
	}
	for _, c := range cases {
		if got := issueLabel(c.in); got != c.want {
			t.Errorf("issueLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
