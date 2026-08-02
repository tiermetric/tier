package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// sha256Hex returns the hex-encoded SHA-256 of b, matching what
// InsertWebhookPayload stores in body_sha256.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// outcomeIDFor returns the newest outcomes.id for (developer, issueID). White-box
// helper: the tests are package store, so direct db access is available.
func outcomeIDFor(t *testing.T, db *DB, developer, issueID string) int64 {
	t.Helper()
	var id int64
	if err := db.db.QueryRow(
		`SELECT id FROM outcomes WHERE developer = ? AND issue_id = ? ORDER BY id DESC LIMIT 1`,
		developer, issueID,
	).Scan(&id); err != nil {
		t.Fatalf("outcomeIDFor(%s,%s): %v", developer, issueID, err)
	}
	return id
}

// outcomeQuality reads outcomes.quality for the given id.
func outcomeQuality(t *testing.T, db *DB, id int64) float64 {
	t.Helper()
	var q float64
	if err := db.db.QueryRow(`SELECT quality FROM outcomes WHERE id = ?`, id).Scan(&q); err != nil {
		t.Fatalf("outcomeQuality(%d): %v", id, err)
	}
	return q
}

// TestUpdateQuality_WritesHistoryRow asserts every UpdateQuality — including a
// no-op write (new == old) — appends a quality_history row recording the
// transition, with reason 'legacy-update-quality'. Fails on main (table absent).
func TestUpdateQuality_WritesHistoryRow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-1", PRNumber: 1, Weight: 5.0, Quality: 1.0, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	id := outcomeIDFor(t, db, "alice", "issue-1")

	// 1.0 -> 0.5: a real transition.
	if err := db.UpdateQuality(ctx, "alice", "issue-1", 0.5); err != nil {
		t.Fatalf("UpdateQuality 0.5: %v", err)
	}
	hist, err := db.QualityHistoryForOutcome(ctx, id)
	if err != nil {
		t.Fatalf("QualityHistoryForOutcome: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("after first update: got %d history rows, want 1", len(hist))
	}
	h0 := hist[0]
	if h0.OldQuality != 1.0 || h0.NewQuality != 0.5 {
		t.Errorf("history[0] old/new = %v/%v, want 1.0/0.5", h0.OldQuality, h0.NewQuality)
	}
	if h0.Reason != legacyUpdateQualityReason {
		t.Errorf("history[0] reason = %q, want %q", h0.Reason, legacyUpdateQualityReason)
	}
	if h0.OutcomeID != id || h0.Developer != "alice" || h0.IssueID != "issue-1" {
		t.Errorf("history[0] identity = (%d,%s,%s), want (%d,alice,issue-1)", h0.OutcomeID, h0.Developer, h0.IssueID, id)
	}

	// 0.5 -> 0.5: a no-op write STILL appends an audit row (a write occurred).
	if err := db.UpdateQuality(ctx, "alice", "issue-1", 0.5); err != nil {
		t.Fatalf("UpdateQuality no-op: %v", err)
	}
	hist, err = db.QualityHistoryForOutcome(ctx, id)
	if err != nil {
		t.Fatalf("QualityHistoryForOutcome (2): %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("after no-op update: got %d history rows, want 2", len(hist))
	}
	if hist[1].OldQuality != 0.5 || hist[1].NewQuality != 0.5 {
		t.Errorf("history[1] old/new = %v/%v, want 0.5/0.5", hist[1].OldQuality, hist[1].NewQuality)
	}
}

// TestQualityHistory_RederivesOutcomeQuality asserts the definition of done:
// after a chain of writes, outcomes.quality equals the last new_quality in the
// history chain.
func TestQualityHistory_RederivesOutcomeQuality(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-2", PRNumber: 2, Weight: 3.0, Quality: 1.0, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	id := outcomeIDFor(t, db, "bob", "issue-2")

	for _, q := range []float64{0.7, 0.5, 0.1} {
		if err := db.UpdateQuality(ctx, "bob", "issue-2", q); err != nil {
			t.Fatalf("UpdateQuality %v: %v", q, err)
		}
	}

	hist, err := db.QualityHistoryForOutcome(ctx, id)
	if err != nil {
		t.Fatalf("QualityHistoryForOutcome: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("got %d history rows, want 3", len(hist))
	}
	last := hist[len(hist)-1].NewQuality
	if got := outcomeQuality(t, db, id); got != last {
		t.Errorf("outcomes.quality = %v, want last(new_quality) = %v", got, last)
	}
	if last != 0.1 {
		t.Errorf("last new_quality = %v, want 0.1", last)
	}
}

// TestUpdateQualityForOutcome_TargetsRowByID asserts the §C9 scoping fix (#134):
// UpdateQualityForOutcome degrades ONLY the addressed outcome row, leaving a
// sibling outcome on the same issue untouched — where the issue-wide
// UpdateQuality would hit both. It also records the event_type reason and
// source_ref in quality_history.
func TestUpdateQualityForOutcome_TargetsRowByID(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Two PRs (two outcomes) on ONE issue.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-9", PRNumber: 1, Weight: 3, Quality: 1.0,
		MergeCommitSHA: "1111111111111111111111111111111111111111", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome 1: %v", err)
	}
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-9", PRNumber: 2, Weight: 5, Quality: 1.0,
		MergeCommitSHA: "2222222222222222222222222222222222222222", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome 2: %v", err)
	}

	o1, ok, err := db.OutcomeByMergeCommit(ctx, "1111111111111111111111111111111111111111")
	if err != nil || !ok {
		t.Fatalf("OutcomeByMergeCommit sha1: ok=%v err=%v", ok, err)
	}
	o2, _, _ := db.OutcomeByMergeCommit(ctx, "2222222222222222222222222222222222222222")

	if err := db.UpdateQualityForOutcome(ctx, o1.ID, 0.1, "revert_quality", "revsha"); err != nil {
		t.Fatalf("UpdateQualityForOutcome: %v", err)
	}

	if q := outcomeQuality(t, db, o1.ID); q != 0.1 {
		t.Errorf("target outcome quality = %v, want 0.1", q)
	}
	if q := outcomeQuality(t, db, o2.ID); q != 1.0 {
		t.Errorf("sibling outcome quality = %v, want 1.0 (must not be degraded — C9)", q)
	}

	hist, err := db.QualityHistoryForOutcome(ctx, o1.ID)
	if err != nil {
		t.Fatalf("QualityHistoryForOutcome: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history rows = %d, want 1", len(hist))
	}
	if hist[0].OldQuality != 1.0 || hist[0].NewQuality != 0.1 {
		t.Errorf("history old/new = %v/%v, want 1.0/0.1", hist[0].OldQuality, hist[0].NewQuality)
	}
	if hist[0].Reason != "revert_quality" || hist[0].SourceRef != "revsha" {
		t.Errorf("history reason/source = %q/%q, want revert_quality/revsha", hist[0].Reason, hist[0].SourceRef)
	}

	// A missing id is an error, not a silent no-op.
	if err := db.UpdateQualityForOutcome(ctx, 999999, 0.5, "ci_fail", "x"); err == nil {
		t.Error("UpdateQualityForOutcome on missing id: got nil error, want failure")
	}
}

