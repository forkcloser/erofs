package corpus

import (
	"encoding/binary"
	"hash/crc32"
	"maps"
	"slices"
	"strings"
)

// The assembler writes an image from a described layout, at the offsets
// erofs_fs.h defines and under the rules the kernel reads them by (super.c,
// inode.c, dir.c, xattr.c, data.c), each cited where it is applied. It
// exists for the states mkfs.erofs never writes from a tar: shared xattrs
// (mkfs shares only from a directory source, which no two hosts reproduce),
// an xattr body of exactly a header, a flat-plain inode with the null start
// block, an xattr name under index 0. It is test support, the third party
// the reference and the package are both checked against: it shares no
// code with either, and it has one layout, below, with nothing to choose.
//
// The layout: block 0 holds the superblock at byte 1024, the shared xattr
// area from byte 1152 (so xattr_blkaddr is 1152 / blocksize and the first
// id is the offset within that block over four), then the long prefix
// table; the inode area starts at the next block (meta_blkaddr); the root
// directory is nid 0 with its dirents inline; each entry's inode follows,
// its xattr body and inline data after it, every inode on a 32-byte slot;
// the data blocks of plain files come after the inode area; the block
// count is the whole.

// Layout is what the assembler is asked for.
type Layout struct {
	BlockBits  uint8     // blkszbits, 9 to 16
	Extended   bool      // 64-byte inodes throughout; else 32-byte ones
	NoChecksum bool      // leave FEATURE_COMPAT_SB_CHKSUM unset and the field zero
	Prefixes   []APrefix // the long xattr name prefix table, in order
	Shared     []AXattr  // the shared xattr area, in order; entries refer to it by index
	Entries    []AEntry  // the root directory's children
}

// APrefix is one long xattr name prefix: the built-in index it extends and
// the infix after it.
type APrefix struct {
	BaseIndex uint8
	Infix     string
}

// AXattr is one xattr entry as stored: e_name_index (the long-prefix bit
// included), e_name and the value.
type AXattr struct {
	Index  uint8
	Suffix string
	Value  []byte
}

// AEntry is one child of the root directory.
type AEntry struct {
	Name    string
	Mode    uint16 // type and permission bits, as i_mode
	UID     uint32
	GID     uint32
	Mtime   int64
	MtimeNs uint32
	Nlink   uint32
	Rdev    uint32

	Data   []byte // a regular file's content, inline unless Plain
	Plain  bool   // the content in data blocks (flat plain), never inline
	Hole   int64  // a flat-plain file of this size with the null start block: all zeros
	Target string // a symlink's target, inline

	Inline     []AXattr // inline xattr entries, in order
	SharedIDs  []int    // indexes into Layout.Shared
	HeaderOnly bool     // an xattr body of exactly the 12-byte header (i_xattr_icount 1)
}

// AssembledBuildTime is the epoch and every compact inode's time.
const AssembledBuildTime = 1_700_000_000

// Assemble writes the image.
func Assemble(l Layout) []byte {
	a := &assembler{l: l, bs: 1 << l.BlockBits}

	return a.build()
}

type assembler struct {
	l  Layout
	bs int

	img []byte
	// The shared area's ids and the prefix table's start, in 4-byte units
	// from the start of their block (xattr.c: pos = blkaddr * bs + id * 4;
	// erofs_xattr_prefixes_init: pos = xattr_prefix_start << 2).
	sharedIDs   []uint32
	xattrBlk    uint32
	prefixStart uint32

	metaBlk uint32
	inodes  [][]byte // each inode's bytes, slot-aligned, in nid order
	data    []byte   // the plain files' blocks
	dataBlk uint32   // the first data block
}

