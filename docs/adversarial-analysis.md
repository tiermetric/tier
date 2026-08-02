# TIER Formula — Adversarial Analysis
## Comprehensive Failure Mode Assessment

**Analysis performed:** March 26, 2026. This date is the historical record of when the assessment was carried out, not a freshness stamp — the findings below are re-checked against the code when the formula changes.

**Formula under review:** `TIER = Σ(outcome_weight × quality_multiplier) / (total_tokens / 1,000,000)`

**Scope:** Gaming vectors, fairness failures, organizational abuse, edge-case math breakdowns, and perverse incentives

---

## Executive Summary

The TIER formula is a meaningful improvement over raw token-consumption metrics and DORA-only approaches. Its ratio structure — outcome per token — correctly frames token spending as a cost to justify rather than a trophy to accumulate. However, the formula as currently specified has **9 Critical or High severity failure modes** that would allow a sufficiently motivated actor (individual developer or manager) to meaningfully distort their score, and a further **12 Medium or High severity fairness failures** that would make the metric produce misleading comparative rankings in everyday enterprise conditions without any intentional gaming. These must be addressed before TIER is used in any performance-review-adjacent context.

The failures cluster into five categories analyzed in detail below.

---

## Part 1: Individual Gaming Scenarios

### G-01: Cherry-Picking High-Weight Issues

**Description:** The numerator reward for completing a weight-13 Epic is 13x larger than completing a weight-1 task, but a weight-13 issue does not necessarily require 13x more tokens or 13x more work. A developer with scheduling influence can strategically assign themselves Epic-labeled issues and defer or hand off lower-weight tasks to colleagues.

**Worked example:** Developer A self-assigns three weight-13 Epics. Developer B (same skill level) ships nine weight-5 issues of equivalent total complexity. Developer A's numerator: 39. Developer B's numerator: 45. Even if Developer B uses 20% fewer tokens, the weighting structure dramatically rewards issue selection over actual output volume.

**Severity:** High
**Likelihood:** Common — in any organization where developers have any self-assignment capability, this is the most natural and invisible form of gaming. It does not require deliberate malice; it simply reflects rational behavior under the incentive structure.
**Mitigation:** Weight assignments must be locked before work begins and must not be changeable by the assigned developer. Introduce a weight-to-cycle-time calibration check: if a weight-13 issue closes in under 2 days, flag it for manager review. Consider a secondary normalization: outcome per issue, not just outcome per token, to surface issue-volume signals alongside yield signals.

---

### G-02: Token Suppression via Off-Books AI Usage

**Description:** The TIER proxy captures tokens routed through the organization's sanctioned infrastructure (vLLM, API gateway). A developer who uses a personal Claude Max subscription, browser-based ChatGPT, Gemini Advanced, or a local Ollama instance generates zero recorded tokens while still producing work. Their denominator stays near-zero, and their TIER approaches infinity for any shipped outcome.

**Worked example:** Developer A uses personal Claude Pro ($20/month) to write 80% of their code, routes only clarification questions through the company proxy (5K tokens), then ships a weight-8 feature. TIER = 8.0 × 1.0 / (0.005) = 1,600. This is not measured efficiency — it is a measurement gap.

**Severity:** Critical
**Likelihood:** Common — this is already the default behavior for many developers who have personal AI subscriptions predating their employer's infrastructure. Claude Code (Max subscription mode) reads local JSONL files, not a proxy, so all Max-plan usage is invisible to the proxy-based collector unless the ccusage adapter is explicitly deployed and developers comply with it.
**Mitigation:**
1. Establish a mandatory baseline token floor per active working day (e.g., any day with a closed issue but fewer than 10K recorded tokens triggers an attribution audit flag).
2. Require ccusage integration as a prerequisite for TIER score publication, not an optional adapter.
3. Define a formal "untracked AI usage" disclosure policy and treat undisclosed off-books usage as a policy violation, not just a metric gap.
4. Consider an organizational policy requiring all AI API keys to be organization-managed, similar to how cloud spend is managed through consolidated billing accounts.

---

### G-03: Issue Splitting (Weight Fragmentation)

**Description:** A developer can take one naturally large task and split it into multiple separately-tracked issues, each accumulating its own outcome weight entry in the numerator. A single weight-13 Epic, if split into five sub-issues labeled as weight-3, weight-3, weight-3, weight-2, weight-2 (total: 13), produces the same numerator value. However, if the developer is strategic and labels the sub-issues at weight-3, weight-5, weight-3, weight-5, weight-5 (total: 21), they have inflated their numerator by 62% by relabeling one integrated piece of work.

**Severity:** High
**Likelihood:** Occasional — this requires access to issue creation and labeling, which most developers have. It is more likely in teams with immature ticket discipline.
**Mitigation:** Enforce a parent-issue/sub-issue hierarchy in the data model. Only leaf-node issues count toward TIER numerator, and parent issues cannot be simultaneously weighted and have weighted children. Additionally, track issue-splitting rate per developer as an ancillary signal — a developer who creates 5x more issues than peers on similar work deserves a flag.

---

### G-04: Quality Score Manipulation (Self-Reporting Bias)

