package store

import (
	"errors"
	"slices"
	"strings"
)

// AttributionRule names the rule that assigned a token event's issue_id (#823).
// The set is closed and holds only fixed labels: ValidateAttributionRule refuses
// every other value, so no path text can reach the attribution_rule column. The
// empty rule means none was recorded and is stored as NULL.
type AttributionRule string

// The recordable attribution rules.
const (
	AttributionRuleNone             AttributionRule = ""
	AttributionRuleBranch           AttributionRule = "branch"
	AttributionRuleWorktreeCWD      AttributionRule = "worktree-cwd"
	AttributionRuleWorktreeToolPath AttributionRule = "worktree-toolpath"
	AttributionRuleCarry            AttributionRule = "carry"
)

// AttributionRules returns every non-empty rule ValidateAttributionRule accepts.
// It is the one list of the closed set.
func AttributionRules() []AttributionRule {
	return []AttributionRule{
		AttributionRuleBranch, AttributionRuleWorktreeCWD,
		AttributionRuleWorktreeToolPath, AttributionRuleCarry,
	}
}

// ErrInvalidAttributionRule reports a rule outside the closed set. It never
// carries the refused value, which may be arbitrary caller text.
var ErrInvalidAttributionRule = errors.New("attribution_rule must be empty or one of: " + joinAttributionRules())

func joinAttributionRules() string {
	names := make([]string, 0, len(AttributionRules()))
	for _, r := range AttributionRules() {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}

// ValidateAttributionRule accepts the empty rule and the members of
// AttributionRules, and returns ErrInvalidAttributionRule for anything else.
func ValidateAttributionRule(r AttributionRule) error {
	if r == AttributionRuleNone || slices.Contains(AttributionRules(), r) {
		return nil
	}
	return ErrInvalidAttributionRule
}
