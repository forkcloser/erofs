//go:build darwin || freebsd || linux

package erofs

import (
	"fmt"
	"io/fs"
	"syscall"

	"github.com/forkcloser/go-erofs/internal/builder"
)

// statInt is any integer a syscall.Stat_t field might be: the same field is
// a different width on nearly every platform, and sometimes signed.
type statInt interface {
	~int16 | ~int32 | ~int64 | ~uint16 | ~uint32 | ~uint64
}

// statEntry builds the Entry for a host file from the fields every unix
// stat carries, widened or narrowed to the on-disk widths. Generic so each
// platform file can pass its fields as they are, whatever their width. rdev
// is the device number already in the on-disk encoding (onDiskDev).
func statEntry[D, I, N, S statInt](
	uid, gid uint32, dev D, ino I, nlink N, rdev uint32, sec, nsec S,
) *builder.Entry {
	return &builder.Entry{
		UID:     uid,
		GID:     gid,
		Mtime:   uint64(sec),
		MtimeNs: uint32(nsec),
		Nlink:   uint32(nlink),
		Rdev:    rdev,
		Dev:     uint64(dev),
		Ino:     uint64(ino),
	}
}

// onDiskDev is a device number as i_rdev holds it: the kernel's
// new_encode_dev of a 12-bit major and a 20-bit minor (kdev_t.h). Every
// platform lays a dev_t out its own way, so the host's is taken apart by
// hostDev and put together here: copied as it was, a darwin /dev/null,
// (3, 2), is (0, 12290) to the kernel. Only a device has a number: the
// field of any other file is left at zero, as the kernel reads it.
func onDiskDev(info fs.FileInfo, sys *syscall.Stat_t) (uint32, error) {
	if info.Mode()&fs.ModeDevice == 0 {
		return 0, nil
	}

	major, minor := hostDev(sys)
	if major > 0xfff || minor > 0xfffff {
		return 0, fmt.Errorf("device %d:%d does not fit i_rdev, a major of 12 bits and a minor of 20: %w",
			major, minor, ErrInvalid)
	}

	// #nosec G115 -- 12 bits of major and 20 of minor, bounded just above
	return uint32(minor&0xff | major<<8 | (minor&^0xff)<<12), nil //nolint:mnd // new_encode_dev
}
