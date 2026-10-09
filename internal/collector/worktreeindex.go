package collector

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// worktreeClass is the verdict worktreeIndex.classify gives one recorded path.
// The zero value is worktreeUnresolvable, so a resolution nobody filled in
// falls back to today's rule instead of keeping a carry.
type worktreeClass int

const (
	// worktreeUnresolvable: the path is inside something git-shaped that did
	// not verify. It must never set a carry, and it resets one.
	worktreeUnresolvable worktreeClass = iota
	// worktreeNeutral: a relative or ".." path, a path under a neutral root
	// (/tmp, which holds the scratchpad, and ~/.claude), or a path with no
	// ".git" entry at or above it, even one whose directory does not exist. It
	// neither sets nor resets a carry.
	worktreeNeutral
	// worktreeMainCheckout: inside a configured target's main checkout.
	worktreeMainCheckout
	// worktreeLinked: inside a verified linked worktree of a configured target.
	worktreeLinked
	// worktreeForeignMain: inside the main checkout of a repo no target covers.
	// It resets a carry and books nothing. It carries no path or slug.
	worktreeForeignMain
	// worktreeForeignWorktree: inside a verified linked worktree of a repo no
	// target covers (#823 Q3: the unattributed:foreign-repo bucket). It carries
	// no path or slug by construction.
	worktreeForeignWorktree
)

// pathClassification is one classification. Target is the configured target
// string (MainCheckout and Linked only); Root and AdminDir are the canonical
// (EvalSymlinks) worktree root and <main>/.git/worktrees/<id> (Linked only).
type pathClassification struct {
	Class    worktreeClass
	Target   string
	Root     string
	AdminDir string
}

// worktreeIndex classifies paths against the configured main checkouts from
// git's on-disk layout alone: it never runs a git binary. It is immutable after
// newWorktreeIndex, so it is safe for concurrent use.
type worktreeIndex struct {
	targets []worktreeTarget
	neutral []string // cleaned and canonical forms of each neutral root
	uid     int      // the uid a linked worktree must be owned by
	// partial is set when a configured target was dropped: a linked worktree
	// outside every indexed target may then be the dropped target's, so it is
	// Unresolvable (today's rule), never ForeignWorktree.
	partial bool
	// owned reports whether the entry at path passes the owner check. It is
	// ownedBy(info, uid) except where a test swaps it to fail one path alone.
	owned func(path string, info os.FileInfo) bool
}

// worktreeTarget is one configured target: the canonical root of the main
// repository holding it, and the target's own directory as path elements
// below the root of its own checkout, main or linked (none when the target is
// that checkout's root).
type worktreeTarget struct {
	name     string
	root     string
	rootInfo os.FileInfo
	sub      []string
	dirInfo  os.FileInfo
}

// defaultWorktreeNeutralRoots is /tmp (the scratchpad lives under it on both
// macOS and Linux) and home's .claude directory.
func defaultWorktreeNeutralRoots(home string) []string {
	roots := []string{"/tmp"}
	if home != "" {
		roots = append(roots, filepath.Join(home, ".claude"))
	}
	return roots
}

// newWorktreeIndex records each target by its own canonical directory and the
// canonical root of the main repository holding it. A path belongs to a target
// only when it lies under that target's directory, the deepest directory
// winning, so a target that is a subdirectory of a larger repo (a monorepo
// service, a dotfiles home) never claims the rest of that repo. A target that
// does not exist, or is in no git tree, when the index is built is dropped
// (see partial); when two targets name the same directory, the first
// configured wins.
func newWorktreeIndex(targets, neutralRoots []string) *worktreeIndex {
	x := &worktreeIndex{uid: os.Getuid()}
	x.owned = func(_ string, info os.FileInfo) bool { return ownedBy(info, x.uid) }
	for _, t := range targets {
		if tgt, ok := resolveWorktreeTarget(t); ok {
			x.targets = append(x.targets, tgt)
		} else {
			x.partial = true
		}
	}
	for _, r := range neutralRoots {
		if !filepath.IsAbs(r) || filepath.Clean(r) == string(filepath.Separator) {
			continue
		}
		x.neutral = append(x.neutral, filepath.Clean(r))
		if canon, err := filepath.EvalSymlinks(r); err == nil && canon != filepath.Clean(r) {
			x.neutral = append(x.neutral, canon)
		}
	}
	return x
}

