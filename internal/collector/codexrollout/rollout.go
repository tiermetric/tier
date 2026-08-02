// Package codexrollout captures Codex CLI spend from the rollout logs Codex
// writes locally to ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl (#464).
//
// It is the Codex analogue of the Claude Code JSONL collector: local session
// files, cwd-based repo attribution, per-call token events, no proxy in front.
// It is also the ONLY path that captures Codex at all — Codex speaks the OpenAI
// *Responses* API (input_tokens / output_tokens), while the reverse proxy's
// OpenAI parser reads the *Chat Completions* usage shape (prompt_tokens /
// completion_tokens), so a Codex response through the proxy yields no token
// event whatsoever (#463). Until this collector, TIER could not measure Codex.
//
// # The load-bearing correctness rule: difference, never sum
//
// A rollout log's `token_count` event carries BOTH a cumulative
// `total_token_usage` (session-to-date) and a per-call `last_token_usage`.
// Summing `last_token_usage` is WRONG: Codex sometimes RE-EMITS a token_count
// event, and the per-call values then double-count. This is not theoretical —
// see the regression fixture and TestDuplicateTokenCountEventIsNotDoubleCounted.
//
// This package derives each call's usage by DIFFERENCING consecutive cumulative
// snapshots instead. See deltaFrom for the full argument.
//
// # Fail loud, never estimate
//
// The containment invariants Codex's own numbers must satisfy are checked on
// every snapshot and are FATAL: a violated invariant fails the whole session
// file, which then contributes ZERO events FROM THAT SCAN. A silently-wrong cost
// figure is worse than no figure, and a warn-and-continue posture here would
// publish one. Sibling session files are unaffected — one corrupt log must not
// blind the scan.
//
// ACROSS scans the guarantee is weaker, and it is worth stating plainly because
// the store is what an operator actually reads. A session is scanned repeatedly
// as it grows: if it was healthy at scan N, its events for that prefix are
// already ingested and CANNOT be withdrawn when an append at scan N+1 breaks an
// invariant. What the store then holds is a real, correctly-priced PARTIAL
// figure for that session — every ingested row is honest, but the session total
// is an under-report, and nothing in the row marks it as truncated. The scan
// error names the file on every pass, so the condition is visible in the logs;
// it is not visible in the numbers. Making it visible in the numbers would mean
// either withdrawing already-ingested spend (destroying honest rows) or holding
// every session's events until it ends (losing the live capture the collector
// exists for) — both worse trades than a logged, bounded under-report.
package codexrollout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

const (
	// DefaultScanInterval is the re-scan cadence when config omits scan_interval.
	// After the first pass a scan is a stat-gated walk plus a parse of only the
	// files touched since the previous pass (see the scan cursor on Collector),
	// and the idempotency keys make the resulting re-ingests no-ops at the store.
	// 5m keeps a running Codex session's spend visible on the dashboard within
	// one coffee-refill without spinning the disk.
	//
	// The FIRST pass is different and deliberately so: it parses the whole
	// history under the caller's `since` (backfill). Measured on 31 real local
	// sessions: 3.8 MB of input, 10.3 ms, ~9 MB of garbage. That is a fine
	// one-time cost and an unacceptable per-tick one, which is what the cursor
	// exists to prevent.
	DefaultScanInterval = 5 * time.Minute

	// cursorLagFactor multiplies the scan interval to produce the safety lag
	// subtracted from both cursors before use. Two intervals of overlap covers
	// coarse filesystem mtime granularity, a file appended DURING a scan (its
	// mtime advances after we read it), and modest clock skew. The cost of too
	// much lag is a few redundant no-op inserts; the cost of too little is
	// permanently missed spend, so the asymmetry is priced in favour of overlap.
	cursorLagFactor = 2

	// attributionLookback widens the git-log window used for issue attribution
	// relative to the event cursor. The join matches a commit within ±30 minutes
	// of an event (collector.joinWindow), so a cursor-tight `git log --since`
	// would drop the very commits a just-captured event needs and silently
	// downgrade it to branch-name attribution. A day is far past that window and
	// still bounds `git log` to a day of history per pass.
	attributionLookback = 24 * time.Hour

	// MinScanInterval is the config floor. Below this the walk cost stops being
	// negligible while adding no meaningful freshness — a Codex turn takes longer
	// than 30s to produce a token_count event in the first place.
	MinScanInterval = 30 * time.Second

	// rolloutFilePrefix / rolloutFileExt select rollout logs out of whatever else
	// lives under ~/.codex/sessions. Matching on BOTH keeps a future sibling
	// artifact (a cache, an index) from being parsed as a rollout.
	rolloutFilePrefix = "rollout-"
	rolloutFileExt    = ".jsonl"

	// maxRolloutLine caps the scanner's per-line buffer. Rollout logs embed the
	// full system prompt in session_meta.base_instructions, so lines are far
	// larger than Claude Code's; 10 MB matches the JSONL collector's cap and the
	// proxy's response buffer. A line beyond the cap ends the scan for that file
	// (surfaced via scanner.Err).
	maxRolloutLine = 10 << 20

	// providerOpenAI is the provider tag used when building cross-source
	// idempotency keys. Codex is an OpenAI-API client, so a Codex call captured
	// here and (hypothetically) by a future Responses-API-aware proxy would key
	// to the same namespace.
	providerOpenAI = "openai"
)

