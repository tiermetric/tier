package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// ManifestSchema is the scheme tag stamped on every report manifest (#715).
//
// 🔑 IT IS NOT DECORATION, IT IS THE ROLLBACK SEAM — the same reasoning that put
// "tierpt1:" on the price-table digests (#713). A consumer holding two manifests
// has to be able to tell "these were produced by DIFFERENT manifest schemes"
// apart from "these describe different database states". Without a tag those two
// are the same shape of JSON, and the first legitimate change to what a watermark
// MEANS would silently reinterpret every manifest already published.
//
// Bump it whenever the meaning of an existing field changes — a windowed field
// becoming global, a count changing its population, an id sequence being
// redefined. ADDING a field does not require a bump: an old consumer ignores it
// and every field it does read still means what it meant.
const ManifestSchema = "tiermanifest1"

// reportManifestJSON is the full provenance record for one report (#715).
//
// 🔴 THE PROBLEM IT SOLVES: a report's window predicate is an UNSTABLE IDENTITY.
// Late JSONL ingestion adds rows carrying yesterday's `ts` that legitimately join
// "the same window", so two runs of the same query over the same [since, until)
// can return different numbers and both be correct. A report binds to
// (predicate, AS-OF), never the predicate alone. Everything here is one half or
// the other of that pair: the window/scope/mode block is the PREDICATE, the
// watermark block is the AS-OF, and the price-table/rubric/build block pins the
// CODE AND CONSTANTS that turned one into the other.
//
// ⚠️ WHAT EQUAL MANIFESTS DO AND DO NOT PROVE. Equal manifests mean no input this
// structure watches has moved. They are not a proof of byte-identical output —
// store.Watermarks documents the two uncovered surfaces (in-place
// period_membership edits, and Open()-time migrations, which the tool_version
// stamp rather than a watermark makes visible). Read it as "nothing I can see has
// changed", which is a useful and honest claim, and not as an attestation.
type reportManifestJSON struct {
	// ManifestSchema names the scheme that produced every other field's meaning.
	// FIRST field so it is the first thing a reader — human or parser — sees.
	ManifestSchema string `json:"manifest_schema"`

	// --- the predicate half ---

	// Since/Until are the half-open window [since, until), as RFC3339 UTC
	// INSTANTS, not calendar dates.
	//
	// 🔑 THE INSTANT IS THE HONEST RENDERING OF WHATEVER BOUND WAS USED, and that
	// is why the type is an instant rather than a date.
	//
	// ⚠️ ITS ORIGINAL JUSTIFICATION EXPIRED WITH #746 — do not repeat it. This
	// used to argue that an omitted `?since=` returns `now - 90d` carrying the
	// current TIME OF DAY, so truncating to "2006-01-02" would give two manifests
	// taken at 09:00 and 17:00 one identity while they bounded windows eight
	// hours apart. parseSince now SNAPS that default back to the start of its UTC
	// day, and every explicit bound already parses to midnight, so every value
	// this emitter can publish here is midnight UTC.
	//
	// ⛔ THAT IS NOT A LICENCE TO STAMP A DATE. The field is shipped as an instant
	// (docs/api-compatibility.md rule 2 — a type change is breaking), the
	// consumer side must keep reading instants written by pre-#746 builds and by
	// hand, and `tierd verify-report` distinguishes a midnight bound it can
	// replay from a mid-day one it must refuse — a distinction a date erases at
	// exactly the moment it matters.
	//
	// 🔑 AND NOTE WHERE THE DISCRIMINATION WENT. The old argument was that this
	// field had to separate two manifests taken hours apart on the same day. It
	// no longer does — post-#746 they carry an identical `since` AND an identical
	// (empty) `until`. That is safe because the predicate was never what
	// distinguished them: the watermark and digest blocks are, and they are the
	// half that can see the ingestion between the two requests. A manifest binds
	// to (predicate, AS-OF); #746 made the predicate stable, which is what a
	// predicate should be, and left the AS-OF to do the discriminating.
	//
	// Until is EMPTY when the request left the upper bound open. That is a
	// DIFFERENT report from one bounded at today: an open-ended window re-run
	// tomorrow covers more days, so the emptiness is meaningful and is never
	// filled in with a synthetic "now".
	Since string `json:"since"`
	Until string `json:"until,omitempty"`
	// TokenSince is the resolved lower bound of the token_events ATTRIBUTION BAND
	// (`since - AttributableWindow`), published so a consumer never has to infer
	// it. The scoring path's zero-token tripwire reads token_events from up to 14
	// days before `since`, so `watermarks.window.token_event_count` counts that
	// wider band and is legitimately larger than a naive read of `since` suggests.
	// See store.WindowWatermarks.
	TokenSince string `json:"token_since"`
	// Repo is the repository scope, absent when the read was fleet-wide.
	Repo string `json:"repo,omitempty"`
	// Aggregation is the ANONYMIZED grouping level this server serves at:
	// "developer", "team" (#185) or "division" (#270). Always present — unlike
	// /scores, which omits it in developer mode. A manifest exists to be diffed
	// against another manifest, and a field that is absent in one mode and present
	// in another makes "developer" and "the key was dropped" the same reading.
	Aggregation string `json:"aggregation"`
	// K is the k-anonymity cohort floor, and it is a POINTER so that "no floor is
	// applied" and "the floor is zero" are different states on the wire. It is
	// emitted ONLY in an anonymized mode, because that is the only mode in which
	// any floor is applied; publishing the configured integer in developer mode
	// would assert a protection that is not in force. Same reasoning as
	// versionResponse.Modified.
	K *int `json:"k,omitempty"`

	// --- the code-and-constants half ---

	// PriceTable pins the dollars: version, effective_date and the two #713
	// content digests.
	//
	// ⚠️ price_table.source IS DELIBERATELY ABSENT, and #715 asked for it. It is a
	// local filesystem path, and the ruling of 2026-08-28 on the #713 security
	// review confined it to `tierd score-log` (a local CLI printing to the invoking
	// operator's own terminal) precisely because a SERVED surface has no business
	// disclosing the server's directory layout. This is a served surface. table_hash
	// already answers the question reproducibility actually asks — "are these the
	// same PRICES" — and answers it better than a path, which two installs can share
	// while holding different content. See priceTableJSON.
	//
	// It is built by priceTableStamp, not by hand, so this block and /scores can
	// never stamp different subsets of the same struct.
	PriceTable priceTableJSON `json:"price_table"`
	// Rubric pins the weights, exactly as PriceTable pins the dollars (#239).
	Rubric rubricJSON `json:"rubric"`
	// ToolVersion and Commit pin the BINARY. They are not redundant with the two
	// above: Open()-time migrations rewrite token_events in place with no ledger
	// row at all, so a binary upgrade is a report input that no watermark can see.
	// This is the field that makes it visible. Commit is omitempty because an
	// unstamped build genuinely does not know it, and publishing "" would be a
	// false attestation of a known-empty revision.
	ToolVersion string `json:"tool_version"`
	Commit      string `json:"commit,omitempty"`

	// --- the as-of half ---

	// Watermarks carries store.Watermarks' two sub-structs rather than a remapped
	// field list. That is deliberate: a ledger added to the store type reaches the
	// wire automatically, where a hand-copied mapping would silently drop it —
	// exactly the drift that made priceTableStamp necessary one section up.
	//
	// 🔒 `window` IS A POINTER SO IT CAN BE WITHHELD. See manifestWatermarksJSON.
	Watermarks manifestWatermarksJSON `json:"watermarks"`

	// EventsDigest and OutcomesDigest are #716's content identity over the rows
	// this report was computed over, wired here by #740.
	//
	// 🔴 THEY ARE NOT A REDUNDANT SECOND COPY OF THE WATERMARKS, AND THIS IS THE
	// WHOLE REASON THEY ARE HERE. store.Watermarks says plainly that a
	// MAX(id)/COUNT(*) pair "CANNOT see an in-place UPDATE". A digest over the row
	// CONTENTS can. Until #740 the manifest pinned only the class its own code
	// documents as insufficient while the sufficient one sat unused three files
	// away — and because the verifier's `no digest` branch prints NOT PINNED on an
	// otherwise green run, the absence of the strongest check was indistinguishable
	// from that check passing.
	//
	// 🔑 THE TWO WINDOWS DIFFER AND THAT IS store.ReportDigests' CONTRACT, NOT AN
	// ACCIDENT: events cover [token_since, until) — the attribution band the
	// scoring path actually reads — and outcomes cover [since, until). So
	// `events_digest.rows` is comparable with `watermarks.window.token_event_count`
	// and `outcomes_digest.rows` with `watermarks.window.outcome_count`.
	//
	// 🔑 THAT HOLDS UNDER `?repo=` TOO, SINCE #747: the digests take the SAME
	// store.RepoScope the watermarks do, so both halves count the same population.
	// Before #747 they were unscoped and the handler omitted them for a scoped
	// request rather than publish an identity for a superset of the report's rows.
	// ⚠️ A scoped digest is a DIFFERENT VALUE from the fleet-wide digest over the
	// same rows — the predicate is hashed in (see store.digestDomainFor) — so a
	// consumer must never compare one against the other.
	//
	// Pointers + omitempty because ABSENT and EMPTY are different facts: an empty
	// window still has a digest (a domain-separated sentinel, never sha256("")), so
	// a missing key must mean "not published", and DigestsOmitted below says why.
	EventsDigest   *manifestDigestJSON `json:"events_digest,omitempty"`
	OutcomesDigest *manifestDigestJSON `json:"outcomes_digest,omitempty"`
	// DigestsOmitted names, in one field, WHY the two above are absent.
	//
	// 🔴 A SILENTLY ABSENT DIGEST IS THE DEFECT #740 EXISTS TO CLOSE, ONE LEVEL UP.
	// A consumer that cannot tell "this server does not publish digests" from "this
	// server withheld them for a stated reason" is back to reading an absence as a
	// pass. Emitted whenever, and only when, the two fields above are omitted.
	DigestsOmitted string `json:"digests_omitted,omitempty"`

	// RepoScopeExcluded pins the ONE published figure a scoped manifest's own
	// digests structurally cannot cover (#751).
	//
	// 🔴 WHY IT HAS TO EXIST, AND WHY THE DIGESTS ABOVE ARE NOT ENOUGH. #747 made
	// the digests honour `?repo=` STRICTLY (store.RepoScope.clause is `repo = ?`),
	// so the reserved `unqualified` sentinel rows are excluded — correctly, because
	// a tolerant predicate would over-attribute every repo-blind row in the fleet
	// to whichever repository the caller named (#590). But a scoped /scores STILL
	// READS those rows: buildScopeDisclosure calls store.UnqualifiedExclusionWindow
	// over this very band and publishes data_quality.repo_scope_excluded, the
	// disclosure that stops strict scoping from under-counting silently. That
	// figure therefore sat outside every identity a scoped manifest ships — and a
	// scoped manifest carries no fleet-wide digest either.
	//
	// Measured before this field existed: an in-place reprice of the sentinel rows
	// moved repo_scope_excluded.cost_usd from 9 to 14 while the scoped digests AND
	// the scoped watermarks stayed byte-identical, and `tierd verify-report`
	// printed REPRODUCED over changed served bytes. Same class as the
	// cost_coverage_start gap named in cmd/tierd/verifyreport.go's header.
	//
	// 🔑 IT PINS THE COUNTS, NOT A DIGEST OVER THE SENTINEL ROWS, and that is a
	// ruling rather than the lazy option. Three reasons, in order of weight:
	//
	//   1. THE PIN MUST BE COMMENSURATE WITH THE PUBLISHED FIGURE. What /scores
	//      publishes IS these three quantities. A digest over the sentinel rows
	//      attests a strictly larger claim — every digested column of every repo-blind row —
	//      so it would report CHANGED for an edit that moved no published number
	//      (a developer rename on a repo-blind row, say: the scored figures exclude
	//      those rows entirely and the disclosure is developer-blind). A false alarm
	//      is the one failure mode a tamper-evidence surface may not have.
	//   2. IT IS DISCLOSURE-NEUTRAL. Every value here is already in the /scores body
	//      for the same request. A digest over the sentinel rows would introduce a
	//      NEW identity over a FLEET-WIDE row set on a request that named one
	//      repository — the shape #593 refuses and the reason option (b) on #751
	//      (publishing the fleet-wide digest alongside the scoped one) was rejected.
	//   3. COST, MEASURED RATHER THAN ASSUMED. The digests already make this
	//      endpoint two full-window scans at ~30-36 ms; a digest over the sentinel
	//      rows would be a third and fourth of the same shape. The counts are two
	//      window aggregates — the same two a scoped /scores already runs — at
	//      2.98-4.56 ms on the 20,000-event fixture, +9.9% to +11.9% on the
	//      endpoint — a ratio taken WITHIN one interleaved run, never against the
	//      #740 figures further down, which are a DIFFERENT run.
	//      ⚠️ Not free, and not the "~0.3 ms" a first draft of this comment guessed.
	//      No index can serve the `repo` filter here, so both statements touch the
	//      table for every in-window row — ⛔ and NOT because there is no repo index:
	//      outcomes carries idx_outcomes_push_daily_repo with repo LEADING, which is
	//      unusable only because it is PARTIAL on `source = 'push'`. That is a claim
	//      about the planner, so it is measured rather than argued —
	//      store.TestExclusionReadIsAWindowSeekNotARepoSeek pins both plans and the
	//      partiality they depend on. See store.BenchmarkReportManifestExclusion,
	//      the third member of the pair the block below tells you to re-run.
	//
	// ⚠️ THE RESIDUAL, STATED SO IT IS NOT A SURPRISE: an in-place edit to a
	// sentinel row's NON-cost, NON-timestamp columns moves nothing here. It also
	// moves no figure this report published, which is precisely why the pin stops
	// where it does. cost_micro is a SUM, so the reprice above IS caught; ts moves
	// a row in or out of the window, so that is caught too.
	//
	// 🔑 EMITTED ON EVERY SCOPED MANIFEST, INCLUDING AN ALL-ZERO ONE, WHICH IS A
	// DELIBERATE DIVERGENCE FROM /scores. buildScopeDisclosure omits the block when
	// the window is clean, because there the ABSENCE is the signal to a human
	// reader that the figure is a true total. Here the block is a PIN, and an
	// omit-when-clean pin would be vacuous exactly when it matters most: a window
	// clean at publish time into which a sentinel row is later inserted would show
	// NOT PINNED, and an absent check reading as a passing one is the defect #740
	// exists to close. So on a scoped manifest presence is unconditional, and
	// `repo` present ⟺ this block present is a rule a consumer can check.
	//
	// ⛔ ABSENT on a fleet-wide manifest, and that is not the same omission. A
	// fleet-wide report EXCLUDED nothing — the sentinel rows are inside its own
	// digests — so there is no such published figure to pin, and emitting an
	// install-wide repo-blind cost on a request that asked for no scope would be
	// adding disclosure to close a reproducibility gap.
	//
	// It is unreachable in an anonymized mode by construction: allowRepoScope
	// refuses `?repo=` there (#185, #270), so a scoped manifest is always a
	// developer-aggregation one and no k-anonymity withhold arises here.
	RepoScopeExcluded *manifestRepoScopeExcludedJSON `json:"repo_scope_excluded,omitempty"`

	// KAnonSuppressed declares a withhold, and it is emitted ONLY when one
	// happened. A manifest that goes silently quiet is the failure mode the
	// /scores kanon_suppressed block exists to prevent: absent must never be
	// confusable with zero.
	KAnonSuppressed *manifestKAnonJSON `json:"kanon_suppressed,omitempty"`
}

