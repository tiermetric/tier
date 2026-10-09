package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// PushCommitStatus reports what RecordPushCommit did with one commit (#849).
type PushCommitStatus int

const (
	// PushCommitRecorded: the commit entered the ledger and its (repo, issue,
	// UTC day) push outcome was created or joined.
	PushCommitRecorded PushCommitStatus = iota + 1
	// PushCommitDuplicate: the ledger already holds this (repo, commit) — a
	// redelivery, or the commit reintroduced by another push. Nothing was
	// written, unless this push's time is earlier than the one stored: see
	// RecordPushCommit.
	PushCommitDuplicate
	// PushCommitPRCaptured: a stored outcome's merge_commit_sha is this commit —
	// the pull_request path already counted it. Nothing was written.
	PushCommitPRCaptured
)

// Push-outcome audit actions (push_outcome_audit.action).
const (
	PushAuditSuperseded    = "superseded"
	PushAuditRederivedFrom = "rederived_from"
	PushAuditRederivedTo   = "rederived_to"
)

// preLedgerSHAPrefix marks a push_outcome_commits entry that stands for commits
// nobody recorded. A real commit id never contains ':', which ValidPushCommitID
// enforces, so no delivered SHA can equal a marker.
const preLedgerSHAPrefix = "pre-ledger:"

// push_outcome_commits.push_order values other than a push time (#938).
const (
	// pushOrderFirst is a pre-ledger marker's key, and the column DEFAULT every
	// entry recorded before #938 carries: those keep precedence, so nothing
	// already stored changes owner.
	pushOrderFirst int64 = 0
	// pushOrderUnknown is the key of a commit whose push carried no usable
	// pushed_at. It sorts after every push time, and among such commits by
	// arrival (the ledger id), so such a commit is recorded (credit,
	// double-count guard) but never takes a row over.
	pushOrderUnknown int64 = math.MaxInt64
)

// pushOrderKey maps a push time to its ledger sort key: Unix seconds, or
// pushOrderUnknown for a zero or non-positive time, which cannot be ordered
// after the pre-#938 entries holding key 0.
func pushOrderKey(pushedAt time.Time) int64 {
	if pushedAt.IsZero() || pushedAt.Unix() <= pushOrderFirst {
		return pushOrderUnknown
	}
	return pushedAt.Unix()
}

// ValidPushCommitID reports whether a push commit id can be a ledger key (#849).
// An empty id cannot be deduplicated, and one containing ':' could collide with
// a pre-ledger marker. RecordPushCommit refuses any other id.
func ValidPushCommitID(sha string) bool {
	return sha != "" && !strings.Contains(sha, ":")
}

// pushRow is the part of a source='push' outcome the reconciler reads and
// rewrites.
type pushRow struct {
	id                 int64
	developer          string
	repo, issueID, day string
	ts                 time.Time
}

