package main

import (
	"bytes"
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
)

// gatedSourceConsts is each seal-gated source's name constant, as serve's source
// writes spell it, and its value.
var gatedSourceConsts = map[string]string{
	"SourceAnthropicAdmin":  collector.SourceAnthropicAdmin,
	"SourceOpenAIUsage":     collector.SourceOpenAIUsage,
	"subscriptionFeeSource": subscriptionFeeSource,
	"SourceCodexRollout":    collector.SourceCodexRollout,
	"SourceOpencode":        collector.SourceOpencode,
	"SourceMuse":            collector.SourceMuse,
}

// TestSealGatedSources_EveryConfiguredSource (#913-D9 condition 3): every
// pulled source serve can start is registered when configured, and none when
// nothing is.
func TestSealGatedSources_EveryConfiguredSource(t *testing.T) {
	all := sealGatedSources(&anthropicAdminSettings{}, &openAIUsageSettings{}, []config.Subscription{{RoutePrefix: "p"}},
		&codexRolloutSettings{}, &opencodeSettingsT{}, &museSettingsT{})
	var want []string
	for _, v := range gatedSourceConsts {
		want = append(want, v)
	}
	slices.Sort(want)
	got := slices.Sorted(slices.Values(all))
	if !slices.Equal(got, want) {
		t.Errorf("every source configured: %v, want %v", got, want)
	}
	if none := sealGatedSources(nil, nil, nil, nil, nil, nil); len(none) != 0 {
		t.Errorf("nothing configured: %v, want none", none)
	}
}

// TestServe_EverySealGatedSourceSettles (#913-D9 condition 6): in
// runServeWithOptions, each source serve starts is given sealSettled for its
// own name, serve registers sealGatedSources before the first of them, and
// each argument of that call is the settings variable the source it registers
// is started from. Deleting one source's Settled, naming the wrong source, or
// passing another value for a source's settings fails here. Each local
// collector is also given sealLost for its own name (#913-D9 ruling R-8).
func TestServe_EverySealGatedSourceSettles(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites := map[string]string{
		"anthropicadmin.PollerConfig":  "SourceAnthropicAdmin",
		"openaiusage.PollerConfig":     "SourceOpenAIUsage",
		"codexrollout.Config":          "SourceCodexRollout",
		"opencode.Config":              "SourceOpencode",
		"muse.Config":                  "SourceMuse",
		"runSubscriptionFeeReconciler": "subscriptionFeeSource",
	}
	// Each site's settings variable, in sealGatedSources' parameter order.
	settings := []struct{ site, name string }{
		{"anthropicadmin.PollerConfig", "adminSettings"}, {"openaiusage.PollerConfig", "openaiSettings"},
		{"runSubscriptionFeeReconciler", "subscriptionsCfg"}, {"codexrollout.Config", "codexSettings"},
		{"opencode.Config", "opencodeSettings"}, {"muse.Config", "museSettings"},
	}
	reads := map[string]map[string]bool{}
	identsIn := func(n ast.Node) map[string]bool {
		ids := map[string]bool{}
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				ids[id.Name] = true
			}
			return true
		})
		return ids
	}
	var gatedArgs []string
	settled, lost := map[string]string{}, map[string]string{}
	var registerAt, firstSettled token.Pos
	settledOf := func(e ast.Expr) string {
		c, ok := e.(*ast.CallExpr)
		if !ok || exprName(c.Fun) != "sealSettled" || len(c.Args) != 3 {
			return ""
		}
		if firstSettled == token.NoPos || c.Pos() < firstSettled {
			firstSettled = c.Pos()
		}
		if sel, ok := c.Args[1].(*ast.SelectorExpr); ok {
			return sel.Sel.Name
		}
		return exprName(c.Args[1])
	}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "runServeWithOptions" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				sel, ok := n.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				site := exprName(sel.X) + "." + sel.Sel.Name
				if _, gated := sites[site]; !gated {
					return true
				}
				settled[site] = "(none)"
				reads[site] = identsIn(n)
				for _, el := range n.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if exprName(kv.Key) == "Settled" {
						settled[site] = settledOf(kv.Value)
					}
					if c, isCall := kv.Value.(*ast.CallExpr); isCall && exprName(kv.Key) == "Lost" && exprName(c.Fun) == "sealLost" && len(c.Args) == 2 {
						if sel, isSel := c.Args[1].(*ast.SelectorExpr); isSel {
							lost[site] = sel.Sel.Name
						}
					}
				}
			case *ast.CallExpr:
				switch exprName(n.Fun) {
				case "runSubscriptionFeeReconciler":
					settled["runSubscriptionFeeReconciler"] = settledOf(n.Args[len(n.Args)-1])
					reads["runSubscriptionFeeReconciler"] = identsIn(n)
				case "sealGatedSources":
					for _, a := range n.Args {
						gatedArgs = append(gatedArgs, exprName(a))
					}
				case "registerSealSources":
					registerAt = n.Pos()
				}
			}
			return true
		})
	}
	for site, want := range sites {
		if settled[site] != want {
			t.Errorf("%s in runServeWithOptions settles %q, want sealSettled(db, %s, logger)", site, settled[site], want)
		}
	}
	for _, site := range []string{"codexrollout.Config", "opencode.Config", "muse.Config"} {
		if lost[site] != sites[site] {
			t.Errorf("%s in runServeWithOptions records lost spend as %q, want sealLost(db, %s)", site, lost[site], sites[site])
		}
	}
	if len(gatedArgs) != len(settings) {
		t.Fatalf("sealGatedSources is called with %v, want %d settings", gatedArgs, len(settings))
	}
	for i, st := range settings {
		if gatedArgs[i] != st.name || !reads[st.site][st.name] {
			t.Errorf("sealGatedSources argument %d is %q; want %s, which %s is started from (it reads it: %v)",
				i, gatedArgs[i], st.name, st.site, reads[st.site][st.name])
		}
	}
	if registerAt == token.NoPos || firstSettled == token.NoPos || registerAt > firstSettled {
		t.Errorf("registerSealSources at %s, first source at %s: serve must register the gated sources before starting any",
			fset.Position(registerAt), fset.Position(firstSettled))
	}
}

