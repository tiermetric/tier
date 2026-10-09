package api

import (
	"regexp"
	"strings"
	"testing"
)

// The k-anonymity leak guards in this package work by substring-scanning the
// WHOLE response body for a seeded developer or team name. That is deliberately
// crude and deliberately broad: it catches a name leaking through ANY field,
// including one nobody thought to model in a struct.
//
// 🔴 Its cost is that any HIGH-ENTROPY opaque field in the response can contain
// a short seeded name BY CHANCE. #713 added two 64-hex-character digests to
// price_table, and three of those guards immediately went red on seeds named
// "b2", "b3", "e1" and "e2" — every one a false positive from hex characters
// inside a hash, with no identity anywhere in the response. Left unfixed the
// fleet-visible outcome is worse than a flaky test: a k-anonymity guard that
// fails for reasons unrelated to k-anonymity gets diagnosed as noise and then
// gets weakened.
//
// leakScanBody redacts EXACTLY the opaque digests and nothing else, so the scan
// keeps its breadth over every field that can carry an identity.
//
// ⚠️ It must never be widened into "redact anything that looks random". The
// blast radius of a redaction here is a leak the guard can no longer see.
var priceHashValueRE = regexp.MustCompile(`"(?:table_hash|file_hash)":"[a-z0-9]+:[0-9a-f]{64}"`)

// leakScanBody returns the response body with the price_table content-identity
// digests replaced by a fixed placeholder, ready for a `strings.Contains` scan
// for leaked identities.
//
// ⚠️ TWO USAGE NOTES, both learned in review.
//
//  1. It t.Fatals on a response that has NO price_table — /scores/{developer},
//     404 bodies, /healthz. Use it only on the endpoints that carry the block.
//  2. It treats the SYMPTOM. The root cause is test seeds drawn entirely from
//     [0-9a-f] ("b2", "b3", "e1", "e2"), which a 64-hex digest contains by
//     chance. Every other body-substring scan in this package currently uses
//     non-hex seeds (alice, bob, carol, loner, ghost, ...), so nothing else is
//     at risk today — but the class reopens the moment someone picks another
//     hex-only seed, and the failure will read as a k-anonymity leak. Preferring
//     a seed with at least one non-hex character retires the class outright.
//
// It FAILS the calling test if no digest was found. Silently redacting nothing
// would quietly restore the false positives above the moment the field is
// renamed or its shape changes — the redaction has to be observable to be
// trustworthy. Every /scores and /scores/compare response carries both hashes
// unconditionally (priceTableJSON has no omitempty), so "none found" is a real
// contract change, not a legitimate case.
func leakScanBody(t *testing.T, body []byte) string {
	t.Helper()
	raw := string(body)
	if !priceHashValueRE.MatchString(raw) {
		t.Fatalf("leakScanBody found no price_table table_hash/file_hash digest to redact — "+
			"either the response stopped carrying the #713 content identity, or the field "+
			"names/shape changed and this redaction now silently does nothing (which would "+
			"reinstate the hex-collision false positives it exists to remove).\nbody: %s", raw)
	}
	return priceHashValueRE.ReplaceAllString(raw, `"<price-table-digest-redacted>"`)
}

// TestLeakScanBody_RedactsOnlyTheDigests is the control for the helper above:
// the redaction must remove the hex that causes false positives and must leave
// every identity-bearing byte of the body alone. Without this, a helper that
// over-redacted would make the guards it feeds silently blind.
func TestLeakScanBody_RedactsOnlyTheDigests(t *testing.T) {
	body := []byte(`{"since":"2026-01-01","price_table":{"version":9,"effective_date":"2026-07-26",` +
		`"table_hash":"tierpt1:7acfb82b76f518d25b81d64a1065e29208feaa3dc821e15c636dba4a517db29c",` +
		`"file_hash":"sha256:96719948a59cd5b379baadbf6ed2fb451bb8a62349dc38cb5875d6ed94ef2094"},` +
		`"teams":[{"team":"engineering"}]}`)

	// Fixture-integrity check. ⚠️ Be precise about what this is: `body` is a
	// hardcoded literal, so this loop cannot fail today — it is NOT a live
	// control, and review correctly flagged it when it was labelled "premise".
	// It is kept so that an edit to the fixture digests which happened to remove
	// every hex-borne collision would fail here rather than quietly turn the
	// assertions below into a test of nothing.
	rawStr := string(body)
	for _, needle := range []string{"b2", "e2"} {
		if !strings.Contains(rawStr, needle) {
			t.Fatalf("fixture no longer contains %q inside its digests — the sample was edited "+
				"and no longer reproduces the hex false positive this test is about", needle)
		}
	}

	got := leakScanBody(t, body)

	// The hex-borne false positives are gone...
	for _, needle := range []string{"7acfb82b", "96719948"} {
		if strings.Contains(got, needle) {
			t.Errorf("digest fragment %q survived redaction: %s", needle, got)
		}
	}
	// ...and everything that could carry an identity is untouched.
	for _, keep := range []string{`"since":"2026-01-01"`, `"version":9`, `"effective_date":"2026-07-26"`, `"team":"engineering"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q — it must touch ONLY the two digest values, or the "+
				"leak scans it feeds go blind: %s", keep, got)
		}
	}
}
