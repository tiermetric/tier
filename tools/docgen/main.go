// Command docgen renders the project's markdown documentation (docs/*.md and
// docs/internals/*.md) into the static HTML committed under
// internal/docs/html/*.html, which tierd then embeds and serves at /docs/.
//
// WHY a build-time generator (and not a runtime markdown renderer): the release
// binary must not carry a markdown engine or an HTML sanitiser. Rendering here,
// at build time, keeps goldmark + bluemonday in THIS nested module only (see
// go.mod) — they never enter the parent module's dependency graph or the shipped
// binary. The generated HTML is committed and a Makefile no-drift gate
// (docs-html-check) fails the build if the committed output ever diverges from a
// fresh render, so "single-source markdown" cannot silently rot.
//
// SECURITY posture (defence in depth — the served docs carry NO JavaScript):
//   - goldmark runs with raw-HTML rendering OFF (html.WithUnsafe is deliberately
//     NOT set), so any inline HTML in a markdown source is escaped to inert text,
//     not emitted as live markup.
//   - The rendered fragment is then passed through a bluemonday UGCPolicy(), a
//     second, independent allow-list pass. A <script> that somehow reached the
//     fragment would be stripped here even if the first line of defence regressed.
//   - The shared layout adds no <script>; the docs handler serves a CSP with no
//     script-src (it falls through to default-src 'none'), so no JS can execute.
//
// LINK model and what the integrity pass ACTUALLY guarantees (input boundary stays
// at docs/ — the generator never reads the repo root or repo assets):
//   - Every markdown link is resolved RELATIVE TO ITS SOURCE FILE to a
//     repo-relative path. Output is a FLAT directory (internal/docs/html/*.html).
//   - An IN-BUNDLE target (a .md file that IS rendered into the output set) is
//     rewritten to its .html basename, preserving any #anchor. Both the target
//     page AND the #anchor are checked: the build FAILS if the anchor does not
//     resolve to a heading id on the target page (goldmark auto heading ids).
//   - A link that resolves to a .md UNDER docs/ that is NOT rendered is a genuine
//     dangling in-bundle link and FAILS the build.
//   - An ESCAPING link (a repo path outside docs/, e.g. the root README.md) or a
//     NON-.md repo asset (e.g. docs/contracts/*.json) is rewritten to a canonical
//     GitHub blob URL (the public repo is live, so it resolves) and is thereafter
//     treated as external — not anchor-checked here, because those files live in
//     the repo, not in this bundle.
//   - Already-external http(s):// and mailto: links are left untouched.
//   - ORPHAN check: every rendered page except the index (README.html) must be the
//     link target of at least one OTHER in-bundle page; a served-but-unlinked page
//     FAILS the build.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// indexPage is the flat output name treated as the docs index. It is exempt from
// the orphan check (nothing needs to link TO the index) and is what the handler
// serves at /docs/.
const indexPage = "README.html"

// ghBlobBase is the canonical GitHub blob URL prefix escaping/non-.md repo links
// are rewritten to. The public repo is live, so these resolve.
const ghBlobBase = "https://github.com/tiermetric/tier/blob/main/"

