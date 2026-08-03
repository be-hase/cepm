//go:build unix

package cli

import (
	"io/fs"
	"syscall"
)

// deviceOf returns the filesystem a file lives on, or false when the
// platform does not say.
func deviceOf(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// inodeOf returns the file's inode, or false when the platform does not say.
func inodeOf(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ino, true
}
