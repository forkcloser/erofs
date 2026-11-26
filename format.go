package erofs

import (
	"cmp"
	"fmt"
	"hash/maphash"
	"io/fs"
	"math"
	"math/bits"
	"slices"
	"strings"

	"github.com/forkcloser/go-erofs/internal/disk"
)

// xattrSplit splits a full xattr name into (NameIndex, suffix), by the
// prefixes the kernel lists. Index 0 with the name whole: it has none of
// them, and stored so no mount would show it.
//
// A prefix that ends in a dot takes what follows it. The two POSIX ACL
// names are whole names: the kernel reads an ACL under its index with the
// empty name alone (xattr.c: erofs_get_acl), so one stored with anything
// after it would be listed and served to no call.
func xattrSplit(name string) (uint8, string) {
	for idx := uint8(1); idx <= lastListedIndex; idx++ {
		p, ok := builtinPrefix(idx)

		switch {
		case !ok:
		case name == p:
			return idx, ""
		case strings.HasSuffix(p, ".") && strings.HasPrefix(name, p):
			return idx, name[len(p):]
		}
	}

	return 0, name
}

// xattr is one extended attribute of an entry the Writer holds. An entry's
// are in the order of their names, the order they are written in.
type xattr struct {
	name, value string
}

// xattrMap is attrs as the map a caller is handed, its own; nil for none.
func xattrMap(attrs []xattr) map[string]string {
	if len(attrs) == 0 {
		return nil
	}

	m := make(map[string]string, len(attrs))
	for _, x := range attrs {
		m[x.name] = x.value
	}

	return m
}

// xattrAt is where name is among attrs, or where it would go.
func xattrAt(attrs []xattr, name string) (int, bool) {
	return slices.BinarySearchFunc(attrs, name, func(x xattr, name string) int {
		return cmp.Compare(x.name, name)
	})
}

// listedXattrs is attrs without the names the kernel serves under no prefix,
// in attrs' own array: there are nearly always none.
func listedXattrs(attrs []xattr) []xattr {
	return slices.DeleteFunc(attrs, func(x xattr) bool {
		idx, _ := xattrSplit(x.name)

		return idx == 0
	})
}

// byXattrName orders attributes as they are written.
func byXattrName(a, b xattr) int { return cmp.Compare(a.name, b.name) }

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
	idx, suffix := xattrSplit(name)
	// The format would store it, under index 0, and the kernel would list
	// it on no mount and find it for no getxattr: an attribute written to
	// be lost.
	if idx == 0 {
		return fmt.Errorf(
			"xattr name %q has none of the prefixes the kernel serves (user., trusted., security., "+
				"system.posix_acl_access, system.posix_acl_default): %w", name, ErrInvalid,
		)
	}
	// Linux refuses a bare namespace prefix ("user.", "trusted.") with
	// EINVAL, and it cannot be addressed by getxattr once stored; the two
	// POSIX ACL names are whole names the format indexes, and carry no
	// suffix by design.
	if prefix, _ := builtinPrefix(idx); suffix == "" && strings.HasSuffix(prefix, ".") {
		return fmt.Errorf("xattr name %q has nothing after its prefix: %w", name, ErrInvalid)
	}

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

// sharedXattrOver is how many inodes may carry an attribute before it goes
// to the shared area: one that more than two carry is stored once there,
// mkfs.erofs's default (-x2). maxSharedXattrs is how many an inode can name
// there, its h_shared_count being one byte.
const (
	sharedXattrOver = 2
	maxSharedXattrs = 255
)

// xattrSharing is how an erofsWriter knows which attributes are in the
// shared xattr area, for the price of one hash of an attribute most
// inodes carry alone, and less for one they nearly all carry.
type xattrSharing struct {
	seed maphash.Seed
	// seen counts, to sharedXattrOver+1 and no further, the inodes whose
	// attributes hash to each of its slots: an attribute whose slot is not
	// past sharedXattrOver is carried by too few to be shared, whatever
	// else fell there.
	seen []uint8
	// ids is where each shared attribute is in the area, in units of four
	// bytes.
	ids map[xattr]uint32
	// last is the attribute last asked about at each of an inode's first
	// places, and what was answered: the files of a tree carry the same
	// label one after another, and equal strings that are one string
	// compare at once.
	last [xattrPlaces]xattrAnswer
}

