package store

// All evidence links are nullable and have no outcome-delete cascade. Rational
// columns contain bounded canonical numerator/denominator text, never SQL REAL.
// Reviewed and sealed-policy fields are reserved here for the whole Event 3
// stack: later units must not add PII under an eraser that cannot see it.
const schemaCorrectionProjection = `
CREATE TABLE IF NOT EXISTS correction_assessments (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 relation_id INTEGER REFERENCES corrective_relations(id),
 revision INTEGER NOT NULL, chain_version INTEGER,
 participant_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 basis TEXT NOT NULL CHECK(basis IN ('provisional','reviewed')),
 original_weight TEXT CHECK(length(original_weight)<=4096), original_quality TEXT CHECK(length(original_quality)<=4096),
 reviewed_own_weight TEXT CHECK(length(reviewed_own_weight)<=4096),
 before_weight TEXT CHECK(length(before_weight)<=4096), after_weight TEXT CHECK(length(after_weight)<=4096),
 marginal_credit TEXT CHECK(length(marginal_credit)<=4096), pure_repair INTEGER NOT NULL DEFAULT 0 CHECK(pure_repair IN (0,1)),
 comparison_base TEXT CHECK(length(CAST(comparison_base AS BLOB))<=128),
 comparison_before TEXT CHECK(length(CAST(comparison_before AS BLOB))<=128),
 comparison_after TEXT CHECK(length(CAST(comparison_after AS BLOB))<=128),
 scope TEXT CHECK(length(CAST(scope AS BLOB))<=4096), rationale TEXT CHECK(length(CAST(rationale AS BLOB))<=4096),
 reviewer TEXT CHECK(length(CAST(reviewer AS BLOB))<=128), reviewed_at TIMESTAMP,
 recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS correction_assessment_review_revision ON correction_assessments(relation_id,revision,participant_outcome_id)
 WHERE basis='reviewed' AND relation_id IS NOT NULL AND participant_outcome_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS correction_assessment_projection_version ON correction_assessments(chain_version,participant_outcome_id)
 WHERE basis='provisional' AND participant_outcome_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS correction_assessment_outcome ON correction_assessments(participant_outcome_id);
CREATE INDEX IF NOT EXISTS correction_assessment_reviewer ON correction_assessments(reviewer);
CREATE INDEX IF NOT EXISTS correction_assessment_chain ON correction_assessments(chain_version);
CREATE TABLE IF NOT EXISTS correction_allocations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, chain_version INTEGER NOT NULL,
 relation_id INTEGER REFERENCES corrective_relations(id), outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 allocation_parent_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 original_weight TEXT NOT NULL CHECK(length(original_weight)<=4096),
 original_quality TEXT NOT NULL CHECK(length(original_quality)<=4096),
 role TEXT NOT NULL, raw_weight TEXT NOT NULL CHECK(length(raw_weight)<=4096),
 original_term TEXT NOT NULL CHECK(length(original_term)<=4096),
 reviewed_own_weight TEXT NOT NULL CHECK(length(reviewed_own_weight)<=4096),
 before_weight TEXT NOT NULL CHECK(length(before_weight)<=4096), after_weight TEXT NOT NULL CHECK(length(after_weight)<=4096),
 share TEXT NOT NULL CHECK(length(share)<=4096), projected_quality TEXT NOT NULL CHECK(length(projected_quality)<=4096),
 allocated_points TEXT NOT NULL CHECK(length(allocated_points)<=4096), marginal_credit TEXT NOT NULL CHECK(length(marginal_credit)<=4096),
 ranking_eligibility TEXT NOT NULL CHECK(ranking_eligibility IN ('eligible','ineligible','unknown')),
 status TEXT NOT NULL, pending_reason TEXT NOT NULL, provisional INTEGER NOT NULL CHECK(provisional IN (0,1)),
 disputed INTEGER NOT NULL DEFAULT 0 CHECK(disputed IN (0,1)),
 frozen INTEGER NOT NULL DEFAULT 0 CHECK(frozen IN (0,1)),
 recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS correction_allocation_outcome ON correction_allocations(outcome_id) WHERE outcome_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS correction_allocation_chain ON correction_allocations(chain_version);
CREATE INDEX IF NOT EXISTS correction_allocation_relation ON correction_allocations(relation_id);
CREATE TABLE IF NOT EXISTS correction_policy (
 id INTEGER PRIMARY KEY CHECK(id=1), scoring_enabled INTEGER NOT NULL DEFAULT 0 CHECK(scoring_enabled IN (0,1)),
 rubric_version INTEGER NOT NULL DEFAULT 1, rule_version TEXT NOT NULL DEFAULT 'legacy-v1',
 projection_revision INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO correction_policy(id) VALUES(1);
`