// maxRolloutFile bounds how many bytes we will read from one rollout log. Real
// logs are ~100 KB; 64 MB is far past any plausible session while capping the
// damage if some path produces a runaway file. Exceeding it FAILS the file (see
// parseRollout) — a truncated prefix would be a silent under-report.
//
// A var, not a const, purely so the boundary test can shrink it: proving the cap
// is enforced by writing an actual 64 MB fixture would put a multi-second,
// 64 MB-of-disk test into every `make check`.
var maxRolloutFile int64 = 64 << 20

// RepoTarget names one repository this collector attributes cost to.
type RepoTarget struct {
	// Path is the git checkout root. A rollout session whose cwd is at, inside,
	// or a git worktree of this path is attributed here.
	Path string
	// Slug is the OPERATOR OVERRIDE for the canonical "owner/repo" identity
	// (#231), winning over remote.origin.url. It exists for forks: a contributor
	// whose origin is "alice/tier" produces cost rows that would never join
	// outcomes from the upstream "tiermetric/tier" webhook. Empty falls back to
	// remote.origin.url, then to repoid.Unqualified.
	Slug string
}

// Collector reads Codex rollout logs and emits one TokenEvent per API call.
// It implements collector.Collector.
//
// EVERY FIELD IS UNEXPORTED and Config is the only way in. That is not style:
// New performs the checks that make the zero value impossible (at least one repo
// target, non-blank paths, a non-nil logger, a defaulted interval and sessions
// root), and an exported field set would let `&Collector{...}` skip all of them —
// the nil Logger alone panics on the first walk warning. Same shape as
// anthropicadmin.Poller and openaiusage.Poller.
type Collector struct {
	// sessionsDir is the ~/.codex/sessions root (or a test/operator override).
	sessionsDir string
	// repos are the repositories in scope. A session whose cwd matches none of
	// them is DROPPED, not attributed — cross-repo bleed would put another
	// project's dollars on this project's issues (#15). An empty list means no
	// session can ever match, which is a misconfiguration, so New rejects it.
	repos []RepoTarget
	// developerID labels every emitted event. Empty falls back to the OS
	// username via the same chain the JSONL collector uses.
	developerID string
	// interval is the re-scan cadence used by Run.
	interval time.Duration
	// logger receives scan diagnostics. Never nil after New.
	logger *slog.Logger

	// slugOnce/slugs memoize per-target repo slug resolution so the
	// "cannot determine repository slug" warning fires once per collector, not
	// once per scanned session file.
	slugOnce sync.Once
	slugs    []string

	// mu guards the mutable scan state below. Run is single-goroutine, but
	// nothing stops a caller from also invoking Collect, and the failed-file memo
	// is written on every pass.
	mu sync.Mutex
	// cursor is the scan window Run advances (see advanceCursor). Zero until the
	// first pass, which uses the caller's `since`.
	cursor scanWindow
	// failed memoizes files that failed a fatal parse, keyed by path, so a
	// permanently-corrupt log is reported at ERROR once rather than every tick
	// forever (#464 Y-G3).
	failed map[string]failedFile
	// damaged memoizes files that PARSED but lost every billable line to damage,
	// for the same reason and with the same (size, mtime) re-arming (#526). Kept
	// separate from `failed` so neither condition can suppress the other's report
	// just because the same path is already recorded.
	damaged map[string]failedFile
}

// scanWindow is the set of floors one scan pass applies. They are separate
// because they bound different things and a single value cannot serve all three:
// fileFloor decides which files are worth opening, eventFloor decides which
// parsed events are worth emitting, and gitFloor decides how much history the
// attribution join can see.
type scanWindow struct {
	fileFloor  time.Time // mtime pre-filter; zero disables the pre-filter
	eventFloor time.Time // per-event emission floor; zero emits everything parsed
	gitFloor   time.Time // git log --since for the attribution snapshot
}

