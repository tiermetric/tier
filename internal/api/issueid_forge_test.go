package api

// #466 SECURITY: the unattributed sentinel family is SERVER-ASSIGNED. A client that
// forges it moves its own dollars out of segment_reconciliation.no_outcome (the thrash
// signal) and out of the attributed side of the #234 coverage split.
//
// 🔴 THE VECTOR THAT MADE IT WORSE THAN A SHIFTED DOLLAR. store.IsUnattributed uses
// case-SENSITIVE HasPrefix; the SQL family match used LIKE, which SQLite evaluates
// case-INSENSITIVELY. A forged issue_id of "UNATTRIBUTED:main" was therefore
// *unattributed* to SQL and *no-outcome* to Go — the same dollar reported
// simultaneously as exploration by cost_composition and as abandoned real-issue work
// by segment_reconciliation, against a named developer, in ONE response.
//
// The resolution is BOTH halves, and neither alone is sufficient:
//
//   - READ side: SQL is now GLOB (case-sensitive), so the two engines agree on rows
//     already in the table. Pinned in store by TestUnattributedMatcherAgreesWithSQL.
//   - WRITE side: ingest refuses anything that merely RESEMBLES the sentinel, so a
//     forged case variant never becomes a row at all. Rejecting more at ingest than
//     you match at read is the safe direction. Pinned here.
//
// /events is the one endpoint that must ALLOW the family: it is the collector's own
// transport and the collector legitimately assigns it. It allows the four exact
// canonical spellings and nothing else.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// forgeries are the ids no CLIENT surface may write. Every one either is the sentinel
// family or is a case variant of it — the write side is deliberately wider than the
// read side.
var forgeries = []string{
	store.UnattributedIssueID,
	store.UnattributedMainBucket,
	store.UnattributedDetachedHEADBucket,
	store.UnattributedNoIssueBucket,
	"UNATTRIBUTED",
	"Unattributed",
	"UNATTRIBUTED:main",
	"UNATTRIBUTED:MAIN",
	"Unattributed:Main",
	"unattributed:anything",
	"unattributed:",
	"  unattributed:main  ", // surrounding whitespace must not smuggle it past
}

// legitimate are ordinary issue ids that must keep working. A guard that also rejects
// these is a capture outage, not a fix.
var legitimate = []string{
	"#42", "ABC-123", "unattributed-work", "unattributedly", "not-unattributed",
	"xunattributed:main", "un attributed", "",
}

// TestValidateIssueID_RejectsSentinelFamilyCaseInsensitively drives the real predicate.
func TestValidateIssueID_RejectsSentinelFamilyCaseInsensitively(t *testing.T) {
	for _, id := range forgeries {
		if err := validateIssueID(id); err == nil {
			t.Errorf("validateIssueID(%q) = nil; a client may not forge the server-assigned sentinel (#466)", id)
		}
	}
	for _, id := range legitimate {
		if err := validateIssueID(id); err != nil {
			t.Errorf("validateIssueID(%q) = %v; this is an ordinary issue id and rejecting it is a capture outage", id, err)
		}
	}
}

// TestValidateShippedIssueID_AllowsExactCanonicalFamilyOnly pins the /events variant:
// the collector's four exact spellings pass, every case variant and near-miss does not.
//
// Getting this wrong is not degraded reporting, it is TOTAL capture loss:
// handlePostEvents validates all-or-nothing so one exploratory event fails a whole
// batch, the shipper treats 4xx as terminal with no retry, and the CLI is stateless so
// the next run re-ships the identical batch and fails identically. A developer who has
// ever committed on main without an issue would lose 100% of their capture.
func TestValidateShippedIssueID_AllowsExactCanonicalFamilyOnly(t *testing.T) {
	canonical := []string{
		store.UnattributedIssueID,
		store.UnattributedMainBucket,
		store.UnattributedDetachedHEADBucket,
		store.UnattributedNoIssueBucket,
	}
	for _, id := range canonical {
		if err := validateShippedIssueID(id); err != nil {
			t.Errorf("validateShippedIssueID(%q) = %v; the JSONL collector assigns this "+
				"and a rejection destroys capture for every developer who works on main", id, err)
		}
	}
	for _, id := range []string{
		"UNATTRIBUTED", "Unattributed:Main", "UNATTRIBUTED:main",
		"unattributed:anything", "unattributed:", "  unattributed:main  ",
	} {
		if err := validateShippedIssueID(id); err == nil {
			t.Errorf("validateShippedIssueID(%q) = nil; only the four EXACT canonical "+
				"spellings are legitimate — a case variant is a forgery", id)
		}
	}
	for _, id := range legitimate {
		if err := validateShippedIssueID(id); err != nil {
			t.Errorf("validateShippedIssueID(%q) = %v; ordinary ids must ship", id, err)
		}
	}
}

