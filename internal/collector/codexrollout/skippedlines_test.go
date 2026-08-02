package codexrollout

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The #526 regression.
//
// Malformed rollout lines are TOLERATED by design — Codex appends to these logs
// while it runs, so failing a file on one bad line would break `serve`. That
// tolerance is correct and this change does not touch it. What was wrong is that
// the loss was unquantified: the skip count reached a logger.Warn and nothing
// else, so no consumer could say "some spend is missing here".
//
// The decisive case is a log damaged badly enough to lose every billable line.
// parseRollout used to return (nil, nil) for it, which scanOnce classified as
// idle — so a fully-corrupt log was INDISTINGUISHABLE from a session that
// genuinely never spent anything. Those two states mean opposite things: one
// session cost nothing, the other cost something no longer measurable. For TIER
// that is the #492 shape — outcomes still arrive by webhook, so unmeasured spend
// makes the work read as cheaper than it was.
//
// ASSERT VALUES, NOT KEYS. The scan summary emits every key unconditionally once
// it fires, so `strings.Contains(logs, "damaged_files")` passes with
// damaged_files=0. A first version of these tests did exactly that, and mutations
// printing a hardcoded 0 — or counting every file as damaged — survived the whole
// suite. Every summary assertion below pins the number.

// warnPrefix is the damaged-to-empty warning, INCLUDING the level. "WARN, not
// Info" is a documented decision (a demotion silences it on the `ship` path,
// which runs at warn), and a message-only match cannot see a demotion.
const warnPrefix = `level=WARN msg="codex-rollout: a rollout log lost EVERY billable line`

// garbageLine is a COMPLETE (newline-terminated) line that cannot decode — real
// damage, as opposed to a mid-flush tail.
const garbageLine = `{"timestamp":"2026-07-23T00:16:00.000Z","type":"event_ms` + "\n"

// wantSummary asserts every given substring appears in the captured log.
func wantSummary(t *testing.T, logs string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(logs, w) {
			t.Errorf("scan summary missing %q — the counters do not report what the scan "+
				"actually saw:\n%s", w, logs)
		}
	}
}

// TestSkippedLines_DamagedToEmptyIsDistinguishableFromIdle is the core #526
// assertion, and it is a DISCRIMINATION test: the two fixtures below produce the
// same externally-visible outcome (zero billable calls, no error, exit 0) and
// must now be tellable apart.
func TestSkippedLines_DamagedToEmptyIsDistinguishableFromIdle(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	// No token_count snapshots: session_meta + turn_context only.
	idleBody := synthetic(repo, testBranch, "gpt-5.6-terra", nil)

	t.Run("genuinely idle session reports no damage", func(t *testing.T) {
		sessions := t.TempDir()
		path := writeRollout(t, sessions, "idle.jsonl", idleBody)

		sess, err := parseRollout(path, quietLogger())
		if err != nil {
			t.Fatalf("parseRollout: %v", err)
		}
		// A successful parse now ALWAYS returns a session, even with no calls —
		// that is what carries SkippedLines out. A nil here would mean the
		// (nil, nil) return came back and the whole fix is inert.
		if sess == nil {
			t.Fatal("parseRollout returned a nil session for a clean idle log; the skip " +
				"count can no longer reach any caller (#526 regression)")
		}
		if len(sess.Calls) != 0 {
			t.Fatalf("Calls = %d, want 0 — the fixture is no longer idle, so this test "+
				"is not exercising the case it claims to", len(sess.Calls))
		}
		if sess.SkippedLines != 0 {
			t.Errorf("SkippedLines = %d, want 0 — an undamaged log must report no loss, "+
				"or the signal means nothing", sess.SkippedLines)
		}
	})

	t.Run("damaged to empty reports the loss", func(t *testing.T) {
		sessions := t.TempDir()
		path := writeRollout(t, sessions, "damaged-empty.jsonl", idleBody+garbageLine+garbageLine)

		sess, err := parseRollout(path, quietLogger())
		// Still NOT an error: the live-append tolerance is deliberate.
		if err != nil {
			t.Fatalf("parseRollout returned an error for malformed lines; #526 is additive "+
				"and must not make them fatal (that would break serve on live logs): %v", err)
		}
		if sess == nil {
			t.Fatal("parseRollout returned (nil, nil) for a log that lost every billable " +
				"line — this is the exact #526 defect: indistinguishable from an idle session")
		}
		if len(sess.Calls) != 0 {
			t.Fatalf("Calls = %d, want 0", len(sess.Calls))
		}
		if sess.SkippedLines != 2 {
			t.Errorf("SkippedLines = %d, want 2 — the lost lines must be COUNTED, not just "+
				"logged; a warning is not a return value", sess.SkippedLines)
		}
	})
}

