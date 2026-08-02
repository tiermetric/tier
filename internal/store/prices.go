package store

import (
	"bytes"
	_ "embed"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	// go.yaml.in/yaml/v3 is the maintained YAML-org continuation of the
	// archived gopkg.in/yaml.v3 (#52) — same API, same `yaml:` tags, same
	// KnownFields strict decoding. The price table's strict-unknown-key
	// rejection (parsePriceTable below) is preserved unchanged by the swap.
	"go.yaml.in/yaml/v3"
)

// modelPrice holds per-million-token pricing for a single model.
type modelPrice struct {
	inputPerM  float64
	outputPerM float64
	// combined is set for self-hosted models where in+out share one rate.
	combined bool
	// provider ("anthropic" / "openai" / "google" / "xai" / "deepseek" /
	// "self-hosted") selects the parse-time DEFAULT cache multipliers (see
	// providerDefaultMults). It is no longer read at ComputeCost time — the
	// EFFECTIVE multipliers are baked into cacheReadMult/cacheWrite{5m,1h}Mult
	// below (provider default, overridden by any explicit per-model YAML value).
	provider string
	// cacheReadMult / cacheWrite5mMult / cacheWrite1hMult are the EFFECTIVE cache
	// multipliers (of the SELECTED input rate) for this model, resolved once at
	// parse time. Cache discounts are per-MODEL, not merely per-provider (OpenAI
	// 4-era reads at 0.5× but the 5-era reads at 0.1×; Gemini 2.5+ implicit cache
	// reads at 0.25×; DeepSeek publishes an absolute cache-hit rate) — so the YAML
	// carries optional overrides and parsePriceTable bakes the resolved value here.
	// They stay MULTIPLIERS (not absolute $/M) so they scale correctly off the
	// premium input rate under the long-context over-tier below. Unused on
	// combined entries (the combined path bills every class at the single rate).
	cacheReadMult    float64
	cacheWrite5mMult float64
	cacheWrite1hMult float64
	// contextThreshold, when > 0, marks a long-context model that re-prices a
	// request at inputPerMOver/outputPerMOver once its input-side context
	// exceeds this many tokens (#4). Real today: Anthropic's Sonnet 1M beta
	// (≤200K $3/$15, >200K $6/$22.50) and Gemini Pro (>200K premium). A zero
	// threshold means single-tier flat pricing (the common case). The over-tier
	// rates are meaningless without a threshold, so parsePriceTable rejects a
	// partial specification.
	contextThreshold int
	inputPerMOver    float64
	outputPerMOver   float64
	// billingMode records HOW this host bills the model (#300), resolved once at
	// parse time: per_token for metered APIs, subscription for flat $/mo hosts
	// (#113, Ollama Cloud / GLM — seeded by #268), or self_hosted_amortized for
	// the self-hosted reference rates. ComputeCostHost returns it so a stored event
	// carries the honest billing basis and a subscription/amortized figure is never
	// dressed as a canonical $/M. Defaults per_token, or self_hosted_amortized for a
	// provider=self-hosted entry, when the YAML omits it.
	billingMode string
}

// Provider tags used by modelPrice.provider and the cache-multiplier switch
// in ComputeCost. Exposed as constants so callers and tests can refer to them
// without stringly-typed literals.
const (
	providerAnthropic  = "anthropic"
	providerOpenAI     = "openai"
	providerGoogle     = "google"
	providerXAI        = "xai"
	providerDeepSeek   = "deepseek"
	providerSelfHosted = "self-hosted"
)

// Billing-mode tags recorded on a priced event (#300). The cost of an
// open-weights model is a property of the SERVING HOST, not the weights, so the
// mode records how that host charges: metered per token, a flat subscription
// (#113 — Ollama Cloud / GLM $/mo), or an amortized self-hosted estimate. The
// subscription/amortized modes flag a DERIVED/APPROXIMATE figure so it is never
// read as a canonical $/M.
const (
	BillingPerToken            = "per_token"
	BillingSubscription        = "subscription"
	BillingSelfHostedAmortized = "self_hosted_amortized"
)

// validBillingModes is the allowlist a loaded table's billing_mode fields must
// match. An unrecognized value is a fail-loud parse error (mirrors validProviders)
// rather than a silent default, so a fat-fingered mode can't quietly mislabel spend.
var validBillingModes = map[string]bool{
	BillingPerToken: true, BillingSubscription: true, BillingSelfHostedAmortized: true,
}

// HostUnknown is the sentinel serving host recorded when the producer could not
// determine one — the first-party JSONL/poller paths (always Anthropic) and any
// proxy without a target. It is stored in the NOT NULL host column in place of ""
// (mirrors repoid.Unqualified) and NEVER used as a host-qualified pricing key: an
// unknown host prices at the model-only rate, exactly as before #300.
const HostUnknown = "unknown"

// hostKeySep joins a normalized model and a serving host into the host-qualified
// price-table key #268 seeds rates under. See HostModelKey.
const hostKeySep = "@"

// dateVersionRE strips date/version suffixes like "-20250514" or "-preview" from model names.
var dateVersionRE = regexp.MustCompile(`-\d{8}$|-\d{6}$|-\d{4}$|-preview\d*$|-latest$`)

// yyyymmddRE matches a trailing "YYYY-MM-DD" date segment used by some provider model IDs.
var yyyymmddRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// paramCountRE matches parameter-count suffixes like "7b", "13b", "70b" as a whole word.
var paramCountRE = regexp.MustCompile(`\d+b\b`)

// defaultPriceTableYAML is the canonical Reference Price Table, embedded so the
// static binary ships no external file and zero-config still works (#68). It is
// parsed once at package init into priceTable; prices.yaml is the source of
// truth (edit there, not here).
//
//go:embed prices.yaml
var defaultPriceTableYAML []byte

// priceTableDoc is the versioned YAML document shape (#68). version +
// effective_date version the whole table as one atomic unit; models maps a
// normalized model key to its list price.
type priceTableDoc struct {
	Version       int                   `yaml:"version"`
	EffectiveDate string                `yaml:"effective_date"`
	Models        map[string]priceEntry `yaml:"models"`
}

