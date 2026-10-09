package collector

import "os"

// WorktreeAttribution turns on #823 worktree attribution for a command's
// Claude Code capture. It is one immutable worktree index, built once per
// command and shared by that command's collectors or watcher. A nil
// *WorktreeAttribution is attribution off: today's branch rule, no rule
// recorded.
type WorktreeAttribution struct{ index *worktreeIndex }

// NewWorktreeAttribution indexes targets, the command's repo paths, each
// spelled exactly as the collector (RepoPath) or watcher (Repos) that will use
// it spells it: every target must be collected, under that same string. The
// neutral roots are /tmp and ~/.claude.
func NewWorktreeAttribution(targets []string) *WorktreeAttribution {
	home, _ := os.UserHomeDir() // "" leaves /tmp as the only neutral root
	return &WorktreeAttribution{index: newWorktreeIndex(targets, defaultWorktreeNeutralRoots(home))}
}

func (a *WorktreeAttribution) worktreeIndex() *worktreeIndex {
	if a == nil {
		return nil
	}
	return a.index
}

// SetWorktreeAttribution turns #823 attribution on for c with a, or off with
// nil. a must index c.RepoPath and only targets that are collected too.
func (c *JSONLCollector) SetWorktreeAttribution(a *WorktreeAttribution) {
	c.worktrees = a.worktreeIndex()
}

// SetWorktreeAttribution turns #823 attribution on for w with a, or off with
// nil. a must index each w.Repos entry and nothing else. Call it before Run.
func (w *Watcher) SetWorktreeAttribution(a *WorktreeAttribution) {
	w.worktrees = a.worktreeIndex()
}
