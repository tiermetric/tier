package dashboard

import (
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/diagnostics"
)

func TestAttributionCoverageExplanationLink(t *testing.T) {
	// Use the same embedded assets the HTTP handler serves.
	html, js := indexHTML, dashboardJS
	if !strings.Contains(string(html), `href="`+diagnostics.WorktreeExplanationURL+`"`) {
		t.Fatal("banner does not link to doctor's explanation")
	}
	if !strings.Contains(string(js), "help.textContent =") {
		t.Fatal("explanation link must use textContent")
	}
	if !strings.Contains(string(html), `id="attr-coverage-help" hidden`) || !strings.Contains(string(js), "help.hidden = share === null || share === ATTR_NOT_SHOWN") {
		t.Fatal("help must be hidden when no explanation is shown")
	}
}
