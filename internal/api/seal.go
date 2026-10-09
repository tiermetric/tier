package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	sqlite "modernc.org/sqlite"
)

// errSealDeveloperMode refuses a seal in developer mode, which publishes named
// rows and never serves a sealed period (#913).
var errSealDeveloperMode = errors.New("developer mode never seals a reporting period")

// errPeriodNotSealable refuses a period that is open, inside its grace lag,
// before the earliest sealable period, or reaches into the retention horizon.
var errPeriodNotSealable = errors.New("period is not sealable")

// errSealingNotArmed refuses the first seal, which pins the earliest sealable
// month, until the operator arms sealing (#913-D4 ruling B′).
var errSealingNotArmed = errors.New("sealing not armed: no month is sealed until the operator arms sealing " +
	"with seal_from or `tierd seal --arm` (#913)")

// errAwaitingSeal is a read's refusal of a month that is sealable but not yet
// sealed: only the background sealer seals, and reads never do (#913-D4).
var errAwaitingSeal = errors.New("month is sealable but not yet sealed")

// errSealGapped is a read's refusal of a month recorded as a permanent gap
// (#913-D6): it is never sealed.
var errSealGapped = errors.New("month is recorded as a gap")

// errSealDryRun is a dry run's seal, stopped before SealReport.
var errSealDryRun = errors.New("dry run: the seal was computed and not written")

// errSealFloorMoved refuses a seal whose snapshot's earliest sealable month is
// not the one its pass started from; the next pass starts from the new one.
var errSealFloorMoved = errors.New("the earliest sealable month moved during the seal pass")

// errSealRefoldMismatch refuses a seal whose stored fold inputs do not refold
// to the body being sealed (#913-D3 ruling A').
var errSealRefoldMismatch = errors.New("stored fold inputs do not refold to the sealed body")

// errSourceBehind refuses a seal of a month a registered, unretired source has
// not settled past (#913-D9 ruling C′); the next pass retries it.
var errSourceBehind = errors.New("a configured source has not settled through the month's end")

// sourceBehindError is errSourceBehind naming month and each source it waits on.
type sourceBehindError struct {
	month Period
	words string
}

func (e sourceBehindError) Error() string { return e.words }
func (e sourceBehindError) Unwrap() error { return errSourceBehind }

// sealAttempts bounds how often sealFloored recomputes a period after an erase
// commits while it is computed (store.ErrSealEraseRaced).
const sealAttempts = 3

// sealFoldRule versions the grouping, census and fold code a sealed body is
// computed by. Bump it with ANY change to how a period's population is grouped,
// counted or folded (groupWindow, kanonCensusFor, the #856 predicate,
// AggregateFolded), so two bodies split by different rules never share a
// config_digest.
const sealFoldRule = 1

// sealConfig is what sealing reads beside the handler's level and k.
type sealConfig struct {
	grace time.Duration
	// sealFrom arms sealing (#913-D4 ruling B′): the earliest sealable month is
	// the later of it and the first full month of cost coverage. A zero Start
	// leaves sealing unarmed until a floor is pinned.
	sealFrom Period
}

// sealTick is the interval between background seal passes (#913-D4).
const sealTick = time.Hour

// sealer seals closed reporting periods in the background, one at a time, and
// the read path serves the stored bytes (#913, #914 ruling A, #913-D4 ruling
// B′). The body and the config digest both read the handler's level and k, so
// they cannot disagree.
type sealer struct {
	h   *Handler
	now func() time.Time
	// beforeSeal, when set, runs after the body and fold inputs are computed and
	// before SealReport's transaction begins.
	beforeSeal func()
	// cfg is the config every seal and sealed read uses; newSealer validates it.
	cfg sealConfig
	// dryRun stops each seal after its computation, before SealReport, with
	// errSealDryRun: nothing is written.
	dryRun bool
	// inSnapshot, when set, runs inside each seal's read snapshot.
	inSnapshot func()
	// tick is the interval between background passes.
	tick time.Duration
	// passMu serialises seal passes: at most one seal snapshot is open, so a
	// seal holds at most one pooled connection (#913-D4).
	passMu sync.Mutex
	// failures counts failed seals; owedSince is when the oldest month still
	// owed became sealable (zero when none is), in UnixNano; lastPass is the
	// last pass's end in UnixNano; notArmed is set while sealing is unarmed;
	// passing is set while a pass runs, so a read never names a month the pass
	// may be sealing as unreported.
	failures, owedSince, lastPass atomic.Int64
	notArmed, passing             atomic.Bool
	// stall is the month the last pass failed to seal and why; nil when the
	// last pass sealed every month it owed.
	stall atomic.Pointer[sealStall]
}

// sealStall is an owed month a pass failed to seal, with sealFailureOf's code
// and words for its error: a read serves those, never the error's text.
type sealStall struct {
	month        Period
	code, reason string
}

