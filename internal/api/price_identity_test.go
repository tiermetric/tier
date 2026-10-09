package api

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// The price_table content identity (#713) is carried on exactly THREE endpoints,
// and deliberately withheld from a fourth. This file pins both halves.
//
// ⚠️ It read "TWO ... a third" until #715 added GET /api/v1/report_manifest,
// which is the canonical case the #713 ruling itself names ("a third party
// verifying a published report reads the digests from the REPORT'S OWN
// manifest"). A count in a comment is a claim like any other; this one is
// re-stated here so the next endpoint has to update it too.
//
// PRESENT on the reproducibility surfaces: /scores (covered by
// TestGetScores_StampsPriceTableVersion), /scores/compare (below), and
// /report_manifest (TestReportManifest_StampsPriceTableIdentity, which calls
// assertPriceIdentity from this file for exactly this reason). Both build
// their block with priceTableStamp. That shared helper is good design but it is
// a CONVENTION, not an enforcement — review measured that reverting
// /scores/compare to a bare `priceTableJSON{Version, EffectiveDate}` left the
// entire internal/api suite green, while docs/api-compatibility.md promised the
// fields were "always present" there.
//
// ABSENT from /api/v1/version, which is unauthenticated (ruled 2026-08-28 —
// see priceTableJSON in handler.go). That half is guarded by an INVERTED test
// below rather than by no test at all: the ruling is only durable if re-adding
// the digests reddens something.

var tierptRE = regexp.MustCompile(`^tierpt1:[0-9a-f]{64}$`)

// assertPriceIdentity checks a decoded price_table block against the ACTIVE
// table. It asserts VALUES, not key presence: priceTableJSON has no omitempty,
// so every key is present for any value — including the empty string a dropped
// assignment produces.
func assertPriceIdentity(t *testing.T, endpoint string, got priceTableJSON) {
	t.Helper()
	active := store.ActivePriceTableInfo()
	// Guard the guard first: an unstamped active table makes everything below
	// compare "" against "".
	if active.TableHash == "" || active.FileHash == "" {
		t.Fatal("the ACTIVE price table carries no hashes — every assertion here is vacuous")
	}
	if got.TableHash != active.TableHash {
		t.Errorf("%s price_table.table_hash = %q, want the active table's %q — "+
			"docs/api-compatibility.md promises this field is always present on this endpoint",
			endpoint, got.TableHash, active.TableHash)
	}
	if got.FileHash != active.FileHash {
		t.Errorf("%s price_table.file_hash = %q, want %q", endpoint, got.FileHash, active.FileHash)
	}
	if !tierptRE.MatchString(got.TableHash) {
		t.Errorf("%s price_table.table_hash = %q, want ^tierpt1:[0-9a-f]{64}$ — the scheme tag "+
			"is what lets a consumer tell a canonicalization change from a price change",
			endpoint, got.TableHash)
	}
	// Version must still be stamped: the identity is an ADDITION to the #233
	// provenance, never a replacement for it.
	if got.Version != active.Version {
		t.Errorf("%s price_table.version = %d, want %d", endpoint, got.Version, active.Version)
	}
}

// TestCompare_StampsPriceTableIdentity pins /scores/compare.
func TestCompare_StampsPriceTableIdentity(t *testing.T) {
	h, db := newTestHandler(t)
	seedCostAt(t, db, "alice", "issue-a", 5.00, winAInstant)
	seedOutcomeAt(t, db, "alice", "issue-a", 3, 1.0, winAInstant)
	seedCostAt(t, db, "alice", "issue-b", 6.00, winBInstant)
	seedOutcomeAt(t, db, "alice", "issue-b", 4, 1.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("compare: status %d", code)
	}
	assertPriceIdentity(t, "/scores/compare", resp.PriceTable)
}

