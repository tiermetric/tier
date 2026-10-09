package main

// tierd verify-report (#718, part of #710): take a report manifest and either
// REPRODUCE its numbers against a database, or say WHICH named input moved.
//
// THE ASK IT ANSWERS, verbatim (the maintainer, 2026-08-28):
//
//	"We can't run these reports and then be unable to recreate them again. We
//	 should be able to run them against a pricing model, so we know that THIS
//	 pricing model — maybe a hash of it — was run against THESE logs and it came
//	 up with THESE numbers."
//
// 🔴 WHY A BOOLEAN WOULD BE USELESS. Late JSONL ingestion, an audited reprice
// (`tierd reprice`), a repo repair (`tierd repair-repo`), a sanctioned cost
// correction, a webhook-driven quality revision, a GDPR erasure, a
// developer_alias edit and an org_hierarchy change ALL legitimately move a
// published figure.
//
// ⚠️ AND SO DOES A ROW OUTSIDE THE WINDOW — measured while building this, and
// not obvious. /scores emits the installation's COST HORIZON (#512) in
// data_quality.cost_coverage_start, derived from the earliest captured event in
// the WHOLE database. Inserting a token event a year before the window therefore
// changes the served bytes while every window-scoped watermark correctly reports
// UNCHANGED. That is a real gap in tiermanifest1's watermark set, not a defect
// in this command; the UNATTRIBUTED block names it FIRST because it is the one
// an operator would never guess. Raised on #715 together with the two identity
// tables (developer_alias, org_hierarchy), which upsert in place and are
// likewise unwatermarked. A tool that only says "different" makes every one of those
// indistinguishable from corruption, and an operator who cannot tell them apart
// stops reading the tool. So the output is an ATTRIBUTION: one line per input
// dimension, each saying UNCHANGED, or exactly what moved and by how much.
//
// 🔴 WHAT THIS COMMAND DOES **NOT** DO, and it must never imply otherwise.
// It RECOMPUTES the report over the manifest's window against the database AS IT
// IS NOW. It does NOT replay the historical row population: as-of bounded reads
// (#717) are not in this build, so there is no way to ask the store "what did
// this window contain at watermark W". That means:
//
//   - When nothing moved, the recomputation IS the original computation over the
//     same rows, and REPRODUCED is a true statement.
//   - When something moved, this names WHICH input moved and by how much. It does
//     NOT reconstruct the manifest's original numbers from the current database.
//
// Every run prints that limit in a LIMITS block, unconditionally, including the
// successful ones — a limitation an operator only sees on failure is a limitation
// they will forget on success. See the judgement recorded in the #718 report.
//
// 🔴 IT NEVER TOUCHES THE DATABASE YOU POINT IT AT. Every read — the report
// re-run and the attribution queries alike — happens against a `VACUUM INTO`
// SNAPSHOT taken at the start of the run, and the snapshot is deleted at the
// end. That is not tidiness, it is the difference between an auditor and a
// contaminant: reaching the real report path needs a *store.DB, store.Open is
// the only way to get one, and store.Open MIGRATES. On an archived pre-#233
// database the migration chain's price-version backfill would stamp
// `token_events.price_version` — rewriting the exact provenance column the audit
// is about. store.Backup is the one door that "opens its OWN minimal connection
// and runs NO migrations"; this command goes through it.
//
// ⚠️ THE SNAPSHOT ITSELF **IS** MIGRATED, AND SINCE #740 THAT REACHES A RESULT.
// store.Backup copies without migrating; store.Open is then called ON THE COPY,
// so the migration chain runs — on the copy, never on the operator's file. That
// was always true and was previously inconsequential, because every dimension
// read id SEQUENCES. The window digests read stored BYTES, so an Open()-time
// migration (the price_version backfill, the cost_micro conversion) can move a
// digest with no edit having been made. The digest line says so on a divergence
// and points at the tool_version line, which is the signal for exactly that case.
//
// ⚠️ ONE CLASS OF MANIFEST CANNOT BE RE-RUN AT ALL, and the answer is rc 2 rather
// than a guess. The served manifest publishes RFC3339 INSTANTS; /api/v1/scores
// accepts only whole-day bounds (YYYY-MM-DD, YYYY-MM, YYYY), so an instant that
// is not midnight UTC has no query that reproduces its window. See
// parseManifestBound: truncating the bound would re-run a DIFFERENT window and
// compare its numbers against this manifest's.
//
// ✅ THE CASE THAT USED TO HIT IS CLOSED (#746) AND THE REFUSAL IS STILL LIVE —
// those are two different facts and both matter. It used to be `?since=` OMITTED:
// api.parseSince resolved to now-90d carrying a time of day, so the DEFAULT
// manifest was the one shape this command could not read. That bound is now
// snapped back to the start of its UTC day, so a default manifest verifies
// normally. What remains reachable is any manifest this server did not emit — a
// hand-written one, or one from a pre-#746 build — and for those rc 2 is still
// the honest answer, never a truncation.
//
// ⛔ NEVER WRAP THIS IN A `make` TARGET FOR CI. CLAUDE.md documents that GNU make
// REPLACES a recipe's exit code (2 for any failed recipe whatever it returned, or
// 0 where errors are ignored), which would collapse this command's 1 and 2 into
// the same number. That is exactly how the CVE re-scan announced "nothing was
// established" over two real HIGHs in a published image. Invoke the binary.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite" // the read-only attribution handle opens its own DSN
)

// manifestSchemaTag is the ONLY manifest scheme this binary understands (#715).
//
// 🔑 The tag is the rollback seam, exactly as it is on the price-table hashes
// (store.PriceIdentityStampFormat): a verifier must be able to tell "this
// manifest was written by a DIFFERENT scheme" apart from "these inputs
// disagree". An unrecognised tag is a COULD-NOT-CHECK (rc 2), never a pass and
// never a divergence — we have no idea what the fields mean.
const manifestSchemaTag = "tiermanifest1"

// digestScheme is the ONLY window-digest scheme this binary can COMPUTE (#716).
//
// 🔑 IT IS THE ROLLBACK SEAM FOR THE DIGESTS, exactly as manifestSchemaTag is for
// the manifest as a whole. A manifest pinning a tierdig2 value is reported NOT
// CHECKED — neither agreement nor divergence — because comparing two values from
// two schemes is not a comparison at all. ⛔ That branch must stay REACHABLE: it
// is what lets a future scheme land without every archived manifest reading as a
// forgery. Before #740 it was the ONLY outcome, which is a different defect.
const digestScheme = "tierdig1"

// The three-valued exit discipline (#718). These are the SCRIPT's — i.e. the
// process's — codes, and 2 is never a pass. A sealed month's manifest adds 3 and
// 4 (verifysealed.go).
const (
	// rcReproduced: every pinned input matched and the recomputed report equals
	// the manifest's in CANONICAL form — key-sorted, whitespace-normalised, numbers
	// re-rendered (see canonicalJSON). ⚠️ NOT "byte-identical", which is a stronger
	// property this command deliberately does not check: a re-indented or
	// re-key-ordered manifest is the same report, and failing it would be a
	// fabricated divergence.
	rcReproduced = 0
	// rcDiverged: at least one pinned input moved, or the numbers moved. The
	// report names which.
	rcDiverged = 1
	// rcCannotCheck: the verification did not happen. A missing database, an
	// unreadable/unknown manifest, a report that would not recompute, or a
	// manifest that pins nothing at all, or data pins present but not checked.
	// 🔴 NEVER a pass — and deliberately NOT
	// folded into rcDiverged, because "we checked and it moved" and "we did not
	// check" are different facts and an operator must act differently on each.
	rcCannotCheck = 2
)

// maxQualityRevisionsListed bounds the per-revision detail lines. The count is
// always exact; only the enumeration is capped, mirroring maxListedInReport in
// reprice.go — a silently truncated list would make the attribution quietly
// incomplete, which is worse than a long one.
const maxQualityRevisionsListed = 10

// ---------------------------------------------------------------------------
// The manifest contract (#715 emits it; this decodes it)
// ---------------------------------------------------------------------------

// reportManifest is the on-disk manifest, decoded. It mirrors the SERVED #715
// contract (internal/api.reportManifestJSON) field for field, plus one additive
// extension (Results, below).
//
// 🔑 THE CONTRACT IS THE JSON, NOT THIS GO TYPE. #715 owns the emitter and lives
// in internal/store + internal/api; this is a decode-side mirror in cmd/tierd on
// purpose, so a verifier can read a manifest written by an older or newer binary.
// Do not "de-duplicate" this into a shared type without a reason: a verifier that
// can only parse what the current binary emits cannot verify history, which is
// its entire job.
//
// 🔴 BUT "A DECODE-SIDE MIRROR" IS A LICENCE TO LAG, NOT A LICENCE TO DIVERGE,
// AND IT HAD DIVERGED (#741). This type was written against the contract as #718
// projected it and #715 shipped something else, so for one commit the two were
// MUTUALLY UNREADABLE while both called themselves `tiermanifest1`. Measured on
// d8beb84 by feeding the real handler's bytes to loadManifest:
//
//	parse manifest …: json: cannot unmarshal string into Go struct field
//	reportManifest.aggregation of type main.manifestAggregation
//
// The served manifest could not be verified AT ALL — rc 2, on the one document
// this command exists to consume — and nothing reported it, because the only
// producer anyone exercised was `--emit`, this file's own stopgap, which agreed
// with this type by construction. That is #741's "two emitters of one contract"
// in its terminal form. ⇒ The fields below are now the SERVED ones, `--emit` is
// gone, and the fixture that exercises this command drives the real handler, so
// a future divergence has to get past a decode of live bytes.
type reportManifest struct {
	Schema string `json:"manifest_schema"`

	// Since/Until are the half-open window, RFC3339 UTC INSTANTS as the served
	// emitter writes them. The legacy date grammar (YYYY-MM-DD, YYYY-MM, YYYY) is
	// ALSO accepted — see parseManifestBound — because an operator writing a
	// manifest by hand will reach for a date, and refusing one buys nothing.
	Since string `json:"since"`
	Until string `json:"until,omitempty"`
	// TokenSince is the emitter's resolved attribution-band lower bound. Decoded
	// so it is not reported as an unknown pin; the band is RE-DERIVED here from
	// `since` rather than trusted, because a manifest that could move the band it
	// is audited against could hide a late row from its own verification.
	TokenSince string `json:"token_since,omitempty"`
	// Repo is the repository scope, absent when the read was fleet-wide. Named
	// `repo` on the wire, matching the served field and the ?repo= parameter.
	Repo string `json:"repo,omitempty"`
	// Aggregation is the served grouping level: developer | team | division.
	Aggregation string `json:"aggregation"`
	// K is a POINTER because the emitter writes it ONLY in an anonymized mode:
	// "no floor is applied" and "the floor is zero" are different states, and a
	// k of 0 read from an absent key would let this binary re-run an anonymized
	// report with no floor at all.
	K *int `json:"k,omitempty"`

	PriceTable  manifestPriceTable `json:"price_table"`
	Rubric      manifestRubric     `json:"rubric"`
	ToolVersion string             `json:"tool_version"`
	Commit      string             `json:"commit,omitempty"`

	// Watermarks is a POINTER, and so are its halves and every field inside them,
	// because "absent" and "zero" are different facts and conflating them is a
	// spectacular false positive: a nil max_quality_history_id read as 0 would
	// attribute EVERY quality revision in the database's history to this window.
	// An absent watermark is reported as NOT PINNED, never as UNCHANGED.
	Watermarks *manifestWatermarks `json:"watermarks,omitempty"`
	// KAnonSuppressed is the emitter's declaration that an anonymized mode
	// WITHHELD the window block and the digests. Decoded so this binary can say
	// "withheld by k-anonymity" instead of the bare "not pinned" it would
	// otherwise print for the same absence — two different facts.
	KAnonSuppressed *manifestKAnon `json:"kanon_suppressed,omitempty"`

	// EventsDigest / OutcomesDigest are #716's content identity over the rows the
	// report was computed over, RECOMPUTED and COMPARED by this build since #740.
	EventsDigest   *manifestDigest `json:"events_digest,omitempty"`
	OutcomesDigest *manifestDigest `json:"outcomes_digest,omitempty"`
	// DigestsOmitted is the emitter's stated reason for their absence, echoed
	// into the NOT PINNED line so an operator learns WHY rather than only THAT.
	DigestsOmitted string `json:"digests_omitted,omitempty"`

	// RepoScopeExcluded is #751's pin of the ONE published figure a scoped
	// manifest's own digests structurally cannot cover: the repo-blind rows a
	// strict `?repo=` dropped, which /scores discloses as
	// data_quality.repo_scope_excluded. A POINTER because absent (a fleet-wide
	// manifest, or a pre-#751 emitter) and all-zero (a scoped window that genuinely
	// excluded nothing) are different facts, and the second is a real pin that a
	// later sentinel insert must be able to falsify.
	RepoScopeExcluded *manifestRepoScopeExcluded `json:"repo_scope_excluded,omitempty"`

	// Results is an ADDITIVE EXTENSION to the tiermanifest1 contract, defined
	// here (#718). The served emitter does NOT write it.
	//
	// 🔴 WHY IT HAS TO EXIST. The #715 contract pins the INPUTS — "THIS pricing
	// model was run against THESE logs" — and stops there. The maintainer's ask has a
	// third clause: "and it came up with THESE numbers". Without a recorded
	// output there is nothing to reproduce, and the headline line this command
	// exists to print ("TIER for alice moved 41.20 -> 39.85") is unwritable.
	//
	// omitempty, and a manifest without it still verifies: the input dimensions
	// are checked and the output comparison is reported as NOT PINNED. That is
	// strictly more useful than refusing, and it is what makes this command work
	// against the bare served manifest as it stands today — attach the /scores
	// body under `results.scores` to get the output half as well.
	Results *manifestResults `json:"results,omitempty"`

	// unknownFields is the set of keys present in the FILE that this binary does
	// not evaluate. Unexported, so it can never round-trip into a written
	// manifest; populated by loadManifest. See unknownManifestFields.
	unknownFields []string
}

// manifestWindow is the report's window RESOLVED: the instants the attribution
// queries bind, and the query values the re-run replays into /scores.
//
// 🔴 THE TWO RENDERINGS ARE NOT INTERCHANGEABLE AND THAT IS THE POINT. The
// manifest publishes RFC3339 instants; /api/v1/scores accepts ONLY the date
// grammar (internal/api parseWindowDate: YYYY-MM-DD, YYYY-MM, YYYY). A bound that
// is not midnight UTC therefore cannot be replayed at all, and resolveWindow says
// so as a COULD NOT CHECK rather than silently truncating it — truncation would
// re-run a DIFFERENT window and compare its numbers against this manifest's,
// which is a fabricated verdict in either direction. See resolveWindow.
type manifestWindow struct {
	Since, Until           time.Time // resolved; a zero Until is open-ended
	SinceQuery, UntilQuery string    // the /scores date-grammar rendering
}

type manifestPriceTable struct {
	Version       int    `json:"version"`
	EffectiveDate string `json:"effective_date"`
	TableHash     string `json:"table_hash"` // "tierpt1:<hex>"
	FileHash      string `json:"file_hash"`  // "sha256:<hex>"
}

type manifestRubric struct {
	Version int `json:"version"`
}

// manifestDigest mirrors api.manifestDigestJSON: the scheme-tagged value and the
// row count it covers.
//
// rows is decoded and REPORTED but never trusted as a check on its own — it is
// the emitter's claim about its own denominator, and the recomputation produces
// its own. Printing both is what turns "the digests differ" into "they differ AND
// the window now holds 4 rows where it held 3".
type manifestDigest struct {
	Value string `json:"value"`
	Rows  int64  `json:"rows"`
}

// manifestRepoScopeExcluded mirrors api.manifestRepoScopeExcludedJSON: the
// repo-blind rows a strict repo scope dropped from this report's window (#751).
//
// ⚠️ THE MONEY FIELD IS cost_micro, NOT cost_usd, AND THE MIRROR MUST STAY THAT
// WAY. /scores publishes cost_usd (a float); the manifest pins the stored
// integer, because this value is compared for EQUALITY and float equality over a
// JSON-round-tripped dollar figure can manufacture a divergence. See the emitter
// type for the full argument.
type manifestRepoScopeExcluded struct {
	TokenEvents int64 `json:"token_events"`
	CostMicro   int64 `json:"cost_micro"`
	Outcomes    int64 `json:"outcomes"`
}

// manifestKAnon mirrors api.manifestKAnonJSON.
type manifestKAnon struct {
	WithheldWindow  bool   `json:"withheld_window"`
	WithheldDigests bool   `json:"withheld_digests"`
	KAnonymity      int    `json:"k_anonymity"`
	Aggregation     string `json:"aggregation"`
	Reason          string `json:"reason"`
}

// manifestWatermarks mirrors the served watermark block, INCLUDING its split into
// `window` and `ledgers`.
//
// 🔑 THE SPLIT IS CARRIED RATHER THAN FLATTENED, and store.Watermarks calls it
// "THE MOST IMPORTANT THING THIS TYPE SAYS": the two halves are read over
// DIFFERENT predicates (window-and-scope vs install-wide) and carry DIFFERENT
// disclosure risk — `window` is withheld entirely in an anonymized mode, which
// is why it is a pointer here. A flattening decode-side mirror would make
// "withheld for k-anonymity" and "this ledger has no watermark" the same reading.
type manifestWatermarks struct {
	Window  *manifestWindowWatermarks `json:"window,omitempty"`
	Ledgers *manifestLedgerWatermarks `json:"ledgers,omitempty"`
}