// sealUnreported is the stall a read names for an overdue month no seal pass
// has reported failing while no pass runs (#913-D8 ruling C): serve is stopped,
// read-only, or not sealing, or its last pass failed before reaching the month.
var sealUnreported = sealFailure{code: "unreported",
	words: "no seal pass has reported on it: check that tierd serve is running and not read-only"}

// sealOverdueTicks is how many seal passes after its sealable_at an owed month
// may stay unsealed before a read names it as stalled with no pass reporting.
const sealOverdueTicks = 2

// sealFailure is a category of seal failure: code is what a gap records
// (store.SealedGap.Category) and words what a read serves. A transient failure
// is one a later pass can clear, for which `tierd seal --skip` records no gap.
type sealFailure struct {
	code, words string
	transient   bool
	errs        []error
}

// sealFailures are the categories in match order; a code is never reassigned.
var sealFailures = []sealFailure{
	{"write_lock_busy", "the database write lock was busy", true, []error{store.ErrWriteLockUnavailable}},
	{"erase_raced", "an erasure raced every attempt", true, []error{store.ErrSealEraseRaced}},
	{"clock_behind", "the server clock is behind the newest seal", true, []error{store.ErrSealClockBehind}},
	{store.SealGapRefoldMismatch, "its stored inputs did not refold to its body", false, []error{errSealRefoldMismatch}},
	{store.SealGapFloorMoved, "the earliest sealable month moved", true, []error{errSealFloorMoved, store.ErrSealBeforeFloor}},
	{store.SealGapNotNext, "another process sealed a different month first", true, []error{store.ErrSealNotNext}},
	{store.SealGapOverlap, "it overlaps a period sealed under another config", false, []error{store.ErrSealedPeriodOverlap}},
	// Not transient, so `tierd seal --skip` can record the month as a gap once the
	// operator confirms the stall it names (#913-D9).
	{store.SealGapSourceBehind, "a configured source has not settled through the month's end", false, []error{errSourceBehind}},
	{"gapped", "it is recorded as a gap", true, []error{store.ErrSealedPeriodGapped}},
	{store.SealGapNotSealable, "it is not sealable", true, []error{errPeriodNotSealable}},
	{"cancelled", "the seal was cancelled", true, []error{context.Canceled, context.DeadlineExceeded}},
}

// sealFailureInternal is every error no sealFailures entry matches.
var sealFailureInternal = sealFailure{code: store.SealGapInternal, words: "an internal error"}

// sealFailureOf is err's category.
func sealFailureOf(err error) sealFailure {
	for _, f := range sealFailures {
		for _, e := range f.errs {
			if errors.Is(err, e) {
				return f
			}
		}
	}
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case 5, 6, 7, 10, 13: // SQLITE_BUSY, LOCKED, NOMEM, IOERR, FULL
			return sealFailureStorage
		}
	}
	return sealFailureInternal
}

// sealFailureStorage is the SQLite errors a later pass can clear, for which
// `tierd seal --skip` records no gap.
var sealFailureStorage = sealFailure{code: "storage", words: "the database was busy, locked, out of memory or space, or failed to read or write", transient: true}

// sealFailureWords is the words for a recorded category code, or the code
// itself when this binary does not know it.
func sealFailureWords(code string) string {
	for _, f := range append(sealFailures, sealFailureInternal) {
		if f.code == code {
			return f.words
		}
	}
	return code
}

// SealStallWords is the words for a stall_reason code a read serves, and false
// for a code this binary does not know (a later release may add codes).
func SealStallWords(code string) (string, bool) {
	for _, f := range append([]sealFailure{sealUnreported, sealFailureStorage, sealFailureInternal}, sealFailures...) {
		if f.code == code {
			return f.words, true
		}
	}
	return "", false
}

// minSealGrace is #913-D2's minimum grace: a month sealed sooner after it
// closes permanently leaves out rows that arrive late.
const minSealGrace = 24 * time.Hour

// newSealer returns h's sealer under cfg, refusing a grace below minSealGrace
// and a sealFrom that is set but not a calendar month.
func newSealer(h *Handler, cfg sealConfig) (*sealer, error) {
	if cfg.grace < minSealGrace {
		return nil, fmt.Errorf("report grace %s is below the minimum %s (#913)", cfg.grace, minSealGrace)
	}
	if f := cfg.sealFrom; !f.Start.IsZero() && (f.Kind != periodMonth || !f.Start.Equal(monthOf(f.Start).Start)) {
		return nil, fmt.Errorf("the month sealing is armed from, %s, is not the start of a calendar month (#913)",
			f.Start.Format(time.RFC3339))
	}
	return &sealer{h: h, now: time.Now, cfg: cfg, tick: sealTick}, nil
}

