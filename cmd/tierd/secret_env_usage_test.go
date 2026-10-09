package main

// #1004: a secret-bearing flag must never take its DEFAULT from the
// environment, because flag.PrintDefaults prints a non-empty string default
// into --help and parse-error usage. These tests set each secret env var to a
// sentinel, drive every affected command's usage path, and assert the sentinel
// is absent; then pin that the env var still reaches the flag when the flag is
// not given, and that a given flag wins.
//
// ship and serve use flag.ExitOnError (and os.Exit on a bad token), so they run
// in a child: this test binary re-executed with -test.run selecting
// TestHelperProcess_SecretEnv, the os/exec helper-process pattern. It does not
// depend on the integration-tagged TestMain, so it runs under `make check`.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const childArgsEnvVar = "TIERD_1004_CHILD_ARGS"

// secretEnvChildDeadline bounds each child so a regression that lets a child
// run on (e.g. a serve that boots) fails the test instead of hanging it.
const secretEnvChildDeadline = 60 * time.Second

// TestHelperProcess_SecretEnv is not a test: it becomes `tierd <args>` when
// runSecretEnvChild starts this binary as a child.
func TestHelperProcess_SecretEnv(t *testing.T) {
	args := os.Getenv(childArgsEnvVar)
	if args == "" {
		t.Skip("helper process for the #1004 tests; runs only as a child")
	}
	os.Exit(dispatch(strings.Split(args, "\n"), os.Stdout, os.Stderr))
}

// runSecretEnvChild runs `tierd <args...>` in a child whose environment carries
// no TIER_* variable except those in env, and returns its exit code and its
// stdout and stderr together.
func runSecretEnvChild(t *testing.T, env map[string]string, args ...string) (int, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretEnvChildDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestHelperProcess_SecretEnv$")
	cmd.WaitDelay = time.Second
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TIER_") || strings.HasPrefix(e, "TIERD_") {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, childArgsEnvVar+"="+strings.Join(args, "\n"))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("child tierd %v still running after %s, killed; output:\n%s", args, secretEnvChildDeadline, out.String())
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return 0, out.String()
	case errors.As(runErr, &exitErr):
		return exitErr.ExitCode(), out.String()
	}
	t.Fatalf("run child tierd %v: %v (output: %s)", args, runErr, out.String())
	return 0, ""
}

// serveSecretEnv maps each serve secret flag to its env var.
var serveSecretEnv = map[string]string{
	"api-token":      "TIER_API_TOKEN",
	"read-token":     "TIER_READ_TOKEN",
	"metrics-token":  "TIER_METRICS_TOKEN",
	"webhook-secret": "TIER_WEBHOOK_SECRET",
}

// serveSecretConfigKey maps each serve secret flag to its key under the config
// file's http block.
var serveSecretConfigKey = map[string]string{
	"api-token":      "api_token",
	"read-token":     "read_token",
	"metrics-token":  "metrics_token",
	"webhook-secret": "webhook_secret",
}

func sentinelFor(envVar string) string { return "SENTINEL-1004-" + envVar }

// assertUsageOmitsSentinels checks the output is real usage (it names every
// flag in flags, so an empty or unrelated output cannot pass) and carries no
// sentinel.
func assertUsageOmitsSentinels(t *testing.T, label, out string, flags []string, env map[string]string) {
	t.Helper()
	for _, f := range flags {
		if !strings.Contains(out, "-"+f) {
			t.Fatalf("%s: output does not name -%s, so it is not the usage text:\n%s", label, f, out)
		}
	}
	for k, v := range env {
		if strings.Contains(out, v) {
			t.Fatalf("%s: usage printed the value of %s:\n%s", label, k, out)
		}
	}
}

func TestUsageOmitsSecretEnv_Doctor(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", sentinelFor("TIER_API_TOKEN"))
	env := map[string]string{"TIER_API_TOKEN": sentinelFor("TIER_API_TOKEN")}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"doctor --help", []string{"doctor", "--help"}},
		{"doctor unknown flag", []string{"doctor", "--no-such-flag"}},
	} {
		var out, errb bytes.Buffer
		_ = dispatch(tc.args, &out, &errb)
		assertUsageOmitsSentinels(t, tc.name, out.String()+errb.String(), []string{"api-token"}, env)
	}
}

func TestUsageOmitsSecretEnv_ShipAndServe(t *testing.T) {
	env := map[string]string{}
	for _, v := range serveSecretEnv {
		env[v] = sentinelFor(v)
	}
	serveFlags := make([]string, 0, len(serveSecretEnv))
	for f := range serveSecretEnv {
		serveFlags = append(serveFlags, f)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		flags []string
	}{
		{"ship --help", []string{"ship", "--help"}, []string{"api-token"}},
		{"ship unknown flag", []string{"ship", "--no-such-flag"}, []string{"api-token"}},
		{"serve --help", []string{"serve", "--help"}, serveFlags},
		{"serve unknown flag", []string{"serve", "--no-such-flag"}, serveFlags},
	} {
		_, out := runSecretEnvChild(t, env, tc.args...)
		assertUsageOmitsSentinels(t, tc.name, out, tc.flags, env)
	}
}

// The precedence tests use `@<path>` values naming files that do not exist:
// resolveSecretFlag's error quotes the path, so the output says which source
// the command actually used without any server or network.
func missingSecretPaths(t *testing.T) (fromEnv, fromFlag string) {
	dir := t.TempDir()
	return filepath.Join(dir, "missing-from-env"), filepath.Join(dir, "missing-from-flag")
}

func assertSecretSource(t *testing.T, label, out, want, notWant string) {
	t.Helper()
	if !strings.Contains(out, want) || strings.Contains(out, notWant) {
		t.Fatalf("%s: want the output to name %q and not %q:\n%s", label, want, notWant, out)
	}
}

