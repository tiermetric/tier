package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// serveBody is runServeWithOptions' body, parsed from main.go.
func serveBody(t *testing.T) *ast.BlockStmt {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runServeWithOptions" {
			return fd.Body
		}
	}
	t.Fatal("runServeWithOptions not found in main.go")
	return nil
}

// methodCalls is every call to a method named name on an identifier, in order.
func methodCalls(body *ast.BlockStmt, name string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				calls = append(calls, c)
			}
		}
		return true
	})
	return calls
}

// TestServe_WiresTheSealer (#913 switch-on) pins serve's wiring, which the api
// tests of EnableSealing and StartSealer cannot see: serve builds the sealer
// once from its resolved grace and seal_from, before the startup lines that
// read it (logSealOverdue takes its sealing arm only with a sealer) and before
// mounting the routes; it starts it once, unconditionally (StartSealer itself
// refuses developer mode and --read-only), on the background context with
// --read-only; every join site waits on sealDone; the aggregation line logs
// report_grace; serve never builds its handler WithUnsealedRecompute; and the
// order is SetAggregation, then EnableSealing, then StartSealer.
func TestServe_WiresTheSealer(t *testing.T) {
	body := serveBody(t)
	enable := methodCalls(body, "EnableSealing")
	if len(enable) != 1 || len(enable[0].Args) != 2 {
		t.Fatalf("runServeWithOptions calls EnableSealing %d times, want once with (grace, seal_from)", len(enable))
	}
	for i, want := range []string{"grace", "sealFrom"} {
		if sel, ok := enable[0].Args[i].(*ast.SelectorExpr); !ok || exprName(sel.X) != "sealCfg" || sel.Sel.Name != want {
			t.Errorf("EnableSealing argument %d is not sealCfg.%s", i, want)
		}
	}
	for _, later := range []string{"logSealOverdue", "logSealConfigGap"} {
		var pos token.Pos
		ast.Inspect(body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && exprName(c.Fun) == later && pos == 0 {
				pos = c.Pos()
			}
			return true
		})
		if pos == 0 || pos < enable[0].Pos() {
			t.Errorf("%s is not called after EnableSealing, so it cannot see the sealer", later)
		}
	}
	for _, reg := range append(methodCalls(body, "Register"), methodCalls(body, "RegisterReadOnly")...) {
		if reg.Pos() < enable[0].Pos() {
			t.Error("routes are mounted before EnableSealing")
		}
	}

	var direct *ast.AssignStmt
	for _, stmt := range body.List {
		if as, ok := stmt.(*ast.AssignStmt); ok && len(as.Rhs) == 1 && len(methodCalls(&ast.BlockStmt{List: []ast.Stmt{as}}, "StartSealer")) == 1 {
			direct = as
		}
	}
	start := methodCalls(body, "StartSealer")
	if len(start) != 1 || direct == nil || exprName(direct.Lhs[0]) != "sealDone" {
		t.Fatalf("runServeWithOptions calls StartSealer %d times, want once, as `sealDone, _ := …StartSealer(…)` directly in its body", len(start))
	}
	// EnableSealing builds no sealer before the handler is anonymised, and
	// StartSealer starts none before it is built: either swap leaves a team
	// serve that never seals.
	agg := methodCalls(body, "SetAggregation")
	if len(agg) != 1 || agg[0].Pos() > enable[0].Pos() {
		t.Errorf("runServeWithOptions calls SetAggregation %d times, want once, before EnableSealing", len(agg))
	}
	if start[0].Pos() < enable[0].Pos() {
		t.Error("StartSealer is called before EnableSealing")
	}
	if a := start[0].Args; len(a) != 2 || exprName(a[0]) != "watcherCtx" {
		t.Errorf("StartSealer is not started on watcherCtx, which shutdown cancels")
	} else if star, ok := a[1].(*ast.StarExpr); !ok || exprName(star.X) != "readOnly" {
		t.Errorf("StartSealer is not given *readOnly")
	}

	joins := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if fn := exprName(c.Fun); (fn == "joinBackgroundWriters" || fn == "shutdownServer") && len(c.Args) > 4 && exprName(c.Args[4]) == "sealDone" {
				joins[fn]++
			}
		}
		return true
	})
	if joins["joinBackgroundWriters"] != 1 || joins["shutdownServer"] != 2 {
		t.Errorf("join sites passing sealDone: %v, want joinBackgroundWriters 1 and shutdownServer 2", joins)
	}

	var logsGrace bool
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && len(c.Args) > 0 {
			if lit, ok := c.Args[0].(*ast.BasicLit); ok && lit.Value == `"aggregation mode"` {
				for _, a := range c.Args {
					if l, ok := a.(*ast.BasicLit); ok && l.Value == `"report_grace"` {
						logsGrace = true
					}
				}
			}
		}
		return true
	})
	if !logsGrace {
		t.Error(`the "aggregation mode" startup line does not log report_grace`)
	}
	if n := len(methodCalls(body, "WithUnsealedRecompute")); n != 0 {
		t.Errorf("runServeWithOptions calls api.WithUnsealedRecompute %d times: an anonymised serve would publish live windows", n)
	}
}

