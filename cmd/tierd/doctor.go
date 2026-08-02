package main

// tierd doctor (#236): the install-fidelity self-check. An adopter running a
// partial rollout (10 of 50 developers shipping) otherwise discovers a capture
// gap only as a mysteriously great or terrible TIER score, with no answer to "is
// it working?" short of reading SQLite. doctor runs the same local JSONL discovery
// and attribution `tierd score`/`ship` do, plus an optional round-trip to a central
// tierd, and reports each signal as OK / WARN / FAIL. It exits non-zero when any
// check FAILs, so it drops straight into a post-install acceptance step.
//
// Split like runBackup: it takes injected writers and returns an exit code so the
// no-network local checks and the pure eval helpers are unit-testable without
// os.Exit, a real server, or a real ~/.claude tree.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

// doctorSinceDays is the look-back window for the "sessions parsed recently" and
// "branch-to-issue extraction rate" checks. 7 days matches the issue spec and is
// long enough to span a weekend gap yet short enough that a shipper that died
// mid-week still trips the check.
const doctorSinceDays = 7

// defaultMinAttribution is the default FAIL floor for the NAMED-BRANCH attribution
// rate (#352, rescoped by #488): of the dollars spent on feature branches
// (attributed + branch-without-issue), the fraction that resolved to an issue. Below
// it doctor FAILs (non-zero exit). It applies ONLY to branch work — the part
// branch-naming discipline actually controls — NOT to exploratory spend on main,
// which legitimately cannot attribute and is reported separately as information. The
// old floor spanned ALL spend, so it was unreachable on any repo where roughly half
// of agentic work happens on main (planning/orchestration) — a false alarm, not a
// gate (#488). A low named-branch rate means branch names don't carry an issue
// number (feature/foo instead of feature/236-foo). Overridable per-run with
// --min-attribution; the 0.5 default is maintainer-overturnable.
const defaultMinAttribution = 0.5

// attributionHealthyAtOrAbove is the "healthy" bar for the named-branch attribution
// rate: at or above this fraction the check is OK; between the floor and this bar it
// is a WARN (yellow, still exit 0) — branch naming is working but has headroom. It is
// a fixed constant, not a flag: the OK-vs-WARN line stays consistent across installs
// while the FAIL floor is the single tunable knob. When an operator raises the floor
// above this bar the WARN band simply closes (a rate is then either below the floor
// -> FAIL or at/above it -> OK). Set high (#488): branch naming is fully controllable,
// so healthy discipline ties nearly all feature-branch spend to an issue.
const attributionHealthyAtOrAbove = 0.9

// doctorClockSkewWarn is the server-vs-local clock offset above which doctor WARNs.
// A laptop shipper and a central tierd disagreeing by more than this mis-window
// the 14-day attribution look-back (#136) and every since/until bound; 2 minutes
// is well inside NTP tolerance yet catches a wrong-timezone or unset clock.
const doctorClockSkewWarn = 2 * time.Minute

// checkStatus is the severity of one doctor check. Ordering matters: the process
// exit code is non-zero iff any result reached statusFail.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

