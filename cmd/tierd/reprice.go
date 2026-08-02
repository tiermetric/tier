package main

// tierd reprice (#294): the sanctioned, audited path to recompute historical
// token_events costs after the price table changes.
//
// THE PROBLEM IT SOLVES: since #233 the server is the single pricing authority and
// a stored cost_micro is IMMUTABLE on the normal replay path — a re-ship of the
// same message never reprices it. That immutability is what stops a mixed-version
// fleet from silently ratcheting history, but it also means a genuine pricing
// correction (a fixed rate, a newly-seeded host rate, or the placeholder-promotion
// edge where a row's cost froze under-priced) can NEVER reach already-stored rows
// through ingestion. `tierd reprice` is the ONLY sanctioned mutator of a priced
// cost: it recomputes cost_micro for rows priced at price_version >= N using the
// CURRENT table, bumps them to the active version, and records an audit row.
//
// This RETROACTIVELY changes historical TIER scores, so it is SAFE BY DEFAULT:
//   - Dry run unless --commit. Bare `tierd reprice --from-version N` computes and
//     prints exactly what WOULD change (rows, before/after cost, affected
//     developers/versions) and writes NOTHING.
//   - --commit applies the change in a SINGLE transaction (all per-row updates,
//     the per-row before-images, AND the aggregate audit ledger commit together or
//     not at all — no partial reprice).
//   - --from-version is REQUIRED, so the whole table is never repriced by accident.
//   - A --commit that would rewrite real historical cost to a size-class GUESS
//     estimate (a model not in the active table) is BLOCKED unless --allow-guessed
//     is passed — repricing audited history to an estimate must be opted into.
//   - Every committed run writes two ledgers: reprice_audit (one row per affected
//     price_version — the per-version aggregate shift) and reprice_row_audit (one
//     before-image per changed row: its exact old cost/version/billing_mode).
//
// OPERATIONAL NOTES:
//   - The per-row reprice_row_audit before-images make a reprice reversible at row
//     grain in principle (the inverse-undo command is future work — see the
//     reprice_row_audit schema comment in store.go). A `tierd backup` snapshot
//     before committing is still prudent belt-and-braces, especially for a large
//     first-time correction.
//   - Run against a QUIESCED database. A committed reprice applies every changed
//     row inside one SQLite write transaction; on the single-writer store that
//     holds the write lock for the duration and will contend with a live
//     `tierd serve` ingestion path. Prefer an offline copy or a maintenance window
//     for a large first-time correction.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tiermetric/tier/internal/store"
)

// runRepriceCmd parses flags, applies any --prices override, opens the store, and
// runs the reprice, returning the process exit code (0 clean, 1 fatal) so main's
// os.Exit stays the single exit point and the deferred db.Close always runs.
// Output goes to the injected writers so the subcommand is testable through
// dispatch (mirrors backfill/doctor/backup).
func runRepriceCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reprice", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path to reprice")
	// Default 0 is the "unset" sentinel: valid values are >= 1, so 0 unambiguously
	// means the operator omitted the (required) flag. Requiring it is the guard
	// against repricing the whole table by accident.
	fromVersion := fs.Int("from-version", 0, "reprice every token_events row with price_version >= N against the CURRENT table (required, >= 1)")
	commit := fs.Bool("commit", false, "apply the reprice (default: DRY RUN — print what would change and write nothing)")
	// A --commit that would rewrite real historical cost to a size-class GUESS
	// estimate (a model not in the active table) is BLOCKED unless the operator
	// opts in here. Dry run always just reports+warns; this gate only bites commit.
	allowGuessed := fs.Bool("allow-guessed", false, "permit --commit to rewrite audited history to a self-hosted GUESS estimate for models not in the active price table (default: --commit refuses when any changed row would be guessed)")
	pricesPath := fs.String("prices", os.Getenv("TIER_PRICES"), "path to a price-table YAML override (#68); empty uses the embedded default. The reprice recomputes costs against THIS table")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *fromVersion < 1 {
		_, _ = fmt.Fprintln(stderr, "reprice: --from-version is required and must be >= 1 (the price_version floor to reprice from; refusing to reprice the whole table by accident)")
		return 1
	}

	// Reprice operates on an EXISTING database (dry run and commit alike) — refuse a
	// path that does not exist rather than let store.Open create an empty DB and
	// silently report "0 rows" for a typo'd --db. A missing file is almost certainly
	// operator error, not an intent to reprice nothing.
	if _, err := os.Stat(*dbPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "reprice: --db %s: %v (reprice operates on an existing database)\n", *dbPath, err)
		return 1
	}

	// Apply a --prices override BEFORE opening the store so the reprice recomputes
	// against the intended table (ActivePriceTableInfo is what store.Reprice reads).
	// A bad file fails the process loudly (never a silent fallback), matching score.
	if info, ok := loadPricesOverride(*pricesPath); ok {
		_, _ = fmt.Fprintf(stderr, "price table: override %s (version %d, %s, %d models)\n",
			*pricesPath, info.Version, info.EffectiveDate, info.ModelCount)
	} else {
		info := store.ActivePriceTableInfo()
		_, _ = fmt.Fprintf(stderr, "price table: embedded default (version %d, %s, %d models)\n",
			info.Version, info.EffectiveDate, info.ModelCount)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reprice: open db: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	// Cancel cleanly on SIGINT/SIGTERM. A cancelled commit rolls the transaction
	// back (store.Reprice is all-or-nothing), so an interrupted reprice never
	// leaves a partial mutation.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	res, err := db.Reprice(ctx, store.RepriceOptions{
		FromVersion:  *fromVersion,
		Commit:       *commit,
		AllowGuessed: *allowGuessed,
		ToolVersion:  versionString(),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reprice: %v\n", err)
		return 1
	}

	printRepriceResult(stdout, res)
	return 0
}

