package collector

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Fixture lines are hand-written in the shape Claude Code writes today (one
// tool_use block per assistant line, usage and envelope keys around it), with
// synthetic paths only. "ROOT/" becomes an absolute root on the running OS.

func tpRoot() string {
	if runtime.GOOS == "windows" {
		return "C:/"
	}
	return "/"
}

func tpFix(s string) string { return strings.ReplaceAll(s, "ROOT/", tpRoot()) }

// tpAssistant wraps content blocks in a full assistant-line envelope.
func tpAssistant(content string) []byte {
	return []byte(tpFix(`{"parentUuid":"0b6f1c2e-1111-4a2b-9c3d-000000000001","isSidechain":false,` +
		`"message":{"model":"claude-opus-4-5","id":"msg_01SyntheticAAAAAAAAAAAAAA","type":"message","role":"assistant",` +
		`"content":` + content + `,"stop_reason":"tool_use","stop_sequence":null,` +
		`"usage":{"input_tokens":4,"cache_creation_input_tokens":210,"cache_read_input_tokens":51234,"output_tokens":88,` +
		`"cache_creation":{"ephemeral_1h_input_tokens":210,"ephemeral_5m_input_tokens":0},"service_tier":"standard"}},` +
		`"requestId":"req_011SyntheticBBBBBBBBBBBB","type":"assistant","uuid":"0b6f1c2e-1111-4a2b-9c3d-000000000002",` +
		`"timestamp":"2026-09-29T10:15:30.123Z","userType":"external","entrypoint":"cli","cwd":"ROOT/work/app",` +
		`"sessionId":"5d0c7a3e-2222-4b1c-8d9e-000000000003","version":"2.1.0","gitBranch":"main"}`))
}

func tpWant(paths ...string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = tpFix(p)
	}
	return out
}

func tpCheck(t *testing.T, name string, line []byte, want []string) {
	t.Helper()
	if got := toolPathsFromLine(line); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: toolPathsFromLine = %q, want %q", name, got, want)
	}
}

func TestToolPathsFromLine_EachAllowlistedTool(t *testing.T) {
	cases := []struct {
		name, block, want string
	}{
		{"Read", `{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"file_path":"ROOT/work/app/main.go","offset":10,"limit":40},"caller":{"type":"direct"}}`, "ROOT/work/app/main.go"},
		{"Edit", `{"type":"tool_use","id":"toolu_01E","name":"Edit","input":{"file_path":"ROOT/work/app/edit.go","old_string":"a := 1","new_string":"a := 2","replace_all":false}}`, "ROOT/work/app/edit.go"},
		{"MultiEdit", `{"type":"tool_use","id":"toolu_01M","name":"MultiEdit","input":{"file_path":"ROOT/work/app/multi.go","edits":[{"old_string":"x","new_string":"y","path":"ROOT/elsewhere/not-this.go"}]}}`, "ROOT/work/app/multi.go"},
		{"Write", `{"type":"tool_use","id":"toolu_01W","name":"Write","input":{"file_path":"ROOT/work/app/new.txt","content":"hello"}}`, "ROOT/work/app/new.txt"},
		{"NotebookEdit", `{"type":"tool_use","id":"toolu_01N","name":"NotebookEdit","input":{"notebook_path":"ROOT/work/app/nb.ipynb","cell_id":"c1","new_source":"print(1)","edit_mode":"replace"}}`, "ROOT/work/app/nb.ipynb"},
		{"Glob", `{"type":"tool_use","id":"toolu_01G","name":"Glob","input":{"pattern":"**/*.go","path":"ROOT/work/app/internal"}}`, "ROOT/work/app/internal"},
		{"Grep", `{"type":"tool_use","id":"toolu_01P","name":"Grep","input":{"pattern":"func main","path":"ROOT/work/app","glob":"*.go","output_mode":"content","-n":true,"type":"go"}}`, "ROOT/work/app"},
	}
	for _, tc := range cases {
		tpCheck(t, tc.name, tpAssistant(`[`+tc.block+`]`), tpWant(tc.want))
	}
}

