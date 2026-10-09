package collector

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// routeFixture adds to wtFixture a worktree wt2 of the second configured repo
// on "scratch-r2", a branch naming no issue, whose one commit (dated f.at)
// closes #66, and an origin naming repo2 "owner/repo2".
type routeFixture struct {
	*wtFixture
	wt2 string
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	f := &routeFixture{wtFixture: newWTFixture(t)}
	f.wt2 = addWorktree(t, f.repo2, filepath.Join(f.base, "wt2"), "scratch-r2")
	cmd := exec.Command("git", "commit", "--quiet", "--allow-empty", "-m", "fix: r2 (closes #66)")
	cmd.Dir = f.wt2
	date := f.at.Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit in wt2: %v\n%s", err, out)
	}
	// The commit's reflog entry is dated f.at; the files' mtimes must not be
	// older than it, or branchAt fails closed.
	admin := filepath.Join(f.repo2, ".git", "worktrees", "wt2")
	for _, p := range []string{filepath.Join(admin, "logs", "HEAD"), filepath.Join(admin, "HEAD")} {
		if err := os.Chtimes(p, f.at, f.at); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, f.repo2, "config", "remote.origin.url", "https://github.com/owner/repo2.git")
	return f
}

// routeTranscripts is three transcripts: s1 in repo's main checkout, s2 in
// repo2's, s3 in the unconfigured repo's main checkout (outside every target).
// Each message's OutputTok is its number.
func (f *routeFixture) routeTranscripts() map[string][]string {
	line := func(id string, n int, cwd, branch, content string) string {
		return wtJSONLLine(id, f.wtAt(n), cwd, branch, content, n)
	}
	return map[string][]string{
		"s1.jsonl": {
			line("msg_1", 1, f.repo, "main", wtRead(filepath.Join(f.wt2, "r.go"))),
			line("msg_2", 2, f.repo, "main", wtText),
			line("msg_3", 3, f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go"))),
			line("msg_4", 4, f.repo, "main", wtRead(filepath.Join(f.foreignWT, "f.go"))),
			line("msg_5", 5, f.foreignWT, "feature/44-f", wtText),
			line("msg_6", 6, f.repo, "main", wtText),
		},
		"s2.jsonl": {
			line("msg_9", 9, f.repo2, "main", wtText),
			line("msg_8", 8, f.repo2, "main", wtRead(filepath.Join(f.wtA, "b.go"))),
		},
		"s3.jsonl": {
			line("msg_7", 7, f.foreign, "main", wtRead(filepath.Join(f.wt2, "r.go"))),
		},
	}
}

// Pins the carry across targets (#823 Q2): another configured repo's worktree
// sets it and routes the carried messages there; a worktree of a different
// repo clears it, and so does the main checkout of any configured repo.
func TestWorktreeResolver_CarryAcrossTargets(t *testing.T) {
	f := newRouteFixture(t)
	r := f.resolver()
	var got []string
	for _, g := range []messageGroup{
		f.g(filepath.Join(f.wt2, "r.go")), f.g(), f.g(filepath.Join(f.wtA, "a.go")), f.g(),
		f.g(filepath.Join(f.wtA, "a.go")), f.g(), f.g(filepath.Join(f.wt2, "r.go")), f.g(),
		f.g(filepath.Join(f.wt2, "r.go")), f.g(filepath.Join(f.repo2, "m.go")), f.g(),
	} {
		a, target := r.resolve(g, "", ""), ""
		if a.Target != "" {
			target = filepath.Base(a.Target)
		}
		got = append(got, strings.TrimSpace(fmt.Sprintf("%s %s %s", a.Rule, target, a.Branch)))
	}
	wantVerdicts(t, got,
		"worktree-toolpath repo2 scratch-r2", "carry repo2 scratch-r2", "worktree-toolpath repo feature/11-a", "",
		"worktree-toolpath repo feature/11-a", "carry repo feature/11-a", "worktree-toolpath repo2 scratch-r2", "",
		"worktree-toolpath repo2 scratch-r2", "", "",
	)
}

// byOutputRepo renders events as "<issue> <rule> <repo>" keyed by OutputTok,
// failing on an OutputTok seen twice.
func byOutputRepo(t *testing.T, events []TokenEvent) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, e := range events {
		if _, dup := out[e.OutputTok]; dup {
			t.Errorf("message %d emitted twice by one collector", e.OutputTok)
		}
		out[e.OutputTok] = e.IssueID + " " + string(e.AttributionRule) + " " + e.Repo
	}
	return out
}

