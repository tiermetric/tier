package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/shipper"
	"github.com/tiermetric/tier/internal/store"
)

// Pins --worktree-attribution's precedence (CLI > env > config > default) and
// the source each startup line names; the config arm reads the real YAML key.
func TestResolveWorktreeAttribution_Precedence(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(cfgPath, []byte("watch:\n  worktree_attribution: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfgOn, f := cfg.Watch.WorktreeAttribution, false
	cfgOff := &f
	for _, tc := range []struct {
		name string
		args []string
		env  string
		cfg  *bool
		want worktreeAttrSetting
	}{
		{"default", nil, "", nil, worktreeAttrSetting{false, "default"}},
		{"config", nil, "", cfgOn, worktreeAttrSetting{true, "config watch.worktree_attribution"}},
		{"env beats config", nil, "0", cfgOn, worktreeAttrSetting{false, "env TIER_WORKTREE_ATTRIBUTION"}},
		{"env on", nil, "true", cfgOff, worktreeAttrSetting{true, "env TIER_WORKTREE_ATTRIBUTION"}},
		{"CLI beats env", []string{"--worktree-attribution=false"}, "1", cfgOn, worktreeAttrSetting{false, "flag --worktree-attribution"}},
		{"CLI on", []string{"--worktree-attribution"}, "0", cfgOff, worktreeAttrSetting{true, "flag --worktree-attribution"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(worktreeAttrEnv, tc.env)
			fs := flag.NewFlagSet("x", flag.ContinueOnError)
			cli := fs.Bool(worktreeAttrFlag, false, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			got, err := resolveWorktreeAttribution(fs, *cli, tc.cfg)
			if err != nil || got != tc.want {
				t.Errorf("got %+v, %v; want %+v", got, err, tc.want)
			}
			if (got.attribution([]string{t.TempDir()}) != nil) != tc.want.on {
				t.Errorf("attribution() non-nil = %v, want %v (nil is today's rule)", !tc.want.on, tc.want.on)
			}
		})
	}
	t.Setenv(worktreeAttrEnv, "yes-please")
	for _, args := range [][]string{nil, {"--worktree-attribution"}} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		cli := fs.Bool(worktreeAttrFlag, false, "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveWorktreeAttribution(fs, *cli, cfgOn); err == nil {
			t.Errorf("args %v: a malformed env value resolved; it must fail loud like envBool, even under an explicit flag", args)
		}
	}
}

// wtShipFixture is a repo with a linked worktree on feature/44-wt and one
// transcript whose session sits in the main checkout on "main" while its one
// message reads a file in the worktree, two hours from now (past the reflog's
// first entry and the repo commit's join window, so branch names alone decide).
func wtShipFixture(t *testing.T) (repo, wt, claudeDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := nonNeutralDir(t)
	repo, wt = filepath.Join(base, "repo"), filepath.Join(base, "wt")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.email=t@test", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "feat: scaffold (closes #42)"},
		{"-C", repo, "worktree", "add", "--quiet", "-b", "feature/44-wt", wt},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	claudeDir = t.TempDir()
	at := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	writeWTTranscript(t, claudeDir, fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"sessionId":"sess-wt","gitBranch":"main","cwd":%q,"isSidechain":false,`+
			`"message":{"id":"msg_wt_1","model":"claude-sonnet-4","role":"assistant",`+
			`"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":%q}}],`+
			`"usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		at, repo, filepath.Join(wt, "a.go")))
	return repo, wt, claudeDir
}

// nonNeutralDir is a canonical temp dir outside the index's neutral roots:
// t.TempDir() is under /tmp on Linux, where every path reads neutral.
func nonNeutralDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, neutral := range []string{"/tmp", "/private/tmp"} {
		if d == neutral || strings.HasPrefix(d, neutral+"/") {
			if d, err = os.MkdirTemp("/var/tmp", "tier-823-"); err != nil {
				t.Skipf("no temp dir outside /tmp: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(d) })
			if d, err = filepath.EvalSymlinks(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	return d
}

func writeWTTranscript(t *testing.T, claudeDir string, lines ...string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", "p-wt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func clearCaptureEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"TIER_CODEX_ROLLOUT", "TIER_OPENCODE", "TIER_MUSE", worktreeAttrEnv} {
		t.Setenv(k, "")
	}
}

func storedIssueRules(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().Add(24*time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, strings.TrimSpace(r.IssueID+" "+string(r.AttributionRule)))
	}
	return got
}

// Pins ship's wiring both ways: off books the worktree read to today's rule
// with no rule stored; on builds the index from --repo, attributes the message
// to the worktree branch's issue, and stores the rule.
func TestRunShip_WorktreeAttribution_OffAndOn(t *testing.T) {
	clearCaptureEnv(t)
	loadDeterministicPrices(t)
	repo, _, claudeDir := wtShipFixture(t)
	for _, tc := range []struct {
		extra []string
		want  string
	}{
		{nil, collector.UnattributedMain},
		{[]string{"--worktree-attribution"}, "issue-44 worktree-toolpath"},
	} {
		srv, db := newShipTestServer(t, "")
		captureStdout(t, func() {
			runShip(append([]string{"--server", srv.URL, "--repo", repo, "--claude-dir", claudeDir,
				"--since", "2026-01-01", "--developer", "alice"}, tc.extra...))
		})
		if got := storedIssueRules(t, db); !reflect.DeepEqual(got, []string{tc.want}) {
			t.Errorf("ship %v stored %q, want [%q]", tc.extra, got, tc.want)
		}
	}
}

// Pins ship's one index over every --repo, under each collector's resolved
// path: a session in repo a whose message reads a file in repo b's linked
// worktree is stored under b by b's collector, not booked to a as
// unattributed:foreign-repo. b is passed relative, so the index must be built
// from the resolved paths the collectors use.
func TestRunShip_WorktreeAttribution_IndexCoversEveryRepo(t *testing.T) {
	clearCaptureEnv(t)
	loadDeterministicPrices(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := nonNeutralDir(t)
	a, b, bwt := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "bwt")
	commit := []string{"-c", "user.email=t@test", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "scaffold"}
	for _, args := range [][]string{
		{"init", "-q", a}, append([]string{"-C", a}, commit...),
		{"init", "-q", b}, append([]string{"-C", b}, commit...),
		{"-C", b, "worktree", "add", "--quiet", "-b", "feature/44-wt", bwt},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	claudeDir := t.TempDir()
	at := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	writeWTTranscript(t, claudeDir, fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"sessionId":"sess-ab","gitBranch":"main","cwd":%q,"isSidechain":false,`+
			`"message":{"id":"msg_ab_1","model":"claude-sonnet-4","role":"assistant",`+
			`"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":%q}}],`+
			`"usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		at, a, filepath.Join(bwt, "a.go")))
	t.Chdir(base)

	srv, db := newShipTestServer(t, "")
	captureStdout(t, func() {
		runShip([]string{"--server", srv.URL, "--repo", a, "--repo", "b", "--repo-slug", a + "=o/a", "--repo-slug", "b=o/b",
			"--claude-dir", claudeDir, "--since", "2026-01-01", "--developer", "alice", "--worktree-attribution", "--allow-empty"})
	})
	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().Add(24*time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Repo+" "+r.IssueID+" "+string(r.AttributionRule))
	}
	if want := []string{"o/b issue-44 worktree-toolpath"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stored %q, want %q", got, want)
	}
}

// Pins the release-note claim: a flag-on shipper run that a tierd without #823
// support refuses (a 400 on every batch carrying attribution_rule) is recovered
// by a later run against the upgraded server whose --since covers the message,
// rule included. The default --since
// (90 days) covers a message 29 days old, inside Claude Code's ~30-day
// transcript retention; a --since after the message does not.
func TestRunShip_WorktreeAttribution_WrongOrderUpgradeRecovers(t *testing.T) {
	clearCaptureEnv(t)
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	msgAt := time.Now().AddDate(0, 0, -29).UTC()
	writeWTTranscript(t, claudeDir, fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"sessionId":"sess-old","gitBranch":"feature/42-foo","cwd":%q,"message":{"id":"msg_old_1","model":"claude-sonnet-4","role":"assistant","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		msgAt.Format(time.RFC3339Nano), repo))

	var rejected atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"attribution_rule"`)) {
			rejected.Add(1)
			http.Error(w, `json: unknown field "attribution_rule"`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(old.Close)
	client, err := shipper.New(shipper.Config{ServerURL: old.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := &collector.JSONLCollector{RepoPath: repo, ClaudeDir: claudeDir, DeveloperID: "alice"}
	c.SetWorktreeAttribution(collector.NewWorktreeAttribution([]string{repo}))
	ctx := context.Background()
	runErr := c.Run(ctx, msgAt.AddDate(0, 0, -1), client)
	if runErr == nil {
		runErr = client.Flush(ctx)
	}
	if runErr == nil || rejected.Load() == 0 {
		t.Fatalf("the pre-#823 server accepted the flag-on batch (err %v, rejected %d); the test no longer exercises the wrong order", runErr, rejected.Load())
	}

	srv, db := newShipTestServer(t, "")
	ship := func(extra ...string) {
		captureStdout(t, func() {
			runShip(append([]string{"--server", srv.URL, "--repo", repo, "--claude-dir", claudeDir,
				"--developer", "alice", "--worktree-attribution", "--allow-empty"}, extra...))
		})
	}
	ship("--since", msgAt.AddDate(0, 0, 1).Format("2006-01-02"))
	if got := storedIssueRules(t, db); len(got) != 0 {
		t.Fatalf("a --since after the message stored %q; the control must recover nothing", got)
	}
	ship()
	if got := storedIssueRules(t, db); !reflect.DeepEqual(got, []string{"issue-42 branch"}) {
		t.Errorf("re-ship with the default --since stored %q, want [issue-42 branch]", got)
	}
}

type fileStamp struct {
	size int64
	mod  time.Time
	mode fs.FileMode
}

func treeStamps(t *testing.T, roots ...string) map[string]fileStamp {
	t.Helper()
	out := map[string]fileStamp{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			out[p] = fileStamp{info.Size(), info.ModTime(), info.Mode()}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Pins the audit: off, score prints no audit; on, it prints the counts and the
// changed message (session, old -> new issue, rule) and names no path, and the
// run writes nothing under the transcripts, the repo or the worktree.
func TestRunScore_WorktreeAttributionAudit(t *testing.T) {
	clearCaptureEnv(t)
	loadDeterministicPrices(t)
	repo, wt, claudeDir := wtShipFixture(t)
	score := func(extra ...string) string {
		return captureStdout(t, func() {
			runScore(append([]string{"--repo", repo, "--claude-dir", claudeDir, "--since", "2026-01-01", "--developer", "alice"}, extra...))
		})
	}
	if out := score(); strings.Contains(out, "Worktree attribution audit") || !strings.Contains(out, "unattributed: main/master") {
		t.Errorf("flag off: want today's report and no audit; got:\n%s", out)
	}
	before := treeStamps(t, claudeDir, repo, wt)
	out := score("--worktree-attribution")
	if after := treeStamps(t, claudeDir, repo, wt); !reflect.DeepEqual(before, after) {
		t.Errorf("the audit wrote to disk:\nbefore %v\nafter  %v", before, after)
	}
	for _, want := range []string{
		"Worktree attribution audit (#823): a dry run, nothing is stored",
		"messages: 1 with the flag off, 1 with it on",
		"changed attribution (issue or repo): 1",
		"only with the flag off (booked to another target): 0",
		"unattributed:foreign-repo: 0",
		"    worktree-toolpath                  1",
		`session "sess-wt"  "unqualified" unattributed: main/master -> "unqualified" issue-44 (worktree-toolpath)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q; got:\n%s", want, out)
		}
	}
	for _, leak := range []string{wt, repo, "a.go", "toolu_1"} {
		if strings.Contains(out, leak) {
			t.Errorf("audit printed %q (a path or message content); got:\n%s", leak, out)
		}
	}
}

// Pins the audit's counts and its sample bound on synthetic events.
func TestPrintWorktreeAudit_CountsAndSampleBound(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ev := func(key, repo, issue string, rule store.AttributionRule, n int) collector.TokenEvent {
		return collector.TokenEvent{IdempotencyKey: key, Repo: repo, IssueID: issue, AttributionRule: rule,
			SessionID: "s", Timestamp: at.Add(time.Duration(n) * time.Minute)}
	}
	var off, on []collector.TokenEvent
	for i := range 25 {
		k := fmt.Sprintf("k%02d", i)
		off = append(off, ev(k, "o/r", collector.UnattributedMain, "", i))
		// on arrives newest first, so the sample is "first 20 by time" only
		// if the audit sorts it.
		j := 24 - i
		on = append(on, ev(fmt.Sprintf("k%02d", j), "o/r", "issue-7", store.AttributionRuleCarry, j))
	}
	off = append(off, ev("same", "o/r", "issue-1", "", 30), ev("gone", "o/r", "issue-2", "", 31))
	on = append(on, ev("same", "o/r", "issue-1", store.AttributionRuleBranch, 30),
		ev("new", "unqualified", collector.UnattributedForeignRepo, store.AttributionRuleWorktreeToolPath, 32))
	var b strings.Builder
	printWorktreeAudit(&b, off, on)
	out := b.String()
	for _, want := range []string{
		"messages: 27 with the flag off, 27 with it on",
		"changed attribution (issue or repo): 25",
		"only with the flag off (booked to another target): 1",
		"only with the flag on: 1",
		"unattributed:foreign-repo: 1",
		"    branch                             1\n    carry                              25\n    worktree-toolpath                  1\n",
		"    \"o/r\"                              26\n    \"unqualified\"                      1\n",
		"changed messages (the first 20 of 25 by time):",
		"2026-09-01T00:19:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "2026-09-01T00:20:00Z") {
		t.Errorf("the sample listed a 21st change; got:\n%s", out)
	}
}
