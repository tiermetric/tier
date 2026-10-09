package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// Build identity over the wire (#638).
//
// 🔴 What this file is really guarding, because it is easy to write a version
// test that cannot fail. The point of /version is that an operator can ASSERT a
// deployment is the build they think it is. An assertion is only worth anything
// if it says NO to the wrong build — so every "it reports X" arm below is paired
// with a control arm proving the same comparison REJECTS a different build.
//
// The issue's own acceptance criterion states it: "an assertion that the endpoint
// reports the expected build must FAIL when pointed at a deliberately older
// binary. Otherwise 'the version matches' is indistinguishable from 'the check
// never ran'."

// stamps builds a pinned vcsInfo for a test. Injected PER HANDLER rather than
// swapped on a package global: a global mutated by tests races every concurrent
// handler read the moment any test in this package calls t.Parallel().
//
// Without a seam at all, the only assertable fact would be "some string came
// back", which a hardcoded value satisfies just as well — a test that cannot fail.
func stamps(revision string, modified bool, goVersion string) vcsInfo {
	return vcsInfo{Stamped: true, Commit: revision, Modified: modified, GoVersion: goVersion}
}

func getVersion(t *testing.T, h *Handler, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux) // read-only: the mode the public demo runs in
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal %s: %v (body=%q)", path, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func newVersionHandler(t *testing.T, version string, opts ...Option) *Handler {
	t.Helper()
	return New(nil, slog.New(slog.DiscardHandler), "", nil, version, RateLimitConfig{}, opts...)
}

// TestVersionEndpoint_ReportsBuild_AndRejectsAWrongOne is the core arm plus its
// control. The literals here are deliberate: they are NOT read from the code
// under test, so narrowing the handler cannot flip the expectation along with it
// (the tautological-accept-arm defect, false-green ledger 12).
func TestVersionEndpoint_ReportsBuild_AndRejectsAWrongOne(t *testing.T) {
	const (
		wantVersion = "0.4.0"
		wantCommit  = "ca27d9f07c0838da15595e9fad97047b389866bd"
		otherCommit = "0da8186aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	code, body := getVersion(t, newVersionHandler(t, wantVersion, withVCSStamps(stamps(wantCommit, false, "go1.26.5"))), "/api/v1/version")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/version = %d, want 200", code)
	}

	// deployMatches is the assertion an operator actually makes: "is the thing
	// running the build I published?" Both arms below drive THIS function, so the
	// control genuinely exercises the predicate rather than a restatement of it.
	deployMatches := func(b map[string]any, version, commit string) bool {
		return b["version"] == version && b["commit"] == commit
	}

	if !deployMatches(body, wantVersion, wantCommit) {
		t.Fatalf("expected build not reported: version=%v commit=%v", body["version"], body["commit"])
	}

	// 🔴 CONTROL ARM — the whole reason this test exists. The SAME predicate,
	// against a DIFFERENT build, must say no. If this ever passes, the arm above
	// proves nothing and "the version matches" means "the check never ran".
	if deployMatches(body, wantVersion, otherCommit) {
		t.Error("CONTROL FAILED: a different commit was accepted — the assertion " +
			"does not discriminate, so the positive arm above is worthless")
	}
	if deployMatches(body, "0.3.0", wantCommit) {
		t.Error("CONTROL FAILED: a different version was accepted — same problem")
	}

	// Y1 (review): go_version was a documented contract field with ZERO assertions
	// anywhere in the repo — a mutant returning "go0.0.0-WRONG" survived the whole
	// suite. Pinned to a literal, not read from the code under test.
	if body["go_version"] != "go1.26.5" {
		t.Errorf("go_version = %v, want \"go1.26.5\"", body["go_version"])
	}

	// The commit must not be the version wearing a different name: a tagged
	// release reports the same `version` however it was built, which is exactly
	// why commit was added. Equal values here would mean the field is decorative.
	if body["commit"] == body["version"] {
		t.Error("commit equals version — commit is not carrying independent identity")
	}
}

