package collector

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fixture lines follow what Claude Code writes today: one assistant line per
// content block, all sharing message.id and requestId and repeating usage,
// with the user tool_result line between tool_use lines. Paths are synthetic;
// "ROOT/" becomes an absolute root on the running OS (tpFix).

const (
	mmThinking = `[{"type":"thinking","thinking":"","signature":"c2lnbmF0dXJl"}]`
	mmText     = `[{"type":"text","text":"Reading the file."}]`
)

func mmRead(p string) string {
	return `[{"type":"tool_use","id":"toolu_01Read","name":"Read","input":{"file_path":"` + p + `"},"caller":{"type":"direct"}}]`
}

func mmEdit(p string) string {
	return `[{"type":"tool_use","id":"toolu_01Edit","name":"Edit","input":{"file_path":"` + p + `","old_string":"a","new_string":"b"}}]`
}

// mmMain is a main-transcript assistant line; mmSide a subagent's line.
func mmMain(id, uuid, ts, content string) []byte { return mmLine(id, uuid, ts, false, "", content) }

func mmSide(agent, id, uuid, ts, content string) []byte {
	return mmLine(id, uuid, ts, true, agent, content)
}

func mmLine(id, uuid, ts string, sidechain bool, agent, content string) []byte {
	agentKey := ""
	if agent != "" {
		agentKey = `"agentId":"` + agent + `",`
	}
	return []byte(tpFix(fmt.Sprintf(`{"parentUuid":"p-%s","isSidechain":%t,%s`+
		`"message":{"model":"claude-opus-4-5","id":"%s","type":"message","role":"assistant","content":%s,`+
		`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":120,`+
		`"cache_read_input_tokens":151464,"output_tokens":317,"cache_creation":{"ephemeral_1h_input_tokens":120,`+
		`"ephemeral_5m_input_tokens":0},"service_tier":"standard"}},"requestId":"req_%s","type":"assistant",`+
		`"uuid":"%s","timestamp":"%s","userType":"external","entrypoint":"cli","cwd":"ROOT/work/app",`+
		`"sessionId":"5d0c7a3e-2222-4b1c-8d9e-000000000003","version":"2.1.215","gitBranch":"main"}`,
		uuid, sidechain, agentKey, id, content, id, uuid, ts)))
}

// mmResult is the user tool_result line Claude Code writes between tool_use
// lines of one message.
func mmResult(uuid string) []byte {
	return []byte(`{"parentUuid":"p-` + uuid + `","isSidechain":false,"type":"user","message":{"role":"user","content":` +
		`[{"tool_use_id":"toolu_01Read","type":"tool_result","content":"file text"}]},"uuid":"` + uuid + `",` +
		`"timestamp":"2026-09-29T10:00:03.000Z","sourceToolAssistantUUID":"x"}`)
}

func mmTime(sec, milli int) time.Time {
	return time.Date(2026, 9, 29, 10, 0, sec, milli*int(time.Millisecond), time.UTC)
}

// mmFeed adds lines at offsets 100, 200, ... after base.
func mmFeed(m *messageMerger, base int64, lines ...[]byte) {
	for i, l := range lines {
		m.add(l, base+int64(i+1)*100)
	}
}

// mmCheck compares groups with End cleared: End is pinned by
// TestMessageMerger_EndIsTheLastLineStaleIncluded.
func mmCheck(t *testing.T, got, want []messageGroup) {
	t.Helper()
	for i := range got {
		got[i].End = 0
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups:\n got %+v\nwant %+v", got, want)
	}
}

