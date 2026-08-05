//go:build integration

package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a mutex-guarded io.Writer for the child's stderr. os/exec
// copies the child's stderr on its own goroutine, so a failure-path String()
// read from the test goroutine while the child is still running would race a
// concurrent Write without this guard.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMain lets this test binary re-exec ITSELF as `tierd`. runServe blocks
// until SIGTERM and calls os.Exit on a startup error, so it cannot run in the
// parent test process -- it must run in a child. This is the stdlib
// self-re-exec pattern used by net/http and os/exec's own tests: the child is
// selected purely by an env var, so no `go build` shell-out and no new deps.
//
// This TestMain is behind //go:build integration, so it only takes effect for
// `go test -tags integration ./cmd/tierd/` (make check-full). The untagged
// `make check` run has no TestMain and is unaffected.
func TestMain(m *testing.M) {
	if args := os.Getenv("TIERD_SMOKE_CHILD_ARGS"); args != "" {
		// Child role: BECOME tierd. dispatch is the injectable entry point
		// (main.go), so we route through it exactly as main() does. Args are
		// newline-separated: an env-var value cannot carry a NUL byte, and no
		// serve arg (subcommand, addr, temp path, aggregation) contains a newline.
		os.Exit(dispatch(strings.Split(args, "\n"), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// freeLoopbackPort asks the kernel for an unused loopback TCP port, then
// releases it so the child can bind it. There is a tiny window between release
// and re-bind, accepted here because it is loopback-only and the child rebinds
// immediately; fixed ports would be far flakier under -count=20.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return addr
}

// TestServeSmoke_BootDashboardAndShutdown boots a REAL `tierd serve` child on a
// loopback port and asserts the boot BEHAVIOR end to end: flags parse, the mux
// composes, the listener serves, the dashboard is actually mounted at "/", an
// unknown path 404s through the real mux, and SIGTERM triggers the graceful
// shutdown leg (exit 0, "tierd stopped").
//
// Honesty note: the child runs in a separate process, so its execution does
// NOT land in the parent's coverage profile -- runServe's reported % may stay 0.
// The deliverable here is pinned boot behavior (flags -> mux -> listen -> serve
// -> graceful stop), not the coverage number.
func TestServeSmoke_BootDashboardAndShutdown(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	addr := freeLoopbackPort(t)
	dbPath := filepath.Join(t.TempDir(), "smoke.db")

	// --aggregation is REQUIRED since #185 (serve refuses to start without it);
	// 'developer' is the boot-neutral choice for a smoke test. A tokenless bind
	// is legal on loopback per validateBind, so no --api-token is needed.
	childArgs := strings.Join([]string{
		"serve",
		"--addr", addr,
		"--db", dbPath,
		"--aggregation", "developer",
	}, "\n")

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	// A single goroutine owns cmd.Wait() (calling it twice is undefined). Its
	// result flows through exited, consumed either by waitUntilLive (fail fast if
	// the child dies during boot) or by the shutdown assertion after SIGTERM.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Safety net: if any assertion below fails early, don't leak the child. Kill
	// is idempotent-ish here -- a SIGKILL to an already-reaped process just errors
	// harmlessly, and the owning goroutine has already Wait()ed.
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
	})

	base := "http://" + addr

	// Poll the open livez probe until the server is up (no fixed sleep -- house
	// rule). 10s deadline / 50ms interval.
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)

	// Y1/Y2: the dashboard is really mounted at "/" in the live composition.
	t.Run("dashboard served at /", func(t *testing.T) {
		res, body := httpGet(t, base+"/")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET / status = %d, want 200", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET / Content-Type = %q, want text/html...", ct)
		}
		if !strings.Contains(body, "<title>TIER") {
			t.Errorf("GET / body missing the <title>TIER dashboard marker")
		}
	})

	t.Run("unknown path 404s through the real mux", func(t *testing.T) {
		res, _ := httpGet(t, base+"/definitely-not-a-route")
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /definitely-not-a-route status = %d, want 404", res.StatusCode)
		}
	})

	// Graceful shutdown: SIGTERM -> exit 0 within 10s, "tierd stopped" logged.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal SIGTERM: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("child exited non-zero on SIGTERM: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("child did not exit within 10s of SIGTERM (stderr: %s)", stderr.String())
	}
	if !strings.Contains(stderr.String(), "tierd stopped") {
		t.Errorf("graceful-shutdown log line missing; stderr:\n%s", stderr.String())
	}
}

