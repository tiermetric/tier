package main

// Tests for #452: `tierd doctor` probing a configured actual-cost collector's
// live credential. Split into four layers so each is honest about what it
// proves:
//
//  1. checkCollectorCredential — pure classification, no network at all.
//  2. checkCollectorConfigsAt against httptest servers — proves the WIRE
//     (config → resolve → Client.Probe → classify) end to end without ever
//     touching a real provider, so it runs in `make check`.
//  3. The anthropicadmin/openaiusage Client.Probe methods themselves are
//     tested in their own packages (client_probe_test.go) against httptest —
//     including the shipped Authorized()/Unauthorized() predicates, which
//     credProbe below deliberately does NOT exercise.
//  4. runDoctor itself, driven with real argv and a temp YAML — the ONLY layer
//     that proves the `--config` branch an operator actually types is wired to
//     layer 2 at all. Added by review RED-2: without it, deleting the probe
//     call from runDoctor compiled and passed layers 1-3 unchanged.
//
// None of these need a real credential — that is the live E2E's job (#451),
// which this doctor probe exists to make actionable ahead of.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/config"
)

func strPtr(s string) *string { return &s }

// credProbe builds a credentialProbeResult the way the real call sites do:
// authorized/unauthorized computed from statusCode/err, mirroring
// anthropicadmin.ProbeResult.Authorized()/Unauthorized() exactly (2xx =
// authorized; 401/403 = unauthorized). A local reimplementation, not a call
// into the provider package, is deliberate here — this file's layer 1 is
// "pure classification, no network, no provider dependency" by design (see
// the file doc); the wire tests further down independently prove the real
// provider methods agree with this shape end to end.
func credProbe(statusCode int, err error) credentialProbeResult {
	if err != nil {
		return credentialProbeResult{err: err}
	}
	return credentialProbeResult{
		statusCode:   statusCode,
		authorized:   statusCode >= 200 && statusCode < 300,
		unauthorized: statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden,
	}
}

// TestCheckCollectorCredential_Classification is a pure table test of the
// FAIL/WARN/OK decision — no network, no config, just the raw HTTP outcome in
// and a checkResult out.
func TestCheckCollectorCredential_Classification(t *testing.T) {
	cases := []struct {
		name   string
		result credentialProbeResult
		want   checkStatus
	}{
		{"200 OK is authorized", credProbe(200, nil), statusOK},
		{"299 boundary still 2xx", credProbe(299, nil), statusOK},
		{"401 is a credential FAIL, not unreachable", credProbe(401, nil), statusFail},
		{"403 is a credential FAIL", credProbe(403, nil), statusFail},
		{"429 is unexpected-but-reached, a WARN not a FAIL", credProbe(429, nil), statusWarn},
		{"500 is unexpected-but-reached, a WARN not a FAIL", credProbe(500, nil), statusWarn},
		{"transport error is unreachable FAIL", credProbe(0, context.DeadlineExceeded), statusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCollectorCredential("x credential", "collectors.x.api_key", tc.result)
			if got.status != tc.want {
				t.Errorf("status = %v, want %v (detail=%q)", got.status, tc.want, got.detail)
			}
		})
	}
}

