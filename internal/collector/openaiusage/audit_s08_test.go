package openaiusage

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// S08-7: a broken continuation preserves fetched rows but never completes the report.
func TestAuditS08PaginationMissingCursorFails(t *testing.T) {
	for _, cost := range []bool{false, true} {
		for _, later := range []bool{false, true} {
			t.Run(fmt.Sprintf("cost=%t/later=%t", cost, later), func(t *testing.T) {
				firstUsage := dayBucket("2026-06-15", modelResult("gpt-4o", 100, 0, 20))
				secondUsage := dayBucket("2026-06-16", modelResult("gpt-4o", 100, 0, 20))
				firstCost := costDay("2026-06-15", 1)
				secondCost := costDay("2026-06-16", 2)
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call := calls.Add(1)
					first := later && call == 1
					if !first && later && r.URL.Query().Get("page") != "second" {
						t.Errorf("continuation cursor = %q", r.URL.Query().Get("page"))
					}
					cursor := ""
					if first {
						cursor = "second"
					}
					if cost {
						bucket := secondCost
						if first {
							bucket = firstCost
						}
						_, _ = w.Write(mustJSON(t, costReport{Data: []costBucket{bucket}, HasMore: true, NextPage: cursor}))
					} else {
						bucket := secondUsage
						if first {
							bucket = firstUsage
						}
						_, _ = w.Write(mustJSON(t, usageReport{Data: []usageBucket{bucket}, HasMore: true, NextPage: cursor}))
					}
				}))
				defer srv.Close()
				c := newTestClient(t, srv, nil)
				var err error
				if cost {
					var buckets []costBucket
					buckets, err = c.fetchCost(context.Background(), fixedNow.Add(-24*time.Hour), fixedNow)
					want := []costBucket{secondCost}
					if later {
						want = append([]costBucket{firstCost}, want...)
					}
					if !reflect.DeepEqual(buckets, want) {
						t.Errorf("cost buckets = %+v, want %+v", buckets, want)
					}
				} else {
					var buckets []usageBucket
					buckets, err = c.fetchUsage(context.Background(), fixedNow.Add(-24*time.Hour), fixedNow)
					want := []usageBucket{secondUsage}
					if later {
						want = append([]usageBucket{firstUsage}, want...)
					}
					if !reflect.DeepEqual(buckets, want) {
						t.Errorf("usage buckets = %+v, want %+v", buckets, want)
					}
				}
				if err == nil || !strings.Contains(err.Error(), "has_more") || !strings.Contains(err.Error(), "next_page") {
					t.Fatalf("missing protocol error: %v", err)
				}
				want := 1
				if later {
					want = 2
				}
				if got := calls.Load(); int(got) != want {
					t.Fatalf("made %d requests, want %d", got, want)
				}
			})
		}
	}
}

// A later-page failure still ingests usage, leaves spend unchanged, and holds
// settlement (and its watermark) until a complete retry succeeds.
func TestAuditS08PaginationPartialPollWithholdsSettlement(t *testing.T) {
	for _, cost := range []bool{false, true} {
		t.Run(fmt.Sprintf("cost=%t", cost), func(t *testing.T) {
			firstUsage := dayBucket("2026-06-15", modelResult("gpt-4o", 100, 0, 20))
			secondUsage := dayBucket("2026-06-16", modelResult("gpt-4o", 100, 0, 20))
			firstCost := costDay("2026-06-15", 1)
			secondCost := costDay("2026-06-16", 2)
			var broken atomic.Bool
			broken.Store(true)
			var costCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				second := r.URL.Query().Get("page") == "second"
				cursor := "second"
				if second {
					cursor = ""
				}
				switch r.URL.Path {
				case usagePath:
					bucket := firstUsage
					if second {
						bucket = secondUsage
					}
					_, _ = w.Write(mustJSON(t, usageReport{Data: []usageBucket{bucket}, HasMore: !second || (broken.Load() && !cost), NextPage: cursor}))
				case costPath:
					costCalls.Add(1)
					bucket := firstCost
					if second {
						bucket = secondCost
					}
					_, _ = w.Write(mustJSON(t, costReport{Data: []costBucket{bucket}, HasMore: !second || (broken.Load() && cost), NextPage: cursor}))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			fs := newFakeStore()
			source := collector.SourceOpenAIUsage
			fs.net[netKey("2026-06", source)] = 5000000
			var logs bytes.Buffer
			p := newTestPoller(newTestClient(t, srv, nil), fs, &logs)
			m := newRecordingMetrics()
			p.metrics = m
			settlements := 0
			p.settled = func(context.Context, time.Time, time.Time) { settlements++ }
			ing := &recordingIngester{}
			p.runPass(context.Background(), ing)
			if len(ing.events) != 2 {
				t.Fatalf("ingested %d events, want both fetched days", len(ing.events))
			}
			for _, ev := range ing.events {
				if ev.InputTok != 100 || ev.OutputTok != 20 || ev.IdempotencyKey == "" {
					t.Errorf("fetched usage not preserved: %+v", ev)
				}
			}
			indexed := indexEvents(ing.events)
			for _, day := range []string{"2026-06-15", "2026-06-16"} {
				if _, ok := indexed[eventKey{"gpt-4o", day}]; !ok {
					t.Errorf("missing fetched day %s", day)
				}
			}
			wantPollOutcomes(t, m.values(), false)
			if settlements != 0 {
				t.Errorf("settled incomplete walk %d times", settlements)
			}
			if len(fs.posted) != 0 || fs.net[netKey("2026-06", source)] != 5000000 {
				t.Errorf("reconciled incomplete month: %+v", fs.posted)
			}
			if !cost && costCalls.Load() != 0 {
				t.Errorf("cost report requested after incomplete usage walk")
			}
			if !strings.Contains(logs.String(), "pagination protocol error") {
				t.Errorf("protocol error not logged: %s", logs.String())
			}
			if !cost {
				events, err := p.Collect(context.Background(), time.Time{})
				if len(events) != 2 || err == nil {
					t.Errorf("Collect lost fetched usage or error: %v, %v", events, err)
				}
			}
			broken.Store(false)
			p.runPass(context.Background(), ing)
			wantPollOutcomes(t, m.values(), false, true)
			if settlements != 1 {
				t.Errorf("complete retry settled %d times, want 1", settlements)
			}
			if len(ing.events) != 4 {
				t.Fatalf("retry ingested %d total events, want 4", len(ing.events))
			}
			for i := 0; i < 2; i++ {
				if !reflect.DeepEqual(ing.events[i], ing.events[i+2]) {
					t.Errorf("retry changed idempotent usage event")
				}
			}
			if len(fs.posted) != 1 || fs.net[netKey("2026-06", source)] != 3000000 {
				t.Errorf("complete retry spend = %d, rows = %v; want 3000000 across one delta", fs.net[netKey("2026-06", source)], fs.posted)
			}
		})
	}
}

func TestAuditS08PaginationTerminalPageNeedsNoCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[{}],"has_more":false}`)) }))
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	if got, err := c.fetchUsage(context.Background(), fixedNow.Add(-24*time.Hour), fixedNow); err != nil || len(got) != 1 {
		t.Fatalf("terminal usage: %v, %v", got, err)
	}
	if got, err := c.fetchCost(context.Background(), fixedNow.Add(-24*time.Hour), fixedNow); err != nil || len(got) != 1 {
		t.Fatalf("terminal cost: %v, %v", got, err)
	}
}
