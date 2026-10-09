package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// sealTestMonth is the first full month of the seeded cost data: 100 days
// before now, so it has closed and passed the default grace.
func sealTestMonth() time.Time {
	t := time.Now().UTC().AddDate(0, 0, -100)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// sealTestDB is a database whose cost coverage starts exactly at sealTestMonth.
func sealTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	m := sealTestMonth()
	for i, ts := range []time.Time{m, m.AddDate(0, 0, 3)} {
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4", InputTok: 10 + i,
			CostMicro: 1_000_000, Source: "api", Fidelity: "estimated", Timestamp: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// serve has registered its sources, none of them gated (#913-D9).
	if _, err := db.RegisterSources(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return path
}

// sealState is the sealed months and the pinned floor ("" when none).
func sealState(t *testing.T, path string) (sealed, floor string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := raw.QueryRow(`SELECT COALESCE(group_concat(period_start), '') FROM sealed_report`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COALESCE(MAX(period_start), '') FROM seal_floor`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	return sealed, floor
}

func runSeal(t *testing.T, stdin string, args ...string) (rc int, stdout, stderr string) {
	t.Helper()
	for _, v := range []string{"TIER_PRICES", "TIER_READ_ONLY", "TIER_AGGREGATION", "TIER_K_ANONYMITY", "TIER_REPORT_GRACE", "TIER_SEAL_FROM", "TIER_API_TOKEN"} {
		t.Setenv(v, "")
	}
	var out, errOut bytes.Buffer
	rc = runSealCmd(args, strings.NewReader(stdin), &out, &errOut)
	return rc, out.String(), errOut.String()
}

// TestSealCmd_ArmsThenAlreadyArmed: exit 0 when this call sealed the first
// month and pinned it; a second arm writes nothing and exits 3 naming the
// pinned month.
func TestSealCmd_ArmsThenAlreadyArmed(t *testing.T) {
	path := sealTestDB(t)
	month := sealTestMonth().Format("2006-01")
	rc, out, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path)
	if rc != 0 || !strings.Contains(out, "armed: sealed "+month) {
		t.Fatalf("first arm: rc %d, stdout %q, stderr %q; want 0 naming %s", rc, out, errOut, month)
	}
	want := sealTestMonth().Format(time.RFC3339)
	if sealed, floor := sealState(t, path); sealed != want || floor != want {
		t.Fatalf("first arm left sealed %q, floor %q; want %s for both", sealed, floor, want)
	}
	rc, out, _ = runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path)
	if rc != 3 || !strings.Contains(out, "already armed") || !strings.Contains(out, month) {
		t.Errorf("second arm: rc %d, stdout %q; want 3 naming %s", rc, out, month)
	}
	if sealed, _ := sealState(t, path); sealed != want {
		t.Errorf("second arm left sealed %q, want %s alone", sealed, want)
	}
}

// TestSealCmd_DryRunAndConfirmation: --dry-run exits 0 naming the month and
// writes nothing; without --yes a declined or absent answer exits 1 and writes
// nothing, and "y" arms.
func TestSealCmd_DryRunAndConfirmation(t *testing.T) {
	path := sealTestDB(t)
	month := sealTestMonth().Format("2006-01")
	base := []string{"--arm", "earliest", "--aggregation", "team", "--db", path}
	if rc, out, _ := runSeal(t, "", append(base, "--dry-run")...); rc != 0 || !strings.Contains(out, "will seal "+month) {
		t.Errorf("dry run: rc %d, stdout %q; want 0 naming %s", rc, out, month)
	}
	for _, answer := range []string{"", "n\n", "yes please\n"} {
		if rc, _, errOut := runSeal(t, answer, base...); rc != 1 || !strings.Contains(errOut, "not confirmed") {
			t.Errorf("answer %q: rc %d, stderr %q; want 1, not confirmed", answer, rc, errOut)
		}
	}
	if sealed, floor := sealState(t, path); sealed != "" || floor != "" {
		t.Fatalf("dry run and declined arms left sealed %q, floor %q; want nothing", sealed, floor)
	}
	if rc, _, errOut := runSeal(t, "y\n", base...); rc != 0 {
		t.Errorf("answer y: rc %d, stderr %q; want 0", rc, errOut)
	}
}

// TestSealCmd_Refusals: each precondition exits 1, says why, and writes
// nothing.
func TestSealCmd_Refusals(t *testing.T) {
	now := time.Now().UTC()
	current := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
	emptyDB := filepath.Join(t.TempDir(), "empty.db")
	db, err := store.Open(emptyDB)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for _, c := range []struct {
		name string
		args func(path string) []string
		want string
	}{
		{"no --arm", func(p string) []string { return []string{"--aggregation", "team", "--db", p} }, "--arm"},
		{"developer mode", func(p string) []string {
			return []string{"--arm", "earliest", "--yes", "--aggregation", "developer", "--db", p}
		}, "developer mode"},
		{"read-only", func(p string) []string {
			return []string{"--arm", "earliest", "--yes", "--aggregation", "team", "--read-only", "--db", p}
		}, "--read-only"},
		{"malformed month", func(p string) []string {
			return []string{"--arm", "2026-5", "--yes", "--aggregation", "team", "--db", p}
		}, "YYYY-MM"},
		{"bad price table", func(p string) []string {
			bad := filepath.Join(t.TempDir(), "prices.yaml")
			if err := os.WriteFile(bad, []byte("not: [a price table"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"--arm", "earliest", "--yes", "--aggregation", "team", "--prices", bad, "--db", p}
		}, "--prices"},
		{"malformed seal_from", func(p string) []string {
			return []string{"--arm", "earliest", "--yes", "--aggregation", "team", "--seal-from", "May", "--db", p}
		}, "--seal-from"},
		{"no cost data", func(string) []string {
			return []string{"--arm", "earliest", "--yes", "--aggregation", "team", "--db", emptyDB}
		}, "no cost data"},
		{"month not sealable now", func(p string) []string {
			return []string{"--arm", current, "--yes", "--aggregation", "team", "--db", p}
		}, current + " is open or inside its grace lag until "},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := c.args(sealTestDB(t))
			rc, out, errOut := runSeal(t, "", args...)
			if rc != 1 || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, "nothing was sealed or pinned") {
				t.Errorf("rc %d, stdout %q, stderr %q; want 1 naming %q", rc, out, errOut, c.want)
			}
			if sealed, floor := sealState(t, args[len(args)-1]); sealed != "" || floor != "" {
				t.Errorf("left sealed %q, floor %q; want nothing", sealed, floor)
			}
		})
	}
}

// TestSealCmd_CannotRun: a database that does not exist (and is not created),
// a file that is not a SQLite database, and a seal that cannot take the write
// lock each exit 2 and seal and pin nothing.
func TestSealCmd_CannotRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", missing); rc != 2 || !strings.Contains(errOut, "could not run") {
		t.Errorf("missing db: rc %d, stderr %q; want 2", rc, errOut)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("missing db was created: %v", err)
	}
	notDB := filepath.Join(t.TempDir(), "not.db")
	if err := os.WriteFile(notDB, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", notDB); rc != 2 || !strings.Contains(errOut, "open db") {
		t.Errorf("not a database: rc %d, stderr %q; want 2 naming open db", rc, errOut)
	}
	path := sealTestDB(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The answer takes the write lock after the plan, so the seal cannot.
	lockThenAnswer := readerFunc(func(p []byte) (int, error) {
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Error(err)
		}
		return copy(p, "y\n"), io.EOF
	})
	var out, errOut bytes.Buffer
	if rc := runSealCmd([]string{"--arm", "earliest", "--aggregation", "team", "--db", path}, lockThenAnswer, &out, &errOut); rc != 2 || !strings.Contains(errOut.String(), "could not run") {
		t.Errorf("write lock held: rc %d, stderr %q; want 2", rc, errOut.String())
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if sealed, floor := sealState(t, path); sealed != "" || floor != "" {
		t.Errorf("left sealed %q, floor %q; want nothing", sealed, floor)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestSealCmd_SealsUnderServeKAndGrace: the first month is sealed under the k
// and report_grace --config gives, as serve would seal it: k=7 is stamped on
// the row and its config digest, and a grace the month has not passed refuses.
func TestSealCmd_SealsUnderServeKAndGrace(t *testing.T) {
	digestAt := func(cfg string) (k int, digest string) {
		path := sealTestDB(t)
		cfgPath := filepath.Join(t.TempDir(), "tierd.yaml")
		if err := os.WriteFile(cfgPath, []byte("db: "+path+"\naggregation: team\n"+cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if rc, out, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--config", cfgPath); rc != 0 {
			t.Fatalf("config %q: rc %d, stdout %q, stderr %q; want 0", cfg, rc, out, errOut)
		} else if cfg != "" && !strings.Contains(out, "k=7 report_grace=48h0m0s") {
			t.Errorf("plan line %q does not show k=7 report_grace=48h0m0s", out)
		}
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		if err := raw.QueryRow(`SELECT k, config_digest FROM sealed_report`).Scan(&k, &digest); err != nil {
			t.Fatal(err)
		}
		return k, digest
	}
	k7, d7 := digestAt("k_anonymity: 7\nreport_grace: 48h\n")
	k5, d5 := digestAt("")
	if k7 != 7 || k5 != scoring.DefaultKAnonymity || d7 == d5 {
		t.Errorf("sealed k %d (digest %s) under k_anonymity 7, k %d (digest %s) by default; want 7, %d and two digests", k7, d7, k5, d5, scoring.DefaultKAnonymity)
	}
	path := sealTestDB(t)
	cfgPath := filepath.Join(t.TempDir(), "tierd.yaml")
	if err := os.WriteFile(cfgPath, []byte("db: "+path+"\naggregation: team\nreport_grace: 2400h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--config", cfgPath); rc != 1 || !strings.Contains(errOut, "inside its grace lag") {
		t.Errorf("report_grace 2400h: rc %d, stderr %q; want 1, inside its grace lag", rc, errOut)
	}
}

// TestSealCmd_InterruptAtPromptIsNotConfirmed: SIGINT while the prompt waits
// ends the command with exit 1, sealing and pinning nothing.
func TestSealCmd_InterruptAtPromptIsNotConfirmed(t *testing.T) {
	path := sealTestDB(t)
	unblock := make(chan struct{})
	defer close(unblock)
	interruptThenWait := readerFunc(func([]byte) (int, error) {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Error(err)
		}
		<-unblock
		return 0, io.EOF
	})
	var out, errOut bytes.Buffer
	if rc := runSealCmd([]string{"--arm", "earliest", "--aggregation", "team", "--db", path}, interruptThenWait, &out, &errOut); rc != 1 || !strings.Contains(errOut.String(), "not confirmed") {
		t.Errorf("SIGINT at the prompt: rc %d, stderr %q; want 1, not confirmed", rc, errOut.String())
	}
	if sealed, floor := sealState(t, path); sealed != "" || floor != "" {
		t.Errorf("left sealed %q, floor %q; want nothing", sealed, floor)
	}
}

// TestSealCmd_ReadsServeConfig: `tierd seal --config` resolves the file as
// serve does: its db, aggregation and seal_from apply, and a flag overrides it.
func TestSealCmd_ReadsServeConfig(t *testing.T) {
	path := sealTestDB(t)
	cfgPath := filepath.Join(t.TempDir(), "tierd.yaml")
	write := func(body string) {
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("db: " + path + "\naggregation: developer\n")
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--config", cfgPath); rc != 1 || !strings.Contains(errOut, "developer mode") {
		t.Errorf("config aggregation developer: rc %d, stderr %q; want 1, developer mode", rc, errOut)
	}
	write("db: " + path + "\naggregation: team\nseal_from: \"2026-13\"\n")
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--config", cfgPath); rc != 1 || !strings.Contains(errOut, "--seal-from") {
		t.Errorf("config seal_from 2026-13: rc %d, stderr %q; want 1 naming --seal-from", rc, errOut)
	}
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--config", cfgPath, "--seal-from", "2026-01"); rc != 0 {
		t.Errorf("flag over config: rc %d, stderr %q; want 0", rc, errOut)
	}
}

// TestSealServeFlags_SealFromSource: resolve names where seal_from came from,
// flag over env over config.
func TestSealServeFlags_SealFromSource(t *testing.T) {
	resolveWith := func(env string, args ...string) string {
		t.Setenv("TIER_SEAL_FROM", env)
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		sf := addSealServeFlags(fs)
		if err := fs.Parse(append([]string{"--aggregation", "team"}, args...)); err != nil {
			t.Fatal(err)
		}
		setFlags := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
		v := "2026-02"
		sf.applyConfig(fs, setFlags, sf.envSet(), configWithSealFrom(&v))
		s, err := sf.resolve(fs)
		if err != nil {
			t.Fatal(err)
		}
		return s.sealFrom + " " + s.sealFromSource
	}
	for _, c := range []struct {
		env  string
		args []string
		want string
	}{
		{"", nil, "2026-02 config seal_from"},
		{"2026-03", nil, "2026-03 env TIER_SEAL_FROM"},
		{"2026-03", []string{"--seal-from", "2026-04"}, "2026-04 flag --seal-from"},
		{"", []string{"--seal-from", "2026-04"}, "2026-04 flag --seal-from"},
	} {
		if got := resolveWith(c.env, c.args...); got != c.want {
			t.Errorf("env %q args %v: %q, want %q", c.env, c.args, got, c.want)
		}
	}
}

// TestLogSealArm: startup names where sealing is armed from, and warns when
// seal_from names a month other than the pinned floor.
func TestLogSealArm(t *testing.T) {
	path := sealTestDB(t)
	month := sealTestMonth().Format("2006-01")
	logOf := func(s sealSettings) string {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var buf bytes.Buffer
		if err := logSealArm(context.Background(), db, s, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	team := sealSettings{mode: scoring.AggregationTeam}
	if got := logOf(team); !strings.Contains(got, "sealing not armed") {
		t.Errorf("unarmed, no seal_from: %q", got)
	}
	fromEnv := sealSettings{mode: scoring.AggregationTeam, sealFrom: "2026-02", sealFromSource: "env TIER_SEAL_FROM"}
	if got := logOf(fromEnv); !strings.Contains(got, `seals each month once its grace lag ends (#913)" seal_from=2026-02 source="env TIER_SEAL_FROM"`) {
		t.Errorf("unarmed, seal_from from env: %q", got)
	}
	readOnly := sealSettings{mode: scoring.AggregationTeam, readOnly: true}
	if got := logOf(readOnly); !strings.Contains(got, "read-only server never seals") || strings.Contains(got, "tierd seal --arm") {
		t.Errorf("read-only: %q; want never seals, and no advice to arm", got)
	}
	if got := logOf(sealSettings{mode: scoring.AggregationDeveloper, sealFrom: "2026-02"}); got != "" {
		t.Errorf("developer mode logged %q, want nothing", got)
	}
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path); rc != 0 {
		t.Fatalf("arm: rc %d, %s", rc, errOut)
	}
	got := logOf(fromEnv)
	if !strings.Contains(got, `seals each month once its grace lag ends (#913)" floor=`+month) || !strings.Contains(got, "level=WARN") ||
		!strings.Contains(got, "seal_from=2026-02") || !strings.Contains(got, "floor="+month) {
		t.Errorf("pinned %s, seal_from 2026-02: %q; want armed at the floor and a WARN naming both", month, got)
	}
	agree := sealSettings{mode: scoring.AggregationTeam, sealFrom: month, sealFromSource: "config seal_from"}
	if got := logOf(agree); strings.Contains(got, "level=WARN") {
		t.Errorf("seal_from equal to the pinned floor warned: %q", got)
	}
}

// sealingHandler is serve's handler with its background sealer running, as a
// writable team/division serve's is (#913).
type sealingHandler struct{ *api.Handler }

func (sealingHandler) SealsInBackground(bool) bool { return true }

// TestLogSealOverdue (#913-D6 ruling D, D8 ruling C): startup warns when the
// database shows an overdue month, naming it and its sealable_at. Only a serve
// whose background sealer runs is told its first pass retries the month and,
// once a month is sealed, given the skip command; that command, run as
// printed, is refused and records no gap until a reason is written into it. A
// serve with no sealer, the first month and a read-only server get no command
// and no stall claim; developer mode and an unarmed database get no warning.
func TestLogSealOverdue(t *testing.T) {
	path := sealTestDB(t)
	first := sealTestMonth()
	logOf := func(s sealSettings, sealing bool) string {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{})
		h.SetAggregation(s.mode, 5)
		var src sealOverdueReader = h
		if sealing {
			src = sealingHandler{h}
		}
		var buf bytes.Buffer
		if err := logSealOverdue(context.Background(), src, s, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	team := func(sealFrom string, readOnly bool) sealSettings {
		return sealSettings{mode: scoring.AggregationTeam, grace: defaultReportGrace, sealFrom: sealFrom, readOnly: readOnly}
	}
	const notOn, retries = "this server's background sealer does not run", "first seal pass retries it now"
	for _, sealing := range []bool{false, true} {
		if got := logOf(team("", false), sealing); got != "" {
			t.Errorf("unarmed, sealing=%v: %q, want nothing", sealing, got)
		}
	}
	firstMonth := first.Format("2006-01")
	if got := logOf(team(firstMonth, false), false); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "month="+firstMonth) ||
		!strings.Contains(got, notOn) || strings.Contains(got, "stalled") || strings.Contains(got, "command=") {
		t.Errorf("armed by seal_from, nothing sealed, no sealer: %q; want a WARN naming %s, the sealer not on, no stall and no command", got, firstMonth)
	}
	if got := logOf(team(firstMonth, false), true); !strings.Contains(got, "month="+firstMonth) || !strings.Contains(got, retries) ||
		strings.Contains(got, notOn) || strings.Contains(got, "command=") {
		t.Errorf("armed by seal_from, nothing sealed, sealing: %q; want %s named, the first pass retrying it and no command", got, firstMonth)
	}
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path); rc != 0 {
		t.Fatalf("arm: rc %d, %s", rc, errOut)
	}
	next := first.AddDate(0, 1, 0).Format("2006-01")
	owedSince := "owed_since=" + first.AddDate(0, 2, 0).Add(defaultReportGrace).Format(time.RFC3339)
	if got := logOf(team("", false), false); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "month="+next) ||
		!strings.Contains(got, owedSince) || !strings.Contains(got, notOn) || strings.Contains(got, "stalled") || strings.Contains(got, "command=") {
		t.Errorf("%s sealed, %s overdue, no sealer: %q; want a WARN naming %s, %s, the sealer not on, no stall and no command",
			firstMonth, next, got, next, owedSince)
	}
	got := logOf(team("", false), true)
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "month="+next) || !strings.Contains(got, owedSince) ||
		!strings.Contains(got, retries) || !strings.Contains(got, `command="tierd seal --skip `+next+` --reason ''"`) {
		t.Errorf("%s sealed, %s overdue, sealing: %q; want a WARN naming %s, %s, the first pass retrying it and its skip command",
			firstMonth, next, got, next, owedSince)
	}
	if got := logOf(team("", true), true); !strings.Contains(got, "month="+next) || strings.Contains(got, "command=") {
		t.Errorf("read-only: %q; want %s named and no command", got, next)
	}
	if got := logOf(sealSettings{mode: scoring.AggregationDeveloper, grace: defaultReportGrace}, true); got != "" {
		t.Errorf("developer mode: %q, want nothing", got)
	}

	// The printed command, shell-split as an operator's paste would be, against
	// a month whose seal fails for good: without a written reason it is refused.
	_, rest, _ := strings.Cut(got, `command="`)
	command, _, _ := strings.Cut(rest, `"`)
	var argv []string
	for _, f := range strings.Fields(command)[2:] {
		argv = append(argv, strings.TrimSuffix(strings.TrimPrefix(f, "'"), "'"))
	}
	breakSealing(t, path)
	armed, _ := sealState(t, path)
	if rc, _, errOut := runSeal(t, "", append(argv, "--yes", "--aggregation", "team", "--db", path)...); rc != 1 ||
		!strings.Contains(errOut, "--skip needs --reason") {
		t.Errorf("the printed command %q, run as printed: rc %d, stderr %q; want 1, refused for its missing reason", command, rc, errOut)
	}
	if sealed, _ := sealState(t, path); sealed != armed || gapState(t, path) != "" {
		t.Errorf("the printed command left sealed %q, gaps %q; want %q and none", sealed, gapState(t, path), armed)
	}
}