// RecordPushCommit folds one captured direct commit into its (repo, issue_id,
// UTC day) push outcome and records it in the push_outcome_commits ledger (#849).
// day is the commit's UTC 'YYYY-MM-DD'; o carries the commit's author, issue,
// repo and commit time; pushedAt is GitHub's repository.pushed_at for the push
// that carried the commit, zero when the payload had none (#938). Everything runs in ONE write-locked transaction:
//
//  1. a stored merge_commit_sha equal to commitSHA — in ANY repo, because the
//     #60 unique index makes merge_commit_sha install-wide and a PR outcome's
//     repo may be unqualified — means the pull_request path already counted this
//     commit: PushCommitPRCaptured, nothing written. supersedePushCommit uses the
//     same install-wide key;
//  2. a commit the ledger already holds returns PushCommitDuplicate. A commit
//     can reach the default branch in several pushes (removed, then
//     reintroduced), and its entry keeps the EARLIEST push time, so the owner
//     does not depend on which push was delivered first: a usable pushedAt
//     strictly earlier than the stored key lowers it and re-derives the entry's
//     row (audited). An identical, later or unusable push time, or a stored
//     key 0 (recorded before #938), writes nothing;
//  3. the day's push outcome is inserted if absent (ON CONFLICT DO NOTHING);
//  4. a pre-existing row with no ledger entries held commits nobody recorded,
//     so a pre-ledger marker is written for it first — the reconciler then never
//     deletes that row;
//  5. the ledger entry is inserted ON CONFLICT DO NOTHING — the unique (repo,
//     commit_sha) index is the arbiter, and a conflict is a redelivery: the
//     transaction rolls back and PushCommitDuplicate is returned;
//  6. the row's developer and ts are re-derived from its earliest ledger entry
//     (push time, then commit time, then SHA; entries with no push time by
//     arrival — see rederivePushOwner), so the owner
//     depends on neither arrival order nor the committer-set commit time; a
//     change is written to push_outcome_audit.
//
// UNBOUNDED write lock (the DSN's busy_timeout), deliberately, although the
// caller is the webhook request path: GitHub does not redeliver a failed
// delivery, so failing at the request-path cap would lose the commit where
// main's single-statement write waited. ErrWriteLockUnavailable after that wait
// is still answered with a 503.
func (d *DB) RecordPushCommit(ctx context.Context, o Outcome, day, commitSHA string, pushedAt time.Time) (PushCommitStatus, error) {
	if day == "" {
		return 0, errors.New("store: RecordPushCommit requires a non-empty UTC day (YYYY-MM-DD)")
	}
	if !ValidPushCommitID(commitSHA) {
		return 0, fmt.Errorf("store: RecordPushCommit: commit id %q cannot be a ledger key", commitSHA)
	}
	repo := normalizeRepo(o.Repo)
	o.Repo = repo
	commitTS := normalizeTS(o.Timestamp)

	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var prCaptured bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM outcomes WHERE merge_commit_sha = ?)`, commitSHA,
	).Scan(&prCaptured); err != nil {
		return 0, fmt.Errorf("RecordPushCommit: merge-commit lookup: %w", err)
	}
	if prCaptured {
		return PushCommitPRCaptured, nil // the deferred Rollback ends an empty tx
	}

	var heldID, heldOrder int64
	var heldDev string
	switch err := tx.QueryRowContext(ctx,
		`SELECT outcome_id, developer, push_order FROM push_outcome_commits WHERE repo = ? AND commit_sha = ?`,
		repo, commitSHA,
	).Scan(&heldID, &heldDev, &heldOrder); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, fmt.Errorf("RecordPushCommit: ledger lookup: %w", err)
	default:
		// pushOrderKey is always above pushOrderFirst and is pushOrderUnknown for
		// an unusable pushedAt, so neither a key-0 entry nor an unusable time lowers.
		key := pushOrderKey(pushedAt)
		if key >= heldOrder {
			return PushCommitDuplicate, nil // the deferred Rollback ends an empty tx
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE push_outcome_commits SET push_order = ? WHERE repo = ? AND commit_sha = ?`,
			key, repo, commitSHA,
		); err != nil {
			return 0, fmt.Errorf("RecordPushCommit: lower push_order: %w", err)
		}
		if held, found, err := loadPushRowByID(ctx, tx, heldID); err != nil {
			return 0, err
		} else if found {
			if err := rederivePushOwner(ctx, tx, held, pushCause{commitSHA, heldDev}, true); err != nil {
				return 0, err
			}
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("RecordPushCommit: commit: %w", err)
		}
		return PushCommitDuplicate, nil
	}

	created, err := upsertPushOutcomeRow(ctx, tx, o, day)
	if err != nil {
		return 0, fmt.Errorf("RecordPushCommit: upsert day row: %w", err)
	}
	row, err := loadPushRowByKey(ctx, tx, repo, o.IssueID, day)
	if err != nil {
		return 0, err
	}
	if !created {
		n, err := countLedgerEntries(ctx, tx, row.id)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO push_outcome_commits (repo, commit_sha, outcome_id, developer, ts, push_order)
				VALUES (?, ?, ?, ?, ?, ?)`,
				repo, preLedgerSHAPrefix+strconv.FormatInt(row.id, 10), row.id, row.developer, normalizeTS(row.ts), pushOrderFirst,
			); err != nil {
				return 0, fmt.Errorf("RecordPushCommit: pre-ledger marker: %w", err)
			}
		}
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO push_outcome_commits (repo, commit_sha, outcome_id, developer, ts, push_order)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo, commit_sha) DO NOTHING`,
		repo, commitSHA, row.id, o.Developer, commitTS, pushOrderKey(pushedAt))
	if err != nil {
		return 0, fmt.Errorf("RecordPushCommit: ledger insert: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		// Redelivery. Rolling back discards anything above, so a replay writes
		// nothing at all.
		return PushCommitDuplicate, nil
	}
	if err := rederivePushOwner(ctx, tx, row, pushCause{commitSHA, o.Developer}, true); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("RecordPushCommit: commit: %w", err)
	}
	return PushCommitRecorded, nil
}

