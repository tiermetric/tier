package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tiermetric/tier/internal/api"
)

func writeHierarchyCSV(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "team.csv")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// hierarchyCSVOfN is a valid file of n rows; row i sits on CSV line i+2.
func hierarchyCSVOfN(n int) string {
	var b strings.Builder
	b.WriteString("developer,team,division,org\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "dev-%05d,team-%d,,acme\n", i, i%7)
	}
	return b.String()
}

func runHierarchyForTest(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = dispatch(append([]string{"hierarchy"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// TestReadHierarchyCSV pins header, BOM, trimming, empty-cell and duplicate
// handling, and that each row carries its own CSV line number.
func TestReadHierarchyCSV(t *testing.T) {
	bom := "\xEF\xBB\xBF"
	tests := []struct {
		name      string
		in        string
		wantErr   string
		wantRows  []hierarchyImportItem
		wantLines []int
	}{
		{
			name: "empty division and org cells, trimmed, header case-insensitive and reordered",
			in:   " Team , DEVELOPER ,Division,org\nplatform, alice ,,\n  payments ,bob, core ,acme\n",
			wantRows: []hierarchyImportItem{
				{Developer: "alice", Team: "platform"},
				{Developer: "bob", Team: "payments", Division: "core", Org: "acme"},
			},
			wantLines: []int{2, 3},
		},
		{
			name:      "UTF-8 BOM before the header",
			in:        bom + "developer,team,division,org\nalice,platform,,\n",
			wantRows:  []hierarchyImportItem{{Developer: "alice", Team: "platform"}},
			wantLines: []int{2},
		},
		{
			name:      "UTF-8 BOM before a quoted header cell",
			in:        bom + "\"developer\",team,division,org\r\nalice,platform,,\r\n",
			wantRows:  []hierarchyImportItem{{Developer: "alice", Team: "platform"}},
			wantLines: []int{2},
		},
		{name: "unknown column named", in: "developer,team,division,org,email\nalice,p,,,a@x\n", wantErr: `line 1: unknown column "email"`},
		// #886: membership is dated by the server clock; a file cannot carry a date.
		{name: "valid_from column refused", in: "developer,team,division,org,valid_from\nalice,p,,,2020-01-01\n", wantErr: `line 1: unknown column "valid_from"`},
		{name: "valid_to column refused", in: "developer,team,division,org,valid_to\nalice,p,,,2020-01-01\n", wantErr: `line 1: unknown column "valid_to"`},
		{name: "semicolon export gets a hint", in: "developer;team;division;org\nalice;p;;\n", wantErr: "re-export with commas"},
		{name: "Windows-1252 byte is refused, not replaced", in: "developer,team,division,org\nalice,p,,\nm\xFCller,q,,\n", wantErr: "line 3: not UTF-8; re-export from your spreadsheet as CSV UTF-8"},
		{name: "missing column named", in: "developer,team,division\nalice,p,\n", wantErr: `line 1: missing column "org"`},
		{name: "column twice", in: "developer,team,team,org\nalice,p,p,\n", wantErr: `line 1: column "team" appears twice`},
		{name: "empty developer with line", in: "developer,team,division,org\nalice,p,,\n  ,q,,\n", wantErr: "line 3: developer is empty"},
		{name: "empty team with line", in: "developer,team,division,org\nalice, ,,\n", wantErr: `line 2: team is empty for developer "alice"`},
		{name: "duplicate developer names both lines", in: "developer,team,division,org\nalice,p,,\nbob,q,,\n alice ,r,,\n", wantErr: `line 4: developer "alice" is already assigned on line 2`},
		{name: "ragged row", in: "developer,team,division,org\nalice,p\n", wantErr: "wrong number of fields"},
		{name: "header only", in: "developer,team,division,org\n", wantErr: "no rows after the header"},
		{name: "empty file", in: "", wantErr: "file is empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := readHierarchyCSV(strings.NewReader(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(rows) != len(tc.wantRows) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tc.wantRows))
			}
			for i, r := range rows {
				if r.item != tc.wantRows[i] || r.line != tc.wantLines[i] {
					t.Errorf("row %d = %+v line %d, want %+v line %d", i, r.item, r.line, tc.wantRows[i], tc.wantLines[i])
				}
			}
		})
	}
}

// TestHierarchyImport_EndToEnd drives the command against the REAL handler and
// store: a file of exactly the row cap is one request the handler decodes (it
// uses DisallowUnknownFields) and every row must be stored as written.
func TestHierarchyImport_EndToEnd(t *testing.T) {
	const token = "hierarchy-test-token"
	srv, db := newShipTestServer(t, token)
	n := api.MaxHierarchyPerBatch
	path := writeHierarchyCSV(t, hierarchyCSVOfN(n))

	code, out, errOut := runHierarchyForTest("import", "--server", srv.URL, "--api-token", token, path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	if want := fmt.Sprintf("hierarchy import: %d rows written\n", n); out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	rows, err := db.ListHierarchy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("stored %d rows, want %d", len(rows), n)
	}
	last := rows[len(rows)-1]
	if want := fmt.Sprintf("dev-%05d", n-1); last.Developer != want || last.Team != fmt.Sprintf("team-%d", (n-1)%7) || last.Division != "" || last.Org != "acme" {
		t.Fatalf("last stored row = %+v, want developer %s in team-%d, org acme, empty division", last, want, (n-1)%7)
	}

	t.Run("no token is refused with a hint", func(t *testing.T) {
		t.Setenv("TIER_API_TOKEN", "")
		code, _, errOut := runHierarchyForTest("import", "--server", srv.URL, path)
		if code != 1 || !strings.Contains(errOut, "HTTP 401") || !strings.Contains(errOut, "admin token") {
			t.Fatalf("exit %d, stderr %q; want 1 with an HTTP 401 admin-token hint", code, errOut)
		}
	})
}

// TestHierarchyImport_Outcome pins what exit 1 claims: a 4xx wrote nothing
// (the import is all-or-nothing); a 500 or 503, which a proxy may produce after
// a commit, is never called unwritten; a gateway 502/504, a lost answer or a
// short 201 is UNKNOWN.
func TestHierarchyImport_Outcome(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", "")
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}
	}
	const nothing, unknown = "nothing was written", "outcome is UNKNOWN"
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    []string
	}{
		{"400 names the CSV line", status(http.StatusBadRequest, `{"error":"org_hierarchy[1]: developer is reserved"}`),
			[]string{nothing, "org_hierarchy[1] (CSV line 3): developer is reserved"}},
		{"500", status(http.StatusInternalServerError, `{"error":"store error"}`), []string{"HTTP 500"}},
		{"503", status(http.StatusServiceUnavailable, `{"error":"busy"}`), []string{"HTTP 503"}},
		{"502", status(http.StatusBadGateway, "bad gateway"), []string{unknown, "HTTP 502"}},
		{"504", status(http.StatusGatewayTimeout, "timeout"), []string{unknown, "HTTP 504"}},
		{"201 without every row", status(http.StatusCreated, `{"accepted":1}`), []string{unknown, "accepted 1 of 2 rows"}},
		{"lost answer", func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}, []string{unknown}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			code, out, errOut := runHierarchyForTest("import", "--server", srv.URL, writeHierarchyCSV(t, hierarchyCSVOfN(2)))
			if code != 1 || out != "" {
				t.Fatalf("exit %d stdout %q; want 1 and no success line", code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(errOut, w) {
					t.Errorf("stderr missing %q:\n%s", w, errOut)
				}
			}
			if strings.Contains(errOut, nothing) && strings.Contains(errOut, unknown) {
				t.Errorf("stderr claims both outcomes:\n%s", errOut)
			}
			if strings.HasPrefix(tc.name, "50") && strings.Contains(errOut, nothing) {
				t.Errorf("a 5xx may come from a proxy after a commit; stderr must not claim nothing was written:\n%s", errOut)
			}
		})
	}
}