// xattrPlaces is how many of an inode's first attributes are remembered
// from the inode before it, where they are asked about and where they are
// counted.
const xattrPlaces = 4

// xattrAnswer is what xattrSharing.id answered for x, once asked.
type xattrAnswer struct {
	x      xattr
	id     uint32
	shared bool
	asked  bool
}

// xattrRun is an attribute that so many inodes in a row carry at one place.
type xattrRun struct {
	x      xattr
	inodes int
}

// slot is where x counts in seen.
func (s *xattrSharing) slot(x xattr) *uint8 {
	var h maphash.Hash

	h.SetSeed(s.seed)
	_, _ = h.WriteString(x.name)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(x.value)

	return &s.seen[h.Sum64()&uint64(len(s.seen)-1)] // #nosec G115 -- a length, a power of two, is not negative
}

// id is where the attribute x, the i-th of an inode, is in the shared area,
// and whether it is there.
func (s *xattrSharing) id(i int, x xattr) (uint32, bool) {
	if len(s.ids) == 0 {
		return 0, false
	}

	if i < len(s.last) && s.last[i].asked && s.last[i].x == x {
		return s.last[i].id, s.last[i].shared
	}

	var (
		id     uint32
		shared bool
	)

	if *s.slot(x) > sharedXattrOver {
		id, shared = s.ids[x]
	}

	if i < len(s.last) {
		s.last[i].x, s.last[i].id, s.last[i].shared, s.last[i].asked = x, id, shared, true
	}

	return id, shared
}

// shareXattrs chooses the shared xattr area: the attributes, name and
// value, that more than sharedXattrOver of the entries carry, in the order
// of their names and then of their values.
func (w *erofsWriter) shareXattrs() {
	total := 0
	for _, e := range w.entries {
		total += len(e.xattrs)
	}

	w.shared, w.sharedSize = w.shared[:0], 0
	w.sharing = xattrSharing{seed: maphash.MakeSeed()}

	if total == 0 {
		return
	}

	// Four slots an attribute: few fall together that are not the same.
	w.sharing.seen = make([]uint8, 1<<bits.Len(uint(4*total-1)))

	ids := w.countCarried()

	w.shared = slices.Grow(w.shared, len(ids))

	for x, inodes := range ids {
		if inodes > sharedXattrOver {
			w.shared = append(w.shared, x)

			continue
		}

		delete(ids, x)
	}

	slices.SortFunc(w.shared, func(a, b xattr) int {
		return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.value, b.value))
	})

	for i, x := range w.shared {
		// An id is 32 bits, of four bytes each: what would lie past them
		// stays in the inodes that carry it.
		if w.sharedSize/4 > math.MaxUint32 {
			for _, left := range w.shared[i:] {
				delete(ids, left)
			}

			w.shared = w.shared[:i]

			break
		}

		ids[x] = uint32(w.sharedSize / 4) // #nosec G115 -- held to 32 bits just above
		w.sharedSize += xattrEntrySize(x.name, x.value)
	}

	w.sharing.ids = ids
}