// TestShutdownServer_SealerJoin is the sealer twin of
// TestShutdownServer_PrunerJoin: a seal is a transaction, so shutdown waits
// for sealDone before the caller's db.Close, and gives up with an ERROR after
// drainTimeout rather than ignore SIGTERM.
func TestShutdownServer_SealerJoin(t *testing.T) {
	t.Run("late-draining sealer: waits for the join", func(t *testing.T) {
		sealDone := make(chan struct{})
		const drainDelay = 40 * time.Millisecond
		logger, logs := captureErrorLogger()
		start := time.Now()
		go func() {
			tm := time.NewTimer(drainDelay)
			defer tm.Stop()
			<-tm.C
			close(sealDone)
		}()
		shutdownServer(func() {}, closedChan(), closedChan(), closedChan(), sealDone, &http.Server{}, 5*time.Second, time.Second, logger)
		if elapsed := time.Since(start); elapsed < drainDelay {
			t.Errorf("shutdown returned in %v, before the sealer drained (%v)", elapsed, drainDelay)
		}
		if logs.Len() != 0 {
			t.Errorf("guard timer fired spuriously: %q", logs.String())
		}
	})
	t.Run("wedged sealer: proceeds after the guard timer with an ERROR", func(t *testing.T) {
		const drainTimeout = 30 * time.Millisecond
		logger, logs := captureErrorLogger()
		start := time.Now()
		shutdownServer(func() {}, closedChan(), closedChan(), closedChan(), make(chan struct{}), &http.Server{}, drainTimeout, time.Second, logger)
		if elapsed := time.Since(start); elapsed < drainTimeout {
			t.Errorf("shutdown returned in %v, before the %v guard timer", elapsed, drainTimeout)
		}
		if !strings.Contains(logs.String(), "background sealer failed to drain") {
			t.Errorf("expected a wedged-sealer ERROR log, got %q", logs.String())
		}
	})
	t.Run("a live sealer counts as a live writer", func(t *testing.T) {
		logger, _ := captureErrorLogger()
		sealDone := make(chan struct{})
		if !joinBackgroundWriters(func() { close(sealDone) }, closedChan(), closedChan(), closedChan(), sealDone, time.Second, logger) {
			t.Error("joinBackgroundWriters reported no live writer while the sealer ran")
		}
	})
}

// TestServeSealing_StopsOnShutdown: the sealer serve starts on a writable
// team-mode handler stops when shutdown cancels its context, so the join
// returns without its ERROR; a read-only handler's sealDone is closed already.
func TestServeSealing_StopsOnShutdown(t *testing.T) {
	path := sealTestDB(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{})
	h.SetAggregation(scoring.AggregationTeam, 5)
	if err := h.EnableSealing(defaultReportGrace, ""); err != nil {
		t.Fatal(err)
	}
	if done, started := h.StartSealer(context.Background(), true); started || !isClosed(done) {
		t.Errorf("read-only: started %v, done closed %v; want never started and closed", started, isClosed(done))
	}
	ctx, cancel := context.WithCancel(context.Background())
	sealDone, started := h.StartSealer(ctx, false)
	if !started {
		t.Fatal("a writable team-mode handler did not start its sealer")
	}
	logger, logs := captureErrorLogger()
	joinBackgroundWriters(cancel, closedChan(), closedChan(), closedChan(), sealDone, 10*time.Second, logger)
	if !isClosed(sealDone) || logs.Len() != 0 {
		t.Errorf("after the join: sealer stopped %v, logs %q; want stopped with no ERROR", isClosed(sealDone), logs.String())
	}
}

// TestLogSealConfigGap (#913-D1 ruling A): with a month sealed at k=5, startup
// at k=6 warns, naming the month, both configs and the month the current
// config applies from; at k=5, and in developer mode, it says nothing.
func TestLogSealConfigGap(t *testing.T) {
	path := sealTestDB(t)
	if rc, _, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path); rc != 0 {
		t.Fatalf("arm: rc %d, %s", rc, errOut)
	}
	sealed := sealTestMonth().Format("2006-01")
	logOf := func(mode scoring.AggregationMode, k int) string {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{})
		h.SetAggregation(mode, k)
		if err := h.EnableSealing(defaultReportGrace, ""); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := logSealConfigGap(context.Background(), h, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	got := logOf(scoring.AggregationTeam, 6)
	next := sealTestMonth().AddDate(0, 1, 0).Format("2006-01")
	for _, want := range []string{"level=WARN", "newest_sealed=" + sealed, "current_from=" + next, "k=5 ", "k=6 "} {
		if !strings.Contains(got, want) {
			t.Errorf("k=6 over a month sealed at k=5: %q, want %q", got, want)
		}
	}
	for name, got := range map[string]string{"k=5": logOf(scoring.AggregationTeam, 5), "developer": logOf(scoring.AggregationDeveloper, 0)} {
		if got != "" {
			t.Errorf("%s: logged %q, want nothing", name, got)
		}
	}
}

// TestRerunScores_ReplaysAnAnonymisedLiveWindow: verify-report re-runs a
// team-mode live-window manifest over its window (api.WithUnsealedRecompute),
// where a handler with no sealer would answer 503.
func TestRerunScores_ReplaysAnAnonymisedLiveWindow(t *testing.T) {
	f := seedVerifyDB(t)
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	w := manifestWindow{SinceQuery: verifyWindowSince, UntilQuery: verifyWindowUntil}
	body, err := rerunScores(context.Background(), db, w, "", scoring.AggregationTeam, scoring.DefaultKAnonymity)
	if err != nil || !strings.Contains(string(body), `"aggregation":"team"`) {
		t.Errorf("team-mode re-run: err %v, body %.200s; want a live team body", err, body)
	}
}
