package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// decodeScores unmarshals a wire payload the way doctor really does. The tests
// below deliberately go through JSON rather than constructing scoresResponse
// literals: the defect this check is most likely to regress into lives in the
// TAGS, not the logic — decoding window_predates_cost_capture into a plain bool
// would turn "the server said nothing" into "the window is covered", and a
// struct literal would sail straight past that.
func decodeScores(t *testing.T, body string) scoresResponse {
	t.Helper()
	var s scoresResponse
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return s
}

func TestCheckCostHorizon(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		want       checkStatus
		wantAbsent bool     // no result emitted at all
		mustSay    []string // substrings the operator-facing detail/hint must carry
		mustNotSay []string
	}{
		{
			name: "window predates the horizon warns and names both dates",
			body: `{"since":"2026-04-28","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "cost_coverage_safe_since":"2026-06-24",
			                        "window_predates_cost_capture":true}}`,
			want: statusWarn,
			// Asserts the POSITIVE phrasing, not the absence of an implausible
			// substring: the hint must actively tell the operator retention is the
			// wrong lever, since that is the knob they would otherwise reach for.
			mustSay:    []string{"2026-04-28", "2026-06-23", "HIGH", "NOT a retention setting"},
			mustNotSay: []string{"session logs"},
		},
		{
			// The remedy must be a value that CLEARS the warning. The horizon is an
			// instant (10:01) but `since` only parses to midnight, so echoing the
			// horizon's own day would hand back an instruction that reproduces this
			// warning verbatim, forever. The server precomputes the first fully
			// covered day; doctor must print THAT.
			name: "remedy prints the safe since, not the horizon's own day",
			body: `{"since":"2026-04-28","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "cost_coverage_safe_since":"2026-06-24",
			                        "window_predates_cost_capture":true}}`,
			want:       statusWarn,
			mustSay:    []string{"?since=2026-06-24"},
			mustNotSay: []string{"?since=2026-06-23"},
		},
		{
			name: "covered window reports OK",
			body: `{"since":"2026-07-01","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "window_predates_cost_capture":false}}`,
			want:    statusOK,
			mustSay: []string{"2026-06-23", "fully covered"},
		},
		{
			// The version-skew arm. A server older than #512 emits neither field;
			// silence must NOT read as coverage.
			name: "server reported cost but no horizon signal warns",
			body: `{"since":"2026-04-28","total":{"total_cost_usd":100},
			        "data_quality":{}}`,
			want:    statusWarn,
			mustSay: []string{"cannot be checked", "#512"},
		},
		{
			// Distinguished from the arm above ONLY by total cost. A store with no
			// cost has nothing to cover, and warning there would train operators to
			// ignore the check on every fresh install.
			name:       "no signal and no cost stays silent",
			body:       `{"since":"2026-04-28","total":{"total_cost_usd":0},"data_quality":{}}`,
			wantAbsent: true,
		},
		{
			// The global horizon is the loosest bound: this window clears it, so the
			// server's own flag is false, yet one capture path started later and
			// contributes no cost to the earlier part of the window.
			name: "window clears the global horizon but predates a source",
			body: `{"since":"2026-07-01","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "window_predates_cost_capture":false,
			                        "source_coverage_start":{"jsonl":"2026-06-23T10:01:01Z",
			                                                 "codex-rollout":"2026-07-15T00:00:00Z"}}}`,
			want:    statusWarn,
			mustSay: []string{"codex-rollout", "2026-07-15"},
			// The source that IS covered must not be named as a problem.
			mustNotSay: []string{"jsonl ("},
		},
		{
			// A comparison that could not be MADE must not report as one that
			// passed. This case previously asserted statusOK — "fully covered" for
			// sources nobody checked — which is the exact false-green class this
			// whole change exists to eliminate, committed by the check itself.
			name: "unparseable since WARNS rather than reporting covered",
			body: `{"since":"not-a-date","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "window_predates_cost_capture":false,
			                        "source_coverage_start":{"jsonl":"2026-06-23T10:01:01Z",
			                                                 "codex-rollout":"2026-07-15T00:00:00Z"}}}`,
			want:    statusWarn,
			mustSay: []string{"could NOT be checked"},
			// Must not name a specific late source: none was established.
			mustNotSay: []string{"codex-rollout (", "fully covered"},
		},
		{
			// Same rule one level down: a source whose own date is unusable leaves
			// that source unplaced, so the set cannot be cleared.
			name: "unparseable SOURCE date also warns rather than reporting covered",
			body: `{"since":"2026-07-01","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "window_predates_cost_capture":false,
			                        "source_coverage_start":{"jsonl":"2026-06-23T10:01:01Z",
			                                                 "codex-rollout":"garbage"}}}`,
			want:       statusWarn,
			mustSay:    []string{"could NOT be checked"},
			mustNotSay: []string{"fully covered"},
		},
		{
			// A single-source (or pre-#512) server sends no map at all. That is not
			// an unmade comparison — there is no claim to check — so it must stay
			// green rather than warn on every ordinary install.
			name: "absent source map is covered, not unverifiable",
			body: `{"since":"2026-07-01","total":{"total_cost_usd":100},
			        "data_quality":{"cost_coverage_start":"2026-06-23T10:01:01Z",
			                        "window_predates_cost_capture":false}}`,
			want:    statusOK,
			mustSay: []string{"fully covered"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkCostHorizon(decodeScores(t, tt.body))
			if tt.wantAbsent {
				if len(got) != 0 {
					t.Fatalf("want no result, got %+v", got)
				}
				return
			}
			if s := statusOf(got, "cost horizon"); s != tt.want {
				t.Fatalf("status = %v, want %v (results: %+v)", s, tt.want, got)
			}
			text := got[0].detail + " " + got[0].hint
			for _, want := range tt.mustSay {
				if !strings.Contains(text, want) {
					t.Errorf("message must mention %q; got %q", want, text)
				}
			}
			for _, bad := range tt.mustNotSay {
				if strings.Contains(text, bad) {
					t.Errorf("message must NOT mention %q; got %q", bad, text)
				}
			}
		})
	}
}

