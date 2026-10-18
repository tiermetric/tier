package issueref

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var storedTrackerRE = regexp.MustCompile(`^([A-Z][A-Z0-9]{0,15})-[0-9]+$`)

// Diagnostic suggests at most ten unconfigured prefixes; it never changes the grammar.
func (g Grammar) Diagnostic(issueCounts map[string]int) string {
	line := "tracker keys: none configured (OP built in)"
	if len(g.keys) > 0 {
		line = "tracker keys: " + strings.Join(g.keys, ",") + " (OP built in)"
	}
	counts := map[string]int{}
	for id, n := range issueCounts {
		if m := storedTrackerRE.FindStringSubmatch(id); m != nil && !g.accepts(m[1]) {
			counts[m[1]] += n
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 10 {
		keys = keys[:10]
	}
	suggestions := make([]string, 0, len(keys))
	for _, key := range keys {
		suggestions = append(suggestions, fmt.Sprintf("%s (%d)", key, counts[key]))
	}
	if len(suggestions) == 0 {
		suggestions = append(suggestions, "none")
	}
	return line + "; unconfigured prefixes (suggest only): " + strings.Join(suggestions, ", ")
}
