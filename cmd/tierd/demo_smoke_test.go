//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// This file is the end-to-end control for the SECURITY INVARIANT stated on
// runDemo in demo.go (#508): "a `tierd demo` boot can never let a byte of
// pre-existing --db content reach an HTTP client." It drives the REAL `tierd
// demo` subcommand as a subprocess (the TestMain self-re-exec harness set up in
// serve_smoke_test.go) rather than calling guardDemoDBPath/seedDemo directly, so
// it exercises the actual wiring: flag parsing -> guard -> delete -> reseed ->
// serve. demo_test.go's unit tests already pin guardDemoDBPath's own
// refuse/allow DECISION in isolation, by calling it directly; what those tests
// cannot see is whether runDemo actually calls it (a deleted or short-circuited
// call site) or actually honors its "delete before serving" half. The tests
// below are the control arms for those two gaps — see the SECURITY INVARIANT
// comment on runDemo in demo.go for which is which.
//
// Gap 2 has TWO arms, because the delete can fail in two different ways and a
// review found the second one live: the delete can be SKIPPED (caught by
// TestDemoSmoke_AlwaysReseedsCanonicalData) or it can be ATTEMPTED AND FAIL
// while the code ignores the error (caught by
// TestDemoSmoke_RefusesWhenTheStaleDBCannotBeDeleted). The second shipped, and
// served a $999 row to an HTTP client. Do not collapse these into one arm.

// canonicalDemoDevelopers returns the current demo-cast developer names,
// derived from demoData() rather than hardcoded, so this file cannot drift from
// the guard's own identity set (demo.go:isRecreatableDemoDB derives the same
// way, for the same reason: the cast is free to change without either place
// needing an edit).
func canonicalDemoDevelopers() []string {
	devs := demoData()
	names := make([]string, 0, len(devs))
	for _, d := range devs {
		names = append(names, d.name)
	}
	return names
}

