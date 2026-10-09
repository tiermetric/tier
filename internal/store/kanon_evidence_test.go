package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDeveloperEvidenceWindow_SplitsCapturedFromManual pins the k-anonymity
// census's evidence read (#856): per raw id, source='api' rows are manual and
// every other source is captured, inside the half-open window only.
func TestDeveloperEvidenceWindow_SplitsCapturedFromManual(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	insCost := func(dev, source, fidelity string, cost int64, ts time.Time) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: dev, IssueID: "i-1", Model: "claude-sonnet-4", InputTok: 10,
			CostMicro: cost, Source: source, Fidelity: fidelity, Timestamp: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(dev, source string, ts time.Time) { t.Helper(); insCost(dev, source, "realtime", 1000, ts) }
	ins("both", "jsonl", now)
	ins("both", "api", now)
	insCost("both", "anthropic-admin", "daily", 500, now)
	insCost("both", "api", "daily", 700, now)
	ins("manual", "api", now)
	ins("manual", "api", now)
	ins("proxy", "proxy", now)
	ins("old", "jsonl", now.AddDate(0, 0, -30)) // outside the window

	got, err := db.DeveloperEvidenceWindow(ctx, now.AddDate(0, 0, -1), time.Time{}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	want := []DeveloperCostEvidence{
		{Developer: "both", CapturedRows: 2, ManualRows: 2},
		{Developer: "manual", CapturedRows: 0, ManualRows: 2},
		{Developer: "proxy", CapturedRows: 1, ManualRows: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeveloperEvidenceWindow = %+v, want %+v", got, want)
	}
}

// TestDeveloperIssueCostsWindow_SplitsCapturedCostByShare pins the per-row input
// of the k-anonymity share check (#943): each (developer, repo, issue) row
// carries the captured part of its realtime and non-realtime cost, with the
// manual source='api' rows excluded from both and every other source (proxy,
// openai-usage) and non-realtime fidelity (daily, estimated) included.
func TestDeveloperIssueCostsWindow_SplitsCapturedCostByShare(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []struct {
		source, fidelity string
		cost             int64
	}{
		{"jsonl", "realtime", 1000}, {"proxy", "realtime", 2000}, {"api", "realtime", 300},
		{"anthropic-admin", "daily", 500}, {"openai-usage", "estimated", 4000}, {"api", "daily", 700},
	} {
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "dev", IssueID: "i-1", Model: "claude-sonnet-4", InputTok: 10,
			CostMicro: e.cost, Source: e.source, Fidelity: e.fidelity, Timestamp: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.DeveloperIssueCostsWindow(ctx, now.AddDate(0, 0, -1), time.Time{}, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	want := []DevIssueCost{{Developer: "dev", Repo: "unqualified", IssueID: "i-1", TotalCostMicro: 8500,
		RealtimeCostMicro: 3300, CapturedRealtimeMicro: 3000, CapturedNonRealtimeMicro: 4500}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeveloperIssueCostsWindow = %+v, want %+v", got, want)
	}
}

// TestBotDevelopers_ReadsCapturedAuthorType pins the bot half of the census
// (#856): InsertOutcome stores the GitHub account type from its closed set, an
// unknown value is stored as not-captured, BotDevelopers returns exactly the
// "Bot" authors with an outcome in the window, and the DSAR export carries the
// column.
func TestBotDevelopers_ReadsCapturedAuthorType(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	in := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for i, o := range []Outcome{
		{Developer: "Copilot", AuthorType: AuthorTypeBot, Timestamp: in},
		{Developer: "alice", AuthorType: AuthorTypeUser, Timestamp: in},
		{Developer: "forged", AuthorType: "bot", Timestamp: in}, // not in the closed set: stored NULL
		{Developer: "legacy", Timestamp: in},
		{Developer: "later-bot", AuthorType: AuthorTypeBot, Timestamp: in.AddDate(0, 1, 0)},
	} {
		o.IssueID, o.Weight, o.Quality = "i-1", 1, 1
		o.MergeCommitSHA = strings.Repeat("a", 39) + string(rune('0'+i))
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	bots, err := db.BotDevelopers(ctx, in.AddDate(0, 0, -1), in.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bots, []string{"Copilot"}) {
		t.Errorf("BotDevelopers = %v, want [Copilot] (later-bot merged after the window)", bots)
	}
	bots, err = db.BotDevelopers(ctx, in.AddDate(0, 0, -1), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bots, []string{"Copilot", "later-bot"}) {
		t.Errorf("open-ended BotDevelopers = %v, want [Copilot later-bot]", bots)
	}

	exp, err := db.ExportDeveloper(ctx, "forged")
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Outcomes) != 1 || exp.Outcomes[0].AuthorType != nil {
		t.Errorf("an unknown author type must be stored as NULL; export = %+v", exp.Outcomes)
	}
	exp, err = db.ExportDeveloper(ctx, "Copilot")
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Outcomes) != 1 || exp.Outcomes[0].AuthorType == nil || *exp.Outcomes[0].AuthorType != AuthorTypeBot {
		t.Errorf("export must carry author_type; got %+v", exp.Outcomes)
	}
}