// TestHierarchyImport_TooLargeIsRefused pins that a file over either cap is
// refused before any request, in a real run and a dry run alike: split across
// requests, an alias and its canonical id would silently overwrite each other.
func TestHierarchyImport_TooLargeIsRefused(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", "")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	// Ten rows, far under the row cap, whose team names total over 1 MiB.
	var big strings.Builder
	big.WriteString("developer,team,division,org\n")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&big, "d%d,%s,,\n", i, strings.Repeat("t", 110_000))
	}
	tests := []struct {
		name, csv, want string
	}{
		{"one row over the row cap", hierarchyCSVOfN(api.MaxHierarchyPerBatch + 1),
			fmt.Sprintf("has %d rows; one import holds at most %d", api.MaxHierarchyPerBatch+1, api.MaxHierarchyPerBatch)},
		{"ten rows over the byte cap", big.String(), fmt.Sprintf("bytes; one import holds at most %d", api.MaxHierarchyBody)},
	}
	for _, tc := range tests {
		path := writeHierarchyCSV(t, tc.csv)
		for _, args := range [][]string{{"--server", srv.URL}, {"--dry-run"}} {
			t.Run(tc.name+" "+args[0], func(t *testing.T) {
				code, out, errOut := runHierarchyForTest(append(append([]string{"import"}, args...), path)...)
				if code != 1 || out != "" || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "split it into smaller files") {
					t.Fatalf("exit %d stdout %q stderr %q; want 1 with %q and the split advice", code, out, errOut, tc.want)
				}
			})
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server saw %d requests, want 0: an oversized file must be refused before sending", hits.Load())
	}
}

