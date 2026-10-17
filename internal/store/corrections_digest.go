package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// CorrectionsDigest keeps the existing token/outcome digest denominators intact.
// Counts name each evidence population, including connected outcomes outside the
// report window. A late disposition is selected by dependency, never review time.
type CorrectionsDigest struct {
	Value  string                 `json:"value"`
	Counts CorrectionDigestCounts `json:"counts"`
}
type CorrectionDigestCounts struct {
	Declarations       int64 `json:"declarations"`
	Relations          int64 `json:"relations"`
	Audit              int64 `json:"audit"`
	Assessments        int64 `json:"assessments"`
	Allocations        int64 `json:"allocations"`
	DependencyOutcomes int64 `json:"dependency_outcomes"`
}

// A finite undirected evidence graph closes over shared ancestors, late children
// and frozen chain versions. UNION (not UNION ALL) terminates even for cycles.
// Erasure severs these links in the stored rows; this reader invents none.
const correctionDigestGraph = `WITH RECURSIVE
 links(a,b) AS (
 SELECT 'd'||id,'o'||outcome_id FROM correction_declarations WHERE outcome_id IS NOT NULL
 UNION SELECT 'r'||id,'d'||child_declaration_id FROM corrective_relations WHERE child_declaration_id IS NOT NULL
 UNION SELECT 'r'||id,'d'||parent_declaration_id FROM corrective_relations WHERE parent_declaration_id IS NOT NULL
 UNION SELECT 'r'||id,'o'||child_outcome_id FROM corrective_relations WHERE child_outcome_id IS NOT NULL
 UNION SELECT 'r'||id,'o'||parent_outcome_id FROM corrective_relations WHERE parent_outcome_id IS NOT NULL
 UNION SELECT 'a'||id,'o'||outcome_id FROM correction_allocations WHERE outcome_id IS NOT NULL
 UNION SELECT 'a'||id,'o'||allocation_parent_outcome_id FROM correction_allocations WHERE allocation_parent_outcome_id IS NOT NULL
 UNION SELECT 'a'||id,'r'||relation_id FROM correction_allocations WHERE relation_id IS NOT NULL
 UNION SELECT 'a'||id,'c'||chain_version FROM correction_allocations
 UNION SELECT 's'||id,'o'||participant_outcome_id FROM correction_assessments WHERE participant_outcome_id IS NOT NULL
 UNION SELECT 's'||id,'r'||relation_id FROM correction_assessments WHERE relation_id IS NOT NULL
 UNION SELECT 's'||id,'c'||chain_version FROM correction_assessments WHERE chain_version IS NOT NULL
 UNION SELECT 'h'||id,'r'||relation_id FROM correction_audit WHERE relation_id IS NOT NULL
 UNION SELECT 'h'||id,'o'||child_outcome_id FROM correction_audit WHERE child_outcome_id IS NOT NULL
 UNION SELECT 'h'||id,'o'||parent_outcome_id FROM correction_audit WHERE parent_outcome_id IS NOT NULL
 UNION SELECT 'h'||id,'d'||child_declaration_id FROM correction_audit WHERE child_declaration_id IS NOT NULL
 UNION SELECT 'h'||id,'d'||parent_declaration_id FROM correction_audit WHERE parent_declaration_id IS NOT NULL
 ), edges(a,b) AS (SELECT a,b FROM links UNION SELECT b,a FROM links),
 seed(node) AS (
 SELECT 'o'||id FROM outcomes WHERE ts>=@since AND (@until IS NULL OR ts<@until) AND (@repo='' OR repo=@repo)
 UNION SELECT 'd'||id FROM correction_declarations WHERE merged_at>=@since AND (@until IS NULL OR merged_at<@until) AND (@repo='' OR repo=@repo)
 UNION SELECT 'r'||id FROM corrective_relations WHERE
 (child_merged_at>=@since AND (@until IS NULL OR child_merged_at<@until) AND (@repo='' OR child_repo=@repo)) OR
 (parent_merged_at>=@since AND (@until IS NULL OR parent_merged_at<@until) AND (@repo='' OR parent_repo=@repo))
 ), reach(node) AS (SELECT node FROM seed UNION SELECT b FROM edges JOIN reach ON a=node)
 `

// These explicit, ordered column lists are tierdig2's wire format. Timestamps
// are CAST to TEXT to avoid driver-version-dependent datetime normalization.
// Every value has one frame: n=NULL, i=integer, f=canonical float, s=raw text.
var correctionDigestTables = []struct{ table, kind, columns string }{
	{"correction_declarations", "d", "id,outcome_id,developer,repo,pr_number,merge_commit_sha,merged_at,opened_at,additions,deletions,changed_files,declaration_state,supporting_text,provenance,recorded_at,rule_version"},
	{"corrective_relations", "r", "id,child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,child_developer,parent_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,parent_merge_commit_sha,child_merged_at,parent_merged_at,state,resolution_reason,revision,recorded_at,updated_at,rule_version,supporting_text,disposition_text,appeal_text"},
	{"correction_audit", "h", "id,relation_id,revision,action,actor,actor_role,recorded_at,rule_version,child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,child_developer,parent_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,parent_merge_commit_sha,child_merged_at,parent_merged_at,reason_text"},
	{"correction_assessments", "s", "id,relation_id,revision,chain_version,participant_outcome_id,basis,original_weight,original_quality,reviewed_own_weight,before_weight,after_weight,marginal_credit,pure_repair,comparison_base,comparison_before,comparison_after,scope,rationale,reviewer,reviewed_at,recorded_at,rule_version"},
	{"correction_allocations", "a", "id,chain_version,relation_id,outcome_id,allocation_parent_outcome_id,original_weight,original_quality,role,raw_weight,original_term,reviewed_own_weight,before_weight,after_weight,share,projected_quality,allocated_points,marginal_credit,ranking_eligibility,status,pending_reason,provisional,disputed,frozen,recorded_at,rule_version"},
	// Raw dependency evidence outside outcomes_digest's window affects projection.
	{"outcomes", "o", "id,ts,developer,repo,issue_id,weight,quality,work_type,source,pr_number,merge_commit_sha,additions,deletions,changed_files,merged_at"},
}

