package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tiermetric/tier/internal/store"
	"io"
	"os"
)

func runCostConsistency(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("doctor cost-consistency", flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("db", "", "database path (required)")
	version := fs.Int("version", 0, "exact original version for repair")
	originalTable := fs.String("original-table", "", "explicit original YAML table; must match its unique registry identity")
	originalRule := fs.String("original-rule", "", "retained original rule implementation, required with --original-table")
	dry := fs.Bool("dry-run", false, "preview original-basis repair")
	apply := fs.Bool("apply", false, "apply original-basis repair")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*originalTable != "" && ((!*dry && !*apply) || *originalRule == "")) || (*originalTable == "" && *originalRule != "") || *path == "" || fs.NArg() != 0 || (*dry && *apply) || ((*dry || *apply) && *version < 1) {
		_, _ = fmt.Fprintln(errOut, "require --db; repairs require --version and one of --dry-run/--apply")
		return 2
	}
	if _, err := os.Stat(*path); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 2
	}
	db, err := store.Open(*path)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	defer func() { _ = db.Close() }()
	var rows []store.CostConsistencyRow
	if *dry || *apply {
		var evidence []store.OriginalBasisEvidence
		if *originalTable != "" {
			raw, readErr := os.ReadFile(*originalTable)
			if readErr != nil {
				_, _ = fmt.Fprintln(errOut, readErr)
				return 1
			}
			evidence = append(evidence, store.OriginalBasisEvidence{Table: raw, Rule: *originalRule})
		}
		rows, err = db.RepairPricingBasis(context.Background(), *version, *apply, evidence...)
	} else {
		rows, err = db.CostConsistency(context.Background())
	}
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	var increase, decrease int64
	for _, r := range rows {
		if r.Status == "inconsistent" && r.Class != "sealed_cost_mismatch" && r.Class != "poller_cost_mismatch" {
			delta := r.ExpectedMicro - r.StoredMicro
			if delta > 0 {
				increase += delta
			} else {
				decrease -= delta
			}
		}
	}
	if err := json.NewEncoder(out).Encode(struct {
		Rows                                   []store.CostConsistencyRow
		GrossIncreaseMicro, GrossDecreaseMicro int64
		Applied                                bool
	}{rows, increase, decrease, *apply}); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}
