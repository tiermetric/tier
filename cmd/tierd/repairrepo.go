package main

// tierd repair-repo (#493): the sanctioned, audited path to repair token_events
// rows stuck on the 'unqualified' repo sentinel because a pre-#491 shipper
// dropped `repo` on the wire.
//
// THE PROBLEM IT SOLVES: #491's wire fix is FORWARD-ONLY. InsertTokenEvents
// excludes `repo` from its ON CONFLICT ... DO UPDATE clause on purpose, so a
// repo-blind producer replaying a message can never downgrade a row another
// producer already qualified. The side effect is that the obvious operator
// response to #491 — upgrade and re-ship the same window — changes NOTHING:
// every event collides on idempotency_key, `repo` stays 'unqualified', and the
// server still answers 201. Ingestion structurally cannot repair that history,
// so `tierd repair-repo` is the only path, and it mirrors the `tierd reprice`
// (#294) shape because it carries the same risk profile: it retroactively moves
// spend between per-repository TIER scores.
//
//   - Dry run unless --commit. A bare run prints exactly what WOULD change (per
//     repo: rows, sessions, spend) and writes no token_events row and no audit
//     row. (It is not a read-only OPEN: like every other subcommand it runs the
//     store's migrations, which on a first run creates the two audit tables. No
//     data row is touched.)
//   - --commit applies every row UPDATE, every before-image, and every aggregate
//     audit row in a SINGLE transaction — no partial repair.
//   - --developer is REQUIRED, so the whole table can never be swept by accident.
//   - The slug never comes from a guess: --map / --map-file supply an explicit
//     session_id -> owner/repo mapping, canonicalized through internal/repoid so
//     the repair can only write an identity the collector itself would produce.
//   - Two ledgers per committed run: repo_repair_audit (per-target-repo
//     aggregate) + repo_repair_row_audit (per-row before-image).
//
// OPERATIONAL NOTES:
//   - Run against a QUIESCED database: a committed repair holds the single-writer
//     lock for the span of its transaction and will contend with a live
//     `tierd serve` ingest path.
//   - A `tierd backup` snapshot before committing is prudent belt-and-braces for
//     a large first-time repair, even though the per-row before-images make the
//     change reversible-in-principle at row grain.
//   - WHERE THE MAPPING COMES FROM: one Claude Code session belongs to exactly
//     one working tree, and ~/.claude/projects is keyed by that tree's path — so
//     the operator can derive `<session-uuid>=<owner/repo>` from the same place
//     the collector derives it. Rows with no session (proxy/poller) are
//     unresolvable by construction and are reported, never guessed.
//
// SCOPE — token_events ONLY. `outcomes` also carries a `repo` column that can hold
// the same sentinel, and this command deliberately does not touch it. Two reasons:
// the outcome producer is the GitHub webhook, which reads repository.full_name and
// therefore ALWAYS knows the repository (an unqualified outcome means something
// different, and would need a different diagnosis, not this repair); and session_id
// does not exist on outcomes, so the mapping key this command is built on has no
// meaning there. #493 is scoped to the token_events damage #491 left behind.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// maxReportedBuckets caps how many unresolved-session or conflict lines the
// report prints before collapsing the rest into a "... and N more" line. A repair
// on a real laptop can face thousands of unmapped sessions, and a report that
// scrolls the actionable summary off the terminal is a report nobody reads. The
// COUNTS above the list are always complete — only the enumeration is truncated.
const maxReportedBuckets = 20

