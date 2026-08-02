package main

// tierd backfill (#237): reconstruct outcomes from merged-PR history for a day-0
// cold start or post-data-loss recovery.
//
// THE PROBLEM IT SOLVES: outcomes (the TIER numerator) are born ONLY from live
// GitHub webhook deliveries or live API posts. Costs, by contrast, already backfill
// 90 days from local JSONL (`tierd ship`/`tierd score`). So a brand-new adopter — or
// anyone restoring a lost DB — has a full denominator and an empty numerator: large
// spend, TIER near zero, and a zero-outcome tripwire firing on a webhook that is
// working perfectly. This command walks the repo's merged-PR history via the GitHub
// REST API and reconstructs one outcome per merged PR, reusing the SAME derivation
// the webhook applies so backfilled rows are indistinguishable in provenance except
// source='backfill'.
//
// IDEMPOTENCY IS MANDATORY (issue #237). Every reconstructed outcome routes through
// store.InsertOutcome, whose ON CONFLICT DO NOTHING on the merge_commit_sha unique
// index (idx_outcomes_merge_commit_sha_uq) makes a re-run — or an overlap with the
// live webhook, which keys on the same SHA — a silent no-op. Re-running is always
// safe and never double-counts. A merged PR always carries a merge_commit_sha, so
// every backfilled row participates in that dedup.
//
// SCOPE (deliberately bounded to the numerator gap). This reconstructs OUTCOMES
// only. It does NOT backfill quality DEGRADATION signals:
//   - CI failures (workflow_run) do not backfill: a historical CI conclusion is not
//     reliably re-derivable, and the 48h observation window has long since closed.
//   - Reverts are not reconstructed here either: doing so needs a commit-search pass
//     and the append→resolve quality path, and is left for a follow-up. A revert that
//     lands AFTER the backfill still degrades the reconstructed outcome via the live
//     webhook's issue-id tier, because the outcome row now exists to be found.
// Both mean a backfilled outcome starts at quality 1.0 — the same honest baseline a
// freshly-merged PR takes before any degradation signal arrives.
//
// The GitHub token is required and read via the shared @file indirection (#37), so it
// never sits on the command line. Client-controlled strings (PR titles, bodies, branch
// names, logins) are NEVER logged — only PR numbers and counts — to avoid log injection
// from an attacker-authored PR body.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/prderive"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// outcomeSourceBackfill is the provenance stamped on every reconstructed outcome
// (#237). It is a deliberately new value alongside store.OutcomeSource{GitHubWebhook,
// API,Push}: the `source` column is free-text with no CHECK constraint, so a report
// that segments on provenance can distinguish a reconstructed row from a live one
// while scoring treats the two identically. Defined here (not in internal/store)
// because this wave's scope fence forbids modifying the store package.
const outcomeSourceBackfill = "backfill"

// outcomeInserter is the store subset backfill needs — the same InsertOutcome the
// webhook relies on, whose ON CONFLICT DO NOTHING on merge_commit_sha is the
// idempotency boundary. A narrow consumer-side interface keeps the reconstruction
// loop unit-testable with a fake and honors "accept interfaces".
type outcomeInserter interface {
	InsertOutcome(ctx context.Context, o store.Outcome) (inserted bool, err error)
}

