package main

import (
	"testing"
	"time"
)

// TestParseSince_DefaultBoundIsUTC guards the cmd/tierd copy of parseSince
// against the #180 non-UTC-bound defect. time.Now() carries the host's local
// zone; a bound that leaks a non-UTC offset mis-windows any ts >= ? comparison
// against UTC-stored rows because modernc.org/sqlite compares DATETIME values
// as offset-bearing strings. This CLI's since currently feeds only instant-safe
// paths, but the default bound must stay UTC so a future store-backed caller
// inherits a correct window. Fails on pre-fix main (Location was Local).
func TestParseSince_DefaultBoundIsUTC(t *testing.T) {
	got, err := parseSince("")
	if err != nil {
		t.Fatalf("parseSince(\"\") err = %v", err)
	}
	if loc := got.Location(); loc != time.UTC {
		t.Fatalf("default since bound Location = %v, want UTC (#180)", loc)
	}
}
