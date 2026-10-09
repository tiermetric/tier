package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Hand-written object ids, so every reflog here is deterministic.
const (
	oidZero = "0000000000000000000000000000000000000000"
	oidA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oidC    = "cccccccccccccccccccccccccccccccccccccccc"
	oidD    = "dddddddddddddddddddddddddddddddddddddddd"
)

// rlLine is one reflog line in git's format; an empty msg writes no tab, as
// `git worktree add` does for its first entry.
func rlLine(old, new string, ts int64, msg string) string {
	line := old + " " + new + " Test User <test@example.com> " + strconv.FormatInt(ts, 10) + " -0400"
	if msg != "" {
		line += "\t" + msg
	}
	return line + "\n"
}

// historyLog is a worktree created on feature/823-x at 1000, committed on at
// 1010, switched to other at 1020, committed on at 1030 and switched back at
// 1040. Its HEAD file is historyHead.
var historyLog = rlLine(oidZero, oidA, 1000, "") +
	rlLine(oidA, oidA, 1000, "reset: moving to HEAD") +
	rlLine(oidA, oidB, 1010, "commit: two") +
	rlLine(oidB, oidB, 1020, "checkout: moving from feature/823-x to other") +
	rlLine(oidB, oidC, 1030, "commit: three") +
	rlLine(oidC, oidC, 1040, "checkout: moving from other to feature/823-x")

const historyHead = "ref: refs/heads/feature/823-x\n"

// reflogNow is the clock every hand-written fixture is read at: far enough
// past its entries that none is beyond git's expiry horizon.
var reflogNow = sec(10000)

// reflogAdmin writes an admin dir holding head as HEAD, log as logs/HEAD and a
// gitdir, and returns an index and a Linked classification naming it. As git
// leaves them, HEAD and logs/HEAD carry the newest entry's time and gitdir the
// first's.
func reflogAdmin(t *testing.T, head, log string) (*worktreeIndex, pathClassification, string) {
	t.Helper()
	first, newest := sec(1000), sec(1000)
	if es, ok := parseReflog(log); ok {
		first, newest = sec(es[0].ts), sec(es[len(es)-1].ts)
	}
	return reflogAdminAt(t, head, log, newest, newest, first)
}

// reflogAdminAt is reflogAdmin with the mtimes of HEAD, logs/HEAD and gitdir
// given.
func reflogAdminAt(t *testing.T, head, log string, headM, logM, gitdirM time.Time) (*worktreeIndex, pathClassification, string) {
	t.Helper()
	needOwner(t)
	admin := filepath.Join(canonPath(t, t.TempDir()), "worktrees", "wt")
	for name, body := range map[string]string{"HEAD": head, filepath.Join("logs", "HEAD"): log, "gitdir": "/repo-wt/.git\n"} {
		writeFile(t, filepath.Join(admin, name), body)
	}
	for name, m := range map[string]time.Time{"HEAD": headM, filepath.Join("logs", "HEAD"): logM, "gitdir": gitdirM} {
		if err := os.Chtimes(filepath.Join(admin, name), m, m); err != nil {
			t.Fatal(err)
		}
	}
	c := pathClassification{Class: worktreeLinked, Target: "repo", Root: "/repo-wt", AdminDir: admin}
	return newWorktreeIndex(nil, nil), c, admin
}

func sec(s int64) time.Time { return time.Unix(s, 0) }

// fsec is a fractional unix time, as stat and the capture probes print it.
func fsec(f float64) time.Time { return time.Unix(0, int64(f*1e9)) }

func wantBranchAt(t *testing.T, x *worktreeIndex, c pathClassification, at time.Time, want string) {
	t.Helper()
	got, ok := x.branchAt(c, at, reflogNow)
	if want == "" {
		if ok {
			t.Errorf("branchAt(%v) = %q, want unresolvable", at.Unix(), got)
		}
		return
	}
	if !ok || got != want {
		t.Errorf("branchAt(%v) = %q, %v; want %q", at.Unix(), got, ok, want)
	}
}

