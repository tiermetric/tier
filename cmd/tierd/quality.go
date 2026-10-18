package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/quality"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

type triggerPlanRow struct {
	store.LegacyCIEvent
	Trigger *string `json:"trigger_event"`
	Source  string  `json:"evidence_source"`
}
type triggerPlan struct {
	Version    int              `json:"version"`
	Candidates []triggerPlanRow `json:"candidates"`
	Resolved   int              `json:"resolved"`
	Unresolved int              `json:"unresolved"`
}

func (p triggerPlan) bytes() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	return append(b, '\n'), err
}

func runQualityCmd(args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int { _, _ = fmt.Fprintf(stderr, "resolve-triggers: %v\n", err); return 1 }
	if len(args) == 0 || args[0] != "resolve-triggers" {
		return fail(errors.New("usage: tierd quality resolve-triggers --dry-run | --apply PLANFILE --actor OPERATOR --approved-digest SHA256"))
	}
	fs := flag.NewFlagSet("quality resolve-triggers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "path to tier database")
	dry := fs.Bool("dry-run", false, "read only; report resolved and unresolved counts")
	apply := fs.String("apply", "", "approved dry-run plan file; requires operator sign-off; no unattended apply")
	out := fs.String("out", "", "write dry-run plan to a new file")
	actor := fs.String("actor", "", "self-asserted approving operator")
	approved := fs.String("approved-digest", "", "SHA-256 of raw plan bytes approved by the operator")
	token := fs.String("token", "", "GitHub token; prefer TIER_GITHUB_TOKEN or @/path/to/file")
	apiURL := fs.String("github-api-url", "https://api.github.com", "HTTPS GitHub REST API base URL")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 {
		return fail(errors.New("unexpected positional arguments"))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *apply != "" {
		invalid := *dry || *out != ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "token" || f.Name == "github-api-url" {
				invalid = true
			}
		})
		if invalid {
			return fail(errors.New("--apply cannot be combined with dry-run, output or API flags"))
		}
		if strings.TrimSpace(*actor) == "" || *approved == "" {
			return fail(errors.New("--apply requires --actor and --approved-digest recording operator sign-off on this plan; no unattended apply"))
		}
		raw, err := os.ReadFile(*apply)
		if err != nil {
			return fail(err)
		}
		if planBytesDigest(raw) != *approved {
			return fail(errors.New("approved digest does not match plan bytes"))
		}
		var p triggerPlan
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return fail(err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return fail(errors.New("unexpected trailing plan data"))
		}
		canonical, err := p.bytes()
		if err != nil {
			return fail(err)
		}
		if !bytes.Equal(raw, canonical) {
			return fail(errors.New("plan is not canonical: use the unmodified dry-run output"))
		}
		if err := p.validate(); err != nil {
			return fail(err)
		}
		db, err := store.Open(*dbPath)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = db.Close() }()
		if notice := db.UpgradeNotice(); notice != "" {
			_, _ = fmt.Fprintln(stderr, notice)
		}
		retained, err := db.RetainedCIRuns(ctx)
		if err != nil {
			return fail(err)
		}
		legacy, err := db.LegacyCIEvents(ctx)
		if err != nil {
			return fail(err)
		}
		current := make(map[int64]store.LegacyCIEvent, len(legacy))
		for _, e := range legacy {
			current[e.ID] = e
		}
		for _, r := range p.Candidates {
			if r.Trigger != nil && r.Source == "api" && current[r.ID] != r.LegacyCIEvent {
				return fail(fmt.Errorf("stale trigger plan: API quality event %d identity or provenance changed", r.ID))
			}
			if r.Trigger == nil || r.Source != "payload" {
				continue
			}
			var matches []store.RetainedCIRun
			repo, _ := repoid.Canonical(r.Repo)
			ref := quality.ParseCIRef(r.SourceRef)
			for _, e := range retained {
				retainedRepo, ok := repoid.Canonical(e.Repository.FullName)
				if ok && retainedRepo == repo && e.Action == "completed" && matchesCIRef(e.Run, ref) {
					matches = append(matches, e.Run)
				}
			}
			if trigger := uniqueTrigger(matches); trigger == nil || *trigger != *r.Trigger {
				return fail(fmt.Errorf("payload evidence changed for quality event %d", r.ID))
			}
		}
		applied, apiApplied := 0, 0
		for _, r := range p.Candidates {
			if r.Trigger == nil {
				continue
			}
			ok, preserved, err := db.ApplyCITrigger(ctx, r.LegacyCIEvent, *r.Trigger, *actor, *approved, r.Source)
			if err != nil {
				return fail(err)
			}
			if ok {
				applied++
				if r.Source == "api" {
					apiApplied++
				}
				if preserved {
					if _, err := fmt.Fprintf(stdout, "quality event %d (outcome %d): preserved non-event quality\n", r.ID, r.OutcomeID); err != nil {
						return fail(err)
					}
				}
			}
		}
		_, err = fmt.Fprintf(stdout, "resolved: %d; unresolved: %d; applied: %d (sealed outcomes skipped)\n", p.Resolved, p.Unresolved, applied)
		if err != nil {
			return fail(err)
		}
		if _, err := fmt.Fprintf(stdout, "API-sourced rows applied as reviewed, not re-fetched: %d\n", apiApplied); err != nil {
			return fail(err)
		}
		return 0
	}
	if *actor != "" || *approved != "" {
		return fail(errors.New("actor and approved-digest are apply-only flags"))
	}
	base, err := validateAPIBaseURL(*apiURL)
	if err != nil {
		return fail(err)
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" {
		return fail(errors.New("refusing http:// API URL; use https"))
	}
	rawToken := *token
	if rawToken == "" {
		rawToken = os.Getenv("TIER_GITHUB_TOKEN")
	}
	tok, err := resolveSecretFlag("--token", rawToken)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(tok) == "" {
		return fail(errors.New("--token is required (TIER_GITHUB_TOKEN or @/path/to/file)"))
	}
	db, err := store.OpenReadOnly(*dbPath)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = db.Close() }()
	gh := &githubClient{http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: qualityEvidenceRedirect}, baseURL: base, token: tok, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p, err := buildTriggerPlan(ctx, db, gh)
	if err != nil {
		return fail(err)
	}
	raw, err := p.bytes()
	if err != nil {
		return fail(err)
	}
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(err)
		}
		_, werr := f.Write(raw)
		cerr := f.Close()
		if err := errors.Join(werr, cerr); err != nil {
			return fail(err)
		}
	} else {
		if _, err := stdout.Write(raw); err != nil {
			return fail(err)
		}
	}
	_, _ = fmt.Fprintf(stderr, "resolved: %d; unresolved: %d\nplan SHA-256 (raw bytes): %s\n", p.Resolved, p.Unresolved, planBytesDigest(raw))
	return 0
}

func qualityEvidenceRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errRedirectRefused
	}
	return githubEvidenceRedirect(req, via)
}

func (p triggerPlan) validate() error {
	if p.Version != 1 {
		return errors.New("invalid trigger plan version")
	}
	seen := map[int64]bool{}
	resolved, unknown := 0, 0
	for _, r := range p.Candidates {
		if r.ID <= 0 || r.OutcomeID <= 0 || seen[r.ID] || (r.EventType != "ci_pass" && r.EventType != "ci_fail" && r.EventType != "ci_fail_flaky") {
			return errors.New("invalid or duplicate trigger plan identity")
		}
		seen[r.ID] = true
		if r.Trigger == nil {
			unknown++
			continue
		}
		ref := quality.ParseCIRef(r.SourceRef)
		if _, ok := repoid.Canonical(r.Repo); !ok || ref.WorkflowID == "" || ref.RunAttempt <= 0 || ref.HeadSHA != r.MergeSHA || *r.Trigger != store.NormalizeCITrigger(*r.Trigger) || (r.Source != "payload" && r.Source != "api") {
			return errors.New("resolved trigger lacks evidence identity")
		}
		resolved++
	}
	if resolved != p.Resolved || unknown != p.Unresolved {
		return errors.New("trigger plan counts disagree")
	}
	return nil
}

