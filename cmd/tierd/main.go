package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/anthropicadmin"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/collector/openaiusage"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/dashboard"
	"github.com/tiermetric/tier/internal/docs"
	"github.com/tiermetric/tier/internal/health"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/proxy"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	"github.com/tiermetric/tier/internal/webhook"
)

// version is the build version reported by /api/v1/livez. The Makefile injects
// the git description via -ldflags "-X main.version=..."; "dev" is the
// unstamped fallback for `go run` and bare `go build`.
var version = "dev"

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch routes a subcommand and returns the process exit code. It is split
// from main so the no-arg, version, and unknown-command paths are unit-testable
// without os.Exit / global os.Args / os.Stdout (#66). score and ship manage
// their own lifecycle and exit on their own errors, so they don't return here;
// serve, backfill, and backup return an exit code that dispatch propagates.
func dispatch(args []string, stdout, stderr io.Writer) int {
	// Diagnostic writes to the injected writers are best-effort; ignore the
	// (n, err) so errcheck/golangci-lint stays clean for the generic io.Writer
	// (errcheck only auto-excludes direct os.Stdout/os.Stderr writes).
	if len(args) < 1 {
		// No command is a usage error: usage to stderr, exit 1.
		printUsage(stderr)
		return 1
	}

	switch args[0] {
	case "score":
		runScore(args[1:])
	case "serve":
		return runServe(args[1:])
	case "ship":
		runShip(args[1:])
	case "backfill":
		return runBackfillCmd(args[1:], stdout, stderr)
	case "backup":
		return runBackup(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "reprice":
		return runRepriceCmd(args[1:], stdout, stderr)
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	// "healthcheck": probe a running tierd over HTTP and exit 0/1. Backs the
	// Dockerfile HEALTHCHECK — the runtime image has no shell and no HTTP
	// client, so tierd is the only thing available to call (#571).
	case "healthcheck":
		return runHealthcheck(args[1:], stdout, stderr)
	// "version", "--version", "-v": print the ldflags-injected build version so
	// operators can confirm exactly which binary is deployed (#66). Mirrors the
	// value /api/v1/livez reports, but available without starting the server.
	case "version", "--version", "-version", "-v":
		_, _ = fmt.Fprintln(stdout, versionString())
	// An explicit help request is success, not an error: usage to stdout,
	// exit 0. `tierd --help`/`-h`/`help` is the first thing a new user types
	// and must not look broken (#378).
	case "help", "--help", "-help", "-h":
		printUsage(stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		return 1
	}
	return 0
}

// printUsage writes the top-level command listing to w. It backs both the
// no-arg path (to stderr, exit 1) and an explicit help request (to stdout,
// exit 0), so the two callers stay in lockstep as subcommands are added (#378).
func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: tierd <command> [flags]")
	_, _ = fmt.Fprintln(w, "  score    compute TIER from local Claude Code JSONL files")
	_, _ = fmt.Fprintln(w, "  serve    run the TIER HTTP server (proxy + webhook + dashboard)")
	_, _ = fmt.Fprintln(w, "  ship     forward locally captured JSONL events to a central tierd (#126)")
	_, _ = fmt.Fprintln(w, "  backfill reconstruct outcomes from merged-PR history via the GitHub API (#237)")
	_, _ = fmt.Fprintln(w, "  backup   write a consistent snapshot of the database (VACUUM INTO)")
	_, _ = fmt.Fprintln(w, "  doctor   verify this install captures correctly (local + optional --server, #236)")
	_, _ = fmt.Fprintln(w, "  reprice  recompute historical costs under the current price table (audited, #294)")
	_, _ = fmt.Fprintln(w, "  demo     serve the dashboard on SYNTHETIC sample data (no setup, #383)")
	_, _ = fmt.Fprintln(w, "  healthcheck  probe a running tierd and exit 0/1 (backs the container HEALTHCHECK, #571)")
	_, _ = fmt.Fprintln(w, "  version  print the build version and exit")
	_, _ = fmt.Fprintln(w, "  help     print this help and exit")
}

// defaultListenAddr is the default bind address for `serve` and `demo`, and
// therefore the default target for `tierd healthcheck` (#571). It lives in one
// place because the container probe is only correct while it matches what the
// server actually binds: three independent copies of the same literal is a
// coupling that nothing enforces and that no test would catch drifting.
// Loopback by default — a non-loopback bind requires --api-token (#59).
const defaultListenAddr = "127.0.0.1:8080"

// versionString formats the build version with the Go toolchain and target
// platform, e.g. "tierd v0.3.1-2-gcfaf273 go1.26.4 darwin/arm64". version is
// "dev" for an unstamped `go build`/`go run`.
func versionString() string {
	return fmt.Sprintf("tierd %s %s %s/%s", resolveVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// codexWatchCheck decides what to do when Codex capture is requested (#479).
// --codex-rollout attributes to the SAME repos as --watch-repo, so enabling it
// with no watched repo is a silent no-op — a knob that does nothing. Returns a
// non-empty `fatal` message (abort startup) when codex is enabled with no repo
// and serve is NOT read-only; a non-empty `warn` when it is read-only (which
// deliberately disables all capture, so the mismatch is expected); both empty
// otherwise. watchRepoCount already merges the CLI flag and config watch.repos.
func codexWatchCheck(codexEnabled bool, watchRepoCount int, readOnly bool) (warn, fatal string) {
	if !codexEnabled || watchRepoCount > 0 {
		return "", ""
	}
	if readOnly {
		return "--codex-rollout is set but --read-only disables all capture; Codex spend will not be recorded", ""
	}
	return "", "--codex-rollout needs a repository to attribute Codex spend to, but no --watch-repo (or watch.repos config) is set. Add --watch-repo <path>, or drop --codex-rollout. (#464)"
}

// resolveVersion returns the ldflag-injected version when the Makefile stamped
// one, and otherwise falls back to the module version from the build info.
// `go install github.com/tiermetric/tier/cmd/tierd@v0.1.0` does not run the
// Makefile, so `version` stays "dev" — but the module version IS embedded by
// the toolchain, so a released install can still report "v0.1.0" instead of a
// bare "dev" (#477). A local `go run`/`go build` has neither and stays "dev".
func resolveVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

// runBackup writes a consistent snapshot of the database via store.Backup
// (VACUUM INTO, #141) and returns the process exit code. Split to return an int
// (like the version path) so it is unit-testable without os.Exit — the injected
// writers carry its output. --db defaults to defaultDBPath(); --out is required
// and must not already exist (store.Backup enforces both refusals). On success it
// prints one line ("backup written: <dest> (<n> bytes)") to stdout and returns 0;
// any error prints to stderr and returns 1.
func runBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path to back up")
	out := fs.String("out", "", "destination path for the snapshot (required; must not already exist)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *out == "" {
		_, _ = fmt.Fprintln(stderr, "backup: --out is required (destination path for the snapshot)")
		return 1
	}
	if err := store.Backup(context.Background(), *dbPath, *out); err != nil {
		_, _ = fmt.Fprintf(stderr, "backup: %v\n", err)
		return 1
	}
	// Report the snapshot size so an operator can eyeball that it is non-trivial.
	// A Stat failure here is not fatal — the backup succeeded — but is surfaced so
	// a surprising 0-byte or vanished file does not pass silently.
	var sizeNote string
	if fi, err := os.Stat(*out); err == nil {
		sizeNote = fmt.Sprintf(" (%d bytes)", fi.Size())
	}
	_, _ = fmt.Fprintf(stdout, "backup written: %s%s\n", *out, sizeNote)
	return 0
}

// loadPricesOverride applies a --prices YAML override when path is non-empty,
// failing the process loudly on a bad file (#68 — never a silent fallback to the
// embedded default). Returns the loaded table's metadata and true when an
// override was applied, or the zero value and false when path is empty (the
// embedded default, loaded at package init, stays active).
func loadPricesOverride(path string) (store.PriceTableInfo, bool) {
	if path == "" {
		return store.PriceTableInfo{}, false
	}
	info, err := store.LoadPriceTable(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--prices: %v\n", err)
		os.Exit(1)
	}
	return info, true
}

func runScore(args []string) {
	// NOTE(#185): the --aggregation team|developer gate does NOT apply to `tierd
	// score`, and that is a CONSCIOUS carve-out. `score` is a LOCAL, single-operator
	// CLI that reads the invoking user's own Claude Code JSONL and prints to their
	// own terminal — it is not a served, multi-viewer surface, so it needs no
	// k-anonymity floor or required-mode gate. Team-only aggregation is enforced on
	// `tierd serve` (GET /scores, the dashboard, GET /scores/{developer}), which are
	// the surfaces an organization actually consumes.
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the git repository")
	repoSlug := fs.String("repo-slug", "", `canonical "owner/repo" identity for --repo (#231). Omit to read remote.origin.url. REQUIRED ON A FORK: origin names the fork, while the upstream webhook records outcomes against the upstream, so without this your cost never joins your outcomes`)
	sinceStr := fs.String("since", "", "start date, e.g. 2026-01-01 (default: 90 days ago)")
	developer := fs.String("developer", "", "developer ID override (default: OS username)")
	claudeDir := fs.String("claude-dir", "", "override ~/.claude directory (for testing)")
	pricesPath := fs.String("prices", os.Getenv("TIER_PRICES"), "path to a price-table YAML override (#68); empty uses the embedded default. A bad file fails the command")
	_ = fs.Parse(args)

	// Apply a --prices override before Collect, which prices events via
	// ComputeCost. No override → the embedded default stays active. Note which
	// table priced the report to stderr (audit: "which prices produced these
	// numbers"), mirroring the structured log `tierd serve` emits.
	if info, ok := loadPricesOverride(*pricesPath); ok {
		fmt.Fprintf(os.Stderr, "price table: override %s (version %d, %s, %d models)\n",
			*pricesPath, info.Version, info.EffectiveDate, info.ModelCount)
	} else {
		info := store.ActivePriceTableInfo()
		fmt.Fprintf(os.Stderr, "price table: embedded default (version %d, %s, %d models)\n",
			info.Version, info.EffectiveDate, info.ModelCount)
	}

	since, err := parseSince(*sinceStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since value: %v\n", err)
		os.Exit(1)
	}

	repoPath, err := resolveRepo(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot resolve repo: %v\n", err)
		os.Exit(1)
	}

	c := &collector.JSONLCollector{
		RepoPath:    repoPath,
		ClaudeDir:   *claudeDir,
		DeveloperID: *developer,
		RepoSlug:    *repoSlug,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	events, err := c.Collect(ctx, since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "collect error: %v\n", err)
		os.Exit(1)
	}
	if len(events) == 0 {
		fmt.Println("No Claude Code sessions found for the given repo and time period.")
		fmt.Printf("Searched: ~/.claude/projects/ for sessions since %s\n", since.Format("2006-01-02"))
		fmt.Println("Make sure TIER is running in the correct git repository directory.")
		fmt.Println("Note: `tierd score` reads Claude Code sessions only — it does not capture Codex CLI spend. For Codex, run `tierd serve --watch-repo <path> --codex-rollout`, or `tierd ship --repo <path> --codex-rollout` to send it to a central tierd (#492).")
		return
	}

	// Without a live server we have no outcome data, so `tierd score` shows cost
	// attribution only — the "day-one value" mode: see where AI money is going
	// before the 14-day rework windows that full TIER scoring needs have elapsed.
	// Both reports re-aggregate straight from events; full per-developer TIER is
	// computed server-side (tierd serve), where outcomes and the actual_spend
	// ledger exist.

	// Print cost-only summary (outcomes not yet available in zero-setup mode).
	printCostReport(events, since)

	// Show issue-level cost breakdown.
	printIssueCosts(events)

	fmt.Printf("\nTip: run `tierd backfill` then `tierd serve` to record PR outcomes and compute full TIER scores. See /docs/quickstart.\n")
}

// printCostReport prints the per-developer cost summary.
func printCostReport(events []collector.TokenEvent, since time.Time) {
	type row struct {
		inputTok     int
		outputTok    int
		cacheRead    int
		cacheWrite5m int
		cacheWrite1h int
		costMicro    int64
	}
	byDev := map[string]*row{}
	for _, e := range events {
		if byDev[e.Developer] == nil {
			byDev[e.Developer] = &row{}
		}
		r := byDev[e.Developer]
		r.inputTok += e.InputTok
		r.outputTok += e.OutputTok
		r.cacheRead += e.CacheRead
		r.cacheWrite5m += e.CacheWrite5m
		r.cacheWrite1h += e.CacheWrite1h
		r.costMicro += e.CostMicro
	}

	fmt.Printf("\nTIER Cost Attribution — since %s\n", since.Format("2006-01-02"))
	fmt.Printf("Source: Claude Code JSONL (real-time, per-request)\n")
	fmt.Println("───────────────────────────────────────────────────────────────────────────────────────────")
	// Cache columns split (#55): Read, 5m Write, and 1h Write each carry a
	// different cost multiplier (Anthropic: 0.1× / 1.25× / 2.0× of input
	// rate), so summing them into a single "Cache" cell would be misleading.
	fmt.Printf("%-20s  %10s  %10s  %10s  %10s  %10s  %10s\n",
		"Developer", "Input tok", "Output tok", "Cache rd", "Cache w5m", "Cache w1h", "Cost ($)")
	fmt.Println("───────────────────────────────────────────────────────────────────────────────────────────")
	var totalCostMicro int64
	for dev, r := range byDev {
		fmt.Printf("%-20s  %10d  %10d  %10d  %10d  %10d  %10.4f\n",
			dev, r.inputTok, r.outputTok, r.cacheRead, r.cacheWrite5m, r.cacheWrite1h, store.MicroToDollars(r.costMicro))
		totalCostMicro += r.costMicro
	}
	fmt.Println("───────────────────────────────────────────────────────────────────────────────────────────")
	fmt.Printf("%-20s  %78.4f\n", "TOTAL", store.MicroToDollars(totalCostMicro))
}

// printIssueCosts prints per-issue cost breakdown.
func printIssueCosts(events []collector.TokenEvent) {
	type row struct {
		costMicro int64
		model     string
	}
	byIssue := map[string]*row{}
	for _, e := range events {
		if byIssue[e.IssueID] == nil {
			byIssue[e.IssueID] = &row{}
		}
		r := byIssue[e.IssueID]
		r.costMicro += e.CostMicro
		if r.model == "" {
			r.model = store.NormalizeModel(e.Model)
		}
	}

	// Deterministic order: by descending cost, ties broken by issue id. Replaces
	// the old map-iteration order and, with the labeled unattributed buckets
	// (#refocus, Option B), keeps the exploratory/detached-head/branch-without-issue
	// lines adjacent and stable instead of a single opaque "unattributed" row.
	issues := make([]string, 0, len(byIssue))
	for issue := range byIssue {
		issues = append(issues, issue)
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := byIssue[issues[i]], byIssue[issues[j]]
		if a.costMicro != b.costMicro {
			return a.costMicro > b.costMicro
		}
		return issues[i] < issues[j]
	})

	fmt.Println("\nCost by Issue")
	fmt.Println("──────────────────────────────────────────────────────────────────")
	fmt.Printf("%-34s  %-20s  %10s\n", "Issue", "Model", "Cost ($)")
	fmt.Println("──────────────────────────────────────────────────────────────────")
	for _, issue := range issues {
		r := byIssue[issue]
		fmt.Printf("%-34s  %-20s  %10.4f\n", issueLabel(issue), r.model, store.MicroToDollars(r.costMicro))
	}
}

// issueLabel renders an issue id for the cost-by-issue report, expanding the
// labeled unattributed buckets (#refocus, Option B) into a human-readable form so
// the report reads "exploratory (main, no issue)" instead of a bare
// "unattributed:main" sentinel. Real issue ids pass through unchanged.
func issueLabel(issue string) string {
	switch issue {
	case collector.UnattributedIssueID:
		return "unattributed (unlabeled)"
	case collector.UnattributedMain:
		return "unattributed: exploratory (main)"
	case collector.UnattributedDetachedHEAD:
		return "unattributed: detached HEAD"
	case collector.UnattributedNoIssue:
		return "unattributed: branch, no issue #"
	default:
		return issue
	}
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		// Return the default 90-day lower bound in UTC. time.Now() carries the
		// host's local zone; a non-UTC bound mis-windows any ts >= ? comparison
		// against UTC-stored rows because modernc.org/sqlite compares DATETIME
		// as offset-bearing strings (#180). This CLI's since currently feeds
		// only instant-safe paths (collector time.Before, gitLog's own
		// .UTC().Format), but normalizing here keeps the bound correct-from-start
		// for any future store-backed caller.
		return time.Now().AddDate(0, 0, -90).UTC(), nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, got %q", s)
}

func resolveRepo(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("path does not exist: %s", abs)
	}
	return abs, nil
}

