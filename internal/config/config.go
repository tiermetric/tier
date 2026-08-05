// Package config defines the YAML schema for `tierd serve --config <file>`
// and the loader that parses it (closes #29).
//
// The schema mirrors the flags 1:1 with logical nesting (http / proxy /
// watch) so an operator can read a config file and immediately know which
// flag each value would override. Precedence (enforced by the caller, not
// here): explicit CLI flag > env var > config file > builtin default.
package config

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	// go.yaml.in/yaml/v3 is the YAML org's maintained continuation of the
	// archived gopkg.in/yaml.v3 (#52). It is a same-API, import-path-only
	// replacement: the decoder, KnownFields strict mode, and `yaml:` struct
	// tags are byte-for-byte identical, so this swap is behavior-preserving.
	//
	// Threat model: config input is operator-owned and read once at startup
	// from a trusted local path — never attacker-controlled. The historical
	// DoS class that affected this parser family (billion-laughs / deeply
	// nested aliases feeding an untrusted decoder) is therefore out of scope
	// here; a hostile operator already controls the whole process. The reason
	// to leave the archived module is future-patchability, not a live exploit:
	// any vulnerability found against yaml.v3 will never get an upstream fix,
	// whereas the fork is maintained. govulncheck stays wired into `make lint`
	// (fail-hard on a finding, soft only when the tool is absent) so a future
	// GO-ID against the parser or any required module trips the build.
	"go.yaml.in/yaml/v3"
)

// Config is the parsed YAML body. All fields are pointers (or slices /
// nested structs) so the caller can distinguish "key absent from file" from
// "key present with zero value" — only present keys override flag defaults.
//
// Pointer fields use *string instead of string so a YAML `http: { addr: "" }`
// (explicit empty value) is treated as an override-to-empty rather than
// "not present."
type Config struct {
	HTTP       HTTPConfig       `yaml:"http"`
	DB         *string          `yaml:"db,omitempty"`
	PricesFile *string          `yaml:"prices_file,omitempty"`
	Proxy      ProxyConfig      `yaml:"proxy"`
	Watch      WatchConfig      `yaml:"watch"`
	Collectors CollectorsConfig `yaml:"collectors"`
	Outcomes   OutcomesConfig   `yaml:"outcomes"`
	// ZeroOutcomeWindowDays configures the zero-outcome tripwire (#189): the
	// look-back window, in days, over which "cost accrued but zero outcomes
	// recorded" is evaluated. Mirrors --zero-outcome-window-days. Absent → nil →
	// the flag default (7 days). Must be >= 1 (validated by the caller).
	ZeroOutcomeWindowDays *int `yaml:"zero_outcome_window_days,omitempty"`
	// Aggregation selects the reporting posture (#185, #270): "team" emits only
	// team-level aggregates and "division" rolls up one level higher to divisions
	// — both k-anonymized and never naming an individual (the safe posture for EU
	// works-council / GDPR Art. 22 co-determination regimes); "developer" keeps
	// the named per-developer rows. Mirrors --aggregation (env
	// TIER_AGGREGATION). It is a REQUIRED setting with NO silent default: the
	// caller fails loud at startup when it resolves empty from every source, so an
	// existing deployment's reporting behavior can never change silently. A
	// pointer so an absent key is distinguishable from an explicit value.
	Aggregation *string `yaml:"aggregation,omitempty"`
	// KAnonymity is the k-anonymity cohort floor for team mode (#185): a team with
	// fewer than this many contributing developers is collapsed into an "other"
	// aggregate so no sub-k cohort is individually identifiable. Mirrors
	// --k-anonymity (env TIER_K_ANONYMITY). Absent → nil → the flag default (5);
	// the caller rejects any value below the hard minimum of 3 at startup.
	KAnonymity *int `yaml:"k_anonymity,omitempty"`
	// Subscriptions declares the flat monthly FEES the org pays for
	// subscription-billed model routes (#113, list-rate inversion). It is
	// deliberately the fee ONLY: what a subscription route's tokens are worth in
	// the TIER denominator is a PRICE-TABLE fact (`billing_mode: subscription`
	// plus a comparable list rate), not a config fact, so the two artifacts each
	// hold exactly one truth and cannot disagree about pricing.
	//
	// A slice, not a pointer: list-valued, config-only (no CLI flag or env var —
	// a per-route fee is a deployment fact, not a switch), and an explicit `[]`
	// is the documented "no subscriptions" state the shipped example carries.
	Subscriptions []Subscription `yaml:"subscriptions"`
}