// RecordPROutcome writes a merged pull request's outcome and, in the SAME
// write-locked transaction, removes its merge commit from the push ledger
// (#849). inserted is false when an outcome with this merge_commit_sha already
// exists — a redelivery, or a row an earlier writer stored without reconciling —
// and the ledger is reconciled anyway: that is a no-op once the entry is gone.
// Every PR insert path calls it: the pull_request webhook, `tierd backfill` and
// POST /api/v1/outcomes.
//
// When push capture already recorded the merge commit, its ledger entry is
// deleted. The push outcome that held it is deleted only if no entry remains —
// a pre-ledger marker counts as one — and is otherwise re-derived to its earliest
// remaining commit. Either change is written to push_outcome_audit. So a push
// outcome is never deleted while it holds a commit that did not come from this
// pull request, and one written before the ledger existed is never deleted.
//
// UNBOUNDED write lock, for the same reason as RecordPushCommit.
func (d *DB) RecordPROutcome(ctx context.Context, o Outcome) (inserted bool, err error) {
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	inserted, err = insertOutcomeRow(ctx, tx, o)
	if err != nil {
		return false, fmt.Errorf("RecordPROutcome: insert: %w", err)
	}
	if sha := o.MergeCommitSHA; ValidPushCommitID(sha) {
		if err := supersedePushCommit(ctx, tx, sha); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("RecordPROutcome: commit: %w", err)
	}
	return inserted, nil
}

