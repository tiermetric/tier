package main

// #689 — the FAST arm of the unknown-model-WARN level guard, composing tierd's
// REAL logger constructor with internal/store's REAL pricing path.
//
// The authoritative guard is TestServeSmoke_UnknownModelWarnSurvivesLogLevel
// (unknown_model_warn_level_smoke_test.go), which boots a real `tierd serve`
// child, posts a real event, and reads the two real streams separately — the
// only shape that discriminates a defect living in process-globals. That test
// is behind //go:build integration, so it runs in `make check-full`.
//
// This file exists so `make check` reddens too. It is deliberately NOT a
// substitute: it mutates slog's process default in-process, which means it can
// prove the level filter now carries the WARN through, but cannot prove
// anything about stdout-vs-stderr or about flag parsing. Read it as the cheap
// tripwire, not the proof.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// unknownModelSeq makes every model string these tests price unique within the
// process.
//
// 🔴 IT EXISTS BECAUSE `go test -count=2 ./cmd/tierd/` FAILED WITHOUT IT, and
// the failure was in the GUARD, not the code. internal/store's #286 dedupe
// ("one WARN per distinct model") is a process-global sync.Map, and -count=N
// reruns every test IN THE SAME PROCESS. So a fixed model string warns on
// iteration 1 and is silently deduped on iteration 2 — the guard then reports
// "no WARN reached the sink", which is indistinguishable from the very #689
// regression it exists to catch. A guard whose failure mode is a false RED on
// the second run is a guard people learn to ignore.
//
// The store's own reset helper (resetUnknownModelDedupe) is package-private to
// internal/store and cannot be reached from package main, and exporting one
// purely for tests would put a reset-the-security-cap lever on the public API.
// A per-call unique NAME sidesteps the dedupe instead of disarming it — which
// also keeps these tests honest about the dedupe rather than turning it off.
//
// A COUNTER, NOT A RANDOM SUFFIX: -count=N shares one process, so an atomic
// counter is both unique and deterministic, and a failure reproduces exactly.
//
// ⚠️ THIS RAISES THE -count CEILING; IT DOES NOT REMOVE IT. Each iteration of
// this file's tests consumes a handful of slots from the same process-global
// maxUnknownModelWarn = 1024 cap, so somewhere past roughly -count=200 the cap
// trips, the per-model WARNs are suppressed by design, and the positive arms
// report the very "#689 regression" false RED this helper exists to prevent —
// one order of magnitude further out than before, not gone. Nothing in `make
// check` runs anywhere near that (`go test -race -count=1 ./...`).
var unknownModelSeq atomic.Int64

// freshUnknownModel returns a model string derived from base that has never
// been priced before in this process.
//
// The base-26 letter suffix cannot introduce a parameter count: paramCountRE
// requires digits (optionally decimal) followed by b with no trailing letter or
// digit. This keeps flat-guess names on that path while making them unique.
func freshUnknownModel(base string) string {
	n := unknownModelSeq.Add(1)
	var suf []byte
	for n > 0 {
		suf = append(suf, byte('a'+(n-1)%26))
		n = (n - 1) / 26
	}
	return base + "-" + string(suf)
}

// lockedTestBuffer is a mutex-guarded io.Writer sink for a logger installed as
// the PROCESS DEFAULT.
//
// The mutex is not ceremony: internal/collector logs via top-level slog.Warn
// from watcher goroutines, so any goroutine outliving an earlier test in this
// package writes through slog.Default() — into this buffer — while the test
// goroutine calls String(). That is a data race a plain bytes.Buffer loses under
// -race. serve_smoke_test.go's lockedBuffer is the same idea, but it sits behind
// //go:build integration and so is unreachable from this untagged file.
type lockedTestBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedTestBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedTestBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// warnRecord is the subset of a slog JSON record these assertions read.
//
// Model is matched with Contains, not equality: logsafe.Str %q-quotes the value
// before it becomes an attribute, so the field arrives as "\"acme-llm-x\"" —
// the double-quoting the tree already accepts at its other 49 logsafe.Str-as-attr
// sites (internal/collector/clamp.go:95 logs this very field the same way).
type warnRecord struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Model string `json:"model"`
}

