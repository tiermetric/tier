package gates

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoTestTimeoutE2ESummary(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is not on PATH (%v)", err)
	}
	for _, tc := range []struct {
		name, body, diagnostic string
		missing, allowEmpty    bool
		wantCode               int
	}{
		{name: "missing", missing: true, wantCode: 1, diagnostic: "cannot read scripts/e2e-summary.sh"},
		{name: "unanchored", body: "env GOFLAGS= go test ./...\n", wantCode: 1, diagnostic: "zero anchored command matches"},
		{name: "missing timeout", body: "go test ./...\n", wantCode: 1, diagnostic: "go test command lacks -timeout"},
		{name: "two commands", body: "go test -timeout 30m ./...\n(cd x && go test -timeout 30m ./...)\n", diagnostic: "2 anchored go test commands"},
		{name: "empty", wantCode: 1, diagnostic: "expected at least 1 anchored go test command"},
		{name: "comment only", body: "#!/bin/sh\n# go test -timeout 30m ./...\n", wantCode: 1, diagnostic: "expected at least 1 anchored go test command"},
		{name: "stub without exemption", body: "#!/bin/sh\nprintf 'ran %s\\n' \"$*\"\n", wantCode: 1, diagnostic: "expected at least 1 anchored go test command"},
		{name: "path fixture stub", body: "#!/bin/sh\nprintf 'ran %s\\n' \"$*\"\n", allowEmpty: true, diagnostic: "0 anchored go test commands"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if err := os.WriteFile(filepath.Join(dir, "scripts", "e2e-summary.sh"), []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "go-test-timeout-selftest.sh"), "30m")
			cmd.Dir, cmd.Env = dir, baseEnv(dir)
			allowEmpty := "0"
			if tc.allowEmpty {
				allowEmpty = "1"
			}
			cmd.Env = append(cmd.Env, "GO_TEST_TIMEOUT_ALLOW_EMPTY_E2E_FIXTURE="+allowEmpty)
			cmd.Stdin = strings.NewReader("go test -race -count=1 -timeout 30m ./...\n")
			out, code := runCmd(t, cmd)
			if code != tc.wantCode || !strings.Contains(out, tc.diagnostic) {
				t.Fatalf("rc=%d want %d, diagnostic %q:\n%s", code, tc.wantCode, tc.diagnostic, out)
			}
		})
	}
}
