package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

func runAttribution(args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "attribution: %s\n", logsafe.Str(err.Error()))
		return 1
	}
	if len(args) == 0 || args[0] != "recovery-receipt" {
		return fail(fmt.Errorf("usage: tierd attribution recovery-receipt --since YYYY-MM-DD [--until YYYY-MM-DD] [--repo owner/repo] [--json] [--events] [--db path]"))
	}
	fs := flag.NewFlagSet("attribution recovery-receipt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("db", defaultDBPath(), "existing SQLite database (read-only; no migration)")
	sinceArg := fs.String("since", "", "required inclusive date YYYY-MM-DD")
	untilArg := fs.String("until", "", "exclusive date YYYY-MM-DD")
	repo := fs.String("repo", "", "exact owner/repo scope")
	asJSON := fs.Bool("json", false, "JSON receipt")
	events := fs.Bool("events", false, "include per-event rows in JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return flagParseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(fmt.Errorf("unexpected positional arguments"))
	}
	since, err := time.Parse("2006-01-02", *sinceArg)
	if err != nil {
		return fail(fmt.Errorf("--since must be YYYY-MM-DD"))
	}
	var until time.Time
	if *untilArg != "" {
		until, err = time.Parse("2006-01-02", *untilArg)
		if err != nil || !until.After(since) {
			return fail(fmt.Errorf("--until must be YYYY-MM-DD after --since"))
		}
	}
	scope := store.FleetWide
	if *repo != "" {
		canonical, ok := repoid.Canonical(*repo)
		if !ok {
			return fail(fmt.Errorf("invalid --repo"))
		}
		scope = store.RepoScope(canonical)
	}
	db, err := store.OpenReadOnly(*path)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = db.Close() }()
	receipt, err := db.RecoveryReceipt(context.Background(), since, until, scope)
	if err != nil {
		return fail(err)
	}
	if !*events {
		receipt.Events = nil
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(receipt); err != nil {
			return fail(err)
		}
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "Recovery receipt since %s (until exclusive: %s; repo: %s)\n%s\n", since.Format("2006-01-02"), *untilArg, receipt.Repo, receipt.Population)
	_, _ = fmt.Fprintf(stdout, "Candidates: %d events, %d micro-dollars; assigned reference: %d events, %d micro-dollars\n", receipt.Candidates.Events, receipt.Candidates.CostMicro, receipt.AssignedReference.Events, receipt.AssignedReference.CostMicro)
	for _, name := range store.RecoveryDispositions() {
		v := receipt.Dispositions[name]
		_, _ = fmt.Fprintf(stdout, "%s: %d events, %d micro-dollars\n", name, v.Events, v.CostMicro)
	}
	_, _ = fmt.Fprintf(stdout, "Recovered candidate cost share: %.6f; precision: %s\nOverlapping reason tags (not terminal totals):\n", receipt.RecoveredCostShare, receipt.Precision)
	for _, name := range store.RecoveryReasonTags() {
		v := receipt.Reasons[name]
		if !v.Measured {
			_, _ = fmt.Fprintf(stdout, "%s: unmeasured\n", name)
		} else {
			_, _ = fmt.Fprintf(stdout, "%s: %d events, %d micro-dollars\n", name, v.Events, v.CostMicro)
		}
	}
	_, _ = fmt.Fprintln(stdout, receipt.EvidenceLimit)
	return 0
}
