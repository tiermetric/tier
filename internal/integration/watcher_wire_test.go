//go:build integration

// This file adds the flagship-wire end-to-end test that the rest of the
// integration suite omits: a real Claude Code JSONL session file, tailed live
// by the fsnotify watcher, priced from the embedded table, deduped and
// persisted to a real on-disk SQLite store, and finally read back over HTTP
// through GET /api/v1/scores. The existing integration tests inject costs via
// POST /api/v1/costs, so the watcher->store leg and the JSONL->pricing leg had
// zero end-to-end coverage before this (#148); #96 (current-format sessions
// silently recording $0) shipped precisely because nothing replayed a real
// session through the real wire.
package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

// quietWatcherLogger discards the watcher's diagnostic output so a passing run
// stays clean under `go test -v`. The commit-join git-log WARN (the temp repo
// has .git but no commits — branch attribution is the intended fallback) is
// expected and asserted-nothing-about, per the spec.
func quietWatcherLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Fixture usage constants — copied verbatim from the single assistant turn in
// testdata/seam-jsonl-ingestion/sample.jsonl (message.id
// "msg_012XdSi3hSntA2dxK9MvjeJX"): the definitive post-stream entry's
// message.usage block. The nested cache_creation object routes the 38698
// creation tokens to the 1h bucket (ephemeral_1h_input_tokens), with 0 in the
// 5m bucket — the parser treats the nested object as authoritative
// (jsonl.go TTL split). When the seam sample is re-captured these constants
// change — that coupling is deliberate freshness enforcement.
const (
	fixtureModel        = "claude-opus-4-7"
	fixtureInput        = 6
	fixtureOutput       = 125
	fixtureCacheRead    = 0
	fixtureCacheWrite5m = 0
	fixtureCacheWrite1h = 38698

	// fixtureDeveloper labels every event the watcher emits under this rig.
	fixtureDeveloper = "wire-dev"
	// The fixture branch feature/0-seam-sample attributes to issue-0 via
	// issueref.FromBranch (numeric-segment rule); with .git present but no
	// commits, the commit-join yields nothing and the branch fallback applies.
	repoPlaceholder = "__TIER_REPO__"
)

// fixtureUsage is the CostUsage the collector reconstructs from the fixture's
// message.usage block — declared once so both the cost pin and the
// priced-from-table proof compute against identical inputs.
func fixtureUsage() store.CostUsage {
	return store.CostUsage{
		Input:        fixtureInput,
		Output:       fixtureOutput,
		CacheRead:    fixtureCacheRead,
		CacheWrite5m: fixtureCacheWrite5m,
		CacheWrite1h: fixtureCacheWrite1h,
	}
}

// bootWatcherWire runs an already-constructed Watcher in a goroutine and
// registers cleanup that cancels it and waits for a clean shutdown. Copied
// from internal/collector/watcher_test.go's bootWatcher — the package boundary
// (collector's helper is unexported) prevents direct reuse. The 150ms startup
// wait is the one sanctioned sleep: it gives Run() a moment to register its
// fsnotify watches before the test writes files, since the watcher only
// ingests files that receive events after Run starts (watcher.go). 50ms flaked
// under load on Linux; 150ms is loose enough for busy runners.
func bootWatcherWire(t *testing.T, w *collector.Watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("watcher did not exit within 2s of ctx cancel")
		}
	})
	time.Sleep(150 * time.Millisecond)
}

// waitFor polls cond every interval until it returns true or the deadline
// expires, returning cond's final value. The house pattern for "no sleeps for
// synchronization" — modelled on waitForEvents in
// internal/collector/watcher_test.go, adapted to poll an arbitrary predicate
// against the real store instead of a recording fake.
func waitFor(cond func() bool, deadline, interval time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(interval)
	}
	return cond()
}

// longAgo is a since-bound early enough to include every event the wire tests
// produce, without depending on wall-clock.
var longAgo = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// developerCostMicro returns the persisted micro-dollar total for developer, or
// (0,false) when no row exists yet. Money is compared at the store layer in
// integer micro-dollars — never as floats.
func developerCostMicro(t *testing.T, db *store.DB, developer string) (int64, bool) {
	t.Helper()
	costs, err := db.DeveloperCosts(context.Background(), longAgo)
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	for _, c := range costs {
		if c.Developer == developer {
			return c.TotalCostMicro, true
		}
	}
	return 0, false
}

