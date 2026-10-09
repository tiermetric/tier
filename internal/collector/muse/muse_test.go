package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// The fixture (testdata/session.jsonl) is SYNTHETIC: invented ids, paths and
// numbers in the record structure measured on Muse Code 1.4.0. Its text fields
// carry the marker "LEAKMARK" next to identifier-shaped strings so the
// allowlist test can prove none of them reaches an event.
const (
	fixtureRoot    = "/WORKSPACE_ROOT"
	fixtureSession = "ses-fixture-0001"
	fixtureBranch  = "feature/895-muse-fixture"
	wantIssueID    = "issue-895"
	leakMarker     = "LEAKMARK"
	fixtureModel   = "muse-spark-1.3-contributor"
	fixtureRun     = "run-fixture-a"
)

// fixtureWant is every billed record in the fixture and the classes it must map
// to. rec-0007 carries cache_write_tokens=1000, priced inside input.
var fixtureWant = map[string]tokens{
	"rec-fixture-0005": {Input: 12000, CacheRead: 8000, Output: 500}, // agent: 20000-8000
	"rec-fixture-0007": {Input: 5000, CacheRead: 19000, Output: 200}, // agent: 24000-19000, write inside input
	"rec-fixture-0009": {Input: 26000, CacheRead: 4000, Output: 400}, // review: non_cached
	"rec-fixture-0011": {Input: 1000, CacheRead: 25000, Output: 150}, // agent: 26000-25000
}