// TestServe_LogsSealArm: serve calls logSealArm and logSealOverdue with the
// settings it resolved, so the startup lines TestLogSealArm and
// TestLogSealOverdue pin are reached.
func TestServe_LogsSealArm(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runServeWithOptions" {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && len(c.Args) == 4 && exprName(c.Args[2]) == "sealCfg" {
					calls[exprName(c.Fun)]++
				}
				return true
			})
		}
	}
	for _, fn := range []string{"logSealArm", "logSealOverdue"} {
		if calls[fn] != 1 {
			t.Errorf("runServeWithOptions calls %s(…, sealCfg, …) %d times, want 1", fn, calls[fn])
		}
	}
}

func TestSealCmd_HelpExitsZero(t *testing.T) {
	var out bytes.Buffer
	if rc := runSealCmd([]string{"--help"}, strings.NewReader(""), io.Discard, &out); rc != 0 {
		t.Errorf("--help: rc %d", rc)
	}
}

func configWithSealFrom(v *string) *config.Config { return &config.Config{SealFrom: v} }

// backfillThenAnswer is a confirmation that, before answering "y", records a
// cost event a month before the seeded coverage: history landing while the
// operator reads the prompt.
type backfillThenAnswer struct {
	t    *testing.T
	path string
	done bool
}

