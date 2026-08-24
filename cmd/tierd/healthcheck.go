package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Defaults for the container probe. The address is LOOPBACK on purpose: the
// probe runs inside the container's own network namespace, where a server that
// bound 0.0.0.0 is reachable on 127.0.0.1, and a server that bound only
// loopback is reachable too. Probing the published address instead would leave
// the check passing when the container is fine but the publish is broken —
// which is the host's problem, not this container's health.
//
// defaultListenAddr is shared with `serve` and `demo` rather than repeated: the
// probe's default is only correct while it matches the server's default, and
// three independent copies of the same string is a coupling nothing enforces.
// TestHealthcheck_DefaultAddrTracksServe pins the relationship.
const (
	defaultHealthcheckPath    = "/api/v1/livez"
	defaultHealthcheckTimeout = 2 * time.Second
	// healthcheckAddrEnv retargets the probe at runtime. Sharing
	// defaultListenAddr fixes LITERAL drift between the probe and the server's
	// default, but not RUNTIME drift: `serve --addr 0.0.0.0:9090`, or an addr
	// set from the config file, leaves the probe on 8080 forever and the
	// container permanently unhealthy while it serves correctly. An exec-form
	// HEALTHCHECK does no shell expansion, so `-e` is the only way an operator
	// can retarget it without replacing the whole instruction.
	healthcheckAddrEnv = "TIER_HEALTHCHECK_ADDR"
)

// defaultHealthcheckAddr resolves the probe target: the env override when set,
// otherwise the same address serve/demo bind by default.
func defaultHealthcheckAddr() string {
	if v := strings.TrimSpace(os.Getenv(healthcheckAddrEnv)); v != "" {
		return v
	}
	return defaultListenAddr
}

// maxHealthcheckBody caps what we read back for the diagnostic line. Docker
// stores healthcheck output in the container's inspect state, so this is
// bounded rather than unbounded — a probe must not be able to bloat that.
const maxHealthcheckBody = 256