// fixtureCostMicro is each fixture record's cost computed BY HAND from Meta's
// published muse-spark-1.3-contributor rate — $0.10/M input, $0.20/M output,
// $0.002/M cached input — never from store.ComputeCostHost, so a missing price
// row (the $0.50/M fallback) or a wrong one fails here.
var fixtureCostMicro = map[string]int64{
	"rec-fixture-0005": 1200 + 16 + 100, // 12000×0.10 + 8000×0.002 + 500×0.20
	"rec-fixture-0007": 500 + 38 + 40,   // 5000×0.10 + 19000×0.002 + 200×0.20
	"rec-fixture-0009": 2600 + 8 + 80,   // 26000×0.10 + 4000×0.002 + 400×0.20
	"rec-fixture-0011": 100 + 50 + 30,   // 1000×0.10 + 25000×0.002 + 150×0.20
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// initGitRepo makes dir a real git checkout; NewIssueResolver runs `git log`.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

func readFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

// writeSession writes body as <home>/sessions/2026/09/21/<dir>/session.jsonl,
// substituting the workspace root, and returns the path.
func writeSession(t *testing.T, home, dir, root, body string) string {
	t.Helper()
	d := filepath.Join(home, "sessions", "2026", "09", "21", dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "session.jsonl")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, fixtureRoot, root)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fixtureLinesWithout returns the fixture with every line containing any of
// the given substrings removed.
func fixtureLinesWithout(t *testing.T, drop ...string) string {
	t.Helper()
	var keep []string
	for _, l := range strings.SplitAfter(readFixture(t), "\n") {
		skip := false
		for _, d := range drop {
			if strings.Contains(l, d) {
				skip = true
			}
		}
		if !skip && l != "" {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "")
}

func newCollector(t *testing.T, home string, repos []string, now func() time.Time) *Collector {
	t.Helper()
	targets := make([]RepoTarget, len(repos))
	for i, r := range repos {
		targets[i] = RepoTarget{Path: r}
	}
	c, err := New(Config{Home: home, Repos: targets, DeveloperID: "dev-fixture", Logger: quietLogger(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func byKey(evs []collector.TokenEvent) map[string]collector.TokenEvent {
	m := make(map[string]collector.TokenEvent, len(evs))
	for _, e := range evs {
		m[e.IdempotencyKey] = e
	}
	return m
}

// TestCollect_FixtureEndToEnd: one event per billed record, each mapped, keyed,
// attributed to the run's recorded branch, and priced.
func TestCollect_FixtureEndToEnd(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, repo, readFixture(t))
	c := newCollector(t, home, []string{repo}, nil)

	evs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(evs) != len(fixtureWant) {
		t.Fatalf("got %d events, want %d (3 model_completed + 1 automated_review_completed)", len(evs), len(fixtureWant))
	}
	got := byKey(evs)
	for id, want := range fixtureWant {
		key := collector.IdempotencyKey(collector.SourceMuse, providerMeta, id)
		e, ok := got[key]
		if !ok {
			t.Errorf("no event keyed on record %s", id)
			continue
		}
		if e.InputTok != want.Input || e.CacheRead != want.CacheRead || e.OutputTok != want.Output || e.CacheWrite5m != 0 || e.CacheWrite1h != 0 {
			t.Errorf("%s: tokens in=%d read=%d out=%d w5m=%d w1h=%d, want %+v and no cache-write class",
				id, e.InputTok, e.CacheRead, e.OutputTok, e.CacheWrite5m, e.CacheWrite1h, want)
		}
		if e.CostMicro != fixtureCostMicro[id] {
			t.Errorf("%s: cost_micro=%d, want %d at the published Contributor rate", id, e.CostMicro, fixtureCostMicro[id])
		}
		if e.Source != collector.SourceMuse || e.Fidelity != collector.FidelityRealtime {
			t.Errorf("%s: source/fidelity = %q/%q", id, e.Source, e.Fidelity)
		}
		if e.IssueID != wantIssueID {
			t.Errorf("%s: IssueID = %q, want %q from the run's workspace_branch %q", id, e.IssueID, wantIssueID, fixtureBranch)
		}
		if e.Model != fixtureModel || e.SessionID != fixtureSession || e.Developer != "dev-fixture" || e.Host != "" {
			t.Errorf("%s: model=%q session=%q developer=%q host=%q", id, e.Model, e.SessionID, e.Developer, e.Host)
		}
		if e.Timestamp.IsZero() || e.Timestamp.Location() != time.UTC {
			t.Errorf("%s: timestamp %v must be non-zero UTC", id, e.Timestamp)
		}
	}
}

// TestTokenMapping_MeasuredNumbers pins the mapping with the figures measured on
// a real Muse session (#895): cached input carved out of input, reasoning left
// inside output, cache writes left inside input.
func TestTokenMapping_MeasuredNumbers(t *testing.T) {
	i := func(v int64) *int64 { return &v }

	// model_completed: input 23142 INCLUDES cache_read 11505; reasoning 507 is
	// INSIDE output 676.
	agent, wrote, err := mapAgentUsage(agentUsage{
		InputTokens: i(23142), OutputTokens: i(676), CachedTokens: i(11505),
		CacheReadTokens: i(11505), CacheWriteTokens: i(0), ReasoningTokens: i(507),
	})
	if err != nil || wrote {
		t.Fatalf("mapAgentUsage: err=%v wrote=%v", err, wrote)
	}
	if want := (tokens{Input: 23142 - 11505, CacheRead: 11505, Output: 676}); agent != want {
		t.Errorf("agent = %+v, want %+v (never input 23142, never output 676+507)", agent, want)
	}

	// automated_review_completed: input 36633 = cached_input 4721 +
	// non_cached_input 31912; total 37061 = input + output 428 (reasoning 266
	// inside output).
	review, err := mapReviewUsage(reviewUsage{
		InputTokens: i(36633), CachedInputTokens: i(4721), NonCachedInputTokens: i(31912),
		OutputTokens: i(428), ReasoningTokens: i(266), TotalTokens: i(37061),
	})
	if err != nil {
		t.Fatalf("mapReviewUsage: %v", err)
	}
	if want := (tokens{Input: 31912, CacheRead: 4721, Output: 428}); review != want {
		t.Errorf("review = %+v, want %+v", review, want)
	}

	// The cached prefix is priced ONCE. Pricing the raw input as well would be
	// the double-count this mapping exists to prevent — prove the two differ.
	once, _ := store.ComputeCostHost("", fixtureModel, store.CostUsage{Input: agent.Input, CacheRead: agent.CacheRead, Output: agent.Output})
	twice, _ := store.ComputeCostHost("", fixtureModel, store.CostUsage{Input: 23142, CacheRead: 11505, Output: 676})
	if once >= twice {
		t.Errorf("carved cost %d must be below the double-priced %d", once, twice)
	}

	// Cache writes: taken to sit INSIDE input and priced there. Not carved out,
	// not added, and flagged.
	w, wrote, err := mapAgentUsage(agentUsage{InputTokens: i(1000), CacheReadTokens: i(300), CacheWriteTokens: i(200), OutputTokens: i(10)})
	if err != nil || !wrote {
		t.Fatalf("a cache write inside input must map and be flagged: err=%v wrote=%v", err, wrote)
	}
	if want := (tokens{Input: 700, CacheRead: 300, Output: 10}); w != want {
		t.Errorf("cache-write record = %+v, want %+v (write stays in input, counted once)", w, want)
	}

	// cached_tokens is the fallback only when cache_read_tokens is absent.
	fb, _, err := mapAgentUsage(agentUsage{InputTokens: i(100), CachedTokens: i(40), OutputTokens: i(1)})
	if err != nil || fb.CacheRead != 40 || fb.Input != 60 {
		t.Errorf("cached_tokens fallback = %+v err=%v, want read 40 input 60", fb, err)
	}
}

// TestTokenMapping_RefusesContradictions: every identity the mapping relies on
// refuses the record rather than pricing a guess.
func TestTokenMapping_RefusesContradictions(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	agent := map[string]agentUsage{
		"read_exceeds_input":         {InputTokens: i(10), CacheReadTokens: i(11), OutputTokens: i(1)},
		"read_plus_write_exceeds":    {InputTokens: i(10), CacheReadTokens: i(6), CacheWriteTokens: i(5), OutputTokens: i(1)},
		"cached_disagrees_with_read": {InputTokens: i(24000), CacheReadTokens: i(0), CachedTokens: i(19000), OutputTokens: i(1)},
		"reasoning_exceeds_output":   {InputTokens: i(10), OutputTokens: i(5), ReasoningTokens: i(6)},
		"negative":                   {InputTokens: i(-1), OutputTokens: i(5)},
		"over_ceiling":               {InputTokens: i(maxRecordTokens + 1), OutputTokens: i(5)},
		"input_absent":               {OutputTokens: i(500)},
		"output_absent":              {InputTokens: i(500)},
		"usage_empty":                {},
	}
	for name, u := range agent {
		if _, _, err := mapAgentUsage(u); err == nil {
			t.Errorf("agent %s: mapped, want refusal", name)
		}
	}
	review := map[string]reviewUsage{
		"split_disagrees":          {InputTokens: i(100), CachedInputTokens: i(40), NonCachedInputTokens: i(50), OutputTokens: i(1)},
		"total_disagrees":          {InputTokens: i(100), CachedInputTokens: i(40), OutputTokens: i(10), TotalTokens: i(120)},
		"cached_exceeds_input":     {InputTokens: i(10), CachedInputTokens: i(11), OutputTokens: i(1)},
		"reasoning_exceeds_output": {InputTokens: i(10), OutputTokens: i(1), ReasoningTokens: i(2)},
		"input_absent":             {OutputTokens: i(500)},
		"output_absent":            {InputTokens: i(500)},
		"usage_empty":              {},
	}
	for name, u := range review {
		if _, err := mapReviewUsage(u); err == nil {
			t.Errorf("review %s: mapped, want refusal", name)
		}
	}
}

// TestNoTextReachesAnEvent: the fixture's text fields hold identifier-shaped
// strings (a GitHub token shape, an AWS key prefix, an sk- key, a home path)
// beside the LEAKMARK marker, including inside the billed records themselves.
// None may reach an event, and neither may the workspace path.
func TestNoTextReachesAnEvent(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, repo, readFixture(t))
	if !strings.Contains(readFixture(t), leakMarker) {
		t.Fatal("control: the fixture carries no marker, so this test would pass vacuously")
	}
	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil || len(evs) == 0 {
		t.Fatalf("Collect: %d events, err %v", len(evs), err)
	}
	for _, e := range evs {
		dump := fmt.Sprintf("%#v", e)
		for _, bad := range []string{leakMarker, "ghp_", "AKIA", "sk-live", "/home/someone", repo} {
			if strings.Contains(dump, bad) {
				t.Errorf("event carries %q: %s", bad, dump)
			}
		}
	}
}

// TestDecoderAllowlist pins every JSON field this package can decode. Adding a
// field to any decode struct fails here until it is listed — and a text field
// (prompt, reply, tool args or output) must never be.
func TestDecoderAllowlist(t *testing.T) {
	want := []string{
		"id", "recorded_at", "stream.id", "payload.kind", "payload.run_id", "payload.event.kind",
		"payload.event.model", "payload.event.model.model_id", "payload.event.completed_at_ms",
		"payload.event.usage.input_tokens", "payload.event.usage.output_tokens",
		"payload.event.usage.cached_tokens", "payload.event.usage.cache_read_tokens",
		"payload.event.usage.cache_write_tokens", "payload.event.usage.reasoning_tokens",
		"payload.event.usage.cached_input_tokens", "payload.event.usage.non_cached_input_tokens",
		"payload.event.usage.total_tokens",
		"payload.event.parent_run_id", "payload.event.child_session_id", // link record: ids only (#901)
		"payload.record.workspace_root", "payload.record.command_id",
		"payload.record.reference.kind", "payload.record.reference.name", "payload.record.vcs",
	}
	got := map[string]bool{}
	for _, v := range []any{envelope{}, agentLine{}, reviewLine{}, metadataLine{}, branchLine{}, linkLine{}} {
		collectTags(reflect.TypeOf(v), "", got)
	}
	var gotList []string
	for k := range got {
		gotList = append(gotList, k)
	}
	sort.Strings(gotList)
	sort.Strings(want)
	if !reflect.DeepEqual(gotList, want) {
		t.Errorf("decodable fields changed:\n got  %v\n want %v", gotList, want)
	}
}

func collectTags(tp reflect.Type, prefix string, out map[string]bool) {
	for tp.Kind() == reflect.Pointer {
		tp = tp.Elem()
	}
	if tp.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" && f.Anonymous {
			collectTags(f.Type, prefix, out)
			continue
		}
		if name == "" {
			// encoding/json decodes an untagged exported field under its Go
			// name, so it is as reachable as a tagged one.
			name = f.Name
		}
		path := strings.TrimPrefix(prefix+"."+name, ".")
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			collectTags(ft, path, out)
			continue
		}
		out[path] = true
	}
}

// TestCollectTags_SeesUntaggedFields pins the allowlist walker itself: an
// untagged or name-less-tagged field decodes under its Go name, so it must be
// reported, or TestDecoderAllowlist is blind to a text field added that way.
func TestCollectTags_SeesUntaggedFields(t *testing.T) {
	type probe struct {
		Payload struct {
			Content string
			Reply   string `json:",omitempty"`
			Skipped string `json:"-"`
		} `json:"payload"`
	}
	got := map[string]bool{}
	collectTags(reflect.TypeOf(probe{}), "", got)
	for _, want := range []string{"payload.Content", "payload.Reply"} {
		if !got[want] {
			t.Errorf("collectTags missed %s (got %v)", want, got)
		}
	}
	if got["payload.Skipped"] || got["payload.-"] {
		t.Errorf("collectTags reported a json:\"-\" field (got %v)", got)
	}
}

// memIngester stores events keyed like the store's unique index.
type memIngester struct {
	mu     sync.Mutex
	byKey  map[string]collector.TokenEvent
	ingest int
}

func (m *memIngester) Ingest(_ context.Context, e collector.TokenEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byKey == nil {
		m.byKey = map[string]collector.TokenEvent{}
	}
	m.byKey[e.IdempotencyKey] = e
	m.ingest++
	return nil
}

// TestDedup_ReReadAndDuplicateRecord: re-reading the same file yields the same
// keys (so the store holds each call once), and a record repeated inside one
// file is emitted once.
func TestDedup_ReReadAndDuplicateRecord(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	body := readFixture(t)
	var dup string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, `"id":"rec-fixture-0005"`) {
			dup = l + "\n"
		}
	}
	if dup == "" {
		t.Fatal("control: fixture record rec-fixture-0005 not found")
	}
	writeSession(t, home, fixtureSession, repo, body+dup)
	c := newCollector(t, home, []string{repo}, nil)

	ing := &memIngester{}
	c.runPass(context.Background(), time.Time{}, ing)
	c.runPass(context.Background(), time.Time{}, ing)
	first, _ := c.Collect(context.Background(), time.Time{})
	second, _ := c.Collect(context.Background(), time.Time{})
	if len(ing.byKey) != len(fixtureWant) {
		t.Errorf("store would hold %d rows after two passes, want %d", len(ing.byKey), len(fixtureWant))
	}
	if len(first) != len(fixtureWant) || !reflect.DeepEqual(byKey(first), byKey(second)) {
		t.Errorf("re-read changed the result: %d then %d events", len(first), len(second))
	}
}

