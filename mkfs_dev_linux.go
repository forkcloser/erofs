package erofs

import "syscall"

// hostDev is the major and minor of a file's st_rdev, as glibc's
// gnu_dev_major and gnu_dev_minor take them from a 64-bit dev_t
// (bits/sysmacros.h).
func hostDev(sys *syscall.Stat_t) (major, minor uint64) {
	dev := uint64(sys.Rdev) //nolint:unconvert // Rdev is not a uint64 on every Linux architecture

	return dev>>8&0xfff | dev>>32&0xfffff000, dev&0xff | dev>>12&0xffffff00
}
