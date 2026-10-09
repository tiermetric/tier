package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

// hierarchyImportTimeout bounds the POST so a hung server cannot hang the command.
const hierarchyImportTimeout = 60 * time.Second

// hierarchyColumns is the required CSV header, compared case-insensitively.
var hierarchyColumns = []string{"developer", "team", "division", "org"}

// hierarchyImportItem is one element of the POST /api/v1/org_hierarchy array.
// Its JSON tags must equal api.hierarchyBulkItem's: the handler decodes with
// DisallowUnknownFields, so any drift is a 400 on every import.
type hierarchyImportItem struct {
	Rejoin    bool   `json:"rejoin,omitempty"`
	Developer string `json:"developer"`
	Team      string `json:"team"`
	Division  string `json:"division"`
	Org       string `json:"org"`
}

type hierarchyCSVRow struct {
	line int
	item hierarchyImportItem
}

func runHierarchyCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		printHierarchyUsage(stderr)
		return 1
	}
	switch args[0] {
	case "import":
		return runHierarchyImport(args[1:], stdout, stderr)
	case "help", "--help", "-help", "-h":
		printHierarchyUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown hierarchy subcommand: %s\n", logsafe.Str(args[0]))
		printHierarchyUsage(stderr)
		return 1
	}
}

func printHierarchyUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: tierd hierarchy import [--server URL] [--api-token TOKEN] [--dry-run] <file.csv>

Loads your developer -> team map into a running tierd through its admin endpoint
POST /api/v1/org_hierarchy. Each row is an upsert, so re-running a file is safe.
The import only ADDS and UPDATES: a developer left out of the file keeps their
current team, so it never removes anyone. To end a developer's membership, use
POST /api/v1/period_membership/{developer}/end. Re-import keeps an explicitly
ended membership ended across all aliases unless a membership in any org
started at or after that end month. Kept departures are reported as
kept-departed. An earlier move reopens this month. An unclassified end also
reopens this month and is reported: end them again if they left.
Each report lists the first 100 people and gives the total when truncated.

<file.csv>   your own team list, kept in any spreadsheet and exported as CSV
             (Google Sheets: File > Download > CSV; Excel: Save As > CSV UTF-8).
             The first line must be this header, columns in any order:
                 developer,team,division,org
             developer  the person's id exactly as the dashboard and
                        /api/v1/scores show it (usually their GitHub login). An
                        alias registered with /api/v1/developer_alias also
                        works: the server resolves it on import. Required.
             team       the team name. Required. This import is what defines
                        the team names that ?team= and the dashboards show.
                        Names are case-sensitive: "Platform" and "platform"
                        are two different teams, so spell each one the same
                        way every time.
             division   the group of teams above it. May be left empty.
             org        the organisation name. May be left empty. It must be
                        the same org name used when posting org_actual_spend.
             rejoin     optional column: true/false (case-insensitive).
                        Empty or omitted = false. Set true to explicitly
                        re-enrol a departed person this month.
             One row per developer; a developer listed twice is refused.
--server     the address where 'tierd serve' is listening, e.g.
             http://127.0.0.1:8080 on a laptop, https://tier.example for a team
             server. Required unless --dry-run. With a token it must be https
             unless the host is loopback (127.0.0.1, ::1, localhost).
--api-token  the ADMIN (write) token 'tierd serve' was started with. Default:
             $TIER_API_TOKEN. Use @/path/to/file to read it from a file; a
             literal token here shows up in ps and shell history. The read-only
             viewer token is refused. Omit only for a laptop serve with no token.
--dry-run    read and check the file, print the row count, send nothing. The
             server applies more rules on a real import (identifier length,
             reserved ids), so a clean dry run is not a guarantee.

