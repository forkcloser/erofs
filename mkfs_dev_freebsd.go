package erofs

import "syscall"

// hostDev is the major and minor of a file's st_rdev, as FreeBSD's __major
// and __minor take them from a 64-bit dev_t (sys/types.h).
func hostDev(sys *syscall.Stat_t) (major, minor uint64) {
	dev := sys.Rdev

	return dev>>32&0xffffff00 | dev>>8&0xff, dev>>24&0xff00 | dev&0xffff00ff
}
