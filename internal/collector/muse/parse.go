package muse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ─── The allowlist decoder ───────────────────────────────────────────────────
//
// 🔴 A Muse session.jsonl holds the developer's prompts, the model's replies,
// tool arguments and tool output. This package must never decode any of it. So
// there is no general-purpose line struct: every line is decoded first into
// `envelope` (discriminators only), and only a line whose discriminators name a
// record this collector prices is decoded a SECOND time into the narrow struct
// for that one record kind. Every field reachable from these structs is listed
// in TestDecoderAllowlist, which fails when one is added.
//
// Two-stage decoding is also what keeps a TYPE MISMATCH on an unrelated line
// (a future Muse writing a number where another record kind has a string) from
// being counted as damage: stage 1 reads only fields every record carries.

// envelope is the stage-1 decode: the discriminators every Muse record carries.
type envelope struct {
	// ID is the record's unique uuid — the idempotency anchor.
	ID string `json:"id"`
	// RecordedAt is unix MICROSECONDS (measured: 16 digits on every record).
	RecordedAt int64 `json:"recorded_at"`
	Stream     struct {
		// ID is the session id (stream.kind is always "session" in this file).
		ID string `json:"id"`
	} `json:"stream"`
	Payload struct {
		Kind  string `json:"kind"`
		RunID string `json:"run_id"`
		Event struct {
			Kind string `json:"kind"`
		} `json:"event"`
	} `json:"payload"`
}

// Discriminators, measured on Muse Code 1.4.0.
const (
	kindRun             = "run"              // payload.kind of an agent turn's events
	kindApproval        = "approval"         // payload.kind of a tool-approval review
	kindMetadata        = "metadata"         // payload.kind carrying the workspace root
	kindWorkspaceBranch = "workspace_branch" // payload.kind carrying the run's branch

	eventModelCompleted  = "model_completed"            // one billed agent model call
	eventReviewCompleted = "automated_review_completed" // one billed approval-review call
	eventRunTerminal     = "terminal"                   // the run has ended
	// eventChildLinked is written into a PARENT session when one of its runs
	// spawns a subagent session; it is the only record tying the child to a run.
	eventChildLinked = "memory_reminder_child_session_linked"

	refKindBranch = "branch"
	vcsGit        = "git"
)

// agentLine is the stage-2 decode of a `run` / `model_completed` record.
type agentLine struct {
	Payload struct {
		Event struct {
			// Model is a plain string here ("muse-spark-1.3-contributor"). The
			// review record carries an OBJECT under the same key, which is one
			// reason the two record kinds get separate structs.
			Model string     `json:"model"`
			Usage agentUsage `json:"usage"`
		} `json:"event"`
	} `json:"payload"`
}

// agentUsage is model_completed's usage block. Pointers so ABSENT is not read
// as zero by a check that then passes vacuously.
type agentUsage struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CachedTokens     *int64 `json:"cached_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"`
}

// reviewLine is the stage-2 decode of an `approval` /
// `automated_review_completed` record: Muse asks a model whether a pending tool
// call is safe, and that call is billed like any other.
type reviewLine struct {
	Payload struct {
		Event struct {
			Model struct {
				ModelID string `json:"model_id"`
			} `json:"model"`
			Usage reviewUsage `json:"usage"`
			// CompletedAtMS is the fallback timestamp when recorded_at is absent
			// (measured equal to recorded_at at millisecond precision).
			CompletedAtMS int64 `json:"completed_at_ms"`
		} `json:"event"`
	} `json:"payload"`
}

// reviewUsage is automated_review_completed's usage block — a DIFFERENT shape
// from agentUsage (cached_input/non_cached_input and a total).
type reviewUsage struct {
	InputTokens          *int64 `json:"input_tokens"`
	CachedInputTokens    *int64 `json:"cached_input_tokens"`
	NonCachedInputTokens *int64 `json:"non_cached_input_tokens"`
	OutputTokens         *int64 `json:"output_tokens"`
	ReasoningTokens      *int64 `json:"reasoning_tokens"`
	TotalTokens          *int64 `json:"total_tokens"`
}

// metadataLine is the stage-2 decode of a `metadata` record.
type metadataLine struct {
	Payload struct {
		Record struct {
			// WorkspaceRoot is the absolute directory Muse ran in — the basis for
			// repo scoping, kept exactly as the other readers keep their cwd.
			WorkspaceRoot string `json:"workspace_root"`
		} `json:"record"`
	} `json:"payload"`
}