// manifestWindowWatermarks is the position of the two windowed row sequences.
//
// ⚠️ token_event_count IS NOT "rows in the report's window". Both token fields
// are read over the ATTRIBUTION BAND [since - AttributableWindow, until), 14 days
// wider than the outcome side, because the scoring path funds an outcome from
// token events up to 14 days before `since`. Every read in this file that
// compares against them must use the same band — see verifyDimsWatermarks — or
// the deletion arm computes a survivor count over one population and subtracts it
// from a pin taken over another.
type manifestWindowWatermarks struct {
	MaxTokenEventID *int64 `json:"max_token_event_id"`
	// 🔴 THE ROW COUNTS ARE THE ONLY PIN THAT CAN SEE A DELETION, and without
	// them this command returns a confident REPRODUCED over a database that has
	// lost rows. MAX(id) is INVARIANT under deleting any non-maximal row, and a
	// deletion writes to none of the five audit ledgers — so an EraseDeveloper
	// (GDPR Art. 17) run against the window leaves every watermark reading
	// UNCHANGED while the figures move.
	TokenEventCount *int64 `json:"token_event_count"`
	MaxOutcomeID    *int64 `json:"max_outcome_id"`
	OutcomeCount    *int64 `json:"outcome_count"`
}

// manifestLedgerWatermarks is the position of the five append-only mutation
// ledgers, install-wide.
//
// 🔴 THEY NAME THE **ROW** LEDGERS, NOT THE AGGREGATE ONES, AND THE DIFFERENCE
// IS NOT COSMETIC (#741). `reprice_audit` and `repo_repair_audit` are written
// once per RUN; `reprice_row_audit` and `repo_repair_row_audit` are written once
// per MUTATED ROW, in the same transaction as the UPDATE, and they are what a
// replay needs. They are also SEPARATE AUTOINCREMENT SEQUENCES: an id from one is
// not comparable with an id from the other, so a verifier bounding its queries on
// the aggregate id while the emitter pinned the row id would be comparing two
// unrelated numbers and calling the result an attribution. This binary used to do
// exactly that.
// ⭐ EACH LEDGER PUBLISHES A MAX **AND** A COUNT, AND THE COUNT IS NOT REDUNDANT.
// MAX(id) rises on an INSERT and is invariant under deleting any non-maximal row;
// only the count moves when a row is REMOVED. These tables are append-only by
// intent, but EraseDeveloper (GDPR Art. 17) hard-deletes from them — so a
// shrinking count is a real, reachable event, and it is a change to the very
// evidence a manifest's provenance rests on. Decoding these without checking them
// would leave five published pins unexamined on a green run.
type manifestLedgerWatermarks struct {
	MaxQualityHistoryID      *int64 `json:"max_quality_history_id"`
	QualityHistoryCount      *int64 `json:"quality_history_count"`
	MaxRepriceRowAuditID     *int64 `json:"max_reprice_row_audit_id"`
	RepriceRowAuditCount     *int64 `json:"reprice_row_audit_count"`
	MaxCostCorrectionAuditID *int64 `json:"max_cost_correction_audit_id"`
	CostCorrectionAuditCount *int64 `json:"cost_correction_audit_count"`
	MaxRepoRepairRowAuditID  *int64 `json:"max_repo_repair_row_audit_id"`
	RepoRepairRowAuditCount  *int64 `json:"repo_repair_row_audit_count"`
	// #849: push outcomes superseded by a merged PR or re-owned in place.
	MaxPushOutcomeAuditID *int64 `json:"max_push_outcome_audit_id"`
	PushOutcomeAuditCount *int64 `json:"push_outcome_audit_count"`
}

// window / ledgers return the sub-blocks or their zero value, so every caller
// reads through ONE nil-safe accessor rather than repeating the nil checks. The
// zero value is every field nil, which each dimension renders as its own NOT
// PINNED line — see verifyDimsWatermarks.
func (m reportManifest) window() manifestWindowWatermarks {
	if m.Watermarks == nil || m.Watermarks.Window == nil {
		return manifestWindowWatermarks{}
	}
	return *m.Watermarks.Window
}

func (m reportManifest) ledgers() manifestLedgerWatermarks {
	if m.Watermarks == nil || m.Watermarks.Ledgers == nil {
		return manifestLedgerWatermarks{}
	}
	return *m.Watermarks.Ledgers
}

// manifestResults carries the report the manifest is a manifest OF.
//
// Scores is the /api/v1/scores response body VERBATIM, as raw JSON. Storing the
// served bytes rather than a hand-picked subset is deliberate: any invented
// projection would drift from the endpoint it claims to pin, and the comparison
// would then be a proxy for reproduction rather than reproduction itself.
type manifestResults struct {
	Scores json.RawMessage `json:"scores"`
}

// scoresEnvelope is the partial decode of a /scores body used ONLY to name the
// rows whose TIER moved. The full-body canonical comparison is what decides
// whether anything moved at all; this decides what to SAY about it, so it can
// safely ignore every field it does not mention.
type scoresEnvelope struct {
	Developers []scoresRow `json:"developers"`
	Teams      []scoresRow `json:"teams"`
}

type scoresRow struct {
	Developer string  `json:"developer"`
	Team      string  `json:"team"`
	TIER      float64 `json:"tier"`
}

// label is the row's identity: the developer id in developer mode, the cohort
// label in an anonymized mode. Never both — the two modes are disjoint.
func (r scoresRow) label() string {
	if r.Developer != "" {
		return r.Developer
	}
	return r.Team
}

func (e scoresEnvelope) rows() []scoresRow {
	out := make([]scoresRow, 0, len(e.Developers)+len(e.Teams))
	out = append(out, e.Developers...)
	out = append(out, e.Teams...)
	return out
}

// ---------------------------------------------------------------------------
// The verification result (a pure value, so the printer is testable)
// ---------------------------------------------------------------------------

// dimStatus is the three-valued state of ONE input dimension. It is three-valued
// for the same reason the exit code is: "we did not check this" must never
// render as "this is unchanged".
type dimStatus int

const (
	// 🔴 dimUnknown IS THE ZERO VALUE, AND THAT IS THE WHOLE POINT. A file whose
	// thesis is that "we did not check this" must never render as "this is
	// unchanged" cannot afford UNCHANGED to be what a forgotten field defaults to.
	// Every error path in this package returns verifyDim{}, and with UNCHANGED at
	// iota 0 each of those was one refactor away from silently asserting
	// agreement. label() renders this as UNKNOWN, which is loud and wrong-looking
	// on purpose.
	dimUnknown dimStatus = iota
	dimUnchanged
	dimChanged
	// dimNotPinned: the manifest carries no value for this dimension, so
	// nothing can be said about it. Reported, never counted as agreement.
	dimNotPinned
	// dimNotCheckable: the manifest pins it, but this build cannot evaluate it.
	// Reported, never counted as agreement, and never a divergence — we have no
	// reading either way. ⚠️ This used to name events_digest as the example, which
	// is now the OPPOSITE of the truth (#740 made the digests the strongest thing
	// this build CAN evaluate) and would tell a reader that #740 never landed. The
	// real cases: a digest under a scheme this binary does not know, a manifest
	// whose token_since names a different attribution band, a self-contradictory
	// digest pin, and a field a NEWER tierd wrote.
	// ⚠️ "a digest pinned beside a repo scope" was on that list until #747 gave
	// the #716 read a repo predicate; it is now CHECKABLE, and the recomputation
	// carries the manifest's canonicalized scope.
	dimNotCheckable
)

// label is the status TOKEN printed in its own fixed-width column.
//
// 🔴 THE TOKEN IS A SEPARATE COLUMN ON PURPOSE, AND THIS IS NOT COSMETIC. When
// the status was folded into the prose, "UNCHANGED" contained "CHANGED" as a
// substring — so any check for the wrong attribution (the whole point of the A
// and B arms) matched on a correct report. A guard written against that layout
// is unsound BY CONSTRUCTION, and papering over it in the test would leave the
// same trap for the next reader. The column makes the status exactly matchable.
func (s dimStatus) label() string {
	switch s {
	case dimChanged:
		return "CHANGED"
	case dimNotPinned:
		return "NOT PINNED"
	case dimNotCheckable:
		return "NOT CHECKED"
	case dimUnchanged:
		return "UNCHANGED"
	}
	return "UNKNOWN"
}

// verifyDim is one attribution line.
type verifyDim struct {
	Name   string // the column label, e.g. "token_events"
	Status dimStatus
	// Detail is the prose right of the status column. It must NOT restate the
	// status word — Status owns that, and two sources for one fact drift.
	// Every client-controlled value in it is logsafe-wrapped by its producer.
	Detail string
	// bandWithheld marks a PINNED comparison withheld because the manifest's
	// attribution band disagrees with this binary's (#1033). verdict reads it.
	bandWithheld bool
}

// scoreMove is one row whose score changed between the manifest and the re-run.
type scoreMove struct {
	Label string // developer id or cohort label — client-controlled
	Was   float64
	Now   float64
	// Kind distinguishes a moved row from one that appeared or vanished, which
	// are different facts: an appearing developer is a population change, not a
	// score change.
	Kind string // "moved" | "appeared" | "vanished"
}

// verifyResult is everything printVerifyReport needs. Kept as data so the
// printer is a pure function of it — the same split reprice.go and repairrepo.go
// use so report_forge_test.go can observe the bytes an operator sees.
type verifyResult struct {
	Schema string
	// Since/Until are the manifest's own WIRE strings, printed verbatim so the
	// header line shows what the manifest said rather than this binary's
	// re-rendering of it.
	Since, Until string
	Scope        string
	Aggregation  string
	K            int

	// ResultsPinned reports whether the manifest carried a results block at all.
	ResultsPinned bool
	// ResultsIdentical is meaningful only when ResultsPinned.
	ResultsIdentical bool
	Moves            []scoreMove

	Dims []verifyDim

	ToolVersionManifest, ToolVersionNow string
	CommitManifest, CommitNow           string
}

// diverged reports whether anything the manifest PINNED actually moved.
//
// 🔴 The rule, stated once: a divergence is a moved OUTPUT or a moved DATA
// input. tool_version and commit drift are reported prominently but do NOT on
// their own make this true, and that is a deliberate ruling, not laxity. The
// #710 guarantee is that a re-run reproduces "bit-identically on the same binary
// and architecture" — a re-run on a DIFFERENT binary that lands on identical
// numbers is a STRONGER result than the guarantee asks for, not a weaker one.
// Failing it would also make the tool useless in exactly the audit scenario it
// exists for: someone verifying last year's published figure with this year's
// binary. The drift is never hidden — it is printed on its own line, and listed
// as a suspect whenever the numbers did move.
func (r verifyResult) diverged() bool {
	if r.ResultsPinned && !r.ResultsIdentical {
		return true
	}
	for _, d := range r.Dims {
		if d.Status == dimChanged {
			return true
		}
	}
	return false
}

// quorumDims are the dimensions whose presence means this run examined the
// DATA. Deliberately NOT every dimension.
//
// 🔴 price_table and rubric are excluded, and that is the correction to the first
// draft. Both describe THIS BINARY, not the database: rubric.version is a
// compiled-in constant, and the active price table travels with the binary and
// prices nothing on the read path (#233). A manifest carrying only those two
// would have returned rc 0 "REPRODUCED: every pinned input is unchanged" having
// examined not one row — a green tick earned entirely by comparing the binary to
// itself. The seven watermark dimensions (#849 added push reconciliations) and
// the two digests are what say anything about the data. ⚠️ This read "only the
// six watermark dimensions" until #740 put the digests in the map below — a count nobody re-checked while the thing it
// counted changed, which is the shape this file keeps rediscovering.
//
// ⭐ THE TWO DIGESTS ARE IN, and they are the strongest members of this set: they
// are the only dimensions that read the row CONTENTS, so a manifest pinning
// nothing but a digest has still had its data examined — more thoroughly than one
// pinning all seven watermarks, which are structurally blind to an in-place edit.
var quorumDims = map[string]bool{
	"token_events": true, "outcomes": true, "quality revisions": true,
	"reprice": true, "cost corrections": true, "repo repairs": true,
	"push reconciliations": true, "events_digest": true, "outcomes_digest": true,
}

// ⛔ repo_scope_excluded (#751) IS DELIBERATELY NOT IN THE MAP ABOVE, and the
// omission is the point rather than an oversight. It is the one data dimension
// that reads rows OUTSIDE the report's own predicate — the repo-blind sentinel
// rows a strict `?repo=` dropped. A manifest pinning only that would have had
// the fleet's unattributable rows examined and its own report's rows not at all;
// reporting "REPRODUCED: every pinned input is unchanged" over it would be the
// fail-open shape this whole file exists to prevent, with a data dimension as the
// alibi. It still makes a run DIVERGE when it moves — diverged() reads every
// dimension, not this map.

// checkedAnything reports whether this run actually verified something. A
// manifest pinning no results and no watermarks has told us nothing to check
// ABOUT THE DATA, and reporting rcReproduced for it would be the purest form of
// the fail-open defect this whole program exists to prevent.
func (r verifyResult) checkedAnything() bool {
	if r.ResultsPinned {
		return true
	}
	for _, d := range r.Dims {
		if quorumDims[d.Name] && (d.Status == dimUnchanged || d.Status == dimChanged) {
			return true
		}
	}
	return false
}

// verdict is the SINGLE source of truth for both the process exit code and the
// report's headline.
//
// 🔴 IT EXISTS BECAUSE THE TWO CAN DISAGREE, AND ONE ARRANGEMENT OF THAT
// DISAGREEMENT IS EXACTLY THE FAILURE THIS PROGRAM EXISTS TO PREVENT. The first
// draft printed the report and only THEN checked whether anything had been
// verified — so a manifest that pinned nothing produced "REPRODUCED" on stdout
// and rc 2 on the process. An operator (or a script) reading stdout would have
// seen a reproduction over a contract with nothing in it. That is the CVE
// re-scan's "nothing was established" announced as a pass, one layer down.
//
// CLAUDE.md's rule for the merge gate is the same rule: do not pipe stdout and
// expect a verdict. Here the stronger guarantee is available and taken — the
// headline and the exit code are computed once, by this function.
//
// An unchecked pinned data dimension prevents reproduction even when other
// comparisons are unchanged. Anything that did move still diverges.
//
// A detected change is checked FIRST: a CHANGED pin is a refutation whether or
// not any quorum comparison survived, so it is never reported as "nothing was
// refuted".
func (r verifyResult) verdict() int {
	if r.diverged() {
		return rcDiverged
	}
	if !r.checkedAnything() {
		return rcCannotCheck
	}
	if r.dataPinsNotChecked() {
		return rcCannotCheck
	}
	return rcReproduced
}

// bandDisagrees reports whether the attribution band line is present: the
// manifest's band differs from this binary's, or could not be parsed.
func (r verifyResult) bandDisagrees() bool {
	for _, d := range r.Dims {
		if d.Name == "attribution band" {
			return true
		}
	}
	return false
}

// dataPinsNotChecked blocks reproduction for unchecked quorum pins or band-withheld
// comparisons. Unknown pins and fleet-wide repo_scope_excluded do not block it.
func (r verifyResult) dataPinsNotChecked() bool {
	for _, d := range r.Dims {
		if d.bandWithheld || (quorumDims[d.Name] && (d.Status == dimNotCheckable || d.Status == dimUnknown)) {
			return true
		}
	}
	return false
}

