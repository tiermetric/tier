package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/event3"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestVerifyReport_EngineF1NoScoresCorrectionCoverage(t *testing.T) {
	for _, kind := range []string{"confirm", "allocation_only", "omitted_digest"} {
		t.Run(kind, func(t *testing.T) {
			f := seedVerifyDB(t)
			db, err := store.Open(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := t.Context()
			var ids []int64
			for i := 1; i <= 2; i++ {
				at := time.Date(2026, 8, 10+i, 0, 0, 0, 0, time.UTC)
				o := store.Outcome{Developer: "alice", Repo: "acme/tier", PRNumber: 900 + i, IssueID: "engine-f1", MergeCommitSHA: "engine-f1-" + strings.Repeat("s", i), Weight: 5, Quality: 1, Timestamp: at, MergedAt: &at}
				body := "Corrects: none"
				if i == 2 {
					o.Weight, body = 1, "Corrects: #901"
				}
				a, b, c := 10, 0, 1
				o.SetDiffStats(&a, &b, &c)
				if _, err := db.InsertOutcome(ctx, o); err != nil {
					t.Fatal(err)
				}
				stored, ok, err := db.OutcomeByMergeCommit(ctx, o.MergeCommitSHA)
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				ids = append(ids, stored.ID)
				if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "engine F1 evidence", "test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
				t.Fatal(err)
			}
			f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil, api.WithStoredScoringPolicy())
			m := readFixtureManifest(t, f.manifestPath)
			m.Results = nil
			if kind == "omitted_digest" {
				m.CorrectionsDigest = nil
				m.OutcomeCounts = nil
				m.DigestsOmitted = "corrective evidence unavailable"
			}
			writeManifest(t, f.manifestPath, m)
			if kind != "omitted_digest" {
				if rc, out, stderr := runVerify(t, f.manifestPath, "--db", f.dbPath); rc != rcReproduced {
					t.Fatal("baseline", rc, out, stderr)
				}
			}
			if kind == "allocation_only" {
				execOnFixture(t, f.dbPath, `UPDATE correction_allocations SET allocated_points='99/1' WHERE outcome_id=?`, ids[0])
			} else {
				raw, err := sql.Open("sqlite", f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = raw.Close() }()
				var relation int64
				if err := raw.QueryRow(`SELECT id FROM corrective_relations WHERE child_outcome_id=?`, ids[1]).Scan(&relation); err != nil {
					t.Fatal(err)
				}
				review := event3.Review{Revision: 1, Original: big.NewRat(5, 1), Own: big.NewRat(1, 1), Before: big.NewRat(1, 1), After: big.NewRat(3, 1), Scope: "original scope", Rationale: "repair and growth", Reviewer: "operator", ReviewedAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), ComparisonBase: "base", ComparisonBefore: "before", ComparisonAfter: "after"}
				if err := db.ConfirmCorrection(ctx, store.CorrectionCommand{RelationID: relation, Revision: 1, Actor: "operator", Role: "operator", Review: &review}); err != nil {
					t.Fatal(err)
				}
			}
			want := rcDiverged
			status := "CHANGED"
			if kind == "omitted_digest" {
				want, status = rcCannotCheck, "NOT CHECKED"
			}
			rc, out, stderr := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if rc != want {
				t.Fatalf("no-scores %s: rc=%d want=%d\n%s\n%s", kind, rc, want, out, stderr)
			}
			assertDimStatus(t, out, "corrections_digest", status)
			assertDimStatus(t, out, "events_digest", "UNCHANGED")
			assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
			t.Logf("no results.scores: rc=%d corrections_digest %s", rc, status)
		})
	}
}

