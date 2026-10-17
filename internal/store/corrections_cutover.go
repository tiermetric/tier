package store

// This store-wide, identifier-free high-water mark survives erasure and reopening.
// Conservatively treat every pre-cutover API all-zero row as unavailable,
// including genuine zeros written by the pointer-aware version-7 API writer.
// The marker cannot distinguish those from older omitted stats stored as zeros.
const schemaCorrectionCutover = `
CREATE TABLE IF NOT EXISTS correction_stats_cutover (
 id INTEGER PRIMARY KEY CHECK(id=1), outcome_id INTEGER NOT NULL
);
INSERT OR IGNORE INTO correction_stats_cutover(id,outcome_id)
 SELECT 1,COALESCE(MAX(id),0) FROM outcomes;
`

// Keep numeric values wire-compatible while passing availability to every reader.
const outcomeDiffStatsUnavailableSQL = `CASE WHEN source='api-outcome' AND id<=(SELECT outcome_id FROM correction_stats_cutover WHERE id=1)
 AND additions=0 AND deletions=0 AND changed_files=0 THEN 1 ELSE 0 END`

const outcomeDiffStatsSQL = `additions, deletions, changed_files, ` + outcomeDiffStatsUnavailableSQL + `,`