**Description:** The quality multiplier depends on observable downstream events: CI outcomes, regression incidents within 30 days, and code discard events. The 30-day regression window creates an artificial incentive: a developer who knows their code has a latent bug can subtly delay surfacing it past the 30-day window. They ship at 1.0 and avoid the 0.3 penalty.

Additionally, the "minor issues — needs follow-up fixes" (0.8) multiplier is the most subjectively defined tier. If the follow-up fix is logged as a new, separate issue rather than as a correction to the original, the original issue retains its 1.0 multiplier. This is not fraud; it is legitimate ticket hygiene that happens to benefit the original developer's score.

**Severity:** High
**Likelihood:** Occasional for intentional delay; Common for the follow-up-as-new-issue pattern (this is standard practice at most engineering organizations and benefits TIER scores accidentally, not deliberately).
**Mitigation:**
1. Extend the quality observation window to 60 or 90 days. Thirty days does not cover monthly release cycles or slow-rolling production issues.
2. Create a formal "corrective issue" type in the schema. When Issue B is created to fix a defect introduced by Issue A, Issue A's quality multiplier retroactively updates.
3. Add a fifth multiplier tier: "discovered defect — fix pending" (0.5) as an in-flight state between clean-ship (1.0) and regression (0.3), to reduce the binary cliff between no-problem and full-regression.

---

### G-05: Prompt Engineering to Minimize Output Tokens

**Description:** Token count includes both input and output tokens. A developer who has learned to write extremely concise prompts that produce minimal output tokens (e.g., requesting code without explanations, using file references instead of pasting code) will have a structurally lower denominator than a developer who uses verbose prompting or asks for explanations they need to learn.

This is not purely gaming — it can reflect genuine skill. But it creates a measurement distortion: developers penalized for the learning behavior that produces better code over time.

**Severity:** Medium
**Likelihood:** Common — developers will naturally optimize toward low-token prompting once they understand the denominator.
**Mitigation:** Consider capping the token-efficiency benefit at a ceiling (e.g., a TIER score above the 95th percentile cohort average triggers an audit flag, not automatic praise). Distinguish between session tokens and context tokens in the data model — loading a 128K-token context window to ask a 10-token question should be weighted differently from generating 10K tokens of code output.

---

### G-06: Artificially Deferring Token Attribution

**Description:** Token attribution currently relies on issue ID being present in the request (via header, metadata, or system prompt inference). A developer can start an AI session without attaching it to any issue — routing tokens to the "unattributed" bucket during exploration — then open the issue, do the actual code generation in a fresh attributed session, and close the issue. The exploration tokens that informed the solution never appear in the denominator for that issue.

**Severity:** Medium
**Likelihood:** Occasional — this requires deliberate session management, which most developers will not do consistently. But power users who understand the attribution model will do this instinctively.
**Mitigation:** If unattributed tokens are more than X% of a developer's total tokens in a period (suggested threshold: 30%), surface a warning in the dashboard and exclude that developer from ranked comparisons until attribution improves. Do not silently accept high unattributed rates as normal.

---

## Part 2: Unfair Comparison Scenarios

### U-01: Legacy Codebase Context Penalty

**Description:** Developer A works in a 10-year-old monolith. Every AI session requires loading 80-120K tokens of legacy context, tribal knowledge comments, and technical debt documentation just to make a safe change. Developer B works on a greenfield microservice with clean abstractions. Developer B's context overhead is 5-10K tokens per session.

For the same outcome weight shipped, Developer A's denominator is 10-20x larger through no fault of their own. Developer A's TIER will be 10-20x lower despite potentially being the more skilled, more valuable developer for the organization.

**Severity:** Critical
**Likelihood:** Common — every mature engineering organization has a legacy surface. The developers assigned to maintain it are typically among the most experienced and highest-value engineers in the company.
**Mitigation:**
1. Introduce a "codebase complexity coefficient" at the repository or service level. Token consumption is normalized against this coefficient before entering the TIER denominator.
2. Track average tokens-per-issue at the team or service level and use that as a baseline for individual comparison rather than using a global baseline.
3. Do not compare TIER scores across teams working on fundamentally different codebases. Cross-team TIER comparison should require an explicit normalization acknowledgment.

---

### U-02: Mentoring and Knowledge Transfer Penalty

**Description:** Developer B is a senior engineer who spends significant time helping junior team members through AI-assisted pair sessions — walking through architecture decisions, reviewing AI-generated code together, explaining why certain patterns are dangerous. This is extremely high-value organizational work. However, in the TIER model:
- The tokens consumed during these sessions are attributed to Developer B (the senior engineer running the session)
- The outcomes may be attributed to the junior developer (whose issue ID is in the branch)
- Or neither developer gets credit if the session is exploratory and no issue is open

Result: Developer B has high token consumption, low or zero attributed outcome, and a potentially devastating TIER score for the most senior and valuable developer on the team.

**Severity:** Critical
**Likelihood:** Common — mentoring is a core function of senior engineers. Any organization that uses TIER for performance evaluation will inadvertently penalize investment in team development.
**Mitigation:**
1. Create a formal "mentoring session" attribution type in the token collector. Tokens marked as mentoring do not enter any individual's TIER denominator; they optionally roll up to a separate team "investment" metric.
2. Define a "knowledge transfer outcome weight" category for sessions that produce documented decisions, architecture diagrams, or updated runbooks — these are outcomes that should appear in the numerator.
3. At minimum, add a "context" field to token sessions that allows the session to be flagged as non-individual-productivity work.

