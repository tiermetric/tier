package opencode

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// SCHEMA DRIFT MUST BE LOUD, NEVER AN EMPTY SUCCESSFUL SCAN
// ─────────────────────────────────────────────────────────────────────────────

// TestSchemaDriftIsLoud walks the ways an Opencode upgrade can move the ground
// under this collector, and requires each one to fail with an error that NAMES
// what it could not find.
//
// 🔴 THE FAILURE THIS PREVENTS IS A CLEAN LOG. Every case below produces zero
// events. Zero events is also what a healthy, up-to-date collector produces, so
// without these probes an operator gets an identical quiet INFO line every five
// minutes while their spend stops being captured — and the numbers do not look
// broken, they look small.
func TestSchemaDriftIsLoud(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	goodRow := buildRow(t, msgSpec{
		ID: "msg_ok", SessionID: "ses_ok",
		Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
		Completed: &completed, TimeUpdated: completed,
		Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
	})

	cases := []struct {
		name    string
		spec    dbSpec
		wantErr string
	}{
		{
			name:    "Opencode's migration ledger is gone",
			spec:    dbSpec{NoMigrationTable: true, Rows: []msgRow{goodRow}},
			wantErr: "`migration` table",
		},
		{
			name:    "the migration ledger is present but empty",
			spec:    dbSpec{Migrations: 0, Rows: []msgRow{goodRow}},
			wantErr: "EMPTY",
		},
		{
			name: "the message table lost a column this collector reads",
			spec: dbSpec{
				Migrations: 3,
				MessageDDL: "CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL, `data` text NOT NULL)",
			},
			wantErr: "(id, session_id, time_updated, data)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := newFixtureDB(t, tc.spec)
			logger, _ := newTestLogger()
			c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			events, err := c.Collect(context.Background(), time.Time{})
			if err == nil {
				t.Fatalf("want a LOUD error; got a clean scan returning %d events — this is exactly the silent failure the probes exist for", len(events))
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v\nwant it to name %q so an operator can act on it", err, tc.wantErr)
			}
			if IsDisabled(err) {
				t.Errorf("a drifted schema must not be reported as the benign \"Opencode is not installed\" case")
			}
		})
	}
}

// TestZeroEventsFromANonEmptyStoreIsAnERROR is the hardest of the drift arms and
// the one an implementation is most likely to miss: the tables are all present,
// the columns are all present, the rows are all there — and the JSON inside them
// no longer carries the fields we read.
//
// Nothing in a per-row parse can raise this, because every row individually
// "just" fails to match. It takes an independent count of what is IN the store,
// compared against what the scan matched, which is what scanHealth.totalMessages
// exists for.
func TestZeroEventsFromANonEmptyStoreIsAnERROR(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)

	t.Run("role labelling changed", func(t *testing.T) {
		dbPath := newFixtureDB(t, dbSpec{
			Migrations: 3,
			Rows: []msgRow{
				{ID: "m1", SessionID: "s1", TimeUpdated: 1_787_939_000_000, Data: `{"role":"model","modelID":"glm-5.3","providerID":"zai-coding-plan"}`},
				{ID: "m2", SessionID: "s1", TimeUpdated: 1_787_939_000_001, Data: `{"role":"model","modelID":"glm-5.3","providerID":"zai-coding-plan"}`},
			},
		})
		logger, cap := newTestLogger()
		c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if got := cap.find(slog.LevelError, "NO assistant messages"); len(got) != 1 {
			t.Errorf("a non-empty store with no assistant rows must ERROR; got %d such records. Records:%s", len(got), cap.dump())
		}
	})

	t.Run("time_updated stopped meaning what we think", func(t *testing.T) {
		// Rows exist, but every one carries time_updated = -1, so a `>= 0` read
		// matches nothing. That is the shape of "the column changed units or
		// semantics", and it must not read as a clean scan.
		dbPath := newFixtureDB(t, dbSpec{
			Migrations: 3,
			Rows: []msgRow{
				{ID: "m1", SessionID: "s1", TimeUpdated: -1, Data: `{"role":"assistant"}`},
				{ID: "m2", SessionID: "s1", TimeUpdated: -2, Data: `{"role":"assistant"}`},
			},
		})
		logger, cap := newTestLogger()
		c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if got := cap.find(slog.LevelError, "matched zero rows"); len(got) != 1 {
			t.Errorf("a full backfill of a non-empty message table that matched zero rows must ERROR; got %d. Records:%s", len(got), cap.dump())
		}
	})

	t.Run("CONTROL: a healthy up-to-date scan is quiet", func(t *testing.T) {
		// The control arm for the two above. If a healthy scan ALSO errored, the
		// probes would be noise an operator learns to ignore — which is the same
		// outcome as having no probe at all.
		completed := goldenCompleted
		dbPath := newFixtureDB(t, dbSpec{
			Migrations: 3,
			Rows: []msgRow{buildRow(t, msgSpec{
				ID: "msg_ok", SessionID: "ses_ok",
				Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
				Completed: &completed, TimeUpdated: completed,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
			})},
		})
		logger, cap := newTestLogger()
		c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		events, err := c.Collect(context.Background(), time.Time{})
		if err != nil || len(events) != 1 {
			t.Fatalf("healthy scan: %d events, err %v", len(events), err)
		}
		if got := cap.find(slog.LevelError, ""); len(got) != 0 {
			t.Errorf("a healthy scan must log NO errors; got %d. Records:%s", len(got), cap.dump())
		}
	})
}

