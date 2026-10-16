package erofsspec

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// The inode (erofs_fs.h: struct erofs_inode_compact, erofs_inode_extended,
// the i_format bits and the data layouts).
const (
	// SlotSize is the inode slot: every inode starts on a 32-byte boundary
	// and a nid counts slots from meta_blkaddr (internal.h, erofs_iloc).
	SlotSize = 32
	// CompactSize and ExtendedSize are the two inode layouts' sizes.
	CompactSize  = 32
	ExtendedSize = 64

	// i_format: bit 0 is the layout version, bits 1 to 3 the data layout,
	// bit 4 "nlink is 1" on a compact non-directory and "the . dirent is
	// omitted" on a directory; any higher bit is unsupported (EROFS_I_ALL).
	formatVersionBit    = 0
	formatLayoutBit     = 1
	formatLayoutMask    = 0x07
	formatNlinkOneBit   = 4
	formatDotOmittedBit = 4
	formatAll           = 1<<(formatNlinkOneBit+1) - 1

	// The data layouts (EROFS_INODE_*).
	LayoutFlatPlain         = 0
	LayoutCompressedFull    = 1
	LayoutFlatInline        = 2
	LayoutCompressedCompact = 3
	LayoutChunkBased        = 4
	layoutMax               = 5

	// Chunk format (erofs_inode_chunk_info.format): the chunk size in block
	// bits, whether the map is 8-byte indexes or a 4-byte block array, and
	// 48-bit addresses; any other bit is unsupported (EROFS_CHUNK_FORMAT_ALL).
	ChunkFormatBlkBitsMask = 0x001F
	ChunkFormatIndexes     = 0x0020
	ChunkFormat48Bit       = 0x0040
	chunkFormatAll         = ChunkFormat48Bit<<1 - 1

	// NullAddr marks a hole in a chunk map (EROFS_NULL_ADDR, all ones in
	// the address's width).
	NullAddr = ^uint64(0)

	// The type bits of i_mode, as stat(2) defines them.
	ModeTypeMask = 0o170000
	ModeSocket   = 0o140000
	ModeSymlink  = 0o120000
	ModeRegular  = 0o100000
	ModeBlock    = 0o060000
	ModeDir      = 0o040000
	ModeChar     = 0o020000
	ModeFifo     = 0o010000

	// NameMax is EROFS_NAME_LEN, the longest dirent name and xattr infix.
	NameMax = 255
)

// Inode is one on-disk inode, decoded as inode.c's erofs_read_inode decodes
// it, with its xattrs resolved (xattr.c).
type Inode struct {
	Nid      uint64
	Extended bool
	Format   uint16
	Layout   uint8
	Mode     uint16
	Nlink    uint32
	Size     int64
	UID      uint32
	GID      uint32
	Mtime    int64
	MtimeNs  uint32
	Ino      uint32
	XattrCnt uint16
	NB       uint16 // i_nb as stored: nlink or the start block's high half

	// Derived as the kernel derives them.
	XattrSize   int    // erofs_xattr_ibody_size(i_xattr_icount)
	StartBlk    uint64 // flat layouts: the first data block, NullAddr for none
	Rdev        uint32 // character and block devices
	ChunkFormat uint16 // chunk-based inodes
	ChunkBits   uint   // log2 of the chunk size in bytes
	DotOmitted  bool   // directories: the "." dirent is not stored
	NameFilter  uint32 // h_name_filter of the xattr ibody header
	SharedIDs   []uint32
	Xattrs      []Xattr

	// InlineTarget is a symlink's target when it is stored inline (inode.c,
	// erofs_fill_symlink); nil when the target is in a data block.
	InlineTarget []byte

	loc    int64 // byte offset of the inode in the image (erofs_iloc)
	isize  int64 // CompactSize or ExtendedSize
	mask48 bool  // addresses are 48-bit wide
}

