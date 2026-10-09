package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/docs"
)

// TestScoreClosingTip_DocsPathIsServed pins that the docs path the `tierd score`
// closing tip prints is a page the real docs handler returns 200 for, and that
// the URL it prints is on the default serve address (#799).
func TestScoreClosingTip_DocsPathIsServed(t *testing.T) {
	tip := scoreClosingTip()

	i := strings.Index(tip, "/docs/")
	if i < 0 {
		t.Fatalf("closing tip names no /docs/ path: %q", tip)
	}
	path := tip[i:]
	if j := strings.IndexAny(path, " \n"); j >= 0 {
		path = path[:j]
	}
	path = strings.TrimSuffix(path, ".") // sentence punctuation, not part of the path

	rec := httptest.NewRecorder()
	docs.New().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("closing tip points at %q; docs handler returned %d, want 200", path, rec.Code)
	}

	if want := "http://" + defaultListenAddr + path; !strings.Contains(tip, want) {
		t.Fatalf("closing tip does not name %q on the default serve address: %q", want, tip)
	}
}

// TestScoreClosingTip_NoBareServeOrBackfill pins that the tip never instructs a
// bare `tierd serve` or `tierd backfill` — both refuse to run without flags the
// quickstart supplies, and the quickstart URL is only live once a server runs —
// and that it names `tierd demo`, a subcommand the dispatcher accepts, as the
// no-setup way to read the quickstart now (#799).
func TestScoreClosingTip_NoBareServeOrBackfill(t *testing.T) {
	tip := scoreClosingTip()

	for _, cmd := range []string{"tierd serve", "tierd backfill"} {
		for _, instruction := range []string{"run `" + cmd + "`", "`" + cmd + "` then", "`" + cmd + "` to "} {
			if strings.Contains(strings.ToLower(tip), strings.ToLower(instruction)) {
				t.Errorf("closing tip instructs a bare %q (%q), which exits on missing required flags: %q", cmd, instruction, tip)
			}
		}
	}

	if !strings.Contains(tip, "`tierd demo`") {
		t.Fatalf("closing tip does not name `tierd demo` as the no-setup way to read the quickstart: %q", tip)
	}
	var out, errOut strings.Builder
	if rc := dispatch([]string{"demo", "-h"}, &out, &errOut); rc != 0 {
		t.Fatalf("`tierd demo -h` returned %d, want 0 (the tip names a subcommand the dispatcher rejects); stderr: %s", rc, errOut.String())
	}
	// Control arm: an unknown subcommand is refused, so the 0 above is earned.
	if rc := dispatch([]string{"no-such-subcommand"}, &out, &errOut); rc == 0 {
		t.Fatal("dispatcher accepted an unknown subcommand; the `tierd demo` check above proves nothing")
	}
}
