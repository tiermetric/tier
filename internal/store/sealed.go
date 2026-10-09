package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// schemaSealed is the sealed closed-period store (#913, #914 ruling A, #913-D1
// ruling A). Open applies it after Phase 3; none of these tables is ever rebuilt,
// so their triggers cannot be dropped by a later phase.
//
// A sealed period is a stored, never-recomputed snapshot: sealed_report holds the
// served body and its digest; sealed_rollup (the per-label pre-fold sums) and
// sealed_person (per-measure person keys, HMAC(install secret, period ‖ canonical
// id)) are never served and exist only for /compare to refold from.
//
// Period bounds and sealed_at are Go-written UTC RFC 3339 strings at whole
// seconds (sealTime), so lexical comparison is chronological; bind strings,
// never time.Time.
//
// UNIQUE(period_size, period_start): one row per period. The #913-D1 rule — a
// period sealed under ANY config blocks every overlapping span under another —
// includes month ⊂ quarter, which no unique index can express, so SealReport
// checks overlap inside its write transaction. Tenant retrofit: tenant_id LEADS
// -> (tenant_id, period_size, period_start).
//
// Triggers follow #604: BEFORE UPDATE RAISE(ABORT) on every sealed table, and no
// BEFORE DELETE on sealed_report, because deleting a whole period (retention) is
// lawful and no trigger can tell it from tampering. sealed_person admits one
// UPDATE, the #914 erase transition, modelled on #886's close-only trigger; its
// tombstone may not be a key another row already holds, or UPDATE OR REPLACE
// would delete that row without firing its triggers.
//
// sealed_rollup's has_<measure> and contributes columns are the fold inputs no
// sum or person set can rebuild (#913-D3 ruling A'); they have no DEFAULT, so a
// write that omits one fails and a dev database whose sealed_rollup predates them
// refuses to open unless the table is empty (sealedRollupFoldCols). erase_epoch
// counts committed erasures and alias edits and stores no id (SealCheck.EraseEpoch).
//
// seal_floor pins the earliest sealable period at the first seal (#913-D2 item
// 4), so cost rows back-dated after it never make an earlier period sealable.
// Like install_secret it refuses UPDATE and DELETE and ignores a second INSERT.
//
// install_secret also refuses DELETE: nothing lawfully removes it, and Open would
// silently mint a new secret, leaving every stored person key unmatchable by the
// erase that must find it. Foreign keys are off, so deleting a sealed_report row
// deletes its children by trigger, and a child row cannot be deleted while its
// period exists (an erase tombstones, never deletes).
//
// recursive_triggers is off, so INSERT OR REPLACE deletes a conflicting row
// without firing its UPDATE or DELETE triggers. Every table therefore also has a
// BEFORE INSERT trigger on its keys: install_secret and sealed_report ignore the
// insert (keeping first-wins), sealed_rollup and sealed_person abort it. NEW.rowid
// is -1 in a BEFORE INSERT whose rowid is auto-assigned (SQLite documents it as
// undefined; measured -1 on modernc SQLite 3.51.3), so the rowid arm matches only
// an explicit rowid. Same limit as #604: these stop a code path, not someone
// holding the file.
const schemaSealed = `
CREATE TABLE IF NOT EXISTS sealed_report (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    level         TEXT    NOT NULL,
    period_size   TEXT    NOT NULL,
    period_start  TEXT    NOT NULL,
    period_end    TEXT    NOT NULL,
    k             INTEGER NOT NULL,
    config_digest TEXT    NOT NULL,
    sealed_at     TEXT    NOT NULL,
    body          BLOB    NOT NULL,
    body_digest   TEXT    NOT NULL,
    tool_version  TEXT    NOT NULL,
    tool_commit   TEXT    NOT NULL,
    CHECK (period_start < period_end)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sealed_report_period
    ON sealed_report(period_size, period_start);
CREATE TABLE IF NOT EXISTS sealed_rollup (
    report_id        INTEGER NOT NULL REFERENCES sealed_report(id),
    label            TEXT    NOT NULL,
    weighted_points  REAL    NOT NULL,
    total_cost_usd   REAL    NOT NULL,
    actual_paid_usd  REAL    NOT NULL,
    realtime_usd     REAL    NOT NULL,
    sample_n         INTEGER NOT NULL,
    flagged_outcomes INTEGER NOT NULL,
    has_points       INTEGER NOT NULL CHECK (has_points IN (0, 1)),
    has_cost         INTEGER NOT NULL CHECK (has_cost IN (0, 1)),
    has_realtime     INTEGER NOT NULL CHECK (has_realtime IN (0, 1)),
    has_non_realtime INTEGER NOT NULL CHECK (has_non_realtime IN (0, 1)),
    has_paid         INTEGER NOT NULL CHECK (has_paid IN (0, 1)),
    contributes      INTEGER NOT NULL CHECK (contributes IN (0, 1)),
    PRIMARY KEY (report_id, label)
);
CREATE TABLE IF NOT EXISTS sealed_person (
    report_id  INTEGER NOT NULL REFERENCES sealed_report(id),
    label      TEXT    NOT NULL,
    measure    TEXT    NOT NULL,
    person_key BLOB    NOT NULL,
    tombstoned INTEGER NOT NULL DEFAULT 0 CHECK (tombstoned IN (0, 1)),
    PRIMARY KEY (report_id, label, measure, person_key)
);
CREATE TABLE IF NOT EXISTS erase_epoch (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    n  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS install_secret (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    secret BLOB    NOT NULL CHECK (length(secret) = 32)
);
CREATE TABLE IF NOT EXISTS seal_floor (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    period_start TEXT    NOT NULL
);
CREATE TRIGGER IF NOT EXISTS trg_seal_floor_no_update
BEFORE UPDATE ON seal_floor
BEGIN
    SELECT RAISE(ABORT, 'seal_floor is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_seal_floor_no_delete
BEFORE DELETE ON seal_floor
BEGIN
    SELECT RAISE(ABORT, 'seal_floor is append-only: DELETE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_seal_floor_first_wins
BEFORE INSERT ON seal_floor
WHEN EXISTS (SELECT 1 FROM seal_floor)
BEGIN
    SELECT RAISE(IGNORE);
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_report_no_update
BEFORE UPDATE ON sealed_report
BEGIN
    SELECT RAISE(ABORT, 'sealed_report is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_rollup_no_update
BEFORE UPDATE ON sealed_rollup
BEGIN
    SELECT RAISE(ABORT, 'sealed_rollup is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_person_tombstone_only
BEFORE UPDATE ON sealed_person
WHEN NOT (OLD.tombstoned = 0 AND NEW.tombstoned = 1
          AND NEW.person_key <> OLD.person_key
          AND NEW.rowid = OLD.rowid
          AND NEW.report_id = OLD.report_id
          AND NEW.label = OLD.label
          AND NEW.measure = OLD.measure
          AND NOT EXISTS (SELECT 1 FROM sealed_person p
                          WHERE p.report_id = NEW.report_id AND p.label = NEW.label
                            AND p.measure = NEW.measure AND p.person_key = NEW.person_key))
BEGIN
    SELECT RAISE(ABORT, 'sealed_person is append-only: only tombstoning a person key is permitted (#914)');
END;
CREATE TRIGGER IF NOT EXISTS trg_install_secret_no_update
BEFORE UPDATE ON install_secret
BEGIN
    SELECT RAISE(ABORT, 'install_secret is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_install_secret_no_delete
BEFORE DELETE ON install_secret
BEGIN
    SELECT RAISE(ABORT, 'install_secret is append-only: DELETE is refused (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_install_secret_first_wins
BEFORE INSERT ON install_secret
WHEN EXISTS (SELECT 1 FROM install_secret)
BEGIN
    SELECT RAISE(IGNORE);
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_report_first_wins
BEFORE INSERT ON sealed_report
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE id = NEW.id
             OR (period_size = NEW.period_size AND period_start = NEW.period_start))
BEGIN
    SELECT RAISE(IGNORE);
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_rollup_no_replace
BEFORE INSERT ON sealed_rollup
WHEN EXISTS (SELECT 1 FROM sealed_rollup WHERE rowid = NEW.rowid
             OR (report_id = NEW.report_id AND label = NEW.label))
BEGIN
    SELECT RAISE(ABORT, 'sealed_rollup is append-only: a row with that key exists (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_person_no_replace
BEFORE INSERT ON sealed_person
WHEN EXISTS (SELECT 1 FROM sealed_person WHERE rowid = NEW.rowid
             OR (report_id = NEW.report_id AND label = NEW.label
                 AND measure = NEW.measure AND person_key = NEW.person_key))
BEGIN
    SELECT RAISE(ABORT, 'sealed_person is append-only: a row with that key exists (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_rollup_no_lone_delete
BEFORE DELETE ON sealed_rollup
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE id = OLD.report_id)
BEGIN
    SELECT RAISE(ABORT, 'sealed_rollup is append-only: delete its sealed period instead (#913)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_person_no_lone_delete
BEFORE DELETE ON sealed_person
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE id = OLD.report_id)
BEGIN
    SELECT RAISE(ABORT, 'sealed_person is append-only: tombstone, or delete its sealed period (#914)');
END;
CREATE TRIGGER IF NOT EXISTS trg_sealed_report_delete_children
AFTER DELETE ON sealed_report
BEGIN
    DELETE FROM sealed_rollup WHERE report_id = OLD.id;
    DELETE FROM sealed_person WHERE report_id = OLD.id;
END;
`