// TestBranchAttribution_HeldUntilSettled: the branch arrives AFTER a run's
// calls, and the store never rewrites issue_id, so an unsettled run must not be
// emitted — and each settle rule must release it.
func TestBranchAttribution_HeldUntilSettled(t *testing.T) {
	repo := initGitRepo(t)
	ctx := context.Background()

	t.Run("no_terminal_no_branch_is_held", func(t *testing.T) {
		home := t.TempDir()
		writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
		evs, err := newCollector(t, home, []string{repo}, nil).Collect(ctx, time.Time{})
		if err != nil || len(evs) != 0 {
			t.Fatalf("a running run emitted %d events (err %v); it must be held until its branch is known", len(evs), err)
		}
	})
	t.Run("abandoned_run_emits_unattributed_after_grace", func(t *testing.T) {
		home := t.TempDir()
		writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
		later := func() time.Time { return time.Now().Add(settleGrace + time.Minute) }
		evs, err := newCollector(t, home, []string{repo}, later).Collect(ctx, time.Time{})
		if err != nil || len(evs) != len(fixtureWant) {
			t.Fatalf("got %d events (err %v), want %d once the file is quiescent", len(evs), err, len(fixtureWant))
		}
		for _, e := range evs {
			if e.IssueID != collector.UnattributedDetachedHEAD {
				t.Errorf("IssueID = %q, want %q: no branch was recorded, so none may be guessed", e.IssueID, collector.UnattributedDetachedHEAD)
			}
		}
	})
	t.Run("terminal_as_last_record_is_held", func(t *testing.T) {
		home := t.TempDir()
		writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"workspace_branch"`))
		evs, _ := newCollector(t, home, []string{repo}, nil).Collect(ctx, time.Time{})
		if len(evs) != 0 {
			t.Fatalf("emitted %d events between the terminal and its workspace_branch record", len(evs))
		}
	})
	t.Run("retained_frame_after_terminal_is_held", func(t *testing.T) {
		home := t.TempDir()
		body := fixtureLinesWithout(t, `"kind":"workspace_branch"`) +
			`{"retained_frame":"fixture-frame-2","frame_schema_version":1,"outer_log_ordinal":2,"transaction_id":"txn-fixture-2","children":[],"content_sha256":"sha256:fixture"}` + "\n"
		writeSession(t, home, "s", repo, body)
		evs, _ := newCollector(t, home, []string{repo}, nil).Collect(ctx, time.Time{})
		if len(evs) != 0 {
			t.Fatalf("an id-less retained_frame after the terminal released %d events as branchless", len(evs))
		}
	})
	t.Run("terminal_followed_by_another_record_emits_unattributed", func(t *testing.T) {
		home := t.TempDir()
		body := fixtureLinesWithout(t, `"kind":"workspace_branch"`) +
			`{"id":"rec-fixture-0099","recorded_at":1790000099000000,"stream":{"kind":"session","id":"ses-fixture-0001"},"payload":{"kind":"run","run_id":"run-fixture-b","event":{"kind":"started"}}}` + "\n"
		writeSession(t, home, "s", repo, body)
		evs, _ := newCollector(t, home, []string{repo}, nil).Collect(ctx, time.Time{})
		if len(evs) != len(fixtureWant) {
			t.Fatalf("got %d events, want %d", len(evs), len(fixtureWant))
		}
		for _, e := range evs {
			if e.IssueID != collector.UnattributedDetachedHEAD {
				t.Errorf("IssueID = %q, want %q", e.IssueID, collector.UnattributedDetachedHEAD)
			}
		}
	})
	t.Run("detached_head_branch_record_is_unattributed", func(t *testing.T) {
		home := t.TempDir()
		body := strings.Replace(readFixture(t), `"reference":{"kind":"branch"`, `"reference":{"kind":"detached"`, 1)
		writeSession(t, home, "s", repo, body)
		evs, _ := newCollector(t, home, []string{repo}, nil).Collect(ctx, time.Time{})
		if len(evs) != len(fixtureWant) {
			t.Fatalf("got %d events, want %d", len(evs), len(fixtureWant))
		}
		for _, e := range evs {
			if e.IssueID != collector.UnattributedDetachedHEAD {
				t.Errorf("IssueID = %q, want %q", e.IssueID, collector.UnattributedDetachedHEAD)
			}
		}
	})
}

// TestRun_PendingFileIsReReadPastTheCursor: a held file whose mtime falls
// behind the scan cursor must still be re-read until it settles, or an
// abandoned run's spend is stranded forever.
func TestRun_PendingFileIsReReadPastTheCursor(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	path := writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
	start := time.Now()
	var mu sync.Mutex
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	c := newCollector(t, home, []string{repo}, now)
	ing := &memIngester{}

	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != 0 {
		t.Fatalf("pass 1 emitted %d events for an unsettled run", len(ing.byKey))
	}
	// The file goes quiet: its mtime is now far behind the cursor.
	old := start.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clock = old.Add(settleGrace + time.Minute)
	mu.Unlock()
	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != len(fixtureWant) {
		t.Fatalf("pass 2 stored %d events, want %d: the pending file was not re-read past the cursor", len(ing.byKey), len(fixtureWant))
	}
	if len(c.pending) != 0 {
		t.Errorf("pending = %v after the run settled, want empty", c.pending)
	}
}

// TestOutOfScopeSessionIsSkipped: a session whose workspace_root is outside
// every configured repo yields nothing.
func TestOutOfScopeSessionIsSkipped(t *testing.T) {
	repo := initGitRepo(t)
	other := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, other, readFixture(t))
	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil || len(evs) != 0 {
		t.Fatalf("a foreign-repo session emitted %d events (err %v)", len(evs), err)
	}
	// Control: the same session IS captured when its repo is in scope.
	evs, _ = newCollector(t, home, []string{repo, other}, nil).Collect(context.Background(), time.Time{})
	if len(evs) != len(fixtureWant) {
		t.Fatalf("control: in-scope session emitted %d events, want %d", len(evs), len(fixtureWant))
	}
}

// TestMalformedAndUnknownLinesAreSkipped: garbage lines, unknown record kinds,
// a billed record with an undecodable usage block, and an unterminated final
// line never fail the file; every good record is still emitted.
func TestMalformedAndUnknownLinesAreSkipped(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	body := readFixture(t) +
		"this is not json\n" +
		`{"id":"rec-x1","recorded_at":1790000100000000,"payload":{"kind":"future_kind","event":{"kind":"whatever"}}}` + "\n" +
		`{"id":"rec-x2","recorded_at":1790000101000000,"payload":{"kind":"run","run_id":"run-fixture-a","event":{"kind":"model_completed","model":"m","usage":{"input_tokens":"lots"}}}}` + "\n" +
		`{"id":"rec-x3","recorded_at":17900`
	p := writeSession(t, home, fixtureSession, repo, body)

	sess, err := parseSession(p)
	if err != nil {
		t.Fatalf("parseSession failed the file: %v", err)
	}
	if sess.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1 (the garbage line; the unterminated tail is a writer mid-flush)", sess.SkippedLines)
	}
	if sess.Refused[refuseUndecodable] != 1 {
		t.Errorf("refused = %v, want one %s", sess.Refused, refuseUndecodable)
	}
	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil || len(evs) != len(fixtureWant) {
		t.Fatalf("got %d events (err %v), want %d", len(evs), err, len(fixtureWant))
	}
}

// ─── Subagent sessions (#901) ────────────────────────────────────────────────
//
// testdata/subagent_session.jsonl has the structure of a real Muse Code 1.4.0
// <parent>/subagent/<id>/session.jsonl — a leading retained_frame, a metadata
// record with NO workspace_root, no workspace_branch, no parent pointer, its run
// id equal to its own session id, terminal last — with synthetic ids and
// numbers. The parent fixture (testdata/session.jsonl) carries the real link
// record shape: run/memory_reminder_child_session_linked naming run-fixture-a,
// before that run's terminal, whose workspace_branch lands after it.

const (
	childSession = "ses-fixture-child"
	childRecord  = "sub-rec-0004"
)

// childWant is the child's one call: input 8000 INCLUDES cache_read 2800.
var childWant = tokens{Input: 5200, CacheRead: 2800, Output: 2000}

var childKey = collector.IdempotencyKey(collector.SourceMuse, providerMeta, childRecord)

func readChildFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "subagent_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// writeChild writes body as the subagent session childID of the session
// directory parentDir, and returns its path.
func writeChild(t *testing.T, home, parentDir, childID, body string) string {
	t.Helper()
	return writeSession(t, home, filepath.Join(parentDir, subagentDirName, childID), "", body)
}

// logRecords decodes every JSON log line.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// linesWithMsg returns the log records whose msg is msg.
func linesWithMsg(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// newLoggedCollector is newCollector with a JSON logger into the returned buffer.
func newLoggedCollector(t *testing.T, home, repo string, now func() time.Time) (*Collector, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c, err := New(Config{Home: home, Repos: []RepoTarget{{Path: repo}}, DeveloperID: "dev-fixture",
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return c, &logs
}

// TestSubagent_AttributedToParentRun: the child's call is emitted with the
// parent run's branch, the parent's repo, its own token mapping and its own
// record key — by the stateful Run pass and the stateless Collect (ship) alike.
func TestSubagent_AttributedToParentRun(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	child := readChildFixture(t)
	if strings.Contains(child, "workspace_root") || strings.Contains(child, "workspace_branch") ||
		!strings.Contains(child, `"run_id":"`+childSession+`"`) {
		t.Fatal("control: the child fixture must have the real shape: no root, no branch, run id == session id")
	}
	writeSession(t, home, fixtureSession, repo, readFixture(t))
	writeChild(t, home, fixtureSession, childSession, child)

	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(evs) != len(fixtureWant)+1 {
		t.Fatalf("Collect: %d events, want the parent's %d plus the child's 1", len(evs), len(fixtureWant))
	}
	c, logs := newLoggedCollector(t, home, repo, nil)
	ing := &memIngester{}
	c.runPass(context.Background(), time.Time{}, ing)

	parentEv := byKey(evs)[collector.IdempotencyKey(collector.SourceMuse, providerMeta, "rec-fixture-0005")]
	for name, e := range map[string]collector.TokenEvent{"Collect": byKey(evs)[childKey], "Run": ing.byKey[childKey]} {
		if e.IdempotencyKey != childKey {
			t.Errorf("%s: no event keyed on the child's record %s", name, childRecord)
			continue
		}
		if e.IssueID != wantIssueID {
			t.Errorf("%s: child IssueID = %q, want %q from its parent run's workspace_branch", name, e.IssueID, wantIssueID)
		}
		if (tokens{Input: e.InputTok, CacheRead: e.CacheRead, Output: e.OutputTok}) != childWant {
			t.Errorf("%s: child tokens in=%d read=%d out=%d, want %+v", name, e.InputTok, e.CacheRead, e.OutputTok, childWant)
		}
		if e.Repo == "" || e.Repo != parentEv.Repo {
			t.Errorf("%s: child repo %q, want the parent's %q", name, e.Repo, parentEv.Repo)
		}
		if e.SessionID != childSession || e.Source != collector.SourceMuse || e.Model != fixtureModel {
			t.Errorf("%s: child session=%q source=%q model=%q", name, e.SessionID, e.Source, e.Model)
		}
	}
	recs := logRecords(t, logs)
	done := linesWithMsg(recs, "muse scan complete")
	if len(done) != 1 || done[0]["no_workspace_root"] != float64(0) || done[0]["unlinked_subagents"] != float64(0) || done[0]["subagent_events"] != float64(1) {
		t.Errorf("scan line = %v, want no_workspace_root 0, unlinked_subagents 0, subagent_events 1", done)
	}
	if n := len(linesWithMsg(recs, msgNoRootExcluded)) + len(linesWithMsg(recs, msgUnlinkedSubagent)); n != 0 {
		t.Errorf("an attributed child raised %d exclusion WARNs", n)
	}
}

// TestSubagent_HeldUntilParentRunSettles: the parent run's branch lands after
// its terminal, which lands after the child's calls; a stored row's issue_id is
// never rewritten, so the child must not be emitted until the parent run settles.
func TestSubagent_HeldUntilParentRunSettles(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	parentPath := writeSession(t, home, fixtureSession, repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
	childPath := writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
	c := newCollector(t, home, []string{repo}, nil)
	ing := &memIngester{}

	running := fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`)
	quiet := settleGrace + time.Hour
	// The run is unsettled in every stage; the last two age ONE file past
	// settleGrace. The hold ends only when BOTH logs have been silent that long.
	for _, stage := range []struct {
		name, body          string
		parentAge, childAge time.Duration
	}{
		{"parent_run_running", running, 0, 0},
		{"parent_terminal_is_last_record", fixtureLinesWithout(t, `"kind":"workspace_branch"`), 0, 0},
		{"child_quiet_parent_still_writing", running, 0, quiet},
		{"parent_quiet_child_still_writing", running, quiet, 0},
	} {
		if err := os.WriteFile(parentPath, []byte(strings.ReplaceAll(stage.body, fixtureRoot, repo)), 0o644); err != nil {
			t.Fatal(err)
		}
		for p, age := range map[string]time.Duration{parentPath: stage.parentAge, childPath: stage.childAge} {
			if err := os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
		c.runPass(context.Background(), time.Time{}, ing)
		if e, ok := ing.byKey[childKey]; ok {
			t.Fatalf("%s: the child was emitted (IssueID %q) before its parent run settled", stage.name, e.IssueID)
		}
		if _, ok := c.pending[childPath]; !ok {
			t.Fatalf("%s: the held child is not pending: %v", stage.name, c.pending)
		}
		evs, _ := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
		if _, ok := byKey(evs)[childKey]; ok {
			t.Fatalf("%s: Collect returned the child before its parent run settled", stage.name)
		}
	}

	// The branch record lands: the parent run settles and the child is emitted.
	if err := os.WriteFile(parentPath, []byte(strings.ReplaceAll(readFixture(t), fixtureRoot, repo)), 0o644); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), time.Time{}, ing)
	if e, ok := ing.byKey[childKey]; !ok || e.IssueID != wantIssueID {
		t.Fatalf("after the parent's branch record: child event %+v (present %v), want IssueID %q", e, ok, wantIssueID)
	}
	if _, ok := c.pending[childPath]; ok {
		t.Errorf("the child is still pending after it was emitted")
	}
}

// TestSubagent_NoLinkRecord: a child no parent link record names is held while
// the parent log may still be written, then EXCLUDED — never given the parent
// session's branch — and every pass WARNs.
func TestSubagent_NoLinkRecord(t *testing.T) {
	repo := initGitRepo(t)
	unlinkedParent := fixtureLinesWithout(t, eventChildLinked)
	if strings.Contains(unlinkedParent, childSession) {
		t.Fatal("control: the unlinked parent still names the child")
	}
	var link string
	for _, l := range strings.Split(readFixture(t), "\n") {
		if strings.Contains(l, eventChildLinked) {
			link = l
		}
	}
	if link == "" {
		t.Fatal("control: the parent fixture carries no link record")
	}
	// A second record naming the same child from a different run: ambiguous.
	ambiguous := strings.Replace(readFixture(t), link, link+"\n"+strings.NewReplacer(
		`"id":"rec-fixture-0020"`, `"id":"rec-fixture-0021"`,
		`"parent_run_id":"run-fixture-a"`, `"parent_run_id":"run-fixture-other"`).Replace(link), 1)
	later := func() time.Time { return time.Now().Add(settleGrace + time.Minute) }

	quiet := settleGrace + time.Hour
	for _, tc := range []struct {
		name                string
		parent              *string // nil: no parent session log at all
		now                 func() time.Time
		parentAge, childAge time.Duration // file mtimes, relative to the real clock
		wantHeld            int
		wantExcl            int
		wantEvent           int // parent events expected
		wantReason          string
	}{
		{"held_before_grace", &unlinkedParent, nil, 0, 0, 1, 0, len(fixtureWant), "no_link=1"},
		{"child_quiet_parent_still_writing_held", &unlinkedParent, nil, 0, quiet, 1, 0, len(fixtureWant), "no_link=1"},
		{"parent_quiet_child_still_writing_held", &unlinkedParent, nil, quiet, 0, 1, 0, len(fixtureWant), "no_link=1"},
		{"excluded_after_grace", &unlinkedParent, later, 0, 0, 0, 1, len(fixtureWant), "no_link=1"},
		{"ambiguous_link_excluded_after_grace", &ambiguous, later, 0, 0, 0, 1, len(fixtureWant), "ambiguous_link=1"},
		{"no_parent_log_held", nil, nil, 0, 0, 1, 0, 0, "no_parent_log=1"},
		{"no_parent_log_excluded_after_grace", nil, later, 0, 0, 0, 1, 0, "no_parent_log=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.parent != nil {
				p := writeSession(t, home, fixtureSession, repo, *tc.parent)
				if err := os.Chtimes(p, time.Now().Add(-tc.parentAge), time.Now().Add(-tc.parentAge)); err != nil {
					t.Fatal(err)
				}
			}
			childPath := writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
			if err := os.Chtimes(childPath, time.Now().Add(-tc.childAge), time.Now().Add(-tc.childAge)); err != nil {
				t.Fatal(err)
			}
			c, logs := newLoggedCollector(t, home, repo, tc.now)
			var lost [][2]time.Time
			c.lost = func(_ context.Context, from, through time.Time) error {
				lost = append(lost, [2]time.Time{from, through})
				return nil
			}
			ing := &memIngester{}
			c.runPass(context.Background(), time.Time{}, ing)
			// #913-D9 ruling R-8: an excluded child's spend is recorded lost.
			if len(lost) != tc.wantExcl {
				t.Errorf("lost %v, want %d span(s)", lost, tc.wantExcl)
			}
			evs, err := newCollector(t, home, []string{repo}, tc.now).Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			for name, got := range map[string]map[string]collector.TokenEvent{"Run": ing.byKey, "Collect": byKey(evs)} {
				if e, ok := got[childKey]; ok {
					t.Errorf("%s: the unlinked child was emitted with IssueID %q; it must never be attributed", name, e.IssueID)
				}
				if len(got) != tc.wantEvent {
					t.Errorf("%s: %d events, want the parent's %d", name, len(got), tc.wantEvent)
				}
			}
			_, pending := c.pending[childPath]
			if pending != (tc.wantHeld > 0) {
				t.Errorf("child pending = %v, want %v", pending, tc.wantHeld > 0)
			}
			recs := logRecords(t, logs)
			warn := linesWithMsg(recs, msgUnlinkedSubagent)
			if len(warn) != 1 || warn[0]["level"] != "WARN" || warn[0]["held_calls"] != float64(tc.wantHeld) ||
				warn[0]["excluded_calls"] != float64(tc.wantExcl) || warn[0]["sessions"] != float64(1) || warn[0]["reason"] != tc.wantReason {
				t.Errorf("unlinked WARN = %v, want one WARN with held_calls %d excluded_calls %d sessions 1 reason %q", warn, tc.wantHeld, tc.wantExcl, tc.wantReason)
			}
			if done := linesWithMsg(recs, "muse scan complete"); len(done) != 1 || done[0]["unlinked_subagents"] != float64(1) {
				t.Errorf("scan line = %v, want unlinked_subagents 1", done)
			}
			if n := len(linesWithMsg(recs, msgNoRootExcluded)); n != 0 {
				t.Errorf("an unlinked child raised %d no_workspace_root WARNs; that line is for root-less sessions", n)
			}
		})
	}
}

// TestSubagent_RootlessParentExcludedAsNoRoot: a child of a root-less parent is
// excluded with its parent as no_workspace_root — linked or not, it is never
// held or reported as unlinked, and nothing from either file is emitted.
func TestSubagent_RootlessParentExcludedAsNoRoot(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, "", fixtureLinesWithout(t, eventChildLinked))
	childPath := writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
	c, logs := newLoggedCollector(t, home, repo, nil)
	ing := &memIngester{}
	c.runPass(context.Background(), time.Time{}, ing)
	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(ing.byKey) != 0 || len(evs) != 0 {
		t.Fatalf("Run stored %d, Collect returned %d events from root-less sessions", len(ing.byKey), len(evs))
	}
	if _, ok := c.pending[childPath]; ok {
		t.Error("the child of a root-less parent is held pending; it has no root to be scoped by")
	}
	recs := logRecords(t, logs)
	calls := len(fixtureWant) + 1 // the parent's billed calls plus the child's one
	if w := linesWithMsg(recs, msgNoRootExcluded); len(w) != 1 || w[0]["level"] != "WARN" ||
		w[0]["excluded_calls"] != float64(calls) || w[0]["sessions"] != float64(2) {
		t.Errorf("no_workspace_root WARN = %v, want one WARN with excluded_calls %d sessions 2", w, calls)
	}
	if n := len(linesWithMsg(recs, msgUnlinkedSubagent)); n != 0 {
		t.Errorf("the child of a root-less parent raised %d unlinked WARNs", n)
	}
	if done := linesWithMsg(recs, "muse scan complete"); len(done) != 1 || done[0]["no_workspace_root"] != float64(2) || done[0]["unlinked_subagents"] != float64(0) {
		t.Errorf("scan line = %v, want no_workspace_root 2, unlinked_subagents 0", done)
	}
}