// Pins: the branch between entries comes from the next checkout's "from" or,
// after the last entry, from HEAD; t before the first entry, or inside the
// first entry's second, is unresolvable.
func TestBranchAt_History(t *testing.T) {
	x, c, _ := reflogAdmin(t, historyHead, historyLog)
	wantBranchAt(t, x, c, sec(999), "")
	wantBranchAt(t, x, c, sec(1000).Add(500*time.Millisecond), "")
	wantBranchAt(t, x, c, sec(1001), "feature/823-x")
	wantBranchAt(t, x, c, sec(1019), "feature/823-x")
	wantBranchAt(t, x, c, sec(1021), "other")
	wantBranchAt(t, x, c, sec(1039), "other")
	wantBranchAt(t, x, c, sec(1041), "feature/823-x")
	wantBranchAt(t, x, c, sec(5000), "feature/823-x")
}

// Pins the boundary: reflog times are whole seconds, so a checkout in t's own
// second may be before or after t and the answer is unresolvable, while an
// entry that keeps HEAD on its branch (a commit) in that second is not.
func TestBranchAt_SameSecondBoundary(t *testing.T) {
	x, c, _ := reflogAdmin(t, historyHead, historyLog)
	wantBranchAt(t, x, c, sec(1020), "")
	wantBranchAt(t, x, c, sec(1020).Add(999*time.Millisecond), "")
	wantBranchAt(t, x, c, sec(1040), "")
	wantBranchAt(t, x, c, sec(1010), "feature/823-x")
	wantBranchAt(t, x, c, sec(1030).Add(time.Millisecond), "other")
}

// Pins: a checkout of a tag, a commit, or a branch under --detach is named by
// "to" like a branch, but the next checkout's "from" is an object id, so that
// interval is unresolvable; and a HEAD detached now leaves the last one so.
func TestBranchAt_DetachedHead(t *testing.T) {
	log := rlLine(oidZero, oidA, 1000, "") +
		rlLine(oidA, oidA, 1010, "checkout: moving from main to v1.0") +
		rlLine(oidA, oidB, 1020, "commit: detached work") +
		rlLine(oidB, oidA, 1030, "checkout: moving from "+oidB+" to main") +
		rlLine(oidA, oidA, 1040, "checkout: moving from main to main") +
		rlLine(oidA, oidA, 1050, "checkout: moving from "+oidA+" to main")
	x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", log)
	wantBranchAt(t, x, c, sec(1005), "main")
	wantBranchAt(t, x, c, sec(1015), "")
	wantBranchAt(t, x, c, sec(1025), "")
	wantBranchAt(t, x, c, sec(1035), "main")
	wantBranchAt(t, x, c, sec(1045), "") // `git switch --detach main`
	wantBranchAt(t, x, c, sec(1055), "main")

	x, c, _ = reflogAdmin(t, oidA+"\n", rlLine(oidZero, oidA, 1000, "")+rlLine(oidA, oidA, 1010, "checkout: moving from main to "+oidA))
	wantBranchAt(t, x, c, sec(1005), "main")
	wantBranchAt(t, x, c, sec(1015), "")

	// No checkout names the interval, so only the "from" object id marks it.
	x, c, _ = reflogAdmin(t, "ref: refs/heads/main\n", rlLine(oidZero, oidA, 1000, "")+
		rlLine(oidA, oidB, 1010, "rebase (start): checkout main")+
		rlLine(oidB, oidB, 1020, "checkout: moving from "+oidB+" to main"))
	wantBranchAt(t, x, c, sec(1015), "")
	wantBranchAt(t, x, c, sec(1025), "main")
}

