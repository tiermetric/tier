package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
)

// The sealed read path (#913-D2): in an anonymised mode with a sealer, /scores
// and /report_manifest answer one sealed calendar month, never a free window.

// sealedReadParams is the parameter a sealed read accepts beside its endpoint's
// filters. The allowlists are mode-dependent (#913-D2 item 7): the live ones
// list since/until/before, and this one lists no freeBoundParams key.
var sealedReadParams = []string{"period"}

// sealedScoresFilters and sealedManifestFilters are the live filters of /scores
// and /report_manifest. A sealed month is one stored fleet-wide body, so none is
// answerable from it (#913-D2 item 5); each is refused by name, never recomputed.
var (
	sealedScoresFilters   = []string{"team", "work_type", "repo"}
	sealedManifestFilters = []string{"repo"}
)

// A sealed /scores body is the stored bytes verbatim; its month, seal time and
// #913-D2 item 3's markers ride in these headers rather than a wrapper, so the
// body keeps /scores' shape and its sha256 equals the manifest's body_digest.
// The last three carry the manifest's next_seal_at, earliest_period and
// latest_period.
const (
	headerSealedPeriod   = "Tier-Period"
	headerSealedAt       = "Tier-Sealed-At"
	headerNextSealAt     = "Tier-Next-Seal-At"
	headerEarliestPeriod = "Tier-Earliest-Period"
	headerLatestPeriod   = "Tier-Latest-Period"
)

// SealedManifestSchema tags a sealed period's manifest, a different record from
// a live window's (ManifestSchema).
const SealedManifestSchema = "tiersealedmanifest1"

// sealedRead reports whether a /scores, /report_manifest or /scores/compare
// request is served from sealed months: every anonymised one, except on a
// handler built WithUnsealedRecompute and no sealer. Developer mode reads live.
func (h *Handler) sealedRead() bool {
	return h.aggregation.Anonymized() && (h.sealer != nil || !h.unsealedRecompute)
}

// noSealer answers a sealed read on a handler with no sealer with a 503 and
// reports whether it did: an anonymised report is only ever a sealed month.
func (h *Handler) noSealer(w http.ResponseWriter) bool {
	if h.sealer != nil {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, "sealed months are not available: an anonymised report is "+
		"published only as a sealed calendar month, and this server has no sealer (#913)")
	return true
}

// sealedNotFoundJSON is the 404 for a month that is not sealed and cannot be
// sealed now. sealable_at is when that month becomes sealable; it is absent for
// a month that never will (before the earliest sealable month) or when no time
// is known.
type sealedNotFoundJSON struct {
	Error       string `json:"error"`
	Aggregation string `json:"aggregation"`
	Period      string `json:"period"`
	SealableAt  string `json:"sealable_at,omitempty"`
	SealStallJSON
}

// SealStallJSON names the month sealing is stalled at (sealer.stalled), all
// three fields absent while nothing is (tierd doctor and tierd seal --status
// decode it): stall_reason is a sealFailure code, or
// sealUnreported's, never an error's text, and owed_since is the month's
// sealable_at.
type SealStallJSON struct {
	StalledPeriod string `json:"stalled_period,omitempty"`
	StallReason   string `json:"stall_reason,omitempty"`
	OwedSince     string `json:"owed_since,omitempty"`
}

// stallJSON is the wire form of stall st.
func (s *sealer) stallJSON(st sealStall) SealStallJSON {
	return SealStallJSON{StalledPeriod: st.month.String(), StallReason: st.code,
		OwedSince: sealableAt(st.month, s.cfg.grace).Format(time.RFC3339)}
}

