package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYAML drops body into a temp file and returns its path. Tests use
// t.TempDir so cleanup happens automatically.
func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

// TestLoad_FullSchema parses a YAML with every supported key and verifies
// the round-trip. The pointer-field choice is intentional: a present key
// produces a non-nil pointer; an absent key stays nil so the wiring in
// cmd/tierd can distinguish "config didn't say" from "config explicitly
// set this to empty."
func TestLoad_FullSchema(t *testing.T) {
	path := writeYAML(t, `
http:
  addr: ":9999"
  webhook_secret: "secret-1"
  api_token: "tok-1"
  read_token: "read-tok-1"
db: /tmp/tier.db
prices_file: /etc/tier/prices.yaml
zero_outcome_window_days: 14
outcomes:
  push_capture: true
  generated_paths:
    - "vendor/"
    - "*.pb.go"
proxy:
  anthropic_target: "https://anthropic.local"
  openai_target: "https://openai.local"
watch:
  repos:
    - /repos/a
    - /repos/b
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr == nil || *cfg.HTTP.Addr != ":9999" {
		t.Errorf("addr = %v, want \":9999\"", cfg.HTTP.Addr)
	}
	if cfg.HTTP.WebhookSecret == nil || *cfg.HTTP.WebhookSecret != "secret-1" {
		t.Errorf("webhook_secret = %v", cfg.HTTP.WebhookSecret)
	}
	if cfg.HTTP.APIToken == nil || *cfg.HTTP.APIToken != "tok-1" {
		t.Errorf("api_token = %v", cfg.HTTP.APIToken)
	}
	if cfg.HTTP.ReadToken == nil || *cfg.HTTP.ReadToken != "read-tok-1" {
		t.Errorf("read_token = %v", cfg.HTTP.ReadToken)
	}
	if cfg.DB == nil || *cfg.DB != "/tmp/tier.db" {
		t.Errorf("db = %v", cfg.DB)
	}
	if cfg.PricesFile == nil || *cfg.PricesFile != "/etc/tier/prices.yaml" {
		t.Errorf("prices_file = %v", cfg.PricesFile)
	}
	if cfg.ZeroOutcomeWindowDays == nil || *cfg.ZeroOutcomeWindowDays != 14 {
		t.Errorf("zero_outcome_window_days = %v, want 14", cfg.ZeroOutcomeWindowDays)
	}
	if cfg.Outcomes.PushCapture == nil || *cfg.Outcomes.PushCapture != true {
		t.Errorf("outcomes.push_capture = %v, want true", cfg.Outcomes.PushCapture)
	}
	if want := []string{"vendor/", "*.pb.go"}; len(cfg.Outcomes.GeneratedPaths) != 2 ||
		cfg.Outcomes.GeneratedPaths[0] != want[0] || cfg.Outcomes.GeneratedPaths[1] != want[1] {
		t.Errorf("outcomes.generated_paths = %v, want %v", cfg.Outcomes.GeneratedPaths, want)
	}
	if cfg.Proxy.AnthropicTarget == nil || *cfg.Proxy.AnthropicTarget != "https://anthropic.local" {
		t.Errorf("anthropic_target = %v", cfg.Proxy.AnthropicTarget)
	}
	if cfg.Proxy.OpenAITarget == nil || *cfg.Proxy.OpenAITarget != "https://openai.local" {
		t.Errorf("openai_target = %v", cfg.Proxy.OpenAITarget)
	}
	want := []string{"/repos/a", "/repos/b"}
	if len(cfg.Watch.Repos) != 2 || cfg.Watch.Repos[0] != want[0] || cfg.Watch.Repos[1] != want[1] {
		t.Errorf("watch.repos = %v, want %v", cfg.Watch.Repos, want)
	}
}

// TestLoad_PartialKeysLeaveOthersNil verifies the absent-vs-empty
// distinction: only http.addr is set; everything else stays nil so the
// caller's wiring leaves CLI/env defaults in place.
func TestLoad_PartialKeysLeaveOthersNil(t *testing.T) {
	path := writeYAML(t, "http:\n  addr: \":8080\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr == nil || *cfg.HTTP.Addr != ":8080" {
		t.Errorf("addr = %v, want \":8080\"", cfg.HTTP.Addr)
	}
	if cfg.HTTP.WebhookSecret != nil {
		t.Errorf("webhook_secret should be nil for absent key, got %v", cfg.HTTP.WebhookSecret)
	}
	if cfg.HTTP.ReadToken != nil {
		t.Errorf("read_token should be nil for absent key, got %v", cfg.HTTP.ReadToken)
	}
	if cfg.DB != nil {
		t.Errorf("db should be nil for absent key, got %v", cfg.DB)
	}
	if cfg.PricesFile != nil {
		t.Errorf("prices_file should be nil for absent key, got %v", cfg.PricesFile)
	}
	if cfg.ZeroOutcomeWindowDays != nil {
		t.Errorf("zero_outcome_window_days should be nil for absent key, got %v", cfg.ZeroOutcomeWindowDays)
	}
	if cfg.Outcomes.PushCapture != nil {
		t.Errorf("outcomes.push_capture should be nil for absent key, got %v", cfg.Outcomes.PushCapture)
	}
	if cfg.Outcomes.GeneratedPaths != nil {
		t.Errorf("outcomes.generated_paths should be nil for absent key, got %v", cfg.Outcomes.GeneratedPaths)
	}
	if cfg.Proxy.AnthropicTarget != nil {
		t.Errorf("anthropic_target should be nil for absent key, got %v", cfg.Proxy.AnthropicTarget)
	}
}

// TestLoad_ExplicitEmptyOverridesDefault: a key present with value "" is
// distinguishable from absent. The caller's wiring treats this as
// "operator wants empty" — useful for disabling a proxy by setting the
// URL to empty string in the config.
func TestLoad_ExplicitEmptyOverridesDefault(t *testing.T) {
	path := writeYAML(t, `
proxy:
  anthropic_target: ""
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.AnthropicTarget == nil {
		t.Fatal("anthropic_target should be non-nil when explicitly empty")
	}
	if *cfg.Proxy.AnthropicTarget != "" {
		t.Errorf("anthropic_target = %q, want empty string", *cfg.Proxy.AnthropicTarget)
	}
}

// TestLoad_RejectsUnknownFields covers the strict-parsing contract: a
// typo'd key (e.g. webook_secret instead of webhook_secret) must surface
// as a parse error so the operator notices at startup instead of silently
// running with flag defaults.
func TestLoad_RejectsUnknownFields(t *testing.T) {
	path := writeYAML(t, `
http:
  webook_secret: "typo"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "webook_secret") {
		t.Errorf("error should name the unknown field; got %v", err)
	}
}

// TestLoad_MissingFile returns a wrapped os.ErrNotExist so callers can
// errors.Is to give a clearer message ("did you forget to create the file?"
// vs "permission denied?").
func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should wrap os.ErrNotExist; got %v", err)
	}
}

// TestLoad_EmptyPath rejects the zero-value path argument — defensive
// against a future caller that forgets to gate on --config != "".
func TestLoad_EmptyPath(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestLoad_TrustedProxyCIDRs covers the #131 key: the list parses into the
// string slice, an absent key stays nil (so the caller leaves the CLI flag /
// default in place), and an unknown sibling key still fails strict parsing.
func TestLoad_TrustedProxyCIDRs(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		path := writeYAML(t, `
http:
  trusted_proxy_cidrs:
    - "10.0.0.0/8"
    - "192.168.0.0/16"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := []string{"10.0.0.0/8", "192.168.0.0/16"}
		if len(cfg.HTTP.TrustedProxyCIDRs) != 2 ||
			cfg.HTTP.TrustedProxyCIDRs[0] != want[0] ||
			cfg.HTTP.TrustedProxyCIDRs[1] != want[1] {
			t.Errorf("trusted_proxy_cidrs = %v, want %v", cfg.HTTP.TrustedProxyCIDRs, want)
		}
	})
	t.Run("absent_is_nil", func(t *testing.T) {
		path := writeYAML(t, "http:\n  addr: \":8080\"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTP.TrustedProxyCIDRs != nil {
			t.Errorf("trusted_proxy_cidrs should be nil for absent key, got %v", cfg.HTTP.TrustedProxyCIDRs)
		}
	})
	t.Run("unknown_sibling_still_rejected", func(t *testing.T) {
		path := writeYAML(t, `
http:
  trusted_proxy_cidrs: ["10.0.0.0/8"]
  trusted_proxy_cdirs: ["typo"]
`)
		if _, err := Load(path); err == nil {
			t.Fatal("expected strict-mode error for unknown sibling key")
		}
	})
}