// priceEntry is the YAML form of modelPrice — yaml.v3 needs exported fields, so
// this mirrors modelPrice and is converted in parsePriceTable.
type priceEntry struct {
	InputPerM  float64 `yaml:"input_per_m"`
	OutputPerM float64 `yaml:"output_per_m"`
	Combined   bool    `yaml:"combined"`
	Provider   string  `yaml:"provider"`
	// Optional per-model cache multipliers (#122). 0 / omitted means "use the
	// provider default" (see providerDefaultMults); an explicit positive value
	// overrides that class. A negative value is a parse error, and any of these
	// on a combined entry is a parse error (combined bills every class at the
	// single rate — a multiplier there would be silently ignored).
	CacheReadMult    float64 `yaml:"cache_read_mult"`
	CacheWrite5mMult float64 `yaml:"cache_write_5m_mult"`
	CacheWrite1hMult float64 `yaml:"cache_write_1h_mult"`
	// Optional long-context tier (#4): a request whose input-side context
	// exceeds ContextThreshold tokens is priced at InputPerMOver/OutputPerMOver
	// instead of the base rates. All three are set together or none; a partial
	// specification is a parse error.
	ContextThreshold int     `yaml:"context_threshold"`
	InputPerMOver    float64 `yaml:"input_per_m_over"`
	OutputPerMOver   float64 `yaml:"output_per_m_over"`
	// Optional billing mode (#300): per_token | subscription | self_hosted_amortized.
	// Omitted means "derive from provider" — self_hosted_amortized for a self-hosted
	// entry, per_token otherwise (see parsePriceTable). #268 sets it explicitly on
	// the flat-subscription host rows it seeds.
	BillingMode string `yaml:"billing_mode"`
}

// PriceTableInfo is the public view of the active table's version metadata, for
// the caller to log at startup.
type PriceTableInfo struct {
	Version       int
	EffectiveDate string
	ModelCount    int
}

// priceTable is the active Reference Price Table, keyed by normalized model name.
// Populated at package init from the embedded prices.yaml and optionally swapped
// ONCE, before serving, by LoadPriceTable (a `tierd --prices` override).
//
// WRITE-ONCE-BEFORE-SERVE: every ComputeCost read happens-after that single
// startup write (a clean happens-before via the serve goroutine), so the plain
// map needs no lock. Do NOT add a runtime reload without making this access
// synchronized.
var priceTable map[string]modelPrice

// activePriceTableInfo describes the currently-loaded table (logged at startup).
var activePriceTableInfo PriceTableInfo

// validProviders is the allowlist a loaded table's provider fields must match —
// the same set providerDefaultMults resolves at parse time. A typo'd provider
// would silently get the 1.0× default multipliers, so parsing rejects it.
var validProviders = map[string]bool{
	providerAnthropic: true, providerOpenAI: true, providerGoogle: true,
	providerXAI: true, providerDeepSeek: true, providerSelfHosted: true,
}

// requiredSelfHostedKeys MUST exist in any loaded table: self-hosted-medium is
// the unknown-model fallback rate ComputeCost looks up unconditionally, and
// selfHostedClass resolves param-count heuristics to all three. A table missing
// any of them would silently price matched events at $0, so parsing rejects it.
var requiredSelfHostedKeys = []string{"self-hosted-large", "self-hosted-medium", "self-hosted-small"}

// Per-provider DEFAULT cache multipliers, applied to a model's SELECTED input
// rate. Historically these were the ONLY multipliers and lived in ComputeCost as
// a per-provider switch; #122 reverses that — cache discounts turned out to be
// per-MODEL, not per-provider (OpenAI 4-era reads at 0.5× but the 5-era at 0.1×,
// Gemini 2.5+ implicit cache at 0.25×, DeepSeek at an absolute cache-hit rate),
// so these are now only the fallback a model inherits when prices.yaml sets no
// explicit override. parsePriceTable bakes the resolved value into modelPrice.
//
// Anthropic: published prices (claude.com/pricing) — 5m write 1.25×, 1h write
// 2.0×, read 0.1×. OpenAI: prompt-cache hit at 0.5× of input; no write SKU (its
// write buckets stay 0 in the parser, so the 1.0× default is inert). Others:
// no documented cache discount today; treat as 1.0×.
const (
	anthropicWrite5mMult = 1.25
	anthropicWrite1hMult = 2.00
	anthropicReadMult    = 0.10
	openAIReadMult       = 0.50
)

// providerDefaultMults returns the (read, write5m, write1h) cache multipliers a
// model of the given provider bills when it carries no explicit override in
// prices.yaml. Self-hosted entries never reach this via ComputeCost (the
// combined path bills a single rate), but they still resolve to 1.0× here so the
// baked value is well-defined.
func providerDefaultMults(provider string) (read, write5m, write1h float64) {
	switch provider {
	case providerAnthropic:
		return anthropicReadMult, anthropicWrite5mMult, anthropicWrite1hMult
	case providerOpenAI:
		return openAIReadMult, 1.0, 1.0
	default:
		// google / xai / deepseek / self-hosted: no documented discount.
		return 1.0, 1.0, 1.0
	}
}