// Pins: during a rebase HEAD is detached; its finish names the branch it
// returns to; a reset or amend keeps HEAD on its branch; and the interval
// before a rebase start, which records no "from", is unresolvable.
func TestBranchAt_RebaseAndReset(t *testing.T) {
	log := rlLine(oidZero, oidA, 1000, "") +
		rlLine(oidA, oidB, 1010, "commit: one") +
		rlLine(oidB, oidC, 1020, "rebase (start): checkout main") +
		rlLine(oidC, oidD, 1021, "rebase (pick): one") +
		rlLine(oidD, oidD, 1022, "rebase (finish): returning to refs/heads/topic") +
		rlLine(oidD, oidA, 1030, "reset: moving to HEAD~1") +
		rlLine(oidA, oidB, 1040, "commit (amend): one again") +
		rlLine(oidB, oidB, 1050, "checkout: moving from topic to main")
	x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", log)
	wantBranchAt(t, x, c, sec(1015), "")
	wantBranchAt(t, x, c, sec(1021).Add(time.Millisecond*500), "")
	wantBranchAt(t, x, c, sec(1025), "topic")
	wantBranchAt(t, x, c, sec(1035), "topic")
	wantBranchAt(t, x, c, sec(1045), "topic")
	wantBranchAt(t, x, c, sec(1055), "main")
}

// Pins: the finish of a rebase and the next checkout's "from" must agree, and
// so must a checkout's "to" and HEAD, and a finish and HEAD; a HEAD detached
// now overrides a finish; any disagreement is unresolvable.
func TestBranchAt_SourcesMustAgree(t *testing.T) {
	log := rlLine(oidZero, oidA, 1000, "") +
		rlLine(oidA, oidA, 1010, "rebase (finish): returning to refs/heads/topic") +
		rlLine(oidA, oidA, 1020, "checkout: moving from elsewhere to main")
	x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", log)
	wantBranchAt(t, x, c, sec(1015), "")
	wantBranchAt(t, x, c, sec(1025), "main")

	x, c, _ = reflogAdmin(t, "ref: refs/heads/renamed\n", log)
	wantBranchAt(t, x, c, sec(1025), "")

	finished := rlLine(oidZero, oidA, 1000, "") + rlLine(oidA, oidA, 1010, "rebase (finish): returning to refs/heads/topic")
	x, c, _ = reflogAdmin(t, "ref: refs/heads/topic\n", finished)
	wantBranchAt(t, x, c, sec(1015), "topic")
	x, c, _ = reflogAdmin(t, oidA+"\n", finished) // detached since, with no entry
	wantBranchAt(t, x, c, sec(1015), "")
}

// Pins: an entry not known to keep HEAD's branch (here a branch rename) ends
// what an earlier entry proved, and a gap in the old/new chain (an expired
// entry) could hide a checkout, so both are unresolvable.
func TestBranchAt_UnknownEntryAndGap(t *testing.T) {
	log := rlLine(oidZero, oidA, 1000, "") +
		rlLine(oidA, oidA, 1010, "rebase (finish): returning to refs/heads/topic") +
		rlLine(oidA, oidA, 1020, "Branch: renamed refs/heads/topic to refs/heads/main")
	x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", log)
	wantBranchAt(t, x, c, sec(1015), "topic")
	wantBranchAt(t, x, c, sec(1025), "main")

	gap := rlLine(oidZero, oidA, 1000, "") +
		rlLine(oidA, oidB, 1010, "commit: one") +
		rlLine(oidC, oidD, 1020, "commit: two") +
		rlLine(oidD, oidD, 1030, "checkout: moving from topic to main")
	x, c, _ = reflogAdmin(t, "ref: refs/heads/main\n", gap)
	wantBranchAt(t, x, c, sec(1005), "")
	wantBranchAt(t, x, c, sec(1025), "")
	wantBranchAt(t, x, c, sec(1035), "main")
}

