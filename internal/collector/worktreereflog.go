package collector

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxReflogFile bounds the bytes read from a worktree's HEAD reflog. A larger
// reflog is unresolvable, never read in part: dropping entries would move the
// intervals an answer is read from.
const maxReflogFile = 1 << 20

// reflogExpireUnreachable is git's default gc.reflogExpireUnreachable. Expiry
// can drop a whole detour (a checkout and its return) and leave a chain that
// still links, so no time older than this before now is answered.
const reflogExpireUnreachable = 30 * 24 * time.Hour

// mtimeSlack is how far a file's mtime may pass the reflog time git wrote with
// it: reflog times are whole seconds, taken just before the write.
const mtimeSlack = time.Second

// reflogEntry is one line of a HEAD reflog:
// "<old> <new> <name> <<email>> <unix-ts> <tz>[\t<message>]".
type reflogEntry struct {
	old, new string
	ts       int64
	msg      string
}

// branchAt returns the branch checked out in the verified linked worktree c at
// time t, from the worktree's HEAD reflog and HEAD file alone; it never runs
// git. It fails closed (ok false) on anything it cannot prove: t before the
// first entry, a reflog that is missing, not a regular file, owned by another
// uid, over maxReflogFile or not in git's format, and a HEAD that was detached
// or cannot be pinned to one branch. It also refuses t older than now minus
// reflogExpireUnreachable, t before the admin dir's gitdir was last written (a
// worktree move rewrites it and logs nothing), a file whose newest entry is
// later than its own mtime (a future GIT_COMMITTER_DATE), and any HEAD, logs or
// logs/HEAD that is world-writable. It refuses every interval HEAD's file names
// (those after the last entry that set HEAD's target) when HEAD's mtime is
// later than logs/HEAD's or than the newest entry's second: git writes HEAD's
// lock before it appends the entry that logs the move, so a HEAD written later
// moved unlogged (an orphan switch logs nothing).
//
// Residuals: a repo configured with a shorter unreachable-expiry can still
// drop a detour inside the horizon; a backdated GIT_COMMITTER_DATE moves a
// boundary earlier and nothing on disk shows it; an operator-set
// GIT_REFLOG_ACTION can make any entry read as a message it is not; and an
// unlogged HEAD change is missed when the filesystem's mtimes are too coarse
// to order the two writes, or when logs/HEAD is rewritten after it (reflog
// expire) and it fell inside the newest entry's second.
//
// A "checkout: moving from X to Y" entry's Y is only the name given to
// checkout (a branch, a tag, a commit, or a branch under --detach), so it is
// never trusted alone. What git writes as X is authoritative: the branch HEAD
// was on, or a full object id when HEAD was detached. So the branch after an
// entry is read from the NEXT checkout's X, or from the HEAD file when no entry
// follows, or from a "(finish): returning to refs/heads/B" entry, and every
// such source present must agree with each other and with Y.
//
// Reflog times are whole seconds, so an entry in t's own second may be before
// or after t: every state inside that second must agree, else ok is false.
func (x *worktreeIndex) branchAt(c pathClassification, t, now time.Time) (string, bool) {
	if c.Class != worktreeLinked || !filepath.IsAbs(c.AdminDir) || t.Before(now.Add(-reflogExpireUnreachable)) {
		return "", false
	}
	if gd, err := os.Lstat(filepath.Join(c.AdminDir, "gitdir")); err != nil || t.Before(gd.ModTime()) {
		return "", false
	}
	// HEAD is read before the reflog, so a checkout landing between the two
	// reads can only add an entry newer than the HEAD read, whose "to" then
	// disagrees with HEAD. This relies on git logging an entry no later than it
	// moves HEAD (refs/files-backend.c writes the log before committing the ref).
	head, headInfo, ok := x.readOwnedRegular(filepath.Join(c.AdminDir, "HEAD"), maxGitdirFile)
	if !ok {
		return "", false
	}
	logs := filepath.Join(c.AdminDir, "logs")
	if info, err := os.Lstat(logs); err != nil || !info.IsDir() || !x.owned(logs, info) || info.Mode().Perm()&0o002 != 0 {
		return "", false
	}
	raw, logInfo, ok := x.readOwnedRegular(filepath.Join(logs, "HEAD"), maxReflogFile)
	if !ok {
		return "", false
	}
	entries, ok := parseReflog(string(raw))
	if !ok {
		return "", false
	}
	newest := time.Unix(entries[len(entries)-1].ts, 0)
	if newest.After(logInfo.ModTime().Add(mtimeSlack)) {
		return "", false
	}
	current, onBranch := strings.CutPrefix(strings.TrimSuffix(string(head), "\n"), "ref: refs/heads/")
	if !onBranch || headInfo.ModTime().After(logInfo.ModTime()) || headInfo.ModTime().After(newest.Add(mtimeSlack)) {
		current = "" // detached now, or HEAD written after the reflog last was
	}
	at := t.Unix()
	lo, hi := -1, -1
	for i, e := range entries {
		if e.ts < at {
			lo = i
		}
		if e.ts <= at {
			hi = i
		}
	}
	if lo < 0 {
		return "", false
	}
	var branch string
	for k := lo; k <= hi; k++ {
		b, ok := branchAfter(entries, k, current)
		if !ok || (k > lo && b != branch) {
			return "", false
		}
		branch = b
	}
	return branch, true
}

