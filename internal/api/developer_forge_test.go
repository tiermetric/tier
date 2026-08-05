package api

// #619 SECURITY: the unattributed sentinel names TWO columns — issue_id and developer.
// #466 closed forgery on the first. This file closes the second, which is the WORSE
// half.
//
// 🔴 WHY WORSE. TIER is points / (cost/1000). Forging `issue_id` moves a dollar
// BETWEEN BUCKETS INSIDE the forger's own denominator — out of
// segment_reconciliation.no_outcome, onto the unattributed side of the #234 coverage
// split — and leaves the headline score untouched. Forging `developer` moves the
// dollar OUT of that denominator entirely: it lands on the "unattributed"
// pseudo-developer, the forger's cost falls, and the forger's score RISES. Forging
// issue_id is an accounting smudge; forging developer pays.
//
// The rule is the SAME rule, deliberately: one predicate
// (validateReservedIdentifier) backs both validateIssueID and validateDeveloper, so
// the two columns can never drift about what "resembles the sentinel" means.
//
// ⚠️ THE ASYMMETRY THAT IS NOT AN OVERSIGHT. `issue_id` needed an /events allowlist
// (validateShippedIssueID) because the JSONL collector legitimately ships the sentinel
// family on every exploratory session. `developer` needs NO allowlist anywhere,
// because the producers that legitimately ASSIGN the developer sentinel — the
// anthropic-admin and openai-usage org pollers — never cross an HTTP boundary: cmd/tierd
// wires them into ingester.Store(db) in-process. TestOrgPollerPathStillWritesTheSentinel
// below is the control arm that keeps that claim honest, and
// TestEventsDeveloperHasNoAllowlist pins that /events is strict on this column.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// devForgeries are the developer values no client surface may write. Reuses the same
// corpus as the issue_id half (see `forgeries` in issueid_forge_test.go) because it is
// the same predicate; retyping it would let the two lists drift.
var devForgeries = forgeries

// legitimateDevelopers are ordinary developer identities that must keep working. A
// guard that also rejects these is a capture outage, not a fix. "unknown" is the real
// no-identity fallback collector.OSUsername() emits — rejecting it would break every
// container with no /etc/passwd entry.
var legitimateDevelopers = []string{
	"alice", "bob-gh", "unknown", "unattributed-bot", "unattributedly",
	"not-unattributed", "xunattributed:main", "un attributed", "DOMAIN\\alice",
}

// TestValidateDeveloper_RejectsSentinelFamilyCaseInsensitively drives the real
// predicate directly.
func TestValidateDeveloper_RejectsSentinelFamilyCaseInsensitively(t *testing.T) {
	for _, d := range devForgeries {
		if err := validateDeveloper(d); err == nil {
			t.Errorf("validateDeveloper(%q) = nil; a client may not forge the "+
				"server-assigned sentinel into its own developer field (#619)", d)
		} else if !strings.Contains(err.Error(), "developer") {
			t.Errorf("validateDeveloper(%q) error names the wrong field: %v — a client "+
				"told to fix its issue_id will never find this", d, err)
		}
	}
	for _, d := range legitimateDevelopers {
		if err := validateDeveloper(d); err != nil {
			t.Errorf("validateDeveloper(%q) = %v; this is an ordinary identity and "+
				"rejecting it is a capture outage", d, err)
		}
	}
}