// watchForResponse polls url in a loop until either it gets ANY response (sets
// *served and returns) or stop is closed. It is started BEFORE the child
// process, so it observes the child's entire lifetime -- unlike a single
// post-exit dial, which can only ever observe "nothing is listening now" (the
// process is already dead by construction) and therefore has no power to catch
// a "bind, serve briefly, then exit non-zero" regression. Call wg.Wait() after
// closing stop to guarantee *served has its final value before reading it.
func watchForResponse(wg *sync.WaitGroup, url string, stop <-chan struct{}, servedMu *sync.Mutex, served *bool) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		client := &http.Client{Timeout: 150 * time.Millisecond}
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := client.Get(url) //nolint:noctx // short-lived loopback poll in a test
			if err == nil {
				_ = resp.Body.Close()
				servedMu.Lock()
				*served = true
				servedMu.Unlock()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// TestDemoSmoke_RefusesRealDataAndNeverBinds is the end-to-end control arm for
// guard (1) in the SECURITY INVARIANT comment on runDemo: pointed at a database
// holding one real (non-demo-cast) developer, `tierd demo` must exit 1 with the
// guard's real-data refusal message, WITHOUT ever answering an HTTP request on
// --addr, and the file must survive byte-for-byte untouched. This is the
// "verify the thing, not a proxy" version of TestGuardDemoDBPath_RefusesRealDB:
// that test proves guardDemoDBPath itself refuses; this test proves runDemo
// actually calls it and stops there -- a regression that deleted or
// short-circuited the call site would not be caught by the unit test alone,
// since that test invokes guardDemoDBPath directly, bypassing runDemo entirely.
func TestDemoSmoke_RefusesRealDataAndNeverBinds(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "real.db")

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A real developer NOT in the demo cast, under the demo org so a guard that
	// only checked the org would wave it through (mirrors demo_test.go's unit
	// control arm).
	if err := db.UpsertHierarchy(context.Background(), "realdev", "Payments", "Product", demoOrg); err != nil {
		t.Fatalf("seed real developer: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}

	addr := freeLoopbackPort(t)

	// Started BEFORE the child: watches the child's entire lifetime for any
	// response at all, not just "is something listening after the child is
	// already dead" (see watchForResponse's doc comment for why that
	// distinction has real mutation power -- a bind-then-refuse regression would
	// pass a post-exit-only check).
	var (
		wg       sync.WaitGroup
		servedMu sync.Mutex
		served   bool
	)
	stopWatcher := make(chan struct{})
	watchForResponse(&wg, "http://"+addr+"/api/v1/livez", stopWatcher, &servedMu, &served)

	childArgs := strings.Join([]string{"demo", "--addr", addr, "--db", dbPath}, "\n")
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		close(stopWatcher)
		t.Fatalf("start tierd demo child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	// The child must exit on its own with exit code 1 -- specifically the
	// guard's clean refusal, not e.g. a panic (which would also be non-zero,
	// exit 2, and must not be mistaken for "the guard worked").
	var exitErr error
	select {
	case exitErr = <-exited:
	case <-time.After(10 * time.Second):
		close(stopWatcher)
		wg.Wait()
		t.Fatalf("demo child did not exit within 10s (served=%v) — it may have bound and started serving real data; stderr:\n%s", served, stderr.String())
	}
	close(stopWatcher)
	wg.Wait()

	var ee *exec.ExitError
	if !errors.As(exitErr, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want a clean exit 1 from the guard, got %v; stderr:\n%s", exitErr, stderr.String())
	}
	// Match the SPECIFIC real-data refusal text, not merely "refusing" --
	// guardDemoDBPath also logs "refusing" on its cannot-confirm/read-error
	// branch (demo.go), and this test must exercise the real-data branch, not
	// accidentally pass because the fixture tripped a different one.
	if !strings.Contains(stderr.String(), "it contains real (non-demo) data") {
		t.Errorf("stderr missing the real-data refusal message; stderr:\n%s", stderr.String())
	}

	servedMu.Lock()
	gotServed := served
	servedMu.Unlock()
	if gotServed {
		t.Errorf("demo answered an HTTP request on %s before/instead of refusing — real data could have been served", addr)
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read file after refused demo run: %v", err)
	}
	if string(before) != string(after) {
		t.Error("real-data DB file was modified despite the guard refusing — data loss (#475)")
	}
}

// seedStaleGuardApprovedDemoDB writes a demo DB that PASSES guardDemoDBPath but
// is NOT canonical: an extra cost row under an issue ID (and dollar amount) no
// seedDemo() output ever produces.
//
// Both properties are load-bearing. Guard-approved, or the test silently
// exercises the #475 refuse path instead of the path it targets. Non-canonical
// with a stale idempotency key, or InsertTokenEvents dedups it against
// seedDemo's own rows and there is nothing left to detect.
func seedStaleGuardApprovedDemoDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	staleArtifact := demoEvent("demo-ada", "DEMO-STALE-ARTIFACT", "claude-sonnet-5", 999.00, "demo-ada-stale-artifact")
	staleArtifact.Timestamp = time.Now().AddDate(0, 0, -3)
	if err := db.InsertTokenEvents(context.Background(), []store.TokenEvent{staleArtifact}); err != nil {
		t.Fatalf("seed stale artifact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestDemoSmoke_AlwaysReseedsCanonicalData is the end-to-end control arm for
// guard (2) in the SECURITY INVARIANT comment on runDemo -- specifically the
// os.Remove calls, not seedDemo (seedDemo alone is additive/idempotent and
// would happily MERGE the canonical cast into stale content rather than
// replacing it; only os.Remove makes replacement rather than merge the actual
// behavior). It seeds a GUARD-APPROVED fixture containing an artifact seedDemo
// can never produce or overwrite -- an extra token_events row for demo-ada
// under a $999 issue ID no canonical seed uses -- and asserts BOTH that the
// live board, after a real `tierd demo` boot, shows the full canonical
// four-developer cast, AND that demo-ada's total cost is exactly the canonical
// $25.70, not the $999-inflated figure the stale row would produce if it
// survived. The second assertion is the one that actually detects "os.Remove
// was skipped": presence-of-four-names alone does not, because seedDemo's
// idempotent upserts/inserts make the canonical cast appear either way -- only
// checking for the STALE row's absence tells the two paths apart. This is the
// exact "ordinary refactor with nothing failing" #508's re-scope warns about:
// an "optimization" that skips the delete because the guard already approved
// the file.
func TestDemoSmoke_AlwaysReseedsCanonicalData(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "stale-demo.db")
	seedStaleGuardApprovedDemoDB(t, dbPath)
	// Confirm the fixture is actually guard-approved -- otherwise this test would
	// silently exercise the #475 refuse path (already covered by the sibling
	// test above), not the reseed path this test targets.
	if err := guardDemoDBPath(dbPath, io.Discard); err != nil {
		t.Fatalf("test fixture invalid: stale file must pass the guard to test the reseed path, got: %v", err)
	}

	addr := freeLoopbackPort(t)
	childArgs := strings.Join([]string{"demo", "--addr", addr, "--db", dbPath}, "\n")
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd demo child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	base := "http://" + addr
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)

	res, body := httpGet(t, base+"/api/v1/scores")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/scores status = %d, want 200 (stderr: %s)", res.StatusCode, stderr.String())
	}
	var out struct {
		Developers []struct {
			Developer    string  `json:"developer"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"developers"`
	}
	if len(body) == 0 {
		t.Fatal("GET /api/v1/scores returned an empty body")
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode scores: %v (body: %s)", err, body)
	}

	byDev := make(map[string]float64, len(out.Developers))
	for _, d := range out.Developers {
		byDev[d.Developer] = d.TotalCostUSD
	}
	// Half A: the canonical cast is present. This alone is satisfied whether or
	// not os.Remove ran (seedDemo's idempotent writes land either way), so it
	// only detects a fully-skipped reseed, not a merge-instead-of-replace bug.
	for _, want := range canonicalDemoDevelopers() {
		if _, ok := byDev[want]; !ok {
			t.Errorf("served board missing %s — the file was not reseeded with the canonical cast", want)
		}
	}
	// Half B: the actual detector for "os.Remove was skipped". demo-ada's
	// canonical total is exactly $12.00+$9.50+$4.20 = $25.70 (see demoData());
	// the stale $999 artifact would inflate it to ~$1024.70 if it survived.
	const wantAdaCostUSD = 25.70
	if got := byDev["demo-ada"]; math.Abs(got-wantAdaCostUSD) > 0.001 {
		t.Errorf("demo-ada total_cost_usd = %.2f, want canonical %.2f — the stale $999 pre-existing row survived into the served board (os.Remove did not run)", got, wantAdaCostUSD)
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

// TestDemoSmoke_RefusesWhenTheStaleDBCannotBeDeleted is the control arm for the
// FAIL-CLOSED half of the os.Remove loop, and it exists because a security
// review REPRODUCED the hole it closes.
//
// The removes used to be `_ = os.Remove(...)` — best-effort — while the comment
// above them said UNCONDITIONAL. unlink can fail while the file stays perfectly
// openable (read-only parent directory, Docker single-file bind mount, Windows
// open handle). When it did, store.Open reopened the survivor, seedDemo MERGED
// into it, and GET /api/v1/scores served demo-ada at $1024.70 against a
// canonical $25.70 — banner printed, no warning, exit 0.
//
// Note the guard CANNOT catch this: the planted row is under demo-ada, so
// guardDemoDBPath approves it. On an unlink failure the guard is the only
// control left, and it only promises "no developer outside the demo cast".
//
// GUARD COVERAGE: revert the loop to `_ = os.Remove(...)` and this test fails —
// the child boots, binds, and serves the $999 row instead of exiting 1.
func TestDemoSmoke_RefusesWhenTheStaleDBCannotBeDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0555 directory does not block unlink for root, so the fixture cannot make os.Remove fail")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "undeletable-demo.db")
	seedStaleGuardApprovedDemoDB(t, dbPath)

	// ⚠️ THE SIDECARS ARE PART OF THE FIXTURE, NOT INCIDENTAL. guardDemoDBPath
	// opens with mode=ro, and this store runs journal_mode=WAL, so SQLite needs a
	// -shm. In a read-only directory it cannot CREATE one, and the guard fails
	// first with "attempt to write a readonly database (1544)" — the remove loop
	// is never reached and this test would pass for the wrong reason.
	//
	// Pre-creating them is also the realistic case rather than a contrivance: the
	// exposure needs a PRIOR demo run, which is the most likely state of all.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		f, err := os.Create(sidecar)
		if err != nil {
			t.Fatalf("create %s: %v", sidecar, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close %s: %v", sidecar, err)
		}
	}

	// Make unlink fail without making the file unreadable: strip write from the
	// CONTAINING directory. The files' own modes are untouched, so store.Open
	// still succeeds — precisely what let the original bug serve data.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Confirm the fixture is guard-approved UNDER the read-only directory, so
	// this test exercises the delete failure and not the #475 refuse path. This
	// assertion must come AFTER the chmod for the same reason the sidecars must
	// come before it.
	if err := guardDemoDBPath(dbPath, io.Discard); err != nil {
		t.Fatalf("test fixture invalid: must pass the guard to reach the remove loop, got: %v", err)
	}

	addr := freeLoopbackPort(t)
	childArgs := strings.Join([]string{"demo", "--addr", addr, "--db", dbPath}, "\n")
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd demo child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	// The watcher is started BEFORE we wait on the child, so it can catch a live
	// listener mid-flight. Probing only after exit has zero mutation power — the
	// mistake this file's first draft made and a review caught by measurement.
	var (
		wg          sync.WaitGroup
		stopWatcher = make(chan struct{})
		servedMu    sync.Mutex
		served      bool
	)
	watchForResponse(&wg, "http://"+addr+"/api/v1/livez", stopWatcher, &servedMu, &served)

	select {
	case err := <-exited:
		if err == nil {
			close(stopWatcher)
			wg.Wait()
			t.Fatalf("child exited 0; want non-zero — it must refuse to boot when the stale DB cannot be deleted (stderr: %s)", stderr.String())
		}
	case <-time.After(15 * time.Second):
		close(stopWatcher)
		wg.Wait()
		t.Fatalf("child did not exit within 15s; it should refuse immediately (served=%v, stderr: %s)", served, stderr.String())
	}
	close(stopWatcher)
	wg.Wait()
	servedMu.Lock()
	gotServed := served
	servedMu.Unlock()
	if gotServed {
		t.Errorf("child bound and answered on %s — it must never serve when the stale DB survived (stderr: %s)", addr, stderr.String())
	}
	if s := stderr.String(); !strings.Contains(s, "cannot delete") || !strings.Contains(s, "refusing to serve") {
		t.Errorf("stderr = %q, want it to NAME the undeletable file and say it is refusing — a bare non-zero exit leaves the operator guessing", s)
	}
	// The file must still be there: refusing is not licence to destroy the
	// operator's file by some other means.
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("stat %s after refusal: %v — the refused file must be left intact", dbPath, err)
	}
}