// repeatableStringSlice satisfies flag.Value so a string flag can be passed
// multiple times on the command line, e.g. --watch-repo A --watch-repo B.
type repeatableStringSlice []string

func (s *repeatableStringSlice) String() string { return strings.Join(*s, ",") }
func (s *repeatableStringSlice) Set(v string) error {
	if v == "" {
		return nil
	}
	*s = append(*s, v)
	return nil
}

// buildWebhookOptions assembles the webhook.Handler construction options from
// the resolved server config. It is the single source of truth for the
// config→handler plumbing so both runServe and its test exercise the same wiring
// — the #240 regression (outcomes.generated_paths parsed but never plumbed) went
// unnoticed precisely because there was no seam to test.
//
//   - pushCapture (outcomes.push_capture, #196) toggles WithPushCapture.
//   - generatedPaths (outcomes.generated_paths, #240) is nil when the config key
//     is absent → the option is omitted so the handler keeps its built-in
//     defaultGeneratedPaths. A non-nil slice — including an explicit empty `[]`,
//     which disables all exclusion — is plumbed into WithGeneratedPaths. go-yaml
//     yields a non-nil empty slice for `[]` and nil for an absent key, so != nil
//     selects the override exactly when the operator supplied one.
//   - sizeLabels (outcomes.size_labels, #244) is nil when the config key is absent
//     → the option is omitted so the handler keeps its built-in defaultSizeLabels.
//     A non-nil map is plumbed into WithSizeLabels, which itself no-ops on an empty
//     map so an explicit `{}` preserves the defaults too — matching the documented
//     `{}`/absent = defaults, custom = replace semantics.
func buildWebhookOptions(pushCapture bool, unattributed webhook.PushUnattributedCounter, generatedPaths []string, sizeLabels map[string]float64) []webhook.Option {
	var opts []webhook.Option
	if pushCapture {
		opts = append(opts, webhook.WithPushCapture(unattributed))
	}
	if generatedPaths != nil {
		opts = append(opts, webhook.WithGeneratedPaths(generatedPaths))
	}
	if sizeLabels != nil {
		opts = append(opts, webhook.WithSizeLabels(sizeLabels))
	}
	return opts
}

// runServe runs the HTTP server subcommand and returns the process exit code
// (0 clean, 1 fatal), so main's os.Exit is the single exit point and the
// deferred db.Close always runs (#146). The early flag/config/validation
// failures below still call os.Exit(1) directly: they fire BEFORE any resource
// (the DB, the watcher) is open, so no defer is skipped that matters. Every
// os.Exit AFTER store.Open has been converted to `return 1` so db.Close and the
// watcher-drain shutdown sequence run on the fatal path too.
func runServe(args []string) int {
	// The `serve` CLI entry ALWAYS uses a zero serveOptions — no serve flag or env
	// var can set syntheticDemo. That unreachability is the #476 security property:
	// only runDemo passes syntheticDemo:true. See serveOptions and validateBind.
	return runServeWithOptions(args, serveOptions{})
}

// serveOptions carries process-internal serve settings that are DELIBERATELY not
// CLI flags or env vars. It exists so `tierd demo` can hand the serve path a
// signal that must be impossible to set from a `serve` invocation on real data.
type serveOptions struct {
	// syntheticDemo marks a serve driven by `tierd demo` (#476): the dataset is
	// invented and every write/ingest/admin route is structurally absent
	// (--read-only), so exposing it beyond loopback leaks nothing real. It relaxes
	// validateBind for that case ONLY. It is set exclusively by runDemo and is
	// unreachable from any serve flag/env, so a serve over real data on a
	// non-loopback bind without a token is still refused.
	syntheticDemo bool
}

