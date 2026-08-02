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
)

const (
	// webhookPayloadRetentionDays bounds the raw-body audit trail by age:
	// PruneWebhookPayloads deletes rows whose received_at is older than this.
	webhookPayloadRetentionDays = 90

	// webhookPayloadMaxRows caps the raw-body audit trail by count: the oldest
	// rows beyond this many are evicted. Worst case is ~50k x ~10 KB gz = ~500 MB;
	// a typical single-tenant install stays at a few MB.
	webhookPayloadMaxRows = 50000

	// maxWebhookBodyBytes bounds a single decompressed body when reading back a
	// payload, so a corrupt or hostile gzip blob cannot exhaust memory. It is 2x
	// the 1 MB webhook read limit in the handler — comfortably above any real
	// GitHub body while still a hard ceiling.
	maxWebhookBodyBytes = 2 << 20

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
// on the same outcome+type (e.g. head_sha:run_attempt for CI, the revert commit
// SHA for reverts); together (OutcomeID, EventType, SourceRef) is the unique key
// that makes replayed webhook deliveries idempotent. ID and RecordedAt are
// populated on read only.
type QualityEvent struct {
	ID         int64
	OutcomeID  int64
	Developer  string
	IssueID    string
	EventType  string
	SourceRef  string
	EventTS    time.Time
	RecordedAt time.Time
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
// maxWebhookBodyBytes so a corrupt or oversized blob cannot exhaust memory.
func gunzipBytes(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	// +1 so an exactly-at-limit body is distinguishable from an over-limit one.
	out, err := io.ReadAll(io.LimitReader(zr, maxWebhookBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxWebhookBodyBytes {
		return nil, fmt.Errorf("webhook payload exceeds %d-byte decompression limit", maxWebhookBodyBytes)
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
// PK id). Runs synchronously at Open() (store.go, Phase 4).
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
func (d *DB) PruneWebhookPayloads(ctx context.Context) (int64, error) {
	var total int64

	ageRes, err := d.db.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM webhook_payloads
		WHERE received_at < datetime('now', '-%d days')`, webhookPayloadRetentionDays),
	)
	if err != nil {
		return total, fmt.Errorf("prune webhook_payloads by age: %w", err)
	}
	if n, err := ageRes.RowsAffected(); err == nil {
		total += n
	}

	// Keep the newest webhookPayloadMaxRows by id; delete the rest. The subquery
	// selects the survivors so the DELETE removes exactly the oldest overflow.
	capRes, err := d.db.ExecContext(ctx, `
		DELETE FROM webhook_payloads
		WHERE id NOT IN (
			SELECT id FROM webhook_payloads ORDER BY id DESC LIMIT ?
		)`,
		webhookPayloadMaxRows,
	)
	if err != nil {
		return total, fmt.Errorf("prune webhook_payloads by row cap: %w", err)
	}
	if n, err := capRes.RowsAffected(); err == nil {
		total += n
	}
	return total, nil
}

// AppendQualityEvent appends one quality_events row and reports whether it
// landed. event_type is validated against the Go-side allowlist. The insert is
// idempotent on the (outcome_id, event_type, source_ref) unique key: a replayed
// webhook delivery inserts nothing and returns inserted=false, so P2-03 (#134)
// can rely on this for replay-safe, event-derived quality.
func (d *DB) AppendQualityEvent(ctx context.Context, e QualityEvent) (inserted bool, err error) {
	if _, ok := validQualityEventTypes[e.EventType]; !ok {
		return false, fmt.Errorf("AppendQualityEvent: unknown event_type %q", e.EventType)
	}
	// Normalise to UTC so the stored event_ts is self-consistent regardless of
	// the caller's location — makes the UTC contract the #134 consumer relies on
	// self-enforcing rather than a caller convention.
	e.EventTS = e.EventTS.UTC()
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO quality_events
		    (outcome_id, developer, issue_id, event_type, source_ref, event_ts)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (outcome_id, event_type, source_ref) DO NOTHING`,
		e.OutcomeID, e.Developer, e.IssueID, e.EventType, e.SourceRef, e.EventTS,
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
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, outcome_id, developer, issue_id, event_type, source_ref, event_ts, recorded_at
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
			&e.EventType, &e.SourceRef, &e.EventTS, &e.RecordedAt); err != nil {
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
