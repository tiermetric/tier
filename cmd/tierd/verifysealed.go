package main

// tierd verify-report on a SEALED month's manifest (#913): the month is replayed
// from its stored bytes, never recomputed, over the live path's snapshot, and
// nothing here can seal. docs/reproducibility.md states the checks and codes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

// A sealed manifest's two outcomes the live contract has no word for. Both are
// non-zero, so neither is ever a pass; 0, 1 and 2 keep their live meanings.
const (
	// rcNotSealed: this database holds no sealed row for the manifest's month.
	rcNotSealed = 3
	// rcUnknownFoldRule: the month was sealed by a fold rule this binary does not
	// know or does not refold, so the refold was not run. REPRODUCED always refolds.
	rcUnknownFoldRule = 4
)

// errSealedManifest is loadManifest's answer for a sealed month's manifest.
var errSealedManifest = errors.New("a sealed month's manifest")

// sealedManifest mirrors api.sealedManifestJSON, decode-side, as reportManifest
// mirrors the live manifest.
type sealedManifest struct {
	Schema         string `json:"manifest_schema"`
	Period         string `json:"period"`
	PeriodStart    string `json:"period_start"`
	PeriodEnd      string `json:"period_end"`
	SealedAt       string `json:"sealed_at"`
	NextSealAt     string `json:"next_seal_at"`
	EarliestPeriod string `json:"earliest_period,omitempty"`
	LatestPeriod   string `json:"latest_period"`
	Config         struct {
		Aggregation string `json:"aggregation"`
		PeriodSize  string `json:"period_size"`
		K           int    `json:"k"`
		FoldRule    int    `json:"fold_rule,omitempty"`
		Digest      string `json:"digest"`
	} `json:"config"`
	BodyDigest  string          `json:"body_digest"`
	ToolVersion string          `json:"tool_version"`
	Commit      string          `json:"commit,omitempty"`
	ConfigGap   json.RawMessage `json:"config_gap,omitempty"`
	// StalledPeriod, StallReason and OwedSince name a stalled seal: install
	// markers like next_seal_at, never checked.
	StalledPeriod string `json:"stalled_period,omitempty"`
	StallReason   string `json:"stall_reason,omitempty"`
	OwedSince     string `json:"owed_since,omitempty"`
	// Results is the served /scores body an operator attached, as on the live path.
	Results *manifestResults `json:"results,omitempty"`
}

func loadSealedManifest(path string) (sealedManifest, []string, error) {
	var m sealedManifest
	raw, err := os.ReadFile(path) // #nosec G304 -- an operator-supplied manifest path is the command's whole input
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	if err != nil {
		return m, nil, fmt.Errorf("parse sealed manifest %s: %w", path, err)
	}
	if m.Period == "" || m.BodyDigest == "" || m.Config.Digest == "" {
		return m, nil, fmt.Errorf("sealed manifest %s: period, body_digest and config.digest are required", path)
	}
	unknown, err := unknownFieldsOf(raw, reflect.TypeOf(sealedManifest{}))
	return m, unknown, err
}

// sealedVerifyResult is what printSealedVerify prints; verdict is its exit code
// and its headline. Recompute is information only.
type sealedVerifyResult struct {
	Period                 string
	NotSealed, UnknownRule bool
	Dims                   []verifyDim
	Recompute              string
}

func (r sealedVerifyResult) verdict() int {
	if r.NotSealed {
		return rcNotSealed
	}
	for _, d := range r.Dims {
		if d.Status == dimChanged {
			return rcDiverged
		}
	}
	if r.UnknownRule {
		return rcUnknownFoldRule
	}
	return rcReproduced
}

func runVerifySealed(ctx context.Context, db *store.DB, path string, stdout, stderr io.Writer, pricesOverride bool) int {
	m, unknown, err := loadSealedManifest(path)
	if err == nil {
		var res sealedVerifyResult
		if res, err = verifySealed(ctx, db, m, unknown); err == nil {
			if pricesOverride && res.Recompute != "" {
				res.Recompute += "; --prices changes the live recompute's price-table stamp and can cause a difference even on untouched data"
			}
			printSealedVerify(stdout, res)
			return res.verdict()
		}
	}
	_, _ = fmt.Fprintf(stderr, "verify-report: %v (NOTHING was verified)\n", logsafe.Err(err))
	return rcCannotCheck
}

// sealedField compares one manifest value with the stored one.
func sealedField(name, manifest, stored string) verifyDim {
	if manifest == stored {
		return verifyDim{Name: name, Status: dimUnchanged, Detail: logsafe.Str(stored)}
	}
	return verifyDim{Name: name, Status: dimChanged, Detail: fmt.Sprintf("manifest %s, stored %s",
		logsafe.Str(manifest), logsafe.Str(stored))}
}

