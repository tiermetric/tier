//go:build integration

package main

// #689 — the unknown/guessed-model pricing WARN must actually REACH a stream at
// the log level an operator sets in order to see warnings.
//
// THE DEFECT THIS PINS. `slog.SetDefault` (main.go, immediately after flag
// parse) installs `log.SetOutput(&handlerWriter{...})` on the STDLIB `log`
// package. `handlerWriter.Write` reads:
//
//	level := w.level.Level()
//	if !w.h.Enabled(context.Background(), level) { return 0, nil }
//
// `w.level` is slog's package-level `logLoggerLevel` LevelVar, whose zero value
// is Info — a DEFAULT, not a constant: `slog.SetLogLoggerLevel` moves it, and
// nothing in this tree calls it (grep: 0 hits), so Info is what tier ran with.
// So while internal/store emitted these warnings through `log.Logger.Printf`,
// every one was rendered at INFO severity and, under `--log-level warn` or
// `--log-level error`, DISCARDED — bytes dropped, nil error returned, nothing on
// stdout, nothing on stderr, no failure anywhere. The text said "WARN:" while
// the handler saw INFO.
//
// WHY THIS TEST IS A SUBPROCESS AND CANNOT BE ANYTHING ELSE. The mechanism is
// two process-globals — `slog.SetDefault` and the stdlib log package's output —
// mutated by `runServeWithOptions`. An in-process test can set those globals but
// cannot exercise the real flag → newLogger → SetDefault → pricing-path chain in
// the configuration a released binary boots with, and it cannot observe the two
// real streams separately. A sibling service (which measured this family first)
// had a unit test PASS while the contract was broken, because it asserted on the
// returned error and the defect is in the RENDERING: Write returns n=0, err=nil.
//
// WHY "STDERR CONTAINS IT" IS NOT THE ASSERTION. That passes on a build writing
// to both streams. The discriminating pair, asserted at every level below, is:
//
//	present on STDERR (positive arms) AND ABSENT FROM STDOUT (every arm).
//
// AND WHY THE SEVERITY IS ASSERTED, NOT JUST THE TEXT. The bug was never that
// the string was missing — the string said "WARN". It was that the RECORD was
// INFO. The assertion below therefore parses the JSON record and pins
// `"level":"WARN"`, so a regression that restores the text at the wrong severity
// (invisible to every severity-keyed alert) still reddens this test.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

// unknownWarnModel is the model string posted to /api/v1/events. It must:
// survive NormalizeModel (no trailing 4/6/8-digit suffix to strip), match no
// price-table entry, and match no selfHostedClass pattern (no NNNb parameter
// count) — so it takes the FLAT guess path and warns on first sighting. Each
// arm below runs in a fresh child process, so the one-time-per-model dedupe in
// internal/store never crosses arms.
const unknownWarnModel = "acme-llm-x"

// pricingWarnMarker is the stable half of the WARN message. Kept short and
// free of punctuation that JSON escaping would alter.
const pricingWarnMarker = "not in the price table"

// logRecord is the subset of a slog JSON record these assertions read.
//
// Model is matched with Contains, not equality: logsafe.Str %q-quotes the value
// before it becomes an attribute, so the field arrives as "\"acme-llm-x\"" —
// the double-quoting the tree already accepts at its other logsafe.Str-as-attr
// sites (internal/collector/clamp.go:95 logs this very field the same way).
type logRecord struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Model string `json:"model"`
}

// findPricingWarn scans JSON-lines log output for the unknown-model pricing
// warning about unknownWarnModel and returns it. found=false means the record
// is not in this stream at all — which is the assertion for stdout, and for
// stderr at --log-level error.
//
// Non-JSON lines are skipped rather than failing: a child that dies before
// slog is installed can print a plain-text startup error, and this helper's job
// is to answer "is the pricing WARN here", not to validate every line.
func findPricingWarn(t *testing.T, stream string) (logRecord, bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec logRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if strings.Contains(rec.Msg, pricingWarnMarker) && strings.Contains(rec.Model, unknownWarnModel) {
			return rec, true
		}
	}
	// 🔴 sc.Err() MUST BE CHECKED. An over-long line ends the scan with an error
	// nobody would otherwise read, and this helper would then answer "not found"
	// — a SILENT PASS on the absence assertions, which is the same
	// guard-goes-quiet shape #689 is about.
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the captured stream failed (%v) — a truncated stream reads as "+
			"'no WARN found', which would silently pass the absence assertions", err)
	}
	return logRecord{}, false
}

