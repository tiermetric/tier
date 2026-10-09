package main

// tierd ship (#126, capture-topology Option A): the thin laptop shipper.
// Runs the local JSONL collector over each --repo and forwards the events to
// a central tierd's POST /api/v1/events instead of a local SQLite store.
//
// STATELESS BY DESIGN: no checkpoint file exists or is needed. Every event
// carries an idempotency key and the server's MAX-on-conflict UPSERT absorbs
// re-sends, so re-shipping the same 90 days on every cron tick is a no-op.
// Typical deployment: a 15-minute cron —
//
//	*/15 * * * * tierd ship --server https://tier.example --repo ~/src/app
//
// or the launchd equivalent on macOS (StartInterval 900).

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/collector/muse"
	"github.com/tiermetric/tier/internal/collector/opencode"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/shipper"
	"github.com/tiermetric/tier/internal/store"
)

// shipCodexRollout scans the Codex sessions root once and forwards every event
// it yields into the shipper.
//
// Collect, not Run: Run is a LOOP that re-scans on an interval until ctx is
// cancelled, which is right for `serve` and would hang a cron-driven `ship`
// forever. Collect is one stateless pass over the caller's whole window, which
// is exactly the shipper's contract — stateless, no checkpoint, re-shippable.
//
// PARTIAL RESULTS ARE REAL. Collect returns the events from the files that
// parsed AND an error naming the files that did not; the two are not exclusive.
// The caller ships the events regardless and treats the error as fatal AFTER
// the flush, so one corrupt rollout log cannot silently zero out a laptop's
// whole Codex spend.
func shipCodexRollout(ctx context.Context, targets []codexrollout.RepoTarget, sessionsDir, developer string, since time.Time, ing collector.Ingester) error {
	// WARN, not the slog default's INFO. `ship` is documented as a */15 cron
	// job; the collector logs at Info on any pass that filtered a session, and
	// "a Codex session outside the --repo set" counts as filtered, so the
	// default level would mail the operator a line every fifteen minutes on a
	// perfectly healthy run. A successful ship stays silent; warnings and
	// errors still surface. TIER_LOG_LEVEL overrides it, because "codex-rollout
	// scan filtered sessions ... foreign_repo=N" at INFO is the diagnostic for
	// "why did my Codex scan find nothing?" and silencing it with no way back
	// would trade one invisible failure for another.
	logger, err := newLogger(os.Stderr, "auto", cmp.Or(os.Getenv("TIER_LOG_LEVEL"), "warn"))
	if err != nil {
		return err
	}
	c, err := codexrollout.New(codexrollout.Config{
		SessionsDir: sessionsDir,
		Repos:       targets,
		DeveloperID: developer,
		Logger:      logger,
	})
	if err != nil {
		// Wrapped, not rebuilt with errors.New: flattening to a string would
		// discard the %w chain (e.g. the underlying home-dir resolution error)
		// and errors.Is/As would stop working on it forever. The caller strips
		// the duplicated "codex-rollout: " prefix at the print site instead.
		return fmt.Errorf("%w", err)
	}
	events, scanErr := c.Collect(ctx, since)
	for _, ev := range events {
		if err := ing.Ingest(ctx, ev); err != nil {
			// An ingest failure is the wire breaking, not a bad file: stop and
			// report it, but keep the scan error too — losing it here would
			// hide a corrupt log behind a transport error.
			return errors.Join(scanErr, err)
		}
	}
	return scanErr
}

