package collector

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxGitdirFile bounds how many bytes we read from a worktree's ".git" file and
// its "commondir" pointer. Both are single short lines in git's on-disk layout;
// 4 KB is generous headroom while capping RAM against a hostile/oversized file.
const maxGitdirFile = 4 << 10

// gitMainRepoRoot resolves the primary repository root governing path, following
// git's own discovery rules without exec'ing git:
//  1. Walk UP from path (filepath.Dir chain, stopping at the filesystem root)
//     to the first entry containing ".git".
//  2. ".git" is a directory  -> that ancestor IS a primary checkout root; return it.
//  3. ".git" is a file       -> parse the "gitdir: <p>" line (p made absolute
//     relative to the ancestor if relative). Then locate the common dir:
//     read <p>/commondir if it exists (content is a path, usually "../..",
//     relative to <p>); otherwise use <p> itself. Clean the result.
//  4. If filepath.Base(commonDir) == ".git" -> return filepath.Dir(commonDir).
//     Otherwise (bare repo, exotic layout) -> return "".
//
// Any stat/read/parse failure returns "" — the caller falls back to the
// prefix-match verdict it already had. Reads are bounded (gitdir file <= 4 KB).
// We do NOT exec git and do NOT follow gitdir chains recursively: one hop is
// git's own layout for a linked worktree, so a single hop is both sufficient
// and a hard bound against a crafted chain.
//
// Documented residual limit: a worktree that has already been REMOVED from disk
// cannot be resolved — its ".git" file no longer exists, so this returns "".
// The live watcher is immune in practice (fsnotify events fire while the session
// runs, when the worktree necessarily exists); only retroactive `tierd score`
// scans over sessions whose temp worktrees were cleaned lose them. That is
// inherent to path-recorded evidence; we deliberately do not bolt on heuristics.
//
// Safety (identity, not prefix): resolution only MAPS a cwd to a repo root; the
// caller still decides whether that root is a configured target, so a foreign
// repo's worktree stays foreign.
func gitMainRepoRoot(path string) string {
	if path == "" {
		return ""
	}
	dir := filepath.Clean(path)
	for {
		gitPath := filepath.Join(dir, ".git")
		if info, err := os.Lstat(gitPath); err == nil {
			switch {
			case info.IsDir():
				// Primary checkout: this ancestor is the repo root.
				return dir
			case info.Mode().IsRegular():
				// Linked worktree (or submodule): resolve via the gitdir pointer.
				return resolveViaGitdir(dir, gitPath)
			}
			// A ".git" entry that is neither a directory nor a regular file
			// (symlink, socket, device) is not a layout git produces here —
			// ignore it and keep walking up.
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding a ".git" — not a repo.
			return ""
		}
		dir = parent
	}
}

// resolveViaGitdir handles the ".git"-as-file case: it reads the "gitdir: <p>"
// pointer, then the optional "commondir" file next to <p>, and returns the main
// repository root (the parent of the common ".git" directory). Any failure
// returns "".
func resolveViaGitdir(ancestor, gitFile string) string {
	gitdir := parseGitdirPointer(readBoundedFirstLine(gitFile))
	if gitdir == "" {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(ancestor, gitdir)
	}
	gitdir = filepath.Clean(gitdir)

	// The common dir is where the shared object store / refs live. For a linked
	// worktree gitdir is <main>/.git/worktrees/<name> and commondir points back
	// at <main>/.git (usually the relative "../.."). Absent commondir, the
	// gitdir itself is the common dir (a plain checkout addressed via a file).
	commonDir := gitdir
	if line := readBoundedFirstLine(filepath.Join(gitdir, "commondir")); line != "" {
		if !filepath.IsAbs(line) {
			line = filepath.Join(gitdir, line)
		}
		commonDir = filepath.Clean(line)
	}
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	// Bare repo or an exotic layout we don't attribute — fail closed.
	return ""
}

// parseGitdirPointer extracts <p> from a "gitdir: <p>" line, returning "" when
// the prefix is absent or the path is empty.
func parseGitdirPointer(line string) string {
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(rest)
}

// readBoundedFirstLine reads at most maxGitdirFile bytes from path and returns
// the first line, trimmed of surrounding whitespace. Returns "" on any error
// (missing file, permission denied) so callers fail closed. Bounding the read
// caps RAM against an oversized or hostile file.
func readBoundedFirstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, maxGitdirFile))
	if err != nil {
		return ""
	}
	s := string(buf)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