// TestLoad_AuthBlock covers the #85 http.auth block: all three keys round-trip
// into non-nil pointers with exact values; an absent block leaves all three nil so
// the caller keeps the flag/env/builtin defaults; and a partial block populates only
// the keys present. Durations are carried as strings on purpose (see AuthConfig doc:
// a bare YAML int would be nanoseconds), so the assertions compare the raw string.
func TestLoad_AuthBlock(t *testing.T) {
	t.Run("all_present", func(t *testing.T) {
		path := writeYAML(t, `
http:
  auth:
    max_failures: 5
    failure_window: "30s"
    lockout: "10m"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTP.Auth.MaxFailures == nil || *cfg.HTTP.Auth.MaxFailures != 5 {
			t.Errorf("max_failures = %v, want 5", cfg.HTTP.Auth.MaxFailures)
		}
		if cfg.HTTP.Auth.FailureWindow == nil || *cfg.HTTP.Auth.FailureWindow != "30s" {
			t.Errorf("failure_window = %v, want \"30s\"", cfg.HTTP.Auth.FailureWindow)
		}
		if cfg.HTTP.Auth.Lockout == nil || *cfg.HTTP.Auth.Lockout != "10m" {
			t.Errorf("lockout = %v, want \"10m\"", cfg.HTTP.Auth.Lockout)
		}
	})
	t.Run("block_absent_all_nil", func(t *testing.T) {
		path := writeYAML(t, "http:\n  addr: \":8080\"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTP.Auth.MaxFailures != nil {
			t.Errorf("max_failures should be nil for absent block, got %v", cfg.HTTP.Auth.MaxFailures)
		}
		if cfg.HTTP.Auth.FailureWindow != nil {
			t.Errorf("failure_window should be nil for absent block, got %v", cfg.HTTP.Auth.FailureWindow)
		}
		if cfg.HTTP.Auth.Lockout != nil {
			t.Errorf("lockout should be nil for absent block, got %v", cfg.HTTP.Auth.Lockout)
		}
	})
	t.Run("partial_block_only_lockout", func(t *testing.T) {
		path := writeYAML(t, `
http:
  auth:
    lockout: "1h"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTP.Auth.Lockout == nil || *cfg.HTTP.Auth.Lockout != "1h" {
			t.Errorf("lockout = %v, want \"1h\"", cfg.HTTP.Auth.Lockout)
		}
		if cfg.HTTP.Auth.MaxFailures != nil {
			t.Errorf("max_failures should be nil when only lockout set, got %v", cfg.HTTP.Auth.MaxFailures)
		}
		if cfg.HTTP.Auth.FailureWindow != nil {
			t.Errorf("failure_window should be nil when only lockout set, got %v", cfg.HTTP.Auth.FailureWindow)
		}
	})
	t.Run("max_failures_zero_is_present_not_absent", func(t *testing.T) {
		// The pointer discipline must distinguish an explicit `max_failures: 0`
		// (operator disabling the limiter) from an omitted key. A non-nil pointer to
		// 0 is the "disable" signal; nil is "use the default".
		path := writeYAML(t, "http:\n  auth:\n    max_failures: 0\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTP.Auth.MaxFailures == nil || *cfg.HTTP.Auth.MaxFailures != 0 {
			t.Errorf("max_failures = %v, want non-nil 0 (explicit disable)", cfg.HTTP.Auth.MaxFailures)
		}
	})
}

// TestLoad_AuthUnknownKeyRejected pins strict KnownFields decoding at the nested
// http.auth level (#85): a typo'd key inside the block (max_failurs) must fail
// startup, not silently fall back to the default. Complements the http- and
// proxy-sibling cases; asserts rejection without pinning error text (parser-behavior
// contract that must survive the #52 yaml swap).
func TestLoad_AuthUnknownKeyRejected(t *testing.T) {
	path := writeYAML(t, `
http:
  auth:
    max_failurs: 3
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected strict-mode error for typo'd key under http.auth")
	}
}

// TestLoad_InvalidYAML covers the syntactically-broken case: a value
// without a closing quote produces a clear parse error pointing at the
// file path.
func TestLoad_InvalidYAML(t *testing.T) {
	path := writeYAML(t, "http:\n  addr: \"unclosed\nthing")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the path; got %v", err)
	}
}

// TestLoad_RejectsUnknownNestedKeyUnderProxy pins strict decoding at a SECOND
// nesting level (proxy, complementing the http-sibling case in
// TestLoad_TrustedProxyCIDRs). Strict unknown-key rejection is the single
// load-bearing property of the YAML loader (#52): a typo'd key must fail startup
// at ANY depth, not only the top level. This is a parser-behavior contract pin —
// it must hold identically before and after the yaml.v3 -> go.yaml.in/yaml/v3
// swap, which is why it asserts rejection without pinning the error text.
func TestLoad_RejectsUnknownNestedKeyUnderProxy(t *testing.T) {
	path := writeYAML(t, `
proxy:
  anthropic_target: "https://anthropic.local"
  anthropci_target: "typo"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected strict-mode error for unknown key nested under proxy")
	}
}

// TestLoad_RejectsDuplicateKey pins that a duplicated mapping key is a decode
// ERROR, not silent last-wins. yaml.v3 v3.0.1 rejects duplicate keys; the
// replacement parser must preserve that, because silent last-wins on a
// security-relevant key (e.g. two api_token lines) would let a later stanza
// override an earlier one with no warning. A duplicate is never legitimate
// operator intent, so rejecting it is fail-loud (matching this codebase's
// posture). Contract pin for the #52 swap -- asserts rejection, not error text.
func TestLoad_RejectsDuplicateKey(t *testing.T) {
	path := writeYAML(t, "db: /tmp/a.db\ndb: /tmp/b.db\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate top-level key, got nil (silent last-wins)")
	}
}