// wantNoPathText fails if any event names a fixture path, or the unconfigured
// worktree by its directory name.
func wantNoPathText(t *testing.T, f *routeFixture, events []TokenEvent) {
	t.Helper()
	for _, e := range events {
		s := fmt.Sprintf("%#v", e)
		if strings.Contains(s, f.base) || strings.Contains(s, "foreignWT") {
			t.Errorf("event carries path text: %s", s)
		}
	}
}

func (f *routeFixture) collectors(t *testing.T, on bool) (repo, repo2 *JSONLCollector) {
	t.Helper()
	repo = wtCollector(t, f.wtFixture, "claude-route", f.routeTranscripts())
	repo2 = &JSONLCollector{RepoPath: f.repo2, ClaudeDir: repo.ClaudeDir, DeveloperID: "dev", RepoSlug: "owner/repo2"}
	if on {
		repo.worktrees, repo2.worktrees = f.x, f.x
	}
	return repo, repo2
}

// Pins #823 Q3 on the batch (ship) path: a verdict naming a worktree of the
// other configured repo books the message to that repo's collector, joined
// against that repo's own commits (scratch-r2 names no issue; only repo2's
// commit gives issue-66) and slug, in both directions; the carry follows it
// across the targets; a worktree of the unconfigured repo, by tool path or by
// cwd, books to unattributed:foreign-repo with repo "unqualified" and no path
// text; a session outside every target books nothing (Q6); and each message is
// emitted by exactly one collector.
func TestJSONLCollector_RoutesVerdictsAcrossTargets(t *testing.T) {
	f := newRouteFixture(t)
	c1, c2 := f.collectors(t, true)
	e1, e2 := wtCollect(t, f.wtFixture, c1), wtCollect(t, f.wtFixture, c2)
	want1 := map[int]string{
		3: "issue-11 worktree-toolpath owner/repo",
		4: "unattributed:foreign-repo worktree-toolpath unqualified",
		5: "unattributed:foreign-repo worktree-cwd unqualified",
		6: "unattributed:main branch owner/repo",
		8: "issue-11 worktree-toolpath owner/repo",
	}
	want2 := map[int]string{
		1: "issue-66 worktree-toolpath owner/repo2",
		2: "issue-66 carry owner/repo2",
		9: "unattributed:main branch owner/repo2",
	}
	if got := byOutputRepo(t, e1); !reflect.DeepEqual(got, want1) {
		t.Errorf("repo's collector:\n got %v\nwant %v", got, want1)
	}
	if got := byOutputRepo(t, e2); !reflect.DeepEqual(got, want2) {
		t.Errorf("repo2's collector:\n got %v\nwant %v", got, want2)
	}
	wantNoPathText(t, f, append(e1, e2...))
}

// Pins: a session whose cwd two configured targets both own (a worktree of repo
// inside repo2's directory) books each message once across the two collectors:
// a verdict to the target it names, and a message with no verdict to the one
// owner the watcher's matchTarget picks (repo2, whose directory holds the cwd).
func TestJSONLCollector_DoublyOwnedSessionBooksOnce(t *testing.T) {
	f := newRouteFixture(t)
	nested := addWorktree(t, f.repo, filepath.Join(f.repo2, "wtN"), "feature/55-n")
	if own, ok := matchTarget(nested, resolveTargets([]string{f.repo, f.repo2})); !ok || own.name != f.repo2 {
		t.Fatalf("watcher owner of the nested cwd = %q (matched %v), want repo2 %q", own.name, ok, f.repo2)
	}
	c1 := wtCollector(t, f.wtFixture, "claude-nested", map[string][]string{"s.jsonl": {
		wtJSONLLine("msg_1", f.wtAt(1), nested, "main", wtText, 1),
		wtJSONLLine("msg_2", f.wtAt(2), nested, "main", wtRead(filepath.Join(f.wtA, "a.go")), 2),
	}})
	c2 := &JSONLCollector{RepoPath: f.repo2, ClaudeDir: c1.ClaudeDir, DeveloperID: "dev", RepoSlug: "owner/repo2"}
	c1.worktrees, c2.worktrees = f.x, f.x
	if got, want := byOutputRepo(t, wtCollect(t, f.wtFixture, c1)), map[int]string{2: "issue-11 worktree-toolpath owner/repo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("repo's collector:\n got %v\nwant %v", got, want)
	}
	if got, want := byOutputRepo(t, wtCollect(t, f.wtFixture, c2)), map[int]string{1: "unattributed:main branch owner/repo2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("repo2's collector:\n got %v\nwant %v", got, want)
	}
}

