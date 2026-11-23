//go:build !(darwin || freebsd || linux || windows)

package erofs

import "io/fs"

// hostFile reports a file the host's own stat describes. No stat is known
// here: every file is opened to learn whether it is an image's.
func hostFile(fs.FileInfo) bool { return false }