// runRepairRepoCmd parses flags, loads the operator-supplied session -> slug
// mapping, opens the store, and runs the repair, returning the process exit code
// (0 clean, 1 fatal) so main's os.Exit stays the single exit point and the
// deferred db.Close always runs. Output goes to the injected writers so the
// subcommand is testable through dispatch (mirrors reprice/backfill/doctor).
func runRepairRepoCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repair-repo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path to repair")
	// REQUIRED, and the structural analogue of reprice's --from-version: the guard
	// that makes a whole-table mutation impossible by accident. Empty is the
	// unset sentinel — a developer id is never legitimately empty.
	//
	// The help text states the ALIAS SCOPE out loud because the flag's name
	// invites the opposite assumption: "developer" reads as "the person", but the
	// column holds the RAW producer id (OS username from a JSONL capture, GitHub
	// login from the webhook) and developer_alias unifies them only at READ time.
	// An operator who does not know that gets a partial repair and a clean report.
	// The report also NOTEs any sibling identity holding unrepaired rows, but the
	// flag help is where the operator is standing when they make the choice.
	developer := fs.String("developer", "", "developer whose 'unqualified' rows to repair (required; bounds the repair so it can never sweep the whole table). This is the RAW token_events.developer value as STORED, and aliases are NOT resolved — if this person's rows were captured under more than one identity (e.g. an OS username and a GitHub login joined by developer_alias), run the command once per stored identity")
	var mapPairs repeatableStringSlice
	fs.Var(&mapPairs, "map", `session-to-repository mapping, as "<session-id>=<owner/repo>" (repeatable; #493). One Claude Code session belongs to exactly one working tree, so this is the only honest key — developer/source/model are many-to-many with repositories and would force a guess`)
	mapFile := fs.String("map-file", "", `path to a file of "<session-id>=<owner/repo>" lines (one per line; blank lines and lines starting with # are ignored). Use this instead of --map when repairing a real history — thousands of sessions do not fit on a command line`)
	commit := fs.Bool("commit", false, "apply the repair (default: DRY RUN — print what would change and write nothing)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	if *developer == "" {
		_, _ = fmt.Fprintln(stderr, "repair-repo: --developer is required (the developer whose 'unqualified' rows to repair; refusing to repair every developer's rows at once)")
		return 1
	}

	mapping, err := loadRepairMapping(mapPairs, *mapFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "repair-repo: %v\n", err)
		return 1
	}
	if len(mapping) == 0 {
		// An empty mapping would resolve nothing and report a clean "0 changed",
		// which is indistinguishable from "your history is already fine". Refuse
		// so a forgotten --map cannot read as success.
		_, _ = fmt.Fprintln(stderr, "repair-repo: no mapping supplied — pass --map <session-id>=<owner/repo> (repeatable) or --map-file <path>. Without a mapping there is nothing to resolve, and a run that resolves nothing must not look like a successful repair")
		return 1
	}

	// The repair operates on an EXISTING database (dry run and commit alike) —
	// refuse a path that does not exist rather than let store.Open create an empty
	// DB and silently report "0 rows" for a typo'd --db. Same refusal reprice makes.
	if _, err := os.Stat(*dbPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "repair-repo: --db %s: %v (repair-repo operates on an existing database)\n", *dbPath, err)
		return 1
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "repair-repo: open db: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	// Cancel cleanly on SIGINT/SIGTERM. A cancelled commit rolls the transaction
	// back (store.RepairRepo is all-or-nothing), so an interrupted repair never
	// leaves a partial mutation.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	res, err := db.RepairRepo(ctx, store.RepairRepoOptions{
		Developer:     *developer,
		SlugBySession: mapping,
		Commit:        *commit,
		ToolVersion:   versionString(),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "repair-repo: %v\n", err)
		return 1
	}

	printRepairRepoResult(stdout, res)
	return 0
}

// loadRepairMapping merges the repeatable --map pairs with the --map-file lines
// into one session -> canonical-slug map.
//
// Both sources go through the same parser, so a pair typed on the command line
// and a line in a file are validated identically. A duplicate session id is only
// tolerated when both entries name the SAME repository: two entries disagreeing
// about where one session's spend belongs is an ambiguity that last-wins would
// resolve arbitrarily and invisibly, and the wrong arbitrary answer is precisely
// the mis-attribution this command exists to fix.
func loadRepairMapping(pairs []string, mapFilePath string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if err := addRepairMapping(out, p, "--map"); err != nil {
			return nil, err
		}
	}
	if mapFilePath == "" {
		return out, nil
	}

	f, err := os.Open(mapFilePath)
	if err != nil {
		return nil, fmt.Errorf("--map-file %s: %w", mapFilePath, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		// Blank lines and whole-line comments only. A trailing "#" is NOT stripped:
		// it cannot appear in a canonical slug (repoid rejects it), so treating it
		// as a comment would silently turn a malformed line into a valid-looking
		// one instead of failing loudly on it.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := addRepairMapping(out, line, fmt.Sprintf("%s:%d", mapFilePath, lineNo)); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		// Includes bufio.ErrTooLong for a pathologically long line — surfaced
		// rather than silently truncating the mapping.
		return nil, fmt.Errorf("--map-file %s: %w", mapFilePath, err)
	}
	return out, nil
}

