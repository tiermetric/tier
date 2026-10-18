package qualityrule

import (
	"strconv"
	"strings"
)

type CIKey struct {
	HeadSHA    string
	WorkflowID string
}

type CIRef struct {
	CIKey
	RunAttempt int
}

func BuildCISourceRef(headSHA string, runAttempt int, workflowID int64) string {
	if runAttempt < 0 {
		runAttempt = 0
	}
	ref := headSHA + ":" + strconv.Itoa(runAttempt)
	if workflowID <= 0 {
		return ref
	}
	return ref + ":" + strconv.FormatInt(workflowID, 10)
}

func ParseCIRef(sourceRef string) CIRef {
	if wfAt := strings.LastIndexByte(sourceRef, ':'); wfAt > 0 {
		if attemptAt := strings.LastIndexByte(sourceRef[:wfAt], ':'); attemptAt >= 0 {
			attempt, aok := parseDecimal(sourceRef[attemptAt+1 : wfAt])
			if aok && isPositiveDecimal(sourceRef[wfAt+1:]) {
				return CIRef{
					CIKey:      CIKey{HeadSHA: sourceRef[:attemptAt], WorkflowID: sourceRef[wfAt+1:]},
					RunAttempt: attempt,
				}
			}
		}
		if attempt, ok := parseDecimal(sourceRef[wfAt+1:]); ok {
			return CIRef{CIKey: CIKey{HeadSHA: sourceRef[:wfAt]}, RunAttempt: attempt}
		}
	}
	if i := strings.IndexByte(sourceRef, ':'); i >= 0 {
		return CIRef{CIKey: CIKey{HeadSHA: sourceRef[:i]}}
	}
	return CIRef{CIKey: CIKey{HeadSHA: sourceRef}}
}

func isPositiveDecimal(s string) bool {
	nonZero := false
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		if s[i] != '0' {
			nonZero = true
		}
	}
	return nonZero
}

func parseDecimal(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false // overflow on an absurdly long digit run
	}
	return n, true
}
