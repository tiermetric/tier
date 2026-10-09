package main

import (
	"strings"
	"testing"
)

func TestPrintRepoSummary_LogSafePath(t *testing.T) {
	out := captureStdout(t, func() {
		printRepoSummary([]repoSummary{{
			Path: "/repo\r\nforged\x1b[31m", SessionsWithEvents: 1, EventsShipped: 2,
		}}, nil)
	})
	want := "Per-repo summary:\n  \"/repoforged\\x1b[31m\": sessions_with_events=1 events_shipped=2\n"
	if out != want {
		t.Fatalf("summary = %q, want one sanitized repo row %q", out, want)
	}
	if strings.Count(out, "\n") != 2 || strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("summary contains extra rows or raw control characters: %q", out)
	}
}

func TestParseRepoSlugPairs_Duplicates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pairs    []string
		conflict bool
	}{
		{"conflicting", []string{".=owner/first", ".=owner/second"}, true},
		{"conflicting_cleaned", []string{".=owner/first", "./=owner/second"}, true},
		{"identical", []string{".=owner/first", ".=owner/first"}, false},
		{"identical_cleaned", []string{".=owner/first", "./=Owner/First"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRepoSlugPairs(tc.pairs)
			if tc.conflict {
				want := `repo path "." is mapped to both "owner/first" and "owner/second" — resolve the ambiguity rather than letting one silently win`
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %q", err, want)
				}
				return
			}
			if err != nil || len(got) != 1 || got["."] != "owner/first" {
				t.Fatalf("identical mappings = %v, %v; want one owner/first mapping", got, err)
			}
		})
	}
}

func TestRunShip_RepoSlugDuplicates(t *testing.T) {
	t.Chdir(initGitRepo(t))
	for _, tc := range []struct {
		name     string
		second   string
		wantExit int
	}{
		{"conflicting", ".=owner/second", 1},
		{"conflicting_cleaned", "./=owner/second", 1},
		{"identical", ".=owner/first", 0},
		{"identical_cleaned", "./=owner/first", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runSecretEnvChild(t, nil, "ship",
				"--server", "http://127.0.0.1:1",
				"--repo", ".", "--repo-slug", ".=owner/first", "--repo-slug", tc.second,
				"--claude-dir", t.TempDir(), "--since", "2026-01-01", "--allow-empty",
			)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; output:\n%s", code, tc.wantExit, out)
			}
			if tc.wantExit == 1 {
				want := `--repo-slug: repo path "." is mapped to both "owner/first" and "owner/second" — resolve the ambiguity rather than letting one silently win`
				if !strings.Contains(out, want) {
					t.Fatalf("missing conflict diagnostic %q; output:\n%s", want, out)
				}
			} else if !strings.Contains(out, "No events shipped") {
				t.Fatalf("identical mappings did not complete shipping; output:\n%s", out)
			}
		})
	}
}