// branchAfter returns the branch HEAD pointed at after entries[k]. current is
// the HEAD file's branch, "" when HEAD is detached now.
func branchAfter(es []reflogEntry, k int, current string) (string, bool) {
	s := k // the entry that last set HEAD's target, at or before k
	for s >= 0 && reflogPreserves(es[s].msg) {
		s--
	}
	n := k + 1 // the next entry that sets it
	for n < len(es) && reflogPreserves(es[n].msg) {
		n++
	}
	// A gap (an orphan, a rename elsewhere) could hide a checkout. Expiry that
	// drops a whole detour leaves no gap; branchAt's horizon covers that.
	for j := max(s+1, 1); j <= min(n, len(es)-1); j++ {
		if es[j].old != es[j-1].new {
			return "", false
		}
	}
	var proven, named string
	if s >= 0 {
		if _, to, ok := parseCheckoutMsg(es[s].msg); ok {
			named = to
		} else if b, ok := parseFinishMsg(es[s].msg); ok {
			proven = b
		}
	}
	back := current
	if n < len(es) {
		back = ""
		if from, _, ok := parseCheckoutMsg(es[n].msg); ok {
			back = from
			if isObjectID(from) {
				return "", false
			}
		}
	} else if current == "" {
		return "", false
	}
	if back != "" {
		if proven != "" && proven != back {
			return "", false
		}
		proven = back
	}
	if (named != "" && named != proven) || !validBranchName(proven) {
		return "", false
	}
	return proven, true
}

// reflogPreserves reports whether an entry's message is one that leaves HEAD
// on the ref it was on (it moves that ref, or a detached HEAD, only). Any
// message not listed is treated as one that may have changed it. The action is
// the text before the first ": "; what follows may be a commit subject, so it
// is matched only where git writes a fixed text there (reset, merge, pull).
func reflogPreserves(msg string) bool {
	act, how, ok := strings.Cut(msg, ": ")
	if !ok {
		return false
	}
	switch act {
	case "commit", "commit (amend)", "commit (merge)", "commit (initial)", "cherry-pick", "revert", "am":
		return true
	case "reset":
		return strings.HasPrefix(how, "moving to ")
	}
	// merge <names>, and pull <args> when it merges: a rebasing pull's action
	// ends in "(start)", "(pick)" or "(finish)". A bare "merge <name>" is what a
	// checkout inside a merge hook writes, and it has no ": ".
	if (strings.HasPrefix(act, "merge ") || act == "pull" || strings.HasPrefix(act, "pull ")) && !strings.ContainsAny(act, "()") {
		return strings.HasPrefix(how, "Fast-forward") || strings.HasPrefix(how, "Merge made by the ") || how == "In-index merge"
	}
	return false
}