// failedFile is the identity of a file whose parse failed, used to tell "the
// same broken file again" from "it changed, re-report it". Size and mtime
// together are enough: a rollout log only ever grows.
type failedFile struct {
	size    int64
	modTime time.Time
}

// Config configures a Collector. Repos is required; everything else defaults.
type Config struct {
	SessionsDir string
	Repos       []RepoTarget
	DeveloperID string
	Interval    time.Duration
	Logger      *slog.Logger
}

// New builds a Collector with defaults filled in, or returns an error for a
// configuration that could never capture anything.
//
// Fail-fast rather than silently-disabled is deliberate (correct-from-start): an
// operator who enabled the Codex collector and got zero rows must be told why at
// startup, not left to discover it from an empty dashboard weeks later.
func New(cfg Config) (*Collector, error) {
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("codex-rollout: at least one repo target is required (no target means no session can ever be attributed)")
	}
	for i, r := range cfg.Repos {
		if strings.TrimSpace(r.Path) == "" {
			return nil, fmt.Errorf("codex-rollout: repos[%d].Path is empty", i)
		}
	}
	c := &Collector{
		sessionsDir: cfg.SessionsDir,
		repos:       cfg.Repos,
		developerID: cfg.DeveloperID,
		interval:    cfg.Interval,
		logger:      cfg.Logger,
		failed:      make(map[string]failedFile),
		damaged:     make(map[string]failedFile),
	}
	if c.interval <= 0 {
		c.interval = DefaultScanInterval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.sessionsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("codex-rollout: resolve home dir for the default sessions root: %w", err)
		}
		c.sessionsDir = filepath.Join(home, ".codex", "sessions")
	}
	return c, nil
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return collector.SourceCodexRollout }

// Run implements collector.Collector: one scan immediately, then a re-scan every
// Interval until ctx is cancelled.
//
// DELIBERATE ASYMMETRY with the JSONL watcher, matching the org pollers: a scan
// failure is logged at ERROR and retried on the next tick — it does NOT abort Run
// and must NOT kill serve. A single corrupt rollout log (or a permissions blip on
// ~/.codex) is not a reason to take down the whole binary. This does not weaken
// the fail-loud posture on cost: a file that fails an invariant contributes zero
// events either way, so the loudness lives in the log and the missing rows, never
// in a wrong number.
//
// Events already produced by GOOD files in a pass are still ingested even when a
// sibling file failed — otherwise one bad log would blind the whole scan.
//
// THE SCAN CURSOR (#464 R3). `since` bounds the FIRST pass only. Every later pass
// runs against a cursor the collector advances itself, so the work per tick is
// proportional to what CHANGED, not to how much Codex history exists on the disk.
// Without it, `since` never moved and every rollout log ever written was
// re-walked, re-parsed, re-emitted and re-ingested every interval forever —
// correct (the idempotency keys absorb it) but unbounded: measured at 3.8 MB
// read / ~9 MB garbage per pass for 31 local sessions, i.e. ~1.2 GB read and
// ~2.9 GB garbage per tick at 10 000 sessions, plus ~60 000 no-op upserts against
// single-writer SQLite.
func (c *Collector) Run(ctx context.Context, since time.Time, ing collector.Ingester) error {
	if ing == nil {
		return fmt.Errorf("codex-rollout: Ingester is required")
	}
	// The first pass is the backfill: the caller's window, no cursor.
	c.setCursor(scanWindow{fileFloor: since, eventFloor: since, gitFloor: since})
	c.runPass(ctx, ing)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.runPass(ctx, ing)
		}
	}
}

// runPass performs one scan and ingests its events, logging (not returning) any
// error. Ingest errors abort the pass: an Ingester that is refusing writes will
// refuse the next event too, and hammering it for the rest of the backlog helps
// nobody.
//
// CURSOR ORDERING, the load-bearing part: the cursor advances ONLY after every
// event this pass produced has been handed to the Ingester successfully. An
// aborted ingest (write error or shutdown) leaves the cursor where it was, so the
// un-ingested tail is re-scanned next pass. Advancing first — or advancing in a
// defer — would drop that tail PERMANENTLY, and nothing downstream would ever
// know: the events simply never arrive.
//
// A pass in which some FILE failed to parse still advances the cursor. Those
// files produced no events (nothing was lost by moving on), they will fail again
// identically, and pinning the cursor on them would restore exactly the unbounded
// re-walk the cursor exists to remove. A file that later GROWS gets a fresh mtime
// and is re-parsed, so a log that heals is still picked up.
func (c *Collector) runPass(ctx context.Context, ing collector.Ingester) {
	passStart := time.Now().UTC()
	events, scanErr := c.scanOnce(ctx, c.window())

	var maxTS time.Time
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return // cursor deliberately NOT advanced
		}
		if err := ing.Ingest(ctx, ev); err != nil {
			c.logger.Error("codex-rollout ingest failed; will retry next scan", "err", err)
			return // cursor deliberately NOT advanced
		}
		if ev.Timestamp.After(maxTS) {
			maxTS = ev.Timestamp
		}
	}
	if ctx.Err() != nil {
		return
	}
	c.advanceCursor(passStart, maxTS)
	if scanErr != nil {
		c.logger.Error("codex-rollout scan reported failing session files; their spend was NOT ingested", "err", scanErr)
	}
}