func TestToolPathsFromLine_IgnoresUnlistedToolsAndFields(t *testing.T) {
	cases := map[string]string{
		"Bash command":            `{"type":"tool_use","id":"toolu_01B","name":"Bash","input":{"command":"cat ROOT/work/app/main.go","description":"show it"}}`,
		"Bash with a path key":    `{"type":"tool_use","id":"toolu_01B","name":"Bash","input":{"command":"ls","path":"ROOT/work/app"}}`,
		"MCP tool with path":      `{"type":"tool_use","id":"toolu_01X","name":"mcp__fs__read_file","input":{"path":"ROOT/work/app/main.go"}}`,
		"lower-case read":         `{"type":"tool_use","id":"toolu_01r","name":"read","input":{"file_path":"ROOT/work/app/main.go"}}`,
		"Agent prompt":            `{"type":"tool_use","id":"toolu_01A","name":"Agent","input":{"description":"d","prompt":"edit ROOT/work/app/main.go","subagent_type":"general-purpose"}}`,
		"Read via path key":       `{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"path":"ROOT/work/app/main.go"}}`,
		"Grep via file_path key":  `{"type":"tool_use","id":"toolu_01P","name":"Grep","input":{"pattern":"x","file_path":"ROOT/work/app/main.go"}}`,
		"Glob with no path":       `{"type":"tool_use","id":"toolu_01G","name":"Glob","input":{"pattern":"ROOT/work/app/**/*.go"}}`,
		"text block shaped as it": `{"type":"text","text":"reading","name":"Read","input":{"file_path":"ROOT/work/app/main.go"}}`,
		"thinking block":          `{"type":"thinking","thinking":"ROOT/work/app/main.go","signature":"sig"}`,
	}
	for name, block := range cases {
		tpCheck(t, name, tpAssistant(`[`+block+`]`), nil)
	}
}

func TestToolPathsFromLine_SkipsNonAbsolutePaths(t *testing.T) {
	for _, p := range []string{"src/main.go", "./main.go", "../other/main.go", "~/work/main.go", ""} {
		block := fmt.Sprintf(`{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"file_path":%q}}`, p)
		tpCheck(t, "relative "+p, tpAssistant(`[`+block+`]`), nil)
	}
}

func TestToolPathsFromLine_OnlyAssistantLines(t *testing.T) {
	cases := map[string]string{
		"tool_result": `{"parentUuid":"p","isSidechain":false,"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01R","type":"tool_result","content":"ROOT/work/app/main.go"}]},` +
			`"toolUseResult":{"type":"text","file":{"filePath":"ROOT/work/app/main.go"}},"cwd":"ROOT/work/app","sessionId":"s","timestamp":"2026-09-29T10:15:31Z"}`,
		"user line holding a tool_use": `{"type":"user","message":{"role":"user","content":[{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"file_path":"ROOT/work/app/main.go"}}]}}`,
		"summary":                      `{"type":"summary","summary":"ROOT/work/app","leafUuid":"u"}`,
		"no type":                      `{"message":{"content":[{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"file_path":"ROOT/work/app/main.go"}}]}}`,
	}
	for name, line := range cases {
		tpCheck(t, name, []byte(tpFix(line)), nil)
	}
}

// One message's blocks in content order: a duplicate keeps its first position,
// and non-path blocks between them change nothing.
func TestToolPathsFromLine_OrderedAndDeduped(t *testing.T) {
	line := tpAssistant(`[{"type":"text","text":"Looking."},` +
		`{"type":"tool_use","id":"toolu_01a","name":"Grep","input":{"pattern":"x","path":"ROOT/work/b"}},` +
		`{"type":"tool_use","id":"toolu_01b","name":"Read","input":{"file_path":"ROOT/work/a/f.go"}},` +
		`{"type":"tool_use","id":"toolu_01c","name":"Bash","input":{"command":"ls ROOT/work/c"}},` +
		`{"type":"tool_use","id":"toolu_01d","name":"Edit","input":{"file_path":"ROOT/work/b","old_string":"o","new_string":"n"}},` +
		`{"type":"tool_use","id":"toolu_01e","name":"Write","input":{"file_path":"ROOT/work/a/f.go","content":"c"}}]`)
	tpCheck(t, "mixed", line, tpWant("ROOT/work/b", "ROOT/work/a/f.go"))
}

