package store

// audit.go holds the append-only audit trail introduced by #137: raw webhook
// payload retention (webhook_payloads) and the two append-only quality logs
// (quality_events, consumed by P2-03/#134; quality_history, written on every
// quality mutation). The tables and indexes are created in schemaTables
// (store.go); this file owns the structs, gzip helpers, and the read/write
// methods. Keeping the three-table method cluster here keeps store.go focused
// on the cost/outcome core.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tiermetric/tier/internal/qualityrule"
)

const (
	// webhookPayloadRetentionDays bounds the raw-body audit trail by age:
	// PruneWebhookPayloads deletes rows whose received_at is older than this.
	webhookPayloadRetentionDays = 90

	// webhookPayloadMaxRows caps the raw-body audit trail by count: the oldest
	// rows beyond this many are evicted. At the 25 MiB request cap, 50k
	// incompressible bodies can occupy ~1.2 TiB before gzip/database overhead;
	// this is a row cap, not a byte cap.
	webhookPayloadMaxRows = 50000

	// MaxWebhookBodyBytes is the push request cap and the decompression ceiling
	// for retained payloads. Sharing it keeps accepted bodies readable while
	// bounding corrupt or hostile gzip blobs. Other events are capped at 1 MiB
	// by the handler. GitHub's separate provider cap is documented as 25 MB.
	MaxWebhookBodyBytes = 25 << 20

	// legacyUpdateQualityReason is the quality_history reason recorded by the
	// issue-wide UpdateQuality path (the pre-P2-03 revert path). P2-03 writes the
	// real event_type instead.
	legacyUpdateQualityReason = "legacy-update-quality"
)

// validQualityEventTypes is the Go-side allowlist for quality_events.event_type.
// The table carries no SQL CHECK (see schemaTables): validating here means a
// Phase-2 event type is a one-line addition, not a SQLite table rebuild. Kept in
// sync with the resolution semantics P2-03 (#134) layers on top.
var validQualityEventTypes = map[string]struct{}{
	"ci_pass":          {},
	"ci_fail":          {},
	"ci_fail_flaky":    {},
	"revert_quality":   {},
	"revert_strategic": {},
	"revert_ambiguous": {},
}

// WebhookPayload is a stored raw webhook delivery. Body holds the decompressed
// original request body; BodySHA256 is the hex digest of that raw body.
type WebhookPayload struct {
	ID         int64
	Event      string
	DeliveryID string
	Body       []byte
	BodySHA256 string
	ReceivedAt time.Time
}

// QualityEvent is one append-only signal against a specific outcome row,
// consumed by P2-03 (#134) to derive quality. SourceRef distinguishes signals
// on the same outcome+type (head_sha:run_attempt:workflow_id for CI — see
// quality.BuildCISourceRef — the revert commit SHA for reverts); together
// (OutcomeID, EventType, SourceRef) is the unique key that makes replayed
// webhook deliveries idempotent. ID and RecordedAt are populated on read only.
//
// ⚠️ The workflow id is part of source_ref and therefore part of that unique
// key ON PURPOSE (#687): without it, two workflows failing on one merge commit
// at the same attempt collide and the second ci_fail is silently dropped. Rows
// written before #687 carry the two-component form and still parse — see
// quality.CIKey for what that means for matching.
type QualityEvent struct {
	TriggerEvent *string // NULL preserves unknown legacy provenance.
	Reason       string  // ambiguous diagnostic reason; empty for legacy/scoring events
	ID           int64
	OutcomeID    int64
	Developer    string
	IssueID      string
	EventType    string
	SourceRef    string
	EventTS      time.Time
	RecordedAt   time.Time
}

// IsEvidence excludes unresolved and unknown event types by default.
func (e QualityEvent) IsEvidence() bool {
	return qualityrule.IsEvidence(e.EventType) && (!qualityrule.IsCI(e.EventType) || (e.TriggerEvent != nil && *e.TriggerEvent == "push"))
}

// Status reports observation status, not the stored numeric quality.
func (e QualityEvent) Status() string {
	if qualityrule.IsCI(e.EventType) && e.TriggerEvent == nil {
		return "CI trigger unknown"
	}
	if e.IsEvidence() {
		return "observed"
	}
	return "unknown"
}

// DistinctAmbiguousReverts counts checks rather than candidate rows (U32).
// Use the same source_ref set across outcomes when measuring a cohort.
func DistinctAmbiguousReverts(events []QualityEvent) int {
	refs := make(map[string]struct{})
	for _, e := range events {
		if e.EventType == qualityrule.EventRevertAmbiguous {
			refs[e.SourceRef] = struct{}{}
		}
	}
	return len(refs)
}

// QualityTransition is one append-only quality_history row: a recorded
// old->new quality change on an outcome, with the driving Reason (an event_type
// or legacyUpdateQualityReason). ID and Timestamp are populated on read only.
type QualityTransition struct {
	ID         int64
	OutcomeID  int64
	Developer  string
	IssueID    string
	OldQuality float64
	NewQuality float64
	Reason     string
	SourceRef  string
	Timestamp  time.Time
}