// messageA is one message across five lines: thinking, a Read, its result, an
// Edit of a second file and a repeated Read of the first.
func messageA() [][]byte {
	return [][]byte{
		mmMain("msg_A", "a1", "2026-09-29T10:00:01.000Z", mmThinking),
		mmMain("msg_A", "a2", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/one/x.go")),
		mmResult("r1"),
		mmMain("msg_A", "a3", "2026-09-29T10:00:04.000Z", mmEdit("ROOT/wt/one/y.go")),
		mmMain("msg_A", "a4", "2026-09-29T10:00:05.500Z", mmRead("ROOT/wt/one/x.go")),
	}
}

var wantA = messageGroup{
	ID: "msg_A", Start: 100, First: mmTime(1, 0), Last: mmTime(5, 500),
	Paths: tpWant("ROOT/wt/one/x.go", "ROOT/wt/one/y.go"),
}

func TestMessageMerger_MergesOneMessageAcrossLines(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0, messageA()...)
	if got := m.take(); len(got) != 0 {
		t.Fatalf("a message with no later message finished early: %+v", got)
	}
	m.add(mmMain("msg_B", "b1", "2026-09-29T10:00:07.000Z", mmText), 600)
	mmCheck(t, m.take(), []messageGroup{wantA})
	if off, ok := m.pendingFrom(); !ok || off != 600 {
		t.Errorf("pendingFrom = %d, %v; want 600, true", off, ok)
	}
	if m.take() != nil {
		t.Error("take returned a group twice")
	}
	m.flush()
	mmCheck(t, m.take(), []messageGroup{{ID: "msg_B", Start: 600, First: mmTime(7, 0), Last: mmTime(7, 0)}})
	if _, ok := m.pendingFrom(); ok {
		t.Error("pendingFrom reports a message after flush")
	}
}

// A tool call of the trailing message arrives in a later read of the growing
// file: it must join the message, whether the merger is kept across reads or a
// fresh one re-reads from pendingFrom.
func TestMessageMerger_ToolCallInLaterRead(t *testing.T) {
	lines := messageA()
	next := mmMain("msg_B", "b1", "2026-09-29T10:00:07.000Z", mmText)

	var kept messageMerger
	mmFeed(&kept, 0, lines[:2]...) // read 1 ends after the first Read
	if got := kept.take(); len(got) != 0 {
		t.Fatalf("read 1 finished the trailing message: %+v", got)
	}
	hold, ok := kept.pendingFrom()
	if !ok || hold != 100 {
		t.Fatalf("pendingFrom after read 1 = %d, %v; want 100, true", hold, ok)
	}
	mmFeed(&kept, 200, lines[2:]...) // read 2: the rest, at offsets 300..500
	kept.add(next, 600)
	mmCheck(t, kept.take(), []messageGroup{wantA})

	var fresh messageMerger // a restart re-reads everything from the hold
	mmFeed(&fresh, hold-100, lines...)
	fresh.add(next, 600)
	mmCheck(t, fresh.take(), []messageGroup{wantA})

	var refed messageMerger // the same merger re-reads from the hold
	mmFeed(&refed, 0, lines[:2]...)
	mmFeed(&refed, hold-100, lines...)
	refed.add(next, 600)
	mmCheck(t, refed.take(), []messageGroup{wantA})
}

func TestMessageMerger_ReAddingLinesIsIdempotent(t *testing.T) {
	var m messageMerger
	all := append(messageA(), mmMain("msg_B", "b1", "2026-09-29T10:00:07.000Z", mmRead("ROOT/wt/two/z.go")))
	mmFeed(&m, 0, all...)
	mmFeed(&m, 0, all...)
	m.flush()
	mmCheck(t, m.take(), []messageGroup{wantA, {
		ID: "msg_B", Start: 600, First: mmTime(7, 0), Last: mmTime(7, 0), Paths: tpWant("ROOT/wt/two/z.go"),
	}})
	if m.stale != 4 { // msg_A's four assistant lines re-added after it finished
		t.Errorf("stale = %d, want 4", m.stale)
	}
}