// Subscription is one flat-fee plan the org pays for a subscription-billed
// model route (#113). Steve's 2026-07-02 ruling (the list-rate INVERSION, over
// the originally-ordered amortized divisor) splits the money in two:
//
//   - the route's TOKENS are priced at the comparable list rate carried by its
//     price-table entry — a local, per-call, window-independent number, so a
//     developer's cost never depends on how much a colleague used the route;
//   - MonthlyFeeUSD — the dollars actually paid — enters ONLY the actual-paid
//     side of Spend Leverage, delta-posted into org_actual_spend by the
//     reconciler in cmd/tierd. It NEVER enters the TIER denominator.
//
// Unlike the pointer fields elsewhere in this package these are plain values:
// every required field is validated non-empty / > 0 at Load, so "absent" and
// "zero" are both loud errors rather than states a caller must tell apart.
type Subscription struct {
	// RoutePrefix identifies the route this fee pays for, as a prefix of the
	// PRICE-TABLE KEY — either a normalized model (`glm-5.2`) or the
	// host-qualified `model@host` form (`glm-5.2@ollama.com`, store.HostModelKey).
	// It must be lowercase (table keys are normalized lowercase) and must
	// prefix-match at least one `billing_mode: subscription` entry in the ACTIVE
	// price table; `tierd serve` refuses to start otherwise, because a fee posted
	// for a route TIER does not price as subscription silently inflates
	// actual-paid with no matching list value.
	RoutePrefix string `yaml:"route_prefix"`
	// Plan is informational (e.g. "max") and appears in logs only.
	Plan string `yaml:"plan,omitempty"`
	// MonthlyFeeUSD is the real flat fee paid per calendar MONTH (> 0, finite).
	// There is no proration: the org owes the whole fee for any month the plan is
	// active, so a partial month posts the full amount.
	MonthlyFeeUSD float64 `yaml:"monthly_fee_usd"`
	// Org is the org_actual_spend key the fee posts under. Set it to the org your
	// developers are enrolled in (org_hierarchy / period_membership) so the fee
	// allocates to seats and reaches Spend Leverage. Optional: when empty the fee
	// posts under the route prefix with any trailing '/' trimmed — visible in the
	// ledger, but allocated to nobody until seats exist under that org.
	Org string `yaml:"org,omitempty"`
	// ActiveSince is the FIRST period (YYYY-MM) the plan was active (#155). When
	// set, the startup reconcile pass catches up every unposted period from here
	// through the current one, so a tierd that was offline across a month boundary
	// does not leave that month's actual-paid understated by the whole fee. Absent
	// keeps the current-period-only behaviour — there is deliberately no inferred
	// start date and no unbounded backfill.
	ActiveSince string `yaml:"active_since,omitempty"`
}

// OrgKey returns the org_actual_spend key this subscription's fee posts under:
// the explicit Org when set, else the route prefix with any trailing '/'
// trimmed. See the Org field docs for the allocation caveat.
func (s Subscription) OrgKey() string {
	if s.Org != "" {
		return s.Org
	}
	return strings.TrimSuffix(s.RoutePrefix, "/")
}

// maxSubscriptionIdentifierLen caps the identifier-ish subscription fields at
// the same 256 chars the API layer allows for an identifier, so a config-sourced
// org can never exceed what a POST /api/v1/org_actual_spend body could carry.
const maxSubscriptionIdentifierLen = 256

// maxBackfillPeriods bounds the #155 startup catch-up at 20 years of months.
// active_since is operator-declared and already bounds the backfill, so this is
// not a policy limit — it is a TYPO limit: `0226-06` parses, validates as <=
// now, and would silently issue ~21,000 reconcile transactions at every boot.
// Refusing it names the mistake instead of making startup mysteriously slow.
const maxBackfillPeriods = 240

// maxMonthlyFeeUSD mirrors store.MaxCostUSD — the trust-boundary magnitude cap
// every other money entry point in this system enforces (POST /api/v1/costs,
// POST /api/v1/org_actual_spend). It is DUPLICATED rather than imported for the
// same reason periodRE is: internal/config has no production dependency on
// internal/store, and one float constant is a cheaper price than that edge.
// TestMonthlyFeeCapMatchesStore pins the two equal, so the duplicate cannot
// drift into being the loose one.
const maxMonthlyFeeUSD = 1e12

