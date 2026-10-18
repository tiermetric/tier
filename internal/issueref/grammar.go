package issueref

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tiermetric/tier/internal/corrects"
)

// Grammar is immutable after construction. Its zero value accepts only built-in OP.
type Grammar struct {
	keys  []string
	keyRE *regexp.Regexp
}

var (
	hiddenHTMLRE      = regexp.MustCompile(`(?is)<!--.*?(?:-->|$)|<pre(?:\s[^>]*)?>.*?(?:</pre\s*>|$)`)
	listFencePrefixRE = regexp.MustCompile(`^(?:[-+*]|[0-9]+[.)])\s+`)
	trackerPrefixRE   = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,15}$`)
	branchKeyRE       = regexp.MustCompile(`^([A-Z][A-Z0-9]{0,15})-(0*[1-9][0-9]*)(?:[-_./]|$)`)
	branchNumberRE    = regexp.MustCompile(`^([1-9][0-9]*)(?:[-_.]|$)`)
	branchDateRE      = regexp.MustCompile(`^(?:[0-9]{4}-[0-9]{2}(?:-[0-9]{2})?|[0-9]{8})(?:[-_.]|$)`)
	builtInKeyRE      = regexp.MustCompile(`\bOP-0*[1-9][0-9]*\b`)
	structuredKeyRE   = regexp.MustCompile(`(?:(?i:\b(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?))(?:\s+|:\s*)|(?i:\b(?:refs|references|part of|relates to|tracks|implements))\s+)([A-Z][A-Z0-9]{0,15})-(0*[1-9][0-9]*)\b`)
	keyTrailerRE      = regexp.MustCompile(`(?m)^(?i:Issue|Issues|Ref|Refs|References|Ticket|Tracker|Jira|OpenProject)\s*:\s*([A-Z][A-Z0-9]{0,15})-(0*[1-9][0-9]*)\b`)
	relationRE        = regexp.MustCompile(`(?i)\b(?:refs|references|part of|relates to|tracks|implements|half\s+of)\s+#([1-9][0-9]*)` + issueTerminator)
	relationTrailerRE = regexp.MustCompile(`(?im)^(?:Issue|Refs?|References)\s*:\s*#([1-9][0-9]*)` + issueTerminator)
)

// NewGrammar rejects every malformed entry; empty means no configured prefixes.
func NewGrammar(value string) (Grammar, error) {
	var g Grammar
	if value == "" {
		return g, nil
	}
	for _, key := range strings.Split(value, ",") {
		key = strings.TrimSpace(key)
		if !trackerPrefixRE.MatchString(key) {
			return Grammar{}, fmt.Errorf("invalid tracker key %q (expected ^[A-Z][A-Z0-9]{0,15}$)", key)
		}
		if !slices.Contains(g.keys, key) {
			g.keys = append(g.keys, key)
		}
	}
	keys := append(g.Keys(), "OP")
	g.keyRE = regexp.MustCompile(`\b(?:` + strings.Join(keys, "|") + `)-0*[1-9][0-9]*\b`)
	return g, nil
}

// Keys returns a copy of the configured prefixes, excluding implicit OP.
func (g Grammar) Keys() []string          { return slices.Clone(g.keys) }
func (g Grammar) accepts(key string) bool { return key == "OP" || slices.Contains(g.keys, key) }
func (g Grammar) matcher() *regexp.Regexp {
	if g.keyRE != nil {
		return g.keyRE
	}
	return builtInKeyRE
}

// IsRevertBranch identifies branches whose spend cannot supply issue evidence.
func IsRevertBranch(branch string) bool {
	branch = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"))
	return strings.HasPrefix(branch, "revert-") || strings.HasPrefix(branch, "revert/")
}

func (g Grammar) FromBranch(branch string) string { return g.FromBranchRef(branch).ID }

// FromBranchRef consults only the first slot; rejected slots have no numeric fallback.
func (g Grammar) FromBranchRef(branch string) Ref {
	branch = strings.TrimSpace(branch)
	if IsExploratoryBranch(branch) {
		return Ref{}
	}
	parts := strings.Split(branch, "/")
	switch strings.ToLower(parts[0]) {
	case "release", "releases", "revert", "dependabot", "renovate", "backup", "archive", "gh-pages":
		return Ref{}
	}
	if IsRevertBranch(branch) {
		return Ref{}
	}
	slot := parts[0]
	if len(parts) > 1 {
		slot = parts[1]
	}
	if m := branchKeyRE.FindStringSubmatch(slot); m != nil && g.accepts(m[1]) {
		return Ref{ID: m[1] + "-" + m[2], Rule: RuleBranchConfiguredKey}
	}
	if branchDateRE.MatchString(slot) {
		return Ref{}
	}
	if m := branchNumberRE.FindStringSubmatch(slot); m != nil && !isLikelyYear(m[1]) {
		return Ref{ID: "issue-" + m[1], Rule: RuleBranchFirstSlot}
	}
	return Ref{}
}

func (g Grammar) FromPRBody(body string) string { return g.FromPRBodyRef(body).ID }