// interleaved is a parent message with two subagents' lines between its own,
// at offsets 100..700.
func interleaved() [][]byte {
	return [][]byte{
		mmMain("msg_P", "p1", "2026-09-29T10:00:01.000Z", mmRead("ROOT/wt/parent/a.go")),
		mmSide("agent-1", "msg_S1", "s1", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/sub1/b.go")),
		mmSide("agent-2", "msg_S2", "s2", "2026-09-29T10:00:03.000Z", mmRead("ROOT/wt/sub2/c.go")),
		mmSide("agent-1", "msg_S1", "s3", "2026-09-29T10:00:04.000Z", mmEdit("ROOT/wt/sub1/d.go")),
		mmMain("msg_P", "p2", "2026-09-29T10:00:05.000Z", mmEdit("ROOT/wt/parent/e.go")),
		mmSide("agent-1", "msg_S3", "s4", "2026-09-29T10:00:06.000Z", mmText),
		mmMain("msg_Q", "q1", "2026-09-29T10:00:07.000Z", mmText),
	}
}

// Subagent lines between a parent message's lines do not finish it, and each
// agent is its own stream.
func TestMessageMerger_InterleavedSidechainLines(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0, interleaved()...)
	mmCheck(t, m.take(), []messageGroup{
		{ID: "msg_S1", Sidechain: true, Agent: "agent-1", Start: 200, First: mmTime(2, 0), Last: mmTime(4, 0),
			Paths: tpWant("ROOT/wt/sub1/b.go", "ROOT/wt/sub1/d.go")},
		{ID: "msg_P", Start: 100, First: mmTime(1, 0), Last: mmTime(5, 0),
			Paths: tpWant("ROOT/wt/parent/a.go", "ROOT/wt/parent/e.go")},
	})
	if off, _ := m.pendingFrom(); off != 300 { // msg_S2 is still open
		t.Errorf("pendingFrom = %d, want 300", off)
	}
	m.flush()
	var ids []string
	for _, g := range m.take() {
		ids = append(ids, g.ID)
	}
	if want := []string{"msg_S2", "msg_S3", "msg_Q"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("flush order = %q, want %q", ids, want)
	}
}

// A sidechain line with no agentId is its own stream, apart from the main one:
// it does not finish the parent message around it.
func TestMessageMerger_AgentlessSidechainLineKeepsParentOpen(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0,
		mmMain("msg_P", "p1", "2026-09-29T10:00:01.000Z", mmRead("ROOT/wt/parent/a.go")),
		mmSide("", "msg_S0", "s1", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/sub/b.go")),
		mmMain("msg_P", "p2", "2026-09-29T10:00:03.000Z", mmEdit("ROOT/wt/parent/e.go")),
	)
	if got := m.take(); len(got) != 0 {
		t.Fatalf("the agentId-less sidechain line finished a message: %+v", got)
	}
	m.flush()
	mmCheck(t, m.take(), []messageGroup{
		{ID: "msg_P", Start: 100, First: mmTime(1, 0), Last: mmTime(3, 0),
			Paths: tpWant("ROOT/wt/parent/a.go", "ROOT/wt/parent/e.go")},
		{ID: "msg_S0", Sidechain: true, Start: 200, First: mmTime(2, 0), Last: mmTime(2, 0),
			Paths: tpWant("ROOT/wt/sub/b.go")},
	})
	if m.stale != 0 {
		t.Errorf("stale = %d, want 0", m.stale)
	}
}

