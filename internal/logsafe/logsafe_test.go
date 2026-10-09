package logsafe

import (
	"errors"
	"strings"
	"testing"
)

// TestStr_StripsCRLFAndQuotes pins the barrier contract: CR/LF are removed
// outright (the CodeQL-recognized sanitizer), and %q quotes the remainder, so a
// forged newline-bearing value can never begin its own log record.
func TestStr_StripsCRLFAndQuotes(t *testing.T) {
	const forgedMarker = `level=ERROR msg="auth bypassed"`
	got := Str("evilsha\r\ntime=2026-07-12T00:00:00Z " + forgedMarker)

	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Str leaked a raw CR/LF, permitting a forged log record: %q", got)
	}
	// Stripped, not escaped: the halves are joined onto one line, so "evilsha"
	// is now adjacent to "time=". A dropped-byte bug would lose the injection but
	// also corrupt the diagnostic; a joined value proves the strip is clean.
	if !strings.Contains(got, "evilshatime=") {
		t.Fatalf("expected CR/LF stripped and halves joined, got: %q", got)
	}
	// The %q signature: the value is wrapped in quotes, so the diagnostic content
	// survives while the embedded quotes from the marker are escaped.
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("expected a %%q-quoted value, got: %q", got)
	}
}

// wantMaxStrLen is the cap Str actually promises: maxStrLen bytes of RENDERED
// output (quotes included) plus the truncation marker the renderer appends
// outside them. Stated as one expression so a test that passes is a statement
// about the documented bound rather than about a hand-tuned slack term.
const wantMaxStrLen = maxStrLen + len(truncMark)

// TestStr_TruncatesTheRenderedValue bounds a flood-sized field — and does it with
// payloads that can actually break the bound.
//
// The predecessor of this test used 300 printable 'x' bytes and asserted only
// len <= maxStrLen+len(truncMark)+8. That is structurally blind: %q renders a
// printable byte as itself, so the printable case passes under ANY
// implementation that trims the input, including the one that shipped 1040 bytes
// for a 300-byte non-printable payload against a 256 cap. Only a payload %q
// EXPANDS can tell a rendered cap from an input cap, so one is required here.
func TestStr_TruncatesTheRenderedValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantContains is a substring of the surviving diagnostic — the point of
		// a sanitizer that is not just a deleter.
		wantContains string
		// forbid, when non-empty, must NOT appear in the output. Used to pin that
		// a cut landed on a rune boundary: splitting a multi-byte rune leaves
		// bytes that are invalid on their own, and %q renders those as \xNN.
		forbid string
	}{
		{
			// The original case. Kept, because it is the one that pins that a
			// long PRINTABLE value is not over-trimmed: 4x-safe arithmetic that
			// divided every budget by four would still respect the cap while
			// throwing away three quarters of every ordinary diagnostic.
			name:         "printable overflows by length",
			in:           strings.Repeat("x", maxStrLen+100),
			wantContains: strings.Repeat("x", 200),
		},
		{
			// 🔴 THE CASE THAT WAS MISSING. %q renders NUL as the four bytes
			// `\x00`, so an input-side cap of 256 emitted 1040 bytes here.
			name:         "non-printable overflows by RENDERED width",
			in:           strings.Repeat("\x00", maxStrLen+44),
			wantContains: `\x00`,
		},
		{
			// Invalid UTF-8 expands the same 4x way and takes the other branch of
			// quotedPrefixLen's rune decode (size 1, RuneError).
			name:         "invalid UTF-8 overflows by RENDERED width",
			in:           strings.Repeat("\xff", maxStrLen+44),
			wantContains: `\xff`,
		},
		{
			// Multi-byte runes: the cut must land on a rune boundary, never mid
			// rune, or the tail renders as `\xe2`-style garbage.
			name:         "3-byte runes are cut on a rune boundary",
			in:           strings.Repeat("€", maxStrLen),
			wantContains: "€€€",
			// A byte-wise walk would cut "€" in the middle and %q would render the
			// orphaned bytes as \xe2 / \x82 — garbage on the tail of an operator's
			// diagnostic. A length assertion alone cannot see it: the mangled
			// output is the same size as the correct one.
			forbid: `\x`,
		},
		{
			// 🔴 BOTH RUNE WIDTHS ARE REQUIRED, and this is the case that does the
			// work. A byte-wise walk over the 3-byte case above lands on a rune
			// boundary by ARITHMETIC COINCIDENCE — its cut index is 63, which
			// happens to divide by 3 — so that case passes under a broken
			// implementation. 63 is odd, so a 2-byte rune cannot survive the same
			// walk. Measured: revert quotedPrefixLen to advance one byte at a time
			// and only this subtest fails.
			name:         "2-byte runes are cut on a rune boundary",
			in:           strings.Repeat("é", maxStrLen),
			wantContains: "ééé",
			forbid:       `\x`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Str(tc.in)
			if !strings.Contains(got, truncMark) {
				t.Errorf("oversized string not truncated: %q", got)
			}
			if len(got) > wantMaxStrLen {
				t.Errorf("RENDERED output exceeds the documented cap: len=%d, want <= %d.\n"+
					"The cap must bound what reaches the log, not what entered the function — "+
					"%%q expands one control byte into four, so an input-side trim is not a cap.",
					len(got), wantMaxStrLen)
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("truncation destroyed the diagnostic: %q does not contain %q", got, tc.wantContains)
			}
			if tc.forbid != "" && strings.Contains(got, tc.forbid) {
				t.Errorf("output contains %q: %q\n"+
					"quotedPrefixLen must advance a RUNE at a time. Cutting on a byte "+
					"boundary splits a multi-byte rune and %%q renders the orphaned bytes "+
					"as hex escapes, corrupting the tail of the diagnostic.", tc.forbid, got)
			}
		})
	}
}