func TestSecretEnvPrecedence_Doctor(t *testing.T) {
	fromEnv, fromFlag := missingSecretPaths(t)
	t.Setenv("TIER_API_TOKEN", "@"+fromEnv)
	base := []string{"doctor", "--repo", t.TempDir(), "--claude-dir", t.TempDir(), "--server", "http://127.0.0.1:1"}
	var out, errb bytes.Buffer
	_ = dispatch(base, &out, &errb)
	assertSecretSource(t, "doctor, flag absent", out.String()+errb.String(), fromEnv, fromFlag)
	out.Reset()
	errb.Reset()
	_ = dispatch(append(base, "--api-token", "@"+fromFlag), &out, &errb)
	assertSecretSource(t, "doctor, flag given", out.String()+errb.String(), fromFlag, fromEnv)
}

func TestSecretEnvPrecedence_Hierarchy(t *testing.T) {
	fromEnv, fromFlag := missingSecretPaths(t)
	t.Setenv("TIER_API_TOKEN", "@"+fromEnv)
	csv := filepath.Join(t.TempDir(), "teams.csv")
	code, _, errOut := runHierarchyForTest("import", "--server", "http://127.0.0.1:1", csv)
	if code != 1 {
		t.Fatalf("hierarchy, flag absent: exit %d, want 1", code)
	}
	assertSecretSource(t, "hierarchy, flag absent", errOut, fromEnv, fromFlag)
	code, _, errOut = runHierarchyForTest("import", "--server", "http://127.0.0.1:1", "--api-token", "@"+fromFlag, csv)
	if code != 1 {
		t.Fatalf("hierarchy, flag given: exit %d, want 1", code)
	}
	assertSecretSource(t, "hierarchy, flag given", errOut, fromFlag, fromEnv)
}

func TestSecretEnvPrecedence_Ship(t *testing.T) {
	fromEnv, fromFlag := missingSecretPaths(t)
	env := map[string]string{"TIER_API_TOKEN": "@" + fromEnv}
	code, out := runSecretEnvChild(t, env, "ship", "--server", "http://127.0.0.1:1")
	if code != 1 {
		t.Fatalf("ship, flag absent: exit %d, want 1:\n%s", code, out)
	}
	assertSecretSource(t, "ship, flag absent", out, fromEnv, fromFlag)
	code, out = runSecretEnvChild(t, env, "ship", "--server", "http://127.0.0.1:1", "--api-token", "@"+fromFlag)
	if code != 1 {
		t.Fatalf("ship, flag given: exit %d, want 1:\n%s", code, out)
	}
	assertSecretSource(t, "ship, flag given", out, fromFlag, fromEnv)
}

func TestSecretEnvPrecedence_Serve(t *testing.T) {
	for flagName, envVar := range serveSecretEnv {
		t.Run(flagName, func(t *testing.T) {
			fromEnv, fromFlag := missingSecretPaths(t)
			env := map[string]string{envVar: "@" + fromEnv}
			// A non-loopback bind: should a fallback regress and leave every
			// token empty, validateBind refuses at once, so the arm fails on
			// its path assertion instead of booting a server.
			base := []string{"serve", "--db", filepath.Join(t.TempDir(), "t.db"),
				"--addr", "0.0.0.0:0", "--aggregation", "developer"}
			code, out := runSecretEnvChild(t, env, base...)
			if code != 1 {
				t.Fatalf("serve, --%s absent: exit %d, want 1:\n%s", flagName, code, out)
			}
			assertSecretSource(t, "serve, --"+flagName+" absent", out, fromEnv, fromFlag)
			code, out = runSecretEnvChild(t, env, append(base, "--"+flagName, "@"+fromFlag)...)
			if code != 1 {
				t.Fatalf("serve, --%s given: exit %d, want 1:\n%s", flagName, code, out)
			}
			assertSecretSource(t, "serve, --"+flagName+" given", out, fromFlag, fromEnv)

			// The config layer: it applies only when both the flag and the
			// env var are absent.
			fromConfig := filepath.Join(filepath.Dir(fromEnv), "missing-from-config")
			cfgPath := filepath.Join(t.TempDir(), "tier.yaml")
			cfgYAML := "http:\n  " + serveSecretConfigKey[flagName] + ": \"@" + fromConfig + "\"\n"
			if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			withConfig := append(append([]string{}, base...), "--config", cfgPath)
			code, out = runSecretEnvChild(t, nil, withConfig...)
			if code != 1 {
				t.Fatalf("serve, --%s and %s absent, config set: exit %d, want 1:\n%s", flagName, envVar, code, out)
			}
			assertSecretSource(t, "serve, config alone", out, fromConfig, fromEnv)
			code, out = runSecretEnvChild(t, env, withConfig...)
			if code != 1 {
				t.Fatalf("serve, --%s absent, %s and config set: exit %d, want 1:\n%s", flagName, envVar, code, out)
			}
			assertSecretSource(t, "serve, env and config", out, fromEnv, fromConfig)
		})
	}
}

// TestSecretEnvFallback pins the helper's precedence, which is the one the
// env-var default had: a flag given on the command line wins even when given
// empty; otherwise the env var's value is used.
func TestSecretEnvFallback(t *testing.T) {
	t.Setenv("TIER_1004_TEST_SECRET", "from-env")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"flag absent", nil, "from-env"},
		{"flag given", []string{"--tok", "from-flag"}, "from-flag"},
		{"flag given empty", []string{"--tok="}, ""},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		p := fs.String("tok", "", "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		secretEnvFallback(fs, p, "tok", "TIER_1004_TEST_SECRET")
		if *p != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, *p, tc.want)
		}
	}
}