func (s checkStatus) label() string {
	switch s {
	case statusOK:
		return "OK"
	case statusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// checkResult is one line of the doctor report. hint is a fix suggestion printed
// under a WARN/FAIL row; it is empty for OK rows.
type checkResult struct {
	name   string
	status checkStatus
	detail string
	hint   string
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to the git repository whose Claude Code sessions to check")
	repoSlug := fs.String("repo-slug", "", `canonical "owner/repo" identity for --repo (#231); omit to read remote.origin.url`)
	claudeDir := fs.String("claude-dir", "", "override ~/.claude directory (for testing)")
	developer := fs.String("developer", "", "developer ID override (default: OS username)")
	server := fs.String("server", "", "central tierd base URL to round-trip against, e.g. https://tier.example (optional)")
	apiToken := fs.String("api-token", os.Getenv("TIER_API_TOKEN"), "API token for --server, sent as Authorization: Bearer. Prefer TIER_API_TOKEN or @/path/to/file (#37)")
	minAttribution := fs.Float64("min-attribution", defaultMinAttribution, "named-branch attribution floor in [0,1]: below this fraction of feature-branch SPEND resolving to an issue, the attribution check FAILs (non-zero exit) instead of merely warning (#352, #488). Exploratory spend on main is excluded and reported separately")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	// Positive-range test (not `< 0 || > 1`) so a NaN — which strconv.ParseFloat
	// accepts and against which every ordered comparison is false — is REJECTED
	// rather than slipping past into every `rate < minAttribution` being false,
	// which would silently defeat the FAIL floor this command exists to enforce.
	if !(*minAttribution >= 0 && *minAttribution <= 1) {
		_, _ = fmt.Fprintf(stderr, "doctor: --min-attribution must be in [0,1], got %g\n", *minAttribution)
		return 1
	}

	// One SIGINT/SIGTERM-cancellable context for both the local collect and the
	// server round-trip, so Ctrl-C interrupts a hung request too (a CI acceptance
	// step must be interruptible).
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var results []checkResult

	// The ~/.claude/projects check is independent of --repo, so it runs
	// UNCONDITIONALLY — a wrong --repo must not hide "Claude Code never wrote a
	// session", the most basic capture failure.
	results = append(results, checkClaudeProjects(*claudeDir))

	// --- Local checks: git identity + JSONL capture path ---
	repoPath, err := resolveRepo(*repo)
	if err != nil {
		results = append(results, checkResult{
			name:   "git repository",
			status: statusFail,
			detail: fmt.Sprintf("cannot resolve --repo %q: %v", *repo, err),
			hint:   "run tierd doctor from inside your git repo, or pass --repo /path/to/repo",
		})
	} else {
		results = append(results, checkGitRepo(repoPath, *repoSlug))

		// Discover and attribute local sessions over the recent window, exactly as
		// `tierd score`/`ship` do, so doctor validates the SAME path capture uses.
		since := time.Now().AddDate(0, 0, -doctorSinceDays).UTC()
		c := &collector.JSONLCollector{
			RepoPath:    repoPath,
			ClaudeDir:   *claudeDir,
			DeveloperID: *developer,
			RepoSlug:    *repoSlug,
		}
		events, collectErr := c.Collect(ctx, since)
		results = append(results, evalCapture(events, collectErr, doctorSinceDays, *minAttribution)...)
	}

	// --- Server checks: reachability, auth, clock offset, price-table parity ---
	if *server != "" {
		token, err := resolveSecretFlag(*apiToken)
		if err != nil {
			results = append(results, checkResult{
				name:   "server auth token",
				status: statusFail,
				detail: fmt.Sprintf("--api-token: %v", err),
				hint:   "check the @file path or TIER_API_TOKEN value",
			})
		} else {
			// Bounded client: a hung central tierd must not hang the acceptance step.
			client := &http.Client{Timeout: doctorServerTimeout}
			results = append(results, checkServer(ctx, client, *server, token, time.Now())...)
		}
	} else {
		// An invisible check reads as a passing one — the same reasoning this file
		// applies to a missing Date header. The identity join (#496) can only be
		// evaluated against a store, so without --server it is NOT checked, and
		// ending the report with "all checks passed" would be false confidence
		// about the single failure mode most likely to be silently wrong.
		//
		// The operator most exposed is precisely the one least likely to pass
		// --server: a single-machine self-hosted user, whose OS username IS the
		// cost-side identity while their outcomes carry a GitHub login.
		results = append(results, checkResult{
			name:   "identity join",
			status: statusWarn,
			detail: "not checked — cost/outcome identity mapping can only be verified against a running tierd",
			hint:   "re-run with --server http://127.0.0.1:8080 (or your tierd URL) to confirm your captured spend actually joins your merged-PR outcomes",
		})
	}

	return reportDoctor(results, stdout)
}

// doctorServerTimeout bounds the single /scores round-trip. 30s matches the
// backfill command's GitHub client and is generous for a health probe while still
// failing a wedged server rather than hanging forever.
const doctorServerTimeout = 30 * time.Second

// checkGitRepo reports whether the repo has a canonical owner/repo identity (#231).
// A resolvable slug is OK; falling back to the unqualified sentinel is a WARN, not
// a FAIL — cost still attributes per developer, but multi-repo issues sharing a
// number will fuse, so it is worth surfacing.
func checkGitRepo(repoPath, repoSlugOverride string) checkResult {
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		return checkResult{
			name:   "git repository",
			status: statusFail,
			detail: fmt.Sprintf("%s is not a git repository (.git not found)", repoPath),
			hint:   "point --repo at a git working tree; TIER joins sessions to git history",
		}
	}
	if repoSlugOverride != "" {
		return checkResult{name: "git remote", status: statusOK, detail: fmt.Sprintf("repo slug overridden: %s", repoSlugOverride)}
	}
	if slug := collector.RepoSlugFromGitConfig(repoPath); slug != "" {
		return checkResult{name: "git remote", status: statusOK, detail: fmt.Sprintf("remote.origin.url resolves to %s", slug)}
	}
	return checkResult{
		name:   "git remote",
		status: statusWarn,
		detail: "no canonical owner/repo slug (no remote.origin.url); cost rows will be repo-unqualified",
		hint:   "add a git remote, or pass --repo-slug owner/repo (REQUIRED on a fork — origin names the fork, not the upstream webhook)",
	}
}

// checkClaudeProjects verifies the ~/.claude/projects tree exists — the source of
// every JSONL session. Missing it means Claude Code has never written a session
// (or CLAUDE_CONFIG_DIR points elsewhere), so capture cannot work at all.
func checkClaudeProjects(claudeDirOverride string) checkResult {
	dir := claudeDirOverride
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return checkResult{
				name:   "claude projects dir",
				status: statusFail,
				detail: fmt.Sprintf("cannot determine home directory: %v", err),
				hint:   "set --claude-dir to your ~/.claude location",
			}
		}
		dir = filepath.Join(home, ".claude")
	}
	projects := filepath.Join(dir, "projects")
	if fi, err := os.Stat(projects); err != nil || !fi.IsDir() {
		return checkResult{
			name:   "claude projects dir",
			status: statusFail,
			detail: fmt.Sprintf("%s not found", projects),
			hint:   "run Claude Code at least once so it writes session files, or point --claude-dir at the right location",
		}
	}
	return checkResult{name: "claude projects dir", status: statusOK, detail: projects}
}

