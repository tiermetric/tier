package gates

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVet386_IsPhony(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if names, ok := strings.CutPrefix(line, ".PHONY:"); ok {
			for _, name := range strings.Fields(names) {
				if name == "vet-386" {
					return
				}
			}
		}
	}
	t.Fatal("vet-386 must be declared .PHONY")
}

func TestVet386_VetsDefaultAndIntegration(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s %s %s\\n' \"$GOOS\" \"$GOARCH\" \"$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(repoRoot(t), "scripts", "vet-386.sh"))
	cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vet-386.sh: %v\n%s", err, out)
	}
	want := "linux 386 vet ./...\nlinux 386 vet -tags integration ./...\n"
	if string(out) != want {
		t.Fatalf("vet invocations = %q, want %q", out, want)
	}
}

// Drive check through the real Makefile, not just the standalone script: losing
// the prerequisite or ignoring the recipe's error must break this test.
func TestVet386_MakeCheckWiring(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is not on PATH (%v)", err)
	}
	const defaultVet = "linux 386 vet ./...\n"
	const integrationVet = "linux 386 vet -tags integration ./...\n"
	for _, tc := range []struct {
		name, fail, wantLog string
		wantCode            int
	}{
		{name: "both tag sets", wantLog: defaultVet + integrationVet},
		{name: "default failure", fail: "vet ./...", wantLog: defaultVet, wantCode: 2},
		{name: "integration failure", fail: "vet -tags integration ./...", wantLog: defaultVet + integrationVet, wantCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, subdir := range []string{"scripts", "stub-bin"} {
				if err := os.Mkdir(filepath.Join(dir, subdir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"Makefile", "scripts/vet-386.sh"} {
				src := filepath.Join(repoRoot(t), name)
				info, err := os.Stat(src)
				if err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(src)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), body, info.Mode().Perm()); err != nil {
					t.Fatal(err)
				}
			}
			// Override only unrelated recipes in a second makefile. Preserve the
			// real check dependencies and vet-386 recipe; never recurse into tests
			// or run real lint/build/docs work. No private fragment is installed.
			fixture := "expect-head lint build test docs-test docs-html docs-html-check cve-rescan-selftest e2e-summary-selftest go-test-timeout-selftest homebrew-formula-selftest: ; @:\n"
			if err := os.WriteFile(filepath.Join(dir, "fixture.mk"), []byte(fixture), 0o644); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/sh
printf '%s %s %s\n' "$GOOS" "$GOARCH" "$*" >> "$VET386_LOG"
if [ "$*" = "$VET386_FAIL" ]; then
    echo "vet-386 fixture failure: $*" >&2
    exit 42
fi
`
			for tool, body := range map[string]string{"go": stub, "git": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(dir, "stub-bin", tool), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(dir, "vet-386.log")
			if err := os.WriteFile(log, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("make", "-f", "Makefile", "-f", "fixture.mk", "check")
			cmd.Dir = dir
			cmd.Env = append(baseEnv(dir), "MAKEFLAGS=", "MAKELEVEL=", "MFLAGS=",
				"PATH="+filepath.Join(dir, "stub-bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
				"VET386_LOG="+log, "VET386_FAIL="+tc.fail)
			out, code := runCmd(t, cmd)
			invocations, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if string(invocations) != tc.wantLog {
				t.Fatalf("make check vet-386 invocations = %q, want %q; rc=%d\n%s", invocations, tc.wantLog, code, out)
			}
			if code != tc.wantCode {
				t.Fatalf("make check rc=%d, want %d (vet-386 failure must propagate)\n%s", code, tc.wantCode, out)
			}
			if tc.fail != "" && !strings.Contains(out, "vet-386 fixture failure: "+tc.fail) {
				t.Fatalf("make check failed without reaching the injected vet-386 failure:\n%s", out)
			}
		})
	}
}
