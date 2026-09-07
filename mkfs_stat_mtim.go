//go:build dragonfly || illumos || linux || openbsd || solaris

package erofs

import (
	"io/fs"
	"syscall"

	"github.com/forkcloser/erofs/internal/builder"
)

// entryFromSys extracts metadata from info.Sys(). Returns nil if the type is
// not recognized, allowing the caller to use a default. This is the
// platform family whose Stat_t spells the modification time Mtim.
func entryFromSys(info fs.FileInfo) *builder.Entry {
	switch sys := info.Sys().(type) {
	case *builder.Entry:
		return sys
	case *syscall.Stat_t:
		return statEntry(sys.Uid, sys.Gid, sys.Dev, sys.Ino, sys.Nlink, sys.Rdev, sys.Mtim.Sec, sys.Mtim.Nsec)
	default:
		return nil
	}
}
