// Package prderive is the single source of truth for deriving a TIER outcome's
// size-label weight and work-type category from a merged pull request's labels.
//
// Both ingestion paths depend on it so a PR scores IDENTICALLY however it arrived:
//   - the live GitHub webhook (internal/webhook), and
//   - the historical reconstruction command (cmd/tierd backfill, #237).
//
// It exists to close a drift risk (#301): the two paths previously carried
// byte-for-byte copies of this derivation (webhook's unexported helpers vs.
// backfill's `backfill*` reproductions), and a change to one that skipped the
// other would silently weight the same PR differently on live vs. backfilled
// rows. Consolidating here means one implementation and one set of unit tests.
//
// The package operates on plain label NAMES ([]string): the minimal input both
// callers can supply from their respective GitHub payload shapes ("accept the
// least specific thing you need"). It imports only internal/store for the
// work-type and provenance constants, so there is no import cycle with either
// consumer.
package prderive

import (
	"strings"

	"github.com/tiermetric/tier/internal/store"
)

// defaultSizeLabels is TIER's built-in PR size-label -> outcome-weight table
// (#244): the GitHub size/xs..xl labels and their bare xs..xl forms mapped onto
// the fixed 0.5/1/3/5/8 outcome scale. It is the fallback SizeWeight uses when no
// configured table is supplied, so an adopter who configures nothing sees no
// behaviour change. Keys are lowercase; SizeWeight lowercases each label name
// before matching. Kept unexported (SizeWeight is the only public entry point) so
// no importer can mutate this process-wide shared map out from under the webhook,
// which reads it concurrently while serving deliveries.
var defaultSizeLabels = map[string]float64{
	"size/xs": 0.5,
	"xs":      0.5,
	"size/s":  1.0,
	"s":       1.0,
	"size/m":  3.0,
	"m":       3.0,
	"size/l":  5.0,
	"l":       5.0,
	"size/xl": 8.0,
	"xl":      8.0,
}

// NormalizeSizeLabels folds a configured size-label table (outcomes.size_labels,
// #244) so lookups match case-insensitively: each key is lowercased and trimmed,
// mirroring how SizeWeight folds label names. It returns nil for an empty input
// (nil OR non-nil-empty), which is the deliberate "use the defaults" signal both
// callers rely on — an explicit `size_labels: {}` in config must preserve the
// built-in table, not install an empty one that scores every PR by the heuristic.
// Callers normalize ONCE at construction and hand the result to SizeWeight, so
// the per-PR lookup stays allocation-free. config.Load has already validated the
// weights are on the fixed scale and that no two names collide once folded, so no
// re-validation happens here.
func NormalizeSizeLabels(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	norm := make(map[string]float64, len(m))
	for k, v := range m {
		norm[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return norm
}

// SizeWeight returns the outcome weight of the first label whose lowercased name
// is present in table, or 0 when none match — the "fall through to the git-diff
// heuristic" signal store.ResolveWeight expects. A nil (or empty) table falls back
// to the built-in defaultSizeLabels, so callers pass the operator's
// NormalizeSizeLabels result and get the built-in table automatically when nothing
// was configured. A custom table REPLACES the defaults rather than extending them:
// an org's remap is the single source of truth for what its labels mean. Label
// order is honoured (first match wins), so a configured match still yields
// weight_source='label'. Note the label name is lowercased but NOT trimmed before
// lookup (GitHub does not pad label names) — deliberately matching the historical
// webhook/backfill lookups this consolidated, and distinct from WorkTypeFromLabels,
// which does trim.
func SizeWeight(labels []string, table map[string]float64) float64 {
	if len(table) == 0 {
		table = defaultSizeLabels
	}
	for _, name := range labels {
		if w, ok := table[strings.ToLower(name)]; ok {
			return w
		}
	}
	return 0
}

// workTypePrecedence is the FIXED, documented precedence WorkTypeFromLabels
// applies when a PR carries more than one type label (#187). Highest-impact
// category wins so a specialist's work is never diluted into 'feature': a PR
// labelled both `security` and `feature` is security work. Order (highest first):
// security, incident, compliance, bug, tech-debt, research, feature. This is a
// DELIBERATELY different order from store.canonicalWorkTypes (declaration order) —
// precedence is an impact ranking, not the enum's listing order. 'feature' sits
// last so it only wins when it is the sole type label, which is indistinguishable
// in score terms from the no-label default. Kept unexported so no importer can
// reorder this process-wide ranking (WorkTypeFromLabels is the only entry point).
var workTypePrecedence = []string{
	store.WorkTypeSecurity,
	store.WorkTypeIncident,
	store.WorkTypeCompliance,
	store.WorkTypeBug,
	store.WorkTypeTechDebt,
	store.WorkTypeResearch,
	store.WorkTypeFeature,
}

// WorkTypeFromLabels derives an outcome's work_type and its provenance from a PR's
// label names (#187). A label maps to a category when it equals a canonical type
// name (e.g. `security`) OR carries a `type:<name>` / `kind:<name>` prefix (e.g.
// `type:incident`, `kind:research`) — all matched case-insensitively after trimming
// surrounding whitespace. When several labels match different categories the
// workTypePrecedence order breaks the tie (highest-impact wins), so the result is
// deterministic regardless of the order GitHub happens to serialise the labels in.
// No matching label yields ('feature', 'default'), the same honest baseline a
// labelless PR takes for its weight.
func WorkTypeFromLabels(labels []string) (workType, source string) {
	matched := make(map[string]bool, len(labels))
	for _, l := range labels {
		name := strings.ToLower(strings.TrimSpace(l))
		token := name
		if rest, ok := strings.CutPrefix(name, "type:"); ok {
			token = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(name, "kind:"); ok {
			token = strings.TrimSpace(rest)
		}
		if store.ValidWorkType(token) {
			matched[token] = true
		}
	}
	for _, wt := range workTypePrecedence {
		if matched[wt] {
			return wt, store.WorkTypeSourceLabel
		}
	}
	return store.WorkTypeFeature, store.WorkTypeSourceDefault
}
