# TIER documentation

TIER measures the **yield** of AI token spend: outcome per token, not tokens
consumed. This index is grouped by what you are trying to do. Every page is
written to be a stopping point — you should never have to read two documents to
get one answer.

**New to TIER? Start with [understanding-tier.md](understanding-tier.md)** — it
answers "what is this and why would my team want it" in three layers, each a
complete stopping point: the short answer, how the meter works, then the full
method.

---

## Understand the number

| Document | What it answers |
|---|---|
| [understanding-tier.md](understanding-tier.md) | What TIER is and how the number is produced, in three layers. **Start here.** |
| [how-it-works.md](how-it-works.md) | What TIER actually does today, grounded in the code — the engineer's evaluation read. |
| [interpreting-the-number.md](interpreting-the-number.md) | You have a score. Is it good? What should you change? |
| [rubric.md](rubric.md) | The canonical v1 rubric — how merged work becomes weighted points. |
| [pricing-philosophy.md](pricing-philosophy.md) | What the denominator measures, and why list prices rather than your invoice. |
| [how-tier-relates.md](how-tier-relates.md) | Where TIER sits next to DORA, SPACE, and DX Core 4. |
| [peer-learning.md](peer-learning.md) | Using TIER to move practices between people — without ranking them. |

## Run it

| Document | What it answers |
|---|---|
| [quickstart.md](quickstart.md) | Copy-paste-ready first run: install, demo, score your repo, the full score, Codex capture, and viewing the dashboard from another machine. **Start here to run it.** |
| [conventions.md](conventions.md) | Size labels and issue-reference formats TIER depends on for attribution. |
| [webhook-setup.md](webhook-setup.md) | Wiring GitHub webhooks so merged work becomes outcomes. |
| [open-weights-capture.md](open-weights-capture.md) | Capturing spend for self-hosted / open-weights models. |
| [security.md](security.md) | Hardening a deployment: tokens, bind addresses, exposure. |

## Interface reference

| Document | What it answers |
|---|---|
| [api-compatibility.md](api-compatibility.md) | The `/api/v1` compatibility contract and what may change without notice. |
| [outcomes-api.md](outcomes-api.md) | Provider-neutral outcome ingest, for teams not on GitHub. |
| [reference-price-table.md](reference-price-table.md) | Narrative companion to the embedded price table. The machine-readable source of truth is `internal/store/prices.yaml`. |
| [contracts/tier-jsonl-ingestion.schema.json](contracts/tier-jsonl-ingestion.schema.json) | JSON Schema for the JSONL token-capture ingestion seam. |

## Specifications

These describe the **target** design. Each carries an implementation-status
banner stating what the code enforces today — read that banner before relying on
any section.

| Document | What it specifies |
|---|---|
| [cost-normalization-spec.md](cost-normalization-spec.md) | How raw token consumption becomes cost-normalized dollars. |
| [quality-degradation-spec.md](quality-degradation-spec.md) | The quality multiplier: build breaks, CI failures, merge problems. |

## Privacy, law, and honest limits

| Document | What it answers |
|---|---|
| [privacy.md](privacy.md) | Exactly what TIER reads, stores, and never touches — grounded in the code. |
| [legal-and-privacy.md](legal-and-privacy.md) | Deploying where per-developer measurement is regulated: works councils, GDPR, DPIA. |
| [adversarial-analysis.md](adversarial-analysis.md) | How the formula can be gamed, and where it fails. We ship this on purpose. |

## Internals

Planning and maintainer references. Not needed to run or evaluate TIER.

| Document | What it covers |
|---|---|
| [internals/tenancy-retrofit-playbook.md](internals/tenancy-retrofit-playbook.md) | What a future `tenant_id` retrofit touches. No `tenant_id` exists today — TIER is single-tenant. |

---

**A note on `_public-CLAUDE.md`:** that file is a publish artifact, not a reader
document. It becomes the repository's root `CLAUDE.md` in the public export and
carries contributor guidance for AI coding agents. It is deliberately unlinked,
and the leading underscore tells docgen not to render it.

It needs that treatment because three requirements were otherwise unsatisfiable
(#544). docgen enforces link integrity in BOTH directions — no dangling links and
no orphan pages — while publish-audit gate (b) forbids this file from shipping.
As a rendered page it therefore had to be linked, the link had to resolve inside
the export, and so the file had to ship. It could not. The published tree really
did fail its own `make check` this way; a build input simply must not be a
rendered page.