// TestSealSettled_AdvancesItsSourcesRow: registering names at WARN a source it
// retires; the hook serve gives a source advances that source's row in a real
// store; a source serve never registered is refused, logged, and writes
// nothing.
func TestSealSettled_AdvancesItsSourcesRow(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	if _, err := db.RegisterSources(ctx, []string{collector.SourceAnthropicAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := registerSealSources(ctx, db, []string{collector.SourceMuse}, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "sources=["+collector.SourceMuse+"]") {
		t.Errorf("startup log %q does not name the gated sources", logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "retired=["+collector.SourceAnthropicAdmin+"]") {
		t.Errorf("startup log %q does not name the retired source at WARN", logs.String())
	}
	logs.Reset()
	from, through := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, time.June, 3, 0, 0, 0, 0, time.UTC)
	sealSettled(db, collector.SourceMuse, logger)(ctx, from, through)
	sealSettled(db, collector.SourceCodexRollout, logger)(ctx, from, through)
	rows, err := db.SourceWatermarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Source != collector.SourceMuse || !rows[1].SettledThrough.Equal(through) || rows[0].Settled {
		t.Errorf("rows %+v, want muse settled through %s and nothing else", rows, through)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), collector.SourceCodexRollout) {
		t.Errorf("an unregistered source's settle was not logged: %q", logs.String())
	}
	if err := sealLost(db, collector.SourceMuse)(ctx, from, from); err != nil {
		t.Fatal(err)
	}
	if rows, err = db.SourceWatermarks(ctx); err != nil || len(rows[1].Lost) != 1 || !rows[1].Lost[0].From.Equal(from) {
		t.Errorf("rows %+v, %v; want muse's lost span recorded from %s", rows, err, from)
	}
}

// TestSubscriptionReconciler_SettledOnlyWhenEveryPostSucceeds: a pass reports
// the latest of the subscriptions' first periods through the end of the
// current one only when every post succeeded.
func TestSubscriptionReconciler_SettledOnlyWhenEveryPostSucceeds(t *testing.T) {
	current, prev1 := monthsBack(0), monthsBack(1)
	requireDistinctPeriods(t, prev1, current)
	subs := []config.Subscription{
		{RoutePrefix: "glm@h", Org: "acme", MonthlyFeeUSD: 1, ActiveSince: prev1},
		{RoutePrefix: "bad@host", Org: "acme", MonthlyFeeUSD: 1},
	}
	if first, ok := reconcileSubscriptionFeesOnce(context.Background(), &fakeSubscriptionStore{}, subs, quietLogger(), current, true); !ok || first != current {
		t.Errorf("clean pass: first %q ok %v, want %s and true (the second subscription starts at the current period)", first, ok, current)
	}
	prev2 := monthsBack(2)
	backfilled := []config.Subscription{subs[0], {RoutePrefix: "glm@k", Org: "acme", MonthlyFeeUSD: 1, ActiveSince: prev2}}
	if first, ok := reconcileSubscriptionFeesOnce(context.Background(), &fakeSubscriptionStore{}, backfilled, quietLogger(), current, true); !ok || first != prev2 {
		t.Errorf("every subscription backfilled: first %q ok %v, want the earliest active_since %s", first, ok, prev2)
	}
	if _, ok := reconcileSubscriptionFeesOnce(context.Background(), &fakeSubscriptionStore{failOn: "bad@host"}, subs, quietLogger(), current, true); ok {
		t.Error("a pass with a failed post reported success")
	}

	got := make(chan [2]time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSubscriptionFeeReconciler(ctx, &fakeSubscriptionStore{}, subs[:1], quietLogger(), time.Hour,
		func(_ context.Context, from, through time.Time) { got <- [2]time.Time{from, through} })
	start, _ := time.Parse("2006-01", prev1)
	end, _ := time.Parse("2006-01", current)
	select {
	case w := <-got:
		if !w[0].Equal(start) || !w[1].Equal(end.AddDate(0, 1, 0)) {
			t.Errorf("startup pass settled %v, want %s through the end of %s", w, start, current)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the startup pass never settled")
	}
}

// TestSealCmd_ArmRefusedWhileASourceIsBehind (#913-D9 condition 1): `tierd seal
// --arm` refuses, exit 1, a month a source serve registered has not settled
// past, naming the source and sealing nothing; --status names it from the
// database alone.
func TestSealCmd_ArmRefusedWhileASourceIsBehind(t *testing.T) {
	path := sealTestDB(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterSources(context.Background(), []string{collector.SourceAnthropicAdmin}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	rc, out, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path)
	if rc != sealExitRefused || !strings.Contains(errOut, "refused") || !strings.Contains(errOut, collector.SourceAnthropicAdmin) {
		t.Errorf("arm: rc %d, stdout %q, stderr %q; want %d naming %s", rc, out, errOut, sealExitRefused, collector.SourceAnthropicAdmin)
	}
	if sealed, floor := sealState(t, path); sealed != "" || floor != "" {
		t.Errorf("refused arm left sealed %q, floor %q; want nothing", sealed, floor)
	}
	_, out, _ = runSeal(t, "", "--status", "--aggregation", "team", "--seal-from", "2020-01", "--db", path)
	if !strings.Contains(out, "sources behind:") || !strings.Contains(out, collector.SourceAnthropicAdmin+" has not completed a successful pass") {
		t.Errorf("--status: %q; want the behind source named", out)
	}
}

// TestSealCmd_ArmRefusedBeforeServeRegistered (#913-D9 condition 3): on a
// database no serve has registered its sources on, `tierd seal --arm` refuses,
// exit 1, sealing nothing, whatever its own config holds.
func TestSealCmd_ArmRefusedBeforeServeRegistered(t *testing.T) {
	path := sealTestDB(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM source_registration`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	rc, out, errOut := runSeal(t, "", "--arm", "earliest", "--yes", "--aggregation", "team", "--db", path)
	if rc != sealExitRefused || !strings.Contains(errOut, "has not registered its sources") {
		t.Errorf("arm: rc %d, stdout %q, stderr %q; want %d naming the missing registration", rc, out, errOut, sealExitRefused)
	}
	if sealed, floor := sealState(t, path); sealed != "" || floor != "" {
		t.Errorf("refused arm left sealed %q, floor %q; want nothing", sealed, floor)
	}
}
