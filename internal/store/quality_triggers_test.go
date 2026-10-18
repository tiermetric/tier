package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQualityTriggerMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
	if _, err = db.AppendQualityEvent(context.Background(), QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: "sha:1:42", EventTS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.db.Exec(`ALTER TABLE quality_events DROP COLUMN trigger_event; DELETE FROM tier_migrations WHERE name='quality-trigger-recording-v1'; PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := db.UpgradeNotice(), UpgradeNoticeQualityTriggers+"\n"+UpgradeNoticeIssueRule; got != want {
		t.Fatalf("upgrade notice = %q, want %q", got, want)
	}
	var stamp time.Time
	if err = db.db.QueryRow(`SELECT applied_at FROM tier_migrations WHERE name=?`, QualityTriggerCutover).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	var trigger sql.NullString
	if err = db.db.QueryRow(`SELECT trigger_event FROM quality_events`).Scan(&trigger); err != nil || trigger.Valid {
		t.Fatalf("%+v %v", trigger, err)
	}
	var uq string
	if err = db.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='idx_quality_events_uq'`).Scan(&uq); err != nil || strings.Contains(uq, "trigger_event") {
		t.Fatalf("%s %v", uq, err)
	}
	_ = db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if db.UpgradeNotice() != "" {
		t.Fatal("reopen repeated upgrade notice", db.UpgradeNotice())
	}
	var stamp2 time.Time
	if err = db.db.QueryRow(`SELECT applied_at FROM tier_migrations WHERE name=?`, QualityTriggerCutover).Scan(&stamp2); err != nil || !stamp.Equal(stamp2) {
		t.Fatalf("cutover moved %s %s %v", stamp, stamp2, err)
	}
	if readUserVersion(t, path) != schemaVersion || qualityTriggerSchemaVersion != 11 || schemaVersion != 12 {
		t.Fatal("schema version not stamped")
	}
}

