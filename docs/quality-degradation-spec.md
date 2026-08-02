# TIER Quality Multiplier — Degradation Rules Specification
## Deterministic State Machine for Post-Merge Quality Assessment

> ⚠️ **Implementation status (as of 2026-07-12, `main`).** This document
> specifies the **full, target** 8-event quality model. `tierd` implements a
> **subset** of it today (#134). Quality is **derived, not mutated**: each signal
> is appended to an append-only `quality_events` log and the outcome's quality is
> recomputed as the **worst-of** the applicable floors, clamped to `[0.1, 1.0]`
> (`internal/quality.Resolve`, driven from `internal/webhook/handler.go`).
>
> **Implemented today:**
> - **Event 1 — CI passes** (`ci_pass`; quality stays `1.0`), including the
>   30-minute same-SHA flaky-rerun neutralisation of an earlier failure.
> - **Event 2 — CI fails on the merge commit** → floor **`0.7`**, within the
>   48-hour window (from the `workflow_run` webhook).
> - **Event 4 — full revert** → the strategic-vs-quality classification IS
>   performed: a **quality** revert floors to **`0.1`**, a **strategic** revert
>   to **`0.8`**, within the 60-day window (from the `push` webhook).
>
> **NOT yet implemented** (specified below, but no code enforces them):
> - **Event 3** — follow-up fix (`-0.15`)
> - **Event 5** — partial revert (`1.0 - 0.9·fraction`)
> - **Event 6** — production-incident correlation
> - **Event 7** — hotfix-branch detection (`0.4`)
> - **Event 8** — downstream-CI failure (`-0.20` per service)
> - the **no-CI `0.95`** "unverified" penalty (the Event 1 edge case), the
>   cross-PR flaky registry, and the persisted provisional → observing → final
>   phase state machine / dashboard provisional indicator (the 48h/60d bounds ARE
>   applied as event windows, but the lifecycle state is not materialized).
>
> Treat Sections 3–12 as the intended design; the bullets above are what the tool
> measures today. (Ref: readiness review finding C2 — publishing this spec
> as-if-fully-implemented would misrepresent what the tool measures.)

**Specification dated:** March 26, 2026 — the date this design was written. See the implementation-status banner above for what the code enforces today.

**Author:** TIER maintainers

**Status:** Complete specification, partially implemented. The design below is settled and not a draft; `tierd` enforces the subset itemized in the banner above. "Final" here describes the specification, not the shipped behavior.

**Design constraint:** 100% automated. Zero human input. The quality multiplier is a pure function of observable system events.

---

## 1. Governing Principles

1. **Innocence by default.** Every merged PR starts at quality = 1.0. The system only degrades; it never requires proof of quality.
2. **Degradations stack via worst-of.** When multiple degradation events apply to the same PR, the system takes the **minimum** (worst) quality value, not the sum of penalties. Rationale: stacking penalties would double-punish cascading failures (CI fails, then a revert follows, which is the expected response to a CI failure -- the revert should not be an additional penalty on top of the failure it corrected).
3. **One exception to worst-of: follow-up fix is additive with CI failure.** A CI failure (0.7) followed by a follow-up fix (penalty of -0.15) compounds to 0.55, because these are genuinely independent quality signals. See Section 3 for the precise rule.
4. **Observation window is two-phase.** Phase 1 (48 hours): fast signals lock. Phase 2 (60 days): slow signals can still degrade. After 60 days, quality is FINAL and immutable.
5. **No human override endpoint.** The adversarial analysis (O-02) correctly identifies that override mechanisms become gaming vectors. Instead, every edge case has a deterministic rule that handles it without judgment.

---

## 2. Observation Window Design

### Why two phases?

The existing architecture doc proposed 48 hours for initial finalization and 60 days for the full window. This is correct, but the two phases serve distinct purposes that need to be explicit.

### Phase 1: Immediate Verification (0-48 hours post-merge)

**Purpose:** Catch build breaks, CI failures, and obvious merge problems.

**What it covers:**
- CI pass/fail on the merge commit
- Flaky CI re-run resolution
- Immediate revert (someone notices a break and reverts within hours)

**Behavior:** Quality is "provisional" during this phase. Dashboard marks the score with a provisional indicator. At T+48h, whatever quality value exists becomes the Phase 1 baseline.

**Why 48 hours, not 24:** Many teams have CI pipelines that run integration tests on a nightly or twice-daily cadence. A 24-hour window would miss the first integration test cycle for PRs merged in the afternoon. 48 hours guarantees at least one full CI cycle on any merge schedule.

### Phase 2: Regression Observation (48 hours - 60 days post-merge)

**Purpose:** Catch production regressions, follow-up fixes, reverts, downstream failures, and incident correlation.

**What it covers:**
- Follow-up fix commits touching the same files
- Full or partial reverts
- Production incidents correlated to deploys containing this PR
- Hotfix branches referencing this PR's issue
- Downstream CI failures in other services

**Behavior:** Quality can only degrade during Phase 2 (never improve). At T+60 days, quality is locked as FINAL.

**Why 60 days, not 30:** The adversarial analysis (G-04) identified that 30 days does not cover monthly release cycles or slow-rolling production issues. 60 days covers two full monthly release cycles and captures the vast majority of regression signals. Anything beyond 60 days is either a strategic revert (not a quality signal) or a latent bug so subtle that penalizing the original author is unfair.

### Post-window (60+ days)

Quality is FINAL. No further events can modify it. If a regression surfaces at day 61+, it contributes to the team-level "long-tail regression" sidecar metric but does not affect the individual PR's quality score. This is a deliberate design choice: at some point, responsibility shifts from the author to the system.

---

## 3. Event-by-Event Degradation Rules

### Event 1: CI Passes Cleanly on Merge

**Trigger signal:** `workflow_run` webhook with `conclusion: "success"` for the merge commit SHA on the target branch.

**Degradation:** None. Quality remains at 1.0.

**Detection logic:**
```
ON tier.ci.run WHERE
    head_sha = pr.merge_commit_sha AND
    conclusion = "success" AND
    branch = pr.base_branch
SET quality_event(pr_id, "ci_pass", timestamp)
```

**Edge case -- no CI configured:** If no `workflow_run` event arrives within 48 hours of merge, the system cannot verify CI. Quality receives a -0.05 penalty (quality = 0.95) as a mild incentive to adopt CI. This is NOT a failure penalty; it is an "unverified" penalty. The dashboard labels it "no CI signal" rather than "CI failed."

---

### Event 2: CI Fails on the Merge Commit

**Trigger signal:** `workflow_run` webhook with `conclusion: "failure"` for the merge commit SHA on the target branch.

**Degradation:** quality = 0.7

**Detection logic:**
```
ON tier.ci.run WHERE
    head_sha = pr.merge_commit_sha AND
    conclusion = "failure" AND
    branch = pr.base_branch

# Check flaky test registry before applying penalty
IF failure_test_names INTERSECT flaky_registry(7_day_window) != EMPTY:
    # All failing tests are known-flaky -- do not penalize
    SET quality_event(pr_id, "ci_fail_flaky", timestamp)
    # quality stays at 1.0
ELSE IF failure_test_names PARTIAL_INTERSECT flaky_registry(7_day_window):
    # Some tests are flaky, some are genuine failures
    SET quality_event(pr_id, "ci_fail_partial_flaky", timestamp)
    # quality = 0.7 (genuine failures present)
ELSE:
    SET quality_event(pr_id, "ci_fail", timestamp)
    # quality = 0.7
```

**Flaky CI handling (the 30-minute re-run rule):**

A test is classified as "flaky" if it meets BOTH of the following:
1. The same test name has failed on at least 2 unrelated PRs (different authors, different file sets) in the last 7 days.
2. OR: The CI run fails, then a re-run of the exact same commit SHA succeeds within 30 minutes with no code changes.

When a CI failure is fully attributable to known-flaky tests:
- Quality remains at 1.0
- The event is logged as `ci_fail_flaky` for audit purposes
- The flaky test is added/refreshed in the flaky registry with a 7-day TTL

When a CI failure contains a mix of flaky and non-flaky test failures:
- Quality = 0.7 (the genuine failure is real)
- The flaky tests are noted in the event metadata but do not mitigate the penalty

**The flaky registry is automated:**
```
Flaky Registry Rules:
- Entry: (test_name, repo, first_seen, last_seen, failure_count, unrelated_pr_count)
- A test enters the registry when it fails on >= 2 PRs with disjoint file sets within 7 days
- TTL: 7 days from last_seen. If the test stops being flaky, it ages out.
- A test is removed from the registry if it passes on 10 consecutive CI runs
- The registry is per-repository, not global
```

**Why 0.7 specifically:** The adversarial analysis (P-04) notes the steep cliff between CI failure (0.7) and regression (0.3). The 0.7 value represents a 30% reduction -- significant enough to incentivize pre-merge CI hygiene, but not catastrophic. A CI failure is a "you should have caught this" signal, not a "you broke production" signal. The 30% penalty is calibrated to roughly offset the weight-1 bonus of a trivial task, ensuring that a CI failure on a trivial task effectively zeroes its contribution.

---

### Event 3: Follow-Up Fix Commit Touches Same Files Within N Days

**Trigger signal:** A new commit on the target branch that:
1. Modifies at least one file that was also modified by the original PR, AND
2. Is part of a PR/issue that references the original PR/issue (via commit message, PR body, or issue link), OR
3. Is authored by a different developer AND touches >= 50% of the same files within 14 days (even without an explicit reference)

**Degradation:** quality -= 0.15 (from whatever the current quality is)

**Observation sub-window:** 14 days from merge. Follow-up fixes after 14 days are considered normal maintenance, not quality signals from the original PR.

**Detection logic:**
```
ON tier.git.push WHERE
    branch = target_branch AND
    timestamp <= pr.merged_at + 14_days

FOR each commit in push:
    changed_files = commit.files_changed
    FOR each recent_pr in prs_merged_in_last_14_days:
        file_overlap = changed_files INTERSECT recent_pr.files_changed

        IF file_overlap IS NOT EMPTY:
            # Check for explicit reference
            has_reference = (
                commit.message MATCHES /(?:fixes|closes|follow-up|followup|caused by|regression from)\s*#?{recent_pr.issue_id}/i
                OR commit.pr_body MATCHES /(?:fixes|closes|follow-up|followup|caused by|regression from)\s*#?{recent_pr.issue_id}/i
            )

            # Check for implicit signal (different author, high file overlap)
            is_implicit_fix = (
                commit.author != recent_pr.author AND
                |file_overlap| / |recent_pr.files_changed| >= 0.5
            )

            IF has_reference OR is_implicit_fix:
                SET quality_event(recent_pr.id, "followup_fix", timestamp, {
                    fixing_commit: commit.sha,
                    file_overlap: file_overlap,
                    detection_method: "explicit" if has_reference else "implicit"
                })
                # quality = current_quality - 0.15
```

**Why -0.15 (subtractive) instead of a fixed floor:** A follow-up fix after a clean merge (1.0 - 0.15 = 0.85) is different from a follow-up fix after a CI failure (0.7 - 0.15 = 0.55). The first case is "minor issue, you missed something." The second case is "CI already told you something was wrong, and someone else had to clean up more." The subtractive model captures this compound signal naturally.

**Stacking rule for multiple follow-up fixes:** The -0.15 penalty applies only once. If three different developers each submit follow-up fixes to the same original PR's files, the original PR's quality degrades by 0.15 total, not 0.45. Rationale: one quality problem can manifest in multiple fix commits. The problem count is one; the fix count is irrelevant.

**Edge case -- quality improvement follow-up:** See Section 4 (Edge Cases), item 4.

---

### Event 4: Full Revert of the PR Within 60 Days

**Trigger signal:** A commit on the target branch that:
1. Has a commit message matching `Revert "<original PR title>"` or `revert: <description>` referencing the original PR, OR
2. Is detected by `git revert --find-object=<merge_commit_sha>`, OR
3. Removes >= 90% of the lines added by the original PR (measured by inverse diff analysis)

**Degradation:** quality = 0.1

**Observation window:** 60 days (the full Phase 2 window).

**Detection logic:**
```
ON tier.git.push WHERE
    branch = target_branch AND
    timestamp <= pr.merged_at + 60_days

FOR each commit in push:
    # Method 1: Commit message pattern matching
    is_revert_by_message = (
        commit.message MATCHES /^Revert ".*"/i
        OR commit.message MATCHES /revert.*#\d+/i
        OR commit.message MATCHES /revert.*{pr.merge_commit_sha[:8]}/i
    )

    # Method 2: Inverse diff analysis
    IF NOT is_revert_by_message:
        original_additions = lines_added_by(pr)
        commit_deletions = lines_deleted_by(commit)
        overlap = original_additions INTERSECT commit_deletions
        is_revert_by_content = |overlap| / |original_additions| >= 0.90

    IF is_revert_by_message OR is_revert_by_content:
        # Determine revert reason via commit message classification
        reason = classify_revert_reason(commit.message, commit.pr_body)

        IF reason == "strategic":
            # Product decision, not quality failure
            SET quality_event(pr_id, "revert_strategic", timestamp)
            # quality = 0.8 (not the author's fault, but work did not survive)
        ELSE:
            # Quality, performance, or stability revert
            SET quality_event(pr_id, "revert_quality", timestamp)
            # quality = 0.1
```

**Revert reason classification (deterministic, no LLM):**

The revert commit message and PR body are scanned for keyword patterns:

```
STRATEGIC_PATTERNS = [
    /product decision/i,
    /PM requested/i,
    /feature flag.*disable/i,
    /business requirement.*changed/i,
    /pivot/i,
    /deprecat/i,
    /sunset/i,
    /removing feature/i,
    /no longer needed/i,
    /replaced by/i
]

QUALITY_PATTERNS = [
    /broke/i, /break/i, /broken/i,
    /crash/i, /OOM/i, /out of memory/i,
    /regression/i, /degradation/i,
    /incident/i, /outage/i,
    /bug/i, /defect/i,
    /performance.*degrad/i,
    /memory leak/i,
    /data loss/i, /corrupt/i,
    /timeout/i, /deadlock/i,
    /security.*vuln/i
]

classify_revert_reason(message, body):
    text = message + " " + (body or "")
    strategic_hits = count(pattern in STRATEGIC_PATTERNS if pattern.match(text))
    quality_hits = count(pattern in QUALITY_PATTERNS if pattern.match(text))

    IF strategic_hits > 0 AND quality_hits == 0:
        RETURN "strategic"
    ELSE:
        # Default to quality. When in doubt, the conservative
        # interpretation is that the code had a problem.
        RETURN "quality"
```

**Why 0.1 for quality reverts:** A full revert means the code was bad enough that the fastest path to recovery was complete removal. This is the second-worst outcome (only "AI code discarded before merge" at 0.0 is worse, but that case never enters the quality multiplier because the PR was never merged). The 0.1 is not zero because the developer still did work that informed what NOT to do -- there is marginal learning value, and a 0.0 multiplier would make the denominator (tokens spent) entirely wasted, which overpunishes.

**Why 0.8 for strategic reverts:** The developer wrote working code that was removed for business reasons. They should not receive full credit (the code is gone), but they should not be heavily penalized either. 0.8 represents a 20% haircut -- "the work did not survive, but that is not your fault."

---

### Event 5: Partial Revert (Some Files Reverted, Others Kept)

**Trigger signal:** A commit that reverts SOME but not ALL of the original PR's changes. Specifically:
- Removes between 20% and 89% of the lines added by the original PR (below 20% is noise; above 90% is a full revert per Event 4)

**Degradation:** quality = 1.0 - (0.9 * revert_fraction)

Where `revert_fraction` = (lines from original PR that were removed) / (total lines added by original PR)

**The math:**
```
revert_fraction = |lines_removed_from_original| / |lines_added_by_original|

# Clamp to the partial revert range
IF revert_fraction < 0.20:
    # Below threshold: not a meaningful revert, ignore
    RETURN (no degradation)

IF revert_fraction >= 0.90:
    # Full revert: handle under Event 4 rules
    RETURN Event_4_handling

# Partial revert degradation
quality = max(0.1, 1.0 - (0.9 * revert_fraction))

# Examples:
#   20% reverted: quality = 1.0 - 0.18 = 0.82
#   30% reverted: quality = 1.0 - 0.27 = 0.73
#   50% reverted: quality = 1.0 - 0.45 = 0.55
#   70% reverted: quality = 1.0 - 0.63 = 0.37
#   89% reverted: quality = 1.0 - 0.80 = 0.20
```

**Observation window:** 60 days (same as full revert).

**Strategic vs. quality classification:** Same keyword-based classification as Event 4. If the partial revert is strategic, apply a reduced haircut: quality = 1.0 - (0.2 * revert_fraction). This means a 50% strategic partial revert yields quality = 0.90 rather than 0.55.

**Detection logic:**
```
ON tier.git.push WHERE
    branch = target_branch AND
    timestamp <= pr.merged_at + 60_days

FOR each commit in push:
    FOR each recent_pr in prs_merged_in_last_60_days:
        original_additions = lines_added_by(recent_pr)
        commit_deletions = lines_deleted_by(commit) INTERSECT original_additions

        revert_fraction = |commit_deletions| / |original_additions|

        IF 0.20 <= revert_fraction < 0.90:
            reason = classify_revert_reason(commit.message, commit.pr_body)
            IF reason == "strategic":
                quality = 1.0 - (0.2 * revert_fraction)
            ELSE:
                quality = max(0.1, 1.0 - (0.9 * revert_fraction))

            SET quality_event(recent_pr.id, "partial_revert", timestamp, {
                revert_fraction: revert_fraction,
                reason: reason,
                quality_value: quality
            })
```

---

### Event 6: Production Incident Correlated to a Deploy Containing This PR

**Trigger signal:** An incident webhook (`incident.triggered` from PagerDuty, OpsGenie, Datadog, or equivalent) where:
1. The incident fires within the observation window of any PR merged in the last 60 days, AND
2. The deploy that is running when the incident fires contains the PR's merge commit (determined by deploy manifest / release tag / commit range in the deploy), AND
3. The incident's affected service matches the PR's target repository or a declared downstream dependency

**Degradation:** Depends on incident severity:

| Severity | Quality | Signal |
|----------|---------|--------|
| P0 (full outage, data loss) | 0.2 | `incident.severity = "P0" OR incident.severity = "SEV1"` |
| P1 (major degradation, >10% users) | 0.3 | `incident.severity = "P1" OR incident.severity = "SEV2"` |
| P2 (minor degradation, <10% users) | 0.5 | `incident.severity = "P2" OR incident.severity = "SEV3"` |
| P3 (cosmetic, no user impact) | 0.7 | `incident.severity = "P3" OR incident.severity = "SEV4"` |

**Attribution logic -- the hard part:**

A deploy typically contains multiple PRs. Which one caused the incident? This is the single most difficult attribution problem in the entire quality system.

**Solution: Deploy-window proportional attribution.**

```
ON tier.incident.new WHERE incident.service IN monitored_services:

# Step 1: Identify the deploy that was running when the incident fired
deploy = get_active_deploy(incident.service, incident.fired_at)

# Step 2: Get all PRs in that deploy
prs_in_deploy = get_prs_in_deploy(deploy)

# Step 3: Score each PR's causal likelihood using deterministic signals
FOR each pr in prs_in_deploy:
    pr.causal_score = compute_causal_score(pr, incident)

# Step 4: Apply penalty only to PRs above the causal threshold
FOR each pr in prs_in_deploy WHERE pr.causal_score >= CAUSAL_THRESHOLD:
    severity_quality = severity_to_quality(incident.severity)
    # Scale the penalty by causal confidence
    pr.quality = 1.0 - (pr.causal_score * (1.0 - severity_quality))
    SET quality_event(pr.id, "incident_correlated", timestamp, {
        incident_id: incident.id,
        severity: incident.severity,
        causal_score: pr.causal_score,
        quality_value: pr.quality
    })
```

**Causal score computation (deterministic, no LLM):**

```
CAUSAL_THRESHOLD = 0.4

compute_causal_score(pr, incident):
    score = 0.0

    # Signal 1: File overlap with error stack trace (if available)
    # Weight: 0.35
    IF incident.stack_trace IS NOT NULL:
        error_files = extract_files_from_stacktrace(incident.stack_trace)
        overlap = pr.files_changed INTERSECT error_files
        IF |overlap| > 0:
            score += 0.35 * (|overlap| / |error_files|)

    # Signal 2: Service/package match
    # Weight: 0.25
    IF pr.target_repo == incident.service_repo:
        score += 0.15
        IF pr.packages_modified INTERSECT incident.affected_packages:
            score += 0.10

    # Signal 3: Recency (more recent PRs are more likely causal)
    # Weight: 0.20
    hours_since_merge = (incident.fired_at - pr.merged_at).total_hours()
    recency_score = max(0, 1.0 - (hours_since_merge / (60 * 24)))  # linear decay over 60 days
    score += 0.20 * recency_score

    # Signal 4: Change risk profile
    # Weight: 0.20
    risk = 0.0
    IF pr.modifies_database_migrations: risk += 0.08
    IF pr.modifies_api_surface: risk += 0.06
    IF pr.modifies_auth_or_security: risk += 0.06
    IF pr.lines_changed > 500: risk += 0.04  # large changes carry more risk
    IF pr.files_changed_count > 20: risk += 0.04
    score += min(0.20, risk)

    RETURN min(1.0, score)
```

**When causal attribution is uncertain:** If no PR in the deploy scores above the CAUSAL_THRESHOLD (0.4), the incident is logged as "unattributed incident" at the team level. No individual PR's quality is affected. This prevents the system from randomly penalizing innocent PRs when the root cause cannot be determined from signals alone.

**Edge case -- cascading failure across deploys:** If the incident spans multiple services, each service's deploy is analyzed independently. A PR only gets penalized via the deploy of its own target repository, not via downstream service incidents. Downstream failures are handled by Event 8.

---

### Event 7: Hotfix Branch Created Referencing This PR's Issue

**Trigger signal:** A branch is created matching the pattern `hotfix/*` that:
1. References the original PR's issue number in the branch name (e.g., `hotfix/123-fix-auth-crash`), OR
2. Has its first commit message reference the original issue (e.g., `hotfix: fix crash introduced by #123`)

**Degradation:** quality = 0.4

**Rationale:** A hotfix is stronger than a follow-up fix but weaker than a full revert. Someone created an emergency branch to fix something the original PR broke. This is a clear signal that the original code caused a production-grade problem, but the fix was targeted rather than a wholesale rollback.

**Detection logic:**
```
ON tier.git.branch WHERE
    branch_name MATCHES /^hotfix\//

# Extract issue references from branch name
issue_refs = extract_issue_ids(branch_name)

# Also check the first commit on the branch
first_commit = get_first_commit_on_branch(branch_name)
issue_refs += extract_issue_ids(first_commit.message)

FOR each issue_id in issue_refs:
    original_pr = get_pr_for_issue(issue_id)
    IF original_pr IS NOT NULL AND
       original_pr.merged_at + 60_days >= now():
        SET quality_event(original_pr.id, "hotfix_branch", timestamp, {
            hotfix_branch: branch_name,
            referenced_issue: issue_id
        })
        # quality = 0.4
```

**Why 0.4:** Positioned between follow-up fix (0.85 from a clean start) and full revert (0.1). A hotfix means the code caused an urgent problem severe enough to bypass normal workflow, but the code was salvageable (unlike a full revert). The 0.4 value aligns with the P1 incident severity (0.3) plus a small margin because not all hotfixes are incident-driven -- some are proactive catches before incidents fire.

**Interaction with Event 6:** If a hotfix is created AND a production incident fires for the same issue, the system takes the worst-of: min(0.4, incident_quality). The hotfix event does not stack additional penalty on top of an incident.

---

### Event 8: PR Causes Downstream CI Failure in Another Service

**Trigger signal:** A CI failure in Service B where:
1. Service B's CI runs integration tests against Service A, AND
2. Service A's latest deploy includes the PR under evaluation, AND
3. The CI failure in Service B started occurring AFTER the deploy containing this PR, AND
4. The CI failure in Service B did NOT exist in the CI run immediately before the deploy

**Degradation:** quality -= 0.20 (subtractive, same as follow-up fix logic)

**Observation window:** 14 days from the deploy containing the PR. Downstream failures beyond 14 days are too far removed to attribute with confidence.

**Detection logic:**
```
ON tier.ci.run WHERE
    conclusion = "failure" AND
    repo != pr.target_repo  # Different service

# Step 1: Check if this service depends on the PR's target service
IF NOT dependency_graph.depends_on(ci_run.repo, pr.target_repo):
    SKIP  # No declared dependency, no attribution

# Step 2: Check temporal causality
deploy = get_most_recent_deploy(pr.target_repo, before=ci_run.started_at)
IF pr.merge_commit_sha NOT IN deploy.commit_range:
    SKIP  # This PR was not in the relevant deploy

# Step 3: Check that this failure is new (not pre-existing)
previous_ci_run = get_previous_ci_run(ci_run.repo, ci_run.workflow)
IF previous_ci_run.conclusion == "failure":
    SKIP  # This failure pre-dates the deploy; not caused by this PR

# Step 4: Verify via flaky registry
IF failure_test_names ALL IN flaky_registry(ci_run.repo, 7_day_window):
    SKIP  # All failures are known-flaky in the downstream service

# Attribution confirmed
SET quality_event(pr.id, "downstream_ci_failure", timestamp, {
    downstream_repo: ci_run.repo,
    failing_tests: failure_test_names,
    deploy_id: deploy.id
})
# quality = current_quality - 0.20
```

**Cascading failure rule (PR A is fine, PR B breaks PR A's functionality):**

This is the "who gets penalized" problem. The rule is: **the PR whose deploy caused the failure gets penalized, not the PR whose code was broken.**

Rationale: PR A was working before PR B was deployed. PR B introduced the breaking change. PR B's author is responsible for verifying that their change does not break existing contracts. PR A's quality is unaffected.

Concretely: if PR B in Service X causes Service Y's CI to fail, and Service Y's CI tests exercise functionality introduced by PR A (merged weeks ago), then:
- PR B receives the -0.20 downstream CI penalty
- PR A's quality is unchanged

The temporal causality check (Step 3 above) enforces this: only PRs in the deploy that caused the NEW failure are candidates for attribution.

**Stacking rule:** Like follow-up fixes, the downstream CI penalty applies at most once per downstream service. If PR X breaks CI in three different downstream services, the total penalty is -0.20 * 3 = -0.60 (with a floor at 0.1). This is intentional: breaking three services is worse than breaking one.

---

## 4. Edge Cases

### Edge Case 1: Flaky CI -- Test Fails on Merge, Passes on Re-Run Within 30 Minutes

**Rule:** If the exact same commit SHA passes CI on a re-run within 30 minutes of the initial failure, AND no code changes were pushed between the failure and the re-run, the failure is retroactively classified as flaky.

**Implementation:**
```
ON tier.ci.run WHERE
    head_sha = previously_failed_sha AND
    conclusion = "success" AND
    timestamp <= original_failure.timestamp + 30_minutes AND
    no_push_events_between(original_failure.timestamp, timestamp)

# Retroactively reclassify the failure
UPDATE quality_event SET
    event_type = "ci_fail_flaky",
    quality_impact = NULL  # no penalty
WHERE
    pr_id = affected_pr.id AND
    event_type = "ci_fail" AND
    ci_run_sha = previously_failed_sha

# Add to flaky registry
flaky_registry.upsert(
    test_name = original_failure.failing_tests,
    repo = original_failure.repo,
    last_seen = timestamp
)
```

**What if the re-run happens after 30 minutes?** The 30-minute window is firm. Beyond 30 minutes, the system cannot distinguish between "flaky test" and "transient infrastructure issue that was silently fixed." The penalty stands, but the event is annotated with "re-run passed at T+{minutes}" for transparency.

**What if someone pushes a fix and then re-runs?** If a push event exists between the failure and the re-run, it is NOT a flaky test -- it is a follow-up fix. The CI failure penalty (0.7) applies, and if the fix constitutes a follow-up fix under Event 3 rules, that penalty applies too.

---

### Edge Case 2: Cascading Failure -- PR A is Fine, PR B Breaks PR A's Functionality

**Rule:** Addressed in Event 8 above. The causal PR (PR B) is penalized. The broken PR (PR A) is not. Temporal causality is the deciding factor.

**What about the case where PR A introduced a latent fragility that PR B exposed?** Example: PR A adds a function that works but does not validate input. PR B sends unexpected input. Whose fault is it?

**Rule:** PR B is penalized. The reasoning: PR A was working within its specified contract. PR B introduced a new interaction that violated that contract. Even if PR A "should have" validated input, the system has no way to deterministically identify latent fragilities -- only actual failures. PR A's quality reflects what happened, not what might have happened.

If the team subsequently identifies PR A's fragility and creates a follow-up fix for it, Event 3 will apply to PR A at that point, degrading its quality by -0.15. This is the correct outcome: the fragility was not a quality problem until it manifested.

---

### Edge Case 3: Long-Tail Regression -- Incident Happens 45 Days After Merge

**Rule:** 45 days is within the 60-day observation window. The incident attribution process (Event 6) runs normally. The causal score's recency signal will be low (45/60 of the decay applied), which reduces the causal score but does not eliminate it. If the stack trace and file overlap signals are strong, the PR will still score above the CAUSAL_THRESHOLD and receive the incident penalty.

**What about an incident at day 61?** The observation window is closed. The PR's quality is FINAL. The incident contributes to the team-level "long-tail regression" metric but does not affect the individual PR score.

**Rationale:** Any system needs a statute of limitations. Code that has been running in production for 60 days without incident has survived a meaningful burn-in period. If it fails at day 61, the failure is as much a function of environmental change (load growth, dependency updates, config drift) as original code quality. Punishing a 2-month-old PR for a new failure is not a useful quality signal.

---

### Edge Case 4: Quality Improvement -- Follow-Up Commit IMPROVES the Original

**Rule:** Quality can only degrade, never improve, during the observation window. A follow-up commit that adds tests, improves error handling, or enhances the original code does NOT increase the quality multiplier above its current value.

**Rationale for no positive adjustment:**

1. **Gaming vector.** If improvements could raise quality, developers would submit intentionally incomplete PRs at 0.8, then submit a "follow-up" to boost to 1.1 (or some bonus). The follow-up is trivial -- add a few test cases, improve an error message -- but the TIER numerator inflates.

2. **Asymmetric information.** The system cannot deterministically distinguish between "genuine improvement to already-good code" and "fixing an omission that should have been in the original PR." A human can tell the difference; the system cannot. Since this specification requires zero human input, the safe default is to not grant positive credit for something that might be a disguised fix.

3. **The correct metric for improvements already exists.** The follow-up improvement is itself a separate PR with its own outcome weight and quality score. The developer gets credit for the improvement through its own TIER contribution. Double-counting it as a quality boost to the original PR inflates the numerator twice for one piece of work.

**How to detect an improvement (for logging, not for scoring):**

```
is_improvement = (
    commit.files_changed SUBSET_OF pr.files_changed AND
    commit.message MATCHES /(?:add.*test|improve.*error|enhance|refactor|clean.*up|document)/i AND
    commit.lines_added > commit.lines_deleted AND  # Net addition, not replacement
    NOT commit.message MATCHES /(?:fix|bug|broken|regression|hotfix)/i  # Not a fix
)

IF is_improvement:
    LOG quality_event(pr.id, "followup_improvement", timestamp)
    # quality unchanged -- logged for analytics only
```

**Exception -- recovery from CI failure:** If a PR has quality = 0.7 due to CI failure (Event 2), and a follow-up commit fixes the CI failure and CI subsequently passes, the quality does NOT recover to 1.0. The CI failure happened; the fact that it was fixed is good, but the quality signal (code was merged in a broken state) is real. The fix itself is its own PR/commit with its own quality score.

---

## 5. Quality Resolution Algorithm

When multiple events affect the same PR, the final quality is computed by this algorithm:

```
resolve_quality(pr_id):
    events = get_quality_events(pr_id)

    IF events IS EMPTY:
        RETURN 1.0  # No degradation events, clean ship

    # Separate events into categories
    floor_events = []      # Events that set an absolute quality floor
    additive_events = []   # Events that subtract from quality

    FOR each event in events:
        SWITCH event.type:
            CASE "ci_pass":
                CONTINUE  # No effect

            CASE "ci_fail_flaky":
                CONTINUE  # No effect (retroactively cleared)

            CASE "ci_fail", "ci_fail_partial_flaky":
                floor_events.append(0.7)

            CASE "followup_fix":
                additive_events.append(-0.15)  # Applies once regardless of count

            CASE "revert_quality":
                floor_events.append(0.1)

            CASE "revert_strategic":
                floor_events.append(0.8)

            CASE "partial_revert":
                floor_events.append(event.quality_value)

            CASE "incident_correlated":
                floor_events.append(event.quality_value)

            CASE "hotfix_branch":
                floor_events.append(0.4)

            CASE "downstream_ci_failure":
                # Stacks per downstream service, max 3 services
                additive_events.append(-0.20)  # Per unique downstream service

            CASE "no_ci_signal":
                additive_events.append(-0.05)

    # Step 1: Find the worst floor event
    IF floor_events IS NOT EMPTY:
        worst_floor = min(floor_events)
    ELSE:
        worst_floor = 1.0

    # Step 2: Apply additive penalties
    # De-duplicate follow-up fix (only one -0.15 regardless of count)
    unique_followup_count = min(1, count(e for e in additive_events if e == -0.15))
    # De-duplicate downstream failures per service (max 3)
    unique_downstream_count = min(3, count(e for e in additive_events if e == -0.20))
    # No-CI signal (only once)
    unique_no_ci = min(1, count(e for e in additive_events if e == -0.05))

    total_additive = (unique_followup_count * -0.15) + (unique_downstream_count * -0.20) + (unique_no_ci * -0.05)

    # Step 3: Combine
    # Start from worst floor, then apply additive penalties
    quality = worst_floor + total_additive

    # Step 4: Clamp
    quality = max(0.1, min(1.0, quality))

    RETURN quality
```

### Worked Examples

**Example A: Clean merge, no events**
- Events: [ci_pass]
- Floor events: none
- Additive events: none
- Quality = 1.0

**Example B: CI fails on merge**
- Events: [ci_fail]
- Floor: min(0.7) = 0.7
- Additive: none
- Quality = 0.7

**Example C: CI fails, then someone submits a follow-up fix**
- Events: [ci_fail, followup_fix]
- Floor: min(0.7) = 0.7
- Additive: -0.15
- Quality = 0.7 - 0.15 = 0.55

**Example D: Clean merge, but PR causes a P1 incident with causal_score 0.8**
- Events: [ci_pass, incident_correlated(severity=P1, causal=0.8)]
- incident quality = 1.0 - (0.8 * (1.0 - 0.3)) = 1.0 - 0.56 = 0.44
- Floor: min(0.44) = 0.44
- Additive: none
- Quality = 0.44

**Example E: PR gets fully reverted due to quality issues**
- Events: [ci_pass, revert_quality]
- Floor: min(0.1) = 0.1
- Additive: none
- Quality = 0.1

**Example F: Incident fires AND a hotfix branch is created**
- Events: [incident_correlated(severity=P1, quality=0.3), hotfix_branch]
- Floor: min(0.3, 0.4) = 0.3
- Additive: none
- Quality = 0.3 (hotfix does not add additional penalty; worst-of applies)

**Example G: CI fails, follow-up fix, AND downstream CI failure in 2 services**
- Events: [ci_fail, followup_fix, downstream_ci_failure(service_a), downstream_ci_failure(service_b)]
- Floor: min(0.7) = 0.7
- Additive: -0.15 + (-0.20 * 2) = -0.55
- Quality = 0.7 - 0.55 = 0.15

**Example H: Partial revert of 40% of the code (quality reason)**
- Events: [partial_revert(fraction=0.4, reason=quality)]
- quality = max(0.1, 1.0 - (0.9 * 0.4)) = max(0.1, 0.64) = 0.64
- Floor: min(0.64) = 0.64
- Quality = 0.64

**Example I: Strategic full revert (product decision)**
- Events: [revert_strategic]
- Floor: min(0.8) = 0.8
- Quality = 0.8

---

## 6. State Machine Diagram

```
                           PR MERGED
                               │
                               ▼
                    ┌─────────────────────┐
                    │  State: PROVISIONAL  │
                    │  quality = 1.0       │
                    │  phase = 1           │
                    └──────────┬──────────┘
                               │
              ┌────────────────┼────────────────┐
              ▼                ▼                 ▼
         CI SIGNAL        REVERT SIGNAL    48h TIMER
              │                │                 │
         ┌────┴────┐     ┌────┴────┐            │
         │ Pass    │     │Detected │            │
         │ q=1.0   │     │         │            │
         └─────────┘     │ Full?   │            │
              │          ┌┴──┐     │            │
         ┌────┴────┐    Yes  No    │            │
         │ Fail    │     │    │    │            │
         │         │     ▼    ▼    │            │
         │ Flaky?  │  q=0.1  Partial            │
         ├─Yes─►1.0│         q=f(frac)          │
         │         │                            │
         └─No──►0.7│                            │
                   │                            │
                   └───────────┬────────────────┘
                               │
                               ▼
                    ┌─────────────────────┐
                    │  State: OBSERVING    │   T+48h to T+60d
                    │  quality = Phase1_q  │
                    │  phase = 2           │
                    └──────────┬──────────┘
                               │
          ┌──────────┬─────────┼──────────┬──────────┐
          ▼          ▼         ▼          ▼          ▼
     FOLLOWUP    REVERT    INCIDENT   HOTFIX    DOWNSTREAM
     FIX         (full/    CORR.     BRANCH    CI FAIL
     q -= 0.15   partial)                      q -= 0.20
                 q = 0.1   q = f(sev, q = 0.4  (per svc)
                 or f()    causal)
          │          │         │          │          │
          └──────────┴─────────┴──────────┴──────────┘
                               │
                          (worst-of floors
                           + sum of additives)
                               │
                               ▼ T+60d
                    ┌─────────────────────┐
                    │  State: FINAL        │
                    │  quality = locked     │
                    │  immutable            │
                    └──────────────────────┘
```

**State transitions:**
- PROVISIONAL -> OBSERVING: Automatic at T+48h
- PROVISIONAL -> FINAL: Only if a full quality revert (0.1) occurs in Phase 1 (early finalization -- no point observing further)
- OBSERVING -> FINAL: Automatic at T+60d
- FINAL -> (no transitions): Terminal state

---

## 7. Configuration Parameters

All thresholds are configurable per organization. These are the defaults:

```yaml
quality_config:
  # Observation windows
  phase1_duration_hours: 48
  phase2_duration_days: 60
  followup_fix_window_days: 14
  downstream_failure_window_days: 14

  # CI handling
  flaky_rerun_window_minutes: 30
  flaky_registry_ttl_days: 7
  flaky_min_unrelated_failures: 2
  flaky_clear_after_consecutive_passes: 10
  no_ci_penalty: 0.05

  # Degradation values
  ci_failure_quality: 0.7
  followup_fix_penalty: 0.15       # Subtractive
  full_revert_quality: 0.1          # Quality reason
  strategic_revert_quality: 0.8     # Business reason
  hotfix_branch_quality: 0.4
  downstream_failure_penalty: 0.20  # Subtractive, per service
  downstream_failure_max_services: 3

  # Partial revert
  partial_revert_min_fraction: 0.20  # Below this, not a revert
  partial_revert_max_fraction: 0.90  # Above this, full revert
  partial_revert_quality_coefficient: 0.9   # quality = 1.0 - (coeff * fraction)
  partial_revert_strategic_coefficient: 0.2  # Reduced coefficient for strategic

  # Incident correlation
  incident_causal_threshold: 0.4
  incident_severity_map:
    P0: 0.2
    P1: 0.3
    P2: 0.5
    P3: 0.7

  # Global
  quality_floor: 0.1  # Absolute minimum quality (never 0.0 for merged code)
```

---

## 8. Data Model

### quality_events table

```sql
CREATE TABLE quality_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pr_id           TEXT NOT NULL,           -- PR identifier (repo#number)
    issue_id        TEXT,                    -- Linked issue identifier
    event_type      TEXT NOT NULL,           -- See enum below
    event_timestamp TIMESTAMPTZ NOT NULL,    -- When the event occurred
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When TIER detected it
    quality_impact  DECIMAL(3,2),            -- The quality value this event produces
    metadata        JSONB NOT NULL DEFAULT '{}', -- Event-specific data

    -- Audit fields
    source_event_id TEXT,                    -- ID of the webhook/signal that triggered this

    CONSTRAINT valid_event_type CHECK (event_type IN (
        'ci_pass', 'ci_fail', 'ci_fail_flaky', 'ci_fail_partial_flaky',
        'no_ci_signal',
        'followup_fix', 'followup_improvement',
        'revert_quality', 'revert_strategic',
        'partial_revert',
        'incident_correlated',
        'hotfix_branch',
        'downstream_ci_failure'
    ))
);

CREATE INDEX idx_quality_events_pr ON quality_events(pr_id);
CREATE INDEX idx_quality_events_type ON quality_events(event_type);
CREATE INDEX idx_quality_events_timestamp ON quality_events(event_timestamp);
```

### quality_scores table (materialized)

```sql
CREATE TABLE quality_scores (
    pr_id           TEXT PRIMARY KEY,
    issue_id        TEXT,
    merged_at       TIMESTAMPTZ NOT NULL,
    phase           TEXT NOT NULL DEFAULT 'provisional',  -- provisional, observing, final
    quality         DECIMAL(3,2) NOT NULL DEFAULT 1.00,
    last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finalized_at    TIMESTAMPTZ,            -- Set when phase = 'final'
    event_count     INT NOT NULL DEFAULT 0,
    worst_event     TEXT,                    -- The event_type that had the most impact

    CONSTRAINT valid_phase CHECK (phase IN ('provisional', 'observing', 'final')),
    CONSTRAINT valid_quality CHECK (quality >= 0.10 AND quality <= 1.00)
);

CREATE INDEX idx_quality_scores_phase ON quality_scores(phase);
CREATE INDEX idx_quality_scores_issue ON quality_scores(issue_id);
```

### flaky_registry table

```sql
CREATE TABLE flaky_registry (
    test_name       TEXT NOT NULL,
    repo            TEXT NOT NULL,
    first_seen      TIMESTAMPTZ NOT NULL,
    last_seen       TIMESTAMPTZ NOT NULL,
    failure_count   INT NOT NULL DEFAULT 1,
    unrelated_pr_count INT NOT NULL DEFAULT 0,
    consecutive_passes INT NOT NULL DEFAULT 0,

    PRIMARY KEY (test_name, repo)
);

CREATE INDEX idx_flaky_registry_last_seen ON flaky_registry(last_seen);
```

---

## 9. Implementation Priority

### Phase 1 (MVP -- ship first)
1. Event 1 & 2: CI pass/fail detection (workflow_run webhooks)
2. Event 4: Full revert detection (commit message pattern matching)
3. The 48h/60d observation window and phase transitions
4. The quality resolution algorithm
5. Basic flaky registry (30-minute re-run rule only)

### Phase 2 (Core quality signals)
6. Event 3: Follow-up fix detection (file overlap + reference parsing)
7. Event 5: Partial revert detection (inverse diff analysis)
8. Event 7: Hotfix branch detection
9. Full flaky registry with 7-day TTL and cross-PR failure tracking

### Phase 3 (Advanced attribution)
10. Event 6: Incident correlation (requires PagerDuty/OpsGenie integration)
11. Event 8: Downstream CI failure detection (requires dependency graph)
12. Causal score computation
13. Revert reason classification (strategic vs. quality keywords)

---

## 10. Validation Test Cases

These test cases must pass before the quality system ships:

```
TEST 1: Clean merge
  Input: PR merged, CI passes, no events for 60 days
  Expected: quality = 1.0, phase = final

TEST 2: CI failure
  Input: PR merged, CI fails, no re-run
  Expected: quality = 0.7

TEST 3: Flaky CI recovery
  Input: PR merged, CI fails, same SHA re-run passes in 15 minutes
  Expected: quality = 1.0, event reclassified as ci_fail_flaky

TEST 4: Flaky CI timeout
  Input: PR merged, CI fails, same SHA re-run passes in 45 minutes
  Expected: quality = 0.7 (30-minute window exceeded)

TEST 5: Follow-up fix after clean merge
  Input: PR merged clean, follow-up fix at day 7 touches same files
  Expected: quality = 0.85

TEST 6: Follow-up fix after CI failure
  Input: PR merged, CI fails, follow-up fix at day 3
  Expected: quality = 0.55 (0.7 - 0.15)

TEST 7: Multiple follow-up fixes (no stacking)
  Input: PR merged clean, three different follow-up fixes at days 2, 5, 9
  Expected: quality = 0.85 (only one -0.15 penalty)

TEST 8: Follow-up fix outside window
  Input: PR merged clean, follow-up fix at day 20
  Expected: quality = 1.0 (14-day window exceeded)

TEST 9: Full quality revert
  Input: PR merged, reverted at day 10, message says "caused OOM"
  Expected: quality = 0.1

TEST 10: Full strategic revert
  Input: PR merged, reverted at day 30, message says "product decision to remove feature"
  Expected: quality = 0.8

TEST 11: Partial revert (50% quality)
  Input: PR merged, 50% of lines reverted at day 15
  Expected: quality = 0.55 (1.0 - 0.9 * 0.5)

TEST 12: Partial revert (50% strategic)
  Input: PR merged, 50% of lines reverted at day 15, strategic reason
  Expected: quality = 0.90 (1.0 - 0.2 * 0.5)

TEST 13: Partial revert below threshold
  Input: PR merged, 15% of lines reverted
  Expected: quality = 1.0 (below 20% threshold, ignored)

TEST 14: P0 incident with high causal score
  Input: PR merged, P0 incident fires, causal_score = 0.9
  Expected: quality = 1.0 - (0.9 * 0.8) = 0.28

TEST 15: P2 incident with threshold causal score
  Input: PR merged, P2 incident fires, causal_score = 0.4
  Expected: quality = 1.0 - (0.4 * 0.5) = 0.80

TEST 16: Incident with below-threshold causal score
  Input: PR merged, P1 incident fires, causal_score = 0.3
  Expected: quality = 1.0 (below CAUSAL_THRESHOLD, no penalty)

TEST 17: Hotfix branch created
  Input: PR merged, hotfix branch created referencing this PR's issue
  Expected: quality = 0.4

TEST 18: Hotfix + incident (worst-of)
  Input: PR merged, P1 incident (quality=0.3) AND hotfix branch created
  Expected: quality = 0.3 (worst-of: min(0.3, 0.4))

TEST 19: Downstream CI failure in one service
  Input: PR merged, downstream CI fails in one service
  Expected: quality = 0.80 (1.0 - 0.20)

TEST 20: Downstream CI failure in three services
  Input: PR merged, downstream CI fails in three services
  Expected: quality = 0.40 (1.0 - 0.60)

TEST 21: Combined worst case
  Input: PR merged, CI fails (0.7), follow-up fix (-0.15), downstream fails in 2 services (-0.40)
  Expected: quality = max(0.1, 0.7 - 0.15 - 0.40) = max(0.1, 0.15) = 0.15

TEST 22: No CI configured
  Input: PR merged, no CI webhook received for 48 hours
  Expected: quality = 0.95 (1.0 - 0.05 no_ci_penalty)

TEST 23: Improvement follow-up (no positive effect)
  Input: PR merged clean, follow-up adds tests to same files, message says "add tests for auth module"
  Expected: quality = 1.0 (improvement logged but no quality change)

TEST 24: Revert outside observation window
  Input: PR merged, reverted at day 65
  Expected: quality = 1.0 (finalized at day 60, revert ignored for this PR)

TEST 25: Phase transition timing
  Input: PR merged at T=0, CI passes at T+1h, follow-up fix at T+36h
  Expected at T+36h: quality = 0.85, phase = provisional
  Expected at T+48h: quality = 0.85, phase = observing
  Expected at T+60d: quality = 0.85, phase = final
```

---

## 11. Dashboard Presentation

The quality multiplier appears in the dashboard with the following visual treatment:

| Quality Range | Label | Color | Icon |
|---------------|-------|-------|------|
| 1.0 | Clean Ship | Green | Checkmark |
| 0.90 - 0.99 | Minor Deduction | Light Green | Checkmark (dimmed) |
| 0.70 - 0.89 | Issues Detected | Yellow | Warning |
| 0.40 - 0.69 | Significant Issues | Orange | Alert |
| 0.10 - 0.39 | Severe Quality Failure | Red | Error |

Each PR in the dashboard shows:
- Current quality value
- Phase (provisional / observing / final)
- Event timeline (chronological list of quality events with timestamps)
- Days remaining in observation window (if not final)

---

## 12. Relationship to Adversarial Analysis Mitigations

This specification directly addresses the following adversarial analysis findings:

| Finding | Resolution |
|---------|------------|
| G-04: Quality Score Manipulation (30-day window too short) | Extended to 60-day window with two phases |
| G-04: Follow-up-as-new-issue pattern | Event 3 detects follow-ups via file overlap even without explicit issue references |
| O-02: Quality Multiplier Override Abuse | No human override mechanism exists. Flaky CI is handled deterministically |
| P-04: Speed-Quality Tradeoff Exploitation | Graduated penalty scale (not binary cliffs). Partial reverts scale linearly |
| M-05: Retroactive Score Changes | Two-phase observation with "provisional" labeling and finalization at T+60d |
| M-06: Reverted Work | Both strategic and quality reverts handled with distinct penalties |
