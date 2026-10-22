package store

import (
	"context"
	"database/sql"
	"math"
	"time"
)

const PricingAggregateApprox = "aggregate_approx"
const PricingLegacyAggregate = "legacy_aggregate"
const AggregateRuleV1 = "aggregate-v1"

// AggregatePricingCutover names the installation-local policy activation recorded
// in tier_migrations. It applies to newly started poll days, including backfills;
// existing days inherit their first pricing rule. It is not a usage-date backfill.
const AggregatePricingCutover = "aggregate-approx-v1-2026-10-14"

// ComputeAggregateCostHost prices a daily remainder at base rates. Only this path
// disables request thresholds; priceCallV1 and ComputeCostHost remain request-grain.
func ComputeAggregateCostHost(host, model string, u CostUsage) (int64, string) {
	p, exact := initialPrice(host, model)
	cost := aggregatePriceV1(p, u)
	recordPricedCost(float64(cost))
	if !exact {
		norm := NormalizeModel(model)
		guessPath := GuessPathFlat
		if cls := selfHostedClass(norm); cls != "" {
			warnHeuristicModel(norm, cls)
			guessPath = GuessPathSizeClass
		} else {
			warnUnknownModel(norm)
		}
		recordUnknownModel(guessPath)
		recordUnknownModelCost(float64(cost))
	}
	return cost, p.billingMode
}
func aggregatePriceV1(p modelPrice, u CostUsage) int64 {
	p.contextThreshold = 0
	return priceCallV1(p, u)
}
func basisCost(p modelPrice, u CostUsage, basis string) int64 {
	if basis == PricingAggregateApprox {
		return aggregatePriceV1(p, u)
	}
	return priceCallV1(p, u)
}

// PollerUsesAggregatePricing preserves the first rule of an existing poll day.
func (d *DB) PollerUsesAggregatePricing(ctx context.Context, day time.Time, source string) (bool, error) {
	return pollerUsesAggregatePricing(ctx, d.db, day, source)
}
func pollerUsesAggregatePricing(ctx context.Context, q readQuerier, day time.Time, source string) (bool, error) {
	start := day.UTC().Truncate(24 * time.Hour)
	var legacy int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_events WHERE source=? AND ts>? AND ts<=? AND COALESCE(pricing_basis,'')<>?`, source, start, start.Add(24*time.Hour), PricingAggregateApprox).Scan(&legacy)
	return legacy == 0, err
}

const AggregateApproximationNotice = "Provider daily remainders use base-rate aggregate approximations without request-level context evidence. Genuine long-context traffic may cost more, so these approximations may understate cost and raise TIER."
const AggregatePricingPolicyNotice = "New provider daily remainders use base-rate approximations and may understate long-context cost, raising TIER. Historical provider aggregates may differ from request-level billing in either direction. Request lengths are unavailable in daily aggregates."
const AnonymousAggregateNotice = "Provider daily remainders, if present, use base-rate aggregate approximations and may understate genuine long-context cost, raising TIER. Historical provider aggregates can differ from request-level billing in either direction. Provider remainders use pseudo-developer attribution and are excluded from identified-group denominators. Population and cost ceilings are not shown in this mode."
const HistoricalAggregateNotice = "This window includes historical provider aggregates priced without request-level context evidence. Their stored costs may differ from request-level billing, in either direction."

type AggregatePricingDisclosure struct {
	Cutover             string `json:"cutover"`
	ApproximateRows     int64  `json:"approximate_rows,omitempty"`
	HistoricalRows      int64  `json:"historical_rows,omitempty"`
	UnverifiableRows    int64  `json:"unverifiable_rows,omitempty"`
	ApproximationNotice string `json:"approximation_notice,omitempty"`
	HistoricalNotice    string `json:"historical_notice,omitempty"`
	// The ceiling is computed from every token class and retained original rates,
	// using the highest base/tier rate for each token class. Missing bases never use active rates.
	PossibleIncreaseMicro int64  `json:"possible_increase_micro,omitempty"`
	CeilingStatus         string `json:"ceiling_status"`
}

func (d *DB) AggregatePricingWindow(ctx context.Context, since, until time.Time, scope RepoScope) (AggregatePricingDisclosure, error) {
	return (reader{d.db}).AggregatePricingWindow(ctx, since, until, scope)
}
func (r reader) AggregatePricingWindow(ctx context.Context, since, until time.Time, scope RepoScope) (AggregatePricingDisclosure, error) {
	where, args := digestPredicate(since, until, scope)
	// Only Anthropic/OpenAI remainder feeds lack request-level pricing evidence.
	// copilot-billing reports dollars and cursor-admin prices each event before
	// aggregation, so neither belongs in this daily-approximation disclosure.
	rows, err := r.q.QueryContext(ctx, `SELECT input_tok,output_tok,cache_read,cache_write_5m,cache_write_1h,cost_micro,price_version,billing_mode,COALESCE(pricing_basis,''),COALESCE(rate_snapshot,''),s.body FROM token_events LEFT JOIN pricing_rate_snapshots s ON s.id=token_events.rate_snapshot `+" WHERE "+where+` AND source IN ('anthropic-admin','openai-usage')`, args...)
	if err != nil {
		return AggregatePricingDisclosure{}, err
	}
	defer func() { _ = rows.Close() }()
	out := AggregatePricingDisclosure{Cutover: AggregatePricingCutover, CeilingStatus: "available"}
	for rows.Next() {
		var e TokenEvent
		var basis, id string
		var raw sql.NullString
		if err := rows.Scan(&e.InputTok, &e.OutputTok, &e.CacheRead, &e.CacheWrite5m, &e.CacheWrite1h, &e.CostMicro, &e.PriceVersion, &e.BillingMode, &basis, &id, &raw); err != nil {
			return out, err
		}
		if basis == PricingAggregateApprox {
			out.ApproximateRows++
			out.ApproximationNotice = AggregateApproximationNotice
		} else {
			out.HistoricalRows++
			out.HistoricalNotice = HistoricalAggregateNotice
		}
		e.PricingBasis = basis
		s, err := decodeRateSnapshot(e, id, raw.String)
		if err != nil {
			out.UnverifiableRows++
			out.CeilingStatus = "unverifiable_original_basis"
			continue
		}
		if s.Guessed || s.Rate.BillingMode != BillingPerToken {
			out.UnverifiableRows++
			if out.CeilingStatus != "unverifiable_original_basis" {
				out.CeilingStatus = "estimated_rates_no_billing_bound"
			}
			continue
		}
		p := s.price()
		p.inputPerM = math.Max(p.inputPerM, p.inputPerMOver)
		p.outputPerM = math.Max(p.outputPerM, p.outputPerMOver)
		p.contextThreshold = 0
		dollars := priceCallDollarsV1(p, usage(e))
		if math.IsNaN(dollars) || math.IsInf(dollars, 0) || dollars*1e6 >= float64(math.MaxInt64) {
			if out.CeilingStatus == "available" {
				out.CeilingStatus = "ceiling_overflow"
			}
			continue
		}
		high := DollarsToMicro(dollars)
		delta := high - e.CostMicro
		if delta > 0 {
			if delta > math.MaxInt64-out.PossibleIncreaseMicro {
				out.PossibleIncreaseMicro = math.MaxInt64
				if out.CeilingStatus == "available" {
					out.CeilingStatus = "ceiling_overflow"
				}
			} else {
				out.PossibleIncreaseMicro += delta
			}
		}
	}
	return out, rows.Err()
}