// Pins: with the option off, the same transcripts give today's output: each
// session books whole to the target owning its cwd, with no rule, and the
// unconfigured worktree's branch lands on repo's issue-44.
func TestJSONLCollector_RoutingOffIsTodaysOutput(t *testing.T) {
	f := newRouteFixture(t)
	c1, c2 := f.collectors(t, false)
	want1 := map[int]string{
		1: "unattributed:main  owner/repo", 2: "unattributed:main  owner/repo", 3: "unattributed:main  owner/repo",
		4: "unattributed:main  owner/repo", 5: "issue-44  owner/repo", 6: "unattributed:main  owner/repo",
	}
	want2 := map[int]string{8: "unattributed:main  owner/repo2", 9: "unattributed:main  owner/repo2"}
	if got := byOutputRepo(t, wtCollect(t, f.wtFixture, c1)); !reflect.DeepEqual(got, want1) {
		t.Errorf("repo's collector:\n got %v\nwant %v", got, want1)
	}
	if got := byOutputRepo(t, wtCollect(t, f.wtFixture, c2)); !reflect.DeepEqual(got, want2) {
		t.Errorf("repo2's collector:\n got %v\nwant %v", got, want2)
	}
}

// routeRig is a heldRig over repo's transcript watching both configured repos,
// whose commit source gives repo2 alone a commit on scratch-r2 closing #66.
func (f *routeFixture) routeRig(t *testing.T, base string) *heldRig {
	t.Helper()
	h := newHeldRig(t, base, f.repo)
	h.targets = resolveTargets([]string{f.repo, f.repo2})
	commits := func(_ context.Context, repo string, _ time.Time) ([]gitCommit, error) {
		if repo != f.repo2 {
			return nil, nil
		}
		return []gitCommit{{Hash: "c66", Timestamp: f.at, Branches: []string{"scratch-r2"}, IssueID: "issue-66"}}, nil
	}
	h.cache = newCommitCache(time.Hour, time.Hour, time.Second, commits, time.Now)
	return h
}

// routed returns the rig's events from index from on as "<out> <issue> <rule>
// <repo>".
func (h *heldRig) routed(from int) []string {
	var out []string
	for _, e := range h.rec.snapshot()[from:] {
		out = append(out, fmt.Sprintf("%d %s %s %s", e.OutputTok, e.IssueID, e.AttributionRule, e.Repo))
	}
	return out
}

// Pins #823 Q3 on the watcher path: a verdict naming the other configured
// repo's worktree books to that repo's commits and slug, and the carry keeps
// routing there across reads (the checkpoint holds wt2, a configured
// worktree); a worktree of the unconfigured repo books to
// unattributed:foreign-repo with repo "unqualified", clears the carry, and
// leaves no path text in any event or in the checkpoint.
func TestWatcher_RoutesVerdictsAcrossTargets(t *testing.T) {
	f := newRouteFixture(t)
	h := f.routeRig(t, f.base)
	w := &Watcher{worktrees: f.x}
	line := func(id string, n int, cwd, branch, content string) string {
		return wtJSONLLine(id, f.wtAt(n), cwd, branch, content, n)
	}
	h.read(t, w,
		line("msg_1", 1, f.repo, "main", wtRead(filepath.Join(f.wt2, "r.go"))),
		line("msg_2", 2, f.repo, "main", wtText),
	)
	if got := h.state.metadata.LastWorktree; got != f.wt2 {
		t.Errorf("carried worktree after read 1 = %q, want %q", got, f.wt2)
	}
	h.read(t, w,
		line("msg_3", 3, f.repo, "main", wtText),
		line("msg_4", 4, f.repo, "main", wtRead(filepath.Join(f.foreignWT, "f.go"))),
		line("msg_5", 5, f.foreignWT, "feature/44-f", wtText),
		line("msg_6", 6, f.repo, "main", wtText),
		line("msg_7", 7, f.repo, "main", wtText),
	)
	wantEvents(t, h.routed(0),
		"1 issue-66 worktree-toolpath owner/repo2", // read 1 holds msg_2 back
		"4 unattributed:foreign-repo worktree-toolpath unqualified",
		"5 unattributed:foreign-repo worktree-cwd unqualified",
		"6 unattributed:main branch owner/repo",
		"2 issue-66 carry owner/repo2",
		"3 issue-66 carry owner/repo2",
	)
	wantNoPathText(t, f, h.rec.snapshot())
	cp, err := checkpointFromState(h.path, h.state)
	if err != nil {
		t.Fatal(err)
	}
	if h.state.metadata.LastWorktree != "" || strings.Contains(cp.Metadata, "foreignWT") {
		t.Errorf("after the unconfigured worktree: carried %q, checkpoint %s; want nothing carried and no foreign path",
			h.state.metadata.LastWorktree, cp.Metadata)
	}
	// A file repo2 owns (not the first watched target): its own share books to
	// repo2, and a verdict naming repo's worktree books to repo.
	h2 := f.routeRig(t, t.TempDir())
	h2.read(t, w,
		line("msg_9", 9, f.repo2, "main", wtText),
		line("msg_8", 8, f.repo2, "main", wtRead(filepath.Join(f.wtA, "b.go"))),
		line("msg_10", 10, f.repo2, "main", wtText),
	)
	wantEvents(t, h2.routed(0),
		"8 issue-11 worktree-toolpath owner/repo",
		"9 unattributed:main branch owner/repo2", // msg_10 is held back
	)
}