// evalCapture turns a completed local collection into the recent-sessions and
// attribution-rate checks. minAttribution is the FAIL floor (#352): below it the
// attribution check is a FAIL (non-zero exit), between it and attributionHealthyAtOrAbove
// a WARN, and at/above the healthy bar an OK. Pure (events + error + floor in,
// results out) so the healthy band, both boundaries, and every failure mode are
// table-testable without a real ~/.claude tree.
func evalCapture(events []collector.TokenEvent, collectErr error, sinceDays int, minAttribution float64) []checkResult {
	if collectErr != nil {
		return []checkResult{{
			name:   "recent sessions",
			status: statusFail,
			detail: fmt.Sprintf("collect failed: %v", collectErr),
			hint:   "check --claude-dir and that the JSONL files are readable",
		}}
	}
	if len(events) == 0 {
		return []checkResult{{
			name:   "recent sessions",
			status: statusWarn,
			detail: fmt.Sprintf("no Claude Code sessions parsed in the last %dd for this repo", sinceDays),
			hint:   "confirm you have used Claude Code in this repo recently, and that --repo matches the working directory you used",
		}}
	}
	out := []checkResult{{
		name:   "recent sessions",
		status: statusOK,
		detail: fmt.Sprintf("%d events parsed in the last %dd", len(events), sinceDays),
	}}
	// Two attribution signals (#488), both in DOLLARS so no line mixes a rate with
	// dollar figures. The FAIL is the NAMED-BRANCH rate — of feature-branch spend,
	// how much tied to an issue — the only part branch-naming discipline controls.
	// Exploratory spend on main is reported separately as information, never a FAIL:
	// planning/orchestration on main legitimately spans issues and cannot attribute,
	// so folding it into one floor (as before) both made the floor unreachable and
	// mislabeled correct behavior as waste.
	dollars := attributionByDollar(events)
	out = append(out, attributionResult(dollars, minAttribution))
	if info := exploratoryResult(dollars); info != nil {
		out = append(out, *info)
	}
	return out
}

// attributionDollars decomposes recent events into per-bucket dollar sums — the
// basis for doctor's two attribution signals (#488). Dollars, not event counts, so
// every attribution sentence stays in one unit and matches how the score actually
// weights cost.
type attributionDollars struct {
	attributed int64 // resolved to a real issue id (named branch / header / env)
	noIssue    int64 // on a branch with no parseable issue number — FIXABLE by naming
	main       int64 // planning/orchestration on main — legitimately spans issues
	detached   int64 // detached HEAD / worktree-agent spend (see #490)
	other      int64 // bare "unattributed" sentinel — uncategorized remainder
}

// branchWork is the spend branch-naming discipline governs: work that resolved to
// an issue, plus work on a branch we could not tie to one. Exploratory main/detached
// spend is excluded — naming a branch cannot attribute it.
func (a attributionDollars) branchWork() int64 { return a.attributed + a.noIssue }

// exploratory is spend that legitimately does not attribute to a single issue:
// planning on main, detached-HEAD/worktree work, and any uncategorized remainder.
func (a attributionDollars) exploratory() int64 { return a.main + a.detached + a.other }

// attributionByDollar sorts each event's cost into its bucket. The unattributed
// FAMILY (base sentinel + labeled members) is matched with IsUnattributed FIRST so a
// labeled bucket is never miscounted as an attributed issue (#refocus); the exact
// members are then split out, with the bare sentinel or any future member falling to
// "other".
func attributionByDollar(events []collector.TokenEvent) attributionDollars {
	var a attributionDollars
	for _, e := range events {
		switch {
		case !collector.IsUnattributed(e.IssueID):
			a.attributed += e.CostMicro
		case e.IssueID == collector.UnattributedNoIssue:
			a.noIssue += e.CostMicro
		case e.IssueID == collector.UnattributedMain:
			a.main += e.CostMicro
		case e.IssueID == collector.UnattributedDetachedHEAD:
			a.detached += e.CostMicro
		default:
			a.other += e.CostMicro
		}
	}
	return a
}

// attributionResult is the named-branch attribution signal (#488): of the dollars
// spent on feature branches (attributed + branch-without-issue), the share that
// resolved to an issue — the ONLY part branch-naming discipline controls. FAIL below
// minAttribution, WARN up to the healthy bar, OK at/above it. Every figure is a
// dollar amount, and the message never implies unattributed == unproductive.
func attributionResult(a attributionDollars, minAttribution float64) checkResult {
	branch := a.branchWork()
	if branch == 0 {
		// No feature-branch spend to attribute in this window — no coverage gap that
		// discipline could close. Any spend that exists was exploratory; the
		// exploratory row (if present) reports it.
		return checkResult{
			name:   "issue attribution",
			status: statusOK,
			detail: "no feature-branch spend to attribute in this window",
		}
	}
	rate := float64(a.attributed) / float64(branch)
	base := fmt.Sprintf("$%.2f of $%.2f spent on feature branches is tied to an issue (%.0f%%)",
		store.MicroToDollars(a.attributed), store.MicroToDollars(branch), rate*100)
	noIssueUSD := store.MicroToDollars(a.noIssue)
	nameHint := "name branches feature/<issue-number>-slug so TIER can extract the issue (see docs/how-it-works.md#5-how-it-gives-purpose-linking-cost--outcome)"
	switch {
	case rate < minAttribution:
		return checkResult{
			name:   "issue attribution",
			status: statusFail,
			detail: fmt.Sprintf("%s; $%.2f was on branches with no parseable issue number", base, noIssueUSD),
			hint:   nameHint + "; or lower --min-attribution if partial branch coverage is acceptable for this install",
		}
	case rate < attributionHealthyAtOrAbove:
		return checkResult{
			name:   "issue attribution",
			status: statusWarn,
			detail: fmt.Sprintf("%s — above the %.0f%% floor but below the %.0f%% healthy bar; $%.2f was on branches with no parseable issue number", base, minAttribution*100, attributionHealthyAtOrAbove*100, noIssueUSD),
			hint:   nameHint,
		}
	default:
		return checkResult{
			name:   "issue attribution",
			status: statusOK,
			detail: base,
		}
	}
}

