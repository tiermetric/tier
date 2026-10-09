package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/anthropicadmin"
	"github.com/tiermetric/tier/internal/collector/openaiusage"
	"github.com/tiermetric/tier/internal/store"
)

// TestStartedPollerProviders pins that a provider is named exactly when its
// resolved settings are non-nil, the same test the poller start blocks apply.
func TestStartedPollerProviders(t *testing.T) {
	admin, oai := &anthropicAdminSettings{}, &openAIUsageSettings{}
	cases := []struct {
		name  string
		admin *anthropicAdminSettings
		oai   *openAIUsageSettings
		want  []string
	}{
		{"none", nil, nil, nil},
		{"anthropic", admin, nil, []string{anthropicadmin.Provider}},
		{"openai", nil, oai, []string{openaiusage.Provider}},
		{"both", admin, oai, []string{anthropicadmin.Provider, openaiusage.Provider}},
	}
	for _, c := range cases {
		if got := startedPollerProviders(c.admin, c.oai); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: startedPollerProviders = %v, want %v", c.name, got, c.want)
		}
	}
	if anthropicadmin.Provider != "anthropic" || openaiusage.Provider != "openai" {
		t.Errorf("poller provider tags = %q/%q, want the price-table tags anthropic/openai",
			anthropicadmin.Provider, openaiusage.Provider)
	}
}

// TestServe_PassesStartedPollersToAPI pins the #854 wiring: serve's api.New
// carries api.WithPolledProviders(startedPollerProviders(adminSettings,
// openaiSettings)...), built from the same variables the poller start blocks
// test, and those start blocks key on exactly those variables being non-nil.
func TestServe_PassesStartedPollersToAPI(t *testing.T) {
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
	isSel := func(e ast.Expr, pkg, name string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		return ok && id.Name == pkg && s.Sel.Name == name
	}
	isIdent := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	var apiNews, wired int
	nilGuarded := map[string]bool{}
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if b, ok := n.Cond.(*ast.BinaryExpr); ok && b.Op == token.NEQ && isIdent(b.Y, "nil") {
				if id, ok := b.X.(*ast.Ident); ok {
					nilGuarded[id.Name] = true
				}
			}
		case *ast.CallExpr:
			if !isSel(n.Fun, "api", "New") {
				return true
			}
			apiNews++
			for _, arg := range n.Args {
				opt, ok := arg.(*ast.CallExpr)
				if !ok || !isSel(opt.Fun, "api", "WithPolledProviders") || len(opt.Args) != 1 || !opt.Ellipsis.IsValid() {
					continue
				}
				inner, ok := opt.Args[0].(*ast.CallExpr)
				if ok && isIdent(inner.Fun, "startedPollerProviders") && len(inner.Args) == 2 &&
					isIdent(inner.Args[0], "adminSettings") && isIdent(inner.Args[1], "openaiSettings") {
					wired++
				}
			}
		}
		return true
	})
	if apiNews != 1 {
		t.Fatalf("runServeWithOptions calls api.New %d times, want 1", apiNews)
	}
	if wired != 1 {
		t.Error("serve's api.New does not pass api.WithPolledProviders(startedPollerProviders(adminSettings, openaiSettings)...) — /costs would admit rows a running poller already counts (#854)")
	}
	for _, v := range []string{"adminSettings", "openaiSettings"} {
		if !nilGuarded[v] {
			t.Errorf("no `if %s != nil` poller start block found; the polled set must key on the same test", v)
		}
	}
}

// TestStartedPollers pins each started poller's provider to the source its rows
// carry, the pair the startup count reads.
func TestStartedPollers(t *testing.T) {
	got := startedPollers(&anthropicAdminSettings{}, &openAIUsageSettings{})
	want := []startedPoller{
		{anthropicadmin.Provider, collector.SourceAnthropicAdmin},
		{openaiusage.Provider, collector.SourceOpenAIUsage},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("startedPollers = %v, want %v", got, want)
	}
	if got := startedPollers(nil, nil); got != nil {
		t.Errorf("no settings: startedPollers = %v, want none", got)
	}
}