// parsePriceTable strict-decodes a versioned price-table YAML document and
// validates it into the internal modelPrice map. It is the single gate for BOTH
// the embedded default and any --prices override, so a malformed or incomplete
// table can never reach ComputeCost. Errors are returned (the caller chooses
// fatal-at-init vs fatal-at-startup) rather than logged.
func parsePriceTable(data []byte) (map[string]modelPrice, PriceTableInfo, error) {
	var doc priceTableDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown keys — matches config.Load's strictness
	if err := dec.Decode(&doc); err != nil {
		return nil, PriceTableInfo{}, fmt.Errorf("decode price table: %w", err)
	}
	if doc.Version < 1 {
		return nil, PriceTableInfo{}, fmt.Errorf("price table version must be >= 1, got %d", doc.Version)
	}
	if !yyyymmddRE.MatchString(strings.TrimSpace(doc.EffectiveDate)) {
		return nil, PriceTableInfo{}, fmt.Errorf("price table effective_date %q must be YYYY-MM-DD", doc.EffectiveDate)
	}
	if len(doc.Models) == 0 {
		return nil, PriceTableInfo{}, fmt.Errorf("price table has no models")
	}
	tbl := make(map[string]modelPrice, len(doc.Models))
	for name, e := range doc.Models {
		if !validProviders[e.Provider] {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: invalid provider %q (want one of anthropic/openai/google/xai/deepseek/self-hosted)", name, e.Provider)
		}
		// Rate validation rejects non-positive prices, not just negatives: a 0
		// rate (an omitted or typo'd key — KnownFields catches unknown keys but
		// not a MISSING one) would silently price tokens at $0. This guards the
		// exact "$0 table" hazard the externalization is meant to remove —
		// including a $0 self-hosted-medium, which would make the unknown-model
		// fallback itself free. input_per_m is required for every model;
		// output_per_m is required for non-combined models (combined entries bill
		// every token class at input_per_m, so their output_per_m is unused).
		if e.InputPerM <= 0 {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: input_per_m must be > 0, got %v", name, e.InputPerM)
		}
		if !e.Combined && e.OutputPerM <= 0 {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: output_per_m must be > 0 for a non-combined model, got %v", name, e.OutputPerM)
		}
		// Long-context tier (#4): the three over-tier fields are all-or-nothing.
		// A partial spec (a threshold with no premium rate, or a premium rate with
		// no threshold) would silently do nothing or mis-select — reject it so the
		// misconfiguration is a loud parse error, not a quiet mispricing. The tier
		// is meaningless on a combined self-hosted entry (single rate, no output
		// column), so reject that pairing too.
		if e.ContextThreshold != 0 || e.InputPerMOver != 0 || e.OutputPerMOver != 0 {
			if e.Combined {
				return nil, PriceTableInfo{}, fmt.Errorf("model %q: a long-context over-tier (context_threshold/input_per_m_over/output_per_m_over) is not supported on a combined entry", name)
			}
			if e.ContextThreshold <= 0 {
				return nil, PriceTableInfo{}, fmt.Errorf("model %q: context_threshold must be > 0 when an over-tier rate is set, got %d", name, e.ContextThreshold)
			}
			if e.InputPerMOver <= 0 {
				return nil, PriceTableInfo{}, fmt.Errorf("model %q: input_per_m_over must be > 0 when context_threshold is set, got %v", name, e.InputPerMOver)
			}
			if e.OutputPerMOver <= 0 {
				return nil, PriceTableInfo{}, fmt.Errorf("model %q: output_per_m_over must be > 0 when context_threshold is set, got %v", name, e.OutputPerMOver)
			}
		}
		// Per-model cache multipliers (#122). A negative multiplier is a
		// mispricing waiting to happen (a "discount" that becomes a credit), so
		// reject it fail-loud; 0 is allowed and means "use the provider default".
		// Any multiplier on a combined entry is rejected — the combined path bills
		// every class at the single rate and would silently ignore it (mirrors the
		// over-tier rejection above).
		if e.CacheReadMult < 0 {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache_read_mult must be >= 0, got %v", name, e.CacheReadMult)
		}
		if e.CacheWrite5mMult < 0 {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache_write_5m_mult must be >= 0, got %v", name, e.CacheWrite5mMult)
		}
		if e.CacheWrite1hMult < 0 {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache_write_1h_mult must be >= 0, got %v", name, e.CacheWrite1hMult)
		}
		if e.Combined && (e.CacheReadMult != 0 || e.CacheWrite5mMult != 0 || e.CacheWrite1hMult != 0) {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache multipliers (cache_read_mult/cache_write_5m_mult/cache_write_1h_mult) are not supported on a combined entry", name)
		}
		// Bake the EFFECTIVE multipliers: start from the provider default, then let
		// any explicit positive value override its class. One resolution point, so
		// ComputeCost needs no per-provider branching.
		readMult, write5mMult, write1hMult := providerDefaultMults(e.Provider)
		if e.CacheReadMult != 0 {
			readMult = e.CacheReadMult
		}
		if e.CacheWrite5mMult != 0 {
			write5mMult = e.CacheWrite5mMult
		}
		if e.CacheWrite1hMult != 0 {
			write1hMult = e.CacheWrite1hMult
		}
		// Resolve billing_mode (#300). An explicit value must be in the allowlist
		// (fail loud, like provider); omitted derives from the provider — a
		// self-hosted entry is an amortized estimate, everything else is metered
		// per token. #268 sets subscription explicitly on flat-rate host rows.
		billingMode := e.BillingMode
		if billingMode == "" {
			if e.Provider == providerSelfHosted {
				billingMode = BillingSelfHostedAmortized
			} else {
				billingMode = BillingPerToken
			}
		} else if !validBillingModes[billingMode] {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: invalid billing_mode %q (want one of per_token/subscription/self_hosted_amortized)", name, billingMode)
		}
		tbl[name] = modelPrice{
			inputPerM:        e.InputPerM,
			outputPerM:       e.OutputPerM,
			combined:         e.Combined,
			provider:         e.Provider,
			cacheReadMult:    readMult,
			cacheWrite5mMult: write5mMult,
			cacheWrite1hMult: write1hMult,
			contextThreshold: e.ContextThreshold,
			inputPerMOver:    e.InputPerMOver,
			outputPerMOver:   e.OutputPerMOver,
			billingMode:      billingMode,
		}
	}
	for _, k := range requiredSelfHostedKeys {
		if _, ok := tbl[k]; !ok {
			return nil, PriceTableInfo{}, fmt.Errorf("price table missing required fallback entry %q", k)
		}
	}
	return tbl, PriceTableInfo{Version: doc.Version, EffectiveDate: doc.EffectiveDate, ModelCount: len(tbl)}, nil
}

// LoadPriceTable parses and validates a price-table YAML file and replaces the
// active table. Intended for a `tierd --prices` override, called ONCE at startup
// before any ComputeCost read (the write-once-before-serve discipline on
// priceTable). A parse/validation error leaves the existing (embedded-default)
// table untouched and is returned so the caller fails startup loudly — never a
// silent fallback. Returns the loaded table's metadata for logging.
func LoadPriceTable(path string) (PriceTableInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PriceTableInfo{}, fmt.Errorf("read price table %s: %w", path, err)
	}
	tbl, info, err := parsePriceTable(data)
	if err != nil {
		return PriceTableInfo{}, fmt.Errorf("price table %s: %w", path, err)
	}
	priceTable = tbl
	activePriceTableInfo = info
	return info, nil
}

// ActivePriceTableInfo returns the version metadata of the currently-loaded
// table (the embedded default unless --prices overrode it).
func ActivePriceTableInfo() PriceTableInfo { return activePriceTableInfo }