// TestServeSmoke_ProxiesMountedInNormalMode is the POSITIVE counterpart to
// TestServeSmoke_ReadOnlyOmitsWrites' 404 assertions: that test proves the
// three proxy routes are OFF the mux under --read-only; nothing anywhere
// proved they are actually ON the mux otherwise. A typo in any of the three
// registerProxy(mux, "/anthropic"|"/openai"|"/gemini", ...) calls (main.go) —
// e.g. "/gemni" — would 404 here exactly like the read-only case and pass
// every other test in the tree, including #459 task 4's own headline
// deliverable (the /gemini/ mount).
//
// Proxy targets point at a DEAD loopback address (127.0.0.1:1, matching the
// read-only test's pattern) so this stays hermetic — no real network call to
// any provider — and --api-token is set so an unauthenticated POST gets a
// clean 401 (mounted + auth-gated) rather than the reverse proxy actually
// attempting to dial the dead upstream (which ProxyAuth would allow through
// unauthenticated, per its own "no token configured -> open" contract, and
// which would then race a connection-refused 502 instead of cleanly proving
// the mount).
func TestServeSmoke_ProxiesMountedInNormalMode(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	addr := freeLoopbackPort(t)
	dbPath := filepath.Join(t.TempDir(), "proxy-mount-smoke.db")

	childArgs := strings.Join([]string{
		"serve",
		"--addr", addr,
		"--db", dbPath,
		"--aggregation", "developer",
		"--api-token", "smoke-test-api-token",
		"--anthropic-target", "http://127.0.0.1:1/",
		"--openai-target", "http://127.0.0.1:1/",
		"--gemini-target", "http://127.0.0.1:1/",
	}, "\n")

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	base := "http://" + addr
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)

	for path, route := range map[string]string{
		"/anthropic/v1/messages":                  "/anthropic/",
		"/openai/v1/chat/completions":             "/openai/",
		"/gemini/v1beta/models/x:generateContent": "/gemini/",
	} {
		t.Run(route, func(t *testing.T) {
			resp, err := http.Post(base+path, "application/json", strings.NewReader("{}")) //nolint:noctx // loopback test POST
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("unauthenticated POST %s = %d, want 401 (mounted + auth-gated, not 404 off-the-mux)", path, resp.StatusCode)
			}
		})
	}
}

// TestServeSmoke_BootsWithExampleConfig boots a REAL `tierd serve` using the
// SHIPPED config.example.yaml verbatim — only --addr is overridden (a free
// loopback port instead of the example's fixed 8080), and the child runs in a
// temp cwd so the example's relative `db: tier.db` lands there. It proves the
// #377 contract end to end: a stranger who copies the example and runs
// `tierd serve --config config.example.yaml` gets a live server — no root-owned
// db path, no @/etc/tier secret read, no outbound poller (collectors are
// commented out, so boot stays offline).
func TestServeSmoke_BootsWithExampleConfig(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	exampleCfg, err := filepath.Abs(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("resolve example config path: %v", err)
	}
	addr := freeLoopbackPort(t)
	// Run in a temp cwd so the example's relative `db: tier.db` is created here
	// (and torn down) instead of polluting the package directory.
	workDir := t.TempDir()

	// Only --addr is overridden. aggregation (team), db (relative tier.db), and
	// the disabled collectors all come from the example file verbatim. team mode
	// boots with a WARN when no org_hierarchy is present — it does not fail.
	childArgs := strings.Join([]string{
		"serve",
		"--config", exampleCfg,
		"--addr", addr,
	}, "\n")

	cmd := exec.Command(self)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	base := "http://" + addr
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)

	// The example config really composes a serving dashboard.
	res, body := httpGet(t, base+"/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200 (stderr: %s)", res.StatusCode, stderr.String())
	}
	if !strings.Contains(body, "<title>TIER") {
		t.Errorf("GET / body missing the <title>TIER dashboard marker")
	}

	// The example's relative db path landed under the temp cwd — proof MkdirAll
	// succeeded on a path a fresh machine can actually write (the /var/lib bug).
	if _, err := os.Stat(filepath.Join(workDir, "tier.db")); err != nil {
		t.Errorf("example db not created at cwd/tier.db: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal SIGTERM: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("child exited non-zero on SIGTERM: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("child did not exit within 10s of SIGTERM (stderr: %s)", stderr.String())
	}
}

