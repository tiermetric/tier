#!/usr/bin/env python3
"""Capture a scrubbed, current-format Claude Code JSONL sample for the
tier-jsonl-ingestion contract check (captured-sample class).

It preserves the STRUCTURAL contract the tier collector depends on — line types
and ordering, field presence, the cwd-appears-on-a-later-line trait that #96
turned on, and the verbatim message.usage token accounting — while removing
everything else. This keeps the sample a REAL capture (it reflects the live
format) rather than a synthetic fixture that would stay green when the upstream
format drifts — the whole point of the captured-sample class.

Scrub design = ALLOWLIST (leak-proof by construction): only the fields the
collector actually consumes survive (see internal/collector/jsonl.go). A future
Claude Code release that adds a new conversation/content field is dropped
automatically rather than silently leaking into the committed fixture. Two
further scrubs:
  - cwd value      -> __TIER_REPO__  (the exercise runner binds it to the real
                                      checkout path; keeps the sample portable)
  - gitBranch value -> a synthetic, issue-resolvable placeholder (so a real
                       branch name / project context never lands in the repo,
                       while branch-name issue attribution still works)

A post-scrub assertion fails loudly if any surviving string value still looks
like content (multi-line or very long) — a belt-and-suspenders backstop on the
allowlist.

Usage:
    scripts/seam-capture.py <source-session.jsonl> <output-sample.jsonl>

Writes <output-dir>/last_captured = today (YYYY-MM-DD) so the captured bytes and
the freshness marker can never diverge. Refresh by re-running against a recent
real session when the format changes or the sample ages past max_sample_age.
"""
import datetime
import json
import os
import sys

# Only these keys survive — keyed to what internal/collector/jsonl.go consumes.
TOP_KEEP = {"type", "timestamp", "gitBranch", "cwd", "sessionId"}
MESSAGE_KEEP = {"id", "model", "role", "usage"}

CWD_PLACEHOLDER = "__TIER_REPO__"
# Synthetic branch: numeric segment makes issueref.FromBranch resolve it
# (-> issue-0) so the exercise scores non-zero, with no real branch leaked.
BRANCH_PLACEHOLDER = "feature/0-seam-sample"

# Heuristic for "this surviving string looks like leaked content, not a
# structural label". Structural values (UUIDs, ISO timestamps, model ids,
# enum/region codes) are short and/or space-free; conversation/file content is
# multi-line or long-and-spaced (prose). message.usage is kept verbatim, so this
# is the backstop for anything content-shaped that ever lands inside it.
MAX_SPACED_STR = 80   # a string longer than this AND containing a space = prose


def scrub_entry(o):
    """Keep only allowlisted top-level keys; recurse into message with its own
    allowlist; normalise cwd and gitBranch values."""
    out = {}
    for k in TOP_KEEP:
        if k not in o:
            continue
        if k == "cwd":
            out[k] = CWD_PLACEHOLDER
        elif k == "gitBranch":
            out[k] = BRANCH_PLACEHOLDER
        else:
            out[k] = o[k]
    m = o.get("message")
    if isinstance(m, dict):
        out["message"] = {k: m[k] for k in MESSAGE_KEEP if k in m}
    return out


def assert_no_content(o, path=""):
    """Fail loudly if any string value looks like leaked content."""
    if isinstance(o, dict):
        for k, v in o.items():
            assert_no_content(v, f"{path}/{k}")
    elif isinstance(o, list):
        for i, v in enumerate(o):
            assert_no_content(v, f"{path}[{i}]")
    elif isinstance(o, str):
        if "\n" in o or (len(o) > MAX_SPACED_STR and " " in o):
            sys.exit(f"refusing to write: suspected content leak at {path} "
                     f"(len={len(o)}, multiline={chr(10) in o})")


def has_billable_usage(o):
    m = o.get("message")
    if not isinstance(m, dict):
        return False
    u = m.get("usage")
    if not isinstance(u, dict):
        return False
    return (u.get("input_tokens") or 0) + (u.get("output_tokens") or 0) > 0


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: seam-capture.py <source-session.jsonl> <output-sample.jsonl>")
    src, dst = sys.argv[1], sys.argv[2]
    out = []
    with open(src) as f:
        for raw in f:
            raw = raw.strip()
            if not raw:
                continue
            o = json.loads(raw)
            scrubbed = scrub_entry(o)
            assert_no_content(scrubbed)
            out.append(scrubbed)
            # Keep the structural preamble through the FIRST assistant turn that
            # carries billable usage — enough for a non-zero TOTAL, no more.
            if o.get("type") == "assistant" and has_billable_usage(o):
                break

    with open(dst, "w") as f:
        for o in out:
            f.write(json.dumps(o, separators=(",", ":")) + "\n")

    marker = os.path.join(os.path.dirname(dst) or ".", "last_captured")
    today = datetime.date.today().isoformat()
    with open(marker, "w") as f:
        f.write(today + "\n")
    print(f"wrote {len(out)} scrubbed lines to {dst}; last_captured={today}")


if __name__ == "__main__":
    main()
