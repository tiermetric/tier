package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rootPricesFlagRE matches an instruction to pass --prices to BARE tierd:
// `tierd` followed directly by the flag (either dash form, the flag package
// accepts both). `tierd serve --prices` does not match; `./tierd --prices` and
// `/usr/local/bin/tierd --prices` do.
var rootPricesFlagRE = regexp.MustCompile(`\btierd[ \t]+--?prices\b`)

// rootPricesFlagScanRoots is every operator-facing surface the #801 guard
// reads. A directory is walked recursively.
// Known blind spots, because the scan is one line at a time:
//   - a shell `\` continuation that puts --prices on the next line after tierd;
//   - the docker ENTRYPOINT form, `docker run <image> --prices`, which never names tierd.
var rootPricesFlagScanRoots = []string{
	"../../docs",
	"../../README.md",
	"../../config.example.yaml",
	"../../deploy",
	"../../internal/store/prices.yaml",
	"../../internal/store/prices.go",
}

// rootPricesFlagPendingDocs exempts ONE known line per entry: the two
// occurrences PR #798 (docs/797-explain-inputs) rewrites. They are left alone
// here so the two PRs do not conflict. Each entry names the file AND a fragment
// unique to that exact sentence, so any NEW occurrence in these files still
// fails. Once #798 is on main the fragment no longer appears, the entry exempts
// nothing, and the test logs it as removable; it does not fail, so the check
// passes on main before AND after #798 merges, in either merge order.
var rootPricesFlagPendingDocs = []struct{ file, fragment string }{
	{"docs/how-it-works.md", "overridable at runtime with `tierd --prices /path/to/prices.yaml` (an override must not reuse"},
	{"docs/open-weights-capture.md", "a rate has drifted, override it with `tierd --prices /path/to/prices.yaml` (a full copy"},
}

// rootPricesFlagExemption returns the index of the rootPricesFlagPendingDocs
// entry that exempts line in file rel, or -1. An entry exempts only a line of
// its own file that contains its fragment.
func rootPricesFlagExemption(rel, line string) int {
	for i, p := range rootPricesFlagPendingDocs {
		if p.file == rel && strings.Contains(line, p.fragment) {
			return i
		}
	}
	return -1
}

// TestDocs_NoRootLevelPricesFlag pins #801: `--prices` is a flag of a
// subcommand (serve, score, ship, score-log, ...), never of bare tierd, so no
// doc, example or price-table comment may tell an operator to run
// `tierd --prices`. It fails naming every offending file:line.
func TestDocs_NoRootLevelPricesFlag(t *testing.T) {
	// Premise: the sentence this guard forbids really is refused. If bare
	// `tierd --prices` ever becomes valid, this arm fails and the guard below
	// should be retired rather than kept as a rule about nothing.
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"--prices", "x.yaml"}, &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "unknown command: --prices") {
		t.Fatalf("premise: `tierd --prices` = exit %d, stderr %q; want exit 1 with %q",
			code, errOut.String(), "unknown command: --prices")
	}

	// Controls: the scan below passes when it finds nothing, so prove the
	// matcher and the exemption it uses can each still fire and still refuse.
	t.Run("controls", func(t *testing.T) {
		for _, tc := range []struct {
			line string
			want bool
		}{
			{"`tierd --prices x`", true},
			{"tierd -prices", true},
			{"./tierd  --prices", true},
			{"tierd\t--prices=x", true},
			{"/usr/local/bin/tierd --prices", true},
			{"tierd serve --prices", false},
			{"tierd serve -prices", false},
			{"tierd  serve --prices", false},
		} {
			if got := rootPricesFlagRE.MatchString(tc.line); got != tc.want {
				t.Errorf("rootPricesFlagRE.MatchString(%q) = %v, want %v", tc.line, got, tc.want)
			}
		}
		if len(rootPricesFlagPendingDocs) == 0 {
			t.Log("no pending-doc exemptions left; exemption controls have nothing to exercise")
			return
		}
		p := rootPricesFlagPendingDocs[0]
		if got := rootPricesFlagExemption(p.file, "run `tierd --prices x` "+p.fragment); got != 0 {
			t.Errorf("exemption(%s, line WITH its fragment) = %d, want 0", p.file, got)
		}
		if got := rootPricesFlagExemption(p.file, "run `tierd --prices x` here"); got != -1 {
			t.Errorf("exemption(%s, line WITHOUT its fragment) = %d, want -1: an entry must not exempt its whole file", p.file, got)
		}
		if got := rootPricesFlagExemption("docs/other.md", p.fragment); got != -1 {
			t.Errorf("exemption(docs/other.md, %s's fragment) = %d, want -1: an entry must not exempt other files", p.file, got)
		}
	})

	used := make([]bool, len(rootPricesFlagPendingDocs))
	var hits []string
	scanned := 0
	scan := func(path string) {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer func() { _ = f.Close() }()
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if !rootPricesFlagRE.MatchString(line) {
				continue
			}
			if i := rootPricesFlagExemption(rel, line); i >= 0 {
				used[i] = true
			} else {
				hits = append(hits, rel+":"+strconv.Itoa(n)+": "+strings.TrimSpace(line))
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		scanned++
	}
	for _, root := range rootPricesFlagScanRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				scan(path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// Denominator: a scan that silently read nothing would pass vacuously.
	if scanned < len(rootPricesFlagScanRoots) {
		t.Fatalf("scanned %d files, want at least %d (one per scan root)", scanned, len(rootPricesFlagScanRoots))
	}
	for _, h := range hits {
		t.Errorf("%s\n\t`--prices` is not a root flag (`tierd --prices` exits 1: unknown command); name the subcommand, e.g. `tierd serve --prices <file>`", h)
	}
	for i, p := range rootPricesFlagPendingDocs {
		if !used[i] {
			t.Logf("pending-doc exemption for %s no longer matches any line (PR #798 has landed); it can be deleted", p.file)
		}
	}
}