func runServeWithOptions(args []string, opts serveOptions) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config file (#29); CLI flags + env vars override its values")
	addr := fs.String("addr", defaultListenAddr, "listen address (loopback by default; a non-loopback bind requires --api-token)")
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	pricesPath := fs.String("prices", os.Getenv("TIER_PRICES"), "path to a price-table YAML override (#68); empty uses the embedded default. A bad file fails startup")
	webhookSecret := fs.String("webhook-secret", os.Getenv("TIER_WEBHOOK_SECRET"), "GitHub webhook secret. Prefer TIER_WEBHOOK_SECRET env var or @/path/to/file (read from disk); a literal value here leaks via ps/shell history (#37)")
	apiToken := fs.String("api-token", os.Getenv("TIER_API_TOKEN"), "API token: Bearer on POST writes and score GETs, X-Tier-Token on the proxies; empty disables auth and restricts the bind to loopback (#59). Prefer TIER_API_TOKEN env var or @/path/to/file; a literal value here leaks via ps/shell history (#37)")
	readToken := fs.String("read-token", os.Getenv("TIER_READ_TOKEN"), "Read-only viewer token (#190): Bearer that grants GET /scores, /scores/{dev}, and /metrics (the dashboard data) but is rejected on all writes and the proxies. Must differ from --api-token; alone it does NOT permit a non-loopback bind. Prefer TIER_READ_TOKEN env var or @/path/to/file; a literal value here leaks via ps/shell history (#37)")
	anthropicTarget := fs.String("anthropic-target", "https://api.anthropic.com", "upstream Anthropic API URL")
	openaiTarget := fs.String("openai-target", "https://api.openai.com", "upstream OpenAI-compatible API URL")
	rlDefault := api.DefaultRateLimitConfig()
	authMaxFailures := fs.Int("auth-max-failures", rlDefault.MaxFailures, "per-IP failed-auth attempts within --auth-failure-window before a 429 lockout (#36); 0 disables the limiter")
	authFailureWindow := fs.Duration("auth-failure-window", rlDefault.Window, "sliding window over which --auth-max-failures is counted (#36)")
	authLockout := fs.Duration("auth-lockout", rlDefault.Lockout, "how long an IP stays locked out (429) after tripping --auth-max-failures (#36)")
	zeroOutcomeWindowDays := fs.Int("zero-outcome-window-days", 7, "zero-outcome tripwire look-back window in days (#189): serve fails loud (WARN log + tier_zero_outcome_tripwire metric) when cost accrued in this window but zero outcomes were recorded. Config key: zero_outcome_window_days. Must be >= 1")
	pushCapture := fs.Bool("push-capture", envBool("TIER_PUSH_CAPTURE"), "capture a qualifying direct commit to the default branch as a degraded (0.5, per-issue-per-UTC-day) outcome so trunk-based teams aren't scored ~0 (#196). OFF by default. Env TIER_PUSH_CAPTURE, config key outcomes.push_capture. Precedence: CLI > env > config > default")
	readOnly := fs.Bool("read-only", envBool("TIER_READ_ONLY"), "public-demo mode (#429): mount ONLY the read + health routes; every write/ingest/admin route, the GitHub webhook, the proxies, the watcher, and the pollers are STRUCTURALLY ABSENT (404 / not started), not merely token-gated — so a leaked token cannot reach any mutation. Governs WRITES only: reads stay OPEN per the token/aggregation config, so public exposure is safe ONLY on synthetic data (e.g. `tierd demo`) or with a read-token + k-anonymized aggregation. For the community demo behind a rate-limiter. OFF by default. Env TIER_READ_ONLY")
	aggregation := fs.String("aggregation", os.Getenv("TIER_AGGREGATION"), "REQUIRED reporting mode (#185, #270): 'team' emits only team-level aggregates and 'division' rolls up one level higher to divisions — both k-anonymized and NEVER naming an individual (the safe posture under EU works-council / GDPR Art. 22 co-determination — Germany §87 BetrVG, France, NL); 'developer' keeps named per-developer rows. NO default — serve FAILS to start when unset from flag/env/config, so an existing deployment's behavior never changes silently. Env TIER_AGGREGATION, config key aggregation")
	kAnonymity := fs.Int("k-anonymity", envIntDefault("TIER_K_ANONYMITY", scoring.DefaultKAnonymity), "k-anonymity cohort floor for the anonymized aggregation modes --aggregation team|division (#185, #270): a group with fewer than this many CONTRIBUTING developers collapses into an aggregate 'other' bucket so no sub-k cohort is identifiable. Default 5; HARD minimum 3 (serve refuses a smaller value). Env TIER_K_ANONYMITY, config key k_anonymity")
	var trustedProxyCIDRs repeatableStringSlice
	fs.Var(&trustedProxyCIDRs, "trusted-proxy-cidr", "CIDR of a trusted reverse proxy/TLS terminator (repeatable). When the direct peer is inside a trusted CIDR, the failed-auth lockout keys on the client IP from X-Forwarded-For (rightmost untrusted hop) instead of the peer address. Default: unset — X-Forwarded-For is never trusted (#131)")
	var watchRepos repeatableStringSlice
	fs.Var(&watchRepos, "watch-repo", "git repo path to tail Claude Code JSONL for (repeatable; omit to disable live ingestion)")
	codexRollout := fs.Bool("codex-rollout", envBool("TIER_CODEX_ROLLOUT"), "capture Codex CLI spend from the local rollout logs at ~/.codex/sessions/**/rollout-*.jsonl (#464). This is the ONLY path that captures Codex — Codex speaks the OpenAI Responses API, which the reverse proxy's Chat Completions parser cannot read (#463). Attributes to the same repos as --watch-repo; with no watched repo serve refuses to start (except under --read-only, which disables all capture anyway). OFF by default. Env TIER_CODEX_ROLLOUT, config block collectors.codex_rollout (which also sets sessions_dir / scan_interval)")
	logFormat := fs.String("log-format", cmp.Or(os.Getenv("TIER_LOG_FORMAT"), "auto"), "log format: auto|json|text (#67; auto = JSON unless stderr is a terminal)")
	logLevel := fs.String("log-level", cmp.Or(os.Getenv("TIER_LOG_LEVEL"), "info"), "log level: debug|info|warn|error (#67)")
	_ = fs.Parse(args)

	// Build the configured logger first thing after flag parse and make it the
	// process default, so EVERY line below — including the config-resolution
	// warning and any slog.Default() in library code — honors --log-format /
	// --log-level (#67). log-format/log-level come only from flags+env, not the
	// config file, so they're fully resolved here.
	logger, err := newLogger(os.Stderr, *logFormat, *logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	// Apply config-file values to flags the operator did NOT pass on the
	// command line AND that didn't already pick up a non-empty env-var
	// default (#29). Precedence: CLI flag > env var > config file >
	// builtin default. fs.Visit distinguishes CLI-set from defaulted;
	// envVarSet below tracks the env-var dimension (the flag layer
	// otherwise loses that information because env vars are wired as
	// flag-construction-time defaults).
	// anthropicAdminCfg is populated only from the config file (collectors block
	// has no CLI flag / env var). nil = block absent = poller disabled.
	// watchRepoSlugs carries the config-only watch.repo_slugs override map (#231)
	// out of the config block so the Watcher construction below can consume it.
	// There is deliberately no CLI flag: a per-path map is a poor fit for a
	// repeatable string flag, and the override is a set-once deployment fact.
	var watchRepoSlugs map[string]string
	var anthropicAdminCfg *config.AnthropicAdminConfig
	var openaiUsageCfg *config.OpenAIUsageConfig
	// codexRolloutCfg carries the collectors.codex_rollout block (#464). Unlike
	// the two pollers this collector DOES have a CLI flag (--codex-rollout),
	// because it needs no credential — the flag alone is enough to enable it
	// with defaults, and the block only exists to override sessions_dir /
	// scan_interval.
	var codexRolloutCfg *config.CodexRolloutConfig
	// generatedPathsCfg carries the config-only outcomes.generated_paths override
	// (#240) out of the config block to the webhook wiring below. There is no CLI
	// flag / env var, so nil means "config key absent" → keep the handler's built-in
	// defaultGeneratedPaths; a non-nil slice (including an explicit empty `[]`, which
	// disables exclusion) is plumbed into webhook.WithGeneratedPaths.
	var generatedPathsCfg []string
	// sizeLabelsCfg carries the config-only outcomes.size_labels override (#244) out
	// of the config block to the webhook wiring below. There is no CLI flag / env
	// var, so nil means "config key absent" → keep the handler's built-in
	// defaultSizeLabels; a non-nil map (config.Load has already validated every
	// weight is on the fixed scale and no two names collide) is plumbed into
	// webhook.WithSizeLabels, which treats an empty map as a no-op so `{}` preserves
	// the defaults.
	var sizeLabelsCfg map[string]float64
	if *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config: %v\n", err)
			os.Exit(1)
		}
		anthropicAdminCfg = cfg.Collectors.AnthropicAdmin
		openaiUsageCfg = cfg.Collectors.OpenAIUsage
		codexRolloutCfg = cfg.Collectors.CodexRollout
		watchRepoSlugs = cfg.Watch.RepoSlugs
		generatedPathsCfg = cfg.Outcomes.GeneratedPaths
		sizeLabelsCfg = cfg.Outcomes.SizeLabels
		setFlags := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
		envVarSet := map[string]bool{
			"webhook-secret": os.Getenv("TIER_WEBHOOK_SECRET") != "",
			"api-token":      os.Getenv("TIER_API_TOKEN") != "",
			"read-token":     os.Getenv("TIER_READ_TOKEN") != "",
			"prices":         os.Getenv("TIER_PRICES") != "",
			"push-capture":   os.Getenv("TIER_PUSH_CAPTURE") != "",
			"aggregation":    os.Getenv("TIER_AGGREGATION") != "",
			"k-anonymity":    os.Getenv("TIER_K_ANONYMITY") != "",
		}
		applyStringFromConfig(fs, setFlags, envVarSet, "addr", cfg.HTTP.Addr)
		applyStringFromConfig(fs, setFlags, envVarSet, "db", cfg.DB)
		applyStringFromConfig(fs, setFlags, envVarSet, "prices", cfg.PricesFile)
		applyStringFromConfig(fs, setFlags, envVarSet, "webhook-secret", cfg.HTTP.WebhookSecret)
		applyStringFromConfig(fs, setFlags, envVarSet, "api-token", cfg.HTTP.APIToken)
		applyStringFromConfig(fs, setFlags, envVarSet, "read-token", cfg.HTTP.ReadToken)
		applyStringFromConfig(fs, setFlags, envVarSet, "anthropic-target", cfg.Proxy.AnthropicTarget)
		applyStringFromConfig(fs, setFlags, envVarSet, "openai-target", cfg.Proxy.OpenAITarget)
		applyIntFromConfig(fs, setFlags, envVarSet, "zero-outcome-window-days", cfg.ZeroOutcomeWindowDays)
		applyBoolFromConfig(fs, setFlags, envVarSet, "push-capture", cfg.Outcomes.PushCapture)
		applyStringFromConfig(fs, setFlags, envVarSet, "aggregation", cfg.Aggregation)
		applyIntFromConfig(fs, setFlags, envVarSet, "k-anonymity", cfg.KAnonymity)
		// Auth-lockout knobs (#85). No env-var layer exists for these, so their
		// envVarSet keys are simply absent (false). The resolved flag values flow
		// into the existing >0 validation below, so a config-sourced combo is
		// validated identically to a flag-sourced one.
		applyIntFromConfig(fs, setFlags, envVarSet, "auth-max-failures", cfg.HTTP.Auth.MaxFailures)
		applyDurationFromConfig(fs, setFlags, envVarSet, "auth-failure-window", cfg.HTTP.Auth.FailureWindow)
		applyDurationFromConfig(fs, setFlags, envVarSet, "auth-lockout", cfg.HTTP.Auth.Lockout)
		// watch-repo is repeatable. CLI wins entirely when any --watch-repo
		// is passed — partial override via config makes no sense for a
		// list-valued flag. Log a warning so the operator notices the
		// config's list was discarded.
		if setFlags["watch-repo"] {
			if len(cfg.Watch.Repos) > 0 {
				logger.Warn("--watch-repo on CLI overrides config watch.repos entirely",
					"cli_repos", []string(watchRepos),
					"config_repos_ignored", cfg.Watch.Repos)
			}
		} else {
			for _, r := range cfg.Watch.Repos {
				if err := watchRepos.Set(r); err != nil {
					fmt.Fprintf(os.Stderr, "config: invalid watch.repos entry %q: %v\n", r, err)
					os.Exit(1)
				}
			}
		}
		// trusted-proxy-cidr is repeatable, same list-valued CLI-wins-entirely
		// precedence as watch-repo (#131): any --trusted-proxy-cidr on the CLI
		// discards the config list entirely rather than partially merging.
		if setFlags["trusted-proxy-cidr"] {
			if len(cfg.HTTP.TrustedProxyCIDRs) > 0 {
				logger.Warn("--trusted-proxy-cidr on CLI overrides config http.trusted_proxy_cidrs entirely",
					"cli_cidrs", []string(trustedProxyCIDRs),
					"config_cidrs_ignored", cfg.HTTP.TrustedProxyCIDRs)
			}
		} else {
			for _, c := range cfg.HTTP.TrustedProxyCIDRs {
				if err := trustedProxyCIDRs.Set(c); err != nil {
					fmt.Fprintf(os.Stderr, "config: invalid http.trusted_proxy_cidrs entry %q: %v\n", c, err)
					os.Exit(1)
				}
			}
		}
	}

	// Validate the zero-outcome tripwire window (#189) after CLI+config resolution.
	// A sub-day window is a misconfiguration (correct-from-start: fail loud rather
	// than silently coerce), so refuse it at startup.
	if *zeroOutcomeWindowDays < 1 {
		fmt.Fprintf(os.Stderr, "--zero-outcome-window-days must be >= 1, got %d\n", *zeroOutcomeWindowDays)
		os.Exit(1)
	}
	zeroOutcomeWindow := time.Duration(*zeroOutcomeWindowDays) * 24 * time.Hour

	// Resolve the REQUIRED aggregation mode (#185) after CLI+env+config resolution.
	// There is deliberately NO silent default: defaulting would flip an existing
	// deployment between naming individuals and not on upgrade — an EU works-council
	// / GDPR Art. 22 co-determination concern — so an unset value is a hard startup
	// error that tells the operator how to choose.
	aggMode, err := resolveAggregationMode(*aggregation)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// k-anonymity floor (#185): reject anything below the hard minimum (3), which
	// would gut the anonymity set. Validated unconditionally so a misconfigured
	// value fails loud even in developer mode (where it is unused) rather than
	// lurking until someone switches to team mode.
	if err := validateKAnonymity(*kAnonymity); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Resolve `@file` secret indirection (#37) before anything reads the token
	// or secret. Runs after config resolution so a value sourced from CLI, env,
	// or config alike can use the `@/path` form; keeps the secret off the
	// command line (and out of ps/shell history).
	resolveOrExit := func(flagName string, p *string) {
		v, err := resolveSecretFlag(*p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "--%s: %v\n", flagName, err)
			os.Exit(1)
		}
		*p = v
	}
	resolveOrExit("api-token", apiToken)
	resolveOrExit("read-token", readToken)
	resolveOrExit("webhook-secret", webhookSecret)

	// Read-token guardrails (#190), checked after @file/env/config resolution so
	// they see the effective values regardless of source. A read token equal to
	// the api token would match the WRITE scope (classify prefers write) and
	// silently grant writes, defeating least privilege — refuse to start.
	if err := checkReadToken(*apiToken, *readToken); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// A read token with no api token has no effect: auth is disabled entirely in
	// that mode, and validateBind still refuses a non-loopback bind on a read
	// token alone. Warn rather than fail so loopback dev use is unhurt.
	if *readToken != "" && *apiToken == "" {
		logger.Warn("--read-token is set but --api-token is empty: auth is disabled and the read-only token has no effect; a non-loopback bind still requires --api-token (#190)")
	}

	// Resolve + validate the Anthropic Admin poller config (#138) BEFORE store.Open
	// so a misconfiguration fails fast at startup rather than after the DB is open.
	// Absent block → nil settings → poller stays disabled (logged where it would
	// otherwise start). The admin key uses the same @file indirection as the other
	// secrets, so it never appears on the command line.
	adminSettings, err := resolveAnthropicAdminConfig(anthropicAdminCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// Same for the OpenAI Usage/Costs poller (#139): resolve+validate before
	// store.Open so a misconfiguration fails fast. Absent block → nil → disabled.
	openaiSettings, err := resolveOpenAIUsageConfig(openaiUsageCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// Same for the Codex rollout-log collector (#464): resolve+validate before
	// store.Open. Enabled by EITHER --codex-rollout or the config block.
	codexSettings, err := resolveCodexRolloutConfig(codexRolloutCfg, *codexRollout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Fail fast when Codex capture is requested but has no repo to attribute to
	// (#479). --codex-rollout attributes to the SAME repos as --watch-repo, so
	// enabling it with no watched repo is a silent no-op — the exact "knob that
	// does nothing" that erodes trust in a capture path. watchRepos here already
	// merges the CLI flag and the config watch.repos block, so a config-only repo
	// counts. The one exception is --read-only, which deliberately nulls every
	// ingest config just below (including codex); there the mismatch is expected,
	// so it warns rather than aborts.
	if warn, fatal := codexWatchCheck(codexSettings != nil, len(watchRepos), *readOnly); fatal != "" {
		fmt.Fprintln(os.Stderr, fatal)
		os.Exit(1)
	} else if warn != "" {
		logger.Warn(warn)
	}

	// Read-only public-demo mode (#429): neutralize EVERY ingest / relay / write
	// CONFIG in ONE place, so the ordinary guards below (len(watchRepos)>0,
	// *webhookSecret!="", parseProxyTarget!=nil, adminSettings!=nil, ...) skip these
	// subsystems naturally — and, critically, a FUTURE ingest subsystem keyed off
	// its own config var is auto-disabled here rather than silently exposed. The API
	// write ROUTES are omitted structurally via RegisterReadOnly (below); this single
	// choke point handles the background collectors, the webhook, and the relays.
	// See the SECURITY invariant on api.Handler.RegisterReadOnly.
	if *readOnly {
		watchRepos = nil
		*webhookSecret = ""
		*anthropicTarget, *openaiTarget = "", ""
		adminSettings, openaiSettings = nil, nil
		codexSettings = nil
	}

	// A non-zero failure cap with a non-positive window or lockout would create
	// a limiter that looks armed but can never trip (#36) — refuse to start
	// rather than expose auth that is silently un-throttled.
	if err := validateAuthLockout(*authMaxFailures, *authFailureWindow, *authLockout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Parse the trusted-proxy CIDRs into prefixes, failing loud on a bad entry
	// (#131). A bare IP is the common mistake, so the error suggests the fix.
	// Empty list → nil → X-Forwarded-For stays untrusted (default unchanged).
	trustedProxies, err := parseTrustedProxies(trustedProxyCIDRs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Fail-closed exposure check (#59) — runs after config-file resolution so
	// it sees the effective addr/token regardless of where they came from.
	// DELIBERATELY passes only *apiToken (#190): a read-only token alone must NOT
	// satisfy the bind — a non-loopback listener still requires the write/admin
	// token, so a read token can never open the writes/proxies to the network.
	// opts.syntheticDemo (#476) is the sole non-token exemption and is reachable
	// only via `tierd demo`, never from a serve flag/env — see serveOptions.
	if err := validateBind(*addr, *apiToken, opts.syntheticDemo); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Apply a --prices override (#68) BEFORE store.Open: Open runs the #55 cost
	// recompute, which prices rows via ComputeCost, so the table must be settled
	// first. A bad file exits non-zero rather than silently using the embedded
	// default. With no override, the embedded default (loaded at package init)
	// stays active.
	if info, ok := loadPricesOverride(*pricesPath); ok {
		logger.Info("loaded price-table override", "path", *pricesPath,
			"version", info.Version, "effective_date", info.EffectiveDate, "models", info.ModelCount)
	} else {
		info := store.ActivePriceTableInfo()
		logger.Info("using embedded price table",
			"version", info.Version, "effective_date", info.EffectiveDate, "models", info.ModelCount)
	}

	// Ensure the database directory exists before opening.
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "create db dir: %v\n", err)
		os.Exit(1)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	mux := http.NewServeMux()

	// Shared watcher health state. Only allocated when --watch-repo is set;
	// otherwise nil flows into api.New, which makes /healthz report
	// status=not_configured. Avoids reserving a heap object that would
	// otherwise be never written to and would invite a future reader to
	// wonder "why is this here?"
	var watcherState *health.WatcherState
	if len(watchRepos) > 0 {
		watcherState = health.NewWatcherState()
	}

	// REST API.
	// Metrics registry + the fixed metric set (#67). Built before the handler so
	// the auth-gated GET /metrics route is mounted by Register.
	srvMetrics := newServeMetrics(version)
	// Route unknown-model pricing fallbacks into tier_unknown_model_events_total
	// (#68). Set once here, before serving (the write-once-before-serve seam in
	// internal/store); the nil-default recorder is a no-op for `tierd score`.
	// Installed AFTER store.Open by design: the counter is a LIVE mispriced-spend
	// signal, so the one-time #55 historical recompute (which also runs through
	// ComputeCost during Open) is intentionally NOT counted — its unknown models
	// still surface via the one-time WARN log, just not this counter.
	store.SetUnknownModelRecorder(srvMetrics.unknownModels)
	// Cost-weighted companions (#135): the micro-dollar cost billed at the
	// unknown-model fallback rate, and the total priced cost across all events.
	// Same write-once-before-serve seam and the same live-signal rationale as the
	// event counter above (the historical #55 recompute during Open is not
	// counted). The 10-minute ticker below reads these to alert on the SHARE of
	// window spend billed at the fallback rate.
	store.SetUnknownModelCostRecorder(srvMetrics.unknownModelCost)
	store.SetPricedCostRecorder(srvMetrics.pricedCost)
	// Route parser-boundary negative-token clamps into
	// tier_negative_tokens_clamped_total (#121). Same write-once-before-serve
	// discipline as the recorder above; the nil default is a no-op for
	// `tierd score` and unit tests, so no clamp is lost when serving.
	collector.SetClampRecorder(srvMetrics.clampedNegTok)

	apiHandler := api.New(db, logger, *apiToken, watcherState, version, api.RateLimitConfig{
		MaxFailures:    *authMaxFailures,
		Window:         *authFailureWindow,
		Lockout:        *authLockout,
		TrustedProxies: trustedProxies,
	})
	apiHandler.SetMetricsRegistry(srvMetrics.reg)
	// Wire the unjoined-identity gauge (#125), same write-once-before-serve seam
	// as the registry above; /scores Sets it per read, nil-safe elsewhere.
	apiHandler.SetIdentityGauge(srvMetrics.identityUnjoined)
	// Wire the pricing-divergence counter (#233), same seam; /events Inc()s it when
	// a shipper's client cost_usd disagrees with the server's authoritative price.
	apiHandler.SetPricingDivergenceCounter(srvMetrics.pricingDivergence)
	// Read-only viewer token (#190), same write-once-before-serve seam. Empty =
	// no read scope armed. Must precede Register so the read routes bind the
	// scope-aware middleware.
	apiHandler.SetReadToken(*readToken)
	// Reporting mode + k-anonymity floor (#185), same write-once-before-serve seam.
	// In team mode the served /scores, the dashboard it feeds, and /scores/{developer}
	// never name an individual developer.
	apiHandler.SetAggregation(aggMode, *kAnonymity)
	logger.Info("aggregation mode", "mode", *aggregation, "k_anonymity", *kAnonymity)
	// Team-mode empty-hierarchy tripwire (#232): warn loudly at startup when team
	// aggregation is armed with no hierarchy populated, so an operator does not
	// trust a one-row "other" dashboard. A cheap single read; never blocks serve.
	checkEmptyTeamHierarchy(context.Background(), aggMode, db, logger)
	// Announce which auth scopes are armed so an operator can confirm at startup
	// that the write and (optionally) read credentials took effect. Booleans
	// only — the secrets themselves are never logged.
	logger.Info("API auth scopes",
		"write_armed", *apiToken != "",
		"read_armed", *readToken != "")
	// Announce the RESOLVED failed-login lockout state (#36). Config parity (#85)
	// newly lets an operator disable the brute-force throttle by committing
	// http.auth.max_failures: 0 into a repo YAML — a persistent, invisible loss of
	// defence-in-depth. Logging the resolved values (thresholds are not secrets)
	// makes an accidental disable obvious at boot rather than discovered under attack.
	logger.Info("auth failed-login lockout",
		"enabled", *authMaxFailures > 0,
		"max_failures", *authMaxFailures,
		"window", authFailureWindow.String(),
		"lockout", authLockout.String())
	// Read-only public-demo mode (#429): mount ONLY the read + health routes — every
	// write/ingest/admin route is structurally absent (404), not merely token-gated,
	// so a leaked token cannot reach any mutation. The webhook, the proxies, the
	// JSONL watcher, and the coverage pollers below are ALL skipped for the same
	// reason; any --watch-repo / poller config is deliberately ignored in this mode.
	// For the community demo instance on synthetic data behind a rate-limiter.
	if *readOnly {
		apiHandler.RegisterReadOnly(mux)
		logger.Warn("READ-ONLY mode ENABLED (#429): write/ingest/admin API routes, the GitHub webhook, the proxies, the watcher, and the pollers are OFF (404 / not started). Any ingest config is ignored. NOTE: read-only governs WRITES only — the read routes stay OPEN per the token/aggregation config, so public exposure is safe ONLY on synthetic data, or with a read-token + a k-anonymized aggregation (team/division).")
	} else {
		apiHandler.Register(mux)
	}

	// GitHub webhook — mounted only when a secret is configured (#60). An
	// unvalidated webhook would let anyone who can reach the listener forge
	// merged-PR outcomes or fake revert pushes. The handler also fails closed
	// internally; not mounting keeps the surface off the mux entirely and makes the
	// disabled state visible at startup. In read-only mode (#429) the choke point
	// above has already blanked *webhookSecret, so this falls to the disabled branch.
	if *webhookSecret != "" {
		// Push-to-default-branch capture (#196) is opt-in via --push-capture. When
		// enabled the handler also captures qualifying direct commits to the default
		// branch as degraded outcomes; unattributed commits are counted so the drop
		// is observable in /metrics.
		whOpts := buildWebhookOptions(*pushCapture, srvMetrics.pushUnattributed, generatedPathsCfg, sizeLabelsCfg)
		if *pushCapture {
			logger.Info("push-to-default-branch outcome capture is ENABLED (#196): direct commits to the default branch earn degraded 0.5, per-issue-per-UTC-day outcomes")
		}
		mux.Handle("POST /webhook/github", webhook.New(db, *webhookSecret, logger, whOpts...))
	} else {
		logger.Warn("TIER_WEBHOOK_SECRET is not set — POST /webhook/github is disabled (fail closed, #60)")
	}

	// Dashboard.
	mux.Handle("/", dashboard.New())

	// Documentation (#449). Mounted UNCONDITIONALLY — including read-only
	// public-demo mode (#429), right next to the dashboard above and OUTSIDE the
	// `if *readOnly` write-route choke point. The served pages are static,
	// read-only HTML generated from the markdown in docs/ (see internal/docs and
	// tools/docgen); they carry no JavaScript and expose nothing to write or
	// token-gate, so they are safe for the public demo.
	mux.Handle("/docs/", docs.New())

	// eventSink is the shared collector.Ingester the live collectors funnel
	// through (#46 closes the #27 asymmetry): the proxy and the watcher route
	// their writes through ingester.Store(db) — the same adapter the JSONL and
	// admin/usage collectors already used — so the store-write path is uniform no
	// matter which collector produced the event. Per-collector concerns (the
	// Source name via SourceTagger, the watcher's RecordEvent stamp via
	// RecordingIngester) are layered as decorators on top of this one base rather
	// than re-implemented at each write site.
	eventSink := ingester.Store(db)

	// Reverse proxies — only registered when flags are explicitly set and the
	// target URL has a valid http/https scheme and non-empty host. Gated
	// behind the API token via X-Tier-Token (#59): without the gate the
	// proxy is an open relay to the provider for anyone who can reach it.
	//
	// The bare eventSink is passed straight through: proxy.New builds its own
	// collector.SourceProxy tagger around it internally (#337), so there is no
	// separate SourceTagger wrap to keep in sync here — a proxy whose events land
	// with an empty source is unconstructable.
	if t := parseProxyTarget(*anthropicTarget, logger); t != nil {
		p := proxy.New(t, proxy.ProviderAnthropic, collector.SourceProxy, eventSink, srvMetrics.proxyWrites, srvMetrics.proxyUncaptured, logger)
		p.SetUnattributedRecorder(srvMetrics.proxyUnattributed)
		registerProxy(mux, "/anthropic", p, apiHandler.ProxyAuth, logger)
		logger.Info("Anthropic proxy", "path", "/anthropic/", "target", *anthropicTarget)
	}
	if t := parseProxyTarget(*openaiTarget, logger); t != nil {
		p := proxy.New(t, proxy.ProviderOpenAI, collector.SourceProxy, eventSink, srvMetrics.proxyWrites, srvMetrics.proxyUncaptured, logger)
		p.SetUnattributedRecorder(srvMetrics.proxyUnattributed)
		registerProxy(mux, "/openai", p, apiHandler.ProxyAuth, logger)
		logger.Info("OpenAI proxy", "path", "/openai/", "target", *openaiTarget)
	}

	srv := &http.Server{
		Addr: *addr,
		// requestLogger wraps the whole mux so every request (API, webhook,
		// dashboard, proxy) gets one structured access-log line (#67).
		Handler:      requestLogger(logger, srvMetrics.http, mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// watcherCtx outlives a single request and is cancelled on shutdown so
	// the fsnotify loop drains cleanly. Created even when no --watch-repo is
	// set, so we can use cancel() unconditionally below.
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()

	// srvErr carries any fatal server error (HTTP listener crash or watcher
	// unrecoverable failure) back to the main goroutine. Buffered to 2 so
	// neither writer blocks the other on a race during shutdown.
	srvErr := make(chan error, 2)

	// supDone is closed when the watcher supervisor goroutine has fully
	// returned. Shutdown JOINs on it before the deferred db.Close runs (#146):
	// Watcher.Run waits for in-flight inserts (inflight.Wait) before returning,
	// so once Supervise has returned no watcher insert can still be racing the
	// DB close. Pre-closed when no watcher is configured so the join is a no-op.
	supDone := make(chan struct{})

	// Live JSONL ingestion via fsnotify (issue #18). Disabled when no
	// --watch-repo is set; the watcher would otherwise drain events for
	// nothing. As of #28 the watcher runs under a Supervisor that restarts
	// it with exponential backoff on transient failures (fsnotify channel
	// closed, inotify ENOSPC, etc.) and only terminates after 5 failures
	// inside a 60-second window. The Supervisor updates watcherState so
	// /healthz reports the current state to any operator polling.
	if len(watchRepos) > 0 {
		resolved, err := resolveWatchRepos(watchRepos)
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch-repo: %v\n", err)
			// Post-store.Open: return so the deferred db.Close runs (#146).
			return 1
		}
		w := &collector.Watcher{
			Repos: resolved,
			// Operator overrides for repository identity (#231). Absent -> the
			// watcher reads remote.origin.url, then degrades to 'unqualified' with
			// one warning per repo. Required for forks, where origin names the fork
			// and the upstream webhook names the upstream.
			RepoSlugs: watchRepoSlugs,
			// Events funnel through the shared collector.Ingester (#46). The
			// watcher's source is "jsonl" — it is the live counterpart of the
			// JSONLCollector, tailing the same Claude Code session files — so it
			// shares SourceJSONL. RecordingIngester stamps watcherState.RecordEvent
			// (so /healthz last_event_ts reflects live ingestion, #50) AND the
			// tier_watcher_events_total counter (#67) on every event the watcher
			// lands. watcherState is non-nil here (allocated in the same
			// len(watchRepos) > 0 guard).
			Ingester: ingester.RecordingIngester(
				watcherEventRecorder{watcherState, srvMetrics.watcherEvents},
				ingester.SourceTagger(collector.SourceJSONL, eventSink),
			),
			// Checkpoints (#71) persist per-file tail state directly to the store;
			// they are a resume cache, not a token-event sink, so they bypass the
			// Ingester decorators above.
			Checkpoints: db,
			Logger:      logger,
			// Surface silent capture degradation (#142): a failed watch add is
			// otherwise logged-and-swallowed, so a macOS laptop that hits its fd
			// limit goes blind directory-by-directory with nothing observable.
			// Route each failure to /healthz (watch_add_failures) and /metrics
			// (errno-labelled counter). watcherState is non-nil in this guard.
			OnWatchAddFailure: func(_ string, err error) {
				watcherState.RecordWatchAddFailure(err)
				srvMetrics.watcherWatchAddFail.Inc(watchAddErrno(err))
			},
		}
		sup := &health.Supervisor{
			Run:    w,
			State:  watcherState,
			Logger: logger,
		}
		go func() {
			// close(supDone) on return is the join the shutdown sequence waits
			// on so no watcher insert can race the db.Close (#146).
			defer close(supDone)
			if err := sup.Supervise(watcherCtx); err != nil {
				select {
				case srvErr <- fmt.Errorf("watcher supervisor: %w", err):
				default:
					// srvErr is already populated by the HTTP listener — log
					// and let the existing error path drive shutdown.
					logger.Error("watcher supervisor exited", "err", err)
				}
			}
		}()
	} else {
		// No watcher configured: nothing to drain, so the shutdown join is a
		// no-op. Pre-close so shutdownServer never blocks waiting for it.
		close(supDone)
	}

	// Anthropic Admin API usage/cost poller (#138): org-level remainder ingestion +
	// actual-spend reconciliation. Started only when the collectors.anthropic_admin
	// config block is present and valid. Cancelled via watcherCtx on shutdown.
	//
	// DELIBERATE ASYMMETRY with the watcher supervisor above: the poller goroutine
	// does NOT feed srvErr. Coverage polling is a reconciliation feed, not a
	// liveness-critical data plane, so a poll failure logs ERROR and retries on the
	// next tick (Poller.Run swallows poll errors) — a dead poller must never take
	// down serve.
	if adminSettings != nil {
		adminClient := anthropicadmin.NewClient(anthropicadmin.ClientConfig{
			APIKey: adminSettings.apiKey,
			Logger: logger,
		})
		adminPoller := anthropicadmin.NewPoller(anthropicadmin.PollerConfig{
			Client:   adminClient,
			Store:    db,
			Org:      adminSettings.org,
			Interval: adminSettings.interval,
			Logger:   logger,
			Metrics: anthropicAdminMetrics{
				polls:      srvMetrics.adminPolls,
				events:     srvMetrics.adminEvents,
				costDeltas: srvMetrics.adminCostDeltas,
			},
		})
		logger.Info("Anthropic Admin poller enabled", "org", adminSettings.org, "poll_interval", adminSettings.interval)
		go func() {
			// Run returns nil on ctx cancel (clean shutdown); it never surfaces a
			// poll error, so a non-nil return here is unexpected and worth logging.
			if err := adminPoller.Run(watcherCtx, time.Time{}, ingester.Store(db)); err != nil {
				logger.Error("anthropic-admin poller exited", "err", err)
			}
		}()
	} else {
		logger.Info("Anthropic Admin poller disabled (no collectors.anthropic_admin config block)")
	}

	// OpenAI Usage/Costs API poller (#139): the structural twin of the Anthropic
	// Admin poller above — org-level OpenAI remainder ingestion + actual-spend
	// reconciliation, started only when collectors.openai_usage is present and
	// valid, cancelled via watcherCtx on shutdown. Same DELIBERATE ASYMMETRY: a
	// poll failure logs ERROR and retries; it never feeds srvErr, so a dead
	// poller cannot take down serve.
	if openaiSettings != nil {
		openaiClient := openaiusage.NewClient(openaiusage.ClientConfig{
			APIKey: openaiSettings.apiKey,
			Logger: logger,
		})
		openaiPoller := openaiusage.NewPoller(openaiusage.PollerConfig{
			Client:   openaiClient,
			Store:    db,
			Org:      openaiSettings.org,
			Interval: openaiSettings.interval,
			Logger:   logger,
			Metrics: openaiUsageMetrics{
				polls:      srvMetrics.openaiPolls,
				events:     srvMetrics.openaiEvents,
				costDeltas: srvMetrics.openaiCostDeltas,
			},
		})
		logger.Info("OpenAI Usage poller enabled", "org", openaiSettings.org, "poll_interval", openaiSettings.interval)
		go func() {
			// Run returns nil on ctx cancel (clean shutdown); it never surfaces a
			// poll error, so a non-nil return here is unexpected and worth logging.
			if err := openaiPoller.Run(watcherCtx, time.Time{}, ingester.Store(db)); err != nil {
				logger.Error("openai-usage poller exited", "err", err)
			}
		}()
	} else {
		logger.Info("OpenAI Usage poller disabled (no collectors.openai_usage config block)")
	}

	// Codex rollout-log collector (#464): local, per-developer, per-call Codex
	// capture from ~/.codex/sessions/**/rollout-*.jsonl. It is the ONLY path
	// that captures Codex — Codex speaks the OpenAI Responses API, which the
	// proxy's Chat-Completions parser cannot read (#463), so Codex spend was
	// previously invisible to TIER entirely.
	//
	// SCOPED TO THE SAME REPOS AS THE JSONL WATCHER. A Codex session whose cwd
	// is outside every --watch-repo is dropped, not attributed: cross-repo bleed
	// would put another project's dollars on this project's issues (#15). With
	// no watched repo there is nothing to attribute to, so we say so and skip.
	//
	// Same DELIBERATE ASYMMETRY as the pollers above: a scan failure is logged
	// and retried on the next tick; it never feeds srvErr, so a corrupt rollout
	// log or an unreadable ~/.codex cannot take down serve.
	switch {
	case codexSettings == nil:
		logger.Info("Codex rollout collector disabled (no --codex-rollout flag and no collectors.codex_rollout config block)")
	case len(watchRepos) == 0:
		// Defense in depth: the fail-fast guard after resolveCodexRolloutConfig
		// (#479) already aborts a non-read-only serve in this state, and
		// read-only nulls codexSettings (caught by the case above), so this
		// branch is now unreachable in normal flow. Kept as a belt-and-braces
		// log in case a future refactor reorders those guards.
		logger.Warn("Codex rollout collector enabled but NO --watch-repo is set; it has no repository to attribute Codex spend to and will stay idle. Add --watch-repo (or watch.repos) to capture Codex (#464)")
	default:
		resolved, err := resolveWatchRepos(watchRepos)
		if err != nil {
			// Unreachable in practice: the watcher branch above resolved the
			// same list and returned on error. Handle it rather than assume.
			fmt.Fprintf(os.Stderr, "watch-repo: %v\n", err)
			return 1
		}
		targets := make([]codexrollout.RepoTarget, 0, len(resolved))
		for _, p := range resolved {
			// Reuse the SAME operator slug overrides the watcher gets (#231), so
			// Claude Code and Codex rows for one repo carry an identical repo
			// identity and join the same outcomes. A fork whose origin differs
			// from the upstream would otherwise have its Codex spend land under
			// a second repo identity.
			targets = append(targets, codexrollout.RepoTarget{Path: p, Slug: watchRepoSlugs[p]})
		}
		codexCollector, err := codexrollout.New(codexrollout.Config{
			SessionsDir: codexSettings.sessionsDir,
			Repos:       targets,
			Interval:    codexSettings.interval,
			Logger:      logger,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "codex-rollout: %v\n", err)
			return 1
		}
		// time.Time{} bounds the FIRST pass only: it backfills every rollout log
		// on disk once (idempotency keys make a re-run of that backfill a no-op
		// at the store), after which the collector's own scan cursor bounds each
		// tick to what changed (#464 R3). The cursor is in-memory by design — a
		// restart pays the one-time backfill again rather than carrying a
		// persisted checkpoint whose staleness would be a second failure mode.
		logger.Info("Codex rollout collector enabled",
			"repos", resolved,
			"sessions_dir", cmp.Or(codexSettings.sessionsDir, "(default ~/.codex/sessions)"),
			"scan_interval", codexSettings.interval)
		// OBSERVABILITY (#464 Y7): a collector that silently stops producing rows
		// must be visible somewhere other than the log. RecordingIngester stamps
		// health state's last_event_ts (so /healthz reflects Codex capture, not
		// just the JSONL watcher) and tier_codex_rollout_events_total, which is
		// what an operator alerts on. watcherState is non-nil here: this branch
		// only runs when len(watchRepos) > 0, the same guard that allocates it.
		//
		// It is NOT wrapped in a health.Supervisor, unlike the watcher. The
		// Supervisor exists to restart a Run that FAILED; this Run never returns
		// an error — a failing pass is logged and retried on the next tick by
		// design (see Collector.Run) — so supervising it would restart nothing
		// and report a liveness it cannot actually observe.
		codexIngester := ingester.RecordingIngester(
			codexRolloutEventRecorder{watcherState, srvMetrics.codexRolloutEvents},
			ingester.SourceTagger(collector.SourceCodexRollout, eventSink),
		)
		go func() {
			// Run returns nil on ctx cancel (clean shutdown); it never surfaces a
			// scan error, so a non-nil return here is unexpected and worth logging.
			if err := codexCollector.Run(watcherCtx, time.Time{}, codexIngester); err != nil {
				logger.Error("codex-rollout collector exited", "err", err)
			}
		}()
	}

	// Unknown-model fallback-share monitor (#135): every 10 minutes, WARN if the
	// cost billed at the unknown-model fallback rate in the last interval exceeded
	// unknownCostShareWarnThreshold of total priced cost — the "passive gaming"
	// signal where a newly-launched model bills at the near-free $0.50/M fallback
	// and quietly inflates TIER. Cancelled via watcherCtx on shutdown.
	go runUnknownCostShareMonitor(watcherCtx, unknownCostShareCheckInterval,
		srvMetrics.unknownModelCost, srvMetrics.pricedCost, logger)

	// Zero-outcome tripwire (#189): fail loud when cost accrued in the last
	// zeroOutcomeWindow but NO outcomes were recorded there — the silent-TIER-0
	// signal for trunk-based teams (direct pushes behind flags, no pull_request
	// event) or a broken webhook. Runs a startup check then re-checks periodically;
	// surfaces via a WARN log AND the tier_zero_outcome_tripwire metric. Cancelled
	// via watcherCtx on shutdown. (The actual push-to-default-branch CAPTURE path
	// is deferred to #196; this issue is the fail-loud tripwire only.)
	go runZeroOutcomeTripwire(watcherCtx, zeroOutcomeWindow, zeroOutcomeCheckInterval,
		db, srvMetrics.zeroOutcomeTripwire, logger)

	go func() {
		logger.Info("tierd listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Both the fatal-error path and the signal path run the SAME shutdown
	// sequence (cancel → join watcher → drain HTTP), so the deferred db.Close
	// always runs strictly AFTER the watcher has drained (#146). The fatal path
	// no longer calls os.Exit — that skipped every defer, leaving the DB open
	// and the watcher mid-insert; it now returns 1 so db.Close and the drain
	// run identically to a clean SIGTERM.
	select {
	case err := <-srvErr:
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		shutdownServer(watcherCancel, supDone, srv, watcherDrainTimeout, httpShutdownTimeout, logger)
		logger.Info("tierd stopped")
		return 1
	case <-quit:
		shutdownServer(watcherCancel, supDone, srv, watcherDrainTimeout, httpShutdownTimeout, logger)
		logger.Info("tierd stopped")
		return 0
	}
}

// httpShutdownTimeout bounds the graceful HTTP drain (unchanged from the
// original inline 10s). watcherDrainTimeout bounds how long shutdown waits for
// the watcher supervisor to join before closing the DB anyway (#146): a
// pathological insert hang must not make SIGTERM hang forever (systemd would
// SIGKILL, but we want a logged, orderly give-up). 15s > httpShutdownTimeout so
// a healthy watcher always joins well within it.
const (
	httpShutdownTimeout = 10 * time.Second
	watcherDrainTimeout = 15 * time.Second
)

// shutdownServer is the single graceful-shutdown sequence shared by the signal
// and fatal-error paths (#146). Order is load-bearing: cancel the watcher
// context, JOIN the supervisor (supDone) so Watcher.Run's inflight.Wait has
// returned and no insert can race the caller's deferred db.Close, then drain
// the HTTP server. The join is bounded by drainTimeout: if the watcher wedges
// (an insert that never returns) it logs an ERROR and proceeds to close anyway,
// so a stuck watcher can't make shutdown ignore SIGTERM indefinitely. supDone
// is pre-closed when no watcher is configured, making the join an instant no-op.
func shutdownServer(watcherCancel context.CancelFunc, supDone <-chan struct{}, srv *http.Server, drainTimeout, httpTimeout time.Duration, logger *slog.Logger) {
	watcherCancel()
	select {
	case <-supDone:
	case <-time.After(drainTimeout):
		logger.Error("watcher failed to drain before shutdown timeout; closing DB anyway",
			"timeout", drainTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// unknownCostShareWarnThreshold is the fraction of interval priced spend billed
// at the unknown-model fallback rate above which runUnknownCostShareMonitor logs
// a WARN (#135). 5% is small enough to catch a newly-launched model quietly
// eating the denominator, large enough to ignore incidental one-off unknowns.
const unknownCostShareWarnThreshold = 0.05

// unknownCostShareCheckInterval is how often the fallback-share monitor samples
// the two cost counters. 10 minutes: long enough that a handful of unknown
// events cannot trip a spurious WARN, short enough to surface a systematic
// mispricing within one working session.
const unknownCostShareCheckInterval = 10 * time.Minute

// runUnknownCostShareMonitor samples the unknown-model and total priced-cost
// counters every interval and calls checkUnknownCostShare on the per-interval
// DELTAS (the counters are monotone, so delta = now - previous). It returns when
// ctx is cancelled (shutdown). No initial sample is emitted — the first tick
// establishes the baseline for the first real interval.
func runUnknownCostShareMonitor(ctx context.Context, interval time.Duration, unknown, priced *costRecorder, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	prevUnknown, prevPriced := unknown.Total(), priced.Total()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			curUnknown, curPriced := unknown.Total(), priced.Total()
			checkUnknownCostShare(curUnknown-prevUnknown, curPriced-prevPriced, logger)
			prevUnknown, prevPriced = curUnknown, curPriced
		}
	}
}

// checkUnknownCostShare logs a WARN when the unknown-model fallback cost over an
// interval exceeded unknownCostShareWarnThreshold of the total priced cost.
// Deltas are micro-dollar counter increments over the interval. It is pure and
// synchronously testable (deltas + logger in, one optional WARN out) — no ticker,
// no clock. Returns true when it warned. A non-positive unknown or total delta
// is silent: no fallback spend to flag, or no priced spend to divide by.
func checkUnknownCostShare(unknownDeltaMicro, totalDeltaMicro int64, logger *slog.Logger) bool {
	if unknownDeltaMicro <= 0 || totalDeltaMicro <= 0 {
		return false
	}
	share := float64(unknownDeltaMicro) / float64(totalDeltaMicro)
	if share <= unknownCostShareWarnThreshold {
		return false
	}
	logger.Warn("unknown-model fallback exceeds threshold of priced spend this interval; add the missing models to prices.yaml",
		"share", share,
		"threshold", unknownCostShareWarnThreshold,
		"unknown_usd", store.MicroToDollars(unknownDeltaMicro),
		"total_usd", store.MicroToDollars(totalDeltaMicro))
	return true
}

// zeroOutcomeCheckInterval is how often the zero-outcome tripwire re-queries after
// its startup check (#189). 1h: the window is measured in days, so an hourly
// re-check surfaces a newly-broken outcome path within a working session without
// hammering the DB.
const zeroOutcomeCheckInterval = time.Hour

// windowActivityStore is the store slice the zero-outcome tripwire needs (#189).
// A method-subset interface (consumer-side, per CLAUDE.md's store-seam note) keeps
// checkZeroOutcome unit-testable with a fake — no real DB, ticker, or clock.
type windowActivityStore interface {
	WindowActivity(ctx context.Context, since time.Time) (store.WindowActivity, error)
}

// teamHierarchyStore is the store slice the empty-hierarchy startup tripwire
// needs (#232, #270). Consumer-side subset, like windowActivityStore, so the
// check is unit-testable with a fake. It carries BOTH level reads so the check
// can probe the level that is actually armed (see checkEmptyTeamHierarchy).
type teamHierarchyStore interface {
	TeamsForDevelopers(ctx context.Context) (map[string]string, error)
	DivisionsForDevelopers(ctx context.Context) (map[string]string, error)
}

// checkEmptyTeamHierarchy warns once at startup when an ANONYMIZED aggregation
// mode — team (#232) or division (#270) — is armed but org_hierarchy names no
// group AT THE ACTIVE LEVEL. Without any named group, EVERY developer falls in
// the "" group, under the k-anonymity floor, and folds into a single "other"
// bucket, while period_membership opens no seat so org_actual_spend allocation
// reads 0 — the required EU-safe mode renders one anonymous row that looks like a
// working dashboard.
//
// The probe is LEVEL-SPECIFIC, not "is the table empty": division is nullable
// (unlike team), so a hierarchy with every team populated but every division NULL
// has a non-empty team map yet still collapses entirely to "other" in division
// mode. Probing the active level's resolver — and treating "all labels blank" the
// same as "table empty" — catches that case, which is the one division mode is
// MOST prone to. Returns true when the warning fired, false otherwise (developer
// mode, a named group exists, or a query error). A query error is surfaced but
// never blocks serve — the check is advisory, not a gate.
func checkEmptyTeamHierarchy(ctx context.Context, mode scoring.AggregationMode, st teamHierarchyStore, logger *slog.Logger) bool {
	if !mode.Anonymized() {
		return false
	}
	// Select the active level's hierarchy read explicitly. A new anonymized level
	// added to scoring.AggregationMode without wiring its read here must NOT silently
	// fall through to the team probe (which would check team-population while, say,
	// org mode is armed): fail loud and skip, mirroring how internal/api's
	// resolveGroupLabels errors on a mode with no registered resolver (#270). The
	// api.groupLabelResolvers map is the sibling seam; it lives in another package,
	// so this switch is the local mirror — keep the two level lists in lockstep.
	var read func(context.Context) (map[string]string, error)
	switch mode {
	case scoring.AggregationTeam:
		read = st.TeamsForDevelopers
	case scoring.AggregationDivision:
		read = st.DivisionsForDevelopers
	default:
		logger.Warn("aggregation startup check: no org_hierarchy probe wired for this anonymized mode; skipping empty-hierarchy check (a new level needs its read added here)", "mode", mode.String())
		return false
	}
	labels, err := read(ctx)
	if err != nil {
		logger.Warn("aggregation startup check: could not read org_hierarchy", "mode", mode.String(), "err", err)
		return false
	}
	named := 0
	for _, label := range labels {
		if label != "" {
			named++
		}
	}
	if named == 0 {
		logger.Warn("--aggregation " + mode.String() + " is set but org_hierarchy names no " + mode.String() + " (the table is empty, or every " + mode.String() + " is blank): all developers will aggregate into 'other' (the k-anonymity floor folds every unmapped developer into one bucket), and org_actual_spend allocation will read 0. Populate hierarchy via PUT /api/v1/org_hierarchy/{developer} or bulk POST /api/v1/org_hierarchy before trusting /scores (#232)")
		return true
	}
	return false
}

// runZeroOutcomeTripwire runs the startup check immediately, then re-checks every
// interval until ctx is cancelled (shutdown) — mirroring runUnknownCostShareMonitor's
// lifecycle, so the goroutine joins cleanly on watcherCtx cancellation with no
// leak. It owns no state beyond the ticker.
func runZeroOutcomeTripwire(ctx context.Context, window, interval time.Duration, st windowActivityStore, gauge *metrics.GaugeVec, logger *slog.Logger) {
	// Startup check first (the acceptance criterion is a startup check AND a
	// periodic re-check), then the ticker loop.
	checkZeroOutcome(ctx, window, st, gauge, logger)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkZeroOutcome(ctx, window, st, gauge, logger)
		}
	}
}

// checkZeroOutcome queries the window once and evaluates the tripwire. It sets the
// gauge (1 tripped / 0 clear) and WARNs when tripped; returns true when tripped.
// A query error is logged and treated as not-tripped, and it deliberately does NOT
// touch the gauge — keeping the last known value rather than flapping to 0 on a
// transient DB error. Split from evalZeroOutcome so the predicate is pure-testable.
func checkZeroOutcome(ctx context.Context, window time.Duration, st windowActivityStore, gauge *metrics.GaugeVec, logger *slog.Logger) bool {
	since := time.Now().Add(-window).UTC()
	act, err := st.WindowActivity(ctx, since)
	if err != nil {
		logger.Warn("zero-outcome tripwire: window activity query failed", "err", err)
		return false
	}
	if !evalZeroOutcome(act) {
		gauge.Set(0)
		return false
	}
	gauge.Set(1)
	logger.Warn("zero-outcome tripwire: cost accrued but ZERO outcomes recorded in window — team TIER will read ~0; check GitHub webhook delivery, or a trunk-based workflow whose merges don't fire pull_request events (enable outcomes.push_capture / --push-capture to capture direct commits to the default branch, #196)",
		"window_days", int(window/(24*time.Hour)),
		"cost_usd", store.MicroToDollars(act.CostMicro))
	return true
}

// evalZeroOutcome is the pure tripwire predicate: cost accrued in the window AND no
// outcome landed there. Split out so the decision is unit-testable without a DB,
// ticker, gauge, or clock.
func evalZeroOutcome(a store.WindowActivity) bool {
	return a.CostMicro > 0 && a.Outcomes == 0
}

// applyIntFromConfig mirrors applyStringFromConfig for an int-typed flag: it sets
// the flag from the config file only when the operator did not pass it on the CLI,
// it did not pick up an env-var default, and the config key was present (non-nil).
// Honours CLI > env > config > builtin default. A flag with no env-var layer (e.g.
// zero-outcome-window-days) simply passes an envVarSet that lacks its key, so the
// env dimension is a no-op for it.
func applyIntFromConfig(fs *flag.FlagSet, setFlags, envVarSet map[string]bool, name string, v *int) {
	if setFlags[name] || envVarSet[name] || v == nil {
		return
	}
	if err := fs.Set(name, strconv.Itoa(*v)); err != nil {
		fmt.Fprintf(os.Stderr, "config: apply %s: %v\n", name, err)
		os.Exit(1)
	}
}

// applyDurationFromConfig mirrors applyIntFromConfig for a duration-typed flag
// (#85: --auth-failure-window, --auth-lockout). The config value arrives as a
// STRING (config.AuthConfig documents why: go.yaml.in/yaml/v3 decodes a bare int
// into time.Duration as nanoseconds and won't decode "15m" at all), so this helper
// validates it with time.ParseDuration — the SAME parser the CLI flag.Duration uses
// — before applying, guaranteeing a config-sourced duration is parsed identically to
// a flag-sourced one. A malformed value (e.g. failure_window: "banana") fails loud at
// startup naming the config key rather than silently falling back to the default.
//
// It deliberately does NOT reject non-positive durations (e.g. "0s"): the >0
// sanity check is a resolved-value concern the caller enforces AFTER this in the
// existing MaxFailures>0 validation block, so a config combo like
// {max_failures: 5, lockout: "0s"} still trips that guard exactly as the flag path
// would. Honours CLI > env (none here) > config > builtin default.
func applyDurationFromConfig(fs *flag.FlagSet, setFlags, envVarSet map[string]bool, name string, v *string) {
	if setFlags[name] || envVarSet[name] || v == nil {
		return
	}
	d, err := parseConfigDuration(name, *v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if err := fs.Set(name, d.String()); err != nil {
		fmt.Fprintf(os.Stderr, "config: apply %s: %v\n", name, err)
		os.Exit(1)
	}
}

// parseConfigDuration parses a config-sourced duration string for the given flag
// name (#85). It is the SAME time.ParseDuration the CLI --auth-* duration flags use,
// so a value from the YAML file is validated byte-identically to one from the command
// line — the config-vs-flag divergence class is closed by construction. On failure it
// returns a descriptive error naming the config key and the offending value. Pure and
// unit-testable; applyDurationFromConfig exits non-zero on the error.
func parseConfigDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("config: %s: invalid duration %q: %w", name, value, err)
	}
	return d, nil
}

// validateAuthLockout enforces the auth-lockout coherence rule (#36) on the RESOLVED
// values (#85). A non-zero MaxFailures with a non-positive window or lockout would be
// a limiter that looks armed but can never trip — auth silently un-throttled — so it
// is a fail-loud startup error. Because the config wiring feeds the very same flag
// variables the CLI does, this validates a config-sourced combo identically to a
// flag-sourced one: an operator cannot smuggle a lockout-disabling value past it via
// YAML that the flag path would have rejected. Pure and unit-testable; the caller
// exits non-zero on error.
func validateAuthLockout(maxFailures int, window, lockout time.Duration) error {
	if maxFailures > 0 && (window <= 0 || lockout <= 0) {
		return fmt.Errorf("--auth-failure-window and --auth-lockout must be > 0 when --auth-max-failures > 0")
	}
	return nil
}

// envBool resolves a boolean flag's env-var default (#196). Empty/unset → false;
// otherwise strconv.ParseBool ("true/false/1/0/t/f"). A malformed value fails loud
// rather than silently defaulting — a typo'd TIER_PUSH_CAPTURE=yes must not quietly
// leave capture off (correct-from-start).
func envBool(name string) bool {
	v := os.Getenv(name)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s must be a boolean (true|false|1|0), got %q\n", name, v)
		os.Exit(1)
	}
	return b
}

// envIntDefault resolves an int-valued flag's env-var default (#185). Empty/unset
// → fallback; otherwise strconv.Atoi. A malformed value fails loud rather than
// silently falling back to the default — the same correct-from-start discipline as
// envBool, so a typo'd TIER_K_ANONYMITY=five can't quietly leave the floor at 5.
func envIntDefault(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s must be an integer, got %q\n", name, v)
		os.Exit(1)
	}
	return n
}

// resolveAggregationMode maps the resolved --aggregation string to a
// scoring.AggregationMode (#185, #270). Empty is a hard error — the setting is
// REQUIRED with no silent default (see the flag help and the CLAUDE.md compliance
// note) — and any value other than "team", "division", or "developer" is rejected
// with the same guidance. Pure and unit-testable (string in, mode-or-error out);
// the caller exits non-zero on error. It is the inverse of AggregationMode.String.
func resolveAggregationMode(s string) (scoring.AggregationMode, error) {
	switch s {
	case "team":
		return scoring.AggregationTeam, nil
	case "division":
		return scoring.AggregationDivision, nil
	case "developer":
		return scoring.AggregationDeveloper, nil
	case "":
		return 0, fmt.Errorf("--aggregation is REQUIRED and has no default: set it to 'team' (team-only aggregates) or 'division' (division-only aggregates, one level up) — both k-anonymized and never naming an individual, the safe choice under EU works-council / GDPR Art. 22 co-determination — or 'developer' (named per-developer rows). Provide it via --aggregation, the TIER_AGGREGATION env var, or the config key aggregation (#185, #270)")
	default:
		return 0, fmt.Errorf("--aggregation must be 'team', 'division', or 'developer', got %q (#185, #270)", s)
	}
}

// validateKAnonymity enforces the k-anonymity hard minimum (#185): a floor below
// scoring.MinKAnonymity (3) would leave an anonymity set small enough to single
// out an individual, so it is a fail-loud startup error. Pure and unit-testable;
// the caller exits non-zero on error.
func validateKAnonymity(k int) error {
	if k < scoring.MinKAnonymity {
		return fmt.Errorf("--k-anonymity must be >= %d, got %d (#185: a smaller cohort floor would defeat k-anonymity)", scoring.MinKAnonymity, k)
	}
	return nil
}

// applyBoolFromConfig sets a bool-typed flag from the config file when the operator
// did not pass it on the CLI AND it did not pick up its env-var default AND the
// config key was present (non-nil). Honours CLI > env > config > builtin default,
// the same precedence as applyStringFromConfig.
func applyBoolFromConfig(fs *flag.FlagSet, setFlags, envVarSet map[string]bool, name string, v *bool) {
	if setFlags[name] || envVarSet[name] || v == nil {
		return
	}
	if err := fs.Set(name, strconv.FormatBool(*v)); err != nil {
		fmt.Fprintf(os.Stderr, "config: apply %s: %v\n", name, err)
		os.Exit(1)
	}
}

// applyStringFromConfig sets a string-typed flag's value from the config
// file when ALL of:
//   - the operator did not pass the flag on the command line,
//   - the flag did not pick up a non-empty value from its env-var default,
//   - the config file actually had a value for the field (non-nil pointer).
//
// Honours the documented precedence: CLI > env > config > builtin default.
// fs.Set's error is surfaced rather than discarded — string-valued flags
// can't error today, but a future custom flag.Value with validation would
// silently swallow misconfiguration without this check.
func applyStringFromConfig(fs *flag.FlagSet, setFlags, envVarSet map[string]bool, name string, v *string) {
	if setFlags[name] || envVarSet[name] || v == nil {
		return
	}
	if err := fs.Set(name, *v); err != nil {
		fmt.Fprintf(os.Stderr, "config: apply %s: %v\n", name, err)
		os.Exit(1)
	}
}

// parseTrustedProxies converts the --trusted-proxy-cidr / http.trusted_proxy_cidrs
// string list into netip.Prefix values, failing loud on any malformed entry
// (#131) — a silent skip would leave an operator believing a proxy is trusted
// when it isn't. An empty input returns (nil, nil): no CIDRs means X-Forwarded-For
// is never trusted, the default byte-identical to the pre-#131 behavior. A bare
// IP (the most common mistake) is rejected with a message pointing at the /32
// or /128 fix rather than the raw netip parse error.
func parseTrustedProxies(in []string) ([]netip.Prefix, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(in))
	for _, c := range in {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			// A bare IP parses as an Addr but not a Prefix; nudge toward the fix.
			if _, aerr := netip.ParseAddr(c); aerr == nil {
				return nil, fmt.Errorf("--trusted-proxy-cidr %q: missing prefix length; use %s/32 (IPv4) or %s/128 (IPv6) for a single host", c, c, c)
			}
			return nil, fmt.Errorf("--trusted-proxy-cidr %q: %w", c, err)
		}
		// Reject IPv4-mapped-IPv6 prefixes (e.g. ::ffff:10.0.0.0/104): peers and
		// XFF hops are Unmap()'d to native form before the containment check, so
		// a mapped prefix would silently never match — a "trusted" proxy that is
		// never trusted. Fail loud toward the native form rather than accept a
		// CIDR that can't fire (#131).
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf("--trusted-proxy-cidr %q: IPv4-mapped IPv6 CIDR never matches; write the native IPv4 form (e.g. 10.0.0.0/8)", c)
		}
		out = append(out, p)
	}
	return out, nil
}

// resolveWatchRepos validates each --watch-repo argument: resolves to
// absolute, checks the directory exists and contains a .git entry. Fail-fast
// at startup beats a silent watcher that ingests nothing because every repo
// path was a typo.
//
// Accepts both .git as a directory (normal clone) and .git as a regular file
// (git worktree, submodule), since the watcher cares only about the working
// tree's existence, not the gitdir layout.
func resolveWatchRepos(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, r := range in {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("--watch-repo %q: %w", r, err)
		}
		if _, err := os.Stat(filepath.Join(abs, ".git")); err != nil {
			return nil, fmt.Errorf("--watch-repo %q: not a git repository (.git not found)", r)
		}
		out = append(out, abs)
	}
	return out, nil
}

