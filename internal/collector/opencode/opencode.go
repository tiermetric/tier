package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite" // the same pure-Go driver internal/store uses
)

const (
	// DefaultScanInterval is the re-scan cadence when config omits scan_interval.
	// A pass is one indexed-by-nothing table read bounded by the watermark, which
	// measured 120 ms over the maintainer's ENTIRE 10,206-row message table
	// (4.9 MB of JSON) on a 795 MB database — the `message` table is a small part
	// of that file; the bulk is tool output in `part`. 5m keeps a running
	// Opencode session visible on the dashboard within one coffee refill at a
	// duty cycle of about 0.04%.
	DefaultScanInterval = 5 * time.Minute

	// MinScanInterval is the config floor. Below this the read stops being
	// negligible against a live WAL while adding no freshness an Opencode turn
	// could actually deliver.
	MinScanInterval = 30 * time.Second

	// watermarkLagFactor multiplies the scan interval to produce the overlap
	// subtracted from the watermark before each pass.
	//
	// Re-reading a row is FREE at the store (the idempotency key collides and the
	// counters MAX to the same values), while missing one is permanent, so the
	// asymmetry is priced in favour of overlap. Concretely it covers a row whose
	// `time_updated` lands slightly behind one we already read — two turns
	// completing concurrently in different sessions — which a strict high-water
	// mark would step over.
	watermarkLagFactor = 2

	// maxClockSkew bounds how far ahead of THIS machine's clock an Opencode
	// timestamp may be before it is treated as CORRUPT rather than as time.
	//
	// 🔴 WITHOUT IT, ONE OUT-OF-RANGE ROW POISONS THE COLLECTOR PERMANENTLY, and
	// does it while logging a healthy scan. `time_updated` and `time.completed`
	// are third-party integers with no bound, and the shape that produces a bad
	// one needs no attacker — a microsecond value in a millisecond column, from a
	// unit mix-up or a skewed device, is enough. Measured end to end before this
	// guard existed:
	//
	//   - the watermark advanced to that row and was PERSISTED, so `floor()` sat
	//     in the year 58628; setWatermark only moves forward and the checkpoint
	//     survives restarts, so no later row ever matched again;
	//   - the poison row itself kept matching `time_updated >= floor`, so
	//     rowsRead stayed non-zero and every loud arm in reportPass was bypassed —
	//     "opencode scan complete" at INFO, forever, capturing nothing;
	//   - and its `time.completed` became the event's Timestamp, so its cost sat
	//     inside EVERY window query from now until the year 58628.
	//
	// A day is far past any legitimate clock disagreement between one machine's
	// Go runtime and the SQLite file it wrote seconds ago, and far short of any
	// value a corrupt row produces.
	maxClockSkew = 24 * time.Hour

	// checkpointSchema versions the JSON blob this collector owns in
	// watcher_checkpoint.metadata. Bump it if the shape changes; an unrecognized
	// version is treated as "no watermark" (a full, idempotent re-scan) rather
	// than misread as one.
	checkpointSchema = 1
)

// maxRowsPerPass bounds one read transaction. A pass loops until a batch comes
// back short, so a first-run backfill over an arbitrarily large store still
// completes; the cap is about keeping each individual read SHORT (a long-lived
// reader pins Opencode's WAL and bloats THEIR file) and keeping peak memory
// proportional to the batch rather than to all of history.
//
// A var, not a const, purely so the batching tests can shrink it — the same
// reason codexrollout's maxRolloutFile is one. Proving the multi-batch loop and
// its tie-group guard with a real 20,000-row fixture would put a slow, large test
// into every `make check`, and leaving them unproven is how a loop that silently
// stops after one batch ships.
var maxRowsPerPass = 20_000

// RepoTarget names one repository this collector attributes cost to. Identical
// in shape and meaning to codexrollout.RepoTarget so `tierd serve` can build both
// from the same --watch-repo list.
type RepoTarget struct {
	// Path is the git checkout root. An Opencode message whose recorded cwd is
	// at, inside, or a git worktree of this path is attributed here.
	Path string
	// Slug is the OPERATOR OVERRIDE for the canonical "owner/repo" identity
	// (#231), winning over remote.origin.url. Empty falls back to
	// remote.origin.url, then to repoid.Unqualified.
	Slug string
}

// CheckpointStore is the persistence seam for the scan watermark — satisfied by
// *store.DB. It is an interface, not the concrete type, so this package's tests
// can exercise a lost/corrupt/absent checkpoint without a database, and so a
// caller that has no store at all (the on-demand CLI path) can pass nil.
//
// NOTE the watermark is a DERIVED CACHE, exactly as the JSONL watcher's byte
// offsets are: losing it costs one re-scan, which the idempotency keys absorb.
// Nothing about correctness depends on it surviving.
//
// It asks for its OWN KEY rather than enumerating the table. That is not just
// cheaper — watcher_checkpoint is shared with the JSONL watcher, and a consumer
// that holds every other consumer's rows is one missing filter away from acting
// on one. See store.IsFileCheckpoint for the defect that already caused.
type CheckpointStore interface {
	LoadWatcherCheckpoint(ctx context.Context, key string) (store.WatcherCheckpoint, bool, error)
	SaveWatcherCheckpoint(ctx context.Context, cp store.WatcherCheckpoint) error
}

// checkpointMetadata is the JSON blob this collector owns in
// watcher_checkpoint.metadata.
//
// 🔴 THE WATERMARK IS `time_updated`, NEVER `message.id`. Opencode's message ids
// are NOT monotonic: measured on the maintainer's store 2026-08-28, the
// lexicographic maximum id (`msg_fc96fb406001…`) belongs to a row written
// 2026-08-03, while the newest row (2026-08-28) sorts far below it. A collector
// that resumed from `max(id)` would have skipped 25 days of spend and reported a
// clean, quiet, successful scan every five minutes while doing it.
//
// It holds nothing about the developer: two integers and a schema tag. See
// docs/privacy.md, which enumerates this blob's contents as complete.
type checkpointMetadata struct {
	Schema int `json:"schema"`
	// TimeUpdatedMS is the highest Opencode `message.time_updated` (epoch
	// milliseconds) fully processed and ingested.
	TimeUpdatedMS int64 `json:"opencode_time_updated_ms"`
	// MigrationCount is how many rows Opencode's own `migration` table held at
	// that point — the schema-drift tripwire. A change means Opencode migrated
	// its schema under us and this collector's field mapping is worth re-checking.
	MigrationCount int `json:"opencode_migration_count"`
}

