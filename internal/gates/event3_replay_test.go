package gates

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvent3ReplayPortableScratch(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/event3-replay.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(script, []byte("/private/")) {
		t.Fatal("replay gate contains a platform-specific /private/ path")
	}
}

// Exercise the real script's shell control flow without building Go packages.
func TestEvent3ReplayScriptReceipts(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/event3-replay.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, hash, label, timeout string
		listCode, wantCode         int
	}{
		{"sha256sum-fallback", "1ee66621df1b000461d7c2ebefacfdae1bc09ab0191802d4f514d18b51780d19", "EVENT3_REPLAY=GREEN", "", 0, 0},
		{"custom-timeout", "1ee66621df1b000461d7c2ebefacfdae1bc09ab0191802d4f514d18b51780d19", "EVENT3_REPLAY=GREEN", "7m", 0, 0},
		{"listing-failure", "1ee66621df1b000461d7c2ebefacfdae1bc09ab0191802d4f514d18b51780d19", "EVENT3_REPLAY=TEST_RED; replay test listing failed", "", 7, 1},
		{"fixture-mismatch", "bad-hash", "EVENT3_REPLAY=TEST_RED; adjudicated fixture SHA-256 mismatch", "", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			scratch := filepath.Join(dir, "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatal(err)
			}
			writeGateFile(t, filepath.Join(dir, "scripts/event3-replay.sh"), string(script), 0o755)
			// Deliberately omit shasum from PATH to exercise Linux's fallback.
			for _, name := range []string{"bash", "dirname", "mktemp", "rm", "awk", "cat"} {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			writeGateFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = rev-parse ]; then echo fixture-head; fi\n", 0o755)
			writeGateFile(t, filepath.Join(bin, "sha256sum"), "#!/bin/sh\nprintf '%s  %s\\n' \"$FIXTURE_HASH\" \"$1\"\n", 0o755)
			writeGateFile(t, filepath.Join(bin, "go"), `#!/bin/sh
set -eu
# The caller's cache/temp settings must survive unchanged.
[ "$GOCACHE" = "$EXPECTED_CACHE" ]
[ "$GOTMPDIR" = "$EXPECTED_TEMP" ]
if [ "$1" = test ]; then
  timeout_seen=0
  previous=
  for arg in "$@"; do
    if [ "$previous" = -timeout ]; then
      [ "$arg" = "$EXPECTED_TIMEOUT" ]
      timeout_seen=1
    fi
    previous="$arg"
  done
  [ "$timeout_seen" = 1 ]
fi
if [ "$1 $2" = 'test -c' ]; then
  while [ "$1" != -o ]; do shift; done
  shift
  case "$1" in "$TMPDIR"/event3-replay.*/replay.test) ;; *) exit 99 ;; esac
  cat > "$1" <<'LIST'
#!/bin/sh
[ "$LIST_CODE" = 0 ] || exit "$LIST_CODE"
i=0
while [ "$i" -lt 13 ]; do
  echo "TestEvent3Replay_$i"
  i=$((i+1))
done
LIST
  /bin/chmod +x "$1"
elif [ "$1" = test ]; then
  [ "$2" = -race ]
  echo '--- PASS: TestEvent3Replay_stub (0.00s)'
fi
`, 0o755)
			cmd := exec.Command(filepath.Join(bin, "bash"), filepath.Join(dir, "scripts/event3-replay.sh"))
			cmd.Env = append(baseEnv(filepath.Join(dir, "absent-home")), "PATH="+bin, "TMPDIR="+scratch,
				"GOCACHE="+filepath.Join(dir, "cache"), "EXPECTED_CACHE="+filepath.Join(dir, "cache"),
				"GOTMPDIR="+filepath.Join(dir, "go-temp"), "EXPECTED_TEMP="+filepath.Join(dir, "go-temp"),
				"FIXTURE_HASH="+tc.hash, fmt.Sprintf("LIST_CODE=%d", tc.listCode))
			timeout := tc.timeout
			if timeout == "" {
				timeout = "30m"
			} else {
				cmd.Env = append(cmd.Env, "GO_TEST_TIMEOUT="+timeout)
			}
			cmd.Env = append(cmd.Env, "EXPECTED_TIMEOUT="+timeout)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.wantCode || !strings.Contains(string(out), tc.label) {
				t.Fatalf("exit=%d want %d; label=%s\n%s", code, tc.wantCode, tc.label, out)
			}
			entries, err := os.ReadDir(scratch)
			if err != nil || len(entries) != 0 {
				t.Fatalf("scratch not cleaned: %v, %v", entries, err)
			}
		})
	}
}
