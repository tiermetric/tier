package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveWatchRepos pins resolveWatchRepos (was 0% coverage): it must
// resolve each --watch-repo to an absolute path, accept a repo whose .git is
// either a directory (normal clone) OR a regular file (git worktree /
// submodule shape -- the :1366-1368 doc promise that the live-ingestion setup
// relies on), and fail-fast with a clear "not a git repository" error on a
// path that has no .git. No git binary is involved -- only os.Stat of a .git
// entry -- so this runs anywhere.
func TestResolveWatchRepos(t *testing.T) {
	// gitDirRepo: a directory containing a .git DIRECTORY (normal clone).
	gitDirRepo := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDirRepo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git dir: %v", err)
	}

	// gitFileRepo: a directory containing a .git REGULAR FILE (worktree shape).
	gitFileRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(gitFileRepo, ".git"), []byte("gitdir: /elsewhere"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	// noGitRepo: a directory with no .git entry at all.
	noGitRepo := t.TempDir()

	// missingPath: a path that does not exist.
	missingPath := filepath.Join(t.TempDir(), "does-not-exist")

	t.Run("git-as-directory accepted, absolute path returned", func(t *testing.T) {
		got, err := resolveWatchRepos([]string{gitDirRepo})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d paths, want 1", len(got))
		}
		if !filepath.IsAbs(got[0]) {
			t.Errorf("resolved path %q is not absolute", got[0])
		}
	})

	t.Run("git-as-regular-file (worktree) accepted", func(t *testing.T) {
		got, err := resolveWatchRepos([]string{gitFileRepo})
		if err != nil {
			t.Fatalf("worktree-shaped repo (.git as a file) must be accepted, got error: %v", err)
		}
		if len(got) != 1 || !filepath.IsAbs(got[0]) {
			t.Fatalf("got %v, want one absolute path", got)
		}
	})

	t.Run("no .git is a not-a-git-repository error", func(t *testing.T) {
		_, err := resolveWatchRepos([]string{noGitRepo})
		if err == nil {
			t.Fatal("expected error for a directory without .git, got nil")
		}
		if !strings.Contains(err.Error(), "not a git repository") {
			t.Errorf("error %q does not mention 'not a git repository'", err)
		}
	})

	t.Run("nonexistent path is an error", func(t *testing.T) {
		if _, err := resolveWatchRepos([]string{missingPath}); err == nil {
			t.Fatal("expected error for a nonexistent path, got nil")
		}
	})

	t.Run("relative input resolves to an absolute path", func(t *testing.T) {
		// Change into the git-dir repo's PARENT and pass the repo's base name as a
		// relative path; the resolved result must still be absolute.
		parent := filepath.Dir(gitDirRepo)
		base := filepath.Base(gitDirRepo)
		t.Chdir(parent)
		got, err := resolveWatchRepos([]string{base})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || !filepath.IsAbs(got[0]) {
			t.Fatalf("got %v, want one absolute path from a relative input", got)
		}
	})

	t.Run("order preserved and first bad entry aborts", func(t *testing.T) {
		// A good repo followed by a bad one: the whole call must fail (fail-fast),
		// not silently return the good prefix.
		if _, err := resolveWatchRepos([]string{gitDirRepo, noGitRepo}); err == nil {
			t.Fatal("expected the bad second entry to abort the whole call")
		}
		// Two good repos: order preserved.
		got, err := resolveWatchRepos([]string{gitDirRepo, gitFileRepo})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d paths, want 2", len(got))
		}
		wantFirst, _ := filepath.Abs(gitDirRepo)
		wantSecond, _ := filepath.Abs(gitFileRepo)
		if got[0] != wantFirst || got[1] != wantSecond {
			t.Errorf("order not preserved: got %v, want [%q %q]", got, wantFirst, wantSecond)
		}
	})
}

// TestParseProxyTarget pins parseProxyTarget (was 0% coverage): a well-formed
// http/https URL is returned parsed; an empty string is a silent nil (no warn);
// a malformed URL, an empty host, or a non-http(s) scheme returns nil AND emits
// the "invalid proxy target URL" warning so a typo'd upstream is visible in the
// log rather than silently dropping traffic.
func TestParseProxyTarget(t *testing.T) {
	newLogger := func(buf *bytes.Buffer) *slog.Logger {
		return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}

	t.Run("empty string is a silent nil with no warning", func(t *testing.T) {
		var buf bytes.Buffer
		if got := parseProxyTarget("", newLogger(&buf)); got != nil {
			t.Errorf("parseProxyTarget(\"\") = %v, want nil", got)
		}
		if strings.Contains(buf.String(), "invalid proxy target URL") {
			t.Errorf("empty target must not warn, got: %q", buf.String())
		}
	})

	t.Run("valid https target", func(t *testing.T) {
		var buf bytes.Buffer
		got := parseProxyTarget("https://api.anthropic.com", newLogger(&buf))
		if got == nil {
			t.Fatal("valid https URL returned nil")
		}
		if got.Host != "api.anthropic.com" {
			t.Errorf("Host = %q, want api.anthropic.com", got.Host)
		}
		if got.Scheme != "https" {
			t.Errorf("Scheme = %q, want https", got.Scheme)
		}
		if buf.Len() != 0 {
			t.Errorf("valid target must not warn, got: %q", buf.String())
		}
	})

	t.Run("valid http target with port", func(t *testing.T) {
		var buf bytes.Buffer
		got := parseProxyTarget("http://localhost:9999", newLogger(&buf))
		if got == nil {
			t.Fatal("valid http URL returned nil")
		}
		if got.Host != "localhost:9999" {
			t.Errorf("Host = %q, want localhost:9999", got.Host)
		}
	})

	invalid := []struct {
		name string
		url  string
	}{
		{"parse error (control byte in host)", "http://\x7f"},
		{"empty host", "http://"},
		{"non-http(s) scheme", "ftp://host"},
	}
	for _, tc := range invalid {
		t.Run("invalid: "+tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if got := parseProxyTarget(tc.url, newLogger(&buf)); got != nil {
				t.Errorf("parseProxyTarget(%q) = %v, want nil", tc.url, got)
			}
			if !strings.Contains(buf.String(), "invalid proxy target URL") {
				t.Errorf("invalid target %q must warn, log was: %q", tc.url, buf.String())
			}
		})
	}
}

// TestDefaultDBPath pins defaultDBPath (was 0% coverage). darwin/linux
// semantics: os.UserHomeDir reads $HOME, so a set HOME yields
// $HOME/.tier/tier.db and an empty HOME (which makes os.UserHomeDir fail on
// unix) falls back to the bare "tier.db". This suite runs on darwin/linux, so
// the $HOME lever is the portable way to drive both branches.
func TestDefaultDBPath(t *testing.T) {
	t.Run("HOME set -> $HOME/.tier/tier.db", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".tier", "tier.db")
		if got := defaultDBPath(); got != want {
			t.Errorf("defaultDBPath() = %q, want %q", got, want)
		}
	})

	t.Run("HOME empty -> bare tier.db fallback", func(t *testing.T) {
		t.Setenv("HOME", "")
		if got := defaultDBPath(); got != "tier.db" {
			t.Errorf("defaultDBPath() = %q, want \"tier.db\" fallback", got)
		}
	})
}
