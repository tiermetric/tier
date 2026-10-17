package issueref

import "testing"

func TestBranchHashVectors(t *testing.T) {
	for _, tc := range []struct{ repo, branch, want string }{
		{"acme/widgets", "feature/team/topic", "35e85223b771502072fff7aa9c2abb8f009cc96fbcadb3f637357da565de8976"},
		{"acme/widgets", "refs/heads/feature/team/topic", "35e85223b771502072fff7aa9c2abb8f009cc96fbcadb3f637357da565de8976"},
		{"acme/widgets", "topic", "227a0f6593dba7dbb8bd195189c73350646129acc3f3622346eb42a3eb7dc168"},
		{"acme/other", "feature/team/topic", "659e7e4e770ac6ecf2376252688b74ee515bd933b0a67d4896ee8cbf419f465e"},
	} {
		if got := BranchHash(tc.repo, tc.branch); got == nil || *got != tc.want {
			t.Fatalf("BranchHash(%q,%q) = %v, want %s", tc.repo, tc.branch, got, tc.want)
		}
	}
	for _, branch := range []string{"", "main", "master", "HEAD", "detached", "refs/heads/main"} {
		if BranchHash("acme/widgets", branch) != nil {
			t.Fatalf("hashed default/unknown %q", branch)
		}
	}
	if BranchHash("unqualified", "feature/team/topic") != nil {
		t.Fatal("hashed unqualified repo")
	}
}
