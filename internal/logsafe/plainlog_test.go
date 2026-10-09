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
// 🔴 IT IS EMPTY, AND THAT IS THE POINT (#689). Its one entry was
// internal/store/prices.go, whose unknown-model WARNs predated the slog
// convention and went through a swappable *log.Logger. #689 measured what that
// exception actually cost: `slog.SetDefault` (cmd/tierd/main.go) installs
// `log.SetOutput(&handlerWriter{...})` on the stdlib log package, and
// handlerWriter.Write DISCARDS the bytes and returns a nil error whenever the
// slog handler is not enabled at the bridge level (Info here — see reason 2
// below for why that is a default and not a constant). So `tierd --log-level
// warn` silently ate
// every one of those pricing warnings — a cost-correctness signal, gone, with no
// error on any stream. prices.go now emits through slog.Warn and the exception
// is closed.
//
// A new entry here is therefore not a formality. Adding one re-opens BOTH the
// missing-CR/LF-escaping hole below AND the silent-discard hole above.
var plainLogAllowed = map[string]bool{}

// TestNoNonTestFileImportsPlainLog pins the claim that NO non-test file in the
// tree imports plain "log".
//
// It was called TestPlainLogImportStaysTheDocumentedException while exactly one
// exception existed. #689 closed that exception, so the old name described a
// contract this test no longer asserts — and this is the one file in the tree
// whose stated job is keeping names and comments true.
//
// WHY IT MATTERS RATHER THAN BEING TRIVIA — TWO REASONS, AND THE SECOND WAS
// LEARNED THE HARD WAY.
//
//  1. Escaping. The #321 review's severity split turns on this fact. slog's Text
//     and JSON handlers both escape CR/LF, so every slog sink in the tree gets
//     forgery protection even when a caller forgets the barrier — which is why
//     the unwrapped internal/collector sinks were graded as a log-flood risk
//     rather than a forgery. A plain "log" sink gets NO such backstop.
//
//  2. Reachability (#689). Under `slog.SetDefault`, the stdlib log package is
//     not an independent, always-visible sink at all: it is routed through the
//     slog handler and dropped, silently (n=0, nil error), whenever that handler
//     is not enabled at slog's package-level `logLoggerLevel` — a LevelVar whose
//     zero value is Info. It is NOT immutable: `slog.SetLogLoggerLevel` moves it,
//     and its doc covers the post-SetDefault case explicitly. Nothing in this
//     tree calls it (grep: 0 hits), so Info is the level that applied. A comment
//     in this tree asserted the opposite of all of it — that plain "log"
//     "bypasses any logger config" — and it was false at the line it annotated.
//     Treat that sentence as folklore wherever it appears.
//
// A new file that imports plain "log" fails here, which is the moment to decide
// whether it needs the same treatment — not after a review finds it.
func TestNoNonTestFileImportsPlainLog(t *testing.T) {
	root := filepath.Join("..", "..")
	tracked := trackedGoFiles(t, root)
	// found     — non-test files importing plain "log". The contract: empty.
	// inTestFile — TEST files importing it. Not a violation; it is the live
	//              control that proves this matcher can still find one.
	var found, inTestFile []string
	var parsed int

	for _, rel := range tracked {
		path := filepath.Join(root, filepath.FromSlash(rel))
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			// A file this test cannot parse (or that is staged-deleted, so absent
			// from disk) is not evidence either way; the build gate owns that.
			continue
		}
		parsed++
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || p != "log" {
				continue
			}
			// 🔴 THE TEST/NON-TEST SPLIT HAPPENS HERE, AFTER THE MATCH, NOT AS A
			// `continue` AT THE TOP OF THE LOOP — and that is the whole point of
			// the restructure. Skipping _test.go before matching meant the ONLY
			// producer of a finding was the non-test path, which is empty in the
			// healthy state; every assertion below then read inputs, and mutating
			// `p == "log"` to anything else left the entire test green.
			// Classifying after the match makes the real matcher, over real
			// on-disk files, produce a real positive on every healthy run.
			if strings.HasSuffix(rel, "_test.go") {
				inTestFile = append(inTestFile, rel)
			} else {
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
	// VACUITY GUARD — AND IT HAD TO BE REBUILT, NOT JUST RELAXED (#689).
	//
	// It used to read `if len(found) == 0 { fail }`: "finding nothing at all means
	// the enumeration broke". That was a legitimate anti-vacuity check ONLY while
	// exactly one plain-"log" import was known to exist. Now that the tree has
	// none, `found` is empty in the HEALTHY state, so it can no longer distinguish
	// "clean tree" from "walk reached nothing" — and deleting it outright would
	// have left a guard that passes on an empty enumeration, which is the exact
	// silent-green this file exists to prevent.
	//
	// The replacement has TWO halves, and the second one was missing from the
	// first attempt at this rebuild.
	//
	//  (a) INPUT liveness — the walk really enumerated the repo and really parsed
	//      what it enumerated (the three assertions below).
	//  (b) MATCHER liveness — the real loop, over real files, still produces a
	//      real positive (the inTestFile control further down).
	//
	// (a) alone is not enough, and that is exactly how the first rebuild failed
	// review: every one of its assertions read `tracked`/`parsed` and none was a
	// function of `found`, so breaking the matcher left the test green.
	// TestPlainLogGuardFindsRealImports does not close that either — it parses its
	// own in-memory fixture with a DUPLICATED copy of the loop, so it proves a
	// matcher can work, not that THIS one ran on THESE files.
	//
	// MEASURED 2026-08-28 (at 43fe67f, after #711/#712/#713/#722 landed): 296 tracked
	// .go files, 74 of them non-test. The floor
	// of 50 sits well below both so it never becomes a maintenance tripwire — but
	// be honest about what it buys: it catches "the walk returned a handful of
	// stragglers", NOT "the walk quietly lost a third of the tree".
	if len(tracked) < 50 {
		t.Errorf("git ls-files returned only %d tracked .go files — the enumeration is not "+
			"reaching the tree, so an empty finding proves nothing. Retarget it.", len(tracked))
	}
	if parsed < 50 {
		t.Errorf("only %d files parsed out of %d tracked .go files — the parse arm is "+
			"skipping nearly everything, so an empty finding proves nothing.", parsed, len(tracked))
	}
	// 🔴 (b) THE MATCHER CONTROL. The production matcher above must yield a
	// non-empty result on every healthy run, or an empty `found` is meaningless.
	// Test files are where plain "log" legitimately survives, so they are the
	// natural positive: measured 2026-08-28 there are two —
	// cmd/tierd/guess_path_label_test.go and this file itself.
	//
	// Self-maintaining by construction: this very file imports plain "log" for
	// TestPlainLogGuardFindsRealImports' fixture, so the control cannot rot into
	// vacuity without someone editing the file the control lives in. Mutate
	// `p == "log"` above and THIS is the assertion that reddens.
	if len(inTestFile) == 0 {
		t.Error("the import matcher found plain \"log\" in NO file, not even a _test.go — " +
			"it is not matching anything, so the empty non-test result above proves nothing. " +
			"(This file imports plain \"log\" itself, so a working matcher always finds at least one.)")
	}
	// Positive control on the enumeration itself: a file that certainly exists,
	// certainly is tracked, and is certainly a non-test .go file must be in the
	// list. A path filter that quietly excluded internal/ would pass the counts
	// above (cmd/ alone might clear 50) and fail here.
	if !slices.Contains(tracked, "internal/store/prices.go") {
		t.Error("internal/store/prices.go is not in the enumerated file list; the walk is not " +
			"reaching internal/. (It no longer imports plain \"log\" — #689 moved it to slog — " +
			"but it is still the sentinel that proves the enumeration covers that package.)")
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