// TestValidatorsDelegateToTheSharedPredicate pins that BOTH api validators are the
// shared store.ResemblesUnattributed and not lookalikes that happen to agree today.
//
// ⚠️ AN EARLIER VERSION OF THIS TEST COULD NOT FAIL, and the reason is worth keeping.
// It compared validateIssueID against validateDeveloper — but both were one-line
// wrappers around the SAME local function, so it asserted `f(x) == f(x)`. It was
// green by construction while the copy that could ACTUALLY drift — internal/proxy's
// hand-restated four-liner — went unpinned. Comparing two wrappers around one
// implementation proves nothing; compare each wrapper against the shared
// implementation instead, and pin the cross-package pair in the package that holds
// the other copy (see proxy.TestProxyGuardDelegatesToTheSharedPredicate).
func TestValidatorsDelegateToTheSharedPredicate(t *testing.T) {
	corpus := append([]string{}, forgeries...)
	corpus = append(corpus, legitimate...)
	corpus = append(corpus, legitimateDevelopers...)
	corpus = append(corpus, unicodeNearMisses...)

	for _, s := range corpus {
		want := store.ResemblesUnattributed(s)
		if got := validateIssueID(s) != nil; got != want {
			t.Errorf("validateIssueID(%q) rejects=%v but store.ResemblesUnattributed=%v — "+
				"the validator has stopped delegating and is now a second, drifting rule", s, got, want)
		}
		if got := validateDeveloper(s) != nil; got != want {
			t.Errorf("validateDeveloper(%q) rejects=%v but store.ResemblesUnattributed=%v — "+
				"the validator has stopped delegating and is now a second, drifting rule", s, got, want)
		}
	}
}

// TestValidatorMessagesNameTheirOwnField pins that each wrapper's message names the
// column the client actually got wrong. The wrappers own their full literal text
// precisely so a shared (field, subject) formatter cannot transpose them — a
// transposition would still contain the right field name and no assertion on
// "contains developer" could catch it, which is why this asserts on the SUBJECT
// clause too.
func TestValidatorMessagesNameTheirOwnField(t *testing.T) {
	devErr := validateDeveloper(store.UnattributedIssueID)
	issueErr := validateIssueID(store.UnattributedIssueID)
	if devErr == nil || issueErr == nil {
		t.Fatal("both validators must reject the bare sentinel")
	}
	if !strings.HasPrefix(devErr.Error(), "developer ") ||
		!strings.Contains(devErr.Error(), "cannot be tied to a person") {
		t.Errorf("developer error = %q; it must name the developer field AND describe "+
			"the developer condition, or a client is told to fix the wrong thing", devErr)
	}
	if !strings.HasPrefix(issueErr.Error(), "issue_id ") ||
		!strings.Contains(issueErr.Error(), "an issue cannot be resolved") {
		t.Errorf("issue_id error = %q; it must name the issue_id field AND describe the "+
			"issue condition", issueErr)
	}
}

// unicodeNearMisses are non-ASCII strings a reader might mistake for the sentinel.
// They exist to answer one question precisely: is the write-side guard's byte-slicing
// (`t[:len(prefix)]` fed to the rune-based strings.EqualFold) a hole?
//
// MEASURED ANSWER: no. Every one of these is REJECTED BY NEITHER SIDE, and that is
// consistent, not a gap — see TestWriteGuardStrictlyContainsTheReadMatcher.
var unicodeNearMisses = []string{
	"unattrıbuted",            // Turkish dotless ı
	"unattrıbuted:main",       //
	"UNATTRİBUTED",            // Turkish dotted capital İ
	"UNATTRİBUTED:main",       //
	"ＵＮＡＴＴＲＩＢＵＴＥＤ",            // fullwidth homoglyphs
	"ＵＮＡＴＴＲＩＢＵＴＥＤ:main",       //
	"unattributed́",           // combining acute appended
	"unattributed:main",       // ASCII 'd' spelled as an escape — byte-identical, IS the sentinel
	"unattributeḍ:main",       // d-with-dot-below (multi-byte straddling the prefix boundary)
	"unattributedé:main",      // multi-byte straddling the prefix boundary
	"ünattributed:main",       // multi-byte at position 0
	"unattributed\u200d:main", // zero-width joiner spliced in
	"unattributed",            // ASCII 'b' as an escape — byte-identical, IS the sentinel
	// The two the review specifically asked to pin, because they are the cases where
	// a reader's intuition about "Go folds Unicode" is WRONG in the safe direction:
	"unattributedK:main", // U+212A KELVIN SIGN — folds to ASCII 'k', but there is no
	// 'k' in "unattributed:", so it can never help a forger. Not rejected.
	"unattributed：main", // U+FF1A FULLWIDTH COLON — does NOT fold to ':'. The prefix
	// arm therefore does not match, and neither does any read path. Not rejected.
}

