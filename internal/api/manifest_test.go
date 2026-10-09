package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// decodeManifest routes a manifest request and decodes it into a generic map.
// A map rather than reportManifestJSON on purpose: several assertions below are
// about a key's PRESENCE or ABSENCE on the wire, and decoding into the struct
// would make an absent key and a zero value indistinguishable — the exact
// confusion the pointer fields exist to prevent.
func decodeManifest(t *testing.T, h *Handler, target string) map[string]any {
	t.Helper()
	code, body := doRequest(t, h, "GET", target, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, body %s", target, code, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode manifest: %v (body %s)", err, body)
	}
	return m
}

// TestReportManifest_StampsSchemaWindowAndProvenance is the shape arm: every
// block the manifest promises is present, and the scheme tag identifies it.
func TestReportManifest_StampsSchemaWindowAndProvenance(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.5)

	m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")

	if got := m["manifest_schema"]; got != ManifestSchema {
		t.Errorf("manifest_schema = %v, want %q", got, ManifestSchema)
	}
	// 🔑 The literal value, not just "non-empty". A rename of the constant
	// compiles clean and silently reinterprets every manifest already published,
	// which is the one thing a scheme tag exists to prevent.
	if ManifestSchema != "tiermanifest1" {
		t.Errorf("ManifestSchema = %q, want %q — changing the tag reinterprets every published "+
			"manifest; bump it deliberately and update this pin", ManifestSchema, "tiermanifest1")
	}
	// RFC3339 INSTANTS, not dates. (The original reason — an omitted ?since=
	// carrying a time of day — was closed by #746's snap; the type is shipped and
	// the consumer must still read instants from other producers. See
	// reportManifestJSON.Since.)
	if got, _ := m["since"].(string); got != "2020-01-01T00:00:00Z" {
		t.Errorf("since = %v, want the RFC3339 instant 2020-01-01T00:00:00Z", m["since"])
	}
	// token_since is the resolved attribution-band lower bound: since - 14d.
	if got, _ := m["token_since"].(string); got != "2019-12-18T00:00:00Z" {
		t.Errorf("token_since = %v, want 2019-12-18T00:00:00Z (since - AttributableWindow)", m["token_since"])
	}
	// An open-ended window emits NO until key. It is a different report from one
	// bounded at today, so the absence is meaningful and must not be backfilled.
	if _, ok := m["until"]; ok {
		t.Errorf("until present on an open-ended window: %v", m["until"])
	}
	// Fleet-wide emits no repo key.
	if _, ok := m["repo"]; ok {
		t.Errorf("repo present on a fleet-wide read: %v", m["repo"])
	}
	if got := m["aggregation"]; got != "developer" {
		t.Errorf("aggregation = %v, want developer", got)
	}
	if got := m["tool_version"]; got != "test" {
		t.Errorf("tool_version = %v, want test", got)
	}

	pt, ok := m["price_table"].(map[string]any)
	if !ok {
		t.Fatalf("price_table missing or not an object: %v", m["price_table"])
	}
	for _, k := range []string{"version", "effective_date", "table_hash", "file_hash"} {
		if _, ok := pt[k]; !ok {
			t.Errorf("price_table.%s missing", k)
		}
	}
	rubric, ok := m["rubric"].(map[string]any)
	if !ok {
		t.Fatalf("rubric missing or not an object: %v", m["rubric"])
	}
	if got := rubric["version"]; got != float64(scoring.RubricVersion) {
		t.Errorf("rubric.version = %v, want %d", got, scoring.RubricVersion)
	}

	wmTop, ok := m["watermarks"].(map[string]any)
	if !ok {
		t.Fatalf("watermarks missing or not an object: %v", m["watermarks"])
	}
	window, ok := wmTop["window"].(map[string]any)
	if !ok {
		t.Fatalf("watermarks.window missing or not an object: %v", wmTop["window"])
	}
	ledgers, ok := wmTop["ledgers"].(map[string]any)
	if !ok {
		t.Fatalf("watermarks.ledgers missing or not an object: %v", wmTop["ledgers"])
	}
	// Flattened purely so the presence loop below can name every key in one list.
	wm := map[string]any{}
	for k, v := range window {
		wm[k] = v
	}
	for k, v := range ledgers {
		wm[k] = v
	}
	// Every ledger reaches the wire. This is what the store.Watermarks embed buys:
	// a field added there appears here with no hand-copied mapping to forget.
	//
	// 🔴 THIS LOOP ALSO OWNS THE `omitempty` CONCERN, and it is the only test that
	// can. On this fixture the four ledger watermarks are legitimately ZERO, so an
	// `omitempty` added to any of them would DROP the key here — making "absent"
	// and "zero" the same reading, and making a manifest diff show a key appearing
	// and disappearing as ledgers fill. A zero watermark is a real measurement
	// ("nothing has happened yet") and must always ship. Measured: adding
	// `,omitempty` to max_quality_history_id reddens this test and NOT the store's
	// coverage pin, which by design checks ledger coverage rather than encoding.
	for _, k := range []string{
		"max_token_event_id", "token_event_count",
		"max_outcome_id", "outcome_count",
		"max_quality_history_id", "quality_history_count",
		"max_reprice_row_audit_id", "reprice_row_audit_count",
		"max_cost_correction_audit_id", "cost_correction_audit_count",
		"max_repo_repair_row_audit_id", "repo_repair_row_audit_count",
		"max_push_outcome_audit_id", "push_outcome_audit_count",
	} {
		if _, ok := wm[k]; !ok {
			t.Errorf("watermarks.%s missing from the wire", k)
		}
	}
	if got := wm["max_token_event_id"]; got == float64(0) {
		t.Errorf("max_token_event_id = 0 after seeding one event — the watermark is not reading the window")
	}
}