// TestHierarchyImport_ServerURL pins the --server check, run before the CSV is
// read: a token never goes over http to a non-loopback host, loopback http
// works, a schemeless address is refused, and tokenless http to a remote host
// is allowed with ship's cleartext warning.
func TestHierarchyImport_ServerURL(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", "")
	var hits atomic.Int32
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"accepted":1}`)
	}))
	defer srv.Close()
	missing := filepath.Join(t.TempDir(), "never-read.csv")

	for _, extra := range [][]string{nil, {"--dry-run"}} {
		args := append([]string{"import", "--server", "http://tier.example", "--api-token", "tok"}, extra...)
		code, _, errOut := runHierarchyForTest(append(args, missing)...)
		if code != 1 || !strings.Contains(errOut, "refusing to send the API token over plaintext http") || strings.Contains(errOut, "never-read.csv") {
			t.Fatalf("%v: exit %d stderr %q; want 1, refused before the CSV is opened", extra, code, errOut)
		}
	}

	code, _, errOut := runHierarchyForTest("import", "--server", strings.TrimPrefix(srv.URL, "http://"), "--api-token", "tok", missing)
	if code != 1 || !strings.Contains(errOut, "--server") || strings.Contains(errOut, "never-read.csv") {
		t.Fatalf("schemeless: exit %d stderr %q; want 1, refused before the CSV is opened", code, errOut)
	}
	if hits.Load() != 0 {
		t.Fatalf("server saw %d requests before any valid import, want 0", hits.Load())
	}

	path := writeHierarchyCSV(t, hierarchyCSVOfN(1))
	code, out, errOut := runHierarchyForTest("import", "--server", srv.URL+"/", "--api-token", "tok", path)
	if code != 0 || out != "hierarchy import: 1 rows written\n" || hits.Load() != 1 || gotAuth.Load() != "Bearer tok" {
		t.Fatalf("loopback http: exit %d stdout %q stderr %q hits %d auth %v; want 0, one request with the token", code, out, errOut, hits.Load(), gotAuth.Load())
	}

	code, _, errOut = runHierarchyForTest("import", "--dry-run", "--server", "http://tier.example", path)
	if code != 0 || !strings.Contains(errOut, "warning: sending over cleartext http to a non-loopback host") {
		t.Fatalf("tokenless remote http: exit %d stderr %q; want 0 with the cleartext warning", code, errOut)
	}
}

// TestHierarchyImport_DryRunSendsNothing pins that --dry-run never contacts
// the server, even when one is named, and needs no --server at all.
func TestHierarchyImport_DryRunSendsNothing(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", "")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	path := writeHierarchyCSV(t, hierarchyCSVOfN(2))
	want := "hierarchy import: dry run: 2 rows valid (139 bytes); nothing sent\n"
	for _, args := range [][]string{
		{"import", "--dry-run", "--server", srv.URL, path},
		{"import", path, "--dry-run"}, // flags after the file are honoured, not dropped
	} {
		code, out, errOut := runHierarchyForTest(args...)
		if code != 0 || out != want {
			t.Fatalf("%v: exit %d stdout %q stderr %q; want 0 and %q", args, code, out, errOut, want)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("dry run sent %d requests, want 0", hits.Load())
	}
}

// TestHierarchyCmd_UsageAndArgs pins the usage paths and argument refusals.
func TestHierarchyCmd_UsageAndArgs(t *testing.T) {
	t.Setenv("TIER_API_TOKEN", "")
	path := writeHierarchyCSV(t, hierarchyCSVOfN(1))
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no subcommand is usage on stderr", nil, 1, "", "developer,team,division,org"},
		{"--help is usage on stdout", []string{"--help"}, 0, "?team=", ""},
		{"unknown subcommand", []string{"export"}, 1, "", "unknown hierarchy subcommand"},
		{"missing file", []string{"import", "--dry-run"}, 1, "", "missing <file.csv>"},
		{"two files", []string{"import", "--dry-run", path, path}, 1, "", "unexpected argument"},
		{"no server without dry run", []string{"import", path}, 1, "", "--server is required"},
		{"validation error names the line", []string{"import", "--dry-run", writeHierarchyCSV(t, "developer,team,division,org\nalice,,,\n")}, 1, "", "line 2: team is empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runHierarchyForTest(tc.args...)
			if code != tc.wantCode || !strings.Contains(out, tc.wantStdout) || !strings.Contains(errOut, tc.wantStderr) {
				t.Fatalf("exit %d stdout %q stderr %q; want %d, stdout ~%q, stderr ~%q", code, out, errOut, tc.wantCode, tc.wantStdout, tc.wantStderr)
			}
		})
	}
	var top bytes.Buffer
	printUsage(&top)
	if !strings.Contains(top.String(), "hierarchy") {
		t.Fatalf("top-level usage does not list hierarchy:\n%s", top.String())
	}
}
