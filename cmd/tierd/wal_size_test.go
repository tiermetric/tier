package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the #669 WAL size tripwire — the instrument that had to exist before
// the store's connection pool could be raised above 1.
//
// 🔴 WHAT IT WATCHES FOR, because the threshold is meaningless without it. A
// SQLite WAL is checkpointed passively at commit, and a passive checkpoint can
// only RESET the file when no reader holds an older snapshot. At a pool of 1 a
// reader and the writer shared the one connection, so a commit never found a
// concurrent reader. Above 1 it can, and the WAL then grows without bound —
// MEASURED 2026-08-13 at 12.9MB after 1200 writes under back-to-back reads,
// against a 4.15MB ceiling in the zero-reader control. Disk is the first symptom.

// TestEvalWALSize pins the pure predicate, including both sides of the boundary.
// Split from the stat/gauge/log machinery for the same reason evalZeroOutcome is.
func TestEvalWALSize(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want bool
	}{
		{"empty -> clear", 0, false},
		{"the measured healthy ceiling (~4.1MB, 1000 pages) -> clear", 4_148_872, false},
		{"the worst overshoot measured under a periodic checkpoint (1.68x) -> clear", 6_892_792, false},
		{"exactly at the threshold -> clear (strictly greater trips)", walSizeWarnBytes, false},
		{"one byte over -> tripped", walSizeWarnBytes + 1, true},
		{"far over -> tripped", 512 << 20, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalWALSize(c.size); got != c.want {
				t.Errorf("evalWALSize(%d) = %v, want %v", c.size, got, c.want)
			}
		})
	}
}

// TestCheckWALSize covers the three filesystem states, and the ABSENT case is the
// one worth having: the -wal sidecar does not exist until the first write after
// open and is removed on a clean close. Both are healthy and must read as 0 —
// never as a warning, and never as a stat error.
func TestCheckWALSize(t *testing.T) {
	newGauge := func() (*serveMetrics, func() string) {
		sm := newServeMetrics("v0.0.0-test")
		return sm, func() string {
			var sb strings.Builder
			sm.reg.Render(&sb)
			return sb.String()
		}
	}

	t.Run("absent -wal is healthy, not an error", func(t *testing.T) {
		sm, render := newGauge()
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		dbPath := filepath.Join(t.TempDir(), "tier.db")
		if tripped := checkWALSize(dbPath, sm.walBytes, sm.walStatErrors, logger); tripped {
			t.Error("a missing -wal tripped the warning; it is the normal state before the first write and after a clean close")
		}
		if !strings.Contains(render(), "tier_sqlite_wal_bytes 0") {
			t.Errorf("missing -wal should publish 0:\n%s", render())
		}
		if strings.Contains(buf.String(), "stat failed") {
			t.Errorf("a missing -wal must not log a stat error: %q", buf.String())
		}
	})

	t.Run("small -wal publishes its size and does not warn", func(t *testing.T) {
		sm, render := newGauge()
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		dbPath := filepath.Join(t.TempDir(), "tier.db")
		writeWAL(t, dbPath, 1024)
		if tripped := checkWALSize(dbPath, sm.walBytes, sm.walStatErrors, logger); tripped {
			t.Error("a 1KiB -wal tripped the warning")
		}
		if !strings.Contains(render(), "tier_sqlite_wal_bytes 1024") {
			t.Errorf("expected the real size on the gauge:\n%s", render())
		}
		if strings.Contains(buf.String(), "wal size tripwire") {
			t.Errorf("healthy -wal must not WARN: %q", buf.String())
		}
	})

	t.Run("oversized -wal trips and says WHY", func(t *testing.T) {
		sm, render := newGauge()
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		dbPath := filepath.Join(t.TempDir(), "tier.db")
		writeWAL(t, dbPath, walSizeWarnBytes+1)
		if tripped := checkWALSize(dbPath, sm.walBytes, sm.walStatErrors, logger); !tripped {
			t.Fatal("an oversized -wal did NOT trip — this is the disk-exhaustion signal and it is the only one")
		}
		// 🔴 THE SERIES LINE, NOT THE BARE METRIC NAME. `# HELP tier_sqlite_wal_bytes`
		// and `# TYPE tier_sqlite_wal_bytes gauge` are written UNCONDITIONALLY at
		// render time, so a bare `Contains("tier_sqlite_wal_bytes")` is true even
		// when the gauge holds no series at all — it would stay green with
		// checkWALSize's gauge.Set deleted, which is the one case where an operator
		// most needs the size on the wire. Asserting the VALUE is what makes this
		// arm able to fail.
		want := fmt.Sprintf("tier_sqlite_wal_bytes %d", int64(walSizeWarnBytes)+1)
		if !strings.Contains(render(), want) {
			t.Errorf("expected the tripped size on the wire (%q):\n%s", want, render())
		}
		// The message must name the CAUSE, not just the number: an operator who
		// reads "wal is big" has no next action, and the next action (find the
		// client polling with no gap) is the whole value of the warning.
		if !strings.Contains(buf.String(), "never RESET") {
			t.Errorf("the WARN must explain that a reader is blocking the checkpoint reset, got: %q", buf.String())
		}
	})
}

