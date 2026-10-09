package store

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tiermetric/tier/internal/logsafe"

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
	// provider ("anthropic" / "openai" / "google" / "xai" / "deepseek" / "zai" /
	// "meta" / "self-hosted") selects the parse-time DEFAULT cache multipliers (see
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
	// (#113; operator-declared — none ships embedded), or self_hosted_amortized for
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
	providerZAI        = "zai"
	providerMeta       = "meta"
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

// providerModelPrefixes are the "<providerID>/" prefixes opencode writes in front
// of a model id ("zai-coding-plan/glm-5.3", #786). NormalizeModel strips them so
// the id matches its model-only price row.
var providerModelPrefixes = []string{"zai-coding-plan/"}

// dateVersionRE strips date/version suffixes like "-20250514" or "-preview" from model names.
var dateVersionRE = regexp.MustCompile(`-\d{8}$|-\d{6}$|-\d{4}$|-preview\d*$|-latest$`)

// yyyymmddRE matches a trailing "YYYY-MM-DD" date segment used by some provider model IDs.
var yyyymmddRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// paramCountRE captures a parameter count in billions with no leading digit or trailing letter/digit.
var paramCountRE = regexp.MustCompile(`(?i)(?:^|[^0-9])([0-9]+(?:\.[0-9]+)?)b(?:[^0-9a-z]|$)`)

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
	// entry, per_token otherwise (see parsePriceTable). #268's host rows set it to
	// per_token; subscription rows are operator-declared, never embedded.
	BillingMode string `yaml:"billing_mode"`
}

// PriceTableInfo is the public view of the active table's version metadata, for
// the caller to log at startup.
//
// 🔴 EVERY FIELD MUST STAY COMPARABLE (no slice, map or func). cmd/tierd's
// score_test.go asserts `info != (store.PriceTableInfo{})` on the no-override
// path, which is a compile error the moment an incomparable field lands here.
// That is why the #713 identity fields below are STRINGS carrying pre-encoded
// hex rather than the [32]byte digests they are computed from.
type PriceTableInfo struct {
	Version       int
	EffectiveDate string
	ModelCount    int
	// Source names WHERE this table came from: PriceSourceEmbedded for the
	// compiled-in default, or the filesystem path LoadPriceTable read. It is
	// provenance for a local operator ("which prices produced these numbers"),
	// NOT part of either hash — the same table loaded from two paths is the
	// same table.
	//
	// ⚠️ Deliberately NOT surfaced over HTTP: it is a local filesystem path and
	// the served surfaces are read by clients that have no business learning the
	// server's directory layout. `tierd score-log` (a local CLI printing to the
	// invoking user's own terminal) does surface it.
	Source string
	// TableHash identifies the RESOLVED table's CONTENT — sha256 over the
	// canonical serialization of the in-memory map, scheme-tagged
	// "tierpt1:<64 hex>". See canonicalPriceTableBytes for why this is the
	// binding identity and file_hash is not.
	TableHash string
	// FileHash is sha256 over the RAW SOURCE BYTES, scheme-tagged
	// "sha256:<64 hex>". It moves when a comment or key ordering moves and
	// TableHash does not, which is exactly what keeps the source-URL comments
	// in prices.yaml auditable without letting a comment edit re-identify the
	// prices.
	FileHash string
}

// PriceSourceEmbedded is the PriceTableInfo.Source value for the compiled-in
// default table. Exported so the printers (cmd/tierd) and this package share ONE
// spelling rather than two hand-copied literals.
const PriceSourceEmbedded = "embedded"

// Scheme tags on the two identity hashes (#713).
//
// 🔑 The tag is NOT decoration, it is the rollback seam. A downstream guard that
// refuses a mismatched table has to be able to tell "this canonicalization was
// computed by a DIFFERENT (older or newer) scheme" apart from "these prices
// disagree". Without a tag those two are the same 64 hex characters, and the
// first legitimate fix to the canonicalization below would brick every
// deployment running the previous binary. Any change to the byte layout in
// canonicalPriceTableBytes MUST bump priceTableHashScheme in the same commit.
//
// 🔴 WHAT THIS IS NOT. These are UNKEYED sha256 digests, computed by the same
// process that reports them. Against an adversary who controls the install they
// prove NOTHING — such an adversary changes the table and reports whatever hash
// they like, or patches priceTableHash to return a constant. This is not a
// signature and not an authentication mechanism.
//
// What it does detect, which is the actual job: accidental drift between two
// installs, an operator who edited the YAML on a box whose BINARY is trusted,
// and a serializer that cannot distinguish two different tables (the framing
// argument below). Where this file and its tests say "tamper-evident" or
// "forgeable", they are claims about the SERIALIZATION — that distinct tables
// cannot be made to produce equal bytes — not about the transport.
const (
	// priceTableHashScheme versions the CANONICALIZATION, not the table.
	priceTableHashScheme = "tierpt1"
	// fileHashScheme names a plain digest over untransformed bytes; there is no
	// canonicalization to version, so it is the algorithm name.
	fileHashScheme = "sha256"

	// canonicalFramesPerModel is the FIXED number of length-prefixed frames each
	// model contributes: the key plus every field of modelPrice. The injectivity
	// argument in canonicalPriceTableBytes depends on this being a constant —
	// see the warning there before changing it, and change modelPrice's field
	// count and this number together or the decode test fails loudly.
	canonicalFramesPerModel = 12

	// canonicalBytesPerModelHint pre-sizes the serialization buffer. Measured
	// against the embedded table: 208.5 bytes/model, so this clears it.
	canonicalBytesPerModelHint = 224
)