// Xattr is one extended attribute of an inode, inline or shared, with its
// name assembled from the built-in prefix the entry's index names, the long
// prefix's infix when the index refers to one, and the suffix stored in the
// entry. Index 0 and indexes with no kernel handler (5, lustre, and 7 up)
// leave Prefix empty and Unresolved set: the kernel's listxattr hides them
// and its getxattr cannot find them, but the format stores them like any
// other, so a reader is free to list the suffix as the whole name.
type Xattr struct {
	Index      uint8  // e_name_index as stored, long-prefix bit included
	Prefix     string // the built-in prefix plus the long prefix's infix
	Suffix     string // e_name
	Value      []byte
	Shared     bool
	SharedID   uint32
	Unresolved bool
}

// Name is the attribute's full name, prefix and suffix.
func (x Xattr) Name() string { return x.Prefix + x.Suffix }

// IsDir says whether Mode's type bits are a directory's.
func (in *Inode) IsDir() bool { return in.Mode&ModeTypeMask == ModeDir }

// IsRegular says whether Mode's type bits are a regular file's.
func (in *Inode) IsRegular() bool { return in.Mode&ModeTypeMask == ModeRegular }

// IsSymlink says whether Mode's type bits are a symlink's.
func (in *Inode) IsSymlink() bool { return in.Mode&ModeTypeMask == ModeSymlink }

// Inode reads and decodes the inode at nid (inode.c, erofs_read_inode, then
// xattr.c's erofs_init_inode_xattrs and the iterators).
func (im *Image) Inode(nid uint64) (*Inode, error) {
	// A nid with bit 63 set lives in the metabox, which Parse refused.
	if nid>>63 != 0 {
		return nil, fmt.Errorf("%w: nid %#x in the metabox", ErrUnsupported, nid)
	}

	in := &Inode{Nid: nid, loc: int64(im.Super.MetaBlkAddr)*int64(im.Super.BlockSize) + int64(nid)*SlotSize}

	raw, err := im.bytesAt(in.loc, CompactSize)
	if err != nil {
		return nil, fmt.Errorf("inode %d: %w", nid, err)
	}

	in.Format = binary.LittleEndian.Uint16(raw[0:])
	// "unsupported i_format %u of nid %llu"
	if in.Format&^uint16(formatAll) != 0 {
		return nil, fmt.Errorf("%w: inode %d i_format %#x", ErrUnsupported, nid, in.Format)
	}

	in.Layout = uint8(in.Format>>formatLayoutBit) & formatLayoutMask // #nosec G115 -- masked to three bits
	// "unsupported datalayout %u of nid %llu"
	if in.Layout >= layoutMax {
		return nil, fmt.Errorf("%w: inode %d datalayout %d", ErrUnsupported, nid, in.Layout)
	}

	in.Extended = in.Format>>formatVersionBit&1 == 1
	if in.Extended {
		err = im.decodeExtended(in)
	} else {
		im.decodeCompact(in, raw)
	}

	if err != nil {
		return nil, err
	}

	if err := im.decodeType(in); err != nil {
		return nil, err
	}

	if err := im.decodeXattrs(in); err != nil {
		return nil, err
	}

	return in, nil
}

// decodeCompact is the EROFS_INODE_LAYOUT_COMPACT arm: 16-bit uid, gid and
// nlink, a 32-bit size, mtime as seconds from the superblock's epoch with
// its fixed nanoseconds, and, on a non-directory with the nlink-is-1 bit,
// a link count of one with i_nb holding the address's high 16 bits.
func (im *Image) decodeCompact(in *Inode, raw []byte) {
	in.isize = CompactSize
	in.XattrCnt = binary.LittleEndian.Uint16(raw[2:])
	in.XattrSize = xattrBodySize(in.XattrCnt)
	in.Mode = binary.LittleEndian.Uint16(raw[4:])
	nb := binary.LittleEndian.Uint16(raw[6:])
	in.NB = nb
	in.Size = int64(binary.LittleEndian.Uint32(raw[8:]))
	in.Mtime = im.Super.Epoch + int64(binary.LittleEndian.Uint32(raw[12:]))
	in.MtimeNs = im.Super.FixedNsec
	u := binary.LittleEndian.Uint32(raw[16:])
	in.Ino = binary.LittleEndian.Uint32(raw[20:])
	in.UID = uint32(binary.LittleEndian.Uint16(raw[24:]))
	in.GID = uint32(binary.LittleEndian.Uint16(raw[26:]))

	if !in.IsDir() && in.Format>>formatNlinkOneBit&1 == 1 {
		in.Nlink = 1
		in.StartBlk = uint64(u) | uint64(nb)<<32
		in.mask48 = true
	} else {
		in.Nlink = uint32(nb)
		in.StartBlk = uint64(u)
	}

	in.Rdev = u
	in.ChunkFormat = uint16(u) // #nosec G115 -- the low half of i_u is the chunk format (erofs_inode_chunk_info)
}

