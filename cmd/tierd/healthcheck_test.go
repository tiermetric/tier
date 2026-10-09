package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostPort strips the scheme off an httptest server URL, since --addr takes a
// host:port rather than a URL in the normal container case.
func hostPort(t *testing.T, rawURL string) string {
	t.Helper()
	return strings.TrimPrefix(rawURL, "http://")
}

// runHealthcheckBounded runs the probe on a goroutine and fails the test if it
// does not return within hardStop.
//
// This exists because the obvious `elapsed > 5*time.Second` assertion CANNOT
// FIRE: elapsed is only computed after runHealthcheck returns, so a probe that
// never returns hangs the package until the go-test timeout and takes every
// other cmd/tierd result down with it. That is a ten-minute panic in place of
// the assertion that claims to catch it — the exact "a green that means never
// ran" shape, inverted. Measured: deleting the context deadline hangs rather
// than failing, so this wrapper is what makes the deadline arms real.
func runHealthcheckBounded(t *testing.T, hardStop time.Duration, args []string) (code int, elapsed time.Duration, stdout, stderr string) {
	t.Helper()
	type result struct {
		code           int
		elapsed        time.Duration
		stdout, stderr string
	}
	ch := make(chan result, 1)
	go func() {
		var out, errb bytes.Buffer
		start := time.Now()
		c := runHealthcheck(args, &out, &errb)
		ch <- result{c, time.Since(start), out.String(), errb.String()}
	}()
	select {
	case r := <-ch:
		return r.code, r.elapsed, r.stdout, r.stderr
	case <-time.After(hardStop):
		t.Fatalf("runHealthcheck(%v) did not return within %s — the timeout does not bound the exchange", args, hardStop)
		return 0, 0, "", ""
	}
}

// TestHealthcheck_LiveServerExitsZero is the POSITIVE arm (#571 criterion 1).
// On its own it proves almost nothing — see the negative arms below, which are
// what make this one evidence rather than a green that means "never ran".
func TestHealthcheck_LiveServerExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive","uptime_s":12,"version":"0.2.1"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := runHealthcheck([]string{"--addr", hostPort(t, srv.URL)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	// The probe must identify WHICH build answered, not merely that something
	// did: a stale process holding the port has answered 200 for a fresh one.
	if !strings.Contains(stdout.String(), `"version":"0.2.1"`) {
		t.Errorf("stdout should echo the livez body so docker inspect shows the build; got %q", stdout.String())
	}
}

// TestHealthcheck_AgainstRealHandler is the "verify the thing, not a proxy"
// arm. Every other test probes a hand-rolled stub, so nothing otherwise ties
// defaultHealthcheckPath to the route the server actually mounts, nor to its
// auth-exempt status. Rename that route or put it behind the token and this
// suite would stay green while every container went unhealthy.
//
// The token is configured ON PURPOSE: the probe has no credential, so this also
// pins that /livez stays reachable without one.
func TestHealthcheck_AgainstRealHandler(t *testing.T) {
	// newShipTestServer builds the same bearer-gated composition runServe
	// mounts, backed by a real store — reusing it keeps this arm tied to the
	// production wiring rather than to a second hand-rolled copy of it.
	srv, _ := newShipTestServer(t, "test-token-571")

	// Control arm: an authenticated route on the SAME server must NOT be
	// reachable without the token. Without this, "livez answered" could just
	// mean the token was never enforced, and the test above would be vacuous.
	code, _, _, _ := runHealthcheckBounded(t, 10*time.Second,
		[]string{"--addr", hostPort(t, srv.URL), "--path", "/api/v1/org_actual_spend", "--timeout", "5s"})
	if code == 0 {
		t.Fatal("control arm: a write-scoped route answered without a token — the token is not enforced, so this test cannot prove /livez is exempt")
	}

	// The real arm: default path, no token, must pass.
	code, _, stdout, stderr := runHealthcheckBounded(t, 10*time.Second,
		[]string{"--addr", hostPort(t, srv.URL), "--timeout", "5s"})
	if code != 0 {
		t.Fatalf("probing the REAL handler at the default path: exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"status":"alive"`) {
		t.Errorf("expected the real livez body; got %q", stdout)
	}
}

// TestHealthcheck_DeadAddressExitsNonZero is THE control arm (#571 criterion
// 2). If this ever passes, the probe is not probing anything.
func TestHealthcheck_DeadAddressExitsNonZero(t *testing.T) {
	addr := deadAddr(t)

	code, _, stdout, stderr := runHealthcheckBounded(t, 10*time.Second,
		[]string{"--addr", addr, "--timeout", "2s"})
	if code == 0 {
		t.Fatalf("exit code = 0 for a dead address, want non-zero (stdout: %s)", stdout)
	}
	if !strings.Contains(stderr, "unreachable") {
		t.Errorf("stderr should say the address was unreachable; got %q", stderr)
	}
}

// deadAddr returns a host:port nothing is listening on. It binds and closes so
// the port is one the OS just confirmed assignable — a hardcoded "surely
// nothing runs here" port is a flaky test waiting to happen.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// TestHealthcheck_BoundButNotAnsweringExitsNonZero is the arm that separates
// this probe from a bare TCP connect test (#571 criterion 3). A wedged process
// still holds its listener and still completes a TCP handshake; only an
// end-to-end HTTP exchange with a deadline can tell it from a serving one.
func TestHealthcheck_BoundButNotAnsweringExitsNonZero(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Accept connections and never write a response. Hold them until the test
	// ends so the client sees a silent peer rather than an EOF (an immediate
	// close would fail for the wrong reason and prove nothing about deadlines).
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				<-done
				_ = conn.Close()
			}()
		}
	}()

	code, elapsed, stdout, _ := runHealthcheckBounded(t, 5*time.Second,
		[]string{"--addr", ln.Addr().String(), "--timeout", "300ms"})
	if code == 0 {
		t.Fatalf("exit code = 0 against a bound-but-silent port, want non-zero (stdout: %s)", stdout)
	}
	// Assert it failed because the DEADLINE fired, not because the connection
	// broke some other way — "failed for the stated reason" is the point.
	if elapsed < 250*time.Millisecond {
		t.Errorf("returned after %s, too fast to have hit the 300ms deadline — it likely failed for another reason", elapsed)
	}
}

