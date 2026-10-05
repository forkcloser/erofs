//go:build darwin || freebsd || linux

package erofs

import (
	"fmt"

	"github.com/forkcloser/erofs/internal/builder"
)

// statInt is any integer a syscall.Stat_t field might be: the same field is
// a different width on nearly every platform, and sometimes signed.
type statInt interface {
	~int16 | ~int32 | ~int64 | ~uint16 | ~uint32 | ~uint64
}

// statEntry builds the Entry for a host file from the fields every unix
// stat carries, widened or narrowed to the on-disk widths. Generic so each
// platform file can pass its fields as they are, whatever their width.
func statEntry[D, I, N, R, S statInt](
	uid, gid uint32, dev D, ino I, nlink N, rdev R, sec, nsec S,
) (*builder.Entry, error) {
	// i_rdev is 32 bits on disk. A 64-bit dev_t keeps a major past 4095 or a
	// minor past 2^20-1 in its upper half, which narrowing would drop and so
	// name another device; a 32-bit dev_t always survives the round trip.
	if R(uint32(rdev)) != rdev {
		return nil, fmt.Errorf("rdev %#x does not fit the 32-bit on-disk field: %w", rdev, ErrInvalid)
	}

	return &builder.Entry{
		UID:     uid,
		GID:     gid,
		Mtime:   uint64(sec),
		MtimeNs: uint32(nsec),
		Nlink:   uint32(nlink),
		Rdev:    uint32(rdev),
		Dev:     uint64(dev),
		Ino:     uint64(ino),
	}, nil
}
