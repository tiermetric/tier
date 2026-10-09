package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// wtFixture is a configured repo with two linked worktrees (on feature/11-a
// and feature/22-b), a second configured repo, an unconfigured repo with a
// worktree, a forged worktree that does not verify, and a neutral scratch dir.
type wtFixture struct {
	base, repo, wtA, wtB, repo2, foreign, foreignWT, forged, scratch string
	x                                                                *worktreeIndex
	at                                                               time.Time // a time every worktree's reflog answers
}

func newWTFixture(t *testing.T) *wtFixture {
	t.Helper()
	needOwner(t)
	base := canonPath(t, t.TempDir())
	f := &wtFixture{base: base, repo: wtRepo(t, base, "repo"), repo2: wtRepo(t, base, "repo2"), foreign: wtRepo(t, base, "foreign")}
	f.wtA = addWorktree(t, f.repo, filepath.Join(base, "wtA"), "feature/11-a")
	f.wtB = addWorktree(t, f.repo, filepath.Join(base, "wtB"), "feature/22-b")
	f.foreignWT = addWorktree(t, f.foreign, filepath.Join(base, "foreignWT"), "feature/44-f")
	f.forged = filepath.Join(base, "forged")
	writeFile(t, filepath.Join(f.forged, ".git"), "gitdir: "+filepath.Join(base, "no-such-admin")+"\n")
	f.scratch = filepath.Join(base, "scratch")
	mkdirs(t, f.scratch)
	f.x = newWorktreeIndex([]string{f.repo, f.repo2}, []string{f.scratch})
	// Past the reflogs' first second, and more than joinWindow after the
	// repos' only commits, so branch names alone decide the issue.
	f.at = time.Now().Add(2 * time.Hour)
	return f
}

func (f *wtFixture) resolver() *worktreeResolver {
	return &worktreeResolver{index: f.x, now: f.at.Add(time.Hour)}
}

// g is a message group at the fixture's time naming paths.
func (f *wtFixture) g(paths ...string) messageGroup {
	return messageGroup{ID: "msg_x", First: f.at, Last: f.at, Paths: paths}
}

// verdict renders an attribution as "<rule> <branch>", or "<rule> foreign-repo"
// for a Foreign one (which must name no target or branch), checking the target
// of every other non-empty rule is the configured repo named want.
func verdict(t *testing.T, a messageAttribution, wantTarget string) string {
	t.Helper()
	if a.Foreign {
		if a.Target != "" || a.Branch != "" {
			t.Errorf("foreign attribution %+v names a target or branch", a)
		}
		return string(a.Rule) + " foreign-repo"
	}
	if a.Rule != "" && a.Target != wantTarget {
		t.Errorf("attribution %+v: target %q, want %q", a, a.Target, wantTarget)
	}
	return strings.TrimSpace(string(a.Rule) + " " + a.Branch)
}

// run resolves gs in order with no cwd and returns each verdict.
func (f *wtFixture) run(t *testing.T, r *worktreeResolver, gs ...messageGroup) []string {
	t.Helper()
	var out []string
	for _, g := range gs {
		out = append(out, verdict(t, r.resolve(g, "", ""), f.repo))
	}
	return out
}

func wantVerdicts(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("verdicts:\n got %q\nwant %q", got, want)
	}
}

// Pins arm 1: a cwd in a linked worktree wins over the message's own tool path
// when the named transcript branch equals the worktree's reflog branch. A
// branch the reflog disagrees with (the parent checkout's), or an empty,
// "HEAD" or harness branch, lets the next arm decide, and never clears the
// carry.
func TestWorktreeResolver_CWDArm(t *testing.T) {
	f := newWTFixture(t)
	r := f.resolver()
	got := verdict(t, r.resolve(f.g(filepath.Join(f.wtB, "b.go")), f.wtA, "feature/11-a"), f.repo)
	if got != "worktree-cwd feature/11-a" {
		t.Errorf("cwd in a worktree = %q, want %q", got, "worktree-cwd feature/11-a")
	}
	if got := verdict(t, r.resolve(f.g(), f.wtA, "feature/33-c"), f.repo); got != "carry feature/22-b" {
		t.Errorf("cwd whose reflog disagrees with the transcript branch = %q, want the carry's verdict", got)
	}
	for _, branch := range []string{"feature/33-c", "", "HEAD", "worktree-agent-a1b2c3"} {
		got := verdict(t, r.resolve(f.g(filepath.Join(f.wtB, "b.go")), f.wtA, branch), f.repo)
		if got != "worktree-toolpath feature/22-b" {
			t.Errorf("cwd with branch %q = %q, want the tool path's verdict", branch, got)
		}
	}
	for _, cwd := range []string{f.repo, f.scratch, ""} {
		if got := verdict(t, r.resolve(f.g(), cwd, "feature/33-c"), f.repo); got != "carry feature/22-b" {
			t.Errorf("cwd %q outside a worktree = %q, want the carry's verdict", cwd, got)
		}
	}
}

