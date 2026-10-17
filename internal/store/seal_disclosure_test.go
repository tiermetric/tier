package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSealDisclosureLegacyMigrationPreservesBytes(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	r := sealOne(t, db)
	// A pre-disclosure writer leaves the grace and observation unknown.
	var grace, open any
	if err := db.db.QueryRow(`SELECT grace_ns, revert_windows_open FROM sealed_report WHERE id=?`, r.ID).Scan(&grace, &open); err != nil {
		t.Fatal(err)
	}
	if grace != nil || open != nil {
		t.Fatalf("invented legacy disclosure: %v %v", grace, open)
	}
	assertRefused(t, db, `UPDATE sealed_report SET grace_ns = 1`, `SELECT body FROM sealed_report`, "july")
}

func TestSealDisclosureOpenWindowUsesActualInsertTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"open", 59 * 24 * time.Hour, true}, {"boundary", 60 * 24 * time.Hour, true}, {"closed", 61 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			at := sealMonth(9).Add(14 * 24 * time.Hour)
			db.now = func() time.Time { return at }
			merged := at.Add(-tc.age)
			_, err := db.InsertOutcome(context.Background(), Outcome{Developer: "alice", IssueID: "i-1", Weight: 1, Quality: 1, Timestamp: merged})
			if err != nil {
				t.Fatal(err)
			}
			r := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july")
			grace := int64(14 * 24 * time.Hour)
			r.GraceNS = &grace
			got, _, err := db.SealReport(context.Background(), r, nil, nil, SealCheck{Floor: testSealFloor, LatestOutcomeAt: merged})
			if err != nil {
				t.Fatal(err)
			}
			if got.RevertWindowsOpen == nil || *got.RevertWindowsOpen != tc.want || got.GraceNS == nil || *got.GraceNS != grace || !got.SealedAt.Equal(at) {
				t.Fatalf("wrong sealed disclosure: %+v", got)
			}
			// Late evidence and a later grace setting must never overwrite a prior seal.
			db.now = func() time.Time { return at.Add(90 * 24 * time.Hour) }
			longer := int64(90 * 24 * time.Hour)
			r.GraceNS = &longer
			again, won, err := db.SealReport(context.Background(), r, nil, nil, SealCheck{Floor: testSealFloor, LatestOutcomeAt: merged})
			if err != nil || won || *again.GraceNS != grace || *again.RevertWindowsOpen != tc.want {
				t.Fatalf("seal changed: %+v %v %v", again, won, err)
			}
		})
	}
}

func TestSealDisclosureUpgradePreservesLegacySeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before := sealOne(t, db)
	for _, col := range []string{"grace_ns", "revert_windows_open"} {
		if _, err := db.db.Exec(`ALTER TABLE sealed_report DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	after, err := db.SealedReport(context.Background(), "month", sealMonth(7))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Body, after.Body) || before.BodyDigest != after.BodyDigest || before.ConfigDigest != after.ConfigDigest || !before.SealedAt.Equal(after.SealedAt) || after.GraceNS != nil || after.RevertWindowsOpen != nil {
		t.Fatalf("legacy seal changed during migration: before=%+v after=%+v", before, after)
	}
}

func TestSealDisclosureDoesNotUseOutcomesOutsideBodySnapshot(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	at := sealMonth(9).Add(14 * 24 * time.Hour)
	db.now = func() time.Time { return at }
	// This outcome arrived after the caller's body snapshot. Its window is open,
	// but the body only included July 1 outcomes, whose windows are closed.
	if _, err := db.InsertOutcome(context.Background(), Outcome{Developer: "alice", IssueID: "late", Weight: 1, Quality: 1, Timestamp: sealMonth(8).Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "july")
	grace := int64(14 * 24 * time.Hour)
	r.GraceNS = &grace
	got, _, err := db.SealReport(context.Background(), r, nil, nil, SealCheck{Floor: testSealFloor, LatestOutcomeAt: sealMonth(7)})
	if err != nil {
		t.Fatal(err)
	}
	if got.RevertWindowsOpen == nil || *got.RevertWindowsOpen {
		t.Fatal("late outcome invented an open-window statement for the sealed body")
	}
}