// unattributed reports the awkward case worth naming out loud: the numbers moved
// and NOTHING we could check moved with them. That is either a dimension this
// manifest did not pin, or a genuine anomaly — and an operator must be told
// which of those two situations they are in rather than left staring at a wall
// of UNCHANGED.
func (r verifyResult) unattributed() bool {
	if !r.ResultsPinned || r.ResultsIdentical {
		return false
	}
	for _, d := range r.Dims {
		if d.Status == dimChanged {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The command
// ---------------------------------------------------------------------------

// runVerifyReportCmd parses flags, loads the manifest, recomputes the report and
// prints the attribution, returning the process exit code (0 reproduced, 1
// diverged, 2 could not check; a sealed month's manifest adds 3 and 4, see
// verifysealed.go) so main's os.Exit stays the single exit point.
// Output goes to the injected writers so the subcommand is testable through
// dispatch (mirrors reprice/repair-repo/doctor).
//
// ⚠️ A flag-parse error returns 1, not 2. That is not an inconsistency: a
// misspelled flag is a usage error and the operator is right there reading it,
// whereas 2 is reserved for "the verification you asked for did not happen",
// which is a claim about the DATA. Keeping usage errors at 1 also keeps this
// command aligned with every other subcommand's convention.
func runVerifyReportCmd(args []string, stdout, stderr io.Writer) int {
	// The manifest is a POSITIONAL argument (`tierd verify-report m.json --db
	// …`), and Go's flag package stops at the first non-flag token. Lift it out
	// before Parse so flags may follow it, which is the shape the issue
	// specifies and the shape an operator will type.
	var manifestPath string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		manifestPath = args[0]
		args = args[1:]
	}

	// 🔴 --emit IS REFUSED BY NAME, NOT MERELY UNDEFINED (#741). It was this
	// command's stopgap manifest producer "until #715 lands its emitter"; #715 has
	// landed, and two emitters of one contract is how they drift — measurably, in
	// this case, since the two had already diverged into mutual unreadability
	// while both stamped `tiermanifest1`. Left to Go's flag package the operator
	// gets "flag provided but not defined: -emit", which tells them the flag is
	// gone and NOT where the manifest now comes from. ⛔ A removed flag that
	// parses and does nothing would be worse still: a "verification" that silently
	// wrote nothing and exited 0 is the fail-open shape this whole command exists
	// to refuse. Checked BEFORE fs.Parse so the four --emit-only flags it used to
	// carry (--since/--until/--repo/--aggregation) also land here rather than as
	// four separate undefined-flag errors.
	fs := flag.NewFlagSet("verify-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path to verify the report against. NEVER written to or migrated: the run works from a VACUUM INTO snapshot, so it needs free space alongside the database")
	pricesPath := fs.String("prices", os.Getenv("TIER_PRICES"), "path to the price-table YAML the report was served under (#68); empty uses the embedded default. The re-run is served and stamped under this table. Passing a different table moves the stamp, reported on the price_table line. An override whose version collides with a different table already recorded in the database is an identity error: it is refused with exit 2; give the override its own version. Stored cost_micro is immutable (#233), so this does NOT re-price stored costs. On a sealed manifest the table is validated but unused")
	// Refused AFTER the FlagSet is built, so the scan can ask fs which flags take
	// a separate value instead of restating that list — see refuseRemovedEmitFlag.
	if rc, refused := refuseRemovedEmitFlag(fs, args, stderr); refused {
		return rc
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if extra := fs.Args(); len(extra) > 0 {
		_, _ = fmt.Fprintf(stderr, "verify-report: unexpected argument %s (usage: tierd verify-report <manifest.json> --db PATH)\n", logsafe.Str(extra[0]))
		return 1
	}
	if manifestPath == "" {
		_, _ = fmt.Fprintln(stderr, "verify-report: a manifest path is required (usage: tierd verify-report <manifest.json> --db PATH). "+
			"Obtain one from the running server: GET /api/v1/report_manifest?since=YYYY-MM-DD[&until=…][&repo=…], "+
			"or on a team/division server, which serves sealed months only, GET /api/v1/report_manifest?period=YYYY-MM. "+
			"To compare the NUMBERS as well as the inputs, attach the /api/v1/scores body for the same window under a top-level \"results\": {\"scores\": …} key.")
		return 1
	}

	// The database must EXIST. store.Open creates a missing file and applies the
	// schema, so without this guard a typo'd --db would produce a valid, empty
	// database, recompute an empty report, and hand back a confident answer
	// about nothing. 🔴 This is rc 2, NOT rc 1 — reprice/repair-repo return 1
	// here because they only have two codes to spend; the whole point of this
	// command's third code is that "I could not look" is not "it changed".
	if _, err := os.Stat(*dbPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "verify-report: --db %s: %v (verify-report reads an existing database; NOTHING was verified)\n", logsafe.Str(*dbPath), logsafe.Err(err))
		return rcCannotCheck
	}

	// The re-run must be served and stamped under the table the report was
	// served under. A different --prices table moves the stamp and is attributed
	// on the price_table line; stored costs are not re-priced. An override whose
	// version collides with a different table already recorded in the database
	// is an identity error, refused with exit 2; give the override its own version.
	//
	// 🔴 NOT loadPricesOverride. That helper prints to os.Stderr directly and
	// calls os.Exit(1) on a bad file (main.go) — which in THIS command would
	// report "could not check" as "diverged", in the one binary whose entire
	// premise is that those are different facts. It would also make the path
	// untestable through dispatch. LoadPriceTable activates the table used by
	// the snapshot and the report re-run while returning errors to this command.
	info := store.ActivePriceTableInfo()
	if *pricesPath != "" {
		loaded, err := store.LoadPriceTable(*pricesPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "verify-report: --prices: %v (NOTHING was verified)\n", logsafe.Err(err))
			return rcCannotCheck
		}
		info = loaded
	}

	// Cancel cleanly on SIGINT/SIGTERM, same as reprice/repair-repo. A
	// verification is a snapshot plus a full-window scan plus the post-watermark
	// reads; an operator who Ctrl-Cs must get a cancelled read (which surfaces as
	// an error → rc 2, "could not check"), never a partial attribution reported
	// as a result. Established before the snapshot so a Ctrl-C during the VACUUM
	// is also clean.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 🔴 WORK FROM A SNAPSHOT, NEVER FROM THE OPERATOR'S FILE. store.Open applies
	// the full migration chain, and one of those migrations backfills
	// token_events.price_version — so verifying an archived database in place
	// would rewrite the provenance column the audit exists to check. store.Backup
	// is documented as the one path that "opens its OWN minimal connection and
	// runs NO migrations", and it is transactionally consistent, which the
	// attribution also wants: every read below sees ONE instant.
	snapDir, err := os.MkdirTemp("", "tierd-verify-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify-report: create snapshot dir: %v (NOTHING was verified)\n", logsafe.Err(err))
		return rcCannotCheck
	}
	defer func() { _ = os.RemoveAll(snapDir) }()
	snapPath := filepath.Join(snapDir, "verify-snapshot.db")
	if err := store.Backup(ctx, *dbPath, snapPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "verify-report: snapshot %s: %v (NOTHING was verified; a verification needs free space alongside the database)\n",
			logsafe.Str(*dbPath), logsafe.Err(err))
		return rcCannotCheck
	}

	db, err := store.Open(snapPath)
	if err != nil {
		if *pricesPath != "" && strings.Contains(err.Error(), "two different price tables share one version number") {
			err = fmt.Errorf("--prices version collision for version %d: give the override its own version; this identity error is refused with exit 2", info.Version)
		}
		_, _ = fmt.Fprintf(stderr, "verify-report: open snapshot: %v (NOTHING was verified)\n", logsafe.Err(err))
		return rcCannotCheck
	}
	defer func() { _ = db.Close() }()

	// A SECOND, read-only handle on the SAME snapshot, for the POST-WATERMARK
	// attribution reads.
	//
	// 🔑 WHY IT SURVIVED #741, WHEN readWatermarks DID NOT. The reason originally
	// given here — "#714/#715/#716 are in flight in internal/store and adding them
	// there would collide" — is spent: #715 landed, its contract is what this
	// command now consumes, and the private watermark reader that stood beside it
	// is deleted. What is left on this handle is a DIFFERENT question from
	// anything the store exposes. store.ReportWatermarks answers "where is each
	// ledger NOW"; these queries answer "which rows arrived, were removed, or were
	// audited SINCE the id the manifest pinned", per ledger, joined to this
	// report's own window and scope. That is verify-report's question, it has one
	// caller, and hoisting it into internal/store would put a CLI's attribution
	// vocabulary in the storage layer for no second consumer.
	//
	// ⚠️ WHAT IT MUST NOT BECOME is a second reader of something the store already
	// defines — that is exactly what #741 removed. Before adding a query here, ask
	// whether internal/store already answers it.
	//
	// mode=ro is the same posture store.InspectIdentities uses for the #475 safety
	// guarantee. Opened AFTER store.Open so the snapshot's schema and migrations
	// are already applied — the digests read through `db` see the same bytes.
	ro, err := sql.Open("sqlite", readOnlySnapshotDSN(snapPath))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify-report: open read-only handle: %v (NOTHING was verified)\n", logsafe.Err(err))
		return rcCannotCheck
	}
	// One connection: the attribution reads are strictly sequential, and a pool
	// here would open WAL readers nothing uses. Same reasoning as store.Backup's
	// and the Opencode collector's single-connection handles.
	ro.SetMaxOpenConns(1)
	ro.SetMaxIdleConns(1)
	defer func() { _ = ro.Close() }()

	m, err := loadManifest(manifestPath)
	if errors.Is(err, errSealedManifest) {
		if *pricesPath != "" {
			_, _ = fmt.Fprintf(stdout, "  %-21s %-11s %s\n", "--prices:", "NOT CHECKED", "validated but unused for sealed verification: a sealed manifest is verified from its stored body and fold inputs; the informational live recompute uses the override")
		}
		return runVerifySealed(ctx, db, manifestPath, stdout, stderr, *pricesPath != "")
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify-report: %v (NOTHING was verified)\n", logsafe.Err(err))
		return rcCannotCheck
	}

	res, err := verifyAgainst(ctx, db, ro, info, m)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify-report: %v (NOTHING was verified)\n", logsafe.Err(err))
		return rcCannotCheck
	}

	printVerifyReport(stdout, res)

	rc := res.verdict()
	switch {
	case rc == rcCannotCheck && res.dataPinsNotChecked():
		_, _ = fmt.Fprintln(stderr, "verify-report: this manifest pins data that was NOT CHECKED (see the NOT "+
			"CHECKED lines and LIMITS), so a change there could not have been seen. Refusing to report a "+
			"reproduction.")
		for _, d := range res.Dims {
			if d.bandWithheld {
				_, _ = fmt.Fprintf(stderr, "  %s: %s (#1033).\n", d.Name, d.Detail)
			} else if quorumDims[d.Name] && (d.Status == dimNotCheckable || d.Status == dimUnknown) {
				_, _ = fmt.Fprintf(stderr, "  %s: %s\n", d.Name, d.Detail)
			}
		}
	case rc == rcCannotCheck:
		_, _ = fmt.Fprintln(stderr, "verify-report: this manifest pins no results, no watermarks and no window "+
			"digests, so nothing about the DATA was examined — a price-table or rubric pin only compares this "+
			"binary to itself. Refusing to report a reproduction (#718).")
	}
	return rc
}

// refuseRemovedEmitFlag reports the deleted --emit lane by name.
//
// It scans the RAW argv rather than registering a hidden flag, so `--emit`,
// `-emit`, `--emit=x` and the four options that were documented as "--emit only"
// all produce one sentence naming the replacement instead of a bare parse error.
// rc 1: a removed flag is a USAGE error, and this command reserves rc 2 for "the
// verification you asked for did not happen", which is a claim about the DATA.
func refuseRemovedEmitFlag(fs *flag.FlagSet, args []string, stderr io.Writer) (int, bool) {
	// --since/--until/--repo/--aggregation/-k existed ONLY to configure --emit;
	// there is no verify-time meaning for any of them (the window, scope and mode
	// come from the manifest), so each is reported here rather than left to look
	// like a flag that might do something.
	removed := map[string]string{
		"emit":        "--emit wrote a manifest; the server does that now",
		"since":       "--since configured --emit only; the window comes from the manifest",
		"until":       "--until configured --emit only; the window comes from the manifest",
		"repo":        "--repo configured --emit only; the scope comes from the manifest",
		"aggregation": "--aggregation configured --emit only; the mode comes from the manifest",
		"k":           "-k configured --emit only; the floor comes from the manifest",
	}
	// A flag this command still HAS may take its value as a SEPARATE argv token,
	// and that token must not be read as a flag name: `--db -k`, a database file
	// literally named "-k", is pathological but legal and must reach the ordinary
	// "no such file" refusal rather than "-k was REMOVED".
	//
	// 🔑 ASKED OF fs, NOT RESTATED AS A LIST. A hand-written {"db","prices"} map
	// is a second copy of the FlagSet declared above, and this file's own
	// verifyDimsWatermarks comment states the rule it would break: "two sources for
	// one list is how the two drift". Add a third value-taking flag, forget the
	// map, and `--newflag -k` refuses a legitimate invocation. fs.Lookup answers it
	// from the real declaration; a bool flag never consumes the next token, which
	// is what the IsBoolFlag probe (flag's own interface) distinguishes.
	takesSeparateValue := func(name string) bool {
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		return !ok || !b.IsBoolFlag()
	}
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if takesSeparateValue(name) && !hasValue {
			skipNext = true
			continue
		}
		why, ok := removed[name]
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(stderr, "verify-report: -%s was REMOVED (#741): %s.\n"+
			"  A manifest now comes from the running server, which is its only emitter:\n"+
			"    GET /api/v1/report_manifest?since=YYYY-MM-DD[&until=YYYY-MM-DD][&repo=owner/name]\n"+
			"  or on a team/division server, which serves sealed months only:\n"+
			"    GET /api/v1/report_manifest?period=YYYY-MM\n"+
			"  To compare the NUMBERS as well as the inputs, attach the /api/v1/scores body for the\n"+
			"  same window under a top-level \"results\": {\"scores\": …} key.\n", name, why)
		return 1, true
	}
	return 0, false
}

// loadManifest reads and validates the manifest's SCHEME. An unknown scheme is
// an error (→ rc 2) rather than a best-effort decode: the fields of a future
// tiermanifest2 could mean something else entirely, and a verifier that guesses
// is worse than one that refuses.
func loadManifest(path string) (reportManifest, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- an operator-supplied manifest path is the command's whole input
	if err != nil {
		return reportManifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m reportManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return reportManifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Schema == api.SealedManifestSchema {
		return reportManifest{}, errSealedManifest
	}
	if m.Schema != manifestSchemaTag {
		return reportManifest{}, fmt.Errorf("manifest %s: manifest_schema is %q, want %q — this binary cannot interpret it, so nothing was checked",
			path, m.Schema, manifestSchemaTag)
	}
	if m.Since == "" {
		return reportManifest{}, fmt.Errorf("manifest %s: since is empty; a report with no window bound cannot be re-run", path)
	}
	unknown, err := unknownManifestFields(raw)
	if err != nil {
		return reportManifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	m.unknownFields = unknown
	return m, nil
}

// unknownManifestFields returns the keys a NEWER emitter wrote that this binary
// does not evaluate, as dotted paths ("watermarks.row_counts").
//
// 🔴 THE RULING, AND WHY IT IS NOT json.Decoder.DisallowUnknownFields. The first
// draft REFUSED such a manifest outright (rc 2), on the reasoning that a pin
// nobody examined must not sit under a green tick. Right instinct, wrong
// mechanism, for a concrete reason: #715's own spec adds row counts to the
// watermark block, so the very next sibling PR would emit a `tiermanifest1` this
// binary rejects ENTIRELY — and "a newer tierd wrote this" would then arrive as
// COULD NOT CHECK, indistinguishable from a corrupt file.
//
// The SCHEME TAG is the compatibility lever — that is what a scheme tag is for,
// and loadManifest above already refuses a `tiermanifest2`. Within the scheme, a
// manifest is additively extensible. So an unknown field is neither accepted
// silently nor fatal: it becomes its own NOT CHECKED dimension, named on its own
// line and disclosed in LIMITS. Nothing is ever skipped without saying so.
//
// 🔑 IT RECURSES, AND THE DENOMINATOR IS REFLECTION, NOT A HAND-TYPED LIST. The
// second draft walked only the TOP level against a hand-maintained key map,
// which missed the exact extension its own justification cites — #715's counts
// land INSIDE `watermarks` — and made the map a second source of truth that a
// new field could silently desynchronise. Deriving the known set from the struct
// tags removes both defects at once: there is nothing to keep in step.
func unknownManifestFields(raw []byte) ([]string, error) {
	return unknownFieldsOf(raw, reflect.TypeOf(reportManifest{}))
}

// unknownFieldsOf is unknownManifestFields against manifest type t.
func unknownFieldsOf(raw []byte, t reflect.Type) ([]string, error) {
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	var out []string
	collectUnknownFields(top, t, "", &out)
	// Deterministic: map iteration is unordered, and a diagnostic whose line
	// content changes run to run cannot be diffed.
	sort.Strings(out)
	return out, nil
}

// collectUnknownFields walks a decoded JSON value against the Go type that is
// supposed to describe it, appending the dotted path of every object key the
// type has no json tag for.
//
// A non-struct target is opaque and terminates the walk: that is what stops it
// descending into results.scores (a json.RawMessage — deliberately an unparsed
// served body, not a contract this binary owns) and into the *int64 watermarks.
func collectUnknownFields(v any, t reflect.Type, prefix string, out *[]string) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	known := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		known[name] = f.Type
	}
	for k, child := range obj {
		ft, ok := known[k]
		if !ok {
			*out = append(*out, prefix+k)
			continue
		}
		collectUnknownFields(child, ft, prefix+k+".", out)
	}
}

