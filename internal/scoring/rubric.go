package scoring

// RubricVersion is the version of the canonical NORMATIVE rubric — the fixed
// 0.5/1/3/5/8 outcome-weight scale (xs/s/m/l/xl) plus the work-type taxonomy —
// under which a TIER number and its weighted-point count were produced (#239).
// It is stamped into /scores alongside the price-table version (see the wire
// `rubric` block) so that any self-over-time or cross-org comparison can verify
// BOTH inputs were held constant: the dollars (price_table.version) AND the
// weight rubric (this constant). A weighted point — and therefore TIER and
// cost_per_point — is only comparable within a matched RubricVersion.
//
// Two distinct mechanisms close the generous-vs-strict labeling exploit; keep
// them separate:
//   - NORMATIVITY (the docs/rubric.md worked examples) is what makes a strict and
//     a generous org score the SAME real work the same way: both are expected to
//     calibrate against the shared canonical rubric, not their house habits. This
//     is the cross-org guard.
//   - The VERSION STAMP (this constant, surfaced on the wire) is provenance: it
//     makes a change to the rubric DEFINITION detectable over time, so a
//     self-over-time or matched-version comparison can confirm the weighting did
//     not drift under it. Two orgs on the same binary both stamp the same version
//     regardless of how generously each labels — the stamp does NOT by itself
//     detect a cross-org generosity difference (nor a local outcomes.size_labels
//     remap; see docs/rubric.md). Do not credit the integer with normativity's job.
//
// Any edit to the weight scale or the work-type taxonomy MUST bump this constant —
// otherwise the provenance is a lie. A guard test pins the canonical scale and
// taxonomy to this version so the bump cannot be forgotten silently.
//
// Deliberately absent: any absolute good/ok/poor band. TIER has no reference
// cohort baked into the code, and none is invented here — cost_per_point is a
// SELF-relative unit (compared to the same org over time, or to a peer only
// under a matched rubric + price version). A cross-org benchmark distribution is
// a forward-compatible follow-up that needs a second org's opt-in data to exist
// first (#239 item 4), not a hardcoded legend.
//
// v1: the initial canonical rubric — weight scale {0.5, 1, 3, 5, 8} mapped from
// xs/s/m/l/xl, and the seven-category work-type taxonomy (feature, bug,
// security, incident, tech-debt, research, compliance). See docs/rubric.md for
// the worked per-size and per-work_type examples and the "what you may / may not
// compare" rules, and docs/api-compatibility.md for the wire contract.
const RubricVersion = 1