// TestMigrationDriftIsReported: Opencode applying a migration between two passes
// is the moment this collector's field mapping stopped being verified. It is a
// WARN, not a failure — the mapping usually still holds — but it must be visible.
func TestMigrationDriftIsReported(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	logger, cap := newTestLogger()
	c, err := New(Config{DBPath: filepath.Join(t.TempDir(), "x.db"), Repos: []RepoTarget{{Path: repo}}, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.noteMigrationDrift(38) // first observation: nothing to compare against
	if got := cap.find(slog.LevelWarn, "applied schema migrations"); len(got) != 0 {
		t.Fatalf("the FIRST observation must not warn (there is no previous value); got %d", len(got))
	}
	c.noteMigrationDrift(38) // unchanged
	if got := cap.find(slog.LevelWarn, "applied schema migrations"); len(got) != 0 {
		t.Fatalf("an unchanged ledger must not warn; got %d", len(got))
	}
	c.noteMigrationDrift(39) // Opencode upgraded
	got := cap.find(slog.LevelWarn, "applied schema migrations")
	if len(got) != 1 {
		t.Fatalf("a changed migration ledger must WARN exactly once; got %d. Records:%s", len(got), cap.dump())
	}
	if attrOf(got[0], "migrations_before") != "38" || attrOf(got[0], "migrations_now") != "39" {
		t.Errorf("the WARN must carry both counts; got before=%q now=%q", attrOf(got[0], "migrations_before"), attrOf(got[0], "migrations_now"))
	}
}

// TestAbsentDatabaseIsDisabledNotAnError: Opencode may simply never have run here.
// That is a clean, named INFO — not an error that would also drag down a `serve`
// whose Claude Code capture is working fine.
func TestAbsentDatabaseIsDisabledNotAnError(t *testing.T) {
	repo := repoDir(t)
	logger, _ := newTestLogger()
	c, err := New(Config{DBPath: filepath.Join(t.TempDir(), "nope.db"), Repos: []RepoTarget{{Path: repo}}, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := c.Collect(context.Background(), time.Time{})
	if len(events) != 0 {
		t.Errorf("want no events, got %d", len(events))
	}
	if !IsDisabled(err) {
		t.Fatalf("an absent database must be reported as the disabled condition; got %v", err)
	}

	// And through Run, it must be an INFO once — not an ERROR every tick.
	logger2, cap := newTestLogger()
	c2, err := New(Config{DBPath: filepath.Join(t.TempDir(), "nope.db"), Repos: []RepoTarget{{Path: repo}}, Logger: logger2, Interval: MinScanInterval})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c2.runPass(context.Background(), time.Time{}, collector.IngesterFunc(func(context.Context, collector.TokenEvent) error { return nil }))
	c2.runPass(context.Background(), time.Time{}, collector.IngesterFunc(func(context.Context, collector.TokenEvent) error { return nil }))
	if got := cap.find(slog.LevelError, ""); len(got) != 0 {
		t.Errorf("an absent database must never log at ERROR; got %d. Records:%s", len(got), cap.dump())
	}
	if got := cap.find(slog.LevelInfo, "collector disabled"); len(got) != 1 {
		t.Errorf("want exactly one INFO across two passes; got %d. Records:%s", len(got), cap.dump())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE WATERMARK
// ─────────────────────────────────────────────────────────────────────────────

// TestWatermarkIsTimeUpdatedNotMessageID is the measurement that killed the
// obvious implementation, expressed as a test.
//
// Opencode's message ids are NOT monotonic. Measured on the real store
// 2026-08-28: the lexicographic maximum id (`msg_fc96fb406001…`) belongs to a row
// written 2026-08-03, while the newest row (2026-08-28) sorts far below it. The
// fixture reproduces exactly that relationship — a lexicographically LARGE id on
// the OLD row and a small one on the new — so an implementation that resumed from
// max(id) would skip the new row and report a clean scan.
func TestWatermarkIsTimeUpdatedNotMessageID(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")

	const oldMS = int64(1_785_790_977_442) // 2026-08-03, as measured
	const newMS = int64(1_787_956_602_046) // 2026-08-28, as measured
	oldDone, newDone := oldMS, newMS

	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{
			buildRow(t, msgSpec{ // OLD row, lexicographically LARGEST id
				ID: "msg_fc96fb406001m9N8KJWx9JSRp8", SessionID: "ses_old", Cwd: cwd,
				Created: oldMS - 1000, Completed: &oldDone, TimeUpdated: oldMS,
				Tokens: autoTotal(100, 10, 5, 50, 0),
			}),
			buildRow(t, msgSpec{ // NEW row, lexicographically SMALL id
				ID: "msg_04a839b9c0014J5GLiZH5ItWKK", SessionID: "ses_new", Cwd: cwd,
				Created: newMS - 1000, Completed: &newDone, TimeUpdated: newMS,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
			}),
		},
	})

	// Sanity: the fixture really does invert id order against time order, or the
	// test below would prove nothing.
	if strings.Compare("msg_fc96fb406001m9N8KJWx9JSRp8", "msg_04a839b9c0014J5GLiZH5ItWKK") <= 0 || oldMS >= newMS {
		t.Fatal("the fixture no longer reproduces the non-monotonic id relationship it exists to model")
	}

	logger, _ := newTestLogger()
	cp := &memCheckpoints{}
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger, Checkpoints: cp, Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var got []collector.TokenEvent
	sink := collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		got = append(got, ev)
		return nil
	})
	c.loadWatermark(context.Background())
	c.runPass(context.Background(), time.Time{}, sink)
	if len(got) != 2 {
		t.Fatalf("first pass must capture BOTH rows; got %d", len(got))
	}
	if c.watermark != newMS {
		t.Errorf("watermark = %d, want the newest time_updated %d. If this is %d the collector resumed from the wrong row entirely",
			c.watermark, newMS, oldMS)
	}

	// A second pass must not re-scan history.
	//
	// ⚠️ IT DOES NOT EMIT NOTHING, and asserting that it did would be asserting a
	// bug. The floor is `watermark - 2*interval`, so the newest rows stay inside
	// the deliberate overlap window and are re-read every pass — re-reads are
	// no-ops at the store (the idempotency key collides and the counters MAX to
	// the same values), and the overlap is what protects a row committed with a
	// slightly older time_updated than one we already saw. What must NEVER come
	// back is the 25-day-old row: that would mean the watermark bought nothing.
	got = nil
	c.runPass(context.Background(), time.Time{}, sink)
	for _, ev := range got {
		if ev.SessionID == "ses_old" {
			t.Errorf("the second pass re-emitted the 25-day-old row; the watermark is not bounding the scan at all")
		}
	}
	if len(got) > 1 {
		t.Errorf("the second pass emitted %d events; only rows inside the %s overlap window may be re-read", len(got), 2*time.Millisecond)
	}
}

// memCheckpoints is an in-memory CheckpointStore.
type memCheckpoints struct {
	rows    map[string]store.WatcherCheckpoint
	saveErr error
	loadErr error
}

func (m *memCheckpoints) LoadWatcherCheckpoint(_ context.Context, key string) (store.WatcherCheckpoint, bool, error) {
	if m.loadErr != nil {
		return store.WatcherCheckpoint{}, false, m.loadErr
	}
	cp, ok := m.rows[key]
	return cp, ok, nil
}

func (m *memCheckpoints) SaveWatcherCheckpoint(_ context.Context, cp store.WatcherCheckpoint) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	if m.rows == nil {
		m.rows = make(map[string]store.WatcherCheckpoint)
	}
	m.rows[cp.Path] = cp
	return nil
}

// TestWatermarkSurvivesARestart: the checkpoint is what makes a restart cheap.
// It is a DERIVED cache, so losing it must cost a re-scan and nothing else —
// both halves are asserted.
func TestWatermarkSurvivesARestart(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	// TWO rows a day apart. The older one is what the restart must NOT re-emit;
	// the newer one sits inside the deliberate overlap window and may be re-read
	// (a no-op at the store), so the assertion is on the OLD row, not on a total
	// of zero — see the note in TestWatermarkIsTimeUpdatedNotMessageID.
	completed := goldenCompleted
	older := completed - 24*60*60*1000
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 7,
		Rows: []msgRow{
			buildRow(t, msgSpec{
				ID: "msg_wm_old", SessionID: "ses_wm_old", Cwd: filepath.Join(repo, "src"),
				Created: older - 1000, Completed: &older, TimeUpdated: older,
				Tokens: autoTotal(100, 10, 5, 50, 0),
			}),
			buildRow(t, msgSpec{
				ID: "msg_wm", SessionID: "ses_wm", Cwd: filepath.Join(repo, "src"),
				Created: completed - 1000, Completed: &completed, TimeUpdated: completed,
				Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
			}),
		},
	})
	cp := &memCheckpoints{}
	var checkpointKey string
	run := func(cps CheckpointStore) []collector.TokenEvent {
		logger, _ := newTestLogger()
		c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger, Checkpoints: cps, Interval: time.Millisecond})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		checkpointKey = c.checkpointKey()
		var got []collector.TokenEvent
		c.loadWatermark(context.Background())
		c.runPass(context.Background(), time.Time{}, collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
			got = append(got, ev)
			return nil
		}))
		return got
	}
	sessionsOf := func(evs []collector.TokenEvent) []string {
		out := make([]string, 0, len(evs))
		for _, e := range evs {
			out = append(out, e.SessionID)
		}
		return out
	}
	if got := run(cp); len(got) != 2 {
		t.Fatalf("first process: want both rows, got %v", sessionsOf(got))
	}
	for _, ev := range run(cp) {
		if ev.SessionID == "ses_wm_old" {
			t.Errorf("the second process re-emitted the day-old row; the persisted watermark bought nothing")
		}
	}
	// The persisted blob must carry the drift tripwire too, and NOTHING about the
	// developer — docs/privacy.md enumerates this blob's contents as complete.
	saved := cp.rows[checkpointKey]
	if !strings.Contains(saved.Metadata, `"opencode_migration_count":7`) {
		t.Errorf("the checkpoint must record Opencode's migration count as the drift tripwire; metadata = %s", saved.Metadata)
	}
	for _, forbidden := range []string{"alice", "cwd", repo, "glm-5.3", "zai"} {
		if strings.Contains(saved.Metadata, forbidden) {
			t.Errorf("the checkpoint metadata contains %q; it must hold only the watermark and the migration count (docs/privacy.md enumerates it as complete). metadata = %s", forbidden, saved.Metadata)
		}
	}
	// Losing the checkpoint costs a re-scan and nothing else.
	if got := run(&memCheckpoints{}); len(got) != 2 {
		t.Errorf("a lost checkpoint must degrade to a full (idempotent) re-scan of both rows; got %v", sessionsOf(got))
	}
}

