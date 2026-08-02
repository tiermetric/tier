package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testGenerator builds a generator with a fixed docsRoot ("docs") and the given
// in-bundle map, for unit-testing link classification without touching disk.
func testGenerator(bundle map[string]string) *generator {
	return &generator{
		md:       newMarkdown(),
		policy:   docsPolicy(),
		bundle:   bundle,
		docsRoot: "docs",
	}
}

// sampleBundle mirrors the shape of the real docs set: some top-level pages, one
// under internals/, and README as the index.
func sampleBundle() map[string]string {
	return map[string]string{
		"docs/README.md":      "README.html",
		"docs/conventions.md": "conventions.html",
		"docs/rubric.md":      "rubric.html",
		"docs/internals/tenancy-retrofit-playbook.md": "tenancy-retrofit-playbook.html",
	}
}

// TestClassifyLink pins the link model: in-bundle .md -> .html (basename, anchor
// preserved); escaping .md (e.g. the repo-root README) and non-.md repo assets ->
// canonical GitHub blob URL; external + same-page anchors handled distinctly.
func TestClassifyLink(t *testing.T) {
	g := testGenerator(sampleBundle())
	const gh = "https://github.com/tiermetric/tier/blob/main/"

	tests := []struct {
		name        string
		dest        string
		srcDir      string
		wantDest    string // expected rewritten destination ("" => not rewritten)
		wantRewrite bool
		wantHas     bool   // recorded for integrity?
		wantTarget  string // in-bundle target outName
		wantAnchor  string
		wantSelf    bool
		wantDangle  bool
	}{
		{
			name: "in-bundle same dir", dest: "conventions.md", srcDir: "docs",
			wantDest: "conventions.html", wantRewrite: true, wantHas: true, wantTarget: "conventions.html",
		},
		{
			name: "in-bundle dot-slash with anchor", dest: "./rubric.md#size", srcDir: "docs",
			wantDest: "rubric.html#size", wantRewrite: true, wantHas: true, wantTarget: "rubric.html", wantAnchor: "size",
		},
		{
			name: "in-bundle from internals up to top", dest: "../conventions.md", srcDir: "docs/internals",
			wantDest: "conventions.html", wantRewrite: true, wantHas: true, wantTarget: "conventions.html",
		},
		{
			name: "in-bundle down into internals", dest: "internals/tenancy-retrofit-playbook.md", srcDir: "docs",
			wantDest: "tenancy-retrofit-playbook.html", wantRewrite: true, wantHas: true, wantTarget: "tenancy-retrofit-playbook.html",
		},
		{
			// The reviewed change: an escaping .md (repo-root README) becomes a GitHub
			// URL, NOT README.html — it is no longer collapsed onto the docs index.
			name: "escaping root README becomes GitHub URL", dest: "../README.md", srcDir: "docs",
			wantDest: gh + "README.md", wantRewrite: true, wantHas: false,
		},
		{
			name: "escaping root README with anchor keeps anchor on GitHub URL", dest: "../README.md#see-it-in-60-seconds", srcDir: "docs",
			wantDest: gh + "README.md#see-it-in-60-seconds", wantRewrite: true, wantHas: false,
		},
		{
			name: "non-md repo asset becomes GitHub URL", dest: "contracts/x.schema.json", srcDir: "docs",
			wantDest: gh + "docs/contracts/x.schema.json", wantRewrite: true, wantHas: false,
		},
		{
			name: "external https left untouched", dest: "https://example.com/x.md", srcDir: "docs",
			wantRewrite: false, wantHas: false,
		},
		{
			name: "same-page anchor recorded for self-validation", dest: "#local-anchor", srcDir: "docs",
			wantRewrite: false, wantHas: true, wantAnchor: "local-anchor", wantSelf: true,
		},
		{
			name: "dangling in-bundle md fails (recorded)", dest: "missing.md", srcDir: "docs",
			wantRewrite: false, wantHas: true, wantDangle: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDest, ref, rewrite, has := g.classifyLink(tc.dest, tc.srcDir)
			if rewrite != tc.wantRewrite {
				t.Errorf("rewrite = %v, want %v", rewrite, tc.wantRewrite)
			}
			if tc.wantRewrite && gotDest != tc.wantDest {
				t.Errorf("dest = %q, want %q", gotDest, tc.wantDest)
			}
			if has != tc.wantHas {
				t.Errorf("has = %v, want %v", has, tc.wantHas)
			}
			if ref.target != tc.wantTarget {
				t.Errorf("ref.target = %q, want %q", ref.target, tc.wantTarget)
			}
			if ref.anchor != tc.wantAnchor {
				t.Errorf("ref.anchor = %q, want %q", ref.anchor, tc.wantAnchor)
			}
			if ref.selfAnchor != tc.wantSelf {
				t.Errorf("ref.selfAnchor = %v, want %v", ref.selfAnchor, tc.wantSelf)
			}
			if ref.dangling != tc.wantDangle {
				t.Errorf("ref.dangling = %v, want %v", ref.dangling, tc.wantDangle)
			}
		})
	}
}