// minMonthlyFeeUSD is the smallest fee that is REPRESENTABLE in the integer
// micro-dollars org_actual_spend stores — one micro-dollar. Positivity and the
// magnitude cap above are both satisfied by a value like 4e-7, but
// store.DollarsToMicro(4e-7) is 0, and a zero fee is not merely useless here: it
// is DESTRUCTIVE, because of how #155's backfill remembers what it has done.
//
// PostSubscriptionFeeIfUnposted treats "this period has any row" as posted, so a
// zero-valued row permanently marks the period covered. Worse, it returns 0 —
// indistinguishable from "already covered" — so the reconciler takes its
// `delta == 0 → continue` branch and logs NOTHING. The rows are written
// invisibly, and later correcting monthly_fee_usd can never repair those months,
// because the backfill will keep finding a row and skipping them. That defeats
// the function's own stated invariant, which reserves a period that "nets to
// zero" for a DELIBERATE human correction.
//
// So the bound is representability, checked once here at the single config entry
// point, rather than a second guard smeared across the store. (A value between
// 5e-7 and 1e-6 would in fact round to one micro-dollar, so this rejects a hair
// more than it strictly must — one micro-dollar is the honest unit to state, and
// nobody's real plan costs less than it.)
const minMonthlyFeeUSD = 1e-6

// periodRE matches a billing period: YYYY-MM with a real month (01-12). It is
// deliberately stricter than time.Parse("2006-01", …), which accepts a
// single-digit month ("2026-7") that would later violate the
// GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]' CHECK on org_actual_spend.period —
// turning a config typo into a runtime INSERT failure hours after startup.
// (Duplicated from store.ValidPeriod rather than imported: internal/config has
// no dependency on internal/store today, and one regexp is a cheaper price than
// that edge. store.ValidPeriod is the enforcement point; this is the
// fail-early copy, and TestActiveSincePeriodShapeMatchesStore pins them equal.)
var periodRE = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// validateSubscriptions fail-loud-checks the `subscriptions:` block (#113/#155).
// Everything here feeds money math or a UTC-month key, so a malformed block must
// fail the LOAD — not surface later as a $0 fee, an INSERT that violates a CHECK
// constraint, or a backfill that silently posts nothing.
//
// now is the clock the active_since upper bound is judged against; Load passes
// time.Now(). A future active_since is rejected because it can only be a typo:
// it would make the catch-up range inverted, and the reconciler would post
// nothing at all for a plan the operator believes is being billed.
func validateSubscriptions(subs []Subscription, now time.Time) error {
	for i, s := range subs {
		at := fmt.Sprintf("subscriptions[%d]", i)
		if s.RoutePrefix == "" {
			return fmt.Errorf("%s: route_prefix is required", at)
		}
		if len(s.RoutePrefix) > maxSubscriptionIdentifierLen {
			return fmt.Errorf("%s: route_prefix must be <= %d chars", at, maxSubscriptionIdentifierLen)
		}
		if s.RoutePrefix != strings.ToLower(s.RoutePrefix) {
			return fmt.Errorf("%s: route_prefix %q must be lowercase — it is matched against normalized price-table keys, which are lowercase", at, s.RoutePrefix)
		}
		if !positiveFinite(s.MonthlyFeeUSD) {
			return fmt.Errorf("%s (%s): monthly_fee_usd must be > 0 and finite, got %v", at, s.RoutePrefix, s.MonthlyFeeUSD)
		}
		// REPRESENTABILITY, a distinct failure from positivity: a fee that is
		// positive but rounds to zero micro-dollars writes a ZERO row that
		// permanently marks the period posted, silently and unrepairably. See
		// minMonthlyFeeUSD.
		if s.MonthlyFeeUSD < minMonthlyFeeUSD {
			return fmt.Errorf("%s (%s): monthly_fee_usd must be >= %g (one micro-dollar, the storage grain), got %v — a smaller fee rounds to zero and would write an unrepairable zero-valued posting", at, s.RoutePrefix, minMonthlyFeeUSD, s.MonthlyFeeUSD)
		}
		// MAGNITUDE bound, not just positivity — this is a money entry point and
		// must not be the one that skips the cap every other one enforces.
		// store.DollarsToMicro SATURATES to MaxInt64 rather than erroring, and one
		// saturated org_actual_spend row makes the allocation read's
		// SUM(actual_paid_micro) fail with "integer overflow" as soon as any second
		// row shares its (org, period) — so a fat-fingered fee does not merely
		// report a wrong number, it takes /scores down. POST /api/v1/org_actual_spend
		// already rejects the same magnitude (handler.go, store.MaxCostUSD); this is
		// that guard on the config path.
		if s.MonthlyFeeUSD > maxMonthlyFeeUSD {
			return fmt.Errorf("%s (%s): monthly_fee_usd must be <= %g, got %v", at, s.RoutePrefix, maxMonthlyFeeUSD, s.MonthlyFeeUSD)
		}
		if len(s.Org) > maxSubscriptionIdentifierLen {
			return fmt.Errorf("%s (%s): org must be <= %d chars", at, s.RoutePrefix, maxSubscriptionIdentifierLen)
		}
		// A degenerate prefix like "/" derives an empty org key, which would post
		// the fee under org "" — invisible to every allocation read. Reject the
		// shape rather than write money to a key nobody queries.
		if s.OrgKey() == "" {
			return fmt.Errorf("%s: route_prefix %q derives an empty org key — set org explicitly", at, s.RoutePrefix)
		}
		if s.ActiveSince != "" {
			if !periodRE.MatchString(s.ActiveSince) {
				return fmt.Errorf("%s (%s): active_since %q must be YYYY-MM with month 01-12", at, s.RoutePrefix, s.ActiveSince)
			}
			// Zero-padded YYYY-MM compares chronologically as a plain string.
			current := now.UTC().Format("2006-01")
			if s.ActiveSince > current {
				return fmt.Errorf("%s (%s): active_since %q is in the future (current period is %s) — the plan cannot have started after now", at, s.RoutePrefix, s.ActiveSince, current)
			}
			if n := monthsBetween(s.ActiveSince, current); n > maxBackfillPeriods {
				return fmt.Errorf("%s (%s): active_since %q would backfill %d periods (max %d) — check for a typo in the year", at, s.RoutePrefix, s.ActiveSince, n, maxBackfillPeriods)
			}
		}
		// Two entries sharing a prefix, or one shadowing another, would make the
		// same price-table entry billable twice — the org's actual-paid would carry
		// the fee once per overlapping entry. Reject rather than pick a winner.
		//
		// Scanned as an indexed loop over the PRIOR entries, not via a map: with two
		// overlapping predecessors a map's iteration order would name a different
		// one on each run, so the same bad config would produce two different
		// startup errors. An error message an operator cannot reproduce is worth
		// less than one they can.
		for j := 0; j < i; j++ {
			prev := subs[j].RoutePrefix
			if strings.HasPrefix(s.RoutePrefix, prev) || strings.HasPrefix(prev, s.RoutePrefix) {
				return fmt.Errorf("%s: route_prefix %q overlaps subscriptions[%d] %q — overlapping prefixes would post the same route's fee twice", at, s.RoutePrefix, j, prev)
			}
		}
	}
	return nil
}