// TestVersionEndpoint_DiscriminatesTwoBuildsOfTheSameTag is the failure mode that
// motivated #638 and that `version` alone CANNOT catch: a tag rebuilt from a
// different tree reports an identical version string. Only commit separates them.
func TestVersionEndpoint_DiscriminatesTwoBuildsOfTheSameTag(t *testing.T) {
	const tag = "0.4.0"

	_, first := getVersion(t, newVersionHandler(t, tag, withVCSStamps(stamps("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, "go1.26.5"))), "/api/v1/version")

	_, second := getVersion(t, newVersionHandler(t, tag, withVCSStamps(stamps("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", false, "go1.26.5"))), "/api/v1/version")

	if first["version"] != second["version"] {
		t.Fatalf("fixture broken: the two builds must share a version to exercise this, got %v and %v",
			first["version"], second["version"])
	}
	if first["commit"] == second["commit"] {
		t.Fatal("two builds of the same tag are indistinguishable — commit is not being read")
	}
}

// TestVersionEndpoint_ReportsDirtyTree: a `-dirty` build must SAY so. An operator
// verifying a release needs to know the artifact was built from an unclean tree.
func TestVersionEndpoint_ReportsDirtyTree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modified bool
	}{{"clean", false}, {"dirty", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVersionHandler(t, "0.4.0",
				withVCSStamps(stamps("cccccccccccccccccccccccccccccccccccccccc", tc.modified, "go1.26.5")))
			_, body := getVersion(t, h, "/api/v1/version")
			if got, ok := body["modified"].(bool); !ok || got != tc.modified {
				t.Errorf("modified = %v (ok=%v), want %v", body["modified"], ok, tc.modified)
			}
		})
	}
}

// TestVersionEndpoint_MountedInReadOnlyMode. #638 exists because the DEPLOYMENT
// hardest to identify is the public demo — and the demo runs read-only. A version
// endpoint absent from that mode would be useless exactly where it is needed.
//
// The write-route arm is the control: it proves RegisterReadOnly is really being
// exercised, so "GET /version is mounted" is not vacuously true of a mux that
// happens to mount everything.
func TestVersionEndpoint_MountedInReadOnlyMode(t *testing.T) {
	h := newVersionHandler(t, "0.4.0", withVCSStamps(stamps("dddddddddddddddddddddddddddddddddddddddd", false, "go1.26.5")))

	for _, path := range []string{"/api/v1/version", "/api/v1/livez"} {
		if code, _ := getVersion(t, h, path); code != http.StatusOK {
			t.Errorf("read-only GET %s = %d, want 200", path, code)
		}
	}

	// CONTROL: a write route MUST be structurally absent from the same mux —
	// otherwise "GET /version is mounted" is vacuously true of a mux that mounted
	// everything, and the arms above prove nothing.
	//
	// ⚠️ The absent status is 404 OR 405, and asserting 404 alone is WRONG. Go
	// 1.22+ ServeMux answers 405 when the PATH is registered under a different
	// method — and `GET /api/v1/events` IS a read route, so a read-only mux gives
	// POST /api/v1/events → 405. Measured: this test failed on a 404-only
	// assertion while the security property held perfectly. What actually matters
	// is that no POST HANDLER ran: a mounted write route would answer 401/403
	// (requireAuth) or 2xx, never 405.
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)
	for _, path := range []string{"/api/v1/events", "/api/v1/costs", "/api/v1/outcomes"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		switch rec.Code {
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			// absent — correct
		default:
			t.Fatalf("CONTROL FAILED: POST %s = %d in read-only mode; a write handler "+
				"answered (401/403 = mounted-but-gated, 2xx = mounted and accepting). "+
				"This mux is not read-only, so the assertions above prove nothing",
				path, rec.Code)
		}
	}
}

