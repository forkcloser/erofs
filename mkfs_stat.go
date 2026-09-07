//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package erofs

import "github.com/forkcloser/erofs/internal/builder"

// statInt is any integer a syscall.Stat_t field might be: the same field is
// a different width on nearly every platform, and sometimes signed.
type statInt interface {
	~int16 | ~int32 | ~int64 | ~uint16 | ~uint32 | ~uint64
}

// statEntry builds the Entry for a host file from the fields every unix
// stat carries, widened or narrowed to the on-disk widths. Generic so each
// platform file can pass its fields as they are, whatever their width.
func statEntry[D, I, N, R, S statInt](uid, gid uint32, dev D, ino I, nlink N, rdev R, sec, nsec S) *builder.Entry {
	return &builder.Entry{
		UID:     uid,
		GID:     gid,
		Mtime:   uint64(sec),
		MtimeNs: uint32(nsec),
		Nlink:   uint32(nlink),
		Rdev:    uint32(rdev),
		Dev:     uint64(dev),
		Ino:     uint64(ino),
	}
}