// installSecretLen is the install secret's size in bytes.
const installSecretLen = 32

// ensureInstallSecret creates the install secret on the first Open of a database
// and leaves an existing one untouched.
func ensureInstallSecret(db *sql.DB) error {
	secret := make([]byte, installSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate install secret: %w", err)
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO install_secret (id, secret) VALUES (1, ?)`, secret)
	return err
}

type rowReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// installSecret returns this database's install secret, the HMAC key for sealed
// person keys (#914). It is stable for the life of the database and never
// leaves this package.
func installSecret(ctx context.Context, q rowReader) ([]byte, error) {
	var secret []byte
	if err := q.QueryRowContext(ctx, `SELECT secret FROM install_secret WHERE id = 1`).Scan(&secret); err != nil {
		return nil, fmt.Errorf("read install secret: %w", err)
	}
	return secret, nil
}

// personKey is a sealed person key (#914 ruling A): HMAC-SHA256, keyed by the
// install secret, over exactly these bytes:
//
//	period_size ‖ 0x00 ‖ period_start ‖ 0x00 ‖ canonical_id
//
// period_start is sealTime's form (RFC 3339, UTC, whole seconds), the string
// sealed_report stores. The encoding is injective because neither period_size
// (SealReport refuses a 0x00 in it) nor an RFC 3339 instant contains 0x00, and
// canonical_id comes last. SealReport and EraseDeveloper must compute the same
// bytes, or erasure cannot find a stored key; TestPersonKey_PinnedEncoding pins
// them with a fixed vector.
func personKey(secret []byte, periodSize string, periodStart time.Time, canonicalID string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(periodSize))
	mac.Write([]byte{0})
	mac.Write([]byte(sealTime(periodStart)))
	mac.Write([]byte{0})
	mac.Write([]byte(canonicalID))
	return mac.Sum(nil)
}

// cryptoPerm returns a uniformly random permutation of 0..n-1 drawn from
// crypto/rand (Fisher-Yates).
func cryptoPerm(n int) ([]int, error) {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, fmt.Errorf("shuffle sealed persons: %w", err)
		}
		p[i], p[j.Int64()] = p[j.Int64()], p[i]
	}
	return p, nil
}

// tombstoneSealedPersons replaces, in every sealed period, each person key that
// one of ids computes to, or (#975) the canonical of one of links active when
// that period was sealed, with a fresh random tombstone and sets tombstoned = 1
// (#914 ruling A). It runs inside EraseDeveloper's transaction and returns the
// number of rows tombstoned. It deletes no row and never writes sealed_report
// or sealed_rollup.
//
// One tombstone per (period, key), reused under every label and measure that
// key is held under, so a period's person sets keep exactly the equality
// structure they were sealed with and a refold counts what it counted before.
// A person stored under two ids that were separate people when the period was
// sealed holds two keys there, possibly under one label and measure; those keep
// two tombstones, since one would take the other row's primary key, which
// trg_sealed_person_tombstone_only refuses.
func tombstoneSealedPersons(ctx context.Context, tx *sql.Tx, ids []string, links []canonicalLink) (int64, error) {
	type period struct {
		id    int64
		size  string
		start time.Time
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, period_size, period_start FROM sealed_report ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("query sealed periods: %w", err)
	}
	var periods []period
	for rows.Next() {
		var p period
		var start string
		if err := rows.Scan(&p.id, &p.size, &start); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if p.start, err = time.Parse(time.RFC3339, start); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("parse sealed_report %d period_start %q: %w", p.id, start, err)
		}
		periods = append(periods, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(periods) == 0 {
		return 0, nil
	}

	secret, err := installSecret(ctx, tx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range periods {
		periodIDs := append([]string(nil), ids...)
		for _, l := range links {
			if l.activeAt(p.id) && !slices.Contains(periodIDs, l.canonical) {
				periodIDs = append(periodIDs, l.canonical)
			}
		}
		for _, id := range periodIDs {
			tombstone := make([]byte, sha256.Size)
			if _, err := rand.Read(tombstone); err != nil {
				return 0, fmt.Errorf("generate sealed person tombstone: %w", err)
			}
			res, err := tx.ExecContext(ctx, `UPDATE sealed_person SET person_key = ?, tombstoned = 1
				WHERE report_id = ? AND person_key = ? AND tombstoned = 0`,
				tombstone, p.id, personKey(secret, p.size, p.start, id))
			if err != nil {
				return 0, fmt.Errorf("tombstone sealed_person: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

// SealedReport is one sealed closed reporting period (#913).
type SealedReport struct {
	ID           int64
	Level        string
	PeriodSize   string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	K            int
	ConfigDigest string
	SealedAt     time.Time
	Body         []byte
	BodyDigest   string
	ToolVersion  string
	ToolCommit   string
}

// SealedMeasures names sealed_rollup's has_<name> columns in measure order; it
// equals scoring.MeasureNames, which a test pins, and a name is never reassigned.
var SealedMeasures = [...]string{"points", "cost", "realtime", "non_realtime", "paid"}

// sealedRollupFoldCols are sealed_rollup's fold-input columns: has_<name> for
// each SealedMeasures entry, then contributes.
var sealedRollupFoldCols = func() []string {
	var cols []string
	for _, m := range SealedMeasures {
		cols = append(cols, "has_"+m)
	}
	return append(cols, "contributes")
}()

// SealedRollup is one label's pre-fold input in a sealed period, mirroring
// scoring.LabelInput's sums, Has and Contributes. It is never served.
type SealedRollup struct {
	Label                                                    string
	WeightedPoints, TotalCostUSD, ActualPaidUSD, RealtimeUSD float64
	SampleN, FlaggedOutcomes                                 int
	Has                                                      [len(SealedMeasures)]bool
	Contributes                                              bool
}

// SealedPersonKey is one stored person row of a sealed period: its key, never
// the canonical id it was computed from.
type SealedPersonKey struct {
	Label, Measure string
	Key            []byte
}

// SealCheck is what SealReport verifies inside its write transaction before a
// new period commits.
type SealCheck struct {
	// EraseEpoch is the erase epoch as the caller read it inside the same read
	// snapshot as its window reads (anywhere in it); an erase or alias edit that
	// snapshot cannot see refuses the seal with ErrSealEraseRaced.
	EraseEpoch int64
	// Refold receives the new period's fold inputs as read back from the
	// transaction; an error rolls the seal back. Nil skips it.
	Refold func([]SealedRollup, []SealedPersonKey) error
	// Floor is the earliest sealable period's start as the caller computed it.
	// The first seal must start at it and pins it (seal_floor), unless one is
	// pinned already; a zero Floor is refused with ErrSealBeforeFloor, and a
	// period that is not the next one with ErrSealNotNext.
	Floor time.Time
	// First refuses the seal with ErrSealNotNext unless no period is sealed and
	// no floor is pinned: the seal that arms sealing (#913-D5 ruling C′).
	First bool
	// Sources re-checks the seal gate (#913-D9) against source_watermark as this
	// transaction reads it; an error rolls the seal back. Nil skips it.
	Sources func(ctx context.Context, r *Snapshot) error
}

// ErrSealBeforeFloor is SealReport's refusal of a period that starts before the
// earliest sealable period pinned at the first seal (#913-D2 item 4).
var ErrSealBeforeFloor = errors.New("period starts before the earliest sealable period pinned at the first seal")

// SealFloor returns the earliest sealable period's start pinned at the first
// seal; ok is false before any period is sealed.
func (d *DB) SealFloor(ctx context.Context) (start time.Time, ok bool, err error) {
	return sealFloor(ctx, d.db)
}

func (r reader) SealFloor(ctx context.Context) (time.Time, bool, error) { return sealFloor(ctx, r.q) }

func sealFloor(ctx context.Context, q rowReader) (time.Time, bool, error) {
	var s string
	err := q.QueryRowContext(ctx, `SELECT period_start FROM seal_floor WHERE id = 1`).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read seal floor: %w", err)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse seal floor %q: %w", s, err)
	}
	return t, true, nil
}

// pinSealFloor pins floor unless one is pinned already (first wins), then
// refuses a period starting before the pinned floor. It runs inside SealReport's
// write transaction, so the first seal and its floor commit together.
func pinSealFloor(ctx context.Context, tx *sql.Tx, start, floor time.Time) error {
	if floor.IsZero() {
		return fmt.Errorf("%w: no earliest sealable period given", ErrSealBeforeFloor)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO seal_floor (id, period_start) VALUES (1, ?)`, sealTime(floor)); err != nil {
		return fmt.Errorf("pin seal floor: %w", err)
	}
	pinned, ok, err := sealFloor(ctx, tx)
	if err != nil {
		return err
	}
	if !ok || start.Before(pinned) {
		return fmt.Errorf("%w: %s is before %s", ErrSealBeforeFloor, sealTime(start), sealTime(pinned))
	}
	return nil
}