// manifestWatermarksJSON splits the two halves on the wire because they are read
// over different predicates AND carry different disclosure risk.
//
// 🔴 `window` IS OMITTED ENTIRELY IN AN ANONYMIZED AGGREGATION MODE, AND THAT IS
// A K-ANONYMITY REQUIREMENT, NOT CONSERVATISM. `token_event_count` and
// `outcome_count` are UNFLOORED COUNTS OF WORK over a caller-chosen window.
// #593 — which is CLOSED, and whose reproduction is a live test
// (TestKAnonResidual_TimeAxisReproduction) — established that narrowing `?since=`
// ALONE, with no `?repo=` involved, shrinks a cohort below the floor; the
// /scores strip pass therefore withholds `total`, `cost_composition` and
// `segment_reconciliation` for such a window, and states its own rule: what may
// stay carries "no figure and no count of people or work". These are counts of
// work. Publishing them would hand back, for exactly the windows /scores now
// refuses, the volume of the one contributor /scores just declined to name —
// sweepable day by day into a per-individual activity calendar. Refusing
// `?repo=` does not help: the time axis needs no repo.
//
// ⚠️ THE WITHHOLD IS DELIBERATELY BLUNT — every anonymized-mode request, not
// only sub-k windows. The precise alternative is to compute the REAL
// scoring.KAnonSuppression signal for the window and publish when it is k-safe.
// That is the better endpoint and it is NOT done here, for one reason: the real
// signal costs a full loadWindow, which is the whole cost of /scores, and a
// CHEAPER approximation (say COUNT(DISTINCT developer)) would be a SECOND,
// DIVERGENT floor — it counts un-canonicalized identities, so two aliases of one
// person read as two contributors and a sub-k cohort clears it. A hand-rolled
// floor that errs toward disclosure is worse than no window block. Refine it
// with the real signal or not at all.
//
// The `ledgers` half stays in every mode: it is install-wide and window-
// independent, so it carries no count of the caller's chosen population.
type manifestWatermarksJSON struct {
	Window  *store.WindowWatermarks `json:"window,omitempty"`
	Ledgers store.LedgerWatermarks  `json:"ledgers"`
}

