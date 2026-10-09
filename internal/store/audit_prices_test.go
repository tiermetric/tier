package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// S02-2: race two retirement updates and insertion of a newer retired identity
// on independent connections. Only the start is synchronized; SQLite decides
// the order, without confirmation callbacks or triggers. Repeat on one database
// to exercise both the conditional update and identity-specific readback.
func TestAuditS02_2_PriceForgetConcurrentRetireAndUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contention.db")
	pinEmbeddedPriceTable(t)
	ctx := context.Background()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := openRegistryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	for iteration := range 64 {
		t.Run(fmt.Sprintf("round_%02d", iteration), func(t *testing.T) {
			original := ActivePriceTableInfo()
			original.Version += 10000 + iteration
			if err := recordPriceTableIdentity(raw, path, original); err != nil {
				t.Fatal(err)
			}
			confirmed, found := readRegistryRow(t, path, original.Version)
			if !found {
				t.Fatal("original identity missing")
			}
			replacement := original
			replacement.TableHash += "-replacement"
			type result struct {
				row   PriceTableRegistryRow
				actor string
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			updated := make(chan error, 1)
			for _, actor := range []string{"operator-a", "operator-b"} {
				go func() {
					<-start
					row, err := ForgetPriceTableVersion(ctx, path, original.Version, true, actor, nil)
					results <- result{row: row, actor: actor, err: err}
				}()
			}
			go func() {
				<-start
				// Model a later retirement of this reusable version, as in the
				// trigger test below, but let this write contend naturally.
				_, err := raw.ExecContext(ctx, `INSERT INTO price_table_registry
					(version, table_hash, file_hash, effective_date, model_count, source,
					 tool_version, first_seen, forgotten_at, forgotten_by)
					SELECT version, ?, file_hash, effective_date, model_count, source,
					       tool_version, first_seen, '2099-01-01 00:00:00', 'replacement'
					FROM price_table_registry WHERE id = ?`, replacement.TableHash, confirmed.id)
				updated <- err
			}()
			close(start)
			// Join every goroutine before assertions or starting the next round.
			outcomes := []result{<-results, <-results}
			if err := <-updated; err != nil {
				t.Fatal(err)
			}
			winners := 0
			var winner result
			for _, outcome := range outcomes {
				if outcome.err != nil {
					if !errors.Is(outcome.err, ErrNoPriceTableRegistryRow) {
						t.Errorf("%s: unexpected retirement error: %v", outcome.actor, outcome.err)
					}
					continue
				}
				winners++
				winner = outcome
				if outcome.row.id != confirmed.id || outcome.row.TableHash != original.TableHash ||
					!outcome.row.Forgotten() || outcome.row.ForgottenBy != outcome.actor {
					t.Errorf("%s returned a different retirement: %+v", outcome.actor, outcome.row)
				}
			}
			if winners != 1 {
				t.Fatalf("retirement winners = %d, want exactly 1: %+v", winners, outcomes)
			}
			audit, err := ListPriceForgetAudit(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			entries := 0
			for _, entry := range audit {
				if entry.Version == original.Version {
					entries++
					if entry.TableHash != original.TableHash || entry.Actor != winner.actor {
						t.Errorf("audit does not match winning retirement: %+v", entry)
					}
				}
			}
			if entries != 1 {
				t.Errorf("audit entries = %d, want exactly 1", entries)
			}
			if err := recordPriceTableIdentity(raw, path, replacement); err != nil {
				t.Fatal(err)
			}
			live, found := readRegistryRow(t, path, original.Version)
			if !found || live.TableHash != replacement.TableHash || live.Forgotten() || live.id == confirmed.id {
				t.Errorf("replacement is no longer live: found=%v row=%+v", found, live)
			}
		})
	}
}

// S02-2: interleave three operator actions at the confirmation boundary:
// a stale retirement, another retirement, and registration of a replacement.
func TestAuditS02_2_ForgetDoesNotRetireReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retire.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()
	const version = 4250
	first := writePriceTable(t, dir, "first.yaml", version, 3)
	second := writePriceTable(t, dir, "second.yaml", version, 4)
	info, err := LoadPriceTable(first)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	var replacement PriceTableInfo
	_, err = ForgetPriceTableVersion(ctx, path, version, true, "stale", func(row PriceTableRegistryRow) error {
		if row.TableHash != info.TableHash {
			t.Fatalf("confirmed hash = %q, want %q", row.TableHash, info.TableHash)
		}
		if _, err := ForgetPriceTableVersion(ctx, path, version, true, "winner", nil); err != nil {
			return err
		}
		var err error
		replacement, err = LoadPriceTable(second)
		if err != nil {
			return err
		}
		db, err := Open(path)
		if err != nil {
			return err
		}
		return db.Close()
	})
	if !errors.Is(err, ErrNoPriceTableRegistryRow) {
		t.Errorf("stale retirement error = %v, want ErrNoPriceTableRegistryRow", err)
	}
	live, found := readRegistryRow(t, path, version)
	if !found || live.TableHash != replacement.TableHash {
		t.Errorf("replacement is no longer live: found=%v row=%+v", found, live)
	}
	audit, err := ListPriceForgetAudit(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Actor != "winner" || audit[0].TableHash != info.TableHash {
		t.Errorf("audit = %+v, want only the winning retirement of the original", audit)
	}
}

// S02-2: seed a newer retired identity at the write boundary, modeling another
// retirement before the old post-commit, version-only readback. The result must
// describe the confirmed row even when it is no longer the newest retirement.
func TestAuditS02_2_ForgetReturnsConfirmedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readback.db")
	restoreDefaultPriceTable(t)
	ctx := context.Background()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	info := ActivePriceTableInfo()
	raw, err := openRegistryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TRIGGER newer_retirement AFTER UPDATE OF forgotten_at ON price_table_registry
		WHEN NEW.forgotten_by = 'operator'
		BEGIN
			INSERT INTO price_table_registry
				(version, table_hash, file_hash, effective_date, model_count, source,
				 tool_version, first_seen, forgotten_at, forgotten_by)
			VALUES (NEW.version, 'later-identity', NEW.file_hash, NEW.effective_date,
				NEW.model_count, NEW.source, NEW.tool_version, NEW.first_seen,
				'2099-01-01 00:00:00', 'later');
		END`)
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	row, err := ForgetPriceTableVersion(ctx, path, info.Version, true, "operator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if row.TableHash != info.TableHash || row.ForgottenBy != "operator" || row.ForgottenAt == "" {
		t.Errorf("returned another retirement: %+v", row)
	}
}