// printRepriceResult renders the reprice outcome — identical shape for a dry run
// and a commit, differing only in the header verb and the trailing line (a dry run
// tells the operator to re-run with --commit; a commit reports the audit id).
func printRepriceResult(w io.Writer, res store.RepriceResult) {
	verb := "DRY RUN"
	changeVerb := "would change"
	if res.Committed {
		verb = "COMMITTED"
		changeVerb = "changed"
	}
	_, _ = fmt.Fprintf(w, "reprice %s: from-version %d -> active table version %d (effective %s)\n",
		verb, res.FromVersion, res.ToVersion, res.EffectiveDate)
	_, _ = fmt.Fprintf(w, "  examined %d row(s) priced >= v%d; %d %s\n",
		res.RowCount, res.FromVersion, res.ChangedRowCount, changeVerb)

	// Distinguish "the floor matched no rows" (likely a mis-typed --from-version)
	// from "rows matched but were already correctly priced" — reporting the former
	// as the latter would mask an operator's typo as a successful no-op.
	if res.RowCount == 0 {
		_, _ = fmt.Fprintf(w, "  no rows have price_version >= v%d — nothing examined (check --from-version)\n", res.FromVersion)
		return
	}
	if res.ChangedRowCount == 0 {
		_, _ = fmt.Fprintln(w, "  no rows need repricing (already priced correctly under the active table) — nothing to do")
		return
	}

	delta := res.NewCostMicroSum - res.OldCostMicroSum
	_, _ = fmt.Fprintf(w, "  cost_micro over changed rows: %d -> %d (delta %+d, %+.6f USD)\n",
		res.OldCostMicroSum, res.NewCostMicroSum, delta, store.MicroToDollars(delta))
	_, _ = fmt.Fprintf(w, "  affected developers: %s\n", joinOrNone(res.Developers))
	_, _ = fmt.Fprintln(w, "  by old price_version:")
	for _, d := range res.ByOldVersion {
		vDelta := d.NewCostMicroSum - d.OldCostMicroSum
		_, _ = fmt.Fprintf(w, "    v%d -> v%d: %d row(s), cost_micro %d -> %d (delta %+d)\n",
			d.OldPriceVersion, d.NewPriceVersion, d.RowCount, d.OldCostMicroSum, d.NewCostMicroSum, vDelta)
	}

	// Loudly flag rows repriced by the self-hosted GUESS fallback: their model has
	// no exact entry in the active table, so this rewrites audited historical cost
	// to an ESTIMATE. Usually means a model was dropped from the table since the row
	// was priced — the operator should add it back before committing.
	if res.GuessedRowCount > 0 {
		// Name the exact models so the operator knows WHAT to add to the price table.
		override := ""
		if res.Committed {
			// A committed run with guessed rows necessarily passed --allow-guessed
			// (the gate blocks it otherwise) — record that the override was used.
			override = " — COMMITTED under --allow-guessed"
		}
		_, _ = fmt.Fprintf(w, "  WARNING: %d of %d changed row(s) priced at the self-hosted GUESS fallback (model(s) not in the active table: %s) — audited history would be rewritten to an ESTIMATE. Add the missing model(s) to the price table for an accurate reprice%s.\n",
			res.GuessedRowCount, res.ChangedRowCount, joinOrNone(res.GuessedModels), override)
	}

	if res.Committed {
		_, _ = fmt.Fprintf(w, "  audit: %d row(s) to reprice_audit + %d per-row before-image(s) to reprice_row_audit (reprice_id %s)\n",
			len(res.ByOldVersion), res.ChangedRowCount, res.RepriceID)
		return
	}
	_, _ = fmt.Fprintln(w, "  NOTHING was written. Re-run with --commit to apply this reprice.")
}

// joinOrNone renders a developer list for the report, collapsing an empty set to a
// readable placeholder rather than a blank line.
func joinOrNone(devs []string) string {
	if len(devs) == 0 {
		return "(none)"
	}
	return strings.Join(devs, ", ")
}