// manifestDigestJSON is one window digest and the row count it covers.
//
// 🔴 rows IS NOT DECORATION AND IT IS NOT DERIVABLE FROM value. store.Digest's
// own doc block calls it "the DENOMINATOR that proves a digest was earned": an
// empty window and a window whose read was cut short both yield a valid-looking
// value, and only rows separates "this window genuinely holds nothing" from
// "this measured nothing". Publishing value without rows would hand a consumer
// a hash it cannot tell apart from a hash of nothing — the same class of defect
// as the missing denominators this repo keeps rediscovering.
type manifestDigestJSON struct {
	// Value is "tierdig1:" + 64 hex characters. The SCHEME TAG is the rollback
	// seam: a verifier holding a digest it cannot compute must be able to say
	// NOT CHECKED rather than guess, exactly as ManifestSchema lets it refuse a
	// tiermanifest2.
	Value string `json:"value"`
	Rows  int64  `json:"rows"`
}

// manifestRepoScopeExcludedJSON is the pinned form of /scores'
// data_quality.repo_scope_excluded (#751). Name-free — counts and money only —
// exactly as repoScopeExcludedJSON is.
//
// 🔴 THE MONEY IS PINNED IN MICRO-DOLLARS WHERE /scores PUBLISHES cost_usd, AND
// THE DIFFERENCE IS DELIBERATE. cost_micro is the stored integer (#69);
// cost_usd is store.MicroToDollars(cost_micro), a float. A pin exists to be
// compared for EQUALITY by a verifier, and float equality over a
// JSON-round-tripped dollar figure can report CHANGED on a representation
// difference — a fabricated divergence in the one tool whose purpose is correct
// attribution. The integer is exact, and the published figure is recoverable
// from it by the same pure function /scores uses; the reverse is not true.
//
// ⚠️ SO THE MEMBER NAMES DIFFER FROM THE /scores BLOCK OF THE SAME KEY NAME
// (`cost_micro` here, `cost_usd` there). The outer key matches because it names
// the same quantity and a consumer diffing "did the disclosure move" looks for
// that name; the member differs because the two surfaces have different jobs.
// ⛔ Do not "align" them by publishing cost_usd here, and do not publish BOTH —
// two representations of one fact drift, and the one that drifts is the one
// nobody re-reads.
type manifestRepoScopeExcludedJSON struct {
	// TokenEvents and CostMicro are the repo-blind token_events the strict scope
	// dropped and their summed cost. ⚠️ Their band is the ATTRIBUTION BAND
	// [since - AttributableWindow, until), NOT [since, until) — see
	// store.UnqualifiedExclusionWindow, which widens the token side deliberately
	// because a scope can suppress a repo-blind row in an outcome's 14-day
	// look-back. A verifier recomputing over the narrower window would compare two
	// populations.
	TokenEvents int64 `json:"token_events"`
	CostMicro   int64 `json:"cost_micro"`
	// Outcomes is the repo-blind outcome records in [since, until) — the plain
	// window, because outcomes are the reporting population rather than a
	// look-back input.
	Outcomes int64 `json:"outcomes"`
}