// legitimateMultiByteDevelopers are real human names on a human-name column. The
// developer field holds people's logins, so non-ASCII here is ORDINARY, not exotic —
// and a guard that grew a normalisation step would start merging distinct people into
// one row while closing no hole. Kept separate from legitimateDevelopers so the
// multi-byte coverage cannot be deleted by someone tidying the ASCII list.
var legitimateMultiByteDevelopers = []string{
	"José", "Łukasz", "Ægir", "田中太郎", "Мария", "أحمد", "Zoë-O'Brien",
	"müller", "İbrahim", "Ναυσικά",
}

// TestMultiByteDeveloperNamesAreAccepted pins that ordinary non-ASCII identities pass
// every developer surface. On a column holding human logins this is not a corner case,
// and the failure it guards against is silent: a rejected name is a developer whose
// entire spend stops being captured.
func TestMultiByteDeveloperNamesAreAccepted(t *testing.T) {
	h, _ := newTestHandler(t)
	for i, d := range legitimateMultiByteDevelopers {
		if err := validateDeveloper(d); err != nil {
			t.Errorf("validateDeveloper(%q) = %v; this is an ordinary human name", d, err)
		}
		body := validCostPayload()
		body["developer"] = d
		body["idempotency_key"] = fmt.Sprintf("mb-%d", i)
		if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/costs", body); code >= 300 {
			t.Errorf("POST /costs developer=%q: status = %d, want 2xx — rejecting a real "+
				"name silently drops that person's entire spend (body=%s)", d, code, resp)
		}
	}
}

// TestWriteGuardStrictlyContainsTheReadMatcher IS THE UNICODE ANSWER, as a property
// rather than a claim.
//
// 🔴 THE INVARIANT THAT MAKES THE GUARD SOUND: the write side must reject a STRICT
// SUPERSET of what the read side classifies as the sentinel. Rejecting more at ingest
// than you match at read is the safe direction — the reverse is a hole, because a
// value the read path counts as unattributed could then be written by a client.
//
// Why this settles the Unicode question for BOTH engines. The read side is bytewise on
// both: Go's store.IsUnattributed is `==` / `strings.HasPrefix`, and the SQL side is
// `= ? OR GLOB ?`, which SQLite evaluates case-SENSITIVELY and WITHOUT any Unicode
// normalisation (that is the #466 LIKE->GLOB fix). store.TestUnattributedMatcherAgreesWithSQL
// already pins SQL == Go, so containment against Go is containment against SQL.
//
// The consequence for homoglyphs, NFKC and zero-width joiners is therefore that they
// are NOT a sentinel-forgery vector at all: a homoglyph string is a DIFFERENT byte
// string, so no read path ever treats it as the sentinel. It is stored as an ordinary
// developer whose name happens to contain odd characters — which is exactly what it is.
// (Homoglyph impersonation of ANOTHER developer, e.g. Cyrillic "аlice" for "alice", is
// a real but SEPARATE concern: it applies to every identity field, has nothing to do
// with the sentinel, and is not in #619's scope.)
//
// The byte-slice itself is safe for a structural reason worth stating: matching the
// 13-byte ASCII prefix "unattributed:" under EqualFold requires 13 runes in 13 bytes,
// which forces all 13 to be ASCII. A multi-byte rune can therefore never fold INTO the
// prefix, only out of it.
func TestWriteGuardStrictlyContainsTheReadMatcher(t *testing.T) {
	corpus := append([]string{}, forgeries...)
	corpus = append(corpus, legitimate...)
	corpus = append(corpus, legitimateDevelopers...)
	corpus = append(corpus, unicodeNearMisses...)

	for _, s := range corpus {
		readSaysSentinel := store.IsUnattributed(s)
		writeRejects := validateDeveloper(s) != nil
		if readSaysSentinel && !writeRejects {
			t.Errorf("HOLE on %q: the read path classifies this as the unattributed "+
				"sentinel but the write guard ACCEPTS it — a client can write a value "+
				"that every aggregate then counts as unattributed spend. The write side "+
				"must reject a strict superset of what the read side matches.", s)
		}
	}
}