// TestUnreadableCheckpointDegradesToAFullScan: a corrupt or unknown-schema
// checkpoint must never be half-believed. Resuming from a misread watermark would
// skip real spend silently, which is strictly worse than re-scanning.
func TestUnreadableCheckpointDegradesToAFullScan(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	dbPath := filepath.Join(t.TempDir(), "x.db")
	logger, cap := newTestLogger()
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.checkpoints = &memCheckpoints{rows: map[string]store.WatcherCheckpoint{
		c.checkpointKey(): {Path: c.checkpointKey(), Metadata: `{"schema":999,"opencode_time_updated_ms":99999999999999}`},
	}}
	c.loadWatermark(context.Background())
	if c.watermark != 0 {
		t.Errorf("an unknown checkpoint schema must leave the watermark at zero (full re-scan); got %d", c.watermark)
	}
	if got := cap.find(slog.LevelWarn, "unreadable or of an unknown schema"); len(got) != 1 {
		t.Errorf("want a WARN naming the unreadable checkpoint; got %d. Records:%s", len(got), cap.dump())
	}
}

// TestIngestFailureDoesNotAdvanceTheWatermark. Advancing before the sink has
// accepted an event would drop that event PERMANENTLY, and nothing downstream
// would ever know — the events simply never arrive.
func TestIngestFailureDoesNotAdvanceTheWatermark(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_fail", SessionID: "ses_fail", Cwd: filepath.Join(repo, "src"),
			Created: completed - 1000, Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	logger, _ := newTestLogger()
	cp := &memCheckpoints{}
	c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "alice", Logger: logger, Checkpoints: cp, Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.runPass(context.Background(), time.Time{}, collector.IngesterFunc(func(context.Context, collector.TokenEvent) error {
		return errRefused
	}))
	if c.watermark != 0 {
		t.Fatalf("watermark advanced to %d despite a refused ingest; that event is now lost forever", c.watermark)
	}
	// And the retry captures it.
	n := 0
	c.runPass(context.Background(), time.Time{}, collector.IngesterFunc(func(context.Context, collector.TokenEvent) error {
		n++
		return nil
	}))
	if n != 1 {
		t.Errorf("the retry must capture the previously-refused event; got %d", n)
	}
}