// pricingWarns returns every unknown-model pricing WARN record in a JSON-lines
// buffer whose model attribute names model. Returning the SLICE (not a bool) is
// what lets the dedupe arm below count occurrences.
//
// 🔴 sc.Err() IS CHECKED, and t is taken purely so it can be. A line longer than
// the scanner's cap ends the scan with an error nobody would otherwise read, and
// this helper would then report "not found" — turning a truncated stream into a
// silent PASS on every absence assertion. That is the same guard-goes-quiet
// shape #689 is about, so it must not be reachable from inside the guard. The
// cap is also raised explicitly to match the smoke test's, rather than left at
// the 64 KiB default.
func pricingWarns(t *testing.T, stream, model string) []warnRecord {
	t.Helper()
	var out []warnRecord
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r warnRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if strings.Contains(r.Msg, "not in the price table") && strings.Contains(r.Model, model) {
			out = append(out, r)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning captured log failed (%v) — a truncated stream would read as "+
			"'no WARN found', which is a silent pass on every absence assertion here", err)
	}
	return out
}

// withDefaultLoggerAt installs a JSON logger at the given --log-level string,
// built by tierd's OWN newLogger, as the process default for the duration of
// the calling test, and returns the buffer it writes to.
//
// 🔴 RESTORING slog.Default() IS NECESSARY AND NOT SUFFICIENT, AND THE
// INSUFFICIENCY IS #689 ITSELF. An earlier version of this helper restored only
// slog.Default() — citing lifecycle_test.go — and that left the process with a
// silently-discarding logger for the rest of the test binary. Measured: after
// the last subtest, log.Default().Writer() was a *slog.handlerWriter over a dead
// bytes.Buffer, log.Flags() was 0 (from 3), and BOTH log.Printf and
// slog.Default().Warn reached no stream at all.
//
// The mechanism is the asymmetry inside slog.SetDefault (go1.26.6
// $GOROOT/src/log/slog/logger.go):
//
//	if _, ok := l.Handler().(*defaultHandler); !ok {
//		log.SetOutput(&handlerWriter{l.Handler(), &logLoggerLevel, capturePC})
//		log.SetFlags(0)
//	}
//
// Installing a real handler TAKES the stdlib log package over; restoring the
// built-in logger does NOT give it back, because that restore takes the
// defaultHandler branch and skips log.SetOutput entirely. And defaultHandler
// routes back out through log.Output — so once the bridge points at a dead
// buffer, slog.Default() is dead too.
//
// ⇒ A guard for silent log discard that installed silent log discard. The
// stdlib globals must be restored explicitly, the way guess_path_label_test.go
// already pairs them.
func withDefaultLoggerAt(t *testing.T, level string) *lockedTestBuffer {
	t.Helper()
	buf := &lockedTestBuffer{}
	logger, err := newLogger(buf, "json", level)
	if err != nil {
		t.Fatalf("newLogger(%q): %v", level, err)
	}
	origSlog := slog.Default()
	origWriter, origFlags := log.Writer(), log.Flags()
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(origSlog)
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	return buf
}

// TestUnknownModelWarn_LevelMatrix pins what an operator sees at each
// --log-level value. See the smoke test's matrix comment for the reasoning
// behind each row; the short version:
//
//	debug/info/warn — the WARN is emitted, AT WARN SEVERITY.
//	error           — deliberately silent: a WARN is below ERROR, and an
//	                  operator who asked for errors only has opted out.
//
// Every arm draws its model from freshUnknownModel, so no two arms — and no two
// -count iterations — ever share a name. internal/store's one-time dedupe is
// process-global; a shared name would let arm 2 "pass" on arm 1's WARN, and a
// reused one would make iteration 2 report a false regression. See
// freshUnknownModel for the measured failure that forced this.
func TestUnknownModelWarn_LevelMatrix(t *testing.T) {
	cases := []struct {
		level string
		want  bool
	}{
		{level: "debug", want: true},
		{level: "info", want: true},
		{level: "warn", want: true},
		{level: "error", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			buf := withDefaultLoggerAt(t, tc.level)
			model := freshUnknownModel("acme-llm-lvl-" + tc.level)

			store.ComputeCost(model, store.CostUsage{Input: 1_000_000})

			got := pricingWarns(t, buf.String(), model)
			if (len(got) > 0) != tc.want {
				t.Fatalf("pricing WARN for %q at --log-level %s: found %d records, want present=%v.\n"+
					"#689: slog.SetDefault redirects the STDLIB log package through a handler that "+
					"discards below INFO and returns a nil error, so a log.Logger sink here vanishes "+
					"at exactly the level an operator sets to see warnings.\nlog:\n%s",
					model, tc.level, len(got), tc.want, buf.String())
			}
			if tc.want && got[0].Level != "WARN" {
				t.Errorf("record level = %q, want \"WARN\" — the text always said WARN; the bug was the "+
					"RECORD being INFO, which no severity-keyed alert can see", got[0].Level)
			}

			// 🔴 LIVENESS CONTROL FOR THE ABSENCE ARM. `error` asserts that
			// NOTHING was emitted, and an absence assertion passes just as
			// happily when the emit path never ran at all — a dedupe slot
			// already consumed, the model somehow priced, an early return in
			// warnUnknownModel. Re-run the same pricing call against a
			// warn-level sink and require a record: that proves the path was
			// live at this instant, so the silence above was the LEVEL FILTER
			// and not a dead code path.
			if !tc.want {
				live := withDefaultLoggerAt(t, "warn")
				control := freshUnknownModel("acme-llm-lvl-" + tc.level + "-control")
				store.ComputeCost(control, store.CostUsage{Input: 1_000_000})
				if len(pricingWarns(t, live.String(), control)) == 0 {
					t.Fatalf("control failed: the pricing WARN path emitted nothing even at "+
						"--log-level warn, so the silence asserted at --log-level %s proves "+
						"nothing about the level filter.\nlog:\n%s", tc.level, live.String())
				}
			}
		})
	}
}