// verifyDimUnknownPins reports fields this binary could not evaluate.
// ok is false when there are none, so a manifest this binary fully understands
// prints no line at all.
func verifyDimUnknownPins(unknown []string) (verifyDim, bool) {
	if len(unknown) == 0 {
		return verifyDim{}, false
	}
	return verifyDim{
		Name:   "unknown pins",
		Status: dimNotCheckable,
		Detail: fmt.Sprintf("this manifest pins %d field(s) this binary does not evaluate: %s — these may come from a NEWER tierd or a hand-written or misspelled key; those pins went unexamined",
			len(unknown), logsafe.Join(unknown, maxListedInReport)),
	}, true
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// verifyAgainst recomputes the manifest's report and builds the attribution.
//
// An error return means COULD NOT CHECK (rc 2) — the caller must never treat it
// as a pass or as a divergence.
func verifyAgainst(ctx context.Context, db *store.DB, ro *sql.DB, info store.PriceTableInfo, m reportManifest) (verifyResult, error) {
	res := verifyResult{
		Schema:              m.Schema,
		Since:               m.Since,
		Until:               m.Until,
		Scope:               m.Repo,
		Aggregation:         m.Aggregation,
		ToolVersionManifest: m.ToolVersion,
		ToolVersionNow:      version,
		CommitManifest:      m.Commit,
		CommitNow:           commit,
	}

	mode, err := resolveAggregationMode(m.Aggregation)
	if err != nil {
		return verifyResult{}, fmt.Errorf("manifest aggregation: %w", err)
	}
	// 🔴 IN AN ANONYMIZED MODE THE FLOOR MUST BE PINNED, AND A NIL k IS NOT ZERO.
	// The emitter writes `k` only in an anonymized mode precisely so the two are
	// distinguishable; re-running a team report at k=0 would compare a floored
	// published report against an UNFLOORED recomputation and call the difference
	// a divergence — while having computed, in this process, exactly the cohort
	// the floor exists to withhold.
	var k int
	if mode.Anonymized() {
		if m.K == nil {
			return verifyResult{}, fmt.Errorf("manifest declares %s aggregation but pins no k: an anonymized report "+
				"cannot be re-run without its k-anonymity floor, and guessing one would recompute a DIFFERENT report", mode.String())
		}
		k = *m.K
		if err := validateKAnonymity(k); err != nil {
			return verifyResult{}, fmt.Errorf("manifest k: %w", err)
		}
	}
	res.K = k

	// The window is resolved ONCE, here, and every consumer below takes the same
	// value: the re-run replays its query rendering and the attribution binds its
	// instants. Resolving it twice is how a re-run and an attribution end up
	// windowing different rows while both look correct.
	window, err := resolveWindow(m)
	if err != nil {
		return verifyResult{}, err
	}

	// 🔴 CANONICALIZE THE SCOPE ONCE, AND FAIL CLOSED. This is the seam where the
	// re-run and the attribution could silently disagree, and the disagreement is
	// invisible: /api/v1/scores puts ?repo= through repoid.Canonical (lowercase,
	// strip .git and stray slashes, to a FIXED POINT), while the attribution SQL
	// binds the raw string into `repo = ?`. A manifest scoped to "Acme/Tier" would
	// therefore re-run against acme/tier while every post-watermark query matched
	// ZERO rows — seven confident UNCHANGED lines over a window nothing looked at.
	// Identical readings, opposite meanings: the #657 shape exactly.
	//
	// An unparseable scope is rc 2, never a silent fall-back to fleet-wide: a
	// fleet-wide read presented as a scoped one is #590 wearing a filter.
	scope := m.Repo
	if scope != "" {
		canon, ok := repoid.Canonical(scope)
		if !ok {
			return verifyResult{}, fmt.Errorf("manifest repo %q is not a canonical owner/repo — this binary cannot reproduce a scoped report it cannot resolve, and reading it fleet-wide would present an unscoped figure as a scoped one", scope)
		}
		scope = canon
	}
	res.Scope = scope

	// ⚠️ REFUSED UP FRONT, not by letting the handler 400. /scores rejects ?repo=
	// in any anonymized mode (#185/#270): narrowing to one repository can shrink a
	// cohort below the k-anonymity floor. Such a report was never servable, so
	// such a manifest was never emitted by a real /scores — and an operator
	// deserves that sentence rather than a raw HTTP body.
	if scope != "" && mode.Anonymized() {
		return verifyResult{}, fmt.Errorf("manifest pins repo %q with %s aggregation, which /api/v1/scores refuses (#185, #270): "+
			"a repo-scoped anonymized report cannot be served and therefore cannot be reproduced", scope, mode.String())
	}

	// 🔴 IS THIS EVEN THE RIGHT DATABASE? Nothing else asks. os.Stat proves a file
	// exists; every attribution query below asks "what arrived AFTER id N", and
	// against an unrelated (or empty) tier database the honest answer to all seven
	// is "nothing" — so a manifest with no results block earns rc 0 "REPRODUCED:
	// every pinned input is unchanged" over a database that never held the rows
	// it describes. A staging copy, a fresh install, a wrong --db: all green.
	//
	// The floor is cheap and nearly exact: ids only ever grow, so a table whose
	// GLOBAL MAX(id) is BELOW a watermark taken from it cannot be the table that
	// watermark came from. rc 2, never rc 1 — this is "I am looking at the wrong
	// thing", not "the thing changed".
	if err := assertSnapshotHoldsManifest(ctx, ro, m); err != nil {
		return verifyResult{}, err
	}

	// Recompute the report through the REAL serving path.
	nowScores, err := rerunScores(ctx, db, window, scope, mode, k)
	if err != nil {
		return verifyResult{}, err
	}

	// --- output comparison -------------------------------------------------
	if m.Results != nil && len(m.Results.Scores) > 0 {
		res.ResultsPinned = true
		wantCanon, err := canonicalJSON(m.Results.Scores)
		if err != nil {
			return verifyResult{}, fmt.Errorf("manifest results.scores is not valid JSON: %w", err)
		}
		gotCanon, err := canonicalJSON(nowScores)
		if err != nil {
			return verifyResult{}, fmt.Errorf("recomputed report is not valid JSON: %w", err)
		}
		res.ResultsIdentical = bytes.Equal(wantCanon, gotCanon)
		if !res.ResultsIdentical {
			res.Moves = diffScoreRows(m.Results.Scores, nowScores)
		}
	}

	// --- input dimensions --------------------------------------------------
	res.Dims = append(res.Dims, verifyDimPriceTable(m.PriceTable, info))
	res.Dims = append(res.Dims, verifyDimRubric(m.Rubric))

	// 🔴 THE BAND IS DECIDED BEFORE ANY PIN TAKEN OVER IT IS COMPARED (#1033).
	// When the manifest's token_since disagrees with this binary's band (or cannot
	// be parsed), its band-dependent pins cover a different row set: comparing them
	// reports CHANGED — and exits 1 — on a database nobody touched.
	bandDim, bandDisagrees := verifyDimAttributionBand(m, window)

	wmDims, err := verifyDimsWatermarks(ctx, ro, m, window, scope, bandDisagrees)
	if err != nil {
		return verifyResult{}, err
	}
	res.Dims = append(res.Dims, wmDims...)

	digestDims, err := verifyDimsDigests(ctx, db, m, window, scope, bandDisagrees)
	if err != nil {
		return verifyResult{}, err
	}
	res.Dims = append(res.Dims, digestDims...)

	// #751: the repo-blind exclusion a scoped report published. It reads rows
	// OUTSIDE the scope every line above is bound to, which is the entire reason
	// it is a separate dimension — see verifyDimRepoScopeExcluded.
	exDim, ok, err := verifyDimRepoScopeExcluded(ctx, db, m, window, scope, bandDisagrees)
	if err != nil {
		return verifyResult{}, err
	}
	if ok {
		res.Dims = append(res.Dims, exDim)
	}

	if bandDisagrees {
		res.Dims = append(res.Dims, bandDim)
	}
	if d, ok := verifyDimUnknownPins(m.unknownFields); ok {
		res.Dims = append(res.Dims, d)
	}
	return res, nil
}

// rerunScores re-runs the report by serving GET /api/v1/scores IN PROCESS,
// through the real internal/api handler.
//
// 🔴 THIS IS THE LOAD-BEARING DESIGN DECISION AND IT IS NOT NEGOTIABLE. The
// report is assembled by (*api.Handler).loadWindow — unexported, coupled to
// *Handler, and the single place that does the store reads, alias
// canonicalization, cost/outcome join, zero-token tripwire, k-anon fold and
// fixed-seed bootstrap CI. Reimplementing any of that in cmd/tierd would mean
// this command verifies a REPLICA of the report rather than the report, and the
// first time the two drifted the tool would report a divergence that does not
// exist — or, far worse, miss one that does. A reproducibility checker that
// checks a proxy is not a reproducibility checker.
//
// The handler is mounted with RegisterReadOnly, so every write/ingest/admin
// route is STRUCTURALLY ABSENT from the mux: a verification physically cannot
// mutate anything, independent of any token check. No listener is opened and no
// port is bound — the request never leaves the process.
func rerunScores(ctx context.Context, db *store.DB, w manifestWindow, scope string, mode scoring.AggregationMode, k int) (json.RawMessage, error) {
	// Discard the handler's logs: api.New warns when no API token is set, which
	// is correct for a server and pure noise for a local verifier reading its
	// own database.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// WithUnsealedRecompute: a live-window manifest is replayed over its window
	// in every mode, team and division included (#913).
	h := api.New(db, logger, "", nil, version, api.RateLimitConfig{}, api.WithCommit(commit), api.WithUnsealedRecompute())
	h.SetAggregation(mode, k)

	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)

	q := url.Values{}
	q.Set("since", w.SinceQuery)
	if w.UntilQuery != "" {
		q.Set("until", w.UntilQuery)
	}
	if scope != "" {
		q.Set("repo", scope)
	}
	// WithContext, not NewRequest: loadWindow honours r.Context(), and the
	// recompute is the EXPENSIVE half of this command (a full-window scan). A
	// SIGINT that only cancelled the five cheap attribution reads would leave the
	// operator waiting on the one thing they wanted to stop.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/scores?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build scores request: %w", err)
	}
	// ServeMux and the auth limiter read RemoteAddr; an in-process request has
	// none. Loopback is the truthful value for a request that never left here.
	req.RemoteAddr = "127.0.0.1:0"

	rec := &captureWriter{header: http.Header{}}
	mux.ServeHTTP(rec, req)
	// status 0 means the handler returned without writing anything at all, which
	// "HTTP 0" describes to nobody. Name it.
	if rec.status == 0 {
		return nil, errors.New("re-running the report produced no response at all (the handler wrote nothing)")
	}
	if rec.status != http.StatusOK {
		return nil, fmt.Errorf("re-running the report returned HTTP %d: %s",
			rec.status, strings.TrimSpace(rec.body.String()))
	}
	return json.RawMessage(rec.body.Bytes()), nil
}

// captureWriter is a minimal in-memory http.ResponseWriter.
//
// Written by hand rather than pulling in net/http/httptest: httptest is a
// testing package and this is production code in the release binary, whose
// dependency graph this repo keeps deliberately narrow (see the tools/docgen
// nested module for the same discipline applied to goldmark).
type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// readOnlySnapshotDSN uses a file: URI because modernc ignores mode=ro on
// bare paths. URL escaping preserves '?', '#' and '%' in the snapshot path.
func readOnlySnapshotDSN(path string) string {
	return store.ReadOnlyURI(path, "mode=ro&_pragma=busy_timeout(5000)")
}

// canonicalJSON normalizes a JSON document so two encodings of the same value
// compare equal: whitespace and indentation are dropped and object keys are
// emitted in sorted order (encoding/json sorts map[string]any keys). Without it
// a re-indented manifest would read as a divergence.
//
// ⚠️ TWO THINGS IT DOES NOT DISTINGUISH, stated because the whole point of this
// function is that the comparison is exact. The any round-trip collapses
// DUPLICATE object keys (last wins) and renders every number as a float64, so
// two integers above 2^53 that differ can compare equal. Neither is reachable
// from /api/v1/scores — it emits Go structs, and its largest integer is a sample
// count — but a future field carrying raw micro-dollars (2^53 µUSD ≈ $9e9) would
// make the second real, and this is the comment that has to be confronted then.
func canonicalJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// diffScoreRows names the rows whose score moved, appeared or vanished. It is
// the HEADLINE, not the verdict: whether anything moved at all is decided by the
// canonical full-body comparison, which sees fields this partial decode ignores
// (cost, coverage, CI bounds, the sidecars). A change with no row-level move
// therefore still fails — it just prints no "moved" line, and the LIMITS block
// says where else to look.
func diffScoreRows(was, now []byte) []scoreMove {
	var a, b scoresEnvelope
	// A decode failure here means the manifest's results block is valid JSON of
	// the WRONG SHAPE. Returning no moves is correct (there is nothing to name),
	// and the caller still reports the divergence from the canonical comparison —
	// the headline says the change is in a field this summary does not rank,
	// which is true. Deliberately not an error: a malformed results block must not
	// downgrade a real divergence to "could not check".
	if err := json.Unmarshal(was, &a); err != nil {
		return nil
	}
	if err := json.Unmarshal(now, &b); err != nil {
		return nil
	}
	oldBy := map[string]float64{}
	for _, r := range a.rows() {
		oldBy[r.label()] = r.TIER
	}
	newBy := map[string]float64{}
	for _, r := range b.rows() {
		newBy[r.label()] = r.TIER
	}

	var moves []scoreMove
	for label, was := range oldBy {
		now, ok := newBy[label]
		switch {
		case !ok:
			moves = append(moves, scoreMove{Label: label, Was: was, Kind: "vanished"})
		case was != now:
			moves = append(moves, scoreMove{Label: label, Was: was, Now: now, Kind: "moved"})
		}
	}
	for label, now := range newBy {
		if _, ok := oldBy[label]; !ok {
			moves = append(moves, scoreMove{Label: label, Now: now, Kind: "appeared"})
		}
	}
	// Deterministic order. Map iteration above is unordered, and a diagnostic
	// whose line order changes run to run is one an operator cannot diff — the
	// same class of defect #722 fixed inside the scoring path itself.
	sort.Slice(moves, func(i, j int) bool { return moves[i].Label < moves[j].Label })
	return moves
}

// ---------------------------------------------------------------------------
// Attribution dimensions
// ---------------------------------------------------------------------------

func verifyDimPriceTable(want manifestPriceTable, got store.PriceTableInfo) verifyDim {
	d := verifyDim{Name: "price_table"}
	if want.TableHash == "" {
		d.Status = dimNotPinned
		d.Detail = "manifest carries no table_hash — this report's prices cannot be identified (#713)"
		return d
	}
	if want.TableHash == got.TableHash {
		d.Status = dimUnchanged
		d.Detail = fmt.Sprintf("%s, version %d, effective %s", shortHash(got.TableHash), got.Version, logsafe.Str(got.EffectiveDate))
		return d
	}
	d.Status = dimChanged
	// 🔴 logsafe.Str ON THE MANIFEST'S HASH, AND IT IS NOT BELT-AND-BRACES.
	// It wraps the SHORTENED value, i.e. the sanitizer is the OUTERMOST operation
	// and therefore the last thing between this value and the report — which is
	// the correct ordering for a sink, and also the only one that renders a
	// balanced quoted string (sanitizing first lets shortHash truncate the
	// closing quote away).
	// want.TableHash is whatever the manifest FILE said, and shortHash only
	// truncates when the value splits on ':' with a tail longer than 8 bytes —
	// anything else it returns VERBATIM and unbounded. Measured through this exact
	// format string: a table_hash of "x\n  quality revisions   UNCHANGED   none
	// since quality_history id 4" renders as TWO lines, the second of which the
	// suite's own dimLine helper parses as a genuine attribution. A manifest could
	// therefore make the verifier appear to assert the WRONG attribution — the one
	// thing the status column exists to make unforgeable.
	d.Detail = fmt.Sprintf("%s (version %d) -> %s (version %d, effective %s)",
		logsafe.Str(shortHash(want.TableHash)), want.Version,
		shortHash(got.TableHash), got.Version, logsafe.Str(got.EffectiveDate))
	// file_hash moves on a comment or key-order edit while table_hash does not,
	// so an equal file_hash under an unequal table_hash would mean the
	// CANONICALIZATION changed, not the prices — a different and much more
	// alarming fact than a price edit. Say which one it is.
	if want.FileHash != "" && want.FileHash == got.FileHash {
		d.Detail += " — ⚠ file_hash is IDENTICAL, so the SOURCE BYTES did not change; the resolved table did. Suspect a canonicalization or scheme change, not a price edit"
	}
	// 🔑 WHAT THIS DOES AND DOES NOT MOVE, said on the line rather than left for
	// the operator to deduce. cost_micro is immutable per row (#233) and the
	// report READS it as stored — it is not recomputed — so a different active
	// table changes the report's provenance STAMP and, through it, the published
	// bytes. It does NOT by itself move a single figure. The only sanctioned
	// mutator of a stored cost is `tierd reprice`, which writes the ledger on its
	// own line, so that line is where "did the money actually move" is answered.
	d.Detail += ". Stored cost_micro is immutable (#233), so this moves the re-run's price STAMP (including when selected by --prices), not its figures — see the reprice line for whether any cost actually changed"
	return d
}

func verifyDimRubric(want manifestRubric) verifyDim {
	d := verifyDim{Name: "rubric"}
	if want.Version == 0 {
		d.Status = dimNotPinned
		d.Detail = "manifest carries no rubric.version"
		return d
	}
	if want.Version == scoring.RubricVersion {
		d.Status = dimUnchanged
		d.Detail = fmt.Sprintf("version %d", scoring.RubricVersion)
		return d
	}
	d.Status = dimChanged
	d.Detail = fmt.Sprintf("version %d -> %d — the weight rubric itself moved, so every weighted point is on a different scale",
		want.Version, scoring.RubricVersion)
	return d
}