// TestVersionEndpoint_OmitsPriceTableIdentity is the INVERTED guard for the
// 2026-08-28 ruling: the #713 content digests are gated OFF the unauthenticated
// GET /api/v1/version.
//
// 🔴 Why this exists as an assertion rather than as a deleted test. The earlier
// version of this file asserted the digests were PRESENT here. When the ruling
// went the other way, simply deleting that test would have left the surface
// unguarded — and an unguarded surface is exactly how a convenience re-addition
// ("it'd be handy to check build and prices in one probe") lands later with a
// green suite. Inverting it makes the ruling itself the thing under test: the
// old mutant (revert /version to a bare stamp) is now the correct code, and the
// new mutant (re-add the digests) is what must redden.
//
// The gate is also enforced by the TYPE — buildIdentity fills a
// versionPriceTableJSON, which has no digest fields — so re-adding them takes a
// deliberate struct change. This test is the second lock, and the one that
// explains itself in the failure message.
func TestVersionEndpoint_OmitsPriceTableIdentity(t *testing.T) {
	_, body := getVersion(t, newVersionHandler(t, "0.4.0",
		withVCSStamps(stamps("ffffffffffffffffffffffffffffffffffffffff", false, "go1.26.5"))),
		"/api/v1/version")

	pt, ok := body["price_table"].(map[string]any)
	if !ok {
		t.Fatalf("price_table missing or not an object: %v", body["price_table"])
	}

	// 🔴 CONTROL FIRST. Without this, every absence assertion below would pass on
	// a response that had no price_table content at all — including a handler
	// that stopped stamping the block entirely, or a decode that silently
	// produced an empty map. Absence tests are the ones that rot into vacuity,
	// so pin what MUST still be here before asserting what must not.
	active := store.ActivePriceTableInfo()
	if active.Version == 0 {
		t.Fatal("control: the active price table version is 0 — this response carries no real " +
			"table, so the absence assertions below prove nothing")
	}
	if got, want := pt["version"], float64(active.Version); got != want {
		t.Fatalf("control: price_table.version = %v, want %v — /version must still stamp the "+
			"#233 provenance; the ruling gated the DIGESTS, not the block", got, want)
	}
	if got, want := pt["effective_date"], active.EffectiveDate; got != want {
		t.Fatalf("control: price_table.effective_date = %v, want %v", got, want)
	}
	// And the digests must genuinely exist on the active table — otherwise their
	// absence below is trivially satisfied by the feature being switched off.
	if active.TableHash == "" || active.FileHash == "" {
		t.Fatal("control: the active table carries NO digests at all, so their absence from " +
			"/version says nothing about the gate")
	}

	// THE RULING. Neither digest may appear, under any spelling.
	for _, banned := range []string{"table_hash", "file_hash", "TableHash", "FileHash"} {
		if v, present := pt[banned]; present {
			t.Errorf("GET /api/v1/version leaked price_table.%s = %v.\n"+
				"This endpoint is UNAUTHENTICATED, and an unkeyed digest over a low-entropy "+
				"price table is a confirmation oracle against an operator's private --prices "+
				"rates. Ruled 2026-08-28: the digests live on /scores, /scores/compare and "+
				"score-log — the reproducibility surfaces — and a third party verifying a "+
				"published report reads them from the report's own manifest, not from a live "+
				"probe. Use versionPriceTableJSON here.", banned, v)
		}
	}
	// `source` was already gated and stays gated: it is the server's local
	// --prices path.
	if v, present := pt["source"]; present {
		t.Errorf("GET /api/v1/version leaked price_table.source (%v) — the server's local "+
			"filesystem path, on an unauthenticated endpoint", v)
	}

	// 🔴 The digests must still be REACHABLE somewhere, or "gated" has quietly
	// become "removed". /scores is the read-scoped surface that carries them, and
	// TestGetScores_StampsPriceTableVersion asserts their values there; this arm
	// only pins that the two endpoints genuinely DIFFER, so a change that dropped
	// the digests everywhere cannot pass as compliance with the ruling.
	if len(pt) >= 4 {
		t.Errorf("price_table on /version has %d keys (%v), want exactly the 2 non-digest "+
			"fields — something re-widened this block", len(pt), jsonKeysOf(pt))
	}
}