// Pins: exactly these messages keep HEAD on its branch across them, so a
// rebase finish still names the interval after them; a pull, a rebase step or
// a rename is not known to, and ends what the finish proved.
func TestBranchAt_PreservingEntries(t *testing.T) {
	cases := map[string]string{
		"commit: x":                  "topic",
		"commit (amend): x":          "topic",
		"reset: moving to HEAD~1":    "topic",
		"merge topic2: Fast-forward": "topic",
		"cherry-pick: x":             "topic",
		"pull: Fast-forward":         "topic",
		"revert: Revert \"x\"":       "topic",
		"am: x":                      "topic",
		"pull -q origin main: Merge made by the 'ort' strategy.": "topic",
		"rebase (pick): x":                                  "",
		"rebase (pick): commit: x":                          "",
		"pull --rebase (pick): Fast-forward":                "",
		"merge topic2":                                      "",
		"merge topic2: x":                                   "",
		"reset: updating HEAD":                              "",
		"Branch: renamed refs/heads/topic to refs/heads/t2": "",
	}
	for msg, want := range cases {
		log := rlLine(oidZero, oidA, 1000, "") +
			rlLine(oidA, oidA, 1010, "rebase (finish): returning to refs/heads/topic") +
			rlLine(oidA, oidB, 1020, msg) +
			rlLine(oidB, oidB, 1030, "Branch: renamed refs/heads/topic to refs/heads/main")
		x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", log)
		t.Run(msg, func(t *testing.T) { wantBranchAt(t, x, c, sec(1025), want) })
	}
}

// Pins: a branch name git allows (slashes, dots, non-ASCII) resolves as
// written; one with a space, which git never writes, is unresolvable wherever
// it appears; and git's "(invalid)" is not a branch.
func TestBranchAt_BranchNameText(t *testing.T) {
	odd := "feature/823-ü.x_y+z"
	x, c, _ := reflogAdmin(t, "ref: refs/heads/"+odd+"\n",
		rlLine(oidZero, oidA, 1000, "")+rlLine(oidA, oidA, 1010, "checkout: moving from main to "+odd))
	wantBranchAt(t, x, c, sec(1005), "main")
	wantBranchAt(t, x, c, sec(1015), odd)

	x, c, _ = reflogAdmin(t, "ref: refs/heads/other\n",
		rlLine(oidZero, oidA, 1000, "")+rlLine(oidA, oidA, 1010, "checkout: moving from my branch to main")+
			rlLine(oidA, oidA, 1020, "checkout: moving from main to other"))
	wantBranchAt(t, x, c, sec(1005), "")
	wantBranchAt(t, x, c, sec(1015), "main")
	wantBranchAt(t, x, c, sec(1025), "other")

	x, c, _ = reflogAdmin(t, "ref: refs/heads/main\n",
		rlLine(oidZero, oidA, 1000, "")+rlLine(oidA, oidA, 1010, "checkout: moving from (invalid) to main"))
	wantBranchAt(t, x, c, sec(1005), "")
	wantBranchAt(t, x, c, sec(1015), "main")

	x, c, _ = reflogAdmin(t, "ref: refs/heads/good\n", rlLine(oidZero, oidA, 1000, ""))
	wantBranchAt(t, x, c, sec(1005), "good")
	for _, bad := range []string{"my branch", "a..b", "a~1", "a^", "a:b", "a?", "a*", "a[", "a\\b", "-x", "HEAD", "a@{1}", "x\x7f", "x\ty"} {
		x, c, _ = reflogAdmin(t, "ref: refs/heads/"+bad+"\n", rlLine(oidZero, oidA, 1000, ""))
		wantBranchAt(t, x, c, sec(1005), "")
	}
}

// Pins: a reflog git did not write fails as a whole: CRLF line ends, a final
// line cut short, a time going backwards or not a number, a header short of a
// field, a bad or mixed-length object id or zone, and an empty file. The same history with git's own line ends resolves.
func TestBranchAt_MalformedReflog(t *testing.T) {
	cases := map[string]string{
		"crlf":             strings.ReplaceAll(historyLog, "\n", "\r\n"),
		"cr in a message":  strings.Replace(historyLog, "commit: two\n", "commit: two\r\n", 1),
		"non-hex oid":      historyLog + rlLine(oidC, strings.Repeat("z", 40), 1050, "commit: x"),
		"truncated final":  strings.TrimSuffix(historyLog, "\n"),
		"truncated mid":    historyLog[:len(historyLog)-30],
		"time backwards":   historyLog + rlLine(oidC, oidC, 1039, "commit: late"),
		"short oid":        historyLog + strings.Replace(rlLine(oidC, oidC, 1050, "commit: x"), oidC+" ", "cccc ", 1),
		"bad zone":         strings.Replace(historyLog, "-0400", "EST", 1),
		"non-numeric time": strings.Replace(historyLog, " 1000 ", " 1o00 ", 1),
		"short header":     historyLog + oidC + " " + oidC + " 1050 -0400\tcommit: x\n",
		"mixed oid length": historyLog + rlLine(oidC, strings.Repeat("c", 64), 1050, "commit: x"),
		"empty":            "",
	}
	for name, log := range cases {
		x, c, _ := reflogAdmin(t, historyHead, log)
		if got, ok := x.branchAt(c, sec(1025), reflogNow); ok {
			t.Errorf("%s: branchAt = %q, want unresolvable", name, got)
		}
	}
	x, c, _ := reflogAdmin(t, historyHead, historyLog)
	wantBranchAt(t, x, c, sec(1025), "other")
}