// resolveSealed validates a sealed read's parameters, resolves its month and
// loads it; it never seals (#913-D4). With no ?period= the month is
// markers.defaultMonth (#913-D2 item 3), never defaultSince. On a refusal it has
// written the response and ok is false.
func (h *Handler) resolveSealed(w http.ResponseWriter, r *http.Request, filters []string) (p Period, body []byte, sealedAt time.Time, ok bool) {
	if h.noSealer(w) {
		return
	}
	if err := refuseFreeBounds(r, h.aggregation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Filters are refused by name before the allowlist, which lists only what a
	// sealed read accepts.
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed query string: "+err.Error())
		return
	}
	var given []string
	for _, f := range filters {
		if _, has := q[f]; has {
			given = append(given, "?"+f+"=")
		}
	}
	if len(given) > 0 {
		list := strings.Join(given, ", ")
		writeError(w, http.StatusBadRequest, list+" not accepted with a sealed month (#913): an anonymised "+
			"report is one stored body per calendar month and is never recomputed for a filter. Remove "+list)
		return
	}
	if !rejectUnknownQueryParams(w, r, sealedReadParams...) {
		return
	}
	p, periodGiven, err := parseRequestPeriod(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s, ctx := h.sealer, r.Context()
	var m sealMarkers
	if !periodGiven {
		if m, err = s.markers(ctx); err != nil {
			h.sealedReadFailed(w, p, err)
			return
		}
		p = m.defaultMonth()
	}
	body, sealedAt, err = s.load(ctx, p)
	if err != nil {
		h.writeSealErr(ctx, w, p, err)
		return Period{}, nil, time.Time{}, false
	}
	return p, body, sealedAt, true
}

// writeSealErr answers a sealed read's error for period p.
func (h *Handler) writeSealErr(ctx context.Context, w http.ResponseWriter, p Period, err error) {
	switch {
	case notSealable(err):
		resp := sealedNotFoundJSON{Error: err.Error(), Aggregation: h.aggregation.String(), Period: p.String()}
		if at, known := h.sealer.sealableAtFor(ctx, p); known && !errors.Is(err, errSealGapped) {
			resp.SealableAt = at.Format(time.RFC3339)
		}
		st, stalled, serr := h.sealer.stalled(ctx)
		if serr != nil {
			h.sealedReadFailed(w, p, serr)
			return
		}
		if stalled {
			resp.SealStallJSON = h.sealer.stallJSON(st)
		}
		writeJSON(w, http.StatusNotFound, resp)
	default:
		h.sealedReadFailed(w, p, err)
	}
}

func notSealable(err error) bool {
	return errors.Is(err, errPeriodNotSealable) || errors.Is(err, errSealingNotArmed) || errors.Is(err, errAwaitingSeal) ||
		errors.Is(err, errSealGapped)
}

func (h *Handler) sealedReadFailed(w http.ResponseWriter, p Period, err error) {
	h.logger.Error("sealed read", "period", p.String(), "err", err)
	writeError(w, http.StatusInternalServerError, "db error")
}

// sealableAtFor is when an unsealable month p becomes sealable: its seal time
// when it is at or after the earliest sealable month and open or inside its
// grace lag. known is false otherwise, including for a month before the
// earliest, which never becomes sealable.
func (s *sealer) sealableAtFor(ctx context.Context, p Period) (time.Time, bool) {
	floor, ok, err := s.floor(ctx, s.h.store)
	if err != nil || !ok || p.Start.Before(floor.Start) || !p.Start.After(lastSealable(s.now(), s.cfg.grace).Start) {
		return time.Time{}, false
	}
	return sealableAt(p, s.cfg.grace), true
}

// sealMarkers are #913-D2 item 3's markers. latest is the later of the latest
// sealable month and the newest sealed one (a raised grace never hides a month
// already sealed); earliest is the pinned floor, else the earliest sealable
// month; nextSealAt is when the month after latest becomes sealable, zero
// while sealing is stalled (stall), since no later month seals until then.
type sealMarkers struct {
	earliest, latest, newest Period
	hasEarliest, hasNewest   bool
	nextSealAt               time.Time
	stall                    SealStallJSON
}

// defaultMonth is a read's month with no ?period=: the newest sealed month, else
// latest, or earliest when that is later.
func (m sealMarkers) defaultMonth() Period {
	switch {
	case m.hasNewest:
		return m.newest
	case m.hasEarliest && m.earliest.Start.After(m.latest.Start):
		return m.earliest
	}
	return m.latest
}

func (s *sealer) markers(ctx context.Context) (sealMarkers, error) {
	var m sealMarkers
	var err error
	if m.earliest, m.hasEarliest, err = s.floor(ctx, s.h.store); err != nil && !errors.Is(err, errSealingNotArmed) {
		return m, err
	}
	start, ok, err := s.h.store.LatestSealedPeriod(ctx, periodMonth.String())
	if err != nil {
		return m, err
	}
	m.latest = lastSealable(s.now(), s.cfg.grace)
	if ok {
		m.newest, m.hasNewest = Period{Kind: periodMonth, Start: start.UTC()}, true
		if m.newest.Start.After(m.latest.Start) {
			m.latest = m.newest
		}
	}
	st, stalled, err := s.stalled(ctx)
	if err != nil {
		return m, err
	}
	if stalled {
		m.stall = s.stallJSON(st)
	} else {
		m.nextSealAt = sealableAt(Period{Kind: periodMonth, Start: m.latest.Start.AddDate(0, 1, 0)}, s.cfg.grace)
	}
	return m, nil
}

// serveSealedScores writes a sealed month's stored /scores body byte for byte.
func (h *Handler) serveSealedScores(w http.ResponseWriter, r *http.Request) {
	p, body, sealedAt, ok := h.resolveSealed(w, r, sealedScoresFilters)
	if !ok {
		return
	}
	m, err := h.sealer.markers(r.Context())
	if err != nil {
		h.sealedReadFailed(w, p, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerSealedPeriod, p.String())
	w.Header().Set(headerSealedAt, sealedAt.UTC().Format(time.RFC3339))
	m.setHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// setHeaders sets the markers' Tier-Next-Seal-At (absent while stalled),
// Tier-Earliest-Period and Tier-Latest-Period headers.
func (m sealMarkers) setHeaders(hdr http.Header) {
	if !m.nextSealAt.IsZero() {
		hdr.Set(headerNextSealAt, m.nextSealAt.Format(time.RFC3339))
	}
	if m.hasEarliest {
		hdr.Set(headerEarliestPeriod, m.earliest.String())
	}
	hdr.Set(headerLatestPeriod, m.latest.String())
}

// sealedConfigJSON is the config a month was sealed under: the fields of
// sealConfigDigest, with fold_rule absent when no rule this binary knows yields
// the stored digest.
type sealedConfigJSON struct {
	Aggregation string `json:"aggregation"`
	PeriodSize  string `json:"period_size"`
	K           int    `json:"k"`
	FoldRule    int    `json:"fold_rule,omitempty"`
	Digest      string `json:"digest"`
}

// sealedManifestJSON is a sealed month's manifest. It describes the stored
// report only: it carries no watermark and no count of rows that arrived after
// the seal (#913-D2 item 7).
type sealedManifestJSON struct {
	ManifestSchema string           `json:"manifest_schema"`
	Period         string           `json:"period"`
	PeriodStart    string           `json:"period_start"`
	PeriodEnd      string           `json:"period_end"`
	SealedAt       string           `json:"sealed_at"`
	NextSealAt     string           `json:"next_seal_at,omitempty"`
	EarliestPeriod string           `json:"earliest_period,omitempty"`
	LatestPeriod   string           `json:"latest_period"`
	Config         sealedConfigJSON `json:"config"`
	BodyDigest     string           `json:"body_digest"`
	ToolVersion    string           `json:"tool_version"`
	Commit         string           `json:"commit,omitempty"`
	// ConfigGap is present when the current config differs from the one this
	// month was sealed under (#913-D1 ruling A): the month keeps its sealed config.
	ConfigGap *sealedConfigGapJSON `json:"config_gap,omitempty"`
	SealStallJSON
}

type sealedConfigGapJSON struct {
	Current sealedConfigJSON `json:"current"`
	Note    string           `json:"note"`
}

// sealedConfigOf names a sealed config, recovering its fold rule from the digest.
func sealedConfigOf(level, size string, k int, digest string) sealedConfigJSON {
	c := sealedConfigJSON{Aggregation: level, PeriodSize: size, K: k, Digest: digest}
	c.FoldRule, _ = SealedFoldRuleOf(level, size, k, digest)
	return c
}

// currentSealConfig is the config a month sealed now is sealed under.
func (h *Handler) currentSealConfig() sealedConfigJSON {
	k := max(h.kAnonymity, scoring.MinKAnonymity)
	return sealedConfigOf(h.aggregation.String(), periodMonth.String(), k, sealConfigDigest(sealFoldRule, h.aggregation, periodMonth, k))
}

// label is c for a log line.
func (c sealedConfigJSON) label() string {
	return fmt.Sprintf("aggregation=%s period_size=%s k=%d fold_rule=%d", c.Aggregation, c.PeriodSize, c.K, c.FoldRule)
}

// serveSealedManifest writes the manifest of the month /scores serves for the
// same request.
func (h *Handler) serveSealedManifest(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := h.resolveSealed(w, r, sealedManifestFilters)
	if !ok {
		return
	}
	ctx := r.Context()
	rep, err := h.store.SealedReport(ctx, p.Kind.String(), p.Start)
	var m sealMarkers
	if err == nil {
		m, err = h.sealer.markers(ctx)
	}
	if err != nil {
		h.sealedReadFailed(w, p, err)
		return
	}
	resp := sealedManifestJSON{
		ManifestSchema: SealedManifestSchema,
		Period:         p.String(),
		PeriodStart:    rep.PeriodStart.UTC().Format(time.RFC3339),
		PeriodEnd:      rep.PeriodEnd.UTC().Format(time.RFC3339),
		SealedAt:       rep.SealedAt.UTC().Format(time.RFC3339),
		LatestPeriod:   m.latest.String(),
		Config:         sealedConfigOf(rep.Level, rep.PeriodSize, rep.K, rep.ConfigDigest),
		BodyDigest:     rep.BodyDigest,
		ToolVersion:    rep.ToolVersion,
		Commit:         rep.ToolCommit,
	}
	if m.hasEarliest {
		resp.EarliestPeriod = m.earliest.String()
	}
	if !m.nextSealAt.IsZero() {
		resp.NextSealAt = m.nextSealAt.Format(time.RFC3339)
	}
	resp.SealStallJSON = m.stall
	if cur := h.currentSealConfig(); cur.Digest != resp.Config.Digest {
		resp.ConfigGap = &sealedConfigGapJSON{Current: cur, Note: "this month was sealed under config, which differs " +
			"from the current config: a sealed month keeps the config it was sealed under, and the current " +
			"config applies only to months sealed after it took effect (#913)"}
	}
	writeJSON(w, http.StatusOK, resp)
}