// TestSubagent_LinkRecordVariants: the same link named twice by the SAME run is
// still one link (only different runs are ambiguous), and a link record with no
// parent_run_id falls back to the record's own run_id.
func TestSubagent_LinkRecordVariants(t *testing.T) {
	repo := initGitRepo(t)
	fixture := readFixture(t)
	var link string
	for _, l := range strings.Split(fixture, "\n") {
		if strings.Contains(l, eventChildLinked) {
			link = l
		}
	}
	const parentRun = `"parent_run_id":"run-fixture-a",`
	if link == "" || strings.Count(fixture, parentRun) != 1 {
		t.Fatal("control: the fixture must carry one link record naming parent_run_id")
	}
	for name, parent := range map[string]string{
		"duplicate_same_run": strings.Replace(fixture, link, link+"\n"+strings.Replace(link, `"id":"rec-fixture-0020"`, `"id":"rec-fixture-0021"`, 1), 1),
		"no_parent_run_id":   strings.Replace(fixture, parentRun, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeSession(t, home, fixtureSession, repo, parent)
			writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
			evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if e, ok := byKey(evs)[childKey]; !ok || e.IssueID != wantIssueID {
				t.Errorf("child event %+v (present %v), want IssueID %q from run-fixture-a", e, ok, wantIssueID)
			}
		})
	}
}