// TestHealthcheck_HeadersThenWedgeExitsNonZero covers the wedge ONE LAYER UP
// from a dead port: a handler that writes its status line and then blocks. A
// goroutine stuck after WriteHeader — say, on the single-writer SQLite mutex —
// still emits 200. Counting that as healthy would let `docker ps` report
// healthy forever while no response ever completes.
func TestHealthcheck_HeadersThenWedgeExitsNonZero(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64") // promise a body that never arrives
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	code, _, stdout, stderr := runHealthcheckBounded(t, 5*time.Second,
		[]string{"--addr", hostPort(t, srv.URL), "--timeout", "300ms"})
	if code == 0 {
		t.Fatalf("exit 0 for a 200 whose body never arrived, want non-zero (stdout: %s)", stdout)
	}
	if !strings.Contains(stderr, "never completed") {
		t.Errorf("stderr should say the response never completed; got %q", stderr)
	}
}

// TestHealthcheck_SlowButWithinBudgetExitsZero is the other half of the
// deadline contract: it must not fire EARLY, and must not be applied per-read.
func TestHealthcheck_SlowButWithinBudgetExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	}))
	defer srv.Close()

	code, _, _, stderr := runHealthcheckBounded(t, 10*time.Second,
		[]string{"--addr", hostPort(t, srv.URL), "--timeout", "5s"})
	if code != 0 {
		t.Fatalf("a server answering well inside the budget must pass; exit = %d (stderr: %s)", code, stderr)
	}
}