// TestReportManifest_StampsPriceTableIdentity asserts the digests by VALUE, via
// the shared helper the other two digest-carrying endpoints use.
//
// ⚠️ Key PRESENCE is not enough and price_identity_test.go says so explicitly:
// priceTableJSON has no omitempty, so every key is present for any value —
// including the empty string a dropped assignment produces. The shape test above
// only checks presence; this is the one that would catch a blanked digest.
func TestReportManifest_StampsPriceTableIdentity(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)

	code, body := doRequest(t, h, "GET", "/api/v1/report_manifest?since=2020-01-01", nil)
	if code != http.StatusOK {
		t.Fatalf("GET report_manifest = %d, body %s", code, body)
	}
	var resp reportManifestJSON
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPriceIdentity(t, "/report_manifest", resp.PriceTable)
}

// TestReportManifest_WindowAndScopeReachTheStore pins the handler-to-store
// plumbing, which nothing else covered.
//
// 🔴 THE FAILURE IT CATCHES IS A MANIFEST THAT CONTRADICTS ITSELF. Passing
// `time.Time{}` and `store.FleetWide` to ReportWatermarks — ignoring ?until= and
// ?repo= entirely — left the whole api suite green, because the response still
// ECHOES the narrowed predicate correctly. The served document would then claim a
// scoped, bounded window over fleet-wide, open-ended watermarks: the two halves
// of the identity describing different questions.
func TestReportManifest_WindowAndScopeReachTheStore(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seed := func(issue, repo string, at time.Time) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: "alice", IssueID: issue, Model: "claude-sonnet-4",
			InputTok: 2000, CostMicro: 1000, Source: "jsonl", Fidelity: "realtime",
			Repo: repo, Timestamp: at,
		}); err != nil {
			t.Fatalf("InsertTokenEvent(%s): %v", issue, err)
		}
	}
	seed("issue-a", "acme/widgets", now.Add(-2*time.Hour))
	seed("issue-b", "acme/other", now.Add(-2*time.Hour))
	// Far in the future, so an `until` in the past must exclude it.
	seed("issue-future", "acme/widgets", now.AddDate(1, 0, 0))

	count := func(target string) float64 {
		t.Helper()
		m := decodeManifest(t, h, target)
		wm := m["watermarks"].(map[string]any)
		win, ok := wm["window"].(map[string]any)
		if !ok {
			t.Fatalf("watermarks.window missing on %s", target)
		}
		return win["token_event_count"].(float64)
	}

	fleet := count("/api/v1/report_manifest?since=2020-01-01")
	scoped := count("/api/v1/report_manifest?since=2020-01-01&repo=acme/widgets")
	// `until` is TOMORROW, not today. A bare date parses to that day at 00:00 and the
	// window is half-open [since, until), so `until=today` excludes every row seeded
	// at now-2h whenever the clock has passed 02:00 UTC -- i.e. this assertion held
	// for a two-hour slice of each day and failed for the other twenty-two. Measured:
	// it was merged at ~01:4x UTC (passing) and reddened at 02:05 UTC the same night.
	// Tomorrow's date is unconditionally after the two seeded rows and unconditionally
	// before the +1-year row, so the arm discriminates at every hour.
	bounded := count("/api/v1/report_manifest?since=2020-01-01&until=" + now.AddDate(0, 0, 1).Format("2006-01-02"))

	if fleet != 3 {
		t.Errorf("fleet-wide token_event_count = %v, want 3", fleet)
	}
	if scoped != 2 {
		t.Errorf("?repo=acme/widgets token_event_count = %v, want 2 — the repo scope is not "+
			"reaching the store, so the manifest echoes a scope it did not apply", scoped)
	}
	if bounded != 2 {
		t.Errorf("?until= token_event_count = %v, want 2 — the upper bound is not reaching the "+
			"store, so the manifest echoes a window it did not apply", bounded)
	}
}