// runHealthcheck probes a running tierd over HTTP and exits 0 only if it
// answers with a 2xx AND the response completes. It exists because the runtime
// image is Chainguard Wolfi static — no shell, no wget, no curl — so a
// Dockerfile HEALTHCHECK has nothing to call except tierd itself (#571). The
// binary is already in the image, so this adds no layer and no attack surface.
//
// It asserts LIVENESS ONLY: the port is bound and the server answers. It
// deliberately does NOT assert data quality, attribution, or capture state —
// that is `doctor`'s job, and `doctor` wants a git repo, a ~/.claude dir and an
// attribution floor, so it would report unhealthy for reasons unrelated to
// serving traffic. A healthcheck that fails for the wrong reason is worse than
// none: it trains the reader to ignore it.
//
// The default path is /api/v1/livez, which stays open for probes (no auth, no
// spend data — see api.Handler.Register) and reports the build version, so a
// passing probe also identifies WHICH binary answered. That matters: a stale
// process holding the port has answered 200 for a fresh one before.
func runHealthcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultHealthcheckAddr(), "host:port to probe (inside this container's network namespace; env "+healthcheckAddrEnv+")")
	path := fs.String("path", defaultHealthcheckPath, "URL path to probe")
	timeout := fs.Duration("timeout", defaultHealthcheckTimeout, "total time budget for the probe")
	if err := fs.Parse(args); err != nil {
		// An explicit --help is success; ANY other parse error is a failure. A
		// mistyped flag in the Dockerfile HEALTHCHECK must never exit 0 —
		// that would report `healthy` forever for a container serving nothing.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	// A stray positional is the same class of mistake as a mistyped flag, and
	// worse in effect: `healthcheck --addr X /api/v1/healthz` would silently
	// probe the DEFAULT path and exit 0, reporting on something the operator
	// did not ask for. Fail rather than guess.
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "healthcheck: unexpected argument %q (flags only; did you mean --path?)\n", fs.Arg(0))
		return 1
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(stderr, "healthcheck: --timeout must be positive, got %s\n", *timeout)
		return 1
	}

	probeURL, err := healthcheckURL(*addr, *path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}

	// The timeout is on the CONTEXT, not just the dial, so it bounds the whole
	// exchange: connect, write, response headers, and the body read below. A
	// port that is bound but never answers must FAIL — otherwise the probe
	// degrades to a TCP connect test, which cannot tell a serving process from
	// a wedged one holding the listener.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: bad request for %s: %v\n", probeURL, err)
		return 1
	}
	// Identify the probe in access logs so it is distinguishable from real
	// traffic (and from a human curling the same path).
	req.Header.Set("User-Agent", "tierd-healthcheck/"+resolveVersion())

	client := &http.Client{
		// An explicit Transport, NOT the process-global http.DefaultTransport
		// a nil field would inherit. Proxy is nil so an ambient HTTP_PROXY can
		// never point the probe at something other than the address the
		// operator named — the same failure CheckRedirect guards, arriving
		// through another door. Keep-alives are off because this process makes
		// exactly one request and exits; there is nothing left to reuse a
		// pooled connection, and a one-shot command should not deposit state
		// in a global.
		//
		// No per-transport dial/TLS timeouts on purpose: the context is the
		// SINGLE budget. A second timeout would be another thing to keep in
		// sync and another reason the probe could fail with a message other
		// than the one it documents.
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %s unreachable: %v\n", probeURL, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	// Read a bounded prefix of the body. For the default /livez path this
	// carries {"status":"alive","uptime_s":…,"version":"…"} — printing it means
	// `docker inspect` shows which build answered, not merely that something
	// did.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxHealthcheckBody))
	snippet := sanitizeHealthcheckSnippet(body)
	// Mark truncation rather than printing a body cut mid-token as if it were
	// the whole thing. /livez is ~105 bytes since #638 added `commit` (it was ~70,
	// and that number was left stale by the commit that changed it) so this never fires on the default
	// path; it matters for a --path override onto a chattier endpoint.
	if len(body) == maxHealthcheckBody {
		snippet += "…(truncated)"
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			_, _ = fmt.Fprintf(stderr, "healthcheck: %s returned %d (body unread: %v)\n", probeURL, resp.StatusCode, readErr)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "healthcheck: %s returned %d: %s\n", probeURL, resp.StatusCode, snippet)
		return 1
	}
	// A 2xx whose response never completed is NOT healthy. io.LimitReader turns
	// truncation into a clean EOF (io.ReadAll returns nil for it), so a non-nil
	// error here can only mean the exchange itself broke — deadline blown or
	// connection dropped mid-response. That is the wedged-but-listening server
	// this probe exists to catch, one layer up from a dead port: a handler
	// blocked on the single-writer SQLite mutex still emits its status line.
	// Treating the status line alone as healthy would let `docker ps` report
	// healthy forever while no response ever completes.
	if readErr != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %s returned %d but the response never completed: %v\n", probeURL, resp.StatusCode, readErr)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "healthcheck: %s %d %s\n", probeURL, resp.StatusCode, snippet)
	return 0
}

// sanitizeHealthcheckSnippet trims the body prefix and drops non-printable
// bytes. The output lands verbatim in `docker inspect`'s health log, so an
// arbitrary body must not be able to inject newlines or control sequences into
// an operator's terminal.
func sanitizeHealthcheckSnippet(body []byte) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || (r >= 0x20 && r != 0x7f) {
			return r
		}
		return -1
	}, strings.TrimSpace(string(body)))
}

// healthcheckURL builds the probe URL from --addr and --path, and REJECTS an
// address that would send the probe somewhere other than the host named.
//
// --addr is a host:port, not a URL: tierd's own server speaks plain HTTP (TLS
// termination, where it exists, is upstream), and the probe runs inside the
// container against a loopback listener. Accepting a full http:// URL anyway is
// a few lines and spares an operator a confusing failure when they type the
// thing they expect to work. The scheme test is case-insensitive so HTTP:// is
// not silently prefixed into http://HTTP://…
//
// The userinfo check is the third door on the same failure the redirect policy
// and the nil Proxy already close: "127.0.0.1:1@evil:80" parses as user
// "127.0.0.1:1" at host "evil:80", so raw concatenation would probe a
// different machine and exit 0. tierd's probe never authenticates — /livez is
// open — so userinfo can only ever be this mistake.
func healthcheckURL(addr, path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	raw := "http://" + addr + path
	lower := strings.ToLower(addr)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		raw = strings.TrimSuffix(addr, "/") + path
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad --addr %q: %w", addr, err)
	}
	if u.User != nil {
		return "", fmt.Errorf("bad --addr %q: contains credentials before '@', which would probe host %q instead", addr, u.Host)
	}
	if u.Host == "" {
		return "", fmt.Errorf("bad --addr %q: no host:port", addr)
	}
	return raw, nil
}
