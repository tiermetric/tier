package main

// End-to-end tests for `tierd ship --codex-rollout` (#492): a real Codex
// rollout log → runShip → api.Handler + SQLite over httptest → per-source rows
// in the store.
//
// WHY THIS TEST EXISTS. POST /api/v1/events rejected every source but "jsonl",
// so Codex spend could never reach a central tierd. Outcomes arrive by webhook
// regardless of which model did the work, so the failure did not read as
// "Codex is missing" — it read as "Codex work was FREE", which inflates TIER
// for exactly the developers moving onto the cheaper path. Both halves are
// asserted here: the events land, AND they carry non-zero cost.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/codexrollout"
	"github.com/tiermetric/tier/internal/store"
)

// codexFixturePath points at the codexrollout package's testdata rather than a
// copy. That fixture is a VERBATIM capture of a real Codex session and is the
// ground truth its own package's constants are derived from; a second copy here
// would drift silently and this test would then be asserting against a stale
// idea of the wire format.
// cleanSessionBillableCalls is the number of billable calls in the clean
// fixture — its 5th token_count differences to zero and is dropped. Mirrors
// cleanBillableCalls in the codexrollout package's own tests.
const cleanSessionBillableCalls = 4

const codexFixturePath = "../../internal/collector/codexrollout/testdata/rollout-clean-session.jsonl"

// stageCodexRollout writes the real rollout fixture into a Codex-shaped
// sessions tree (<root>/YYYY/MM/DD/rollout-*.jsonl), rewriting the captured cwd
// to repo and filling in a branch, so repo scoping and issue attribution run
// against a temp checkout instead of the machine the fixture came from.
func stageCodexRollout(t *testing.T, repo string) string {
	t.Helper()
	raw, err := os.ReadFile(codexFixturePath)
	if err != nil {
		t.Fatalf("read codex fixture (is it still at %s?): %v", codexFixturePath, err)
	}
	body := string(raw)

	// Read the captured cwd out of session_meta rather than hardcoding it, so a
	// re-captured fixture does not silently stop matching.
	var capturedCWD string
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		var l struct {
			Type    string `json:"type"`
			Payload struct {
				CWD string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &l) == nil && l.Type == "session_meta" && l.Payload.CWD != "" {
			capturedCWD = l.Payload.CWD
			break
		}
	}
	if capturedCWD == "" {
		t.Fatal("codex fixture has no session_meta cwd — it cannot be scoped to a repo")
	}
	body = strings.ReplaceAll(body, capturedCWD, repo)
	// The fixture ran outside a checkout, so Codex wrote an empty git object.
	// Fill it the way Codex does when it has git info, so the session attributes
	// to issue-42 like the Claude Code fixture alongside it.
	const emptyGit = `"git":{}`
	if !strings.Contains(body, emptyGit) {
		t.Fatalf("codex fixture no longer contains %s — the branch fill below would be a silent no-op and the session would attribute to nothing", emptyGit)
	}
	body = strings.ReplaceAll(body, emptyGit, `"git":{"branch":"feature/42-foo"}`)

	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-clean-session.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return root
}

// sourceStarts returns the set of sources that have at least one row in the
// store — the per-source landing signal (#512's SourceCoverageStart), which is
// what "did Codex spend actually arrive" means at the store grain.
func sourceStarts(t *testing.T, db *store.DB) map[string]time.Time {
	t.Helper()
	starts, err := db.SourceCoverageStart(context.Background())
	if err != nil {
		t.Fatalf("SourceCoverageStart: %v", err)
	}
	return starts
}

func totalMicro(t *testing.T, db *store.DB) int64 {
	t.Helper()
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	var sum int64
	for _, c := range costs {
		sum += c.TotalCostMicro
	}
	return sum
}

