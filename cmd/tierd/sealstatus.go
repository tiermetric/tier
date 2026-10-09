package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/store"
)

// sealSkipCommand is the `tierd seal --skip` command for month, which callers
// take only from a stored month or api.ParseSealMonth, never from free text.
func sealSkipCommand(month string) string {
	return "tierd seal --skip " + month + " --reason ''"
}

// sealStallReply is the sealing fields of GET /api/v1/report_manifest
// (#913-D8 ruling C) as a server sent them: untrusted text.
type sealStallReply struct {
	Schema      string `json:"manifest_schema"`
	Aggregation string `json:"aggregation"`
	Period      string `json:"period"`
	api.SealStallJSON
	// nothingSealed is set for the sealed 404 a read with no ?period= answers
	// only while no month is sealed.
	nothingSealed bool
}

// errSealTokenRefused and errSealManifestUnknown are fetchSealStall's
// answers from a server that was reached but did not say whether sealing is
// stalled; their text holds nothing the server sent.
var (
	errSealTokenRefused    = errors.New("the server refused the API token")
	errSealManifestUnknown = errors.New("the server's manifest is not one this tierd binary reads")
)

// fetchSealStall reads the server's sealing fields. sealed is false, with no
// error, only for a live manifest (api.ManifestSchema): the server serves no
// sealed months: it is in developer mode, or its tierd predates sealed months.
func fetchSealStall(ctx context.Context, client *http.Client, server, token string) (r sealStallReply, sealed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/api/v1/report_manifest", nil)
	if err != nil {
		return r, false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return r, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
	case http.StatusUnauthorized, http.StatusForbidden:
		return r, false, fmt.Errorf("%w (GET /api/v1/report_manifest returned %d)", errSealTokenRefused, resp.StatusCode)
	default:
		return r, false, fmt.Errorf("GET /api/v1/report_manifest returned %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if json.Unmarshal(body, &r) != nil {
		return sealStallReply{}, false, errSealManifestUnknown
	}
	r.nothingSealed = resp.StatusCode == http.StatusNotFound
	switch {
	case r.nothingSealed && isSealed404(r.Aggregation, r.Period), !r.nothingSealed && r.Schema == api.SealedManifestSchema:
		return r, true, nil
	case !r.nothingSealed && r.Schema == api.ManifestSchema:
		return r, false, nil
	}
	return sealStallReply{}, false, errSealManifestUnknown
}

// isSealed404 reports whether a 404 body's aggregation and period are those of
// tierd's sealed 404: an aggregation named, and a month api.ParseSealMonth
// accepts. Any other 404 is not a statement that nothing is sealed.
func isSealed404(aggregation, period string) bool {
	_, err := api.ParseSealMonth(period)
	return aggregation != "" && err == nil
}

// stallReason is a server's stall_reason code, quoted, with this binary's
// words for it when it knows the code.
func stallReason(code string) string {
	if words, ok := api.SealStallWords(code); ok {
		return logsafe.Str(code) + " (" + words + ")"
	}
	return logsafe.Str(code)
}

// safeInstant re-emits an RFC3339 instant a server sent, else sanitizes it.
func safeInstant(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return logsafe.Str(s)
}

// checkSealStall is doctor's sealing line (#913-D8 ruling C, condition 5): OK
// when the server names no stalled month, WARN naming it when it does (only the
// server host can clear it), WARN when it cannot tell, and nothing when the
// server serves a live manifest, so no sealed months. The skip command is built
// only from a month api.ParseSealMonth accepts; the reason is a line of its
// own, never part of the command.
func checkSealStall(ctx context.Context, client *http.Client, server, token string) []checkResult {
	r, sealed, err := fetchSealStall(ctx, client, server, token)
	switch {
	case errors.Is(err, errSealManifestUnknown):
		return []checkResult{{name: "sealing", status: statusWarn, detail: "not checked: " + logsafe.Err(err),
			hint: "run the same tierd release here and on the server, and confirm --server points at a tierd"}}
	case err != nil:
		return []checkResult{{name: "sealing", status: statusWarn, detail: "not checked: " + logsafe.Err(err),
			hint: "GET /api/v1/report_manifest must answer with the same token as /api/v1/scores"}}
	case !sealed:
		return nil
	case r.StalledPeriod == "":
		return []checkResult{{name: "sealing", status: statusOK, detail: "no month is stalled"}}
	}
	res := checkResult{name: "sealing", status: statusWarn}
	reason := "stall reason (as the server sent it): " + stallReason(r.StallReason)
	start, err := api.ParseSealMonth(r.StalledPeriod)
	if err != nil {
		res.detail = "the server names a stalled month that is not YYYY-MM: " + logsafe.Str(r.StalledPeriod)
		res.hint = "confirm --server points at a tierd; `tierd seal --status` on the server host reads the month from its database"
		res.more = []string{reason}
		return []checkResult{res}
	}
	month := start.Format("2006-01")
	res.detail = fmt.Sprintf("stalled at %s, owed since %s: no later month is sealed until it is", month, safeInstant(r.OwedSince))
	if r.nothingSealed {
		res.hint = month + " is the first month sealed, which cannot be skipped: fix what its seal fails on"
		res.more = []string{reason}
		return []checkResult{res}
	}
	res.hint = "if its seal cannot succeed, record it as a gap: run this on the server host with serve's --config, flags and env"
	res.more = []string{"command: " + sealSkipCommand(month), reason}
	return []checkResult{res}
}

// runSealStatus is `tierd seal --status` (#913-D5 ruling C′, D8 ruling C): the
// sealing state from the database opened read-only, and the stall reason from
// the server when --server is given. Its exit code is sealExitNotStalled,
// sealExitStalled or sealExitStallUnknown, or a refusal or could-not-run.
func runSealStatus(ctx context.Context, dbPath string, settings sealSettings, server, apiToken string, stdout io.Writer,
	refused, cannotRun func(string, ...any) int) int {
	db, err := store.OpenReadOnly(dbPath)
	switch {
	case errors.Is(err, store.ErrSchemaMismatch):
		return refused("%v", logsafe.Err(err))
	case err != nil:
		return cannotRun("%v", logsafe.Err(err))
	}
	defer func() { _ = db.Close() }()
	h := api.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, version, api.RateLimitConfig{})
	h.SetAggregation(settings.mode, settings.k)
	st, err := h.SealStatus(ctx, settings.grace, settings.sealFrom)
	if err != nil {
		return cannotRun("read the sealing state: %v", logsafe.Err(err))
	}
	line := func(k, v string) { _, _ = fmt.Fprintf(stdout, "%-30s %s\n", k+":", v) }
	orNone := func(s string) string {
		if s == "" {
			return "none"
		}
		return s
	}
	switch {
	case st.Floor != "":
		line("armed", "yes, by the first seal")
	case st.Armed:
		line("armed", "yes, by seal_from "+settings.sealFrom+" ("+settings.sealFromSource+"); no month is sealed yet")
	default:
		line("armed", "no: no month is sealed until `tierd seal --arm YYYY-MM|earliest` is run")
	}
	line("pinned floor", orNone(st.Floor))
	line("newest sealed or gapped month", orNone(st.Newest))
	if st.Owed != "" {
		line("first owed month", st.Owed+", sealable at "+st.OwedSealableAt)
	} else {
		line("first owed month", "none")
	}
	if st.Overdue.Month != "" {
		line("overdue", "yes: "+st.Overdue.Month+" is more than two seal passes past its sealable_at")
	} else {
		line("overdue", "no")
	}
	line("sources behind", orNone(st.Behind))
	reason, rc := sealStallVerdict(ctx, st, server, apiToken)
	line("stall reason", reason)
	if rc != sealExitStalled && st.Overdue.Month == "" {
		return rc
	}
	switch {
	case settings.readOnly:
		line("skip", "none: this read-only server never seals "+st.Owed)
	case st.Newest == "":
		line("skip", "none: "+st.Owed+" is the first month sealed, which cannot be skipped")
	default:
		line("skip", "if its seal cannot succeed, record it as a gap with serve's --config, flags and env:")
		_, _ = fmt.Fprintln(stdout, "  "+sealSkipCommand(st.Owed))
	}
	return rc
}