var errRefused = errRefusedType{}

type errRefusedType struct{}

func (errRefusedType) Error() string { return "sink refused" }

// ─────────────────────────────────────────────────────────────────────────────
// READ-ONLY, AND NEVER IMMUTABLE
// ─────────────────────────────────────────────────────────────────────────────

// TestOpenIsReadOnly opens a fixture through the collector's own open() and
// proves a write is REFUSED by SQLite.
//
// Asserting on the DSN string would only prove the string; this asserts the
// behaviour the string is supposed to buy.
// 🔴 IT IS TABLE-DRIVEN OVER URI METACHARACTERS, AND THAT IS THE POINT. The
// original version of this test used one benign filename, and passed against a
// DSN built by `fmt.Sprintf("file:%s?mode=ro…")` with the path interpolated RAW —
// which is broken for any path containing `?`, `#` or `%`, because those are URI
// metacharacters and `mode=ro` is only a query parameter. Measured against the
// pinned driver with that construction:
//
//	…/oc.db?mode=rwc&junk → a DIFFERENT, newly CREATED database, opened
//	                        READ-WRITE; `CREATE TABLE` returned nil
//	…/a#b.db              → `#b.db` read as a fragment, DISCARDING mode=ro;
//	                        the file `…/a` was CREATED and written
//	…/oc%41.db            → percent-decoded to `…/ocA.db`
//
// So a benign-path test could not fail no matter how wrong the DSN construction
// was. The path is operator-supplied (`--opencode-db`, `collectors.opencode.db_path`),
// and each case below asserts BOTH halves: the write is refused, and no stray
// file appeared beside the fixture — the second half is what caught the `#` case,
// because the write "succeeded" against a file nobody asked for.
func TestOpenIsReadOnly(t *testing.T) {
	repo := repoDir(t)
	cases := []struct {
		name     string
		fileName string
	}{
		{"ordinary path", "opencode.db"},
		{"path with a query metacharacter", "oc.db?mode=rwc&junk"},
		{"path with a fragment metacharacter", "a#b.db"},
		{"path with a percent escape", "oc%41.db"},
		{"path with a space and an ampersand", "o c&x.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := newFixtureDBNamed(t, tc.fileName, dbSpec{Migrations: 3})
			dir := filepath.Dir(dbPath)
			before := dirEntries(t, dir)

			logger, _ := newTestLogger()
			c, err := New(Config{DBPath: dbPath, Repos: []RepoTarget{{Path: repo}}, Logger: logger})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			db, err := c.open()
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()

			// CONTROL ARM, and it must come first: it proves we opened the file the
			// operator NAMED. Without it, a DSN that silently opened some other
			// (empty, absent) database would sail through the write assertions
			// below — a missing table refuses writes just as convincingly as
			// read-only mode does.
			var n int
			if err := db.QueryRow("SELECT count(*) FROM migration").Scan(&n); err != nil || n != 3 {
				t.Fatalf("the handle is not reading the fixture at %q (n=%d err=%v) — the DSN opened a different file", dbPath, n, err)
			}

			if _, err := db.Exec("CREATE TABLE tier_should_never_exist (x int)"); err == nil {
				t.Error("the collector opened Opencode's database WRITABLE; it must never be able to modify a third-party tool's store")
			}
			if _, err := db.Exec("DELETE FROM migration"); err == nil {
				t.Error("a DELETE succeeded against Opencode's database")
			}

			// Nothing was written, and nothing NEW appeared beside the fixture.
			//
			// ⚠️ THE VERIFY HANDLE USES readOnlyDSN TOO, and it has to. The driver's
			// BARE-path DSN form splits on '?' exactly as the URI form does, so
			// `sql.Open("sqlite", dbPath)` on a `?`-bearing path would open — and
			// CREATE — a different, empty file, then report "no such table:
			// migration" as though the collector had destroyed the fixture. Using
			// the escaped form is not circular: the control arm above already
			// established that this DSN reaches the file the operator named.
			verify, err := sql.Open("sqlite", readOnlyDSN(dbPath))
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = verify.Close() }()
			if err := verify.QueryRow("SELECT count(*) FROM migration").Scan(&n); err != nil || n != 3 {
				t.Errorf("the third-party database was modified: n=%d err=%v", n, err)
			}
			for name := range dirEntries(t, dir) {
				if before[name] {
					continue
				}
				// SQLite's own WAL side files for THIS database are expected, not
				// stray: a mode=ro open of a WAL database still creates/uses the
				// -shm (that is the WAL protocol, and it is why a read-only mount
				// or a foreign uid cannot read one). What must never appear is a
				// file whose name is not derived from the fixture's.
				if name == tc.fileName+"-shm" || name == tc.fileName+"-wal" {
					continue
				}
				t.Errorf("opening %q created a stray file %q — the path escaped the URI and SQLite opened (and created) something else",
					tc.fileName, name)
			}
		})
	}
}