// validateBind enforces the fail-closed exposure rule (#59): binding beyond
// loopback without an API token would expose per-developer spend, the score
// ranking, and an open relay to the upstream providers — so it is a startup
// error, not a warning. Hostnames other than "localhost" are refused in
// tokenless mode: what they resolve to can't be known statically, and
// fail-closed beats guessing. "localhost" itself is trusted by convention —
// an /etc/hosts that points it somewhere routable can defeat the check, and
// we accept that deliberately rather than break the most common dev
// invocation. Zoned IPv6 literals ([::1%lo0]) are refused, also
// deliberately: net.ParseIP rejects zones and the safe direction is to ask
// the operator for the plain form.
//
// This makes tokenless mode safe BY DEFAULT, not unconditionally: an SSH
// tunnel or container port-map (docker run -p 0.0.0.0:8080:8080) can still
// re-expose a loopback listener from outside the process.
//
// syntheticDemo (#476) is the ONE exemption: `tierd demo` serves an invented
// dataset in --read-only mode (every write/ingest/admin route is structurally
// absent), so a non-loopback bind exposes nothing real. That bit is set only by
// runDemo and is unreachable from any serve flag or env var, so a `serve` over
// REAL data on a non-loopback bind without a token is still refused — the whole
// point of routing the exemption through an in-code signal rather than a flag.
func validateBind(addr, apiToken string, syntheticDemo bool) error {
	if apiToken != "" || syntheticDemo {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %v", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to bind %q without an API token: a non-loopback listener would expose unauthenticated spend data and an open provider relay; set --api-token (or TIER_API_TOKEN) or bind to 127.0.0.1", addr)
}

