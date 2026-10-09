package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// maxToolPathLen is the longest path toolPathsFromLine returns, in bytes
	// (Linux PATH_MAX). A longer one is skipped, never truncated: a truncated
	// path names a different file.
	maxToolPathLen = 4096
	// maxToolPathsPerLine caps the distinct paths returned for one line; the
	// first ones in content order are kept.
	maxToolPathsPerLine = 16
	// maxToolBlocksPerLine bounds the content blocks decoded: encoding/json
	// skips a fixed array's surplus elements unbuilt (a slice cost 1.5 GB for a
	// 10 MiB line of `{}`, #823). Real lines hold at most 3 tool_use blocks.
	maxToolBlocksPerLine = 64
)

// toolPathLine is everything toolPathsFromLine decodes from a transcript line,
// and deliberately nothing more (#823 Q1 = A: path fields only). It is its own
// type, never jsonlEntry and never a generic map, so the token parser's decode
// gains no content field. Every other key, including the rest of each tool
// input (Edit's old_string/new_string, Write's content, Bash's command), is
// skipped by the decoder without being materialised. Claude Code writes the
// keys decoded here, so they match as encoding/json does (case-folded, last
// duplicate wins); the line's type and the model-written input keys are exact.
type toolPathLine struct {
	Message struct {
		Content [maxToolBlocksPerLine]toolPathBlock `json:"content"`
	} `json:"message"`
}

type toolPathBlock struct {
	Type  string        `json:"type"`
	Name  string        `json:"name"`
	Input toolPathInput `json:"input"`
}

// toolPathInput's keys are matched byte-exactly by UnmarshalJSON: FILE_PATH, a
// Kelvin-sign fold or an escaped spelling is not file_path, and a repeated key
// yields nothing. Every other member is scanned past, never decoded or copied.
type toolPathInput struct {
	FilePath, NotebookPath, Path string
}

func (in *toolPathInput) UnmarshalJSON(data []byte) error {
	*in = toolPathInput{jsonStringMember(data, "file_path"), jsonStringMember(data, "notebook_path"), jsonStringMember(data, "path")}
	return nil
}

// path returns the one input field an allowlisted tool names its target in.
// Names match exactly: an MCP tool, a differently cased name and Bash (command
// scanning is a later, default-off PR) all return "".
func (b *toolPathBlock) path() string {
	switch b.Name {
	case "Read", "Edit", "MultiEdit", "Write":
		return b.Input.FilePath
	case "NotebookEdit":
		return b.Input.NotebookPath
	case "Glob", "Grep":
		return b.Input.Path
	}
	return ""
}

// toolPathsFromLine returns the absolute paths named by the allowlisted
// tool_use blocks of one Claude Code JSONL assistant line: distinct, in content
// order, at most maxToolPathsPerLine of them, nil when there are none.
//
// Paths are returned verbatim, never cleaned: the caller must resolve one
// (filepath.EvalSymlinks) before any containment test. A path is the call's
// request, not its effect: a call later denied or failed still yields its
// path (one line cannot see the tool_result), and the caller decides.
//
// It never fails. A line that is not JSON, not an assistant line, or over
// maxJSONLLine yields nil. A wrongly typed field yields nothing for that field
// alone: encoding/json fills the rest and reports only an UnmarshalTypeError.
// A relative path is skipped (neutral, #823), as is one over maxToolPathLen or
// holding a NUL. Path text is returned to the caller only; nothing here logs.
// A network path (isNetworkPath) is skipped too.
func toolPathsFromLine(line []byte) []string {
	if len(line) > maxJSONLLine || !json.Valid(line) {
		return nil
	}
	if jsonStringMember(line, "type") != "assistant" {
		return nil // before the decode: a non-assistant line costs one scan
	}
	var l toolPathLine
	if err := json.Unmarshal(line, &l); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil
		}
	}
	var paths []string
	for i := range l.Message.Content {
		b := &l.Message.Content[i]
		if b.Type != "tool_use" {
			continue
		}
		p := b.path()
		if len(p) > maxToolPathLen || strings.IndexByte(p, 0) >= 0 || !filepath.IsAbs(p) || isNetworkPath(p) || slices.Contains(paths, p) {
			continue
		}
		paths = append(paths, p)
		if len(paths) == maxToolPathsPerLine {
			break
		}
	}
	return paths
}

// isNetworkPath reports a leading double separator: on Windows a UNC or device
// path (\\host\share, \\?\C:) that resolving would authenticate to. Every OS.
func isNetworkPath(p string) bool {
	return len(p) >= 2 && (p[0] == '/' || p[0] == '\\') && (p[1] == '/' || p[1] == '\\')
}

// jsonStringMember returns the string value of obj's member whose key is
// written byte-exactly as key, allocating only that string: "" when obj is not
// an object or the key is absent, repeated or not a string. obj must be valid.
func jsonStringMember(obj []byte, key string) string {
	var val []byte
	i := len(obj) - len(bytes.TrimLeft(obj, " \t\r\n"))
	for sep := byte('{'); i < len(obj) && obj[i] == sep; sep = ',' {
		ks, ke := jsonNext(obj, i+1)
		if ke-ks < 2 || obj[ks] != '"' {
			break
		}
		_, ce := jsonNext(obj, ke) // the colon
		vs, ve := jsonNext(obj, ce)
		if ke-ks == len(key)+2 && string(obj[ks+1:ke-1]) == key {
			if val != nil {
				return ""
			}
			val = obj[vs:ve]
		}
		i, _ = jsonNext(obj, ve)
	}
	var s string
	_ = json.Unmarshal(val, &s) // on any error s stays ""
	return s
}

// jsonNext skips whitespace from b[i] and returns the extent of the value, or
// the one punctuation byte, starting there; valid JSON in, clamped to len(b).
func jsonNext(b []byte, i int) (start, end int) {
	for i < len(b) && b[i] <= ' ' {
		i++
	}
	start, depth := i, 0
	for i < len(b) {
		c := b[i]
		i++
		switch c {
		case '"':
			for esc := false; i < len(b) && (esc || b[i] != '"'); i++ {
				esc = !esc && b[i] == '\\'
			}
			i++
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		if depth <= 0 && (i >= len(b) || jsonDelim(c) || jsonDelim(b[i])) {
			break
		}
	}
	return start, min(i, len(b))
}

func jsonDelim(c byte) bool { return c <= ' ' || strings.IndexByte(`"{}[],:`, c) >= 0 }