func TestResolveCITriggerUnsealedOnlyAndAudit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ids := []int64{seedOutcomeForQuality(t, db, "alice", "issue-1", "sha-a"), seedOutcomeForQuality(t, db, "bob", "issue-2", "sha-b")}
	ts := []time.Time{time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)}
	for i, id := range ids {
		if _, err := db.db.Exec(`UPDATE outcomes SET repo='acme/app',ts=?,quality=0.7 WHERE id=?`, normalizeTS(ts[i]), id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: []string{"alice", "bob"}[i], IssueID: "issue", EventType: "ci_fail", SourceRef: []string{"sha-a:1:42", "sha-b:1:42"}[i], EventTS: ts[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`INSERT INTO sealed_report(level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-02-01T00:00:00Z','2026-03-01T00:00:00Z',2,'x','2026-03-10T00:00:00Z','{}','x','dev','x')`); err != nil {
		t.Fatal(err)
	}
	candidates, err := db.LegacyCIEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range candidates {
		applied, _, err := db.ApplyCITrigger(ctx, c, "schedule", "operator", "digest", "api")
		if err != nil || applied != (i == 0) {
			t.Fatalf("%v %v", applied, err)
		}
	}
	if outcomeQuality(t, db, ids[0]) != 1 || outcomeQuality(t, db, ids[1]) != 0.7 {
		t.Fatal("quality/seal guard")
	}
	events, err := db.QualityEventsForOutcome(ctx, ids[1])
	if err != nil || events[0].TriggerEvent != nil {
		t.Fatalf("sealed provenance changed: %+v %v", events, err)
	}
	hist, err := db.QualityHistoryForOutcome(ctx, ids[0])
	if err != nil || len(hist) != 1 || !strings.Contains(hist[0].Reason, "operator") || !strings.Contains(hist[0].Reason, "digest") {
		t.Fatalf("%+v %v", hist, err)
	}
	applied, _, err := db.ApplyCITrigger(ctx, candidates[0], "schedule", "operator", "digest", "api")
	if err == nil || !strings.Contains(err.Error(), "provenance changed") || applied {
		t.Fatalf("stale API replay %v %v", applied, err)
	}
}

func TestRecoveredPushEvidenceAndCurrentOutcomeJoin(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
	if _, err := db.db.Exec(`UPDATE outcomes SET repo='acme/app',quality=0.7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: "sha:1:42", EventTS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before, err := db.QualityEvidenceWindow(ctx, time.Time{}, time.Time{}, RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	if before.Outcomes[id].CI || before.Outcomes[id].Unknown != 1 {
		t.Fatal(before)
	}
	rows, err := db.LegacyCIEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplyCITrigger(ctx, rows[0], "push", "operator", "digest", "api"); err != nil {
		t.Fatal(err)
	}
	after, err := db.QualityEvidenceWindow(ctx, time.Time{}, time.Time{}, RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	v := after.Outcomes[id]
	if !v.CI || v.Unknown != 0 || v.RecoveredRows != 1 || v.RecordedRows != 0 || !v.Workflows["42"] || outcomeQuality(t, db, id) != 0.7 {
		t.Fatal(after)
	}
	if _, err := db.db.Exec(`DELETE FROM outcomes WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	after, err = db.QualityEvidenceWindow(ctx, time.Time{}, time.Time{}, RepoScope(""))
	if err != nil || len(after.Outcomes) != 0 {
		t.Fatalf("orphan contributes: %+v %v", after, err)
	}
}

func TestResolvedTriggerCannotBeOverwrittenByStaleWebhook(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
	if _, err := db.db.Exec(`UPDATE outcomes SET repo='acme/app',quality=0.7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: "sha:1:42", EventTS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.LegacyCIEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplyCITrigger(ctx, rows[0], "schedule", "operator", "digest", "api"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQualityForOutcome(ctx, id, 0.7, "ci_fail", "sha:1:42"); err != nil {
		t.Fatal(err)
	}
	if q := outcomeQuality(t, db, id); q != 1 {
		t.Fatalf("stale webhook restored resolved non-push penalty: %v", q)
	}
}

func TestCorrectionDiagnosticEvidenceUsesResolvedTrigger(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
	schedule := "schedule"
	if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: "sha:1:42", TriggerEvent: &schedule, EventTS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d, err := (reader{db.db}).CorrectionDiagnosticsWindow(ctx, time.Time{}, time.Time{}, time.Now(), RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	if d.CIOrRevertEvidence {
		t.Fatal("scheduled CI claimed as correction-diagnostic evidence")
	}
}

func TestPushCollisionDoesNotUpgradeLegacyNull(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
	e := QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: "sha:1:42", EventTS: time.Now()}
	if _, err := db.AppendQualityEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	push := "push"
	e.TriggerEvent = &push
	if changed, err := db.AppendQualityEvent(ctx, e); err != nil || changed {
		t.Fatalf("NULL upgraded: changed=%v err=%v", changed, err)
	}
	events, err := db.QualityEventsForOutcome(ctx, id)
	if err != nil || len(events) != 1 || events[0].TriggerEvent != nil {
		t.Fatalf("%+v %v", events, err)
	}
}

func TestResolveCITriggerPreservesNonEventQuality(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
	if _, err := db.db.Exec(`UPDATE outcomes SET repo='acme/app' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: "sha:1:42", EventTS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQuality(ctx, "alice", "issue-1", 0.1); err != nil {
		t.Fatal(err)
	}
	rows, err := db.LegacyCIEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	applied, preserved, err := db.ApplyCITrigger(ctx, rows[0], "schedule", "operator", "digest", "api")
	if err != nil || !applied || !preserved {
		t.Fatalf("applied=%v preserved=%v err=%v", applied, preserved, err)
	}
	if q := outcomeQuality(t, db, id); q != 0.1 {
		t.Fatalf("non-event quality lost: %v", q)
	}
	hist, err := db.QualityHistoryForOutcome(ctx, id)
	if err != nil || len(hist) != 2 || hist[1].OldQuality != 0.1 || hist[1].NewQuality != 0.1 || !strings.Contains(hist[1].Reason, `"EvidenceSource":"api"`) {
		t.Fatalf("%+v %v", hist, err)
	}
}

func TestResolveCITriggerQualityHistoryProvenance(t *testing.T) {
	for _, mode := range []string{"stale event-derived", "operator equals event resolution", "operator after no-op recovery"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			id := seedOutcomeForQuality(t, db, "alice", "issue-1", "sha")
			if _, err := db.db.Exec(`UPDATE outcomes SET repo='acme/app' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			for _, ref := range []string{"sha:1:42", "sha:1:43"} {
				if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "ci_fail", SourceRef: ref, EventTS: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			want, wantPreserved := 0.7, true
			if mode == "stale event-derived" {
				if err := db.UpdateQualityForOutcome(ctx, id, 0.7, "ci_fail", "sha:1:42"); err != nil {
					t.Fatal(err)
				}
				if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-1", EventType: "revert_quality", SourceRef: "revert", EventTS: time.Now()}); err != nil {
					t.Fatal(err)
				}
				want, wantPreserved = 0.1, false
			} else if err := db.UpdateQuality(ctx, "alice", "issue-1", 0.7); err != nil {
				t.Fatal(err)
			}
			rows, err := db.LegacyCIEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "operator after no-op recovery" {
				if _, preserved, err := db.ApplyCITrigger(ctx, rows[1], "schedule", "operator", "digest", "api"); err != nil || !preserved {
					t.Fatalf("first recovery preserved=%v err=%v", preserved, err)
				}
			}
			applied, preserved, err := db.ApplyCITrigger(ctx, rows[0], "schedule", "operator", "digest", "api")
			if err != nil || !applied || preserved != wantPreserved {
				t.Fatalf("applied=%v preserved=%v err=%v", applied, preserved, err)
			}
			if q := outcomeQuality(t, db, id); q != want {
				t.Fatalf("quality=%v, want %v", q, want)
			}
		})
	}
}
