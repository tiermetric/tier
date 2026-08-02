package collector

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiermetric/tier/internal/repoid"
)

// maxGitConfigFile bounds how many bytes we read from a repository's .git/config.
// A pathological config is not worth unbounded RAM; 256 KB is far past any real
// one while still capping a hostile file.
const maxGitConfigFile = 256 << 10

// RepoSlugFromGitConfig resolves a repository root to its canonical "owner/repo"
// slug by reading remote.origin.url out of <root>/.git/config, or returns "" when
// it cannot (#231).
//
// It does NOT exec git — same discipline as gitMainRepoRoot: a bounded file read,
// no subprocess, no PATH dependency, safe to call from a watcher hot path. The
// parse is deliberately narrow: find the [remote "origin"] section, take its url.
//
// Returns "" (not the sentinel) so the caller can distinguish "no origin remote"
// from "the operator told us the slug". A local-path remote also yields "": a
// filesystem path is not an identity a webhook could ever agree with, and inventing
// one would fabricate a join key only the collector can produce.
//
// FORK HAZARD, and why the config override exists: a contributor working on a fork
// has origin = "alice/tier" while the upstream webhook fires from "tiermetric/tier".
// Their cost would never join their outcomes. Callers must let an operator override
// this value; see JSONLCollector.RepoSlug.
func RepoSlugFromGitConfig(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	// A linked worktree's .git is a file; resolve to the main root first so we read
	// the shared config rather than missing it.
	if root := gitMainRepoRoot(repoRoot); root != "" {
		repoRoot = root
	}
	url := originURLFromGitConfig(filepath.Join(repoRoot, ".git", "config"))
	if url == "" {
		return ""
	}
	return repoid.FromRemoteURL(url)
}

// originURLFromGitConfig extracts the url of the [remote "origin"] section from a
// git config file. Returns "" on any read/parse failure — callers fail closed to
// the sentinel.
func originURLFromGitConfig(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, maxGitConfigFile))
	if err != nil {
		return ""
	}

	inOrigin := false
	for line := range strings.Lines(string(buf)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// Section header. git normalizes to `[remote "origin"]`, but tolerate
			// internal spacing.
			normalized := strings.Join(strings.Fields(line), " ")
			inOrigin = normalized == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		return strings.TrimSpace(val)
	}
	return ""
}