// TestServeSmoke_UnknownModelWarnSurvivesLogLevel boots a REAL `tierd serve`
// child once per --log-level value, posts one event naming a model that is not
// in the price table, and asserts what lands on each stream.
//
// THE LEVEL MATRIX, AND THE REASONING FOR EACH ROW:
//
//	debug — present. The operator asked for everything.
//	info  — present. This is the only level that worked before the fix, which
//	        is precisely why the bug survived: the default setting was fine.
//	warn  — present. 🔴 THE REGRESSION ARM. Before the fix this was EMPTY, at
//	        exactly the setting an operator chooses to see warnings.
//	error — ABSENT, DELIBERATELY. A WARN is below ERROR; an operator who asks
//	        for errors only has opted out of warnings, and that is the whole
//	        point of routing through slog rather than pinning an os.Stderr sink
//	        that no level filter can reach (the issue's rejected option 2 — it
//	        would leave two logging systems with different formats and make
//	        --log-level a half-truth). This row is asserted, not skipped: it is
//	        what proves the WARN is now a REAL severity-filtered record and not
//	        an unconditional write that merely happens to be visible.
//
// Every row also asserts absence from STDOUT.
func TestServeSmoke_UnknownModelWarnSurvivesLogLevel(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	cases := []struct {
		level string
		// wantOnStderr is whether the pricing WARN must appear on stderr at
		// this level. See the matrix above for why each value is what it is.
		wantOnStderr bool
	}{
		{level: "debug", wantOnStderr: true},
		{level: "info", wantOnStderr: true},
		{level: "warn", wantOnStderr: true},
		{level: "error", wantOnStderr: false},
	}

	for _, tc := range cases {
		t.Run("--log-level="+tc.level, func(t *testing.T) {
			addr := freeLoopbackPort(t)
			dbPath := filepath.Join(t.TempDir(), "warn-level-smoke.db")

			// --log-format json is explicit, not "auto": auto resolves by TTY
			// detection on stderr, and stderr here is a pipe. Pinning it keeps
			// the record shape the assertions parse independent of where the
			// test runs.
			childArgs := strings.Join([]string{
				"serve",
				"--addr", addr,
				"--db", dbPath,
				"--aggregation", "developer",
				"--log-format", "json",
				"--log-level", tc.level,
			}, "\n")

			cmd := exec.Command(self)
			// scrubbedEnv, not os.Environ(): an ambient TIER_PRICES pointing at a
			// table that happens to price unknownWarnModel would make the three
			// positive arms fail for the wrong reason AND make the `error` arm
			// pass while proving nothing — a vacuous green driven by the
			// developer's shell. (TIER_API_TOKEN and TIER_READ_ONLY would break
			// the POST loudly rather than silently, but there is no reason to
			// inherit any of them.)
			cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
			// BOTH streams, captured SEPARATELY. This separation is the test:
			// a single combined buffer cannot tell "correctly on stderr" from
			// "leaked to stdout too".
			stdout := &lockedBuffer{}
			stderr := &lockedBuffer{}
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start tierd child: %v", err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

			base := "http://" + addr
			waitUntilLive(t, base+"/api/v1/livez", stderr, exited)

			postUnknownModelEvent(t, base)

			// Positive arms: poll for the record rather than reading once.
			// os/exec copies the child's stderr on its own goroutine, so the
			// bytes can lag the 201 by a scheduling hop. Absence arms are
			// asserted only AFTER the child exits, below, where the streams
			// are complete by construction.
			if tc.wantOnStderr {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, ok := findPricingWarn(t, stderr.String()); ok {
						break
					}
					// 🔴 WATCH `exited`, the way waitUntilLive does. Without this
					// a child that dies after livez burns the full 10s and then
					// reports "This is #689: slog.SetDefault redirects the stdlib
					// log package…" — a confident, specific, WRONG diagnosis of a
					// crash. A guard that names the wrong cause is worse than one
					// that simply fails.
					select {
					case err := <-exited:
						t.Fatalf("child exited before the WARN arrived: %v — this is a dead child, "+
							"NOT #689.\nstderr:\n%s", err, stderr.String())
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("the unknown-model pricing WARN never reached stderr at --log-level %s.\n"+
							"This is #689: slog.SetDefault redirects the stdlib log package through a "+
							"handler that discards below the bridge level and returns a nil error.\nstderr:\n%s",
							tc.level, stderr.String())
					}
					time.Sleep(50 * time.Millisecond)
				}
			}

			// Stop the child so both streams are final before the absence
			// assertions read them.
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

			rec, onStderr := findPricingWarn(t, stderr.String())
			if onStderr != tc.wantOnStderr {
				t.Fatalf("pricing WARN on stderr = %v at --log-level %s, want %v.\nstderr:\n%s",
					onStderr, tc.level, tc.wantOnStderr, stderr.String())
			}
			if onStderr && rec.Level != "WARN" {
				t.Errorf("pricing WARN was emitted at level %q, want \"WARN\" at --log-level %s.\n"+
					"The #689 defect was NOT a missing string — the text always said WARN. It was the "+
					"RECORD being INFO, which is invisible to every severity-keyed alert.",
					rec.Level, tc.level)
			}

			// THE DISCRIMINATING ASSERTION, at every level including error:
			// tierd's logs go to stderr, and a fix that pinned an unconditional
			// os.Stderr sink or wrote to both streams must not pass here.
			//
			// It asserts stdout is EMPTY rather than "does not contain the WARN",
			// and that is deliberately the stronger form: `tierd serve` writes
			// nothing to stdout at all, so emptiness is the true contract, it
			// cannot decay into a tautology if findPricingWarn ever stops
			// matching, and it additionally catches an unwired cmd.Stdout — which
			// a "does not contain" check would read as a pass.
			if s := stdout.String(); s != "" {
				t.Errorf("tierd serve wrote %d bytes to STDOUT at --log-level %s; it must write "+
					"logs to stderr only.\nstdout:\n%s", len(s), tc.level, s)
			}
		})
	}
}

