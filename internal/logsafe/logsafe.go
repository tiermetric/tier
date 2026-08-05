// Package logsafe renders client-controlled values safe for structured logs,
// neutralizing log/CRLF injection (#321, CodeQL go/log-injection).
//
// It is the single shared sanitizer barrier: the webhook, API, proxy, and tierd
// packages route their client-controlled log fields through Str or Err here rather
// than each carrying its own copy of the escaping logic. (It does not claim to be
// the only thing every sink in those packages does — a value that is provably
// bounded, e.g. a regex-constrained hex SHA, is logged bare with an inline
// rationale instead.) Consolidating the barrier on one implementation means it is
// audited — and hardened — in exactly one place.
//
// WHY AN EXPLICIT CR/LF STRIP, not just %q: a log field logged verbatim lets a
// caller embed a newline and forge a second, standalone log record
// ("<value>\ntime=... level=ERROR msg=...") that an operator, or a line-oriented
// SIEM, reads as genuine. fmt.Sprintf("%q", s) neutralizes that at runtime by
// ESCAPING the newline; the strip REMOVES it. Three reasons the removal is the
// primary barrier and %q is the backstop, not the other way round:
//
//  1. Escaping is reversible downstream, removal is not. After %q alone the
//     newline still exists in the record as the two bytes `\n`, and any consumer
//     that unquotes the field to render it — a JSON log viewer, a SIEM that
//     expands escapes before matching — resurrects the line break and with it the
//     forged record. A stripped value cannot be un-stripped by anything.
//  2. The static-analysis credit for %q is attached to the FORMAT CALL's result,
//     not to the string. CodeQL's go/log-injection sanitizes
//     `call.getAResult()` of a format call whose directive matches `%[^%#]*q`
//     (SafeFormatArgumentSanitizer in LogInjectionCustomizations.qll), so the
//     credit survives only while the value's last transformation is that %q — and
//     it is silently lost if anyone rewrites the directive to `%#q`, which the
//     regex deliberately excludes because Go's alternate quoting preserves raw
//     newlines. The strip is credited separately and unconditionally, by
//     ReplaceSanitizer (`getReplacedString() = ["\r", "\n"]`), which is exactly
//     the shape used below.
//  3. It makes the value single-line for handlers that do not quote at all.
//
// ⚠️ CORRECTION (#321, verified 2026-08-04 against github/codeql@main): earlier
// revisions of this comment — and #321's own body — asserted that CodeQL "does
// NOT model %q/strconv.Quote as a sanitizing barrier". That is HALF WRONG and the
// half that is wrong was the stated reason this function exists. `%q` used through
// a format call IS credited (SafeFormatArgumentSanitizer, above); `strconv.Quote`
// is NOT, because it is not a format call. The reasons above are why the strip
// stays regardless.
//
// ⚠️ AND THE SCANNER IS NOT WATCHING TODAY. go/log-injection declares
// `@precision medium`; GitHub's DEFAULT code-scanning suite includes only
// `precision: high|very-high` (github/codeql's
// misc/suite-helpers/code-scanning-selectors.yml — a path in THAT repo, not this one),
// so the query ships in `security-extended`, not `default`. The public mirror runs
// default-setup with `query_suite: "default"` and the private repo is
// not-configured, so NEITHER repo runs this query — a green Security tab is not
// evidence that this barrier holds. The tests in this package are.
package logsafe

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Field length caps. A payload stuffing a megabyte into one field must not flood
// the log, so the sanitized value is bounded. Strings (ids, paths, names) are
// capped tighter than errors, which may carry a short human sentence plus a
// wrapped value.
//
// ⚠️ THE CAP BOUNDS THE RENDERED OUTPUT, not the input (#321 review, 2026-08-04).
// It did not always: an earlier revision trimmed the input to the cap and then
// %q-quoted, and %q expands one control byte into four (`\x00`), so 300 NUL bytes
// against a 256 cap rendered 1040 bytes — a 4x flood through the very function
// whose stated job is to stop one. clean now measures the QUOTED width, so the
// guarantee is honest and checkable:
//
//	len(clean(s, max)) <= max + len(truncMark)   for max >= 2
//
// TestStr_TruncatesTheRenderedValue pins it with a non-printable payload, which is
// the only kind that can break it.
const (
	maxStrLen = 256
	maxErrLen = 512
	truncMark = "...(truncated)"
)

// Str renders a client-controlled string as a single-line, quoted log value.
// It strips CR and LF (the injection vector), then %q-quotes to escape any
// remaining control bytes, quotes, and invalid UTF-8. The RENDERED result is
// length-capped — see the cap block above for why that distinction is the whole
// point. Both transformations are barriers CodeQL's go/log-injection recognizes;
// see the package doc for why the strip is the primary one.
//
// The CR/LF are REMOVED, not escaped: after Str, the value can never span more
// than the one log line it is emitted on, so a forged "\ntime=... level=ERROR"
// rider is glued onto that line inside the quotes instead of beginning its own
// record. The diagnostic survives; the injection does not.
func Str(s string) string {
	return clean(s, maxStrLen)
}

// Err renders a (possibly nil) error the same way Str renders a string: a store
// or validation error can wrap an attacker-controlled value (a free-form
// developer id, org name, or commit SHA that was length-capped but never
// charset-validated), so logging err verbatim is the same injection vector. A
// nil error renders as the empty string so callers can log it unconditionally.
func Err(err error) string {
	if err == nil {
		return ""
	}
	return clean(err.Error(), maxErrLen)
}