// parseFinishMsg reads the branch from a rebase's "<action> (finish):
// returning to refs/heads/B", where the action is git's own (rebase, rebase
// -i, or a rebasing pull) and never text a commit subject can supply.
func parseFinishMsg(msg string) (string, bool) {
	act, rest, _ := strings.Cut(msg, ": ")
	base, ok := strings.CutSuffix(act, " (finish)")
	if !ok || strings.ContainsAny(base, "()") ||
		base != "rebase" && base != "rebase -i" && base != "pull" && !strings.HasPrefix(base, "pull ") {
		return "", false
	}
	return strings.CutPrefix(rest, "returning to refs/heads/")
}

// parseCheckoutMsg splits "checkout: moving from X to Y". Git ref names hold
// no space, so any other shape is not a checkout this parser can read.
func parseCheckoutMsg(msg string) (from, to string, ok bool) {
	rest, ok := strings.CutPrefix(msg, "checkout: moving from ")
	if !ok {
		return "", "", false
	}
	f := strings.Split(rest, " ")
	if len(f) != 3 || f[1] != "to" || f[0] == "" || f[2] == "" {
		return "", "", false
	}
	return f[0], f[2], true
}

// parseReflog parses a whole reflog. Any line not in git's format, a CR, a
// final line without its newline, or a time that goes backwards fails it all.
func parseReflog(s string) ([]reflogEntry, bool) {
	if s == "" || !strings.HasSuffix(s, "\n") || strings.ContainsRune(s, '\r') {
		return nil, false
	}
	var es []reflogEntry
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		header, msg, _ := strings.Cut(line, "\t")
		f := strings.Split(header, " ")
		if len(f) < 5 || !isObjectID(f[0]) || len(f[1]) != len(f[0]) || !isObjectID(f[1]) {
			return nil, false
		}
		ts, err := strconv.ParseInt(f[len(f)-2], 10, 64)
		tz := f[len(f)-1]
		if err != nil || len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') || !allDigits(tz[1:]) {
			return nil, false
		}
		if len(es) > 0 && ts < es[len(es)-1].ts {
			return nil, false
		}
		es = append(es, reflogEntry{old: f[0], new: f[1], ts: ts, msg: msg})
	}
	return es, true
}

// readOwnedRegular reads path when it is a regular file (not a symlink, FIFO
// or device) owned by the index's uid and at most limit bytes. The open does
// not block on a FIFO. Lstat sees a symlink as itself, so the opened file must
// be the same file Lstat saw; a larger file is refused, never read in part,
// and so is a world-writable one. It returns the opened file's info.
func (x *worktreeIndex) readOwnedRegular(path string, limit int64) ([]byte, os.FileInfo, bool) {
	info, err := os.Lstat(path)
	if err != nil || !x.owned(path, info) {
		return nil, nil, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, nil, false
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Mode().Perm()&0o002 != 0 {
		return nil, nil, false
	}
	buf, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(buf)) > limit {
		return nil, nil, false
	}
	return buf, opened, true
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object id, which is
// also what git writes as a checkout's "from" when HEAD was detached.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// validBranchName is a subset of git check-ref-format: it refuses what git
// never names a branch, and "(invalid)", git's "from" for an unreadable HEAD.
func validBranchName(b string) bool {
	if b == "" || b == "HEAD" || b == "(invalid)" || b[0] == '-' ||
		strings.Contains(b, "..") || strings.Contains(b, "@{") || strings.ContainsAny(b, "~^:?*[\\") {
		return false
	}
	for i := 0; i < len(b); i++ {
		if b[i] <= ' ' || b[i] == 0x7f {
			return false
		}
	}
	return true
}
