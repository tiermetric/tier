package opencode

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// NEGATIVE CONTROL 1 — break the providerID map, the guessed-cost counter fires
// ─────────────────────────────────────────────────────────────────────────────

// guessRecorder counts unknown-model pricing fallbacks and the micro-dollars
// billed at them — the two signals internal/store already exposes to `tierd
// serve` as tier_unknown_model_events_total and
// tier_unknown_model_cost_micro_total (#68/#135/#326).
type guessRecorder struct {
	mu     sync.Mutex
	events int
	micro  float64
	paths  []string
}

func (g *guessRecorder) Inc(labels ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events++
	g.paths = append(g.paths, strings.Join(labels, ","))
}

func (g *guessRecorder) Add(v float64, _ ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.micro += v
}

func (g *guessRecorder) snapshot() (int, float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.events, g.micro
}

// installGuessRecorder wires a fresh recorder into the store's pricing path for
// the calling test and clears it afterwards.
func installGuessRecorder(t *testing.T) *guessRecorder {
	t.Helper()
	g := &guessRecorder{}
	store.SetUnknownModelRecorder(g)
	store.SetUnknownModelCostRecorder(g)
	t.Cleanup(func() {
		store.SetUnknownModelRecorder(nil)
		store.SetUnknownModelCostRecorder(nil)
	})
	return g
}

// withProviderPolicy swaps the package's provider allowlist for the calling test
// and restores it afterwards. The map is package-level state, so tests using it
// must not run in parallel.
func withProviderPolicy(t *testing.T, mutate func(m map[string]struct {
	decision providerDecision
	why      string
})) {
	t.Helper()
	saved := make(map[string]struct {
		decision providerDecision
		why      string
	}, len(providerPolicy))
	for k, v := range providerPolicy {
		saved[k] = v
	}
	t.Cleanup(func() { providerPolicy = saved })
	mutate(providerPolicy)
}

