package collector

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// wtRepo creates base/name as a git repository with one commit and returns its
// canonical path.
func wtRepo(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepoWithCommit(t, dir)
	return canonPath(t, dir)
}

func canonPath(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantClass(t *testing.T, x *worktreeIndex, path string, want pathClassification) {
	t.Helper()
	if got := x.classify(path); got != want {
		t.Errorf("classify(%q) = %+v, want %+v", path, got, want)
	}
}

// Test-local expectations, so a mutant of the production verdict values cannot
// also move what the tests expect.
var (
	wantNeutral         = pathClassification{Class: worktreeNeutral}
	wantUnresolvable    = pathClassification{Class: worktreeUnresolvable}
	wantForeignMain     = pathClassification{Class: worktreeForeignMain}
	wantForeignWorktree = pathClassification{Class: worktreeForeignWorktree}
)

func mainCheckout(target string) pathClassification {
	return pathClassification{Class: worktreeMainCheckout, Target: target}
}

// needOwner skips a test that expects a verified linked worktree where no
// owner uid can be read: every linked worktree is Unresolvable there.
func needOwner(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no owner uid on windows: ownedBy is always false there")
	}
}

func linked(target, root, admin string) pathClassification {
	return pathClassification{Class: worktreeLinked, Target: target, Root: root, AdminDir: admin}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Pins: a worktree nested inside its own main checkout (Claude Code's
// .claude/worktrees layout) is Linked, not MainCheckout, and the zero value is
// Unresolvable.
func TestWorktreeIndex_NestedDotClaudeWorktreeIsWorktree(t *testing.T) {
	needOwner(t)
	repo := wtRepo(t, t.TempDir(), "repo")
	wt := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "agent-1"), "b1")
	x := newWorktreeIndex([]string{repo}, nil)
	admin := filepath.Join(repo, ".git", "worktrees", "agent-1")
	mkdirs(t, filepath.Join(wt, "internal"))
	wantClass(t, x, filepath.Join(wt, "internal", "x.go"), linked(repo, wt, admin))
	wantClass(t, x, wt, linked(repo, wt, admin))
	mkdirs(t, filepath.Join(repo, "internal"))
	wantClass(t, x, filepath.Join(repo, "internal", "x.go"), mainCheckout(repo))
	if (pathClassification{}).Class != worktreeUnresolvable {
		t.Error("zero pathClassification must be Unresolvable")
	}
}

// Pins: git's relative-pointer layout (worktree.useRelativePaths) still
// verifies, also when the worktree is reached through a symlink, because a
// relative pointer resolves against the canonical root.
func TestWorktreeIndex_RelativePointersResolve(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
	admin := filepath.Join(repo, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: ../repo/.git/worktrees/wt\n")
	writeFile(t, filepath.Join(admin, "gitdir"), "../../../../wt/.git\n")
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(wt, "f"), linked(repo, wt, admin))
	elsewhere := filepath.Join(base, "elsewhere")
	mkdirs(t, elsewhere)
	if err := os.Symlink(wt, filepath.Join(elsewhere, "wtlink")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	wantClass(t, x, filepath.Join(elsewhere, "wtlink", "f"), linked(repo, wt, admin))
}

// Pins: a worktree moved without `git worktree move` fails the back-pointer
// check, including when another checkout with its own .git file now sits at
// the old path, so the back-pointer resolves and the root comparison decides.
func TestWorktreeIndex_MovedWorktreeFailsClosed(t *testing.T) {
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
	moved := filepath.Join(base, "moved")
	if err := os.Rename(wt, moved); err != nil {
		t.Fatal(err)
	}
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(moved, "f"), wantUnresolvable)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt")+"\n")
	wantClass(t, x, filepath.Join(moved, "f"), wantUnresolvable)
}

func TestWorktreeIndex_RemovedAdminDirFailsClosed(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(wt, "f"), linked(repo, wt, filepath.Join(repo, ".git", "worktrees", "wt")))
	if err := os.RemoveAll(filepath.Join(repo, ".git", "worktrees", "wt")); err != nil {
		t.Fatal(err)
	}
	wantClass(t, x, filepath.Join(wt, "f"), wantUnresolvable)
}

