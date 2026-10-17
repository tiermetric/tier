package issueref

import "testing"

func TestFromCommitSubjectPR(t *testing.T) {
	for _, tc := range []struct {
		subject string
		want    int
	}{
		{"fix: widget (#42)", 42}, {"Merge pull request #42 from branch", 42},
		{"#42", 42}, {"fix: widget (#42) closes #9", 0},
		{"fix: PROJ-9 (#42)", 0}, {"fix: widget (#0)", 0},
		{"fix: widget (#042)", 0}, {"fix: widget (#42foo)", 0},
		{"fix: widget (#9999999999999999999999999)", 0},
	} {
		got := FromCommitSubjectPR(tc.subject)
		if tc.want == 0 {
			if got != nil {
				t.Errorf("%q = %d, want nil", tc.subject, *got)
			}
		} else if got == nil || *got != tc.want {
			t.Errorf("%q = %v, want %d", tc.subject, got, tc.want)
		}
	}
}