// verifySealed checks m against the month db has sealed. It reads only: no
// sealer exists in this process. An error is COULD NOT CHECK.
func verifySealed(ctx context.Context, db *store.DB, m sealedManifest, unknown []string) (sealedVerifyResult, error) {
	res := sealedVerifyResult{Period: m.Period}
	p, err := api.ParsePeriod(m.Period)
	if err != nil {
		return res, fmt.Errorf("manifest period: %w", err)
	}
	start, _ := p.Bounds()
	rep, err := db.SealedReport(ctx, p.Kind.String(), start)
	if errors.Is(err, store.ErrSealedReportNotFound) {
		res.NotSealed = true
		return res, nil
	}
	if err != nil {
		return res, err
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	sum := sha256.Sum256(rep.Body)
	bodyDigest := "sha256:" + hex.EncodeToString(sum[:])
	res.Dims = []verifyDim{
		sealedField("body_digest", m.BodyDigest, bodyDigest),
		{Name: "stored digest", Status: dimUnchanged, Detail: "the stored body hashes to the row's body_digest column"},
		sealedField("period_start", m.PeriodStart, stamp(rep.PeriodStart)),
		sealedField("period_end", m.PeriodEnd, stamp(rep.PeriodEnd)),
		sealedField("sealed_at", m.SealedAt, stamp(rep.SealedAt)),
		sealedField("aggregation", m.Config.Aggregation, rep.Level),
		sealedField("period_size", m.Config.PeriodSize, rep.PeriodSize),
		sealedField("k", strconv.Itoa(m.Config.K), strconv.Itoa(rep.K)),
		sealedField("config digest", m.Config.Digest, rep.ConfigDigest),
		sealedField("tool_version", m.ToolVersion, rep.ToolVersion),
		sealedField("commit", m.Commit, rep.ToolCommit),
	}
	if rep.BodyDigest != bodyDigest {
		res.Dims[1] = verifyDim{Name: "stored digest", Status: dimChanged, Detail: fmt.Sprintf("the row's "+
			"body_digest column is %s, but its body hashes to %s", logsafe.Str(rep.BodyDigest), bodyDigest)}
	}
	rule, known := api.SealedFoldRuleOf(rep.Level, rep.PeriodSize, rep.K, rep.ConfigDigest)
	switch {
	case !known:
		res.UnknownRule = true
		res.Dims = append(res.Dims, verifyDim{Name: "fold_rule", Status: dimNotCheckable, Detail: "the stored " +
			"config digest matches no fold rule this binary knows over the row's aggregation, period_size and k, " +
			"so the refold was not run: a tierd with a higher fold rule sealed it, or those columns no longer " +
			"match the digest"})
	case m.Config.FoldRule == 0:
		res.Dims = append(res.Dims, verifyDim{Name: "fold_rule", Status: dimNotPinned, Detail: "the manifest " +
			"names none (the serving binary did not know rule " + strconv.Itoa(rule) + ")"})
	default:
		res.Dims = append(res.Dims, sealedField("fold_rule", strconv.Itoa(m.Config.FoldRule), strconv.Itoa(rule)))
	}
	if known {
		rollups, persons, err := db.SealedFoldInputs(ctx, rep.ID)
		if err != nil {
			return res, err
		}
		d := verifyDim{Name: "refold", Status: dimUnchanged, Detail: fmt.Sprintf("the stored fold inputs "+
			"(%d labels, %d person rows) refold at k=%d to the stored body's rows", len(rollups), len(persons), rep.K)}
		switch err := api.RefoldSealed(rep, rollups, persons); {
		case errors.Is(err, api.ErrSealedRuleNotRefolded):
			res.UnknownRule = true
			d = verifyDim{Name: "refold", Status: dimNotCheckable, Detail: "sealed under fold rule " +
				strconv.Itoa(rule) + ", which this binary does not refold: only a tierd whose fold rule is " +
				strconv.Itoa(rule) + " refolds it"}
		case err != nil:
			d = verifyDim{Name: "refold", Status: dimChanged, Detail: logsafe.Err(err)}
		}
		res.Dims = append(res.Dims, d)
	}
	d, err := sealedResults(m.Results, rep.Body)
	if err != nil {
		return res, err
	}
	res.Dims = append(res.Dims, d)
	if u, ok := verifyDimUnknownPins(unknown); ok {
		res.Dims = append(res.Dims, u)
	}
	res.Recompute = recomputeSealed(ctx, db, p, rep, bodyDigest)
	// A cancelled run is COULD NOT CHECK, as on the live path, never a verdict.
	return res, ctx.Err()
}

// sealedResults compares an attached served body with the stored one, as JSON
// values; with none attached, the body the reader holds was not compared.
func sealedResults(r *manifestResults, stored []byte) (verifyDim, error) {
	if r == nil || len(r.Scores) == 0 {
		return verifyDim{Name: "results", Status: dimNotPinned, Detail: "no /scores body is attached under " +
			"results.scores, so the body you hold was not compared: compare its sha256 with body_digest"}, nil
	}
	want, err := canonicalJSON(r.Scores)
	if err != nil {
		return verifyDim{}, fmt.Errorf("manifest results.scores is not valid JSON: %w", err)
	}
	got, err := canonicalJSON(stored)
	if err != nil {
		return verifyDim{}, fmt.Errorf("stored sealed body is not valid JSON: %w", err)
	}
	if !bytes.Equal(want, got) {
		return verifyDim{Name: "results", Status: dimChanged, Detail: "the attached results.scores is not the stored body"}, nil
	}
	return verifyDim{Name: "results", Status: dimUnchanged, Detail: "the attached results.scores equals the stored body"}, nil
}

// recomputeSealed computes the month from today's rows under its sealed level
// and k, sealing nothing, and says whether a seal now would differ. It is
// informational: it never changes the verdict.
func recomputeSealed(ctx context.Context, db *store.DB, p api.Period, rep store.SealedReport, stored string) string {
	mode, err := resolveAggregationMode(rep.Level)
	if err != nil {
		return "NOT RUN: " + logsafe.Err(err)
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{}, api.WithCommit(commit))
	h.SetAggregation(mode, rep.K)
	body, err := h.RecomputeSealedBody(ctx, p)
	if err != nil {
		return "NOT RUN: " + logsafe.Err(err)
	}
	sum := sha256.Sum256(body)
	if now := "sha256:" + hex.EncodeToString(sum[:]); now != stored {
		return "a seal computed from today's rows would DIFFER (" + now + ") under the month's sealed level and k, not this install's current config: a row, alias or membership moved after the seal, or this binary computes the month differently"
	}
	return "a seal computed from today's rows would be IDENTICAL under the month's sealed level and k, not this install's current config"
}

func printSealedVerify(w io.Writer, res sealedVerifyResult) {
	_, _ = fmt.Fprintf(w, "verify-report: %s  period %s  (a sealed month, #913)\n", api.SealedManifestSchema, logsafe.Str(res.Period))
	_, _ = fmt.Fprintf(w, "verified by tierd %s  commit %s\n\n", logsafe.Str(version), logsafe.Str(commit))
	switch res.verdict() {
	case rcNotSealed:
		_, _ = fmt.Fprintf(w, "NOT SEALED IN THIS DB: this database holds no sealed month %s. Nothing was compared, "+
			"and nothing was sealed: verify-report never seals. Point --db at the database that served the manifest.\n",
			logsafe.Str(res.Period))
	case rcDiverged:
		_, _ = fmt.Fprintf(w, "DIFFERS: the manifest, the sealed month this database holds, its fold inputs and "+
			"any attached body do not all agree; the CHANGED lines below name each.\n")
	case rcUnknownFoldRule:
		_, _ = fmt.Fprintf(w, "UNKNOWN FOLD RULE: every compared field is unchanged, but the month was sealed by a "+
			"fold rule this binary does not know or does not refold, so the refold was not run.\n")
	default:
		_, _ = fmt.Fprintf(w, "REPRODUCED: the stored body's sha256 equals the manifest's body_digest and the row's "+
			"column, every stored field matches the manifest, and the stored fold inputs refold to the body's teams "+
			"and kanon_suppressed (its other fields rest on the digest alone).\n")
	}
	_, _ = fmt.Fprintln(w)
	for _, d := range res.Dims {
		_, _ = fmt.Fprintf(w, "  %-21s %-11s %s\n", d.Name+":", d.Status.label(), d.Detail)
	}
	if res.Recompute != "" {
		_, _ = fmt.Fprintf(w, "  %-21s %-11s %s\n", "live recompute:", "INFO", res.Recompute)
	}
	_, _ = fmt.Fprintf(w, "\nLIMITS\n"+
		"  - A sealed month is REPLAYED from its stored bytes, never recomputed. The live recompute line is\n"+
		"    informational and never changes the verdict: a sealed month keeps its bytes when rows move.\n"+
		"  - next_seal_at, earliest_period, latest_period, config_gap, stalled_period, stall_reason and\n"+
		"    owed_since describe the install when the manifest was served, not the sealed month, and were\n"+
		"    not checked.\n"+
		"  - Every read ran against a VACUUM INTO snapshot, now deleted, and nothing was sealed in it.\n"+
		"  - This run checked the database, not the /scores body you hold, unless it was attached under\n"+
		"    results.scores: save that body verbatim and compare its sha256 with body_digest.\n")
	for _, d := range res.Dims {
		switch d.Status {
		case dimNotPinned:
			_, _ = fmt.Fprintf(w, "  - NOT PINNED: %s — %s\n", d.Name, d.Detail)
		case dimNotCheckable:
			_, _ = fmt.Fprintf(w, "  - NOT CHECKED: %s — %s\n", d.Name, d.Detail)
		case dimUnchanged, dimChanged:
			// Printed above; nothing to disclose.
		}
	}
}