// TestPostCosts_RejectsForgedSentinel exercises the real HTTP surface, not the
// predicate: POST /api/v1/costs must 400 on the whole family.
func TestPostCosts_RejectsForgedSentinel(t *testing.T) {
	h, _ := newTestHandler(t)
	costBody := func(issue string) map[string]any {
		b := validCostPayload()
		b["developer"] = "mallory"
		b["issue_id"] = issue
		return b
	}
	for _, id := range forgeries {
		code, resp := doRequest(t, h, http.MethodPost, "/api/v1/costs", costBody(id))
		if code != http.StatusBadRequest {
			t.Errorf("POST /costs issue_id=%q: status = %d, want 400 (body=%s)", id, code, resp)
		}
		if !strings.Contains(string(resp), "reserved") {
			t.Errorf("POST /costs issue_id=%q rejected for the WRONG reason: %s — a 400 "+
				"from schema validation would make this test pass while the guard is absent",
				id, resp)
		}
	}
	// Control arm: an ordinary id on the SAME payload still writes, so the 400s above
	// are the guard firing and not a broken fixture.
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/costs", costBody("#42")); code >= 300 {
		t.Fatalf("control arm rejected too: status = %d body = %s — the forge test above is vacuous", code, resp)
	}
}

// TestPostOutcomes_RejectsForgedSentinel covers the path where forging actually PAYS.
// An outcome on the sentinel earns weighted points whose cost no segment denominator
// ever sees — it inflates the NUMERATOR, not merely shifts the denominator — and makes
// one response self-contradictory: segments that can attribute $0 of a developer's
// spend while being denominated on it.
func TestPostOutcomes_RejectsForgedSentinel(t *testing.T) {
	h, _ := newTestHandler(t)
	for i, id := range forgeries {
		body := validOutcome(fmt.Sprintf("sha-forge-%d", i), map[string]any{"issue_id": id})
		code, resp := doRequest(t, h, http.MethodPost, "/api/v1/outcomes", body)
		if code != http.StatusBadRequest {
			t.Errorf("POST /outcomes issue_id=%q: status = %d, want 400 (body=%s)", id, code, resp)
		}
		if !strings.Contains(string(resp), "reserved") {
			t.Errorf("POST /outcomes issue_id=%q rejected for the WRONG reason: %s", id, resp)
		}
	}
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/outcomes",
		validOutcome("sha-forge-ok", nil)); code >= 300 {
		t.Fatalf("control arm rejected too: status = %d body = %s", code, resp)
	}
}

// TestPostEvents_AllowsCollectorFamilyRejectsForgery is the endpoint-level pair of
// TestValidateShippedIssueID_AllowsExactCanonicalFamilyOnly, over the real router.
func TestPostEvents_AllowsCollectorFamilyRejectsForgery(t *testing.T) {
	h, _ := newTestHandler(t)
	post := func(issue, key string) (int, []byte) {
		e := validEvent(key)
		e["issue_id"] = issue
		return postEvents(t, h, []map[string]any{e})
	}
	for _, id := range []string{
		store.UnattributedIssueID,
		store.UnattributedMainBucket,
		store.UnattributedDetachedHEADBucket,
		store.UnattributedNoIssueBucket,
	} {
		if code, body := post(id, "canon-key-"+id); code >= 300 {
			t.Errorf("POST /events issue_id=%q: status = %d, want 2xx — the collector "+
				"assigns this and a 4xx is terminal for the shipper (total capture loss). body=%s",
				id, code, body)
		}
	}
	for _, id := range []string{"UNATTRIBUTED:main", "Unattributed:Main", "unattributed:forged"} {
		code, body := post(id, "forge-key-"+id)
		if code != http.StatusBadRequest {
			t.Errorf("POST /events issue_id=%q: status = %d, want 400 (body=%s)", id, code, body)
		} else if !strings.Contains(string(body), "reserved") {
			t.Errorf("POST /events issue_id=%q rejected for the WRONG reason: %s", id, body)
		}
	}
}