// checkReadToken enforces the read/write token distinctness rule (#190): a
// read-only viewer token equal to the write api token would be classified as
// the WRITE scope (api.Handler.classify prefers write on a tie) and silently
// grant write access, defeating the least-privilege purpose of the read scope.
// Both empty, only one set, or two distinct non-empty values are all fine —
// only an equal, non-empty pair is refused. Pure and unit-testable; called
// after @file/env/config resolution so it sees the effective secrets.
func checkReadToken(apiToken, readToken string) error {
	if readToken == "" || apiToken == "" {
		return nil
	}
	if readToken == apiToken {
		return fmt.Errorf("--read-token must differ from --api-token: a read token equal to the write token would grant write access, defeating the read-only scope (#190)")
	}
	return nil
}

// resolveSecretFlag supports `@/path/to/file` indirection for secret-valued
// flags — --api-token, --read-token, and --webhook-secret (#37). Passing a secret as a literal
// flag value leaks it to `ps aux`, shell history, and process-accounting logs on
// a multi-user host. A value beginning with '@' is instead read from the file at
// the remaining path (a single trailing newline is trimmed), so the secret never
// appears on the command line. Any other value is returned unchanged, preserving
// the env-var default and the literal-value path for laptop testing. A literal
// secret that genuinely begins with '@' is unsupported — API tokens and webhook
// secrets are base64/hex and never start with '@'; use the env var for that edge.
func resolveSecretFlag(raw string) (string, error) {
	if !strings.HasPrefix(raw, "@") {
		return raw, nil
	}
	path := raw[1:]
	if path == "" {
		return "", fmt.Errorf("empty file path after '@'")
	}
	// Reject non-regular files up front so `@/dev/zero` (would read forever) or
	// `@/some/dir` fails fast with a clear message instead of hanging or
	// surfacing a confusing read error.
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat secret file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %q is not a regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", path, err)
	}
	// Trim trailing CR/LF — `echo secret > file` or an editor appends one (and
	// TrimRight collapses an accidental run of them). Other trailing whitespace
	// is preserved in case it is genuinely part of the secret.
	secret := strings.TrimRight(string(b), "\r\n")
	// The '@' prefix is unambiguous "a secret lives in this file" intent, so an
	// empty result is a misconfiguration (truncated mount, not-yet-populated
	// secret), NOT a request to disable auth. Fail closed — an empty token would
	// silently turn off the Bearer gate and relax the non-loopback bind check.
	if secret == "" {
		return "", fmt.Errorf("secret file %q is empty", path)
	}
	return secret, nil
}

