package corrects

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseCorrects_MultipleParentsAndNone(t *testing.T) {
	cases := []struct {
		name, body string
		want       Declaration
	}{
		{"missing", "closes #42", Declaration{State: StateMissing}},
		{"empty", "", Declaration{State: StateMissing}},
		{"none", "Corrects: none", Declaration{State: StateNone}},
		{"repeated none", "Corrects: none\nCorrects: none", Declaration{State: StateNone}},
		{"single", "Corrects: #42", Declaration{State: StateParents, Parents: []int{42}}},
		{"ordered dedup", "Corrects: #99, #7, #99, #42", Declaration{State: StateParents, Parents: []int{99, 7, 42}}},
		{"case and whitespace", " \tcOrReCtS \t: \t#7,\t#3 \t", Declaration{State: StateParents, Parents: []int{7, 3}}},
		{"equivalent lines", "Corrects: #9, #2\nCorrects: #2, #9, #2", Declaration{State: StateParents, Parents: []int{9, 2}}},
		{"CRLF", "description\r\nCORRECTS: #8, #3\r\ncloses #42\r\n", Declaration{State: StateParents, Parents: []int{8, 3}}},
		{"inline prose is not a key", "This Corrects: #3", Declaration{State: StateMissing}},
		{"maximum int", "Corrects: #" + strconv.Itoa(int(^uint(0)>>1)), Declaration{State: StateParents, Parents: []int{int(^uint(0) >> 1)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Parse(c.body); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Parse = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseCorrects_Guards(t *testing.T) {
	for _, c := range []struct {
		name, body string
		state      State
	}{
		{"missing_colon", "Corrects", StateMissing},
		{"lone_hash", "Corrects: #", StateInvalid},
		{"trailing_lone_hash", "Corrects: #7, #", StateInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Parse(c.body); !reflect.DeepEqual(got, Declaration{State: c.state}) {
				t.Errorf("Parse(%q) = %+v, want %s with nil parents", c.body, got, c.state)
			}
		})
	}
}

func TestParseCorrects_RejectsMalformedAndContradictoryLines(t *testing.T) {
	for _, body := range []string{
		"Corrects:", "Corrects: #0", "Corrects: #-1", "Corrects: #abc",
		"Corrects: #01", "Corrects: #+1", "Corrects: 42", "Corrects: #1.5",
		"Corrects: #99999999999999999999999999999999999999",
		"Corrects: owner/repo#7", "Corrects: #7, owner/repo#8",
		"Corrects: none, #7", "Corrects: #7, none", "Corrects: NONE",
		"Corrects: #7 #8", "Corrects: #7 and #8", "Corrects: #7,",
		"Corrects: ,#7", "Corrects: #7,,#8", "Corrects: #7 trailing prose",
		"Corrects: #7\nCorrects: #8", "Corrects: #7, #8\nCorrects: #7",
		"Corrects: none\nCorrects: #7", "Corrects: #7\nCorrects: none",
		"Corrects: #7\nCorrects: garbage", "Corrects: garbage\nCorrects: none",
	} {
		t.Run(body, func(t *testing.T) {
			if got := Parse(body); got.State != StateInvalid || len(got.Parents) != 0 {
				t.Errorf("Parse = %+v, want invalid with no parents", got)
			}
		})
	}
}

func TestParseCorrects_Bounds(t *testing.T) {
	body := "Corrects: #1" + strings.Repeat(" ", MaxBodyBytes-len("Corrects: #1"))
	if got := Parse(body); got.State != StateParents {
		t.Fatalf("body at cap: %+v", got)
	}
	for _, oversized := range []string{body + " ", strings.Repeat("x", MaxBodyBytes+1)} {
		if got := Parse(oversized); got.State != StateInvalid || len(got.Parents) != 0 {
			t.Errorf("body over cap: %+v", got)
		}
	}
	refs := make([]string, MaxParents)
	for i := range refs {
		refs[i] = fmt.Sprintf("#%d", i+1)
	}
	body = "Corrects: " + strings.Join(refs, ", ")
	if got := Parse(body + ", #1"); got.State != StateParents || len(got.Parents) != MaxParents {
		t.Fatalf("distinct parents at cap with duplicate: %+v", got)
	}
	if got := Parse(body + fmt.Sprintf(", #%d", MaxParents+1)); got.State != StateInvalid || len(got.Parents) != 0 {
		t.Errorf("parents over cap: %+v", got)
	}
}

func TestParseCorrects_AdversarialLines(t *testing.T) {
	// Fuzz-style corpus: malformed bytes, Unicode lookalikes, scanner-sized
	// lines, and line endings must never panic or expose partial parents.
	cases := []struct {
		name, line string
		state      State
	}{
		{"Arabic digits", "Corrects: #١٢", StateInvalid},
		{"fullwidth digits", "Corrects: #１２", StateInvalid},
		{"mixed digits", "Corrects: #1２", StateInvalid},
		{"NUL", "Corrects: #7\x00", StateInvalid},
		{"invalid UTF8", "Corrects: #7\xff", StateInvalid},
		{"huge number", "Corrects: #" + strings.Repeat("9", MaxBodyBytes/2), StateInvalid},
		{"huge malformed value", "Corrects: " + strings.Repeat("x", MaxBodyBytes/2) + " closes #88", StateInvalid},
		{"CRLF", "Corrects: #7\r\n", StateParents},
		{"malformed CRLF", "Corrects: closes #88\r\n", StateInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.line) > MaxBodyBytes {
				t.Fatal("corpus must exercise value parsing within the body cap")
			}
			if got := Parse(c.line); got.State != c.state || got.State == StateInvalid && len(got.Parents) != 0 {
				t.Errorf("Parse = %+v, want %s", got, c.state)
			}
			body := "before\n" + c.line + "\nafter"
			stripped := StripDeclarationLines(body)
			if strings.Contains(stripped, "Corrects:") || !strings.HasPrefix(stripped, "before\n") || !strings.HasSuffix(stripped, "\nafter") {
				t.Errorf("stripping lost boundaries or retained a declaration (length %d)", len(stripped))
			}
			if StripDeclarationLines(stripped) != stripped {
				t.Error("stripping is not idempotent")
			}
		})
	}
}

func TestStripDeclarationLines_PreservesOtherText(t *testing.T) {
	body := "closes #42\r\n Corrects : garbage #7\r\nsee PROJ-9\nCorrects: #8\nlast"
	want := "closes #42\r\nx\r\nsee PROJ-9\nx\nlast"
	if got := StripDeclarationLines(body); got != want {
		t.Errorf("StripDeclarationLines = %q, want %q", got, want)
	}
}

func TestParseCorrects_FencedExamples(t *testing.T) {
	for _, fence := range []string{"```", "~~~"} {
		for _, real := range []string{"", "\nCorrects: none"} {
			t.Run(fence+real, func(t *testing.T) {
				example := fence + "markdown\nCorrects: #99\n" + fence
				body := example + real
				want := Declaration{State: StateMissing}
				if real != "" {
					want.State = StateNone
				}
				if got := Parse(body); !reflect.DeepEqual(got, want) {
					t.Errorf("Parse = %+v, want %+v", got, want)
				}
				stripped := StripDeclarationLines(body)
				wantStripped := fence + "markdown\nx\n" + fence
				if real != "" {
					wantStripped += "\nx"
				}
				if stripped != wantStripped {
					t.Errorf("stripping must sanitize example refs and preserve fences: %q, want %q", stripped, wantStripped)
				}
			})
		}
	}
	body := "> Corrects: #99"
	if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateMissing}) {
		t.Errorf("blockquote Parse = %+v, want missing", got)
	}
	if got := StripDeclarationLines(body); got != body {
		t.Errorf("stripping changed blockquote: %q", got)
	}
}

