package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tiermetric/tier/internal/prderive"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

type revertPlanRow struct {
	store.PRExclusionIdentity
	EvidenceSource string `json:"evidence_source"`
	HeadRef        string `json:"head_ref"`
	Title          string `json:"title"`
	Verdict        string `json:"verdict"`
	Reason         string `json:"reason"`
}

type revertPlanCounts struct {
	Total   int `json:"total"`
	Exclude int `json:"exclude"`
	Keep    int `json:"keep"`
	Unknown int `json:"unknown"`
}

type revertPlan struct {
	Version    int              `json:"version"`
	Repo       string           `json:"repo"`
	Since      string           `json:"since"`
	Candidates []revertPlanRow  `json:"candidates"`
	Counts     revertPlanCounts `json:"counts"`
}

// canonicalBytes is exactly the dry-run encoder's output, including its newline.
// Approval hashes these raw bytes; the plan cannot contain its own digest.
func (p revertPlan) canonicalBytes() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	_ = enc.Encode(p) // This plan contains only JSON-encodable scalar fields.
	return b.Bytes()
}

func planBytesDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p revertPlan) digest() string { return planBytesDigest(p.canonicalBytes()) }

func (p revertPlan) validate() error {
	if p.Version != 1 {
		return errors.New("exclusion plan version invalid; use the exact approved dry-run plan")
	}
	counts := revertPlanCounts{Total: len(p.Candidates)}
	seen := map[int64]bool{}
	for _, r := range p.Candidates {
		if r.OutcomeID <= 0 || r.PR <= 0 || seen[r.OutcomeID] {
			return errors.New("invalid or duplicate exclusion plan identity")
		}
		seen[r.OutcomeID] = true
		switch r.Verdict {
		case "exclude":
			if _, ok := repoid.Canonical(r.Repo); !ok || r.MergeSHA == "" ||
				(r.EvidenceSource != "payload" && r.EvidenceSource != "api") || prderive.RevertEvidence(r.HeadRef, r.Title) == "" {
				return errors.New("exclude row lacks recognised revert evidence")
			}
			if r.Reason != "recognised GitHub revert PR" {
				return errors.New("exclusion plan reason inconsistent with verdict")
			}
			counts.Exclude++
		case "keep":
			if prderive.RevertEvidence(r.HeadRef, r.Title) != "" || r.HeadRef == "" || r.Title == "" || r.Reason != "no recognised GitHub revert form" {
				return errors.New("exclusion plan evidence/reason inconsistent with keep verdict")
			}
			counts.Keep++
		case "unknown":
			if prderive.RevertEvidence(r.HeadRef, r.Title) != "" || (r.HeadRef != "" && r.Title != "") {
				return errors.New("exclusion plan evidence inconsistent with unknown verdict")
			}
			switch r.Reason {
			case "missing repository or merge identity", "GitHub PR missing (404)", "GitHub PR identity/merge SHA missing or differs from stored outcome", "head ref/title evidence incomplete":
			default:
				return errors.New("exclusion plan reason inconsistent with unknown verdict")
			}
			counts.Unknown++
		default:
			return errors.New("invalid exclusion plan verdict")
		}
	}
	if counts != p.Counts {
		return errors.New("exclusion plan counts invalid")
	}
	return nil
}

func runOutcomesCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		args = []string{"exclude-reverts", "--help"}
	}
	if len(args) == 0 || args[0] != "exclude-reverts" {
		_, _ = fmt.Fprintln(stderr, "usage: tierd outcomes exclude-reverts [--dry-run | --apply PLANFILE --actor OPERATOR --approved-digest SHA256] [flags]")
		return 1
	}
	fs := flag.NewFlagSet("outcomes exclude-reverts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "local SQLite database")
	dry := fs.Bool("dry-run", false, "create an approval plan without database writes (default)")
	apply := fs.String("apply", "", "apply the operator-approved plan file")
	out := fs.String("out", "", "write dry-run JSON to a file instead of stdout")
	repo := fs.String("repo", "", "optional owner/name filter")
	sinceFlag := fs.String("since", "", "optional inclusive UTC outcome date (YYYY-MM-DD or RFC3339)")
	actor := fs.String("actor", "", "self-asserted approving operator; required for apply")
	approved := fs.String("approved-digest", "", "SHA-256 of raw plan file bytes approved by the operator (shasum -a 256 plan.json)")
	token := fs.String("token", "", "GitHub token; prefer TIER_GITHUB_TOKEN or @/path/to/file")
	apiURL := fs.String("github-api-url", "https://api.github.com", "GitHub REST API base URL")
	fail := func(err error) int { _, _ = fmt.Fprintf(stderr, "exclude-reverts: %v\n", err); return 1 }
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 {
		return fail(errors.New("unexpected positional arguments"))
	}
	if *apply != "" {
		invalid := *dry || *out != "" || *repo != "" || *sinceFlag != ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "token" || f.Name == "github-api-url" {
				invalid = true
			}
		})
		if invalid {
			return fail(errors.New("--apply cannot be combined with dry-run, output, filters or API flags"))
		}
		if strings.TrimSpace(*actor) == "" || *approved == "" {
			return fail(errors.New("--apply requires --actor and --approved-digest recording operator sign-off on this plan; no unattended apply"))
		}
		return applyRevertPlan(*dbPath, *apply, *actor, *approved, stdout, stderr)
	}
	if *actor != "" || *approved != "" {
		return fail(errors.New("actor and approved-digest are apply-only flags"))
	}
	if *repo != "" {
		canonical, ok := repoid.Canonical(*repo)
		if !ok {
			return fail(errors.New("invalid --repo; use owner/name"))
		}
		*repo = canonical
	}
	var since time.Time
	if *sinceFlag != "" {
		var err error
		since, err = time.Parse(time.RFC3339, *sinceFlag)
		if err != nil {
			since, err = time.Parse("2006-01-02", *sinceFlag)
		}
		if err != nil {
			return fail(errors.New("invalid --since; use YYYY-MM-DD or RFC3339"))
		}
		since = since.UTC()
	}
	baseURL, err := validateAPIBaseURL(*apiURL)
	if err != nil {
		return fail(err)
	}
	raw := *token
	if raw == "" {
		raw = os.Getenv("TIER_GITHUB_TOKEN")
	}
	tok, err := resolveSecretFlag("--token", raw)
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
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	gh := &githubClient{http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: githubEvidenceRedirect}, baseURL: baseURL, token: tok, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	plan, err := buildRevertPlan(ctx, db, gh, *repo, since)
	if err != nil {
		return fail(err)
	}
	if *out != "" {
		// Refuse an existing file, including the database or an approved plan.
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(err)
		}
		_, writeErr := f.Write(plan.canonicalBytes())
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return fail(err)
		}
	} else if _, err := stdout.Write(plan.canonicalBytes()); err != nil {
		return fail(err)
	}
	_, _ = fmt.Fprintf(stderr, "plan SHA-256 (raw bytes): %s\n", plan.digest())
	return 0
}

func githubEvidenceRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 || req.URL.Host != via[0].URL.Host || (via[0].URL.Scheme == "https" && req.URL.Scheme != "https") {
		return errRedirectRefused
	}
	return nil
}

func buildRevertPlan(ctx context.Context, db *store.DB, gh *githubClient, repo string, since time.Time) (revertPlan, error) {
	p := revertPlan{Version: 1, Repo: repo, Candidates: []revertPlanRow{}}
	if !since.IsZero() {
		p.Since = since.Format(time.RFC3339Nano)
	}
	ids, err := db.PRExclusionCandidates(ctx, repo, since)
	if err != nil {
		return p, err
	}
	retained, err := db.RetainedPREvidence(ctx)
	if err != nil {
		return p, err
	}
	payloads := preferredRevertPayloads(retained)
	for _, id := range ids {
		r := revertPlanRow{PRExclusionIdentity: id, EvidenceSource: "api", Verdict: "unknown", Reason: "missing repository or merge identity"}
		canonical, valid := repoid.Canonical(id.Repo)
		if valid && id.MergeSHA != "" {
			e, ok := payloads[revertPayloadKey{canonical, id.PR, id.MergeSHA}]
			if !ok {
				e, ok = payloads[revertPayloadKey{canonical, id.PR, ""}]
			}
			if ok {
				r.EvidenceSource = "payload"
				r.HeadRef = e.HeadRef
				r.Title = e.Title
			} else {
				r.EvidenceSource = "api"
				pr, err := gh.getPR(ctx, canonical, id.PR)
				if err != nil {
					if ctx.Err() != nil {
						return p, ctx.Err()
					}
					var status *githubStatusError
					if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
						return p, fmt.Errorf("fetch revert evidence for %s PR %d: %w", canonical, id.PR, err)
					}
					r.Reason = "GitHub PR missing (404)"
				} else if (pr.Number != 0 && pr.Number != id.PR) || pr.MergeCommitSHA != id.MergeSHA {
					r.Reason = "GitHub PR identity/merge SHA missing or differs from stored outcome"
				} else {
					r.HeadRef = pr.Head.Ref
					r.Title = pr.Title
				}
			}
			if prderive.RevertEvidence(r.HeadRef, r.Title) != "" {
				r.Verdict = "exclude"
				r.Reason = "recognised GitHub revert PR"
			} else if r.HeadRef != "" && r.Title != "" {
				r.Verdict = "keep"
				r.Reason = "no recognised GitHub revert form"
			} else if r.Reason == "missing repository or merge identity" {
				r.Reason = "head ref/title evidence incomplete"
			}
		}
		p.Candidates = append(p.Candidates, r)
		switch r.Verdict {
		case "exclude":
			p.Counts.Exclude++
		case "keep":
			p.Counts.Keep++
		default:
			p.Counts.Unknown++
		}
	}
	p.Counts.Total = len(p.Candidates)
	return p, nil
}

