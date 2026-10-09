package collector

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubAdder is an in-memory watchAdder that records every Add call and can be
// made to fail — globally (failAll) or per-path (failOn) — so the EMFILE/ENOSPC
// branches and the OnWatchAddFailure callback can be exercised WITHOUT pushing
// the test machine to its real file-descriptor limit (#142).
type stubAdder struct {
	mu      sync.Mutex
	added   []string
	failAll error
	failOn  map[string]error
}

func (s *stubAdder) Add(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.added = append(s.added, p)
	if s.failOn != nil {
		if err, ok := s.failOn[p]; ok {
			return err
		}
	}
	return s.failAll
}

func (s *stubAdder) addedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.added...)
}

func (s *stubAdder) wasAdded(p string) bool {
	for _, a := range s.addedPaths() {
		if a == p {
			return true
		}
	}
	return false
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

// TestAddRecursive_EMFILEHintLoggedOnce injects a failing Add that returns
// EMFILE for every directory and asserts the macOS fd-limit hint is logged
// exactly once at ERROR, with all subsequent failures falling to WARN.
func TestAddRecursive_EMFILEHintLoggedOnce(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	stub := &stubAdder{failAll: syscall.EMFILE}
	w := &Watcher{}

	// nil targets => no pruning => root + a + b + c = 4 Add attempts, all fail.
	if err := w.addRecursive(stub, root, root, nil, logger); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}

	out := buf.String()
	if got := strings.Count(out, "ulimit -n 4096"); got != 1 {
		t.Errorf("EMFILE ERROR hint count = %d, want 1\n%s", got, out)
	}
	if !strings.Contains(out, "file-descriptor limit reached") {
		t.Errorf("missing EMFILE ERROR message:\n%s", out)
	}
	// 4 dirs attempted: 1 ERROR (first EMFILE) + 3 WARN (subsequent).
	if got := strings.Count(out, "watcher: add failed"); got != 3 {
		t.Errorf("WARN 'add failed' count = %d, want 3\n%s", got, out)
	}
}

// TestAddRecursive_ENOSPCBranchUnchanged is a regression pin: the Linux inotify
// path must keep logging the sysctl hint exactly once through the #142 refactor.
func TestAddRecursive_ENOSPCBranchUnchanged(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, filepath.Join(root, "a"), filepath.Join(root, "b"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	stub := &stubAdder{failAll: syscall.ENOSPC}
	w := &Watcher{}

	if err := w.addRecursive(stub, root, root, nil, logger); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}

	out := buf.String()
	// The exact sysctl hint value appears once (the message also mentions the
	// knob, so count the unique hint string, not the substring).
	if got := strings.Count(out, "sudo sysctl fs.inotify.max_user_watches=524288"); got != 1 {
		t.Errorf("ENOSPC sysctl hint count = %d, want 1\n%s", got, out)
	}
	if strings.Contains(out, "ulimit") {
		t.Errorf("ENOSPC path must not emit the macOS ulimit hint:\n%s", out)
	}
}

// TestWatcher_OnWatchAddFailureCallback confirms the callback fires once per
// failed Add with the offending path and error, and not at all for successes.
func TestWatcher_OnWatchAddFailureCallback(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "bad")
	mkdirs(t, bad, filepath.Join(root, "good"))

	stub := &stubAdder{failOn: map[string]error{bad: syscall.EMFILE}}
	var (
		mu    sync.Mutex
		paths []string
		errs  []error
	)
	w := &Watcher{OnWatchAddFailure: func(p string, err error) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, p)
		errs = append(errs, err)
	}}

	if err := w.addRecursive(stub, root, root, nil, discardLogger()); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 {
		t.Fatalf("callback invocations = %d, want 1 (paths=%v)", len(paths), paths)
	}
	if paths[0] != bad {
		t.Errorf("callback path = %q, want %q", paths[0], bad)
	}
	if !errors.Is(errs[0], syscall.EMFILE) {
		t.Errorf("callback err = %v, want EMFILE", errs[0])
	}
}

