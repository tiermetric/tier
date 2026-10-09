package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// skipMonth is the month after sealTestMonth: closed and past the default
// grace, and the first owed month once sealTestMonth is armed.
func skipMonth() string { return sealTestMonth().AddDate(0, 1, 0).Format("2006-01") }

// armedSealTestDB is sealTestDB with sealTestMonth armed (sealed and pinned).
func armedSealTestDB(t *testing.T) string {
	t.Helper()
	path := sealTestDB(t)
	if rc, out, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path); rc != 0 {
		t.Fatalf("arm: rc %d, stdout %q, stderr %q", rc, out, errOut)
	}
	return path
}

// gapState is the recorded gaps as "start category reason" ("" when none).
func gapState(t *testing.T, path string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var gaps string
	if err := raw.QueryRow(`SELECT COALESCE(group_concat(period_start || ' ' || category || ' ' || reason), '') FROM sealed_gap`).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	return gaps
}

// breakSealing makes every later seal fail for good: each sealed period gains
// a person row under a label with no rollup, which the in-transaction refold
// check rejects (an internal error).
func breakSealing(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`CREATE TRIGGER test_phantom_person AFTER INSERT ON sealed_report BEGIN
		INSERT INTO sealed_person (report_id, label, measure, person_key) VALUES (NEW.id, 'phantom', 'people', randomblob(32));
		END`); err != nil {
		t.Fatal(err)
	}
}

// TestSealSkipCmd_CleanRetrySealsNoGap: for a month that now seals, --dry-run
// exits 0 saying the seal is retried first and writes nothing; --skip exits 0,
// seals it and records no gap.
func TestSealSkipCmd_CleanRetrySealsNoGap(t *testing.T) {
	path := armedSealTestDB(t)
	armed, _ := sealState(t, path)
	base := []string{"--skip", skipMonth(), "--reason", "stuck", "--aggregation", "team", "--db", path}
	if rc, out, errOut := runSeal(t, "", append(base, "--dry-run")...); rc != 0 || !strings.Contains(out, "retried first") {
		t.Errorf("dry run: rc %d, stdout %q, stderr %q; want 0 saying the seal is retried first", rc, out, errOut)
	}
	if sealed, _ := sealState(t, path); sealed != armed || gapState(t, path) != "" {
		t.Fatalf("dry run left sealed %q, gaps %q; want %q and none", sealed, gapState(t, path), armed)
	}
	rc, out, errOut := runSeal(t, "", append(base, "--yes")...)
	if rc != 0 || !strings.Contains(out, "sealed "+skipMonth()+", no gap needed") {
		t.Errorf("skip: rc %d, stdout %q, stderr %q; want 0, sealed with no gap", rc, out, errOut)
	}
	want := armed + "," + sealTestMonth().AddDate(0, 1, 0).Format(time.RFC3339)
	if sealed, _ := sealState(t, path); sealed != want || gapState(t, path) != "" {
		t.Errorf("left sealed %q, gaps %q; want %q and none", sealed, gapState(t, path), want)
	}
}

// TestSealSkipCmd_RecordsGap: for a month whose seal fails for good, --dry-run
// and a declined prompt write nothing; before the prompt the retry's category
// and observed error are printed; a confirmed skip exits 0, records the gap
// with that category and the reason, and prints the error again; a second skip
// of it exits 1.
func TestSealSkipCmd_RecordsGap(t *testing.T) {
	path := armedSealTestDB(t)
	armed, _ := sealState(t, path)
	breakSealing(t, path)
	base := []string{"--skip", skipMonth(), "--reason", "phantom rows (OPS-7)", "--aggregation", "team", "--db", path}
	const observed = `has no rollup`
	if rc, _, errOut := runSeal(t, "", append(base, "--dry-run")...); rc != 0 {
		t.Errorf("dry run: rc %d, stderr %q; want 0", rc, errOut)
	}
	if rc, _, errOut := runSeal(t, "n\n", base...); rc != 1 || !strings.Contains(errOut, "not confirmed") || !strings.Contains(errOut, "no gap was recorded") {
		t.Errorf("declined: rc %d, stderr %q; want 1, not confirmed", rc, errOut)
	}
	if sealed, _ := sealState(t, path); sealed != armed || gapState(t, path) != "" {
		t.Fatalf("dry run and decline left sealed %q, gaps %q; want %q and none", sealed, gapState(t, path), armed)
	}
	// The answer is read at the prompt, so atPrompt is what the operator saw
	// before confirming.
	var out, errOut bytes.Buffer
	var atPrompt string
	answer := readerFunc(func(p []byte) (int, error) {
		atPrompt = out.String()
		return copy(p, "y\n"), io.EOF
	})
	rc := runSealCmd(base, answer, &out, &errOut)
	_, after, _ := strings.Cut(out.String(), atPrompt)
	if rc != 0 || !strings.Contains(after, "recorded "+skipMonth()+" as a permanent gap (category internal) after its seal failed: ") || !strings.Contains(after, observed) {
		t.Fatalf("skip: rc %d, stdout %q, stderr %q; want 0 recording the gap and printing the observed error", rc, out.String(), errOut.String())
	}
	if !strings.Contains(atPrompt, "(category internal): ") || !strings.Contains(atPrompt, observed) {
		t.Errorf("before the prompt: %q; want the category and the observed error", atPrompt)
	}
	wantGap := sealTestMonth().AddDate(0, 1, 0).Format(time.RFC3339) + " internal phantom rows (OPS-7)"
	if sealed, _ := sealState(t, path); sealed != armed || gapState(t, path) != wantGap {
		t.Errorf("left sealed %q, gaps %q; want %q and %q", sealed, gapState(t, path), armed, wantGap)
	}
	if rc, _, errOut := runSeal(t, "", append(base, "--yes")...); rc != 1 || !strings.Contains(errOut, "is recorded as a gap already") {
		t.Errorf("second skip: rc %d, stderr %q; want 1", rc, errOut)
	}
}