// TestLoad_SizeLabels covers the #244 outcomes.size_labels key: a present map of
// on-scale weights round-trips, an absent key stays nil (so the handler keeps its
// built-in table), and an explicit {} is a non-nil empty map (also "use the
// defaults"). Only label NAMES are configurable — the WEIGHTS must stay on the
// fixed 0.5/1/3/5/8 outcome scale, so an off-scale weight or a blank label name is
// rejected at Load so a misconfiguration fails loud at startup.
func TestLoad_SizeLabels(t *testing.T) {
	t.Run("present_on_scale", func(t *testing.T) {
		path := writeYAML(t, `
outcomes:
  size_labels:
    "size: xs": 0.5
    "s": 1
    "size-m": 3
    "L": 5
    "xxl": 8
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := cfg.Outcomes.SizeLabels
		if got == nil {
			t.Fatal("size_labels should be non-nil when present")
		}
		want := map[string]float64{"size: xs": 0.5, "s": 1, "size-m": 3, "L": 5, "xxl": 8}
		if len(got) != len(want) {
			t.Fatalf("size_labels = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("size_labels[%q] = %v, want %v", k, got[k], v)
			}
		}
	})
	t.Run("absent_is_nil", func(t *testing.T) {
		path := writeYAML(t, "http:\n  addr: \":8080\"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Outcomes.SizeLabels != nil {
			t.Errorf("size_labels should be nil for absent key, got %v", cfg.Outcomes.SizeLabels)
		}
	})
	t.Run("explicit_empty_is_non_nil", func(t *testing.T) {
		path := writeYAML(t, "outcomes:\n  size_labels: {}\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Outcomes.SizeLabels == nil {
			t.Error("size_labels should be non-nil empty map for explicit {}")
		}
		if len(cfg.Outcomes.SizeLabels) != 0 {
			t.Errorf("size_labels should be empty for {}, got %v", cfg.Outcomes.SizeLabels)
		}
	})
	t.Run("off_scale_weight_rejected", func(t *testing.T) {
		path := writeYAML(t, "outcomes:\n  size_labels:\n    \"m\": 2\n")
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for off-scale weight 2")
		}
		if !strings.Contains(err.Error(), "size_labels") {
			t.Errorf("error should name size_labels; got %v", err)
		}
	})
	t.Run("blank_label_name_rejected", func(t *testing.T) {
		path := writeYAML(t, "outcomes:\n  size_labels:\n    \"  \": 1\n")
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for blank label name")
		}
		if !strings.Contains(err.Error(), "size_labels") {
			t.Errorf("error should name size_labels; got %v", err)
		}
	})
	t.Run("case_collision_rejected", func(t *testing.T) {
		// Two names that fold to the same token would collapse into one entry with
		// a nondeterministic winner once the handler lowercases keys, so Load must
		// reject them rather than silently pick one.
		path := writeYAML(t, "outcomes:\n  size_labels:\n    \"M\": 3\n    \" m \": 5\n")
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for case/whitespace-colliding label names")
		}
		if !strings.Contains(err.Error(), "size_labels") {
			t.Errorf("error should name size_labels; got %v", err)
		}
	})
}
