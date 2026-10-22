package config

import (
	"strings"
	"testing"
)

func TestLoadCursorAdminStrict(t *testing.T) {
	cfg, err := Load(writeYAML(t, `collectors:
  cursor_admin:
    api_key: "@/secret/cursor-admin"
    api_base_url: "https://cursor.example.test"
    poll_interval: "2h"
    settle_days: 2
    lookback_days: 10
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Collectors.CursorAdmin
	if c == nil || c.APIKey == nil || *c.APIKey != "@/secret/cursor-admin" || *c.APIBaseURL != "https://cursor.example.test" || *c.PollInterval != "2h" || *c.SettleDays != 2 || *c.LookbackDays != 10 {
		t.Fatalf("cursor config=%+v", c)
	}
	absent, err := Load(writeYAML(t, "collectors: {}\n"))
	if err != nil || absent.Collectors.CursorAdmin != nil {
		t.Fatalf("absent=%+v err=%v", absent, err)
	}
	empty, err := Load(writeYAML(t, "collectors:\n  cursor_admin: {}\n"))
	if err != nil || empty.Collectors.CursorAdmin == nil {
		t.Fatalf("empty block lost: err=%v", err)
	}
	const sentinel = "never-echo-sentinel"
	for _, field := range []string{"unknown: true", "settle_days: yesterday", "lookback_days: 1.5"} {
		_, err := Load(writeYAML(t, "collectors:\n  cursor_admin:\n    api_key: "+sentinel+"\n    "+field+"\n"))
		if err == nil {
			t.Fatalf("invalid Cursor config accepted: %s", field)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Fatal("secret in parser error")
		}
	}
}
