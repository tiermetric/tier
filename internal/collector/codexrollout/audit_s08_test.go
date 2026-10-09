package codexrollout

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// S08-6: future call stamps, including the session fallback, cannot poison Run's cursor.
func TestAuditS08CodexClockHorizon(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "event timestamp"
		if fallback {
			name = "session fallback"
		}
		t.Run(name, func(t *testing.T) {
			repo := initGitRepo(t, t.TempDir())
			sessions := t.TempDir()
			body := syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})
			if fallback {
				body = strings.ReplaceAll(body, `"timestamp":"2026-07-23T01:00:00.000Z",`, "")
			}
			body = strings.ReplaceAll(body, "2026-07-23", "2099-07-23")
			path := writeRollout(t, sessions, "future.jsonl", body)
			c, logs := newLoggingCollector(t, sessions, RepoTarget{Path: repo})
			evs, err := c.Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 0 {
				t.Fatalf("captured %d implausibly future events", len(evs))
			}
			if !strings.Contains(logs.String(), "clock horizon") {
				t.Fatalf("missing refusal warning: %s", logs.String())
			}
			var captured []collector.TokenEvent
			ing := collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error { captured = append(captured, ev); return nil })
			c.runPass(context.Background(), ing)
			if !c.cursor.eventFloor.IsZero() {
				t.Fatalf("future row poisoned cursor: %s", c.cursor.eventFloor)
			}
			// The next normal call, written after the pass, remains capturable.
			body = syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			c.runPass(context.Background(), ing)
			if len(captured) != 1 {
				t.Fatalf("normal row after corrupt stamp: %d events", len(captured))
			}
		})
	}
}

func TestAuditS08CodexAllowsModestClockSkew(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	body := syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})
	body = strings.ReplaceAll(body, "2026-07-23T01:00:00.000Z", time.Now().Add(12*time.Hour).UTC().Format(time.RFC3339Nano))
	writeRollout(t, sessions, "skew.jsonl", body)
	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	evs, err := c.Collect(context.Background(), time.Time{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("legitimate clock skew: %d events, %v", len(evs), err)
	}
}

func TestRunPass_ClockHorizonRefusalRecordsLostAndHoldsCursorOnError(t *testing.T) {
	for _, recordFails := range []bool{false, true} {
		name := "loss recorded"
		if recordFails {
			name = "loss recorder fails"
		}
		t.Run(name, func(t *testing.T) {
			repo := initGitRepo(t, t.TempDir())
			sessions := t.TempDir()
			body := syntheticSpaced(repo, testBranch, "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})
			body = strings.ReplaceAll(body, "2026-07-23T01:00:00.000Z", time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339Nano))
			path := writeRollout(t, sessions, "future.jsonl", body)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			c := newTestCollector(t, sessions, RepoTarget{Path: repo})
			initial := scanWindow{fileFloor: testSince, eventFloor: testSince, gitFloor: testSince}
			c.setCursor(initial)
			settled := settleRecorder(c, testSince)
			var lost [][2]time.Time
			c.lost = func(_ context.Context, from, through time.Time) error {
				lost = append(lost, [2]time.Time{from, through})
				if recordFails {
					return errors.New("loss recorder unavailable")
				}
				return nil
			}
			ing := &countingIngester{}
			before := time.Now().UTC()
			c.runPass(context.Background(), ing)
			if ing.n != 0 {
				t.Errorf("ingested %d events for a refused future call, want none", ing.n)
			}
			// The refused timestamp is untrustworthy; loss covers the session's
			// conservative day-to-mtime span, as for damaged rollout lines.
			day := time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC)
			if len(lost) != 1 || !lost[0][0].Equal(day) || !lost[0][1].Equal(info.ModTime()) {
				t.Errorf("lost %v, want the refused session's span [%s, %s]", lost, day, info.ModTime())
			}
			if recordFails {
				if c.cursor != initial {
					t.Errorf("cursor advanced after loss recording failed: %+v, want %+v", c.cursor, initial)
				}
				if len(*settled) != 0 {
					t.Errorf("settled %v after loss recording failed, want nothing", *settled)
				}
			} else {
				if c.cursor.fileFloor.Before(before) || !c.cursor.eventFloor.Equal(testSince) {
					t.Errorf("cursor after recording loss: %+v, want file floor advanced and event floor held", c.cursor)
				}
				if len(*settled) != 1 {
					t.Errorf("settled %v after recording loss, want once", *settled)
				}
			}
		})
	}
}

func TestAdvanceCursor_ClampsEventFloorToPassStart(t *testing.T) {
	c := newTestCollector(t, t.TempDir(), RepoTarget{Path: t.TempDir()})
	passStart := testSince.Add(time.Hour)
	c.setCursor(scanWindow{eventFloor: testSince})
	c.advanceCursor(passStart, passStart.Add(12*time.Hour))
	if !c.cursor.eventFloor.Equal(passStart) {
		t.Fatalf("event floor = %s, want pass start %s", c.cursor.eventFloor, passStart)
	}
}