// TestUnicodeNearMissesAreOrdinaryIdentities is the other half of the answer, and the
// half that would catch an over-eager "fix". A non-ASCII lookalike is not the sentinel
// on either side, so it must be stored as an ordinary developer. A guard that started
// normalising or folding these would silently merge distinct real people into one row.
func TestUnicodeNearMissesAreOrdinaryIdentities(t *testing.T) {
	for _, s := range unicodeNearMisses {
		readSaysSentinel := store.IsUnattributed(s)
		writeRejects := validateDeveloper(s) != nil
		// Two of the corpus entries are byte-identical to the sentinel (spelled with
		// \u escapes for ASCII letters) — they are the control that this test is not
		// vacuously true of everything in the list.
		if readSaysSentinel {
			if !writeRejects {
				t.Errorf("%q IS the sentinel bytewise but the guard accepts it", s)
			}
			continue
		}
		if writeRejects {
			t.Errorf("%q is NOT the sentinel on either read path, yet the write guard "+
				"rejects it. That is over-reach: it refuses a real identity while "+
				"closing no hole. Do not normalise or fold beyond simple case.", s)
		}
	}
}

// TestPostCosts_RejectsForgedDeveloper exercises the real HTTP surface: the manual
// import path must 400 on the whole family.
func TestPostCosts_RejectsForgedDeveloper(t *testing.T) {
	h, _ := newTestHandler(t)
	body := func(dev string) map[string]any {
		b := validCostPayload()
		b["developer"] = dev
		return b
	}
	for _, d := range devForgeries {
		code, resp := doRequest(t, h, http.MethodPost, "/api/v1/costs", body(d))
		if code != http.StatusBadRequest {
			t.Errorf("POST /costs developer=%q: status = %d, want 400 (body=%s)", d, code, resp)
		}
		if !strings.Contains(string(resp), "reserved") {
			t.Errorf("POST /costs developer=%q rejected for the WRONG reason: %s — a 400 "+
				"from schema validation would make this test pass while the guard is absent",
				d, resp)
		}
	}
	// Control arm: an ordinary identity on the SAME payload still writes, so the 400s
	// above are the guard firing and not a broken fixture.
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/costs", body("mallory")); code >= 300 {
		t.Fatalf("control arm rejected too: status = %d body = %s — the forge test above is vacuous", code, resp)
	}
}

// TestPostOutcomes_RejectsForgedDeveloper is the NUMERATOR mirror: an outcome filed
// against the sentinel developer credits weighted points to a pool of spend that by
// construction has no owner.
func TestPostOutcomes_RejectsForgedDeveloper(t *testing.T) {
	h, _ := newTestHandler(t)
	for i, d := range devForgeries {
		body := validOutcome(fmt.Sprintf("sha-devforge-%d", i), map[string]any{"developer": d})
		code, resp := doRequest(t, h, http.MethodPost, "/api/v1/outcomes", body)
		if code != http.StatusBadRequest {
			t.Errorf("POST /outcomes developer=%q: status = %d, want 400 (body=%s)", d, code, resp)
		}
		if !strings.Contains(string(resp), "reserved") {
			t.Errorf("POST /outcomes developer=%q rejected for the WRONG reason: %s", d, resp)
		}
	}
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/outcomes",
		validOutcome("sha-devforge-ok", nil)); code >= 300 {
		t.Fatalf("control arm rejected too: status = %d body = %s", code, resp)
	}
}