// Collector reads the Opencode SQLite session store and emits one TokenEvent per
// completed assistant message. It implements collector.Collector.
//
// EVERY FIELD IS UNEXPORTED and Config is the only way in — same reasoning as
// codexrollout.Collector: New performs the checks that make the zero value
// impossible, and an exported field set would let `&Collector{}` skip all of them.
type Collector struct {
	dbPath      string
	repos       []RepoTarget
	developerID string
	interval    time.Duration
	logger      *slog.Logger

	slugOnce sync.Once
	slugs    []string

	// policyOnce gates the one-time startup INFO that names the capture policy.
	policyOnce sync.Once
	// disabledOnce gates the one-time "no database here" INFO.
	disabledOnce sync.Once
	// warnedProviders remembers which unknown providerIDs have already been
	// WARNed about, so an unsupported route is reported once rather than once per
	// row forever. Bounded by maxWarnedProviders — providerID is a third-party
	// string, so an unbounded set keyed on it is a memory sink on a corrupt store.
	warnedProviders     sync.Map
	warnedProviderCount atomic.Int64
	warnSuppressed      atomic.Bool

	checkpoints CheckpointStore
	settled     collector.SettledFunc
	lost        collector.LostFunc

	// now is the clock the skew horizon is measured against. Injectable so the
	// horizon's behaviour is testable without waiting or sleeping; never nil
	// after New.
	now func() time.Time

	mu sync.Mutex
	// watermark is the highest fully-ingested `time_updated`, in epoch ms. Zero
	// means "no watermark" — scan everything the caller's window admits.
	watermark int64
	// migrationCount is the last observed size of Opencode's `migration` table.
	// -1 means "not yet observed".
	migrationCount int
}

// Config configures a Collector. Repos is required; everything else defaults.
type Config struct {
	// DBPath overrides ~/.local/share/opencode/opencode.db.
	DBPath string
	// Repos are the repositories in scope. A message whose cwd matches none of
	// them is DROPPED, not attributed — cross-repo bleed would put another
	// project's dollars on this project's issues (#15).
	Repos []RepoTarget
	// DeveloperID labels every emitted event. Empty falls back to the OS username
	// via the same chain the JSONL collector uses.
	DeveloperID string
	Interval    time.Duration
	Logger      *slog.Logger
	// Now overrides the clock used for the corrupt-timestamp horizon. Nil means
	// time.Now; tests set it to reach the horizon deterministically.
	Now func() time.Time
	// Checkpoints persists the scan watermark across restarts (#71). Nil is
	// legal and means "in-memory only": every process start re-scans the window
	// the caller asked for, which is correct but pays the backfill again.
	Checkpoints CheckpointStore
	// Settled, when set, is called after each pass whose scan succeeded, from
	// Run's since through the pass's start less the watermark's lag: the
	// watermark advances only over what was ingested, so every pass extends the
	// run the first pass began.
	Settled collector.SettledFunc
	// Lost, when set, records each in-scope row a pass steps over without
	// recording its spend, before the watermark moves past it.
	Lost collector.LostFunc
}

// New builds a Collector with defaults filled in, or returns an error for a
// configuration that could never capture anything. Fail-fast rather than
// silently-disabled: an operator who enabled this collector and got zero rows
// must be told why at startup, not left to infer it from an empty dashboard.
func New(cfg Config) (*Collector, error) {
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("opencode: at least one repo target is required (no target means no message can ever be attributed)")
	}
	for i, r := range cfg.Repos {
		if strings.TrimSpace(r.Path) == "" {
			return nil, fmt.Errorf("opencode: repos[%d].Path is empty", i)
		}
	}
	c := &Collector{
		dbPath:         cfg.DBPath,
		repos:          cfg.Repos,
		developerID:    cfg.DeveloperID,
		interval:       cfg.Interval,
		logger:         cfg.Logger,
		checkpoints:    cfg.Checkpoints,
		settled:        cfg.Settled,
		lost:           cfg.Lost,
		now:            cfg.Now,
		migrationCount: -1,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.interval <= 0 {
		c.interval = DefaultScanInterval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("opencode: resolve home dir for the default database path: %w", err)
		}
		c.dbPath = filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	}
	return c, nil
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return collector.SourceOpencode }

// horizonMS is the newest Opencode timestamp (epoch ms) this collector will
// treat as real rather than corrupt. See maxClockSkew.
func (c *Collector) horizonMS() int64 { return c.now().Add(maxClockSkew).UnixMilli() }

// DBPath returns the resolved database path, for startup logging.
func (c *Collector) DBPath() string { return c.dbPath }

// Run implements collector.Collector: one pass immediately, then a re-scan every
// Interval until ctx is cancelled.
//
// DELIBERATE ASYMMETRY with the JSONL watcher, matching codexrollout and the org
// pollers: a pass failure is logged at ERROR and retried on the next tick — it
// does NOT abort Run and must NOT kill serve. A locked, corrupt or migrated
// Opencode database is not a reason to take down the whole binary and stop
// capturing Claude Code.
func (c *Collector) Run(ctx context.Context, since time.Time, ing collector.Ingester) error {
	if ing == nil {
		return fmt.Errorf("opencode: Ingester is required")
	}
	c.logPolicyOnce()
	c.loadWatermark(ctx)
	c.runPass(ctx, since, ing)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.runPass(ctx, since, ing)
		}
	}
}

// Collect implements collector.Collector: one pass returning every event in a
// slice.
//
// STATELESS with respect to the watermark — it neither reads nor advances it, and
// scans the caller's full window every time. (It does observe the collector's
// migration-drift counter, which is a diagnostic and not a scan bound.) A batch caller asking for
// "everything since T" must get everything since T even when a concurrent Run has
// already consumed it.
func (c *Collector) Collect(ctx context.Context, since time.Time) ([]collector.TokenEvent, error) {
	c.logPolicyOnce()
	var out []collector.TokenEvent
	sink := collector.IngesterFunc(func(_ context.Context, ev collector.TokenEvent) error {
		out = append(out, ev)
		return nil
	})
	if err := c.scan(ctx, since, 0, sink, nil); err != nil {
		return out, err
	}
	return out, nil
}

// runPass performs one scan and ingests its events, logging (not returning) any
// error.
func (c *Collector) runPass(ctx context.Context, since time.Time, ing collector.Ingester) {
	passStart := c.now()
	floor := c.floor()
	advance := func(newWatermark int64, migrations int) {
		c.setWatermark(ctx, newWatermark, migrations)
	}
	if err := c.scan(ctx, since, floor, ing, advance); err != nil {
		if ctx.Err() != nil {
			return // clean shutdown; the watermark stays where the last good batch left it
		}
		// An absent database is the ordinary "Opencode is not installed here"
		// case, not a failure: INFO, and only once, so an operator who enabled
		// the collector ahead of installing Opencode does not get an ERROR every
		// five minutes for something that is working as designed.
		if IsDisabled(err) {
			c.disabledOnce.Do(func() {
				c.logger.Info("opencode collector disabled: no database at the configured path",
					"db", logsafe.Str(c.dbPath),
					"hint", "install Opencode, or drop --opencode / the collectors.opencode config block")
			})
			return
		}
		c.logger.Error("opencode scan failed; will retry next tick", "err", logsafe.Err(err))
		return
	}
	// A row read in flight completes with its own time.completed, which a
	// concurrent commit can place just before passStart: the watermark's lag
	// covers the same race.
	if c.settled != nil {
		c.settled(ctx, since, passStart.Add(-time.Duration(watermarkLagFactor)*c.interval))
	}
}