// gzipBytes compresses b with gzip.BestSpeed. BestSpeed is chosen because the
// audit write sits on the webhook request path — throughput matters more than a
// few extra stored KB.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzipBytes decompresses a gzip blob produced by gzipBytes, bounded to
// MaxWebhookBodyBytes so a corrupt or oversized blob cannot exhaust memory.
func gunzipBytes(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	// +1 so an exactly-at-limit body is distinguishable from an over-limit one.
	out, err := io.ReadAll(io.LimitReader(zr, MaxWebhookBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > MaxWebhookBodyBytes {
		return nil, fmt.Errorf("webhook payload exceeds %d-byte decompression limit", MaxWebhookBodyBytes)
	}
	return out, nil
}

// InsertWebhookPayload gzips rawBody and appends one webhook_payloads row,
// recording the SHA-256 of the RAW (pre-gzip) body for integrity. deliveryID may
// be empty (test clients / non-GitHub callers). Intended to be called
// best-effort from the webhook handler: a failure here must not fail the
// delivery, so the caller logs and continues.
func (d *DB) InsertWebhookPayload(ctx context.Context, event, deliveryID string, rawBody []byte) error {
	gz, err := gzipBytes(rawBody)
	if err != nil {
		return fmt.Errorf("gzip webhook payload: %w", err)
	}
	sum := sha256.Sum256(rawBody)
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256)
		VALUES (?, ?, ?, ?)`,
		event, deliveryID, gz, hex.EncodeToString(sum[:]),
	)
	return err
}

// WebhookPayloadByDelivery returns the most recent stored raw body for
// (event, deliveryID), decompressed, plus a found flag. Redeliveries reuse a
// GUID and legitimately produce multiple rows (the index is non-unique), so this
// returns the newest by id. For ops tooling and tests.
func (d *DB) WebhookPayloadByDelivery(ctx context.Context, event, deliveryID string) ([]byte, bool, error) {
	var gz []byte
	err := d.db.QueryRowContext(ctx, `
		SELECT body_gz FROM webhook_payloads
		WHERE event = ? AND delivery_id = ?
		ORDER BY id DESC
		LIMIT 1`,
		event, deliveryID,
	).Scan(&gz)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw, err := gunzipBytes(gz)
	if err != nil {
		return nil, false, fmt.Errorf("gunzip webhook payload: %w", err)
	}
	return raw, true, nil
}

// PruneWebhookPayloads enforces the two retention bounds and returns the total
// number of rows deleted. Two passes: (1) delete rows older than
// webhookPayloadRetentionDays by received_at; (2) delete the oldest rows beyond
// webhookPayloadMaxRows by id. Both use indexed columns (received_at, and the
// PK id). Runs synchronously at Open() (store.go, Phase 4), then every 24h from
// a running serve (cmd/tierd runWebhookPayloadPruner, #846).
//
// datetime('now', '-N days') is evaluated by SQLite in UTC, matching the UTC
// CURRENT_TIMESTAMP default on received_at, so the age comparison is
// timezone-consistent without any Go-side time formatting.
//
// Evictability / evidence-of-record boundary: webhook_payloads is a best-effort
// re-derivation CONVENIENCE, not the evidence of record. The 50k-row cap is a
// churnable window — an attacker holding a valid webhook secret can flood
// deliveries and evict older raw bodies out of the cap. The non-evictable
// authority is the append-only trail: quality_history and quality_events (never
// pruned here) plus the outcomes rows they explain. A defensible score rests on
// those, not on the presence of any particular raw payload.
//
// 🔴 BOTH PASSES ARE ONE TRANSACTION (#673). The row-cap pass's subquery READS
// the table the age pass just wrote, so as two independent statements it is a
// read-then-write straddling a window another writer can enter: a delivery
// landing between them is visible to the subquery that chooses the survivors but
// not to the age pass that ran before it, and the returned total then describes
// neither state. beginImmediate — the UNBOUNDED helper — because both callers,
// Open() and serve's daily prune loop (#846), are background paths that should
// WAIT for the lock, with no request behind it to fail fast for.
//
// ⚠️ THE ENCLOSING TRANSACTION NOW MATTERS (#846). The prune also runs from the
// serve loop, concurrently with webhook deliveries, so that one transaction is
// what closes the window above there. Dropping it would fail silently: under no
// contention the counts look identical either way. That is why the pin is the
// caller census in deferred_conversion_test.go rather than a row-count assertion.
func (d *DB) PruneWebhookPayloads(ctx context.Context) (int64, error) {
	var total int64

	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return 0, fmt.Errorf("prune webhook_payloads: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op (ErrTxDone) after a successful Commit

	ageRes, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM webhook_payloads
		WHERE received_at < datetime('now', '-%d days')`, webhookPayloadRetentionDays),
	)
	if err != nil {
		return 0, fmt.Errorf("prune webhook_payloads by age: %w", err)
	}
	if n, err := ageRes.RowsAffected(); err == nil {
		total += n
	}

	// Keep the newest webhookPayloadMaxRows by id; delete the rest. The subquery
	// selects the survivors so the DELETE removes exactly the oldest overflow.
	capRes, err := tx.ExecContext(ctx, `
		DELETE FROM webhook_payloads
		WHERE id NOT IN (
			SELECT id FROM webhook_payloads ORDER BY id DESC LIMIT ?
		)`,
		webhookPayloadMaxRows,
	)
	if err != nil {
		return 0, fmt.Errorf("prune webhook_payloads by row cap: %w", err)
	}
	if n, err := capRes.RowsAffected(); err == nil {
		total += n
	}

	// 🔴 THE COUNTS ARE RETURNED ONLY AFTER THE COMMIT SUCCEEDS, and every error
	// path above now returns 0 rather than a partial total. Reporting deletions
	// from a transaction that then rolled back would describe rows that are still
	// in the table — the same both-halves-or-neither property the enclosure
	// provides, carried through to what the caller is told.
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune webhook_payloads: commit: %w", err)
	}
	return total, nil
}

