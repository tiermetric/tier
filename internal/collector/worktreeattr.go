package collector

import (
	"cmp"
	"path/filepath"
	"slices"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// messageAttribution is the #823 verdict for one message. The zero value means
// no worktree rule applies and today's branch rule decides. A Foreign verdict
// books to UnattributedForeignRepo and carries no Target or Branch: nothing
// that names the unconfigured repo.
type messageAttribution struct {
	Rule    store.AttributionRule
	Target  string // the configured target the branch belongs to
	Branch  string
	Foreign bool // the message ran in a linked worktree of an unconfigured repo
}

// worktreeResolver attributes the messages of ONE transcript file, in file
// order, by the #823 priority: the message's cwd in a linked worktree, then the
// worktree its own tool paths name, then the carry, then today's rule. It holds
// the carry latch, so use one per file. It reads the disk only through its
// index, never logs, and never reads a group's stream label (Sidechain, Agent):
// a subagent file carries across its whole length like any other.
type worktreeResolver struct {
	index *worktreeIndex
	now   time.Time // the clock branchAt measures the reflog's expiry horizon from
	// carry is the worktree the message paths have named since the last reset,
	// a worktreeLinked classification, or the zero value when nothing is
	// carried. A reference to a DIFFERENT worktree clears it (observe); only the
	// next reference sets it again.
	carry pathClassification
}

// resolve returns g's attribution and updates the carry from g's paths. cwd and
// gitBranch are the message's own transcript fields ("" when the message has no
// usage event to take them from). A cwd that is not a network path decides when
// it is in a linked worktree of an unconfigured repo (Foreign) and g's paths
// name no configured worktree (one they name decides instead), or when
// gitBranch is named (not "", "HEAD" or a harness worktree name) and the cwd
// worktree's reflog branch at g's times equals it: a transcript often reports
// the parent checkout's branch from inside a worktree. The cwd never sets or
// clears the carry; g's paths move it alike whichever arm decides.
func (r *worktreeResolver) resolve(g messageGroup, cwd, gitBranch string) messageAttribution {
	direct, ok := r.observe(g)
	if !isNetworkPath(cwd) {
		if a, decided := r.cwdArm(g, cwd, gitBranch); decided {
			if ok && direct.Class == worktreeLinked {
				// branchOf also clears a carry it cannot read.
				if b := r.branchOf(direct, g, store.AttributionRuleWorktreeToolPath); a.Foreign {
					return b
				}
			}
			return a
		}
	}
	if ok && direct.Class == worktreeForeignWorktree {
		return messageAttribution{Rule: store.AttributionRuleWorktreeToolPath, Foreign: true}
	}
	if ok {
		return r.branchOf(direct, g, store.AttributionRuleWorktreeToolPath)
	}
	if r.carry.Class == worktreeLinked {
		return r.branchOf(r.carry, g, store.AttributionRuleCarry)
	}
	return messageAttribution{}
}

// cwdArm is the cwd rule of resolve, whose doc states it.
func (r *worktreeResolver) cwdArm(g messageGroup, cwd, gitBranch string) (messageAttribution, bool) {
	c := r.index.classify(cwd)
	switch {
	case c.Class == worktreeForeignWorktree:
		return messageAttribution{Rule: store.AttributionRuleWorktreeCWD, Foreign: true}, true
	case c.Class != worktreeLinked || gitBranch == "" || gitBranch == "HEAD" || IsHarnessWorktreeBranch(gitBranch):
		return messageAttribution{}, false
	}
	if b, agree := r.branchIn(c, g); agree && b == gitBranch {
		return messageAttribution{Rule: store.AttributionRuleWorktreeCWD, Target: c.Target, Branch: b}, true
	}
	return messageAttribution{}, false
}

// observe classifies g's paths. It returns the one linked worktree they name,
// if they name exactly one and nothing else that is not neutral, or
// resolvedForeignWorktree if every path that is not neutral is in a linked
// worktree of an unconfigured repo, and updates the carry: neutral paths leave
// it; a path in a main checkout, a foreign repo, an unresolvable tree (another
// uid's worktree included), a second worktree in the same message, or a group
// whose paths may be incomplete clears it; a worktree other than the carried
// one clears it; the carried worktree, or any worktree when nothing is
// carried, sets it. A foreign worktree never sets it: it has no Root to carry.
func (r *worktreeResolver) observe(g messageGroup) (pathClassification, bool) {
	var one pathClassification
	named, foreign, reset := false, false, g.Truncated
	for _, p := range g.Paths {
		switch c := r.index.classify(p); c.Class {
		case worktreeNeutral:
		case worktreeLinked:
			reset = reset || (named && c != one)
			one, named = c, true
		case worktreeForeignWorktree:
			foreign = true
		default:
			reset = true
		}
	}
	switch {
	case reset || (named && foreign):
		r.carry = pathClassification{}
		return pathClassification{}, false
	case foreign:
		r.carry = pathClassification{}
		return resolvedForeignWorktree, true
	case !named:
		return pathClassification{}, false
	case r.carry.Class == worktreeLinked && r.carry != one:
		r.carry = pathClassification{}
	default:
		r.carry = one
	}
	return one, true
}

// branchIn reads c's branch at g's first and last line times from the
// worktree's reflog; ok only when both resolve to the same branch.
func (r *worktreeResolver) branchIn(c pathClassification, g messageGroup) (string, bool) {
	first, ok := r.index.branchAt(c, g.First, r.now)
	if ok && !g.Last.Equal(g.First) {
		var last string
		last, ok = r.index.branchAt(c, g.Last, r.now)
		ok = ok && last == first
	}
	return first, ok
}

// branchOf is branchIn for the tool-path and carry arms. A branch it cannot
// read, or a harness worktree name (no signal: #490's inheritance decides), is
// today's rule and clears the carry, never kept stale.
func (r *worktreeResolver) branchOf(c pathClassification, g messageGroup, rule store.AttributionRule) messageAttribution {
	b, ok := r.branchIn(c, g)
	if !ok || IsHarnessWorktreeBranch(b) {
		r.carry = pathClassification{}
		return messageAttribution{}
	}
	return messageAttribution{Rule: rule, Target: c.Target, Branch: b}
}

// worktreeAttribution is the option that turns on #823 attribution. It is nil
// unless the operator enables it.
type worktreeAttribution struct {
	index *worktreeIndex
	now   time.Time
}

// attribute sorts one file's groups by Start, in place, so they resolve in file
// order (the merger returns them in finish order, which differs when streams
// interleave), starting from carry, and returns the attribution of each usage
// event keyed by its non-empty message id, and the carry after the last group.
// A group is joined to a usage event by message id alone: an ID-less group
// updates the carry but attributes nothing. A verdict may name any configured
// target; the caller routes it (sessionSummary.routedTo). Two groups sharing
// an id (a line late by more than maxRecentMessageIDs messages) keep their
// verdict only when both agree; otherwise today's rule decides, whatever their
// order.
func (wa *worktreeAttribution) attribute(groups []messageGroup, msgs []messageUsage, carry pathClassification) (map[string]messageAttribution, pathClassification) {
	usage := make(map[string]*messageUsage, len(msgs))
	for i := range msgs {
		if msgs[i].messageID != "" {
			usage[msgs[i].messageID] = &msgs[i]
		}
	}
	slices.SortStableFunc(groups, func(a, b messageGroup) int { return cmp.Compare(a.Start, b.Start) })
	r := worktreeResolver{index: wa.index, now: wa.now, carry: carry}
	out := make(map[string]messageAttribution, len(usage))
	for _, g := range groups {
		var cwd, branch string
		u := usage[g.ID]
		if u != nil {
			cwd, branch = u.cwd, u.lineBranch
		}
		a := r.resolve(g, cwd, branch)
		if u == nil {
			continue
		}
		if prev, seen := out[g.ID]; seen && prev != a {
			a = messageAttribution{}
		}
		out[g.ID] = a
	}
	return out, r.carry
}

// routedTo returns s holding only the messages that book to target: those
// whose verdict names target and, when own (target owns s's cwd), those whose
// verdict names no configured target (today's rule, or Foreign).
func (s sessionSummary) routedTo(target string, own bool) sessionSummary {
	out := s
	out.Messages = nil
	for _, m := range s.Messages {
		if t := s.attribution[m.messageID].Target; t == target || (own && t == "") {
			out.Messages = append(out.Messages, m)
		}
	}
	return out
}

// routeSessions is filterSessionsByRepo with #823 attribution on, for the
// collector of target; targets is every configured target, target included. A
// session target owns keeps the messages no verdict routes to another target;
// a session another target owns and target does not gives target the messages
// whose verdict names it. A session no target owns is dropped whole (#823 Q6).
// A session two targets own has one owner, chosen by MatchScopes over targets
// as the watcher's matchTarget chooses it, so a message with no verdict books
// once, to the target the watcher books it to.
func routeSessions(sessions []sessionSummary, target string, targets []string) []sessionSummary {
	scopes := make([]*RepoScope, len(targets))
	for i, t := range targets {
		scopes[i] = NewRepoScope(t)
	}
	var out []sessionSummary
	for _, s := range filterSessionsByRepo(sessions, target) {
		if i := MatchScopes(scopes, s.CWD); i >= 0 && targets[i] != target {
			if r := s.routedTo(target, false); len(r.Messages) > 0 {
				out = append(out, r)
			}
			continue
		}
		out = append(out, s.routedTo(target, true))
	}
	self := NewRepoScope(target)
	var others []*RepoScope
	for _, t := range targets {
		if t != target {
			others = append(others, NewRepoScope(t))
		}
	}
	for _, s := range sessions {
		if len(others) == 0 {
			break
		}
		if self.Contains(s.CWD) || MatchScopes(others, s.CWD) < 0 {
			continue
		}
		if r := s.routedTo(target, false); len(r.Messages) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// targetNames returns the names of the targets x indexes, in configured order.
func (x *worktreeIndex) targetNames() []string {
	names := make([]string, 0, len(x.targets))
	for _, t := range x.targets {
		names = append(names, t.name)
	}
	return names
}

// carryAt re-verifies a persisted carry: the one linked worktree classification
// whose Root is root, found by classifying root at each target's own directory.
// A root that no longer verifies, or that verifies for two targets, carries
// nothing.
func (x *worktreeIndex) carryAt(root string) pathClassification {
	var got pathClassification
	if root == "" {
		return got
	}
	for _, t := range x.targets {
		c := x.classify(filepath.Join(append([]string{root}, t.sub...)...))
		if c.Class != worktreeLinked || c.Root != root || c == got {
			continue
		}
		if got.Class == worktreeLinked {
			return pathClassification{}
		}
		got = c
	}
	return got
}
