package opencode

// Test fixtures for the Opencode collector.
//
// EVERY COMMITTED FIXTURE IS SYNTHETIC. The maintainer's real store
// (~/.local/share/opencode/opencode.db) is read-only, ~795 MB, and contains
// prompt content, OAuth access/refresh tokens (`account`), and API credentials
// (`credential`) — none of which belongs in a repository, and none of which this
// collector reads. The synthetic databases below reproduce ONLY the two tables
// the collector touches, with the column types Opencode actually declares
// (verified against the live schema 2026-08-28), so a shape change in the real
// store shows up as a test that no longer describes reality rather than as a
// fixture that quietly diverged.
//
// The ONE test that reads the real store is TestReconcileAgainstRealStore, which
// is env-gated and skips by default — see its doc.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite"
)

// embeddedPricesPath is the built-in price table. Its model-only glm-5.3 row
// (provider zai, per_token, #786) is the rate every GLM-5.3 event on the Z.ai
// coding plan prices at; the pricing tests read the REAL file rather than an
// inlined copy of its rates.
const embeddedPricesPath = "../../store/prices.yaml"

// withZaiPrices makes the built-in price table the active one for the calling
// test and restores it afterwards, so a table another test swapped in cannot
// price this test's events.
//
// store.LoadPriceTable mutates a PACKAGE-GLOBAL table with no restore of its own,
// so this helper owns the restore. Tests in this file must not call t.Parallel():
// the global is process-wide and two parallel tests would race over which table
// is active — which would not fail loudly, it would silently price one test's
// events with the other test's rates.
func withZaiPrices(t *testing.T) {
	t.Helper()
	if _, err := store.LoadPriceTable(embeddedPricesPath); err != nil {
		t.Fatalf("load the built-in price table %s: %v", embeddedPricesPath, err)
	}
	t.Cleanup(func() {
		if _, err := store.LoadPriceTable(embeddedPricesPath); err != nil {
			t.Fatalf("restore the embedded price table: %v", err)
		}
	})
}

// msgRow is one synthetic `message` row.
type msgRow struct {
	ID          string
	SessionID   string
	TimeUpdated int64
	Data        string
}

// tokens describes one synthetic message's token counts. Pointers everywhere the
// real blob has an optional field, so a fixture can express ABSENT as distinct
// from zero — the distinction the parser depends on.
type tokenSpec struct {
	Total      *int64
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
}

func i64(v int64) *int64 { return &v }

// autoTotal returns a tokenSpec whose Total is the additive sum — the shape every
// real Opencode row has.
func autoTotal(input, output, reasoning, cacheRead, cacheWrite int64) tokenSpec {
	return tokenSpec{
		Total:      i64(input + output + reasoning + cacheRead + cacheWrite),
		Input:      input,
		Output:     output,
		Reasoning:  reasoning,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	}
}

// msgSpec describes one synthetic message end to end.
type msgSpec struct {
	ID          string
	SessionID   string
	Role        string // defaults to "assistant"
	Provider    string // defaults to "zai-coding-plan"
	Model       string // defaults to "glm-5.3"
	Cwd         string
	Created     int64
	Completed   *int64 // nil = still streaming
	TimeUpdated int64
	Tokens      tokenSpec
	// ClientCost, when non-zero, is written into the blob's `cost` field — the
	// field this collector must never read.
	ClientCost float64
	// RawOverride, when non-empty, replaces the whole JSON blob.
	RawOverride string
}

// buildRow renders one msgSpec into a `message` row.
func buildRow(t *testing.T, s msgSpec) msgRow {
	t.Helper()
	if s.RawOverride != "" {
		return msgRow{ID: s.ID, SessionID: s.SessionID, TimeUpdated: s.TimeUpdated, Data: s.RawOverride}
	}
	role := s.Role
	if role == "" {
		role = "assistant"
	}
	provider := s.Provider
	if provider == "" {
		provider = "zai-coding-plan"
	}
	model := s.Model
	if model == "" {
		model = "glm-5.3"
	}
	// Built as a map so the `cost` key can be present in the fixture even though
	// the production decode struct has no field for it — which is exactly what
	// TestNeverTrustsTheClientsCost needs to exercise.
	blob := map[string]any{
		"role":       role,
		"modelID":    model,
		"providerID": provider,
		"cost":       s.ClientCost,
		"path":       map[string]any{"cwd": s.Cwd, "root": "/"},
		"time":       timeObject(s.Created, s.Completed),
		"tokens":     tokensObject(s.Tokens),
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal fixture message: %v", err)
	}
	return msgRow{ID: s.ID, SessionID: s.SessionID, TimeUpdated: s.TimeUpdated, Data: string(raw)}
}

func timeObject(created int64, completed *int64) map[string]any {
	out := map[string]any{"created": created}
	if completed != nil {
		out["completed"] = *completed
	}
	return out
}

func tokensObject(ts tokenSpec) map[string]any {
	out := map[string]any{
		"input":     ts.Input,
		"output":    ts.Output,
		"reasoning": ts.Reasoning,
		"cache":     map[string]any{"read": ts.CacheRead, "write": ts.CacheWrite},
	}
	if ts.Total != nil {
		out["total"] = *ts.Total
	}
	return out
}

