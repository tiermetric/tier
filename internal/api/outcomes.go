package api

// POST /api/v1/outcomes — provider-neutral outcome ingest (#188).
//
// Outcomes historically arrived only through the GitHub-specific webhook
// (internal/webhook, X-Hub-Signature-256). GitLab / Bitbucket / Gitea shops —
// a large share of self-hosted and EU enterprises — could capture cost but
// never outcomes, so they had no TIER score at all. issueref and the outcome
// schema are already provider-neutral; only the transport was GitHub-bound.
// This endpoint is the neutral transport: one bearer-gated route any CI can
// POST a merged-PR/MR outcome to, rather than N signature-verifying parsers.
//
// PROVENANCE (read before touching validation): this endpoint records
// source=OutcomeSourceAPI on every row, distinguishing a token-authenticated,
// possibly-manual outcome from a signature-verified GitHub-webhook one in audit
// (#34/#82 provenance discipline, extended to outcomes). The bearer token is
// the ONLY authenticator — there is no per-provider signature — so any holder
// can post an outcome as any developer. That is the same attribution-grade
// trust model /events documents; treat the token as an org-level secret.
//
// WEIGHT/QUALITY PARITY: weight is resolved through store.ResolveWeight, the
// same shared branching the webhook uses (a client-supplied label-equivalent
// weight wins, else the diff-size git-heuristic), so a score is IDENTICAL
// regardless of whether the outcome arrived via GitHub webhook or this
// endpoint. Quality defaults to 1.0 (the webhook's insert-time value); later
// CI/revert degradation is a GitHub-webhook-only signal today.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// maxOutcomeBody caps the /outcomes request body at 1 MiB, matching the other
// write endpoints. One outcome is tiny; the cap only bounds abuse.
const maxOutcomeBody = 1 << 20