// sealConfigDigest is sealed_report.config_digest: every setting that changes a
// period's breakdown — the fold rule version, the aggregation level, the period
// size and the effective k floor. Grace, the price table and the rubric change
// when or which figures a period holds, never how its population is split into rows.
func sealConfigDigest(rule int, level scoring.AggregationMode, size periodKind, k int) string {
	return sealConfigDigestOf(rule, level.String(), size.String(), k)
}

// sealConfigDigestOf is sealConfigDigest over the stored spellings of level and size.
func sealConfigDigestOf(rule int, level, size string, k int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "tier-seal-config/v1\x00rule=%d\x00level=%s\x00period_size=%s\x00k=%d", rule, level, size, k))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sealOrLoad returns period p's sealed body and seal time. A sealed period's
// stored bytes are returned verbatim, under whatever config sealed it (#913-D1
// ruling A). An unsealed, sealable period is computed with the folded functions
// and sealed; when a concurrent sealer wins, its bytes are returned, never this
// computation's. A span overlapping a period sealed under another config is
// refused with store.ErrSealedPeriodOverlap. An erase or alias edit that
// commits while the period is computed discards the computation; after sealAttempts the
// store.ErrSealEraseRaced is returned.
func (s *sealer) sealOrLoad(ctx context.Context, p Period) ([]byte, time.Time, error) {
	body, at, _, err := s.sealFloored(ctx, p, Period{}, false)
	return body, at, err
}

// sealFloored is sealOrLoad refusing, with errSealFloorMoved, a seal whose
// snapshot's earliest sealable month is not want; a zero want is not checked.
// won reports whether this call sealed p, rather than finding it sealed. With
// first, the seal is refused unless it is the one that arms sealing
// (store.SealCheck.First).
func (s *sealer) sealFloored(ctx context.Context, p, want Period, first bool) (body []byte, at time.Time, won bool, err error) {
	for attempt := 1; ; attempt++ {
		body, at, won, err = s.sealOnce(ctx, p, want, first)
		if !errors.Is(err, store.ErrSealEraseRaced) || attempt == sealAttempts {
			return body, at, won, err
		}
	}
}

func (s *sealer) sealOnce(ctx context.Context, p, want Period, first bool) ([]byte, time.Time, bool, error) {
	h := s.h
	if !h.aggregation.Anonymized() {
		return nil, time.Time{}, false, errSealDeveloperMode
	}
	start, end := p.Bounds()
	var (
		epoch  int64
		stored *store.SealedReport
		floor  Period
		resp   scoresResponse
		inputs []scoring.LabelInput
	)
	// Every read below is one snapshot, so the body, its fold inputs and the
	// floor describe one database state.
	err := h.store.ReadSnapshot(ctx, func(snap *store.Snapshot) error {
		if s.inSnapshot != nil {
			s.inSnapshot()
		}
		// The epoch must be read inside this snapshot (its order within it does
		// not matter): an erase or alias edit the snapshot cannot see then moves
		// the epoch, and SealReport refuses the seal.
		var err error
		if epoch, err = snap.EraseEpoch(ctx); err != nil {
			return err
		}
		rep, err := snap.SealedReport(ctx, p.Kind.String(), start)
		if err == nil {
			stored = &rep
			return nil
		}
		if !errors.Is(err, store.ErrSealedReportNotFound) {
			return err
		}
		if floor, err = s.sealable(ctx, snap, p); err != nil {
			return err
		}
		if !want.Start.IsZero() && !floor.Start.Equal(want.Start) {
			return fmt.Errorf("%w: from %s to %s", errSealFloorMoved, want, floor)
		}
		if err := s.gate(ctx, snap, p); err != nil {
			return err
		}
		resp, inputs, err = h.scoresForWindow(ctx, snap, scoresQuery{since: start, until: end, scope: store.FleetWide, reportUnjoined: true}, true)
		return err
	})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	if stored != nil {
		return stored.Body, stored.SealedAt, false, nil
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("marshal sealed body: %w", err)
	}
	rollups, persons := sealedInputs(inputs)
	k := max(h.kAnonymity, scoring.MinKAnonymity)
	// The body's rows and suppression are this fold's; the body nulls the rows
	// when the residual is withheld (#864), so the check compares the fold itself.
	teams, sup := scoring.AggregateFolded(inputs, k)
	refold, err := refoldCheck(k, teamRows(teams), sup)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	digest := sha256.Sum256(body)
	build := h.buildIdentity()
	if s.beforeSeal != nil {
		s.beforeSeal()
	}
	if s.dryRun {
		return nil, time.Time{}, false, errSealDryRun
	}
	sealed, won, err := h.store.SealReport(ctx, store.SealedReport{
		Level: h.aggregation.String(), PeriodSize: p.Kind.String(), PeriodStart: start, PeriodEnd: end,
		K: k, ConfigDigest: sealConfigDigest(sealFoldRule, h.aggregation, p.Kind, k),
		Body: body, BodyDigest: "sha256:" + hex.EncodeToString(digest[:]),
		ToolVersion: build.Version, ToolCommit: build.Commit,
	}, rollups, persons, store.SealCheck{EraseEpoch: epoch, Refold: refold, Floor: floor.Start, First: first,
		Sources: func(ctx context.Context, snap *store.Snapshot) error { return s.gate(ctx, snap, p) }})
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("seal period %s: %w", p, err)
	}
	return sealed.Body, sealed.SealedAt, won, nil
}

