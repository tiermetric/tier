package main

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var event3OperationDocs = []string{
	"how-it-works.md", "technical-reference.md", "rubric.md", "privacy.md",
}

const event3EnableLink = "technical-reference.md#enable-event-3-scoring"
const event3CoverageLink = "technical-reference.md#event-3-coverage"

var event3Heading = regexp.MustCompile(`^(#{1,6}) `)
var event3Fence = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")

// Only ATX headings outside fenced code delimit documentation contracts.
func event3Section(body, heading string) string {
	lines := strings.Split(body, "\n")
	start, level := -1, 0
	fence := ""
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if match := event3Fence.FindStringSubmatch(line); match != nil {
			if fence == "" {
				fence = match[1]
			} else if match[1][0] == fence[0] && len(match[1]) >= len(fence) && strings.TrimSpace(match[2]) == "" {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		match := event3Heading.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if start >= 0 && len(match[1]) <= level {
			return strings.Join(lines[start:i], "\n")
		}
		if line == heading {
			start, level = i+1, len(match[1])
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n")
	}
	return ""
}

func TestEvent3Section_IgnoresNonHeadingsAndFencedCode(t *testing.T) {
	body := "```md\n### Target\nfalse section\n```\n" +
		"### Target\nkept\n#1266 is a PR reference\n" +
		"~~~md\n## Fenced heading\n~~~\nstill kept\n## End\nexcluded"
	got := event3Section(body, "### Target")
	for _, want := range []string{"kept", "#1266", "still kept"} {
		if !strings.Contains(got, want) {
			t.Errorf("section lost %q: %q", want, got)
		}
	}
	if strings.Contains(got, "false section") || strings.Contains(got, "excluded") {
		t.Errorf("section includes content outside its real headings: %q", got)
	}
	if got := event3Section("~~~\n### Target\nfalse section\n~~~", "### Target"); got != "" {
		t.Errorf("fenced heading created a section: %q", got)
	}
}

// Include the repository entry point as well as every served page.
func event3PublicDocPaths(t *testing.T) []string {
	t.Helper()
	files, err := collectDocs("../../docs")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"../../README.md"}
	for _, file := range files {
		paths = append(paths, file.srcPath)
	}
	return paths
}

// Match any subject, and normalize each paragraph so wrapping cannot hide a claim.
var event3Claim = regexp.MustCompile(`(?i)\baccounts?\b[^.!?]*\bcorrective\b`)
var event3OffByDefault = regexp.MustCompile(`(?i)off[ -]by[ -]default`)

func event3HasClaim(body string) bool {
	for _, paragraph := range strings.Split(body, "\n\n") {
		if event3Claim.MatchString(strings.Join(strings.Fields(paragraph), " ")) {
			return true
		}
	}
	return false
}

func event3ClaimProblems(body string) []string {
	if !event3HasClaim(body) {
		return nil
	}
	var problems []string
	for _, paragraph := range strings.Split(body, "\n\n") {
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		if event3Claim.MatchString(paragraph) && !event3OffByDefault.MatchString(paragraph) {
			problems = append(problems, "claim lacks default-off qualification: "+paragraph)
		}
	}
	capability := event3Section(body, "### Event 3 capability")
	if !event3HasClaim(capability) || !event3OffByDefault.MatchString(capability) {
		problems = append(problems, "capability section needs the claim and default-off qualification")
	}
	for _, marker := range []string{"Corrects: #N", "does not detect undeclared repairs"} {
		if !strings.Contains(strings.Join(strings.Fields(capability), " "), marker) {
			problems = append(problems, "capability section missing "+marker)
		}
	}
	if event3Section(body, "### Enable Event 3 scoring") == "" && !strings.Contains(body, event3EnableLink) {
		problems = append(problems, "claim needs the canonical enable section or a pointer to it")
	}
	return problems
}

func TestEvent3PublicClaim_RejectsWrappedUnqualifiedClaim(t *testing.T) {
	for _, body := range []string{
		"TIER accounts for confirmed\ncorrective work.",
		"This installation accounts\nfor confirmed corrective work.",
	} {
		if len(event3ClaimProblems(body)) == 0 {
			t.Errorf("unqualified claim escaped detection: %q", body)
		}
	}
	foundREADME := false
	for _, path := range event3PublicDocPaths(t) {
		foundREADME = foundREADME || filepath.Clean(path) == "../../README.md"
	}
	if !foundREADME {
		t.Error("public claim scan omits root README.md")
	}
}

func TestEvent3PublicClaim_RequiresEnabledScoringDocumentation(t *testing.T) {
	claims := 0
	for _, path := range event3PublicDocPaths(t) {
		body := readFile(t, path)
		if event3HasClaim(body) {
			claims++
		}
		for _, problem := range event3ClaimProblems(body) {
			t.Errorf("%s: %s", path, problem)
		}
	}
	if claims == 0 {
		t.Error("no published Event 3 capability section")
	}
	for _, name := range event3OperationDocs {
		body := readFile(t, "../../docs/"+name)
		if name == "technical-reference.md" {
			enable := event3Section(body, "### Enable Event 3 scoring")
			for _, marker := range []string{"--event3-scoring", "TIER_EVENT3_SCORING", "outcomes.event3_scoring", "restore the pre-upgrade", "do not run older binaries on"} {
				if !strings.Contains(enable, marker) {
					t.Errorf("%s: enable section missing %s", name, marker)
				}
			}
			continue
		}
		if !strings.Contains(body, event3EnableLink) || event3Section(body, "### Enable Event 3 scoring") != "" {
			t.Errorf("%s: require a pointer to canonical enable instructions, without a duplicate section", name)
		}
	}
}

var event3DeferredFeature = regexp.MustCompile(`(?i)\b(nominat\w*|history import\w*|historical backfill\w*)\b`)
var event3AvailableFeature = regexp.MustCompile(`(?i)\b(shipped|available|implemented|automatically|supports)\b`)
var event3Deferral = regexp.MustCompile(`(?i)\b(planned|future|unimplemented|after[ -]launch)\b|\b(?:not|never)\s+(?:(?:yet|currently)\s+)?(?:shipped|available|implemented|supported)\b|\bno\s+(?:automatic\s+)?nominat\w*`)
var event3SentenceEnd = regexp.MustCompile(`[.!?](?:\s+|$)`)

func event3UndeferredFeatures(body string) []string {
	var problems []string
	for _, paragraph := range strings.Split(body, "\n\n") {
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		for _, sentence := range event3SentenceEnd.Split(paragraph, -1) {
			if event3DeferredFeature.MatchString(sentence) && event3AvailableFeature.MatchString(sentence) && !event3Deferral.MatchString(sentence) {
				problems = append(problems, sentence)
			}
		}
	}
	return problems
}

func TestEvent3Docs_DeferralMustQualifyTheSameSentence(t *testing.T) {
	for _, body := range []string{
		"Nomination is available, not optional.",
		"A future change is planned. Automatic nominations are available.",
		"History import is implemented. Other features are not shipped.",
		"Historical backfills are available. Nomination is planned, not shipped.",
		"TIER automatically nominates repairs.",
	} {
		if len(event3UndeferredFeatures(body)) == 0 {
			t.Errorf("unsupported feature escaped guard: %q", body)
		}
	}
	if got := event3UndeferredFeatures("Automatic nomination is planned, not shipped."); len(got) != 0 {
		t.Errorf("deferred feature rejected: %v", got)
	}
}

func TestEvent3Docs_DoNotClaimNominationOrHistoricalCoverageBeforeShipping(t *testing.T) {
	for _, name := range event3OperationDocs {
		body := readFile(t, "../../docs/"+name)
		coverage := event3Section(body, "### Event 3 coverage")
		if name != "technical-reference.md" {
			if !strings.Contains(body, event3CoverageLink) || coverage != "" {
				t.Errorf("%s: require a pointer to canonical coverage, without a duplicate section", name)
			}
			continue
		}
		available := strings.Join(strings.Fields(event3Section(coverage, "#### Available")), " ")
		for _, marker := range []string{"Corrects:", "tierd backfill", "backfill:fetch-time", "not as it was at merge", "backfilled pairs can change live points", "not_established"} {
			if !strings.Contains(available, marker) {
				t.Errorf("%s: available coverage missing %s", name, marker)
			}
		}
		deferred := event3Section(coverage, "#### Not shipped")
		if !event3DeferredFeature.MatchString(deferred) || !event3Deferral.MatchString(deferred) {
			t.Errorf("%s: automatic nomination must be deferred", name)
		}
	}
	for _, path := range event3PublicDocPaths(t) {
		for _, sentence := range event3UndeferredFeatures(readFile(t, path)) {
			t.Errorf("%s: nomination/history capability lacks a deferred qualification in the same sentence: %s", path, sentence)
		}
	}
}

func TestEvent3PublicDocs_UsePlainDescriptions(t *testing.T) {
	jargon := regexp.MustCompile(`(?i)\b(?:U1|U2|U3|U4|U5|S3)\b|\bPR 21|\bplan PRs? \d|launch claim|confirmed-work claim`)
	for _, path := range event3PublicDocPaths(t) {
		if match := jargon.FindString(readFile(t, path)); match != "" {
			t.Errorf("%s: replace internal jargon %q with a public description", path, match)
		}
	}
}
