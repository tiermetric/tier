package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func TestRecoveryReceiptCLIReadOnlyJSONAndHuman(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	e := store.TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: store.UnattributedIssueID, CostMicro: 7, Source: "opencode", Fidelity: "realtime", Timestamp: at}
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	e.Source = "codex-rollout"
	e.CostMicro = 13
	e.BranchHash = issueref.BranchHash("acme/a", "feature/topic")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	opened, merged := at.Add(-time.Hour), at.Add(time.Hour)
	o := store.Outcome{Developer: "alice", Repo: "acme/a", IssueID: "issue-42", PRNumber: 42, Weight: 4, Quality: 1, Source: "backfill", HeadBranchHash: e.BranchHash, CreatedAt: &opened, MergedAt: &merged, Timestamp: merged}
	if _, err := db.InsertOutcome(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"attribution", "recovery-receipt", "--db", path, "--since", "2026-06-01", "--until", "2026-07-01", "--repo", "Acme/A.git"}
	var out, errs bytes.Buffer
	if code := dispatch(append(append([]string{}, base...), "--json"), &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	var receipt store.RecoveryReceipt
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Events != nil || strings.Contains(out.String(), `"developer"`) || strings.Contains(out.String(), `"events": [`) {
		t.Fatal("default JSON disclosed events", out.String())
	}
	out.Reset()
	errs.Reset()
	if code := dispatch(append(append([]string{}, base...), "--json", "--events"), &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Events) != 2 || receipt.Events[0].Developer != "alice" {
		t.Fatal(receipt)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if receipt.Candidates.Events != 2 || receipt.Candidates.CostMicro != 20 || receipt.Dispositions["unique_read_time_recovery"].CostMicro != 13 || receipt.Dispositions["unresolved"].CostMicro != 7 {
		t.Fatal(receipt)
	}
	out.Reset()
	errs.Reset()
	if code := dispatch(base, &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	for _, needle := range []string{"repo: acme/a", "claimant_without_issue:", "20 micro-dollars", "unique_read_time_recovery: 1 events, 13 micro-dollars", "sessions_jsonl_gone: unmeasured", "precision: not measured"} {
		if !strings.Contains(out.String(), needle) {
			t.Fatal(out.String(), needle)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("receipt modified database bytes")
	}
	for _, args := range [][]string{{}, {"recovery-receipt"}, {"recovery-receipt", "--since", "invalid"}, {"recovery-receipt", "--since", "2026-06-01", "--until", "2026-05-01"}, {"recovery-receipt", "--since", "2026-06-01", "--repo", "unqualified"}, {"recovery-receipt", "--since", "2026-06-01", "extra"}} {
		out.Reset()
		errs.Reset()
		if runAttribution(args, &out, &errs) == 0 || out.Len() != 0 {
			t.Fatal(args, out.String(), errs.String())
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	out.Reset()
	errs.Reset()
	if runAttribution([]string{"recovery-receipt", "--db", missing, "--since", "2026-06-01"}, &out, &errs) == 0 {
		t.Fatal("created missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
