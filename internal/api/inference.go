package api

import "net/http"

// Legacy ci_* and significant field names remain wire-compatible. These headers
// describe all TIER and reciprocal cost-per-point bounds, including segments and
// developer detail. No inference verdict is established by a non-overlap result.
func setInferenceHeaders(h http.Header) {
	h.Set("Tier-Inference", "not_established")
	h.Set("Tier-Bounds-Status", "unvalidated_descriptive")
	h.Set("Tier-Bounds-Note", "Nominal 95% bounds. Coverage is unvalidated: in constructed tests where several outcomes share one issue, they covered the true value less often than 95%. Excludes uncertainty from unattributed spend, incorrect attribution and uncaptured usage.")
}
