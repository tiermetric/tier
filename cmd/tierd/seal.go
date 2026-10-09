package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// sealServeFlags are the serve settings sealing reads (#913-D5 ruling C′).
// serve and `tierd seal` both register them with addSealServeFlags and resolve
// them with applyConfig and resolve, so the two open the same database under
// the same price table, aggregation, k, grace, read-only and seal_from.
type sealServeFlags struct {
	config, db, prices, aggregation, sealFrom *string
	k                                         *int
	grace                                     *time.Duration
	readOnly                                  *bool
	// sealFromConfig is set when applyConfig took seal_from from the config file,
	// which fs.Visit would otherwise report as a CLI flag.
	sealFromConfig *bool
}

func addSealServeFlags(fs *flag.FlagSet) sealServeFlags {
	return sealServeFlags{
		config:         fs.String("config", "", "path to YAML config file (#29); CLI flags + env vars override its values"),
		db:             fs.String("db", defaultDBPath(), "SQLite database path"),
		prices:         fs.String("prices", os.Getenv("TIER_PRICES"), "path to a price-table YAML override (#68); empty uses the embedded default. A bad file fails startup"),
		readOnly:       fs.Bool("read-only", envBool("TIER_READ_ONLY"), "public-demo mode (#429): mount ONLY the read + health routes; every write/ingest/admin route, the GitHub webhook, the proxies, the watcher, and the pollers are STRUCTURALLY ABSENT (404 / not started), not merely token-gated — so a leaked token cannot reach any mutation. Governs WRITES only: reads stay OPEN per the token/aggregation config, so public exposure is safe ONLY on synthetic data (e.g. `tierd demo`) or with a read-token + k-anonymized aggregation. The webhook payload retention prune still runs, at every database open and daily, and DELETES webhook_payloads rows past its age limit or row cap (unexpired rows too, once the table is over the cap), so the database is not write-free. For the community demo behind a rate-limiter. OFF by default. Env TIER_READ_ONLY"),
		aggregation:    fs.String("aggregation", os.Getenv("TIER_AGGREGATION"), "REQUIRED reporting mode (#185, #270): 'team' emits only team-level aggregates and 'division' rolls up one level higher to divisions — both k-anonymized and NEVER naming an individual (the safe posture under EU works-council / GDPR Art. 22 co-determination — Germany §87 BetrVG, France, NL); 'developer' keeps named per-developer rows. NO default — serve FAILS to start when unset from flag/env/config, so an existing deployment's behavior never changes silently. Env TIER_AGGREGATION, config key aggregation"),
		grace:          fs.Duration("report-grace", envDurationDefault("TIER_REPORT_GRACE", defaultReportGrace), fmt.Sprintf("in team/division mode, how long after a calendar month closes before serve's background sealer seals it (#913). A Go duration (336h is 14 days); minimum %s. Env TIER_REPORT_GRACE, config key report_grace", minReportGrace)),
		k:              fs.Int("k-anonymity", envIntDefault("TIER_K_ANONYMITY", scoring.DefaultKAnonymity), "k-anonymity cohort floor for the anonymized aggregation modes --aggregation team|division (#185, #270): a group with fewer than this many CONTRIBUTING developers collapses into an aggregate 'other' bucket so no sub-k cohort is identifiable. Default 5; HARD minimum 3 (serve refuses a smaller value). Env TIER_K_ANONYMITY, config key k_anonymity"),
		sealFromConfig: new(bool),
		sealFrom:       fs.String("seal-from", os.Getenv("TIER_SEAL_FROM"), "in team/division mode, the calendar month (YYYY-MM) sealing is armed from while no month is sealed; after the first seal it has no effect. Prefer 'tierd seal --arm' once the history is backfilled: arming is irreversible. Env TIER_SEAL_FROM, config key seal_from"),
	}
}

// envSet is envVarSet's entries for these flags: whether each picked up a
// non-empty env default, which a config value must not override.
func (sealServeFlags) envSet() map[string]bool {
	return map[string]bool{
		"prices":       os.Getenv("TIER_PRICES") != "",
		"aggregation":  os.Getenv("TIER_AGGREGATION") != "",
		"k-anonymity":  os.Getenv("TIER_K_ANONYMITY") != "",
		"report-grace": os.Getenv("TIER_REPORT_GRACE") != "",
		"seal-from":    os.Getenv("TIER_SEAL_FROM") != "",
	}
}

