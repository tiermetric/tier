package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// PRExclusionIdentity is the stored identity approved by an operator. Repo is
// kept verbatim so apply detects even a change in its stored representation.
type PRExclusionIdentity struct {
	Repo      string `json:"repo"`
	PR        int    `json:"pr"`
	OutcomeID int64  `json:"outcome_id"`
	MergeSHA  string `json:"merge_sha"`
}

// PRExclusionCandidates reads only live PR outcomes, in canonical plan order.
// Missing merge evidence stays in the plan for an explicit unknown verdict.
func (d *DB) PRExclusionCandidates(ctx context.Context, repo string, since time.Time) ([]PRExclusionIdentity, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT COALESCE(repo,''), pr_number, id, COALESCE(merge_commit_sha,'')
 FROM outcomes WHERE pr_number>0 AND COALESCE(source,'') != 'push'
 AND (?='' OR lower(repo)=?) AND ts>=?
 ORDER BY repo, pr_number, id`, repo, repo, normalizeTS(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []PRExclusionIdentity{}
	for rows.Next() {
		var v PRExclusionIdentity
		if err := rows.Scan(&v.Repo, &v.PR, &v.OutcomeID, &v.MergeSHA); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RetainedPREvidence is evidence from a retained GitHub pull_request delivery.
// When present, SHA rejects evidence for a different merge of the same PR.
type RetainedPREvidence struct {
	Repo         string
	PR           int
	MergeSHA     string
	HeadRef      string
	Title        string
	ClosedMerged bool
}

// RetainedPREvidence reads newest first, using the same bounded decompressor as
// the existing payload reader. Corrupt/incomplete bodies cannot classify a PR;
// the caller falls back to the API. This never prunes retained payloads.
func (d *DB) RetainedPREvidence(ctx context.Context) ([]RetainedPREvidence, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT body_gz FROM webhook_payloads WHERE event='pull_request' ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RetainedPREvidence
	for rows.Next() {
		var gz []byte
		if err := rows.Scan(&gz); err != nil {
			return nil, err
		}
		raw, err := gunzipBytes(gz)
		if err != nil {
			continue
		}
		var p struct {
			Action     string `json:"action"`
			Number     int    `json:"number"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			PR struct {
				Merged   bool   `json:"merged"`
				Number   int    `json:"number"`
				Title    string `json:"title"`
				MergeSHA string `json:"merge_commit_sha"`
				Head     struct {
					Ref string `json:"ref"`
				} `json:"head"`
			} `json:"pull_request"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		repo, ok := repoid.Canonical(p.Repository.FullName)
		if !ok {
			continue
		}
		n := p.PR.Number
		if n == 0 {
			n = p.Number
		}
		if n <= 0 || (p.Number != 0 && p.Number != n) {
			continue
		}
		out = append(out, RetainedPREvidence{repo, n, p.PR.MergeSHA, p.PR.Head.Ref, p.PR.Title, p.Action == "closed" && p.PR.Merged})
	}
	return out, rows.Err()
}
