// Package diagnostics describes captured records without changing attribution.
package diagnostics

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tiermetric/tier/internal/store"
)

const WorktreeExplanation = "Sessions and subagents started in the main checkout can record main even when the work happens in linked worktrees. Start agents and sessions in the worktree. tier#823 worktree attribution is available when explicitly enabled; it does not correct already assigned historical issues."
const WorktreeExplanationURL = "/docs/technical-reference.html#captured-dollar-diagnostics"

// CapturedDollars measures only captured token-event dollars (manual API costs
// excluded). Engine enablement describes this server now, not historical capture
// or remote shippers. Missing configuration is explicitly unknown.
type CapturedDollars struct {
	ShareLabel                string            `json:"share_label"`
	Since                     string            `json:"since"`
	Until                     string            `json:"until,omitempty"`
	Scope                     string            `json:"scope"`
	StoredCWDSampleSize       int64             `json:"stored_cwd_sample_size"`
	StoredCWDCostShare        float64           `json:"stored_cwd_cost_share"`
	SampleSize                int64             `json:"sample_size"`
	CapturedCostUSD           float64           `json:"captured_cost_usd"`
	EngineEnablement          map[string]string `json:"engine_enablement"`
	EnablementScope           string            `json:"enablement_scope"`
	Buckets                   []CapturedBucket  `json:"buckets"`
	MainCheckoutWorktreeShare float64           `json:"main_checkout_worktree_share"`
}
type CapturedBucket struct {
	Engine     string  `json:"engine"`
	Bucket     string  `json:"bucket"`
	Origin     string  `json:"subagent_origin"`
	SampleSize int64   `json:"sample_size"`
	CostUSD    float64 `json:"captured_cost_usd"`
	Share      float64 `json:"share"`
}

func BuildCaptured(rows []store.CapturedDollarRow, since, until, scope string, enabled map[string]string) *CapturedDollars {
	if scope == "" {
		scope = "fleet"
	}
	d := &CapturedDollars{ShareLabel: "share of captured dollars", Since: since, Until: until, Scope: scope, EnablementScope: "current server only; remote shippers and historical enablement unknown", EngineEnablement: map[string]string{}, Buckets: []CapturedBucket{}}
	for _, e := range []string{"claude-code", "codex-rollout", "opencode", "muse"} {
		d.EngineEnablement[e] = "unknown"
	}
	for e, state := range enabled {
		d.EngineEnablement[e] = state
	}
	type key struct{ engine, bucket, origin string }
	groups := map[key]CapturedBucket{}
	groupMicro := map[key]int64{}
	var total, main, cwdMicro int64
	checked := map[string]bool{}
	for _, r := range rows {
		total += r.CostMicro
		d.SampleSize += r.SampleSize
		if r.CWD != "" {
			d.StoredCWDSampleSize += r.SampleSize
			cwdMicro += r.CostMicro
		}
		if _, ok := d.EngineEnablement[r.Engine]; !ok {
			d.EngineEnablement[r.Engine] = "unknown"
		}
		// A cwd is evidence of where the session started, never of the issue it
		// worked on. Disk metadata establishes only that linked worktrees exist now.
		if r.Bucket == "unattributed:main" && r.CWD != "" {
			found, ok := checked[r.CWD]
			if !ok {
				found = MainCheckoutWithWorktrees(r.CWD)
				checked[r.CWD] = found
			}
			if found {
				main += r.CostMicro
			}
		}
		if !store.IsUnattributed(r.Bucket) {
			continue
		}
		k := key{r.Engine, r.Bucket, r.Origin}
		b := groups[k]
		b.Engine = r.Engine
		b.Bucket = r.Bucket
		b.Origin = r.Origin
		b.SampleSize += r.SampleSize
		groupMicro[k] += r.CostMicro
		groups[k] = b
	}
	d.CapturedCostUSD = store.MicroToDollars(total)
	if total > 0 {
		d.MainCheckoutWorktreeShare = float64(main) / float64(total)
		d.StoredCWDCostShare = float64(cwdMicro) / float64(total)
	}
	for k, b := range groups {
		b.CostUSD = store.MicroToDollars(groupMicro[k])
		if total > 0 {
			b.Share = float64(groupMicro[k]) / float64(total)
		}
		d.Buckets = append(d.Buckets, b)
	}
	sort.Slice(d.Buckets, func(i, j int) bool {
		a, b := d.Buckets[i], d.Buckets[j]
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		if a.Bucket != b.Bucket {
			return a.Bucket < b.Bucket
		}
		return a.Origin < b.Origin
	})
	return d
}

// MainCheckoutWithWorktrees reads only bounded git metadata at a stored cwd.
// It verifies the linked checkout's backlink; stale or unrelated metadata is
// insufficient. No git subprocess, transcript read, or maintenance operation.
func MainCheckoutWithWorktrees(cwd string) bool {
	if !filepath.IsAbs(cwd) {
		return false
	}
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	// A cwd inside the main checkout is also a main-checkout session.
	for {
		gitdir := filepath.Join(root, ".git")
		if info, err := os.Lstat(gitdir); err == nil {
			if !info.IsDir() {
				return false
			} // linked checkouts have a .git file
			entries, err := os.ReadDir(filepath.Join(gitdir, "worktrees"))
			if err != nil {
				return false
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				admin := filepath.Join(gitdir, "worktrees", e.Name())
				link, ok := smallRegular(filepath.Join(admin, "gitdir"))
				if !ok {
					continue
				}
				linkedGit := strings.TrimSpace(link)
				if !filepath.IsAbs(linkedGit) || filepath.Base(linkedGit) != ".git" {
					continue
				}
				back, ok := smallRegular(linkedGit)
				if !ok {
					continue
				}
				if !strings.HasPrefix(back, "gitdir: ") {
					continue
				}
				target := strings.TrimSpace(strings.TrimPrefix(back, "gitdir: "))
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(linkedGit), target)
				}
				actual, err := filepath.EvalSymlinks(target)
				if err == nil && actual == admin {
					return true
				}
			}
			return false
		}
		parent := filepath.Dir(root)
		if parent == root {
			return false
		}
		root = parent
	}
}
func smallRegular(path string) (string, bool) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 4096 {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || !os.SameFile(before, info) {
		return "", false
	}
	b := make([]byte, 4097)
	n, err := f.Read(b)
	if err != nil || n > 4096 {
		return "", false
	}
	return string(b[:n]), true
}