// addRepairMapping parses one "<session-id>=<owner/repo>" entry into out.
// `origin` names where the entry came from (a flag name, or "file:line") so a
// rejection points the operator at the exact thing to fix.
//
// The slug is canonicalized here with the SAME repoid.Canonical the collector and
// `ship --repo-slug` use, so a hand-typed "Tiermetric/Tier" lands as
// "tiermetric/tier" — byte-identical to what the webhook and the JSONL collector
// write, which is what keeps the repaired rows joinable to their outcomes (#231).
// A malformed entry is a hard error, never a skipped line.
func addRepairMapping(out map[string]string, entry, origin string) error {
	session, slug, ok := strings.Cut(entry, "=")
	session = strings.TrimSpace(session)
	slug = strings.TrimSpace(slug)
	if !ok || session == "" || slug == "" {
		return fmt.Errorf("%s: expected <session-id>=<owner/repo>, got %q", origin, entry)
	}
	canon, ok := repoid.Canonical(slug)
	if !ok {
		return fmt.Errorf("%s: %q is not a canonical owner/repo slug (the reserved %q sentinel and single-segment names are rejected)", origin, slug, repoid.Unqualified)
	}
	if prev, dup := out[session]; dup && prev != canon {
		return fmt.Errorf("%s: session %q is mapped to both %q and %q — resolve the ambiguity rather than letting one silently win", origin, session, prev, canon)
	}
	out[session] = canon
	return nil
}