// TestHealthcheck_StatusCodeBoundaries pins the 2xx window at its edges. 199
// and 300 must fail, 200/204/299 must pass — an off-by-one in the comparison
// is otherwise invisible.
func TestHealthcheck_StatusCodeBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantExit int
	}{
		// No 1xx arm: net/http consumes informational responses internally and
		// hands the caller the FINAL status, so a "199" is unreachable through
		// a real client. The `< 200` half of the window check is therefore
		// defensive only — stated here rather than left as an untested branch
		// someone later assumes is covered.
		{"200_ok", http.StatusOK, 0},
		{"204_no_content", http.StatusNoContent, 0},
		{"299_top_of_2xx", 299, 0},
		{"300_multiple_choices", http.StatusMultipleChoices, 1},
		{"307_temporary_redirect", http.StatusTemporaryRedirect, 1},
		{"308_permanent_redirect", http.StatusPermanentRedirect, 1},
		{"401_unauthorized", http.StatusUnauthorized, 1},
		{"404_not_found", http.StatusNotFound, 1},
		{"503_degraded", http.StatusServiceUnavailable, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// A 204 must not carry a body; writing one would make net/http
				// complain and muddy the arm.
				if c.status != http.StatusNoContent {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(c.status)
				if c.status != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"error":"nope"}`))
				}
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			got := runHealthcheck([]string{"--addr", hostPort(t, srv.URL)}, &stdout, &stderr)
			if got != c.wantExit {
				t.Fatalf("HTTP %d: exit = %d, want %d (stdout %q, stderr %q)", c.status, got, c.wantExit, stdout.String(), stderr.String())
			}
			if c.wantExit != 0 {
				// Assert the STATUS is named, anchored to the message shape —
				// a bare Contains(stderr, "503") can be satisfied by digits in
				// an ephemeral port number in the URL.
				want := fmt.Sprintf("returned %d", c.status)
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr should contain %q so an operator can tell 503-degraded from 401-misconfigured; got %q", want, stderr.String())
				}
			}
		})
	}
}

// TestHealthcheck_DoesNotFollowRedirects guards against the probe silently
// reporting on a DIFFERENT target than the operator named.
func TestHealthcheck_DoesNotFollowRedirects(t *testing.T) {
	var healthyHit atomic.Bool
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, healthy.URL, http.StatusFound)
	}))
	defer redirector.Close()

	var stdout, stderr bytes.Buffer
	code := runHealthcheck([]string{"--addr", hostPort(t, redirector.URL)}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("a redirect must not count as healthy; exit code = 0 (stdout: %s)", stdout.String())
	}
	if healthyHit.Load() {
		t.Error("the probe followed the redirect and reported on a different server")
	}
}

// TestHealthcheck_BodyIsBounded defends maxHealthcheckBody. The probe's output
// is stored in the container's inspect state, so an arbitrarily large body must
// not be able to bloat it.
func TestHealthcheck_BodyIsBounded(t *testing.T) {
	big := strings.Repeat("A", 64*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := runHealthcheck([]string{"--addr", hostPort(t, srv.URL)}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if n := strings.Count(stdout.String(), "A"); n > maxHealthcheckBody {
		t.Errorf("echoed %d body bytes, want at most maxHealthcheckBody=%d", n, maxHealthcheckBody)
	}
	if len(stdout.String()) > maxHealthcheckBody+256 {
		t.Errorf("stdout is %d bytes; the probe must not be able to bloat docker inspect state", len(stdout.String()))
	}
}

// TestHealthcheck_SnippetIsSanitized — the body lands verbatim in an operator's
// terminal via `docker inspect`, so control bytes must not survive.
func TestHealthcheck_SnippetIsSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("alive\n\x1b[31mFAKE ALERT\x00"))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := runHealthcheck([]string{"--addr", hostPort(t, srv.URL)}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	// Exactly one trailing newline — the one the probe itself prints.
	if n := strings.Count(stdout.String(), "\n"); n != 1 {
		t.Errorf("snippet injected newlines into the health log: %q", stdout.String())
	}
	if strings.ContainsAny(stdout.String(), "\x1b\x00") {
		t.Errorf("control bytes survived sanitization: %q", stdout.String())
	}
}

// TestHealthcheck_FlagErrors — a mistyped flag in the Dockerfile HEALTHCHECK
// must NOT exit 0. This is the false-green path that matters most here: it
// would report `healthy` forever for a container serving nothing.
func TestHealthcheck_FlagErrors(t *testing.T) {
	t.Run("unknown_flag_is_non_zero", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"--adrr", "127.0.0.1:1"}, &stdout, &stderr); code == 0 {
			t.Error("a mistyped flag exited 0 — a Dockerfile typo would report healthy forever")
		}
	})
	t.Run("help_is_zero", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"--help"}, &stdout, &stderr); code != 0 {
			t.Errorf("--help exit = %d, want 0", code)
		}
	})
	t.Run("malformed_addr_is_non_zero", func(t *testing.T) {
		for _, addr := range []string{"http://exa mple:80", "http://[::1", "http://\x7f/"} {
			var stdout, stderr bytes.Buffer
			if code := runHealthcheck([]string{"--addr", addr, "--timeout", "2s"}, &stdout, &stderr); code == 0 {
				t.Errorf("--addr %q exited 0, want non-zero", addr)
			}
		}
	})
	t.Run("non_positive_timeout_is_non_zero", func(t *testing.T) {
		for _, arg := range []string{"0s", "-1s"} {
			var stdout, stderr bytes.Buffer
			if code := runHealthcheck([]string{"--timeout", arg}, &stdout, &stderr); code == 0 {
				t.Errorf("--timeout %s: exit code = 0, want non-zero", arg)
			}
			if !strings.Contains(stderr.String(), "timeout") {
				t.Errorf("--timeout %s: stderr should name the flag; got %q", arg, stderr.String())
			}
		}
	})
}

// TestHealthcheck_DefaultPath pins the default to the auth-exempt liveness
// endpoint and covers the documented --path override.
func TestHealthcheck_DefaultPath(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPath string
	}{
		{"default_is_livez", nil, defaultHealthcheckPath},
		{"override_to_healthz", []string{"--path", "/api/v1/healthz"}, "/api/v1/healthz"},
		{"path_without_leading_slash", []string{"--path", "api/v1/healthz"}, "/api/v1/healthz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPath := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case gotPath <- r.URL.Path:
				default:
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			args := append([]string{"--addr", hostPort(t, srv.URL)}, c.args...)
			if code := runHealthcheck(args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
			}
			select {
			case got := <-gotPath:
				if got != c.wantPath {
					t.Errorf("path = %q, want %q", got, c.wantPath)
				}
			default:
				t.Fatal("handler never ran")
			}
		})
	}
	// /api/v1/livez is the endpoint that stays OPEN for probes; moving the
	// default onto an authenticated route would make every container with a
	// token configured go unhealthy. TestHealthcheck_AgainstRealHandler proves
	// the route exists and is exempt; this pins the constant it depends on.
	if defaultHealthcheckPath != "/api/v1/livez" {
		t.Errorf("default path is %q, want /api/v1/livez", defaultHealthcheckPath)
	}
}

// TestHealthcheck_DefaultAddrTracksServe — the probe's default is only correct
// while it matches what serve/demo actually bind. They now share one constant;
// this fails if that is ever unpicked back into separate literals.
func TestHealthcheck_DefaultAddrTracksServe(t *testing.T) {
	t.Setenv(healthcheckAddrEnv, "")
	if got := defaultHealthcheckAddr(); got != defaultListenAddr {
		t.Errorf("healthcheck default addr %q != serve/demo default %q — a container would probe the wrong port",
			got, defaultListenAddr)
	}
	// main.go may contain the literal EXACTLY ONCE: the `const
	// defaultListenAddr` declaration itself. An earlier version of this guard
	// exempted main.go entirely with `&& f != "main.go"`, which made the
	// iteration a file read with no assertion — and main.go is where `serve`
	// lives, the command a container actually runs. Mutation-proven: with the
	// exemption, re-hardcoding the literal into serve's --addr flag left this
	// test reporting ok.
	limits := map[string]int{"main.go": 1, "demo.go": 0}
	for f, allowed := range limits {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if n := bytes.Count(src, []byte(`"`+defaultListenAddr+`"`)); n > allowed {
			t.Errorf("%s contains the listen-address literal %d times (allowed %d) — use defaultListenAddr instead of re-hardcoding it",
				f, n, allowed)
		}
	}
}

// TestHealthcheck_AddrEnvOverride — an exec-form HEALTHCHECK does no shell
// expansion, so this env var is the ONLY way an operator can retarget the probe
// when the server binds a non-default address. Without it, `serve --addr
// 0.0.0.0:9090` leaves the container permanently unhealthy while serving fine.
func TestHealthcheck_AddrEnvOverride(t *testing.T) {
	t.Run("env_sets_the_default", func(t *testing.T) {
		t.Setenv(healthcheckAddrEnv, "10.11.12.13:9090")
		if got := defaultHealthcheckAddr(); got != "10.11.12.13:9090" {
			t.Errorf("defaultHealthcheckAddr() = %q, want the env value", got)
		}
	})
	t.Run("blank_env_falls_back", func(t *testing.T) {
		t.Setenv(healthcheckAddrEnv, "   ")
		if got := defaultHealthcheckAddr(); got != defaultListenAddr {
			t.Errorf("a blank env must fall back to %q; got %q", defaultListenAddr, got)
		}
	})
	t.Run("explicit_flag_beats_env", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		// Env points somewhere dead; --addr points at the live server. The flag
		// must win, or an operator cannot override the override.
		t.Setenv(healthcheckAddrEnv, deadAddr(t))
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"--addr", hostPort(t, srv.URL)}, &stdout, &stderr); code != 0 {
			t.Errorf("--addr must take precedence over %s; exit = %d (stderr %s)", healthcheckAddrEnv, code, stderr.String())
		}
	})
}

