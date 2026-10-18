package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/qualityrule"
	"github.com/tiermetric/tier/internal/repoid"
)

const qualityTriggerSchemaVersion = 11

var UpgradeNoticeQualityTriggers = fmt.Sprintf("tierd: this database was upgraded to schema version %d (push-only CI quality and trigger provenance, #1230). ", qualityTriggerSchemaVersion) + upgradeOneWayNotice

const QualityTriggerCutover = "quality-trigger-recording-v1"

// The marker records installation time, not the occurrence date of old events.
func migrateQualityTriggers(db *sql.DB) error {
	if err := addColumnIfMissing(db, "quality_events", "trigger_event", "TEXT"); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO tier_migrations(name) VALUES (?)`, QualityTriggerCutover)
	return err
}

func NormalizeCITrigger(event string) string { return qualityrule.NormalizeTrigger(event) }

type LegacyCIEvent struct {
	MergeSHA  string `json:"merge_sha"`
	ID        int64  `json:"id"`
	OutcomeID int64  `json:"outcome_id"`
	Repo      string `json:"repo"`
	SourceRef string `json:"source_ref"`
	EventType string `json:"event_type"`
}

func (d *DB) LegacyCIEvents(ctx context.Context) ([]LegacyCIEvent, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT q.id,q.outcome_id,COALESCE(o.repo,''),q.source_ref,q.event_type,COALESCE(o.merge_commit_sha,'') FROM quality_events q JOIN outcomes o ON o.id=q.outcome_id WHERE q.trigger_event IS NULL AND q.event_type IN ('ci_pass','ci_fail','ci_fail_flaky') ORDER BY q.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LegacyCIEvent{}
	for rows.Next() {
		var e LegacyCIEvent
		if err := rows.Scan(&e.ID, &e.OutcomeID, &e.Repo, &e.SourceRef, &e.EventType, &e.MergeSHA); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type RetainedCIRun struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Attempt    int    `json:"run_attempt"`
	WorkflowID int64  `json:"workflow_id"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
}

type RetainedCIEnvelope struct {
	Action     string        `json:"action"`
	Run        RetainedCIRun `json:"workflow_run"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

func (d *DB) RetainedCIRuns(ctx context.Context) ([]RetainedCIEnvelope, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT body_gz FROM webhook_payloads WHERE event='workflow_run' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RetainedCIEnvelope
	for rows.Next() {
		var gz []byte
		if err := rows.Scan(&gz); err != nil {
			return nil, err
		}
		raw, err := gunzipBytes(gz)
		if err != nil {
			continue
		}
		var p RetainedCIEnvelope
		if json.Unmarshal(raw, &p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// ApplyCITrigger checks the approved identity and seal under the same write lock.
// Every provenance fill has an audit row, including fills that leave quality equal.
// qualityPreserved identifies non-push fills with a recorded non-event quality write.
func (d *DB) ApplyCITrigger(ctx context.Context, expected LegacyCIEvent, trigger, actor, digest, source string) (applied, qualityPreserved bool, err error) {
	ref := qualityrule.ParseCIRef(expected.SourceRef)
	if _, ok := repoid.Canonical(expected.Repo); !ok || ref.WorkflowID == "" || ref.RunAttempt <= 0 || ref.HeadSHA != expected.MergeSHA || !qualityrule.IsCI(expected.EventType) {
		return false, false, errors.New("trigger identity lacks repository, workflow or attempt")
	}
	if (source != "payload" && source != "api") || trigger != NormalizeCITrigger(trigger) || trigger == "" || strings.TrimSpace(actor) == "" || digest == "" {
		return false, false, errors.New("invalid trigger approval")
	}
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.prepareCorrectionWriteTx(ctx, tx); err != nil {
		return false, false, err
	}
	var dev, issue string
	var ts time.Time
	var old float64
	var current *string
	err = tx.QueryRowContext(ctx, `SELECT o.developer,o.issue_id,o.ts,o.quality,q.trigger_event FROM quality_events q JOIN outcomes o ON o.id=q.outcome_id WHERE q.id=? AND q.outcome_id=? AND COALESCE(o.repo,'')=? AND q.source_ref=? AND q.event_type=? AND COALESCE(o.merge_commit_sha,'')=?`, expected.ID, expected.OutcomeID, expected.Repo, expected.SourceRef, expected.EventType, expected.MergeSHA).Scan(&dev, &issue, &ts, &old, &current)
	if err != nil {
		return false, false, fmt.Errorf("stale trigger plan: %w", err)
	}
	if current != nil {
		if source == "payload" && *current == trigger {
			return false, false, nil
		}
		return false, false, errors.New("stale trigger plan: provenance changed")
	}
	var sealed bool
	// Sealed periods protect the full UTC calendar month of each outcome.
	start := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sealed_report WHERE period_start<? AND period_end>?)`, start.AddDate(0, 1, 0).Format(time.RFC3339), start.Format(time.RFC3339)).Scan(&sealed); err != nil {
		return false, false, err
	}
	if sealed {
		return false, false, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE quality_events SET trigger_event=? WHERE id=? AND trigger_event IS NULL`, trigger, expected.ID); err != nil {
		return false, false, err
	}
	events, err := qualityEventsFrom(ctx, tx, expected.OutcomeID)
	if err != nil {
		return false, false, err
	}
	q := old
	var lastReason string
	var lastQuality float64
	err = tx.QueryRowContext(ctx, `SELECT reason,new_quality FROM quality_history WHERE outcome_id=? AND NOT (reason LIKE 'resolve-triggers:%' AND old_quality=new_quality) ORDER BY id DESC LIMIT 1`, expected.OutcomeID).Scan(&lastReason, &lastQuality)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	_, eventDerived := validQualityEventTypes[lastReason]
	qualityPreserved = trigger != "push" && err == nil && lastQuality == old && !eventDerived && !strings.HasPrefix(lastReason, "resolve-triggers:")
	if trigger != "push" && !qualityPreserved {
		q = resolveQualityEvents(events)
	}
	if q != old {
		if _, err = tx.ExecContext(ctx, `UPDATE outcomes SET quality=? WHERE id=?`, q, expected.OutcomeID); err != nil {
			return false, false, err
		}
	}
	approval, _ := json.Marshal(struct{ Actor, Digest, EvidenceSource string }{actor, digest, source})
	if _, err = tx.ExecContext(ctx, `INSERT INTO quality_history(outcome_id,developer,issue_id,old_quality,new_quality,reason,source_ref) VALUES (?,?,?,?,?,?,?)`, expected.OutcomeID, dev, issue, old, q, "resolve-triggers:"+string(approval), "quality-event:"+strconv.FormatInt(expected.ID, 10)); err != nil {
		return false, false, err
	}
	if err = d.refreshEnabledCorrectionsTx(ctx, tx, expected.OutcomeID); err != nil {
		return false, false, err
	}
	return true, qualityPreserved, tx.Commit()
}

func resolveQualityEvents(events []QualityEvent) float64 {
	facts := make([]qualityrule.Event, 0, len(events))
	for _, e := range events {
		ref := qualityrule.ParseCIRef(e.SourceRef)
		facts = append(facts, qualityrule.Event{EventType: e.EventType, HeadSHA: ref.HeadSHA, WorkflowID: ref.WorkflowID, TriggerEvent: e.TriggerEvent})
	}
	return qualityrule.Resolve(facts).Float64
}