func (a *assembler) build() []byte {
	a.img = make([]byte, 1152)
	a.layoutSharedAndPrefixes()

	a.metaBlk = uint32(roundUpInt(len(a.img), a.bs) / a.bs) // #nosec G115 -- small
	a.img = append(a.img, make([]byte, int(a.metaBlk)*a.bs-len(a.img))...)

	entries, sizes := a.plan()
	a.dataBlk = a.metaBlk + uint32(roundUpInt(sum(sizes), a.bs)/a.bs) // #nosec G115 -- small

	a.layoutInodes(entries, sizes)

	meta := slices.Concat(a.inodes...)
	a.img = append(a.img, meta...)
	a.img = append(a.img, make([]byte, roundUpInt(len(a.img), a.bs)-len(a.img))...)
	a.img = append(a.img, a.data...)

	a.writeSuper()

	return a.img
}

// layoutSharedAndPrefixes writes the shared xattr area and the long prefix
// table after the superblock, in block 0.
func (a *assembler) layoutSharedAndPrefixes() {
	a.xattrBlk = uint32(1152 / a.bs) // #nosec G115 -- small
	base := int(a.xattrBlk) * a.bs

	for _, x := range a.l.Shared {
		// An id is the entry's offset within xattr_blkaddr's area over four
		// (xattr.c, erofs_xattr_iter_shared); the entry is 4-aligned (fsck:
		// "unaligned xattr entry in xattr shared area").
		a.sharedIDs = append(a.sharedIDs, uint32((len(a.img)-base)/4)) // #nosec G115 -- small
		a.img = append(a.img, xattrEntry(x)...)
	}

	if len(a.l.Prefixes) == 0 {
		return
	}

	a.img = append(a.img, make([]byte, roundUpInt(len(a.img), 4)-len(a.img))...)
	a.prefixStart = uint32(len(a.img) / 4) // #nosec G115 -- small

	for _, p := range a.l.Prefixes {
		// erofs_read_metadata: a 16-bit length, then base_index and the
		// infix (struct erofs_xattr_long_prefix), the next item 4-aligned.
		item := append([]byte{p.BaseIndex}, p.Infix...)
		a.img = binary.LittleEndian.AppendUint16(a.img, uint16(len(item))) // #nosec G115 -- one name at most
		a.img = append(a.img, item...)
		a.img = append(a.img, make([]byte, roundUpInt(len(a.img), 4)-len(a.img))...)
	}
}

// xattrEntry encodes struct erofs_xattr_entry and its name and value,
// rounded up to four bytes (erofs_xattr_entry_size).
func xattrEntry(x AXattr) []byte {
	nameLen := byte(len(x.Suffix)) // #nosec G115 -- the layouts' own short names
	b := []byte{nameLen, x.Index}
	b = binary.LittleEndian.AppendUint16(b, uint16(len(x.Value))) // #nosec G115 -- the layouts' own small values
	b = append(b, x.Suffix...)
	b = append(b, x.Value...)

	return append(b, make([]byte, roundUpInt(len(b), 4)-len(b))...)
}

// plan is the inode order and each inode's size in bytes: the root first,
// then the entries by name, a directory's dirents sized in (a subdirectory
// holds "." and ".." alone). The sizes fix every nid before anything is
// encoded.
func (a *assembler) plan() ([]AEntry, []int) {
	entries := slices.Clone(a.l.Entries)
	slices.SortFunc(entries, func(x, y AEntry) int { return strings.Compare(x.Name, y.Name) })

	root := AEntry{Name: ".", Mode: 0o40755, Nlink: 2, Mtime: AssembledBuildTime}
	root.Data = a.dirents(entries, 0, nil)
	all := append([]AEntry{root}, entries...)

	sizes := make([]int, len(all))

	for i, e := range all {
		if i > 0 && e.Mode&0o170000 == 0o040000 {
			all[i].Data = a.dirents(nil, 0, nil)
		}

		sizes[i] = a.inodeSize(all[i])
	}

	return all, sizes
}