// canonicalPriceTableBytes serializes a RESOLVED price table to a deterministic
// byte string whose sha256 is the table's content identity.
//
// 🔑 WHY THE RESOLVED TABLE AND NOT THE FILE. parsePriceTable bakes the
// provider-default cache multipliers, resolved from CODE (providerDefaultMults),
// into every modelPrice. Change the Go constant anthropicReadMult from 0.10 to
// 0.15 and every cached-token cost in the system changes while prices.yaml is
// byte-identical. A hash over the file would certify "same rates" across a real
// rate change — strictly worse than no hash at all. The same asymmetry runs the
// other way: a comment-only edit to prices.yaml must NOT re-identify the table.
// Hashing the resolved map gets both.
//
// 🔴 LENGTH-PREFIXED FRAMING, NOT DELIMITER JOINING. Model keys are unvalidated
// for charset — parsePriceTable validates every VALUE and never the KEY — and
// YAML permits a quoted key containing newlines, "|", or any other separator a
// concatenating serializer might pick. Under `strings.Join(parts, "|")` the
// single-model table {"a|b": P} and a two-model table {"a": …, "b": …} can be
// crafted to produce identical bytes, so the tamper-evidence claim would be
// false by construction. Every field here is written as an 8-byte big-endian
// length followed by its bytes, which is uniquely decodable for ANY key content.
//
// The discipline is deliberately RE-IMPLEMENTED from
// internal/collector/collector.go's IdempotencyKey (~12 lines) rather than
// shared: collector imports store, so store cannot import collector without an
// import cycle. Keep the two in step by intent, not by reference.
//
// Determinism rests on three things, each of which has a mutation-tested guard
// in prices_hash_test.go:
//   - Keys are sort.Strings'ed. Go randomizes map iteration order, so an
//     unsorted walk yields a different hash on nearly every run.
//   - Floats use strconv.FormatFloat(f,'x',-1,64) — the exact, shortest
//     round-tripping hexadecimal form. It is platform-independent and, unlike
//     any 'f'/'g' precision, LOSSLESS: two rates that differ in the last ulp
//     hash differently.
//   - Negative zero is normalized to positive zero (canonPriceFloat), because
//     FormatFloat(-0.0,'x',…) is "-0x0p+00" and would false-mismatch against an
//     arithmetically identical table.
//
// 🔴 THE INJECTIVITY ARGUMENT, AND THE INVARIANT IT RESTS ON. Distinct resolved
// tables must produce distinct bytes, or the identity certifies nothing. The
// proof: a length-prefixed stream is uniquely decodable into a FRAME SEQUENCE
// for arbitrary payload bytes; each model emits exactly canonicalFramesPerModel
// frames, so the sequence regroups into records unambiguously (the model count
// is recoverable as frames/12 — which is why no model-count prefix is needed);
// keys are map-unique and sorted, so records → map is a bijection.
//
// ⚠️ That middle step is the load-bearing one and it is FRAGILE. Wrap any field
// below in a condition — `if p.contextThreshold != 0 { … }`, the sort of "skip
// the zero fields" tidy-up that looks free — and records become variable-arity,
// the stream stops being groupable, and two distinct tables CAN collide. Neither
// the field-coverage pin nor any equality test notices; only the golden vector
// reddens, and its documented response ("decide whether the canonicalization
// changed on purpose, then update the literal") is exactly the wrong one here.
// TestPriceTableHash_FramingIsUniquelyDecodable decodes the stream and pins the
// arity, which is the assertion this paragraph actually makes.
//
// The field list below is FIXED and ordered. Every field of modelPrice must
// appear; TestPriceTableHash_CoversEveryModelPriceField pins that by reflection
// so a field added later cannot silently fall outside the identity.
//
// version and effective_date are deliberately ABSENT. The question table_hash
// answers is "does version V mean the same PRICES here as it does there" — and
// folding V into the digest makes the "content unchanged, version bumped" and
// "version unchanged, content changed" cases both undetectable.
//
// Complexity: O(n log n) in the model count for the sort, O(n) hashing. Called
// once per table load (parse time), never on a pricing hot path.
func canonicalPriceTableBytes(tbl map[string]modelPrice) []byte {
	keys := make([]string, 0, len(tbl))
	for k := range tbl {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	// MEASURED, not estimated: the embedded 85-model v10 table canonicalizes to
	// 17682 bytes — 208.0 per model, of which 96 is pure framing
	// (canonicalFramesPerModel × 8). An earlier comment here claimed "~24 bytes
	// each plus the 8-byte frames" and reserved 192, which is both arithmetically
	// inconsistent with itself (32 × 12 = 384) and 1269 bytes SHORT on the 77-model
	// table it named — it regrew every time. 224 clears the real figure with
	// headroom for longer keys.
	buf := bytes.NewBuffer(make([]byte, 0, len(keys)*canonicalBytesPerModelHint))
	for _, k := range keys {
		p := tbl[k]
		writeLengthPrefixed(buf, k)
		writeLengthPrefixed(buf, canonPriceFloat(p.inputPerM))
		writeLengthPrefixed(buf, canonPriceFloat(p.outputPerM))
		writeLengthPrefixed(buf, strconv.FormatBool(p.combined))
		writeLengthPrefixed(buf, p.provider)
		writeLengthPrefixed(buf, canonPriceFloat(p.cacheReadMult))
		writeLengthPrefixed(buf, canonPriceFloat(p.cacheWrite5mMult))
		writeLengthPrefixed(buf, canonPriceFloat(p.cacheWrite1hMult))
		writeLengthPrefixed(buf, strconv.Itoa(p.contextThreshold))
		writeLengthPrefixed(buf, canonPriceFloat(p.inputPerMOver))
		writeLengthPrefixed(buf, canonPriceFloat(p.outputPerMOver))
		writeLengthPrefixed(buf, p.billingMode)
	}
	return buf.Bytes()
}

// writeLengthPrefixed writes an 8-byte big-endian length followed by s. See
// canonicalPriceTableBytes for why the framing exists.
//
// ⚠️ The parameter is *bytes.Buffer, NOT io.Writer, and that is deliberate.
// Ignoring the write errors is only safe because bytes.Buffer's Write is
// documented to always return a nil error. Under an io.Writer that safety is a
// claim about the CALLER enforced by nothing in the signature: a future caller
// passing an *os.File or an http.ResponseWriter would get a silently truncated
// digest with no error anywhere — a wrong identity reported as a right one.
// Narrowing the type makes the "cannot fail" argument type-enforced instead of
// comment-enforced. uint64(len(s)) cannot overflow or go negative: len is a
// non-negative int on every GOARCH.
func writeLengthPrefixed(buf *bytes.Buffer, s string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
	buf.Write(lenBuf[:]) //nolint:errcheck // *bytes.Buffer.Write always returns nil
	buf.WriteString(s)   //nolint:errcheck // ditto; WriteString avoids a []byte(s) copy
}

// canonPriceFloat renders a float64 in the exact, shortest round-tripping
// hexadecimal form, with negative zero normalized to positive zero.
//
// 🔴 The -0.0 normalization is not hypothetical. A table can carry a negative
// zero into modelPrice by two measured routes, because the validators that
// would reject it are all written against `!= 0` or `> 0`, and -0.0 satisfies
// neither test in the direction that would catch it:
//   - `output_per_m: -0.0` on a `combined: true` entry — output_per_m is
//     deliberately unvalidated there (a combined entry bills every class at
//     input_per_m).
//   - `input_per_m_over: -0.0` / `output_per_m_over: -0.0` with no
//     context_threshold — the all-or-nothing over-tier gate tests
//     `e.InputPerMOver != 0`, which is FALSE for -0.0, so the whole validation
//     block is skipped and the value is stored as-is.
//
// FormatFloat(-0.0,'x',-1,64) is "-0x0p+00" and FormatFloat(0.0,…) is
// "0x0p+00", so without this the two arithmetically identical tables would
// report different table_hash values — a live false-mismatch on a real YAML.
// (`cache_read_mult: -0.0` is NOT one of the routes, despite passing
// usableMult: the override gate `if e.CacheReadMult != 0` is false for -0.0, so
// the provider default is baked instead. Measured, not reasoned.)
func canonPriceFloat(f float64) string {
	if f == 0 {
		f = 0 // collapses -0.0 to +0.0; a no-op for every other value
	}
	return strconv.FormatFloat(f, 'x', -1, 64)
}

// parseEmbeddedPriceTable parses the compiled-in default and stamps it with the
// embedded Source. It is the SINGLE place that decides what
// PriceTableInfo.Source is for the zero-config path.
//
// 🔴 Why it exists rather than two lines in init(). Review measured the defect
// it removes: with the stamp inline in init(), and the test helpers that
// "restore the embedded default" ALSO assigning PriceSourceEmbedded themselves,
// the only test asserting Source read back a value a helper had just written —
// a tautology. Deleting the stamp from init() left the whole suite green while
// `tierd score-log`'s price_table.source silently became "" on the most common
// (zero-config) invocation, because cmd/tierd now forwards info.Source instead
// of hardcoding the literal. With one shared producer the test observes the
// real thing, and the restore helpers restore exactly what init() installed.
func parseEmbeddedPriceTable() (map[string]modelPrice, PriceTableInfo, error) {
	tbl, info, err := parsePriceTable(defaultPriceTableYAML)
	if err != nil {
		return nil, PriceTableInfo{}, err
	}
	info.Source = PriceSourceEmbedded
	return tbl, info, nil
}

// priceTableHash returns the scheme-tagged content identity of a resolved table.
func priceTableHash(tbl map[string]modelPrice) string {
	sum := sha256.Sum256(canonicalPriceTableBytes(tbl))
	return priceTableHashScheme + ":" + hex.EncodeToString(sum[:])
}

// priceFileHash returns the scheme-tagged digest of the RAW source bytes.
func priceFileHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fileHashScheme + ":" + hex.EncodeToString(sum[:])
}

