package collector

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// guardedLogKeys are the structured-log keys whose VALUES are producer- or
// filesystem-controlled and unbounded in length, so every one of them must be
// rendered through internal/logsafe.
//
// The list is by KEY rather than by call site on purpose. A per-site review
// closes the sites someone happened to read; this closes the class, and a new
// slog call that logs a path next month fails here rather than in a review two
// quarters later.
var guardedLogKeys = map[string]bool{
	"path":           true,
	"model":          true,
	"session_id":     true,
	"first_session":  true,
	"other_session":  true,
	"models":         true,
	"first_file":     true,
	"unmapped_paths": true,
}

// TestCollectorLogSinksUseLogsafe pins the #321-review finding that this package
// tree treated one half of itself differently from the other.
//
// MEASURED ASYMMETRY, 2026-08-04: jsonl.go, watcher.go and clamp.go imported
// logsafe ZERO times, while the sibling codexrollout/ imported it and wrapped
// every path and session_id it logged. Same package tree, same value classes,
// opposite treatment — and nothing could tell you, because the two halves are
// only comparable by reading both.
//
// WHAT THE EXPOSURE ACTUALLY IS, stated precisely so the next author does not
// over- or under-react: these are slog sinks. slog's Text and JSON handlers both
// ESCAPE CR/LF, so this class is a log FLOOD, not record forgery — genuinely
// lower severity than the raw fmt.Fprintf report writers in cmd/tierd. But slog
// caps NOTHING (measured: a 4 KB value produced a 4170-byte record), and the
// values here are file paths and provider-supplied model strings. logsafe is the
// only thing in the tree that bounds them.
//
// The test reads source rather than runtime for the same reason
// logsafe/shape_test.go does: the property IS the shape. A slog call that omits
// the wrap behaves identically until someone points a real payload at it.
func TestCollectorLogSinksUseLogsafe(t *testing.T) {
	// Covers the SUBPACKAGES too (codexrollout, anthropicadmin, openaiusage), not
	// just this directory. A guard scoped to one package would have re-created the
	// exact asymmetry it exists to catch: the pollers' "models" lists were missed
	// by the review's own enumeration AND survived a mutant while this test was
	// glob-scoped to *.go in the current directory.
	//
	// Enumerated via git ls-files rather than filepath.WalkDir, for the reason
	// spelled out in internal/logsafe/plainlog_test.go's trackedGoFiles: a walker
	// reads nested worktrees, gitignored copies, and build output as if they were
	// source in this repo, which makes the guard's result depend on which checkout
	// it runs in. The sibling guard passed in a worktree and failed in the main
	// tree exactly that way. Tracked-file enumeration has no such dependence.
	var files []string
	for _, rel := range trackedGoFiles(t, ".") {
		if !strings.HasSuffix(rel, "_test.go") {
			files = append(files, rel)
		}
	}
	checked := 0
	for _, name := range files {
		src, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil {
			// Staged-deleted files are in the index but not on disk; nothing to check.
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isLogCall(call) {
				return true
			}
			// Structured args are (key, value) pairs after the message. Walk
			// pairwise and check the value of every guarded key.
			for i := 0; i+1 < len(call.Args); i++ {
				key, ok := stringLitArg(call.Args[i])
				if !ok || !guardedLogKeys[key] {
					continue
				}
				checked++
				if !isLogsafeCall(call.Args[i+1]) {
					pos := fset.Position(call.Args[i+1].Pos())
					t.Errorf("%s: log key %q is not rendered through internal/logsafe.\n"+
						"  Wrap it: logsafe.Str(v) for a scalar, logsafe.Join(v, n) for a list.\n"+
						"  These values are filesystem paths and provider-supplied model/session "+
						"strings — unbounded in length, and slog caps nothing. The sibling "+
						"codexrollout package wraps every one of them; this file must not drift "+
						"from it.\n"+
						"  If this particular value is PROVABLY bounded (a package constant, a "+
						"regex-pinned id), logsafe's package doc permits logging it bare — say so "+
						"in an inline comment and add the key to an exemption here rather than "+
						"deleting the check.", pos, key)
				}
			}
			return true
		})
	}
	// A guard that silently stops finding anything is not a guard. If a refactor
	// renames the keys or moves the sinks, this fails rather than passing vacuously.
	if checked < 25 {
		t.Errorf("only %d guarded log values found in package collector; expected at least 25. "+
			"Either the sinks moved or the key names changed — retarget this test rather than "+
			"letting it pass on an empty set.", checked)
	}
}

// trackedGoFiles returns every .go file TRACKED BY GIT under dir, as
// slash-separated paths relative to dir.
//
// Deliberately a second copy of internal/logsafe/plainlog_test.go's helper rather
// than a new shared test-only package: it is twenty lines, and a package existing
// only to host it would be a heavier structural change than the duplication. Keep
// the two in step — if one learns something (submodules, sparse checkouts), the
// other needs it too.
//
// Fails loudly rather than skipping when git is unavailable: a skip here is a
// false green, and the repo already hard-depends on git in its own make gates.
func trackedGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "ls-files", "-z", "--", "*.go")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v (%s)\n"+
			"This guard enumerates TRACKED files on purpose. Do not replace it with a "+
			"filesystem walk to make this error go away — see the doc above.",
			dir, err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}
	return files
}

// isLogCall matches slog.X(...) and <logger>.X(...) for the level methods.
func isLogCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Debug", "Info", "Warn", "Error", "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
		return true
	}
	return false
}

// isLogsafeCall reports whether e is logsafe.Str(...) / logsafe.Join(...).
func isLogsafeCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "logsafe" {
		return false
	}
	return sel.Sel.Name == "Str" || sel.Sel.Name == "Join"
}

func stringLitArg(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}
