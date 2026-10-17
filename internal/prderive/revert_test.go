package prderive

import "testing"

func TestRevertEvidence(t *testing.T) {
	for _, tc := range []struct {
		head, title string
		want        bool
	}{
		{"revert-140664-pre-quota", "renamed", true},
		{"feature/42", `Revert "original"`, true},
		{"feature/42", "Revert to v2 API…", false},
		{"revert-x-topic", "normal", false},
		{"topic/revert-42-x", "normal", false},
		{"revert-42", "normal", false},
		{"Revert-42-topic", "normal", false},
		{"normal", ` Revert "original"`, false},
	} {
		t.Run(tc.head+tc.title, func(t *testing.T) {
			if got := RevertEvidence(tc.head, tc.title) != ""; got != tc.want {
				t.Fatalf("recognised=%v want=%v", got, tc.want)
			}
		})
	}
}
