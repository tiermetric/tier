package issueref

import (
	"github.com/tiermetric/tier/internal/corrects"
	"strings"
	"testing"
	"time"
)

func TestStrictBranchExamples(t *testing.T) {
	for _, tc := range []struct {
		branch, keys, id string
		rule             Rule
	}{
		{"feature/42-auth", "", "issue-42", RuleBranchFirstSlot}, {"fix/42", "", "issue-42", RuleBranchFirstSlot},
		{"fix/TIER-99-crash", "TIER", "TIER-99", RuleBranchConfiguredKey}, {"fix/TIER-99-crash", "", "", ""},
		{"JIRA-456-x", "JIRA", "JIRA-456", RuleBranchConfiguredKey},
		{"OP-2024-x", "", "OP-2024", RuleBranchConfiguredKey}, {"feature/OP-007", "", "OP-007", RuleBranchConfiguredKey}, {"feature/OP-2024-x", "", "OP-2024", RuleBranchConfiguredKey},
		{"42_auth", "", "issue-42", RuleBranchFirstSlot}, {"feature/42.foo/later-99", "", "issue-42", RuleBranchFirstSlot},
		{"feature/1899", "", "issue-1899", RuleBranchFirstSlot}, {"feature/2100", "", "issue-2100", RuleBranchFirstSlot},
		{"fix/A-1", "A", "A-1", RuleBranchConfiguredKey},
	} {
		t.Run(tc.branch+"/"+tc.keys, func(t *testing.T) {
			g, err := NewGrammar(tc.keys)
			if err != nil {
				t.Fatal(err)
			}
			want := Ref{tc.id, tc.rule}
			if got := g.FromBranchRef(tc.branch); got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
			if g.FromBranch(tc.branch) != want.ID {
				t.Fatal("ID mismatch")
			}
		})
	}
	for _, branch := range []string{"", "main", "master", "HEAD", "docs/notes-2026-10-14", "docs/notes-2026-07-23", "fix/audit-s09-6-x", "fix/435b-auth-50051-cutover", "dependabot/docker/node-26-alpine", "revert-45-fix/123-x", "release/2024-42", "release/2024-hotfix-42", "feature/007-42", "claude/sweet-hopper-3", "fix/utf-8-handling", "chore/node-22-upgrade", "chore/bump-go-1-27", "feature/1900-x/42", "feature/2099-x", "feature/1234-01-42", "feature/12340101-x", "feature/topic/42", "feature_42", "feature/tier-42", "feature/OP-0", "feature/0", "feature/42b"} {
		t.Run(branch, func(t *testing.T) {
			if got := (Grammar{}).FromBranchRef(branch); got != (Ref{}) {
				t.Fatal(got)
			}
		})
	}
	for _, prefix := range []string{"release", "releases", "revert", "dependabot", "renovate", "backup", "archive", "gh-pages"} {
		for _, p := range []string{prefix, strings.ToUpper(prefix)} {
			if got := (Grammar{}).FromBranchRef(p + "/42"); got != (Ref{}) {
				t.Fatal(p, got)
			}
		}
	}
}