// window returns the floors for the next pass, with the safety lag applied.
//
// The lag is subtracted at USE rather than baked into the stored cursor so the
// cursor keeps meaning "everything up to here is done" — a value that stays
// correct if the interval is ever reconfigured.
func (c *Collector) window() scanWindow {
	c.mu.Lock()
	defer c.mu.Unlock()
	lag := time.Duration(cursorLagFactor) * c.interval
	w := c.cursor
	// A zero floor means "no floor" and must stay zero: shifting it backwards
	// would turn it into a real (year-1-ish) timestamp and change nothing, but
	// shifting a NON-zero floor backwards is exactly the overlap we want.
	if !w.fileFloor.IsZero() {
		w.fileFloor = w.fileFloor.Add(-lag)
	}
	if !w.eventFloor.IsZero() {
		w.eventFloor = w.eventFloor.Add(-lag)
	}
	if !w.gitFloor.IsZero() {
		w.gitFloor = w.gitFloor.Add(-lag - attributionLookback)
	}
	return w
}

func (c *Collector) setCursor(w scanWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor = w
}

// advanceCursor moves the cursor forward after a fully-ingested pass.
//
// fileFloor moves to when the pass STARTED (not to now): a file appended while
// the scan was running must still look "touched since" on the next pass.
// eventFloor moves to the newest event actually ingested, and only ever forwards
// — a pass that emitted nothing must not drag the floor back to zero and re-emit
// the world.
func (c *Collector) advanceCursor(passStart, maxIngestedTS time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor.fileFloor = passStart
	c.cursor.gitFloor = passStart
	if maxIngestedTS.After(c.cursor.eventFloor) {
		c.cursor.eventFloor = maxIngestedTS
	}
}

// Collect implements collector.Collector: one scan returning every event in a
// slice.
//
// It may return BOTH events and a non-nil error — the error names the session
// files that failed their containment invariants and were therefore dropped
// whole, while the events are the ones from files that passed. Callers that need
// an all-or-nothing guarantee must treat any non-nil error as fatal; callers
// reporting a best-effort figure should render the events AND surface the error,
// never the events alone.
//
// Collect is STATELESS with respect to the scan cursor: it scans the caller's
// full window every time and neither reads nor advances the cursor Run keeps. A
// batch caller asking for "everything since T" must get everything since T, even
// if a Run loop on the same collector has already ingested some of it.
func (c *Collector) Collect(ctx context.Context, since time.Time) ([]collector.TokenEvent, error) {
	return c.scanOnce(ctx, scanWindow{fileFloor: since, eventFloor: since, gitFloor: since})
}

// scanTarget is one resolved repo target: the scope decision, the git-log
// attribution snapshot, and the canonical slug, all built once per scan pass.
type scanTarget struct {
	scope    *collector.RepoScope
	resolver *collector.IssueResolver
	slug     string
}

