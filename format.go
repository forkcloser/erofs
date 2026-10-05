package erofs

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/forkcloser/erofs/internal/disk"
)

// xattrSplit splits a full xattr name into (NameIndex, suffix). The prefixes
// are the ones xattrIndex spells, so the two directions cannot drift.
func xattrSplit(name string) (uint8, string) {
	for idx := xattrIndex(1); idx <= xattrIndexLast; idx++ {
		if p := idx.String(); strings.HasPrefix(name, p) {
			return uint8(idx), name[len(p):]
		}
	}

	return 0, name
}

// On-disk xattr field limits. erofs_xattr_entry stores the name suffix
// length in a uint8 and the value length in a uint16, and an inode's
// i_xattr_icount is a uint16 counting 4-byte units past the header. Linux
// applies the same bounds (XATTR_NAME_MAX 255, XATTR_SIZE_MAX 65536), so
// rejecting beyond them costs no real capability.
const (
	maxXattrNameLen  = 255
	maxXattrValueLen = 65535
	maxXattrICount   = 65535
)

// validateXattr checks a single attribute against the on-disk field widths.
// Without this the lengths are silently truncated into their fields while the
// full bytes are still written, so the recorded and actual sizes disagree and
// everything after that entry is misparsed — fsck.erofs reports "xattr entry
// beyond xattr_isize".
func validateXattr(name, value string) error {
	_, suffix := xattrSplit(name)
	if len(suffix) > maxXattrNameLen {
		return fmt.Errorf("xattr name %q is %d bytes after its prefix, over the %d byte on-disk limit: %w",
			name, len(suffix), maxXattrNameLen, ErrInvalid)
	}

	if len(value) > maxXattrValueLen {
		return fmt.Errorf("xattr %q has a %d byte value, over the %d byte on-disk limit: %w",
			name, len(value), maxXattrValueLen, ErrInvalid)
	}

	return nil
}

// xattrEntrySize returns the on-disk size of a single xattr entry, padded to 4 bytes.
func xattrEntrySize(name, value string) int {
	_, suffix := xattrSplit(name)

	sz := disk.SizeXattrEntry + len(suffix) + len(value)
	if sz%4 != 0 {
		sz = (sz + 3) & ^3
	}

	return sz
}

// calcXattrSize returns the total xattr area size (header + entries), or 0.
func calcXattrSize(e *erofsEntry) int {
	if len(e.xattrs) == 0 {
		return 0
	}

	entriesSize := 0
	for name, value := range e.xattrs {
		entriesSize += xattrEntrySize(name, value)
	}

	return disk.SizeXattrBodyHeader + entriesSize
}

// xattrICount is the value i_xattr_icount encodes for an xattr area of the
// given size, computed in int so an area too large to represent can be
// detected before it is narrowed into the uint16 field.
func xattrICount(xattrSize int) int {
	if xattrSize == 0 {
		return 0
	}

	return (xattrSize-disk.SizeXattrBodyHeader)/disk.SizeXattrEntry + 1
}

// xattrCount encodes the xattr area size into the inode XattrCount field.
// checkLimits has already rejected areas too large for the field.
func xattrCount(xattrSize int) uint16 {
	return uint16(xattrICount(xattrSize))
}

// sortedXattrKeys returns xattr keys in deterministic order.
func sortedXattrKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// inodeFormat builds the Format field: bit 0 = extended, bits 1-3 = layout.
func inodeFormat(layout uint8, compact bool) uint16 {
	f := uint16(layout) << 1
	if !compact {
		f |= disk.InodeFormatExtended
	}

	return f
}

// goModeToUnixMode converts Go fs.FileMode to Unix mode bits.
func goModeToUnixMode(m fs.FileMode) uint16 {
	mode := uint16(m.Perm())

	if m&fs.ModeSetuid != 0 {
		mode |= disk.StatTypeIsUID
	}

	if m&fs.ModeSetgid != 0 {
		mode |= disk.StatTypeIsGID
	}

	if m&fs.ModeSticky != 0 {
		mode |= disk.StatTypeIsVTX
	}

	//nolint:exhaustive // fs.FileMode is a bit set, not an enum: m.Type() yields only the type bits
	switch m.Type() {
	case 0: // regular file
		mode |= disk.StatTypeReg
	case fs.ModeDir:
		mode |= disk.StatTypeDir
	case fs.ModeSymlink:
		mode |= disk.StatTypeSymlink
	case fs.ModeDevice | fs.ModeCharDevice:
		mode |= disk.StatTypeChrdev
	case fs.ModeDevice:
		mode |= disk.StatTypeBlkdev
	case fs.ModeNamedPipe:
		mode |= disk.StatTypeFifo
	case fs.ModeSocket:
		mode |= disk.StatTypeSock
	default:
		// fs.ModeIrregular, or type bits in a combination no FileInfo
		// produces: the mode carries no type.
	}

	return mode
}

// modeToFileType converts Unix mode bits to an EROFS file type.
func modeToFileType(mode uint16) uint8 {
	switch mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		return disk.FileTypeReg
	case disk.StatTypeDir:
		return disk.FileTypeDir
	case disk.StatTypeChrdev:
		return disk.FileTypeChrdev
	case disk.StatTypeBlkdev:
		return disk.FileTypeBlkdev
	case disk.StatTypeFifo:
		return disk.FileTypeFifo
	case disk.StatTypeSock:
		return disk.FileTypeSock
	case disk.StatTypeSymlink:
		return disk.FileTypeSymlink
	default:
		return 0
	}
}