// TestLivez_CarriesCommit_WithoutBreakingItsExistingShape. /livez is the endpoint
// operators already have wired, so commit was added there too — but additively.
// Every field the previous contract promised must still be present and typed the
// same, or a deployed probe breaks on upgrade.
func TestLivez_CarriesCommit_WithoutBreakingItsExistingShape(t *testing.T) {
	const commit = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	code, body := getVersion(t, newVersionHandler(t, "0.4.0", withVCSStamps(stamps(commit, false, "go1.26.5"))), "/api/v1/livez")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/livez = %d, want 200", code)
	}
	// Pinned to literals, not to the struct — a renamed json tag must fail here.
	if body["status"] != "alive" {
		t.Errorf("status = %v, want \"alive\"", body["status"])
	}
	if _, ok := body["uptime_s"].(float64); !ok {
		t.Errorf("uptime_s missing or not a number: %v", body["uptime_s"])
	}
	if body["version"] != "0.4.0" {
		t.Errorf("version = %v, want \"0.4.0\"", body["version"])
	}
	if body["commit"] != commit {
		t.Errorf("commit = %v, want %q", body["commit"], commit)
	}

	// G3 (review): /livez's commit omitempty was unasserted while /version's was
	// guarded. An unstamped build must omit the field, not emit "".
	_, unstamped := getVersion(t, newVersionHandler(t, "0.4.0", withVCSStamps(vcsInfo{})), "/api/v1/livez")
	if c, present := unstamped["commit"]; present {
		t.Errorf("livez commit = %v on an unstamped build, want the field omitted", c)
	}
	// The pre-existing contract fields must still be there in that case.
	if unstamped["status"] != "alive" || unstamped["version"] != "0.4.0" {
		t.Errorf("livez lost its existing fields on an unstamped build: %v", unstamped)
	}
}

// TestVersionEndpoint_DegradesWhenBuildInfoUnavailable. A binary built with
// -buildvcs=false has no stamps. Reporting what is known beats a 500: an endpoint
// whose job is diagnosing a deployment must not fail on the deployments hardest
// to diagnose.
func TestVersionEndpoint_DegradesWhenBuildInfoUnavailable(t *testing.T) {
	// Stamped:false is the SHIPPED CONTAINER's real state — .dockerignore excludes
	// .git, so the toolchain records nothing. Measured on the published v0.4.0
	// image: zero vcs settings. This is not an exotic -buildvcs=false case.
	h := newVersionHandler(t, "0.4.0", withVCSStamps(vcsInfo{}))
	code, body := getVersion(t, h, "/api/v1/version")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/version = %d with no build info, want 200", code)
	}
	if body["version"] != "0.4.0" {
		t.Errorf("version = %v, want the ldflags value to survive", body["version"])
	}
	if c, present := body["commit"]; present {
		t.Errorf("commit = %v, want the field omitted when unknown (omitempty) rather "+
			"than an empty string a caller could compare against", c)
	}
	// 🔴 THE POINT OF THE TRI-STATE. `modified` must be ABSENT, not false. An
	// earlier draft emitted a plain bool, so a binary with no stamps published
	// `"modified": false` — a confident, unfounded attestation that its tree was
	// clean. That is strictly worse than silence, and it was the SHIPPED
	// container's behaviour.
	if m, present := body["modified"]; present {
		t.Errorf("modified = %v, want the field ABSENT when the toolchain stamped "+
			"nothing — reporting false here asserts a clean tree the binary knows "+
			"nothing about", m)
	}
	// Platform comes from the runtime, not from build stamps, so it must survive.
	if body["platform"] != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("platform = %v, want %q", body["platform"], runtime.GOOS+"/"+runtime.GOARCH)
	}
}

// TestVersionEndpoint_PriceTableIsSnakeCase guards the shape defect caught in
// review: store.PriceTableInfo carries NO json tags, so embedding it directly
// emits {"Version":9,"EffectiveDate":…} — PascalCase in a snake_case API and a
// DIFFERENT shape from the price_table /scores has always returned.
func TestVersionEndpoint_PriceTableIsSnakeCase(t *testing.T) {
	_, body := getVersion(t, newVersionHandler(t, "0.4.0", withVCSStamps(stamps("ffffffffffffffffffffffffffffffffffffffff", false, "go1.26.5"))), "/api/v1/version")

	pt, ok := body["price_table"].(map[string]any)
	if !ok {
		t.Fatalf("price_table missing or not an object: %v", body["price_table"])
	}
	if _, ok := pt["version"]; !ok {
		t.Errorf("price_table.version absent — got keys %v", jsonKeysOf(pt))
	}
	if _, ok := pt["effective_date"]; !ok {
		t.Errorf("price_table.effective_date absent — got keys %v", jsonKeysOf(pt))
	}
	// CONTROL: the PascalCase spellings must NOT be present. Without this the test
	// passes on a struct emitting both, which is the shape defect itself.
	for _, bad := range []string{"Version", "EffectiveDate", "ModelCount"} {
		if _, present := pt[bad]; present {
			t.Errorf("price_table carries PascalCase key %q — store.PriceTableInfo is "+
				"being marshalled directly instead of priceTableJSON", bad)
		}
	}

	// Y2 (review): this test checked KEY PRESENCE only, so a mutant hardcoding
	// version 1 / 1970-01-01 SURVIVED the entire suite. priceTableJSON has no
	// omitempty, so the keys are present for ANY value — key presence is a shape
	// guard that cannot see content. Assert the VALUES (ledger 17: assert the
	// number, not the key).
	active := store.ActivePriceTableInfo()
	if got, want := pt["version"], float64(active.Version); got != want {
		t.Errorf("price_table.version = %v, want %v — the endpoint is not reporting "+
			"the ACTIVE table", got, want)
	}
	if got, want := pt["effective_date"], active.EffectiveDate; got != want {
		t.Errorf("price_table.effective_date = %v, want %v", got, want)
	}
	// Guard the guard: if the active table were ever the zero value, the two
	// assertions above would compare 0 to 0 and pass while measuring nothing.
	if active.Version == 0 {
		t.Fatal("active price table version is 0 — the assertions above are vacuous")
	}
}

func jsonKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBuildIdentity_InjectedCommitWins is the guard on RED-1's fix, and it exists
// because a mutation deleting the injected-commit read SURVIVED the rest of this
// file: every other test pins the VCS stamps, so nothing exercised precedence.
//
// 🔴 The middle case is the SHIPPED CONTAINER. .dockerignore excludes .git, so the
// image has NO stamps — measured on the published v0.4.0 image, zero vcs settings,
// while the release tarball from the same run carried vcs.revision=ca27d9f0…. If
// the injected value did not win (or was not read at all), /version would report
// nothing on the exact deployment #638 was filed about, and every test here would
// still be green.
func TestBuildIdentity_InjectedCommitWins(t *testing.T) {
	const (
		injected = "1111111111111111111111111111111111111111"
		stamped  = "2222222222222222222222222222222222222222"
	)

	for _, tc := range []struct {
		name   string
		commit string
		vcs    vcsInfo
		want   string
	}{
		{"injected beats stamped", injected, stamps(stamped, false, "go1.26.5"), injected},
		{"CONTAINER: injected, no stamps at all", injected, vcsInfo{}, injected},
		{"stamped used when nothing injected", "", stamps(stamped, false, "go1.26.5"), stamped},
		{"neither: field omitted, never guessed", "", vcsInfo{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVersionHandler(t, "0.4.0", WithCommit(tc.commit), withVCSStamps(tc.vcs))
			_, body := getVersion(t, h, "/api/v1/version")
			got, present := body["commit"]
			if tc.want == "" {
				if present {
					t.Fatalf("commit = %v, want the field ABSENT", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("commit = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestReadVCSStamps_ExercisesTheRealToolchainPath. Every other test in this file
// injects stamps, so readVCSStamps itself — the function that actually talks to
// debug.ReadBuildInfo — was covered by nothing. A mutation returning a wrong
// GoVersion SURVIVED the whole suite because the seam bypassed it.
//
// This is the shape of gap that let RED-1 ship: a seam makes the tests
// deterministic and, unwatched, makes the production path untested.
func TestReadVCSStamps_ExercisesTheRealToolchainPath(t *testing.T) {
	got := readVCSStamps()

	if got.GoVersion == "" {
		t.Error("GoVersion is empty — readVCSStamps is not reading debug.ReadBuildInfo")
	}
	if !strings.HasPrefix(got.GoVersion, "go") {
		t.Errorf("GoVersion = %q, want something starting with \"go\"", got.GoVersion)
	}
	// CONTROL: prove the value is real rather than a constant that happens to look
	// right — it must match the runtime this test is executing on.
	if got.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want runtime.Version() = %q", got.GoVersion, runtime.Version())
	}

	// The commit half is environment-dependent: a tree built with -buildvcs=false
	// or outside a repository legitimately has none. Skip rather than fail, but
	// SAY SO — a silent skip here is how "the real path is covered" becomes untrue.
	if !got.Stamped {
		t.Skip("this test binary carries no VCS stamps (-buildvcs=false or no repo); " +
			"the commit half of readVCSStamps is NOT covered in this environment")
	}
	if len(got.Commit) != 40 {
		t.Errorf("Commit = %q (len %d), want a 40-char git SHA", got.Commit, len(got.Commit))
	}
}
