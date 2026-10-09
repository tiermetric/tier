//go:build !unix

package collector

import "os"

// openNonblock is unix-only; there is no FIFO to block on here.
const openNonblock = 0

// ownedBy cannot read an owner uid on this platform, so no linked worktree
// verifies here: each one classifies as Unresolvable.
func ownedBy(os.FileInfo, int) bool { return false }