// supersedePushCommit removes sha's ledger entries — in every repo, the key
// RecordPushCommit's merge-commit check uses — and reconciles each push outcome
// that held one. A missing entry (push capture off, or the push has not arrived
// yet) is a no-op: the push path will then see the merge commit and skip.
func supersedePushCommit(ctx context.Context, tx *sql.Tx, sha string) error {
	// Each outcome holds at most one entry for sha (one repo per outcome, unique
	// (repo, commit_sha)); its author is read before the DELETE removes it.
	type held struct {
		id  int64
		dev string
	}
	var entries []held
	rows, err := tx.QueryContext(ctx,
		`SELECT outcome_id, developer FROM push_outcome_commits WHERE commit_sha = ?`, sha)
	if err != nil {
		return fmt.Errorf("RecordPROutcome: ledger lookup: %w", err)
	}
	for rows.Next() {
		var e held
		if err := rows.Scan(&e.id, &e.dev); err != nil {
			_ = rows.Close()
			return fmt.Errorf("RecordPROutcome: ledger lookup: %w", err)
		}
		entries = append(entries, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("RecordPROutcome: ledger lookup: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM push_outcome_commits WHERE commit_sha = ?`, sha,
	); err != nil {
		return fmt.Errorf("RecordPROutcome: ledger delete: %w", err)
	}
	for _, e := range entries {
		if _, err := reconcilePushRow(ctx, tx, e.id, pushCause{sha, e.dev}, false); err != nil {
			return err
		}
	}
	return nil
}

// pushCause is the commit whose arrival or removal caused an audited change,
// and that commit's author.
type pushCause struct {
	sha, developer string
}

// reconcilePushRow brings push outcome id back in line with its ledger after
// entries were removed: re-derived to its earliest remaining entry, or deleted
// when none remains. cause is recorded in the audit rows. erasure
// writes no audit row about the erased subject — no 'superseded' image and no
// 'rederived_from' before-image (whose developer is necessarily the subject) —
// since the erasure would delete it in the same transaction.
func reconcilePushRow(ctx context.Context, tx *sql.Tx, id int64, cause pushCause, erasure bool) (deleted bool, err error) {
	row, found, err := loadPushRowByID(ctx, tx, id)
	if err != nil || !found {
		return false, err
	}
	remaining, err := countLedgerEntries(ctx, tx, row.id)
	if err != nil {
		return false, err
	}
	if remaining > 0 {
		return false, rederivePushOwner(ctx, tx, row, cause, !erasure)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outcomes WHERE id = ?`, row.id); err != nil {
		return false, fmt.Errorf("delete push outcome with no commits left: %w", err)
	}
	if erasure {
		return true, nil
	}
	return true, insertPushAudit(ctx, tx, row, PushAuditSuperseded, cause)
}

// eraseSubjectPushLedger is EraseDeveloper's #849 step, run BEFORE the
// per-table loop. It deletes only the subject's own ledger entries and
// reconciles every push outcome that held one, so a co-contributor's commit on a
// row the subject owned keeps its credit (the row is re-owned to it) and a row
// is deleted only when no entry remains. First it blanks, in every
// push_outcome_audit row, any commit SHA that is the subject's — a ledger entry,
// a merge commit of the subject's outcome, or a cause the subject authored
// (commit_developer, which survives the entry's supersede) — so another developer's audit
// trail no longer names the erased subject's commits. The re-derivations it
// writes carry an empty cause for the same reason. Returns the ledger entries
// deleted and the push outcomes deleted.
func eraseSubjectPushLedger(ctx context.Context, tx *sql.Tx, placeholders string, args []any) (ledger, outcomes int64, err error) {
	if _, err := tx.ExecContext(ctx, `
		UPDATE push_outcome_audit SET commit_sha = '', commit_developer = ''
		WHERE commit_developer IN (`+placeholders+`)
		   OR commit_sha IN (SELECT commit_sha FROM push_outcome_commits WHERE developer IN (`+placeholders+`))
		   OR commit_sha IN (SELECT merge_commit_sha FROM outcomes
		                     WHERE merge_commit_sha IS NOT NULL AND developer IN (`+placeholders+`))`,
		append(append(append([]any{}, args...), args...), args...)...); err != nil {
		return 0, 0, fmt.Errorf("blank the subject's commits in push_outcome_audit: %w", err)
	}
	ids, err := queryInt64s(ctx, tx,
		`SELECT DISTINCT outcome_id FROM push_outcome_commits WHERE developer IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("find the subject's push outcomes: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM push_outcome_commits WHERE developer IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("delete the subject's push_outcome_commits: %w", err)
	}
	if ledger, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		deleted, err := reconcilePushRow(ctx, tx, id, pushCause{}, true)
		if err != nil {
			return 0, 0, err
		}
		if deleted {
			outcomes++
		}
	}
	return ledger, outcomes, nil
}

func queryInt64s(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// rederivePushOwner sets the push row's developer and ts to its earliest ledger
// entry (push_order, ts, then commit_sha) and audits a change as a before/after pair (the
// after-image alone when !before). Entries keyed pushOrderUnknown order by id
// (arrival) before ts: their ts is committer-set, so ordering them by it would
// let a later unkeyed commit take the row from an earlier one.
func rederivePushOwner(ctx context.Context, tx *sql.Tx, row pushRow, cause pushCause, before bool) error {
	var dev string
	var ts time.Time
	if err := tx.QueryRowContext(ctx, `
		SELECT developer, ts FROM push_outcome_commits
		WHERE outcome_id = ?
		ORDER BY push_order, CASE WHEN push_order = ? THEN id END, ts, commit_sha LIMIT 1`,
		row.id, pushOrderUnknown,
	).Scan(&dev, &ts); err != nil {
		return fmt.Errorf("re-derive push owner: %w", err)
	}
	if dev == row.developer && ts.Equal(row.ts) {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE outcomes SET developer = ?, ts = ? WHERE id = ?`, dev, normalizeTS(ts), row.id,
	); err != nil {
		return fmt.Errorf("re-derive push owner: update: %w", err)
	}
	if before {
		if err := insertPushAudit(ctx, tx, row, PushAuditRederivedFrom, cause); err != nil {
			return err
		}
	}
	after := row
	after.developer, after.ts = dev, ts
	return insertPushAudit(ctx, tx, after, PushAuditRederivedTo, cause)
}

func insertPushAudit(ctx context.Context, tx *sql.Tx, row pushRow, action string, cause pushCause) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO push_outcome_audit
		    (outcome_id, repo, issue_id, push_day, action, developer, outcome_ts, commit_sha, commit_developer)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.repo, row.issueID, row.day, action, row.developer, normalizeTS(row.ts),
		cause.sha, cause.developer,
	); err != nil {
		return fmt.Errorf("push outcome audit (%s): %w", action, err)
	}
	return nil
}

func countLedgerEntries(ctx context.Context, tx *sql.Tx, outcomeID int64) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM push_outcome_commits WHERE outcome_id = ?`, outcomeID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count push ledger entries: %w", err)
	}
	return n, nil
}

const pushRowSelect = `SELECT id, developer, repo, issue_id, push_day, ts FROM outcomes `

func scanPushRow(r *sql.Row) (pushRow, error) {
	var p pushRow
	err := r.Scan(&p.id, &p.developer, &p.repo, &p.issueID, &p.day, &p.ts)
	return p, err
}

func loadPushRowByKey(ctx context.Context, tx *sql.Tx, repo, issueID, day string) (pushRow, error) {
	p, err := scanPushRow(tx.QueryRowContext(ctx, pushRowSelect+
		`WHERE source = 'push' AND repo = ? AND issue_id = ? AND push_day = ?`, repo, issueID, day))
	if err != nil {
		return pushRow{}, fmt.Errorf("load push outcome (%s, %s, %s): %w", repo, issueID, day, err)
	}
	return p, nil
}

// loadPushRowByID returns found=false when the row is gone (erased), which the
// caller treats as nothing left to reconcile.
func loadPushRowByID(ctx context.Context, tx *sql.Tx, id int64) (pushRow, bool, error) {
	p, err := scanPushRow(tx.QueryRowContext(ctx, pushRowSelect+
		`WHERE id = ? AND source = 'push'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return pushRow{}, false, nil
	}
	if err != nil {
		return pushRow{}, false, fmt.Errorf("load push outcome %d: %w", id, err)
	}
	return p, true, nil
}
