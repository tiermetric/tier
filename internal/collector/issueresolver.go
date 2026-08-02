package collector

import (
	"context"
	"fmt"
	"time"
)

// IssueResolver maps a (branch, timestamp) pair to the issue id a token event
// should be attributed to, using a snapshot of one repository's git log.
//
// It is the exported face of the attribution rule the Claude Code JSONL join
// already uses (resolveMessageIssue), extracted so a SECOND session-file
// collector — the Codex rollout-log reader in
// internal/collector/codexrollout — attributes cost by exactly the same rule
// instead of growing a parallel one. Resolve delegates straight to
// resolveMessageIssue, so there is one implementation and no way for the two
// paths to disagree about the same branch.
//
// Resolution order (see resolveMessageIssue for the full rationale):
//
//  1. Commit match, feature branches ONLY — a commit whose branch shares this
//     branch's leaf name and whose timestamp is within ±joinWindow contributes
//     its issue. An exploratory branch (main/master/HEAD/empty) is deliberately
//     skipped here so it cannot inherit a nearby merge's `closes #N`.
//  2. Branch-name derivation — issueref.FromBranch on a feature branch that
//     names its issue, even when no commit matched.
//  3. Labeled unattributed bucket — UnattributedMain / UnattributedDetachedHEAD
//     / UnattributedNoIssue. Cost is never dropped; it lands in the developer's
//     denominator under an honest label naming WHY it did not attribute.
//
// Resolve NEVER returns the empty string.
//
// Thread safety: the commit index is built once in NewIssueResolver and is
// read-only thereafter, so a single IssueResolver is safe for concurrent
// Resolve calls. The snapshot is fixed at construction — a commit that lands
// afterwards does not retroactively re-attribute; build a fresh resolver per
// scan pass to pick up new commits.
type IssueResolver struct {
	commitsByLeaf map[string][]gitCommit
}

// NewIssueResolver snapshots repoPath's git log since the given time and indexes
// it for attribution. repoPath must be a git checkout (a .git directory OR a
// .git file, so worktrees and submodules are accepted).
//
// Returns an error when the path is not a checkout or `git log` fails — both are
// operator-visible misconfigurations, not conditions to degrade past silently.
func NewIssueResolver(ctx context.Context, repoPath string, since time.Time) (*IssueResolver, error) {
	if err := validateGitRepo(repoPath); err != nil {
		return nil, fmt.Errorf("repo validation: %w", err)
	}
	commits, err := gitLog(ctx, repoPath, since)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	return &IssueResolver{commitsByLeaf: indexCommitsByLeaf(commits)}, nil
}

// Resolve returns the issue id (or labeled unattributed bucket) for one event
// that happened on branch at ts. A nil receiver resolves to the labeled bucket
// for the branch — callers that could not build a resolver (no git log
// available) still get honest, denominator-preserving attribution rather than a
// nil-pointer panic or a dropped event.
func (r *IssueResolver) Resolve(branch string, ts time.Time) string {
	if r == nil {
		return bucketForBranch(branch)
	}
	return resolveMessageIssue(branch, ts, r.commitsByLeaf)
}
