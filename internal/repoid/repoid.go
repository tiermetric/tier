// Package repoid canonicalizes repository identity into the single string form
// TIER stores in the `repo` column of token_events and outcomes (#231).
//
// # WHY THIS PACKAGE EXISTS, AND WHY IT IS SHARED
//
// Cost rows and outcome rows are written by different producers that learn the
// repository from different places: the GitHub webhook reads `repository.full_name`
// ("Tiermetric/Tier"), while the JSONL collector reads `remote.origin.url` off the
// laptop's disk ("git@github.com:tiermetric/tier.git"). Those two strings describe
// the same repository. If they canonicalize differently by so much as a byte, the
// cost<->outcome join misses and the developer's score silently drops.
//
// So canonicalization lives in exactly one place, is total (never panics, never
// errors mid-pipeline), and is exercised by a table test covering every remote-URL
// shape we have seen. Do not reimplement it at a call site.
//
// # THE SENTINEL
//
// Some producers structurally cannot know the repository — the reverse proxy sees
// only an HTTP request and a header. Those rows carry Unqualified rather than "" or
// NULL. Two reasons, both load-bearing:
//
//  1. SQLite treats NULLs as DISTINCT inside a unique index. A NULL `repo` in
//     idx_outcomes_push_daily_repo (repo, issue_id, push_day) would make every
//     replayed push a fresh row, silently destroying the #196 idempotency guard.
//     A NOT NULL sentinel keeps the dedup semantics intact.
//  2. "unqualified" is queryable. An adopter can measure how much of their capture
//     is still repo-blind; "" or NULL just looks like missing data.
//
// Producers may not forge the sentinel — the ingest API rejects a client-supplied
// "unqualified", mirroring the discipline that guards collector.UnattributedIssueID.
package repoid

import (
	"net/url"
	"strings"
)

// Unqualified is the reserved `repo` value meaning "this producer could not
// determine the repository". It is a real, NOT NULL column value — see the package
// doc for why this is a sentinel and not NULL.
const Unqualified = "unqualified"

// MaxLen bounds a canonical slug. Mirrors api.maxIdentifierLen so the trust
// boundary and the store agree on what fits.
const MaxLen = 256

// IsReal reports whether s names an actual repository, as opposed to the
// Unqualified sentinel or an empty string. The tolerant cost<->outcome join keys
// off this: repo disambiguates only when BOTH sides are real (#231, ruling 2).
func IsReal(s string) bool { return s != "" && s != Unqualified }

// Canonical normalizes an already-slug-shaped repository name ("Owner/Repo",
// "group/subgroup/project") into TIER's canonical form, or returns ("", false) if
// it is not a usable slug.
//
// The canonical form is: lowercase, no host, no scheme, no trailing ".git", no
// leading or trailing "/", at least two non-empty segments. GitLab nested groups
// keep their full path, so "Group/Sub/Proj" -> "group/sub/proj".
//
// Lowercasing is deliberate and slightly lossy: GitHub owner/repo names are
// case-insensitive for lookup but `full_name` preserves creation case, so
// "Tiermetric/Tier" and "tiermetric/tier" name one repository and must collapse to
// one key. The cost of the lossiness is that two repositories differing only by
// case would fuse — GitHub cannot create such a pair, so this is safe there. It is
// the reason we do not simply compare raw strings.
//
// The reserved sentinel is rejected: a producer cannot claim to be Unqualified.
func Canonical(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxLen {
		return "", false
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	if s == "" || s == Unqualified {
		return "", false
	}

	segs := strings.Split(s, "/")
	if len(segs) < 2 {
		// A bare "tier" is not a repository identity — it collides across owners,
		// which is the entire bug #231 fixes. Refuse rather than half-qualify.
		return "", false
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
		if !validSegment(seg) {
			return "", false
		}
	}
	return s, true
}

// FromRemoteURL extracts a canonical slug from a git remote URL, or "" if the URL
// names no host-qualified repository.
//
// Handles the three shapes git writes into .git/config:
//
//	git@github.com:owner/repo.git          (scp-like, no scheme)
//	ssh://git@github.com/owner/repo.git    (scheme)
//	https://github.com/owner/repo.git      (scheme, optionally with userinfo)
//
// A local-path remote ("/srv/git/repo", "file:///srv/git/repo") returns "" on
// purpose: a filesystem path is not a repository identity that a webhook could ever
// agree with, so inventing a slug from it would fabricate a join key that only one
// side of the pipeline can produce.
func FromRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// scp-like syntax has no "//" and puts the path after the first colon.
	// Guard against a Windows drive letter ("C:\src\repo") by requiring the
	// host part to contain a dot and the path part to be non-absolute.
	if !strings.Contains(raw, "//") {
		if host, path, ok := strings.Cut(raw, ":"); ok {
			if _, h, hasUser := strings.Cut(host, "@"); hasUser {
				host = h
			}
			if strings.Contains(host, ".") && path != "" && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, `\`) {
				slug, ok := Canonical(path)
				if !ok {
					return ""
				}
				return slug
			}
		}
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// file:// has no host; a parse failure is not something to guess around.
		return ""
	}
	switch u.Scheme {
	case "ssh", "git", "http", "https":
	default:
		return ""
	}
	slug, ok := Canonical(u.Path)
	if !ok {
		return ""
	}
	return slug
}

// validSegment reports whether one path segment is composed only of characters
// git forges and hosts accept. Rejecting anything else keeps a hostile remote URL
// from smuggling separators or control bytes into a stored join key.
func validSegment(seg string) bool {
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
