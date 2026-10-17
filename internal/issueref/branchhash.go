package issueref

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/tiermetric/tier/internal/repoid"
)

// BranchHash is the shared event/outcome branch key. Repo must be the event's
// canonical owner/repo. Only refs/heads/ is removed; full branch names matter.
// This is pseudonymisation, not secrecy: guessable names can be recovered.
func BranchHash(repo, branch string) *string {
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if !repoid.IsReal(repo) || IsExploratoryBranch(branch) || branch == "detached" {
		return nil
	}
	sum := sha256.Sum256([]byte(repo + "\x00" + branch))
	key := hex.EncodeToString(sum[:])
	return &key
}
