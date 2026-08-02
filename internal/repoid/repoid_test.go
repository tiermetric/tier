package repoid

import "testing"

func TestCanonical(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		wantK bool
	}{
		{"plain", "owner/repo", "owner/repo", true},
		{"github full_name preserves creation case", "Tiermetric/Tier", "tiermetric/tier", true},
		{"trailing .git", "owner/repo.git", "owner/repo", true},
		{"surrounding slashes", "/owner/repo/", "owner/repo", true},
		{"whitespace", "  owner/repo\n", "owner/repo", true},
		{"gitlab nested group keeps full path", "Group/Sub/Proj", "group/sub/proj", true},
		{"dots and dashes and underscores", "my-org/my_repo.v2", "my-org/my_repo.v2", true},

		{"empty", "", "", false},
		{"bare name has no owner", "tier", "", false},
		{"reserved sentinel cannot be forged", "unqualified", "", false},
		{"reserved sentinel cased", "Unqualified", "", false},
		{"empty segment", "owner//repo", "", false},
		{"dot segment", "owner/./repo", "", false},
		{"parent segment", "owner/../repo", "", false},
		{"space inside segment", "own er/repo", "", false},
		{"colon smuggled in", "owner/re:po", "", false},
		{"newline smuggled in", "owner/re\npo", "", false},
		{"too long", string(make([]byte, MaxLen+1)), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Canonical(tc.in)
			if ok != tc.wantK || got != tc.want {
				t.Fatalf("Canonical(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantK)
			}
		})
	}
}

func TestFromRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"scp-like", "git@github.com:owner/repo.git", "owner/repo"},
		{"scp-like no .git", "git@github.com:owner/repo", "owner/repo"},
		{"scp-like mixed case", "git@github.com:Tiermetric/Tier.git", "tiermetric/tier"},
		{"ssh scheme", "ssh://git@github.com/owner/repo.git", "owner/repo"},
		{"https", "https://github.com/owner/repo.git", "owner/repo"},
		{"https no .git", "https://github.com/owner/repo", "owner/repo"},
		{"https with token userinfo", "https://user:tok@gitlab.com/group/sub/proj.git", "group/sub/proj"},
		{"git scheme", "git://github.com/owner/repo.git", "owner/repo"},
		{"http", "http://ghe.internal/owner/repo.git", "owner/repo"},

		// A filesystem path is not a repository identity. The webhook can never
		// produce "users/steve/src/tier", so neither may we -- a fabricated key
		// that only one producer emits is worse than an honest sentinel.
		{"absolute local path", "/srv/git/repo.git", ""},
		{"file scheme", "file:///srv/git/repo.git", ""},
		{"relative local path", "../sibling/repo", ""},
		{"windows drive letter", `C:\src\repo`, ""},
		{"empty", "", ""},
		{"scheme we do not trust", "ftp://example.com/owner/repo.git", ""},
		{"host-only, no path", "https://github.com/", ""},
		{"single path segment", "https://github.com/repo.git", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromRemoteURL(tc.in); got != tc.want {
				t.Fatalf("FromRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The whole point of the package: the webhook's full_name and the collector's
// origin URL must land on the SAME key, or the cost<->outcome join silently misses
// and the developer's score drops to zero with no error anywhere.
func TestWebhookAndCollectorAgree(t *testing.T) {
	fromWebhook, ok := Canonical("Tiermetric/Tier") // repository.full_name
	if !ok {
		t.Fatal("webhook full_name failed to canonicalize")
	}
	for _, remote := range []string{
		"git@github.com:Tiermetric/Tier.git",
		"https://github.com/Tiermetric/Tier.git",
		"ssh://git@github.com/tiermetric/tier",
	} {
		if got := FromRemoteURL(remote); got != fromWebhook {
			t.Fatalf("collector remote %q -> %q, webhook full_name -> %q; they MUST agree", remote, got, fromWebhook)
		}
	}
}

func TestIsReal(t *testing.T) {
	if IsReal("") || IsReal(Unqualified) {
		t.Fatal("empty and sentinel must not be real")
	}
	if !IsReal("owner/repo") {
		t.Fatal("a real slug must be real")
	}
}
