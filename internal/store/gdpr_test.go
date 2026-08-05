package store

import (
	"context"
	"testing"
	"time"
)

// seedDeveloperPII inserts exactly one row into every developer-PII table for
// dev (#184), so an erase/export can be asserted table-by-table. Values are
// dev-scoped (source_ref, issue) so two developers' fixtures never collide on a
// unique index (quality_events is unique on outcome_id+event_type+source_ref).
func seedDeveloperPII(t *testing.T, db *DB, dev string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity, ts) VALUES (?,?,?,?,?,?,?)`,
			[]any{dev, "issue-" + dev, "claude-sonnet-4", int64(1000), "jsonl", "realtime", now}},
		{`INSERT INTO outcomes (developer, issue_id, weight, quality, ts) VALUES (?,?,?,?,?)`,
			[]any{dev, "issue-" + dev, 1.0, 1.0, now}},
		{`INSERT INTO actual_spend (developer, period, actual_paid_micro, ts) VALUES (?,?,?,?)`,
			[]any{dev, "2026-05", int64(2000), now}},
		{`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?,?,?,?)`,
			[]any{dev, "platform", "eng", "acme"}},
		{`INSERT INTO period_membership (developer, org, period_start, period_end) VALUES (?,?,?,?)`,
			[]any{dev, "acme", "2026-01", nil}},
		{`INSERT INTO quality_events (outcome_id, developer, issue_id, event_type, source_ref, event_ts) VALUES (?,?,?,?,?,?)`,
			[]any{int64(1), dev, "issue-" + dev, "ci_pass", dev + ":sha1", now}},
		{`INSERT INTO quality_history (outcome_id, developer, issue_id, old_quality, new_quality, reason) VALUES (?,?,?,?,?,?)`,
			[]any{int64(1), dev, "issue-" + dev, 1.0, 0.5, "ci_fail"}},
		// #493: repo_repair_audit records which repositories a `tierd repair-repo`
		// run moved this developer's stored spend into. NOTE this seed list is a
		// PARALLEL REGISTRY to developerPIITables — a table added there without a
		// row here fails TestEraseDeveloper_RemovesAllPIITables loudly, which is
		// how the omission is meant to be caught. Keep them in step.
		{`INSERT INTO repo_repair_audit (repair_id, developer, from_repo, to_repo, row_count, cost_micro_sum, tool_version) VALUES (?,?,?,?,?,?,?)`,
			[]any{"repair-" + dev, dev, "unqualified", "acme/" + dev, int64(1), int64(1000), "tierd-test"}},
	}
	for _, s := range stmts {
		if _, err := db.db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed %s for %s: %v", s.sql, dev, err)
		}
	}
}

// countDeveloperRows returns how many rows in table carry developer == dev.
func countDeveloperRows(t *testing.T, db *DB, table, dev string) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE developer = ?", dev).Scan(&n); err != nil {
		t.Fatalf("count %s(%s): %v", table, dev, err)
	}
	return n
}

func TestEraseDeveloper_RemovesAllPIITables(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedDeveloperPII(t, db, "alice")
	seedDeveloperPII(t, db, "bob") // control: must survive
	if err := db.UpsertDeveloperAlias(ctx, "alice-alt", "alice"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	counts, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}

	// Every developer-PII table must be zero for alice, and each reported count
	// must equal the single seeded row.
	for _, table := range developerPIITables {
		if got := countDeveloperRows(t, db, table, "alice"); got != 0 {
			t.Errorf("%s: alice rows after erase = %d, want 0", table, got)
		}
		if counts[table] != 1 {
			t.Errorf("counts[%s] = %d, want 1", table, counts[table])
		}
		// Control developer untouched.
		if got := countDeveloperRows(t, db, table, "bob"); got != 1 {
			t.Errorf("%s: bob rows after erase = %d, want 1 (must not touch other developers)", table, got)
		}
	}
	if counts["developer_alias"] != 1 {
		t.Errorf("counts[developer_alias] = %d, want 1", counts["developer_alias"])
	}

	// The alias row must be gone.
	aliases, err := db.DeveloperAliases(ctx)
	if err != nil {
		t.Fatalf("DeveloperAliases: %v", err)
	}
	if _, ok := aliases["alice-alt"]; ok {
		t.Errorf("developer_alias still maps alice-alt after erase")
	}
}

func TestEraseDeveloper_Idempotent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedDeveloperPII(t, db, "alice")

	first, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("first EraseDeveloper: %v", err)
	}
	var firstTotal int64
	for _, n := range first {
		firstTotal += n
	}
	if firstTotal == 0 {
		t.Fatalf("first erase deleted nothing, expected >0")
	}

	// Second erase must be a no-op: all-zero counts, no error.
	second, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("second EraseDeveloper: %v", err)
	}
	for table, n := range second {
		if n != 0 {
			t.Errorf("second erase counts[%s] = %d, want 0 (idempotent)", table, n)
		}
	}
}

func TestEraseDeveloper_NonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	counts, err := db.EraseDeveloper(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("EraseDeveloper(ghost): %v", err)
	}
	for table, n := range counts {
		if n != 0 {
			t.Errorf("counts[%s] = %d, want 0 for a never-seen developer", table, n)
		}
	}
}

func TestEraseDeveloper_AliasGraph(t *testing.T) {
	// Data is stored under BOTH the canonical id and a raw login that aliases to
	// it. Erasing by EITHER id must remove all of it.
	cases := []struct{ name, eraseBy string }{
		{"by-canonical", "alice"},
		{"by-raw-alias", "alice-gh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()

			seedDeveloperPII(t, db, "alice")    // canonical
			seedDeveloperPII(t, db, "alice-gh") // raw login
			if err := db.UpsertDeveloperAlias(ctx, "alice-gh", "alice"); err != nil {
				t.Fatalf("UpsertDeveloperAlias: %v", err)
			}

			if _, err := db.EraseDeveloper(ctx, tc.eraseBy); err != nil {
				t.Fatalf("EraseDeveloper(%s): %v", tc.eraseBy, err)
			}

			for _, table := range developerPIITables {
				if got := countDeveloperRows(t, db, table, "alice"); got != 0 {
					t.Errorf("%s: canonical rows after erase = %d, want 0", table, got)
				}
				if got := countDeveloperRows(t, db, table, "alice-gh"); got != 0 {
					t.Errorf("%s: raw-alias rows after erase = %d, want 0", table, got)
				}
			}
			aliases, err := db.DeveloperAliases(ctx)
			if err != nil {
				t.Fatalf("DeveloperAliases: %v", err)
			}
			if len(aliases) != 0 {
				t.Errorf("developer_alias not fully cleared: %v", aliases)
			}
		})
	}
}

func TestExportDeveloper_ReturnsAllTables(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedDeveloperPII(t, db, "alice")
	seedDeveloperPII(t, db, "alice-gh")
	seedDeveloperPII(t, db, "bob") // must NOT appear in alice's export
	if err := db.UpsertDeveloperAlias(ctx, "alice-gh", "alice"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}

	if exp.Developer != "alice" {
		t.Errorf("Developer = %q, want alice", exp.Developer)
	}
	// Identifier set = {alice, alice-gh}, sorted.
	if len(exp.Identifiers) != 2 || exp.Identifiers[0] != "alice" || exp.Identifiers[1] != "alice-gh" {
		t.Errorf("Identifiers = %v, want [alice alice-gh]", exp.Identifiers)
	}

	// Two developer identifiers seeded (canonical + alias) → 2 rows per
	// developer-keyed table; 1 alias row.
	if len(exp.TokenEvents) != 2 {
		t.Errorf("TokenEvents = %d, want 2", len(exp.TokenEvents))
	}
	if len(exp.Outcomes) != 2 {
		t.Errorf("Outcomes = %d, want 2", len(exp.Outcomes))
	}
	if len(exp.ActualSpend) != 2 {
		t.Errorf("ActualSpend = %d, want 2", len(exp.ActualSpend))
	}
	if len(exp.OrgHierarchy) != 2 {
		t.Errorf("OrgHierarchy = %d, want 2", len(exp.OrgHierarchy))
	}
	if len(exp.PeriodMembership) != 2 {
		t.Errorf("PeriodMembership = %d, want 2", len(exp.PeriodMembership))
	}
	if len(exp.QualityEvents) != 2 {
		t.Errorf("QualityEvents = %d, want 2", len(exp.QualityEvents))
	}
	if len(exp.QualityHistory) != 2 {
		t.Errorf("QualityHistory = %d, want 2", len(exp.QualityHistory))
	}
	if len(exp.DeveloperAlias) != 1 {
		t.Errorf("DeveloperAlias = %d, want 1", len(exp.DeveloperAlias))
	}

	// No bob PII must leak into alice's export.
	for _, te := range exp.TokenEvents {
		if te.Developer == "bob" {
			t.Errorf("bob token_event leaked into alice export")
		}
	}
}

func TestExportDeveloper_ByRawEqualsByCanonical(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedDeveloperPII(t, db, "alice")
	seedDeveloperPII(t, db, "alice-gh")
	if err := db.UpsertDeveloperAlias(ctx, "alice-gh", "alice"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	byCanon, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper(alice): %v", err)
	}
	byRaw, err := db.ExportDeveloper(ctx, "alice-gh")
	if err != nil {
		t.Fatalf("ExportDeveloper(alice-gh): %v", err)
	}
	if byCanon.RowCount() != byRaw.RowCount() {
		t.Errorf("RowCount by canonical (%d) != by raw (%d)", byCanon.RowCount(), byRaw.RowCount())
	}
	if byCanon.Developer != byRaw.Developer {
		t.Errorf("Developer by canonical (%q) != by raw (%q)", byCanon.Developer, byRaw.Developer)
	}
}

func TestExportDeveloper_NonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	exp, err := db.ExportDeveloper(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("ExportDeveloper(ghost): %v", err)
	}
	if exp.RowCount() != 0 {
		t.Errorf("RowCount = %d, want 0 for a never-seen developer", exp.RowCount())
	}
	if exp.Developer != "ghost" {
		t.Errorf("Developer = %q, want ghost", exp.Developer)
	}
}

// TestExportDeveloper_IncludesWorkType pins that the GDPR Art. 15 export (#184)
// carries the #187 work_type + work_type_source on every outcome row — a column
// added to the table must not silently drop out of the data-subject export.
func TestExportDeveloper_IncludesWorkType(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-sec", Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-sec", WorkType: WorkTypeSecurity, WorkTypeSource: WorkTypeSourceLabel,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	if len(exp.Outcomes) != 1 {
		t.Fatalf("Outcomes = %d, want 1", len(exp.Outcomes))
	}
	if exp.Outcomes[0].WorkType != WorkTypeSecurity || exp.Outcomes[0].WorkTypeSource != WorkTypeSourceLabel {
		t.Errorf("export work_type = (%q,%q), want (security,label)",
			exp.Outcomes[0].WorkType, exp.Outcomes[0].WorkTypeSource)
	}
}