// outcomeRequest is the POST /api/v1/outcomes body. Weight and Quality are
// pointers so an absent field is distinguishable from an explicit zero:
//   - Weight nil  → derive from the diff-size heuristic (source 'git-heuristic').
//     Weight set  → a label-equivalent weight (source 'label'); must be in
//     (0, MaxOutcomeWeight] so a caller cannot inflate points past the largest
//     real PR.
//   - Quality nil → 1.0 (the webhook's insert-time default). Quality set → a
//     multiplier in [0, 1].
//
// Additions/Deletions/ChangedFiles are the raw diff stats: they feed the
// heuristic when Weight is absent AND are retained on every row for future
// re-scoring, exactly as the webhook persists them.
type outcomeRequest struct {
	Developer      string   `json:"developer"`
	IssueID        string   `json:"issue_id"`
	PRNumber       int      `json:"pr_number"`
	Weight         *float64 `json:"weight,omitempty"`
	Quality        *float64 `json:"quality,omitempty"`
	MergeCommitSHA string   `json:"merge_commit_sha"`
	// MergedAt is the RFC3339 merge time. Required: it becomes the outcome's ts,
	// which /scores windows on — an outcome shipped after the fact must carry
	// when the PR merged, not when the batch arrived (mirrors /events).
	MergedAt     string `json:"merged_at"`
	Additions    int    `json:"additions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	ChangedFiles int    `json:"changed_files,omitempty"`
	// WorkType is the optional work category (#187). When present it must be one of
	// the fixed taxonomy (store.ValidWorkType) — an unknown value is a 400, never a
	// silently-coerced default — and the row records work_type_source='api'. When
	// absent the outcome falls back to 'feature'/'default', identical to a labelless
	// webhook PR, so a CI integrator that doesn't set it is not penalised.
	WorkType string `json:"work_type,omitempty"`
	// Repo is the OPTIONAL canonical repository ("owner/repo") this outcome was
	// earned in (#231). Omitted -> the 'unqualified' sentinel, so every existing
	// GitLab/Bitbucket integration keeps working unchanged. A multi-repo org should
	// send it: without it, two repos' issue #42 remain one entity. The reserved
	// sentinel may not be supplied explicitly.
	Repo string `json:"repo,omitempty"`
}

// outcomeResponse is the JSON body for both outcomes: "created" on a fresh
// insert (201) or "duplicate" when the merge_commit_sha was already recorded
// (200). WeightSource echoes which scale produced the stored weight ('label'
// vs 'git-heuristic') so a CI integrator can confirm parity with the webhook.
type outcomeResponse struct {
	Status       string  `json:"status"`
	WeightSource string  `json:"weight_source,omitempty"`
	Weight       float64 `json:"weight,omitempty"`
	// WorkType echoes the stored category (#187) so a CI integrator can confirm the
	// row landed under the type they intended (or the 'feature' default).
	WorkType string `json:"work_type,omitempty"`
}

// handlePostOutcome ingests one provider-neutral merged-PR/MR outcome. Wrapped
// by requireAuth in Register, sharing the #36 failed-auth lockout with the
// other bearer-gated routes.
//
// Dedup is on merge_commit_sha and is insert-first: InsertOutcome reports
// whether a row actually landed (its ON CONFLICT DO NOTHING skips a replay), so
// the store's unique partial index — not a racy read-before-insert — is the
// single source of truth for both row count AND the response label. Two racing
// identical posts land exactly one row; the loser is reported as "duplicate"
// (200), never a false "created". A replay therefore never double-inserts.
func (h *Handler) handlePostOutcome(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxOutcomeBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req outcomeRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("request body exceeds %d bytes", maxOutcomeBody))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Reject trailing JSON after the first object — a second object would
	// otherwise silently succeed (mirrors handlePostCosts).
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}

	o, err := validateOutcomeRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	inserted, err := h.store.InsertOutcome(r.Context(), o)
	if err != nil {
		// merge_commit_sha is required + length-capped but NEVER charset-validated
		// (validateOutcomeRequest), so it is client-controlled and CRLF-injectable;
		// the store error can wrap it too. Both go through the shared logsafe barrier
		// (#321, CodeQL go/log-injection).
		h.logger.Error("insert outcome", "err", logSafeErr(err), "sha", logSafeStr(o.MergeCommitSHA))
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	if !inserted {
		// The merge_commit_sha was already recorded — a replay. Echo the stored
		// row's weight/source (best-effort; a read error still yields a truthful
		// 200 duplicate, just without the echo).
		h.logger.Debug("outcome already recorded for merge commit, replay ignored",
			"sha", logSafeStr(o.MergeCommitSHA), "pr", o.PRNumber)
		resp := outcomeResponse{Status: "duplicate"}
		if existing, found, rerr := h.store.OutcomeByMergeCommit(r.Context(), o.MergeCommitSHA); rerr != nil {
			h.logger.Error("outcome duplicate echo lookup", "err", logSafeErr(rerr), "sha", logSafeStr(o.MergeCommitSHA))
		} else if found {
			resp.WeightSource = existing.WeightSource
			resp.Weight = existing.Weight
			resp.WorkType = existing.WorkType
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSON(w, http.StatusCreated, outcomeResponse{
		Status:       "created",
		WeightSource: o.WeightSource,
		Weight:       o.Weight,
		WorkType:     o.WorkType,
	})
}

// validateOutcomeRequest applies the element-level rules and converts to the
// store row. Bounds mirror handlePostCosts / handlePostEvents (required
// identifiers, 256-char caps, finite/non-negative numerics, RFC3339 timestamp
// with a bounded year); the outcome-specific rules are:
//
//   - merge_commit_sha: required and non-empty — dedup rests on it, so an
//     outcome without one has no replay protection (unlike the webhook, where a
//     NULL SHA is a tolerated legacy case).
//   - pr_number: required (>= 1) — this endpoint models a merged PR/MR.
//   - weight: optional; when set must be finite and in (0, MaxOutcomeWeight].
//   - quality: optional; when set must be finite and in [0, 1]. Absent → 1.0.
//   - additions/deletions/changed_files: optional, must be >= 0.
//
// weight_source is set by store.ResolveWeight, so it always matches what the
// webhook would record for the same inputs.
func validateOutcomeRequest(req outcomeRequest) (store.Outcome, error) {
	var zero store.Outcome
	if req.Developer == "" || req.IssueID == "" {
		return zero, fmt.Errorf("developer and issue_id are required")
	}
	if len(req.Developer) > maxIdentifierLen || len(req.IssueID) > maxIdentifierLen {
		return zero, fmt.Errorf("identifier fields must be <= %d chars", maxIdentifierLen)
	}
	if req.MergeCommitSHA == "" {
		return zero, fmt.Errorf("merge_commit_sha is required (outcome dedup depends on it)")
	}
	if len(req.MergeCommitSHA) > maxIdentifierLen {
		return zero, fmt.Errorf("merge_commit_sha must be <= %d chars", maxIdentifierLen)
	}
	if req.PRNumber < 1 {
		return zero, fmt.Errorf("pr_number is required and must be >= 1")
	}
	if req.Additions < 0 || req.Deletions < 0 || req.ChangedFiles < 0 {
		return zero, fmt.Errorf("additions, deletions, and changed_files must be >= 0")
	}

	// weight: an explicit label-equivalent weight, or 0 to fall through to the
	// heuristic in store.ResolveWeight. A caller-supplied weight is bounded so it
	// cannot exceed the largest real label — a fail-loud guard against point
	// inflation on a token-authenticated (not signature-verified) surface.
	var explicitWeight float64
	if req.Weight != nil {
		w := *req.Weight
		if math.IsNaN(w) || math.IsInf(w, 0) {
			return zero, fmt.Errorf("weight must be finite")
		}
		if w <= 0 || w > store.MaxOutcomeWeight {
			return zero, fmt.Errorf("weight must be in (0, %g] (send a label-equivalent value: 0.5, 1, 3, 5, 8)", store.MaxOutcomeWeight)
		}
		explicitWeight = w
	}
	weight, weightSource := store.ResolveWeight(explicitWeight, req.Additions, req.Deletions, req.ChangedFiles)

	// quality: a multiplier in [0, 1]; absent defaults to 1.0 (the webhook's
	// insert-time value). >1 would inflate the score, <0 is nonsensical.
	quality := 1.0
	if req.Quality != nil {
		q := *req.Quality
		if math.IsNaN(q) || math.IsInf(q, 0) {
			return zero, fmt.Errorf("quality must be finite")
		}
		if q < 0 || q > 1 {
			return zero, fmt.Errorf("quality must be in [0, 1]")
		}
		quality = q
	}

	// work_type: optional; when set it must be a canonical category (#187),
	// rejected fail-loud at the trust boundary rather than coerced. Absent → the
	// 'feature'/'default' baseline, matching a labelless webhook PR.
	workType := store.WorkTypeFeature
	workTypeSource := store.WorkTypeSourceDefault
	if req.WorkType != "" {
		if !store.ValidWorkType(req.WorkType) {
			return zero, fmt.Errorf("work_type must be one of: %s", store.WorkTypeList())
		}
		workType = req.WorkType
		workTypeSource = store.WorkTypeSourceAPI
	}

	if req.MergedAt == "" {
		return zero, fmt.Errorf("merged_at is required (RFC3339)")
	}
	ts, err := time.Parse(time.RFC3339, req.MergedAt)
	if err != nil {
		return zero, fmt.Errorf("merged_at must be RFC3339: %v", err)
	}
	// Bound the year to the same window /events and /period use: merged_at lands
	// in the ordered ts column /scores windows on, so a far-future value would
	// appear in every since-window and a far-past one would hide the row.
	if y := ts.Year(); y < minPeriodYear || y > maxPeriodYear {
		return zero, fmt.Errorf("merged_at year %d out of range [%d, %d]", y, minPeriodYear, maxPeriodYear)
	}

	repo, err := validateRepo(req.Repo)
	if err != nil {
		return zero, err
	}

	return store.Outcome{
		Developer:      req.Developer,
		IssueID:        req.IssueID,
		PRNumber:       req.PRNumber,
		Weight:         weight,
		WeightSource:   weightSource,
		Quality:        quality,
		MergeCommitSHA: req.MergeCommitSHA,
		Additions:      req.Additions,
		Deletions:      req.Deletions,
		ChangedFiles:   req.ChangedFiles,
		Source:         store.OutcomeSourceAPI,
		WorkType:       workType,
		WorkTypeSource: workTypeSource,
		Repo:           repo,
		Timestamp:      ts.UTC(),
	}, nil
}