// scan runs the batched read loop.
//
// CURSOR ORDERING, the load-bearing part: `advance` is called only after every
// event in a batch has been handed to the Ingester successfully. An aborted
// ingest (write error or shutdown) leaves the watermark where it was, so the
// un-ingested tail is re-read next pass. Advancing first — or in a defer — would
// drop that tail PERMANENTLY, and nothing downstream would ever know: the events
// simply never arrive.
//
// A row that was SKIPPED (incomplete, excluded provider, failed identity) still
// moves the watermark past itself. Nothing was lost — it produced no event — and
// pinning the watermark on it would re-read it forever. A row that later heals
// gets a fresh `time_updated` from the UPDATE that healed it and is re-read then;
// that is the whole reason the watermark is on `time_updated` and not on an
// insert-time column.
func (c *Collector) scan(ctx context.Context, since time.Time, floor int64, ing collector.Ingester, advance func(int64, int)) error {
	db, err := c.open()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	health, err := c.checkSchema(ctx, db, floor)
	if err != nil {
		return err
	}
	c.noteMigrationDrift(health.migrationCount)

	targets := c.resolveTargets()
	// Built ONCE per pass, not per row: this used to be called inside eventFor,
	// which allocated a fresh slice for every candidate row (20,000 of them on a
	// full batch) to hold the same pointers each time.
	scopes := scopesOf(targets)
	developer := c.developerID
	if developer == "" {
		developer = collector.OSUsername()
	}
	sinceMS := int64(0)
	if !since.IsZero() {
		sinceMS = since.UTC().UnixMilli()
	}

	var total passStats
	// The WITHIN-PASS read position. See rowCursor: it is a KEYSET over
	// (time_updated, id), not the plain `floor` the pass started from.
	//
	// Seeding it at (floor, "") reproduces `time_updated >= floor` exactly, because
	// the keyset's second clause is `time_updated = floor AND id > ""` and every
	// real message id sorts after the empty string. (A row whose id were literally
	// "" AND whose time_updated were exactly the floor would be missed at that one
	// boundary; `id` is Opencode's TEXT PRIMARY KEY, and an empty or NULL id is
	// already dropped by the unreadable-row guard below, so no such row can reach
	// here.) Seeding at floor-1 would be the tempting alternative and is WRONG: it
	// widens the window by a millisecond, and on a floor of 0 it makes a
	// negative-stamped row match — which is exactly the corrupt shape the
	// zero-rows-matched drift alarm exists to detect.
	cur := rowCursor{timeUpdated: floor}
	var maxTS int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, st, next, err := c.readBatch(ctx, db, cur, targets, scopes, developer, sinceMS)
		if err != nil {
			return err
		}
		total.add(st)
		for _, ev := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := ing.Ingest(ctx, ev); err != nil {
				return fmt.Errorf("opencode: ingest failed (watermark NOT advanced; the un-ingested tail is re-read next pass): %w", err)
			}
		}
		// The watermark moves past a row whose spend was not recorded only once
		// the loss is in the store (#913-D9 ruling R-8).
		for i := 0; advance != nil && c.lost != nil && i < len(st.lost); i++ {
			if err := c.lost(ctx, st.lost[i], st.lost[i]); err != nil {
				return fmt.Errorf("opencode: record lost spend (watermark NOT advanced; re-read next pass): %w", err)
			}
		}
		if next.watermarkTS > maxTS {
			maxTS = next.watermarkTS
		}
		// The watermark advances per BATCH, not per pass, so a long backfill makes
		// durable progress instead of losing all of it to one late failure — and
		// only AFTER every event in the batch has been accepted by the sink.
		if advance != nil && maxTS > 0 {
			advance(maxTS, health.migrationCount)
		}
		if st.rowsRead < maxRowsPerPass {
			break
		}
		cur = next
	}

	c.reportPass(total, health)
	return nil
}

// rowCursor is the WITHIN-PASS read position: a keyset over (time_updated, id).
//
// 🔴 IT IS A KEYSET, NOT A HIGH-WATER TIMESTAMP, AND THAT IS NOT A REFINEMENT.
// `time_updated` is NOT unique — Opencode can write two rows in the same
// millisecond — so a cursor of "the largest time_updated I have seen" cannot
// express "…and I have already consumed three of the five rows at that
// millisecond". Continuing with `>= maxTS` re-reads the whole boundary group
// (measured: five rows through two-row batches emitted NINE events, four of them
// duplicates); continuing with `> maxTS` SKIPS the unread remainder of that group,
// which is silent capture loss. Only the composite key distinguishes the two.
//
// It also makes the loop terminate by construction: (time_updated, id) strictly
// increases every batch, so a tie group larger than one batch is PAGED THROUGH
// rather than spun on — which is why no "cannot advance" special case is needed.
//
// Duplicate emission would be a no-op at the store (the idempotency key collides),
// so this is about work and honesty rather than wrong money — but a pass that
// reports nine events for five rows is not one anybody can reason about.
type rowCursor struct {
	timeUpdated int64
	id          string
	// watermarkTS is the largest PLAUSIBLE time_updated the batch saw — the value
	// the persisted watermark may advance to. Separate from timeUpdated because
	// the cursor must step OVER a corrupt future-stamped row (or the pass never
	// finishes) while the watermark must not (or the resume point is poisoned
	// forever). See maxClockSkew.
	watermarkTS int64
}

// scanHealth is what one pass learned about the DATABASE, as opposed to about its
// rows. It exists so "0 events because nothing is new" and "0 events from a
// present, non-empty database" can be told apart — see reportPass.
type scanHealth struct {
	totalMessages  int
	migrationCount int
	// backfill is true when this pass started from no watermark at all, i.e. it
	// was entitled to see the whole store. Only such a pass can conclude anything
	// from having matched zero rows.
	backfill bool
}

// open opens the Opencode database READ-ONLY.
//
// ⛔ `mode=ro`, NEVER `immutable=1`. Opencode is a live process holding an active
// WAL. `immutable=1` promises SQLite the file cannot change, so it skips the WAL
// and the locking protocol entirely — it does not error, it returns pre-WAL or
// torn data with full confidence, which is a corruption-grade wrong answer in a
// program whose output is money. `mode=ro` takes the normal shared lock, reads
// through the WAL, and is REFUSED (SQLITE_READONLY) if anything tries to write —
// verified against the real database: `CREATE TABLE` returns
// "attempt to write a readonly database (8)".
//
// busy_timeout rides in the DSN rather than a post-Open Exec for the same reason
// internal/store does it (#63): it is connection-scoped, and database/sql
// transparently discards and recreates pooled connections.
//
// ⚠️ `mode=ro` still needs WRITE access to the `-shm` file (or its directory) on a
// WAL database — that is SQLite's WAL protocol, not something this code chooses.
// Fine for the same-user laptop case this is built for; a different-uid or
// read-only-mount deployment gets an open error every tick, and the error will
// name a permission problem rather than this sentence, so it is written down here.
func (c *Collector) open() (*sql.DB, error) {
	if _, err := os.Stat(c.dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Not an error: Opencode may simply never have run on this machine.
			// An operator who enabled the collector ahead of installing Opencode
			// gets a clean, named "disabled" line — not a startup failure that
			// would also take down the Claude Code capture path.
			return nil, errCollectorDisabled{path: c.dbPath}
		}
		return nil, fmt.Errorf("opencode: database %s: %w", logsafe.Str(c.dbPath), err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(c.dbPath))
	if err != nil {
		return nil, fmt.Errorf("opencode: open %s read-only: %w", logsafe.Str(c.dbPath), err)
	}
	// One connection. This is a bounded background reader, not a serving path,
	// and a single connection keeps the number of WAL readers we hold at one.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// readOnlyDSN builds the read-only `file:` URI for a database path.
//
// 🔴 THE PATH MUST BE URI-ESCAPED, AND `mode=ro` IS WHAT DEPENDS ON IT. `?`, `#`
// and `%` are URI metacharacters, and modernc.org/sqlite always asks SQLite for
// SQLITE_OPEN_READWRITE|SQLITE_OPEN_CREATE — so `mode=ro` in the query string is
// the ONLY thing standing between this collector and a writable handle on a
// third-party database. Interpolating the path raw lets it be dropped. Measured
// against the pinned driver, with the naive `fmt.Sprintf("file:%s?mode=ro…")`:
//
//	…/oc.db?mode=rwc&junk   → opened a DIFFERENT, newly CREATED database,
//	                          and `CREATE TABLE` SUCCEEDED (err = <nil>)
//	…/a#b.db                → `#b.db` parsed as a fragment, DISCARDING mode=ro;
//	                          opened and CREATED the file `…/a`, write succeeded
//	…/oc%41.db              → percent-decoded; opened `…/ocA.db`, a file the
//	                          operator never named
//
// The path is not "Opencode's own", which is what an earlier version of this
// comment claimed: it comes from `--opencode-db` or `collectors.opencode.db_path`,
// and that flag's own help text advertises it for a relocated XDG_DATA_HOME.
//
// Two further guarantees ride on the same escaping, and both were broken by the
// naive form: os.Stat validates the path we are ABOUT to open (rather than a
// different one), and the collector creates nothing.
//
// url.URL does the escaping in one place with the standard library's rules.
// OmitHost keeps the result `file:/abs/path` rather than `file:///abs/path`;
// SQLite accepts both, and the former round-trips a Windows drive letter without
// an empty authority component.
func readOnlyDSN(dbPath string) string {
	u := url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     dbPath,
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}
	return u.String()
}

