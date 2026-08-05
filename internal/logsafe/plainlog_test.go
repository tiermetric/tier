package logsafe

import (
	"bytes"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// plainLogAllowed lists the non-test files permitted to import the standard
// library's plain "log" rather than "log/slog", keyed by repo-relative path.
//
// internal/store/prices.go is here because its two unknown-model WARNs predate
// the slog convention and are routed through a swappable *log.Logger that tests
// capture. It is not a problem in itself — but it IS a documented exception that
// two comments in that file now depend on, so it must not silently become the
// norm.
var plainLogAllowed = map[string]bool{
	"internal/store/prices.go": true,
}

// TestPlainLogImportStaysTheDocumentedException pins a claim that two comments in
// internal/store/prices.go make load-bearing: that prices.go is the ONLY non-test
// file in the tree importing plain "log".
//
// WHY IT MATTERS RATHER THAN BEING TRIVIA. The #321 review's severity split turns
// entirely on this fact. slog's Text and JSON handlers both escape CR/LF, so
// every slog sink in the tree gets forgery protection even when a caller forgets
// the barrier — which is why the unwrapped internal/collector sinks were graded
// as a log-flood risk rather than a forgery. prices.go gets NO such backstop.
// "This sink is uniquely exposed" is the reason logsafe.Str is mandatory there,
// and it stops being true the moment a second file imports plain "log".
//
// A new file that imports plain "log" fails here, which is the moment to decide
// whether it needs the same treatment — not after a review finds it.
func TestPlainLogImportStaysTheDocumentedException(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string

	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			// A file this test cannot parse (or that is staged-deleted, so absent
			// from disk) is not evidence either way; the build gate owns that.
			continue
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr == nil && p == "log" {
				found = append(found, rel)
			}
		}
	}

	for _, f := range found {
		if !plainLogAllowed[f] {
			t.Errorf("%s imports plain \"log\" and is not in plainLogAllowed.\n"+
				"slog escapes CR/LF and plain \"log\" does not, so a client-controlled value "+
				"logged here has NO backstop if the logsafe wrap is forgotten — the property "+
				"the #321 severity split depends on. Either switch it to log/slog, or add it "+
				"here AND route its client-controlled values through logsafe.Str.", f)
		}
	}
	for f := range plainLogAllowed {
		if !slices.Contains(found, f) {
			t.Errorf("plainLogAllowed lists %s, but it no longer imports plain \"log\". "+
				"Drop the entry — a stale allowlist hides the next real one.", f)
		}
	}
	// Vacuity guard: finding nothing at all means the enumeration broke, not that
	// the tree is clean.
	if len(found) == 0 {
		t.Error("found no plain-\"log\" imports anywhere, not even the known one in " +
			"internal/store/prices.go — the file enumeration is not reaching the tree. Retarget it.")
	}
}

// trackedGoFiles returns every .go file TRACKED BY GIT under root, as
// slash-separated paths relative to root.
//
// 🔴 IT ASKS GIT RATHER THAN WALKING THE FILESYSTEM, AND THAT IS THE WHOLE POINT.
// A filepath.WalkDir version of this guard passed in a worktree and FAILED in the
// main tree: the main tree carries nested worktrees under .claude/worktrees/*,
// which are separate checkouts of this same repo pinned at older commits, and the
// walker read their internal/store/prices.go copies as if they were source files
// here. A check whose result depends on which checkout it runs in is worse than
// no check — it is the same bug class as a gate that silently skips in a worktree,
// just pointing the other way.
//
// git ls-files closes the CLASS, not just that symptom: nested worktrees, any
// gitignored or vendored copy, build output, scratch directories, and editor
// backups are all untracked and therefore all invisible here, without this guard
// having to know any of their names. A hardcoded ".claude" skip would have fixed
// today's symptom and left every other member of the class live.
//
// Fails loudly rather than skipping when git is unavailable. A skip here is a
// false green, and the repo already hard-depends on git in its own make gates.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v (%s)\n"+
			"This guard enumerates TRACKED files on purpose — see trackedGoFiles. "+
			"Do not replace it with a filesystem walk to make this error go away.",
			root, err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}
	return files
}

// TestPlainLogGuardFindsRealImports is the control arm: it proves the AST walk
// above actually detects a plain "log" import rather than passing because it
// never looks. Without it, a broken matcher and a clean tree are the same green.
func TestPlainLogGuardFindsRealImports(t *testing.T) {
	const src = `package p

import (
	"fmt"
	"log"
	"log/slog"
)

var _ = fmt.Sprint
var _ = log.Print
var _ = slog.Info
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	for _, imp := range f.Imports {
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr == nil && p == "log" {
			got = append(got, p)
		}
	}
	// Exactly one: the fixture also imports "log/slog", and a matcher that used
	// strings.HasPrefix or strings.Contains instead of an exact compare would
	// count two — silently widening the allowlist check into uselessness.
	if len(got) != 1 {
		t.Errorf("matcher found %d plain-\"log\" imports in a fixture with exactly one; "+
			"it must not confuse \"log/slog\" with \"log\"", len(got))
	}
	if len(f.Imports) != 3 {
		t.Errorf("fixture parse lost imports: got %d, want 3", len(f.Imports))
	}
}