---

### U-03: Security and Audit Work Structural Disadvantage

**Description:** Developer C works on security — threat modeling, code audits, penetration testing preparation, dependency vulnerability analysis, compliance reviews. This work:
- Generates very few "shipped" outcomes (security issues are often closed with "no action required" or produce policy documents, not code)
- Requires extensive AI-assisted research and analysis (high token consumption for exploration)
- Produces outcomes that are invisible to the current label-based weight system (no Fibonacci label for "prevented a CVE-10 breach")

Developer C will consistently score near the bottom of any TIER ranking despite performing work that is existentially important to the organization.

**Severity:** High
**Likelihood:** Common — security, platform reliability, SRE, and compliance roles are systematically penalized by any output-centric metric that lacks categories for their work type.
**Mitigation:**
1. Add outcome weight categories explicitly for security work: "Security audit — no findings" (weight 3), "Security audit — vulnerability found and mitigated" (weight 8), "Compliance certification work" (weight 5), "Threat model produced" (weight 5).
2. Allow issues to carry a "work type" tag (feature / bug-fix / security / incident / tech-debt / compliance) and apply work-type-specific TIER interpretation. Comparing a security engineer's TIER to a feature developer's TIER is a category error.
3. Consider whether security roles should be excluded from individual TIER ranking entirely and measured only at team level.

---

### U-04: Incident Response and On-Call Penalty

**Description:** Developer D handles all production incidents during a sprint. Incident response using AI tools involves:
- High token consumption for rapid diagnosis (log analysis, stack trace interpretation, runbook queries)
- Extremely time-sensitive work with no time for token efficiency optimization
- Outcomes that often manifest as a rollback (negative output — reverting to a previous state) or a hotfix (often a trivial code change, weight-1 or weight-2)
- Context-heavy sessions because understanding a production incident requires loading extensive operational context

A developer who saves the company $500K by diagnosing and fixing a P0 incident in 2 hours, consuming 500K tokens in the process, may receive a TIER score of (2 × 1.0) / (0.5) = 4.0 for that sprint — ranking last on the team — while a developer who quietly shipped two weight-5 features with minimal AI assistance scores 10.0 / 0.1 = 100.

**Severity:** Critical
**Likelihood:** Common — every team has on-call rotations. This failure mode will appear every sprint for whoever is on-call.
**Mitigation:**
1. Create an "incident response" outcome type with a separate weight scale that reflects actual impact: P0 (weight 13), P1 (weight 8), P2 (weight 5), P3 (weight 3). MTTR (mean time to resolution) and dollar impact should feed this weight, not issue label mapping.
2. Tokens consumed during an incident response session should be excluded from individual TIER and rolled up to a separate "operational overhead" organizational metric.
3. At minimum, allow on-call periods to be flagged so TIER scores for on-call weeks are excluded from performance comparison.

---

### U-05: Design and Research Work Attribution Gap

**Description:** Developer E spends a sprint doing design work — architecture exploration, proof-of-concept evaluation, technology selection research. They consume 800K tokens across 20 AI sessions, produce a 30-page architecture decision record, and unblock 5 other developers for the next quarter. No issue closes during the sprint. The shipped outcome shows up 6 weeks later when other developers build on the design.

Developer E's TIER for the sprint: 0 / 0.8 = 0.0. They appear to have produced nothing.

**Severity:** High
**Likelihood:** Common — architectural spikes, technical discovery, and R&D work are standard parts of software development. These are systematically invisible in any output-gated metric.
**Mitigation:**
1. Add "research/spike" issue types with outcome weights that trigger on completion of the research artifact (ADR closed, design doc approved), not on code merging.
2. Allow a "design credit carry-forward" mechanism: when a design decision is confirmed as the basis for subsequent implementation issues, the designer receives a deferred outcome credit.
3. Track "unresolved research tokens" separately and do not include them in individual TIER until the associated outcome materializes. This preserves accuracy without punishing exploration work in-progress.

---

### U-06: Pairing and Collaborative Work Double-Counting / Erasure

**Description:** Two developers collaborate on an issue. Developer F drives the keyboard and AI sessions; Developer G contributes architectural direction, review, and debugging guidance verbally. Under the current model:
- All tokens go to Developer F's denominator (their session, their proxy traffic)
- The outcome weight goes to whoever is assigned the issue in GitHub — say, Developer F
- Developer G contributes substantial intellectual work and receives zero TIER credit
- Developer F's denominator is inflated relative to solo work (longer sessions, more exploration)

If the issue is co-assigned (both names on it), the current schema doesn't clearly define whether the outcome weight is split, duplicated, or assigned to one developer. The TIER formula specification notes this as an edge case but does not define resolution.