// postUnknownModelEvent posts one well-formed /api/v1/events element naming
// unknownWarnModel. This is the REAL pricing path — internal/api's handler calls
// store.ComputeCostHost, which is what emits the WARN — not a synthetic call to
// the logger.
//
// cost_usd is set to the value the server's own table produces for these token
// counts (1,000,000 input tokens at the $0.50/M self-hosted-medium guess rate),
// so the #233 shipper-divergence WARN does not fire and add an unrelated record
// to the stream under test.
func postUnknownModelEvent(t *testing.T, base string) {
	t.Helper()
	body := fmt.Sprintf(`[{
		"developer": "alice",
		"issue_id": "issue-689",
		"model": %q,
		"input_tokens": 1000000,
		"output_tokens": 0,
		"cost_usd": 0.50,
		"source": "jsonl",
		"fidelity": "realtime",
		"idempotency_key": "tier-689-unknown-model-warn",
		"timestamp": %q
	}]`, unknownWarnModel, time.Now().UTC().Format(time.RFC3339))

	resp, err := http.Post(base+"/api/v1/events", "application/json", strings.NewReader(body)) //nolint:noctx // loopback test POST
	if err != nil {
		t.Fatalf("POST /api/v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /api/v1/events response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/events status = %d, want 201 — the pricing path was never reached, so this test proves nothing.\nbody: %s",
			resp.StatusCode, bytes.TrimSpace(got))
	}
}
