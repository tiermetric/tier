package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PricingRuleV1 pins priceCall's request-tier, cache, rounding and clamp rules.
// Keep this implementation available when adding another rule; unknown rules fail closed.
const PricingRuleV1 = "request-v1"

// The companion policy marker distinguishes retirement from an old migration
// actually having repriced/stamped rows. Legacy markers also keep older binaries
// from applying those automatic transformations after this policy activates.
const OriginalBasisHistoryPolicy = "original-basis-history-preserved-v1-2026-10-14"

type rateSnapshot struct {
	Rule, TableHash string
	Version         int
	Rate            priceEntry
	Guessed         bool `json:",omitempty"`
	// PreserveCost retains the replay policy for direct store observations whose
	// supplied dollars did not match token pricing. It is not pricing authority:
	// an explicit reprice may still replace those dollars and this snapshot.
	PreserveCost bool `json:",omitempty"`
}

func snapshotPrice(p modelPrice) priceEntry {
	return priceEntry{InputPerM: p.inputPerM, OutputPerM: p.outputPerM, Combined: p.combined, Provider: p.provider,
		CacheReadMult: p.cacheReadMult, CacheWrite5mMult: p.cacheWrite5mMult, CacheWrite1hMult: p.cacheWrite1hMult,
		ContextThreshold: p.contextThreshold, InputPerMOver: p.inputPerMOver, OutputPerMOver: p.outputPerMOver, BillingMode: p.billingMode}
}
func (s rateSnapshot) price() modelPrice {
	p := s.Rate
	return modelPrice{inputPerM: p.InputPerM, outputPerM: p.OutputPerM, combined: p.Combined, provider: p.Provider,
		cacheReadMult: p.CacheReadMult, cacheWrite5mMult: p.CacheWrite5mMult, cacheWrite1hMult: p.CacheWrite1hMult,
		contextThreshold: p.ContextThreshold, inputPerMOver: p.InputPerMOver, outputPerMOver: p.OutputPerMOver, billingMode: p.BillingMode}
}
func usage(e TokenEvent) CostUsage {
	return CostUsage{Input: e.InputTok, Output: e.OutputTok, CacheRead: e.CacheRead, CacheWrite5m: e.CacheWrite5m, CacheWrite1h: e.CacheWrite1h}
}
func tokenVector(e TokenEvent) [5]int {
	return [5]int{e.InputTok, e.OutputTok, e.CacheRead, e.CacheWrite5m, e.CacheWrite1h}
}
func isPoller(s string) bool { return s == "anthropic-admin" || s == "openai-usage" }