// runBackfillCmd parses flags, resolves the token, opens the store, and runs the
// reconstruction, returning the process exit code (0 clean, 1 fatal) so main's
// os.Exit stays the single exit point and the deferred db.Close always runs. Output
// goes to the injected writers so the whole subcommand is testable through dispatch.
func runBackfillCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoFlag := fs.String("repo", "", `GitHub repository as "owner/name" (required)`)
	sinceStr := fs.String("since", "", "reconstruct merged PRs on or after this date, e.g. 2026-01-01 (default: 90 days ago)")
	// Default is EMPTY on purpose (#237 review R1): flag.PrintDefaults echoes a
	// non-empty string default into `-h`/parse-error usage output, so seeding this
	// with os.Getenv(TIER_GITHUB_TOKEN) would print a live token to stderr. The env
	// fallback is applied AFTER Parse instead, keeping the token off every surface.
	token := fs.String("token", "", "GitHub API token. Prefer TIER_GITHUB_TOKEN env var or @/path/to/file (read from disk); a literal value here leaks via ps/shell history (#37)")
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path to write reconstructed outcomes into")
	apiURL := fs.String("github-api-url", "https://api.github.com", "GitHub REST API base URL (override for GitHub Enterprise)")
	// Backfill reads ONLY outcomes.size_labels from the config (#301): an org that
	// remaps its size labels (#244) must score backfilled PRs on the SAME custom
	// weights the live webhook applies, or historical and live outcomes for the same
	// PR would diverge. Absent/empty → the built-in default table, exactly as serve.
	configPath := fs.String("config", "", `path to the YAML config file (#29); backfill reads outcomes.size_labels so custom size-label weights match the live webhook (#301)`)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if *repoFlag == "" {
		_, _ = fmt.Fprintln(stderr, "backfill: --repo is required (the GitHub \"owner/name\" to reconstruct outcomes for)")
		return 1
	}
	repo, ok := repoid.Canonical(*repoFlag)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "backfill: --repo %q is not a canonical owner/name slug\n", *repoFlag)
		return 1
	}
	// Refuse to send the Bearer token over plaintext (#237 review Y2): the base URL
	// is operator-overridable, and an http:// endpoint would leak the GitHub token on
	// the wire. Loopback is exempt so a local test/proxy (httptest, a localhost GHE
	// shim) still works.
	baseURL, err := validateAPIBaseURL(*apiURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill: --github-api-url: %v\n", err)
		return 1
	}
	since, err := parseSince(*sinceStr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill: invalid --since value: %v\n", err)
		return 1
	}
	// Env fallback applied here (not as the flag default) so the token never reaches
	// usage output (#237 review R1). CLI flag wins when set; otherwise TIER_GITHUB_TOKEN.
	tokRaw := *token
	if tokRaw == "" {
		tokRaw = os.Getenv("TIER_GITHUB_TOKEN")
	}
	// Resolve @file indirection so the token never sits on the command line (#37).
	// Fail loud on an empty token: the GitHub API allows only 60 unauthenticated
	// requests/hour and cannot read private repos, so a tokenless backfill would
	// silently under-recover — exactly the failure this command exists to prevent.
	tok, err := resolveSecretFlag(tokRaw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill: --token: %v\n", err)
		return 1
	}
	if tok == "" {
		_, _ = fmt.Fprintln(stderr, "backfill: --token is required (set TIER_GITHUB_TOKEN or pass @/path/to/token)")
		return 1
	}

	// Resolve the operator's size-label table BEFORE opening the DB so a bad config
	// fails loud with no resource held (#301). config.Load has already validated the
	// weights are on the fixed scale and that no two names collide once folded;
	// NormalizeSizeLabels folds the keys and returns nil for an absent/`{}` table so
	// SizeWeight falls back to the built-in defaults — the same absent/`{}`/custom
	// semantics serve applies via webhook.WithSizeLabels.
	var sizeLabels map[string]float64
	if *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "backfill: config: %v\n", err)
			return 1
		}
		sizeLabels = prderive.NormalizeSizeLabels(cfg.Outcomes.SizeLabels)
	} else {
		// No config supplied: size-label weights fall back to the built-in default
		// table. Warn loudly (backfill is a one-shot admin command, so this is a
		// single line, not log spam) because an operator whose serve runs with a
		// custom outcomes.size_labels (#244) but backfills WITHOUT the same --config
		// would silently weight the same PR differently on live vs. backfilled rows —
		// exactly the divergence #301 exists to prevent. Harmless for the common case
		// (no custom table), decisive for the org that has one.
		slog.Default().Warn("backfill: no --config supplied; using built-in default size-label weights. " +
			"If serve runs with a custom outcomes.size_labels (#244), pass the SAME --config here so " +
			"backfilled and live outcomes weight identically (#301)")
	}

	logger := slog.Default()

	if err := os.MkdirAll(dbDir(*dbPath), 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill: create db dir: %v\n", err)
		return 1
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill: open db: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	// Cancel cleanly on SIGINT/SIGTERM so a long backfill (many API pages) stops
	// promptly, matching score/ship.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	gh := &githubClient{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: baseURL,
		token:   tok,
		logger:  logger,
	}

	stats, err := backfillOutcomes(ctx, gh, db, repo, since, sizeLabels)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout,
		"backfill complete: repo=%s since=%s merged=%d inserted=%d skipped=%d unattributed=%d\n",
		repo, since.Format("2006-01-02"), stats.merged, stats.inserted, stats.skipped, stats.unattributed)
	return 0
}