// TestForgedSentinelIsNeverDoubleCounted is the READ-side reproduction, and the one
// that shows the two engines now agree.
//
// It writes the forged row DIRECTLY through the store — bypassing the ingest guards on
// purpose, because rows can predate a guard or arrive out of band, and "we now reject
// it at the door" is not an answer for data already in the table. The property under
// test is that ONE dollar is classified ONE way by BOTH surfaces of the same response.
//
// Before the GLOB fix this fixture reported the same $1.00 twice: as unattributed
// exploration in cost_composition (SQL's case-insensitive LIKE matched
// "UNATTRIBUTED:main") and as abandoned real-issue work in segment_reconciliation
// (Go's HasPrefix did not).
func TestForgedSentinelIsNeverDoubleCounted(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	const forged = "UNATTRIBUTED:main"

	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "mallory", IssueID: forged, Repo: repoid.Unqualified,
		Model: "claude-sonnet-4", InputTok: 2000, CostMicro: store.DollarsToMicro(1.00),
		Source: "jsonl", Fidelity: "realtime", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	resp := getScores(t, h, "/api/v1/scores")
	row := reconRow(t, resp, "mallory")
	assertPartition(t, "mallory", row)
	if resp.CostComposition == nil {
		t.Fatal("no cost_composition block; this test compares the two surfaces")
	}

	// Go's classification: the forged id is NOT the sentinel, so it is spend on a real
	// (if bogus) issue that produced no outcome.
	if got, want := row.NoOutcomeCostMicro, micro(1.00); got != want {
		t.Errorf("segment_reconciliation no_outcome = %d, want %d", got, want)
	}
	if row.UnattributedCostMicro != 0 {
		t.Errorf("segment_reconciliation unattributed = %d, want 0", row.UnattributedCostMicro)
	}

	// SQL's classification must MATCH. If cost_composition still calls this dollar
	// unattributed, the same $1.00 is reported as exploration there and as abandoned
	// real-issue work above — two different, non-additive things, in one response.
	if got := store.DollarsToMicro(resp.CostComposition.UnattributedCostUSD); got != 0 {
		t.Errorf("cost_composition unattributed_cost_usd = %v (%d micro) but "+
			"segment_reconciliation classified the same row as no-outcome. SQL and Go "+
			"disagree about %q — this is the #466 double-count, and it means SQL is "+
			"matching the sentinel family case-insensitively again (LIKE, not GLOB)",
			resp.CostComposition.UnattributedCostUSD, got, forged)
	}
	if got, want := store.DollarsToMicro(resp.CostComposition.AttributedCostUSD), micro(1.00); got != want {
		t.Errorf("cost_composition attributed_cost_usd = %d micro, want %d", got, want)
	}

	// Control arm: the REAL sentinel on the same fixture must classify as unattributed
	// on BOTH surfaces, or the assertions above are satisfied by a read path that has
	// simply stopped recognizing the family at all.
	h2, db2 := newTestHandler(t)
	if err := db2.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "mallory", IssueID: store.UnattributedMainBucket, Repo: repoid.Unqualified,
		Model: "claude-sonnet-4", InputTok: 2000, CostMicro: store.DollarsToMicro(1.00),
		Source: "jsonl", Fidelity: "realtime", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (control): %v", err)
	}
	resp2 := getScores(t, h2, "/api/v1/scores")
	row2 := reconRow(t, resp2, "mallory")
	if got, want := row2.UnattributedCostMicro, micro(1.00); got != want {
		t.Errorf("control arm: canonical sentinel classified as unattributed = %d, want %d", got, want)
	}
	if got, want := store.DollarsToMicro(resp2.CostComposition.UnattributedCostUSD), micro(1.00); got != want {
		t.Errorf("control arm: cost_composition unattributed = %d micro, want %d — the "+
			"family matcher has stopped recognizing its own sentinel", got, want)
	}
}