// Unique keys lead with the eventual tenant partition; no global version key is added.
// Snapshots contain rates and replay policy only. The observation ledger contains developer data and is
// erased/exported with developerPIITables. No legacy basis is silently backfilled.
func migratePricingBasis(db *sql.DB) error {
	for _, col := range []string{"old_pricing_basis", "old_rate_snapshot"} {
		if err := addColumnIfMissing(db, "reprice_row_audit", col, "TEXT"); err != nil {
			return err
		}
	}
	for _, col := range []string{"pricing_basis", "rate_snapshot", "cache_write_evidence"} {
		if err := addColumnIfMissing(db, "token_events", col, "TEXT"); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS pricing_rate_snapshots (
 id TEXT PRIMARY KEY, body TEXT NOT NULL);
 CREATE TRIGGER IF NOT EXISTS pricing_snapshot_immutable BEFORE UPDATE ON pricing_rate_snapshots BEGIN SELECT RAISE(ABORT,'immutable pricing snapshot'); END;
 CREATE TRIGGER IF NOT EXISTS pricing_snapshot_no_delete BEFORE DELETE ON pricing_rate_snapshots BEGIN SELECT RAISE(ABORT,'immutable pricing snapshot'); END;
 CREATE TABLE IF NOT EXISTS pricing_observation_audit (
 id INTEGER PRIMARY KEY, developer TEXT NOT NULL, token_event_id INTEGER NOT NULL,
 kind TEXT NOT NULL, before_json TEXT NOT NULL, after_json TEXT NOT NULL, ts DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
 CREATE INDEX IF NOT EXISTS idx_pricing_observation_event ON pricing_observation_audit(token_event_id,kind);
 CREATE INDEX IF NOT EXISTS idx_pricing_observation_subject ON pricing_observation_audit(developer,token_event_id);`)
	if err != nil {
		return err
	}
	// Retire both startup pricing transformations before Open can reach them.
	// Historical cost conversion may still change units, never the rate basis.
	_, err = db.Exec(`INSERT INTO tier_migrations(name) VALUES (?),(?),(?),(?) ON CONFLICT(name) DO NOTHING`, AggregatePricingCutover, OriginalBasisHistoryPolicy, migrationRecomputeCacheTTL, migrationBackfillPriceVersion)
	return err
}

// initialPrice is used ONLY for first observations; historical resolution never
// calls it. Retaining a labeled first-write guess is distinct from substituting
// today's rates into an unavailable historical basis.
func initialPrice(host, model string) (modelPrice, bool) {
	p, exact := premiumLookup(host, model)
	if exact {
		return p, true
	}
	cls := selfHostedClass(NormalizeModel(model))
	if cls == "" {
		cls = "self-hosted-medium"
	}
	return priceTable[cls], false
}
func retainRate(ctx context.Context, tx *sql.Tx, e TokenEvent) (string, error) {
	if stampPriceVersion(e.PriceVersion) != ActivePriceTableInfo().Version {
		return "", nil
	}
	p, exact := initialPrice(e.Host, e.Model)
	if normalizeBillingMode(e.BillingMode) != p.billingMode {
		return "", nil
	}
	rule := PricingRuleV1
	if e.PricingBasis == PricingAggregateApprox {
		rule = AggregateRuleV1
	}
	expected, _ := clampTokenEventCost(basisCost(p, usage(e), e.PricingBasis))
	supplied, _ := clampTokenEventCost(e.CostMicro)
	return saveRateSnapshot(ctx, tx, rateSnapshot{Rule: rule, TableHash: ActivePriceTableInfo().TableHash, Version: ActivePriceTableInfo().Version, Rate: snapshotPrice(p), Guessed: !exact,
		PreserveCost: !e.TokenDerived && !isPoller(e.Source) && supplied != expected})
}
func saveRateSnapshot(ctx context.Context, tx *sql.Tx, s rateSnapshot) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	id := "rate1:" + hex.EncodeToString(h[:])
	_, err = tx.ExecContext(ctx, `INSERT INTO pricing_rate_snapshots(id,body) VALUES (?,?) ON CONFLICT(id) DO NOTHING`, id, string(raw))
	return id, err
}

var errBasisUnavailable = errors.New("basis_unavailable")

func decodeRateSnapshot(e TokenEvent, id, raw string) (rateSnapshot, error) {
	h := sha256.Sum256([]byte(raw))
	if id == "" || id != "rate1:"+hex.EncodeToString(h[:]) {
		return rateSnapshot{}, errBasisUnavailable
	}
	var s rateSnapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("%w: %v", errBasisUnavailable, err)
	}
	if (s.Rule != PricingRuleV1 && s.Rule != AggregateRuleV1) || s.Version != e.PriceVersion || s.Rate.BillingMode != e.BillingMode || (e.PricingBasis == PricingAggregateApprox) != (s.Rule == AggregateRuleV1) {
		return s, errBasisUnavailable
	}
	return s, nil
}
func resolveRate(ctx context.Context, tx *sql.Tx, e TokenEvent, id string) (modelPrice, error) {
	s, err := resolveRateSnapshot(ctx, tx, e, id)
	return s.price(), err
}
func resolveRateSnapshot(ctx context.Context, tx *sql.Tx, e TokenEvent, id string) (rateSnapshot, error) {
	var raw string
	if id == "" {
		return rateSnapshot{}, errBasisUnavailable
	}
	if err := tx.QueryRowContext(ctx, `SELECT body FROM pricing_rate_snapshots WHERE id=?`, id).Scan(&raw); err != nil {
		return rateSnapshot{}, err
	}
	return decodeRateSnapshot(e, id, raw)
}

// ValidateCacheWriteEvidence accepts observations only, never source-based guesses.
func ValidateCacheWriteEvidence(evidence string, write1h int) error {
	if evidence != "" && evidence != "nested" && evidence != "legacy" {
		return fmt.Errorf("invalid cache_write_evidence")
	}
	if evidence == "legacy" && write1h != 0 {
		return fmt.Errorf("legacy cache_write_evidence cannot contain 1h writes")
	}
	return nil
}
func readPricingRow(ctx context.Context, tx *sql.Tx, key any) (TokenEvent, string, string, error) {
	var e TokenEvent
	var basis, id string
	where := "idempotency_key=?"
	if _, ok := key.(int64); ok {
		where = "id=?"
	}
	err := tx.QueryRowContext(ctx, `SELECT id,developer,model,source,input_tok,output_tok,cache_read,cache_write_5m,cache_write_1h,cost_micro,host,billing_mode,price_version,COALESCE(pricing_basis,''),COALESCE(rate_snapshot,''),COALESCE(cache_write_evidence,'') FROM token_events WHERE `+where, key).Scan(&e.ID, &e.Developer, &e.Model, &e.Source, &e.InputTok, &e.OutputTok, &e.CacheRead, &e.CacheWrite5m, &e.CacheWrite1h, &e.CostMicro, &e.Host, &e.BillingMode, &e.PriceVersion, &basis, &id, &e.CacheWriteEvidence)
	e.PricingBasis, e.RateSnapshot = basis, id
	return e, basis, id, err
}
func pricingAudit(ctx context.Context, tx *sql.Tx, old, next TokenEvent, kind string) error {
	// Keep only the observation needed to prove selection and pricing.
	observation := func(e TokenEvent) any {
		return struct {
			Counts                            [5]int
			CostMicro                         int64
			Evidence, Source, Snapshot, Basis string
		}{tokenVector(e), e.CostMicro, e.CacheWriteEvidence, e.Source, e.RateSnapshot, e.PricingBasis}
	}
	a, err := json.Marshal(observation(old))
	if err != nil {
		return err
	}
	b, err := json.Marshal(observation(next))
	if err != nil {
		return err
	}
	// Identical conflict replay adds no duplicate audit. Both observations survive.
	_, err = tx.ExecContext(ctx, `INSERT INTO pricing_observation_audit(developer,token_event_id,kind,before_json,after_json)
 SELECT ?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM pricing_observation_audit WHERE token_event_id=? AND kind=? AND before_json=? AND after_json=?)`, old.Developer, old.ID, kind, string(a), string(b), old.ID, kind, string(a), string(b))
	return err
}
func pricingProtected(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	predicate, args, err := unsealedFillPredicate(ctx, tx)
	if err != nil {
		return false, err
	}
	var n int
	args = append(args, id)
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_events WHERE `+predicate+` AND id=?`, args...).Scan(&n)
	return n == 0, err
}

