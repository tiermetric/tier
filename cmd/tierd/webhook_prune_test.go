package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// TestRunWebhookPayloadPruner_PrunesAgedRowWithoutRestart pins #846: a row that
// ages past the retention bound AFTER store.Open's boot prune is removed by the
// running loop, with no restart. The fresh control row must survive, so a loop
// that wiped the table would fail too.
func TestRunWebhookPayloadPruner_PrunesAgedRowWithoutRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prune.db")
	db, err := store.Open(path) // the boot prune runs here, on an empty table
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.InsertWebhookPayload(ctx, "push", "fresh", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("InsertWebhookPayload fresh: %v", err)
	}
	// InsertWebhookPayload always stamps received_at = now, so the aged row goes
	// in by raw SQL — AFTER Open, so only the loop can remove it.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"ok":true}`))
	_ = zw.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256, received_at)
		VALUES ('push', 'stale', ?, 'deadbeef', datetime('now','-91 days'))`, gz.Bytes()); err != nil {
		_ = raw.Close()
		t.Fatalf("insert stale row: %v", err)
	}
	_ = raw.Close()
	if _, found, err := db.WebhookPayloadByDelivery(ctx, "push", "stale"); err != nil || !found {
		t.Fatalf("stale row not present before the loop ran: found=%v err=%v", found, err)
	}

	logger, logs := capturingLogger(slog.LevelInfo)
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWebhookPayloadPruner(loopCtx, db, 5*time.Millisecond, logger)
	}()

	waitFor(t, func() bool {
		_, found, err := db.WebhookPayloadByDelivery(ctx, "push", "stale")
		return err == nil && !found
	}, "the 91-day-old row was never pruned by the running loop")
	if _, found, err := db.WebhookPayloadByDelivery(ctx, "push", "fresh"); err != nil || !found {
		t.Errorf("fresh row was pruned: found=%v err=%v", found, err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runWebhookPayloadPruner did not return after cancel")
	}
	// Read only after the join: the buffer is not safe for concurrent use.
	if !strings.Contains(logs.String(), "rows_pruned=1") {
		t.Errorf("no rows_pruned=1 log line; got:\n%s", logs.String())
	}
}

// fakePruner counts calls and returns errs[i] on call i (nil once exhausted).
// When block is set, a call waits for ctx and returns its error, modelling a
// prune that is mid-transaction when shutdown cancels.
type fakePruner struct {
	mu    sync.Mutex
	n     int
	errs  []error
	block bool
}

func (f *fakePruner) PruneWebhookPayloads(ctx context.Context) (int64, error) {
	f.mu.Lock()
	i := f.n
	f.n++
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return 0, f.errs[i]
	}
	return 0, nil
}

func (f *fakePruner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// TestRunWebhookPayloadPruner_ErrorDoesNotStopLoop pins that a failed prune is
// logged and retried at the next tick rather than ending the loop.
func TestRunWebhookPayloadPruner_ErrorDoesNotStopLoop(t *testing.T) {
	fake := &fakePruner{errs: []error{errors.New("database is locked")}}
	logger, logs := capturingLogger(slog.LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWebhookPayloadPruner(ctx, fake, time.Millisecond, logger)
	}()

	waitFor(t, func() bool { return fake.calls() >= 2 },
		"the loop made %d prune calls after an error, want >= 2", fake.calls)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runWebhookPayloadPruner did not return after cancel")
	}
	if got := logs.String(); !strings.Contains(got, "webhook payload prune failed") || !strings.Contains(got, "database is locked") {
		t.Errorf("the failed prune was not logged with its error; got:\n%s", got)
	}
}

// TestRunWebhookPayloadPruner_ExitsOnCancel pins the shutdown half: the loop
// returns promptly on cancel both while idle between ticks and while a prune is
// in flight, and a prune aborted by the cancel is not logged as a failure.
// Run under -race by make check.
func TestRunWebhookPayloadPruner_ExitsOnCancel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		fake     *fakePruner
		inFlight bool
	}{
		{"idle between ticks", time.Hour, &fakePruner{}, false},
		{"prune in flight", time.Millisecond, &fakePruner{block: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := capturingLogger(slog.LevelInfo)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				runWebhookPayloadPruner(ctx, tc.fake, tc.interval, logger)
			}()
			if tc.inFlight {
				waitFor(t, func() bool { return tc.fake.calls() == 1 }, "no prune started")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("runWebhookPayloadPruner did not return after cancel — it would leak past shutdown")
			}
			if tc.fake.calls() > 1 {
				t.Errorf("prune called %d times, want at most 1", tc.fake.calls())
			}
			if logs.Len() != 0 {
				t.Errorf("shutdown produced log output: %q", logs.String())
			}
		})
	}
}

// TestShutdownServer_PrunerJoin is the pruner twin of
// TestShutdownServer_FeeReconcilerJoin: the prune opens a transaction, so
// shutdown waits for pruneDone before the caller's db.Close, and gives up with an
// ERROR after drainTimeout rather than ignore SIGTERM.
func TestShutdownServer_PrunerJoin(t *testing.T) {
	t.Run("late-draining pruner: waits for the join", func(t *testing.T) {
		pruneDone := make(chan struct{})
		const drainDelay = 40 * time.Millisecond
		logger, logs := captureErrorLogger()
		start := time.Now()
		go func() {
			tm := time.NewTimer(drainDelay)
			defer tm.Stop()
			<-tm.C
			close(pruneDone)
		}()
		shutdownServer(func() {}, closedChan(), closedChan(), pruneDone, closedChan(), &http.Server{}, 5*time.Second, time.Second, logger)
		if elapsed := time.Since(start); elapsed < drainDelay {
			t.Errorf("shutdown returned in %v, before the pruner drained (%v)", elapsed, drainDelay)
		}
		if logs.Len() != 0 {
			t.Errorf("guard timer fired spuriously: %q", logs.String())
		}
	})
	t.Run("wedged pruner: proceeds after the guard timer with an ERROR", func(t *testing.T) {
		pruneDone := make(chan struct{}) // never closed
		const drainTimeout = 30 * time.Millisecond
		logger, logs := captureErrorLogger()
		start := time.Now()
		shutdownServer(func() {}, closedChan(), closedChan(), pruneDone, closedChan(), &http.Server{}, drainTimeout, time.Second, logger)
		if elapsed := time.Since(start); elapsed < drainTimeout {
			t.Errorf("shutdown returned in %v, before the %v guard timer", elapsed, drainTimeout)
		}
		if !strings.Contains(logs.String(), "webhook payload pruner failed to drain") {
			t.Errorf("expected a wedged-pruner ERROR log, got %q", logs.String())
		}
	})
}

// TestWebhookPruneInterval_IsDaily pins the cadence docs/privacy.md promises.
func TestWebhookPruneInterval_IsDaily(t *testing.T) {
	if webhookPruneInterval != 24*time.Hour {
		t.Errorf("webhookPruneInterval = %v, want 24h (docs/privacy.md says every 24 hours)", webhookPruneInterval)
	}
}

// TestServe_StartsWebhookPayloadPruner pins that runServeWithOptions actually
// starts the loop, in a goroutine, at webhookPruneInterval — the unit tests above
// all pass against a serve that never calls it. AST, not grep, so a comment that
// mentions the name does not count.
//
// The GoStmt must be a DIRECT statement of the function body: the pruner runs in
// every serve, --read-only included, so a start nested under any `if` (e.g.
// `if !*readOnly { go … }`) fails. And every join site — the abrupt-exit
// joinBackgroundWriters and both shutdownServer calls — must pass pruneDone in
// the pruneDone position, or shutdown would close the DB under an open prune.
func TestServe_StartsWebhookPayloadPruner(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var serve *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runServeWithOptions" {
			serve = fd
		}
	}
	if serve == nil {
		t.Fatal("runServeWithOptions not found in main.go")
	}
	startsPruner := func(g *ast.GoStmt) bool {
		var found bool
		ast.Inspect(g, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "runWebhookPayloadPruner" || len(call.Args) != 4 {
				return true
			}
			if iv, ok := call.Args[2].(*ast.Ident); ok && iv.Name == "webhookPruneInterval" {
				found = true
			}
			return true
		})
		return found
	}
	var started, nested bool
	direct := make(map[*ast.GoStmt]bool)
	for _, stmt := range serve.Body.List {
		if g, ok := stmt.(*ast.GoStmt); ok {
			direct[g] = true
		}
	}
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		g, ok := n.(*ast.GoStmt)
		if !ok || !startsPruner(g) {
			return true
		}
		if direct[g] {
			started = true
		} else {
			nested = true
		}
		return true
	})
	if nested {
		t.Error("runServeWithOptions starts runWebhookPayloadPruner under a conditional — it must start in every serve, --read-only included (#846)")
	}
	if !started {
		t.Error("runServeWithOptions does not start runWebhookPayloadPruner(…, webhookPruneInterval, …) in a goroutine as a direct statement of its body — a long-running serve would never prune (#846)")
	}

	// Every join site passes pruneDone as its 4th argument (index 3).
	want := map[string]int{"joinBackgroundWriters": 1, "shutdownServer": 2}
	got := make(map[string]int)
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if _, isJoin := want[fn.Name]; !isJoin {
			return true
		}
		if len(call.Args) < 4 {
			t.Errorf("%s call has %d args, want the pruneDone position (index 3)", fn.Name, len(call.Args))
			return true
		}
		if id, ok := call.Args[3].(*ast.Ident); !ok || id.Name != "pruneDone" {
			t.Errorf("%s call passes %s in the pruneDone position, want pruneDone — shutdown would not wait for the pruner (#846)", fn.Name, exprName(call.Args[3]))
			return true
		}
		got[fn.Name]++
		return true
	})
	for name, n := range want {
		if got[name] != n {
			t.Errorf("runServeWithOptions has %d %s call(s) passing pruneDone, want %d", got[name], name, n)
		}
	}
}

// exprName renders an argument expression for a failure message.
func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", e)
}