// layoutInodes encodes the planned inodes with their nids resolved: each
// nid is the slots before it; the root's dirents name the entries, a
// subdirectory's name itself and the root.
func (a *assembler) layoutInodes(all []AEntry, sizes []int) {
	nids := make([]uint64, len(all))
	for i := 1; i < len(all); i++ {
		nids[i] = nids[i-1] + uint64(sizes[i-1])/32 // #nosec G115 -- small
	}

	all[0].Data = a.dirents(all[1:], 0, nids[1:])

	for i := 1; i < len(all); i++ {
		if all[i].Mode&0o170000 == 0o040000 {
			all[i].Data = a.dirents(nil, nids[i], nil)
		}
	}

	for _, e := range all {
		a.inodes = append(a.inodes, a.inode(e))
	}
}

func sum(v []int) int {
	t := 0
	for _, x := range v {
		t += x
	}

	return t
}

// dirents is a directory's block: "." (dot) and ".." (the root), then the
// entries with their nids, dirents first, names packed after in the same
// order, nameoff of the first doubling as the count (erofs.rst,
// "Directories").
func (*assembler) dirents(entries []AEntry, dot uint64, nids []uint64) []byte {
	type de struct {
		name string
		nid  uint64
		typ  byte
	}

	// ".." is the root, nid 0: the layout has one level below it.
	list := []de{{".", dot, 2}, {"..", 0, 2}}

	for i, e := range entries {
		var nid uint64
		if i < len(nids) {
			nid = nids[i]
		}

		list = append(list, de{e.Name, nid, fileType(e.Mode)})
	}

	slices.SortFunc(list, func(x, y de) int { return strings.Compare(x.name, y.name) })

	nameoff := 12 * len(list)
	head := make([]byte, 0, nameoff)

	var names []byte

	for _, d := range list {
		head = binary.LittleEndian.AppendUint64(head, d.nid)
		off := uint16(nameoff + len(names)) // #nosec G115 -- one block
		head = binary.LittleEndian.AppendUint16(head, off)
		head = append(head, d.typ, 0)
		names = append(names, d.name...)
	}

	return append(head, names...)
}

// fileType is erofs_dirent.file_type for an i_mode (fs_types.h FT_*).
func fileType(mode uint16) byte {
	switch mode & 0o170000 {
	case 0o040000:
		return 2
	case 0o020000:
		return 3
	case 0o060000:
		return 4
	case 0o010000:
		return 5
	case 0o140000:
		return 6
	case 0o120000:
		return 7
	default:
		return 1
	}
}

// inodeSize is the slots an entry takes: the inode, its xattr body and its
// inline bytes, rounded up to 32.
func (a *assembler) inodeSize(e AEntry) int {
	return roundUpInt(a.headerSize()+a.xattrBodySize(e)+len(a.inlineBytes(e)), 32)
}

func (a *assembler) headerSize() int {
	if a.l.Extended {
		return 64
	}

	return 32
}

// xattrBodySize is erofs_xattr_ibody_size for the entry: nothing without
// xattrs, the header alone when HeaderOnly, else the header, four bytes per
// shared id and the inline entries.
func (*assembler) xattrBodySize(e AEntry) int {
	switch {
	case e.HeaderOnly:
		return 12
	case len(e.Inline) == 0 && len(e.SharedIDs) == 0:
		return 0
	}

	n := 12 + 4*len(e.SharedIDs)
	for _, x := range e.Inline {
		n += len(xattrEntry(x))
	}

	return n
}

// inlineBytes is what follows the xattr body: a directory's dirents, a
// symlink's target, a regular file's content unless Plain or a Hole.
func (*assembler) inlineBytes(e AEntry) []byte {
	switch {
	case e.Target != "":
		return []byte(e.Target)
	case e.Plain || e.Hole > 0:
		return nil
	default:
		return e.Data
	}
}

