//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package erofs

import (
	"io/fs"

	"github.com/forkcloser/erofs/internal/builder"
)

// entryFromSys extracts metadata from info.Sys(). On platforms with no
// syscall.Stat_t this package understands, only a *builder.Entry the source
// supplies itself carries ownership and link identity; mode, size and
// modification time still come from the fs.FileInfo.
func entryFromSys(info fs.FileInfo) *builder.Entry {
	if be, ok := info.Sys().(*builder.Entry); ok {
		return be
	}
	return nil
}