// priceTable is the active Reference Price Table, keyed by normalized model name.
// Populated at package init from the embedded prices.yaml and optionally swapped
// ONCE, before serving, by LoadPriceTable (a `tierd serve --prices` override,
// or the same flag on another subcommand such as `score`).
//
// WRITE-ONCE-BEFORE-SERVE: every ComputeCost read happens-after that single
// startup write (a clean happens-before via the serve goroutine), so the plain
// map needs no lock. Do NOT add a runtime reload without making this access
// synchronized.
var priceTable map[string]modelPrice

// EmbeddedPriceStampFormat is the startup line every command prints for the EMBEDDED
// price table, and it is exported so the printers and the README guard share ONE
// definition rather than two hand-copies.
//
// 🔴 Why it exists. README.md pins this line as sample output, and
// TestREADMESamplePriceTableMatchesEmbedded checks it — but the first version of that
// guard compared the README against ActivePriceTableInfo() while the README actually
// quotes a `fmt.Fprintf` format string in cmd/tierd. The two were connected by nothing.
// Measured: mutating BOTH print sites from `version %d` to `v%d` changed what the
// binary prints, left the README stale, and produced ZERO additional test failures
// across the whole suite. The guard's own comment claimed it "asks the program"; it
// asked a struct. Sharing the format makes drift a compile-coupled failure.
//
// internal/store cannot import cmd/tierd, so the constant lives on this side of the
// dependency edge — where the values it formats already live.
const EmbeddedPriceStampFormat = "price table: embedded default (version %d, %s, %d models)"

