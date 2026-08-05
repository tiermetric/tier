package api

// Org-hierarchy write surface (#232).
//
// Until this file, store.UpsertHierarchy / store.EndMembership had ZERO
// production callers: no HTTP route, no CLI, no config loader. Yet --aggregation
// is REQUIRED (cmd/tierd/main.go) and config.example.yaml ships the EU-safe
// `aggregation: team` default, so an org adopting that default had every
// developer resolve to team "" -> fall under the k-anonymity floor -> fold into
// scoring.OtherCohort, rendering ONE anonymous "other" row forever; and
// period_membership never opened a seat, so org_actual_spend allocation read 0.
// #226 shipped the READ side of #185 and this closes the WRITE side.
//
// TEAM-MODE CARVE-OUT (read before adding an h.aggregation guard): these
// handlers deliberately stay available in team-aggregation mode. org hierarchy
// is org STRUCTURE, not per-developer SCORE data — a team/division/org label is
// exactly the metadata team aggregation groups BY, never an individual metric it
// would expose. This mirrors the #185 carve-out for the GDPR erase/export
// endpoints (handler.go): admin write tooling is not a reporting surface, so it
// is not suppressed there. Suppressing hierarchy writes in team mode would make
// the required mode impossible to configure — the exact bug this issue fixes.
//
// CANONICALIZATION (correctness-critical, #125 + #232): every write resolves the
// developer through the developer_alias map BEFORE storing, so the org_hierarchy
// key matches the canonical key the score join uses (handleGetScores). Skipping
// this would key hierarchy rows by a raw alias the join never looks up, and team
// mode would STILL collapse to "other" — i.e. the bug would not be fixed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

const (
	// maxHierarchyBody caps the request body at 1 MiB, matching every other
	// write endpoint. A bulk row is a few identifiers, so a maximal onboarding
	// batch fits comfortably under both this and maxHierarchyPerBatch.
	maxHierarchyBody = 1 << 20
	// maxHierarchyPerBatch bounds the single import transaction. Onboarding a
	// few-thousand-developer org in one call is supported; a larger batch is a
	// bug or abuse and is rejected rather than held in one long-running write.
	maxHierarchyPerBatch = 1000
)

// hierarchyPutRequest is the PUT /org_hierarchy/{developer} body — the developer
// travels in the path, the placement in the body. division and org are optional
// (nullable columns); an omitted field stores "".
type hierarchyPutRequest struct {
	Team     string `json:"team"`
	Division string `json:"division"`
	Org      string `json:"org"`
}

// hierarchyBulkItem is one element of the POST /org_hierarchy import array. It
// is the PUT body plus the developer, since a bulk element carries its own key.
type hierarchyBulkItem struct {
	Developer string `json:"developer"`
	Team      string `json:"team"`
	Division  string `json:"division"`
	Org       string `json:"org"`
}

// hierarchyBulkResponse is the 201 body: how many rows the batch carried
// (mirrors eventsResponse.Accepted). The import is idempotent, so "accepted"
// counts rows processed, not rows newly created.
type hierarchyBulkResponse struct {
	Accepted int `json:"accepted"`
}

// hierarchyListResponse is the GET /org_hierarchy body: every stored assignment,
// developer-ordered.
type hierarchyListResponse struct {
	Hierarchy []store.HierarchyRow `json:"hierarchy"`
}

// endMembershipRequest is the POST /period_membership/{developer}/end body: the
// developer is in the path, org names WHICH membership to close, and period_end
// is the last active YYYY-MM the developer counts as a seat for.
type endMembershipRequest struct {
	Org       string `json:"org"`
	PeriodEnd string `json:"period_end"`
}

// endMembershipResponse echoes the canonicalized action so the caller sees the
// resolved (post-alias) developer their request actually affected.
type endMembershipResponse struct {
	Developer string `json:"developer"`
	Org       string `json:"org"`
	PeriodEnd string `json:"period_end"`
}

