package codexrollout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
)

// rolloutSession is one parsed rollout log: the session identity, the working
// directory and branch it ran in, and its per-call token usage.
type rolloutSession struct {
	// SessionID is session_meta.payload.id — an opaque UUID, no prompt content.
	SessionID string
	// CWD is session_meta.payload.cwd, the basis for repo attribution.
	CWD string
	// Branch is session_meta.payload.git.branch when Codex recorded git info,
	// else "". Empty routes attribution to a labeled unattributed bucket rather
	// than dropping the spend.
	Branch string
	// StartTime is the first timestamp seen in the file; the fallback for a
	// token_count event that carried none.
	StartTime time.Time
	// Calls holds one entry per BILLABLE token_count event, in file order.
	// Re-emitted (zero-delta) events are absent — see parseRollout.
	Calls []rolloutCall
	// SkippedLines counts COMPLETE lines that failed to decode and were dropped
	// (#526). It excludes an unterminated final line, which is a writer mid-flush
	// rather than damage.
	//
	// It exists because the skip was previously reported ONLY through a
	// logger.Warn and never reached the return value, which made lost spend
	// unquantified for every caller. The decisive case: when damage removes every
	// billable token_count line, parseRollout used to return (nil, nil) — and a
	// fully-corrupt log became indistinguishable from a genuinely idle session.
	// Whatever a caller decides to do about it, it must first be able to SEE it.
	SkippedLines int
}

// rolloutCall is one API call's usage, derived by differencing.
type rolloutCall struct {
	// Ordinal is this event's 0-based position among ALL token_count events in
	// the file, including the skipped zero-delta duplicates. It anchors the
	// idempotency key.
	//
	// It is deliberately a property of the FILE, not an index into Calls. Both
	// are stable across a re-scan of an append-only log TODAY, so this is not a
	// correctness claim about the current code — it is a coupling choice. An
	// index into Calls would tie every already-ingested row's key to our
	// EMISSION POLICY: the day parse-time filtering changes (skip events before
	// the scan cursor, stop dropping zero-delta duplicates, split a call), every
	// surviving event renumbers and re-ingests as a NEW row, doubling recorded
	// spend with no error anywhere. The scan cursor added for #464-R3 makes
	// parse-time windowing a live temptation, so the decoupling earns its keep.
	// Pinned by TestOrdinalIsFilePositionNotCallIndex, which fails on the index
	// form.
	Ordinal   int
	Timestamp time.Time
	Model     string
	// Input is the FRESH (non-cached) input for this call: cached tokens are
	// carved out so the two classes never overlap, exactly as the proxy's
	// parseOpenAI does.
	Input     int
	CacheRead int
	// Output is the full output count. reasoning_output_tokens is a SUBSET of
	// it and is deliberately absent from this struct: adding it would bill the
	// same tokens twice.
	Output int
}