// Pins: a hand-written .git file naming a real admin dir (of the target or of
// an unconfigured repo) is Unresolvable, while the real worktrees classify as
// Linked and ForeignWorktree, the other main checkout as ForeignMain, and
// neither foreign class carries a path.
func TestWorktreeIndex_ForgedGitFileOtherRepo(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	target := wtRepo(t, base, "target")
	other := wtRepo(t, base, "other")
	wtT := addWorktree(t, target, filepath.Join(base, "wtT"), "b1")
	wtO := addWorktree(t, other, filepath.Join(base, "wtO"), "b2")
	x := newWorktreeIndex([]string{target}, nil)
	adminT := filepath.Join(target, ".git", "worktrees", "wtT")
	adminO := filepath.Join(other, ".git", "worktrees", "wtO")
	wantClass(t, x, filepath.Join(wtT, "f"), linked(target, wtT, adminT))
	wantClass(t, x, filepath.Join(wtO, "f"), wantForeignWorktree)
	wantClass(t, x, filepath.Join(other, "f"), wantForeignMain)
	for name, admin := range map[string]string{"forgedT": adminT, "forgedO": adminO} {
		forged := filepath.Join(base, name)
		writeFile(t, filepath.Join(forged, ".git"), "gitdir: "+admin+"\n")
		wantClass(t, x, filepath.Join(forged, "f"), wantUnresolvable)
	}
}

// Pins: an admin tree built only from committable paths (no ".git" element)
// inside the target, plus a planted .git file, is Unresolvable: its commondir
// is not a ".git" directory, even though its worktrees/ placement and its
// back-pointer both check out.
func TestWorktreeIndex_PlantedArchiveAdminDirUnresolvable(t *testing.T) {
	needOwner(t)
	target := wtRepo(t, t.TempDir(), "target")
	adm := filepath.Join(target, "foo", "worktrees", "x")
	writeFile(t, filepath.Join(adm, "commondir"), "../..\n")
	writeFile(t, filepath.Join(adm, "gitdir"), "../../../wt/.git\n")
	writeFile(t, filepath.Join(target, "wt", ".git"), "gitdir: ../foo/worktrees/x\n")
	wantClass(t, newWorktreeIndex([]string{target}, nil), filepath.Join(target, "wt", "f"), wantUnresolvable)
}

// Pins: paths under a neutral root are Neutral even when a real worktree of
// the target lives there or the home directory is itself a git repo; the
// default roots include the real /tmp, shown by a target worktree under it
// that is Linked without neutral roots and Neutral with the defaults.
func TestWorktreeIndex_TmpScratchpadDotClaudeNeutral(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	tmp := filepath.Join(base, "tmp")
	home := wtRepo(t, base, "home")
	scratchWT := addWorktree(t, repo, filepath.Join(tmp, "claude-501", "scratchpad", "wt"), "b1")
	x := newWorktreeIndex([]string{repo}, []string{tmp, filepath.Join(home, ".claude")})
	wantClass(t, x, filepath.Join(scratchWT, "f"), wantNeutral)
	wantClass(t, x, filepath.Join(tmp, "claude-501", "scratchpad", "notes.md"), wantNeutral)
	wantClass(t, x, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), wantNeutral)
	wantClass(t, x, filepath.Join(home, "notes.txt"), wantForeignMain)

	realTmp, err := os.MkdirTemp("/tmp", "tier-wtindex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(realTmp) })
	tmpWT := addWorktree(t, repo, filepath.Join(realTmp, "wt"), "b2")
	if got := newWorktreeIndex([]string{repo}, nil).classify(filepath.Join(tmpWT, "f")); got.Class != worktreeLinked {
		t.Fatalf("control: a target worktree under /tmp without neutral roots = %+v, want Linked", got)
	}
	d := newWorktreeIndex([]string{repo}, defaultWorktreeNeutralRoots(home))
	wantClass(t, d, filepath.Join(tmpWT, "f"), wantNeutral)
	wantClass(t, d, filepath.Join(home, ".claude", "settings.json"), wantNeutral)
}