// TestHealthcheck_RejectsRetargetingAddr — "host:port@otherhost:port" parses as
// URL userinfo, so raw concatenation would probe a DIFFERENT machine and exit
// 0. This is the same failure the redirect policy and the nil Proxy already
// close, arriving through a third door.
func TestHealthcheck_RejectsRetargetingAddr(t *testing.T) {
	var elsewhereHit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	addr := "127.0.0.1:1@" + hostPort(t, elsewhere.URL)
	var stdout, stderr bytes.Buffer
	if code := runHealthcheck([]string{"--addr", addr, "--timeout", "2s"}, &stdout, &stderr); code == 0 {
		t.Errorf("--addr %q exited 0 — the probe reported on a host the operator did not name", addr)
	}
	if elsewhereHit.Load() {
		t.Error("the probe contacted the userinfo host instead of the named one")
	}
	if !strings.Contains(stderr.String(), "credentials") {
		t.Errorf("stderr should explain the '@' rejection; got %q", stderr.String())
	}
}

// TestHealthcheck_RejectsPositionalArgs — `healthcheck --addr X /api/v1/healthz`
// would otherwise probe the DEFAULT path and exit 0, reporting on something the
// operator did not ask for. Same class as a mistyped flag.
func TestHealthcheck_RejectsPositionalArgs(t *testing.T) {
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := runHealthcheck([]string{"--addr", hostPort(t, srv.URL), "/api/v1/healthz"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("a stray positional exited 0 while probing the default path (stdout %q)", stdout.String())
	}
	select {
	case p := <-gotPath:
		t.Errorf("it probed %q instead of failing — the operator's argument was silently dropped", p)
	default:
	}
}

func TestHealthcheckURL(t *testing.T) {
	cases := []struct {
		name, addr, path, want string
	}{
		{"host_port", "127.0.0.1:8080", "/api/v1/livez", "http://127.0.0.1:8080/api/v1/livez"},
		{"path_without_leading_slash", "127.0.0.1:8080", "api/v1/livez", "http://127.0.0.1:8080/api/v1/livez"},
		{"full_http_url", "http://1.2.3.4:9", "/api/v1/livez", "http://1.2.3.4:9/api/v1/livez"},
		{"full_https_url", "https://1.2.3.4:9", "/api/v1/livez", "https://1.2.3.4:9/api/v1/livez"},
		{"uppercase_scheme_not_double_prefixed", "HTTP://1.2.3.4:9", "/api/v1/livez", "HTTP://1.2.3.4:9/api/v1/livez"},
		{"url_with_trailing_slash", "http://1.2.3.4:9/", "/api/v1/livez", "http://1.2.3.4:9/api/v1/livez"},
		{"ipv6", "[::1]:8080", "/api/v1/livez", "http://[::1]:8080/api/v1/livez"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := healthcheckURL(c.addr, c.path)
			if err != nil {
				t.Fatalf("healthcheckURL(%q, %q): unexpected error: %v", c.addr, c.path, err)
			}
			if got != c.want {
				t.Errorf("healthcheckURL(%q, %q) = %q, want %q", c.addr, c.path, got, c.want)
			}
		})
	}
}