// Pins: a reflog of exactly maxReflogFile bytes resolves, and one byte more is
// refused, never read in part.
func TestBranchAt_OversizedReflog(t *testing.T) {
	build := func(size int) string {
		var b strings.Builder
		b.WriteString(rlLine(oidZero, oidA, 1000, "") + rlLine(oidA, oidA, 1010, "checkout: moving from main to topic"))
		filler := rlLine(oidA, oidA, 1030, "commit: filler")
		for b.Len()+2*len(filler) <= size {
			b.WriteString(filler)
		}
		last := rlLine(oidA, oidA, 1030, "commit: ")
		b.WriteString(strings.Replace(last, "commit: ", "commit: "+strings.Repeat("p", size-b.Len()-len(last)), 1))
		if b.Len() != size {
			t.Fatalf("fixture is %d bytes, want %d", b.Len(), size)
		}
		return b.String()
	}
	x, c, _ := reflogAdmin(t, "ref: refs/heads/topic\n", build(1<<20))
	wantBranchAt(t, x, c, sec(1040), "topic")
	x, c, _ = reflogAdmin(t, "ref: refs/heads/topic\n", build(1<<20+1))
	wantBranchAt(t, x, c, sec(1040), "")
	// Its first maxReflogFile bytes alone would parse and resolve.
	x, c, _ = reflogAdmin(t, "ref: refs/heads/topic\n", build(1<<20)+"\n")
	wantBranchAt(t, x, c, sec(1040), "")
}