// PriceIdentityStampFormat is the SECOND startup line every command prints —
// the content identity of whichever table the line above described.
//
// 🔴 It is a SEPARATE line on purpose. EmbeddedPriceStampFormat is pinned
// verbatim by TestREADMESamplePriceTableMatchesEmbedded against a sample block
// in the public README; widening it would have made a hash the README cannot
// hold (it changes with every prices.yaml edit) part of that pinned line, and
// the guard would then fail on every legitimate price change. Adding a line
// leaves the pinned one untouched.
//
// Ordered table_hash first because that is the identity a comparison acts on;
// file_hash is the auditing companion. scripts/seam-exercise.sh pins the
// tierpt1 shape of the first field OUTSIDE `go test`.
const PriceIdentityStampFormat = "price table identity: table_hash=%s file_hash=%s"

// activePriceTableInfo describes the currently-loaded table (logged at startup).
var activePriceTableInfo PriceTableInfo

// embeddedPriceTableInfo describes the COMPILED-IN default table and is written
// exactly once, by init(). Unlike activePriceTableInfo — which LoadPriceTable
// replaces and four test helpers in this package swap — this one never moves, so
// it is a fixed point LoadPriceTable's #714 layer-1 collision guard can compare
// an override against.
//
// Keeping it separate is what makes the guard mean anything: comparing an
// override to activePriceTableInfo would compare it to whatever was loaded LAST
// (possibly another override), so a second `--prices` file reusing the first
// one's version would be measured against the wrong baseline.
var embeddedPriceTableInfo PriceTableInfo

// embeddedPriceTable is the COMPILED-IN default table, written exactly once by
// init() and never mutated. LoadPriceTable compares an override's billing modes
// against it (#921); like embeddedPriceTableInfo it must not follow priceTable,
// which a second override or a test helper can replace.
var embeddedPriceTable map[string]modelPrice

