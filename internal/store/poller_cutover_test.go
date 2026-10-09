package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPollerBaselineCutoverMigrationAndReplay(t *testing.T) {
	for _, source := range []string{"anthropic-admin", "openai-usage"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			// Emulate origin/main's schema and an old-rule row before upgrade.
			if _, err := db.db.Exec(`ALTER TABLE token_events DROP COLUMN poller_baseline`); err != nil {
				t.Fatal(err)
			}
			day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
			old := TokenEvent{Developer: "unattributed", IssueID: "unattributed", Model: "gpt-4o",
				InputTok: 10, OutputTok: 20, CacheRead: 30, CacheWrite5m: 40, CacheWrite1h: 50,
				CostMicro: 123, Source: source, Fidelity: "daily", IdempotencyKey: "old", Timestamp: day.Add(24 * time.Hour)}
			res, err := db.db.ExecContext(ctx, `INSERT INTO token_events
				(developer, issue_id, model, input_tok, output_tok, cache_read, cache_write_5m,
				 cache_write_1h, cost_micro, source, fidelity, idempotency_key, ts)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, old.Developer, old.IssueID, old.Model,
				old.InputTok, old.OutputTok, old.CacheRead, old.CacheWrite5m, old.CacheWrite1h,
				old.CostMicro, old.Source, old.Fidelity, old.IdempotencyKey, old.Timestamp)
			if err != nil {
				t.Fatal(err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			before := rowSnapshot(t, db, id)
			var baseline sql.NullString
			if err := db.db.QueryRowContext(ctx, `SELECT poller_baseline FROM token_events WHERE id = ?`, id).Scan(&baseline); err != nil {
				t.Fatal(err)
			}
			if baseline.Valid {
				t.Fatalf("upgrade backfilled an old-rule row with %q", baseline.String)
			}
			changed := old
			changed.InputTok += 100
			changed.OutputTok += 100
			changed.CacheRead += 100
			changed.CacheWrite5m += 100
			changed.CacheWrite1h += 100
			changed.CostMicro += 100
			if err := db.InsertTokenEvent(ctx, changed); err != nil {
				t.Fatal(err)
			}
			if err := db.InsertTokenEvents(ctx, []TokenEvent{changed}); err != nil {
				t.Fatal(err)
			}
			if after := rowSnapshot(t, db, id); !reflect.DeepEqual(before, after) {
				t.Errorf("poller conflict partially updated history: before %v, after %v", before, after)
			}
			fresh := old
			fresh.IdempotencyKey = "new"
			fresh.Timestamp = old.Timestamp.Add(24 * time.Hour)
			proxy := old
			proxy.IdempotencyKey = "proxy"
			proxy.Source = "proxy"
			if err := db.InsertTokenEvents(ctx, []TokenEvent{fresh, proxy}); err != nil {
				t.Fatal(err)
			}
			// Non-poller count MAX semantics are preserved.
			proxy.InputTok = 100
			if err := db.InsertTokenEvent(ctx, proxy); err != nil {
				t.Fatal(err)
			}
			var input int
			if err := db.db.QueryRow(`SELECT input_tok FROM token_events WHERE idempotency_key = 'proxy'`).Scan(&input); err != nil {
				t.Fatal(err)
			}
			if input != 100 {
				t.Errorf("capture count MAX = %d, want 100", input)
			}
			exp, err := db.ExportDeveloper(ctx, old.Developer)
			if err != nil {
				t.Fatal(err)
			}
			if len(exp.TokenEvents) != 3 {
				t.Fatalf("exported %d rows, want 3", len(exp.TokenEvents))
			}
			for _, row := range exp.TokenEvents {
				if *row.IdempotencyKey == "new" {
					if row.PollerBaseline == nil || *row.PollerBaseline != "per-token-host-v1" {
						t.Errorf("new-row marker = %v", row.PollerBaseline)
					}
				} else if row.PollerBaseline != nil {
					t.Errorf("old/non-poller marker = %v, want NULL", row.PollerBaseline)
				}
			}
			// Reopening never restamps legacy rows or loses new-day provenance.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if after := rowSnapshot(t, db, id); !reflect.DeepEqual(before, after) {
				t.Error("reopen changed old-rule row")
			}
		})
	}
}

func TestPollerModelsByDayBoundariesAndSource(t *testing.T) {
	for _, stamp := range []time.Duration{24 * time.Hour, 24*time.Hour - time.Second} {
		t.Run(stamp.String(), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
			if err := db.InsertTokenEvent(ctx, TokenEvent{Developer: "unattributed", IssueID: "unattributed",
				Model: "gpt-4o-2024-08-06", CostMicro: 1, Source: "openai-usage", Timestamp: day.Add(stamp)}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				day    time.Time
				source string
				want   bool
			}{
				{day, "openai-usage", true},
				{day.Add(time.Hour).In(time.FixedZone("offset", -5*3600)), "openai-usage", true},
				{day.Add(-24 * time.Hour), "openai-usage", false},
				{day.Add(24 * time.Hour), "openai-usage", false},
				{day, "anthropic-admin", false},
			} {
				got, err := db.PollerModelsByDay(ctx, tc.day, tc.source)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{}
				if tc.want {
					want["gpt-4o"] = "per-token-host-v1"
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("PollerModelsByDay(%v, %s) = %v, want %v", tc.day, tc.source, got, want)
				}
			}
		})
	}
}