// TestNegativeControl_AdmittedProviderServingUnpricedModelFiresTheGuessedCostCounter
// is the two-armed proof that the guessed-cost signal CAN redden on this path:
// an admitted provider serving a model with no price row fires the guessed-cost
// counter.
//
// 🔴 THE CONTROL ARM IS NOT OPTIONAL. "The counter is zero" is the assertion the
// whole done-when condition rests on ("zero guessed-cost share"), and a counter
// that is never wired, never installed, or reading a different global would also
// report zero — forever, silently, exactly like the passes it is supposed to
// certify. Arm 1 shows a correctly-mapped provider produces zero. Arm 2 admits a
// provider to the priced arm and has it serve a model with no price row at all,
// and shows the same counter goes non-zero on the same corpus.
//
// ⚠️ This does NOT catch a mis-admitted provider serving glm-5.3: since #786 the
// model-only glm-5.3 row prices glm-5.3 on any host, so that event prices at the
// audited rate and this counter stays at zero.
func TestNegativeControl_AdmittedProviderServingUnpricedModelFiresTheGuessedCostCounter(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted

	buildDB := func(provider, model string) string {
		return newFixtureDB(t, dbSpec{
			Migrations: 3,
			Rows: []msgRow{buildRow(t, msgSpec{
				ID: "msg_np", SessionID: "ses_np", Provider: provider, Model: model,
				Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
				Completed: &completed, TimeUpdated: completed,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
			})},
		})
	}

	t.Run("control/correctly-mapped-provider-guesses-nothing", func(t *testing.T) {
		g := installGuessRecorder(t)
		events, _ := collectFrom(t, buildDB("zai-coding-plan", ""), repo)
		if len(events) != 1 {
			t.Fatalf("want 1 event, got %d", len(events))
		}
		if n, micro := g.snapshot(); n != 0 || micro != 0 {
			t.Errorf("a correctly-mapped provider must guess NOTHING; got %d guessed events / %.0f guessed micro-dollars", n, micro)
		}
		if events[0].CostMicro != goldenCostMicro {
			t.Errorf("control arm priced at %d micro, want the audited %d", events[0].CostMicro, goldenCostMicro)
		}
	})

	t.Run("mutant/provider-admitted-with-no-price-row", func(t *testing.T) {
		// The built-in model-only glm-5.3 row prices glm-5.3 on ANY host (#786), so the
		// mutant carries a model with no row at all: glm-5.2 (R-2026-09-28-12 adds none).
		const bogus = "zai-coding-plan-v2"
		const unpricedModel = "glm-5.2"
		withProviderPolicy(t, func(m map[string]struct {
			decision providerDecision
			why      string
		}) {
			m[bogus] = struct {
				decision providerDecision
				why      string
			}{providerPriced, "deliberately broken by TestNegativeControl_AdmittedProviderServingUnpricedModelFiresTheGuessedCostCounter"}
		})
		g := installGuessRecorder(t)
		events, _ := collectFrom(t, buildDB(bogus, unpricedModel), repo)
		if len(events) != 1 {
			t.Fatalf("want 1 event, got %d", len(events))
		}
		n, micro := g.snapshot()
		if n == 0 {
			t.Fatalf("the guessed-cost counter did NOT fire for a provider with no price row — the guard cannot redden, so the control arm above proves nothing")
		}
		if micro == 0 {
			t.Errorf("guessed EVENT count is %d but guessed COST is 0; the cost-weighted signal is not wired", n)
		}
		if events[0].CostMicro == goldenCostMicro {
			t.Errorf("the mutant priced identically to the audited rate (%d micro) — the fixture is not exercising the fallback at all", events[0].CostMicro)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// NEGATIVE CONTROL 2 — Codex-shaped (subset) tokens are rejected LOUDLY
// ─────────────────────────────────────────────────────────────────────────────

// TestNegativeControl_CodexShapedTokensAreRejectedLoudly feeds the exact shape a
// Codex rollout log produces — `total == input + output` with reasoning as a
// SUBSET already inside output — and requires it to be refused with a WARN, not
// silently folded in under either convention.
//
// Folding it in silently is the failure that matters. Under Opencode's additive
// mapping the row would bill (output + reasoning) when only `output` was ever
// charged, over-reporting the output class by the whole reasoning count; under
// Codex's mapping every genuine Opencode row loses 84.8% of its output-side
// tokens. There is no mapping that is right for both, so the only honest answer
// to a row that does not sum is to refuse it and say so.
func TestNegativeControl_CodexShapedTokensAreRejectedLoudly(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted

	// The Codex shape: total = input + output; reasoning sits INSIDE output.
	const (
		cxInput     = 800
		cxOutput    = 200
		cxReasoning = 50 // <= output, and already counted within it
		cxTotal     = cxInput + cxOutput
	)
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_codex_shaped", SessionID: "ses_codex",
			Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: tokenSpec{Total: i64(cxTotal), Input: cxInput, Output: cxOutput, Reasoning: cxReasoning},
		})},
	})

	events, cap := collectFrom(t, dbPath, repo)
	if len(events) != 0 {
		t.Fatalf("a Codex-shaped row must produce NO event; got %d (cost %d micro) — it was folded in silently under one convention or the other",
			len(events), events[0].CostMicro)
	}
	warns := cap.find(slog.LevelWarn, "additive identity")
	if len(warns) != 1 {
		t.Fatalf("want exactly one WARN naming the additive identity; got %d. Records:%s", len(warns), cap.dump())
	}
	if got := attrOf(warns[0], "message_id"); !strings.Contains(got, "msg_codex_shaped") {
		t.Errorf("the WARN must name the offending message; message_id = %q", got)
	}
	// The scan summary must be at WARN too, and must carry the counter — a WARN
	// per row is easy to miss in a busy log; the per-pass count is what an
	// operator actually reads.
	summaries := cap.find(slog.LevelWarn, "opencode scan complete")
	if len(summaries) != 1 {
		t.Fatalf("the pass summary must be raised to WARN when rows were rejected; got %d WARN summaries. Records:%s", len(summaries), cap.dump())
	}
	if got := attrOf(summaries[0], "skipped_identity_violation"); got != "1" {
		t.Errorf("skipped_identity_violation = %q, want \"1\"", got)
	}
}

