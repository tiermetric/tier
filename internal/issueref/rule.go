package issueref

import (
	"errors"
	"slices"
)

// Rule is a closed extraction-path label; empty means no extractor evidence.
type Rule string

// Ref pairs an issue ID with the extraction path that produced it.
type Ref struct {
	ID   string
	Rule Rule
}

const (
	RuleBranchFirstNumeric   Rule = "branch_first_numeric"
	RuleBranchTrackerShape   Rule = "branch_tracker_shape"
	RuleBodyCloseDirective   Rule = "body_close_directive"
	RuleBodyTrackerShape     Rule = "body_tracker_shape"
	RuleBodyBareHash         Rule = "body_bare_hash"
	RuleCommitCloseDirective Rule = "commit_close_directive"
	RuleCommitTrackerShape   Rule = "commit_tracker_shape"
	RuleBranchFirstSlot      Rule = "branch_first_slot"
	RuleBranchConfiguredKey  Rule = "branch_configured_key"
	RuleBodyConfiguredKey    Rule = "body_configured_key"
	RuleBodyRelationRef      Rule = "body_relation_ref"
	RuleCommitConfiguredKey  Rule = "commit_configured_key"
)

// Rules returns the closed set accepted from clients, including future strict paths.
func Rules() []Rule {
	return []Rule{
		RuleBranchFirstNumeric, RuleBranchTrackerShape,
		RuleBodyCloseDirective, RuleBodyTrackerShape, RuleBodyBareHash,
		RuleCommitCloseDirective, RuleCommitTrackerShape,
		RuleBranchFirstSlot, RuleBranchConfiguredKey,
		RuleBodyConfiguredKey, RuleBodyRelationRef, RuleCommitConfiguredKey,
	}
}

// ErrInvalidRule never includes the refused value.
var ErrInvalidRule = errors.New("issue_rule is outside the closed set")

// Validate accepts an absent rule or a member of Rules.
func Validate(r Rule) error {
	if r == "" || slices.Contains(Rules(), r) {
		return nil
	}
	return ErrInvalidRule
}