// Re-adding the interleaved lines from pendingFrom into the same merger drops
// the two finished messages' lines as stale and returns each message once.
func TestMessageMerger_ReAddFromPendingFromAfterInterleaving(t *testing.T) {
	var m messageMerger
	lines := interleaved()
	mmFeed(&m, 0, lines...)
	hold, ok := m.pendingFrom()
	if !ok || hold != 300 {
		t.Fatalf("pendingFrom = %d, %v; want 300, true", hold, ok)
	}
	mmFeed(&m, 200, lines[2:]...) // offsets 300..700 again
	m.flush()
	type idStart struct {
		id    string
		start int64
	}
	var got []idStart
	for _, g := range m.take() {
		got = append(got, idStart{g.ID, g.Start})
	}
	want := []idStart{{"msg_S1", 200}, {"msg_P", 100}, {"msg_S2", 300}, {"msg_S3", 600}, {"msg_Q", 700}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
	if m.stale != 2 { // msg_S1's line at 400 and msg_P's at 500
		t.Errorf("stale = %d, want 2", m.stale)
	}
}

// The merger remembers the last 64 finished ids: a line of the 64th most
// recently finished message is stale, one of the 65th opens a new message.
func TestMessageMerger_RemembersLast64FinishedIDs(t *testing.T) {
	var m messageMerger
	for i := range 65 { // msg_00 .. msg_64, one stream
		m.add(mmMain(fmt.Sprintf("msg_%02d", i), "u", "2026-09-29T10:00:01.000Z", mmText), int64(i)*100)
	}
	m.flush()
	if got := m.take(); len(got) != 65 || got[0].ID != "msg_00" || got[64].ID != "msg_64" {
		t.Fatalf("finished %d messages, want msg_00 .. msg_64", len(got))
	}
	m.add(mmMain("msg_01", "u", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/one/x.go")), 7000)
	if _, open := m.pendingFrom(); open || m.stale != 1 {
		t.Errorf("64th most recent: open = %v, stale = %d; want false, 1", open, m.stale)
	}
	m.add(mmMain("msg_00", "u", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/one/x.go")), 7100)
	m.flush()
	mmCheck(t, m.take(), []messageGroup{{
		ID: "msg_00", Start: 7100, First: mmTime(2, 0), Last: mmTime(2, 0), Paths: tpWant("ROOT/wt/one/x.go"),
	}})
	if m.stale != 1 {
		t.Errorf("65th most recent was dropped: stale = %d, want 1", m.stale)
	}
}

func TestMessageMerger_OutOfOrderTimestampsAndLateLine(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0,
		mmMain("msg_A", "a1", "2026-09-29T10:00:09.000Z", mmRead("ROOT/wt/one/x.go")),
		mmMain("msg_A", "a2", "2026-09-29T10:00:02.000Z", mmText),
		mmMain("msg_A", "a3", "2026-09-29T10:00:05.000Z", mmText),
		mmMain("msg_B", "b1", "2026-09-29T10:00:10.000Z", mmText),
		// A late copy of msg_A's line naming a new path: dropped, not reopened.
		mmMain("msg_A", "a4", "2026-09-29T10:00:11.000Z", mmRead("ROOT/wt/other/late.go")),
	)
	m.flush()
	mmCheck(t, m.take(), []messageGroup{
		{ID: "msg_A", Start: 100, First: mmTime(2, 0), Last: mmTime(9, 0), Paths: tpWant("ROOT/wt/one/x.go")},
		{ID: "msg_B", Start: 400, First: mmTime(10, 0), Last: mmTime(10, 0)},
	})
	if m.stale != 1 {
		t.Errorf("stale = %d, want 1", m.stale)
	}
}

func TestMessageMerger_IDLessLineIsItsOwnMessage(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0,
		mmMain("msg_A", "a1", "2026-09-29T10:00:01.000Z", mmRead("ROOT/wt/one/x.go")),
		mmMain("", "n1", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/one/y.go")),
		mmMain("", "n2", "2026-09-29T10:00:03.000Z", mmRead("ROOT/wt/one/y.go")),
	)
	mmCheck(t, m.take(), []messageGroup{
		{ID: "msg_A", Start: 100, First: mmTime(1, 0), Last: mmTime(1, 0), Paths: tpWant("ROOT/wt/one/x.go")},
		{Start: 200, First: mmTime(2, 0), Last: mmTime(2, 0), Paths: tpWant("ROOT/wt/one/y.go")},
		{Start: 300, First: mmTime(3, 0), Last: mmTime(3, 0), Paths: tpWant("ROOT/wt/one/y.go")},
	})
	if _, ok := m.pendingFrom(); ok {
		t.Error("an ID-less line was held open")
	}
}

func TestMessageMerger_IgnoresLinesTheUsageParserWouldNot(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0,
		mmResult("r1"),
		[]byte(`{"type":"assistant","message":{"id":"msg_T`), // truncated
		[]byte(strings.Replace(string(mmMain("msg_BadTS", "x1", "2026-09-29T10:00:01.000Z", mmText)),
			`"timestamp":"2026-09-29T10:00:01.000Z"`, `"timestamp":"yesterday"`, 1)),
		[]byte(`{"type":"assistant","message":null,"uuid":"x2","timestamp":"2026-09-29T10:00:02.000Z"}`),
		[]byte(`{"type":"summary","summary":"s","leafUuid":"x3"}`),
		// A wrongly typed isSidechain is absent, not a reason to drop the line.
		[]byte(strings.Replace(string(mmMain("msg_C", "c1", "2026-09-29T10:00:03.000Z", mmRead("ROOT/wt/one/x.go"))),
			`"isSidechain":false`, `"isSidechain":"no"`, 1)),
	)
	m.flush()
	mmCheck(t, m.take(), []messageGroup{
		{ID: "msg_C", Start: 600, First: mmTime(3, 0), Last: mmTime(3, 0), Paths: tpWant("ROOT/wt/one/x.go")},
	})
}

// A line the usage parser drops (a wrongly typed message.id) or does not
// count (no message.usage) neither opens a message nor finishes the open one.
func TestMessageMerger_ParserRejectedLinesDoNotFinishAMessage(t *testing.T) {
	var m messageMerger
	mmFeed(&m, 0,
		mmMain("msg_A", "a1", "2026-09-29T10:00:01.000Z", mmRead("ROOT/wt/one/x.go")),
		[]byte(strings.Replace(string(mmMain("msg_A", "a2", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/one/bad.go"))),
			`"id":"msg_A"`, `"id":7`, 1)),
		[]byte(strings.Replace(string(mmMain("msg_N", "n1", "2026-09-29T10:00:03.000Z", mmRead("ROOT/wt/one/nousage.go"))),
			`"usage":`, `"x_usage":`, 1)),
		mmMain("msg_A", "a3", "2026-09-29T10:00:04.000Z", mmEdit("ROOT/wt/one/y.go")),
	)
	m.flush()
	mmCheck(t, m.take(), []messageGroup{
		{ID: "msg_A", Start: 100, First: mmTime(1, 0), Last: mmTime(4, 0), Paths: tpWant("ROOT/wt/one/x.go", "ROOT/wt/one/y.go")},
	})
	if m.stale != 0 {
		t.Errorf("stale = %d, want 0", m.stale)
	}
}

func TestMessageMerger_BoundsMemory(t *testing.T) {
	var m messageMerger
	for i := range maxOpenMessages + 1 { // one stream too many finishes the oldest
		m.add(mmSide(fmt.Sprintf("agent-%02d", i), fmt.Sprintf("msg_%02d", i), "u", "2026-09-29T10:00:01.000Z", mmText), int64(i))
	}
	if got := m.take(); len(got) != 1 || got[0].ID != "msg_00" || !got[0].Truncated {
		t.Fatalf("overflow finished %+v, want msg_00 alone, truncated", got)
	}
	m.add(mmSide("agent-00", "msg_00", "u", "2026-09-29T10:00:02.000Z", mmRead("ROOT/wt/one/x.go")), 99)
	if m.stale != 1 {
		t.Errorf("a line of the early-finished message: stale = %d, want 1", m.stale)
	}
	if len(m.open) != maxOpenMessages {
		t.Errorf("open = %d, want %d", len(m.open), maxOpenMessages)
	}

	var p messageMerger
	for i := range maxToolPathsPerMessage + 2 {
		p.add(mmMain("msg_P", "u", "2026-09-29T10:00:01.000Z", mmRead(fmt.Sprintf("ROOT/wt/one/f%02d.go", i))), 0)
	}
	p.flush()
	g := p.take()[0]
	if len(g.Paths) != 16 || g.Paths[15] != tpFix("ROOT/wt/one/f15.go") || !g.Truncated {
		t.Errorf("paths = %d (last %q), truncated = %v; want 16 ending f15.go, true", len(g.Paths), g.Paths[len(g.Paths)-1], g.Truncated)
	}

	var k messageMerger
	long := strings.Repeat("x", maxMessageKeyLen+1)
	mmFeed(&k, 0,
		mmSide(long, long, "u1", "2026-09-29T10:00:01.000Z", mmText),
		mmSide("", "msg_S", "u2", "2026-09-29T10:00:02.000Z", mmText),
	)
	k.flush()
	mmCheck(t, k.take(), []messageGroup{
		{Sidechain: true, Start: 100, First: mmTime(1, 0), Last: mmTime(1, 0)},
		{ID: "msg_S", Sidechain: true, Start: 200, First: mmTime(2, 0), Last: mmTime(2, 0)},
	})

	var b messageMerger // a 256-byte id is kept, a 257-byte one blanked
	id256, id257 := strings.Repeat("y", 256), strings.Repeat("z", 257)
	mmFeed(&b, 0,
		mmMain(id256, "u1", "2026-09-29T10:00:01.000Z", mmText),
		mmMain(id256, "u2", "2026-09-29T10:00:02.000Z", mmText),
		mmMain(id257, "u3", "2026-09-29T10:00:03.000Z", mmText),
	)
	b.flush()
	mmCheck(t, b.take(), []messageGroup{
		{ID: id256, Start: 100, First: mmTime(1, 0), Last: mmTime(2, 0)},
		{Start: 300, First: mmTime(3, 0), Last: mmTime(3, 0)},
	})
}