// validateAPIBaseURL normalizes and safety-checks the GitHub API base URL (#237
// review Y2). It strips a trailing slash and requires https, except for a loopback
// host (127.0.0.0/8, ::1, localhost) where http is allowed so a local test server or
// on-box GHE shim works without shipping the Bearer token over the network.
func validateAPIBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(raw, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("must be an http(s) URL, got %q", raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("refusing to send the API token over plaintext http to a non-loopback host %q; use https", u.Hostname())
	}
	return trimmed, nil
}

// isLoopbackHost reports whether host is a loopback address or "localhost", so the
// https requirement can be relaxed for on-box endpoints only.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// dbDir returns the directory component of a db path, or "." when there is none,
// so os.MkdirAll never receives an empty string (mirrors runServe's MkdirAll).
func dbDir(path string) string {
	if i := strings.LastIndexByte(path, os.PathSeparator); i >= 0 {
		return path[:i]
	}
	return "."
}

// backfillStats is the reconstruction tally reported to the operator.
type backfillStats struct {
	merged       int // merged PRs within the window that were examined
	inserted     int // fresh outcome rows written
	skipped      int // ON CONFLICT DO NOTHING no-ops (already present: replay/overlap)
	unattributed int // merged PRs with no resolvable issue id (no outcome, logged)
}

// backfillOutcomes pages the repo's closed PRs newest-updated first, reconstructs an
// outcome for each merged PR whose merge landed on/after since, and inserts it
// idempotently. Pagination stops as soon as a PR's updated_at precedes since: the
// list is sorted updated-descending and a PR's merged_at never exceeds its
// updated_at, so no later page can hold an in-window merge — this bounds the API
// calls to the window rather than the repo's whole history.
//
// A concurrent mutation can, in principle, shift a PR across a page boundary under
// sort=updated and cause it to be missed; the idempotent re-run is the mitigation
// (re-running never double-counts and picks up any straggler).
// sizeLabels is the operator's normalized outcomes.size_labels table (#301), or nil
// to use the built-in defaults; it is threaded to reconstructOutcome so backfilled
// weights match the live webhook for custom-label orgs.
func backfillOutcomes(ctx context.Context, gh *githubClient, ins outcomeInserter, repo string, since time.Time, sizeLabels map[string]float64) (backfillStats, error) {
	var stats backfillStats
	for page := 1; ; page++ {
		if page > ghMaxPages {
			gh.logger.Warn("backfill: hit the pagination ceiling; stopping scan early",
				"max_pages", ghMaxPages)
			break
		}
		items, err := gh.listClosedPRs(ctx, repo, page)
		if err != nil {
			return stats, fmt.Errorf("list closed PRs (page %d): %w", page, err)
		}
		if len(items) == 0 {
			break
		}
		reachedWindowEnd := false
		for _, it := range items {
			updated, uerr := parseGitHubTime(it.UpdatedAt)
			// A PR updated before `since` — and every one after it in this
			// updated-descending list — cannot have merged in-window. Stop.
			if uerr == nil && updated.Before(since) {
				reachedWindowEnd = true
				break
			}
			// merged_at null = closed-unmerged; skip.
			if it.MergedAt == nil {
				continue
			}
			mergedAt, merr := parseGitHubTime(*it.MergedAt)
			if merr != nil {
				// A merged PR whose timestamp will not parse is surfaced, not
				// silently dropped (fail-loud ethos, #237 review G1). Log the PR
				// NUMBER only — the timestamp is client data, never logged.
				gh.logger.Warn("backfill: merged PR has an unparseable merged_at, skipping", "pr", it.Number)
				continue
			}
			// merged_at before `since` = out of window (merged earlier but updated
			// recently, e.g. a late comment) — skip without stopping the scan.
			if mergedAt.Before(since) {
				continue
			}

			// A merged, in-window PR: count it, then partition by reconstruction
			// result so merged == inserted + skipped + unattributed always holds.
			stats.merged++
			inserted, unattributed, err := gh.reconstructOutcome(ctx, ins, repo, it.Number, sizeLabels)
			if err != nil {
				return stats, fmt.Errorf("reconstruct PR #%d: %w", it.Number, err)
			}
			switch {
			case unattributed:
				stats.unattributed++
			case inserted:
				stats.inserted++
			default:
				stats.skipped++
			}
		}
		if reachedWindowEnd || len(items) < ghPerPage {
			break
		}
	}
	return stats, nil
}