**Severity:** High
**Likelihood:** Common — pair programming and collaborative debugging are industry-standard practices, particularly in AI-assisted development where one developer watches and critiques AI output while the other drives.
**Mitigation:**
1. Define explicit collaboration attribution in the schema: issues can have a "primary developer" and "collaborators" array. Outcome weight splits according to a configurable ratio (default 60/40).
2. Token attribution: collaborators can optionally attribute their session tokens to a shared issue, reducing the primary developer's apparent token overhead.
3. Treat "collab:@username" as a first-class annotation on issues, not an optional tag.

---

## Part 3: Organizational Gaming Scenarios

### O-01: Manager-Level Issue Weight Manipulation

**Description:** Managers who control issue labeling can systematically assign weight-13 labels to work done by their favored team members and weight-1 labels to equivalent work done by disfavored ones. This is particularly effective because the Fibonacci scale creates a 13:1 numerator ratio between the highest and lowest weight — the largest leverage point in the entire formula.

**Severity:** Critical
**Likelihood:** Occasional — this requires a manager who is both motivated to game the metric and has control over labeling. In practice, organizations that adopt TIER for performance evaluation create exactly this motivation.
**Mitigation:**
1. Weight assignment must be done before work begins and must require a second approver (tech lead, product manager, or peer) for any weight-8 or weight-13 assignment.
2. Track the distribution of weights assigned by each manager over time. If one manager's team consistently receives 3x higher average weights than peer teams on objectively similar issues, flag it.
3. Introduce a calibration process: periodically sample 20% of closed issues and have a cross-team panel re-estimate their weights. Use the delta to generate a "calibration factor" applied to that team's TIER scores.

---

### O-02: Quality Multiplier Override Abuse

**Description:** If managers have the ability to set or override quality multipliers — for example, to "waive" a CI failure that was caused by flaky infrastructure rather than the developer's code — this creates a mechanism for score inflation that is extremely hard to audit. A manager who systematically upgrades their team's multipliers from 0.7 to 1.0 on CI failures produces a 43% numerator inflation on affected issues.

**Severity:** High
**Likelihood:** Occasional — the "flaky CI" justification for overriding a multiplier penalty is genuinely legitimate in many cases, which makes it a perfect cover for abuse.
**Mitigation:**
1. Quality multiplier overrides must be logged with a required justification string, approver identity, and timestamp. This log must be visible to TIER administrators, not just team managers.
2. Create a separate "infrastructure failure" quality multiplier category (e.g., 0.85 — not penalized for flaky CI but not full credit either) so that the legitimate override case has a defined home that doesn't require manual intervention.
3. Track override rate per manager over time. Flag outliers.

---

### O-03: Token Attribution Shifting

**Description:** In a team environment, if a team member logs tokens under a colleague's developer ID — either deliberately or due to misconfigured session metadata — they can shift denominator tokens between individuals. A high-performing developer's denominator could be inflated by attributing other people's wasteful sessions to them, lowering their TIER. Or a low-performing developer's denominator could be deflated by routing their tokens to an unmonitored "team account."

**Severity:** Medium
**Likelihood:** Rare for deliberate cross-developer attribution manipulation. Occasional for accidental misconfiguration — particularly in shared pairing sessions where session credentials are not strictly isolated.
**Mitigation:**
1. Developer ID in token events must be sourced from authentication credentials (OAuth token, API key tied to individual identity), not from request metadata that any client can set.
2. Implement anomaly detection: flag when a developer's tokens-per-day changes by more than 3x without a corresponding change in issue activity.
3. Token events should include a cryptographic session token that is non-transferable, preventing post-hoc reassignment.

---

### O-04: Selective Sprint Inclusion

**Description:** A manager or TIER administrator can choose which sprints or date ranges to include in TIER reporting. By including a period when their team had exceptional conditions (greenfield work, no incidents, clear requirements) and excluding a period with legacy maintenance or incident load, they can present a dramatically higher team TIER to leadership.

**Severity:** Medium
**Likelihood:** Common — this is standard "data presentation" behavior in any organization that doesn't have strict controls over reporting period definition.
**Mitigation:**
1. All TIER reports must include a standard rolling period (trailing 90 days) alongside any custom date range. Cherry-picked ranges should be visually marked as custom.
2. Require audit log entries for any report exported with a non-standard date range.
3. The dashboard should show TIER trend over time by default, making it immediately visible if a presented period is cherry-picked from a favorable window.

---

## Part 4: Mathematical Edge Cases

### M-01: Division by Zero — No Token Usage

**Description:** A developer who ships work without using any AI tools has a denominator of zero. The formula is mathematically undefined. In practice, this affects:
- Developers who do not use AI tools at all
- Developers whose AI usage is entirely off-books (see G-02)
- New developers who have not yet configured the token collector
- Developers working offline or in air-gapped environments

The TIER formula specification acknowledges this case but does not specify the resolution.

**Severity:** Critical
**Likelihood:** Occasional — as AI tool adoption broadens from 60-70% toward 90%+, this edge case becomes rare. But in current enterprise conditions, it affects a meaningful minority of developers.
**Mitigation:** Define and implement a formal fallback:
1. If total_tokens = 0 and total_outcomes > 0: TIER = NULL (not 0, not infinity) with a "no AI data" badge.
2. Do not include NULL-TIER developers in any ranked list.
3. Add a "non-AI developer" mode in the schema that tracks outcome quality without a TIER score, preserving the quality multiplier benefit without creating false ranking signals.