// sealable refuses a period that starts before the earliest sealable period
// (floor), ends after now − grace, or reaches into the retention horizon; it
// returns the floor, which the first seal pins. The floor is checked first: a
// month before it never becomes sealable, so it is never refused as in grace.
func (s *sealer) sealable(ctx context.Context, r floorReader, p Period) (Period, error) {
	floor, ok, err := s.floor(ctx, r)
	if err != nil {
		return Period{}, err
	}
	if !ok || p.Start.Before(floor.Start) {
		return Period{}, fmt.Errorf("%w: %s is before the earliest sealable month", errPeriodNotSealable, p)
	}
	last := lastSealable(s.now(), s.cfg.grace)
	if p.Kind != last.Kind || p.Start.After(last.Start) {
		return Period{}, fmt.Errorf("%w: %s is open or inside its grace lag until %s", errPeriodNotSealable, p,
			sealableAt(p, s.cfg.grace).Format(time.RFC3339))
	}
	start, _ := p.Bounds()
	if err := s.h.checkWindowRetention(start); err != nil {
		return Period{}, fmt.Errorf("%w: %v", errPeriodNotSealable, err)
	}
	return floor, nil
}

// watermarkReader is the source_watermark read: h.store, or a seal's snapshot.
type watermarkReader interface {
	SourceWatermarks(ctx context.Context) ([]store.SourceWatermark, error)
}

// gate is a sourceBehindError when a registered, unretired source in r has not
// settled past month p (#913-D9 ruling C′). It is read in the seal path, never
// in sealable or lastSealable, so sealable_at and next_seal_at keep their
// meaning.
func (s *sealer) gate(ctx context.Context, r watermarkReader, p Period) error {
	words, err := behind(ctx, r, p)
	if err != nil || words == "" {
		return err
	}
	return sourceBehindError{month: p, words: words}
}

// behind names each registered, unretired source in r that does not certify
// month p, "" when every one does. A source certifies p only when one gap-free
// run of its successful passes covers all of p and it lost no spend in p. Before serve has registered
// its sources every month waits; after, a source with no row is not configured
// and gates nothing, and a row with no run yet gates every month.
func behind(ctx context.Context, r watermarkReader, p Period) (string, error) {
	rows, err := r.SourceWatermarks(ctx)
	if errors.Is(err, store.ErrSourcesNotRegistered) {
		return fmt.Sprintf("sealing %s waits: %v; start it once with its --config", p, err), nil
	} else if err != nil {
		return "", err
	}
	start, end := p.Bounds()
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	var why []string
	for _, w := range rows {
		g, lost := w.LostIn(start, end)
		switch {
		case w.Retired, w.Covers(start, end):
		case lost:
			why = append(why, fmt.Sprintf("%s lost spend from %s to %s that it can never re-read: the month never seals "+
				"automatically; record it as a gap with `tierd seal --skip`", w.Source, at(g.From), at(g.Through)))
		case !w.Settled:
			why = append(why, w.Source+" has not completed a successful pass")
		case w.CoveredFrom.After(start) && !w.GapFrom.IsZero() && w.GapFrom.Before(end):
			why = append(why, fmt.Sprintf("%s has an unfetchable gap from %s to %s", w.Source, at(w.GapFrom), at(w.CoveredFrom)))
		case w.CoveredFrom.After(start) && len(w.Earlier) == 0:
			why = append(why, fmt.Sprintf("%s has no record before %s, its first recorded coverage: an earlier month never "+
				"seals automatically (arm a later month; for subscription fees, set active_since and restart serve)", w.Source, at(w.CoveredFrom)))
		case w.CoveredFrom.After(start):
			why = append(why, fmt.Sprintf("%s has no unbroken record before %s", w.Source, at(w.CoveredFrom)))
		case w.SettledThrough.Before(end):
			why = append(why, fmt.Sprintf("%s has settled only through %s", w.Source, at(w.SettledThrough)))
		}
	}
	if len(why) == 0 {
		return "", nil
	}
	return fmt.Sprintf("sealing %s waits on %s", p, strings.Join(why, "; ")), nil
}

// floorReader is the reads floor makes: h.store, or a seal's snapshot.
type floorReader interface {
	SealFloor(ctx context.Context) (start time.Time, ok bool, err error)
	CostCoverageStart(ctx context.Context, scope store.RepoScope) (time.Time, bool, error)
}

