package store

// UnknownPricingTokenShareLabel is the operator-ruled wording for this read-time
// diagnostic (R-2026-10-14-17, D-32 C″). It is not an original guessed-dollar
// share or a bound on pricing error.
const UnknownPricingTokenShareLabel = "share of tokens with an unknown pricing basis or without an exact entry in the current price table"

// PricingTokenCounts holds the token-weighted read-time pricing diagnostic.
// Total includes all five non-overlapping token classes, even on zero-cost rows.
// Unknown includes unknown or estimated bases and rows at a different price
// version regardless of today's model lookup:
// the registry retains table identity, not historical rates, so that lookup cannot
// establish their original pricing basis. Exact configured estimates remain exact
// table entries; this diagnostic does not certify provider list rates.
type PricingTokenCounts struct {
	Total   int64
	Unknown int64
}

// Add classifies a row (or a group with identical host/model/version/basis) against the
// active table without repricing or writing it. Tables are loaded before serving,
// just as for ComputeCostHost. A zero-token row contributes to neither count.
func (c *PricingTokenCounts) Add(host, model string, priceVersion int, basis string, tokens int64) {
	if tokens <= 0 {
		return
	}
	c.Total += tokens
	// Caller-authoritative dollars have a known basis, though they need not equal
	// a token-derived cost. Missing, unfamiliar, guessed and aggregate bases fail
	// closed even if today's model lookup succeeds.
	knownBasis := basis == "token_derived" || basis == "caller_authoritative"
	if !knownBasis || priceVersion != ActivePriceTableInfo().Version || !modelIsExactHost(host, model) {
		c.Unknown += tokens
	}
}

// Share returns a fraction in [0, 1], or zero with no recorded tokens.
func (c PricingTokenCounts) Share() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Unknown) / float64(c.Total)
}

// CostLabel qualifies a shown cost iff its unknown-pricing token share is nonzero.
func (c PricingTokenCounts) CostLabel() string {
	if c.Share() > 0 {
		return "estimated cost"
	}
	return ""
}