// waitForCheckpoint polls the real store until a watcher checkpoint for path is
// persisted, or the deadline expires. The watcher saves a checkpoint AFTER
// process() has parsed the file AND issued its InsertTokenEvent calls
// (watcher.go: process inserts, then saveCheckpoint runs), so an appearing
// checkpoint is a deterministic positive control that the file was fully
// processed — including for a foreign-repo session, which still checkpoints its
// advanced offset even though it emits no events. This turns the negative
// assertions ("no cost row appeared") from timing-dependent into deterministic.
func waitForCheckpoint(t *testing.T, db *store.DB, path string, deadline, interval time.Duration) bool {
	t.Helper()
	return waitFor(func() bool {
		cps, err := db.LoadWatcherCheckpoints(context.Background())
		if err != nil {
			t.Fatalf("LoadWatcherCheckpoints: %v", err)
		}
		for _, cp := range cps {
			if cp.Path == path {
				return true
			}
		}
		return false
	}, deadline, interval)
}

// loadFixture reads the captured real session fixture and substitutes the repo
// placeholder for repoPath, mirroring the sed step in scripts/seam-exercise.sh.
// A missing fixture must FAIL, not skip — the wire test is meaningless without
// the real capture.
func loadFixture(t *testing.T, repoPath string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "seam-jsonl-ingestion", "sample.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return []byte(strings.ReplaceAll(string(raw), repoPlaceholder, repoPath))
}

// devByName returns the scores entry for developer, failing the test when it
// is absent. The existing alice() helper hard-codes "alice"; the wire tests
// need an arbitrary developer name.
func devByName(t *testing.T, sr scoresResponse, developer string) devScore {
	t.Helper()
	for _, d := range sr.Developers {
		if d.Developer == developer {
			return d
		}
	}
	t.Fatalf("developer %q not in scores response: %+v", developer, sr)
	return devScore{}
}

// wireRig builds the shared arrangement: a temp git repo (`.git` present, no
// commits), a temp claudeDir with a projects subdir the watcher will tail, and
// the real HTTP composition over an on-disk store. It returns the store, its
// serving URL, the repo path, and the session-file path the caller writes
// AFTER booting the watcher.
func wireRig(t *testing.T) (db *store.DB, srvURL, repo, claudeDir, sessionPath string) {
	t.Helper()
	repo = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	claudeDir = t.TempDir()
	projectsDir := filepath.Join(claudeDir, "projects", "wire-test")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	srv, database := newServerWithStore(t)
	sessionPath = filepath.Join(projectsDir, "session.jsonl")
	return database, srv.URL, repo, claudeDir, sessionPath
}

