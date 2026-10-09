package store

import (
	"bytes"
	"encoding/binary"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file guards the price-table content identity (#713): table_hash over the
// RESOLVED in-memory table, file_hash over the raw source bytes.
//
// The through-line of every test here is that a content hash is trivially
// FALSE-GREENABLE. A function returning a constant satisfies every "these two
// are equal" arm ever written for it, so each equality arm below is paired, in
// the SAME test function, with an inequality arm that a constant cannot pass.
// Where a test compares two inputs, it also asserts the inputs actually differ.

// hashTestBaseModels is the three self-hosted fallback keys parsePriceTable
// requires in every table. Kept as a fragment so each case below states only
// the model it is actually about.
const hashTestBaseModels = `  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// mustParse parses a price-table YAML document and fails the test on error.
func mustParse(t *testing.T, doc string) (map[string]modelPrice, PriceTableInfo) {
	t.Helper()
	tbl, info, err := parsePriceTable([]byte(doc))
	if err != nil {
		t.Fatalf("parsePriceTable: %v\n--- document ---\n%s", err, doc)
	}
	return tbl, info
}

// goldenVectorYAML is a FROZEN four-model document. It must never be edited:
// the point of the golden vector is that its hash is a constant of the
// canonicalization scheme, so changing the input silently restores the drift
// this test exists to catch.
const goldenVectorYAML = `version: 1
effective_date: "2026-01-01"
models:
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
  golden-model: {input_per_m: 3, output_per_m: 15, provider: anthropic, context_threshold: 200000, input_per_m_over: 6, output_per_m_over: 22.5, billing_mode: per_token}
`

// TestPriceTableHash_GoldenVector is the anti-drift anchor.
//
// 🔴 The expected value is a HARDCODED HEX LITERAL, deliberately not recomputed
// from the implementation. Every other test in this file compares one run of the
// hasher against another run of the same hasher, so a change to the field order,
// the float format, the framing or the scheme tag keeps all of them green while
// silently re-identifying every price table in the fleet. This is the one test
// that notices — and when it goes red the correct response is NOT to paste in
// the new value, it is to decide whether the canonicalization changed on
// purpose and, if it did, to bump priceTableHashScheme in the same commit.
//
// The tag is half the anchor. A downstream guard comparing two tables has to
// distinguish "computed by a different scheme version" from "these two disagree
// about prices"; without the tag both are 64 anonymous hex characters, and the
// first legitimate fix here would brick every deployment on the older binary.
//
// 🔴 THIS TEST IS THE ONLY THING STANDING BETWEEN US AND A SILENT FORMAT CHANGE.
// Measured: swapping the float encoding from 'x' to 'g' is killed by THIS ARM
// AND NOTHING ELSE in the entire suite — because 'g' with precision -1 is also
// lossless and also platform-independent, so every equality, inequality and
// float-edge arm still passes. (The lossy 'f',2 variant IS caught elsewhere, by
// TestPriceTableHash_FloatEdges; the dangerous mutation is the one that stays
// correct-looking.) A change that re-encodes every price table in the fleet
// would otherwise ship green. Do not delete this test, and do not "refresh" the
// literal without reading the two cases in the failure message below.
func TestPriceTableHash_GoldenVector(t *testing.T) {
	const wantTableHash = "tierpt1:47eb4ac19f973956539b7178a6840939f2838868cea8b972fa89951f012feaac"

	_, info := mustParse(t, goldenVectorYAML)
	if info.TableHash != wantTableHash {
		t.Errorf("golden vector table_hash drifted.\n  got:  %s\n  want: %s\n"+
			"TWO different changes land here, and they need opposite responses.\n"+
			"  (1) The CANONICALIZATION changed — field order, float format, framing, "+
			"negative-zero handling or the scheme tag in canonicalPriceTableBytes. If that "+
			"was deliberate, bump priceTableHashScheme in the SAME commit so a downstream "+
			"guard can tell a re-scheme from tampering, then update this literal.\n"+
			"  (2) A provider-default constant changed (anthropicReadMult and friends). "+
			"golden-model is an anthropic entry with no explicit multipliers, so its "+
			"RESOLVED values come from code — and this test going red on such a change is "+
			"the feature, not a defect: it is the exact case a file hash would miss. Update "+
			"the literal; do NOT bump the scheme.\n"+
			"Either way, never paste the new value in without deciding which one happened.",
			info.TableHash, wantTableHash)
	}

	// ⚠️ The hex literal above is the ONLY thing pinning the provider-default
	// RESOLUTION. golden-model is an anthropic entry with no explicit cache
	// multipliers, so its baked values come from the anthropic* constants in THIS
	// package — change anthropicReadMult and the literal moves with prices.yaml
	// untouched. That is the whole reason the hash is over the resolved table.
	//
	// (An earlier version of this comment claimed the ModelCount check below
	// "asserts the resolution". It does not — a count of 4 says nothing about a
	// multiplier. The check is kept, but for what it actually is: a tripwire on
	// the FROZEN document having been edited, which would invalidate the literal
	// for a reason unrelated to the canonicalization.)
	if got := info.ModelCount; got != 4 {
		t.Fatalf("golden vector has %d models, want 4 — the frozen document was edited, so the "+
			"hex literal above no longer describes the vector it claims to", got)
	}
}

// TestPriceTableHash_CommentOnlyEditKeepsTableHash pins the asymmetry that is
// the entire reason there are two hashes: a comment-only edit must move
// file_hash and must NOT move table_hash.
func TestPriceTableHash_CommentOnlyEditKeepsTableHash(t *testing.T) {
	rawA := []byte("version: 7\neffective_date: \"2026-03-04\"\nmodels:\n" + hashTestBaseModels)
	rawB := []byte("# source: https://example.invalid/pricing (checked 2026-03-04)\n" +
		"version: 7\neffective_date: \"2026-03-04\"\nmodels:\n" + hashTestBaseModels +
		"# end of table\n")

	// 🔴 CONTROL: identical inputs would make BOTH assertions below vacuous —
	// equal table hashes AND equal file hashes for the trivial reason.
	if bytes.Equal(rawA, rawB) {
		t.Fatal("control: inputs identical — assertions vacuous")
	}

	_, infoA := mustParse(t, string(rawA))
	_, infoB := mustParse(t, string(rawB))

	if infoA.TableHash != infoB.TableHash {
		t.Errorf("a COMMENT-ONLY edit moved table_hash:\n  %s\n  %s\n"+
			"table_hash must be a digest of the RESOLVED table, so the source-URL comments "+
			"in prices.yaml stay editable without re-identifying the prices",
			infoA.TableHash, infoB.TableHash)
	}
	if infoA.FileHash == infoB.FileHash {
		t.Errorf("a comment-only edit did NOT move file_hash (both %s) — file_hash is "+
			"supposed to be a digest of the RAW bytes; if it tracks table_hash the two "+
			"hashes answer the same question and one of them is dead weight", infoA.FileHash)
	}
}

// TestPriceTableHash_ResolutionEquality is the test that proves table_hash is
// over the RESOLVED table rather than the file.
//
// 🔴 The three arms belong in ONE function on purpose. The first two ("provider
// default resolving to 0.10" == "explicit 0.10") are a classic false-green: a
// hash that returns a constant passes both. The 0.11 arm is the control that a
// constant cannot pass, and separating it into its own test would let a future
// edit delete or skip it without the equality arms noticing.
func TestPriceTableHash_ResolutionEquality(t *testing.T) {
	const head = "version: 3\neffective_date: \"2026-02-02\"\nmodels:\n" + hashTestBaseModels

	// Default: anthropic with NO cache_read_mult. providerDefaultMults bakes
	// anthropicReadMult (0.10) — from CODE, not from this YAML.
	defaulted := head + "  m: {input_per_m: 1, output_per_m: 2, provider: anthropic}\n"
	// Explicit, and numerically identical to the resolved default.
	explicitSame := head + "  m: {input_per_m: 1, output_per_m: 2, provider: anthropic, cache_read_mult: 0.10}\n"
	// Explicit, and DIFFERENT. This is a real rate change.
	explicitDiff := head + "  m: {input_per_m: 1, output_per_m: 2, provider: anthropic, cache_read_mult: 0.11}\n"

	_, infoDefault := mustParse(t, defaulted)
	_, infoSame := mustParse(t, explicitSame)
	_, infoDiff := mustParse(t, explicitDiff)

	if infoDefault.TableHash != infoSame.TableHash {
		t.Errorf("provider-default 0.10 and explicit 0.10 produced DIFFERENT table_hash:\n"+
			"  default:  %s\n  explicit: %s\n"+
			"They resolve to the same effective multipliers, so they are the same TABLE; a "+
			"hash that separates them is hashing the file, not the resolved table",
			infoDefault.TableHash, infoSame.TableHash)
	}
	// Vacuity control for the equality above: the two documents are genuinely
	// different bytes, so the match is not "same input, same output".
	if infoDefault.FileHash == infoSame.FileHash {
		t.Fatal("control: the defaulted and explicit documents hashed to the same file_hash — " +
			"they are supposed to be different bytes, so the table_hash equality above proves nothing")
	}

	// 🔴 THE CONTROL ARM. A hash function returning a constant passes every
	// assertion above and fails this one.
	if infoDefault.TableHash == infoDiff.TableHash {
		t.Errorf("cache_read_mult 0.10 and 0.11 produced the SAME table_hash (%s) — a real "+
			"rate change is invisible to the identity, which makes the whole guard a "+
			"rubber stamp", infoDiff.TableHash)
	}
}

// TestPriceTableHash_MapOrderIndependent pins determinism against Go's
// randomized map iteration.
//
// A single-shot comparison passes by luck roughly 1/n! of the time in the
// direction that matters, so this hashes 1000 times and asserts EXACTLY ONE
// distinct result — and compares the canonical BYTES too, because two different
// orderings could in principle be distinguishable in the serialization while
// colliding in a truncated digest.
func TestPriceTableHash_MapOrderIndependent(t *testing.T) {
	// Six models, not two: mutation testing showed a two-model table lets a
	// sort-less implementation survive on luck far too often.
	keys := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	prices := make([]modelPrice, len(keys))
	for i := range keys {
		prices[i] = modelPrice{
			inputPerM: float64(i + 1), outputPerM: float64(2*i + 3),
			provider: providerAnthropic, cacheReadMult: 0.1,
			cacheWrite5mMult: 1.25, cacheWrite1hMult: 2, billingMode: BillingPerToken,
		}
	}

	ascending := make(map[string]modelPrice, len(keys))
	for i := range keys {
		ascending[keys[i]] = prices[i]
	}
	descending := make(map[string]modelPrice, len(keys))
	for i := len(keys) - 1; i >= 0; i-- {
		descending[keys[i]] = prices[i]
	}

	const iterations = 1000
	seenHashes := map[string]bool{}
	seenBytes := map[string]bool{}
	ran := 0
	for i := 0; i < iterations; i++ {
		// Alternate maps so BOTH insertion sequences are exercised, and so the
		// two must agree with each other as well as with themselves.
		tbl := ascending
		if i%2 == 1 {
			tbl = descending
		}
		seenHashes[priceTableHash(tbl)] = true
		seenBytes[string(canonicalPriceTableBytes(tbl))] = true
		ran++
	}

	// Loop-integrity tripwire. ⚠️ Be honest about what this is: `ran++` is
	// unconditional inside a fixed-count loop, so today it CANNOT fail — review
	// correctly flagged it as decorative when presented as THE vacuity control.
	// It is kept because it becomes real the moment someone adds a `continue` or
	// a conditional to the body, which would silently shrink the sample. The
	// actual vacuity risk for the arms below — a zero-iteration loop — is
	// foreclosed by `iterations` being a const.
	if ran != iterations {
		t.Fatalf("the hashing loop ran %d times, want %d — the sample below is smaller than "+
			"it claims, so a rare ordering may simply never have been drawn", ran, iterations)
	}
	if len(seenHashes) != 1 {
		t.Errorf("%d distinct table_hash values over %d runs of the SAME table, want 1 — "+
			"canonicalPriceTableBytes is walking the map without sort.Strings, so the identity "+
			"changes run to run: %v", len(seenHashes), iterations, keysOf(seenHashes))
	}
	if len(seenBytes) != 1 {
		t.Errorf("%d distinct canonical BYTE strings over %d runs, want 1 — the serialization "+
			"itself is order-dependent (a digest could mask this by colliding; the bytes cannot)",
			len(seenBytes), iterations)
	}
}

// keysOf renders a set's keys for a failure message.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// naiveJoinedSerialization is the FORGEABLE serializer this scheme deliberately
// is not: the same field order, joined with a delimiter instead of framed by
// length. It exists only inside this test, as the control that proves the
// crafted collision below is real rather than asserted.
func naiveJoinedSerialization(tbl map[string]modelPrice) string {
	keys := make([]string, 0, len(tbl))
	for k := range tbl {
		keys = append(keys, k)
	}
	// sort.Strings equivalent without importing sort into the test's semantics.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	models := make([]string, 0, len(keys))
	for _, k := range keys {
		models = append(models, strings.Join(append([]string{k}, naiveFields(tbl[k])...), "|"))
	}
	return strings.Join(models, "|")
}

func naiveFields(p modelPrice) []string {
	return []string{
		canonPriceFloat(p.inputPerM), canonPriceFloat(p.outputPerM),
		strconv.FormatBool(p.combined), p.provider,
		canonPriceFloat(p.cacheReadMult), canonPriceFloat(p.cacheWrite5mMult),
		canonPriceFloat(p.cacheWrite1hMult), strconv.Itoa(p.contextThreshold),
		canonPriceFloat(p.inputPerMOver), canonPriceFloat(p.outputPerMOver),
		p.billingMode,
	}
}

// TestPriceTableHash_SeparatorInjectionCannotForge is the SERIALIZATION-forgery
// test.
//
// ⚠️ Scope, stated precisely, because the words below are easy to over-read:
// "forge" here means "make two DIFFERENT resolved tables produce the SAME
// canonical bytes". It is a property of the encoding. It is NOT a claim that the
// reported hash is trustworthy against an adversary who controls the install —
// the digest is unkeyed and self-reported, so against that adversary it proves
// nothing at all (see the scheme-tag block in prices.go).
//
// 🔑 Model keys are NEVER validated for charset — parsePriceTable checks every
// VALUE and no key — and YAML permits a quoted key containing newlines, "|", or
// any other character a concatenating serializer might use as a delimiter. So a
// one-model table whose KEY embeds a second model's serialization can be made
// byte-identical to a genuine two-model table under delimiter joining. If the
// scheme collided this way, "the table_hash matches" would not mean "the prices
// match" even between two honest installs, and the guard built on it would be
// decorative.
//
// The first assertion is the CONTROL: it proves the crafted pair really does
// collide under the naive scheme, so the second assertion is testing resistance
// to a live forgery rather than to an imagined one.
func TestPriceTableHash_SeparatorInjectionCannotForge(t *testing.T) {
	honest := modelPrice{
		inputPerM: 1, outputPerM: 2, provider: providerAnthropic,
		cacheReadMult: 0.1, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2,
		billingMode: BillingPerToken,
	}
	forged := modelPrice{
		inputPerM: 999, outputPerM: 999, provider: providerAnthropic,
		cacheReadMult: 0.1, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2,
		billingMode: BillingPerToken,
	}

	// The genuine two-model table.
	two := map[string]modelPrice{"alpha": honest, "beta": forged}

	// The forgery: ONE model whose key swallows alpha's whole record plus
	// beta's key, so `alpha|<alpha fields>|beta|<beta fields>` is reproduced
	// exactly by `<crafted key>|<beta fields>`.
	craftedKey := strings.Join(append([]string{"alpha"}, naiveFields(honest)...), "|") + "|beta"
	one := map[string]modelPrice{craftedKey: forged}

	// 🔴 CONTROL: the craft must actually collide under delimiter joining, or
	// the real assertion below is resisting nothing.
	if naiveJoinedSerialization(one) != naiveJoinedSerialization(two) {
		t.Fatalf("control: the crafted pair does NOT collide under delimiter joining, so the "+
			"assertion below proves nothing about forgeability.\n  one: %q\n  two: %q",
			naiveJoinedSerialization(one), naiveJoinedSerialization(two))
	}

	if priceTableHash(one) == priceTableHash(two) {
		t.Errorf("a 1-model table and a 2-model table hashed IDENTICALLY (%s) — the "+
			"serialization is delimiter-joined, so two DIFFERENT price tables now share one "+
			"identity and table_hash equality no longer implies price equality",
			priceTableHash(one))
	}

	// A key containing a newline must also round-trip unsplit: the issue's
	// original framing of this attack, kept as a second, independent shape.
	nl := map[string]modelPrice{"a\nb": honest}
	split := map[string]modelPrice{"a": honest, "b": honest}
	if priceTableHash(nl) == priceTableHash(split) {
		t.Error(`{"a\nb": P} and {"a": P, "b": P} hashed identically — a newline in a model ` +
			`key is being treated as a record separator`)
	}
}

// TestPriceTableHash_FloatEdges pins the exactness of the float encoding.
//
// ⚠️ The lossless requirement is not pedantry. FormatFloat(f,'f',2,64) renders
// 0.3 and 0.30000000000000004 as the SAME string "0.30", so a "simplification"
// to a fixed precision would make two genuinely different rates hash equal —
// the exact failure mode a content hash exists to prevent. The 'x' (hex) form
// is exact and platform-independent.
func TestPriceTableHash_FloatEdges(t *testing.T) {
	// ⚠️ MUST be computed at RUNTIME, not written as the constant expression
	// `0.1 + 0.2`. Go evaluates untyped constant arithmetic at arbitrary
	// precision and rounds ONCE, so the constant `0.1+0.2` IS exactly 0.3
	// (0x1.3333333333333p-02) — the opposite of the runtime float64 sum
	// (0x1.3333333333334p-02). Measured; the distinction is the whole test.
	a, b := 0.1, 0.2
	runtimeSum := a + b

	sum := map[string]modelPrice{"m": {inputPerM: runtimeSum, provider: providerAnthropic}}
	literal := map[string]modelPrice{"m": {inputPerM: 0.30000000000000004, provider: providerAnthropic}}
	nearby := map[string]modelPrice{"m": {inputPerM: 0.3, provider: providerAnthropic}}

	if priceTableHash(sum) != priceTableHash(literal) {
		t.Errorf("runtime 0.1+0.2 and the literal 0.30000000000000004 hashed differently "+
			"(%s vs %s) — they are the same float64, so the encoding is not a function of "+
			"the VALUE", priceTableHash(sum), priceTableHash(literal))
	}
	if priceTableHash(sum) == priceTableHash(nearby) {
		t.Errorf("0.30000000000000004 and 0.3 hashed identically (%s) — the float encoding is "+
			"lossy. FormatFloat('f',2) collides exactly these two; the scheme requires the "+
			"exact 'x' form", priceTableHash(sum))
	}
	// Prove the two inputs really are distinct float64s, or the arm above is
	// asserting about one value under two names.
	if runtimeSum == 0.3 {
		t.Fatal("control: the runtime sum 0.1+0.2 equals 0.3 on this platform — the " +
			"inequality arm above compares a value with itself")
	}

	// Negative zero. Reachable through parsePriceTable by two measured routes
	// (see canonPriceFloat); constructed directly here so the arm states the
	// invariant rather than a YAML quirk.
	negZero := math.Copysign(0, -1)
	if !math.Signbit(negZero) {
		t.Fatal("control: math.Copysign(0,-1) is not a negative zero — this arm is vacuous")
	}
	posZeroTbl := map[string]modelPrice{"m": {inputPerM: 1, outputPerM: 0, combined: true, provider: providerSelfHosted}}
	negZeroTbl := map[string]modelPrice{"m": {inputPerM: 1, outputPerM: negZero, combined: true, provider: providerSelfHosted}}
	if priceTableHash(posZeroTbl) != priceTableHash(negZeroTbl) {
		t.Errorf("+0.0 and -0.0 hashed differently (%s vs %s) — FormatFloat renders them "+
			"\"0x0p+00\" and \"-0x0p+00\", so canonPriceFloat's normalization is missing and "+
			"an arithmetically identical table false-mismatches",
			priceTableHash(posZeroTbl), priceTableHash(negZeroTbl))
	}

	// And the -0.0 route through the real parser, so the guard covers what an
	// operator can actually write in prices.yaml. `output_per_m` is
	// deliberately unvalidated on a combined entry.
	const head = "version: 1\neffective_date: \"2026-01-01\"\nmodels:\n" + hashTestBaseModels
	_, plus := mustParse(t, head+"  m: {input_per_m: 1, output_per_m: 0.0, combined: true, provider: self-hosted}\n")
	_, minus := mustParse(t, head+"  m: {input_per_m: 1, output_per_m: -0.0, combined: true, provider: self-hosted}\n")
	if plus.TableHash != minus.TableHash {
		t.Errorf("YAML `output_per_m: 0.0` and `-0.0` on a combined entry produced different "+
			"table_hash (%s vs %s) — this is a LIVE false-mismatch, not a theoretical one",
			plus.TableHash, minus.TableHash)
	}
	if plus.FileHash == minus.FileHash {
		t.Fatal("control: the +0.0 and -0.0 documents have the same file_hash — they are " +
			"supposed to be different bytes, so the equality above proves nothing")
	}
}

// TestPriceTableHash_HostQualifiedKeys pins that a host-qualified key (#300 —
// "model@host") is hashed whole. The key is data, not structure: nothing in the
// canonicalization may split on hostKeySep.
func TestPriceTableHash_HostQualifiedKeys(t *testing.T) {
	p := modelPrice{inputPerM: 1, outputPerM: 2, provider: providerAnthropic, billingMode: BillingPerToken}

	h1 := map[string]modelPrice{"m" + hostKeySep + "h1": p}
	h2 := map[string]modelPrice{"m" + hostKeySep + "h2": p}
	if priceTableHash(h1) == priceTableHash(h2) {
		t.Errorf("m@h1 and m@h2 hashed identically (%s) — two hosts' rates for the same model "+
			"are different prices and must be different identities", priceTableHash(h1))
	}

	// An "@" key must NOT be equivalent to the model and host as separate keys.
	joined := map[string]modelPrice{"m" + hostKeySep + "h1": p}
	splitUp := map[string]modelPrice{"m": p, "h1": p}
	if priceTableHash(joined) == priceTableHash(splitUp) {
		t.Error("a host-qualified key is being split on \"@\" — the key is opaque data to the " +
			"canonicalization")
	}
}

// TestPriceTableHash_VersionAndDateAreNotHashed pins the scoping decision, in
// BOTH directions.
//
// table_hash answers "does version V mean the same PRICES here as there". Fold
// V into the digest and the two failure modes it is supposed to expose —
// "content unchanged, version bumped" and "version unchanged, content changed" —
// both become one undifferentiated "hashes differ".
func TestPriceTableHash_VersionAndDateAreNotHashed(t *testing.T) {
	const models = "models:\n" + hashTestBaseModels

	_, v1 := mustParse(t, "version: 1\neffective_date: \"2026-01-01\"\n"+models)
	_, v2 := mustParse(t, "version: 2\neffective_date: \"2026-01-01\"\n"+models)
	_, d2 := mustParse(t, "version: 1\neffective_date: \"2027-12-31\"\n"+models)

	// Direction 1: metadata moved, content did not ⇒ table_hash MUST NOT move.
	if v1.TableHash != v2.TableHash {
		t.Errorf("bumping version 1→2 with identical models moved table_hash (%s → %s) — "+
			"\"content unchanged, version bumped\" is then undetectable, because it is "+
			"indistinguishable from a real price change", v1.TableHash, v2.TableHash)
	}
	if v1.TableHash != d2.TableHash {
		t.Errorf("changing effective_date with identical models moved table_hash (%s → %s)",
			v1.TableHash, d2.TableHash)
	}
	// Controls: the metadata really did change, and the FILES really do differ.
	if v1.Version == v2.Version || v1.EffectiveDate == d2.EffectiveDate {
		t.Fatal("control: the version/effective_date variants are not actually different — " +
			"the assertions above compare a table with itself")
	}
	if v1.FileHash == v2.FileHash || v1.FileHash == d2.FileHash {
		t.Fatal("control: version/date-only edits did not move file_hash — the raw-bytes " +
			"digest is not reading the raw bytes")
	}

	// Direction 2: version held constant, content moved ⇒ table_hash MUST move.
	// Without this arm, "hash ignores version" is satisfied by a constant.
	_, changed := mustParse(t, "version: 1\neffective_date: \"2026-01-01\"\nmodels:\n"+
		"  self-hosted-large: {input_per_m: 2.5, combined: true, provider: self-hosted}\n"+
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n"+
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n")
	if v1.Version != changed.Version {
		t.Fatal("control: the content-changed document has a different version — it is not " +
			"isolating the content change")
	}
	if v1.TableHash == changed.TableHash {
		t.Errorf("a rate change at the SAME version left table_hash unchanged (%s) — this is "+
			"the exact case version alone cannot see, and the reason table_hash exists",
			v1.TableHash)
	}
}

// TestPriceTableHash_SchemeTagShape pins the wire shape both hashes carry. The
// tag is what a rollback depends on; a bare digest silently loses it.
func TestPriceTableHash_SchemeTagShape(t *testing.T) {
	_, info := mustParse(t, goldenVectorYAML)

	tableRe := regexp.MustCompile(`^tierpt1:[0-9a-f]{64}$`)
	fileRe := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	if !tableRe.MatchString(info.TableHash) {
		t.Errorf("table_hash = %q, want ^tierpt1:[0-9a-f]{64}$ — scripts/seam-exercise.sh "+
			"pins the same shape outside `go test`", info.TableHash)
	}
	if !fileRe.MatchString(info.FileHash) {
		t.Errorf("file_hash = %q, want ^sha256:[0-9a-f]{64}$", info.FileHash)
	}
	// The two tags must not be interchangeable: a table_hash that validated as a
	// file_hash would let a caller compare the wrong pair of digests.
	if strings.HasPrefix(info.TableHash, fileHashScheme+":") {
		t.Error("table_hash carries the file_hash scheme tag — the two hashes answer " +
			"different questions and must not be confusable")
	}
}

// TestPriceTableHash_CoversEveryModelPriceField is the field-coverage pin: a
// field added to modelPrice later must be INSIDE the identity, or a price
// change carried by that field would be invisible to table_hash.
//
// ⚠️ modelPrice's fields are unexported, so reflect.Value.Set panics on them.
// The mutators are therefore explicit closures, and reflection is used only to
// enumerate the field NAMES that must have one — which is what makes the test
// fail with the missing field's name rather than a vague count mismatch.
func TestPriceTableHash_CoversEveryModelPriceField(t *testing.T) {
	base := modelPrice{
		inputPerM: 1, outputPerM: 2, combined: false, provider: providerAnthropic,
		cacheReadMult: 0.1, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2,
		contextThreshold: 200000, inputPerMOver: 6, outputPerMOver: 22.5,
		billingMode: BillingPerToken,
	}

	// One mutator per field, each moving that field ALONE to a different value.
	mutators := map[string]func(*modelPrice){
		"inputPerM":        func(p *modelPrice) { p.inputPerM = 1.5 },
		"outputPerM":       func(p *modelPrice) { p.outputPerM = 2.5 },
		"combined":         func(p *modelPrice) { p.combined = true },
		"provider":         func(p *modelPrice) { p.provider = providerOpenAI },
		"cacheReadMult":    func(p *modelPrice) { p.cacheReadMult = 0.5 },
		"cacheWrite5mMult": func(p *modelPrice) { p.cacheWrite5mMult = 1.5 },
		"cacheWrite1hMult": func(p *modelPrice) { p.cacheWrite1hMult = 3 },
		"contextThreshold": func(p *modelPrice) { p.contextThreshold = 400000 },
		"inputPerMOver":    func(p *modelPrice) { p.inputPerMOver = 7 },
		"outputPerMOver":   func(p *modelPrice) { p.outputPerMOver = 30 },
		"billingMode":      func(p *modelPrice) { p.billingMode = BillingSubscription },
	}

	rt := reflect.TypeOf(modelPrice{})
	// 🔴 CONTROL: reflection must actually find fields. A walk that matched
	// nothing makes the coverage check below vacuous in the "missing" direction.
	if rt.NumField() == 0 {
		t.Fatal("control: reflection found ZERO fields — every check above is vacuous")
	}

	fields := make(map[string]bool, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		fields[rt.Field(i).Name] = true
	}
	for name := range fields {
		if _, ok := mutators[name]; !ok {
			t.Errorf("modelPrice.%s has NO mutator in this test, so nothing proves it is inside "+
				"table_hash. Add both a mutator here and the field to canonicalPriceTableBytes "+
				"in the same commit — otherwise a price carried by %s changes with the table's "+
				"identity unchanged.", name, name)
		}
	}
	for name := range mutators {
		if !fields[name] {
			t.Errorf("this test mutates %q, which is not a field of modelPrice — the mutator set "+
				"has drifted from the struct and is guarding a field that no longer exists", name)
		}
	}

	baseHash := priceTableHash(map[string]modelPrice{"m": base})
	for name, mutate := range mutators {
		mutated := base
		mutate(&mutated)
		if mutated == base {
			t.Errorf("the %q mutator did not change the value — it cannot detect anything", name)
			continue
		}
		if got := priceTableHash(map[string]modelPrice{"m": mutated}); got == baseHash {
			t.Errorf("changing modelPrice.%s did NOT change table_hash (%s) — that field is "+
				"outside canonicalPriceTableBytes, so a price change carried by it is invisible "+
				"to the identity", name, got)
		}
	}
}

// TestPriceTableInfo_StaysComparable pins the struct property cmd/tierd depends
// on: score_test.go does `info != (store.PriceTableInfo{})`, which stops
// compiling the moment a slice, map or func field lands on PriceTableInfo.
// Asserting it HERE turns a downstream compile break into a local, explained
// failure.
func TestPriceTableInfo_StaysComparable(t *testing.T) {
	rt := reflect.TypeOf(PriceTableInfo{})
	if rt.NumField() == 0 {
		t.Fatal("control: reflection found ZERO fields on PriceTableInfo — this check is vacuous")
	}
	if !rt.Comparable() {
		t.Fatal("PriceTableInfo is no longer comparable — cmd/tierd/score_test.go compares it " +
			"against the zero value with !=, which is now a compile error. Identity fields must " +
			"be strings, not slices/maps.")
	}
	// The identity fields specifically must be strings: a [32]byte digest is
	// comparable too, but it would force every consumer to encode it and would
	// lose the scheme tag that makes a rollback distinguishable from tampering.
	for _, name := range []string{"Source", "TableHash", "FileHash"} {
		f, ok := rt.FieldByName(name)
		if !ok {
			t.Errorf("PriceTableInfo has no %s field (#713)", name)
			continue
		}
		if f.Type.Kind() != reflect.String {
			t.Errorf("PriceTableInfo.%s is %s, want string — the scheme tag is part of the value",
				name, f.Type)
		}
	}
}

// TestActivePriceTableInfo_EmbeddedCarriesIdentity pins that the DEFAULT path
// (no --prices) is stamped. A feature that only lights up under an override
// would leave every zero-config install unidentified.
func TestActivePriceTableInfo_EmbeddedCarriesIdentity(t *testing.T) {
	loadDefaultPriceTable(t)
	info := ActivePriceTableInfo()

	// 🔴 This assertion is only meaningful because loadDefaultPriceTable routes
	// through parseEmbeddedPriceTable — the SAME producer init() uses — instead
	// of stamping Source itself. Review measured the earlier shape: the helper
	// assigned PriceSourceEmbedded, this line read it back, and deleting the real
	// stamp from init() left the whole suite green while `tierd score-log` began
	// emitting price_table.source = "" on the zero-config path. A test whose
	// subject is a value its own fixture wrote is not a test.
	if info.Source != PriceSourceEmbedded {
		t.Errorf("embedded table Source = %q, want %q — the zero-config path stamps this and "+
			"cmd/tierd forwards it verbatim into price_table.source", info.Source, PriceSourceEmbedded)
	}
	// Pin the WIRE LITERAL too, not only the constant. cmd/tierd/scorelog.go and
	// docs/api-compatibility.md both publish "embedded" as the price_table.source
	// value; asserting only `== PriceSourceEmbedded` would let a rename of the
	// constant change the emitted JSON with every test still green.
	if PriceSourceEmbedded != "embedded" {
		t.Errorf("PriceSourceEmbedded = %q, but \"embedded\" is the PUBLISHED price_table.source "+
			"value. Renaming this constant is a wire-format change, not a refactor.",
			PriceSourceEmbedded)
	}
	if !regexp.MustCompile(`^tierpt1:[0-9a-f]{64}$`).MatchString(info.TableHash) {
		t.Errorf("embedded table_hash = %q, want a tierpt1-tagged digest", info.TableHash)
	}
	if info.FileHash != priceFileHash(defaultPriceTableYAML) {
		t.Errorf("embedded file_hash = %q, want the digest of the embedded prices.yaml (%q)",
			info.FileHash, priceFileHash(defaultPriceTableYAML))
	}
	// The embedded table must not hash like a trivially empty one — the failure
	// shape if the identity were computed before the map was populated.
	if info.TableHash == priceTableHash(map[string]modelPrice{}) {
		t.Error("the embedded table hashes identically to an EMPTY table — the identity is " +
			"being computed over an unpopulated map")
	}
}

// TestPriceTableHash_FramingIsUniquelyDecodable pins the invariant the whole
// injectivity argument rests on, and which nothing else in this file can see.
//
// 🔴 Why every other test misses it. canonicalPriceTableBytes is injective
// because a length-prefixed stream decodes into a FRAME SEQUENCE unambiguously
// AND every model contributes exactly canonicalFramesPerModel frames — the
// fixed arity is what lets the sequence be regrouped into records, and it is
// why no model-count prefix is needed. Wrap any field in a condition (`if
// p.contextThreshold != 0 { … }` — the sort of "don't serialize the zero
// values" tidy-up that looks free) and records become variable-arity, the
// stream stops being groupable, and two distinct tables CAN collide.
//
// Measured by review: that mutation leaves the field-coverage pin and every
// equality arm GREEN. Only the golden vector reddens — and the golden vector's
// own failure message tells you to consider updating the literal, which is
// exactly the wrong response here. So this test decodes the bytes and asserts
// the arity directly.
func TestPriceTableHash_FramingIsUniquelyDecodable(t *testing.T) {
	// Keys chosen to be hostile to any framing shortcut: an embedded newline, an
	// embedded "|", an embedded NUL, an empty key, and a key that itself looks
	// like a length prefix.
	tbl := map[string]modelPrice{
		"":              {inputPerM: 1, provider: providerSelfHosted, combined: true},
		"a\nb":          {inputPerM: 2, outputPerM: 3, provider: providerAnthropic},
		"c|d":           {inputPerM: 4, outputPerM: 5, provider: providerOpenAI},
		"e\x00f":        {inputPerM: 6, outputPerM: 7, provider: providerGoogle},
		"\x00\x00\x00g": {inputPerM: 8, outputPerM: 9, provider: providerXAI},
	}
	raw := canonicalPriceTableBytes(tbl)

	// Decode: 8-byte big-endian length, then that many bytes, repeatedly.
	var frames []string
	for off := 0; off < len(raw); {
		if off+8 > len(raw) {
			t.Fatalf("truncated length prefix at offset %d of %d — the stream is not "+
				"length-framed and therefore not uniquely decodable", off, len(raw))
		}
		n := binary.BigEndian.Uint64(raw[off : off+8])
		off += 8
		if uint64(len(raw)-off) < n {
			t.Fatalf("frame at offset %d claims %d bytes but only %d remain — the framing "+
				"is inconsistent", off-8, n, len(raw)-off)
		}
		frames = append(frames, string(raw[off:off+int(n)]))
		off += int(n)
	}

	// 🔴 CONTROL: a decode that produced nothing would make the arity check below
	// pass for an empty table and prove nothing.
	if len(frames) == 0 {
		t.Fatal("control: decoded ZERO frames from a non-empty table — the assertions below are vacuous")
	}
	wantFrames := canonicalFramesPerModel * len(tbl)
	if len(frames) != wantFrames {
		t.Fatalf("decoded %d frames for %d models, want exactly %d (canonicalFramesPerModel=%d).\n"+
			"Records are no longer FIXED-ARITY, so the frame sequence cannot be regrouped into "+
			"models unambiguously and two distinct tables can now collide. If a field was made "+
			"conditional, that is the defect; if a field was added or removed, update "+
			"canonicalFramesPerModel in the same commit.",
			len(frames), len(tbl), wantFrames, canonicalFramesPerModel)
	}

	// Regroup and assert the KEYS round-trip exactly — including every hostile
	// byte above. This is the "uniquely decodable for ANY key content" claim.
	gotKeys := make(map[string]bool, len(tbl))
	for i := 0; i < len(frames); i += canonicalFramesPerModel {
		gotKeys[frames[i]] = true
	}
	if len(gotKeys) != len(tbl) {
		t.Errorf("regrouping recovered %d distinct keys from %d models — a key was split or "+
			"two keys merged", len(gotKeys), len(tbl))
	}
	for k := range tbl {
		if !gotKeys[k] {
			t.Errorf("key %q did not round-trip through the framing intact; recovered keys: %q",
				k, slices.Sorted(maps.Keys(gotKeys)))
		}
	}

	// The recovered keys must be in SORTED order, which is the other half of
	// "records → map is a bijection".
	var recovered []string
	for i := 0; i < len(frames); i += canonicalFramesPerModel {
		recovered = append(recovered, frames[i])
	}
	if !slices.IsSorted(recovered) {
		t.Errorf("recovered keys are not in sorted order: %q", recovered)
	}
}

// TestPriceTableHash_EmptyTableIsNotAWildcard pins the degenerate case: an empty
// table must not hash like anything else. It is the boundary the frame decoder
// above deliberately does not cover (it needs a non-empty input for its control).
func TestPriceTableHash_EmptyTableIsNotAWildcard(t *testing.T) {
	empty := priceTableHash(map[string]modelPrice{})
	one := priceTableHash(map[string]modelPrice{"m": {inputPerM: 1, provider: providerAnthropic}})
	if empty == one {
		t.Errorf("an EMPTY table and a one-model table hashed identically (%s)", empty)
	}
	if len(canonicalPriceTableBytes(map[string]modelPrice{})) != 0 {
		t.Error("an empty table must canonicalize to zero bytes; anything else is a header " +
			"the injectivity argument does not account for")
	}
}