func TestParseCorrects_MultibyteBodyLimit(t *testing.T) {
	const declaration = "\nCorrects: #7"
	body := strings.Repeat("😀", 65536-len(declaration)) + declaration
	if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateParents, Parents: []int{7}}) {
		t.Errorf("65,536-character body (%d bytes): %+v, want parent 7", len(body), got)
	}
}

func TestParseCorrects_MarkdownVariants(t *testing.T) {
	for _, body := range []string{"**Corrects:** #7", "- Corrects: #7", "Corrects：#7", "*Corrects:* #7", "_Corrects_: #7", "+ cOrReCtS: #7", "* Corrects: #7"} {
		t.Run(body, func(t *testing.T) {
			if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateParents, Parents: []int{7}}) {
				t.Errorf("Parse = %+v, want parent 7", got)
			}
			if got := StripDeclarationLines(body); strings.Contains(got, "#7") {
				t.Errorf("declaration leaked reference: %q", got)
			}
		})
	}
	body := "Corrects #7"
	if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateInvalid}) {
		t.Errorf("missing colon Parse = %+v, want invalid", got)
	}
	if got := StripDeclarationLines(body); strings.Contains(got, "#7") {
		t.Errorf("malformed declaration leaked reference: %q", got)
	}
}

func TestParseCorrects_ASCIIKey(t *testing.T) {
	body := "Correctſ: #7"
	if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateMissing}) {
		t.Errorf("Unicode lookalike Parse = %+v, want missing", got)
	}
	if got := StripDeclarationLines(body); got != body {
		t.Errorf("stripping changed Unicode lookalike: %q", got)
	}
}

func TestParseCorrects_ContainerFences(t *testing.T) {
	// The review's exact body must ignore the example and retain the real none.
	body := "- ```\n  Corrects: #99\n  ```\n\nCorrects: none"
	if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateNone}) {
		t.Errorf("exact body Parse = %+v, want none", got)
	}
	if got, want := StripDeclarationLines(body), "- ```\nx\n  ```\n\nx"; got != want {
		t.Errorf("exact body StripDeclarationLines = %q, want %q", got, want)
	}
	for _, bullet := range []string{"- ", "* ", "+ ", "1. "} {
		for indent := 0; indent <= 3; indent++ {
			for _, fence := range []string{"```", "~~~"} {
				prefix := strings.Repeat(" ", indent) + bullet
				for _, closing := range []string{prefix, strings.Repeat(" ", len(prefix))} {
					body := prefix + fence + "\n" + strings.Repeat(" ", len(prefix)) + "Corrects: #99\n" + closing + fence + "\n\nCorrects: none"
					t.Run(fmt.Sprintf("%q/%q/%q", prefix, fence, closing), func(t *testing.T) {
						if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateNone}) {
							t.Errorf("Parse = %+v, want none", got)
						}
						if got := StripDeclarationLines(body); strings.Contains(got, "#99") || strings.Contains(got, "Corrects: none") {
							t.Errorf("declarations survived stripping: %q", got)
						}
					})
				}
			}
		}
	}
}

func TestParseCorrects_ReferenceBeforeLaterColon(t *testing.T) {
	for _, body := range []string{"Corrects #7: note", "Corrects#7: note", "Corrects \t#7：note", "**Corrects** #7: note"} {
		t.Run(body, func(t *testing.T) {
			if got := Parse(body); !reflect.DeepEqual(got, Declaration{State: StateInvalid}) {
				t.Errorf("Parse = %+v, want invalid with no parents", got)
			}
			if got := StripDeclarationLines(body); got != "x" {
				t.Errorf("StripDeclarationLines = %q, want x", got)
			}
		})
	}
}
