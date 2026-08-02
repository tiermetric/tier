package collector

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// storeEventFromCollector mirrors the field-for-field copy the watcher performs
// (watcher.go InsertTokenEvent call site) when handing a collector TokenEvent to
// the store. Keeping it here lets the store-backed regression exercise the exact
// Timestamp value the watcher would persist without importing the watcher's
// unexported plumbing.
func storeEventFromCollector(ev TokenEvent) store.TokenEvent {
	return store.TokenEvent{
		Developer:      ev.Developer,
		IssueID:        ev.IssueID,
		Model:          ev.Model,
		InputTok:       ev.InputTok,
		OutputTok:      ev.OutputTok,
		CacheRead:      ev.CacheRead,
		CacheWrite5m:   ev.CacheWrite5m,
		CacheWrite1h:   ev.CacheWrite1h,
		CostMicro:      ev.CostMicro,
		Source:         ev.Source,
		Fidelity:       ev.Fidelity,
		IdempotencyKey: ev.IdempotencyKey,
		Timestamp:      ev.Timestamp,
	}
}

// TestJoinSessionsToCommits_MessageTimestampNormalizedToUTC is the message-derived
// half of the #199 regression. A session message carries a +02:00 offset-bearing
// timestamp (14:00+02:00 == 12:00Z). Every ts emitted through the collector must
// be UTC-normalized before storage, because modernc.org/sqlite compares DATETIME
// strings lexically: an offset-encoded ts windows incorrectly against the UTC
// query bounds fixed in #180.
//
// Pre-fix, joinSessionsToCommits copies m.timestamp verbatim and the emitted
// event carries a +02:00 fixed-zone Location — this test fails on both the
// Location assertion and the preserved-instant/wall-clock check. Post-fix the
// Location is UTC and the instant is unchanged.
func TestJoinSessionsToCommits_MessageTimestampNormalizedToUTC(t *testing.T) {
	plusTwo := time.FixedZone("UTC+2", 2*3600)
	// Same instant, expressed in +02:00: wall clock 14:00, instant 12:00Z.
	msgInstant := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	msgInPlusTwo := msgInstant.In(plusTwo)

	sessions := []sessionSummary{{
		SessionID: "sess-utc",
		GitBranch: "feature/199-utc",
		Model:     "claude-sonnet-4",
		StartTime: msgInPlusTwo,
		EndTime:   msgInPlusTwo,
		Messages: []messageUsage{{
			messageID: "m1", timestamp: msgInPlusTwo,
			model: "claude-sonnet-4", input: 1000, output: 500,
		}},
	}}

	events := joinSessionsToCommits(sessions, nil, "alice", "")
	if len(events) != 1 {
		t.Fatalf("joinSessionsToCommits emitted %d events, want 1", len(events))
	}
	got := events[0].Timestamp
	if loc := got.Location(); loc != time.UTC {
		t.Errorf("emitted ts Location = %v, want UTC (offset-encoded ts mis-windows against UTC bounds — #199)", loc)
	}
	if !got.Equal(msgInstant) {
		t.Errorf("emitted ts instant = %s, want %s (normalization must not shift the instant)",
			got.Format(time.RFC3339Nano), msgInstant.Format(time.RFC3339Nano))
	}
}