// scanOnce walks the sessions root, parses every rollout log the window admits,
// and returns the resulting events plus a joined error naming any file that
// failed.
func (c *Collector) scanOnce(ctx context.Context, w scanWindow) ([]collector.TokenEvent, error) {
	targets, targetErrs := c.resolveTargets(ctx, w.gitFloor)
	if len(targets) == 0 {
		// Every target failed to resolve; there is nothing to attribute to.
		return nil, errors.Join(targetErrs...)
	}

	paths, err := c.findRolloutFiles(w.fileFloor)
	if err != nil {
		return nil, errors.Join(append(targetErrs, err)...)
	}

	developer := c.developerID
	if developer == "" {
		developer = collector.OSUsername()
	}

	var (
		events  []collector.TokenEvent
		errs    = targetErrs
		foreign int
		empty   int
		idle    int
		failed  int
		// #526: malformed-line loss, which is tolerated rather than fatal and so
		// never reaches `failed`. damaged counts FILES that lost at least one
		// line; skippedLines is the total across them; damagedEmpty is the subset
		// that lost every billable line and would otherwise be reported as idle.
		damaged      int
		skippedLines int
		damagedEmpty int
		resuppd      int // parse failures suppressed as unchanged repeats (#464 Y-G3)
		seenFirst    = make(map[string]string, len(paths))
	)
	scopes := make([]*collector.RepoScope, len(targets))
	for i := range targets {
		scopes[i] = targets[i].scope
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return events, err
		}
		sess, err := parseRollout(path, c.logger)
		if err != nil {
			// FATAL for this file: it produced no events and must be named. Keep
			// scanning so one corrupt log cannot hide every other session's spend.
			failed++
			if c.noteFailure(path) {
				errs = append(errs, fmt.Errorf("%s: %w", logsafe.Str(path), err))
			} else {
				// Same file, same bytes, same failure as last pass. Already
				// reported at ERROR; repeating it every interval forever turns a
				// real signal into noise an operator learns to filter out.
				resuppd++
				c.logger.Debug("codex-rollout: unchanged file failed again (already reported)",
					"path", logsafe.Str(path), "err", err)
			}
			continue
		}
		c.clearFailure(path)
		if sess == nil {
			// UNREACHABLE under parseRollout's contract (#526): a nil session
			// always accompanies a non-nil error, handled above. Reported loudly
			// rather than absorbed as `idle`, because silently treating it as an
			// idle session is exactly the defect #526 fixed — if the contract is
			// ever broken, this must be the thing that says so.
			c.logger.Error("codex-rollout: parseRollout returned (nil, nil), violating its return contract",
				"path", logsafe.Str(path))
			failed++
			continue
		}
		if sess.SkippedLines == 0 {
			// Clean now: re-arm the damage report so a FUTURE corruption of this
			// path is warned about again rather than suppressed as a repeat.
			c.clearDamage(path)
		}
		// SCOPE FIRST (#526). ~/.codex/sessions is machine-GLOBAL: Codex writes
		// every session on the box there, while this collector only attributes
		// the configured Repos, so a foreign session is the COMMON case, not an
		// edge one. Accounting damage before this decision claimed "our spend is
		// missing" for logs that were never in scope — a warning that fires
		// routinely, which is the #464 defect this change's own control arm
		// guards against, re-entered through a different door.
		//
		// Computed once here and reused for the foreign check below, so the file
		// is still scoped exactly once.
		scopeIdx := collector.MatchScopes(scopes, sess.CWD)
		// An UNKNOWN cwd counts as ours. Total damage can consume session_meta
		// itself, leaving CWD == "" — and that is precisely the case worth
		// warning about, so gating on `scopeIdx >= 0` alone would silence the
		// loudest signal this change exists to produce. Only a session we can
		// POSITIVELY place in another repo is excluded.
		ourDamage := sess.SkippedLines > 0 && (sess.CWD == "" || scopeIdx >= 0)
		if ourDamage {
			skippedLines += sess.SkippedLines
			damaged++
		}
		if len(sess.Calls) == 0 {
			// #526: "no billable lines" and "damaged so badly no billable line
			// survived" are the SAME observation from the outside, and they mean
			// opposite things — one session cost nothing, the other cost something
			// we can no longer measure. Separating them is the whole point of
			// carrying SkippedLines out of the parser.
			if ourDamage {
				damagedEmpty++
				// Suppressed on repeat, exactly like a fatal parse failure
				// (#464 Y-G3). The bytes do not change between scans, so a
				// 5-minute serve loop — or a 15-minute ship cron — would mail
				// this same line forever with no action an operator can take to
				// clear it. "It never self-heals" is the argument FOR reporting
				// it once, not for repeating it. noteDamage re-arms when the
				// file's (size, mtime) changes, i.e. when Codex appends to it.
				if c.noteDamage(path) {
					c.logger.Warn("codex-rollout: a rollout log lost EVERY billable line: no line in it decoded; its spend is missing, not zero",
						"path", logsafe.Str(path), "skipped_lines", sess.SkippedLines)
				} else {
					c.logger.Debug("codex-rollout: unchanged damaged-to-empty log (already reported)",
						"path", logsafe.Str(path), "skipped_lines", sess.SkippedLines)
				}
			} else {
				idle++
			}
			continue
		}
		if sess.CWD == "" {
			empty++
			continue
		}
		// One session id in two files would mean two files' calls sharing the
		// (session, ordinal) idempotency key space — the second file's spend
		// would be swallowed by the store's unique index with no error anywhere.
		// MEASURED 2026-07-22 on the 31 local rollout files: the 30 carrying
		// billable calls had 30 DISTINCT session ids, so no id spanned two files.
		// That is evidence, not enforcement — warn if the assumption ever breaks,
		// so the swallow is visible before someone debugs a missing figure. See
		// buildEvents for why the key has no file component.
		if first, dup := seenFirst[sess.SessionID]; dup {
			c.logger.Warn("codex-rollout: one session id appears in TWO rollout files; the second file's calls collide with the first's idempotency keys and will be silently dropped by the store",
				"session_id", logsafe.Str(sess.SessionID),
				"first_file", logsafe.Str(first),
				"second_file", logsafe.Str(path))
		} else {
			seenFirst[sess.SessionID] = path
		}
		if scopeIdx < 0 {
			foreign++
			continue
		}
		events = append(events, c.buildEvents(sess, &targets[scopeIdx], developer, w.eventFloor)...)
	}
	if foreign > 0 || empty > 0 || idle > 0 || failed > 0 || damaged > 0 {
		c.logger.Info("codex-rollout scan filtered sessions",
			"scanned", len(paths),
			"emitting", len(events),
			"foreign_repo", foreign,
			"empty_cwd", empty,
			"no_token_count", idle,
			"failed", failed,
			"failed_repeats_suppressed", resuppd,
			// #526. damaged_files can be non-zero while every other counter here
			// is zero and the scan still emits events: a partially-damaged log
			// yields real spend AND lost some. That combination has no other
			// signal, which is exactly why it needs one.
			//
			// Read these three carefully, because they do NOT join the partition
			// the other keys form:
			//   - damaged_to_empty ⊆ damaged_files, and it TAKES files from
			//     no_token_count rather than adding to it — so an alert keyed on
			//     no_token_count now under-counts by exactly this amount.
			//   - damaged_files overlays the partition (a damaged file may also
			//     be counted in damaged_to_empty, empty_cwd, or emitting).
			//   - skipped_lines is a LINE count among file counts.
			// All three are scope-gated: a session positively placed in a repo
			// this collector does not watch is excluded, since its spend was
			// never ours to lose. A file whose cwd is UNKNOWN counts as ours.
			// A file that later failed a FATAL check contributes nothing here —
			// it is named by `failed` and the returned error instead — so
			// skipped_lines is not a complete loss total across the scan.
			"damaged_files", damaged,
			"skipped_lines", skippedLines,
			"damaged_to_empty", damagedEmpty,
		)
	}
	return events, errors.Join(errs...)
}

