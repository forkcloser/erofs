//go:build darwin || freebsd || linux

package erofs

import (
	"io/fs"
	"syscall"
)

// hostFile reports a file the host's own stat describes: never a file of
// an image, so CopyFrom need not open it to find out.
func hostFile(info fs.FileInfo) bool {
	_, ok := info.Sys().(*syscall.Stat_t)

	return ok
}