// anthropicAdminSettings is the resolved, validated Anthropic Admin poller config
// (#138): the Admin key already de-referenced from any @file, the org, and a
// parsed poll interval. nil means the poller is disabled.
type anthropicAdminSettings struct {
	apiKey   string
	org      string
	interval time.Duration
}

// minAnthropicAdminInterval is the config floor for poll_interval. Below this the
// org poll adds provider load without materially fresher settled-day data
// (settlement lags a full day regardless of poll cadence).
const minAnthropicAdminInterval = 5 * time.Minute

// resolveAnthropicAdminConfig validates the collectors.anthropic_admin block and
// resolves its secret. Returns (nil, nil) when the block is absent (disabled). A
// present block MUST carry a non-empty api_key (after @file resolution) and org;
// poll_interval defaults to 1h and must parse to >= 5m. Any violation is a
// fail-fast startup error (correct-from-start: a misconfigured poller is louder
// than a silently disabled one). The resolved key is never logged.
func resolveAnthropicAdminConfig(c *config.AnthropicAdminConfig) (*anthropicAdminSettings, error) {
	if c == nil {
		return nil, nil // block absent → disabled
	}
	rawKey := ""
	if c.APIKey != nil {
		rawKey = *c.APIKey
	}
	key, err := resolveSecretFlag(rawKey)
	if err != nil {
		return nil, fmt.Errorf("collectors.anthropic_admin.api_key: %w", err)
	}
	if key == "" {
		return nil, fmt.Errorf("collectors.anthropic_admin: api_key is required when the block is present")
	}
	org := ""
	if c.Org != nil {
		org = *c.Org
	}
	if org == "" {
		return nil, fmt.Errorf("collectors.anthropic_admin: org is required when the block is present")
	}
	interval := anthropicadmin.DefaultPollInterval
	if c.PollInterval != nil && *c.PollInterval != "" {
		d, err := time.ParseDuration(*c.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("collectors.anthropic_admin.poll_interval: %w", err)
		}
		interval = d
	}
	if interval < minAnthropicAdminInterval {
		return nil, fmt.Errorf("collectors.anthropic_admin.poll_interval must be >= %s, got %s", minAnthropicAdminInterval, interval)
	}
	return &anthropicAdminSettings{apiKey: key, org: org, interval: interval}, nil
}

