package collector

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"time"
)

// idleRelease is how long a transcript goes without an append before a read
// releases the message still open at its end. It exceeds the longest pause
// between two lines of one message, 417 s (measured 2026-09-30 over 156,834
// multi-line messages in 8,532 local transcripts), so none of the measured
// pauses would split a message; a longer one returns its tail as a late
// fragment. Idleness compares the file's mtime with the wall clock: an mtime in
// the future holds the message until the clock passes it, and a sleep longer
// than idleRelease counts as idle.
const idleRelease = 600 * time.Second

// heldChunkCap bounds non-discarded bytes before open messages are released.
// A variable so a test can shrink it.
var heldChunkCap int64 = maxJSONLChunk

// parseHeldBack is the watcher's read with #823 attribution on. It returns the
// summary of the lines before the hold-back offset, that offset, and the
// finished messages starting before it. The offset is where the next read
// starts, so a message is attributed once, from all of its lines:
//
//   - it is never past pendingFrom, so an open message is re-read whole;
//   - it is before every line of a message it holds back, and after every line
//     of a message it returns, so a fresh merger reading from it returns no
//     message returned before, unless a line of one is written after it (a
//     late line, one of a message released idle, or one of a message finished
//     early at maxOpenMessages): that line returns as a fragment, and the
//     store's upsert keeps the larger token counts and the first row's issue,
//     rule, cost and price version (#233); the fragment's read can move the
//     carry.
//
// A positive release, the size of a file seen idle, stops the read there and
// finishes every message still open at its end. The offset reaches the last
// complete line within release and the available file bytes.
//
// The summary is parsed up to that offset alone, so its metadata (the #490
// latch, NextParseSeq) is the state there, as a full parse would have it. An
// established session's no-usage tail returns a metadata-only summary so the
// checkpoint can advance with the updated LastRealBranch latch (#1095, S07-5).
// Without an established session, a no-usage prefix has no summary; if reparsing
// a held-back prefix returns none, the read retains from to parse it again.
//
// session is the id of the lines read, returned when no summary is, so a
// checkpoint that holds everything back still names its session (#919).
//
// Reads cap non-discarded bytes at heldChunkCap and finish open messages so
// a held message cannot pin from indefinitely as the file grows. Over-long
// lines are skipped without ending the scan or finishing open messages.
func parseHeldBack(path string, from int64, meta sessionMetadata, release int64) (_ *sessionSummary, _ int64, _ []messageGroup, session string, _ error) {
	var m messageMerger
	stop, size := release, release
	if size <= 0 {
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
	}
	if size-from > heldChunkCap {
		stop = from + heldChunkCap
	}
	s, end, err := parseSessionLinesCapped(path, from, meta, true, &m, release, heldChunkCap)
	if err != nil {
		return nil, 0, nil, "", err
	}
	if s != nil {
		session = s.SessionID
	}
	if stop > 0 {
		m.flush()
	}
	open, isOpen := m.pendingFrom()
	groups := m.take()
	cut := end
	if isOpen {
		cut = heldFrom(groups, open)
	}
	if cut < end {
		if cut <= from {
			return nil, from, nil, session, nil
		}
		if s, end, err = parseSessionLines(path, from, meta, true, nil, cut); err != nil {
			return nil, 0, nil, "", err
		}
		if s == nil {
			return nil, from, nil, session, nil
		}
	}
	groups = slices.DeleteFunc(groups, func(g messageGroup) bool { return g.Start >= cut })
	return s, end, groups, session, nil
}

// heldFrom returns the offset open lowered to the Start of every finished group
// with a line at or after it, repeatedly, so no group straddles the result.
func heldFrom(groups []messageGroup, open int64) int64 {
	cut := open
	for moved := true; moved; {
		moved = false
		for _, g := range groups {
			if g.Start < cut && g.End >= cut {
				cut, moved = g.Start, true
			}
		}
	}
	return cut
}

// attributeHeld attributes s's messages from groups, starting from the carry
// persisted as carried, and records the carry after them in s.LastWorktree.
func (w *Watcher) attributeHeld(s *sessionSummary, groups []messageGroup, carried string) {
	wa := worktreeAttribution{index: w.worktrees, now: w.clock()}
	var carry pathClassification
	s.attribution, carry = wa.attribute(groups, s.Messages, w.worktrees.carryAt(carried))
	s.LastWorktree = carry.Root
}

// clock is w.now, or time.Now.
func (w *Watcher) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// idleWait is w.idleAfter, or idleRelease.
func (w *Watcher) idleWait() time.Duration {
	if w.idleAfter > 0 {
		return w.idleAfter
	}
	return idleRelease
}

// joinRouted is the watcher's join with #823 attribution on: each of s's
// messages is joined against the commits and slug of the target it books to
// (sessionSummary.routedTo), own being the target that owns s's cwd. A
// message whose verdict names a target not in targets books nowhere; their
// count, never a path, is logged.
func (w *Watcher) joinRouted(ctx context.Context, s *sessionSummary, own resolvedPath, targets []resolvedPath, developer string, cache *commitCache, logger *slog.Logger) []TokenEvent {
	var events []TokenEvent
	booked := 0
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if seen[t.name] {
			continue
		}
		seen[t.name] = true
		part := s.routedTo(t.name, t.name == own.name)
		booked += len(part.Messages)
		if len(part.Messages) == 0 {
			continue
		}
		events = append(events, joinSessionsToCommits([]sessionSummary{part}, cache.get(ctx, t.abs, logger), developer, w.repoSlug(t, logger))...)
	}
	if n := len(s.Messages) - booked; n > 0 {
		logger.Warn("watcher: worktree verdicts name an unwatched target; messages not booked", "messages", n)
	}
	return events
}