// reconstructOutcome fetches PR #number's detail and, if it is a merged PR with a
// resolvable issue id, derives and idempotently inserts one outcome. It applies the
// webhook's handlePR derivation exactly — same issueref primary-issue rule and the
// SAME shared prderive helpers for the size-label weight (with git-heuristic
// fallback) and work-type precedence (#301) — differing only in source='backfill'.
// sizeLabels is the operator's normalized size-label table (or nil for the built-in
// defaults), so a custom-label org scores backfilled PRs the same way the live
// webhook does (#244/#301). Returns (inserted, unattributed): an unattributed PR (no
// issue id) is not an error — it is logged (PR number only) and produces no outcome,
// mirroring the webhook's Debug skip.
func (gh *githubClient) reconstructOutcome(ctx context.Context, ins outcomeInserter, repo string, number int, sizeLabels map[string]float64) (inserted, unattributed bool, err error) {
	pr, err := gh.getPR(ctx, repo, number)
	if err != nil {
		return false, false, err
	}
	// Guard against a list/detail race: only reconstruct genuinely-merged PRs.
	if pr.MergedAt == nil || pr.MergeCommitSHA == "" {
		return false, false, nil
	}
	mergedAt, err := parseGitHubTime(*pr.MergedAt)
	if err != nil {
		return false, false, fmt.Errorf("parse merged_at: %w", err)
	}

	issueID := issueref.FromBranchOrBody(pr.Head.Ref, pr.Body)
	if issueID == "" {
		// No resolvable issue id — cannot attribute. Log the PR NUMBER only (an int,
		// not attacker-controlled); never the title/body/branch, which are.
		gh.logger.Debug("backfill: merged PR has no derivable issue id, skipping", "pr", number)
		return false, true, nil
	}
	// Multi-issue attribution parity with the webhook (#189): a PR that closes
	// several issues is attributed to the single primary (issueID); log the set so
	// the un-credited issues are observable, not silent. Issue ids are "issue-<n>",
	// not free text, so they are safe to log.
	if closed := issueref.ClosedIssues(pr.Body); len(closed) > 1 {
		gh.logger.Info("backfill: PR closes multiple issues; attributed to primary only (#189)",
			"pr", number, "primary", issueID, "closed", closed)
	}

	labels := labelNames(pr.Labels)
	weight, weightSource := store.ResolveWeight(prderive.SizeWeight(labels, sizeLabels),
		pr.Additions, pr.Deletions, pr.ChangedFiles)
	workType, workTypeSource := prderive.WorkTypeFromLabels(labels)

	o := store.Outcome{
		Developer:      pr.User.Login,
		IssueID:        issueID,
		PRNumber:       pr.Number,
		Weight:         weight,
		WeightSource:   weightSource,
		Quality:        1.0,
		MergeCommitSHA: pr.MergeCommitSHA,
		Additions:      pr.Additions,
		Deletions:      pr.Deletions,
		ChangedFiles:   pr.ChangedFiles,
		WorkType:       workType,
		WorkTypeSource: workTypeSource,
		Source:         outcomeSourceBackfill,
		Repo:           repo, // already canonical (repoid.Canonical in runBackfillCmd)
		Timestamp:      mergedAt,
	}
	ins2, err := ins.InsertOutcome(ctx, o)
	if err != nil {
		return false, false, err
	}
	return ins2, false, nil
}

// --- GitHub REST client ---

const (
	ghPerPage    = 100
	ghAPIVersion = "2022-11-28"
	// ghMaxRetries bounds retries of genuine ERRORS (transport failures, 5xx) with
	// exponential backoff capped at ghMaxRetryWait. Rate-limit waits are counted
	// SEPARATELY (ghMaxRateLimitWaits) so riding out a legitimate limit does not
	// consume the error budget (#237 review Y1).
	ghMaxRetries    = 6
	ghMaxRetryWait  = 5 * time.Minute
	ghBaseRetryWait = time.Second
	// ghMaxRateLimitWaits bounds how many times do will wait out a rate limit before
	// giving up (#237 review Y1). Each wait clears a full primary-limit window, so a
	// handful covers even a large history; the idempotent re-run resumes if exceeded.
	ghMaxRateLimitWaits = 4
	// ghMaxRateLimitWait caps a single rate-limit wait. GitHub's PRIMARY limit resets
	// up to ~1h out, so this is larger than ghMaxRetryWait — it must actually ride out
	// a real reset — while still bounding a malicious far-future X-RateLimit-Reset.
	ghMaxRateLimitWait = 65 * time.Minute
	// ghSecondaryRateLimitFloor is the wait used for a rate-limit response that
	// carries NO usable header (#237 review G2). GitHub's abuse/secondary-limit
	// guidance is to back off >= 60s; hammering at 1s risks escalating the throttle.
	ghSecondaryRateLimitFloor = 60 * time.Second
	// ghMaxResponseBytes caps one API response body (#237 review Y1). A PR list page
	// or a single PR detail is well under this; the cap only stops a hostile or
	// misconfigured base URL from driving the process to OOM with an unbounded body.
	ghMaxResponseBytes = 16 << 20 // 16 MiB
	// ghMaxPages is an absolute pagination ceiling (#237 review Y3). Real GitHub
	// terminates the scan via the updated-desc `since` short-circuit; this is pure
	// insurance against a sort-contract change or a hostile endpoint that always
	// returns a full page, which would otherwise loop forever firing detail fetches.
	ghMaxPages = 10_000
)