// TestSubagent_NestedSubagentIsNeverAttributed: a subagent's own subagent is
// excluded as no_workspace_root — never scoped by the intermediate subagent's
// own root record, even one that links it — and a scanned top-level parent
// always brings in its whole subagent tree, whatever the map order.
func TestSubagent_NestedSubagentIsNeverAttributed(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	const grand = "ses-fixture-grand"
	parentPath := writeSession(t, home, fixtureSession, repo, readFixture(t))
	child := strings.Replace(readChildFixture(t), `"record":{"provider_id"`, `"record":{"workspace_root":"`+fixtureRoot+`","provider_id"`, 1)
	child = strings.Replace(child, `{"schema_version":1,"id":"sub-rec-0006"`, fmt.Sprintf(
		`{"schema_version":1,"id":"sub-rec-0020","stream":{"kind":"session","id":%[1]q},"sequence":20,"recorded_at":1790000024500000,"record_type":"event","durability":"durable","causation_id":null,"payload_type":"runtime.session","payload_schema_version":1,"payload":{"kind":"run","run_id":%[1]q,"event":{"kind":"memory_reminder_child_session_linked","parent_session_id":%[1]q,"parent_run_id":%[1]q,"task_id":"task-grand","generation_id":1,"child_session_id":%[2]q}}}`+"\n"+
			`{"schema_version":1,"id":"sub-rec-0006"`, childSession, grand), 1)
	if !strings.Contains(child, `"workspace_root":"`) || !strings.Contains(child, `"child_session_id":"`+grand) {
		t.Fatal("control: the intermediate subagent must carry its own root and link the grandchild")
	}
	childDir := filepath.Join(fixtureSession, subagentDirName, childSession)
	childPath := writeSession(t, home, childDir, repo, child)
	grandPath := writeChild(t, home, childDir, grand, strings.NewReplacer(childSession, grand, "sub-rec-", "grand-rec-").Replace(readChildFixture(t)))
	grandKey := collector.IdempotencyKey(collector.SourceMuse, providerMeta, "grand-rec-0004")

	later := func() time.Time { return time.Now().Add(settleGrace + time.Minute) }
	c, logs := newLoggedCollector(t, home, repo, later)
	ing := &memIngester{}
	c.runPass(context.Background(), time.Time{}, ing)
	if e, ok := ing.byKey[grandKey]; ok {
		t.Fatalf("the nested subagent was emitted (IssueID %q)", e.IssueID)
	}
	if e, ok := ing.byKey[childKey]; !ok || e.IssueID != wantIssueID {
		t.Errorf("control: the intermediate subagent event %+v (present %v), want IssueID %q", e, ok, wantIssueID)
	}
	if w := linesWithMsg(logRecords(t, logs), msgNoRootExcluded); len(w) != 1 || w[0]["excluded_calls"] != float64(1) || w[0]["sessions"] != float64(1) {
		t.Errorf("no_workspace_root WARN = %v, want excluded_calls 1 sessions 1 (the nested subagent)", w)
	}

	// Only the parent is due; both subagent files are behind the floor. Map
	// iteration order varies per run, so repeat: an order-dependent pull-in
	// misses the grandchild on about half of them.
	for i := 0; i < 32; i++ {
		paths, err := c.findSessionFiles(time.Now().Add(time.Hour), map[string]time.Time{parentPath: {}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(paths, "\n") != strings.Join([]string{parentPath, childPath, grandPath}, "\n") {
			t.Fatalf("attempt %d: paths = %v, want the parent and its whole subagent tree", i, paths)
		}
	}
}

// TestSubagent_ParentUnreadable: a parent log that is not a regular file is
// refused as the walk refuses one, leaving the child unlinked (held, never
// attributed through a symlink); a parent log that fails to READ fails the child
// too — it names the parent, keeps a held child pending, and emits nothing.
func TestSubagent_ParentUnreadable(t *testing.T) {
	repo := initGitRepo(t)
	t.Run("symlinked_parent_is_unlinked", func(t *testing.T) {
		home := t.TempDir()
		real := writeSession(t, t.TempDir(), "elsewhere", repo, readFixture(t))
		childPath := writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
		parentPath := filepath.Join(home, "sessions", "2026", "09", "21", fixtureSession, sessionFileName)
		if err := os.Symlink(real, parentPath); err != nil {
			t.Skipf("symlink: %v", err)
		}
		c, logs := newLoggedCollector(t, home, repo, nil)
		ing := &memIngester{}
		c.runPass(context.Background(), time.Time{}, ing)
		if len(ing.byKey) != 0 {
			t.Fatalf("stored %d events through a symlinked parent log", len(ing.byKey))
		}
		if _, ok := c.pending[childPath]; !ok {
			t.Errorf("the child of a refused parent is not held pending: %v", c.pending)
		}
		if w := linesWithMsg(logRecords(t, logs), msgUnlinkedSubagent); len(w) != 1 || w[0]["held_calls"] != float64(1) {
			t.Errorf("unlinked WARN = %v, want held_calls 1", w)
		}
	})
	t.Run("parent_read_failure_fails_the_child", func(t *testing.T) {
		home := t.TempDir()
		writeSession(t, home, fixtureSession, repo, readFixture(t))
		childPath := writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
		c := newCollector(t, home, []string{repo}, nil)
		c.pending = map[string]time.Time{childPath: {}}
		orig := maxSessionFile
		maxSessionFile = int64(len(readChildFixture(t))) + 10 // the child fits; the parent does not
		t.Cleanup(func() { maxSessionFile = orig })
		if int64(len(readFixture(t))) <= maxSessionFile {
			t.Fatal("control: the parent fixture must exceed the cap the child fits under")
		}
		_, err := c.Collect(context.Background(), time.Time{})
		if err == nil || !strings.Contains(err.Error(), "parent session") {
			t.Fatalf("Collect err = %v, want the child failed naming its parent session", err)
		}
		ing := &memIngester{}
		c.runPass(context.Background(), time.Time{}, ing)
		if _, ok := ing.byKey[childKey]; ok {
			t.Error("the child was emitted though its parent log could not be read")
		}
		if _, ok := c.pending[childPath]; !ok {
			t.Errorf("a parent read failure dropped the held child from pending: %v", c.pending)
		}
	})
}

// TestSubagent_OldChildReadWhenParentScanned: a child file whose mtime is behind
// the scan cursor (a pass before #901 excluded it) is read once its parent is
// scanned again, whatever the child's own mtime.
func TestSubagent_OldChildReadWhenParentScanned(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	parentPath := writeSession(t, home, fixtureSession, repo, readFixture(t))
	childPath := writeChild(t, home, fixtureSession, childSession, readChildFixture(t))
	start := time.Now()
	old := start.Add(-time.Hour)
	for _, p := range []string{parentPath, childPath} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	c := newCollector(t, home, []string{repo}, func() time.Time { return start })
	c.fileFloor = start.Add(-time.Minute) // an earlier pass already went past both files
	ing := &memIngester{}

	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != 0 {
		t.Fatalf("control: %d events from files behind the cursor; the floor must skip them", len(ing.byKey))
	}
	// The parent is written again (a later run); the child is not touched.
	if err := os.Chtimes(parentPath, start, start); err != nil {
		t.Fatal(err)
	}
	c.runPass(context.Background(), time.Time{}, ing)
	if e, ok := ing.byKey[childKey]; !ok || e.IssueID != wantIssueID {
		t.Fatalf("child event %+v (present %v): a child behind the cursor was not read with its re-scanned parent", e, ok)
	}
}

// TestSubagent_EachChildTakesItsOwnParentRunsBranch: two runs of one parent
// session on different branches each spawn a child. Each child takes ITS
// parent run's branch — never the session's latest one.
func TestSubagent_EachChildTakesItsOwnParentRunsBranch(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	rec := func(id, seq int, run, payload string) string {
		return fmt.Sprintf(`{"schema_version":1,"id":"rec-two-%04d","stream":{"kind":"session","id":"ses-two"},"sequence":%d,"recorded_at":%d,"record_type":"event","durability":"durable","causation_id":null,"payload_type":"runtime.session","payload_schema_version":1,"payload":{"kind":"run","run_id":%q,%s}}`+"\n",
			id, seq, 1790000100000000+int64(seq)*1000000, run, payload)
	}
	branchRec := func(seq int, run, branch string) string {
		return fmt.Sprintf(`{"schema_version":1,"id":"rec-two-%04d","stream":{"kind":"session","id":"ses-two"},"sequence":%d,"recorded_at":%d,"record_type":"event","durability":"durable","causation_id":null,"payload_type":"session.workspace_branch.observed","payload_schema_version":1,"payload":{"kind":"workspace_branch","record":{"command_id":%q,"workspace_root":%q,"reference":{"kind":"branch","name":%q},"vcs":"git","commit":"1111111111111111111111111111111111111111"}}}`+"\n",
			seq, seq, 1790000100000000+int64(seq)*1000000, run, repo, branch)
	}
	link := func(run, child string) string {
		return fmt.Sprintf(`"event":{"kind":"memory_reminder_child_session_linked","parent_session_id":"ses-two","parent_run_id":%q,"task_id":"task-%s","reminder_agent_id":"fixture","generation_id":1,"child_session_id":%q}`, run, child, child)
	}
	const call = `"event":{"kind":"model_completed","model":"muse-spark-1.3-contributor","usage":{"input_tokens":100,"output_tokens":10,"cached_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"reasoning_tokens":0}}`
	parent := fmt.Sprintf(`{"schema_version":1,"id":"rec-two-0001","stream":{"kind":"session","id":"ses-two"},"sequence":1,"recorded_at":1790000100000000,"record_type":"event","durability":"durable","causation_id":null,"payload_type":"runtime.session.metadata","payload_schema_version":1,"payload":{"kind":"metadata","record":{"workspace_root":%q}}}`+"\n", repo) +
		rec(2, 2, "run-first", `"event":{"kind":"started"}`) +
		rec(3, 3, "run-first", call) +
		rec(4, 4, "run-first", link("run-first", "child-first")) +
		rec(5, 5, "run-first", `"event":{"kind":"terminal"}`) +
		branchRec(6, "run-first", "feature/901-first") +
		rec(7, 7, "run-second", `"event":{"kind":"started"}`) +
		rec(8, 8, "run-second", call) +
		rec(9, 9, "run-second", link("run-second", "child-second")) +
		rec(10, 10, "run-second", `"event":{"kind":"terminal"}`) +
		branchRec(11, "run-second", "feature/902-second")
	writeSession(t, home, "ses-two", repo, parent)
	for _, child := range []string{"child-first", "child-second"} {
		body := strings.NewReplacer(childSession, child, "sub-rec-", child+"-rec-").Replace(readChildFixture(t))
		writeChild(t, home, "ses-two", child, body)
	}

	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := byKey(evs)
	for child, want := range map[string]string{"child-first": "issue-901", "child-second": "issue-902"} {
		e, ok := got[collector.IdempotencyKey(collector.SourceMuse, providerMeta, child+"-rec-0004")]
		if !ok {
			t.Errorf("%s: no event", child)
			continue
		}
		if e.IssueID != want {
			t.Errorf("%s: IssueID = %q, want %q from the run that spawned it", child, e.IssueID, want)
		}
	}
	if len(evs) != 4 {
		t.Errorf("got %d events, want 4 (two parent calls, two child calls)", len(evs))
	}
}

// TestRefusals: records the collector must not price.
func TestRefusals(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	body := readFixture(t) +
		// no model
		`{"id":"rec-r1","recorded_at":1790000200000000,"payload":{"kind":"run","run_id":"run-fixture-a","event":{"kind":"model_completed","usage":{"input_tokens":10,"output_tokens":1}}}}` + "\n" +
		// no id
		`{"recorded_at":1790000201000000,"payload":{"kind":"run","run_id":"run-fixture-a","event":{"kind":"model_completed","model":"m","usage":{"input_tokens":10,"output_tokens":1}}}}` + "\n" +
		// no timestamp at all
		`{"id":"rec-r3","payload":{"kind":"run","run_id":"run-fixture-a","event":{"kind":"model_completed","model":"m","usage":{"input_tokens":10,"output_tokens":1}}}}` + "\n" +
		// far-future timestamp (year ~2286)
		`{"id":"rec-r4","recorded_at":9999999999000000,"payload":{"kind":"run","run_id":"run-fixture-a","event":{"kind":"model_completed","model":"m","usage":{"input_tokens":10,"output_tokens":1}}}}` + "\n"
	p := writeSession(t, home, fixtureSession, repo, body)
	sess, err := parseSession(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{refuseNoModel, refuseNoRecordID, refuseNoTimestamp} {
		if sess.Refused[r] != 1 {
			t.Errorf("refused[%s] = %d, want 1 (all: %v)", r, sess.Refused[r], sess.Refused)
		}
	}
	evs, _ := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if len(evs) != len(fixtureWant) {
		t.Errorf("got %d events, want %d: the future-stamped record must be refused", len(evs), len(fixtureWant))
	}
}

// TestMissingSessionsRootIsAnEmptyScan: Muse not installed is not an error.
func TestMissingSessionsRootIsAnEmptyScan(t *testing.T) {
	repo := initGitRepo(t)
	evs, err := newCollector(t, filepath.Join(t.TempDir(), "absent"), []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil || len(evs) != 0 {
		t.Fatalf("got %d events, err %v; want an empty, error-free scan", len(evs), err)
	}
}

// TestSymlinkNamedSessionIsRefused: a symlink is never followed out of the tree.
func TestSymlinkNamedSessionIsRefused(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	real := writeSession(t, t.TempDir(), "elsewhere", repo, readFixture(t))
	d := filepath.Join(home, "sessions", "2026", "09", "21", "linked")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(d, sessionFileName)); err != nil {
		t.Skipf("symlink: %v", err)
	}
	evs, _ := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if len(evs) != 0 {
		t.Fatalf("followed a symlinked session file: %d events", len(evs))
	}
}

// TestFileOverCapFails: a truncated prefix is never priced.
func TestFileOverCapFails(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	p := writeSession(t, home, fixtureSession, repo, readFixture(t))
	orig := maxSessionFile
	maxSessionFile = 100
	t.Cleanup(func() { maxSessionFile = orig })
	if _, err := parseSession(p); err == nil {
		t.Fatal("a file over the read cap parsed; its spend would be under-reported silently")
	}
}

// TestNewRejectsNoRepos: a collector that could never attribute anything fails fast.
func TestNewRejectsNoRepos(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no repos must fail")
	}
	if _, err := New(Config{Repos: []RepoTarget{{Path: "  "}}}); err == nil {
		t.Error("New with a blank repo path must fail")
	}
}

// TestRun_CursorHoldsWhenNothingWasScanned: a pass that could look at no file
// (here, no attributable repo) must not advance the cursor past files it never
// read, or sessions written during the outage are never scanned.
func TestRun_CursorHoldsWhenNothingWasScanned(t *testing.T) {
	notARepo := t.TempDir()
	c := newCollector(t, t.TempDir(), []string{notARepo}, nil)
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	if !c.fileFloor.IsZero() {
		t.Fatalf("cursor advanced to %v after a pass that scanned nothing", c.fileFloor)
	}
	// Control: a pass that did scan advances it.
	ok := newCollector(t, t.TempDir(), []string{initGitRepo(t)}, nil)
	ok.runPass(context.Background(), time.Time{}, &memIngester{})
	if ok.fileFloor.IsZero() {
		t.Fatal("control: a completed pass did not advance the cursor")
	}
}

// TestRun_PendingSurvivesAReadFailure: a held file that fails to read on one
// pass stays pending, so its calls are not stranded behind the cursor.
func TestRun_PendingSurvivesAReadFailure(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	path := writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
	c := newCollector(t, home, []string{repo}, nil)
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	if _, ok := c.pending[path]; !ok {
		t.Fatalf("control: the held file is not pending after pass 1: %v", c.pending)
	}
	orig := maxSessionFile
	maxSessionFile = 100 // the next read fails the file
	t.Cleanup(func() { maxSessionFile = orig })
	c.runPass(context.Background(), time.Time{}, &memIngester{})
	if _, ok := c.pending[path]; !ok {
		t.Fatalf("a read failure dropped the held file from pending: %v", c.pending)
	}
}

// TestCollect_SinceExcludesOlderCalls: a call stamped before `since` is not
// returned — the `ship --since` contract.
func TestCollect_SinceExcludesOlderCalls(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, repo, readFixture(t))
	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.UnixMicro(1790000008000000))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := byKey(evs)
	for _, id := range []string{"rec-fixture-0009", "rec-fixture-0011"} {
		if _, ok := got[collector.IdempotencyKey(collector.SourceMuse, providerMeta, id)]; !ok {
			t.Errorf("%s (after since) missing", id)
		}
	}
	if len(evs) != 2 {
		t.Errorf("got %d events, want 2: the two calls stamped before since must be excluded", len(evs))
	}
}