// NormalizeModel strips date suffixes and lowercases a raw model string so it
// matches the price table keys.
//
//	"claude-sonnet-4-20250514" → "claude-sonnet-4"
//	"gpt-4o-2024-05-13"        → "gpt-4o"
func NormalizeModel(raw string) string {
	m := strings.ToLower(strings.TrimSpace(raw))
	m = dateVersionRE.ReplaceAllString(m, "")
	// Strip a trailing date segment like "-2024-05-13" (yyyy-mm-dd format).
	parts := strings.Split(m, "-")
	if len(parts) >= 3 {
		last := parts[len(parts)-1]
		if len(last) == 2 || len(last) == 4 {
			joined := strings.Join(parts[len(parts)-3:], "-")
			if yyyymmddRE.MatchString(joined) {
				m = strings.Join(parts[:len(parts)-3], "-")
			}
		}
	}
	return m
}

// normalizeHost lowercases and trims a serving-host string for BOTH host-qualified
// price-table keying and storage in the token_events.host column (#300). An empty
// host maps to the HostUnknown sentinel so the NOT NULL column never stores "" and
// ComputeCostHost can cheaply skip the host-qualified lookup for it.
func normalizeHost(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	if h == "" {
		return HostUnknown
	}
	return h
}

// hostQualifiedKey joins an ALREADY-normalized model and host into the price-table
// key. It is the single source of the "model@host" convention: both HostModelKey
// (the exported form #268 seeds YAML keys against) and ComputeCostHost's lookup
// route through it, so the seeded key and the lookup key cannot drift.
func hostQualifiedKey(normModel, normHost string) string {
	return normModel + hostKeySep + normHost
}

// HostModelKey builds the host-qualified price-table key for a raw (host, model)
// pair (#300): NormalizeModel(model) + "@" + lowercased host. It is the convention
// #268 authors its per-host YAML keys against, and ComputeCostHost looks up the
// same shape via hostQualifiedKey.
//
// HOST GRANULARITY: the host is the URL HOSTNAME with no port — the proxy derives
// it via url.URL.Hostname() (see internal/proxy New), so `openrouter.ai:443` and
// `openrouter.ai` price identically and a self-hosted `localhost:11434` collapses to
// `localhost` (all localhost is one self-hosted basis, the desired pricing grain).
// #268 must seed keys as bare hostnames, never host:port. The unknown-host sentinel
// never forms a key — ComputeCostHost guards that before calling here.
func HostModelKey(host, model string) string {
	return hostQualifiedKey(NormalizeModel(model), strings.ToLower(strings.TrimSpace(host)))
}

// ProviderOf returns the provider tag ("anthropic", "openai", "google", …) for
// an ALREADY-NORMALIZED model key, or "" when the key is not in the active price
// table. Callers that hold a raw model string must NormalizeModel it first.
//
// It exists so cross-source reconciliation (the Anthropic Admin poller, #138) can
// restrict a set of captured token_events to a single provider's models without
// re-implementing the provider taxonomy: the price table already carries the
// authoritative provider tag per model. An unknown key returns "" rather than a
// guess — the self-hosted fallback ComputeCost applies is a PRICING decision, not
// a provider claim, so it must not leak into a provenance filter.
//
// Thread-safety: reads priceTable under the same write-once-before-serve contract
// as ComputeCost (see priceTable's doc); no runtime reload may add a read here
// without synchronizing that seam.
func ProviderOf(norm string) string {
	if p, ok := priceTable[norm]; ok {
		return p.provider
	}
	return ""
}

// selfHostedClass returns the self-hosted size class for a model string, or "".
// Looks for known 70B+, 7B-70B, and <7B model families.
func selfHostedClass(model string) string {
	model = strings.ToLower(model)
	largePatterns := []string{"70b", "72b", "65b", "90b", "nemotron-ultra", "deepseek-r1-full"}
	for _, p := range largePatterns {
		if strings.Contains(model, p) {
			return "self-hosted-large"
		}
	}
	smallPatterns := []string{"3b", "1b", "phi-4-mini", "qwen2.5-3b", "embeddings", "reranker"}
	for _, p := range smallPatterns {
		if strings.Contains(model, p) {
			return "self-hosted-small"
		}
	}
	// Match explicit parameter counts like "7b", "8b", "32b", "49b".
	if paramCountRE.MatchString(model) {
		return "self-hosted-medium"
	}
	return ""
}

// unknownModelLogger receives WARN messages when ComputeCost falls through to
// the self-hosted-medium fallback for a real-looking model string. Stored as
// an atomic pointer so test helpers can swap it without racing concurrent
// ComputeCost readers — `var x = log.Default()` would be an unsynchronized
// two-word interface assignment, latently unsafe under `t.Parallel()`.
var unknownModelLogger atomic.Pointer[log.Logger]

// maxUnknownModelWarn bounds the distinct-model WARN dedupe set. Once this many
// never-before-seen model strings have each logged their one-time WARN, the set
// stops growing and further unknown/guessed models are priced and counted but no
// longer logged — closing the unbounded-growth / WARN-flood vector on adversarial
// or noisy input (a proxy or JSONL stream can carry attacker-varied model names,
// #286). The cap is generous: a real fleet's legitimate open-weights + minor-
// version churn is far below 1024 distinct unknowns, so a normally-operated
// process still logs every genuinely new model.
const maxUnknownModelWarn = 1024

// unknownModelSeen dedupes WARN logs so a flood of identical events for the same
// unknown model only logs once. unknownModelSeenCount tracks the set's size so
// claimUnknownModelWarn can enforce maxUnknownModelWarn without ranging the map,
// and unknownModelWarnSuppressed gates the one-time "further WARNs suppressed"
// notice. All three reset together via the test helper.
var (
	unknownModelSeen           sync.Map
	unknownModelSeenCount      atomic.Int64
	unknownModelWarnSuppressed atomic.Bool
)