// sealStallVerdict is --status's stall reason and exit code (#913-D8 ruling C,
// condition 7). Stalled is only a stall the server confirms on the database's
// first owed month; a server not given, not reached, refusing the token, not
// serving sealed months or naming another month is unknown, even when the
// database shows the month overdue. Not stalled is a server naming no stall
// while that month is not overdue, or, with no --server, no month sealable yet.
func sealStallVerdict(ctx context.Context, st api.SealState, server, apiToken string) (string, int) {
	const notReached = "UNKNOWN (server not reached)"
	due := st.Due || st.Overdue.Month != ""
	if server == "" {
		if !due {
			return "none: no owed month is sealable yet", sealExitNotStalled
		}
		return notReached + "; pass --server and an API token to ask tierd serve", sealExitStallUnknown
	}
	token, err := resolveSecretFlag("--api-token", apiToken)
	if err != nil {
		return notReached + "; --api-token: " + logsafe.Err(err), sealExitStallUnknown
	}
	base, err := doctorServerBase(server, token)
	if err != nil {
		return notReached + "; --server: " + logsafe.Err(err), sealExitStallUnknown
	}
	r, sealed, err := fetchSealStall(ctx, newServerCheckClient(), base, token)
	switch {
	case errors.Is(err, errSealTokenRefused), errors.Is(err, errSealManifestUnknown):
		return "UNKNOWN (" + err.Error() + ")", sealExitStallUnknown
	case err != nil:
		return notReached + "; " + logsafe.Err(err), sealExitStallUnknown
	case !sealed:
		return "UNKNOWN (the server serves no sealed months: it is in developer mode, or its tierd predates sealed months)", sealExitStallUnknown
	case r.StalledPeriod == "" && st.Overdue.Month == "":
		if !due {
			return "none: no owed month is sealable yet, and the server names no stalled month", sealExitNotStalled
		}
		return "none: the server names no stalled month", sealExitNotStalled
	case r.StalledPeriod != st.Owed || !due:
		named, shows := "no stalled month", st.Owed+" owed"
		if r.StalledPeriod != "" {
			named = logsafe.Str(r.StalledPeriod)
		}
		if !due {
			shows = "no owed month sealable yet"
		}
		return "UNKNOWN (the server names " + named + ", but this database shows " + shows +
			": is --server serving this --db with the same --report-grace and --seal-from?)", sealExitStallUnknown
	}
	return stallReason(r.StallReason) + ", as the server reported it", sealExitStalled
}
