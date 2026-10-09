package main

// tierd prices (#714): operator commands for the price-table registry — the
// table that binds each `version:` integer to the #713 content identity of the
// price table actually served under it.
//
// TWO subcommands, and the pairing is deliberate:
//
//   - `list` READS the registry. A guard you cannot inspect is a guard you can
//     only escape: without this, the only way to see what a version was bound to
//     was a dry run of the DELETE command, which is a poor thing to reach for
//     while diagnosing a refused startup. It also lets an operator check a
//     database BEFORE upgrading rather than discovering a collision when the
//     daemon will not come up.
//   - `forget-version` is THE DOCUMENTED REMEDY for the fail-closed collision
//     refusal in store.Open. A guard whose only escape is hand-editing production
//     SQLite is a guard that gets commented out the first time it fires out of
//     hours. See store.ForgetPriceTableVersion for why this is emphatically NOT
//     the same thing as a `--accept-rehash` startup flag, why such a flag must
//     never be added, and the honest limit of that argument.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

// runPricesCmd routes `tierd prices <subcommand>` and returns the process exit
// code. Output goes to the injected writers so the whole command is testable
// through dispatch (mirrors backfill/doctor/backup/reprice).
func runPricesCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		printPricesUsage(stderr)
		return 1
	}
	switch args[0] {
	case "list":
		return runPricesList(args[1:], stdout, stderr)
	case "audit":
		return runPricesAudit(args[1:], stdout, stderr)
	case "forget-version":
		return runPricesForgetVersion(args[1:], stdout, stderr)
	case "help", "--help", "-help", "-h":
		printPricesUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown prices subcommand: %s\n", logsafe.Str(args[0]))
		printPricesUsage(stderr)
		return 1
	}
}

// printPricesUsage prints a WORKING invocation for each subcommand, not just the
// names. A usage block that lists `forget-version` without its required flags
// leaves the operator to guess the syntax — and getting that syntax wrong is
// exactly the defect four reviews caught in the refusal message this command
// exists to answer.
func printPricesUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: tierd prices <subcommand> [flags]")
	_, _ = fmt.Fprintln(w, "  list            show every price-table identity recorded in a database (#714)")
	_, _ = fmt.Fprintln(w, "                  tierd prices list --db /var/lib/tier/tier.db [--json]")
	_, _ = fmt.Fprintln(w, "  audit           the append-only ledger of every retired identity (#714)")
	_, _ = fmt.Fprintln(w, "                  tierd prices audit --db /var/lib/tier/tier.db [--json]")
	_, _ = fmt.Fprintln(w, "  forget-version  RETIRE one recorded price-table identity (dry run unless --commit)")
	_, _ = fmt.Fprintln(w, "                  the row is kept and stamped, never deleted — see docs/reference-price-table.md §9")
	_, _ = fmt.Fprintln(w, "                  tierd prices forget-version --db /var/lib/tier/tier.db --version 9 [--actor NAME] [--json] [--commit]")
}

// rejectTrailingArgs refuses leftover positional arguments.
//
// 🔑 IT EXISTS BECAUSE OF THE PASTED-REMEDY CASE. Go's flag package stops parsing
// at the first non-flag argument, so `forget-version 9 --commit` silently
// discards --commit and does a dry run — the direction is fail-safe, but the
// operator sees "DRY RUN" when they asked to commit and concludes the command is
// broken. Refusing loudly turns a confusing no-op into a fixable message.
func rejectTrailingArgs(fs *flag.FlagSet, name string, stderr io.Writer) bool {
	if fs.NArg() == 0 {
		return false
	}
	_, _ = fmt.Fprintf(stderr, "prices %s: unexpected argument %s — every value is a flag here (did you mean --version %s?)\n",
		name, logsafe.Str(fs.Arg(0)), logsafe.Str(fs.Arg(0)))
	fs.Usage()
	return true
}