// dirEntries returns the set of names in dir, so a test can prove nothing new
// appeared.
func dirEntries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// TestNeverOpensImmutable is a SOURCE-level guard, deliberately, because the
// failure it prevents has no runtime symptom to test for.
//
// `immutable=1` on a live WAL database does not error and does not return an
// empty result: SQLite skips the WAL and the locking protocol and returns
// pre-WAL or torn data with total confidence. A behavioural test would need a
// concurrent writer and would still only catch it probabilistically. Refusing the
// token in the source is the check that actually holds.
// It inspects STRING LITERALS, not raw file text: this package's doc comments
// discuss `immutable=1` at length precisely because it must not be used, and a
// substring scan over whole files would flag that prose. Reading the literals is
// what distinguishes "the code does this" from "a comment mentions this".
func TestNeverOpensImmutable(t *testing.T) {
	fset := token.NewFileSet()
	sawReadOnlyDSN := false
	for _, path := range packageGoFiles(t) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "immutable") {
				t.Errorf("%s builds a DSN containing %q. Opencode holds a live WAL; immutable=1 makes SQLite skip the WAL and the locking protocol and return stale or torn data with NO error", path, lit.Value)
			}
			if strings.Contains(lit.Value, "mode=ro") {
				sawReadOnlyDSN = true
			}
			return true
		})
	}
	// CONTROL ARM. "No literal contains immutable" is vacuously true over an empty
	// literal set — an unparsed file, a renamed package directory, a walk that
	// found nothing. The read-only DSN is the literal this guard is the
	// counterpart of, so seeing it proves the walk reached the code that matters.
	if !sawReadOnlyDSN {
		t.Fatal("the literal walk never saw a `mode=ro` DSN, so it is not reading open()'s source and the immutable result above is meaningless")
	}
}