// Pins: a cwd-arm verdict leaves the carry alone, and a network cwd is never
// looked up on disk.
func TestWorktreeResolver_CWDNeverMovesTheCarry(t *testing.T) {
	f := newWTFixture(t)
	r := f.resolver()
	wantVerdicts(t, []string{
		verdict(t, r.resolve(f.g(filepath.Join(f.wtA, "a.go")), "", ""), f.repo),
		verdict(t, r.resolve(f.g(), f.wtB, "feature/22-b"), f.repo),
		verdict(t, r.resolve(f.g(), "", ""), f.repo),
	}, "worktree-toolpath feature/11-a", "worktree-cwd feature/22-b", "carry feature/11-a")
	if got := verdict(t, f.resolver().resolve(f.g(), "/"+f.wtB, "feature/22-b"), f.repo); got != "" {
		t.Errorf("network cwd = %q, want today's rule", got)
	}
}

// Pins: when the cwd decides, the message's tool-path worktree still sets the
// carry only if its branch reads alike at the message's first and last lines;
// the message leaves the same carry whatever its cwd.
func TestWorktreeResolver_CWDWinsStillValidatesToolPathCarry(t *testing.T) {
	f := newWTFixture(t)
	// wtB checks out "other" between msg_1's first and last lines.
	gitFile, err := os.ReadFile(filepath.Join(f.wtB, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(strings.TrimPrefix(string(gitFile), "gitdir:"))
	logPath := filepath.Join(admin, "logs", "HEAD")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	es, ok := parseReflog(string(raw))
	if !ok {
		t.Fatal("wtB reflog does not parse")
	}
	tip, checkout := es[len(es)-1].new, f.at.Add(10*time.Second)
	log := string(raw) + rlLine(tip, tip, checkout.Unix(), "checkout: moving from feature/22-b to other")
	writeFile(t, logPath, log)
	writeFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/other\n")
	for _, p := range []string{logPath, filepath.Join(admin, "HEAD")} {
		if err := os.Chtimes(p, checkout, checkout); err != nil {
			t.Fatal(err)
		}
	}
	msg1 := messageGroup{ID: "msg_1", First: f.at, Last: f.at.Add(20 * time.Second), Paths: []string{filepath.Join(f.wtB, "b.go")}}
	msg2 := messageGroup{ID: "msg_2", First: f.at.Add(30 * time.Second), Last: f.at.Add(30 * time.Second)}
	r := f.resolver()
	wantVerdicts(t, []string{
		verdict(t, r.resolve(msg1, f.wtA, "feature/11-a"), f.repo),
		verdict(t, r.resolve(msg2, "", ""), f.repo),
	}, "worktree-cwd feature/11-a", "")
	// Control: without the cwd, msg_1 is today's rule and carries nothing.
	wantVerdicts(t, f.run(t, f.resolver(), msg1, msg2), "", "")
}

// Pins: a message whose first and last lines straddle a checkout in the
// carried worktree is today's rule, and clears the carry.
func TestWorktreeResolver_BranchMustAgreeAcrossMessage(t *testing.T) {
	x, c, _ := reflogAdmin(t, historyHead, historyLog)
	span := func(first, last int64) messageGroup {
		return messageGroup{ID: "msg_x", First: sec(first), Last: sec(last)}
	}
	for _, tc := range []struct {
		name string
		gs   []messageGroup
		want []string
	}{
		{"inside one branch", []messageGroup{span(1011, 1019), span(1041, 1041)}, []string{"carry feature/823-x", "carry feature/823-x"}},
		{"straddles a checkout", []messageGroup{span(1015, 1025), span(1041, 1041)}, []string{"", ""}},
	} {
		r := &worktreeResolver{index: x, now: reflogNow, carry: c}
		var got []string
		for _, g := range tc.gs {
			got = append(got, verdict(t, r.resolve(g, "", ""), "repo"))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: verdicts %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Pins arm 2: a message's own tool path names its worktree, whose branch at the
// message's time comes from the reflog.
func TestWorktreeResolver_ToolPathArm(t *testing.T) {
	f := newWTFixture(t)
	wantVerdicts(t, f.run(t, f.resolver(), f.g(filepath.Join(f.wtA, "a.go"), filepath.Join(f.scratch, "n"))),
		"worktree-toolpath feature/11-a")
}

// Pins arm 3 and arm 4: later messages carry the last worktree referenced; a
// message before any reference, with no signal, is today's rule.
func TestWorktreeResolver_CarryArm(t *testing.T) {
	f := newWTFixture(t)
	wantVerdicts(t, f.run(t, f.resolver(), f.g(), f.g(filepath.Join(f.wtA, "a.go")), f.g(), f.g(filepath.Join(f.wtA, "b.go")), f.g()),
		"", "worktree-toolpath feature/11-a", "carry feature/11-a", "worktree-toolpath feature/11-a", "carry feature/11-a")
}

// Pins every reset: after the trigger message the carry is gone. The trigger's
// own verdict is listed too.
func TestWorktreeResolver_CarryResets(t *testing.T) {
	f := newWTFixture(t)
	a := filepath.Join(f.wtA, "a.go")
	late := f.g()
	late.First, late.Last = f.at, f.at.Add(time.Hour)
	for _, tc := range []struct {
		name    string
		trigger messageGroup
		want    string
	}{
		{"own main checkout", f.g(filepath.Join(f.repo, "m.go")), ""},
		{"another configured repo's main checkout", f.g(filepath.Join(f.repo2, "m.go")), ""},
		{"unconfigured repo's main checkout", f.g(filepath.Join(f.foreign, "m.go")), ""},
		{"unconfigured repo's worktree", f.g(filepath.Join(f.foreignWT, "m.go")), "worktree-toolpath foreign-repo"},
		{"git tree that does not resolve", f.g(filepath.Join(f.forged, "m.go")), ""},
		{"a different worktree", f.g(filepath.Join(f.wtB, "b.go")), "worktree-toolpath feature/22-b"},
		{"two worktrees in one message", f.g(filepath.Join(f.wtA, "x.go"), filepath.Join(f.wtB, "y.go")), ""},
		{"worktree plus main checkout in one message", f.g(a, filepath.Join(f.repo, "m.go")), ""},
		{"paths may be incomplete", messageGroup{ID: "msg_x", First: f.at, Last: f.at, Paths: []string{a}, Truncated: true}, ""},
		{"branch unresolvable at the message's time", messageGroup{ID: "msg_x", First: time.Now().Add(-time.Hour), Last: f.at}, ""},
		{"message spans a time the reflog cannot answer", messageGroup{ID: "msg_x", First: f.at, Last: f.at.Add(-time.Hour * 24 * 40)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantVerdicts(t, f.run(t, f.resolver(), f.g(a), tc.trigger, f.g()),
				"worktree-toolpath feature/11-a", tc.want, "")
		})
	}
	// A message whose lines span an hour on one branch still carries.
	wantVerdicts(t, f.run(t, f.resolver(), f.g(a), late), "worktree-toolpath feature/11-a", "carry feature/11-a")
}

// Pins the Foreign verdict: a message whose only non-neutral paths are in a
// worktree of an unconfigured repo is Foreign by tool path and clears the
// carry; mixed with a configured worktree it is today's rule and clears it; a
// cwd in such a worktree is Foreign by cwd, whatever the transcript branch,
// and leaves the carry alone; a network cwd is never looked up.
func TestWorktreeResolver_ForeignWorktreeVerdict(t *testing.T) {
	f := newWTFixture(t)
	a, fw := filepath.Join(f.wtA, "a.go"), filepath.Join(f.foreignWT, "f.go")
	wantVerdicts(t, f.run(t, f.resolver(), f.g(a), f.g(fw, filepath.Join(f.scratch, "n")), f.g()),
		"worktree-toolpath feature/11-a", "worktree-toolpath foreign-repo", "")
	wantVerdicts(t, f.run(t, f.resolver(), f.g(a), f.g(fw, filepath.Join(f.wtA, "b.go")), f.g()),
		"worktree-toolpath feature/11-a", "", "")
	for _, branch := range []string{"feature/44-f", "", "HEAD", "worktree-agent-a1b2c3"} {
		r := f.resolver()
		wantVerdicts(t, []string{
			verdict(t, r.resolve(f.g(a), "", ""), f.repo),
			verdict(t, r.resolve(f.g(), f.foreignWT, branch), f.repo),
			verdict(t, r.resolve(f.g(), "", ""), f.repo),
		}, "worktree-toolpath feature/11-a", "worktree-cwd foreign-repo", "carry feature/11-a")
	}
	if got := verdict(t, f.resolver().resolve(f.g(), "/"+f.foreignWT, "feature/44-f"), f.repo); got != "" {
		t.Errorf("network cwd in the unconfigured worktree = %q, want today's rule", got)
	}
}

// Pins: a configured worktree the message's own tool paths name outranks a cwd
// in an unconfigured repo's worktree, including a monorepo target's worktree
// root, which classifies as foreign because it is above the target directory.
func TestWorktreeResolver_ConfiguredToolPathOutranksForeignCWD(t *testing.T) {
	f := newWTFixture(t)
	got := verdict(t, f.resolver().resolve(f.g(filepath.Join(f.wtA, "a.go")), f.foreignWT, "feature/44-f"), f.repo)
	if got != "worktree-toolpath feature/11-a" {
		t.Errorf("foreign cwd, configured tool path = %q, want %q", got, "worktree-toolpath feature/11-a")
	}
	mono := wtRepo(t, f.base, "mono")
	writeFile(t, filepath.Join(mono, "svcA", "k.txt"), "k\n")
	runGit(t, mono, "add", ".")
	runGit(t, mono, "commit", "--quiet", "-m", "svcA")
	monoWT := addWorktree(t, mono, filepath.Join(f.base, "monoWT"), "feature/77-m")
	svcA := filepath.Join(mono, "svcA")
	r := &worktreeResolver{index: newWorktreeIndex([]string{svcA}, nil), now: f.at.Add(time.Hour)}
	if got := verdict(t, r.resolve(f.g(filepath.Join(monoWT, "svcA", "k.txt")), monoWT, "feature/77-m"), svcA); got != "worktree-toolpath feature/77-m" {
		t.Errorf("monorepo worktree root cwd, target tool path = %q, want %q", got, "worktree-toolpath feature/77-m")
	}
}

// Pins: an index that dropped a configured target (absent when built) books no
// foreign-repo verdict, by tool path or cwd: a worktree outside every indexed
// target may be the dropped target's, so it resets the carry to today's rule.
func TestWorktreeResolver_PartialIndexBooksNoForeignRepo(t *testing.T) {
	f := newWTFixture(t)
	x := newWorktreeIndex([]string{f.repo, filepath.Join(f.base, "not-yet")}, nil)
	r := &worktreeResolver{index: x, now: f.at.Add(time.Hour)}
	wantVerdicts(t, []string{
		verdict(t, r.resolve(f.g(filepath.Join(f.wtA, "a.go")), "", ""), f.repo),
		verdict(t, r.resolve(f.g(filepath.Join(f.foreignWT, "f.go")), "", ""), f.repo),
		verdict(t, r.resolve(f.g(), "", ""), f.repo),
		verdict(t, r.resolve(f.g(), f.foreignWT, "feature/44-f"), f.repo),
	}, "worktree-toolpath feature/11-a", "", "", "")
}

// Pins: a worktree whose .git file another uid owns resets the carry and never
// sets one.
func TestWorktreeResolver_OtherUIDWorktreeResetsAndNeverSets(t *testing.T) {
	f := newWTFixture(t)
	gitFile := filepath.Join(f.wtA, ".git")
	f.x.owned = func(p string, info os.FileInfo) bool { return p != gitFile && ownedBy(info, f.x.uid) }
	a := filepath.Join(f.wtA, "a.go")
	wantVerdicts(t, f.run(t, f.resolver(), f.g(filepath.Join(f.wtB, "b.go")), f.g(a), f.g()),
		"worktree-toolpath feature/22-b", "", "")
	wantVerdicts(t, f.run(t, f.resolver(), f.g(a), f.g()), "", "")
}

// Pins: a relative path, a ".." path and a neutral root neither set nor reset
// the carry.
func TestWorktreeResolver_RelativePathIsNeutral(t *testing.T) {
	f := newWTFixture(t)
	neutral := []string{"repo/m.go", "wtB/b.go", f.repo + "/../wtB/b.go", filepath.Join(f.scratch, "n.go")}
	wantVerdicts(t, f.run(t, f.resolver(), f.g(neutral...), f.g(filepath.Join(f.wtA, "a.go")), f.g(neutral...)),
		"", "worktree-toolpath feature/11-a", "carry feature/11-a")
}

// Pins: a subagent file carries from its own first reference across the whole
// file, however many messages and however long after; nothing before that
// reference is attributed (no parent-at-spawn inheritance).
func TestWorktreeResolver_SubagentCarriesAcrossWholeFile(t *testing.T) {
	f := newWTFixture(t)
	r := f.resolver()
	side := func(at time.Time, paths ...string) messageGroup {
		return messageGroup{ID: "msg_s", Sidechain: true, Agent: "a1", First: at, Last: at, Paths: paths}
	}
	got := f.run(t, r, side(f.at), side(f.at, filepath.Join(f.wtB, "b.go")))
	for i := 0; i < 200; i++ {
		got = append(got, f.run(t, r, side(f.at.Add(time.Duration(i)*time.Minute), filepath.Join(f.scratch, "n")))...)
	}
	want := []string{"", "worktree-toolpath feature/22-b"}
	for len(want) < len(got) {
		want = append(want, "carry feature/22-b")
	}
	wantVerdicts(t, got, want...)
}

// Pins: the resolver never reads a group's stream label; the same sequence
// with every label changed gives the same verdicts.
func TestWorktreeResolver_IgnoresStreamLabel(t *testing.T) {
	f := newWTFixture(t)
	seq := []messageGroup{f.g(filepath.Join(f.wtA, "a.go")), f.g(), f.g(filepath.Join(f.repo, "m.go")), f.g()}
	base := f.run(t, f.resolver(), seq...)
	for i := range seq {
		seq[i].Sidechain, seq[i].Agent = i%2 == 0, fmt.Sprintf("agent-%d", i)
	}
	wantVerdicts(t, f.run(t, f.resolver(), seq...), base...)
	wantVerdicts(t, base, "worktree-toolpath feature/11-a", "carry feature/11-a", "", "")
}

// Pins the join: a group attributes only the usage event with its non-empty
// message id; an ID-less group still moves the carry; a verdict for another
// configured target is kept, naming that target.
func TestWorktreeAttribution_JoinsByMessageIDOnly(t *testing.T) {
	f := newWTFixture(t)
	wa := &worktreeAttribution{index: f.x, now: f.at.Add(time.Hour)}
	g := func(id string, paths ...string) messageGroup {
		return messageGroup{ID: id, First: f.at, Last: f.at, Paths: paths}
	}
	wt2 := addWorktree(t, f.repo2, filepath.Join(f.base, "wt2"), "feature/55-r2")
	got, _ := wa.attribute(
		[]messageGroup{g("", filepath.Join(f.wtA, "a.go")), g("msg_1"), g("msg_2", filepath.Join(wt2, "r.go")), g("msg_3"), g("msg_nousage", filepath.Join(f.wtB, "b.go")), g("msg_4")},
		[]messageUsage{{parseOrder: 0}, {messageID: "msg_1"}, {messageID: "msg_2"}, {messageID: "msg_3"}, {messageID: "msg_4"}},
		pathClassification{},
	)
	want := map[string]string{"msg_1": "carry feature/11-a", "msg_2": "worktree-toolpath feature/55-r2", "msg_3": "", "msg_4": "carry feature/22-b"}
	if len(got) != len(want) {
		t.Errorf("attribution has %d ids, want %d: %+v", len(got), len(want), got)
	}
	for id, w := range want {
		target := f.repo
		if id == "msg_2" {
			target = f.repo2
		}
		if v := verdict(t, got[id], target); v != w {
			t.Errorf("%s = %q, want %q", id, v, w)
		}
	}
}

// Pins: two groups sharing a message id keep the verdict only when they agree,
// in either order.
func TestWorktreeAttribution_DuplicateGroupIDsMustAgree(t *testing.T) {
	f := newWTFixture(t)
	wa := &worktreeAttribution{index: f.x, now: f.at.Add(time.Hour)}
	g := func(paths ...string) messageGroup {
		return messageGroup{ID: "msg_d", First: f.at, Last: f.at, Paths: paths}
	}
	a, b := g(filepath.Join(f.wtA, "a.go")), g(filepath.Join(f.wtB, "b.go"))
	usage := []messageUsage{{messageID: "msg_d"}}
	for name, tc := range map[string]struct {
		groups []messageGroup
		want   string
	}{
		"agree":          {[]messageGroup{a, a}, "worktree-toolpath feature/11-a"},
		"disagree A, B":  {[]messageGroup{a, b}, ""},
		"disagree B, A":  {[]messageGroup{b, a}, ""},
		"one has none":   {[]messageGroup{a, g(filepath.Join(f.repo, "m.go"))}, ""},
		"none then some": {[]messageGroup{g(), a}, ""},
	} {
		got, _ := wa.attribute(tc.groups, usage, pathClassification{})
		if v := verdict(t, got["msg_d"], f.repo); v != tc.want {
			t.Errorf("%s: msg_d = %q, want %q", name, v, tc.want)
		}
	}
}

// Pins: groups resolve in file order, not the merger's finish order. With a
// sidechain interleaved, the main message M1 finishes after S1, whose path sets
// the carry; M1 was written before S1 and must not be attributed by it.
func TestWorktreeAttribution_ResolvesInFileOrder(t *testing.T) {
	f := newWTFixture(t)
	side := func(l string) []byte {
		return []byte(strings.Replace(l, `"isSidechain":false`, `"isSidechain":true`, 1))
	}
	const text = `[{"type":"text","text":"ok"}]`
	var m messageMerger
	m.add([]byte(wtJSONLLine("msg_m1", f.at, f.repo, "main", text, 1)), 10)
	m.add(side(wtJSONLLine("msg_s1", f.at.Add(time.Second), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 1)), 20)
	m.add(side(wtJSONLLine("msg_s2", f.at.Add(2*time.Second), f.repo, "main", text, 1)), 30)
	m.add([]byte(wtJSONLLine("msg_m2", f.at.Add(3*time.Second), f.repo, "main", text, 1)), 40)
	m.flush()
	groups := m.take()
	var order []string
	for _, g := range groups {
		order = append(order, g.ID)
	}
	wantVerdicts(t, order, "msg_s1", "msg_m1", "msg_s2", "msg_m2") // the precondition: finish order
	wa := &worktreeAttribution{index: f.x, now: f.at.Add(time.Hour)}
	got, _ := wa.attribute(groups, []messageUsage{{messageID: "msg_m1"}, {messageID: "msg_s1"}, {messageID: "msg_s2"}, {messageID: "msg_m2"}}, pathClassification{})
	var verdicts []string
	for _, id := range []string{"msg_m1", "msg_s1", "msg_s2", "msg_m2"} {
		verdicts = append(verdicts, verdict(t, got[id], f.repo))
	}
	wantVerdicts(t, verdicts, "", "worktree-toolpath feature/11-a", "carry feature/11-a", "carry feature/11-a")
}

// wtJSONLLine is a Claude Code assistant line; content is a message content
// array.
func wtJSONLLine(id string, at time.Time, cwd, branch, content string, output int) string {
	idField := ""
	if id != "" {
		idField = `"id":"` + id + `",`
	}
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"sess-823","gitBranch":%q,"cwd":%q,`+
		`"isSidechain":false,"message":{%s"model":"claude-sonnet-4","role":"assistant","content":%s,`+
		`"usage":{"input_tokens":10,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		at.UTC().Format(time.RFC3339Nano), branch, cwd, idField, content, output)
}

func wtRead(p string) string {
	return `[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":` + fmt.Sprintf("%q", p) + `}}]`
}

// wtSession writes one transcript exercising every rule and returns a
// collector for f.repo reading it.
func wtSession(t *testing.T, f *wtFixture) *JSONLCollector {
	t.Helper()
	const text = `[{"type":"text","text":"ok"}]`
	at := func(n int) time.Time { return f.at.Add(time.Duration(n) * time.Second) }
	claudeDir := filepath.Join(f.base, "claude")
	projects := filepath.Join(claudeDir, "projects", "p")
	mkdirs(t, projects)
	writeJSONL(t, projects, "s.jsonl", []string{
		// m1: the largest usage is on the first line, its tool call on a later one.
		wtJSONLLine("msg_1", at(1), f.repo, "main", text, 500),
		`{"type":"user","message":{"role":"user","content":"x"}}`,
		wtJSONLLine("msg_1", at(2), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 5),
		wtJSONLLine("msg_2", at(3), f.repo, "main", text, 7),
		wtJSONLLine("msg_3", at(4), f.wtB, "feature/22-b", text, 7),
		wtJSONLLine("msg_4", at(5), f.repo, "main", wtRead(filepath.Join(f.repo, "m.go")), 7),
		wtJSONLLine("", at(6), f.repo, "main", wtRead(filepath.Join(f.wtA, "z.go")), 7),
		wtJSONLLine("msg_6", at(7), f.repo, "main", text, 7),
	})
	// Files with no assistant usage parse to no session.
	writeJSONL(t, projects, "usageless.jsonl", []string{`{"type":"user","message":{"role":"user","content":"x"}}`})
	writeJSONL(t, projects, "empty.jsonl", nil)
	return &JSONLCollector{RepoPath: f.repo, ClaudeDir: claudeDir, DeveloperID: "dev", RepoSlug: "owner/repo"}
}

func wtCollect(t *testing.T, f *wtFixture, c *JSONLCollector) []TokenEvent {
	t.Helper()
	events, err := c.Collect(context.Background(), f.at.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return events
}

func issuesAndRules(events []TokenEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.IssueID+" "+string(e.AttributionRule))
	}
	return out
}

// Pins the stamping end to end, and the enum: with the option on, the four
// rules emitted are exactly the closed set, each spelled as ruled.
func TestJSONLCollector_WorktreeAttributionStampsEachRule(t *testing.T) {
	f := newWTFixture(t)
	c := wtSession(t, f)
	c.worktrees = f.x
	events := wtCollect(t, f, c)
	wantVerdicts(t, issuesAndRules(events),
		"issue-11 worktree-toolpath", // the tool call arrived on a later line of msg_1
		"issue-11 carry",
		"issue-22 worktree-cwd",
		"unattributed:main branch", // the main checkout reset the carry
		"unattributed:main branch", // ID-less: attributes nothing, sets the carry
		"issue-11 carry",
	)
	seen := map[store.AttributionRule]bool{}
	for _, e := range events {
		seen[e.AttributionRule] = true
	}
	wantSet := []store.AttributionRule{"branch", "worktree-cwd", "worktree-toolpath", "carry"}
	if len(seen) != len(wantSet) || !reflect.DeepEqual(wantSet, store.AttributionRules()) {
		t.Errorf("rules emitted %v, want exactly %v (store.AttributionRules() = %v)", seen, wantSet, store.AttributionRules())
	}
	for _, r := range wantSet {
		if !seen[r] {
			t.Errorf("rule %q never emitted", r)
		}
	}
	if events[0].OutputTok != 500 {
		t.Errorf("msg_1 OutputTok = %d, want 500 (the largest-usage line)", events[0].OutputTok)
	}
}

// wtCollector returns a collector for f.repo over a projects dir holding files,
// each a relative path and its lines.
func wtCollector(t *testing.T, f *wtFixture, name string, files map[string][]string) *JSONLCollector {
	t.Helper()
	claudeDir := filepath.Join(f.base, name)
	for rel, lines := range files {
		p := filepath.Join(claudeDir, "projects", "p", rel)
		mkdirs(t, filepath.Dir(p))
		writeJSONL(t, filepath.Dir(p), filepath.Base(p), lines)
	}
	return &JSONLCollector{RepoPath: f.repo, ClaudeDir: claudeDir, DeveloperID: "dev", RepoSlug: "owner/repo"}
}

// byOutput renders events as "<issue> <rule>" keyed by OutputTok.
func byOutput(events []TokenEvent) map[int]string {
	out := map[int]string{}
	for _, e := range events {
		out[e.OutputTok] = e.IssueID + " " + string(e.AttributionRule)
	}
	return out
}

// Pins: a reflog branch that is a harness worktree name is no verdict; the
// message and the path-less one after it get today's rule (#490 inheritance),
// with the option on exactly as off.
func TestJSONLCollector_WorktreeAttributionHarnessBranchIsTodaysRule(t *testing.T) {
	f := newWTFixture(t)
	h := addWorktree(t, f.repo, filepath.Join(f.base, "harness"), "worktree-agent-a1b2c3")
	const text = `[{"type":"text","text":"ok"}]`
	at := func(n int) time.Time { return f.at.Add(time.Duration(n) * time.Second) }
	c := wtCollector(t, f, "claude-harness", map[string][]string{"s.jsonl": {
		wtJSONLLine("msg_1", at(1), f.repo, "feature/77-parent", text, 1),
		wtJSONLLine("msg_2", at(2), h, "worktree-agent-a1b2c3", wtRead(filepath.Join(h, "a.go")), 2),
		wtJSONLLine("msg_3", at(3), f.repo, "feature/77-parent", text, 3),
	}})
	off := byOutput(wtCollect(t, f, c))
	want := map[int]string{1: "issue-77 ", 2: "issue-77 ", 3: "issue-77 "}
	if !reflect.DeepEqual(off, want) {
		t.Fatalf("option off = %v, want %v", off, want)
	}
	c.worktrees = f.x
	want = map[int]string{1: "issue-77 branch", 2: "issue-77 branch", 3: "issue-77 branch"}
	if on := byOutput(wtCollect(t, f, c)); !reflect.DeepEqual(on, want) {
		t.Errorf("option on = %v, want %v", on, want)
	}
}

// Pins: the carry is per file; a subagent file does not inherit the parent
// file's carry and carries only from its own first reference.
func TestJSONLCollector_WorktreeAttributionSubagentFileHasOwnCarry(t *testing.T) {
	f := newWTFixture(t)
	const text = `[{"type":"text","text":"ok"}]`
	at := func(n int) time.Time { return f.at.Add(time.Duration(n) * time.Second) }
	c := wtCollector(t, f, "claude-subagent", map[string][]string{
		"s.jsonl": {
			wtJSONLLine("msg_p1", at(1), f.repo, "main", wtRead(filepath.Join(f.wtA, "a.go")), 1),
			wtJSONLLine("msg_p2", at(2), f.repo, "main", text, 2),
		},
		filepath.Join("sess-823", "subagents", "agent-x.jsonl"): {
			wtJSONLLine("msg_s1", at(3), f.repo, "main", text, 3),
			wtJSONLLine("msg_s2", at(4), f.repo, "main", wtRead(filepath.Join(f.wtB, "b.go")), 4),
			wtJSONLLine("msg_s3", at(5), f.repo, "main", text, 5),
		},
	})
	c.worktrees = f.x
	want := map[int]string{
		1: "issue-11 worktree-toolpath", 2: "issue-11 carry",
		3: "unattributed:main branch", 4: "issue-22 worktree-toolpath", 5: "issue-22 carry",
	}
	if got := byOutput(wtCollect(t, f, c)); !reflect.DeepEqual(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
}

// Pins: a session ending before since is not attributed, so no path of a file
// the caller drops is looked up.
func TestParseSessionFileAttributed_SkipsSessionsBeforeSince(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	path := writeJSONL(t, t.TempDir(), "s.jsonl", []string{wtJSONLLine("msg_1", at, "/r", "main", `[{"type":"text","text":"a"}]`, 1)})
	wa := &worktreeAttribution{index: newWorktreeIndex(nil, nil), now: at}
	for _, tc := range []struct {
		since      time.Time
		attributed bool
	}{{at.Add(-time.Hour), true}, {at.Add(time.Hour), false}} {
		s, err := parseSessionFileAttributed(path, tc.since, wa)
		if err != nil || s == nil {
			t.Fatalf("since %v: %v, %v", tc.since, s, err)
		}
		if got := s.attribution != nil; got != tc.attributed {
			t.Errorf("since %v: attributed %v, want %v", tc.since, got, tc.attributed)
		}
	}
}

// Pins: with the option off (the zero JSONLCollector), output is today's: no
// rule on any event, and every issue from the transcript branch. With it on,
// only IssueID and AttributionRule may differ.
func TestJSONLCollector_WorktreeAttributionOffIsTodaysOutput(t *testing.T) {
	f := newWTFixture(t)
	off := wtCollect(t, f, wtSession(t, f))
	wantVerdicts(t, issuesAndRules(off),
		"unattributed:main ", "unattributed:main ", "issue-22 ", "unattributed:main ", "unattributed:main ", "unattributed:main ")
	c := wtSession(t, f)
	c.worktrees = f.x
	on := wtCollect(t, f, c)
	if len(on) != len(off) {
		t.Fatalf("on %d events, off %d", len(on), len(off))
	}
	for i := range on {
		on[i].IssueID, on[i].AttributionRule = off[i].IssueID, off[i].AttributionRule
		if on[i] != off[i] {
			t.Errorf("event %d differs beyond IssueID and AttributionRule:\n on %+v\noff %+v", i, on[i], off[i])
		}
	}
}

// Pins: the parse feeds the merger every scanned line at its byte offset in
// the file, CRLF and blank lines included, also when it starts mid-file.
func TestParseSessionLines_FeedsMergerAtFileOffsets(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	l1 := wtJSONLLine("msg_1", at, "/r", "main", `[{"type":"text","text":"a"}]`, 1)
	l2 := wtJSONLLine("msg_2", at.Add(time.Second), "/r", "main", `[{"type":"text","text":"b"}]`, 1)
	l3 := wtJSONLLine("msg_3", at.Add(2*time.Second), "/r", "main", `[{"type":"text","text":"c"}]`, 1)
	body := l1 + "\r\n\n" + l2 + "\n" + l3 + "\n"
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	off2 := int64(len(l1) + 3)
	off3 := off2 + int64(len(l2)+1)
	for _, tc := range []struct {
		from int64
		want map[string]int64
	}{
		{0, map[string]int64{"msg_1": 0, "msg_2": off2, "msg_3": off3}},
		{off2, map[string]int64{"msg_2": off2, "msg_3": off3}},
	} {
		var m messageMerger
		if _, _, err := parseSessionLines(path, tc.from, sessionMetadata{}, false, &m, 0); err != nil {
			t.Fatal(err)
		}
		m.flush()
		got := map[string]int64{}
		for _, g := range m.take() {
			got[g.ID] = g.Start
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("from %d: starts %v, want %v", tc.from, got, tc.want)
		}
	}
}