// claimUnknownModelWarn reports whether the caller should emit its one-time WARN
// for norm. It returns true exactly once per distinct model — on that model's
// first sighting while the dedupe set is still under maxUnknownModelWarn — and
// false thereafter (already seen, or the set is full). When the set is full it
// logs a single process-lifetime "suppressed" notice so an operator can tell the
// silence apart from "no unknown models". The per-event counters in
// ComputeCostHost are independent of this gate, so pricing observability is never
// suppressed — only the human-readable WARN log is bounded.
//
// The count check and LoadOrStore are not one atomic step, so under heavy
// concurrent first-sightings the set can overshoot to maxUnknownModelWarn + G,
// where G is the number of goroutines racing the same threshold. G is bounded by
// the ingestion concurrency (a fixed set of proxy/JSONL/poller workers), NOT by
// attacker input — so the set size stays bounded regardless of how many distinct
// model strings an adversary streams, which is the whole point of the cap.
// Callers must apply the marker guard (empty / <>-bearing strings) BEFORE calling,
// so a synthetic placeholder never consumes a slot.
func claimUnknownModelWarn(norm string) bool {
	if _, ok := unknownModelSeen.Load(norm); ok {
		return false // already warned for this model
	}
	if unknownModelSeenCount.Load() >= maxUnknownModelWarn {
		if unknownModelWarnSuppressed.CompareAndSwap(false, true) {
			unknownModelLogger.Load().Printf(
				"WARN: unknown/guessed-model WARN log capped after %d distinct models; further never-before-seen models are still priced and counted but will not be logged individually. Add the missing models to prices.yaml (or your --prices override).",
				maxUnknownModelWarn,
			)
		}
		return false
	}
	if _, loaded := unknownModelSeen.LoadOrStore(norm, struct{}{}); loaded {
		return false // another goroutine claimed this model first
	}
	unknownModelSeenCount.Add(1)
	return true
}

func init() {
	unknownModelLogger.Store(log.Default())
	// Parse the embedded default price table. The YAML is compiled into the
	// binary and covered by tests, so a parse failure here is a build-time
	// programmer error (bad prices.yaml edit), not a runtime condition — panic
	// loudly rather than start with a $0 table.
	tbl, info, err := parsePriceTable(defaultPriceTableYAML)
	if err != nil {
		panic("store: embedded prices.yaml is invalid: " + err.Error())
	}
	priceTable = tbl
	activePriceTableInfo = info
}

// GuessPathSizeClass and GuessPathFlat are the ONLY two values ComputeCostHost
// records for the guess_path label on tier_unknown_model_events_total (#326).
// They name WHICH of the two pricing-guess branches fired — the size-class
// heuristic (a parameter count in the model string mapped to a self-hosted-*
// class) or the flat self-hosted-medium fallback (nothing matched). The set is
// FIXED and small by construction: the raw model string is NEVER used as a
// label value, so the label's cardinality is bounded at 2 and cannot explode on
// the unbounded space of upstream-controlled model names.
const (
	GuessPathSizeClass = "size_class"
	GuessPathFlat      = "flat"
)

// UnknownModelRecorder counts API calls priced at the unknown-model fallback
// rate so mispriced spend is observable (#68). Mirrors the proxy WriteRecorder
// seam (#70): a nil recorder is a no-op, which keeps internal/store off
// internal/metrics. Installed ONCE before serving via SetUnknownModelRecorder.
// Inc is variadic so the interface signature still permits a zero-label
// implementation (e.g. a test stub), while the installed serve-path recorder
// carries the bounded guess_path label value (#326).
type UnknownModelRecorder interface {
	Inc(labelValues ...string)
}

// unknownModelRecorder holds the active recorder. atomic.Pointer mirrors
// unknownModelLogger so a test can swap it without racing concurrent ComputeCost
// readers under -race.
var unknownModelRecorder atomic.Pointer[UnknownModelRecorder]

// SetUnknownModelRecorder installs (or clears, with nil) the recorder bumped on
// every unknown-model pricing fallback. Called once from `tierd serve` startup
// with the tier_unknown_model_events_total counter.
func SetUnknownModelRecorder(r UnknownModelRecorder) {
	if r == nil {
		unknownModelRecorder.Store(nil)
		return
	}
	unknownModelRecorder.Store(&r)
}

// recordUnknownModel bumps the unknown-model counter if a recorder is installed,
// tagging the increment with the guess_path label (#326) so an operator can see
// WHICH branch guessed — GuessPathSizeClass or GuessPathFlat. Unlike
// warnUnknownModel (deduped to one WARN per model), this fires on EVERY fallback
// event so the metric reflects mispriced-spend volume, not distinct models.
func recordUnknownModel(guessPath string) {
	if p := unknownModelRecorder.Load(); p != nil {
		(*p).Inc(guessPath)
	}
}

// UnknownModelCostRecorder accumulates a micro-dollar cost total. Its single
// method mirrors metrics.CounterVec.Add(delta, labelValues...), so a
// *metrics.CounterVec (or an adapter wrapping one) satisfies it directly — the
// same nil-safe seam discipline as UnknownModelRecorder keeps internal/store off
// internal/metrics (#68/#135). One interface shape serves BOTH cost counters
// (the unknown-model fallback cost and the all-events priced cost) because the
// recording contract is identical; the two SetX functions below install them
// independently. A nil recorder is a no-op (the `tierd score` / test path).
type UnknownModelCostRecorder interface {
	Add(v float64, labelValues ...string)
}

// unknownModelCostRecorder holds the recorder for micro-dollars billed at the
// unknown-model fallback rate; pricedCostRecorder holds the recorder for the
// micro-dollar cost of EVERY ComputeCost call. atomic.Pointer mirrors
// unknownModelRecorder so a test can swap either without racing concurrent
// ComputeCost readers under -race.
var (
	unknownModelCostRecorder atomic.Pointer[UnknownModelCostRecorder]
	pricedCostRecorder       atomic.Pointer[UnknownModelCostRecorder]
)

// SetUnknownModelCostRecorder installs (or clears, with nil) the recorder that
// accumulates micro-dollars billed at the unknown-model fallback rate
// (tier_unknown_model_cost_micro_total, #135). Cost-weighted counterpart to
// SetUnknownModelRecorder: the event count alone cannot tell an operator whether
// a burst of fallbacks is noise or a large share of window spend. Same
// write-once-before-serve discipline — called once from `tierd serve` startup.
func SetUnknownModelCostRecorder(r UnknownModelCostRecorder) {
	if r == nil {
		unknownModelCostRecorder.Store(nil)
		return
	}
	unknownModelCostRecorder.Store(&r)
}

// SetPricedCostRecorder installs (or clears, with nil) the recorder that
// accumulates the micro-dollar cost of EVERY ComputeCost result
// (tier_priced_cost_micro_total, #135) — the denominator against which the
// unknown-model fallback cost share is judged. Same seam and discipline as
// SetUnknownModelCostRecorder.
func SetPricedCostRecorder(r UnknownModelCostRecorder) {
	if r == nil {
		pricedCostRecorder.Store(nil)
		return
	}
	pricedCostRecorder.Store(&r)
}