// TestNeverDecodesTheClientsCost reads the AST and requires that no struct in this
// package declares a field tagged `json:"cost"`.
//
// The behavioural guard (TestNeverTrustsTheClientsCost) proves the value is not
// USED today. This one proves it is not even AVAILABLE — so the next contributor
// cannot wire it up by autocompletion. Opencode writes `cost: 0` on every row.
func TestNeverDecodesTheClientsCost(t *testing.T) {
	fset := token.NewFileSet()
	files := packageGoFiles(t)
	if len(files) < 2 {
		t.Fatalf("scanned %d non-test files — the scan is broken, and every check below would pass vacuously", len(files))
	}
	sawTokensField := false
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := field.Tag.Value
				if strings.Contains(tag, `json:"cost"`) {
					t.Errorf("%s declares a field tagged json:\"cost\". Opencode writes cost:0 on every row; the field must not exist at all", path)
				}
				if strings.Contains(tag, `json:"tokens"`) {
					sawTokensField = true
				}
			}
			return true
		})
	}
	// CONTROL ARM. Every assertion above is "nothing was found", which passes
	// vacuously if the walk never reaches a struct tag. Prove it reaches the
	// tags it is supposed to be inspecting.
	if !sawTokensField {
		t.Fatal(`the AST walk never saw a json:"tokens" tag, so it is not inspecting the decode struct and the "no cost field" result is meaningless`)
	}
}

// packageGoFiles returns this package's non-test .go files.
func packageGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, n)
	}
	return out
}