func (d *DB) CorrectionsDigest(ctx context.Context, since, until time.Time, scope RepoScope) (out CorrectionsDigest, err error) {
	err = d.ReadSnapshot(ctx, func(s *Snapshot) error { out, err = s.CorrectionsDigest(ctx, since, until, scope); return err })
	return
}
func (r reader) CorrectionsDigest(ctx context.Context, since, until time.Time, scope RepoScope) (CorrectionsDigest, error) {
	return r.correctionDigestTo(ctx, since, until, scope, nil)
}
func (r reader) correctionDigestTo(ctx context.Context, since, until time.Time, scope RepoScope, inputs io.Writer) (CorrectionsDigest, error) {
	if err := checkDigestWindow(since, until); err != nil {
		return CorrectionsDigest{}, err
	}
	h := sha256.New()
	var sink io.Writer = h
	if inputs != nil {
		sink = io.MultiWriter(h, inputs)
	}
	var buf bytes.Buffer
	writeLengthPrefixed(&buf, digestDomainFor(digestScheme+"/corrections", scope))
	if _, err := sink.Write(buf.Bytes()); err != nil {
		return CorrectionsDigest{}, err
	}
	buf.Reset()
	// Close the dependency graph once in this snapshot, then reuse its row ids
	// for every typed evidence population. The concatenated values come only
	// from INTEGER ids in the graph, never from manifest or stored text fields.
	var upper any
	if !until.IsZero() {
		upper = until.UTC()
	}
	reachRows, err := r.q.QueryContext(ctx, correctionDigestGraph+`SELECT substr(node,1,1),group_concat(substr(node,2)) FROM reach GROUP BY substr(node,1,1)`, sql.Named("since", since.UTC()), sql.Named("until", upper), sql.Named("repo", scope.String()))
	if err != nil {
		return CorrectionsDigest{}, fmt.Errorf("corrective dependency closure: %w", err)
	}
	reach := map[string]string{}
	for reachRows.Next() {
		var kind, ids string
		if err := reachRows.Scan(&kind, &ids); err != nil {
			_ = reachRows.Close()
			return CorrectionsDigest{}, err
		}
		reach[kind] = ids
	}
	err = reachRows.Err()
	closeErr := reachRows.Close()
	if err != nil {
		return CorrectionsDigest{}, err
	}
	if closeErr != nil {
		return CorrectionsDigest{}, closeErr
	}
	var out CorrectionsDigest
	counts := []*int64{&out.Counts.Declarations, &out.Counts.Relations, &out.Counts.Audit, &out.Counts.Assessments, &out.Counts.Allocations, &out.Counts.DependencyOutcomes}
	for i, table := range correctionDigestTables {
		cols := strings.Split(table.columns, ",")
		selects := make([]string, len(cols))
		for j, c := range cols {
			selects[j] = c
			if strings.HasSuffix(c, "_at") || c == "ts" {
				selects[j] = "CAST(" + c + " AS TEXT)"
			}
		}
		if table.table == "outcomes" {
			selects = append(selects, outcomeDiffStatsUnavailableSQL)
		}
		ids := reach[table.kind]
		if ids == "" {
			ids = "NULL"
		}
		query := `SELECT ` + strings.Join(selects, ",") + ` FROM ` + table.table + ` WHERE id IN (` + ids + `) ORDER BY id`
		rows, err := r.q.QueryContext(ctx, query)
		if err != nil {
			return CorrectionsDigest{}, fmt.Errorf("corrections digest %s: %w", table.table, err)
		}
		writeLengthPrefixed(&buf, digestScheme+"/"+table.table)
		writeLengthPrefixed(&buf, table.columns) // pins field order and fixed arity
		if _, err = sink.Write(buf.Bytes()); err != nil {
			_ = rows.Close()
			return CorrectionsDigest{}, err
		}
		buf.Reset()
		for rows.Next() {
			values := make([]any, len(selects))
			ptrs := make([]any, len(values))
			for j := range values {
				ptrs[j] = &values[j]
			}
			if err = rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				return CorrectionsDigest{}, err
			}
			for _, v := range values {
				s, err := correctionDigestValue(v)
				if err != nil {
					_ = rows.Close()
					return CorrectionsDigest{}, err
				}
				writeLengthPrefixed(&buf, s)
			}
			if _, err = sink.Write(buf.Bytes()); err != nil {
				_ = rows.Close()
				return CorrectionsDigest{}, err
			}
			buf.Reset()
			*counts[i]++
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return CorrectionsDigest{}, err
		}
		if closeErr != nil {
			return CorrectionsDigest{}, closeErr
		}
	}
	out.Value = digestScheme + ":" + hex.EncodeToString(h.Sum(nil))
	return out, nil
}
func correctionDigestValue(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "n", nil
	case int64:
		return "i" + strconv.FormatInt(v, 10), nil
	case float64:
		return "f" + canonPriceFloat(v), nil
	case string:
		return "s" + v, nil
	case []byte:
		return "s" + string(v), nil
	default:
		return "", fmt.Errorf("unsupported corrective digest value %T", v)
	}
}