// failIngester refuses every event, as a store that is down would.
type failIngester struct{}

func (failIngester) Ingest(context.Context, collector.TokenEvent) error {
	return fmt.Errorf("store unavailable")
}

// cancelOnIngest cancels the pass's context while accepting its n-th event.
type cancelOnIngest struct {
	memIngester
	n      int
	cancel context.CancelFunc
}

func (c *cancelOnIngest) Ingest(ctx context.Context, e collector.TokenEvent) error {
	err := c.memIngester.Ingest(ctx, e)
	if c.ingest == c.n {
		c.cancel()
	}
	return err
}

// TestRun_CursorHoldsOnAbortedIngest: an ingest failure, or a cancellation
// arriving during the last ingest, leaves the cursor where it was, so the next
// pass re-reads the tail.
func TestRun_CursorHoldsOnAbortedIngest(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, repo, readFixture(t))

	c := newCollector(t, home, []string{repo}, nil)
	c.runPass(context.Background(), time.Time{}, failIngester{})
	if !c.fileFloor.IsZero() {
		t.Errorf("cursor advanced to %v after every ingest failed", c.fileFloor)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ing := &cancelOnIngest{n: len(fixtureWant), cancel: cancel}
	c = newCollector(t, home, []string{repo}, nil)
	c.runPass(ctx, time.Time{}, ing)
	if ing.ingest != len(fixtureWant) {
		t.Fatalf("control: %d events ingested, want %d before the cancel", ing.ingest, len(fixtureWant))
	}
	if !c.fileFloor.IsZero() {
		t.Errorf("cursor advanced to %v after the pass was cancelled", c.fileFloor)
	}
}