---

### M-02: Zero Outcomes, Non-Zero Tokens

**Description:** A developer spends a sprint consuming 2M tokens on exploratory, WIP, or abandoned work. Nothing ships. Their TIER = 0 / 2.0 = 0. This correctly reflects zero shipped outcome, but it:
- Does not distinguish between "working on something big that ships next sprint" and "genuinely unproductive"
- Severely penalizes multi-sprint epics: a 3-sprint epic produces TIER = 0 for sprints 1 and 2, then a massive spike in sprint 3
- Creates incentives to create artificial checkpoints and ship partial work to avoid zero-outcome periods

**Severity:** High
**Likelihood:** Common — multi-sprint work is the norm for weight-13 Epics. The current model is structurally biased toward frequent small deliveries over long complex ones.
**Mitigation:**
1. Implement in-progress (WIP) outcome weighting: an issue in active development accrues a fraction of its final outcome weight per sprint based on progress signals (PR opened, commits made, review comments resolved). This is a "milestone credit" model.
2. Alternatively, allow token consumption to be time-shifted: tokens for a multi-sprint issue are all attributed to the sprint in which the issue closes, not the sprint in which they were consumed. This eliminates zero-TIER sprints for long-running work.
3. At minimum, flag issues as "multi-sprint in progress" so that sprint-level TIER scores for developers working on those issues are annotated rather than used raw.

---

### M-03: Agentic Loop Token Spikes

**Description:** A developer runs a long agentic loop — Claude Code with 200 auto-iterations, automated test-and-fix cycles, architecture generation followed by evaluation — and consumes 5M tokens on a single weight-8 issue. Their TIER for that issue: 8 × 1.0 / 5.0 = 1.6. Their team member ships a weight-8 issue with 50K tokens: TIER = 160.

Both developers shipped a weight-8 feature. One used a different (more agentic) workflow. The TIER gap is 100:1, but this may not reflect a 100:1 difference in actual productivity or value delivered. It reflects a difference in AI usage pattern.