// TestCheckWALSizeStatError covers the branch whose whole purpose is to NOT
// touch the gauge — the one case where "the code does nothing" is the behaviour
// under test, and therefore the one most easily broken without any test noticing.
//
// Reached without mocks: make `x` a regular FILE and ask for `x/db-wal`, which
// stats as ENOTDIR rather than ErrNotExist.
func TestCheckWALSizeStatError(t *testing.T) {
	sm := newServeMetrics("v0.0.0-test")
	render := func() string {
		var sb strings.Builder
		sm.reg.Render(&sb)
		return sb.String()
	}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// A prior healthy sample, so "left untouched" is distinguishable from "set 0".
	sm.walBytes.Set(4_148_872)

	notADir := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(notADir, []byte("i am a file"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	dbPath := filepath.Join(notADir, "db")

	if tripped := checkWALSize(dbPath, sm.walBytes, sm.walStatErrors, logger); tripped {
		t.Error("a stat error must not report the WAL as tripped")
	}
	// 🔴 THE GAUGE MUST BE UNCHANGED, NOT ZEROED. Flapping to 0 on a transient
	// filesystem error would read as "the WAL is fine now", which is the exact
	// false-green this branch exists to avoid.
	if !strings.Contains(render(), "tier_sqlite_wal_bytes 4148872") {
		t.Errorf("a stat error must LEAVE the gauge at its last known value:\n%s", render())
	}
	// 🔴 AND IT MUST BE COUNTED, or a PERMANENT failure is indistinguishable from
	// a healthy WAL: the gauge would sit at its last good sample forever.
	if !strings.Contains(render(), "tier_sqlite_wal_stat_errors_total 1") {
		t.Errorf("a stat error must increment the error counter, or the frozen gauge above reads as healthy:\n%s", render())
	}
	if !strings.Contains(buf.String(), "stat failed") {
		t.Errorf("expected a stat-failure WARN, got: %q", buf.String())
	}
}

// TestCheckWALSizeSuppressesARepeatWarn pins the warn-on-TRANSITION behaviour.
// The tripped condition is permanent by construction (a starved WAL does not
// shrink on its own), so warning every 5-minute pass would emit 288 identical
// WARNs a day and train operators to filter the one signal that matters.
func TestCheckWALSizeSuppressesARepeatWarn(t *testing.T) {
	sm := newServeMetrics("v0.0.0-test")
	dbPath := filepath.Join(t.TempDir(), "tier.db")
	writeWAL(t, dbPath, walSizeWarnBytes+1)

	var first strings.Builder
	if !checkWALSize(dbPath, sm.walBytes, sm.walStatErrors, slog.New(slog.NewTextHandler(&first, nil)), false) {
		t.Fatal("first crossing should trip")
	}
	if !strings.Contains(first.String(), "wal size tripwire") {
		t.Errorf("the FIRST crossing must WARN, got: %q", first.String())
	}

	var second strings.Builder
	if !checkWALSize(dbPath, sm.walBytes, sm.walStatErrors, slog.New(slog.NewTextHandler(&second, nil)), true) {
		t.Error("a still-tripped WAL must still REPORT tripped, even when the warn is suppressed")
	}
	if strings.Contains(second.String(), "wal size tripwire") {
		t.Errorf("an already-reported condition must not re-WARN every pass, got: %q", second.String())
	}
	// The METRIC must stay continuous even while the log is quiet — otherwise
	// suppressing the warn would also blind the scraper.
	var sb strings.Builder
	sm.reg.Render(&sb)
	want := fmt.Sprintf("tier_sqlite_wal_bytes %d", int64(walSizeWarnBytes)+1)
	if !strings.Contains(sb.String(), want) {
		t.Errorf("the gauge must keep tracking while the warn is suppressed (%q):\n%s", want, sb.String())
	}
}

// TestNewServeMetrics_RegistersWALGauge pins the metric under its exact name,
// because that name is the wire contract a scraper depends on.
func TestNewServeMetrics_RegistersWALGauge(t *testing.T) {
	sm := newServeMetrics("v1.2.3")
	sm.walBytes.Set(0)
	var sb strings.Builder
	sm.reg.Render(&sb)
	if !strings.Contains(sb.String(), "tier_sqlite_wal_bytes 0") {
		t.Errorf("serve metric set missing tier_sqlite_wal_bytes:\n%s", sb.String())
	}
}

// writeWAL creates a -wal sidecar of exactly n bytes next to dbPath. It uses
// Truncate rather than writing n real bytes so the 64MiB case costs no disk.
func writeWAL(t *testing.T, dbPath string, n int64) {
	t.Helper()
	p := dbPath + "-wal"
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create fake -wal: %v", err)
	}
	if err := f.Truncate(n); err != nil {
		t.Fatalf("size fake -wal: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close fake -wal: %v", err)
	}
}