// printRepairRepoResult renders the repair outcome — identical shape for a dry
// run and a commit, differing only in the header verb and the trailing line.
//
// The supplied-mapping size is echoed in the header so a mapping that silently
// lost entries (a truncated --map-file, a shell that ate a quote) is visible
// against the per-repo session counts below. It is read from res, NOT from a
// separate parameter: the MAPPING section says "N of your M matched no row", and
// M appearing twice from two sources is a reconciliation pass waiting to drift —
// the same argument RepairRepo's repoAcc makes for not keeping two maps on one
// key. One source, one number, no way for the header and the gap line to
// disagree.
func printRepairRepoResult(w io.Writer, res store.RepairRepoResult) {
	verb := "DRY RUN"
	// PASSIVE, to match the committed form: the sentence is "%d row(s) %s", so
	// the active "would repair" rendered as "3 row(s) would repair" — the rows
	// doing the repairing. Small, and in the one line an operator reads first.
	changeVerb := "would be repaired"
	if res.Committed {
		verb = "COMMITTED"
		changeVerb = "repaired"
	}
	// Every operator- or producer-controlled string in this report goes through
	// logsafe.Str (#321), developer included. %q escapes CR/LF at RUNTIME but is
	// NOT the barrier this repo standardised on: the strip REMOVES the newline
	// where %q only escapes it, and an escape is reversible by any consumer that
	// unquotes the field to render it. See internal/logsafe's package doc, which
	// is the SINGLE SOURCE OF TRUTH for what CodeQL does and does not credit —
	// this comment used to paraphrase it, and the paraphrase went stale the day
	// the doc was corrected (#321 review, 2026-08-04: %q via a format call IS
	// credited; strconv.Quote is not). Do not restate the rule here; link it.
	//
	// This report is routinely piped into a maintenance log, and
	// `--developer $'alice\n  UNRESOLVED: 0 rows'` would otherwise forge a line an
	// operator reads as genuine — pinned by
	// TestPrintRepairRepoResult_NotForgeable.
	_, _ = fmt.Fprintf(w, "repair-repo %s: developer %s, %d session(s) mapped\n", verb, logsafe.Str(res.Developer), res.MappedSessionCount)

	// Distinguish "the developer matched no rows at all" (almost always a typo in
	// --developer) from "rows matched but none needed repair". Reporting the
	// former as the latter would mask an operator's typo as a successful no-op —
	// the same false green reprice's RowCount==0 branch exists to prevent.
	if res.ScannedRowCount == 0 {
		_, _ = fmt.Fprintf(w, "  developer %s owns no token_events rows at all — nothing examined (check --developer)\n", logsafe.Str(res.Developer))
		// 🔴 THE ALIAS NOTE MUST FIRE HERE, and it is the one place it is most
		// needed. "check --developer" is a question, and when the named identity
		// is an alias of the one that actually owns the rows, this result is
		// already holding the ANSWER — RepairRepo populates the alias fields
		// BEFORE the scan, so they survive a zero-row run. Returning without
		// printing them told an operator to go hunting for a name the report knew.
		printRepairAliasNote(w, res)
		return
	}

	_, _ = fmt.Fprintf(w, "  examined %d row(s): %d already qualified (never touched), %d unqualified\n",
		res.ScannedRowCount, res.AlreadyQualifiedRowCount, res.UnqualifiedRowCount)
	// Printed only when something actually moves. A "0 row(s) repaired, carrying 0
	// micro-USD" line is pure noise on the two most common zero-change outcomes
	// (a completed repair re-run, and a mapping that disagrees), and it pushes the
	// line that explains WHY nothing moved further down the screen.
	//
	// No leading "+": unlike reprice's cost DELTA, this is a spend TOTAL. A signed
	// total reads as "spend increased by", which is exactly what a repair does NOT
	// do — it moves existing spend between repository identities without changing
	// a single cost_micro.
	if res.ChangedRowCount > 0 {
		_, _ = fmt.Fprintf(w, "  %d row(s) %s, carrying %d micro-USD (%.6f USD) of existing spend (no cost is changed — only its repository attribution)\n",
			res.ChangedRowCount, changeVerb, res.ChangedCostMicroSum, store.MicroToDollars(res.ChangedCostMicroSum))
		for _, del := range res.ByRepo {
			_, _ = fmt.Fprintf(w, "    %s -> %s: %d row(s) across %d session(s), %d micro-USD\n",
				del.FromRepo, del.Repo, del.RowCount, del.SessionCount, del.CostMicroSum)
		}
	}

	printRepairUnresolved(w, res)
	printRepairMappingGaps(w, res)
	printRepairConflicts(w, res)
	printRepairAliasNote(w, res)

	if res.Committed {
		if res.ChangedRowCount == 0 {
			// A commit that changed nothing wrote no audit rows. Say so explicitly
			// rather than printing an audit line with a zero count, which reads as
			// "an audit exists" when none does.
			_, _ = fmt.Fprintf(w, "  nothing to repair — no rows changed and no audit rows were written (%s)\n", whyNothingChanged(res))
			return
		}
		_, _ = fmt.Fprintf(w, "  audit: %d row(s) to repo_repair_audit + %d per-row before-image(s) to repo_repair_row_audit (repair_id %s)\n",
			len(res.ByRepo), res.ChangedRowCount, res.RepairID)
		// Said out loud, because the first thing a mistaken operator hits is the
		// CONFLICT path: once a row is repaired it reads as already-qualified, so a
		// corrective mapping is refused rather than applied. There is no --undo
		// command (the before-images are the substrate for one, not the one), so
		// the recovery is hand-written SQL against the ledger. Better to learn that
		// here than after the wrong mapping has been committed.
		_, _ = fmt.Fprintf(w, "  NOTE: there is no --undo. To reverse this run, restore each row from repo_repair_row_audit WHERE repair_id = '%s' (old_repo, by token_event_id). A repaired row reads as already-qualified, so re-running with a corrected mapping will be REFUSED, not applied.\n", res.RepairID)
		return
	}
	if res.ChangedRowCount == 0 {
		_, _ = fmt.Fprintf(w, "  NOTHING was written, and nothing WOULD be (%s)\n", whyNothingChanged(res))
		return
	}
	_, _ = fmt.Fprintln(w, "  NOTHING was written. Re-run with --commit to apply this repair.")
}

