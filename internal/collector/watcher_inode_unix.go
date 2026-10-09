//go:build unix

package collector

import (
	"os"
	"syscall"
)

// inodeOf extracts the file's inode number from os.FileInfo. Returns 0 on
// platforms where Stat doesn't expose an inode (Windows); that fallback lives
// in watcher_inode_other.go. The incremental tailing path treats inode=0 as
// "we can't tell if this is the same file" and always full-re-parses -- losing
// the optimization but staying correct.
//
// tier runs on Unix in production, so this inode path is the production case.
func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}