The whole file is sent as ONE all-or-nothing request, so it may hold at most %d
rows and at most %d bytes once encoded (1000 rows of long names can exceed the
byte limit). A larger file is refused before anything is sent: split it into
smaller files and import each one, never putting the same person (under any id
or alias) in two files. Exit 0 means every row was written. On exit 1 the
message says whether nothing was written or the outcome is UNKNOWN (for a 500
or 503 it depends on whether tierd or a proxy answered); re-running the same
file is safe either way.
`, api.MaxHierarchyPerBatch, api.MaxHierarchyBody)
}

func runHierarchyImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hierarchy import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printHierarchyUsage(stderr) }
	server := fs.String("server", "", "central tierd base URL, e.g. https://tier.example (required unless --dry-run; https unless loopback)")
	apiToken := fs.String("api-token", "", "admin API token sent as Authorization: Bearer. When the flag is not given, TIER_API_TOKEN is used. Prefer TIER_API_TOKEN or @/path/to/file (#37)")
	dryRun := fs.Bool("dry-run", false, "validate the CSV and print the row count; send nothing")
	// Parsed twice so flags may follow the file: Go's flag package stops at the
	// first positional argument and would otherwise drop them silently.
	if err := fs.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if fs.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "hierarchy import: missing <file.csv>")
		printHierarchyUsage(stderr)
		return 1
	}
	path := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return flagParseExit(err)
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "hierarchy import: unexpected argument %s (one CSV file per run)\n", logsafe.Str(fs.Arg(0)))
		return 1
	}
	secretEnvFallback(fs, apiToken, "api-token", "TIER_API_TOKEN")
	if !*dryRun && *server == "" {
		_, _ = fmt.Fprintln(stderr, "hierarchy import: --server is required (the address where tierd serve listens, e.g. http://127.0.0.1:8080)")
		return 1
	}
	token, err := resolveSecretFlag("--api-token", *apiToken)
	if err != nil && !*dryRun {
		_, _ = fmt.Fprintf(stderr, "hierarchy import: --api-token: %v\n", err)
		return 1
	}
	base := ""
	if *server != "" {
		u, _ := url.Parse(*server)
		if token == "" && u != nil && u.Scheme == "http" && u.Host != "" && !isLoopbackHost(u.Hostname()) {
			// No token to leak, so allowed, as ship allows it - but said aloud.
			_, _ = fmt.Fprintln(stderr, "hierarchy import: warning: sending over cleartext http to a non-loopback host; the team map crosses the network unencrypted, prefer https://")
			base = strings.TrimRight(*server, "/")
		} else if base, err = validateAPIBaseURL(*server); err != nil {
			_, _ = fmt.Fprintf(stderr, "hierarchy import: --server: %v\n", err)
			return 1
		}
	}

	// Installed before the file is read so a Ctrl-C from here on is caught and
	// checked before the request goes out.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	f, err := os.Open(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hierarchy import: %v\n", err)
		return 1
	}
	rows, err := readHierarchyCSV(f)
	_ = f.Close()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hierarchy import: %s: %v\n", logsafe.Str(path), err)
		return 1
	}
	// One request, so the server's per-request duplicate-identity check sees
	// every row: split across requests, an alias and its canonical id would
	// silently overwrite each other.
	items := make([]hierarchyImportItem, len(rows))
	for i, r := range rows {
		items[i] = r.item
	}
	body, _ := json.Marshal(items) // a slice of string structs cannot fail to marshal
	const split = "split it into smaller files and import each one, never putting the same person (under any id or alias) in two files"
	if len(rows) > api.MaxHierarchyPerBatch {
		_, _ = fmt.Fprintf(stderr, "hierarchy import: %s has %d rows; one import holds at most %d - %s\n", logsafe.Str(path), len(rows), api.MaxHierarchyPerBatch, split)
		return 1
	}
	if len(body) > api.MaxHierarchyBody {
		_, _ = fmt.Fprintf(stderr, "hierarchy import: %s encodes to %d bytes; one import holds at most %d - %s\n", logsafe.Str(path), len(body), api.MaxHierarchyBody, split)
		return 1
	}
	if *dryRun {
		_, _ = fmt.Fprintf(stdout, "hierarchy import: dry run: %d rows valid (%d bytes); nothing sent\n", len(rows), len(body))
		return 0
	}

	if ctx.Err() != nil {
		_, _ = fmt.Fprintln(stderr, "hierarchy import: interrupted; nothing was sent")
		return 1
	}
	client := &http.Client{
		Timeout: hierarchyImportTimeout,
		// A redirected admin POST is reported, never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if err := postHierarchy(ctx, client, base+"/api/v1/org_hierarchy", token, rows, body, stdout); err != nil {
		// Only a handler answer proves nothing was written; a lost answer, a
		// gateway 502/504 or an unexpected status may follow a commit.
		fate := "the outcome is UNKNOWN - the server may have written every row"
		rerun := "re-running the same file is safe"
		var refused *hierarchyRefusedError
		if errors.As(err, &refused) {
			fate = "nothing was written (the import is all-or-nothing)"
			if refused.status >= 500 {
				// A proxy can synthesize a 500/503 after tierd committed.
				fate = "tierd's own handler writes nothing when it answers this status, but if a proxy in front of tierd produced it the outcome is unknown"
				rerun = "re-running the same file is safe (the import is an idempotent upsert)"
			}
		}
		_, _ = fmt.Fprintf(stderr, "hierarchy import: failed, %s: %v; %s\n", fate, err, rerun)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "hierarchy import: %d rows written\n", len(rows))
	return 0
}

func flagParseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 1
}

// readHierarchyCSV parses and validates the whole file before anything is sent.
func readHierarchyCSV(r io.Reader) ([]hierarchyCSVRow, error) {
	br := bufio.NewReader(r)
	if bom, err := br.Peek(3); err == nil && bytes.Equal(bom, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("file is empty; the first line must be the header %s", strings.Join(hierarchyColumns, ","))
	}
	if err != nil {
		return nil, err
	}
	col := make(map[string]int, len(hierarchyColumns))
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		if name != "rejoin" && !slices.Contains(hierarchyColumns, name) {
			hint := ""
			if strings.Contains(h, ";") {
				hint = "; if your spreadsheet saved with semicolons, re-export with commas"
			}
			return nil, fmt.Errorf("line 1: unknown column %q; the header must be %s%s", h, strings.Join(hierarchyColumns, ","), hint)
		}
		if _, dup := col[name]; dup {
			return nil, fmt.Errorf("line 1: column %q appears twice", name)
		}
		col[name] = i
	}
	for _, c := range hierarchyColumns {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("line 1: missing column %q; the header must be %s", c, strings.Join(hierarchyColumns, ","))
		}
	}
	var rows []hierarchyCSVRow
	seen := make(map[string]int)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := cr.FieldPos(0)
		for _, c := range rec {
			if !utf8.ValidString(c) {
				// encoding/json would silently turn the bad bytes into U+FFFD.
				return nil, fmt.Errorf("line %d: not UTF-8; re-export from your spreadsheet as CSV UTF-8", line)
			}
		}
		cell := func(c string) string { return strings.TrimSpace(rec[col[c]]) }
		it := hierarchyImportItem{Developer: cell("developer"), Team: cell("team"), Division: cell("division"), Org: cell("org")}
		if _, ok := col["rejoin"]; ok {
			switch strings.ToLower(cell("rejoin")) {
			case "true":
				it.Rejoin = true
			case "", "false":
			default:
				return nil, fmt.Errorf("line %d: rejoin must be true or false (empty = false), got %q", line, cell("rejoin"))
			}
		}
		if it.Developer == "" {
			return nil, fmt.Errorf("line %d: developer is empty", line)
		}
		if it.Team == "" {
			return nil, fmt.Errorf("line %d: team is empty for developer %q", line, it.Developer)
		}
		if prev, dup := seen[it.Developer]; dup {
			return nil, fmt.Errorf("line %d: developer %q is already assigned on line %d; a developer belongs to one team", line, it.Developer, prev)
		}
		seen[it.Developer] = line
		rows = append(rows, hierarchyCSVRow{line: line, item: it})
	}
	if len(rows) == 0 {
		return nil, errors.New("no rows after the header")
	}
	return rows, nil
}

// hierarchyRefusedError is a 4xx, 500 or 503. tierd's all-or-nothing handler
// writes nothing on any of them; a 500 or 503 may also come from a proxy.
type hierarchyRefusedError struct {
	msg    string
	status int
}

func (e *hierarchyRefusedError) Error() string { return e.msg }

var hierarchyIndexRE = regexp.MustCompile(`org_hierarchy\[(\d+)\]`)

// postHierarchy sends the import and succeeds only on 201 with every row accepted.
func postHierarchy(ctx context.Context, client *http.Client, endpoint, token string, rows []hierarchyCSVRow, body []byte, stdout io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("bad --server URL: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxHierarchyBody))
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		// The server indexes rows in the request; name the CSV line instead.
		msg = hierarchyIndexRE.ReplaceAllStringFunc(msg, func(m string) string {
			i, err := strconv.Atoi(hierarchyIndexRE.FindStringSubmatch(m)[1])
			if err != nil || i >= len(rows) {
				return m
			}
			return fmt.Sprintf("%s (CSV line %d)", m, rows[i].line)
		})
		hint := ""
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			hint = " (set --api-token or TIER_API_TOKEN to the admin token tierd serve was started with)"
		case http.StatusForbidden:
			hint = " (this is the read-only viewer token; the import needs the admin token)"
		}
		err := fmt.Errorf("HTTP %d: %s%s", resp.StatusCode, logsafe.Str(msg), hint)
		if (resp.StatusCode >= 400 && resp.StatusCode < 500) ||
			resp.StatusCode == http.StatusInternalServerError || resp.StatusCode == http.StatusServiceUnavailable {
			return &hierarchyRefusedError{msg: err.Error(), status: resp.StatusCode}
		}
		return err // a gateway (502, 504) or a redirect answered, not the handler
	}
	var ok struct {
		store.HierarchyImportResult
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil || ok.Accepted != len(rows) {
		return fmt.Errorf("server answered 201 but accepted %d of %d rows (body %s)", ok.Accepted, len(rows), logsafe.Str(string(raw)))
	}
	kept := ok.KeptDeparted[:min(len(ok.KeptDeparted), store.HierarchyImportReportLimit)]
	for _, member := range kept {
		_, _ = fmt.Fprintf(stdout, "kept-departed %s in %s; set rejoin=true to re-enrol\n", logsafe.Str(member.Developer), logsafe.Str(member.Org))
	}
	if ok.KeptDepartedCount > len(kept) {
		_, _ = fmt.Fprintf(stdout, "kept-departed: %d total (first %d shown)\n", ok.KeptDepartedCount, len(kept))
	}
	reseated := ok.ReseatedUnknown[:min(len(ok.ReseatedUnknown), store.HierarchyImportReportLimit)]
	for _, member := range reseated {
		_, _ = fmt.Fprintf(stdout, "re-seated %s; their earlier end in %s has no classified departure reason — end them again if they left\n", logsafe.Str(member.Developer), logsafe.Str(member.Org))
	}
	if ok.ReseatedUnknownCount > len(reseated) {
		_, _ = fmt.Fprintf(stdout, "re-seated: %d total (first %d shown)\n", ok.ReseatedUnknownCount, len(reseated))
	}
	return nil
}