// TestEventsDeveloperHasNoAllowlist pins the asymmetry between the two columns on the
// ONE endpoint where they are treated differently.
//
// On /events, `issue_id` is allowlisted: the four exact canonical spellings pass,
// because the JSONL collector assigns them and a 4xx here is terminal for the shipper
// (total, permanent capture loss). `developer` is STRICT: nothing that ships over this
// wire assigns the sentinel to it, so an allowlist would only make forging as
// effective as honesty.
//
// The two halves are asserted in ONE test on purpose. Split apart, a future edit that
// "made the endpoint consistent" by allowlisting developer too would leave a green
// suite; here the same fixture asserts both directions of the asymmetry at once.
func TestEventsDeveloperHasNoAllowlist(t *testing.T) {
	h, _ := newTestHandler(t)
	post := func(dev, issue, key string) (int, []byte) {
		e := validEvent(key)
		e["developer"] = dev
		e["issue_id"] = issue
		return postEvents(t, h, []map[string]any{e})
	}

	// The canonical family is ALLOWED on issue_id — unchanged by #619.
	for i, id := range []string{
		store.UnattributedIssueID,
		store.UnattributedMainBucket,
		store.UnattributedDetachedHEADBucket,
		store.UnattributedNoIssueBucket,
	} {
		if code, body := post("alice", id, fmt.Sprintf("issue-canon-%d", i)); code >= 300 {
			t.Errorf("POST /events issue_id=%q: status = %d, want 2xx — #619 must not "+
				"disturb the #466 allowlist; a 4xx here is terminal for the shipper. body=%s",
				id, code, body)
		}
	}

	// The SAME values are REFUSED on developer — no allowlist, not even the exact
	// canonical spelling.
	for i, d := range []string{
		store.UnattributedIssueID,
		store.UnattributedMainBucket,
		store.UnattributedDetachedHEADBucket,
		store.UnattributedNoIssueBucket,
		"UNATTRIBUTED", "Unattributed:Main", "  unattributed  ",
	} {
		code, body := post(d, "issue-42", fmt.Sprintf("dev-forge-%d", i))
		if code != http.StatusBadRequest {
			t.Errorf("POST /events developer=%q: status = %d, want 400 — the shippable "+
				"collectors label events with --developer or OSUsername (fallback "+
				"%q), never the sentinel, so there is nothing here to allowlist. body=%s",
				d, code, "unknown", body)
		} else if !strings.Contains(string(body), "reserved") {
			t.Errorf("POST /events developer=%q rejected for the WRONG reason: %s", d, body)
		}
	}

	// Control arm: the collector's real shape — an ordinary developer carrying a
	// canonical unattributed issue — is the single most common event a shipper sends,
	// and it must still land.
	if code, body := post("alice", store.UnattributedMainBucket, "dev-forge-control"); code >= 300 {
		t.Fatalf("control arm rejected: status = %d body = %s — a developer who has ever "+
			"worked on main would now lose 100%% of their capture, permanently", code, body)
	}
}

// TestPostActualSpend_RejectsForgedDeveloper covers the tier-1 invoice ledger behind
// Spend Leverage — a denominator by another name. This surface is NOT named in #619;
// it was found by enumerating every client-supplied `developer` write.
func TestPostActualSpend_RejectsForgedDeveloper(t *testing.T) {
	h, _ := newTestHandler(t)
	body := func(dev string) map[string]any {
		return map[string]any{"developer": dev, "period": "2026-05", "actual_paid_usd": 200.0}
	}
	for _, d := range devForgeries {
		code, resp := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", body(d))
		if code != http.StatusBadRequest {
			t.Errorf("POST /actual_spend developer=%q: status = %d, want 400 — posting "+
				"your own invoice under the sentinel drops your actual-paid out of your "+
				"row exactly as forging /costs drops your metered cost. body=%s", d, code, resp)
		} else if !strings.Contains(string(resp), "reserved") {
			t.Errorf("POST /actual_spend developer=%q rejected for the WRONG reason: %s", d, resp)
		}
	}
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", body("mallory")); code >= 300 {
		t.Fatalf("control arm rejected too: status = %d body = %s", code, resp)
	}
}