// TestCheckCollectorCredential_401NotConfusedWithUnreachable is the CONTROL
// ARM proving the 401-vs-unreachable distinction actually discriminates: a
// mutant that classified every non-2xx (transport error OR any bad status)
// identically would pass every case above except this one, because 401 and
// "unreachable" produce the SAME statusFail status but must carry DIFFERENT,
// actionable hints (credential vs network). If this collapses, an operator
// with a perfectly reachable endpoint and a wrong key would be told to check
// their network instead of their key.
func TestCheckCollectorCredential_401NotConfusedWithUnreachable(t *testing.T) {
	unauthorized := checkCollectorCredential("x credential", "collectors.x.api_key", credProbe(401, nil))
	unreachable := checkCollectorCredential("x credential", "collectors.x.api_key", credProbe(0, context.DeadlineExceeded))
	if unauthorized.status != statusFail || unreachable.status != statusFail {
		t.Fatalf("both cases must FAIL to make this a meaningful test: got %v / %v", unauthorized.status, unreachable.status)
	}
	if strings.Contains(unauthorized.hint, "network") {
		t.Errorf("401 hint must point at the credential, not the network: %q", unauthorized.hint)
	}
	if !strings.Contains(unauthorized.hint, "collectors.x.api_key") {
		t.Errorf("401 hint must name the config key: %q", unauthorized.hint)
	}
	if strings.Contains(unreachable.hint, "collectors.x.api_key") {
		t.Errorf("unreachable hint must NOT blame the key: %q", unreachable.hint)
	}
	if unauthorized.detail == unreachable.detail {
		t.Errorf("401 and transport-error details must be distinguishable, both were %q", unauthorized.detail)
	}
}

// newProbeServer serves 200 for a request carrying wantAuthHeaderValue in the
// given header, 401 otherwise — a minimal stand-in for either provider's
// auth-gated cost endpoint.
func newProbeServer(t *testing.T, header, wantValue string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(header) != wantValue {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_page":""}`))
	}))
}

// TestCheckCollectorConfigsAt_AbsentBlocksProduceNoChecks proves an operator
// with no collectors configured gets NO rows (not a silent OK, not a spurious
// FAIL) — mirrors resolveAnthropicAdminConfig/resolveOpenAIUsageConfig's own
// "(nil, nil) = disabled" contract. This is also the control arm for the next
// two tests: it proves a PRESENT block is what causes a row to appear at all.
func TestCheckCollectorConfigsAt_AbsentBlocksProduceNoChecks(t *testing.T) {
	cfg := &config.Config{}
	got := checkCollectorConfigsAt(context.Background(), cfg, "unused", "unused")
	if len(got) != 0 {
		t.Fatalf("want 0 checks for an empty Collectors block, got %d: %+v", len(got), got)
	}
}