// TestWatcher_PrunesNonTargetProjectDirs proves the core #142 fd-saving
// behaviour: a project dir for a repo that is NOT a configured target is never
// watched, the projects root and the target dir ARE, and a session under the
// target still ingests end-to-end through the real fsnotify path.
func TestWatcher_PrunesNonTargetProjectDirs(t *testing.T) {
	claudeDir := t.TempDir()
	targetRepo := t.TempDir()
	otherRepo := t.TempDir()
	projectsRoot := filepath.Join(claudeDir, "projects")
	targetProj := filepath.Join(projectsRoot, encodeProjectDir(targetRepo))
	otherProj := filepath.Join(projectsRoot, encodeProjectDir(otherRepo))
	mkdirs(t, targetProj, otherProj)

	targets := resolveTargets([]string{targetRepo})
	stub := &stubAdder{}
	w := &Watcher{}
	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := w.addRecursive(stub, projectsRoot, projectsRoot, targets, logger); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}

	if !stub.wasAdded(projectsRoot) {
		t.Errorf("projects root must always be watched (new-dir CREATE events); added=%v", stub.addedPaths())
	}
	if !stub.wasAdded(targetProj) {
		t.Errorf("target project dir must be watched; added=%v", stub.addedPaths())
	}
	if stub.wasAdded(otherProj) {
		t.Errorf("non-target project dir %q must NOT be watched (wastes an fd)", otherProj)
	}
	// The operator breadcrumb must report exactly the one dir that was pruned.
	if got := logbuf.String(); !strings.Contains(got, "skipped non-target project dirs") || !strings.Contains(got, "count=1") {
		t.Errorf("expected a single-dir prune INFO log with count=1, got:\n%s", got)
	}

	// End-to-end through real fsnotify with pruning active: a session written
	// under the target project dir still ingests.
	rec := &recordingStore{}
	startWatcher(t, claudeDir, []string{targetRepo}, rec)
	writeJSONLLine(t, filepath.Join(targetProj, "s1.jsonl"), "sess-1", targetRepo, "feature/42-foo", 100)
	if events := waitForEvents(t, rec, 1, 2*time.Second); len(events) != 1 {
		t.Fatalf("target session did not ingest with pruning active: got %d events, want 1", len(events))
	}
}

// TestWatcher_OutOfTreeWorktreeWatched pins the regression the reviewer caught:
// a git worktree of a target located OUTSIDE the repo tree has a project dir
// whose encoded name shares no prefix with the main repo, yet matchTarget
// accepts its sessions via gitMainRepoRoot — so its project dir must be kept.
// The keep-set is expanded from .git/worktrees, which we fabricate directly
// (git's on-disk layout) so the test needs no git binary.
func TestWatcher_OutOfTreeWorktreeWatched(t *testing.T) {
	claudeDir := t.TempDir()
	targetRepo := t.TempDir()
	worktree := t.TempDir() // an out-of-tree location, no shared prefix with targetRepo
	projectsRoot := filepath.Join(claudeDir, "projects")

	// Fabricate targetRepo/.git/worktrees/wt1/gitdir -> <worktree>/.git, exactly
	// as `git worktree add` would record it.
	wtMeta := filepath.Join(targetRepo, ".git", "worktrees", "wt1")
	mkdirs(t, wtMeta)
	if err := os.WriteFile(filepath.Join(wtMeta, "gitdir"), []byte(filepath.Join(worktree, ".git")+"\n"), 0o644); err != nil {
		t.Fatalf("write gitdir: %v", err)
	}

	worktreeProj := filepath.Join(projectsRoot, encodeProjectDir(worktree))
	mkdirs(t, worktreeProj)

	targets := resolveTargets([]string{targetRepo})
	stub := &stubAdder{}
	w := &Watcher{}
	if err := w.addRecursive(stub, projectsRoot, projectsRoot, targets, discardLogger()); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}
	if !stub.wasAdded(worktreeProj) {
		t.Errorf("out-of-tree worktree project dir %q must be watched (matchTarget accepts its sessions); added=%v", worktreeProj, stub.addedPaths())
	}
}