// manifestKAnonJSON names what was withheld and why, mirroring the shape and the
// intent of /scores' kanon_suppressed block.
type manifestKAnonJSON struct {
	// WithheldWindow is always true when this object is present — the object
	// exists only to declare that withhold — but it is written explicitly so a
	// consumer reads a positive statement rather than inferring from presence.
	WithheldWindow bool `json:"withheld_window"`
	// WithheldDigests is true when the two window digests were withheld for the
	// SAME reason as the window block. Written explicitly rather than left to be
	// inferred from `digests_omitted`, so this record is a complete statement of
	// what k-anonymity withheld from this manifest.
	WithheldDigests bool   `json:"withheld_digests"`
	KAnonymity      int    `json:"k_anonymity"`
	Aggregation     string `json:"aggregation"`
	Reason          string `json:"reason"`
}

// digestOmittedAnonymized is the ONE remaining reason this handler omits the
// digests. It is a constant so the handler and its tests name the same string,
// and so a consumer can tell it apart; match on withheld_digests, not this text.
//
// ⚠️ THE FIELD STAYS EVEN THOUGH ONLY ONE REASON REMAINS (#747 removed the
// scoped one). `digests_omitted` is the difference between "this server does not
// publish digests" and "this server withheld them, here is why", and that
// distinction is what #740 exists to preserve — it is not a list of two.
const digestOmittedAnonymized = "withheld: events_digest.rows and outcomes_digest.rows are unfloored counts of work " +
	"over a caller-chosen window, the same quantity watermarks.window carries (#593), so they are withheld in " +
	"an anonymized aggregation mode. See kanon_suppressed."