// TestHealthcheck_DispatchWiring asserts the subcommand is reachable through
// dispatch and listed in usage — a subcommand the Dockerfile calls but the
// binary does not route would fail only at container runtime.
func TestHealthcheck_DispatchWiring(t *testing.T) {
	t.Run("unknown_command_control", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := dispatch([]string{"healthchekc"}, &stdout, &stderr); code == 0 {
			t.Fatal("a typo'd subcommand must not exit 0 — the control arm for the case below")
		}
	})
	t.Run("routed", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := dispatch([]string{"healthcheck", "--addr", deadAddr(t), "--timeout", "2s"}, &stdout, &stderr)
		// Exit 1 because nothing is listening — but crucially NOT the unknown
		// command path. Assert POSITIVE evidence that the probe itself ran:
		// keying on main.go's "unknown command" wording means a reword
		// silently stops the guard from guarding while still reporting PASS.
		if code == 0 {
			t.Errorf("exit code = 0 against a dead address, want non-zero")
		}
		if !strings.Contains(stderr.String(), "healthcheck:") {
			t.Errorf("dispatch did not reach runHealthcheck (no probe output): %q", stderr.String())
		}
	})
	t.Run("listed_in_usage", func(t *testing.T) {
		var usage bytes.Buffer
		printUsage(&usage)
		if !strings.Contains(usage.String(), "healthcheck") {
			t.Errorf("printUsage omits healthcheck:\n%s", usage.String())
		}
	})
}