// inode encodes one entry: the compact or extended header (erofs_fs.h,
// struct erofs_inode_compact and erofs_inode_extended), the xattr body, the
// inline bytes, and, for a plain file, its data blocks appended to the
// image's data.
func (a *assembler) inode(e AEntry) []byte {
	layout, size, u := a.dataLayout(e)

	format := uint16(layout) << 1 // #nosec G115 -- 0 to 4
	if a.l.Extended {
		format |= 1
	}

	icount := uint16(0)
	if body := a.xattrBodySize(e); body > 0 {
		icount = uint16((body-12)/4 + 1) // #nosec G115 -- small
	}

	var b []byte

	b = binary.LittleEndian.AppendUint16(b, format)
	b = binary.LittleEndian.AppendUint16(b, icount)
	b = binary.LittleEndian.AppendUint16(b, e.Mode)

	if a.l.Extended {
		b = binary.LittleEndian.AppendUint16(b, 0)            // i_nb: startblk_hi
		b = binary.LittleEndian.AppendUint64(b, uint64(size)) // #nosec G115 -- the layouts' own sizes
		b = binary.LittleEndian.AppendUint32(b, u)
		b = binary.LittleEndian.AppendUint32(b, 0) // i_ino
		b = binary.LittleEndian.AppendUint32(b, e.UID)
		b = binary.LittleEndian.AppendUint32(b, e.GID)
		b = binary.LittleEndian.AppendUint64(b, uint64(e.Mtime)) // #nosec G115 -- the field's bits
		b = binary.LittleEndian.AppendUint32(b, e.MtimeNs)
		b = binary.LittleEndian.AppendUint32(b, e.Nlink)
		b = append(b, make([]byte, 16)...)
	} else {
		// Compact: nlink in i_nb, a 32-bit size, mtime as seconds from the
		// epoch, 16-bit ids; the layouts keep every value in range.
		nlink := uint16(e.Nlink)                      // #nosec G115 -- in range by construction
		size32 := uint32(size)                        // #nosec G115 -- in range by construction
		mtime := uint32(e.Mtime - AssembledBuildTime) // #nosec G115 -- in range by construction
		uid := uint16(e.UID)                          // #nosec G115 -- in range by construction
		gid := uint16(e.GID)                          // #nosec G115 -- in range by construction
		b = binary.LittleEndian.AppendUint16(b, nlink)
		b = binary.LittleEndian.AppendUint32(b, size32)
		b = binary.LittleEndian.AppendUint32(b, mtime)
		b = binary.LittleEndian.AppendUint32(b, u)
		b = binary.LittleEndian.AppendUint32(b, 0) // i_ino
		b = binary.LittleEndian.AppendUint16(b, uid)
		b = binary.LittleEndian.AppendUint16(b, gid)
		b = append(b, make([]byte, 4)...)
	}

	b = append(b, a.xattrBody(e)...)
	b = append(b, a.inlineBytes(e)...)

	return append(b, make([]byte, roundUpInt(len(b), 32)-len(b))...)
}

// dataLayout is the entry's i_format layout, i_size and i_u: a device's
// rdev; a hole's size with the all-ones start block (inode.c: a flat-plain
// start block of all ones is EROFS_NULL_ADDR, unmapped); a plain file's
// first data block, appended to the data; anything inline with the null
// block, as mkfs writes it.
func (a *assembler) dataLayout(e AEntry) (layout int, size int64, u uint32) {
	switch {
	case e.Mode&0o170000 == 0o020000 || e.Mode&0o170000 == 0o060000:
		return 0, 0, e.Rdev
	case e.Hole > 0:
		return 0, e.Hole, 0xFFFFFFFF
	case e.Plain:
		blk := a.dataBlk + uint32(len(a.data)/a.bs) // #nosec G115 -- small
		a.data = append(a.data, e.Data...)
		a.data = append(a.data, make([]byte, roundUpInt(len(a.data), a.bs)-len(a.data))...)

		return 0, int64(len(e.Data)), blk
	case len(a.inlineBytes(e)) > 0:
		return 2, int64(len(a.inlineBytes(e))), 0xFFFFFFFF
	default:
		return 0, 0, 0xFFFFFFFF
	}
}

