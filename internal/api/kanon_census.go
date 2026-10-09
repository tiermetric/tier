package api

// The k-anonymity census (#856): which active ids in one window count as people
// toward the k floor. Before #856 the floor counted developer id strings, so ids
// that are not k distinct people (a manual /costs row, a push-only commit author,
// an id nobody put on the roster, a bot) could lift a group to k and publish the
// figures of the real people in it.
//
// An active id counts only when ALL of these hold:
//   - it is not a pseudo-developer (#853);
//   - it is not a bot: under any raw id active in the window that canonicalizes
//     to it, GitHub user.type "Bot" on an outcome the webhook captured, a "[bot]"
//     login suffix, or a login on the fixed list below (for rows captured before
//     user.type was stored);
//   - it is on the roster in the window: one of the raw ids its cost events or
//     outcomes in the window were recorded under holds its own membership row
//     overlapping the window. An alias never lends an idle id's rows to one that
//     acted, so an alias edit cannot roster a past window's activity (#914);
//   - it has CAPTURED evidence in the window: a token event from any source other
//     than a manual /costs row, or an outcome other than a push-captured one.
//
// Ids are canonicalized through the alias map first, so two aliases of one person
// are one id. Every check reads the CANONICAL id (the roster and bot flags are
// set on it from its raw ids, as above).
//
// The census is built once per window and handed, unchanged, to every fold that
// window feeds (the pooled rows, every work-type segment, and its side of
// /compare), so no path can count differently. An uncounted id's figures stay
// summed in its row and in the totals: only the count excludes it. A
// pseudo-developer's rows are dropped from the window first (#864).
//
// The promise this makes is scoped to the READ token. A write-token holder can
// edit the roster and aliases today (#908), so no counting rule stops them.

import (
	"context"
	"fmt"
	"strings"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// knownBotLogins are bot accounts that do not carry the "[bot]" suffix, matched
// case-insensitively. They cover outcome rows captured before user.type was
// stored (#856); a newly captured row is recognised by its type.
var knownBotLogins = map[string]struct{}{
	"copilot":            {},
	"copilot-swe-agent":  {},
	"dependabot":         {},
	"dependabot-preview": {},
	"github-actions":     {},
	"renovate":           {},
	"renovate-bot":       {},
}

// isBotLogin reports whether a login names a bot by its spelling alone.
func isBotLogin(id string) bool {
	l := strings.ToLower(id)
	if strings.HasSuffix(l, "[bot]") {
		return true
	}
	_, ok := knownBotLogins[l]
	return ok
}

// uncountedActiveIDsJSON is data_quality.uncounted_active_ids (#856): how many
// active ids in the window did not count toward the k floor, split by reason.
// Stated once per window, org-wide, and only on /scores: never per row, per
// team, per segment, or per /compare window, where a count beside a row would
// be a finer slice of the people inside it.
//
// Each uncounted id is classified once, by the first reason that applies in this
// order: bot, not_on_roster, then the evidence reason (manual_only when it has any
// manual /costs row, else push_only).
type uncountedActiveIDsJSON struct {
	ManualOnly  int `json:"manual_only"`
	PushOnly    int `json:"push_only"`
	NotOnRoster int `json:"not_on_roster"`
	Bot         int `json:"bot"`
}

func (u uncountedActiveIDsJSON) any() bool {
	return u.ManualOnly+u.PushOnly+u.NotOnRoster+u.Bot > 0
}

// kanonCensus is one window's counting rule. Its zero value counts nobody.
type kanonCensus struct {
	counted map[string]bool
	// uncountedActive is the per-reason count of active ids that did not count.
	uncountedActive uncountedActiveIDsJSON
}

// scoring is the census the folds take. Both predicates fail closed: an id the
// census never classified does not count, and a row whose captured cost was
// never filled (groupWindow fills it) has none.
func (c kanonCensus) scoring() scoring.Census {
	return scoring.Census{
		Uncounted: func(dev string) bool { return !c.counted[dev] },
		UncapturedCost: func(row scoring.DeveloperScore, realtime bool) bool {
			if realtime {
				return row.CapturedRealtimeUSD <= 0
			}
			return row.CapturedNonRealtimeUSD <= 0
		},
		Pseudo: store.ResemblesUnattributed,
	}
}

// kanonCensusFor classifies every active id of win. roster is the canonical
// developers on the roster in the window (windowGroups.roster).
func (h *Handler) kanonCensusFor(ctx context.Context, r windowReader, win windowScores, roster map[string]bool) (kanonCensus, error) {
	evidence, err := r.DeveloperEvidenceWindow(ctx, win.since, win.until, win.scope)
	if err != nil {
		return kanonCensus{}, fmt.Errorf("query cost evidence: %w", err)
	}
	bots, err := r.BotDevelopers(ctx, win.since, win.until)
	if err != nil {
		return kanonCensus{}, fmt.Errorf("query bot developers: %w", err)
	}
	capturedCost := map[string]bool{}
	manualCost := map[string]bool{}
	for _, e := range evidence {
		dev := win.canon(e.Developer)
		if e.CapturedRows > 0 {
			capturedCost[dev] = true
		}
		if e.ManualRows > 0 {
			manualCost[dev] = true
		}
	}
	capturedOutcome := map[string]bool{}
	for _, o := range win.outcomes {
		if o.Source != store.OutcomeSourcePush {
			capturedOutcome[win.canon(o.Developer)] = true
		}
	}
	// A bot mark on any raw id makes its canonical id a bot, whether the mark is
	// a captured user.type or the raw id's spelling: an alias must not erase it.
	bot := make(map[string]bool, len(bots))
	for _, b := range bots {
		bot[win.canon(b)] = true
	}
	for _, e := range evidence {
		if isBotLogin(e.Developer) {
			bot[win.canon(e.Developer)] = true
		}
	}
	for _, o := range win.outcomes {
		if isBotLogin(o.Developer) {
			bot[win.canon(o.Developer)] = true
		}
	}

	c := kanonCensus{counted: map[string]bool{}}
	for _, s := range win.devScores {
		dev := s.Developer
		if !s.HasActivity() || store.ResemblesUnattributed(dev) {
			continue
		}
		switch {
		case bot[dev] || isBotLogin(dev):
			c.uncountedActive.Bot++
		case !roster[dev]:
			c.uncountedActive.NotOnRoster++
		case !capturedCost[dev] && !capturedOutcome[dev]:
			if manualCost[dev] {
				c.uncountedActive.ManualOnly++
			} else {
				c.uncountedActive.PushOnly++
			}
		default:
			c.counted[dev] = true
		}
	}
	return c, nil
}