// TestSkippedLines_ScanWarnsWhenAFileIsDamagedToEmpty pins the operator-facing
// half AND the machine-readable half. The parser counts the loss; this asserts
// the scan says so at WARN and that the COUNTERS separate it from an idle file.
//
// The counter half is the part that was missing: with only the WARN asserted, a
// build where damaged_to_empty is permanently 0 and no_token_count is inflated
// passed the whole suite.
func TestSkippedLines_ScanWarnsWhenAFileIsDamagedToEmpty(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	idleBody := synthetic(repo, testBranch, "gpt-5.6-terra", nil)

	t.Run("damaged to empty warns and is counted apart from idle", func(t *testing.T) {
		sessions := t.TempDir()
		writeRollout(t, sessions, "damaged-empty.jsonl", idleBody+garbageLine)

		c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
		if _, err := c.Collect(context.Background(), testSince); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if !strings.Contains(logs.String(), warnPrefix) {
			t.Errorf("a log that lost every billable line did not warn AT WARN LEVEL; its "+
				"missing spend is reported as an idle session (#526):\n%s", logs.String())
		}
		wantSummary(t, logs.String(),
			"damaged_to_empty=1", // the whole point: NOT idle
			"no_token_count=0",   // and it was taken OUT of idle, not added alongside
			"damaged_files=1",
			"skipped_lines=1",
		)
	})

	t.Run("a genuinely idle session does NOT warn and is counted as idle", func(t *testing.T) {
		sessions := t.TempDir()
		writeRollout(t, sessions, "idle.jsonl", idleBody)

		c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
		if _, err := c.Collect(context.Background(), testSince); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if strings.Contains(logs.String(), warnPrefix) {
			t.Errorf("an undamaged idle session raised the damage warning. Codex sessions "+
				"that spent nothing are routine, so this would fire constantly and train "+
				"operators to filter the signal:\n%s", logs.String())
		}
		// Positive anchor FIRST: proves the file was actually scanned, so the
		// negative assertion above is not about an empty buffer.
		wantSummary(t, logs.String(),
			"no_token_count=1",
			"damaged_files=0",
			"damaged_to_empty=0",
			"skipped_lines=0",
		)
	})
}

// TestSkippedLines_ForeignRepoDamageIsNotOurLoss is the control arm for the scope
// gate.
//
// ~/.codex/sessions is machine-GLOBAL: Codex writes every session on the box
// there, while this collector attributes only the configured repos. So a foreign
// session is the COMMON case. Accounting damage before the scope decision claimed
// "our spend is missing" for logs that were never in scope — a warning that fires
// routinely, which is the #464 defect the idle control arm above already guards
// against, re-entered through a different door.
func TestSkippedLines_ForeignRepoDamageIsNotOurLoss(t *testing.T) {
	watched := initGitRepo(t, t.TempDir())
	foreign := initGitRepo(t, t.TempDir())
	// The session ran in `foreign`; the collector watches `watched`.
	foreignBody := synthetic(foreign, testBranch, "gpt-5.6-terra", nil)

	sessions := t.TempDir()
	writeRollout(t, sessions, "foreign-damaged.jsonl", foreignBody+garbageLine)

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: watched})
	if _, err := c.Collect(context.Background(), testSince); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if strings.Contains(logs.String(), warnPrefix) {
		t.Errorf("a damaged session from a repo this collector does not watch warned that "+
			"OUR spend is missing. It was never in scope, and on a real machine foreign "+
			"sessions are the common case — this warning would fire constantly:\n%s", logs.String())
	}
	wantSummary(t, logs.String(),
		"damaged_files=0",    // not our loss
		"damaged_to_empty=0", // not our loss
		"skipped_lines=0",    // not our loss
		"no_token_count=1",   // it is simply a zero-call session, as before
	)
}