// TestSealSkipCmd_Refusals: each precondition exits 1, says why, and records
// no gap.
func TestSealSkipCmd_Refusals(t *testing.T) {
	now := time.Now().UTC()
	current := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
	unarmed := sealTestDB(t)
	armed := armedSealTestDB(t)
	for _, c := range []struct {
		name, db string
		args     []string
		want     string
	}{
		{"no reason", armed, []string{"--skip", skipMonth()}, "--skip needs --reason"},
		{"arm and skip", armed, []string{"--skip", skipMonth(), "--arm", "earliest", "--reason", "r"}, "cannot be given together"},
		{"reason with arm", armed, []string{"--arm", "earliest", "--reason", "r"}, "--reason is given only with --skip"},
		{"reason with a line break", armed, []string{"--skip", skipMonth(), "--reason", "a\nb"}, "printable text"},
		{"developer mode", armed, []string{"--skip", skipMonth(), "--reason", "r", "--aggregation", "developer"}, "developer mode"},
		{"read-only", armed, []string{"--skip", skipMonth(), "--reason", "r", "--read-only"}, "--read-only"},
		{"not armed", unarmed, []string{"--skip", skipMonth(), "--reason", "r"}, "sealing not armed"},
		{"sealed month", armed, []string{"--skip", sealTestMonth().Format("2006-01"), "--reason", "r"}, "is sealed already"},
		{"open month", armed, []string{"--skip", current, "--reason", "r"}, current + " is open or inside its grace lag"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"--yes", "--aggregation", "team", "--db", c.db}, c.args...)
			if rc, out, errOut := runSeal(t, "", args...); rc != 1 || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, "refused") {
				t.Errorf("rc %d, stdout %q, stderr %q; want 1 naming %q", rc, out, errOut, c.want)
			}
			if g := gapState(t, c.db); g != "" {
				t.Errorf("recorded gaps %q", g)
			}
		})
	}
}

// TestSealSkipCmd_TransientExitsTwo: a retry that cannot take the write lock
// exits 2 and records no gap.
func TestSealSkipCmd_TransientExitsTwo(t *testing.T) {
	path := armedSealTestDB(t)
	breakSealing(t, path)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The answer takes the write lock after the plan, so the retry cannot.
	lockThenAnswer := readerFunc(func(p []byte) (int, error) {
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Error(err)
		}
		return copy(p, "y\n"), io.EOF
	})
	var out, errOut bytes.Buffer
	rc := runSealCmd([]string{"--skip", skipMonth(), "--reason", "r", "--aggregation", "team", "--db", path}, lockThenAnswer, &out, &errOut)
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if rc != 2 || !strings.Contains(errOut.String(), "could not run") || !strings.Contains(errOut.String(), "no gap was recorded") {
		t.Errorf("write lock held: rc %d, stderr %q; want 2", rc, errOut.String())
	}
	if g := gapState(t, path); g != "" {
		t.Errorf("recorded gaps %q, want none", g)
	}
}

// cancelOnGapDB's RecordSealedGap writes the gap and then cancels, as a SIGINT
// landing after the write does.
type cancelOnGapDB struct {
	*store.DB
	cancel context.CancelFunc
}

func (d cancelOnGapDB) RecordSealedGap(ctx context.Context, g store.SealedGap) (store.SealedGap, error) {
	defer d.cancel()
	return d.DB.RecordSealedGap(ctx, g)
}

// TestSealSkipCmd_CancelAfterGapWriteExitsZero: a cancellation that lands after
// the gap is written exits 0 printing the recorded gap, never 2 with "no gap
// was recorded".
func TestSealSkipCmd_CancelAfterGapWriteExitsZero(t *testing.T) {
	path := armedSealTestDB(t)
	breakSealing(t, path)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := api.New(cancelOnGapDB{db, cancel}, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{})
	h.SetAggregation(scoring.AggregationTeam, scoring.DefaultKAnonymity)
	settings := sealSettings{mode: scoring.AggregationTeam, k: scoring.DefaultKAnonymity, grace: defaultReportGrace}
	var out, errOut bytes.Buffer
	exit := func(rc int) func(string, ...any) int {
		return func(format string, a ...any) int { _, _ = fmt.Fprintf(&errOut, format+"\n", a...); return rc }
	}
	rc := runSealSkip(ctx, h, settings, skipMonth(), "r", false, true, strings.NewReader(""), &out, exit(sealExitRefused), exit(sealExitCannotRun))
	if rc != 0 || !strings.Contains(out.String(), "skipped: recorded "+skipMonth()+" as a permanent gap") || ctx.Err() == nil {
		t.Errorf("rc %d, stdout %q, stderr %q (ctx %v); want 0 printing the recorded gap", rc, out.String(), errOut.String(), ctx.Err())
	}
	if g := gapState(t, path); !strings.HasSuffix(g, " internal r") {
		t.Errorf("recorded gaps %q, want the one internal gap", g)
	}
}