// TestStr_TruncationMarkerIsOutsideTheQuotes pins a claim clean() makes in a
// comment and nothing else checked: the marker is appended AFTER the closing
// quote, so it is the renderer speaking rather than the value.
//
// Inside the quotes it would be forgeable. %q escapes any `"` in the value, so
// with the marker outside, a caller who embeds the literal `"...(truncated)` in
// their own string cannot make a COMPLETE value look like a cut one — the
// operator's "there was more here" signal stays trustworthy. Move the marker
// above the fmt.Sprintf and this test fails; nothing else does.
func TestStr_TruncationMarkerIsOutsideTheQuotes(t *testing.T) {
	// A genuinely truncated value: the marker must be the literal tail, with the
	// closing quote immediately before it.
	got := Str(strings.Repeat("x", maxStrLen+100))
	if !strings.HasSuffix(got, truncMark) {
		t.Errorf("truncated value = %q; want it to END with %q, outside the closing quote",
			got, truncMark)
	}
	if quoted := strings.TrimSuffix(got, truncMark); !strings.HasSuffix(quoted, `"`) {
		t.Errorf("truncated value = %q; the %q marker must follow the CLOSING quote, "+
			"not sit inside the quoted value where a caller could forge it", got, truncMark)
	}

	// The forgery half: an UNtruncated value that embeds the marker text must not
	// come out looking truncated.
	forged := Str(`totally-fine"` + truncMark)
	if strings.HasSuffix(forged, truncMark) {
		t.Errorf("a short value forged the truncation marker: %q — an operator reading this "+
			"believes content was cut when nothing was", forged)
	}
}

// TestStr_CRLFDenseValueKeepsItsDiagnostic pins the ORDER of the strip and the
// trim, which is separately load-bearing from either one on its own.
//
// When the trim ran first, the entire budget could be spent on bytes that were
// about to be deleted: 300 bytes of CR/LF rendered as `"...(truncated)"` and
// nothing else. The injection was neutralized and the diagnostic went with it,
// which contradicts this package's own doc promise. Swap the two ReplaceAll
// calls back below the trim in clean() and this test fails; nothing else does.
func TestStr_CRLFDenseValueKeepsItsDiagnostic(t *testing.T) {
	const diagnostic = "the-actual-model-name"
	got := Str(strings.Repeat("\r\n", maxStrLen) + diagnostic)

	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Str leaked a raw CR/LF: %q", got)
	}
	if !strings.Contains(got, diagnostic) {
		t.Fatalf("CR/LF padding consumed the whole budget and erased the diagnostic: %q\n"+
			"clean() must strip BEFORE it trims, or padding an injection with %d bytes of "+
			"CR/LF silently blinds every report that uses this barrier.", got, maxStrLen*2)
	}
	if len(got) > wantMaxStrLen {
		t.Errorf("output exceeds the documented cap: len=%d, want <= %d", len(got), wantMaxStrLen)
	}
}

// TestStr_AllCRLFRendersEmptyAndUnmarked pins the honest edge: a value that is
// ENTIRELY injection has no diagnostic to preserve, so it renders as the empty
// quoted string — and WITHOUT the truncation marker, because nothing was
// truncated. Marking it would tell an operator a longer value was cut when in
// fact every byte was stripped, sending them to look for content that never
// existed.
func TestStr_AllCRLFRendersEmptyAndUnmarked(t *testing.T) {
	got := Str(strings.Repeat("\r\n", maxStrLen))
	if got != `""` {
		t.Fatalf("all-CRLF value = %q, want %q (stripped to nothing, not truncated)", got, `""`)
	}
	if strings.Contains(got, truncMark) {
		t.Errorf("all-CRLF value claims truncation it did not perform: %q", got)
	}
}

// TestJoin covers the list surface: every element is sanitized, the element
// COUNT is capped independently of each element's length, and the overflow is
// reported rather than silently dropped.
func TestJoin(t *testing.T) {
	if got := Join(nil, 3); got != "" {
		t.Errorf("Join(nil) = %q, want empty", got)
	}
	got := Join([]string{"alice", "bob\r\ntime=2026-01-01 level=ERROR msg=\"forged\"", "carol", "dave"}, 3)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Join leaked a raw CR/LF: %q", got)
	}
	if !strings.Contains(got, "(+1 more)") {
		t.Errorf("Join did not report the elements it dropped: %q", got)
	}
	if strings.Contains(got, "dave") {
		t.Errorf("Join exceeded maxShown: %q", got)
	}
	// The count cap is the flood bound: a thousand short elements are as much of
	// a flood as one long one, and Str alone cannot see that.
	flood := make([]string, 1000)
	for i := range flood {
		flood[i] = "m"
	}
	if got := Join(flood, 3); len(got) > 64 {
		t.Errorf("Join did not bound a long list: len=%d, %q", len(got), got)
	}
}

// TestErr mirrors Str for the error surface and pins the nil contract.
func TestErr(t *testing.T) {
	if got := Err(nil); got != "" {
		t.Fatalf("Err(nil) = %q, want empty", got)
	}
	forged := errors.New("upsert failed: org=acme\ntime=2026-07-09 level=ERROR msg=\"auth bypassed\"")
	got := Err(forged)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Err leaked a raw CR/LF: %q", got)
	}
	if !strings.Contains(got, "org=acmetime=") {
		t.Fatalf("expected the newline stripped so the halves join: %q", got)
	}
	if !strings.Contains(got, "upsert failed") {
		t.Errorf("sanitizer destroyed the diagnostic: %q", got)
	}
	if got := Err(errors.New(strings.Repeat("x", 900))); len(got) > 600 {
		t.Errorf("oversized error not truncated: len=%d", len(got))
	}
}
