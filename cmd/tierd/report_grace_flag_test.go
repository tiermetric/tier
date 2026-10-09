package main

import (
	"flag"
	"strings"
	"testing"
	"time"
)

// TestReportGraceConstants pins the #913 D2 ruling: default 14 days, minimum 1 day.
func TestReportGraceConstants(t *testing.T) {
	if defaultReportGrace != 14*24*time.Hour {
		t.Errorf("defaultReportGrace = %s, want 336h (14 days)", defaultReportGrace)
	}
	if minReportGrace != 24*time.Hour {
		t.Errorf("minReportGrace = %s, want 24h (1 day)", minReportGrace)
	}
}

// TestValidateReportGrace pins the minimum: anything under 24h is refused with an
// error naming the flag, and 24h itself is accepted.
func TestValidateReportGrace(t *testing.T) {
	for _, d := range []time.Duration{-time.Hour, 0, time.Hour, 24*time.Hour - time.Second} {
		err := validateReportGrace(d)
		if err == nil {
			t.Errorf("validateReportGrace(%s) = nil, want a refusal below 24h", d)
			continue
		}
		if !strings.Contains(err.Error(), "--report-grace") {
			t.Errorf("validateReportGrace(%s) error %q does not name --report-grace", d, err)
		}
	}
	for _, d := range []time.Duration{24 * time.Hour, defaultReportGrace, 90 * 24 * time.Hour} {
		if err := validateReportGrace(d); err != nil {
			t.Errorf("validateReportGrace(%s) = %v, want nil", d, err)
		}
	}
}

// TestParseEnvDuration pins TIER_REPORT_GRACE's parsing: unset keeps the default,
// a Go duration is taken, and a malformed value (including a "14d" day suffix,
// which Go durations do not accept) is an error naming the variable.
func TestParseEnvDuration(t *testing.T) {
	got, err := parseEnvDuration("TIER_REPORT_GRACE", "", defaultReportGrace)
	if err != nil || got != defaultReportGrace {
		t.Errorf("unset: got (%s, %v), want (%s, nil)", got, err, defaultReportGrace)
	}
	got, err = parseEnvDuration("TIER_REPORT_GRACE", "48h", defaultReportGrace)
	if err != nil || got != 48*time.Hour {
		t.Errorf("48h: got (%s, %v), want (48h, nil)", got, err)
	}
	for _, bad := range []string{"banana", "14d", "14"} {
		_, err := parseEnvDuration("TIER_REPORT_GRACE", bad, defaultReportGrace)
		if err == nil {
			t.Errorf("%q: want an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "TIER_REPORT_GRACE") || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: error %q must name the variable and quote the value", bad, err)
		}
	}
}

// TestApplyReportGraceFromConfig_Precedence pins CLI > env > config > default for
// the report_grace key, the same table TestApplyAuthFromConfig_Precedence runs for
// the auth durations. A malformed config value is refused by parseConfigDuration,
// the check applyDurationFromConfig exits on.
func TestApplyReportGraceFromConfig_Precedence(t *testing.T) {
	ptr := func(s string) *string { return &s }
	cases := []struct {
		name     string
		cliSet   bool
		envSet   bool
		cfgValue *string
		want     string
	}{
		{"no overrides -> builtin default", false, false, nil, "336h0m0s"},
		{"config only -> config wins", false, false, ptr("48h"), "48h0m0s"},
		{"env set blocks config", false, true, ptr("48h"), "336h0m0s"},
		{"cli set blocks config", true, false, ptr("48h"), "336h0m0s"},
		{"cli set blocks env and config", true, true, ptr("48h"), "336h0m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.Duration("report-grace", defaultReportGrace, "")
			setFlags := map[string]bool{}
			if tc.cliSet {
				setFlags["report-grace"] = true
			}
			envVarSet := map[string]bool{}
			if tc.envSet {
				envVarSet["report-grace"] = true
			}
			applyDurationFromConfig(fs, setFlags, envVarSet, "report-grace", tc.cfgValue)
			if got := fs.Lookup("report-grace").Value.String(); got != tc.want {
				t.Errorf("report-grace resolved to %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("malformed config value refused", func(t *testing.T) {
		_, err := parseConfigDuration("report-grace", "14d")
		if err == nil || !strings.Contains(err.Error(), "report-grace") || !strings.Contains(err.Error(), "14d") {
			t.Errorf("parseConfigDuration(report-grace, 14d) = %v, want an error naming the key and value", err)
		}
	})
}