// shipOpencode scans the Opencode SQLite session store once and forwards every
// event it yields into the shipper (#719). The Codex twin of this function is
// directly above; read its doc first — the Collect-not-Run reasoning and the
// partial-results posture are identical, and are not repeated here.
//
// The database is opened READ-ONLY. Opencode may be running while `ship` reads
// it, which is exactly why it must never be opened with `immutable=1`.
func shipOpencode(ctx context.Context, targets []opencode.RepoTarget, dbPath, developer string, since time.Time, ing collector.Ingester) error {
	// WARN by default, for the same cron-noise reason shipCodexRollout uses it:
	// this collector logs its per-pass capture policy and exclusion counts at
	// INFO, which is the right level for `serve` and would mail the operator a
	// line every fifteen minutes from a healthy cron. TIER_LOG_LEVEL overrides it.
	logger, err := newLogger(os.Stderr, "auto", cmp.Or(os.Getenv("TIER_LOG_LEVEL"), "warn"))
	if err != nil {
		return err
	}
	c, err := opencode.New(opencode.Config{
		DBPath:      dbPath,
		Repos:       targets,
		DeveloperID: developer,
		Logger:      logger,
		// No Checkpoints: `ship` is documented as STATELESS and re-shippable, so
		// it must scan the caller's whole --since window every run. A watermark
		// would make a second run in the same window ship nothing and quietly
		// turn a re-ship — the documented recovery action — into a no-op.
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	events, scanErr := c.Collect(ctx, since)
	// An absent Opencode database is not a failure: the operator asked for
	// Opencode capture on a machine where Opencode has not run. Say so once and
	// exit 0, rather than failing a cron job for a configuration that is merely
	// premature.
	if opencode.IsDisabled(scanErr) {
		fmt.Fprintf(os.Stderr, "ship opencode: %s\n", logsafe.Err(scanErr))
		return nil
	}
	for _, ev := range events {
		if err := ing.Ingest(ctx, ev); err != nil {
			return errors.Join(scanErr, err)
		}
	}
	return scanErr
}

// shipMuse scans the Muse sessions tree once and forwards every event it yields
// into the shipper (#895). The Codex twin above carries the Collect-not-Run and
// partial-results reasoning. A run still in progress is not returned; the next
// cron run ships it once it settles — `ship` is stateless and re-shippable.
func shipMuse(ctx context.Context, targets []muse.RepoTarget, home, developer string, since time.Time, ing collector.Ingester) error {
	// WARN by default, for the cron-noise reason shipCodexRollout gives.
	logger, err := newLogger(os.Stderr, "auto", cmp.Or(os.Getenv("TIER_LOG_LEVEL"), "warn"))
	if err != nil {
		return err
	}
	c, err := muse.New(muse.Config{Home: home, Repos: targets, DeveloperID: developer, Logger: logger})
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	events, scanErr := c.Collect(ctx, since)
	for _, ev := range events {
		if err := ing.Ingest(ctx, ev); err != nil {
			return errors.Join(scanErr, err)
		}
	}
	return scanErr
}

// repoSummary is what one --repo target contributed to a ship run: its
// resolved path, how many DISTINCT sessions produced at least one shipped
// event, and how many events those sessions produced (#549 arm 2). Printed
// unconditionally at the end of a run so a zero row is visible without
// reading the log, and it is also the input to allReposEmpty (arm 3).
type repoSummary struct {
	Path               string
	SessionsWithEvents int
	EventsShipped      int
	Failed             bool
}

// repoTally wraps the shipper client to count sessions and events for ONE
// --repo target, purely for the completion summary and the empty-repo exit
// guard (#549 arms 2 and 3). It is a transparent passthrough — Ingest always
// forwards to Next first and only tallies on success — so wrapping cannot
// change what ships, only what this run reports about what shipped.
type repoTally struct {
	Next collector.Ingester

	// mu guards the counters below. collector.Ingester's contract is explicit
	// that "implementations must be safe to call from any goroutine" — the
	// fsnotify watcher and the proxy both call Ingest concurrently, and
	// shipper.Client mutex-guards itself for exactly this reason.
	//
	// Today this type is only reached from runShip's sequential
	// loop, so the lock is uncontended and no race exists. It is here anyway
	// because a decorator that silently does NOT hold its interface's stated
	// invariant is a landmine: the next person to wire it into the watcher
	// gets a data race that `go test -race` will not catch until it is in
	// serve. One uncontended lock per event against an HTTP POST every 500
	// events is unmeasurable.
	mu       sync.Mutex
	events   int
	sessions map[string]struct{}
}

// Ingest implements collector.Ingester.
func (t *repoTally) Ingest(ctx context.Context, ev collector.TokenEvent) error {
	if err := t.Next.Ingest(ctx, ev); err != nil {
		// Do not count a failed forward — a partial/failed Ingest must not
		// inflate the summary this run reports, since runShip exits non-zero
		// on this error anyway (see the tally.Ingest call site) and the summary
		// would never be printed.
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events++
	// Keyed by SessionID, not a per-event counter: two events from the same
	// session must count as ONE session. The map is constructed at the single
	// construction site rather than lazily here, so this stays a plain assign
	// with no per-event nil branch.
	t.sessions[ev.SessionID] = struct{}{}
	return nil
}

// WithEvents returns the number of distinct sessions this tally saw an event
// from.
//
// ⚠️ NOT the same quantity as filterSessionsByRepo's `kept`, which counts
// sessions IN SCOPE. A session can be correctly in scope and yield no billable
// event, in which case that diagnostic says kept=1 and this says 0. The two
// were both called "kept" in the first draft, in operator-facing output on the
// same run — hence the rename.
func (t *repoTally) WithEvents() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// EventsShipped returns the events this tally forwarded successfully.
func (t *repoTally) EventsShipped() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.events
}

// shipRecoveredNothing reports whether this run recovered NOTHING AT ALL — no
// --repo kept a Claude Code session AND nothing reached the server from any
// path, Codex included (#549 arm 3).
//
// 🔴 THE `shipped` ARGUMENT IS LOAD-BEARING AND WAS MISSING IN THE FIRST DRAFT.
// The cross-repo sources (Codex, and Opencode since #719) are each scanned ONCE
// across every --repo — their stores are one flat tree or one database keyed by
// cwd, not a per-repo directory — so they produce no `repoSummary` row and are
// therefore invisible to allReposEmpty. (They ingest through their own
// `repoTally` as of #719, which is what the per-source summary line counts; that
// tally is still not in `repoSummaries`, so the conclusion is unchanged.) Gating
// the exit on allReposEmpty alone made a
// COMPLETELY HEALTHY Codex-only machine exit 1 — two independent reviews
// reproduced it: 4 Codex events and $0.0638 of real spend landed in the store,
// and the run still died claiming "almost always a wrong --repo path".
//
// That is strictly worse than the bug #549 exists to fix. Under the documented
// */15 cron it is a failure mail every fifteen minutes on a working install,
// and the remedy the flag help used to suggest (--allow-empty on a Codex-only
// box) would permanently disable this guard on exactly the machine most likely
// to need it — restoring the original false green.
//
// When every repo is empty, client.Shipped() is exactly the Codex count, so
// this is precise rather than a fudge.
func shipRecoveredNothing(summaries []repoSummary, shipped int) bool {
	return shipped == 0 && allReposEmpty(summaries)
}

// allReposEmpty reports whether EVERY repo summary kept zero sessions. It is
// one INPUT to the exit decision, never the whole of it — see
// shipRecoveredNothing.
//
// Split out as a pure function so the exit decision is directly unit
// testable: runShip's actual non-zero exit goes through os.Exit, which would
// kill the test process if invoked in-process (ship.go is one of this
// codebase's two legacy os.Exit subcommands — see the file's top-of-function
// comment on why it isn't refactored to `return int` here). Testing THIS
// function instead proves the guard's logic without needing a subprocess for
// every case; the subprocess-based smoke test (ship_exitcode_smoke_test.go,
// integration-tagged) proves the os.Exit wiring itself.
//
// An empty summaries slice (defensive; --repo always yields at least ".")
// counts as empty — vacuously true is the fail-loud answer here, not the
// permissive one.
func allReposEmpty(summaries []repoSummary) bool {
	for _, s := range summaries {
		if s.SessionsWithEvents > 0 {
			return false
		}
	}
	return true
}

// crossRepoSource is one capture path that is scanned ONCE across every --repo
// rather than per repo, and therefore has no per-repo summary row of its own.
// Both of them (Codex rollout logs, the Opencode store) read a single flat store
// keyed by each session's cwd, not a per-repo directory.
type crossRepoSource struct {
	// Label is the name printed in the summary.
	Label string
	// Enabled records whether the path RAN. It is carried separately from the
	// count because "it ran and found nothing" and "it never ran" are different
	// facts and the operator needs to tell them apart — see the note below.
	Enabled bool
	// Events is the number of events this path shipped, counted DIRECTLY by its
	// own tally.
	//
	// ⚠️ IT USED TO BE DERIVED as totalShipped minus the per-repo Claude tallies,
	// which was correct only while there was exactly one such path. A second one
	// makes a subtraction silently attribute BOTH paths' events to whichever is
	// printed — so the derivation was replaced with per-path tallies when the
	// Opencode path landed (#719), not left to be discovered as a wrong number.
	Events int
}

// printRepoSummary writes one line per --repo target's sessions-kept and
// events-shipped counts (#549 arm 2), then one line per cross-repo source.
// Printed unconditionally — including on the exit-1 empty-repo path below —
// because the entire point is that a zero row must be visible without reading the
// log or diffing the store by hand.
func printRepoSummary(summaries []repoSummary, extras []crossRepoSource) {
	fmt.Println("Per-repo summary:")
	for _, s := range summaries {
		failureNote := ""
		if s.Failed {
			failureNote = " failed=true"
		}
		fmt.Printf("  %s: sessions_with_events=%d events_shipped=%d%s\n",
			logsafe.Str(s.Path), s.SessionsWithEvents, s.EventsShipped, failureNote)
	}
	// Printed whenever the path was ENABLED, INCLUDING at zero: without this line
	// the summary could show every repo at zero while a cross-repo path's spend
	// had in fact landed, which is what made the first draft of #549's exit guard
	// fire on a healthy machine.
	for _, e := range extras {
		if e.Enabled {
			fmt.Printf("  %s (all repos): events_shipped=%d\n", e.Label, e.Events)
		}
	}
}

func runShip(args []string) {
	fs := flag.NewFlagSet("ship", flag.ExitOnError)
	server := fs.String("server", "", "central tierd base URL, e.g. https://tier.example (required)")
	apiToken := fs.String("api-token", "", "API token sent as Authorization: Bearer. When the flag is not given, TIER_API_TOKEN is used. Prefer TIER_API_TOKEN env var or @/path/to/file; a literal value here leaks via ps/shell history (#37)")
	var repos repeatableStringSlice
	fs.Var(&repos, "repo", `git repo path whose Claude Code sessions to ship (repeatable; default ".")`)
	var repoSlugs repeatableStringSlice
	fs.Var(&repoSlugs, "repo-slug", `canonical repository identity for one --repo, as "<path>=<owner/repo>" (repeatable; #231). Omit to read remote.origin.url. REQUIRED ON A FORK: origin names the fork, while the upstream webhook records outcomes against the upstream, so without this your cost never joins your outcomes`)
	claudeDir := fs.String("claude-dir", "", "override ~/.claude directory (for testing)")
	sinceStr := fs.String("since", "", "start date, e.g. 2026-01-01 (default: 90 days ago). Over-shipping is safe: the server dedups on idempotency keys, so a wide window costs nothing")
	developer := fs.String("developer", "", "developer ID override (default: OS username)")
	codexRollout := fs.Bool("codex-rollout", envBool("TIER_CODEX_ROLLOUT"), "also ship Codex CLI spend from the local rollout logs at ~/.codex/sessions/**/rollout-*.jsonl (#464). The rollout logs are the path that captures Codex as you actually run it: the reverse proxy can parse the Responses API Codex speaks (#459), but only for traffic you deliberately point at it with API-key auth, and that path is not yet live-verified. Do NOT do both at once — Codex routed through /openai/ while this flag is on is counted TWICE (the proxy keys on the response id, this collector keys on session+ordinal, and the two cannot dedup). Attributes to the same --repo set as the Claude Code scan. OFF by default, mirroring `tierd serve --codex-rollout`. Env TIER_CODEX_ROLLOUT")
	codexSessionsDir := fs.String("codex-sessions-dir", "", "override ~/.codex/sessions directory (for testing)")
	opencodeFlag := fs.Bool("opencode", envBool("TIER_OPENCODE"), "also ship Opencode spend from its local SQLite session store at ~/.local/share/opencode/opencode.db (#719). Opened READ-ONLY; Opencode may keep running. Ships only providers with an AUDITED per-token rate — today the Z.ai coding plan — and names every exclusion; Ollama's cloud tier is excluded because it publishes no per-token rate. GLM-5.3 needs no price-table override: the embedded glm-5.3 row prices it per token. Do NOT also route Opencode through tier's proxy: the two idempotency keys are unrelatable and the spend DOUBLES. Attributes to the same --repo set as the Claude Code scan. OFF by default, mirroring `tierd serve --opencode`. Env TIER_OPENCODE")
	opencodeDB := fs.String("opencode-db", "", "override the ~/.local/share/opencode/opencode.db path (for testing, or a relocated XDG_DATA_HOME)")
	museFlag := fs.Bool("muse", envBool("TIER_MUSE"), "also ship Meta Muse Code spend from its local session logs at ~/.local/share/muse/sessions/**/session.jsonl (#895). Reads only token counts, model names, the session id (shipped), each run's recorded branch and the workspace root — never prompt, reply or tool text. A run still in progress is shipped by a later run once it ends. Attributes to the same --repo set as the Claude Code scan. OFF by default, mirroring `tierd serve --muse`. Env TIER_MUSE")
	museHome := fs.String("muse-home", "", "override the ~/.local/share/muse Muse home (for testing, or a relocated XDG_DATA_HOME)")
	pricesPath := fs.String("prices", os.Getenv("TIER_PRICES"), "path to a price-table YAML override (#68); empty uses the embedded default. A bad file fails the command. Mirrors `tierd score --prices` and exists for the same reason: the SERVER re-prices every shipped event authoritatively, but the shipper also computes a local cost_usd that the server compares against to detect a mixed-version fleet — so a shipper on a different table reports a divergence on every event. It matters most for a route whose rate lives ONLY in an override, e.g. a host-qualified <model>@<host> row")
	worktreeAttr := fs.Bool(worktreeAttrFlag, false, worktreeAttrHelp)
	allowEmpty := fs.Bool("allow-empty", envBool("TIER_SHIP_ALLOW_EMPTY"), "exit 0 instead of 1 when the run recovered nothing at all (#549 arm 3). Off by default: a run that recovers nothing is almost always a wrong --repo path, and the whole point of #549 is that such a run must not look like a successful one. Env TIER_SHIP_ALLOW_EMPTY")
	_ = fs.Parse(args)
	secretEnvFallback(fs, apiToken, "api-token", "TIER_API_TOKEN")

	if *server == "" {
		fmt.Fprintln(os.Stderr, "--server is required (the central tierd base URL)")
		os.Exit(1)
	}
	// Resolve the price table BEFORE any collector runs — they price on the way
	// out. A bad file fails the command rather than silently falling back to the
	// embedded default, which is the same fail-loud posture serve and score take.
	if *pricesPath != "" {
		info, err := store.LoadPriceTable(*pricesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "prices: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "ship: price table %s (version %d)\n", *pricesPath, info.Version)
	}
	// Resolve @file indirection so the token never sits on the command line
	// (#37) — same contract as tierd serve.
	token, err := resolveSecretFlag("--api-token", *apiToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--api-token: %v\n", err)
		os.Exit(1)
	}
	since, err := parseSince(*sinceStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since value: %v\n", err)
		os.Exit(1)
	}
	if len(repos) == 0 {
		repos = repeatableStringSlice{"."}
	}
	wtSetting, err := resolveWorktreeAttribution(fs, *worktreeAttr, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--%s: %v\n", worktreeAttrFlag, err)
		os.Exit(1)
	}
	// The worktree index covers every --repo, each under the path its own
	// collector resolves below, so with the flag on every path must resolve
	// before any repo ships.
	var wt *collector.WorktreeAttribution
	if wtSetting.on {
		fmt.Fprintf(os.Stderr, "ship: %s\n", wtSetting)
		repoPaths := make([]string, 0, len(repos))
		for _, repo := range repos {
			repoPath, err := resolveRepo(repo)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cannot resolve repo %q: %v\n", repo, err)
				os.Exit(1)
			}
			repoPaths = append(repoPaths, repoPath)
		}
		wt = wtSetting.attribution(repoPaths)
	}

	client, err := shipper.New(shipper.Config{ServerURL: *server, APIToken: token})
	if err != nil {
		fmt.Fprintf(os.Stderr, "shipper: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// One collector per repo, all funneling into the same HTTP client so
	// batches can span repos. Any failure exits non-zero: a silently
	// half-shipped run would look like missing spend on the server, and the
	// next stateless run re-covers everything anyway.
	slugFor, err := parseRepoSlugPairs(repoSlugs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--repo-slug: %v\n", err)
		os.Exit(1)
	}
	// Match the same typed/cleaned and absolute spellings the lookup below
	// accepts. Report unused keys so a typo cannot silently lose an override.
	for path := range slugFor {
		matched := false
		for _, repo := range repos {
			abs, err := filepath.Abs(repo)
			if path == filepath.Clean(repo) || (err == nil && path == abs) {
				matched = true
				break
			}
		}
		if !matched {
			fmt.Fprintf(os.Stderr, "ship: warning: --repo-slug mapping key %s does not match any --repo; mapping ignored\n", logsafe.Str(path))
		}
	}

	codexTargets := make([]codexrollout.RepoTarget, 0, len(repos))
	opencodeTargets := make([]opencode.RepoTarget, 0, len(repos))
	museTargets := make([]muse.RepoTarget, 0, len(repos))
	repoSummaries := make([]repoSummary, 0, len(repos))
	repoFailed := false
	for _, repo := range repos {
		// An unreadable --repo path (os.Stat failure) intentionally stops the run.
		repoPath, err := resolveRepo(repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve repo %q: %v\n", repo, err)
			os.Exit(1)
		}
		// Match on both the operator-typed path and its resolved form, so
		// `--repo . --repo-slug .=owner/repo` works as written.
		slug := firstNonEmpty(slugFor[filepath.Clean(repo)], slugFor[repoPath])
		// Reuse the SAME resolved path and operator slug override the Claude
		// Code scan uses (#231), so a repo's Claude and Codex rows carry
		// one identity and join the same outcomes. A fork whose origin names
		// the fork would otherwise split its cost across two repo identities.
		codexTargets = append(codexTargets, codexrollout.RepoTarget{Path: repoPath, Slug: slug})
		opencodeTargets = append(opencodeTargets, opencode.RepoTarget{Path: repoPath, Slug: slug})
		museTargets = append(museTargets, muse.RepoTarget{Path: repoPath, Slug: slug})
		c := &collector.JSONLCollector{
			RepoPath:    repoPath,
			ClaudeDir:   *claudeDir,
			DeveloperID: *developer,
			RepoSlug:    slug,
		}
		c.SetWorktreeAttribution(wt)
		events, err := c.Collect(ctx, since)
		if err != nil {
			if err := ctx.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "ship %s: %v\n", repoPath, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "ship %s: %s; skipping repo\n", logsafe.Str(repoPath), logsafe.Err(err))
			repoSummaries = append(repoSummaries, repoSummary{Path: repoPath, Failed: true})
			repoFailed = true
			continue
		}
		// tally sits BETWEEN the collector and the real shipper client: every
		// event still reaches client exactly as before (Ingest forwards first,
		// counts second), so wrapping it changes nothing about what ships —
		// only what this run can report about what shipped (#549 arms 2/3).
		tally := &repoTally{Next: client, sessions: map[string]struct{}{}}
		for _, ev := range events {
			if err := ctx.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "ship %s: %v\n", repoPath, err)
				os.Exit(1)
			}
			if err := tally.Ingest(ctx, ev); err != nil {
				fmt.Fprintf(os.Stderr, "ship %s: ingest: %v\n", repoPath, err)
				os.Exit(1)
			}
		}
		repoSummaries = append(repoSummaries, repoSummary{
			Path:               repoPath,
			SessionsWithEvents: tally.WithEvents(),
			EventsShipped:      tally.EventsShipped(),
		})
	}

	// Codex is scanned ONCE across every --repo rather than per repo: the
	// sessions root is a single flat, date-partitioned tree keyed by each
	// session's cwd, not a per-repo directory like ~/.claude/projects. Scanning
	// it per repo would re-walk and re-parse the whole tree N times to emit the
	// same events.
	//
	// Each cross-repo path gets its OWN tally so the summary reports what that
	// path actually shipped rather than inferring it by subtraction — see
	// crossRepoSource.Events.
	var codexErr error
	codexTally := &repoTally{Next: client, sessions: map[string]struct{}{}}
	if *codexRollout {
		codexErr = shipCodexRollout(ctx, codexTargets, *codexSessionsDir, *developer, since, codexTally)
	}
	// Opencode is likewise scanned ONCE across every --repo: it is a single
	// SQLite store keyed by each message's cwd, not a per-repo directory.
	var opencodeErr error
	opencodeTally := &repoTally{Next: client, sessions: map[string]struct{}{}}
	if *opencodeFlag {
		opencodeErr = shipOpencode(ctx, opencodeTargets, *opencodeDB, *developer, since, opencodeTally)
	}
	// Muse is likewise scanned ONCE: one date-partitioned tree keyed by each
	// session's workspace root.
	var museErr error
	museTally := &repoTally{Next: client, sessions: map[string]struct{}{}}
	if *museFlag {
		museErr = shipMuse(ctx, museTargets, *museHome, *developer, since, museTally)
	}

	// Flush the final partial batch — without this, up to batchSize-1
	// trailing events would be dropped on every run. This runs even when the
	// Codex scan failed: Collect returns the events from the files that DID
	// parse alongside the error, and dropping good spend on the floor is the
	// exact "Codex work looks free" failure #492 exists to close. Ship what we
	// have, then fail loudly.
	if err := client.Flush(ctx); err != nil {
		// codexErr is joined in rather than reported separately, because the
		// two failures are not independent: when Ingest broke mid-batch the
		// shipper RETAINS the unsent events, so this Flush re-POSTs them and
		// fails identically. Reporting only the flush error would then discard
		// the scan error naming the corrupt rollout files — the exact
		// information the join inside shipCodexRollout exists to preserve.
		fmt.Fprintf(os.Stderr, "ship: flush: %v\n", errors.Join(codexErr, opencodeErr, museErr, err))
		os.Exit(1)
	}
	// #549 arm 2: printed HERE — after Flush so the counts are real, but BEFORE
	// the codex-failure exit below. Review found the first draft's comment
	// claimed "unconditionally" while the call sat beneath three os.Exit(1)s, so
	// on the codex partial-failure path — where "which repo contributed what"
	// matters MOST — the operator got exit 1 and no summary at all.
	//
	// ⚠️ It cannot move ABOVE Flush, which was the first attempt at this fix:
	// shipper.Client increments its shipped counter inside flushLocked, so
	// pre-Flush Shipped() omits the final partial batch while repoTally has
	// already counted those events as ingested — making the derived Codex row
	// (shipped - claude) UNDERSTATE, and on a small run go NEGATIVE. The flush
	// -failure path above is left uncovered deliberately: when Flush fails the
	// counts are not trustworthy and its own error message says what happened.
	printRepoSummary(repoSummaries, []crossRepoSource{
		{Label: "codex-rollout", Enabled: *codexRollout, Events: codexTally.EventsShipped()},
		{Label: "opencode", Enabled: *opencodeFlag, Events: opencodeTally.EventsShipped()},
		{Label: "muse", Enabled: *museFlag, Events: museTally.EventsShipped()},
	})

	if codexErr != nil {
		// Some Codex spend may have shipped before this fired; say so, or an
		// operator reading a non-zero exit assumes nothing landed and the
		// partial-ship posture above becomes invisible. The prefix is stripped
		// here rather than in shipCodexRollout so the error chain stays intact
		// for callers: codexrollout's own errors already say "codex-rollout: ".
		fmt.Fprintf(os.Stderr, "ship codex-rollout: %s\n(%d events did ship and are safe to re-ship)\n",
			logsafe.Str(strings.TrimPrefix(codexErr.Error(), "codex-rollout: ")), client.Shipped())
		os.Exit(1)
	}

	if opencodeErr != nil {
		// Same posture as the Codex arm above: some Opencode spend may have
		// shipped before this fired, so say so — an operator reading a non-zero
		// exit otherwise assumes nothing landed.
		// logsafe-wrapped: the paths inside these errors are already wrapped at
		// construction, but the DRIVER's text is not, and it embeds strings taken
		// from a third-party database.
		fmt.Fprintf(os.Stderr, "ship opencode: %s\n(%d events did ship and are safe to re-ship)\n",
			logsafe.Str(strings.TrimPrefix(opencodeErr.Error(), "opencode: ")), client.Shipped())
		os.Exit(1)
	}

	if museErr != nil {
		// Same posture as the Codex and Opencode arms above.
		fmt.Fprintf(os.Stderr, "ship muse: %s\n(%d events did ship and are safe to re-ship)\n",
			logsafe.Str(strings.TrimPrefix(museErr.Error(), "muse: ")), client.Shipped())
		os.Exit(1)
	}

	if repoFailed {
		fmt.Fprintf(os.Stderr, "ship: some repos failed\n(%d events did ship and are safe to re-ship)\n", client.Shipped())
		os.Exit(1)
	}

	if shipRecoveredNothing(repoSummaries, client.Shipped()) && !*allowEmpty {
		// #549 arm 3. This is the false-green this issue exists to close: a
		// --repo pointing at the wrong checkout previously logged kept=0 at
		// INFO and exited 0, so "ship reported success" and "ship recovered
		// nothing" became indistinguishable without diffing the store by
		// hand. A --repo that legitimately has no Claude Code history yet is
		// real but rare enough that failing closed and naming the escape
		// hatch is the right default.
		fmt.Fprintf(os.Stderr, "ship: every --repo target kept 0 sessions since %s — this is almost always a wrong --repo path, not a legitimately idle repo. Pass --allow-empty if zero is genuinely expected.\n",
			since.Format("2006-01-02"))
		os.Exit(1)
	}

	// #549 arm 4: silence is what let the --codex-rollout omission recur
	// during the 2026-07-30 dogfood backfill — "Shipped 132290 events"
	// printed and exited 0 while zero Codex rows landed, because nothing in
	// the completion line said Codex was never scanned. Named on EVERY
	// completion line while the flag is off, success or empty alike, not
	// just when Shipped()==0 — the omission is invisible precisely on a run
	// that otherwise looks completely healthy.
	//
	// The SAME reasoning applies to every optional local source, so Opencode gets
	// its own clause (#719) rather than being silently absent from a line that
	// already exists to name what was skipped.
	//
	// Each source keeps its OWN "<Source> NOT included: pass --<flag>" phrasing
	// rather than being folded into one shared list. That is deliberate: the
	// wording is the thing under test (ship_test.go pins the Codex string
	// literally, because #549 arm 4 is about what the operator READS), and a
	// merged phrasing would silently rewrite an assertion whose whole purpose is
	// to be brittle.
	omissionNote := ""
	if !*codexRollout {
		omissionNote += " (Codex NOT included: pass --codex-rollout)"
	}
	if !*opencodeFlag {
		omissionNote += " (Opencode NOT included: pass --opencode)"
	}
	if !*museFlag {
		omissionNote += " (Muse NOT included: pass --muse)"
	}
	if client.Shipped() == 0 {
		fmt.Printf("No events shipped for the given repo(s) since %s%s.\n", since.Format("2006-01-02"), omissionNote)
		return
	}
	fmt.Printf("Shipped %d events to %s (since %s)%s. Re-running is safe: the server dedups on idempotency keys.\n",
		client.Shipped(), *server, since.Format("2006-01-02"), omissionNote)
}

// parseRepoSlugPairs parses repeatable `--repo-slug <path>=<owner/repo>` pairs (#231).
// Keyed by the raw path as typed AND cleaned, so the caller can look up by either the
// operator's spelling or the resolved absolute path.
//
// A malformed pair is a hard error rather than a warning: an operator reaching for
// this flag is fixing an attribution bug (usually a fork), and silently ignoring the
// value would leave them with the exact wrong-repo attribution they came to fix.
func parseRepoSlugPairs(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		path, slug, ok := strings.Cut(p, "=")
		if !ok || path == "" || slug == "" {
			return nil, fmt.Errorf("expected <path>=<owner/repo>, got %q", p)
		}
		canon, ok := repoid.Canonical(slug)
		if !ok {
			return nil, fmt.Errorf("%q is not a canonical owner/repo slug", slug)
		}
		key := filepath.Clean(path)
		if prev, dup := out[key]; dup && prev != canon {
			return nil, fmt.Errorf("repo path %q is mapped to both %q and %q — resolve the ambiguity rather than letting one silently win", key, prev, canon)
		}
		out[key] = canon
	}
	return out, nil
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