func resolveWorktreeTarget(t string) (worktreeTarget, bool) {
	abs, err := filepath.Abs(t)
	if err != nil {
		return worktreeTarget{}, false
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return worktreeTarget{}, false
	}
	dirInfo, err := os.Stat(dir)
	if err != nil || !dirInfo.IsDir() {
		return worktreeTarget{}, false
	}
	root, err := filepath.EvalSymlinks(gitMainRepoRoot(dir))
	if err != nil {
		return worktreeTarget{}, false
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return worktreeTarget{}, false
	}
	tgt := worktreeTarget{name: t, root: root, rootInfo: rootInfo, dirInfo: dirInfo}
	// sub is relative to the checkout holding the target, as classify makes a
	// path's elements relative to the checkout holding it; a target that is a
	// linked worktree's root names the whole repository.
	if rel, err := filepath.Rel(gitCheckoutRoot(dir), dir); err == nil && rel != "." && !hasDotDotElem(rel) {
		tgt.sub = splitElems(rel)
	}
	return tgt, true
}

// gitCheckoutRoot returns the nearest ancestor of dir, dir included, holding a
// ".git" entry of any kind: the checkout classify measures dir's paths from.
func gitCheckoutRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

var (
	resolvedNeutral         = pathClassification{Class: worktreeNeutral}
	resolvedUnresolvable    = pathClassification{Class: worktreeUnresolvable}
	resolvedForeignMain     = pathClassification{Class: worktreeForeignMain}
	resolvedForeignWorktree = pathClassification{Class: worktreeForeignWorktree}
)

// classify walks up from path to the first ".git" entry. A missing entry or a
// non-directory component keeps walking; any other stat error (a symlink loop,
// permission denied) is Unresolvable, never a guess. A path whose own
// directory does not exist is Unresolvable when the walk finds a ".git" entry
// whose root is not neutral: a removed worktree nested in its main checkout
// would otherwise walk up to the main ".git" and read as MainCheckout. With no
// ".git" entry at or above it, it is Neutral like any path in no git tree.
func (x *worktreeIndex) classify(path string) pathClassification {
	if path == "" || !filepath.IsAbs(path) || hasDotDotElem(path) || x.underNeutral(filepath.Clean(path)) {
		return resolvedNeutral
	}
	clean := filepath.Clean(path)
	_, err := os.Lstat(filepath.Dir(clean))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return resolvedUnresolvable
	}
	missing := err != nil
	for dir := clean; ; {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			rel := splitElems(strings.TrimPrefix(clean[len(dir):], string(filepath.Separator)))
			if c := x.classifyRoot(dir, rel, info); !missing || c.Class == worktreeNeutral {
				return c
			}
			return resolvedUnresolvable
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return resolvedUnresolvable
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return resolvedNeutral
		}
		dir = parent
	}
}

// classifyRoot classifies the path at elements rel below dir, whose ".git"
// entry is gitEntry.
func (x *worktreeIndex) classifyRoot(dir string, rel []string, gitEntry os.FileInfo) pathClassification {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return resolvedUnresolvable
	}
	if x.underNeutral(root) {
		return resolvedNeutral
	}
	switch {
	case gitEntry.IsDir():
		t, ok, err := x.target(root, rel)
		if err != nil {
			return resolvedUnresolvable
		}
		if ok {
			return pathClassification{Class: worktreeMainCheckout, Target: t}
		}
		return resolvedForeignMain
	case gitEntry.Mode().IsRegular():
		return x.classifyLinked(dir, root, rel, gitEntry)
	}
	return resolvedUnresolvable // a symlinked or special ".git" is not a layout git writes
}