func TestToolPathsFromLine_Bounds(t *testing.T) {
	read := func(p string) string {
		return fmt.Sprintf(`{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"file_path":%q}}`, p)
	}
	// Path length: 4096 bytes kept, 4097 skipped (not truncated).
	fill := func(n int) string {
		p := tpFix("ROOT/")
		return p + strings.Repeat("a", n-len(p))
	}
	tpCheck(t, "4096-byte path", tpAssistant(`[`+read(fill(4096))+`]`), []string{fill(4096)})
	tpCheck(t, "4097-byte path", tpAssistant(`[`+read(fill(4097))+`]`), nil)
	tpCheck(t, "NUL in path", tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"ROOT/work/a\u0000b"}}]`), nil)

	// Count: 20 distinct paths keep the first 16, in order.
	var blocks, want []string
	for i := range 20 {
		p := tpFix(fmt.Sprintf("ROOT/work/f%02d.go", i))
		blocks = append(blocks, read(p))
		if i < 16 {
			want = append(want, p)
		}
	}
	tpCheck(t, "20 paths", tpAssistant(`[`+strings.Join(blocks, ",")+`]`), want)

	// Duplicates do not use up the count: 30 copies of one path, then a second.
	var dups []string
	for range 30 {
		dups = append(dups, read(tpFix("ROOT/work/same.go")))
	}
	dups = append(dups, read(tpFix("ROOT/work/other.go")))
	tpCheck(t, "30 duplicates", tpAssistant(`[`+strings.Join(dups, ",")+`]`), tpWant("ROOT/work/same.go", "ROOT/work/other.go"))

	// Line length: a line of exactly 10 MiB is read, one byte more is not.
	head := tpFix(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"ROOT/work/big.go"}}]},"pad":"`)
	tail := `"}`
	line := []byte(head + strings.Repeat("x", 10<<20-len(head)-len(tail)) + tail)
	tpCheck(t, "10 MiB line", line, tpWant("ROOT/work/big.go"))
	line = []byte(head + strings.Repeat("x", 10<<20-len(head)-len(tail)+1) + tail)
	tpCheck(t, "10 MiB + 1 line", line, nil)
}

func TestToolPathsFromLine_Malformed(t *testing.T) {
	good := `{"type":"tool_use","id":"toolu_01P","name":"Grep","input":{"pattern":"x","path":"ROOT/work/app"}}`
	for name, line := range map[string][]byte{
		"empty":           nil,
		"not JSON":        []byte("not json at all"),
		"truncated":       tpAssistant(`[` + good + `]`)[:120],
		"trailing junk":   append(tpAssistant(`[`+good+`]`), '}'),
		"JSON null":       []byte("null"),
		"JSON array":      []byte(tpFix(`[{"type":"assistant"}]`)),
		"message null":    []byte(`{"type":"assistant","message":null}`),
		"message string":  []byte(`{"type":"assistant","message":"hi"}`),
		"content string":  tpAssistant(`"plain text reply"`),
		"input string":    tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":"ROOT/work/app/main.go"}]`),
		"file_path array": tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":["ROOT/work/app/main.go"]}}]`),
	} {
		tpCheck(t, name, line, nil)
	}
	// A wrongly typed block field (an UnmarshalTypeError) costs only that
	// block: the good block beside it, before or after, still counts.
	bad := `{"type":"tool_use","id":"t","name":42,"input":{"path":"ROOT/work/bad"}}`
	tpCheck(t, "type error before", tpAssistant(`[`+bad+`,`+good+`]`), tpWant("ROOT/work/app"))
	tpCheck(t, "type error after", tpAssistant(`[`+good+`,`+bad+`]`), tpWant("ROOT/work/app"))
}

// Keys outside the allowlist struct are never read, even when they carry a
// path: wireToolInputs duplicates tool inputs at the top level, and nested
// objects inside input may hold path-named keys.
func TestToolPathsFromLine_UnknownFieldsIgnored(t *testing.T) {
	line := []byte(tpFix(`{"type":"assistant","wireToolInputs":{"toolu_01B":{"file_path":"ROOT/work/wire.go"}},` +
		`"message":{"role":"assistant","file_path":"ROOT/work/msg.go","content":[` +
		`{"type":"tool_use","id":"toolu_01B","name":"Bash","input":{"command":"ls"},"caller":{"type":"direct","path":"ROOT/work/caller"}},` +
		`{"type":"tool_use","id":"toolu_01R","name":"Read","input":{"file_path":"ROOT/work/real.go","extra":{"file_path":"ROOT/work/nested.go"}},"path":"ROOT/work/block.go"}]},` +
		`"cwd":"ROOT/work","toolUseResult":{"filePath":"ROOT/work/result.go"}}`))
	tpCheck(t, "unknown fields", line, tpWant("ROOT/work/real.go"))
}