// Pins: with the option off, the watcher books the same lines whole to repo,
// with no rule, and the unconfigured worktree's branch lands on issue-44.
func TestWatcher_RoutingOffIsTodaysOutput(t *testing.T) {
	f := newRouteFixture(t)
	h := f.routeRig(t, t.TempDir())
	line := func(id string, n int, cwd, branch, content string) string {
		return wtJSONLLine(id, f.wtAt(n), cwd, branch, content, n)
	}
	h.read(t, &Watcher{},
		line("msg_1", 1, f.repo, "main", wtRead(filepath.Join(f.wt2, "r.go"))),
		line("msg_2", 2, f.repo, "main", wtText),
		line("msg_4", 4, f.repo, "main", wtRead(filepath.Join(f.foreignWT, "f.go"))),
		line("msg_5", 5, f.foreignWT, "feature/44-f", wtText),
	)
	wantEvents(t, h.routed(0),
		"1 unattributed:main  owner/repo",
		"2 unattributed:main  owner/repo",
		"4 unattributed:main  owner/repo",
		"5 issue-44  owner/repo",
	)
}

// Pins: a message whose verdict names an indexed target the watcher does not
// watch books nowhere and is counted in a warning that names no path.
func TestWatcher_UnwatchedVerdictTargetIsCounted(t *testing.T) {
	f := newRouteFixture(t)
	targets := resolveTargets([]string{f.repo})
	s := &sessionSummary{
		SessionID: "sess-823",
		Messages: []messageUsage{
			{messageID: "msg_1", timestamp: f.wtAt(1), model: "claude-sonnet-4", output: 1},
			{messageID: "msg_2", timestamp: f.wtAt(2), gitBranch: "main", model: "claude-sonnet-4", output: 2},
		},
		attribution: map[string]messageAttribution{"msg_1": {Rule: "worktree-toolpath", Target: f.repo2, Branch: "scratch-r2"}},
	}
	noCommits := func(context.Context, string, time.Time) ([]gitCommit, error) { return nil, nil }
	cache := newCommitCache(time.Hour, time.Hour, time.Second, noCommits, time.Now)
	var buf bytes.Buffer
	w := &Watcher{worktrees: f.x, RepoSlugs: map[string]string{f.repo: "owner/repo"}}
	events := w.joinRouted(context.Background(), s, targets[0], targets, "dev", cache, slog.New(slog.NewTextHandler(&buf, nil)))
	if got := byOutputRepo(t, events); !reflect.DeepEqual(got, map[int]string{2: "unattributed:main branch owner/repo"}) {
		t.Errorf("events = %v, want msg_2 alone", got)
	}
	if log := buf.String(); !strings.Contains(log, "messages=1") || strings.Contains(log, f.base) {
		t.Errorf("log = %q, want a count of 1 and no path", log)
	}
}