// classifyLinked verifies a ".git" file in both directions: it must name an
// admin dir registered under a common ".git/worktrees", and that admin dir's
// gitdir must point back at this same worktree. Only then is the main root
// looked up, so a moved, pruned or forged worktree is Unresolvable.
func (x *worktreeIndex) classifyLinked(dir, root string, rel []string, gitFile os.FileInfo) pathClassification {
	gitPath := filepath.Join(dir, ".git")
	if !x.owned(gitPath, gitFile) {
		return resolvedUnresolvable
	}
	// A relative pointer is resolved against the canonical root, as git does,
	// so a symlinked path to the worktree resolves like its real path.
	admin, ok := resolvePointer(root, parseGitdirPointer(readBoundedFirstLine(gitPath)))
	if !ok {
		return resolvedUnresolvable
	}
	if info, err := os.Lstat(admin); err != nil || !info.IsDir() || !x.owned(admin, info) {
		return resolvedUnresolvable
	}
	common, ok := resolvePointer(admin, readBoundedFirstLine(filepath.Join(admin, "commondir")))
	if !ok || filepath.Base(common) != ".git" || filepath.Dir(admin) != filepath.Join(common, "worktrees") {
		return resolvedUnresolvable
	}
	back, ok := resolvePointer(admin, readBoundedFirstLine(filepath.Join(admin, "gitdir")))
	if !ok || filepath.Base(back) != ".git" {
		return resolvedUnresolvable
	}
	// A root spelled differently from git's record (letter case on a
	// case-insensitive filesystem) must be the same directory; Root is then
	// git's spelling, so every spelling of one worktree gives one Root.
	if filepath.Dir(back) != root {
		bi, errB := os.Stat(filepath.Dir(back))
		ri, errR := os.Stat(root)
		if errB != nil || errR != nil || !os.SameFile(bi, ri) {
			return resolvedUnresolvable
		}
		root = filepath.Dir(back)
	}
	t, ok, err := x.target(filepath.Dir(common), rel)
	if err != nil {
		return resolvedUnresolvable
	}
	if !ok && x.partial {
		return resolvedUnresolvable
	}
	if !ok {
		return resolvedForeignWorktree
	}
	return pathClassification{Class: worktreeLinked, Target: t, Root: root, AdminDir: admin}
}

// target returns the configured target covering the path at elements rel below
// the main repository root spelled root. A root or target directory spelled
// differently from the configured one (letter case on a case-insensitive
// filesystem) is matched by os.SameFile, so it is never read as foreign. A root
// that cannot be stat'ed is an error, never a foreign verdict.
func (x *worktreeIndex) target(root string, rel []string) (string, bool, error) {
	var rootInfo os.FileInfo
	best, depth := "", -1
	for _, t := range x.targets {
		if len(t.sub) <= depth || len(rel) < len(t.sub) {
			continue
		}
		if t.root != root {
			if rootInfo == nil {
				info, err := os.Stat(root)
				if err != nil {
					return "", false, err
				}
				rootInfo = info
			}
			if !os.SameFile(rootInfo, t.rootInfo) {
				continue
			}
		}
		if len(t.sub) > 0 && !slices.Equal(rel[:len(t.sub)], t.sub) {
			info, err := os.Stat(filepath.Join(append([]string{root}, rel[:len(t.sub)]...)...))
			if err != nil || !os.SameFile(info, t.dirInfo) {
				continue
			}
		}
		best, depth = t.name, len(t.sub)
	}
	return best, depth >= 0, nil
}

// resolvePointer makes a pointer read from a file under base absolute and
// canonical. An empty pointer or one that does not resolve on disk fails.
func resolvePointer(base, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	canon, err := filepath.EvalSymlinks(p)
	return canon, err == nil
}

func (x *worktreeIndex) underNeutral(p string) bool {
	for _, r := range x.neutral {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// splitElems splits a relative path into its elements; "" and "." have none.
func splitElems(rel string) []string {
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

func hasDotDotElem(p string) bool {
	for _, e := range strings.Split(filepath.ToSlash(p), "/") {
		if e == ".." {
			return true
		}
	}
	return false
}
