package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const looseSecretMsg = "secret file is accessible to group or other users; restrict it to its owner"

// looseSecretWarns returns the decoded loose-secret-file WARN records in buf's
// JSON log lines.
func looseSecretWarns(t *testing.T, out string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == looseSecretMsg {
			got = append(got, rec)
		}
	}
	return got
}

// writeSecretFile writes secret to a fresh file and chmods it to mode (after
// the write, so the process umask cannot mask the bits under test).
func writeSecretFile(t *testing.T, name, secret string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
	return p
}

// TestResolveSecretFlag_LooseModeWarns pins #920: an @file secret whose mode
// has any group/other bit produces exactly one WARN naming the setting and
// octal mode (never the path or the contents), and the secret still resolves — a loose file
// is warned about, never refused.
func TestResolveSecretFlag_LooseModeWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows; TestSecretFileIsLoose pins the skip")
	}
	const secret = "s3cr3t-920-do-not-log"
	cases := []struct {
		name     string
		mode     os.FileMode
		wantWarn bool
		wantMode string
	}{
		{"owner-only 0600 is silent", 0o600, false, ""},
		{"world-readable 0644 warns", 0o644, true, "0644"},
		{"read-only secret mount 0444 warns", 0o444, true, "0444"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := withDefaultLoggerAt(t, "info")
			p := writeSecretFile(t, "token", secret, tc.mode)

			got, err := resolveSecretFlag("--api-token", "@"+p)
			if err != nil {
				t.Fatalf("resolveSecretFlag: %v (a loose mode must warn, never refuse)", err)
			}
			if got != secret {
				t.Fatalf("resolved %q, want %q", got, secret)
			}

			out := buf.String()
			if strings.Contains(out, secret) {
				t.Fatalf("log output contains the secret contents:\n%s", out)
			}
			warns := looseSecretWarns(t, out)
			if !tc.wantWarn {
				if len(warns) != 0 {
					t.Fatalf("got %d loose-secret WARNs for mode %04o, want 0:\n%s", len(warns), uint32(tc.mode), out)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("got %d loose-secret WARNs, want 1:\n%s", len(warns), out)
			}
			w := warns[0]
			if w["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", w["level"])
			}
			if w["setting"] != "--api-token" {
				t.Errorf("setting = %v, want --api-token", w["setting"])
			}
			// The WARN never logs the file path: the path comes from a secret
			// flag or api_key config value, so it is kept out of the log
			// entirely (CodeQL go/clear-text-logging). setting names the file.
			if v, ok := w["path"]; ok {
				t.Errorf("WARN carries a path attribute %v, want none", v)
			}
			if strings.Contains(out, p) {
				t.Errorf("log output contains the secret file path %q:\n%s", p, out)
			}
			if w["mode"] != tc.wantMode {
				t.Errorf("mode = %v, want %s", w["mode"], tc.wantMode)
			}
			// The remedy is fixed text: it never embeds the operator-controlled
			// path as a paste-ready command.
			if remedy, _ := w["remedy"].(string); remedy == "" || strings.Contains(remedy, p) {
				t.Errorf("remedy = %q, want non-empty text that does not contain the path %q", remedy, p)
			}
		})
	}
}

// TestResolveSecretFlag_LooseModeWarnsOncePerPath pins the per-process dedupe:
// two settings resolving the same loose file produce one WARN, not two.
func TestResolveSecretFlag_LooseModeWarnsOncePerPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	buf := withDefaultLoggerAt(t, "info")
	p := writeSecretFile(t, "shared", "s3cr3t-shared", 0o644)
	for _, setting := range []string{"collectors.anthropic_admin.api_key", "collectors.openai_usage.api_key"} {
		if _, err := resolveSecretFlag(setting, "@"+p); err != nil {
			t.Fatalf("resolveSecretFlag(%s): %v", setting, err)
		}
	}
	if n := len(looseSecretWarns(t, buf.String())); n != 1 {
		t.Fatalf("got %d loose-secret WARNs for one path resolved twice, want 1:\n%s", n, buf.String())
	}
}

// TestSecretFileIsLoose pins the predicate, including the Windows skip, which
// cannot be exercised end to end off Windows.
func TestSecretFileIsLoose(t *testing.T) {
	cases := []struct {
		goos string
		perm os.FileMode
		want bool
	}{
		{"linux", 0o600, false},
		{"linux", 0o400, false},
		{"linux", 0o700, false},
		{"linux", 0o640, true},
		{"linux", 0o604, true},
		{"linux", 0o644, true},
		{"darwin", 0o444, true},
		{"linux", 0o620, true},
		{"windows", 0o644, false},
		{"windows", 0o666, false},
	}
	for _, tc := range cases {
		if got := secretFileIsLoose(tc.goos, tc.perm); got != tc.want {
			t.Errorf("secretFileIsLoose(%q, %04o) = %v, want %v", tc.goos, uint32(tc.perm), got, tc.want)
		}
	}
}