// U2's Event 3 reader consumes persisted projections and refuses missing ones.
// Resolve them before publication and serve both documents under the same policy;
// changing only a legacy manifest's policy no longer makes a valid fixture.
func newEvent3VerifyFixture(t *testing.T) verifyFixture {
	t.Helper()
	f := seedVerifyDB(t)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, id := range f.outcomeIDs {
		if err := db.ReconcileCorrectionChain(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	policy := scoring.ScoringPolicy{Event3: true, Resolved: true, RuleVersion: "event3-v2"}
	return serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil, api.WithScoringPolicy(policy))
}

func TestVerifyReport_ReproducesServedEvent3(t *testing.T) {
	f := newEvent3VerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	if m.ScoringPolicy != "event3" || m.CorrectionRuleVersion != "event3-v2" || m.Rubric.Version != 2 || m.RubricVersion != 2 {
		t.Fatalf("fixture did not serve Event 3: %+v", m)
	}
	if m.OutcomeCounts == nil || m.OutcomeCounts.OutcomeN == nil || m.OutcomeCounts.SampleN == nil || *m.OutcomeCounts.OutcomeN != 3 || *m.OutcomeCounts.SampleN != 3 {
		t.Fatalf("fixture lost its raw/eligible observations: %+v", m.OutcomeCounts)
	}
	code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced || !strings.Contains(out, "REPRODUCED") || !strings.Contains(out, "the recomputed report is identical") {
		t.Fatalf("served Event 3 replay failed: rc=%d\n%s\n%s", code, out, errout)
	}
	for _, dim := range []string{"rubric", "scoring policy", "events_digest", "outcomes_digest", "corrections_digest", "outcome_counts"} {
		assertDimStatus(t, out, dim, "UNCHANGED")
	}
}

func TestVerifyReport_EngineF4ExplicitLegacyMatchesDisabledPolicy(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	m.ScoringPolicy, m.CorrectionRuleVersion = "legacy", "legacy-v1"
	m.RubricVersion, m.Rubric.Version = 1, 1
	writeManifest(t, f.manifestPath, m)
	rc, out, stderr := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if rc != rcReproduced || strings.Contains(out, " CHANGED ") {
		t.Fatal(rc, out, stderr)
	}
	assertDimStatus(t, out, "scoring policy", "UNCHANGED")
}

func TestVerifyReport_Event3MissingProjectionIsNotCheckable(t *testing.T) {
	f := newEvent3VerifyFixture(t)
	execOnFixture(t, f.dbPath, `DELETE FROM correction_allocations WHERE outcome_id=(SELECT MIN(id) FROM outcomes)`)
	code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcCannotCheck || strings.Contains(out, "REPRODUCED") || !strings.Contains(errout, "HTTP 500") || !strings.Contains(errout, "NOTHING was verified") {
		t.Fatalf("unresolved Event 3 projection accepted: rc=%d\n%s\n%s", code, out, errout)
	}
}

func TestVerifyReport_OldSchemeIsNotFalseTampering(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	// Frozen tierdig1 empty-window literals from origin/main's store goldens.
	// Recomputing the expected pins here would hide a serializer regression.
	m.Since, m.Until, m.TokenSince = "2025-01-01", "2025-02-01", ""
	m.Results = nil
	m.Watermarks = nil
	m.EventsDigest = &manifestDigest{Value: "tierdig1:471f175a8135c504950c9e9f9e92aba4450fd5efa718db0684abe3f5857d1f52", Rows: 0}
	m.OutcomesDigest = &manifestDigest{Value: "tierdig1:958df6619e6881d1bf974a9d410d0471f0ee8c9eb38011e1e681cce205f1d758", Rows: 0}
	m.ScoringPolicy = ""
	m.CorrectionRuleVersion = ""
	m.RubricVersion = 0
	m.OutcomeCounts = nil
	m.CorrectionsDigest = nil
	writeManifest(t, f.manifestPath, m)
	code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != 0 || !strings.Contains(out, "REPRODUCED") {
		t.Fatalf("old scheme not clean: rc=%d\n%s\n%s", code, out, errout)
	}
}

func TestVerifyReport_ReplaysPinnedEvent3Policy(t *testing.T) {
	for _, policy := range []string{"legacy", "event3"} {
		m := reportManifest{ScoringPolicy: policy, CorrectionRuleVersion: "legacy-v1", RubricVersion: 1, Rubric: manifestRubric{Version: 1}}
		if policy == "event3" {
			m.CorrectionRuleVersion = "event3-v2"
			m.RubricVersion = 2
			m.Rubric.Version = 2
		}
		p, d, err := manifestScoringPolicy(m)
		if err != nil || p.Event3 != (policy == "event3") || d.Status != dimUnchanged {
			t.Fatalf("policy lost: %+v %+v %v", p, d, err)
		}
	}
	m := reportManifest{ScoringPolicy: "event3", CorrectionRuleVersion: "future", RubricVersion: 2}
	if _, _, err := manifestScoringPolicy(m); err == nil {
		t.Fatal("unknown policy silently reruns defaults")
	}
}

func TestVerifyReport_CorrectionChangeIsDetected(t *testing.T) {
	f := newEvent3VerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	if code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath); code != rcReproduced {
		t.Fatalf("baseline Event 3 replay: rc=%d\n%s\n%s", code, out, errout)
	}
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var id int64
	if err := raw.QueryRow(`SELECT id FROM outcomes ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Evidence inserted after publication still belongs to the endpoint's window.
	if _, err := raw.Exec(`INSERT INTO corrective_relations(child_outcome_id,state,revision,recorded_at,updated_at,rule_version,disposition_text) VALUES(?,'disputed',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v2','operator review')`, id); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	w, err := resolveWindow(m)
	if err != nil {
		t.Fatal(err)
	}
	policy, _, err := manifestScoringPolicy(m)
	if err != nil {
		t.Fatal(err)
	}
	dims, err := verifyCorrectionPins(context.Background(), db, m, w, "", policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(dims) == 0 || dims[0].Name != "corrections_digest" || dims[0].Status != dimChanged {
		t.Fatalf("late Event 3 correction missed: %+v", dims)
	}
	code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged || strings.Contains(out, "REPRODUCED") {
		t.Fatalf("late Event 3 correction replay: rc=%d\n%s\n%s", code, out, errout)
	}
	assertDimStatus(t, out, "corrections_digest", "CHANGED")
	assertDimStatus(t, out, "events_digest", "UNCHANGED")
	assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
}

func TestVerifyReport_CorrectivePinsCannotGoUnchecked(t *testing.T) {
	for _, kind := range []string{"scheme", "counts", "policy", "rubric"} {
		t.Run(kind, func(t *testing.T) {
			f := newVerifyFixture(t)
			m := readFixtureManifest(t, f.manifestPath)
			switch kind {
			case "scheme":
				m.CorrectionsDigest.Value = "tierdig999:" + strings.Repeat("aa", 32)
			case "counts":
				*m.CorrectionsDigest.Counts.Relations++
			case "policy":
				m.ScoringPolicy = "future-policy"
			case "rubric":
				m.RubricVersion = 2
			}
			writeManifest(t, f.manifestPath, m)
			code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcCannotCheck || strings.Contains(out, "REPRODUCED") {
				t.Fatalf("unchecked %s allowed: rc=%d\n%s\n%s", kind, code, out, errout)
			}
		})
	}
}

func TestVerifyReport_CorrectiveCountPresenceIsRequired(t *testing.T) {
	for _, whole := range []bool{true, false} {
		t.Run(map[bool]string{true: "whole_counts", false: "zero_audit_count"}[whole], func(t *testing.T) {
			f := newVerifyFixture(t)
			raw, err := os.ReadFile(f.manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			d := doc["corrections_digest"].(map[string]any)
			if whole {
				delete(d, "counts")
			} else {
				delete(d["counts"].(map[string]any), "audit")
			}
			raw, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.manifestPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcCannotCheck {
				t.Fatalf("missing count read as zero: rc=%d\n%s\n%s", code, out, errout)
			}
		})
	}
}

func TestVerifyReport_OutcomeCountPresenceIsRequired(t *testing.T) {
	f := newVerifyFixture(t)
	raw, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["outcome_counts"].(map[string]any), "sample_n")
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcCannotCheck {
		t.Fatalf("missing sample count treated as change: rc=%d\n%s\n%s", code, out, errout)
	}
}

func TestVerifyReport_UnknownRubricIsChanged(t *testing.T) {
	for _, policy := range []string{"legacy", "event3"} {
		t.Run(policy, func(t *testing.T) {
			fixture := newVerifyFixture
			if policy == "event3" {
				fixture = newEvent3VerifyFixture
			}
			f := fixture(t)
			m := readFixtureManifest(t, f.manifestPath)
			m.Rubric.Version, m.RubricVersion = 99, 99
			writeManifest(t, f.manifestPath, m)
			code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcDiverged {
				t.Fatalf("R-2026-10-14-20 requires unknown rubric as changed: rc=%d\n%s\n%s", code, out, errout)
			}
			assertDimStatus(t, out, "rubric", "CHANGED")
		})
	}
}

// R1 RECEIPTS: global progress neither refutes nor verifies this window.
func TestVerifyReport_ReceiptR1CorrectiveContext(t *testing.T) {
	for _, kind := range []string{"unrelated_repo", "projection_only", "revision_only_manifest", "partial_pins"} {
		t.Run(kind, func(t *testing.T) {
			f := newVerifyFixture(t)
			m := readFixtureManifest(t, f.manifestPath)
			query := ""
			want := rcReproduced
			switch kind {
			case "unrelated_repo":
				query = `INSERT INTO correction_audit(revision,action,recorded_at,rule_version,child_repo) VALUES(1,'declare',CURRENT_TIMESTAMP,'event3-v2','other/repo')`
			case "projection_only":
				query = `UPDATE correction_policy SET projection_revision=projection_revision+1 WHERE id=1`
			case "revision_only_manifest":
				m.Results = nil
				m.EventsDigest, m.OutcomesDigest, m.CorrectionsDigest, m.OutcomeCounts = nil, nil, nil, nil
				led := m.ledgers()
				m.Watermarks = &manifestWatermarks{Ledgers: &manifestLedgerWatermarks{MaxCorrectionAuditID: led.MaxCorrectionAuditID, CorrectionAuditCount: led.CorrectionAuditCount, CorrectionProjectionRevision: led.CorrectionProjectionRevision}}
				query = `UPDATE outcomes SET quality=0.01,weight=99`
				want = rcCannotCheck
			case "partial_pins":
				m.Watermarks.Ledgers.CorrectionAuditCount = nil
				want = rcCannotCheck
			}
			writeManifest(t, f.manifestPath, m)
			if query != "" {
				raw, err := sql.Open("sqlite", f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = raw.Close() }()
				if _, err := raw.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != want {
				t.Fatalf("rc=%d want=%d\n%s\n%s", code, want, out, errout)
			}
			status := "UNCHANGED"
			if kind == "partial_pins" {
				status = "NOT CHECKED"
			}
			assertDimStatus(t, out, "corrective revisions", status)
			if kind == "unrelated_repo" || kind == "projection_only" {
				assertDimStatus(t, out, "events_digest", "UNCHANGED")
				assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
				if !strings.Contains(out, "install-wide corrective audit/revision moved") {
					t.Fatal("install-wide movement was not disclosed")
				}
			}
		})
	}
}

func TestVerifyReport_ReceiptR2LegacyDeclarationIsContext(t *testing.T) {
	f := newVerifyFixture(t)
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	// A later Corrects declaration points back into the published window.
	_, err = raw.Exec(`INSERT INTO correction_declarations(declaration_state,supporting_text,merged_at,recorded_at,rule_version) VALUES('parents','Corrects: #42','2026-10-01',CURRENT_TIMESTAMP,'event3-v2');
 INSERT INTO corrective_relations(child_declaration_id,parent_outcome_id,state,revision,recorded_at,updated_at,rule_version) VALUES(last_insert_rowid(),(SELECT MIN(id) FROM outcomes),'pending',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'event3-v2')`)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcReproduced {
		t.Fatalf("legacy declaration diverged: rc=%d\n%s\n%s", code, out, errout)
	}
	assertDimStatus(t, out, "corrections context", "UNCHANGED")
	assertDimStatus(t, out, "events_digest", "UNCHANGED")
	assertDimStatus(t, out, "outcomes_digest", "UNCHANGED")
	if !strings.Contains(out, "informational") || !strings.Contains(out, "moved") {
		t.Fatal("missing informational movement")
	}
}

// U2 now applies the requested Event 3 policy. Simulate its predecessor by
// serving the real legacy reader while ignoring the requested policy, retaining
// the original command-level rc 2 / NOT CHECKED / never REPRODUCED assertions.
func TestVerifyReport_ReceiptR3RejectsLegacyReplayForEvent3(t *testing.T) {
	f := newEvent3VerifyFixture(t)
	legacyReader := func(ctx context.Context, db *store.DB, w manifestWindow, scope string, mode scoring.AggregationMode, k int, _ ...scoring.ScoringPolicy) (json.RawMessage, error) {
		return rerunScores(ctx, db, w, scope, mode, k)
	}
	var out, errout bytes.Buffer
	code := runVerifyReportCmdWithReplay([]string{f.manifestPath, "--db", f.dbPath}, &out, &errout, legacyReader)
	if code != rcCannotCheck || strings.Contains(out.String(), "REPRODUCED") {
		t.Fatalf("legacy replay accepted: rc=%d\n%s\n%s", code, &out, &errout)
	}
	if !strings.Contains(out.String()+errout.String(), "NOT CHECKED") {
		t.Fatalf("missing refusal: %s\n%s", &out, &errout)
	}
	if !strings.Contains(errout.String(), "legacy-v1") || !strings.Contains(errout.String(), "reader returned") || !strings.Contains(errout.String(), "manifest requires event3/event3-v2/rubric 2") {
		t.Fatalf("refusal did not identify the policy mismatch: %s", &errout)
	}

	// Keep 54ebc9a's R-20 interaction: an unsupported rubric is still
	// refuted as CHANGED even when a legacy reader cannot replay Event 3.
	m := readFixtureManifest(t, f.manifestPath)
	m.Rubric.Version, m.RubricVersion = 99, 99
	writeManifest(t, f.manifestPath, m)
	out.Reset()
	errout.Reset()
	code = runVerifyReportCmdWithReplay([]string{f.manifestPath, "--db", f.dbPath}, &out, &errout, legacyReader)
	if code != rcDiverged || strings.Contains(out.String(), "REPRODUCED") {
		t.Fatalf("unsupported rubric lost its refutation: rc=%d\n%s\n%s", code, &out, &errout)
	}
	assertDimStatus(t, out.String(), "rubric", "CHANGED")
	assertDimStatus(t, out.String(), "scoring policy", "NOT CHECKED")
}

func TestVerifyReport_ReceiptR3ValidatesEveryReplayStamp(t *testing.T) {
	f := newEvent3VerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	policy := scoring.ScoringPolicy{Event3: true, Resolved: true, RuleVersion: "event3-v2"}
	for _, kind := range []string{"served", "policy", "rule", "nested_rubric", "missing_nested_rubric", "body_rubric", "missing_receipt", "conflicting_flat_stamp"} {
		t.Run(kind, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(m.Results.Scores, &doc); err != nil {
				t.Fatal(err)
			}
			stamp := doc["correction_policy"].(map[string]any)
			switch kind {
			case "policy":
				stamp["scoring_policy"] = "legacy"
			case "rule":
				stamp["correction_rule_version"] = "future"
			case "nested_rubric":
				stamp["rubric_version"] = 1
			case "missing_nested_rubric":
				delete(stamp, "rubric_version")
			case "body_rubric":
				doc["rubric"].(map[string]any)["version"] = 1
			case "missing_receipt":
				delete(doc, "correction_policy")
			case "conflicting_flat_stamp":
				doc["scoring_policy"] = "legacy"
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			err = verifyReplayPolicy(data, policy)
			if kind == "served" {
				if err != nil {
					t.Fatalf("valid U2 receipt refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "NOT CHECKED") {
				t.Fatalf("%s receipt accepted or refusal lost: %v", kind, err)
			}
		})
	}
}

func TestVerifyReport_ModernDigestSchemesCannotDowngrade(t *testing.T) {
	for _, kind := range []string{"policy", "corrections", "mixed_old_manifest", "missing_corrections", "outcome_counts_additions", "outcome_counts_deletions", "outcome_counts_changed_files", "outcome_counts_merged_at"} {
		t.Run(kind, func(t *testing.T) {
			f := newVerifyFixture(t)
			m := readFixtureManifest(t, f.manifestPath)
			db, err := store.Open(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			w, err := resolveWindow(m)
			if err != nil {
				t.Fatal(err)
			}
			events, outcomes, err := db.ReportDigestsForScheme(context.Background(), w.Since, w.Until, store.FleetWide, "tierdig1")
			_ = db.Close()
			if err != nil {
				t.Fatal(err)
			}
			m.Results = nil
			switch kind {
			case "policy", "corrections":
				m.EventsDigest, m.OutcomesDigest = &manifestDigest{Value: events.Value, Rows: events.Rows}, &manifestDigest{Value: outcomes.Value, Rows: outcomes.Rows}
				if kind == "policy" {
					m.CorrectionsDigest = nil
				} else {
					m.ScoringPolicy, m.CorrectionRuleVersion, m.RubricVersion = "", "", 0
				}
			case "mixed_old_manifest":
				m.ScoringPolicy, m.CorrectionRuleVersion, m.RubricVersion, m.CorrectionsDigest = "", "", 0, nil
				m.EventsDigest = &manifestDigest{Value: events.Value, Rows: events.Rows}
			case "missing_corrections":
				m.CorrectionsDigest = nil
			default:
				// Retain only outcome_counts as evidence of a U3 emitter.
				m.ScoringPolicy, m.CorrectionRuleVersion, m.RubricVersion, m.CorrectionsDigest = "", "", 0, nil
				if m.OutcomeCounts == nil {
					t.Fatal("fixture carries no outcome_counts")
				}
				m.EventsDigest, m.OutcomesDigest = &manifestDigest{Value: events.Value, Rows: events.Rows}, &manifestDigest{Value: outcomes.Value, Rows: outcomes.Rows}
			}
			writeManifest(t, f.manifestPath, m)
			// These columns are outside tierdig1, but part of U3's tierdig2 coverage.
			// Only the downgrade cases are edited: valid tierdig2 must still detect edits.
			if kind == "policy" || kind == "corrections" {
				raw, err := sql.Open("sqlite", f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`UPDATE outcomes SET additions=99,merged_at='2026-08-10'`); err != nil {
					t.Fatal(err)
				}
				_ = raw.Close()
			}
			switch kind {
			case "outcome_counts_additions":
				execOnFixture(t, f.dbPath, `UPDATE outcomes SET additions=additions+99`)
			case "outcome_counts_deletions":
				execOnFixture(t, f.dbPath, `UPDATE outcomes SET deletions=deletions+99`)
			case "outcome_counts_changed_files":
				execOnFixture(t, f.dbPath, `UPDATE outcomes SET changed_files=changed_files+99`)
			case "outcome_counts_merged_at":
				execOnFixture(t, f.dbPath, `UPDATE outcomes SET merged_at='2026-08-10'`)
			}
			code, out, errout := runVerify(t, f.manifestPath, "--db", f.dbPath)
			if code != rcCannotCheck || strings.Contains(out, "REPRODUCED") {
				t.Fatalf("downgrade accepted: rc=%d\n%s\n%s", code, out, errout)
			}
			name := "events_digest"
			if kind == "missing_corrections" {
				name = "corrections_digest"
			}
			assertDimStatus(t, out, name, "NOT CHECKED")
			if strings.HasPrefix(kind, "outcome_counts_") {
				assertDimStatus(t, out, "outcomes_digest", "NOT CHECKED")
				assertDimStatus(t, out, "outcome_counts", "UNCHANGED")
			}
		})
	}
}

func TestVerifyReport_EachU3PinRejectsLegacyWindowDigests(t *testing.T) {
	f := newVerifyFixture(t)
	m := readFixtureManifest(t, f.manifestPath)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	w, err := resolveWindow(m)
	if err != nil {
		t.Fatal(err)
	}
	events, outcomes, err := db.ReportDigestsForScheme(t.Context(), w.Since, w.Until, store.FleetWide, "tierdig1")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"legacy", "scoring_policy", "correction_rule_version", "rubric_version", "outcome_counts", "corrections_digest"} {
		t.Run(kind, func(t *testing.T) {
			pin := reportManifest{
				EventsDigest:   &manifestDigest{Value: events.Value, Rows: events.Rows},
				OutcomesDigest: &manifestDigest{Value: outcomes.Value, Rows: outcomes.Rows},
			}
			switch kind {
			case "scoring_policy":
				pin.ScoringPolicy = m.ScoringPolicy
			case "correction_rule_version":
				pin.CorrectionRuleVersion = m.CorrectionRuleVersion
			case "rubric_version":
				pin.RubricVersion = m.RubricVersion
			case "outcome_counts":
				pin.OutcomeCounts = m.OutcomeCounts
			case "corrections_digest":
				pin.CorrectionsDigest = m.CorrectionsDigest
			}
			dims, err := verifyDimsDigests(t.Context(), db, pin, w, "", false)
			if err != nil {
				t.Fatal(err)
			}
			want := dimNotCheckable
			if kind == "legacy" {
				want = dimUnchanged
			}
			if len(dims) != 2 {
				t.Fatalf("missing window digest dimensions: %+v", dims)
			}
			for _, dim := range dims {
				if dim.Status != want {
					t.Fatalf("%s alone: %s = %v, want %v", kind, dim.Name, dim.Status, want)
				}
			}
		})
	}
}