// branchLine is the stage-2 decode of a `workspace_branch` record, which Muse
// writes once per run, immediately AFTER the run's terminal event.
type branchLine struct {
	Payload struct {
		Record struct {
			// CommandID is the run id this branch observation belongs to.
			CommandID     string `json:"command_id"`
			WorkspaceRoot string `json:"workspace_root"`
			Reference     struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"reference"`
			VCS string `json:"vcs"`
		} `json:"record"`
	} `json:"payload"`
}

// linkLine is the stage-2 decode of a `run` / memory_reminder_child_session_linked
// record: ids only. It names the run that spawned a subagent session, which is
// the run whose branch and settlement that subagent's calls inherit (#901).
type linkLine struct {
	Payload struct {
		Event struct {
			ParentRunID    string `json:"parent_run_id"`
			ChildSessionID string `json:"child_session_id"`
		} `json:"event"`
	} `json:"payload"`
}

// ─── The token mapping ───────────────────────────────────────────────────────

// maxRecordTokens bounds any single token class in one record. Each record is an
// independent per-call fact, so this is opencode's per-message bound, not
// codexrollout's cumulative one: 2^29 keeps every narrowing to `int` safe on a
// 32-bit build (the sum cache_read + cache_write is compared as int64 and
// never narrowed), and it is ~14,000x the largest record measured (~37k tokens).
const maxRecordTokens = 1 << 29

// tokens is one billed call in TIER's three non-overlapping classes. There is
// no cache-write class: see mapAgentUsage.
type tokens struct {
	Input     int // NON-CACHED input: the cached prefix is carved out
	CacheRead int
	Output    int // reasoning is INSIDE this, never added to it
}

func (t tokens) any() bool {
	return t.Input != 0 || t.CacheRead != 0 || t.Output != 0
}

// errAbsentCount refuses a usage block missing input_tokens or output_tokens:
// an absent count is unknown spend, never a zero.
var errAbsentCount = errors.New("usage block lacks input_tokens or output_tokens; an absent count is not a zero")

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// checkRange rejects a negative or implausibly large count before any
// arithmetic, so no subtraction below can go negative and no narrowing wraps.
func checkRange(named map[string]int64) error {
	for name, v := range named {
		if v < 0 {
			return fmt.Errorf("negative %s=%d", name, v)
		}
		if v > maxRecordTokens {
			return fmt.Errorf("%s=%d exceeds the %d-token per-record ceiling", name, v, maxRecordTokens)
		}
	}
	return nil
}

// mapAgentUsage maps a model_completed usage block to TIER's classes, and
// reports whether the record carried cache-write tokens (priced at the input
// rate; see below).
//
// THE MAPPING. Measured on Muse Code 1.4.0 (one real session, 15
// model_completed records) and matched against Meta's token-counting docs by the
// price-table PR (#895):
//
//   - input_tokens INCLUDES the cached prefix: cache_read_tokens <= input_tokens
//     on 15/15 (e.g. input 23142 with cache_read 11505), and the review record
//     states input = cached + non_cached outright. So non-cached input is
//     input - cache_read, and the cached prefix is priced ONCE, at the
//     cache-read rate — never again at the input rate.
//   - cached_tokens == cache_read_tokens on 15/15: a second name for the same
//     count. cache_read_tokens is used; cached_tokens only when it is absent. A
//     record carrying both with different values is refused, not guessed at.
//   - cache_write_tokens is 0 on 15/15, so its placement is UNMEASURED. It is
//     taken to sit INSIDE input_tokens and is priced there, at the input rate —
//     the price table carries no Muse cache-write rate — so it is NOT carved out
//     and NOT added (either would count it twice or at a rate nobody published).
//     The check cache_read + cache_write <= input is what makes that assumption
//     fail loudly, not silently, if a record ever contradicts it.
//   - reasoning_tokens is a SUBSET of output_tokens — the Codex/OpenAI
//     convention, not Opencode's additive one, and Meta's docs say so. Measured:
//     reasoning <= output on 15/15 (output 676, reasoning 507), and the review
//     record has total = input + output with reasoning 266 > 0, which an
//     additive count would break. Output is billed as-is; reasoning > output is
//     refused.
func mapAgentUsage(u agentUsage) (tokens, bool, error) {
	if u.InputTokens == nil || u.OutputTokens == nil {
		return tokens{}, false, errAbsentCount
	}
	in, out := deref(u.InputTokens), deref(u.OutputTokens)
	cr := deref(u.CacheReadTokens)
	if u.CacheReadTokens == nil {
		cr = deref(u.CachedTokens)
	} else if u.CachedTokens != nil && *u.CachedTokens != cr {
		return tokens{}, false, fmt.Errorf("cache_read_tokens=%d != cached_tokens=%d; they are expected to name the same count", cr, *u.CachedTokens)
	}
	cw, reasoning := deref(u.CacheWriteTokens), deref(u.ReasoningTokens)
	if err := checkRange(map[string]int64{
		"input_tokens": in, "output_tokens": out, "cached_tokens": deref(u.CachedTokens),
		"cache_read_tokens": cr, "cache_write_tokens": cw, "reasoning_tokens": reasoning,
	}); err != nil {
		return tokens{}, false, err
	}
	if cr+cw > in {
		return tokens{}, false, fmt.Errorf("cache_read_tokens=%d + cache_write_tokens=%d exceeds input_tokens=%d; input is expected to INCLUDE both", cr, cw, in)
	}
	if reasoning > out {
		return tokens{}, false, fmt.Errorf("reasoning_tokens=%d > output_tokens=%d; reasoning is expected to be a subset of output", reasoning, out)
	}
	return tokens{Input: int(in - cr), CacheRead: int(cr), Output: int(out)}, cw > 0, nil
}

// mapReviewUsage maps an automated_review_completed usage block. Same classes,
// same conventions as mapAgentUsage, with the review shape's own identities
// checked where present (measured on the one real review record:
// input 36633 = cached 4721 + non_cached 31912, total 37061 = input + output).
func mapReviewUsage(u reviewUsage) (tokens, error) {
	if u.InputTokens == nil || u.OutputTokens == nil {
		return tokens{}, errAbsentCount
	}
	in, cached, out := deref(u.InputTokens), deref(u.CachedInputTokens), deref(u.OutputTokens)
	nonCached, reasoning, total := deref(u.NonCachedInputTokens), deref(u.ReasoningTokens), deref(u.TotalTokens)
	if err := checkRange(map[string]int64{
		"input_tokens": in, "cached_input_tokens": cached, "non_cached_input_tokens": nonCached,
		"output_tokens": out, "reasoning_tokens": reasoning, "total_tokens": total,
	}); err != nil {
		return tokens{}, err
	}
	if cached > in {
		return tokens{}, fmt.Errorf("cached_input_tokens=%d > input_tokens=%d", cached, in)
	}
	if u.NonCachedInputTokens != nil && cached+nonCached != in {
		return tokens{}, fmt.Errorf("cached_input_tokens=%d + non_cached_input_tokens=%d != input_tokens=%d", cached, nonCached, in)
	}
	if u.TotalTokens != nil && total != 0 && total != in+out {
		return tokens{}, fmt.Errorf("total_tokens=%d != input_tokens+output_tokens=%d", total, in+out)
	}
	if reasoning > out {
		return tokens{}, fmt.Errorf("reasoning_tokens=%d > output_tokens=%d; reasoning is expected to be a subset of output", reasoning, out)
	}
	return tokens{Input: int(in - cached), CacheRead: int(cached), Output: int(out)}, nil
}

// ─── The session parse ───────────────────────────────────────────────────────

// Call kinds, for diagnostics and tests.
const (
	callAgent  = "agent"
	callReview = "review"
)

// call is one billed model call, with everything the collector needs to emit it
// and nothing else.
type call struct {
	RecordID string
	RunID    string
	Kind     string
	Model    string
	Tokens   tokens
	// Timestamp is recorded_at (µs), falling back to completed_at_ms on a review
	// record. Never zero: a record with neither is refused.
	Timestamp time.Time
	// Ordinal is the record's 0-based position among decoded lines — used only to
	// ask "was anything written after this run's terminal event?".
	Ordinal int
}

// session is one parsed session.jsonl.
type session struct {
	// SessionID is stream.id, first seen.
	SessionID string
	// WorkspaceRoot is the first metadata record's workspace_root, else the
	// first workspace_branch record's. "" when the file named neither.
	WorkspaceRoot string
	Calls         []call
	// branches maps a run id to the branch Muse recorded for it ("" when that
	// run's workspace was on a detached HEAD or not in git).
	branches map[string]string
	// childRuns maps a subagent session id to the run of THIS session that
	// spawned it, from memory_reminder_child_session_linked records. "" marks a
	// child named by two records with different runs: ambiguous, so unlinked.
	childRuns map[string]string
	// terminalAt maps a run id to the ordinal of its terminal event.
	terminalAt map[string]int
	// lastOrdinal is the ordinal of the last decoded record in the file.
	lastOrdinal int

	// SkippedLines counts COMPLETE lines that were not JSON (or whose
	// discriminators had the wrong type). An unterminated final line is a writer
	// mid-flush and is not counted.
	SkippedLines int
	// Refused counts billed records that were recognized but NOT priced, keyed
	// by reason. Every one is spend that is missing, so the collector reports
	// them at WARN.
	Refused map[string]int
	// firstRefusal is one example error, for the log line.
	firstRefusal string
	// ZeroTokenCalls counts billed-kind records carrying no tokens at all (an
	// aborted call). Not spend, so not a refusal — counted so it is visible.
	ZeroTokenCalls int
	// CacheWriteCalls counts calls that reported cache-write tokens, which are
	// priced inside input at the input rate (see mapAgentUsage). Never observed
	// yet, so it is counted to make the first occurrence visible.
	CacheWriteCalls int
}

// Refusal reasons.
const (
	refuseUndecodable = "undecodable_usage_record"
	refuseNoRecordID  = "no_record_id"
	refuseNoModel     = "no_model"
	refuseBadUsage    = "usage_identity_violation"
	refuseDuplicateID = "duplicate_record_id"
	refuseNoTimestamp = "no_timestamp"
)

func (s *session) refuse(reason string, err error) {
	if s.Refused == nil {
		s.Refused = make(map[string]int)
	}
	s.Refused[reason]++
	if s.firstRefusal == "" && err != nil {
		s.firstRefusal = reason + ": " + err.Error()
	}
}

// branchFor returns the branch Muse recorded for a run and whether it recorded
// one at all.
func (s *session) branchFor(runID string) (string, bool) {
	b, ok := s.branches[runID]
	return b, ok
}

// runForChild returns the run of this session that spawned the subagent
// session childID, and whether exactly one run is linked to it.
func (s *session) runForChild(childID string) (string, bool) {
	run := s.childRuns[childID]
	return run, run != ""
}

// linkChild records that run spawned the subagent session child. A second
// record naming the same child with a DIFFERENT run makes the link ambiguous:
// picking either would file the child's spend under a branch nothing proves.
func (s *session) linkChild(child, run string) {
	if child == "" || run == "" {
		return
	}
	if prev, seen := s.childRuns[child]; seen && prev != run {
		s.childRuns[child] = ""
		return
	}
	s.childRuns[child] = run
}

// runEnded reports whether the run's terminal event has been seen AND some
// record was written after it. Muse writes a run's workspace_branch immediately
// after its terminal event, so a terminal with a later record but no branch
// means Muse is not going to record one for that run.
func (s *session) runEnded(runID string) bool {
	at, ok := s.terminalAt[runID]
	return ok && s.lastOrdinal > at
}

// maxSessionFile bounds how much of one session.jsonl is read. The largest
// measured file is ~0.7 MB; exceeding the cap FAILS the file rather than
// pricing a truncated prefix. A var so the boundary test can shrink it.
var maxSessionFile int64 = 64 << 20

// maxSessionLine caps one line. Muse keeps large tool output in a sibling
// tool-outputs directory, so lines are small (~1 KB average measured). A line
// past the cap fails the file, for the same reason as the file cap.
const maxSessionLine = 10 << 20

// parseSession reads one session.jsonl. A non-nil error means the FILE could
// not be read honestly (I/O, a line or file over its cap) and nothing from it
// may be emitted; per-line and per-record problems are counted on the session
// instead, because Muse records are independent facts and one bad record costs
// exactly itself.
func parseSession(path string) (*session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	lr := &io.LimitedReader{R: f, N: maxSessionFile + 1}
	tail := &lastByteReader{r: lr}
	scanner := bufio.NewScanner(tail)
	scanner.Buffer(nil, maxSessionLine)

	s := &session{branches: map[string]string{}, terminalAt: map[string]int{}, childRuns: map[string]string{}}
	seen := map[string]bool{}
	ordinal := -1
	lastLineMalformed := false
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			lastLineMalformed = false
			continue
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			s.SkippedLines++
			lastLineMalformed = true
			continue
		}
		lastLineMalformed = false
		// Only a session RECORD (it carries an id) moves the ordinal, so an
		// out-of-band line after a terminal event cannot pass for "Muse wrote
		// past this run". The one such line measured is a retained_frame
		// (1 per file, at line 1, on 3/3 real files, Muse Code 1.4.0) whose
		// children are permission records, not billed calls — it matches no
		// case below and is skipped whole.
		if env.ID != "" {
			ordinal++
			s.lastOrdinal = ordinal
		}
		if s.SessionID == "" {
			s.SessionID = env.Stream.ID
		}
		switch env.Payload.Kind {
		case kindMetadata:
			var m metadataLine
			if json.Unmarshal(raw, &m) == nil && s.WorkspaceRoot == "" {
				s.WorkspaceRoot = m.Payload.Record.WorkspaceRoot
			}
		case kindWorkspaceBranch:
			var b branchLine
			if json.Unmarshal(raw, &b) != nil {
				continue
			}
			rec := b.Payload.Record
			if s.WorkspaceRoot == "" {
				s.WorkspaceRoot = rec.WorkspaceRoot
			}
			if rec.CommandID == "" {
				continue
			}
			// A detached HEAD, a non-branch ref or a non-git workspace records
			// the run as branchless: "" resolves to the labelled
			// unattributed bucket, never to a guessed branch.
			name := ""
			if rec.VCS == vcsGit && rec.Reference.Kind == refKindBranch {
				name = rec.Reference.Name
			}
			s.branches[rec.CommandID] = name
		case kindRun:
			switch env.Payload.Event.Kind {
			case eventRunTerminal:
				if env.Payload.RunID != "" {
					s.terminalAt[env.Payload.RunID] = ordinal
				}
			case eventChildLinked:
				var k linkLine
				if json.Unmarshal(raw, &k) != nil {
					continue
				}
				run := k.Payload.Event.ParentRunID
				if run == "" {
					run = env.Payload.RunID
				}
				s.linkChild(k.Payload.Event.ChildSessionID, run)
			case eventModelCompleted:
				var a agentLine
				if err := json.Unmarshal(raw, &a); err != nil {
					s.refuse(refuseUndecodable, err)
					continue
				}
				t, wrote, err := mapAgentUsage(a.Payload.Event.Usage)
				if s.addCall(env, seen, ordinal, callAgent, a.Payload.Event.Model, t, err, 0) && wrote {
					s.CacheWriteCalls++
				}
			}
		case kindApproval:
			if env.Payload.Event.Kind != eventReviewCompleted {
				continue
			}
			var r reviewLine
			if err := json.Unmarshal(raw, &r); err != nil {
				s.refuse(refuseUndecodable, err)
				continue
			}
			t, err := mapReviewUsage(r.Payload.Event.Usage)
			s.addCall(env, seen, ordinal, callReview, r.Payload.Event.Model.ModelID, t, err, r.Payload.Event.CompletedAtMS)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan terminated early: %w", err)
	}
	if lr.N == 0 {
		return nil, fmt.Errorf("session file exceeds the %d-byte read cap; refusing to report a truncated prefix of its spend", maxSessionFile)
	}
	if lastLineMalformed && !tail.endedWithNewline() {
		s.SkippedLines--
	}
	return s, nil
}

// addCall validates one recognized billed record and appends it, or counts why
// it was refused, and reports whether it was appended. completedAtMS is the
// review record's fallback clock.
func (s *session) addCall(env envelope, seen map[string]bool, ordinal int, kind, model string, t tokens, mapErr error, completedAtMS int64) bool {
	switch {
	case env.ID == "":
		s.refuse(refuseNoRecordID, fmt.Errorf("a %s record has no id, so it cannot be keyed for dedup", kind))
		return false
	case seen[env.ID]:
		s.refuse(refuseDuplicateID, nil)
		return false
	case mapErr != nil:
		s.refuse(refuseBadUsage, mapErr)
		return false
	case !t.any():
		s.ZeroTokenCalls++
		return false
	case model == "":
		s.refuse(refuseNoModel, fmt.Errorf("a %s record names no model; refusing to price it at a guessed fallback rate", kind))
		return false
	}
	var ts time.Time
	switch {
	case env.RecordedAt > 0:
		ts = time.UnixMicro(env.RecordedAt).UTC()
	case completedAtMS > 0:
		ts = time.UnixMilli(completedAtMS).UTC()
	default:
		// A zero time stores as year 1 and drops out of every window: present in
		// the table, invisible in every figure. Refuse it by name instead.
		s.refuse(refuseNoTimestamp, fmt.Errorf("a %s record carries neither recorded_at nor completed_at_ms", kind))
		return false
	}
	seen[env.ID] = true
	s.Calls = append(s.Calls, call{
		RecordID:  env.ID,
		RunID:     env.Payload.RunID,
		Kind:      kind,
		Model:     model,
		Tokens:    t,
		Timestamp: ts,
		Ordinal:   ordinal,
	})
	return true
}

// lastByteReader remembers the final byte read, so an unterminated final line
// (Muse mid-write) is told apart from a damaged complete line. Same device as
// codexrollout's.
type lastByteReader struct {
	r    io.Reader
	last byte
	read bool
}

func (l *lastByteReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.last = p[n-1]
		l.read = true
	}
	return n, err
}

func (l *lastByteReader) endedWithNewline() bool { return l.read && l.last == '\n' }