// runPricesList implements `tierd prices list`.
func runPricesList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prices list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	jsonOut := fs.Bool("json", false, "emit the rows as JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if rejectTrailingArgs(fs, "list", stderr) {
		return 1
	}

	rows, err := store.ListPriceTableRegistry(context.Background(), *dbPath)
	if err != nil {
		if errors.Is(err, store.ErrNoPriceTableRegistryRow) {
			_, _ = fmt.Fprintf(stderr, "prices list: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "prices list: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := make([]priceRegistryJSON, 0, len(rows))
		for _, r := range rows {
			out = append(out, priceRegistryJSON{
				Version: r.Version, TableHash: r.TableHash, FileHash: r.FileHash,
				EffectiveDate: r.EffectiveDate, ModelCount: r.ModelCount,
				Source: r.Source, ToolVersion: r.ToolVersion, FirstSeen: r.FirstSeen,
				ForgottenAt: r.ForgottenAt, ForgottenBy: r.ForgottenBy,
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "prices list: write json: %v\n", err)
			return 1
		}
		return 0
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(stdout, "no price tables recorded in this database")
		return 0
	}
	for _, r := range rows {
		header := fmt.Sprintf("price_table_registry version %d:", r.Version)
		if r.Forgotten() {
			header = fmt.Sprintf("price_table_registry version %d (RETIRED):", r.Version)
		}
		if err := writeRegistryRow(stdout, r, header); err != nil {
			_, _ = fmt.Fprintf(stderr, "prices list: %v\n", err)
			return 1
		}
	}
	// State the limit of what a recorded row establishes, right where an operator
	// reads the rows. See docs/reference-price-table.md §9 — a registry row is
	// authoritative for rows written at or after first_seen, and says nothing
	// about rows already carrying that version beforehand.
	_, _ = fmt.Fprintln(stdout, "note: a recorded identity is authoritative for rows written at or after its first_seen; rows already stamped with that version beforehand are not covered by it.")
	return 0
}

// writeRegistryRow renders one row and RETURNS the write error.
//
// 🔴 THE ERROR RETURN IS THE WHOLE POINT ON THE forget-version PATH, and its
// absence was a measured data-loss bug: `forget-version --commit >&-` exited 0
// with the row deleted and nothing shown. Every DB-sourced field goes through
// logsafe.Str — `source` is whatever --prices path some earlier boot was given,
// stored in a database this very feature treats as untrusted enough to fail
// closed against, so a newline in it could forge a line into the one output
// meant to BE the evidence.
func writeRegistryRow(w io.Writer, r store.PriceTableRegistryRow, header string) error {
	if _, err := fmt.Fprintf(w, "%s\n", header); err != nil {
		return err
	}
	for _, f := range []struct{ label, value string }{
		{"table_hash", logsafe.Str(r.TableHash)},
		{"file_hash", logsafe.Str(r.FileHash)},
		{"effective_date", logsafe.Str(r.EffectiveDate)},
		{"model_count", fmt.Sprintf("%d", r.ModelCount)},
		{"source", logsafe.Str(r.Source)},
		{"tool_version", logsafe.Str(r.ToolVersion)},
		{"first_seen", logsafe.Str(r.FirstSeen)},
	} {
		if _, err := fmt.Fprintf(w, "  %-14s %s\n", f.label, f.value); err != nil {
			return err
		}
	}
	// The stamps are printed ONLY on a retired row, so a live listing stays terse
	// and a retired one is unmistakable. This is the ruling's guarantee rendered:
	// version N once meant table_hash X, forgotten at T by U.
	if r.Forgotten() {
		for _, f := range []struct{ label, value string }{
			{"forgotten_at", logsafe.Str(r.ForgottenAt)},
			{"forgotten_by", logsafe.Str(r.ForgottenBy)},
		} {
			if _, err := fmt.Fprintf(w, "  %-14s %s\n", f.label, f.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// runPricesForgetVersion implements `tierd prices forget-version`.
//
// SAFE BY DEFAULT, exactly like `tierd reprice`: a DRY RUN unless --commit, and
// either way it PRINTS the full row — version, both hashes, effective date,
// model count, source, the binary that recorded it, and when.
//
// 🔴 THE PRINT HAPPENS BEFORE THE DELETE, AND ITS FAILURE ABORTS THE DELETE. The
// store calls back into `confirm` between reading the row and deleting it, and a
// write error there means nothing is deleted. This is not defensive style: the
// first version deleted FIRST and printed afterwards, with every write error
// discarded (`_, _ = fmt.Fprintf`), so a stdout that could not be written
// destroyed the only record of what a version meant and still exited 0.
// TestRunPricesForgetVersion_UnwritableStdoutAbortsTheDelete pins the fix; see
// store.ForgetPriceTableVersion for why the obvious shell probes for this are
// false witnesses on macOS and must not be used to re-check it.
func runPricesForgetVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prices forget-version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	// 0 is the "unset" sentinel: valid versions are >= 1, so 0 unambiguously
	// means the operator omitted the required flag rather than asking to forget
	// version zero. Same convention as reprice's --from-version.
	version := fs.Int("version", 0, "the price-table version whose recorded identity to forget (required, >= 1)")
	commit := fs.Bool("commit", false, "actually delete the row (default: DRY RUN — print the row and change nothing)")
	jsonOut := fs.Bool("json", false, "emit the row as JSON instead of text, so it can be archived alongside the ledger entry")
	// Defaults to the OS username. SELF-ASSERTED and unvalidated — a local CLI has
	// no authenticated principal to bind it to — but recorded, because an
	// attribution ledger with no attribution is not one.
	actor := fs.String("actor", "", "who is retiring this identity; recorded in price_table_registry.forgotten_by and the audit ledger (default: the OS username)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if rejectTrailingArgs(fs, "forget-version", stderr) {
		return 1
	}
	who := strings.TrimSpace(*actor)
	if who == "" {
		who = collector.OSUsername()
	}
	if *version < 1 {
		_, _ = fmt.Fprintln(stderr, "prices forget-version: --version is required and must be >= 1")
		fs.Usage()
		return 1
	}

	// confirm renders the evidence and reports whether it actually landed. The
	// store deletes nothing unless this returns nil.
	confirm := func(r store.PriceTableRegistryRow) error {
		if *jsonOut {
			// EXACTLY ONE JSON document, emitted HERE — before anything is written.
			// An earlier draft also re-emitted the stamped row afterwards, which
			// produced two top-level objects and was not parseable as JSON at all
			// (caught by the round-trip assertion in the CLI test).
			//
			// forgotten_by is known now (it is the --actor the operator chose);
			// forgotten_at is assigned by the database and is therefore NOT in this
			// archive. That is the right split: this document is the answer to "what
			// did version N mean", which is fully known before the write, while the
			// exact retirement timestamp lives in the two places designed to hold it
			// — the retained row (`tierd prices list`) and the ledger
			// (`tierd prices audit`). Blocking the archive on a value the write has
			// not produced yet would mean printing after the write, which is the
			// evidence-loss bug this ordering exists to prevent.
			by := ""
			if *commit {
				by = who
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(priceRegistryJSON{
				Version: r.Version, TableHash: r.TableHash, FileHash: r.FileHash,
				EffectiveDate: r.EffectiveDate, ModelCount: r.ModelCount,
				Source: r.Source, ToolVersion: r.ToolVersion, FirstSeen: r.FirstSeen,
				ForgottenBy: by, Retired: *commit,
			})
		}
		verb := "would retire"
		if *commit {
			verb = "retiring"
		}
		return writeRegistryRow(stdout, r, fmt.Sprintf("%s price_table_registry row for version %d:", verb, r.Version))
	}

	row, err := store.ForgetPriceTableVersion(context.Background(), *dbPath, *version, *commit, who, confirm)
	if err != nil {
		// "nothing to forget" is a different operator situation from "the command
		// failed": there is no row, which usually means a mistyped --version or the
		// wrong --db. Say that plainly, and name the neighbouring command that
		// answers it, rather than dumping a wrapped database error.
		if errors.Is(err, store.ErrNoPriceTableRegistryRow) {
			_, _ = fmt.Fprintf(stderr, "prices forget-version: nothing to forget — %v\n", err)
			_, _ = fmt.Fprintf(stderr, "run 'tierd prices list --db %s' to see which versions this database has recorded\n", *dbPath)
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "prices forget-version: %v\n", err)
		return 1
	}

	if !*commit {
		if !*jsonOut {
			_, _ = fmt.Fprintln(stdout, "DRY RUN — nothing was changed. Re-run with --commit to retire this identity.")
		}
		return 0
	}
	if *jsonOut {
		// The archive was already emitted by confirm, before the write. Emitting a
		// second document here would make the output invalid JSON.
		return 0
	}
	// State the consequence AND what was kept, on the successful path rather than
	// buried in a doc. "Retired, not deleted" is the whole ruling in one line.
	_, _ = fmt.Fprintf(stdout, "retired at %s by %s. The row was KEPT, not deleted: this database can still answer what version %d meant.\n",
		logsafe.Str(row.ForgottenAt), logsafe.Str(row.ForgottenBy), row.Version)
	_, _ = fmt.Fprintf(stdout, "The guard no longer binds version %d, so the next start records the replacement identity. Rows already stamped price_version=%d were priced under the retired table above.\n", row.Version, row.Version)
	_, _ = fmt.Fprintf(stdout, "Recorded in the audit ledger: tierd prices audit --db %s\n", *dbPath)
	return 0
}

// runPricesAudit implements `tierd prices audit` — the append-only ledger of
// every retired identity.
//
// 🔑 IT IS THE SECOND ANSWER TO THE SAME QUESTION, ON PURPOSE. The retained
// registry row and this ledger both record that version N once meant table_hash
// X, and they survive different accidents: the ledger keeps its own copy of the
// hash, so it still answers even if someone later hard-deletes the registry row
// by hand. That redundancy is the point of a ledger, not an oversight.
func runPricesAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prices audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	jsonOut := fs.Bool("json", false, "emit the ledger as JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if rejectTrailingArgs(fs, "audit", stderr) {
		return 1
	}

	rows, err := store.ListPriceForgetAudit(context.Background(), *dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "prices audit: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := make([]priceForgetAuditJSON, 0, len(rows))
		for _, r := range rows {
			out = append(out, priceForgetAuditJSON{
				ForgetID: r.ForgetID, Version: r.Version, TableHash: r.TableHash,
				FileHash: r.FileHash, EffectiveDate: r.EffectiveDate, ModelCount: r.ModelCount,
				Source: r.Source, RecordedToolVersion: r.RecordedToolVersion,
				FirstSeen: r.FirstSeen, Actor: r.Actor, ToolVersion: r.ToolVersion, TS: r.Timestamp,
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "prices audit: write json: %v\n", err)
			return 1
		}
		return 0
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(stdout, "no price-table identities have been retired in this database")
		return 0
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(stdout, "retired version %d at %s by %s (forget_id %s)\n",
			r.Version, logsafe.Str(r.Timestamp), logsafe.Str(r.Actor), logsafe.Str(r.ForgetID)); err != nil {
			_, _ = fmt.Fprintf(stderr, "prices audit: %v\n", err)
			return 1
		}
		for _, f := range []struct{ label, value string }{
			{"table_hash", logsafe.Str(r.TableHash)},
			{"file_hash", logsafe.Str(r.FileHash)},
			{"effective_date", logsafe.Str(r.EffectiveDate)},
			{"model_count", fmt.Sprintf("%d", r.ModelCount)},
			{"source", logsafe.Str(r.Source)},
			{"recorded_by", logsafe.Str(r.RecordedToolVersion)},
			{"first_seen", logsafe.Str(r.FirstSeen)},
			{"retired_with", logsafe.Str(r.ToolVersion)},
		} {
			if _, err := fmt.Fprintf(stdout, "  %-14s %s\n", f.label, f.value); err != nil {
				_, _ = fmt.Fprintf(stderr, "prices audit: %v\n", err)
				return 1
			}
		}
	}
	return 0
}

// priceForgetAuditJSON is the --json shape of one ledger entry. Separate from the
// store type for the same reason priceRegistryJSON is.
type priceForgetAuditJSON struct {
	ForgetID            string `json:"forget_id"`
	Version             int    `json:"version"`
	TableHash           string `json:"table_hash"`
	FileHash            string `json:"file_hash"`
	EffectiveDate       string `json:"effective_date"`
	ModelCount          int    `json:"model_count"`
	Source              string `json:"source"`
	RecordedToolVersion string `json:"recorded_tool_version"`
	FirstSeen           string `json:"first_seen"`
	Actor               string `json:"actor"`
	ToolVersion         string `json:"tool_version"`
	TS                  string `json:"ts"`
}

// priceRegistryJSON is the --json shape for both subcommands. Deliberately a
// separate struct from store.PriceTableRegistryRow so the CLI's output contract
// does not silently change shape when a column is added to the store type.
type priceRegistryJSON struct {
	Version       int    `json:"version"`
	TableHash     string `json:"table_hash"`
	FileHash      string `json:"file_hash"`
	EffectiveDate string `json:"effective_date"`
	ModelCount    int    `json:"model_count"`
	Source        string `json:"source"`
	ToolVersion   string `json:"tool_version"`
	FirstSeen     string `json:"first_seen"`
	// Empty on a live row; on a retired one these ARE the evidence.
	ForgottenAt string `json:"forgotten_at,omitempty"`
	ForgottenBy string `json:"forgotten_by,omitempty"`
	// Retired distinguishes a dry run from a real retirement in machine-readable
	// output, where the text path's "DRY RUN" line is not present. It is emitted
	// only by forget-version; `list` omits it. (It was named `deleted` while this
	// command hard-deleted; nothing is deleted any more, and a field that says so
	// would misdescribe the retained row it accompanies.)
	Retired bool `json:"retired,omitempty"`
}