// handleGetReportManifest serves GET /api/v1/report_manifest (#715): the
// (predicate, as-of) identity of the report /scores would return for the same
// window and scope.
//
// 🔒 MOUNTED BEHIND requireRead, NOT UNAUTHENTICATED. Two independent reasons,
// either sufficient. It carries the #713 content digests, which the 2026-08-28
// ruling keeps off unauthenticated surfaces because an unkeyed digest of a
// low-entropy operator-written file is a confirmation oracle. And it publishes
// row counts over a caller-chosen window, which is install-shape information.
//
// It answers for the window as it stands NOW. It does not reconstruct a past
// as-of state — replaying a report at an earlier watermark is a separate
// capability that this record is the prerequisite for, not an implementation of.
func (h *Handler) handleGetReportManifest(w http.ResponseWriter, r *http.Request) {
	if h.sealedRead() {
		h.serveSealedManifest(w, r)
		return
	}
	// Strict parameter allowlist FIRST, before any parsing or store work (#590):
	// an unimplemented parameter is answered, not quietly ignored. `before` is the
	// legacy alias parseWindowUpperBound still honors, so it must be listed or the
	// allowlist would reject a parameter this handler accepts. The list is
	// deliberately NARROWER than /scores': ?team= and ?work_type= do not change any
	// watermark or any field below, so accepting them would let a caller believe
	// they had requested a manifest for a filtered report and receive one for the
	// unfiltered window.
	if !rejectUnknownQueryParams(w, r, "since", "until", "before", "repo") {
		return
	}

	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	// Normalize to UTC at the call site, matching /scores (#180): the windowed
	// reads must window by instant, and a manifest that described a different
	// window than the report it stamps would be worse than none.
	since = sinceUTC(since)

	until, ok := h.parseWindowUpperBound(w, r, since)
	if !ok {
		return
	}

	scope, ok := parseRepoScope(w, r)
	if !ok {
		return
	}
	// The SAME refusal /scores applies, via the same method so the two cannot
	// drift. A per-repo row count over a caller-chosen window is exactly the
	// cohort-size quantity a k-anonymity floor protects; see allowRepoScope.
	if !h.allowRepoScope(w, scope) {
		return
	}

	var marks store.Watermarks

	// Via buildIdentity, NOT by reading h.version/h.buildCommit directly — the
	// difference is load-bearing. buildIdentity carries the INJECTED-then-STAMPED
	// commit precedence (#638): the shipped container excludes .git, so it has no
	// VCS stamps and relies on the ldflags value, while a tarball build has stamps
	// and may have no injected value. Reading the field alone would publish an
	// empty commit on exactly the builds where provenance matters most, and would
	// disagree with what /version reports about the same binary.
	build := h.buildIdentity()
	resp := reportManifestJSON{
		ManifestSchema: ManifestSchema,
		// RFC3339 INSTANTS, not dates — see the field comment for why that stays
		// true after #746 snapped the default bound to midnight UTC.
		Since:       since.Format(time.RFC3339),
		TokenSince:  since.Add(-store.AttributableWindow).Format(time.RFC3339),
		Repo:        scope.String(),
		Aggregation: h.aggregation.String(),
		PriceTable:  priceTableStamp(store.ActivePriceTableInfo()),
		Rubric:      rubricJSON{Version: scoring.RubricVersion},
		ToolVersion: build.Version,
		Commit:      build.Commit,
	}
	if !until.IsZero() {
		resp.Until = until.Format(time.RFC3339)
	}
	// 🔴 THE DIGESTS ARE EMITTED HERE AND NOWHERE ELSE, AND THE COST IS THE REASON.
	// This is TWO FULL-WINDOW SCANS on a request path. Measured end to end by
	// BenchmarkReportManifestWatermarks / BenchmarkReportManifestDigests — a PAIR,
	// because the question is what happened to the ENDPOINT, not whether a digest
	// is fast (Apple M5 Max, 20,000 in-window token_events + 2,000 outcomes,
	// 2026-08-29):
	//
	//	the watermark read above   1.42 - 1.49 ms/op   ~4-6 KB     ~85 allocs
	//	ReportDigests, added      28.5  - 34.6 ms/op   12.15 MB   ~725,000 allocs
	//
	// ⇒ ~1.4 ms becomes ~30-36 ms, a ~20x increase, ~1.5 µs per token_events row,
	// scaling LINEARLY with the window. That is not a rounding error; it is now the
	// whole cost of this endpoint. It is affordable HERE because a manifest is
	// fetched once per published report. ⛔ It would NOT be affordable on /scores,
	// which already pays four full-window scans and is polled by the dashboard —
	// do not lift this onto the scoring path, and do not add a third scan here
	// without re-running that pair.
	//
	// ⚠️ THE PAIR IS NOW A TRIO (#751). BenchmarkReportManifestExclusion measures
	// the repo-blind exclusion read added below: 2.98-4.56 ms on the same fixture,
	// interleaved with the two above rather than run after them. That instruction
	// was followed, and it is what caught a first draft claiming ~0.3 ms.
	//
	// ⚠️ THOSE FIGURES ARE THE FLEET-WIDE ONES, and #747's scoped read was NOT
	// re-benchmarked — deliberately, and say so rather than imply a measurement
	// nobody took. A scoped read has the same SHAPE (a window seek on
	// idx_*_ts_id then an ordered walk, pinned by
	// store's TestScopedDigestReadIsStillAWindowSeek) and hashes a SUBSET of the
	// same rows, so the fleet-wide number is its CEILING rather than its value.
	//
	// ONE CALL, not EventsDigest followed by OutcomesDigest — the two single-table
	// methods run on two pooled connections and would pair two different instants,
	// which is a FALSE ALARM for an auditor recomputing both. See the TRAP note on
	// store.WindowDigests.
	//
	// 🔑 `scope` IS PASSED THROUGH, NOT DROPPED (#747), AND IT IS THE SAME VALUE
	// ReportWatermarks is given. That is what makes `events_digest.rows`
	// comparable with `watermarks.window.token_event_count` on a SCOPED manifest
	// as well as a fleet-wide one; handing the digest a different predicate than
	// the watermark would publish two counts of two different populations side by
	// side. It is already canonical — parseRepoScope ran repoid.Canonical at the
	// trust boundary, and a raw spelling bound into `repo = ?` would match zero
	// rows and publish a confident digest over nothing (#718's shape).
	//
	// ⚠️ Until #747 there was a second omission arm here, for a scoped request.
	// It is gone because the digest now honours the scope; the anonymized arm
	// remains, and `digests_omitted` remains with it.
	anonymized := h.aggregation.Anonymized()
	if anonymized {
		resp.DigestsOmitted = digestOmittedAnonymized
	}
	setDigests := func(events, outcomes store.Digest) {
		resp.EventsDigest = &manifestDigestJSON{Value: events.Value, Rows: events.Rows}
		resp.OutcomesDigest = &manifestDigestJSON{Value: outcomes.Value, Rows: outcomes.Rows}
	}

	// 🔴 THE ONE PUBLISHED FIGURE THE DIGESTS ABOVE STRUCTURALLY CANNOT COVER
	// (#751). See reportManifestJSON.RepoScopeExcluded for the full argument; the
	// short form is that a scoped /scores reads the `unqualified` sentinel rows to
	// publish data_quality.repo_scope_excluded, and a strict scoped digest excludes
	// exactly those rows.
	//
	// 🔑 IT TAKES THE SAME (since, until) buildScopeDisclosure DOES, AND NO SCOPE,
	// because store.UnqualifiedExclusionWindow takes none — the sentinel rows are
	// the same set whichever repository was named, and it owns the
	// AttributableWindow widening on the token side. Passing the report's window is
	// what makes this pin the figure the report published rather than a neighbour
	// of it.
	//
	// 🔴 ON A SCOPED MANIFEST THE DIGESTS AND THIS PIN ARE READ IN ONE SNAPSHOT
	// (#1038). `repo = <scope>` and `repo = 'unqualified'` are disjoint only WITHIN
	// one database state: a repo repair (`tierd repair-repo`) moves a token_events
	// row from the sentinel to the scoped repository in ONE UPDATE. Read in two
	// snapshots with that repair committing between them, the row is counted in
	// NEITHER set, the manifest matches no state the database was ever in, and
	// verify-report on an untouched database reports a divergence. Pinned by
	// TestReportManifest_ScopedDigestsAndExclusionShareOneSnapshot.
	//
	// Watermarks also share this snapshot, for scoped and fleet-wide manifests
	// alike (audit S04-2). Counts and digests must describe the same state.
	//
	// Fleet-wide reads no exclusion, mirroring buildScopeDisclosure: nothing was
	// scoped, so nothing was excluded, and there is no honest figure to pin.
	err = h.store.ReadSnapshot(r.Context(), func(snap *store.Snapshot) error {
		var err error
		marks, err = snap.ReportWatermarks(r.Context(), since, until, scope)
		if err != nil {
			return fmt.Errorf("report watermarks: %w", err)
		}
		if !anonymized {
			events, outcomes, err := snap.ReportDigests(r.Context(), since, until, scope)
			if err != nil {
				return fmt.Errorf("report digests: %w", err)
			}
			setDigests(events, outcomes)
		}
		if !scope.IsFleetWide() {
			ex, err := snap.UnqualifiedExclusionWindow(r.Context(), since, until)
			if err != nil {
				return fmt.Errorf("unqualified exclusion: %w", err)
			}
			// Unconditional, INCLUDING the all-zero case — an omit-when-clean pin is
			// vacuous exactly when it matters most. See the field comment.
			resp.RepoScopeExcluded = &manifestRepoScopeExcludedJSON{
				TokenEvents: ex.TokenEvents,
				CostMicro:   ex.CostMicro,
				Outcomes:    ex.OutcomeRecords,
			}
		}
		return nil
	})
	if err != nil {
		h.logger.Error("report manifest snapshot", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	resp.Watermarks.Ledgers = marks.Ledgers

	if h.aggregation.Anonymized() {
		k := h.kAnonymity
		resp.K = &k
		// 🔴 WITHHOLD THE WINDOW BLOCK AND SAY SO. Unfloored counts of work over a
		// caller-chosen window are exactly what #593's strip pass refuses on
		// /scores; see manifestWatermarksJSON for why this is blunt rather than
		// computed. resp.Watermarks.Window is left nil (omitempty drops the key),
		// and the declaration below is what stops the absence from reading as
		// "this install has no rows".
		resp.KAnonSuppressed = &manifestKAnonJSON{
			WithheldWindow: true,
			// The digests go with it, and for the identical reason: events_digest.rows
			// and outcomes_digest.rows are the same unfloored counts of work over the
			// same caller-chosen window. Declared here as well as in digests_omitted so
			// this block is a COMPLETE statement of what k-anonymity withheld — a
			// partial withhold record is the same defect as a silent one.
			//
			// 🔑 DERIVED FROM THE RESPONSE, NOT RE-ASSERTED. A hand-written `true`
			// would be a SECOND statement of the same fact, and the two can only ever
			// drift in one direction: a declaration that a field was withheld while
			// the field sits published beside it. Reading resp.EventsDigest makes this
			// a report of what the switch above actually did.
			WithheldDigests: resp.EventsDigest == nil,
			KAnonymity:      h.kAnonymity,
			Aggregation:     h.aggregation.String(),
			Reason: "watermarks.window and the two window digests carry unfloored row counts over a " +
				"caller-chosen window; narrowing the window alone can shrink a cohort below the " +
				"k-anonymity floor (#593), so they are withheld in an anonymized aggregation mode.",
		}
	} else {
		window := marks.Window
		resp.Watermarks.Window = &window
	}
	writeJSON(w, http.StatusOK, resp)
}
