package gates

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestMakePathsWithSpaces(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is not on PATH (%v)", err)
	}
	dir := filepath.Join(t.TempDir(), "clone with spaces")
	installGuard(t, dir)
	link := filepath.Join(t.TempDir(), "linked clone with spaces")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	dir = link
	// Make's CURDIR resolves symlinks, including macOS's /var -> /private/var.
	physicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"e2e-summary.sh", "image-cve-rescan.sh", "seam-exercise.sh", "vet-386.sh"} {
		stub := "#!/bin/sh\nprintf 'ran %s\\npath <%s>\\n' \"$*\" \"$0\"\n"
		if err := os.WriteFile(filepath.Join(dir, "scripts", script), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The real script, not a stub: go-test-timeout-selftest's control arm needs it to exit 1 on a go test line without -timeout.
	selftest, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "go-test-timeout-selftest.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "go-test-timeout-selftest.sh"), selftest, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, subdir := range []string{"tools/docgen", "stub-bin"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"go", "git", "golangci-lint", "govulncheck"} {
		stub := "#!/bin/sh\nexit 0\n"
		if tool == "golangci-lint" {
			stub = "#!/bin/sh\nprintf 'cache <%s>\\n' \"$GOLANGCI_LINT_CACHE\"\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "stub-bin", tool), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, target := range []string{"expect-head", "e2e-summary", "e2e-summary-selftest", "cve-rescan", "cve-rescan-selftest", "lint", "seam-exercise", "check-full"} {
		t.Run(target, func(t *testing.T) {
			cmd := exec.Command("make", target)
			cmd.Dir = dir
			// This path fixture uses an e2e-summary stub with no go test commands.
			cmd.Env = append(baseEnv(dir), "MAKEFLAGS=", "MAKELEVEL=", "MFLAGS=", "GO_TEST_TIMEOUT_ALLOW_EMPTY_E2E_FIXTURE=1", "PATH="+filepath.Join(dir, "stub-bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			dry := exec.Command("make", "-n", target)
			dry.Dir, dry.Env = cmd.Dir, cmd.Env
			planned, code := runCmd(t, dry)
			if code != 0 {
				t.Fatalf("make -n %s: rc=%d\n%s", target, code, planned)
			}
			if err := checkMakeScripts(dir, planned); err != nil {
				t.Fatal(err)
			}
			out, code := runCmd(t, cmd)
			if code != 0 {
				t.Fatalf("make %s in path with spaces: rc=%d\n%s", target, code, out)
			}
			if target != "expect-head" && target != "lint" && !strings.Contains(out, "ran ") {
				t.Fatalf("make %s did not run its script:\n%s", target, out)
			}
			scripts := map[string][]string{
				"seam-exercise": {"seam-exercise.sh"},
				"check-full":    {"image-cve-rescan.sh", "e2e-summary.sh", "seam-exercise.sh"},
			}
			for _, script := range scripts[target] {
				if !strings.Contains(out, "path <"+filepath.Join(physicalDir, "scripts", script)+">") &&
					!strings.Contains(out, "path <./scripts/"+script+">") {
					t.Fatalf("make %s lost script path %s:\n%s", target, script, out)
				}
			}
			if (target == "lint" || target == "check-full") && !strings.Contains(out, "cache <"+filepath.Join(physicalDir, "bin", ".golangci-lint-cache")+">") {
				t.Fatalf("make %s lost lint cache path:\n%s", target, out)
			}

			if strings.HasSuffix(target, "-selftest") && !strings.Contains(out, "ran --selftest") {
				t.Fatalf("make %s did not pass --selftest:\n%s", target, out)
			}
		})
	}
}

// Keep the stub list explicit, but diagnose recipe drift before running make.
func checkMakeScripts(dir, planned string) error {
	for _, match := range regexp.MustCompile(`scripts/([a-zA-Z0-9_-]+\.sh)`).FindAllStringSubmatch(planned, -1) {
		if _, err := os.Stat(filepath.Join(dir, "scripts", match[1])); err != nil {
			return fmt.Errorf("TestMakePathsWithSpaces: missing fixture script scripts/%s; update the explicit fixture list: %w", match[1], err)
		}
	}
	return nil
}

func TestMakePathsWithSpaces_MissingScriptDiagnostic(t *testing.T) {
	dir := t.TempDir()
	planned := `"/clone with spaces/scripts/new-required-script.sh" --selftest`
	err := checkMakeScripts(dir, planned)
	if err == nil || !strings.Contains(err.Error(), "missing fixture script scripts/new-required-script.sh") {
		t.Fatalf("missing script diagnostic = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "new-required-script.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMakeScripts(dir, planned); err != nil {
		t.Fatalf("present script rejected: %v", err)
	}
}

func TestTierdServiceInstallPath(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is not on PATH (%v)", err)
	}
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "tierd.service"))
	if err != nil {
		t.Fatal(err)
	}
	var servicePath, installSource, installDestination string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			fields := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
			if len(fields) > 0 {
				servicePath = fields[0]
			}
		}
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if len(fields) == 6 && strings.Join(fields[:4], " ") == "sudo install -m 0755" {
			installSource, installDestination = fields[4], fields[5]
		}
	}
	cmd := exec.Command("make", "-n", "build")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(baseEnv(cmd.Dir), "MAKEFLAGS=", "MAKELEVEL=", "MFLAGS=")
	out, code := runCmd(t, cmd)
	if code != 0 {
		t.Fatalf("make -n build: rc=%d\n%s", code, out)
	}
	var buildPath string
	fields := strings.Fields(out)
	for i, field := range fields {
		if field == "-o" && i+1 < len(fields) {
			buildPath = fields[i+1]
		}
	}
	if servicePath != "/usr/local/bin/tierd" {
		t.Fatalf("service executable = %q, want /usr/local/bin/tierd", servicePath)
	}
	if buildPath != "bin/tierd" {
		t.Fatalf("make build output = %q, want bin/tierd", buildPath)
	}
	if installSource != buildPath || installDestination != servicePath {
		t.Fatalf("sudo install source %q destination %q, want build output %q and ExecStart %q", installSource, installDestination, buildPath, servicePath)
	}
}