// verifyDimsDigests RECOMPUTES #716's content identity over the manifest's
// window and compares it (#740).
//
// 🔴 THIS IS THE STRONGEST CHECK THIS COMMAND HAS, AND FOR ONE COMMIT IT WAS
// HARD-CODED TO "CANNOT". store.Watermarks says in terms that a MAX(id)/COUNT(*)
// pair "CANNOT see an in-place UPDATE"; a digest over the row CONTENTS can. Until
// #740 the manifest emitted no digest and this function returned dimNotCheckable
// unconditionally, so the pair produced `events_digest: NOT PINNED` on an
// otherwise green run that exited 0 REPRODUCED — the absence of the strongest
// check being indistinguishable from that check passing, which is this repo's
// most frequently-recurring bug class.
//
// FOUR outcomes, and each one is a different fact:
//
//	NOT PINNED    the manifest carries no digest (an older emitter, or the
//	              declared anonymized-mode omission — the reason is echoed).
//	NOT CHECKED   pinned under a scheme this build cannot compute. ⛔ It must stay
//	              REACHABLE: it is the rollback seam for a future tierdig2, and the
//	              alternative to refusing is comparing two values this build cannot
//	              know are commensurable.
//	UNCHANGED     recomputed and equal.
//	CHANGED       recomputed and different — the row CONTENTS moved.
//
// 🔑 THE RECOMPUTATION IS SCOPED (#747). `scope` is the manifest's repo, already
// canonicalized by verifyAgainst — the ONE canonicalization, shared with the
// re-run and the attribution, so all three read the same rows. A fleet-wide
// recomputation compared against a scoped pin was the reason this used to answer
// NOT CHECKED here; that branch is gone because the comparison is now honest.
// ⛔ Do not "simplify" by dropping the scope: a fleet-wide recomputation would
// differ from a scoped pin on every install with more than one repository, and
// verify-report would report CHANGED on data nobody touched.
func verifyDimsDigests(ctx context.Context, db *store.DB, m reportManifest, w manifestWindow, scope string, bandDisagrees bool) ([]verifyDim, error) {
	pins := []struct {
		name string
		want *manifestDigest
	}{
		{"events_digest", m.EventsDigest},
		{"outcomes_digest", m.OutcomesDigest},
	}

	// Recompute ONCE, and only when something is actually comparable — a full
	// scan of both tables is the most expensive read in this command and there is
	// no reason to pay it for a manifest that pins nothing.
	var (
		events, outcomes store.Digest
		computed         bool
	)
	for _, p := range pins {
		if digestIsCheckable(p.want) {
			var err error
			// ReportDigests, not EventsDigest + OutcomesDigest: the single-table
			// methods run on two pooled connections and would pair two different
			// instants. It also owns the token/outcome window asymmetry, which is
			// what makes this recomputation cover the same rows the emitter did.
			events, outcomes, err = db.ReportDigests(ctx, w.Since, w.Until, store.RepoScope(scope))
			if err != nil {
				return nil, fmt.Errorf("recompute window digests: %w", err)
			}
			computed = true
			break
		}
	}

	got := map[string]store.Digest{"events_digest": events, "outcomes_digest": outcomes}
	dims := make([]verifyDim, 0, len(pins))
	for _, p := range pins {
		// The events digest covers the token side's band; outcomes_digest does not.
		if p.name == "events_digest" && bandDisagrees && digestIsCheckable(p.want) {
			dims = append(dims, bandIncommensurable(p.name))
			continue
		}
		dims = append(dims, verifyDimDigest(p.name, p.want, got[p.name], computed, m.DigestsOmitted))
	}
	return dims, nil
}

// verifyDimRepoScopeExcluded RECOMPUTES the repo-blind exclusion the manifest
// pinned and compares it (#751).
//
// 🔴 IT IS THE ONLY LINE THAT LOOKS AT ROWS OUTSIDE THE REPORT'S OWN SCOPE, AND
// THAT IS EXACTLY WHY IT EXISTS. Every other data dimension above reads through
// the report's predicate — the watermarks and the digests alike carry the
// manifest's `repo`, which is STRICT (`repo = ?`), so the reserved `unqualified`
// sentinel rows are covered by none of them. A scoped /scores nevertheless reads
// those rows, to publish data_quality.repo_scope_excluded. Measured before this
// dimension existed: an in-place reprice of the sentinel rows moved that figure
// from $9 to $14 while every line above held still and this command printed
// REPRODUCED over changed served bytes.
//
// FOUR outcomes, and a fifth state that prints NOTHING:
//
//	(no line)     the manifest is fleet-wide and pins nothing. A fleet-wide report
//	              EXCLUDED nothing — the sentinel rows are inside its own digests —
//	              so there is no such published figure, and a NOT PINNED line would
//	              invite an operator to hunt for a pin that should not exist.
//	NOT PINNED    a SCOPED manifest carrying no pin: a pre-#751 emitter, or a
//	              hand-written file. Reported, never counted as agreement.
//	NOT CHECKED   a FLEET-WIDE manifest that pins it anyway. A "known" field is
//	              invisible to unknownManifestFields by construction, so without
//	              this branch such a pin would be decoded and silently ignored —
//	              pinnedButUnexamined's failure mode, one dimension over.
//	UNCHANGED     recomputed and equal on all three quantities.
//	CHANGED       recomputed and different — and the detail NAMES which of the
//	              three moved, because "the exclusion changed" is not an
//	              attribution.
//
// 🔑 THE RECOMPUTATION TAKES NO SCOPE, MATCHING THE EMITTER. store.
// UnqualifiedExclusionWindow deliberately takes the window only: the sentinel
// rows are the same set whichever repository was named. It also owns the
// AttributableWindow widening on the token side, so passing the report's window
// here reproduces the emitter's band rather than a neighbour of it. ⛔ Do not
// hand-roll the query with a `repo = ?` conjunct: that would select the scoped
// rows, which are the ones every other line already covers, and this dimension
// would silently become a duplicate of the digest.
//
// ⚠️ IT IS DELIBERATELY NOT IN quorumDims. It reads rows that are NOT the
// report's population, so a manifest pinning only this has had the fleet's
// repo-blind rows examined and its own report's rows not at all — "REPRODUCED:
// every pinned input is unchanged" over that would be the fail-open shape this
// command exists to prevent.
func verifyDimRepoScopeExcluded(ctx context.Context, db *store.DB, m reportManifest, w manifestWindow, scope string, bandDisagrees bool) (verifyDim, bool, error) {
	d := verifyDim{Name: "repo_scope_excluded"}
	want := m.RepoScopeExcluded
	if scope == "" {
		if want == nil {
			return verifyDim{}, false, nil
		}
		d.Status = dimNotCheckable
		d.Detail = fmt.Sprintf("this manifest pins a repo-blind exclusion (%d token_events, %d micro-dollars, "+
			"%d outcomes) but declares NO repo scope — a fleet-wide report excludes nothing and publishes no such "+
			"figure, so there is no served number this pin corresponds to and it was NOT examined",
			want.TokenEvents, want.CostMicro, want.Outcomes)
		return d, true, nil
	}
	if want == nil {
		d.Status = dimNotPinned
		d.Detail = "manifest carries no repo_scope_excluded (#751), so a change to the repo-blind rows this " +
			"SCOPED report disclosed in data_quality.repo_scope_excluded would be invisible to this run — no " +
			"watermark and no digest above covers them, because the scope is strict (`repo = ?`)"
		return d, true, nil
	}

	got, err := db.UnqualifiedExclusionWindow(ctx, w.Since, w.Until)
	if err != nil {
		return verifyDim{}, false, fmt.Errorf("recompute repo-blind exclusion: %w", err)
	}
	// #1033: the token leg (count and cost) is read over the band; the outcome leg
	// is read over [since, until) and stays comparable. So a moved outcome count is
	// still a divergence, and an unmoved one is NOT CHECKED, never UNCHANGED.
	if bandDisagrees {
		if want.Outcomes != got.OutcomeRecords {
			d.Status = dimChanged
			d.Detail = fmt.Sprintf("outcomes %d -> %d: the repo-blind outcomes this scoped report disclosed have "+
				"moved. The token leg was NOT compared: it was pinned over a different attribution band",
				want.Outcomes, got.OutcomeRecords)
			return d, true, nil
		}
		d.Status, d.bandWithheld = dimNotCheckable, true
		d.Detail = fmt.Sprintf("the outcome leg is unmoved (%d repo-blind outcome(s)), but the token leg (count and "+
			"cost) was pinned over a different attribution band and was NOT compared", got.OutcomeRecords)
		return d, true, nil
	}
	if want.TokenEvents == got.TokenEvents && want.CostMicro == got.CostMicro && want.Outcomes == got.OutcomeRecords {
		d.Status = dimUnchanged
		d.Detail = fmt.Sprintf("%d repo-blind token_events ($%.2f) and %d repo-blind outcome(s) — the disclosure "+
			"this scoped report published is unmoved",
			got.TokenEvents, store.MicroToDollars(got.CostMicro), got.OutcomeRecords)
		return d, true, nil
	}
	// 🔴 NAME THE MOVED QUANTITY, NOT MERELY THE DIMENSION. The three move for
	// different reasons — a reprice moves cost alone and no id or count with it,
	// which is the case no line above can see; a late repo-blind ingest moves a
	// count; a repo repair moves counts DOWN by qualifying rows that were blind.
	// A single "it changed" would collapse all three.
	var moved []string
	if want.TokenEvents != got.TokenEvents {
		moved = append(moved, fmt.Sprintf("token_events %d -> %d", want.TokenEvents, got.TokenEvents))
	}
	if want.CostMicro != got.CostMicro {
		moved = append(moved, fmt.Sprintf("cost $%.2f -> $%.2f",
			store.MicroToDollars(want.CostMicro), store.MicroToDollars(got.CostMicro)))
	}
	if want.Outcomes != got.OutcomeRecords {
		moved = append(moved, fmt.Sprintf("outcomes %d -> %d", want.Outcomes, got.OutcomeRecords))
	}
	d.Status = dimChanged
	d.Detail = strings.Join(moved, ", ") + ": the repo-blind rows this scoped report disclosed have moved. " +
		"They sit OUTSIDE the strict `repo = ?` predicate, so no watermark and no digest above can see them — " +
		"and /scores publishes them in data_quality.repo_scope_excluded, so the served body differs"
	if want.TokenEvents == got.TokenEvents && want.Outcomes == got.OutcomeRecords {
		d.Detail += ". Both counts are unchanged, so this is most likely an in-place reprice of a repo-blind row, " +
			"or compensating changes"
	}
	// ⚠️ SAME CAVEAT AS THE DIGEST LINE, AND FOR THE SAME REASON. This recomputes
	// through `db`, the MIGRATED snapshot — it must, because the cost_micro
	// conversion migration rewrites the very column this sums, so a pre-migration
	// read would compare two different representations of the same money. (The
	// watermark lines use the unmigrated `ro` handle because ids and counts are
	// migration-invariant; this figure is not.) The operator's file is untouched —
	// the migration runs on the VACUUM INTO copy — but a binary upgrade is a
	// genuine, non-tamper explanation for this line.
	d.Detail += ". ⚠ Check the tool_version line first: an Open()-time migration on a newer binary rewrites " +
		"cost_micro in place and would move this figure with no edit having been made"
	return d, true, nil
}

// bandIncommensurable is the line for a pin taken over the attribution band when
// the manifest's band disagrees with this binary's: NOT CHECKED, never compared.
func bandIncommensurable(name string) verifyDim {
	return verifyDim{
		Name:   name,
		Status: dimNotCheckable,
		Detail: "pinned over a different attribution band than this binary derives, so it is not commensurable " +
			"with a recomputation and was NOT compared — see the attribution band line",
		bandWithheld: true,
	}
}

// verifyDimAttributionBand compares the manifest's published `token_since`
// against the band THIS binary derives, and reports a line ONLY when they
// disagree. ok is false when they agree or when the manifest pins nothing, so an
// ordinary run prints nothing.
//
// 🔴 THE PIN WAS DECODED AND NEVER COMPARED, WHICH IS THE DEFECT THIS WHOLE
// BRANCH EXISTS TO REMOVE, ONE LEVEL DOWN. `token_since` is the emitter's
// statement of the lower bound it watermarked and digested token_events over.
// THREE dimensions here RE-DERIVE that bound as `since -
// store.AttributableWindow`: token_events, events_digest and — since #751 —
// repo_scope_excluded, whose token side is widened by the same constant inside
// store.UnqualifiedExclusionWindow. If the emitting binary used a different
// AttributableWindow (or the file was hand-edited), the recomputation covers a
// DIFFERENT ROW SET than the emitter did, and comparing them reports
// `token_events CHANGED` / `events_digest CHANGED` / `repo_scope_excluded
// CHANGED`: a confident misattribution, in the one tool whose entire purpose is
// correct attribution. So verifyAgainst calls this FIRST and, on ok, those three
// lines are NOT CHECKED instead of compared (#1033 — appending this line after
// the comparisons had already run left them free to report CHANGED and exit 1).
//
// ⚠️ THE COUNT WAS "TWO" UNTIL #751 ADDED THE THIRD, AND NOBODY RE-READ IT — the
// exact shape quorumDims warns about one screen up. ⛔ Do not restate a NUMBER in
// the printed detail below; name the pins, so the next addition cannot silently
// falsify a count.
//
// ⚠️ IT IS NOT TRUSTED, ONLY COMPARED, AND THE DISTINCTION IS THE POINT. Binding
// the manifest's own value would let a manifest choose the band it is audited
// against — a narrower one hides exactly the late row the band exists to catch.
// So the derived bound stays authoritative and a disagreement is a COULD-NOT-
// CHECK on the affected dimensions, never a silent adoption and never a
// divergence: we have no reading either way.
func verifyDimAttributionBand(m reportManifest, w manifestWindow) (verifyDim, bool) {
	if m.TokenSince == "" {
		return verifyDim{}, false
	}
	// RFC3339 is the only form the emitter writes (internal/api/manifest.go). Not
	// parseManifestBound: its midnight rule is about re-running /scores, and a
	// non-midnight band is a DIFFERENT band, not an unreadable one.
	want, err := time.Parse(time.RFC3339, m.TokenSince)
	if err != nil {
		return verifyDim{
			Name:   "attribution band",
			Status: dimNotCheckable,
			Detail: fmt.Sprintf("manifest pins token_since %s, which this binary cannot parse (%v) — the band its "+
				"token_events watermark, its row count, its events_digest and its repo_scope_excluded token counts "+
				"were taken over is unknown, so none of them was compared",
				logsafe.Str(m.TokenSince), logsafe.Err(err)),
		}, true
	}
	got := w.Since.Add(-store.AttributableWindow)
	if want.Equal(got) {
		return verifyDim{}, false
	}
	return verifyDim{
		Name:   "attribution band",
		Status: dimNotCheckable,
		Detail: fmt.Sprintf("manifest pins token_since %s but this binary derives %s from since (%s - %s) — the "+
			"emitting build used a DIFFERENT attribution band, so its token_events watermark, its row count, its "+
			"events_digest and its repo_scope_excluded token counts cover a different row set than this binary "+
			"would recompute. Every one of those pins is NOT commensurable with this manifest and was NOT compared",
			logsafe.Str(m.TokenSince), got.UTC().Format(time.RFC3339),
			w.Since.UTC().Format(time.RFC3339), store.AttributableWindow),
	}, true
}

// digestIsCheckable is the SINGLE definition of "this pin can be recomputed and
// compared by this build".
//
// 🔑 IT EXISTS SO THERE IS ONE COPY OF THAT PREDICATE, AND THE DUPLICATE IT
// REPLACES WOULD HAVE FAILED SILENTLY. verifyDimsDigests asks it to decide
// whether to pay for a full two-table scan; verifyDimDigest asks it to decide
// what to REPORT. Two hand-written copies drifting apart produce exactly one
// shape — "pinned, never compared, reported as fine" — which is #740's own defect
// re-created inside its fix. The order of the two conditions is not significant
// here, but it IS in verifyDimDigest, which reports a DIFFERENT reason for each;
// keep them in step.
//
// ⚠️ IT TOOK A `scope` ARGUMENT UNTIL #747 and refused any scoped manifest. The
// digest now honours a repo predicate, so a scoped pin IS checkable — against a
// recomputation carrying the same scope, which verifyDimsDigests passes.
func digestIsCheckable(want *manifestDigest) bool {
	return want != nil && want.Value != "" &&
		strings.HasPrefix(want.Value, digestScheme+":")
}