// githubClient is the minimal authenticated GitHub REST client backfill needs. It
// exists rather than a dependency because the hard constraint is zero new deps
// (tasks/README.md): net/http + encoding/json cover the two endpoints used.
type githubClient struct {
	http    *http.Client
	baseURL string
	token   string
	logger  *slog.Logger
}

type ghListItem struct {
	Number    int     `json:"number"`
	UpdatedAt string  `json:"updated_at"`
	MergedAt  *string `json:"merged_at"` // null for closed-but-unmerged PRs
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghPR struct {
	Number         int     `json:"number"`
	Body           string  `json:"body"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Head           struct {
		Ref string `json:"ref"`
	} `json:"head"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels       []ghLabel `json:"labels"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
}

// listClosedPRs fetches one page of closed PRs sorted updated-descending. Diff stats
// (additions/deletions/changed_files) are intentionally absent from the list payload —
// GitHub serves them only on the per-PR detail endpoint — so only the fields needed
// for windowing (number, updated_at, merged_at) are decoded here.
func (gh *githubClient) listClosedPRs(ctx context.Context, repo string, page int) ([]ghListItem, error) {
	q := url.Values{}
	q.Set("state", "closed")
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	q.Set("per_page", strconv.Itoa(ghPerPage))
	q.Set("page", strconv.Itoa(page))
	body, err := gh.do(ctx, "/repos/"+repo+"/pulls?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var items []ghListItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("decode PR list: %w", err)
	}
	return items, nil
}

// getPR fetches one PR's full detail (including the diff stats the list omits).
func (gh *githubClient) getPR(ctx context.Context, repo string, number int) (ghPR, error) {
	body, err := gh.do(ctx, "/repos/"+repo+"/pulls/"+strconv.Itoa(number))
	if err != nil {
		return ghPR{}, err
	}
	var pr ghPR
	if err := json.Unmarshal(body, &pr); err != nil {
		return ghPR{}, fmt.Errorf("decode PR detail: %w", err)
	}
	return pr, nil
}

// do performs one authenticated GET against path (which begins with "/") and returns
// the response body, transparently waiting out GitHub's rate limits. It handles:
//   - 403/429 with X-RateLimit-Remaining: 0 → wait until X-RateLimit-Reset,
//   - 403/429 with Retry-After (secondary/abuse limit) → wait that long,
//   - 5xx / transport error → exponential backoff.
//
// Error retries (transport/5xx) are bounded by ghMaxRetries; rate-limit waits are
// counted SEPARATELY (ghMaxRateLimitWaits) so riding out a legitimate limit does not
// exhaust the error budget (#237 review Y1). A 4xx other than a rate limit is a
// permanent error (bad repo, bad token) and is returned immediately.
func (gh *githubClient) do(ctx context.Context, path string) ([]byte, error) {
	var lastErr error
	errAttempts := 0 // transport/5xx retries used
	rlWaits := 0     // rate-limit waits used

	// backoffOrGiveUp records err, then either aborts (budget exhausted) or backs off
	// for the next attempt. It returns (abortErr, true) to stop, or (nil, false) to retry.
	backoffOrGiveUp := func(err error) (error, bool) {
		lastErr = err
		errAttempts++
		if errAttempts >= ghMaxRetries {
			return fmt.Errorf("GitHub API GET %s: giving up after %d attempts: %w", path, errAttempts, lastErr), true
		}
		if werr := gh.waitBackoff(ctx, errAttempts-1); werr != nil {
			return werr, true
		}
		return nil, false
	}

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gh.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+gh.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", ghAPIVersion)
		req.Header.Set("User-Agent", "tierd-backfill")

		resp, err := gh.http.Do(req)
		if err != nil {
			// Transient transport error (DNS blip, reset): back off and retry.
			if abort, stop := backoffOrGiveUp(err); stop {
				return nil, abort
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, ghMaxResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			if abort, stop := backoffOrGiveUp(readErr); stop {
				return nil, abort
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case isRateLimited(resp):
			rlWaits++
			if rlWaits > ghMaxRateLimitWaits {
				return nil, fmt.Errorf("GitHub API GET %s: still rate limited after %d waits (status %d)", path, ghMaxRateLimitWaits, resp.StatusCode)
			}
			wait := rateLimitWait(resp, time.Now())
			gh.logger.Warn("backfill: GitHub rate limit hit, waiting", "wait", wait.String(), "wait_num", rlWaits)
			if werr := gh.waitFor(ctx, wait); werr != nil {
				return nil, werr
			}
			continue
		case resp.StatusCode >= 500:
			if abort, stop := backoffOrGiveUp(fmt.Errorf("server error: status %d", resp.StatusCode)); stop {
				return nil, abort
			}
			continue
		default:
			// Permanent 4xx (404 repo/token, 401 auth, 422): do not retry. The
			// GitHub error body can echo the request; return status only, never the
			// body, so nothing attacker-influenced reaches a log.
			return nil, fmt.Errorf("GitHub API GET %s: unexpected status %d", path, resp.StatusCode)
		}
	}
}

// isRateLimited reports whether resp is a GitHub rate-limit rejection — a 429, or a
// 403 with the primary-limit remaining counter at zero or a Retry-After present.
func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode == http.StatusForbidden {
		if resp.Header.Get("Retry-After") != "" {
			return true
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return true
		}
	}
	return false
}

// rateLimitWait computes how long to wait before retrying a rate-limited response,
// preferring Retry-After (seconds) then X-RateLimit-Reset (unix seconds). When a
// header is present but resolves to a non-positive delay (the reset already passed),
// it retries promptly (1s). When NO usable header is present at all, it uses the
// secondary-limit floor (#237 review G2) rather than hammering. The result is clamped
// to [1s, ghMaxRateLimitWait] so a malformed or far-future header can neither
// busy-loop nor hang the process past a real primary-limit reset (#237 review Y1).
func rateLimitWait(resp *http.Response, now time.Time) time.Duration {
	var wait time.Duration
	hadHeader := false
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
			hadHeader = true
		}
	}
	if wait <= 0 {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if epoch, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); err == nil {
				// +1s cushion past the reset instant. Measured against the injected
				// `now` so the wait is deterministic under test.
				wait = time.Unix(epoch, 0).Add(time.Second).Sub(now)
				hadHeader = true
			}
		}
	}
	if wait <= 0 {
		if hadHeader {
			// A header said the limit already reset — retry promptly rather than
			// idling a full minute.
			wait = time.Second
		} else {
			// No signal at all: back off politely (GitHub abuse-limit guidance).
			wait = ghSecondaryRateLimitFloor
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	if wait > ghMaxRateLimitWait {
		wait = ghMaxRateLimitWait
	}
	return wait
}

// waitBackoff sleeps for an exponentially growing delay (capped) before the next
// attempt, aborting early if ctx is cancelled.
func (gh *githubClient) waitBackoff(ctx context.Context, attempt int) error {
	wait := ghBaseRetryWait << attempt
	if wait > ghMaxRetryWait || wait <= 0 {
		wait = ghMaxRetryWait
	}
	return gh.waitFor(ctx, wait)
}

// waitFor blocks for d but returns ctx.Err() immediately if the context is cancelled
// first, so SIGINT during a long rate-limit wait stops the command promptly. A timer
// (not a sleeping goroutine) keeps the wait cancellable with nothing left running.
func (gh *githubClient) waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseGitHubTime parses a GitHub RFC3339 timestamp, normalized to UTC so all window
// comparisons are instant-safe (the store compares DATETIME as offset-bearing strings).
func parseGitHubTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// labelNames flattens the GitHub label objects to their names.
func labelNames(labels []ghLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// The size-label→weight and work-type derivation lives in internal/prderive (#301),
// the single source of truth BOTH this backfill path and the live webhook consume, so
// a PR scores identically however it was ingested. Backfill previously carried
// verbatim copies (backfillLabelWeight / backfillWorkType) that could silently drift
// from the webhook originals and — after #244 made the size table config-driven —
// ignored the operator's custom outcomes.size_labels entirely; both divergences are
// closed now that reconstructOutcome calls prderive.SizeWeight / prderive.WorkTypeFromLabels
// with the threaded config table.