// FromPRBodyRef selects the first match in the highest-confidence class.
func (g Grammar) FromPRBodyRef(body string) Ref {
	body = attributionText(body)
	if m := closesRE.FindStringSubmatch(body); m != nil {
		return Ref{ID: "issue-" + m[1], Rule: RuleBodyCloseDirective}
	}
	best, keyID := len(body)+1, ""
	for _, re := range []*regexp.Regexp{structuredKeyRE, keyTrailerRE} {
		for _, loc := range re.FindAllStringSubmatchIndex(body, -1) {
			key := body[loc[2]:loc[3]]
			if loc[0] < best && g.accepts(key) {
				best, keyID = loc[0], key+"-"+body[loc[4]:loc[5]]
			}
		}
	}
	if keyID != "" {
		return Ref{ID: keyID, Rule: RuleBodyConfiguredKey}
	}
	best, id := len(body)+1, ""
	for _, re := range []*regexp.Regexp{relationRE, relationTrailerRE} {
		if loc := re.FindStringSubmatchIndex(body); loc != nil && loc[0] < best {
			best, id = loc[0], body[loc[2]:loc[3]]
		}
	}
	if id != "" {
		return Ref{ID: "issue-" + id, Rule: RuleBodyRelationRef}
	}
	return Ref{}
}

func (g Grammar) FromCommitSubject(subject string) string { return g.FromCommitSubjectRef(subject).ID }

// FromCommitSubjectRef never interprets relation references as issue evidence.
func (g Grammar) FromCommitSubjectRef(subject string) Ref {
	subject = attributionText(subject)
	if ids := ClosedIssues(subject); len(ids) > 0 {
		return Ref{ID: ids[0], Rule: RuleCommitCloseDirective}
	}
	if id := g.matcher().FindString(subject); id != "" {
		return Ref{ID: id, Rule: RuleCommitConfiguredKey}
	}
	return Ref{}
}

func (g Grammar) FromBranchOrBody(branch, body string) string {
	return g.FromBranchOrBodyRef(branch, body).ID
}
func (g Grammar) FromBranchOrBodyRef(branch, body string) Ref {
	if ref := g.FromBranchRef(branch); ref.ID != "" {
		return ref
	}
	return g.FromPRBodyRef(body)
}

// attributionText leaves barriers where examples were removed so tokens cannot join across them.
func attributionText(body string) string {
	// Attribution work is bounded by the same UTF-8 body limit as Corrects parsing.
	if len(body) > corrects.MaxBodyBytes {
		return "\x00"
	}
	body = corrects.StripDeclarationLines(body)
	body = hiddenHTMLRE.ReplaceAllString(body, " \x00 ")
	lines := strings.Split(body, "\n")
	var fence byte
	var fenceLen int
	var listIndents []int
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if fence != 0 {
			lines[i] = "\x00"
			n := 0
			for n < len(trim) && trim[n] == fence {
				n++
			}
			if n >= fenceLen && strings.TrimSpace(trim[n:]) == "" {
				fence = 0
			}
			continue
		}
		indent := 0
	indentLoop:
		for _, ch := range line {
			switch ch {
			case ' ':
				indent++
			case '\t':
				indent += 4 - indent%4
			default:
				break indentLoop
			}
		}
		for trim != "" && len(listIndents) > 0 && indent < listIndents[len(listIndents)-1] {
			listIndents = listIndents[:len(listIndents)-1]
		}
		listIndent := 0
		if len(listIndents) > 0 {
			listIndent = listIndents[len(listIndents)-1]
		}
		if marker := listFencePrefixRE.FindStringIndex(trim); marker != nil && indent < listIndent+4 {
			listIndent = indent + marker[1]
			listIndents = append(listIndents, listIndent)
		}
		if indent >= listIndent+4 || strings.HasPrefix(trim, ">") {
			lines[i] = "\x00"
			continue
		}
		fenceText := listFencePrefixRE.ReplaceAllString(trim, "")
		if strings.HasPrefix(fenceText, "```") || strings.HasPrefix(fenceText, "~~~") {
			trim = fenceText
			fence, fenceLen = trim[0], 0
			for fenceLen < len(trim) && trim[fenceLen] == fence {
				fenceLen++
			}
			lines[i] = "\x00"
		}
	}
	body = strings.Join(lines, "\n")
	// Each run is indexed once; matching an opener never rescans the suffix.
	type run struct{ start, end int }
	var runs []run
	for i := 0; i < len(body); {
		if body[i] != '`' {
			i++
			continue
		}
		start := i
		for i < len(body) && body[i] == '`' {
			i++
		}
		runs = append(runs, run{start, i})
	}
	next := make([]int, len(runs))
	byLength := make(map[int]int)
	for i := len(runs) - 1; i >= 0; i-- {
		next[i] = -1
		n := runs[i].end - runs[i].start
		if j, ok := byLength[n]; ok {
			next[i] = j
		}
		byLength[n] = i
	}
	var out strings.Builder
	start := 0
	for i := 0; i < len(runs); i++ {
		if j := next[i]; j >= 0 {
			out.WriteString(body[start:runs[i].start])
			out.WriteString(" \x00 ")
			start = runs[j].end
			i = j
		}
	}
	out.WriteString(body[start:])
	return stripReferenceEmphasis(out.String())
}

var referenceEmphasisRE = regexp.MustCompile(`(\*{1,2}|_{1,2})(#[1-9][0-9]*|[A-Z][A-Z0-9]{0,15}-0*[1-9][0-9]*)(\*{1,2}|_{1,2})`)

// Only balanced emphasis immediately surrounding a reference token is removed.
func stripReferenceEmphasis(body string) string {
	var out strings.Builder
	start := 0
	for _, m := range referenceEmphasisRE.FindAllStringSubmatchIndex(body, -1) {
		if body[m[2]:m[3]] != body[m[6]:m[7]] ||
			(m[0] > 0 && strings.ContainsRune("*_", rune(body[m[0]-1]))) ||
			(m[1] < len(body) && strings.ContainsRune("*_", rune(body[m[1]]))) {
			continue
		}
		out.WriteString(body[start:m[0]])
		out.WriteString(body[m[4]:m[5]])
		start = m[1]
	}
	out.WriteString(body[start:])
	return out.String()
}