// insertPricedToken runs under the caller's immediate writer transaction.
// Whole-vector dominance is safe without writer evidence. An explicit nested
// observation may replace legacy TTL allocation with the same total writes;
// otherwise incomparable vectors retain the first vector and record a conflict.
func insertPricedToken(ctx context.Context, tx *sql.Tx, e TokenEvent) (sql.Result, error) {
	if err := ValidateAttributionRule(e.AttributionRule); err != nil {
		return nil, err
	}
	if e.PRNumber != nil && *e.PRNumber < 1 {
		return nil, fmt.Errorf("pr_number must be >= 1")
	}
	if err := ValidateCacheWriteEvidence(e.CacheWriteEvidence, e.CacheWrite1h); err != nil {
		return nil, err
	}
	old, basis, id, err := readPricingRow(ctx, tx, e.IdempotencyKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if old.Developer != e.Developer || isPoller(old.Source) || isPoller(e.Source) {
			return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE 0`)
		}
		a, b := tokenVector(old), tokenVector(e)
		increased, dominates := false, true
		for i := range a {
			if b[i] > a[i] {
				increased = true
			}
			if b[i] < a[i] {
				dominates = false
			}
		}
		nested := e.CacheWriteEvidence == "nested" && old.CacheWriteEvidence != "nested" && e.CacheWrite5m+e.CacheWrite1h >= old.CacheWrite5m+old.CacheWrite1h && e.InputTok >= old.InputTok && e.OutputTok >= old.OutputTok && e.CacheRead >= old.CacheRead
		if !increased && !nested {
			return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE id=?`, old.ID)
		}
		protected, err := pricingProtected(ctx, tx, old.ID)
		if err != nil {
			return nil, err
		}
		if protected {
			return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE id=?`, old.ID)
		}
		if old.CacheWriteEvidence == "nested" && e.CacheWriteEvidence != "nested" && (old.CacheWrite5m != e.CacheWrite5m || old.CacheWrite1h != e.CacheWrite1h) {
			dominates = false
			nested = false
		}
		if !dominates && !nested {
			// A legacy replay cannot displace retained nested authority.
			if old.CacheWriteEvidence == "nested" && e.CacheWriteEvidence == "legacy" && e.CacheWrite5m+e.CacheWrite1h == old.CacheWrite5m+old.CacheWrite1h && e.InputTok <= old.InputTok && e.OutputTok <= old.OutputTok && e.CacheRead <= old.CacheRead {
				return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE id=?`, old.ID)
			}
			if err := pricingAudit(ctx, tx, old, e, "observation_conflict"); err != nil {
				return nil, err
			}
			return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE id=?`, old.ID)
		}
		next := old
		next.InputTok = e.InputTok
		next.OutputTok = e.OutputTok
		next.CacheRead = e.CacheRead
		next.CacheWrite5m = e.CacheWrite5m
		next.CacheWrite1h = e.CacheWrite1h
		next.CacheWriteEvidence = e.CacheWriteEvidence
		if old.CacheWriteEvidence == "nested" && e.CacheWriteEvidence != "nested" {
			next.CacheWriteEvidence = "nested"
		}
		next.Source = e.Source // audit contributor; the UPDATE preserves the first row source
		var mark any
		if basis != "caller_authoritative" {
			snapshot, err := resolveRateSnapshot(ctx, tx, old, id)
			if err != nil {
				if !errors.Is(err, errBasisUnavailable) && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				if err := pricingAudit(ctx, tx, old, e, "basis_unavailable"); err != nil {
					return nil, err
				}
				return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE id=?`, old.ID)
			}
			if !snapshot.PreserveCost {
				p := snapshot.price()
				expectedOld, _ := clampTokenEventCost(basisCost(p, usage(old), basis))
				if expectedOld != old.CostMicro {
					if err := pricingAudit(ctx, tx, old, e, "existing_cost_mismatch"); err != nil {
						return nil, err
					}
					return tx.ExecContext(ctx, `UPDATE token_events SET id=id WHERE id=?`, old.ID)
				}
				next.CostMicro, mark = clampTokenEventCost(basisCost(p, usage(next), basis))
			}
		}
		if err := pricingAudit(ctx, tx, old, next, "promotion"); err != nil {
			return nil, err
		}
		return tx.ExecContext(ctx, `UPDATE token_events SET input_tok=?,output_tok=?,cache_read=?,cache_write_5m=?,cache_write_1h=?,cost_micro=?,cost_clamped=COALESCE(?,cost_clamped),cache_write_evidence=NULLIF(?,'') WHERE id=?`, next.InputTok, next.OutputTok, next.CacheRead, next.CacheWrite5m, next.CacheWrite1h, next.CostMicro, mark, next.CacheWriteEvidence, old.ID)
	}
	// The store arbitrates the day policy under the same lock as INSERT, closing
	// a race between a legacy day being created and the poller's earlier read.
	if e.PricingBasis == PricingAggregateApprox {
		e.TokenDerived = true
	}
	if isPoller(e.Source) && e.TokenDerived {
		day := normalizeTS(e.Timestamp).Add(-time.Nanosecond).Truncate(24 * time.Hour)
		aggregate, err := pollerUsesAggregatePricing(ctx, tx, day, e.Source)
		if err != nil {
			return nil, err
		}
		e.PricingBasis = PricingLegacyAggregate
		if aggregate {
			e.PricingBasis = PricingAggregateApprox
		}
		e.TokenDerived = true
		if aggregate {
			p, _ := initialPrice(e.Host, e.Model)
			e.CostMicro, e.BillingMode = aggregatePriceV1(p, usage(e)), p.billingMode
		} else {
			p, _ := initialPrice(e.Host, e.Model)
			e.CostMicro, e.BillingMode = priceCallV1(p, usage(e)), p.billingMode
		}
	}
	basis = "token_derived"
	p, exact := initialPrice(e.Host, e.Model)
	if !exact {
		basis = "guessed_estimate"
	}
	if isPoller(e.Source) {
		basis = e.PricingBasis
		if basis == "" {
			basis = PricingLegacyAggregate
		}
	}
	// A mismatched supplied cost is not evidence of a caller-authoritative
	// override: token-derived observations can need an explicit repair/reprice.
	// Manual imports and money without tokens are authoritative; sanctioned
	// corrections mark their authority explicitly in CorrectManualCostEvent.
	if e.Source == "api" || (e.CostMicro != 0 && tokenVector(e) == [5]int{}) {
		basis = "caller_authoritative"
	}
	// Resolve an omitted mode for a new token-derived observation from its own
	// active table, just as ingestion does. Never infer historical modes from it.
	if basis != "caller_authoritative" && e.BillingMode == "" && stampPriceVersion(e.PriceVersion) == ActivePriceTableInfo().Version {
		e.BillingMode = p.billingMode
	}
	e.PricingBasis = basis
	id = ""
	if basis != "caller_authoritative" {
		id, err = retainRate(ctx, tx, e)
		if err != nil {
			return nil, err
		}
		if id == "" {
			basis = "basis_unavailable"
		}
	}

	args, err := insertTokenEventArgs(e)
	if err != nil {
		return nil, err
	}
	args[33], args[34], args[35] = basis, id, e.CacheWriteEvidence
	res, err := tx.ExecContext(ctx, insertTokenEventSQL, args...)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n > 0 && basis != "caller_authoritative" {
		e.ID, err = res.LastInsertId()
		if err != nil {
			return nil, err
		}
		e.RateSnapshot = id
		e.PricingBasis = basis
		e.PriceVersion = stampPriceVersion(e.PriceVersion)
		e.Host = normalizeHost(e.Host)
		e.BillingMode = normalizeBillingMode(e.BillingMode)
		e.CostMicro, _ = clampTokenEventCost(e.CostMicro)
		if err := pricingAudit(ctx, tx, e, e, "initial_observation"); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// CostConsistency never mutates historical rows; RepairPricingBasis is explicit.
type CostConsistencyRow struct {
	ID                         int64
	Status, Class              string
	StoredMicro, ExpectedMicro int64
}

func (d *DB) CostConsistency(ctx context.Context) ([]CostConsistencyRow, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return costConsistency(ctx, tx, 0, false)
}
func costConsistency(ctx context.Context, tx *sql.Tx, version int, apply bool) ([]CostConsistencyRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(idempotency_key,''),price_version FROM token_events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type keyRow struct {
		id      int64
		key     string
		version int
	}
	var keys []keyRow
	for rows.Next() {
		var k keyRow
		if err := rows.Scan(&k.id, &k.key, &k.version); err != nil {
			_ = rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var out []CostConsistencyRow
	for _, k := range keys {
		if version != 0 && k.version != version {
			continue
		}
		r := CostConsistencyRow{ID: k.id, Status: "unverifiable", Class: "basis_unavailable"}
		e, basis, id, err := readPricingRow(ctx, tx, k.id)
		if err != nil {
			return nil, err
		}
		r.StoredMicro = e.CostMicro
		if basis == "caller_authoritative" || e.Source == "api" || (e.CostMicro != 0 && tokenVector(e) == [5]int{}) {
			r.Status = "consistent"
			r.Class = basis
			r.ExpectedMicro = e.CostMicro
			out = append(out, r)
			continue
		}
		snapshot, err := resolveRateSnapshot(ctx, tx, e, id)
		p := snapshot.price()
		if err != nil && !errors.Is(err, errBasisUnavailable) && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			r.ExpectedMicro, _ = clampTokenEventCost(basisCost(p, usage(e), basis))
			r.Status = "consistent"
			r.Class = basis
			if r.ExpectedMicro != r.StoredMicro {
				r.Status = "inconsistent"
				r.Class = "cost_mismatch"
			}
			if isPoller(e.Source) {
				if r.Status == "inconsistent" {
					r.Class = "poller_cost_mismatch"
				}
				base := p
				base.contextThreshold = 0
				r.ExpectedMicro, _ = clampTokenEventCost(priceCallV1(base, usage(e)))
				if basis == PricingAggregateApprox && r.ExpectedMicro == r.StoredMicro {
					r.Status = "consistent"
					r.Class = "aggregate_approx"
				}
				if basis != "aggregate_approx" && p.contextThreshold > 0 {
					r.Class = "poller_aggregate_tiered"
					r.Status = "unverifiable" // request sizes are absent; this is potential exposure, not proven overbilling
				}
			}
		}
		var conflicts int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pricing_observation_audit WHERE token_event_id=? AND kind='observation_conflict'`, e.ID).Scan(&conflicts); err != nil {
			return nil, err
		}
		var proofs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pricing_observation_audit WHERE token_event_id=? AND kind IN ('initial_observation','promotion')`, e.ID).Scan(&proofs); err != nil {
			return nil, err
		}
		if conflicts > 0 || (proofs == 0 && e.CacheWrite5m+e.CacheWrite1h > 0) {
			r.Status = "unverifiable"
			r.Class = "ttl_split_unverifiable"
		}
		protected, err := pricingProtected(ctx, tx, e.ID)
		if err != nil {
			return nil, err
		}
		if apply && r.Status == "inconsistent" && !protected && !isPoller(e.Source) {
			next := e
			next.CostMicro = r.ExpectedMicro
			if snapshot.PreserveCost {
				// Explicit repair establishes token-derived dollars at the retained
				// original rates; subsequent promotions use those same rates.
				snapshot.PreserveCost = false
				next.RateSnapshot, err = saveRateSnapshot(ctx, tx, snapshot)
				if err != nil {
					return nil, err
				}
			}
			if err := pricingAudit(ctx, tx, e, next, "original_basis_repair"); err != nil {
				return nil, err
			}
			_, mark := clampTokenEventCost(basisCost(p, usage(e), basis))
			if _, err := tx.ExecContext(ctx, `UPDATE token_events SET cost_micro=?,cost_clamped=COALESCE(?,cost_clamped),rate_snapshot=? WHERE id=?`, next.CostMicro, mark, next.RateSnapshot, e.ID); err != nil {
				return nil, err
			}
		}
		if protected && r.Status == "inconsistent" {
			r.Class = "sealed_cost_mismatch"
		}
		out = append(out, r)
	}
	return out, nil
}

// RepairPricingBasis selects exactly one original version, never today's table.
// The caller explicitly chooses apply. A supplied original-table proof needs
// a writer transaction even for a dry run; all temporary recovery rolls
// back. Without proof, a dry run takes only a read snapshot.
func (d *DB) RepairPricingBasis(ctx context.Context, version int, apply bool, evidence ...OriginalBasisEvidence) ([]CostConsistencyRow, error) {
	if version < 1 {
		return nil, fmt.Errorf("exact --version must be >= 1")
	}
	var tx *sql.Tx
	var err error
	var release func()
	if apply || len(evidence) > 0 {
		tx, err = beginImmediate(ctx, d.db)
		if err == nil {
			release = func() { _ = tx.Rollback() }
		}
	} else {
		tx, err = d.db.BeginTx(ctx, nil)
		release = func() { _ = tx.Rollback() }
	}
	if err != nil {
		return nil, err
	}
	defer release()
	if len(evidence) > 1 {
		return nil, fmt.Errorf("only one original pricing basis may be declared")
	}
	if len(evidence) == 1 {
		if err := recoverOriginalBasis(ctx, tx, version, evidence[0], apply); err != nil {
			return nil, err
		}
	}
	out, err := costConsistency(ctx, tx, version, apply)
	if err != nil {
		return nil, err
	}
	if apply {
		err = tx.Commit()
	}
	return out, err
}
