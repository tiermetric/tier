package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/tiermetric/tier/internal/event3"
	"github.com/tiermetric/tier/internal/store"
)

func runCorrectionsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		args = []string{"confirm", "--help"}
	}
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: tierd corrections list|show|confirm|dispute|withdraw|settle [flags]")
		return 1
	}
	fs := flag.NewFlagSet("corrections "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "local SQLite database")
	id := fs.Int64("relation", 0, "relation ID")
	rev := fs.Int64("revision", 0, "expected relation revision")
	actor := fs.String("actor", "", "self-asserted actor (not authenticated)")
	role := fs.String("role", "", "self-asserted role; confirmer/settler must be operator")
	reason := fs.String("reason", "", "bounded disposition reason")
	settlement := fs.String("settlement", "", "uphold, re-assess or remove")
	original := fs.String("reviewed-original", "", "reviewed original scope weight")
	own := fs.String("reviewed-own", "", "reviewed follow-up own weight")
	before := fs.String("reviewed-before", "", "cumulative weight before follow-up")
	after := fs.String("reviewed-after", "", "cumulative weight after follow-up")
	pure := fs.Bool("pure-repair", false, "explicit zero growth")
	scope := fs.String("scope", "", "reviewed scope")
	rationale := fs.String("rationale", "", "review rationale")
	reviewer := fs.String("reviewer", "", "reviewer, must equal actor")
	reviewedAt := fs.String("reviewed-at", "", "review time, RFC3339")
	base := fs.String("comparison-base", "", "comparison base SHA")
	prev := fs.String("comparison-before", "", "before SHA")
	next := fs.String("comparison-after", "", "after SHA")
	cursor := fs.String("cursor", "", "next page recording time (RFC3339Nano)")
	cursorID := fs.Int64("cursor-id", 0, "next page ID")
	limit := fs.Int("limit", 100, "page size, at most 1000")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "unexpected positional arguments")
		return 1
	}
	fail := func(err error) int { _, _ = fmt.Fprintln(stderr, err); return 1 }
	cmd := store.CorrectionCommand{RelationID: *id, Revision: *rev, Actor: *actor, Role: *role, Reason: *reason, Settlement: *settlement}
	switch args[0] {
	case "confirm", "dispute", "withdraw", "settle", "list", "show":
	default:
		return fail(fmt.Errorf("unknown corrections command %q", args[0]))
	}
	if args[0] != "list" && *id <= 0 {
		return fail(fmt.Errorf("positive --relation required"))
	}
	acceptsReview := args[0] == "confirm" || (args[0] == "settle" && *settlement == "re-assess")
	if !acceptsReview {
		reviewFlags := false
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "reviewed-original", "reviewed-own", "reviewed-before", "reviewed-after", "pure-repair", "scope", "rationale", "reviewer", "reviewed-at", "comparison-base", "comparison-before", "comparison-after":
				reviewFlags = true
			}
		})
		if reviewFlags {
			return fail(fmt.Errorf("reviewed sizing is accepted only for confirmation or re-assessment"))
		}
	}
	if acceptsReview {
		at, err := time.Parse(time.RFC3339Nano, *reviewedAt)
		if err != nil {
			return fail(fmt.Errorf("reviewed sizing requires --reviewed-at RFC3339: %w", err))
		}
		for _, size := range []string{*original, *own, *before, *after} {
			if size != "" {
				if _, ok := new(big.Rat).SetString(size); !ok {
					return fail(fmt.Errorf("invalid reviewed rational %q", size))
				}
			}
		}
		rat := func(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }
		v := event3.Review{Original: rat(*original), Own: rat(*own), Before: rat(*before), After: rat(*after), PureRepair: *pure, Scope: *scope, Rationale: *rationale, Reviewer: *reviewer, ReviewedAt: at, Revision: *rev, ComparisonBase: *base, ComparisonBefore: *prev, ComparisonAfter: *next}
		if err := v.Validate(); err != nil {
			return fail(err)
		}
		cmd.Review = &v
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = db.Close() }()
	if n := db.UpgradeNotice(); n != "" {
		_, _ = fmt.Fprintln(stderr, n)
	}
	ctx := context.Background()
	var result any
	switch args[0] {
	case "list":
		if *limit <= 0 || *limit > 1000 {
			return fail(fmt.Errorf("limit must be 1..1000"))
		}
		var at time.Time
		if *cursor != "" {
			at, err = time.Parse(time.RFC3339Nano, *cursor)
			if err != nil {
				return fail(err)
			}
			at = at.UTC()
		}
		rows, more, e := db.ListCorrectionEvidence(ctx, "relations", time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{TS: at, ID: *cursorID}, *limit)
		err = e
		result = map[string]any{"actor_identity": "self-asserted; local CLI input is not authentication", "relations": rows, "has_more": more}
	case "show":
		result, err = db.CorrectionDetail(ctx, *id)
	case "confirm":
		err = db.ConfirmCorrection(ctx, cmd)
	case "dispute":
		err = db.DisputeCorrection(ctx, cmd)
	case "withdraw":
		err = db.WithdrawCorrection(ctx, cmd)
	case "settle":
		err = db.SettleCorrection(ctx, cmd)
	}
	if err != nil {
		return fail(err)
	}
	if result == nil {
		result, err = db.CorrectionDetail(ctx, *id)
		if err != nil {
			return fail(err)
		}
	}
	if err = json.NewEncoder(stdout).Encode(result); err != nil {
		return fail(err)
	}
	return 0
}