// errCollectorDisabled marks the benign "Opencode is not installed here" case so
// callers can log it at INFO rather than ERROR.
type errCollectorDisabled struct{ path string }

func (e errCollectorDisabled) Error() string {
	return fmt.Sprintf("opencode: no database at %s; collector disabled", e.path)
}

// IsDisabled reports whether err is the benign absent-database condition (rather
// than a real failure), so a caller can log it at INFO.
func IsDisabled(err error) bool {
	var d errCollectorDisabled
	return errors.As(err, &d)
}

// checkSchema probes the shapes this collector depends on BEFORE reading a single
// row, so a schema change from an Opencode upgrade surfaces as a named error
// rather than as a scan that quietly matches nothing.
//
// 🔴 SCHEMA DRIFT MUST NEVER LOOK LIKE A CLEAN EMPTY SCAN. Every probe below
// fails the pass loudly. The one that is easy to get wrong is the LAST one: a
// `message` table that still exists, still has rows, and whose `data` blobs no
// longer carry the fields we read produces zero events and no error at all unless
// something counts the rows independently — which is what totalMessages is for.
// floor is the scan's own starting floor, NOT c.floor(): Collect always scans
// from zero regardless of what Run's watermark holds, and reading the watermark
// here would both make Collect's stateless contract false and mislabel a
// full-store Collect as non-backfill — silently disabling the two loud arms in
// reportPass that exist for exactly that scan.
func (c *Collector) checkSchema(ctx context.Context, db *sql.DB, floor int64) (scanHealth, error) {
	var h scanHealth
	// Opencode's own migration ledger. Its ABSENCE means we are not looking at an
	// Opencode database at all (a stale path, a different tool's file); its SIZE
	// is the drift tripwire carried in the checkpoint.
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM migration`).Scan(&h.migrationCount); err != nil {
		return h, fmt.Errorf("opencode: cannot read Opencode's own `migration` table in %s — this is either not an Opencode database or its schema changed incompatibly; refusing to report an empty scan as success: %w",
			logsafe.Str(c.dbPath), err)
	}
	if h.migrationCount == 0 {
		return h, fmt.Errorf("opencode: Opencode's `migration` table is EMPTY in %s; an initialized Opencode store always carries applied migrations, so this file is not one", logsafe.Str(c.dbPath))
	}
	// The exact projection the scan uses. A renamed column fails HERE, naming
	// itself, instead of at the first row read.
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT id, session_id, time_updated, data FROM message LIMIT 1)`).Scan(new(int)); err != nil {
		return h, fmt.Errorf("opencode: the `message` table in %s does not expose (id, session_id, time_updated, data) — Opencode's schema changed and this collector's field mapping must be re-checked before it can be trusted: %w",
			logsafe.Str(c.dbPath), err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM message`).Scan(&h.totalMessages); err != nil {
		return h, fmt.Errorf("opencode: cannot count rows in `message` in %s: %w", logsafe.Str(c.dbPath), err)
	}
	h.backfill = floor == 0
	return h, nil
}

// readBatch reads up to maxRowsPerPass message rows at or after the cursor, in
// (time_updated, id) order, and converts the eligible ones into TokenEvents. It
// returns the cursor to continue from — the last row READ, so the next batch
// resumes exactly after it.
//
// The rows are streamed and closed inside this call: the read transaction lives
// for exactly one batch and never spans an ingest. A reader held open across
// ingest would pin Opencode's WAL for as long as our own SQLite writes take,
// which would grow THEIR file.
func (c *Collector) readBatch(ctx context.Context, db *sql.DB, cur rowCursor, targets []scanTarget, scopes []*collector.RepoScope, developer string, sinceMS int64) ([]collector.TokenEvent, passStats, rowCursor, error) {
	var (
		st     passStats
		events []collector.TokenEvent
	)
	next := cur
	// plausibleTS is the largest time_updated this batch saw that is INSIDE the
	// clock horizon. It is what the persisted watermark may advance to —
	// deliberately not the cursor, which must step over a corrupt row so the pass
	// can finish. Two jobs, two values.
	var plausibleTS int64
	// The keyset predicate, matching the ORDER BY exactly — that correspondence is
	// what makes the paging exactly-once. `time_updated` is not indexed in
	// Opencode's schema (we cannot add an index to someone else's read-only
	// database), so this is a scan plus a sort; measured at 120 ms over the whole
	// 10,206-row table, and it degrades linearly.
	rows, err := db.QueryContext(ctx,
		`SELECT id, session_id, time_updated, data FROM message
		 WHERE time_updated > ? OR (time_updated = ? AND id > ?)
		 ORDER BY time_updated, id LIMIT ?`,
		cur.timeUpdated, cur.timeUpdated, cur.id, maxRowsPerPass)
	if err != nil {
		return nil, st, cur, fmt.Errorf("opencode: read message rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	horizon := c.horizonMS()
	for rows.Next() {
		// NULLABLE SCAN TARGETS, deliberately. A NULL or a TEXT in any of these
		// columns is a third-party data defect, and scanning into plain Go types
		// would return an error for the WHOLE batch — so one bad row would stop
		// every row after it from ever being ingested, on every pass, forever.
		// That contradicts the per-row posture the rest of this scan takes (see
		// checkAdditiveIdentity's note on row independence): a bad row must cost
		// exactly itself.
		var id, sessionID, data sql.NullString
		var timeUpdated sql.NullInt64
		if err := rows.Scan(&id, &sessionID, &timeUpdated, &data); err != nil {
			st.skippedUnreadableRow++
			c.logger.Warn("opencode: a message row could not be read into the expected column types; skipping it (its spend is NOT captured)",
				"err", logsafe.Err(err))
			continue
		}
		st.rowsRead++
		// The cursor tracks the last row READ, whatever became of it. A skipped row
		// must still be stepped over, or the next batch re-reads it forever.
		if id.Valid && timeUpdated.Valid {
			next.timeUpdated, next.id = timeUpdated.Int64, id.String
		}
		if !id.Valid || !timeUpdated.Valid || !data.Valid {
			if timeUpdated.Valid {
				st.lose(timeUpdated.Int64, sinceMS)
			}
			st.skippedUnreadableRow++
			c.logger.Warn("opencode: a message row has a NULL in a column this collector requires; skipping it (its spend is NOT captured)",
				"message_id", logsafe.Str(id.String), "id_null", !id.Valid, "time_updated_null", !timeUpdated.Valid, "data_null", !data.Valid)
			continue
		}
		// 🔴 A FUTURE TIMESTAMP MUST NOT MOVE THE WATERMARK. See maxClockSkew: a
		// single out-of-range value would pin the resume point beyond every real
		// row and be persisted there, and the collector would then report a clean
		// scan forever while capturing nothing. The CURSOR still steps over it
		// (above) so this pass finishes; only plausibleTS — the value the
		// watermark is derived from — excludes it. The row is dropped rather than
		// emitted, because a row whose own metadata is corrupt is not a row whose
		// numbers we are willing to bill.
		if timeUpdated.Int64 > horizon {
			st.skippedFutureStamp++
			continue
		}
		if timeUpdated.Int64 > plausibleTS {
			plausibleTS = timeUpdated.Int64
		}
		ev, ok := c.eventFor(id.String, sessionID.String, data.String, timeUpdated.Int64, targets, scopes, developer, sinceMS, horizon, &st)
		if ok {
			events = append(events, ev)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, st, cur, fmt.Errorf("opencode: iterate message rows: %w", err)
	}
	next.watermarkTS = plausibleTS
	return events, st, next, nil
}

// eventFor turns one message row into a TokenEvent, or explains in `st` why it
// did not. The ORDER of the gates below is deliberate and each one is commented
// with what it costs to get wrong.
func (c *Collector) eventFor(id, sessionID, data string, updatedMS int64, targets []scanTarget, scopes []*collector.RepoScope, developer string, sinceMS, horizonMS int64, st *passStats) (collector.TokenEvent, bool) {
	var zero collector.TokenEvent

	m, err := decodeMessage([]byte(data))
	if err != nil {
		st.lose(updatedMS, sinceMS)
		st.skippedUndecodable++
		c.logger.Warn("opencode: a message row's JSON did not decode; skipping it (its spend is NOT captured)",
			"message_id", logsafe.Str(id), "err", logsafe.Err(err))
		return zero, false
	}
	// Only assistant rows carry usage. User rows are the bulk of the remainder
	// and are not a diagnostic event.
	if m.Role != roleAssistant {
		st.nonAssistant++
		return zero, false
	}
	st.assistantRows++

	// 🔴 COMPLETED ONLY. See the package doc: an in-flight row carries partial
	// counts, and the store freezes cost_micro at whatever the first writer said
	// while continuing to MAX the token counters upward. Ingesting one produces a
	// permanently under-priced row that nothing will ever correct.
	if m.Time.Completed == nil {
		st.skippedIncomplete++
		return zero, false
	}

	decision, why := classifyProvider(m.ProviderID)
	if decision != providerPriced {
		st.excluded(m.ProviderID)
		if decision == providerExcludedUnknown {
			c.warnUnknownProviderOnce(m.ProviderID, why)
		}
		return zero, false
	}

	u := readUsage(m)
	// A row with no tokens at all prices to nothing and describes no API work —
	// an aborted or errored turn. Dropping it before the identity check keeps the
	// loud arm loud: 13 such rows exist in the real store, every one with
	// `tokens.total` absent, and warning about each would train an operator to
	// ignore the warning that matters.
	if !u.AnyNonZero() {
		st.skippedZeroToken++
		return zero, false
	}
	// Tokens but no `total` to check them against. NOT silently trusted: we would
	// be pricing a shape we cannot verify.
	// A row its cwd places outside every watched repo was never ours to lose.
	lose := func() {
		if collector.MatchScopes(scopes, m.Path.Cwd) >= 0 {
			if at := *m.Time.Completed; at > 0 && at <= horizonMS {
				updatedMS = at
			}
			st.lose(updatedMS, sinceMS)
		}
	}
	if !u.TotalPresent {
		lose()
		st.skippedUnverifiable++
		c.logger.Warn("opencode: a message carries token counts but no `tokens.total` to verify them against; skipping it (its spend is NOT captured). Opencode's schema may have changed",
			"message_id", logsafe.Str(id), "model", logsafe.Str(m.ModelID))
		return zero, false
	}
	if err := checkAdditiveIdentity(u); err != nil {
		lose()
		st.skippedIdentity++
		c.logger.Warn("opencode: a message's token counts do not satisfy the additive identity; skipping it (its spend is NOT captured)",
			"message_id", logsafe.Str(id), "model", logsafe.Str(m.ModelID), "err", logsafe.Err(err))
		return zero, false
	}

	// Repo scoping. A message whose cwd is outside every watched repo is DROPPED,
	// not attributed: cross-repo bleed would put another project's dollars on
	// this project's issues (#15).
	idx := collector.MatchScopes(scopes, m.Path.Cwd)
	if idx < 0 {
		st.skippedForeignRepo++
		return zero, false
	}

	// 🔴 THE COMPLETION STAMP BECOMES THE EVENT'S ts, WHICH EVERY WINDOW QUERY
	// READS. An unbounded value is not merely odd data: a far-future ts satisfies
	// `ts >= since` for every window from now until then, so a corrupt row's cost
	// is added to that developer's spend indefinitely, and the store's ON CONFLICT
	// clause holds ts INSERT-only (#235) so a corrected re-read cannot pull it
	// back. A non-positive stamp is refused for the mirror-image reason: it stores
	// as 1970-or-earlier and silently drops out of every window.
	completed := *m.Time.Completed
	if completed <= 0 || completed > horizonMS {
		st.lose(updatedMS, sinceMS)
		st.skippedImplausibleStamp++
		c.logger.Warn("opencode: a message's completion timestamp is outside the plausible range; skipping it (its spend is NOT captured)",
			"message_id", logsafe.Str(id), "time_completed_ms", completed, "horizon_ms", horizonMS)
		return zero, false
	}
	ts := time.UnixMilli(completed).UTC()
	if sinceMS > 0 && completed < sinceMS {
		st.skippedBeforeWindow++
		return zero, false
	}

	// 🔴 OutputTok = output + reasoning. The whole point of this collector; see
	// the package doc for the measurement and for why copying Codex is wrong.
	outputTok := u.Output + u.Reasoning

	// Host is the providerID VERBATIM, and that is not a shortcut — it is what a
	// host-qualified price row is keyed on ("<model>@zai-coding-plan"); with none,
	// the built-in model-only glm-5.3 row prices it per token (#786). Opencode
	// records no hostname at all, and a hostname could not distinguish the Z.ai
	// coding plan from the metered API.
	cost, billingMode := store.ComputeCostHost(m.ProviderID, m.ModelID, store.CostUsage{
		Input:     int(u.Input),
		Output:    int(outputTok),
		CacheRead: int(u.CacheRead),
		// No Opencode row has ever reported a cache write (measured: 0 of 9,267).
		// Passing it through anyway is correct if one ever appears: it prices at
		// the row's write multiplier, 1.0x input for both the embedded zai rows and
		// a self-hosted override row, since Z.ai publishes no cache-write rate.
		CacheWrite5m: int(u.CacheWrite),
	})
	// NEVER TRUST A ZERO. The client writes `cost: 0` on every row and this
	// collector does not even decode that field — but the same failure can arrive
	// from OUR side, if the price table has no row for this (host, model) and the
	// fallback rounds a small event to nothing. Real tokens priced at zero is the
	// exact shape that makes work read as FREE and inflates TIER, so it is
	// counted and named rather than quietly persisted.
	if cost == 0 {
		st.zeroCostWithTokens++
		c.logger.Warn("opencode: a message with real tokens priced to ZERO micro-dollars; it will be stored as free spend. Check that the price table carries a row for this model@provider",
			"message_id", logsafe.Str(id), "model", logsafe.Str(m.ModelID), "provider", logsafe.Str(m.ProviderID),
			"input", u.Input, "output", outputTok, "cache_read", u.CacheRead)
	}

	t := targets[idx]
	st.emitted++
	st.emittedTokens += u.Input + outputTok + u.CacheRead + u.CacheWrite
	st.emittedCostMicro += cost
	return collector.TokenEvent{
		Developer: developer,
		// 🔴 OPENCODE RECORDS NO GIT BRANCH — not in `message.data`, not on
		// `session`, and its `workspace` table (which HAS a branch column) is
		// empty. So there is nothing to resolve an issue from, and the nil
		// IssueResolver's fallback is used deliberately: it routes through the
		// SAME bucketForBranch rule every other collector uses, so this cannot
		// drift if the unattributed family changes, and it yields
		// unattributed:detached-head — whose documented meaning is exactly "a
		// message that recorded no branch at all".
		//
		// ⛔ Do NOT "improve" this by reading the CURRENT branch of the cwd at
		// scan time. That would attribute months-old spend to whatever branch
		// happens to be checked out today: a confident wrong number, which is
		// worse than an honest labelled bucket that still counts in the
		// developer's denominator.
		IssueID:        (*collector.IssueResolver)(nil).Resolve("", ts),
		Model:          m.ModelID,
		InputTok:       int(u.Input),
		OutputTok:      int(outputTok),
		CacheRead:      int(u.CacheRead),
		CacheWrite5m:   int(u.CacheWrite),
		CostMicro:      cost,
		Source:         collector.SourceOpencode,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: collector.IdempotencyKey(collector.SourceOpencode, m.ProviderID, id),
		Repo:           t.slug,
		SessionID:      sessionID,
		Host:           m.ProviderID,
		BillingMode:    billingMode,
		Timestamp:      ts,
	}, true
}

// scanTarget pairs a repo scope with its resolved slug.
type scanTarget struct {
	scope *collector.RepoScope
	slug  string
}

func scopesOf(targets []scanTarget) []*collector.RepoScope {
	out := make([]*collector.RepoScope, len(targets))
	for i, t := range targets {
		out[i] = t.scope
	}
	return out
}

// resolveTargets builds one memoizing scope per configured repo for this pass.
//
// NO IssueResolver IS BUILT, unlike codexrollout. That is not an omission: an
// IssueResolver is a `git log` snapshot indexed BY BRANCH, and Opencode records
// no branch, so every lookup would miss and the only thing achieved would be a
// `git log` per repo per five minutes producing nothing.
func (c *Collector) resolveTargets() []scanTarget {
	c.resolveSlugs()
	targets := make([]scanTarget, 0, len(c.repos))
	for i, r := range c.repos {
		targets = append(targets, scanTarget{scope: collector.NewRepoScope(r.Path), slug: c.slugs[i]})
	}
	return targets
}

// resolveSlugs resolves each target's canonical "owner/repo" slug exactly once
// per collector: operator override, else remote.origin.url, else the
// 'unqualified' sentinel. Degrading to the sentinel is deliberate rather than
// fatal (mirroring codexrollout): a repo we cannot NAME still produces true
// per-developer cost, and the primary TIER denominator groups by developer alone.
func (c *Collector) resolveSlugs() {
	c.slugOnce.Do(func() {
		c.slugs = make([]string, len(c.repos))
		for i, r := range c.repos {
			if slug, ok := repoid.Canonical(r.Slug); ok {
				c.slugs[i] = slug
				continue
			}
			if r.Slug != "" {
				c.logger.Warn("opencode: configured repo slug is not a canonical owner/repo; ignoring",
					"repo_path", logsafe.Str(r.Path), "configured", logsafe.Str(r.Slug))
			}
			if slug := collector.RepoSlugFromGitConfig(r.Path); slug != "" {
				c.slugs[i] = slug
				continue
			}
			c.slugs[i] = repoid.Unqualified
			c.logger.Warn("opencode: cannot determine repository slug; Opencode cost rows will be repo-unqualified and multi-repo issues sharing a number will fuse",
				"repo_path", logsafe.Str(r.Path),
				"hint", "set the per-repo `repo:` override, or add a remote.origin.url")
		}
	})
}

// logPolicyOnce emits the capture policy at startup: which providers are priced,
// which are excluded BY NAME and why.
//
// 🔴 THE ollama-cloud EXCLUSION IS ANNOUNCED HERE, unconditionally, before any
// row is read. Silence was explicitly not an option: an operator must be able to
// see that a large slice of their Opencode traffic is deliberately not in the
// numbers, rather than discover a hole in the totals later. The measured COUNT of
// what was excluded is reported after every pass by reportPass — the two halves
// together are the disclosure.
func (c *Collector) logPolicyOnce() {
	c.policyOnce.Do(func() {
		// The reasons are DERIVED from namedExclusions(), never hardcoded by key.
		// A third named exclusion added to providerPolicy would otherwise appear in
		// excluded_providers with no reason attached — which silently defeats the
		// "both halves together are the disclosure" contract this function exists
		// to keep, and a mistyped key would render as an empty string rather than
		// failing.
		c.logger.Info("opencode collector capture policy",
			"priced_providers", strings.Join(pricedProviders(), ","),
			"excluded_providers", strings.Join(namedExclusions(), ","),
			"excluded_why", exclusionReasons(),
			"unlisted_providers", "excluded and WARNed once each; no rate can be assumed for a route nobody has classified")
	})
}

// maxWarnedProviders bounds the unknown-provider WARN dedup set. providerID is a
// third-party string, so an unbounded map keyed on it grows for the process
// lifetime on a corrupt store. Mirrors maxUnknownModelWarn in internal/store,
// which bounds the identical shape for model names.
const maxWarnedProviders = 1024

func (c *Collector) warnUnknownProviderOnce(providerID, why string) {
	if _, loaded := c.warnedProviders.LoadOrStore(providerID, struct{}{}); loaded {
		return
	}
	if n := c.warnedProviderCount.Add(1); n > maxWarnedProviders {
		if c.warnSuppressed.CompareAndSwap(false, true) {
			c.logger.Warn("opencode: unclassified-provider WARNs capped; further never-before-seen providers are still skipped and counted, but will not be logged individually",
				"cap", maxWarnedProviders)
		}
		return
	}
	c.logger.Warn("opencode: skipping every message from an unclassified provider; its spend is NOT captured",
		"provider", logsafe.Str(providerID), "why", why,
		"hint", "add an audited <model>@<provider> row to the price table FIRST, then add the provider to internal/collector/opencode/providers.go")
}

// noteMigrationDrift compares Opencode's own migration-ledger size against the
// last one we recorded and reports a change. A new migration is normal (Opencode
// upgraded); it is reported because it is the moment this collector's field
// mapping stopped being verified against the shape it was written for.
func (c *Collector) noteMigrationDrift(now int) {
	c.mu.Lock()
	prev := c.migrationCount
	c.migrationCount = now
	c.mu.Unlock()
	if prev >= 0 && prev != now {
		c.logger.Warn("opencode: Opencode applied schema migrations since the last scan; its `message.data` field mapping is no longer verified against the shape this collector was written for",
			"migrations_before", prev, "migrations_now", now,
			"hint", "re-check tokens{total,input,output,reasoning,cache{read,write}} and time.completed before trusting new rows")
	}
}

// reportPass turns one pass's counters into log lines.
//
// 🔴 "0 EVENTS FROM A PRESENT, NON-EMPTY DATABASE" IS A DIFFERENT FACT FROM
// "NOTHING NEW", and this is where they are separated. A scan that matched no
// rows because the watermark is current is the ordinary quiet case and is logged
// at DEBUG. A BACKFILL pass — one entitled to see the entire store — that read
// zero rows, or read rows and found no assistant among them, means the shape we
// depend on is gone. That is an ERROR, because the alternative is a collector
// that reports a clean successful scan every five minutes forever while capturing
// nothing.
func (c *Collector) reportPass(st passStats, h scanHealth) {
	if h.backfill && h.totalMessages > 0 && st.rowsRead == 0 {
		c.logger.Error("opencode: a full backfill scan of a NON-EMPTY message table matched zero rows — `time_updated` is not the column this collector believes it is; refusing to call this a clean scan",
			"message_rows_in_db", h.totalMessages, "db", logsafe.Str(c.dbPath))
		return
	}
	// ⚠️ THE ROW FLOOR IS NOT DECORATION. A brand-new Opencode install whose only
	// rows are the user's turns — the assistant reply not yet written — has
	// rowsRead > 0 and assistantRows == 0 through no fault of anything, and would
	// otherwise trip the loudest arm this collector has with a message claiming the
	// role labelling changed. A false alarm on THIS arm is expensive: it is the
	// arm an operator is supposed to trust, and the control sub-test in
	// drift_test.go exists precisely to keep it quiet on healthy scans.
	if st.rowsRead >= minRowsForRoleAlarm && st.assistantRows == 0 && h.backfill {
		c.logger.Error("opencode: read message rows but found NO assistant messages — Opencode's role labelling changed and no spend can be captured until the mapping is updated",
			"rows_read", st.rowsRead, "db", logsafe.Str(c.dbPath))
		return
	}
	if st.emitted == 0 && st.rowsRead == 0 {
		c.logger.Debug("opencode scan: nothing new since the watermark", "message_rows_in_db", h.totalMessages)
		return
	}
	level := slog.LevelInfo
	attrs := []any{
		"rows_read", st.rowsRead,
		"assistant_rows", st.assistantRows,
		"events", st.emitted,
		"tokens", st.emittedTokens,
		"cost_micro", st.emittedCostMicro,
		"skipped_incomplete", st.skippedIncomplete,
		"skipped_zero_token", st.skippedZeroToken,
		"skipped_foreign_repo", st.skippedForeignRepo,
		"skipped_before_window", st.skippedBeforeWindow,
		"skipped_identity_violation", st.skippedIdentity,
		"skipped_unverifiable", st.skippedUnverifiable,
		"skipped_undecodable", st.skippedUndecodable,
		"skipped_unreadable_row", st.skippedUnreadableRow,
		"skipped_future_time_updated", st.skippedFutureStamp,
		"skipped_implausible_completion", st.skippedImplausibleStamp,
		"zero_cost_with_tokens", st.zeroCostWithTokens,
		"excluded_by_provider", st.excludedSummary(),
	}
	if st.skippedIdentity > 0 || st.skippedUnverifiable > 0 || st.skippedUndecodable > 0 ||
		st.skippedUnreadableRow > 0 || st.skippedFutureStamp > 0 ||
		st.skippedImplausibleStamp > 0 || st.zeroCostWithTokens > 0 {
		level = slog.LevelWarn
	}
	c.logger.Log(context.Background(), level, "opencode scan complete", attrs...)
}

// minRowsForRoleAlarm is how many message rows a backfill must have read before
// "no assistant messages among them" is evidence of drift rather than of a store
// that is simply new. A real Opencode store reaches this within one exchange.
const minRowsForRoleAlarm = 2

// passStats counts what one pass did with every row it read. Every skip has its
// own counter: a single "skipped" total cannot tell a healthy exclusion from a
// broken parse, and this collector's failure mode is precisely a quiet scan.
type passStats struct {
	rowsRead            int
	nonAssistant        int
	assistantRows       int
	emitted             int
	emittedTokens       int64
	emittedCostMicro    int64
	skippedIncomplete   int
	skippedZeroToken    int
	skippedForeignRepo  int
	skippedBeforeWindow int
	skippedIdentity     int
	skippedUnverifiable int
	skippedUndecodable  int
	// skippedUnreadableRow counts rows whose COLUMNS could not be read (a NULL or
	// a wrong type), as opposed to rows whose JSON could not be decoded.
	skippedUnreadableRow int
	// skippedFutureStamp counts rows whose `time_updated` is beyond the clock
	// horizon. Kept separate from skippedImplausibleStamp because they bound
	// different things: this one protects the WATERMARK, that one the event ts.
	skippedFutureStamp      int
	skippedImplausibleStamp int
	zeroCostWithTokens      int
	excludedByProvider      map[string]int
	// lost is when each in-window row this batch skipped without recording its
	// spend ran; add does not merge it, because each batch records its own.
	lost []time.Time
}

// lose notes a skipped row's spend as lost at ms, unless it ran before the
// window.
func (s *passStats) lose(ms, sinceMS int64) {
	if ms >= sinceMS {
		s.lost = append(s.lost, time.UnixMilli(ms).UTC())
	}
}

func (s *passStats) excluded(providerID string) {
	if s.excludedByProvider == nil {
		s.excludedByProvider = make(map[string]int)
	}
	s.excludedByProvider[providerID]++
}

func (s *passStats) add(o passStats) {
	s.rowsRead += o.rowsRead
	s.nonAssistant += o.nonAssistant
	s.assistantRows += o.assistantRows
	s.emitted += o.emitted
	s.emittedTokens += o.emittedTokens
	s.emittedCostMicro += o.emittedCostMicro
	s.skippedIncomplete += o.skippedIncomplete
	s.skippedZeroToken += o.skippedZeroToken
	s.skippedForeignRepo += o.skippedForeignRepo
	s.skippedBeforeWindow += o.skippedBeforeWindow
	s.skippedIdentity += o.skippedIdentity
	s.skippedUnverifiable += o.skippedUnverifiable
	s.skippedUndecodable += o.skippedUndecodable
	s.skippedUnreadableRow += o.skippedUnreadableRow
	s.skippedFutureStamp += o.skippedFutureStamp
	s.skippedImplausibleStamp += o.skippedImplausibleStamp
	s.zeroCostWithTokens += o.zeroCostWithTokens
	for k, v := range o.excludedByProvider {
		if s.excludedByProvider == nil {
			s.excludedByProvider = make(map[string]int)
		}
		s.excludedByProvider[k] += v
	}
}

// maxRenderedProviders bounds how many providers one exclusion summary names.
const maxRenderedProviders = 16

// excludedSummary renders the per-provider exclusion counts, highest count first,
// so the disclosure the policy INFO promises is actually readable in the log.
func (s *passStats) excludedSummary() string {
	if len(s.excludedByProvider) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(s.excludedByProvider))
	for id := range s.excludedByProvider {
		ids = append(ids, id)
	}
	// By descending count, then by name for stability. Ordering by COUNT is what
	// makes truncation safe: the providers worth seeing are the ones excluding the
	// most rows, and they survive the cut.
	sort.Slice(ids, func(i, j int) bool {
		ci, cj := s.excludedByProvider[ids[i]], s.excludedByProvider[ids[j]]
		if ci != cj {
			return ci > cj
		}
		return ids[i] < ids[j]
	})
	// BOUNDED. logsafe.Str caps each rendered VALUE but not how many there are,
	// and providerID is a third-party string — so an unbounded join emits one
	// O(distinct-providers) log record every pass on a corrupt store.
	shown := ids
	var overflow string
	if len(shown) > maxRenderedProviders {
		overflow = fmt.Sprintf(" ...and %d more", len(ids)-maxRenderedProviders)
		shown = shown[:maxRenderedProviders]
	}
	parts := make([]string, 0, len(shown))
	for _, id := range shown {
		parts = append(parts, fmt.Sprintf("%s=%d", logsafe.Str(id), s.excludedByProvider[id]))
	}
	return strings.Join(parts, " ") + overflow
}

// ── watermark persistence ────────────────────────────────────────────────────

func (c *Collector) floor() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watermark == 0 {
		return 0
	}
	lag := int64(watermarkLagFactor) * c.interval.Milliseconds()
	if c.watermark <= lag {
		return 0
	}
	return c.watermark - lag
}

// loadWatermark seeds the in-memory watermark from watcher_checkpoint. Every
// failure here degrades to "no watermark" — a full, idempotent re-scan — because
// the checkpoint is a derived cache and a wrong resume is the only outcome worth
// preventing.
func (c *Collector) loadWatermark(ctx context.Context) {
	if c.checkpoints == nil {
		return
	}
	cp, ok, err := c.checkpoints.LoadWatcherCheckpoint(ctx, c.checkpointKey())
	if err != nil {
		c.logger.Warn("opencode: cannot load the scan watermark; this pass re-scans from the beginning (idempotent, just slower)", "err", logsafe.Err(err))
		return
	}
	if ok {
		var md checkpointMetadata
		if err := json.Unmarshal([]byte(cp.Metadata), &md); err != nil || md.Schema != checkpointSchema {
			c.logger.Warn("opencode: the persisted scan watermark is unreadable or of an unknown schema; re-scanning from the beginning",
				"schema", md.Schema, "want_schema", checkpointSchema)
			return
		}
		// 🔴 A PERSISTED WATERMARK BEYOND THE HORIZON IS REFUSED, not resumed.
		// The in-scan guard stops a bad row from creating one, but a checkpoint
		// written by an older binary (or corrupted in place) would otherwise pin
		// the resume point past every real row FOREVER — setWatermark only moves
		// forward. The checkpoint is a derived cache, so refusing it costs one
		// idempotent re-scan and nothing else, which makes this the cheap side of
		// a very asymmetric trade.
		if horizon := c.horizonMS(); md.TimeUpdatedMS > horizon {
			c.logger.Error("opencode: the persisted scan watermark is beyond the plausible clock horizon and is being DISCARDED; re-scanning from the beginning. A watermark this far ahead would have silently captured nothing while reporting healthy scans",
				"watermark_ms", md.TimeUpdatedMS, "horizon_ms", horizon)
			return
		}
		c.mu.Lock()
		c.watermark = md.TimeUpdatedMS
		c.migrationCount = md.MigrationCount
		c.mu.Unlock()
		c.logger.Info("opencode: resuming from the persisted scan watermark",
			"time_updated_ms", md.TimeUpdatedMS,
			"resume_at", time.UnixMilli(md.TimeUpdatedMS).UTC().Format(time.RFC3339))
	}
}

// setWatermark advances the in-memory watermark and persists it. It only ever
// moves FORWARD: a pass that read an older row must not drag the resume point
// backwards and re-emit the world.
func (c *Collector) setWatermark(ctx context.Context, ts int64, migrations int) {
	c.mu.Lock()
	if ts <= c.watermark {
		c.mu.Unlock()
		return
	}
	c.watermark = ts
	c.mu.Unlock()

	if c.checkpoints == nil {
		return
	}
	md, err := json.Marshal(checkpointMetadata{
		Schema:         checkpointSchema,
		TimeUpdatedMS:  ts,
		MigrationCount: migrations,
	})
	if err != nil {
		c.logger.Warn("opencode: cannot encode the scan watermark; it will not survive a restart", "err", logsafe.Err(err))
		return
	}
	// The unused fields are zero on purpose: inode/byte_offset/head_crc describe a
	// FILE tail, and this checkpoint describes a database row watermark. The
	// collector owns `metadata`, which is where the meaning lives.
	if err := c.checkpoints.SaveWatcherCheckpoint(ctx, store.WatcherCheckpoint{
		Path:     c.checkpointKey(),
		Metadata: string(md),
	}); err != nil {
		c.logger.Warn("opencode: cannot persist the scan watermark; a restart will re-scan from the beginning (idempotent, just slower)", "err", logsafe.Err(err))
	}
}

// checkpointKey namespaces this collector's row in watcher_checkpoint, whose
// primary key is a PATH shared with the JSONL watcher. A bare database path could
// in principle collide with a watched session file of the same name; the prefix
// makes that impossible and makes the row self-describing in a manual query.
// The scope is part of the cache key: adding a repo must backfill rows the
// previous scope excluded, even when they precede the database watermark.
// Sorting makes configuration order irrelevant. Legacy DB-only keys are not
// resumed, so upgrading costs one idempotent backfill without a schema change.
func (c *Collector) checkpointKey() string {
	scope := make([]string, len(c.repos))
	for i, repo := range c.repos {
		path, err := filepath.Abs(repo.Path)
		if err != nil {
			path = filepath.Clean(repo.Path)
		}
		scope[i] = fmt.Sprintf("%q:%q", path, repo.Slug)
	}
	sort.Strings(scope)
	raw, _ := json.Marshal(scope) // a slice of strings cannot fail to marshal
	return fmt.Sprintf("opencode-db:%s:%x", c.dbPath, sha256.Sum256(raw))
}