func verifyDimDigest(name string, want *manifestDigest, got store.Digest, computed bool, omitted string) verifyDim {
	d := verifyDim{Name: name}
	if want == nil || want.Value == "" {
		d.Status = dimNotPinned
		d.Detail = fmt.Sprintf("manifest carries no %s (#716), so an in-place edit to a row's CONTENTS — which moves no id and no count — would be invisible to this run", name)
		if omitted != "" {
			// The emitter's own stated reason. "Absent and declared" and "absent
			// and silent" are different facts, and an operator must be able to
			// tell a k-anonymity withhold from an older binary.
			d.Detail += ". Emitter says: " + logsafe.Str(omitted)
		}
		return d
	}
	if !strings.HasPrefix(want.Value, digestScheme+":") {
		d.Status = dimNotCheckable
		d.Detail = fmt.Sprintf("manifest pins %s, which this build cannot compute — it knows the %s scheme only, and "+
			"comparing values from two schemes is not a comparison",
			logsafe.Str(shortHash(want.Value)), digestScheme)
		return d
	}
	if !computed {
		// Unreachable while the loop above shares this predicate; kept because the
		// alternative to a loud refusal here is a silent UNCHANGED, and dimUnknown
		// renders as the deliberately wrong-looking UNKNOWN rather than agreement.
		d.Status = dimUnknown
		d.Detail = "the digest was pinned but never recomputed — this is a bug in verify-report, not a finding about the data"
		return d
	}
	if want.Value == got.Value {
		// 🔴 THE DENOMINATOR IS CHECKED, NOT JUST REPRINTED. `rows` is published
		// beside `value` precisely because a digest over an empty window is a
		// well-formed value and a useless claim — and under fixed-arity framing an
		// equal value IMPLIES an equal row count, so a disagreement here means the
		// manifest contradicts itself (hand-edited, or an emitter that computed its
		// count over a different population than its digest). Reporting UNCHANGED on
		// a self-contradictory manifest would be agreement earned by ignoring half
		// the pin, which is the shape this whole branch removes.
		if want.Rows != got.Rows {
			d.Status = dimNotCheckable
			d.Detail = fmt.Sprintf("%s matches, but the manifest pins %d row(s) where the recomputation covers %d — "+
				"an equal digest cannot come from a different row count, so this manifest CONTRADICTS ITSELF and "+
				"neither half of it can be relied on",
				shortHash(got.Value), want.Rows, got.Rows)
			return d
		}
		d.Status = dimUnchanged
		d.Detail = fmt.Sprintf("%s over %d row(s) — every digested column of every row is unchanged as the digest reads it; "+
			"columns outside the digest were not compared", shortHash(got.Value), got.Rows)
		return d
	}
	d.Status = dimChanged
	d.Detail = fmt.Sprintf("%s (%d rows) -> %s (%d rows): the CONTENTS of the rows this report was computed over have changed",
		logsafe.Str(shortHash(want.Value)), want.Rows, shortHash(got.Value), got.Rows)
	if want.Rows == got.Rows {
		// 🔑 THE READING THAT IS EASY TO GET BACKWARDS. Equal counts with an
		// unequal digest MOST LIKELY means an in-place EDIT — the case every
		// watermark on this report is structurally blind to, and the reason this
		// dimension exists at all — but compensating changes (an insert matched by
		// a deletion, or a row moved into and another out of the window) read the
		// same, so the detail must not state the edit as fact.
		d.Detail += ". The row COUNT is unchanged, so this is most likely an in-place edit, or compensating changes — " +
			"see the watermark lines"
	}
	// ⚠️ The snapshot this recomputation reads was opened through store.Open,
	// which MIGRATES it (the price_version backfill and the cost_micro conversion
	// both rewrite token_events). The operator's file is untouched — the migration
	// runs on the VACUUM INTO copy — but a binary upgrade since the manifest was
	// written is a genuine, non-tamper explanation for this line, and it is the
	// first thing to rule out.
	d.Detail += ". ⚠ Check the tool_version line first: an Open()-time migration on a newer binary rewrites rows in " +
		"place and would move this digest with no edit having been made"
	return d
}

// assertSnapshotHoldsManifest refuses a database that cannot be the one the
// manifest was computed over.
//
// ⚠️ THE ONE FALSE POSITIVE, NAMED. Deleting the literally-newest row of a table
// does lower its global MAX(id), so an erasure that removed the most recent
// token event would trip this and report rc 2 rather than the deletion. That is
// the safe direction (a refusal, not a fabricated pass), the error says so, and
// the alternative — dropping the check — reinstates a silent green over the
// wrong database.
func assertSnapshotHoldsManifest(ctx context.Context, ro *sql.DB, m reportManifest) error {
	if m.Watermarks == nil {
		return nil
	}
	win, led := m.window(), m.ledgers()
	for _, f := range []struct {
		table string
		pin   *int64
	}{
		{"token_events", win.MaxTokenEventID},
		{"outcomes", win.MaxOutcomeID},
		{"quality_history", led.MaxQualityHistoryID},
		// The ROW ledgers, matching store.LedgerWatermarks. The aggregate
		// `reprice_audit` / `repo_repair_audit` tables carry their own,
		// independent AUTOINCREMENT sequences, so checking a row-ledger pin
		// against an aggregate table's MAX(id) would compare two unrelated
		// numbers — and, because the row ledgers are always the larger of the
		// two, would refuse legitimate databases.
		{"reprice_row_audit", led.MaxRepriceRowAuditID},
		{"cost_correction_audit", led.MaxCostCorrectionAuditID},
		{"repo_repair_row_audit", led.MaxRepoRepairRowAuditID},
		{"push_outcome_audit", led.MaxPushOutcomeAuditID},
	} {
		if f.pin == nil || *f.pin == 0 {
			continue
		}
		var maxID int64
		// Table name is an in-file constant; nothing is interpolated from input.
		if err := ro.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM "+f.table).Scan(&maxID); err != nil {
			return fmt.Errorf("read %s high-water mark: %w", f.table, err)
		}
		if maxID < *f.pin {
			return fmt.Errorf("this database cannot be the one this manifest describes: %s holds ids up to %d, "+
				"but the manifest was taken at %d. Check --db points at the right database "+
				"(or, far less likely, that the newest %s row was deleted)",
				f.table, maxID, *f.pin, f.table)
		}
	}
	return nil
}

// verifyDimsWatermarks builds the seven post-watermark dimensions.
//
// ⚠️ An ABSENT watermarks block is NOT special-cased with its own name list. It
// is the zero manifestWatermarks — every field a nil *int64 — so each dimension
// below reports NOT PINNED through its OWN code path, with its own name and its
// own message. A separate "no watermarks" branch would need a second, hand-typed
// copy of the seven dimension names, and two sources for one list is how the two
// drift: a dimension renamed here would keep its old name on the nil path, and
// nothing would go red.
func verifyDimsWatermarks(ctx context.Context, ro *sql.DB, m reportManifest, w manifestWindow, scope string, bandDisagrees bool) ([]verifyDim, error) {
	win, led := m.window(), m.ledgers()
	since, until := w.Since, w.Until

	dims := make([]verifyDim, 0, 7)

	// 🔴 THE TOKEN SIDE IS READ OVER THE ATTRIBUTION BAND, NOT THE REPORT WINDOW,
	// AND THIS FILE USED TO GET IT WRONG (#741). store.WindowWatermarks is
	// explicit: token_events is watermarked over [since - AttributableWindow,
	// until) because OutcomeTokenTotals funds an outcome from token events up to
	// 14 days before `since`. Bounded at `since`, a late-ingested row inside that
	// band can lift a (developer, issue) total past scoring.MinAttributableTokens,
	// clear the #136 tripwire, un-suppress a developer and change /scores — while
	// this dimension reports UNCHANGED and the run lands in UNATTRIBUTED. Worse,
	// the pinned count is taken over the band, so subtracting a survivor count
	// computed over the narrower window would report phantom deletions.
	// The outcome side is NOT widened: an outcome before `since` is not in this
	// report, and counting it would be a false alarm.
	// A watermark-less pin still takes verifyDimLateRows' NOT PINNED path, which
	// discloses a stranded row count; only a comparison is withheld here.
	var d verifyDim
	var err error
	if bandDisagrees && win.MaxTokenEventID != nil {
		d = bandIncommensurable("token_events")
	} else {
		d, err = verifyDimLateRows(ctx, ro, lateRowsQuery{
			name: "token_events", table: "token_events", watermark: win.MaxTokenEventID,
			rowCount: win.TokenEventCount,
			since:    since.Add(-store.AttributableWindow), until: until, scope: scope, costColumn: "cost_micro",
		})
		if err != nil {
			return nil, err
		}
	}
	dims = append(dims, d)

	d, err = verifyDimLateRows(ctx, ro, lateRowsQuery{
		name: "outcomes", table: "outcomes", watermark: win.MaxOutcomeID,
		rowCount: win.OutcomeCount,
		since:    since, until: until, scope: scope,
	})
	if err != nil {
		return nil, err
	}
	dims = append(dims, d)

	d, err = verifyDimQualityRevisions(ctx, ro, led.MaxQualityHistoryID, led.QualityHistoryCount, since, until, scope)
	if err != nil {
		return nil, err
	}
	dims = append(dims, d)

	// 🔴 THE THREE AUDIT LEDGERS ARE SCOPED TO THIS REPORT'S OWN ROWS, and the
	// first draft was not. It counted ledger ENTRIES globally, so ANY `tierd
	// reprice`, `repair-repo` or cost correction anywhere in the install — a
	// different repository, a different year — flipped the dimension to CHANGED
	// and made every manifest emitted before it diverge FOREVER. In an install
	// that uses reprice at all, the tool would have been permanently red for a
	// change that never touched the window, and it would have printed a
	// fleet-wide dollar figure beside a window that moved nothing.
	//
	// (#849 added a fourth, push_outcome_audit, scoped by the push row's own ts
	// and repo carried in each audit row — see its scopedLedgers entry.)
	//
	// The join is possible because all three ledgers carry a per-ROW before-image
	// keyed by token_event_id (reprice_row_audit, repo_repair_row_audit, and
	// cost_correction_audit itself), so a ledger entry can be asked whether it
	// touched a row THIS window scores. The unscoped count is still reported, as
	// context rather than as a divergence.
	//
	// ⚠️ THESE THREE JOIN ON THE REPORT WINDOW, NOT THE ATTRIBUTION BAND, AND THAT
	// IS DELIBERATE — it is the one place in this file where the two differ. The
	// token_events dimension above is widened to the band because its PIN is taken
	// over the band (store.WindowWatermarks), so matching populations is arithmetic
	// necessity. Here the pin is on the ledger table and `te.ts` is this file's own
	// "did it touch the report" filter, which should match the read the operation
	// perturbs: the cost totals a reprice or a correction moves are read over
	// [since, until). Widening it would report operations that cannot move this
	// report — the "permanently red" direction the fleet-wide first draft had.
	// ⇒ A ledger operation on a BAND row is not silently lost: it rewrites
	// cost_micro or repo, both of which are inside the events digest, and that
	// digest covers the band. The digest names it; this line does not claim to.
	for _, cfg := range scopedLedgers(led) {
		d, err = verifyDimScopedLedger(ctx, ro, cfg, since, until, scope)
		if err != nil {
			return nil, err
		}
		dims = append(dims, d)
	}

	return dims, nil
}

// scopedLedgerQuery describes one audit ledger's per-row before-image join.
//
// Every SQL fragment here is an in-file constant; the watermark, window bounds
// and scope are the only bound values. The %s slots take the window/scope
// conjuncts, appended in the same text order their binds are.
//
// ⛔ rowsSQL GOES THROUGH fmt.Sprintf, SO IT MUST CONTAIN NO OTHER `%`. A LIKE
// pattern ('%x%'), a modulo, or a stray percent renders as `%!x(MISSING)` INSIDE
// the statement — a malformed query at RUNTIME, on the scoped path only, with
// nothing at compile time to catch it. Escape any real one as `%%`, or move the
// conjuncts to a token replacement.
type scopedLedgerQuery struct {
	name      string // the report label
	table     string // the AGGREGATE table the manifest watermarks
	rowsSQL   string // scoped: rows, distinct events, distinct operations, net micro
	globalSQL string // unscoped operation count above the watermark, for context
	watermark *int64
	// rowCount is the manifest's pinned COUNT(*) for `table`, or nil. The removal
	// read below is taken over `table` too, so the pin and the read can never be
	// pointed at different tables. (An earlier draft carried a separate
	// `ledgerTable` field for that read; it held the same string in all three
	// entries, so it was a second name for one fact rather than a guard.)
	rowCount *int64
	// scopeBinds is how many times the scope value is bound in rowsSQL. Repair
	// needs it twice — see the comment on its query.
	scopeBinds int
	// moneyLabel names what the net figure MEANS for this ledger; they differ.
	// Empty for a ledger that moves no money (push reconciliations): no net
	// figure is printed.
	moneyLabel string
	// netMayDoubleCount marks a ledger whose per-row before-images can stack
	// (two reprices of one row), making the summed net an over-count.
	netMayDoubleCount bool
}

// scopedLedgers builds the four ledger queries for one manifest's watermarks.
func scopedLedgers(w manifestLedgerWatermarks) []scopedLedgerQuery {
	return []scopedLedgerQuery{
		{
			name: "reprice", table: "reprice_row_audit", watermark: w.MaxRepriceRowAuditID,
			rowCount: w.RepriceRowAuditCount,
			// te.cost_micro is the row's CURRENT cost and r.old_cost_micro its
			// pre-reprice image, so the difference is the drift since the
			// manifest — which is the figure an operator wants, not the delta of
			// any single run.
			//
			// 🔴 BOUNDED ON THE ROW LEDGER'S OWN id, NOT ON THE AGGREGATE
			// reprice_audit's (#741). #715 watermarks reprice_row_audit, and the
			// two tables have INDEPENDENT AUTOINCREMENT sequences — a pin from one
			// compared against the other is two unrelated numbers wearing the same
			// name. It is also the more direct question: "which per-row
			// before-images were written after this point" is exactly what a
			// replay needs, where the aggregate id only answers it by proxy.
			rowsSQL: `SELECT COUNT(*), COUNT(DISTINCT r.token_event_id), COUNT(DISTINCT r.reprice_id),
			       COALESCE(SUM(te.cost_micro - r.old_cost_micro), 0)
			FROM reprice_row_audit r
			JOIN token_events te ON te.id = r.token_event_id
			WHERE r.id > ? AND te.ts >= ?%s%s`,
			globalSQL:         `SELECT COUNT(DISTINCT reprice_id) FROM reprice_row_audit WHERE id > ?`,
			scopeBinds:        1,
			moneyLabel:        "cost drift vs the pre-reprice images",
			netMayDoubleCount: true,
		},
		{
			name: "cost corrections", table: "cost_correction_audit", watermark: w.MaxCostCorrectionAuditID,
			rowCount: w.CostCorrectionAuditCount,
			// This ledger IS its own row ledger — no group id — and successive
			// corrections telescope, so the summed net is exact.
			rowsSQL: `SELECT COUNT(*), COUNT(DISTINCT c.token_event_id), COUNT(DISTINCT c.id),
			       COALESCE(SUM(c.new_cost_micro - c.old_cost_micro), 0)
			FROM cost_correction_audit c
			JOIN token_events te ON te.id = c.token_event_id
			WHERE c.id > ? AND te.ts >= ?%s%s`,
			globalSQL:  `SELECT COUNT(*) FROM cost_correction_audit WHERE id > ?`,
			scopeBinds: 1,
			moneyLabel: "net correction",
		},
		{
			name: "repo repairs", table: "repo_repair_row_audit", watermark: w.MaxRepoRepairRowAuditID,
			rowCount: w.RepoRepairRowAuditCount,
			// 🔴 THE SCOPE PREDICATE IS A DISJUNCTION HERE, AND IT HAS TO BE.
			// A repair MUTATES THE JOIN KEY: token_events.repo is what a scoped
			// window filters on, and the repair is what changed it. Matching only
			// the CURRENT repo would see rows repaired INTO scope and miss every
			// row repaired OUT of it — and a row that left the scope changed this
			// report just as much as one that entered. old_repo is the
			// before-image, so the disjunction catches both directions.
			rowsSQL: `SELECT COUNT(*), COUNT(DISTINCT r.token_event_id), COUNT(DISTINCT r.repair_id),
			       COALESCE(SUM(te.cost_micro), 0)
			FROM repo_repair_row_audit r
			JOIN token_events te ON te.id = r.token_event_id
			WHERE r.id > ? AND te.ts >= ?%s%s`,
			globalSQL:  `SELECT COUNT(DISTINCT repair_id) FROM repo_repair_row_audit WHERE id > ?`,
			scopeBinds: 2,
			moneyLabel: "spend on the moved rows",
			// 🔴 STACKS THE SAME WAY REPRICE DOES, AND THE FIRST DRAFT SAID IT DID
			// NOT. repo_repair_row_audit is UNIQUE(repair_id, token_event_id), so ONE
			// run touches a row once — but two runs can (repaired A->B, then B->C),
			// and then SUM(te.cost_micro) counts that row's spend twice while
			// COUNT(DISTINCT token_event_id) counts it once. Printing that figure as
			// fact is "a number that is wrong in a direction nobody can see", which
			// is the exact sentence the reprice entry above uses to justify its own
			// guard. The rows != events condition suppresses it precisely when it
			// would be wrong and prints it otherwise.
			netMayDoubleCount: true,
		},
		{
			// #849. A push outcome superseded by a merged PR (deleted) or re-owned
			// in place (developer/ts rewritten). The audit row carries the push
			// row's own ts and repo in each image, so it is scoped to this report
			// WITHOUT a join — a superseded row no longer exists to join to. The
			// derived table renames outcome_ts to ts so the shared window/scope
			// conjuncts (te.ts, te.repo) apply unchanged. No money moves.
			name: "push reconciliations", table: "push_outcome_audit", watermark: w.MaxPushOutcomeAuditID,
			rowCount: w.PushOutcomeAuditCount,
			rowsSQL: `SELECT COUNT(*), COUNT(DISTINCT te.outcome_id), COUNT(DISTINCT te.commit_sha), 0
			FROM (SELECT id, outcome_id, commit_sha, outcome_ts AS ts, repo FROM push_outcome_audit) te
			WHERE te.id > ? AND te.ts >= ?%s%s`,
			globalSQL:  `SELECT COUNT(DISTINCT commit_sha) FROM push_outcome_audit WHERE id > ?`,
			scopeBinds: 1,
		},
	}
}