// noteFailure records that path failed to parse and reports whether this failure
// is NEW — either the first one for this path, or a different (size, mtime) than
// the one already recorded, which means the file changed and is worth reporting
// again. An unstattable file is always treated as new: losing the report is worse
// than repeating it.
func (c *Collector) noteFailure(path string) bool {
	return c.noteRepeat(c.failed, path)
}

// noteDamage is noteFailure's counterpart for a file that PARSED but lost every
// billable line to damage (#526). It uses a SEPARATE memo on purpose: damage and
// a fatal parse failure are different conditions, and sharing one map would let a
// damaged file suppress a later fatal error's ERROR line (or vice versa) purely
// because the same path was already recorded.
func (c *Collector) noteDamage(path string) bool {
	return c.noteRepeat(c.damaged, path)
}

// noteRepeat records path's current (size, mtime) in memo and reports whether
// this observation is NEW — the first for this path, or a different identity than
// the one already recorded, which means the file changed and is worth reporting
// again.
func (c *Collector) noteRepeat(memo map[string]failedFile, path string) bool {
	var id failedFile
	if info, err := os.Stat(path); err == nil {
		id = failedFile{size: info.Size(), modTime: info.ModTime()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, seen := memo[path]
	memo[path] = id
	return !seen || prev != id
}

// clearDamage forgets a path that now parses without losing lines, so a future
// corruption is warned about again rather than suppressed as a repeat. Also keeps
// the memo bounded across a long-lived process, like clearFailure.
func (c *Collector) clearDamage(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.damaged, path)
}

// clearFailure forgets a path that now parses cleanly, so a future failure is
// reported at ERROR again rather than suppressed as a repeat. It also keeps the
// memo from growing without bound across a long-lived process.
func (c *Collector) clearFailure(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.failed, path)
}

