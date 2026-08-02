package collector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tiermetric/tier/internal/repoid"
)

func writeGitConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepoSlugFromGitConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		want string
	}{
		{
			name: "ssh origin",
			cfg: "[core]\n\tbare = false\n" +
				"[remote \"origin\"]\n\turl = git@github.com:Tiermetric/Tier.git\n\tfetch = +refs/heads/*\n",
			want: "tiermetric/tier",
		},
		{
			name: "https origin",
			cfg:  "[remote \"origin\"]\n\turl = https://github.com/acme/app.git\n",
			want: "acme/app",
		},
		{
			name: "gitlab nested group",
			cfg:  "[remote \"origin\"]\n\turl = https://gitlab.com/group/sub/proj.git\n",
			want: "group/sub/proj",
		},
		{
			name: "non-origin remote is ignored",
			cfg:  "[remote \"upstream\"]\n\turl = git@github.com:upstream/repo.git\n",
			want: "",
		},
		{
			name: "origin after another remote",
			cfg: "[remote \"upstream\"]\n\turl = git@github.com:upstream/repo.git\n" +
				"[remote \"origin\"]\n\turl = git@github.com:fork/repo.git\n",
			want: "fork/repo",
		},
		{
			name: "local path origin is not an identity",
			cfg:  "[remote \"origin\"]\n\turl = /srv/git/repo.git\n",
			want: "",
		},
		{
			name: "no origin",
			cfg:  "[core]\n\tbare = false\n",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeGitConfig(t, root, tc.cfg)
			if got := RepoSlugFromGitConfig(root); got != tc.want {
				t.Fatalf("RepoSlugFromGitConfig = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepoSlugFromGitConfig_MissingRepo(t *testing.T) {
	if got := RepoSlugFromGitConfig(t.TempDir()); got != "" {
		t.Fatalf("no .git => %q, want empty", got)
	}
	if got := RepoSlugFromGitConfig(""); got != "" {
		t.Fatalf("empty path => %q, want empty", got)
	}
}

// The fork case, which is the entire reason the override exists: origin names the
// FORK, while the upstream webhook reports the UPSTREAM. Without the override the
// contributor's cost never joins their outcomes.
func TestJSONLCollector_RepoSlugOverrideBeatsOriginRemote(t *testing.T) {
	root := t.TempDir()
	writeGitConfig(t, root, "[remote \"origin\"]\n\turl = git@github.com:alice/tier.git\n")

	// Without an override we honestly report the fork.
	plain := &JSONLCollector{RepoPath: root}
	if got := plain.repo(); got != "alice/tier" {
		t.Fatalf("no override => %q, want alice/tier (the fork, per origin)", got)
	}

	// With one, the operator's upstream slug wins, so cost joins upstream outcomes.
	overridden := &JSONLCollector{RepoPath: root, RepoSlug: "Tiermetric/Tier"}
	if got := overridden.repo(); got != "tiermetric/tier" {
		t.Fatalf("override => %q, want tiermetric/tier (canonicalized)", got)
	}
}

func TestJSONLCollector_RepoFallsBackToSentinel(t *testing.T) {
	c := &JSONLCollector{RepoPath: t.TempDir()} // no .git at all
	if got := c.repo(); got != repoid.Unqualified {
		t.Fatalf("unresolvable repo => %q, want the %q sentinel", got, repoid.Unqualified)
	}
	// Memoized: a second call must not re-resolve (and must not re-warn).
	if got := c.repo(); got != repoid.Unqualified {
		t.Fatalf("second call => %q", got)
	}
}

// An invalid configured slug must not be silently accepted as a repo identity.
func TestJSONLCollector_InvalidOverrideFallsThrough(t *testing.T) {
	root := t.TempDir()
	writeGitConfig(t, root, "[remote \"origin\"]\n\turl = git@github.com:acme/app.git\n")

	c := &JSONLCollector{RepoPath: root, RepoSlug: "not a slug"}
	if got := c.repo(); got != "acme/app" {
		t.Fatalf("invalid override => %q, want fallback to origin acme/app", got)
	}
}
