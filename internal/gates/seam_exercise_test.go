package gates

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seamScript(t *testing.T) []byte {
	t.Helper()
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/seam-exercise.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func seamCommand(t *testing.T, script []byte, dir, policy, captured, diagnostic string) (*exec.Cmd, string) {
	t.Helper()
	writeGateFile(t, filepath.Join(dir, "scripts/seam-exercise.sh"), string(script), 0o755)
	writeGateFile(t, filepath.Join(dir, "testdata/seam-jsonl-ingestion/sample.jsonl"), `{"cwd":"__TIER_REPO__","again":"__TIER_REPO__"}`+"\n", 0o644)
	writeGateFile(t, filepath.Join(dir, "testdata/seam-jsonl-ingestion/last_captured"), captured+"\n", 0o644)
	toolsDir := t.TempDir()
	trace := filepath.Join(toolsDir, "trace")
	physicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(physicalDir); err != nil {
		t.Fatal(err)
	}
	quoted := strings.TrimSuffix(encoded.String(), "\n")
	writeGateFile(t, filepath.Join(toolsDir, "expected.jsonl"), `{"cwd":`+quoted+`,"again":`+quoted+"}\n", 0o644)
	writeGateFile(t, filepath.Join(toolsDir, "diagnostic"), diagnostic, 0o644)
	writeGateFile(t, filepath.Join(toolsDir, "go"), `#!/bin/sh
set -eu
[ "$*" = 'build -o bin/tierd ./cmd/tierd' ]
echo build >> "$TRACE"
mkdir -p bin
cp "$SCORE_STUB" bin/tierd
`, 0o755)
	writeGateFile(t, filepath.Join(toolsDir, "score"), `#!/bin/sh
set -eu
[ "$#" -eq 7 ] || { echo 'bad score argument count' >&2; exit 99; }
[ "$1" = score ] || { echo 'bad score command' >&2; exit 99; }
[ "$2" = --repo ] || { echo 'bad score repo flag' >&2; exit 99; }
[ "$4" = --claude-dir ] || { echo 'bad score claude-dir flag' >&2; exit 99; }
[ "$6" = --since ] || { echo 'bad score since flag' >&2; exit 99; }
[ "$7" = 2026-01-01 ] || { echo 'bad score since date' >&2; exit 99; }
cmp "$EXPECTED_SAMPLE" "$5/projects/seam-exercise/sample.jsonl" || { echo 'bad score repo path JSON' >&2; exit 99; }
echo score >> "$TRACE"
echo 'TOTAL 1.25'
echo 'price table identity: table_hash=tierpt1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' >&2
cat "$DIAGNOSTIC" >&2
`, 0o755)
	cmd := exec.Command("bash", filepath.Join(dir, "scripts/seam-exercise.sh"))
	cmd.Dir = dir
	cmd.Env = append(baseEnv(dir), "PATH="+toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SEAM_STALE_POLICY="+policy, "TRACE="+trace, "SCORE_STUB="+filepath.Join(toolsDir, "score"),
		"EXPECTED_SAMPLE="+filepath.Join(toolsDir, "expected.jsonl"), "DIAGNOSTIC="+filepath.Join(toolsDir, "diagnostic"))
	return cmd, trace
}

func TestSeamExerciseRunsInWorktree(t *testing.T) {
	requireGit(t)
	script := seamScript(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "seam fixture")
	worktree := filepath.Join(t.TempDir(), `worktree & | " \ with spaces`)
	gitIn(t, repo, "worktree", "add", "-q", "-b", "seam-test", worktree)
	info, err := os.Stat(filepath.Join(worktree, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("fixture must have a .git pointer file: %v", err)
	}
	for _, fixture := range []struct{ name, dir string }{
		{"clone", repo},
		{"worktree", worktree},
		{"non-repo", t.TempDir()},
		{"nested-export", filepath.Join(repo, "nested export")},
	} {
		for _, policy := range []string{"fail", "warn"} {
			t.Run(fixture.name+"/"+policy, func(t *testing.T) {
				dir := fixture.dir
				cmd, trace := seamCommand(t, script, dir, policy, time.Now().UTC().Format("2006-01-02"), "")
				out, code := runCmd(t, cmd)
				if fixture.name == "non-repo" || fixture.name == "nested-export" {
					want := 1
					if policy == "warn" {
						want = 0
						if !strings.Contains(out, "SKIPPING seam-exercise") {
							t.Fatalf("non-repo warn must still skip: %s", out)
						}
					}
					if code != want {
						t.Fatalf("non-repo: rc=%d, want %d\n%s", code, want, out)
					}
					if _, err := os.Stat(trace); !os.IsNotExist(err) {
						t.Fatal("non-repo must stop before building")
					}
					return
				}
				if code != 0 || !strings.Contains(out, "PASS: TOTAL=") || strings.Contains(out, "SKIPPING") {
					t.Fatalf("seam must run to its contract assertions: rc=%d\n%s", code, out)
				}
				got, err := os.ReadFile(trace)
				if err != nil || string(got) != "build\nscore\n" {
					t.Fatalf("seam operations = %q (%v), want build then score", got, err)
				}
			})
		}
	}
}

func TestSeamExerciseEmptyCwdDiagnostics(t *testing.T) {
	requireGit(t)
	script := seamScript(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	for _, fixture := range []struct {
		name, diagnostic, failure string
	}{
		{"absent", "", ""},
		{"text-zero", "time=now empty_cwd=0\n", ""},
		{"json-zero", `{"empty_cwd":0}` + "\n", ""},
		{"json-spaces", `{"empty_cwd" : 0, "sessions":2}` + "\n", ""},
		{"text-dropped", "time=now empty_cwd=1 sessions=2\n", "parser dropped 1 session(s)"},
		{"text-colon", "empty_cwd: 2\n", "parser dropped 2 session(s)"},
		{"json-dropped", `{"empty_cwd":1}` + "\n", "parser dropped 1 session(s)"},
		{"later-dropped", "empty_cwd=0\n" + `{"empty_cwd":2}` + "\n", "parser dropped 2 session(s)"},
		{"text-malformed", "empty_cwd=unknown\n", "unparseable empty_cwd diagnostic"},
		{"json-malformed", `{"empty_cwd":"unknown"}` + "\n", "unparseable empty_cwd diagnostic"},
		{"json-null", `{"empty_cwd":null}` + "\n", "unparseable empty_cwd diagnostic"},
		{"negative", `{"empty_cwd":-1}` + "\n", "unparseable empty_cwd diagnostic"},
		{"fraction", `{"empty_cwd":1.5}` + "\n", "unparseable empty_cwd diagnostic"},
		{"bad-suffix", "empty_cwd=1oops\n", "unparseable empty_cwd diagnostic"},
		{"later-malformed", "empty_cwd=0\nempty_cwd=unknown\n", "unparseable empty_cwd diagnostic"},
		{"unterminated-json", `{"empty_cwd":1}`, "parser dropped 1 session(s)"},
	} {
		for _, policy := range []string{"fail", "warn"} {
			t.Run(fixture.name+"/"+policy, func(t *testing.T) {
				cmd, trace := seamCommand(t, script, repo, policy, time.Now().UTC().Format("2006-01-02"), fixture.diagnostic)
				out, code := runCmd(t, cmd)
				if fixture.failure == "" {
					if code != 0 || !strings.Contains(out, "PASS: TOTAL=") {
						t.Fatalf("rc=%d, want PASS\n%s", code, out)
					}
				} else if code != 1 || !strings.Contains(out, fixture.failure) || strings.Contains(out, "PASS: TOTAL=") {
					t.Fatalf("rc=%d, want failure %q\n%s", code, fixture.failure, out)
				}
				got, err := os.ReadFile(trace)
				if err != nil || string(got) != "build\nscore\n" {
					t.Fatalf("must inspect score diagnostics: trace=%q (%v)", got, err)
				}
			})
		}
	}
}

func TestSeamExerciseStaleMarker(t *testing.T) {
	requireGit(t)
	script := seamScript(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	for _, policy := range []string{"fail", "warn"} {
		t.Run(policy, func(t *testing.T) {
			cmd, trace := seamCommand(t, script, repo, policy, "2000-01-01", "")
			out, code := runCmd(t, cmd)
			if policy == "fail" {
				if code != 1 || !strings.Contains(out, "FAIL: captured sample is stale:") {
					t.Fatalf("aged marker must fail: rc=%d\n%s", code, out)
				}
				if _, err := os.Stat(trace); !os.IsNotExist(err) {
					t.Fatal("stale fail must stop before building")
				}
			} else {
				if code != 0 || !strings.Contains(out, "WARN: captured sample is stale:") || !strings.Contains(out, "PASS: TOTAL=") {
					t.Fatalf("aged marker must warn and exercise: rc=%d\n%s", code, out)
				}
				got, err := os.ReadFile(trace)
				if err != nil || string(got) != "build\nscore\n" {
					t.Fatalf("stale warn must build and score: trace=%q (%v)", got, err)
				}
			}
		})
	}
	t.Run("warn-still-rejects-dropped-session", func(t *testing.T) {
		cmd, _ := seamCommand(t, script, repo, "warn", "2000-01-01", `{"empty_cwd":1}`+"\n")
		out, code := runCmd(t, cmd)
		if code != 1 || !strings.Contains(out, "WARN: captured sample is stale:") || !strings.Contains(out, "parser dropped 1 session(s)") {
			t.Fatalf("stale warn must preserve the ingestion guard: rc=%d\n%s", code, out)
		}
	})
}

func TestSeamExerciseMalformedMarker(t *testing.T) {
	requireGit(t)
	script := seamScript(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	for _, captured := range []string{"", "garbage", "2026-06-22-x", "2026-6-22", "2026-00-01", "2026-13-01", "2026-99-99", "2026-01-00", "2026-01-32", "2026-01-99"} {
		for _, policy := range []string{"fail", "warn"} {
			t.Run(captured+"/"+policy, func(t *testing.T) {
				cmd, trace := seamCommand(t, script, repo, policy, captured, "")
				out, code := runCmd(t, cmd)
				if code != 1 || !strings.Contains(out, "last_captured must be YYYY-MM-DD") {
					t.Fatalf("malformed marker must fail shape check: rc=%d\n%s", code, out)
				}
				if _, err := os.Stat(trace); !os.IsNotExist(err) {
					t.Fatal("malformed marker must stop before building")
				}
			})
		}
	}
}

func TestSeamExerciseIgnoresGitEnvironment(t *testing.T) {
	requireGit(t)
	script := seamScript(t)
	repo, foreign := t.TempDir(), t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, foreign, "init", "-q")
	for _, policy := range []string{"fail", "warn"} {
		t.Run(policy, func(t *testing.T) {
			cmd, _ := seamCommand(t, script, repo, policy, time.Now().UTC().Format("2006-01-02"), "")
			cmd.Env = append(cmd.Env, "GIT_DIR="+filepath.Join(foreign, ".git"), "GIT_WORK_TREE="+foreign)
			out, code := runCmd(t, cmd)
			if code != 0 || !strings.Contains(out, "PASS: TOTAL=") || strings.Contains(out, "SKIPPING") {
				t.Fatalf("git environment must not redirect the preflight: rc=%d\n%s", code, out)
			}
		})
	}
}