// AppendQualityEvent reports whether an event was inserted or its known non-push
// trigger upgraded to push. NULL legacy provenance is never upgraded by replay.
// The (outcome_id, event_type, source_ref) unique key makes replays idempotent.
// Ambiguous markers also return inserted=false if the outcome no longer belongs
// to the developer: the existence check and insert share one atomic SQL write.
func (d *DB) AppendQualityEvent(ctx context.Context, e QualityEvent) (inserted bool, err error) {
	if _, ok := validQualityEventTypes[e.EventType]; !ok {
		return false, fmt.Errorf("AppendQualityEvent: unknown event_type %q", e.EventType)
	}
	// Normalise to UTC so the stored event_ts is self-consistent regardless of
	// the caller's location — makes the UTC contract the #134 consumer relies on
	// self-enforcing rather than a caller convention. event_ts is an ORDERED
	// column (keyset-exported via keysetWindowSQLFor("event_ts")), so see
	// normalizeTS in store.go for the full reason the encoding must be pinned.
	// Routed through that helper rather than an inline .UTC() so `grep normalizeTS
	// internal/store/` enumerates every ordered-ts writer (#723) — this site was
	// the precedent for the helper and must not be the one it cannot find.
	e.EventTS = normalizeTS(e.EventTS)
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO quality_events
		    (outcome_id, developer, issue_id, event_type, source_ref, event_ts, reason, trigger_event)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE ? <> 'revert_ambiguous' OR EXISTS (
		    SELECT 1 FROM outcomes WHERE id = ? AND developer = ?
		)
		ON CONFLICT (outcome_id, event_type, source_ref) DO UPDATE SET trigger_event='push', event_ts=excluded.event_ts
		WHERE excluded.trigger_event='push' AND trigger_event IS NOT NULL AND trigger_event<>'push'`,
		e.OutcomeID, e.Developer, e.IssueID, e.EventType, e.SourceRef, e.EventTS, e.Reason, e.TriggerEvent,
		e.EventType, e.OutcomeID, e.Developer,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// QualityEventsForOutcome returns all quality_events for an outcome, oldest
// first (by id). Used by P2-03 (#134) resolution and by audit reads.
func (d *DB) QualityEventsForOutcome(ctx context.Context, outcomeID int64) ([]QualityEvent, error) {
	return qualityEventsFrom(ctx, d.db, outcomeID)
}

func qualityEventsFrom(ctx context.Context, q readQuerier, outcomeID int64) ([]QualityEvent, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, outcome_id, developer, issue_id, event_type, source_ref, event_ts, recorded_at, reason, trigger_event
		FROM quality_events
		WHERE outcome_id = ?
		ORDER BY id`,
		outcomeID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []QualityEvent
	for rows.Next() {
		var e QualityEvent
		if err := rows.Scan(&e.ID, &e.OutcomeID, &e.Developer, &e.IssueID,
			&e.EventType, &e.SourceRef, &e.EventTS, &e.RecordedAt, &e.Reason, &e.TriggerEvent); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QualityHistoryForOutcome returns the append-only quality_history rows for an
// outcome, oldest first (by id), so outcome quality can be re-derived from the
// chain (the last new_quality equals outcomes.quality; an empty chain means
// quality is still 1.0).
func (d *DB) QualityHistoryForOutcome(ctx context.Context, outcomeID int64) ([]QualityTransition, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, outcome_id, developer, issue_id, old_quality, new_quality, reason, source_ref, ts
		FROM quality_history
		WHERE outcome_id = ?
		ORDER BY id`,
		outcomeID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []QualityTransition
	for rows.Next() {
		var t QualityTransition
		if err := rows.Scan(&t.ID, &t.OutcomeID, &t.Developer, &t.IssueID,
			&t.OldQuality, &t.NewQuality, &t.Reason, &t.SourceRef, &t.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