// TestDockerfileHealthcheck automates #571 criteria 4 and 6 WITHOUT a Docker
// daemon. The regressions they guard are textual, and the shell-form one is the
// one that actually breaks: `HEALTHCHECK CMD tierd healthcheck` (no JSON array)
// is silently run as `/bin/sh -c`, and this base has no /bin/sh — it would fail
// at container runtime with "no such file or directory", never here.
//
// Measured during development: exactly that mistake made a control container go
// unhealthy for the WRONG REASON, which looked like a passing negative arm.
func TestDockerfileHealthcheck(t *testing.T) {
	src, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	df := string(src)

	// Join the HEALTHCHECK instruction across backslash continuations. Done in
	// Go rather than one clever regex: a greedy [^\n]* swallows the trailing
	// backslash and silently matches only the first line, which is how the
	// first version of this test failed to see the CMD at all.
	var hc string
	lines := strings.Split(df, "\n")
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "HEALTHCHECK") {
			continue
		}
		hc = ln
		for strings.HasSuffix(strings.TrimSpace(hc), `\`) && i+1 < len(lines) {
			i++
			hc = strings.TrimSuffix(strings.TrimSpace(hc), `\`) + " " + strings.TrimSpace(lines[i])
		}
		break
	}
	if hc == "" {
		t.Fatal("Dockerfile declares no HEALTHCHECK — docker ps would report a bare 'Up', and Config.Healthcheck would be NONE")
	}

	// EXEC form: the CMD payload must be a JSON array. Shell form needs /bin/sh.
	cmdIdx := strings.Index(hc, "CMD")
	if cmdIdx < 0 {
		t.Fatalf("HEALTHCHECK has no CMD: %q", hc)
	}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(hc[cmdIdx+3:]), "\\"))
	payload = strings.TrimSpace(payload)
	if !strings.HasPrefix(payload, "[") {
		t.Fatalf("HEALTHCHECK CMD must be EXEC form (a JSON array) — shell form becomes /bin/sh -c and this base has no shell; got %q", payload)
	}

	// argv[0] must be the same binary ENTRYPOINT names — catches path drift.
	entry := regexp.MustCompile(`(?m)^ENTRYPOINT\s+\["([^"]+)"`).FindStringSubmatch(df)
	if entry == nil {
		t.Fatal("could not parse ENTRYPOINT")
	}
	argv := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(payload, -1)
	if len(argv) < 2 {
		t.Fatalf("HEALTHCHECK CMD needs at least [binary, subcommand]; got %q", payload)
	}
	if argv[0][1] != entry[1] {
		t.Errorf("HEALTHCHECK argv[0] = %q but ENTRYPOINT is %q — path drift between the two", argv[0][1], entry[1])
	}
	if argv[1][1] != "healthcheck" {
		t.Errorf("HEALTHCHECK argv[1] = %q, want \"healthcheck\"", argv[1][1])
	}
	// And that token must actually route, or the probe fails only in a container.
	var stdout, stderr bytes.Buffer
	if code := dispatch([]string{argv[1][1], "--addr", deadAddr(t), "--timeout", "2s"}, &stdout, &stderr); strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("the Dockerfile calls %q but dispatch does not route it (exit %d)", argv[1][1], code)
	}

	// Docker's own --timeout must exceed the probe's default, so the probe
	// self-bounds and reports its reason before Docker kills it. That
	// relationship is load-bearing and was previously coincidental.
	m := regexp.MustCompile(`--timeout=(\d+)s`).FindStringSubmatch(hc)
	if m == nil {
		t.Fatalf("HEALTHCHECK declares no --timeout: %q", hc)
	}
	dockerTimeout, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse --timeout: %v", err)
	}
	if time.Duration(dockerTimeout)*time.Second <= defaultHealthcheckTimeout {
		t.Errorf("Docker --timeout=%ds must exceed the probe's own default %s, or Docker kills the probe before it can report why",
			dockerTimeout, defaultHealthcheckTimeout)
	}
}
