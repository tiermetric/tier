package store

// Outcome links are nullable and never cascade-delete correction evidence.
// Connections do not enable foreign_keys, so ON DELETE SET NULL is documentary;
// EraseDeveloper explicitly scrubs endpoint links before deleting source rows.
// ExcludePROutcome also deletes outcome rows, so endpoint ids may dangle.
// Copied identity columns keep erase/export matching after either deletion.
const schemaCorrections = `
CREATE TABLE IF NOT EXISTS correction_declarations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 developer TEXT, repo TEXT CHECK(repo IS NULL OR (repo != 'unqualified' AND instr(repo,'/')>1)), pr_number INTEGER, merge_commit_sha TEXT, merged_at TIMESTAMP, opened_at TIMESTAMP,
 additions INTEGER, deletions INTEGER, changed_files INTEGER,
 declaration_state TEXT NOT NULL CHECK(declaration_state IN ('missing','invalid','none','parents')),
 supporting_text TEXT CHECK(length(CAST(supporting_text AS BLOB))<=4096), provenance TEXT CHECK(length(CAST(provenance AS BLOB))<=128),
 recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS correction_declaration_identity ON correction_declarations(repo,pr_number,merge_commit_sha)
 WHERE repo IS NOT NULL AND pr_number IS NOT NULL AND merge_commit_sha IS NOT NULL;
CREATE INDEX IF NOT EXISTS correction_declaration_outcome ON correction_declarations(outcome_id);
CREATE INDEX IF NOT EXISTS correction_declaration_developer ON correction_declarations(developer);
CREATE INDEX IF NOT EXISTS correction_declaration_recorded ON correction_declarations(recorded_at,id);
CREATE TABLE IF NOT EXISTS corrective_relations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 child_declaration_id INTEGER REFERENCES correction_declarations(id), parent_declaration_id INTEGER REFERENCES correction_declarations(id),
 child_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL, parent_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 child_developer TEXT, parent_developer TEXT, child_repo TEXT, parent_repo TEXT, child_pr_number INTEGER, parent_pr_number INTEGER,
 child_merge_commit_sha TEXT, parent_merge_commit_sha TEXT, child_merged_at TIMESTAMP, parent_merged_at TIMESTAMP,
 state TEXT NOT NULL, resolution_reason TEXT CHECK(length(CAST(resolution_reason AS BLOB))<=4096), revision INTEGER NOT NULL,
 recorded_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL,
 supporting_text TEXT CHECK(length(CAST(supporting_text AS BLOB))<=4096),
 disposition_text TEXT CHECK(length(CAST(disposition_text AS BLOB))<=4096), appeal_text TEXT CHECK(length(CAST(appeal_text AS BLOB))<=4096),
 CHECK(child_repo != parent_repo OR child_pr_number != parent_pr_number)
);
CREATE UNIQUE INDEX IF NOT EXISTS correction_relation_edge ON corrective_relations(child_repo,child_pr_number,child_merge_commit_sha,parent_repo,parent_pr_number)
 WHERE child_repo IS NOT NULL AND child_pr_number IS NOT NULL AND child_merge_commit_sha IS NOT NULL AND parent_repo IS NOT NULL AND parent_pr_number IS NOT NULL;
CREATE INDEX IF NOT EXISTS correction_relation_parent_endpoint ON corrective_relations(parent_repo,parent_pr_number);
CREATE INDEX IF NOT EXISTS correction_relation_child_outcome ON corrective_relations(child_outcome_id);
CREATE INDEX IF NOT EXISTS correction_relation_parent_outcome ON corrective_relations(parent_outcome_id);
CREATE INDEX IF NOT EXISTS correction_relation_child_developer ON corrective_relations(child_developer);
CREATE INDEX IF NOT EXISTS correction_relation_parent_developer ON corrective_relations(parent_developer);
CREATE INDEX IF NOT EXISTS correction_relation_state ON corrective_relations(state,recorded_at,id);
CREATE TABLE IF NOT EXISTS correction_audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT, relation_id INTEGER REFERENCES corrective_relations(id), revision INTEGER NOT NULL, action TEXT NOT NULL,
 actor TEXT CHECK(length(CAST(actor AS BLOB))<=128), actor_role TEXT CHECK(length(CAST(actor_role AS BLOB))<=128), recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL,
 child_declaration_id INTEGER REFERENCES correction_declarations(id), parent_declaration_id INTEGER REFERENCES correction_declarations(id),
 child_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL, parent_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 child_developer TEXT, parent_developer TEXT, child_repo TEXT, parent_repo TEXT, child_pr_number INTEGER, parent_pr_number INTEGER,
 child_merge_commit_sha TEXT, parent_merge_commit_sha TEXT, child_merged_at TIMESTAMP, parent_merged_at TIMESTAMP,
 reason_text TEXT CHECK(length(CAST(reason_text AS BLOB))<=4096)
);
CREATE UNIQUE INDEX IF NOT EXISTS correction_audit_action ON correction_audit(relation_id,revision,action);
CREATE INDEX IF NOT EXISTS correction_audit_relation ON correction_audit(relation_id,id);
`