// whyNothingChanged explains a zero-change run, because "nothing to repair" is
// three different facts wearing one sentence and only one of them is benign.
//
// 🔴 The first draft printed "re-running a completed repair is a no-op"
// unconditionally, which a REAL-BINARY run caught telling a lie: a mapping that
// disagreed with every stored repo produced zero changes and was then described
// as a completed repair. An operator whose mapping is wrong would read that as
// "already done" and stop looking — the exact false green this command family
// exists to avoid. The loud WARNING block above still prints in that case; this
// makes the summary line agree with it.
func whyNothingChanged(res store.RepairRepoResult) string {
	var reason string
	switch {
	case res.UnqualifiedRowCount == 0:
		// Nothing was left to repair. This is the genuine completed-repair re-run.
		reason = "every row for this developer already carries a real repo; re-running a completed repair is a no-op"
	case res.UnresolvedNoSessionRowCount == res.UnqualifiedRowCount:
		// Every remaining candidate is session-less, so no mapping could ever fix
		// it. Also benign, and distinct from "you forgot a mapping entry" — saying
		// "check your mapping keys" here would send the operator hunting for a
		// line they can never write.
		reason = "the only unqualified rows left carry no session id, which no mapping can repair; re-running a completed repair is a no-op"
	default:
		reason = "no unqualified row matched a mapped session; check --developer and your mapping keys"
	}
	if res.ConflictRowCount > 0 {
		// Appended, never substituted: the disagreement is an ADDITIONAL fact and
		// must not be able to hide behind an otherwise-benign reason.
		reason += "; and your mapping disagrees with rows that are already qualified — see the WARNING above"
	}
	if res.AliasUnqualifiedRowCount > 0 {
		// Same class as the conflict append, and for the same reason. The alias
		// NOTE prints earlier, but this summary is the LAST line an operator
		// reads — and unappended it says "re-running a completed repair is a
		// no-op" while a sibling identity of the same person still holds
		// unattributed spend. That is the false green this command exists to
		// avoid, one identity to the left.
		reason += "; and a sibling identity of this developer still holds unqualified rows this run did NOT examine — see the NOTE above"
	}
	return reason
}

// printRepairUnresolved reports the unqualified rows the mapping could not
// resolve. They are LEFT unqualified and named here rather than guessed at — the
// #493 acceptance bullet. The no-session bucket is called out separately because
// it is categorically different: those rows carry no session at all (the proxy
// and poller paths structurally cannot know one), so no mapping entry could ever
// repair them, and an operator hunting for a missing --map line would otherwise
// waste time on them.
//
// The two subtotal lines are printed ONLY when their count is non-zero, and that
// suppression is the fix for a real defect found driving the binary. The section
// used to be one sentence carrying both subtotals, so a run with one unmapped
// session and no session-less rows printed:
//
//	UNRESOLVED: 1 unqualified row(s) stay unqualified — 0 of them carry no
//	  session id at all, carrying 0 micro-USD that NO mapping can ever repair
//	    unmapped session sess-z: 1 row(s), 3000 micro-USD
//
// The zeros were true (they described the no-session subset) but they sat
// immediately above a 3000, and the two lines read as contradicting each other.
// An operator who cannot tell which number to believe stops believing all of
// them. Now every subtotal names the subset it describes on its own line, and a
// subset with no rows is not mentioned at all — so no zero can ever appear beside
// the non-zero it does not describe.
func printRepairUnresolved(w io.Writer, res store.RepairRepoResult) {
	if res.UnresolvedRowCount == 0 {
		return
	}
	// Spend is reported per subset AND in total, in micro-USD, not just as row
	// counts: this is the portion of the developer's history that is still
	// unattributed to a repository, and an operator deciding whether to keep
	// chasing mapping entries needs its size, not just its cardinality.
	noSessionSpend, totalSpend := int64(0), int64(0)
	for _, u := range res.Unresolved {
		totalSpend += u.CostMicroSum
		if u.SessionID == "" {
			// += rather than =: at most one "" bucket can exist (the scan keys
			// unresolved buckets by session), so the two are equivalent — but the
			// loop no longer breaks, and an assignment inside a summing loop reads
			// as one that forgot its +=.
			noSessionSpend += u.CostMicroSum
		}
	}
	namedRows := res.UnresolvedRowCount - res.UnresolvedNoSessionRowCount
	namedSpend := totalSpend - noSessionSpend

	_, _ = fmt.Fprintf(w, "  UNRESOLVED: %d unqualified row(s) stay unqualified, carrying %d micro-USD in total\n",
		res.UnresolvedRowCount, totalSpend)
	if res.UnresolvedNoSessionRowCount > 0 {
		_, _ = fmt.Fprintf(w, "    %d of them %s no session id at all (proxy/poller rows), carrying %d micro-USD that NO mapping can ever repair\n",
			res.UnresolvedNoSessionRowCount, pluralCarry(res.UnresolvedNoSessionRowCount), noSessionSpend)
	}
	if namedRows == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "    %d of them %s a session id your mapping does not name, carrying %d micro-USD — add a --map entry for each:\n",
		namedRows, pluralCarry(namedRows), namedSpend)
	shown := 0
	for _, u := range res.Unresolved {
		if u.SessionID == "" {
			continue // summarized on its own line above; never enumerated
		}
		if shown == maxReportedBuckets {
			_, _ = fmt.Fprintf(w, "      ... and %d more unmapped session(s)\n", len(res.Unresolved)-shown-boolToInt(res.UnresolvedNoSessionRowCount > 0))
			break
		}
		_, _ = fmt.Fprintf(w, "      unmapped session %s: %d row(s), %d micro-USD\n", logsafe.Str(u.SessionID), u.RowCount, u.CostMicroSum)
		shown++
	}
}

