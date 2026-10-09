package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// S05-3: even one identity on each side may represent two different people.
func TestAuditS053DoctorAliasRequiresSamePerson(t *testing.T) {
	for _, identities := range []unjoinedDevelopers{
		{CostOnly: []string{"alice"}, OutcomeOnly: []string{"bob"}},
		{CostOnly: []string{"alice", "carol"}, OutcomeOnly: []string{"bob", "dave"}},
		{CostOnlyCount: 1, OutcomeOnlyCount: 1},
	} {
		var s scoresResponse
		s.Total.TotalCostUSD = 10
		s.DataQuality.AttributedOutcomeShare = ptrF(0)
		s.DataQuality.UnjoinedDevelopers = identities
		got := checkIdentityJoin(s)
		if len(got) != 1 || got[0].status != statusFail {
			t.Fatalf("identity check = %+v, want one FAIL", got)
		}
		hint := got[0].hint
		for _, want := range []string{"same person", "never merge different people", "developer_alias"} {
			if !strings.Contains(hint, want) {
				t.Errorf("remedy %q missing %q", hint, want)
			}
		}
		if strings.Contains(hint, `"alias":"alice"`) || strings.Contains(hint, `"canonical":"bob"`) {
			t.Errorf("remedy invents a pairing of distinct people: %q", hint)
		}
	}
}

// S06-4: a valid slug with a misspelled or unselected repo key must be reported.
func TestAuditS064ShipReportsUnmatchedRepoMappings(t *testing.T) {
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	t.Setenv("TIER_MUSE", "")
	t.Setenv(worktreeAttrEnv, "")
	t.Setenv("TIER_PRICES", "")
	repo := initGitRepo(t)
	otherRepo := initGitRepo(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, repo)
	if err != nil {
		t.Fatal(err)
	}
	claudeDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(claudeDir, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	srv, _ := newShipTestServer(t, "")
	for _, tc := range []struct {
		name, repoPath, key string
		unmatched           bool
	}{
		{"misspelled", repo, repo + "-typo", true},
		{"unselected", repo, otherRepo, true},
		{"control-characters", repo, repo + "-\nforged\x1b[31m", true},
		{"absolute", relative, repo, false},
		{"typed", relative, relative, false},
		{"cleaned", relative, relative + string(filepath.Separator) + ".", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr := captureBothStreams(t, func() {
				runShip([]string{"--server", srv.URL, "--repo", tc.repoPath,
					"--repo-slug", tc.key + "=acme/app", "--claude-dir", claudeDir,
					"--since", "2026-01-01", "--allow-empty"})
			})
			reported := strings.Contains(stderr, "does not match any --repo")
			if reported != tc.unmatched {
				t.Errorf("stderr = %q; unmatched warning = %v, want %v", stderr, reported, tc.unmatched)
			}
			if tc.name == "control-characters" {
				// The newline must be removed and ESC rendered as a printable escape.
				// Keep the expectation independent of logsafe.Str so losing the barrier fails.
				want := "ship: warning: --repo-slug mapping key \"" + repo + "-forged\\x1b[31m\" does not match any --repo; mapping ignored\n"
				if !strings.Contains(stderr, want) {
					t.Errorf("warning = %q, want %q", stderr, want)
				}
				if strings.Count(stderr, "\n") != 1 || strings.ContainsAny(stderr, "\r\x1b") {
					t.Errorf("warning must stay on one line with control characters neutralised: %q", stderr)
				}
			} else if tc.unmatched && !strings.Contains(stderr, tc.key) {
				t.Errorf("warning does not name unused mapping key %q: %q", tc.key, stderr)
			}
		})
	}
}

// S06-5: dry runs preserve expired webhook rows and refuse older schemas
// without migrating them. Read back through raw SQL, never mutating store.Open.
func TestAuditS065DryRunsOpenReadOnly(t *testing.T) {
	for _, v := range []string{"TIER_PRICES", "TIER_READ_ONLY", "TIER_AGGREGATION", "TIER_K_ANONYMITY", "TIER_REPORT_GRACE", "TIER_SEAL_FROM", "TIER_API_TOKEN"} {
		t.Setenv(v, "")
	}
	for _, command := range []string{"seal-arm", "seal-skip", "reprice", "repair-repo"} {
		for _, oldSchema := range []bool{false, true} {
			name := command + "/current-schema"
			if oldSchema {
				name = command + "/old-schema"
			}
			t.Run(name, func(t *testing.T) {
				path := sealTestDB(t)
				if command == "seal-skip" {
					path = armedSealTestDB(t)
				}
				rawExec(t, path, `INSERT INTO webhook_payloads (event, body_gz, body_sha256, received_at) VALUES ('push', x'00', 'x', '2000-01-01 00:00:00')`)
				if oldSchema {
					rawExec(t, path, `PRAGMA user_version = 1`)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				beforeInfo, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				var rc int
				switch command {
				case "seal-arm":
					rc = runSealCmd([]string{"--db", path, "--aggregation", "team", "--arm", "earliest", "--dry-run"}, strings.NewReader(""), &stdout, &stderr)
				case "seal-skip":
					rc = runSealCmd([]string{"--db", path, "--aggregation", "team", "--skip", skipMonth(), "--reason", "audit test", "--dry-run"}, strings.NewReader(""), &stdout, &stderr)
				case "reprice":
					rc = runRepriceCmd([]string{"--db", path, "--from-version", "1"}, &stdout, &stderr)
				case "repair-repo":
					rc = runRepairRepoCmd([]string{"--db", path, "--developer", "alice", "--map", "sess-a=acme/app"}, &stdout, &stderr)
				}
				if oldSchema {
					if rc == 0 || !strings.Contains(stderr.String(), "schema version mismatch") {
						t.Errorf("old-schema dry run: rc=%d stdout=%q stderr=%q; want schema mismatch refusal", rc, stdout.String(), stderr.String())
					}
				} else if rc != 0 {
					t.Errorf("dry run: rc=%d stdout=%q stderr=%q; want success", rc, stdout.String(), stderr.String())
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				afterInfo, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
					t.Error("dry run changed database bytes or mtime")
				}
				if n := webhookRows(t, path); n != 1 {
					t.Errorf("dry run pruned webhook payloads: got %d, want 1", n)
				}
				if oldSchema {
					raw, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = raw.Close() }()
					var stored int
					if err := raw.QueryRow(`PRAGMA user_version`).Scan(&stored); err != nil {
						t.Fatal(err)
					}
					if stored != 1 {
						t.Errorf("dry run migrated schema to %d, want 1", stored)
					}
				}
			})
		}
	}
}