// verifyDimScopedLedger reports the audited operations that touched rows THIS
// report scores, since the manifest's watermark.
func verifyDimScopedLedger(ctx context.Context, ro *sql.DB, q scopedLedgerQuery, since, until time.Time, scope string) (verifyDim, error) {
	d := verifyDim{Name: q.name}
	if q.watermark == nil {
		d.Status = dimNotPinned
		d.Detail = fmt.Sprintf("manifest pins no %s watermark", q.table)
		// 🔴 SAY SO WHEN A SIBLING PIN IS BEING DROPPED. The count and the MAX(id)
		// are published together, and the count answers a question the watermark
		// cannot (a REMOVAL). Returning here on a nil watermark would leave a pinned
		// count decoded, unexamined and — because it is a KNOWN field — invisible to
		// the unknown-pins line too: a pin under a green tick, which is this
		// branch's whole subject. ledgerRowsRemoved states the mirror rule ("an
		// absent pin is NOT PINNED, never 'nothing was removed'"); this is the case
		// it does not cover.
		d.Detail += pinnedButUnexamined("row count", q.rowCount)
		if q.rowCount != nil {
			d.Status = dimNotCheckable
		}
		return d, nil
	}
	wm := *q.watermark

	untilClause := ""
	args := []any{wm, since.UTC()}
	if !until.IsZero() {
		untilClause = " AND te.ts < ?"
		args = append(args, until.UTC())
	}
	scopeClause := ""
	if scope != "" {
		if q.scopeBinds == 2 {
			scopeClause = " AND (te.repo = ? OR r.old_repo = ?)"
			args = append(args, scope, scope)
		} else {
			scopeClause = " AND te.repo = ?"
			args = append(args, scope)
		}
	}

	var rows, events, ops, netMicro int64
	if err := ro.QueryRowContext(ctx, fmt.Sprintf(q.rowsSQL, untilClause, scopeClause), args...).
		Scan(&rows, &events, &ops, &netMicro); err != nil {
		return verifyDim{}, fmt.Errorf("read post-watermark %s: %w", q.table, err)
	}
	var globalOps int64
	if err := ro.QueryRowContext(ctx, q.globalSQL, wm).Scan(&globalOps); err != nil {
		return verifyDim{}, fmt.Errorf("count %s entries: %w", q.table, err)
	}

	// The elsewhere count is CONTEXT, never a divergence: an operation that touched
	// no row this report scores did not change this report. Computed HERE, above
	// every branch that prints it, so no branch can be written that forgets it —
	// the ledger-shrank arm below was exactly that omission.
	elsewhere := ""
	if globalOps > ops {
		elsewhere = fmt.Sprintf(" (%d more operation(s) elsewhere, not this window)", globalOps-ops)
	}

	// 🔴 THE LEDGER-SHRANK ARM. The manifest pins each ledger's COUNT(*) as well
	// as its MAX(id), and only the count can see a REMOVAL: MAX(id) is invariant
	// under deleting any non-maximal row. These tables are append-only by intent,
	// but EraseDeveloper (GDPR Art. 17) hard-deletes from them, so this is a
	// reachable and legitimate event — and it is still a change to the very
	// evidence this report's provenance rests on, so it is reported rather than
	// swallowed. Without it the four pinned counts would be decoded and never
	// examined, which is a pin under a green tick.
	removedLedger, err := ledgerRowsRemoved(ctx, ro, q.table, wm, q.rowCount)
	if err != nil {
		return verifyDim{}, err
	}
	if removedLedger > 0 {
		d.Status = dimChanged
		d.Detail = fmt.Sprintf("%d of the %d pinned %s row(s) are GONE — an append-only ledger has LOST rows. "+
			"An erasure (GDPR Art. 17) hard-deletes audit rows, so this is not necessarily tampering, but the "+
			"provenance evidence for this report is no longer what the manifest attests",
			removedLedger, *q.rowCount, q.table)
		if rows > 0 {
			d.Detail += fmt.Sprintf("; and %d operation(s) touched %d row(s) in this window since id %d", ops, events, wm)
		}
		// The elsewhere count is carried onto this path too. Every other branch
		// prints it, and an operator reading "N rows are GONE" needs the same
		// context — dropping it here would make the removal look like the only thing
		// that happened in the ledger.
		d.Detail += elsewhere
		return d, nil
	}

	if rows == 0 {
		d.Status = dimUnchanged
		d.Detail = fmt.Sprintf("none touching this window since %s id %d%s", q.table, wm, elsewhere)
		if q.rowCount == nil {
			d.Detail += " ⚠ no row count pinned, so a DELETION would be invisible"
		}
		return d, nil
	}
	d.Status = dimChanged
	d.Detail = fmt.Sprintf("%d operation(s) touched %d row(s) in this window since %s id %d",
		ops, events, q.table, wm)
	if q.netMayDoubleCount && rows != events {
		// Stacked before-images: summing them would over-count. Say so rather
		// than print a number that is wrong in a direction nobody can see.
		d.Detail += fmt.Sprintf("; %d before-images across %d rows, so the net is NOT summable here — read it from the results comparison", rows, events)
	} else if q.moneyLabel != "" {
		d.Detail += fmt.Sprintf("  (%+.2f USD %s)", store.MicroToDollars(netMicro), q.moneyLabel)
	}
	d.Detail += elsewhere
	return d, nil
}

// pinnedButUnexamined returns the disclosure clause for a sibling pin this run
// declined to look at, or "" when there is nothing to disclose.
//
// 🔑 IT EXISTS BECAUSE "NOT PINNED" AND "PINNED, AND I SKIPPED IT" ARE DIFFERENT
// FACTS, and the second has no other channel. A known-but-unread field is
// invisible to unknownManifestFields by construction — it IS known — so without
// this sentence the only trace of a dropped pin would be its absence from a line
// nobody knew to look for.
func pinnedButUnexamined(what string, pin *int64) string {
	if pin == nil {
		return ""
	}
	return fmt.Sprintf("; ⚠ a %s IS pinned (%d) and was NOT examined, because the comparison it feeds needs the "+
		"watermark this manifest omitted", what, *pin)
}

// ledgerRowsRemoved reports how many of a manifest's pinned ledger rows are no
// longer present. Zero when the manifest pinned no count for that ledger — an
// absent pin is NOT PINNED, never "nothing was removed".
//
// 🔴 THE PINNED POPULATION IS EXACTLY THE ROWS WITH id <= watermark, SO ONLY
// THOSE ARE COUNTED (#1032). store.ReportWatermarks reads each ledger's COUNT(*)
// and COALESCE(MAX(id), 0) unwindowed and unscoped, in one statement on one
// snapshot, so every counted row has id <= the pinned MAX(id); and every ledger
// is INTEGER PRIMARY KEY AUTOINCREMENT, so no later AUTO-ASSIGNED id can fall at
// or below it. An explicit-id INSERT or a lowered sqlite_sequence can; that is a
// direct-DB edit, like an in-place UPDATE, which no MAX/COUNT pair can see
// (docs/reproducibility.md §1). Counting the whole table instead let any newer
// row stand in for a deleted pinned one: delete a non-maximal pinned row, add one
// row elsewhere, and the count, MAX(id) and the scoped query all read unchanged.
//
// 🔒 `table` is always one of the in-file constants in scopedLedgers or
// verifyDimsWatermarks; nothing here is interpolated from the manifest, which is
// operator-supplied. Keep it that way — this function concatenates its SQL.
func ledgerRowsRemoved(ctx context.Context, ro *sql.DB, table string, watermark int64, pinned *int64) (int64, error) {
	if pinned == nil {
		return 0, nil
	}
	var survivors int64
	if err := ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE id <= ?", watermark).Scan(&survivors); err != nil {
		return 0, fmt.Errorf("count %s rows at or below id %d: %w", table, watermark, err)
	}
	return pinnedRowsRemoved(*pinned, survivors), nil
}

// pinnedRowsRemoved is the deletion arithmetic every removal check shares: the
// rows a manifest pinned, minus the rows still present AT OR BELOW its watermark.
// It reports only a NET shortfall: survivors at or above the pin read as zero, so
// a removal offset by rows that entered the counted set in place (a ts or repo
// rewrite into the window) is not seen here. Rows above the watermark are the
// post-watermark reads' business, never a negative removal.
func pinnedRowsRemoved(pinned, survivors int64) int64 {
	if survivors >= pinned {
		return 0
	}
	return pinned - survivors
}

type lateRowsQuery struct {
	name, table  string
	watermark    *int64
	rowCount     *int64 // the manifest's pinned in-window row count, or nil
	since, until time.Time
	scope        string
	costColumn   string // empty when the table carries no cost
}

// verifyDimLateRows counts (and prices) the in-window rows that arrived above a
// watermark.
func verifyDimLateRows(ctx context.Context, ro *sql.DB, q lateRowsQuery) (verifyDim, error) {
	d := verifyDim{Name: q.name}
	if q.watermark == nil {
		d.Status = dimNotPinned
		d.Detail = fmt.Sprintf("manifest pins no %s watermark — late rows are invisible to this run", q.table)
		// See verifyDimScopedLedger: a pinned row count whose watermark is absent
		// must be DISCLOSED as unexamined, not silently dropped.
		d.Detail += pinnedButUnexamined("row count", q.rowCount)
		if q.rowCount != nil {
			d.Status = dimNotCheckable
		}
		return d, nil
	}
	wm := *q.watermark

	cost := "0"
	if q.costColumn != "" {
		cost = "COALESCE(SUM(" + q.costColumn + "), 0)"
	}
	// The table name and cost column are package constants chosen above, never
	// operator input; every VALUE is bound. The window and scope bounds are
	// parameterised in the same text order they appear.
	sqlText := "SELECT COUNT(*), " + cost + " FROM " + q.table + " WHERE id > ? AND ts >= ?"
	currentSQL := "SELECT COUNT(*) FROM " + q.table + " WHERE ts >= ?"
	args := []any{wm, q.since.UTC()}
	if !q.until.IsZero() {
		sqlText += " AND ts < ?"
		currentSQL += " AND ts < ?"
		args = append(args, q.until.UTC())
	}
	if q.scope != "" {
		sqlText += " AND repo = ?"
		currentSQL += " AND repo = ?"
		args = append(args, q.scope)
	}

	var count, costMicro int64
	if err := ro.QueryRowContext(ctx, sqlText, args...).Scan(&count, &costMicro); err != nil {
		return verifyDim{}, fmt.Errorf("read post-watermark %s: %w", q.table, err)
	}

	// 🔴 THE DELETION ARM. Rows that existed when the manifest was written, minus
	// the rows still present that PREDATE the watermark, is exactly how many were
	// removed — and removal is invisible to every other signal this command has:
	// MAX(id) does not move when a non-maximal row is deleted, and no audit
	// ledger records an erasure. Without this, a GDPR Art. 17 EraseDeveloper over
	// the window returns rc 0 REPRODUCED against a database that has lost rows.
	var removed, survivors int64
	if q.rowCount != nil {
		var current int64
		if err := ro.QueryRowContext(ctx, currentSQL, args[1:]...).Scan(&current); err != nil {
			return verifyDim{}, fmt.Errorf("count %s in window: %w", q.table, err)
		}
		survivors = current - count
		removed = pinnedRowsRemoved(*q.rowCount, survivors)
	}

	switch {
	case count == 0 && removed == 0:
		d.Status = dimUnchanged
		if q.rowCount == nil {
			d.Detail = fmt.Sprintf("no rows above watermark %d in this window ⚠ no row count pinned, so a DELETION would be invisible", wm)
		} else {
			d.Detail = fmt.Sprintf("no rows above watermark %d, and the counted population still holds %d rows at or below it, %d pinned (count only)",
				wm, survivors, *q.rowCount)
		}
		return d, nil
	case count == 0:
		d.Status = dimChanged
		d.Detail = fmt.Sprintf("%d of the %d pinned rows were REMOVED or moved out of the counted window or repo scope (count only; no new rows since watermark %d) — a deletion writes to no audit ledger and does not move MAX(id)",
			removed, *q.rowCount, wm)
		return d, nil
	}

	d.Status = dimChanged
	d.Detail = fmt.Sprintf("+%d rows arrived after watermark %d", count, wm)
	if q.costColumn != "" {
		d.Detail += fmt.Sprintf("  (%+.2f USD)", store.MicroToDollars(costMicro))
	}
	if removed > 0 {
		d.Detail += fmt.Sprintf("; and %d of the %d pinned rows were REMOVED or moved out of the counted window or repo scope (count only)", removed, *q.rowCount)
	}
	return d, nil
}

