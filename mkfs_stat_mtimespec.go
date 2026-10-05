//go:build darwin || freebsd

package erofs

import (
	"io/fs"
	"syscall"

	"github.com/forkcloser/erofs/internal/builder"
)

// entryFromSys extracts metadata from info.Sys(). Returns nil if the type is
// not recognized, allowing the caller to use a default, and an error when the
// platform stat holds a value the on-disk format cannot. This is the
// platform family whose Stat_t spells the modification time Mtimespec.
func entryFromSys(info fs.FileInfo) (*builder.Entry, error) {
	switch sys := info.Sys().(type) {
	case *builder.Entry:
		return sys, nil
	case *syscall.Stat_t:
		return statEntry(sys.Uid, sys.Gid, sys.Dev, sys.Ino, sys.Nlink, sys.Rdev, sys.Mtimespec.Sec, sys.Mtimespec.Nsec)
	default:
		//nolint:nilnil // nil is no platform stat: each caller then picks its own default entry
		return nil, nil
	}
}