// TestWire_JSONLFileToWatcherToStoreToScores exercises TIER's flagship data
// path end to end: a real Claude Code JSONL session file -> live fsnotify
// watcher -> table pricing -> dedup -> real SQLite -> GET /api/v1/scores. It is
// the only test that would catch a regression in the watcher->store leg or the
// JSONL->pricing leg together, which is why it gates the P0-07 / P0-11
// collector-semantics changes.
func TestWire_JSONLFileToWatcherToStoreToScores(t *testing.T) {
	db, srvURL, repo, claudeDir, sessionPath := wireRig(t)

	// Expected cost, computed from first principles through the SAME pricing
	// function the collector uses, so the pin tracks any table/formula change.
	expectedMicro := store.ComputeCost(fixtureModel, fixtureUsage())
	if expectedMicro <= 0 {
		t.Fatalf("fixture priced to %d micro-dollars; want > 0 (the #96 tripwire)", expectedMicro)
	}

	// Priced-from-table proof: an unknown model falls back to the
	// self-hosted-medium combined rate. If the fixture's model were silently
	// falling through that path, these two totals would match — they must not.
	// claude-opus-4-7 is an explicit $5/$25-per-M row in internal/store/prices.yaml.
	fallbackMicro := store.ComputeCost("model-that-does-not-exist-xyz", fixtureUsage())
	if expectedMicro == fallbackMicro {
		t.Fatalf("fixture cost %d equals the self-hosted-medium fallback %d — model priced from fallback, not the table", expectedMicro, fallbackMicro)
	}

	// Act: boot the real watcher, THEN write the session file — the watcher
	// only ingests files that receive fsnotify events after Run starts.
	bootWatcherWire(t, &collector.Watcher{
		Repos:         []string{repo},
		Ingester:      ingester.Store(db),
		Checkpoints:   db,
		DeveloperID:   fixtureDeveloper,
		ClaudeDir:     claudeDir,
		DebounceDelay: 50 * time.Millisecond,
		Logger:        quietWatcherLogger(),
	})
	if err := os.WriteFile(sessionPath, loadFixture(t, repo), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// Await: poll the real store until the developer's cost row lands.
	if !waitFor(func() bool {
		_, ok := developerCostMicro(t, db, fixtureDeveloper)
		return ok
	}, 5*time.Second, 10*time.Millisecond) {
		t.Fatalf("no cost row for %q within 5s — session did not flow through the wire", fixtureDeveloper)
	}

	// Exact cost pin at the store layer, in integer micro-dollars.
	gotMicro, _ := developerCostMicro(t, db, fixtureDeveloper)
	if gotMicro != expectedMicro {
		t.Fatalf("persisted cost = %d micro-dollars, want %d", gotMicro, expectedMicro)
	}

	// Assert over HTTP — the wire's public end.
	sr := getScores(t, srvURL)
	dev := devByName(t, sr, fixtureDeveloper)
	if dev.TotalCostUSD <= 0 {
		t.Fatalf("scores total_cost_usd = %v, want > 0 (the #96 tripwire)", dev.TotalCostUSD)
	}
	// Float comparison only against MicroToDollars of the SAME micro value the
	// store returned — the scores handler derives the field the same way, so
	// this is an exact equality, not an epsilon comparison.
	if want := store.MicroToDollars(expectedMicro); dev.TotalCostUSD != want {
		t.Fatalf("scores total_cost_usd = %v, want %v", dev.TotalCostUSD, want)
	}

	// Idempotency leg: replay the SAME message through a SECOND session file.
	// A distinct path is deliberate — an in-place rewrite of session.jsonl would
	// resume from the cached EOF offset and re-parse zero bytes, so the store's
	// dedup index would never be reached (the assertion would pass one layer
	// above the contract it names). session2.jsonl parses from byte 0, so the
	// watcher re-emits the identical MessageIdempotencyKey (the key is derived
	// from message.id, not the file path) and ONLY the store's
	// ON CONFLICT(idempotency_key) UPSERT — the #18/#19 contract on the real
	// partial index — can keep the total from doubling.
	session2 := filepath.Join(filepath.Dir(sessionPath), "session2.jsonl")
	if err := os.WriteFile(session2, loadFixture(t, repo), 0o644); err != nil {
		t.Fatalf("write session2: %v", err)
	}
	// Positive control: the second file's checkpoint is saved only after its
	// InsertTokenEvent (the duplicate) has already been attempted, so waiting
	// for it makes the "total unchanged" assertion deterministic rather than a
	// timing-dependent negative window.
	if !waitForCheckpoint(t, db, session2, 5*time.Second, 10*time.Millisecond) {
		t.Fatalf("second session file was not processed within 5s")
	}
	gotMicro, _ = developerCostMicro(t, db, fixtureDeveloper)
	if gotMicro != expectedMicro {
		t.Fatalf("cost after replaying message.id through a second file = %d micro-dollars, want %d (store dedup on idempotency key regressed — the duplicate was double-counted)", gotMicro, expectedMicro)
	}
}

// TestWire_ForeignRepoSessionNotIngested pins matchTarget on the live wire: a
// session whose cwd is OUTSIDE every configured --watch-repo must not produce a
// TokenEvent. This keeps P0-11's future worktree-capture change honest — when
// worktree CWDs start resolving to the main repo, THIS test defines what still
// must not be captured (a genuinely foreign repo).
func TestWire_ForeignRepoSessionNotIngested(t *testing.T) {
	db, _, repo, claudeDir, sessionPath := wireRig(t)

	// The session's cwd points at a DIFFERENT temp dir, not `repo`.
	foreign := t.TempDir()

	bootWatcherWire(t, &collector.Watcher{
		Repos:         []string{repo},
		Ingester:      ingester.Store(db),
		Checkpoints:   db,
		DeveloperID:   fixtureDeveloper,
		ClaudeDir:     claudeDir,
		DebounceDelay: 50 * time.Millisecond,
		Logger:        quietWatcherLogger(),
	})
	if err := os.WriteFile(sessionPath, loadFixture(t, foreign), 0o644); err != nil {
		t.Fatalf("write foreign session: %v", err)
	}

	// Positive control: a foreign session still checkpoints its advanced offset
	// (process() returns ok=true for an unmatched cwd), so waiting for that
	// checkpoint proves the watcher actually parsed and evaluated the file —
	// without it, this test could pass vacuously if the file were simply never
	// watched. Once the checkpoint lands, the matchTarget decision has been made.
	if !waitForCheckpoint(t, db, sessionPath, 5*time.Second, 10*time.Millisecond) {
		t.Fatalf("foreign session was not processed within 5s — cannot conclude it was excluded rather than missed")
	}
	// The decision has been made and it must be exclusion: no cost row exists.
	if _, ok := developerCostMicro(t, db, fixtureDeveloper); ok {
		t.Fatalf("foreign-repo session was ingested — matchTarget leaked a cwd outside every watch-repo")
	}
}