// main parses flags, runs the generator, and maps any error to a non-zero exit
// so the Makefile gate (and a human) sees a hard failure rather than a silent
// partial render.
func main() {
	inDir := flag.String("in", "docs", "input documentation directory (reads <in>/*.md and <in>/internals/*.md)")
	outDir := flag.String("out", "internal/docs/html", "output directory for generated *.html")
	flag.Parse()

	if err := run(*inDir, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
}

// docFile is one markdown source paired with the flat output name it renders to
// and its repo-relative path (used to resolve links that originate in it).
type docFile struct {
	srcPath  string // filesystem path, e.g. docs/internals/tenancy-retrofit-playbook.md
	repoPath string // repo-relative path, e.g. docs/internals/tenancy-retrofit-playbook.md
	outName  string // flat output name, e.g. tenancy-retrofit-playbook.html
	relLabel string // human label for errors, e.g. internals/tenancy-retrofit-playbook.md
}

// generator holds the immutable configuration shared across every file render:
// the markdown engine, the sanitiser, and the bundle map (repo-relative .md path
// -> flat output name) that classifies each link as in-bundle vs escaping.
type generator struct {
	md     goldmark.Markdown
	policy *bluemonday.Policy
	bundle map[string]string // repoPath(.md) -> outName(.html)
	// docsRoot is the repo-relative directory the input tree maps to ("docs").
	docsRoot string
}

// run is the whole generation pipeline. Ordering is deliberate and load-bearing:
// render every file into memory, collect per-page heading ids, run ALL integrity
// checks (dangling links, unresolved anchors, orphan pages), and only once every
// check passes prune + rewrite the output directory. A failure therefore never
// leaves a partial or drift-inducing write, and pruning kills pages orphaned by a
// since-deleted/renamed source.
func run(inDir, outDir string) error {
	files, err := collectDocs(inDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no markdown files found under %q", inDir)
	}

	g := &generator{
		md:       newMarkdown(),
		policy:   docsPolicy(),
		bundle:   make(map[string]string, len(files)),
		docsRoot: repoRootDir(inDir),
	}
	for _, f := range files {
		g.bundle[f.repoPath] = f.outName
	}

	rendered := make(map[string][]byte, len(files))
	headingIDs := make(map[string]map[string]bool, len(files)) // outName -> set of heading ids
	var allLinks []linkRef
	for _, f := range files {
		src, readErr := os.ReadFile(f.srcPath)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", f.srcPath, readErr)
		}
		html, links, ids, renderErr := g.renderDoc(f, src)
		if renderErr != nil {
			return fmt.Errorf("render %s: %w", f.srcPath, renderErr)
		}
		rendered[f.outName] = html
		headingIDs[f.outName] = ids
		allLinks = append(allLinks, links...)
	}

	if errs := checkIntegrity(files, allLinks, headingIDs); len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("link-integrity check failed (%d problem(s)):\n%s",
			len(errs), strings.Join(errs, "\n"))
	}

	// Integrity passed: safe to destroy and rewrite the committed output so it is
	// EXACTLY the current render (no orphaned files from deleted/renamed docs).
	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("prune output dir %s: %w", outDir, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %s: %w", outDir, err)
	}
	names := make([]string, 0, len(rendered))
	for name := range rendered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(outDir, name), rendered[name], 0o644); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(outDir, name), err)
		}
	}
	fmt.Printf("docgen: rendered %d file(s) to %s\n", len(names), outDir)
	return nil
}

// repoRootDir returns the repo-relative directory the input tree maps to. docs/
// always lives at <repo>/docs, so the basename of the (cleaned) -in path is the
// repo-relative root regardless of how -in was passed (docs, ../../docs, an
// absolute path). Forward slashes throughout so it composes with path.Join.
func repoRootDir(inDir string) string {
	return path.Base(filepath.ToSlash(filepath.Clean(inDir)))
}

