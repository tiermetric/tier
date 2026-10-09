//go:build integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// writeSubscriptionServeConfig writes a serve config carrying ONE subscription
// whose route_prefix matches nothing in the embedded price table (which seeds no
// subscription rows at all). That is deliberately the configuration `tierd serve`
// must REFUSE — so any boot that succeeds below has to have skipped the gate for
// a reason the test names.
func writeSubscriptionServeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier.yaml")
	body := "subscriptions:\n" +
		"  - route_prefix: \"glm-5.2@ollama.com\"\n" +
		"    plan: \"max\"\n" +
		"    org: \"acme\"\n" +
		"    monthly_fee_usd: 100.00\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeCoveredSubscriptionServeConfig writes a config whose subscription IS
// covered — it ships its own price table carrying the matching
// `billing_mode: subscription` row — so `serve` boots and the reconciler runs
// for real. Returns the config path.
func writeCoveredSubscriptionServeConfig(t *testing.T, dir string) string {
	t.Helper()
	prices := filepath.Join(dir, "prices.yaml")
	pricesBody := "version: 1\neffective_date: \"2026-08-01\"\nmodels:\n" +
		"  \"glm-5.2@ollama.com\": { input_per_m: 0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }\n" +
		"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
	if err := os.WriteFile(prices, []byte(pricesBody), 0o600); err != nil {
		t.Fatalf("write price table: %v", err)
	}
	path := filepath.Join(dir, "tier.yaml")
	body := "prices_file: \"" + prices + "\"\n" +
		"subscriptions:\n" +
		"  - route_prefix: \"glm-5.2@ollama.com\"\n" +
		"    plan: \"max\"\n" +
		"    org: \"acme\"\n" +
		"    monthly_fee_usd: 100.00\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestServeSmoke_SubscriptionFeeReconcilerPostsThroughTheRealBinary is the
// POSITIVE arm the read-only test needs to mean anything. That test proves the
// reconciler is OFF under --read-only by reading a log line; on its own, a
// reconciler that never ran at all under any configuration would satisfy it.
//
// Here the whole wire runs in a child process — config → coverage gate →
// store.Open → goroutine → a real org_actual_spend row — and the assertion is
// the ROW, read back out of SQLite, not a log line.
func TestServeSmoke_SubscriptionFeeReconcilerPostsThroughTheRealBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub-live.db")
	addr := freeLoopbackPort(t)
	childArgs := strings.Join([]string{
		"serve",
		"--addr", addr,
		"--db", dbPath,
		"--aggregation", "developer",
		"--config", writeCoveredSubscriptionServeConfig(t, dir),
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

	waitUntilLive(t, "http://"+addr+"/api/v1/livez", stderr, exited)

	// Poll for the row: the startup pass races /livez by microseconds, and a
	// fixed sleep would be either flaky or slow.
	var org, period, source string
	var micro int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		err = db.QueryRow(`SELECT org, period, actual_paid_micro, source FROM org_actual_spend`).
			Scan(&org, &period, &micro, &source)
		_ = db.Close()
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("query org_actual_spend: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if org == "" {
		t.Fatalf("no org_actual_spend row appeared within 10s — the reconciler did not post through the real binary.\nstderr:\n%s", stderr.String())
	}
	if org != "acme" || micro != 100_000_000 || source != "subscription:glm-5.2@ollama.com" {
		t.Errorf("posted row = (org=%s, period=%s, micro=%d, source=%s), want (acme, <current period>, 100000000, subscription:glm-5.2@ollama.com)",
			org, period, micro, source)
	}
	if !strings.Contains(stderr.String(), "subscription fee reconciler enabled") {
		t.Errorf("the enabled log line never appeared; got:\n%s", stderr.String())
	}
}

// TestServeSmoke_SubscriptionCoverageGateRefusesBoot drives the REAL binary
// through the two-artifacts-one-truth gate (#113). A fee configured for a route
// the active price table does not price as a subscription must stop startup —
// not warn, not proceed — because it would add real dollars to actual-paid with
// no matching list value and deflate a CFO-facing number.
//
// A child process is the only honest way to assert this: the gate calls
// os.Exit(1), which cannot be observed in-process.
func TestServeSmoke_SubscriptionCoverageGateRefusesBoot(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	childArgs := strings.Join([]string{
		"serve",
		"--addr", freeLoopbackPort(t),
		"--db", filepath.Join(t.TempDir(), "sub-gate.db"),
		"--aggregation", "developer",
		"--config", writeSubscriptionServeConfig(t),
	}, "\n")

	// 🔴 CommandContext, not Command. `cmd.Run()` blocks until the child exits —
	// and if this gate is ever deleted or moved below store.Open, the child boots
	// a live HTTP server and NEVER exits. The test would then HANG to the package
	// timeout, taking every other cmd/tierd result with it, instead of reporting
	// the deleted guard. A guard whose removal hangs is not a guard that fails.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self)
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr

	err = cmd.Run()
	// Check the deadline BEFORE the error shape: a context kill also surfaces as
	// an *exec.ExitError (code -1), which would otherwise be reported as
	// "exit code = -1, want 1" — a failure for a misleading reason.
	if ctx.Err() != nil {
		t.Fatalf("tierd serve did not exit within 30s — it BOOTED with an uncovered subscription route, so the coverage gate did not fire.\nstderr:\n%s", stderr.String())
	}
	if err == nil {
		t.Fatalf("tierd serve started with an uncovered subscription route; want a non-zero exit.\nstderr:\n%s", stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("tierd serve failed for the wrong reason: %v\nstderr:\n%s", err, stderr.String())
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "billing_mode: subscription") {
		t.Errorf("startup error does not tell the operator how to fix it; stderr:\n%s", stderr.String())
	}
}

// TestServeSmoke_ReadOnlyNeutralizesSubscriptionFees is BOTH arms of the
// read-only interaction, and it exists because the ordering is the kind of thing
// that is right by accident until someone moves a block.
//
//  1. The subscription-fee reconciler WRITES org_actual_spend rows, so
//     `--read-only` (#429) must neutralize it like every other background
//     writer. The assertion is the log line, since the reconciler leaves no
//     other trace when it does not run.
//  2. Neutralization must happen BEFORE the coverage gate. The very config that
//     makes the test above refuse to boot must here boot CLEANLY — which can
//     only happen if the read-only choke point cleared the block before the gate
//     read it. Move the gate above the choke point and this test fails while the
//     one above still passes.
func TestServeSmoke_ReadOnlyNeutralizesSubscriptionFees(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	addr := freeLoopbackPort(t)
	childArgs := strings.Join([]string{
		"serve",
		"--addr", addr,
		"--db", filepath.Join(t.TempDir(), "sub-ro.db"),
		"--aggregation", "developer",
		"--read-only",
		"--config", writeSubscriptionServeConfig(t),
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

	// Arm 2: it BOOTED. With the same config, the non-read-only run above exits 1.
	waitUntilLive(t, "http://"+addr+"/api/v1/livez", stderr, exited)

	// Arm 1: and the reconciler is off. Poll rather than read once — the startup
	// log lines race the /livez readiness by microseconds.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stderr.String(), "subscription fee reconciler disabled") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(stderr.String(), "subscription fee reconciler enabled") {
		t.Fatalf("read-only mode STARTED the subscription fee reconciler — a background writer in the public demo.\nstderr:\n%s", stderr.String())
	}
	t.Fatalf("read-only serve logged neither the enabled nor the disabled reconciler line; the assertion is not observing what it claims.\nstderr:\n%s", stderr.String())
}