// TestGitHubSlug pins the GitHub-compatible slug rules that the goldmark default
// got wrong: '_' is PRESERVED (not rewritten to '-'), punctuation/emoji are
// dropped, and a removed inner char leaves the surrounding spaces (so an em-dash
// yields a double hyphen) — matching the anchors the docs' TOCs were authored for.
func TestGitHubSlug(t *testing.T) {
	cases := map[string]string{
		"GET /api/v1/quality_events":                                 "get-apiv1quality_events",
		"2. Six UNIQUE indexes gain tenant_id as the leading column": "2-six-unique-indexes-gain-tenant_id-as-the-leading-column",
		"Layer 1 — the short answer":                                 "layer-1--the-short-answer",
		// The emoji is dropped but its trailing space survives and becomes a leading
		// hyphen — this matches github-slugger (trim, strip specials, spaces->'-').
		"📌 Scope": "-scope",
	}
	for in, want := range cases {
		if got := githubSlug(in); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRun_StripsScript is the non-negotiable XSS assertion, run end-to-end: a
// markdown source containing a raw <script> must NOT yield a live <script> in the
// committed output (two independent defences: raw-HTML off + bluemonday).
func TestRun_StripsScript(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(in, "README.md"), "# Title\n\nHello <script>alert(1)</script> world.\n")

	if err := run(in, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := readFile(t, filepath.Join(out, "README.html"))
	lower := strings.ToLower(body)
	if strings.Contains(lower, "<script") {
		t.Errorf("output contains a live <script> tag:\n%s", body)
	}
	if strings.Contains(lower, "javascript:") {
		t.Errorf("output contains a javascript: URI:\n%s", body)
	}
}

// TestRun_FailsOnDanglingLink asserts the integrity check fails (non-zero) on an
// in-bundle .md link with no rendered target, and writes NO output.
func TestRun_FailsOnDanglingLink(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(in, "README.md"), "# Index\n\nSee [gone](nonexistent.md).\n")

	err := run(in, out)
	if err == nil || !strings.Contains(err.Error(), "not a rendered doc") {
		t.Fatalf("run err = %v, want a dangling-link integrity error", err)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("output dir has %d entries after a failed run, want 0", len(entries))
	}
}

// TestRun_FailsOnDeadCrossPageAnchor asserts the anchor-integrity check fails when
// an in-bundle link targets a #anchor that is not a heading id on the target page.
func TestRun_FailsOnDeadCrossPageAnchor(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	// README (index) links to a real page but a non-existent anchor on it.
	writeFile(t, filepath.Join(in, "README.md"), "# Index\n\nJump to [x](other.md#no-such-heading).\n")
	writeFile(t, filepath.Join(in, "other.md"), "# Other\n\n## Real Heading\n")

	err := run(in, out)
	if err == nil || !strings.Contains(err.Error(), "anchor #no-such-heading not found") {
		t.Fatalf("run err = %v, want a dead-anchor integrity error", err)
	}
}

// TestRun_FailsOnOrphanPage asserts a rendered page that no other page links to
// (and is not the index) fails the build.
func TestRun_FailsOnOrphanPage(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(in, "README.md"), "# Index\n\nNothing links to the other page.\n")
	writeFile(t, filepath.Join(in, "orphan.md"), "# Orphan\n")

	err := run(in, out)
	if err == nil || !strings.Contains(err.Error(), "orphan page: orphan.html") {
		t.Fatalf("run err = %v, want an orphan-page integrity error", err)
	}
}

// TestRun_ResolvesValidLinks is the positive path: a self-consistent set (mutual
// links + a valid cross-page anchor) renders and produces both outputs, and the
// escaping ../README.md lands as a GitHub URL in the committed HTML.
func TestRun_ResolvesValidLinks(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(in, "README.md"),
		"# Home\n\nGo to [other](other.md#section-two) or the [repo root](../README.md).\n")
	writeFile(t, filepath.Join(in, "other.md"),
		"# Other\n\n## Section Two\n\nBack to [home](README.md).\n")

	if err := run(in, out); err != nil {
		t.Fatalf("run failed on a valid doc set: %v", err)
	}
	for _, name := range []string{"README.html", "other.html"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("expected output %s: %v", name, err)
		}
	}
	// The escaping ../README.md became a GitHub URL (not collapsed onto README.html).
	home := readFile(t, filepath.Join(out, "README.html"))
	if !strings.Contains(home, "https://github.com/tiermetric/tier/blob/main/README.md") {
		t.Error("escaping ../README.md should have been rewritten to a GitHub blob URL")
	}
}

// TestRun_PrunesOrphanedOutput asserts run() removes a stale output file that no
// longer corresponds to any source (the pruning that keeps the committed set
// exactly equal to a fresh render).
func TestRun_PrunesOrphanedOutput(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(in, "README.md"), "# Index\n")
	// A stale file from a previous render whose source has since been deleted.
	writeFile(t, filepath.Join(out, "deleted-doc.html"), "<!doctype html>stale")

	if err := run(in, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "deleted-doc.html")); !os.IsNotExist(err) {
		t.Errorf("stale output deleted-doc.html should have been pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "README.html")); err != nil {
		t.Errorf("README.html should exist after prune+write: %v", err)
	}
}

// TestCollectDocs_DetectsCollision pins that two sources collapsing to the same
// flat output name fail loudly rather than silently overwriting one doc.
func TestCollectDocs_DetectsCollision(t *testing.T) {
	in := t.TempDir()
	if err := os.MkdirAll(filepath.Join(in, "internals"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(in, "guide.md"), "# Guide\n")
	writeFile(t, filepath.Join(in, "internals", "guide.md"), "# Internal Guide\n")

	if _, err := collectDocs(in); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("collectDocs err = %v, want a basename-collision error", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