// openAIUsageSettings is the resolved, validated OpenAI Usage/Costs poller
// config (#139) — the structural twin of anthropicAdminSettings. nil means the
// poller is disabled.
type openAIUsageSettings struct {
	apiKey   string
	org      string
	interval time.Duration
}

// minOpenAIUsageInterval is the config floor for poll_interval, same rationale
// as minAnthropicAdminInterval: below this the org poll adds provider load
// without materially fresher settled-day data (settlement lags a full day
// regardless of poll cadence).
const minOpenAIUsageInterval = 5 * time.Minute

// resolveOpenAIUsageConfig validates the collectors.openai_usage block and
// resolves its secret. Mirrors resolveAnthropicAdminConfig exactly: (nil, nil)
// when the block is absent (disabled); a present block MUST carry a non-empty
// api_key (after @file resolution) and org; poll_interval defaults to 1h and
// must parse to >= 5m. Any violation is a fail-fast startup error
// (correct-from-start: a misconfigured poller is louder than a silently
// disabled one). The resolved key is never logged.
func resolveOpenAIUsageConfig(c *config.OpenAIUsageConfig) (*openAIUsageSettings, error) {
	if c == nil {
		return nil, nil // block absent → disabled
	}
	rawKey := ""
	if c.APIKey != nil {
		rawKey = *c.APIKey
	}
	key, err := resolveSecretFlag(rawKey)
	if err != nil {
		return nil, fmt.Errorf("collectors.openai_usage.api_key: %w", err)
	}
	if key == "" {
		return nil, fmt.Errorf("collectors.openai_usage: api_key is required when the block is present")
	}
	org := ""
	if c.Org != nil {
		org = *c.Org
	}
	if org == "" {
		return nil, fmt.Errorf("collectors.openai_usage: org is required when the block is present")
	}
	interval := openaiusage.DefaultPollInterval
	if c.PollInterval != nil && *c.PollInterval != "" {
		d, err := time.ParseDuration(*c.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("collectors.openai_usage.poll_interval: %w", err)
		}
		interval = d
	}
	if interval < minOpenAIUsageInterval {
		return nil, fmt.Errorf("collectors.openai_usage.poll_interval must be >= %s, got %s", minOpenAIUsageInterval, interval)
	}
	return &openAIUsageSettings{apiKey: key, org: org, interval: interval}, nil
}