// validProviders is the allowlist a loaded table's provider fields must match —
// the same set providerDefaultMults resolves at parse time. A typo'd provider
// would silently get the 1.0× default multipliers, so parsing rejects it.
var validProviders = map[string]bool{
	providerAnthropic: true, providerOpenAI: true, providerGoogle: true,
	providerXAI: true, providerDeepSeek: true, providerZAI: true, providerMeta: true,
	providerSelfHosted: true,
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
// write buckets stay 0 in the parser, so the 1.0× default is inert). Z.ai
// publishes no per-token cache-write rate (its "limited-time free" line is cache
// STORAGE, not writes), so zai writes default to 1.0× input — the conservative
// choice — and every zai row sets its own read multiplier. Meta publishes no
// cache-write rate either: cached_tokens is "a subset of your input tokens",
// billed "at a reduced rate compared to uncached input", with no write step
// (dev.meta.ai/docs/prompt-caching, read 2026-09-27), so meta writes are 1.0×
// input and every meta row sets its own read multiplier.
// Others: no documented cache discount today; treat as 1.0×.
const (
	anthropicWrite5mMult = 1.25
	anthropicWrite1hMult = 2.00
	anthropicReadMult    = 0.10
	openAIReadMult       = 0.50
)

// usableRate reports whether a per-million-token price is a usable money-shaped
// number: strictly positive, FINITE, and inside the same magnitude bound every
// other money entry point in this system enforces (MaxCostUSD — see money.go).
//
// The finiteness half is the part that is easy to miss, and the reason this is a
// named predicate rather than an inline `> 0`. YAML decodes `.inf` and `.nan`
// straight into a float64, and the old `<= 0` guard caught NEITHER:
//
//   - `.nan` PASSES `<= 0` (every ordered comparison with NaN is false), and a
//     NaN rate makes ComputeCost's product NaN, which DollarsToMicro pins to 0 —
//     every call on that route then costs $0. That is precisely the "$0 table"
//     hazard the guard below says it exists to prevent, walking through it.
//   - `.inf` (and any absurd magnitude like 1e300) saturates cost_micro to
//     MaxInt64, and two such rows overflow any SUM(cost_micro) aggregate.
//
// Both comparisons are false for NaN, so `f > 0 && f <= MaxCostUSD` rejects the
// whole non-finite family without a separate math.IsNaN/IsInf call; it is spelled
// out here because the omission, not the arithmetic, was the bug.
func usableRate(f float64) bool { return f > 0 && f <= MaxCostUSD }

// usableMult is usableRate for a cache multiplier, where 0 is legal and means
// "inherit the provider default" (see providerDefaultMults). The old guard was
// `>= 0`, which NaN also fails to trip — a NaN multiplier prices every cached
// token at $0 by the same route as a NaN rate, so the multipliers are bounded on
// the same terms rather than being the loose half of the pair.
func usableMult(f float64) bool { return f >= 0 && f <= MaxCostUSD }

// providerDefaultMults returns the (read, write5m, write1h) cache multipliers a
// model of the given provider bills when it carries no explicit override in
// prices.yaml.
//
// ⚠️ The self-hosted 1.0× defaults ARE live on the pricing path — this comment
// used to say self-hosted entries "never reach this via ComputeCost (the combined
// path bills a single rate)", which is false and was false when written. Only the
// three self-hosted-{large,medium,small} fallbacks are `combined`; the embedded
// table carries 17 NON-combined provider: self-hosted rows (measured 2026-08-28 —
// the #268 host-qualified open-weights rates), and every operator override for a
// subscription/open-weights host adds more. The count will drift; that the path is
// LIVE is pinned by TestSelfHostedDefaults_ArePricedNotBypassed. Those take the per-class path in priceCall, so a cached read or
// cache write on them bills at the multiplier resolved here. A row that means to
// price cached reads at a discount MUST set cache_read_mult explicitly — inheriting
// 1.0× bills a cached read at the full input rate, which on a cache-heavy route is
// a multi-fold overstatement, not a rounding error.
func providerDefaultMults(provider string) (read, write5m, write1h float64) {
	switch provider {
	case providerAnthropic:
		return anthropicReadMult, anthropicWrite5mMult, anthropicWrite1hMult
	case providerOpenAI:
		return openAIReadMult, 1.0, 1.0
	default:
		// google / xai / deepseek / zai / meta / self-hosted: no documented
		// discount. zai and meta publish no cache-write rate, so their writes bill
		// like input.
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
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: invalid provider %q (want one of anthropic/openai/google/xai/deepseek/zai/meta/self-hosted)", name, e.Provider)
		}
		// Rate validation rejects non-positive prices, not just negatives: a 0
		// rate (an omitted or typo'd key — KnownFields catches unknown keys but
		// not a MISSING one) would silently price tokens at $0. This guards the
		// exact "$0 table" hazard the externalization is meant to remove —
		// including a $0 self-hosted-medium, which would make the unknown-model
		// fallback itself free. input_per_m is required for every model;
		// output_per_m is required for non-combined models (combined entries bill
		// every token class at input_per_m, so their output_per_m is unused).
		//
		// usableRate, not `> 0`: the bound is finite AND <= MaxCostUSD, because
		// `.nan` slips past a bare positivity test and prices the route at $0 —
		// re-opening the very hazard this paragraph describes. See usableRate.
		// #113 is what makes this load-bearing rather than hygiene: no
		// subscription-priced row ships in the embedded table, so the comparable
		// rate for a flat-fee route is ALWAYS hand-authored, and it lands in the
		// TIER denominator.
		if !usableRate(e.InputPerM) {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: input_per_m must be > 0, finite, and <= %g, got %v", name, MaxCostUSD, e.InputPerM)
		}
		if !e.Combined && !usableRate(e.OutputPerM) {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: output_per_m must be > 0, finite, and <= %g for a non-combined model, got %v", name, MaxCostUSD, e.OutputPerM)
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
			// Same usableRate bound as the base rates: the over-tier IS a rate, and
			// a long-context request re-prices at it, so a `.nan` here is a $0 route
			// for exactly the largest calls.
			if !usableRate(e.InputPerMOver) {
				return nil, PriceTableInfo{}, fmt.Errorf("model %q: input_per_m_over must be > 0, finite, and <= %g when context_threshold is set, got %v", name, MaxCostUSD, e.InputPerMOver)
			}
			if !usableRate(e.OutputPerMOver) {
				return nil, PriceTableInfo{}, fmt.Errorf("model %q: output_per_m_over must be > 0, finite, and <= %g when context_threshold is set, got %v", name, MaxCostUSD, e.OutputPerMOver)
			}
		}
		// Per-model cache multipliers (#122). A negative multiplier is a
		// mispricing waiting to happen (a "discount" that becomes a credit), so
		// reject it fail-loud; 0 is allowed and means "use the provider default".
		// Any multiplier on a combined entry is rejected — the combined path bills
		// every class at the single rate and would silently ignore it (mirrors the
		// over-tier rejection above).
		//
		// usableMult, not `< 0`: a multiplier is half of a money product, so it is
		// bounded on the same terms as the rate it multiplies. `.nan` passes a bare
		// `>= 0` and zeroes every cached token's cost.
		if !usableMult(e.CacheReadMult) {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache_read_mult must be >= 0, finite, and <= %g, got %v", name, MaxCostUSD, e.CacheReadMult)
		}
		if !usableMult(e.CacheWrite5mMult) {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache_write_5m_mult must be >= 0, finite, and <= %g, got %v", name, MaxCostUSD, e.CacheWrite5mMult)
		}
		if !usableMult(e.CacheWrite1hMult) {
			return nil, PriceTableInfo{}, fmt.Errorf("model %q: cache_write_1h_mult must be >= 0, finite, and <= %g, got %v", name, MaxCostUSD, e.CacheWrite1hMult)
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
		// per token. Only operator overrides set subscription; none ships embedded.
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
	// Content identity (#713). Computed HERE, after every validation and after
	// the provider defaults have been baked into tbl, so table_hash is a digest
	// of exactly the bytes ComputeCost will price with. Source is left to the
	// caller (init / LoadPriceTable) — this function has no idea where `data`
	// came from.
	return tbl, PriceTableInfo{
		Version:       doc.Version,
		EffectiveDate: doc.EffectiveDate,
		ModelCount:    len(tbl),
		TableHash:     priceTableHash(tbl),
		FileHash:      priceFileHash(data),
	}, nil
}

// LoadPriceTable parses and validates a price-table YAML file and replaces the
// active table. Intended for a `--prices` override (`tierd serve --prices`, or
// the same flag on another subcommand such as `score`), called ONCE at startup
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
	// Record WHERE this table came from (#713). The path is local provenance for
	// a CLI operator; it is not part of either hash and is not served over HTTP.
	info.Source = path
	// Layer 1 of the #714 version-collision guard, and it runs BEFORE the two
	// assignments below so a refused override leaves the active table untouched
	// — the same never-a-silent-fallback discipline a parse error gets.
	//
	// 🔴 IT LIVES HERE, IN LoadPriceTable, NOT IN cmd/tierd's loadPricesOverride
	// WRAPPER, AND THAT IS MEASURED, NOT STYLISTIC. `tierd score-log` calls
	// store.LoadPriceTable DIRECTLY (cmd/tierd/scorelog.go), bypassing that
	// wrapper entirely; a guard in the wrapper would leave score-log — the
	// command whose whole output is a priced number with a table_hash stamp on
	// it — completely unguarded. Every caller of the load path gets it here.
	//
	// ⚠️ THIS IS A DELIBERATE COMPAT BREAK (#714). An override that reuses the
	// embedded `version:` with different rates was previously accepted and is now
	// a startup failure. That is the intended fail-closed posture: such a table
	// makes one integer denote two different price tables, and the rows stamped
	// under each are indistinguishable forever. See docs/reference-price-table.md.
	if err := checkPriceTableVersionCollision(info); err != nil {
		return PriceTableInfo{}, err
	}
	// #921: WARN, never refuse, when the override relabels a built-in per-token
	// route. Here for the same every-caller reason as the collision guard.
	warnBillingModeChanges(billingModeChanges(embeddedPriceTable, tbl))
	priceTable = tbl
	activePriceTableInfo = info
	return info, nil
}

// checkPriceTableVersionCollision refuses an override that declares the EMBEDDED
// table's version number while resolving to different rates (#714).
//
// Scope, precisely: this compares against the table compiled into THIS BINARY,
// which is the one collision that needs no database to detect — and therefore
// the only one a store-less command can be guarded against. Two different
// override files sharing a version, or an override edited between two boots, are
// invisible here and are caught by layer 2 at Open (recordPriceTableIdentity).
//
// An override that reuses the embedded version with IDENTICAL resolved content
// passes: it is the same table, said twice. Only table_hash is compared, for the
// reasons on recordPriceTableIdentity — a comment-only edit moves file_hash and
// must not fail startup.
func checkPriceTableVersionCollision(info PriceTableInfo) error {
	emb := embeddedPriceTableInfo
	// A blank embedded hash means init() has not run or was subverted; there is
	// nothing to compare against, so there is no collision to report. This cannot
	// happen for a real caller (init panics rather than leave the table unset).
	if emb.TableHash == "" || info.Version != emb.Version || info.TableHash == emb.TableHash {
		return nil
	}
	return fmt.Errorf(
		"price table %s declares version %d, which is this binary's EMBEDDED price table version, but its table_hash %s does not match the embedded table_hash %s "+
			"— two different price tables would share one version number, and every row stamped price_version=%d under each is indistinguishable forever. "+
			"Bump 'version:' in %s to a number the embedded table does not use (overrides conventionally start at 1000). There is deliberately no flag that accepts the collision",
		logsafe.Str(info.Source), info.Version, logsafe.Str(info.TableHash), logsafe.Str(emb.TableHash),
		info.Version, logsafe.Str(info.Source),
	)
}

// billingModeChange is one price-table key that the embedded table resolves to
// per_token and a --prices override resolves to a different billing_mode (#921,
// R-2026-09-28-12).
type billingModeChange struct {
	Model        string // the key's model part (before "@")
	Host         string // the key's host part; "" for a model-only key
	EmbeddedMode string
	OverrideMode string
}

// billingModeChangeFix is the remedy every billingModeChange WARN carries. It
// matches the #921 CHANGELOG upgrader note: --prices REPLACES the whole table,
// so removing only the offending row would leave the model priced by a guess.
// It must also fit the embedded host-only rows (e.g. openrouter.ai), which have
// no model-only row to keep.
const billingModeChangeFix = "if unintended, rebuild the override from the embedded table (internal/store/prices.yaml) with a fresh version:, restoring this model's embedded row(s) unchanged and adding no <model>@<host> row the embedded table lacks; dropping --prices also works but discards every other row of the override"

// resolveTableKey returns the entry tbl prices a key with, in ComputeCostHost's
// order: the exact key, then — for a "model@host" key — the model-only row.
func resolveTableKey(tbl map[string]modelPrice, key string) (modelPrice, bool) {
	if p, ok := tbl[key]; ok {
		return p, true
	}
	if model, _, ok := strings.Cut(key, hostKeySep); ok {
		p, ok := tbl[model]
		return p, ok
	}
	return modelPrice{}, false
}

// billingModeChanges returns, sorted by model then host, every key of either
// table that embedded resolves to per_token and override resolves to another
// billing_mode. A host-qualified override row is compared against the embedded
// row it outranks, so `glm-5.3@zai-coding-plan: subscription` is a change against
// the embedded model-only glm-5.3 row. A key the override does not resolve at all
// (a dropped model) is not a change: it is priced by a guess, which the
// unknown-model WARN reports. A "model@host" key ComputeCostHost can never look
// up — its host part empty, HostUnknown, or not in normalizeHost form — is
// skipped: it prices nothing. Pure: no logging, no reads of package state.
func billingModeChanges(embedded, override map[string]modelPrice) []billingModeChange {
	keys := make(map[string]struct{}, len(embedded)+len(override))
	for k := range embedded {
		keys[k] = struct{}{}
	}
	for k := range override {
		keys[k] = struct{}{}
	}
	var out []billingModeChange
	for k := range keys {
		if _, host, ok := strings.Cut(k, hostKeySep); ok && (host == HostUnknown || normalizeHost(host) != host) {
			continue // unreachable: normalizeHost("") is HostUnknown, never ""
		}
		emb, ok := resolveTableKey(embedded, k)
		if !ok || emb.billingMode != BillingPerToken {
			continue
		}
		ov, ok := resolveTableKey(override, k)
		if !ok || ov.billingMode == BillingPerToken {
			continue
		}
		model, host, _ := strings.Cut(k, hostKeySep)
		out = append(out, billingModeChange{
			Model: model, Host: host,
			EmbeddedMode: emb.billingMode, OverrideMode: ov.billingMode,
		})
	}
	slices.SortFunc(out, func(a, b billingModeChange) int {
		if c := strings.Compare(a.Model, b.Model); c != 0 {
			return c
		}
		return strings.Compare(a.Host, b.Host)
	})
	return out
}

// warnBillingModeChanges logs one WARN per change, through the package's one
// slog WARN sink (unknownModelLog: resolved fresh, test-overridable). Model and
// host come from the
// operator's YAML keys, so both go through logsafe.Str; the two modes are
// allowlisted constants (validBillingModes) and need no barrier.
func warnBillingModeChanges(changes []billingModeChange) {
	for _, c := range changes {
		unknownModelLog().Warn(
			"a --prices override gives a built-in per-token model a different billing mode; this model's spend will carry the override's billing_mode, not per_token",
			"model", logsafe.Str(c.Model),
			"host", logsafe.Str(c.Host),
			"embedded_billing_mode", c.EmbeddedMode,
			"override_billing_mode", c.OverrideMode,
			"fix", billingModeChangeFix,
		)
	}
}

// ActivePriceTableInfo returns the version metadata of the currently-loaded
// table (the embedded default unless --prices overrode it).
func ActivePriceTableInfo() PriceTableInfo { return activePriceTableInfo }

// NormalizeModel strips date suffixes and lowercases a raw model string so it
// matches the price table keys.
//
//	"claude-sonnet-4-20250514" → "claude-sonnet-4"
//	"gpt-4o-2024-05-13"        → "gpt-4o"
//	"zai-coding-plan/glm-5.3"  → "glm-5.3"
//
// Only the known opencode provider prefixes in providerModelPrefixes are
// stripped, never any "x/" prefix: host-qualified open-weights keys such as
// "meta-llama/llama-3.3-70b-instruct@openrouter.ai" carry a slash of their own.
func NormalizeModel(raw string) string {
	m := strings.ToLower(strings.TrimSpace(raw))
	for _, prefix := range providerModelPrefixes {
		m = strings.TrimPrefix(m, prefix)
	}
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
	largePatterns := []string{"nemotron-ultra", "deepseek-r1-full"}
	for _, p := range largePatterns {
		if strings.Contains(model, p) {
			return "self-hosted-large"
		}
	}
	smallPatterns := []string{"phi-4-mini", "embeddings", "reranker"}
	for _, p := range smallPatterns {
		if strings.Contains(model, p) {
			return "self-hosted-small"
		}
	}
	if match := paramCountRE.FindStringSubmatch(model); match != nil {
		if count, err := strconv.ParseFloat(match[1], 64); err == nil {
			switch {
			case count >= 70:
				return "self-hosted-large"
			case count >= 7:
				return "self-hosted-medium"
			default:
				return "self-hosted-small"
			}
		}
	}
	return ""
}

// unknownModelLogger is an OPTIONAL override sink for the unknown/guessed-model
// WARNs. nil — the default, and the value in every non-test process — means
// "resolve slog.Default() at call time"; see unknownModelLog.
//
// Stored as an atomic pointer so test helpers can swap it without racing
// concurrent ComputeCost readers — a plain `var x = slog.Default()` would be an
// unsynchronized two-word assignment, latently unsafe under `t.Parallel()`.
//
// 🔴 THE POINTER IS NIL BY DEFAULT ON PURPOSE, AND THAT IS THE FIX FOR #689.
// The previous form was `atomic.Pointer[log.Logger]` seeded in init() with
// `log.Default()`. `slog.SetDefault` (cmd/tierd/main.go, right after flag parse)
// installs `log.SetOutput(&handlerWriter{...})` on the stdlib log package, and
// handlerWriter.Write drops the record when the slog handler is not enabled at
// the bridge level — returning n=0 with a NIL ERROR, so nothing anywhere fails:
//
//	level := w.level.Level()
//	if !w.h.Enabled(context.Background(), level) { return 0, nil }
//
// That bridge level is slog's package-level `logLoggerLevel` LevelVar, whose
// zero value is Info. It is NOT immutable — `slog.SetLogLoggerLevel` exists to
// change it, and its doc covers the post-SetDefault case explicitly. Nothing in
// this tree calls it (grep: 0 hits), so Info is what tier actually ran with, and
// every one of these WARNs was rendered at INFO severity and DISCARDED the
// moment an operator ran `tierd serve --log-level warn` — exactly the setting chosen
// in order to see warnings.
//
// ⚠️ So `slog.SetLogLoggerLevel(lvl)` beside main's SetDefault WAS a third
// option, absent from the issue's list: it would have made the old plain-log
// path track --log-level. It is not the one taken, and the reason is not that it
// could not work — routing through slog.Warn makes the severity REAL (a WARN
// that a severity-keyed alert can see) and leaves one logging system instead of
// two coupled by a global. Resolving Default() lazily is what makes the process
// pick up the logger main installs AFTER this package's init has run.
var unknownModelLogger atomic.Pointer[slog.Logger]

// unknownModelLog returns the sink for the unknown/guessed-model WARNs: the
// test override if one is installed, otherwise the process default resolved
// FRESH on every call.
//
// The freshness is load-bearing. Capturing slog.Default() once at init time
// would freeze the pre-SetDefault logger — whose handler is the stdlib-log
// bridge this bug is about — and reintroduce #689 through the back door.
func unknownModelLog() *slog.Logger {
	if l := unknownModelLogger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

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
			// maxUnknownModelWarn is our own compile-time constant, so it
			// needs no logsafe barrier before becoming an attribute.
			unknownModelLog().Warn(
				"unknown/guessed-model WARN log capped; further never-before-seen models are still priced and counted but will not be logged individually. Add the missing models to prices.yaml (or your --prices override).",
				"cap", maxUnknownModelWarn,
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
	// unknownModelLogger is deliberately left NIL here — see its doc comment.
	// Seeding it at init time froze a logger built before main's
	// slog.SetDefault, which is the whole of #689.
	//
	// Parse the embedded default price table. The YAML is compiled into the
	// binary and covered by tests, so a parse failure here is a build-time
	// programmer error (bad prices.yaml edit), not a runtime condition — panic
	// loudly rather than start with a $0 table.
	tbl, info, err := parseEmbeddedPriceTable()
	if err != nil {
		panic("store: embedded prices.yaml is invalid: " + err.Error())
	}
	priceTable = tbl
	activePriceTableInfo = info
	embeddedPriceTableInfo = info
	embeddedPriceTable = tbl
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
	// logsafe.Str, and the difference here is the LENGTH CAP, not the escaping.
	// See warnHeuristicModel for the full reasoning; both warnings share the one
	// sink and must share the one barrier. slog escapes but does NOT cap, so
	// moving to slog.Warn (#689) does not retire this call.
	//
	// The model is a slog ATTRIBUTE, not interpolated into the message, matching
	// the 49 other logsafe.Str-as-attr sites in the tree — including
	// internal/collector/clamp.go:95, which logs this very field the same way.
	// Two properties come with it: `msg` stays CONSTANT (an interpolated model
	// name would give it unbounded, upstream-controlled cardinality, which is a
	// smaller echo of the flood #286 exists to bound), and `model` stays a
	// queryable field rather than prose an operator has to regex out.
	//
	// The cosmetic cost is real and accepted: logsafe.Str already returns a
	// %q-quoted value, so the handler quotes it again — `model="\"acme-llm-x\""`.
	// That is the tree's existing convention, and the CodeQL go/log-injection
	// credit is unaffected either way, because it attaches to the result of
	// logsafe's OWN format call, not to where that result is subsequently used.
	unknownModelLog().Warn(
		"model not in the price table; priced by a GUESS at the self-hosted-medium reference rate, not an audited rate. Add an entry to prices.yaml (or your --prices override) for accurate cost.",
		"model", logsafe.Str(norm),
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
// count. class is one of our own self-hosted-* constants and is safe to print
// plainly.
//
// 🔴 THE MODEL NAME GOES THROUGH logsafe.Str, AND THE REASON IS THE CAP, NOT THE
// ESCAPING (#321 review, 2026-08-04). An earlier revision of this comment said
// the name "is rendered with %q and never interpolated raw" and treated that as
// sufficient. It is not: while %q does escape CR/LF, nothing bounded the LENGTH.
// An upstream model string of a megabyte produced a megabyte log record.
//
// ⚠️ THE REST OF THAT COMMENT WAS FOLKLORE AND IS DELETED (#689). It said
// `prices.go`'s plain `"log"` import was "the ONLY one in any non-test file, so
// this sink does NOT get slog's handling". The second clause was exactly
// backwards: `slog.SetDefault` REDIRECTS the stdlib log package through the slog
// handler, and handlerWriter.Write silently drops the bytes when that handler is
// not enabled at INFO — so this sink got slog's LEVEL FILTER while getting none
// of slog's severity. Both warnings now go through slog.Warn directly.
//
// logsafe.Str survives that move unchanged: slog escapes but never CAPS, so the
// length bound below is still this call's own responsibility.
//
// The dedup gate above bounds how OFTEN this fires per distinct model; logsafe
// bounds how BIG each one is. Neither substitutes for the other: #286's cap is
// on the number of distinct models, so a single model with a huge name floods
// through it unimpeded.
func warnHeuristicModel(norm, class string) {
	if norm == "" || strings.ContainsAny(norm, "<>") {
		return
	}
	if !claimUnknownModelWarn(norm) {
		return
	}
	// Attributes, not interpolation — see warnUnknownModel for the full rationale.
	// class is one of our own self-hosted-* constants, so it needs no barrier.
	unknownModelLog().Warn(
		"model not in the price table; priced by a GUESS from the size-class heuristic at a self-hosted reference rate (an estimate, not an audited rate). Add an audited entry to prices.yaml (or your --prices override) for accurate cost.",
		"model", logsafe.Str(norm), "class", class,
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

// MaxTokenCount is enforced at the /api/v1/events boundary and by the codexrollout
// parser for cumulative counts. 1e12 leaves ample headroom for real sessions while
// keeping token sums and priced costs at reference rates well below int64 overflow.
// Priced costs are bounded at save time (#1082).
const MaxTokenCount = 1_000_000_000_000

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

// IsAuditedRate is the EXPORTED form of modelIsExactHost, for callers outside
// this package that need the real audited-vs-guessed signal ComputeCostHost
// computes internally (as `exact`/`guessed`) but does not return — only the
// resolved billing_mode. billing_mode is NOT a substitute for this (#465
// review finding): a self-hosted entry's billing_mode is always
// self_hosted_amortized whether it was reached by an EXACT operator-provided
// key or by the size-class GUESS/flat fallback, and a --prices override
// could in principle set billing_mode: per_token on the very fallback key
// ComputeCostHost guesses through. A caller that reports "this cost is
// audited" (e.g. `tierd score-log`, #465) must call this, not compare
// billing_mode against BillingSelfHostedAmortized.
//
// True means (host, model) resolved to a real price-table entry — a
// host-qualified rate (#300) or a model-only rate — with no WARN, no
// size-class heuristic, no flat fallback. False means ComputeCostHost priced
// it as a GUESS. host="" is the host-blind form (see ComputeCost).
func IsAuditedRate(host, model string) bool {
	return modelIsExactHost(host, model)
}

// PremiumInputRateThresholdPerM is the list input rate ($/M tokens) at or above
// which a model counts as "premium" for the premium_model_share lever (#234) —
// the frontier / reasoning tier (Claude Opus & Fable, OpenAI o1 and the
// ultra-premium *-pro tiers, etc.) whose per-token rate an adopter can act on by
// routing routine work to a cheaper workhorse model. It keys off the BASE input
// rate (below any long-context over-tier), the clearest tier separator in the
// current market: at 4.0 it admits Opus (all gens, $4–15 — Opus 5.5 is $4, #786),
// Fable ($10), o1 ($15) and the $30 *-pro tiers while excluding Sonnet ($2–3),
// Haiku, and every Flash/mini/nano workhorse (< $3). Lowered from 5.0 in v10: no
// other row prices in [$4, $5), so Opus 5.5 is the only row it reclassifies. This
// is a deliberate, documented cutoff, not a market truth — revisit it here (one
// place) as list prices move.
const PremiumInputRateThresholdPerM = 4.0

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
		total := (float64(u.Input) + float64(u.Output) + float64(u.CacheRead) + float64(u.CacheWrite5m) + float64(u.CacheWrite1h)) / perM * p.inputPerM
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
		if float64(u.Input)+float64(u.CacheRead)+float64(u.CacheWrite5m)+float64(u.CacheWrite1h) > float64(p.contextThreshold) {
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