// monthsBetween returns the inclusive count of YYYY-MM periods from..to. Both
// are assumed to have already matched periodRE and to satisfy from <= to.
func monthsBetween(from, to string) int {
	fy, fm := periodParts(from)
	ty, tm := periodParts(to)
	return (ty-fy)*12 + (tm - fm) + 1
}

// periodParts splits a periodRE-matching YYYY-MM into its integer year and
// month. The regexp has already proven both halves are digits, so the
// conversions cannot fail and are done by hand rather than through strconv with
// two ignored errors.
func periodParts(p string) (year, month int) {
	for i := 0; i < 4; i++ {
		year = year*10 + int(p[i]-'0')
	}
	month = int(p[5]-'0')*10 + int(p[6]-'0')
	return year, month
}

// positiveFinite reports whether f is a usable positive money value — strictly
// positive AND finite. YAML happily decodes `.inf` and `.nan` into a float64,
// and store.DollarsToMicro would turn those into MaxInt64 / 0 micro-dollars,
// i.e. invoice-grade garbage in org_actual_spend.
func positiveFinite(f float64) bool {
	return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// OutcomesConfig groups outcome-capture toggles (#196). PushCapture is a pointer
// so an ABSENT key means "flag default" (off) rather than "explicit false" — the
// same present-vs-absent discipline the rest of this package uses, so a config that
// omits it can't override an operator's --push-capture / TIER_PUSH_CAPTURE.
type OutcomesConfig struct {
	// PushCapture mirrors --push-capture (env TIER_PUSH_CAPTURE): capture a
	// qualifying direct commit to the default branch as a degraded (0.5,
	// per-issue-per-UTC-day) outcome so trunk-based teams aren't scored ~0 (#196).
	// Absent → nil → the flag default (false). Precedence, enforced by the caller:
	// CLI flag > env var > this config key > builtin default.
	PushCapture *bool `yaml:"push_capture,omitempty"`
	// GeneratedPaths overrides the built-in generated/vendored deny-list (#240,
	// audit C4) used to exclude no-op churn from push capture: a direct commit to
	// the default branch whose changed files are ALL generated (a regenerated
	// protobuf, `go mod vendor`, a lockfile bump) carries no engineering outcome
	// and must not earn a degraded push outcome. Each entry is a path pattern —
	// a trailing "/" is a directory prefix (vendor/, node_modules/), a leading
	// "*" is a filename suffix (*.pb.go, *_generated.go), otherwise an exact
	// basename (go.sum, package-lock.json). Absent → nil → the handler's built-in
	// default set (webhook.defaultGeneratedPaths); an explicit `[]` disables all
	// exclusion. A slice (not a pointer) so the example.yaml drift guard can
	// observe its presence; cmd/tierd plumbs a non-nil value into
	// webhook.WithGeneratedPaths.
	GeneratedPaths []string `yaml:"generated_paths,omitempty"`
	// SizeLabels overrides the built-in PR size-label → outcome-weight table
	// (#244): the label NAMES an org uses are configurable but the WEIGHTS are not.
	// TIER recognises GitHub's size/xs..xl labels (and the bare xs..xl forms) out
	// of the box; an org whose convention is different (`size: M` with a space,
	// `size-l`, an `S`-prefixed scheme) can remap the names here. Every value MUST
	// be on the fixed 0.5/1/3/5/8 outcome scale so scores stay comparable across
	// orgs — Load rejects an off-scale weight (or a blank name) at startup. Matching
	// is case-insensitive (the handler lowercases both sides). Absent → nil → the
	// handler keeps its built-in table (webhook.defaultSizeLabels), so existing
	// deployments are unchanged; an explicit `{}` is also "use the defaults". A map
	// (not a pointer) so the example.yaml drift guard can observe its presence;
	// cmd/tierd plumbs a non-nil value into webhook.WithSizeLabels. weight_source
	// stays 'label' for any configured match — no new provenance value.
	SizeLabels map[string]float64 `yaml:"size_labels,omitempty"`
}

// validSizeWeights is the fixed outcome scale a configured size label may map to
// (#244). Only the label NAMES are configurable; the weights stay on TIER's
// 0.5/1/3/5/8 scale so an outcome scored at one org is directly comparable to
// another's. This mirrors the scale labelWeight/GitHeuristic already use.
var validSizeWeights = []float64{0.5, 1, 3, 5, 8}

// validateSizeLabels rejects an outcomes.size_labels entry whose weight is off the
// fixed scale, whose name is blank, or that COLLIDES with another entry once folded
// for matching — so a misconfiguration fails loud at startup instead of silently
// skewing scores. A nil/empty map is valid — it means "use the built-in default
// table." The fold (lowercase + trim) mirrors webhook.WithSizeLabels exactly: the
// handler stores keys folded, so two names that fold to the same token (e.g. "M"
// and " m ") would otherwise collapse into one entry with a nondeterministic,
// map-iteration-order winner. Catching it here keeps the winner from being a coin
// flip and keeps config validation the single fail-loud gate.
func validateSizeLabels(m map[string]float64) error {
	folded := make(map[string]string, len(m))
	for name, w := range m {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("outcomes.size_labels: blank label name (weight %g)", w)
		}
		onScale := false
		for _, v := range validSizeWeights {
			if w == v {
				onScale = true
				break
			}
		}
		if !onScale {
			return fmt.Errorf("outcomes.size_labels[%q] = %g: weight must be one of the fixed scale 0.5, 1, 3, 5, 8", name, w)
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if other, dup := folded[key]; dup {
			return fmt.Errorf("outcomes.size_labels: %q and %q both match label %q (case-insensitive) — remove the duplicate", other, name, key)
		}
		folded[key] = name
	}
	return nil
}

// CollectorsConfig groups external polling collectors (#138). Each collector is a
// pointer sub-struct so an ABSENT block means "disabled" — the zero value of the
// pointer (nil) is unambiguously distinct from a present-but-empty block, matching
// the present-vs-absent discipline the rest of this package uses.
type CollectorsConfig struct {
	AnthropicAdmin *AnthropicAdminConfig `yaml:"anthropic_admin,omitempty"`
	OpenAIUsage    *OpenAIUsageConfig    `yaml:"openai_usage,omitempty"`
	CodexRollout   *CodexRolloutConfig   `yaml:"codex_rollout,omitempty"`
}

// CodexRolloutConfig configures the Codex rollout-log collector (#464).
//
// UNLIKE its two siblings above, this is NOT an org-level poller and needs no
// credential: it reads the rollout logs the Codex CLI already writes to
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl on this machine. It is the local
// twin of the Claude Code JSONL watcher — per-developer, per-call, real-time
// fidelity — and the only path that captures Codex as Codex is actually run.
// The reverse proxy learned to parse the Responses shape Codex speaks in #459
// task 2, but reaching it means pointing Codex at the proxy with API-key auth
// and has never been exercised by live traffic; this block is the verified path.
//
// SCOPE: it attributes to the SAME repositories the JSONL watcher does, taken
// from watch.repos / --watch-repo (and honoring watch.repo_slugs). A Codex
// session whose cwd is outside every watched repo is dropped, not attributed —
// cross-repo bleed would put another project's dollars on this project's issues.
// With no watched repo there is nothing to attribute to, so the collector stays
// disabled and says so at startup.
//
// DOUBLE-COUNTING, both paths that could cause it:
//
//  1. Against the PROXY — not possible today. A Codex call the proxy saw produced
//     no token event at all (#463), so there is no proxy row to collide with. If a
//     future Responses-API-aware proxy lands, the two paths must grow a shared
//     upstream-id idempotency key before both are enabled.
//  2. Against the OpenAI USAGE POLLER (collectors.openai_usage) — real, and
//     handled in the store. Codex runs on an OpenAI key, so its calls also appear
//     in OpenAI's org-level usage report. The poller ingests only the REMAINDER
//     after subtracting store.CapturedTokensByDayModel, so 'codex-rollout' is in
//     that query's source list (#464 R1). Removing it there re-counts every Codex
//     token inside the poller's remainder — per-call row AND org aggregate.
//
// The general rule: enabling any new per-call capture source alongside an org
// poller for the same provider requires the source to be in the poller's
// subtraction baseline. There is no other guard.
type CodexRolloutConfig struct {
	// SessionsDir overrides the ~/.codex/sessions root. Optional; empty means
	// the default. Useful when CODEX_HOME is relocated.
	SessionsDir *string `yaml:"sessions_dir,omitempty"`
	// ScanInterval is the re-scan cadence, e.g. "5m". Optional; default 5m;
	// minimum 30s (validated at startup). Re-scanning is cheap and idempotent —
	// events carry stable keys, so a re-scan converges rather than duplicating.
	ScanInterval *string `yaml:"scan_interval,omitempty"`
}

// AnthropicAdminConfig configures the Anthropic Admin API poller (#138). Presence
// of the block enables the poller; APIKey and Org are then required. APIKey
// supports the @/path/to/file indirection (resolved by cmd/tierd's resolveSecretFlag)
// so the org Admin key (sk-ant-admin…) never appears on the command line.
//
// The poller reconciles by posting the DELTA between the provider's month-to-date
// cost and the recorded org_actual_spend net for (org, period). That net (and the
// row it writes) is SCOPED to source='anthropic-admin' (#138), so the poller
// reconciles only against its own prior rows. Other-provider spend (a #139 OpenAI
// poller under 'openai-usage', or manual finance rows under 'manual') coexists
// safely for the same (org, period): org_actual_spend stores per source and the
// allocation read sums across sources, so the org total stays complete and no feed
// cannibalizes another.
//
// ⚠ Remaining caveat: do NOT ALSO post this org's Anthropic spend by hand via
// POST /api/v1/org_actual_spend — that lands under source='manual' and the
// cross-source read would then DOUBLE-COUNT the same Anthropic invoice (manual +
// poller). The poller owns the org's Anthropic feed; record only OTHER providers'
// spend manually.
type AnthropicAdminConfig struct {
	APIKey       *string `yaml:"api_key,omitempty"`
	Org          *string `yaml:"org,omitempty"`
	PollInterval *string `yaml:"poll_interval,omitempty"` // e.g. "1h"; default 1h; min 5m
}

// OpenAIUsageConfig configures the OpenAI Usage/Costs API poller (#139). It is
// the exact structural twin of AnthropicAdminConfig — same presence-enables
// semantics, same required APIKey (an org admin/owner-scoped key,
// sk-...admin...) + Org, same optional poll_interval — differing only in the
// provider it reconciles. APIKey supports the @/path/to/file indirection
// (resolved by cmd/tierd's resolveSecretFlag) so the org key never appears on
// the command line.
//
// SOURCE-SCOPING (mirrors #138 review R1): the poller reconciles by posting the
// DELTA between OpenAI's month-to-date cost and the recorded org_actual_spend
// net for (org, period) SCOPED to source='openai-usage', so it nets only against
// its own prior rows. An Anthropic-admin poller row, another provider's row, or a
// manual finance row sharing the same (org, period) coexists safely — the
// allocation read sums across sources, so the org total stays complete and no
// feed cannibalizes another.
//
// ⚠ Remaining caveat (same as anthropic_admin): do NOT ALSO post this org's
// OpenAI spend by hand via POST /api/v1/org_actual_spend — that lands under
// source='manual' and the cross-source read would then DOUBLE-COUNT the same
// OpenAI invoice (manual + poller). The poller owns the org's OpenAI feed; record
// only OTHER providers' spend manually.
type OpenAIUsageConfig struct {
	APIKey       *string `yaml:"api_key,omitempty"`
	Org          *string `yaml:"org,omitempty"`
	PollInterval *string `yaml:"poll_interval,omitempty"` // e.g. "1h"; default 1h; min 5m
}

// HTTPConfig groups listen-address and the secret fields. Bearer-token auth
// (api_token — the write/admin scope; read_token — the read-only viewer scope,
// #190) is an HTTP-layer concern; webhook_secret signs incoming GitHub
// deliveries — also HTTP-layer.
type HTTPConfig struct {
	Addr          *string `yaml:"addr,omitempty"`
	WebhookSecret *string `yaml:"webhook_secret,omitempty"`
	APIToken      *string `yaml:"api_token,omitempty"`
	// ReadToken is the read-only viewer bearer token (#190): a second credential
	// authorized ONLY on the GET read routes (/scores, /scores/{developer},
	// /metrics — the data the dashboard renders) and rejected on every write
	// route. Same secret-provisioning rules as api_token: CLI --read-token, the
	// TIER_READ_TOKEN env var, and @/path/to/file indirection all resolve through
	// the same code path. It never relaxes the fail-closed bind rule — a
	// non-loopback bind still requires api_token. Absent → nil → no read scope.
	ReadToken *string `yaml:"read_token,omitempty"`
	// TrustedProxyCIDRs lists the reverse-proxy/TLS-terminator CIDRs whose
	// X-Forwarded-For the failed-auth lockout may believe (#131). A slice, not a
	// pointer: list-valued, and any --trusted-proxy-cidr on the CLI discards this
	// config list ENTIRELY (CLI-wins, no partial merge — same as watch.repos),
	// with a WARN. Absent → nil → the CLI flag / default (never trust XFF) wins.
	TrustedProxyCIDRs []string `yaml:"trusted_proxy_cidrs,omitempty"`
	// Auth carries the per-IP failed-auth lockout knobs (#85, gating #36's
	// limiter). A nested struct rather than a pointer: its own fields carry the
	// present-vs-absent discipline, so an omitted `http.auth` block simply leaves
	// all three pointers nil and every builtin default in place.
	Auth AuthConfig `yaml:"auth"`
}

// AuthConfig mirrors the three --auth-* flags into the config schema (#85): the
// per-IP failed-auth lockout (#36) was the lone serve knob configurable only via
// flag/env, not YAML. Precedence, enforced by the caller: CLI flag > env var (none
// exists for these) > this config block > builtin default.
//
// FailureWindow and Lockout are *string, NOT *time.Duration, on purpose:
// go.yaml.in/yaml/v3 decodes a bare YAML integer into time.Duration as NANOSECONDS
// (so `lockout: 900` would silently mean 900ns, not 15m) and refuses to decode a
// human string like "15m" into a Duration at all. Keeping them as strings lets the
// value flow through flag.FlagSet.Set, where the SAME time.ParseDuration the CLI
// --auth-lockout flag uses is the single validator — a bad `failure_window: banana`
// fails loud at startup with an identical parse error whether it came from the flag
// or the file, closing the config-vs-flag divergence class by construction rather
// than reimplementing duration parsing here.
type AuthConfig struct {
	// MaxFailures mirrors --auth-max-failures: per-IP failed-auth attempts within
	// FailureWindow before a 429 lockout. 0 disables the limiter. Absent → nil →
	// the flag default (10). A pointer so an explicit `max_failures: 0` (disable)
	// is distinguishable from an absent key.
	MaxFailures *int `yaml:"max_failures,omitempty"`
	// FailureWindow mirrors --auth-failure-window: the sliding window over which
	// MaxFailures is counted, as a Go duration string (e.g. "60s"). Absent → nil →
	// the flag default (60s). See the type doc for why this is a string.
	FailureWindow *string `yaml:"failure_window,omitempty"`
	// Lockout mirrors --auth-lockout: how long a tripped IP stays locked out (429),
	// as a Go duration string (e.g. "15m"). Absent → nil → the flag default (15m).
	// See the type doc for why this is a string.
	Lockout *string `yaml:"lockout,omitempty"`
}

// ProxyConfig holds the upstream URLs for each provider's reverse proxy.
// Setting a field to "" explicitly in YAML disables that provider's proxy
// (parseProxyTarget in cmd/tierd treats empty as "no proxy").
type ProxyConfig struct {
	AnthropicTarget *string `yaml:"anthropic_target,omitempty"`
	OpenAITarget    *string `yaml:"openai_target,omitempty"`
	// GeminiTarget mounts /gemini/ (#459 task 4). The parser
	// (internal/proxy's parseGemini) has existed since v1 (#1), extended by
	// #122 (thinking/cache tokens) and #300 (host stamping); only the mount
	// was missing, so pointing a Gemini client here before this landed
	// 404'd. Defaults to "https://generativelanguage.googleapis.com".
	GeminiTarget *string `yaml:"gemini_target,omitempty"`
}

// WatchConfig holds the JSONL watch-repo list. Setting Repos to an explicit
// empty list `[]` disables live ingestion, same as omitting all
// --watch-repo flags.
type WatchConfig struct {
	Repos []string `yaml:"repos,omitempty"`
	// RepoSlugs maps a watched repository path to its canonical "owner/repo"
	// identity (#231). Absent entries fall back to the repo's remote.origin.url,
	// then to the 'unqualified' sentinel (logged once).
	//
	// It is a separate map rather than a richer `repos:` element type on purpose:
	// `repos` is a documented []string in every shipped config and quickstart, and
	// silently changing its shape would break existing deployments on upgrade.
	//
	// Set it when a developer works from a FORK: their remote.origin.url reads
	// "alice/tier" while the upstream webhook reports "tiermetric/tier", so without
	// an override their cost never joins their outcomes.
	//
	//	watch:
	//	  repos:
	//	    - /Users/alice/src/tier
	//	  repo_slugs:
	//	    /Users/alice/src/tier: tiermetric/tier
	RepoSlugs map[string]string `yaml:"repo_slugs,omitempty"`
}

// Load reads a YAML config from path. Returns a descriptive error for the
// three failure modes that have distinct operator responses:
//   - file not found
//   - file unreadable (permission)
//   - file present but invalid YAML or contains unknown keys
//
// Strict parsing (KnownFields) catches typo'd keys (e.g. `wacht_repos`
// instead of `watch_repos`) so a misconfiguration surfaces at startup
// instead of silently using flag defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("config: path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// os.ReadFile distinguishes ENOENT vs EACCES vs other in the wrapped
		// PathError; the caller can inspect with errors.Is(err, os.ErrNotExist)
		// if it needs to.
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown keys
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	// Semantic validation of values the strict decoder can't catch. Kept here (not
	// in the caller) for outcomes.size_labels specifically because an off-scale
	// weight is a data-model violation — scores stop being comparable across orgs —
	// so it must fail loud at startup regardless of which caller wires the value
	// through (#244).
	if err := validateSizeLabels(cfg.Outcomes.SizeLabels); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	// Subscriptions feed money math and a UTC-month key (#113/#155). A malformed
	// block must fail the load rather than surface later as a $0 fee, a CHECK
	// violation on INSERT, or a backfill that posts nothing.
	if err := validateSubscriptions(cfg.Subscriptions, time.Now()); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}