// floor is the earliest sealable period: the one pinned at the first seal, else
// the later of cfg.sealFrom and the first full month of fleet-wide cost coverage
// (#913-D2 item 4, #913-D4 ruling B′). ok is false when no cost is covered; the
// error is errSealingNotArmed when no floor is pinned and sealFrom is unset.
func (s *sealer) floor(ctx context.Context, r floorReader) (Period, bool, error) {
	if start, ok, err := r.SealFloor(ctx); err != nil || ok {
		return Period{Kind: periodMonth, Start: start.UTC()}, ok, err
	}
	if s.cfg.sealFrom.Start.IsZero() {
		return Period{}, false, errSealingNotArmed
	}
	horizon, _, err := r.CostCoverageStart(ctx, store.FleetWide)
	if err != nil {
		return Period{}, false, fmt.Errorf("query cost coverage start: %w", err)
	}
	p, ok := earliestSealable(horizon)
	if ok && p.Start.Before(s.cfg.sealFrom.Start) {
		p = s.cfg.sealFrom
	}
	return p, ok, nil
}

// load returns p's stored body and seal time. A read never seals (#913-D4
// ruling B′): an unsealed month is refused with the reason it is not sealed —
// errSealingNotArmed, sealable's refusal, or errAwaitingSeal.
func (s *sealer) load(ctx context.Context, p Period) ([]byte, time.Time, error) {
	rep, err := s.h.store.SealedReport(ctx, p.Kind.String(), p.Start)
	if errors.Is(err, store.ErrSealedReportNotFound) {
		err = s.gapped(ctx, p)
		if err == nil {
			if _, err = s.sealable(ctx, s.h.store, p); err == nil {
				err = s.awaitingSeal(ctx, p)
			}
		}
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return rep.Body, rep.SealedAt, nil
}

// gapped is errSealGapped naming p's recorded gap, its category and the
// operator's reason, or nil when p is not a gap.
func (s *sealer) gapped(ctx context.Context, p Period) error {
	g, err := s.h.store.SealedGap(ctx, p.Kind.String(), p.Start)
	switch {
	case errors.Is(err, store.ErrSealedGapNotFound):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("%w: %s is never sealed: on %s the operator recorded it as a gap after its seal failed "+
		"(%s), with the reason %q; sealing continues with the months after it", errSealGapped, p,
		g.CreatedAt.UTC().Format("2006-01-02"), sealFailureWords(g.Category), g.Reason)
}

// awaitingSeal is load's refusal of sealable month p. While sealing is stalled
// at p or an earlier month it names that month and why, and promises no next
// pass.
func (s *sealer) awaitingSeal(ctx context.Context, p Period) error {
	st, ok, err := s.stalled(ctx)
	if err != nil {
		return err
	}
	if ok && !p.Start.Before(st.month.Start) {
		return fmt.Errorf("%w: %s: months are sealed oldest first, and sealing is stalled at %s (%s)",
			errAwaitingSeal, p, st.month, st.reason)
	}
	return fmt.Errorf("%w: %s is sealed by the server's next seal pass", errAwaitingSeal, p)
}

// stalled is the month sealing is stalled at, read from the database (#913-D8
// ruling C): the first owed month, when the last pass failed at it or it is
// past its sealable_at by sealOverdueTicks passes. The last pass's failure
// names the reason only while its month is still the first owed one, so a month
// a CLI skip or arm has since cleared is never named; otherwise, while no pass
// runs, the reason is sealUnreported. ok is false while nothing is stalled or
// sealing is unarmed.
func (s *sealer) stalled(ctx context.Context) (st sealStall, ok bool, err error) {
	next, _, owed, err := s.firstOwed(ctx)
	switch {
	case errors.Is(err, errSealingNotArmed):
		return sealStall{}, false, nil
	case err != nil || !owed:
		return sealStall{}, false, err
	}
	if live := s.stall.Load(); live != nil && live.month.Start.Equal(next.Start) {
		return *live, true, nil
	}
	if s.passing.Load() || !s.now().After(sealableAt(next, s.cfg.grace).Add(sealOverdueTicks*s.tick)) {
		return sealStall{}, false, nil
	}
	// A source behind the month is the reason no pass seals it (#913-D9).
	switch words, err := behind(ctx, s.h.store, next); {
	case err != nil:
		return sealStall{}, false, err
	case words != "":
		return sealStall{month: next, code: store.SealGapSourceBehind, reason: words}, true, nil
	}
	return sealStall{month: next, code: sealUnreported.code, reason: sealUnreported.words}, true, nil
}

// SealOverdue is SealStalled's answer: Month (YYYY-MM) is "" when no month is
// overdue; OwedSince is its sealable_at; First is set when no month is sealed or
// gapped yet, so it cannot be skipped.
type SealOverdue struct {
	Month, OwedSince string
	First            bool
}

// SealStalled is the month sealing is stalled at by the database alone
// (sealer.stalled with no pass reporting), under serve's grace and seal_from
// ("" when unset). Developer mode never seals, so nothing is overdue in it.
func (h *Handler) SealStalled(ctx context.Context, grace time.Duration, sealFrom string) (SealOverdue, error) {
	st, err := h.SealStatus(ctx, grace, sealFrom)
	return st.Overdue, err
}

// SealState is SealStatus's answer, each month YYYY-MM and "" when there is
// none: Floor is the pinned earliest sealed month; Newest the newest month
// sealed or recorded as a gap; Owed the first owed month, with OwedSealableAt
// its sealable_at, and Due set when it is sealable now; Behind names the
// sources a due month waits on (#913-D9); Overdue is SealStalled's answer.
// Armed is false while no floor is pinned and seal_from is unset.
type SealState struct {
	Armed                                       bool
	Floor, Newest, Owed, OwedSealableAt, Behind string
	Due                                         bool
	Overdue                                     SealOverdue
}

// SealStatus is the sealing state by the database alone, under serve's grace
// and seal_from ("" when unset), for `tierd seal --status` (#913-D5 ruling C′,
// D8 ruling C). Overdue is sealer.stalled's rule with no pass reporting.
// Developer mode never seals, so its state is empty.
func (h *Handler) SealStatus(ctx context.Context, grace time.Duration, sealFrom string) (SealState, error) {
	s, err := newServeSealer(h, grace, sealFrom)
	if err != nil || !h.aggregation.Anonymized() {
		return SealState{}, err
	}
	var st SealState
	start, pinned, err := h.store.SealFloor(ctx)
	if err != nil {
		return st, err
	}
	if pinned {
		st.Floor = Period{Kind: periodMonth, Start: start.UTC()}.String()
	}
	next, floor, owed, err := s.firstOwed(ctx)
	switch {
	case errors.Is(err, errSealingNotArmed):
		return st, nil
	case err != nil:
		return st, err
	}
	st.Armed = true
	if owed {
		if floor.Start.IsZero() {
			st.Newest = next.prev().String()
		}
		st.Owed, st.OwedSealableAt = next.String(), sealableAt(next, grace).Format(time.RFC3339)
		st.Due = !next.Start.After(lastSealable(s.now(), grace).Start)
		if st.Due {
			if st.Behind, err = behind(ctx, h.store, next); err != nil {
				return st, err
			}
		}
	}
	stall, ok, err := s.stalled(ctx)
	if err != nil || !ok {
		return st, err
	}
	st.Overdue = SealOverdue{Month: stall.month.String(), OwedSince: s.stallJSON(stall).OwedSince, First: st.Newest == ""}
	return st, nil
}

// sealDue seals every month owed, oldest first and one at a time: each month
// after the newest sealed one, or from the earliest sealable month while none is
// sealed, through the latest sealable month. It stops at the first failure,
// which the next pass retries.
func (s *sealer) sealDue(ctx context.Context) error {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	s.passing.Store(true)
	defer s.passing.Store(false)
	defer func() { s.lastPass.Store(s.now().UnixNano()) }()
	next, floor, owed, err := s.firstOwed(ctx)
	switch {
	case errors.Is(err, errSealingNotArmed):
		s.notArmed.Store(true)
		s.owedSince.Store(0)
		return err
	case err != nil:
		// A failed lookup leaves the gauge, the stall and armed as they were.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.failures.Add(1)
		return err
	}
	s.notArmed.Store(false)
	for last := lastSealable(s.now(), s.cfg.grace); owed && !next.Start.After(last.Start); next = next.next() {
		if err := s.sealRecovered(ctx, next, floor); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.failures.Add(1)
			s.owedSince.Store(sealableAt(next, s.cfg.grace).UnixNano())
			f := sealFailureOf(err)
			reason := f.words
			if sb := (sourceBehindError{}); errors.As(err, &sb) {
				reason = sb.words
			}
			s.stall.Store(&sealStall{month: next, code: f.code, reason: reason})
			return fmt.Errorf("seal pass stopped at %s: %w", next, err)
		}
	}
	s.owedSince.Store(0)
	s.stall.Store(nil)
	return nil
}

// errSealPanicked is a seal that panicked: its category is internal, and the
// background sealer keeps running.
var errSealPanicked = errors.New("the seal panicked")

// sealRecovered is sealFloored with a panic logged and returned as
// errSealPanicked. The seal's read and write transactions roll back as the
// panic unwinds, so a panicked seal commits nothing.
func (s *sealer) sealRecovered(ctx context.Context, p, floor Period) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.h.logger.Error("seal panicked", "month", p.String(), "panic", logSafeStr(fmt.Sprint(r)),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("%w: %s", errSealPanicked, p)
		}
	}()
	_, _, _, err = s.sealFloored(ctx, p, floor, false)
	return err
}

