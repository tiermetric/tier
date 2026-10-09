package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/logsafe"
)

// The #823 worktree-attribution switch: one flag name on every command that
// scans Claude Code JSONL (ship, serve's watcher, score). Off by default.
const (
	worktreeAttrFlag      = "worktree-attribution"
	worktreeAttrEnv       = "TIER_WORKTREE_ATTRIBUTION"
	worktreeAttrConfigKey = "watch.worktree_attribution"
)

// worktreeAttrWhat is the flag's help shared by ship and serve.
const worktreeAttrWhat = "attribute Claude Code spend to the git worktree its session or tool calls worked in, and record the rule that chose each issue (#823). OFF by default, and a PREVIEW: carry in a parent session (any top-level session, interactive ones included) was wrong in 12 of 14 hand-checked messages, and a stored attribution is permanent. Read docs/how-it-works.md, \"Measured accuracy\", before turning it on. "

// worktreeAttrHelp is ship's help: ship sends events to a tierd server.
const worktreeAttrHelp = worktreeAttrWhat + "UPGRADE TIERD (THE SERVER) FIRST: with this on, events carry attribution_rule and may carry the unattributed:foreign-repo bucket, and a tierd without #823 support rejects every such batch with HTTP 400. Nothing is lost if the server is upgraded while the transcripts still exist (Claude Code keeps about 30 days) and a re-ship's --since covers the gap. Preview the effect with `tierd score --worktree-attribution`. ship reads only this flag or env " + worktreeAttrEnv + " (it takes no config file), and the upgrade-first rule applies to both"

// worktreeAttrServeHelp is serve's help: its watcher writes serve's own store,
// so no upgrade order applies.
const worktreeAttrServeHelp = worktreeAttrWhat + "Governs only serve's own watcher, which writes this server's store; each `tierd ship` host sets its own --" + worktreeAttrFlag + " or " + worktreeAttrEnv + ". Env " + worktreeAttrEnv + ", config key " + worktreeAttrConfigKey + " (read only by serve). Precedence: CLI > env > config > default"

// worktreeAttrSetting is --worktree-attribution resolved, and where from.
type worktreeAttrSetting struct {
	on   bool
	from string
}

// resolveWorktreeAttribution applies CLI > env > config > default (off). cli
// is the parsed flag; cfg is the config value, nil when absent or when the
// command reads no config.
func resolveWorktreeAttribution(fs *flag.FlagSet, cli bool, cfg *bool) (worktreeAttrSetting, error) {
	// A malformed env value fails even under an explicit flag, as envBool does.
	v := os.Getenv(worktreeAttrEnv)
	env, err := strconv.ParseBool(v)
	if v != "" && err != nil {
		return worktreeAttrSetting{}, fmt.Errorf("%s must be a boolean (true|false|1|0), got %q", worktreeAttrEnv, v)
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == worktreeAttrFlag })
	if set {
		return worktreeAttrSetting{on: cli, from: "flag --" + worktreeAttrFlag}, nil
	}
	if v != "" {
		return worktreeAttrSetting{on: env, from: "env " + worktreeAttrEnv}, nil
	}
	if cfg != nil {
		return worktreeAttrSetting{on: *cfg, from: "config " + worktreeAttrConfigKey}, nil
	}
	return worktreeAttrSetting{from: "default"}, nil
}

// String is the startup line naming the state and its source.
func (s worktreeAttrSetting) String() string {
	state := "off"
	if s.on {
		state = "on"
	}
	return fmt.Sprintf("worktree attribution (#823): %s (from %s)", state, s.from)
}

// attribution is the index over targets when on, nil (today's rule) when off.
func (s worktreeAttrSetting) attribution(targets []string) *collector.WorktreeAttribution {
	if !s.on {
		return nil
	}
	return collector.NewWorktreeAttribution(targets)
}

// worktreeAuditSample bounds the changed messages the audit lists.
const worktreeAuditSample = 20

