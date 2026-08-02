// Package issueref provides shared issue ID extraction from branch names and
// commit/PR messages. Used by both the JSONL collector and the webhook handler
// to ensure consistent attribution across all data paths.
package issueref

import (
	"regexp"
	"strconv"
	"strings"
)

// Year-guard window. A bare 4-digit branch segment inside this inclusive range
// is treated as a calendar year (e.g. "release/2024-fix") rather than an issue
// number, because release/date-stamped branches are far more common than issues
// numbered in the ~1900–2099 band. See the tradeoff note in FromBranch.
const (
	yearGuardLow  = 1900
	yearGuardHigh = 2099
)

var (
	// closesRE matches "closes #42", "fixes #99", "resolves #7" (case-insensitive).
	// The captured number uses [1-9]\d* so a leading zero or bare "#0" never
	// yields a bogus issue id — GitHub issue numbers are >= 1 and never
	// zero-padded (matches spaceHashIssueRE's guard).
	closesRE = regexp.MustCompile(`(?i)(?:closes?|fixes?|resolves?)\s+#([1-9]\d*)`)
	// prefixedIssueRE matches tracker-style keys like TIER-42, JIRA-123.
	prefixedIssueRE = regexp.MustCompile(`([A-Z][A-Z0-9]+-\d+)`)
	// spaceHashIssueRE matches "#42" preceded by whitespace or start-of-line,
	// but not Markdown headings like "## Section" or hex colours like "#FF0000".
	spaceHashIssueRE = regexp.MustCompile(`(?:^|\s)#([1-9]\d*)\b`)
	// closeClauseRE matches a close directive and the WHOLE run of issue refs that
	// belong to it (captured group 1): "closes #12", "closes #12, #15",
	// "fixes #12 and #15", "resolves #12 #15". Separators inside the run are a
	// comma, the word "and", or bare whitespace, so both the GitHub-strict
	// "closes #a, closes #b" (two clauses) and the shorthand "closes #a, #b" (one
	// clause, two refs) enumerate. ClosedIssues then pulls each #N out of the run.
	// The run stops at the first token that is not a #N ref, so trailing prose is
	// never swept in.
	closeClauseRE = regexp.MustCompile(`(?i)(?:closes?|fixes?|resolves?)\s+(#[1-9]\d*(?:(?:\s*,\s*|\s+and\s+|\s+)#[1-9]\d*)*)`)
	// hashNumRE extracts each "#N" issue number from a close-clause run.
	hashNumRE = regexp.MustCompile(`#([1-9]\d*)`)
)

// IsExploratoryBranch reports whether a branch name carries NO feature identity
// of its own: the mainline branches (main/master), a detached HEAD (recorded as
// "HEAD"), or an empty/absent branch. These are the exact inputs FromBranch
// returns "" for.
//
// It exists so the collector can make one decision consistently: an exploratory
// branch must never INHERIT an issue from a nearby commit. A session that merely
// opened on main within ±30min of a merge commit whose subject says "closes #N"
// is real exploratory/planning cost, not work on #N — inheriting the merge's
// issue there is a MIS-attribution, and a false attribution is worse than an
// honest "unattributed" bucket. The mainline/detached set is centralized here
// (rather than re-listed at each call site) so the two notions — "yields no
// issue" (FromBranch) and "must not inherit one" (this) — cannot drift apart.
func IsExploratoryBranch(branch string) bool {
	branch = strings.TrimSpace(branch)
	return branch == "" || branch == "HEAD" || branch == "main" || branch == "master"
}

// FromBranch extracts an issue ID from a branch name alone.
//
//	"feature/42-auth"      → "issue-42"
//	"feature/42"           → "issue-42"   (tail number, no suffix)
//	"fix/TIER-99-crash"    → "TIER-99"
//	"release/2024-fix"     → ""           (year-guarded, see note below)
//	"release/2024-42"      → "issue-42"   (year skipped, real issue recovered)
//	"main"                 → ""
//
// Precedence: an explicit tracker key (TIER-42) wins over a bare numeric
// segment. Bare numeric segments are then scanned left to right and the first
// plausible issue number wins.
//
// Year-guard tradeoff: because a bare 4-digit segment in the year band is
// ambiguous ("release/2024-fix" vs a genuine issue #2024), we bias toward the
// far more common release/date-stamp interpretation and skip it. The cost is
// that an unlucky org whose issue numbers reach 1900–2099 must disambiguate by
// using a tracker key ("PROJ-2024") or referencing "#2024" in the PR body,
// where no guard applies. 1–3 digit and 5+ digit numbers are never guarded, and
// a year segment is only skipped (not fatal), so a date-stamped branch that also
// carries an issue number still attributes.
func FromBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" || branch == "main" || branch == "master" {
		return ""
	}
	// Prefixed project key takes priority. It is an explicit, unambiguous marker,
	// so the year guard does not apply here — "REL-2024" is a valid key.
	if m := prefixedIssueRE.FindString(branch); m != "" {
		return m
	}
	// Scan numeric segments left to right, splitting on the same delimiters the
	// branch-naming convention uses (/ - _). Return the first segment that is a
	// plausible issue number (all digits, no leading zero) and is not a calendar
	// year. Skipping — rather than aborting on — a year segment lets a
	// date-stamped branch that also carries an issue number attribute, e.g.
	// "release/2024-42" → "issue-42" (issue #181).
	for _, seg := range strings.FieldsFunc(branch, isBranchDelimiter) {
		if isIssueNumber(seg) && !isLikelyYear(seg) {
			return "issue-" + seg
		}
	}
	return ""
}

