package erofs

import "syscall"

// hostDev is the major and minor of a file's st_rdev: eight bits and
// twenty-four of a 32-bit dev_t (XNU's sys/types.h).
func hostDev(sys *syscall.Stat_t) (major, minor uint64) {
	dev := uint64(uint32(sys.Rdev)) // #nosec G115 -- the bits as they are: Rdev is a signed 32-bit dev_t

	return dev >> 24 & 0xff, dev & 0xffffff //nolint:mnd // the layout, as the header's macros spell it
}
