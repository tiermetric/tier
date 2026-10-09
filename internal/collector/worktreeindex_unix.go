//go:build unix

package collector

import (
	"os"
	"syscall"
)

// openNonblock makes opening a FIFO return at once instead of waiting for a
// writer; it changes nothing for a regular file.
const openNonblock = syscall.O_NONBLOCK

// ownedBy reports whether info's owner is uid. An owner that cannot be read
// is not a match.
func ownedBy(info os.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uid >= 0 && int64(st.Uid) == int64(uid)
}