// Pins: a neutral root configured through a symlink also covers its canonical
// spelling (macOS records /tmp paths as /private/tmp), and a path reached
// through a symlink into a neutral root is Neutral by its canonical root.
func TestWorktreeIndex_SymlinkedNeutralRootCoversCanonicalPath(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	realTmp := filepath.Join(base, "realtmp")
	wt := addWorktree(t, repo, filepath.Join(realTmp, "wt"), "b1")
	link := filepath.Join(base, "tmplink")
	if err := os.Symlink(realTmp, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got := newWorktreeIndex([]string{repo}, nil).classify(filepath.Join(wt, "f")); got.Class != worktreeLinked {
		t.Fatalf("control: without neutral roots = %+v, want Linked", got)
	}
	wantClass(t, newWorktreeIndex([]string{repo}, []string{link}), filepath.Join(wt, "f"), wantNeutral)
	work := filepath.Join(base, "work")
	mkdirs(t, work)
	if err := os.Symlink(realTmp, filepath.Join(work, "scratch")); err != nil {
		t.Fatal(err)
	}
	wantClass(t, newWorktreeIndex([]string{repo}, []string{realTmp}), filepath.Join(work, "scratch", "wt", "f"), wantNeutral)
}

func TestWorktreeIndex_RelativeAndDotDotNeutral(t *testing.T) {
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	x := newWorktreeIndex([]string{repo}, nil)
	for _, p := range []string{"", "x.go", "./x.go", "../repo/x.go", filepath.Join(repo, "sub") + "/../x.go"} {
		wantClass(t, x, p, wantNeutral)
	}
	mkdirs(t, filepath.Join(base, "plain"))
	wantClass(t, x, filepath.Join(base, "plain", "f"), wantNeutral)
	wantClass(t, x, filepath.Join(repo, "x.go"), mainCheckout(repo))
}

// Pins: target via symlink or real path x recorded path via symlink or real
// path all give the same Linked result; a symlink loop is Unresolvable.
func TestWorktreeIndex_SymlinkFourWay(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	realDir := filepath.Join(base, "real")
	repo := wtRepo(t, realDir, "repo")
	wt := addWorktree(t, repo, filepath.Join(realDir, "wt"), "b1")
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	admin := filepath.Join(repo, ".git", "worktrees", "wt")
	for _, target := range []string{repo, filepath.Join(link, "repo")} {
		x := newWorktreeIndex([]string{target}, nil)
		for _, p := range []string{filepath.Join(wt, "f"), filepath.Join(link, "wt", "f")} {
			wantClass(t, x, p, linked(target, wt, admin))
		}
	}
	if err := os.Symlink("loop", filepath.Join(wt, "loop")); err != nil {
		t.Fatal(err)
	}
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(wt, "loop", "f"), wantUnresolvable)
	wantClass(t, x, filepath.Join(wt, "loop"), wantUnresolvable)
}

// Pins: a linked worktree whose .git file or admin dir is not owned by the
// index's uid is Unresolvable. The index's uid is set to another uid because
// creating files owned by another uid needs root.
func TestWorktreeIndex_OtherUidUnresolvable(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(wt, "f"), linked(repo, wt, filepath.Join(repo, ".git", "worktrees", "wt")))
	x.uid = os.Getuid() + 1
	wantClass(t, x, filepath.Join(wt, "f"), wantUnresolvable)
}

// Pins: each owner check fails the worktree on its own: a .git file owned by
// another uid, and an admin dir owned by another uid, are each Unresolvable
// while every other entry passes.
func TestWorktreeIndex_OwnerCheckedOnEachEntry(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
	admin := filepath.Join(repo, ".git", "worktrees", "wt")
	for name, bad := range map[string]string{"git file": filepath.Join(wt, ".git"), "admin dir": admin} {
		x := newWorktreeIndex([]string{repo}, nil)
		wantClass(t, x, filepath.Join(wt, "f"), linked(repo, wt, admin))
		x.owned = func(p string, info os.FileInfo) bool { return p != bad && ownedBy(info, x.uid) }
		if got := x.classify(filepath.Join(wt, "f")); got != wantUnresolvable {
			t.Errorf("%s owned by another uid: classify = %+v, want Unresolvable", name, got)
		}
	}
}

// Pins: a path inside a removed worktree nested in its main checkout is
// Unresolvable, not the main checkout its walk would reach.
func TestWorktreeIndex_RemovedNestedWorktreeUnresolvable(t *testing.T) {
	repo := wtRepo(t, t.TempDir(), "repo")
	wt := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "agent-1"), "b1")
	x := newWorktreeIndex([]string{repo}, nil)
	for _, d := range []string{wt, filepath.Join(repo, ".git", "worktrees", "agent-1")} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
	wantClass(t, x, filepath.Join(wt, "internal", "x.go"), wantUnresolvable)
	wantClass(t, x, filepath.Join(wt, "x.go"), wantUnresolvable)
	wantClass(t, x, filepath.Join(repo, ".claude", "worktrees", "new.go"), mainCheckout(repo))
}

