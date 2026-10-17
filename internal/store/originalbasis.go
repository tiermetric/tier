package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// OriginalBasisEvidence is an explicit operator declaration, not a selection of
// today's table. Rule must name a retained implementation. The table must match
// the unique registry identity of this exact original version; reused versions
// and missing identities fail closed. Dry-run recovery is rolled back in full.
type OriginalBasisEvidence struct {
	Table []byte
	Rule  string
}

func recoverOriginalBasis(ctx context.Context, tx *sql.Tx, version int, evidence OriginalBasisEvidence, apply bool) error {
	if evidence.Rule != PricingRuleV1 {
		return fmt.Errorf("original basis requires the retained rule %q", PricingRuleV1)
	}
	table, info, err := parsePriceTable(evidence.Table)
	if err != nil {
		return err
	}
	if info.Version != version {
		return fmt.Errorf("original table version %d does not equal exact selected version %d", info.Version, version)
	}
	var n int
	var hash string
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(table_hash),'') FROM price_table_registry WHERE version=?`, version).Scan(&n, &hash); err != nil {
		return err
	}
	if n != 1 || hash != info.TableHash {
		return fmt.Errorf("basis_unavailable: original table does not resolve to one matching registry identity for version %d", version)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM token_events WHERE price_version=? AND rate_snapshot IS NULL AND COALESCE(pricing_basis,'')<> 'caller_authoritative'
 AND source IN ('jsonl','proxy','codex-rollout','opencode','muse')
 AND NOT EXISTS (SELECT 1 FROM cost_correction_audit a WHERE a.token_event_id=token_events.id)`, version)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		old, _, _, err := readPricingRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.CostMicro != 0 && tokenVector(old) == [5]int{} {
			continue
		}
		protected, err := pricingProtected(ctx, tx, id)
		if err != nil {
			return err
		}
		if protected {
			continue
		}
		key := NormalizeModel(old.Model)
		// An upstream model string cannot select a serving host. Match the
		// retained request lookup rule, including its host-separator guard.
		if strings.Contains(key, hostKeySep) {
			continue
		}
		if normalizeHost(old.Host) != HostUnknown {
			if _, ok := table[hostQualifiedKey(key, normalizeHost(old.Host))]; ok {
				key = hostQualifiedKey(key, normalizeHost(old.Host))
			}
		}
		p, ok := resolveTableKey(table, key)
		if !ok || p.billingMode != old.BillingMode {
			continue
		} // no model guess, no current fallback
		snapshot, err := saveRateSnapshot(ctx, tx, rateSnapshot{Rule: evidence.Rule, TableHash: info.TableHash, Version: version, Rate: snapshotPrice(p)})
		if err != nil {
			return err
		}
		next := old
		next.RateSnapshot = snapshot
		next.PricingBasis = "token_derived"
		if apply {
			if err := pricingAudit(ctx, tx, old, next, "original_basis_recovery"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE token_events SET rate_snapshot=?,pricing_basis='token_derived' WHERE id=?`, snapshot, id); err != nil {
			return err
		}
	}
	return nil
}