// dbSpec configures a synthetic Opencode database.
type dbSpec struct {
	// Migrations is how many rows the `migration` table gets. Zero writes the
	// table but leaves it empty (a shape the collector must reject).
	Migrations int
	// NoMigrationTable omits Opencode's migration ledger entirely.
	NoMigrationTable bool
	// MessageDDL overrides the `message` table definition, for drift tests.
	MessageDDL string
	Rows       []msgRow
}

// realMessageDDL is Opencode's own `message` table definition, copied verbatim
// from the live store's sqlite_master 2026-08-28 minus the foreign key (whose
// referenced `session` table this collector never reads).
const realMessageDDL = "CREATE TABLE `message` (" +
	"`id` text PRIMARY KEY," +
	"`session_id` text NOT NULL," +
	"`time_created` integer NOT NULL," +
	"`time_updated` integer NOT NULL," +
	"`data` text NOT NULL)"

// newFixtureDB writes a synthetic Opencode database and returns its path.
func newFixtureDB(t *testing.T, spec dbSpec) string {
	t.Helper()
	return newFixtureDBNamed(t, "opencode.db", spec)
}

// newFixtureDBNamed is newFixtureDB with an explicit FILENAME, so a test can
// stage a fixture at a path containing URI metacharacters. It writes through a
// bare path (not a `file:` URI), which is exactly why it can create names the
// URI form would otherwise mangle.
func newFixtureDBNamed(t *testing.T, fileName string, spec dbSpec) string {
	t.Helper()
	dir := t.TempDir()
	final := filepath.Join(dir, fileName)
	// ⚠️ BUILT UNDER A PLAIN NAME AND THEN RENAMED. The fixture writer goes
	// through the same driver, whose BARE-path DSN form splits on '?' — so
	// creating a `?`-bearing fixture directly would silently write to a truncated
	// path and the test would then "fail" on a file that was never created. The
	// rename is what lets this helper stage a name the DSN form cannot.
	path := filepath.Join(dir, "fixture-under-construction.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	// Closed explicitly below (before the rename); this defer only covers the
	// t.Fatalf paths above it.
	defer func() { _ = db.Close() }()

	if !spec.NoMigrationTable {
		if _, err := db.Exec("CREATE TABLE `migration` (id TEXT PRIMARY KEY, time_completed INTEGER NOT NULL)"); err != nil {
			t.Fatalf("create migration table: %v", err)
		}
		for i := 0; i < spec.Migrations; i++ {
			if _, err := db.Exec("INSERT INTO migration (id, time_completed) VALUES (?, ?)",
				fmt.Sprintf("2026010100000%d_fixture", i), 1_700_000_000_000+int64(i)); err != nil {
				t.Fatalf("seed migration: %v", err)
			}
		}
	}
	ddl := spec.MessageDDL
	if ddl == "" {
		ddl = realMessageDDL
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create message table: %v", err)
	}
	for _, r := range spec.Rows {
		if _, err := db.Exec(
			"INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)",
			r.ID, r.SessionID, r.TimeUpdated, r.TimeUpdated, r.Data); err != nil {
			t.Fatalf("insert fixture message %s: %v", r.ID, err)
		}
	}
	// Close before renaming so the WAL is checkpointed into the main file and the
	// side files are gone; otherwise the renamed database would be missing its
	// content until a -wal that no longer matches its name was replayed.
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture db: %v", err)
	}
	if path != final {
		if err := os.Rename(path, final); err != nil {
			t.Fatalf("rename fixture to %q: %v", fileName, err)
		}
	}
	return final
}

// capturingLogger records every log record so a test can assert that a skip was
// LOUD, not merely counted. A skip that increments a counter but logs nothing is
// invisible to the operator who has to notice it.
type capturingLogger struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturingLogger) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturingLogger) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *capturingLogger) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturingLogger) WithGroup(string) slog.Handler      { return c }

// find returns every record at or above level whose message contains substr.
func (c *capturingLogger) find(level slog.Level, substr string) []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slog.Record
	for _, r := range c.records {
		if r.Level >= level && strings.Contains(r.Message, substr) {
			out = append(out, r)
		}
	}
	return out
}

// attrOf returns the value of one attribute on a record, rendered as a string.
func attrOf(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

// dump renders every captured record, for a failure message that says what
// actually happened rather than only what did not.
func (c *capturingLogger) dump() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, r := range c.records {
		fmt.Fprintf(&b, "\n  [%s] %s", r.Level, r.Message)
	}
	if b.Len() == 0 {
		return "\n  (no log records at all)"
	}
	return b.String()
}

func newTestLogger() (*slog.Logger, *capturingLogger) {
	h := &capturingLogger{}
	return slog.New(h), h
}

// repoDir creates a directory to use as a repo target and returns it. RepoScope
// does not require a real git checkout (only IssueResolver does, and this
// collector deliberately builds none), so a plain directory is the honest
// minimum: it exercises the same prefix + symlink matching a real checkout would.
func repoDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(d, "src"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	return d
}