// logRecords decodes every JSON log line in buf.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestWarnUndeclaredManualCosts pins #854's startup WARN: one line per running
// poller whose provider has undeclared manual rows on a day it covered, with
// their count, dollar sum and the shared remedy; a manual row on the day AFTER
// the covered day is not counted; a polled provider with none logs nothing.
func TestWarnUndeclaredManualCosts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	ins := func(source, model string, ts time.Time, micro int64) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: model, InputTok: 10,
			CostMicro: micro, Source: source, Fidelity: "estimated", Timestamp: ts,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins(collector.SourceAnthropicAdmin, "claude-sonnet-4", day.Add(24*time.Hour), 1) // covers day
	ins("api", "claude-sonnet-4", day.Add(9*time.Hour), 2_500_000)
	ins("api", "claude-sonnet-4", day.Add(20*time.Hour), 1_500_000)
	ins("api", "claude-sonnet-4", day.Add(33*time.Hour), 9_000_000) // day+1: not covered
	ins(collector.SourceOpenAIUsage, "gpt-4o", day.Add(24*time.Hour), 1)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	// Count 0: the OpenAI poller covered the day but no undeclared gpt row exists.
	warnUndeclaredManualCosts(ctx, db, logger, startedPollers(nil, &openAIUsageSettings{}))
	if buf.Len() != 0 {
		t.Errorf("count 0 logged a line: %s", buf.String())
	}

	// One WARN per running poller with rows: give OpenAI its own undeclared row.
	ins("api", "gpt-4o", day.Add(5*time.Hour), 3_000_000)
	buf.Reset()
	warnUndeclaredManualCosts(ctx, db, logger, startedPollers(&anthropicAdminSettings{}, &openAIUsageSettings{}))

	recs := logRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("got %d log lines, want exactly 2 (one per running poller with undeclared rows): %s", len(recs), buf.String())
	}
	want := map[string][2]float64{
		anthropicadmin.Provider: {2, 4.0}, // the day+1 row must not count
		openaiusage.Provider:    {1, 3.0},
	}
	for _, r := range recs {
		if r["level"] != "WARN" || r["msg"] != undeclaredManualCostsWarning {
			t.Errorf("line = %v, want the WARN %q", r, undeclaredManualCostsWarning)
		}
		p, _ := r["provider"].(string)
		w, ok := want[p]
		if !ok {
			t.Errorf("unexpected or repeated provider %q in %v", p, r)
			continue
		}
		delete(want, p)
		if r["rows"] != w[0] || r["cost_usd"] != w[1] {
			t.Errorf("%s: rows/cost_usd = %v/%v, want %v/%v", p, r["rows"], r["cost_usd"], w[0], w[1])
		}
		if r["remedy"] != api.PolledProviderOverlapRemedy {
			t.Errorf("%s: remedy = %v, want api.PolledProviderOverlapRemedy", p, r["remedy"])
		}
	}
}

// TestServe_WarnsUndeclaredManualCostsAfterPollersStart pins the call site: serve
// calls warnUndeclaredManualCosts(…, startedPollers(adminSettings,
// openaiSettings)) once, as a direct statement of its body, after the END of
// both poller start blocks (so neither block's condition gates it).
func TestServe_WarnsUndeclaredManualCostsAfterPollersStart(t *testing.T) {
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
	var lastEnd, call token.Pos
	calls := 0
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if b, ok := n.Cond.(*ast.BinaryExpr); ok && b.Op == token.NEQ {
				if id, ok := b.X.(*ast.Ident); ok && (id.Name == "adminSettings" || id.Name == "openaiSettings") && n.End() > lastEnd {
					lastEnd = n.End()
				}
			}
		case *ast.CallExpr:
			fn, ok := n.Fun.(*ast.Ident)
			if !ok || fn.Name != "warnUndeclaredManualCosts" || len(n.Args) != 4 {
				return true
			}
			inner, ok := n.Args[3].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "startedPollers" && len(inner.Args) == 2 {
				a, _ := inner.Args[0].(*ast.Ident)
				o, _ := inner.Args[1].(*ast.Ident)
				if a != nil && o != nil && a.Name == "adminSettings" && o.Name == "openaiSettings" {
					calls++
					call = n.Pos()
				}
			}
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("serve calls warnUndeclaredManualCosts(…, startedPollers(adminSettings, openaiSettings)) %d times, want 1 (#854)", calls)
	}
	if lastEnd == token.NoPos || call < lastEnd {
		t.Error("warnUndeclaredManualCosts is not after the end of the last poller start block; it must run once both have started (#854)")
	}
	direct := false
	for _, st := range serve.Body.List {
		if es, ok := st.(*ast.ExprStmt); ok && es.X.Pos() == call {
			direct = true
		}
	}
	if !direct {
		t.Error("warnUndeclaredManualCosts is nested under a conditional; it must be a direct statement of runServeWithOptions so every started poller is warned about (#854)")
	}
}
