package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// sealedMay is the month every arm seals: by default an empty month, whose body
// is the one api computes for it and whose empty fold inputs refold to that body.
var sealedMay = time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)

// sealTestDigest is api's sealConfigDigestOf, restated so this package can seal
// a month without a sealer; each arm asserts api.SealedFoldRuleOf knows it.
func sealTestDigest(rule int, level string, k int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "tier-seal-config/v1\x00rule=%d\x00level=%s\x00period_size=month\x00k=%d", rule, level, k))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sealedFixture seals May under config digest into a new database and writes
// the manifest /report_manifest?period=2026-05 serves for it; body is the stored
// body. withheld seals real fold inputs instead of an empty fold: one sub-k label
// whose residual the refold withholds, so the body is its kanon_suppressed.
func sealedFixture(t *testing.T, digest string, withheld bool) (dbPath, manifestPath string, m map[string]any, body []byte) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "sealed.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := api.ParsePeriod("2026-05")
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{}, api.WithCommit(commit))
	h.SetAggregation(scoring.AggregationTeam, 5)
	body, err = h.RecomputeSealedBody(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var rollups []store.SealedRollup
	var persons []store.SealedPerson
	if withheld {
		small := scoring.LabelInput{Label: "small", People: []string{"a", "b"}, Contributes: true,
			Sums: scoring.RollupSums{WeightedPoints: 3, TotalCostUSD: 6, SampleN: 2}}
		small.Has[0], small.Has[1] = true, true
		teams, sup := scoring.AggregateFolded([]scoring.LabelInput{small}, 5)
		if !sup.Any() || teams != nil {
			t.Fatalf("control: the fold must withhold the residual (teams %v, %+v)", teams, sup)
		}
		rollups = []store.SealedRollup{{Label: small.Label, WeightedPoints: 3, TotalCostUSD: 6, SampleN: 2, Has: small.Has, Contributes: true}}
		for _, id := range small.People {
			persons = append(persons, store.SealedPerson{Label: small.Label, Measure: scoring.PeopleSetName, CanonicalID: id})
		}
		body = fmt.Appendf(nil, `{"teams":null,"data_quality":{"kanon_suppressed":{"developers":%d,"k_anonymity":%d,`+
			`"withheld_total":true,"withheld_teams":true}}}`, sup.Developers, sup.K)
	}
	sum := sha256.Sum256(body)
	rep, _, err := db.SealReport(context.Background(), store.SealedReport{
		Level: "team", PeriodSize: "month", PeriodStart: sealedMay, PeriodEnd: sealedMay.AddDate(0, 1, 0),
		K: 5, ConfigDigest: digest, Body: body, BodyDigest: "sha256:" + hex.EncodeToString(sum[:]),
		ToolVersion: "v9.9.9", ToolCommit: "abc123",
	}, rollups, persons, store.SealCheck{Floor: sealedMay})
	if err != nil {
		t.Fatal(err)
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	m = map[string]any{
		"manifest_schema": api.SealedManifestSchema, "period": "2026-05",
		"period_start": stamp(rep.PeriodStart), "period_end": stamp(rep.PeriodEnd), "sealed_at": stamp(rep.SealedAt),
		"next_seal_at": "2026-07-15T00:00:00Z", "earliest_period": "2026-05", "latest_period": "2026-05",
		"config":      map[string]any{"aggregation": "team", "period_size": "month", "k": 5, "fold_rule": 1, "digest": digest},
		"body_digest": rep.BodyDigest, "tool_version": "v9.9.9", "commit": "abc123",
	}
	manifestPath = filepath.Join(dir, "sealed-manifest.json")
	writeSealedManifest(t, manifestPath, m)
	return dbPath, manifestPath, m, body
}

// editSealedRow runs stmt on the source database after dropping trigger, the
// append-only guard that would refuse it: a stored row moved after the seal.
func editSealedRow(trigger, stmt string) func(t *testing.T, dbPath string) {
	return func(t *testing.T, dbPath string) {
		t.Helper()
		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		for _, q := range []string{"DROP TRIGGER " + trigger, stmt} {
			if res, err := raw.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			} else if n, _ := res.RowsAffected(); q == stmt && n != 1 {
				t.Fatalf("%s: %d rows, want 1", q, n)
			}
		}
	}
}

func writeSealedManifest(t *testing.T, path string, m map[string]any) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sealedState is the database file's sha256 and mtime and its sealed row counts.
func sealedState(t *testing.T, dbPath string) string {
	t.Helper()
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := sql.Open("sqlite", readOnlySnapshotDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	var counts []int
	for _, table := range []string{"sealed_report", "sealed_rollup", "sealed_person", "seal_floor"} {
		var n int
		if err := ro.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, n)
	}
	return fmt.Sprintf("%x %s %v", sha256.Sum256(raw), fi.ModTime(), counts)
}