// TestAdditiveIdentityAcceptsWhereTheConventionsAGREE is the companion that keeps
// the check above from being read as stricter than it is.
//
// A Codex-shaped row whose reasoning is ZERO satisfies the additive identity too,
// and is priced identically under either convention — so it is accepted, and
// SHOULD be. Without this arm a future contributor could "strengthen" the check
// into something that rejects ordinary no-reasoning turns.
func TestAdditiveIdentityAcceptsWhereTheConventionsAGREE(t *testing.T) {
	if err := checkAdditiveIdentity(usage{Total: 1000, Input: 800, Output: 200, TotalPresent: true}); err != nil {
		t.Errorf("a row with no reasoning satisfies BOTH conventions and must be accepted: %v", err)
	}
}

// TestAdditiveIdentityTable pins the invariant arms directly, including the
// unmeasured cache-write term and the magnitude ceiling.
func TestAdditiveIdentityTable(t *testing.T) {
	cases := []struct {
		name    string
		u       usage
		wantErr string
	}{
		{"the real measured shape", usage{Total: 26605, Input: 26455, Output: 8, Reasoning: 14, CacheRead: 128, TotalPresent: true}, ""},
		{"reasoning exceeds output (77% of the real corpus)", usage{Total: 300, Input: 100, Output: 20, Reasoning: 180, TotalPresent: true}, ""},
		{"cache write counted inside total", usage{Total: 150, Input: 100, Output: 20, Reasoning: 5, CacheRead: 10, CacheWrite: 15, TotalPresent: true}, ""},
		{"cache write NOT counted inside total", usage{Total: 135, Input: 100, Output: 20, Reasoning: 5, CacheRead: 10, CacheWrite: 15, TotalPresent: true}, "additive identity broken"},
		{"total absent is unchecked here", usage{Input: 100, Output: 20}, ""},
		{"negative count", usage{Total: 100, Input: -1, Output: 101, TotalPresent: true}, "negative token count"},
		{"above the sanity ceiling", usage{Total: maxMessageTokens + 1, Input: maxMessageTokens + 1, TotalPresent: true}, "sanity ceiling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAdditiveIdentity(tc.u)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("want accepted, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("want an error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// COMPLETED-ONLY
// ─────────────────────────────────────────────────────────────────────────────

// TestIncompleteMessagesAreSkippedAndRereadWhenTheyComplete proves both halves of
// the completed-only rule: the partial row produces nothing, and the SAME message
// is captured once its completing UPDATE bumps `time_updated`.
//
// The second half is what makes the first half safe. Skipping a row is only
// correct if something brings it back, and the thing that brings it back is the
// watermark being on `time_updated` — a column the completing UPDATE moves.
func TestIncompleteMessagesAreSkippedAndRereadWhenTheyComplete(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	const startMS = int64(1_787_939_000_000)

	// Pass 1: mid-stream. Partial counts, no time.completed.
	partial := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_stream", SessionID: "ses_stream",
			Cwd: filepath.Join(repo, "src"), Created: startMS,
			Completed: nil, TimeUpdated: startMS + 100,
			Tokens: autoTotal(10, 1, 0, 0, 0),
		})},
	})
	events, _ := collectFrom(t, partial, repo)
	if len(events) != 0 {
		t.Fatalf("an in-flight message must produce NO event (the store freezes cost_micro at the first writer's value, so a partial insert under-prices that message FOREVER); got %d", len(events))
	}

	// Pass 2: the same message id, now completed, with the final counts.
	done := startMS + 5_000
	complete := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_stream", SessionID: "ses_stream",
			Cwd: filepath.Join(repo, "src"), Created: startMS,
			Completed: &done, TimeUpdated: done,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	events2, _ := collectFrom(t, complete, repo)
	if len(events2) != 1 {
		t.Fatalf("the completed message must be captured; got %d events", len(events2))
	}
	if events2[0].CostMicro != goldenCostMicro {
		t.Errorf("CostMicro = %d, want the FULL-count price %d", events2[0].CostMicro, goldenCostMicro)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// IDEMPOTENCY
// ─────────────────────────────────────────────────────────────────────────────

// TestIdempotencyKeyIsSourceNamespacedNotMessageNamespaced pins the key
// construction against the one it must NOT use.
//
// collector.MessageIdempotencyKey's "msg" namespace exists so that ONE upstream
// call captured twice — once by a session log, once by the proxy — collides on
// the store's partial unique index and is stored once. That contract requires
// both producers to key on the SAME upstream response id. Opencode's `message.id`
// is a CLIENT-side row id (`msg_039aea…`), not an upstream response id, so
// putting it in the "msg" namespace would claim a cross-path identity that does
// not exist: it would never collide with a real proxy row, and it would sit in a
// namespace whose whole meaning is "this value is comparable across paths".
//
// ⚠️ THE CONSEQUENCE, stated because it is a real operational hazard and not a
// theoretical one: if Opencode is ALSO pointed at tier's reverse proxy, the same
// call is captured twice under two unrelatable keys — `opencode`+message.id here,
// `msg`+response id there — and the store cannot dedup them. That is double-count,
// and no key choice fixes it; only not doing both does. cmd/tierd warns at startup
// when both are enabled, mirroring the Codex hazard (#459 task 2).
func TestIdempotencyKeyIsSourceNamespacedNotMessageNamespaced(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: goldenMessageID, SessionID: goldenSessionID,
			Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	events, _ := collectFrom(t, dbPath, repo)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}

	want := collector.IdempotencyKey(collector.SourceOpencode, "zai-coding-plan", goldenMessageID)
	if events[0].IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want the source-namespaced key %q", events[0].IdempotencyKey, want)
	}
	if forbidden := collector.MessageIdempotencyKey("zai-coding-plan", goldenMessageID); events[0].IdempotencyKey == forbidden {
		t.Errorf("IdempotencyKey uses the \"msg\" cross-path namespace, which is reserved for UPSTREAM response ids with proxy-dedup semantics; Opencode's message.id is a client-side row id")
	}

	// Re-scanning the same database must produce the identical key, or a re-scan
	// would double-count instead of colliding.
	again, _ := collectFrom(t, dbPath, repo)
	if len(again) != 1 || again[0].IdempotencyKey != events[0].IdempotencyKey {
		t.Errorf("a re-scan produced a different key (%v); re-scans must converge, not duplicate", again)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ATTRIBUTION
// ─────────────────────────────────────────────────────────────────────────────

// TestForeignRepoIsDropped: a message whose cwd is outside every watched repo
// must not be attributed here. Cross-repo bleed puts another project's dollars on
// this project's issues (#15).
func TestForeignRepoIsDropped(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_foreign", SessionID: "ses_foreign",
			Cwd: filepath.Join(t.TempDir(), "somewhere-else"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	events, _ := collectFrom(t, dbPath, repo)
	if len(events) != 0 {
		t.Errorf("a message from outside every watched repo must be dropped; got %d events", len(events))
	}
}

// TestIssueIDIsTheNoBranchBucket: Opencode records no git branch anywhere, so
// every event lands in the labelled unattributed bucket whose documented meaning
// is "a message that recorded no branch at all". It must be a REAL member of the
// family the /events allowlist accepts, or shipped capture would 400 forever.
func TestIssueIDIsTheNoBranchBucket(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_attr", SessionID: "ses_attr",
			Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	events, _ := collectFrom(t, dbPath, repo)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if got := events[0].IssueID; got != collector.UnattributedDetachedHEAD {
		t.Errorf("IssueID = %q, want %q", got, collector.UnattributedDetachedHEAD)
	}
	if !collector.IsUnattributed(events[0].IssueID) {
		t.Errorf("IssueID %q is not recognised by collector.IsUnattributed; the spend would be counted as ATTRIBUTED and inflate coverage", events[0].IssueID)
	}
	// The value must be one the shipping endpoint accepts, checked against the
	// canonical family rather than a literal.
	found := false
	for _, b := range collector.UnattributedBucketIDs {
		if b == events[0].IssueID {
			found = true
		}
	}
	if !found {
		t.Errorf("IssueID %q is not in collector.UnattributedBucketIDs; POST /api/v1/events would reject every shipped Opencode event, permanently", events[0].IssueID)
	}
}

// TestDeveloperFallsBackToOSUsername: an unset DeveloperID uses the same chain
// every other local collector uses, so one machine's Claude Code and Opencode rows
// carry one identity and join the same outcomes.
func TestDeveloperFallsBackToOSUsername(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_dev", SessionID: "ses_dev",
			Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	logger, _ := newTestLogger()
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if want := collector.OSUsername(); events[0].Developer != want {
		t.Errorf("Developer = %q, want the OS username %q", events[0].Developer, want)
	}
}
