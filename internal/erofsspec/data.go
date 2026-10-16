package erofsspec

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Extent is one piece of a file's content: Length bytes at Logical, read
// from Physical on Device, or zeros when Hole. Device 0 is the image.
type Extent struct {
	Logical  int64
	Length   int64
	Device   uint16
	Physical int64
	Hole     bool
}

// Extents maps the whole of an inode's content, as data.c's
// erofs_map_blocks does one position at a time: the flat layouts from the
// start block with the tail inline after the inode, chunk-based files from
// their chunk map. Zero extents for an empty file.
func (im *Image) Extents(in *Inode) ([]Extent, error) {
	switch {
	case in.Size == 0:
		return nil, nil
	case in.Layout == LayoutChunkBased:
		return im.chunkExtents(in)
	case in.Layout == LayoutFlatPlain || in.Layout == LayoutFlatInline:
		return im.flatExtents(in)
	default:
		return nil, fmt.Errorf("%w: inode %d datalayout %d", ErrUnsupported, in.Nid, in.Layout)
	}
}

// flatExtents is the flat arm of erofs_map_blocks: a flat-plain inode with
// no start block is all hole; otherwise the blocks before the last one (or
// every block, flat-plain) sit at the start block, and a flat-inline tail
// follows the inode and its xattrs and must not cross its block ("inline
// data across blocks").
func (im *Image) flatExtents(in *Inode) ([]Extent, error) {
	bs := int64(im.Super.BlockSize)
	inline := in.Layout == LayoutFlatInline

	if !inline && in.StartBlk == NullAddr {
		return []Extent{{Length: in.Size, Hole: true}}, nil
	}

	blocks := (in.Size + bs - 1) / bs
	mapped := blocks * bs

	if inline {
		mapped = (blocks - 1) * bs
	}

	var out []Extent

	if mapped > 0 {
		physical := int64(in.StartBlk) * bs // #nosec G115 -- bounded by the read that follows
		out = append(out, Extent{Length: min(mapped, in.Size), Physical: physical})
	}

	if inline {
		pa := in.loc + in.isize + int64(in.XattrSize)
		tail := in.Size - mapped

		if pa%bs+tail > bs {
			return nil, fmt.Errorf("%w: inode %d inline data across blocks", ErrCorrupt, in.Nid)
		}

		out = append(out, Extent{Logical: mapped, Length: tail, Physical: pa})
	}

	return out, nil
}

// chunkExtents is data.c's erofs_map_chunks over the whole file: one
// extent per chunk, each from its own index entry.
func (im *Image) chunkExtents(in *Inode) ([]Extent, error) {
	chunk := int64(1) << in.ChunkBits
	count := (in.Size + chunk - 1) / chunk
	unit := int64(4)

	if in.ChunkFormat&ChunkFormatIndexes != 0 {
		unit = 8
	}

	// The whole map must lie in the image before anything is sized from
	// the count: a hostile size must not become an allocation.
	if _, err := im.bytesAt(roundUp(in.loc+in.isize+int64(in.XattrSize), unit), count*unit); err != nil {
		return nil, fmt.Errorf("inode %d chunk map: %w", in.Nid, err)
	}

	out := make([]Extent, 0, count)

	for i := range count {
		e, err := im.chunkExtent(in, i)
		if err != nil {
			return nil, err
		}

		out = append(out, e)
	}

	return out, nil
}

// chunkExtent is chunk i of a chunk-based inode: the map starts at the
// first multiple of the entry size after the inode and its xattrs, one
// entry per chunk; a 4-byte entry is a block address, an 8-byte one a
// 48-bit address and a device id masked by the superblock's; an all-ones
// address is a hole.
func (im *Image) chunkExtent(in *Inode, i int64) (Extent, error) {
	bs := int64(im.Super.BlockSize)
	unit := int64(4)
	addrMask := uint64(1)<<32 - 1

	if in.ChunkFormat&ChunkFormatIndexes != 0 {
		unit = 8
	}

	if in.ChunkFormat&ChunkFormat48Bit != 0 {
		addrMask = uint64(1)<<48 - 1
	}

	chunk := int64(1) << in.ChunkBits
	pos := roundUp(in.loc+in.isize+int64(in.XattrSize), unit) + i*unit

	entry, err := im.bytesAt(pos, unit)
	if err != nil {
		return Extent{}, fmt.Errorf("inode %d chunk map: %w", in.Nid, err)
	}

	e := Extent{Logical: i * chunk, Length: min(chunk, in.Size-i*chunk)}

	var addr uint64
	if unit == 4 {
		addr = uint64(binary.LittleEndian.Uint32(entry))
	} else {
		addr = uint64(binary.LittleEndian.Uint16(entry))<<32 | uint64(binary.LittleEndian.Uint32(entry[4:]))
		e.Device = binary.LittleEndian.Uint16(entry[2:]) & im.Super.DeviceIDMask
	}

	addr &= addrMask
	if addr == addrMask {
		e.Hole = true
		e.Device = 0
	} else {
		e.Physical = int64(addr) * bs // #nosec G115 -- 48 bits at most, times 2^16 at most
	}

	return e, nil
}

// MaxContent is the largest content Read materializes: the reference holds
// whole contents in memory, and a hostile size field must not become an
// allocation. A sound image of the tests' corpora is far below it.
const MaxContent = 64 << 20

// Read returns the whole content of an inode: its extents concatenated,
// holes as zeros, each extent read from its device. Every extent is checked
// against its device before anything is allocated.
func (im *Image) Read(in *Inode) ([]byte, error) {
	if in.Size > MaxContent {
		return nil, fmt.Errorf("%w: inode %d of %d bytes", ErrTooLarge, in.Nid, in.Size)
	}

	return im.ReadAt(in, 0, in.Size)
}