// TestUpdateQualityForOutcome_NoOpSuppressesHistory asserts the event-path
// semantics (#134): a write whose target equals the current quality writes NO
// quality_history row (unlike the issue-wide UpdateQuality, which logs no-ops).
// This is what keeps clean CI passes and replayed deliveries from spamming the
// transition log; a genuinely-different value still writes and self-heals.
func TestUpdateQualityForOutcome_NoOpSuppressesHistory(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "dave", IssueID: "issue-13", PRNumber: 4, Weight: 3, Quality: 1.0,
		MergeCommitSHA: "4444444444444444444444444444444444444444", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	id := outcomeIDFor(t, db, "dave", "issue-13")

	// No-op: target equals current (1.0). No history row.
	if err := db.UpdateQualityForOutcome(ctx, id, 1.0, "ci_pass", "sha:1"); err != nil {
		t.Fatalf("UpdateQualityForOutcome no-op: %v", err)
	}
	if hist, _ := db.QualityHistoryForOutcome(ctx, id); len(hist) != 0 {
		t.Fatalf("no-op write appended %d history rows, want 0", len(hist))
	}

	// Real transition: writes and records.
	if err := db.UpdateQualityForOutcome(ctx, id, 0.7, "ci_fail", "sha:1"); err != nil {
		t.Fatalf("UpdateQualityForOutcome transition: %v", err)
	}
	hist, _ := db.QualityHistoryForOutcome(ctx, id)
	if len(hist) != 1 || hist[0].NewQuality != 0.7 || hist[0].Reason != "ci_fail" {
		t.Fatalf("after transition: hist=%+v, want 1 row 1.0->0.7 reason ci_fail", hist)
	}
	if q := outcomeQuality(t, db, id); q != 0.7 {
		t.Errorf("quality = %v, want 0.7", q)
	}

	// Repeated same-value write (idempotent replay) is again a no-op.
	if err := db.UpdateQualityForOutcome(ctx, id, 0.7, "ci_fail", "sha:1"); err != nil {
		t.Fatalf("UpdateQualityForOutcome repeat: %v", err)
	}
	if hist, _ := db.QualityHistoryForOutcome(ctx, id); len(hist) != 1 {
		t.Errorf("repeat same-value write changed history to %d rows, want 1", len(hist))
	}
}