// TestToolPathAllowlistStructIsNarrow pins every JSON key the extractor decodes
// (#823: "a reflect test pins the allowlist struct"). Adding a field — above
// all a content field such as command, content or old_string — fails here and
// needs its own privacy review. A method can widen a decode without a field, so
// every decoded type must also be free of json.Unmarshaler and
// encoding.TextUnmarshaler, except toolPathInput, whose exact-key scan is the
// one reviewed decoder: its three string fields are all it can fill.
func TestToolPathAllowlistStructIsNarrow(t *testing.T) {
	unmarshalers := []reflect.Type{reflect.TypeFor[json.Unmarshaler](), reflect.TypeFor[encoding.TextUnmarshaler]()}
	var got []string
	var walk func(reflect.Type, string)
	walk = func(tp reflect.Type, prefix string) {
		for {
			for _, u := range unmarshalers {
				if reflect.PointerTo(tp).Implements(u) {
					if tp != reflect.TypeFor[toolPathInput]() {
						t.Errorf("%s: decoded type %v implements %v", prefix, tp, u)
					}
					got = append(got, prefix+u.String())
				}
			}
			if k := tp.Kind(); k != reflect.Slice && k != reflect.Array && k != reflect.Pointer && k != reflect.Map {
				break
			}
			tp = tp.Elem()
		}
		if tp.Kind() != reflect.Struct {
			return
		}
		for i := range tp.NumField() {
			f := tp.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				name = f.Name
			}
			got = append(got, prefix+name+":"+f.Type.Kind().String())
			walk(f.Type, prefix+name+".")
		}
	}
	walk(reflect.TypeOf(toolPathLine{}), "")
	slices.Sort(got)
	want := []string{
		"message.content.input.FilePath:string",
		"message.content.input.NotebookPath:string",
		"message.content.input.Path:string",
		"message.content.input.json.Unmarshaler",
		"message.content.input:struct",
		"message.content.name:string",
		"message.content.type:string",
		"message.content:array",
		"message:struct",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toolPathLine decodes %q, want exactly %q", got, want)
	}
}

// Decode memory is fixed by maxToolBlocksPerLine, not by the content array's
// length, and a non-assistant line is never decoded at all. A []toolPathBlock
// cost ~1.5 GB (TotalAlloc) on the 10 MiB `{}` line below and ~3.3 GB on the
// user line; the bound here is far under one copy of the line.
func TestToolPathsFromLine_AllocationBounded(t *testing.T) {
	fill := func(head, elem, tail string) []byte {
		n := (10<<20 - len(head) - len(tail)) / len(elem)
		return []byte(head + strings.Repeat(elem, n) + tail)
	}
	read := tpFix(`{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"ROOT/work/big.go"}},`)
	for name, tc := range map[string]struct {
		line []byte
		want []string
	}{
		"assistant, 3.5M empty blocks": {fill(`{"type":"assistant","message":{"content":[`+read, `{},`, `{}]}}`), tpWant("ROOT/work/big.go")},
		"assistant, 5M number blocks":  {fill(`{"type":"assistant","message":{"content":[`+read, `1,`, `1]}}`), tpWant("ROOT/work/big.go")},
		"user, 5M number blocks":       {fill(`{"type":"user","message":{"content":[`, `1,`, `1]}}`), nil},
		"assistant, 1.7M input keys":   {fill(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{`, `"a":0,`, tpFix(`"file_path":"ROOT/x"}}]}}`)), tpWant("ROOT/x")},
	} {
		if len(tc.line) > maxJSONLLine {
			t.Fatalf("%s: fixture is %d bytes, over maxJSONLLine", name, len(tc.line))
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		got := toolPathsFromLine(tc.line)
		runtime.ReadMemStats(&after)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: toolPathsFromLine = %q, want %q", name, got, tc.want)
		}
		n := after.TotalAlloc - before.TotalAlloc
		t.Logf("%s: %d bytes allocated", name, n)
		if n > 1<<20 {
			t.Errorf("%s: one call allocated %d bytes, want <= 1 MiB", name, n)
		}
	}
}

// Two leading separators name a network host on Windows (UNC, device path);
// resolving one would authenticate to that host, so it is refused on every OS.
func TestToolPathsFromLine_SkipsNetworkPaths(t *testing.T) {
	for _, p := range []string{`//host/share/x`, `\\host\share\x`, `\\?\C:\x`, `\\.\pipe\x`, `/\host\share\x`, `\/host/share/x`} {
		block := fmt.Sprintf(`{"type":"tool_use","id":"t","name":"Read","input":{"file_path":%q}}`, p)
		tpCheck(t, "network "+p, tpAssistant(`[`+block+`]`), nil)
	}
}

