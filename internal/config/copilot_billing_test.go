package config

import "testing"

func TestCopilotBillingStrictConfigParse(t *testing.T) {
	cfg, err := Load(writeYAML(t, `collectors:
  copilot_billing:
    org: acme
    token: '@/secrets/github-token'
    app_id: '42'
    installation_id: '123'
    private_key: '@/secrets/key.pem'
    api_base_url: https://ghe.example/api/v3
    poll_interval: 6h
    settle_days: 2
    lookback_days: 7
`))
	if err != nil {
		t.Fatal(err)
	}
	// Like its siblings, semantic/secret validation is shared by serve and
	// doctor; Load is responsible for strict schema and presence semantics.
	c := cfg.Collectors.CopilotBilling
	if c == nil || c.Org == nil || *c.Org != "acme" || c.Token == nil || *c.Token != "@/secrets/github-token" || c.AppID == nil || *c.AppID != "42" || c.InstallationID == nil || *c.InstallationID != "123" || c.PrivateKey == nil || *c.PrivateKey != "@/secrets/key.pem" || c.APIBaseURL == nil || *c.APIBaseURL != "https://ghe.example/api/v3" || c.PollInterval == nil || *c.PollInterval != "6h" || c.SettleDays == nil || *c.SettleDays != 2 || c.LookbackDays == nil || *c.LookbackDays != 7 {
		t.Fatal("Copilot fields lost during strict parse")
	}
	for _, body := range []string{
		"collectors:\n  copilot_billing:\n    tokne: secret\n",
		"collectors:\n  copilot_billing:\n    settle_days: bad\n",
	} {
		if _, err := Load(writeYAML(t, body)); err == nil {
			t.Fatal("accepted typo or malformed field")
		}
	}
	cfg, err = Load(writeYAML(t, "collectors: {}\n"))
	if err != nil || cfg.Collectors.CopilotBilling != nil {
		t.Fatal("absent block enabled poller")
	}
	cfg, err = Load(writeYAML(t, "collectors:\n  copilot_billing: {}\n"))
	if err != nil || cfg.Collectors.CopilotBilling == nil {
		t.Fatal("empty block lost presence")
	}
}