// TestWorkspaceRootFromBranchRecord: a session whose metadata record is lost is
// still scoped by its workspace_branch record's root.
func TestWorkspaceRootFromBranchRecord(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeSession(t, home, fixtureSession, repo, fixtureLinesWithout(t, `"kind":"metadata"`))
	evs, err := newCollector(t, home, []string{repo}, nil).Collect(context.Background(), time.Time{})
	if err != nil || len(evs) != len(fixtureWant) {
		t.Fatalf("got %d events (err %v), want %d scoped by the workspace_branch root", len(evs), err, len(fixtureWant))
	}
	for _, e := range evs {
		if e.IssueID != wantIssueID {
			t.Errorf("IssueID = %q, want %q", e.IssueID, wantIssueID)
		}
	}
}

// TestReviewTimestampFallsBackToCompletedAtMS: a review record with no
// recorded_at is stamped from completed_at_ms, not refused.
func TestReviewTimestampFallsBackToCompletedAtMS(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	const stamp = `"recorded_at":1790000009000000,`
	if !strings.Contains(readFixture(t), stamp) {
		t.Fatal("control: the review record's recorded_at is not in the fixture")
	}
	p := writeSession(t, home, fixtureSession, repo, strings.Replace(readFixture(t), stamp, "", 1))
	sess, err := parseSession(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, cl := range sess.Calls {
		if cl.RecordID == "rec-fixture-0009" {
			if want := time.UnixMilli(1790000009000).UTC(); !cl.Timestamp.Equal(want) {
				t.Errorf("timestamp = %v, want %v from completed_at_ms", cl.Timestamp, want)
			}
			return
		}
	}
	t.Fatalf("the review record was not kept (refused: %v)", sess.Refused)
}

// TestRun_UnlistableDirectoryKeepsHeldFiles: a held file under a directory the
// pass cannot list stays pending, and an unlistable ROOT also holds the cursor,
// or the held run's spend is stranded.
func TestRun_UnlistableDirectoryKeepsHeldFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, tc := range []struct {
		name       string
		dir        func(home, path string) string
		holdCursor bool
	}{
		{"root", func(home, _ string) string { return filepath.Join(home, "sessions") }, true},
		{"session_dir", func(_, path string) string { return filepath.Dir(path) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initGitRepo(t)
			home := t.TempDir()
			path := writeSession(t, home, "s", repo, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
			c := newCollector(t, home, []string{repo}, nil)
			c.runPass(context.Background(), time.Time{}, &memIngester{})
			floor := c.fileFloor
			if _, ok := c.pending[path]; !ok || floor.IsZero() {
				t.Fatalf("control: pass 1 must hold the file and advance the cursor; pending=%v floor=%v", c.pending, floor)
			}
			dir := tc.dir(home, path)
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			c.runPass(context.Background(), time.Time{}, &memIngester{})
			if _, ok := c.pending[path]; !ok {
				t.Errorf("the held file left pending while its directory was unlistable: %v", c.pending)
			}
			if tc.holdCursor && !c.fileFloor.Equal(floor) {
				t.Errorf("cursor moved %v -> %v past an unlistable root", floor, c.fileFloor)
			}
		})
	}
}

// TestRun_FileUnreadableOnFirstPassIsReadLater: a file that fails to read on
// the pass that first sees it must be read once it becomes readable, even with
// an mtime behind where the cursor would have moved.
func TestRun_FileUnreadableOnFirstPassIsReadLater(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	path := writeSession(t, home, "s", repo, readFixture(t))
	start := time.Now()
	old := start.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	c := newCollector(t, home, []string{repo}, now)
	ing := &memIngester{}

	orig := maxSessionFile
	maxSessionFile = 100 // pass 1 cannot read the file
	c.runPass(context.Background(), time.Time{}, ing)
	maxSessionFile = orig
	if len(ing.byKey) != 0 {
		t.Fatalf("control: pass 1 stored %d events from a file it could not read", len(ing.byKey))
	}
	mu.Lock()
	clock = start.Add(time.Minute)
	mu.Unlock()
	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != len(fixtureWant) {
		t.Fatalf("pass 2 stored %d events, want %d: the file that failed on pass 1 fell behind the cursor", len(ing.byKey), len(fixtureWant))
	}
}

// TestRun_ResolverFailureKeepsPendingFiles: while one repo's resolver fails,
// its held file must stay pending, or the run's spend is stranded behind the
// cursor once the repo resolves again.
func TestRun_ResolverFailureKeepsPendingFiles(t *testing.T) {
	repoA, repoB := initGitRepo(t), initGitRepo(t)
	home := t.TempDir()
	path := writeSession(t, home, "s", repoB, fixtureLinesWithout(t, `"kind":"terminal"`, `"kind":"workspace_branch"`))
	start := time.Now()
	var mu sync.Mutex
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	setClock := func(t time.Time) { mu.Lock(); clock = t; mu.Unlock() }
	c := newCollector(t, home, []string{repoA, repoB}, now)
	ing := &memIngester{}

	c.runPass(context.Background(), time.Time{}, ing)
	if _, ok := c.pending[path]; !ok {
		t.Fatalf("control: the held file is not pending after pass 1: %v", c.pending)
	}
	old := start.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	gitDir, hidden := filepath.Join(repoB, ".git"), filepath.Join(repoB, "git-hidden")
	if err := os.Rename(gitDir, hidden); err != nil {
		t.Fatal(err)
	}
	setClock(start.Add(time.Minute))
	c.runPass(context.Background(), time.Time{}, ing) // repoB's resolver fails
	if err := os.Rename(hidden, gitDir); err != nil {
		t.Fatal(err)
	}
	setClock(old.Add(settleGrace + time.Minute))
	c.runPass(context.Background(), time.Time{}, ing)
	if len(ing.byKey) != len(fixtureWant) {
		t.Fatalf("stored %d events, want %d: a failing resolver dropped the held file from pending", len(ing.byKey), len(fixtureWant))
	}
}