// recordUnknownModelCost adds the micro-dollar fallback cost of one event to the
// unknown-model cost recorder, if installed. costMicro is passed as float64 to
// match the Add(v float64, ...) counter contract; ComputeCost feeds it the
// int64 micro-dollar result, which float64 represents exactly at these
// magnitudes. A nil recorder is a no-op.
func recordUnknownModelCost(costMicro float64) {
	if p := unknownModelCostRecorder.Load(); p != nil {
		(*p).Add(costMicro)
	}
}

// recordPricedCost adds the micro-dollar cost of one ComputeCost result to the
// priced-cost recorder, if installed. Fires on EVERY call (known or fallback),
// so it is the total-spend denominator for the fallback-share alert. A nil
// recorder is a no-op.
func recordPricedCost(costMicro float64) {
	if p := pricedCostRecorder.Load(); p != nil {
		(*p).Add(costMicro)
	}
}

// warnUnknownModel emits a one-time WARN that the given normalized model name
// was not found in the price table or any self-hosted class detector and is being
// priced by a GUESS at the self-hosted-medium reference rate. The wording says
// "guess", not "fallback": the self-hosted-medium rate is an unaudited estimate
// standing in for the real one, and framing it as a routine fallback understates
// the data_quality cost (#286). Markers like the empty string or `<synthetic>`
// (Claude Code's placeholder for non-billable internal events) are intentionally
// suppressed, and the WARN itself is bounded by claimUnknownModelWarn.
func warnUnknownModel(norm string) {
	if norm == "" || strings.ContainsAny(norm, "<>") {
		return
	}
	if !claimUnknownModelWarn(norm) {
		return
	}
	unknownModelLogger.Load().Printf(
		"WARN: model %q not in the price table; priced by a GUESS at the self-hosted-medium reference rate ($0.50/M combined), not an audited rate. Add an entry to prices.yaml (or your --prices override) for accurate cost.",
		norm,
	)
}

// warnHeuristicModel emits a one-time WARN that the given normalized model name
// had NO exact price-table entry and was priced by the size-class heuristic
// (selfHostedClass) at the named self-hosted reference class — a GUESS from a
// parameter count in the string, not an audited rate (#267). Before #267 this
// path was silent: a `…70b…` model matched self-hosted-large and priced
// correctly, but fired no WARN, so an org running open-weights could not see a
// chunk of spend was estimated.
//
// Deduped AND bounded per model through the SAME claimUnknownModelWarn gate as
// warnUnknownModel: a given model string routes down exactly one guess path (a
// heuristic match OR the flat fallback, never both), so the shared set cannot
// collide across the two, and both paths share one distinct-model cap (#286).
// The marker guard mirrors warnUnknownModel — a synthetic/placeholder string
// (empty, or containing <>) never warns even if it happens to embed a param
// count. The model name is upstream-controlled, so it is rendered with %q and
// never interpolated raw; class is one of our own self-hosted-* constants and is
// safe to print plainly.
func warnHeuristicModel(norm, class string) {
	if norm == "" || strings.ContainsAny(norm, "<>") {
		return
	}
	if !claimUnknownModelWarn(norm) {
		return
	}
	unknownModelLogger.Load().Printf(
		"WARN: model %q not in the price table; priced by a GUESS from the size-class heuristic at the %s reference rate (an estimate, not an audited rate). Add an audited entry to prices.yaml (or your --prices override) for accurate cost.",
		norm, class,
	)
}

// CostUsage describes the per-call token totals ComputeCost prices. Use the
// struct form rather than positional args so adding new token classes (e.g.
// future TTLs, batch-API flags, fast-mode flags) doesn't break every call site.
//
// CacheRead, CacheWrite5m, CacheWrite1h carry provider-specific semantics:
//   - Anthropic: CacheRead is `cache_read_input_tokens`. CacheWrite5m / CacheWrite1h
//     come from the nested `cache_creation.ephemeral_{5m,1h}_input_tokens` object.
//     Legacy entries without the nested object bucket all writes into CacheWrite5m.
//   - OpenAI: CacheRead is `prompt_tokens_details.cached_tokens`. Both write
//     buckets are 0 — OpenAI has no notion of cache writes.
//   - Google (Gemini): CacheRead is `cachedContentTokenCount` (a subset of
//     promptTokenCount, carved out by the parser). Both write buckets are 0.
//   - xAI / DeepSeek: all three cache fields are 0 today (DeepSeek publishes a
//     cache-hit rate, but the OpenAI-compatible response exposes no cached count
//     to populate CacheRead with yet).
//   - Self-hosted: cache fields are summed at the combined rate (no discount).
type CostUsage struct {
	Input        int
	Output       int
	CacheRead    int
	CacheWrite5m int
	CacheWrite1h int
}

