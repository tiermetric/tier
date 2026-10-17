// Package corrects parses corrective PR declarations separately from issue refs.
package corrects

import (
	"slices"
	"strconv"
	"strings"
)

const (
	// MaxBodyBytes bounds Parse's work and covers GitHub's 65,536-character PR
	// body limit at up to four UTF-8 bytes per character (262,144 bytes).
	MaxBodyBytes = 256 << 10
	// MaxParents limits distinct parents in a declaration after deduplication.
	MaxParents = 32
)

// State distinguishes absent, malformed, explicitly unrelated, and parent PRs.
type State string

const (
	StateMissing State = "missing"
	StateInvalid State = "invalid"
	StateNone    State = "none"
	StateParents State = "parents"
)

// Declaration contains same-repository PR numbers, never issue identities.
// Parents is populated only for StateParents, in first-declared order.
type Declaration struct {
	State   State
	Parents []int
}

// Parse reads whole lines of the form "Corrects: #N[, #N...]" or "Corrects: none".
// The key is case-insensitive; "none" is lowercase. Surrounding whitespace is
// ignored, including around the key, colon, and comma-separated references.
// Numbers must be positive ASCII decimal ints without signs or leading zeros;
// overflow and repository-qualified references are invalid, not resolved.
// Duplicate parents are removed without sorting or selecting a preferred PR.
// Repeated lines must name the same parent set (first-line order wins), or all
// say none. Any malformed or contradictory line invalidates the entire body,
// with no partial parents returned. LF and CRLF are supported. Bodies exceeding
// MaxBodyBytes or declarations exceeding MaxParents distinct parents are invalid.
// Fenced code and blockquotes are ignored; emphasis, one list bullet, and a
// fullwidth colon are accepted. A key followed by a reference without a colon
// is malformed; a bare "Corrects" is not a declaration. The key is ASCII-only.
func Parse(body string) Declaration {
	if len(body) > MaxBodyBytes {
		return Declaration{State: StateInvalid}
	}
	result := Declaration{State: StateMissing}
	var recognizer lineRecognizer
	for line := range strings.SplitSeq(body, "\n") {
		value, ok := recognizer.declarationValue(line)
		if !ok {
			continue
		}
		next := parseValue(value)
		if next.State == StateInvalid {
			return next
		}
		if result.State == StateMissing {
			result = next
			continue
		}
		if result.State != next.State || len(result.Parents) != len(next.Parents) {
			return Declaration{State: StateInvalid}
		}
		for _, parent := range next.Parents {
			if !slices.Contains(result.Parents, parent) {
				return Declaration{State: StateInvalid}
			}
		}
	}
	return result
}

func parseValue(value string) Declaration {
	if value == "none" {
		return Declaration{State: StateNone}
	}
	result := Declaration{State: StateParents}
	for ref := range strings.SplitSeq(value, ",") {
		ref = strings.TrimSpace(ref)
		if len(ref) < 2 || ref[0] != '#' || ref[1] < '1' || ref[1] > '9' {
			return Declaration{State: StateInvalid}
		}
		for i := 2; i < len(ref); i++ {
			if ref[i] < '0' || ref[i] > '9' {
				return Declaration{State: StateInvalid}
			}
		}
		n, err := strconv.Atoi(ref[1:])
		if err != nil {
			return Declaration{State: StateInvalid}
		}
		if !slices.Contains(result.Parents, n) {
			if len(result.Parents) == MaxParents {
				return Declaration{State: StateInvalid}
			}
			result.Parents = append(result.Parents, n)
		}
	}
	return result
}

// StripDeclarationLines replaces the contents of every line whose key matches
// Parse's Corrects key, even when its value is malformed or the body exceeds
// MaxBodyBytes, with a non-whitespace, non-punctuation barrier. It preserves
// line endings and all other text, so declaration refs cannot become issues
// and close-clause runs cannot join across a removed declaration.
// This helper does not truncate bodies: issue attribution outside declaration
// lines must survive even when Parse would report StateInvalid.
// Declaration refs in fenced examples are stripped too, for issue attribution.
func StripDeclarationLines(body string) string {
	var out strings.Builder
	for line := range strings.SplitAfterSeq(body, "\n") {
		var recognizer lineRecognizer // Match keys independently of fence state.
		if _, ok := recognizer.declarationValue(line); !ok {
			out.WriteString(line)
		} else {
			out.WriteByte('x') // Neither issueTerminator nor a close-clause separator.
			if strings.HasSuffix(line, "\r\n") {
				out.WriteString("\r\n")
			} else if strings.HasSuffix(line, "\n") {
				out.WriteByte('\n')
			}
		}
	}
	return out.String()
}

type lineRecognizer struct {
	fence  byte
	width  int
	indent int // List container indentation of the opening fence.
}

func (r *lineRecognizer) declarationValue(line string) (string, bool) {
	text := strings.TrimLeft(line, " ")
	indent, containerIndent := len(line)-len(text), 0
	for _, prefix := range []string{"- ", "* ", "+ ", "1. "} {
		if indent <= 3 && strings.HasPrefix(text, prefix) {
			containerIndent = indent + len(prefix)
			text = strings.TrimLeft(text[len(prefix):], " ")
			break
		}
	}
	if containerIndent == 0 && r.fence != 0 {
		indent -= r.indent
	}
	if indent <= 3 && len(text) >= 3 && (text[0] == '`' || text[0] == '~') {
		n := 1
		for n < len(text) && text[n] == text[0] {
			n++
		}
		if r.fence != 0 {
			if text[0] == r.fence && n >= r.width && strings.TrimSpace(text[n:]) == "" {
				r.fence, r.width, r.indent = 0, 0, 0
			}
		} else if n >= 3 && (text[0] != '`' || !strings.Contains(text[n:], "`")) {
			r.fence, r.width, r.indent = text[0], n, containerIndent
		}
		return "", false
	}
	if r.fence != 0 {
		return "", false
	}
	line = strings.TrimSpace(line)
	if len(line) >= 2 && strings.ContainsAny(line[:1], "-*+") && (line[1] == ' ' || line[1] == '\t') {
		line = strings.TrimSpace(line[2:])
	}
	colon := strings.IndexAny(line, ":：")
	key, value := line, ""
	if hash := strings.IndexByte(line, '#'); hash > 0 && (colon < 0 || hash < colon) {
		key = line[:hash]
	} else if colon >= 0 {
		width := 1
		if line[colon] != ':' {
			width = len("：")
		}
		key, value = line[:colon], line[colon+width:]
	} else {
		return "", false
	}
	key = strings.TrimSpace(key)
	emphasis := key[:len(key)-len(strings.TrimLeft(key, "*_"))]
	key = strings.TrimSpace(strings.Trim(key, "*_"))
	if len(key) != len("corrects") {
		return "", false
	}
	for i := range key {
		c := key[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != "corrects"[i] {
			return "", false
		}
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), emphasis)), true
}
