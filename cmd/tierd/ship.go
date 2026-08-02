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
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/shipper"
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

func runShip(args []string) {
	fs := flag.NewFlagSet("ship", flag.ExitOnError)
	server := fs.String("server", "", "central tierd base URL, e.g. https://tier.example (required)")
	apiToken := fs.String("api-token", os.Getenv("TIER_API_TOKEN"), "API token sent as Authorization: Bearer. Prefer TIER_API_TOKEN env var or @/path/to/file; a literal value here leaks via ps/shell history (#37)")
	var repos repeatableStringSlice
	fs.Var(&repos, "repo", `git repo path whose Claude Code sessions to ship (repeatable; default ".")`)
	var repoSlugs repeatableStringSlice
	fs.Var(&repoSlugs, "repo-slug", `canonical repository identity for one --repo, as "<path>=<owner/repo>" (repeatable; #231). Omit to read remote.origin.url. REQUIRED ON A FORK: origin names the fork, while the upstream webhook records outcomes against the upstream, so without this your cost never joins your outcomes`)
	claudeDir := fs.String("claude-dir", "", "override ~/.claude directory (for testing)")
	sinceStr := fs.String("since", "", "start date, e.g. 2026-01-01 (default: 90 days ago). Over-shipping is safe: the server dedups on idempotency keys, so a wide window costs nothing")
	developer := fs.String("developer", "", "developer ID override (default: OS username)")
	codexRollout := fs.Bool("codex-rollout", envBool("TIER_CODEX_ROLLOUT"), "also ship Codex CLI spend from the local rollout logs at ~/.codex/sessions/**/rollout-*.jsonl (#464). The rollout logs are the only path that captures Codex — Codex speaks the OpenAI Responses API, which the reverse proxy's Chat Completions parser cannot read (#463). Attributes to the same --repo set as the Claude Code scan. OFF by default, mirroring `tierd serve --codex-rollout`. Env TIER_CODEX_ROLLOUT")
	codexSessionsDir := fs.String("codex-sessions-dir", "", "override ~/.codex/sessions directory (for testing)")
	_ = fs.Parse(args)

	if *server == "" {
		fmt.Fprintln(os.Stderr, "--server is required (the central tierd base URL)")
		os.Exit(1)
	}
	// Resolve @file indirection so the token never sits on the command line
	// (#37) — same contract as tierd serve.
	token, err := resolveSecretFlag(*apiToken)
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

	codexTargets := make([]codexrollout.RepoTarget, 0, len(repos))
	for _, repo := range repos {
		repoPath, err := resolveRepo(repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve repo %q: %v\n", repo, err)
			os.Exit(1)
		}
		// Match on both the operator-typed path and its resolved form, so
		// `--repo . --repo-slug .=owner/repo` works as written.
		slug := firstNonEmpty(slugFor[filepath.Clean(repo)], slugFor[repoPath])
		c := &collector.JSONLCollector{
			RepoPath:    repoPath,
			ClaudeDir:   *claudeDir,
			DeveloperID: *developer,
			RepoSlug:    slug,
		}
		if err := c.Run(ctx, since, client); err != nil {
			fmt.Fprintf(os.Stderr, "ship %s: %v\n", repoPath, err)
			os.Exit(1)
		}
		// Reuse the SAME resolved path and operator slug override the Claude
		// Code scan just used (#231), so a repo's Claude and Codex rows carry
		// one identity and join the same outcomes. A fork whose origin names
		// the fork would otherwise split its cost across two repo identities.
		codexTargets = append(codexTargets, codexrollout.RepoTarget{Path: repoPath, Slug: slug})
	}

	// Codex is scanned ONCE across every --repo rather than per repo: the
	// sessions root is a single flat, date-partitioned tree keyed by each
	// session's cwd, not a per-repo directory like ~/.claude/projects. Scanning
	// it per repo would re-walk and re-parse the whole tree N times to emit the
	// same events.
	var codexErr error
	if *codexRollout {
		codexErr = shipCodexRollout(ctx, codexTargets, *codexSessionsDir, *developer, since, client)
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
		fmt.Fprintf(os.Stderr, "ship: flush: %v\n", errors.Join(codexErr, err))
		os.Exit(1)
	}
	if codexErr != nil {
		// Some Codex spend may have shipped before this fired; say so, or an
		// operator reading a non-zero exit assumes nothing landed and the
		// partial-ship posture above becomes invisible. The prefix is stripped
		// here rather than in shipCodexRollout so the error chain stays intact
		// for callers: codexrollout's own errors already say "codex-rollout: ".
		fmt.Fprintf(os.Stderr, "ship codex-rollout: %s\n(%d events did ship and are safe to re-ship)\n",
			strings.TrimPrefix(codexErr.Error(), "codex-rollout: "), client.Shipped())
		os.Exit(1)
	}

	if client.Shipped() == 0 {
		scanned := "Claude Code sessions"
		if *codexRollout {
			scanned = "Claude Code or Codex sessions"
		}
		fmt.Printf("No %s found for the given repo(s) since %s — nothing to ship.\n", scanned, since.Format("2006-01-02"))
		return
	}
	fmt.Printf("Shipped %d events to %s (since %s). Re-running is safe: the server dedups on idempotency keys.\n",
		client.Shipped(), *server, since.Format("2006-01-02"))
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
		out[filepath.Clean(path)] = canon
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