// ComputeCost returns the cost of a single API call in integer micro-dollars
// (issue #69) using the reference price table and the per-model cache
// multipliers baked into it at parse time. Per-class costs are summed in float
// dollars from the float price table and then rounded ONCE, at the micro-dollar
// boundary, with round-half-to-
// even (see DollarsToMicro) — so each event truncates at most half a micro-dollar
// (≤ $0.0000005) with no directional bias, and all downstream SUMs are exact
// integer arithmetic.
//
// When the normalized model name has no EXACT price-table entry, it is priced at
// a self-hosted reference rate — a size-class heuristic match if a parameter count
// is present, else the flat self-hosted-medium fallback — and in BOTH cases emits
// a one-time WARN and bumps the unknown-model event/cost counters (#267), making
// the silent-estimate hazard observable. This is the structural fix for the class
// of bug where new minor versions (e.g. `claude-opus-4-8`) or open-weights models
// (e.g. `llama-3.1-70b`) ship before the table is updated and quietly bill at a
// guessed rate instead of the real one.
//
// ComputeCost is the host-agnostic form: it prices at the model-only rate, which
// is exactly ComputeCostHost with an unknown host. It exists as a separate entry
// point so callers with no host and no use for the billing basis keep a stable
// signature (#300).
//
// It DISCARDS the resolved billing_mode, so any caller that PERSISTS a
// TokenEvent should use ComputeCostHost and store the mode; otherwise the row
// silently takes normalizeBillingMode's per_token default and claims a billing
// basis it has not earned. That is how the same Codex session came to record
// self_hosted_amortized under `serve` and per_token under `ship` (#492); the
// /events re-pricer was that caller and now uses ComputeCostHost.
//
// ⚠️ NO PERSISTING CALLER MAY USE THIS. The JSONL collector and both org
// pollers did, and were fixed by #525 — that was the last of them. The ONE
// remaining caller is the one-shot #55 repricer at the top of Open()
// (store.go), which is marker-guarded, runs before any insert, and only ever
// touches pre-#300 rows whose host is the backfilled sentinel; for those the
// model-only rate is exactly correct. It UPDATEs cost_micro on existing rows
// rather than persisting a new TokenEvent, so it resolves no mode to discard.
//
// That exemption is narrow and load-bearing: it is NOT a precedent. A new
// caller that persists a TokenEvent must use ComputeCostHost and store the
// mode. TestComputeCost_NoNewPersistingCallers enforces this — if you are
// reading this comment because that test failed, the fix is to switch your
// caller to ComputeCostHost, not to add yourself to the allowlist.
//
// SCOPE, so the sentence above is not over-read: "no persisting caller uses
// ComputeCost" is NOT the same claim as "billing_mode is honest on every path".
// Producers that import a cost they never derived — /costs manual imports and the
// demo seeder — set no mode at all and take normalizeBillingMode's per_token
// default. They call nothing here, so neither this comment nor the guard says
// anything about them; a manually imported subscription cost still exports as
// per_token.
func ComputeCost(model string, u CostUsage) int64 {
	cost, _ := ComputeCostHost("", model, u)
	return cost
}

// ComputeCostHost is the host-aware pricing entry point (#300): the cost of an
// open-weights model is a property of the SERVING HOST, not the weights, so a
// host-qualified (host, model) rate — seeded by #268 — outranks the model-only
// rate, but ONLY for that host. It returns the cost in integer micro-dollars and
// the resolved billing_mode (per_token / subscription / self_hosted_amortized) so
// the caller can store the honest billing basis alongside the cost.
//
// Lookup order:
//  1. host-qualified key HostModelKey(host, model), when the host is known and #268
//     has seeded a rate for it — an audited per-host rate, priced silently;
//  2. model-only exact entry — the pre-#300 path, unchanged;
//  3. size-class heuristic / flat self-hosted-medium fallback — a GUESS, still
//     WARNed and counted exactly as before.
//
// Until #268 seeds host-qualified entries, step 1 never hits, so every existing
// cost, WARN, and counter is preserved byte-for-byte and an unknown host behaves
// identically to the old model-only pricing.
func ComputeCostHost(host, model string, u CostUsage) (int64, string) {
	norm := NormalizeModel(model)
	// Host-qualified lookup FIRST. An audited per-host rate is neither a guess nor
	// a WARN — it is the whole point of #300. Skip it for the unknown-host sentinel
	// so a host-blind producer prices exactly at the model-only rate.
	if nh := normalizeHost(host); nh != HostUnknown {
		if p, ok := priceTable[hostQualifiedKey(norm, nh)]; ok {
			total := priceCall(p, u)
			recordPricedCost(float64(total))
			return total, p.billingMode
		}
	}
	// Model-only exact lookup — but NEVER against a norm that itself contains the
	// host separator. Host-qualified entries ("model@host") share this map, and a
	// real model key never contains "@" (NormalizeModel does not add one). Because
	// `model` is UPSTREAM-controlled, a hostile response claiming model
	// "llama-3.1-70b@openrouter.ai" would otherwise hit that (likely cheap,
	// subscription-flagged) host row directly via this exact lookup — bypassing the
	// host guard above and forging a per-host rate regardless of the real target
	// (#300 review, security). Treating a norm with "@" as "not a model-only key"
	// routes it to the guessed self-hosted path below, which WARNs and counts it —
	// so the forgery attempt is surfaced, not silently under-priced.
	var (
		p     modelPrice
		exact bool
	)
	if !strings.Contains(norm, hostKeySep) {
		p, exact = priceTable[norm]
	}
	// A model with no exact table entry is GUESSED — priced at a self-hosted
	// reference rate we cannot audit. There are two guess paths and #267 makes
	// BOTH observable (a one-time-per-model WARN plus the event/cost counters
	// below), so an org running open-weights can see what share of spend is
	// estimated rather than billed at an audited rate:
	//
	//  1. size-class heuristic — a parameter count in the string (…70b…, …7b…)
	//     maps to self-hosted-large/medium/small via selfHostedClass. Before #267
	//     this path was SILENT: it priced correctly but set ok=true, so it was
	//     lumped with an exact hit and skipped every signal — the bug #267 fixes.
	//  2. flat fallback — nothing matched, so self-hosted-medium (combined, no
	//     cache discount: the safe choice when the provider is unknown).
	//
	// Only an EXACT table hit is silent — a real audited model, OR an operator
	// deliberately pricing at an explicit self-hosted-* key (that key is in the
	// table, so it lands here as exact and must NOT warn).
	guessed := !exact
	var guessPath string
	if !exact {
		if cls := selfHostedClass(norm); cls != "" {
			// Heuristic match: priced at the class rate, but still a GUESS. The
			// WARN names the class (diagnosis); the counters fire below.
			p = priceTable[cls]
			warnHeuristicModel(norm, cls)
			guessPath = GuessPathSizeClass
		} else {
			// Nothing matched — flat self-hosted-medium fallback.
			warnUnknownModel(norm)
			p = priceTable["self-hosted-medium"]
			guessPath = GuessPathFlat
		}
	}

	total := priceCall(p, u)

	// Observe spend on a SINGLE return path (#135/#267). recordPricedCost fires on
	// every call so it is the total-spend denominator; a GUESSED price (heuristic
	// OR flat fallback) additionally bumps the per-event count and the guessed
	// COST, so an operator can alert on the SHARE of window spend billed at a
	// non-audited self-hosted reference rate — an event count alone cannot say
	// whether 500 guesses are noise or half the spend.
	recordPricedCost(float64(total))
	if guessed {
		recordUnknownModel(guessPath)
		recordUnknownModelCost(float64(total))
	}
	return total, p.billingMode
}

