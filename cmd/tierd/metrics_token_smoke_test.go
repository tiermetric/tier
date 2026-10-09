//go:build integration

package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	smokeAPITok     = "smoke-write-token-944-aaaaaaaaaa"
	smokeReadTok    = "smoke-read-token-944-bbbbbbbbbbb"
	smokeMetricsTok = "smoke-metrics-token-944-ccccccccc"
)

// bootTeamServe starts a real team-mode `tierd serve` child with the write and
// read tokens on the command line, extra args appended, and env layered on an
// environment scrubbed of every ambient TIER_* variable. It returns the base URL
// and the child's stderr once /livez answers.
func bootTeamServe(t *testing.T, extraArgs, env []string) (string, *lockedBuffer) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	addr := freeLoopbackPort(t)
	args := append([]string{
		"serve",
		"--addr", addr,
		"--db", filepath.Join(t.TempDir(), "metrics-token.db"),
		"--aggregation", "team",
		"--api-token", smokeAPITok,
		"--read-token", smokeReadTok,
	}, extraArgs...)
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
	cmd.Env = append(cmd.Env, env...)
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
	return base, stderr
}

// scrapeMetrics GETs /metrics with a Bearer token and returns status and body.
func scrapeMetrics(t *testing.T, base, tok string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/metrics", nil) //nolint:noctx // loopback test GET
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// writeMetricsSmokeFile writes content to a 0600 file in a temp dir and returns its path.
func writeMetricsSmokeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestServeSmoke_MetricsTokenTeamMode boots a REAL team-mode `tierd serve` with
// the metrics token supplied as TIER_METRICS_TOKEN=@file (#944) and scrapes
// /metrics through the live composition: the metrics token (read from the file)
// and the write token get 200, the read token gets 403 naming --metrics-token.
// The file holds the token plus a trailing newline, which @file trims, so a 200
// proves the @file form resolved rather than the literal "@/path" string.
func TestServeSmoke_MetricsTokenTeamMode(t *testing.T) {
	tokFile := writeMetricsSmokeFile(t, "metrics-token", smokeMetricsTok+"\n")
	base, stderr := bootTeamServe(t, nil, []string{"TIER_METRICS_TOKEN=@" + tokFile})

	if code, body := scrapeMetrics(t, base, smokeMetricsTok); code != http.StatusOK || !strings.Contains(body, "tier_build_info") {
		t.Errorf("metrics token (from @file) GET /metrics = %d, want 200 with the exposition; body = %.200s", code, body)
	}
	if code, _ := scrapeMetrics(t, base, smokeAPITok); code != http.StatusOK {
		t.Errorf("write token GET /metrics = %d, want 200", code)
	}
	if code, body := scrapeMetrics(t, base, smokeReadTok); code != http.StatusForbidden || !strings.Contains(body, "--metrics-token") {
		t.Errorf("read token GET /metrics = %d, want 403 naming --metrics-token; body = %s", code, body)
	}
	if !strings.Contains(stderr.String(), `"metrics_armed":true`) {
		t.Errorf("startup log does not report metrics_armed=true; stderr:\n%s", stderr.String())
	}
}

// TestServeSmoke_MetricsTokenFromConfig pins the http.metrics_token config key
// (#944): alone it arms the metrics scope, and TIER_METRICS_TOKEN overrides it,
// leaving the config value unaccepted.
func TestServeSmoke_MetricsTokenFromConfig(t *testing.T) {
	const cfgTok = "config-metrics-token-944-eeeeeeee"
	cfgPath := writeMetricsSmokeFile(t, "tier.yaml", "http:\n  metrics_token: \""+cfgTok+"\"\n")

	t.Run("config alone", func(t *testing.T) {
		base, _ := bootTeamServe(t, []string{"--config", cfgPath}, nil)
		if code, body := scrapeMetrics(t, base, cfgTok); code != http.StatusOK {
			t.Errorf("config metrics_token GET /metrics = %d, want 200; body = %s", code, body)
		}
	})
	t.Run("env overrides config", func(t *testing.T) {
		base, _ := bootTeamServe(t, []string{"--config", cfgPath}, []string{"TIER_METRICS_TOKEN=" + smokeMetricsTok})
		if code, body := scrapeMetrics(t, base, smokeMetricsTok); code != http.StatusOK {
			t.Errorf("env metrics token GET /metrics = %d, want 200; body = %s", code, body)
		}
		if code, _ := scrapeMetrics(t, base, cfgTok); code != http.StatusUnauthorized {
			t.Errorf("overridden config metrics_token GET /metrics = %d, want 401", code)
		}
	})
}

// TestServeSmoke_EqualTokensRefused pins that serve refuses to start when the
// metrics token equals the read or the write token (#944), including when the
// metrics token comes from an @file: the comparison runs on resolved secrets.
func TestServeSmoke_EqualTokensRefused(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	apiFile := writeMetricsSmokeFile(t, "metrics-equals-api", smokeAPITok+"\n")
	for _, tc := range []struct {
		name      string
		api, read string
		metrics   []string // extra args
		env       []string
		want      string
	}{
		{"metrics == read", smokeAPITok, "same-token-944-dddddddddddddddd",
			[]string{"--metrics-token", "same-token-944-dddddddddddddddd"}, nil,
			"--metrics-token must differ from --read-token"},
		{"metrics == api", "same-token-944-dddddddddddddddd", smokeReadTok,
			[]string{"--metrics-token", "same-token-944-dddddddddddddddd"}, nil,
			"--metrics-token must differ from --api-token"},
		{"metrics @file == api", smokeAPITok, smokeReadTok,
			nil, []string{"TIER_METRICS_TOKEN=@" + apiFile},
			"--metrics-token must differ from --api-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"serve",
				"--addr", freeLoopbackPort(t),
				"--db", filepath.Join(t.TempDir(), "equal.db"),
				"--aggregation", "team",
				"--api-token", tc.api,
				"--read-token", tc.read,
			}, tc.metrics...)
			cmd := exec.Command(self)
			cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
			cmd.Env = append(cmd.Env, tc.env...)
			stderr := &lockedBuffer{}
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start tierd child: %v", err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })
			var err error
			select {
			case err = <-exited:
			case <-time.After(15 * time.Second):
				t.Fatalf("serve with %s did not refuse to start within 15s; stderr:\n%s", tc.name, stderr.String())
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("serve with %s: err = %v, want exit 1; stderr:\n%s", tc.name, err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr does not name the refusal %q:\n%s", tc.want, stderr.String())
			}
		})
	}
}