func applyRevertPlan(dbPath, path, actor, approved string, stdout, stderr io.Writer) int {
	fail := func(err error) int { _, _ = fmt.Fprintf(stderr, "exclude-reverts: %v\n", err); return 1 }
	raw, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p revertPlan
	if err := dec.Decode(&p); err != nil {
		return fail(err)
	}
	if !bytes.Equal(raw, p.canonicalBytes()) {
		return fail(errors.New("plan bytes differ from canonical dry-run output; use the untouched plan file"))
	}
	if err := p.validate(); err != nil {
		return fail(err)
	}
	digest := planBytesDigest(raw)
	if approved != digest {
		return fail(errors.New("approved digest differs from raw plan bytes; approve the exact dry-run output"))
	}
	probe, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fail(err)
	}
	if err := probe.Close(); err != nil {
		return fail(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	retained, err := db.RetainedPREvidence(ctx)
	if err != nil {
		return fail(err)
	}
	payloads := preferredRevertPayloads(retained)
	for _, r := range p.Candidates {
		if r.EvidenceSource != "payload" {
			continue
		}
		canonical, _ := repoid.Canonical(r.Repo)
		e, ok := payloads[revertPayloadKey{canonical, r.PR, r.MergeSHA}]
		if !ok {
			e, ok = payloads[revertPayloadKey{canonical, r.PR, ""}]
		}
		if !ok || e.HeadRef != r.HeadRef || e.Title != r.Title {
			return fail(fmt.Errorf("payload evidence differs from retained payloads for %s PR %d", r.Repo, r.PR))
		}
	}
	applied, replayed := 0, 0
	for _, r := range p.Candidates {
		if r.Verdict != "exclude" {
			continue
		}
		evidence, _ := json.Marshal(struct {
			PlanDigest string        `json:"plan_digest"`
			Row        revertPlanRow `json:"candidate"`
		}{digest, r})
		changed, err := db.ApplyPRExclusion(ctx, r.PRExclusionIdentity, string(evidence), actor)
		if err != nil {
			return fail(fmt.Errorf("%w (committed %d rows before failure; replayed %d rows; per-row atomic apply)", err, applied, replayed))
		}
		if changed {
			applied++
		} else {
			replayed++
		}
	}
	if err := json.NewEncoder(stdout).Encode(map[string]any{"plan_digest": digest, "applied": applied, "replayed": replayed, "skipped": p.Counts.Keep + p.Counts.Unknown, "actor_identity": "self-asserted operator approval"}); err != nil {
		return fail(err)
	}
	return 0
}

// Retained evidence is newest first. Prefer a usable closed+merged delivery for
// each merge SHA over later edits; exact SHA always wins over absent legacy SHA.
type revertPayloadKey struct {
	repo string
	pr   int
	sha  string
}

func preferredRevertPayloads(retained []store.RetainedPREvidence) map[revertPayloadKey]store.RetainedPREvidence {
	payloads := map[revertPayloadKey]store.RetainedPREvidence{}
	for _, e := range retained {
		if prderive.RevertEvidence(e.HeadRef, e.Title) == "" && (e.HeadRef == "" || e.Title == "") {
			continue
		}
		k := revertPayloadKey{e.Repo, e.PR, e.MergeSHA}
		old, ok := payloads[k]
		if !ok || (e.MergeSHA != "" && e.ClosedMerged && !old.ClosedMerged) {
			payloads[k] = e
		}
	}
	return payloads
}