// TestReportManifest_WithholdsWindowBlockInAnAnonymizedMode is the k-anonymity
// arm on the TIME axis, and it is the one that matters.
//
// 🔴 #593 IS CLOSED, and its fix is /scores' strip pass: a window narrowed until
// the cohort is sub-k withholds `total`, `cost_composition` and
// `segment_reconciliation`, and declares `kanon_suppressed`. That pass states its
// own rule — what may stay carries "no figure and no count of people or work".
// `token_event_count` and `outcome_count` are counts of work over a caller-chosen
// window. Publishing them would hand back, for exactly the windows /scores now
// refuses, the activity volume of the one contributor /scores declined to name,
// sweepable day by day. Refusing ?repo= does not help: the time axis needs no repo.
func TestReportManifest_WithholdsWindowBlockInAnAnonymizedMode(t *testing.T) {
	for _, mode := range []struct {
		name string
		agg  scoring.AggregationMode
	}{
		{"team", scoring.AggregationTeam},
		{"division", scoring.AggregationDivision},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			seedCosts(t, db, "alice", "issue-1", 1.0)
			h.SetAggregation(mode.agg, 5)

			m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
			wm, ok := m["watermarks"].(map[string]any)
			if !ok {
				t.Fatalf("watermarks missing: %v", m["watermarks"])
			}
			if v, present := wm["window"]; present {
				t.Errorf("watermarks.window = %v is published in %s mode — it carries unfloored "+
					"row counts over a caller-chosen window, which is the quantity the k-anonymity "+
					"floor exists to protect", v, mode.name)
			}
			// The ledgers half MUST remain: it is install-wide and window-independent,
			// so it carries no count of the caller's chosen population. Without this,
			// withholding the entire watermarks object would pass the check above.
			if _, present := wm["ledgers"]; !present {
				t.Errorf("watermarks.ledgers was withheld too — it is unwindowed and unscoped, " +
					"so it carries no cohort count and must survive")
			}
			// 🔴 THE WITHHOLD MUST BE DECLARED. A manifest that goes silently quiet is
			// the failure mode /scores' kanon_suppressed block exists to prevent:
			// absent must never be confusable with "this install has no rows".
			sup, ok := m["kanon_suppressed"].(map[string]any)
			if !ok {
				t.Fatalf("kanon_suppressed missing in %s mode — the window block was withheld "+
					"SILENTLY, which reads as an empty install", mode.name)
			}
			if sup["withheld_window"] != true {
				t.Errorf("kanon_suppressed.withheld_window = %v, want true", sup["withheld_window"])
			}
			if sup["k_anonymity"] != float64(5) {
				t.Errorf("kanon_suppressed.k_anonymity = %v, want 5", sup["k_anonymity"])
			}
			if r, _ := sup["reason"].(string); r == "" {
				t.Error("kanon_suppressed.reason is empty — a withhold with no reason is not a declaration")
			}
		})
	}

	// 🔴 CONTROL ARM. Developer mode must still publish the window block and must
	// carry NO suppression declaration. Without this, an implementation that
	// withheld unconditionally — making the endpoint useless — would pass every
	// assertion above.
	t.Run("developer mode publishes it", func(t *testing.T) {
		h, db := newTestHandler(t)
		seedCosts(t, db, "alice", "issue-1", 1.0)
		h.SetAggregation(scoring.AggregationDeveloper, 5)

		m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
		wm := m["watermarks"].(map[string]any)
		win, ok := wm["window"].(map[string]any)
		if !ok {
			t.Fatalf("control: watermarks.window withheld in DEVELOPER mode — the endpoint would " +
				"be useless and the assertions above would prove nothing")
		}
		if win["token_event_count"] == float64(0) {
			t.Error("control: token_event_count is 0 after seeding one event")
		}
		if v, present := m["kanon_suppressed"]; present {
			t.Errorf("kanon_suppressed = %v declared in developer mode, where nothing was withheld", v)
		}
	})
}