// Pins: a missing reflog or HEAD, a symlinked logs/HEAD, logs dir or HEAD
// (each pointing at a healthy file), and a class other than Linked are all
// unresolvable.
func TestBranchAt_FilesMustBeTheWorktreesOwn(t *testing.T) {
	x, c, admin := reflogAdmin(t, historyHead, historyLog)
	wantBranchAt(t, x, c, sec(1025), "other") // control

	for _, cls := range []worktreeClass{worktreeMainCheckout, worktreeForeignWorktree, worktreeUnresolvable} {
		c2 := c
		c2.Class = cls
		wantBranchAt(t, x, c2, sec(1025), "")
	}
	t.Chdir(filepath.Dir(filepath.Dir(admin))) // so the relative dir names a real one
	rel := c
	rel.AdminDir = filepath.Join("worktrees", "wt")
	wantBranchAt(t, x, rel, sec(1025), "")

	elsewhere := canonPath(t, t.TempDir())
	writeFile(t, filepath.Join(elsewhere, "logs", "HEAD"), historyLog)
	writeFile(t, filepath.Join(elsewhere, "HEAD"), historyHead)

	swap := func(name, target string) {
		t.Helper()
		p := filepath.Join(admin, name)
		saved := p + ".saved"
		if err := os.Rename(p, saved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		wantBranchAt(t, x, c, sec(1025), "")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(saved, p); err != nil {
			t.Fatal(err)
		}
		wantBranchAt(t, x, c, sec(1025), "other")
	}
	swap(filepath.Join("logs", "HEAD"), filepath.Join(elsewhere, "logs", "HEAD"))
	swap("logs", filepath.Join(elsewhere, "logs"))
	swap("HEAD", filepath.Join(elsewhere, "HEAD"))

	if err := os.Remove(filepath.Join(admin, "logs", "HEAD")); err != nil {
		t.Fatal(err)
	}
	wantBranchAt(t, x, c, sec(1025), "")
	x, c, admin = reflogAdmin(t, historyHead, historyLog)
	if err := os.Remove(filepath.Join(admin, "HEAD")); err != nil {
		t.Fatal(err)
	}
	wantBranchAt(t, x, c, sec(1025), "")
}

// Pins: logs/HEAD, the logs dir or HEAD owned by another uid is unresolvable,
// each on its own, through the index's owner check.
func TestBranchAt_OtherOwner(t *testing.T) {
	for _, name := range []string{filepath.Join("logs", "HEAD"), "logs", "HEAD"} {
		x, c, admin := reflogAdmin(t, historyHead, historyLog)
		foreign := filepath.Join(admin, name)
		x.owned = func(p string, info os.FileInfo) bool { return p != foreign && ownedBy(info, x.uid) }
		wantBranchAt(t, x, c, sec(1025), "")
		x.owned = func(_ string, info os.FileInfo) bool { return ownedBy(info, x.uid) }
		wantBranchAt(t, x, c, sec(1025), "other")
	}
}

// reflogCapture is a linked worktree's admin dir as real git 2.55 left it:
// testdata/reflog/<name>.log is its logs/HEAD, and HEAD's text and the three
// mtimes are as stat read them.
type reflogCapture struct {
	name, head           string
	headM, logM, gitdirM float64
}

// reflogProbe is a time the capture script sampled, the branch git reported
// then ("" when detached), and what branchAt must answer.
type reflogProbe struct {
	at          float64
	truth, want string
}

// replayCapture checks every probe against the capture read at now. A want
// must be the truth or unresolvable, so the table itself cannot pin a wrong
// branch.
func replayCapture(t *testing.T, rc reflogCapture, now time.Time, probes []reflogProbe) {
	t.Helper()
	log, err := os.ReadFile(filepath.Join("testdata", "reflog", rc.name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	x, c, _ := reflogAdminAt(t, rc.head, string(log), fsec(rc.headM), fsec(rc.logM), fsec(rc.gitdirM))
	for _, p := range probes {
		if p.want != "" && p.want != p.truth {
			t.Fatalf("%s: probe %.3f wants %q but git reported %q", rc.name, p.at, p.want, p.truth)
		}
		got, ok := x.branchAt(c, fsec(p.at), now)
		if !ok {
			got = ""
		}
		if got != p.want {
			t.Errorf("%s: branchAt(%.3f) = %q, want %q (git reported %q)", rc.name, p.at, got, p.want, p.truth)
		}
	}
}

// Pins: a commit subject that reads like a rebase finish is not one. Real git:
// `git rebase -i` stopped with `edit` on a commit whose subject is "docs:
// rebase (finish): returning to refs/heads/evil" logs that text after
// "rebase (edit): ", and HEAD is detached until the real finish.
func TestBranchAt_FinishTextInSubject(t *testing.T) {
	rc := reflogCapture{"finish_in_subject", "ref: refs/heads/other\n", 1790703867.102, 1790703867.103, 1790703849.079}
	replayCapture(t, rc, fsec(1790703930), []reflogProbe{
		{1790703850.234, "feat", ""}, // before a rebase start, which names nothing
		{1790703856.970, "feat", ""},
		{1790703859.226, "", ""}, // stopped at the edit
		{1790703861.470, "", ""},
		{1790703863.724, "feat", "feat"},
		{1790703865.978, "feat", "feat"},
		{1790703868.210, "other", "other"},
	})
}

// Pins: no time older than git's default unreachable-expiry is answered.
// Real git: feat, a detour to otherfeat and back, otherfeat deleted, then
// `git reflog expire --expire-unreachable=now --all` dropped the detour and
// left a chain that still links.
func TestBranchAt_ExpiredDetour(t *testing.T) {
	rc := reflogCapture{"expired_detour", "ref: refs/heads/done-wt\n", 1790703965.316, 1790703967.589, 1790703951.666}
	gc := fsec(1790703967.589)
	replayCapture(t, rc, gc.Add(reflogExpireUnreachable+24*time.Hour), []reflogProbe{
		{1790703952.801, "feat", ""},
		{1790703957.377, "otherfeat", ""},
		{1790703959.651, "otherfeat", ""},
		{1790703966.428, "done-wt", ""},
	})
	replayCapture(t, rc, gc.Add(reflogExpireUnreachable-24*time.Hour), []reflogProbe{
		{1790703952.801, "feat", "feat"},
		{1790703966.428, "done-wt", "done-wt"},
	})
}

// Pins: HEAD written after the newest entry cannot name the last interval.
// Real git: `git switch --orphan gh-pages` rewrote HEAD and logged nothing.
func TestBranchAt_OrphanSwitchLast(t *testing.T) {
	rc := reflogCapture{"orphan_last", "ref: refs/heads/gh-pages\n", 1790704208.087, 1790704205.831, 1790704203.504}
	replayCapture(t, rc, fsec(1790704270), []reflogProbe{
		{1790704204.670, "feat", ""},
		{1790704206.954, "feat", ""},
		{1790704209.195, "gh-pages", ""},
	})
}

// Pins: HEAD written after logs/HEAD's own last write cannot name the tail,
// even inside the newest entry's second. Real git: `git commit -q -m c1 &&
// git switch -q --orphan docs` in one second; HEAD's mtime is 12ms after the
// reflog's and before the commit entry's second ends.
func TestBranchAt_OrphanSwitchSameSecond(t *testing.T) {
	rc := reflogCapture{"orphan_same_second", "ref: refs/heads/docs\n", 1790708769.107, 1790708769.095, 1790708766.832}
	replayCapture(t, rc, fsec(1790708830), []reflogProbe{
		{1790708767.958, "feat", ""},
		{1790708770.218, "docs", ""},
	})
}

// Pins: the gap check reaches the next target-setting entry itself. Real git:
// `git checkout --orphan orph` logged nothing, and the later switch logged
// "moving from orph" with old 0000..., so orph names no earlier interval.
func TestBranchAt_OrphanGapAtNextCheckout(t *testing.T) {
	rc := reflogCapture{"orphan_gap", "ref: refs/heads/other\n", 1790706473.517, 1790706475.765, 1790706464.501}
	replayCapture(t, rc, fsec(1790706540), []reflogProbe{
		{1790706465.681, "feat", ""},
		{1790706467.934, "feat", ""},
		{1790706470.167, "orph", ""},
		{1790706474.628, "other", "other"},
		{1790706476.880, "other", "other"},
	})
}

// Pins: a bare "merge <name>" is not a merge. Real git: a post-merge hook ran
// `git checkout side9`, which logged merge's GIT_REFLOG_ACTION verbatim.
func TestBranchAt_MergeHookCheckout(t *testing.T) {
	rc := reflogCapture{"merge_hook", "ref: refs/heads/r3\n", 1790706482.913, 1790706486.963, 1790706478.287}
	replayCapture(t, rc, fsec(1790706550), []reflogProbe{
		{1790706479.416, "r3", ""},
		{1790706481.795, "side9", "side9"}, // named by the next checkout's "from"
		{1790706485.830, "r3", "r3"},
		{1790706488.079, "r3", "r3"},
	})
}

// Pins: a reflog whose newest entry is later than its own mtime is refused
// whole. Real git: `GIT_COMMITTER_DATE=2030-01-01T00:00:00Z git checkout -b b1`.
func TestBranchAt_FutureDatedEntry(t *testing.T) {
	rc := reflogCapture{"future_dated", "ref: refs/heads/b1\n", 1790706493.891, 1790706493.891, 1790706489.388}
	replayCapture(t, rc, fsec(1790706560), []reflogProbe{
		{1790706492.767, "a2", ""},
		{1790706495.005, "b1", ""},
	})
}

// Pins: a pull that merges, cherry-pick, revert, am, reset and a merge keep
// HEAD on its branch. Real git, one worktree on feat throughout.
func TestBranchAt_PreservingFormsFromGit(t *testing.T) {
	rc := reflogCapture{"preserving_forms", "ref: refs/heads/other\n", 1790704175.008, 1790704175.008, 1790704151.974}
	var probes []reflogProbe
	for _, at := range []float64{1790704153.126, 1790704157.687, 1790704162.280, 1790704164.533, 1790704166.841, 1790704173.880} {
		probes = append(probes, reflogProbe{at, "feat", "feat"})
	}
	replayCapture(t, rc, fsec(1790704240), append(probes, reflogProbe{1790704176.121, "other", "other"}))
}

// Pins: t before gitdir was last written is unresolvable. Real git:
// `git worktree move` rewrote gitdir and logged nothing.
func TestBranchAt_BeforeWorktreeMove(t *testing.T) {
	rc := reflogCapture{"worktree_move", "ref: refs/heads/feat-new\n", 1790704107.759, 1790704116.326, 1790704114.061}
	replayCapture(t, rc, fsec(1790704180), []reflogProbe{
		{1790704089.558, "feat", ""},
		{1790704112.929, "feat-new", ""},
		{1790704115.178, "feat-new", "feat-new"},
		{1790704117.441, "feat-new", "feat-new"},
	})
}

// Pins: a finish is read only from git's own action text, and only as
// "returning to refs/heads/<name>" with nothing after the name.
func TestBranchAt_FinishForms(t *testing.T) {
	cases := map[string]string{
		"rebase (finish): returning to refs/heads/topic":                         "topic",
		"rebase -i (finish): returning to refs/heads/topic":                      "topic",
		"pull -q --rebase origin main (finish): returning to refs/heads/topic":   "topic",
		"rebase (pick): x (finish): returning to refs/heads/topic":               "",
		"checkout: moving from a to :/x (finish): returning to refs/heads/topic": "",
		"merge r2 (finish): returning to refs/heads/topic":                       "",
		"rebase (finish): returning to refs/heads/topic extra":                   "",
		"rebase (finish): returning to refs/heads/topic\x00":                     "",
	}
	for msg, want := range cases {
		log := rlLine(oidZero, oidA, 1000, "") +
			rlLine(oidA, oidA, 1010, msg) +
			rlLine(oidA, oidA, 1020, "Branch: renamed refs/heads/topic to refs/heads/main")
		x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", log)
		t.Run(msg, func(t *testing.T) { wantBranchAt(t, x, c, sec(1015), want) })
	}
}

// Pins: a chain gap right after the entry that set HEAD's target is seen.
func TestBranchAt_GapAfterTargetEntry(t *testing.T) {
	build := func(commitOld string) string {
		return rlLine(oidZero, oidA, 1000, "") +
			rlLine(oidA, oidA, 1010, "checkout: moving from main to topic") +
			rlLine(commitOld, oidC, 1020, "commit: x") +
			rlLine(oidC, oidC, 1030, "checkout: moving from topic to main")
	}
	x, c, _ := reflogAdmin(t, "ref: refs/heads/main\n", build(oidA))
	wantBranchAt(t, x, c, sec(1015), "topic") // control
	x, c, _ = reflogAdmin(t, "ref: refs/heads/main\n", build(oidB))
	wantBranchAt(t, x, c, sec(1015), "")
	wantBranchAt(t, x, c, sec(1025), "")
}

// Pins: a world-writable HEAD, logs dir or logs/HEAD is refused, each alone.
func TestBranchAt_WorldWritable(t *testing.T) {
	for name, mode := range map[string]os.FileMode{"HEAD": 0o646, "logs": 0o757, filepath.Join("logs", "HEAD"): 0o646} {
		x, c, admin := reflogAdmin(t, historyHead, historyLog)
		p := filepath.Join(admin, name)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		wantBranchAt(t, x, c, sec(1025), "")
		if err := os.Chmod(p, mode&^0o002); err != nil {
			t.Fatal(err)
		}
		wantBranchAt(t, x, c, sec(1025), "other")
	}
}