// TestSkippedLines_RepeatScanSuppressesTheWarning pins the #464 Y-G3 posture for
// the new warning.
//
// The bytes do not change between scans, so a 5-minute serve loop — or a
// 15-minute ship cron — would emit this same line forever with no action an
// operator can take to clear it. "It never self-heals" is the argument FOR
// reporting it once, not for repeating it; the fatal-parse path is memoized for
// exactly this reason.
func TestSkippedLines_RepeatScanSuppressesTheWarning(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	idleBody := synthetic(repo, testBranch, "gpt-5.6-terra", nil)

	sessions := t.TempDir()
	writeRollout(t, sessions, "damaged-empty.jsonl", idleBody+garbageLine)

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	for i := range 3 {
		if _, err := c.Collect(context.Background(), testSince); err != nil {
			t.Fatalf("Collect %d: %v", i+1, err)
		}
	}
	if got := strings.Count(logs.String(), warnPrefix); got != 1 {
		t.Errorf("the damage warning fired %d times across 3 scans of unchanged bytes, want 1. "+
			"An unactionable line repeated every interval is how a real signal becomes cron "+
			"noise operators filter (#464 Y-G3):\n%s", got, logs.String())
	}
}

// TestSkippedLines_ScanAggregatesAcrossDamagedFiles covers the counters AS
// ACCUMULATORS. Every other test here uses exactly one rollout file, which cannot
// tell a running total from a last-write-wins assignment.
//
// skipped_lines=3 across damaged_files=2 is the only combination that
// distinguishes the two.
func TestSkippedLines_ScanAggregatesAcrossDamagedFiles(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	idleBody := synthetic(repo, testBranch, "gpt-5.6-terra", nil)

	sessions := t.TempDir()
	writeRollout(t, sessions, "d1.jsonl", idleBody+garbageLine+garbageLine) // 2 lost
	writeRollout(t, sessions, "d2.jsonl", idleBody+garbageLine)             // 1 lost

	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	if _, err := c.Collect(context.Background(), testSince); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantSummary(t, logs.String(),
		"damaged_files=2",
		"skipped_lines=3", // a sum, not the last file's count
		"damaged_to_empty=2",
		"no_token_count=0",
	)
}

// TestSkippedLines_PartialDamageStillEmitsAndStillCounts covers the case with no
// other signal at all: a log that yields REAL spend and also loses some. It is
// not idle, not failed, and emits events, so every pre-existing counter reports it
// as a clean scan.
func TestSkippedLines_PartialDamageStillEmitsAndStillCounts(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	full := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	})

	sessions := t.TempDir()
	path := writeRollout(t, sessions, "partial.jsonl", full+garbageLine)

	sess, err := parseRollout(path, quietLogger())
	if err != nil {
		t.Fatalf("parseRollout: %v", err)
	}
	if sess == nil {
		t.Fatal("nil session for a partially-damaged log")
	}
	// The surviving spend must still be emitted — the tolerance is the point.
	if len(sess.Calls) == 0 {
		t.Fatal("a partially-damaged log emitted NO calls; malformed-line tolerance is " +
			"deliberate and dropping good spend over one bad line is the failure #526 " +
			"exists to prevent, not the fix")
	}
	if sess.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1 — a log can carry real spend AND lose some; "+
			"that combination has no other signal", sess.SkippedLines)
	}

	// And the scan reports it while still emitting.
	c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
	events, err := c.Collect(context.Background(), testSince)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("Collect emitted no events for a partially-damaged log")
	}
	wantSummary(t, logs.String(),
		"damaged_files=1",
		"skipped_lines=1",
		"damaged_to_empty=0", // partial loss, not total
		"no_token_count=0",   // it emitted; it is not idle
	)
}

// TestSkippedLines_MidFlushTailIsNotCounted extends the #464 rule from the
// warning to the COUNT. A number that grows on every scan of every running
// session is worse than no number, because it looks like data.
//
// The two subtests differ ONLY in the trailing newline, which makes the
// discrimination self-evident rather than spread across files.
func TestSkippedLines_MidFlushTailIsNotCounted(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	full := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	})

	tests := []struct {
		name string
		tail string
		want int
		why  string
	}{
		{
			name: "terminated bad line is damage",
			tail: garbageLine,
			want: 1,
			why:  "a COMPLETE line that cannot decode is real damage",
		},
		{
			name: "unterminated bad line is a writer mid-flush",
			tail: strings.TrimSuffix(garbageLine, "\n"),
			want: 0,
			why: "identical bytes minus the newline: Codex is still writing it, and the " +
				"next scan sees the completed line. Counting it makes every running " +
				"session report permanent loss",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions := t.TempDir()
			path := writeRollout(t, sessions, "live.jsonl", full+tc.tail)
			sess, err := parseRollout(path, quietLogger())
			if err != nil {
				t.Fatalf("parseRollout: %v", err)
			}
			if sess == nil {
				t.Fatal("nil session")
			}
			if sess.SkippedLines != tc.want {
				t.Errorf("SkippedLines = %d, want %d: %s", sess.SkippedLines, tc.want, tc.why)
			}
		})
	}
}