// TestReportManifest_WithheldReasonsDoNotAdviseDeveloperMode pins #959: in an
// anonymized mode the manifest still withholds the digests and the window block,
// and neither declared reason points the reader at developer aggregation or the
// --aggregation flag: that mode publishes named per-developer rows to every
// read-token holder.
func TestReportManifest_WithheldReasonsDoNotAdviseDeveloperMode(t *testing.T) {
	for _, mode := range []struct {
		name string
		agg  scoring.AggregationMode
	}{
		{"team", scoring.AggregationTeam},
		{"division", scoring.AggregationDivision},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			seedCosts(t, db, "alice", "issue-1", 1.0)
			h.SetAggregation(mode.agg, 5)

			m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
			for _, key := range []string{"events_digest", "outcomes_digest"} {
				if v, ok := m[key]; ok {
					t.Errorf("%s = %v published in %s mode", key, v, mode.name)
				}
			}
			wm, ok := m["watermarks"].(map[string]any)
			if !ok {
				t.Fatalf("watermarks missing in %s mode: %v", mode.name, m["watermarks"])
			}
			if v, present := wm["window"]; present {
				t.Errorf("watermarks.window = %v published in %s mode", v, mode.name)
			}
			sup, ok := m["kanon_suppressed"].(map[string]any)
			if !ok {
				t.Fatalf("kanon_suppressed missing in %s mode: %v", mode.name, m["kanon_suppressed"])
			}
			for _, flag := range []string{"withheld_window", "withheld_digests"} {
				if sup[flag] != true {
					t.Errorf("kanon_suppressed.%s = %v in %s mode, want true", flag, sup[flag], mode.name)
				}
			}
			omitted, _ := m["digests_omitted"].(string)
			reason, _ := sup["reason"].(string)

			// Each reason names what it withholds and why; the anchors are field
			// names and issue numbers, not sentences.
			for _, want := range []string{"events_digest", "outcomes_digest"} {
				if !strings.Contains(omitted, want) {
					t.Errorf("digests_omitted does not name %s in %s mode: %q", want, mode.name, omitted)
				}
			}
			if !strings.Contains(omitted, "k-anonymity") && !strings.Contains(omitted, "#593") {
				t.Errorf("digests_omitted names no k-anonymity cause in %s mode: %q", mode.name, omitted)
			}
			if !strings.Contains(reason, "watermarks.window") {
				t.Errorf("kanon_suppressed.reason does not name watermarks.window in %s mode: %q", mode.name, reason)
			}

			// Neither reason legitimately mentions a developer or an aggregation
			// setting, so any such token is advice to switch modes.
			for field, r := range map[string]string{"digests_omitted": omitted, "kanon_suppressed.reason": reason} {
				for _, advice := range []string{"developer", "--aggregation", "aggregation=", "aggregation:"} {
					if strings.Contains(strings.ToLower(r), advice) {
						t.Errorf("%s points at a mode switch (%q) in %s mode: %q", field, advice, mode.name, r)
					}
				}
			}
		})
	}
}

