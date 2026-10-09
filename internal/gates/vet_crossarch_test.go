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
