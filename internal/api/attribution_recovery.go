package api

import (
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func recoveryLayer(win windowScores, mode scoring.AggregationMode) string {
	if mode.Anonymized() {
		return ""
	}
	for _, c := range win.issueCosts {
		if c.ReadTimeRecoveryEvents > 0 {
			return string(store.AttributionRecovery)
		}
	}
	return ""
}
