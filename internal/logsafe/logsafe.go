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
// SIEM, reads as genuine. fmt.Sprintf("%q", s) DOES neutralize that at runtime
// (it escapes the newline), but static analyzers — CodeQL's go/log-injection in
// particular — do NOT model %q/strconv.Quote as a sanitizing barrier; they credit
// explicit CR/LF stripping or replacement. So the barrier strips "\r" and "\n"
// outright BEFORE %q, making it recognizable to the scanner while %q still hardens
// the remainder (other control bytes, quotes, invalid UTF-8). The injection dies
// on both the runtime and the static-analysis paths.
package logsafe

import (
	"fmt"
	"strings"
)

// Field length caps. A payload stuffing a megabyte into one field must not flood
// the log, so the sanitized value is bounded. Strings (ids, paths, names) are
// capped tighter than errors, which may carry a short human sentence plus a
// wrapped value.
const (
	maxStrLen = 256
	maxErrLen = 512
	truncMark = "...(truncated)"
)

// Str renders a client-controlled string as a single-line, quoted log value.
// It strips CR and LF (the injection vector, and the transformation CodeQL
// recognizes as a barrier), then %q-quotes to escape any remaining control
// bytes, quotes, and invalid UTF-8. The result is length-capped.
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

// clean is the shared barrier: truncate to a byte budget, strip CR/LF so the
// value is single-line (the CodeQL-recognized sanitizer), then %q-quote the
// remainder. Truncation happens first so the strip/quote work is bounded; a byte
// budget can split a UTF-8 rune, but %q escapes the resulting invalid bytes, so
// the output is always valid, single-line, quoted text.
func clean(s string, max int) string {
	if len(s) > max {
		s = s[:max] + truncMark
	}
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return fmt.Sprintf("%q", s)
}
