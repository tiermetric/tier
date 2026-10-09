package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/tiermetric/tier/internal/metrics"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

func TestTokenEventCostCeilingWriters(t *testing.T) {
	writers := map[string]func(*DB, TokenEvent) error{
		"manual insert": func(db *DB, e TokenEvent) error {
			return db.InsertManualCostEvent(context.Background(), e)
		},
		"fresh-key correction": func(db *DB, e TokenEvent) error {
			_, err := db.CorrectManualCostEvent(context.Background(), e, "operator", "import correction")
			return err
		},
		"manual correction": func(db *DB, e TokenEvent) error {
			if err := db.InsertManualCostEvent(context.Background(), manualCostEvent(e.IdempotencyKey, 1)); err != nil {
				return err
			}
			_, err := db.CorrectManualCostEvent(context.Background(), e, "operator", "import correction")
			return err
		},
		"existing correction": func(db *DB, e TokenEvent) error {
			if err := db.InsertManualCostEvent(context.Background(), manualCostEvent(e.IdempotencyKey, 1)); err != nil {
				return err
			}
			_, err := db.CorrectExistingManualCostEvent(context.Background(), e, "operator", "import correction")
			return err
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			for _, cost := range []int64{-20_000 * MicroPerUSD, MaxTokenEventCostMicro, MaxTokenEventCostMicro + 1} {
				db, cleanup := newTestDB(t)
				err := write(db, manualCostEvent("limit", cost))
				if cost <= MaxTokenEventCostMicro {
					if err != nil {
						t.Fatalf("accepted cost %d: %v", cost, err)
					}
					var got int64
					if err := db.db.QueryRow(`SELECT cost_micro FROM token_events WHERE idempotency_key = 'limit'`).Scan(&got); err != nil || got != cost {
						t.Fatalf("stored = %d, err = %v", got, err)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "10000000000") || !strings.Contains(err.Error(), "split") {
						t.Fatalf("over ceiling: %v", err)
					}
					var bad int
					if err := db.db.QueryRow(`SELECT COUNT(*) FROM token_events WHERE cost_micro > ?`, MaxTokenEventCostMicro).Scan(&bad); err != nil || bad != 0 {
						t.Fatalf("over-ceiling rows = %d, err = %v", bad, err)
					}
				}
				cleanup()
			}
		})
	}
}

func TestTokenEventCostCeilingRepricers(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("20,000,000,002 tokens exceed the 32-bit int limit (2,147,483,647) of TokenEvent.InputTok")
	}
	inputTokens := int64(20_000_000_002)
	for _, mode := range []string{"migration", "dry-run", "commit"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := manualCostEvent("limit", 1)
			e.Model, e.Source, e.InputTok, e.OutputTok, e.PriceVersion = "self-hosted-medium", "jsonl", int(inputTokens), 0, 1
			if err := db.InsertTokenEvent(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if mode == "migration" {
				if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = ?`, migrationRecomputeCacheTTL); err != nil {
					t.Fatal(err)
				}
				if err := recomputeKnownSourceCosts(db.db); err != nil {
					t.Fatal(err)
				}
				if got := onlyCostMicro(t, db); got != MaxTokenEventCostMicro+1 {
					t.Fatalf("migration cost = %d", got)
				}
				return
			}
			// Existing over-ceiling data is legal, including an unchanged active-table row.
			if _, err := db.db.Exec(`UPDATE token_events SET cost_micro = ?, price_version = ?`, MaxTokenEventCostMicro+1, ActivePriceTableInfo().Version); err != nil {
				t.Fatal(err)
			}
			res, err := db.Reprice(context.Background(), RepriceOptions{FromVersion: 1, Commit: mode == "commit"})
			if err != nil {
				t.Fatal(err)
			}
			if res.NewCostMicroSum != MaxTokenEventCostMicro {
				t.Fatalf("repriced total = %d", res.NewCostMicroSum)
			}
			want := MaxTokenEventCostMicro
			if mode == "dry-run" {
				want++
			}
			if got := onlyCostMicro(t, db); got != want {
				t.Fatalf("stored = %d, want %d", got, want)
			}
			var marked bool
			if err := db.db.QueryRow(`SELECT COALESCE(cost_clamped, 0) FROM token_events`).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked != (mode == "commit") {
				t.Fatalf("marked = %v", marked)
			}
		})
	}
}

// Pre-existing corrupt/legacy rows still raise the driver's identifiable error.
func TestTokenEventSumOverflowRemainsTyped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for range 2 {
		if _, err := db.db.Exec(`INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity) VALUES ('alice', 'old', 'm', ?, 'api', 'estimated')`, int64(math.MaxInt64)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := db.DeveloperCosts(context.Background(), time.Time{})
	var driverErr *sqlite.Error
	if !errors.As(err, &driverErr) || driverErr.Code() != 1 || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("want typed SQLITE_ERROR integer overflow, got %T: %v", err, err)
	}
}

func TestTokenEventCostCeilingLegacyMigration(t *testing.T) {
	for _, cost := range []float64{10000, 10000.000001} {
		db, cleanup := newTestDB(t)
		if _, err := db.db.Exec(`ALTER TABLE token_events ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`INSERT INTO token_events (developer, issue_id, model, cost_usd, cost_micro, source, fidelity) VALUES ('alice', 'old', 'm', ?, 0, 'api', 'estimated')`, cost); err != nil {
			t.Fatal(err)
		}
		err := migrateCostUSDToMicro(db.db)
		if err != nil {
			t.Fatalf("legacy migration: %v", err)
		}
		if got := onlyCostMicro(t, db); got != DollarsToMicro(cost) {
			t.Fatalf("stored = %d", got)
		}

		cleanup()
	}
}