// modelIsExactHost reports whether (host, model) resolves to an exact, audited
// entry in the active price table — i.e. NOT the size-class heuristic or the flat
// self-hosted-medium fallback that ComputeCostHost WARNs and meters as a pricing
// guess (#267). It mirrors ComputeCostHost's NOT-guessed determination step for
// step: a host-qualified entry (step 1, an audited per-host rate, #300) OR a
// model-only exact entry (step 2). The host-key separator guard matches step 2's
// forged-"model@host" rejection. A caller counting "unknown-model" spend (the
// fidelity endpoint, #236) MUST pass the row's host so an open-weights model
// priced at an audited host-qualified rate — which has no model-only entry — is
// not miscounted as unauditable. The reads are unlocked, matching ComputeCost's
// write-once-before-serve contract on priceTable.
func modelIsExactHost(host, model string) bool {
	// Delegates to premiumLookup, which reproduces this exact host-qualified →
	// model-only resolution (including the forged-"model@host" separator guard) and
	// is strictly more general (it also returns the resolved entry). Keeping the
	// resolution defined once means the security-critical guard has a single home.
	_, ok := premiumLookup(host, model)
	return ok
}

// modelIsExact is the host-blind form of modelIsExactHost — the pre-#300 model-only
// exactness test. Retained for callers that legitimately have no host (and for the
// host-blind unit test); it is exactly modelIsExactHost with the unknown-host
// sentinel, so it never consults a host-qualified entry.
func modelIsExact(model string) bool {
	return modelIsExactHost("", model)
}

// PremiumInputRateThresholdPerM is the list input rate ($/M tokens) at or above
// which a model counts as "premium" for the premium_model_share lever (#234) —
// the frontier / reasoning tier (Claude Opus & Fable, OpenAI o1 and the
// ultra-premium *-pro tiers, etc.) whose per-token rate an adopter can act on by
// routing routine work to a cheaper workhorse model. It keys off the BASE input
// rate (below any long-context over-tier), the clearest tier separator in the
// current market: at 5.0 it admits Opus (all gens, $5–15), Fable ($10), o1 ($15)
// and the $30 *-pro tiers while excluding Sonnet ($3), Haiku, and every
// Flash/mini/nano workhorse (< $3). This is a deliberate, documented cutoff, not
// a market truth — revisit it here (one place) as list prices move.
const PremiumInputRateThresholdPerM = 5.0

// premiumLookup resolves (host, model) to its price-table entry using the SAME
// host-qualified → model-only order as ComputeCostHost (#300), minus every side
// effect (no WARN, no metrics, no guess fallback) — including the "@"-in-norm
// guard that rejects a forged "model@host" from hitting a host row directly. It
// returns the resolved entry only on an exact hit; a guessed self-hosted price
// yields ok=false. This is the single home of the exact-resolution logic:
// modelIsExactHost (the #267 unknown-model exactness test) delegates to it, and
// IsPremiumModel keys the premium bit off the entry it returns.
func premiumLookup(host, model string) (modelPrice, bool) {
	norm := NormalizeModel(model)
	if nh := normalizeHost(host); nh != HostUnknown {
		if p, ok := priceTable[hostQualifiedKey(norm, nh)]; ok {
			return p, true
		}
	}
	if strings.Contains(norm, hostKeySep) {
		return modelPrice{}, false
	}
	p, exact := priceTable[norm]
	return p, exact
}

// IsPremiumModel reports whether (host, model) prices at or above
// PremiumInputRateThresholdPerM on its base input rate — the host-aware premium
// classifier behind premium_model_share (#234). It resolves the same host-aware
// order as ComputeCostHost via premiumLookup, so an open-weights model priced at
// an audited per-host rate is judged on THAT host's rate, not the weights. A
// model with no exact table entry (a guessed self-hosted fallback) is never
// premium: an unauditable low-cost estimate is not the frontier tier.
func IsPremiumModel(host, model string) bool {
	p, ok := premiumLookup(host, model)
	if !ok {
		return false
	}
	return p.inputPerM >= PremiumInputRateThresholdPerM
}

// priceCall is the pure pricing arithmetic for one API call against an already-
// resolved modelPrice: no table lookup, no fallback, no metric side effects. It
// sums per-class costs in float dollars and rounds ONCE at the micro-dollar
// boundary (see ComputeCost's contract), so the extraction is exactly cost-
// equivalent to the pre-#135 inline form. Kept separate so ComputeCost has a
// single return point at which it can record spend.
func priceCall(p modelPrice, u CostUsage) int64 {
	const perM = 1_000_000.0
	if p.combined {
		// Combined rate: every token class billed at the single rate, no
		// cache discount. This matches the "we don't know your real billing"
		// semantics of self-hosted reference rates.
		total := float64(u.Input+u.Output+u.CacheRead+u.CacheWrite5m+u.CacheWrite1h) / perM * p.inputPerM
		return DollarsToMicro(total)
	}

	// Per-model cache multipliers (#122), resolved once at parse time from the
	// provider default plus any explicit prices.yaml override. No per-provider
	// branching here — the resolution lives in parsePriceTable. When a new
	// discount is published, edit prices.yaml AND docs/reference-price-table.md
	// §7 in the same commit (per the discipline below).
	readMult, write5mMult, write1hMult := p.cacheReadMult, p.cacheWrite5mMult, p.cacheWrite1hMult

	// Long-context tier (#4): once the input-side context crosses the model's
	// threshold, the whole request re-prices at the premium input/output rates.
	// The tier is chosen by the size of the INPUT context (input + every cache
	// class) — the tokens the provider must process — not by output length, and
	// the boundary is strict (>threshold), matching Anthropic's "prompts larger
	// than 200K" wording. Cache multipliers below scale off the SELECTED input
	// rate, so a cached >200K request bills its reads at the premium base too.
	inputRate, outputRate := p.inputPerM, p.outputPerM
	if p.contextThreshold > 0 {
		if u.Input+u.CacheRead+u.CacheWrite5m+u.CacheWrite1h > p.contextThreshold {
			inputRate, outputRate = p.inputPerMOver, p.outputPerMOver
		}
	}

	in := float64(u.Input) / perM * inputRate
	read := float64(u.CacheRead) / perM * inputRate * readMult
	write5m := float64(u.CacheWrite5m) / perM * inputRate * write5mMult
	write1h := float64(u.CacheWrite1h) / perM * inputRate * write1hMult
	out := float64(u.Output) / perM * outputRate
	return DollarsToMicro(in + read + write5m + write1h + out)
}
