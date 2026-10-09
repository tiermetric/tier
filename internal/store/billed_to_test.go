package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestOpen_AddsBilledToToExistingDatabase pins the #854 migration: a database
// stored by the release before billed_to existed gains the nullable column on
// Open, keeps its rows (reading back undeclared), accepts a declared row, and a
// second Open is a no-op.
func TestOpen_AddsBilledToToExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre854.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()

	// Reshape the file to the pre-#854 token_events and store a row in it.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE token_events DROP COLUMN billed_to`); err != nil {
		t.Fatalf("drop billed_to (the column this test expects Open to add): %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity, ts)
		VALUES ('alice', 'issue-1', 'claude-sonnet-4', 5000000, 'api', 'estimated', ?)`,
		time.Now().UTC()); err != nil {
		t.Fatalf("seed pre-#854 row: %v", err)
	}
	_ = raw.Close()

	for i := 1; i <= 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d on a pre-#854 database: %v", i, err)
		}
		if i == 1 {
			if err := db.InsertManualCostEvent(ctx, TokenEvent{
				Developer: "bob", IssueID: "issue-2", Model: "claude-sonnet-4",
				CostMicro: 1_000_000, Source: "api", Fidelity: "estimated",
				BilledTo: "other", Timestamp: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("insert declared row after migration: %v", err)
			}
		}
		got := map[string]sql.NullString{}
		rows, err := db.db.QueryContext(ctx, `SELECT developer, billed_to FROM token_events`)
		if err != nil {
			t.Fatalf("Open #%d: select billed_to: %v", i, err)
		}
		for rows.Next() {
			var dev string
			var b sql.NullString
			if err := rows.Scan(&dev, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[dev] = b
		}
		_ = rows.Close()
		if b, ok := got["alice"]; !ok || b.Valid {
			t.Errorf("Open #%d: pre-#854 row billed_to = %+v (present %v), want NULL", i, b, ok)
		}
		if b := got["bob"]; !b.Valid || b.String != "other" {
			t.Errorf("Open #%d: declared row billed_to = %+v, want \"other\"", i, b)
		}
		_ = db.Close()
	}
}

// TestManualCostEvent_BilledToStoredOutsideIdentity pins the store half of the
// #854 contract: billed_to is stored, reaches the DSAR export, and a keyed
// re-post that differs only in billed_to is an idempotent no-op, not a conflict.
func TestManualCostEvent_BilledToStoredOutsideIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ev := TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
		CostMicro: 2_000_000, Source: "api", Fidelity: "estimated",
		IdempotencyKey: "k-1", BilledTo: "other", Timestamp: time.Now().UTC(),
	}
	if err := db.InsertManualCostEvent(ctx, ev); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ev.BilledTo = ""
	if err := db.InsertManualCostEvent(ctx, ev); err != nil {
		t.Fatalf("re-post differing only in billed_to: %v, want nil (billed_to is not identity)", err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	if len(exp.TokenEvents) != 1 {
		t.Fatalf("export has %d token rows, want 1", len(exp.TokenEvents))
	}
	if b := exp.TokenEvents[0].BilledTo; b == nil || *b != "other" {
		t.Errorf("DSAR billed_to = %v, want \"other\" (the first writer's declaration, unchanged)", b)
	}
}

// TestUndeclaredManualCostsOnPolledDays pins the #854 startup count: a poller
// row stored at D+1 00:00 UTC (the exclusive end of its bucket) covers day D, so
// undeclared non-zero api rows in [D, D+1) of that provider count — including
// one at exactly D 00:00 and one stored under a dated model id — and a row at
// exactly D+1 00:00 or later on D+1 does not. Several poller rows on one day
// count it once; every covered day adds. Declared, $0, other providers' and
// captured rows never count.
func TestUndeclaredManualCostsOnPolledDays(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	next := day.Add(24 * time.Hour)
	ins := func(source, model, billedTo string, ts time.Time, micro int64) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: model, InputTok: 10,
			CostMicro: micro, Source: source, Fidelity: "estimated",
			BilledTo: billedTo, Timestamp: ts,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins("anthropic-admin", "claude-sonnet-4", "", next, 1)              // covers day D
	ins("api", "claude-sonnet-4", "", day, 5_000_000)                   // counted: D 00:00
	ins("api", "claude-sonnet-4", "", day.Add(23*time.Hour), 2_000_000) // counted
	ins("api", "claude-sonnet-4", "", next, 7_000_000)                  // D+1 00:00: not covered
	ins("api", "claude-sonnet-4", "", next.Add(10*time.Hour), 7_000_000)
	ins("api", "claude-sonnet-4", "other", day.Add(time.Hour), 11_000_000) // declared
	ins("api", "gpt-4o", "", day.Add(time.Hour), 13_000_000)               // other provider
	ins("jsonl", "claude-sonnet-4", "", day.Add(time.Hour), 17_000_000)    // captured
	// The stored model is the raw request string: a dated id counts only through
	// the same normalisation the poller applies.
	ins("api", "claude-sonnet-4-20250514", "", day.Add(2*time.Hour), 3_000_000)
	// A row the audited override corrected to $0 no longer adds to spend.
	ins("api", "claude-sonnet-4", "", day.Add(5*time.Hour), 0)
	// Two poller rows on one covered day count that day once; a second covered
	// day (D+2, from a row at D+3 00:00) adds its own undeclared rows.
	ins("anthropic-admin", "claude-sonnet-4-5", "", next, 1)
	ins("anthropic-admin", "claude-sonnet-4", "", day.Add(72*time.Hour), 1)
	ins("api", "claude-sonnet-4", "", day.Add(50*time.Hour), 4_000_000)

	n, micro, err := db.UndeclaredManualCostsOnPolledDays(ctx, "anthropic", "anthropic-admin")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 || micro != 14_000_000 {
		t.Errorf("anthropic: got %d rows / %d micro, want 4 / 14000000 (three non-zero D rows, one dated, plus the D+2 row)", n, micro)
	}

	// The other direction: a poller that covered D+1 counts the D+1 rows only.
	ins("openai-usage", "gpt-4o", "", next.Add(24*time.Hour), 1)
	ins("api", "gpt-4o", "", next.Add(time.Hour), 19_000_000)
	n, micro, err = db.UndeclaredManualCostsOnPolledDays(ctx, "openai", "openai-usage")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 || micro != 19_000_000 {
		t.Errorf("openai: got %d rows / %d micro, want 1 / 19000000 (the D row is not on a covered day)", n, micro)
	}

	n, micro, err = db.UndeclaredManualCostsOnPolledDays(ctx, "anthropic", "no-such-poller")
	if err != nil || n != 0 || micro != 0 {
		t.Errorf("no poller rows: got %d / %d / %v, want 0 / 0 / nil", n, micro, err)
	}
}