func TestStrictBodyAndCommitExamples(t *testing.T) {
	g, err := NewGrammar("PROJ,TIER")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text         string
		g            Grammar
		body, commit Ref
	}{
		{"Fixes an error in PR #446", g, Ref{}, Ref{}}, {"Use UTF-8", g, Ref{}, Ref{}},
		{"Follow-up to #733", g, Ref{}, Ref{}}, {"see #42 for context", g, Ref{}, Ref{}},
		{"Implements PROJ-123", Grammar{}, Ref{}, Ref{}},
		{"closes #42", g, Ref{"issue-42", RuleBodyCloseDirective}, Ref{"issue-42", RuleCommitCloseDirective}},
		{"closes #42`example`", g, Ref{"issue-42", RuleBodyCloseDirective}, Ref{"issue-42", RuleCommitCloseDirective}},
		{"refs #42", g, Ref{"issue-42", RuleBodyRelationRef}, Ref{}}, {"Corrects: #733", g, Ref{}, Ref{}},
		{"Implements PROJ-123", g, Ref{"PROJ-123", RuleBodyConfiguredKey}, Ref{"PROJ-123", RuleCommitConfiguredKey}},
		{"OP-2024", Grammar{}, Ref{}, Ref{"OP-2024", RuleCommitConfiguredKey}},
		{"Issue: OP-2024", Grammar{}, Ref{"OP-2024", RuleBodyConfiguredKey}, Ref{"OP-2024", RuleCommitConfiguredKey}},
		{"PROJ-9 refs #42 closes #7", g, Ref{"issue-7", RuleBodyCloseDirective}, Ref{"issue-7", RuleCommitCloseDirective}},
		{"refs #42\nIssue: PROJ-9", g, Ref{"PROJ-9", RuleBodyConfiguredKey}, Ref{"PROJ-9", RuleCommitConfiguredKey}},
		{"Issue: #7\nrefs #42", g, Ref{"issue-7", RuleBodyRelationRef}, Ref{}},
		{"refs #42\nIssue: #7", g, Ref{"issue-42", RuleBodyRelationRef}, Ref{}},
		{"refs PROJ-9 implements TIER-7", g, Ref{"PROJ-9", RuleBodyConfiguredKey}, Ref{"PROJ-9", RuleCommitConfiguredKey}},
		{"discloses #42", g, Ref{}, Ref{}}, {"prefrefs #42", g, Ref{}, Ref{}},
		{"PROJ-9 in prose", g, Ref{}, Ref{"PROJ-9", RuleCommitConfiguredKey}},
		{"XPROJ-9", g, Ref{}, Ref{}},
		{"implementsPROJ-9", g, Ref{}, Ref{}},
		{"refsOP-9", g, Ref{}, Ref{}},
		{"implements: PROJ-9", g, Ref{}, Ref{"PROJ-9", RuleCommitConfiguredKey}},
		{"closes:PROJ-9", g, Ref{"PROJ-9", RuleBodyConfiguredKey}, Ref{"PROJ-9", RuleCommitConfiguredKey}}, {"refs PROJ-09", g, Ref{"PROJ-09", RuleBodyConfiguredKey}, Ref{"PROJ-09", RuleCommitConfiguredKey}},
		{"```\ncloses #42\n```\nrefs #7", g, Ref{"issue-7", RuleBodyRelationRef}, Ref{}},
		{"~~~go\nrefs PROJ-9\n~~~", g, Ref{}, Ref{}},
		{"   > closes #42\n> refs PROJ-9", g, Ref{}, Ref{}},
		{"`closes #42` and ``PROJ-9``", g, Ref{}, Ref{}},
		{"closes `example` #42", g, Ref{}, Ref{}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := tc.g.FromPRBodyRef(tc.text); got != tc.body {
				t.Errorf("body %+v want %+v", got, tc.body)
			}
			if got := tc.g.FromCommitSubjectRef(tc.text); got != tc.commit {
				t.Errorf("commit %+v want %+v", got, tc.commit)
			}
		})
	}
	for _, token := range []string{"UTF-8", "SHA-256", "HTTP-2", "CVE-2026-1234", "GPT-5", "ISO-8601", "RFC-6749"} {
		if got := (Grammar{}).FromPRBodyRef("implements " + token); got != (Ref{}) {
			t.Fatal(got)
		}
	}
	for _, verb := range []string{"refs", "references", "part of", "relates to", "tracks", "implements"} {
		if got := g.FromPRBodyRef(strings.ToUpper(verb) + " #42"); got != (Ref{"issue-42", RuleBodyRelationRef}) {
			t.Fatal(verb, got)
		}
		if got := g.FromPRBodyRef(verb + " PROJ-9"); got != (Ref{"PROJ-9", RuleBodyConfiguredKey}) {
			t.Fatal(verb, got)
		}
	}
	for _, trailer := range []string{"Issue", "Issues", "Ref", "Refs", "References", "Ticket", "Tracker", "Jira", "OpenProject"} {
		if got := g.FromPRBodyRef(trailer + ": PROJ-9"); got != (Ref{"PROJ-9", RuleBodyConfiguredKey}) {
			t.Fatal(trailer, got)
		}
	}
	for _, trailer := range []string{"Issue", "Ref", "Refs", "References"} {
		if got := g.FromPRBodyRef(trailer + ": #42"); got != (Ref{"issue-42", RuleBodyRelationRef}) {
			t.Fatal(trailer, got)
		}
	}
}

func TestGrammarInvalidKeys(t *testing.T) {
	for _, key := range []string{"lower", "A-B", "_A", "1A", "ABCDEFGHIJKLMNOPQ", "A B", "A,", "A,,B", "A\nB", "Å"} {
		if _, err := NewGrammar(key); err == nil || !strings.Contains(err.Error(), "invalid tracker key") {
			t.Fatal(key, err)
		}
	}
	g, err := NewGrammar("A,Z9,ABCDEFGHIJKLMNOP,OP,A")
	if err != nil {
		t.Fatal(err)
	}
	keys := g.Keys()
	keys[0] = "MUTATED"
	if g.Keys()[0] != "A" {
		t.Fatal("mutable grammar")
	}
}

