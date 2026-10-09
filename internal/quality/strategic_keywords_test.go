package quality

import (
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// S10-1: substrings are not evidence of a business decision. Keep the intended
// strategic inflections and punctuation/case handling while bounding words.
func TestClassifyRevert_StrategicWholeWords(t *testing.T) {
	for _, message := range []string{
		"Revert apivot adapter",
		"Revert pivotable adapter",
		"Revert repivot helper",
		"Revert sunsetter integration",
		"Revert undeprecated endpoint",
		"Revert deprecator helper",
		"Revert byproduct decision logic",
		"Revert XPM requested change",
		"Revert feature flag disabledness check",
		"Revert business requirement unchanged",
		"Revert removing featurette",
		"Revert no longer neededness check",
		"Revert unreplaced bystander",
		"Revert pivotingly named helper",
		"Revert sunsettingly named helper",
		"Revert product decisionsuffix logic",
		"Revert feature flagship disabled",
		"Revert feature flags redisabled",
		"Revert feature flags disablingness check",
		"Revert business requirements unchanged",
		"Revert business requirements changeless",
		"Revert removing featuresuffix",
	} {
		t.Run(message, func(t *testing.T) {
			if got := ClassifyRevert(message); got != EventRevertQuality {
				t.Errorf("ClassifyRevert(%q) = %q, want conservative quality default", message, got)
			}
		})
	}
	for _, message := range []string{
		"Revert: (PIVOT).", "Revert: pivots", "Revert: pivoted", "Revert: pivoting",
		"Revert: sunset", "Revert: sunsets", "Revert: sunsetted", "Revert: sunsetting",
		"Revert: deprecate",
		"Revert: deprecated", "Revert: deprecates", "Revert: deprecating",
		"Revert: deprecation", "Revert: deprecations",
		"Revert: product decision", "Revert: product decisions", "Revert: PM requested",
		"Revert: feature flag disable", "Revert: feature flag disables",
		"Revert: feature flag was disabled", "Revert: feature flag disabling",
		"Revert: feature flags disable", "Revert: feature flags disables",
		"Revert: feature flags disabled", "Revert: feature flags disabling",
		"Revert: business requirement change", "Revert: business requirement has changed",
		"Revert: business requirements change", "Revert: business requirements changed",
		"Revert: removing feature", "Revert: removing features",
		"Revert: no longer needed", "Revert: replaced by another feature",
	} {
		t.Run(message, func(t *testing.T) {
			if got := ClassifyRevert(message); got != EventRevertStrategic {
				t.Errorf("ClassifyRevert(%q) = %q, want strategic", message, got)
			}
			if got := Resolve([]store.QualityEvent{ev(ClassifyRevert(message), "revert-sha")}); got != 0.8 {
				t.Errorf("Resolve(ClassifyRevert(%q)) = %v, want strategic floor 0.8", message, got)
			}
		})
	}
	if got := ClassifyRevert("product decision after a crash"); got != EventRevertQuality {
		t.Errorf("mixed strategic and quality evidence = %q, want quality", got)
	}
}