// countCarried fills seen and counts, by name and value, the inodes that
// carry each attribute whose slot is past sharedXattrOver, to
// sharedXattrOver+1 and no further. An attribute of an inode that cannot
// hold its own counts as past it, however few carry it: an inode's body is
// sized in 65535 units of four bytes, and what does not fit it whole is
// named in the shared area, as an image may store it.
//
// The table is the one shareXattrs then keeps the ids in, and it is made
// once, at the count of the slots that pass sharedXattrOver: about as many
// as the attributes it will hold.
func (w *erofsWriter) countCarried() map[xattr]uint32 {
	past := 0

	w.eachCarried(func(over bool, x xattr, inodes int) {
		slot := w.sharing.slot(x)
		was := *slot

		*slot = uint8(min(int(was)+inodes, sharedXattrOver+1)) // #nosec G115 -- at most sharedXattrOver+1
		if over {
			*slot = sharedXattrOver + 1
		}

		if was <= sharedXattrOver && *slot > sharedXattrOver {
			past++
		}
	})

	counts := make(map[xattr]uint32, past)

	w.eachCarried(func(over bool, x xattr, inodes int) {
		switch {
		case over:
			counts[x] = sharedXattrOver + 1
		case *w.sharing.slot(x) > sharedXattrOver:
			counts[x] = uint32(
				min(int(counts[x])+inodes, sharedXattrOver+1),
			) // #nosec G115 -- at most sharedXattrOver+1
		}
	})

	return counts
}

// eachCarried calls fn for each attribute the entries carry, with how many
// inodes in a row carry it at the same place of theirs, counted together:
// the files of a tree carry the same label one after another. over says
// that the attribute is one of more than its inode can hold.
func (w *erofsWriter) eachCarried(fn func(over bool, x xattr, inodes int)) {
	var run [xattrPlaces]xattrRun

	for _, e := range w.entries {
		over := xattrsOverflow(e.xattrs)

		for i, x := range e.xattrs {
			if over || i >= len(run) {
				fn(over, x, 1)

				continue
			}

			r := &run[i] // #nosec G602 -- i is under len(run), tested just above
			if r.inodes == 0 || r.x != x {
				r.end(fn)
				r.x = x
			}

			r.inodes++
		}
	}

	for i := range run {
		run[i].end(fn)
	}
}

// end hands the run to fn, if it has begun, and empties it.
func (r *xattrRun) end(fn func(over bool, x xattr, inodes int)) {
	if r.inodes > 0 {
		fn(false, r.x, r.inodes)
	}

	r.inodes = 0
}

// xattrsOverflow reports whether attrs are more than an inode's xattr body
// holds, sized as it is in maxXattrICount units of four bytes.
func xattrsOverflow(attrs []xattr) bool {
	// An entry is under eight bytes more than its name and value: nearly
	// every inode is told apart without splitting a name.
	most := len(attrs) * 8
	for _, x := range attrs {
		most += len(x.name) + len(x.value)
	}

	return most > maxXattrICount*4 && xattrICount(inlineXattrSize(attrs)) > maxXattrICount
}

// inlineXattrSize is an xattr body that holds all of attrs itself.
func inlineXattrSize(attrs []xattr) int {
	size := disk.SizeXattrBodyHeader
	for _, x := range attrs {
		size += xattrEntrySize(x.name, x.value)
	}

	return size
}

// xattrBody is the bytes attrs put in an inode's xattr body: a reference
// for each of the first maxSharedXattrs of them that are in the shared
// area, then the entries of the others. With keep, it leaves the two in
// bodyIDs and bodyOwn, in attrs' order, for writeXattrs.
func (w *erofsWriter) xattrBody(attrs []xattr, keep bool) int {
	if len(attrs) == 0 {
		return 0
	}

	size, shared := disk.SizeXattrBodyHeader, 0

	if keep {
		w.bodyIDs, w.bodyOwn = w.bodyIDs[:0], w.bodyOwn[:0]
	}

	for i, x := range attrs {
		if id, is := w.sharing.id(i, x); is && shared < maxSharedXattrs {
			shared++
			size += 4 // a reference to the shared area is a __le32

			if keep {
				w.bodyIDs = append(w.bodyIDs, id)
			}

			continue
		}

		if keep {
			w.bodyOwn = append(w.bodyOwn, x)

			continue
		}

		size += xattrEntrySize(x.name, x.value)
	}

	return size
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
	// #nosec G115 -- checkLimits rejected any area whose count exceeds maxXattrICount, the uint16 maximum
	return uint16(xattrICount(xattrSize))
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
	mode := uint16(m.Perm()) // #nosec G115 -- the permission bits are 0o777 at most

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