// Join renders a LIST of client-controlled strings as one report/log field:
// every element through Str, comma-separated, with at most maxShown elements
// listed and the remainder collapsed to a "(+N more)" count.
//
// The count cap is not decoration. Str bounds each element, but a report that
// interpolates a whole list is bounded by the LENGTH of that list, and the list
// is as attacker-influenced as the elements are — a producer that invents ten
// thousand distinct model strings floods an operator's terminal with a
// per-element cap alone. Callers choose maxShown because the right number is a
// function of what the operator has to DO with the list: doctor names identities
// to eyeball (3 is plenty), reprice names the models to add to the price table
// (a truncated list there is an incomplete remedy).
//
// This started as joinSafe in cmd/tierd/doctor.go. It lives here because
// internal/store needed the same thing (#321 review, 2026-08-04) and a second
// copy of a sanitizer barrier is how the first copy drifts — the package doc's
// "audited in exactly one place" is the whole reason this package exists.
// An empty list renders as the empty string; callers wanting a placeholder such
// as "(none)" own that choice, since it is report wording, not sanitization.
func Join(vals []string, maxShown int) string {
	if len(vals) == 0 {
		return ""
	}
	shown := vals
	if maxShown >= 0 && len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	parts := make([]string, 0, len(shown))
	for _, v := range shown {
		parts = append(parts, Str(v))
	}
	out := strings.Join(parts, ", ")
	if len(vals) > len(shown) {
		out += fmt.Sprintf(" (+%d more)", len(vals)-len(shown))
	}
	return out
}

// clean is the shared barrier: strip CR/LF so the value is single-line, then
// %q-quote the remainder, bounded so the RENDERED result fits the byte budget.
//
// 🔴 THE TWO ReplaceAll CALLS ARE LOAD-BEARING IN THEIR EXACT SHAPE, not just in
// their effect. CodeQL's ReplaceSanitizer matches a StringOps::ReplaceAll whose
// REPLACED string literal is "\r" or "\n" — the shape written below. A rewrite
// to strings.Map, a bytes.Buffer loop, or a regexp keeps the runtime behaviour
// and is not that shape, so the barrier would have to be re-credited some other
// way. (strings.NewReplacer is the uncertain one: CodeQL's StringOps has modelled
// Replacer in some versions, so treat "NewReplacer loses the credit" as UNVERIFIED
// rather than as a reason to avoid it.)
//
// TestClean_KeepsTheCodeQLRecognizedShape fails on any such rewrite. If you are
// here because that test failed, keep these two calls rather than relax the test —
// and note that nothing else can tell you: neither repo's CodeQL suite runs
// go/log-injection (see the package doc), so there is no scanner run to fall back
// on. Claims here were verified against github/codeql@main on 2026-08-04; that is
// a moving target, so re-check before treating any of it as still current.
//
// ORDER MATTERS, AND IT IS THE REVERSE OF WHAT IT WAS. The strip runs BEFORE the
// trim (#321 review, 2026-08-04). Trimming first spent the whole budget on bytes
// that were about to be deleted, so a value whose leading 256 bytes were CR/LF
// padding rendered as `"...(truncated)"` and nothing else — the injection was
// neutralized and the diagnostic was destroyed with it, contradicting this
// package's own promise that "the diagnostic survives; the injection does not".
// Stripping first costs one extra linear pass over the raw value (the caller
// already holds it in memory) and buys a budget spent entirely on signal.
//
// A value that is ENTIRELY CR/LF still renders as `""` — correctly, and without
// the truncation marker: nothing was truncated, everything was stripped, and
// claiming otherwise would be a lie about which barrier fired.
//
// A byte budget can split a UTF-8 rune, but quotedPrefixLen cuts only on rune
// boundaries and %q escapes any invalid bytes that were already in the value, so
// the output is always valid, single-line, quoted text.
func clean(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")

	// First trim bounds the quoting WORK (a megabyte value is never quoted);
	// quotedPrefixLen then bounds the rendered WIDTH, which is the honest cap.
	truncated := false
	if len(s) > max {
		s = s[:max]
		truncated = true
	}
	if k := quotedPrefixLen(s, max); k < len(s) {
		s = s[:k]
		truncated = true
	}

	// The single %q call, on the value itself, is what CodeQL's
	// SafeFormatArgumentSanitizer credits — keep it here rather than folding it
	// into the helper above, whose result is a WIDTH and never reaches the log.
	out := fmt.Sprintf("%q", s)
	if truncated {
		// OUTSIDE the quotes, deliberately: %q escapes any `"` in the value, so a
		// caller cannot embed a convincing `"...(truncated)` of their own and make
		// an operator believe a longer value was cut. Inside the quotes it would
		// be forgeable; outside it is the renderer speaking, not the value.
		out += truncMark
	}
	return out
}

// quotedPrefixLen returns the largest rune-boundary byte length k such that the
// %q rendering of s[:k] — including the two quotes %q adds — fits in max bytes.
//
// It exists because %q's expansion is data-dependent and unbounded relative to
// the input: a printable byte renders as itself, a control byte as four
// (`\x00`), an invalid byte as four. Capping the input therefore does not cap
// the record, which is exactly the bug this replaced.
//
// strconv.Quote is used here as a RULER, not as a barrier. %q renders each rune
// independently, so the width of the whole is the sum of the widths of its
// parts, which makes a single forward pass exact. (It is emphatically not the
// sanitizer — see the package doc: CodeQL credits the format CALL's result, and
// strconv.Quote is not a format call. Nothing this function returns is ever
// logged.)
func quotedPrefixLen(s string, max int) int {
	width := 2 // the two quotes %q wraps the value in
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		w := len(strconv.Quote(s[i:i+size])) - 2
		if width+w > max {
			return i
		}
		width += w
		i += size
	}
	return len(s)
}