// TestOutcomeReads_SurfaceRowID asserts OutcomeByMergeCommit and
// LatestOutcomeByIssue populate Outcome.ID, which the event-derived quality path
// needs to address rows (#134).
func TestOutcomeReads_SurfaceRowID(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "carol", IssueID: "issue-11", PRNumber: 3, Weight: 3, Quality: 1.0,
		MergeCommitSHA: "3333333333333333333333333333333333333333", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	want := outcomeIDFor(t, db, "carol", "issue-11")

	byMerge, ok, err := db.OutcomeByMergeCommit(ctx, "3333333333333333333333333333333333333333")
	if err != nil || !ok {
		t.Fatalf("OutcomeByMergeCommit: ok=%v err=%v", ok, err)
	}
	if byMerge.ID != want {
		t.Errorf("OutcomeByMergeCommit ID = %d, want %d", byMerge.ID, want)
	}

	byIssue, ok, err := db.LatestOutcomeByIssue(ctx, "", "issue-11")
	if err != nil || !ok {
		t.Fatalf("LatestOutcomeByIssue: ok=%v err=%v", ok, err)
	}
	if byIssue.ID != want {
		t.Errorf("LatestOutcomeByIssue ID = %d, want %d", byIssue.ID, want)
	}
}

// TestInsertWebhookPayload_RoundTripsGzip inserts a large JSON body and reads it
// back byte-identical, with the stored SHA matching the raw body.
func TestInsertWebhookPayload_RoundTripsGzip(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// ~100 KB of realistic-ish JSON.
	var b bytes.Buffer
	b.WriteString(`{"items":[`)
	for i := 0; i < 4000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"n":%d,"s":"payload-%d"}`, i, i)
	}
	b.WriteString(`]}`)
	raw := b.Bytes()
	if len(raw) < 90_000 {
		t.Fatalf("test body too small: %d bytes", len(raw))
	}

	if err := db.InsertWebhookPayload(ctx, "pull_request", "deliv-1", raw); err != nil {
		t.Fatalf("InsertWebhookPayload: %v", err)
	}

	got, found, err := db.WebhookPayloadByDelivery(ctx, "pull_request", "deliv-1")
	if err != nil {
		t.Fatalf("WebhookPayloadByDelivery: %v", err)
	}
	if !found {
		t.Fatal("payload not found")
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("round-trip body mismatch: got %d bytes, want %d", len(got), len(raw))
	}

	// The stored gz blob must be materially smaller than the raw body, proving
	// compression actually happened.
	var gzLen, rawLen int
	if err := db.db.QueryRowContext(ctx,
		`SELECT length(body_gz) FROM webhook_payloads WHERE delivery_id = ?`, "deliv-1",
	).Scan(&gzLen); err != nil {
		t.Fatalf("length(body_gz): %v", err)
	}
	rawLen = len(raw)
	if gzLen >= rawLen {
		t.Errorf("gz length %d not smaller than raw %d", gzLen, rawLen)
	}

	// The stored SHA must be the digest of the RAW body.
	wantSHA := sha256Hex(raw)
	var gotSHA string
	if err := db.db.QueryRowContext(ctx,
		`SELECT body_sha256 FROM webhook_payloads WHERE delivery_id = ?`, "deliv-1",
	).Scan(&gotSHA); err != nil {
		t.Fatalf("body_sha256: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("stored sha = %s, want %s", gotSHA, wantSHA)
	}

	// Missing delivery → found=false, no error.
	if _, found, err := db.WebhookPayloadByDelivery(ctx, "pull_request", "nope"); err != nil || found {
		t.Errorf("missing delivery: found=%v err=%v, want false/nil", found, err)
	}
}

// TestPruneWebhookPayloads_AgeAndCap covers both retention bounds: the 90-day
// age prune and the 50k-row cap eviction of the oldest rows.
func TestPruneWebhookPayloads_AgeAndCap(t *testing.T) {
	t.Run("age", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		ctx := context.Background()

		// One fresh row (via the real path) and one 91-day-old row (direct SQL,
		// since InsertWebhookPayload always stamps received_at = now).
		if err := db.InsertWebhookPayload(ctx, "push", "fresh", []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("InsertWebhookPayload fresh: %v", err)
		}
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256, received_at)
			VALUES ('push', 'stale', x'00', 'deadbeef', datetime('now','-91 days'))`); err != nil {
			t.Fatalf("insert stale row: %v", err)
		}

		deleted, err := db.PruneWebhookPayloads(ctx)
		if err != nil {
			t.Fatalf("PruneWebhookPayloads: %v", err)
		}
		if deleted != 1 {
			t.Errorf("age prune deleted %d, want 1", deleted)
		}
		if got := webhookPayloadCount(t, db); got != 1 {
			t.Errorf("rows after age prune = %d, want 1", got)
		}
		if _, found, _ := db.WebhookPayloadByDelivery(ctx, "push", "stale"); found {
			t.Error("stale row survived age prune")
		}
		if _, found, _ := db.WebhookPayloadByDelivery(ctx, "push", "fresh"); !found {
			t.Error("fresh row was wrongly pruned")
		}
	})

	t.Run("cap", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		ctx := context.Background()

		// Insert maxRows+1 rows directly (bypass gzip for speed); all stamped
		// "now" so the age prune deletes nothing and only the cap fires.
		const n = webhookPayloadMaxRows + 1
		tx, err := db.db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		stmt, err := tx.Prepare(`INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256) VALUES ('push', ?, x'00', 'x')`)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		for i := 0; i < n; i++ {
			if _, err := stmt.Exec(fmt.Sprintf("d-%d", i)); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}

		deleted, err := db.PruneWebhookPayloads(ctx)
		if err != nil {
			t.Fatalf("PruneWebhookPayloads: %v", err)
		}
		if deleted != 1 {
			t.Errorf("cap prune deleted %d, want 1 (the single overflow row)", deleted)
		}
		if got := webhookPayloadCount(t, db); got != webhookPayloadMaxRows {
			t.Errorf("rows after cap prune = %d, want %d", got, webhookPayloadMaxRows)
		}
		// The oldest row (id 1) must be the one evicted.
		var minID int64
		if err := db.db.QueryRow(`SELECT MIN(id) FROM webhook_payloads`).Scan(&minID); err != nil {
			t.Fatalf("MIN(id): %v", err)
		}
		if minID != 2 {
			t.Errorf("oldest surviving id = %d, want 2 (id 1 evicted)", minID)
		}
	})
}