// TestCheckCollectorConfigsAt_InvalidBlockFailsWithoutNetworkCall proves a
// config block that resolveAnthropicAdminConfig itself would reject (missing
// org) produces a FAIL — and does so by pointing baseURL at a server that
// ALWAYS 500s, which only a real network call would reach. If a mutant made
// the invalid-config path fall through to probing anyway, this server would
// answer and the check's detail would contain the base URL's 500 rather than
// the "org is required" validation message — the assertion below pins the
// latter, not just the status.
func TestCheckCollectorConfigsAt_InvalidBlockFailsWithoutNetworkCall(t *testing.T) {
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("network call made for an invalid config block — should have failed validation first, hit %s", r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer trap.Close()

	cfg := &config.Config{}
	cfg.Collectors.AnthropicAdmin = &config.AnthropicAdminConfig{
		APIKey: strPtr("sk-ant-admin-test"),
		// Org deliberately omitted — resolveAnthropicAdminConfig requires it.
	}
	got := checkCollectorConfigsAt(context.Background(), cfg, trap.URL, "")
	if len(got) != 1 {
		t.Fatalf("want exactly 1 check, got %d: %+v", len(got), got)
	}
	if got[0].status != statusFail {
		t.Errorf("status = %v, want statusFail", got[0].status)
	}
	if !strings.Contains(got[0].detail, "org is required") {
		t.Errorf("detail should surface the config validation error, got %q", got[0].detail)
	}
}

// TestCheckCollectorConfigsAt_LiveWire_AnthropicAdmin drives the FULL wire —
// config → resolveAnthropicAdminConfig → anthropicadmin.NewClient →
// Client.Probe → checkCollectorCredential — against an httptest server,
// proving the doctor code path a real credential would exercise, without
// touching api.anthropic.com. Both a good and a bad key are asserted so the
// test cannot pass by the check always returning OK regardless of input.
func TestCheckCollectorConfigsAt_LiveWire_AnthropicAdmin(t *testing.T) {
	srv := newProbeServer(t, "x-api-key", "sk-ant-admin-good")
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Collectors.AnthropicAdmin = &config.AnthropicAdminConfig{
		APIKey: strPtr("sk-ant-admin-good"),
		Org:    strPtr("acme"),
	}
	got := checkCollectorConfigsAt(context.Background(), cfg, srv.URL, "")
	want := findCheck(t, got, "anthropic_admin credential")
	if want.status != statusOK {
		t.Errorf("good key: status = %v, want statusOK (detail=%q)", want.status, want.detail)
	}

	cfg.Collectors.AnthropicAdmin.APIKey = strPtr("sk-ant-admin-wrong")
	got = checkCollectorConfigsAt(context.Background(), cfg, srv.URL, "")
	bad := findCheck(t, got, "anthropic_admin credential")
	if bad.status != statusFail {
		t.Errorf("bad key: status = %v, want statusFail (detail=%q)", bad.status, bad.detail)
	}
	// Not just ANY FAIL: specifically the credential-rejected branch, not the
	// unreachable one — a mutant that made Probe return an Err for a 401
	// (e.g. by reverting to the retrying doGet path) would still leave
	// status==statusFail here, silently telling an operator with a
	// perfectly-reachable-but-wrong key to go debug their network instead.
	if !strings.Contains(bad.detail, "credential rejected") {
		t.Errorf("bad key: detail = %q, want it to say \"credential rejected\" (not \"endpoint unreachable\")", bad.detail)
	}
}

// TestCheckCollectorConfigsAt_LiveWire_OpenAIUsage is the OpenAI twin of the
// above, over the Bearer-auth wire.
func TestCheckCollectorConfigsAt_LiveWire_OpenAIUsage(t *testing.T) {
	srv := newProbeServer(t, "Authorization", "Bearer sk-openai-good")
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Collectors.OpenAIUsage = &config.OpenAIUsageConfig{
		APIKey: strPtr("sk-openai-good"),
		Org:    strPtr("acme"),
	}
	got := checkCollectorConfigsAt(context.Background(), cfg, "", srv.URL)
	want := findCheck(t, got, "openai_usage credential")
	if want.status != statusOK {
		t.Errorf("good key: status = %v, want statusOK (detail=%q)", want.status, want.detail)
	}

	cfg.Collectors.OpenAIUsage.APIKey = strPtr("sk-openai-wrong")
	got = checkCollectorConfigsAt(context.Background(), cfg, "", srv.URL)
	bad := findCheck(t, got, "openai_usage credential")
	if bad.status != statusFail {
		t.Errorf("bad key: status = %v, want statusFail (detail=%q)", bad.status, bad.detail)
	}
	if !strings.Contains(bad.detail, "credential rejected") {
		t.Errorf("bad key: detail = %q, want it to say \"credential rejected\" (not \"endpoint unreachable\")", bad.detail)
	}
}

// TestRunDoctor_NoConfigWarnsCollectorsNotChecked proves the visibility
// requirement: an operator who never passes --config sees an explicit WARN
// row saying so, rather than an all-green report that silently checked
// nothing about any configured collector.
func TestRunDoctor_NoConfigWarnsCollectorsNotChecked(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr strings.Builder
	_ = runDoctor([]string{"--repo", dir, "--claude-dir", dir}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "collector credentials") {
		t.Errorf("doctor output should mention the (unchecked) collector credentials row, got:\n%s", stdout.String())
	}
	// Not just that the row exists — that it says "not checked". Both the OK
	// and FAIL rows also contain the substring "collector credentials"-ish
	// text, so the check above alone can't tell "we verified it's fine" from
	// "we never looked".
	if !strings.Contains(stdout.String(), "not checked") {
		t.Errorf("doctor output should say the collector credentials were NOT CHECKED (no --config given), got:\n%s", stdout.String())
	}
}

// --- Layer 4: through runDoctor itself (review RED-2) -----------------------

// withDoctorProbeBaseURLs points runDoctor's production probe seam at test
// servers for the duration of one test, restoring the production zero value on
// cleanup. Not parallel-safe (package-level state), which is why no test in
// this file calls t.Parallel.
func withDoctorProbeBaseURLs(t *testing.T, anthropic, openai string) {
	t.Helper()
	prev := doctorProbeBaseURLs
	doctorProbeBaseURLs.anthropic = anthropic
	doctorProbeBaseURLs.openai = openai
	t.Cleanup(func() { doctorProbeBaseURLs = prev })
}

// writeDoctorConfig writes a minimal tierd YAML carrying one real
// collectors.anthropic_admin block and returns its path.
func writeDoctorConfig(t *testing.T, apiKey string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tierd.yaml")
	body := fmt.Sprintf("collectors:\n  anthropic_admin:\n    api_key: %q\n    org: \"acme\"\n", apiKey)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// TestRunDoctor_ConfigProbesCollectorCredential is the RED-2 mutant-killer.
// It drives runDoctor with the real `--config` argv an operator types and
// asserts on the REPORT TEXT, both directions.
//
// The mutant it kills — replacing runDoctor's
// `results = append(results, checkCollectorConfigs(ctx, cfg)...)` with
// `_ = cfg` — compiled and passed the entire suite, including `-tags
// integration`, because every other test in this file calls
// checkCollectorConfigsAt directly and so never crossed runDoctor's `--config`
// branch at all.
//
// Asserting the accepted/rejected TEXT rather than the process exit code is
// deliberate: doctor's exit code is a whole-report aggregate, and the temp
// --repo used here already produces unrelated WARNs, so a code-only assertion
// would neither prove the collector row appeared nor tell the two verdicts
// apart.
func TestRunDoctor_ConfigProbesCollectorCredential(t *testing.T) {
	srv := newProbeServer(t, "x-api-key", "sk-ant-admin-good")
	defer srv.Close()
	withDoctorProbeBaseURLs(t, srv.URL, "")

	t.Run("good key reports credential accepted", func(t *testing.T) {
		dir := t.TempDir()
		var stdout, stderr strings.Builder
		_ = runDoctor([]string{"--repo", dir, "--claude-dir", dir, "--config", writeDoctorConfig(t, "sk-ant-admin-good")}, &stdout, &stderr)
		out := stdout.String()
		// doctorRowFor t.Fatals if the row is absent — which IS the `_ = cfg`
		// mutant's signature: no collector row printed at all.
		row := doctorRowFor(t, out, "anthropic_admin credential")
		if !strings.Contains(row, "credential accepted") {
			t.Errorf("doctor did not report the credential as ACCEPTED. Row: %q\nFull output:\n%s", row, out)
		}
		if !strings.HasPrefix(row, "[OK") {
			t.Errorf("an accepted credential must be an OK row, got %q", row)
		}
	})

	t.Run("bad key reports credential rejected", func(t *testing.T) {
		dir := t.TempDir()
		var stdout, stderr strings.Builder
		rc := runDoctor([]string{"--repo", dir, "--claude-dir", dir, "--config", writeDoctorConfig(t, "sk-ant-admin-wrong")}, &stdout, &stderr)
		out := stdout.String()
		row := doctorRowFor(t, out, "anthropic_admin credential")
		if !strings.Contains(row, "credential rejected") {
			t.Errorf("doctor did not report the wrong key as REJECTED (a FAIL naming the credential, not the network). Row: %q\nFull output:\n%s", row, out)
		}
		// A rejected credential is a statusFail, and doctor's contract is to
		// exit non-zero when any check FAILs — the property a CI acceptance
		// step depends on.
		if rc == 0 {
			t.Errorf("runDoctor exit code = 0 with a rejected credential, want non-zero. Output:\n%s", out)
		}
	})
}

// TestRunDoctor_ConfigWithNoCollectorsIsExplicit closes the second half of
// RED-2, which is worse than a coverage hole. config.example.yaml ships its
// `collectors:` block COMMENTED OUT, so `tierd doctor --config
// config.example.yaml` used to print no collector row whatsoever — output
// byte-identical to what a silently-removed probe would produce. An operator
// could not tell "nothing is configured" from "the check stopped running".
//
// This asserts the disambiguating row exists, and is distinguishable from the
// no---config WARN (which says "not checked"): the two states have different
// remedies, so they must not print the same words.
func TestRunDoctor_ConfigWithNoCollectorsIsExplicit(t *testing.T) {
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a probe was made for a config with no collectors block: %s", r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer trap.Close()
	withDoctorProbeBaseURLs(t, trap.URL, trap.URL)

	// A VALID config that simply configures no collector — the shape
	// config.example.yaml ships. config.Load unmarshals strictly, so an
	// invented key would fail the load and exercise the wrong branch entirely.
	path := filepath.Join(t.TempDir(), "tierd.yaml")
	if err := os.WriteFile(path, []byte("http:\n  addr: \":8080\"\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	dir := t.TempDir()
	var stdout, stderr strings.Builder
	_ = runDoctor([]string{"--repo", dir, "--claude-dir", dir, "--config", path}, &stdout, &stderr)
	out := stdout.String()
	row := doctorRowFor(t, out, "collector credentials")
	if !strings.Contains(row, "no actual-cost collectors configured") {
		t.Errorf("a --config with no collectors block must say so explicitly, not print nothing. Row was %q, full output:\n%s", row, out)
	}
	// Scoped to THIS row on purpose: "not checked" also appears in the
	// unrelated identity-join WARN, so an output-wide search would pass
	// vacuously (or fail spuriously) without saying anything about the row
	// under test.
	if strings.Contains(row, "not checked") {
		t.Errorf("the \"nothing configured\" row must not reuse the no---config \"not checked\" wording — they mean different things. Row: %q", row)
	}
	if !strings.HasPrefix(row, "[OK") {
		t.Errorf("a valid JSONL-only install is not a warning or a failure; want an OK row, got %q", row)
	}
}

// doctorRowFor returns the single report line whose check name is `name`,
// failing the test if it is absent or duplicated. Assertions on doctor output
// are scoped through this rather than run against the whole report, because
// several rows share common phrases ("not checked") and a whole-output
// strings.Contains cannot tell which row supplied the match.
func doctorRowFor(t *testing.T, out, name string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		// Report format is "[STAT] <name>: <detail>" (reportDoctor); hint
		// lines are indented, so anchoring on "] " excludes them.
		if strings.HasPrefix(line, "[") && strings.Contains(line, "] "+name+":") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %q row in the doctor report, got %d. Output:\n%s", name, len(found), out)
	}
	return found[0]
}

// TestRunDoctor_ProbeBaseURLsDefaultToProduction is the CONTROL ARM for the
// seam itself. The override exists only for tests, so its production zero
// value must stay empty — the two Client constructors read empty as "use my
// production endpoint" (pinned in each provider package by
// TestProbe_EmptyBaseURLOverrideKeepsProductionDefault). A stray non-empty
// default here would silently point every operator's doctor at whatever host
// was left in it, and no other test would notice.
func TestRunDoctor_ProbeBaseURLsDefaultToProduction(t *testing.T) {
	if doctorProbeBaseURLs.anthropic != "" || doctorProbeBaseURLs.openai != "" {
		t.Fatalf("doctorProbeBaseURLs must be empty (= each Client's production default) outside a test override, got %+v", doctorProbeBaseURLs)
	}
}
