package gates

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func writeGateFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// Execute the release's actual shell blocks offline. A finding or scanner error
// must stop before archiving or uploading, for every matrix binary (including .exe).
func TestReleaseBinariesScannedBeforeAttach(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct{ Goos, Goarch string }
				}
			}
			Steps []struct {
				Name, Run       string
				If              string
				ContinueOnError bool `yaml:"continue-on-error"`
				Env             map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["binaries"]
	install, build, attest, upload := -1, -1, -1, -1
	for i, step := range job.Steps {
		switch step.Name {
		case "Install govulncheck":
			install = i
		case "Build binary + checksum":
			build = i
		case "Attest binary provenance":
			attest = i
		case "Upload binaries to the Release":
			upload = i
		}
	}
	if install < 0 || build <= install || attest <= build || upload <= attest {
		t.Fatalf("want install < build/scan < attest < upload; got %d, %d, %d, %d", install, build, attest, upload)
	}
	installer := job.Steps[install]
	if strings.Contains(installer.Run, "@latest") || strings.TrimSpace(installer.Run) != "go install golang.org/x/vuln/cmd/govulncheck@v1.8.0" || len(installer.Env) != 0 {
		t.Fatal("install the pinned govulncheck tool for the runner, outside the cross-build environment; @latest is forbidden")
	}
	for _, i := range []int{install, build, upload} {
		if job.Steps[i].ContinueOnError || job.Steps[i].If != "" {
			t.Fatalf("%s must run unconditionally and fail on errors", job.Steps[i].Name)
		}
	}
	if len(job.Strategy.Matrix.Include) != 5 {
		t.Fatalf("expected all five release platforms, got %d", len(job.Strategy.Matrix.Include))
	}
	for _, platform := range job.Strategy.Matrix.Include {
		for _, scanRC := range []int{0, 3, 1} { // clean, CVE finding, scanner failure
			t.Run(fmt.Sprintf("%s/%s/scan-exit-%d", platform.Goos, platform.Goarch, scanRC), func(t *testing.T) {
				dir := t.TempDir()
				toolsDir := filepath.Join(dir, "tools")
				trace := filepath.Join(dir, "trace")
				expected := fmt.Sprintf("tierd-v0.0.0-%s-%s/tierd", platform.Goos, platform.Goarch)
				if platform.Goos == "windows" {
					expected += ".exe"
				}
				writeGateFile(t, filepath.Join(toolsDir, "go"), `#!/bin/sh
set -eu
if [ "$1" = install ]; then
  [ -z "${GOOS:-}" ] || { echo 'bad installer GOOS' >&2; exit 99; }
  [ -z "${GOARCH:-}" ] || { echo 'bad installer GOARCH' >&2; exit 99; }
  cp "$SCANNER_STUB" "$GOPATH/bin/govulncheck"
  exit 0
fi
[ "$1" = build ]
while [ "$1" != -o ]; do shift; done
shift
mkdir -p "$(dirname "$1")"
: > "$1"
echo build >> "$TRACE"
`, 0o755)
				writeGateFile(t, filepath.Join(dir, "scanner"), `#!/bin/sh
set -eu
[ "$#" -eq 2 ] || { echo 'bad scanner argument count' >&2; exit 99; }
[ "$1" = -mode=binary ] || { echo 'bad scanner mode' >&2; exit 99; }
[ "$2" = "$EXPECTED_BINARY" ] || { echo 'bad scanner binary path' >&2; exit 99; }
[ -f "$2" ] || { echo 'missing scanner binary' >&2; exit 99; }
echo scan >> "$TRACE"
exit "$SCAN_RC"
`, 0o755)
				for _, tool := range []string{"tar", "sha256sum", "gh"} {
					writeGateFile(t, filepath.Join(toolsDir, tool), "#!/bin/sh\necho "+tool+" >> \"$TRACE\"\n", 0o755)
				}
				gopath := filepath.Join(dir, "gopath")
				if err := os.MkdirAll(filepath.Join(gopath, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				env := append(baseEnv(dir), "PATH="+toolsDir+string(os.PathListSeparator)+filepath.Join(gopath, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
					"GOPATH="+gopath, "SCANNER_STUB="+filepath.Join(dir, "scanner"), "TRACE="+trace,
					"EXPECTED_BINARY="+expected, fmt.Sprintf("SCAN_RC=%d", scanRC), "GITHUB_REF_NAME=v0.0.0", "GITHUB_SHA=fixture")
				cmd := exec.Command("bash", "-e", "-c", installer.Run)
				cmd.Dir, cmd.Env = dir, env
				if out, code := runCmd(t, cmd); code != 0 {
					t.Fatalf("install: rc=%d\n%s", code, out)
				}
				cmd = exec.Command("bash", "-e", "-c", job.Steps[build].Run)
				cmd.Dir = dir
				cmd.Env = append(env, "GOOS="+platform.Goos, "GOARCH="+platform.Goarch, "CGO_ENABLED=0")
				out, code := runCmd(t, cmd)
				if code != scanRC {
					t.Fatalf("build/scan: rc=%d, want %d\n%s", code, scanRC, out)
				}
				want := "build\nscan\n"
				if code == 0 {
					cmd = exec.Command("bash", "-e", "-c", job.Steps[upload].Run)
					cmd.Dir, cmd.Env = dir, env
					if out, code := runCmd(t, cmd); code != 0 {
						t.Fatalf("upload: rc=%d\n%s", code, out)
					}
					want += "tar\nsha256sum\ngh\n"
				}
				got, err := os.ReadFile(trace)
				if err != nil || string(got) != want {
					t.Fatalf("release operations = %q (%v), want %q", got, err, want)
				}
			})
		}
	}
}
