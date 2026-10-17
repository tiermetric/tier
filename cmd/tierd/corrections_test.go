package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/store"
)

func TestCorrectionsCLI_ConfirmRequiresReviewedSizing(t *testing.T) {
	var out, errs bytes.Buffer
	code := dispatch([]string{"corrections", "confirm", "--relation", "1", "--revision", "1", "--actor", "operator-login", "--role", "operator"}, &out, &errs)
	if code == 0 || !strings.Contains(errs.String(), "review") {
		t.Fatal(code, errs.String())
	}
}
func TestCorrectionsCLI_ReviewedConfirmAndSelfAssertedActor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		at := time.Date(2026, 10, i, 0, 0, 0, 0, time.UTC)
		o := store.Outcome{Developer: []string{"", "original", "fixer"}[i], Repo: "acme/app", PRNumber: i, IssueID: fmt.Sprint(i), Weight: 5, Quality: 1, MergedAt: &at, Timestamp: at, MergeCommitSHA: fmt.Sprint(i)}
		if i == 2 {
			o.Weight = 8
		}
		a, b, c := 10, 0, 1
		o.SetDiffStats(&a, &b, &c)
		if _, err = db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
		body := "Corrects: none"
		if i == 2 {
			body = "Corrects: #1"
		}
		if _, err = db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "scope", "test"); err != nil {
			t.Fatal(err)
		}
	}
	child, _, err := db.OutcomeByMergeCommit(ctx, "2")
	if err != nil {
		t.Fatal(err)
	}
	rels, err := db.CorrectionRelationsByOutcome(ctx, child.ID)
	if err != nil || len(rels) != 1 {
		t.Fatal(rels, err)
	}
	args := []string{"corrections", "confirm", "--db", path, "--relation", fmt.Sprint(rels[0]["id"]), "--revision", "1", "--actor", "operator-login", "--role", "operator", "--reviewed-original", "5", "--reviewed-own", "1", "--pure-repair", "--scope", "original scope", "--rationale", "repair only", "--reviewer", "operator-login", "--reviewed-at", "2026-10-03T01:02:03Z"}
	var out, errs bytes.Buffer
	for _, name := range []string{"--reviewed-before", "--reviewed-after"} {
		invalid := append(append([]string(nil), args...), name, "not-a-rational")
		if code := dispatch(invalid, &out, &errs); code == 0 || !strings.Contains(errs.String(), "invalid reviewed rational") {
			t.Fatal("invalid rational accepted", code, errs.String())
		}
		out.Reset()
		errs.Reset()
	}
	if code := dispatch(args, &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	if !strings.Contains(out.String(), "self-asserted") || !strings.Contains(out.String(), `"allocated_points":"13/4"`) || !strings.Contains(out.String(), `"raw_weight":"8/1"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	errs.Reset()
	if code := dispatch(args, &out, &errs); code != 0 {
		t.Fatal("replay", code, errs.String())
	}
	// A different review time cannot reuse the confirmed revision.
	args[len(args)-1] = "2026-10-04T01:02:03Z"
	if code := dispatch(args, &out, &errs); code == 0 {
		t.Fatal("conflicting replay accepted")
	}
}

func TestCorrectionsCLI_UsageStartsWithHeader(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	if !strings.HasPrefix(out.String(), "usage:") || !strings.Contains(out.String(), "  corrections ") {
		t.Fatal(out.String())
	}
}
func TestCorrectionsCLI_PrintsUpgradeNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawExec(t, path, `PRAGMA user_version=9`)
	for i := 0; i < 2; i++ {
		var out, errs bytes.Buffer
		if code := dispatch([]string{"corrections", "list", "--db", path}, &out, &errs); code != 0 {
			t.Fatal(code, errs.String())
		}
		if got := strings.Contains(errs.String(), store.UpgradeNoticeCorrectionProjection); got != (i == 0) {
			t.Fatal("upgrade notice", i, errs.String())
		}
	}
}

// correctionDispatchFixture uses captured outcomes and declarations, then exercises
// lifecycle transitions only through the top-level CLI dispatcher.
func correctionDispatchFixture(t *testing.T) (*store.DB, string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	var child store.Outcome
	for i := 1; i <= 2; i++ {
		at := time.Date(2026, 10, i, 0, 0, 0, 0, time.UTC)
		o := store.Outcome{Developer: []string{"", "original", "fixer"}[i], Repo: "acme/app", PRNumber: i, IssueID: fmt.Sprint(i), Weight: 5, Quality: 1, Timestamp: at, MergedAt: &at, MergeCommitSHA: fmt.Sprint(i)}
		if i == 2 {
			o.Weight = 1
		}
		a, b, c := 10, 0, 1
		o.SetDiffStats(&a, &b, &c)
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
		body := "Corrects: none"
		if i == 2 {
			body = "Corrects: #1"
		}
		if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "reviewed scope", "test"); err != nil {
			t.Fatal(err)
		}
	}
	child, _, err = db.OutcomeByMergeCommit(ctx, "2")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	rels, err := db.CorrectionRelationsByOutcome(ctx, child.ID)
	if err != nil || len(rels) != 1 {
		t.Fatal(rels, err)
	}
	return db, path, rels[0]["id"].(int64)
}

func correctionDispatchReview(own string) []string {
	return []string{"--reviewed-original", "5", "--reviewed-own", own, "--pure-repair", "--scope", "reviewed original scope", "--rationale", "repair only", "--reviewer", "operator-login", "--reviewed-at", "2026-10-03T01:02:03Z"}
}

func correctionDispatch(t *testing.T, path string, id int64, revision int, command string, extra ...string) {
	t.Helper()
	args := []string{"corrections", command, "--db", path, "--relation", fmt.Sprint(id), "--revision", fmt.Sprint(revision), "--actor", "operator-login", "--role", "operator", "--reason", "reviewed disposition"}
	args = append(args, extra...)
	var out, errs bytes.Buffer
	if code := dispatch(args, &out, &errs); code != 0 {
		t.Fatalf("%s exit %d: %s", command, code, errs.String())
	}
}

func assertCorrectionDispatch(t *testing.T, db *store.DB, id int64, state string, revision int64, parent, child, action string) {
	t.Helper()
	detail, err := db.CorrectionDetail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	rel := detail["relations"].([]map[string]any)[0]
	if rel["state"] != state || rel["revision"] != revision {
		t.Fatalf("persisted relation=%v, want %s revision %d", rel, state, revision)
	}
	rows := detail["allocations"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("allocations=%v", rows)
	}
	for _, row := range rows {
		want := parent
		if row["outcome_id"] == rel["child_outcome_id"] {
			want = child
		}
		if row["allocated_points"] != want {
			t.Fatalf("allocation=%v, want %s", row, want)
		}
	}
	audit := detail["audit"].([]map[string]any)
	if len(audit) == 0 || audit[len(audit)-1]["action"] != action {
		t.Fatalf("audit=%v, want latest %s", audit, action)
	}
}

func TestCorrectionsCLI_DispatchDisputeWithdrawSettle(t *testing.T) {
	t.Run("dispute-declared", func(t *testing.T) {
		db, path, id := correctionDispatchFixture(t)
		correctionDispatch(t, path, id, 1, "dispute")
		assertCorrectionDispatch(t, db, id, "disputed-declared", 2, "5/1", "1/1", "dispute")
	})
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("withdraw-confirmed=%t", confirmed), func(t *testing.T) {
			db, path, id := correctionDispatchFixture(t)
			revision := 1
			wantState, wantParent := "withdrawn", "5/1"
			if confirmed {
				correctionDispatch(t, path, id, revision, "confirm", correctionDispatchReview("1")...)
				revision++
				wantState, wantParent = "withdrawn-confirmed", "13/4"
			}
			correctionDispatch(t, path, id, revision, "withdraw")
			// D3: a confirmed withdrawal stays held until operator settlement.
			assertCorrectionDispatch(t, db, id, wantState, int64(revision+1), wantParent, "1/1", "withdraw")
		})
	}
	for _, settlement := range []string{"uphold", "remove", "re-assess"} {
		t.Run("settle-"+settlement, func(t *testing.T) {
			db, path, id := correctionDispatchFixture(t)
			correctionDispatch(t, path, id, 1, "confirm", correctionDispatchReview("1")...)
			correctionDispatch(t, path, id, 2, "dispute")
			assertCorrectionDispatch(t, db, id, "disputed", 3, "13/4", "1/1", "dispute")
			extra := []string{"--settlement", settlement}
			state, parent, child := "confirmed", "13/4", "1/1"
			switch settlement {
			case "remove":
				state, parent = "removed", "5/1"
			case "re-assess":
				extra = append(extra, correctionDispatchReview("3")...)
				parent, child = "5/4", "3/1"
			}
			correctionDispatch(t, path, id, 3, "settle", extra...)
			assertCorrectionDispatch(t, db, id, state, 4, parent, child, "settle-"+settlement)
		})
	}
}

func TestCorrectionsCLI_RejectsReviewFlagsOutsideReviewCommands(t *testing.T) {
	flags := []string{"reviewed-original", "reviewed-own", "reviewed-before", "reviewed-after", "pure-repair", "scope", "rationale", "reviewer", "reviewed-at", "comparison-base", "comparison-before", "comparison-after"}
	for _, command := range []string{"dispute", "withdraw", "list", "show", "settle-uphold", "settle-remove"} {
		for _, name := range flags {
			t.Run(command+"/"+name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "must-not-open.db")
				verb := command
				var extra []string
				if strings.HasPrefix(command, "settle-") {
					verb = "settle"
					extra = []string{"--settlement", strings.TrimPrefix(command, "settle-")}
				}
				args := []string{"corrections", verb, "--db", path, "--relation", "1", "--revision", "1", "--actor", "operator-login", "--role", "operator", "--reason", "review"}
				args = append(args, extra...)
				value := "1"
				if name == "pure-repair" {
					value = "false"
				}
				args = append(args, "--"+name+"="+value)
				var out, errs bytes.Buffer
				if code := dispatch(args, &out, &errs); code == 0 || !strings.Contains(errs.String(), "reviewed sizing is accepted only for confirmation or re-assessment") {
					t.Fatalf("exit %d: %s", code, errs.String())
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("unexpected database open: %v", err)
				}
			})
		}
	}
}

func TestCorrectionsCLI_CursorNormalizesUTC(t *testing.T) {
	db, path, _ := correctionDispatchFixture(t)
	rows, _, err := db.ListCorrectionEvidence(context.Background(), "relations", time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 1)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	at := rows[0]["recorded_at"].(time.Time)
	var baseline string
	for i, cursor := range []time.Time{at.UTC(), at.In(time.FixedZone("east", 14*60*60)), at.In(time.FixedZone("west", -12*60*60))} {
		var out, errs bytes.Buffer
		args := []string{"corrections", "list", "--db", path, "--cursor", cursor.Format(time.RFC3339Nano), "--cursor-id", fmt.Sprint(rows[0]["id"])}
		if code := dispatch(args, &out, &errs); code != 0 {
			t.Fatal(code, errs.String())
		}
		if i == 0 {
			baseline = out.String()
			if !strings.Contains(baseline, `"relations":null`) {
				t.Fatal(baseline)
			}
		} else if out.String() != baseline {
			t.Fatalf("equivalent cursor changed page:\nUTC %s\noffset %s", baseline, out.String())
		}
	}
}