// TestDeveloperAlias_RejectsForgedSentinelBothDirections closes the ONE-HOP BYPASS.
//
// 🔴 Without this the whole of #619 is decorative. The score join resolves every
// stored developer through the alias map before aggregating, so an alias row renames
// the identity space RETROACTIVELY, over rows already in the table — the guards on
// /costs, /events, /outcomes and /actual_spend all check a value this endpoint can
// then relabel. Two distinct attacks, hence both columns:
//
//   - canonical == sentinel is SELF-DEALING: {alice -> unattributed} folds alice's
//     whole cost and outcome history into the pseudo-developer.
//   - alias == sentinel is SABOTAGE: {unattributed -> bob} dumps every org-poller
//     aggregate and every proxy-unresolved dollar into BOB's denominator.
func TestDeveloperAlias_RejectsForgedSentinelBothDirections(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, tc := range []struct {
		name             string
		alias, canonical string
	}{
		{"canonical is the sentinel (self-dealing)", "alice", store.UnattributedIssueID},
		{"alias is the sentinel (sabotage)", store.UnattributedIssueID, "bob"},
		{"canonical is a case variant", "alice", "UNATTRIBUTED"},
		{"alias is a case variant", "Unattributed", "bob"},
		{"canonical is a labeled sub-bucket", "alice", store.UnattributedMainBucket},
		{"alias carries smuggling whitespace", "  unattributed  ", "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias",
				developerAliasRequest{Alias: tc.alias, Canonical: tc.canonical})
			if code != http.StatusBadRequest {
				t.Errorf("POST /developer_alias {%q -> %q}: status = %d, want 400 — an "+
					"alias is a retroactive rename of the identity space and bypasses "+
					"every write-side guard (body=%s)", tc.alias, tc.canonical, code, resp)
			} else if !strings.Contains(string(resp), "reserved") {
				t.Errorf("POST /developer_alias {%q -> %q} rejected for the WRONG reason: %s",
					tc.alias, tc.canonical, resp)
			}
		})
	}
	// Control arm: an ordinary alias still maps, so the 400s above are this guard and
	// not the store's pre-existing self-map / chain rules.
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/developer_alias",
		developerAliasRequest{Alias: "alice-gh", Canonical: "alice"}); code >= 300 {
		t.Fatalf("control arm rejected too: status = %d body = %s — ordinary aliasing "+
			"is broken and the assertions above are vacuous", code, resp)
	}
}