// TestVerifySealed_Outcomes pins each sealed outcome to its exit code and
// headline: 0 REPRODUCED, 1 DIFFERS naming the field, 3 NOT SEALED IN THIS DB,
// 4 UNKNOWN FOLD RULE, 2 COULD NOT CHECK. The database file and its sealed rows
// are unchanged by every run.
func TestVerifySealed_Outcomes(t *testing.T) {
	known := sealTestDigest(1, "team", 5)
	if rule, ok := api.SealedFoldRuleOf("team", "month", 5, known); !ok || rule != 1 {
		t.Fatalf("control: api does not know the test digest (rule %d, %v); sealTestDigest drifted", rule, ok)
	}
	unknown := sealTestDigest(99, "team", 5)
	cfg := func(key string, v any) func(m map[string]any) {
		return func(m map[string]any) { m["config"].(map[string]any)[key] = v }
	}
	top := func(key string, v any) func(m map[string]any) { return func(m map[string]any) { m[key] = v } }
	for name, c := range map[string]struct {
		digest   string
		edit     func(m map[string]any)
		wantRC   int
		headline string
		line     string // a dimension line that must read "<name>: <STATUS>"
		absent   string // text stdout must not carry
		withheld bool   // seal real fold inputs (sealedFixture)
		attach   string // "stored" or "altered": the /scores body under results.scores
		store    func(t *testing.T, dbPath string)
	}{
		"reproduced":         {digest: known, wantRC: rcReproduced, headline: "REPRODUCED:", line: "refold: UNCHANGED"},
		"body digest moved":  {digest: known, edit: top("body_digest", "sha256:00"), wantRC: rcDiverged, headline: "DIFFERS:", line: "body_digest: CHANGED"},
		"period_start moved": {digest: known, edit: top("period_start", "2026-04-01T00:00:00Z"), wantRC: rcDiverged, headline: "DIFFERS:", line: "period_start: CHANGED"},
		"period_end moved":   {digest: known, edit: top("period_end", "2026-07-01T00:00:00Z"), wantRC: rcDiverged, headline: "DIFFERS:", line: "period_end: CHANGED"},
		"sealed_at moved":    {digest: known, edit: top("sealed_at", "2020-01-01T00:00:00Z"), wantRC: rcDiverged, headline: "DIFFERS:", line: "sealed_at: CHANGED"},
		"aggregation moved":  {digest: known, edit: cfg("aggregation", "division"), wantRC: rcDiverged, headline: "DIFFERS:", line: "aggregation: CHANGED"},
		"period_size moved":  {digest: known, edit: cfg("period_size", "week"), wantRC: rcDiverged, headline: "DIFFERS:", line: "period_size: CHANGED"},
		"k moved":            {digest: known, edit: cfg("k", 6), wantRC: rcDiverged, headline: "DIFFERS:", line: "k: CHANGED"},
		"config digest moved": {digest: known, edit: cfg("digest", "sha256:ff"), wantRC: rcDiverged, headline: "DIFFERS:",
			line: "config digest: CHANGED"},
		"tool_version moved": {digest: known, edit: top("tool_version", "v0.0.1"), wantRC: rcDiverged, headline: "DIFFERS:", line: "tool_version: CHANGED"},
		"commit moved":       {digest: known, edit: top("commit", "def456"), wantRC: rcDiverged, headline: "DIFFERS:", line: "commit: CHANGED"},
		"fold_rule moved":    {digest: known, edit: cfg("fold_rule", 2), wantRC: rcDiverged, headline: "DIFFERS:", line: "fold_rule: CHANGED"},
		"fold_rule absent": {digest: known, edit: func(m map[string]any) { delete(m["config"].(map[string]any), "fold_rule") },
			wantRC: rcReproduced, headline: "REPRODUCED:", line: "fold_rule: NOT PINNED"},
		"stored digest column moved": {digest: known, wantRC: rcDiverged, headline: "DIFFERS:", line: "stored digest: CHANGED",
			store: editSealedRow("trg_sealed_report_no_update", `UPDATE sealed_report SET body_digest = 'sha256:00'`)},
		"inputs refold": {digest: known, withheld: true, wantRC: rcReproduced, headline: "REPRODUCED:", line: "refold: UNCHANGED"},
		"a rollup moved": {digest: known, withheld: true, wantRC: rcDiverged, headline: "DIFFERS:", line: "refold: CHANGED",
			store: editSealedRow("trg_sealed_rollup_no_update", `UPDATE sealed_rollup SET contributes = 0`)},
		"stored body attached":  {digest: known, attach: "stored", wantRC: rcReproduced, headline: "REPRODUCED:", line: "results: UNCHANGED"},
		"altered body attached": {digest: known, attach: "altered", wantRC: rcDiverged, headline: "DIFFERS:", line: "results: CHANGED"},
		"no body attached":      {digest: known, wantRC: rcReproduced, headline: "REPRODUCED:", line: "results: NOT PINNED"},
		"not sealed here":       {digest: known, edit: top("period", "2026-04"), wantRC: rcNotSealed, headline: "NOT SEALED IN THIS DB:"},
		"unknown fold rule":     {digest: unknown, wantRC: rcUnknownFoldRule, headline: "UNKNOWN FOLD RULE:", line: "fold_rule: NOT CHECKED"},
		"changed beats unknown rule": {digest: unknown, edit: top("sealed_at", "2020-01-01T00:00:00Z"), wantRC: rcDiverged,
			headline: "DIFFERS:", line: "sealed_at: CHANGED"},
		"unknown pin": {digest: known, edit: top("new_pin", 1), wantRC: rcReproduced, headline: "REPRODUCED:", line: "unknown pins: NOT CHECKED"},
		// #913-D8: a stalled server's manifest carries the stall and no next_seal_at.
		"stall markers": {digest: known, wantRC: rcReproduced, headline: "REPRODUCED:", absent: "unknown pins", edit: func(m map[string]any) {
			delete(m, "next_seal_at")
			m["stalled_period"], m["stall_reason"], m["owed_since"] = "2026-06", "refold_mismatch", "2026-07-15T00:00:00Z"
		}},
		"bad period": {digest: known, edit: top("period", "May"), wantRC: rcCannotCheck},
	} {
		t.Run(name, func(t *testing.T) {
			dbPath, path, m, body := sealedFixture(t, c.digest, c.withheld)
			if c.edit != nil {
				c.edit(m)
			}
			switch c.attach {
			case "stored":
				m["results"] = map[string]any{"scores": json.RawMessage(body)}
			case "altered":
				m["results"] = map[string]any{"scores": map[string]any{"teams": []any{map[string]any{"team": "TAMPERED", "tier": 999}}}}
			}
			writeSealedManifest(t, path, m)
			if c.store != nil {
				c.store(t, dbPath)
			}
			before := sealedState(t, dbPath)
			code, out, errb := runVerify(t, path, "--db", dbPath)
			if code != c.wantRC {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, c.wantRC, out, errb)
			}
			if c.headline != "" && !strings.Contains(out, "\n"+c.headline) {
				t.Errorf("stdout lacks the %q headline:\n%s", c.headline, out)
			}
			if c.line != "" {
				dim, status, _ := strings.Cut(c.line, ": ")
				assertDimStatus(t, out, dim, status)
			}
			if c.absent != "" && strings.Contains(out, c.absent) {
				t.Errorf("stdout carries %q:\n%s", c.absent, out)
			}
			if c.wantRC == rcCannotCheck && (out != "" || !strings.Contains(errb, "NOTHING was verified")) {
				t.Errorf("could not check: stdout %q stderr %q", out, errb)
			}
			if after := sealedState(t, dbPath); after != before {
				t.Errorf("the database changed under verify-report:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// TestVerifySealed_ReproducedLinesAreChecked: a reproduced sealed month reports
// every compared field UNCHANGED, and the live recompute as information that a
// row landing in the month after its seal moves without moving the verdict.
func TestVerifySealed_ReproducedLinesAreChecked(t *testing.T) {
	dbPath, path, _, _ := sealedFixture(t, sealTestDigest(1, "team", 5), false)
	for _, c := range []struct{ name, recompute string }{
		{"as sealed", "would be IDENTICAL"},
		{"a row landed after the seal", "would DIFFER"},
	} {
		if c.name != "as sealed" {
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
				Developer: "alice", IssueID: "issue-1", Repo: "acme/tier", Model: "claude-sonnet-4", InputTok: 10_000,
				CostMicro: store.DollarsToMicro(3), Source: "jsonl", Fidelity: "realtime", PriceVersion: 9,
				Timestamp: sealedMay.AddDate(0, 0, 10),
			}); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
		}
		code, out, errb := runVerify(t, path, "--db", dbPath)
		if code != rcReproduced {
			t.Fatalf("%s: exit %d: %s %s", c.name, code, out, errb)
		}
		for _, dim := range []string{"body_digest", "stored digest", "period_start", "period_end", "sealed_at",
			"aggregation", "period_size", "k", "config digest", "tool_version", "commit", "fold_rule", "refold"} {
			assertDimStatus(t, out, dim, "UNCHANGED")
		}
		if !strings.Contains(out, "\n  live recompute:       INFO        a seal computed from today's rows "+c.recompute) {
			t.Errorf("%s: the live recompute line does not say %q:\n%s", c.name, c.recompute, out)
		}
	}
}