// Paths come back verbatim: no Clean, so ".." and "//" survive for the caller
// to resolve before any containment test.
func TestToolPathsFromLine_ReturnsPathsVerbatim(t *testing.T) {
	line := tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"ROOT/work/a/../b/f.go"}},` +
		`{"type":"tool_use","id":"u","name":"Grep","input":{"pattern":"x","path":"ROOT/work/./c//d"}}]`)
	tpCheck(t, "uncleaned", line, tpWant("ROOT/work/a/../b/f.go", "ROOT/work/./c//d"))
}

// The model writes input keys, so they match byte-exactly: a case or Unicode
// fold of a path key is not that key, and a repeated key yields nothing. The
// line's own type is exact too. Claude Code's envelope keys (message, content,
// a block's type/name/input) still match as encoding/json does; pinned below.
func TestToolPathsFromLine_ExactKeys(t *testing.T) {
	read := func(input string) []byte {
		return tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":` + tpFix(input) + `}]`)
	}
	tpCheck(t, "FILE_PATH", read(`{"FILE_PATH":"ROOT/work/decoy.go"}`), nil)
	tpCheck(t, "File_Path after file_path", read(`{"file_path":"ROOT/work/real.go","File_Path":"ROOT/work/decoy.go"}`), tpWant("ROOT/work/real.go"))
	tpCheck(t, "escaped key", read(`{"file\u005fpath":"ROOT/work/decoy.go"}`), nil)
	tpCheck(t, "repeated file_path", read(`{"file_path":"ROOT/work/real.go","file_path":"ROOT/work/decoy.go"}`), nil)
	tpCheck(t, "Grep PATH", tpAssistant(`[{"type":"tool_use","id":"t","name":"Grep","input":{"pattern":"x","PATH":"`+tpFix("ROOT/work/decoy")+`"}}]`), nil)
	kelvin := tpAssistant(`[{"type":"tool_use","id":"t","name":"NotebookEdit","input":{"notebook_path":"` + tpFix("ROOT/work/real.ipynb") + `","noteboo` + "\u212a" + `_path":"` + tpFix("ROOT/work/decoy.ipynb") + `"}}]`)
	tpCheck(t, "Kelvin-sign fold", kelvin, tpWant("ROOT/work/real.ipynb"))
	block := `[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"ROOT/work/a.go"}}]`
	tpCheck(t, "top-level TYPE", []byte(tpFix(`{"type":"user","TYPE":"assistant","message":{"content":`+block+`}}`)), nil)
	tpCheck(t, "top-level type repeated", []byte(tpFix(`{"type":"assistant","type":"user","message":{"content":`+block+`}}`)), nil)
	tpCheck(t, "envelope NAME folds", tpAssistant(`[{"type":"tool_use","id":"t","NAME":"Read","input":{"file_path":"ROOT/work/a.go"}}]`), tpWant("ROOT/work/a.go"))
}

// Path text never reaches a log: neither a skipped path nor a returned one,
// nor a malformed line.
func TestToolPathsFromLine_LogsNothing(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	toolPathsFromLine(tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"ROOT/work/secret-name.go"}}]`))
	toolPathsFromLine(tpAssistant(`[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"relative/secret-name.go"}}]`))
	toolPathsFromLine([]byte(`{"type":"assistant","message":{"content":[{"name":"Read","input":{"file_path":"/secret-name`))
	if buf.Len() != 0 {
		t.Errorf("toolPathsFromLine logged %q", buf.String())
	}
}