// TestOrgHierarchy_RejectsForgedSentinel covers the RED the FIRST enumeration MISSED.
//
// 🔴 WHY IT WAS MISSED, AND WHY IT IS THE WORST OF THE SET. org_hierarchy reads as
// org-structure admin, not a spend write, so it did not look like a `developer` write
// path at all. But store.upsertHierarchyTx does not merely file a label: it also opens
// a period_membership SEAT backdated to '0000-01' ("active since the beginning of
// time"), and nothing downstream excludes the sentinel from a per-developer aggregate.
//
// One authenticated write therefore drags the ENTIRE unattributed pool — ~72% of
// dogfood spend — into a rival team's denominator and craters its TIER, without
// touching a single cost row. Every other #619 surface is self-dealing; this one is
// aimed at somebody else.
//
// PUT and bulk are asserted together because the guard lives in the shared
// validateHierarchyRow: testing only one would leave the other free to drift.
func TestOrgHierarchy_RejectsForgedSentinel(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, d := range devForgeries {
		// Single PUT.
		code, resp := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/"+url.PathEscape(d),
			map[string]any{"team": "platform", "org": "acme"})
		if code != http.StatusBadRequest {
			t.Errorf("PUT /org_hierarchy/%q: status = %d, want 400 — this enrolls the "+
				"sentinel as a seat and moves the whole unattributed pool into a team "+
				"denominator (body=%s)", d, code, resp)
		} else if !strings.Contains(string(resp), "reserved") {
			t.Errorf("PUT /org_hierarchy/%q rejected for the WRONG reason: %s", d, resp)
		}
		// Bulk.
		code, resp = doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy",
			[]map[string]any{{"developer": d, "team": "platform", "org": "acme"}})
		if code != http.StatusBadRequest {
			t.Errorf("POST /org_hierarchy [developer=%q]: status = %d, want 400 (body=%s)", d, code, resp)
		} else if !strings.Contains(string(resp), "reserved") {
			t.Errorf("POST /org_hierarchy [developer=%q] rejected for the WRONG reason: %s", d, resp)
		}
	}

	// 🔴 ALL-OR-NOTHING: a forged row in a batch must reject the WHOLE batch, not land
	// the valid rows beside it. A partial hierarchy silently mis-aggregates, which is
	// the failure the bulk endpoint's transaction exists to prevent.
	code, resp := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", []map[string]any{
		{"developer": "alice", "team": "platform", "org": "acme"},
		{"developer": store.UnattributedIssueID, "team": "platform", "org": "acme"},
		{"developer": "bob", "team": "platform", "org": "acme"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("mixed batch: status = %d, want 400 (body=%s)", code, resp)
	}
	rows := getHierarchyDevelopers(t, h)
	if len(rows) != 0 {
		t.Errorf("mixed batch wrote %v; a batch containing a forged row must write "+
			"NOTHING — a partially applied hierarchy mis-aggregates silently", rows)
	}

	// Control arm: an ordinary developer still enrolls, on both shapes, so the 400s
	// above are this guard and not a broken fixture.
	if code, resp := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/alice",
		map[string]any{"team": "platform", "org": "acme"}); code >= 300 {
		t.Fatalf("control arm (PUT) rejected: status = %d body = %s", code, resp)
	}
	if code, resp := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy",
		[]map[string]any{{"developer": "bob", "team": "platform", "org": "acme"}}); code >= 300 {
		t.Fatalf("control arm (bulk) rejected: status = %d body = %s", code, resp)
	}

	// 🔴 THE HELPER MUST BE ABLE TO SEE ROWS AT ALL. The all-or-nothing assertion above
	// is `len(rows) != 0`, which a helper that always returns empty satisfies
	// vacuously — and the first draft of getHierarchyDevelopers did exactly that, by
	// decoding the response under the key "rows" when the real key is "hierarchy".
	// json.Unmarshal ignores unknown keys, so it stayed green with the guard deleted.
	// Asserting the helper is LIVE here is what converts that assertion into evidence.
	after := getHierarchyDevelopers(t, h)
	if len(after) != 2 {
		t.Fatalf("getHierarchyDevelopers = %v after two successful writes, want 2 rows — "+
			"the helper cannot see stored rows, so the all-or-nothing assertion above "+
			"proved nothing", after)
	}
}

// getHierarchyDevelopers lists the developers currently in org_hierarchy, for the
// all-or-nothing assertion above.
func getHierarchyDevelopers(t *testing.T, h *Handler) []string {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/org_hierarchy", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /org_hierarchy: status = %d body = %s", code, body)
	}
	// ⚠️ The key is "hierarchy", NOT "rows". The first draft of this helper guessed
	// "rows"; json.Unmarshal ignores unknown keys, so it decoded to an EMPTY slice on
	// every call and the all-or-nothing assertion (`len(rows) != 0`) passed
	// vacuously — it would have stayed green with the guard deleted and a partial
	// batch written. DisallowUnknownFields is what makes the shape a real assertion
	// rather than a hope.
	// Decoded into the REAL response type, not a hand-written lookalike: the field tag
	// then comes from the handler's own struct, so a renamed key is a compile error
	// here rather than a silently-empty slice.
	var resp hierarchyListResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode hierarchy: %v (body=%s) — if this is an unknown-field error the "+
			"response shape changed and this helper must be updated, not loosened", err, body)
	}
	out := make([]string, 0, len(resp.Hierarchy))
	for _, r := range resp.Hierarchy {
		out = append(out, r.Developer)
	}
	return out
}