// TestShipCodexRollout_PartialScanShipsWhatParsed is the guard for this
// change's central design decision: Collect returns the events from the files
// that parsed ALONGSIDE an error naming the files that did not, and the shipper
// must forward the good ones anyway.
//
// Dropping the whole batch because one rollout log is unreadable would zero out
// a laptop's entire Codex spend while its outcomes still arrived — #492's
// failure mode reproduced one layer down, and silently, since the operator sees
// a non-zero exit either way. shipCodexRollout returns an error instead of
// exiting, so both halves are directly assertable here.
func TestShipCodexRollout_PartialScanShipsWhatParsed(t *testing.T) {
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	sessionsDir := stageCodexRollout(t, repo)

	// A second rollout log in the same tree that cannot be read at all. Its
	// mode is restored by t.Cleanup so the temp dir can be removed.
	bad := filepath.Join(sessionsDir, "2026", "07", "23", "rollout-unreadable.jsonl")
	if err := os.WriteFile(bad, []byte(`{"type":"session_meta"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write unreadable fixture: %v", err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	if _, err := os.ReadFile(bad); err == nil {
		t.Skip("running with privileges that ignore file modes (root?) — the unreadable arm cannot be staged")
	}

	var got []collector.TokenEvent
	ing := collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		got = append(got, ev)
		return nil
	})
	err := shipCodexRollout(context.Background(),
		[]codexrollout.RepoTarget{{Path: repo}}, sessionsDir, "alice",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ing)

	// BOTH halves, in this order — the events matter more than the error.
	// The exact count, not just >0: shipping 1 of the good session's 4 billable
	// calls is the same silent under-report in miniature.
	if len(got) != cleanSessionBillableCalls {
		t.Errorf("one unreadable rollout log left %d events shipped, want the %d from the good session. Dropping them silently zeroes a laptop's Codex spend while its outcomes still arrive (#492)", len(got), cleanSessionBillableCalls)
	}
	if err == nil {
		t.Fatal("an unreadable rollout log must be reported, not swallowed — ship would exit 0 on a partial capture")
	}
	if !strings.Contains(err.Error(), "rollout-unreadable.jsonl") {
		t.Errorf("the error must name the offending file so an operator can fix it; got: %v", err)
	}
}

// TestRunShip_CodexRollout is the #492 regression: with --codex-rollout, a real
// Codex session on the laptop reaches a central tierd with its cost attached.
func TestRunShip_CodexRollout(t *testing.T) {
	// The flag defaults to envBool("TIER_CODEX_ROLLOUT"), which the docs tell
	// operators to export. Neutralise it, or this test passes on a build where
	// --codex-rollout is broken and its off-by-default sibling fails blaming the
	// flag for the environment's doing.
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	sessionsDir := stageCodexRollout(t, repo)
	srv, db := newShipTestServer(t, "")

	// #231: the operator's slug override must reach the Codex targets too, or a
	// fork's Codex rows land under a second repo identity and stop joining the
	// outcomes its Claude Code rows join. Without an explicit override both the
	// correct and the broken build produce an empty slug, and the assertion
	// below cannot tell them apart.
	const wantSlug = "tiermetric/tier"
	out := captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--repo-slug", repo + "=" + wantSlug,
			"--claude-dir", claudeDir,
			"--codex-rollout",
			"--codex-sessions-dir", sessionsDir,
			"--since", "2026-01-01",
			"--developer", "alice",
		})
	})

	starts := sourceStarts(t, db)
	if _, ok := starts[collector.SourceCodexRollout]; !ok {
		t.Fatalf("no %q rows in the store after shipping a Codex session — the central path is still closed (#492). Sources present: %v\nship output:\n%s",
			collector.SourceCodexRollout, starts, out)
	}
	// The Claude Code path must still ship alongside it: widening the allowlist
	// must not have traded one source for the other.
	if _, ok := starts[collector.SourceJSONL]; !ok {
		t.Errorf("shipping Codex lost the %q rows; sources present: %v", collector.SourceJSONL, starts)
	}

	// The shipper reports one line per billable call: 1 Claude Code event plus
	// the fixture's 4 billable Codex calls (its 5th token_count differences to
	// zero and is dropped by the cumulative-series decoding).
	if !strings.Contains(out, "Shipped 5 events") {
		t.Errorf("expected 1 Claude + 4 Codex events shipped; got:\n%s", out)
	}

	// THE ACTUAL BUG. Rows landing is not enough — #492's damage was cost
	// reading as ZERO while outcomes counted, so a money assertion is the only
	// one that can see it. The Claude fixture is 10,500 micro-dollars (see
	// TestRunShip_EndToEnd) and the Codex fixture's documented final token
	// classes price, at the table pinned in loadDeterministicPrices, as:
	//
	//	 6,722 fresh input @ $2.50/M     =  16,805 µ$
	//	55,296 cache read  @ $0.25/M     =  13,824 µ$   (input x cache_read_mult 0.10)
	//	 2,211 output      @ $15.00/M    =  33,165 µ$
	//	                                   ─────────
	//	                                    63,794 µ$
	//
	// Asserting the exact sum rather than "> 10500" is deliberate: it also
	// proves the per-call rounding across those 4 events did not drift from the
	// session totals its own package measured.
	const jsonlOnlyMicro = 10500
	const codexMicro = 63794
	if got := totalMicro(t, db); got != jsonlOnlyMicro+codexMicro {
		t.Errorf("total cost = %d micro-dollars, want %d (%d Claude + %d Codex). A total of %d exactly means the Codex session shipped rows but no money — the #492 failure mode in a different disguise",
			got, jsonlOnlyMicro+codexMicro, jsonlOnlyMicro, codexMicro, jsonlOnlyMicro)
	}

	// #231 propagation: every stored row — Claude and Codex alike — carries the
	// operator's slug, so one repo has one identity across both capture paths.
	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 50)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	var codexRows int
	for _, r := range rows {
		if r.Source == collector.SourceCodexRollout {
			codexRows++
		}
		if r.Repo != wantSlug {
			t.Errorf("%s row has repo %q, want %q — the --repo-slug override did not reach this capture path, so its rows join a different repo identity (#231)", r.Source, r.Repo, wantSlug)
		}
	}
	if codexRows == 0 {
		t.Error("no Codex rows in the listing — the slug assertion above never ran against the path it exists to check")
	}
}

// TestRunShip_CodexRolloutOffByDefault is the control arm. Without the flag the
// SAME fixture on disk must produce no Codex rows — otherwise the test above
// would pass on a build that scanned ~/.codex unconditionally, and it would be
// proving nothing about the flag.
//
// Off-by-default is also the contract: the Codex log lives outside the repo and
// under a different tool's control, so reading it is opt-in, exactly as
// `tierd serve --codex-rollout` is.
func TestRunShip_CodexRolloutOffByDefault(t *testing.T) {
	// Without this the test fails on any machine that exported the documented
	// TIER_CODEX_ROLLOUT, blaming the flag for the environment's doing.
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	loadDeterministicPrices(t)
	repo := initGitRepo(t)
	claudeDir := t.TempDir()
	writeSessionFixture(t, claudeDir, repo)
	sessionsDir := stageCodexRollout(t, repo)
	srv, db := newShipTestServer(t, "")

	captureStdout(t, func() {
		runShip([]string{
			"--server", srv.URL,
			"--repo", repo,
			"--claude-dir", claudeDir,
			// no --codex-rollout, but the sessions dir is still pointed at the
			// staged fixture so "found nothing" cannot be confused with
			// "looked somewhere empty".
			"--codex-sessions-dir", sessionsDir,
			"--since", "2026-01-01",
			"--developer", "alice",
		})
	})

	starts := sourceStarts(t, db)
	if _, ok := starts[collector.SourceCodexRollout]; ok {
		t.Errorf("Codex rows landed without --codex-rollout; the flag does not gate the scan. Sources: %v", starts)
	}
	if _, ok := starts[collector.SourceJSONL]; !ok {
		t.Fatalf("the Claude Code path shipped nothing either — this run proves nothing about the flag. Sources: %v", starts)
	}
}