func (r *backfillThenAnswer) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	db, err := store.Open(r.path)
	if err != nil {
		r.t.Error(err)
		return 0, err
	}
	defer func() { _ = db.Close() }()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "bob", IssueID: "issue-2", Model: "claude-sonnet-4", InputTok: 10,
		CostMicro: 1_000_000, Source: "api", Fidelity: "estimated", Timestamp: sealTestMonth().AddDate(0, -1, 0),
	}); err != nil {
		r.t.Error(err)
	}
	return copy(p, "y\n"), nil
}

// TestSealCmd_ConfirmedMonthIsTheArmedMonth: when the month to seal changes
// between the prompt and the answer, the arm refuses (exit 1) and seals and
// pins nothing, never a month the operator did not confirm.
func TestSealCmd_ConfirmedMonthIsTheArmedMonth(t *testing.T) {
	path := sealTestDB(t)
	var out, errOut bytes.Buffer
	rc := runSealCmd([]string{"--arm", "earliest", "--aggregation", "team", "--db", path}, &backfillThenAnswer{t: t, path: path}, &out, &errOut)
	if rc != 1 || !strings.Contains(errOut.String(), sealTestMonth().Format("2006-01")) {
		t.Errorf("rc %d, stdout %q, stderr %q; want 1 naming the confirmed month", rc, out.String(), errOut.String())
	}
	if sealed, floor := sealState(t, path); sealed != "" || floor != "" {
		t.Errorf("left sealed %q, floor %q; want nothing", sealed, floor)
	}
}