// ErrSealNotNext is SealReport's refusal of a new period that does not start
// where the newest sealed or gapped period ends or, while none is sealed, at the
// pinned floor (#913-D5 ruling C′): periods seal in order, skipping only a
// recorded gap, whichever process seals them.
var ErrSealNotNext = errors.New("period is not the next one to seal")

// checkSealNext refuses new period r with ErrSealNotNext unless it starts where
// the newest sealed or gapped period ends or, while none is sealed, at the pinned floor,
// else at floor. A zero floor with none pinned is left to pinSealFloor's refusal.
func checkSealNext(ctx context.Context, tx *sql.Tx, r SealedReport, floor time.Time) error {
	want, occupied, err := newestOccupiedEnd(ctx, tx)
	if err != nil {
		return err
	}
	if !occupied {
		pinned, ok, err := sealFloor(ctx, tx)
		if err != nil {
			return err
		}
		if ok {
			floor = pinned
		}
		if floor.IsZero() {
			return nil
		}
		want = sealTime(floor)
	}
	if got := sealTime(r.PeriodStart); got != want {
		return fmt.Errorf("%w: %s %s, the next is %s", ErrSealNotNext, r.PeriodSize, got, want)
	}
	return nil
}

// ErrSealEraseRaced is SealReport's refusal when an erase or alias edit
// committed after the caller read the erase epoch: its figures may hold the
// erased person, or keys for a canonical the edit retired that a later erase
// would not find (#975 intervals start at the edit), so the caller recomputes
// and retries.
var ErrSealEraseRaced = errors.New("an erase or alias edit committed while the period was computed; recompute and retry")