func TestTokenEventCostCeilingComputedOverflow(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("MaxTokenCount (1e12) exceeds the 32-bit int limit (2,147,483,647) of CostUsage token fields")
	}
	tokens := int64(MaxTokenCount)
	db, cleanup := newTestDB(t)
	defer cleanup()
	// priceCall saturates the computed value before the capture ceiling applies.
	p := modelPrice{inputPerM: MaxCostUSD, outputPerM: MaxCostUSD, combined: true}
	cost := priceCall(p, CostUsage{
		Input:        int(tokens),
		Output:       int(tokens),
		CacheRead:    int(tokens),
		CacheWrite5m: int(tokens),
		CacheWrite1h: int(tokens),
	})
	if cost != math.MaxInt64 {
		t.Fatalf("huge tokens × max rate wrapped: cost = %d, want MaxInt64 saturation", cost)
	}
	if err := db.InsertTokenEvent(context.Background(), manualCostEvent("overflow", cost)); err != nil {
		t.Fatal(err)
	}
	var got int64
	var marked bool
	if err := db.db.QueryRow(`SELECT cost_micro, cost_clamped FROM token_events`).Scan(&got, &marked); err != nil {
		t.Fatal(err)
	}
	if got != MaxTokenEventCostMicro || !marked {
		t.Fatalf("cost = %d, marked = %v", got, marked)
	}
}

func TestTokenEventCaptureBatchClampsAndContinues(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	if err := db.InsertTokenEvents(context.Background(), []TokenEvent{manualCostEvent("huge", MaxTokenEventCostMicro+1), manualCostEvent("normal", 1)}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM token_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stored rows = %d", n)
	}
	for _, tt := range []struct {
		key         string
		wantCost    int64
		wantClamped sql.NullInt64
	}{
		{"huge", MaxTokenEventCostMicro, sql.NullInt64{Int64: 1, Valid: true}},
		{"normal", 1, sql.NullInt64{}},
	} {
		var cost int64
		var clamped sql.NullInt64
		if err := db.db.QueryRow(`SELECT cost_micro, cost_clamped FROM token_events WHERE idempotency_key = ?`, tt.key).Scan(&cost, &clamped); err != nil {
			t.Fatal(err)
		}
		if cost != tt.wantCost || clamped != tt.wantClamped {
			t.Fatalf("%s: cost = %d, marker = %v; want cost = %d, marker = %v", tt.key, cost, clamped, tt.wantCost, tt.wantClamped)
		}
	}
}