// TestOrgPollerPathStillWritesTheSentinel IS THE CONTROL ARM FOR THE WHOLE ISSUE.
//
// "Refuses the sentinel" is trivially satisfiable by breaking real ingestion. The org
// pollers (internal/collector/anthropicadmin, internal/collector/openaiusage)
// legitimately assign developer="unattributed": an org-level invoice aggregate cannot
// honestly be split per person, and an explicit sentinel row is an HONEST coverage gap
// where silence would be a lie. This test drives the EXACT adapter cmd/tierd hands
// those pollers — ingester.Store(db), the same collector.Ingester their Run methods
// take — and asserts the sentinel still lands and still aggregates.
//
// It deliberately does NOT call the poller's HTTP-free path through a mock: the
// property under test is that the guard added on the HTTP surface did not migrate
// down into the shared write path underneath it.
func TestOrgPollerPathStillWritesTheSentinel(t *testing.T) {
	_, db := newTestHandler(t)
	ctx := context.Background()
	ing := ingester.Store(db)

	// The literal event shape anthropicadmin/poller.go emits for a daily org
	// aggregate: the sentinel on BOTH columns, daily fidelity, poller source.
	ev := collector.TokenEvent{
		Developer:      collector.UnattributedIssueID,
		IssueID:        collector.UnattributedIssueID,
		Model:          "claude-sonnet-4",
		InputTok:       120_000,
		OutputTok:      8_000,
		CostMicro:      store.DollarsToMicro(12.34),
		Source:         collector.SourceAnthropicAdmin,
		Fidelity:       collector.FidelityDaily,
		IdempotencyKey: "anthropic-admin|day|2026-05-19|claude-sonnet-4",
		Timestamp:      time.Now().UTC().Add(-time.Hour),
	}
	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("org poller ingest rejected: %v\n\n"+
			"The #619 guard has leaked out of the HTTP surface and into the shared "+
			"write path. Org-level invoice spend cannot be split per developer; "+
			"refusing it here does not close a forgery, it DELETES real money from "+
			"every org that runs a poller.", err)
	}

	costs, err := db.DeveloperCosts(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	var found bool
	for _, c := range costs {
		if c.Developer == collector.UnattributedIssueID {
			found = true
			if got, want := c.TotalCostMicro, store.DollarsToMicro(12.34); got != want {
				t.Errorf("poller row cost = %d micro, want %d", got, want)
			}
		}
	}
	if !found {
		t.Fatalf("the org poller's row did not surface as the %q pseudo-developer; "+
			"the sentinel is server-assignable in name only",
			collector.UnattributedIssueID)
	}
}

// TestProxyPathStillWritesTheSentinel is the second control arm: the proxy assigns
// developer="unattributed" for a 2xx with no X-Tier-Developer, and — like the pollers —
// reaches the store through ingester.Store(db) rather than over HTTP. Same property,
// different producer, because these are the only two and losing either is silent.
func TestProxyPathStillWritesTheSentinel(t *testing.T) {
	_, db := newTestHandler(t)
	ctx := context.Background()

	if err := ingester.Store(db).Ingest(ctx, collector.TokenEvent{
		Developer:      collector.UnattributedIssueID,
		IssueID:        collector.UnattributedIssueID,
		Model:          "claude-sonnet-4",
		InputTok:       500,
		CostMicro:      store.DollarsToMicro(0.75),
		Source:         collector.SourceProxy,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: "proxy|no-header|1",
		Repo:           repoid.Unqualified,
		Timestamp:      time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("proxy unattributed ingest rejected: %v — a proxied call with no "+
			"X-Tier-Developer would now vanish instead of landing in the visible "+
			"unattributed pool", err)
	}
}