// rolloutLine is the union of every field this collector reads from a rollout
// log line. Codex writes several line types (session_meta, turn_context,
// event_msg, response_item, world_state); one flattened payload struct decodes
// them all in a single pass, and encoding/json skips the fields that do not
// apply to a given type — notably session_meta.base_instructions, which is the
// bulk of the file's bytes and is never unmarshalled into anything.
type rolloutLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Payload   *struct {
		// session_meta fields.
		ID  string `json:"id"`
		CWD string `json:"cwd"`
		Git *struct {
			Branch string `json:"branch"`
		} `json:"git"`
		// turn_context field. Codex can change models mid-session, so the model
		// is latched per turn and applied to the calls that follow.
		Model string `json:"model"`
		// event_msg fields. PayloadType is the inner discriminator
		// ("token_count", "agent_message", ...) — distinct from the outer
		// line Type.
		PayloadType string `json:"type"`
		Info        *struct {
			TotalTokenUsage *tokenUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// tokenUsage is a Codex usage block. The collector reads only the CUMULATIVE
// one (total_token_usage); last_token_usage is deliberately not parsed, so
// there is no field here that a future edit could accidentally sum. See
// deltaFrom.
type tokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// Line type / payload type discriminators.
const (
	lineSessionMeta = "session_meta"
	lineTurnContext = "turn_context"
	lineEventMsg    = "event_msg"
	payloadTokenLog = "token_count"

	// maxCumulativeTokens is the absolute sanity ceiling on any single cumulative
	// count in one session (see checkContainment). 1e12 is ~10^6 times the largest
	// session observed locally (the biggest of the 31 captured rollouts is well
	// under 10^6 tokens) and still orders of magnitude below the int64 cost
	// overflow it exists to prevent, so it can only ever reject a corrupt file.
	maxCumulativeTokens = 1_000_000_000_000
)

// parseRollout reads one rollout log and returns its session identity plus one
// rolloutCall per billable API call.
//
// Returns a session with ZERO Calls when the file contains no billable
// token_count events — an aborted or purely-conversational session is not an
// error, it just has no spend. (It is deliberately NOT nil; see RETURN CONTRACT
// below, which this summary line used to contradict.) Returns a non-nil error
// when a containment invariant is violated: the
// caller must then drop the WHOLE file, because a log whose own numbers
// contradict each other cannot be priced honestly.
//
// Malformed lines are skipped with a per-file warning, not an error. Cumulative
// differencing makes this safe in a way that summing never could be: a dropped
// snapshot simply merges its usage into the NEXT snapshot's delta, so the
// session TOKEN total stays exact — only the per-call split gets coarser.
//
// That exactness is a TOKEN property, not a COST one. If the session switched
// models between a dropped snapshot and the next, the merged delta is priced
// entirely at the LATER model's rate, so the earlier model's tokens are billed
// wrong (in either direction) even though the token total is right. Same-model
// sessions — the overwhelming majority — are exact in both.
//
// An UNTERMINATED final line is the expected shape of a live Codex session
// caught mid-flush, not damage: it is skipped SILENTLY (no warning), exactly as
// the JSONL collector's tailMode trims at the last newline. Only a COMPLETE line
// that fails to decode counts as malformed and raises the warning — otherwise
// the 5-minute cadence would warn about every running session forever and train
// operators to ignore the one signal that means "this log is damaged".
//
// RETURN CONTRACT (#526): a nil session means a FATAL parse error and is always
// paired with a non-nil error. A successful parse ALWAYS returns a non-nil
// session, including one with zero Calls — that is how a caller distinguishes a
// genuinely idle session (SkippedLines == 0) from a log damaged badly enough to
// lose every billable line (SkippedLines > 0). Those two states are identical
// from the outside and mean opposite things: one session cost nothing, the other
// cost something no longer measurable. Do not "simplify" the zero-call case back
// to (nil, nil).
//
// ⚠️ On a ZERO-Calls session only SkippedLines is reliable. SessionID, CWD,
// Branch and StartTime may all be zero, because damage can consume session_meta
// itself — and the checks that REFUSE those (missing session id, unnamed model,
// zero timestamp) all run BELOW the zero-call return, so none of them guards this
// object. Do not read those fields without checking len(Calls) > 0 first.
func parseRollout(path string, logger *slog.Logger) (*rolloutSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	// STREAM the file; never buffer it whole. bufio.Scanner already reads
	// incrementally, so an io.ReadAll bought nothing but a 64 MB worst-case
	// allocation (~2x peak during ReadAll's growth doubling).
	//
	// The +1 byte is what makes the cap DETECTABLE. io.LimitedReader reports
	// io.EOF when it hits its limit, so reading exactly maxRolloutFile bytes
	// leaves err == nil and the parse SUCCEEDS on a truncated prefix — the
	// session's spend under-reported with nothing to say so. Reading one byte
	// past the cap turns "hit the limit" into an observable state (lr.N == 0),
	// checked after the scan and fatal, matching how an over-long LINE is fatal.
	lr := &io.LimitedReader{R: f, N: maxRolloutFile + 1}
	tail := &lastByteReader{r: lr}

	var (
		s        rolloutSession
		prev     tokenUsage // the previous cumulative snapshot; zero before the first
		curModel string     // latched from the most recent turn_context
		ordinal  int        // position among ALL token_count events
		skipped  int
		cwWarned bool
		// lastLineMalformed tracks whether the MOST RECENT line failed to decode,
		// so an unterminated final line (writer mid-flush) can be discounted from
		// `skipped` once the scan ends. Reset by every line that decodes.
		lastLineMalformed bool
	)

	scanner := bufio.NewScanner(tail)
	scanner.Buffer(nil, maxRolloutLine)

	for scanner.Scan() {
		// TrimSpace handles CRLF (Scanner strips '\n' but leaves '\r', which
		// json.Unmarshal rejects) and stray blank lines.
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			// Reset the flag too (#526). A blank or whitespace-only line means an
			// EARLIER malformed line was not the file's last, so it is real damage
			// and must not be discounted by the mid-flush check after the loop.
			// The realistic trigger is the CRLF case this comment already
			// contemplates: a writer caught between '\r' and '\n' leaves a final
			// line of exactly "\r", which TrimSpace empties — silently cancelling
			// a genuinely damaged line and under-reporting the very number this
			// field exists to produce.
			lastLineMalformed = false
			continue
		}
		var line rolloutLine
		if err := json.Unmarshal(raw, &line); err != nil {
			// One malformed line does not poison the file. Whether this one is
			// real damage or just the tail of a line Codex is still writing is
			// decided after the loop, from whether the file ended in a newline.
			skipped++
			lastLineMalformed = true
			continue
		}
		lastLineMalformed = false
		if s.StartTime.IsZero() && !line.Timestamp.IsZero() {
			s.StartTime = line.Timestamp
		}
		if line.Payload == nil {
			continue
		}

		switch line.Type {
		case lineSessionMeta:
			// First-seen wins: a rollout log describes exactly one session, and
			// latching guards against a concatenated or replayed file silently
			// re-pointing the identity mid-stream.
			if s.SessionID == "" {
				s.SessionID = line.Payload.ID
			}
			if s.CWD == "" {
				s.CWD = line.Payload.CWD
			}
			if s.Branch == "" && line.Payload.Git != nil {
				s.Branch = line.Payload.Git.Branch
			}
		case lineTurnContext:
			if line.Payload.Model != "" {
				curModel = line.Payload.Model
			}
		case lineEventMsg:
			if line.Payload.PayloadType != payloadTokenLog ||
				line.Payload.Info == nil || line.Payload.Info.TotalTokenUsage == nil {
				continue
			}
			cur := *line.Payload.Info.TotalTokenUsage
			thisOrdinal := ordinal
			ordinal++

			if err := checkContainment(cur); err != nil {
				return nil, fmt.Errorf("token_count event %d: %w", thisOrdinal, err)
			}
			if cur.CacheWriteInputTokens != 0 && !cwWarned {
				// OpenAI publishes no cache-write SKU, so there is no rate to
				// price these at. The observed shape is that they are already
				// inside input_tokens — total == input + output is CONSISTENT
				// with that (it does not prove it; a separately-carried write
				// count could also satisfy the sum). On that reading they are
				// billed at the input rate and dropping the field loses nothing.
				// Warn once so a future Codex/OpenAI change that starts charging
				// separately is not absorbed silently.
				logger.Warn("codex-rollout: cache_write_input_tokens is nonzero; billed at the input rate (OpenAI has no cache-write SKU)",
					"path", logsafe.Str(path), "cache_write_input_tokens", cur.CacheWriteInputTokens)
				cwWarned = true
			}

			delta, err := deltaFrom(prev, cur)
			if err != nil {
				return nil, fmt.Errorf("token_count event %d: %w", thisOrdinal, err)
			}
			prev = cur

			// A zero delta is a RE-EMITTED token_count event carrying a
			// cumulative snapshot we have already billed. Differencing turns it
			// into nothing, which is the whole point — but there is no reason to
			// store a zero-token row for it. The ordinal is still consumed above
			// so surviving events keep their positional idempotency keys.
			if delta.Input == 0 && delta.CacheRead == 0 && delta.Output == 0 {
				continue
			}
			delta.Ordinal = thisOrdinal
			delta.Timestamp = line.Timestamp
			delta.Model = curModel
			s.Calls = append(s.Calls, delta)
		}
	}
	if err := scanner.Err(); err != nil {
		// Most likely bufio.ErrTooLong (a line beyond maxRolloutLine). Returning
		// a partial parse would under-report this session's spend WITHOUT saying
		// so, which is exactly the silently-wrong figure this collector refuses
		// to produce. Fail the file.
		return nil, fmt.Errorf("scan terminated early: %w", err)
	}
	// The file outgrew the read cap: everything past it was NOT seen, so any
	// total derived from what we did see is an under-report. Same posture as an
	// over-long line — fail the file rather than publish a confident partial.
	if lr.N == 0 {
		return nil, fmt.Errorf("rollout log exceeds the %d-byte read cap; refusing to report a truncated prefix of its spend", maxRolloutFile)
	}
	// Discount an unterminated final line: that is a writer mid-flush, and the
	// next scan will see the completed line (the ordinal is not consumed, so its
	// idempotency key is unchanged).
	if lastLineMalformed && !tail.endedWithNewline() {
		skipped--
	}
	if skipped > 0 {
		logger.Warn("codex-rollout: skipped malformed rollout lines",
			"path", logsafe.Str(path), "count", skipped)
	}
	// Carry the count OUT, not just to the log (#526). A warning is not a return
	// value: it reaches an operator watching stderr at the right moment and
	// nothing else. Every consumer that wants to say "some spend is missing here"
	// needs the number.
	s.SkippedLines = skipped

	if len(s.Calls) == 0 {
		// Return the SESSION, not nil. This used to be `return nil, nil`, which
		// threw the skip count away at exactly the moment it mattered most: a log
		// damaged badly enough to lose every billable line reported identically to
		// a session that genuinely never spent anything. The caller still treats
		// zero calls as "nothing to emit" — it just gets to see WHY there is
		// nothing, which it previously could not.
		//
		// Deliberately still (session, nil) and NOT an error: the live-append
		// tolerance is correct. Rollout logs are appended while Codex runs, so
		// failing a file on malformed lines would break `serve`. Loud stays loud
		// (IO failure, over-long line, read cap, containment violation, missing
		// session id, unnamed model, both deltaFrom cross-snapshot checks); this
		// is about making the SILENT case countable, not about making it fatal.
		return &s, nil
	}
	if s.SessionID == "" {
		// Without a session id the idempotency key degenerates to the ordinal
		// alone, so two different Codex sessions would collide and one would be
		// silently discarded by the store's unique index. Refuse.
		return nil, fmt.Errorf("no session_meta id found, but %d billable token_count events present — cannot build a collision-free idempotency key", len(s.Calls))
	}
	// Model is required to price. Backfill from the session's first known model
	// for any call that preceded the first turn_context, then refuse if the file
	// never named a model at all: pricing an unnamed model falls through to the
	// self-hosted size-class GUESS, which is precisely the silently-wrong cost
	// figure this collector exists to avoid.
	if err := backfillModels(&s); err != nil {
		return nil, err
	}
	// A call with no timestamp falls back to the session's first timestamp
	// (buildEvents). When the FILE carried no timestamp anywhere, that fallback
	// is the zero time, which stores as year 1 — outside every dashboard window,
	// so the spend is present in the table and invisible in every figure. That is
	// the silent-disappearance failure mode, so refuse the file instead.
	if s.StartTime.IsZero() {
		for _, c := range s.Calls {
			if c.Timestamp.IsZero() {
				return nil, fmt.Errorf("token_count event %d has no timestamp and the file carries none to fall back on — a zero timestamp would store as year 1 and vanish from every window", c.Ordinal)
			}
		}
	}
	return &s, nil
}

// lastByteReader remembers the final byte handed to the reader above it, which
// is how parseRollout tells a writer mid-flush from real damage: a rollout log
// that does NOT end in '\n' has a final line Codex has not finished writing.
//
// It exists because bufio.Scanner strips the terminator, so by the time a line
// reaches the loop the distinction is gone; and because the alternative — an
// extra Stat+ReadAt of the file's last byte — is a second syscall per file per
// scan for information the read already flows past.
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

// endedWithNewline reports whether the stream's final byte was '\n'. An empty
// file reports false; it has no final line to be mid-flush either way.
func (l *lastByteReader) endedWithNewline() bool { return l.read && l.last == '\n' }

// checkContainment asserts the invariants Codex's own numbers must satisfy,
// matching score.py from the tiermetric/model-bench repository — the working
// reference implementation that priced the Codex half of the TIER Model Report.
// (That working copy is .gitignore'd here, so the citation is to the repo, not
// to a path that survives a fresh clone.)
//
// These are FATAL, not warnings, by explicit design (#464). Every one of them
// failing means the log is self-contradictory, and any cost we computed from it
// would be a confident wrong number. No figure beats a wrong figure.
//
// FOUR RELATIONAL INVARIANTS PLUS ONE ABSOLUTE ONE. The relational checks below
// only assert the fields agree with EACH OTHER, so an internally-consistent but
// absurd snapshot (~4e18 input and output) satisfies every one of them; the
// ceiling is what bounds magnitude. Without it such a row prices to a clamped
// MaxInt64 cost_micro (~$9.2 trillion), and two of those overflow SQLite's
// integer SUM() — which ERRORS rather than saturating, and SUM(cost_micro) backs
// /api/v1/scores, so one corrupt local file would take down the read plane.
//
// CAVEAT worth knowing before trusting a session: total == input + output is the
// strongest of these (it is the only one that cross-checks the two classes we
// actually bill), and it is SKIPPED when total_tokens is absent or zero. A Codex
// build that stopped emitting total_tokens would therefore keep passing while
// losing the check that matters most — visible only as this comment, since the
// degradation is deliberate (see below).
func checkContainment(u tokenUsage) error {
	// Absolute sanity ceiling, checked FIRST because it is the only bound on
	// magnitude and the relational checks below can all pass at any scale.
	// 1e12 cumulative tokens is ~10^6x the largest observed session and over $1M
	// at current Codex rates: nothing legitimate approaches it, and anything that
	// does is a corrupt file, not a bill.
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 ||
		u.ReasoningOutputTokens < 0 || u.TotalTokens < 0 {
		return fmt.Errorf("implausible usage: a negative token count (input=%d cached=%d output=%d reasoning=%d total=%d)",
			u.InputTokens, u.CachedInputTokens, u.OutputTokens, u.ReasoningOutputTokens, u.TotalTokens)
	}
	if u.InputTokens > maxCumulativeTokens || u.OutputTokens > maxCumulativeTokens ||
		u.CachedInputTokens > maxCumulativeTokens || u.TotalTokens > maxCumulativeTokens {
		return fmt.Errorf("implausible usage: a cumulative count exceeds the %d-token sanity ceiling (input=%d cached=%d output=%d total=%d)",
			maxCumulativeTokens, u.InputTokens, u.CachedInputTokens, u.OutputTokens, u.TotalTokens)
	}
	// total == input + output. Guarded on total != 0 (as score.py is) so a
	// future Codex build that omits total_tokens degrades to "unverified" rather
	// than failing every session. cached and reasoning are SUBSETS of input and
	// output respectively, which is exactly why they are absent from this sum.
	if u.TotalTokens != 0 && u.TotalTokens != u.InputTokens+u.OutputTokens {
		return fmt.Errorf("containment broken: total_tokens=%d != input_tokens+output_tokens=%d",
			u.TotalTokens, u.InputTokens+u.OutputTokens)
	}
	// cached ⊆ input. If this ever failed, carving cached out of input would
	// produce a NEGATIVE fresh-input count.
	if u.CachedInputTokens > u.InputTokens {
		return fmt.Errorf("containment broken: cached_input_tokens=%d > input_tokens=%d",
			u.CachedInputTokens, u.InputTokens)
	}
	// reasoning ⊆ output. We never add reasoning on top of output; this asserts
	// the assumption that makes that correct.
	if u.ReasoningOutputTokens > u.OutputTokens {
		return fmt.Errorf("containment broken: reasoning_output_tokens=%d > output_tokens=%d",
			u.ReasoningOutputTokens, u.OutputTokens)
	}
	return nil
}

// deltaFrom derives ONE call's usage by differencing two consecutive cumulative
// snapshots.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY DIFFERENCE THE CUMULATIVE SERIES INSTEAD OF SUMMING `last_token_usage`
// ─────────────────────────────────────────────────────────────────────────────
// Every Codex `token_count` event carries both a cumulative `total_token_usage`
// and a per-call `last_token_usage`. Summing the per-call field is the obvious
// implementation and it is WRONG: Codex sometimes RE-EMITS a token_count event,
// and the re-emitted per-call values are counted a second time.
//
// Measured, not theorised. In testdata/rollout-duplicate-token-count.jsonl (a
// real captured session) the cumulative series is:
//
//	[17129, 37728, 58507, 58507, 80154, 101922, 124386]
//	                      ^^^^^^^^^^^^^^ 58507 repeats — that is the duplicate
//
// Summing `last_token_usage` over that file gives 145,165 tokens. The true
// session total is 124,386. A 20,779-token, 17% OVERCOUNT — straight into the
// cost figure, in the direction that flatters nobody.
//
// The cumulative series is monotonic non-decreasing (verified across all 18
// captured sessions in model-bench/codex). Differencing consecutive snapshots
// therefore buys three things in one move:
//
//   - CORRECTNESS — the deltas telescope back to the final cumulative total by
//     construction, so the session total can never drift from what Codex says.
//   - PER-CALL GRANULARITY — the same granularity a naive sum was reaching for,
//     without its double-count.
//   - AUTOMATIC DEDUP — a re-emitted snapshot differences to exactly zero and
//     drops out. No dedup table, no "largest total wins" heuristic.
//
// This is the same defect class as the Claude Code streaming-chunk duplication
// documented in internal/collector/jsonl.go (issue #6/#19), INVERTED: there the
// upstream emits partial snapshots that must be deduped by keeping the LARGEST;
// here it emits a repeated cumulative snapshot that differencing cancels. In
// both cases the rule is the same — trust the cumulative statement of record,
// never the per-chunk deltas the wire happens to hand you.
//
// DO NOT "optimize" this back into a sum of last_token_usage.
//
// ─────────────────────────────────────────────────────────────────────────────
//
// Returns an error when the series is not monotonic. A decreasing cumulative
// snapshot means either a log rewind, a concatenation of two sessions into one
// file, or a Codex change that breaks the model this collector is built on —
// all of which make the deltas meaningless, and one of which (a negative delta)
// would push a NEGATIVE cost into the store and corrupt every SUM downstream.
// Fatal, per #464.
func deltaFrom(prev, cur tokenUsage) (rolloutCall, error) {
	if cur.InputTokens < prev.InputTokens ||
		cur.OutputTokens < prev.OutputTokens ||
		cur.CachedInputTokens < prev.CachedInputTokens ||
		cur.TotalTokens < prev.TotalTokens {
		return rolloutCall{}, fmt.Errorf(
			"cumulative token series is not monotonic: previous (input=%d cached=%d output=%d total=%d) -> current (input=%d cached=%d output=%d total=%d)",
			prev.InputTokens, prev.CachedInputTokens, prev.OutputTokens, prev.TotalTokens,
			cur.InputTokens, cur.CachedInputTokens, cur.OutputTokens, cur.TotalTokens)
	}
	dInput := cur.InputTokens - prev.InputTokens
	dCached := cur.CachedInputTokens - prev.CachedInputTokens
	dOutput := cur.OutputTokens - prev.OutputTokens
	// Per-snapshot containment (cached ⊆ input) does NOT imply the same for the
	// deltas, so assert it here too. Without this a call whose cached growth
	// outran its input growth would yield a negative fresh-input count and a
	// negative cost. Verified to hold across all 18 captured sessions.
	if dCached > dInput {
		return rolloutCall{}, fmt.Errorf(
			"containment broken across consecutive snapshots: cached delta=%d exceeds input delta=%d",
			dCached, dInput)
	}
	return rolloutCall{
		// Carve cached out of input so the two token classes never overlap —
		// identical to the proxy's parseOpenAI. Codex's input_tokens is
		// INCLUSIVE of cached_input_tokens (Responses API semantics), and the
		// price table applies a separate cache-read multiplier, so leaving them
		// overlapped would bill the cached prefix at both rates.
		Input:     dInput - dCached,
		CacheRead: dCached,
		Output:    dOutput,
	}, nil
}

// backfillModels fills any call that has no model with the session's first known
// model (a call recorded before the first turn_context line), and reports an
// error when the file never named a model at all.
//
// Refusing an unnamed model is deliberate. store.ComputeCost would happily price
// "" via its self-hosted size-class fallback, emitting a WARN and a confident
// dollar figure derived from a guess. For a collector whose entire justification
// is that TIER could not previously produce an honest Codex number, shipping a
// guess dressed as a measurement is the one outcome worse than shipping nothing.
func backfillModels(s *rolloutSession) error {
	fallback := ""
	for _, c := range s.Calls {
		if c.Model != "" {
			fallback = c.Model
			break
		}
	}
	if fallback == "" {
		return fmt.Errorf("no turn_context model found in %d billable token_count events — refusing to price an unnamed model at the self-hosted fallback rate", len(s.Calls))
	}
	for i := range s.Calls {
		if s.Calls[i].Model == "" {
			s.Calls[i].Model = fallback
		}
	}
	return nil
}
