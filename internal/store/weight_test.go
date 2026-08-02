package store

import "testing"

// TestGitHeuristic pins the diff-size buckets that both the GitHub webhook and
// POST /api/v1/outcomes resolve weight through (#132/#188). The boundary values
// matter: a drift here silently re-scores every unlabeled outcome.
func TestGitHeuristic(t *testing.T) {
	cases := []struct {
		name         string
		lines, files int
		want         float64
	}{
		{"one-line typo", 1, 1, 0.5}, // effort 11
		{"xs boundary", 5, 1, 0.5},   // effort 15
		{"just over xs", 6, 1, 1.0},  // effort 16
		{"s bucket", 30, 2, 1.0},     // effort 50
		{"s boundary", 50, 1, 1.0},   // effort 60
		{"m bucket", 50, 3, 3.0},     // effort 80
		{"m boundary", 100, 10, 3.0}, // effort 200
		{"l bucket", 200, 5, 5.0},    // effort 250
		{"l boundary", 900, 10, 5.0}, // effort 1000
		{"xl bucket", 1000, 10, 8.0}, // effort 1100
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitHeuristic(tc.lines, tc.files); got != tc.want {
				t.Errorf("GitHeuristic(%d, %d) = %v, want %v", tc.lines, tc.files, got, tc.want)
			}
		})
	}
}

// TestResolveWeight pins the shared branching: a non-zero explicit (label /
// label-equivalent) weight wins with source 'label'; zero falls through to the
// heuristic with source 'git-heuristic'. This is the single guarantee that a
// webhook outcome and an /outcomes API outcome score identically for the same
// inputs.
func TestResolveWeight(t *testing.T) {
	cases := []struct {
		name                   string
		explicit               float64
		additions, dels, files int
		wantWeight             float64
		wantSource             string
	}{
		{"explicit label wins", 5.0, 999, 999, 99, 5.0, WeightSourceLabel},
		{"zero falls through to heuristic", 0, 50, 0, 3, 3.0, WeightSourceHeuristic},
		{"heuristic combines add+del", 0, 40, 10, 3, 3.0, WeightSourceHeuristic},
		{"explicit xs", 0.5, 0, 0, 0, 0.5, WeightSourceLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, src := ResolveWeight(tc.explicit, tc.additions, tc.dels, tc.files)
			if w != tc.wantWeight || src != tc.wantSource {
				t.Errorf("ResolveWeight(%v, %d, %d, %d) = (%v, %q), want (%v, %q)",
					tc.explicit, tc.additions, tc.dels, tc.files, w, src, tc.wantWeight, tc.wantSource)
			}
		})
	}
}
