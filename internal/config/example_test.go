package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/config"
)

// TestConfigExampleLoads is the drift guard for config.example.yaml (#127):
// the shipped example must always parse cleanly and mention EVERY config key.
//
// It relies on two properties of the loader working together:
//   - config.Load uses strict KnownFields, so any typo'd or non-existent key in
//     the example fails Load — the example can never document a key the loader
//     would reject.
//   - Every schema field is a pointer or slice, absent from the parsed result
//     (nil) unless the YAML actually set it. Reflection-walking the parsed
//     Config and asserting each exported field is non-nil therefore proves every
//     key round-trips through Load — i.e. the example is COMPLETE, not just valid.
//
// Add a field to config.Config and this test fails until config.example.yaml
// documents it, keeping the example honest forever.
func TestConfigExampleLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}

	// collectors.* are OPTIONAL pollers, DISABLED by default (#377): shipping
	// them ACTIVE would either fail startup (the @/etc/tier secret files don't
	// exist on a fresh machine) or silently enable an outbound poller on a
	// placeholder key — so the example ships them commented out. Exempt the
	// Collectors subtree from the non-nil "key present" walk; TestConfigExample
	// DocumentsCollectors below keeps those keys honest via a text scan instead.
	exempt := map[string]bool{"Config.Collectors": true}
	assertFullyPopulated(t, reflect.ValueOf(*cfg), "Config", exempt)
}

// TestConfigExampleFreshMachineSafe pins the #377 contract: the shipped example
// must start `tierd serve` verbatim on a fresh machine with no root-owned paths
// and no secret files. The db path must be relative (created under cwd, always
// writable) rather than an absolute /var/lib path MkdirAll can't create, and
// both optional pollers must be DISABLED so no @/etc/tier secret is read at
// startup. This is the regression guard for the paper cut a stranger hit first.
func TestConfigExampleFreshMachineSafe(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}
	if cfg.DB == nil {
		t.Fatal("db key missing from config.example.yaml")
	}
	if filepath.IsAbs(*cfg.DB) {
		t.Errorf("db = %q is absolute; the example must ship a relative path so serve starts verbatim on a fresh machine", *cfg.DB)
	}
	if cfg.Collectors.AnthropicAdmin != nil {
		t.Error("collectors.anthropic_admin must be DISABLED (commented out) in the example — an active block reads @/etc/tier and fails startup verbatim")
	}
	if cfg.Collectors.OpenAIUsage != nil {
		t.Error("collectors.openai_usage must be DISABLED (commented out) in the example — an active block reads @/etc/tier and fails startup verbatim")
	}
}

// TestConfigExampleDocumentsCollectors replaces, for the commented-out collector
// blocks, the "every key is documented" guarantee that assertFullyPopulated can
// no longer enforce via reflection (#377). The optional pollers stay disabled by
// default but MUST remain discoverable in the example, so scan the file text for
// each collector key — a future edit that drops the documentation fails here.
func TestConfigExampleDocumentsCollectors(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	text := string(body)
	// Derive the expected keys from the struct's yaml tags (not a hand-kept
	// list) so a NEW collector field or poller added to CollectorsConfig /
	// AnthropicAdminConfig / OpenAIUsageConfig is automatically required to stay
	// documented in the commented-out example — re-closing the drift-detection
	// hole that exempting Config.Collectors from the reflection walk opens.
	collectorsField, ok := reflect.TypeOf(config.Config{}).FieldByName("Collectors")
	if !ok {
		t.Fatal("Config has no Collectors field — did the schema change?")
	}
	keys := append([]string{yamlKey(collectorsField.Tag)}, collectorYAMLKeys(collectorsField.Type)...)
	for _, key := range keys {
		if key == "" {
			continue
		}
		if !strings.Contains(text, key+":") {
			t.Errorf("config.example.yaml no longer documents collector key %q", key+":")
		}
	}
}

// yamlKey returns the yaml field name from a struct tag, stripping ",omitempty"
// and other options ("" for an absent or "-" tag).
func yamlKey(tag reflect.StructTag) string {
	name := strings.Split(tag.Get("yaml"), ",")[0]
	if name == "-" {
		return ""
	}
	return name
}

// collectorYAMLKeys returns the yaml key names for every field of a struct type,
// descending into pointer-to-struct fields so nested blocks (anthropic_admin ->
// api_key/org/poll_interval) are included.
func collectorYAMLKeys(t reflect.Type) []string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if name := yamlKey(f.Tag); name != "" {
			keys = append(keys, name)
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			keys = append(keys, collectorYAMLKeys(ft)...)
		}
	}
	return keys
}

// assertFullyPopulated recurses over v's exported fields asserting KEY presence,
// not value content: every pointer must be non-nil and every slice non-nil (an
// explicit `[]` in YAML yields a non-nil empty slice; an absent key yields nil),
// so a missing key surfaces as a nil the walk reports. Nested structs are walked
// in turn. An empty string behind a non-nil pointer (e.g. api_token: "") is a
// legitimate value, so scalar contents are deliberately NOT checked — only that
// the key was present enough for the loader to allocate its pointer.
func assertFullyPopulated(t *testing.T, v reflect.Value, path string, exempt map[string]bool) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		typ := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if !typ.Field(i).IsExported() {
				continue
			}
			childPath := path + "." + typ.Field(i).Name
			// An exempt subtree (e.g. the optional, disabled-by-default
			// collectors block, #377) is documented via a text scan instead of
			// the non-nil walk — skip it here so a legitimately-absent key
			// isn't reported as a dropped one.
			if exempt[childPath] {
				continue
			}
			assertFullyPopulated(t, v.Field(i), childPath, exempt)
		}
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("%s: nil — key missing from config.example.yaml", path)
			return
		}
		// Recurse only into nested structs; a non-nil scalar pointer (e.g.
		// *string) has already proven its key is present.
		if v.Elem().Kind() == reflect.Struct {
			assertFullyPopulated(t, v.Elem(), path, exempt)
		}
	case reflect.Slice:
		if v.IsNil() {
			t.Errorf("%s: nil slice — key missing from config.example.yaml (use [] to set it explicitly)", path)
		}
	case reflect.Map:
		// A map has the same observable-absence property as a slice: an absent key
		// yields a nil map, while an explicit `{}` yields a non-nil empty one. So it
		// is checkable, and the guard keeps guarding.
		if v.IsNil() {
			t.Errorf("%s: nil map — key missing from config.example.yaml (use {} to set it explicitly)", path)
		}
	default:
		// The walk can only prove key PRESENCE for fields whose absence is
		// observable — pointers (nil), slices (nil), and maps (nil). A bare scalar
		// (string, int, bool) zeroes to a value indistinguishable from "key absent",
		// so this guard could not detect it being dropped from the example. Fail
		// loudly the moment such a field is added rather than let the guard silently
		// stop guarding: make the new field a pointer (so absence is detectable) or
		// extend this walk to cover its kind.
		t.Errorf("%s: unhandled kind %s — this guard only detects dropped keys for "+
			"pointer/slice/map fields; make the field a pointer or extend assertFullyPopulated", path, v.Kind())
	}
}