// collectDocs enumerates the input files in a DETERMINISTIC order: every
// <in>/*.md plus every <in>/internals/*.md. It fails if two inputs would collapse
// to the same flat output name (a basename collision), because the flat output
// model cannot represent both and silently overwriting one would drop a doc.
func collectDocs(inDir string) ([]docFile, error) {
	root := repoRootDir(inDir)
	var files []docFile
	seen := make(map[string]string) // outName -> first srcPath, for collision detection

	add := func(srcPath, relLabel, repoPath string) error {
		outName := strings.TrimSuffix(filepath.Base(srcPath), ".md") + ".html"
		if prev, dup := seen[outName]; dup {
			return fmt.Errorf("output name collision: %s and %s both map to %s (flat output cannot hold both)",
				prev, srcPath, outName)
		}
		seen[outName] = srcPath
		files = append(files, docFile{srcPath: srcPath, repoPath: repoPath, outName: outName, relLabel: relLabel})
		return nil
	}

	top, err := filepath.Glob(filepath.Join(inDir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/*.md: %w", inDir, err)
	}
	sort.Strings(top)
	for _, p := range top {
		base := filepath.Base(p)
		if isPublishArtifact(base) {
			continue
		}
		if err := add(p, base, path.Join(root, base)); err != nil {
			return nil, err
		}
	}

	internal, err := filepath.Glob(filepath.Join(inDir, "internals", "*.md"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/internals/*.md: %w", inDir, err)
	}
	sort.Strings(internal)
	for _, p := range internal {
		base := filepath.Base(p)
		if err := add(p, path.Join("internals", base), path.Join(root, "internals", base)); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// isPublishArtifact reports whether a docs/ entry is a build input rather than a
// reader page. Leading-underscore files are skipped and never rendered.
//
// Why this exists (#544). docs/_public-CLAUDE.md is the slim CLAUDE.md that
// PUBLISHING.md installs at the root of the public export. It lived in docs/ as a
// plain .md, so docgen rendered it — and docgen enforces link integrity in BOTH
// directions: a rendered page with no inbound link is an ORPHAN, and a link to a
// file the export does not ship DANGLES. That made three requirements mutually
// unsatisfiable, because publish-audit gate (b) forbids the file from shipping at
// all:
//
//	rendered  => must be linked  => the link must resolve in the export
//	            => the file must ship  => gate (b) rejects the export
//
// The published tree really did end up unbuildable this way: `make check` inside
// the export failed docgen's own link check. The resolution is that a publish
// artifact must not be a rendered page in the first place. The underscore is the
// marker; it is deliberately a CONVENTION rather than a hardcoded filename, so the
// next build input dropped into docs/ does not re-open the same trap.

func isPublishArtifact(base string) bool { return strings.HasPrefix(base, "_") }

// newMarkdown builds the goldmark renderer used for every file.
//
// SECURITY-CRITICAL configuration:
//   - extension.GFM enables tables, autolinks and strikethrough (the docs use
//     tables heavily).
//   - html.WithUnsafe() is deliberately ABSENT: raw HTML in a source is escaped
//     to inert text, so a markdown author cannot inject live markup. Do not add
//     WithUnsafe here — the bluemonday pass is a backstop, not a licence to enable
//     unsafe rendering.
//   - parser.WithAutoHeadingID() emits GitHub-style heading ids so the docs'
//     in-page #anchor cross-links resolve (and so the anchor-integrity pass has
//     ids to validate against).
func newMarkdown() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
}

// docsPolicy returns the bluemonday policy applied to every rendered fragment.
//
// It starts from UGCPolicy() (a conservative allow-list) and widens it exactly
// enough for generated docs:
//   - AllowRelativeURLs(true): in-bundle links are relative (conventions.html,
//     README.html#anchor); without this UGCPolicy would strip them, breaking every
//     cross-link and in-page anchor. (Escaping links became absolute GitHub URLs
//     and are allowed as standard https.)
//   - id/name attributes: heading anchors (auto heading ids) and their fragment
//     targets need id/name to survive sanitisation. Values are generator-produced
//     slugs over our own trusted docs, and the docs handler serves default-src
//     'none' with no script-src, so DOM-clobbering has no script to influence.
//
// Anchor href schemes remain UGCPolicy-restricted (http/https/mailto + relative),
// so javascript:/data: hrefs are still stripped.
func docsPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowRelativeURLs(true)
	p.AllowAttrs("id").Globally()
	p.AllowAttrs("name").OnElements("a")
	return p
}

// linkRef records one link discovered on a page, for the post-render integrity
// checks. Only links that need checking are recorded: in-bundle links (target +
// optional anchor), same-page anchors (selfAnchor), and dangling in-bundle .md
// links (dangling). Escaping/external links are not recorded.
type linkRef struct {
	onPage     string // outName of the page the link is ON
	original   string // original markdown destination (for error messages)
	target     string // in-bundle target outName (empty for a pure same-page anchor)
	anchor     string // fragment WITHOUT '#', or "" if none
	selfAnchor bool   // true = same-page "#anchor" (target is onPage; not an orphan link)
	dangling   bool   // true = resolved to a .md under docs/ that is not rendered
	resolved   string // the repo path that failed to resolve (dangling only)
}

// renderDoc renders one markdown source to a full, sanitised, layout-wrapped HTML
// page. It returns the page bytes, the links it discovered (for integrity), and
// the set of heading ids present on the page (for anchor validation of links that
// TARGET this page).
func (g *generator) renderDoc(f docFile, src []byte) ([]byte, []linkRef, map[string]bool, error) {
	reader := text.NewReader(src)
	// Parse with a GitHub-compatible heading-id generator (see githubIDs): the docs'
	// tables of contents were authored against GitHub's slug algorithm, which
	// preserves underscores and Unicode letters where goldmark's default rewrites
	// them. A fresh generator per file gives per-document duplicate-slug numbering,
	// exactly as GitHub does.
	ctx := parser.NewContext(parser.WithIDs(newGitHubIDs()))
	doc := g.md.Parser().Parse(reader, parser.WithContext(ctx))
	srcDir := path.Dir(f.repoPath)

	title := ""
	ids := map[string]bool{}
	var links []linkRef
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := n.(type) {
		case *ast.Heading:
			if id, ok := headingID(node); ok {
				ids[id] = true
			}
			if node.Level == 1 && title == "" {
				title = strings.TrimSpace(nodeText(node, src))
			}
		case *ast.Link:
			newDest, ref, rewrite, has := g.classifyLink(string(node.Destination), srcDir)
			if rewrite {
				node.Destination = []byte(newDest)
			}
			if has {
				ref.onPage = f.outName
				links = append(links, ref)
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("walk AST: %w", err)
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(f.srcPath), ".md")
	}

	var frag bytes.Buffer
	if err := g.md.Renderer().Render(&frag, src, doc); err != nil {
		return nil, nil, nil, fmt.Errorf("render HTML: %w", err)
	}
	safe := g.policy.SanitizeBytes(frag.Bytes())

	page, err := wrapLayout(title, template.HTML(safe)) //nolint:gosec // safe is bluemonday-sanitised
	if err != nil {
		return nil, nil, nil, err
	}
	return page, links, ids, nil
}

// classifyLink resolves a markdown link destination relative to its source
// directory and decides how to rewrite it, returning:
//
//	newDest — the rewritten destination (valid only when rewrite is true)
//	ref     — the integrity record (valid only when has is true; onPage unset)
//	rewrite — whether node.Destination should be replaced with newDest
//	has     — whether ref should be recorded for the integrity pass
//
// See the package doc for the full rule table. srcDir is the source file's
// repo-relative directory (e.g. "docs" or "docs/internals").
func (g *generator) classifyLink(dest, srcDir string) (newDest string, ref linkRef, rewrite, has bool) {
	lower := strings.ToLower(dest)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "mailto:") {
		return "", linkRef{}, false, false // already external
	}

	pathPart, frag := dest, ""
	if i := strings.IndexByte(dest, '#'); i >= 0 {
		pathPart, frag = dest[:i], dest[i:]
	}
	anchor := strings.TrimPrefix(frag, "#")

	// Pure same-page anchor ("#section"): validate against THIS page's ids.
	if pathPart == "" {
		if anchor == "" {
			return "", linkRef{}, false, false // empty/degenerate link, nothing to check
		}
		return "", linkRef{selfAnchor: true, anchor: anchor, original: dest}, false, true
	}

	repoPath := path.Clean(path.Join(srcDir, pathPart))
	isMD := strings.HasSuffix(strings.ToLower(repoPath), ".md")

	if isMD {
		if out, ok := g.bundle[repoPath]; ok {
			nd := out
			if anchor != "" {
				nd = out + "#" + anchor
			}
			return nd, linkRef{target: out, anchor: anchor, original: dest}, true, true
		}
		// A .md that resolves INSIDE the docs input tree but is not rendered is a
		// genuine dangling in-bundle link — a typo or a since-deleted doc.
		if repoPath == g.docsRoot || strings.HasPrefix(repoPath, g.docsRoot+"/") {
			return "", linkRef{dangling: true, resolved: repoPath, original: dest}, false, true
		}
		// An escaping .md (outside docs/, e.g. the repo-root README.md): link to
		// where it lives in the repo. External thereafter — not anchor-checked here.
		return ghBlobBase + repoPath + frag, linkRef{}, true, false
	}

	// Any non-.md repo path (assets like docs/contracts/*.json): GitHub URL.
	return ghBlobBase + repoPath + frag, linkRef{}, true, false
}

// checkIntegrity runs every post-render check and returns a (possibly empty)
// slice of human-readable problems. It is pure: it reads the rendered set, the
// discovered links, and the per-page heading ids; it mutates nothing.
//
// Checks:
//   - dangling: an in-bundle .md link that has no rendered page.
//   - anchor: an in-bundle (or same-page) #anchor with no matching heading id on
//     the target page.
//   - orphan: a rendered page (other than the index) that no OTHER page links to.
func checkIntegrity(files []docFile, links []linkRef, headingIDs map[string]map[string]bool) []string {
	var errs []string
	linkedTargets := map[string]bool{}

	for _, l := range links {
		if l.dangling {
			errs = append(errs, fmt.Sprintf(
				"  %s: link %q resolves to %q, which is not a rendered doc", l.onPage, l.original, l.resolved))
			continue
		}
		target := l.target
		if l.selfAnchor {
			target = l.onPage
		}
		if l.anchor != "" {
			if !headingIDs[target][l.anchor] {
				errs = append(errs, fmt.Sprintf(
					"  %s: link %q -> anchor #%s not found on %s", l.onPage, l.original, l.anchor, target))
			}
		}
		// Only a cross-page in-bundle link counts toward reachability (a page's own
		// table-of-contents self-anchors must not make it its own parent).
		if !l.selfAnchor && l.target != "" {
			linkedTargets[l.target] = true
		}
	}

	for _, f := range files {
		if f.outName == indexPage {
			continue // the index needs no inbound link
		}
		if !linkedTargets[f.outName] {
			errs = append(errs, fmt.Sprintf(
				"  orphan page: %s is rendered but no other doc links to it (add a link, e.g. in the index)", f.outName))
		}
	}
	return errs
}

// githubIDs is a goldmark parser.IDs that generates GitHub-compatible heading
// slugs, so the anchors the docs' tables of contents were authored against (on
// GitHub) resolve against the rendered ids. It differs from goldmark's default in
// two ways that caused real mismatches: it PRESERVES '_' (default rewrites it to
// '-', so "quality_events" became "quality-events") and it keeps Unicode letters
// (default drops any multi-byte rune). One instance is used per document; the
// used-set gives GitHub's "-1", "-2" suffixing for duplicate slugs in order.
type githubIDs struct {
	used map[string]bool
}

// newGitHubIDs returns a fresh per-document id generator.
func newGitHubIDs() *githubIDs { return &githubIDs{used: map[string]bool{}} }

// Generate implements parser.IDs. It slugs value, falls back to a kind-based stub
// when the slug is empty, and de-duplicates with an "-N" suffix (1-indexed on the
// second occurrence), matching GitHub.
func (g *githubIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	base := githubSlug(string(value))
	if base == "" {
		if kind == ast.KindHeading {
			base = "heading"
		} else {
			base = "id"
		}
	}
	slug := base
	for i := 1; g.used[slug]; i++ {
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	g.used[slug] = true
	return []byte(slug)
}

// Put implements parser.IDs, recording an explicitly-authored id as used.
func (g *githubIDs) Put(value []byte) { g.used[string(value)] = true }

// githubSlug converts heading text to a GitHub-style anchor slug: trim, lowercase
// letters/numbers (Unicode-aware), map any whitespace run element to '-', keep '_'
// and '-' verbatim, and drop every other character (punctuation, emoji). This
// matches github-slugger closely enough for our documentation set.
func githubSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsSpace(r):
			b.WriteByte('-')
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(unicode.ToLower(r))
		case r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// headingID returns the auto-generated id attribute of a heading node, if the
// parser assigned one. goldmark stores it as a []byte attribute value.
func headingID(h *ast.Heading) (string, bool) {
	v, ok := h.AttributeString("id")
	if !ok {
		return "", false
	}
	switch id := v.(type) {
	case []byte:
		return string(id), true
	case string:
		return id, true
	default:
		return "", false
	}
}

// nodeText concatenates the plain-text content of an inline subtree (used to
// derive a page title from its first H1). It walks only ast.Text/ast.String leaf
// segments, so formatting marks (emphasis, code spans) contribute their text
// without their markup.
func nodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(source))
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// layoutTmpl is the single shared page shell wrapped around every doc fragment.
// It is trusted, generator-authored HTML (never user input) and carries NO
// <script>. The inline <style> supports light and dark via prefers-color-scheme
// and is intentionally minimal and brand-neutral. {{.Title}} and link text are
// auto-escaped by html/template; {{.Body}} is a template.HTML because it has
// already been sanitised by bluemonday.
var layoutTmpl = template.Must(template.New("doc").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — TIER docs</title>
<style>
:root {
  --bg: #ffffff; --fg: #1a1d21; --muted: #5c636b; --border: #e2e5e9;
  --link: #0b6bcb; --code-bg: #f4f6f8; --accent: #0b6bcb;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14171a; --fg: #e6e9ec; --muted: #9aa2ab; --border: #2a2f36;
    --link: #6db3f2; --code-bg: #1d2126; --accent: #6db3f2;
  }
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body {
  margin: 0; background: var(--bg); color: var(--fg);
  font: 16px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
}
.wrap { max-width: 760px; margin: 0 auto; padding: 0 20px 64px; }
header.site {
  border-bottom: 1px solid var(--border); margin-bottom: 32px;
}
header.site .bar { max-width: 760px; margin: 0 auto; padding: 14px 20px; }
header.site a.brand { color: var(--fg); text-decoration: none; font-weight: 700; letter-spacing: .01em; }
header.site a.brand .tier { color: var(--accent); }
main { overflow-wrap: break-word; }
h1, h2, h3, h4 { line-height: 1.25; margin-top: 1.8em; }
h1 { margin-top: .4em; font-size: 1.9rem; }
a { color: var(--link); }
p, li { color: var(--fg); }
code {
  background: var(--code-bg); padding: .15em .4em; border-radius: 4px;
  font: .875em/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
pre {
  background: var(--code-bg); padding: 14px 16px; border-radius: 8px;
  overflow-x: auto; border: 1px solid var(--border);
}
pre code { background: none; padding: 0; }
blockquote {
  margin: 1.2em 0; padding: .2em 1em; color: var(--muted);
  border-left: 3px solid var(--border);
}
table { border-collapse: collapse; width: 100%; display: block; overflow-x: auto; }
th, td { border: 1px solid var(--border); padding: 8px 12px; text-align: left; }
th { background: var(--code-bg); }
img { max-width: 100%; height: auto; }
hr { border: none; border-top: 1px solid var(--border); margin: 2em 0; }
footer.site {
  max-width: 760px; margin: 48px auto 0; padding: 20px;
  border-top: 1px solid var(--border); color: var(--muted); font-size: .875rem;
}
footer.site a { color: var(--muted); }
</style>
</head>
<body>
<header class="site"><div class="bar"><a class="brand" href="README.html"><span class="tier">TIER</span> docs</a></div></header>
<div class="wrap"><main>
{{.Body}}
</main></div>
<footer class="site">Generated from the TIER markdown documentation. <a href="README.html">Back to the docs index</a>.</footer>
</body>
</html>
`))

// layoutData is the template payload for layoutTmpl.
type layoutData struct {
	Title string
	Body  template.HTML
}

// wrapLayout renders a sanitised fragment into the shared page shell.
func wrapLayout(title string, body template.HTML) ([]byte, error) {
	var buf bytes.Buffer
	if err := layoutTmpl.Execute(&buf, layoutData{Title: title, Body: body}); err != nil {
		return nil, fmt.Errorf("execute layout: %w", err)
	}
	return buf.Bytes(), nil
}