// applyConfig applies cfg to the flags neither the CLI nor env set.
func (f sealServeFlags) applyConfig(fs *flag.FlagSet, setFlags, envVarSet map[string]bool, cfg *config.Config) {
	applyStringFromConfig(fs, setFlags, envVarSet, "db", cfg.DB)
	applyStringFromConfig(fs, setFlags, envVarSet, "prices", cfg.PricesFile)
	applyStringFromConfig(fs, setFlags, envVarSet, "aggregation", cfg.Aggregation)
	applyIntFromConfig(fs, setFlags, envVarSet, "k-anonymity", cfg.KAnonymity)
	applyDurationFromConfig(fs, setFlags, envVarSet, "report-grace", cfg.ReportGrace)
	applyStringFromConfig(fs, setFlags, envVarSet, "seal-from", cfg.SealFrom)
	*f.sealFromConfig = !setFlags["seal-from"] && !envVarSet["seal-from"] && cfg.SealFrom != nil
}

// sealSettings are sealServeFlags resolved and validated.
type sealSettings struct {
	mode  scoring.AggregationMode
	k     int
	grace time.Duration
	// sealFrom is "" when unset; sealFromSource names where it was set.
	sealFrom, sealFromSource string
	readOnly                 bool
}

// resolve validates the flags after config resolution. The aggregation, k and
// grace refusals are serve's own; a seal_from that is not YYYY-MM is refused.
func (f sealServeFlags) resolve(fs *flag.FlagSet) (sealSettings, error) {
	mode, err := resolveAggregationMode(*f.aggregation)
	if err != nil {
		return sealSettings{}, err
	}
	if err := validateKAnonymity(*f.k); err != nil {
		return sealSettings{}, err
	}
	if err := validateReportGrace(*f.grace); err != nil {
		return sealSettings{}, err
	}
	s := sealSettings{mode: mode, k: *f.k, grace: *f.grace, sealFrom: *f.sealFrom, readOnly: *f.readOnly}
	if s.sealFrom == "" {
		return s, nil
	}
	if _, err := api.ParseSealMonth(s.sealFrom); err != nil {
		return sealSettings{}, fmt.Errorf("--seal-from: %w (#913)", err)
	}
	s.sealFromSource = "env TIER_SEAL_FROM"
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "seal-from" {
			s.sealFromSource = "flag --seal-from"
		}
	})
	if *f.sealFromConfig {
		s.sealFromSource = "config seal_from"
	}
	return s, nil
}

// logSealArm logs, at startup in team/division mode, where sealing is armed
// from: the floor the first seal pinned, else seal_from and where it was set.
// It warns when seal_from names a month other than the pinned floor, which it
// can no longer move. A read-only server never seals, so it logs only that.
func logSealArm(ctx context.Context, db *store.DB, s sealSettings, logger *slog.Logger) error {
	if !s.mode.Anonymized() {
		return nil
	}
	if s.readOnly {
		logger.Info("sealing: this read-only server never seals; it serves the months a writable server sealed (#913)")
		return nil
	}
	start, pinned, err := db.SealFloor(ctx)
	switch {
	case err != nil:
		return err
	case pinned:
		floor := start.UTC().Format("2006-01")
		logger.Info("sealing armed; the background sealer seals each month once its grace lag ends (#913)", "floor", floor, "source", "the first seal")
		if s.sealFrom != "" && s.sealFrom != floor {
			logger.Warn("seal_from has no effect: sealing is armed and its earliest month is pinned, which never moves (#913)",
				"seal_from", s.sealFrom, "source", s.sealFromSource, "floor", floor)
		}
	case s.sealFrom != "":
		logger.Info("sealing armed by seal_from; the background sealer seals each month once its grace lag ends (#913)", "seal_from", s.sealFrom, "source", s.sealFromSource)
	default:
		logger.Info("sealing not armed: no month is sealed until `tierd seal --arm YYYY-MM|earliest` is run (#913)")
	}
	return nil
}

// sealOverdueReader is what logSealOverdue reads from serve's handler.
type sealOverdueReader interface {
	SealStalled(ctx context.Context, grace time.Duration, sealFrom string) (api.SealOverdue, error)
	SealsInBackground(readOnly bool) bool
}