// TestJoinSessionsToCommits_OffsetTsWindowsCorrectlyInStore is the store-backed
// straddle for the message-derived path (#199 acceptance criterion). It ingests a
// message whose timestamp is +02:00 offset-bearing (14:00+02:00 == 12:00Z),
// persists the emitted event through the same field copy the watcher uses, then
// windows it against a UTC bound at 13:00Z — one hour AFTER the row instant, so
// the row MUST be excluded.
//
// Pre-fix the stored ts string is "2026-03-01 14:00:00+02:00"; SQLite's lexical
// compare weighs its "14" against the bound's "13" and wrongly INCLUDES the row.
// Post-fix the stored ts is "2026-03-01 12:00:00+00:00" and the row is correctly
// excluded. A control bound one hour BEFORE the instant confirms the row is still
// found when it should be (guards against the fix over-excluding).
func TestJoinSessionsToCommits_OffsetTsWindowsCorrectlyInStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tier-collector-utc.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	plusTwo := time.FixedZone("UTC+2", 2*3600)
	rowInstant := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	rowInPlusTwo := rowInstant.In(plusTwo) // 14:00:00+02:00, same instant

	events := joinSessionsToCommits([]sessionSummary{{
		SessionID: "sess-utc-store",
		GitBranch: "feature/199-utc",
		Model:     "claude-sonnet-4",
		StartTime: rowInPlusTwo,
		EndTime:   rowInPlusTwo,
		Messages: []messageUsage{{
			messageID: "m1", timestamp: rowInPlusTwo,
			model: "claude-sonnet-4", input: 1000, output: 500,
		}},
	}}, nil, "alice", "")
	if len(events) != 1 {
		t.Fatalf("joinSessionsToCommits emitted %d events, want 1", len(events))
	}
	if err := db.InsertTokenEvent(ctx, storeEventFromCollector(events[0])); err != nil {
		t.Fatalf("insert token event: %v", err)
	}

	cases := []struct {
		name         string
		since        time.Time
		wantIncluded bool
	}{
		{
			// Bound one hour AFTER the row instant → exclude. Pre-fix the
			// offset-encoded "14" lexically beats "13" and wrongly includes.
			name:         "utc_bound_after_row_excludes",
			since:        rowInstant.Add(1 * time.Hour), // 13:00Z
			wantIncluded: false,
		},
		{
			// Control: bound one hour BEFORE the row instant → include.
			name:         "utc_bound_before_row_includes",
			since:        rowInstant.Add(-1 * time.Hour), // 11:00Z
			wantIncluded: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			costs, err := db.DeveloperCosts(ctx, tc.since)
			if err != nil {
				t.Fatalf("DeveloperCosts: %v", err)
			}
			if included := len(costs) > 0; included != tc.wantIncluded {
				t.Errorf("DeveloperCosts included=%v, want %v (since %s vs row %s)",
					included, tc.wantIncluded,
					tc.since.Format(time.RFC3339), rowInstant.Format(time.RFC3339))
			}
		})
	}
}

// TestGitLog_CommitTimestampNormalizedToUTC is the git-derived half of the #199
// regression. git author dates are essentially always offset-bearing, and the
// gitLog parser uses a "-0700" layout, so a commit dated with a +02:00 offset
// yields a FixedZone (+02:00) time.Time pre-fix. gitLog must normalize it to UTC
// so any downstream persistence or comparison stays in a single canonical zone.
//
// Pre-fix commit.Timestamp.Location() is a +02:00 fixed zone → the assertion
// fails. Post-fix it is UTC with the instant preserved.
func TestGitLog_CommitTimestampNormalizedToUTC(t *testing.T) {
	dir := t.TempDir()
	initEmptyGitRepo(t, dir)
	// Author date carries an explicit +02:00 offset; instant is 12:00Z.
	commitAtDate(t, dir, "2026-03-01T14:00:00+02:00", "commit with offset date")

	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	commits, err := gitLog(context.Background(), dir, since)
	if err != nil {
		t.Fatalf("gitLog: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("gitLog returned %d commits, want 1", len(commits))
	}
	got := commits[0].Timestamp
	if loc := got.Location(); loc != time.UTC {
		t.Errorf("commit ts Location = %v, want UTC (offset-encoded commit ts mis-windows against UTC bounds — #199)", loc)
	}
	wantInstant := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if !got.Equal(wantInstant) {
		t.Errorf("commit ts instant = %s, want %s (normalization must not shift the instant)",
			got.Format(time.RFC3339), wantInstant.Format(time.RFC3339))
	}
}