type worktreeAuditChange struct{ old, new collector.TokenEvent }

// printWorktreeAudit prints what #823 attribution changes: on and off are the
// same JSONL scan with the flag on and off. It prints counts, repo slugs, issue
// labels, session ids and timestamps: never message content, never a path.
func printWorktreeAudit(w io.Writer, off, on []collector.TokenEvent) {
	offBy := make(map[string]collector.TokenEvent, len(off))
	for _, e := range off {
		offBy[e.IdempotencyKey] = e
	}
	rules, targets := map[string]int{}, map[string]int{}
	onKeys := make(map[string]bool, len(on))
	foreign, onlyOn := 0, 0
	var changed []worktreeAuditChange
	for _, e := range on {
		onKeys[e.IdempotencyKey] = true
		rules[cmp.Or(string(e.AttributionRule), "none")]++
		targets[e.Repo]++
		if e.IssueID == collector.UnattributedForeignRepo {
			foreign++
		}
		o, ok := offBy[e.IdempotencyKey]
		switch {
		case !ok:
			onlyOn++
		case o.IssueID != e.IssueID || o.Repo != e.Repo:
			changed = append(changed, worktreeAuditChange{old: o, new: e})
		}
	}
	onlyOff := 0
	for k := range offBy {
		if !onKeys[k] {
			onlyOff++
		}
	}
	_, _ = fmt.Fprintln(w, "\nWorktree attribution audit (#823): a dry run, nothing is stored")
	_, _ = fmt.Fprintln(w, "  compared with a local flag-off scan; the server keeps the first issue, repo and rule it stored for a message, so only messages it has not stored yet change")
	_, _ = fmt.Fprintf(w, "  messages: %d with the flag off, %d with it on\n", len(off), len(on))
	_, _ = fmt.Fprintf(w, "  changed attribution (issue or repo): %d\n", len(changed))
	_, _ = fmt.Fprintf(w, "  only with the flag off (booked to another target): %d\n", onlyOff)
	_, _ = fmt.Fprintf(w, "  only with the flag on: %d\n", onlyOn)
	_, _ = fmt.Fprintf(w, "  %s: %d\n", collector.UnattributedForeignRepo, foreign)
	// Rules are this repo's constants; a repo slug is git-config text.
	for _, c := range []struct {
		title  string
		counts map[string]int
		render func(string) string
	}{{"by rule (flag on)", rules, func(s string) string { return s }}, {"by repo (flag on)", targets, logsafe.Str}} {
		_, _ = fmt.Fprintf(w, "  %s:\n", c.title)
		for _, k := range slices.Sorted(maps.Keys(c.counts)) {
			_, _ = fmt.Fprintf(w, "    %-34s %d\n", c.render(k), c.counts[k])
		}
	}
	slices.SortFunc(changed, func(a, b worktreeAuditChange) int {
		return cmp.Or(a.new.Timestamp.Compare(b.new.Timestamp), cmp.Compare(a.new.IdempotencyKey, b.new.IdempotencyKey))
	})
	if len(changed) > worktreeAuditSample {
		_, _ = fmt.Fprintf(w, "  changed messages (the first %d of %d by time):\n", worktreeAuditSample, len(changed))
		changed = changed[:worktreeAuditSample]
	} else if len(changed) > 0 {
		_, _ = fmt.Fprintln(w, "  changed messages:")
	}
	for _, c := range changed {
		// Issue labels come from git refnames or this repo's constants (see
		// printIssueCosts); the session id and repo slug are transcript and
		// git-config text, so they are stripped.
		_, _ = fmt.Fprintf(w, "    %s  session %s  %s %s -> %s %s (%s)\n",
			c.new.Timestamp.UTC().Format(time.RFC3339), logsafe.Str(c.new.SessionID),
			logsafe.Str(c.old.Repo), issueLabel(c.old.IssueID),
			logsafe.Str(c.new.Repo), issueLabel(c.new.IssueID), c.new.AttributionRule)
	}
}