// resolveTargets builds the per-repo scope + attribution snapshot for one scan
// pass. The git-log snapshot is rebuilt each pass on purpose: attribution must
// see commits that landed since the last scan.
//
// ONE UNRESOLVABLE TARGET DOES NOT BLIND THE OTHERS. A repo that was renamed,
// unmounted, or had its .git removed used to abort the whole scan, so every
// OTHER repo's Codex spend stopped being captured until an operator noticed —
// the opposite of the per-file resilience this collector applies everywhere
// else. The failing target is skipped with its error joined into the scan's
// error, and the healthy ones keep capturing. A skipped target's sessions become
// foreign for that pass, which is honest: with no git log we cannot attribute
// them, and IssueResolver's nil-receiver fallback is for a resolver we chose not
// to build, not for a repo we cannot see.
func (c *Collector) resolveTargets(ctx context.Context, since time.Time) ([]scanTarget, []error) {
	c.resolveSlugs()
	targets := make([]scanTarget, 0, len(c.repos))
	var errs []error
	for i, r := range c.repos {
		resolver, err := collector.NewIssueResolver(ctx, r.Path, since)
		if err != nil {
			errs = append(errs, fmt.Errorf("codex-rollout: repo %q is not attributable this pass (skipped; other repos still scanned): %w", r.Path, err))
			continue
		}
		targets = append(targets, scanTarget{
			scope:    collector.NewRepoScope(r.Path),
			resolver: resolver,
			slug:     c.slugs[i],
		})
	}
	return targets, errs
}

// resolveSlugs resolves each target's canonical "owner/repo" slug exactly once
// per collector: operator override, else remote.origin.url, else the
// 'unqualified' sentinel.
//
// Degrading to the sentinel is deliberate rather than fatal — mirroring the JSONL
// collector. A repo we cannot NAME still produces true per-developer cost, and
// the primary TIER denominator groups by developer alone. What degrades is only
// cross-repo issue disambiguation, so failing here would trade a real number for
// no number. But it must be OBSERVABLE, never silent.
func (c *Collector) resolveSlugs() {
	c.slugOnce.Do(func() {
		c.slugs = make([]string, len(c.repos))
		for i, r := range c.repos {
			if slug, ok := repoid.Canonical(r.Slug); ok {
				c.slugs[i] = slug
				continue
			}
			if r.Slug != "" {
				c.logger.Warn("configured repo slug is not a canonical owner/repo; ignoring",
					"repo_path", r.Path, "configured", r.Slug)
			}
			if slug := collector.RepoSlugFromGitConfig(r.Path); slug != "" {
				c.slugs[i] = slug
				continue
			}
			c.slugs[i] = repoid.Unqualified
			c.logger.Warn("cannot determine repository slug; Codex cost rows will be repo-unqualified and multi-repo issues sharing a number will fuse",
				"repo_path", r.Path,
				"hint", "set the per-repo `repo:` override, or add a remote.origin.url")
		}
	})
}

// buildEvents turns one parsed session's per-call deltas into TokenEvents.
//
// Events older than `floor` are skipped here rather than at parse time: the
// deltas must be differenced over the FULL cumulative series or the first
// surviving event would absorb the whole pre-window prefix as its own usage.
func (c *Collector) buildEvents(s *rolloutSession, t *scanTarget, developer string, floor time.Time) []collector.TokenEvent {
	events := make([]collector.TokenEvent, 0, len(s.Calls))
	for _, call := range s.Calls {
		ts := call.Timestamp
		if ts.IsZero() {
			// Guaranteed non-zero: parseRollout refuses a file that has neither a
			// per-event timestamp here nor any timestamp to fall back on, because
			// a zero time stores as year 1 and drops out of every window.
			ts = s.StartTime
		}
		// Normalize to UTC (#199): modernc.org/sqlite compares DATETIME strings
		// lexically, so an offset-bearing timestamp would window incorrectly
		// against the UTC query bounds. Codex writes Z today; do not rely on it.
		ts = ts.UTC()
		if ts.Before(floor) {
			continue
		}
		cost, billingMode := store.ComputeCostHost("", call.Model, store.CostUsage{
			Input:     call.Input,
			Output:    call.Output,
			CacheRead: call.CacheRead,
			// OpenAI has no cache-write SKU, so both write buckets stay zero —
			// identical to the proxy's parseOpenAI. See parseUsage for what
			// happens to a nonzero cache_write_input_tokens.
		})
		events = append(events, collector.TokenEvent{
			Developer: developer,
			// Attribution uses the session's git branch (recorded by Codex in
			// session_meta.git) and THIS call's timestamp, resolved by the same
			// rule the Claude Code join uses. A rollout log with no git info
			// resolves to a labeled unattributed bucket, so the spend still
			// lands in the developer's denominator instead of vanishing.
			IssueID:   t.resolver.Resolve(s.Branch, ts),
			Model:     call.Model,
			InputTok:  call.Input,
			OutputTok: call.Output,
			CacheRead: call.CacheRead,
			CostMicro: cost,
			Source:    collector.SourceCodexRollout,
			Fidelity:  collector.FidelityRealtime,
			// Keyed by (session id, event ordinal). The ordinal is the
			// token_count event's position in the file, which is stable across
			// re-scans of an append-only log — including across the skipped
			// zero-delta duplicates, whose ordinals are consumed but not
			// emitted. Re-ingesting the same rollout therefore collides on the
			// store's partial unique index and stores one row per call.
			//
			// NO FILE COMPONENT, deliberately. Adding the basename would make the
			// key robust to one session id appearing in two files, but would make
			// it FRAGILE to a file being renamed (every call re-keys and its spend
			// is counted twice) — and doubling a figure is the failure this
			// collector exists to avoid, while the collision under-reports. The
			// one-id-one-file assumption is VERIFIED on the 31 local sessions and
			// is not enforced by anything, so scanOnce warns loudly if it ever
			// sees a second file claim a session id.
			IdempotencyKey: collector.IdempotencyKey(
				collector.SourceCodexRollout, providerOpenAI, s.SessionID, strconv.Itoa(call.Ordinal)),
			Repo: t.slug,
			// Host is deliberately left empty (the store normalizes it to the
			// unknown sentinel). Only the proxy can know the serving host it
			// dialed; a local log records the client's view, and Codex may be
			// pointed at any OpenAI-compatible base URL. Claiming "openai" here
			// would forge a host-qualified pricing key we cannot substantiate.
			Host:        "",
			BillingMode: billingMode,
			SessionID:   s.SessionID,
			Timestamp:   ts,
		})
	}
	return events
}