func TestTrackerDiagnosticTopTen(t *testing.T) {
	g, err := NewGrammar("PROJ")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{"OP-42": 99, "PROJ-7": 99, "issue-42": 99, "AA-01": 2, "AA-2": 3, "BB-1": 5, "UTF-8": 7, "CVE-2026-1234": 100, "aA-1": 100, "A-1": 100}
	for _, key := range []string{"CC", "DD", "EE", "FF", "GG", "HH", "II", "JJ", "KK", "LL"} {
		counts[key+"-1"] = 1
	}
	got := g.Diagnostic(counts)
	want := "tracker keys: PROJ (OP built in); unconfigured prefixes (suggest only): A (100), UTF (7), AA (5), BB (5), CC (1), DD (1), EE (1), FF (1), GG (1), HH (1)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := g.FromCommitSubject("use UTF-8"); got != "" {
		t.Fatal("suggestion enabled a prefix", got)
	}
	if got := (Grammar{}).Diagnostic(nil); got != "tracker keys: none configured (OP built in); unconfigured prefixes (suggest only): none" {
		t.Fatal(got)
	}
}

func TestAttributionCodeBarriers(t *testing.T) {
	for _, text := range []string{"- ```\n  closes #42\n  ```", "1. ~~~\n   closes #42\n   ~~~", "closes\n```\nexample\n```\n#42", "refs `example` #42", "``closes #42``", "`````\ncloses #42\n```\nfixes #7\n`````", "Corrects: #733\n> closes #42", "\u00a0> closes #42"} {
		if got := (Grammar{}).FromPRBodyRef(text); got != (Ref{}) {
			t.Fatal(text, got)
		}
	}
}

func TestAttributionUnrenderedText(t *testing.T) {
	g, err := NewGrammar("PROJ")
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{
		"<!--\nPR template: Closes #123\n-->",
		"<!-- Closes #123 -->",
		"    Closes #123\n    PROJ-9",
		"    - Closes #123", "\t1. Closes #123",
		"\tCloses #123",
		"<pre>Closes #123\nPROJ-9</pre>",
		"<PRE class=\"example\">Closes #123</PRE>",
		"<!-- Closes #123", "<pre>Closes #123",
	} {
		t.Run(hidden, func(t *testing.T) {
			if got := g.FromPRBodyRef(hidden); got != (Ref{}) {
				t.Fatal(got)
			}
			if got := g.FromCommitSubjectRef(hidden); got != (Ref{}) {
				t.Fatal(got)
			}
			if got := g.FromPRBody(hidden + "\n\nrefs #42"); !strings.Contains(hidden, "-->") && strings.HasPrefix(hidden, "<!--") || !strings.Contains(strings.ToLower(hidden), "</pre>") && strings.HasPrefix(hidden, "<pre>") {
				if got != "" {
					t.Fatal(got)
				}
			} else if got != "issue-42" {
				t.Fatal(got)
			}
		})
	}
	if got := g.FromPRBody("- Explanation\n    refs #42"); got != "issue-42" {
		t.Fatal("list continuation", got)
	}
	for _, text := range []string{"Closes <!-- hidden -->#123", "Closes <pre>hidden</pre>#123"} {
		if got := g.FromPRBody(text); got != "" {
			t.Fatal(got)
		}
	}
}

func TestAttributionAdversarialInputBound(t *testing.T) {
	var body strings.Builder
	for n := 1; body.Len() < 4<<20; n++ {
		body.WriteString(strings.Repeat("`", n))
		body.WriteString(" x ")
	}
	input := body.String() + " closes #123"
	start := time.Now()
	if got := attributionText(input); got != "\x00" {
		t.Fatal("oversized attribution was accepted")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("4 MiB attribution took %s (limit 1s)", elapsed)
	}
	// Distinct unmatched runs within the cap also have bounded work.
	start = time.Now()
	_ = attributionText(input[:corrects.MaxBodyBytes])
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("capped unmatched runs took %s (limit 1s)", elapsed)
	}
	if got := FromPRBody(strings.Repeat("x", corrects.MaxBodyBytes) + "\ncloses #123"); got != "" {
		t.Fatal(got)
	}
}

func TestTrackerDiagnosticPrefixLength(t *testing.T) {
	got := (Grammar{}).Diagnostic(map[string]int{"A-1": 1, "ABCDEFGHIJKLMNOP-2": 1, "ABCDEFGHIJKLMNOPQ-3": 1})
	if !strings.Contains(got, "A (1)") || !strings.Contains(got, "ABCDEFGHIJKLMNOP (1)") || strings.Contains(got, "ABCDEFGHIJKLMNOPQ") {
		t.Fatal(got)
	}
}

func TestGrammarTrimsCommaSeparatedKeys(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{" TIER, JIRA ", "TIER,JIRA"}, {" A", "A"}} {
		g, err := NewGrammar(tc.input)
		if err != nil || strings.Join(g.Keys(), ",") != tc.want {
			t.Fatalf("input=%q keys=%v err=%v", tc.input, g.Keys(), err)
		}
	}
	for _, input := range []string{"TIER,,JIRA", "TIER, ", " "} {
		if _, err := NewGrammar(input); err == nil {
			t.Fatalf("accepted empty entry in %q", input)
		}
	}
}