// decodeExtended is the EROFS_INODE_LAYOUT_EXTENDED arm: 64 bytes, which
// may cross a block boundary and are read whole here; 32-bit uid, gid and
// nlink, 64-bit size and mtime seconds, 32-bit nanoseconds; the address's
// high 16 bits in i_nb.
func (im *Image) decodeExtended(in *Inode) error {
	in.isize = ExtendedSize

	raw, err := im.bytesAt(in.loc, ExtendedSize)
	if err != nil {
		return fmt.Errorf("inode %d: %w", in.Nid, err)
	}

	in.XattrCnt = binary.LittleEndian.Uint16(raw[2:])
	in.XattrSize = xattrBodySize(in.XattrCnt)
	in.Mode = binary.LittleEndian.Uint16(raw[4:])
	nb := binary.LittleEndian.Uint16(raw[6:])
	in.NB = nb
	in.Size = int64(binary.LittleEndian.Uint64(raw[8:])) // #nosec G115 -- the sign is the check below
	u := binary.LittleEndian.Uint32(raw[16:])
	in.Ino = binary.LittleEndian.Uint32(raw[20:])
	in.UID = binary.LittleEndian.Uint32(raw[24:])
	in.GID = binary.LittleEndian.Uint32(raw[28:])
	mtime := binary.LittleEndian.Uint64(raw[32:])
	in.Mtime = int64(mtime) // #nosec G115 -- inode_set_mtime takes the seconds as a signed time64_t
	in.MtimeNs = binary.LittleEndian.Uint32(raw[40:])
	in.Nlink = binary.LittleEndian.Uint32(raw[44:])
	in.StartBlk = uint64(u) | uint64(nb)<<32
	in.mask48 = true
	in.Rdev = u
	in.ChunkFormat = uint16(u) // #nosec G115 -- the low half of i_u is the chunk format

	// "negative i_size @ nid %llu"
	if in.Size < 0 {
		return fmt.Errorf("%w: inode %d negative size", ErrCorrupt, in.Nid)
	}

	return nil
}

// decodeType is the i_mode switch of erofs_read_inode: directories, regular
// files and symlinks carry a start block (a flat-plain all-ones address is
// "no data"), devices a device number, FIFOs and sockets nothing, and any
// other type is a bogus i_mode; a compressed layout is this reader's limit;
// a chunk-based inode's format is bounded.
func (im *Image) decodeType(in *Inode) error {
	switch in.Mode & ModeTypeMask {
	case ModeDir:
		in.DotOmitted = in.Format>>formatDotOmittedBit&1 == 1

		fallthrough
	case ModeRegular, ModeSymlink:
		mask := uint64(1)<<32 - 1
		if in.mask48 {
			mask = uint64(1)<<48 - 1
		}

		if in.Layout == LayoutFlatPlain && in.StartBlk&mask == mask {
			in.StartBlk = NullAddr
		}

		in.Rdev = 0
	case ModeChar, ModeBlock:
		in.StartBlk = 0
	case ModeFifo, ModeSocket:
		in.StartBlk = 0
		in.Rdev = 0
	default:
		// "bogus i_mode (%o) @ nid %llu"
		return fmt.Errorf("%w: inode %d mode %#o", ErrCorrupt, in.Nid, in.Mode)
	}

	if in.Layout == LayoutCompressedFull || in.Layout == LayoutCompressedCompact {
		return fmt.Errorf("%w: inode %d is compressed", ErrUnsupported, in.Nid)
	}

	if in.Layout == LayoutChunkBased {
		// "unsupported chunk format %x of nid %llu"
		if in.ChunkFormat&^uint16(chunkFormatAll) != 0 {
			return fmt.Errorf("%w: inode %d chunk format %#x", ErrUnsupported, in.Nid, in.ChunkFormat)
		}

		in.ChunkBits = uint(im.Super.BlockBits) + uint(in.ChunkFormat&ChunkFormatBlkBitsMask)
	}

	if in.IsSymlink() {
		return im.decodeInlineSymlink(in)
	}

	return nil
}