// firstOwed is the month after the newest one sealed or recorded as a gap, else
// the earliest sealable month, which is then also floor (zero otherwise); owed
// is false when there is neither.
func (s *sealer) firstOwed(ctx context.Context) (next, floor Period, owed bool, err error) {
	newest, ok, err := s.h.store.LatestSealedPeriod(ctx, periodMonth.String())
	if err != nil {
		return Period{}, Period{}, false, err
	}
	gap, gapped, err := s.h.store.LatestSealedGap(ctx, periodMonth.String())
	if err != nil {
		return Period{}, Period{}, false, err
	}
	if gapped && (!ok || gap.After(newest)) {
		newest, ok = gap, true
	}
	if ok {
		return Period{Kind: periodMonth, Start: newest.UTC()}.next(), Period{}, true, nil
	}
	floor, owed, err = s.floor(ctx, s.h.store)
	return floor, floor, owed, err
}

// run is the background sealer: a pass now, then one every tick until ctx, the
// server's shutdown context, ends.
func (s *sealer) run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		var sb sourceBehindError
		switch err := s.sealDue(ctx); {
		case err == nil, errors.Is(err, errSealingNotArmed), ctx.Err() != nil:
		case errors.As(err, &sb):
			s.h.logger.Info("background seal pass waits for sources to settle past the month's end (#913)",
				"month", sb.month.String(), "waiting", sb.words)
		default:
			s.h.logger.Error("background seal pass", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// newServeSealer is h's sealer under serve's grace and seal_from ("" when unset).
func newServeSealer(h *Handler, grace time.Duration, sealFrom string) (*sealer, error) {
	cfg := sealConfig{grace: grace}
	if sealFrom != "" {
		p, err := parsePeriod(sealFrom)
		if err != nil {
			return nil, err
		}
		cfg.sealFrom = p
	}
	return newSealer(h, cfg)
}

// EnableSealing builds h's sealer under serve's grace and seal_from ("" when
// unset) in team/division mode (#913); developer mode gets none. Call it after
// SetAggregation and before serving. Every anonymised /scores, /report_manifest
// and /scores/compare read is then a sealed month, and a read-only server,
// which never starts the sealer, serves the months a writable one sealed.
func (h *Handler) EnableSealing(grace time.Duration, sealFrom string) error {
	if !h.aggregation.Anonymized() {
		return nil
	}
	s, err := newServeSealer(h, grace, sealFrom)
	if err != nil {
		return err
	}
	h.sealer = s
	return nil
}

// SealsInBackground reports whether StartSealer starts h's background sealer on
// a server that is read-only when readOnly is set: a read-only server,
// developer mode and a handler with no sealer never start it (#913-D4).
func (h *Handler) SealsInBackground(readOnly bool) bool {
	return h.sealer != nil && !readOnly && h.aggregation.Anonymized()
}

// StartSealer starts h's background sealer on ctx when SealsInBackground: a
// pass now, then one every tick until ctx ends. It reports whether it did;
// done closes once the sealer has stopped, and is closed already when it never
// started, so a shutdown can always wait on it.
func (h *Handler) StartSealer(ctx context.Context, readOnly bool) (done <-chan struct{}, started bool) {
	ch := make(chan struct{})
	if !h.SealsInBackground(readOnly) {
		close(ch)
		return ch, false
	}
	go func() {
		defer close(ch)
		h.sealer.run(ctx)
	}()
	return ch, true
}

// SealConfigGap is the newest sealed month and the config it was sealed under
// when that differs from the current config (#913-D1 ruling A); Newest is ""
// when it does not. From is the first month the current config applies to.
type SealConfigGap struct {
	Newest, From, Sealed, Current string
}

// SealConfigGap compares the newest sealed month's config with the current one.
// A handler with no sealer never seals, so it reports no gap.
func (h *Handler) SealConfigGap(ctx context.Context) (SealConfigGap, error) {
	if h.sealer == nil {
		return SealConfigGap{}, nil
	}
	start, ok, err := h.store.LatestSealedPeriod(ctx, periodMonth.String())
	if err != nil || !ok {
		return SealConfigGap{}, err
	}
	rep, err := h.store.SealedReport(ctx, periodMonth.String(), start)
	if err != nil {
		return SealConfigGap{}, err
	}
	cur := h.currentSealConfig()
	if cur.Digest == rep.ConfigDigest {
		return SealConfigGap{}, nil
	}
	from, _, _, err := h.sealer.firstOwed(ctx)
	if err != nil {
		return SealConfigGap{}, err
	}
	return SealConfigGap{Newest: monthOf(start).String(), From: from.String(),
		Sealed: sealedConfigOf(rep.Level, rep.PeriodSize, rep.K, rep.ConfigDigest).label(), Current: cur.label()}, nil
}

// sealerHealth is the background sealer's failure counter and health gauge:
// failed seals so far, when the oldest month still owed became sealable (zero
// when none is), the end of the last pass (zero before the first), and whether
// the last pass found sealing armed.
type sealerHealth struct {
	failures  int64
	owedSince time.Time
	lastPass  time.Time
	armed     bool
}

func (s *sealer) health() sealerHealth {
	h := sealerHealth{failures: s.failures.Load(), armed: !s.notArmed.Load()}
	if n := s.owedSince.Load(); n != 0 {
		h.owedSince = time.Unix(0, n).UTC()
	}
	if n := s.lastPass.Load(); n != 0 {
		h.lastPass = time.Unix(0, n).UTC()
	}
	return h
}

// sealedInputs is the stored form of a period's fold inputs: one rollup per
// label, and one person row per label, set and canonical id, the set named by
// scoring.PeopleSetName or scoring.MeasureNames.
func sealedInputs(inputs []scoring.LabelInput) ([]store.SealedRollup, []store.SealedPerson) {
	var rollups []store.SealedRollup
	var persons []store.SealedPerson
	for _, in := range inputs {
		rollups = append(rollups, store.SealedRollup{
			Label: in.Label, WeightedPoints: in.Sums.WeightedPoints, TotalCostUSD: in.Sums.TotalCostUSD,
			ActualPaidUSD: in.Sums.ActualPaidUSD, RealtimeUSD: in.Sums.RealtimeUSD,
			SampleN: in.Sums.SampleN, FlaggedOutcomes: in.Sums.FlaggedOutcomes,
			Has: in.Has, Contributes: in.Contributes,
		})
		for _, id := range in.People {
			persons = append(persons, store.SealedPerson{Label: in.Label, Measure: scoring.PeopleSetName, CanonicalID: id})
		}
		for m, ids := range in.Carriers {
			for _, id := range ids {
				persons = append(persons, store.SealedPerson{Label: in.Label, Measure: scoring.MeasureNames[m], CanonicalID: id})
			}
		}
	}
	return rollups, persons
}

// refoldCheck is store.SealCheck.Refold for a body folded to teams and sup: the
// stored inputs must refold at k to exactly both.
func refoldCheck(k int, teams []teamScoreJSON, sup scoring.KAnonSuppression) (func([]store.SealedRollup, []store.SealedPersonKey) error, error) {
	want, err := json.Marshal(teams)
	if err != nil {
		return nil, err
	}
	return func(rs []store.SealedRollup, ps []store.SealedPersonKey) error {
		stored, err := foldInputsOf(rs, ps)
		if err != nil {
			return err
		}
		teams, gotSup := scoring.AggregateFolded(stored, k)
		got, err := json.Marshal(teamRows(teams))
		if err != nil {
			return err
		}
		if gotSup != sup || !bytes.Equal(got, want) {
			return errSealRefoldMismatch
		}
		return nil
	}, nil
}

// teamRows is the served form of a fold's named rows.
func teamRows(teams []scoring.TeamScore) []teamScoreJSON {
	var rows []teamScoreJSON
	for _, ts := range teams {
		rows = append(rows, newTeamScoreJSON(ts))
	}
	return rows
}

// foldInputsOf is a sealed period's fold inputs as stored, each person named by
// the hex of its key; a person row under an unknown label or set is an error.
func foldInputsOf(rollups []store.SealedRollup, persons []store.SealedPersonKey) ([]scoring.LabelInput, error) {
	measure := map[string]int{}
	for m, name := range scoring.MeasureNames {
		measure[name] = m
	}
	out := make([]scoring.LabelInput, len(rollups))
	byLabel := map[string]*scoring.LabelInput{}
	for i, r := range rollups {
		out[i] = scoring.LabelInput{Label: r.Label, Has: r.Has, Contributes: r.Contributes, Sums: scoring.RollupSums{
			WeightedPoints: r.WeightedPoints, TotalCostUSD: r.TotalCostUSD, ActualPaidUSD: r.ActualPaidUSD,
			RealtimeUSD: r.RealtimeUSD, SampleN: r.SampleN, FlaggedOutcomes: r.FlaggedOutcomes,
		}}
		byLabel[r.Label] = &out[i]
	}
	for _, p := range persons {
		in, key := byLabel[p.Label], hex.EncodeToString(p.Key)
		m, isMeasure := measure[p.Measure]
		switch {
		case in == nil:
			return nil, fmt.Errorf("sealed person row under label %q has no rollup", p.Label)
		case p.Measure == scoring.PeopleSetName:
			in.People = append(in.People, key)
		case isMeasure:
			in.Carriers[m] = append(in.Carriers[m], key)
		default:
			return nil, fmt.Errorf("sealed person row under unknown set %q", p.Measure)
		}
	}
	return out, nil
}
