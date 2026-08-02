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

// TestStr_Truncates bounds a flood-sized field.
func TestStr_Truncates(t *testing.T) {
	got := Str(strings.Repeat("x", maxStrLen+100))
	if !strings.Contains(got, truncMark) {
		t.Errorf("oversized string not truncated: %q", got)
	}
	// maxStrLen bytes + marker + two quotes — comfortably under 2x the cap.
	if len(got) > maxStrLen+len(truncMark)+8 {
		t.Errorf("truncated output too long: len=%d", len(got))
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