// printRepairMappingGaps reports the OTHER side of the ledger: mapping entries
// the operator supplied that matched no row at all.
//
// Everything else in this report describes the database. This describes the
// OPERATOR'S INPUT, and it is the command's most likely failure mode — a stale
// session export, a typo'd UUID, or a UTF-8 BOM on the first line of a
// --map-file (strings.TrimSpace does not strip U+FEFF, so entry one silently
// carries three invisible bytes and can never match). Without this line all three
// produce the same clean "0 row(s) repaired" as a database that was already
// healthy, and the operator has no way to tell which they are looking at.
//
// It is NOT an error. Mapping a session that belongs to another developer, or
// whose rows are already qualified, is a legitimate way to run this command —
// so the gap is reported, and the exit code stays 0.
func printRepairMappingGaps(w io.Writer, res store.RepairRepoResult) {
	if len(res.UnmatchedSessions) == 0 {
		return
	}
	// res.Developer goes through logsafe.Str, not a bare %q. %q escapes a newline
	// at RUNTIME but is not a barrier CodeQL's go/log-injection credits (#321), and
	// this report is routinely piped into a maintenance log. The rendered output is
	// byte-identical for any sane identifier.
	_, _ = fmt.Fprintf(w, "  MAPPING: %d of your %d mapping entr%s matched NO row owned by %s — a stale session export, a typo'd session id, or a UTF-8 BOM on the first line of --map-file (which is never stripped, so entry 1 carries three invisible bytes):\n",
		len(res.UnmatchedSessions), res.MappedSessionCount,
		pluralEntries(res.MappedSessionCount), logsafe.Str(res.Developer))
	for i, session := range res.UnmatchedSessions {
		if i == maxReportedBuckets {
			_, _ = fmt.Fprintf(w, "    ... and %d more unmatched mapping entr%s\n",
				len(res.UnmatchedSessions)-i, pluralEntries(len(res.UnmatchedSessions)-i))
			break
		}
		// Quoted via logsafe.Str, which is doing double duty here: it is the
		// log-injection barrier every operator-supplied string goes through, AND
		// it is what makes an invisible byte visible — %q escapes a leading BOM
		// into the printable escape \ufeff, which is the whole diagnosis in one
		// line. Printed bare, a BOM'd session id looks identical to a clean one.
		_, _ = fmt.Fprintf(w, "    unmatched mapping entry %s\n", logsafe.Str(session))
	}
}

