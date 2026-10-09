//go:build !unix

package collector

import "os"

// inodeOf returns 0 on platforms where os.FileInfo.Sys() does not expose an
// inode (e.g. Windows). The incremental tailing path treats inode=0 as "we
// can't tell if this is the same file" and always full-re-parses -- losing the
// optimization but staying correct. This is the documented fallback contract;
// the real inode-returning implementation lives in watcher_inode_unix.go.
func inodeOf(_ os.FileInfo) uint64 {
	return 0
}