// TestSkippedLines_WhitespaceTailDoesNotCancelRealDamage is the regression for a
// subtler interaction between the two rules above.
//
// lastLineMalformed is reset by every line that DECODES, but a blank or
// whitespace-only line `continue`s before that reset. So a stale flag from an
// EARLIER damaged line was consumed by the mid-flush discount, silently erasing
// real damage. The realistic trigger is CRLF, which the parser explicitly
// contemplates: a writer caught between '\r' and '\n' leaves a final line of
// exactly "\r", which TrimSpace empties.
func TestSkippedLines_WhitespaceTailDoesNotCancelRealDamage(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	full := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	})

	for _, tail := range []struct{ name, bytes string }{
		{"whitespace-only unterminated tail", "   "},
		{"bare CR (writer caught mid-CRLF)", "\r"},
	} {
		t.Run(tail.name, func(t *testing.T) {
			sessions := t.TempDir()
			// A genuinely damaged line, THEN the empty tail.
			path := writeRollout(t, sessions, "crlf.jsonl", full+garbageLine+tail.bytes)
			sess, err := parseRollout(path, quietLogger())
			if err != nil {
				t.Fatalf("parseRollout: %v", err)
			}
			if sess.SkippedLines != 1 {
				t.Errorf("SkippedLines = %d, want 1 — a blank/whitespace tail discounted a "+
					"genuinely damaged line, under-reporting the exact number this field "+
					"exists to produce", sess.SkippedLines)
			}
		})
	}
}

// TestSkippedLines_CleanInputIsNeverCountedAsDamage is the over-count anchor.
//
// Every other test here builds damage on purpose, so all of them would still pass
// if the counter over-reported on ordinary input. This covers the two shapes the
// parser explicitly tolerates — blank lines and CRLF line endings — neither of
// which appears in the captured fixture.
func TestSkippedLines_CleanInputIsNeverCountedAsDamage(t *testing.T) {
	t.Run("captured real-world fixture", func(t *testing.T) {
		sess, err := parseRollout(filepath.Join("testdata", cleanFixture), quietLogger())
		if err != nil {
			t.Fatalf("parseRollout: %v", err)
		}
		if sess == nil {
			t.Fatal("nil session for the clean fixture")
		}
		if len(sess.Calls) == 0 {
			t.Fatal("the clean fixture produced no calls; it is no longer exercising a real parse")
		}
		if sess.SkippedLines != 0 {
			t.Errorf("SkippedLines = %d on the captured clean fixture, want 0", sess.SkippedLines)
		}
	})

	t.Run("blank lines and CRLF are not damage", func(t *testing.T) {
		repo := initGitRepo(t, t.TempDir())
		full := synthetic(repo, testBranch, "gpt-5.6-terra", []tokenUsage{
			usage(1000, 400, 100, 40),
			usage(2500, 1200, 260, 90),
		})
		// A stray blank line, and CRLF endings on every line — both are shapes
		// the parser deliberately absorbs (TrimSpace), not decode failures.
		body := strings.ReplaceAll(full, "\n", "\r\n")
		body = strings.Replace(body, "\r\n", "\r\n\r\n", 1)

		sessions := t.TempDir()
		path := writeRollout(t, sessions, "crlf-clean.jsonl", body)
		sess, err := parseRollout(path, quietLogger())
		if err != nil {
			t.Fatalf("parseRollout: %v", err)
		}
		if len(sess.Calls) == 0 {
			t.Fatal("CRLF input produced no calls; the fixture stopped parsing at all, so a " +
				"zero SkippedLines below would be vacuous")
		}
		if sess.SkippedLines != 0 {
			t.Errorf("SkippedLines = %d on clean CRLF input with a blank line, want 0 — "+
				"counting either shape as damage would report loss on healthy logs",
				sess.SkippedLines)
		}
	})
}
