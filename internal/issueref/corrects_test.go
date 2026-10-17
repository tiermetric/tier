package issueref

import (
	"reflect"
	"strings"
	"testing"
)

func TestFromPRBody_CorrectsNeverAttributesAnIssue(t *testing.T) {
	for _, line := range []string{
		"Corrects: #99", "Corrects: #99, #7", "Corrects: none",
		"cOrReCtS: #99", " \tCorrects \t: #99\r",
		"Corrects: none, #99", "Corrects: #0, #99", "Corrects: #-1, #99",
		"Corrects: owner/repo#99", "Corrects: #99, closes #88, PROJ-9",
		"Corrects: closes #88", "Corrects: PROJ-9", "Corrects: #１２, #99",
		"Corrects: " + strings.Repeat("x", 70<<10) + " #99 closes #88 PROJ-9",
	} {
		// Avoid enormous test names while retaining the full adversarial input.
		name := line
		if len(name) > 80 {
			name = "oversized malformed declaration"
		}
		t.Run(name, func(t *testing.T) {
			for _, suffix := range []struct{ text, want string }{
				{"", ""}, {"\ncloses #42", "issue-42"},
				{"\ntracked in PROJ-42", "PROJ-42"}, {"\nsee #42", "issue-42"},
			} {
				if got := FromPRBody(line + suffix.text); got != suffix.want {
					t.Errorf("FromPRBody with suffix %q = %q, want %q", suffix.text, got, suffix.want)
				}
			}
		})
	}
	if got := FromPRBody("Corrects: #99\nCorrects: #7\ncloses #42"); got != "issue-42" {
		t.Errorf("contradictory declarations: %q, want issue-42", got)
	}
}

func TestClosedIssues_IgnoresCorrectsLines(t *testing.T) {
	for _, line := range []string{
		"Corrects: #99", "Corrects: none", "CORRECTS: closes #99",
		" \tCorrects : #7, fixes #99\r", "Corrects: none, resolves #99",
		"Corrects: " + strings.Repeat("x", 70<<10) + " closes #99",
	} {
		if got := ClosedIssues(line); len(got) != 0 {
			t.Errorf("declaration of length %d closes %v, want none", len(line), got)
		}
		body := "closes #42, #3\r\n" + line + "\nfixes #5, #42"
		if got, want := ClosedIssues(body), []string{"issue-42", "issue-3", "issue-5"}; !reflect.DeepEqual(got, want) {
			t.Errorf("ClosedIssues around declaration = %v, want %v", got, want)
		}
	}
}

func TestFromPRBody_CorrectsContainerFence(t *testing.T) {
	body := "- ```\n  Corrects: #99\n  ```\n\nCorrects: none"
	if got := FromPRBody(body); got != "" {
		t.Errorf("FromPRBody = %q, want no attribution", got)
	}
	if got := FromPRBody(body + "\nsee #42"); got != "issue-42" {
		t.Errorf("FromPRBody with real reference = %q, want issue-42", got)
	}
}

func TestFromPRBody_CorrectsReferenceBeforeLaterColon(t *testing.T) {
	body := "Corrects #7: note"
	if got := FromPRBody(body); got != "" {
		t.Errorf("FromPRBody = %q, want no attribution", got)
	}
	if got := FromPRBody(body + "\nsee #42"); got != "issue-42" {
		t.Errorf("FromPRBody with real reference = %q, want issue-42", got)
	}
	if got := ClosedIssues("Corrects #7: closes #8"); len(got) != 0 {
		t.Errorf("ClosedIssues = %v, want none", got)
	}
}