func TestGrammarZeroPaddedTrackerNumbers(t *testing.T) {
	g, err := NewGrammar("ACME")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ACME-006", "OP-0042"} {
		t.Run(id, func(t *testing.T) {
			grammar := g
			if strings.HasPrefix(id, "OP-") {
				grammar = Grammar{}
			}
			for _, tc := range []struct {
				name      string
				got, want Ref
			}{
				{"branch-slot", grammar.FromBranchRef("fix/" + id + "-x"), Ref{id, RuleBranchConfiguredKey}},
				{"body-relation", grammar.FromPRBodyRef("Implements " + id), Ref{id, RuleBodyConfiguredKey}},
				{"body-trailer", grammar.FromPRBodyRef("Issue: " + id), Ref{id, RuleBodyConfiguredKey}},
				{"commit-subject", grammar.FromCommitSubjectRef("fix: " + id), Ref{id, RuleCommitConfiguredKey}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.got != tc.want {
						t.Fatalf("got %+v want %+v", tc.got, tc.want)
					}
				})
			}
		})
	}
	for _, tc := range []struct {
		name string
		got  Ref
	}{
		{"branch-numeric", g.FromBranchRef("007-x")},
		{"body-hash", g.FromPRBodyRef("closes #007")},
		{"commit-hash", g.FromCommitSubjectRef("closes #007")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != (Ref{}) {
				t.Fatal(tc.got)
			}
		})
	}
}

func TestGrammarHalfOfRelation(t *testing.T) {
	for _, tc := range []struct{ text, id string }{
		{"the GET half of #1944", "issue-1944"},
		{"second half of #829", "issue-829"},
		{"HALF\tOF #42", "issue-42"},
		{"behalf of #5", ""},
	} {
		t.Run(tc.text, func(t *testing.T) {
			want := Ref{}
			if tc.id != "" {
				want = Ref{tc.id, RuleBodyRelationRef}
			}
			if got := (Grammar{}).FromPRBodyRef(tc.text); got != want {
				t.Fatalf("body %+v want %+v", got, want)
			}
			if got := (Grammar{}).FromCommitSubjectRef(tc.text); got != (Ref{}) {
				t.Fatalf("commit %+v", got)
			}
		})
	}
}

func TestGrammarReferenceEmphasis(t *testing.T) {
	g, err := NewGrammar("ACME")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text   string
		body, commit Ref
	}{
		{"relation-key-bold", "Implements **ACME-006**", Ref{"ACME-006", RuleBodyConfiguredKey}, Ref{"ACME-006", RuleCommitConfiguredKey}},
		{"close-bold", "closes **#42**", Ref{"issue-42", RuleBodyCloseDirective}, Ref{"issue-42", RuleCommitCloseDirective}},
		{"close-italic", "Closes *#42*", Ref{"issue-42", RuleBodyCloseDirective}, Ref{"issue-42", RuleCommitCloseDirective}},
		{"relation-underscore", "refs __#7__", Ref{"issue-7", RuleBodyRelationRef}, Ref{}},
		{"emphasized-verb-still-does-not-attribute", "**closes** #42", Ref{}, Ref{}},
		{"unbalanced", "closes **#42*", Ref{}, Ref{}},
		{"mismatched", "closes **#42__", Ref{}, Ref{}},
		{"non-token", "closes **#42abc**", Ref{}, Ref{}},
		{"suffix-stays-invalid", "closes **#42**abc", Ref{}, Ref{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.FromPRBodyRef(tc.text); got != tc.body {
				t.Fatalf("body %+v want %+v", got, tc.body)
			}
			if got := g.FromCommitSubjectRef(tc.text); got != tc.commit {
				t.Fatalf("commit %+v want %+v", got, tc.commit)
			}
		})
	}
}

func TestGrammarListIndentedCode(t *testing.T) {
	for _, tc := range []struct{ name, text, id string }{
		{"list-code-stripped", "- Example:\n\n      closes #42\n\nRefs: #7", "issue-7"},
		{"list-continuation-attributable", "- Item\n  closes #42", "issue-42"},
		{"nested-code-stripped", "- Outer\n  - Example:\n\n        closes #42\n\nRefs: #7", "issue-7"},
		{"nested-continuation-attributable", "- Outer\n  - Item\n    closes #42", "issue-42"},
		{"parent-code-after-nested-list-stripped", "- Outer\n  - Item\n  Parent\n\n      closes #42\n\nRefs: #7", "issue-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Grammar{}).FromPRBody(tc.text); got != tc.id {
				t.Fatalf("got %q want %q", got, tc.id)
			}
		})
	}
}