// TestCostHorizonExplicitFalseSurvivesDecode pins the wire contract the whole
// signal rests on. The server emits an explicit false for a covered window
// precisely so that "checked and covered" is distinguishable from "no signal";
// if this field is ever decoded into a plain bool, BOTH states arrive as false
// and doctor silently reports a pre-#512 server as covered. That failure is
// invisible in every other test, so it gets its own.
func TestCostHorizonExplicitFalseSurvivesDecode(t *testing.T) {
	covered := decodeScores(t, `{"data_quality":{"window_predates_cost_capture":false}}`)
	if covered.DataQuality.WindowPredatesCostCapture == nil {
		t.Fatal("explicit false decoded as nil — 'covered' is indistinguishable from 'no signal'")
	}
	if *covered.DataQuality.WindowPredatesCostCapture {
		t.Fatal("explicit false decoded as true")
	}

	silent := decodeScores(t, `{"data_quality":{}}`)
	if silent.DataQuality.WindowPredatesCostCapture != nil {
		t.Fatal("absent field decoded as non-nil — 'no signal' is indistinguishable from 'covered'")
	}
}

// TestCostHorizonSanitizesServerStrings pins the terminal-injection barrier.
// doctor points at an arbitrary --server, so every date it prints is attacker-
// controllable. Without sanitization a response can clear the screen and forge
// doctor's own summary line — defeating the check whose entire purpose is to stop
// an operator misreading the report.
func TestCostHorizonSanitizesServerStrings(t *testing.T) {
	// ESC and CRLF written as JSON \u / \r escapes so the payload is valid JSON
	// on the wire -- which is exactly how a hostile server would send it.
	hostile := "{\"since\":\"\\u001b[2J\\u001b[1;1HFAKE doctor: all checks passed\\r\\n\"," +
		"\"total\":{\"total_cost_usd\":100}," +
		"\"data_quality\":{\"cost_coverage_start\":\"\\u001b[31mnot-a-time\"," +
		"\"window_predates_cost_capture\":true}}"
	got := checkCostHorizon(decodeScores(t, hostile))
	if len(got) == 0 {
		t.Fatal("want a result")
	}
	text := got[0].detail + " " + got[0].hint
	if strings.Contains(text, "\x1b") {
		t.Errorf("raw ESC reached the terminal string: %q", text)
	}
	for _, bad := range []string{"\n", "\r"} {
		if strings.Contains(text, bad) {
			t.Errorf("raw %q reached the terminal string: %q", bad, text)
		}
	}
}

// TestSafeDateStrKeepsPastableDates guards the other half of that barrier: the
// sanitizer must not mangle a legitimate date, or the remedy becomes unpastable
// (?since="2026-06-24" with the quotes) and operators stop following it.
func TestSafeDateStrKeepsPastableDates(t *testing.T) {
	if got := safeDateStr("2026-06-24"); got != "2026-06-24" {
		t.Errorf("a valid date must survive verbatim, got %q", got)
	}
	if got := safeDateStr("\x1b[2Jhi"); strings.Contains(got, "\x1b") {
		t.Errorf("hostile input must be sanitized, got %q", got)
	}
}

// TestReportDoctorSummaryDistinguishesWarnFromClean covers the closing line an
// operator actually reads. A warned run previously ended with "all checks
// passed", which tells them the warnings above were cosmetic — and one of them
// now says the number they just looked at is inflated.
func TestReportDoctorSummaryDistinguishesWarnFromClean(t *testing.T) {
	tests := []struct {
		name       string
		results    []checkResult
		wantCode   int
		mustSay    string
		mustNotSay string
	}{
		{
			name:     "clean run still says all checks passed",
			results:  []checkResult{{name: "a", status: statusOK}},
			wantCode: 0,
			mustSay:  "all checks passed",
		},
		{
			name: "warned run does NOT claim all checks passed",
			results: []checkResult{
				{name: "a", status: statusOK},
				{name: "cost horizon", status: statusWarn, detail: "d", hint: "h"},
			},
			wantCode:   0, // exit-code contract is unchanged: WARN never fails the process
			mustSay:    "1 check(s) need attention",
			mustNotSay: "all checks passed",
		},
		{
			name: "failed run reports failure and exits 1",
			results: []checkResult{
				{name: "cost horizon", status: statusWarn},
				{name: "b", status: statusFail},
			},
			wantCode:   1,
			mustSay:    "FAILED",
			mustNotSay: "all checks passed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := reportDoctor(tt.results, &buf)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			out := buf.String()
			if !strings.Contains(out, tt.mustSay) {
				t.Errorf("summary must contain %q; got:\n%s", tt.mustSay, out)
			}
			if tt.mustNotSay != "" && strings.Contains(out, tt.mustNotSay) {
				t.Errorf("summary must NOT contain %q; got:\n%s", tt.mustNotSay, out)
			}
		})
	}
}