func buildTriggerPlan(ctx context.Context, db *store.DB, gh *githubClient) (triggerPlan, error) {
	p := triggerPlan{Version: 1, Candidates: []triggerPlanRow{}}
	ids, err := db.LegacyCIEvents(ctx)
	if err != nil {
		return p, err
	}
	retained, err := db.RetainedCIRuns(ctx)
	if err != nil {
		return p, err
	}
	for _, id := range ids {
		row := triggerPlanRow{LegacyCIEvent: id}
		repo, valid := repoid.Canonical(id.Repo)
		ref := quality.ParseCIRef(id.SourceRef)
		if valid && ref.WorkflowID != "" && ref.RunAttempt > 0 && ref.HeadSHA == id.MergeSHA {
			var matches []store.RetainedCIRun
			for _, e := range retained {
				r, ok := repoid.Canonical(e.Repository.FullName)
				if ok && r == repo && e.Action == "completed" && matchesCIRef(e.Run, ref) {
					matches = append(matches, e.Run)
				}
			}
			if len(matches) > 0 {
				row.Trigger = uniqueTrigger(matches)
				row.Source = "payload"
			} else {
				matches, err = gh.triggerRuns(ctx, repo, ref)
				if err != nil {
					return p, err
				}
				row.Trigger = uniqueTrigger(matches)
				row.Source = "api"
			}
		}
		if row.Trigger == nil {
			row.Source = ""
			p.Unresolved++
		} else {
			p.Resolved++
		}
		p.Candidates = append(p.Candidates, row)
	}
	return p, nil
}
func matchesCIRef(run store.RetainedCIRun, ref quality.CIRef) bool {
	return run.HeadSHA == ref.HeadSHA && strconv.FormatInt(run.WorkflowID, 10) == ref.WorkflowID && run.Attempt == ref.RunAttempt
}

// Conflicting deliveries and independent runs remain unknown, even for the same trigger.
func uniqueTrigger(runs []store.RetainedCIRun) *string {
	unique := map[string]string{}
	for _, r := range runs {
		if r.ID <= 0 || r.Event == "" {
			return nil
		}
		key := strconv.FormatInt(r.ID, 10) + ":" + strconv.Itoa(r.Attempt) + ":" + r.Event
		unique[key] = store.NormalizeCITrigger(r.Event)
	}
	if len(unique) != 1 {
		return nil
	}
	for _, v := range unique {
		return &v
	}
	return nil
}

func (gh *githubClient) triggerRuns(ctx context.Context, repo string, ref quality.CIRef) ([]store.RetainedCIRun, error) {
	var matches []store.RetainedCIRun
	for page := 1; page <= 1000; page++ {
		raw, err := gh.do(ctx, "/repos/"+repo+"/actions/runs?head_sha="+url.QueryEscape(ref.HeadSHA)+"&per_page=100&page="+strconv.Itoa(page))
		if err != nil {
			var status *githubStatusError
			if errors.As(err, &status) && (status.StatusCode == 404 || status.StatusCode == 410) {
				return nil, nil
			}
			return nil, err
		}
		var data struct {
			Runs []store.RetainedCIRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		for _, run := range data.Runs {
			if run.HeadSHA != ref.HeadSHA || strconv.FormatInt(run.WorkflowID, 10) != ref.WorkflowID || run.Attempt < ref.RunAttempt {
				continue
			}
			if run.Attempt != ref.RunAttempt {
				raw, err := gh.do(ctx, "/repos/"+repo+"/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/attempts/"+strconv.Itoa(ref.RunAttempt))
				if err != nil {
					var status *githubStatusError
					if errors.As(err, &status) && (status.StatusCode == 404 || status.StatusCode == 410) {
						continue
					}
					return nil, err
				}
				if err := json.Unmarshal(raw, &run); err != nil {
					return nil, err
				}
			}
			if matchesCIRef(run, ref) {
				matches = append(matches, run)
			}
		}
		if len(data.Runs) < 100 {
			return matches, nil
		}
	}
	return nil, errors.New("GitHub run pagination incomplete; provenance stays unknown")
}
