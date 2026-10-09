package collector

import (
	"encoding/json"
	"slices"
	"time"
)

const (
	// maxOpenMessages bounds the messages held open at once, one per stream.
	// Opening one more finishes the oldest early; its later lines are stale.
	maxOpenMessages = 16
	// maxRecentMessageIDs is how many finished message ids are remembered so a
	// later line of one is dropped as stale instead of reopening it.
	maxRecentMessageIDs = 64
	// maxToolPathsPerMessage caps the distinct paths kept for one message; the
	// first ones in line order are kept and Truncated records the loss.
	maxToolPathsPerMessage = maxToolPathsPerLine
	// maxMessageKeyLen bounds a stored message.id or agentId. A longer id is
	// treated as absent (the line is a message of its own); a longer agentId
	// joins the unnamed stream.
	maxMessageKeyLen = 256
)

// messageGroup is one assistant message merged across its transcript lines.
// Claude Code writes one line per content block (thinking, text, each
// tool_use), all sharing message.id and requestId and repeating usage, with
// the user tool_result lines between them.
type messageGroup struct {
	// ID is message.id. It equals the usage parser's key for the same message
	// (messageUsage.messageID) only when non-empty: an id longer than
	// maxMessageKeyLen is blanked here while the parser keys it whole. "" is
	// an ID-less line, a message of its own with no parser key: it must never
	// be paired with the parser's ID-less (__noid) events by position.
	ID string
	// Sidechain and Agent are the lines' isSidechain and agentId: the stream.
	Sidechain bool
	Agent     string
	// Start is the caller's offset of the message's first line, and End of its
	// last: a stale line of the message, added before take, moves End too.
	Start, End int64
	// First and Last are the earliest and latest line timestamps (zero when no
	// line carried one), by value, so lines out of order do not move them.
	First, Last time.Time
	// Paths are toolPathsFromLine's paths from every line, distinct, in the
	// order the lines were added.
	Paths []string
	// Truncated reports that Paths may lack the message's later references: a
	// distinct path was dropped at maxToolPathsPerMessage, or the message was
	// finished early at maxOpenMessages so its later lines are stale. A path
	// dropped by toolPathsFromLine's own per-line cap is not reported.
	Truncated bool
}

// messageMerger groups one transcript's assistant lines by message. It is pure
// (no I/O, no logging) and not safe for concurrent use; use one per transcript.
//
// A message finishes when an assistant line of a different message arrives on
// its stream. Streams are (isSidechain, agentId), so interleaved subagent lines
// never finish a parent message. Measured 2026-09-29 over 204,737 messages in
// 8,336 local transcripts: a message's lines are never interrupted by another
// message's assistant line on the same stream (the one exception was a
// re-written copy of an already-read line), requestId never differs within a
// message, and no file mixes sidechain and main lines.
//
// Adding a line with a message.id twice changes nothing: an open message
// merges it again (paths are a set, timestamps a min and max) and a finished
// one drops it as stale. An ID-less line is not deduplicated: re-adding it
// returns it again as a new message. So a caller may re-add lines from
// pendingFrom's offset into the same merger, provided fewer than
// maxRecentMessageIDs messages finished after that offset and no ID-less line
// lies after it; a fresh merger re-reading from it returns those finished
// messages again.
type messageMerger struct {
	open   []*messageGroup // in the order opened
	done   []messageGroup  // finished, not yet taken
	recent [maxRecentMessageIDs]string
	next   int
	// stale counts lines dropped because their message had already finished: a
	// late line, a re-added one, or one of a message finished early.
	stale int
}

// messageLineHead is what add decodes besides toolPathsFromLine's allowlist:
// the usage parser's jsonlEntry, plus the stream fields the parser never
// decodes, kept raw so a wrongly typed one cannot reject the line.
type messageLineHead struct {
	jsonlEntry
	IsSidechain json.RawMessage `json:"isSidechain"`
	AgentID     json.RawMessage `json:"agentId"`
}

// add merges one transcript line found at the caller's offset off. Only an
// assistant line that decodes as a jsonlEntry with message.usage is used; any
// other line is ignored. The caller feeds add from the usage parser's
// maxJSONLLine-capped scanner, which skips over-long lines and continues with
// later lines. A wrongly typed isSidechain or agentId is absent.
func (m *messageMerger) add(line []byte, off int64) {
	if len(line) > maxJSONLLine {
		return
	}
	var h messageLineHead
	if json.Unmarshal(line, &h) != nil || h.Type != "assistant" || h.Message == nil || h.Message.Usage == nil {
		return
	}
	var sidechain bool
	var agent string
	_ = json.Unmarshal(h.IsSidechain, &sidechain)
	_ = json.Unmarshal(h.AgentID, &agent)
	id := h.Message.ID
	if len(id) > maxMessageKeyLen {
		id = ""
	}
	if len(agent) > maxMessageKeyLen {
		agent = ""
	}
	if id != "" && slices.Contains(m.recent[:], id) {
		m.stale++
		for k := len(m.done) - 1; k >= 0; k-- {
			if m.done[k].ID == id {
				m.done[k].End = max(m.done[k].End, off)
				break
			}
		}
		return
	}
	i := slices.IndexFunc(m.open, func(g *messageGroup) bool { return id != "" && g.ID == id })
	if i < 0 {
		if j := slices.IndexFunc(m.open, func(g *messageGroup) bool { return g.Sidechain == sidechain && g.Agent == agent }); j >= 0 {
			m.finish(j)
		}
		if len(m.open) == maxOpenMessages {
			m.open[0].Truncated = true
			m.finish(0)
		}
		m.open = append(m.open, &messageGroup{ID: id, Sidechain: sidechain, Agent: agent, Start: off})
		i = len(m.open) - 1
	}
	g := m.open[i]
	g.End = max(g.End, off)
	if t := h.Timestamp; !t.IsZero() {
		if g.First.IsZero() || t.Before(g.First) {
			g.First = t
		}
		if t.After(g.Last) {
			g.Last = t
		}
	}
	for _, p := range toolPathsFromLine(line) {
		switch {
		case slices.Contains(g.Paths, p):
		case len(g.Paths) == maxToolPathsPerMessage:
			g.Truncated = true
		default:
			g.Paths = append(g.Paths, p)
		}
	}
	if id == "" {
		m.finish(i)
	}
}

// finish moves open[i] to the finished list and remembers its id.
func (m *messageMerger) finish(i int) {
	g := m.open[i]
	m.open = slices.Delete(m.open, i, i+1)
	m.done = append(m.done, *g)
	if g.ID != "" {
		m.recent[m.next] = g.ID
		m.next = (m.next + 1) % maxRecentMessageIDs
	}
}

// take returns the messages finished since the last take, in the order they
// finished (within one stream, the order their lines were added), and forgets
// them. Pending messages are not returned: their next line may be unread.
func (m *messageMerger) take() []messageGroup {
	out := m.done
	m.done = nil
	return out
}

// pendingFrom returns the smallest Start of the messages still open, so a
// caller that must not finalize a trailing message can hold its checkpoint
// there; ok is false when none is open.
func (m *messageMerger) pendingFrom() (off int64, ok bool) {
	for _, g := range m.open {
		if !ok || g.Start < off {
			off, ok = g.Start, true
		}
	}
	return off, ok
}

// flush finishes every open message, oldest first: for a file known to be
// complete, where no later line can arrive.
func (m *messageMerger) flush() {
	for len(m.open) > 0 {
		m.finish(0)
	}
}