// logSealOverdue warns at startup in team/division mode when the database shows
// a month owed past its deadline (#913-D6 ruling D, D8 ruling C), naming it.
// Only a serve whose background sealer runs (the condition that starts it)
// gets the `tierd seal --skip` command, for if the first pass cannot seal the
// month; its --reason is empty, which the command refuses, until the operator
// writes one. The month is the database's, formatted by api.Period.
func logSealOverdue(ctx context.Context, h sealOverdueReader, s sealSettings, logger *slog.Logger) error {
	st, err := h.SealStalled(ctx, s.grace, s.sealFrom)
	if err != nil || st.Month == "" {
		return err
	}
	const owed = "a month is owed a seal and past its deadline, and no later month seals until it does (#913)"
	args := []any{"month", st.Month, "owed_since", st.OwedSince}
	switch {
	case s.readOnly:
		logger.Warn(owed+"; this read-only server never seals it", args...)
	case !h.SealsInBackground(s.readOnly):
		logger.Warn(owed+"; this server's background sealer does not run, so it does not seal it", args...)
	case st.First:
		logger.Warn(owed+"; serve's first seal pass retries it now, and it is the first month sealed, which cannot be skipped", args...)
	default:
		logger.Warn(owed+"; serve's first seal pass retries it now. If it stays stalled, record it as a gap by running this "+
			"command with serve's --config, flags and env, writing why between the quotes after --reason",
			append(args, "command", sealSkipCommand(st.Month))...)
	}
	return nil
}

// sealConfigGapReader is what logSealConfigGap reads from serve's handler.
type sealConfigGapReader interface {
	SealConfigGap(ctx context.Context) (api.SealConfigGap, error)
}

// logSealConfigGap warns at startup when the current config differs from the
// one the newest sealed month was sealed under (#913-D1 ruling A), naming the
// first month the current config applies to.
func logSealConfigGap(ctx context.Context, h sealConfigGapReader, logger *slog.Logger) error {
	gap, err := h.SealConfigGap(ctx)
	if err != nil || gap.Newest == "" {
		return err
	}
	logger.Warn("the current config differs from the one the newest sealed month was sealed under: sealed months keep "+
		"theirs, the current config applies from the first month sealed after them, and /scores/compare refuses a pair "+
		"of months across the change (#913)",
		"newest_sealed", gap.Newest, "sealed_config", gap.Sealed, "current_config", gap.Current, "current_from", gap.From)
	return nil
}

// tierd seal's exit codes (#913-D5 ruling C′, #913-D6 ruling D, #913-D8
// ruling C); sealExitCodes states what each means for --arm, --skip and
// --status, and the usage text is generated from it.
const (
	sealExitArmed        = 0
	sealExitRefused      = 1
	sealExitCannotRun    = 2
	sealExitAlreadyArmed = 3
	sealExitStalled      = 4
	sealExitStallUnknown = 5
	// sealExitSkipped, sealExitNotStalled and sealExitHelp share 0 with
	// sealExitArmed; the 0 row says so.
	sealExitSkipped    = 0
	sealExitNotStalled = 0
	sealExitHelp       = 0
)

var sealExitCodes = []struct {
	code              int
	arm, skip, status string
}{
	{sealExitArmed,
		"armed: this call sealed the first month and pinned it as the earliest sealed month (with --dry-run: it would, and nothing was sealed or pinned); --help also exits 0",
		"skipped: the gap is recorded and the months after it seal; or the re-attempted seal sealed the month, so no gap was needed and none was recorded. With --dry-run: nothing was written",
		"not stalled: no owed month is sealable yet and --server, when given, was reached and names no stalled month; or the first owed month is sealable, not overdue, and --server was reached and names no stalled month"},
	{sealExitRefused,
		"refused: developer mode, --read-only, a malformed month, a config file or --prices table that cannot be read or holds a bad value, no cost data, a month not sealable now or held by a configured source (not settled past it, before its first recorded coverage, or no serve has registered its sources yet), a month other than the one confirmed, or no confirmation; nothing was sealed or pinned",
		"refused: developer mode, --read-only, a malformed month, a missing or bad --reason, an unreadable config or --prices, sealing not armed or no month sealed yet, a month that is sealed, a gap, open, inside its grace lag or not the first owed month, a failure other than the one confirmed, or no confirmation; no gap was recorded",
		"refused: an unknown flag or an unexpected argument, a flag given with --status that does not apply to it, developer mode, a config file or --prices table that cannot be read, a bad --aggregation, --k-anonymity, --report-grace or --seal-from, or a database at a schema version other than this binary's; nothing was written"},
	{sealExitCannotRun,
		"could not run: the database could not be opened or read, or the seal failed; nothing was sealed or pinned",
		"could not run: the database could not be opened or read, or the re-attempted seal failed in a way a later pass can clear (the write lock was busy, an erasure raced, the clock is behind, the earliest sealable month moved, another process sealed a different month first, the month is not sealable, the run was cancelled, or SQLite reported busy, locked, out of memory, an I/O error or a full disk); no gap was recorded",
		"could not run: the database could not be opened or read, including where its directory is not writable and SQLite has no -wal/-shm files beside it yet"},
	{sealExitAlreadyArmed,
		"already armed: a month was pinned before this call, by an earlier arm or a running server, and is printed; nothing was sealed or pinned",
		"", ""},
	{sealExitStalled, "", "",
		"stalled: --server was reached and names the database's first owed month, sealable now, as stalled; the reason it reports is printed"},
	{sealExitStallUnknown, "", "",
		"unknown: the first owed month is sealable and --server was not given; or --server was given and was not reached, refused the token, sent a manifest this binary does not read, serves no sealed months, names a stall other than the database's first owed month, names one while the database shows no month sealable, or names none while the database shows the month overdue. It may be stalled; when the database shows the month overdue, its skip command is still printed"},
}