// findRolloutFiles walks the sessions root and returns the rollout logs whose
// mtime is at or after `floor`, in lexical (filepath.WalkDir) order — which, for
// the YYYY/MM/DD layout Codex uses, is chronological.
//
// The mtime pre-filter is a cheap skip, not the authoritative window: a file's
// mtime is its LAST write, so mtime < floor proves every event in it predates the
// window. The per-event floor check in buildEvents is what actually bounds the
// output. From the second pass onwards this filter is also what bounds the WORK
// (#464 R3) — without a moving floor every historical log is reopened per tick.
//
// A missing sessions root is NOT an error — Codex may simply never have run on
// this machine, and an operator who enabled the collector ahead of installing
// Codex should get an empty scan, not a startup failure that also kills the
// Claude Code capture path. That case is handled by the Stat BELOW rather than
// by inspecting WalkDir's error: WalkDir reports a missing root by invoking the
// callback with a non-nil err, and this callback deliberately swallows per-entry
// errors and returns nil — which makes WalkDir itself return nil. Any
// fs.ErrNotExist branch on the walk's return value is therefore DEAD CODE, and
// the real behaviour would be a walk-error WARN every interval forever for the
// most ordinary configuration there is (#464 Y-G1).
func (c *Collector) findRolloutFiles(floor time.Time) ([]string, error) {
	if _, err := os.Stat(c.sessionsDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.logger.Info("codex-rollout sessions root does not exist; nothing to scan",
				"sessions_dir", logsafe.Str(c.sessionsDir))
			return nil, nil
		}
		return nil, fmt.Errorf("codex-rollout: sessions root %q: %w", c.sessionsDir, err)
	}
	var paths []string
	err := filepath.WalkDir(c.sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Per-entry errors (permission denied on one day directory, a
			// vanished temp file) must not abort the walk: the other days'
			// spend is still real and still capturable.
			c.logger.Warn("codex-rollout walk error", "path", logsafe.Str(path), "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, rolloutFilePrefix) || filepath.Ext(base) != rolloutFileExt {
			return nil
		}
		// REGULAR FILES ONLY, checked after the name filter so an unrelated
		// socket or symlink in the tree is skipped silently and only a
		// rollout-NAMED impostor is worth a warning.
		//
		// WalkDir uses Lstat, so d describes the link itself: a symlink named
		// rollout-x.jsonl would be followed by os.Open and read from anywhere on
		// the filesystem, and a FIFO with that name would block os.Open forever —
		// parseRollout has no timeout and checks no context, so that one entry
		// would hang the collector goroutine for the life of the process. Neither
		// is a rollout log.
		if !d.Type().IsRegular() {
			c.logger.Warn("codex-rollout: refusing a non-regular file that is named like a rollout log",
				"path", logsafe.Str(path), "mode", d.Type().String())
			return nil
		}
		if !floor.IsZero() {
			info, ierr := d.Info()
			if ierr == nil && info.ModTime().Before(floor) {
				return nil
			}
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codex-rollout: walk %q: %w", c.sessionsDir, err)
	}
	return paths, nil
}