// xattrBody is struct erofs_xattr_ibody_header (a zero name filter, the
// shared count, reserved bytes), the shared ids, then the inline entries.
func (a *assembler) xattrBody(e AEntry) []byte {
	n := a.xattrBodySize(e)
	if n == 0 {
		return nil
	}

	b := make([]byte, 0, n)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, byte(len(e.SharedIDs))) // #nosec G115 -- the layouts' own few ids
	b = append(b, make([]byte, 7)...)

	for _, i := range e.SharedIDs {
		b = binary.LittleEndian.AppendUint32(b, a.sharedIDs[i])
	}

	for _, x := range e.Inline {
		b = append(b, xattrEntry(x)...)
	}

	return b
}

// writeSuper fills struct erofs_super_block at byte 1024 and, last, its
// checksum: crc32c seeded with 0x5045B54A over the block from the field
// after the checksum, less the superblock's offset once the block is larger
// (super.c, erofs_superblock_csum_verify).
func (a *assembler) writeSuper() {
	sb := a.img[1024:]
	binary.LittleEndian.PutUint32(sb[0:], 0xE0F5E1E2)

	compat := uint32(0x2) // MTIME
	if !a.l.NoChecksum {
		compat |= 0x1
	}

	if len(a.l.Prefixes) > 0 {
		compat |= 0x10 // PLAIN_XATTR_PFX
	}

	binary.LittleEndian.PutUint32(sb[8:], compat)
	sb[12] = a.l.BlockBits
	binary.LittleEndian.PutUint16(sb[14:], 0) // rootnid_2b: the root is nid 0
	binary.LittleEndian.PutUint64(sb[16:], uint64(len(a.l.Entries)+1))
	binary.LittleEndian.PutUint64(sb[24:], AssembledBuildTime)
	binary.LittleEndian.PutUint32(sb[36:], uint32(len(a.img)/a.bs)) // #nosec G115 -- small
	binary.LittleEndian.PutUint32(sb[40:], a.metaBlk)
	binary.LittleEndian.PutUint32(sb[44:], a.xattrBlk)
	copy(
		sb[48:],
		[]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
	)

	if len(a.l.Prefixes) > 0 {
		binary.LittleEndian.PutUint32(sb[80:], 0x40) // XATTR_PREFIXES
		sb[91] = byte(len(a.l.Prefixes))             // #nosec G115 -- the layouts' own few prefixes
		binary.LittleEndian.PutUint32(sb[92:], a.prefixStart)
	}

	if a.l.NoChecksum {
		return
	}

	n := a.bs
	if n > 1024 {
		n -= 1024
	}

	n -= 8
	crc := ^crc32.Update(^uint32(0x5045B54A), crc32.MakeTable(crc32.Castagnoli), a.img[1032:1032+n])
	binary.LittleEndian.PutUint32(sb[4:], crc)
}

func roundUpInt(v, to int) int { return (v + to - 1) / to * to }