func sealUsage(fs *flag.FlagSet, w io.Writer) func() {
	return func() {
		_, _ = fmt.Fprintln(w, "usage: tierd seal --arm YYYY-MM|earliest [--dry-run] [--yes] [serve's --config/--db/... flags]")
		_, _ = fmt.Fprintln(w, "       tierd seal --skip YYYY-MM --reason TEXT [--dry-run] [--yes] [serve's flags]")
		_, _ = fmt.Fprintln(w, "       tierd seal --status [--server URL [--api-token TOKEN]] [serve's flags]")
		_, _ = fmt.Fprintln(w, "--arm seals the first month and pins it as the earliest month ever sealed (#913). Irreversible:")
		_, _ = fmt.Fprintln(w, "run it once the history is backfilled, with the same --config, flags and env as tierd serve.")
		_, _ = fmt.Fprintln(w, "--skip records the month sealing is stuck at as a permanent gap, so the months after it seal.")
		_, _ = fmt.Fprintln(w, "It first tries the seal again, and records no gap if that seals the month or fails in a way a")
		_, _ = fmt.Fprintln(w, "later pass can clear. A month held because a configured source has not settled past it is")
		_, _ = fmt.Fprintln(w, "skipped too, once confirmed: the failure it names is that wait. Irreversible: the month is never sealed.")
		_, _ = fmt.Fprintln(w, "--status prints the sealing state: it opens the database read-only, never writing the database file,")
		_, _ = fmt.Fprintln(w, "though SQLite may create empty -wal/-shm files beside it (so its directory must be writable while")
		_, _ = fmt.Fprintln(w, "SQLite has no -wal/-shm files there yet), and,")
		_, _ = fmt.Fprintln(w, "with --server, reads the stall reason from GET /api/v1/report_manifest.")
		_, _ = fmt.Fprintln(w, "--arm and --skip without --dry-run open the database, applying pending schema migrations and the webhook payload prune.")
		_, _ = fmt.Fprintln(w, "With --dry-run they open the database read-only: no migrations and no webhook payload prune.")
		for _, verb := range []string{"--arm", "--skip", "--status"} {
			_, _ = fmt.Fprintf(w, "Exit codes (%s):\n", verb)
			for _, c := range sealExitCodes {
				if meaning := map[string]string{"--arm": c.arm, "--skip": c.skip, "--status": c.status}[verb]; meaning != "" {
					_, _ = fmt.Fprintf(w, "  %d  %s\n", c.code, meaning)
				}
			}
		}
		fs.PrintDefaults()
	}
}

func runSealCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	arm := fs.String("arm", "", "seal this month first and pin it as the earliest sealed month: YYYY-MM, or "+api.SealFromEarliest+" for the first full month of cost data. The later of it and that month is sealed")
	skip := fs.String("skip", "", "record this month (YYYY-MM), the first owed month, as a permanent gap that is never sealed, after its seal is tried again and fails")
	reason := fs.String("reason", "", fmt.Sprintf("with --skip: why the month is skipped, up to %d characters of printable text; served to every reader of the month, permanent and never erasable, so it must not name a person", store.MaxSealedGapReason))
	dryRun := fs.Bool("dry-run", false, "print what the command would do and write nothing: no seal, no pin, no gap. With --skip it names the seal's failure when computing the seal shows it; otherwise it would retry the seal first, and a clean retry seals the month")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	status := fs.Bool("status", false, "print the sealing state from the database, opened read-only: armed, pinned floor, newest sealed or gapped month, first owed month, overdue, and the stall reason tierd serve reports")
	server := fs.String("server", "", "with --status: tierd serve's base URL, to read the stall reason from its GET /api/v1/report_manifest")
	// Default "" on purpose: PrintDefaults would echo an env default, the token.
	apiToken := fs.String("api-token", "", "with --status: the API token sent to --server as Authorization: Bearer (a read token is enough). Default: env TIER_API_TOKEN; @/path/to/file reads it from a file")
	sf := addSealServeFlags(fs)
	fs.Usage = sealUsage(fs, stderr)
	nothing := "nothing was sealed or pinned"
	refused := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "tierd seal: refused: "+format+"; "+nothing+"\n", a...)
		return sealExitRefused
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return sealExitHelp
		}
		return sealExitRefused
	}
	if fs.NArg() > 0 {
		return refused("unexpected argument %s", logsafe.Str(fs.Arg(0)))
	}
	if *apiToken == "" {
		*apiToken = os.Getenv("TIER_API_TOKEN")
	}
	switch {
	case *status:
		nothing = "nothing was written"
		if *arm != "" || *skip != "" || *reason != "" || *dryRun || *yes {
			return refused("--arm, --skip, --reason, --dry-run and --yes are not given with --status")
		}
	case *server != "":
		return refused("--server is given only with --status")
	case *arm == "" && *skip == "":
		return refused("--arm YYYY-MM|%s or --skip YYYY-MM is required", api.SealFromEarliest)
	case *arm != "" && *skip != "":
		return refused("--arm and --skip cannot be given together")
	case *arm != "" && *reason != "":
		return refused("--reason is given only with --skip")
	case *skip != "":
		nothing = "no gap was recorded"
		if *reason == "" {
			return refused("--skip needs --reason")
		}
	}
	if *sf.config != "" {
		cfg, err := config.Load(*sf.config)
		if err != nil {
			return refused("config: %v", logsafe.Err(err))
		}
		setFlags := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
		sf.applyConfig(fs, setFlags, sf.envSet(), cfg)
	}
	settings, err := sf.resolve(fs)
	if err != nil {
		return refused("%v", logsafe.Err(err))
	}
	if settings.readOnly && !*status {
		return refused("--read-only (TIER_READ_ONLY) is set, and a read-only server never seals")
	}
	if !settings.mode.Anonymized() {
		return refused("--aggregation developer: developer mode never seals")
	}
	cannotRun := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "tierd seal: could not run: "+format+"; "+nothing+"\n", a...)
		return sealExitCannotRun
	}
	// store.Open would create a missing file and report "no cost data" about it.
	if _, err := os.Stat(*sf.db); err != nil {
		return cannotRun("--db %s: %v", logsafe.Str(*sf.db), logsafe.Err(err))
	}
	if *sf.prices != "" {
		if _, err := store.LoadPriceTable(*sf.prices); err != nil {
			return refused("--prices: %v", logsafe.Err(err))
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *status {
		return runSealStatus(ctx, *sf.db, settings, *server, *apiToken, stdout, refused, cannotRun)
	}
	openStore := store.Open
	if *dryRun {
		openStore = store.OpenReadOnly // No migrations or boot-time prune on a dry run.
	}
	db, err := openStore(*sf.db)
	if err != nil {
		return cannotRun("open db: %v", logsafe.Err(err))
	}
	defer func() { _ = db.Close() }()
	if n := db.UpgradeNotice(); n != "" {
		_, _ = fmt.Fprintln(stderr, n)
	}
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{}, api.WithCommit(commit))
	h.SetAggregation(settings.mode, settings.k)
	if *skip != "" {
		return runSealSkip(ctx, h, settings, *skip, *reason, *dryRun, *yes, stdin, stdout, refused, cannotRun)
	}

	armOnce := func(confirm string) (api.SealArm, int) {
		got, err := h.ArmSealing(ctx, settings.grace, *arm, confirm)
		switch {
		case errors.Is(err, api.ErrSealArmRefused):
			return got, refused("%v", logsafe.Err(err))
		case err != nil:
			return got, cannotRun("%v", logsafe.Err(err))
		case got.Pinned != "":
			_, _ = fmt.Fprintf(stdout, "tierd seal: already armed: the earliest sealed month is pinned at %s; nothing was sealed or pinned\n", got.Pinned)
			return got, sealExitAlreadyArmed
		}
		return got, -1
	}
	plan, rc := armOnce("")
	if rc >= 0 {
		return rc
	}
	_, _ = fmt.Fprintf(stdout, "tierd seal: will seal %s at aggregation=%s k=%d report_grace=%s prices=v%d and pin it as the earliest month ever sealed; no earlier month is ever sealed, and the pin cannot be undone\n",
		plan.Month, settings.mode, settings.k, settings.grace, store.ActivePriceTableInfo().Version)
	if *dryRun {
		return sealExitArmed
	}
	if !*yes {
		_, _ = fmt.Fprint(stdout, "Is the history backfilled, and are these serve's settings? Should sealing start at "+plan.Month+"? [y/N] ")
		if !confirmed(ctx, stdin) {
			return refused("not confirmed")
		}
	}
	done, rc := armOnce(plan.Month)
	if rc >= 0 {
		return rc
	}
	_, _ = fmt.Fprintf(stdout, "tierd seal: armed: sealed %s and pinned it as the earliest sealed month\n", done.Month)
	return sealExitArmed
}