// TestAppendQualityEvent_IdempotentOnConflict asserts the (outcome_id,
// event_type, source_ref) unique key makes a replayed event a no-op that reports
// inserted=false, leaving exactly one row.
func TestAppendQualityEvent_IdempotentOnConflict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	e := QualityEvent{
		OutcomeID: 42, Developer: "alice", IssueID: "issue-9",
		EventType: "ci_fail", SourceRef: "abc123:1", EventTS: now,
	}

	inserted, err := db.AppendQualityEvent(ctx, e)
	if err != nil {
		t.Fatalf("AppendQualityEvent 1: %v", err)
	}
	if !inserted {
		t.Error("first append: inserted=false, want true")
	}

	inserted, err = db.AppendQualityEvent(ctx, e)
	if err != nil {
		t.Fatalf("AppendQualityEvent 2: %v", err)
	}
	if inserted {
		t.Error("replayed append: inserted=true, want false")
	}

	events, err := db.QualityEventsForOutcome(ctx, 42)
	if err != nil {
		t.Fatalf("QualityEventsForOutcome: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].EventType != "ci_fail" || events[0].SourceRef != "abc123:1" {
		t.Errorf("event = %+v, want ci_fail/abc123:1", events[0])
	}

	// A different source_ref for the same outcome+type IS a distinct signal.
	e2 := e
	e2.SourceRef = "abc123:2"
	if inserted, err := db.AppendQualityEvent(ctx, e2); err != nil || !inserted {
		t.Errorf("distinct source_ref: inserted=%v err=%v, want true/nil", inserted, err)
	}
	if events, _ := db.QualityEventsForOutcome(ctx, 42); len(events) != 2 {
		t.Errorf("after distinct source_ref: %d events, want 2", len(events))
	}
}

// TestAppendQualityEvent_RejectsUnknownType asserts the Go-side allowlist
// rejects an event_type outside the Phase-1 set and writes nothing.
func TestAppendQualityEvent_RejectsUnknownType(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	inserted, err := db.AppendQualityEvent(ctx, QualityEvent{
		OutcomeID: 1, Developer: "alice", IssueID: "issue-1",
		EventType: "definitely_not_a_type", SourceRef: "x", EventTS: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected error for unknown event_type, got nil")
	}
	if inserted {
		t.Error("inserted=true for rejected event, want false")
	}
	if got := qualityEventCount(t, db); got != 0 {
		t.Errorf("rows after rejected append = %d, want 0", got)
	}
}

// --- small white-box helpers ---

func webhookPayloadCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM webhook_payloads`).Scan(&n); err != nil {
		t.Fatalf("count webhook_payloads: %v", err)
	}
	return n
}

func qualityEventCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM quality_events`).Scan(&n); err != nil {
		t.Fatalf("count quality_events: %v", err)
	}
	return n
}