// ReadAt returns n bytes of an inode's content from off, through its
// extents, holes as zeros; a range past the size is an error. Only the
// extents the range touches are mapped, as the kernel maps a chunk index
// entry when a read reaches its chunk and not before (data.c,
// erofs_map_chunks), and every extent is read from its device before the
// result is allocated.
func (im *Image) ReadAt(in *Inode, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off > in.Size || n > in.Size-off {
		return nil, fmt.Errorf("%w: %d bytes at %d of inode %d, size %d", ErrTruncated, n, off, in.Nid, in.Size)
	}

	type piece struct {
		at   int64
		data []byte
	}

	var pieces []piece

	for pos := off; pos < off+n; {
		e, err := im.ExtentAt(in, pos)
		if err != nil {
			return nil, err
		}

		hi := min(e.Logical+e.Length, off+n)

		if !e.Hole {
			b, err := im.deviceBytes(e.Device, e.Physical+(pos-e.Logical), hi-pos)
			if err != nil {
				return nil, fmt.Errorf("inode %d data at %d: %w", in.Nid, pos, err)
			}

			pieces = append(pieces, piece{at: pos - off, data: b})
		}

		pos = hi
	}

	out := make([]byte, n)
	for _, p := range pieces {
		copy(out[p.at:], p.data)
	}

	return out, nil
}

// ExtentAt maps one position of the content: the flat extent that holds it,
// or, for a chunk-based inode, the one chunk the position falls in, read
// from its index entry alone.
func (im *Image) ExtentAt(in *Inode, pos int64) (Extent, error) {
	if pos < 0 || pos >= in.Size {
		return Extent{}, fmt.Errorf("%w: position %d of inode %d, size %d", ErrTruncated, pos, in.Nid, in.Size)
	}

	if in.Layout != LayoutChunkBased {
		extents, err := im.Extents(in)
		if err != nil {
			return Extent{}, err
		}

		e, ok := extentAt(extents, pos)
		if !ok {
			return Extent{}, fmt.Errorf("%w: position %d of inode %d unmapped", ErrCorrupt, pos, in.Nid)
		}

		return e, nil
	}

	chunk := int64(1) << in.ChunkBits

	return im.chunkExtent(in, pos/chunk)
}

func extentAt(extents []Extent, pos int64) (Extent, bool) {
	for _, e := range extents {
		if pos >= e.Logical && pos < e.Logical+e.Length {
			return e, true
		}
	}

	return Extent{}, false
}

// deviceBytes is data.c's erofs_map_dev: a device-0 address that falls in
// an extra device's unified range, when extra devices were given, is that
// device's (the `extra_devices && !flatdev` arm, uniaddr from the slot's
// mapped_blkaddr) and any other is the image's; an extra device's address
// is its own bytes when they were given to Parse, or, with none given, the
// image at the device's unified address (devs->flatdev).
func (im *Image) deviceBytes(device uint16, off, n int64) ([]byte, error) {
	if device == 0 {
		if i, start, ok := im.unifiedDevice(off); ok {
			return im.extraBytes(i, off-start, n)
		}

		return im.bytesAt(off, n)
	}

	i := int(device) - 1
	if i >= len(im.Devices) {
		return nil, fmt.Errorf("%w: device %d of %d", ErrCorrupt, device, len(im.Devices))
	}

	if len(im.extra) == 0 {
		base := int64(im.Devices[i].UniAddr) * int64(im.Super.BlockSize) // #nosec G115 -- 48 bits times 2^16

		return im.bytesAt(off+base, n)
	}

	return im.extraBytes(i, off, n)
}

// unifiedDevice finds the extra device whose unified range holds a
// device-0 byte offset, when extra devices were given: its index and the
// range's first byte (erofs_map_dev skips a slot with no unified address).
func (im *Image) unifiedDevice(off int64) (int, int64, bool) {
	if len(im.extra) == 0 {
		return 0, 0, false
	}

	bs := int64(im.Super.BlockSize)

	for i, d := range im.Devices {
		start := int64(d.UniAddr) * bs    // #nosec G115 -- 48 bits times 2^16
		end := start + int64(d.Blocks)*bs // #nosec G115 -- 48 bits times 2^16

		if d.UniAddr != 0 && off >= start && off < end {
			return i, start, true
		}
	}

	return 0, 0, false
}

// extraBytes is n bytes at off of extra device i, which must have been
// given.
func (im *Image) extraBytes(i int, off, n int64) ([]byte, error) {
	if i >= len(im.extra) {
		return nil, fmt.Errorf("%w: device %d, %d given", ErrTruncated, i+1, len(im.extra))
	}

	b, err := sliceAt(im.extra[i], off, n)
	if err != nil {
		return nil, fmt.Errorf("device %d: %w", i+1, err)
	}

	return b, nil
}

// TargetMax is the longest symlink target the VFS reads from a data block:
// one page less its terminator (page_get_link, nd_terminate_link with
// PAGE_SIZE - 1), on the 4 KiB pages most kernels run with.
const TargetMax = 4095

// Target is a symlink's target: the inline bytes when the kernel would keep
// them inline, else the first TargetMax bytes of the content up to its
// first NUL, as the VFS terminates a link read from a page.
func (im *Image) Target(in *Inode) ([]byte, error) {
	if in.InlineTarget != nil {
		return in.InlineTarget, nil
	}

	b, err := im.ReadAt(in, 0, min(in.Size, TargetMax))
	if err != nil {
		return nil, err
	}

	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}

	return b, nil
}