// TestReportManifest_OmitsPriceTableSource pins a SECURITY decision, not a
// preference. price_table.source is a local filesystem path; the 2026-08-28
// ruling on the #713 review confined it to `tierd score-log` (a local CLI) and
// kept it off served surfaces. #715's spec text asks for it, and this test is
// the record that the deviation is deliberate rather than an omission — delete
// the guard and re-add the field and this reddens.
func TestReportManifest_OmitsPriceTableSource(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)

	m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
	pt, ok := m["price_table"].(map[string]any)
	if !ok {
		t.Fatalf("price_table missing: %v", m["price_table"])
	}
	if v, ok := pt["source"]; ok {
		t.Errorf("price_table.source = %v is published on a SERVED surface; the #713 ruling "+
			"confines the local filesystem path to `tierd score-log`", v)
	}
	// Control: the block is not simply empty. Without this, deleting price_table
	// entirely would pass the assertion above.
	if pt["table_hash"] == nil || pt["table_hash"] == "" {
		t.Fatalf("control arm: price_table.table_hash is empty, so the absence check above proves nothing")
	}
}

// TestReportManifest_KIsPresentOnlyUnderAnAnonymizedMode pins the pointer. In
// developer mode NO floor is applied, so publishing the configured integer would
// assert a protection that is not in force; in team mode it must be published,
// because it is part of what makes the numbers what they are.
func TestReportManifest_KIsPresentOnlyUnderAnAnonymizedMode(t *testing.T) {
	t.Run("developer mode omits k", func(t *testing.T) {
		h, db := newTestHandler(t)
		seedCosts(t, db, "alice", "issue-1", 1.0)
		h.SetAggregation(scoring.AggregationDeveloper, 5)

		m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
		if v, ok := m["k"]; ok {
			t.Errorf("k = %v published in developer mode, where no floor is applied", v)
		}
	})

	t.Run("team mode publishes k", func(t *testing.T) {
		h, db := newTestHandler(t)
		seedCosts(t, db, "alice", "issue-1", 1.0)
		h.SetAggregation(scoring.AggregationTeam, 5)

		m := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")
		if got := m["k"]; got != float64(5) {
			t.Errorf("k = %v, want 5", got)
		}
		if got := m["aggregation"]; got != "team" {
			t.Errorf("aggregation = %v, want team", got)
		}
	})
}

// TestReportManifest_RefusesRepoScopeInAnAnonymizedMode is the k-anonymity arm.
// The manifest publishes per-repo ROW COUNTS over a caller-chosen window, and a
// cohort count is exactly the quantity the floor protects. It must refuse the
// same requests /scores refuses — via the same shared method, so the two cannot
// drift apart.
func TestReportManifest_RefusesRepoScopeInAnAnonymizedMode(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)
	h.SetAggregation(scoring.AggregationTeam, 5)

	code, body := doRequest(t, h, "GET", "/api/v1/report_manifest?since=2020-01-01&repo=acme/widgets", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("scoped manifest in team mode = %d, want 400; body %s", code, body)
	}

	// Control arms. The refusal must be caused by the MODE, not by the endpoint
	// rejecting ?repo= outright or by team mode rejecting everything — either of
	// which would make the assertion above pass for the wrong reason.
	h.SetAggregation(scoring.AggregationDeveloper, 5)
	if code, body := doRequest(t, h, "GET", "/api/v1/report_manifest?since=2020-01-01&repo=acme/widgets", nil); code != http.StatusOK {
		t.Fatalf("control: scoped manifest in DEVELOPER mode = %d, want 200; body %s", code, body)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	if code, body := doRequest(t, h, "GET", "/api/v1/report_manifest?since=2020-01-01", nil); code != http.StatusOK {
		t.Fatalf("control: UNSCOPED manifest in team mode = %d, want 200; body %s", code, body)
	}
}

// TestReportManifest_RejectsUnknownQueryParams pins the strict allowlist (#590).
// ?work_type= is the interesting case: /scores accepts it, this endpoint does
// not, and silently ignoring it would let a caller believe they had a manifest
// for a filtered report when they hold one for the whole window.
func TestReportManifest_RejectsUnknownQueryParams(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0)

	for _, target := range []string{
		"/api/v1/report_manifest?since=2020-01-01&work_type=feature",
		"/api/v1/report_manifest?since=2020-01-01&team=platform",
		"/api/v1/report_manifest?since=2020-01-01&nonsense=1",
	} {
		if code, body := doRequest(t, h, "GET", target, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400; body %s", target, code, body)
		}
	}
	// Control: the allowlisted parameters are all accepted, so the loop above is
	// not passing because the endpoint rejects everything.
	for _, target := range []string{
		"/api/v1/report_manifest?since=2020-01-01",
		"/api/v1/report_manifest?since=2020-01-01&until=2030-01-01",
		"/api/v1/report_manifest?since=2020-01-01&repo=acme/widgets",
	} {
		if code, body := doRequest(t, h, "GET", target, nil); code != http.StatusOK {
			t.Errorf("control: GET %s = %d, want 200; body %s", target, code, body)
		}
	}
}

