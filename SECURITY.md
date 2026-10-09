# Security Policy

Thank you for helping keep TIER and its users safe. TIER measures the yield of
AI token consumption and can hold per-developer spend and attribution data, so
we take security reports seriously.

## Reporting a vulnerability

**Please report vulnerabilities privately. Do not open a public issue, and do
not post a working exploit in any public channel.**

The preferred and supported channel is **GitHub private vulnerability
reporting**:

1. Go to the repository's **Security** tab.
2. Choose **Report a vulnerability** ("Security -> Report a vulnerability").
3. Fill in what you found, how to reproduce it, and the impact you observed.

This opens a private advisory visible only to the maintainers. It keeps the
report, the discussion, and any fix coordinated in one place until we are ready
to disclose.

If you cannot use GitHub private reporting for some reason, open a **minimal**
public issue that says only "I would like to report a security issue privately"
with no technical detail, and a maintainer will open a private channel with you.
Never include the vulnerability details, reproduction steps, or an exploit in
that placeholder issue.

## What to expect

- **Acknowledgement:** we aim to acknowledge a report within **3 business
  days**.
- **Assessment:** we will confirm the issue, ask for any clarification we need,
  and agree on a severity and a fix timeline with you.
- **Coordinated disclosure:** we practice coordinated disclosure. We will work
  with you on a fix and a disclosure date, and we are happy to credit you in the
  advisory unless you prefer to remain anonymous.
- **No bounty:** TIER is an open-source project and does not run a paid bug
  bounty program. We are grateful for responsible reports regardless.

## Supported versions

TIER is pre-1.0 and ships as a single Go binary. We do not maintain
long-lived release branches. Security fixes land on the latest released tag and
on `main`; there is no backport window for older tags.

| Version | Supported |
|---|---|
| Latest released tag | Yes |
| `main` (unreleased) | Yes |
| Any older tag | No -- please upgrade to the latest release |

If you are running an older build, the first step for any security concern is
to upgrade to the latest release and confirm the issue still reproduces.

## Scope and the operator security model

TIER's security posture is a set of deliberate, documented trade-offs that are
safe **only when the operator understands them** -- for example, TIER is
single-process and single-tenant, and its API token is org-secret-grade rather
than a per-user credential. Before reporting, it is worth checking whether the
behavior you are seeing is a documented trade-off rather than a defect.

See [docs/security.md](docs/security.md) for the full, code-grounded operator
security model, including token scope, tokenless-mode trust boundaries,
filesystem hardening, rate-limit topology, and webhook authentication. Reports
that identify a gap between that documented model and the actual behavior are
especially valuable.