// ErrSealClockBehind is SealReport's and RecordSealedGap's refusal when the
// server clock reads earlier than the newest seal or gap recorded (#913-D4): a
// clock that stepped backwards cannot be trusted to judge which months have
// closed and passed their grace.
var ErrSealClockBehind = errors.New("server clock is earlier than a period already sealed")

// EraseEpoch is the number of erasures and alias edits committed so far (#913);
// it stores no id.
func (d *DB) EraseEpoch(ctx context.Context) (int64, error) { return eraseEpoch(ctx, d.db) }

func (r reader) EraseEpoch(ctx context.Context) (int64, error) { return eraseEpoch(ctx, r.q) }

func eraseEpoch(ctx context.Context, q rowReader) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(n), 0) FROM erase_epoch`).Scan(&n); err != nil {
		return 0, fmt.Errorf("read erase epoch: %w", err)
	}
	return n, nil
}

// bumpEraseEpoch counts one erasure or alias edit, inside its transaction.
func bumpEraseEpoch(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO erase_epoch (id, n) VALUES (1, 1)
		ON CONFLICT (id) DO UPDATE SET n = n + 1`); err != nil {
		return fmt.Errorf("bump erase epoch: %w", err)
	}
	return nil
}

// sealedFoldInputs reads report id's stored fold inputs back, rollups in label
// order and person keys in (label, measure, key) order.
func sealedFoldInputs(ctx context.Context, q queryer, id int64) ([]SealedRollup, []SealedPersonKey, error) {
	rows, err := q.QueryContext(ctx, `SELECT label, weighted_points, total_cost_usd, actual_paid_usd, realtime_usd,
		sample_n, flagged_outcomes, `+strings.Join(sealedRollupFoldCols, ", ")+`
		FROM sealed_rollup WHERE report_id = ? ORDER BY label`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("read back sealed_rollup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var rollups []SealedRollup
	for rows.Next() {
		var s SealedRollup
		dst := []any{&s.Label, &s.WeightedPoints, &s.TotalCostUSD, &s.ActualPaidUSD, &s.RealtimeUSD, &s.SampleN, &s.FlaggedOutcomes}
		for i := range s.Has {
			dst = append(dst, &s.Has[i])
		}
		if err := rows.Scan(append(dst, &s.Contributes)...); err != nil {
			return nil, nil, err
		}
		rollups = append(rollups, s)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	prows, err := q.QueryContext(ctx, `SELECT label, measure, person_key FROM sealed_person
		WHERE report_id = ? ORDER BY label, measure, person_key`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("read back sealed_person: %w", err)
	}
	defer func() { _ = prows.Close() }()
	var persons []SealedPersonKey
	for prows.Next() {
		var p SealedPersonKey
		if err := prows.Scan(&p.Label, &p.Measure, &p.Key); err != nil {
			return nil, nil, err
		}
		persons = append(persons, p)
	}
	return rollups, persons, prows.Err()
}

// SealedFoldInputs returns sealed report id's stored fold inputs, rollups in
// label order and person keys in (label, measure, key) order: the inputs a
// comparison of two sealed periods refolds (#913).
func (d *DB) SealedFoldInputs(ctx context.Context, id int64) ([]SealedRollup, []SealedPersonKey, error) {
	return sealedFoldInputs(ctx, d.db, id)
}

// SealedPerson is one person counted under a label and measure in a sealed
// period, named by canonical id. SealReport stores only its person key.
type SealedPerson struct {
	Label, Measure, CanonicalID string
}

// ErrSealedPeriodOverlap is SealReport's refusal of a span that overlaps an
// already-sealed period other than the same seal (sameSeal) (#913-D1 ruling A).
var ErrSealedPeriodOverlap = errors.New("period overlaps a period already sealed under another config or span")

// sameSeal reports whether a and b are the same sealed period under the same
// config: the only overlap SealReport admits.
func sameSeal(a, b SealedReport) bool {
	return a.PeriodSize == b.PeriodSize && a.PeriodStart.Equal(b.PeriodStart) &&
		a.PeriodEnd.Equal(b.PeriodEnd) && a.ConfigDigest == b.ConfigDigest &&
		a.Level == b.Level && a.K == b.K
}

// ErrSealedPeriodInvalid is SealReport's refusal of an empty or inverted span, a
// bound that is not a whole second (the stored form keeps whole seconds), or a
// period size containing a NUL byte (personKey's encoding separator).
var ErrSealedPeriodInvalid = errors.New("sealed period span is empty, inverted or not at whole seconds, or its size contains a NUL byte")

// ErrSealedReportNotFound is SealedReport's answer when no such period is sealed.
var ErrSealedReportNotFound = errors.New("no sealed report for that period")

// validSealSpan reports whether a sealed or gapped period is not empty or
// inverted, has whole-second bounds, and has a size without a NUL byte.
func validSealSpan(size string, start, end time.Time) bool {
	return start.Before(end) && start.Equal(start.Truncate(time.Second)) &&
		end.Equal(end.Truncate(time.Second)) && strings.IndexByte(size, 0) < 0
}

// checkSealClock refuses with ErrSealClockBehind when now, a sealTime, is
// earlier than the newest sealed_at or gap created_at. Both are fixed-width UTC
// RFC 3339 strings, so they order as strings.
func checkSealClock(ctx context.Context, tx *sql.Tx, now string) error {
	var newest sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MAX(t) FROM (SELECT MAX(sealed_at) AS t FROM sealed_report
		UNION ALL SELECT MAX(created_at) FROM sealed_gap)`).Scan(&newest); err != nil {
		return fmt.Errorf("read newest seal time: %w", err)
	}
	if newest.Valid && now < newest.String {
		return fmt.Errorf("%w: now %s, newest seal or gap at %s", ErrSealClockBehind, now, newest.String)
	}
	return nil
}

// sealTime is the stored form of a sealed-period instant.
func sealTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

const sealedReportCols = `id, level, period_size, period_start, period_end, k, config_digest,
	sealed_at, body, body_digest, tool_version, tool_commit`

func scanSealedReport(row interface{ Scan(...any) error }) (SealedReport, error) {
	var r SealedReport
	var start, end, sealedAt string
	if err := row.Scan(&r.ID, &r.Level, &r.PeriodSize, &start, &end, &r.K, &r.ConfigDigest,
		&sealedAt, &r.Body, &r.BodyDigest, &r.ToolVersion, &r.ToolCommit); err != nil {
		return SealedReport{}, err
	}
	for _, p := range []struct {
		dst *time.Time
		src string
	}{{&r.PeriodStart, start}, {&r.PeriodEnd, end}, {&r.SealedAt, sealedAt}} {
		t, err := time.Parse(time.RFC3339, p.src)
		if err != nil {
			return SealedReport{}, fmt.Errorf("parse sealed_report %d time %q: %w", r.ID, p.src, err)
		}
		*p.dst = t
	}
	return r, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// sealedOverlapping returns every sealed period whose half-open span
// [period_start, period_end) intersects [start, end).
func sealedOverlapping(ctx context.Context, q queryer, start, end time.Time) ([]SealedReport, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+sealedReportCols+` FROM sealed_report
		WHERE period_start < ? AND period_end > ? ORDER BY period_start, id`,
		sealTime(end), sealTime(start))
	if err != nil {
		return nil, fmt.Errorf("query overlapping sealed periods: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SealedReport
	for rows.Next() {
		r, err := scanSealedReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SealedReportsOverlapping returns every sealed period whose span intersects
// [start, end), in period_start order.
func (d *DB) SealedReportsOverlapping(ctx context.Context, start, end time.Time) ([]SealedReport, error) {
	return sealedOverlapping(ctx, d.db, start, end)
}

// LatestSealedPeriod returns the start of the newest sealed period of
// periodSize; ok is false when none is sealed.
func (d *DB) LatestSealedPeriod(ctx context.Context, periodSize string) (start time.Time, ok bool, err error) {
	return latestPeriodStart(ctx, d.db, "sealed_report", periodSize)
}

// latestPeriodStart is the newest period_start of periodSize in table, which is
// sealed_report or sealed_gap, never caller input.
func latestPeriodStart(ctx context.Context, q rowReader, table, periodSize string) (start time.Time, ok bool, err error) {
	var s sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT MAX(period_start) FROM `+table+` WHERE period_size = ?`,
		periodSize).Scan(&s); err != nil {
		return time.Time{}, false, fmt.Errorf("read latest period in %s: %w", table, err)
	}
	if !s.Valid {
		return time.Time{}, false, nil
	}
	if start, err = time.Parse(time.RFC3339, s.String); err != nil {
		return time.Time{}, false, fmt.Errorf("parse latest sealed period %q: %w", s.String, err)
	}
	return start, true, nil
}

// SealedReport returns the period sealed at (periodSize, periodStart) with the
// level, k and config it was sealed under, which may differ from the current
// config (#913-D1 ruling A: sealed history is pinned), or ErrSealedReportNotFound.
func (d *DB) SealedReport(ctx context.Context, periodSize string, periodStart time.Time) (SealedReport, error) {
	return reader{d.db}.SealedReport(ctx, periodSize, periodStart)
}

func (r reader) SealedReport(ctx context.Context, periodSize string, periodStart time.Time) (SealedReport, error) {
	rep, err := scanSealedReport(r.q.QueryRowContext(ctx, `SELECT `+sealedReportCols+` FROM sealed_report
		WHERE period_size = ? AND period_start = ?`, periodSize, sealTime(periodStart)))
	if errors.Is(err, sql.ErrNoRows) {
		return SealedReport{}, ErrSealedReportNotFound
	}
	return rep, err
}

// SealReport seals one closed period with its unserved fold inputs, in one write
// transaction, bounded like every request-path writer. r.ID and r.SealedAt are assigned here. Any
// overlapping sealed period that is not sameSeal is refused with
// ErrSealedPeriodOverlap. Sealing the same period again inserts nothing and
// returns the row already sealed, so concurrent first sealers all serve the
// winner's bytes. Each person's key is computed here, inside the transaction,
// with personKey under the install secret, so no key bytes cross the package
// boundary. A new period is checked against check, and must be the next one
// (checkSealNext), before it commits; a span overlapping a recorded gap is
// refused with ErrSealedPeriodGapped. won reports whether this call's insert
// sealed the period, rather than finding it sealed.
func (d *DB) SealReport(ctx context.Context, r SealedReport, rollups []SealedRollup, persons []SealedPerson, check SealCheck) (sealed SealedReport, won bool, err error) {
	if !validSealSpan(r.PeriodSize, r.PeriodStart, r.PeriodEnd) {
		return SealedReport{}, false, ErrSealedPeriodInvalid
	}
	tx, release, err := beginImmediateBounded(ctx, d.db, requestPathBusyTimeout)
	if err != nil {
		return SealedReport{}, false, err
	}
	defer release()

	if check.First {
		var armed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sealed_report)
			OR EXISTS (SELECT 1 FROM seal_floor)`).Scan(&armed); err != nil {
			return SealedReport{}, false, fmt.Errorf("read whether sealing is armed: %w", err)
		} else if armed {
			return SealedReport{}, false, fmt.Errorf("%w: sealing is armed already", ErrSealNotNext)
		}
	}
	if gapped, err := gapOverlaps(ctx, tx, r.PeriodStart, r.PeriodEnd); err != nil {
		return SealedReport{}, false, err
	} else if gapped {
		return SealedReport{}, false, fmt.Errorf("%w: %s %s", ErrSealedPeriodGapped, r.PeriodSize, sealTime(r.PeriodStart))
	}
	overlapping, err := sealedOverlapping(ctx, tx, r.PeriodStart, r.PeriodEnd)
	if err != nil {
		return SealedReport{}, false, err
	}
	for _, o := range overlapping {
		if !sameSeal(o, r) {
			return SealedReport{}, false, overlapErr(o)
		}
	}
	if len(overlapping) == 0 {
		if err := checkSealNext(ctx, tx, r, check.Floor); err != nil {
			return SealedReport{}, false, err
		}
		if check.Sources != nil {
			if err := check.Sources(ctx, &Snapshot{reader{tx}}); err != nil {
				return SealedReport{}, false, err
			}
		}
	}
	now := sealTime(d.clock())
	if err := checkSealClock(ctx, tx, now); err != nil {
		return SealedReport{}, false, err
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO sealed_report
		(level, period_size, period_start, period_end, k, config_digest, sealed_at,
		 body, body_digest, tool_version, tool_commit)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (period_size, period_start) DO NOTHING`,
		r.Level, r.PeriodSize, sealTime(r.PeriodStart), sealTime(r.PeriodEnd), r.K, r.ConfigDigest,
		now, r.Body, r.BodyDigest, r.ToolVersion, r.ToolCommit)
	if err != nil {
		return SealedReport{}, false, fmt.Errorf("insert sealed_report: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return SealedReport{}, false, err
	} else if won = n == 1; won {
		id, err := res.LastInsertId()
		if err != nil {
			return SealedReport{}, false, err
		}
		// Every erase and alias edit takes the write lock this transaction holds,
		// so the epoch cannot move between this read and the commit.
		if epoch, err := eraseEpoch(ctx, tx); err != nil {
			return SealedReport{}, false, err
		} else if epoch != check.EraseEpoch {
			return SealedReport{}, false, ErrSealEraseRaced
		}
		if err := pinSealFloor(ctx, tx, r.PeriodStart, check.Floor); err != nil {
			return SealedReport{}, false, err
		}
		for _, s := range rollups {
			args := []any{id, s.Label, s.WeightedPoints, s.TotalCostUSD, s.ActualPaidUSD, s.RealtimeUSD, s.SampleN, s.FlaggedOutcomes}
			for _, has := range s.Has {
				args = append(args, has)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sealed_rollup
				(report_id, label, weighted_points, total_cost_usd, actual_paid_usd, realtime_usd, sample_n, flagged_outcomes, `+
				strings.Join(sealedRollupFoldCols, ", ")+`) VALUES (?`+strings.Repeat(", ?", len(args))+`)`,
				append(args, s.Contributes)...); err != nil {
				return SealedReport{}, false, fmt.Errorf("insert sealed_rollup: %w", err)
			}
		}
		secret, err := installSecret(ctx, tx)
		if err != nil {
			return SealedReport{}, false, err
		}
		// A tombstone keeps its row's rowid, so rows go in in random order: a
		// rowid must not reveal where the person sorted among the period's others.
		order, err := cryptoPerm(len(persons))
		if err != nil {
			return SealedReport{}, false, err
		}
		for _, i := range order {
			p := persons[i]
			if _, err := tx.ExecContext(ctx, `INSERT INTO sealed_person (report_id, label, measure, person_key)
				VALUES (?, ?, ?, ?)`, id, p.Label, p.Measure,
				personKey(secret, r.PeriodSize, r.PeriodStart, p.CanonicalID)); err != nil {
				return SealedReport{}, false, fmt.Errorf("insert sealed_person: %w", err)
			}
		}
		if check.Refold != nil {
			stored, keys, err := sealedFoldInputs(ctx, tx, id)
			if err != nil {
				return SealedReport{}, false, err
			}
			if err := check.Refold(stored, keys); err != nil {
				return SealedReport{}, false, err
			}
		}
	}

	sealed, err = scanSealedReport(tx.QueryRowContext(ctx, `SELECT `+sealedReportCols+` FROM sealed_report
		WHERE period_size = ? AND period_start = ?`, r.PeriodSize, sealTime(r.PeriodStart)))
	if err != nil {
		return SealedReport{}, false, fmt.Errorf("re-select sealed_report: %w", err)
	}
	if !sameSeal(sealed, r) {
		return SealedReport{}, false, overlapErr(sealed)
	}
	if err := tx.Commit(); err != nil {
		return SealedReport{}, false, fmt.Errorf("commit seal: %w", err)
	}
	return sealed, won, nil
}

func overlapErr(o SealedReport) error {
	return fmt.Errorf("%w: %s %s..%s is sealed", ErrSealedPeriodOverlap,
		o.PeriodSize, sealTime(o.PeriodStart), sealTime(o.PeriodEnd))
}
