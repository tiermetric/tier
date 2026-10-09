package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

func TestVerifyReport_PricesLoadFailure(t *testing.T) {
	loadEmbeddedPriceTable(t)
	t.Setenv("TIER_PRICES", "")
	f := newVerifyFixture(t)
	before := store.ActivePriceTableInfo()
	for _, name := range []string{"missing", "malformed"} {
		t.Run(name, func(t *testing.T) {
			prices := filepath.Join(t.TempDir(), "prices.yaml")
			if name == "malformed" {
				if err := os.WriteFile(prices, []byte("models: ["), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath, "--prices", prices)
			if code != rcCannotCheck || !strings.Contains(errb, "--prices:") || !strings.Contains(errb, "NOTHING was verified") {
				t.Errorf("exit = %d, want 2 with prices load failure; stdout=%s stderr=%s", code, out, errb)
			}
			if got := store.ActivePriceTableInfo(); got != before {
				t.Errorf("failed load changed active price table: %+v -> %+v", before, got)
			}
		})
	}
}

func TestVerifyReport_PricesServedTable(t *testing.T) {
	for _, servedOverride := range []bool{true, false} {
		name := "override served"
		if !servedOverride {
			name = "embedded served"
		}
		t.Run(name, func(t *testing.T) {
			loadEmbeddedPriceTable(t)
			t.Setenv("TIER_PRICES", "")
			prices := writeCollidingTable(t, t.TempDir(), "override.yaml", store.ActivePriceTableInfo().Version+1000, 7.0)
			if servedOverride {
				if _, err := store.LoadPriceTable(prices); err != nil {
					t.Fatal(err)
				}
			}
			f := newVerifyFixture(t)
			loadEmbeddedPriceTable(t)
			code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath, "--prices", prices)
			if servedOverride {
				if code != rcReproduced || !strings.Contains(out, "REPRODUCED") {
					t.Fatalf("exit = %d, want REPRODUCED; stdout=%s stderr=%s", code, out, errb)
				}
				assertDimStatus(t, out, "price_table", "UNCHANGED")
			} else {
				if code != rcDiverged || strings.Contains(out, "UNATTRIBUTED") || strings.Contains(out, "recomputed report is IDENTICAL") {
					t.Fatalf("exit = %d, want stamp divergence ATTRIBUTED to price_table; stdout=%s stderr=%s", code, out, errb)
				}
				assertDimStatus(t, out, "price_table", "CHANGED")
			}
			for _, dim := range []string{
				"token_events", "outcomes", "quality revisions", "reprice",
				"cost corrections", "repo repairs", "push reconciliations",
				"events_digest", "outcomes_digest", "rubric", "tool_version",
			} {
				assertDimStatus(t, out, dim, "UNCHANGED")
			}
		})
	}
}

func TestVerifyReport_PricesSnapshotCollision(t *testing.T) {
	loadEmbeddedPriceTable(t)
	t.Setenv("TIER_PRICES", "")
	dir := t.TempDir()
	version := store.ActivePriceTableInfo().Version + 1000
	first := writeCollidingTable(t, dir, "first.yaml", version, 3.0)
	second := writeCollidingTable(t, dir, "second.yaml", version, 7.0)
	if _, err := store.LoadPriceTable(first); err != nil {
		t.Fatal(err)
	}
	f := newVerifyFixture(t)
	if _, err := store.LoadPriceTable(second); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath, "--prices", second)
	if code != rcCannotCheck {
		t.Fatalf("exit = %d, want 2 for a version collision; stdout=%s stderr=%s", code, out, errb)
	}
	for _, want := range []string{"open snapshot", "version collision", "give the override its own version", "NOTHING was verified"} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr missing %q: %s", want, errb)
		}
	}
	for _, unwanted := range []string{"forget-version", "verify-snapshot.db", "tierd-verify-"} {
		if strings.Contains(errb, unwanted) {
			t.Errorf("stderr contains unusable snapshot remedy %q: %s", unwanted, errb)
		}
	}
}

func TestVerifyReport_PricesSealedNotice(t *testing.T) {
	loadEmbeddedPriceTable(t)
	t.Setenv("TIER_PRICES", "")
	dbPath, path, _, _ := sealedFixture(t, sealTestDigest(1, "team", 5), false)
	prices := writeCollidingTable(t, filepath.Dir(dbPath), "override.yaml", store.ActivePriceTableInfo().Version+1000, 7.0)
	code, out, errb := runVerify(t, path, "--db", dbPath, "--prices", prices)
	if code != rcReproduced {
		t.Fatalf("exit = %d; stdout=%s stderr=%s", code, out, errb)
	}
	assertDimStatus(t, out, "--prices", "NOT CHECKED")
	if !strings.Contains(out, "validated but unused") {
		t.Fatalf("missing sealed prices notice: %s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "live recompute:") {
			if !strings.Contains(line, "would DIFFER") || !strings.Contains(line, "--prices changes the live recompute's price-table stamp") {
				t.Fatalf("live recompute must name the override's stamp change: %s", line)
			}
			return
		}
	}
	t.Fatalf("missing live recompute line: %s", out)
}
