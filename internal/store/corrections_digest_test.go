package store

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCorrectionDigest_EvidenceAndCrossWindowDependencies(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o, f, relation := projectionFixture(t, db, true)
	ctx := context.Background()
	confirmFixture(t, db, relation)
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	// Report only the original; child and its later review remain dependencies.
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.db.Exec(`UPDATE outcomes SET ts=CASE id WHEN ? THEN ? ELSE ? END`, o.ID, since, since.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	read := func() CorrectionsDigest {
		t.Helper()
		var d CorrectionsDigest
		err := db.ReadSnapshot(ctx, func(s *Snapshot) error {
			var err error
			d, err = s.CorrectionsDigest(ctx, since, since.AddDate(0, 0, 1), RepoScope("acme/app"))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	before := read()
	if before.Counts.Relations != 1 || before.Counts.Allocations != 2 || before.Counts.Assessments < 1 || before.Counts.DependencyOutcomes != 2 {
		t.Fatalf("incomplete dependencies: %+v", before)
	}
	for _, sql := range []string{
		`UPDATE corrective_relations SET appeal_text='disputed',state='disputed'`,
		`UPDATE correction_assessments SET rationale='revised review'`,
		`UPDATE correction_allocations SET allocated_points='123/1' WHERE outcome_id=` + strconv.FormatInt(f.ID, 10),
		`UPDATE correction_declarations SET supporting_text='revised declaration' WHERE outcome_id=` + strconv.FormatInt(f.ID, 10),
		`UPDATE outcomes SET additions=NULL WHERE id=` + strconv.FormatInt(f.ID, 10),
		`UPDATE outcomes SET additions=0 WHERE id=` + strconv.FormatInt(f.ID, 10),
	} {
		if _, err := db.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
		after := read()
		if before.Value == after.Value {
			t.Fatalf("mutation missed: %s", sql)
		}
		before = after
	}
}

func TestCorrectionDigest_ErasedIdentifiersAbsentFromInputs(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, f, _ := projectionFixture(t, db, false)
	if err := db.ReconcileCorrectionChain(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EraseDeveloper(context.Background(), "original"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := db.ReadSnapshot(context.Background(), func(s *Snapshot) error {
		_, err := s.correctionDigestTo(context.Background(), time.Time{}, time.Time{}, FleetWide, &buf)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"original-sha", "original scope", "repair scope"} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("erased input retained %q", s)
		}
	}
}

func TestCorrectionDigest_AuditScopeAndSchemaCoverage(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, f, relation := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	read := func(scope RepoScope) CorrectionsDigest {
		t.Helper()
		d, err := db.CorrectionsDigest(ctx, since, time.Time{}, scope)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	before, other := read(RepoScope("acme/app")), read(RepoScope("other/repo"))
	if _, err := db.db.Exec(`INSERT INTO correction_audit(relation_id,revision,action,actor,actor_role,recorded_at,rule_version,reason_text) VALUES(?,1,'dispute','operator','self-asserted',CURRENT_TIMESTAMP,'event3-v2','appeal')`, relation); err != nil {
		t.Fatal(err)
	}
	after := read(RepoScope("acme/app"))
	if before.Value == after.Value || after.Counts.Audit != before.Counts.Audit+1 {
		t.Fatal("audit omitted")
	}
	if other != read(RepoScope("other/repo")) {
		t.Fatal("unrelated scope moved")
	}
	for _, table := range correctionDigestTables[:5] {
		rows, err := db.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table.table)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, col)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if strings.Join(cols, ",") != table.columns {
			t.Fatalf("%s columns outside digest: %v", table.table, cols)
		}
	}
}

func TestCorrectionWatermarks_ProjectionRevision(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, f, relation := projectionFixture(t, db, false)
	ctx := context.Background()
	read := func() Watermarks {
		t.Helper()
		m, err := db.ReportWatermarks(ctx, time.Time{}, time.Time{}, FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := read()
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if read().Ledgers.CorrectionProjectionRevision <= before.Ledgers.CorrectionProjectionRevision {
		t.Fatal("projection revision omitted")
	}
	if _, err := db.db.Exec(`INSERT INTO correction_audit(relation_id,revision,action,recorded_at,rule_version) VALUES(?,1,'dispute',CURRENT_TIMESTAMP,'event3-v2')`, relation); err != nil {
		t.Fatal(err)
	}
	after := read()
	if after.Ledgers.MaxCorrectionAuditID == 0 || after.Ledgers.CorrectionAuditCount != before.Ledgers.CorrectionAuditCount+1 {
		t.Fatal("audit pins omitted")
	}
	if _, err := db.db.Exec(`DELETE FROM correction_audit`); err != nil {
		t.Fatal(err)
	}
	if read().Ledgers.CorrectionAuditCount != 0 {
		t.Fatal("audit deletion invisible")
	}
}

func TestCorrectionCounts_UsesPinnedPolicyAndRefusesMissingProjection(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, f, _ := projectionFixture(t, db, true)
	read := func(enabled bool) (CorrectionReportCounts, error) {
		var c CorrectionReportCounts
		err := db.ReadSnapshot(context.Background(), func(s *Snapshot) error {
			var err error
			c, err = s.CorrectionReportCounts(context.Background(), time.Time{}, time.Time{}, FleetWide, enabled)
			return err
		})
		return c, err
	}
	if _, err := read(true); err == nil {
		t.Fatal("flag-on missing projection accepted")
	}
	if err := db.ReconcileCorrectionChain(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	on, err := read(true)
	if err != nil {
		t.Fatal(err)
	}
	off, err := read(false)
	if err != nil {
		t.Fatal(err)
	}
	if on.OutcomeN != 2 || on.SampleN != 1 || off.OutcomeN != 2 || off.SampleN != 2 {
		t.Fatalf("on=%+v off=%+v", on, off)
	}
}

func TestCorrectionDigest_GoldenVectors(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	at := time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC)
	db.now = func() time.Time { return at }
	read := func() CorrectionsDigest {
		t.Helper()
		d, err := db.CorrectionsDigest(context.Background(), time.Time{}, time.Time{}, FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	empty := read()
	if empty.Value != "tierdig2:2ae967ea6e5a20de8fbbc08d9c1aba28128c9314218a6dfced1f7022a6b6d0e2" {
		t.Errorf("empty golden: %s", empty.Value)
	}
	_, f, relation := projectionFixture(t, db, false)
	confirmFixture(t, db, relation)
	if err := db.ReconcileCorrectionChain(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	full := read()
	if full.Value != "tierdig2:e74219e28b8c3646fa7898215e7778906206b2244f4f5dd882167742fadd4d47" {
		t.Errorf("full golden: %s counts=%+v", full.Value, full.Counts)
	}
}

func TestDigestLegacy_RetainsOriginalEvidencePopulation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedDigestWindow(t, db)
	read := func(scheme string) Digest {
		t.Helper()
		_, out, err := db.ReportDigestsForScheme(context.Background(), time.Time{}, time.Time{}, FleetWide, scheme)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	old, new := read("tierdig1"), read("tierdig2")
	if _, err := db.db.Exec(`UPDATE outcomes SET additions=0,deletions=0,changed_files=0,merged_at='2026-10-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	if got := read("tierdig1"); got != old {
		t.Fatal("tierdig1 reinterpreted new evidence")
	}
	if got := read("tierdig2"); got == new {
		t.Fatal("tierdig2 omits new evidence")
	}
}

func BenchmarkReportManifestCorrections(b *testing.B) {
	db := benchDigestDB(b, 20000)
	ctx := context.Background()
	// Seed every corrective population, including allocations; a correction-free
	// fixture understates both the graph and typed-row framing cost.
	for _, query := range []string{
		`INSERT INTO correction_declarations(id,outcome_id,developer,repo,pr_number,merge_commit_sha,merged_at,declaration_state,recorded_at,rule_version) SELECT id,id,developer,repo,id,merge_commit_sha,ts,'parents',ts,'event3-v2' FROM outcomes`,
		`INSERT INTO corrective_relations(id,child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,state,revision,recorded_at,updated_at,rule_version) SELECT id,id,id-1,id,id-1,'confirmed',1,ts,ts,'event3-v2' FROM outcomes WHERE id%2=0`,
		`INSERT INTO correction_audit(relation_id,revision,action,recorded_at,rule_version) SELECT id,1,'confirm',recorded_at,'event3-v2' FROM corrective_relations`,
		`INSERT INTO correction_assessments(relation_id,revision,chain_version,participant_outcome_id,basis,original_weight,original_quality,reviewed_own_weight,before_weight,after_weight,marginal_credit,pure_repair,scope,rationale,reviewer,reviewed_at,recorded_at,rule_version) SELECT id,1,id,parent_outcome_id,'reviewed','3/1','1/1','1/1','3/1','3/1','0/1',1,'benchmark scope','benchmark review','operator',recorded_at,recorded_at,'event3-v2' FROM corrective_relations`,
		`INSERT INTO correction_allocations(chain_version,relation_id,outcome_id,allocation_parent_outcome_id,original_weight,original_quality,role,raw_weight,original_term,reviewed_own_weight,before_weight,after_weight,share,projected_quality,allocated_points,marginal_credit,ranking_eligibility,status,pending_reason,provisional,recorded_at,rule_version) SELECT id+id%2,id+id%2,id,CASE WHEN id%2=0 THEN id-1 ELSE NULL END,'3/1','1/1','original','3/1','3/1','1/1','3/1','3/1','1/1','1/1','3/1','0/1','eligible','confirmed','',0,ts,'event3-v2' FROM outcomes`,
	} {
		if _, err := db.db.ExecContext(ctx, query); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := db.CorrectionsDigest(ctx, digestSince, digestUntil, FleetWide)
		if err != nil {
			b.Fatal(err)
		}
		if d.Counts != (CorrectionDigestCounts{Declarations: 2000, Relations: 1000, Audit: 1000, Assessments: 1000, Allocations: 2000, DependencyOutcomes: 2000}) {
			b.Fatalf("incomplete corrective fixture: %+v", d.Counts)
		}
	}
}

func TestCorrectionDigest_AuditNullableRelationKeepsEndpointDependency(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o := correctionOutcome(t, db, "alice", "acme/app", "sha-one", 1)
	ctx := context.Background()
	before, err := db.CorrectionsDigest(ctx, time.Time{}, time.Time{}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO correction_audit(relation_id,child_outcome_id,revision,action,recorded_at,rule_version,reason_text) VALUES(NULL,?,1,'dispute',CURRENT_TIMESTAMP,'event3-v2','late appeal')`, o.ID); err != nil {
		t.Fatal(err)
	}
	after, err := db.CorrectionsDigest(ctx, time.Time{}, time.Time{}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if after.Counts.Audit != before.Counts.Audit+1 || after.Value == before.Value {
		t.Fatal("nullable relation hid endpoint audit")
	}
}

func TestCorrectionDigest_U3WireGolden(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, relation := projectionFixture(t, db, true)
	confirmFixture(t, db, relation)
	if err := db.ReconcileCorrectionChain(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range correctionDigestTables[:5] {
		var sets []string
		for _, col := range strings.Split(table.columns, ",") {
			if strings.HasSuffix(col, "_at") {
				sets = append(sets, col+"='2026-10-03 01:02:03 +0000 UTC'")
			}
		}
		if _, err := db.db.Exec("UPDATE " + table.table + " SET " + strings.Join(sets, ",")); err != nil {
			t.Fatal(err)
		}
	}
	d, err := db.CorrectionsDigest(context.Background(), time.Time{}, time.Time{}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	const want = "tierdig2:2abbd4d97a3c7a5621742191609f251330244714a49849830fd3e0b32b21b120"
	if d.Value != want {
		t.Fatalf("corrective wire moved: got %s want %s counts %+v", d.Value, want, d.Counts)
	}
}