// Pins: a target is its own directory, not the repo enclosing it. A dotfiles
// home repo's other paths, and a monorepo's paths outside every configured
// service, are foreign; the deepest target wins; a linked worktree maps its
// path into the main root before the target is chosen; and a relative target
// is made absolute.
func TestWorktreeIndex_TargetIsItsOwnDirectory(t *testing.T) {
	base := canonPath(t, t.TempDir())
	home := wtRepo(t, base, "home")
	tier := filepath.Join(home, "code", "tier")
	mkdirs(t, tier, filepath.Join(home, "Documents"))
	x := newWorktreeIndex([]string{tier}, nil)
	wantClass(t, x, filepath.Join(tier, "x.go"), mainCheckout(tier))
	wantClass(t, x, filepath.Join(home, "Documents", "other", "main.go"), wantUnresolvable)
	wantClass(t, x, filepath.Join(home, "Documents", "main.go"), wantForeignMain)
	wantClass(t, x, filepath.Join(home, "notes.txt"), wantForeignMain)

	mono := wtRepo(t, base, "mono")
	svcA, svcB := filepath.Join(mono, "svcA"), filepath.Join(mono, "svcB")
	mkdirs(t, svcA, svcB, filepath.Join(mono, "lib"))
	x = newWorktreeIndex([]string{svcA, svcB}, nil)
	wantClass(t, x, filepath.Join(svcA, "f"), mainCheckout(svcA))
	wantClass(t, x, filepath.Join(svcB, "f"), mainCheckout(svcB))
	wantClass(t, x, filepath.Join(mono, "lib", "f"), wantForeignMain)
	x = newWorktreeIndex([]string{mono, svcA}, nil)
	wantClass(t, x, filepath.Join(svcA, "f"), mainCheckout(svcA))
	wantClass(t, x, filepath.Join(svcB, "f"), mainCheckout(mono))

	if runtime.GOOS != "windows" {
		wt := addWorktree(t, mono, filepath.Join(base, "monowt"), "b1")
		mkdirs(t, filepath.Join(wt, "svcB"), filepath.Join(wt, "lib"))
		x = newWorktreeIndex([]string{svcA, svcB}, nil)
		admin := filepath.Join(mono, ".git", "worktrees", "monowt")
		wantClass(t, x, filepath.Join(wt, "svcB", "f"), linked(svcB, wt, admin))
		wantClass(t, x, filepath.Join(wt, "lib", "f"), wantForeignWorktree)
	}

	t.Chdir(base)
	wantClass(t, newWorktreeIndex([]string{"mono/svcA"}, nil), filepath.Join(svcA, "f"), mainCheckout("mono/svcA"))
}

// Pins: on a case-insensitive filesystem a letter-case variant of a target's
// root or directory still matches that target, for a main checkout path and
// for a linked worktree whose admin files spell the root as git wrote it.
func TestWorktreeIndex_CaseVariantMatchesTarget(t *testing.T) {
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "Case")
	variant := filepath.Join(base, "cASE")
	if _, err := os.Lstat(variant); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	mkdirs(t, filepath.Join(repo, "svc"))
	wantClass(t, newWorktreeIndex([]string{repo}, nil), filepath.Join(variant, "f"), mainCheckout(repo))
	svc := filepath.Join(repo, "svc")
	wantClass(t, newWorktreeIndex([]string{svc}, nil), filepath.Join(repo, "SVC", "f"), mainCheckout(svc))
	if runtime.GOOS != "windows" {
		wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
		admin := filepath.Join(repo, ".git", "worktrees", "wt")
		wantClass(t, newWorktreeIndex([]string{variant}, nil), filepath.Join(wt, "f"), linked(variant, wt, admin))
	}
}

// Pins: on a case-insensitive filesystem a recorded path spelling a genuine
// linked worktree's directory in another letter case is Linked, and its Root
// is the spelling git recorded, the same string for every recorded spelling.
func TestWorktreeIndex_CaseVariantPathIntoLinkedWorktree(t *testing.T) {
	needOwner(t)
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "Agent"), "b1")
	variant := filepath.Join(base, "aGENT")
	if _, err := os.Lstat(variant); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	admin := filepath.Join(repo, ".git", "worktrees", "Agent")
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(wt, "f.go"), linked(repo, wt, admin))
	wantClass(t, x, filepath.Join(variant, "f.go"), linked(repo, wt, admin))
}

// Pins: a target configured as a linked worktree nested in its main checkout
// covers that worktree's own files, which classify relative to the worktree.
func TestWorktreeIndex_NestedLinkedWorktreeTarget(t *testing.T) {
	needOwner(t)
	repo := wtRepo(t, t.TempDir(), "repo")
	wt := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "agent-1"), "b1")
	admin := filepath.Join(repo, ".git", "worktrees", "agent-1")
	wantClass(t, newWorktreeIndex([]string{wt}, nil), filepath.Join(wt, "f.go"), linked(wt, wt, admin))
}

// Pins: a path whose parent directory is missing is Neutral when no ancestor
// holds a ".git" entry, and Unresolvable when one does.
func TestWorktreeIndex_MissingParentOutsideEveryRepoNeutral(t *testing.T) {
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	x := newWorktreeIndex([]string{repo}, nil)
	wantClass(t, x, filepath.Join(base, "plain", "gone", "f.go"), wantNeutral)
	wantClass(t, x, filepath.Join(repo, "gone", "f.go"), wantUnresolvable)
}