// exploratoryResult reports exploratory spend — planning/orchestration on main,
// detached-HEAD/worktree work, and any uncategorized remainder — as INFORMATION,
// never a FAIL (#488). This cost legitimately spans several issues (or none), stays
// in the score's denominator by design, and is not a branch-naming problem to fix;
// the old single floor conflated it with the fixable gap and so mislabeled correct
// behavior as waste. Returns nil when there is no exploratory spend.
func exploratoryResult(a attributionDollars) *checkResult {
	if a.exploratory() == 0 {
		return nil
	}
	parts := make([]string, 0, 3)
	if a.main > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f on main (planning/orchestration)", store.MicroToDollars(a.main)))
	}
	if a.detached > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f on a detached HEAD/worktree", store.MicroToDollars(a.detached)))
	}
	if a.other > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f uncategorized", store.MicroToDollars(a.other)))
	}
	return &checkResult{
		name:   "exploratory spend",
		status: statusOK,
		detail: strings.Join(parts, ", ") + " — stays in the denominator by design; it never joins outcomes, and is not a branch-naming gap to fix",
	}
}

// checkServer performs the single round-trip to a central tierd — GET
// /api/v1/scores, which is read-scoped, always carries a price_table stamp, and
// returns the HTTP Date header — then delegates the verdicts to evalServerResponse.
// One request substantiates reachability, auth, clock offset, and price-table
// parity at once.
func checkServer(ctx context.Context, client *http.Client, server, token string, localNow time.Time) []checkResult {
	u := strings.TrimRight(server, "/") + "/api/v1/scores"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return []checkResult{{
			name:   "server reachable",
			status: statusFail,
			detail: fmt.Sprintf("bad --server URL %q: %v", server, err),
			hint:   "pass a full base URL, e.g. https://tier.example",
		}}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return []checkResult{{
			name:   "server reachable",
			status: statusFail,
			detail: fmt.Sprintf("GET %s: %v", u, err),
			hint:   "check the URL, that tierd serve is running, and any firewall/TLS between here and it",
		}}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return evalServerResponse(resp, body, localNow, store.ActivePriceTableInfo().Version)
}