// TestWatcher_PruneAppliesToCreateEventPath exercises the runtime path: the
// fsnotify Create handler calls addRecursive with the newly-created dir as the
// walk root. A non-target top-level dir arriving after startup must be pruned
// (SkipDir at the root => nothing added), while a target dir arriving the same
// way is watched.
func TestWatcher_PruneAppliesToCreateEventPath(t *testing.T) {
	claudeDir := t.TempDir()
	targetRepo := t.TempDir()
	otherRepo := t.TempDir()
	projectsRoot := filepath.Join(claudeDir, "projects")
	targetProj := filepath.Join(projectsRoot, encodeProjectDir(targetRepo))
	otherProj := filepath.Join(projectsRoot, encodeProjectDir(otherRepo))
	mkdirs(t, targetProj, otherProj)

	targets := resolveTargets([]string{targetRepo})
	w := &Watcher{}

	// Simulate the CREATE handler firing for the non-target dir.
	stubOther := &stubAdder{}
	if err := w.addRecursive(stubOther, otherProj, projectsRoot, targets, discardLogger()); err != nil {
		t.Fatalf("addRecursive(other): %v", err)
	}
	if stubOther.wasAdded(otherProj) {
		t.Errorf("CREATE of a non-target project dir must be pruned, but %q was watched", otherProj)
	}

	// And for the target dir — must be watched.
	stubTarget := &stubAdder{}
	if err := w.addRecursive(stubTarget, targetProj, projectsRoot, targets, discardLogger()); err != nil {
		t.Fatalf("addRecursive(target): %v", err)
	}
	if !stubTarget.wasAdded(targetProj) {
		t.Errorf("CREATE of a target project dir must be watched, but %q was not", targetProj)
	}
}

// TestWatcher_SubdirCWDPrefixStillWatched covers the prefix rule: a project dir
// encoding a CWD *inside* a target repo (repoEncoded + "-<subpath>") must be
// watched, because subdirectory CWDs produce in-scope sessions.
func TestWatcher_SubdirCWDPrefixStillWatched(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsRoot := filepath.Join(claudeDir, "projects")
	// A CWD one level inside the repo: /repo/cmd/tierd -> repoEncoded-cmd-tierd.
	subdirProj := filepath.Join(projectsRoot, encodeProjectDir(repo)+"-cmd-tierd")
	mkdirs(t, subdirProj)

	targets := resolveTargets([]string{repo})
	stub := &stubAdder{}
	w := &Watcher{}
	if err := w.addRecursive(stub, projectsRoot, projectsRoot, targets, discardLogger()); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}
	if !stub.wasAdded(subdirProj) {
		t.Errorf("subdir-CWD project dir %q must be watched (prefix rule); added=%v", subdirProj, stub.addedPaths())
	}
}

// TestWatcher_EmptyReposWatchesEverything pins the health-check mode: with no
// configured targets, pruning is disabled and every project dir is watched
// (watching nothing would be the wrong failure direction).
func TestWatcher_EmptyReposWatchesEverything(t *testing.T) {
	claudeDir := t.TempDir()
	projectsRoot := filepath.Join(claudeDir, "projects")
	d1 := filepath.Join(projectsRoot, "-tmp-alpha")
	d2 := filepath.Join(projectsRoot, "-tmp-beta")
	mkdirs(t, d1, d2)

	stub := &stubAdder{}
	w := &Watcher{}
	// Empty targets => no pruning.
	if err := w.addRecursive(stub, projectsRoot, projectsRoot, nil, discardLogger()); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}
	if !stub.wasAdded(d1) || !stub.wasAdded(d2) {
		t.Errorf("empty Repos must watch every project dir; added=%v", stub.addedPaths())
	}
}

// TestWatcher_UnexpectedShapeFailsOpen confirms a project dir whose name is not
// the expected encoded shape (does not start with '-') is watched rather than
// dropped — silently losing a watch loses real cost data.
func TestWatcher_UnexpectedShapeFailsOpen(t *testing.T) {
	claudeDir := t.TempDir()
	repo := t.TempDir()
	projectsRoot := filepath.Join(claudeDir, "projects")
	weird := filepath.Join(projectsRoot, "legacy_no_dash")
	mkdirs(t, weird)

	targets := resolveTargets([]string{repo})
	stub := &stubAdder{}
	w := &Watcher{}
	if err := w.addRecursive(stub, projectsRoot, projectsRoot, targets, discardLogger()); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}
	if !stub.wasAdded(weird) {
		t.Errorf("unexpected-shape dir %q must fail open (be watched); added=%v", weird, stub.addedPaths())
	}
}

// TestEncodeProjectDir verifies the encoding against the documented rule so a
// future layout drift is caught here rather than as silent capture loss.
func TestEncodeProjectDir(t *testing.T) {
	got := encodeProjectDir("/Users/dev/gitrepos/tier")
	want := "-Users-dev-gitrepos-tier"
	if got != want {
		t.Errorf("encodeProjectDir = %q, want %q", got, want)
	}
}