// codexRolloutSettings is the resolved, validated Codex rollout-log collector
// config (#464). nil means the collector is disabled.
type codexRolloutSettings struct {
	sessionsDir string // "" → the collector's ~/.codex/sessions default
	interval    time.Duration
}

// resolveCodexRolloutConfig validates the collectors.codex_rollout block and the
// --codex-rollout flag into one decision.
//
// Enablement is an OR: either the flag or the presence of the config block turns
// the collector on. It differs from the two org pollers on purpose — those need a
// credential, so a bare flag could not enable them, while this one reads local
// files that need no secret. The block exists only to override the defaults, so
// requiring it in order to use the flag would be ceremony with no safety value.
//
// A present-but-invalid block is a fail-fast startup error (correct-from-start: a
// misconfigured collector must be louder than a silently disabled one), even when
// the flag alone would have enabled it — an operator who wrote scan_interval: "2s"
// needs to be told, not quietly given 5m.
func resolveCodexRolloutConfig(c *config.CodexRolloutConfig, flagEnabled bool) (*codexRolloutSettings, error) {
	if c == nil && !flagEnabled {
		return nil, nil // neither flag nor block → disabled
	}
	s := &codexRolloutSettings{interval: codexrollout.DefaultScanInterval}
	if c == nil {
		return s, nil
	}
	if c.SessionsDir != nil {
		s.sessionsDir = strings.TrimSpace(*c.SessionsDir)
	}
	if c.ScanInterval != nil && *c.ScanInterval != "" {
		d, err := time.ParseDuration(*c.ScanInterval)
		if err != nil {
			return nil, fmt.Errorf("collectors.codex_rollout.scan_interval: %w", err)
		}
		s.interval = d
	}
	if s.interval < codexrollout.MinScanInterval {
		return nil, fmt.Errorf("collectors.codex_rollout.scan_interval must be >= %s, got %s", codexrollout.MinScanInterval, s.interval)
	}
	return s, nil
}

// parseProxyTarget parses a URL string for use as a proxy target.
// Returns nil if the URL is empty, malformed, or missing a valid http/https scheme.
func parseProxyTarget(rawURL string, logger *slog.Logger) *url.URL {
	if rawURL == "" {
		return nil
	}
	t, err := url.Parse(rawURL)
	if err != nil || t.Host == "" || (t.Scheme != "http" && t.Scheme != "https") {
		logger.Warn("invalid proxy target URL, skipping", "url", rawURL)
		return nil
	}
	return t
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tier.db"
	}
	return filepath.Join(home, ".tier", "tier.db")
}