// Assembled are the layouts the committed assembled images carry, with the
// tree each must read back as.
func Assembled() []AssembledSet {
	shared := []AXattr{
		{Index: 1, Suffix: "shared", Value: []byte("one value on three files")},
		{Index: 6, Suffix: "selinux", Value: []byte("system_u:object_r:etc_t:s0")},
	}
	three := func(data string, inline []AXattr) AEntry {
		return AEntry{
			Mode:      0o100644,
			Nlink:     1,
			Mtime:     AssembledBuildTime,
			Data:      []byte(data),
			Inline:    inline,
			SharedIDs: []int{0, 1},
		}
	}

	withNames := func(entries []AEntry, names ...string) []AEntry {
		for i := range entries {
			entries[i].Name = names[i]
		}

		return entries
	}

	sharedEntries := func() []AEntry {
		return withNames([]AEntry{
			three("a\n", []AXattr{{Index: 1, Suffix: "own", Value: []byte("a")}}),
			three("b\n", nil),
			three("c\n", []AXattr{{Index: 4, Suffix: "overlay.opaque", Value: []byte("y")}}),
			{Mode: 0o040755, Nlink: 2, Mtime: AssembledBuildTime, SharedIDs: []int{1}},
		}, "a", "b", "c", "dir")
	}

	sharedTree := func() []Node {
		shared := map[string]string{
			"user.shared":      "one value on three files",
			"security.selinux": "system_u:object_r:etc_t:s0",
		}
		with := func(extra map[string]string) map[string]string {
			m := map[string]string{}
			maps.Copy(m, shared)
			maps.Copy(m, extra)

			return m
		}

		return []Node{
			{
				Path:   "a",
				Type:   '0',
				Mode:   0o644,
				Data:   []byte("a\n"),
				Sec:    AssembledBuildTime,
				Xattrs: with(map[string]string{"user.own": "a"}),
			},
			{Path: "b", Type: '0', Mode: 0o644, Data: []byte("b\n"), Sec: AssembledBuildTime, Xattrs: with(nil)},
			{
				Path:   "c",
				Type:   '0',
				Mode:   0o644,
				Data:   []byte("c\n"),
				Sec:    AssembledBuildTime,
				Xattrs: with(map[string]string{"trusted.overlay.opaque": "y"}),
			},
			{
				Path:   "dir",
				Type:   '5',
				Mode:   0o755,
				Sec:    AssembledBuildTime,
				Xattrs: map[string]string{"security.selinux": "system_u:object_r:etc_t:s0"},
			},
		}
	}

	return []AssembledSet{
		{
			Name:   "assembled-shared",
			Layout: Layout{BlockBits: 12, Shared: shared, Entries: sharedEntries()},
			Tree:   sharedTree,
		},
		{
			Name:   "assembled-shared-512-extended",
			Layout: Layout{BlockBits: 9, Extended: true, Shared: shared, Entries: sharedEntries()},
			Tree:   sharedTree,
		},
		{
			Name: "assembled-hole",
			Layout: Layout{BlockBits: 12, Entries: []AEntry{
				{Name: "hole", Mode: 0o100644, Nlink: 1, Mtime: AssembledBuildTime, Hole: 3000},
				{Name: "plain", Mode: 0o100644, Nlink: 1, Mtime: AssembledBuildTime, Data: Pattern(5000), Plain: true},
				{Name: "empty", Mode: 0o100644, Nlink: 1, Mtime: AssembledBuildTime},
			}},
			Tree: func() []Node {
				return []Node{
					{Path: "hole", Type: '0', Mode: 0o644, Data: make([]byte, 3000), Sec: AssembledBuildTime},
					{Path: "plain", Type: '0', Mode: 0o644, Data: Pattern(5000), Sec: AssembledBuildTime},
					{Path: "empty", Type: '0', Mode: 0o644, Sec: AssembledBuildTime},
				}
			},
		},
		{
			Name: "assembled-index0",
			Layout: Layout{BlockBits: 12, Entries: []AEntry{
				{Name: "f", Mode: 0o100644, Nlink: 1, Mtime: AssembledBuildTime, Data: []byte("f\n"), Inline: []AXattr{
					{Index: 0, Suffix: "com.apple.provenance", Value: []byte{1, 2, 3}},
					{Index: 1, Suffix: "k", Value: []byte("v")},
				}},
			}},
			Tree: func() []Node {
				return []Node{
					{
						Path:   "f",
						Type:   '0',
						Mode:   0o644,
						Data:   []byte("f\n"),
						Sec:    AssembledBuildTime,
						Xattrs: map[string]string{"com.apple.provenance": "\x01\x02\x03", "user.k": "v"},
					},
				}
			},
		},
		{
			Name: "assembled-header-only-xattr",
			Layout: Layout{BlockBits: 12, Entries: []AEntry{
				{Name: "f", Mode: 0o100644, Nlink: 1, Mtime: AssembledBuildTime, Data: []byte("f\n"), HeaderOnly: true},
			}},
			// The kernel refuses the inode as unsupported (xattr.c, "xattr_isize
			// %d of nid %llu is not supported yet"), so no tree reads back.
			Tree: nil,
		},
	}
}

// AssembledSet is one committed assembled image: its layout and the tree it
// reads back as, nil where the kernel refuses it.
type AssembledSet struct {
	Name   string
	Layout Layout
	Tree   func() []Node
}
