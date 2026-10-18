package main

import (
	"flag"
	"os"

	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/issueref"
)

func addTrackerKeysFlag(fs *flag.FlagSet) *string {
	return fs.String("tracker-keys", os.Getenv("TIER_TRACKER_KEYS"), "comma-separated uppercase tracker prefixes; OP is built in. Precedence: CLI > TIER_TRACKER_KEYS > attribution.tracker_keys > empty")
}

func resolveTrackerGrammar(fs *flag.FlagSet, value, configPath string) (issueref.Grammar, error) {
	cli := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "tracker-keys" {
			cli = true
		}
	})
	if !cli && os.Getenv("TIER_TRACKER_KEYS") == "" && configPath != "" {
		cfg, err := config.Load(configPath)
		if err != nil {
			return issueref.Grammar{}, err
		}
		if cfg.Attribution.TrackerKeys != nil {
			value = *cfg.Attribution.TrackerKeys
		}
	}
	return issueref.NewGrammar(value)
}