// decodeInlineSymlink is inode.c's erofs_fill_symlink: a flat-inline
// symlink whose target fits in the inode's block after the xattrs is read
// there, and must be non-empty with no NUL inside ("invalid fast symlink
// size"); otherwise the target is in a data block and read as data.
func (im *Image) decodeInlineSymlink(in *Inode) error {
	ofs := in.loc%int64(im.Super.BlockSize) + in.isize + int64(in.XattrSize)
	if in.Layout != LayoutFlatInline || ofs+in.Size > int64(im.Super.BlockSize) {
		return nil
	}

	target, err := im.bytesAt(in.loc+in.isize+int64(in.XattrSize), in.Size)
	if err != nil {
		return fmt.Errorf("inode %d symlink: %w", in.Nid, err)
	}

	if in.Size == 0 || bytes.IndexByte(target, 0) >= 0 {
		return fmt.Errorf("%w: inode %d invalid fast symlink size %d", ErrCorrupt, in.Nid, in.Size)
	}

	in.InlineTarget = bytes.Clone(target)

	return nil
}

// The xattr ibody (erofs_fs.h: struct erofs_xattr_ibody_header, struct
// erofs_xattr_entry, the name indexes).
const (
	xattrHeaderSize = 12
	xattrEntrySize  = 4
	// The long-prefix bit of e_name_index and the index below it.
	xattrLongPrefix     = 0x80
	xattrLongPrefixMask = 0x7F
)

// xattrBodySize is erofs_xattr_ibody_size: nothing for a count of zero,
// else the header plus four bytes per further count.
func xattrBodySize(count uint16) int {
	if count == 0 {
		return 0
	}

	return xattrHeaderSize + 4*(int(count)-1)
}

// xattrPrefix is the built-in name prefix of an index (erofs_fs.h,
// EROFS_XATTR_INDEX_*; xattr.c's handler map gives them their spelling), and
// whether the kernel has a handler for it: index 5 (lustre) has none and
// index 0 none either.
func xattrPrefix(index uint8) (string, bool) {
	switch index {
	case 1:
		return "user.", true
	case 2:
		return "system.posix_acl_access", true
	case 3:
		return "system.posix_acl_default", true
	case 4:
		return "trusted.", true
	case 6:
		return "security.", true
	default:
		return "", false
	}
}