// verifyDimQualityRevisions is the dimension a lazy implementation omits, and
// the one #715 exists to make possible.
//
// 🔴 outcomes.quality is MUTATED IN PLACE by UpdateQualityForOutcome. A revision
// therefore moves a published TIER with NO NEW ROW in token_events and NO NEW
// ROW in outcomes — both of the obvious watermarks stay put and the report
// changes anyway. quality_history is the append-only transition log that makes
// it visible, and reading it is the only reason this command can tell a quality
// revision apart from an unexplained anomaly.
//
// It reports the outcome id, the old -> new values and the reason string,
// because "a quality value changed" is not actionable and "outcome 8821 went
// 1.00 -> 0.75 because of a revert" is.
func verifyDimQualityRevisions(ctx context.Context, ro *sql.DB, watermark, rowCount *int64, since, until time.Time, scope string) (verifyDim, error) {
	d := verifyDim{Name: "quality revisions"}
	if watermark == nil {
		d.Status = dimNotPinned
		d.Detail = "manifest pins no quality_history watermark — an in-place quality revision would be INVISIBLE, and it moves TIER with no new row anywhere else (#715)"
		d.Detail += pinnedButUnexamined("row count", rowCount)
		if rowCount != nil {
			d.Status = dimNotCheckable
		}
		return d, nil
	}
	wm := *watermark

	// Joined to outcomes so only revisions that touch an outcome INSIDE this
	// report's window are attributed to it. quality_history.ts is the moment of
	// the revision, not of the work, so filtering on it would answer the wrong
	// question.
	sqlText := `SELECT q.id, q.outcome_id, q.old_quality, q.new_quality, q.reason
		FROM quality_history q
		JOIN outcomes o ON o.id = q.outcome_id
		WHERE q.id > ? AND o.ts >= ?`
	args := []any{wm, since.UTC()}
	if !until.IsZero() {
		sqlText += " AND o.ts < ?"
		args = append(args, until.UTC())
	}
	if scope != "" {
		sqlText += " AND o.repo = ?"
		args = append(args, scope)
	}
	sqlText += " ORDER BY q.id"

	rows, err := ro.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return verifyDim{}, fmt.Errorf("read post-watermark quality_history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		total    int
		detailed []string
	)
	for rows.Next() {
		var (
			id, outcomeID int64
			oldQ, newQ    float64
			reason        string
		)
		if err := rows.Scan(&id, &outcomeID, &oldQ, &newQ, &reason); err != nil {
			return verifyDim{}, fmt.Errorf("scan quality_history: %w", err)
		}
		total++
		if len(detailed) < maxQualityRevisionsListed {
			// reason is the webhook's event_type, and source_ref-adjacent free
			// text on the legacy path — client-reachable, so it goes through
			// logsafe like every other value a report writer interpolates
			// (#321). The outcome id and the two floats are server-generated.
			detailed = append(detailed, fmt.Sprintf("outcome %d, %.2f -> %.2f, reason %s",
				outcomeID, oldQ, newQ, logsafe.Str(reason)))
		}
	}
	if err := rows.Err(); err != nil {
		return verifyDim{}, fmt.Errorf("iterate quality_history: %w", err)
	}

	// The ledger-shrank arm — see ledgerRowsRemoved. quality_history is the
	// append-only transition log the whole quality dimension rests on; if rows have
	// left it, revisions this report cannot see have been erased from the record.
	removed, err := ledgerRowsRemoved(ctx, ro, "quality_history", wm, rowCount)
	if err != nil {
		return verifyDim{}, err
	}
	if removed > 0 {
		d.Status = dimChanged
		d.Detail = fmt.Sprintf("%d of the %d pinned quality_history row(s) are GONE — the append-only revision log "+
			"has LOST rows, so a revision this manifest attests to is no longer in the record (an erasure "+
			"hard-deletes audit rows)", removed, *rowCount)
		if total > 0 {
			d.Detail += fmt.Sprintf("; and %d new revision(s) since id %d", total, wm)
		}
		return d, nil
	}

	if total == 0 {
		d.Status = dimUnchanged
		d.Detail = fmt.Sprintf("none since quality_history id %d", wm)
		if rowCount == nil {
			d.Detail += " ⚠ no row count pinned, so a DELETION would be invisible"
		}
		return d, nil
	}
	d.Status = dimChanged
	d.Detail = fmt.Sprintf("%d (%s)", total, strings.Join(detailed, "; "))
	if total > len(detailed) {
		d.Detail += fmt.Sprintf(" (+%d more)", total-len(detailed))
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// The report writer
// ---------------------------------------------------------------------------

// printVerifyReport writes the operator-facing attribution.
//
// Pure function of its argument so report_forge_test.go's assertNoForgedLine can
// observe exactly the bytes an operator sees. Every client-controlled value
// interpolated here — developer ids, cohort labels, quality reason strings, the
// pinned hashes — is already logsafe-wrapped by its producer, and the row labels
// are wrapped at the sink below. fmt.Fprintf quotes nothing.
func printVerifyReport(w io.Writer, res verifyResult) {
	scope := res.Scope
	if scope == "" {
		scope = "fleet-wide"
	} else {
		scope = logsafe.Str(scope)
	}
	until := res.Until
	if until == "" {
		until = "(open-ended)"
	}
	// 🔑 `k=0` WOULD BE A LIE IN DEVELOPER MODE, and it is the exact confusion the
	// manifest's *int K exists to prevent one level up: "no floor is applied" and
	// "the floor is zero" are different states, and only an anonymized mode has a
	// floor at all. K is set only for an anonymized mode, so 0 here means "none".
	k := "(none)"
	if res.K != 0 {
		k = strconv.Itoa(res.K)
	}
	_, _ = fmt.Fprintf(w, "verify-report: %s  window %s .. %s  scope %s  aggregation %s k=%s\n\n",
		logsafe.Str(res.Schema), logsafe.Str(res.Since), logsafe.Str(until),
		scope, logsafe.Str(res.Aggregation), k)

	// The headline is switched on verdict(), the same function that produces the
	// process exit code — so stdout can never say REPRODUCED on a run that exits
	// non-zero. See the comment on verdict.
	switch res.verdict() {
	case rcCannotCheck:
		if res.dataPinsNotChecked() {
			_, _ = fmt.Fprintf(w, "COULD NOT CHECK: this manifest pins data, but those pins were NOT CHECKED (the\n"+
				"      NOT CHECKED lines below say why), so a change there could not have been seen.\n"+
				"      NOTHING was reproduced and nothing was refuted. See LIMITS.\n")
			break
		}
		_, _ = fmt.Fprintf(w, "COULD NOT CHECK: this manifest pins nothing about the DATA — no results, no\n"+
			"      watermarks and no window digests. A price-table or rubric pin only compares this\n"+
			"      binary to itself. NOTHING was reproduced and nothing was refuted. See LIMITS.\n")
	case rcDiverged:
		printVerifyHeadline(w, res)
	default:
		if res.ResultsPinned {
			_, _ = fmt.Fprintf(w, "REPRODUCED: the recomputed report is identical to the manifest's, and every pinned input is unchanged.\n")
		} else {
			_, _ = fmt.Fprintf(w, "REPRODUCED: every pinned input is unchanged. ⚠ The manifest pinned NO results, so no number was compared — see LIMITS.\n")
		}
	}

	_, _ = fmt.Fprintln(w)
	for _, d := range res.Dims {
		_, _ = fmt.Fprintf(w, "  %-21s %-11s %s\n", d.Name+":", d.Status.label(), d.Detail)
	}
	printVerifyBuildLine(w, res)

	if res.unattributed() {
		_, _ = fmt.Fprintf(w, "\n🔴 UNATTRIBUTED: the numbers moved and NOT ONE pinned input did.\n")
		if res.bandDisagrees() {
			_, _ = fmt.Fprintf(w, "   ⚠ FIRST SUSPECT: the attribution band line above. This manifest's band differs from\n"+
				"     this binary's, so the recomputation reads a different set of token rows and the\n"+
				"     NOT CHECKED pins that could have seen the move were not compared.\n")
		}
		_, _ = fmt.Fprintf(w, "   Check, in this order:\n"+
			"     1. a token event OUTSIDE this window. /scores reports the installation's\n"+
			"        COST HORIZON (#512) — data_quality.cost_coverage_start, derived from the\n"+
			"        earliest captured event in the WHOLE database — so an out-of-window\n"+
			"        insert changes the response body while every window-scoped watermark\n"+
			"        above correctly holds. MEASURED, not theorised.\n"+
			"     2. developer_alias — an alias edit RETROACTIVELY re-joins spend to outcomes\n"+
			"        and moves every affected TIER, writing no row this manifest watches.\n"+
			"     3. org_hierarchy — in team/division mode it drives the whole k-anon fold,\n"+
			"        also by in-place upsert.\n"+
			"     4. the NOT PINNED and NOT CHECKED lines above — a dimension never pinned or not compared.\n"+
			"   ⚠ None of 1-3 is watermarked by tiermanifest1 (all raised on #715).\n")
	}

	printVerifyLimits(w, res)
}

// printVerifyHeadline writes the FAIL line(s): which row moved, from what to
// what. It is the first thing an operator reads, so it names a score and a
// developer rather than a status word.
func printVerifyHeadline(w io.Writer, res verifyResult) {
	if len(res.Moves) == 0 {
		switch {
		case res.ResultsPinned && !res.ResultsIdentical:
			_, _ = fmt.Fprintf(w, "FAIL: the report changed, but no headline score moved — the difference is in a\n"+
				"      field this summary does not rank: the price/rubric provenance stamp, or a\n"+
				"      cost, coverage, CI bound or sidecar. The lines below say which input moved.\n")
		// 🔴 THE REASSURING CASE, AND IT USED TO PRINT THE OPPOSITE. With results
		// pinned AND identical, a CHANGED input still makes this a divergence —
		// and the first draft fell through to the branch below, telling the
		// operator two false things at once: that no results were pinned, and
		// that the effect on the numbers was unknown. Both were knowable and both
		// were good news. This state is ORDINARY, not exotic: an audited reprice
		// or repair recorded after the watermark flips a ledger dimension while
		// this window's bytes are untouched.
		case res.ResultsPinned && res.ResultsIdentical:
			_, _ = fmt.Fprintf(w, "FAIL: a pinned INPUT moved, but the recomputed report is IDENTICAL to the\n"+
				"      manifest's — the change did NOT reach this window's published numbers.\n")
		default:
			_, _ = fmt.Fprintf(w, "FAIL: a pinned INPUT moved. ⚠ The manifest pinned no results, so whether the\n"+
				"      published numbers moved with it is UNKNOWN to this run — see LIMITS.\n")
		}
		return
	}
	shown := 0
	for _, mv := range res.Moves {
		if shown >= maxListedInReport {
			_, _ = fmt.Fprintf(w, "      (+%d more rows)\n", len(res.Moves)-shown)
			break
		}
		label := logsafe.Str(mv.Label)
		switch mv.Kind {
		case "appeared":
			_, _ = fmt.Fprintf(w, "FAIL: %s APPEARED in the report (TIER %s) — the population changed\n", label, formatTIER(mv.Now))
		case "vanished":
			_, _ = fmt.Fprintf(w, "FAIL: %s VANISHED from the report (was TIER %s) — the population changed\n", label, formatTIER(mv.Was))
		default:
			was, now := formatTIERPair(mv.Was, mv.Now)
			_, _ = fmt.Fprintf(w, "FAIL: TIER for %s moved %s -> %s\n", label, was, now)
		}
		shown++
	}
}

// printVerifyBuildLine reports tool_version / commit drift.
//
// It is deliberately the LAST line and deliberately not a divergence on its own
// (see verifyResult.diverged) — but it is never omitted when it moved, because a
// different binary is the first thing to suspect when numbers move for no other
// named reason.
func printVerifyBuildLine(w io.Writer, res verifyResult) {
	nowV, wantV := res.ToolVersionNow, res.ToolVersionManifest
	if nowV == "" {
		nowV = "dev"
	}
	if wantV == "" {
		wantV = "(unstamped)"
	}
	// DRIFTED, not CHANGED: the token has to say "reported, and deliberately not
	// counted as a divergence" — see verifyResult.diverged. Reusing CHANGED here
	// would make a reader (and a guard) expect rc 1.
	if nowV == wantV {
		_, _ = fmt.Fprintf(w, "  %-21s %-11s %s\n", "tool_version:", "UNCHANGED", logsafe.Str(nowV))
	} else {
		suffix := ""
		if res.diverged() {
			suffix = "   <-- also a suspect"
		}
		_, _ = fmt.Fprintf(w, "  %-21s %-11s tierd %s (manifest: %s)%s\n",
			"tool_version:", "DRIFTED", logsafe.Str(nowV), logsafe.Str(wantV), suffix)
	}
	if res.CommitManifest != "" && res.CommitManifest != res.CommitNow {
		now := res.CommitNow
		if now == "" {
			now = "(unstamped)"
		}
		_, _ = fmt.Fprintf(w, "  %-21s %-11s %s (manifest: %s)\n",
			"commit:", "DRIFTED", logsafe.Str(now), logsafe.Str(res.CommitManifest))
	}
}

// printVerifyLimits states, on EVERY run including the successful ones, what
// this command did not do.
//
// A limitation printed only on failure is a limitation an operator forgets on
// success, and the failure mode that creates — reading REPRODUCED as "the
// historical population was replayed" — is exactly the overclaim #710 exists to
// stop. See the header comment.
func printVerifyLimits(w io.Writer, res verifyResult) {
	_, _ = fmt.Fprintf(w, "\nLIMITS\n")
	_, _ = fmt.Fprintf(w, "  - This run RECOMPUTED the report over the manifest's window against the database\n"+
		"    AS IT IS NOW. It did NOT replay the historical row population — as-of bounded\n"+
		"    reads (#717) are not in this build — so it ATTRIBUTES a difference to a named\n"+
		"    input; it does not reconstruct the manifest's original numbers.\n")
	_, _ = fmt.Fprintf(w, "  - Every read above ran against a VACUUM INTO SNAPSHOT, now deleted. The database\n"+
		"    you named was never opened for writing and never migrated, so an archived file\n"+
		"    is byte-identical after this run. The snapshot needs free space alongside it.\n")
	_, _ = fmt.Fprintf(w, "  - COMPENSATING CHANGES are the case attribution cannot resolve. A repair that moved\n"+
		"    $X out of this window and a late ingest that brought $X in are reported as two\n"+
		"    separate lines; whether they cancel HERE cannot be answered without replaying\n"+
		"    the window as it stood (#717).\n")
	if !res.ResultsPinned {
		_, _ = fmt.Fprintf(w, "  - This manifest pinned NO results, so NO published number was compared. Only the\n"+
			"    inputs above were checked.\n")
	}
	for _, d := range res.Dims {
		switch d.Status {
		case dimNotPinned:
			_, _ = fmt.Fprintf(w, "  - NOT PINNED: %s — %s\n", d.Name, d.Detail)
		case dimNotCheckable:
			_, _ = fmt.Fprintf(w, "  - NOT CHECKED: %s — %s\n", d.Name, d.Detail)
		case dimUnchanged, dimChanged:
			// Reported in the attribution block above; nothing to disclose.
		}
	}
}

// formatTIER renders a score the way the dashboard does.
func formatTIER(v float64) string { return fmt.Sprintf("%.2f", v) }

// formatTIERPair renders a before/after pair, falling back to full precision
// when two-decimal rounding would render a REAL move as "41.20 -> 41.20". A
// diagnostic that prints two identical numbers next to the word "moved" reads as
// a bug in the tool rather than a finding about the data.
func formatTIERPair(was, now float64) (string, string) {
	a, b := formatTIER(was), formatTIER(now)
	if a == b {
		return fmt.Sprintf("%g", was), fmt.Sprintf("%g", now)
	}
	return a, b
}

// shortHash abbreviates a scheme-tagged hash for a terminal line, keeping the
// SCHEME intact — the tag is the rollback seam and an abbreviation that dropped
// it would make two different canonicalizations look like the same value.
//
// 🔴 IT IS NOT A SANITIZER, AND IT LOOKS LIKE ONE. Truncation is not a barrier:
// this cuts at the FIRST colon and shortens only the TAIL, so everything before
// that colon is returned verbatim and unbounded, and a value with no colon of
// its own is returned whole. A manifest-supplied hash therefore reaches the
// report intact unless the CALLER wrapped it — which is why every call site on
// untrusted input reads logsafe.Str(shortHash(x)) — escape AFTER truncating,
// since the reverse order can cut off the closing quote. Measured: a table_hash of
// forge("deadbeef") rendered a second, standalone `time=...` line through this
// function, and an earlier test payload that happened to carry its own leading
// colon was silently truncated into harmlessness, passing a guard that proved
// nothing.
func shortHash(h string) string {
	tag, hex, ok := strings.Cut(h, ":")
	if !ok || len(hex) <= 8 {
		return h
	}
	return tag + ":" + hex[:8] + "…"
}

// resolveWindow turns the manifest's two wire bounds into the ONE value every
// consumer in this file uses: the instants the attribution queries bind, and the
// query rendering the re-run replays into /api/v1/scores.
//
// A bound this cannot resolve is a COULD NOT CHECK (rc 2), never a divergence.
func resolveWindow(m reportManifest) (manifestWindow, error) {
	since, sinceQuery, err := parseManifestBound(m.Since)
	if err != nil {
		return manifestWindow{}, fmt.Errorf("since: %w", err)
	}
	w := manifestWindow{Since: since, SinceQuery: sinceQuery}
	if m.Until != "" {
		until, untilQuery, err := parseManifestBound(m.Until)
		if err != nil {
			return manifestWindow{}, fmt.Errorf("until: %w", err)
		}
		if !until.After(since) {
			return manifestWindow{}, fmt.Errorf("until (%s) must be after since (%s): the window is half-open [since, until)", m.Until, m.Since)
		}
		w.Until, w.UntilQuery = until, untilQuery
	}
	return w, nil
}

// parseManifestBound resolves one wire bound to an instant AND to the query value
// /api/v1/scores would accept for it.
//
// 🔴 THE SECOND RETURN IS THE WHOLE REASON THIS FUNCTION EXISTS, AND THE LIMIT IT
// ENFORCES IS REAL. A manifest publishes RFC3339 INSTANTS, but /api/v1/scores
// accepts ONLY the date grammar (parseWindowDate: YYYY-MM-DD, YYYY-MM, YYYY). So
// an instant that is not midnight UTC cannot be replayed through the serving path
// AT ALL, and this binary REFUSES rather than truncating: truncation would re-run
// a DIFFERENT window and then compare its numbers against this manifest's, which
// fabricates a verdict in whichever direction the rows happen to fall.
//
// ⚠️ DO NOT RESTORE THE OLD JUSTIFICATION FOR THE INSTANT. This comment used to
// cite internal/api's argument that truncating to a date would give "two windows
// eight hours apart one identity" — which was about THIS SERVER's own default
// bound carrying a time of day, and #746 removed that. reportManifestJSON.Since
// now marks that reasoning expired. The surviving reason is one level out: this
// decoder reads manifests OTHER producers wrote (hand-written, or pre-#746), and
// for those the midnight/mid-day distinction is exactly what separates a report
// it can replay from one it must refuse.
//
// ⚠️ THE CASE THAT USED TO HIT WAS `?since=` OMITTED, AND IT WAS FIXED ON THE
// OTHER SIDE OF THE SEAM (#746). api.parseSince resolved to now-90d carrying the
// current time of day, so a manifest taken with no explicit window was NOT
// reproducible by this build — the DEFAULT shape was the unverifiable one. The
// served default bound is now snapped back to the start of its UTC day, so the
// two surfaces' grammars agree for everything this server emits.
//
// ⛔ THAT IS NOT A REASON TO SOFTEN THIS FUNCTION. It still receives manifests
// this server did not write: hand-written ones, and ones emitted by pre-#746
// builds. For those the refusal below is the only thing standing between the
// operator and a green verdict computed over a window nobody ran, so rc 2 stays
// reachable and is asserted directly by
// TestVerifyReport_DefaultWindowIsReplayable's surviving-refusal arm and by
// TestVerifyReport_NonMidnightWindowIsCouldNotCheck.
func parseManifestBound(s string) (time.Time, string, error) {
	// The legacy date grammar first — it is exact, it round-trips, and it is what
	// a hand-written manifest carries.
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, s, nil
		}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%q is neither an RFC3339 instant nor YYYY-MM-DD / YYYY-MM / YYYY", s)
	}
	t = t.UTC()
	if !t.Equal(t.Truncate(24 * time.Hour)) {
		return time.Time{}, "", fmt.Errorf("%q is not midnight UTC, and /api/v1/scores accepts only whole-day bounds "+
			"(YYYY-MM-DD, YYYY-MM, YYYY) — so this report CANNOT be re-run through the serving path. Truncating the "+
			"bound would recompute a different window and compare its numbers against this manifest's. Re-request the "+
			"manifest with an explicit ?since= (and ?until=) on a date boundary", s)
	}
	return t, t.Format("2006-01-02"), nil
}