// printRepairAliasNote names the rows this run structurally COULD NOT reach: the
// ones stored under a sibling identity of --developer.
//
// token_events.developer holds the RAW producer id and developer_alias unifies
// identities only at read time, so one person's spend routinely sits under two
// names. The repair's `WHERE developer = ?` is EXACT and stays exact — widening it
// to the alias set would apply a mapping derived from one machine's session
// history to rows stored under another identifier, which is the cross-person
// re-attribution the --developer bound exists to prevent. So the gap is named
// instead, with the fix in the message: run it again per stored identity.
//
// Without this the partial repair was indistinguishable from a complete one — no
// UNRESOLVED line, no WARNING, just a smaller number than the operator expected
// and no reason given.
func printRepairAliasNote(w io.Writer, res store.RepairRepoResult) {
	if res.AliasUnqualifiedRowCount == 0 {
		return
	}
	// Capped like every other enumeration here. developer_alias is
	// operator-maintained and normally holds one or two siblings, but nothing
	// bounds it, and an unbounded join is the one line in this report that could
	// still scroll the actionable summary away.
	safe := make([]string, 0, len(res.AliasIdentities))
	for _, a := range res.AliasIdentities {
		if len(safe) == maxReportedBuckets {
			safe = append(safe, fmt.Sprintf("... and %d more", len(res.AliasIdentities)-maxReportedBuckets))
			break
		}
		safe = append(safe, logsafe.Str(a))
	}
	_, _ = fmt.Fprintf(w, "  NOTE: developer %s has alias(es) %s carrying %d unqualified row(s) this run did NOT examine — re-run with --developer <alias> for each. token_events stores the raw producer id and developer_alias unifies identities only at read time; the repair's scoping is exact on purpose, because widening it would risk re-attributing another person's spend.\n",
		logsafe.Str(res.Developer), strings.Join(safe, ", "), res.AliasUnqualifiedRowCount)
}

// pluralEntries picks entry/entries, for the same reason pluralCarry exists: the
// MAPPING line is the one an operator acts on, and "1 mapping entries" next to it
// is small enough to be read as carelessness and large enough to cost trust.
func pluralEntries(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// printRepairConflicts reports rows the repair REFUSED to touch because they
// already carry a real repo that the mapping disagrees with.
//
// This is the loudest line in the report and it is deliberately not an error
// exit: the invariant HELD, nothing was mutated. But a disagreement almost always
// means the mapping is wrong, and a silent skip would let the operator conclude
// their repair was complete and correct when part of it was built on a bad key.
func printRepairConflicts(w io.Writer, res store.RepairRepoResult) {
	if res.ConflictRowCount == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  WARNING: %d row(s) already carry a REAL repo that your mapping DISAGREES with. They were NOT modified (a real repo is never overwritten) — but a disagreement usually means the mapping is wrong:\n",
		res.ConflictRowCount)
	for i, c := range res.Conflicts {
		if i == maxReportedBuckets {
			_, _ = fmt.Fprintf(w, "    ... and %d more disagreement(s)\n", len(res.Conflicts)-i)
			break
		}
		// StoredRepo is read straight out of the column and is the only unbounded
		// producer-controlled string on this line, so it is sanitized too.
		// MappedRepo came through repoid.Canonical and is provably bounded to
		// [a-z0-9._/-], so it is printed bare — the same "provably bounded values
		// are logged bare with an inline rationale" carve-out logsafe documents.
		_, _ = fmt.Fprintf(w, "    session %s: stored %s, mapping says %s (%d row(s), %d micro-USD, kept %s)\n",
			logsafe.Str(c.SessionID), logsafe.Str(c.StoredRepo), c.MappedRepo, c.RowCount, c.CostMicroSum, logsafe.Str(c.StoredRepo))
	}
}

// pluralCarry picks carry/carries for a row count, because "1 row(s) carry" is
// the kind of small wrongness that makes an operator distrust the numbers next
// to it.
func pluralCarry(n int64) string {
	if n == 1 {
		return "carries"
	}
	return "carry"
}

// boolToInt renders the no-session bucket's presence as a count, so the "... and
// N more" line does not include it (it is summarized separately and never listed).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