// TestUnknownModelWarn_DedupeSurvivesTheSlogMove is the do-no-harm arm for
// #286's one-WARN-per-distinct-model gate. Routing these warnings through slog
// changed the SINK, and a fix that (say) moved the emit above
// claimUnknownModelWarn would restore visibility while re-opening the WARN
// flood an attacker-varied model stream can drive.
//
// The distinct-model CAP (maxUnknownModelWarn = 1024) is pinned in
// internal/store — TestUnknownModelWarnSetIsBounded and
// TestUnknownModelWarnBoundHoldsUnderConcurrency — because the counters it asserts on are
// package-private there. This arm covers the per-model dedupe over the REAL
// default-logger path those tests bypass via the override seam.
func TestUnknownModelWarn_DedupeSurvivesTheSlogMove(t *testing.T) {
	buf := withDefaultLoggerAt(t, "warn")
	model := freshUnknownModel("acme-llm-dedupe-probe")

	for i := 0; i < 5; i++ {
		store.ComputeCost(model, store.CostUsage{Input: 1_000})
	}

	if got := pricingWarns(t, buf.String(), model); len(got) != 1 {
		t.Errorf("5 identical unknown-model pricings produced %d WARNs, want exactly 1 — "+
			"the #286 dedupe did not survive the move to slog.\nlog:\n%s", len(got), buf.String())
	}
}

// TestUnknownModelWarn_HelperRestoresStdlibLogBridge pins the fix for the leak
// described on withDefaultLoggerAt.
//
// WHY A TEST FOR A TEST HELPER. The leak was INVISIBLE: it broke no assertion,
// printed nothing, and returned no error — it merely left every later log.Printf
// and slog.Default() call in the binary writing into a dead buffer. That is
// precisely the failure mode #689 is about, so the helper that guards against it
// must not be the thing that causes it, and "we restore it now" is a claim that
// needs an assertion behind it like any other.
//
// The three globals asserted are exactly the three slog.SetDefault mutates:
// slog's own default, log.Writer(), and log.Flags().
func TestUnknownModelWarn_HelperRestoresStdlibLogBridge(t *testing.T) {
	beforeSlog := slog.Default()
	beforeWriter, beforeFlags := log.Writer(), log.Flags()

	// A nested scope so the helper's t.Cleanup fires while we can still observe.
	t.Run("inner", func(t *testing.T) {
		buf := withDefaultLoggerAt(t, "warn")
		store.ComputeCost(freshUnknownModel("acme-llm-restore-probe"), store.CostUsage{Input: 1_000})
		// Control: the helper really was installed, so the restore asserted
		// below is undoing something rather than confirming a no-op.
		if buf.String() == "" {
			t.Fatal("helper captured nothing — it was never installed, so this test proves nothing")
		}
		if log.Writer() == beforeWriter {
			t.Error("log.Writer() unchanged while the helper was active; slog.SetDefault did not " +
				"take over the stdlib bridge, so there is nothing here to leak or to restore")
		}
	})

	if got := slog.Default(); got != beforeSlog {
		t.Errorf("slog.Default() not restored: got %p, want %p", got, beforeSlog)
	}
	if got := log.Writer(); got != beforeWriter {
		t.Errorf("log.Writer() not restored: got %T, want the original %T.\n"+
			"slog.SetDefault installs a *slog.handlerWriter over the stdlib log package, and "+
			"restoring slog.Default() does NOT undo it — that restore takes the defaultHandler "+
			"branch and skips log.SetOutput. The bridge stays pointed at a dead buffer and every "+
			"later log.Printf in this binary is silently discarded.", got, beforeWriter)
	}
	if got := log.Flags(); got != beforeFlags {
		t.Errorf("log.Flags() not restored: got %d, want %d (slog.SetDefault zeroes them)", got, beforeFlags)
	}
}