// TestReportManifest_QualityRevisionMovesTheManifest is the end-to-end statement
// of the finding, asserted on the WIRE rather than in the store: a quality
// revision changes what /scores publishes while creating no new token_event and
// no new outcome. If the served manifest could not see that, its whole claim
// would be false at the surface that actually makes it.
//
// Both halves are asserted, and the vacuity control comes first.
func TestReportManifest_QualityRevisionMovesTheManifest(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedCosts(t, db, "alice", "issue-7", 2.0)
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		Developer:      "alice",
		IssueID:        "issue-7",
		PRNumber:       7,
		Weight:         3,
		Quality:        1.0,
		MergeCommitSHA: "sha-manifest-7",
		Timestamp:      now,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	o, ok, err := db.OutcomeByMergeCommit(ctx, "sha-manifest-7")
	if err != nil || !ok {
		t.Fatalf("OutcomeByMergeCommit: ok=%v err=%v", ok, err)
	}

	// Read the two halves separately: the whole point of this test is that one
	// moves and the other does not.
	halves := func() (window, ledgers map[string]any) {
		t.Helper()
		wm := decodeManifest(t, h, "/api/v1/report_manifest?since=2020-01-01")["watermarks"].(map[string]any)
		return wm["window"].(map[string]any), wm["ledgers"].(map[string]any)
	}
	beforeWindow, beforeLedgers := halves()

	// 🔴 Vacuity control: UpdateQualityForOutcome is a NO-OP when the value is
	// unchanged, so the fixture must set a genuinely different quality. 0.5 != 1.0.
	if err := db.UpdateQualityForOutcome(ctx, o.ID, 0.5, "ci_fail", "run-manifest"); err != nil {
		t.Fatalf("UpdateQualityForOutcome: %v", err)
	}
	// And confirm the revision really landed, independently of the manifest,
	// before concluding anything from the manifest.
	after, ok2, err := db.OutcomeByMergeCommit(ctx, "sha-manifest-7")
	if err != nil || !ok2 {
		t.Fatalf("re-read outcome: ok=%v err=%v", ok2, err)
	}
	if after.Quality != 0.5 {
		t.Fatalf("quality = %v, want 0.5: the revision was a no-op and this test would prove nothing", after.Quality)
	}

	gotWindow, gotLedgers := halves()

	// (a) the negative half — no new rows anywhere obvious.
	for _, k := range []string{"max_token_event_id", "max_outcome_id", "token_event_count", "outcome_count"} {
		if gotWindow[k] != beforeWindow[k] {
			t.Errorf("watermarks.window.%s moved on an in-place quality revision: %v -> %v",
				k, beforeWindow[k], gotWindow[k])
		}
	}
	// (b) the positive half.
	if gotLedgers["max_quality_history_id"] == beforeLedgers["max_quality_history_id"] {
		t.Errorf("watermarks.ledgers.max_quality_history_id did not move (%v) on a real quality "+
			"revision — the served manifest cannot see an in-place mutation, which is the whole "+
			"point of #715", beforeLedgers["max_quality_history_id"])
	}
	if gotLedgers["quality_history_count"] == beforeLedgers["quality_history_count"] {
		t.Errorf("watermarks.ledgers.quality_history_count did not move (%v)",
			beforeLedgers["quality_history_count"])
	}
	// (c) and quality_history is the ONLY ledger that moved. Without this, all
	// four ledger reads could be addressing the same table and nothing on the
	// wire would notice — three of them are 0/0 in every fixture.
	for _, k := range []string{
		"max_reprice_row_audit_id", "reprice_row_audit_count",
		"max_cost_correction_audit_id", "cost_correction_audit_count",
		"max_repo_repair_row_audit_id", "repo_repair_row_audit_count",
	} {
		if gotLedgers[k] != beforeLedgers[k] {
			t.Errorf("watermarks.ledgers.%s moved on a QUALITY revision (%v -> %v) — that ledger's "+
				"read is not addressing its own table", k, beforeLedgers[k], gotLedgers[k])
		}
	}
}