// parseServerDate parses an HTTP Date header, returning ok=false when it is empty
// or malformed so the caller can surface a visible "cannot check" WARN rather than
// silently dropping the clock check.
func parseServerDate(dateHdr string) (time.Time, bool) {
	if dateHdr == "" {
		return time.Time{}, false
	}
	t, err := http.ParseTime(dateHdr)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// scoresResponse is the minimal projection of a /scores response doctor reads:
// the price-table provenance stamp every response carries (#233), plus the
// data-quality signals that reveal a broken cost<->outcome join (#496).
type scoresResponse struct {
	PriceTable struct {
		Version int `json:"version"`
	} `json:"price_table"`
	Total struct {
		WeightedPoints float64 `json:"weighted_points"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
	} `json:"total"`
	DataQuality struct {
		// AttributedOutcomeShare is the fraction of outcomes whose canonical
		// (developer, repo, issue) has ANY matching spend (#351).
		//
		// POINTER, mirroring the server (handler.go: `*float64` + omitempty).
		// Three wire states exist and two mean OPPOSITE things: present-and-0.0
		// is "nothing joined, alarm"; ABSENT is "the server said nothing".
		// Decoding into a plain float64 collapses them, so a pre-#351 tierd —
		// which emits no data_quality at all — would read as a total identity
		// split and FAIL doctor against a perfectly healthy server. doctor is a
		// client that can point at any tierd, and version skew is the normal
		// case (this file already carries a whole check for it).
		AttributedOutcomeShare *float64           `json:"attributed_outcome_share"`
		UnjoinedDevelopers     unjoinedDevelopers `json:"unjoined_developers"`
		// CostCoverageStart is the installation's COST HORIZON (#512): the RFC3339
		// instant of the earliest captured token event. Empty when the server
		// predates #512 or holds no cost at all — two states this check must
		// separate, because one is version skew and the other is a fresh install.
		CostCoverageStart string `json:"cost_coverage_start"`
		// WindowPredatesCostCapture is true when the scored window starts before
		// that horizon, so the response's TIER is structurally inflated.
		//
		// POINTER for the same reason as AttributedOutcomeShare above, and the
		// server is deliberate about it too: it emits an explicit FALSE rather
		// than omitting the field, precisely so "we checked, the window is
		// covered" survives the wire as a distinct state from "said nothing". A
		// plain bool here would discard exactly that distinction on arrival and
		// make doctor report a pre-#512 server as covered.
		WindowPredatesCostCapture *bool `json:"window_predates_cost_capture"`
		// CostCoverageSafeSince is the earliest `since` that will not predate the
		// horizon, precomputed server-side. doctor prints it verbatim rather than
		// deriving a date from CostCoverageStart: the horizon is an instant and
		// `since` is a date, so the horizon's own day is usually still hours too
		// early, and a remedy that does not clear the warning it accompanies is
		// worse than no remedy at all.
		CostCoverageSafeSince string `json:"cost_coverage_safe_since"`
		// SourceCoverageStart is the horizon at per-capture-path grain, emitted
		// only when 2+ sources have recorded cost. The global horizon above is
		// the LOOSEST bound: a window can clear it and still predate one source.
		SourceCoverageStart map[string]string `json:"source_coverage_start"`
	} `json:"data_quality"`
	// Since is the window start the server actually scored ("2006-01-02"), which
	// is what the horizon must be compared against — NOT any window doctor
	// believes it asked for. doctor sends no since= and takes the server default,
	// so this is the only reliable statement of what was measured.
	Since string `json:"since"`
}

// unjoinedDevelopers mirrors the server's unjoined_developers block. Named (not
// anonymous) because tests must CONSTRUCT it: anonymous struct types are
// identical only when field names, types and tag strings match byte-for-byte, so
// an inline type forces every test to restate it and breaks on a tag edit.
//
// Value, not pointer: the zero value already means "nothing unjoined", which is
// the useful default, and every consumer only asks len() > 0.
type unjoinedDevelopers struct {
	// Names, suppressed by the server in team (#185) and division (#270)
	// aggregation modes.
	CostOnly    []string `json:"cost_only"`
	OutcomeOnly []string `json:"outcome_only"`
	// Name-free magnitudes, emitted in EVERY mode so the signal survives
	// anonymization. Without these an operator on a team-mode server would see
	// that something is unjoined but never how much.
	CostOnlyCount    int `json:"cost_only_count"`
	OutcomeOnlyCount int `json:"outcome_only_count"`
}

// attributedOutcomeLowCoverage is the share below which doctor notes that few
// outcomes have matching spend. This is a COVERAGE statement, never an identity
// verdict — see checkIdentityJoin for why the two must not be conflated.
//
// Deliberately low (0.15), not the 0.5 that reads naturally. A share well under
// half is the STEADY STATE of a healthy install: `backfill` imports 90 days of
// merged PRs while Claude Code retains ~30 days of JSONL, so ~2/3 of outcomes
// have their spend permanently off-disk and can never join. Every outcome that
// merged before capture was installed is unjoinable forever. A 0.5 bar would
// therefore WARN on nearly every correct install — the same defect as #488,
// where a floor no correct user can clear trains people to ignore the tool.
const attributedOutcomeLowCoverage = 0.15

// checkIdentityJoin reports whether captured cost actually JOINS to recorded
// outcomes (#496).
//
// This is the highest-value check doctor can run, because its failure mode is
// invisible in the headline. Cost events carry the OS username (a session file
// knows nothing else); outcomes carry the GitHub login (what the webhook and
// backfill record). For most people those differ — `asmith` vs `a-smith-gh`
// here — and when they do, NOTHING joins and every per-developer TIER is 0.
//
// The trap is that the org total still looks right: ComputeTeam sums points and
// costs INDEPENDENTLY, so it never requires the join to succeed. A real multi-repo
// fleet run reported a plausible fleet TIER while 100% of its 851 outcomes were
// unjoined. Reading the aggregate as validation is the natural mistake, and the
// remedy already exists (POST /api/v1/developer_alias) — the only thing missing
// was anything that SAYS so.
//
// THE DISCRIMINATOR IS unjoined_developers, NOT the share. A low or even zero
// attributed_outcome_share does NOT imply an identity problem: one correctly
// configured developer reaches share=0 simply by having spend on an in-flight
// issue and an outcome that merged before capture existed. FAILing on the share
// tells a healthy install to alias a developer to itself, which the server
// rejects as a self-map — a red build with no way to make it green.
//
// A genuine split is instead exactly: some developer has cost and no outcomes,
// AND some other developer has outcomes and no cost. The server computes both
// sides (#351/#125) and emits name-free counts even under team/division
// anonymization, so the verdict survives every aggregation mode.
//
// Severity does not key off the share either: a fleet where three people join
// cleanly and ten are split is still broken, and bot identities (dependabot, CI
// committers) join by construction because their name is identical on both
// sides. An exact-zero trigger would let one bot downgrade a real fleet-wide
// split to a warning and a green CI.
//
// Scoped deliberately: a nil share means the server reported no outcomes at all
// (it is *float64 + omitempty precisely for this), so there is nothing to join
// yet — a fresh install, not a fault, and the check stays silent.
func checkIdentityJoin(s scoresResponse) []checkResult {
	share := s.DataQuality.AttributedOutcomeShare
	if share == nil || s.Total.TotalCostUSD <= 0 {
		return nil
	}
	u := s.DataQuality.UnjoinedDevelopers

	// Names are suppressed under k-anonymity; counts always survive. Treat
	// either as evidence so the check works in every aggregation mode.
	costSide := max(len(u.CostOnly), u.CostOnlyCount)
	outcomeSide := max(len(u.OutcomeOnly), u.OutcomeOnlyCount)

	if costSide > 0 && outcomeSide > 0 {
		detail := fmt.Sprintf("%d developer(s) have cost but no outcomes, and %d have outcomes but no cost — their spend and results never meet, so those TIER scores read 0",
			costSide, outcomeSide)
		if len(u.CostOnly) > 0 && len(u.OutcomeOnly) > 0 {
			detail += fmt.Sprintf(" (cost: %s; outcomes: %s)",
				joinSafe(u.CostOnly), joinSafe(u.OutcomeOnly))
		}
		return []checkResult{{
			name:   "identity join",
			status: statusFail,
			detail: detail,
			hint:   aliasRemedy(u) + ". NOTE: the org total still looks plausible because it sums cost and points independently — it is NOT evidence the join worked",
		}}
	}

	// No split. Whatever the share is, it is a coverage statement about window
	// overlap, and must not be worded as an identity fault.
	if *share < attributedOutcomeLowCoverage {
		return []checkResult{{
			name:   "outcome coverage",
			status: statusWarn,
			detail: fmt.Sprintf("%s of outcomes have matching spend; identities line up, so this is window overlap, not a mapping fault", pctStr(*share)),
			// Deliberately says "before capture was installed", NOT "logs expired"
			// (#512). Extracted events are append-only and outlive the provider
			// session logs they came from, so retention is the wrong mechanism and
			// naming it sends the operator to a setting that cannot help. The real
			// bound is the installation's cost horizon, which the cost-horizon check
			// below reports as a date.
			hint:   "outcomes that merged before capture was installed can never join — no retention setting moves that date backwards. Narrow the window (?since=) to the cost horizon reported below to compare like with like",
		}}
	}
	return []checkResult{{
		name:   "identity join",
		status: statusOK,
		detail: fmt.Sprintf("identities line up; %s of outcomes have matching spend", pctStr(*share)),
	}}
}

// checkCostHorizon reports whether the scored window is actually covered by
// captured cost (#512).
//
// The COST HORIZON is the date this installation began capturing spend. Outcomes
// backfill freely; cost does not. A window reaching back past the horizon divides
// a FULL window of outcomes by a PARTIAL window of cost and reports a silently
// INFLATED TIER — about twice on a real multi-repo installation, in the flattering
// direction, which is the direction nobody questions.
//
// WARN, not FAIL, and the distinction is the same one checkIdentityJoin draws
// above: a FAIL means the data is WRONG (spend and outcomes that should meet
// never do), while this is a COVERAGE statement about the window that was asked
// for. The number is a correct measurement of a window that is partly unmeasured.
// Failing here would also fail every default install until the window clamp
// lands, training operators to ignore doctor's exit code — the opposite of the
// point.
func checkCostHorizon(s scoresResponse) []checkResult {
	dq := s.DataQuality

	// No signal. Two causes that must NOT be conflated: a server older than #512
	// (which cannot answer, and whose silence would otherwise read as "covered"),
	// versus a store holding no cost at all (nothing to cover, not a fault). Total
	// cost separates them without guessing.
	if dq.CostCoverageStart == "" || dq.WindowPredatesCostCapture == nil {
		if s.Total.TotalCostUSD <= 0 {
			return nil
		}
		return []checkResult{{
			name:   "cost horizon",
			status: statusWarn,
			detail: "server reported cost but emitted no cost-horizon signal, so this window cannot be checked for coverage",
			// Three causes land here, not two: a pre-#512 server, a current server
			// whose horizon query ERRORED (it logs and omits rather than 500-ing),
			// and a signal lost in transit. Naming only the first would send an
			// operator to an upgrade that fixes nothing.
			hint: "this tierd predates #512, or its horizon query failed — check the server log, and upgrade if it is older than #512. Until then treat the TIER above as an upper bound",
		}}
	}

	horizonDay := doctorDay(dq.CostCoverageStart)
	// The server's precomputed remedy. Fall back to the horizon's own day only if
	// an older server omits it — knowingly approximate, which is why the field
	// exists.
	safeSince := dq.CostCoverageSafeSince
	if safeSince == "" {
		safeSince = horizonDay
	}
	// Wire-sourced strings bound for a terminal. doctor points at an arbitrary
	// --server, so these are untrusted: unsanitized, a hostile or broken one
	// injects ANSI/CR/LF and can forge doctor's own summary line. joinSafe covers
	// the source names; these three reach the terminal by a different path.
	since := safeDateStr(s.Since)
	safeSince = safeDateStr(safeSince)
	horizonDay = safeDateStr(horizonDay)

	if *dq.WindowPredatesCostCapture {
		return []checkResult{{
			name:   "cost horizon",
			status: statusWarn,
			detail: fmt.Sprintf("the scored window starts %s but cost capture began %s — outcomes from the uncovered head are counted against none of their cost, so TIER reads HIGH",
				since, horizonDay),
			hint: fmt.Sprintf("re-run with ?since=%s or later to compare like with like. This is NOT a retention setting: capture began %s on this installation and nothing moves it backwards",
				safeSince, horizonDay),
		}}
	}

	// The global horizon is the loosest bound — a window can clear it and still
	// predate an individual capture path entirely.
	//
	// ok=false means the comparison could not be MADE. That must never fall
	// through to the OK verdict below: reporting "fully covered" for sources
	// nobody managed to check is the precise failure this whole change exists to
	// eliminate, and it would be this file committing it.
	late, ok := lateCaptureSources(dq.SourceCoverageStart, s.Since)
	switch {
	case !ok:
		return []checkResult{{
			name:   "cost horizon",
			status: statusWarn,
			detail: fmt.Sprintf("the global horizon (%s) is cleared, but per-source coverage could NOT be checked: the server reported an unusable window start (%s)",
				horizonDay, since),
			hint: "one capture source may still start after this window does, which the global horizon cannot express. Re-run against a server reporting a valid `since`",
		}}
	case len(late) > 0:
		return []checkResult{{
			name:   "cost horizon",
			status: statusWarn,
			detail: fmt.Sprintf("overall cost capture begins %s and the window clears it, but %d capture source(s) started later and contribute no cost to the earlier part of the window: %s",
				horizonDay, len(late), joinSafe(late)),
			hint: "outcomes those sources produced before their start dates are counted against none of their spend. Narrow the window to the latest source start, or read per-source figures separately",
		}}
	}

	return []checkResult{{
		name:   "cost horizon",
		status: statusOK,
		detail: fmt.Sprintf("cost capture begins %s; the scored window (from %s) is fully covered", horizonDay, since),
	}}
}

// lateCaptureSources returns "name (date)" for every source whose own horizon
// starts after the scored window does, sorted for a deterministic report.
//
// The bool is ok=false for "this comparison could not be made" — an unparseable
// window start — which the caller MUST report rather than treat as nothing found.
// An empty list and an unmade comparison are different facts, and collapsing them
// is how a check that never ran comes to look like a check that passed.
//
// Values are server-supplied and reach the terminal through joinSafe at the call
// site, which sanitizes and caps them like every other untrusted string here.
func lateCaptureSources(perSource map[string]string, since string) ([]string, bool) {
	// Nothing to compare against is not a failed comparison: a single-source (or
	// pre-#512) server legitimately sends no map, and there is no claim to check.
	if len(perSource) == 0 {
		return nil, true
	}
	winStart, err := time.Parse("2006-01-02", since)
	if err != nil {
		return nil, false
	}
	var late []string
	for name, ts := range perSource {
		start, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			// A source we cannot place is a source we cannot clear.
			return nil, false
		}
		if start.After(winStart) {
			late = append(late, fmt.Sprintf("%s (%s)", name, start.UTC().Format("2006-01-02")))
		}
	}
	sort.Strings(late)
	return late, true
}

// safeDateStr makes a wire-supplied date safe to print WITHOUT mangling the
// normal case.
//
// Routing these through logsafe.Str unconditionally would be secure but wrong in
// practice: logsafe quotes with %q, so the remedy would render as
// ?since="2026-06-24" and an operator copying it pastes the quotes too. A hint
// that cannot be pasted is a hint that does not work.
//
// Re-emitting the parsed value instead means the printed string is CONSTRUCTED
// by us from a parsed time, not passed through — so it cannot carry an escape
// sequence by construction. Anything that fails to parse is not a date, has no
// paste value, and falls back to full logsafe sanitization.
func safeDateStr(s string) string {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format("2006-01-02")
	}
	return logsafe.Str(s)
}

// doctorDay renders an RFC3339 instant as a plain UTC date, falling back to the
// raw server string rather than printing a zero time or inventing a value. The
// fallback is untrusted server output — callers must pass it through safeDateStr.
func doctorDay(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.UTC().Format("2006-01-02")
}

// pctStr renders a share without collapsing a small non-zero value to "0%",
// which would print the same digits under two different verdicts.
func pctStr(share float64) string {
	switch p := share * 100; {
	case share > 0 && p < 1:
		return "<1%"
	default:
		return fmt.Sprintf("%.0f%%", p)
	}
}

// joinSafe renders server-supplied identities for a terminal. The values are
// client-controlled (POST /api/v1/events length-caps developer and validates
// nothing else), and this is a NEW sink for that tainted class: without
// sanitizing, an identity carrying ANSI escapes can clear the screen and forge
// doctor's own "all checks passed" line — defeating the very check whose purpose
// is to stop an operator misreading the report. Capped in count as well as
// length so a large fleet cannot emit a multi-megabyte line.
func joinSafe(ids []string) string {
	const maxShown = 3
	shown := ids
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	parts := make([]string, 0, len(shown))
	for _, id := range shown {
		parts = append(parts, logsafe.Str(id))
	}
	out := strings.Join(parts, ", ")
	if len(ids) > maxShown {
		out += fmt.Sprintf(" (+%d more)", len(ids)-maxShown)
	}
	return out
}

// aliasRemedy emits a paste-ready command ONLY when the pairing is unambiguous.
//
// cost_only and outcome_only are independently sorted, so pairing [0] with [0]
// on longer lists marries two unrelated people by alphabetical coincidence — and
// pasting that command credits one developer's spend to another and inflates
// their TIER. A tool whose premise is that operators act on its output without
// re-deriving it must not emit a confidently wrong command.
func aliasRemedy(u unjoinedDevelopers) string {
	if len(u.CostOnly) == 1 && len(u.OutcomeOnly) == 1 {
		// %q is the single quoting authority here — it both escapes and wraps, so
		// it produces valid JSON string literals. Strip CR/LF FIRST (the injection
		// vector) rather than routing through logsafe.Str, which would %q a second
		// time and emit a doubly-quoted, un-pasteable "\"asmith\"".
		return fmt.Sprintf(`map them with: POST /api/v1/developer_alias {"alias":%q,"canonical":%q}`,
			stripLine(u.CostOnly[0]), stripLine(u.OutcomeOnly[0]))
	}
	return `map each cost-side identity to its outcome-side identity with: POST /api/v1/developer_alias {"alias":"<cost-side id>","canonical":"<outcome-side id>"}`
}

// stripLine removes CR/LF so a client-controlled identity cannot break out of the
// single line it is printed on. Quoting is left to the caller's %q so the value
// is escaped exactly once.
func stripLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", "")
}

// evalServerResponse turns a /scores HTTP response into the server-side checks.
// Pure (response + body + local clock + embedded price version in, results out) so
// unreachable/401/403/clock-skew/price-mismatch/healthy are each table-testable
// against an in-memory response.
func evalServerResponse(resp *http.Response, body []byte, localNow time.Time, embeddedPriceVersion int) []checkResult {
	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to the detailed checks below
	case http.StatusUnauthorized:
		return []checkResult{{
			name:   "server auth",
			status: statusFail,
			detail: "server returned 401 Unauthorized",
			hint:   "set --api-token (or TIER_API_TOKEN) to a valid token; the server has auth enabled",
		}}
	case http.StatusForbidden:
		return []checkResult{{
			name:   "server auth",
			status: statusFail,
			detail: "server returned 403 Forbidden",
			hint:   "this token lacks read scope, or the server is in team-aggregation mode; use a token that can read /scores",
		}}
	default:
		return []checkResult{{
			name:   "server reachable",
			status: statusFail,
			detail: fmt.Sprintf("server returned unexpected status %d", resp.StatusCode),
			hint:   "confirm --server points at a tierd HTTP listener",
		}}
	}

	out := []checkResult{{
		name:   "server auth",
		status: statusOK,
		detail: "GET /api/v1/scores returned 200",
	}}

	// Clock offset from the HTTP Date header (RFC 7231 §7.1.1.2). A missing or
	// unparseable Date is surfaced as a WARN rather than silently skipped — an
	// invisible check reads as a passing one, which is exactly the false confidence
	// doctor exists to prevent.
	serverTime, haveDate := parseServerDate(resp.Header.Get("Date"))
	switch {
	case !haveDate:
		out = append(out, checkResult{
			name:   "clock offset",
			status: statusWarn,
			detail: "server sent no parseable Date header; cannot check clock offset",
			hint:   "a proxy may be stripping Date; a large clock offset would mis-window attribution unnoticed",
		})
	default:
		skew := localNow.Sub(serverTime)
		if skew < 0 {
			skew = -skew
		}
		if skew > doctorClockSkewWarn {
			out = append(out, checkResult{
				name:   "clock offset",
				status: statusWarn,
				detail: fmt.Sprintf("local clock differs from server by %s", skew.Round(time.Second)),
				hint:   "sync this machine's clock (NTP); a large offset mis-windows attribution and since/until bounds",
			})
		} else {
			out = append(out, checkResult{
				name:   "clock offset",
				status: statusOK,
				detail: fmt.Sprintf("within %s of server", skew.Round(time.Second)),
			})
		}
	}

	// Price-table parity: the embedded table this binary would price with vs the
	// version the server priced its /scores response with (#233). A real tierd
	// /scores response ALWAYS carries a non-zero price_table.version; a 200 that
	// parses to version 0 (or does not parse at all) means --server points at
	// something that is NOT a tierd /scores endpoint — a captive portal, a load
	// balancer default page, the wrong host. Surfacing that as a WARN closes the
	// exact false-positive doctor exists to catch: a bare "200 OK, all checks
	// passed" against a non-tierd server.
	var stamp scoresResponse
	stampErr := json.Unmarshal(body, &stamp)
	// isTierdScores is the single predicate both the price check and the identity
	// check agree on, so they can never disagree about whether this body is a
	// tierd /scores response. On a parse error encoding/json leaves stamp
	// partially populated, so the identity check MUST gate on this, not on
	// sniffing a decoded field.
	isTierdScores := stampErr == nil && stamp.PriceTable.Version != 0

	switch {
	case !isTierdScores:
		out = append(out, checkResult{
			name:   "price table",
			status: statusWarn,
			detail: "server returned 200 but no price_table stamp — this may not be a tierd /scores endpoint",
			hint:   "confirm --server points at a tierd HTTP listener (its /api/v1/scores response carries a price_table version)",
		})
	case stamp.PriceTable.Version != embeddedPriceVersion:
		out = append(out, checkResult{
			name:   "price table",
			status: statusWarn,
			detail: fmt.Sprintf("local price table v%d != server v%d", embeddedPriceVersion, stamp.PriceTable.Version),
			hint:   "align price tables (--prices / TIER_PRICES) so local cost estimates match server-authoritative costs",
		})
	default:
		out = append(out, checkResult{
			name:   "price table",
			status: statusOK,
			detail: fmt.Sprintf("local and server agree on v%d", stamp.PriceTable.Version),
		})
	}

	// Identity join (#496) — only when this is genuinely a tierd /scores body.
	if isTierdScores {
		out = append(out, checkIdentityJoin(stamp)...)
		// Runs after the join check by design: the join check explains why some
		// outcomes have no matching spend, and the horizon is the most common
		// structural reason for it (#512). Ordering them the other way would
		// report the symptom after the cause.
		out = append(out, checkCostHorizon(stamp)...)
	}
	return out
}

// reportDoctor prints each result and returns the process exit code: 1 if any
// check FAILed, else 0. WARNs are surfaced but do not fail the process — a
// partial-but-working install (e.g. a repo with no remote) should still pass the
// acceptance step while telling the operator what to improve.
//
// The exit code is the contract and does not change. The closing LINE is not:
// a run carrying warnings must never end with "all checks passed", because the
// summary is the one line an operator actually reads and that wording tells them
// the warnings above were cosmetic. Several of them are not — the cost-horizon
// warning (#512) means the number they just looked at is inflated by a known
// factor. Three distinct endings for three distinct states, so a clean run stays
// unambiguously clean and a warned run cannot be mistaken for one.
func reportDoctor(results []checkResult, stdout io.Writer) int {
	failed := false
	warned := 0
	for _, r := range results {
		switch r.status {
		case statusFail:
			failed = true
		case statusWarn:
			warned++
		}
		_, _ = fmt.Fprintf(stdout, "[%-4s] %s: %s\n", r.status.label(), r.name, r.detail)
		if r.hint != "" && r.status != statusOK {
			_, _ = fmt.Fprintf(stdout, "         hint: %s\n", r.hint)
		}
	}
	if failed {
		_, _ = fmt.Fprintln(stdout, "\ndoctor: one or more checks FAILED — capture is not fully working (see hints above)")
		return 1
	}
	if warned > 0 {
		_, _ = fmt.Fprintf(stdout, "\ndoctor: no failures, but %d check(s) need attention — read the hints above before trusting the numbers\n", warned)
		return 0
	}
	_, _ = fmt.Fprintln(stdout, "\ndoctor: all checks passed")
	return 0
}