// runSealSkip is `tierd seal --skip` once the database is open (#913-D6 ruling D).
func runSealSkip(ctx context.Context, h *api.Handler, settings sealSettings, month, reason string, dryRun, yes bool,
	stdin io.Reader, stdout io.Writer, refused, cannotRun func(string, ...any) int) int {
	skipOnce := func(dry, record bool, confirm string) (api.SealSkip, int) {
		got, err := h.SkipSealing(ctx, settings.grace, settings.sealFrom, month, reason, dry, record, confirm)
		switch {
		case errors.Is(err, api.ErrSealSkipRefused):
			return got, refused("%v", logsafe.Err(err))
		case err != nil:
			return got, cannotRun("%v", logsafe.Err(err))
		case got.Sealed:
			_, _ = fmt.Fprintf(stdout, "tierd seal: sealed %s, no gap needed: its seal was tried again and succeeded\n", got.Month)
			return got, sealExitSkipped
		}
		return got, -1
	}
	// Outside --dry-run the plan is a real retry that records no gap, so the
	// category the operator confirms is one a seal actually failed with.
	plan, rc := skipOnce(dryRun, false, "")
	if rc >= 0 {
		return rc
	}
	if plan.Category != "" {
		_, _ = fmt.Fprintf(stdout, "tierd seal: the seal of %s fails: %s (category %s): %s\n", plan.Month, plan.Words, plan.Category, logsafe.Err(plan.Observed))
	} else {
		_, _ = fmt.Fprintf(stdout, "tierd seal: computing the seal of %s succeeds, so the seal will be retried first; a clean retry seals the month and records no gap, and a retry that fails in a way no later pass clears records the gap\n", plan.Month)
	}
	_, _ = fmt.Fprintf(stdout, "tierd seal: will record %s as a permanent gap with the reason %s at aggregation=%s k=%d report_grace=%s; it is never sealed, the months after it seal, and the gap cannot be undone\n",
		plan.Month, logsafe.Str(reason), settings.mode, settings.k, settings.grace)
	if dryRun {
		return sealExitSkipped
	}
	if !yes {
		_, _ = fmt.Fprint(stdout, "Record "+plan.Month+" as a permanent gap that is never sealed? Its reason is served to every reader, is permanent and never erasable, and must not name a person. [y/N] ")
		if !confirmed(ctx, stdin) {
			return refused("not confirmed")
		}
	}
	done, rc := skipOnce(false, true, plan.Category)
	if rc >= 0 {
		return rc
	}
	_, _ = fmt.Fprintf(stdout, "tierd seal: skipped: recorded %s as a permanent gap (category %s) after its seal failed: %s; the next seal pass seals the months after it\n", done.Month, done.Category, logsafe.Err(done.Observed))
	return sealExitSkipped
}

// confirmed reads one answer line and reports whether it is y or yes; a
// cancelled ctx (Ctrl-C) is no.
func confirmed(ctx context.Context, stdin io.Reader) bool {
	answer := make(chan string, 1)
	go func() {
		a, _ := bufio.NewReader(stdin).ReadString('\n')
		answer <- a
	}()
	select {
	case a := <-answer:
		a = strings.ToLower(strings.TrimSpace(a))
		return a == "y" || a == "yes"
	case <-ctx.Done():
		return false
	}
}