// decodeXattrs is erofs_init_inode_xattrs, erofs_xattr_iter_inline and
// erofs_xattr_iter_shared: a body of exactly a header is unsupported and
// one shorter is corrupt; the shared count must fit the body; inline
// entries follow the shared ids and must each fit what remains; shared
// entries sit at xattr_blkaddr's block plus four bytes per id.
func (im *Image) decodeXattrs(in *Inode) error {
	switch {
	case in.XattrSize == 0:
		return nil
	case in.XattrSize == xattrHeaderSize:
		// "xattr_isize %d of nid %llu is not supported yet"
		return fmt.Errorf("%w: inode %d xattr body of %d bytes", ErrUnsupported, in.Nid, in.XattrSize)
	case in.XattrSize < xattrHeaderSize:
		// "bogus xattr ibody @ nid %llu"
		return fmt.Errorf("%w: inode %d xattr body of %d bytes", ErrCorrupt, in.Nid, in.XattrSize)
	}

	pos := in.loc + in.isize

	hdr, err := im.bytesAt(pos, xattrHeaderSize)
	if err != nil {
		return fmt.Errorf("inode %d xattrs: %w", in.Nid, err)
	}

	in.NameFilter = binary.LittleEndian.Uint32(hdr[0:])
	shared := int(hdr[4])
	// "invalid h_shared_count %u @ nid %llu"
	if shared*4 > in.XattrSize-xattrHeaderSize {
		return fmt.Errorf("%w: inode %d shared xattr count %d", ErrCorrupt, in.Nid, shared)
	}

	ids, err := im.bytesAt(pos+xattrHeaderSize, int64(shared)*4)
	if err != nil {
		return fmt.Errorf("inode %d xattrs: %w", in.Nid, err)
	}

	for i := range shared {
		in.SharedIDs = append(in.SharedIDs, binary.LittleEndian.Uint32(ids[4*i:]))
	}

	pos += xattrHeaderSize + int64(shared)*4
	remaining := int64(in.XattrSize) - xattrHeaderSize - int64(shared)*4

	for remaining > 0 {
		x, size, err := im.xattrEntry(pos)
		if err != nil {
			return fmt.Errorf("inode %d xattrs: %w", in.Nid, err)
		}
		// "xattr on-disk corruption: xattr entry beyond xattr_isize"
		if remaining < size {
			return fmt.Errorf("%w: inode %d inline xattr of %d bytes in %d", ErrCorrupt, in.Nid, size, remaining)
		}

		in.Xattrs = append(in.Xattrs, x)
		pos += size
		remaining -= size
	}

	for _, id := range in.SharedIDs {
		x, _, err := im.xattrEntry(int64(im.Super.XattrBlkAddr)*int64(im.Super.BlockSize) + int64(id)*4)
		if err != nil {
			return fmt.Errorf("inode %d shared xattr %d: %w", in.Nid, id, err)
		}

		x.Shared = true
		x.SharedID = id
		in.Xattrs = append(in.Xattrs, x)
	}

	return nil
}

// xattrEntry decodes the entry at pos: e_name_len, e_name_index and
// e_value_size, then the name and the value, the whole rounded up to four
// bytes (erofs_xattr_entry_size). The size returned is that rounded size.
func (im *Image) xattrEntry(pos int64) (Xattr, int64, error) {
	hdr, err := im.bytesAt(pos, xattrEntrySize)
	if err != nil {
		return Xattr{}, 0, err
	}

	nameLen := int64(hdr[0])
	index := hdr[1]
	valueLen := int64(binary.LittleEndian.Uint16(hdr[2:]))

	body, err := im.bytesAt(pos+xattrEntrySize, nameLen+valueLen)
	if err != nil {
		return Xattr{}, 0, err
	}

	x := Xattr{Index: index, Suffix: string(body[:nameLen]), Value: bytes.Clone(body[nameLen:])}
	x.Prefix, x.Unresolved = im.xattrPrefix(index)

	return x, roundUp(xattrEntrySize+nameLen+valueLen, xattrEntrySize), nil
}

// xattrPrefix is the name prefix an index stands for: a long prefix's base
// prefix and infix when bit 7 is set and the lower bits index the table
// (an index past the table is unresolved, as erofs_listxattr_foreach skips
// it), else the built-in prefix, unresolved where the kernel has no handler.
func (im *Image) xattrPrefix(index uint8) (string, bool) {
	if index&xattrLongPrefix != 0 {
		i := int(index & xattrLongPrefixMask)
		if i >= len(im.Prefixes) {
			return "", true
		}

		base, ok := xattrPrefix(im.Prefixes[i].BaseIndex)

		return base + im.Prefixes[i].Infix, !ok
	}

	prefix, ok := xattrPrefix(index)

	return prefix, !ok
}