// FromPRBody extracts an issue ID from a PR description body.
//
//	"closes #42"          → "issue-42"
//	"tracked in PROJ-9"   → "PROJ-9"
//	"see #99"             → "issue-99"
//	"## Section"          → ""   (Markdown heading not matched)
//
// Precedence (highest confidence first, per issue #181): an explicit close
// directive ("closes #N") beats a tracker key ("PROJ-123"), which beats a bare
// "#N". This ordering lets a Jira/Linear shop that references a tracker key in
// the body — with a generic branch name — still attribute (issue #181, gap 3),
// while an explicit GitHub close directive always wins when present.
//
// Multi-issue rule (#189): when a PR body closes MULTIPLE issues
// ("closes #12, #15"), this returns the PRIMARY — the LEFTMOST close directive,
// == ClosedIssues(body)[0]. TIER attributes the one merged PR to exactly that
// issue; ClosedIssues enumerates the rest so the un-credited issues are logged,
// not silently lost. One outcome per PR (never one per issue) is mandatory
// because outcomes.merge_commit_sha is UNIQUE (#60) — see ClosedIssues.
func FromPRBody(body string) string {
	// 1. Explicit close directive — highest confidence.
	if m := closesRE.FindStringSubmatch(body); len(m) == 2 {
		return "issue-" + m[1]
	}
	// 2. Tracker-style key (TIER-42, PROJ-123). Tradeoff: free-form prose that
	//    contains tokens shaped like keys (e.g. "UTF-8", "SHA-256", "COVID-19")
	//    is mis-read as an issue here and — per the issue #181 precedence —
	//    shadows a later bare "#N". Prefer "closes #N" to override. The branch
	//    is consulted before the body in FromBranchOrBody, so this only bites a
	//    PR whose branch carries no issue AND whose body has such a token.
	if m := prefixedIssueRE.FindString(body); m != "" {
		return m
	}
	// 3. Bare "#42" reference — lowest confidence. The regex already guarantees
	//    a leading non-zero digit, so the captured value is always >= 1.
	if m := spaceHashIssueRE.FindStringSubmatch(body); len(m) == 2 {
		return "issue-" + m[1]
	}
	return ""
}

// FromBranchOrBody tries branch first, then falls back to the PR/commit body.
//
// Multi-issue rule (#189): this returns the single PRIMARY issue a PR is
// attributed to — the branch-derived id when the branch carries one, else the
// leftmost close directive in the body (see FromPRBody). A PR that closes several
// issues still yields exactly ONE outcome under this primary; use ClosedIssues to
// enumerate all closed issues (the webhook logs the secondaries).
func FromBranchOrBody(branch, body string) string {
	if id := FromBranch(branch); id != "" {
		return id
	}
	return FromPRBody(body)
}

// ClosedIssues returns every issue id named by a close directive in body, in
// left-to-right order, deduplicated (issue-ids like "issue-42"). It exists so a
// multi-issue PR is not silent: a body that says "closes #12, #15" closes both on
// GitHub, but TIER attributes the merged outcome to exactly ONE issue — the
// PRIMARY (FromBranchOrBody / FromPRBody, == the first element here). The webhook
// logs the full set so the un-credited issues are observable. Returns nil when
// body names no closed issue.
//
// Why one outcome, not one per issue: outcomes.merge_commit_sha is UNIQUE (#60,
// one merged PR ↔ one merge commit), so fanning a PR out into N outcome rows would
// violate that constraint; and crediting full outcome weight to each issue would
// multiply a single PR's contribution to team TIER. The primary-only rule keeps
// "one merged PR = one outcome" exact. This deliberately covers only close
// DIRECTIVES ("closes/fixes/resolves #N"), not bare "#N" mentions or tracker keys:
// the concern is which issues a merge actually CLOSES, which is what fans out.
func ClosedIssues(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, clause := range closeClauseRE.FindAllStringSubmatch(body, -1) {
		for _, ref := range hashNumRE.FindAllStringSubmatch(clause[1], -1) {
			id := "issue-" + ref[1]
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// isBranchDelimiter reports whether r separates segments in a branch name.
// These are the conventional separators: path "/", dash, and underscore.
func isBranchDelimiter(r rune) bool {
	return r == '/' || r == '-' || r == '_'
}

// isIssueNumber reports whether s is a plausible bare issue number: non-empty,
// all ASCII digits, and without a leading zero (GitHub issue numbers are >= 1
// and never zero-padded, so "0" and "007" are rejected).
func isIssueNumber(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isLikelyYear reports whether s is a 4-digit segment in the calendar-year guard
// window [yearGuardLow, yearGuardHigh]. Only exactly-4-digit inputs are guarded:
// 1–3 digit and 5+ digit numbers pass through as issue numbers. Callers pass a
// value already validated by isIssueNumber, so Atoi cannot fail in practice; the
// error branch fails safe (treats the value as not-a-year).
func isLikelyYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	return n >= yearGuardLow && n <= yearGuardHigh
}
