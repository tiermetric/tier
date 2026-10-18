package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServe_IngestTokenDuplicateRefused(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "ingest-token")
	if err := os.WriteFile(tokenFile, []byte("same-1144\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"api-token", "read-token", "metrics-token"} {
		t.Run(other, func(t *testing.T) {
			code, out := runSecretEnvChild(t, map[string]string{"TIER_INGEST_TOKEN": "@" + tokenFile}, "serve", "--db", filepath.Join(dir, other+".db"), "--watch-repo", filepath.Join(dir, "not-a-repo"), "--aggregation", "team", "--addr", "0.0.0.0:0", "--"+other, "same-1144")
			if code != 1 || !strings.Contains(out, "--ingest-token must differ from --"+other) {
				t.Fatalf("exit=%d, want duplicate refusal for %s: %s", code, other, out)
			}
		})
	}
}

func TestServe_IngestTokenAloneCannotBind(t *testing.T) {
	// Drive the actual flag + bind wiring. Old code must fail on the unknown flag,
	// rather than passing a pre-existing unit test of validateBind unchanged.
	dir := t.TempDir()
	code, out := runSecretEnvChild(t, map[string]string{"HOME": dir}, "serve", "--db", filepath.Join(dir, "tier.db"), "--watch-repo", filepath.Join(dir, "not-a-repo"), "--aggregation", "team", "--addr", "0.0.0.0:0", "--ingest-token", "ingest-1144")
	if code != 1 || !strings.Contains(out, "refusing to bind") || !strings.Contains(out, "without an API token") {
		t.Fatalf("exit=%d, want non-loopback refusal: %s", code, out)
	}
}