As agentic loops become standard practice (and are specifically encouraged by tools like Claude Code's autonomous mode), this distortion will become systemic.

**Severity:** High
**Likelihood:** Increasing — this is the direction the industry is moving. TIER's current formula treats agentic usage as waste, which directly contradicts the value proposition of the tools most organizations are adopting.
**Mitigation:**
1. Define a "session type" field in token events: interactive (developer-driven) vs. agentic (automated loop). Apply different weighting to agentic tokens — for example, agentic tokens count at 0.1x toward the denominator, since they represent machine time, not developer time.
2. Set a per-issue token cap for denominator purposes (e.g., cap denominator contribution at 3× the team median for that issue's weight category). Tokens above the cap still count as unattributed overhead at the team level but don't crater an individual's TIER for a single agentic session.
3. Track agentic vs. interactive token split as a separate metric to give context to TIER scores.

---

### M-04: Shared Work Attribution

**Description:** Two developers collaborate on an issue. One developer closes the issue. Under the current schema, the outcome weight appears once in the data (on the closing developer's record), and tokens appear separately on each developer's record. The result:
- Closing developer: receives full outcome credit, denominator inflated by their half of the tokens
- Other developer: receives zero outcome credit, denominator inflated by their half of the tokens
- Team-level rollup: the outcome counts once (correct) but the tokens count twice (also correct — they were both consumed)

This is mathematically consistent but individually unfair. The team TIER is accurate; individual TIERs are not.

**Severity:** High
**Likelihood:** Common — pair work, code review that involves significant AI-assisted analysis, and collaborative debugging are standard practices.
**Mitigation:** See U-06 mitigation. The schema change (collaboration attribution) resolves both the fairness failure and the mathematical representation issue simultaneously.

---

### M-05: Retroactive Score Changes and Instability

**Description:** The quality multiplier changes retroactively within a 30-day window. A developer ships an issue on Day 1 with a multiplier of 1.0. On Day 22, it causes a production incident and the multiplier drops to 0.3. Any TIER report generated between Day 1 and Day 22 showed a different score than the final score. Reports that were printed, shared with leadership, or used for performance decisions during that window contain stale data.

This retroactive instability creates several problems:
- Performance reviews based on mid-period TIER exports may not reflect final values
- Developers are incentivized to dispute incident attribution before the 30-day window closes
- Historical TIER scores are not stable data — the same period can produce different scores depending on when you query it

**Severity:** High
**Likelihood:** Common — any regression within the quality window triggers retroactive score change.
**Mitigation:**
1. Define two TIER score types explicitly: "live TIER" (subject to retroactive quality updates) and "finalized TIER" (locked at the end of the observation window, typically Day 31+). Performance reviews must use finalized TIER only.
2. Add a score-stability indicator to the dashboard: issues within their quality observation window are marked as "provisional."
3. Generate immutable score snapshots at defined reporting periods (monthly, quarterly) that are locked and cannot be retroactively altered. This provides a stable record for performance purposes.

---

### M-06: Reverted Work

**Description:** A developer ships a weight-8 feature. It passes CI. No regression within 30 days — it's a clean ship at 1.0. Six weeks later, a strategic decision is made to revert the entire feature (product pivot, acquisition, regulatory change). The developer's TIER for that work remains at its original value. The 8 × 1.0 credit stands even though the code is no longer in the product.

The formula has no mechanism to handle post-30-day reversals. The developer received full credit for work that was ultimately removed.

**Severity:** Medium
**Likelihood:** Occasional — full reversals are uncommon but not rare. Partial reverts (where some of a developer's work within an issue is removed) are common.
**Mitigation:**
1. Add a "reverted" outcome status that, when set, reduces quality multiplier to 0.0 and creates an offset entry in the TIER record. The developer's historical score is adjusted forward from the revert date, not retroactively.
2. For partial reverts, add a "revert fraction" field that allows fractional quality multiplier reduction (e.g., 40% of the code was reverted → quality multiplier becomes 0.6 of original).
3. Distinguish between revert-due-to-quality (the developer's fault) and revert-due-to-strategy (not the developer's fault). Only quality-related reverts should affect TIER.

---

## Part 5: Perverse Incentives

### P-01: TIER Discourages AI-Assisted Learning

**Description:** A developer who uses AI to understand a new domain, learn a new framework, or explore how an unfamiliar codebase works consumes tokens without producing immediate outcomes. This is high-value investment in capability. Under TIER, it either:
- Increases their denominator with no numerator benefit (if unattributed or attached to an exploratory issue)
- Is hidden off-books (if the developer learns they are penalized for it)

The perverse incentive: developers will stop using AI as a learning tool and use it only for direct code generation against open issues. This degrades code quality over time as developers lose the ability to deeply understand what the AI is generating.

**Severity:** High
**Likelihood:** Common — this is the natural behavioral response to any metric that penalizes activity without immediate output attribution.
**Mitigation:** Create a formal "learning and exploration" token category that is excluded from individual TIER calculation and reported separately as a team investment metric. Model it after how companies track training budget — as a benefit, not a cost.

---

### P-02: Avoidance of Hard Problems

**Description:** TIER creates a rational incentive to avoid issues that are inherently token-expensive: debugging intermittent failures, untangling complex race conditions, reverse-engineering undocumented legacy systems. These problems require extensive AI-assisted exploration — loading large context windows, iterating through hypotheses, generating and discarding test cases — and often produce small outcome weights (the fix may be a 1-line change, weight 2 or 3, despite requiring 500K tokens of investigation).

A developer who consistently takes hard problems will have a structurally lower TIER than one who avoids them. Organizations will lose the cultural willingness to tackle difficult technical debt.

**Severity:** High
**Likelihood:** Common — this is a well-documented response to any output-focused metric. DORA specifically avoids this problem by being team-level rather than individual. TIER reintroduces it at the individual level.
**Mitigation:**
1. Add a "difficulty multiplier" to the outcome weight for issues that are flagged as inherently exploration-heavy (debugging, root cause analysis, reverse engineering). A weight-3 bug fix that required 400K tokens of diagnosis could carry a difficulty multiplier of 2.0, making its effective numerator contribution 6.0.
2. Track token-to-outcome efficiency at the issue-type level rather than just developer level. A developer who consistently achieves better token efficiency than the team median on hard problems is extremely valuable, even if their absolute TIER looks lower.

---

### P-03: Token-Free Approaches Incentivized Even When AI Is Superior

**Description:** Because tokens appear in the denominator, a developer who chooses to solve a problem manually — writing code without AI assistance — has a lower denominator for the same outcome. Their TIER will be higher. TIER therefore creates a systematic incentive to avoid using AI in cases where AI would produce better results, because manual work has zero denominator cost.

This is the opposite of the intended purpose. TIER is designed to encourage efficient AI use, not to discourage AI use entirely.

**Severity:** High
**Likelihood:** Common — any developer who understands the formula will recognize that their highest possible TIER is achieved by doing everything manually and only using AI when absolutely necessary. This incentive runs directly counter to the goal of measuring AI yield.
**Mitigation:** This is a fundamental formula flaw with no perfect fix. Partial mitigations:
1. Establish a minimum token threshold per developer per period. Developers who use zero or near-zero tokens are excluded from TIER ranking rather than ranked at the top.
2. Report TIER alongside absolute outcome weight. A developer with TIER = 200 who shipped 2 units of outcome in a month is less valuable than a developer with TIER = 40 who shipped 40 units. Pure efficiency divorced from volume is misleading.
3. Consider a composite metric: TIER × log(total_outcome_weight), which rewards both efficiency and absolute volume, preventing a low-volume manual developer from ranking above a high-volume AI user.

---

### P-04: Speed-Quality Tradeoff Exploitation

**Description:** The quality multiplier penalizes regressions, but the severity gradient is steep and binary:

| Scenario | Multiplier |
|----------|-----------|
| Clean | 1.0 |
| Minor issues | 0.8 |
| CI failure | 0.7 |
| Regression | 0.3 |
| Throwaway | 0.1 |

The jump from CI failure (0.7) to regression (0.3) is a 57% reduction. This asymmetric penalty structure creates risk aversion that goes beyond healthy quality standards. Developers will avoid shipping anything that has even a small chance of causing a regression — not because they want better quality, but because the TIER penalty for a regression is catastrophic relative to the reward for a clean ship.

The consequence: innovation slowdown. Developers will over-engineer solutions to be safe rather than moving fast and iterating. This is the opposite of the "AI-augmented velocity" value proposition.

**Severity:** Medium
**Likelihood:** Common — loss aversion is a well-documented cognitive bias. The asymmetric multiplier structure will produce risk aversion proportional to the penalty severity.
**Mitigation:**
1. Smooth the quality multiplier curve: replace the cliff between 0.7 and 0.3 with a gradient that includes intermediate states (0.6 "defect found but non-production", 0.5 "minor production impact", 0.3 "major production regression").
2. Add a "severity" dimension to regressions: a P3 incident that affects 1% of users should not receive the same 0.3 multiplier as a P0 that brings down the entire service.
3. Allow quality multiplier recovery: if a developer identifies, fixes, and documents a regression they caused within a defined SLA (e.g., 4 hours), their multiplier recovers from 0.3 to 0.5. This preserves accountability while rewarding responsiveness.

---

### P-05: Experimentation Penalized, Convergent Thinking Rewarded

**Description:** TIER rewards developers who make efficient, direct use of AI to implement well-defined solutions. It penalizes developers who explore multiple approaches before converging on the best one — because exploration tokens count against the denominator even if the exploration was what produced the superior solution.

A developer who runs three competing implementations, evaluates them, and ships the best one consumed 3x the tokens of a developer who implemented the first solution that seemed to work. TIER rewards the fast-and-converging developer, not necessarily the thoughtful-and-thorough one.

Over time, this incentive erodes the engineering culture of careful design and empirical evaluation in favor of cargo-cult AI usage (accept the first thing the model generates, ship it fast).

**Severity:** Medium
**Likelihood:** Common — this will manifest as a gradual cultural shift rather than obvious individual gaming, making it harder to detect.
**Mitigation:**
1. Track "implementation approach count" per issue as an optional annotation. Issues where multiple approaches were evaluated receive an "exploration bonus" that partially offsets the additional denominator cost.
2. Recognize that this is a fundamental tension in the formula. TIER is a trailing indicator of efficiency; it cannot simultaneously reward exploratory process. The mitigation is governance: use TIER as one signal among several, not as a sole productivity measure.

---

### P-06: Goodhart's Law Convergence

**Description:** Once developers know they are measured by TIER, TIER ceases to measure what it was designed to measure. This is Goodhart's Law ("When a measure becomes a target, it ceases to be a good measure") applied to developer productivity. The combination of gaming vectors described above (G-01 through G-06) means that a developer actively optimizing for TIER score will make systematically different choices than a developer simply trying to do good work.

The specific behaviors that maximize TIER score without maximizing value:
- Self-assign high-weight issues (G-01)
- Use personal AI accounts for exploration, company account only for final generation (G-02)
- Break work into many separately-tracked issues (G-03)
- Delay surfacing bugs past the quality window (G-04)
- Write terse prompts to minimize output tokens (G-05)
- Never pair with colleagues (eliminates U-06 penalty)
- Avoid on-call, mentoring, security, and architectural work (U-02, U-03, U-04)

A developer who executes all of these behaviors simultaneously will have an artificially high TIER score and will provide significantly less organizational value than their score implies.

**Severity:** Critical
**Likelihood:** Common, once TIER is used in performance evaluation contexts.
**Mitigation:** This is not fixable by formula changes alone. Required governance controls:
1. TIER must not be the sole input to any performance, compensation, or promotion decision.
2. Quarterly "TIER integrity reviews" where managers audit a random sample of high-TIER contributors to verify that their scores reflect genuine impact.
3. Pair TIER scores with qualitative peer assessment and manager evaluation.
4. Publish the formula publicly (already planned) and explicitly acknowledge Goodhart's Law in the documentation. Organizations that adopt TIER should be warned that the metric will be gamed if treated as a performance target rather than a diagnostic tool.

---

## Summary Risk Matrix

| ID | Failure Mode | Severity | Likelihood | Formula Fix Available? |
|----|-------------|----------|------------|----------------------|
| G-01 | Cherry-picking high-weight issues | High | Common | Partial |
| G-02 | Off-books AI usage (zero denominator) | Critical | Common | No (requires policy) |
| G-03 | Issue splitting / weight fragmentation | High | Occasional | Yes |
| G-04 | Quality score window exploitation | High | Occasional/Common | Yes (extend window) |
| G-05 | Token-minimization via prompt engineering | Medium | Common | No (by design) |
| G-06 | Deferred token attribution | Medium | Occasional | Partial |
| U-01 | Legacy codebase context penalty | Critical | Common | Yes (coefficient) |
| U-02 | Mentoring and knowledge transfer penalty | Critical | Common | Yes (session type) |
| U-03 | Security and audit work invisibility | High | Common | Yes (new categories) |
| U-04 | On-call and incident response penalty | Critical | Common | Yes (outcome type) |
| U-05 | Design and research work attribution gap | High | Common | Yes (spike type) |
| U-06 | Pairing and collaborative work | High | Common | Yes (schema change) |
| O-01 | Manager weight manipulation | Critical | Occasional | Partial |
| O-02 | Quality multiplier override abuse | High | Occasional | Yes (audit log) |
| O-03 | Token attribution shifting | Medium | Rare/Occasional | Yes (auth-bound IDs) |
| O-04 | Selective sprint inclusion | Medium | Common | Yes (standard periods) |
| M-01 | Division by zero (no tokens) | Critical | Occasional | Yes (NULL handling) |
| M-02 | Zero outcomes (WIP/multi-sprint) | High | Common | Yes (WIP credit) |
| M-03 | Agentic loop token spikes | High | Increasing | Yes (session type) |
| M-04 | Shared work attribution | High | Common | Yes (schema change) |
| M-05 | Retroactive score instability | High | Common | Yes (finalized scores) |
| M-06 | Reverted work | Medium | Occasional | Yes (revert status) |
| P-01 | Learning and exploration penalized | High | Common | Yes (learning category) |
| P-02 | Avoidance of hard problems | High | Common | Partial (difficulty multiplier) |
| P-03 | Manual work incentivized over AI | High | Common | Partial (volume floor) |
| P-04 | Speed-quality cliff in multiplier | Medium | Common | Yes (smooth gradient) |
| P-05 | Experimentation penalized | Medium | Common | Partial |
| P-06 | Goodhart's Law convergence | Critical | Common | No (requires governance) |

**Counts by severity:**
- Critical: 7 (G-02, U-01, U-02, U-04, O-01, M-01, P-06)
- High: 14
- Medium: 7

---

## Priority Recommendations

The following changes should be implemented before TIER is used in any performance-evaluation-adjacent context. They are ordered by severity and feasibility.

### Immediate (Pre-Release, Formula Changes)

1. **Define zero-token handling explicitly.** TIER = NULL when total_tokens = 0. Never divide by zero. Never rank NULL-TIER developers alongside measured ones. (Fixes M-01)

2. **Add mandatory outcome type taxonomy.** Extend the current feature-only model with: bug-fix, security-audit, incident-response, research-spike, tech-debt, compliance. Each type gets its own weight interpretation and quality window. (Fixes U-03, U-04, U-05 partially)

3. **Create finalized vs. live score distinction.** Issue TIER scores become final at quality-window-close + 1 day. All performance-relevant exports must use finalized scores only. (Fixes M-05)

4. **Implement developer authentication binding for tokens.** Token events must be sourced from authenticated identity, not from request metadata. (Fixes O-03, partially addresses G-02)

### Short-Term (Before Organizational Rollout)

5. **Add session type field to token events.** Interactive vs. agentic vs. learning vs. mentoring. Agentic and learning tokens are excluded from individual TIER denominator. (Fixes M-03, P-01, U-02 partially)

6. **Implement collaboration attribution in schema.** Issues carry primary_developer + collaborators array. Outcome weight and tokens are distributed according to attribution split. (Fixes U-06, M-04)

7. **Extend quality observation window to 60 days minimum, 90 recommended.** The 30-day window is too short for regressions that surface in monthly release cycles. (Fixes G-04 partially)

8. **Add a WIP/milestone credit model for multi-sprint issues.** Prevents zero-TIER sprints on long-running work. (Fixes M-02)

### Governance Requirements (Non-Formula)

9. **TIER must never be the sole input to a performance decision.** This must be stated explicitly in the documentation, the dashboard, and in the enterprise onboarding process. Organizations that treat TIER as a performance target will experience Goodhart's Law convergence within one review cycle. (Addresses P-06)

10. **Implement weight assignment approval workflow.** Weight-8 and weight-13 assignments require a second approver and are locked before work begins. Post-hoc re-weighting requires manager + skip-level approval. (Addresses G-01, O-01)

11. **Publish the full list of gaming vectors in the TIER documentation.** Transparency about how the metric can be gamed is the best defense against gaming. Organizations that understand the failure modes can build the governance processes to mitigate them. Security through obscurity does not work for metrics.

---

## Comparison with DORA

DORA metrics survive adversarial scrutiny better than TIER for five structural reasons worth acknowledging:

1. **Team-level, not individual.** DORA cannot be gamed by one developer because it measures team output. TIER's individual-level measurement creates all of the gaming vectors described above.
2. **Source data from tooling.** DORA reads from CI/CD pipelines and incident management systems, which are harder to manipulate than issue labels. TIER's numerator is entirely label-dependent.
3. **Empirically validated at scale.** DORA has 39,000+ data points. TIER has a 6-developer synthetic test dataset. Claims of validity should be appropriately scoped until field data is collected.
4. **Leading indicator.** DORA's deployment frequency is a leading indicator of delivery health. TIER is a trailing indicator — it measures what was produced, not what the delivery pipeline is capable of producing.
5. **No single-number ranking.** DORA is four separate metrics with no composite score. A single TIER number is more legible but more gameable and more susceptible to misuse in performance contexts.

This does not mean TIER is inferior — it measures something DORA does not measure at all (AI cost justification). But TIER should be positioned as a complement to DORA, not a replacement, and the maturity gap should be acknowledged honestly in the documentation and marketing.

---

*Analysis complete. 28 failure modes documented across 5 categories. 7 rated Critical. All findings are based on the TIER formula as defined in the project README and core documentation.*