func TestOpenExistingOverCeilingData(t *testing.T) {
	for _, mode := range []string{"legacy dollars", "known-source recompute"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if db != nil {
					_ = db.Close()
				}
			}()
			if mode == "legacy dollars" {
				if _, err := db.db.Exec(`ALTER TABLE token_events ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.Exec(`INSERT INTO token_events (developer, issue_id, model, cost_micro, cost_usd, source, fidelity) VALUES ('alice', 'legacy', 'm', 0, 20000, 'api', 'estimated')`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.db.Exec(`INSERT INTO token_events (developer, issue_id, model, input_tok, cost_micro, source, fidelity) VALUES ('alice', 'legacy', 'self-hosted-medium', 40000000000, 20000000000, 'jsonl', 'realtime')`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = ?`, migrationRecomputeCacheTTL); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.db.Exec(`ALTER TABLE token_events DROP COLUMN cost_clamped`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatalf("Open refused existing data: %v", err)
			}
			var cost int64
			var mark sql.NullBool
			if err := db.db.QueryRow(`SELECT cost_micro, cost_clamped FROM token_events`).Scan(&cost, &mark); err != nil {
				t.Fatal(err)
			}
			if cost != 20_000*MicroPerUSD || mark.Valid {
				t.Fatalf("existing cost = %d, marker = %v", cost, mark)
			}
		})
	}
}

func TestRepriceCeilingMarkerOnlyAndCounter(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	reg := metrics.NewRegistry()
	SetCostClampRecorder(reg.NewCounter("clamps", "test"))
	defer SetCostClampRecorder(nil)
	if _, err := db.db.Exec(`INSERT INTO token_events (developer, issue_id, model, input_tok, cost_micro, source, fidelity, price_version, billing_mode) VALUES ('alice', 'limit', 'self-hosted-medium', 20000000002, ?, 'jsonl', 'realtime', ?, 'self_hosted_amortized')`, MaxTokenEventCostMicro, ActivePriceTableInfo().Version); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []bool{false, true} {
		res, err := db.Reprice(context.Background(), RepriceOptions{FromVersion: 1, Commit: commit})
		if err != nil {
			t.Fatal(err)
		}
		if res.ChangedRowCount != 1 || res.NewCostMicroSum != MaxTokenEventCostMicro || res.OldCostMicroSum != MaxTokenEventCostMicro {
			t.Fatalf("marker-only result: %+v", res)
		}
		var mark sql.NullBool
		if err := db.db.QueryRow(`SELECT cost_clamped FROM token_events`).Scan(&mark); err != nil {
			t.Fatal(err)
		}
		if mark.Valid != commit {
			t.Fatalf("marker: %v, commit: %v", mark, commit)
		}
		exp, err := db.ExportDeveloper(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if len(exp.TokenEvents) != 1 {
			t.Fatalf("DSAR rows = %d", len(exp.TokenEvents))
		}
		exported := exp.TokenEvents[0].CostClamped
		if (exported != nil) != commit || (exported != nil && *exported != 1) {
			t.Fatalf("DSAR clamp marker = %v", exported)
		}
		var out strings.Builder
		reg.Render(&out)
		if !commit && strings.Contains(out.String(), "clamps 1") {
			t.Fatal("dry run bumped counter")
		}
		if commit && !strings.Contains(out.String(), "clamps 1\n") {
			t.Fatalf("counter: %s", out.String())
		}
	}
	res, err := db.Reprice(context.Background(), RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil || res.ChangedRowCount != 0 {
		t.Fatalf("repeat reprice: %+v, %v", res, err)
	}
	// The marker records clamp history and survives later repricing to a normal cost.
	if _, err := db.db.Exec(`UPDATE token_events SET input_tok = 1000`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Reprice(context.Background(), RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
		t.Fatal(err)
	}
	var mark bool
	if err := db.db.QueryRow(`SELECT cost_clamped FROM token_events`).Scan(&mark); err != nil || !mark {
		t.Fatalf("clamp history lost: %v %v", mark, err)
	}
	var out strings.Builder
	reg.Render(&out)
	if !strings.Contains(out.String(), "clamps 1\n") {
		t.Fatalf("counter changed for normal/no-op reprice: %s", out.String())
	}
}