// handlePutHierarchy upserts one developer's org placement (#232). Wrapped by
// requireAuth in Register (write scope): the read-only viewer token (#190) is
// rejected 403. Validation mirrors handlePostDeveloperAlias (MaxBytesReader,
// DisallowUnknownFields, dec.More single-object rejection, required + 256-char
// capped fields). The path developer is canonicalized through the alias map
// before the write so the row keys match the score join (#125). Returns 200 with
// the stored row, echoing the canonical developer so the caller sees what was
// stored (their alias may have resolved to a different id).
func (h *Handler) handlePutHierarchy(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation carve-out (#185): see the file header. Do NOT add an
	// h.aggregation guard — org structure is not per-developer score data.
	developer := r.PathValue("developer")
	if developer == "" {
		writeError(w, http.StatusBadRequest, "developer is required")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxHierarchyBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req hierarchyPutRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}
	row, err := validateHierarchyRow(developer, req.Team, req.Division, req.Org)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	canon, err := h.resolveCanon(r.Context())
	if err != nil {
		h.logger.Error("resolve alias map for hierarchy upsert", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	row.Developer = canon(row.Developer)
	if err := h.store.UpsertHierarchy(r.Context(), row.Developer, row.Team, row.Division, row.Org); err != nil {
		h.logger.Error("upsert hierarchy", "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// handleBulkHierarchy imports a JSON array of org placements in ONE transaction
// (#232). Wrapped by requireAuth in Register (write scope). The batch is
// all-or-nothing, mirroring handlePostEvents: every element is validated and
// canonicalized first, any invalid element 400s naming its index and nothing is
// written; a valid batch rides store.UpsertHierarchies (one transaction) so a
// failure part-way rolls the whole import back rather than leaving a
// half-populated hierarchy that would silently mis-aggregate. Success is 201.
func (h *Handler) handleBulkHierarchy(w http.ResponseWriter, r *http.Request) {
	// Team-aggregation carve-out (#185): see the file header. Do NOT add a guard.
	body := http.MaxBytesReader(w, r.Body, maxHierarchyBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var reqs []hierarchyBulkItem
	if err := dec.Decode(&reqs); err != nil {
		// Distinguish a body-size rejection from malformed JSON so the operator
		// sees the real cause (both are 400s), mirroring handlePostEvents.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("request body exceeds %d bytes; split into smaller batches", maxHierarchyBody))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Reject trailing JSON after the array — a concatenated second batch would
	// otherwise be silently dropped and "succeed".
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON array")
		return
	}
	// Fail-loud: an empty import is a client bug, not a vacuous 201.
	if len(reqs) == 0 {
		writeError(w, http.StatusBadRequest, "hierarchy import must contain at least one row")
		return
	}
	if len(reqs) > maxHierarchyPerBatch {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("import exceeds %d rows; split into smaller batches", maxHierarchyPerBatch))
		return
	}
	canon, err := h.resolveCanon(r.Context())
	if err != nil {
		h.logger.Error("resolve alias map for hierarchy import", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	// Validate + canonicalize the WHOLE batch before writing anything. Reject a
	// batch that maps two rows to the SAME canonical developer: within one
	// transaction those rows would silently last-write-win, and two distinct raw
	// ids resolving to one canonical (e.g. a GitHub login and its OS username both
	// aliased to the same identity) is almost always a client mistake, not an
	// intended overwrite. Fail loud so the operator sees it, matching the
	// codebase's reject-ambiguous-input posture.
	rows := make([]store.HierarchyRow, 0, len(reqs))
	seen := make(map[string]int, len(reqs))
	for i, req := range reqs {
		row, err := validateHierarchyRow(req.Developer, req.Team, req.Division, req.Org)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("org_hierarchy[%d]: %v", i, err))
			return
		}
		row.Developer = canon(row.Developer)
		if prev, dup := seen[row.Developer]; dup {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("org_hierarchy[%d]: developer resolves to the same identity as org_hierarchy[%d]; a batch must not assign one developer twice", i, prev))
			return
		}
		seen[row.Developer] = i
		rows = append(rows, row)
	}
	if err := h.store.UpsertHierarchies(r.Context(), rows); err != nil {
		h.logger.Error("bulk upsert hierarchy", "err", logSafeErr(err), "count", len(rows))
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusCreated, hierarchyBulkResponse{Accepted: len(rows)})
}

// handleGetHierarchy lists every org placement (#232). Wrapped by requireAuth
// (write scope, like the admin alias GET): the full developer→team map is admin
// metadata, not granted to the read-only viewer scope (#190).
func (h *Handler) handleGetHierarchy(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListHierarchy(r.Context())
	if err != nil {
		h.logger.Error("list hierarchy", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusOK, hierarchyListResponse{Hierarchy: rows})
}

// handleEndMembership closes a developer's open seat in an org effective the
// given YYYY-MM (#232 / #41), so they stop diluting active members' org_total
// allocation for later periods. Wrapped by requireAuth (write scope). The
// developer is canonicalized before the write (#125). EndMembership is a no-op
// when there is no open membership, so this is idempotent and returns 200 either
// way — there is no existence oracle to 404 on, matching the store's contract. A
// period_end before the membership's start is incoherent input and returns 400.
func (h *Handler) handleEndMembership(w http.ResponseWriter, r *http.Request) {
	developer := r.PathValue("developer")
	if developer == "" {
		writeError(w, http.StatusBadRequest, "developer is required")
		return
	}
	if len(developer) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("developer must be <= %d chars", maxIdentifierLen))
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxHierarchyBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req endMembershipRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "request must contain exactly one JSON object")
		return
	}
	if req.Org == "" {
		writeError(w, http.StatusBadRequest, "org is required")
		return
	}
	if len(req.Org) > maxIdentifierLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("org must be <= %d chars", maxIdentifierLen))
		return
	}
	// Validate the period at the trust boundary (YYYY-MM, sane year range,
	// canonical form) so a malformed value is a clean 400 rather than a store
	// error; EndMembership re-validates the format as defense in depth.
	if err := validatePeriod(req.PeriodEnd); err != nil {
		writeError(w, http.StatusBadRequest, "period_end "+err.Error())
		return
	}
	canon, err := h.resolveCanon(r.Context())
	if err != nil {
		h.logger.Error("resolve alias map for end membership", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	dev := canon(developer)
	if err := h.store.EndMembership(r.Context(), dev, req.Org, req.PeriodEnd); err != nil {
		// An end period before the membership's start is incoherent client input
		// (you cannot leave an org before you joined it) — a 400, not a 500. The
		// message names the periods, never the developer (no PII in responses).
		if errors.Is(err, store.ErrEndBeforeStart) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.logger.Error("end membership", "err", logSafeErr(err))
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusOK, endMembershipResponse{Developer: dev, Org: req.Org, PeriodEnd: req.PeriodEnd})
}

// validateHierarchyRow applies the element-level rules shared by the single PUT
// and the bulk import, and returns the store row (developer NOT yet
// canonicalized — the caller resolves the alias map once and applies canon). The
// caps mirror the #144 identifier discipline used everywhere else. team is
// required and non-empty: an empty team defeats the whole feature (it is exactly
// the "" value that folds every developer into "other"), so it is a fail-loud
// 400 rather than a silently useless row. division and org are optional.
func validateHierarchyRow(developer, team, division, org string) (store.HierarchyRow, error) {
	var zero store.HierarchyRow
	if developer == "" {
		return zero, errors.New("developer is required")
	}
	if team == "" {
		return zero, errors.New("team is required")
	}
	if len(developer) > maxIdentifierLen || len(team) > maxIdentifierLen ||
		len(division) > maxIdentifierLen || len(org) > maxIdentifierLen {
		return zero, fmt.Errorf("developer, team, division, and org must each be <= %d chars", maxIdentifierLen)
	}
	// 🔴 #619 RED, and the one the first pass MISSED. This looks like org-structure
	// admin rather than a spend write, which is exactly why it was overlooked — but
	// upsertHierarchyTx does not merely file a label. It also opens a period_membership
	// SEAT backdated to '0000-01' ("active since the beginning of time"), and NOTHING
	// downstream excludes the sentinel from a per-developer aggregate: every
	// IsUnattributed call site in this package is on issue_id, never on developer.
	//
	// So one authenticated write — enrolling "unattributed" into a rival team — drags
	// the ENTIRE unattributed pool (~72% of dogfood spend) into that team's denominator
	// and craters its TIER, without touching a single cost row. It is the mirror image
	// of the self-dealing vector #619 is about: the same forged identity, aimed
	// outward. Sabotage does not need the sentinel to be YOUR denominator, only
	// somebody's.
	//
	// Enforced HERE rather than at the two handlers so PUT (single) and POST (bulk)
	// are covered by one edit and cannot drift apart, and so the bulk import stays
	// all-or-nothing: it validates every row before writing any, so a forged row
	// rejects the whole batch instead of landing a partial hierarchy.
	if err := validateDeveloper(developer); err != nil {
		return zero, err
	}
	return store.HierarchyRow{Developer: developer, Team: team, Division: division, Org: org}, nil
}

// resolveCanon fetches the developer_alias map once and returns the same
// canonicalizer handleGetScores builds (#125): a raw identifier resolves to its
// canonical, an unmapped identifier maps to itself. Hierarchy writes MUST route
// the developer through this before storing, or the org_hierarchy key will not
// match the canonical key the score join looks teams[canon] up under, and team
// mode still collapses to "other" (#232). O(aliases) fetch, O(1) per lookup.
func (h *Handler) resolveCanon(ctx context.Context) (func(string) string, error) {
	aliases, err := h.store.DeveloperAliases(ctx)
	if err != nil {
		return nil, err
	}
	return func(id string) string {
		if c, ok := aliases[id]; ok {
			return c
		}
		return id
	}, nil
}

// logSafeErr renders a store error for a structured log, stripping CR/LF and
// quoting the remainder (#321, CodeQL go/log-injection).
//
// The hierarchy endpoints accept `team`, `division`, and `org` as free-form strings
// — they are length-capped but never charset-validated, because an org's real
// division names are not ours to constrain. A store error can wrap those values, so
// logging err verbatim lets a caller embed a newline and forge a log entry
// ("...\ntime=... level=ERROR msg=auth bypassed"). An operator reading the log, or a
// SIEM parsing it line by line, sees a record nobody wrote.
//
// The barrier lives in the shared internal/logsafe package (#321): it STRIPS
// "\r"/"\n", then %q-quotes the remainder and caps the length. This wrapper is
// retained so the api call sites stay unchanged. Sibling of logsafe.Str, which the
// SHA/developer string sinks in this package now use.
//
// See logsafe's package doc for why the strip is the primary barrier and %q the
// backstop — the reasoning is subtler than it looks, and this comment previously
// stated it wrongly.
func logSafeErr(err error) string {
	return logsafe.Err(err)
}

// logSafeStr renders a client-controlled STRING for a structured log, stripping
// CR/LF and quoting the remainder (#321, CodeQL go/log-injection). It is the
// string-typed sibling of logSafeErr: the /outcomes SHA sinks, the per-developer
// path-value sinks, the unjoined-identity WARN (a stored developer id that canon()
// never charset-validates), and the over-budget WARN (a stored org name that is
// length-capped but never charset-validated) in this package route through it. Like logSafeErr
// it is a thin alias for the shared internal/logsafe barrier, kept package-local so
// every api call site sanitizes through one consistent name rather than mixing idioms.
func logSafeStr(s string) string {
	return logsafe.Str(s)
}
