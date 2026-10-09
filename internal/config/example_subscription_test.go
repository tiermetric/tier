package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/config"
)

// TestConfigExampleDocumentsSubscriptionFields closes a hole the existing
// example drift guard cannot see.
//
// TestConfigExampleLoads reflection-walks the parsed Config asserting every
// pointer/slice/map key is non-nil — which for `subscriptions:` proves only that
// the KEY is present. The shipped value is `subscriptions: []` (correctly: an
// active block would post fees on a fresh machine), so the ELEMENT struct is
// never walked and adding a field to config.Subscription would fail no test at
// all. That is exactly the "a guard silently stops guarding" failure the walk's
// own default branch fails loudly about for scalars.
//
// So: derive the expected keys from the struct's yaml tags — not a hand-kept
// list — and scan the example's text for each, the same technique
// TestConfigExampleDocumentsCollectors uses for the commented-out poller blocks.
// A new Subscription field is then required to be documented in the example
// before it can merge.
func TestConfigExampleDocumentsSubscriptionFields(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	text := string(body)

	typ := reflect.TypeOf(config.Subscription{})
	// NON-VACUITY: a reflection walk that found zero fields would pass every
	// assertion below while checking nothing.
	if typ.NumField() == 0 {
		t.Fatal("config.Subscription has no fields — the scan below would be vacuous")
	}
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		if !strings.Contains(text, key+":") {
			t.Errorf("config.example.yaml does not document subscriptions field %q — "+
				"add it to the commented example block", key+":")
		}
	}
}