// TestServeSmoke_ReadOnlyOmitsWrites boots a REAL `tierd serve --read-only` child
// (#429) and asserts the public-demo posture end to end through the live mux: the
// read surface (dashboard, /scores) serves, but every write/ingest route — the API
// writes AND the GitHub webhook — is structurally absent (404, not a token-gated
// 401), and the read-only banner is logged. This pins the FLAG WIRING (that
// --read-only routes through RegisterReadOnly and skips the webhook/proxies), which
// the internal/api RegisterReadOnly unit test cannot see.
func TestServeSmoke_ReadOnlyOmitsWrites(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	addr := freeLoopbackPort(t)
	dbPath := filepath.Join(t.TempDir(), "ro-smoke.db")
	// Deliberately hand read-only a FULL ingest config: proxy targets (unreachable
	// loopback so a regression can't make a real outbound call) AND a bogus
	// --watch-repo. The choke point must blank all of it — so the proxies never
	// mount (asserted 404 below) and, decisively, boot SUCCEEDS despite the bogus
	// watch path: in normal mode resolveWatchRepos would fail boot on /nonexistent,
	// so a live server here proves read-only ignored the watcher config (#429 Y2).
	childArgs := strings.Join([]string{
		"serve",
		"--addr", addr,
		"--db", dbPath,
		"--aggregation", "developer",
		"--read-only",
		"--anthropic-target", "http://127.0.0.1:1/",
		"--openai-target", "http://127.0.0.1:1/",
		"--gemini-target", "http://127.0.0.1:1/",
		"--watch-repo", filepath.Join(t.TempDir(), "nonexistent-repo"),
		// The Codex collector is an INGEST path, so the demo must neutralize it
		// too (#464). Handed on deliberately: the assertion below is the only
		// guard on that one `codexSettings = nil` line in the choke point.
		"--codex-rollout",
	}, "\n")

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	base := "http://" + addr
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)

	// Read surface is up: the dashboard serves (the demo's whole point).
	if res, _ := httpGet(t, base+"/"); res.StatusCode != http.StatusOK {
		t.Errorf("read-only GET / = %d, want 200 (dashboard must serve)", res.StatusCode)
	}

	// Docs stay reachable in read-only mode (#449): the /docs/ mount is UNCONDITIONAL
	// (outside the `if *readOnly` write choke point), and the public demo is exactly
	// where the docs must be reachable. This pins that a future refactor moving the
	// mount inside the read-only branch — which would still pass every other gate —
	// cannot silently break docs on the demo. The docs index (README.html) must serve,
	// a known page must serve, and an unknown page must 404 (structurally routed, not
	// a catch-all 200).
	if res, _ := httpGet(t, base+"/docs/"); res.StatusCode != http.StatusOK {
		t.Errorf("read-only GET /docs/ = %d, want 200 (docs index must serve in the demo)", res.StatusCode)
	}
	if res, _ := httpGet(t, base+"/docs/README.html"); res.StatusCode != http.StatusOK {
		t.Errorf("read-only GET /docs/README.html = %d, want 200 (known docs page must serve)", res.StatusCode)
	}
	if res, _ := httpGet(t, base+"/docs/nope.html"); res.StatusCode != http.StatusNotFound {
		t.Errorf("read-only GET /docs/nope.html = %d, want 404 (unknown docs page)", res.StatusCode)
	}

	// Write/ingest/relay routes are structurally absent: a tokenless POST must 404,
	// NOT the 401 a mounted-but-auth-gated route (or a mounted proxy) would give —
	// proof the route is off the mux, not merely token-gated. The proxy paths are
	// the Y2 regression-catcher: if the choke point stopped blanking the proxy
	// targets, /anthropic, /openai, and /gemini would mount and answer 401 (proxy
	// auth), not 404 — and would never reach the unreachable upstream.
	for _, w := range []string{
		"/api/v1/events", "/api/v1/costs", "/webhook/github",
		"/anthropic/v1/messages", "/openai/v1/chat/completions", "/gemini/v1beta/models",
	} {
		resp, err := http.Post(base+w, "application/json", strings.NewReader("{}")) //nolint:noctx // loopback test POST
		if err != nil {
			t.Fatalf("POST %s: %v", w, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("read-only POST %s = %d, want 404 (write/ingest route must be structurally absent)", w, resp.StatusCode)
		}
	}

	// The read-only banner is logged so an operator can confirm the posture at boot.
	if !strings.Contains(stderr.String(), "READ-ONLY mode ENABLED") {
		t.Errorf("read-only boot banner missing; stderr:\n%s", stderr.String())
	}

	// #464 Y10: the Codex rollout collector is an INGEST path and the demo runs on
	// a machine with real ~/.codex logs, so `codexSettings = nil` in the read-only
	// choke point is a write-disablement GUARANTEE — and it was guarded by nothing
	// but that line. The child was started WITH --codex-rollout, so an "enabled"
	// line here means the demo would be scanning and ingesting local Codex spend.
	// Matched on the exact JSON msg field: the enabled-but-no-repo WARN begins
	// with the same words, so a bare substring would match the wrong line.
	if strings.Contains(stderr.String(), `"msg":"Codex rollout collector enabled"`) {
		t.Errorf("read-only mode started the Codex rollout collector despite --codex-rollout being neutralized; stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Codex rollout collector disabled") {
		t.Errorf("read-only boot did not log the Codex collector as disabled (did the switch change shape?); stderr:\n%s", stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal SIGTERM: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("child exited non-zero on SIGTERM: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("child did not exit within 10s of SIGTERM (stderr: %s)", stderr.String())
	}
}

// TestServeSmoke_CodexRolloutWithoutWatchRepoRefusesToStart pins the OTHER
// untested branch of the Codex wiring (#464 Y10): enabled, but with no
// --watch-repo to attribute to.
//
// BEHAVIOUR REVERSED, deliberately, by operator ruling (2026-07-23). This test
// previously asserted warn-and-skip with the server LIVE, on the reasoning that
// an optional collector should never take down the whole server. That reasoning
// was overturned in favour of failing fast, and the argument is worth keeping
// because it is not obvious:
//
//   - --codex-rollout is OFF by default and must be typed explicitly. An explicit
//     request that CANNOT work is a misconfiguration, not a degraded mode.
//   - Warn-and-skip leaves the operator believing Codex spend is captured when it
//     is not. A WARN in a startup log is not observable enough to correct that
//     belief — the resulting silence is indistinguishable from working capture,
//     which is the exact failure class this codebase keeps paying for.
//   - Under-counted spend is not a neutral loss: outcomes still arrive via webhook
//     and backfill, so uncaptured Codex work inflates TIER rather than lowering it.
//     A flattering number is worse than no number.
//   - The remedy is one flag and the message names it, so the blast radius of
//     failing closed is seconds of operator time.
//
// What the old test protected against is still real — an OBSCURE death from the
// collector constructor. So the pre-construction check stays; what changed is
// that it now exits with a precise, actionable message instead of continuing.
// This test therefore asserts the child exits NON-ZERO with that exact guidance,
// never that it merely fails somehow.
func TestServeSmoke_CodexRolloutWithoutWatchRepoRefusesToStart(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	addr := freeLoopbackPort(t)
	childArgs := strings.Join([]string{
		"serve",
		"--addr", addr,
		"--db", filepath.Join(t.TempDir(), "codex-smoke.db"),
		"--aggregation", "developer",
		"--codex-rollout",
		// deliberately NO --watch-repo
	}, "\n")

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	select {
	case err := <-exited:
		if err == nil {
			t.Fatalf("child exited ZERO; an explicit --codex-rollout that cannot capture anything must fail closed, not start and silently drop Codex spend. stderr:\n%s", stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		t.Fatalf("child still running after 10s; it must refuse to start. stderr:\n%s", stderr.String())
	}

	// Assert the ACTIONABLE message, not merely a non-zero exit. Failing closed
	// is only an improvement over warn-and-skip if the operator is told exactly
	// which flag to add or drop; a bare non-zero exit would be strictly worse
	// than the warning it replaced.
	logs := stderr.String()
	for _, want := range []string{
		"--codex-rollout needs a repository",
		"--watch-repo",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("stderr missing %q — the refusal must name the remedy; stderr:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `"msg":"Codex rollout collector enabled"`) {
		t.Errorf("collector reported enabled with no repo to attribute to; stderr:\n%s", logs)
	}
}

// waitUntilLive polls url until it returns 200, failing fast if the child exits
// during boot (exited fires) and failing on the 10s deadline otherwise.
func waitUntilLive(t *testing.T, url string, stderr *lockedBuffer, exited <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// If the child has already exited, stop waiting and report why.
		select {
		case err := <-exited:
			t.Fatalf("child exited before becoming live (%v); stderr:\n%s", err, stderr.String())
		default:
		}
		resp, err := http.Get(url) //nolint:noctx // short-lived loopback probe in a test
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server not live within 10s at %s; stderr:\n%s", url, stderr.String())
}

// httpGet performs a GET and returns the response plus its fully-read body.
func httpGet(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // short-lived loopback GET in a test
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp, string(body)
}
